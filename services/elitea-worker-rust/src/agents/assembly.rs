//! Strict production `LlmAgent` assembly admission before credential redemption.
//!
//! Application `agent_type=agent` and ad-hoc turns share this direct ADK-Rust
//! `LlmAgent` profile. Pipelines use the graph compiler, while toolsets, MCP,
//! HITL, sessions and browser projection are composed around both runtimes by
//! Elitea-owned boundaries.

#![allow(dead_code)] // Production provider/session assembly remains disabled.

use adk_rust::Content;
use serde_json::{Map, Value};

use super::attachments;
use super::context_management::ContextManagementPlan;
use super::internal_tools::{InternalToolCatalog, InternalToolError};
use super::request::{AgentExecutionKind, AgentExecutionRequest, UserInput};
use super::runtime::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};
use super::variables::{self, AgentVariables};

const MAX_MODEL_NAME_BYTES: usize = 256;
const MAX_USER_INPUT_BYTES: usize = 512 * 1_024;
const MAX_CHAT_HISTORY_MESSAGES: usize = 999;
pub(super) const DEFAULT_AGENT_STEP_LIMIT: u32 = 25;
const MAX_AGENT_STEP_LIMIT: u32 = 1_024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ReasoningEffort {
    Low,
    Medium,
    High,
    None,
}

/// Provider dialect selected by the authoritative frozen Main input.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum OrdinaryModelProvider {
    OpenAiChat,
    NativeAnthropic,
}

/// Frozen model, history, and execution controls for the strict no-tool profile.
#[derive(Clone, Debug)]
pub(crate) struct OrdinaryNoToolProfile {
    kind: AgentExecutionKind,
    instructions: String,
    model_name: String,
    model_provider: OrdinaryModelProvider,
    model_project_id: u32,
    max_tokens: Option<u32>,
    reasoning_effort: Option<ReasoningEffort>,
    temperature: Option<f32>,
    step_limit: u32,
    chat_history: Vec<Content>,
    context_management: ContextManagementPlan,
    internal_tools: InternalToolCatalog,
}

impl OrdinaryNoToolProfile {
    /// Validate every unsupported current feature before PAT redemption.
    pub(crate) fn validate(
        request: &AgentExecutionRequest,
    ) -> Result<Self, NativeAgentAssemblyError> {
        Self::validate_with_mode(request, CommonProfileMode::Fresh)
    }

    /// Validate the same direct `LlmAgent` definition for one exact HITL resume.
    ///
    /// The decision payload itself is admitted separately by `DirectHitlDecision`;
    /// this mode only prevents the otherwise identical model/tool definition from
    /// being rejected because Main supplied the four continuation fields.
    pub(crate) fn validate_direct_hitl_resume(
        request: &AgentExecutionRequest,
    ) -> Result<Self, NativeAgentAssemblyError> {
        Self::validate_with_mode(request, CommonProfileMode::DirectGuardrailContinuation)
    }

    /// Validate a direct `LlmAgent` shell while Main supplies only one
    /// claim-fetched delegated-authorization decision.
    pub(crate) fn validate_delegated_authorization_resume(
        request: &AgentExecutionRequest,
    ) -> Result<Self, NativeAgentAssemblyError> {
        Self::validate_with_mode(request, CommonProfileMode::McpAuthorization)
    }

    /// Validate one fresh model call that continues visible root output.
    pub(crate) fn validate_output_continuation(
        request: &AgentExecutionRequest,
    ) -> Result<Self, NativeAgentAssemblyError> {
        Self::validate_with_mode(request, CommonProfileMode::OutputContinuation)
    }

    /// Validate one explicit regeneration from Main's truncated history.
    ///
    /// Regeneration is neither a fresh append nor an interrupt resume. Main
    /// freezes the history before the response being replaced, and the session
    /// boundary rebuilds the exact thread from that snapshot before running.
    pub(crate) fn validate_regeneration(
        request: &AgentExecutionRequest,
    ) -> Result<Self, NativeAgentAssemblyError> {
        Self::validate_with_mode(request, CommonProfileMode::Regenerate)
    }

