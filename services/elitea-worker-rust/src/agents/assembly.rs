//! Strict production `LlmAgent` assembly admission before credential redemption.
//!
//! Application `agent_type=agent` and ad-hoc turns share this direct ADK-Rust
//! `LlmAgent` profile. Pipelines use the graph compiler, while toolsets, MCP,
//! HITL, sessions and browser projection are composed around both runtimes by
//! Elitea-owned boundaries.

#![allow(dead_code)] // Production provider/session assembly remains disabled.

use adk_rust::Content;
use elitea_agent_runtime::edit_profile::EditProfile;
use serde_json::{Map, Value};
use std::sync::Arc;

use super::attachments;
use super::context_management::ContextManagementPlan;
use super::internal_tools::{InternalToolCatalog, InternalToolError};
use super::request::{
    AgentExecutionKind, AgentExecutionRequest, MAX_AGENT_USER_INPUT_BYTES,
    MAX_OUTPUT_CONTINUATION_BYTES, UserInput,
};
use super::runtime::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};
use super::variables::{self, AgentVariables};

const MAX_MODEL_NAME_BYTES: usize = 256;
const MAX_CHAT_HISTORY_MESSAGES: usize = 999;
pub(super) const DEFAULT_AGENT_STEP_LIMIT: u32 = 25;
pub(crate) const MAX_AGENT_STEP_LIMIT: u32 = 1_024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ReasoningEffort {
    Low,
    Medium,
    High,
    /// `off` (`none` on the wire before ADR-0038).
    None,
    /// The runtime decides per step (ADR-0038 decision 4).
    Auto,
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
    /// The model's thinking control from the catalogue (ADR-0038).
    model_reasoning: Option<super::request::ModelReasoning>,
    temperature: Option<f32>,
    step_limit: u32,
    chat_history: Vec<Content>,
    context_management: ContextManagementPlan,
    /// The root's step compaction (`session_compaction`, #1368): when set,
    /// it replaces the durable compaction of the root request; nested agents
    /// and pipeline nodes keep the durable one.
    step_compaction: Option<super::context_management::CompactionPolicy>,
    context_budget: Option<super::context_budget::RequestContextBudget>,
    summary_model: Option<Arc<SummaryModelProfile>>,
    internal_tools: InternalToolCatalog,
    instruction_plan: super::instruction_authority::InstructionPlan,
    /// The version's `meta.edit_format` (`auto`, `str_replace`, `patch`),
    /// when it names one: with the model it picks the workspace's edit
    /// tools ([`Self::edit_profile`], #1214).
    edit_format: Option<String>,
    /// The version's `meta.output_contract` (ADR-0043 decision 2): the
    /// agent's final answer must meet it. A pipeline keeps its contracts on
    /// its LLM nodes, so a pipeline shell has none.
    output_contract: Option<Arc<elitea_agent_runtime::output_contract::OutputContract>>,
    /// The version's `meta.citations` (ADR-0043 decision 5). Only a root
    /// agent's binding uses it.
    citations: elitea_agent_runtime::grounding::CitationMode,
    /// The version's `meta.memory` (ADR-0050 decision 1b, #1607): binds the
    /// agent memory tools. Only a root application agent has it; Main
    /// recalls no agent memory for a nested agent or a pipeline.
    agent_memory: Option<super::agent_memory_tools::AgentMemoryBinding>,
    /// The run's unattended grant (ADR-0045). Only the root agent of a run
    /// has it; a nested agent pauses as before.
    unattended: Option<Arc<super::unattended::UnattendedGrant>>,
    /// The turn-taking floor (ADR-0050 decision 11, #1611). Only the root
    /// agent of a turn holds it; a nested agent or a pipeline has none.
    floor: Option<super::request::AgentFloor>,
    /// The tool envelope (ADR-0050 decision 3, #1613). Only the root agent
    /// of a turn has it: a nested agent or a pipeline keeps its tools as
    /// before.
    tool_envelope: Option<elitea_agent_runtime::tool_envelope::ToolEnvelope>,
    /// The version's `meta.capabilities.can_propose_agents` (ADR-0050
    /// decision 5a, #1615): binds `propose_agent` beside the sub-agents.
    /// Only a root application agent has it. Main checks it again, with the
    /// person's create permission, on every call.
    can_propose_agents: bool,
    /// The agent memory entries Main recalled into the instructions
    /// (#1826). Only a root application agent with `meta.memory` has them.
    recalled_memory: Vec<super::request::RecalledMemoryEntry>,
    /// The version's `meta.capabilities.can_review_agents` (ADR-0050
    /// decision 5l, #1616): an HR agent, bound `propose_dismissal`. Only a
    /// root application agent has it; Main checks it again on every call.
    can_review_agents: bool,
}

/// Admitted independently from the task model before credential redemption.
#[derive(Clone, Debug)]
pub(crate) struct SummaryModelProfile {
    pub(crate) model_name: String,
    pub(crate) model_project_id: u32,
    pub(crate) model_provider: OrdinaryModelProvider,
    pub(crate) max_tokens: u32,
    pub(crate) temperature: Option<f32>,
    pub(crate) context_budget: super::context_budget::RequestContextBudget,
    /// The summary model's thinking control: a summary call thinks off
    /// where the model can be switched.
    pub(crate) model_reasoning: Option<super::request::ModelReasoning>,
}

