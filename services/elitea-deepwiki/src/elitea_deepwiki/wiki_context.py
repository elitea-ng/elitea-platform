"""Reader-selected wiki pages as question context (``context_paths``).

WHAT THIS IS. The wiki chat can be given ATTACHMENTS: a reader picks pages
out of the wiki they are reading and asks a question against exactly those,
instead of against whatever retrieval happens to surface. Legacy DeepWiki
never had this; it is new product, not a port.

THE MECHANISM, and why it is this one. The client sends IDENTIFIERS — page
ids, never text and never URLs. This module resolves them to page content
against the wiki's OWN artifact bucket, caps the total, and renders a block
that is PREPENDED to ``question``. Prepending is the engine's own idiom:
``engine/ask_tool.py::ask_with_history`` builds

    Given this conversation context:
    {history}

    Current question: {question}

and hands the result to ``ask`` as if the reader had typed it. This module
builds the same shape with a different lead-in, so there is one convention
for "extra context in front of the question" rather than two.

WHY IDENTIFIERS AND NOT TEXT OR URLS. Text from the client is not context,
it is an unbounded prompt the server would pay for and attribute to the
wiki. A URL — or a repository path — would make the client able to choose
what the server fetches or reads, which is an SSRF/arbitrary-read door;
DeepWiki's egress allowlist exists precisely so the client cannot pick a
destination. An identifier can only ever name something that is already in
this wiki's bucket, under this caller's project, in the version the reader
has open, and everything that does not resolve inside that scope is REFUSED
rather than dropped — a silently dropped attachment is an answer that looks
grounded and is not.

THREE PRODUCT DECISIONS THIS ENCODES.

1. WIKI PAGES ONLY. An arbitrary repository path is not selectable. The
   index deliberately excludes files (tests, vendored trees, binaries), and
   letting a reader name a repo path would leak what indexing chose to skip
   — and would read files the wiki never published.
2. PINNED TO THE OPEN VERSION. ``context_wiki_version_id`` is REQUIRED
   whenever ``context_paths`` is non-empty, and the manifest read is for
   that exact version. A question asked against the version on screen must
   not silently resolve against a newer one, which is what "resolve the
   latest manifest" would do the moment a regeneration lands mid-read.
3. THE WIKI IS THE SERVER'S, NOT THE CLIENT'S. The wiki id is derived from
   ``repo_config`` — the same derivation the rest of the invocation uses —
   so a client cannot name pages of a DIFFERENT wiki. It can only attach
   pages of the wiki it is already asking about.

THE BUDGET is 32k characters total and 8k per document, truncated
deterministically (a prefix, never a middle-out summary) with a visible
marker, so a truncated answer is explainable by reading the block. A page
that gets no budget at all is NAMED in the block rather than dropped, for
the same reason.

TWO IMPLEMENTATIONS, ONE FIXTURE. The Go sub-application host resolves this
before it dispatches to a tool (``internal/apps/deepwiki/run/contextpaths.go``),
because the E2E stack's tool table is Go and never reaches this file. Both
are pinned to the same golden fixture,
``conformance/provider/fixtures/deepwiki/context/context_paths.json``, so
the two cannot drift without a test saying so.
"""

from __future__ import annotations

import json
import re
from typing import Any, Callable, Iterable

from .artifact_source import is_artifact_source, parse_artifact_source

#: The whole block's ceiling, in characters.
TOTAL_BUDGET_CHARS = 32_000

#: One page's ceiling, in characters.
PER_DOCUMENT_BUDGET_CHARS = 8_000

#: The lead-in, matching ``ask_tool.ask_with_history``'s shape.
CONTEXT_LEAD_IN = "Given this selected wiki context:"

#: What separates the block from the question, again matching the engine.
QUESTION_LEAD_IN = "Current question: "

#: The visible truncation marker. It names the limit that applied so a
#: reader of the transcript can tell a per-page cut from a total-budget one.
TRUNCATION_MARKER = "\n\n[… truncated to {limit} characters of context budget]"

#: The parameter names. ``context_paths`` carries the selection;
#: ``context_wiki_version_id`` pins it to the version the reader has open.
PATHS_PARAM = "context_paths"
VERSION_PARAM = "context_wiki_version_id"

#: The keys removed from the argument set once the block is built, so a
#: second resolver downstream cannot prepend the same context twice.
CONSUMED_PARAMS = (PATHS_PARAM, VERSION_PARAM)

#: Bucket the wiki's objects live in. The invoke-time value, not the
#: descriptor's — see run/artifacts.go's note on that legacy disagreement.
DEFAULT_BUCKET = "wiki-artifacts"

#: A page id's permitted shape. This is a SECOND gate, not the only one: the
#: id must also appear in the pinned manifest. It exists so a malformed or
#: hostile id can never be handed to the artifact client at all, including
#: by way of a manifest that was itself written badly. No scheme, no
#: absolute path, no traversal, no empty segment, and it must be a wiki page.
_PAGE_ID = re.compile(r"^wiki_pages(?:/[A-Za-z0-9._][A-Za-z0-9._-]*)+\.md$")

