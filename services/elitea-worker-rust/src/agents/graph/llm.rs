//! Strict stored-pipeline `llm` node contract and state projection.
//!
//! The Python node combines prompt mapping, an agentic tool loop, provider
//! compatibility repairs and graph-state projection in one object. Rust keeps
//! the same business boundary but gives the actual model/tool execution to a
//! native ADK `LlmAgent`. This module remains authority-free: the complete YAML,
//! selected toolkit aliases and structured result schema are admitted before a
//! PAT, model or toolkit client can exist.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU8, Ordering};

use adk_rust::futures::{StreamExt as _, stream};
use adk_rust::graph::{GraphError, Node, NodeContext, NodeOutput, State};
use adk_rust::{
    AdkError, Agent, Content, FinishReason, InvocationContext, Llm, LlmRequest, LlmResponse,
    LlmResponseStream, Part, ReadonlyContext, RunConfig, SchemaAdapter, Tool,
    ToolConfirmationDecision, ToolContext, Toolset, tool_call_fingerprint,
};
use async_trait::async_trait;
use ring::digest;
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};
use thiserror::Error;
use tracing::Instrument as _;

use super::yaml::{valid_graph_id, valid_output_key};
use super::{PIPELINE_NODE_EVENT_SCOPE_STATE_KEY, PipelineNodeEventScope, PipelineNodeEventSender};
use crate::agents::direct_hitl::blocked_tool_result;
use crate::agents::events::mask_sensitive_arguments;
use crate::agents::internal_tools::{
    ASK_USER_ANSWER_ACTION, ASK_USER_GUARDRAIL_TYPE, ASK_USER_TOOL_NAME, AskUserRequest,
};
use crate::toolkits::{
    DelegatedAuthorizationRequirement, SensitiveToolPolicy, delegated_authorization_declined_result,
};

const MAX_NODE_YAML_BYTES: usize = 64 * 1024;
const MAX_MAPPING_ENTRIES: usize = 16;
const MAX_NODE_VARIABLES: usize = 64;
const MAX_TOOLKITS: usize = 64;
const MAX_TOOLS_PER_TOOLKIT: usize = 256;
const MAX_MAPPING_TEXT_BYTES: usize = 64 * 1024;
const MAX_TOOL_TIMEOUT_SECONDS: u64 = 900;
const DEFAULT_TOOL_TIMEOUT_SECONDS: u64 = 900;
const CONFIG_DIGEST_DOMAIN: &[u8] = b"elitea.graph.llm.config.v1\0";
const MAX_RENDERED_INPUT_BYTES: usize = 64 * 1024;
const MAX_LLM_REPLAY_BYTES: usize = 512 * 1024;
const MAX_LLM_REPLAY_CONTENTS: usize = 1_024;
const MAX_LLM_REPLAY_DECISIONS: usize = 16;
const MAX_CONFIRMATION_IDENTITY_BYTES: usize = 512;
const MAX_CONFIRMATION_MESSAGE_BYTES: usize = 16 * 1024;
const LLM_TOOL_CONFIRMATION_SCHEMA: &str = "elitea.graph.tool-confirmation.v1";
const LLM_TOOL_REPLAY_SCHEMA: &str = "elitea.graph.llm-tool-replay.v1";
const LLM_INPUT_DIGEST_DOMAIN: &[u8] = b"elitea.graph.llm.input.v1\0";
const LLM_REPLAY_STATE_DIGEST_DOMAIN: &[u8] = b"elitea.graph.llm.replay-state.v1\0";
pub(crate) const LLM_TOOL_RESUME_STATE_KEY: &str = "__elitea_llm_tool_resume_v1";
static LLM_INVOCATION_SEQUENCE: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(1);

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawLlmNodeDefinition {
    id: String,
    #[serde(rename = "type")]
    node_type: String,
    #[serde(default)]
    input_mapping: BTreeMap<String, RawInputMapping>,
    #[serde(default)]
    input: Vec<String>,
    #[serde(default)]
    output: Vec<String>,
    #[serde(default)]
    tool_names: BTreeMap<String, Vec<String>>,
    #[serde(default)]
    structured_output: bool,
    #[serde(default = "default_tool_timeout")]
    tool_execution_timeout: u64,
    #[serde(default)]
    transition: Option<String>,
}

const fn default_tool_timeout() -> u64 {
    DEFAULT_TOOL_TIMEOUT_SECONDS
}

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawInputMapping {
    #[serde(rename = "type")]
    kind: String,
    value: Value,
}

/// One UI input-mapping expression evaluated against the current graph state.
#[derive(Clone)]
pub(super) enum LlmInputMapping {
    Fixed(Value),
    Variable(String),
    Template(String),
}

/// Exact toolkit alias and concrete tool names selected on one LLM node.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct LlmToolkitSelection {
    alias: String,
    tools: Vec<String>,
}

impl LlmToolkitSelection {
    pub(crate) fn alias(&self) -> &str {
        &self.alias
    }

    pub(crate) fn tools(&self) -> &[String] {
        &self.tools
    }
}

/// Authority-free definition of one native ADK `LlmAgent` graph node.
#[derive(Clone)]
pub(crate) struct LlmNodeDefinition {
    id: String,
    input_mapping: BTreeMap<String, LlmInputMapping>,
    input: Vec<String>,
    output: Vec<String>,
    tool_selections: Vec<LlmToolkitSelection>,
    structured_output: bool,
    tool_execution_timeout: u64,
    transition: Option<String>,
}

impl LlmNodeDefinition {
    pub(super) fn from_yaml(yaml: &str) -> Result<Self, LlmConfigurationError> {
        if yaml.is_empty() || yaml.len() > MAX_NODE_YAML_BYTES {
            return Err(LlmConfigurationError::ResourceExhausted);
        }
        let raw = serde_yaml_ng::from_str::<RawLlmNodeDefinition>(yaml)
            .map_err(|source| LlmConfigurationError::MalformedYaml { source })?;
        Self::from_raw(raw)
    }

    fn from_raw(mut raw: RawLlmNodeDefinition) -> Result<Self, LlmConfigurationError> {
        if raw.node_type != "llm" {
            return Err(LlmConfigurationError::Invalid("the node type must be llm"));
        }
        if !valid_graph_id(&raw.id) {
            return Err(LlmConfigurationError::Invalid(
                "the LLM node ID is malformed",
            ));
        }
        if raw.input_mapping.len() > MAX_MAPPING_ENTRIES
            || raw.input.len() > MAX_NODE_VARIABLES
            || raw.output.len() > MAX_NODE_VARIABLES
            || raw.tool_names.len() > MAX_TOOLKITS
        {
            return Err(LlmConfigurationError::ResourceExhausted);
        }
        if raw.input.is_empty() {
            raw.input.push("messages".to_owned());
        }
        for key in raw.input.iter().chain(&raw.output) {
            if !valid_output_key(key) {
                return Err(LlmConfigurationError::Invalid(
                    "an LLM node state variable is malformed",
                ));
            }
        }
        validate_unique(&raw.input)?;
        validate_unique(&raw.output)?;
        if raw
            .output
            .iter()
            .filter(|key| key.as_str() != "messages")
            .count()
            > if raw.structured_output {
                MAX_NODE_VARIABLES
            } else {
                1
            }
        {
            return Err(LlmConfigurationError::Invalid(
                "a non-structured LLM node may write only one data output",
            ));
        }
        if raw.structured_output && raw.output.iter().all(|key| key == "messages") {
            return Err(LlmConfigurationError::Invalid(
                "a structured LLM node must declare a data output",
            ));
        }
        if raw.tool_execution_timeout == 0 || raw.tool_execution_timeout > MAX_TOOL_TIMEOUT_SECONDS
        {
            return Err(LlmConfigurationError::Invalid(
                "the LLM tool timeout is outside its approved range",
            ));
        }
        if raw
            .transition
            .as_deref()
            .is_some_and(|target| target != "END" && !valid_graph_id(target))
        {
            return Err(LlmConfigurationError::Invalid(
                "the LLM node transition is malformed",
            ));
        }

        let input_mapping = validate_input_mapping(raw.input_mapping)?;
        let tool_selections = validate_tool_selections(raw.tool_names)?;
        Ok(Self {
            id: raw.id,
            input_mapping,
            input: raw.input,
            output: raw.output,
            tool_selections,
            structured_output: raw.structured_output,
            tool_execution_timeout: raw.tool_execution_timeout,
            transition: raw.transition,
        })
    }

    pub(crate) fn id(&self) -> &str {
        &self.id
    }

    pub(super) fn input_keys(&self) -> &[String] {
        &self.input
    }

    pub(super) fn output_keys(&self) -> &[String] {
        &self.output
    }

    pub(super) fn transition(&self) -> Option<&str> {
        self.transition.as_deref()
    }

    pub(super) fn input_mapping(&self) -> &BTreeMap<String, LlmInputMapping> {
        &self.input_mapping
    }

    pub(crate) fn tool_selections(&self) -> &[LlmToolkitSelection] {
        &self.tool_selections
    }

    pub(super) const fn structured_output(&self) -> bool {
        self.structured_output
    }

    pub(crate) const fn tool_execution_timeout(&self) -> u64 {
        self.tool_execution_timeout
    }

