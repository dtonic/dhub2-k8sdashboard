import { Link, useLocation, useParams } from "react-router-dom";
import { ISSUE_LABEL, RANGE_LABEL, type IssueReason, type WorkloadSummary } from "@k8s-dashboard/contracts";
import { drillKeys, useNamespaceDetail } from "@/api/queries";
import { useDashboardParams } from "@/state/useDashboardParams";
import { LineChart } from "@/components/LineChart";
import { Panel, StatTile, StatusBadge } from "@/components/primitives";
import { SectionView } from "@/components/SectionState";
import { VirtualTable } from "@/components/VirtualTable";
import { Breadcrumb, IssueFilter, UsageBar, withSearch } from "@/components/drill";
import { EventFeed } from "@/features/overview/panels";
import { duration, num, unitSuffix } from "@/lib/format";
import { PageError, PageHeader, useDrillControls, useInvalidate, useIssueFilter } from "./common";

/**
 * Namespace 상세 — 이슈 #15
 * Deployment/StatefulSet/DaemonSet 상태 표, 사용량 비율, 상태 필터, 추세, 이벤트.
 * Workload가 수백 개여도 견디도록 목록은 가상 스크롤입니다.
 */
export function NamespaceDetail() {
  const { search } = useLocation();
  const { namespace = "" } = useParams();
  const { clusterId, range, refreshMs } = useDashboardParams();
  const q = useNamespaceDetail(clusterId, namespace, range, refreshMs);
  const invalidate = useInvalidate(drillKeys.namespace(clusterId, namespace, range));
  const { controls } = useDrillControls(invalidate, q.isFetching, q.dataUpdatedAt || undefined);
  const filter = useIssueFilter();

  const summary = q.data?.summary.data;
  const workloads = q.data?.workloads.data ?? [];
  const available = [...new Set(workloads.flatMap((w) => w.issues))] as IssueReason[];
  const counts = Object.fromEntries(
    available.map((r) => [r, workloads.filter((w) => w.issues.includes(r)).length]),
  ) as Partial<Record<IssueReason, number>>;
  const shown = filter.selected.length
    ? workloads.filter((w) => filter.selected.every((r) => w.issues.includes(r)))
    : workloads;

  return (
    <div className="page">
      <PageHeader
        crumbs={
          <Breadcrumb
            items={[
              { label: "Cluster Overview", to: withSearch("/", search) },
              { label: "Namespaces", to: withSearch("/namespaces", search) },
              { label: namespace },
            ]}
          />
        }
        title={namespace}
        subtitle={`${clusterId} · ${RANGE_LABEL[range]}`}
        controls={controls}
        actions={
          <IssueFilter
            available={available}
            selected={filter.selected}
            counts={counts}
            onToggle={filter.toggle}
            onClear={filter.clear}
          />
        }
      />

      {q.isError ? (
        <PageError error={q.error} onRetry={invalidate} />
      ) : (
        <>
          {summary && (
            <div className="grid grid--kpi">
              <StatTile
                label="Workload"
                value={`${num(summary.workloads.total - summary.workloads.unhealthy)}/${num(summary.workloads.total)}`}
                tone={summary.workloads.unhealthy > 0 ? "warning" : undefined}
                footnote={`이상 ${summary.workloads.unhealthy}개`}
              />
              <StatTile
                label="Pod"
                value={`${num(summary.pods.running)}/${num(summary.pods.total)}`}
                tone={summary.pods.failed > 0 ? "critical" : undefined}
                footnote={`Pending ${summary.pods.pending} · Failed ${summary.pods.failed}`}
              />
              <StatTile
                label="재시작"
                value={num(summary.pods.restarts)}
                delta={summary.pods.restarts > 0 ? { text: "↑", kind: "bad" } : { text: "→", kind: "flat" }}
                footnote={`${RANGE_LABEL[range]} 합계`}
              />
              <StatTile
                label="CPU / Request"
                value={`${(summary.usage.cpuVsRequest * 100).toFixed(0)}%`}
                footnote={`${num(summary.usage.cpuMilli)}m / ${num(summary.usage.cpuRequestMilli)}m`}
              />
              <StatTile
                label="Memory / Request"
                value={`${(summary.usage.memoryVsRequest * 100).toFixed(0)}%`}
                footnote={`${num(summary.usage.memoryMib)}MiB / ${num(summary.usage.memoryRequestMib)}MiB`}
              />
            </div>
          )}

          <Panel
            title="Workload"
            subtitle={`${num(shown.length)}/${num(workloads.length)}개 표시 · 이름을 클릭하면 상세로 이동합니다`}
            section={q.data?.workloads}
            referenceIso={q.data?.generatedAt}
            flush
          >
            <SectionView
              section={q.data?.workloads}
              loading={q.isLoading}
              emptyTitle="Workload가 없습니다"
              emptyDetail="이 Namespace에 배포된 Workload가 없습니다."
            >
              {() => <WorkloadTable items={shown} search={search} />}
            </SectionView>
          </Panel>

          <SectionView
            section={q.data?.trends}
            loading={q.isLoading}
            emptyTitle="추세 데이터가 없습니다"
          >
            {(panels) => (
              <div className="grid grid--trends">
                {panels.map((p) => (
                  <Panel
                    key={p.id}
                    title={p.title}
                    subtitle={`${RANGE_LABEL[range]} · 단위 ${unitSuffix(p.series[0]!.unit)}`}
                    section={q.data?.trends}
                    referenceIso={q.data?.generatedAt}
                  >
                    <LineChart series={p.series} stepSeconds={p.stepSeconds} ariaLabel={`${namespace} ${p.title}`} />
                  </Panel>
                ))}
              </div>
            )}
          </SectionView>

          <Panel title="최근 Event" section={q.data?.events} referenceIso={q.data?.generatedAt} flush>
            <SectionView section={q.data?.events} loading={q.isLoading} emptyTitle="최근 이벤트가 없습니다">
              {(events) => <EventFeed events={events} referenceIso={q.data!.generatedAt} />}
            </SectionView>
          </Panel>
        </>
      )}
    </div>
  );
}

