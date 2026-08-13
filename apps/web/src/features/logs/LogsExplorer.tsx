import { useCallback, useMemo } from "react";
import { useLocation, useSearchParams } from "react-router-dom";
import { LEVEL_ORDER, RANGE_LABEL, type LogLevel } from "@k8s-dashboard/contracts";
import { logKeys, useLogSearch, type LogFilters } from "@/api/queries";
import { useDashboardParams } from "@/state/useDashboardParams";
import { Panel } from "@/components/primitives";
import { LoadingState, SectionView } from "@/components/SectionState";
import { LogHistogram } from "@/components/LogHistogram";
import { Breadcrumb, withSearch } from "@/components/drill";
import { dayClock, num } from "@/lib/format";
import { PageError, PageHeader, useDrillControls, useInvalidate } from "@/features/drill/common";
import { LogList } from "./LogList";

/**
 * Logs Explorer — 이슈 #16
 * --------------------------------------------------------------------------
 * 그래프의 이상 구간에서 관련 로그와 Kubernetes Event를 **같은 Scope·시간 범위**로
 * 조사하는 화면입니다.
 *
 * 완료 기준과 구현의 대응
 * - 대량 결과를 한 번에 적재하지 않음 : 커서 페이지(100줄) + 라인 가상 스크롤,
 *   서버가 결과 상한을 강제하고 걸리면 화면에 명시합니다.
 * - 다음 페이지에 중복·누락 없음     : offset이 아니라 (timestamp, id) 커서를 씁니다.
 * - Metric → Log → Event 이동 시     : Scope·시간 범위·구간 선택이 모두 URL에 있습니다.
 *   Scope·범위 유지                    Pod 상세에서 넘어올 때 ns/uid/range를 그대로 받습니다.
 * - 마스킹이 API와 UI 모두에 적용     : 서버가 가린 문자열만 내려오고, UI는 가려진 구간을
 *                                      표시만 하며 복사도 보이는 내용만 됩니다.
 */