    /// Minimal no-tool definition used by the model-backed Decision node.
    ///
    /// The invocation still passes through the same claim-bound model factory;
    /// only the node-specific prompt and route projection live elsewhere.
    pub(super) fn for_decision(id: &str) -> Self {
        Self {
            id: id.to_owned(),
            input_mapping: BTreeMap::new(),
            input: Vec::new(),
            output: Vec::new(),
            tool_selections: Vec::new(),
            structured_output: false,
            tool_execution_timeout: DEFAULT_TOOL_TIMEOUT_SECONDS,
            transition: None,
        }
    }

    /// Build the exact JSON Schema ADK validates before graph-state projection.
    pub(crate) fn output_schema(
        &self,
        state_types: &BTreeMap<String, String>,
    ) -> Result<Option<Value>, LlmConfigurationError> {
        if !self.structured_output {
            return Ok(None);
        }
        let mut properties = Map::new();
        let mut required = Vec::new();
        for key in self.output.iter().filter(|key| key.as_str() != "messages") {
            let kind = state_types.get(key).ok_or(LlmConfigurationError::Invalid(
                "an LLM output is not declared in pipeline state",
            ))?;
            properties.insert(key.clone(), state_type_schema(kind)?);
            required.push(Value::String(key.clone()));
        }
        Ok(Some(json!({
            "type": "object",
            "properties": properties,
            "required": required,
            "additionalProperties": false
        })))
    }

    /// Project the final ADK response into the Python-compatible graph shape.
    ///
    /// Messages are always retained. Structured JSON fields are matched to
    /// their declared state keys; a non-structured response is copied to the
    /// first non-message output, matching the active SDK behavior.
    pub(super) fn project_response(
        &self,
        text: &str,
        state_types: &BTreeMap<String, String>,
    ) -> Result<BTreeMap<String, Value>, LlmExecutionError> {
        if text.len() > MAX_NODE_YAML_BYTES {
            return Err(LlmExecutionError::ResourceExhausted);
        }
        let mut updates = BTreeMap::new();
        updates.insert(
            "messages".to_owned(),
            json!([{"role": "assistant", "content": text}]),
        );
        let outputs = self.output.iter().filter(|key| key.as_str() != "messages");
        if self.structured_output {
            let object = serde_json::from_str::<Value>(text)
                .ok()
                .and_then(|value| value.as_object().cloned())
                .ok_or(LlmExecutionError::InvalidStructuredOutput)?;
            let expected = self
                .output
                .iter()
                .filter(|key| key.as_str() != "messages")
                .collect::<BTreeSet<_>>();
            if object.len() != expected.len() || object.keys().any(|key| !expected.contains(key)) {
                return Err(LlmExecutionError::InvalidStructuredOutput);
            }
            for key in outputs {
                let value = object
                    .get(key)
                    .cloned()
                    .ok_or(LlmExecutionError::InvalidStructuredOutput)?;
                let kind = state_types
                    .get(key)
                    .ok_or(LlmExecutionError::InvalidStructuredOutput)?;
                if !state_value_matches(kind, &value) {
                    return Err(LlmExecutionError::InvalidStructuredOutput);
                }
                updates.insert(key.clone(), value);
            }
        } else if let Some(key) = outputs.into_iter().next() {
            updates.insert(key.clone(), Value::String(text.to_owned()));
        }
        Ok(updates)
    }

    fn map_execution_input(&self, state: &State) -> Result<LlmExecutionInput, LlmExecutionError> {
        let mut mapped = BTreeMap::new();
        for (key, mapping) in &self.input_mapping {
            let value = match mapping {
                LlmInputMapping::Fixed(value) => value.clone(),
                LlmInputMapping::Variable(source) => state
                    .get(source)
                    .cloned()
                    .unwrap_or(Value::String(String::new())),
                LlmInputMapping::Template(template) => {
                    Value::String(render_fstring(template, state)?)
                }
            };
            mapped.insert(key.as_str(), value);
        }

        if let Some(messages) = mapped.get("messages") {
            let mut contents = parse_messages(messages)?;
            let task = contents
                .pop()
                .filter(|content| content.role == "user")
                .ok_or(LlmExecutionError::InvalidInputMapping)?;
            return Ok(LlmExecutionInput {
                system: String::new(),
                task,
                history: contents,
            });
        }

        let system = mapped
            .get("system")
            .map(value_as_bounded_text)
            .transpose()?
            .unwrap_or_default();
        let task_text = mapped
            .get("task")
            .map(value_as_bounded_text)
            .transpose()?
            .unwrap_or_default();
        let history = mapped
            .get("chat_history")
            .map(parse_messages)
            .transpose()?
            .unwrap_or_default();
        if task_text.is_empty() && history.last().is_none_or(|content| content.role != "tool") {
            return Err(LlmExecutionError::InvalidInputMapping);
        }
        Ok(LlmExecutionInput {
            system,
            task: Content::new("user").with_text(task_text),
            history,
        })
    }

    pub(super) fn config_digest(&self) -> [u8; 32] {
        let mut context = digest::Context::new(&digest::SHA256);
        context.update(CONFIG_DIGEST_DOMAIN);
        digest_field(&mut context, self.id.as_bytes());
        for (key, mapping) in &self.input_mapping {
            digest_field(&mut context, key.as_bytes());
            match mapping {
                LlmInputMapping::Fixed(value) => {
                    digest_field(&mut context, b"fixed");
                    digest_field(&mut context, &serde_json::to_vec(value).unwrap_or_default());
                }
                LlmInputMapping::Variable(value) => {
                    digest_field(&mut context, b"variable");
                    digest_field(&mut context, value.as_bytes());
                }
                LlmInputMapping::Template(value) => {
                    digest_field(&mut context, b"fstring");
                    digest_field(&mut context, value.as_bytes());
                }
            }
        }
        for key in &self.input {
            digest_field(&mut context, key.as_bytes());
        }
        for key in &self.output {
            digest_field(&mut context, key.as_bytes());
        }
        for selection in &self.tool_selections {
            digest_field(&mut context, selection.alias.as_bytes());
            for tool in &selection.tools {
                digest_field(&mut context, tool.as_bytes());
            }
        }
        digest_field(&mut context, &[u8::from(self.structured_output)]);
        digest_field(&mut context, &self.tool_execution_timeout.to_be_bytes());
        digest_field(
            &mut context,
            self.transition.as_deref().unwrap_or_default().as_bytes(),
        );
        copy_digest(context.finish().as_ref())
    }

    fn digest_label(&self) -> String {
        format!("sha256:{}", hex(&self.config_digest()))
    }
}

/// Prompt material passed to an invocation-owned ADK `LlmAgent`.
pub(crate) struct LlmExecutionInput {
    system: String,
    task: Content,
    history: Vec<Content>,
}

impl LlmExecutionInput {
    pub(crate) fn system(&self) -> &str {
        &self.system
    }

    fn digest_label(&self) -> Result<String, LlmExecutionError> {
        let encoded = serde_json::to_vec(&(&self.system, &self.task, &self.history))
            .map_err(|_| LlmExecutionError::InvalidInputMapping)?;
        if encoded.len() > MAX_LLM_REPLAY_BYTES {
            return Err(LlmExecutionError::ResourceExhausted);
        }
        let mut context = digest::Context::new(&digest::SHA256);
        context.update(LLM_INPUT_DIGEST_DOMAIN);
        digest_field(&mut context, &encoded);
        Ok(format!("sha256:{}", hex(context.finish().as_ref())))
    }

    fn initial_transcript(&self) -> Vec<Content> {
        let mut transcript = self.history.clone();
        transcript.push(self.task.clone());
        transcript
    }

    pub(super) fn for_decision(history: Vec<Content>, prompt: String) -> Self {
        Self {
            system: String::new(),
            task: Content::new("user").with_text(prompt),
            history,
        }
    }
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct PipelineLlmReplayDecision {
    tool_name: String,
    arguments: Value,
    fingerprint: String,
    /// Authorization approval rematerializes the real tool and replays the
    /// call without pre-approving a separate sensitive-tool confirmation.
    #[serde(default)]
    defer_confirmation: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    blocked_result: Option<Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    authorization: Option<DelegatedAuthorizationRequirement>,
}

/// Private, checkpoint-bound continuation state for one native LLM-node tool turn.
///
/// The browser receives only the masked confirmation card. Raw arguments and the
/// exact ADK transcript remain inside the `PostgreSQL` graph/session boundary so a
/// continuation can replay the provider's original function-call IDs without a
/// second planning call.
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct PipelineLlmReplayEnvelope {
    schema_revision: String,
    node_name: String,
    definition_digest: String,
    input_digest: String,
    predecessor_digest: String,
    history_before_pending: Vec<Content>,
    pending_content: Content,
    decisions: BTreeMap<String, PipelineLlmReplayDecision>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    declined_authorizations: Vec<DelegatedAuthorizationRequirement>,
}

impl PipelineLlmReplayEnvelope {
    fn new(
        definition: &LlmNodeDefinition,
        input_digest: String,
        predecessor: Option<&Value>,
        history_before_pending: Vec<Content>,
        pending_content: Content,
        prior: Option<&Self>,
    ) -> Result<Self, LlmExecutionError> {
        let decisions = prior
            .filter(|prior| same_content(&prior.pending_content, &pending_content))
            .map_or_else(BTreeMap::new, |prior| prior.decisions.clone());
        let envelope = Self {
            schema_revision: LLM_TOOL_REPLAY_SCHEMA.to_owned(),
            node_name: definition.id().to_owned(),
            definition_digest: definition.digest_label(),
            input_digest,
            predecessor_digest: replay_state_digest(predecessor)?,
            history_before_pending,
            pending_content,
            decisions,
            declined_authorizations: prior
                .map_or_else(Vec::new, |prior| prior.declined_authorizations.clone()),
        };
        envelope.validate()?;
        Ok(envelope)
    }

