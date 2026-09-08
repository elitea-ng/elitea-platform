"""The wiki_query family's one engine-side step: WHICH wiki a question is about.

The family itself — ``list_wikis``, ``resolve_and_ask``,
``resolve_and_deep_research``, ``delete_wiki`` — is served by the Go
sub-application host (``services/elitea-subapp-host``,
``internal/apps/deepwiki/run/wikiquery.go``), because three of the four are
ARTIFACT operations: a wiki is a set of keys under ``{wiki_id}/`` in the
bucket, and reading the registry, reading manifests and deleting a wiki are
reads and writes against the platform's artifact API. ADR-0023 put that
transport in the host, and this package's own README records why — the
legacy ``MiniArtifactClient`` is precisely the code that sent ``X-SECRET``
and passed ``verify=False``, the two ADR-0022 decision-6 violations the port
removed. Bringing it back here to serve three tools would bring both back.

What CANNOT be done in Go is the resolution: matching a free-text question
against a list of repositories needs a model, and the model clients live in
this closure. So the host reads the registry, hands the candidates over, and
this answers one wiki id.

Ported from ``deepwiki_plugin/methods/invoke.py::_resolve_wiki_with_llm``
(:1239) and the ``create_llm`` factory above it (:98). Two DECLARED
substitutions, and nothing else:

1. ``model_name`` has NO DEFAULT. Legacy fell back to ``"gpt-4o-mini"``,
   which is a hardcoded model in a deployment whose models are configured
   per project — on this platform it is a name the gateway usually cannot
   route, so the resolution failed with a 404 that read as "no wiki
   matched". The facade puts the toolkit's own ``llm_model`` into
   ``llm_settings.model_name`` (providerhost/material.CallbackSettings), so
   an absent one is a misconfiguration and is reported as one.
2. The validation returns the ANSWER; deciding whether an id is real is the
   host's, which holds the registry. Legacy did it here because the handler
   and the resolver were the same object.
"""

from __future__ import annotations

import logging
from typing import Any

logger = logging.getLogger(__name__)

#: The resolution prompt, verbatim from the legacy handler. The wording is
#: load-bearing: "Respond with ONLY the wiki_id" is what makes a bare id
#: parseable, and "or NONE" is the refusal the host turns into "could not
#: determine which wiki to query".
RESOLUTION_PROMPT = """Given the following question and list of available code repositories, determine which repository the question is most likely about.

Question: {question}

Available repositories:
{wiki_list_text}

Respond with ONLY the wiki_id (e.g., "owner--repo--branch") of the most relevant repository, or "NONE" if none are relevant.
Do not include any explanation or other text."""


def _wiki_list_text(wikis: list[Any]) -> str:
    """The candidate list as the prompt carries it, legacy's own formatting."""
    lines = []
    for wiki in wikis:
        if isinstance(wiki, dict):
            wiki_id = wiki.get("wiki_id", "")
            wiki_title = wiki.get("wiki_title", "")
            description = wiki.get("description", "") or ""
        else:
            wiki_id, wiki_title, description = str(wiki), "", ""
        lines.append(f"- {wiki_id}: {wiki_title} - {description[:150]}")
    return "\n".join(lines)


def create_llm(
    *,
    provider: str,
    model_name: str,
    api_key: str,
    api_base: str,
    organization: str | None = None,
    default_headers: dict | None = None,
    max_tokens: int = 4000,
    temperature: float = 0,
    max_retries: int = 2,
):
    """The legacy ``create_llm``, for the two providers the gateway fronts.

    The client class is chosen by provider because the ELITEA gateway's
    paths differ: ChatOpenAI posts ``<base>/chat/completions`` and
    ChatAnthropic posts ``<base>/v1/messages``, so the wrong class is a 403
    rather than a wrong answer.
    """
    if provider == "anthropic":
        from langchain_anthropic import ChatAnthropic  # noqa: PLC0415

        base_url = api_base.rstrip("/")
        if base_url.endswith("/v1"):
            base_url = base_url[:-3]
        if not default_headers:
            default_headers = {
                "openai-organization": str(organization) if organization else "",
                "Authorization": f"Bearer {api_key}",
            }
        return ChatAnthropic(
            model=model_name,
            api_key=api_key,
            base_url=base_url,
            max_tokens=max_tokens,
            temperature=temperature,
            max_retries=max_retries,
            streaming=False,
            default_headers=default_headers,
        )

    from langchain_openai import ChatOpenAI  # noqa: PLC0415

    base_url = api_base.rstrip("/")
    if not base_url.endswith("/v1"):
        base_url += "/v1"
    # o-series models refuse any temperature but 1.
    if str(model_name).startswith("o"):
        temperature = 1.0
    return ChatOpenAI(
        model=model_name,
        temperature=temperature,
        api_key=api_key,
        base_url=base_url,
        organization=organization,
        max_retries=max_retries,
        streaming=False,
        max_tokens=max_tokens,
    )


