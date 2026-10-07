#!/usr/bin/env python3
"""Score the Rust Phase 3 (vendored Leiden) against leidenalg.

    python parity/compare_phase3.py <dump-dir> <rust-out-dir> [--seeds 1,2,3,4,5]

``<dump-dir>`` is a ``python_phase3_dump.py`` dump, ``<rust-out-dir>`` the
output of ``deepwiki-cluster-parity leiden``. No implementation reproduces
leidenalg bit for bit, so the gate (ADR-0026 decision 6) is quality and
agreement, measured against leidenalg's own seed-to-seed spread:

Per level, on the SAME inputs (each recorded leidenalg call's graph):
    modularity of the leidenalg (seed 42) and the Rust membership, both by
    igraph's ``Graph.modularity`` with the call's weights and resolution —
    one shared implementation; ARI and NMI (scikit-learn) Rust vs leidenalg;
    the same for leidenalg seeds ``--seeds`` vs seed 42 (the noise floor).
    The page level pools every section's call: modularity is the mean
    weighted by vertex count, ARI/NMI are over the concatenated labels.

End to end: section and page counts of the Rust and Python Phase 3 and
their consolidation targets; ARI/NMI of the final section and page
assignments, Rust vs Python and Python seed s vs Python seed 42 (Python
Phase 3 re-run on the dump's graph with the other seed).

Gate: Rust modularity >= leidenalg - 0.01 at each level, and the Rust
counts within the targets. Exit status 1 when the gate fails.
"""

from __future__ import annotations

import functools
import json
import math
import sys
import time
from pathlib import Path

import igraph as ig
import leidenalg
import networkx as nx
from sklearn.metrics import adjusted_rand_score, normalized_mutual_info_score


def read_jsonl(path: Path) -> list:
    with open(path, encoding="utf-8") as handle:
        return [json.loads(line) for line in handle if line.strip()]


def igraph_of(call: dict) -> ig.Graph:
    graph = ig.Graph(n=len(call["nodes"]), directed=False)
    graph.add_edges([(u, v) for u, v, _ in call["edges"]])
    graph.es["weight"] = [w for _, _, w in call["edges"]]
    return graph


def modularity(graph: ig.Graph, membership: list, resolution: float) -> float:
    return graph.modularity(membership, weights="weight", resolution=resolution, directed=False)


def leidenalg_membership(graph: ig.Graph, resolution: float, seed: int) -> list:
    return list(leidenalg.find_partition(
        graph, leidenalg.RBConfigurationVertexPartition,
        resolution_parameter=resolution, weights="weight", seed=seed,
    ).membership)


def level_scores(calls: list, graphs: list, memberships: list) -> dict:
    """Pooled scores of ``memberships`` (one per call) vs the recorded ones."""
    total = sum(len(c["nodes"]) for c in calls) or 1
    mod_ref = sum(modularity(g, c["membership"], c["resolution"]) * len(c["nodes"]) for c, g in zip(calls, graphs)) / total
    mod_new = sum(modularity(g, m, c["resolution"]) * len(c["nodes"]) for c, g, m in zip(calls, graphs, memberships)) / total
    ref = [f"{i}:{x}" for i, c in enumerate(calls) for x in c["membership"]]
    new = [f"{i}:{x}" for i, m in enumerate(memberships) for x in m]
    worse = sum(
        1 for c, g, m in zip(calls, graphs, memberships)
        if modularity(g, m, c["resolution"]) < modularity(g, c["membership"], c["resolution"]) - 0.01
    )
    return {
        "calls": len(calls),
        "vertices": sum(len(c["nodes"]) for c in calls),
        "modularity_leidenalg": round(mod_ref, 6),
        "modularity_other": round(mod_new, 6),
        "delta": round(mod_new - mod_ref, 6),
        "ari": round(adjusted_rand_score(ref, new), 4),
        "nmi": round(normalized_mutual_info_score(ref, new), 4),
        "calls_worse_by_0.01": worse,
        "communities_leidenalg": sum(len(set(c["membership"])) for c in calls),
        "communities_other": sum(len(set(m)) for m in memberships),
    }


