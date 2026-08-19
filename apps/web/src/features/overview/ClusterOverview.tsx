import { useQueryClient } from "@tanstack/react-query";
import { RANGE_LABEL } from "@k8s-dashboard/contracts";
import { useClusterOverview, useScope, queryKeys } from "@/api/queries";
import { HttpError } from "@/api/client";
import { useDashboardParams } from "@/state/useDashboardParams";
import { NAMESPACES } from "@/mocks/data";
import { LineChart } from "@/components/LineChart";
import { Panel, StatTile, StatusBadge } from "@/components/primitives";
import { ErrorState, ForbiddenState, LoadingState, SectionView } from "@/components/SectionState";
import { RefreshControl, ScopeSelector, TimeRangePicker } from "@/components/controls";
import { AlertSummaryCard, EventFeed, TopologySummaryCard, UnhealthyTable } from "./panels";
import { num, unitSuffix } from "@/lib/format";

/**
 * Cluster Overview — 이슈 #14
 * --------------------------------------------------------------------------
 * 운영자가 클러스터의 현재 건강 상태와 주요 이상을 **한 화면에서** 판단하는 진입 화면입니다.
 *
 * 완료 기준과 구현의 대응
 * - N+1 없음        : 화면 전체가 `/clusters/{id}/overview` 한 번의 요청입니다.
 * - 상태 구분       : 빈 결과 · 권한 없음 · upstream 장애를 SectionView가 각각 다르게 그립니다.
 * - 데이터만 갱신   : keepPreviousData + refetchInterval. 페이지를 다시 마운트하지 않습니다.
 * - 2클릭 드릴다운  : 이상 목록·이벤트의 이름이 곧 상세 링크입니다.
 */