#: A version id is part of an object key, so it is held to the same rule.
_VERSION_ID = re.compile(r"^[A-Za-z0-9._-]{1,128}$")


class ContextRefused(ValueError):
    """A selection that cannot be honoured exactly as asked.

    ``ValueError`` on purpose: the host maps it to ``invalid_input`` (see
    ``run.EngineError`` and the legacy error contract), so the reader is
    told which id was refused instead of being handed a grounded-looking
    answer that quietly used fewer pages than they picked.
    """


def display_repository_for(repo_config: dict[str, Any] | None) -> str:
    """The repository a wiki is NAMED after.

    For a git source it is the repository itself. For an artifact folder it is
    ``{bucket}/{prefix}``: the ``artifact://`` scheme is dropped first,
    because this string becomes the wiki id — an object-key prefix, and what
    the browser matches a manifest on — and ``artifact:----docs--handbook``
    is not a name anybody asked for.

    The Go twin is ``run.DisplayRepositoryFor``; the two must agree, because
    the Go and the Python fixture runners serve the same journeys on
    different stacks.
    """
    repository = ""
    if isinstance(repo_config, dict):
        repository = str(repo_config.get("repository") or "")
        if not repository:
            provider = repo_config.get("provider_config")
            if isinstance(provider, dict):
                repository = str(provider.get("repository") or "")
    if is_artifact_source(repository):
        source = parse_artifact_source(repository)
        return f"{source.bucket}/{source.prefix}" if source.prefix else source.bucket
    return repository.strip().strip("/")


def wiki_id_for(repo_config: dict[str, Any] | None, branch: str | None) -> str:
    """The canonical ``{owner}--{repo}--{branch}`` the engine derives.

    The one derivation in this package: the fixture runner imports it rather
    than keeping a second copy, because a fixture that derived a different
    wiki id would land its pages under keys nothing reads.
    """
    repository = display_repository_for(repo_config) or "fixture/repository"
    return f"{repository.replace('/', '--')}--{(branch or 'main').strip() or 'main'}"


def manifest_key(wiki_id: str, wiki_version_id: str) -> str:
    """The pinned version's manifest object key."""
    return f"{wiki_id}/wiki_manifest_{wiki_version_id}.json"


def normalise_page_id(wiki_id: str, raw: str) -> str:
    """A page id without its ``{wiki_id}/`` prefix.

    BOTH FORMS EXIST IN THE WILD. The recorded legacy manifest lists pages
    as FULL keys (``acme--notes--main/wiki_pages/overview/x.md``); the
    fixture runners list them RELATIVE (``wiki_pages/overview/x.md``). A
    resolver that accepted only one of those would work on one stack and
    refuse every attachment on the other, which is exactly the kind of
    drift the two-fixture-runner rule exists to catch. Both sides of every
    comparison go through here.
    """
    candidate = str(raw or "").strip()
    prefix = f"{wiki_id}/"
    if candidate.startswith(prefix):
        candidate = candidate[len(prefix):]
    return candidate


def _selection(context_paths: Any) -> list[str]:
    """The requested ids, order preserved and duplicates collapsed."""
    if context_paths is None:
        return []
    if isinstance(context_paths, str) or not isinstance(context_paths, Iterable):
        raise ContextRefused(
            f"{PATHS_PARAM} must be a list of wiki page ids, not "
            f"{type(context_paths).__name__}"
        )
    seen: set[str] = set()
    ordered: list[str] = []
    for entry in context_paths:
        if not isinstance(entry, str) or not entry.strip():
            raise ContextRefused(
                f"{PATHS_PARAM} must contain wiki page ids as strings; "
                f"refused {entry!r}"
            )
        value = entry.strip()
        if value not in seen:
            seen.add(value)
            ordered.append(value)
    return ordered


def _truncate(body: str, limit: int) -> tuple[str, bool]:
    """A deterministic prefix plus a visible marker naming ``limit``."""
    if len(body) <= limit:
        return body, False
    return body[:limit] + TRUNCATION_MARKER.format(limit=limit), True


def build_context_block(
    *,
    selection: list[str],
    read_page: Callable[[str], str],
) -> str:
    """Render the resolved pages under the two budgets.

    ``selection`` is already validated and normalised; ``read_page`` answers
    one page's markdown for a normalised id.
    """
    sections: list[str] = []
    omitted: list[str] = []
    spent = 0
    for page_id in selection:
        remaining = TOTAL_BUDGET_CHARS - spent
        if remaining <= 0:
            omitted.append(page_id)
            continue
        body, _ = _truncate(read_page(page_id), PER_DOCUMENT_BUDGET_CHARS)
        if len(body) > remaining:
            body, _ = _truncate(body, remaining)
        spent += len(body)
        sections.append(f"--- source: {page_id} ---\n{body}")
    block = "\n\n".join(sections)
    if omitted:
        # Named, not dropped: an attachment that contributed nothing has to
        # be visible in the transcript, or the answer looks grounded in
        # pages it never saw.
        block += (
            f"\n\n[… {len(omitted)} further selected page(s) omitted for the "
            f"{TOTAL_BUDGET_CHARS} character context budget: "
            + ", ".join(omitted)
            + "]"
        )
    return block


