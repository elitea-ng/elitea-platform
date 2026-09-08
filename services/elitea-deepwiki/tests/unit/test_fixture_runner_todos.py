"""The fixture deep_research run publishes its plan as a structured event (DWIKI-012b)."""

import json

from elitea_deepwiki.fixture_runner import RESEARCH_TODOS, STEPS, steps_for, todo_update_event


def test_deep_research_publishes_its_plan_before_the_work() -> None:
    steps = steps_for("deep_research")
    structured = [s for s in steps if s.startswith("{")]
    assert len(structured) == 1, "exactly one plan event"
    envelope = json.loads(structured[0])
    assert envelope["event"] == "todo_update"
    items = envelope["data"]["items"]
    assert [i["title"] for i in items] == [t["title"] for t in RESEARCH_TODOS]
    assert all(set(i) == {"id", "title", "description", "status"} for i in items)
    assert steps.index(structured[0]) < steps.index("Reading the relevant pages")


def test_other_tools_carry_no_structured_step() -> None:
    for tool in ("generate_wiki", "ask"):
        assert steps_for(tool) == STEPS[tool]
        assert not any(s.startswith("{") for s in steps_for(tool))


def test_the_placeholder_never_leaks() -> None:
    assert "__RESEARCH_TODOS__" not in steps_for("deep_research")
    assert todo_update_event(()) == json.dumps({"event": "todo_update", "data": {"items": []}})