    /// Validate the model/session shell shared by a stored pipeline.
    ///
    /// The model fields remain part of the frozen application contract even
    /// when the currently admitted pure/control graph does not call a model. Pipeline YAML is
    /// returned as instructions and parsed by the graph-owned boundary.
    pub(crate) fn validate_pipeline_shell(
        request: &AgentExecutionRequest,
        resume: bool,
    ) -> Result<Self, NativeAgentAssemblyError> {
        let mode = if resume {
            CommonProfileMode::Continuation
        } else if request.payload.is_regenerate {
            CommonProfileMode::Regenerate
        } else {
            CommonProfileMode::Fresh
        };
        let common = validate_common_profile(request, mode)?;
        Self::pipeline_shell(request, common)
    }

    pub(crate) fn validate_pipeline_mcp_authorization_shell(
        request: &AgentExecutionRequest,
    ) -> Result<Self, NativeAgentAssemblyError> {
        let common = validate_common_profile(request, CommonProfileMode::McpAuthorization)?;
        Self::pipeline_shell(request, common)
    }

    pub(crate) fn validate_pipeline_guardrail_authorization_shell(
        request: &AgentExecutionRequest,
    ) -> Result<Self, NativeAgentAssemblyError> {
        let common =
            validate_common_profile(request, CommonProfileMode::DirectGuardrailContinuation)?;
        Self::pipeline_shell(request, common)
    }

    fn pipeline_shell(
        request: &AgentExecutionRequest,
        common: CommonProfile,
    ) -> Result<Self, NativeAgentAssemblyError> {
        if request.kind != AgentExecutionKind::Application {
            return Err(unsupported_profile());
        }
        let model = application_model_for_agent_type(request, "pipeline")?;
        let internal_tools = application_internal_tools(request)?;
        Ok(Self {
            kind: request.kind,
            instructions: model.instructions,
            model_name: model.model_name,
            model_provider: model.model_provider,
            model_project_id: model.model_project_id,
            max_tokens: model.max_tokens,
            reasoning_effort: model.reasoning_effort,
            temperature: model.temperature,
            step_limit: validate_step_limit(request.payload.steps_limit)?,
            chat_history: common.chat_history,
            context_management: common.context_management,
            internal_tools,
        })
    }

    fn validate_with_mode(
        request: &AgentExecutionRequest,
        mode: CommonProfileMode,
    ) -> Result<Self, NativeAgentAssemblyError> {
        let common = validate_common_profile(request, mode)?;
        let model = match request.kind {
            AgentExecutionKind::Application => application_model(request)?,
            AgentExecutionKind::Adhoc => adhoc_model(request)?,
        };
        let internal_tools = match request.kind {
            AgentExecutionKind::Application => application_internal_tools(request)?,
            AgentExecutionKind::Adhoc => {
                InternalToolCatalog::from_names(&request.payload.internal_tools)
                    .map_err(internal_tool_profile_error)?
            }
        };
        Ok(Self {
            kind: request.kind,
            instructions: model.instructions,
            model_name: model.model_name,
            model_provider: model.model_provider,
            model_project_id: model.model_project_id,
            max_tokens: model.max_tokens,
            reasoning_effort: model.reasoning_effort,
            temperature: model.temperature,
            step_limit: validate_step_limit(request.payload.steps_limit)?,
            chat_history: common.chat_history,
            context_management: common.context_management,
            internal_tools,
        })
    }

    /// Validate one nested direct agent from Main's claim-materialized version.
    ///
    /// The current SDK falls back to the parent's model when an embedded child
    /// has null model settings. Rust preserves that behavior while giving the
    /// child a fresh provider invocation for every `AgentTool` call.
    pub(crate) fn from_nested_version(
        version: &Map<String, Value>,
        fallback: &Self,
    ) -> Result<Self, NativeAgentAssemblyError> {
        Self::from_nested_version_for_type(version, fallback, "agent")
    }

    /// Validate one nested stored pipeline while preserving the parent's model
    /// as the SDK-compatible fallback for its LLM/Decision nodes.
    pub(crate) fn from_nested_pipeline_version(
        version: &Map<String, Value>,
        fallback: &Self,
    ) -> Result<Self, NativeAgentAssemblyError> {
        Self::from_nested_version_for_type(version, fallback, "pipeline")
    }

