import { Link, useLocation, useParams } from "react-router-dom";
import { ISSUE_LABEL, RANGE_LABEL, type ContainerStatus, type OwnerRef } from "@k8s-dashboard/contracts";
import { drillKeys, useManagedDeployment, useManagedSecret, usePodDetail, useScope } from "@/api/queries";
import { useDashboardParams } from "@/state/useDashboardParams";
import { LineChart } from "@/components/LineChart";
import { Panel, StatTile, StatusBadge } from "@/components/primitives";
import { SectionView } from "@/components/SectionState";
import { Breadcrumb, OwnerChain, UsageBar, logsPath, withSearch } from "@/components/drill";
import { EventFeed } from "@/features/overview/panels";
import { num, since, unitSuffix } from "@/lib/format";
import { PageError, PageHeader, useDrillControls, useInvalidate } from "./common";

/**
 * Pod 상세 — 이슈 #15
 * Container phase · waiting reason · 배포 이미지 · Owner 체인 · 로그 연결.
 *
 * URL의 `uid`가 신원입니다. 이름이 같아도 재생성된 Pod는 다른 인스턴스이며,
 * 캐시 키와 화면 표시가 모두 UID를 따릅니다. (완료 기준)
 */
export function PodDetail() {
  const { search } = useLocation();
  const params = new URLSearchParams(search);
  const { name = "" } = useParams();
  const ns = params.get("ns") ?? "";
  const uid = params.get("uid") ?? "";
  const { clusterId, range, refreshMs } = useDashboardParams();
  const q = usePodDetail(clusterId, ns, name, uid, range, refreshMs);
  const invalidate = useInvalidate(drillKeys.pod(clusterId, ns, name, uid, range));
  const { controls } = useDrillControls(invalidate, q.isFetching, q.dataUpdatedAt || undefined);

  const pod = q.data?.pod.data;
  const ref = q.data?.generatedAt;
  const owner = pod?.owner;

  return (
    <div className="page">
      <PageHeader
        crumbs={
          <Breadcrumb
            items={[
              { label: "Cluster Overview", to: withSearch("/", search) },
              { label: "Namespaces", to: withSearch("/namespaces", search) },
              { label: ns, to: withSearch(`/namespaces/${encodeURIComponent(ns)}`, search) },
              ...(owner ? [{ label: owner.name }] : []),
              { label: name },
            ]}
          />
        }
        title={
          <span className="row">
            {name}
            {pod && (
              /* CrashLoopBackOff Pod의 phase는 실제로 Running입니다. phase만 보여주면
                 "왜 Running인데 빨간색이지?"가 되므로 컨테이너 사유를 함께 붙입니다. */
              <StatusBadge
                severity={pod.severity}
                label={pod.issues.length ? `${pod.phase} · ${ISSUE_LABEL[pod.issues[0]!]}` : pod.phase}
                small
              />
            )}
            {pod?.finishedAt && <span className="muted">종료된 인스턴스</span>}
          </span>
        }
        subtitle={
          <>
            {ns} · {clusterId} · {RANGE_LABEL[range]}
            {pod && (
              <>
                {" · "}
                <span className="ds-ident">UID {pod.uid}</span>
              </>
            )}
          </>
        }
        controls={controls}
        actions={
          pod ? (
            <div className="row row--wrap">
              <Link to={logsPath(pod.ref, search)}>이 Pod의 로그 열기 →</Link>
              <span className="muted">동일 시간 범위가 그대로 전달됩니다</span>
            </div>
          ) : undefined
        }
      />

      {q.isError ? (
        <PageError error={q.error} onRetry={invalidate} />
      ) : (
        <>
          {pod && (
            <div className="grid grid--kpi">
              <StatTile
                label="Phase"
                value={pod.phase}
                tone={pod.severity === "critical" ? "critical" : pod.severity === "warning" ? "warning" : undefined}
                footnote={
                  pod.issues.length
                    ? `컨테이너 사유: ${pod.issues.map((i) => ISSUE_LABEL[i]).join(" · ")}`
                    : "모든 컨테이너 정상"
                }
              />
              <StatTile label="Ready" value={pod.ready} footnote={`재시작 ${pod.restarts}회`} />
              <StatTile
                label="CPU / Request"
                value={`${(pod.usage.cpuVsRequest * 100).toFixed(0)}%`}
                footnote={`${num(pod.usage.cpuMilli)}m / ${num(pod.usage.cpuRequestMilli)}m`}
              />
              <StatTile
                label="Memory / Request"
                value={`${(pod.usage.memoryVsRequest * 100).toFixed(0)}%`}
                footnote={`${num(pod.usage.memoryMib)}MiB / ${num(pod.usage.memoryRequestMib)}MiB`}
              />
              <StatTile
                label="Node"
                value={<span className="ds-ident">{pod.node}</span>}
                footnote={
                  pod.finishedAt
                    ? `${since(pod.startedAt, ref!)} 시작 · ${since(pod.finishedAt, ref!)} 종료`
                    : `${since(pod.startedAt, ref!)} 시작`
                }
              />
            </div>
          )}

          <div className="grid grid--split">
            <Panel title="Container" section={q.data?.containers} referenceIso={ref} flush>
              <SectionView section={q.data?.containers} loading={q.isLoading} emptyTitle="Container 정보가 없습니다">
                {(containers) => <ContainerTable containers={containers} />}
              </SectionView>
            </Panel>

            <div className="grid">
              <Panel title="OwnerReference" subtitle="상위 소유 체인" section={q.data?.ownerChain} referenceIso={ref}>
                <SectionView section={q.data?.ownerChain} loading={q.isLoading} emptyTitle="소유자가 없습니다">
                  {(chain) => <OwnerChain chain={chain} />}
                </SectionView>
              </Panel>

              {pod && (
                <Panel title="식별 정보" section={q.data?.pod} referenceIso={ref}>
                  <div className="facts">
                    <div className="fact">
                      <span className="fact__label">Pod UID</span>
                      <span className="fact__value ds-ident">{pod.uid}</span>
                    </div>
                    <div className="fact">
                      <span className="fact__label">Namespace</span>
                      <span className="fact__value">{pod.namespace}</span>
                    </div>
                    <div className="fact">
                      <span className="fact__label">문제</span>
                      <span className="fact__value">
                        {pod.issues.length ? pod.issues.map((i) => ISSUE_LABEL[i]).join(" · ") : "없음"}
                      </span>
                    </div>
                    <div className="fact">
                      <span className="fact__label">CPU 사용</span>
                      <span className="fact__value">
                        <UsageBar ratio={pod.usage.cpuVsLimit} label="CPU Limit" />
                      </span>
                    </div>
                  </div>
                </Panel>
              )}
            </div>
          </div>

          {pod && (
            <PodManagedCards
              clusterId={clusterId}
              namespace={ns}
              search={search}
              deploymentName={ownerDeploymentName(q.data?.ownerChain.data)}
              secretRefs={q.data?.secretRefs ?? []}
            />
          )}

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

function ContainerTable({ containers }: { containers: ContainerStatus[] }) {
  return (
    <div className="panel__scroll">
      <table className="ds-data-table ds-data-table--compact">
        <thead>
          <tr>
            <th style={{ width: 110 }}>이름</th>
            <th style={{ width: 110 }}>상태</th>
            <th className="ds-num" style={{ width: 80 }}>
              재시작
            </th>
            <th style={{ width: 130 }}>Probe</th>
            <th>이미지 / 사유</th>
          </tr>
        </thead>
        <tbody>
          {containers.map((c) => (
            <tr key={c.name}>
              <td className="ds-ident">{c.name}</td>
              <td>
                <StatusBadge
                  severity={c.ready ? "healthy" : c.state === "Waiting" ? "critical" : "warning"}
                  label={c.reason ?? c.state}
                  small
                />
              </td>
              <td className="ds-num">{c.restarts}</td>
              <td className="muted">
                liveness {c.probes.liveness === "passing" ? "정상" : c.probes.liveness === "failing" ? "실패" : "없음"} ·
                readiness{" "}
                {c.probes.readiness === "passing" ? "정상" : c.probes.readiness === "failing" ? "실패" : "없음"}
              </td>
              <td>
                <div className="ds-ident" style={{ maxWidth: 520 }} title={c.imageId}>
                  {c.image}
                </div>
                {c.message && <div className="muted">{c.message}</div>}
                {c.lastTerminated && (
                  <div className="muted">
                    마지막 종료: {c.lastTerminated.reason} (exit {c.lastTerminated.exitCode})
                  </div>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/* ── Secret/Deployment 조회 카드 (#33) ─────────────────────────────────────
   pod 상세에서 연결된 Deployment/Secret을 순수 조회하고, details 버튼으로 각
   관리 탭의 해당 항목으로 이동합니다. 관리 권한이 있을 때만 노출합니다. */

function ownerDeploymentName(chain?: OwnerRef[]): string {
  return chain?.find((o) => o.kind === "Deployment")?.name ?? "";
}

function PodManagedCards({
  clusterId,
  namespace,
  search,
  deploymentName,
  secretRefs,
}: {
  clusterId: string;
  namespace: string;
  search: string;
  deploymentName: string;
  secretRefs: string[];
}) {
  const scope = useScope();
  if (!scope.data?.canManageWorkloads) return null;
  const dep = useManagedDeployment(clusterId, namespace, deploymentName, Boolean(deploymentName));
  const secretName = secretRefs[0] ?? "";
  const sec = useManagedSecret(clusterId, namespace, secretName, Boolean(secretName));

  return (
    <div className="grid grid--split">
      <Panel
        title="Deployment"
        subtitle="이 Pod의 상위 Deployment (조회)"
        actions={
          deploymentName ? (
            <Link to={withSearch("/deployments", search, { item: `${namespace}/${deploymentName}` })}>details →</Link>
          ) : undefined
        }
      >
        {!deploymentName ? (
          <p className="muted">Deployment 소유자가 없습니다.</p>
        ) : dep.isLoading ? (
          <p className="muted">불러오는 중…</p>
        ) : dep.data ? (
          <div className="facts">
            <div className="fact">
              <span className="fact__label">이름</span>
              <span className="fact__value ds-ident">{dep.data.name}</span>
            </div>
            <div className="fact">
              <span className="fact__label">Ready</span>
              <span className="fact__value">{dep.data.ready}/{dep.data.desired}</span>
            </div>
            <div className="fact">
              <span className="fact__label">Pod 수</span>
              <span className="fact__value">{dep.data.pods.length}</span>
            </div>
          </div>
        ) : (
          <p className="muted">조회할 수 없습니다.</p>
        )}
      </Panel>

      <Panel
        title="Secret"
        subtitle={secretRefs.length > 1 ? `이 Pod가 참조하는 Secret ${secretRefs.length}개 중 첫 번째` : "이 Pod가 참조하는 Secret (조회)"}
        actions={
          secretName ? (
            <Link to={withSearch("/secrets", search, { item: `${namespace}/${secretName}` })}>details →</Link>
          ) : undefined
        }
      >
        {!secretName ? (
          <p className="muted">참조하는 Secret이 없습니다.</p>
        ) : sec.isLoading ? (
          <p className="muted">불러오는 중…</p>
        ) : sec.data ? (
          <div className="facts">
            <div className="fact">
              <span className="fact__label">이름</span>
              <span className="fact__value ds-ident">{sec.data.name}</span>
            </div>
            <div className="fact">
              <span className="fact__label">타입</span>
              <span className="fact__value">{sec.data.secretType}</span>
            </div>
            <div className="fact">
              <span className="fact__label">키 수</span>
              <span className="fact__value">{Object.keys(sec.data.data).length}</span>
            </div>
            {secretRefs.length > 1 && (
              <div className="fact">
                <span className="fact__label">기타 참조</span>
                <span className="fact__value muted">{secretRefs.slice(1).join(", ")}</span>
              </div>
            )}
          </div>
        ) : (
          <p className="muted">조회할 수 없습니다.</p>
        )}
      </Panel>
    </div>
  );
}
