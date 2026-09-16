//! Bounded first-class internal tools owned by the native agent runtime.
//!
//! Internal tools are selected by application/conversation configuration but
//! are not configured external toolkit snapshots. `ask_user` is intentionally
//! implemented as an ordinary ADK tool plus native confirmation: the durable
//! confirmation event parks the exact model call, and resume substitutes the
//! user's answer under that same function-call ID.

use std::sync::Arc;

use adk_rust::tool::BasicToolset;
use adk_rust::{AdkError, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};

use crate::protocol::control::ClaimBoundRuntimeContextAuthority;
use crate::transport::platform_client::PlatformClient;
use crate::transport::runtime_context::{
    ProjectContextWriteRequest, RuntimeContextError, SkillWriteRequest,
};

pub(crate) const ASK_USER_TOOL_NAME: &str = "ask_user";
/// The two chat-authored builder modules (#940 A8).
///
/// They are NOT in `PLATFORM_INTERNAL_TOOLS` below, and the distinction is the
/// whole point of that list: it names what this runtime RECOGNIZES and SKIPS.
/// These two it implements — `SkillsBuilderTool` and `ProjectContextBuilderTool`
/// in this file, writing through the claim-bound content listener — so they
/// bind real tools instead of a warning.
pub(crate) const SKILLS_BUILDER_TOOL_NAME: &str = "skills_builder";
pub(crate) const PROJECT_CONTEXT_BUILDER_TOOL_NAME: &str = "project_context_builder";
const SKILLS_BUILDER_TOOLSET_NAME: &str = "skills_builder";
const PROJECT_CONTEXT_BUILDER_TOOLSET_NAME: &str = "project_context_builder";
const CREATE_SKILL_TOOL_NAME: &str = "create_or_update_skill";
const WRITE_PROJECT_CONTEXT_TOOL_NAME: &str = "write_project_context";

/// The platform's authorable internal-tool names this runtime does NOT
/// implement yet — the create-agent form's own catalogue
/// (`apps/elitea-web/src/features/agents/lib/internalTools.ts`).
///
/// The Python worker is NOT the runtime that serves all of these, whatever
/// this comment used to claim. It skips `pyodide` for the same reason and with
/// the same event name, because its image ships no Deno and the SDK's sandbox
/// tool raises on construction without one
/// (`services/elitea-worker-python/src/elitea_worker/agents/internal_tools.py`).
///
/// A name on this list is SKIPPED with a warning rather than refused, for the
/// same reason `materialize_configured_toolsets` skips an unimplemented
/// toolkit family: these are honest capabilities of the product that a user
/// can toggle on from the agent form, and refusing the whole profile turned
/// every such toggle into an agent that stops answering with a message naming
/// neither the toggle nor the runtime. Measured in a live browser — the
/// previous UI seeded `internal_mcp` into every version and every one of
/// those agents was dead on this runtime.
///
/// A string OUTSIDE this list is still an unsupported capability: it names
/// nothing the platform can do, so the honest answer stays a refusal.
const PLATFORM_INTERNAL_TOOLS: &[&str] = &[
    "attachments",
    "data_analysis",
    "image_generation",
    "internal_mcp",
    "lazy_tools_mode",
    "planner",
    "pyodide",
    "swarm",
];
pub(crate) const ASK_USER_TOOLSET_NAME: &str = "ask_user";
pub(crate) const ASK_USER_GUARDRAIL_TYPE: &str = "clarifying_question";
pub(crate) const ASK_USER_ANSWER_ACTION: &str = "answer";
pub(crate) const ASK_USER_METADATA_KEY: &str = "elitea.ask_user.v1";

const MAX_QUESTIONS: usize = 4;
const MAX_OPTIONS: usize = 8;
const MAX_QUESTION_BYTES: usize = 2_000;
const MAX_HEADER_BYTES: usize = 128;
const MAX_OPTION_LABEL_BYTES: usize = 256;
const MAX_OPTION_DESCRIPTION_BYTES: usize = 1_000;
const MAX_QUESTION_ID_BYTES: usize = 64;
const MAX_ENCODED_REQUEST_BYTES: usize = 16 * 1_024;
const MAX_ANSWER_BYTES: usize = 16 * 1_024;

const ASK_USER_DESCRIPTION: &str = "Ask the user a clarifying question when information is missing or the requested choice is ambiguous instead of guessing. Present 1-4 questions, each with a short header and selectable options; the user can choose an option or provide another answer when allowed. Use this only for genuine decision points, not to request permission to run another tool.";

/// Strict, frozen set of runtime-owned internal tool capabilities.
///
/// `skipped` is a bitmask over `PLATFORM_INTERNAL_TOOLS`' own indices (it has
/// 8 entries, so a `u8` is exact) rather than a `Vec<&str>`: this type stays
/// `Copy`, which every existing caller already relies on
/// (`OrdinaryRuntimeBindings::with_internal_tools` takes it by value, and
/// `session.rs` reads a copy out of a struct it is about to move — see
/// `assemble_ordinary_native_with_sessions_and_runtime_catalogs`). A `Vec`
/// field would force every one of those call sites to clone or restructure.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) struct InternalToolCatalog {
    ask_user: bool,
    /// #940 A8's two modules. Plain flags beside `ask_user` rather than bits in
    /// `skipped`, because `skipped` means "recognized and NOT served" — putting
    /// an implemented capability there would make `skipped_tools_notice_text`
    /// tell the user the tool is unavailable in the same turn it runs.
    skills_builder: bool,
    project_context_builder: bool,
    skipped: u8,
}

