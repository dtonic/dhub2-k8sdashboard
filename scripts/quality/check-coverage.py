#!/usr/bin/env python3
import json
import pathlib
import re
import subprocess
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[2]
API = ROOT / "apps" / "api"
BUDGETS = json.loads((ROOT / "quality" / "budgets.json").read_text(encoding="utf-8"))["goCoverage"]
GENERATED_PROTO_PACKAGE = "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"


def coverage_failures(total, packages, budgets):
    failures = []
    if total < budgets["mergedMinimumPercent"]:
        failures.append(f"merged coverage {total:.1f}% < {budgets['mergedMinimumPercent']:.1f}%")
    for package, minimum in budgets["packageMinimumPercent"].items():
        actual = packages.get(package)
        if actual is None:
            failures.append(f"package coverage missing: {package}")
        elif actual < minimum:
            failures.append(f"{package} coverage {actual:.1f}% < {minimum:.1f}%")
    return failures


synthetic_packages = dict(BUDGETS["packageMinimumPercent"])
if not coverage_failures(BUDGETS["mergedMinimumPercent"] - 0.1, synthetic_packages, BUDGETS):
    raise SystemExit("coverage self-test failed: merged regression was accepted")
for synthetic_package, synthetic_minimum in BUDGETS["packageMinimumPercent"].items():
    if synthetic_minimum == 0:
        continue
    mutation = dict(synthetic_packages)
    mutation[synthetic_package] = synthetic_minimum - 0.1
    expected = f"{synthetic_package} coverage"
    if not any(failure.startswith(expected) for failure in coverage_failures(BUDGETS["mergedMinimumPercent"], mutation, BUDGETS)):
        raise SystemExit(f"coverage self-test failed: {synthetic_package} regression was accepted")
print("coverage negative mutations passed")

with tempfile.TemporaryDirectory(prefix="dashboard-coverage-") as tmp:
    profile = pathlib.Path(tmp) / "merged.out"
    # Generated protobuf accessors are verified by api-proto-check hashes and
    # regeneration drift. Keep all handwritten registry/transport/provider/cmd
    # packages in the merged denominator.
    packages = subprocess.run(
        ["go", "list", "./..."], cwd=API, check=True, text=True, stdout=subprocess.PIPE,
    ).stdout.splitlines()
    excluded = [package for package in packages if package == GENERATED_PROTO_PACKAGE]
    if excluded != [GENERATED_PROTO_PACKAGE]:
        raise SystemExit("generated protobuf coverage exclusion is missing or duplicated")
    coverpkg = ",".join(package for package in packages if package != GENERATED_PROTO_PACKAGE)
    result = subprocess.run(
        ["go", "test", f"-coverpkg={coverpkg}", f"-coverprofile={profile}", "./..."], cwd=API, text=True,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
    )
    print(result.stdout, end="")
    if result.returncode:
        raise SystemExit(result.returncode)
    summary = subprocess.run(
        ["go", "tool", "cover", f"-func={profile}"], cwd=API, check=True, text=True,
        stdout=subprocess.PIPE,
    ).stdout
    package_result = subprocess.run(
        ["go", "test", "-cover", "./..."], cwd=API, text=True,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
    )
    print(package_result.stdout, end="")
    if package_result.returncode:
        raise SystemExit(package_result.returncode)

package_coverage = {}
for line in package_result.stdout.splitlines():
    match = re.search(r"github\.com/xenx96/k8s-dashboard/apps/api/(\S+).*coverage: ([0-9.]+)%", line)
    if match:
        package_coverage[match.group(1)] = float(match.group(2))

total_match = re.search(r"total:\s+\(statements\)\s+([0-9.]+)%", summary)
if not total_match:
    raise SystemExit("merged coverage total was not produced")
total = float(total_match.group(1))
failures = coverage_failures(total, package_coverage, BUDGETS)

print(f"merged coverage: {total:.1f}% (minimum {BUDGETS['mergedMinimumPercent']:.1f}%)")
for package, minimum in BUDGETS["packageMinimumPercent"].items():
    print(f"package coverage: {package}={package_coverage.get(package, 0):.1f}% (minimum {minimum:.1f}%)")
if failures:
    raise SystemExit("\n".join(failures))
