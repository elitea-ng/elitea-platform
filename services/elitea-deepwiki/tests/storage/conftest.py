"""Fixtures for the storage-parity tests.

The PostgreSQL tests need a live database with pgvector. They **skip** when
``DEEPWIKI_TEST_DSN`` is unset, and that skip is deliberately loud in the
report — a parity suite that silently passes because it never connected is the
exact failure mode this whole phase exists to avoid.

    podman run -d --name dwpg -e POSTGRES_PASSWORD=deepwiki \\
        -e POSTGRES_USER=deepwiki -e POSTGRES_DB=deepwiki \\
        -p 15434:5432 pgvector/pgvector:0.8.5-pg16-trixie
    export DEEPWIKI_TEST_DSN=postgresql://deepwiki:deepwiki@127.0.0.1:15434/deepwiki
"""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any, Iterator

import pytest

from elitea_deepwiki.storage.base import Node

# parents[4] is the repository root: storage -> tests -> elitea-deepwiki ->
# services -> root. The fixtures moved out of this service in P1.0.
FIXTURES = (
    Path(__file__).resolve().parents[4]
    / "conformance"
    / "provider"
    / "fixtures"
    / "deepwiki"
    / "retrieval"
    / "sample-repo"
)

DSN_ENV = "DEEPWIKI_TEST_DSN"


def load(*parts: str) -> Any:
    return json.loads(FIXTURES.joinpath(*parts).read_text(encoding="utf-8"))


def query_fixtures() -> list[tuple[str, dict[str, Any]]]:
    return [
        (path.stem, json.loads(path.read_text(encoding="utf-8")))
        for path in sorted((FIXTURES / "queries").glob("*.json"))
    ]


@pytest.fixture(scope="session")
def corpus() -> list[Node]:
    """The frozen chunk set the P0 retrieval fixtures were recorded over."""
    return [
        Node(
            node_id=record["node_id"],
            rel_path=record["rel_path"],
            file_name=record["file_name"],
            language=record["language"],
            start_line=record["start_line"],
            end_line=record["end_line"],
            symbol_name=record["symbol_name"],
            symbol_type=record["symbol_type"],
            parent_symbol=record["parent_symbol"],
            source_text=record["source_text"],
            docstring=record["docstring"],
            signature=record["signature"],
        )
        for record in load("nodes.json")["nodes"]
    ]


@pytest.fixture(scope="session")
def embeddings() -> dict[str, list[float]]:
    """The recorded stub-embedder vectors. No model is called anywhere here."""
    return load("embedding_model.json")["vectors"]


@pytest.fixture(scope="session")
def documents(corpus: list[Node]) -> list[tuple[str, str]]:
    """The BM25 document text: the same concatenation the recorder embedded."""
    return [
        (
            node.node_id,
            "\n".join(
                part
                for part in (
                    node.symbol_name,
                    node.signature,
                    node.docstring,
                    node.source_text,
                )
                if part
            ),
        )
        for node in corpus
    ]


#: Set in CI. Turns the "no database, skip" path into a hard failure, so a
#: misconfigured workflow cannot report a green parity run it never performed.
#: Without this, the most valuable gate in the port is also the easiest one to
#: switch off by accident.
REQUIRE_ENV = "DEEPWIKI_REQUIRE_POSTGRES"


@pytest.fixture(scope="session")
def dsn() -> str:
    value = os.environ.get(DSN_ENV)
    if value:
        return value

    message = (
        f"{DSN_ENV} is not set — the PostgreSQL parity gate did NOT run. "
        "Start pgvector and export the DSN (see this file's docstring)."
    )
    if os.environ.get(REQUIRE_ENV, "").strip().lower() in ("1", "true", "yes"):
        pytest.fail(f"{message} {REQUIRE_ENV} is set, so this is an error.")
    pytest.skip(message)


#: The clean slate the PostgreSQL fixture starts from: every object the
#: migrations create, dropped in dependency order. The build space schema
#: (0003) goes BEFORE the text search configuration: its generated `fts`
#: column depends on `deepwiki_porter`, so dropping the configuration first
#: fails on any database a previous run migrated.
#: `test_migrations.py` checks this list against the migration files.
RESET_STATEMENTS: tuple[str, ...] = (
    "DROP SCHEMA IF EXISTS deepwiki_build CASCADE",
    "DROP TABLE IF EXISTS wiki_bm25_postings, wiki_bm25_terms, "
    "wiki_bm25_docs, wiki_bm25_meta, wiki_node_embeddings, "
    "wiki_edges, wiki_nodes, wikis, invocation_events, invocations, "
    "schema_migrations CASCADE",
    "DROP TEXT SEARCH CONFIGURATION IF EXISTS deepwiki_porter",
    "DROP TEXT SEARCH DICTIONARY IF EXISTS deepwiki_stem",
)


@pytest.fixture(scope="session")
def postgres_backend(dsn: str, corpus, embeddings, documents) -> Iterator[Any]:
    """A migrated database loaded with the fixture corpus."""
    psycopg = pytest.importorskip("psycopg", reason="the storage-postgres extra")

    from elitea_deepwiki.storage.migrate import apply_all
    from elitea_deepwiki.storage.postgres import PostgresBackend

    connection = psycopg.connect(dsn)
    try:
        # A clean slate: parity must not depend on leftovers from a prior run.
        with connection.cursor() as cursor:
            for statement in RESET_STATEMENTS:
                cursor.execute(statement)
        connection.commit()

        apply_all(connection)

        backend = PostgresBackend(connection, "acme--notes-service--main")
        backend.upsert_nodes(corpus)
        backend.upsert_embeddings(
            [(node_id, vector) for node_id, vector in embeddings.items()]
        )
        backend.build_bm25(documents)
        yield backend
    finally:
        connection.close()


@pytest.fixture(scope="session")
def sqlite_backend(corpus, embeddings, documents) -> Iterator[Any]:
    """The legacy reference backend, loaded with the same corpus."""
    pytest.importorskip("networkx", reason="the storage-legacy extra")
    pytest.importorskip("sqlite_vec", reason="the storage-legacy extra")

    from elitea_deepwiki.storage.sqlite import SqliteBackend

    backend = SqliteBackend("acme--notes-service--main", embedding_dim=64)
    try:
        backend.upsert_nodes(corpus)
        backend.upsert_embeddings(
            [(node_id, vector) for node_id, vector in embeddings.items()]
        )
        backend.build_bm25(documents)
        yield backend
    finally:
        backend.close()
