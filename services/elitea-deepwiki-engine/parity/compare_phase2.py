#!/usr/bin/env python3
"""Compare two Phase 2 dumps edge class by edge class.

    python parity/compare_phase2.py <reference-dir> <candidate-dir> [--json report.json]

Both directories are ``--through phase2`` dumps (``nodes.jsonl``,
``edges.jsonl``, ``stats.json``), from ``python_reference.py`` or
``deepwiki-parity graph-dump``. The replay gate wants them byte-identical
(``cmp``); this script MEASURES how far apart two runs are when they are
not — the ADR-0026 "accepted difference" between the FTS5 / sqlite-vec
searches and the PostgreSQL build space (``python_reference.py
--search-dsn``).

Per Phase 2 producer (``created_by``) and per ``edge_class``: the edge
counts on each side and the Jaccard index of the (source, target,
rel_type) sets; then the hub sets, the weights of the edges both sides
have, and the stats that differ.
"""

from __future__ import annotations

import argparse
import collections
import json
import sys
from pathlib import Path


def load(directory: Path):
    edges = [json.loads(line) for line in open(directory / "edges.jsonl", encoding="utf-8")]
    hubs = set()
    with open(directory / "nodes.jsonl", encoding="utf-8") as handle:
        for line in handle:
            row = json.loads(line)
            if row.get("is_hub"):
                hubs.add(row["node_id"])
    stats = json.loads((directory / "stats.json").read_text(encoding="utf-8"))
    return edges, hubs, stats


def jaccard(a: set, b: set) -> float:
    return 1.0 if not a and not b else len(a & b) / len(a | b)


def flatten(value, prefix=""):
    if isinstance(value, dict):
        for key, item in value.items():
            yield from flatten(item, f"{prefix}{key}.")
    else:
        yield prefix.rstrip("."), value


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("reference", type=Path)
    parser.add_argument("candidate", type=Path)
    parser.add_argument("--json", type=Path)
    args = parser.parse_args()

    ref_edges, ref_hubs, ref_stats = load(args.reference)
    cand_edges, cand_hubs, cand_stats = load(args.candidate)

    def key(e):
        return (e["source_id"], e["target_id"], e["rel_type"])

    def groups(edges, field):
        out = collections.defaultdict(list)
        for e in edges:
            out[e[field]].append(e)
        return out

    report = {"by_created_by": {}, "by_edge_class": {}}
    for field, label in (("created_by", "by_created_by"), ("edge_class", "by_edge_class")):
        ref_g, cand_g = groups(ref_edges, field), groups(cand_edges, field)
        for name in sorted(set(ref_g) | set(cand_g)):
            a = {key(e) for e in ref_g.get(name, [])}
            b = {key(e) for e in cand_g.get(name, [])}
            report[label][name] = {
                "reference": len(ref_g.get(name, [])),
                "candidate": len(cand_g.get(name, [])),
                "jaccard": round(jaccard(a, b), 4),
                "only_reference": len(a - b),
                "only_candidate": len(b - a),
            }

    ref_w = {key(e) + (e["edge_class"],): e["weight"] for e in ref_edges}
    cand_w = {key(e) + (e["edge_class"],): e["weight"] for e in cand_edges}
    common = set(ref_w) & set(cand_w)
    report["weights"] = {
        "common_edges": len(common),
        "equal": sum(1 for k in common if ref_w[k] == cand_w[k]),
        "max_abs_diff": max((abs(ref_w[k] - cand_w[k]) for k in common), default=0.0),
    }
    report["hubs"] = {
        "reference": len(ref_hubs),
        "candidate": len(cand_hubs),
        "jaccard": round(jaccard(ref_hubs, cand_hubs), 4),
    }
    ref_flat, cand_flat = dict(flatten(ref_stats)), dict(flatten(cand_stats))
    report["stats_that_differ"] = {
        k: [ref_flat.get(k), cand_flat.get(k)]
        for k in sorted(set(ref_flat) | set(cand_flat))
        if ref_flat.get(k) != cand_flat.get(k)
    }

    for label in ("by_created_by", "by_edge_class"):
        print(f"== {label}")
        for name, entry in report[label].items():
            print(
                f"   {name:28} ref {entry['reference']:>7}  cand {entry['candidate']:>7}  "
                f"jaccard {entry['jaccard']:.4f}"
            )
    print(f"== hubs {report['hubs']}")
    print(f"== weights {report['weights']}")
    print(f"== stats that differ {report['stats_that_differ']}")
    if args.json:
        args.json.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    return 0


if __name__ == "__main__":
    sys.exit(main())