impl InternalToolCatalog {
    #[must_use]
    pub(crate) const fn empty() -> Self {
        Self {
            ask_user: false,
            skills_builder: false,
            project_context_builder: false,
            skipped: 0,
        }
    }

    pub(crate) fn from_values(values: Option<&Value>) -> Result<Self, InternalToolError> {
        let Some(values) = values else {
            return Ok(Self::default());
        };
        let values = values.as_array().ok_or(InternalToolError::InvalidInput)?;
        let mut catalog = Self::default();
        for value in values {
            match value.as_str() {
                Some(ASK_USER_TOOL_NAME) => catalog.ask_user = true,
                Some(SKILLS_BUILDER_TOOL_NAME) => catalog.skills_builder = true,
                Some(PROJECT_CONTEXT_BUILDER_TOOL_NAME) => catalog.project_context_builder = true,
                Some(name) if PLATFORM_INTERNAL_TOOLS.contains(&name) => {
                    // Same contract as the toolkit-family skip
                    // (`agent_toolkit_skipped` in materialize.rs): the agent
                    // runs WITHOUT a capability its author asked for, and a
                    // silent drop is how that reads as "the toggle works" —
                    // which is why, as of #866, the LOG below is no longer
                    // the only trace: `skipped` records the name too, and
                    // `skipped_tools_notice_text` (below) turns it into a
                    // message `session.rs` seeds into the conversation
                    // itself, so the run says so, not only the server log.
                    tracing::warn!(
                        event = "agent_internal_tool_skipped",
                        reason_code = "internal_tool_unsupported",
                        internal_tool = name,
                        "internal tool is unavailable in this runtime and was omitted from the agent"
                    );
                    if let Some(index) = PLATFORM_INTERNAL_TOOLS
                        .iter()
                        .position(|candidate| *candidate == name)
                    {
                        // PLATFORM_INTERNAL_TOOLS has 8 entries; `skipped` is
                        // a u8, so every valid index fits in one bit.
                        catalog.skipped |= 1 << index;
                    }
                }
                Some(_) => return Err(InternalToolError::UnsupportedCapability),
                None => return Err(InternalToolError::InvalidInput),
            }
        }
        Ok(catalog)
    }

    pub(crate) fn from_names(values: &[String]) -> Result<Self, InternalToolError> {
        Self::from_values(Some(&Value::Array(
            values.iter().cloned().map(Value::String).collect(),
        )))
    }

    pub(crate) const fn merge(self, other: Self) -> Self {
        Self {
            ask_user: self.ask_user || other.ask_user,
            skills_builder: self.skills_builder || other.skills_builder,
            project_context_builder: self.project_context_builder || other.project_context_builder,
            skipped: self.skipped | other.skipped,
        }
    }

    #[must_use]
    pub(crate) const fn ask_user_enabled(self) -> bool {
        self.ask_user
    }

    #[must_use]
    pub(crate) const fn skills_builder_enabled(self) -> bool {
        self.skills_builder
    }

    #[must_use]
    pub(crate) const fn project_context_builder_enabled(self) -> bool {
        self.project_context_builder
    }

    #[must_use]
    pub(crate) const fn is_empty(self) -> bool {
        !self.ask_user && !self.skills_builder && !self.project_context_builder && self.skipped == 0
    }

    /// The platform-catalogue names this runtime skipped, in
    /// `PLATFORM_INTERNAL_TOOLS`' own order — stable and deterministic, so
    /// the notice text `skipped_tools_notice_text` builds from it does not
    /// depend on `HashMap`/set iteration order or on the order the caller
    /// happened to list them in `meta.internal_tools`.
    pub(crate) fn skipped_platform_tools(self) -> impl Iterator<Item = &'static str> {
        PLATFORM_INTERNAL_TOOLS
            .iter()
            .copied()
            .enumerate()
            .filter(move |(index, _)| self.skipped & (1 << index) != 0)
            .map(|(_, name)| name)
    }

    /// One line per skipped tool, in the exact shape #866 asked for: `internal
    /// tool 'NAME' is not available on this worker`. `None` when nothing was
    /// skipped, so a caller can `if let Some(text) = …` rather than always
    /// checking emptiness itself.
    ///
    /// This is the text `session.rs` seeds into the conversation
    /// (`skipped_tools_notice_event`) — the run-visible half of #866's fix.
    /// Before it, `agent_internal_tool_skipped` above was the ONLY trace: an
    /// operator reading logs could see the gap, but the user who turned the
    /// toggle on, and the model answering for them, could not.
    #[must_use]
    pub(crate) fn skipped_tools_notice_text(self) -> Option<String> {
        let mut lines = self
            .skipped_platform_tools()
            .map(|name| format!("internal tool '{name}' is not available on this worker"));
        let first = lines.next()?;
        Some(lines.fold(first, |mut text, line| {
            text.push('\n');
            text.push_str(&line);
            text
        }))
    }

    /// The toolsets this catalog binds, given the live platform authority.
    ///
    /// `builders` is an `Option` and not a required argument because the two
    /// builder tools are the only members of this catalog that need to reach
    /// main at EXECUTION time — `ask_user` is parked and resumed by the
    /// runtime itself, and every other recognized name is skipped. A caller
    /// that has no authority to lend (every unit test of the `ask_user` half,
    /// and any future assembly path that binds internal tools before
    /// redemption) passes `None` and gets exactly what it got before #940.
    ///
    /// A builder toggle with no authority is DROPPED, not bound-and-failing:
    /// a tool the model can call that answers "this deployment cannot write"
    /// on every call spends the turn discovering a fact the assembly already
    /// knew.
    pub(crate) fn toolsets(self, builders: Option<&BuilderToolAuthority>) -> Vec<Arc<dyn Toolset>> {
        let mut toolsets: Vec<Arc<dyn Toolset>> = Vec::new();
        if self.ask_user {
            toolsets.push(Arc::new(BasicToolset::new(
                ASK_USER_TOOLSET_NAME,
                vec![Arc::new(AskUserTool) as Arc<dyn Tool>],
            )));
        }
        let Some(builders) = builders else {
            return toolsets;
        };
        if self.skills_builder_enabled() {
            toolsets.push(Arc::new(BasicToolset::new(
                SKILLS_BUILDER_TOOLSET_NAME,
                vec![Arc::new(SkillsBuilderTool {
                    authority: builders.clone(),
                }) as Arc<dyn Tool>],
            )));
        }
        if self.project_context_builder_enabled() {
            toolsets.push(Arc::new(BasicToolset::new(
                PROJECT_CONTEXT_BUILDER_TOOLSET_NAME,
                vec![Arc::new(ProjectContextBuilderTool {
                    authority: builders.clone(),
                }) as Arc<dyn Tool>],
            )));
        }
        toolsets
    }
}

