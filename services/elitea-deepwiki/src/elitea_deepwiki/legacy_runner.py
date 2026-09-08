"""The engine's host — what survives of ``perform_invoke_request`` here.

The legacy ``methods/invoke.py`` carried dispatch, a parameter merge, result
composition, an artifact upload, an LLM factory and a registry write. Under
ADR-0023 the SPI, the merge, the clone-destination check, composition and the
upload are the Go sub-application host's (``services/elitea-subapp-host``,
``internal/apps/deepwiki/run``), pinned there against the same P0 fixtures.
What stays in Python is the part that needs the engine's dependency closure:
binding the copied tool layer to the five hooks it calls back into, running a
tool off the event loop with the keyword set the host derived, and publishing
a completed generation's index into PostgreSQL where the storage code lives.

What is NOT here, and why (unchanged from the port):

* **The LLM factory.** The engine builds its own models from ``llm_settings``.
* **``extract_artifact_settings`` / the artifact upload.** The host uploads,
  through the transport the facade hands over; no surface of this service
  sends ``X-SECRET`` or disables TLS verification.
* **The registry write.** Its successor is the ``wikis`` table from migration
  0001, written by the generation path.
* **Tracebacks in caller-visible messages.** :mod:`elitea_deepwiki.errors`
  logs them instead.
"""

from __future__ import annotations

import json
import logging
from typing import Any, Callable


logger = logging.getLogger(__name__)

#: The legacy bucket every artifact object was tagged with at invoke time.
#: Note it disagrees with the descriptor, which advertises ``"wiki"``; the
#: invoke-time value is the one that reached the platform, so it is the one
#: that is preserved.
class ToolHost:
    """The hooks the copied tool layer expects from its Pylon module.

    ``tool_operations.Method`` is a mixin: every method it defines calls back
    into ``self`` for progress events, cancellation, child-process tracking and
    configuration. In the legacy service that ``self`` was the Pylon module.
    Here it is this object, which forwards to the invocation context and the
    service settings.

    Discovering that the tool layer needed exactly five hooks — and no Pylon
    behaviour beyond them — is what made copying it viable instead of
    rewriting 1400 lines. ``invocation_token`` is a sixth, added with the
    answer-token channel (issue #701); it is OURS, not the legacy module's,
    which is why it is optional on the context side.
    """

    def __init__(self, settings, context: Any) -> None:
        self._settings = settings
        self._context = context
        self._loop = None
        try:
            import asyncio  # noqa: PLC0415

            self._loop = asyncio.get_running_loop()
        except RuntimeError:  # pragma: no cover - only when built off-loop
            self._loop = None

    # -- configuration ----------------------------------------------------

    def runtime_config(self) -> dict[str, Any]:
        """The legacy ``runtime_config``: base_path plus the service URL.

        The tool layer reads ``base_path`` to place caches and worker
        scratch. ADR-0022 decision 4 makes that ephemeral pod storage, which
        is what ``scratch_path`` is.
        """
        return {
            "base_path": self._settings.scratch_path,
            "service_location_url": self._settings.service_location_url,
        }

    # -- invocation hooks -------------------------------------------------
    #
    # The tool layer is synchronous and runs in a worker thread, so these are
    # called from off the event loop. They marshal back onto it rather than
    # touching invocation state from another thread.

    def invocation_thinking(self, message: str) -> None:
        self._call_async(self._context.thinking(message))

    def invocation_token(self, text: str) -> None:
        """One fragment of the answer, on the token channel (issue #701).

        The SIXTH hook, and the only one the legacy Pylon module never had:
        the legacy service streamed tokens over socket.io, which ADR-0022
        removed. A context that predates the channel has no ``token``, so
        this degrades to progress-only rather than failing the tool — the
        answer still arrives whole with the result.
        """
        emit = getattr(self._context, "token", None)
        if emit is None:
            return
        self._call_async(emit(text))

    def invocation_stop_checkpoint(self) -> None:
        self._call_async(self._context.checkpoint())

    def invocation_process_add(self, process) -> None:
        self._context.process_add(process)

    def invocation_process_remove(self, process) -> None:
        self._context.process_remove(process)

    def _call_async(self, coroutine) -> None:
        import asyncio  # noqa: PLC0415

        if self._loop is None or not self._loop.is_running():
            coroutine.close()
            return
        future = asyncio.run_coroutine_threadsafe(coroutine, self._loop)
        # Propagates InvocationCancelled out of checkpoint() into the engine's
        # thread, which is how a cancel actually stops the work.
        future.result()


def _install_postgres_read_path(settings) -> None:
    """Point the engine's read path at PostgreSQL, once, if configured.

    Done here rather than at import time because it needs settings, and at
    first tool use rather than at startup because a service with no database
    configured must still boot and serve the SPI.
    """
    if not getattr(settings, "database_url", None):
        return

    from .storage import install as storage_install  # noqa: PLC0415

    if storage_install.is_installed():
        return

    import psycopg  # noqa: PLC0415

    dsn = settings.database_url
    storage_install.install(lambda: psycopg.connect(dsn))


_MODEL_EGRESS_APPLIED = False


