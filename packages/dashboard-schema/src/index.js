export const DASHBOARD_LIMITS = Object.freeze({
  files: 32, fileBytes: 65_536, widgets: 24, variables: 2, string: 160, columns: 12, rows: 96,
});

const ID = /^[a-z][a-z0-9-]{0,63}$/;
const TYPES = new Set(["TimeSeries", "Stat", "Gauge", "Table", "LogStream", "EventTimeline"]);
const BINDINGS = {
  TimeSeries: new Set(["trends"]), Stat: new Set(["nodes.ready", "pods.running"]),
  Gauge: new Set(["pods.runningPercent"]), Table: new Set(["unhealthy"]),
  LogStream: new Set(["unsupported.logs"]), EventTimeline: new Set(["events"]),
};

const ownKeys = (value) => Object.keys(value);
const record = (value) => value !== null && typeof value === "object" && !Array.isArray(value);
const exact = (value, allowed, errors, at) => {
  for (const key of ownKeys(value)) if (!allowed.has(key)) errors.push(`${at}.${key}: unknown property`);
};
const text = (value) => typeof value === "string" && [...value].length > 0 && [...value].length <= DASHBOARD_LIMITS.string;
const integer = (value) => Number.isInteger(value);

/** Linear validation plus a fixed 12x96 occupancy grid. */
export function validateDashboard(value, queryRefs) {
  const errors = [];
  if (!record(value)) return { valid: false, errors: ["dashboard: object required"] };
  exact(value, new Set(["schemaVersion", "id", "title", "description", "variables", "widgets"]), errors, "dashboard");
  if (value.schemaVersion !== 1) errors.push("schemaVersion: unsupported version");
  if (!text(value.id) || !ID.test(value.id)) errors.push("id: invalid");
  if (!text(value.title)) errors.push("title: invalid");
  if (value.description !== undefined && !text(value.description)) errors.push("description: invalid");
  if (!Array.isArray(value.variables) || value.variables.length > DASHBOARD_LIMITS.variables) errors.push("variables: invalid or over limit");
  if (!Array.isArray(value.widgets) || value.widgets.length === 0 || value.widgets.length > DASHBOARD_LIMITS.widgets) errors.push("widgets: invalid or over limit");

  const variableIds = new Set();
  const variableKinds = new Set();
  for (const [i, variable] of (Array.isArray(value.variables) ? value.variables : []).entries()) {
    const at = `variables[${i}]`;
    if (!record(variable)) { errors.push(`${at}: object required`); continue; }
    exact(variable, new Set(["id", "label", "kind"]), errors, at);
    if (!text(variable.id) || !ID.test(variable.id)) errors.push(`${at}.id: invalid`);
    else if (variableIds.has(variable.id)) errors.push(`${at}.id: duplicate`); else variableIds.add(variable.id);
    if (!text(variable.label)) errors.push(`${at}.label: invalid`);
    if (variable.kind !== "scope" && variable.kind !== "range") errors.push(`${at}.kind: invalid`);
    else if (variableKinds.has(variable.kind)) errors.push(`${at}.kind: duplicate`); else variableKinds.add(variable.kind);
  }

  const widgetIds = new Set();
  const occupied = new Uint8Array(DASHBOARD_LIMITS.columns * DASHBOARD_LIMITS.rows);
  for (const [i, widget] of (Array.isArray(value.widgets) ? value.widgets : []).entries()) {
    const at = `widgets[${i}]`;
    if (!record(widget)) { errors.push(`${at}: object required`); continue; }
    const optionType = widget.type === "Table" || widget.type === "EventTimeline";
    const allowed = new Set(["id", "title", "type", "binding", "layout"]);
    if (widget.type === "TimeSeries") allowed.add("queryRefs");
    if (optionType) allowed.add("options");
    exact(widget, allowed, errors, at);
    if (!text(widget.id) || !ID.test(widget.id)) errors.push(`${at}.id: invalid`);
    else if (widgetIds.has(widget.id)) errors.push(`${at}.id: duplicate`); else widgetIds.add(widget.id);
    if (!text(widget.title)) errors.push(`${at}.title: invalid`);
    if (!TYPES.has(widget.type)) errors.push(`${at}.type: unknown`);
    else if (!BINDINGS[widget.type].has(widget.binding)) errors.push(`${at}.binding: invalid`);
    if (widget.type === "TimeSeries") {
      if (!Array.isArray(widget.queryRefs) || widget.queryRefs.length === 0 || widget.queryRefs.length > 8) errors.push(`${at}.queryRefs: invalid`);
      else {
        const seenRefs = new Set();
        for (const ref of widget.queryRefs) {
          if (!text(ref) || (queryRefs && !queryRefs.has(ref))) errors.push(`${at}.queryRefs: unknown queryRef ${String(ref)}`);
          if (seenRefs.has(ref)) errors.push(`${at}.queryRefs: duplicate queryRef ${String(ref)}`);
          seenRefs.add(ref);
        }
      }
    }
    if (widget.options !== undefined) {
      if (!record(widget.options)) errors.push(`${at}.options: object required`);
      else {
        exact(widget.options, new Set(["maxRows"]), errors, `${at}.options`);
        if (!integer(widget.options.maxRows) || widget.options.maxRows < 1 || widget.options.maxRows > 5000) errors.push(`${at}.options.maxRows: invalid`);
      }
    }
    const layout = widget.layout;
    if (!record(layout)) { errors.push(`${at}.layout: object required`); continue; }
    exact(layout, new Set(["x", "y", "w", "h"]), errors, `${at}.layout`);
    const { x, y, w, h } = layout;
    if (![x, y, w, h].every(integer) || x < 0 || y < 0 || w < 1 || h < 1 || x + w > 12 || y + h > DASHBOARD_LIMITS.rows) {
      errors.push(`${at}.layout: out of bounds`); continue;
    }
    let overlap = false;
    for (let row = y; row < y + h; row++) for (let col = x; col < x + w; col++) {
      const cell = row * 12 + col; if (occupied[cell]) overlap = true; occupied[cell] = 1;
    }
    if (overlap) errors.push(`${at}.layout: overlap`);
  }
  return { valid: errors.length === 0, errors };
}