/// The live claim the two builder tools write under, shared for the duration of
/// one run.
///
/// `ClaimBoundRuntimeContextAuthority` is deliberately neither cloneable nor
/// formattable (`protocol::control`), and this type does NOT weaken either
/// property: it shares the single minted authority behind an `Arc` rather than
/// duplicating it, exactly as `ClaimScopedEliteaContext` is already shared for
/// the whole run by the model facade. What it does change is LIFETIME — the
/// authority now outlives assembly and lives as long as the run does — and that
/// is load-bearing rather than incidental: a tool the model calls mid-run has no
/// other way to hold the claim it must write under, and minting a second one
/// would be a second authorization this worker is not permitted to perform.
#[derive(Clone)]
pub(crate) struct BuilderToolAuthority {
    platform: Arc<PlatformClient>,
    authority: Arc<ClaimBoundRuntimeContextAuthority>,
}

impl BuilderToolAuthority {
    #[must_use]
    pub(crate) const fn new(
        platform: Arc<PlatformClient>,
        authority: Arc<ClaimBoundRuntimeContextAuthority>,
    ) -> Self {
        Self {
            platform,
            authority,
        }
    }
}

/// Turns one transport failure into the sentence the MODEL reads.
///
/// Every builder failure comes back as a tool RESULT, never an `AdkError` that
/// aborts the turn, and the three buckets are chosen so the model's next move
/// differs: a rejected document says write less, an unavailable dependency says
/// the platform could not be reached right now, and a refused claim says stop
/// asking. Returning an error instead would discard the conversation that
/// composed the document, which is the expensive half of this feature.
fn builder_failure_text(subject: &str, error: &RuntimeContextError) -> String {
    match error {
        RuntimeContextError::Rejected(_) | RuntimeContextError::ResourceExhausted(_) => format!(
            "The {subject} was not saved: the content is empty or larger than this platform              accepts. Shorten it and try once more."
        ),
        RuntimeContextError::AuthorizationFailed(_) | RuntimeContextError::NotFound(_) => format!(
            "The {subject} was not saved: this conversation is not authorized to write it.              Do not retry; tell the user."
        ),
        _ => format!(
            "The {subject} was not saved: the platform could not be reached.              Tell the user it was not saved."
        ),
    }
}

/// Reads one required string argument, rejecting anything that is not a
/// non-empty string. Returns `None` so the caller answers the model rather than
/// failing the turn, the same contract the write failures above follow.
fn builder_string_argument(arguments: &Value, key: &str) -> Option<String> {
    let value = arguments.as_object()?.get(key)?.as_str()?.trim();
    (!value.is_empty()).then(|| value.to_owned())
}

fn builder_optional_string(arguments: &Value, key: &str) -> String {
    arguments
        .as_object()
        .and_then(|object| object.get(key))
        .and_then(Value::as_str)
        .unwrap_or_default()
        .trim()
        .to_owned()
}

struct SkillsBuilderTool {
    authority: BuilderToolAuthority,
}

#[async_trait]
impl Tool for SkillsBuilderTool {
    fn name(&self) -> &str {
        CREATE_SKILL_TOOL_NAME
    }

    fn description(&self) -> &'static str {
        "Create a new Skill in this project, or update the existing Skill with the same name,          from what the user asked for. Provide the skill's name, a one-line description, and the          full instructions the skill should follow. Do not guess any of the three: if the user          has not said, ask them first."
    }

    fn is_read_only(&self) -> bool {
        false
    }

    fn is_concurrency_safe(&self) -> bool {
        false
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "name": {"type": "string", "minLength": 1, "maxLength": 128},
                "description": {"type": "string", "maxLength": 4096},
                "instructions": {"type": "string", "minLength": 1, "maxLength": 49152}
            },
            "required": ["name", "instructions"],
            "additionalProperties": false
        }))
    }

    fn response_schema(&self) -> Option<Value> {
        Some(json!({"type": "string"}))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        let Some(name) = builder_string_argument(&arguments, "name") else {
            return Ok(Value::String(
                "The skill was not saved: a non-empty name is required.".to_owned(),
            ));
        };
        let Some(instructions) = builder_string_argument(&arguments, "instructions") else {
            return Ok(Value::String(
                "The skill was not saved: non-empty instructions are required.".to_owned(),
            ));
        };
        let request = SkillWriteRequest {
            name,
            description: builder_optional_string(&arguments, "description"),
            instructions,
        };
        match self
            .authority
            .platform
            .write_skill(&self.authority.authority, &request)
            .await
        {
            Ok(outcome) => {
                let verb = if outcome.created {
                    "Created"
                } else {
                    "Updated"
                };
                Ok(Value::String(format!(
                    "{verb} Skill: [{}](/app/skills/{}). Tell the user by that name and link.",
                    outcome.name, outcome.skill_id
                )))
            }
            Err(error) => {
                tracing::warn!(
                    event = "agent_internal_tool_failed",
                    internal_tool = SKILLS_BUILDER_TOOL_NAME,
                    reason_code = error.code(),
                    "the skills builder could not write the skill"
                );
                Ok(Value::String(builder_failure_text("skill", &error)))
            }
        }
    }
}

