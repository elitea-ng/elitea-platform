"""The only worker module allowed to import ``elitea_sdk``.

Configuration behavior is selected from the installed SDK registry captured at
composition time. Validation and connection checking remain distinct explicit
operations; neither reloads the registry or rewrites provider-specific input.
"""

from __future__ import annotations

import hashlib
import importlib
import json
import re
import sys
import threading
import unicodedata
from contextlib import contextmanager, redirect_stdout
from copy import deepcopy
from dataclasses import dataclass
from functools import lru_cache
from pathlib import Path
from typing import Any

from pydantic import BaseModel, Field, ValidationError

from elitea_worker.agents.attachments import (
    ATTACHMENT_READ_TOOL_NAME,
    ATTACHMENT_TOOLKIT_NAME,
    ATTACHMENT_TOOLKIT_TYPE,
    AttachmentContentWriteback,
    attachment_content_writebacks,
    attachment_message_chunks,
    human_message_content,
    pending_attachment_reads,
    report_failed_attachment_reads,
)
from elitea_worker.agents.client_context import EliteaClientContext
from elitea_worker.agents.configuration_registry import (
    ConfigurationRegistryShadow,
    RegistryLoader,
)
from elitea_worker.agents.internal_tools import (
    ensure_sdk_state_directory,
    serve_internal_tools,
)
from elitea_worker.constants import (
    CONFIGURATION_CATALOG_REVISION,
    CONFIGURATION_CATALOG_SHA256,
    SDK_PACKAGE_TREE_SHA256,
)
from elitea_worker.execution.errors import DependencyUnavailable, UnsupportedCapability
from elitea_worker.handlers.agent import AgentExecutionPayload


_CURRENT_APPLICATION_TOOL_NAME_PATTERN = re.compile(r"[^a-zA-Z0-9_.-]")
_MAX_CURRENT_APPLICATION_TOOL_NAME_BYTES = 128
_NEXT_INPUT_SUGGESTION_PROMPT = (
    "You suggest a likely next user message in a chat, based on the "
    "assistant's latest reply. Reply with ONLY the suggested next user "
    "message, or the single word NONE if the reply doesn't make one "
    "obvious (e.g. a greeting, a simple acknowledgement, or a final "
    "answer with no natural follow-up). Keep it short — one sentence, "
    "written as if the user typed it.\n\n"
    "Examples:\n"
    "Assistant: Hi! How can I help you today?\n"
    "Suggestion: NONE\n\n"
    "Assistant: I've fixed the bug. Want me to also add a test for it?\n"
    "Suggestion: Yes, please add a test.\n\n"
    "Assistant: The capital of France is Paris.\n"
    "Suggestion: NONE\n\n"
    "Assistant reply:\n{reply}\n\nSuggestion:"
)

# The root HITL actions this worker admits, and the ones that carry a value.
#
# Both mirror `validCurrentHITLDecision`
# (services/elitea-main/internal/application/agentexecution/continue.go), the
# only thing that ever populates a decision that reaches here.
#
# `answer` is the clarifying-question resume the SDK's own `AskUserTool`
# pauses for. It arrives with a NON-EMPTY value and NO `guardrail_type` —
# elitea-main declares that field `json:"guardrail_type,omitempty"` and sets it
# for `mcp_auth` alone — so an admission list without `answer` refused every
# clarification with UNSUPPORTED_CAPABILITY. Nothing else could clear the pause
# afterwards either: the SDK returns the stale interrupt to the caller for any
# input that is not a resume, so the conversation stayed parked on a question
# it could never be told the answer to.
_ROOT_HITL_ACTIONS = frozenset(
    {"approve", "reject", "edit", "block_with_comment", "answer"}
)
_AUTHORIZATION_HITL_ACTIONS = frozenset({"authorize", "skip"})
_VALUE_BEARING_HITL_ACTIONS = frozenset({"edit", "block_with_comment", "answer"})
_HITL_ANSWER_ACTION = "answer"

# The SDK module whose `AskUserTool` this adapter re-binds; see
# `_install_ask_user_question_ids`.
_ASK_USER_TOOL_MODULE = "elitea_sdk.runtime.tools.ask_user"
# The native runtime's own bound (`MAX_QUESTION_ID_BYTES`,
# services/elitea-worker-rust/src/agents/internal_tools.rs).
_MAX_ASK_USER_QUESTION_ID_BYTES = 64
_ask_user_question_ids_lock = threading.Lock()
_ask_user_question_ids_installed = False


@dataclass(frozen=True, slots=True)
class SdkValidationError:
    error_type: str
    location: tuple[str | int, ...]
    ordinal: int


@dataclass(frozen=True, slots=True)
class SdkValidationOutcome:
    errors: tuple[SdkValidationError, ...]

    @property
    def valid(self) -> bool:
        return not self.errors


class SdkBudgetExceeded(Exception):
    """Data-free marker for an exact SDK budget policy rejection."""


@contextmanager
def _sdk_budget_boundary() -> Any:
    """Turn the SDK's typed budget rejection into the worker's marker.

    A budget rejection is a POLICY outcome, not a fault. The SDK raises
    ``BudgetExceededError`` for it and states why it must never be swallowed:
    there is no recovery from an exhausted budget, so continuing would feed a
    policy rejection back into the model as if it were data.

    Every SDK entry point this adapter owns needs the boundary, not only the
    indexing one. Agent execution had no boundary, so an exhausted budget
    reached the delivery catch-all as an unclassified exception and was
    reported as ``InternalFailure`` — a retryable internal fault, for a
    condition that no retry can clear.

    The marker carries no message. The SDK's text names the budget and can
    quote the proxy; the worker's public diagnostics stay data-free.
    """

    try:
        yield
    except Exception as error:
        if _is_sdk_budget_exceeded(error):
            raise SdkBudgetExceeded() from None
        raise


@dataclass(frozen=True, slots=True)
class SdkConfigurationBinding:
    configuration_type: str
    schema_id: str
    schema_revision: str
    schema_digest: bytes
    validation_supported: bool
    connection_check_supported: bool