def python_phase3_with_seed(dump: Path, seed: int) -> dict:
    """Re-run the Python Phase 3 on the dump's graph with another Leiden seed."""
    from elitea_deepwiki.engine import graph_clustering as gc  # noqa: PLC0415

    graph = nx.MultiDiGraph()
    for node in read_jsonl(dump / "p3_nodes.jsonl"):
        attrs = {}
        if node["rel_path"]:
            attrs["rel_path"] = node["rel_path"]
        if node["file_name"]:
            attrs["file_name"] = node["file_name"]
        graph.add_node(node["id"], **attrs)
    for u, v, w in read_jsonl(dump / "p3_edges.jsonl"):
        graph.add_edge(u, v, weight=w)
    hubs = set(json.loads((dump / "p3_hubs.json").read_text())["phase3_hubs"])
    summary = json.loads((dump / "p3_summary.json").read_text())
    from elitea_deepwiki.engine.feature_flags import FeatureFlags  # noqa: PLC0415

    flags = FeatureFlags(
        exclude_tests=summary["flags"]["exclude_tests"],
        weight_calibration_profile=summary["flags"]["weight_calibration_profile"],
    )
    captured = {}

    class Db:
        class conn:  # noqa: N801 - mimics the attribute
            @staticmethod
            def execute(*a, **k):
                return None

            @staticmethod
            def commit():
                return None

        def set_clusters_batch(self, batch):
            captured["batch"] = list(batch)

        def set_hub(self, *a, **k):
            return None

        def set_meta(self, key, value):
            captured[key] = value

    original = gc.hierarchical_leiden_cluster
    gc.hierarchical_leiden_cluster = functools.partial(original, seed=seed)
    try:
        started = time.perf_counter()
        stats = gc.run_phase3(Db(), graph, hubs=hubs, feature_flags=flags)
        seconds = time.perf_counter() - started
    finally:
        gc.hierarchical_leiden_cluster = original
    rows = {nid: (macro, micro) for nid, macro, micro in captured["batch"]}
    return {"rows": rows, "stats": stats, "seconds": seconds}


def end_to_end(python_rows: dict, other_rows: dict) -> dict:
    ids = sorted(n for n in python_rows if python_rows[n][0] is not None)
    py_sec = [str(python_rows[n][0]) for n in ids]
    ot_sec = [str(other_rows.get(n, (None, None))[0]) for n in ids]
    py_pg = [f"{python_rows[n][0]}:{python_rows[n][1]}" for n in ids]
    ot_pg = [f"{other_rows.get(n, (None, None))[0]}:{other_rows.get(n, (None, None))[1]}" for n in ids]
    return {
        "section_ari": round(adjusted_rand_score(py_sec, ot_sec), 4),
        "section_nmi": round(normalized_mutual_info_score(py_sec, ot_sec), 4),
        "page_ari": round(adjusted_rand_score(py_pg, ot_pg), 4),
        "page_nmi": round(normalized_mutual_info_score(py_pg, ot_pg), 4),
    }


def target_sections(files: int) -> int:
    return 5 if files < 10 else max(5, min(20, math.ceil(1.2 * math.log2(max(10, files)))))


def target_pages(nodes: int) -> int:
    return max(8, min(200, math.ceil(math.sqrt(nodes / 7))))