struct ProjectContextBuilderTool {
    authority: BuilderToolAuthority,
}

#[async_trait]
impl Tool for ProjectContextBuilderTool {
    fn name(&self) -> &str {
        WRITE_PROJECT_CONTEXT_TOOL_NAME
    }

    fn description(&self) -> &'static str {
        "Write this project's Project Context — the shared background every conversation in the          project is given. A project has exactly ONE context, so this REPLACES it: to extend the          existing context, include the parts that should stay, and do not guess what they are.          Omit `enabled` to leave the project's current on/off setting alone."
    }

    fn is_read_only(&self) -> bool {
        false
    }

    fn is_concurrency_safe(&self) -> bool {
        false
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "content": {"type": "string", "minLength": 1, "maxLength": 49152},
                "enabled": {"type": "boolean"}
            },
            "required": ["content"],
            "additionalProperties": false
        }))
    }

    fn response_schema(&self) -> Option<Value> {
        Some(json!({"type": "string"}))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        let Some(content) = builder_string_argument(&arguments, "content") else {
            return Ok(Value::String(
                "The project context was not saved: non-empty content is required.".to_owned(),
            ));
        };
        let enabled = arguments
            .as_object()
            .and_then(|object| object.get("enabled"))
            .and_then(Value::as_bool);
        let request = ProjectContextWriteRequest { content, enabled };
        match self
            .authority
            .platform
            .write_project_context(&self.authority.authority, &request)
            .await
        {
            Ok(outcome) => {
                let verb = if outcome.created {
                    "Created"
                } else {
                    "Updated"
                };
                let state = if outcome.enabled {
                    "in effect for this project"
                } else {
                    "saved but currently switched off"
                };
                Ok(Value::String(format!(
                    "{verb} Project Context: [this project's context](/app/settings/project-context)                      — {state}. Tell the user by that name and link."
                )))
            }
            Err(error) => {
                tracing::warn!(
                    event = "agent_internal_tool_failed",
                    internal_tool = PROJECT_CONTEXT_BUILDER_TOOL_NAME,
                    reason_code = error.code(),
                    "the project context builder could not write the context"
                );
                Ok(Value::String(builder_failure_text(
                    "project context",
                    &error,
                )))
            }
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum InternalToolError {
    InvalidInput,
    UnsupportedCapability,
    ResourceExhausted,
}

#[derive(Clone, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct AskUserRequest {
    questions: Vec<AskUserQuestion>,
}

#[derive(Clone, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct AskUserQuestion {
    id: String,
    question: String,
    header: String,
    options: Vec<AskUserOption>,
    #[serde(rename = "multiSelect")]
    multi_select: bool,
    allow_other: bool,
    /// Whether the user may move past this question without answering it
    /// (#940 A8, ELITEA-2792).
    ///
    /// It defaults to FALSE, which is the same behaviour every question had
    /// before this field existed: the card's control stays disabled until the
    /// question is answered. Only a model that explicitly marks a question
    /// optional gets the relaxed rule, so a clarification built by an older
    /// prompt cannot accidentally become skippable.
    #[serde(default)]
    optional: bool,
}

#[derive(Clone, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
struct AskUserOption {
    label: String,
    description: String,
}

impl AskUserRequest {
    pub(crate) fn from_arguments(arguments: &Value) -> Result<Self, InternalToolError> {
        let object = arguments
            .as_object()
            .ok_or(InternalToolError::InvalidInput)?;
        if object.keys().any(|key| key != "questions") {
            return Err(InternalToolError::InvalidInput);
        }
        let raw_questions = object
            .get("questions")
            .and_then(Value::as_array)
            .ok_or(InternalToolError::InvalidInput)?;
        if raw_questions.is_empty() || raw_questions.len() > MAX_QUESTIONS {
            return Err(InternalToolError::ResourceExhausted);
        }
        let mut questions = Vec::with_capacity(raw_questions.len());
        for (index, raw) in raw_questions.iter().enumerate() {
            questions.push(AskUserQuestion::normalize(raw, index)?);
        }
        let request = Self { questions };
        if serde_json::to_vec(&request)
            .map_err(|_| InternalToolError::InvalidInput)?
            .len()
            > MAX_ENCODED_REQUEST_BYTES
        {
            return Err(InternalToolError::ResourceExhausted);
        }
        Ok(request)
    }

    pub(crate) fn message(&self) -> &str {
        self.questions
            .first()
            .map_or("Please answer to continue.", |question| {
                question.question.as_str()
            })
    }

    pub(crate) fn questions_value(&self) -> Value {
        serde_json::to_value(&self.questions).unwrap_or_else(|_| Value::Array(Vec::new()))
    }

    pub(crate) fn arguments_value(&self) -> Value {
        json!({"questions": self.questions_value()})
    }

    pub(crate) fn matches_arguments(&self, arguments: &Value) -> bool {
        Self::from_arguments(arguments).is_ok_and(|candidate| candidate == *self)
    }