class EliteaSdkAdapter:
    """Pinned SDK configuration-model adapter loaded once at composition time."""

    def __init__(self, registry_loader: RegistryLoader | None = None) -> None:
        if registry_loader is None:
            # SDK package initializers discover optional integrations and may
            # print diagnostics. Keep stdout reserved for the worker protocol.
            with redirect_stdout(sys.stderr):
                module = _import_sdk_configurations()
                _require_complete_configuration_registry(module)
                package_root = Path(module.__file__).resolve().parents[1]
                if _package_tree_digest(package_root) != SDK_PACKAGE_TREE_SHA256:
                    raise DependencyUnavailable(
                        "The installed Elitea SDK artifact does not match the admitted package tree."
                    )
                self._registry = ConfigurationRegistryShadow(
                    module.get_class_configurations
                )
        else:
            # Injection is restricted to composition/tests. The shadow still
            # copies and validates the registry exactly once.
            self._registry = ConfigurationRegistryShadow(registry_loader)

    @property
    def catalog_revision(self) -> str:
        return CONFIGURATION_CATALOG_REVISION

    @property
    def catalog_digest(self) -> bytes:
        # This remains the admitted catalog identity used by the current wire.
        # Each selected model is additionally bound to its computed schema
        # digest below, so a type cannot be validated against another schema.
        return bytes.fromhex(CONFIGURATION_CATALOG_SHA256)

    def schema(self, configuration_type: str) -> SdkConfigurationBinding:
        entry = self._registry.entry(configuration_type)
        if entry is None:
            raise UnsupportedCapability("Configuration type is not supported.")
        return SdkConfigurationBinding(
            configuration_type=entry.type,
            schema_id=f"elitea.configuration.{entry.type}",
            schema_revision=CONFIGURATION_CATALOG_REVISION,
            schema_digest=entry.schema_digest,
            validation_supported=entry.validation_supported,
            connection_check_supported=entry.connection_check_supported,
        )

    def validate(self, configuration_type: str, settings: dict[str, Any]) -> SdkValidationOutcome:
        binding = self.schema(configuration_type)
        if not binding.validation_supported:
            raise UnsupportedCapability(
                "Validation is not supported for this configuration type."
            )
        model = self._registry.model(binding.configuration_type)
        if model is None:
            raise UnsupportedCapability("Configuration type is not supported.")

        try:
            # Business-compatibility boundary: exactly the registered SDK
            # validation algorithm, exactly once for each admitted request.
            model.model_validate(settings)
        except ValidationError as exc:
            raw_errors = exc.errors(
                include_url=False,
                include_context=False,
                include_input=False,
            )
            errors = tuple(
                SdkValidationError(
                    error_type=str(item.get("type", "unknown")),
                    location=tuple(item.get("loc", ())),
                    ordinal=index,
                )
                for index, item in enumerate(raw_errors)
            )
            return SdkValidationOutcome(errors)
        return SdkValidationOutcome(())

    def check_connection(
        self,
        configuration_type: str,
        settings: dict[str, Any],
    ) -> str | dict[str, Any] | None:
        """Run a registered checker only for an explicit caller operation."""

        binding = self.schema(configuration_type)
        model = self._registry.model(binding.configuration_type)
        checker = (
            getattr(model, "check_connection", None) if model is not None else None
        )
        if not binding.connection_check_supported or not callable(checker):
            raise UnsupportedCapability(
                "Connection checking is not supported for this configuration type."
            )
        return checker(settings)


class EliteaSdkToolkitAdapter:
    """Pinned adapter for the current ``toolkit.available_tools`` algorithm.

    Evidence boundary:
    - ``centry/pylon_indexer/plugins/indexer_worker/methods/``
      ``indexer_toolkit_available_tools.py:32-39`` delegates to this SDK API
      and maps an escaping ``Exception`` to the current response shape.
    - ``elitea_sdk/tools/__init__.py:368-401`` owns type normalization,
      enumerator lookup, result values and toolkit error strings.

    This adapter deliberately performs one keyword call and no normalization,
    filtering, retry, caching or result rewrite.
    """

    def __init__(self) -> None:
        # Loading elitea_sdk.tools discovers optional toolkit modules and may
        # print import diagnostics. Keep command/result stdout free of those
        # diagnostics without changing the SDK's discovery semantics.
        with redirect_stdout(sys.stderr):
            module = importlib.import_module("elitea_sdk.tools")
        package_root = Path(module.__file__).resolve().parents[1]
        if _package_tree_digest(package_root) != SDK_PACKAGE_TREE_SHA256:
            raise DependencyUnavailable(
                "The installed Elitea SDK artifact does not match the admitted package tree."
            )
        self._tools_module = module

    def get_toolkit_available_tools(
        self,
        toolkit_type: str,
        settings: dict[str, Any],
    ) -> dict[str, Any]:
        # Business-compatibility boundary: this is exactly the call performed
        # by the current indexer wrapper, exactly once per admitted execution.
        return self._tools_module.get_toolkit_available_tools(
            toolkit_type=toolkit_type,
            settings=settings,
        )


def verify_sdk_markdown_runtime(sentinel: str) -> None:
    """Exercise the shared SDK Markdown parser without leaking SDK ownership."""

    with redirect_stdout(sys.stderr):
        module = importlib.import_module("elitea_sdk.tools.utils.content_parser")
    documents = list(
        module.process_content_by_type(
            ("# " + sentinel).encode("utf-8"),
            "elitea-runtime-probe.md",
        )
    )
    observed = "\n".join(document.page_content for document in documents)
    if not documents or sentinel not in observed:
        raise RuntimeError("markdown-output-mismatch")


def _secrets_header_kwargs(context: EliteaClientContext) -> dict[str, str]:
    """The X-SECRET argument for one SDK client, or no argument at all.

    THE SDK DEFAULTS THIS HEADER TO THE LITERAL "secret"
    (``elitea_sdk/runtime/clients/client.py``, ``kwargs.get('XSECRET',
    'secret')``), and every project used to accept that literal because pylon
    read ``secrets.get("secrets_header_value", "secret")``. The platform no
    longer accepts it (issue 408), so a call that carries the project's own
    value is the only call the version-details route answers.

    ``XSECRET`` is the argument, NOT ``api_extra_headers``. Both put the value
    in the client's platform headers, but ``api_extra_headers`` is also copied
    onto the MODEL call as ``default_headers``, and on one branch it replaces
    that header map entirely (``client.py``, the openai-compatible path). The
    value authenticates one platform route, so it goes only where that route
    reads it.

    An empty value adds NO argument. The SDK then sends its literal, the
    platform answers 403, and the operator sees one refusal that names the
    repair — which is better than a header this worker made up.
    """

    if not context.secrets_header_value:
        return {}
    return {"XSECRET": context.secrets_header_value}


class EliteaSdkIndexingAdapter:
    """Pinned adapter for the current ``index_data`` SDK entrypoint.

    The authorized runtime composition supplies an initialized ``EliteAClient``.
    Client construction and credential redemption are deliberately outside this
    parity kernel. The adapter preserves the current worker's one public SDK
    call without copying its Pylon event, logging or response-cleaning wrapper.
    """

    def __init__(self, client: Any) -> None:
        client_type = _indexing_client_type()
        if not isinstance(client, client_type):
            raise TypeError(
                "client must be an EliteAClient from the admitted SDK artifact"
            )
        self._client = client

    @classmethod
    def from_context(cls, context: EliteaClientContext) -> EliteaSdkIndexingAdapter:
        """Construct one SDK client from claim-scoped in-memory authority."""

        client_type = _indexing_client_type()
        client = client_type(
            project_id=context.project_id,
            base_url=context.base_url,
            auth_token=context.auth_token,
            **_secrets_header_kwargs(context),
        )
        return cls(client)

    def ingest(
        self,
        *,
        toolkit_config: dict[str, Any],
        tool_params: dict[str, Any],
        runtime_config: dict[str, Any],
        llm_model: str | None,
        llm_config: dict[str, Any],
        mcp_tokens: dict[str, Any] | None,
    ) -> dict[str, Any]:
        invocation_toolkit_config = _current_index_tool_name_compatibility(
            toolkit_config
        )
        # Business-compatibility boundary: exactly the public SDK operation used
        # by the current indexer worker, exactly once per kernel invocation.
        with _sdk_budget_boundary():
            return self._client.test_toolkit_tool(
                toolkit_config=invocation_toolkit_config,
                tool_name="index_data",
                tool_params=deepcopy(tool_params),
                runtime_config=runtime_config,
                llm_model=llm_model,
                llm_config=deepcopy(llm_config),
                mcp_tokens=mcp_tokens,
            )


