#!/usr/bin/env python3
"""Extract one Collector config block from a rendered Helm manifest."""

from __future__ import annotations

import argparse
from pathlib import Path


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("manifest", type=Path)
    parser.add_argument("--suffix", required=True)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()

    for document in args.manifest.read_text(encoding="utf-8").split("\n---\n"):
        lines = document.splitlines()
        if "kind: ConfigMap" not in lines:
            continue
        names = [line.strip().removeprefix("name: ") for line in lines if line.startswith("  name: ")]
        if not names or not names[0].endswith(args.suffix):
            continue
        try:
            start = lines.index("  config.yaml: |") + 1
        except ValueError:
            continue
        config: list[str] = []
        for line in lines[start:]:
            if line and not line.startswith("    "):
                break
            config.append(line[4:] if line.startswith("    ") else "")
        args.output.write_text("\n".join(config) + "\n", encoding="utf-8")
        return 0
    raise SystemExit(f"ConfigMap suffix not found: {args.suffix}")


if __name__ == "__main__":
    raise SystemExit(main())