    fn from_nested_version_for_type(
        version: &Map<String, Value>,
        fallback: &Self,
        expected_agent_type: &'static str,
    ) -> Result<Self, NativeAgentAssemblyError> {
        match version.get("agent_type") {
            None if expected_agent_type == "agent" => {}
            Some(Value::String(value)) if value == expected_agent_type => {}
            Some(Value::String(_)) => return Err(unsupported_profile()),
            Some(_) | None => return Err(invalid_profile()),
        }
        validate_feature_array(version.get("tools"), true)?;
        let internal_tools = internal_tools_from_version(version)?;
        validate_empty_feature_array(version.get("skills"), false)?;
        validate_application_meta(version.get("meta"))?;
        // A nested agent renders its OWN declared variables: the SDK reaches
        // one through `client.application()` too (`runtime/tools/
        // application.py:396`), which builds a fresh `LangChainAssistant` over
        // the child's version and resolves the child's instructions there. The
        // per-CALL overrides that path also passes — the arguments the model
        // supplies for a variable-shaped agent tool — are a separate feature
        // (a tool argument schema derived from the variable list) and stay
        // unimplemented; only the child's stored values are served here.
        let variables = AgentVariables::admit(version, None)?;
        let instructions = version
            .get("instructions")
            .and_then(Value::as_str)
            .filter(|value| bounded_instruction(value, expected_agent_type == "agent"))
            .ok_or_else(invalid_profile)?;
        let rendered = if expected_agent_type == "agent" {
            variables.render(instructions)
        } else {
            instructions.to_owned()
        };
        let instructions = rendered.as_str();
        let model = match version.get("llm_settings") {
            None | Some(Value::Null) => ValidatedModel {
                instructions: instructions.to_owned(),
                model_name: fallback.model_name.clone(),
                model_provider: fallback.model_provider,
                model_project_id: fallback.model_project_id,
                max_tokens: fallback.max_tokens,
                reasoning_effort: fallback.reasoning_effort,
                temperature: fallback.temperature,
            },
            Some(Value::Object(settings)) if settings.is_empty() => ValidatedModel {
                instructions: instructions.to_owned(),
                model_name: fallback.model_name.clone(),
                model_provider: fallback.model_provider,
                model_project_id: fallback.model_project_id,
                max_tokens: fallback.max_tokens,
                reasoning_effort: fallback.reasoning_effort,
                temperature: fallback.temperature,
            },
            Some(Value::Object(settings)) => {
                validate_model(settings, ModelFieldNames::APPLICATION, None, instructions)?
            }
            Some(_) => return Err(invalid_profile()),
        };
        Ok(Self {
            kind: AgentExecutionKind::Application,
            instructions: model.instructions,
            model_name: model.model_name,
            model_provider: model.model_provider,
            model_project_id: model.model_project_id,
            max_tokens: model.max_tokens,
            reasoning_effort: model.reasoning_effort,
            temperature: model.temperature,
            step_limit: fallback.step_limit,
            chat_history: Vec::new(),
            context_management: ContextManagementPlan::Disabled,
            internal_tools,
        })
    }

    #[must_use]
    pub(crate) const fn kind(&self) -> AgentExecutionKind {
        self.kind
    }

    #[must_use]
    pub(crate) fn instructions(&self) -> &str {
        &self.instructions
    }

    #[must_use]
    pub(crate) fn model_name(&self) -> &str {
        &self.model_name
    }

    #[must_use]
    pub(crate) const fn model_provider(&self) -> OrdinaryModelProvider {
        self.model_provider
    }

    #[must_use]
    pub(crate) const fn model_project_id(&self) -> u32 {
        self.model_project_id
    }

    #[must_use]
    pub(crate) const fn max_tokens(&self) -> Option<u32> {
        self.max_tokens
    }

    #[must_use]
    pub(crate) const fn reasoning_effort(&self) -> Option<ReasoningEffort> {
        self.reasoning_effort
    }

    #[must_use]
    pub(crate) const fn temperature(&self) -> Option<f32> {
        self.temperature
    }

    #[must_use]
    pub(crate) const fn step_limit(&self) -> u32 {
        self.step_limit
    }

    #[must_use]
    pub(crate) fn chat_history(&self) -> &[Content] {
        &self.chat_history
    }

    #[must_use]
    pub(crate) fn context_management(&self) -> ContextManagementPlan {
        self.context_management.clone()
    }

    #[must_use]
    pub(crate) const fn internal_tools(&self) -> InternalToolCatalog {
        self.internal_tools
    }
}

struct CommonProfile {
    chat_history: Vec<Content>,
    context_management: ContextManagementPlan,
}

#[derive(Clone, Copy, Eq, PartialEq)]
enum CommonProfileMode {
    Fresh,
    Regenerate,
    Continuation,
    DirectGuardrailContinuation,
    McpAuthorization,
    OutputContinuation,
}