class EliteaSdkToolkitToolAdapter:
    """Pinned adapter for ONE toolkit tool run.

    This is ``EliteaSdkIndexingAdapter`` with the tool name promoted from a
    constant to a parameter. It is a separate class and not a keyword argument
    on that one because the two carry different obligations: the index adapter
    must keep applying the current wrapper's ``index_data`` tool-name
    compatibility rewrite, and a caller-named tool must NOT be rewritten. One
    class per obligation is what stops a later edit from giving an arbitrary
    tool the index path's special case.

    The SDK entrypoint is the same public method the current indexer worker
    calls, so no new SDK surface is admitted by this capability.
    """

    def __init__(self, client: Any) -> None:
        client_type = _indexing_client_type()
        if not isinstance(client, client_type):
            raise TypeError(
                "client must be an EliteAClient from the admitted SDK artifact"
            )
        self._client = client

    @classmethod
    def from_context(cls, context: EliteaClientContext) -> EliteaSdkToolkitToolAdapter:
        """Construct one SDK client from claim-scoped in-memory authority."""

        client_type = _indexing_client_type()
        client = client_type(
            project_id=context.project_id,
            base_url=context.base_url,
            auth_token=context.auth_token,
            # X-SECRET, not api_extra_headers. See _secrets_header_kwargs: the
            # value authenticates one platform route and must not reach the
            # model call. A tool run makes model calls, so this matters here.
            **_secrets_header_kwargs(context),
        )
        return cls(client)

    def call_tool(
        self,
        *,
        toolkit_config: dict[str, Any],
        tool_name: str,
        tool_params: dict[str, Any],
        runtime_config: dict[str, Any],
        llm_model: str | None,
        llm_config: dict[str, Any],
        mcp_tokens: dict[str, Any] | None,
    ) -> dict[str, Any]:
        # Business-compatibility boundary: exactly the public SDK operation the
        # current indexer worker uses, exactly once per kernel invocation, with
        # the caller's tool name passed through unaltered.
        with _sdk_budget_boundary():
            return self._client.test_toolkit_tool(
                toolkit_config=deepcopy(toolkit_config),
                tool_name=tool_name,
                tool_params=deepcopy(tool_params),
                runtime_config=runtime_config,
                llm_model=llm_model,
                llm_config=deepcopy(llm_config),
                mcp_tokens=mcp_tokens,
            )


