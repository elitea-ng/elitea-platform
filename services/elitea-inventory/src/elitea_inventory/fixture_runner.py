"""A canned engine, for a stack that proves the socket hop without the closure.

It answers the same result dict the real runner does — a text result and an
artifact list — so the Go host's composition and upload path can be exercised
end to end on a stack with no LLM, no repository and no ~700 MB of engine
dependencies.

The canned answers come from :mod:`elitea_inventory.fixture_graph`: the SAME
six-entity graph the Go host's OWN fixture runner
(``services/elitea-subapp-host/internal/apps/inventory/run/fixture.go``) uses
for the E2E stack. Before this module read that graph it answered an empty
one and a sentence saying so — correct for "the hop works", useless for
"the standalone-full stack has something to look at". Every result still says
``[fixture]`` somewhere it is visible, so it can never be mistaken for the
real engine.
"""

from __future__ import annotations

import asyncio
import logging
from typing import Any

from .fixture_graph import FIXTURE_HANDLERS, FixtureGraph
from .tools_table import FAMILIES
from .v1_overrides import DEFERRED_TOOLS, DeferredTool

logger = logging.getLogger(__name__)


class FixtureToolRunner:
    """Paced progress and a canned result, from the shared fixture graph.

    The graph loads ONCE per process (``FixtureGraph.load`` reads a small
    JSON file, but there is no reason to re-read it per invocation) and a
    load failure is deferred to the first call rather than raised at
    construction — a sidecar built without the fixtures directory must still
    start and refuse every tool, the same rule ``UnavailableToolRunner``
    states for a missing engine closure.
    """

    name = "fixture"

    def __init__(self, settings=None) -> None:
        self._settings = settings
        self._step = float(getattr(settings, "fixture_step_seconds", 0.0) or 0.0)
        self._fixtures_path = getattr(settings, "fixtures_path", None)
        self._graph: FixtureGraph | None = None

    def _load_graph(self) -> FixtureGraph:
        if self._graph is None:
            self._graph = FixtureGraph.load(self._fixtures_path)
        return self._graph

    async def run_engine_tool(
        self, tool_name: str, arguments: dict[str, Any], context: Any
    ) -> Any:
        family = arguments.get("family") or "inventory"
        table = FAMILIES.get(family)
        if table is None:
            raise ValueError(
                f"Unknown toolkit: {family}. Expected: inventory or inventory_search"
            )
        if tool_name in DEFERRED_TOOLS:
            raise DeferredTool(
                f"'{tool_name}' is not available: it was "
                f"{DEFERRED_TOOLS[tool_name]}, so no implementation has ever "
                f"run on this platform."
            )
        if tool_name not in table:
            raise ValueError(
                f"Unknown tool: {tool_name}. Available: {', '.join(sorted(table))}"
            )

        for step in (f"Received {tool_name}", "Reading the graph", "Done"):
            await context.checkpoint()
            await context.thinking(step)
            if self._step:
                await asyncio.sleep(self._step)

        params = arguments.get("params") or {}
        handler = FIXTURE_HANDLERS.get(tool_name)
        if handler is None:
            # Admitted by the descriptor and the table above, but nothing
            # canned answers it yet — reachable only for a tool this module
            # has not been extended to cover. Deliberately not a silent
            # generic success: a fixture with nothing to say about a tool
            # says exactly that, the same rule fixtureUnwritten states in the
            # Go runner.
            return {
                "success": False,
                "error": (
                    f"the fixture runner has no canned answer for '{tool_name}'; "
                    f"run this deployment against the engine"
                ),
                "error_category": "resource_not_found",
            }

        graph = self._load_graph()
        result = handler(graph, params)
        if "artifacts" not in result:
            result = {**result, "artifacts": []}
        return result

    async def publish(self, result: dict[str, Any], context: Any) -> None:
        return None


__all__ = ["FixtureToolRunner"]
