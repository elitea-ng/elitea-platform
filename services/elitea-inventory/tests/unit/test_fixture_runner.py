"""``FixtureToolRunner``: dispatch, paced progress, and the graph it now serves.

Everything ABOUT the canned answers (shapes, refusals, the ``output_format``
switch) is ``test_fixture_graph.py``'s job and, for the golden subset, the
cross-language ``test_fixture_parity.py``'s. This file is the runner's own
plumbing: family/tool validation, progress pacing, the load-once graph, and
what happens for a tool the table has nothing for.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from elitea_inventory.config import Settings
from elitea_inventory.fixture_runner import FixtureToolRunner
from elitea_inventory.v1_overrides import DEFERRED_TOOLS, DeferredTool

REPO_ROOT = Path(__file__).resolve().parents[4]
CONFORMANCE_DIR = REPO_ROOT / "conformance" / "provider" / "fixtures" / "inventory"


class Context:
    invocation_id = "inv-1"

    def __init__(self) -> None:
        self.thoughts: list[str] = []

    async def thinking(self, message: str) -> None:
        self.thoughts.append(message)

    async def checkpoint(self) -> None:
        return None


def runner(**settings_kwargs) -> FixtureToolRunner:
    return FixtureToolRunner(settings=Settings(**settings_kwargs))


async def run(subject: FixtureToolRunner, tool: str, **arguments):
    return await subject.run_engine_tool(tool, arguments, Context())


async def test_an_unknown_family_is_refused():
    with pytest.raises(ValueError, match="Unknown toolkit: nope"):
        await run(runner(), "get_stats", family="nope", params={})


async def test_a_deferred_tool_is_refused_before_it_ever_reaches_the_graph():
    tool = next(iter(DEFERRED_TOOLS))
    with pytest.raises(DeferredTool):
        await run(runner(), tool, family="inventory", params={})


async def test_a_tool_the_family_does_not_route_is_invalid_input():
    with pytest.raises(ValueError, match="Unknown tool: not-a-real-tool"):
        await run(runner(), "not-a-real-tool", family="inventory", params={})


async def test_a_real_tool_answers_from_the_canned_graph():
    result = await run(runner(), "get_stats", family="inventory", params={"output_format": "json"})
    assert result["success"] is True
    document = json.loads(result["result"])
    assert document["node_count"] == 6
    assert document["edge_count"] == 5


async def test_progress_is_reported_before_the_answer():
    context = Context()
    await FixtureToolRunner(settings=Settings()).run_engine_tool(
        "get_stats", {"family": "inventory", "params": {}}, context,
    )
    assert context.thoughts == ["Received get_stats", "Reading the graph", "Done"]


async def test_the_step_delay_is_honoured():
    import time

    subject = runner(fixture_step_seconds=0.05)
    start = time.monotonic()
    await run(subject, "get_stats", family="inventory", params={})
    # Three steps at 0.05s each: comfortably over one step, comfortably under
    # a flaky test's tolerance for three.
    assert time.monotonic() - start >= 0.05


async def test_run_ingestion_still_uploads_a_graph_artifact():
    result = await run(runner(), "run_ingestion", family="inventory", params={"source": {"type": "github", "id": "x"}})
    assert result["success"] is True
    assert any(a["name"] == "graph.json" for a in result["artifacts"])


async def test_every_result_carries_an_artifacts_list_even_when_empty():
    result = await run(runner(), "get_stats", family="inventory", params={})
    assert result["artifacts"] == []


async def test_settings_fixtures_path_points_the_runner_at_another_directory():
    """``ELITEA_INVENTORY_FIXTURES`` — the standalone-full overlay's escape hatch."""
    subject = runner(fixtures_path=str(CONFORMANCE_DIR))
    result = await run(subject, "get_stats", family="inventory", params={"output_format": "json"})
    document = json.loads(result["result"])
    assert document["node_count"] == 6


async def test_the_graph_loads_once_and_is_reused():
    subject = runner()
    await run(subject, "get_stats", family="inventory", params={})
    first = subject._graph  # noqa: SLF001 - the point of the test is object identity
    await run(subject, "get_entity", family="inventory", params={"entity_id": "code:checkout-service"})
    assert subject._graph is first  # noqa: SLF001
