#!/usr/bin/env python3
"""Validate telemetry comparison evidence before copying it into Helm values."""

from __future__ import annotations

import argparse
import hashlib
import json
import math
from datetime import datetime, timezone
from pathlib import Path


def timestamp(value: str) -> datetime:
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise ValueError("timestamp must include a timezone")
    return parsed.astimezone(timezone.utc)


def integer(value: object, name: str) -> int:
    if not isinstance(value, int) or isinstance(value, bool):
        raise ValueError(f"{name} must be a JSON integer")
    return value


def number(value: object, name: str) -> float:
    if not isinstance(value, (int, float)) or isinstance(value, bool) or not math.isfinite(value) or value < 0:
        raise ValueError(f"{name} must be a nonnegative JSON number")
    return float(value)


def reject_duplicates(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("evidence", type=Path)
    parser.add_argument("--environment", choices=("local", "stage", "prod"), required=True)
    parser.add_argument("--max-age-minutes", type=int, default=120)
    parser.add_argument("--helm-values-out", type=Path)
    args = parser.parse_args()

    data = json.loads(
        args.evidence.read_text(encoding="utf-8"),
        object_pairs_hook=reject_duplicates,
        parse_constant=lambda value: (_ for _ in ()).throw(ValueError(f"invalid JSON constant: {value}")),
    )
    allowed = {
        "schemaVersion", "environment", "kind", "startedAt", "endedAt", "windowMinutes", "raw",
        "artifactHash", "lossPermille", "greptimeTableVisibilityMs", "greptimeAllQueryVisibilityMs",
        "quickwitVisibilityMs", "endToEndRuntimeMs", "p95LatencyMs", "collectorCpuMillicores",
        "collectorMemoryMiB", "egressBytesPerHour", "storageBytesPerDay", "estimatedCostMicrosPerDay",
        "operatorProductionMeasurementsRequired",
    }
    schema_path = Path(__file__).resolve().parents[1] / "telemetry" / "comparison.schema.json"
    schema = json.loads(schema_path.read_text(encoding="utf-8"))
    if set(schema.get("properties", {})) != allowed or set(schema.get("required", ())) != allowed:
        raise SystemExit("comparison.schema.json and evidence checker keys drifted")
    if set(data) != allowed:
        raise SystemExit(f"evidence keys mismatch: {sorted(set(data) ^ allowed)}")
    if data.get("schemaVersion") != 1 or data.get("environment") != args.environment:
        raise SystemExit("evidence schema/environment mismatch")
    expected_kind = "local-synthetic-comparison" if args.environment == "local" else "production-comparison"
    if data.get("kind") != expected_kind:
        raise SystemExit(f"evidence kind must be {expected_kind}")
    started, ended = timestamp(data["startedAt"]), timestamp(data["endedAt"])
    now = datetime.now(timezone.utc)
    duration = (ended - started).total_seconds()
    if duration < 0 or (now - ended).total_seconds() > args.max_age_minutes * 60 or ended > now:
        raise SystemExit("evidence time window is invalid or stale")
    if integer(data["windowMinutes"], "windowMinutes") != max(1, math.ceil(duration / 60)):
        raise SystemExit("windowMinutes does not match startedAt/endedAt")

    artifact = {key: value for key, value in data.items() if key != "artifactHash"}
    digest = hashlib.sha256(json.dumps(artifact, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    if data.get("artifactHash") != digest:
        raise SystemExit("evidence artifactHash mismatch")
    raw = data["raw"]
    raw_keys = {
        "comparisonScope", "baselineTopology", "candidateTopology", "corpusDigest", "corpusEventDigests", "corpusCount",
        "baselineEvents", "candidateEvents", "duplicates", "injected503", "permanent503Attempts",
        "payloadBytes", "quickwitDocuments", "baselineLatenciesMs", "candidateLatenciesMs",
        "baselineTrialEventCounts", "candidateTrialEventCounts", "candidateMeasurementDurationMs", "cpuTrialDurationMs",
        "baselineTrialP95Ms", "candidateTrialP95Ms",
        "baselineCollectorSamples", "candidateCollectorSamples", "storedBytes", "observedStoredEvents", "assumptions",
        "baselineCpuMillicores", "baselineMemoryMiB", "cpuDeltaMillicores", "memoryDeltaMiB",
        "baselineCpuTimeNanos", "candidateCpuTimeNanos", "baselineCpuStatMicros", "candidateCpuStatMicros",
        "cpuTimeDeltaNanos", "baselineCpuNanosPerEvent", "candidateCpuNanosPerEvent",
    }
    if set(raw) != raw_keys:
        raise SystemExit("raw evidence keys mismatch")
    expected_topology = (
        ("synthetic-otlp-hop", "pinned-source-collector->mock", "pinned-source-collector->gateway-transform->mock")
        if args.environment == "local" else
        ("production-existing-vs-otel", "existing-production-collector->backend", "otel-agent-cluster-gateway->backend")
    )
    if (raw["comparisonScope"], raw["baselineTopology"], raw["candidateTopology"]) != expected_topology:
        raise SystemExit("comparison topology/scope mismatch")
    event_digests = raw["corpusEventDigests"]
    if not isinstance(event_digests, list) or any(
        not isinstance(value, str) or len(value) != 64 or any(char not in "0123456789abcdef" for char in value)
        for value in event_digests
    ):
        raise SystemExit("corpus event digests are invalid")
    derived_corpus_digest = hashlib.sha256(json.dumps(event_digests, separators=(",", ":")).encode()).hexdigest()
    if raw["corpusDigest"] != derived_corpus_digest:
        raise SystemExit("corpus digest does not match event identities")
    expected = integer(raw["baselineEvents"], "baselineEvents")
    received = integer(raw["candidateEvents"], "candidateEvents")
    if integer(raw["corpusCount"], "corpusCount") != expected:
        raise SystemExit("corpus count does not match baseline events")
    if len(event_digests) != expected or len(set(event_digests)) != expected:
        raise SystemExit("corpus event identities must be unique and complete")
    if expected < 20 or received < 1 or integer(raw["duplicates"], "duplicates") != 0 or integer(raw["payloadBytes"], "payloadBytes") < 1:
        raise SystemExit("invalid raw sample counts or duplicate samples")
    derived_loss = max(0, (expected - received) * 1000 // expected)
    if integer(data["lossPermille"], "lossPermille") != derived_loss:
        raise SystemExit("lossPermille does not match raw sample counts")
    for key in ("greptimeTableVisibilityMs", "greptimeAllQueryVisibilityMs", "quickwitVisibilityMs", "endToEndRuntimeMs"):
        if integer(data[key], key) < 0:
            raise SystemExit(f"invalid bounded measurement: {key}")
    baseline_latencies = raw["baselineLatenciesMs"]
    candidate_latencies = raw["candidateLatenciesMs"]
    if not isinstance(baseline_latencies, list) or not isinstance(candidate_latencies, list) or len(baseline_latencies) != expected or len(candidate_latencies) != received:
        raise SystemExit("latency sample counts do not match raw event counts")
    for name, values in (("baseline", baseline_latencies), ("candidate", candidate_latencies)):
        if any(integer(value, f"{name}Latency") < 0 for value in values):
            raise SystemExit("invalid latency sample")
    derived_p95 = sorted(candidate_latencies)[math.ceil(len(candidate_latencies) * 0.95) - 1]
    if integer(data["p95LatencyMs"], "p95LatencyMs") != derived_p95:
        raise SystemExit("p95LatencyMs does not match candidate samples")
    if args.environment == "local" and (expected != 30 or received != 30):
        raise SystemExit("local comparison corpus must contain three complete 10-event trials")
    for name, latencies in (("baseline", baseline_latencies), ("candidate", candidate_latencies)):
        trial_key = f"{name}TrialP95Ms"
        count_key = f"{name}TrialEventCounts"
        trials = raw[trial_key]
        counts = raw[count_key]
        if not isinstance(counts, list) or len(counts) != 3 or any(integer(value, count_key) < 1 for value in counts) or sum(counts) != len(latencies):
            raise SystemExit(f"{count_key} must define three complete trials")
        derived_trials, offset = [], 0
        for count in counts:
            values = latencies[offset:offset + count]
            derived_trials.append(sorted(values)[math.ceil(count * 0.95) - 1])
            offset += count
        if not isinstance(trials, list) or len(trials) != 3 or any(integer(value, trial_key) < 0 for value in trials) or trials != derived_trials:
            raise SystemExit(f"{trial_key} does not match the declared trials")

    baseline_samples, candidate_samples = raw["baselineCollectorSamples"], raw["candidateCollectorSamples"]
    if not isinstance(baseline_samples, list) or len(baseline_samples) < 3 or not isinstance(candidate_samples, list) or len(candidate_samples) < 3:
        raise SystemExit("collector comparison requires at least three resource samples")
    def check_sample(sample: object) -> dict[str, object]:
        if not isinstance(sample, dict) or set(sample) != {"name", "cpuPercent", "memoryMiB"} or not isinstance(sample["name"], str):
            raise SystemExit("invalid collector stats sample")
        number(sample["cpuPercent"], "cpuPercent")
        number(sample["memoryMiB"], "memoryMiB")
        return sample
    for sample in baseline_samples:
        if check_sample(sample)["name"] != "baseline":
            raise SystemExit("baseline stats must belong to the baseline collector")
    baseline_cpu = math.ceil(max(number(sample["cpuPercent"], "cpuPercent") * 10 for sample in baseline_samples))
    baseline_memory = math.ceil(max(number(sample["memoryMiB"], "memoryMiB") for sample in baseline_samples))
    candidate_memory = []
    for trial in candidate_samples:
        if not isinstance(trial, list) or len(trial) != 2:
            raise SystemExit("candidate stats must contain source+gateway per trial")
        checked = [check_sample(sample) for sample in trial]
        if {sample["name"] for sample in checked} != {"source", "gateway"}:
            raise SystemExit("candidate stats must belong exactly to source and gateway")
        for sample in checked:
            number(sample["cpuPercent"], "cpuPercent")
        candidate_memory.append(sum(number(sample["memoryMiB"], "memoryMiB") for sample in checked))
    if integer(data["collectorMemoryMiB"], "collectorMemoryMiB") != math.ceil(max(candidate_memory)):
        raise SystemExit("collectorMemoryMiB does not match raw samples")
    if integer(raw["baselineMemoryMiB"], "baselineMemoryMiB") != baseline_memory:
        raise SystemExit("baseline resource summary does not match raw samples")
    if integer(raw["memoryDeltaMiB"], "memoryDeltaMiB") != integer(data["collectorMemoryMiB"], "collectorMemoryMiB") - baseline_memory:
        raise SystemExit("collector memory delta does not match baseline/candidate")
    baseline_cpu_time, candidate_cpu_time = raw["baselineCpuTimeNanos"], raw["candidateCpuTimeNanos"]
    if not isinstance(baseline_cpu_time, list) or not isinstance(candidate_cpu_time, list) or len(baseline_cpu_time) != 3 or len(candidate_cpu_time) != 3:
        raise SystemExit("schedstat CPU evidence requires three trials")
    if any(integer(value, "cpuTimeNanos") <= 0 for value in baseline_cpu_time + candidate_cpu_time):
        raise SystemExit("collector cumulative CPU must advance in every trial")
    for key in ("baselineCpuStatMicros", "candidateCpuStatMicros"):
        values = raw[key]
        if not isinstance(values, list) or len(values) != 3 or any(integer(value, key) < 0 for value in values):
            raise SystemExit("stat tick cross-check requires three nonnegative trials")
    baseline_cpu_total, candidate_cpu_total = sum(baseline_cpu_time), sum(candidate_cpu_time)
    cpu_trial_durations = raw["cpuTrialDurationMs"]
    if not isinstance(cpu_trial_durations, list) or len(cpu_trial_durations) != 3 or any(integer(value, "cpuTrialDurationMs") < 1 for value in cpu_trial_durations):
        raise SystemExit("CPU observation requires three positive trial durations")
    cpu_observation_ms = sum(cpu_trial_durations)
    baseline_cpu = math.ceil(baseline_cpu_total / (cpu_observation_ms * 1000))
    candidate_cpu = math.ceil(candidate_cpu_total / (cpu_observation_ms * 1000))
    if integer(raw["baselineCpuMillicores"], "baselineCpuMillicores") != baseline_cpu:
        raise SystemExit("baseline CPU average does not match schedstat/wall interval")
    if integer(data["collectorCpuMillicores"], "collectorCpuMillicores") != candidate_cpu:
        raise SystemExit("collector CPU average does not match schedstat/wall interval")
    if integer(raw["cpuDeltaMillicores"], "cpuDeltaMillicores") != candidate_cpu - baseline_cpu:
        raise SystemExit("collector CPU delta does not match baseline/candidate")
    if integer(raw["cpuTimeDeltaNanos"], "cpuTimeDeltaNanos") != candidate_cpu_total - baseline_cpu_total:
        raise SystemExit("cumulative CPU delta mismatch")
    if integer(raw["baselineCpuNanosPerEvent"], "baselineCpuNanosPerEvent") != math.ceil(baseline_cpu_total / expected):
        raise SystemExit("baseline CPU/event mismatch")
    if integer(raw["candidateCpuNanosPerEvent"], "candidateCpuNanosPerEvent") != math.ceil(candidate_cpu_total / received):
        raise SystemExit("candidate CPU/event mismatch")

    stored, assumptions = raw["storedBytes"], raw["assumptions"]
    if not isinstance(stored, dict) or set(stored) != {"greptime", "quickwit"}:
        raise SystemExit("storedBytes keys mismatch")
    stored_total = integer(stored["greptime"], "greptimeStoredBytes") + integer(stored["quickwit"], "quickwitStoredBytes")
    observed_events = integer(raw["observedStoredEvents"], "observedStoredEvents")
    assumption_keys = {"eventsPerHour", "retentionDays", "replicationFactor", "priceMicrosPerGiBMonth", "currency", "priceUnit", "priceSource"}
    if not isinstance(assumptions, dict) or set(assumptions) != assumption_keys:
        raise SystemExit("cost assumption keys mismatch")
    events_per_hour = integer(assumptions["eventsPerHour"], "eventsPerHour")
    retention = integer(assumptions["retentionDays"], "retentionDays")
    replication = integer(assumptions["replicationFactor"], "replicationFactor")
    price = integer(assumptions["priceMicrosPerGiBMonth"], "priceMicrosPerGiBMonth")
    if observed_events < 1 or events_per_hour < 1 or retention < 1 or replication < 1 or price < 0:
        raise SystemExit("invalid storage/cost assumptions")
    measurement_ms = integer(raw["candidateMeasurementDurationMs"], "candidateMeasurementDurationMs")
    if measurement_ms < 1:
        raise SystemExit("candidate measurement duration must be positive")
    derived_events_per_hour = math.ceil(received * 3_600_000 / measurement_ms)
    if events_per_hour != derived_events_per_hour:
        raise SystemExit("eventsPerHour does not match candidate latency samples")
    derived_storage = math.ceil(stored_total / observed_events * events_per_hour * 24 * replication)
    derived_cost = math.ceil(derived_storage * retention * price / (1024**3 * 30))
    derived_egress = math.ceil(integer(raw["payloadBytes"], "payloadBytes") / received * events_per_hour)
    if integer(data["storageBytesPerDay"], "storageBytesPerDay") != derived_storage or integer(data["estimatedCostMicrosPerDay"], "estimatedCostMicrosPerDay") != derived_cost or integer(data["egressBytesPerHour"], "egressBytesPerHour") != derived_egress:
        raise SystemExit("derived egress/storage/cost mismatch")
    if args.environment == "local" and (integer(raw["injected503"], "injected503") != 1 or not 2 <= integer(raw["permanent503Attempts"], "permanent503Attempts") <= 20):
        raise SystemExit("local failure-isolation evidence mismatch")

    if args.environment in ("stage", "prod"):
        required = (
            "p95LatencyMs", "collectorCpuMillicores", "collectorMemoryMiB", "egressBytesPerHour",
            "storageBytesPerDay", "estimatedCostMicrosPerDay",
        )
        if any(integer(data.get(key), key) < 0 for key in required):
            raise SystemExit("stage/prod evidence requires real latency/resource/storage/cost measurements")
        if integer(data["windowMinutes"], "windowMinutes") < 30:
            raise SystemExit("stage/prod evidence requires at least 30 minutes")
        if data["operatorProductionMeasurementsRequired"] is not False:
            raise SystemExit("production evidence must mark operator measurements complete")
    elif data["operatorProductionMeasurementsRequired"] is not True:
        raise SystemExit("local evidence must retain the production-measurement warning")
    if args.helm_values_out and args.environment == "local":
        raise SystemExit("local evidence cannot generate a cutover values overlay")
    if args.helm_values_out:
        prefix = "local-fixture" if args.environment == "local" else args.environment
        lines = [
            "telemetry:", "  comparison:", "    recorded: true",
            f"    kind: {data['kind']}", f"    evidenceId: {prefix}/{digest}",
            f"    artifactHash: {digest}", f"    startedAt: \"{data['startedAt']}\"",
            f"    endedAt: \"{data['endedAt']}\"", f"    windowMinutes: {data['windowMinutes']}",
            f"    baselineEvents: {expected}", f"    candidateEvents: {received}",
            f"    lossPermille: {derived_loss}", f"    p95LatencyMs: {data['p95LatencyMs']}",
            f"    collectorCpuMillicores: {data['collectorCpuMillicores']}",
            f"    collectorMemoryMiB: {data['collectorMemoryMiB']}",
            f"    egressBytesPerHour: {data['egressBytesPerHour']}",
            f"    storageBytesPerDay: {data['storageBytesPerDay']}",
            f"    estimatedCostMicrosPerDay: {data['estimatedCostMicrosPerDay']}",
        ]
        args.helm_values_out.write_text("\n".join(lines) + "\n", encoding="utf-8")
    print(f"telemetry evidence passed: environment={args.environment} sha256={digest}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