fn validate_common_profile(
    request: &AgentExecutionRequest,
    mode: CommonProfileMode,
) -> Result<CommonProfile, NativeAgentAssemblyError> {
    let payload = &request.payload;
    match &payload.user_input {
        UserInput::ContentBlocks(_) => return Err(unsupported_profile()),
        UserInput::Text(text) if text.is_empty() || text.contains('\0') => {
            return Err(invalid_profile());
        }
        UserInput::Text(text) if text.len() > MAX_USER_INPUT_BYTES => {
            return Err(resource_exhausted_profile());
        }
        UserInput::Text(_) => {}
    }
    let chat_history = current_text_history(&payload.chat_history)?;
    let context_management = ContextManagementPlan::admit_current(
        &payload.context_settings,
        payload.conversation_id.as_deref(),
    )?;
    let allows_mcp_authority = matches!(
        mode,
        CommonProfileMode::DirectGuardrailContinuation | CommonProfileMode::McpAuthorization
    );
    let regeneration = mode == CommonProfileMode::Regenerate;
    // Fresh turns and regeneration can reuse session credentials without resuming a guard.
    let allows_session_tokens = allows_mcp_authority
        || matches!(
            mode,
            CommonProfileMode::Fresh | CommonProfileMode::Regenerate
        );
    let output_continuation = mode == CommonProfileMode::OutputContinuation;
    let valid_truncated_content = payload
        .truncated_content
        .as_deref()
        .is_some_and(|value| value.len() <= 64 * 1_024 && !value.contains('\0'));
    if (!allows_session_tokens && !payload.mcp_tokens.is_empty())
        || (!allows_mcp_authority
            && (!payload.ignored_mcp_servers.is_empty()
                || !payload.user_declined_mcp_servers.is_empty()))
        || payload.checkpoint_id.is_some()
        || payload.is_regenerate != regeneration
        || payload.supports_vision
        || payload.return_chat_history
        || !payload.invoked_skills.is_empty()
        || !payload.applied_skills.is_empty()
        || payload.auto_approve_sensitive_actions
        || !payload.attached_skills.is_empty()
        || payload.parallel_reconcile.is_some()
        || !payload.parallel_terminal_errors.is_empty()
        || payload.exception_handling_enabled == Some(true)
        || payload.debug_mode.is_some()
        || payload.next_input_suggestion.enabled
        || payload.debug
        || !payload.meta.is_empty()
        || payload.persona != "generic"
        || (output_continuation != valid_truncated_content)
    {
        return Err(unsupported_profile());
    }
    // #606: attachments are no longer an unsupported profile — their chunks are
    // rendered into the human message by `attachments::append_attachment_parts`
    // at session assembly. Their SHAPE is admitted here, before credential
    // redemption, so a chunk this runtime cannot put in front of a model is an
    // `InvalidInput` on a turn that never started rather than a provider error
    // in the middle of one.
    attachments::validate_input_attachments(&payload.input_attachments)?;
    let has_direct_hitl_fields = payload.should_continue
        || payload.hitl_resume
        || payload.hitl_action.is_some()
        || payload.hitl_value.is_some()
        || !payload.hitl_decisions.is_empty();
    let starts_without_interrupt = matches!(
        mode,
        CommonProfileMode::Fresh | CommonProfileMode::Regenerate
    );
    if (starts_without_interrupt && has_direct_hitl_fields)
        || (!starts_without_interrupt && !has_direct_hitl_fields)
        || (mode == CommonProfileMode::McpAuthorization
            && (payload.hitl_resume
                || payload.hitl_action.is_some()
                || payload.hitl_value.is_some()
                || !payload.hitl_decisions.is_empty()))
        || (output_continuation
            && (!payload.should_continue
                || payload.hitl_resume
                || payload.hitl_action.is_some()
                || payload.hitl_value.is_some()
                || !payload.hitl_decisions.is_empty()))
    {
        return Err(unsupported_profile());
    }
    if !payload
        .thread_id
        .as_deref()
        .is_some_and(bounded_runtime_identity)
        || !payload
            .conversation_id
            .as_deref()
            .is_some_and(bounded_runtime_identity)
        || payload
            .execution_generation
            .as_deref()
            .is_some_and(|value| !bounded_runtime_identity(value))
    {
        return Err(invalid_profile());
    }
    Ok(CommonProfile {
        chat_history,
        context_management,
    })
}

