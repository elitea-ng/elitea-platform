"""The wiki_query family's engine-side resolver.

No langchain here: ``resolve_wiki`` takes its model factory by injection for
the same reason ``LegacyToolRunner`` takes its tools by injection — so the
port's own logic is testable without the engine's ~1.1 GB closure, and so
what is asserted is the port and not the SDK.
"""

from __future__ import annotations

import pytest

from elitea_deepwiki.fixture_runner import FIXTURE_TOOLS
from elitea_deepwiki.sidecar import ENGINE_TOOLS
from elitea_deepwiki.wiki_query import RESOLUTION_PROMPT, resolve_wiki

WIKIS = [
    {"wiki_id": "acme--notes-service--main", "wiki_title": "notes-service (main)",
     "description": "The notes service."},
    {"wiki_id": "acme--billing--release-2", "wiki_title": "billing (release-2)", "description": ""},
]

SETTINGS = {
    "model_name": "gpt-4o",
    "api_base": "http://elitea/llm/v1",
    "api_key": "<redacted>",
    "organization": "90200",
}


class _Answer:
    def __init__(self, content):
        self.content = content


class _Model:
    """A chat model that records its prompt and answers a fixed content."""

    def __init__(self, content):
        self._content = content
        self.prompt = None

    def invoke(self, prompt):
        self.prompt = prompt
        return _Answer(self._content)


def factory_for(model, recorder=None):
    def build(**kwargs):
        if recorder is not None:
            recorder.update(kwargs)
        return model
    return build


def test_the_sidecar_routes_resolve_wiki():
    """The regression's engine half: a tool the host calls must be routable."""
    assert "resolve_wiki" in ENGINE_TOOLS
    assert "resolve_wiki" in FIXTURE_TOOLS


def test_it_answers_the_id_the_model_named():
    model = _Model("acme--notes-service--main")
    result = resolve_wiki(
        question="How does notes-service store notes?",
        wikis=WIKIS,
        llm_settings=SETTINGS,
        llm_factory=factory_for(model),
    )
    assert result == {"success": True, "wiki_id": "acme--notes-service--main"}
    # The prompt is the legacy one, and it carries every candidate.
    assert model.prompt.startswith(RESOLUTION_PROMPT.split("\n", 1)[0])
    assert "- acme--notes-service--main: notes-service (main) - The notes service." in model.prompt
    assert "- acme--billing--release-2: billing (release-2) - " in model.prompt


@pytest.mark.parametrize(
    "content,expected",
    [
        ('"acme--billing--release-2"', "acme--billing--release-2"),
        ("'acme--billing--release-2'", "acme--billing--release-2"),
        ("  acme--billing--release-2\n", "acme--billing--release-2"),
        ("NONE", "NONE"),
    ],
)
def test_it_strips_what_a_model_wraps_the_id_in(content, expected):
    result = resolve_wiki(question="?", wikis=WIKIS, llm_settings=SETTINGS,
                          llm_factory=factory_for(_Model(content)))
    assert result["wiki_id"] == expected


def test_it_reads_anthropic_content_blocks():
    """The legacy defect this port does not carry.

    ChatAnthropic answers a LIST of content blocks. Legacy called `.strip()`
    on it, raised AttributeError into its own `except`, and returned None —
    so every Anthropic-backed resolution silently answered "no wiki matched".
    """
    model = _Model([{"type": "text", "text": "acme--notes-service--main"}])
    result = resolve_wiki(question="?", wikis=WIKIS, llm_settings=SETTINGS,
                          llm_factory=factory_for(model))
    assert result == {"success": True, "wiki_id": "acme--notes-service--main"}


def test_the_model_is_never_hardcoded():
    """Substitution 1: no `gpt-4o-mini` fallback.

    A hardcoded model on a platform whose models are configured per project
    is a name the gateway usually cannot route; the 404 that follows reads
    as "no wiki matched", which is a wrong answer that looks right.
    """
    result = resolve_wiki(
        question="?", wikis=WIKIS,
        llm_settings={"api_base": "http://elitea/llm/v1", "api_key": "k"},
        llm_factory=factory_for(_Model("acme--notes-service--main")),
    )
    assert result["success"] is False
    assert "model_name" in result["error"]
    assert result["error_category"] == "invalid_input"


def test_the_model_settings_come_from_the_request():
    recorded: dict = {}
    resolve_wiki(question="?", wikis=WIKIS,
                 llm_settings={**SETTINGS, "provider": "anthropic", "max_tokens": 999},
                 llm_factory=factory_for(_Model("NONE"), recorded))
    assert recorded["provider"] == "anthropic"
    assert recorded["model_name"] == "gpt-4o"
    assert recorded["api_base"] == "http://elitea/llm/v1"
    assert recorded["api_key"] == "<redacted>"
    assert recorded["organization"] == "90200"
    assert recorded["max_tokens"] == 999
    # Temperature 0: a resolver picking a different repository on a re-ask
    # would make the same question answerable from two wikis.
    assert recorded["temperature"] == 0


def test_no_credentials_is_refused_before_a_model_is_built():
    built = []

    def factory(**kwargs):
        built.append(kwargs)
        raise AssertionError("a model was built with no credentials")

    result = resolve_wiki(question="?", wikis=WIKIS,
                          llm_settings={"model_name": "gpt-4o"}, llm_factory=factory)
    assert result["success"] is False and not built


def test_no_candidates_resolves_to_none_without_asking_a_model():
    built = []

    def factory(**kwargs):
        built.append(kwargs)
        raise AssertionError("a model was asked to choose between no candidates")

    assert resolve_wiki(question="?", wikis=[], llm_settings=SETTINGS,
                        llm_factory=factory) == {"success": True, "wiki_id": "NONE"}
    assert not built


def test_a_model_failure_is_reported_not_raised():
    """The host turns this into "could not determine which wiki to query".

    The caller's request was well formed; a model that would not answer is
    not a reason to fail their invocation, and legacy did the same with the
    `None` its own `except` produced.
    """
    def factory(**kwargs):
        raise RuntimeError("gateway refused")

    result = resolve_wiki(question="?", wikis=WIKIS, llm_settings=SETTINGS, llm_factory=factory)
    assert result["success"] is False and "gateway refused" in result["error"]


def test_the_fixture_resolver_scores_like_the_go_one():
    """Both fixture tables must resolve the same wiki for the same question.

    A stack that ran the Python sidecar's fixture runner and one that ran
    the Go host's would otherwise answer about different repositories, and
    the browser journey would pass against one of them only.
    """
    resolve = FIXTURE_TOOLS["resolve_wiki"]
    assert resolve(question="What does e2e-service do?", wikis=[
        {"wiki_id": "acme--e2e-service--main", "wiki_title": "E2E Service Wiki"},
        {"wiki_id": "acme--other--main", "wiki_title": "Other"},
    ])["wiki_id"] == "acme--e2e-service--main"
    # One candidate and no overlap: the only wiki there is.
    assert resolve(question="anything at all", wikis=[
        {"wiki_id": "acme--only--main", "wiki_title": ""},
    ])["wiki_id"] == "acme--only--main"
    # Several candidates and no overlap: NONE, never a guess.
    assert resolve(question="zzz", wikis=[
        {"wiki_id": "acme--a--main", "wiki_title": ""},
        {"wiki_id": "acme--b--main", "wiki_title": ""},
    ])["wiki_id"] == "NONE"