    pub(crate) fn validate(&self) -> Result<(), LlmExecutionError> {
        if self.schema_revision != LLM_TOOL_REPLAY_SCHEMA
            || !valid_replay_identity(&self.node_name)
            || !valid_sha256_label(&self.definition_digest)
            || !valid_sha256_label(&self.input_digest)
            || !valid_sha256_label(&self.predecessor_digest)
            || self.history_before_pending.len() > MAX_LLM_REPLAY_CONTENTS
            || self.decisions.len() > MAX_LLM_REPLAY_DECISIONS
            || self.declined_authorizations.len() > MAX_LLM_REPLAY_DECISIONS
            || self.pending_content.role != "model"
        {
            return Err(LlmExecutionError::InvalidInputMapping);
        }
        let calls = replay_calls(&self.pending_content)?;
        if calls.is_empty() || calls.len() > MAX_LLM_REPLAY_DECISIONS {
            return Err(LlmExecutionError::InvalidInputMapping);
        }
        let mut identities = BTreeSet::new();
        if calls.iter().any(|call| {
            !valid_replay_identity(call.call_id)
                || !valid_replay_identity(call.tool_name)
                || !identities.insert(call.call_id)
        }) {
            return Err(LlmExecutionError::InvalidInputMapping);
        }
        for (call_id, decision) in &self.decisions {
            let Some(call) = calls.iter().find(|call| call.call_id == call_id) else {
                return Err(LlmExecutionError::InvalidInputMapping);
            };
            if !valid_replay_identity(call_id)
                || !valid_replay_identity(&decision.tool_name)
                || decision.tool_name != call.tool_name
                || &decision.arguments != call.arguments
                || decision.fingerprint
                    != tool_call_fingerprint(&decision.tool_name, &decision.arguments)
            {
                return Err(LlmExecutionError::InvalidInputMapping);
            }
            if decision.defer_confirmation && decision.blocked_result.is_some() {
                return Err(LlmExecutionError::InvalidInputMapping);
            }
            if let Some(result_type) = decision
                .blocked_result
                .as_ref()
                .and_then(|result| result.get("type"))
                .and_then(Value::as_str)
                && !matches!(result_type, "sensitive_tool_blocked" | "mcp_auth_decision")
            {
                return Err(LlmExecutionError::InvalidInputMapping);
            }
        }
        let encoded =
            serde_json::to_vec(self).map_err(|_| LlmExecutionError::InvalidInputMapping)?;
        if encoded.len() > MAX_LLM_REPLAY_BYTES {
            return Err(LlmExecutionError::ResourceExhausted);
        }
        Ok(())
    }

    pub(crate) fn matches_checkpoint_predecessor(
        &self,
        predecessor: Option<&Value>,
    ) -> Result<bool, LlmExecutionError> {
        replay_state_digest(predecessor).map(|digest| digest == self.predecessor_digest)
    }

    pub(crate) fn matches_pending_call(
        &self,
        call_id: &str,
        tool_name: &str,
        arguments: &Value,
    ) -> Result<bool, LlmExecutionError> {
        self.validate()?;
        Ok(replay_calls(&self.pending_content)?
            .into_iter()
            .any(|call| {
                call.call_id == call_id
                    && call.tool_name == tool_name
                    && call.arguments == arguments
            }))
    }

    pub(crate) fn resolve_decision(
        mut self,
        call_id: &str,
        tool_name: &str,
        approve: bool,
        denial_comment: Option<&str>,
        policy: (&str, &str, &str),
    ) -> Result<Value, LlmExecutionError> {
        self.validate()?;
        let call = replay_calls(&self.pending_content)?
            .into_iter()
            .find(|call| call.call_id == call_id && call.tool_name == tool_name)
            .ok_or(LlmExecutionError::InvalidInputMapping)?;
        if self.decisions.get(call_id).is_some_and(|decision| {
            !decision.defer_confirmation || decision.blocked_result.is_some()
        }) {
            return Err(LlmExecutionError::InvalidInputMapping);
        }
        let blocked_result = (!approve).then(|| {
            let (toolkit_name, toolkit_type, action_label) = policy;
            blocked_tool_result(
                tool_name,
                toolkit_name,
                toolkit_type,
                action_label,
                denial_comment,
            )
        });
        self.decisions.insert(
            call_id.to_owned(),
            PipelineLlmReplayDecision {
                tool_name: tool_name.to_owned(),
                arguments: call.arguments.clone(),
                fingerprint: tool_call_fingerprint(tool_name, call.arguments),
                defer_confirmation: false,
                blocked_result,
                authorization: None,
            },
        );
        self.validate()?;
        serde_json::to_value(self).map_err(|_| LlmExecutionError::InvalidInputMapping)
    }

    pub(crate) fn resolve_authorization_decision(
        mut self,
        call_id: &str,
        tool_name: &str,
        authorize: bool,
        requirement: &DelegatedAuthorizationRequirement,
    ) -> Result<Value, LlmExecutionError> {
        self.validate()?;
        let call = replay_calls(&self.pending_content)?
            .into_iter()
            .find(|call| call.call_id == call_id && call.tool_name == tool_name)
            .ok_or(LlmExecutionError::InvalidInputMapping)?;
        if self.decisions.contains_key(call_id) {
            return Err(LlmExecutionError::InvalidInputMapping);
        }
        self.decisions.insert(
            call_id.to_owned(),
            PipelineLlmReplayDecision {
                tool_name: tool_name.to_owned(),
                arguments: call.arguments.clone(),
                fingerprint: tool_call_fingerprint(tool_name, call.arguments),
                defer_confirmation: authorize,
                blocked_result: (!authorize)
                    .then(|| delegated_authorization_declined_result(requirement, tool_name)),
                authorization: Some(requirement.clone()),
            },
        );
        if !authorize
            && !self
                .declined_authorizations
                .iter()
                .any(|other| other.same_authority(requirement))
        {
            self.declined_authorizations.push(requirement.clone());
        }
        self.validate()?;
        serde_json::to_value(self).map_err(|_| LlmExecutionError::InvalidInputMapping)
    }

    pub(crate) fn resolve_clarifying_answer(
        mut self,
        call_id: &str,
        tool_name: &str,
        answer: &str,
    ) -> Result<Value, LlmExecutionError> {
        self.validate()?;
        let call = replay_calls(&self.pending_content)?
            .into_iter()
            .find(|call| call.call_id == call_id && call.tool_name == tool_name)
            .ok_or(LlmExecutionError::InvalidInputMapping)?;
        if self.decisions.contains_key(call_id) || tool_name != ASK_USER_TOOL_NAME {
            return Err(LlmExecutionError::InvalidInputMapping);
        }
        let request = AskUserRequest::from_arguments(call.arguments)
            .map_err(|_| LlmExecutionError::InvalidInputMapping)?;
        let result = request
            .format_answer(answer)
            .map_err(|_| LlmExecutionError::InvalidInputMapping)?;
        self.decisions.insert(
            call_id.to_owned(),
            PipelineLlmReplayDecision {
                tool_name: tool_name.to_owned(),
                arguments: call.arguments.clone(),
                fingerprint: tool_call_fingerprint(tool_name, call.arguments),
                defer_confirmation: false,
                blocked_result: Some(result),
                authorization: None,
            },
        );
        self.validate()?;
        serde_json::to_value(self).map_err(|_| LlmExecutionError::InvalidInputMapping)
    }

    fn apply_run_config(&self, run_config: &mut RunConfig) {
        for (call_id, decision) in &self.decisions {
            if decision.defer_confirmation && !decision.is_authorization_proxy() {
                continue;
            }
            run_config
                .tool_confirmation_decisions
                .insert(call_id.clone(), ToolConfirmationDecision::Approve);
            run_config
                .tool_confirmation_fingerprints
                .insert(call_id.clone(), decision.fingerprint.clone());
        }
    }

    fn blocked_replays(&self) -> Vec<PipelineBlockedToolReplay> {
        self.decisions
            .iter()
            .filter_map(|(call_id, decision)| {
                let response = decision.blocked_result.clone().or_else(|| {
                    decision
                        .authorization
                        .as_ref()
                        .filter(|_| decision.is_authorization_proxy())
                        .map(|requirement| {
                            crate::toolkits::delegated_authorization_granted_result(
                                requirement,
                                &decision.tool_name,
                            )
                        })
                });
                response.map(|response| PipelineBlockedToolReplay {
                    call_id: call_id.clone(),
                    tool_name: decision.tool_name.clone(),
                    arguments: decision.arguments.clone(),
                    confirmation_decision: if response.is_string()
                        || response["status"] == "authorized"
                    {
                        ToolConfirmationDecision::Approve
                    } else {
                        ToolConfirmationDecision::Deny
                    },
                    response,
                })
            })
            .collect()
    }

    fn replay_history(&self) -> Vec<Content> {
        self.history_before_pending.clone()
    }

    fn pending_content(&self) -> Content {
        self.pending_content.clone()
    }

    fn decisions(&self) -> &BTreeMap<String, PipelineLlmReplayDecision> {
        &self.decisions
    }

    pub(crate) fn has_deferred_authorization_for(&self, tool_name: &str) -> bool {
        self.decisions.values().any(|decision| {
            decision.tool_name == tool_name
                && decision.defer_confirmation
                && decision.blocked_result.is_none()
        })
    }