const COLS = ["30%", "9%", "10%", "10%", "8%", "15%", "18%"];

function WorkloadTable({ items, search }: { items: WorkloadSummary[]; search: string }) {
  if (items.length === 0) {
    return (
      <div className="state">
        <span className="state__glyph" aria-hidden="true">
          ✓
        </span>
        <span className="state__title">필터에 맞는 Workload가 없습니다</span>
      </div>
    );
  }
  return (
    <VirtualTable
      items={items}
      rowHeight={34}
      height={440}
      getKey={(w) => w.ref.workloadUid ?? w.name}
      header={
        <tr>
          <th style={{ width: COLS[0] }}>이름</th>
          <th style={{ width: COLS[1] }}>Kind</th>
          <th style={{ width: COLS[2] }}>상태</th>
          <th className="ds-num" style={{ width: COLS[3] }}>
            Replica
          </th>
          <th className="ds-num" style={{ width: COLS[4] }}>
            재시작
          </th>
          <th style={{ width: COLS[5] }}>CPU / Request</th>
          <th style={{ width: COLS[6] }}>이미지</th>
        </tr>
      }
      renderRow={(w) => (
        <>
          <td className="ds-ident" style={{ width: COLS[0] }}>
            <Link to={withSearch(`/workloads/${w.kind}/${encodeURIComponent(w.name)}`, search, { ns: w.namespace })}>
              {w.name}
            </Link>
          </td>
          <td style={{ width: COLS[1] }}>{w.kind}</td>
          <td style={{ width: COLS[2] }}>
            <StatusBadge
              severity={w.severity}
              label={w.issues.length ? ISSUE_LABEL[w.issues[0]!] : "Healthy"}
              small
            />
          </td>
          <td className="ds-num" style={{ width: COLS[3] }}>
            {w.replicas.ready}/{w.replicas.desired}
          </td>
          <td className="ds-num" style={{ width: COLS[4] }}>
            {w.restarts}
          </td>
          <td style={{ width: COLS[5] }}>
            <UsageBar ratio={w.usage.cpuVsRequest} label="CPU Request" />
          </td>
          <td className="ds-ident" style={{ width: COLS[6] }} title={`${w.images[0]} · age ${duration(w.ageSeconds)}`}>
            {w.images[0]}
          </td>
        </>
      )}
    />
  );
}
