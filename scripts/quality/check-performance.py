#!/usr/bin/env python3
import argparse
import gzip
import json
import pathlib
import re
import subprocess

ROOT = pathlib.Path(__file__).resolve().parents[2]
BUDGETS = json.loads((ROOT / "quality" / "budgets.json").read_text(encoding="utf-8"))

def validate_benchmarks(output, budgets):
    found = {}
    for line in output.splitlines():
        match = re.match(r"(Benchmark\w+)-\d+\s+\d+\s+\S+ ns/op\s+(\d+) B/op\s+(\d+) allocs/op", line)
        if match:
            found.setdefault(match.group(1), []).append((int(match.group(2)), int(match.group(3))))
    errors = []
    for name, limits in budgets.items():
        if name not in found:
            errors.append(f"benchmark missing: {name}")
            continue
        samples = found[name]
        if len(samples) < 3:
            errors.append(f"benchmark sample count {name}: {len(samples)} < 3")
        for index, (bytes_per_op, allocs_per_op) in enumerate(samples, 1):
            print(f"{name}[{index}]: {allocs_per_op} allocs/op, {bytes_per_op} B/op")
            if allocs_per_op > limits["maxAllocsPerOp"]:
                errors.append(f"{name} sample {index} allocations {allocs_per_op} > {limits['maxAllocsPerOp']}")
            if bytes_per_op > limits["maxBytesPerOp"]:
                errors.append(f"{name} sample {index} bytes {bytes_per_op} > {limits['maxBytesPerOp']}")
    return errors

def validate_bundle_sizes(raw, zipped, limits):
    errors = []
    if raw > limits["maxRawBytes"]:
        errors.append(f"web raw bundle {raw} > {limits['maxRawBytes']}")
    if zipped > limits["maxGzipBytes"]:
        errors.append(f"web gzip bundle {zipped} > {limits['maxGzipBytes']}")
    return errors

def validate_mock_absence(asset_bytes, worker_exists):
    errors = []
    if worker_exists:
        errors.append("production bundle contains mockServiceWorker.js")
    markers = (b"mockServiceWorker", b"Mocking enabled", b"setupWorker")
    if any(marker in body for body in asset_bytes for marker in markers):
        errors.append("production assets contain MSW runtime")
    return errors

def validate_bundle(root, limits):
    files = [path for path in (root / "apps" / "web" / "dist" / "assets").rglob("*") if path.is_file() and not path.name.endswith(".map")]
    if not files:
        return ["production bundle assets are missing"]
    raw = sum(path.stat().st_size for path in files)
    zipped = sum(len(gzip.compress(path.read_bytes(), compresslevel=9, mtime=0)) for path in files)
    print(f"web bundle: raw={raw} bytes, gzip={zipped} bytes, files={len(files)}")
    errors = validate_bundle_sizes(raw, zipped, limits)
    errors.extend(validate_mock_absence(
        [path.read_bytes() for path in files],
        (root / "apps" / "web" / "dist" / "mockServiceWorker.js").exists(),
    ))
    return errors

parser = argparse.ArgumentParser()
parser.add_argument("--self-test", action="store_true")
parser.add_argument("--go-only", action="store_true")
parser.add_argument("--bundle-only", action="store_true")
args = parser.parse_args()
if args.go_only and args.bundle_only:
    raise SystemExit("choose at most one of --go-only and --bundle-only")
if args.self_test:
    sample = "\n".join([
        "BenchmarkOperationalProbe-8 100 1 ns/op 7500 B/op 35 allocs/op",
        "BenchmarkOperationalProbe-8 100 1 ns/op 7000 B/op 30 allocs/op",
        "BenchmarkOperationalProbe-8 100 1 ns/op 7000 B/op 30 allocs/op",
    ])
    allocation_failure = validate_benchmarks(sample, {"BenchmarkOperationalProbe": {"maxAllocsPerOp": 34, "maxBytesPerOp": 7500}})
    bytes_failure = validate_benchmarks(sample, {"BenchmarkOperationalProbe": {"maxAllocsPerOp": 35, "maxBytesPerOp": 7499}})
    if len(allocation_failure) != 1 or "allocations" not in allocation_failure[0]:
        raise SystemExit("allocation overbudget mutation was masked")
    if len(bytes_failure) != 1 or "bytes" not in bytes_failure[0]:
        raise SystemExit("bytes overbudget mutation was masked")
    bundle_limits = {"maxRawBytes": 100, "maxGzipBytes": 50}
    raw_failure = validate_bundle_sizes(101, 50, bundle_limits)
    gzip_failure = validate_bundle_sizes(100, 51, bundle_limits)
    if len(raw_failure) != 1 or "raw" not in raw_failure[0]:
        raise SystemExit("raw bundle overbudget mutation was masked")
    if len(gzip_failure) != 1 or "gzip" not in gzip_failure[0]:
        raise SystemExit("gzip bundle overbudget mutation was masked")
    if len(validate_mock_absence([b"setupWorker runtime"], False)) != 1:
        raise SystemExit("MSW runtime mutation was masked")
    if len(validate_mock_absence([], True)) != 1:
        raise SystemExit("MSW worker mutation was masked")
    print("negative mutation passed: allocation overbudget was rejected")
    print("negative mutation passed: byte overbudget was rejected")
    print("negative mutation passed: raw bundle overbudget was rejected")
    print("negative mutation passed: gzip bundle overbudget was rejected")
    print("negative mutation passed: MSW runtime was rejected")
    print("negative mutation passed: MSW worker was rejected")
    raise SystemExit(0)

errors = []
if not args.bundle_only:
    bench = subprocess.run(
        ["go", "test", "-run=^$", "-bench=Benchmark(OperationalProbe|OverviewForbidden)$", "-benchmem", "-count=3", "./internal/httpapi"],
        cwd=ROOT / "apps" / "api", text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
    )
    print(bench.stdout, end="")
    if bench.returncode:
        raise SystemExit(bench.returncode)
    errors.extend(validate_benchmarks(bench.stdout, BUDGETS["goBenchmarks"]))
if not args.go_only:
    errors.extend(validate_bundle(ROOT, BUDGETS["webBundle"]))
if errors:
    raise SystemExit("\n".join(errors))
