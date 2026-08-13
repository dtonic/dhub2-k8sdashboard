import { useState } from "react";
import { Link, useLocation, useSearchParams } from "react-router-dom";
import {
  ALERT_SEVERITY_LABEL,
  ALERT_STATUS_LABEL,
  RANGE_LABEL,
  type AlertInstance,
  type AlertSeverity,
} from "@k8s-dashboard/contracts";
import { alertKeys, useAlerts } from "@/api/queries";
import { useDashboardParams } from "@/state/useDashboardParams";
import { Panel, StatusBadge, StatusDot } from "@/components/primitives";
import { SectionView } from "@/components/SectionState";
import { Breadcrumb, logsPath, refPath, withSearch } from "@/components/drill";
import { dayClock, duration, num, since } from "@/lib/format";
import { PageError, PageHeader, useDrillControls, useInvalidate } from "@/features/drill/common";

const SEV_TO_STATUS = { critical: "critical", warning: "warning", info: "progressing" } as const;

/**
 * Alerts — 이슈 #17
 * --------------------------------------------------------------------------
 * **자체 평가 엔진을 만들지 않습니다.** Grafana Alerting / Alertmanager가 판단한 결과를
 * 공통 모델로 정규화해 조회만 합니다. Rule 편집 · Silence · Routing 변경은 MVP 비목표이며
 * 화면에서도 제공하지 않고 원본 시스템으로 보냅니다. (README §2-7)
 *
 * 완료 기준과 구현의 대응
 * - Active/Resolved 일관 표시 : 같은 표 형식·같은 상태 어휘를 쓰고 탭으로만 나눕니다.
 * - 관련 Workload/Pod 이동    : label에서 매핑된 EntityRef로 상세·로그 deep link를 겁니다.
 * - Alert backend 장애 격리   : Section이 degraded로 내려와도 화면과 다른 패널은 살아 있습니다.
 * - 중복 grouping 기준 문서화 : 응답의 groupingRule을 화면에 그대로 노출합니다.
 */