    pub(crate) fn apply_authorization_scope(
        &self,
        catalog: &mut crate::toolkits::DelegatedAuthorizationCatalog,
    ) -> Result<(), LlmExecutionError> {
        for decision in self.decisions.values() {
            if decision.defer_confirmation && catalog.requirement_for(&decision.tool_name).is_some()
            {
                return Err(LlmExecutionError::Unavailable);
            }
        }
        for requirement in &self.declined_authorizations {
            catalog.decline(requirement);
        }
        // Old checkpoints contain a per-call result, without a toolkit scope.
        // Match that result to the current frozen authority before widening Skip.
        for decision in self.decisions.values() {
            if let Some(requirement) = catalog.requirement_for(&decision.tool_name).cloned()
                && decision.blocked_result.as_ref()
                    == Some(&delegated_authorization_declined_result(
                        &requirement,
                        &decision.tool_name,
                    ))
            {
                catalog.decline(&requirement);
            }
        }
        Ok(())
    }

    pub(crate) fn definition_digest(&self) -> &str {
        &self.definition_digest
    }

    fn input_digest(&self) -> &str {
        &self.input_digest
    }
}

impl PipelineLlmReplayDecision {
    fn is_authorization_proxy(&self) -> bool {
        self.authorization
            .as_ref()
            .is_some_and(|requirement| self.tool_name == requirement.authorization_tool_name())
    }
}

struct ReplayCall<'a> {
    call_id: &'a str,
    tool_name: &'a str,
    arguments: &'a Value,
}

fn replay_calls(content: &Content) -> Result<Vec<ReplayCall<'_>>, LlmExecutionError> {
    content
        .parts
        .iter()
        .filter_map(|part| match part {
            Part::FunctionCall { name, args, id, .. } => Some(
                id.as_deref()
                    .map(|call_id| ReplayCall {
                        call_id,
                        tool_name: name,
                        arguments: args,
                    })
                    .ok_or(LlmExecutionError::InvalidInputMapping),
            ),
            _ => None,
        })
        .collect()
}

fn same_content(left: &Content, right: &Content) -> bool {
    serde_json::to_vec(left).ok() == serde_json::to_vec(right).ok()
}

fn replay_state_digest(value: Option<&Value>) -> Result<String, LlmExecutionError> {
    let encoded = serde_json::to_vec(value.unwrap_or(&Value::Null))
        .map_err(|_| LlmExecutionError::InvalidInputMapping)?;
    if encoded.len() > MAX_LLM_REPLAY_BYTES {
        return Err(LlmExecutionError::ResourceExhausted);
    }
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(LLM_REPLAY_STATE_DIGEST_DOMAIN);
    digest_field(&mut context, &encoded);
    Ok(format!("sha256:{}", hex(context.finish().as_ref())))
}

fn valid_replay_identity(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_CONFIRMATION_IDENTITY_BYTES
        && !value.chars().any(char::is_control)
}

fn valid_sha256_label(value: &str) -> bool {
    value.len() == 71
        && value.starts_with("sha256:")
        && value[7..].bytes().all(|byte| byte.is_ascii_hexdigit())
}

/// Invocation-owned factory that binds a fresh model and exact node toolsets.
pub(crate) trait PipelineLlmAgentFactory: Send + Sync {
    fn build(
        &self,
        definition: &LlmNodeDefinition,
        input: &LlmExecutionInput,
        output_schema: Option<Value>,
        replay: Option<&PipelineLlmReplayEnvelope>,
    ) -> Result<PipelineLlmAgentBinding, LlmExecutionError>;

    fn event_sender(&self) -> Option<PipelineNodeEventSender> {
        None
    }
}

#[derive(Clone)]
pub(crate) enum PipelineToolGuard {
    Sensitive(SensitiveToolPolicy),
    DelegatedAuthorization(DelegatedAuthorizationRequirement),
    AskUser,
}

/// One node-local agent plus guards keyed by its provider-visible tool names.
pub(crate) struct PipelineLlmAgentBinding {
    agent: Arc<dyn Agent>,
    guards: BTreeMap<String, PipelineToolGuard>,
}

impl PipelineLlmAgentBinding {
    #[must_use]
    pub(crate) fn new(agent: Arc<dyn Agent>, guards: BTreeMap<String, PipelineToolGuard>) -> Self {
        Self { agent, guards }
    }

    fn agent(&self) -> Arc<dyn Agent> {
        Arc::clone(&self.agent)
    }

    fn guard(&self, tool_name: &str) -> Option<PipelineToolGuard> {
        self.guards.get(tool_name).cloned()
    }
}

/// Native ADK agent node with Elitea YAML input/output projection.
pub(super) struct LlmNode {
    definition: LlmNodeDefinition,
    state_types: BTreeMap<String, String>,
    factory: Arc<dyn PipelineLlmAgentFactory>,
}

impl LlmNode {
    pub(super) fn new(
        definition: LlmNodeDefinition,
        state_types: BTreeMap<String, String>,
        factory: Arc<dyn PipelineLlmAgentFactory>,
    ) -> Self {
        Self {
            definition,
            state_types,
            factory,
        }
    }
}

#[async_trait]
impl Node for LlmNode {
    fn name(&self) -> &str {
        self.definition.id()
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let selected_toolkit_count = self.definition.tool_selections().len();
        let selected_tool_count = self
            .definition
            .tool_selections()
            .iter()
            .map(|selection| selection.tools().len())
            .sum::<usize>();
        let span = tracing::info_span!(
            "agent.pipeline.llm_node",
            node_id = self.name(),
            selected_toolkit_count,
            selected_tool_count,
            structured_output = self.definition.structured_output(),
            stage = tracing::field::Empty,
            outcome = tracing::field::Empty,
            error_code = tracing::field::Empty,
        );
        let result = async {
            tracing::Span::current().record("stage", "input_mapping");
            let input = self
                .definition
                .map_execution_input(&context.state)
                .map_err(|_| node_failure(self.name()))?;
            let input_digest = input
                .digest_label()
                .map_err(|_| node_failure(self.name()))?;
            let (replay, remaining_replays) = self
                .replay_state(&context.state, &input_digest)
                .map_err(|_| node_failure(self.name()))?;
            tracing::Span::current().record("stage", "agent_binding");
            let schema = self
                .definition
                .output_schema(&self.state_types)
                .map_err(|_| node_failure(self.name()))?;
            match run_model_agent(
                &self.definition,
                input,
                &input_digest,
                schema,
                &self.factory,
                replay.as_ref(),
                context,
            )
            .await
            .map_err(|_| node_failure(self.name()))?
            {
                PipelineLlmRunOutcome::Completed(text) => {
                    tracing::Span::current().record("stage", "state_projection");
                    let updates = self
                        .definition
                        .project_response(&text, &self.state_types)
                        .map_err(|_| node_failure(self.name()))?;
                    let mut output = NodeOutput::new();
                    if replay.is_some() {
                        output = output.with_update(LLM_TOOL_RESUME_STATE_KEY, remaining_replays);
                    }
                    for (key, value) in updates {
                        output = output.with_update(&key, value);
                    }
                    Ok(output)
                }
                PipelineLlmRunOutcome::Confirmation(confirmation) => {
                    tracing::Span::current().record("stage", "tool_confirmation");
                    self.confirmation_interrupt(*confirmation)
                        .map_err(|_| node_failure(self.name()))
                }
            }
        }
        .instrument(span.clone())
        .await;
        if result.is_ok() {
            span.record("outcome", "completed");
        } else {
            span.record("outcome", "failed");
            span.record("error_code", "pipeline.llm_node.failed");
        }
        result
    }
}

impl LlmNode {
    fn replay_state(
        &self,
        state: &State,
        input_digest: &str,
    ) -> Result<(Option<PipelineLlmReplayEnvelope>, Value), LlmExecutionError> {
        let Some(raw_replays) = state.get(LLM_TOOL_RESUME_STATE_KEY) else {
            return Ok((None, json!({})));
        };
        let replays = raw_replays
            .as_object()
            .ok_or(LlmExecutionError::InvalidInputMapping)?;
        let Some(raw) = replays.get(self.name()) else {
            return Ok((None, raw_replays.clone()));
        };
        let replay = serde_json::from_value::<PipelineLlmReplayEnvelope>(raw.clone())
            .map_err(|_| LlmExecutionError::InvalidInputMapping)?;
        replay.validate()?;
        if replay.node_name != self.name()
            || replay.definition_digest() != self.definition.digest_label()
            || replay.input_digest() != input_digest
            || replay.decisions().is_empty()
        {
            return Err(LlmExecutionError::InvalidInputMapping);
        }
        let mut remaining = replays.clone();
        remaining.remove(self.name());
        Ok((Some(replay), Value::Object(remaining)))
    }