export function LogsExplorer() {
  const { search } = useLocation();
  const [params, setParams] = useSearchParams();
  const { clusterId, namespace, range } = useDashboardParams();

  const levels = (params.get("levels") ?? "").split(",").filter(Boolean) as LogLevel[];
  const workload = params.get("workload") ?? "";
  const podUid = params.get("uid") ?? "";
  const container = params.get("container") ?? "";
  const q = params.get("q") ?? "";
  const winFrom = Number(params.get("from") ?? 0) || undefined;
  const winTo = Number(params.get("to") ?? 0) || undefined;

  const filters: LogFilters = useMemo(
    () => ({ clusterId, namespace, workload, podUid, container, levels, q, range, from: winFrom, to: winTo }),
    [clusterId, namespace, workload, podUid, container, levels.join(","), q, range, winFrom, winTo],
  );

  const qy = useLogSearch(filters);
  const invalidate = useInvalidate(logKeys.search(filters));
  const { controls } = useDrillControls(invalidate, qy.isFetching, qy.dataUpdatedAt || undefined);

  const patch = useCallback(
    (next: Record<string, string | null>) => {
      setParams(
        (prev) => {
          const p = new URLSearchParams(prev);
          for (const [k, v] of Object.entries(next)) {
            if (v === null || v === "") p.delete(k);
            else p.set(k, v);
          }
          return p;
        },
        { replace: true },
      );
    },
    [setParams],
  );

  const first = qy.data?.pages[0];
  const lines = useMemo(() => (qy.data?.pages ?? []).flatMap((p) => p.lines.data ?? []), [qy.data]);
  const facets = first?.facets.data;
  const applied = first?.applied;
  const from = applied ? Date.parse(applied.from) : 0;
  const to = applied ? Date.parse(applied.to) : 0;
  const selection = winFrom && winTo ? { from: winFrom, to: winTo } : null;

  const levelCounts = useMemo(() => {
    const h = first?.histogram.data ?? [];
    return Object.fromEntries(
      LEVEL_ORDER.map((l) => [l, h.reduce((s, b) => s + b.counts[l], 0)]),
    ) as Record<LogLevel, number>;
  }, [first]);

  return (
    <div className="page">
      <PageHeader
        crumbs={
          <Breadcrumb
            items={[
              { label: "Cluster Overview", to: withSearch("/", search) },
              { label: "Namespaces", to: withSearch("/namespaces", search) },
              ...(namespace !== "all"
                ? [{ label: namespace, to: withSearch(`/namespaces/${encodeURIComponent(namespace)}`, search) }]
                : []),
              { label: "Logs Explorer" },
            ]}
          />
        }
        title="Logs Explorer"
        subtitle={
          applied ? (
            <>
              {clusterId} · {applied.namespace ?? "모든 Namespace"} ·{" "}
              {selection ? (
                <>
                  선택 구간 {dayClock(from)} – {dayClock(to)}
                </>
              ) : (
                RANGE_LABEL[range]
              )}
            </>
          ) : (
            RANGE_LABEL[range]
          )
        }
        controls={controls}
      />

      {qy.isError ? (
        <PageError error={qy.error} onRetry={invalidate} />
      ) : (
        <>
          <Panel
            title="레벨 분포와 Event 타임라인"
            subtitle="드래그하면 그 구간으로 좁힙니다 · 점선은 Kubernetes Event"
            section={first?.histogram}
            referenceIso={first?.generatedAt}
          >
            {qy.isLoading || !first ? (
              <LoadingState lines={3} height={180} />
            ) : (
              <LogHistogram
                buckets={first.histogram.data ?? []}
                events={first.events.data ?? []}
                from={from}
                to={to}
                selection={selection}
                onSelect={(w) => patch({ from: String(w.from), to: String(w.to) })}
                onClear={() => patch({ from: null, to: null })}
              />
            )}
          </Panel>

          <Panel
            title="필터"
            subtitle="선택은 URL에 남습니다 · 레벨 칩의 숫자는 레벨 필터를 빼고 센 값입니다"
          >
            <div className="row row--wrap" style={{ gap: "var(--space-5)", alignItems: "flex-end" }}>
              <div className="chips" role="group" aria-label="로그 레벨">
                {LEVEL_ORDER.map((l) => (
                  <button
                    key={l}
                    type="button"
                    className="chip"
                    aria-pressed={levels.includes(l)}
                    onClick={() => {
                      const next = levels.includes(l) ? levels.filter((x) => x !== l) : [...levels, l];
                      patch({ levels: next.join(",") });
                    }}
                  >
                    {l}
                    <span className="chip__count num">{num(levelCounts[l] ?? 0)}</span>
                  </button>
                ))}
              </div>

              <label className="field">
                <span className="field__label">Workload</span>
                <select value={workload} onChange={(e) => patch({ workload: e.target.value, uid: null })}>
                  <option value="">전체</option>
                  {(facets?.workloads ?? []).map((w) => (
                    <option key={w.name} value={w.name}>
                      {w.name} ({num(w.count)})
                    </option>
                  ))}
                </select>
              </label>

              <label className="field">
                <span className="field__label">Pod</span>
                <select value={podUid} onChange={(e) => patch({ uid: e.target.value })}>
                  <option value="">전체</option>
                  {(facets?.pods ?? []).map((p) => (
                    <option key={p.uid} value={p.uid}>
                      {p.name} ({num(p.count)})
                    </option>
                  ))}
                </select>
              </label>

              <label className="field">
                <span className="field__label">Container</span>
                <select value={container} onChange={(e) => patch({ container: e.target.value })}>
                  <option value="">전체</option>
                  {(facets?.containers ?? []).map((c) => (
                    <option key={c.name} value={c.name}>
                      {c.name} ({num(c.count)})
                    </option>
                  ))}
                </select>
              </label>

              <label className="field field--grow">
                <span className="field__label">검색어</span>
                <input
                  type="search"
                  value={q}
                  placeholder="메시지 포함 문자열 (서버에서 이스케이프 처리)"
                  onChange={(e) => patch({ q: e.target.value })}
                />
              </label>

              {(levels.length || workload || podUid || container || q || selection) && (
                <button
                  type="button"
                  className="linkish"
                  onClick={() =>
                    patch({ levels: null, workload: null, uid: null, container: null, q: null, from: null, to: null })
                  }
                >
                  전체 해제
                </button>
              )}
            </div>
          </Panel>

          {applied?.truncated && (
            <div className="notice notice--warning">
              <span aria-hidden="true">!</span>
              결과가 서버 상한 <span className="num">{num(applied.maxLines)}</span>줄에서 잘렸습니다. 시간 범위를
              좁히거나 필터를 추가하세요. 상한은 서버가 강제합니다.
            </div>
          )}

          <Panel
            title="로그"
            subtitle={
              first
                ? `커서 페이징 · 페이지당 ${first.cursor.pageSize}줄${applied?.truncated ? " · 상한 도달" : ""}`
                : undefined
            }
            section={first?.lines}
            referenceIso={first?.generatedAt}
            flush
          >
            <SectionView
              section={first?.lines}
              loading={qy.isLoading}
              emptyTitle="조건에 맞는 로그가 없습니다"
              emptyDetail="시간 범위를 넓히거나 필터를 해제해 보세요."
            >
              {() => (
                <LogList
                  lines={lines}
                  hasMore={Boolean(qy.hasNextPage)}
                  loadingMore={qy.isFetchingNextPage}
                  onLoadMore={() => void qy.fetchNextPage()}
                  search={search}
                />
              )}
            </SectionView>
          </Panel>
        </>
      )}
    </div>
  );
}
