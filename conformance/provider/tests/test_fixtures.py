"""Self-checks over the committed DeepWiki conformance fixtures.

These tests need no legacy checkout and no third-party package beyond pytest:
they read the committed JSON and assert the invariants a P1 replay will lean
on.  If one of these fails, the fixture set is internally inconsistent and no
parity claim built on it is trustworthy.

Re-recording the fixtures from the legacy plugin is a separate step
(``tools/record_*.py --check``); see README.md.
"""

from __future__ import annotations

import json
import math
from pathlib import Path

import pytest

# Fixtures are grouped PER PROVIDER now, because this suite is no longer one
# provider's test directory: it is the published contract, and a second
# provider's recordings have to live somewhere that is not inside DeepWiki's.
#
# The generic half of the contract — the SPI path set every provider must serve
# — is NOT here. It is spi/contract.json at the root of this package, because
# it belongs to no single provider.
PROVIDER = "deepwiki"
FIXTURES = Path(__file__).resolve().parents[1] / "fixtures" / PROVIDER


def load(*parts: str):
    return json.loads((FIXTURES.joinpath(*parts)).read_text(encoding="utf-8"))


# ---------------------------------------------------------------------------
# 1. descriptor
# ---------------------------------------------------------------------------


def test_descriptor_matches_spec_inventory():
    """spec-provider-service PVS-06: three toolkits, nine declarations, seven names."""
    inventory = load("descriptor", "legacy-v0", "descriptor.inventory.json")
    assert inventory["provider_name"] == "wikis"
    assert inventory["toolkit_count"] == 3
    assert inventory["declared_tool_count"] == 9
    assert inventory["unique_tool_count"] == 7
    assert [t["name"] for t in inventory["toolkits"]] == [
        "Wikis",
        "wikis_query",
        "wiki_query",
    ]


def test_inventory_is_a_faithful_projection_of_the_descriptor():
    descriptor = load("descriptor", "legacy-v0", "provider_descriptor.json")
    inventory = load("descriptor", "legacy-v0", "descriptor.inventory.json")

    declared = [
        (toolkit["name"], tool["name"])
        for toolkit in descriptor["provided_toolkits"]
        for tool in toolkit["provided_tools"]
    ]
    projected = [
        (toolkit["name"], tool["name"])
        for toolkit in inventory["toolkits"]
        for tool in toolkit["declared_tools"]
    ]
    assert declared == projected

    for toolkit in descriptor["provided_toolkits"]:
        for tool in toolkit["provided_tools"]:
            # Every tool declares both invocation modes; the service serves only
            # the async one (see fixtures/spi/invoke.post.json).
            assert tool["sync_invocation_supported"] is True
            assert tool["async_invocation_supported"] is True
            assert tool["tool_result_type"] == "String"


def test_required_arguments_are_pinned():
    inventory = load("descriptor", "legacy-v0", "descriptor.inventory.json")
    required = {}
    for toolkit in inventory["toolkits"]:
        for tool in toolkit["declared_tools"]:
            key = f"{toolkit['name']}.{tool['name']}"
            required[key] = sorted(
                name for name, arg in tool["args_schema"].items() if arg["required"]
            )

    assert required["Wikis.generate_wiki"] == ["query"]
    assert required["Wikis.ask"] == ["question"]
    assert required["Wikis.deep_research"] == ["question"]
    assert required["wikis_query.ask"] == ["question"]
    assert required["wiki_query.list_wikis"] == []
    assert required["wiki_query.resolve_and_ask"] == ["question"]
    assert required["wiki_query.delete_wiki"] == ["wiki_id"]


def test_bundle_manifest_pins_the_legacy_v0_documents():
    manifest = load("descriptor", "legacy-v0", "bundle.manifest.json")
    assert manifest["source_revision"], "the legacy revision must be pinned"
    names = {Path(doc["path"]).name for doc in manifest["legacy_v0_schema_documents"]}
    assert names == {
        "ExternalServiceProviderDescriptor.json",
        "epam_ai_run.spi.json",
        "epam_ai_run.spi.schema.json",
    }
    for doc in manifest["legacy_v0_schema_documents"]:
        assert doc.get("sha256"), f"{doc['path']} has no digest"