class EliteaSdkAgentAdapter:
    """Initial synchronous SDK seam for the two current agent constructors.

    The admitted kernel accepts ordinary turns, explicit response regeneration,
    root HITL, and delegated-toolkit authorization continuation. Image
    resolution and unbounded durable children remain deferred.
    """

    def __init__(
        self,
        client: Any,
        *,
        memory: Any = None,
        callbacks: list[Any] | None = None,
        checkpoint_factory: Any = None,
        project_id: int | None = None,
    ) -> None:
        client_type = _indexing_client_type()
        if not isinstance(client, client_type):
            raise TypeError(
                "client must be an EliteAClient from the admitted SDK artifact"
            )
        self._client = client
        # The claim-bound composition owns this checkpointer. It is never part
        # of AgentExecutionInputV1 and never crosses Redis or gRPC. The SDK and
        # LangGraph remain the only readers/writers of checkpoint state.
        self._memory = memory
        self._callbacks = list(callbacks or [])
        self._checkpoint_factory = checkpoint_factory
        self._project_id = project_id
        # #607: what this turn's document reads produced, kept so the terminal
        # result can report it back for persistence. One adapter is built per
        # execution, so this is turn-scoped state and not a cache.
        self._attachment_writebacks: list[AttachmentContentWriteback] = []

    @classmethod
    def from_context(
        cls,
        context: EliteaClientContext,
        *,
        memory: Any = None,
        callbacks: list[Any] | None = None,
        checkpoint_factory: Any = None,
    ) -> EliteaSdkAgentAdapter:
        client_type = _indexing_client_type()
        client = client_type(
            project_id=context.project_id,
            base_url=context.base_url,
            auth_token=context.auth_token,
            **_secrets_header_kwargs(context),
        )
        return cls(
            client,
            memory=memory,
            callbacks=callbacks,
            checkpoint_factory=checkpoint_factory,
            project_id=context.project_id,
        )

    def execute_application(self, payload: AgentExecutionPayload) -> dict[str, Any]:
        _require_initial_agent_kernel(payload)
        _apply_toolkit_guardrails(payload)
        ensure_sdk_state_directory()
        _install_ask_user_question_ids()
        with self._execution_memory() as memory, _sdk_budget_boundary():
            application = payload.application
            version_details = deepcopy(application.get("version_details") or {})
            _serve_version_internal_tools(version_details)
            llm_kwargs = _llm_kwargs(payload.llm)
            executor = self._client.application(
                application_id=application.get("id"),
                application_version_id=application.get("version_id"),
                tools=deepcopy(payload.tools) or None,
                memory=memory,
                application_variables=deepcopy(application.get("variables")),
                version_details=version_details or None,
                mcp_tokens=deepcopy(payload.mcp_tokens),
                conversation_id=payload.conversation_id,
                ignored_mcp_servers=list(payload.ignored_mcp_servers),
                user_declined_mcp_servers=deepcopy(payload.user_declined_mcp_servers),
                exception_handling_enabled=bool(payload.exception_handling_enabled),
                context_settings=deepcopy(payload.context_settings),
                auto_approve_sensitive_actions=payload.auto_approve_sensitive_actions,
                openai_compatible=bool(llm_kwargs.get("openai_compatible", False)),
            )
            return _invoke_initial_agent(
                executor,
                payload,
                version_details.get("meta"),
                self._callbacks,
                memory,
                self._read_attachment_documents,
            )

    def execute_adhoc(self, payload: AgentExecutionPayload) -> dict[str, Any]:
        _require_initial_agent_kernel(payload)
        _apply_toolkit_guardrails(payload)
        ensure_sdk_state_directory()
        _install_ask_user_question_ids()
        internal_tools = serve_internal_tools(payload.internal_tools)
        with self._execution_memory() as memory, _sdk_budget_boundary():
            llm_kwargs = _llm_kwargs(payload.llm)
            llm = self._client.get_llm(
                model_name=llm_kwargs.get("model"),
                model_config={
                    "model_project_id": llm_kwargs.get("model_project_id"),
                    "max_tokens": llm_kwargs.get("max_tokens"),
                    "reasoning_effort": llm_kwargs.get("reasoning_effort"),
                    "temperature": llm_kwargs.get("temperature"),
                    "streaming": llm_kwargs.get("stream", True),
                    "openai_compatible": llm_kwargs.get("openai_compatible", False),
                },
            )
            executor = self._client.predict_agent(
                llm=llm,
                instructions=payload.application.get(
                    "instructions", "You are a helpful assistant."
                ),
                tools=deepcopy(payload.tools),
                chat_history=deepcopy(payload.chat_history),
                memory=memory,
                debug_mode=True if payload.debug_mode is None else payload.debug_mode,
                mcp_tokens=deepcopy(payload.mcp_tokens),
                conversation_id=payload.conversation_id,
                ignored_mcp_servers=list(payload.ignored_mcp_servers),
                persona=payload.persona,
                # `lazy_tools_mode` is read from the ORIGINAL list on purpose: it
                # is a mode flag rather than a tool, nothing materialises it, and
                # it is never one of the names `serve_internal_tools` can drop.
                lazy_tools_mode="lazy_tools_mode" in payload.internal_tools,
                internal_tools=internal_tools,
                exception_handling_enabled=bool(payload.exception_handling_enabled),
                context_settings=deepcopy(payload.context_settings),
                step_limit=payload.steps_limit,
                auto_approve_sensitive_actions=payload.auto_approve_sensitive_actions,
                user_declined_mcp_servers=deepcopy(payload.user_declined_mcp_servers),
            )
            return _invoke_initial_agent(
                executor,
                payload,
                None,
                self._callbacks,
                memory,
                self._read_attachment_documents,
            )

    def suggest_next_input(
        self,
        payload: AgentExecutionPayload,
        *,
        output_text: str,
    ) -> str | None:
        """Generate the current best-effort post-response suggestion."""

        try:
            policy = payload.next_input_suggestion
            if not policy.get("enabled"):
                return None
            if len(output_text) < policy["min_response_chars"]:
                return None

            llm = self._client.get_low_tier_llm(max_tokens=64)
            if llm is None:
                return None

            result: list[Any] = []

            def invoke() -> None:
                try:
                    result.append(
                        llm.invoke(
                            _NEXT_INPUT_SUGGESTION_PROMPT.format(reply=output_text)
                        )
                    )
                except Exception:
                    return

            thread = threading.Thread(target=invoke, daemon=True)
            thread.start()
            thread.join(timeout=policy["timeout_seconds"])
            if thread.is_alive() or not result:
                return None

            suggestion = getattr(result[0], "content", result[0])
            text = str(suggestion or "").strip()
            if not text or text.upper() == "NONE":
                return None
            return text
        except Exception:
            # Exact current behavior: this optional follow-up cannot fail an
            # otherwise successful primary execution.
            return None

    def _read_attachment_documents(
        self,
        payload: AgentExecutionPayload,
        references: list[tuple[str, str]],
    ) -> dict[tuple[str, str], str]:
        """Read this turn's attached documents through the SDK artifact toolkit.

        This is pylon's extraction step (rpc/chat_all.py:344-377 →
        utils/attachments.py:429-497) with its Pylon transport removed. Pylon
        goes through ``test_toolkit_tool_sio`` because its reader lives in
        another process; the worker IS that process, so it makes the same
        public SDK call the indexing adapter already makes
        (``EliteaSdkIndexingAdapter.ingest``) — one batched
        ``read_multiple_files`` per bucket, on the claim-scoped client.

        ONE CALL PER BUCKET, not one per file. Pylon batches too, and it can
        assume a single bucket because it resolves the project default itself
        (utils/internal_tools.py:277-295). The worker takes the bucket from
        each chunk's marker instead — it has no vault access — so files from
        two buckets in one turn cost two calls rather than being silently read
        from the wrong one.

        NOTHING HERE MAY FAIL THE TURN. A bucket the platform cannot read, a
        toolkit that will not instantiate, a file that no longer exists —
        pylon logs each and continues (rpc/chat_all.py:384-386), because the
        question may not even be about the file, and the header chunk still
        tells the model the file exists and that read tools are available. The
        single exception is a budget rejection: it is a policy outcome with no
        recovery, and swallowing it here would only let the agent invocation
        hit the same wall a moment later with the attachment silently dropped.
        """

        contents: dict[tuple[str, str], str] = {}
        failures = 0
        by_bucket: dict[str, list[str]] = {}
        for bucket, name in references:
            by_bucket.setdefault(bucket, []).append(name)
        for bucket, names in by_bucket.items():
            try:
                outcome = self._client.test_toolkit_tool(
                    toolkit_config={
                        "type": ATTACHMENT_TOOLKIT_TYPE,
                        "toolkit_name": ATTACHMENT_TOOLKIT_NAME,
                        # Auto-injected, no toolkit entity in the database —
                        # exactly as pylon builds it
                        # (utils/attachments.py:454-461).
                        "toolkit_id": None,
                        "settings": {"bucket": bucket},
                    },
                    tool_name=ATTACHMENT_READ_TOOL_NAME,
                    tool_params={"file_paths": list(names)},
                    # The turn's own model, so the toolkit is built against the
                    # model this turn is already authorized for rather than the
                    # SDK's 'gpt-4o-mini' default, which a deployment need not
                    # serve. The read tool never calls it: the LLM is only an
                    # argument of toolkit instantiation.
                    llm_model=_llm_kwargs(payload.llm).get("model"),
                )
            except Exception as error:
                if _is_sdk_budget_exceeded(error):
                    raise
                failures += len(names)
                continue
            files = outcome.get("result") if isinstance(outcome, dict) else None
            # `success: False` is RETURNED, not raised, for a toolkit or tool
            # failure (SDK client.test_toolkit_tool), so an unchecked
            # `.get("result")` would forward a refusal's payload as if it were
            # the file's contents.
            if (
                not isinstance(outcome, dict)
                or not outcome.get("success")
                or not isinstance(files, dict)
            ):
                failures += len(names)
                continue
            for name in names:
                text = files.get(name)
                # A per-file read error arrives as its own string
                # ("Error reading file: ...", SDK elitea_base.py:685-687) and
                # pylon forwards it to the model unchanged: it is a truthful
                # answer to "what is in this file". Only an absent or empty
                # entry counts as a failure.
                if isinstance(text, str) and text:
                    contents[(bucket, name)] = text
                else:
                    failures += 1
        report_failed_attachment_reads(failures)
        # Composed here rather than in the caller because this is the only
        # place that knows which reads succeeded. A file that could not be read
        # contributes nothing: it has no text to persist, and leaving its row
        # untouched is what lets a later turn try again.
        self._attachment_writebacks = attachment_content_writebacks(
            payload.input_attachments,
            contents,
        )
        return contents

    @property
    def attachment_content_writebacks(self) -> list[AttachmentContentWriteback]:
        """The enriched attachment rows this turn produced, if any (#607)."""

        return list(self._attachment_writebacks)

    @contextmanager
    def _execution_memory(self):
        """Keep one saver open for exactly one synchronous SDK invocation."""

        if self._memory is not None:
            yield self._memory
            return
        if self._checkpoint_factory is None or self._project_id is None:
            raise DependencyUnavailable(
                "The durable agent checkpoint store is unavailable."
            )
        with self._checkpoint_factory.open(
            self._client,
            project_id=self._project_id,
        ) as memory:
            yield memory


def _serve_version_internal_tools(version_details: dict[str, Any]) -> None:
    """Drop the internal tools this image cannot build, in place.

    The stored-agent path never hands ``payload.internal_tools`` to the SDK: the
    SDK reads the authored set out of ``version_details['meta']`` instead, and
    reads it TWICE — once in ``EliteAClient.application`` to decide which
    middleware to construct, and once in ``LangChainAssistant`` to decide which
    tools to build. Pruning the meta covers both, which pruning the payload
    would not.

    ``version_details`` is the adapter's own deep copy, so this mutates nothing
    the platform sent. A meta that is not an object, or an ``internal_tools``
    that is not a list, is left exactly as it is: this function exists to remove
    names, not to repair a shape, and the SDK's own handling of a malformed
    value is the current behavior.
    """

    meta = version_details.get("meta")
    if not isinstance(meta, dict):
        return
    configured = meta.get("internal_tools")
    if not isinstance(configured, list):
        return
    if not all(isinstance(name, str) for name in configured):
        return
    meta["internal_tools"] = serve_internal_tools(configured)


