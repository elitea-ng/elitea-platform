"""The four v1 decisions, each where it is actually enforced.

Sources, embeddings, the artifact read path and the deferred tools. Every test
here is about a place the port DIFFERS from the legacy plugin, because those are
the places a copy cannot be trusted and a digest proves nothing.
"""

from __future__ import annotations

import json
import types

import pytest

from elitea_inventory import artifacts, embeddings, sources
from elitea_inventory.config import DEFAULT_SOURCE_TYPES, ConfigError, Settings

ALLOWED = DEFAULT_SOURCE_TYPES


# -- sources: the facade expands, this service never resolves ----------------


def test_a_body_with_no_source_is_refused_as_invalid_input():
    """The legacy handler took a bare ``toolkit_id`` and resolved it itself.

    It did so with an admin token, against a toolkit id the caller supplied and
    nothing checked the caller could see — so the refusal is the security
    boundary, not a validation nicety. ``ValueError`` is the class the contract
    reads as ``invalid_input``.
    """
    with pytest.raises(ValueError) as raised:
        sources.parse_source(None, ALLOWED)
    assert "the facade expands them" in str(raised.value)


def test_a_bare_toolkit_id_is_refused_rather_than_resolved():
    with pytest.raises(ValueError) as raised:
        sources.parse_source(42, ALLOWED)
    assert "not resolved here" in str(raised.value)


@pytest.mark.parametrize("source_type", ["github", "ado_repos", "GitHub", " ADO_Repos "])
def test_the_two_supported_types_are_accepted_case_insensitively(source_type):
    source = sources.parse_source(
        {"type": source_type, "name": "repo", "settings": {"repository": "a/b"}}, ALLOWED
    )
    assert source.type in ALLOWED


@pytest.mark.parametrize("source_type", ["gitlab", "bitbucket", "confluence", "jira"])
def test_every_other_type_is_refused_by_name(source_type):
    """The legacy descriptor advertises four source types; v1 ingests two.

    Named, not attempted: the engine builds the SDK toolkit from type+settings,
    and a type whose settings shape it has never been run against fails
    somewhere inside the SDK with a message about a missing key — which reads
    as a platform bug rather than as "not supported yet".
    """
    with pytest.raises(ValueError) as raised:
        sources.parse_source({"type": source_type, "settings": {}}, ALLOWED)
    assert source_type in str(raised.value)
    assert "github, ado_repos" in str(raised.value)


def test_the_allowlist_is_what_is_enforced_not_a_hardcoded_pair():
    """``ELITEA_INVENTORY_SOURCE_TYPES`` widens it; the refusal follows.

    A deployment that has actually run the engine against another type can
    enable it without a code change, and the refusal message names the
    configured set rather than a constant.
    """
    source = sources.parse_source({"type": "gitlab", "settings": {}}, ("gitlab",))
    assert source.type == "gitlab"
    with pytest.raises(ValueError):
        sources.parse_source({"type": "github", "settings": {}}, ("gitlab",))


def test_a_type_is_required():
    with pytest.raises(ValueError) as raised:
        sources.parse_source({"name": "repo", "settings": {}}, ALLOWED)
    assert "source.type is required" in str(raised.value)


def test_patterns_arrive_as_a_list_or_a_comma_separated_string():
    """The legacy UI stored comma-separated strings; the API sends lists."""
    source = sources.parse_source(
        {
            "type": "github",
            "settings": {},
            "whitelist": "**/*.py, **/*.go",
            "blacklist": ["**/vendor/**"],
        },
        ALLOWED,
    )
    assert source.whitelist == ["**/*.py", "**/*.go"]
    assert source.blacklist == ["**/vendor/**"]


def test_an_empty_pattern_string_is_no_filter_not_an_empty_filter():
    """`""` must mean "everything", as it did in the legacy split.

    An empty LIST reaching the pipeline as a whitelist would match no file at
    all, and the ingestion would report success over zero documents.
    """
    source = sources.parse_source(
        {"type": "github", "settings": {}, "whitelist": "", "blacklist": ",  ,"}, ALLOWED
    )
    assert source.whitelist is None
    assert source.blacklist is None


def test_a_source_with_no_name_is_named_after_its_toolkit():
    """The name keys the checkpoint file and every citation's source_toolkit."""
    source = sources.parse_source({"type": "github", "toolkit_id": 9, "settings": {}}, ALLOWED)
    assert source.name == "toolkit_9"