    pub(crate) fn format_answer(&self, encoded: &str) -> Result<Value, InternalToolError> {
        if encoded.is_empty() || encoded.len() > MAX_ANSWER_BYTES || encoded.contains('\0') {
            return Err(InternalToolError::InvalidInput);
        }
        let decoded = serde_json::from_str::<Value>(encoded)
            .unwrap_or_else(|_| Value::String(encoded.to_owned()));
        let output = match decoded {
            Value::String(text) => {
                let text = text.trim();
                if text.is_empty() {
                    "User did not provide an answer.".to_owned()
                } else {
                    format!("User answered: {text}")
                }
            }
            Value::Object(answers) => self.format_answer_map(&answers)?,
            _ => return Err(InternalToolError::InvalidInput),
        };
        Ok(Value::String(output))
    }

    fn format_answer_map(&self, answers: &Map<String, Value>) -> Result<String, InternalToolError> {
        if answers.len() > MAX_QUESTIONS
            || answers
                .keys()
                .any(|key| !self.questions.iter().any(|question| question.id == *key))
        {
            return Err(InternalToolError::InvalidInput);
        }
        let mut lines = Vec::new();
        for question in &self.questions {
            let Some(value) = answers.get(&question.id) else {
                continue;
            };
            let rendered = match value {
                Value::String(value) => value.trim().to_owned(),
                Value::Array(values) if question.multi_select && values.len() <= MAX_OPTIONS => {
                    let values = values
                        .iter()
                        .map(Value::as_str)
                        .collect::<Option<Vec<_>>>()
                        .ok_or(InternalToolError::InvalidInput)?;
                    values
                        .into_iter()
                        .map(str::trim)
                        .filter(|value| !value.is_empty())
                        .collect::<Vec<_>>()
                        .join(", ")
                }
                _ => return Err(InternalToolError::InvalidInput),
            };
            if !rendered.is_empty() {
                let label = if question.question.is_empty() {
                    if question.header.is_empty() {
                        &question.id
                    } else {
                        &question.header
                    }
                } else {
                    &question.question
                };
                lines.push(format!("- {label}: {rendered}"));
            }
        }
        if lines.is_empty() {
            Ok("User did not provide an answer.".to_owned())
        } else {
            Ok(format!("User answered:\n{}", lines.join("\n")))
        }
    }
}

impl AskUserQuestion {
    fn normalize(raw: &Value, index: usize) -> Result<Self, InternalToolError> {
        let object = raw.as_object().ok_or(InternalToolError::InvalidInput)?;
        if object.keys().any(|key| {
            !matches!(
                key.as_str(),
                "id" | "question"
                    | "header"
                    | "options"
                    | "multi_select"
                    | "multiSelect"
                    | "allow_other"
                    | "optional"
            )
        }) {
            return Err(InternalToolError::InvalidInput);
        }
        let id = object
            .get("id")
            .and_then(Value::as_str)
            .map_or_else(|| format!("q{}", index + 1), ToOwned::to_owned);
        let question = optional_text(object.get("question"), MAX_QUESTION_BYTES)?;
        let header = optional_text(object.get("header"), MAX_HEADER_BYTES)?;
        if question.is_empty() && header.is_empty() {
            return Err(InternalToolError::InvalidInput);
        }
        if id.is_empty() || id.len() > MAX_QUESTION_ID_BYTES || id.chars().any(char::is_control) {
            return Err(InternalToolError::InvalidInput);
        }
        let raw_options = match object.get("options") {
            None | Some(Value::Null) => &[][..],
            Some(Value::Array(values)) if values.len() <= MAX_OPTIONS => values.as_slice(),
            Some(Value::Array(_)) => return Err(InternalToolError::ResourceExhausted),
            Some(_) => return Err(InternalToolError::InvalidInput),
        };
        let options = raw_options
            .iter()
            .map(AskUserOption::normalize)
            .collect::<Result<Vec<_>, _>>()?;
        let multi_select = bool_field(object, "multi_select")?
            .or(bool_field(object, "multiSelect")?)
            .unwrap_or(false);
        let allow_other = bool_field(object, "allow_other")?.unwrap_or(true);
        let optional = bool_field(object, "optional")?.unwrap_or(false);
        Ok(Self {
            id,
            question,
            header,
            options,
            multi_select,
            allow_other,
            optional,
        })
    }
}

impl AskUserOption {
    fn normalize(raw: &Value) -> Result<Self, InternalToolError> {
        match raw {
            Value::String(label) => Ok(Self {
                label: required_text(label, MAX_OPTION_LABEL_BYTES)?,
                description: String::new(),
            }),
            Value::Object(object)
                if object
                    .keys()
                    .all(|key| matches!(key.as_str(), "label" | "description")) =>
            {
                let label = object
                    .get("label")
                    .and_then(Value::as_str)
                    .ok_or(InternalToolError::InvalidInput)?;
                Ok(Self {
                    label: required_text(label, MAX_OPTION_LABEL_BYTES)?,
                    description: optional_text(
                        object.get("description"),
                        MAX_OPTION_DESCRIPTION_BYTES,
                    )?,
                })
            }
            _ => Err(InternalToolError::InvalidInput),
        }
    }
}

fn optional_text(value: Option<&Value>, maximum: usize) -> Result<String, InternalToolError> {
    match value {
        None | Some(Value::Null) => Ok(String::new()),
        Some(Value::String(value)) => bounded_text(value, maximum).map(ToOwned::to_owned),
        Some(_) => Err(InternalToolError::InvalidInput),
    }
}