fn validate_step_limit(value: Option<u32>) -> Result<u32, NativeAgentAssemblyError> {
    match value {
        None => Ok(DEFAULT_AGENT_STEP_LIMIT),
        Some(value) if (1..=MAX_AGENT_STEP_LIMIT).contains(&value) => Ok(value),
        Some(_) => Err(invalid_profile()),
    }
}

fn current_text_history(history: &[Value]) -> Result<Vec<Content>, NativeAgentAssemblyError> {
    if history.len() > MAX_CHAT_HISTORY_MESSAGES {
        return Err(resource_exhausted_profile());
    }
    history.iter().map(current_text_history_message).collect()
}

fn current_text_history_message(value: &Value) -> Result<Content, NativeAgentAssemblyError> {
    let message = value.as_object().ok_or_else(invalid_profile)?;
    if message.len() != 3
        || !message
            .get("additional_kwargs")
            .and_then(Value::as_object)
            .is_some_and(Map::is_empty)
    {
        return Err(invalid_profile());
    }
    let role = match message.get("role").and_then(Value::as_str) {
        Some("user") => "user",
        Some("assistant") => "model",
        Some(_) => return Err(unsupported_profile()),
        None => return Err(invalid_profile()),
    };
    let parts = message
        .get("content")
        .and_then(Value::as_array)
        .filter(|parts| !parts.is_empty())
        .ok_or_else(invalid_profile)?;
    let mut content = Content::new(role);
    for part in parts {
        let part = part.as_object().ok_or_else(invalid_profile)?;
        if part.len() != 2 {
            return Err(invalid_profile());
        }
        match part.get("type").and_then(Value::as_str) {
            Some("text") => {}
            Some(_) => return Err(unsupported_profile()),
            None => return Err(invalid_profile()),
        }
        let text = part
            .get("text")
            .and_then(Value::as_str)
            .filter(|text| !text.is_empty() && !text.contains('\0'))
            .ok_or_else(invalid_profile)?;
        content = content.with_text(text);
    }
    Ok(content)
}

fn application_model(
    request: &AgentExecutionRequest,
) -> Result<ValidatedModel, NativeAgentAssemblyError> {
    application_model_for_agent_type(request, "agent")
}

fn application_internal_tools(
    request: &AgentExecutionRequest,
) -> Result<InternalToolCatalog, NativeAgentAssemblyError> {
    let version = request
        .payload
        .application
        .get("version_details")
        .and_then(Value::as_object)
        .ok_or_else(invalid_profile)?;
    let configured = internal_tools_from_version(version)?;
    let conversation = InternalToolCatalog::from_names(&request.payload.internal_tools)
        .map_err(internal_tool_profile_error)?;
    Ok(configured.merge(conversation))
}

fn internal_tools_from_version(
    version: &Map<String, Value>,
) -> Result<InternalToolCatalog, NativeAgentAssemblyError> {
    let root = InternalToolCatalog::from_values(version.get("internal_tools"))
        .map_err(internal_tool_profile_error)?;
    let meta = match version.get("meta") {
        None | Some(Value::Null) => InternalToolCatalog::default(),
        Some(Value::Object(meta)) => InternalToolCatalog::from_values(meta.get("internal_tools"))
            .map_err(internal_tool_profile_error)?,
        Some(_) => return Err(invalid_profile()),
    };
    Ok(root.merge(meta))
}