# ---------------------------------------------------------------------------
# 2. SPI
# ---------------------------------------------------------------------------


def test_route_table_covers_every_spi_operation():
    routes = load("spi", "routes.json")
    table = {route["path"]: route for route in routes["routes"]}
    assert set(table) >= {
        "/descriptor",
        "/health",
        "/slots",
        "/tools/{toolkit_name}/{tool_name}/invoke",
        "/tools/{toolkit_name}/{tool_name}/invocations/{invocation_id}",
    }
    assert table["/tools/{toolkit_name}/{tool_name}/invoke"]["methods"] == ["POST"]
    assert table["/tools/{toolkit_name}/{tool_name}/invocations/{invocation_id}"][
        "methods"
    ] == ["GET", "DELETE"]
    # /descriptor and /slots are DeepWiki extensions, not SPI operations.
    assert table["/descriptor"]["in_legacy_spi_openapi"] is False
    assert table["/slots"]["in_legacy_spi_openapi"] is False


def test_invoke_is_async_only_and_returns_an_invocation_id():
    invoke = load("spi", "invoke.post.json")
    assert invoke["accepted"]["status_code"] == 200
    assert invoke["accepted"]["body"]["status"] == "Started"
    assert invoke["accepted"]["body"]["invocation_id"]
    assert invoke["rejected_when_task_start_fails"]["status_code"] == 500
    assert invoke["malformed_json"]["status_code"] == 400


def test_invocation_status_projection_and_404s():
    inv = load("spi", "invocations.get.json")
    assert inv["status_projection"]["pending"] == "Started"
    assert inv["status_projection"]["running"] == "InProgress"
    assert inv["get"]["unknown_invocation"]["status_code"] == 404
    assert inv["get"]["unknown_invocation"]["body"]["errorCode"] == "404"
    assert inv["get"]["completed"]["body"]["status"] == "Completed"
    assert inv["get"]["completed"]["body"]["result_type"] == "String"

    delete = load("spi", "invocations.delete.json")
    assert delete["known_invocation"]["status_code"] == 204
    assert delete["known_invocation"]["body"] is None
    assert delete["state_after"]["stop_requested"] is True
    assert delete["unknown_invocation"]["status_code"] == 404


def test_custom_events_are_drained_on_read():
    inv = load("spi", "invocations.get.json")
    events = inv["get"]["running_with_events"]["body"]["custom_events"]
    assert events == [
        {"data": {"message": "Cloning repository"}},
        {"data": {"message": "Indexing 128 files"}},
    ]
    # The second poll of the same invocation must not repeat them.
    assert "custom_events" not in inv["get"]["running_after_drain"]["body"]

    envelope = load("spi", "custom_events.json")["envelope"]
    assert list(envelope) == ["custom_events"]
    assert list(envelope["custom_events"][0]) == ["data"]
    assert list(envelope["custom_events"][0]["data"]) == ["message"]


def test_toolkit_aliases_include_every_advertised_name():
    aliases = load("spi", "toolkit_aliases.json")
    accepted = set()
    for names in aliases["accepted_toolkit_names"].values():
        accepted.update(names)
    for declared in aliases["declared_toolkit_names"]:
        assert declared in accepted, f"{declared} is advertised but not accepted"
    assert aliases["tools_per_family"]["wiki_query"] == [
        "list_wikis",
        "resolve_and_ask",
        "resolve_and_deep_research",
        "delete_wiki",
    ]


def test_slots_always_carries_the_canStart_alias():
    slots = load("spi", "slots.get.json")
    recorded = slots["cases"]["subprocess_without_worker_pool_module"]["recorded"]["body"]
    assert recorded["can_start"] == recorded["canStart"]
    for case in slots["cases"].values():
        body = case.get("body") or case.get("recorded", {}).get("body", {})
        assert "canStart" in body


