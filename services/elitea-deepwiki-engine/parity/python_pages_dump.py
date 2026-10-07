#!/usr/bin/env python3
"""Dump the Python engine's page generation and export, reproducibly.

The reference side of the ADR-0026 page gate (phase 5b of the port). It
builds the index exactly as ``python_phase3_dump.py`` does (Phase 1, 1c,
2 with the stand-in embedding, 3), then runs the LIVE page path of
``OptimizedWikiGenerationAgent`` against a FIXED wiki structure:

``dispatch_page_generation`` (page splitting) → ``generate_page_content``
per page, in ``Send`` order (cluster expansion or the doc-only path,
``_format_simple_context``, ``_generate_simple`` with the
``ENHANCED_CONTENT_GENERATION_PROMPT_V3_TONE_ADJUSTED`` prompt, the Mermaid
sanitizer) → ``finalize_wiki`` → ``export_wiki`` → the hybrid wrapper's
result → ``wiki_subprocess_worker.main`` (the real composition code, with
the wrapper and the model factory stubbed).

The model is ``services/elitea-deepwiki-engine/testdata/llm_stub.py``,
served in
process; every chat request body is recorded. LangGraph's map step is run
sequentially in ``Send`` order (its reducer keeps that order), so the
recording is deterministic.

Written to ``<out-dir>``:

``structure.json``
    The structure (``WikiStructureSpec.model_dump``) the pages follow. With
    ``--structure`` it is the given one; without, the cluster planner makes
    it against the stub first (``--planner-only`` stops there).
``nodes.jsonl`` / ``edges.jsonl``
    ``repo_nodes`` / ``repo_edges`` after Phase 3, in rowid order — the
    index the page path reads. The Rust side loads exactly these rows.
``graph.json``
    What the page path reads from the in-memory networkx graph:
    ``node_count``, ``has_name_index``, the ``_get_imports_from_graph``
    answer for every file, and the doc-node scan inputs.
``fts.jsonl``
    Every full-text search the page path ran (``search_fts5`` and the
    ``_fts_fallback`` symbol lookup) with the node ids it returned, in
    order. The Rust replay search answers from these.
``requests.jsonl``
    Every chat completion request body, in order.
``pages.json``
    The ``WikiPage`` list after finalisation, the structure after the
    split, ``errors`` and ``failed_pages``.
``result.json``
    The worker's output file (the composed result).
``summary.json``
    Counts and the page-path wall time.

    PYTHONHASHSEED=0 PYTHONPATH=services/elitea-deepwiki/src \\
        python parity/python_pages_dump.py <repo> <out-dir> [--structure <structure.json>] [--planner-only]
"""

from __future__ import annotations

import argparse
import json
import re
import logging
import os
import sys
import tempfile
import threading
import time
from http.server import ThreadingHTTPServer
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
sys.path.insert(0, str(HERE.parent / "testdata"))

import python_phase3_dump as p3  # noqa: E402

#: The repository context the pages are generated with: what the stub
#: answers to the repository-analysis prompt (a JSON ask).
COMMIT_HASH = "0123456789abcdef0123456789abcdef01234567"

#: With ``--mermaid-pages`` a PAGE prompt (one with the page template's
#: "**Context Variables:**" block) is answered with this text, the page
#: name first: broken diagrams that send every page through the
#: sanitizer's repairs. ``wiki::parity::MERMAID_PAGE`` is the same text.
MERMAID_PAGE = (
    "## Overview\n\nThis page documents the component.\n\n"
    "```mermaid\nA[Start here] --> B[Load (config)]\nB -.-> end[\"Done\"]\nB -->| yes | C[\"x\"]\n```\n\n"
    "Then the sequence:\n\n"
    "```mermaid\nsequenceDiagram\n  participant end\n  User->>API: call(a ,b)\n  API-->>end: ok; done\n  deactivate X\n```\n"
    "\nAn inline closer:\n```mermaid\ngraph\n  X --> Y```\nAfter.\n"
)
PAGE_NAME = re.compile(r"^- Page: (.*)$", re.MULTILINE)
QUERY = "Document the repository"