fn application_model_for_agent_type(
    request: &AgentExecutionRequest,
    expected_agent_type: &str,
) -> Result<ValidatedModel, NativeAgentAssemblyError> {
    let version = request
        .payload
        .application
        .get("version_details")
        .and_then(Value::as_object)
        .ok_or_else(invalid_profile)?;
    // Per-conversation VALUES for variables the version already declares,
    // projected from `chat_participant_mapping.entity_settings`
    // (`internal/db/queries/agent_chat.sql:8`). Main guarantees the array
    // shape (`agentexecution/start.go:184`), so a non-array here is malformed
    // input rather than an unserved capability.
    let participant_variables = request
        .payload
        .application
        .get("variables")
        .filter(|value| value.is_array())
        .ok_or_else(invalid_profile)?;
    match version.get("agent_type") {
        None if expected_agent_type == "agent" => {}
        Some(Value::String(value)) if value == expected_agent_type => {}
        Some(Value::String(_)) => return Err(unsupported_profile()),
        Some(_) | None => return Err(invalid_profile()),
    }
    validate_feature_array(version.get("tools"), true)?;
    InternalToolCatalog::from_values(version.get("internal_tools"))
        .map_err(internal_tool_profile_error)?;
    validate_empty_feature_array(version.get("skills"), false)?;
    validate_application_meta(version.get("meta"))?;
    let variables = AgentVariables::admit(version, Some(participant_variables))?;
    let instructions = version
        .get("instructions")
        .and_then(Value::as_str)
        .filter(|value| bounded_instruction(value, expected_agent_type == "agent"))
        .ok_or_else(invalid_profile)?;
    // A PIPELINE's `instructions` carry the graph YAML, and `assistant.py`'s
    // `pipeline()` (:886-905) hands `self.prompt` to `create_graph` WITHOUT
    // calling `_resolve_jinja2_variables`. Only the react-agent path (:794)
    // renders, so only `agent` renders here.
    let rendered = if expected_agent_type == "agent" {
        variables.render(instructions)
    } else {
        instructions.to_owned()
    };
    let instructions = rendered.as_str();
    let settings = version
        .get("llm_settings")
        .and_then(Value::as_object)
        .ok_or_else(invalid_profile)?;
    let kwargs = request
        .payload
        .llm
        .get("kwargs")
        .and_then(Value::as_object)
        .ok_or_else(invalid_profile)?;
    if kwargs.len() != 1 {
        return Err(unsupported_profile());
    }
    let compatible = match kwargs.get("openai_compatible") {
        Some(Value::Bool(value)) => *value,
        Some(_) => return Err(invalid_profile()),
        None => return Err(unsupported_profile()),
    };
    validate_model(
        settings,
        ModelFieldNames::APPLICATION,
        Some(compatible),
        instructions,
    )
}

fn validate_empty_feature_array(
    value: Option<&Value>,
    required: bool,
) -> Result<(), NativeAgentAssemblyError> {
    let Some(value) = value else {
        return if required {
            Err(invalid_profile())
        } else {
            Ok(())
        };
    };
    let values = value.as_array().ok_or_else(invalid_profile)?;
    if values.is_empty() {
        Ok(())
    } else {
        Err(unsupported_profile())
    }
}

fn validate_feature_array(
    value: Option<&Value>,
    required: bool,
) -> Result<(), NativeAgentAssemblyError> {
    let Some(value) = value else {
        return if required {
            Err(invalid_profile())
        } else {
            Ok(())
        };
    };
    value.as_array().map(|_| ()).ok_or_else(invalid_profile)
}

fn validate_application_meta(value: Option<&Value>) -> Result<(), NativeAgentAssemblyError> {
    let Some(value) = value.filter(|value| !value.is_null()) else {
        return Ok(());
    };
    let meta = value.as_object().ok_or_else(invalid_profile)?;
    validate_application_meta_step_limit(meta.get("step_limit"))?;
    InternalToolCatalog::from_values(meta.get("internal_tools"))
        .map_err(internal_tool_profile_error)?;
    match meta.get("lazy_tools_mode") {
        None | Some(Value::Bool(false)) => {}
        Some(Value::Bool(true)) => {
            // Smart Tools Selection — a form toggle, not a protocol field. The
            // runtime exposes every tool unconditionally, which is the mode's
            // SAFE side (lazy mode narrows exposure; ignoring it widens
            // nothing the author did not already attach), so this degrades
            // with a log instead of refusing the turn — the same contract as
            // `InternalToolCatalog`'s platform-name skip.
            tracing::warn!(
                event = "agent_internal_tool_skipped",
                reason_code = "internal_tool_unsupported",
                internal_tool = "lazy_tools_mode",
                "smart tools selection is unavailable in this runtime; every attached tool stays exposed"
            );
        }
        Some(_) => return Err(invalid_profile()),
    }
    validate_application_meta_variables(meta.get("variables"))
}

/// Admit the variable list in the shapes the platform actually stores.
///
/// Main folds a version's variables into `meta.variables` as an ARRAY — the
/// create path writes one only when it is non-empty, but the UPDATE path writes
/// it on presence, deliberately, so that deleting the last variable is
/// distinguishable from never having had one
/// (`internal/api/v2/applications/handler.go`). The result is that EVERY agent
/// re-saved through the edit page carries `"variables": []`.
///
/// A POPULATED list used to be refused here as an unsupported capability, which
/// was honest while nothing substituted them. `super::variables` now renders
/// them — the SDK's own Jinja2 semantics over the values Main really stores —
/// so both the empty and the populated shapes are admitted, and only a
/// collection that is neither an array nor an object, or an array holding
/// something other than objects, is still malformed input.
fn validate_application_meta_variables(
    value: Option<&Value>,
) -> Result<(), NativeAgentAssemblyError> {
    variables::validate_variables(value)
}