def _install_ask_user_question_ids() -> None:
    """Keep the question ids the MODEL chose on the SDK's ``ask_user`` tool.

    ## What diverges without it

    ``AskUserTool``'s argument schema (``AskUserQuestionSpec``) declares no
    ``id`` field, so pydantic drops the one the model sent before
    ``_normalize_questions`` ever runs and every question is re-keyed
    positionally as ``q1``, ``q2``… The pause the browser renders carries those
    ids, `AnswerQuestionsControl` keys its answer object by them, and the
    answer therefore arrives as ``{"q1": "Staging"}`` where the native runtime
    — which keeps the model's id and falls back to ``q{n}`` only when there is
    none (``AskUserQuestion::normalize``,
    services/elitea-worker-rust/src/agents/internal_tools.rs) — produces
    ``{"environment": "Staging"}``.

    ## Why the ids, and not a translation on the way back in

    The browser ECHOES the ids the pause carried; it never invents them and it
    has no way to know which runtime produced them. So the only way one browser
    payload can answer both runtimes is for both to advertise the same ids for
    the same model output. Rewriting the incoming map onto positional keys in
    this adapter would have to infer which question each key belongs to from
    its position in a JSON object — and `buildAnswerValue` OMITS a question the
    user left blank, so the first skipped question would silently shift every
    later answer onto the wrong one. It would also leave the two runtimes
    STORING different pauses for the same model call, which is what makes a
    transcript still readable after a runtime switch.

    ## Why the seam is here and not in the SDK

    This image builds ``elitea_sdk`` from a pinned upstream revision plus
    cherry-picked upstream commits (``Containerfile``,
    ``elitea-sdk.lock.json``), so a change inside the SDK is a change to
    another repository's history first. The SDK imports the class lazily,
    inside the branch that constructs it (``runtime/toolkits/tools.py``:
    ``from ..tools.ask_user import AskUserTool``), so re-binding that module
    attribute is a seam this worker owns and the SDK honours on the next
    construction.

    ## What it costs

    The subclass ADVERTISES the optional ``id`` to the model, which the native
    runtime tolerates but does not advertise (its parameter schema is
    ``additionalProperties: false`` with no ``id`` property). The alternative —
    accepting a key the schema hides — needs the generated JSON schema to be
    rewritten behind pydantic's back, and a model that is never told it may
    name a question will rarely name one, which leaves the positional keys in
    place for every real model and fixes the divergence only for the scripted
    one.

    Idempotent, and callable on every run: it imports one module and re-binds
    one attribute, under a lock because a worker process serves claims on more
    than one thread.
    """

    global _ask_user_question_ids_installed

    if _ask_user_question_ids_installed:
        return
    with _ask_user_question_ids_lock:
        if _ask_user_question_ids_installed:
            return
        module = importlib.import_module(_ASK_USER_TOOL_MODULE)
        module.AskUserTool = _identified_ask_user_tool(module)
        _ask_user_question_ids_installed = True


def _identified_ask_user_tool(module: Any) -> type[Any]:
    """Build the ``AskUserTool`` subclass that carries a question ``id``.

    Built from the module's own classes rather than from imports at the top of
    this file, so the SDK artifact stays the single definition of what a
    question is: only the ``id`` is added here, and ``_run`` still runs the
    SDK's normalisation, interrupt payload and answer formatting.
    """

    base_tool = module.AskUserTool
    base_question = module.AskUserQuestionSpec

    class _IdentifiedAskUserQuestion(base_question):  # type: ignore[misc, valid-type]
        id: str = Field(
            default="",
            description=(
                "Stable identifier for this question, echoed back with the "
                "user's answer. Use a short name for what is being decided "
                "(for example 'environment'). Defaults to q1, q2… in order."
            ),
        )

    class _IdentifiedAskUserInput(BaseModel):
        questions: list[_IdentifiedAskUserQuestion] = Field(
            description="1-4 questions to ask the user at once.",
        )

    class _IdentifiedAskUserTool(base_tool):  # type: ignore[misc, valid-type]
        args_schema: type[BaseModel] = _IdentifiedAskUserInput

        def _run(
            self,
            questions: Any = None,
            run_manager: Any = None,
            **kwargs: Any,
        ) -> str:
            supplied = questions if questions is not None else kwargs.get("questions")
            kwargs.pop("questions", None)
            return super()._run(
                questions=_ask_user_questions_with_ids(supplied),
                run_manager=run_manager,
                **kwargs,
            )

    return _IdentifiedAskUserTool


def _ask_user_questions_with_ids(questions: Any) -> Any:
    """Hand the SDK's normaliser plain dicts, carrying an admitted ``id``.

    ``_normalize_questions`` reads ``id`` off a dict and off a question spec
    alike, so this only has to decide WHICH ids survive. An id the native
    runtime would refuse is REMOVED rather than refused, and the SDK's
    positional default takes over for that question: a model that sends a
    240-character id has produced a bad name for a question, not an
    unanswerable pause, and the browser can still answer a ``q1``.
    """

    if not isinstance(questions, list):
        return questions
    prepared: list[Any] = []
    for question in questions:
        if isinstance(question, dict):
            item = dict(question)
        elif hasattr(question, "model_dump"):
            item = question.model_dump()
        else:
            prepared.append(question)
            continue
        if not _is_admitted_ask_user_question_id(item.get("id")):
            item.pop("id", None)
        prepared.append(item)
    return prepared


def _is_admitted_ask_user_question_id(value: Any) -> bool:
    """The native runtime's own rule for a question id, restated.

    Non-empty, at most 64 UTF-8 bytes, and no control characters — the same
    three conditions ``AskUserQuestion::normalize`` applies, in the same order.
    The bound matters because the id becomes a KEY in the stored pause and in
    the answer object the browser posts back.
    """

    if not isinstance(value, str) or not value:
        return False
    if len(value.encode("utf-8")) > _MAX_ASK_USER_QUESTION_ID_BYTES:
        return False
    return not any(unicodedata.category(character) == "Cc" for character in value)


def _apply_toolkit_guardrails(payload: AgentExecutionPayload) -> None:
    """Configure the SDK's guardrail state from the policy this run carries.

    ## Why this is per run and not per container

    ``elitea_sdk.runtime.toolkits.security`` keeps its blocklist and its
    sensitive-tool policy in module-level globals, and until now the only thing
    that ever populated them was ``ELITEA_SENSITIVE_TOOLS`` and its siblings,
    read lazily from the environment on first use. That means a policy change
    needs a redeploy, and every tenant sharing a worker pool shares one answer.
    The admin Configuration page writes this policy per platform and expects it
    to take effect on the next call, so the resolved policy travels with the
    command and is applied here, immediately before the agent is built.

    ``configure_blocklist`` and ``configure_sensitive_tools`` are public SDK
    functions that had no non-test caller. This is that caller.

    ## Absent means "leave the environment alone"

    A command with no policy — an older platform, or a replayed command — must
    not clear a blocklist the environment configured. Both SDK functions set an
    ``_initialized`` flag as a side effect, so calling them with empty arguments
    would permanently suppress the environment fallback for the life of the
    process. ``None`` therefore returns without touching anything, and only an
    explicit policy object configures.

    ## Failure is not swallowed

    An exception here means the SDK's guardrail API is not the shape this worker
    was written against. Continuing would run the agent with whatever policy the
    globals last held — which, on a shared worker, is the previous run's. That is
    worse than refusing the command.
    """

    policy = payload.toolkit_guardrails
    if policy is None:
        return

    security = importlib.import_module("elitea_sdk.runtime.toolkits.security")
    security.configure_blocklist(
        blocked_toolkits=list(policy.get("blocked_toolkits") or []),
        blocked_tools=dict(policy.get("blocked_tools") or {}),
    )
    security.configure_sensitive_tools(
        sensitive_tools=dict(policy.get("sensitive_tools") or {}),
        # Empty string, not None: the SDK treats a falsy value as "use my
        # default", and the platform already substituted its own defaults when
        # the operator left the field blank. Passing the resolved value through
        # keeps one source of truth for the dialog copy.
        company_name=policy.get("sensitive_action_company_name") or None,
        message_template=policy.get("sensitive_action_message_template") or None,
    )


