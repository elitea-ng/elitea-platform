/// Additional calls for one truncated answer, including its one boundary repair.
pub(crate) const MAX_OUTPUT_CONTINUATION_CALLS: u32 = 4;

// Match completed-answer capacity; the full encoded request has its own bound.
pub(super) const MAX_OUTPUT_CONTINUATION_BYTES: usize = 4 * 1_024 * 1_024;

use serde_json::{Map, Value};

/// Selects one of the two current agent assembly semantics.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AgentExecutionKind {
    Application,
    Adhoc,
}

/// Immutable content-addressed binding for one materialized agent input.
#[derive(Debug, Eq, PartialEq)]
pub struct AgentInputBinding {
    pub input_bundle_id: String,
    pub input_bundle_digest: [u8; 32],
    pub request_entry_id: String,
    pub request_immutable_version: String,
    pub request_content_digest: [u8; 32],
}

/// The current input permits either plain text or provider content blocks.
#[derive(Eq, PartialEq)]
pub enum UserInput {
    Text(String),
    ContentBlocks(Vec<Value>),
}

/// Validated, bounded guardrail snapshot resolved before dispatch.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct NextInputSuggestionPolicy {
    pub enabled: bool,
    pub min_response_chars: u32,
    pub timeout_seconds: u16,
}

impl Default for NextInputSuggestionPolicy {
    fn default() -> Self {
        Self {
            enabled: false,
            min_response_chars: 150,
            timeout_seconds: 15,
        }
    }
}

/// Fully validated agent data-plane input.
///
/// JSON-owned SDK documents remain owned values. Dispatch authority is not
/// represented here. Claim-fetched `mcp_tokens` can contain credentials, so the
/// payload deliberately implements neither `Clone` nor `Debug`.
#[allow(clippy::struct_excessive_bools)] // Mirrors distinct protocol flags without conflating semantics.
#[derive(Eq, PartialEq)]
pub struct AgentExecutionPayload {
    pub llm: Map<String, Value>,
    pub chat_history: Vec<Value>,
    pub user_input: UserInput,
    pub thread_id: Option<String>,
    pub checkpoint_id: Option<String>,
    pub debug: bool,
    pub tools: Vec<Value>,
    pub application: Map<String, Value>,
    pub internal_tools: Vec<String>,
    pub steps_limit: Option<u32>,
    pub mcp_tokens: Map<String, Value>,
    pub ignored_mcp_servers: Vec<Value>,
    pub user_declined_mcp_servers: Vec<Value>,
    pub should_continue: bool,
    pub hitl_resume: bool,
    pub hitl_action: Option<String>,
    pub hitl_value: Option<String>,
    pub hitl_decisions: Vec<Value>,
    pub execution_generation: Option<String>,
    pub is_regenerate: bool,
    pub meta: Map<String, Value>,
    pub conversation_id: Option<String>,
    pub persona: String,
    pub context_settings: Map<String, Value>,
    pub supports_vision: bool,
    pub return_chat_history: bool,
    pub invoked_skills: Vec<Value>,
    pub applied_skills: Vec<Value>,
    pub auto_approve_sensitive_actions: bool,
    pub attached_skills: Vec<Value>,
    pub input_attachments: Vec<Value>,
    pub parallel_reconcile: Option<Map<String, Value>>,
    pub parallel_terminal_errors: Vec<Value>,
    pub exception_handling_enabled: Option<bool>,
    pub debug_mode: Option<bool>,
    pub next_input_suggestion: NextInputSuggestionPolicy,
    /// Admission-resolved platform guardrails. `None` preserves the startup
    /// fallback for commands produced before field 38 existed; `Some({})`
    /// explicitly clears that fallback for this invocation.
    pub toolkit_guardrails: Option<Map<String, Value>>,
    /// Visible root-assistant output that ended on the provider token limit.
    /// Presence distinguishes output continuation from HITL/authorization.
    pub truncated_content: Option<String>,
    pub project_context: Option<ProjectContextSnapshot>,
    pub model_context_limits: Option<ModelContextLimits>,
    pub summary_model: Option<SummaryModelSnapshot>,
}

/// Main resolves this model and its limits before execution admission.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SummaryModelSnapshot {
    pub llm_settings: Map<String, Value>,
    pub model_context_limits: ModelContextLimits,
}

/// Authorized model limits from the language-neutral runtime contract.
#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ModelContextLimits {
    pub context_window_tokens: u32,
    pub max_output_tokens: u32,
    #[serde(default)]
    pub context_window_fallback: bool,
    #[serde(default)]
    pub max_output_fallback: bool,
    #[serde(default)]
    pub max_input_tokens: Option<u32>,
}

#[derive(Eq, PartialEq)]
pub struct AgentExecutionRequest {
    pub kind: AgentExecutionKind,
    pub binding: AgentInputBinding,
    pub payload: AgentExecutionPayload,
}

/// Main freezes this authorized content in the immutable input bundle.
#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProjectContextSnapshot {
    pub id: String,
    pub revision: String,
    pub scope: String,
    pub content: String,
    #[serde(default)]
    pub activation_description: String,
}
