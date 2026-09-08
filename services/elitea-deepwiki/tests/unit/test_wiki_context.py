"""``context_paths``: resolution, scope refusal, the budgets, the prepend.

Every case here is read out of the golden fixture
``conformance/provider/fixtures/deepwiki/context/context_paths.json``, which
the Go host's own test reads too. Writing the expectations inline instead
would let the two implementations of this rule set drift with both suites
green — which is the failure mode the fixture exists to prevent.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from elitea_deepwiki import wiki_context


FIXTURE = (
    Path(__file__).resolve().parents[4]
    / "conformance"
    / "provider"
    / "fixtures"
    / "deepwiki"
    / "context"
    / "context_paths.json"
)


@pytest.fixture(scope="module")
def spec() -> dict:
    return json.loads(FIXTURE.read_text(encoding="utf-8"))


class RecordingBucket:
    """The wiki's bucket, and a record of every read attempted against it.

    The read COUNT is load-bearing: several cases assert that a refused
    identifier never reaches the transport at all. A resolver that refused
    after downloading would still pass a message assertion.
    """

    def __init__(self, spec: dict) -> None:
        wiki = spec["wiki"]
        self.wiki_id = wiki["wiki_id"]
        self.objects: dict[str, bytes] = {
            wiki["manifest_key"]: json.dumps(wiki["manifest"]).encode("utf-8")
        }
        for page, body in wiki["pages"].items():
            self.objects[f"{self.wiki_id}/{page}"] = body.encode("utf-8")
        for page, shape in wiki["generated_pages"].items():
            if page.startswith("_"):
                continue
            self.objects[f"{self.wiki_id}/{page}"] = (
                shape["fill"] * shape["chars"]
            ).encode("utf-8")
        self.reads: list[str] = []

    def download(self, bucket: str, key: str) -> bytes:
        self.reads.append(key)
        try:
            return self.objects[key]
        except KeyError:
            raise FileNotFoundError(f"{bucket}/{key} does not exist") from None


def run_case(spec: dict, case: dict) -> tuple[str, RecordingBucket]:
    bucket = RecordingBucket(spec)
    parameters = {
        wiki_context.PATHS_PARAM: case.get("context_paths_raw", case.get("context_paths")),
        wiki_context.VERSION_PARAM: case.get("context_wiki_version_id"),
    }
    block = wiki_context.resolve_context_paths(
        parameters=parameters,
        wiki_id=spec["wiki"]["wiki_id"],
        download=bucket.download,
    )
    return wiki_context.prepend_context(case["question"], block), bucket


def cases(kind: str):
    spec = json.loads(FIXTURE.read_text(encoding="utf-8"))
    for name, case in spec["cases"].items():
        if kind == "refusal" and "refusal_contains" in case:
            yield pytest.param(name, id=name)
        if kind == "accepted" and "refusal_contains" not in case:
            yield pytest.param(name, id=name)


@pytest.mark.parametrize("name", list(cases("accepted")))
def test_an_accepted_selection_renders_the_fixture_s_block(spec: dict, name: str) -> None:
    case = spec["cases"][name]
    enhanced, _ = run_case(spec, case)

    if "enhanced_question" in case:
        # The PREPEND SHAPE, exactly: the lead-in, the section headers, the
        # blank lines and the `Current question: ` hand-off are the engine's
        # own enhanced_question idiom, and a caller reading a transcript has
        # to be able to tell context from question.
        assert enhanced == case["enhanced_question"]

    expect = case.get("expect", {})
    if "sections" in expect:
        assert enhanced.count("--- source: ") == expect["sections"]
    for needle in expect.get("contains", []):
        assert needle in enhanced
    if "body_chars_before_marker" in expect:
        header = "--- source: wiki_pages/bulk/p1.md ---\n"
        body = enhanced.split(header, 1)[1].split("[…", 1)[0]
        # The marker's own two leading newlines are not budget.
        assert len(body) - 2 == expect["body_chars_before_marker"]


@pytest.mark.parametrize("name", list(cases("refusal")))
def test_a_selection_outside_scope_is_refused_and_says_which(spec: dict, name: str) -> None:
    case = spec["cases"][name]
    with pytest.raises(wiki_context.ContextRefused) as raised:
        run_case(spec, case)
    message = str(raised.value)
    for needle in case["refusal_contains"]:
        assert needle in message, message


@pytest.mark.parametrize("name", list(cases("refusal")))
def test_a_refusal_that_promises_no_read_performs_none(spec: dict, name: str) -> None:
    """The SSRF assertion, and the only one that can actually catch it.

    An id that is not a wiki page of this wiki must be refused BEFORE the
    artifact transport is touched. Asserting only on the message would pass
    for a resolver that fetched the client's URL and then complained about
    the answer.
    """
    case = spec["cases"][name]
    if "reads_expected" not in case:
        pytest.skip("this case is refused after a legitimate manifest read")
    bucket = RecordingBucket(spec)
    with pytest.raises(wiki_context.ContextRefused):
        wiki_context.resolve_context_paths(
            parameters={
                wiki_context.PATHS_PARAM: case.get(
                    "context_paths_raw", case.get("context_paths")
                ),
                wiki_context.VERSION_PARAM: case.get("context_wiki_version_id"),
            },
            wiki_id=spec["wiki"]["wiki_id"],
            download=bucket.download,
        )
    assert bucket.reads == [], bucket.reads


def test_the_budgets_match_the_fixture(spec: dict) -> None:
    """A quiet change to either ceiling is a change to what callers pay for."""
    assert wiki_context.TOTAL_BUDGET_CHARS == spec["budget"]["total_chars"]
    assert wiki_context.PER_DOCUMENT_BUDGET_CHARS == spec["budget"]["per_document_chars"]


def test_no_transport_is_refused_rather_than_answered_without_context(spec: dict) -> None:
    """A missing artifact grant must not degrade to an ungrounded answer."""
    with pytest.raises(wiki_context.ContextRefused) as raised:
        wiki_context.resolve_context_paths(
            parameters={
                wiki_context.PATHS_PARAM: ["wiki_pages/components/storage.md"],
                wiki_context.VERSION_PARAM: spec["wiki"]["wiki_version_id"],
            },
            wiki_id=spec["wiki"]["wiki_id"],
            download=None,
        )
    assert "artifacts base_url not configured" in str(raised.value)


def test_consume_spends_both_keys_so_nothing_prepends_twice(spec: dict) -> None:
    """The Go host resolves first and removes these; this is the same rule.

    Two resolvers exist on purpose (the host's, in front of every tool
    table, and this one, for a sidecar called directly). They are only safe
    together because whichever runs first spends the keys.
    """
    left = wiki_context.consume(
        {"question": "q", wiki_context.PATHS_PARAM: ["a"], wiki_context.VERSION_PARAM: "v"}
    )
    assert left == {"question": "q"}


def test_the_wiki_id_derivation_is_the_one_the_fixture_runner_uses(spec: dict) -> None:
    """One derivation in the package; the fixture runner imports this one."""
    from elitea_deepwiki import fixture_runner

    repo_config = spec["wiki"]["repo_config"]
    assert (
        fixture_runner.wiki_id_for(repo_config, repo_config["branch"])
        is not None
    )
    assert (
        wiki_context.wiki_id_for(repo_config, repo_config["branch"])
        == spec["wiki"]["wiki_id"]
    )