def _apply_model_egress(settings) -> None:
    """Decide whether the engine may download models, once, before it runs.

    Before rather than during: ``huggingface_hub`` reads these variables when
    it is first used, and the engine is a verbatim copy that calls it directly.
    """
    global _MODEL_EGRESS_APPLIED
    if _MODEL_EGRESS_APPLIED:
        return

    import os  # noqa: PLC0415

    from .security.egress import EgressPolicy, apply_model_egress  # noqa: PLC0415

    apply_model_egress(
        EgressPolicy.parse(getattr(settings, "model_allowlist", None)), os.environ
    )
    _MODEL_EGRESS_APPLIED = True


_JOB_PATH_INSTALLED = False


def _install_job_path() -> None:
    """Repoint the Kubernetes-Job manifest builder, once.

    Unconditional: unlike the PostgreSQL read path this needs no
    configuration, and a Job created with the legacy manifest would reference
    filesystem paths that do not exist in this image.
    """
    global _JOB_PATH_INSTALLED
    if _JOB_PATH_INSTALLED:
        return

    from .jobs import install as install_job_path  # noqa: PLC0415

    install_job_path()
    _JOB_PATH_INSTALLED = True


def wiki_query_tools() -> dict[str, Callable[..., Any]]:
    """The wiki_query family's engine-side tools.

    Imported lazily for the same reason everything else here is: a default
    build carries no langchain, and importing it at module scope would make
    an engine-less deployment fail to boot rather than refuse per tool.
    """
    from .wiki_query import WIKI_QUERY_TOOLS  # noqa: PLC0415

    return WIKI_QUERY_TOOLS


_BOUND_HOST_CACHE: dict[type, type] = {}


def _bound_host_class(method_mixin: type) -> type:
    """Compose the copied tool layer onto the host that satisfies it.

    ``tool_operations.Method`` is a mixin with no ``__init__``; the legacy
    service mixed it into the Pylon module. Here it is mixed into
    :class:`ToolHost`, which supplies the five hooks it calls back into.

    Cached because composing it per invocation would rebuild the class on
    every request.
    """
    cached = _BOUND_HOST_CACHE.get(method_mixin)
    if cached is None:
        cached = type("BoundToolHost", (method_mixin, ToolHost), {})
        _BOUND_HOST_CACHE[method_mixin] = cached
    return cached


