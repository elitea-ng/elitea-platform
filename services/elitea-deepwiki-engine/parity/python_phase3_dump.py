#!/usr/bin/env python3
"""Dump the Python engine's Phase 3 clustering for a repository, reproducibly.

The reference side of the ADR-0026 clustering gate (phase 4 of the port).
It runs the Python engine's Phase 1 and Phase 1c as ``python_reference.py``
does, writes the unified DB, runs Phase 2 (``graph_topology.run_phase2``)
and then Phase 3 (``graph_clustering.run_phase3``) exactly as
``filesystem_indexer._write_unified_db`` calls them, and writes:

``p3_nodes.jsonl``
    The Phase 2 output graph's nodes, in graph order: ``id``, ``rel_path``,
    ``file_name``, ``is_doc`` (the index column) and ``pred`` — the node's
    distinct predecessors in networkx ``_pred`` order. Phase 3 reads the
    in-edges of a hub in that order, and the order is the order the edges
    were FIRST added, which the edge list alone cannot restore.
``p3_edges.jsonl``
    ``[source, target, weight]`` per edge, in ``G.edges(keys=True)`` order.
``p3_hubs.json``
    The hub ids Phase 3 receives (``filesystem_indexer`` passes
    ``phase2_stats["hubs"]["node_ids"]``, which ``run_phase2`` caps at the
    first 20 sorted ids) and the Phase 2 hub count.
``p3_leiden_calls.jsonl``
    Every ``leidenalg.find_partition`` call: level (``section`` / ``page``),
    the vertex names in igraph order, the weighted edges, resolution, seed,
    the membership leidenalg returned and its quality. The Rust replay
    partitioner answers from these, so the Rust non-Leiden logic can be
    compared exactly.
``p3_assignments.jsonl``
    The final rows per graph node, sorted by id: ``macro_cluster``,
    ``micro_cluster``, ``is_hub``, ``hub_assignment`` — what
    ``persist_clusters`` writes (checked against the DB).
``p3_summary.json``
    Counts, flags, the Phase 3 stats and the Phase 3 wall time.

Phase 2 needs embeddings. A model is not reproducible and not available
here, so a stand-in is used: SHA-256 of the UTF-8 text, the 32 bytes
mapped to ``(b - 127.5) / 127.5`` and L2-normalised (32 dimensions). The
same function serves documents and queries.

    PYTHONHASHSEED=0 python parity/python_phase3_dump.py <repo> <out-dir>

``PYTHONHASHSEED=0``: networkx subgraph views iterate a Python ``set`` when
the subgraph is small, so string hashing reaches the order in which the
page pass sums parallel edge weights and lists edges (not the result).

``python_phase3_fixture.py`` reuses the recording and writing helpers on a
synthetic graph.
"""

from __future__ import annotations

import concurrent.futures
import hashlib
import json
import logging
import math
import os
import sys
import tempfile
import time
from pathlib import Path

EMBEDDING_DIM = 32


class _InlineFuture(concurrent.futures.Future):
    pass


class InlineExecutor(concurrent.futures.Executor):
    """Runs each submitted call immediately, on the calling thread."""

    def __init__(self, *args, **kwargs) -> None:  # noqa: D107 - same signature
        pass

    def submit(self, fn, /, *args, **kwargs):
        future = _InlineFuture()
        try:
            future.set_result(fn(*args, **kwargs))
        except BaseException as exc:  # noqa: BLE001 - recorded on the future
            future.set_exception(exc)
        return future

    def map(self, fn, *iterables, timeout=None, chunksize=1):
        return [fn(*args) for args in zip(*iterables)]

    def shutdown(self, wait=True, *, cancel_futures=False) -> None:
        return None


def stand_in_embedding(text: str) -> list[float]:
    """The deterministic stand-in embedding (see the module docstring)."""
    digest = hashlib.sha256(text.encode("utf-8")).digest()
    vector = [(b - 127.5) / 127.5 for b in digest[:EMBEDDING_DIM]]
    norm = math.sqrt(sum(x * x for x in vector)) or 1.0
    return [x / norm for x in vector]


