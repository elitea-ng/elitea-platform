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
* file discovery is sorted;
* with ``--through phase2``, the two set iterations that reach the graph's
  node and edge ORDER follow the Rust engine's order (Phase 2 reads it).

The engine copy itself is not modified.

    python parity/python_reference.py <repo> <out-dir> [--no-phase1c] [--through phase2 [--search-dsn <dsn> [--dense-postfilter]]]

``--no-phase1c`` stops after Phase 1 (the builder's own graph: parsers,
documents, contraction, SQL, ORM), for checking the graph builder port
before the Phase 1c passes are ported.

``--through phase2`` goes on to Phase 2 (``graph_topology.run_phase2``: the
orphan cascade, doc edges, component bridging, weights, hubs) over the
index's own ``.wiki.db``. No model and no network: the embeddings are the
deterministic STAND-IN ``standin_embedding`` (SHA-256 of the text, 16
dimensions, L2-normalised), which the Rust engine implements identically
(``graph::topology::replay::standin_embedding``). The rows are dumped AFTER
Phase 2 (weights, ``is_hub``, the new edges), with ``stats.json`` (the dict
``run_phase2`` returns) and ``recording.jsonl``: every read Phase 2 made of
the index — one line per distinct call, ``{"call", "args", "result"}`` —
which ``deepwiki-parity graph-dump --through phase2 --replay`` answers the
Rust Phase 2 from. One reproducibility patch more for this phase:
``nx.weakly_connected_components`` yields each component SORTED, because
component bridging picks a component's representative with ``max()`` over
a set, whose order follows the hash seed (the Rust engine uses id order).

``--search-dsn <dsn>`` (with ``--through phase2``) measures the ADR-0026
"accepted difference": Phase 2's lexical, phrase-count and vector reads go
to a PostgreSQL emulation of the build space instead of FTS5 / sqlite-vec
(``PostgresSearchDB``: the same rows and vectors, the build space's folded
``deepwiki_porter`` tsvector, ``plainto_tsquery`` matching ranked by BM25
k1=1.2 b=0.75, ``phraseto_tsquery`` counts, exact pgvector L2 KNN filtered
by prefix BEFORE the limit). Everything else is unchanged, and the
recording replays in Rust like any other. ``--dense-postfilter`` makes the
vector search filter AFTER the limit, as sqlite-vec did (the ``k`` nearest
of all vectors, then the prefix), to separate that choice from the lexical
differences. Needs pgvector and ``psycopg``;
the schema ``topo`` in that database is dropped and rebuilt.

