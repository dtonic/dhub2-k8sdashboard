#!/usr/bin/env python3
"""Validate repository-owned dashboards/rules without a cluster."""
import json, re, sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
REQUIRED_PANELS = {"API request rate", "API error rate", "API p95 latency", "In-flight requests", "Response p95 bytes", "Upstream p95 latency", "Upstream outcomes", "Circuit state", "Cache hit ratio", "Query rejects and slow", "Informer sync", "SSE connections"}
SENSITIVE_LABELS = {"user", "subject", "path", "query", "queryref", "requestid", "url", "namespace"}
REQUIRED_METRICS = {"dashboard_http_requests_total", "dashboard_http_inflight_requests", "dashboard_http_request_duration_seconds_bucket", "dashboard_http_response_bytes_bucket", "dashboard_upstream_requests_total", "dashboard_upstream_request_duration_seconds_bucket", "dashboard_upstream_circuit_state", "dashboard_informer_synced", "dashboard_cache_requests_total", "dashboard_query_rejected_total", "dashboard_query_slow_total", "dashboard_stream_connections"}

def validate(dashboard, rules):
    panels = {p["title"] for p in dashboard.get("panels", [])}
    missing = REQUIRED_PANELS - panels
    if missing: raise ValueError(f"missing panels: {sorted(missing)}")
    if len(dashboard.get("panels", [])) != len(REQUIRED_PANELS) or len(panels) != len(REQUIRED_PANELS): raise ValueError("panel titles/count must be exact")
    ids = [p.get("id") for p in dashboard["panels"]]
    if None in ids or len(ids) != len(set(ids)): raise ValueError("panel ids must be unique")
    for p in dashboard["panels"]:
        if p.get("datasource", {}).get("uid") != "${DS_PROMETHEUS}" or "gridPos" not in p: raise ValueError(f"panel wiring: {p['title']}")
    for i, a in enumerate(dashboard["panels"]):
        ga=a["gridPos"]
        for b in dashboard["panels"][i+1:]:
            gb=b["gridPos"]
            if ga["x"] < gb["x"]+gb["w"] and gb["x"] < ga["x"]+ga["w"] and ga["y"] < gb["y"]+gb["h"] and gb["y"] < ga["y"]+ga["h"]: raise ValueError("overlapping panels")
    text = json.dumps(dashboard) + rules
    metrics = set(re.findall(r"\b(dashboard_[a-z0-9_]+)", text))
    go_text = "\n".join(p.read_text(encoding="utf-8") for p in (ROOT/"apps/api").rglob("*.go") if not p.name.endswith("_test.go"))
    known = set(re.findall(r"\b(dashboard_[a-z0-9_]+)", go_text))
    known |= {m+"_bucket" for m in known if m.endswith("_seconds") or m.endswith("_bytes")}
    unknown = metrics - known
    if unknown: raise ValueError(f"unknown metrics: {sorted(unknown)}")
    route_filter = 'route!~"healthz|readyz|version|stream"'
    for title in ("API request rate", "API error rate", "API p95 latency", "Response p95 bytes"):
        panel = next(p for p in dashboard["panels"] if p["title"] == title)
        if any(route_filter not in t.get("expr", "") for t in panel["targets"]): raise ValueError(f"missing API route filter: {title}")
    for alert in ("DashboardAPIUnavailable", "DashboardAPIHighLatency"):
        block = re.search(rf'- alert: {alert}\n(.*?)(?=\n      - alert:|\Z)', rules, re.S)
        if not block or route_filter not in block.group(1): raise ValueError(f"missing alert route filter: {alert}")
    unused = REQUIRED_METRICS - metrics
    if unused: raise ValueError(f"required production metrics unused: {sorted(unused)}")
    cache_text = (ROOT/"apps/api/internal/cache/cache.go").read_text(encoding="utf-8")
    cache_results = set(re.findall(r'Result\w+\s+Result\s*=\s*"([a-z0-9_]+)"', cache_text))
    for selector in re.findall(r'result=~\\?"([^"\\]+)', text):
        if set(selector.split("|")) - cache_results: raise ValueError("unknown cache result enum")
    for labels in re.findall(r"\{([^{}]*)\}", text):
        for label in re.findall(r"([A-Za-z_][A-Za-z0-9_]*)\s*(?:=|=~)", labels):
            if label.lower() in SENSITIVE_LABELS: raise ValueError(f"cardinality-sensitive label: {label}")
    alert_blocks = re.findall(r'- alert: [A-Za-z0-9_]+\n(.*?)(?=\n      - alert:|\Z)', rules, re.S)
    if not alert_blocks or any(b.count("runbook_url:") != 1 for b in alert_blocks): raise ValueError("every alert needs exactly one runbook")
    if any(b.count("severity:") != 1 for b in alert_blocks): raise ValueError("every alert needs exactly one severity")
    if not (ROOT / "docs/runbooks/platform-observability.md").is_file(): raise ValueError("missing runbook")
    runbook = (ROOT / "docs/runbooks/platform-observability.md").read_text(encoding="utf-8")
    for anchor in re.findall(r"platform-observability\.md#([a-z0-9-]+)", rules):
        if f'id="{anchor}"' not in runbook: raise ValueError(f"missing runbook anchor: {anchor}")
    urls = re.findall(r'runbook_url:\s*"([^"]+)"', rules)
    if len(urls) != len(alert_blocks) or any(not u.startswith("https://github.com/") or "#" not in u for u in urls): raise ValueError("runbook URLs must be stable HTTPS GitHub links with anchors")

