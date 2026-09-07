"""The two-runner trap, made mechanical: the Python half.

Loads every fixture file under
``conformance/provider/fixtures/inventory/{ingestion,retrieval}/`` — the SAME
files ``fixture_parity_test.go``
(``services/elitea-subapp-host/internal/apps/inventory/run``) loads — and
asserts this package's fixture graph answers each one with the SAME shape the
Go runner's own test asserts. Neither test can see the other language, so
agreement is only real if both are green against the identical checked-in
document; a change to one runner's canned answer with no matching edit here
(or in the Go file) fails exactly one of the two tests, which is the point.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from elitea_inventory.fixture_graph import FIXTURE_HANDLERS, FixtureGraph

REPO_ROOT = Path(__file__).resolve().parents[4]
FIXTURES_DIR = REPO_ROOT / "conformance" / "provider" / "fixtures" / "inventory"


def _fixture_files(*parts: str) -> list[Path]:
    directory = FIXTURES_DIR.joinpath(*parts)
    return sorted(directory.glob("*.json"))


ALL_FIXTURES = _fixture_files("ingestion") + _fixture_files("retrieval")


@pytest.fixture(scope="module")
def graph() -> FixtureGraph:
    return FixtureGraph.load(FIXTURES_DIR)


def _ids(path: Path) -> str:
    return f"{path.parent.name}/{path.name}"


@pytest.mark.parametrize("path", ALL_FIXTURES, ids=_ids)
def test_the_python_fixture_engine_answers_the_golden_shape(graph, path):
    if not ALL_FIXTURES:
        pytest.fail("no ingestion/retrieval fixture files found under " + str(FIXTURES_DIR))

    fixture = json.loads(path.read_text(encoding="utf-8"))
    tool = fixture["tool"]
    params = fixture["params"]
    expected = fixture["expected"]

    handler = FIXTURE_HANDLERS.get(tool)
    assert handler is not None, f"no fixture handler for tool {tool!r}"

    result = handler(graph, params)
    assert result.get("success") is True, f"{tool} refused: {result}"

    if tool == "run_ingestion":
        got = _ingestion_shape(result)
    else:
        # Every retrieval/ingestion-status fixture asks for
        # output_format=json, so `result` is the document's JSON encoding —
        # the shape both parity tests compare, not the markdown rendering.
        got = json.loads(result["result"])

    assert got == expected, f"{tool}: got {got!r}, want {expected!r}"


def _ingestion_shape(result: dict) -> dict:
    """The {result, graph_metadata, sources_status, checkpoint} shape ``ingestion/run_ingestion.json`` records."""
    shape: dict = {"result": result["result"]}
    for artifact in result["artifacts"]:
        name = artifact["name"]
        document = json.loads(artifact["data"])
        if name == "graph.json":
            metadata = document["_metadata"]
            shape["graph_metadata"] = metadata
            shape["source_label"] = metadata["ingested_source"]
        elif name == "sources_status.json":
            shape["sources_status"] = document
        elif name.startswith(".ingestion-checkpoint-"):
            shape["checkpoint"] = document
    return shape


def test_there_is_at_least_one_fixture_of_each_kind():
    """A parity suite that silently collected zero files would always pass."""
    assert _fixture_files("ingestion"), "no ingestion/ fixtures found"
    assert _fixture_files("retrieval"), "no retrieval/ fixtures found"
