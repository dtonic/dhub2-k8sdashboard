import { http, HttpResponse } from "msw";
import type { DashboardDefinition } from "@k8s-dashboard/dashboard-schema";

type Role = "editor" | "publisher" | "viewer";
type State = "draft" | "submitted" | "approved";
type StoredDraft = {
  id: string;
  owner: string;
  revision: number;
  state: State;
  definition: DashboardDefinition;
  createdAt: string;
  updatedAt: string;
};

const draftID = "11111111-1111-4111-8111-111111111111";
const now = "2026-08-15T00:00:00Z";
const storageKey = "dashboard-builder-e2e-drafts";
function loadDrafts(): StoredDraft[] {
  if (typeof window === "undefined") return [];
  try { return JSON.parse(window.localStorage.getItem(storageKey) ?? "[]") as StoredDraft[]; } catch { return []; }
}
function persistDrafts() {
  if (typeof window !== "undefined") window.localStorage.setItem(storageKey, JSON.stringify(drafts));
}
let drafts: StoredDraft[] = loadDrafts();
let conflictNextPut = false;
let currentRole: Role = "editor";

function roleOf(request: Request): Role {
  const direct = new URL(request.url).searchParams.get("scenario");
  const referer = request.headers.get("referer");
  const scenario = direct ?? (referer ? new URL(referer).searchParams.get("scenario") : null);
  if (scenario === "builder-publisher") currentRole = "publisher";
  else if (scenario === "builder-viewer") currentRole = "viewer";
  else if (scenario === "builder-editor") currentRole = "editor";
  return currentRole;
}

const responseDraft = (draft: StoredDraft, role: Role) => ({
  id: draft.id,
  revision: draft.revision,
  state: draft.state,
  owned: role === "editor" && draft.owner === "editor",
  schemaVersion: draft.definition.schemaVersion,
  definition: draft.definition,
  createdAt: draft.createdAt,
  updatedAt: draft.updatedAt,
});
const error = (status: number, code: string, message: string) =>
  HttpResponse.json({ code, message, requestId: "builder-e2e" }, { status });
const revision = (request: Request) => request.headers.get("if-match");
const find = (id: string) => drafts.find((draft) => draft.id === id);

