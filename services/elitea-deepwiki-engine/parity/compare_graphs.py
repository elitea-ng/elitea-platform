#!/usr/bin/env python3
"""Compare a Rust graph dump with the Python reference — the ADR-0026 graph gate.

    python parity/compare_graphs.py <reference-dir> <candidate-dir> [--language go] [--min-jaccard 0.95] [--json report.json]

Both directories hold ``nodes.jsonl`` and ``edges.jsonl`` in the
``repo_nodes`` / ``repo_edges`` row shape (``python_reference.py`` writes the
reference; ``elitea-deepwiki-engine graph-dump`` writes the candidate).

What it reports, overall and per language:

* node-id Jaccard, with examples of ids only one side has;
* edge Jaccard over (source, target, rel_type) and the multiset of edges by
  (rel_type, edge_class);
* on nodes both sides have: how often each column agrees.

The gate (exit 1) is ADR-0026 decision 9: node-id Jaccard and edge Jaccard
at least ``--min-jaccard`` (0.95) for every language present in the
reference, unless ``--report-only``. Every gap is printed, because the rule
is "every gap explained", not merely "small".
"""

from __future__ import annotations

import argparse
import collections
import json
import sys
from pathlib import Path

COLUMNS = (
    "symbol_type", "symbol_name", "parent_symbol", "rel_path", "file_name",
    "start_line", "end_line", "signature", "docstring", "source_text",
    "return_type", "parameters", "is_architectural", "is_doc", "is_test",
    "analysis_level", "chunk_type",
)


def load(directory: Path):
    nodes = {}
    with open(directory / "nodes.jsonl", encoding="utf-8") as handle:
        for line in handle:
            row = json.loads(line)
            nodes[row["node_id"]] = row
    edges = []
    with open(directory / "edges.jsonl", encoding="utf-8") as handle:
        for line in handle:
            edges.append(json.loads(line))
    return nodes, edges


def language_of(node_id: str, nodes: dict) -> str:
    row = nodes.get(node_id)
    if row and row.get("language"):
        return row["language"]
    return node_id.split("::", 1)[0] if "::" in node_id else ""


def jaccard(a: set, b: set) -> float:
    if not a and not b:
        return 1.0
    return len(a & b) / len(a | b)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("reference", type=Path)
    parser.add_argument("candidate", type=Path)
    parser.add_argument("--language", action="append", default=[])
    parser.add_argument("--min-jaccard", type=float, default=0.95)
    parser.add_argument("--examples", type=int, default=8)
    parser.add_argument("--json", type=Path)
    parser.add_argument("--report-only", action="store_true")
    args = parser.parse_args()

    ref_nodes, ref_edges = load(args.reference)
    cand_nodes, cand_edges = load(args.candidate)

    languages = sorted({language_of(n, ref_nodes) for n in ref_nodes} | {language_of(n, cand_nodes) for n in cand_nodes})
    if args.language:
        languages = [l for l in languages if l in args.language]

    def edge_key(e):
        return (e["source_id"], e["target_id"], e["rel_type"])

    report = {"languages": {}, "gate": {"min_jaccard": args.min_jaccard, "failed": []}}
    for language in languages:
        ref_ids = {n for n in ref_nodes if language_of(n, ref_nodes) == language}
        cand_ids = {n for n in cand_nodes if language_of(n, cand_nodes) == language}
        ref_e = collections.Counter(edge_key(e) for e in ref_edges if language_of(e["source_id"], ref_nodes) == language)
        cand_e = collections.Counter(edge_key(e) for e in cand_edges if language_of(e["source_id"], cand_nodes) == language)
        by_type_ref = collections.Counter(
            (e["rel_type"], e["edge_class"]) for e in ref_edges if language_of(e["source_id"], ref_nodes) == language
        )
        by_type_cand = collections.Counter(
            (e["rel_type"], e["edge_class"]) for e in cand_edges if language_of(e["source_id"], cand_nodes) == language
        )
        common = ref_ids & cand_ids
        agreement = {}
        for column in COLUMNS:
            same = sum(1 for n in common if ref_nodes[n].get(column) == cand_nodes[n].get(column))
            agreement[column] = round(same / len(common), 4) if common else None
        node_j = jaccard(ref_ids, cand_ids)
        edge_j = jaccard(set(ref_e), set(cand_e))
        entry = {
            "nodes": {"reference": len(ref_ids), "candidate": len(cand_ids), "jaccard": round(node_j, 4)},
            "edges": {"reference": sum(ref_e.values()), "candidate": sum(cand_e.values()), "jaccard": round(edge_j, 4)},
            "edge_types": {
                f"{t}/{c}": [by_type_ref.get((t, c), 0), by_type_cand.get((t, c), 0)]
                for (t, c) in sorted(set(by_type_ref) | set(by_type_cand))
            },
            "column_agreement": agreement,
            "only_reference": sorted(ref_ids - cand_ids)[: args.examples],
            "only_candidate": sorted(cand_ids - ref_ids)[: args.examples],
            "edges_only_reference": [list(k) for k in sorted(set(ref_e) - set(cand_e))[: args.examples]],
            "edges_only_candidate": [list(k) for k in sorted(set(cand_e) - set(ref_e))[: args.examples]],
        }
        report["languages"][language] = entry
        if ref_ids and (node_j < args.min_jaccard or edge_j < args.min_jaccard):
            report["gate"]["failed"].append(language)

    for language, entry in report["languages"].items():
        print(f"== {language}: nodes {entry['nodes']}  edges {entry['edges']}")
        weak = {k: v for k, v in entry["column_agreement"].items() if v is not None and v < 1.0}
        if weak:
            print(f"   column agreement < 1: {weak}")
        for label in ("only_reference", "only_candidate", "edges_only_reference", "edges_only_candidate"):
            if entry[label]:
                print(f"   {label}: {entry[label]}")
        diffs = {k: v for k, v in entry["edge_types"].items() if v[0] != v[1]}
        if diffs:
            print(f"   edge types (ref, cand) that differ: {diffs}")
    if args.json:
        args.json.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    failed = report["gate"]["failed"]
    print(f"\ngate: {'FAIL ' + ', '.join(failed) if failed else 'PASS'} (min Jaccard {args.min_jaccard})")
    return 1 if failed and not args.report_only else 0


if __name__ == "__main__":
    sys.exit(main())