def test_no_platform_client_is_injected_into_the_toolkit_settings(monkeypatch):
    """The legacy code put an admin-authorised client into ``settings['elitea']``.

    The SDK uses it for artifact and datasource reads on the toolkit's behalf.
    Supplying it here would put the admin credential the port removed back into
    the ingestion path by the side door.
    """
    seen = {}

    def fake_instantiate(toolkit_data):
        seen.update(toolkit_data)
        return types.SimpleNamespace(api_wrapper=object())

    module = types.ModuleType("elitea_sdk.tools")
    module.instantiate_toolkit = fake_instantiate
    monkeypatch.setitem(__import__("sys").modules, "elitea_sdk", types.ModuleType("elitea_sdk"))
    monkeypatch.setitem(__import__("sys").modules, "elitea_sdk.tools", module)

    source = sources.parse_source(
        {
            "type": "github",
            "name": "r",
            "toolkit_id": 3,
            # The credential block the SDK subscripts; build_toolkit refuses a
            # source without one rather than letting the SDK raise KeyError.
            "settings": {"repository": "a/b", "github_configuration": {}},
        },
        ALLOWED,
    )
    sources.build_toolkit(source)
    # What this test is about: the admin-authorised client is NOT injected.
    assert "elitea" not in seen["settings"]
    # The stored settings reach the SDK unaltered. `build_toolkit` adds the two
    # branch keys github requires and nothing else, so the assertion names what
    # it allows instead of pinning the whole dict — an equality here would fail
    # on any required key the SDK adds, which is not what this test measures.
    assert seen["settings"]["repository"] == "a/b"
    assert seen["settings"]["github_configuration"] == {}
    assert set(seen["settings"]) - {"repository", "github_configuration"} == {
        "active_branch",
        "base_branch",
    }


# -- embeddings: the gateway, and the space the graph was built in -----------


class FakeGraph:
    def __init__(self, metadata=None):
        self._metadata = dict(metadata or {})


def test_a_graph_built_with_another_model_refuses_rather_than_answering():
    """Cosine similarity across two embedding spaces is a number that means nothing.

    The legacy plugin could not make this check: it hardcoded one local model,
    so there was nothing to compare — and if that pin had ever moved, every
    semantic search over an old graph would have silently returned noise.
    """
    graph = FakeGraph({embeddings.METADATA_MODEL_KEY: "text-embedding-3-small"})
    with pytest.raises(embeddings.EmbeddingsModelMismatch) as raised:
        embeddings.check(graph, "text-embedding-3-large")
    assert "text-embedding-3-small" in str(raised.value)
    assert "re-ingest" in str(raised.value)


def test_the_matching_model_passes():
    graph = FakeGraph({embeddings.METADATA_MODEL_KEY: "text-embedding-3-small"})
    embeddings.check(graph, "text-embedding-3-small")


def test_a_graph_with_no_recorded_model_is_allowed():
    """Every graph that exists today was built before this stamp existed.

    Refusing them would break the product at upgrade, and their vectors — if
    any — are in the legacy MiniLM space while lexical search still answers.
    """
    embeddings.check(FakeGraph(), "text-embedding-3-small")


def test_a_request_with_no_model_is_allowed():
    """``embedding_model`` is optional in the descriptor and empty by default."""
    graph = FakeGraph({embeddings.METADATA_MODEL_KEY: "text-embedding-3-small"})
    embeddings.check(graph, None)


def test_the_stamp_records_the_platform_model_not_the_client_class():
    """``KnowledgeGraph.generate_embeddings`` stamps the CLASS name.

    For the gateway client that is ``"OpenAIEmbeddings"`` for every platform
    model — identical everywhere, so no mismatch could ever be detected. The
    stamp overwrites it with the model the toolkit configured.
    """
    graph = FakeGraph({embeddings.METADATA_MODEL_KEY: "OpenAIEmbeddings"})
    embeddings.stamp(graph, "text-embedding-3-small", 1536)
    assert embeddings.recorded(graph) == ("text-embedding-3-small", 1536)


def test_the_model_is_read_from_either_settings_shape():
    assert embeddings.resolve_model({"embedding_model": "m"}) == "m"
    assert embeddings.resolve_model({"toolkit_configuration_embedding_model": "m"}) == "m"
    assert embeddings.resolve_model({"embedding_model": "   "}) is None
    assert embeddings.resolve_model({}) is None


def test_a_configured_model_with_no_transport_refuses_rather_than_falling_back():
    """There is no local fallback, and that is the decision.

    Falling back to a local model would put entity vectors in a space the
    gateway never sees, unmetered, in an image that would then have to carry
    torch.
    """
    with pytest.raises(embeddings.EmbeddingsUnavailable):
        embeddings.build("text-embedding-3-small", {})


# -- the artifact read path ---------------------------------------------------


def test_the_llm_suffix_is_stripped_to_reach_the_platform_root():
    """``api_base`` addresses the gateway; the artifact routes are on the root.

    The same derivation the Go host performs (``ExtractArtifactSettings``) —
    two implementations of one legacy rule, so both are pinned.
    """
    for base in (
        "https://elitea.example/llm/v1",
        "https://elitea.example/llm/api/v1",
        "https://elitea.example/llm",
        "https://elitea.example/llm/",
    ):
        settings = artifacts.extract_settings({"api_base": base, "api_key": "k"})
        assert settings["base_url"] == "https://elitea.example"


def test_the_project_comes_from_the_organization_first():
    settings = artifacts.extract_settings(
        {"api_base": "http://x/llm/v1", "api_key": "k", "organization": "7", "project_id": "9"}
    )
    assert settings["project_id"] == "7"


def test_no_transport_means_no_client_rather_than_a_refusal():
    """A direct SPI call carries no llm_settings, and that is a legitimate shape."""
    assert artifacts.client_from({}) is None
    assert artifacts.client_from({"api_base": "http://x/llm/v1"}) is None


