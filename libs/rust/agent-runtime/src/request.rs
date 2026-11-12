/// Instruction content shares the bounded execution data-plane capacity.
pub const MAX_AGENT_INSTRUCTION_BYTES: usize = 8 * 1_024 * 1_024;

/// Decoded user text capacity shared by protocol and runtime assembly.
pub const MAX_AGENT_USER_INPUT_BYTES: usize = 512 * 1_024;

/// Additional calls for one truncated answer, including its one boundary repair.
pub const MAX_OUTPUT_CONTINUATION_CALLS: u32 = 4;

// Match completed-answer capacity; the full encoded request has its own bound.
pub const MAX_OUTPUT_CONTINUATION_BYTES: usize = 4 * 1_024 * 1_024;

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
    /// The administrator's platform preamble (ADR-0043 decision 3), frozen
    /// at admission. The command's content controls name its revision and
    /// digest; the text travels only here, in the input bundle.
    pub platform_preamble: Option<PlatformPreambleSnapshot>,
    /// The task model's thinking control (ADR-0038 decision 3), frozen at
    /// admission. `None` keeps the form the worker used before the
    /// catalogue declared one.
    pub model_reasoning: Option<ModelReasoning>,
    /// One reference per `chat_history` entry, in order (ADR-0050 decision
    /// 9c). Empty: the session keeps its stored history.
    pub chat_history_refs: Vec<ChatHistoryRef>,
    /// Why this agent holds the floor and who else can take it (ADR-0050
    /// decision 11, #1611). `None`: an input from before the floor, or a
    /// conversation with one agent.
    pub floor: Option<AgentFloor>,
    /// The root agent's tool envelope (ADR-0050 decision 3, #1613), frozen
    /// at admission and validated by the host's protocol. `None`: an input
    /// from before the envelope; no per-tool limit, and unattended turns and
    /// sub-agents get read-only tools.
    pub tool_envelope: Option<crate::tool_envelope::ToolEnvelope>,
    /// The chat message group uuid of this turn's own question (#1831),
    /// lowercase and hyphenated. `None`: a turn that writes no question of
    /// its own (a regeneration of an addressed turn, a continuation), or an
    /// input from before field 75.
    pub current_message_id: Option<String>,
    /// How the run's sub-agents may nest and which other models they may
    /// run on (ADR-0050 decision 4, #1824, #1825), frozen by Main. `None`:
    /// an input from before field 77; depth 1, and every sub-agent runs on
    /// its parent's model.
    pub subagent_policy: Option<SubAgentPolicy>,
    /// The eligibility mode of this reader's context view (`full`,
    /// `since_joined`, `addressed_only` or `none`; ADR-0050 decision 9,
    /// #1609), checked against the history by the host's protocol. The
    /// reader's durable session is bound to it (#1672): a session seeded
    /// under another mode is re-seeded from this view. `None`: an input from
    /// before the view.
    pub context_view_history: Option<String>,
    /// The binding of this reader's context view (#1902): Main's digest of
    /// every setting that decides which messages are eligible (the mode,
    /// `include_side_talk`, the join time under `since_joined`). The durable
    /// session is bound to it as well as to the mode, so a setting that
    /// narrows the view without changing the mode re-seeds it. `None`: an
    /// input from before `ContextViewV1.binding`; the mode alone binds.
    pub context_view_binding: Option<String>,
    /// The agent memory entries Main recalled into the instructions (#1826),
    /// in the order shown, each by id with its stored content: a
    /// `spawn_agent` brief may name these in `memory_entries`. Empty: none
    /// recalled, or an input from before field 76.
    pub recalled_memory: Vec<RecalledMemoryEntry>,
    /// The tag of those entries' `[memory N·TAG, ...]` marks (#1900,
    /// `subagents::memory_ref`): a brief cites an entry as `N·TAG` with it.
    /// `None`: no entry recalled into the instructions; the host cites what
    /// `recall` returns under a tag of its own.
    pub recalled_memory_tag: Option<String>,
}

/// The run's sub-agent policy (ADR-0050 decision 4).
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct SubAgentPolicy {
    /// 1 or 2: with 2 a sub-agent may start sub-agents of its own.
    pub max_depth: u32,
    /// The catalogue models other than the parent's that a sub-agent may
    /// name in `profile.model`, each in the summary model's shape.
    pub models: Vec<SummaryModelSnapshot>,
}