class LegacyToolRunner:
    """Dispatches SPI invocations into the copied tool layer and engine.

    ``tools`` injects callables in place of the real tool layer. That is not a
    convenience: it is how the composition path is tested against the P0
    fixture without the engine's ~1.1 GB dependency closure, and it mirrors how
    the fixture was recorded.
    """

    name = "legacy"

    def __init__(
        self,
        settings=None,
        tools: dict[str, Callable[..., Any]] | None = None,
    ) -> None:
        self._settings = settings
        self._tools = tools

    def _bound_tool(self, name: str, context: Any) -> Callable[..., Any]:
        if self._tools is not None:
            try:
                return self._tools[name]
            except KeyError:
                raise FileNotFoundError(f"Unknown tool: {name}") from None

        # The wiki_query family's resolver is OURS, not the copied engine's:
        # it is a port of methods/invoke.py::_resolve_wiki_with_llm, which
        # lived in the handler rather than in plugin_implementation/, so it
        # is not in the digest-guarded copy and must not be added to it.
        # It needs no engine closure beyond a langchain chat model, so it is
        # bound before the copied tool layer is imported at all.
        if name in wiki_query_tools():
            return wiki_query_tools()[name]

        # Imported here, not at module scope: the default image does not carry
        # the engine closure, and importing it at startup would make an
        # engine-less deployment fail to boot rather than refuse per tool.
        from .tool_operations import Method  # noqa: PLC0415

        if getattr(Method, name, None) is None:
            raise FileNotFoundError(f"Unknown tool: {name}")

        settings = self._settings
        if settings is None:
            from .config import Settings  # noqa: PLC0415

            settings = Settings.from_env()

        # The whole mixin is composed onto the host, not just the one method:
        # ``generate_wiki`` calls ``self._run_wiki_subprocess`` and
        # ``self._run_wiki_job``, ``ask`` calls ``self._run_ask_subprocess``,
        # and binding a single function leaves those unresolvable. Found by
        # running it.
        _install_postgres_read_path(settings)
        _install_job_path()
        _apply_model_egress(settings)

        host = _bound_host_class(Method)(settings, context)
        return getattr(host, name)

    async def _publish(self, result: dict[str, Any], context: Any) -> None:
        """Put the generated index into PostgreSQL before reporting success.

        Ordering matters: this runs BEFORE composition, so a publish failure
        can still be reported through the composed result's ``errors`` list,
        which the legacy composer renders as "⚠️ Partial issues detected".

        A failure does not fail the invocation. The pages, manifest and
        structure are genuine and will land; what is lost is the ability of
        *other* replicas to answer about this wiki. Discarding good artifacts
        over that would be worse, and passing silently would claim a queryable
        wiki that is not — so it is reported in band and logged loudly.
        """
        import asyncio  # noqa: PLC0415

        from .publishing import publish_generation  # noqa: PLC0415

        settings = self._settings
        if settings is None or not getattr(settings, "database_url", None):
            return

        await context.thinking("Publishing the index for query replicas")
        try:
            counts = await asyncio.to_thread(publish_generation, settings, result)
        except Exception as exc:  # noqa: BLE001 - reported, not swallowed
            logger.exception(
                "publishing the index for %s failed", result.get("wiki_id")
            )
            errors = result.get("errors")
            if not isinstance(errors, list):
                errors = []
                result["errors"] = errors
            errors.append(
                f"The wiki was generated but could not be published to the "
                f"index database, so replicas other than this one cannot "
                f"answer questions about it: {exc}"
            )
            await context.thinking("Publishing the index FAILED")
            return

        if counts is not None:
            await context.thinking(
                f"Published {counts['nodes']} nodes and "
                f"{counts['embeddings']} vectors"
            )

    def _artifact_download(self, arguments: dict[str, Any]):
        """The read half of the artifact transport this request carries.

        Derived from ``llm_settings`` exactly as the upload half is — there
        is no deployment-wide credential to build one from, and there must
        not be: the grant is the caller's, minted by the facade for the
        caller's project, which is what scopes a page read to a project at
        all. ``None`` when the request carried no transport.
        """
        llm_settings = arguments.get("llm_settings") or {}
        if not isinstance(llm_settings, dict) or not llm_settings:
            return None
        from .engine.artifacts_platform_client import (  # noqa: PLC0415
            create_platform_client_from_llm_settings,
        )

        client = create_platform_client_from_llm_settings(llm_settings)
        if client is None:
            return None
        return client.download_artifact

    def _apply_context_paths(self, tool_name: str, arguments: dict[str, Any]) -> dict[str, Any]:
        """Resolve reader-selected pages into ``question``.

        WHERE THIS RUNS, AND WHY NOT IN ``tool_operations.py``. The tool
        layer is a guarded verbatim copy of the legacy ``Method`` mixin with
        four declared substitutions (``engine/COPY_MANIFEST.json`` records
        its digest, and ``tools/refresh_engine_copy.py --check`` compares
        it); adding a feature to it would falsify the claim the manifest
        exists to make. This seam — the sidecar's entry into the engine —
        is ours, and it is the last point before the tool sees its keywords.

        NORMALLY A NO-OP, deliberately. The Go sub-application host resolves
        the same selection in front of its tool table
        (``internal/apps/deepwiki/run/contextpaths.go``) and REMOVES the two
        keys, so a request that came through the host arrives here with
        nothing left to do. What this covers is a sidecar called directly —
        a host that does not resolve, or a harness — where the alternative
        is an attachment the reader made being silently ignored.
        """
        from .wiki_context import (  # noqa: PLC0415
            PATHS_PARAM,
            ContextRefused,
            consume,
            prepend_context,
            resolve_context_paths,
            wiki_id_for,
        )

        if not arguments.get(PATHS_PARAM):
            return consume(arguments)
        if tool_name not in ("ask", "deep_research"):
            raise ContextRefused(
                f"{PATHS_PARAM} is not supported by {tool_name}; attach pages "
                f"to ask or deep_research"
            )
        repo_config = arguments.get("repo_config") or {}
        wiki_id = wiki_id_for(repo_config, repo_config.get("branch"))
        block = resolve_context_paths(
            parameters=arguments,
            wiki_id=wiki_id,
            download=self._artifact_download(arguments),
        )
        resolved = consume(arguments)
        resolved["question"] = prepend_context(arguments.get("question") or "", block)
        return resolved

    async def _paced(self, tool_name: str, context: Any) -> None:
        """Progress emitted before the engine answers; the fixture paces here."""

    async def _streamed(self, tool_name: str, result: Any, context: Any) -> None:
        """The answer token channel; the fixture streams here.

        A no-op for the real engine, which emits its own tokens from inside
        the tool (``invocation_token``) while it is still writing. The
        fixture has no model to write with, so it streams the answer it has
        already computed — see :mod:`elitea_deepwiki.fixture_runner`.
        """

    async def run_engine_tool(
        self, tool_name: str, arguments: dict[str, Any], context: Any
    ) -> Any:
        """Run one engine tool with an already-derived keyword set.

        The sidecar's entry point (ADR-0023 H2): the Go host derives the
        legacy keyword set itself and hands it over; this binds the tool to
        the host hooks and runs it off the event loop.

        The engine is synchronous and CPU/IO heavy. Running it on the event
        loop would stall every poll for the whole generation.
        """
        import asyncio  # noqa: PLC0415
        import functools  # noqa: PLC0415

        arguments = self._apply_context_paths(tool_name, arguments)
        await self._paced(tool_name, context)
        tool = self._bound_tool(tool_name, context)
        result = await asyncio.to_thread(functools.partial(tool, **arguments))
        await self._streamed(tool_name, result, context)
        return result

    async def publish(self, result: dict[str, Any], context: Any) -> None:
        """Publish a generated index — the sidecar's name for ``_publish``."""
        await self._publish(result, context)


__all__ = ["LegacyToolRunner", "ToolHost"]
