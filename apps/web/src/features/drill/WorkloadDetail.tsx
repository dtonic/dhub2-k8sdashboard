import { Link, useLocation, useParams } from "react-router-dom";
import { ISSUE_LABEL, RANGE_LABEL, type PodSummary } from "@k8s-dashboard/contracts";
import { drillKeys, useWorkloadDetail } from "@/api/queries";
import { useDashboardParams } from "@/state/useDashboardParams";
import { LineChart } from "@/components/LineChart";
import { Panel, StatTile, StatusBadge } from "@/components/primitives";
import { SectionView } from "@/components/SectionState";
import { VirtualTable } from "@/components/VirtualTable";
import { Breadcrumb, OwnerChain, UsageBar, logsPath, withSearch } from "@/components/drill";
import { EventFeed } from "@/features/overview/panels";
import { duration, num, since, unitSuffix } from "@/lib/format";
import { PageError, PageHeader, useDrillControls, useInvalidate } from "./common";

const ROLLOUT_LABEL = {
  Complete: "완료",
  Progressing: "진행 중",
  Stalled: "지연",
  Paused: "일시정지",
} as const;

/**
 * Workload 상세 — 이슈 #15
 * desired/available/updated replica, rollout 상태, OwnerReference 체인, Pod 목록.
 */
export function WorkloadDetail() {
  const { search } = useLocation();
  const [params] = [new URLSearchParams(search)];
  const { kind = "Deployment", name = "" } = useParams();
  const ns = params.get("ns") ?? "";
  const { clusterId, range, refreshMs } = useDashboardParams();
  const q = useWorkloadDetail(clusterId, ns, kind, name, range, refreshMs);
  const invalidate = useInvalidate(drillKeys.workload(clusterId, ns, kind, name, range));
  const { controls } = useDrillControls(invalidate, q.isFetching, q.dataUpdatedAt || undefined);

  const w = q.data?.workload.data;
  const ref = q.data?.generatedAt;

  return (
    <div className="page">
      <PageHeader
        crumbs={
          <Breadcrumb
            items={[
              { label: "Cluster Overview", to: withSearch("/", search) },
              { label: "Namespaces", to: withSearch("/namespaces", search) },
              { label: ns, to: withSearch(`/namespaces/${encodeURIComponent(ns)}`, search) },
              { label: name },
            ]}
          />
        }
        title={
          <span className="row">
            {name}
            {w && <StatusBadge severity={w.severity} small />}
          </span>
        }
        subtitle={`${kind} · ${ns} · ${clusterId} · ${RANGE_LABEL[range]}`}
        controls={controls}
      />

      {q.isError ? (
        <PageError error={q.error} onRetry={invalidate} />
      ) : (
        <>
          {w && (
            <div className="grid grid--kpi">
              <StatTile
                label="Replica (ready/desired)"
                value={`${w.replicas.ready}/${w.replicas.desired}`}
                tone={w.replicas.ready < w.replicas.desired ? "warning" : undefined}
                footnote={`available ${w.replicas.available} · updated ${w.replicas.updated}`}
              />
              <StatTile
                label="Rollout"
                value={ROLLOUT_LABEL[w.rollout.status]}
                tone={w.rollout.status === "Stalled" ? "degraded" : undefined}
                footnote={w.rollout.message ?? "최신 세대가 모두 배포됨"}
              />
              <StatTile
                label="재시작"
                value={num(w.restarts)}
                delta={w.restarts > 0 ? { text: "↑", kind: "bad" } : { text: "→", kind: "flat" }}
                footnote={`${RANGE_LABEL[range]} 합계`}
              />
              <StatTile
                label="CPU / Request"
                value={`${(w.usage.cpuVsRequest * 100).toFixed(0)}%`}
                footnote={`${num(w.usage.cpuMilli)}m / ${num(w.usage.cpuRequestMilli)}m`}
              />
              <StatTile
                label="Memory / Request"
                value={`${(w.usage.memoryVsRequest * 100).toFixed(0)}%`}
                footnote={`${num(w.usage.memoryMib)}MiB / ${num(w.usage.memoryRequestMib)}MiB`}
              />
            </div>
          )}

          <div className="grid grid--split">
            <Panel
              title="Pod"
              subtitle="이름을 클릭하면 Pod 상세로 이동합니다"
              section={q.data?.pods}
              referenceIso={ref}
              flush
            >
              <SectionView section={q.data?.pods} loading={q.isLoading} emptyTitle="Pod가 없습니다">
                {(pods) => <PodTable pods={pods} search={search} referenceIso={ref!} />}
              </SectionView>
            </Panel>

            <div className="grid">
              <Panel
                title="OwnerReference"
                subtitle="Deployment → ReplicaSet → Pod"
                section={q.data?.ownerChain}
                referenceIso={ref}
              >
                <SectionView
                  section={q.data?.ownerChain}
                  loading={q.isLoading}
                  emptyTitle="중간 소유자가 없습니다"
                  emptyDetail={`${kind}은(는) Pod를 직접 소유합니다.`}
                >
                  {(chain) => <OwnerChain chain={chain} />}
                </SectionView>
              </Panel>

              {w && (
                <Panel title="배포 정보" section={q.data?.workload} referenceIso={ref}>
                  <div className="facts">
                    <div className="fact">
                      <span className="fact__label">이미지</span>
                      <span className="fact__value ds-ident">{w.images.join(", ")}</span>
                    </div>
                    <div className="fact">
                      <span className="fact__label">Workload UID</span>
                      <span className="fact__value ds-ident">{w.ref.workloadUid}</span>
                    </div>
                    <div className="fact">
                      <span className="fact__label">생성 후</span>
                      <span className="fact__value num">{duration(w.ageSeconds)}</span>
                    </div>
                    <div className="fact">
                      <span className="fact__label">문제</span>
                      <span className="fact__value">
                        {w.issues.length ? w.issues.map((i) => ISSUE_LABEL[i]).join(" · ") : "없음"}
                      </span>
                    </div>
                  </div>
                </Panel>
              )}
            </div>
          </div>

          <SectionView section={q.data?.trends} loading={q.isLoading} emptyTitle="추세 데이터가 없습니다">
            {(panels) => (
              <div className="grid grid--trends">
                {panels.map((p) => (
                  <Panel
                    key={p.id}
                    title={p.title}
                    subtitle={`${RANGE_LABEL[range]} · 단위 ${unitSuffix(p.series[0]!.unit)}`}
                    section={q.data?.trends}
                    referenceIso={ref}
                  >
                    <LineChart series={p.series} stepSeconds={p.stepSeconds} ariaLabel={`${name} ${p.title}`} />
                  </Panel>
                ))}
              </div>
            )}
          </SectionView>

          <Panel title="최근 Event" section={q.data?.events} referenceIso={ref} flush>
            <SectionView section={q.data?.events} loading={q.isLoading} emptyTitle="관련 이벤트가 없습니다">
              {(events) => <EventFeed events={events} referenceIso={ref!} />}
            </SectionView>
          </Panel>
        </>
      )}
    </div>
  );
}

