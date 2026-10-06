#!/usr/bin/env python3
"""Dump the Python engine's wiki STRUCTURE for a repository, reproducibly.

The reference side of the ADR-0026 structure gate (phase 5a of the port).
It runs, in one process:

1. Phase 1 + 1c and Phase 2 as ``python_reference.py --through phase2``
   does (stand-in embeddings, every index read recorded);
2. Phase 3 as ``python_phase3_dump.py`` does (every leidenalg call
   recorded);
3. the first two nodes of ``generate_wiki``'s agent graph:
   ``OptimizedWikiGenerationAgent.analyze_repository`` (the repository
   analysis call) and ``generate_wiki_structure`` (the planner the
   ``planner_type`` names), against the deterministic LLM stub
   ``services/elitea-deepwiki/e2e/llm_stub.py``, which this script starts
   on a loopback port and which records every chat request body.

The LLM is the one the subprocess worker builds
(``wiki_subprocess_worker._build_llm_and_embeddings``: ``ChatOpenAI``,
streaming, ``max_tokens`` 64000, temperature 0.1); the indexer is a stand-in
with what the agent reads of ``FilesystemRepositoryIndexer``: the documents
the graph builder streamed, the repository root and a default
``FilterManager``.

Written to ``<out-dir>``:

``requests.jsonl``   every chat request body, in order (the stub's record)
``structure.json``   ``WikiStructureSpec.model_dump()``, ``json.dumps(indent=2)``
``analysis.json``    the analysis node's state (context, tree, README)
``recording.jsonl``  Phase 2's index reads (``deepwiki-parity`` replays them)
``stats.json``       Phase 2's stats dict
``p3_leiden_calls.jsonl``, ``p3_assignments.jsonl``  Phase 3 (as the Phase 3 dump)
``central_calls.jsonl``  every ``select_central_symbols`` call: input order, k, result
``cluster_map.json`` the planner's architectural cluster map, in row order
``summary.json``     counts and wall times

Reproducibility patches, beyond the earlier dumps' (inline thread pools,
sorted discovery, the two graph-order set iterations, sorted components):

* ``os.walk`` lists each directory sorted. The agent's file list
  (``_get_repository_file_paths``) is an ``os.walk`` in file-system order;
  its order reaches the file-type statistics (ties of the count sort).
* After Phase 3, the planner's ``set`` objects keep insertion order
  (``OrderedSet``), and networkx subgraph views keep the order of the
  node list they were given (``show_nodes``). The planner returns
  ``list(set(...))`` for small pages and ranks PageRank ties by the
  subgraph's node order, which a Python ``set`` makes follow the hash
  seed. The Rust engine uses the insertion order.
  ``STRUCTURE_DUMP_HASH_ORDER=1`` leaves the sets alone, to measure what
  the pin changes.

    PYTHONHASHSEED=0 PYTHONPATH=services/elitea-deepwiki/src \\
        python parity/python_structure_dump.py <repo> <out-dir> \\
        [--planner cluster|auto|deepagents] [--repo-name owner/name] [--branch main] \\
        [--repo-identifier owner/name:main:0123abcd]
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import sys
import tempfile
import threading
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import python_phase3_dump as p3  # noqa: E402
import python_reference as ref  # noqa: E402

STUB = HERE.parent.parent / "elitea-deepwiki" / "e2e" / "llm_stub.py"


class OrderedSet:
    """A ``set`` that iterates in insertion order (see the module docs).

    ``a & b`` and ``a - b`` keep ``a``'s order; ``a | b`` is ``a`` then the
    new members of ``b``.
    """

    def __init__(self, items=()):
        self._items = dict.fromkeys(items)

    def __iter__(self):
        return iter(self._items)

    def __len__(self):
        return len(self._items)

    def __contains__(self, item):
        return item in self._items

    def __bool__(self):
        return bool(self._items)

    def __repr__(self):
        return f"OrderedSet({list(self._items)!r})"

    def add(self, item):
        self._items[item] = None

    def discard(self, item):
        self._items.pop(item, None)

    def update(self, *iterables):
        for iterable in iterables:
            for item in iterable:
                self._items[item] = None

    def __and__(self, other):
        return OrderedSet(x for x in self if x in other)

    def __sub__(self, other):
        return OrderedSet(x for x in self if x not in other)

    def __or__(self, other):
        result = OrderedSet(self)
        result.update(other)
        return result


def sorted_walk(original):
    def walk(top, topdown=True, onerror=None, followlinks=False):
        for root, dirs, files in original(top, topdown=topdown, onerror=onerror, followlinks=followlinks):
            dirs.sort()
            files.sort()
            yield root, dirs, files

    return walk


def start_stub(record: Path) -> str:
    """Serve ``llm_stub.py`` on a free loopback port; return its base URL."""
    import importlib.util  # noqa: PLC0415

    os.environ["LLM_STUB_RECORD"] = str(record)
    record.write_text("", encoding="utf-8")
    spec = importlib.util.spec_from_file_location("llm_stub", STUB)
    stub = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(stub)
    server = stub.ThreadingHTTPServer(("127.0.0.1", 0), stub.Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return f"http://127.0.0.1:{server.server_address[1]}/v1"


class StandInIndexer:
    """What ``OptimizedWikiGenerationAgent`` reads of the filesystem indexer."""

    def __init__(self, documents, repo_root: str):
        from elitea_deepwiki.engine.filter_manager import FilterManager  # noqa: PLC0415

        self.all_documents = documents
        self.last_index_stats = {}
        self.filter_manager = FilterManager()
        self._repo_root = repo_root

    def get_all_documents(self):
        return self.all_documents

    def get_repo_root(self):
        return self._repo_root


def build_graph(repo: Path):
    """Phase 1 + 1c (``python_reference.main``'s steps) and the streamed documents."""
    from elitea_deepwiki.engine.code_graph import graph_builder as gb  # noqa: PLC0415
    from elitea_deepwiki.engine.feature_flags import get_feature_flags  # noqa: PLC0415

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
    # The indexer materialises the stream after the build (``_build_filesystem_index``).
    documents = list(analysis.documents_iter) if analysis.documents_iter is not None else list(analysis.documents)
    for doc in documents:
        if "source" not in doc.metadata:
            doc.metadata["source"] = doc.metadata.get("rel_path") or doc.metadata.get("file_path")
    return graph, documents


def install_planner_patches(out: Path):
    """The post-Phase-3 order patches and the central-symbol recorder."""
    import networkx as nx  # noqa: PLC0415
    from elitea_deepwiki.engine.wiki_structure_planner import cluster_planner as cp  # noqa: PLC0415

    def ordered_show_nodes(self, nodes):
        self.nodes = dict.fromkeys(nodes)

    if os.environ.get("STRUCTURE_DUMP_HASH_ORDER") != "1":
        nx.filters.show_nodes.__init__ = ordered_show_nodes
        cp.set = OrderedSet

    calls = []
    original = cp.select_central_symbols

    def recording_select(G, cluster_nodes, k=5):
        result = original(G, cluster_nodes, k=k)
        calls.append({"nodes": list(cluster_nodes), "k": k, "result": list(result)})
        return result

    cp.select_central_symbols = recording_select

    captured = {}
    original_load = cp.ClusterStructurePlanner._load_architectural_cluster_map

    def recording_load(self):
        result = original_load(self)
        captured["cluster_map"] = [
            [macro, [[micro, list(nids)] for micro, nids in micros.items()]]
            for macro, micros in result.items()
        ]
        captured["query_plan"] = [
            list(row)
            for row in self.db.conn.execute(
                "EXPLAIN QUERY PLAN SELECT node_id, macro_cluster, micro_cluster FROM repo_nodes "
                "WHERE macro_cluster IS NOT NULL AND is_architectural = 1" + self._test_sql
            )
        ]
        return result

    cp.ClusterStructurePlanner._load_architectural_cluster_map = recording_load
    return calls, captured


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("repo")
    parser.add_argument("out")
    parser.add_argument("--planner", default="cluster")
    parser.add_argument("--repo-name", default=None)
    parser.add_argument("--branch", default="main")
    parser.add_argument("--repo-identifier", default=None)
    args = parser.parse_args()
    repo = Path(args.repo).resolve()
    out = Path(args.out).resolve()
    out.mkdir(parents=True, exist_ok=True)
    repo_name = args.repo_name or f"acme/{repo.name}"
    repo_identifier = args.repo_identifier or f"{repo_name}:{args.branch}:0123abcd"
    logging.basicConfig(level=os.environ.get("LOGLEVEL", "WARNING"))

    ref._make_reproducible()
    ref._make_node_order_reproducible()
    os.walk = sorted_walk(os.walk)
    from elitea_deepwiki.engine.code_graph import graph_builder as gb  # noqa: PLC0415

    for module in list(sys.modules.values()):
        name = getattr(module, "__name__", "")
        if name.startswith(("elitea_deepwiki", "plugin_implementation")):
            if hasattr(module, "ThreadPoolExecutor"):
                module.ThreadPoolExecutor = ref.InlineExecutor
            if hasattr(module, "as_completed"):
                module.as_completed = ref.concurrent.futures.as_completed
    original_discover = gb.EnhancedUnifiedGraphBuilder._discover_files_by_language

    def sorted_discover(self, *a, **kw):
        found = original_discover(self, *a, **kw)
        return {language: sorted(paths) for language, paths in sorted(found.items())}

    gb.EnhancedUnifiedGraphBuilder._discover_files_by_language = sorted_discover

    timings = {}
    started = time.perf_counter()
    graph, documents = build_graph(repo)
    timings["phase1"] = time.perf_counter() - started

    import networkx as nx  # noqa: PLC0415
    from elitea_deepwiki.engine import graph_clustering as gc  # noqa: PLC0415
    from elitea_deepwiki.engine import graph_topology  # noqa: PLC0415
    from elitea_deepwiki.engine.unified_db import UnifiedWikiDB  # noqa: PLC0415

    nx.weakly_connected_components = ref._sorted_components(nx.weakly_connected_components)
    leiden_calls, captured = p3.install_recorders(gc)

    base_url = start_stub(out / "requests.jsonl")
    with tempfile.TemporaryDirectory() as scratch:
        db_path = os.path.join(scratch, "reference.wiki.db")
        with UnifiedWikiDB(db_path, embedding_dim=ref.STANDIN_DIM) as udb:
            if not udb.vec_available:
                print("sqlite-vec is not loadable", file=sys.stderr)
                return 1
            udb.from_networkx(graph)
            udb.populate_embeddings(embedding_fn=ref.standin_batch, batch_size=64)
            recorder = ref.RecordingDB(udb)
            started = time.perf_counter()
            stats = graph_topology.run_phase2(recorder, graph, embedding_fn=ref.standin_embedding)
            timings["phase2"] = time.perf_counter() - started
            hubs = set(stats.get("hubs", {}).get("node_ids", []))
            started = time.perf_counter()
            gc.run_phase3(db=udb, G=graph, hubs=hubs)
            timings["phase3"] = time.perf_counter() - started
            udb.set_meta("repo_identifier", repo_identifier)
            db_rows = {
                row[0]: row[1:]
                for row in udb.conn.execute(
                    "SELECT node_id, macro_cluster, micro_cluster, is_hub, hub_assignment FROM repo_nodes"
                )
            }
        (out / "stats.json").write_text(json.dumps(stats) + "\n", encoding="utf-8")
        with open(out / "recording.jsonl", "w", encoding="utf-8") as handle:
            for entry in recorder.calls.values():
                handle.write(json.dumps(entry, ensure_ascii=False) + "\n")
        with open(out / "p3_leiden_calls.jsonl", "w", encoding="utf-8") as handle:
            for call in leiden_calls:
                handle.write(json.dumps(call, ensure_ascii=False) + "\n")
        with open(out / "p3_assignments.jsonl", "w", encoding="utf-8") as handle:
            for row in p3.assignment_rows(graph, captured, db_rows):
                handle.write(json.dumps(row, ensure_ascii=False) + "\n")

        # ── generate_wiki: analyze_repository → generate_wiki_structure ──
        central_calls, planner_capture = install_planner_patches(out)
        from elitea_deepwiki.engine.agents.wiki_graph_optimized import OptimizedWikiGenerationAgent  # noqa: PLC0415
        from elitea_deepwiki.engine.state.wiki_state import TargetAudience, WikiStyle  # noqa: PLC0415
        from elitea_deepwiki.engine.wiki_subprocess_worker import _build_llm_and_embeddings  # noqa: PLC0415

        llm, _embeddings, _name = _build_llm_and_embeddings(
            {"api_base": base_url, "api_key": "stub-key", "model_name": "gpt-4o"}, "text-embedding-3-small",
        )
        agent = OptimizedWikiGenerationAgent(
            indexer=StandInIndexer(documents, str(repo)),
            retriever_stack=object(),
            llm=llm,
            repository_url=repo_name,
            branch=args.branch,
            wiki_style=WikiStyle.COMPREHENSIVE,
            target_audience=TargetAudience.MIXED,
            require_diagrams=True,
            enable_progress_tracking=False,
            enable_quality_enhancement=True,
        )
        agent._find_unified_db = lambda: db_path
        config = {"configurable": {"planner_type": args.planner}}
        started = time.perf_counter()
        state = agent.analyze_repository({}, config)
        timings["analysis"] = time.perf_counter() - started
        if state.get("errors"):
            print(f"analysis failed: {state['errors']}", file=sys.stderr)
            return 1
        started = time.perf_counter()
        result = agent.generate_wiki_structure(dict(state), config)
        timings["structure"] = time.perf_counter() - started
        if result.get("errors"):
            print(f"structure failed: {result['errors']}", file=sys.stderr)
            return 1

    spec = result["wiki_structure_spec"].model_dump()
    (out / "structure.json").write_text(json.dumps(spec, indent=2) + "\n", encoding="utf-8")
    (out / "analysis.json").write_text(
        json.dumps(
            {
                "repository_context": state.get("repository_context"),
                "repository_tree": state.get("repository_tree"),
                "readme_content": state.get("readme_content"),
            },
            indent=2,
        )
        + "\n",
        encoding="utf-8",
    )
    with open(out / "central_calls.jsonl", "w", encoding="utf-8") as handle:
        for call in central_calls:
            handle.write(json.dumps(call, ensure_ascii=False) + "\n")
    (out / "cluster_map.json").write_text(json.dumps(planner_capture, indent=1) + "\n", encoding="utf-8")
    requests = (out / "requests.jsonl").read_text(encoding="utf-8").splitlines()
    summary = {
        "repo": str(repo),
        "repo_name": repo_name,
        "branch": args.branch,
        "repo_identifier": repo_identifier,
        "planner": args.planner,
        "nodes": graph.number_of_nodes(),
        "documents": len(documents),
        "chat_requests": len(requests),
        "sections": len(spec["sections"]),
        "pages": spec["total_pages"],
        "central_calls": len(central_calls),
        "seconds": {k: round(v, 3) for k, v in timings.items()},
    }
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(summary))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
