"""The canned graph: loading, the packaged-copy pin, and each tool's answer.

``test_fixture_parity.py`` is the cross-language anchor (the golden files also
``fixture_parity_test.go`` reads). This file is everything that test does not
cover: loading from a directory, the ``output_format`` switch, refusals for a
missing entity/preset, and the handlers no golden file pins.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from elitea_inventory.fixture_graph import (
    FIXTURE_HANDLERS,
    FixtureGraph,
    answer,
    entity_ref,
    graph_document,
    not_found,
    source_label_for,
)

REPO_ROOT = Path(__file__).resolve().parents[4]
CONFORMANCE_DIR = REPO_ROOT / "conformance" / "provider" / "fixtures" / "inventory"
PACKAGED_DIR = Path(__file__).resolve().parents[2] / "src" / "elitea_inventory" / "fixtures" / "inventory"


@pytest.fixture(scope="module")
def graph() -> FixtureGraph:
    return FixtureGraph.load()


# -- the packaged copy is not allowed to drift -------------------------------


def test_the_packaged_copy_is_byte_identical_to_the_conformance_fixture():
    """The two-runner trap, made mechanical for THIS language's two copies.

    A wheel cannot read outside its own package, so the graph is duplicated:
    once under conformance/ (what fixture_parity_test.go and this service's
    own parity test read) and once packaged (what a built image serves by
    default). Editing only one of them is exactly the drift this test exists
    to catch.
    """
    conformance = (CONFORMANCE_DIR / "spi" / "graph.json").read_text(encoding="utf-8")
    packaged = (PACKAGED_DIR / "spi" / "graph.json").read_text(encoding="utf-8")
    assert packaged == conformance


def test_loading_the_default_reads_the_packaged_copy(graph):
    assert len(graph.entities) == 6
    assert len(graph.relations) == 5
    assert graph.source_names() == ["code", "docs"]


def test_loading_an_explicit_directory_reads_the_conformance_copy():
    explicit = FixtureGraph.load(CONFORMANCE_DIR)
    assert [e.id for e in explicit.entities] == [e.id for e in FixtureGraph.load().entities]


# -- lookups ------------------------------------------------------------------


def test_lookup_finds_by_id_and_by_name_case_insensitively(graph):
    assert graph.lookup("code:checkout-service") is not None
    assert graph.lookup("CheckoutService") is not None  # by name, exact case
    assert graph.lookup("checkoutservice") is not None  # by name, case-insensitive
    assert graph.lookup("check") is None  # names are matched whole, not fuzzy
    assert graph.lookup("") is None
    assert graph.lookup("no-such-id") is None


def test_source_of_is_empty_for_an_unknown_id(graph):
    assert graph.source_of("code:checkout-service") == "code"
    assert graph.source_of("nope") == ""


def test_match_with_an_empty_query_matches_everything(graph):
    assert len(graph.match("")) == len(graph.entities)


# -- entity_ref / answer / not_found ------------------------------------------


def test_entity_ref_reads_every_name_the_descriptor_uses():
    assert entity_ref({"entity_id": "a"}) == "a"
    assert entity_ref({"entity_name": "b"}) == "b"
    assert entity_ref({"entity": "c"}) == "c"
    assert entity_ref({"id": "d"}) == "d"
    assert entity_ref({}) == ""


def test_answer_is_markdown_by_default_and_json_on_request():
    markdown = answer({}, {"k": "v"}, "# hi\n")
    assert markdown == {"success": True, "result": "# hi\n"}

    as_json = answer({"output_format": "json"}, {"k": "v"}, "# hi\n")
    assert as_json["success"] is True
    assert json.loads(as_json["result"]) == {"k": "v"}

    # case-insensitive, the same rule the Go runner's fixtureAnswer states
    also_json = answer({"output_format": "JSON"}, {"k": "v"}, "text")
    assert json.loads(also_json["result"]) == {"k": "v"}


def test_not_found_names_the_ref_it_could_not_find():
    result = not_found({"entity_id": "missing-1"}, "entity")
    assert result == {
        "success": False,
        "error": "No entity 'missing-1' in this graph.",
        "error_category": "resource_not_found",
    }


def test_source_label_for_mirrors_the_go_runners_derivation():
    assert source_label_for({"source": {"type": "github", "id": "acme"}}) == "github:acme"
    assert source_label_for({"source": {"type": "github", "toolkit_id": 7}}) == "github:7"
    assert source_label_for({"source": {"type": "github"}}) == "github"
    assert source_label_for({"source": {"id": "acme"}}) == "acme"
    assert source_label_for({}) == "fixture-source"
    assert source_label_for({"source": "not-a-dict"}) == "fixture-source"


def test_graph_document_carries_fixture_metadata(graph):
    doc = graph_document(graph, "github:acme")
    assert doc["_metadata"]["ingested_source"] == "github:acme"
    assert doc["_metadata"]["node_count"] == len(graph.entities)
    assert doc["_metadata"]["edge_count"] == len(graph.relations)
    assert len(doc["nodes"]) == len(graph.entities)
    assert len(doc["edges"]) == len(graph.relations)


# -- every handler answers SOMETHING, including refusals ---------------------


def test_every_fixture_handler_answers_success_or_a_refusal(graph):
    """Mirrors the Go runner's ``TestEveryFixtureToolAnswersSomething``.

    A blank params dict is a request with no id, no query and no preset — the
    worst case every handler must survive without raising.
    """
    for name, handler in FIXTURE_HANDLERS.items():
        result = handler(graph, {})
        assert isinstance(result, dict), name
        assert "success" in result, name
        if result["success"]:
            assert isinstance(result.get("result"), str), name
        else:
            assert result.get("error_category") == "resource_not_found", name


@pytest.mark.parametrize("tool", ["get_entity", "get_entity_details", "get_entity_content", "get_related_entities", "get_entity_neighbors", "impact_analysis"])
def test_a_missing_entity_is_refused_not_answered_empty(graph, tool):
    result = FIXTURE_HANDLERS[tool](graph, {"entity_id": "does-not-exist"})
    assert result["success"] is False
    assert result["error_category"] == "resource_not_found"


def test_a_missing_preset_is_refused(graph):
    result = FIXTURE_HANDLERS["get_preset_info"](graph, {"preset": "does-not-exist"})
    assert result["success"] is False
    assert result["error_category"] == "resource_not_found"


def test_get_preset_info_answers_a_known_preset(graph):
    result = FIXTURE_HANDLERS["get_preset_info"](graph, {"preset": "code", "output_format": "json"})
    assert json.loads(result["result"]) == {
        "preset": "code",
        "description": "Parsers for source files, one entity per class and function.",
    }


def test_get_entities_by_ids_names_what_it_could_not_find(graph):
    result = FIXTURE_HANDLERS["get_entities_by_ids"](
        graph, {"entity_ids": ["code:checkout-service", "no-such-id"], "output_format": "json"},
    )
    document = json.loads(result["result"])
    assert [e["id"] for e in document["entities"]] == ["code:checkout-service"]
    assert document["missing"] == ["no-such-id"]


def test_list_entities_by_type_filters_when_a_type_is_given(graph):
    result = FIXTURE_HANDLERS["list_entities_by_type"](graph, {"entity_type": "document", "output_format": "json"})
    document = json.loads(result["result"])
    assert document["total"] == 2
    assert {e["type"] for e in document["entities"]} == {"document"}


def test_list_entities_by_type_with_no_filter_answers_everything(graph):
    result = FIXTURE_HANDLERS["list_entities_by_type"](graph, {"output_format": "json"})
    document = json.loads(result["result"])
    assert document["total"] == len(graph.entities)


def test_search_family_alias_answers_the_same_graph(graph):
    a = FIXTURE_HANDLERS["search_graph"](graph, {"query": "payment", "output_format": "json"})
    b = FIXTURE_HANDLERS["search_knowledge_graph"](graph, {"query": "payment", "output_format": "json"})
    assert a == b


def test_investigate_cites_the_entities_it_matched(graph):
    result = FIXTURE_HANDLERS["investigate"](graph, {"question": "payment", "output_format": "json"})
    document = json.loads(result["result"])
    assert "code:payment-client" in document["entities"]


def test_run_ingestion_lands_graph_sources_and_checkpoint_artifacts(graph):
    result = FIXTURE_HANDLERS["run_ingestion"](graph, {"source": {"type": "github", "id": "acme-widgets"}})
    names = {a["name"] for a in result["artifacts"]}
    assert names == {"graph.json", "sources_status.json", ".ingestion-checkpoint-github:acme-widgets.json"}


def test_remove_source_entities_counts_the_named_source(graph):
    result = FIXTURE_HANDLERS["remove_source_entities"](graph, {"toolkit_id": "docs", "output_format": "json"})
    document = json.loads(result["result"])
    assert document == {"source": "docs", "removed_entities": 2}


def test_cache_and_maintenance_tools_answer_a_document(graph):
    for tool in ("get_cache_stats", "cleanup_cache", "get_ingestion_status", "normalize_types", "rebuild_indices", "list_graphs", "load_graph", "get_graph_info"):
        result = FIXTURE_HANDLERS[tool](graph, {"output_format": "json"})
        assert result["success"] is True
        json.loads(result["result"])  # must be valid JSON