/// Admit the authored step limit where the control plane keeps it, without
/// letting it select the effective one.
///
/// The effective limit is `payload.steps_limit` and nothing else; Main derives
/// that field from this same key, so the two agree by construction. The key
/// itself cannot be refused: `versionFromBody` writes it into every version on
/// every save (`services/elitea-main/internal/api/v2/applications/handler.go`),
/// so refusing it refused every stored agent — a turn that Main admitted and
/// that then stopped here with nothing on screen. The Python worker reads the
/// same key to set its `LangGraph` recursion limit, which is why it stays in the
/// version rather than moving onto the input alone.
///
/// The bounds are `validate_step_limit`'s, deliberately: a value this profile
/// would refuse on the input must not pass unexamined on the version.
fn validate_application_meta_step_limit(
    value: Option<&Value>,
) -> Result<(), NativeAgentAssemblyError> {
    let Some(value) = value.filter(|value| !value.is_null()) else {
        return Ok(());
    };
    let limit = value
        .as_u64()
        .and_then(|limit| u32::try_from(limit).ok())
        .ok_or_else(invalid_profile)?;
    validate_step_limit(Some(limit)).map(|_| ())
}

fn adhoc_model(
    request: &AgentExecutionRequest,
) -> Result<ValidatedModel, NativeAgentAssemblyError> {
    let instructions = request
        .payload
        .application
        .get("instructions")
        .and_then(Value::as_str)
        .filter(|value| bounded_adhoc_instruction(value))
        .ok_or_else(invalid_profile)?;
    // An ad-hoc turn is `predict_agent`, which builds its assistant data with
    // `variables: []` (`clients/client.py:1270-1280`) and still runs the
    // react path — so the prompt IS rendered, with `current_date` as the only
    // defined name, and every other placeholder survives verbatim.
    let rendered = AgentVariables::default().render(instructions);
    let instructions = rendered.as_str();
    let kwargs = request
        .payload
        .llm
        .get("kwargs")
        .and_then(Value::as_object)
        .ok_or_else(invalid_profile)?;
    validate_model(kwargs, ModelFieldNames::ADHOC, None, instructions)
}

#[derive(Clone, Copy)]
struct ModelFieldNames {
    model: &'static str,
    allowed: &'static [&'static str],
}

impl ModelFieldNames {
    const ADHOC: Self = Self {
        model: "model",
        allowed: &[
            "model",
            "model_project_id",
            "max_tokens",
            "reasoning_effort",
            "temperature",
            "stream",
            "openai_compatible",
        ],
    };
    const APPLICATION: Self = Self {
        model: "model_name",
        allowed: &[
            "model_name",
            "model_project_id",
            "max_tokens",
            "reasoning_effort",
            "temperature",
            "openai_compatible",
        ],
    };
}

struct ValidatedModel {
    instructions: String,
    model_name: String,
    model_provider: OrdinaryModelProvider,
    model_project_id: u32,
    max_tokens: Option<u32>,
    reasoning_effort: Option<ReasoningEffort>,
    temperature: Option<f32>,
}

fn validate_model(
    settings: &Map<String, Value>,
    names: ModelFieldNames,
    compatibility_override: Option<bool>,
    instructions: &str,
) -> Result<ValidatedModel, NativeAgentAssemblyError> {
    if settings
        .keys()
        .any(|key| !names.allowed.contains(&key.as_str()))
        || settings
            .get("stream")
            .is_some_and(|value| value != &Value::Bool(true))
    {
        return Err(unsupported_profile());
    }
    let compatible = match compatibility_override {
        Some(value) => value,
        None => match settings.get("openai_compatible") {
            Some(Value::Bool(value)) => *value,
            None | Some(_) => return Err(invalid_profile()),
        },
    };
    let model_name = settings
        .get(names.model)
        .and_then(Value::as_str)
        .filter(|value| bounded_text(value, MAX_MODEL_NAME_BYTES))
        .ok_or_else(invalid_profile)?
        .to_owned();
    let model_project_id = positive_u32(settings.get("model_project_id"))?;
    let max_tokens = normalized_max_tokens(settings.get("max_tokens"))?;
    let reasoning_effort = settings
        .get("reasoning_effort")
        .filter(|value| !value.is_null())
        .map(parse_reasoning_effort)
        .transpose()?;
    let temperature = settings
        .get("temperature")
        .filter(|value| !value.is_null())
        .map(parse_temperature)
        .transpose()?;
    if temperature.is_some()
        && reasoning_effort.is_some_and(|effort| effort != ReasoningEffort::None)
    {
        return Err(invalid_profile());
    }
    let model_provider = if compatible || !anthropic_model_name(&model_name) {
        OrdinaryModelProvider::OpenAiChat
    } else {
        OrdinaryModelProvider::NativeAnthropic
    };
    Ok(ValidatedModel {
        instructions: instructions.to_owned(),
        model_name,
        model_provider,
        model_project_id,
        max_tokens,
        reasoning_effort,
        temperature,
    })
}