    fn confirmation_interrupt(
        &self,
        confirmation: PipelineLlmConfirmation,
    ) -> Result<NodeOutput, LlmExecutionError> {
        let call_id = confirmation
            .request
            .function_call_id
            .as_deref()
            .filter(|value| valid_replay_identity(value))
            .ok_or(LlmExecutionError::InvalidInputMapping)?
            .to_owned();
        let policy = match confirmation.guard.clone() {
            PipelineToolGuard::DelegatedAuthorization(requirement) => {
                return self.authorization_interrupt(&call_id, confirmation, &requirement);
            }
            PipelineToolGuard::AskUser => {
                return self.clarifying_question_interrupt(&call_id, confirmation);
            }
            PipelineToolGuard::Sensitive(policy) => policy,
        };
        if policy.policy_message().is_empty()
            || policy.policy_message().len() > MAX_CONFIRMATION_MESSAGE_BYTES
            || policy.policy_message().chars().any(|character| {
                character == '\0'
                    || (character.is_control() && !matches!(character, '\n' | '\r' | '\t'))
            })
        {
            return Err(LlmExecutionError::ResourceExhausted);
        }
        let masked = mask_sensitive_arguments(&confirmation.request.args, 0)
            .map_err(|_| LlmExecutionError::ResourceExhausted)?;
        let argument_digest = confirmation_argument_digest(
            &call_id,
            &confirmation.request.tool_name,
            &confirmation.request.args,
        )?;
        let replay = serde_json::to_value(confirmation.replay)
            .map_err(|_| LlmExecutionError::InvalidInputMapping)?;
        let data = json!({
            "schema_revision": LLM_TOOL_CONFIRMATION_SCHEMA,
            "type": "hitl",
            "guardrail_type": "sensitive_tool",
            "node_name": self.name(),
            "message": policy.policy_message(),
            "available_actions": ["approve", "reject", "block_with_comment"],
            "routes": {},
            "definition_digest": self.definition.digest_label(),
            "tool_call_id": call_id,
            "tool_name": confirmation.request.tool_name,
            "toolkit_name": policy.toolkit_name(),
            "toolkit_type": policy.toolkit_type(),
            "action_label": policy.action_name(),
            "tool_args": masked,
            "argument_digest": argument_digest,
            "policy_message": policy.policy_message(),
            "llm_replay": replay,
        });
        Ok(NodeOutput::interrupt_with_data(
            policy.policy_message(),
            data,
        ))
    }

    fn authorization_interrupt(
        &self,
        call_id: &str,
        confirmation: PipelineLlmConfirmation,
        requirement: &DelegatedAuthorizationRequirement,
    ) -> Result<NodeOutput, LlmExecutionError> {
        let message = requirement.user_message();
        if message.is_empty() || message.len() > MAX_CONFIRMATION_MESSAGE_BYTES {
            return Err(LlmExecutionError::ResourceExhausted);
        }
        let argument_digest = confirmation_argument_digest(
            call_id,
            &confirmation.request.tool_name,
            &confirmation.request.args,
        )?;
        let replay = serde_json::to_value(confirmation.replay)
            .map_err(|_| LlmExecutionError::InvalidInputMapping)?;
        let data = json!({
            "schema_revision": "elitea.graph.mcp-authorization.v1",
            "type": "hitl",
            "guardrail_type": "mcp_auth",
            "node_name": self.name(),
            "message": message,
            "available_actions": ["authorize", "skip"],
            "routes": {},
            "definition_digest": self.definition.digest_label(),
            "tool_call_id": call_id,
            "tool_name": confirmation.request.tool_name,
            "toolkit_name": requirement.toolkit_name(),
            "toolkit_type": requirement.toolkit_type(),
            "tool_args": {},
            "argument_digest": argument_digest,
            "server_url": requirement.server_url(),
            "resource_metadata_url": requirement.resource_metadata_url(),
            "www_authenticate": requirement.www_authenticate(),
            "resource_metadata": requirement.resource_metadata(),
            "llm_replay": replay,
        });
        Ok(NodeOutput::interrupt_with_data(&message, data))
    }

    fn clarifying_question_interrupt(
        &self,
        call_id: &str,
        confirmation: PipelineLlmConfirmation,
    ) -> Result<NodeOutput, LlmExecutionError> {
        if confirmation.request.tool_name != ASK_USER_TOOL_NAME {
            return Err(LlmExecutionError::InvalidInputMapping);
        }
        let request = AskUserRequest::from_arguments(&confirmation.request.args)
            .map_err(|_| LlmExecutionError::InvalidInputMapping)?;
        let argument_digest = confirmation_argument_digest(
            call_id,
            &confirmation.request.tool_name,
            &confirmation.request.args,
        )?;
        let replay = serde_json::to_value(confirmation.replay)
            .map_err(|_| LlmExecutionError::InvalidInputMapping)?;
        let message = request.message().to_owned();
        let data = json!({
            "schema_revision": "elitea.graph.clarifying-question.v1",
            "type": "hitl",
            "guardrail_type": ASK_USER_GUARDRAIL_TYPE,
            "node_name": self.name(),
            "message": message,
            "questions": request.questions_value(),
            "available_actions": [ASK_USER_ANSWER_ACTION],
            "routes": {},
            "definition_digest": self.definition.digest_label(),
            "tool_call_id": call_id,
            "tool_name": ASK_USER_TOOL_NAME,
            "tool_args": request.arguments_value(),
            "argument_digest": argument_digest,
            "llm_replay": replay,
        });
        Ok(NodeOutput::interrupt_with_data(&message, data))
    }
}

enum PipelineLlmRunOutcome {
    Completed(String),
    Confirmation(Box<PipelineLlmConfirmation>),
}

struct PipelineLlmConfirmation {
    request: adk_rust::ToolConfirmationRequest,
    replay: PipelineLlmReplayEnvelope,
    guard: PipelineToolGuard,
}

pub(super) async fn run_model_agent_text(
    definition: &LlmNodeDefinition,
    input: LlmExecutionInput,
    output_schema: Option<Value>,
    factory: &Arc<dyn PipelineLlmAgentFactory>,
    context: &NodeContext,
) -> Result<String, LlmExecutionError> {
    match run_model_agent(
        definition,
        input,
        "sha256:0000000000000000000000000000000000000000000000000000000000000000",
        output_schema,
        factory,
        None,
        context,
    )
    .await?
    {
        PipelineLlmRunOutcome::Completed(text) => Ok(text),
        PipelineLlmRunOutcome::Confirmation(_) => Err(LlmExecutionError::Unavailable),
    }
}

async fn run_model_agent(
    definition: &LlmNodeDefinition,
    mut input: LlmExecutionInput,
    input_digest: &str,
    output_schema: Option<Value>,
    factory: &Arc<dyn PipelineLlmAgentFactory>,
    replay: Option<&PipelineLlmReplayEnvelope>,
    context: &NodeContext,
) -> Result<PipelineLlmRunOutcome, LlmExecutionError> {
    let predecessor = context
        .state
        .get(LLM_TOOL_RESUME_STATE_KEY)
        .and_then(Value::as_object)
        .and_then(|replays| replays.get(definition.id()));
    let event_scope =
        PipelineNodeEventScope::from_state(context.state.get(PIPELINE_NODE_EVENT_SCOPE_STATE_KEY))
            .map_err(|_| LlmExecutionError::InvalidInputMapping)?;
    let mut transcript = replay.map_or_else(
        || input.initial_transcript(),
        PipelineLlmReplayEnvelope::replay_history,
    );
    if let Some(replay) = replay {
        input.history = replay.replay_history();
    }
    let binding = factory.build(definition, &input, output_schema, replay)?;
    let agent = binding.agent();
    let invocation = Arc::new(PipelineLlmInvocationContext::new(
        &context.config.thread_id,
        input,
        agent.clone(),
        context.config.parent_context.clone(),
        replay,
    ));
    tracing::Span::current().record("stage", "model_tool_loop");
    let stream = agent
        .run(invocation)
        .await
        .map_err(|_| LlmExecutionError::Unavailable)?;
    tokio::pin!(stream);
    let mut events = Vec::new();
    let mut pending_prefix = None;
    let mut pending_content = None;
    while let Some(event) = stream.next().await {
        let mut event = event.map_err(|_| LlmExecutionError::Unavailable)?;
        if let Some(request) = event.actions.tool_confirmation.clone() {
            let pending_content = pending_content
                .take()
                .ok_or(LlmExecutionError::Unavailable)?;
            let pending_prefix = pending_prefix
                .take()
                .ok_or(LlmExecutionError::Unavailable)?;
            let replay = PipelineLlmReplayEnvelope::new(
                definition,
                input_digest.to_owned(),
                predecessor,
                pending_prefix,
                pending_content,
                replay,
            )?;
            let guard = binding
                .guard(&request.tool_name)
                .ok_or(LlmExecutionError::Unavailable)?;
            return Ok(PipelineLlmRunOutcome::Confirmation(Box::new(
                PipelineLlmConfirmation {
                    request,
                    replay,
                    guard,
                },
            )));
        }
        if !event.llm_response.partial
            && let Some(event_content) = event.llm_response.content.as_mut()
        {
            let has_calls = event_content
                .parts
                .iter()
                .any(|part| matches!(part, Part::FunctionCall { .. }));
            if has_calls {
                normalize_replay_call_ids(event_content, &event.invocation_id)?;
            }
            // Replay needs the whole model turn, not just the terminal delta.
            // Do not change the event forwarded to the live browser stream.
            let mut completed_content = event_content.clone();
            completed_content.parts = events
                .iter()
                .filter(|previous: &&adk_rust::Event| previous.id == event.id)
                .filter_map(adk_rust::Event::content)
                .flat_map(|content| content.parts.iter().cloned())
                .chain(event_content.parts.iter().cloned())
                .collect();
            if has_calls {
                pending_prefix = Some(transcript.clone());
                pending_content = Some(completed_content.clone());
            }
            transcript.push(completed_content);
        }
        if event.tool_progress_stream().is_none()
            && let Some(sender) = factory.event_sender()
        {
            sender
                .send(definition.id(), event_scope.as_ref(), event.clone())
                .await
                .map_err(|_| LlmExecutionError::Unavailable)?;
        }
        events.push(event);
    }
    last_model_text(&events)
        .map(PipelineLlmRunOutcome::Completed)
        .ok_or(LlmExecutionError::Unavailable)
}

