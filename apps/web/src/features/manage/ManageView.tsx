import { useState } from "react";
import { useLocation, useSearchParams } from "react-router-dom";
import type { ManagedWorkload } from "@k8s-dashboard/contracts";
import { useManagedList, useManageAction, useScope } from "@/api/queries";
import { useDashboardParams } from "@/state/useDashboardParams";
import { Panel, StatusBadge } from "@/components/primitives";
import { SectionView } from "@/components/SectionState";
import { Breadcrumb } from "@/components/drill";
import { withSearch } from "@/components/drill";
import { DeployButton } from "@/components/DeployButton";
import { ManifestEditor } from "@/components/ManifestEditor";
import { PageHeader } from "@/features/drill/common";
import { useManagedDeployment, useManagedSecret } from "@/api/queries";
import { num } from "@/lib/format";

type Kind = "deployments" | "secrets";

/**
 * Deployment/Secret 관리 화면 공통 뼈대 (ADR 0014, #33)
 * --------------------------------------------------------------------------
 * 좌: 워크로드 목록 · 우: 선택 항목 상세(pod 목록 + 배포/재배포 + yaml/json 편집).
 * 선택은 URL(?item=ns/name)에 남겨 새로고침·공유에 견딥니다.
 * 관리 권한이 없으면 화면 자체가 막힙니다(nav에서도 숨김).
 */