fn required_text(value: &str, maximum: usize) -> Result<String, InternalToolError> {
    if value.trim().is_empty() {
        return Err(InternalToolError::InvalidInput);
    }
    bounded_text(value, maximum).map(ToOwned::to_owned)
}

fn bounded_text(value: &str, maximum: usize) -> Result<&str, InternalToolError> {
    if value.len() > maximum {
        return Err(InternalToolError::ResourceExhausted);
    }
    if value.contains('\0')
        || value
            .chars()
            .any(|character| character.is_control() && !matches!(character, '\n' | '\r' | '\t'))
    {
        return Err(InternalToolError::InvalidInput);
    }
    Ok(value)
}

fn bool_field(object: &Map<String, Value>, key: &str) -> Result<Option<bool>, InternalToolError> {
    match object.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Bool(value)) => Ok(Some(*value)),
        Some(_) => Err(InternalToolError::InvalidInput),
    }
}

pub(crate) fn encode_ask_user_request(request: &AskUserRequest) -> Option<String> {
    let encoded = serde_json::to_string(request).ok()?;
    (encoded.len() <= MAX_ENCODED_REQUEST_BYTES).then_some(encoded)
}

pub(crate) fn decode_ask_user_request(value: &str) -> Option<AskUserRequest> {
    if value.is_empty() || value.len() > MAX_ENCODED_REQUEST_BYTES {
        return None;
    }
    let request = serde_json::from_str::<AskUserRequest>(value).ok()?;
    AskUserRequest::from_arguments(&request.arguments_value()).ok()
}

struct AskUserTool;

#[async_trait]
impl Tool for AskUserTool {
    fn name(&self) -> &str {
        ASK_USER_TOOL_NAME
    }

    fn description(&self) -> &str {
        ASK_USER_DESCRIPTION
    }

    fn is_read_only(&self) -> bool {
        true
    }

