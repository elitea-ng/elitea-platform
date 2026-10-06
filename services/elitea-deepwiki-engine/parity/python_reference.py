#!/usr/bin/env python3
"""Dump the Python engine's code graph for a repository, reproducibly.

The reference side of the ADR-0026 graph gate. It runs the Python engine's
own Phase 1 (``EnhancedUnifiedGraphBuilder.analyze_repository``) and Phase 1c
(API surfaces, contract nodes, cross-language links, test links, markdown
structure) exactly as ``filesystem_indexer`` does, writes the graph with the
engine's own ``UnifiedWikiDB.from_networkx`` — so the rows are what the index
stores — and dumps ``repo_nodes`` and ``repo_edges`` as sorted JSON lines.

The shipped Python engine is NOT reproducible: parsers collect results with
``as_completed`` and fill shared registries from worker threads, so the
graph's insertion order (and with it the node-id collision suffixes) changes
from run to run. For the reference only, this script makes it reproducible:

* every ``ThreadPoolExecutor`` runs its work inline, in submission order;
* ``concurrent.futures.as_completed`` yields in submission order;
* file discovery is sorted.

The engine copy itself is not modified.

    python parity/python_reference.py <repo> <out-dir> [--no-phase1c]

``--no-phase1c`` stops after Phase 1 (the builder's own graph: parsers,
documents, contraction, SQL, ORM), for checking the graph builder port
before the Phase 1c passes are ported.

Needs the ``engine`` extra (``pip install -e 'services/elitea-deepwiki[engine]'``).
No model and no network: embeddings, Phase 2 and Phase 3 are not run.
"""

from __future__ import annotations

import concurrent.futures
import json
import logging
import os
import sqlite3
import sys
import tempfile
import time
from pathlib import Path


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


def _make_reproducible() -> None:
    concurrent.futures.ThreadPoolExecutor = InlineExecutor  # type: ignore[misc]
    concurrent.futures.ProcessPoolExecutor = InlineExecutor  # type: ignore[misc]
    concurrent.futures.as_completed = lambda fs, timeout=None: list(fs)  # type: ignore[assignment]


def main() -> int:
    argv = [a for a in sys.argv[1:] if a != "--no-phase1c"]
    phase1c = "--no-phase1c" not in sys.argv
    if len(argv) != 2:
        print(__doc__, file=sys.stderr)
        return 2
    repo = Path(argv[0]).resolve()
    out = Path(argv[1]).resolve()
    out.mkdir(parents=True, exist_ok=True)
    logging.basicConfig(level=os.environ.get("LOGLEVEL", "WARNING"))

    _make_reproducible()
    # Import after the patch: modules that did `from concurrent.futures
    # import ThreadPoolExecutor` bind the inline executor.
    from elitea_deepwiki.engine.code_graph import graph_builder as gb  # noqa: PLC0415
    from elitea_deepwiki.engine.feature_flags import get_feature_flags  # noqa: PLC0415
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

    started = time.time()
    builder = gb.EnhancedUnifiedGraphBuilder(max_workers=1)
    analysis = builder.analyze_repository(str(repo), stream_documents=True)
    graph = analysis.unified_graph
    parse_seconds = time.time() - started

    # Phase 1c, as filesystem_indexer._write_unified_db runs it.
    flags = get_feature_flags()
    surfaces = {}
    if not phase1c:
        for name in ("api_surface_extraction", "cross_language_linking", "test_linker", "markdown_structure"):
            object.__setattr__(flags, name, False) if hasattr(flags, name) else None
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

    with tempfile.TemporaryDirectory() as scratch:
        db_path = os.path.join(scratch, "reference.wiki.db")
        with UnifiedWikiDB(db_path) as udb:
            udb.from_networkx(graph)
        connection = sqlite3.connect(db_path)
        connection.row_factory = sqlite3.Row
        node_rows = [dict(r) for r in connection.execute("SELECT * FROM repo_nodes ORDER BY node_id")]
        edge_rows = [
            dict(r)
            for r in connection.execute(
                "SELECT * FROM repo_edges ORDER BY source_id, target_id, rel_type, edge_class, id"
            )
        ]
        connection.close()

    for row in edge_rows:
        row.pop("id", None)
    for row in node_rows:
        row.pop("indexed_at", None)

    def write(name: str, rows: list[dict]) -> None:
        with open(out / name, "w", encoding="utf-8") as handle:
            for row in rows:
                handle.write(json.dumps(row, sort_keys=True, ensure_ascii=False) + "\n")

    write("nodes.jsonl", node_rows)
    write("edges.jsonl", edge_rows)
    summary = {
        "repo": str(repo),
        "nodes": len(node_rows),
        "edges": len(edge_rows),
        "parse_seconds": round(parse_seconds, 2),
        "flags": {
            "api_surface_extraction": flags.api_surface_extraction,
            "cross_language_linking": flags.cross_language_linking,
            "test_linker": flags.test_linker,
            "markdown_structure": flags.markdown_structure,
        },
    }
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(summary))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