export function ManageView({ kind }: { kind: Kind }) {
  const { search } = useLocation();
  const [params, setParams] = useSearchParams();
  const { clusterId, namespace } = useDashboardParams();
  const scope = useScope();
  const canManage = scope.data?.canManageWorkloads ?? false;

  const list = useManagedList(clusterId, kind, namespace, canManage);
  const selected = params.get("item") ?? "";
  const [selNs, selName] = selected.includes("/") ? selected.split("/") : ["", ""];

  const label = kind === "deployments" ? "Deployment" : "Secret";
  const title = kind === "deployments" ? "Deployment 관리" : "Secret 관리";

  const select = (w: ManagedWorkload) =>
    setParams(
      (prev) => {
        const p = new URLSearchParams(prev);
        p.set("item", `${w.namespace}/${w.name}`);
        return p;
      },
      { replace: true },
    );

  if (!canManage) {
    return (
      <div className="page">
        <PageHeader title={title} subtitle={`${clusterId}`} crumbs={<Breadcrumb items={[{ label: title }]} />} />
        <section className="panel">
          <div className="panel__body">
            <div className="state" role="alert">
              <span className="state__title">관리 권한이 없습니다</span>
              <span className="muted">이 기능은 platform.admin 전용입니다.</span>
            </div>
          </div>
        </section>
      </div>
    );
  }

  return (
    <div className="page">
      <PageHeader
        title={title}
        subtitle={`${clusterId} · ${namespace === "all" ? "모든 Namespace" : namespace}`}
        crumbs={<Breadcrumb items={[{ label: "Cluster Overview", to: withSearch("/", search) }, { label: title }]} />}
      />
      <div className="grid grid--split">
        <Panel title={`${label} 목록`} subtitle="이름을 클릭하면 오른쪽에 상세가 열립니다" flush>
          <SectionView
            section={list.data ? { status: "ok", data: list.data.items } : undefined}
            loading={list.isLoading}
            emptyTitle={`${label}이(가) 없습니다`}
          >
            {(items) => (
              <div className="panel__scroll">
                <table className="ds-data-table ds-data-table--compact">
                  <thead>
                    <tr>
                      <th>이름</th>
                      <th style={{ width: 130 }}>Namespace</th>
                      <th style={{ width: 90 }}>{kind === "deployments" ? "Ready" : "타입"}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {items.map((w) => (
                      <tr key={`${w.namespace}/${w.name}`} className={selected === `${w.namespace}/${w.name}` ? "is-selected" : ""}>
                        <td className="ds-ident">
                          <button type="button" className="linkish" onClick={() => select(w)}>
                            {w.name}
                          </button>
                        </td>
                        <td>{w.namespace}</td>
                        <td className="ds-num">
                          {kind === "deployments" ? `${w.ready}/${w.desired}` : (w.secretType || "-")}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </SectionView>
        </Panel>

        {selNs && selName ? (
          kind === "deployments" ? (
            <DeploymentDetailPanel clusterId={clusterId} ns={selNs} name={selName} />
          ) : (
            <SecretDetailPanel clusterId={clusterId} ns={selNs} name={selName} />
          )
        ) : (
          <Panel title="상세" subtitle="왼쪽에서 항목을 선택하세요">
            <div className="state">
              <span className="state__glyph" aria-hidden="true">
                ←
              </span>
              <span className="state__title">항목을 선택하세요</span>
            </div>
          </Panel>
        )}
      </div>
    </div>
  );
}

/* ── pod 목록 공통 ────────────────────────────────────────────────────── */

function PodList({ pods }: { pods: { name: string; uid: string; phase: string; ready: boolean; restarts: number; severity: import("@k8s-dashboard/contracts").Severity }[] }) {
  if (pods.length === 0) return <p className="muted">연결된 Pod가 없습니다.</p>;
  return (
    <table className="ds-data-table ds-data-table--compact">
      <thead>
        <tr>
          <th>Pod</th>
          <th style={{ width: 90 }}>Phase</th>
          <th className="ds-num" style={{ width: 80 }}>
            Restarts
          </th>
        </tr>
      </thead>
      <tbody>
        {pods.map((p) => (
          <tr key={p.uid}>
            <td className="ds-ident">
              <StatusBadge severity={p.severity} label={p.name} small />
            </td>
            <td>{p.phase}</td>
            <td className="ds-num">{num(p.restarts)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

/* ── Deployment 상세 ──────────────────────────────────────────────────── */

function DeploymentDetailPanel({ clusterId, ns, name }: { clusterId: string; ns: string; name: string }) {
  const q = useManagedDeployment(clusterId, ns, name, true);
  const action = useManageAction(clusterId);
  const [draft, setDraft] = useState<string | null>(null);
  const d = q.data;

  return (
    <Panel
      title={
        <span className="row">
          <span className="ds-ident">{name}</span>
          {d && <span className="muted">{d.ready}/{d.desired} ready</span>}
        </span>
      }
      subtitle={ns}
      actions={
        d && (
          <div className="row" style={{ gap: "var(--space-3)" }}>
            <DeployButton
              label="재배포"
              busy={action.isPending}
              onConfirm={() => action.mutateAsync({ method: "POST", path: `deployments/${ns}/${name}/restart` }).then(() => undefined)}
            />
            <DeployButton
              label="배포(저장 적용)"
              busy={action.isPending}
              disabled={draft === null}
              onConfirm={() =>
                action
                  .mutateAsync({ method: "PUT", path: `deployments/${ns}/${name}`, body: { manifest: draft } })
                  .then(() => setDraft(null))
              }
            />
          </div>
        )
      }
    >
      {q.isLoading ? (
        <div className="state" aria-busy="true">
          <span className="state__title">불러오는 중…</span>
        </div>
      ) : q.isError || !d ? (
        <div className="state" role="alert">
          <span className="state__title">상세를 불러오지 못했습니다</span>
        </div>
      ) : (
        <>
          {action.isError && (
            <div className="state" role="alert" style={{ marginBottom: "var(--space-3)" }}>
              <span className="state__title">작업 실패 — 다시 시도하세요</span>
            </div>
          )}
          <h3 className="manage-detail__heading">소속 Pod</h3>
          <PodList pods={d.pods} />
          <h3 className="manage-detail__heading">매니페스트</h3>
          <ManifestEditor jsonText={d.manifest} editable onChange={setDraft} />
        </>
      )}
    </Panel>
  );
}

/* ── Secret 상세 ──────────────────────────────────────────────────────── */

function SecretDetailPanel({ clusterId, ns, name }: { clusterId: string; ns: string; name: string }) {
  const q = useManagedSecret(clusterId, ns, name, true);
  const action = useManageAction(clusterId);
  const [reveal, setReveal] = useState(false);
  const [edited, setEdited] = useState<Record<string, string> | null>(null);
  const s = q.data;
  const data = edited ?? s?.data ?? {};

  return (
    <Panel
      title={
        <span className="row">
          <span className="ds-ident">{name}</span>
          {s && <span className="muted">{s.secretType}</span>}
        </span>
      }
      subtitle={ns}
      actions={
        s && (
          <div className="row" style={{ gap: "var(--space-3)" }}>
            <button type="button" className="linkish" onClick={() => setReveal((v) => !v)}>
              {reveal ? "값 가리기" : "값 보기"}
            </button>
            <DeployButton
              label="재배포"
              busy={action.isPending}
              onConfirm={() => action.mutateAsync({ method: "POST", path: `secrets/${ns}/${name}/restart` }).then(() => undefined)}
            />
            <DeployButton
              label="배포(저장 적용)"
              busy={action.isPending}
              disabled={edited === null}
              onConfirm={() =>
                action.mutateAsync({ method: "PUT", path: `secrets/${ns}/${name}`, body: { data: edited } }).then(() => setEdited(null))
              }
            />
          </div>
        )
      }
    >
      {q.isLoading ? (
        <div className="state" aria-busy="true">
          <span className="state__title">불러오는 중…</span>
        </div>
      ) : q.isError || !s ? (
        <div className="state" role="alert">
          <span className="state__title">상세를 불러오지 못했습니다</span>
        </div>
      ) : (
        <>
          <div className="manage-detail__warn muted" role="note">
            Secret 값은 평문입니다 — 노출·복사에 주의하세요.
          </div>
          <h3 className="manage-detail__heading">참조 Pod</h3>
          <PodList pods={s.pods} />
          <h3 className="manage-detail__heading">데이터 (key / value)</h3>
          <div className="secret-kv">
            {Object.keys(data).length === 0 && <p className="muted">키가 없습니다.</p>}
            {Object.entries(data).map(([k, v]) => (
              <label key={k} className="secret-kv__row">
                <span className="secret-kv__key ds-ident">{k}</span>
                <input
                  className="secret-kv__val"
                  type={reveal ? "text" : "password"}
                  value={v}
                  onChange={(e) => setEdited({ ...data, [k]: e.target.value })}
                  spellCheck={false}
                />
              </label>
            ))}
          </div>
        </>
      )}
    </Panel>
  );
}