def start_stub(mermaid_pages: bool = False):
    """Serve ``llm_stub`` on a free loopback port, recording POST bodies."""
    import llm_stub  # noqa: PLC0415

    if mermaid_pages:
        original_answer = llm_stub.answer

        def answer(prompt: str) -> str:
            if "**Context Variables:**" in prompt:
                match = PAGE_NAME.search(prompt)
                return f"# {match.group(1) if match else ''}\n\n" + MERMAID_PAGE
            return original_answer(prompt)

        llm_stub.answer = answer

    recorded: list[dict] = []
    lock = threading.Lock()

    class Recording(llm_stub.Handler):
        def do_POST(self):  # noqa: N802 - http.server API
            length = int(self.headers.get("content-length", 0))
            raw = self.rfile.read(length) or b"{}"
            body = json.loads(raw)
            if self.path.endswith("/chat/completions"):
                with lock:
                    recorded.append(body)
            # Re-feed the body to the stub handler.
            import io  # noqa: PLC0415

            self.rfile = io.BytesIO(raw)
            self.headers.replace_header("content-length", str(len(raw)))
            return super().do_POST()

    server = ThreadingHTTPServer(("127.0.0.1", 0), Recording)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, recorded, llm_stub


def build_index(repo: Path, db_path: str):
    """Phase 1 + 1c + 2 + 3 into ``db_path``; returns (graph, udb)."""
    import concurrent.futures  # noqa: PLC0415

    concurrent.futures.ThreadPoolExecutor = p3.InlineExecutor  # type: ignore[misc]
    concurrent.futures.ProcessPoolExecutor = p3.InlineExecutor  # type: ignore[misc]
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
                module.ThreadPoolExecutor = p3.InlineExecutor
            if hasattr(module, "as_completed"):
                module.as_completed = concurrent.futures.as_completed

    original_discover = gb.EnhancedUnifiedGraphBuilder._discover_files_by_language

    def sorted_discover(self, *args, **kwargs):
        found = original_discover(self, *args, **kwargs)
        return {language: sorted(paths) for language, paths in sorted(found.items())}

    gb.EnhancedUnifiedGraphBuilder._discover_files_by_language = sorted_discover

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
    if flags.markdown_structure:
        from elitea_deepwiki.engine.markdown_structure import wire_markdown_structure  # noqa: PLC0415

        wire_markdown_structure(graph, flags=flags)

    udb = UnifiedWikiDB(db_path, embedding_dim=p3.EMBEDDING_DIM)
    udb.from_networkx(graph)
    udb.populate_embeddings(
        embedding_fn=lambda texts: [p3.stand_in_embedding(t) for t in texts],
        batch_size=64,
    )
    phase2_stats = run_phase2(db=udb, G=graph, embedding_fn=p3.stand_in_embedding)
    hubs = set(phase2_stats.get("hubs", {}).get("node_ids", []))
    gc.run_phase3(db=udb, G=graph, hubs=hubs)
    udb.set_meta("phase3_completed", "1")
    udb.conn.commit()
    return graph, udb


def dump_rows(udb, out: Path) -> None:
    cur = udb.conn.execute("SELECT * FROM repo_nodes ORDER BY rowid")
    columns = [d[0] for d in cur.description]
    with open(out / "nodes.jsonl", "w", encoding="utf-8") as handle:
        for row in cur:
            record = dict(zip(columns, row))
            record.pop("indexed_at", None)
            handle.write(json.dumps(record, ensure_ascii=False) + "\n")
    cur = udb.conn.execute("SELECT * FROM repo_edges ORDER BY id")
    columns = [d[0] for d in cur.description]
    with open(out / "edges.jsonl", "w", encoding="utf-8") as handle:
        for row in cur:
            record = dict(zip(columns, row))
            handle.write(json.dumps(record, ensure_ascii=False) + "\n")