impl SummaryModelProfile {
    pub(crate) fn admit(
        snapshot: &super::request::SummaryModelSnapshot,
        settings: &Map<String, Value>,
    ) -> Result<Self, NativeAgentAssemblyError> {
        const KEYS: [&str; 5] = [
            "model_name",
            "model_project_id",
            "openai_compatible",
            "max_tokens",
            "temperature",
        ];
        if snapshot
            .llm_settings
            .keys()
            .any(|key| !KEYS.contains(&key.as_str()))
        {
            return Err(invalid_profile());
        }
        let model = validate_model(
            &snapshot.llm_settings,
            ModelFieldNames::APPLICATION,
            None,
            "",
        )?;
        check_temperature(&model, snapshot.model_reasoning)?;
        let max_tokens = model.max_tokens.ok_or_else(invalid_profile)?;
        if max_tokens > snapshot.model_context_limits.max_output_tokens {
            return Err(invalid_profile());
        }
        let context_budget = super::context_budget::RequestContextBudget::resolve(
            Some(snapshot.model_context_limits),
            settings,
            Some(max_tokens),
        )?
        .ok_or_else(invalid_profile)?;
        Ok(Self {
            model_name: model.model_name,
            model_project_id: model.model_project_id,
            model_provider: model.model_provider,
            max_tokens,
            temperature: model.temperature,
            context_budget,
            model_reasoning: snapshot.model_reasoning,
        })
    }
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