def resolve_wiki(
    *,
    question: str,
    wikis: list[Any] | None = None,
    llm_settings: dict[str, Any] | None = None,
    llm_factory=create_llm,
    **_ignored: Any,
) -> dict[str, Any]:
    """Answer which wiki a question is about.

    Returns the engine result dict the host reads: ``{"success": True,
    "wiki_id": …}`` — where the id is ``"NONE"`` when nothing matches — or
    ``{"success": False, "error": …}``.

    A model that will not answer is NOT an invocation failure. The host
    turns an unsuccessful result into "could not determine which wiki to
    query", which is what the legacy handler did with the ``None`` its own
    ``except`` produced, and it is the right answer: the caller's request
    was well formed.
    """
    wikis = wikis or []
    settings = llm_settings or {}
    if not wikis:
        return {"success": True, "wiki_id": "NONE"}

    api_base = settings.get("api_base") or settings.get("openai_api_base") or ""
    api_key = settings.get("api_key") or settings.get("openai_api_key") or ""
    model_name = settings.get("model_name")
    if not model_name:
        # Substitution 1. Reported rather than defaulted: a model this
        # deployment did not configure answers 404, and a 404 here reads as
        # "no wiki matched" — a wrong answer that looks like a right one.
        return {
            "success": False,
            "error": (
                "llm_settings carries no model_name, so the wiki cannot be resolved. "
                "The wiki_query toolkit's llm_model is what supplies it."
            ),
            "error_type": "ValueError",
            "error_category": "invalid_input",
        }
    if not api_base or not api_key:
        return {
            "success": False,
            "error": "llm_settings carries no api_base/api_key, so no model can be reached.",
            "error_type": "ValueError",
            "error_category": "invalid_input",
        }

    prompt = RESOLUTION_PROMPT.format(
        question=question, wiki_list_text=_wiki_list_text(wikis)
    )
    try:
        llm = llm_factory(
            provider=settings.get("provider", "openai"),
            model_name=model_name,
            api_key=api_key,
            api_base=api_base,
            organization=settings.get("organization"),
            default_headers=settings.get("default_headers", {}),
            max_tokens=settings.get("max_tokens", 4000),
            temperature=0,
        )
        response = llm.invoke(prompt)
    except Exception as exc:  # noqa: BLE001 - reported, not raised
        logger.exception("wiki resolution failed")
        return {"success": False, "error": f"Wiki resolution failed: {exc}"}

    content = getattr(response, "content", response)
    if isinstance(content, list):
        # Anthropic answers content BLOCKS, not a string. Legacy called
        # `.strip()` on it and raised AttributeError into its own except,
        # so every Anthropic resolution silently answered "no wiki". Joined
        # here instead — the same text the model actually produced.
        content = "".join(
            block.get("text", "") if isinstance(block, dict) else str(block)
            for block in content
        )
    return {"success": True, "wiki_id": str(content).strip().strip('"').strip("'")}


#: The tools this module serves, by the name the sidecar routes on.
WIKI_QUERY_TOOLS = {"resolve_wiki": resolve_wiki}

__all__ = ["RESOLUTION_PROMPT", "WIKI_QUERY_TOOLS", "create_llm", "resolve_wiki"]