def main():
    dashboard = json.loads((ROOT / "deploy/helm/observability-dashboard/files/dashboard.json").read_text(encoding="utf-8"))
    rules = (ROOT / "deploy/monitoring/alerts.yaml").read_text(encoding="utf-8")
    validate(dashboard, rules)
    if "--self-test" in sys.argv:
        def rejected(d, r, expected):
            try: validate(d, r)
            except ValueError as e:
                if expected not in str(e): raise
                return
            raise SystemExit(f"negative mutation was accepted: {expected}")
        bad = json.loads(json.dumps(dashboard)); bad["panels"][0]["targets"][0]["expr"] = "dashboard_unknown_metric"
        rejected(bad, rules, "unknown metrics")
        bad = json.loads(json.dumps(dashboard)); bad["panels"][0]["targets"][0]["expr"] += '{subject="x"}'
        rejected(bad, rules, "cardinality-sensitive")
        bad = json.loads(json.dumps(dashboard)); bad["panels"][8]["targets"][0]["expr"] = bad["panels"][8]["targets"][0]["expr"].replace("coalesced", "unknown_hit")
        rejected(bad, rules, "cache result")
        bad = json.loads(json.dumps(dashboard)); bad["panels"][1]["id"] = bad["panels"][0]["id"]
        rejected(bad, rules, "panel ids")
        bad = json.loads(json.dumps(dashboard)); bad["panels"][1]["title"] = bad["panels"][0]["title"]
        rejected(bad, rules, "missing panels")
        bad = json.loads(json.dumps(dashboard)); bad["panels"][1]["gridPos"] = dict(bad["panels"][0]["gridPos"])
        rejected(bad, rules, "overlapping panels")
        bad = json.loads(json.dumps(dashboard)); bad["panels"][0].pop("datasource")
        rejected(bad, rules, "panel wiring")
        bad = json.loads(json.dumps(dashboard)); bad["panels"][0]["targets"][0]["expr"] = "dashboard_http_requests_total"
        rejected(bad, rules, "route filter")
        bad_rules = rules.replace('route!~"healthz|readyz|version|stream"', 'route!~"healthz"')
        rejected(dashboard, bad_rules, "alert route filter")
        rejected(dashboard, rules.replace("https://github.com/", "http://invalid/"), "stable HTTPS")
        rejected(dashboard, rules.replace("#api-outage", "#missing-anchor", 1), "missing runbook anchor")
        rejected(dashboard, rules.replace("        labels: {severity: critical}\n", "", 1), "severity")
        bad = json.loads(json.dumps(dashboard)); bad["panels"][11]["targets"][0]["expr"] = "dashboard_informer_synced"
        rejected(bad, rules, "required production metrics unused")
    print("observability assets: ok")

if __name__ == "__main__": main()