def _require_initial_agent_kernel(payload: AgentExecutionPayload) -> None:
    """Keep partial behavior unreachable instead of silently drifting."""

    hitl_resume = payload.hitl_resume or bool(payload.hitl_decisions)
    authorization_resume = _is_authorization_resume(payload)
    # #606: input_attachments is no longer one of these. It was refused here
    # because nothing consumed it, so a turn carrying a file would silently
    # have answered without it — a refusal was the honest outcome. It now has
    # a consumer: the chunks are spliced into the human message and documents
    # are read through the SDK artifact toolkit
    # (_invoke_initial_agent / agents/attachments.py), which is pylon's own
    # behaviour (utils/chat_history.py:67-73, rpc/chat_all.py:344-377). The
    # other three disjuncts keep refusing: each still names a path with no
    # implementation behind it.
    if (
        payload.checkpoint_id
        or payload.parallel_reconcile is not None
        or payload.parallel_terminal_errors
    ):
        raise UnsupportedCapability(
            "This agent execution requires a parity path that is not admitted yet."
        )
    if hitl_resume:
        _require_in_process_hitl_resume(payload)
    elif authorization_resume:
        _require_authorization_resume(payload)
    elif payload.should_continue:
        raise UnsupportedCapability(
            "This agent execution requires a parity path that is not admitted yet."
        )
    if not payload.thread_id and not payload.conversation_id:
        raise UnsupportedCapability(
            "A durable agent thread identity is required for this parity path."
        )


def _is_authorization_resume(payload: AgentExecutionPayload) -> bool:
    """Identify an explicit authorization decision on an agent capability."""

    return bool(
        payload.should_continue
        and not payload.hitl_resume
        and not payload.hitl_decisions
        and (payload.mcp_tokens or payload.user_declined_mcp_servers)
    )


def _require_authorization_resume(payload: AgentExecutionPayload) -> None:
    """Admit only an explicit delegated-toolkit authorize or skip resume."""

    if (
        payload.hitl_resume
        or payload.hitl_decisions
        or payload.hitl_action is not None
        or payload.hitl_value is not None
    ):
        raise UnsupportedCapability(
            "Toolkit authorization continuation cannot contain a HITL decision."
        )
    if not payload.mcp_tokens and not payload.user_declined_mcp_servers:
        raise UnsupportedCapability(
            "Toolkit authorization continuation requires an authorization or skip decision."
        )
    for declined in payload.user_declined_mcp_servers:
        if not isinstance(declined, dict):
            raise UnsupportedCapability(
                "The declined toolkit authorization decision is malformed."
            )
        server_url = declined.get("server_url")
        if not isinstance(server_url, str) or not server_url.strip():
            raise UnsupportedCapability(
                "The declined toolkit authorization server identity is required."
            )


def _require_in_process_hitl_resume(payload: AgentExecutionPayload) -> None:
    """Admit one atomic set of public, checkpoint-bound HITL decisions."""

    if not payload.hitl_resume:
        raise UnsupportedCapability("The HITL resume marker is required.")
    if not payload.should_continue:
        raise UnsupportedCapability("The HITL continuation marker is required.")
    if not 1 <= len(payload.hitl_decisions) <= 16:
        raise UnsupportedCapability(
            "Between one and sixteen HITL decisions are supported in one continuation."
        )
    if len(payload.hitl_decisions) == 1:
        decision = payload.hitl_decisions[0]
        if not isinstance(decision, dict):
            raise UnsupportedCapability("The HITL decision is malformed.")
        if payload.hitl_action != decision.get("action"):
            raise UnsupportedCapability("The HITL decision action is inconsistent.")
        decision_value = decision.get("value", "")
        if payload.hitl_value not in (None, decision_value):
            raise UnsupportedCapability("The HITL decision value is inconsistent.")
    elif payload.hitl_action is not None or payload.hitl_value is not None:
        raise UnsupportedCapability(
            "Parallel HITL decisions cannot contain a scalar HITL decision."
        )

    seen_interrupts: set[str] = set()
    allowed_keys = {
        "interrupt_id",
        "tool_call_id",
        "guardrail_type",
        "action",
        "value",
    }
    for decision in payload.hitl_decisions:
        if not isinstance(decision, dict) or set(decision) - allowed_keys:
            raise UnsupportedCapability("The HITL decision is malformed.")
        interrupt_id = decision.get("interrupt_id")
        if (
            not isinstance(interrupt_id, str)
            or not interrupt_id.strip()
            or interrupt_id in seen_interrupts
        ):
            raise UnsupportedCapability(
                "Each HITL decision requires one unique interrupt identity."
            )
        seen_interrupts.add(interrupt_id)
        tool_call_id = decision.get("tool_call_id")
        if tool_call_id is not None and not isinstance(tool_call_id, str):
            raise UnsupportedCapability("The HITL tool-call identity is malformed.")
        guardrail_type = decision.get("guardrail_type")
        if guardrail_type not in {None, "mcp_auth"}:
            raise UnsupportedCapability("The HITL guardrail type is not supported.")
        action = decision.get("action")
        allowed_actions = (
            _AUTHORIZATION_HITL_ACTIONS
            if guardrail_type == "mcp_auth"
            else _ROOT_HITL_ACTIONS
        )
        if action not in allowed_actions:
            raise UnsupportedCapability("The HITL action is not supported.")
        value = decision.get("value", "")
        if not isinstance(value, str):
            raise UnsupportedCapability("The HITL decision value is malformed.")
        carries_value = (
            guardrail_type != "mcp_auth" and action in _VALUE_BEARING_HITL_ACTIONS
        )
        if carries_value and not value:
            raise UnsupportedCapability("The HITL decision value is required.")
        if not carries_value and value:
            raise UnsupportedCapability("The HITL decision value is not allowed.")


def _llm_kwargs(value: dict[str, Any]) -> dict[str, Any]:
    kwargs = value.get("kwargs")
    if not isinstance(kwargs, dict):
        raise UnsupportedCapability("The agent model settings are malformed.")
    # Authentication and origin come exclusively from EliteaClientContext.
    return {
        key: deepcopy(item)
        for key, item in kwargs.items()
        if key not in {"api_key", "api_extra_headers", "base_url", "deployment"}
    }


def _hitl_resume_value(payload: AgentExecutionPayload) -> Any:
    """The value the SDK hands back to the paused tool's ``interrupt()``.

    Every action but ``answer`` resumes with the free text a reviewer typed, and
    it stays exactly the string that arrived.

    ``answer`` is the clarifying-question resume, and its value is one JSON
    object keyed by question id: `AnswerQuestionsControl` encodes it, every
    layer between the browser and here types `value` as a string, and the SDK's
    ``AskUserTool._format_answer`` turns it into the tool result the model
    reads. That renderer maps the answers onto the questions it asked ONLY when
    it is handed a mapping — given the encoded string it falls through to
    ``User answered: {"environment": "Staging"}`` and the model receives the
    wire format instead of the question it asked. The native runtime decodes
    first (``AskUserRequest::format_answer``,
    services/elitea-worker-rust/src/agents/internal_tools.rs), so decoding here
    is what makes the two runtimes answer the same model the same way.
    """

    value = payload.hitl_value or ""
    if payload.hitl_action != _HITL_ANSWER_ACTION or not value:
        return value
    return _decoded_clarifying_answer(value)