def test_the_bucket_is_read_from_either_settings_shape():
    assert artifacts.resolve_bucket({"bucket": "kg"}) == "kg"
    assert artifacts.resolve_bucket({"toolkit_configuration_bucket": "kg"}) == "kg"
    assert artifacts.resolve_bucket({}) == artifacts.DEFAULT_BUCKET


def test_the_download_never_disables_tls_verification():
    """The legacy plugin passed ``verify=False`` on every platform call.

    That is how a bearer token is handed to whatever answers the address. The
    client verifies against the deployment's CA when one is configured, and
    against the system store otherwise — never against nothing.
    """
    client = artifacts.PlatformArtifactClient(
        {"base_url": "https://x", "api_key": "k", "project_id": "1", "api_path": "/api/v2"}
    )
    assert client._verify is True
    client.ca_file = "/etc/ssl/elitea-ca.pem"
    assert client._verify == "/etc/ssl/elitea-ca.pem"


def test_a_missing_object_is_none_and_a_forbidden_one_raises(monkeypatch):
    """404 and 403 are different answers, and the legacy code conflated them.

    It sniffed the response BODY for the string ``"error"`` in its first 100
    characters (``_is_artifact_error``) — so a graph whose own content happened
    to contain that word was silently treated as missing, and the tool answered
    "No graph configured" over a graph that was right there.
    """
    import sys

    responses = {}

    class FakeResponse:
        def __init__(self, status, content=b""):
            self.status_code = status
            self.content = content
            self.text = content.decode()

    fake = types.ModuleType("requests")
    fake.get = lambda url, **_: responses[url.rsplit("/", 1)[-1]]
    monkeypatch.setitem(sys.modules, "requests", fake)

    client = artifacts.PlatformArtifactClient(
        {"base_url": "https://x", "api_key": "k", "project_id": "1", "api_path": "/api/v2"}
    )
    responses["missing.json"] = FakeResponse(404)
    assert client.download("graphs", "missing.json") is None

    responses["secret.json"] = FakeResponse(403, b"nope")
    with pytest.raises(PermissionError):
        client.download("graphs", "secret.json")

    body = json.dumps({"nodes": [{"name": "error handler"}]}).encode()
    responses["graph.json"] = FakeResponse(200, body)
    assert client.download("graphs", "graph.json") == body


# -- settings ------------------------------------------------------------------


def test_source_types_default_to_the_two_v1_supports(monkeypatch):
    monkeypatch.delenv("ELITEA_INVENTORY_SOURCE_TYPES", raising=False)
    assert Settings.from_env().source_types == DEFAULT_SOURCE_TYPES


def test_a_source_type_list_that_names_nothing_is_a_boot_failure(monkeypatch):
    """An unparsable value must not silently take the default.

    Silently defaulting is how a deployment that MEANT to restrict ingestion
    runs wide open while its configuration says otherwise.
    """
    monkeypatch.setenv("ELITEA_INVENTORY_SOURCE_TYPES", " , ,")
    with pytest.raises(ConfigError):
        Settings.from_env()


def test_there_is_no_jobs_setting(monkeypatch):
    """``INVENTORY_JOBS_ENABLED`` is not read, in either spelling.

    v1 runs ingestion in the sidecar. A setting that can be set and does
    nothing is worse than none — the legacy deployments set it, and an operator
    copying that environment forward must not believe Jobs are in use.
    """
    monkeypatch.setenv("INVENTORY_JOBS_ENABLED", "true")
    monkeypatch.setenv("ELITEA_INVENTORY_JOBS_ENABLED", "true")
    settings = Settings.from_env()
    assert not any("job" in field.lower() for field in vars(settings))


def test_the_callback_ca_comes_from_the_settings_and_nowhere_else(monkeypatch):
    """One read of ELITEA_INVENTORY_TLS_CA_FILE, in config.Settings.

    An earlier draft read it in TWO places: the settings field the runner
    passes down, and a fallback inside the client itself. The settings field
    did not exist, so the runner's `getattr(..., None)` always returned None
    and the hidden fallback was the one that applied — which is how a
    configured CA silently stops being the one in force, failing OPEN to the
    system trust store with nothing to say it had.
    """
    monkeypatch.setenv("ELITEA_INVENTORY_TLS_CA_FILE", "/etc/ssl/elitea-ca.pem")
    assert Settings.from_env().tls_ca_file == "/etc/ssl/elitea-ca.pem"

    # The client trusts what it is GIVEN, and reads no environment of its own.
    client = artifacts.PlatformArtifactClient(
        {"base_url": "https://x", "api_key": "k", "project_id": "1", "api_path": "/api/v2"}
    )
    assert client._verify is True


def test_the_runner_carries_the_settings_ca_to_the_client():
    from elitea_inventory.legacy_runner import ToolHost

    settings = Settings(tls_ca_file="/etc/ssl/elitea-ca.pem")
    host = ToolHost(settings, context=None, request_data={})
    assert host.tls_ca_file == "/etc/ssl/elitea-ca.pem"
    assert host.source_types == DEFAULT_SOURCE_TYPES
