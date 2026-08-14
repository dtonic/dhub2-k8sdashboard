#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";

const allowlistPath = new URL("../../quality/npm-audit-allowlist.json", import.meta.url);
const lockPath = new URL("../../package-lock.json", import.meta.url);
const allowlist = JSON.parse(readFileSync(allowlistPath, "utf8"));
const lock = JSON.parse(readFileSync(lockPath, "utf8"));

function validate(report, now = new Date(), versions = undefined) {
  const errors = [];
  if (now > new Date(`${allowlist.reviewBy}T23:59:59Z`)) errors.push(`allowlist expired: ${allowlist.reviewBy}`);
  const allowedByPackage = new Map(allowlist.entries.map((entry) => [entry.package, entry]));
  const observed = new Map();
  for (const [pkg, vulnerability] of Object.entries(report.vulnerabilities ?? {})) {
    const entry = allowedByPackage.get(pkg);
    if (!entry) {
      errors.push(`unallowlisted ${vulnerability.severity} vulnerability: ${pkg}`);
      continue;
    }
    const installed = versions?.[pkg] ?? lock.packages?.[`node_modules/${pkg}`]?.version;
    if (installed !== entry.installedVersion) errors.push(`${pkg} version drift: ${installed} != ${entry.installedVersion}`);
    if (vulnerability.severity !== entry.severity) errors.push(`${pkg} severity drift: ${vulnerability.severity} != ${entry.severity}`);
    if (vulnerability.range !== entry.vulnerabilityRange) errors.push(`${pkg} vulnerability range drift: ${vulnerability.range} != ${entry.vulnerabilityRange}`);
    if (JSON.stringify(vulnerability.fixAvailable) !== JSON.stringify(entry.fixAvailable)) errors.push(`${pkg} fixAvailable drift`);
    const advisories = (vulnerability.via ?? []).filter((item) => typeof item === "object");
    const tuples = advisories.map((item) => ({
      id: /GHSA-[\w-]+/.exec(item.url ?? "")?.[0],
      range: item.range,
      severity: item.severity,
    })).filter((item) => item.id);
    const ids = tuples.map((item) => item.id);
    const unknown = ids.filter((id) => !(id in entry.advisories));
    if (unknown.length) errors.push(`${pkg} new advisories: ${unknown.join(",")}`);
    for (const tuple of tuples) {
      if (tuple.id in entry.advisories && tuple.range !== entry.advisories[tuple.id]) errors.push(`${pkg} advisory range drift: ${tuple.id}`);
      if (tuple.severity !== entry.severity) errors.push(`${pkg} advisory severity drift: ${tuple.id}`);
    }
    observed.set(pkg, new Set(ids));
  }
  for (const entry of allowlist.entries) {
    const seen = observed.get(entry.package) ?? new Set();
    const missing = Object.keys(entry.advisories).filter((id) => !seen.has(id));
    if (missing.length) errors.push(`${entry.package} allowlist drift, advisories absent: ${missing.join(",")}`);
  }
  return errors;
}

if (process.argv.includes("--self-test")) {
  const versions = Object.fromEntries(allowlist.entries.map((entry) => [entry.package, entry.installedVersion]));
  const base = { vulnerabilities: Object.fromEntries(allowlist.entries.map((entry) => [entry.package, {
    severity: entry.severity,
    range: entry.vulnerabilityRange,
    fixAvailable: entry.fixAvailable,
    via: Object.entries(entry.advisories).map(([id, range]) => ({ url: `https://github.com/advisories/${id}`, range, severity: entry.severity })),
  }])) };
  const unknown = structuredClone(base);
  unknown.vulnerabilities["react-router"].via.push({ url: "https://github.com/advisories/GHSA-xxxx-yyyy-zzzz" });
  const high = structuredClone(base);
  high.vulnerabilities["react-router"].severity = "high";
  const missing = structuredClone(base);
  missing.vulnerabilities["react-router"].via.pop();
  const range = structuredClone(base);
  range.vulnerabilities["react-router"].via[0].range = "<999";
  const fix = structuredClone(base);
  fix.vulnerabilities["react-router"].fixAvailable.version = "7.18.3";
  const driftedVersions = { ...versions, "react-router": "6.30.5" };
  const stringOnly = structuredClone(base);
  stringOnly.vulnerabilities["transitive-only"] = { severity: "high", range: "*", fixAvailable: false, via: ["react-router"] };
  const cases = [
    ["new advisories", validate(unknown, new Date("2026-08-14"), versions), "unknown advisory"],
    ["severity drift", validate(high, new Date("2026-08-14"), versions), "high severity"],
    ["allowlist drift", validate(missing, new Date("2026-08-14"), versions), "missing advisory"],
    ["advisory range drift", validate(range, new Date("2026-08-14"), versions), "range drift"],
    ["fixAvailable drift", validate(fix, new Date("2026-08-14"), versions), "fix drift"],
    ["version drift", validate(base, new Date("2026-08-14"), driftedVersions), "version drift"],
    ["unallowlisted high vulnerability: transitive-only", validate(stringOnly, new Date("2026-08-14"), versions), "string-only vulnerability"],
  ];
  for (const [expected, errors, label] of cases) {
    if (!errors.some((error) => error.includes(expected))) throw new Error(`${label} mutation was masked`);
    console.log(`negative mutation passed: ${label} was rejected`);
  }
  process.exit(0);
}

const audit = spawnSync("npm", ["audit", "--json"], {
  cwd: new URL("../../", import.meta.url), encoding: "utf8", shell: process.platform === "win32",
});
if (audit.error) throw audit.error;
const stdout = audit.stdout;
if (!stdout) throw new Error(audit.stderr || "npm audit produced no JSON");
const errors = validate(JSON.parse(stdout));
if (errors.length) {
  console.error(errors.join("\n"));
  process.exit(1);
}
console.log(`npm audit: only ${allowlist.entries.length} reviewed moderate exception(s), review by ${allowlist.reviewBy}`);
