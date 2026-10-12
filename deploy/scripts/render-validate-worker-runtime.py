#!/usr/bin/env python3
"""Render deploy/runtime/worker-runtime.rust-validate.json from the agent
worker's document.

The VALIDATE-bound Rust worker (deploy/docker-compose.standalone-rust-validate.yml)
is the same image and the same runtime document as the agent worker, differing
only in the identity it presents and the stream it consumes. Keeping a second
hand-edited copy let the two drift (a limit changed in one and not the other
is invisible until a worker misbehaves), so the validate document is derived:
the agent worker's document, with exactly the keys in OVERRIDES replaced.

    python3 deploy/scripts/render-validate-worker-runtime.py          # write
    python3 deploy/scripts/render-validate-worker-runtime.py --check  # CI

`--check` exits 1 when the committed file is not what this script renders.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

RUNTIME = Path(__file__).resolve().parents[1] / "runtime"
SOURCE = RUNTIME / "worker-runtime.rust.json"
TARGET = RUNTIME / "worker-runtime.rust-validate.json"

# The keys that differ, and nothing else. SESSION/PRODUCER are the second
# workload_sessions row written by validate-runtime-session-init in the
# overlay; the consumer id is this process's NATS connection name; the stream
# and durable are the VALIDATE route (runtimecomposition/config.go
# ELITEA_RUNTIME_COMMAND_STREAM / ELITEA_RUNTIME_COMMAND_CONSUMER).
OVERRIDES = {
    "workload_session_id": "standalone-validate-worker-1",
    "producer_id": "standalone-validate-worker-1",
    "consumer_id": "standalone-validate-worker-1-consumer",
    "nats_stream": "ELITEA_RT_V1_VALIDATE",
    "nats_consumer": "elitea-configuration-worker-v1",
}


def render() -> str:
    source = json.loads(SOURCE.read_text())
    missing = sorted(set(OVERRIDES) - set(source))
    if missing:
        sys.exit(f"{SOURCE.name} lacks keys the override replaces: {missing}")
    document = {key: OVERRIDES.get(key, value) for key, value in source.items()}
    return json.dumps(document, indent=2) + "\n"


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument(
        "--check",
        action="store_true",
        help="fail instead of writing when the committed file differs",
    )
    args = parser.parse_args()
    rendered = render()
    if args.check:
        current = TARGET.read_text() if TARGET.exists() else None
        if current != rendered:
            sys.exit(
                f"{TARGET.relative_to(RUNTIME.parents[1])} is not derived from "
                f"{SOURCE.name}; run {Path(__file__).name} and commit the result"
            )
        print(f"{TARGET.name}: derived from {SOURCE.name}")
        return
    TARGET.write_text(rendered)
    print(f"wrote {TARGET}")


if __name__ == "__main__":
    main()