def install_recorders(gc) -> tuple[list[dict], dict]:
    """Wrap ``graph_clustering`` so every Leiden call and the final
    assignments are recorded. Returns ``(calls, captured)``."""
    calls: list[dict] = []
    captured: dict = {}
    names_by_graph: dict[int, list[str]] = {}
    level = {"now": "section"}
    original_to_igraph = gc._nx_to_igraph
    original_contract = gc._contract_to_file_graph
    original_to_undirected = gc._to_weighted_undirected
    original_find_partition = gc.leidenalg.find_partition
    original_persist = gc.persist_clusters

    def marking_contract(g):
        level["now"] = "section"
        return original_contract(g)

    def marking_to_undirected(g):
        level["now"] = "page"
        return original_to_undirected(g)

    def recording_to_igraph(g):
        ig_graph, node_list = original_to_igraph(g)
        names_by_graph[id(ig_graph)] = list(node_list)
        return ig_graph, node_list

    def recording_find_partition(ig_graph, partition_type, **kwargs):
        partition = original_find_partition(ig_graph, partition_type, **kwargs)
        names = names_by_graph.pop(id(ig_graph))
        weights = ig_graph.es["weight"] if ig_graph.ecount() else []
        calls.append({
            "level": level["now"],
            "partition_type": partition_type.__name__,
            "resolution": kwargs.get("resolution_parameter"),
            "seed": kwargs.get("seed"),
            "nodes": names,
            "edges": [[e.source, e.target, w] for e, w in zip(ig_graph.es, weights)],
            "membership": list(partition.membership),
            "quality": partition.quality(),
        })
        return partition

    def capturing_persist(db, macro_assignments, micro_assignments, hub_assignments):
        captured["macro"] = dict(macro_assignments)
        captured["micro"] = {k: dict(v) for k, v in micro_assignments.items()}
        captured["hubs"] = dict(hub_assignments)
        return original_persist(db, macro_assignments, micro_assignments, hub_assignments)

    gc._nx_to_igraph = recording_to_igraph
    gc._contract_to_file_graph = marking_contract
    gc._to_weighted_undirected = marking_to_undirected
    gc.leidenalg.find_partition = recording_find_partition
    gc.persist_clusters = capturing_persist
    return calls, captured


def graph_rows(graph, is_doc: dict) -> tuple[list[dict], list[list]]:
    """The node and edge lines of the Phase 3 input graph."""
    node_lines = []
    for nid, data in graph.nodes(data=True):
        for key in ("rel_path", "file_name"):
            value = data.get(key)
            if value is not None and not isinstance(value, str):
                raise SystemExit(f"{nid}: {key} is {type(value).__name__}")
        if "rel_path" in data and data["rel_path"] is None:
            # `_run_phase3` counts files with `.get("rel_path", "")`, which
            # keeps a None; the Rust graph cannot carry one.
            raise SystemExit(f"{nid}: rel_path is None")
        node_lines.append({
            "id": nid,
            "rel_path": data.get("rel_path") or "",
            "file_name": data.get("file_name") or "",
            "is_doc": bool(is_doc.get(nid, 0)),
            "pred": list(graph._pred[nid]),
        })
    edge_lines = []
    for u, v, _key, data in graph.edges(keys=True, data=True):
        weight = data.get("weight", 1.0)
        if not isinstance(weight, (int, float)) or isinstance(weight, bool):
            raise SystemExit(f"{u}->{v}: weight is {type(weight).__name__}")
        edge_lines.append([u, v, float(weight)])
    return node_lines, edge_lines


def assignment_rows(graph, captured: dict, db_rows: dict | None = None) -> list[dict]:
    """The persisted cluster columns per node, sorted by id; checked against
    the DB rows (``node_id → (macro, micro, is_hub, hub_assignment)``) when
    given."""
    macro = captured["macro"]
    micro = captured["micro"]
    hubs = captured["hubs"]
    rows = []
    for nid in sorted(graph.nodes()):
        macro_id = macro.get(nid)
        micro_id = micro.get(macro_id, {}).get(nid) if macro_id is not None else None
        row = {
            "node_id": nid,
            "macro_cluster": macro_id,
            "micro_cluster": micro_id,
            "is_hub": nid in hubs,
            "hub_assignment": str(hubs[nid][0]) if nid in hubs else None,
        }
        if db_rows is not None and nid in db_rows:
            expected = (macro_id, micro_id, 1 if nid in hubs else 0, row["hub_assignment"])
            if tuple(db_rows[nid]) != expected:
                raise SystemExit(f"{nid}: DB row {db_rows[nid]} != in-memory {expected}")
        rows.append(row)
    return rows


