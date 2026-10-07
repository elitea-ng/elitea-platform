"""The e2e LLM stub answers naming prompts whose page lists hold non-objects."""

from __future__ import annotations

import importlib.util
import json
from pathlib import Path

_STUB = (
    Path(__file__).resolve().parents[3]
    / "elitea-deepwiki-engine"
    / "testdata"
    / "llm_stub.py"
)


def _stub():
    spec = importlib.util.spec_from_file_location("llm_stub", _STUB)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _prompt(head: str, pages: list) -> str:
    return f"{head}\nPAGES IN THIS SECTION:\n{json.dumps(pages)}\n\nOutput ONLY valid JSON"


def test_batched_naming_skips_items_that_are_not_objects():
    stub = _stub()
    pages = [1, "x", None, {"page_id": "p1", "page_symbols": [{"name": "Foo"}, 3], "symbol_count": 2},
             {"page_id": "p2", "page_symbols": "not a list"}]
    answer = json.loads(stub.naming_answer(_prompt('SECTION CLUSTER — 3 symbols "page_id"', pages)))
    assert [p["page_id"] for p in answer["pages"]] == ["p1", "p2"]
    assert answer["pages"][0]["page_name"] == "Working with Foo"
    assert answer["pages"][0]["retrieval_query"] == "Foo"
    assert answer["pages"][1]["page_name"] == "General Utilities"


def test_section_from_pages_tolerates_a_non_object_first_page():
    stub = _stub()
    answer = json.loads(stub.naming_answer(_prompt("SECTION (derived from its pages)", ["x"])))
    assert answer["section_name"] == "Domain of Overview"