fn node_failure(node: &str) -> GraphError {
    GraphError::NodeExecutionFailed {
        node: node.to_owned(),
        message: "the pipeline LLM node failed".to_owned(),
    }
}

fn last_model_text(events: &[adk_rust::Event]) -> Option<String> {
    let last = events.iter().rev().find(|event| {
        event.tool_progress_stream().is_none()
            && event.content().is_some_and(|content| {
                matches!(content.role.as_str(), "model" | "assistant")
                    && content
                        .parts
                        .iter()
                        .any(|part| part.text().is_some_and(|text| !text.is_empty()))
            })
    })?;
    // SSE events carry deltas with a stable model-turn ID. Keep streaming to
    // the browser, but return the whole final turn as the node's state output.
    Some(
        events
            .iter()
            .filter(|event| event.id == last.id)
            .filter_map(adk_rust::Event::content)
            .flat_map(|content| &content.parts)
            .filter_map(Part::text)
            .collect(),
    )
}

fn normalize_replay_call_ids(
    content: &mut Content,
    invocation_id: &str,
) -> Result<(), LlmExecutionError> {
    if !valid_replay_identity(invocation_id) {
        return Err(LlmExecutionError::InvalidInputMapping);
    }
    let mut index = 0_usize;
    for part in &mut content.parts {
        if let Part::FunctionCall { name, id, .. } = part {
            if id.is_none() {
                *id = Some(format!("{invocation_id}_{name}_{index}"));
            }
            index += 1;
        }
    }
    replay_calls(content).map(|_| ())
}

fn confirmation_argument_digest(
    call_id: &str,
    tool_name: &str,
    arguments: &Value,
) -> Result<String, LlmExecutionError> {
    if !valid_replay_identity(call_id) || !valid_replay_identity(tool_name) {
        return Err(LlmExecutionError::InvalidInputMapping);
    }
    let fingerprint = tool_call_fingerprint(tool_name, arguments);
    if fingerprint.len() > MAX_LLM_REPLAY_BYTES {
        return Err(LlmExecutionError::ResourceExhausted);
    }
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(b"elitea.graph.llm.arguments.v1\0");
    digest_field(&mut context, call_id.as_bytes());
    digest_field(&mut context, fingerprint.as_bytes());
    Ok(format!("sha256:{}", hex(context.finish().as_ref())))
}

const PIPELINE_REPLAY_PENDING: u8 = 0;
const PIPELINE_REPLAY_DELEGATING: u8 = 1;

struct PipelineLlmReplayModel {
    delegate: Arc<dyn Llm>,
    state: AtomicU8,
    pending_content: Content,
}

#[async_trait]
impl Llm for PipelineLlmReplayModel {
    fn name(&self) -> &str {
        self.delegate.name()
    }

    fn schema_adapter(&self) -> &dyn SchemaAdapter {
        self.delegate.schema_adapter()
    }

    fn uses_interactions_api(&self) -> bool {
        self.delegate.uses_interactions_api()
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        stream_response: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        if self
            .state
            .compare_exchange(
                PIPELINE_REPLAY_PENDING,
                PIPELINE_REPLAY_DELEGATING,
                Ordering::AcqRel,
                Ordering::Acquire,
            )
            .is_ok()
        {
            for call in replay_calls(&self.pending_content)
                .map_err(|_| AdkError::agent("the pipeline LLM replay is malformed"))?
            {
                if !request.tools.contains_key(call.tool_name) {
                    return Err(AdkError::agent(
                        "the pipeline LLM replay tool is unavailable",
                    ));
                }
            }
            tracing::debug!("re-emitting one checkpointed pipeline LLM tool turn through ADK");
            let response = LlmResponse {
                content: Some(self.pending_content.clone()),
                finish_reason: Some(FinishReason::Stop),
                turn_complete: true,
                ..LlmResponse::default()
            };
            return Ok(Box::pin(stream::once(async move { Ok(response) })));
        }
        validate_replay_results(&request, &self.pending_content)?;
        self.delegate
            .generate_content(request, stream_response)
            .await
    }
}

fn validate_replay_results(request: &LlmRequest, pending: &Content) -> adk_rust::Result<()> {
    for call in replay_calls(pending)
        .map_err(|_| AdkError::agent("the pipeline LLM replay is malformed"))?
    {
        let mut call_seen = false;
        let mut response_seen = false;
        for content in &request.contents {
            for part in &content.parts {
                match part {
                    Part::FunctionCall { name, id, args, .. }
                        if id.as_deref() == Some(call.call_id)
                            && name == call.tool_name
                            && args == call.arguments =>
                    {
                        call_seen = true;
                    }
                    Part::FunctionResponse {
                        function_response,
                        id,
                        ..
                    } if call_seen
                        && id.as_deref() == Some(call.call_id)
                        && function_response.name == call.tool_name =>
                    {
                        response_seen = true;
                    }
                    _ => {}
                }
            }
        }
        if !call_seen || !response_seen {
            return Err(AdkError::agent(
                "the checkpointed pipeline LLM tool turn has no exact result",
            ));
        }
    }
    Ok(())
}

#[derive(Clone)]
struct PipelineBlockedToolReplay {
    call_id: String,
    tool_name: String,
    arguments: Value,
    response: Value,
    confirmation_decision: ToolConfirmationDecision,
}

struct PipelineAuthorizationResult {
    replay: PipelineBlockedToolReplay,
}

#[async_trait]
impl Tool for PipelineAuthorizationResult {
    fn name(&self) -> &str {
        &self.replay.tool_name
    }
    fn description(&self) -> &'static str {
        "Return the validated toolkit authorization result."
    }
    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({"type": "object", "properties": {}, "additionalProperties": false}))
    }
    fn is_read_only(&self) -> bool {
        true
    }
    async fn execute(
        &self,
        context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        if context.function_call_id() != self.replay.call_id || arguments != self.replay.arguments {
            return Err(AdkError::agent(
                "authorization result does not match the checkpointed call",
            ));
        }
        Ok(self.replay.response.clone())
    }
}

struct PipelineBlockedToolset {
    name: String,
    inner: Arc<dyn Toolset>,
    blocked: Arc<BTreeMap<String, PipelineBlockedToolReplay>>,
}

#[async_trait]
impl Toolset for PipelineBlockedToolset {
    fn name(&self) -> &str {
        &self.name
    }

    async fn tools(
        &self,
        context: Arc<dyn ReadonlyContext>,
    ) -> adk_rust::Result<Vec<Arc<dyn Tool>>> {
        self.inner.tools(context).await.map(|tools| {
            tools
                .into_iter()
                .map(|inner| {
                    if self
                        .blocked
                        .values()
                        .any(|blocked| blocked.tool_name == inner.name())
                    {
                        Arc::new(PipelineBlockedTool {
                            inner,
                            blocked: Arc::clone(&self.blocked),
                        }) as Arc<dyn Tool>
                    } else {
                        inner
                    }
                })
                .collect()
        })
    }
}

struct PipelineBlockedTool {
    inner: Arc<dyn Tool>,
    blocked: Arc<BTreeMap<String, PipelineBlockedToolReplay>>,
}

#[async_trait]
impl Tool for PipelineBlockedTool {
    fn name(&self) -> &str {
        self.inner.name()
    }

    fn description(&self) -> &str {
        self.inner.description()
    }

    fn declaration(&self) -> Value {
        self.inner.declaration()
    }

    fn enhanced_description(&self) -> String {
        self.inner.enhanced_description()
    }

    fn is_long_running(&self) -> bool {
        self.inner.is_long_running()
    }

    fn is_builtin(&self) -> bool {
        self.inner.is_builtin()
    }

    fn parameters_schema(&self) -> Option<Value> {
        self.inner.parameters_schema()
    }

    fn response_schema(&self) -> Option<Value> {
        self.inner.response_schema()
    }

    fn required_scopes(&self) -> &[&str] {
        self.inner.required_scopes()
    }

    fn is_read_only(&self) -> bool {
        self.inner.is_read_only()
    }

    fn is_concurrency_safe(&self) -> bool {
        self.inner.is_concurrency_safe()
    }

    async fn execute(
        &self,
        context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        let Some(blocked) = self.blocked.get(context.function_call_id()) else {
            return self.inner.execute(context, arguments).await;
        };
        if blocked.tool_name != self.inner.name() || blocked.arguments != arguments {
            return Err(AdkError::agent(
                "the blocked pipeline LLM tool call does not match its checkpointed replay",
            ));
        }
        let mut actions = context.actions();
        actions.tool_confirmation_decision = Some(blocked.confirmation_decision);
        context.set_actions(actions);
        Ok(blocked.response.clone())
    }
}