def write_dump(out: Path, *, nodes, edges, calls, assignments, hubs: dict, summary: dict) -> None:
    """Write the dump files (see the module docstring)."""
    out.mkdir(parents=True, exist_ok=True)

    def write_jsonl(name: str, rows: list) -> None:
        with open(out / name, "w", encoding="utf-8") as handle:
            for row in rows:
                handle.write(json.dumps(row, ensure_ascii=False) + "\n")

    write_jsonl("p3_nodes.jsonl", nodes)
    write_jsonl("p3_edges.jsonl", edges)
    write_jsonl("p3_leiden_calls.jsonl", calls)
    write_jsonl("p3_assignments.jsonl", assignments)
    (out / "p3_hubs.json").write_text(json.dumps(hubs, indent=2) + "\n", encoding="utf-8")
    (out / "p3_summary.json").write_text(json.dumps(summary, indent=2, default=str) + "\n", encoding="utf-8")


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    repo = Path(sys.argv[1]).resolve()
    out = Path(sys.argv[2]).resolve()
    logging.basicConfig(level=os.environ.get("LOGLEVEL", "WARNING"))

    concurrent.futures.ThreadPoolExecutor = InlineExecutor  # type: ignore[misc]
    concurrent.futures.ProcessPoolExecutor = InlineExecutor  # type: ignore[misc]
    concurrent.futures.as_completed = lambda fs, timeout=None: list(fs)  # type: ignore[assignment]

    from elitea_deepwiki.engine import graph_clustering as gc  # noqa: PLC0415
    from elitea_deepwiki.engine.code_graph import graph_builder as gb  # noqa: PLC0415
    from elitea_deepwiki.engine.feature_flags import get_feature_flags  # noqa: PLC0415
    from elitea_deepwiki.engine.graph_topology import run_phase2  # noqa: PLC0415
    from elitea_deepwiki.engine.unified_db import UnifiedWikiDB  # noqa: PLC0415

    for module in list(sys.modules.values()):
        name = getattr(module, "__name__", "")
        if name.startswith(("elitea_deepwiki", "plugin_implementation")):
            if hasattr(module, "ThreadPoolExecutor"):
                module.ThreadPoolExecutor = InlineExecutor
            if hasattr(module, "as_completed"):
                module.as_completed = concurrent.futures.as_completed

    original_discover = gb.EnhancedUnifiedGraphBuilder._discover_files_by_language

    def sorted_discover(self, *args, **kwargs):
        found = original_discover(self, *args, **kwargs)
        return {language: sorted(paths) for language, paths in sorted(found.items())}

    gb.EnhancedUnifiedGraphBuilder._discover_files_by_language = sorted_discover

    # ── Phase 1 + 1c, as python_reference.py ───────────────────────────
    builder = gb.EnhancedUnifiedGraphBuilder(max_workers=1)
    analysis = builder.analyze_repository(str(repo), stream_documents=True)
    graph = analysis.unified_graph
    flags = get_feature_flags()
    surfaces = {}
    if flags.api_surface_extraction:
        from elitea_deepwiki.engine.code_graph.api_surface_extractor import (  # noqa: PLC0415
            extract_api_surfaces_for_graph,
            materialize_contract_nodes,
        )

        surfaces = extract_api_surfaces_for_graph(graph, repo_root=str(repo))
        if surfaces:
            materialize_contract_nodes(graph, surfaces)
    if flags.cross_language_linking:
        from elitea_deepwiki.engine.code_graph.cross_language_linker import (  # noqa: PLC0415
            run_cross_language_linker,
        )

        for src, tgt, attrs in run_cross_language_linker(
            graph,
            cross_language_relationships=list(getattr(analysis, "cross_language_relationships", []) or []) or None,
            surfaces_by_node=surfaces or None,
            flags=flags,
        ):
            if graph.has_node(src) and graph.has_node(tgt):
                graph.add_edge(src, tgt, **attrs)
    if flags.test_linker:
        from elitea_deepwiki.engine.code_graph.test_linker import run_test_linker  # noqa: PLC0415

        for src, tgt, attrs in run_test_linker(graph, flags=flags):
            if graph.has_node(src) and graph.has_node(tgt):
                graph.add_edge(src, tgt, **attrs)
    if flags.markdown_structure:
        from elitea_deepwiki.engine.markdown_structure import wire_markdown_structure  # noqa: PLC0415

        wire_markdown_structure(graph, flags=flags)

    calls, captured = install_recorders(gc)

    with tempfile.TemporaryDirectory() as scratch:
        db_path = os.path.join(scratch, "reference.wiki.db")
        with UnifiedWikiDB(db_path, embedding_dim=EMBEDDING_DIM) as udb:
            udb.from_networkx(graph)
            udb.populate_embeddings(
                embedding_fn=lambda texts: [stand_in_embedding(t) for t in texts],
                batch_size=64,
            )
            phase2_stats = run_phase2(db=udb, G=graph, embedding_fn=stand_in_embedding)
            # As filesystem_indexer: the (capped) id list from the stats.
            phase2_hubs = set(phase2_stats.get("hubs", {}).get("node_ids", []))

            # Phase 2 output, before Phase 3 touches anything.
            is_doc = dict(udb.conn.execute("SELECT node_id, is_doc FROM repo_nodes").fetchall())
            node_lines, edge_lines = graph_rows(graph, is_doc)

            started = time.perf_counter()
            phase3_stats = gc.run_phase3(db=udb, G=graph, hubs=phase2_hubs)
            phase3_seconds = time.perf_counter() - started

            db_rows = {
                row[0]: row[1:]
                for row in udb.conn.execute(
                    "SELECT node_id, macro_cluster, micro_cluster, is_hub, hub_assignment FROM repo_nodes"
                )
            }

    summary = {
        "repo": str(repo),
        "nodes": len(node_lines),
        "edges": len(edge_lines),
        "leiden_calls": len(calls),
        "phase3_seconds": round(phase3_seconds, 4),
        "phase3_stats": phase3_stats,
        "flags": {
            "exclude_tests": flags.exclude_tests,
            "weight_calibration_profile": flags.weight_calibration_profile,
        },
    }
    write_dump(
        out,
        nodes=node_lines,
        edges=edge_lines,
        calls=calls,
        assignments=assignment_rows(graph, captured, db_rows),
        hubs={
            "phase3_hubs": sorted(phase2_hubs),
            "phase2_hub_count": phase2_stats.get("hubs", {}).get("count"),
        },
        summary=summary,
    )
    print(json.dumps({k: summary[k] for k in ("repo", "nodes", "edges", "leiden_calls", "phase3_seconds")}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