def _decoded_clarifying_answer(value: str) -> Any:
    """Decode one clarification answer, or leave it exactly as it arrived.

    Only the two shapes the browser produces are decoded: the ``{id: answer}``
    object an answered question set submits, and the bare JSON string the
    no-questions fallback submits. Anything else — a number, a bare array, an
    object carrying something the renderer would print as a Python repr, or
    text that is not JSON at all — is passed through unchanged.

    Passing through rather than refusing is deliberate, and it is the ONE place
    this deviates from the native runtime, which answers `InvalidInput` for
    those shapes. A refusal here arrives as a dead conversation — the pause
    stays parked and the next turn is refused as an overlap — whereas an
    unrecognised shape reaching the model as the user's own words costs a
    label. The value is already bounded and NUL-free by the time it gets here
    (`validCurrentHITLDecision`).
    """

    try:
        decoded = json.loads(value)
    except ValueError:
        return value
    if isinstance(decoded, str):
        return decoded
    if isinstance(decoded, dict) and all(
        _is_clarifying_answer_text(item) for item in decoded.values()
    ):
        return decoded
    return value


def _is_clarifying_answer_text(item: Any) -> bool:
    """One question's answer: a string, or the list a multi-select submits."""

    if isinstance(item, str):
        return True
    return isinstance(item, list) and all(isinstance(entry, str) for entry in item)


def _invoke_initial_agent(
    executor: Any,
    payload: AgentExecutionPayload,
    application_meta: Any,
    callbacks: list[Any],
    memory: Any,
    attachment_reader: Any = None,
) -> dict[str, Any]:
    from langchain_core.messages import HumanMessage

    messages = deepcopy(payload.chat_history)
    user_message_content = (
        payload.hitl_value
        if payload.hitl_resume
        and payload.hitl_action == "edit"
        and payload.hitl_value is not None
        else payload.user_input
    )
    # #606: the turn's own attachments, spliced in after the user's content.
    #
    # Pylon does not send attachments as a separate field at all — it stores
    # each one as a message item whose chunks the history projection FLATTENS
    # into the message content list (utils/chat_history.py:67-73). The
    # equivalent here is one content list: the user's text first, then the
    # attachment chunks, which is the order the model reads them in.
    #
    # ``payload`` is not mutated. It is the frozen projection of an immutable
    # input binding, and a retried or resumed command must rebuild the same
    # message from the same bytes rather than from whatever a previous attempt
    # spliced into it.
    attachment_chunks: list[Any] = []
    if payload.input_attachments:
        extracted: dict[tuple[str, str], str] = {}
        pending = pending_attachment_reads(payload.input_attachments)
        if pending and attachment_reader is not None:
            extracted = attachment_reader(payload, pending)
        attachment_chunks = attachment_message_chunks(
            payload.input_attachments,
            extracted,
        )
    messages.append(
        HumanMessage(
            content=human_message_content(user_message_content, attachment_chunks)
        )
    )
    configurable: dict[str, Any] = {
        "thread_id": payload.thread_id or payload.conversation_id,
        "invoked_skills": deepcopy(payload.invoked_skills),
        "attached_skills": deepcopy(payload.attached_skills),
    }
    invoke_config: dict[str, Any] = {"configurable": configurable}
    if callbacks:
        invoke_config["callbacks"] = list(callbacks)
    if isinstance(application_meta, dict):
        step_limit = application_meta.get("step_limit")
        if isinstance(step_limit, int) and not isinstance(step_limit, bool) and step_limit > 0:
            invoke_config["recursion_limit"] = step_limit
    authorization_resume = _is_authorization_resume(payload)
    if payload.hitl_resume:
        invoke_input: dict[str, Any] = {
            "messages": messages,
            "hitl_resume": True,
            "hitl_action": payload.hitl_action,
            "hitl_value": _hitl_resume_value(payload),
            "hitl_decisions": deepcopy(payload.hitl_decisions),
        }
    else:
        invoke_input = {"messages": messages}
    if authorization_resume:
        invoke_input, invoke_config = _configure_authorization_checkpoint_resume(
            executor,
            payload,
            invoke_input,
            invoke_config,
        )
    if payload.is_regenerate:
        _discard_regenerated_thread(memory, invoke_config)
    elif not payload.hitl_resume and not authorization_resume:
        _discard_failed_checkpoint(memory, invoke_config)
        _discard_failed_direct_application_checkpoints(
            memory,
            invoke_config,
            payload,
        )
    result = executor.invoke(invoke_input, invoke_config)
    if not isinstance(result, dict):
        raise TypeError("the SDK agent invocation returned a non-object result")
    for callback in callbacks:
        pause_result = getattr(callback, "authorization_pause_result", None)
        if not callable(pause_result):
            continue
        paused = pause_result()
        if paused is not None:
            return paused
    return result


def _configure_authorization_checkpoint_resume(
    executor: Any,
    payload: AgentExecutionPayload,
    invoke_input: dict[str, Any],
    invoke_config: dict[str, Any],
) -> tuple[dict[str, Any], dict[str, Any]]:
    """Replan the root graph after delegated authorization stopped execution.

    This is the current ``configure_checkpoint_resume`` behavior owned by the
    worker boundary. The SDK reports delegated authorization by bubbling an
    exception; it does not create a LangGraph ``interrupt()`` payload. Find
    only the latest pending root checkpoint, bind it to the invocation, and
    tell the root LLM whether authorization completed or was declined. This
    may create a new nested child invocation and therefore must not be
    described as exact-child resume. No transport or credential value is
    persisted here.
    """

    get_state_history = getattr(executor, "get_state_history", None)
    if not callable(get_state_history):
        raise DependencyUnavailable(
            "The delegated toolkit authorization checkpoint is unavailable."
        )
    states = list(
        get_state_history(
            {"configurable": {"thread_id": payload.thread_id or payload.conversation_id}}
        )
    )
    # Match the current worker: only the latest pending root state may be used
    # for reconstruction. Never time-travel behind a newer completed or failed
    # checkpoint.
    paused = states[0] if states and getattr(states[0], "next", ()) else None
    if paused is None:
        return invoke_input, invoke_config
    state_config = getattr(paused, "config", None)
    configurable = (
        state_config.get("configurable") if isinstance(state_config, dict) else None
    )
    checkpoint_id = (
        configurable.get("checkpoint_id") if isinstance(configurable, dict) else None
    )
    if not isinstance(checkpoint_id, str) or not checkpoint_id:
        raise DependencyUnavailable(
            "The delegated toolkit authorization checkpoint identity is unavailable."
        )

    declined = payload.user_declined_mcp_servers
    if declined and not payload.mcp_tokens:
        details: list[str] = []
        for item in declined:
            reason = item.get("skip_reason") or item.get("denial_reason") or ""
            server_url = item.get("server_url") or ""
            if isinstance(reason, str) and reason.strip():
                details.append(
                    f"{server_url.strip()}: {reason.strip()}"
                    if isinstance(server_url, str) and server_url.strip()
                    else reason.strip()
                )
        reason_text = "; ".join(details)
        continuation = "The user declined toolkit authorization for this session."
        if reason_text:
            continuation += f" Reason: {reason_text}."
        continuation += (
            " Please proceed with the original request without using the unavailable "
            "tools, or explain that you cannot complete it without them."
        )
    else:
        continuation = (
            "The required toolkit authorization has been completed. Please proceed "
            "with the original request using the newly available tools."
        )
    if isinstance(payload.user_input, str) and payload.user_input:
        continuation += f" Original request: {payload.user_input}"

    invoke_config["configurable"]["checkpoint_id"] = checkpoint_id
    invoke_config["should_continue"] = True
    return {"input": continuation}, invoke_config