fn anthropic_model_name(model_name: &str) -> bool {
    let model_name = model_name.to_ascii_lowercase();
    model_name.contains("anthropic") || model_name.contains("claude")
}

fn positive_u32(value: Option<&Value>) -> Result<u32, NativeAgentAssemblyError> {
    value
        .and_then(Value::as_u64)
        .and_then(|value| u32::try_from(value).ok())
        .filter(|value| *value > 0 && i32::try_from(*value).is_ok())
        .ok_or_else(invalid_profile)
}

fn normalized_max_tokens(value: Option<&Value>) -> Result<Option<u32>, NativeAgentAssemblyError> {
    match value {
        None | Some(Value::Null) => Ok(None),
        Some(value) if value.as_i64() == Some(-1) => Ok(None),
        Some(value) => positive_u32(Some(value)).map(Some),
    }
}

fn parse_reasoning_effort(value: &Value) -> Result<ReasoningEffort, NativeAgentAssemblyError> {
    match value.as_str() {
        Some("low") => Ok(ReasoningEffort::Low),
        Some("medium") => Ok(ReasoningEffort::Medium),
        Some("high") => Ok(ReasoningEffort::High),
        Some("none") => Ok(ReasoningEffort::None),
        _ => Err(invalid_profile()),
    }
}

fn parse_temperature(value: &Value) -> Result<f32, NativeAgentAssemblyError> {
    serde_json::from_value::<f32>(value.clone())
        .ok()
        .filter(|value| value.is_finite() && *value >= 0.0 && *value <= 1.0)
        .ok_or_else(invalid_profile)
}

fn bounded_runtime_identity(value: &str) -> bool {
    bounded_text(value, 256)
}

/// Bound a stored version's `instructions`.
///
/// `allow_empty` is the direct-agent case. An agent with no system prompt is a
/// real thing a user can save — the create form does not require the field, and
/// nothing between the form and here fills it in — so refusing it made every
/// such agent answer "The execution input is invalid." on every turn, a
/// sentence that names neither the field nor the fix. The ad-hoc path in this
/// same file has always accepted an empty instruction
/// (`bounded_adhoc_instruction`), and an ad-hoc turn is exactly an agent
/// without a stored prompt, so refusing it here was the odd one out.
///
/// A PIPELINE keeps the non-empty rule: its `instructions` field carries the
/// graph YAML, and an empty graph is not an unconstrained agent — it is a
/// pipeline with nothing to run.
fn bounded_instruction(value: &str, allow_empty: bool) -> bool {
    (allow_empty || !value.is_empty()) && value.len() <= 64 * 1_024 && !value.contains('\0')
}

fn bounded_adhoc_instruction(value: &str) -> bool {
    value.len() <= 64 * 1_024 && !value.contains('\0')
}

fn bounded_text(value: &str, maximum: usize) -> bool {
    !value.is_empty()
        && value.len() <= maximum
        && !value.bytes().any(|byte| matches!(byte, b'\r' | b'\n' | 0))
}

fn unsupported_profile() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::UnsupportedCapability,
        "the authorized agent profile requires a capability that is not admitted yet",
    )
}

pub(super) fn invalid_profile() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidInput,
        "the authorized agent profile is malformed",
    )
}

fn internal_tool_profile_error(error: InternalToolError) -> NativeAgentAssemblyError {
    match error {
        InternalToolError::InvalidInput => invalid_profile(),
        InternalToolError::UnsupportedCapability => unsupported_profile(),
        InternalToolError::ResourceExhausted => resource_exhausted_profile(),
    }
}

pub(super) fn resource_exhausted_profile() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::ResourceExhausted,
        "the authorized agent profile exceeds its approved limit",
    )
}
