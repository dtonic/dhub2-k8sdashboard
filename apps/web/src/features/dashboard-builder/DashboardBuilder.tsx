import { useEffect, useRef, useState } from "react";
import { Link, useLocation, useNavigate, useParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { validateDashboard } from "@k8s-dashboard/dashboard-schema";
import type { DashboardDefinition, DashboardLayout, DashboardWidget, WidgetType } from "@k8s-dashboard/dashboard-schema";
import { apiDownload, apiGet, apiRequest, HttpError } from "@/api/client";
import { ErrorState, ForbiddenState, LoadingState } from "@/components/SectionState";
import { withSearch } from "@/components/drill";
import { embeddedDashboardBindings, embeddedDashboards } from "@/generated/dashboards";
import { ResolvedDashboard } from "@/features/dashboards/DashboardView";
import { placeWidget, updateWidgetLayout } from "./layout";
import { acquireSingleFlight } from "./singleFlight";

type Capabilities = { enabled: boolean; canEdit: boolean; canPublish: boolean; maxDrafts: number; maxWidgets: number };
type Draft = { id: string; revision: number; state: "draft" | "submitted" | "approved"; owned: boolean; schemaVersion: number; definition: DashboardDefinition; createdAt: string; updatedAt: string };
type Page = { items: Draft[]; nextCursor?: string };
const capabilities = () => apiGet<Capabilities>("/api/v1/dashboard-capabilities");
const defaultDefinition = (): DashboardDefinition => ({ schemaVersion: 1, id: `custom-${Date.now().toString(36)}`, title: "Custom dashboard", variables: [{ id: "scope", label: "Scope", kind: "scope" }, { id: "range", label: "Time range", kind: "range" }], widgets: [{ id: "nodes-ready", title: "Nodes Ready", type: "Stat", binding: "nodes.ready", layout: { x: 0, y: 0, w: 3, h: 2 } }] });
const message = (error: unknown) => (error instanceof Error ? error.message : "Dashboard request failed");
const ifMatch = (rev: number) => ({ "If-Match": `"revision-${rev}"` });

// StateBadge는 초안 상태를 색으로 구분합니다. 라벨 텍스트는 계약(subtitle의 "state · revision N")과
// 맞추기 위해 상태 코드 그대로(draft/submitted/approved)를 노출합니다.
function StateBadge({ state }: { state: Draft["state"] }) {
  return <span className={`builder-badge builder-badge--${state}`}>{state}</span>;
}

export function DashboardBuilderList() {
  const nav = useNavigate();
  const [extra, setExtra] = useState<Draft[]>([]);
  const [next, setNext] = useState<string>();
  const [actionError, setActionError] = useState("");
  const [confirmId, setConfirmId] = useState("");
  const fileRef = useRef<HTMLInputElement>(null);
  const caps = useQuery({ queryKey: ["dashboard-capabilities"], queryFn: capabilities, staleTime: 30_000 });
  const list = useQuery({ queryKey: ["dashboard-drafts"], queryFn: () => apiGet<Page>("/api/v1/dashboard-drafts", { limit: "50" }), enabled: Boolean(caps.data?.enabled && (caps.data.canEdit || caps.data.canPublish)) });
  useEffect(() => setNext(list.data?.nextCursor), [list.data]);

  const run = async (fn: () => Promise<Draft>) => { setActionError(""); try { const d = await fn(); nav(`/dashboard-builder/${d.id}`); } catch (e) { setActionError(message(e)); } };
  const create = (definition: DashboardDefinition) => run(() => apiRequest<Draft>("/api/v1/dashboard-drafts", { method: "POST", body: JSON.stringify({ definition }) }));
  const clone = (draft: Draft) => run(() => apiRequest<Draft>(`/api/v1/dashboard-drafts/${draft.id}/clone`, { method: "POST" }));
  const more = async () => { if (!next) return; try { const page = await apiGet<Page>("/api/v1/dashboard-drafts", { limit: "50", cursor: next }); setExtra((v) => [...v, ...page.items]); setNext(page.nextCursor); } catch (e) { setActionError(message(e)); } };
  const remove = async (draft: Draft) => { setActionError(""); try { await apiRequest(`/api/v1/dashboard-drafts/${draft.id}`, { method: "DELETE", headers: ifMatch(draft.revision) }); setConfirmId(""); setExtra((v) => v.filter((d) => d.id !== draft.id)); await list.refetch(); } catch (e) { setActionError(message(e)); } };
  const importFile = async (file: File) => {
    setActionError("");
    try {
      const text = await file.text();
      const d = await apiRequest<Draft>("/api/v1/dashboard-drafts/import", { method: "POST", headers: { "content-type": "application/json" }, body: text });
      nav(`/dashboard-builder/${d.id}`);
    } catch (e) { setActionError(message(e)); }
  };

  if (caps.isLoading) return <div className="page"><LoadingState /></div>;
  if (caps.isError) return <div className="page"><header className="page__header"><h1 className="page__title">Dashboard Builder</h1></header><section className="panel"><div className="panel__body"><ErrorState detail={message(caps.error)} onRetry={() => void caps.refetch()} /></div></section></div>;
  if (!caps.data?.enabled) return (
    <div className="page">
      <header className="page__header"><h1 className="page__title">Dashboard Builder</h1></header>
      <section className="panel"><div className="panel__body">
        <ErrorState detail="Draft 저장소가 연결되지 않았습니다. Builder를 쓰려면 SQLite(DASHBOARD_DB_PATH) 또는 PostgreSQL(DATABASE_URL) 백엔드가 필요합니다." onRetry={() => void caps.refetch()} />
      </div></section>
    </div>
  );
  if (!caps.data.canEdit && !caps.data.canPublish) return <div className="page"><header className="page__header"><h1 className="page__title">Dashboard Builder</h1></header><section className="panel"><div className="panel__body"><ForbiddenState detail="You do not have dashboard permissions." /></div></section></div>;

  const items = [...(list.data?.items ?? []), ...extra];
  return (
    <div className="page">
      <header className="page__header">
        <div>
          <h1 className="page__title">Dashboard Builder</h1>
          <p className="page__subtitle">Custom 대시보드 초안입니다. Git 표준 대시보드와 별개로 관리됩니다.</p>
        </div>
        {caps.data.canEdit && (
          <div className="row row--wrap">
            <button type="button" className="btn btn--primary" onClick={() => void create(defaultDefinition())}>New draft</button>
            <button type="button" className="btn" onClick={() => fileRef.current?.click()}>Import JSON</button>
            <input ref={fileRef} type="file" accept="application/json,.json" hidden onChange={(e) => { const f = e.target.files?.[0]; if (f) void importFile(f); e.target.value = ""; }} />
          </div>
        )}
      </header>

      {caps.data.canEdit && embeddedDashboards.length > 0 && (
        <section className="panel">
          <div className="panel__header">표준 대시보드에서 복제해 시작</div>
          <div className="panel__body row row--wrap">
            {embeddedDashboards.map((d) => (
              <button key={d.id} type="button" className="btn" onClick={() => void create(structuredClone(d) as DashboardDefinition)}>Clone standard {d.title}</button>
            ))}
          </div>
        </section>
      )}

      {actionError && <section className="panel"><div className="panel__body"><ErrorState detail={actionError} /></div></section>}

      <section className="panel">
        <div className="panel__header">초안 목록</div>
        <div className="panel__body">
          {list.isLoading ? <LoadingState /> : list.isError ? <ErrorState detail={message(list.error)} onRetry={() => void list.refetch()} /> : items.length === 0 ? (
            <p className="muted">초안이 없습니다. “New draft”로 시작하거나 JSON을 Import하세요.</p>
          ) : (
            <ul className="builder-list">
              {items.map((d) => (
                <li key={d.id} className="builder-list__item">
                  <div className="builder-list__main">
                    <Link className="builder-list__title" to={`/dashboard-builder/${d.id}`}>{d.definition.title}</Link>
                    <span className="builder-list__meta"><StateBadge state={d.state} /> · revision {d.revision}{d.owned ? "" : " · 공유"}</span>
                  </div>
                  <div className="row">
                    {caps.data.canEdit && (d.owned || d.state === "approved") && <button type="button" className="btn btn--sm" onClick={() => void clone(d)}>Clone</button>}
                    {caps.data.canEdit && d.owned && d.state !== "approved" && (
                      confirmId === d.id ? (
                        <span className="row">
                          <span className="muted">삭제할까요?</span>
                          <button type="button" className="btn btn--sm btn--critical" onClick={() => void remove(d)}>Delete</button>
                          <button type="button" className="btn btn--sm" onClick={() => setConfirmId("")}>Cancel</button>
                        </span>
                      ) : (
                        <button type="button" className="btn btn--sm" onClick={() => setConfirmId(d.id)}>Delete</button>
                      )
                    )}
                  </div>
                </li>
              ))}
            </ul>
          )}
          {next && <div className="row" style={{ marginTop: "var(--space-4)" }}><button type="button" className="btn btn--sm" onClick={() => void more()}>Load more</button></div>}
        </div>
      </section>
    </div>
  );
}

// TimeSeries 팔레트 항목은 카탈로그 패널(panelId)별로 묶고, 사람이 읽을 이름은
// 카탈로그 panelTitle을 씁니다("Time series 1" 같은 익명 라벨 대신). (ADR 0015)
const tsGroups = embeddedDashboardBindings.reduce<Record<string, { title: string; refs: string[] }>>((groups, binding) => { const g = (groups[binding.panelId] ??= { title: (binding as { panelTitle?: string }).panelTitle ?? binding.panelId, refs: [] }); g.refs.push(binding.queryRef); return groups; }, {});
const palette: { type: WidgetType; title: string; binding: DashboardWidget["binding"]; queryRefs?: string[] }[] = [{ type: "Stat", title: "Nodes Ready", binding: "nodes.ready" }, { type: "Gauge", title: "Pods Running", binding: "pods.runningPercent" }, { type: "Table", title: "Unhealthy", binding: "unhealthy" }, { type: "EventTimeline", title: "Events", binding: "events" }, ...Object.values(tsGroups).map((g) => ({ type: "TimeSeries" as const, title: g.title, binding: "trends" as const, queryRefs: g.refs }))];

export function DashboardBuilderEditor() {
  const { id = "" } = useParams(); const nav = useNavigate(); const { search } = useLocation(); const queryClient = useQueryClient(); const gridRef = useRef<HTMLDivElement>(null); const savingRef = useRef(false); const [draft, setDraft] = useState<Draft>(); const [local, setLocal] = useState<DashboardDefinition>(); const [dirty, setDirty] = useState(false); const [saving, setSaving] = useState(false); const [conflict, setConflict] = useState(false); const [actionError, setActionError] = useState(""); const [confirmDelete, setConfirmDelete] = useState(false);
  const drag = useRef<{ id: string; x: number; y: number; layout: DashboardLayout; before: DashboardDefinition; beforeDirty: boolean; pending?: DashboardDefinition; raf: number } | null>(null);
  const caps = useQuery({ queryKey: ["dashboard-capabilities"], queryFn: capabilities }); const remote = useQuery({ queryKey: ["dashboard-draft", id], queryFn: () => apiGet<Draft>(`/api/v1/dashboard-drafts/${encodeURIComponent(id)}`), enabled: Boolean(id && caps.data?.enabled && (caps.data.canEdit || caps.data.canPublish)) });
  useEffect(() => { setDraft(undefined); setLocal(undefined); setDirty(false); setConflict(false); setActionError(""); setConfirmDelete(false); }, [id]);
  useEffect(() => { if (remote.data && !local) { setDraft(remote.data); setLocal(remote.data.definition); } }, [remote.data, local]);
  if (caps.isLoading || remote.isLoading) return <div className="page"><LoadingState /></div>;
  if (caps.isError) return <div className="page"><section className="panel"><div className="panel__body"><ErrorState detail={message(caps.error)} onRetry={() => void caps.refetch()} /></div></section></div>;
  if (!caps.data?.enabled || (!caps.data.canEdit && !caps.data.canPublish)) return <div className="page"><section className="panel"><div className="panel__body"><ForbiddenState detail="Builder를 사용할 수 없거나 권한이 없습니다." /></div></section></div>;
  if (remote.isError) return <div className="page"><section className="panel"><div className="panel__body"><ErrorState detail={message(remote.error)} onRetry={() => void remote.refetch()} /></div></section></div>;
  if (!local || !draft) return <div className="page"><LoadingState /></div>;
  const editable = caps.data.canEdit && draft.owned && draft.state === "draft" && !saving;
  const acceptSaved = (saved: Draft) => { setDraft(saved); setLocal(saved.definition); setDirty(false); setConflict(false); queryClient.setQueryData(["dashboard-draft", id], saved); void queryClient.invalidateQueries({ queryKey: ["dashboard-drafts"] }); };
  const persist = async (next = local) => { const release = acquireSingleFlight(savingRef); if (!release) return; setSaving(true); setActionError(""); try { acceptSaved(await apiRequest<Draft>(`/api/v1/dashboard-drafts/${id}`, { method: "PUT", headers: ifMatch(draft.revision), body: JSON.stringify({ definition: next }) })); } catch (e) { if (e instanceof HttpError && e.status === 409) setConflict(true); else setActionError(message(e)); } finally { release(); setSaving(false); } };
  const mutate = async (fn: () => Promise<Draft>) => { const release = acquireSingleFlight(savingRef); if (!release) return; setSaving(true); setActionError(""); try { acceptSaved(await fn()); } catch (e) { if (e instanceof HttpError && e.status === 409) setConflict(true); else setActionError(message(e)); } finally { release(); setSaving(false); } };
  const setChanged = (next: DashboardDefinition) => { setLocal(next); setDirty(true); };
  const add = (item: (typeof palette)[number]) => { setActionError(""); if (local.widgets.length >= 24) { setActionError(`위젯은 최대 24개입니다.`); return; } const layout = placeWidget(local.widgets); if (!layout) { setActionError("빈 자리가 없습니다. 위젯을 옮기거나 지운 뒤 추가하세요."); return; } let suffix = 1; const used = new Set(local.widgets.map((w) => w.id)); while (used.has(`widget-${suffix}`)) suffix++; const widget = { id: `widget-${suffix}`, title: item.title, type: item.type, binding: item.binding, layout, ...(item.queryRefs ? { queryRefs: item.queryRefs } : {}), ...(item.type === "Table" || item.type === "EventTimeline" ? { options: { maxRows: 200 } } : {}) } as DashboardWidget; const next = { ...local, widgets: [...local.widgets, widget] }; const result = validateDashboard(next, new Set(embeddedDashboardBindings.map((b) => b.queryRef))); if (result.valid) setChanged(next); else setActionError(`검증 실패: ${result.errors.join(", ") || "유효하지 않은 구성"}`); };
  const apply = (widgetId: string, nextLayout: DashboardLayout) => { const next = updateWidgetLayout(local, widgetId, nextLayout); if (!next) return false; setChanged(next); return true; };
  const removeWidget = (widgetId: string) => { if (local.widgets.length === 1) return; setChanged({ ...local, widgets: local.widgets.filter((w) => w.id !== widgetId) }); };
  const pointerLayout = (d: NonNullable<typeof drag.current>, clientX: number, clientY: number) => { const grid = gridRef.current; if (!grid) return null; const styles = getComputedStyle(grid); const columnGap = parseFloat(styles.columnGap) || 0; const rowGap = parseFloat(styles.rowGap) || 0; const col = (grid.clientWidth - columnGap * 11) / 12; const row = parseFloat(styles.gridAutoRows); if (!Number.isFinite(col) || col <= 0 || !Number.isFinite(row) || row <= 0) return null; return updateWidgetLayout(d.before, d.id, { ...d.layout, x: d.layout.x + Math.round((clientX - d.x) / (col + columnGap)), y: d.layout.y + Math.round((clientY - d.y) / (row + rowGap)) }); };
  const pointerDown = (e: React.PointerEvent, id: string, layout: DashboardLayout) => { e.currentTarget.setPointerCapture(e.pointerId); drag.current = { id, x: e.clientX, y: e.clientY, layout, before: local, beforeDirty: dirty, raf: 0 }; };
  const pointerMove = (e: React.PointerEvent) => { const d = drag.current; if (!d) return; const x = e.clientX, y = e.clientY; cancelAnimationFrame(d.raf); d.raf = requestAnimationFrame(() => { const next = pointerLayout(d, x, y); if (next) { d.pending = next; setChanged(next); } }); };
  const pointerEnd = (e: React.PointerEvent) => { const d = drag.current; if (!d) return; cancelAnimationFrame(d.raf); const next = pointerLayout(d, e.clientX, e.clientY) ?? d.pending; drag.current = null; if (e.currentTarget.hasPointerCapture(e.pointerId)) e.currentTarget.releasePointerCapture(e.pointerId); const before = d.before.widgets.find((w) => w.id === d.id)?.layout; const after = next?.widgets.find((w) => w.id === d.id)?.layout; if (next && before && after && JSON.stringify(before) !== JSON.stringify(after)) { setChanged(next); void persist(next); } };
  const pointerCancel = (e: React.PointerEvent) => { const d = drag.current; if (!d) return; cancelAnimationFrame(d.raf); drag.current = null; if (e.currentTarget.hasPointerCapture(e.pointerId)) e.currentTarget.releasePointerCapture(e.pointerId); setLocal(d.before); setDirty(d.beforeDirty); };
  const reload = async () => { setActionError(""); try { const result = await remote.refetch(); if (result.isError || !result.data) throw result.error ?? new Error("Latest dashboard could not be loaded"); setDraft(result.data); setLocal(result.data.definition); setDirty(false); setConflict(false); } catch (e) { setActionError(message(e)); } };
  const fork = async () => { try { const created = await apiRequest<Draft>("/api/v1/dashboard-drafts", { method: "POST", body: JSON.stringify({ definition: local }) }); nav(`/dashboard-builder/${created.id}`); } catch (e) { setActionError(message(e)); } };
  const download = async () => { try { const file = await apiDownload(`/api/v1/dashboard-drafts/${id}/export`); const url = URL.createObjectURL(file.blob); const anchor = document.createElement("a"); anchor.href = url; anchor.download = file.filename; anchor.click(); URL.revokeObjectURL(url); } catch (e) { setActionError(message(e)); } };
  const removeDraft = async () => { setActionError(""); try { await apiRequest(`/api/v1/dashboard-drafts/${id}`, { method: "DELETE", headers: ifMatch(draft.revision) }); void queryClient.invalidateQueries({ queryKey: ["dashboard-drafts"] }); nav(withSearch("/dashboard-builder", search)); } catch (e) { setActionError(message(e)); } };

  return (
    <div className="page">
      <header className="page__header">
        <div>
          <h1 className="page__title">{local.title}</h1>
          <p className="page__subtitle"><StateBadge state={draft.state} /> · revision {draft.revision}{dirty ? " · unsaved" : ""}</p>
        </div>
        <div className="row row--wrap">
          {editable && draft.owned && (confirmDelete ? (
            <span className="row"><span className="muted">Delete this draft?</span><button type="button" className="btn btn--sm btn--critical" onClick={() => void removeDraft()}>Delete</button><button type="button" className="btn btn--sm" onClick={() => setConfirmDelete(false)}>Cancel</button></span>
          ) : (
            <button type="button" className="btn btn--sm" onClick={() => setConfirmDelete(true)}>Delete draft</button>
          ))}
          <Link className="btn btn--sm" to={withSearch("/dashboard-builder", search)}>Back to drafts</Link>
        </div>
      </header>

      {actionError && <section className="panel"><div className="panel__body"><ErrorState detail={actionError} /></div></section>}
      {conflict && (
        <section className="panel panel--degraded"><div className="panel__body" role="alert">
          <p>The server has a newer revision. Your local edits are preserved.</p>
          <div className="row row--wrap"><button type="button" className="btn btn--sm" onClick={() => void reload()}>Reload latest</button><button type="button" className="btn btn--sm" onClick={() => void fork()}>Fork local edits</button></div>
        </div></section>
      )}

      {editable && (
        <section className="panel">
          <div className="panel__header">Approved palette</div>
          <div className="panel__body">
            <div className="row row--wrap">
              {palette.map((p, i) => <button key={`${p.type}-${i}`} type="button" className="btn btn--sm" onClick={() => add(p)}>Add {p.title}</button>)}
            </div>
            <div className="row" style={{ marginTop: "var(--space-4)" }}>
              <button type="button" className="btn btn--primary btn--sm" disabled={!dirty || saving} onClick={() => void persist()}>{saving ? "Saving…" : "Save changes"}</button>
            </div>
          </div>
        </section>
      )}

      <section className="panel">
        <div className="panel__header">Layout</div>
        <div className="panel__body">
          <div ref={gridRef} className="builder-grid" aria-label="Dashboard layout. Use widget buttons for keyboard movement and resize.">
            {local.widgets.map((w) => (
              <article key={w.id} className="panel builder-widget" style={{ gridColumn: `${w.layout.x + 1} / span ${w.layout.w}`, gridRow: `${w.layout.y + 1} / span ${w.layout.h}` }}>
                <div className="panel__header" tabIndex={editable ? 0 : undefined} onPointerDown={editable ? (e) => pointerDown(e, w.id, w.layout) : undefined} onPointerMove={pointerMove} onPointerUp={pointerEnd} onPointerCancel={pointerCancel} onLostPointerCapture={pointerCancel}>{w.title}</div>
                {editable && (
                  <div className="panel__body builder-widget__toolbar" role="toolbar" aria-label={`Edit ${w.title}`}>
                    <button type="button" className="btn btn--icon" aria-label={`Move ${w.title} left`} onClick={() => apply(w.id, { ...w.layout, x: w.layout.x - 1 })}>←</button>
                    <button type="button" className="btn btn--icon" aria-label={`Move ${w.title} right`} onClick={() => apply(w.id, { ...w.layout, x: w.layout.x + 1 })}>→</button>
                    <button type="button" className="btn btn--icon" aria-label={`Move ${w.title} up`} onClick={() => apply(w.id, { ...w.layout, y: w.layout.y - 1 })}>↑</button>
                    <button type="button" className="btn btn--icon" aria-label={`Move ${w.title} down`} onClick={() => apply(w.id, { ...w.layout, y: w.layout.y + 1 })}>↓</button>
                    <button type="button" className="btn btn--icon" aria-label={`Widen ${w.title}`} onClick={() => apply(w.id, { ...w.layout, w: w.layout.w + 1 })}>W+</button>
                    <button type="button" className="btn btn--icon" aria-label={`Narrow ${w.title}`} onClick={() => apply(w.id, { ...w.layout, w: w.layout.w - 1 })}>W−</button>
                    <button type="button" className="btn btn--icon" aria-label={`Make ${w.title} taller`} onClick={() => apply(w.id, { ...w.layout, h: w.layout.h + 1 })}>H+</button>
                    <button type="button" className="btn btn--icon" aria-label={`Make ${w.title} shorter`} onClick={() => apply(w.id, { ...w.layout, h: w.layout.h - 1 })}>H−</button>
                    <button type="button" className="btn btn--icon btn--critical" disabled={local.widgets.length === 1} title={local.widgets.length === 1 ? "A dashboard requires at least one widget" : ""} onClick={() => removeWidget(w.id)}>Delete</button>
                  </div>
                )}
              </article>
            ))}
          </div>
        </div>
      </section>

      <section className="panel">
        <div className="panel__header">Preview</div>
        <div className="panel__body"><ResolvedDashboard definition={local} /></div>
      </section>

      <div className="row row--wrap">
        {editable && <button type="button" className="btn" disabled={dirty || saving} title={dirty ? "Save your changes first." : ""} onClick={() => void mutate(() => apiRequest<Draft>(`/api/v1/dashboard-drafts/${id}/submit`, { method: "POST", headers: ifMatch(draft.revision) }))}>Submit for approval</button>}
        {caps.data.canPublish && draft.state === "submitted" && <button type="button" className="btn btn--primary" disabled={saving} onClick={() => void mutate(() => apiRequest<Draft>(`/api/v1/dashboard-drafts/${id}/approve`, { method: "POST", headers: ifMatch(draft.revision) }))}>Approve</button>}
        {draft.state === "approved" && <button type="button" className="btn" onClick={() => void download()}>Export canonical JSON</button>}
      </div>
    </div>
  );
}