Needs the ``engine`` extra (``pip install -e 'services/elitea-deepwiki[engine]'``).
Without ``--through phase2`` no embedding is computed, and Phase 3 is never
run.
"""

from __future__ import annotations

import concurrent.futures
import hashlib
import json
import math
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


#: Dimension of the stand-in embedding.
STANDIN_DIM = 16


def standin_embedding(text: str) -> list[float]:
    """The deterministic stand-in for the embedding model.

    SHA-256 of the UTF-8 text; component ``i`` is the big-endian 16-bit
    word ``i`` of the digest scaled to [-0.5, 0.5]; then L2-normalised. The
    sum of squares is a plain left-to-right loop (Python 3.12's ``sum()``
    compensates, which the Rust side would have to copy for nothing).
    """
    digest = hashlib.sha256(text.encode("utf-8")).digest()
    vector = [int.from_bytes(digest[2 * i : 2 * i + 2], "big") / 65535.0 - 0.5 for i in range(STANDIN_DIM)]
    squares = 0.0
    for x in vector:
        squares += x * x
    norm = math.sqrt(squares)
    if norm == 0.0:
        return vector
    return [x / norm for x in vector]


def standin_batch(texts: list[str]) -> list[list[float]]:
    return [standin_embedding(text) for text in texts]


_NODE_KEYS = ("symbol_name", "rel_path", "symbol_type", "source_text", "docstring", "is_doc", "language")
_HIT_KEYS = ("node_id", "rel_path", "symbol_type", "fts_rank", "score_norm", "vec_distance")


class RecordingDB:
    """A ``UnifiedWikiDB`` that records every read Phase 2 makes.

    Writes (``set_hub``, ``upsert_edge``, ``upsert_edges_batch``,
    ``set_meta``, ``conn``) pass through unrecorded: their effect is in the
    dumped rows. Each distinct call is kept once, with the projection of its
    result that Phase 2 reads; a repeated call must return the same result.
    ``search_fts5`` and ``search_fts_with_path`` are one call,
    ``search_fts(query, path_prefix, limit)`` (the second is a wrapper of
    the first).
    """

    def __init__(self, db) -> None:
        self._db = db
        self.calls: dict[str, dict] = {}
        self.count = 0

    def __getattr__(self, name):
        return getattr(self._db, name)

    @property
    def vec_available(self) -> bool:
        return self._db.vec_available

    def _record(self, call: str, args: list, run, project):
        self.count += 1
        key = json.dumps([call, args])
        try:
            raw = run()
        except Exception as exc:  # noqa: BLE001 - recorded, then re-raised
            self.calls.setdefault(key, {"call": call, "args": args, "error": repr(exc)})
            raise
        result = project(raw)
        known = self.calls.get(key)
        if known is None:
            self.calls[key] = {"call": call, "args": args, "result": result}
        elif known.get("result") != result:
            raise AssertionError(f"{key}: a repeated call returned a different result")
        return raw

    def get_node(self, node_id):
        return self._record(
            "get_node", [node_id], lambda: self._db.get_node(node_id),
            lambda row: None if row is None else {k: row.get(k) for k in _NODE_KEYS},
        )

    def node_count(self):
        return self._record("node_count", [], self._db.node_count, lambda n: n)

    def count_fts_matches(self, query, *, exact_match=False):
        return self._record(
            "count_fts_matches", [query, exact_match],
            lambda: self._db.count_fts_matches(query, exact_match=exact_match), lambda n: n,
        )

    def _hits(self, rows):
        return [{k: row[k] for k in _HIT_KEYS if k in row} for row in rows or []]

    def search_fts5(self, query, path_prefix=None, cluster_id=None, symbol_types=None, limit=20):
        assert cluster_id is None and symbol_types is None
        return self._record(
            "search_fts", [query, path_prefix, limit],
            lambda: self._db.search_fts5(query=query, path_prefix=path_prefix, limit=limit), self._hits,
        )

    def search_fts_with_path(self, query, path_prefix, *, symbol_types=None, limit=20):
        assert symbol_types is None
        return self._record(
            "search_fts", [query, path_prefix, limit],
            lambda: self._db.search_fts_with_path(query, path_prefix=path_prefix, limit=limit), self._hits,
        )

    def get_embedding_by_id(self, node_id):
        return self._record(
            "get_embedding", [node_id], lambda: self._db.get_embedding_by_id(node_id), lambda v: v,
        )

    def search_vec(self, embedding, k=10, path_prefix=None, cluster_id=None):
        assert cluster_id is None
        return self._record(
            "search_dense", [list(embedding), k, path_prefix],
            lambda: self._db.search_vec(embedding, k=k, path_prefix=path_prefix), self._hits,
        )


def _patch_function(module, name: str, old: str, new: str) -> None:
    """Replace ``old`` with ``new`` in one function of ``module``, in place.

    The function object keeps its identity (only ``__code__`` changes), so
    every module that imported it sees the patched body.
    """
    import inspect  # noqa: PLC0415
    import types  # noqa: PLC0415

    function = getattr(module, name)
    source = inspect.getsource(function)
    if old not in source:
        raise RuntimeError(f"{module.__name__}.{name}: the reproducibility patch no longer applies")
    code = compile(source.replace(old, new), module.__file__, "exec")
    for constant in code.co_consts:
        if isinstance(constant, types.CodeType) and constant.co_name == name:
            function.__code__ = constant
            return
    raise RuntimeError(f"{module.__name__}.{name}: no code object")


def _make_node_order_reproducible() -> None:
    """Make the two set iterations that reach the GRAPH ORDER follow the
    Rust engine's order (Phase 2 reads the order; the row dumps do not):
    the Pylon verb set (sorted) and the L3 member-name intersection (the
    source class's member order)."""
    from elitea_deepwiki.engine.code_graph import api_surface_extractor, cross_language_linker  # noqa: PLC0415

    _patch_function(api_surface_extractor, "_match_pylon_api", "for method in methods:", "for method in sorted(methods):")
    _patch_function(
        cross_language_linker,
        "link_l3_containment",
        "for name in src_members.keys() & tgt_members.keys():",
        "for name in [n for n in src_members if n in tgt_members]:",
    )


_FOLD = "regexp_replace(%(q)s, '[^[:alnum:]]+', ' ', 'g')"


class PostgresSearchDB:
    """A ``UnifiedWikiDB`` whose SEARCHES run on PostgreSQL (see the module
    docs for ``--search-dsn``); rows, writes and everything else are the
    SQLite index's."""

    def __init__(self, db, dsn: str, *, dense_postfilter: bool = False) -> None:
        import psycopg  # noqa: PLC0415

        self._db = db
        self._dense_postfilter = dense_postfilter
        self._pg = psycopg.connect(dsn, autocommit=True)
        self._load()

    def __getattr__(self, name):
        return getattr(self._db, name)

    @property
    def vec_available(self) -> bool:
        return True

    def _load(self) -> None:
        import struct  # noqa: PLC0415

        pg = self._pg
        pg.execute("CREATE EXTENSION IF NOT EXISTS vector")
        pg.execute("DROP SCHEMA IF EXISTS topo CASCADE")
        pg.execute("CREATE SCHEMA topo")
        exists = pg.execute("SELECT 1 FROM pg_ts_config WHERE cfgname = 'deepwiki_porter'").fetchone()
        if not exists:
            pg.execute("CREATE TEXT SEARCH DICTIONARY deepwiki_stem (TEMPLATE = snowball, Language = english)")
            pg.execute("CREATE TEXT SEARCH CONFIGURATION deepwiki_porter (COPY = simple)")
            pg.execute(
                "ALTER TEXT SEARCH CONFIGURATION deepwiki_porter ALTER MAPPING FOR asciiword, asciihword, "
                "hword_asciipart, word, hword, hword_part, numword, hword_numpart WITH deepwiki_stem"
            )
        # Migration 0003's staging table, the columns Phase 2 reads.
        pg.execute(
            """CREATE TABLE topo.nodes (
                node_id TEXT PRIMARY KEY, rel_path TEXT NOT NULL, symbol_name TEXT NOT NULL,
                symbol_type TEXT NOT NULL, signature TEXT NOT NULL, docstring TEXT NOT NULL,
                source_text TEXT NOT NULL, dl DOUBLE PRECISION,
                fts TSVECTOR GENERATED ALWAYS AS (to_tsvector('deepwiki_porter', regexp_replace(
                    symbol_name || ' ' || signature || ' ' || docstring || ' ' || source_text,
                    '[^[:alnum:]]+', ' ', 'g'))) STORED)"""
        )
        pg.execute("CREATE TABLE topo.emb (node_id TEXT PRIMARY KEY, embedding VECTOR NOT NULL)")
        rows = self._db.conn.execute(
            "SELECT node_id, rel_path, symbol_name, symbol_type, signature, docstring, source_text FROM repo_nodes"
        ).fetchall()
        with pg.cursor() as cur, cur.copy(
            "COPY topo.nodes (node_id, rel_path, symbol_name, symbol_type, signature, docstring, source_text) FROM STDIN"
        ) as copy:
            for row in rows:
                copy.write_row([row[0]] + [(v or "") for v in tuple(row)[1:]])
        vectors = self._db.conn.execute("SELECT node_id, embedding FROM repo_vec").fetchall()
        with pg.cursor() as cur, cur.copy("COPY topo.emb (node_id, embedding) FROM STDIN") as copy:
            for node_id, blob in vectors:
                values = struct.unpack(f"{len(blob) // 4}f", blob)
                copy.write_row([node_id, "[" + ",".join(repr(v) for v in values) + "]"])
        # BM25 'fts' statistics: document length in lexeme positions,
        # document frequency per lexeme.
        pg.execute(
            "UPDATE topo.nodes SET dl = coalesce((SELECT sum(coalesce(array_length(positions, 1), 1)) "
            "FROM unnest(fts)), 0)"
        )
        pg.execute("CREATE TABLE topo.terms AS SELECT word AS term, ndoc AS df FROM ts_stat('SELECT fts FROM topo.nodes')")
        pg.execute("CREATE UNIQUE INDEX ON topo.terms (term)")
        pg.execute("CREATE INDEX ON topo.nodes USING GIN (fts)")
        pg.execute('CREATE INDEX ON topo.nodes (rel_path COLLATE "C")')
        pg.execute("ANALYZE")
        self._n, self._avgdl = pg.execute("SELECT count(*)::float8, avg(dl) FROM topo.nodes").fetchone()
        self._avgdl = self._avgdl if self._avgdl and self._avgdl > 0 else 1.0

    @staticmethod
    def _prefix(path_prefix):
        """``rel_path GLOB '<prefix>/*'`` as a range on the C-collated path."""
        if not path_prefix:
            return "", {}
        start = path_prefix.rstrip("/") + "/"
        return (
            ' AND n.rel_path COLLATE "C" >= %(p0)s AND n.rel_path COLLATE "C" < %(p1)s',
            {"p0": start, "p1": start[:-1] + "0"},
        )

    def count_fts_matches(self, query, *, exact_match=False):
        if not query or not query.strip():
            return 0
        function = "phraseto_tsquery" if exact_match else "plainto_tsquery"
        row = self._pg.execute(
            f"SELECT count(*) FROM topo.nodes n WHERE n.fts @@ {function}('deepwiki_porter', {_FOLD})",
            {"q": query},
        ).fetchone()
        return int(row[0])

    def search_fts5(self, query, path_prefix=None, cluster_id=None, symbol_types=None, limit=20):
        from elitea_deepwiki.engine.unified_db import _attach_score_norm  # noqa: PLC0415

        if not query or not query.strip():
            return []
        where, params = self._prefix(path_prefix)
        sql = f"""
            WITH m AS (
                SELECT n.node_id, n.rel_path, n.symbol_type, n.fts, n.dl FROM topo.nodes n
                WHERE n.fts @@ plainto_tsquery('deepwiki_porter', {_FOLD}){where}),
            q AS (SELECT lexeme AS term FROM unnest(to_tsvector('deepwiki_porter', {_FOLD}))),
            s AS (
                SELECT m.node_id, sum(
                    ln(1.0 + (%(n)s - t.df + 0.5) / (t.df + 0.5))
                    * (coalesce(array_length(u.positions, 1), 1) * 2.2)
                    / (coalesce(array_length(u.positions, 1), 1) + 1.2 * (0.25 + 0.75 * m.dl / %(avgdl)s))
                ) AS score
                FROM m CROSS JOIN LATERAL unnest(m.fts) u
                JOIN q ON q.term = u.lexeme JOIN topo.terms t ON t.term = u.lexeme
                GROUP BY m.node_id)
            SELECT m.node_id, m.rel_path, m.symbol_type, -coalesce(s.score, 0.0) AS fts_rank
            FROM m LEFT JOIN s ON s.node_id = m.node_id
            ORDER BY fts_rank, m.node_id COLLATE "C" LIMIT %(limit)s"""
        params.update({"q": query, "n": self._n, "avgdl": self._avgdl, "limit": limit})
        rows = self._pg.execute(sql, params).fetchall()
        return [
            _attach_score_norm({"node_id": r[0], "rel_path": r[1], "symbol_type": r[2], "fts_rank": float(r[3])})
            for r in rows
        ]

    def search_fts_with_path(self, query, path_prefix, *, symbol_types=None, limit=20):
        if not path_prefix:
            raise ValueError("search_fts_with_path requires a non-empty path_prefix")
        return self.search_fts5(query=query, path_prefix=path_prefix, limit=limit)

    def get_embedding_by_id(self, node_id):
        row = self._pg.execute("SELECT embedding::real[] FROM topo.emb WHERE node_id = %s", (node_id,)).fetchone()
        return None if row is None else [float(v) for v in row[0]]

    def search_vec(self, embedding, k=10, path_prefix=None, cluster_id=None):
        where, params = self._prefix(path_prefix)
        if self._dense_postfilter:
            sql = f"""
            SELECT * FROM (
                SELECT n.node_id, n.rel_path, n.symbol_type, e.embedding <-> %(v)s::vector AS vec_distance
                FROM topo.emb e JOIN topo.nodes n ON n.node_id = e.node_id
                ORDER BY vec_distance, n.node_id COLLATE "C" LIMIT %(k)s) n
            WHERE TRUE{where} ORDER BY vec_distance, n.node_id COLLATE "C"
            """
        else:
            sql = f"""
            SELECT n.node_id, n.rel_path, n.symbol_type, e.embedding <-> %(v)s::vector AS vec_distance
            FROM topo.emb e JOIN topo.nodes n ON n.node_id = e.node_id WHERE TRUE{where}
            ORDER BY vec_distance, n.node_id COLLATE "C" LIMIT %(k)s"""
        params.update({"v": "[" + ",".join(repr(float(x)) for x in embedding) + "]", "k": k})
        rows = self._pg.execute(sql, params).fetchall()
        return [{"node_id": r[0], "rel_path": r[1], "symbol_type": r[2], "vec_distance": float(r[3])} for r in rows]


def _sorted_components(original):
    def weakly_connected_components(G):
        for component in original(G):
            yield sorted(component)

    return weakly_connected_components


def main() -> int:
    args = sys.argv[1:]
    through = "phase1c"
    search_dsn = None
    dense_postfilter = "--dense-postfilter" in args
    if dense_postfilter:
        args.remove("--dense-postfilter")
    if "--search-dsn" in args:
        at = args.index("--search-dsn")
        if at + 1 >= len(args):
            print(__doc__, file=sys.stderr)
            return 2
        search_dsn = args[at + 1]
        del args[at : at + 2]
    if "--through" in args:
        at = args.index("--through")
        if at + 1 >= len(args) or args[at + 1] not in ("phase1c", "phase2"):
            print(__doc__, file=sys.stderr)
            return 2
        through = args[at + 1]
        del args[at : at + 2]
    if (search_dsn and through != "phase2") or (dense_postfilter and not search_dsn):
        print("--search-dsn needs --through phase2", file=sys.stderr)
        return 2
    argv = [a for a in args if a != "--no-phase1c"]
    phase1c = "--no-phase1c" not in args
    if len(argv) != 2:
        print(__doc__, file=sys.stderr)
        return 2
    repo = Path(argv[0]).resolve()
    out = Path(argv[1]).resolve()
    out.mkdir(parents=True, exist_ok=True)
    logging.basicConfig(level=os.environ.get("LOGLEVEL", "WARNING"))

    _make_reproducible()
    if through == "phase2":
        _make_node_order_reproducible()
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

    phase2_extra = {}
    with tempfile.TemporaryDirectory() as scratch:
        db_path = os.path.join(scratch, "reference.wiki.db")
        if through == "phase2":
            import networkx as nx  # noqa: PLC0415
            from elitea_deepwiki.engine import graph_topology  # noqa: PLC0415

            nx.weakly_connected_components = _sorted_components(nx.weakly_connected_components)
            with UnifiedWikiDB(db_path, embedding_dim=STANDIN_DIM) as udb:
                if not udb.vec_available:
                    print("sqlite-vec is not loadable: Phase 2 would run without vectors", file=sys.stderr)
                    return 1
                udb.from_networkx(graph)
                started = time.time()
                embedded = udb.populate_embeddings(embedding_fn=standin_batch, batch_size=64)
                embed_seconds = time.time() - started
                searcher = (
                    PostgresSearchDB(udb, search_dsn, dense_postfilter=dense_postfilter) if search_dsn else udb
                )
                recorder = RecordingDB(searcher)
                started = time.time()
                stats = graph_topology.run_phase2(recorder, graph, embedding_fn=standin_embedding)
                phase2_seconds = time.time() - started
            (out / "stats.json").write_text(json.dumps(stats) + "\n", encoding="utf-8")
            with open(out / "recording.jsonl", "w", encoding="utf-8") as handle:
                for entry in recorder.calls.values():
                    handle.write(json.dumps(entry, ensure_ascii=False) + "\n")
            phase2_extra = {
                "through": "phase2",
                "search": ("postgresql-postfilter" if dense_postfilter else "postgresql") if search_dsn else "sqlite",
                "embedded": embedded,
                "embed_seconds": round(embed_seconds, 2),
                "phase2_seconds": round(phase2_seconds, 2),
                "phase2_reads": recorder.count,
                "phase2_distinct_reads": len(recorder.calls),
            }
        else:
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
        **phase2_extra,
    }
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(summary))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