class InsertionOrderedSet:
    """A ``set`` that iterates in insertion order.

    ``cluster_expansion`` walks ``list(seen_ids)`` (a ``set``) to find the
    orphan seeds for framework discovery, so its document order follows
    the string hash seed. The reference makes it insertion order, the
    order the Rust port uses (a recorded deliberate difference)."""

    def __init__(self, items=()):
        self._items = dict.fromkeys(items)

    def add(self, item):
        self._items[item] = None

    def discard(self, item):
        self._items.pop(item, None)

    def __contains__(self, item):
        return item in self._items

    def __iter__(self):
        return iter(list(self._items))

    def __len__(self):
        return len(self._items)

    def __bool__(self):
        return bool(self._items)


def install_fts_recorder(udb, records: list) -> None:
    from elitea_deepwiki.engine import cluster_expansion as ce  # noqa: PLC0415

    ce.set = InsertionOrderedSet  # see InsertionOrderedSet
    original_search = udb.search_fts5

    def recording_search(query, path_prefix=None, cluster_id=None, symbol_types=None, limit=20):
        rows = original_search(
            query=query, path_prefix=path_prefix, cluster_id=cluster_id,
            symbol_types=symbol_types, limit=limit,
        )
        records.append({
            "kind": "search",
            "query": query,
            "cluster_id": cluster_id,
            "limit": limit,
            "node_ids": [r.get("node_id") for r in rows],
        })
        return rows

    udb.search_fts5 = recording_search

    original_fallback = ce._fts_fallback

    def recording_fallback(db, name, macro_id=None):
        rows = original_fallback(db, name, macro_id)
        records.append({
            "kind": "symbol",
            "query": name,
            "cluster_id": macro_id,
            "limit": 3,
            "node_ids": [dict(r).get("node_id") for r in rows],
        })
        return rows

    ce._fts_fallback = recording_fallback


def graph_view(graph, agent, spec) -> dict:
    """What the page path reads from the networkx graph."""
    files = sorted({
        (data.get("rel_path", "") or data.get("file_path", ""))
        for _, data in graph.nodes(data=True)
        if (data.get("rel_path", "") or data.get("file_path", ""))
    })
    imports = {}
    for path in files:
        value = agent._get_imports_from_graph(path)
        if value:
            imports[path] = value
    # The split's file per symbol: `_name_index[symbol][0]`'s path.
    name_index = getattr(graph, "_name_index", None) or {}
    name_paths = {}
    for section in spec.sections:
        for page in section.pages:
            for sym in page.target_symbols:
                nids = name_index.get(sym, [])
                if nids:
                    nd = graph.nodes.get(nids[0], {})
                    fpath = nd.get("rel_path") or nd.get("file_path", "")
                    if fpath:
                        name_paths[sym] = fpath
    return {
        "node_count": graph.number_of_nodes(),
        "has_name_index": bool(name_index),
        "imports": imports,
        "name_paths": name_paths,
    }


def make_llm(base_url: str):
    from langchain_openai import ChatOpenAI  # noqa: PLC0415

    # As wiki_subprocess_worker._build_llm_and_embeddings for an OpenAI
    # provider with the e2e harness's llm_settings.
    return ChatOpenAI(
        model="gpt-4o",
        temperature=0.1,
        api_key="mock",
        base_url=base_url,
        organization=None,
        max_retries=2,
        streaming=True,
        max_tokens=4000,
    )


class FakeIndexer:
    def __init__(self, graph, repo_root: str):
        self.relationship_graph = graph
        self._repo_root = repo_root
        self._last_commit_hash = COMMIT_HASH
        self.graph_manager = None

    def get_repo_root(self):
        return self._repo_root