def prepend_context(question: str, block: str) -> str:
    """The engine's ``enhanced_question`` shape, with our lead-in."""
    if not block:
        return question
    return f"{CONTEXT_LEAD_IN}\n{block}\n\n{QUESTION_LEAD_IN}{question}"


def resolve_context_paths(
    *,
    parameters: dict[str, Any],
    wiki_id: str,
    download: Callable[[str, str], bytes] | None,
    bucket: str = DEFAULT_BUCKET,
) -> str:
    """The whole resolution: validate, pin, read, budget, render.

    ``download(bucket, key)`` is the artifact transport, injected so this is
    testable — and refusable — without HTTP. Answers the rendered block, or
    ``""`` when nothing was selected.
    """
    selection = _selection(parameters.get(PATHS_PARAM))
    if not selection:
        return ""

    version = str(parameters.get(VERSION_PARAM) or "").strip()
    if not version:
        raise ContextRefused(
            f"{VERSION_PARAM} is required when {PATHS_PARAM} is given: a "
            f"selection is pinned to the wiki version it was made in, so it "
            f"cannot silently resolve against a newer one"
        )
    if not _VERSION_ID.match(version):
        raise ContextRefused(f"{VERSION_PARAM} is not a wiki version id: {version!r}")

    if download is None:
        raise ContextRefused(
            "selected wiki pages cannot be read: artifacts base_url not configured"
        )

    normalised: list[str] = []
    for raw in selection:
        page_id = normalise_page_id(wiki_id, raw)
        # Deduplicated AFTER normalisation, not before: the same page can be
        # named twice in the two id forms, and attaching it twice would
        # charge the reader's budget twice for one page.
        if page_id in normalised:
            continue
        if not _PAGE_ID.match(page_id):
            # Refused BEFORE any read. The message names the id and not the
            # key it would have become: the caller chose the id.
            raise ContextRefused(
                f"{PATHS_PARAM} may only name wiki pages of this wiki; "
                f"refused {raw!r}"
            )
        normalised.append(page_id)

    key = manifest_key(wiki_id, version)
    try:
        raw_manifest = download(bucket, key)
    except Exception as exc:  # noqa: BLE001 - reported, not swallowed
        raise ContextRefused(
            f"the wiki version {version!r} is not available, so the selected "
            f"pages cannot be pinned to it: {exc}"
        ) from exc
    try:
        manifest = json.loads(raw_manifest)
    except Exception as exc:  # noqa: BLE001
        raise ContextRefused(
            f"the manifest for wiki version {version!r} is not readable: {exc}"
        ) from exc

    published = {
        normalise_page_id(wiki_id, entry)
        for entry in (manifest.get("pages") or [])
        if isinstance(entry, str)
    }
    unknown = [page for page in normalised if page not in published]
    if unknown:
        # THE SCOPE REFUSAL. An id that is not in THIS wiki's manifest for
        # THIS version is refused, whether it names another project's wiki,
        # a page that only exists in a newer version, or nothing at all.
        raise ContextRefused(
            f"selected page(s) are not part of wiki {wiki_id!r} version "
            f"{version!r}: " + ", ".join(sorted(unknown))
        )

    def read_page(page_id: str) -> str:
        try:
            return download(bucket, f"{wiki_id}/{page_id}").decode("utf-8", "replace")
        except Exception as exc:  # noqa: BLE001
            raise ContextRefused(
                f"the selected page {page_id!r} could not be read: {exc}"
            ) from exc

    return build_context_block(selection=normalised, read_page=read_page)


def consume(parameters: dict[str, Any]) -> dict[str, Any]:
    """``parameters`` without the keys this module has already spent."""
    return {k: v for k, v in parameters.items() if k not in CONSUMED_PARAMS}


__all__ = [
    "CONSUMED_PARAMS",
    "CONTEXT_LEAD_IN",
    "ContextRefused",
    "DEFAULT_BUCKET",
    "PATHS_PARAM",
    "PER_DOCUMENT_BUDGET_CHARS",
    "QUESTION_LEAD_IN",
    "TOTAL_BUDGET_CHARS",
    "TRUNCATION_MARKER",
    "VERSION_PARAM",
    "build_context_block",
    "consume",
    "manifest_key",
    "normalise_page_id",
    "prepend_context",
    "resolve_context_paths",
    "wiki_id_for",
]