def test_error_contract_is_http_200_with_status_error():
    errors = load("spi", "errors.json")
    for label, payload in errors["recorded"].items():
        assert payload["status"] == "Error"
        assert payload["result_type"] == "String"
        assert payload["error_category"] == label
        objects = json.loads(payload["result"])
        assert objects and objects[0]["object_type"] == "message"
        assert objects[0]["result_target"] == "response"


# ---------------------------------------------------------------------------
# 3. retrieval
# ---------------------------------------------------------------------------

QUERY_FILES = sorted((FIXTURES / "retrieval" / "sample-repo" / "queries").glob("*.json"))


def test_there_are_query_fixtures():
    assert QUERY_FILES, "no retrieval query fixtures were recorded"


def test_embeddings_are_unit_length_and_cover_every_node():
    nodes = load("retrieval", "sample-repo", "nodes.json")
    model = load("retrieval", "sample-repo", "embedding_model.json")
    dim = model["embedder"]["dim"]

    node_ids = {node["node_id"] for node in nodes["nodes"]}
    assert set(model["vectors"]) == node_ids
    assert nodes["node_count"] == len(nodes["nodes"])

    for node_id, vector in model["vectors"].items():
        assert len(vector) == dim, node_id
        norm = math.sqrt(sum(value * value for value in vector))
        assert norm == pytest.approx(1.0, abs=1e-4), node_id


@pytest.mark.parametrize("path", QUERY_FILES, ids=lambda p: p.stem)
def test_query_fixture_records_all_four_branches_with_scores(path: Path):
    payload = json.loads(path.read_text(encoding="utf-8"))
    rankings = payload["rankings"]
    assert set(rankings) == {"fts", "dense", "bm25", "fused"}

    score_key = {
        "fts": "fts_rank",
        "dense": "vec_distance",
        "bm25": "bm25_score",
        "fused": "combined_score",
    }
    for branch, key in score_key.items():
        for row in rankings[branch]["results"]:
            assert key in row, f"{branch} result without {key}: {row}"


@pytest.mark.parametrize("path", QUERY_FILES, ids=lambda p: p.stem)
def test_rankings_are_ordered(path: Path):
    rankings = json.loads(path.read_text(encoding="utf-8"))["rankings"]

    fts = [row["fts_rank"] for row in rankings["fts"]["results"]]
    assert fts == sorted(fts), "FTS5 orders by rank ascending (more negative first)"

    dense = [row["vec_distance"] for row in rankings["dense"]["results"]]
    assert dense == sorted(dense), "KNN orders by distance ascending"

    bm25 = [row["bm25_score"] for row in rankings["bm25"]["results"]]
    assert bm25 == sorted(bm25, reverse=True), "BM25 orders by score descending"

    fused = [row["combined_score"] for row in rankings["fused"]["results"]]
    assert fused == sorted(fused, reverse=True), "RRF orders by combined score descending"


@pytest.mark.parametrize("path", QUERY_FILES, ids=lambda p: p.stem)
def test_fused_scores_are_reproducible_from_the_component_rankings(path: Path):
    """The RRF formula, recomputed — this is the gate for the PostgreSQL port.

    A tsvector/dense-store implementation is free to differ in how it produces the
    FTS and dense lists, but once it has them the fusion must be exactly this.
    """
    payload = json.loads(path.read_text(encoding="utf-8"))
    params = payload["parameters"]
    rankings = payload["rankings"]

    k = params["rrf_constant"]
    scores = {}
    for position, row in enumerate(rankings["fts"]["results"], start=1):
        scores[row["node_id"]] = scores.get(row["node_id"], 0.0) + params[
            "fts_weight"
        ] / (k + position)
    for position, row in enumerate(rankings["dense"]["results"], start=1):
        scores[row["node_id"]] = scores.get(row["node_id"], 0.0) + params[
            "vec_weight"
        ] / (k + position)

    for row in rankings["fused"]["results"]:
        assert row["node_id"] in scores, row["node_id"]
        # scores are rounded to 9 decimal places when recorded
        assert row["combined_score"] == pytest.approx(scores[row["node_id"]], abs=1e-9)

    expected_top = sorted(scores.values(), reverse=True)[: params["top_k"]]
    recorded_top = [row["combined_score"] for row in rankings["fused"]["results"]]
    assert recorded_top == pytest.approx(expected_top, abs=1e-9)


