import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import Ajv2020 from "ajv/dist/2020.js";
import { DASHBOARD_LIMITS, dashboardControlKinds, migrateDashboard, validateDashboard, validateEmbeddedFiles } from "../src/index.js";

const schema = JSON.parse(await readFile(new URL("../schema/dashboard.schema.json", import.meta.url), "utf8"));
const sharedParity = JSON.parse(await readFile(new URL("./fixtures/dashboard-parity.json", import.meta.url), "utf8"));
const catalog = new Set(["metrics.cpu.used", "metrics.cpu.requested"]);
const execFileAsync = promisify(execFile);
const schemaSemantics = (_enabled, value) => {
  const variableIds = new Set();
  const variableKinds = new Set();
  for (const variable of value.variables ?? []) {
    if (variableIds.has(variable.id)) return false;
    variableIds.add(variable.id);
    if (variableKinds.has(variable.kind)) return false;
    variableKinds.add(variable.kind);
  }
  const widgetIds = new Set();
  const occupied = new Set();
  for (const widget of value.widgets ?? []) {
    if (widgetIds.has(widget.id)) return false;
    widgetIds.add(widget.id);
    if (widget.type === "TimeSeries" && widget.queryRefs.some((ref) => !catalog.has(ref))) return false;
    const { x, y, w, h } = widget.layout ?? {};
    if (x + w > DASHBOARD_LIMITS.columns || y + h > DASHBOARD_LIMITS.rows) return false;
    for (let row = y; row < y + h; row++) for (let column = x; column < x + w; column++) {
      const cell = row * DASHBOARD_LIMITS.columns + column;
      if (occupied.has(cell)) return false;
      occupied.add(cell);
    }
  }
  return true;
};
const ajv = new Ajv2020({ strict: true });
ajv.addKeyword({ keyword: "x-dashboard-semantics", schemaType: "boolean", type: "object", validate: schemaSemantics });
const schemaValidate = ajv.compile(schema);

test("shared Go/runtime parity corpus has identical outcomes", () => {
  const refs = new Set(sharedParity.queryRefs);
  for (const entry of sharedParity.cases) {
    const value = JSON.parse(entry.raw);
    const schemaValid = schemaValidate(value) && schemaSemantics(true, value);
    assert.equal(schemaValid, entry.valid, `${entry.name}: schema`);
    assert.equal(validateDashboard(value, refs).valid, entry.valid, `${entry.name}: runtime`);
  }
});
const widgets = [
  { id: "time", title: "Time", type: "TimeSeries", binding: "trends", queryRefs: ["metrics.cpu.used"], layout: { x: 0, y: 0, w: 2, h: 1 } },
  { id: "stat", title: "Stat", type: "Stat", binding: "nodes.ready", layout: { x: 2, y: 0, w: 2, h: 1 } },
  { id: "gauge", title: "Gauge", type: "Gauge", binding: "pods.runningPercent", layout: { x: 4, y: 0, w: 2, h: 1 } },
  { id: "table", title: "Table", type: "Table", binding: "unhealthy", options: { maxRows: 20 }, layout: { x: 6, y: 0, w: 2, h: 1 } },
  { id: "logs", title: "Logs", type: "LogStream", binding: "unsupported.logs", layout: { x: 8, y: 0, w: 2, h: 1 } },
  { id: "events", title: "Events", type: "EventTimeline", binding: "events", options: { maxRows: 20 }, layout: { x: 10, y: 0, w: 2, h: 1 } },
];
const valid = { schemaVersion: 1, id: "operations", title: "Operations", variables: [], widgets };

test("Draft 2020-12 schema and runtime agree on the structural parity corpus", () => {
  const cases = [{ value: valid, accepted: true }];
  for (const key of ["schemaVersion", "id", "title", "variables", "widgets"]) {
    const value = structuredClone(valid); delete value[key]; cases.push({ value, accepted: false });
  }
  for (const original of widgets) for (const key of ["id", "title", "type", "binding", "layout", ...(original.type === "TimeSeries" ? ["queryRefs"] : [])]) {
    const copy = structuredClone(widgets); delete copy[widgets.indexOf(original)][key];
    cases.push({ value: { ...valid, widgets: copy }, accepted: false });
  }
  for (const value of [
    { ...valid, schemaVersion: 2 },
    { ...valid, surprise: true },
    { ...valid, widgets: [{ ...widgets[0], type: "RemoteComponent" }] },
    { ...valid, widgets: [{ ...widgets[0], binding: "raw.query" }] },
    { ...valid, widgets: [{ ...widgets[0], queryRefs: ["metrics.cpu.used", "metrics.cpu.used"] }] },
    { ...valid, title: "🙂".repeat(DASHBOARD_LIMITS.string + 1) },
    { ...valid, widgets: [widgets[0], { ...widgets[0] }] },
    { ...valid, widgets: [widgets[0], { ...widgets[1], layout: widgets[0].layout }] },
    { ...valid, widgets: [{ ...widgets[0], layout: { x: 11, y: 0, w: 2, h: 1 } }] },
    { ...valid, widgets: [{ ...widgets[0], queryRefs: ["metrics.missing"] }] },
    { ...valid, variables: [{ id: "scope-a", label: "A", kind: "scope" }, { id: "scope-b", label: "B", kind: "scope" }] },
  ]) cases.push({ value, accepted: false });
  cases.push({ value: { ...valid, title: "🙂".repeat(DASHBOARD_LIMITS.string) }, accepted: true });
  for (const { value, accepted } of cases) {
    assert.equal(schemaValidate(value), accepted, JSON.stringify(schemaValidate.errors));
    assert.equal(validateDashboard(value, catalog).valid, accepted, validateDashboard(value, catalog).errors.join("; "));
  }
});

