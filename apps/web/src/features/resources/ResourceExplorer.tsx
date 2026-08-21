import { useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import type { ResourceDescriptor } from "@k8s-dashboard/contracts";
import { useResourceCatalog, useResourceList, useResourceObject, useScope } from "@/api/queries";
import { useDashboardParams } from "@/state/useDashboardParams";
import { Combobox } from "@/components/Combobox";
import { ScopeSelector } from "@/components/controls";
import { Panel } from "@/components/primitives";
import { LoadingState } from "@/components/SectionState";
import { ResourceDetailDrawer } from "./ResourceDetailDrawer";
import { explorerState, requestErrorMessage, stateFromError, stateNotice, type StateNotice } from "./state";

/* 서버가 강제하는 상한을 UI도 그대로 씁니다 — 더 큰 값을 보내도 서버가 400으로 거절합니다. */
const PAGE_SIZE = 50;
const MAX_NAME_FILTER = 253;
const MAX_LABEL_SELECTOR = 512;

function descriptorKey(d: { group: string; version: string; resource: string }) {
  return `${d.group}/${d.version}/${d.resource}`;
}

function parseKey(value: string): { group: string; version: string; resource: string } | null {
  const parts = value.split("/");
  if (parts.length !== 3 || parts.some((p) => !p)) return null;
  return { group: parts[0], version: parts[1], resource: parts[2] };
}

const STATE_LABEL: Record<string, string> = {
  ready: "조회 가능",
  syncing: "동기화 중",
  unsupported: "미지원",
  forbidden: "권한 없음",
  missing: "미제공",
};

function StateBlock({ notice }: { notice: StateNotice }) {
  const cls = notice.tone === "neutral" ? "state" : `state state--${notice.tone}`;
  return (
    <div className={cls} role={notice.tone === "forbidden" || notice.tone === "error" ? "alert" : undefined} data-state={notice.state}>
      <span className="state__glyph" aria-hidden="true">
        {notice.tone === "neutral" ? "✓" : notice.tone === "forbidden" ? "!" : "▲"}
      </span>
      <span className="state__title">{notice.title}</span>
      <span className="state__detail">{notice.detail}</span>
    </div>
  );
}

/**
 * Resource Explorer (ADR 0018)
 * --------------------------------------------------------------------------
 * API Group → Kind → Namespace → 항목 순으로 좁혀 보는 **조회 전용** 화면입니다.
 *
 * - 브라우저는 BFF의 catalog/list/detail 세 경로만 부릅니다. Kubernetes 직접 호출은 없습니다.
 * - 목록은 서버 keyset cursor로만 이어봅니다. offset 페이징이 없습니다 (ADR 0003).
 * - 폴링하지 않습니다. 갱신은 사용자가 "다시 조회"로 일으킵니다.
 * - 생성·수정·삭제 경로가 없습니다. 상세도 정제된 읽기 전용 YAML뿐입니다.
 * - 필터·선택은 URL에 남겨 새로고침·공유에 견딥니다.
 */
export function ResourceExplorer() {
  const [params, setParams] = useSearchParams();
  const { clusterId, namespace, patch } = useDashboardParams();
  const scope = useScope();
  const available = scope.data?.canExploreResources ?? false;

  const catalog = useResourceCatalog(clusterId, available);
  const descriptors = catalog.data?.items ?? [];

  const selectedKey = params.get("res") ?? "";
  const namePrefix = params.get("q") ?? "";
  const labelSelector = params.get("labels") ?? "";
  const order = params.get("order") === "desc" ? "desc" : "asc";
  const item = params.get("item") ?? "";

  /* 로컬 입력은 즉시 요청하지 않습니다 — 타이핑마다 서버를 부르면 폴링과 다르지 않습니다.
     "조회" 버튼(또는 Enter)에서만 URL이 바뀌고 그때 요청이 나갑니다. */
  const [nameDraft, setNameDraft] = useState(namePrefix);
  const [labelDraft, setLabelDraft] = useState(labelSelector);

  const descriptor: ResourceDescriptor | undefined = useMemo(
    () => descriptors.find((d) => descriptorKey(d) === selectedKey),
    [descriptors, selectedKey],
  );
  const gvr = useMemo(() => parseKey(selectedKey), [selectedKey]);

  const list = useResourceList(
    {
      clusterId,
      group: gvr?.group ?? "",
      version: gvr?.version ?? "",
      resource: gvr?.resource ?? "",
      namespace: descriptor?.namespaced === false ? "all" : namespace,
      namePrefix,
      labelSelector,
      order,
      limit: PAGE_SIZE,
    },
    /* 카탈로그에서 이 GVR의 namespaced 여부를 알기 전에는 요청하지 않습니다 —
       cluster 범위 리소스에 ns를 붙여 보내면 서버가 400으로 거절합니다. */
    available && Boolean(gvr) && Boolean(descriptor),
  );

  const rows = useMemo(() => (list.data?.pages ?? []).flatMap((p) => p.items), [list.data]);
  const total = list.data?.pages?.[0]?.total ?? 0;
  const kind = list.data?.pages?.[0]?.kind ?? descriptor?.kind ?? "";
  const observedAt = list.data?.pages?.[0]?.observedAt;

  const [selNs, selName, selUid] = item ? item.split("/") : ["", "", ""];
  const target = selName && selUid ? { namespace: selNs, name: selName, uid: selUid } : null;
  const object = useResourceObject(clusterId, gvr ?? { group: "", version: "", resource: "" }, target);

  const setParam = (key: string, value: string) =>
    setParams(
      (prev) => {
        const p = new URLSearchParams(prev);
        if (value) p.set(key, value);
        else p.delete(key);
        return p;
      },
      { replace: true },
    );

  const applyFilters = () => {
    setParams(
      (prev) => {
        const p = new URLSearchParams(prev);
        if (nameDraft) p.set("q", nameDraft);
        else p.delete("q");
        if (labelDraft) p.set("labels", labelDraft);
        else p.delete("labels");
        p.delete("item");
        return p;
      },
      { replace: true },
    );
  };

  const selectResource = (value: string) => {
    setParams(
      (prev) => {
        const p = new URLSearchParams(prev);
        if (value) p.set("res", value);
        else p.delete("res");
        p.delete("item");
        return p;
      },
      { replace: true },
    );
  };

  /* 기능 자체가 없는 배포는 필터도 보여주지 않습니다 — 눌러도 되지 않는 컨트롤은 소음입니다. */
  if (!available || stateFromError(catalog.error) === "unavailable") {
    return (
      <Panel title="Resource Explorer" subtitle="조회 전용">
        <StateBlock notice={stateNotice("unavailable")} />
      </Panel>
    );
  }

  const catalogMessage = requestErrorMessage(catalog.error);
  const listMessage = requestErrorMessage(list.error);
  /* allowlist에서 빠진 GVR로 deep link가 들어오면 빈 표가 아니라 "미등록"을 알립니다. */
  const unknownResource = Boolean(gvr) && Boolean(catalog.data) && !descriptor;
  const state = explorerState(descriptor, list.error, rows.length, list.isSuccess);

  return (
    <>
      <Panel title="필터" subtitle="Scope는 서버가 강제합니다 — 여기 값은 요청 힌트입니다">
        <div className="resource-filters">
          <ScopeSelector
            scope={scope.data}
            clusterId={clusterId}
            namespace={namespace}
            onChange={(next) => patch(next)}
            showNamespace={descriptor?.namespaced !== false}
          />

          <Combobox
            id="resource-kind"
            label="리소스 종류"
            value={selectedKey}
            disabled={catalog.isLoading || descriptors.length === 0}
            options={descriptors.map((d) => ({
              value: descriptorKey(d),
              label: `${d.kind || d.resource} · ${d.group}/${d.version}`,
              note: d.state === "ready" ? undefined : STATE_LABEL[d.state],
            }))}
            onSelect={selectResource}
          />

          <label className="field" htmlFor="resource-name">
            <span className="field__label">이름 prefix</span>
            <input
              id="resource-name"
              type="search"
              value={nameDraft}
              maxLength={MAX_NAME_FILTER}
              spellCheck={false}
              placeholder="payments-"
              onChange={(e) => setNameDraft(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") applyFilters();
              }}
            />
          </label>

          <label className="field field--grow" htmlFor="resource-labels">
            <span className="field__label">Label selector</span>
            <input
              id="resource-labels"
              type="text"
              value={labelDraft}
              maxLength={MAX_LABEL_SELECTOR}
              spellCheck={false}
              placeholder="app=payments,tier!=batch"
              onChange={(e) => setLabelDraft(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") applyFilters();
              }}
            />
          </label>

          <div className="chips" role="group" aria-label="정렬 방향">
            <button type="button" className="chip" aria-pressed={order === "asc"} onClick={() => setParam("order", "")}>
              이름 오름차순
            </button>
            <button type="button" className="chip" aria-pressed={order === "desc"} onClick={() => setParam("order", "desc")}>
              내림차순
            </button>
          </div>

          <button type="button" className="ds-button ds-button--sm" onClick={applyFilters}>
            조회
          </button>
        </div>
        {catalogMessage && (
          <p className="resource-filters__error" role="alert">
            {catalogMessage}
          </p>
        )}
        {catalog.data?.degraded && (
          <p className="muted" role="note">
            일부 API group discovery가 실패했습니다{catalog.data.reason ? ` — ${catalog.data.reason}` : ""}. 목록이 완전하지 않을 수 있습니다.
          </p>
        )}
      </Panel>

      <Panel
        title={
          <span className="row">
            <span>{kind || "리소스"}</span>
            {descriptor && descriptor.state !== "ready" && (
              <span className="muted">{STATE_LABEL[descriptor.state]}</span>
            )}
          </span>
        }
        subtitle={
          gvr
            ? `${selectedKey}${total ? ` · 인덱스 ${total}건` : ""}${observedAt ? ` · 관측 ${observedAt}` : ""}`
            : "왼쪽에서 리소스 종류를 고르세요"
        }
        actions={
          gvr ? (
            <button type="button" className="linkish" onClick={() => void list.refetch()} disabled={list.isFetching}>
              다시 조회
            </button>
          ) : undefined
        }
        flush
      >
        {!gvr ? (
          <div className="state">
            <span className="state__glyph" aria-hidden="true">
              →
            </span>
            <span className="state__title">리소스 종류를 선택하세요</span>
            <span className="state__detail">탐색 대상으로 등록된 리소스만 목록에 나옵니다.</span>
          </div>
        ) : unknownResource ? (
          <StateBlock
            notice={stateNotice("missing", "이 리소스는 탐색 대상으로 등록되어 있지 않습니다. 목록에서 다른 종류를 고르세요.")}
          />
        ) : catalog.isLoading || (list.isLoading && rows.length === 0) ? (
          <LoadingState />
        ) : listMessage ? (
          <div className="state state--error" role="alert">
            <span className="state__glyph" aria-hidden="true">
              ✕
            </span>
            <span className="state__title">목록을 불러오지 못했습니다</span>
            <span className="state__detail">{listMessage}</span>
          </div>
        ) : state !== "ready" ? (
          <StateBlock notice={stateNotice(state, descriptor?.reason)} />
        ) : (
          <>
            <div className="panel__scroll panel__scroll--fixed">
              <table className="ds-data-table ds-data-table--compact ds-data-table--interactive">
                <caption className="visually-hidden">{kind} 목록 — 이름을 선택하면 매니페스트가 열립니다</caption>
                <thead>
                  <tr>
                    <th>이름</th>
                    {descriptor?.namespaced !== false && <th style={{ width: 160 }}>Namespace</th>}
                    <th style={{ width: 200 }}>생성</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((row) => (
                    <tr key={row.uid} className={item === `${row.namespace ?? ""}/${row.name}/${row.uid}` ? "is-selected" : ""}>
                      <td className="ds-ident">
                        <button
                          type="button"
                          className="linkish"
                          onClick={() => setParam("item", `${row.namespace ?? ""}/${row.name}/${row.uid}`)}
                        >
                          {row.name}
                        </button>
                      </td>
                      {descriptor?.namespaced !== false && <td>{row.namespace ?? "-"}</td>}
                      <td>{row.createdAt ?? "-"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <div className="resource-list__footer">
              <span className="muted">
                {rows.length}건 표시
                {list.hasNextPage ? " · 서버 cursor로 이어보기" : " · 마지막 페이지"}
              </span>
              {list.hasNextPage && (
                <button
                  type="button"
                  className="ds-button ds-button--sm ds-button--ghost"
                  onClick={() => void list.fetchNextPage()}
                  disabled={list.isFetchingNextPage}
                >
                  {list.isFetchingNextPage ? "불러오는 중…" : "더 보기"}
                </button>
              )}
            </div>
          </>
        )}
      </Panel>

      <ResourceDetailDrawer
        open={Boolean(target)}
        loading={object.isLoading}
        detail={object.data}
        error={object.error}
        onClose={() => setParam("item", "")}
      />
    </>
  );
}