    fn is_concurrency_safe(&self) -> bool {
        false
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "questions": {
                    "type": "array",
                    "minItems": 1,
                    "maxItems": MAX_QUESTIONS,
                    "items": {
                        "type": "object",
                        "properties": {
                            "question": {"type": "string", "minLength": 1, "maxLength": MAX_QUESTION_BYTES},
                            "header": {"type": "string", "maxLength": MAX_HEADER_BYTES},
                            "options": {
                                "type": "array",
                                "maxItems": MAX_OPTIONS,
                                "items": {
                                    "type": "object",
                                    "properties": {
                                        "label": {"type": "string", "minLength": 1, "maxLength": MAX_OPTION_LABEL_BYTES},
                                        "description": {"type": "string", "maxLength": MAX_OPTION_DESCRIPTION_BYTES}
                                    },
                                    "required": ["label"],
                                    "additionalProperties": false
                                }
                            },
                            "multi_select": {"type": "boolean"},
                            "allow_other": {"type": "boolean"},
                            "optional": {"type": "boolean"}
                        },
                        "required": ["question"],
                        "additionalProperties": false
                    }
                }
            },
            "required": ["questions"],
            "additionalProperties": false
        }))
    }

    fn response_schema(&self) -> Option<Value> {
        Some(json!({"type": "string"}))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_rust::Result<Value> {
        Err(AdkError::agent(
            "ask_user must be resumed through its durable clarification decision",
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    use adk_rust::tool::SimpleToolContext;

    #[test]
    fn normalizes_and_formats_structured_answer_in_question_order() {
        let request = AskUserRequest::from_arguments(&json!({
            "questions": [
                {"question": "Target?", "header": "Target", "options": [{"label": "A"}]},
                {"id": "mode", "question": "Mode?", "multi_select": true, "options": ["Fast", "Safe"]}
            ]
        }))
        .expect("request");
        assert_eq!(
            request
                .format_answer(r#"{"mode":["Safe","Fast"],"q1":"A"}"#)
                .expect("answer"),
            Value::String("User answered:\n- Target?: A\n- Mode?: Safe, Fast".to_owned())
        );
    }

    /// A repeated toggle must not become a second, identically named tool.
    ///
    /// `internal_tools` is a plain list, and the same name can arrive twice —
    /// the version carries it and the payload repeats it, and the two are
    /// folded together (`InternalToolCatalog::merge`). Setting a flag is
    /// idempotent by construction, but nothing pinned that the SERVED result
    /// is still one tool, and a model handed `ask_user` twice can call it
    /// twice under two function-call IDs for one clarification.
    #[tokio::test(flavor = "current_thread")]
    async fn a_duplicated_ask_user_toggle_still_serves_exactly_one_tool() {
        let duplicated = InternalToolCatalog::from_names(&[
            ASK_USER_TOOL_NAME.to_owned(),
            ASK_USER_TOOL_NAME.to_owned(),
        ])
        .expect("a repeated internal-tool name is not an error");
        let once = InternalToolCatalog::from_names(&[ASK_USER_TOOL_NAME.to_owned()])
            .expect("single internal-tool name");
        assert!(duplicated.ask_user_enabled());
        assert_eq!(duplicated, once);
        assert_eq!(duplicated.merge(once), once);

        for catalog in [duplicated, duplicated.merge(once)] {
            let toolsets = catalog.toolsets(None);
            assert_eq!(toolsets.len(), 1);
            let tools = toolsets[0]
                .tools(Arc::new(SimpleToolContext::new("internal-tools-test")))
                .await
                .expect("internal toolset tools");
            assert_eq!(
                tools.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
                [ASK_USER_TOOL_NAME]
            );
        }
    }

    #[test]
    fn platform_internal_tools_are_skipped_and_only_ask_user_is_served() {
        // Every name the agent form can author, at once — the catalogue must
        // come back with ask_user alone and no refusal, because a toggle a
        // user can reach must not stop the agent answering.
        let all_platform = [
            "ask_user",
            "attachments",
            "data_analysis",
            "image_generation",
            "internal_mcp",
            "lazy_tools_mode",
            "planner",
            "pyodide",
            "swarm",
        ]
        .map(ToOwned::to_owned);
        let catalog = InternalToolCatalog::from_names(&all_platform).expect("catalog");
        assert!(catalog.ask_user_enabled());
        assert_eq!(catalog.toolsets(None).len(), 1);

        // Outside the platform catalogue is still a refusal: it names nothing
        // the product can do, so skipping it would hide malformed config.
        assert_eq!(
            InternalToolCatalog::from_names(&["not_a_platform_tool".to_owned()]),
            Err(InternalToolError::UnsupportedCapability)
        );
        assert_eq!(
            InternalToolCatalog::from_values(Some(&json!([42]))),
            Err(InternalToolError::InvalidInput)
        );
    }

    // #866: a skipped platform tool is no longer traceable ONLY through the
    // server log — the catalog now records it, in the exact shape session.rs
    // seeds into the conversation as a visible notice.
    #[test]
    fn a_skipped_platform_tool_is_recorded_with_the_exact_866_notice_text() {
        let catalog = InternalToolCatalog::from_names(&["planner".to_owned()]).expect("catalog");
        assert_eq!(
            catalog.skipped_platform_tools().collect::<Vec<_>>(),
            ["planner"]
        );
        assert_eq!(
            catalog.skipped_tools_notice_text().as_deref(),
            Some("internal tool 'planner' is not available on this worker")
        );
        // ask_user is real and served, and this request never named it.
        assert!(!catalog.ask_user_enabled());
    }

    #[test]
    fn no_skipped_tools_means_no_notice() {
        let catalog =
            InternalToolCatalog::from_names(&[ASK_USER_TOOL_NAME.to_owned()]).expect("catalog");
        assert_eq!(catalog.skipped_platform_tools().count(), 0);
        assert_eq!(catalog.skipped_tools_notice_text(), None);
        assert!(
            !catalog.is_empty(),
            "ask_user alone is not an empty catalog"
        );

        assert!(
            InternalToolCatalog::empty()
                .skipped_tools_notice_text()
                .is_none()
        );
        assert!(InternalToolCatalog::empty().is_empty());
    }

    /// The notice text is deterministic and in `PLATFORM_INTERNAL_TOOLS`'
    /// OWN order — not the order the caller listed the names in — so it
    /// cannot flap between otherwise-identical requests.
    #[test]
    fn multiple_skipped_tools_are_reported_one_per_line_in_a_stable_order() {
        let listed_swarm_first = InternalToolCatalog::from_names(&[
            "swarm".to_owned(),
            "planner".to_owned(),
            "data_analysis".to_owned(),
        ])
        .expect("catalog");
        let listed_data_analysis_first = InternalToolCatalog::from_names(&[
            "data_analysis".to_owned(),
            "planner".to_owned(),
            "swarm".to_owned(),
        ])
        .expect("catalog");

        let expected = "internal tool 'data_analysis' is not available on this worker\n\
             internal tool 'planner' is not available on this worker\n\
             internal tool 'swarm' is not available on this worker";
        assert_eq!(
            listed_swarm_first.skipped_tools_notice_text().as_deref(),
            Some(expected)
        );
        assert_eq!(
            listed_swarm_first.skipped_tools_notice_text(),
            listed_data_analysis_first.skipped_tools_notice_text(),
            "the same SET of skipped tools must produce the same notice regardless of input order"
        );
    }

    /// `merge` is how a nested application's own internal-tool set combines
    /// with its parent's (`session.rs`); the skipped set must union the same
    /// way `ask_user_enabled` already does, not overwrite or drop one side.
    #[test]
    fn merge_unions_the_skipped_set_from_both_sides() {
        let mine = InternalToolCatalog::from_names(&["planner".to_owned()]).expect("catalog");
        let theirs = InternalToolCatalog::from_names(&["swarm".to_owned()]).expect("catalog");

        let mut merged = mine
            .merge(theirs)
            .skipped_platform_tools()
            .collect::<Vec<_>>();
        merged.sort_unstable();
        assert_eq!(merged, ["planner", "swarm"]);

        // Merging with an empty catalog is a no-op, same as ask_user_enabled.
        assert_eq!(
            mine.merge(InternalToolCatalog::empty())
                .skipped_platform_tools()
                .collect::<Vec<_>>(),
            ["planner"]
        );
    }

    /// A repeated toggle (the same duplicate-list scenario
    /// `a_duplicated_ask_user_toggle_still_serves_exactly_one_tool` covers
    /// for `ask_user`) must not duplicate the notice line for a skipped name.
    #[test]
    fn a_duplicated_skipped_toggle_produces_one_notice_line_not_two() {
        let catalog =
            InternalToolCatalog::from_names(&["planner".to_owned(), "planner".to_owned()])
                .expect("catalog");
        assert_eq!(
            catalog.skipped_tools_notice_text().as_deref(),
            Some("internal tool 'planner' is not available on this worker")
        );
    }

    /// ELITEA-2783/2778: the two builder modules are INDEPENDENT toggles, and
    /// enabling one changes nothing about `ask_user` or about the other.
    #[test]
    fn the_two_builder_modules_toggle_independently() {
        let neither = InternalToolCatalog::from_names(&[]).expect("empty catalog");
        assert!(!neither.skills_builder_enabled());
        assert!(!neither.project_context_builder_enabled());

        let skills_only = InternalToolCatalog::from_names(&[SKILLS_BUILDER_TOOL_NAME.to_owned()])
            .expect("skills builder");
        assert!(skills_only.skills_builder_enabled());
        assert!(!skills_only.project_context_builder_enabled());
        assert!(!skills_only.ask_user_enabled());

        let context_only =
            InternalToolCatalog::from_names(&[PROJECT_CONTEXT_BUILDER_TOOL_NAME.to_owned()])
                .expect("project context builder");
        assert!(context_only.project_context_builder_enabled());
        assert!(!context_only.skills_builder_enabled());

        // Both, plus ask_user, is the ELITEA-2778 regression shape: nothing
        // that was already on gets turned off by the new names arriving.
        let all = InternalToolCatalog::from_names(&[
            ASK_USER_TOOL_NAME.to_owned(),
            SKILLS_BUILDER_TOOL_NAME.to_owned(),
            PROJECT_CONTEXT_BUILDER_TOOL_NAME.to_owned(),
        ])
        .expect("all three");
        assert!(all.ask_user_enabled());
        assert!(all.skills_builder_enabled());
        assert!(all.project_context_builder_enabled());
        // And neither of them is reported as a SKIPPED platform tool, which
        // would tell the user in-conversation that the tool they just used is
        // not available on this worker.
        assert_eq!(all.skipped_tools_notice_text(), None);
    }

    /// ELITEA-2779: a disabled module binds NO tool, so the model has nothing
    /// to call however explicit the user's request is. Asserted on the SERVED
    /// toolsets rather than on the flags, because a flag that is false while a
    /// tool is still bound is exactly the bug this case describes.
    #[tokio::test(flavor = "current_thread")]
    async fn a_disabled_builder_module_binds_no_tool() {
        let disabled = InternalToolCatalog::from_names(&[ASK_USER_TOOL_NAME.to_owned()])
            .expect("ask_user only");
        let names = served_tool_names(disabled).await;
        assert_eq!(names, [ASK_USER_TOOL_NAME]);
    }

    /// The same catalog with a builder enabled but NO authority to write under
    /// still binds no builder tool — a toggle the deployment cannot serve is
    /// dropped, not bound-and-failing (see `toolsets`' own doc comment).
    #[tokio::test(flavor = "current_thread")]
    async fn a_builder_module_without_authority_binds_no_tool() {
        let enabled = InternalToolCatalog::from_names(&[
            ASK_USER_TOOL_NAME.to_owned(),
            SKILLS_BUILDER_TOOL_NAME.to_owned(),
            PROJECT_CONTEXT_BUILDER_TOOL_NAME.to_owned(),
        ])
        .expect("catalog");
        assert_eq!(served_tool_names(enabled).await, [ASK_USER_TOOL_NAME]);
        // Two enabled builder modules and one bound toolset: the ask_user one.
        assert_eq!(enabled.toolsets(None).len(), 1);
    }

    async fn served_tool_names(catalog: InternalToolCatalog) -> Vec<String> {
        let mut names = Vec::new();
        for toolset in catalog.toolsets(None) {
            let tools = toolset
                .tools(Arc::new(SimpleToolContext::new("internal-tools-test")))
                .await
                .expect("internal toolset tools");
            names.extend(tools.iter().map(|tool| tool.name().to_owned()));
        }
        names
    }

    /// ELITEA-2792: `optional` is carried through normalization, defaults to
    /// false, and survives the encode/decode round trip the durable
    /// clarification decision stores the request through — a flag that is set
    /// on the way in and lost on the way out would leave the CARD (which reads
    /// the decoded request) enforcing the strict rule anyway.
    #[test]
    fn an_optional_question_keeps_its_flag_through_the_durable_round_trip() {
        let request = AskUserRequest::from_arguments(&json!({
            "questions": [
                {"id": "scope", "question": "Scope?", "options": ["Project"]},
                {"id": "notes", "question": "Anything else?", "optional": true}
            ]
        }))
        .expect("request");
        let questions = request.questions_value();
        let rows = questions.as_array().expect("questions array");
        assert_eq!(rows[0]["optional"], json!(false));
        assert_eq!(rows[1]["optional"], json!(true));

        let encoded = encode_ask_user_request(&request).expect("encoded");
        let decoded = decode_ask_user_request(&encoded).expect("decoded");
        assert!(
            decoded == request,
            "the decoded request must equal the encoded one"
        );
    }

    /// A refused document and a refused claim must not read the same to the
    /// model: one says write less, the other says stop. Both are RESULTS, never
    /// turn failures — the conversation that composed the document is the
    /// expensive thing (see `builder_failure_text`).
    #[test]
    fn builder_failures_tell_the_model_what_to_do_next() {
        let rejected = builder_failure_text(
            "skill",
            &RuntimeContextError::Rejected("the builder document cannot be stored as written"),
        );
        assert!(rejected.contains("Shorten it"), "{rejected}");

        let refused = builder_failure_text(
            "project context",
            &RuntimeContextError::AuthorizationFailed("refused"),
        );
        assert!(refused.contains("Do not retry"), "{refused}");

        let unavailable = builder_failure_text(
            "skill",
            &RuntimeContextError::DependencyUnavailable("unreachable"),
        );
        assert!(
            unavailable.contains("could not be reached"),
            "{unavailable}"
        );
        // The three must be distinguishable from each other, not merely
        // non-empty: one shared sentence is how "retry" and "stop" became the
        // same instruction.
        assert_ne!(rejected, refused);
        assert_ne!(refused, unavailable);
    }
}