def main() -> int:
    args = [a for a in sys.argv[1:] if not a.startswith("--seeds")]
    seeds = [1, 2, 3, 4, 5]
    for a in sys.argv[1:]:
        if a.startswith("--seeds="):
            seeds = [int(s) for s in a.split("=", 1)[1].split(",") if s]
    if len(args) != 2:
        print(__doc__, file=sys.stderr)
        return 2
    dump, rust = Path(args[0]), Path(args[1])
    calls = read_jsonl(dump / "p3_leiden_calls.jsonl")
    rust_calls = read_jsonl(rust / "rust_calls.jsonl")
    assert len(calls) == len(rust_calls)
    graphs = [igraph_of(c) for c in calls]

    report: dict = {"dump": str(dump), "levels": {}}
    gate = True
    for level in ("section", "page"):
        idx = [i for i, c in enumerate(calls) if c["level"] == level]
        if not idx:
            continue
        lc = [calls[i] for i in idx]
        lg = [graphs[i] for i in idx]
        rust_scores = level_scores(lc, lg, [rust_calls[i]["membership"] for i in idx])
        floor = []
        for seed in seeds:
            other = [leidenalg_membership(g, c["resolution"], seed) for c, g in zip(lc, lg)]
            floor.append(level_scores(lc, lg, other))
        report["levels"][level] = {
            "rust_vs_leidenalg": rust_scores,
            "leidenalg_seed_spread": {
                "seeds": seeds,
                "ari": [f["ari"] for f in floor],
                "nmi": [f["nmi"] for f in floor],
                "modularity": [f["modularity_other"] for f in floor],
            },
        }
        if rust_scores["delta"] < -0.01:
            gate = False

    py_summary = json.loads((dump / "p3_summary.json").read_text())["phase3_stats"]
    rust_summary = json.loads((rust / "rust_summary.json").read_text())
    rs = rust_summary["phase3_stats"]
    files = py_summary["algorithm_metadata"].get("file_nodes") or 0
    counts = {
        "python": {"sections": py_summary["macro"]["cluster_count"], "pages": py_summary["micro"]["total_pages"],
                   "sections_raw": py_summary["macro"]["cluster_count_raw"], "pages_raw": py_summary["micro"]["total_pages_raw"]},
        "rust": {"sections": rs["macro"]["cluster_count"], "pages": rs["micro"]["total_pages"],
                 "sections_raw": rs["macro"]["cluster_count_raw"], "pages_raw": rs["micro"]["total_pages_raw"]},
        "targets": {"sections": target_sections(files), "pages": target_pages(py_summary["macro"]["nodes_assigned"])},
    }
    if counts["rust"]["sections"] > counts["targets"]["sections"] or counts["rust"]["pages"] > counts["targets"]["pages"]:
        gate = False
    report["counts"] = counts

    python_rows = {r["node_id"]: (r["macro_cluster"], r["micro_cluster"]) for r in read_jsonl(dump / "p3_assignments.jsonl")}
    rust_rows = {r["node_id"]: (r["macro_cluster"], r["micro_cluster"]) for r in read_jsonl(rust / "rust_assignments.jsonl")}
    report["end_to_end"] = {"rust_vs_python": end_to_end(python_rows, rust_rows), "python_seed_spread": []}
    for seed in seeds:
        other = python_phase3_with_seed(dump, seed)
        scores = end_to_end(python_rows, other["rows"])
        scores["seed"] = seed
        scores["sections"] = other["stats"]["macro"]["cluster_count"]
        scores["pages"] = other["stats"]["micro"]["total_pages"]
        scores["python_seconds_no_db"] = round(other["seconds"], 4)
        report["end_to_end"]["python_seed_spread"].append(scores)
    report["timing"] = {
        # With the SQLite writes of persist_clusters.
        "python_phase3_seconds": json.loads((dump / "p3_summary.json").read_text()).get("phase3_seconds"),
        # Without them (a stub DB), median over the re-run seeds.
        "python_phase3_seconds_no_db": sorted(
            s["python_seconds_no_db"] for s in report["end_to_end"]["python_seed_spread"]
        )[len(seeds) // 2] if seeds else None,
        "rust_phase3_seconds_median": rust_summary.get("phase3_seconds_median"),
    }
    report["gate"] = "pass" if gate else "FAIL"
    print(json.dumps(report, indent=2))
    return 0 if gate else 1


if __name__ == "__main__":
    raise SystemExit(main())