/** The declaration order is the UI control order; omitted kinds stay omitted. */
export function dashboardControlKinds(definition) {
  return definition.variables.map((variable) => variable.kind);
}

const migrations = new Map([[1, (value) => value]]);
export function migrateDashboard(value) {
  if (!record(value) || !integer(value.schemaVersion)) throw new Error("schemaVersion is required");
  const migrate = migrations.get(value.schemaVersion);
  if (!migrate) throw new Error(`unsupported dashboard schemaVersion ${value.schemaVersion}`);
  const result = migrate(value);
  const validation = validateDashboard(result);
  if (!validation.valid) throw new Error(validation.errors.join("; "));
  return result;
}

export function validateEmbeddedFiles(files, queryRefs) {
  if (files.length > DASHBOARD_LIMITS.files) throw new Error(`dashboard file count exceeds ${DASHBOARD_LIMITS.files}`);
  const dashboards = [];
  const ids = new Set();
  for (const file of files) {
    if (file.name.includes("..") || file.name.includes("/") || file.name.includes("\\")) throw new Error(`unsafe dashboard filename: ${file.name}`);
    if (!file.name.endsWith(".json")) throw new Error(`non-json dashboard file: ${file.name}`);
    if (file.kind !== "file") throw new Error(`dashboard entry must be a regular file: ${file.name}`);
    if (file.size > DASHBOARD_LIMITS.fileBytes) throw new Error(`dashboard file is too large: ${file.name}`);
    if (typeof file.text !== "string" || new TextEncoder().encode(file.text).byteLength !== file.size) throw new Error(`dashboard file size mismatch: ${file.name}`);
    let value; try { value = JSON.parse(file.text); } catch { throw new Error(`invalid dashboard JSON: ${file.name}`); }
    const validation = validateDashboard(value, queryRefs);
    if (!validation.valid) throw new Error(`${file.name}: ${validation.errors.join("; ")}`);
    if (ids.has(value.id)) throw new Error(`duplicate dashboard id: ${value.id}`);
    ids.add(value.id); dashboards.push(value);
  }
  return dashboards;
}