const COLS = ["30%", "10%", "8%", "8%", "16%", "14%", "8%", "6%"];

function PodTable({ pods, search, referenceIso }: { pods: PodSummary[]; search: string; referenceIso: string }) {
  return (
    <VirtualTable
      items={pods}
      rowHeight={34}
      height={380}
      /* key는 이름이 아니라 UID입니다. 재생성된 동명 Pod가 같은 행으로 합쳐지면 안 됩니다. */
      getKey={(p) => p.uid}
      header={
        <tr>
          <th style={{ width: COLS[0] }}>Pod</th>
          <th style={{ width: COLS[1] }}>Phase</th>
          <th style={{ width: COLS[2] }}>Ready</th>
          <th className="ds-num" style={{ width: COLS[3] }}>
            재시작
          </th>
          <th style={{ width: COLS[4] }}>CPU / Request</th>
          <th style={{ width: COLS[5] }}>Node</th>
          <th style={{ width: COLS[6] }}>시작</th>
          <th style={{ width: COLS[7] }}>로그</th>
        </tr>
      }
      renderRow={(p) => (
        <>
          <td className="ds-ident" style={{ width: COLS[0] }}>
            <Link to={withSearch(`/pods/${encodeURIComponent(p.name)}`, search, { ns: p.namespace, uid: p.uid })}>
              {p.name}
            </Link>
            {p.finishedAt && <span className="muted"> · 종료된 인스턴스</span>}
          </td>
          <td style={{ width: COLS[1] }}>
            <StatusBadge severity={p.severity} label={p.phase} small />
          </td>
          <td style={{ width: COLS[2] }}>{p.ready}</td>
          <td className="ds-num" style={{ width: COLS[3] }}>
            {p.restarts}
          </td>
          <td style={{ width: COLS[4] }}>
            <UsageBar ratio={p.usage.cpuVsRequest} label="CPU Request" />
          </td>
          <td className="ds-ident" style={{ width: COLS[5] }}>
            {p.node}
          </td>
          <td className="muted num" style={{ width: COLS[6] }}>
            {since(p.startedAt, referenceIso)}
          </td>
          <td style={{ width: COLS[7] }}>
            <Link to={logsPath(p.ref, search)}>열기</Link>
          </td>
        </>
      )}
    />
  );
}