def _discard_failed_checkpoint(memory: Any, config: dict[str, Any]) -> None:
    """Remove only a checkpoint with an explicit failed-task write.

    HITL, MCP/toolkit authorization, static interrupts and other intentional
    pauses do not carry ``__error__`` and remain untouched. Main already sends
    the authoritative persisted chat history for an ordinary next turn, so a
    failed graph task can be rebuilt after its incomplete checkpoint is gone.
    """

    get_tuple = getattr(memory, "get_tuple", None)
    delete_thread = getattr(memory, "delete_thread", None)
    if not callable(get_tuple) or not callable(delete_thread):
        return
    checkpoint = get_tuple(config)
    if checkpoint is None:
        return
    pending_writes = getattr(checkpoint, "pending_writes", ()) or ()
    if not any(
        isinstance(write, (tuple, list))
        and len(write) >= 2
        and write[1] == "__error__"
        for write in pending_writes
    ):
        return
    configurable = config.get("configurable")
    thread_id = (
        configurable.get("thread_id")
        if isinstance(configurable, dict)
        else None
    )
    if not isinstance(thread_id, str) or not thread_id:
        raise DependencyUnavailable(
            "The durable agent checkpoint identity is unavailable."
        )
    delete_thread(thread_id)
    configurable.pop("checkpoint_id", None)


def _discard_failed_direct_application_checkpoints(
    memory: Any,
    config: dict[str, Any],
    payload: AgentExecutionPayload,
) -> None:
    """Repair explicit failures on deterministic direct child threads only.

    The current SDK derives an ordinary direct application checkpoint as
    ``<parent-thread>:<clean-tool-name>``. Parallel children add a call ID and
    deeper descendants require the child application's own immutable snapshot;
    neither identity is guessed here. Intentional interrupts remain protected
    by ``_discard_failed_checkpoint``'s exact ``__error__`` predicate.
    """

    configurable = config.get("configurable")
    thread_id = (
        configurable.get("thread_id")
        if isinstance(configurable, dict)
        else None
    )
    if not isinstance(thread_id, str) or not thread_id:
        return
    for tool_name in _direct_application_tool_names(payload):
        _discard_failed_checkpoint(
            memory,
            {"configurable": {"thread_id": f"{thread_id}:{tool_name}"}},
        )


def _direct_application_tool_names(
    payload: AgentExecutionPayload,
) -> tuple[str, ...]:
    tool_groups: list[Any] = [payload.tools]
    version_details = payload.application.get("version_details")
    if isinstance(version_details, dict):
        tool_groups.append(version_details.get("tools"))

    names: list[str] = []
    seen: set[str] = set()
    for group in tool_groups:
        if not isinstance(group, list):
            continue
        for tool in group:
            if not isinstance(tool, dict) or tool.get("type") != "application":
                continue
            name = tool.get("name")
            if not isinstance(name, str) or not name:
                continue
            cleaned = _CURRENT_APPLICATION_TOOL_NAME_PATTERN.sub("", name).replace(
                ".", "_"
            )
            if (
                not cleaned
                or len(cleaned.encode("utf-8"))
                > _MAX_CURRENT_APPLICATION_TOOL_NAME_BYTES
                or cleaned in seen
            ):
                continue
            seen.add(cleaned)
            names.append(cleaned)
    return tuple(names)


def _discard_regenerated_thread(memory: Any, config: dict[str, Any]) -> None:
    """Start explicit regeneration from Main's truncated durable history.

    Regeneration intentionally time-travels to the original question while
    reusing the browser response UUID. A checkpoint on the stable conversation
    thread may contain later turns or a paused graph, so it cannot be merged
    into that replacement run.
    """

    delete_thread = getattr(memory, "delete_thread", None)
    if not callable(delete_thread):
        raise DependencyUnavailable(
            "The durable agent checkpoint store cannot reset a regenerated thread."
        )
    configurable = config.get("configurable")
    thread_id = (
        configurable.get("thread_id")
        if isinstance(configurable, dict)
        else None
    )
    if not isinstance(thread_id, str) or not thread_id:
        raise DependencyUnavailable(
            "The durable agent checkpoint identity is unavailable."
        )
    delete_thread(thread_id)
    configurable.pop("checkpoint_id", None)


def _current_index_tool_name_compatibility(
    toolkit_config: dict[str, Any],
) -> dict[str, Any]:
    """Apply the current R-2.0.5 index-tool rename on an invocation-only copy."""

    result = deepcopy(toolkit_config)
    settings = result.get("settings")
    if not isinstance(settings, dict):
        return result
    selected_tools = settings.get("selected_tools")
    if not isinstance(selected_tools, list) or "list_collections" not in selected_tools:
        return result
    migrated = list(selected_tools)
    migrated.remove("list_collections")
    if "list_indexes" not in migrated:
        migrated.append("list_indexes")
    settings["selected_tools"] = migrated
    return result


def _is_sdk_budget_exceeded(error: Exception) -> bool:
    """Match only the admitted SDK's typed budget exception."""

    try:
        module = importlib.import_module("elitea_sdk.runtime.exceptions")
    except ImportError:
        return False
    error_type = getattr(module, "BudgetExceededError", None)
    return isinstance(error_type, type) and isinstance(error, error_type)


def _package_tree_digest(root: Path) -> str:
    digest = hashlib.sha256()
    paths = sorted(root.rglob("*.py"))
    for path in paths:
        relative = path.relative_to(root).as_posix().encode("utf-8")
        content = path.read_bytes()
        digest.update(len(relative).to_bytes(4, "big"))
        digest.update(relative)
        digest.update(len(content).to_bytes(8, "big"))
        digest.update(content)
    return digest.hexdigest()


def _require_complete_configuration_registry(module: Any) -> None:
    """Reject an SDK registry whose guarded imports were incomplete."""

    failed_imports = getattr(module, "FAILED_IMPORTS", None)
    if not isinstance(failed_imports, dict) or failed_imports:
        raise DependencyUnavailable(
            "The installed Elitea SDK configuration registry is incomplete."
        )


def _import_sdk_configurations() -> Any:
    """Load the current SDK registries in their only complete import order.

    SDK 0.8.26's GitHub configuration imports ``elitea_sdk.tools.utils``.
    Importing Configurations first therefore initializes the Tools package
    while the GitHub configuration module is only partially defined, and the
    SDK permanently records GitHub as a failed tool in that process. Loading
    Tools first lets the same SDK finish both registries without changing any
    provider behavior. Remove this ordering shim after the SDK breaks that
    package-initializer cycle.
    """

    importlib.import_module("elitea_sdk.tools")
    return importlib.import_module("elitea_sdk.configurations")


@lru_cache(maxsize=1)
def _indexing_client_type() -> type[Any]:
    with redirect_stdout(sys.stderr):
        module = importlib.import_module("elitea_sdk.runtime.clients.client")
    package_root = Path(module.__file__).resolve().parents[2]
    if _package_tree_digest(package_root) != SDK_PACKAGE_TREE_SHA256:
        raise DependencyUnavailable(
            "The installed Elitea SDK artifact does not match the admitted package tree."
        )
    return module.EliteAClient