export function ClusterOverview() {
  const { clusterId, namespace, range, refreshMs, patch } = useDashboardParams();
  const scope = useScope();
  const qc = useQueryClient();
  const overview = useClusterOverview({ clusterId, namespace, range, refreshMs });

  const data = overview.data;
  const ref = data?.generatedAt ?? new Date().toISOString();
  const firstLoad = overview.isLoading;

  /* 화면 전체가 403인 경우 — 섹션 단위 forbidden과 다릅니다. */
  const forbidden = overview.error instanceof HttpError && overview.error.status === 403;

  const pods = data?.pods.data;
  const nodes = data?.nodes.data;
  const workloads = data?.workloads.data;

  const controls = (
    <div className="row row--wrap">
      <ScopeSelector
        scope={scope.data}
        clusterId={clusterId}
        namespace={namespace}
        namespaces={NAMESPACES}
        onChange={(next) => patch(next)}
        disabled={scope.isLoading}
      />
      <TimeRangePicker range={range} onChange={(r) => patch({ range: r })} />
      <RefreshControl
        refreshMs={refreshMs}
        onChange={(ms) => patch({ refresh: ms })}
        onRefreshNow={() => qc.invalidateQueries({ queryKey: queryKeys.overview(clusterId, namespace, range) })}
        fetching={overview.isFetching}
        updatedAtMs={overview.dataUpdatedAt || undefined}
      />
    </div>
  );

  return (
    <div className="page">
      <header className="page__header">
        <div>
          <h1 className="page__title">Cluster Overview</h1>
          <p className="page__subtitle">
            {data ? `${data.clusterName} · ` : ""}
            {RANGE_LABEL[range]}
            {data && data.appliedScope.namespaces !== "all" && (
              <> · 적용된 Scope: {(data.appliedScope.namespaces as string[]).join(", ")}</>
            )}
          </p>
        </div>
        {controls}
      </header>

      {forbidden ? (
        <section className="panel">
          <div className="panel__body">
            <ForbiddenState detail={(overview.error as HttpError).body.message} />
          </div>
        </section>
      ) : overview.isError ? (
        <section className="panel">
          <div className="panel__body">
            <ErrorState
              detail={(overview.error as Error).message}
              onRetry={() => qc.invalidateQueries({ queryKey: queryKeys.overview(clusterId, namespace, range) })}
            />
          </div>
        </section>
      ) : (
        <>
          {/* ── KPI ───────────────────────────────────────────────────── */}
          <div className="grid grid--kpi">
            {firstLoad ? (
              <LoadingState lines={2} height={110} />
            ) : nodes ? (
              <StatTile
                label="Nodes Ready"
                value={`${nodes.ready}/${nodes.total}`}
                tone={nodes.notReady > 0 ? "critical" : nodes.pressure > 0 ? "warning" : undefined}
                footnote={
                  nodes.notReady > 0
                    ? `Critical · NotReady ${nodes.notReady} · Pressure ${nodes.pressure}`
                    : nodes.pressure > 0
                      ? `Warning · Pressure ${nodes.pressure}`
                      : "모든 노드 정상"
                }
              />
            ) : (
              /* nodes 섹션은 클러스터 범위 권한이 없으면 forbidden으로 옵니다.
                 로딩이 아니라 "권한 없음"을 명시해야 무한 스켈레톤이 사라집니다. (#버그) */
              <StatTile
                label="Nodes Ready"
                value="—"
                footnote={data?.nodes.status === "forbidden" ? "노드 상태는 클러스터 범위 권한이 필요합니다" : "노드 데이터 없음"}
              />
            )}
            {firstLoad || !pods ? (
              <LoadingState lines={2} height={110} />
            ) : (
              <StatTile
                label="Running Pods"
                value={num(pods.running)}
                footnote={`전체 ${num(pods.total)} · Pending ${pods.pending}`}
              />
            )}
            {firstLoad || !pods ? (
              <LoadingState lines={2} height={110} />
            ) : (
              <StatTile
                label="CrashLoopBackOff"
                value={num(pods.crashLoopBackOff)}
                tone={pods.crashLoopBackOff > 0 ? "critical" : undefined}
                delta={
                  pods.crashLoopBackOff > 0
                    ? { text: `↑ ${pods.crashLoopBackOff}`, kind: "bad" }
                    : { text: "→ 0", kind: "flat" }
                }
                footnote={
                  pods.crashLoopBackOff > 0
                    ? `Critical · ImagePull ${pods.imagePullBackOff} · Failed ${pods.failed}`
                    : "재시작 루프 없음"
                }
              />
            )}
            {firstLoad || !workloads ? (
              <LoadingState lines={2} height={110} />
            ) : (
              <StatTile
                label="Workload 이상"
                value={num(workloads.replicaMismatch + workloads.rolloutStalled)}
                tone={workloads.rolloutStalled > 0 ? "degraded" : workloads.replicaMismatch > 0 ? "warning" : undefined}
                footnote={`Replica 불일치 ${workloads.replicaMismatch} · Rollout 지연 ${workloads.rolloutStalled}`}
              />
            )}
            {firstLoad || !pods ? (
              <LoadingState lines={2} height={110} />
            ) : (
              <StatTile
                label="Container 재시작"
                value={num(pods.restarts)}
                delta={pods.restarts > 0 ? { text: "↑", kind: "bad" } : { text: "→", kind: "flat" }}
                footnote={`${RANGE_LABEL[range]} 합계`}
              />
            )}
          </div>

          {/* ── 추세 ──────────────────────────────────────────────────── */}
          <SectionView
            section={data?.trends}
            loading={firstLoad}
            emptyTitle="추세 데이터가 없습니다"
            emptyDetail="선택한 범위에 수집된 메트릭이 없습니다."
          >
            {(panels) => (
              <div className="grid grid--trends">
                {panels.map((p) => (
                  <Panel
                    key={p.id}
                    title={p.title}
                    subtitle={`${RANGE_LABEL[range]} · step ${p.stepSeconds >= 3600 ? `${p.stepSeconds / 3600}시간` : `${p.stepSeconds / 60}분`} · 단위 ${unitSuffix(p.series[0]!.unit)}`}
                    section={data?.trends}
                    referenceIso={ref}
                  >
                    <LineChart
                      series={p.series}
                      stepSeconds={p.stepSeconds}
                      ariaLabel={`${p.title} 시계열. ${p.series.map((s) => s.label).join(", ")}`}
                    />
                  </Panel>
                ))}
              </div>
            )}
          </SectionView>

          {/* ── 이상 엔티티 + 알림 ────────────────────────────────────── */}
          <div className="grid grid--split">
            <Panel
              title={
                <>
                  이상 엔티티 Top N
                  {data?.unhealthy.data && data.unhealthy.data.length > 0 && (
                    <StatusBadge
                      severity="critical"
                      label="조치 필요"
                      count={data.unhealthy.data.length}
                      small
                    />
                  )}
                </>
              }
              subtitle="이름을 클릭하면 상세 화면으로 이동합니다"
              section={data?.unhealthy}
              referenceIso={ref}
              flush
            >
              <SectionView
                section={data?.unhealthy}
                loading={firstLoad}
                emptyTitle="이상 엔티티가 없습니다"
                emptyDetail="현재 범위에서 비정상 상태인 Pod · Workload · Node가 없습니다."
              >
                {(items) => <UnhealthyTable items={items} referenceIso={ref} />}
              </SectionView>
            </Panel>

            <div className="grid" style={{ gap: "var(--layout-grid-gap)" }}>
              <Panel title="활성 Alert" subtitle="Grafana Alerting" section={data?.alerts} referenceIso={ref}>
                <SectionView
                  section={data?.alerts}
                  loading={firstLoad}
                  emptyTitle="활성 알림이 없습니다"
                >
                  {(alerts) => <AlertSummaryCard alerts={alerts} referenceIso={ref} />}
                </SectionView>
              </Panel>

              <Panel title="통신 경로 요약" subtitle="문제 경로만" section={data?.topology} referenceIso={ref}>
                <SectionView
                  section={data?.topology}
                  loading={firstLoad}
                  emptyTitle="통신 데이터가 없습니다"
                >
                  {(t) => <TopologySummaryCard topology={t} />}
                </SectionView>
              </Panel>
            </div>
          </div>

          {/* ── 최근 이벤트 ───────────────────────────────────────────── */}
          <Panel
            title="최근 Kubernetes Event"
            subtitle="Warning 우선"
            section={data?.events}
            referenceIso={ref}
            flush
          >
            <SectionView
              section={data?.events}
              loading={firstLoad}
              emptyTitle="최근 이벤트가 없습니다"
              emptyDetail="선택한 범위에 기록된 Warning 이벤트가 없습니다."
            >
              {(events) => <EventFeed events={events} referenceIso={ref} />}
            </SectionView>
          </Panel>
        </>
      )}
    </div>
  );
}