/// One agent memory entry Main recalled into a turn (#1826).
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RecalledMemoryEntry {
    /// The entry id, a positive decimal number.
    pub entry_id: String,
    /// The entry's text as stored.
    pub content: String,
}

/// The turn-taking floor this invocation holds (ADR-0050 decision 11).
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AgentFloor {
    pub cause: FloorCause,
    /// The other agents in the conversation the floor can be handed to.
    pub peers: Vec<AgentFloorPeer>,
    /// No human is waiting on this turn: bind only unattended tools (the
    /// envelope's `unattended_tools`, else the read-only tools).
    pub unattended_tools_only: bool,
}

/// What gave this agent the floor.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum FloorCause {
    HumanMessage,
    AgentMention,
    /// A run that a proactive agent's heartbeat started (ADR-0050 decision
    /// 2). Nobody waits on it, so Main also sets `unattended_tools_only`.
    Heartbeat,
}

/// One agent the floor can be handed to.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AgentFloorPeer {
    pub participant_id: i64,
    pub name: String,
    pub role: String,
}

/// Where one `chat_history` entry came from (ADR-0050 decision 9c).
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ChatHistoryRef {
    /// The chat message group uuid.
    pub message_id: String,
    /// The reader's own answer, or a message sent to the reader.
    pub readers_own: bool,
}

/// The catalogue's thinking data for one model (ADR-0038 decisions 3 and
/// 4): its control kind and the `auto` default for tool steps.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ModelReasoning {
    pub control: crate::thinking::ReasoningControl,
    pub tool_steps: bool,
    /// `reasoning_off_with_tools`: `auto` does not think on a request that
    /// carries tools (the provider refuses thinking beside tool calls).
    pub off_with_tools: bool,
}

impl ModelReasoning {
    /// The data from its wire form (`ModelReasoningV1`, or the same JSON
    /// shape on a nested version). An unknown control kind is `None`: the
    /// caller refuses the input, because a newer catalogue value that this
    /// runtime cannot translate must never be sent as another form.
    #[must_use]
    pub fn parse(control: &str, tool_steps: bool) -> Option<Self> {
        crate::thinking::ReasoningControl::parse(control).map(|control| Self {
            control,
            tool_steps,
            off_with_tools: false,
        })
    }

    /// The same data with `reasoning_off_with_tools` set.
    #[must_use]
    pub const fn with_off_with_tools(self, off_with_tools: bool) -> Self {
        Self {
            off_with_tools,
            ..self
        }
    }

    /// The data from a nested version's `model_reasoning` JSON object:
    /// `{"control": <kind>, "tool_steps": <bool>, "off_with_tools": <bool>}`.
    /// Other members are refused.
    #[must_use]
    pub fn from_json(value: &Value) -> Option<Self> {
        let fields = value.as_object()?;
        if fields
            .keys()
            .any(|key| key != "control" && key != "tool_steps" && key != "off_with_tools")
        {
            return None;
        }
        let flag = |name: &str| match fields.get(name) {
            None | Some(Value::Null) => Some(false),
            Some(Value::Bool(flag)) => Some(*flag),
            Some(_) => None,
        };
        let tool_steps = flag("tool_steps")?;
        let off_with_tools = flag("off_with_tools")?;
        Self::parse(fields.get("control")?.as_str()?, tool_steps)
            .map(|reasoning| reasoning.with_off_with_tools(off_with_tools))
    }

    /// The `auto` policy this data sets.
    #[must_use]
    pub const fn auto_policy(self) -> crate::thinking::AutoPolicy {
        crate::thinking::AutoPolicy {
            tool_steps: self.tool_steps,
            off_with_tools: self.off_with_tools,
        }
    }
}

/// One revision of the platform preamble, as admission froze it.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PlatformPreambleSnapshot {
    pub revision: u32,
    /// Lowercase hex SHA-256 of the text, checked when the input is read.
    pub sha256: String,
    pub text: String,
}

/// Main resolves this model and its limits before execution admission.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SummaryModelSnapshot {
    pub llm_settings: Map<String, Value>,
    pub model_context_limits: ModelContextLimits,
    /// The summary model's thinking control: a summary call sends thinking
    /// off where the model can be switched. `None` sends nothing.
    pub model_reasoning: Option<ModelReasoning>,
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