class FakeRetriever:
    """The page path reaches ``search_repository`` only when every other
    retrieval came back empty (the vector fallback the port leaves out)."""

    def __init__(self, graph, reached: list):
        self.relationship_graph = graph
        self._reached = reached

    def search_repository(self, query, k=50):
        self._reached.append(query)
        raise RuntimeError("vector fallback reached (not part of the parity run)")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("repo")
    parser.add_argument("out")
    parser.add_argument("--structure")
    parser.add_argument("--planner-only", action="store_true")
    parser.add_argument("--mermaid-pages", action="store_true", help="answer page prompts with broken diagrams")
    parser.add_argument("--repository", default=None, help="owner/repo (default acme/<dir name>)")
    args = parser.parse_args()

    repo = Path(args.repo).resolve()
    out = Path(args.out).resolve()
    out.mkdir(parents=True, exist_ok=True)
    repository = args.repository or f"acme/{repo.name}"
    logging.basicConfig(level=os.environ.get("LOGLEVEL", "WARNING"))

    server, recorded, llm_stub = start_stub(args.mermaid_pages)
    base_url = f"http://127.0.0.1:{server.server_address[1]}/v1"
    fts_records: list = []
    vector_reached: list = []

    with tempfile.TemporaryDirectory() as scratch:
        db_path = os.path.join(scratch, "pages.wiki.db")
        graph, udb = build_index(repo, db_path)
        dump_rows(udb, out)

        from elitea_deepwiki.engine.state.wiki_state import WikiStructureSpec  # noqa: PLC0415

        llm = make_llm(base_url)
        if args.structure:
            spec = WikiStructureSpec.model_validate(json.loads(Path(args.structure).read_text(encoding="utf-8")))
        else:
            from elitea_deepwiki.engine.wiki_structure_planner.cluster_planner import (  # noqa: PLC0415
                ClusterStructurePlanner,
            )

            spec = ClusterStructurePlanner(db=udb, llm=llm, wiki_title=None).plan_structure()
        (out / "structure.json").write_text(
            json.dumps(spec.model_dump(), indent=2, ensure_ascii=False) + "\n", encoding="utf-8"
        )
        if args.planner_only:
            udb.close()
            server.shutdown()
            print(json.dumps({"repo": str(repo), "sections": len(spec.sections)}))
            return 0
        recorded.clear()

        install_fts_recorder(udb, fts_records)
        from elitea_deepwiki.engine.agents.wiki_graph_optimized import OptimizedWikiGenerationAgent  # noqa: PLC0415

        indexer = FakeIndexer(graph, str(repo))
        agent = OptimizedWikiGenerationAgent(
            indexer=indexer,
            retriever_stack=FakeRetriever(graph, vector_reached),
            llm=llm,
            repository_url=repository,
            branch="main",
        )
        agent._cluster_db_path = db_path
        agent._cluster_db = udb  # the open DB, so the recorders see every search

        repo_context = llm_stub.answer("json")  # what the stub answers to the analysis prompt
        state = {
            "wiki_structure_spec": spec,
            "repository_context": repo_context,
            "wiki_pages": [],
            "errors": [],
            "start_time": time.time(),
        }
        config = {"max_concurrency": 4, "recursion_limit": 5000, "configurable": {"thread_id": "parity"}}

        structure_before_split = spec.model_copy(deep=True)
        started = time.perf_counter()
        sends = agent.dispatch_page_generation(state, config)
        for send in sends:
            update = agent.generate_page_content(send.arg, config)
            state["wiki_pages"] = state["wiki_pages"] + update.get("wiki_pages", [])
            state["errors"] = state["errors"] + update.get("errors", [])
        agent._close_cluster_db = lambda: None  # keep the DB for graph_view; closed below
        final = agent.finalize_wiki(state, config)
        state["wiki_pages"] = final.get("wiki_pages", state["wiki_pages"])
        state["generation_summary"] = final.get("generation_summary")
        exported = agent.export_wiki(state, config)
        page_seconds = time.perf_counter() - started

        # Context building alone (retrieval, budget, formatting; no model),
        # a second pass over the split structure.
        started = time.perf_counter()
        for section in spec.sections:
            for page in section.pages:
                agent._get_relevant_content_for_page(page, repository, repo_context)
        context_seconds = time.perf_counter() - started

        wiki_pages = state["wiki_pages"]
        failed_pages = [
            {"page_id": p.page_id, "title": p.title, "status": p.status}
            for p in wiki_pages
            if getattr(p, "status", None) and str(p.status).lower() in {"failed", "enhancement_failed"}
        ]
        # The agent's generate_wiki result, as the hybrid wrapper reads it.
        agent_result = {
            "success": True,
            "generated_pages": {p.page_id: p.content for p in wiki_pages},
            "generation_summary": state.get("generation_summary") or {},
            "artifacts": exported.get("artifacts", []),
            "execution_time": page_seconds,
            "errors": state["errors"],
            "failed_pages": failed_pages,
            "repository_context": repo_context,
        }
        (out / "pages.json").write_text(json.dumps({
            "structure_after_split": spec.model_dump(),
            "pages": [
                {"page_id": p.page_id, "title": p.title, "content": p.content, "status": p.status}
                for p in wiki_pages
            ],
            "errors": state["errors"],
            "failed_pages": failed_pages,
            "export_wiki_id": exported.get("wiki_id"),
            "export_artifacts": exported.get("artifacts", []),
        }, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")

        view = graph_view(graph, agent, structure_before_split)
        view["vector_fallback_queries"] = vector_reached
        (out / "graph.json").write_text(json.dumps(view, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
        udb.close()

        # ── wiki_subprocess_worker.main over the wrapper result ──
        from elitea_deepwiki.engine import hybrid_wiki_toolkit_wrapper as hw  # noqa: PLC0415
        from elitea_deepwiki.engine import wiki_subprocess_worker as worker  # noqa: PLC0415

        class FakeAgent:
            def __init__(self, **kwargs):
                pass

            def generate_wiki(self, query):
                return json.loads(json.dumps(agent_result))

        def no_index(wrapper_self):
            wrapper_self.indexer = indexer

        import importlib  # noqa: PLC0415

        # The worker imports through the `plugin_implementation` alias,
        # which loads a second module object: patch both.
        for module in (hw, importlib.import_module("plugin_implementation.hybrid_wiki_toolkit_wrapper")):
            module.OptimizedWikiGenerationAgent = FakeAgent
            module.HybridWikiToolkitWrapper._initialize_components_sync = no_index
        worker._build_llm_and_embeddings = lambda settings, model: (None, None, "text-embedding-3-small")
        worker._configure_logging = lambda: None
        owner, name = repository.split("/", 1)
        payload = {
            "base_path": os.path.join(scratch, "worker"),
            "query": QUERY,
            "llm_settings": {"model_name": "gpt-4o", "api_base": base_url, "api_key": "mock"},
            "embedding_model": "text-embedding-3-small",
            "repo_config": {
                "provider_type": "github",
                "provider_config": {"base_url": "https://api.github.com"},
                "repository": repository,
                "branch": "main",
                "project": None,
            },
            "active_branch": "main",
        }
        input_path = os.path.join(scratch, "in.json")
        output_path = os.path.join(scratch, "out.json")
        Path(input_path).write_text(json.dumps(payload), encoding="utf-8")
        stdout = sys.stdout
        sys.stdout = open(os.devnull, "w")  # noqa: SIM115 - the worker prints progress
        try:
            code = worker.main(["--input", input_path, "--output", output_path])
        finally:
            sys.stdout.close()
            sys.stdout = stdout
        if code != 0:
            raise SystemExit(f"worker failed: {Path(output_path).read_text()}")
        (out / "result.json").write_text(Path(output_path).read_text(encoding="utf-8"), encoding="utf-8")

    server.shutdown()
    with open(out / "requests.jsonl", "w", encoding="utf-8") as handle:
        for body in recorded:
            handle.write(json.dumps(body, ensure_ascii=False) + "\n")
    with open(out / "fts.jsonl", "w", encoding="utf-8") as handle:
        for record in fts_records:
            handle.write(json.dumps(record, ensure_ascii=False) + "\n")
    summary = {
        "repo": str(repo),
        "repository": repository,
        "pages": len(wiki_pages),
        "failed": len(failed_pages),
        "requests": len(recorded),
        "fts_calls": len(fts_records),
        "vector_fallbacks": len(vector_reached),
        "page_seconds": round(page_seconds, 4),
        "context_seconds": round(context_seconds, 4),
        "page_answer": "mermaid" if args.mermaid_pages else "stub",
    }
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(summary))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