/// Bind the native ADK replay model and same-call structured block adapters.
pub(crate) fn prepare_pipeline_llm_replay(
    delegate: Arc<dyn Llm>,
    toolsets: Vec<Arc<dyn Toolset>>,
    replay: Option<&PipelineLlmReplayEnvelope>,
) -> (Arc<dyn Llm>, Vec<Arc<dyn Toolset>>) {
    let Some(replay) = replay else {
        return (delegate, toolsets);
    };
    let hidden = replay
        .decisions
        .values()
        .filter(|decision| decision.defer_confirmation && decision.is_authorization_proxy())
        .map(|decision| decision.tool_name.clone())
        .collect();
    let delegate = crate::toolkits::hide_model_tools(delegate, hidden);
    let model: Arc<dyn Llm> = Arc::new(PipelineLlmReplayModel {
        delegate,
        state: AtomicU8::new(PIPELINE_REPLAY_PENDING),
        pending_content: replay.pending_content(),
    });
    let blocked = replay
        .blocked_replays()
        .into_iter()
        .map(|blocked| (blocked.call_id.clone(), blocked))
        .collect::<BTreeMap<_, _>>();
    if blocked.is_empty() {
        return (model, toolsets);
    }
    let mut toolsets = toolsets;
    let authorized: Vec<Arc<dyn Tool>> = blocked
        .values()
        .filter(|blocked| {
            blocked.response["type"] == "mcp_auth_decision"
                && blocked.response["status"] == "authorized"
        })
        .map(|replay| {
            Arc::new(PipelineAuthorizationResult {
                replay: replay.clone(),
            }) as Arc<dyn Tool>
        })
        .collect();
    if !authorized.is_empty() {
        toolsets.push(Arc::new(adk_rust::tool::BasicToolset::new(
            "elitea_authorization_result",
            authorized,
        )));
    }
    let blocked = Arc::new(blocked);
    let toolsets = toolsets
        .into_iter()
        .map(|inner| {
            Arc::new(PipelineBlockedToolset {
                name: format!("{}-blocked", inner.name()),
                inner,
                blocked: Arc::clone(&blocked),
            }) as Arc<dyn Toolset>
        })
        .collect();
    (model, toolsets)
}

pub(super) fn render_fstring(template: &str, state: &State) -> Result<String, LlmExecutionError> {
    let mut rendered = String::with_capacity(template.len());
    let mut cursor = 0;
    while let Some(relative_open) = template[cursor..].find('{') {
        let open = cursor + relative_open;
        rendered.push_str(&template[cursor..open]);
        let Some(relative_end) = template[open + 1..].find('}') else {
            rendered.push_str(&template[open..]);
            cursor = template.len();
            break;
        };
        let end = open + 1 + relative_end;
        let key = &template[open + 1..end];
        if valid_output_key(key)
            && key
                .bytes()
                .all(|byte| byte == b'_' || byte.is_ascii_alphanumeric())
        {
            if let Some(value) = state.get(key) {
                let value = normalize_mapping_value(value)?;
                rendered.push_str(&value);
            } else {
                rendered.push_str(&template[open..=end]);
            }
            cursor = end + 1;
        } else {
            rendered.push('{');
            cursor = open + 1;
        }
        if rendered.len() > MAX_RENDERED_INPUT_BYTES {
            return Err(LlmExecutionError::ResourceExhausted);
        }
    }
    if cursor < template.len() {
        rendered.push_str(&template[cursor..]);
    }
    if rendered.len() > MAX_RENDERED_INPUT_BYTES {
        return Err(LlmExecutionError::ResourceExhausted);
    }
    Ok(rendered)
}

pub(super) fn normalize_mapping_value(value: &Value) -> Result<String, LlmExecutionError> {
    match value {
        Value::String(value) => Ok(value.clone()),
        value => serde_json::to_string(value).map_err(|_| LlmExecutionError::InvalidInputMapping),
    }
}

fn value_as_bounded_text(value: &Value) -> Result<String, LlmExecutionError> {
    let text = normalize_mapping_value(value)?;
    if text.len() > MAX_RENDERED_INPUT_BYTES || text.contains('\0') {
        return Err(LlmExecutionError::ResourceExhausted);
    }
    Ok(text)
}

pub(super) fn parse_messages(value: &Value) -> Result<Vec<Content>, LlmExecutionError> {
    let values = value
        .as_array()
        .ok_or(LlmExecutionError::InvalidInputMapping)?;
    if values.len() > MAX_NODE_VARIABLES * 16 {
        return Err(LlmExecutionError::ResourceExhausted);
    }
    values
        .iter()
        .map(|message| {
            let object = message
                .as_object()
                .ok_or(LlmExecutionError::InvalidInputMapping)?;
            let role = object
                .get("role")
                .and_then(Value::as_str)
                .filter(|role| matches!(*role, "user" | "assistant" | "model" | "tool"))
                .ok_or(LlmExecutionError::InvalidInputMapping)?;
            let role = if role == "assistant" { "model" } else { role };
            parse_message_content(role, object.get("content"))
        })
        .collect()
}

fn parse_message_content(role: &str, value: Option<&Value>) -> Result<Content, LlmExecutionError> {
    let value = value.ok_or(LlmExecutionError::InvalidInputMapping)?;
    if value.is_string() {
        let text = value_as_bounded_text(value)?;
        if text.is_empty() {
            return Err(LlmExecutionError::InvalidInputMapping);
        }
        return Ok(Content::new(role).with_text(text));
    }
    let blocks = value
        .as_array()
        .filter(|blocks| blocks.len() <= MAX_NODE_VARIABLES * 4)
        .ok_or(LlmExecutionError::InvalidInputMapping)?;
    let mut content = Content::new(role);
    let mut total_bytes = 0_usize;
    for block in blocks {
        let text = match block {
            Value::String(_) => value_as_bounded_text(block)?,
            Value::Object(block) => {
                let block_type = block.get("type").and_then(Value::as_str).unwrap_or("text");
                if !matches!(block_type, "text" | "input_text" | "output_text") {
                    // Images and tool-call/result blocks need their exact ADK
                    // part authority; serializing them as prompt text would be
                    // a lossy and potentially privileged conversion.
                    return Err(LlmExecutionError::InvalidInputMapping);
                }
                block
                    .get("text")
                    .map(value_as_bounded_text)
                    .transpose()?
                    .ok_or(LlmExecutionError::InvalidInputMapping)?
            }
            _ => return Err(LlmExecutionError::InvalidInputMapping),
        };
        if text.is_empty() {
            continue;
        }
        total_bytes = total_bytes
            .checked_add(text.len())
            .ok_or(LlmExecutionError::ResourceExhausted)?;
        if total_bytes > MAX_RENDERED_INPUT_BYTES {
            return Err(LlmExecutionError::ResourceExhausted);
        }
        content = content.with_text(text);
    }
    if content.parts.is_empty() {
        return Err(LlmExecutionError::InvalidInputMapping);
    }
    Ok(content)
}

struct PipelineLlmInvocationContext {
    invocation_id: String,
    user_content: Content,
    agent: Arc<dyn Agent>,
    session: PipelineLlmSession,
    run_config: adk_rust::RunConfig,
    parent: Option<Arc<dyn InvocationContext>>,
    ended: AtomicBool,
    user_id: String,
    app_name: String,
    branch: String,
}

impl PipelineLlmInvocationContext {
    fn new(
        session_id: &str,
        mut input: LlmExecutionInput,
        agent: Arc<dyn Agent>,
        parent: Option<Arc<dyn InvocationContext>>,
        replay: Option<&PipelineLlmReplayEnvelope>,
    ) -> Self {
        let (user_id, app_name, branch, mut run_config) = parent.as_ref().map_or_else(
            || {
                (
                    "graph_user".to_owned(),
                    "graph_app".to_owned(),
                    "main".to_owned(),
                    adk_rust::RunConfig::default(),
                )
            },
            |parent| {
                (
                    parent.user_id().to_owned(),
                    parent.app_name().to_owned(),
                    if parent.branch().is_empty() {
                        agent.name().to_owned()
                    } else {
                        format!("{}.{}", parent.branch(), agent.name())
                    },
                    parent.run_config().clone(),
                )
            },
        );
        if let Some(replay) = replay {
            replay.apply_run_config(&mut run_config);
        } else {
            // LlmAgent replaces the last user entry with the current input.
            // A fresh node must append it first, preserving previous user turns.
            // A replay transcript already contains this exact original task.
            input.history.push(input.task.clone());
        }
        Self {
            invocation_id: format!(
                "{session_id}:{}:{}",
                agent.name(),
                LLM_INVOCATION_SEQUENCE.fetch_add(1, Ordering::Relaxed)
            ),
            user_content: input.task,
            agent,
            session: PipelineLlmSession::new(session_id, &app_name, &user_id, input.history),
            run_config,
            parent,
            ended: AtomicBool::new(false),
            user_id,
            app_name,
            branch,
        }
    }
}

impl adk_rust::ReadonlyContext for PipelineLlmInvocationContext {
    fn invocation_id(&self) -> &str {
        &self.invocation_id
    }
    fn agent_name(&self) -> &str {
        self.agent.name()
    }
    fn user_id(&self) -> &str {
        &self.user_id
    }
    fn app_name(&self) -> &str {
        &self.app_name
    }
    fn session_id(&self) -> &str {
        &self.session.id
    }
    fn branch(&self) -> &str {
        &self.branch
    }
    fn user_content(&self) -> &Content {
        &self.user_content
    }
}

#[async_trait]
impl adk_rust::CallbackContext for PipelineLlmInvocationContext {
    fn artifacts(&self) -> Option<Arc<dyn adk_rust::Artifacts>> {
        self.parent.as_ref().and_then(|parent| parent.artifacts())
    }

    fn shared_state(&self) -> Option<Arc<adk_rust::SharedState>> {
        self.parent
            .as_ref()
            .and_then(|parent| parent.shared_state())
    }
}