export function AlertsView() {
  const { search } = useLocation();
  const [params, setParams] = useSearchParams();
  const { clusterId, namespace, range, refreshMs } = useDashboardParams();
  const q = useAlerts(clusterId, namespace, range, refreshMs);
  const invalidate = useInvalidate(alertKeys.list(clusterId, namespace, range));
  const { controls } = useDrillControls(invalidate, q.isFetching, q.dataUpdatedAt || undefined);

  const tab = (params.get("tab") === "resolved" ? "resolved" : "firing") as "firing" | "resolved";
  const setTab = (next: string) =>
    setParams(
      (prev) => {
        const p = new URLSearchParams(prev);
        p.set("tab", next);
        p.delete("alert");
        return p;
      },
      { replace: true },
    );

  const section = tab === "firing" ? q.data?.firing : q.data?.resolved;
  const list = section?.data ?? [];
  const selectedId = params.get("alert") ?? list[0]?.id ?? null;
  const selected = list.find((a) => a.id === selectedId) ?? null;
  const ref = q.data?.generatedAt;

  return (
    <div className="page">
      <PageHeader
        crumbs={
          <Breadcrumb
            items={[
              { label: "Cluster Overview", to: withSearch("/", search) },
              { label: "Alerts" },
            ]}
          />
        }
        title="Alerts"
        subtitle={`${clusterId} · ${namespace === "all" ? "모든 Namespace" : namespace} · ${RANGE_LABEL[range]} · Grafana Alerting / Alertmanager 조회`}
        controls={controls}
      />

      {q.isError ? (
        <PageError error={q.error} onRetry={invalidate} />
      ) : (
        <>
          <div className="notice">
            <span aria-hidden="true">i</span>
            <span>
              이 화면은 <strong>조회 전용</strong>입니다. Rule 편집 · Silence · Notification Routing 변경은 MVP
              비목표이며 원본 시스템에서 수행합니다.
            </span>
          </div>

          <Panel
            title="심각도별 현황"
            subtitle={q.data ? `Grouping 기준 — ${q.data.groupingRule}` : undefined}
            section={q.data?.counts}
            referenceIso={ref}
          >
            <SectionView section={q.data?.counts} loading={q.isLoading} emptyTitle="집계가 없습니다">
              {(c) => (
                <div className="severity-bar">
                  {(Object.keys(c) as AlertSeverity[]).map((sev) => (
                    <span key={sev} className="severity-chip">
                      <StatusDot severity={SEV_TO_STATUS[sev]} />
                      <span className="severity-chip__count">{num(c[sev].firing)}</span>
                      <span className="severity-chip__label">
                        {ALERT_SEVERITY_LABEL[sev]} <span className="muted">· 해소 {num(c[sev].resolved)}</span>
                      </span>
                    </span>
                  ))}
                </div>
              )}
            </SectionView>
          </Panel>

          <div className="grid grid--split">
            <Panel
              title={
                <span className="chips" role="tablist" aria-label="알림 상태">
                  <button type="button" className="chip" role="tab" aria-selected={tab === "firing"} aria-pressed={tab === "firing"} onClick={() => setTab("firing")}>
                    진행 중
                    <span className="chip__count num">{num(q.data?.firing.data?.length ?? 0)}</span>
                  </button>
                  <button type="button" className="chip" role="tab" aria-selected={tab === "resolved"} aria-pressed={tab === "resolved"} onClick={() => setTab("resolved")}>
                    해소됨
                    <span className="chip__count num">{num(q.data?.resolved.data?.length ?? 0)}</span>
                  </button>
                </span>
              }
              subtitle={tab === "firing" ? "지금 발생 중인 알림" : `${RANGE_LABEL[range]} 안에 해소된 알림`}
              section={section}
              referenceIso={ref}
              flush
            >
              <SectionView
                section={section}
                loading={q.isLoading}
                emptyTitle={tab === "firing" ? "진행 중인 알림이 없습니다" : "해소된 알림이 없습니다"}
                emptyDetail={tab === "firing" ? "현재 Scope에서 발생 중인 알림이 없습니다." : "선택한 범위에 해소된 알림이 없습니다."}
              >
                {(items) => (
                  <div className="panel__scroll" style={{ maxHeight: 520 }}>
                    <table className="ds-data-table ds-data-table--compact ds-data-table--interactive">
                      <thead>
                        <tr>
                          <th>알림</th>
                          <th style={{ width: 150 }}>대상</th>
                          <th style={{ width: 96 }}>시작</th>
                          <th style={{ width: 96 }}>{tab === "firing" ? "지속" : "해소"}</th>
                          <th style={{ width: 70 }}>그룹</th>
                        </tr>
                      </thead>
                      <tbody>
                        {items.map((a) => (
                          <tr
                            key={a.id}
                            tabIndex={0}
                            aria-selected={a.id === selectedId}
                            onClick={() =>
                              setParams(
                                (prev) => {
                                  const p = new URLSearchParams(prev);
                                  p.set("alert", a.id);
                                  return p;
                                },
                                { replace: true },
                              )
                            }
                            onKeyDown={(ev) => {
                              if (ev.key !== "Enter") return;
                              setParams(
                                (prev) => {
                                  const p = new URLSearchParams(prev);
                                  p.set("alert", a.id);
                                  return p;
                                },
                                { replace: true },
                              );
                            }}
                          >
                            <td>
                              <StatusBadge severity={SEV_TO_STATUS[a.severity]} label={a.name} small />
                            </td>
                            <td className="ds-ident">{a.entityName ?? "—"}</td>
                            <td className="num muted">{since(a.startsAt, ref!)}</td>
                            <td className="num muted">
                              {a.status === "firing"
                                ? duration((Date.parse(ref!) - Date.parse(a.startsAt)) / 1000)
                                : since(a.endsAt!, ref!)}
                            </td>
                            <td className="ds-num">{a.groupSize > 1 ? `${a.groupSize}건` : "—"}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </SectionView>
            </Panel>

            <AlertDetail alert={selected} referenceIso={ref} search={search} groupingRule={q.data?.groupingRule} />
          </div>
        </>
      )}
    </div>
  );
}

function AlertDetail({
  alert,
  referenceIso,
  search,
  groupingRule,
}: {
  alert: AlertInstance | null;
  referenceIso?: string;
  search: string;
  groupingRule?: string;
}) {
  const [showLabels, setShowLabels] = useState(false);

  if (!alert) {
    return (
      <Panel title="알림 상세">
        <div className="state">
          <span className="state__glyph" aria-hidden="true">
            →
          </span>
          <span className="state__title">목록에서 알림을 선택하세요</span>
        </div>
      </Panel>
    );
  }

  return (
    <Panel
      title={
        <span className="row">
          {alert.name}
          <StatusBadge severity={SEV_TO_STATUS[alert.severity]} label={ALERT_STATUS_LABEL[alert.status]} small />
        </span>
      }
      subtitle={`${alert.source === "grafana" ? "Grafana Alerting" : "Alertmanager"} · ${alert.severity}`}
    >
      <div style={{ display: "flex", flexDirection: "column", gap: "var(--space-5)" }}>
        <div className="facts">
          <div className="fact">
            <span className="fact__label">시작</span>
            <span className="fact__value num">{dayClock(Date.parse(alert.startsAt))}</span>
          </div>
          <div className="fact">
            <span className="fact__label">{alert.status === "firing" ? "지속" : "해소"}</span>
            <span className="fact__value num">
              {alert.status === "firing"
                ? duration((Date.parse(referenceIso!) - Date.parse(alert.startsAt)) / 1000)
                : dayClock(Date.parse(alert.endsAt!))}
            </span>
          </div>
          <div className="fact">
            <span className="fact__label">대상</span>
            <span className="fact__value ds-ident">{alert.entityName ?? "매핑 없음"}</span>
          </div>
          <div className="fact">
            <span className="fact__label">묶인 인스턴스</span>
            <span className="fact__value num">{alert.groupSize}건</span>
          </div>
        </div>

        {alert.annotations.summary && (
          <div>
            <div style={{ font: "var(--type-body-strong)" }}>{alert.annotations.summary}</div>
            {alert.annotations.description && (
              <div className="muted" style={{ marginTop: "var(--space-2)" }}>
                {alert.annotations.description}
              </div>
            )}
          </div>
        )}

        {/* Metric → Log → Event 동선. Scope와 시간 범위를 그대로 넘깁니다. */}
        <div className="row row--wrap">
          {alert.entity ? (
            <>
              <Link to={refPath(alert.entity, search)}>관련 대상 상세 →</Link>
              <Link to={logsPath(alert.entity, search)}>관련 로그 →</Link>
            </>
          ) : (
            <span className="muted" style={{ font: "var(--type-meta)" }}>
              label에서 Entity를 특정하지 못했습니다. 원본 시스템에서 확인하세요.
            </span>
          )}
          {alert.sourceUrl && (
            <span className="muted" style={{ font: "var(--type-meta)" }}>
              원본: <span className="ds-ident">{alert.sourceUrl}</span>
            </span>
          )}
        </div>

        <div>
          <button type="button" className="linkish" aria-expanded={showLabels} onClick={() => setShowLabels((v) => !v)}>
            {showLabels ? "라벨 접기" : `라벨 ${Object.keys(alert.labels).length}개 보기`}
          </button>
          {showLabels && (
            <dl className="logline__fields" style={{ marginTop: "var(--space-4)" }}>
              {Object.entries(alert.labels).map(([k, v]) => (
                <div key={k}>
                  <dt>{k}</dt>
                  <dd className="ds-ident">{v}</dd>
                </div>
              ))}
            </dl>
          )}
        </div>

        {alert.groupSize > 1 && groupingRule && (
          <div className="notice notice--warning">
            <span aria-hidden="true">!</span>
            <span>
              같은 규칙의 인스턴스 {alert.groupSize}건이 한 줄로 묶였습니다. 기준: {alert.groupKey}
            </span>
          </div>
        )}
      </div>
    </Panel>
  );
}