export const dashboardBuilderHandlers = [
  http.post("/api/v1/dashboard-test/reset", () => {
    drafts = [];
    conflictNextPut = false;
    persistDrafts();
    return new HttpResponse(null, { status: 204 });
  }),
  http.post("/api/v1/dashboard-test/conflict-next", () => {
    conflictNextPut = true;
    return new HttpResponse(null, { status: 204 });
  }),
  http.get("/api/v1/dashboard-capabilities", ({ request }) => {
    const role = roleOf(request);
    return HttpResponse.json({ enabled: true, canEdit: role === "editor", canPublish: role === "publisher", maxDrafts: 32, maxWidgets: 24 });
  }),
  http.get("/api/v1/dashboard-drafts", ({ request }) => {
    const role = roleOf(request);
    if (role === "viewer") return error(403, "forbidden", "Dashboard permission is required.");
    const visible = drafts.filter((draft) => role === "editor" ? draft.owner === "editor" : draft.state !== "draft");
    return HttpResponse.json({ items: visible.map((draft) => responseDraft(draft, role)) });
  }),
  http.post("/api/v1/dashboard-drafts", async ({ request }) => {
    if (roleOf(request) !== "editor") return error(403, "forbidden", "Dashboard editor permission is required.");
    if (drafts.length >= 32) return error(409, "draft_limit", "Draft limit reached.");
    const body = await request.json() as { definition: DashboardDefinition };
    const id = drafts.length === 0 ? draftID : `22222222-2222-4222-8222-${String(drafts.length).padStart(12, "0")}`;
    const draft: StoredDraft = { id, owner: "editor", revision: 1, state: "draft", definition: body.definition, createdAt: now, updatedAt: now };
    drafts.push(draft);
    persistDrafts();
    return HttpResponse.json(responseDraft(draft, "editor"), { status: 201, headers: { ETag: '"revision-1"' } });
  }),
  http.post("/api/v1/dashboard-drafts/:id/clone", ({ request, params }) => {
    if (roleOf(request) !== "editor") return error(403, "forbidden", "Dashboard editor permission is required.");
    const source = find(String(params.id));
    if (!source || (source.owner !== "editor" && source.state !== "approved")) return error(404, "not_found", "Dashboard was not found.");
    if (drafts.length >= 32) return error(409, "draft_limit", "Draft limit reached.");
    const id = `22222222-2222-4222-8222-${String(drafts.length).padStart(12, "0")}`;
    const draft: StoredDraft = { id, owner: "editor", revision: 1, state: "draft", definition: structuredClone(source.definition), createdAt: now, updatedAt: now };
    drafts.push(draft);
    persistDrafts();
    return HttpResponse.json(responseDraft(draft, "editor"), { status: 200, headers: { ETag: '"revision-1"' } });
  }),
  http.get("/api/v1/dashboard-drafts/:id/export", ({ request, params }) => {
    const draft = find(String(params.id));
    if (roleOf(request) === "viewer") return error(403, "forbidden", "Dashboard permission is required.");
    if (!draft || draft.state !== "approved") return error(404, "not_found", "Dashboard was not found.");
    const body = `${JSON.stringify(draft.definition, null, 2)}\n`;
    return new HttpResponse(body, { status: 200, headers: { "Content-Type": "application/json; charset=utf-8", "Content-Disposition": `attachment; filename="${draft.definition.id}.json"` } });
  }),
  http.get("/api/v1/dashboard-drafts/:id", ({ request, params }) => {
    const role = roleOf(request);
    const draft = find(String(params.id));
    if (role === "viewer") return error(403, "forbidden", "Dashboard permission is required.");
    if (!draft || (role === "editor" && draft.owner !== "editor") || (role === "publisher" && draft.state === "draft")) return error(404, "not_found", "Dashboard was not found.");
    return HttpResponse.json(responseDraft(draft, role), { headers: { ETag: `"revision-${draft.revision}"` } });
  }),
  http.put("/api/v1/dashboard-drafts/:id", async ({ request, params }) => {
    const draft = find(String(params.id));
    if (roleOf(request) !== "editor") return error(403, "forbidden", "Dashboard editor permission is required.");
    if (!draft) return error(404, "not_found", "Dashboard was not found.");
    if (conflictNextPut) {
      conflictNextPut = false;
      return error(409, "revision_conflict", "Dashboard changed; local edits were not overwritten.");
    }
    if (revision(request) !== `"revision-${draft.revision}"`) return error(409, "revision_conflict", "Dashboard changed; local edits were not overwritten.");
    const body = await request.json() as { definition: DashboardDefinition };
    draft.definition = body.definition;
    draft.revision++;
    draft.updatedAt = now;
    persistDrafts();
    return HttpResponse.json(responseDraft(draft, "editor"), { headers: { ETag: `"revision-${draft.revision}"` } });
  }),
  http.post("/api/v1/dashboard-drafts/:id/submit", ({ request, params }) => {
    const draft = find(String(params.id));
    if (roleOf(request) !== "editor") return error(403, "forbidden", "Dashboard editor permission is required.");
    if (!draft || revision(request) !== `"revision-${draft.revision}"` || draft.state !== "draft") return error(409, "revision_conflict", "Dashboard state changed.");
    draft.state = "submitted";
    draft.revision++;
    persistDrafts();
    return HttpResponse.json(responseDraft(draft, "editor"), { headers: { ETag: `"revision-${draft.revision}"` } });
  }),
  http.post("/api/v1/dashboard-drafts/:id/approve", ({ request, params }) => {
    const draft = find(String(params.id));
    if (roleOf(request) !== "publisher") return error(403, "forbidden", "Dashboard publisher permission is required.");
    if (!draft || revision(request) !== `"revision-${draft.revision}"` || draft.state !== "submitted") return error(409, "revision_conflict", "Dashboard state changed.");
    draft.state = "approved";
    draft.revision++;
    persistDrafts();
    return HttpResponse.json(responseDraft(draft, "publisher"), { headers: { ETag: `"revision-${draft.revision}"` } });
  }),
];
