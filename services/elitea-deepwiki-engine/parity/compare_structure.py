#!/usr/bin/env python3
"""Compare the Python and Rust sides of the ADR-0026 structure gate.

    python3 parity/compare_structure.py <py-dump> <rust-out>

``<py-dump>`` is what ``python_structure_dump.py`` wrote; ``<rust-out>`` is
what ``deepwiki-parity structure-dump`` wrote, with the LLM stub's request
record as ``requests.jsonl`` beside it.

The gate:

* the same number of chat requests, in the same order, and per request the
  same ``messages`` (role and content, byte for byte), ``model``,
  ``temperature``, ``tools`` and token budget (``max_completion_tokens`` /
  ``max_tokens``);
* ``structure.json`` byte-identical (``json.dumps(model_dump(), indent=2)``);
* the analysis state (context, tree, README) equal.

Keys present on one side only are listed (the Rust client asks for
``stream_options.include_usage``; LangChain did not) but are not part of the
gate. Exit status 1 on a difference.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

GATED = ("messages", "model", "temperature", "tools", "max_completion_tokens", "max_tokens")


def load(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]


def first_difference(a: str, b: str) -> str:
    for i, (x, y) in enumerate(zip(a, b)):
        if x != y:
            return f"at char {i}: {a[max(0, i - 60):i + 60]!r} != {b[max(0, i - 60):i + 60]!r}"
    return f"lengths {len(a)} != {len(b)}"


def main() -> int:
    py_dir, rs_dir = Path(sys.argv[1]), Path(sys.argv[2])
    py_requests = load(py_dir / "requests.jsonl")
    rs_requests = load(rs_dir / "requests.jsonl")
    problems: list[str] = []
    if len(py_requests) != len(rs_requests):
        problems.append(f"request count {len(py_requests)} != {len(rs_requests)}")
    extra_keys: set[str] = set()
    for i, (py, rs) in enumerate(zip(py_requests, rs_requests)):
        extra_keys |= set(py) ^ set(rs)
        for key in GATED:
            if py.get(key) != rs.get(key):
                detail = ""
                if key == "messages" and len(py.get(key, [])) == len(rs.get(key, [])):
                    for j, (pm, rm) in enumerate(zip(py[key], rs[key])):
                        if pm != rm:
                            detail = f" message {j}: " + first_difference(
                                json.dumps(pm, ensure_ascii=False), json.dumps(rm, ensure_ascii=False)
                            )
                            break
                problems.append(f"request {i}: {key} differs{detail}")
    py_structure = (py_dir / "structure.json").read_bytes()
    rs_structure = (rs_dir / "structure.json").read_bytes()
    structure_identical = py_structure == rs_structure
    if not structure_identical:
        problems.append(
            "structure.json differs: "
            + first_difference(py_structure.decode("utf-8"), rs_structure.decode("utf-8"))
        )
    py_analysis = json.loads((py_dir / "analysis.json").read_text(encoding="utf-8"))
    rs_analysis = json.loads((rs_dir / "analysis.json").read_text(encoding="utf-8"))
    for key in ("repository_context", "repository_tree", "readme_content"):
        if py_analysis.get(key) != rs_analysis.get(key):
            problems.append(f"analysis {key} differs: " + first_difference(str(py_analysis.get(key)), str(rs_analysis.get(key))))
    structure = json.loads(py_structure)
    report = {
        "requests": len(py_requests),
        "requests_identical": not any(p.startswith(("request", "request count")) for p in problems),
        "structure_identical": structure_identical,
        "sections": len(structure["sections"]),
        "pages": structure["total_pages"],
        "ungated_keys": sorted(extra_keys),
        "problems": problems[:20],
    }
    print(json.dumps(report, indent=2))
    return 1 if problems else 0


if __name__ == "__main__":
    raise SystemExit(main())
