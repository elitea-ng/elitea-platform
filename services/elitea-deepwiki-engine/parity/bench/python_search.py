#!/usr/bin/env python3
"""The Python engine's retrieval for the benchmark question set.

Opens the ``.wiki.db`` a ``generate_wiki`` run left in the scratch cache,
readonly, exactly as ``ask_subprocess_worker`` does, builds the
``UnifiedRetriever`` it builds (the query embedded by the worker's
``OpenAIEmbeddings`` — which sends cl100k token ids, as shipped), and asks
``search_repository(question, k=--limit, apply_expansion=False)``: FTS5 and
sqlite-vec KNN fused by weighted RRF, pools of 30 and 30 — the same call the
Rust ``deepwiki-bench search`` makes against PostgreSQL. A failed vector
branch falls back to FTS5 only, as in the engine.

``--text-embeddings`` is a DIAGNOSTIC variant, not an engine run: the
``.wiki.db`` is copied, every vector the index holds is recomputed from the
same ``source_text`` with ``check_embedding_ctx_length=False`` (the TEXT is
sent, as the Rust engine sends it, instead of cl100k token ids), and the
queries are embedded the same way. Everything else — FTS5, the fusion, the
pools — is the shipped code. It separates the token-id defect from the rest
of the retrieval difference.

One output line per question: ``id``, ``hits`` (``node_id``, ``rel_path``,
``symbol_name``, ``symbol_type``, ``combined_score``), ``embed_ms`` (the
query embedding, measured separately), ``search_ms``.

    PYTHONPATH=services/elitea-deepwiki/src[:parity/bench/pypatch] \\
        python parity/bench/python_search.py <wiki.db> <questions.jsonl> <out.jsonl> \\
        --api-base http://127.0.0.1:18950/v1 --embedding-model Qwen/Qwen3-Embedding-4B
"""

from __future__ import annotations

import argparse
import json
import logging
import time
from pathlib import Path


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("db", type=Path)
    parser.add_argument("questions", type=Path)
    parser.add_argument("out", type=Path)
    parser.add_argument("--api-base", required=True)
    parser.add_argument("--embedding-model", required=True)
    parser.add_argument("--limit", type=int, default=10)
    parser.add_argument("--text-embeddings", action="store_true")
    args = parser.parse_args()
    logging.basicConfig(level=logging.WARNING)

    from langchain_openai import OpenAIEmbeddings  # noqa: PLC0415
    from elitea_deepwiki.engine.unified_db import UnifiedWikiDB  # noqa: PLC0415
    from elitea_deepwiki.engine.unified_retriever import UnifiedRetriever  # noqa: PLC0415

    embeddings = OpenAIEmbeddings(
        model=args.embedding_model, openai_api_key="bench",
        openai_api_base=args.api_base, openai_organization="1",
        **({"check_embedding_ctx_length": False} if args.text_embeddings else {}),
    )
    db_path = args.db
    if args.text_embeddings:
        import shutil  # noqa: PLC0415
        db_path = args.out.with_suffix(".wiki.db")
        shutil.copy(args.db, db_path)
        writer = UnifiedWikiDB(str(db_path))
        writer.conn.execute("DELETE FROM repo_vec")
        writer.populate_embeddings(embeddings.embed_documents, batch_size=32)
        writer.close()
    db = UnifiedWikiDB(str(db_path), readonly=True)
    vectors = db.conn.execute("SELECT count(*) FROM repo_vec").fetchone()[0] if db.vec_available else 0
    timings: dict[str, float] = {}

    def embed(text: str):
        started = time.perf_counter()
        try:
            return embeddings.embed_query(text)
        finally:
            timings["embed_ms"] = (time.perf_counter() - started) * 1000

    retriever = UnifiedRetriever(db=db, embedding_fn=embed, embeddings=embeddings)
    with args.out.open("w", encoding="utf-8") as out:
        for line in args.questions.open(encoding="utf-8"):
            if not line.strip():
                continue
            item = json.loads(line)
            timings.clear()
            started = time.perf_counter()
            docs = retriever.search_repository(item["question"], k=args.limit, apply_expansion=False)
            total_ms = (time.perf_counter() - started) * 1000
            hits = [{
                "node_id": d.metadata.get("node_id"),
                "rel_path": d.metadata.get("rel_path") or d.metadata.get("file_path") or d.metadata.get("source"),
                "symbol_name": d.metadata.get("symbol_name"),
                "symbol_type": d.metadata.get("symbol_type"),
                "combined_score": d.metadata.get("combined_score"),
            } for d in docs]
            embed_ms = timings.get("embed_ms", 0.0)
            out.write(json.dumps({"id": item["id"], "hits": hits, "embed_ms": embed_ms,
                                  "search_ms": total_ms - embed_ms, "index_vectors": vectors}) + "\n")
    db.close()
    if args.text_embeddings:
        db_path.unlink(missing_ok=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