    /// Validate the source turn's frozen input for one idle compaction
    /// (ADR-0050 decision 9b2): an empty input carrying only its marker.
    pub(crate) fn validate_idle_compaction(
        request: &AgentExecutionRequest,
    ) -> Result<Self, NativeAgentAssemblyError> {
        Self::validate_with_mode(request, CommonProfileMode::IdleCompaction)
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
        let mode = if request
            .payload
            .meta
            .contains_key(super::graph::fanout_wake::FANOUT_WAKE_META_KEY)
        {
            // Its marker is checked strictly by `FanoutWake::from_payload`.
            CommonProfileMode::FanoutWake
        } else if resume {
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
        check_temperature(&model, request.payload.model_reasoning)?;
        let internal_tools = application_internal_tools(request)?;
        Ok(Self {
            summary_model: request
                .payload
                .summary_model
                .as_ref()
                .map(|snapshot| {
                    SummaryModelProfile::admit(snapshot, &request.payload.context_settings)
                        .map(Arc::new)
                })
                .transpose()?,
            context_budget: super::context_budget::RequestContextBudget::resolve(
                request.payload.model_context_limits,
                &request.payload.context_settings,
                model.max_tokens,
            )?,
            kind: request.kind,
            instructions: model.instructions,
            model_name: model.model_name,
            model_provider: model.model_provider,
            model_project_id: model.model_project_id,
            max_tokens: model.max_tokens,
            reasoning_effort: model.reasoning_effort,
            model_reasoning: request.payload.model_reasoning,
            temperature: model.temperature,
            step_limit: validate_step_limit(request.payload.steps_limit)?,
            chat_history: common.chat_history,
            context_management: common.context_management,
            step_compaction: common.step_compaction,
            internal_tools,
            instruction_plan: super::instruction_authority::InstructionPlan::admit(request)?,
            edit_format: application_edit_format(request),
            output_contract: None,
            citations: elitea_agent_runtime::grounding::CitationMode::Off,
            agent_memory: None,
            unattended: None,
            floor: None,
            tool_envelope: None,
            can_propose_agents: false,
            recalled_memory: Vec::new(),
            can_review_agents: false,
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
        check_temperature(&model, request.payload.model_reasoning)?;
        let internal_tools = match request.kind {
            AgentExecutionKind::Application => application_internal_tools(request)?,
            AgentExecutionKind::Adhoc => {
                InternalToolCatalog::from_names(&request.payload.internal_tools)
                    .map_err(internal_tool_profile_error)?
            }
        };
        Ok(Self {
            summary_model: request
                .payload
                .summary_model
                .as_ref()
                .map(|snapshot| {
                    SummaryModelProfile::admit(snapshot, &request.payload.context_settings)
                        .map(Arc::new)
                })
                .transpose()?,
            context_budget: super::context_budget::RequestContextBudget::resolve(
                request.payload.model_context_limits,
                &request.payload.context_settings,
                model.max_tokens,
            )?,
            kind: request.kind,
            instructions: model.instructions,
            model_name: model.model_name,
            model_provider: model.model_provider,
            model_project_id: model.model_project_id,
            max_tokens: model.max_tokens,
            reasoning_effort: model.reasoning_effort,
            model_reasoning: request.payload.model_reasoning,
            temperature: model.temperature,
            step_limit: validate_step_limit(request.payload.steps_limit)?,
            chat_history: common.chat_history,
            context_management: common.context_management,
            step_compaction: common.step_compaction,
            internal_tools,
            instruction_plan: super::instruction_authority::InstructionPlan::admit(request)?,
            edit_format: application_edit_format(request),
            output_contract: application_output_contract(request)?,
            citations: application_citations(request),
            agent_memory: application_agent_memory(request),
            unattended: None,
            floor: request.payload.floor.clone(),
            tool_envelope: request.payload.tool_envelope.clone(),
            can_propose_agents: application_can_propose_agents(request),
            recalled_memory: application_recalled_memory(request),
            can_review_agents: application_capability(
                request,
                super::propose_dismissal_tool::CAPABILITY_KEY,
            ),
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
        validate_feature_array(version.get("skills"), false)?;
        validate_application_meta(version.get("meta"))?;
        // Render stored defaults for the frozen base profile. ApplicationAgentTool
        // re-renders the original template with declared per-call overrides on a
        // local profile clone, including when rebuilding an interrupted child.
        let variables = AgentVariables::admit(version, None)?;
        let instructions = version
            .get("instructions")
            .and_then(Value::as_str)
            .ok_or_else(invalid_profile)
            .and_then(|value| admitted_instruction(value, expected_agent_type == "agent"))?;
        let rendered = if expected_agent_type == "agent" {
            with_version_soul(version, &variables.render(instructions))?
        } else {
            instructions.to_owned()
        };
        let model = nested_model(version, fallback, rendered.as_str())?;
        let model_reasoning = nested_model_reasoning(version, fallback, &model)?;
        check_temperature(&model, model_reasoning)?;
        Ok(Self {
            summary_model: fallback.summary_model.clone(),
            context_budget: {
                let limits = match version.get("model_context_limits") {
                    Some(value) => {
                        Some(serde_json::from_value(value.clone()).map_err(|_| invalid_profile())?)
                    }
                    None if model.model_name == fallback.model_name
                        && model.model_project_id == fallback.model_project_id =>
                    {
                        fallback.context_budget.map(|budget| budget.limits)
                    }
                    None => None,
                };
                match (limits, fallback.context_budget) {
                    (Some(limits), Some(budget)) => {
                        Some(budget.for_model(limits, model.max_tokens)?)
                    }
                    _ => super::context_budget::RequestContextBudget::resolve(
                        limits,
                        &Map::new(),
                        model.max_tokens,
                    )?,
                }
            },
            kind: AgentExecutionKind::Application,
            instructions: model.instructions,
            model_name: model.model_name,
            model_provider: model.model_provider,
            model_project_id: model.model_project_id,
            max_tokens: model.max_tokens,
            reasoning_effort: model.reasoning_effort,
            model_reasoning,
            temperature: model.temperature,
            step_limit: fallback.step_limit,
            chat_history: Vec::new(),
            // The child owns its history. Carry only the admitted policy;
            // its model capacity is resolved independently above.
            context_management: fallback.context_management.clone(),
            step_compaction: None,
            internal_tools,
            instruction_plan: super::instruction_authority::InstructionPlan::nested(
                version,
                &fallback.instruction_plan,
            )?,
            // A nested agent runs no workspace turn of its own.
            edit_format: None,
            output_contract: if expected_agent_type == "agent" {
                version_output_contract(version)?
            } else {
                None
            },
            // A nested agent's answer goes to its parent, not to the chat.
            citations: elitea_agent_runtime::grounding::CitationMode::Off,
            agent_memory: None,
            unattended: None,
            floor: None,
            tool_envelope: None,
            can_propose_agents: false,
            recalled_memory: Vec::new(),
            can_review_agents: false,
        })
    }

    #[must_use]
    pub(crate) const fn kind(&self) -> AgentExecutionKind {
        self.kind
    }

    #[must_use]
    pub(crate) fn instruction_plan(&self) -> &super::instruction_authority::InstructionPlan {
        &self.instruction_plan
    }

    #[must_use]
    pub(crate) fn instructions(&self) -> &str {
        &self.instructions
    }

    /// Replace only the invocation-local rendered prompt; all frozen capabilities stay intact.
    pub(super) fn with_instructions(&self, instructions: String) -> Self {
        Self {
            instructions,
            ..self.clone()
        }
    }

    /// Set the run's unattended grant (ADR-0045) on the root profile.
    #[must_use]
    pub(crate) fn with_unattended_grant(
        mut self,
        grant: Option<Arc<super::unattended::UnattendedGrant>>,
    ) -> Self {
        self.unattended = grant;
        self
    }

    /// The run's unattended grant, if any.
    #[must_use]
    pub(crate) fn unattended_grant(&self) -> Option<&super::unattended::UnattendedGrant> {
        self.unattended.as_deref()
    }

    /// The turn-taking floor this turn holds (ADR-0050 decision 11).
    #[must_use]
    pub(crate) const fn floor(&self) -> Option<&super::request::AgentFloor> {
        self.floor.as_ref()
    }

    /// The root agent's tool envelope (ADR-0050 decision 3).
    #[must_use]
    pub(crate) const fn tool_envelope(
        &self,
    ) -> Option<&elitea_agent_runtime::tool_envelope::ToolEnvelope> {
        self.tool_envelope.as_ref()
    }

    /// Whether the version lets the agent propose its sub-agents as saved
    /// agents (`meta.capabilities.can_propose_agents`).
    #[must_use]
    pub(crate) const fn can_propose_agents(&self) -> bool {
        self.can_propose_agents
    }

    /// The agent memory entries Main recalled into this turn (#1826).
    #[must_use]
    pub(crate) fn recalled_memory(&self) -> &[super::request::RecalledMemoryEntry] {
        &self.recalled_memory
    }

    /// Whether the version makes the agent an HR agent
    /// (`meta.capabilities.can_review_agents`).
    #[must_use]
    pub(crate) const fn can_review_agents(&self) -> bool {
        self.can_review_agents
    }

    /// The agent memory tools the version binds (`meta.memory`).
    #[must_use]
    pub(crate) const fn agent_memory(
        &self,
    ) -> Option<super::agent_memory_tools::AgentMemoryBinding> {
        self.agent_memory
    }

    /// The agent's citation setting (`meta.citations`).
    #[must_use]
    pub(crate) const fn citations(&self) -> elitea_agent_runtime::grounding::CitationMode {
        self.citations
    }

    /// The agent's output contract, when its version declares one.
    #[must_use]
    pub(crate) fn output_contract(
        &self,
    ) -> Option<&Arc<elitea_agent_runtime::output_contract::OutputContract>> {
        self.output_contract.as_ref()
    }

    #[must_use]
    pub(crate) fn model_name(&self) -> &str {
        &self.model_name
    }

    /// The edit tools a workspace turn offers (#1214): the version's
    /// `meta.edit_format` when it names one, else the model family's — the
    /// desktop's choice for the same agent and model.
    #[must_use]
    pub(crate) fn edit_profile(&self) -> EditProfile {
        EditProfile::select(&self.model_name, self.edit_format.as_deref())
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
    pub(crate) const fn model_reasoning(&self) -> Option<super::request::ModelReasoning> {
        self.model_reasoning
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
    pub(crate) const fn context_budget(
        &self,
    ) -> Option<super::context_budget::RequestContextBudget> {
        self.context_budget
    }

    pub(crate) fn context_management(&self) -> ContextManagementPlan {
        self.context_management.clone()
    }

    pub(crate) fn step_compaction(&self) -> Option<super::context_management::CompactionPolicy> {
        self.step_compaction.clone()
    }

    pub(crate) fn summary_model(&self) -> Option<&SummaryModelProfile> {
        match self.context_management {
            ContextManagementPlan::Disabled => None,
            ContextManagementPlan::Summarize(_) => self.summary_model.as_deref(),
        }
    }

    #[must_use]
    pub(crate) const fn internal_tools(&self) -> InternalToolCatalog {
        self.internal_tools
    }
}

struct CommonProfile {
    chat_history: Vec<Content>,
    context_management: ContextManagementPlan,
    step_compaction: Option<super::context_management::CompactionPolicy>,
}

#[derive(Clone, Copy, Eq, PartialEq)]
enum CommonProfileMode {
    Fresh,
    Regenerate,
    Continuation,
    DirectGuardrailContinuation,
    McpAuthorization,
    OutputContinuation,
    /// A fixed Parallel's wake (contract §8.1): the parked execution's frozen
    /// fresh turn with an empty input, no decision fields and the wake marker
    /// as its only meta; its decisions come from the ledger.
    FanoutWake,
    /// An idle compaction (ADR-0050 decision 9b2): the source turn's frozen
    /// input with an empty input and the marker as its only meta.
    IdleCompaction,
}

#[allow(clippy::too_many_lines)] // Keep profile admission visibly ordered before credential redemption.
fn validate_common_profile(
    request: &AgentExecutionRequest,
    mode: CommonProfileMode,
) -> Result<CommonProfile, NativeAgentAssemblyError> {
    let payload = &request.payload;
    match &payload.user_input {
        UserInput::ContentBlocks(_) => return Err(unsupported_profile()),
        UserInput::Text(text)
            if (text.is_empty()
                && !matches!(
                    mode,
                    CommonProfileMode::FanoutWake | CommonProfileMode::IdleCompaction
                ))
                || text.contains('\0') =>
        {
            return Err(invalid_profile());
        }
        UserInput::Text(text) if text.len() > MAX_AGENT_USER_INPUT_BYTES => {
            return Err(resource_exhausted_profile());
        }
        UserInput::Text(_) => {}
    }
    let chat_history = current_text_history(&payload.chat_history)?;
    let context_management = ContextManagementPlan::admit_current(
        &payload.context_settings,
        payload.conversation_id.as_deref(),
    )?;
    let step_compaction = context_management.step_policy(&payload.context_settings)?;
    let allows_mcp_authority = matches!(
        mode,
        CommonProfileMode::DirectGuardrailContinuation | CommonProfileMode::McpAuthorization
    );
    let regeneration = mode == CommonProfileMode::Regenerate;
    // Fresh turns and regeneration can reuse session credentials without resuming a guard.
    let allows_session_tokens = allows_mcp_authority
        || matches!(
            mode,
            CommonProfileMode::Fresh
                | CommonProfileMode::Regenerate
                | CommonProfileMode::FanoutWake
                | CommonProfileMode::IdleCompaction
        );
    let output_continuation = mode == CommonProfileMode::OutputContinuation;
    let valid_truncated_content = payload
        .truncated_content
        .as_deref()
        .is_some_and(|value| value.len() <= MAX_OUTPUT_CONTINUATION_BYTES && !value.contains('\0'));
    if (!allows_session_tokens && !payload.mcp_tokens.is_empty())
        || (!allows_mcp_authority
            && (!payload.ignored_mcp_servers.is_empty()
                || !payload.user_declined_mcp_servers.is_empty()))
        || payload.checkpoint_id.is_some()
        || payload.is_regenerate != regeneration
        || payload.supports_vision
        || payload.return_chat_history
        || payload.auto_approve_sensitive_actions
        || payload.parallel_reconcile.is_some()
        || !payload.parallel_terminal_errors.is_empty()
        || payload.exception_handling_enabled == Some(true)
        || payload.debug_mode.is_some()
        || payload.next_input_suggestion.enabled
        || payload.debug
        || !(payload.meta.is_empty()
            || (mode == CommonProfileMode::FanoutWake
                && payload.meta.len() == 1
                && payload
                    .meta
                    .contains_key(super::graph::fanout_wake::FANOUT_WAKE_META_KEY))
            || (mode == CommonProfileMode::IdleCompaction
                && payload.meta.len() == 1
                && super::idle_compaction::requested(payload))
            || (mode == CommonProfileMode::DirectGuardrailContinuation
                && payload.meta.len() == 1
                && super::graph::static_tool_pause::parse_static_tool_decisions(&payload.meta)
                    .ok()
                    .flatten()
                    .is_some()))
        || !matches!(
            payload.persona.as_str(),
            "generic" | "qa" | "nerdy" | "quirky" | "cynical" | "none" | "bare"
        )
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
        CommonProfileMode::Fresh
            | CommonProfileMode::Regenerate
            | CommonProfileMode::FanoutWake
            | CommonProfileMode::IdleCompaction
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
        step_compaction,
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
    let entry = admitted_history_entry(value)?;
    let mut content = Content::new(entry.role);
    match entry.author {
        // One envelope per message: the parts are one author's words.
        Some((kind, name)) => {
            content = content.with_text(elitea_agent_runtime::untrusted::message_envelope(
                kind,
                name,
                &entry.texts.join("\n\n"),
            ));
        }
        None => {
            for text in entry.texts {
                content = content.with_text(text);
            }
        }
    }
    Ok(content)
}

/// The additional-kwargs key that marks a graph-state message as seeded from
/// the reader's context view (#1668), never written by a node of this run.
pub(crate) const CONTEXT_VIEW_MARKER_KEY: &str = "elitea_context_view";

/// The most a pipeline's seeded view carries, JSON-encoded (#1668). An LLM
/// node bounds its whole prompt at 512 KiB and one text block at 64 KiB, so
/// the seed keeps the newest part of the view that fits, never a message a
/// node would refuse, and leaves room for the node's own task.
const MAX_PIPELINE_VIEW_BYTES: usize = 256 * 1024;
const MAX_PIPELINE_VIEW_MESSAGE_BYTES: usize = 60 * 1024;

/// ADR-0050 decision 9 (#1668): the reader's view as graph-state messages,
/// the shape a pipeline's `chat_history` and `messages` channels hold.
///
/// Each item has the role an LLM node reads (`user`, or `assistant` for the
/// reader's own answers) and its text as blocks. A message by another agent,
/// or by a person other than the asker, is a `user` message whose one block
/// is the untrusted author envelope, exactly as an ordinary agent reads it
/// (`current_text_history`); its `additional_kwargs` keep `elitea_author`.
/// Every item carries [`CONTEXT_VIEW_MARKER_KEY`], so a result fallback never
/// takes an earlier turn's answer for this run's. Entries are admitted by the
/// same rules as the ordinary history; past the size bound the oldest drop.
pub(crate) fn pipeline_state_history(
    history: &[Value],
) -> Result<Vec<Value>, NativeAgentAssemblyError> {
    if history.len() > MAX_CHAT_HISTORY_MESSAGES {
        return Err(resource_exhausted_profile());
    }
    let mut items = Vec::with_capacity(history.len());
    let mut total = 0_usize;
    for value in history.iter().rev() {
        let entry = admitted_history_entry(value)?;
        let mut additional = Map::new();
        additional.insert(CONTEXT_VIEW_MARKER_KEY.to_owned(), Value::Bool(true));
        let texts = match entry.author {
            Some((kind, name)) => {
                additional.insert(
                    "elitea_author".to_owned(),
                    serde_json::json!({"kind": kind.as_str(), "name": name}),
                );
                vec![elitea_agent_runtime::untrusted::message_envelope(
                    kind,
                    name,
                    &entry.texts.join("\n\n"),
                )]
            }
            None => entry.texts.iter().map(|text| (*text).to_owned()).collect(),
        };
        if texts.iter().map(String::len).sum::<usize>() > MAX_PIPELINE_VIEW_MESSAGE_BYTES {
            break;
        }
        let blocks = texts
            .into_iter()
            .map(|text| serde_json::json!({"type": "text", "text": text}))
            .collect::<Vec<_>>();
        let item = serde_json::json!({
            "role": if entry.role == "model" { "assistant" } else { "user" },
            "content": blocks,
            "additional_kwargs": additional,
        });
        total = total.saturating_add(item.to_string().len());
        if total > MAX_PIPELINE_VIEW_BYTES {
            break;
        }
        items.push(item);
    }
    items.reverse();
    Ok(items)
}

/// One admitted `chat_history` entry: its model role, its author when
/// someone other than the reader or the asker wrote it, and its text parts.
struct HistoryEntry<'a> {
    role: &'static str,
    author: Option<(elitea_agent_runtime::untrusted::MessageAuthorKind, &'a str)>,
    texts: Vec<&'a str>,
}

fn admitted_history_entry(value: &Value) -> Result<HistoryEntry<'_>, NativeAgentAssemblyError> {
    let message = value.as_object().ok_or_else(invalid_profile)?;
    let additional = message
        .get("additional_kwargs")
        .and_then(Value::as_object)
        .ok_or_else(invalid_profile)?;
    if message.len() != 3 {
        return Err(invalid_profile());
    }
    // ADR-0050 decision 9a (issue 1603): a message by another agent, or by a
    // person other than the one asking, carries its author. It reaches the
    // model as a user-role, labelled, untrusted message, never as an unnamed
    // `assistant`; only the reader's own answers are `model`.
    let author = history_author(additional)?;
    let role = match (message.get("role").and_then(Value::as_str), author) {
        (Some("user"), _) => "user",
        (Some("assistant"), None) => "model",
        (Some("assistant"), Some(_)) | (None, _) => return Err(invalid_profile()),
        (Some(_), _) => return Err(unsupported_profile()),
    };
    let parts = message
        .get("content")
        .and_then(Value::as_array)
        .filter(|parts| !parts.is_empty())
        .ok_or_else(invalid_profile)?;
    let mut texts = Vec::with_capacity(parts.len());
    for part in parts {
        let part = part.as_object().ok_or_else(invalid_profile)?;
        // Demo issue 2: an earlier turn's attachment reaches history as its
        // stored header chunk, and that chunk keeps the `elitea_attachment`
        // marker that main wrote at admission (attachments.go,
        // `attachmentContentScaffold`; agent_chat.sql projects it verbatim).
        // Refusing the third key failed every follow-up question in a chat
        // with a file. Admit the marker in the shape the current turn admits
        // it, and read only `text`, which strips it before the model.
        let has_marker = part.contains_key(attachments::ATTACHMENT_MARKER_KEY);
        if part.len() != 2 + usize::from(has_marker) {
            return Err(invalid_profile());
        }
        attachments::validate_marker(part.get(attachments::ATTACHMENT_MARKER_KEY))?;
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
        texts.push(text);
    }
    Ok(HistoryEntry {
        role,
        author,
        texts,
    })
}

/// The `elitea_author` of a history message, when it names one; the only
/// key `additional_kwargs` may hold.
fn history_author(
    additional: &Map<String, Value>,
) -> Result<
    Option<(elitea_agent_runtime::untrusted::MessageAuthorKind, &str)>,
    NativeAgentAssemblyError,
> {
    use elitea_agent_runtime::untrusted::MessageAuthorKind;
    match additional.len() {
        0 => return Ok(None),
        1 => {}
        _ => return Err(invalid_profile()),
    }
    let author = additional
        .get("elitea_author")
        .and_then(Value::as_object)
        .filter(|author| author.len() == 2)
        .ok_or_else(invalid_profile)?;
    let kind = match author.get("kind").and_then(Value::as_str) {
        Some("agent") => MessageAuthorKind::Agent,
        Some("person") => MessageAuthorKind::Person,
        _ => return Err(invalid_profile()),
    };
    let name = author
        .get("name")
        .and_then(Value::as_str)
        .filter(|name| !name.contains('\0'))
        .ok_or_else(invalid_profile)?;
    Ok(Some((kind, name)))
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

/// An application version's `meta.edit_format`, when it is a string; an
/// ad-hoc turn has none.
fn application_edit_format(request: &AgentExecutionRequest) -> Option<String> {
    if request.kind != AgentExecutionKind::Application {
        return None;
    }
    request
        .payload
        .application
        .get("version_details")?
        .get("meta")?
        .get("edit_format")?
        .as_str()
        .map(str::to_owned)
}

/// An application version's `meta.output_contract`; an ad-hoc turn has
/// none. Main checked it on save; a contract this worker cannot admit refuses
/// the turn rather than running without it.
fn application_output_contract(
    request: &AgentExecutionRequest,
) -> Result<
    Option<Arc<elitea_agent_runtime::output_contract::OutputContract>>,
    NativeAgentAssemblyError,
> {
    if request.kind != AgentExecutionKind::Application {
        return Ok(None);
    }
    match request.payload.application.get("version_details") {
        Some(Value::Object(version)) => version_output_contract(version),
        _ => Ok(None),
    }
}

/// An application version's `meta.citations`; an ad-hoc turn has none.
///
/// Main checks the value on every write path. A stored value that this worker
/// cannot read (a row written before that check) turns citations off with a
/// warning. It does not refuse the turn: citations are a reading aid, and a
/// refusal would stop every chat turn of the agent. The warning names no
/// stored value.
fn application_agent_memory(
    request: &AgentExecutionRequest,
) -> Option<super::agent_memory_tools::AgentMemoryBinding> {
    if request.kind != AgentExecutionKind::Application {
        return None;
    }
    let meta = match request.payload.application.get("version_details") {
        Some(Value::Object(version)) => version.get("meta").and_then(Value::as_object),
        _ => None,
    };
    super::agent_memory_tools::AgentMemoryBinding::from_meta(meta)
}

/// The entries Main recalled into a root application agent's instructions
/// (#1826), kept only when the version binds agent memory: Main recalls
/// none otherwise, so an input that still names some names nothing a brief
/// may pass on.
fn application_recalled_memory(
    request: &AgentExecutionRequest,
) -> Vec<super::request::RecalledMemoryEntry> {
    if application_agent_memory(request).is_none() {
        return Vec::new();
    }
    request.payload.recalled_memory.clone()
}

/// `meta.capabilities.can_propose_agents` of a root application agent:
/// anything but a literal `true` is false.
fn application_can_propose_agents(request: &AgentExecutionRequest) -> bool {
    application_capability(request, super::propose_agent_tool::CAPABILITY_KEY)
}

/// `meta.capabilities.<key>` of a root application agent: anything but a
/// literal `true` is false.
fn application_capability(request: &AgentExecutionRequest, key: &str) -> bool {
    if request.kind != AgentExecutionKind::Application {
        return false;
    }
    request
        .payload
        .application
        .get("version_details")
        .and_then(|version| version.get("meta"))
        .and_then(|meta| meta.get(super::propose_agent_tool::CAPABILITIES_KEY))
        .and_then(|capabilities| capabilities.get(key))
        .and_then(Value::as_bool)
        .unwrap_or(false)
}

fn application_citations(
    request: &AgentExecutionRequest,
) -> elitea_agent_runtime::grounding::CitationMode {
    if request.kind != AgentExecutionKind::Application {
        return elitea_agent_runtime::grounding::CitationMode::Off;
    }
    let meta = match request.payload.application.get("version_details") {
        Some(Value::Object(version)) => version.get("meta").and_then(Value::as_object),
        _ => None,
    };
    elitea_agent_runtime::grounding::CitationMode::from_value(
        meta.and_then(|meta| meta.get(elitea_agent_runtime::grounding::META_KEY)),
    )
    .unwrap_or_else(|_| {
        tracing::warn!(
            event = "agent_citations_setting_invalid",
            "the stored meta.citations is not off, required or null; citations are off for this turn"
        );
        elitea_agent_runtime::grounding::CitationMode::Off
    })
}

fn version_output_contract(
    version: &Map<String, Value>,
) -> Result<
    Option<Arc<elitea_agent_runtime::output_contract::OutputContract>>,
    NativeAgentAssemblyError,
> {
    let contract = version
        .get("meta")
        .and_then(Value::as_object)
        .and_then(|meta| meta.get(elitea_agent_runtime::output_contract::META_KEY));
    elitea_agent_runtime::output_contract::OutputContract::from_value(contract)
        .map(|contract| contract.map(Arc::new))
        .map_err(|_| invalid_profile())
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
    validate_feature_array(version.get("skills"), false)?;
    validate_application_meta(version.get("meta"))?;
    let variables = AgentVariables::admit(version, Some(participant_variables))?;
    let instructions = version
        .get("instructions")
        .and_then(Value::as_str)
        .ok_or_else(invalid_profile)
        .and_then(|value| admitted_instruction(value, expected_agent_type == "agent"))?;
    // A PIPELINE's `instructions` carry the graph YAML, and `assistant.py`'s
    // `pipeline()` (:886-905) hands `self.prompt` to `create_graph` WITHOUT
    // calling `_resolve_jinja2_variables`. Only the react-agent path (:794)
    // renders, so only `agent` renders here.
    let rendered = if expected_agent_type == "agent" {
        with_version_soul(version, &variables.render(instructions))?
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

/// ADR-0050 decision 1a: an agent version's soul (`soul`, present only when
/// the version has one) goes before its rendered instructions. The preamble
/// (platform text and guardrails) is put before both by the model facade and
/// recalled memory already follows the instructions, so the system prompt
/// reads platform, guardrails, soul, instructions, memory. The soul is not
/// a template: it is never rendered with the version's variables. A
/// pipeline has no soul; its `instructions` are the graph.
///
/// A soul this worker cannot admit fails the profile (fail closed) rather
/// than running the agent without it: the soul carries the author's
/// `boundaries`, and an agent that silently lost them is worse than one that
/// does not start. Main admits every stored soul against the same schema, so
/// this fires only for a worker older than Main (UPGRADING: workers first).
pub(crate) fn with_version_soul(
    version: &Map<String, Value>,
    rendered_instructions: &str,
) -> Result<String, NativeAgentAssemblyError> {
    let soul = elitea_agent_runtime::soul::Soul::admit(version.get("soul"))
        .map_err(|_| invalid_profile())?;
    Ok(elitea_agent_runtime::soul::with_soul(
        soul.as_ref(),
        rendered_instructions,
    ))
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
    elitea_agent_runtime::output_contract::OutputContract::from_value(
        meta.get(elitea_agent_runtime::output_contract::META_KEY),
    )
    .map_err(|_| invalid_profile())?;
    InternalToolCatalog::from_values(meta.get("internal_tools"))
        .map_err(internal_tool_profile_error)?;
    match meta.get("lazy_tools_mode") {
        None | Some(Value::Bool(false)) => {}
        Some(Value::Bool(true)) => {
            // The BOOLEAN form of Smart Tools Selection is not a protocol
            // field: the form stores the toggle as the `lazy_tools_mode` entry
            // of `internal_tools`, which this runtime serves
            // (`libs/rust/agent-runtime/src/lazy_tools.rs`), and the SDK's
            // `client.application()` ignores `meta.lazy_tools_mode` the same
            // way (it passes the entry's presence explicitly). Ignoring it
            // exposes every tool, which is the mode's SAFE side (lazy mode
            // narrows exposure), so it degrades with a log instead of
            // refusing the turn.
            tracing::warn!(
                event = "agent_internal_tool_skipped",
                reason_code = "internal_tool_unsupported",
                internal_tool = "lazy_tools_mode",
                "meta.lazy_tools_mode is not read; smart tools selection is turned on by the lazy_tools_mode entry of internal_tools, so every attached tool stays exposed"
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
    let rendered = adhoc_persona_instructions(&request.payload.persona, rendered);
    let instructions = rendered.as_str();
    let kwargs = request
        .payload
        .llm
        .get("kwargs")
        .and_then(Value::as_object)
        .ok_or_else(invalid_profile)?;
    validate_model(kwargs, ModelFieldNames::ADHOC, None, instructions)
}

// SDK Assistant._prepare_prompt selects these styles only for ad-hoc chat.
// Keep project instructions and tool authority in their existing owners.
fn adhoc_persona_instructions(persona: &str, instructions: String) -> String {
    let style = match persona {
        "qa" => {
            "Use a precise testing perspective. Examine requirements, risks, edge cases, and reproducible evidence."
        }
        "nerdy" => {
            "Use an enthusiastic technical style. Explain concepts accurately with useful technical detail and relevant references."
        }
        "quirky" => {
            "Use a playful, imaginative style with light humor and creative analogies. Keep answers accurate and useful."
        }
        "cynical" => {
            "Use a skeptical, critically analytical style with dry humor. Challenge weak assumptions while remaining helpful and respectful."
        }
        // The SDK maps none to its default persona. Bare adds no persona text.
        _ => return instructions,
    };
    if instructions.is_empty() {
        style.to_owned()
    } else {
        format!("{instructions}\n\nResponse style: {style}")
    }
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
            "max_tokens_auto",
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
            "max_tokens_auto",
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

/// A nested version's model: its own `llm_settings`, or the parent's model
/// when it declares none (absent, null or an empty object).
fn nested_model(
    version: &Map<String, Value>,
    fallback: &OrdinaryNoToolProfile,
    instructions: &str,
) -> Result<ValidatedModel, NativeAgentAssemblyError> {
    match version.get("llm_settings") {
        Some(Value::Object(settings)) if !settings.is_empty() => {
            validate_model(settings, ModelFieldNames::APPLICATION, None, instructions)
        }
        None | Some(Value::Null | Value::Object(_)) => Ok(ValidatedModel {
            instructions: instructions.to_owned(),
            model_name: fallback.model_name.clone(),
            model_provider: fallback.model_provider,
            model_project_id: fallback.model_project_id,
            max_tokens: fallback.max_tokens,
            reasoning_effort: fallback.reasoning_effort,
            temperature: fallback.temperature,
        }),
        Some(_) => Err(invalid_profile()),
    }
}

/// The nested version's own thinking control when Main froze one; else the
/// parent's, when the child runs on the parent's model.
fn nested_model_reasoning(
    version: &Map<String, Value>,
    fallback: &OrdinaryNoToolProfile,
    model: &ValidatedModel,
) -> Result<Option<super::request::ModelReasoning>, NativeAgentAssemblyError> {
    match version.get("model_reasoning") {
        None | Some(Value::Null) => {
            let same_model = model.model_name == fallback.model_name
                && model.model_project_id == fallback.model_project_id;
            Ok(fallback.model_reasoning.filter(|_| same_model))
        }
        Some(value) => super::request::ModelReasoning::from_json(value)
            .map(Some)
            .ok_or_else(invalid_profile),
    }
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
    let selected_tokens = normalized_max_tokens(settings.get("max_tokens"))?;
    let max_tokens = match settings.get("max_tokens_auto") {
        Some(Value::Bool(true)) => None,
        None | Some(Value::Bool(false)) => selected_tokens,
        Some(_) => return Err(invalid_profile()),
    };
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

/// A temperature beside a setting that can think is refused on a model that
/// thinks by effort or by budget (an undeclared control is one of these):
/// the provider refuses the pair. A `toggle` model takes both, and a fixed
/// kind sends no thinking form (ADR-0038).
fn check_temperature(
    model: &ValidatedModel,
    reasoning: Option<super::request::ModelReasoning>,
) -> Result<(), NativeAgentAssemblyError> {
    let conflicts = model.temperature.is_some()
        && model
            .reasoning_effort
            .is_some_and(|effort| effort != ReasoningEffort::None)
        && reasoning.is_none_or(|reasoning| {
            matches!(
                reasoning.control,
                elitea_agent_runtime::thinking::ReasoningControl::Effort
                    | elitea_agent_runtime::thinking::ReasoningControl::Budget
            )
        });
    if conflicts {
        return Err(invalid_profile());
    }
    Ok(())
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

/// The canonical setting (ADR-0038 decision 1); `none` is `off`.
fn parse_reasoning_effort(value: &Value) -> Result<ReasoningEffort, NativeAgentAssemblyError> {
    use elitea_agent_runtime::thinking::Thinking;
    match value.as_str().and_then(Thinking::parse) {
        Some(Thinking::Low) => Ok(ReasoningEffort::Low),
        Some(Thinking::Medium) => Ok(ReasoningEffort::Medium),
        Some(Thinking::High) => Ok(ReasoningEffort::High),
        Some(Thinking::Off) => Ok(ReasoningEffort::None),
        Some(Thinking::Auto) => Ok(ReasoningEffort::Auto),
        None => Err(invalid_profile()),
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
/// pipeline with nothing to run. Its bound is the compiler's YAML bound, so the
/// profile never refuses a graph that Main admits and the compiler accepts.
fn bounded_instruction(value: &str, allow_empty: bool) -> bool {
    (allow_empty || !value.is_empty())
        && value.len() <= instruction_byte_bound(allow_empty)
        && !value.contains('\0')
}

const fn instruction_byte_bound(allow_empty: bool) -> usize {
    if allow_empty {
        super::request::MAX_AGENT_INSTRUCTION_BYTES
    } else {
        super::graph::compiler::MAX_PIPELINE_YAML_BYTES
    }
}

/// Admit the saved instructions, naming the bound when they are merely too big.
///
/// A malformed value (empty graph, NUL) stays a generic invalid profile. A value
/// past the byte bound is the registered agent-settings limit, so the person is
/// told which section to shrink instead of "The execution input is invalid.".
fn admitted_instruction(value: &str, allow_empty: bool) -> Result<&str, NativeAgentAssemblyError> {
    if bounded_instruction(value, allow_empty) {
        return Ok(value);
    }
    if value.len() > instruction_byte_bound(allow_empty) {
        return Err(if allow_empty {
            NativeAgentAssemblyError::agent_settings_limit(
                "the saved agent instructions exceed their size limit",
                "native_agent.instruction_bytes_exceeded",
                "instruction_bytes",
            )
        } else {
            NativeAgentAssemblyError::agent_settings_limit(
                "the saved pipeline YAML exceeds its size limit",
                "graph.pipeline.yaml_bytes_exceeded",
                "yaml_bytes",
            )
        });
    }
    Err(invalid_profile())
}

fn bounded_adhoc_instruction(value: &str) -> bool {
    value.len() <= super::request::MAX_AGENT_INSTRUCTION_BYTES && !value.contains('\0')
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