test("negative security mutations independently reject remote widget types and raw query bindings", () => {
  const remoteType = { ...valid, widgets: [{ ...widgets[0], type: "RemoteComponent" }] };
  assert.equal(schemaValidate(remoteType), false, "unknown sensitive widget type was masked");
  assert.equal(validateDashboard(remoteType, catalog).valid, false, "runtime accepted unknown sensitive widget type");

  const rawQuery = { ...valid, widgets: [{ ...widgets[0], binding: "raw.query" }] };
  assert.equal(schemaValidate(rawQuery), false, "raw query binding was masked");
  assert.equal(validateDashboard(rawQuery, catalog).valid, false, "runtime accepted raw query binding");
});

test("runtime rejects semantic ambiguity and enforces exact caps", () => {
  const invalid = [
    { ...valid, widgets: [widgets[0], { ...widgets[0] }] },
    { ...valid, widgets: [widgets[0], { ...widgets[1], layout: widgets[0].layout }] },
    { ...valid, widgets: [{ ...widgets[0], layout: { x: 12, y: 0, w: 1, h: 1 } }] },
    { ...valid, widgets: [{ ...widgets[0], queryRefs: ["metrics.missing"] }] },
    { ...valid, widgets: [{ ...widgets[3], options: { maxRows: 5001 } }] },
    { ...valid, variables: Array.from({ length: DASHBOARD_LIMITS.variables + 1 }, (_, i) => ({ id: `v-${i}`, label: "v", kind: "scope" })) },
    { ...valid, widgets: Array.from({ length: DASHBOARD_LIMITS.widgets + 1 }, (_, i) => ({ ...widgets[1], id: `w-${i}`, layout: { x: i % 12, y: Math.floor(i / 12), w: 1, h: 1 } })) },
  ];
  for (const value of invalid) assert.equal(validateDashboard(value, catalog).valid, false);
  assert.equal(validateDashboard({ ...valid, variables: [{ id: "scope", label: "Scope", kind: "scope" }, { id: "range", label: "Range", kind: "range" }] }, catalog).valid, true);
  assert.equal(validateDashboard({ ...valid, widgets: Array.from({ length: DASHBOARD_LIMITS.widgets }, (_, i) => ({ ...widgets[1], id: `w-${i}`, layout: { x: i % 12, y: Math.floor(i / 12), w: 1, h: 1 } })) }, catalog).valid, true);
  assert.equal(validateDashboard({ ...valid, widgets: [{ ...widgets[3], options: { maxRows: 5000 } }] }, catalog).valid, true);
});

test("migration is explicit identity for v1 and rejects future versions", () => {
  assert.deepEqual(migrateDashboard(valid), valid);
  assert.throws(() => migrateDashboard({ ...valid, schemaVersion: 2 }), /unsupported/);
});

test("variable declarations determine control order and omission", () => {
  assert.deepEqual(dashboardControlKinds({ ...valid, variables: [{ id: "range", label: "Range", kind: "range" }, { id: "scope", label: "Scope", kind: "scope" }] }), ["range", "scope"]);
  assert.deepEqual(dashboardControlKinds({ ...valid, variables: [{ id: "range", label: "Range", kind: "range" }] }), ["range"]);
  assert.deepEqual(dashboardControlKinds({ ...valid, variables: [] }), []);
});

const entry = (name, value = valid) => { const text = JSON.stringify(value); return { name, kind: "file", size: Buffer.byteLength(text), text }; };
test("embedded loader rejects unsafe entries before parse and detects duplicate dashboard ids", () => {
  assert.equal(validateEmbeddedFiles(Array.from({ length: DASHBOARD_LIMITS.files }, (_, i) => entry(`${i}.json`, { ...valid, id: `d-${i}` })), catalog).length, DASHBOARD_LIMITS.files);
  assert.throws(() => validateEmbeddedFiles(Array.from({ length: DASHBOARD_LIMITS.files + 1 }, (_, i) => entry(`${i}.json`, { ...valid, id: `d-${i}` })), catalog), /file count/);
  for (const unsafe of [
    { ...entry("../x.json"), name: "../x.json" }, { ...entry("x/y.json"), name: "x/y.json" },
    { ...entry("x.yaml"), name: "x.yaml" }, { ...entry("x.json"), kind: "symlink" },
    { ...entry("x.json"), size: DASHBOARD_LIMITS.fileBytes + 1 },
  ]) assert.throws(() => validateEmbeddedFiles([unsafe], catalog));
  assert.throws(() => validateEmbeddedFiles([entry("a.json"), entry("b.json")], catalog), /duplicate dashboard/);
});

test("catalog identity is consumed directly: removed refs fail and newly cataloged refs pass", () => {
  assert.throws(() => validateEmbeddedFiles([entry("a.json")], new Set()), /unknown queryRef/);
  const added = structuredClone(valid); added.widgets[0].queryRefs = ["metrics.new"];
  assert.equal(validateEmbeddedFiles([entry("a.json", added)], new Set([...catalog, "metrics.new"])).length, 1);
});

test("embedded generation is byte-identical across consecutive runs", async () => {
  const script = new URL("../scripts/generate-embedded.mjs", import.meta.url);
  const output = new URL("../../../apps/web/src/generated/dashboards.ts", import.meta.url);
  await execFileAsync(process.execPath, [fileURLToPath(script)]);
  const first = createHash("sha256").update(await readFile(output)).digest("hex");
  await execFileAsync(process.execPath, [fileURLToPath(script)]);
  const second = createHash("sha256").update(await readFile(output)).digest("hex");
  assert.equal(first, second);
});