#[async_trait]
impl InvocationContext for PipelineLlmInvocationContext {
    fn agent(&self) -> Arc<dyn Agent> {
        self.agent.clone()
    }
    fn memory(&self) -> Option<Arc<dyn adk_rust::Memory>> {
        self.parent.as_ref().and_then(|parent| parent.memory())
    }
    fn session(&self) -> &dyn adk_rust::Session {
        &self.session
    }
    fn run_config(&self) -> &adk_rust::RunConfig {
        &self.run_config
    }
    fn end_invocation(&self) {
        self.ended.store(true, Ordering::Release);
        if let Some(parent) = &self.parent {
            parent.end_invocation();
        }
    }
    fn ended(&self) -> bool {
        self.ended.load(Ordering::Acquire)
            || self.parent.as_ref().is_some_and(|parent| parent.ended())
    }
    fn is_cancelled(&self) -> bool {
        self.parent
            .as_ref()
            .is_some_and(|parent| parent.is_cancelled())
    }
    fn user_scopes(&self) -> Vec<String> {
        self.parent
            .as_ref()
            .map(|parent| parent.user_scopes())
            .unwrap_or_default()
    }
    fn request_metadata(&self) -> std::collections::HashMap<String, Value> {
        self.parent
            .as_ref()
            .map(|parent| parent.request_metadata())
            .unwrap_or_default()
    }
    async fn get_secret(&self, name: &str) -> adk_rust::Result<Option<String>> {
        match &self.parent {
            Some(parent) => parent.get_secret(name).await,
            None => Ok(None),
        }
    }
    async fn get_secret_for(
        &self,
        request: &adk_rust::SecretRequest,
    ) -> adk_rust::Result<Option<String>> {
        match &self.parent {
            Some(parent) => parent.get_secret_for(request).await,
            None => Ok(None),
        }
    }
}

struct PipelineLlmSession {
    id: String,
    app_name: String,
    user_id: String,
    state: PipelineLlmSessionState,
    history: std::sync::RwLock<Vec<Content>>,
}

impl PipelineLlmSession {
    fn new(id: &str, app_name: &str, user_id: &str, history: Vec<Content>) -> Self {
        Self {
            id: id.to_owned(),
            app_name: app_name.to_owned(),
            user_id: user_id.to_owned(),
            state: PipelineLlmSessionState::default(),
            history: std::sync::RwLock::new(history),
        }
    }
}

impl adk_rust::Session for PipelineLlmSession {
    fn id(&self) -> &str {
        &self.id
    }
    fn app_name(&self) -> &str {
        &self.app_name
    }
    fn user_id(&self) -> &str {
        &self.user_id
    }
    fn state(&self) -> &dyn adk_rust::State {
        &self.state
    }
    fn conversation_history(&self) -> Vec<Content> {
        self.history
            .read()
            .map_or_else(|_| Vec::new(), |history| history.clone())
    }
    fn append_to_history(&self, content: Content) {
        if let Ok(mut history) = self.history.write() {
            history.push(content);
        }
    }
}

#[derive(Default)]
struct PipelineLlmSessionState {
    values: std::sync::RwLock<std::collections::HashMap<String, Value>>,
}

impl adk_rust::State for PipelineLlmSessionState {
    fn get(&self, key: &str) -> Option<Value> {
        self.values.read().ok()?.get(key).cloned()
    }
    fn set(&mut self, key: String, value: Value) {
        if adk_rust::validate_state_key(&key).is_ok()
            && let Ok(mut values) = self.values.write()
        {
            values.insert(key, value);
        }
    }
    fn all(&self) -> std::collections::HashMap<String, Value> {
        self.values.read().map_or_else(
            |_| std::collections::HashMap::new(),
            |values| values.clone(),
        )
    }
}

fn validate_input_mapping(
    raw: BTreeMap<String, RawInputMapping>,
) -> Result<BTreeMap<String, LlmInputMapping>, LlmConfigurationError> {
    if raw.is_empty() {
        return Ok(BTreeMap::from([(
            "messages".to_owned(),
            LlmInputMapping::Variable("messages".to_owned()),
        )]));
    }
    let mut mapping = BTreeMap::new();
    for (key, value) in raw {
        if !matches!(
            key.as_str(),
            "system" | "task" | "chat_history" | "messages"
        ) {
            return Err(LlmConfigurationError::Unsupported(
                "the LLM input mapping contains an unsupported field",
            ));
        }
        let admitted = match value.kind.as_str() {
            "fixed" => {
                ensure_bounded_mapping_value(&value.value)?;
                LlmInputMapping::Fixed(value.value)
            }
            "variable" => {
                let key = bounded_mapping_text(&value.value)?;
                if !valid_output_key(key) {
                    return Err(LlmConfigurationError::Invalid(
                        "an LLM variable mapping is malformed",
                    ));
                }
                LlmInputMapping::Variable(key.to_owned())
            }
            "fstring" => {
                let template = bounded_mapping_text(&value.value)?;
                LlmInputMapping::Template(template.to_owned())
            }
            _ => {
                return Err(LlmConfigurationError::Unsupported(
                    "the LLM input mapping type is not supported",
                ));
            }
        };
        mapping.insert(key, admitted);
    }
    Ok(mapping)
}

fn validate_tool_selections(
    raw: BTreeMap<String, Vec<String>>,
) -> Result<Vec<LlmToolkitSelection>, LlmConfigurationError> {
    let mut selections = Vec::with_capacity(raw.len());
    for (alias, tools) in raw {
        if !valid_output_key(&alias) || tools.len() > MAX_TOOLS_PER_TOOLKIT {
            return Err(LlmConfigurationError::ResourceExhausted);
        }
        // The UI can retain a toolkit key after its final tool is deselected.
        // It grants no capability and must not cause credential redemption or
        // MCP discovery merely because the empty key survived in YAML.
        if tools.is_empty() {
            continue;
        }
        let mut local_names = BTreeSet::new();
        for tool in &tools {
            if !valid_output_key(tool) || !local_names.insert(tool.as_str()) {
                return Err(LlmConfigurationError::Invalid(
                    "LLM tool names must be valid and unique within each selected toolkit",
                ));
            }
        }
        selections.push(LlmToolkitSelection { alias, tools });
    }
    Ok(selections)
}

fn validate_unique(values: &[String]) -> Result<(), LlmConfigurationError> {
    let mut seen = BTreeSet::new();
    if values.iter().any(|value| !seen.insert(value.as_str())) {
        return Err(LlmConfigurationError::Invalid(
            "LLM node variables must be unique within each field",
        ));
    }
    Ok(())
}

fn bounded_mapping_text(value: &Value) -> Result<&str, LlmConfigurationError> {
    value
        .as_str()
        .filter(|value| value.len() <= MAX_MAPPING_TEXT_BYTES && !value.contains('\0'))
        .ok_or(LlmConfigurationError::Invalid(
            "an LLM input mapping value is malformed",
        ))
}

fn ensure_bounded_mapping_value(value: &Value) -> Result<(), LlmConfigurationError> {
    let encoded = serde_json::to_vec(value).map_err(|_| {
        LlmConfigurationError::Invalid("an LLM fixed input mapping value is malformed")
    })?;
    if encoded.len() > MAX_MAPPING_TEXT_BYTES {
        return Err(LlmConfigurationError::ResourceExhausted);
    }
    Ok(())
}

fn state_type_schema(kind: &str) -> Result<Value, LlmConfigurationError> {
    let schema = match kind {
        "str" => json!({"type": "string"}),
        "int" => json!({"type": "integer"}),
        "float" => json!({"type": "number"}),
        "bool" => json!({"type": "boolean"}),
        "list" => json!({"type": "array"}),
        "dict" => json!({"type": "object"}),
        _ => {
            return Err(LlmConfigurationError::Unsupported(
                "an LLM output uses an unsupported state type",
            ));
        }
    };
    Ok(schema)
}

fn state_value_matches(kind: &str, value: &Value) -> bool {
    match kind {
        "str" => value.is_string(),
        "int" => value.as_i64().is_some() || value.as_u64().is_some(),
        "float" => value.as_f64().is_some_and(f64::is_finite),
        "bool" => value.is_boolean(),
        "list" => value.is_array(),
        "dict" => value.is_object(),
        _ => false,
    }
}

fn digest_field(context: &mut digest::Context, value: &[u8]) {
    context.update(&(value.len() as u64).to_be_bytes());
    context.update(value);
}

fn copy_digest(value: &[u8]) -> [u8; 32] {
    let mut digest = [0_u8; 32];
    digest.copy_from_slice(value);
    digest
}

fn hex(bytes: &[u8]) -> String {
    const DIGITS: &[u8; 16] = b"0123456789abcdef";
    let mut output = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        output.push(char::from(DIGITS[usize::from(byte >> 4)]));
        output.push(char::from(DIGITS[usize::from(byte & 0x0f)]));
    }
    output
}

#[derive(Debug, Error)]
pub(crate) enum LlmConfigurationError {
    #[error("the LLM node YAML is malformed")]
    MalformedYaml {
        #[source]
        source: serde_yaml_ng::Error,
    },
    #[error("{0}")]
    Invalid(&'static str),
    #[error("{0}")]
    Unsupported(&'static str),
    #[error("the LLM node exceeds its resource bound")]
    ResourceExhausted,
}

#[derive(Clone, Copy, Debug, Eq, Error, PartialEq)]
pub(crate) enum LlmExecutionError {
    #[error("the LLM input mapping is invalid")]
    InvalidInputMapping,
    #[error("the LLM structured output is invalid")]
    InvalidStructuredOutput,
    #[error("the LLM output exceeds its resource bound")]
    ResourceExhausted,
    #[error("the LLM runtime is unavailable")]
    Unavailable,
}