@pytest.mark.parametrize("path", QUERY_FILES, ids=lambda p: p.stem)
def test_every_ranked_node_exists_in_the_corpus(path: Path):
    nodes = load("retrieval", "sample-repo", "nodes.json")
    node_ids = {node["node_id"] for node in nodes["nodes"]}
    rankings = json.loads(path.read_text(encoding="utf-8"))["rankings"]
    for branch, ranking in rankings.items():
        for row in ranking["results"]:
            assert row["node_id"] in node_ids, f"{branch}: unknown node {row['node_id']}"


def test_bm25_is_not_an_input_to_the_recorded_fusion():
    """The standalone BM25 index is recorded but is NOT fused by the legacy code.

    Pinning this stops a P1 implementation from "improving" the fusion into a
    three-way RRF and calling the result parity.
    """
    for path in QUERY_FILES:
        fused = json.loads(path.read_text(encoding="utf-8"))["rankings"]["fused"]
        assert "not an input" in fused["note"]


# ---------------------------------------------------------------------------
# 4. generation
# ---------------------------------------------------------------------------


def test_composed_generate_wiki_result_has_the_frozen_object_set():
    composed = load("generation", "composed_result.json")
    assert composed["response"]["status"] == "Completed"
    assert composed["response"]["result_type"] == "String"

    objects = composed["result_objects"]
    assert objects[0]["object_type"] == "message"
    assert objects[0]["result_target"] == "response"

    types_seen = [obj["object_type"] for obj in objects]
    assert types_seen.count("wiki_structure") == 1
    assert types_seen.count("wiki_manifest") == 1
    assert types_seen.count("repository_context") == 1
    assert types_seen.count("wiki_page") >= 1

    for obj in objects[1:]:
        assert obj["result_target"] == "artifact"
        assert obj["result_bucket"] == "wiki-artifacts"
        assert obj["result_encoding"] == "plain"
        assert obj["name"]


def test_page_names_agree_across_structure_manifest_and_result():
    pages = load("generation", "page_names.json")
    manifest = load("generation", "wiki_manifest.json")
    composed = load("generation", "composed_result.json")

    from_pages = [page["artifact_name"] for page in pages["pages"]]
    from_result = [
        obj["name"]
        for obj in composed["result_objects"]
        if obj["object_type"] == "wiki_page"
    ]
    assert from_pages == manifest["body"]["pages"] == from_result

    wiki_id = pages["wiki_id"]
    for name in from_pages:
        assert name.startswith(f"{wiki_id}/wiki_pages/")
        assert name.endswith(".md")


def test_manifest_and_registry_agree_on_identity():
    manifest = load("generation", "wiki_manifest.json")["body"]
    registry = load("generation", "registry_entry.json")

    assert registry["entry"]["id"] == manifest["wiki_id"]
    assert registry["entry"]["folder_path"] == f"{manifest['wiki_id']}/"
    assert registry["entry"]["analysis_key"] == manifest["analysis_key"]
    assert manifest["analysis_key"] == (
        f"{manifest['canonical_repo_identifier']}@{manifest['wiki_version_id']}"
    )
    assert manifest["schema_version"] == 2
    assert registry["registry_body"]["schema_version"] == 1
    assert registry["registry_body"]["wikis"] == [registry["entry"]]


def test_wiki_structure_body_shape():
    structure = load("generation", "wiki_structure.json")["body"]
    assert set(structure) == {"wiki_title", "sections"}
    assert structure["sections"], "at least one section"
    for section in structure["sections"]:
        assert set(section) == {"section_name", "pages"}
        for page in section["pages"]:
            assert set(page) == {"page_name", "page_content"}
            assert page["page_name"].endswith(".md")
