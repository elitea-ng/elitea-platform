//! Native direct Toolkit pipeline node.
//!
//! The stored YAML selects one concrete action from one frozen toolkit alias.
//! Assembly resolves that pair after claim-authorized materialization; the graph
//! node then maps state into JSON arguments and invokes the native ADK [`Tool`]
//! exactly once. It never creates a second agent/model turn.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::graph::{END, GraphError, Node, NodeContext, NodeOutput, State};
use adk_rust::tool::SimpleToolContext;
use adk_rust::{
    CallbackContext, Content, ErrorCategory, EventActions, InvocationContext, MemoryEntry,
    ReadonlyContext, SecretRequest, Tool, ToolContext,
};
use async_trait::async_trait;
use elitea_agent_runtime::graph::node_recovery::{
    NodeAttemptLedger, NodeAttemptPhase, RecoveryDecision, StopReason,
};
use elitea_agent_runtime::graph::node_settings::{EXECUTED_ONCE_KEY, OnNodeError, executed_once};
use ring::digest;
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};
use thiserror::Error;
use tracing::Instrument as _;

use super::compiler::{AdmittedNodeSettings, PIPELINE_YAML_BUDGET};
use super::node_recovery::{NodeFailure, NodeFailureClass, NodeRecoveryPolicy, ReplaySafety};
use super::node_recovery_owner::{NodeRecoveryOwnerProof, NodeRecoveryOwnerProofKind};
use super::node_recovery_runtime::{
    NodeAttemptAuthority, NodeAttemptBody, NodeAttemptReportedFailure, NodeRecoveryClock,
    NodeRecoveryFactory, NodeResultRecovery, RecoverableNode,
};
use super::node_settings_runtime::{
    direct_tool_failure_fault, fill_neutral_outputs, mark_node_executed_once, settle_node_failure,
};
use super::yaml::{valid_graph_id, valid_output_key};
use crate::agents::application_tools::PIPELINE_APPLICATION_NODE_METADATA_KEY;
use crate::agents::events::mask_sensitive_arguments;
use crate::bounded_yaml;
use crate::toolkits::{
    DelegatedAuthorizationRequirement, SensitiveToolPolicy, delegated_authorization_requirement,
};

const MAX_NODE_YAML_BYTES: usize = 64 * 1024;
const MAX_MAPPING_ENTRIES: usize = 256;
const MAX_NODE_VARIABLES: usize = 64;
const MAX_TOOL_IDENTITY_BYTES: usize = 1_024;
const MAX_MAPPING_VALUE_BYTES: usize = 64 * 1024;
const MAX_RESULT_BYTES: usize = 512 * 1024;
const MAX_CONFIRMATION_ARGUMENT_BYTES: usize = 40 * 1024;
const MAX_CONFIRMATION_IDENTITY_BYTES: usize = 512;
const MAX_CONFIRMATION_MESSAGE_BYTES: usize = 16 * 1024;
const CONFIG_DIGEST_DOMAIN: &[u8] = b"elitea.graph.direct_tool.config.v1\0";
const ARGUMENT_DIGEST_DOMAIN: &[u8] = b"elitea.graph.direct_tool.arguments.v1\0";
const TOOL_CONFIRMATION_SCHEMA: &str = "elitea.graph.tool-confirmation.v1";
const MCP_AUTHORIZATION_SCHEMA: &str = "elitea.graph.mcp-authorization.v1";
pub(crate) const DIRECT_TOOL_RESUME_STATE_KEY: &str = "__elitea_tool_resume_v1";

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawDirectToolNodeDefinition {
    id: String,
    #[serde(rename = "type")]
    node_type: String,
    toolkit_name: String,
    tool: String,
    #[serde(default = "default_input_mapping")]
    input_mapping: BTreeMap<String, RawInputMapping>,
    #[serde(default)]
    input: Vec<String>,
    #[serde(default)]
    output: Vec<String>,
    #[serde(default)]
    structured_output: bool,
    #[serde(default)]
    transition: Option<String>,
}

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawInputMapping {
    #[serde(rename = "type")]
    kind: String,
    value: Value,
}

/// One direct-tool argument mapping evaluated against checkpointed state.
#[derive(Clone)]
pub(super) enum DirectToolInputMapping {
    Fixed(Value),
    Variable(String),
    Template(String),
}

/// Exact toolset identity selected by one direct Toolkit or MCP node.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct DirectToolSelection {
    kind: DirectToolNodeKind,
    alias: String,
    tool: String,
}

impl DirectToolSelection {
    pub(crate) const fn kind(&self) -> DirectToolNodeKind {
        self.kind
    }

    pub(crate) fn alias(&self) -> &str {
        &self.alias
    }

    pub(crate) fn tool(&self) -> &str {
        &self.tool
    }
}

/// Active direct-call node family selected by stored pipeline YAML.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum DirectToolNodeKind {
    Toolkit,
    Mcp,
}

impl DirectToolNodeKind {
    pub(crate) const fn wire_name(self) -> &'static str {
        match self {
            Self::Toolkit => "toolkit",
            Self::Mcp => "mcp",
        }
    }
}

/// Strict, authority-free direct Toolkit or MCP node definition.
#[derive(Clone)]
pub(crate) struct DirectToolNodeDefinition {
    id: String,
    selection: DirectToolSelection,
    input_mapping: BTreeMap<String, DirectToolInputMapping>,
    input: Vec<String>,
    output: Vec<String>,
    structured_output: bool,
    transition: Option<String>,
}

impl DirectToolNodeDefinition {
    pub(super) fn resolve_legacy_toolkit_aliases(&mut self, aliases: &BTreeMap<String, String>) {
        if let Some(canonical) = aliases.get(&self.selection.alias) {
            self.selection.alias.clone_from(canonical);
        }
    }

    pub(super) fn from_yaml(yaml: &str) -> Result<Self, DirectToolConfigurationError> {
        if yaml.is_empty() || yaml.len() > MAX_NODE_YAML_BYTES {
            return Err(DirectToolConfigurationError::ResourceExhausted);
        }
        let raw = bounded_yaml::from_str_as_yaml_error::<RawDirectToolNodeDefinition>(
            yaml,
            PIPELINE_YAML_BUDGET,
        )
        .map_err(|source| DirectToolConfigurationError::MalformedYaml { source })?;
        Self::from_raw(raw)
    }

    fn from_raw(
        mut raw: RawDirectToolNodeDefinition,
    ) -> Result<Self, DirectToolConfigurationError> {
        let kind = match raw.node_type.as_str() {
            "toolkit" => DirectToolNodeKind::Toolkit,
            "mcp" => DirectToolNodeKind::Mcp,
            _ => {
                return Err(DirectToolConfigurationError::Invalid(
                    "the direct-tool node type is unsupported",
                ));
            }
        };
        if !valid_graph_id(&raw.id) {
            return Err(DirectToolConfigurationError::Invalid(
                "the direct-tool node ID is malformed",
            ));
        }
        if !valid_tool_identity(&raw.toolkit_name) || !valid_tool_identity(&raw.tool) {
            return Err(DirectToolConfigurationError::Invalid(
                "the direct-tool node tool identity is malformed",
            ));
        }
        if raw.input_mapping.len() > MAX_MAPPING_ENTRIES
            || raw.input.len() > MAX_NODE_VARIABLES
            || raw.output.len() > MAX_NODE_VARIABLES
        {
            return Err(DirectToolConfigurationError::ResourceExhausted);
        }
        if raw.input.is_empty() {
            raw.input.push("messages".to_owned());
        }
        for key in raw.input.iter().chain(&raw.output) {
            if !valid_output_key(key) {
                return Err(DirectToolConfigurationError::Invalid(
                    "a direct-tool node state variable is malformed",
                ));
            }
        }
        validate_unique(&raw.input)?;
        validate_unique(&raw.output)?;
        if raw.structured_output && raw.output.iter().all(|key| key == "messages") {
            return Err(DirectToolConfigurationError::Invalid(
                "structured Toolkit output requires a data state variable",
            ));
        }
        if raw
            .transition
            .as_deref()
            .is_some_and(|target| target != "END" && !valid_graph_id(target))
        {
            return Err(DirectToolConfigurationError::Invalid(
                "the direct-tool node transition is malformed",
            ));
        }
        let input_mapping = validate_input_mapping(raw.input_mapping)?;
        Ok(Self {
            id: raw.id,
            selection: DirectToolSelection {
                kind,
                alias: raw.toolkit_name,
                tool: raw.tool,
            },
            input_mapping,
            input: raw.input,
            output: raw.output,
            structured_output: raw.structured_output,
            transition: raw.transition,
        })
    }

    pub(crate) fn id(&self) -> &str {
        &self.id
    }

    pub(crate) const fn selection(&self) -> &DirectToolSelection {
        &self.selection
    }

    pub(super) fn input_keys(&self) -> &[String] {
        &self.input
    }

    pub(super) fn output_keys(&self) -> &[String] {
        &self.output
    }

    pub(super) fn input_mapping(&self) -> &BTreeMap<String, DirectToolInputMapping> {
        &self.input_mapping
    }

    pub(super) const fn structured_output(&self) -> bool {
        self.structured_output
    }

    pub(super) fn transition(&self) -> Option<&str> {
        self.transition.as_deref()
    }

    pub(super) fn config_digest(&self) -> [u8; 32] {
        let mut context = digest::Context::new(&digest::SHA256);
        context.update(CONFIG_DIGEST_DOMAIN);
        digest_field(&mut context, self.id.as_bytes());
        digest_field(&mut context, self.selection.kind.wire_name().as_bytes());
        digest_field(&mut context, self.selection.alias.as_bytes());
        digest_field(&mut context, self.selection.tool.as_bytes());
        for (key, mapping) in &self.input_mapping {
            digest_field(&mut context, key.as_bytes());
            match mapping {
                DirectToolInputMapping::Fixed(value) => {
                    digest_field(&mut context, b"fixed");
                    digest_field(&mut context, &serde_json::to_vec(value).unwrap_or_default());
                }
                DirectToolInputMapping::Variable(value) => {
                    digest_field(&mut context, b"variable");
                    digest_field(&mut context, value.as_bytes());
                }
                DirectToolInputMapping::Template(value) => {
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
        digest_field(&mut context, &[u8::from(self.structured_output)]);
        digest_field(
            &mut context,
            self.transition.as_deref().unwrap_or_default().as_bytes(),
        );
        copy_digest(context.finish().as_ref())
    }

    fn map_arguments(&self, state: &State) -> Result<Value, DirectToolExecutionError> {
        let mut arguments = Map::new();
        for (key, mapping) in &self.input_mapping {
            let value = match mapping {
                DirectToolInputMapping::Fixed(value) => value.clone(),
                DirectToolInputMapping::Variable(source) => state
                    .get(source)
                    .cloned()
                    .unwrap_or(Value::String(String::new())),
                DirectToolInputMapping::Template(template) => {
                    Value::String(render_fstring(template, state)?)
                }
            };
            arguments.insert(key.clone(), value);
        }
        ensure_bounded_value(&Value::Object(arguments.clone()))?;
        Ok(Value::Object(arguments))
    }

    fn project_result(
        &self,
        result: Value,
        state_types: &BTreeMap<String, String>,
    ) -> Result<BTreeMap<String, Value>, DirectToolExecutionError> {
        ensure_bounded_value(&result)?;
        let mut updates = BTreeMap::new();
        if self.output.is_empty() {
            updates.insert("messages".to_owned(), assistant_message(&result)?);
            return Ok(updates);
        }

        if self.structured_output {
            let object = result
                .as_object()
                .ok_or(DirectToolExecutionError::InvalidResult)?;
            for key in self.output.iter().filter(|key| key.as_str() != "messages") {
                let value = object
                    .get(key)
                    .cloned()
                    .ok_or(DirectToolExecutionError::InvalidResult)?;
                ensure_state_type(key, &value, state_types)?;
                updates.insert(key.clone(), value);
            }
            if self.output.iter().any(|key| key == "messages") {
                updates.insert("messages".to_owned(), projected_messages(&result)?);
            }
        } else if self.output.iter().any(|key| key == "messages") {
            updates.insert("messages".to_owned(), projected_messages(&result)?);
            for key in self.output.iter().filter(|key| key.as_str() != "messages") {
                let value = result
                    .as_object()
                    .and_then(|object| object.get(key))
                    .cloned()
                    .unwrap_or_else(|| result.clone());
                ensure_state_type(key, &value, state_types)?;
                updates.insert(key.clone(), value);
            }
        } else if let Some(key) = self.output.first() {
            ensure_state_type(key, &result, state_types)?;
            updates.insert(key.clone(), result);
        }
        ensure_bounded_value(&serde_json::to_value(&updates).unwrap_or(Value::Null))?;
        Ok(updates)
    }
}

/// Invocation-owned lookup of an already materialized concrete tool.
pub(crate) trait PipelineDirectToolResolver: Send + Sync {
    fn resolve(
        &self,
        selection: &DirectToolSelection,
    ) -> Result<ResolvedDirectTool, DirectToolExecutionError>;

    /// The run's public egress policy and ledger (#1490). Delegated-auth
    /// discovery runs after the tool call, outside any egress scope, and is
    /// held to this. A resolver that names no run reaches no public host.
    fn egress(&self) -> std::sync::Arc<elitea_agent_runtime::egress::RunEgress> {
        elitea_agent_runtime::egress::RunEgress::closed()
    }
}

/// One exact materialized tool plus its immutable invocation policy.
#[derive(Clone)]
pub(crate) struct ResolvedDirectTool {
    tool: Arc<dyn Tool>,
    sensitive: Option<SensitiveToolPolicy>,
}

impl ResolvedDirectTool {
    #[must_use]
    pub(crate) const fn new(tool: Arc<dyn Tool>, sensitive: Option<SensitiveToolPolicy>) -> Self {
        Self { tool, sensitive }
    }
}

/// Native graph node that invokes one exact ADK tool.
#[derive(Clone)]
pub(super) struct DirectToolNode {
    events: Option<super::node_events::PipelineNodeEventSender>,
    definition: Arc<DirectToolNodeDefinition>,
    state_types: Arc<BTreeMap<String, String>>,
    resolver: Arc<dyn PipelineDirectToolResolver>,
    // Effectful tools run only behind the fenced Started journal.
    recovery: Option<Arc<dyn NodeRecoveryFactory>>,
    /// Per-node settings (5b-3): `None` keeps zero-overhead legacy execution.
    settings: Option<Arc<AdmittedNodeSettings>>,
    execution_id: Option<Arc<String>>,
    execution_deadline: Option<tokio::time::Instant>,
    clock: Option<Arc<dyn NodeRecoveryClock>>,
}

impl DirectToolNode {
    pub(super) fn with_events(
        mut self,
        events: Option<super::node_events::PipelineNodeEventSender>,
    ) -> Self {
        self.events = events;
        self
    }

    pub(super) fn new(
        definition: DirectToolNodeDefinition,
        state_types: BTreeMap<String, String>,
        resolver: Arc<dyn PipelineDirectToolResolver>,
    ) -> Self {
        Self {
            definition: Arc::new(definition),
            state_types: Arc::new(state_types),
            resolver,
            events: None,
            recovery: None,
            settings: None,
            execution_id: None,
            execution_deadline: None,
            clock: None,
        }
    }

    pub(super) fn with_node_recovery(mut self, authority: Arc<dyn NodeRecoveryFactory>) -> Self {
        self.recovery = Some(authority);
        self
    }

    pub(super) fn with_settings(
        mut self,
        admitted: AdmittedNodeSettings,
        execution_id: Option<Arc<String>>,
    ) -> Self {
        self.settings = Some(Arc::new(admitted));
        self.execution_id = execution_id;
        self
    }

    pub(super) fn with_execution_deadline(
        mut self,
        deadline: Option<tokio::time::Instant>,
    ) -> Self {
        self.execution_deadline = deadline;
        self
    }

    #[cfg(test)]
    pub(super) fn with_clock(mut self, clock: Arc<dyn NodeRecoveryClock>) -> Self {
        self.clock = Some(clock);
        self
    }

    /// Platform-only read-only classification (T-5B §3.2, expert 02 F6).
    /// Native toolkit families use `Tool::is_read_only()`; remote MCP
    /// `readOnlyHint` is NEVER trusted as proof of retry safety.
    fn is_platform_read_only(&self, tool: &dyn Tool) -> bool {
        self.definition.selection().kind() == DirectToolNodeKind::Toolkit && tool.is_read_only()
    }

    /// Whether this invocation must execute behind the fenced Started journal.
    /// Effectful native tools always require a journal. Under `settings:`, any
    /// MCP tool is also treated as effectful (never trusting `readOnlyHint`).
    fn requires_effect_journal(&self, tool: &dyn Tool) -> bool {
        if self.settings.is_some() {
            !self.is_platform_read_only(tool)
        } else {
            !tool.is_read_only()
        }
    }

    fn timeout_ms(&self) -> Option<u64> {
        self.settings
            .as_ref()
            .and_then(|admitted| admitted.settings.timeout_ms)
    }

    fn retry_deadline_budget(&self) -> Option<(u64, u64)> {
        let deadline = self.execution_deadline?;
        let remaining_ms = deadline
            .checked_duration_since(tokio::time::Instant::now())
            .map_or(0, |duration| {
                u64::try_from(duration.as_millis()).unwrap_or(u64::MAX)
            });
        Some((remaining_ms, 1))
    }

    fn now_ms(&self) -> Result<u64, DirectNodeFailure> {
        if let Some(clock) = &self.clock {
            return clock
                .now_ms()
                .map_err(|_| DirectNodeFailure::policy("tool_execution", "Invalid clock state."));
        }
        u64::try_from(
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map_err(|_| DirectNodeFailure::policy("tool_execution", "Invalid clock state."))?
                .as_millis(),
        )
        .map_err(|_| DirectNodeFailure::policy("tool_execution", "Invalid clock state."))
    }

    async fn wait_until(&self, target_ms: u64) -> Result<(), DirectNodeFailure> {
        if let Some(clock) = &self.clock {
            return clock.wait_until(target_ms).await.map_err(|_| {
                DirectNodeFailure::policy("tool_execution", "Retry wait interrupted.")
            });
        }
        let now = self.now_ms()?;
        if target_ms > now {
            tokio::time::sleep(Duration::from_millis((target_ms - now).min(100))).await;
        }
        Ok(())
    }

    async fn execute_mapped(&self, context: &NodeContext) -> Result<NodeOutput, DirectNodeFailure> {
        // D8: revisit in the same turn runs no tool, opens no journal, and
        // changes no state.
        if self
            .settings
            .as_ref()
            .is_some_and(|admitted| admitted.settings.execute_once)
            && executed_once(context.get(EXECUTED_ONCE_KEY), self.name()).is_some()
        {
            return Ok(NodeOutput::new());
        }
        tracing::Span::current().record("stage", "input_mapping");
        let arguments = self
            .definition
            .map_arguments(&context.state)
            .map_err(|cause| DirectNodeFailure::new("input_mapping", cause))?;
        tracing::Span::current().record("stage", "tool_binding");
        let ResolvedDirectTool { tool, sensitive } = self
            .resolver
            .resolve(self.definition.selection())
            .map_err(|cause| DirectNodeFailure::new("tool_binding", cause))?;
        if self.requires_effect_journal(tool.as_ref()) && self.recovery.is_none() {
            return Err(DirectNodeFailure::policy(
                "tool_binding",
                EFFECT_WRITER_UNAVAILABLE,
            ));
        }
        let tool_context = pipeline_tool_context(context, self.name(), tool.name());
        let granted_scopes = tool_context.user_scopes();
        if tool
            .required_scopes()
            .iter()
            .any(|required| !granted_scopes.iter().any(|granted| granted == required))
        {
            return Err(DirectNodeFailure::policy(
                "authorization",
                "The execution does not have the required tool permissions.",
            ));
        }
        if self.has_mcp_authorization_resume(context)
            && let Some(decision) = self
                .mcp_authorization_decision(context, &arguments, tool_context.function_call_id())
                .map_err(|cause| DirectNodeFailure::new("authorization_resume", cause))?
        {
            return match decision {
                McpAuthorizationDecision::Authorize(remaining) => {
                    self.dispatch(
                        context,
                        &tool,
                        tool_context,
                        arguments,
                        Some(remaining),
                        true,
                    )
                    .await
                }
                McpAuthorizationDecision::Skip(remaining) => {
                    Ok(self.mcp_authorization_stopped(remaining, false))
                }
            };
        }
        if let Some(policy) = sensitive.as_ref() {
            return self
                .execute_sensitive(context, &tool, tool_context, arguments, policy)
                .await;
        }
        if context
            .state
            .get(DIRECT_TOOL_RESUME_STATE_KEY)
            .and_then(Value::as_object)
            .is_some_and(|resumes| resumes.contains_key(self.name()))
        {
            return Err(DirectNodeFailure::policy(
                "authorization_resume",
                "The saved tool decision does not match this node invocation.",
            ));
        }
        self.dispatch(context, &tool, tool_context, arguments, None, false)
            .await
    }

    async fn execute_sensitive(
        &self,
        context: &NodeContext,
        tool: &Arc<dyn Tool>,
        tool_context: Arc<dyn ToolContext>,
        arguments: Value,
        policy: &SensitiveToolPolicy,
    ) -> Result<NodeOutput, DirectNodeFailure> {
        tracing::Span::current().record("stage", "tool_confirmation");
        match self
            .sensitive_decision(context, &arguments, tool_context.function_call_id(), policy)
            .map_err(|cause| DirectNodeFailure::new("tool_confirmation", cause))?
        {
            SensitiveDirectToolDecision::Pause(data) => Ok(NodeOutput::interrupt_with_data(
                policy.policy_message(),
                data,
            )),
            SensitiveDirectToolDecision::Approve(remaining) => {
                self.dispatch(
                    context,
                    tool,
                    tool_context,
                    arguments,
                    Some(remaining),
                    false,
                )
                .await
            }
            SensitiveDirectToolDecision::Block {
                remaining,
                result,
                message,
            } => Ok(NodeOutput::new()
                .with_update(DIRECT_TOOL_RESUME_STATE_KEY, remaining)
                .with_update("_pipeline_blocked", Value::String(message.clone()))
                .with_update(
                    "messages",
                    blocked_messages(
                        tool_context.function_call_id(),
                        tool.name(),
                        &result,
                        &message,
                    ),
                )
                .with_goto([END])),
        }
    }

    /// Pauses, blocks and skips are decided before this point and never reach the
    /// journal or retry loop. Effectful tools run behind the fenced Started
    /// journal; platform-classified read-only native tools may retry safely.
    #[allow(
        clippy::too_many_lines,
        reason = "Keep effect-journal admission, live/restore parity, and read-only settings dispatch cohesive."
    )]
    async fn dispatch(
        &self,
        context: &NodeContext,
        tool: &Arc<dyn Tool>,
        tool_context: Arc<dyn ToolContext>,
        arguments: Value,
        remaining: Option<Value>,
        authorization_refresh: bool,
    ) -> Result<NodeOutput, DirectNodeFailure> {
        let Some(authority) = self
            .recovery
            .as_ref()
            .filter(|_| self.requires_effect_journal(tool.as_ref()))
        else {
            if let Some(admitted) = &self.settings {
                return self
                    .dispatch_read_only_with_settings(
                        context,
                        admitted,
                        tool.as_ref(),
                        tool_context,
                        arguments,
                        remaining,
                        authorization_refresh,
                    )
                    .await;
            }
            return self
                .invoke_and_project(
                    tool.as_ref(),
                    tool_context,
                    arguments,
                    remaining,
                    authorization_refresh,
                )
                .await;
        };
        let policy = match &self.settings {
            Some(admitted) => admitted.settings.to_recovery_policy().map_err(|_| {
                DirectNodeFailure::policy(
                    "tool_binding",
                    "The tool node settings could not compile to a recovery policy.",
                )
            })?,
            None => NodeRecoveryPolicy::default(),
        };
        let control = Arc::new(Mutex::new(None));
        let completed = Arc::new(AtomicBool::new(false));
        let attempt = DirectToolAttempt {
            node: self.clone(),
            tool: Arc::clone(tool),
            tool_context,
            arguments,
            remaining,
            authorization_refresh,
            control: Arc::clone(&control),
            completed: Arc::clone(&completed),
        };
        let mut recoverable = RecoverableNode::new(
            Arc::new(attempt),
            self.definition.config_digest(),
            policy,
            Arc::clone(authority),
            None,
        );
        if let Some(admitted) = &self.settings {
            recoverable =
                recoverable.with_settings(admitted.as_ref().clone(), self.execution_id.clone());
        }
        if let Some(clock) = &self.clock {
            recoverable = recoverable.with_clock(Arc::clone(clock));
        }
        let result = recoverable.execute(context).await;
        // Live/restore parity (SEC-5B2-04): a settled `on_error: continue`/
        // `error_output` output (or completed output) has no interrupt and must
        // not be overwritten by a stale `control = Some(Err(_))`.
        let handed_over = control
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .take();
        if let Ok(output) = result {
            if output.interrupt.is_none() {
                return Ok(output);
            }
            if let Some(handed_over) = handed_over {
                return handed_over;
            }
            return Ok(output);
        }
        if let Some(handed_over) = handed_over {
            return handed_over;
        }
        result.map_err(|error| {
            if matches!(&error, GraphError::Other(code)
                if code == super::node_recovery_runtime::CHILD_EFFECT_UNKNOWN_CODE)
            {
                return DirectNodeFailure::may_have_started();
            }
            if matches!(
                &error,
                GraphError::Other(code)
                    if code == "pipeline.node_errors.exhausted"
                        || code == "pipeline.node_settings.invalid"
                        || code == "pipeline.node_recovery.cancelled"
            ) {
                return DirectNodeFailure::graph(error);
            }
            tracing::error!(failure_reason = %error, "pipeline direct-tool node recovery stopped");
            DirectNodeFailure::policy(
                "tool_execution",
                if completed.load(Ordering::SeqCst) {
                    "The tool completed, but its result could not be recorded. It will not run again."
                } else {
                    "The tool attempt was stopped by node recovery."
                },
            )
        })
    }

    #[allow(
        clippy::too_many_arguments,
        reason = "Keep bounded read-only retry ledger, deadline clamping, and settlement explicit."
    )]
    #[allow(
        clippy::too_many_lines,
        reason = "Keep read-only settings retry loop, cancellation check, and settlement cohesive."
    )]
    async fn dispatch_read_only_with_settings(
        &self,
        context: &NodeContext,
        admitted: &Arc<AdmittedNodeSettings>,
        tool: &dyn Tool,
        tool_context: Arc<dyn ToolContext>,
        arguments: Value,
        remaining: Option<Value>,
        authorization_refresh: bool,
    ) -> Result<NodeOutput, DirectNodeFailure> {
        let policy = admitted.settings.to_recovery_policy().map_err(|_| {
            DirectNodeFailure::policy(
                "tool_binding",
                "The tool node settings could not compile to a recovery policy.",
            )
        })?;
        let mut ledger = NodeAttemptLedger::new(self.definition.config_digest(), policy)
            .map_err(|_| DirectNodeFailure::policy("tool_execution", "Invalid retry state."))?;
        loop {
            let now = self.now_ms()?;
            if let Some(expired) = ledger
                .expire_wait(now)
                .map_err(|_| DirectNodeFailure::policy("tool_execution", "Invalid retry state."))?
            {
                ledger = expired;
            }
            if context
                .config
                .parent_context
                .as_ref()
                .is_some_and(|parent| parent.is_cancelled())
            {
                return Err(DirectNodeFailure::graph(GraphError::Other(
                    "pipeline.node_recovery.cancelled".to_owned(),
                )));
            }
            match ledger.phase() {
                NodeAttemptPhase::Ready => {
                    ledger = ledger.start_attempt(now).map_err(|_| {
                        DirectNodeFailure::policy("tool_execution", "Invalid retry transition.")
                    })?;
                }
                NodeAttemptPhase::Failed(RecoveryDecision::RetryAt { not_before_ms, .. }) => {
                    if now < not_before_ms {
                        self.wait_until(not_before_ms).await?;
                        continue;
                    }
                    ledger = ledger.start_attempt(now).map_err(|_| {
                        DirectNodeFailure::policy("tool_execution", "Invalid retry transition.")
                    })?;
                }
                _ => {
                    return Err(DirectNodeFailure::policy(
                        "tool_execution",
                        "Invalid retry phase.",
                    ));
                }
            }
            match self
                .invoke_and_project(
                    tool,
                    Arc::clone(&tool_context),
                    arguments.clone(),
                    remaining.clone(),
                    authorization_refresh,
                )
                .await
            {
                Ok(output) if output.interrupt.is_some() || output.goto.is_some() => {
                    // Auth challenge or auth-stopped terminal: never retry and
                    // never record execute_once.
                    return Ok(output);
                }
                Ok(output) => {
                    let output = fill_neutral_outputs(admitted, output);
                    return mark_node_executed_once(admitted, self.name(), context, output, None)
                        .map_err(DirectNodeFailure::graph);
                }
                Err(failure) => {
                    let class = failure.failure_class();
                    let recorded = NodeFailure::new(class, ReplaySafety::NoExternalEffect);
                    let now = self.now_ms()?;
                    let run_window =
                        self.retry_deadline_budget()
                            .map(|(remaining_ms, required_ms)| {
                                (now.saturating_add(remaining_ms), required_ms)
                            });
                    ledger = if failure.retry_after_ms.is_some() {
                        ledger.record_failure_with_retry_after(
                            recorded,
                            now,
                            failure.retry_after_ms,
                            run_window,
                        )
                    } else if let Some((run_deadline_ms, required_ms)) = run_window {
                        ledger.record_failure_within(recorded, now, run_deadline_ms, required_ms)
                    } else {
                        ledger.record_failure(recorded, now)
                    }
                    .map_err(|_| {
                        DirectNodeFailure::policy("tool_execution", "Invalid retry transition.")
                    })?;
                    match ledger.phase() {
                        NodeAttemptPhase::Failed(RecoveryDecision::RetryAt { .. }) => {}
                        NodeAttemptPhase::Failed(RecoveryDecision::Stop(reason)) => {
                            if can_settle_direct_tool_stop(
                                admitted.settings.on_error,
                                reason,
                                class,
                            ) {
                                let attempts =
                                    u16::try_from(ledger.history().len()).unwrap_or(1).max(1);
                                return settle_node_failure(
                                    admitted,
                                    self.execution_id.as_deref().map(String::as_str),
                                    self.name(),
                                    context,
                                    direct_tool_failure_fault(admitted.family, class),
                                    class.retry_condition().is_some(),
                                    attempts,
                                )
                                .map_err(DirectNodeFailure::graph);
                            }
                            return Err(failure);
                        }
                        _ => return Err(failure),
                    }
                }
            }
        }
    }

    async fn invoke_and_project(
        &self,
        tool: &dyn Tool,
        tool_context: Arc<dyn ToolContext>,
        arguments: Value,
        remaining: Option<Value>,
        authorization_refresh: bool,
    ) -> Result<NodeOutput, DirectNodeFailure> {
        tracing::Span::current().record("stage", "tool_execution");
        let now = tokio::time::Instant::now();
        let remaining_run = self
            .execution_deadline
            .map(|deadline| deadline.saturating_duration_since(now));
        if remaining_run.is_some_and(|remaining| remaining.is_zero()) {
            return Err(DirectNodeFailure::deadline_exceeded_before_dispatch());
        }
        let call_id = tool_context.function_call_id().to_owned();
        let argument_digest = argument_digest(&call_id, tool.name(), &arguments)
            .map_err(|cause| DirectNodeFailure::new("argument_digest", cause))?;
        let attempt_limit = match (self.timeout_ms().map(Duration::from_millis), remaining_run) {
            (Some(node_limit), Some(run_limit)) => {
                Some((node_limit.min(run_limit), run_limit < node_limit))
            }
            (Some(node_limit), None) => Some((node_limit, false)),
            (None, Some(run_limit)) => Some((run_limit, true)),
            (None, None) => None,
        };
        let call = tool.execute(tool_context, arguments);
        let outcome = match attempt_limit {
            Some((limit, bound_by_run_deadline)) => {
                if let Ok(result) = tokio::time::timeout(limit, call).await {
                    result
                } else {
                    if bound_by_run_deadline
                        || self
                            .execution_deadline
                            .is_some_and(|deadline| tokio::time::Instant::now() >= deadline)
                    {
                        return Err(DirectNodeFailure::deadline_exceeded_in_flight());
                    }
                    return Err(DirectNodeFailure::timeout());
                }
            }
            None => call.await,
        };
        let result = match outcome {
            Ok(result) => result,
            Err(error) => {
                let Some(requirement) = delegated_authorization_requirement(&error) else {
                    return Err(DirectNodeFailure::tool_error(&error));
                };
                if authorization_refresh {
                    return Ok(self
                        .mcp_authorization_stopped(remaining.unwrap_or_else(|| json!({})), true));
                }
                let requirement = requirement
                    .resolve_public_metadata(&self.resolver.egress())
                    .await;
                return Ok(self.mcp_authorization_interrupt(
                    &requirement,
                    &call_id,
                    &argument_digest,
                ));
            }
        };
        tracing::Span::current().record("stage", "state_projection");
        let updates = self
            .definition
            .project_result(result, &self.state_types)
            .map_err(|cause| DirectNodeFailure::new("state_projection", cause))?;
        let mut output = NodeOutput::new();
        if let Some(remaining) = remaining {
            output = output.with_update(DIRECT_TOOL_RESUME_STATE_KEY, remaining);
        }
        for (key, value) in updates {
            output = output.with_update(&key, value);
        }
        Ok(output)
    }

    fn mcp_authorization_decision(
        &self,
        context: &NodeContext,
        arguments: &Value,
        call_id: &str,
    ) -> Result<Option<McpAuthorizationDecision>, DirectToolExecutionError> {
        let resumes = context
            .state
            .get(DIRECT_TOOL_RESUME_STATE_KEY)
            .map(|value| {
                value
                    .as_object()
                    .ok_or(DirectToolExecutionError::InvalidArguments)
            })
            .transpose()?;
        let Some(raw) = resumes.and_then(|resumes| resumes.get(self.name())) else {
            return Ok(None);
        };
        let expected_definition = self.definition.digest_label();
        let expected_arguments =
            argument_digest(call_id, self.definition.selection().tool(), arguments)?;
        let decision =
            McpAuthorizationResume::parse(raw, &expected_definition, call_id, &expected_arguments)?;
        let mut remaining = resumes
            .ok_or(DirectToolExecutionError::InvalidArguments)?
            .clone();
        if remaining.remove(self.name()).is_none() {
            return Err(DirectToolExecutionError::InvalidArguments);
        }
        let remaining = Value::Object(remaining);
        Ok(Some(match decision {
            McpAuthorizationResume::Authorize => McpAuthorizationDecision::Authorize(remaining),
            McpAuthorizationResume::Skip => McpAuthorizationDecision::Skip(remaining),
        }))
    }

    fn has_mcp_authorization_resume(&self, context: &NodeContext) -> bool {
        context
            .state
            .get(DIRECT_TOOL_RESUME_STATE_KEY)
            .and_then(Value::as_object)
            .and_then(|resumes| resumes.get(self.name()))
            .and_then(Value::as_object)
            .and_then(|resume| resume.get("action"))
            .and_then(Value::as_str)
            .is_some_and(|action| matches!(action, "authorize" | "skip"))
    }

    fn mcp_authorization_interrupt(
        &self,
        requirement: &DelegatedAuthorizationRequirement,
        call_id: &str,
        argument_digest: &str,
    ) -> NodeOutput {
        let message = requirement.user_message();
        let mut data = json!({
                "schema_revision": MCP_AUTHORIZATION_SCHEMA,
                "type": "hitl",
                "guardrail_type": "mcp_auth",
                "node_name": self.name(),
                "message": message,
                "available_actions": ["authorize", "skip"],
                "routes": {},
                "definition_digest": self.definition.digest_label(),
                "tool_call_id": call_id,
                "tool_name": self.definition.selection().tool(),
                "toolkit_name": requirement.toolkit_name(),
                "toolkit_type": requirement.toolkit_type(),
                "tool_args": {},
                "argument_digest": argument_digest,
                "server_url": requirement.server_url(),
                "resource_metadata_url": requirement.resource_metadata_url(),
                "www_authenticate": requirement.www_authenticate(),
                "resource_metadata": requirement.resource_metadata(),
        });
        with_pause_toolkit_id(&mut data, requirement);
        NodeOutput::interrupt_with_data(&message, data)
    }

    fn mcp_authorization_stopped(&self, remaining: Value, refresh_failed: bool) -> NodeOutput {
        let toolkit = self.definition.selection().alias();
        let tool = self.definition.selection().tool();
        let node = self.name();
        let message = if refresh_failed {
            format!(
                "**Pipeline stopped** — authorization for **{toolkit}** (tool: `{tool}`, node: *{node}*) completed, but the authorized Toolkit was not available in this pipeline instance.\n\nDownstream nodes were not executed with an unavailable tool.\n\n> **Tip:** Run the pipeline again to load the authorized Toolkit."
            )
        } else {
            format!(
                "**Pipeline stopped** — authorization for **{toolkit}** (tool: `{tool}`, node: *{node}*) was skipped.\n\nDownstream nodes that depend on `{node}` output were not executed because the required toolkit is unavailable.\n\n> **Tip:** Authorize the toolkit and run the pipeline again."
            )
        };
        let mut output = NodeOutput::new()
            .with_update(DIRECT_TOOL_RESUME_STATE_KEY, remaining)
            .with_update("_pipeline_blocked", Value::String(message.clone()))
            .with_update(
                "messages",
                json!([{"role": "assistant", "content": message}]),
            )
            .with_goto([END]);
        for key in self
            .definition
            .output_keys()
            .iter()
            .filter(|key| key.as_str() != "messages")
        {
            output = output.with_update(key, Value::Null);
        }
        output
    }
}

enum McpAuthorizationDecision {
    Authorize(Value),
    Skip(Value),
}

enum McpAuthorizationResume {
    Authorize,
    Skip,
}

impl McpAuthorizationResume {
    fn parse(
        raw: &Value,
        definition_digest: &str,
        call_id: &str,
        argument_digest: &str,
    ) -> Result<Self, DirectToolExecutionError> {
        let object = raw
            .as_object()
            .ok_or(DirectToolExecutionError::InvalidArguments)?;
        if object.len() != 4
            || object.get("definition_digest").and_then(Value::as_str) != Some(definition_digest)
            || object.get("tool_call_id").and_then(Value::as_str) != Some(call_id)
            || object.get("argument_digest").and_then(Value::as_str) != Some(argument_digest)
        {
            return Err(DirectToolExecutionError::InvalidArguments);
        }
        match object.get("action").and_then(Value::as_str) {
            Some("authorize") => Ok(Self::Authorize),
            Some("skip") => Ok(Self::Skip),
            _ => Err(DirectToolExecutionError::InvalidArguments),
        }
    }
}

#[async_trait]
impl Node for DirectToolNode {
    fn name(&self) -> &str {
        self.definition.id()
    }

    #[allow(
        clippy::too_many_lines,
        reason = "Keep span instrumentation, pre-dispatch settlement, and failure projection together."
    )]
    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let span = tracing::info_span!(
            "agent.pipeline.direct_tool_node",
            node_id = self.name(),
            node_type = self.definition.selection().kind().wire_name(),
            toolkit_name = self.definition.selection().alias(),
            tool_name = self.definition.selection().tool(),
            structured_output = self.definition.structured_output(),
            stage = tracing::field::Empty,
            outcome = tracing::field::Empty,
            error_code = tracing::field::Empty,
        );
        let result = self.execute_mapped(context).instrument(span.clone()).await;
        match result {
            Ok(output) => {
                span.record("outcome", "completed");
                Ok(output)
            }
            Err(failure) if matches!(failure.cause, DirectNodeFailureCause::MayHaveStarted) => {
                span.record("outcome", "failed");
                span.record(
                    "error_code",
                    crate::protocol::output::RECOVERY_REFUSED_UPSTREAM_CODE,
                );
                // The registered "may have started, not repeated" stop, as an
                // approved saved-Agent call: a Parallel member's branch fails
                // typed, any other run ends with the registered reason.
                if let Some(events) = &self.events
                    && let Some(error) = events
                        .approved_call_stop(
                            crate::protocol::output::RECOVERY_REFUSED_UPSTREAM_CODE,
                            self.name(),
                        )
                        .await
                {
                    return Err(error);
                }
                Err(GraphError::NodeExecutionFailed {
                    node: self.name().to_owned(),
                    message: format!(
                        "{}: {}",
                        crate::agents::pipeline::scoped_runtime::CALL_MAY_HAVE_STARTED_CODE,
                        crate::protocol::output::RECOVERY_REFUSED_DETAIL
                    ),
                })
            }
            Err(DirectNodeFailure {
                cause: DirectNodeFailureCause::Graph(error),
                ..
            }) => {
                span.record("outcome", "failed");
                span.record("error_code", "pipeline.toolkit_node.failed");
                Err(*error)
            }
            Err(failure) => {
                if let Some(admitted) = &self.settings
                    && failure.can_settle_before_dispatch(admitted.settings.on_error)
                {
                    let class = failure.failure_class();
                    return settle_node_failure(
                        admitted,
                        self.execution_id.as_deref().map(String::as_str),
                        self.name(),
                        context,
                        direct_tool_failure_fault(admitted.family, class),
                        false,
                        1,
                    );
                }
                span.record("outcome", "failed");
                span.record("error_code", "pipeline.toolkit_node.failed");
                span.in_scope(|| {
                    tracing::error!(
                        stage = failure.stage,
                        failure_reason = %failure.cause,
                        upstream_error_code = failure.upstream_code.unwrap_or("none"),
                        "pipeline direct-tool node failed"
                    );
                });
                if let Some(events) = &self.events {
                    events
                        .send_execution_failure(failure.public_code())
                        .await
                        .map_err(|_| GraphError::NodeExecutionFailed {
                            node: self.name().to_owned(),
                            message: "The pipeline failure channel closed.".to_owned(),
                        })?;
                }
                let writer_unavailable = matches!(
                    failure.cause,
                    DirectNodeFailureCause::Policy(EFFECT_WRITER_UNAVAILABLE)
                );
                Err(GraphError::NodeExecutionFailed {
                    node: self.name().to_owned(),
                    message: if writer_unavailable {
                        format!(
                            "{EFFECT_WRITER_UNAVAILABLE_CODE}: {}: {}",
                            failure.stage, failure.cause
                        )
                    } else {
                        format!("{}: {}", failure.stage, failure.cause)
                    },
                })
            }
        }
    }
}

fn can_settle_direct_tool_stop(
    on_error: OnNodeError,
    reason: StopReason,
    class: NodeFailureClass,
) -> bool {
    on_error != OnNodeError::Stop
        && matches!(
            reason,
            StopReason::NotRetryable | StopReason::RetryDisabled | StopReason::AttemptsExhausted
        )
        && !class.control_stop()
        && !class.denial()
        && !class.deadline()
}

/// One journaled dispatch of a tool with external effects. Everything that can
/// pause, block or skip was decided before the Started append.
struct DirectToolAttempt {
    node: DirectToolNode,
    tool: Arc<dyn Tool>,
    tool_context: Arc<dyn ToolContext>,
    arguments: Value,
    remaining: Option<Value>,
    authorization_refresh: bool,
    control: Arc<Mutex<Option<Result<NodeOutput, DirectNodeFailure>>>>,
    // Set once the call returned a result, so a failed journal commit is not reported
    // as a failed tool.
    completed: Arc<AtomicBool>,
}

#[async_trait]
impl Node for DirectToolAttempt {
    fn name(&self) -> &str {
        self.node.name()
    }

    async fn execute(&self, _: &NodeContext) -> Result<NodeOutput, GraphError> {
        Err(GraphError::NodeExecutionFailed {
            node: self.name().to_owned(),
            message: "A journaled tool attempt has no direct execution.".to_owned(),
        })
    }
}

#[async_trait]
impl NodeAttemptBody for DirectToolAttempt {
    async fn execute_attempt(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
    ) -> Result<NodeOutput, NodeFailure> {
        self.execute_attempt_reported(context, authority)
            .await
            .map_err(|reported| reported.failure)
    }

    async fn execute_attempt_reported(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
    ) -> Result<NodeOutput, NodeAttemptReportedFailure> {
        let effect_id = authority.dispatch_activation();
        let unknown = ReplaySafety::UnknownExternalEffect { effect_id };
        if !authority.matches(
            self.name(),
            self.node.definition.config_digest(),
            context.step,
        ) {
            return Err(NodeFailure::new(
                NodeFailureClass::AuthorizationDenied,
                ReplaySafety::NoExternalEffect,
            )
            .into());
        }
        // A Started record without a committed result may already have reached the tool.
        if authority.recovering_started() {
            return Err(NodeFailure::new(NodeFailureClass::WorkerInterrupted, unknown).into());
        }
        let result = self
            .node
            .invoke_and_project(
                self.tool.as_ref(),
                Arc::clone(&self.tool_context),
                self.arguments.clone(),
                self.remaining.clone(),
                self.authorization_refresh,
            )
            .await;
        match result {
            Ok(output) if output.interrupt.is_some() || output.goto.is_some() => {
                *self
                    .control
                    .lock()
                    .unwrap_or_else(std::sync::PoisonError::into_inner) = Some(Ok(output));
                Err(NodeFailure::new(
                    NodeFailureClass::AuthenticationDenied,
                    ReplaySafety::NoExternalEffect,
                )
                .into())
            }
            Ok(output) => {
                self.completed.store(true, Ordering::SeqCst);
                Ok(output)
            }
            Err(failure) => {
                let is_timeout = matches!(failure.cause, DirectNodeFailureCause::Timeout);
                let is_pre_dispatch_deadline = matches!(
                    failure.cause,
                    DirectNodeFailureCause::DeadlineExceededBeforeDispatch
                );
                let is_in_flight_deadline = matches!(
                    failure.cause,
                    DirectNodeFailureCause::DeadlineExceededInFlight
                );
                let class = failure.failure_class();
                let recorded = if is_pre_dispatch_deadline {
                    NodeFailure::new(
                        NodeFailureClass::DeadlineInsufficient,
                        ReplaySafety::NoExternalEffect,
                    )
                } else if is_in_flight_deadline {
                    // SEC-5B2-01: tool.execute() already started; keep the
                    // effect uncertain so node recovery Reconciles with 0 repeats.
                    NodeFailure::new(NodeFailureClass::DeadlineInsufficient, unknown)
                } else {
                    match failure.stage {
                        "argument_digest" => NodeFailure::new(
                            NodeFailureClass::InvalidInput,
                            ReplaySafety::NoExternalEffect,
                        ),
                        "state_projection" => NodeFailure::new(
                            NodeFailureClass::InvalidResult,
                            ReplaySafety::CompletedExternalEffect {
                                receipt_id: effect_id,
                            },
                        ),
                        _ => NodeFailure::new(class, unknown),
                    }
                };
                // On a timeout, in-flight deadline expiry, or retryable failure
                // of an effectful tool under `settings:`, surface the Reconcile
                // recovery card (0 repeats) instead of swallowing it behind `control`.
                let escalate_reconcile = self.node.settings.is_some()
                    && matches!(recorded.replay, ReplaySafety::UnknownExternalEffect { .. })
                    && (is_timeout
                        || is_in_flight_deadline
                        || (self
                            .node
                            .settings
                            .as_ref()
                            .is_some_and(|s| s.settings.retry_on_fail)
                            && class.retry_condition().is_some()));
                if !escalate_reconcile {
                    // Preserve direct typed error handover when no recovery card is requested.
                    *self
                        .control
                        .lock()
                        .unwrap_or_else(std::sync::PoisonError::into_inner) = Some(Err(failure));
                }
                Err(NodeAttemptReportedFailure {
                    failure: recorded,
                    terminal_code: Some("pipeline.tool_failed"),
                    failure_detail: None,
                    retry_after_ms: None,
                })
            }
        }
    }

    async fn report_restored_terminal_failure(
        &self,
        _: &NodeContext,
        _: NodeFailureClass,
    ) -> Result<(), GraphError> {
        if let Some(events) = &self.node.events {
            events
                .send_execution_failure("pipeline.tool_failed")
                .await
                .map_err(|_| GraphError::NodeExecutionFailed {
                    node: self.name().to_owned(),
                    message: "The pipeline failure channel closed.".to_owned(),
                })?;
        }
        Ok(())
    }

    fn retry_deadline_budget(&self) -> Option<(u64, u64)> {
        self.node.retry_deadline_budget()
    }
}

/// Wire schema of an owner-attested direct-tool recovery receipt (`FU-067`).
pub(crate) const DIRECT_TOOL_RECOVERY_RECEIPT_SCHEMA: &str =
    "elitea.pipeline.direct-tool-recovery-receipt.v1";

#[derive(Clone, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub(crate) struct DirectToolReceiptBinding {
    pub(crate) execution_id: String,
    pub(crate) original_generation: u64,
    pub(crate) dispatch_activation: String,
    pub(crate) node_digest: String,
    pub(crate) toolkit_alias: String,
    pub(crate) tool_name: String,
    pub(crate) argument_digest: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub(crate) struct DirectToolReceiptVisit {
    pub(crate) activation_id: String,
    pub(crate) attempt: u16,
    pub(crate) expected_revision: u64,
    pub(crate) node_id: String,
    pub(crate) graph_thread: String,
    pub(crate) step: u64,
    pub(crate) receipt_sha256: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub(crate) enum DirectToolOwnerReceipt {
    CommittedResult {
        schema: String,
        binding: DirectToolReceiptBinding,
        visit: DirectToolReceiptVisit,
        result: Value,
    },
    VerifiedNoEffect {
        schema: String,
        binding: DirectToolReceiptBinding,
        visit: DirectToolReceiptVisit,
    },
}

impl DirectToolOwnerReceipt {
    fn parse(wire: &[u8]) -> Result<Self, ()> {
        if wire.is_empty() || wire.len() > MAX_RESULT_BYTES {
            return Err(());
        }
        let receipt: Self = serde_json::from_slice(wire).map_err(|_| ())?;
        if receipt.schema() != DIRECT_TOOL_RECOVERY_RECEIPT_SCHEMA {
            return Err(());
        }
        Ok(receipt)
    }

    fn schema(&self) -> &str {
        match self {
            Self::CommittedResult { schema, .. } | Self::VerifiedNoEffect { schema, .. } => schema,
        }
    }

    fn binding(&self) -> &DirectToolReceiptBinding {
        match self {
            Self::CommittedResult { binding, .. } | Self::VerifiedNoEffect { binding, .. } => {
                binding
            }
        }
    }

    fn visit(&self) -> &DirectToolReceiptVisit {
        match self {
            Self::CommittedResult { visit, .. } | Self::VerifiedNoEffect { visit, .. } => visit,
        }
    }
}

/// Read-only owning result projector for direct Toolkit and MCP recovery cards
/// (`FU-067`). Never holds or invokes a tool runtime client.
pub(crate) struct DirectToolCommittedProjector {
    definition: DirectToolNodeDefinition,
    state_types: BTreeMap<String, String>,
    execution_id: String,
    generation: u64,
}

impl DirectToolCommittedProjector {
    pub(crate) fn new(
        definition: DirectToolNodeDefinition,
        state_types: BTreeMap<String, String>,
        execution_id: String,
        generation: u64,
    ) -> Result<Self, GraphError> {
        if execution_id.len() != 32
            || !execution_id
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
            || !(1..=i64::MAX as u64).contains(&generation)
        {
            return Err(GraphError::NodeExecutionFailed {
                node: definition.id().to_owned(),
                message: "The original direct-tool execution identity is invalid.".to_owned(),
            });
        }
        Ok(Self {
            definition,
            state_types,
            execution_id,
            generation,
        })
    }

    fn validate_owner_receipt(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
        proof: &NodeRecoveryOwnerProof,
        wire: &[u8],
    ) -> Result<DirectToolOwnerReceipt, NodeFailure> {
        let reject = || {
            NodeFailure::new(
                NodeFailureClass::AuthorizationDenied,
                ReplaySafety::NoExternalEffect,
            )
        };
        if !authority.matches(
            self.definition.id(),
            self.definition.config_digest(),
            context.step,
        ) || proof.schema != "elitea.pipeline.node-recovery-owner-proof.v1"
            || proof.execution_id != self.execution_id
            || proof.generation != self.generation
            || proof.activation_id != hex(&authority.logical_activation())
            || proof.attempt != authority.attempt()
        {
            return Err(reject());
        }
        let receipt = DirectToolOwnerReceipt::parse(wire).map_err(|()| reject())?;
        let binding = receipt.binding();
        let visit = receipt.visit();
        let arguments = self
            .definition
            .map_arguments(&context.state)
            .map_err(|_| reject())?;
        let call_id = format!("pipeline:{}:{}", self.definition.id(), context.step);
        let expected_arguments =
            argument_digest(&call_id, self.definition.selection().tool(), &arguments)
                .map_err(|_| reject())?;
        if binding.execution_id != self.execution_id
            || binding.original_generation != self.generation
            || binding.dispatch_activation != hex(&authority.dispatch_activation())
            || binding.dispatch_activation != proof.effect_id
            || binding.node_digest != hex(&self.definition.config_digest())
            || binding.toolkit_alias != self.definition.selection().alias()
            || binding.tool_name != self.definition.selection().tool()
            || binding.argument_digest != expected_arguments
            || visit.activation_id != proof.activation_id
            || visit.attempt != proof.attempt
            || visit.expected_revision != proof.expected_revision
            || visit.node_id != self.definition.id()
            || visit.graph_thread != context.config.thread_id
            || visit.step != context.step as u64
            || authority
                .recovery_receipt_sha256()
                .is_none_or(|hash| visit.receipt_sha256 != hex(&hash))
        {
            return Err(reject());
        }
        let exact = match (&receipt, proof.kind) {
            (
                DirectToolOwnerReceipt::CommittedResult { .. },
                NodeRecoveryOwnerProofKind::CommittedResult,
            ) => {
                proof.owner_receipt_ref.is_none()
                    && proof.result_ref.as_ref().is_some_and(|r| {
                        r.required_grant_audience
                            == super::node_recovery_owner::DIRECT_TOOL_RECEIPT_AUDIENCE
                    })
                    && proof.matches_result_bytes(wire)
            }
            (
                DirectToolOwnerReceipt::VerifiedNoEffect { .. },
                NodeRecoveryOwnerProofKind::VerifiedNoEffect,
            ) => {
                proof.owner_receipt_ref.as_ref().is_some_and(|r| {
                    r.required_grant_audience
                        == super::node_recovery_owner::DIRECT_TOOL_RECEIPT_AUDIENCE
                }) && proof.matches_owner_receipt_bytes(wire)
            }
            _ => false,
        };
        if !exact {
            return Err(reject());
        }
        Ok(receipt)
    }
}

#[async_trait]
impl Node for DirectToolCommittedProjector {
    fn name(&self) -> &str {
        self.definition.id()
    }

    async fn execute(&self, _: &NodeContext) -> Result<NodeOutput, GraphError> {
        Err(GraphError::NodeExecutionFailed {
            node: self.definition.id().to_owned(),
            message: "An owner-result projector cannot execute a direct tool.".to_owned(),
        })
    }
}

impl NodeResultRecovery for DirectToolCommittedProjector {
    fn verify_no_effect_receipt(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
        proof: &NodeRecoveryOwnerProof,
        wire: &[u8],
    ) -> Result<(), NodeFailure> {
        let receipt = self.validate_owner_receipt(context, authority, proof, wire)?;
        if !matches!(receipt, DirectToolOwnerReceipt::VerifiedNoEffect { .. }) {
            return Err(NodeFailure::new(
                NodeFailureClass::AuthorizationDenied,
                ReplaySafety::NoExternalEffect,
            ));
        }
        Ok(())
    }

    fn project_committed_result(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
        proof: &NodeRecoveryOwnerProof,
        wire: &[u8],
    ) -> Result<NodeOutput, NodeFailure> {
        let receipt = self.validate_owner_receipt(context, authority, proof, wire)?;
        let DirectToolOwnerReceipt::CommittedResult { result, .. } = receipt else {
            return Err(NodeFailure::new(
                NodeFailureClass::AuthorizationDenied,
                ReplaySafety::NoExternalEffect,
            ));
        };
        let updates = self
            .definition
            .project_result(result, &self.state_types)
            .map_err(|_| {
                NodeFailure::new(
                    NodeFailureClass::InvalidResult,
                    ReplaySafety::CompletedExternalEffect {
                        receipt_id: authority.dispatch_activation(),
                    },
                )
            })?;
        let mut output = NodeOutput::new();
        for (key, value) in updates {
            output = output.with_update(&key, value);
        }
        Ok(output)
    }
}

enum SensitiveDirectToolDecision {
    Pause(Value),
    Approve(Value),
    Block {
        remaining: Value,
        result: Value,
        message: String,
    },
}

impl DirectToolNode {
    fn sensitive_decision(
        &self,
        context: &NodeContext,
        arguments: &Value,
        call_id: &str,
        policy: &SensitiveToolPolicy,
    ) -> Result<SensitiveDirectToolDecision, DirectToolExecutionError> {
        if [
            self.name(),
            self.definition.selection().tool(),
            policy.toolkit_name(),
            policy.toolkit_type(),
            policy.action_name(),
        ]
        .into_iter()
        .any(|value| {
            value.is_empty()
                || value.len() > MAX_CONFIRMATION_IDENTITY_BYTES
                || value.chars().any(char::is_control)
        }) || policy.policy_message().is_empty()
            || policy.policy_message().len() > MAX_CONFIRMATION_MESSAGE_BYTES
            || policy.policy_message().chars().any(|character| {
                character == '\0'
                    || (character.is_control() && !matches!(character, '\n' | '\r' | '\t'))
            })
        {
            return Err(DirectToolExecutionError::ResourceExhausted);
        }
        // Confirmation carries arguments in a browser event; ordinary calls do not.
        if serde_json::to_vec(arguments)
            .map_err(|_| DirectToolExecutionError::InvalidArguments)?
            .len()
            > MAX_CONFIRMATION_ARGUMENT_BYTES
        {
            return Err(DirectToolExecutionError::ResourceExhausted);
        }
        let argument_digest =
            argument_digest(call_id, self.definition.selection().tool(), arguments)?;
        let definition_digest = self.definition.digest_label();
        let resumes = context
            .state
            .get(DIRECT_TOOL_RESUME_STATE_KEY)
            .map(|value| {
                value
                    .as_object()
                    .ok_or(DirectToolExecutionError::InvalidArguments)
            })
            .transpose()?;
        let raw = resumes.and_then(|resumes| resumes.get(self.name()));
        let Some(raw) = raw else {
            let masked = mask_sensitive_arguments(arguments, 0)
                .map_err(|_| DirectToolExecutionError::ResourceExhausted)?;
            return Ok(SensitiveDirectToolDecision::Pause(
                super::llm::with_policy_approval_rule(
                    json!({
                        "schema_revision": TOOL_CONFIRMATION_SCHEMA,
                        "type": "hitl",
                        "guardrail_type": "sensitive_tool",
                        "node_name": self.name(),
                        "message": policy.policy_message(),
                        "available_actions": ["approve", "reject", "block_with_comment"],
                        "routes": {},
                        "definition_digest": definition_digest,
                        "tool_call_id": call_id,
                        "tool_name": self.definition.selection().tool(),
                        "toolkit_name": policy.toolkit_name(),
                        "toolkit_type": policy.toolkit_type(),
                        "action_label": policy.action_name(),
                        "tool_args": masked,
                        "argument_digest": argument_digest,
                        "policy_message": policy.policy_message(),
                    }),
                    policy.rule(),
                ),
            ));
        };
        let resumes = resumes.ok_or(DirectToolExecutionError::InvalidArguments)?;
        let decision =
            SensitiveResumeDecision::parse(raw, &definition_digest, call_id, &argument_digest)?;
        let mut remaining = resumes.clone();
        if remaining.remove(self.name()).is_none() {
            return Err(DirectToolExecutionError::InvalidArguments);
        }
        let remaining = Value::Object(remaining);
        match decision.action {
            SensitiveResumeAction::Approve => Ok(SensitiveDirectToolDecision::Approve(remaining)),
            SensitiveResumeAction::Reject | SensitiveResumeAction::BlockWithComment => {
                let reason = decision
                    .value
                    .as_deref()
                    .unwrap_or("This exact sensitive tool call was declined and was not executed.");
                let message = blocked_pipeline_message(
                    self.name(),
                    self.definition.selection().tool(),
                    policy.toolkit_type(),
                );
                let result =
                    blocked_result(self.definition.selection().tool(), policy, reason, &message);
                Ok(SensitiveDirectToolDecision::Block {
                    remaining,
                    result,
                    message,
                })
            }
        }
    }
}

#[derive(Clone, Copy, Eq, PartialEq)]
enum SensitiveResumeAction {
    Approve,
    Reject,
    BlockWithComment,
}

struct SensitiveResumeDecision {
    action: SensitiveResumeAction,
    value: Option<String>,
}

impl SensitiveResumeDecision {
    fn parse(
        raw: &Value,
        definition_digest: &str,
        call_id: &str,
        argument_digest: &str,
    ) -> Result<Self, DirectToolExecutionError> {
        let object = raw
            .as_object()
            .ok_or(DirectToolExecutionError::InvalidArguments)?;
        if object.len() > 5
            || object.keys().any(|key| {
                !matches!(
                    key.as_str(),
                    "definition_digest" | "tool_call_id" | "argument_digest" | "action" | "value"
                )
            })
            || object.get("definition_digest").and_then(Value::as_str) != Some(definition_digest)
            || object.get("tool_call_id").and_then(Value::as_str) != Some(call_id)
            || object.get("argument_digest").and_then(Value::as_str) != Some(argument_digest)
        {
            return Err(DirectToolExecutionError::InvalidArguments);
        }
        let action = match object.get("action").and_then(Value::as_str) {
            Some("approve") => SensitiveResumeAction::Approve,
            Some("reject") => SensitiveResumeAction::Reject,
            Some("block_with_comment") => SensitiveResumeAction::BlockWithComment,
            _ => return Err(DirectToolExecutionError::InvalidArguments),
        };
        let value = object
            .get("value")
            .and_then(Value::as_str)
            .filter(|value| !value.is_empty())
            .map(ToOwned::to_owned);
        if (action == SensitiveResumeAction::BlockWithComment && value.is_none())
            || (action != SensitiveResumeAction::BlockWithComment && value.is_some())
        {
            return Err(DirectToolExecutionError::InvalidArguments);
        }
        if value.as_ref().is_some_and(|value| {
            value.len() > 8 * 1024
                || value.chars().any(|character| {
                    character == '\0'
                        || (character.is_control() && !matches!(character, '\n' | '\r' | '\t'))
                })
        }) {
            return Err(DirectToolExecutionError::ResourceExhausted);
        }
        Ok(Self { action, value })
    }
}

impl DirectToolNodeDefinition {
    fn digest_label(&self) -> String {
        format!("sha256:{}", hex(&self.config_digest()))
    }
}

fn argument_digest(
    call_id: &str,
    tool_name: &str,
    arguments: &Value,
) -> Result<String, DirectToolExecutionError> {
    let canonical = canonical_value(arguments, 0)?;
    let encoded =
        serde_json::to_vec(&canonical).map_err(|_| DirectToolExecutionError::InvalidArguments)?;
    if encoded.len() > MAX_RESULT_BYTES {
        return Err(DirectToolExecutionError::ResourceExhausted);
    }
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(ARGUMENT_DIGEST_DOMAIN);
    digest_field(&mut context, call_id.as_bytes());
    digest_field(&mut context, tool_name.as_bytes());
    digest_field(&mut context, &encoded);
    Ok(format!("sha256:{}", hex(context.finish().as_ref())))
}

fn canonical_value(value: &Value, depth: usize) -> Result<Value, DirectToolExecutionError> {
    if depth > 64 {
        return Err(DirectToolExecutionError::ResourceExhausted);
    }
    match value {
        Value::Array(values) => values
            .iter()
            .map(|value| canonical_value(value, depth + 1))
            .collect::<Result<Vec<_>, _>>()
            .map(Value::Array),
        Value::Object(values) => {
            let mut keys = values.keys().collect::<Vec<_>>();
            keys.sort_unstable();
            let mut canonical = Map::with_capacity(values.len());
            for key in keys {
                canonical.insert(
                    key.clone(),
                    canonical_value(
                        values
                            .get(key)
                            .ok_or(DirectToolExecutionError::InvalidArguments)?,
                        depth + 1,
                    )?,
                );
            }
            Ok(Value::Object(canonical))
        }
        value => Ok(value.clone()),
    }
}

fn blocked_result(
    tool_name: &str,
    policy: &SensitiveToolPolicy,
    reason: &str,
    message: &str,
) -> Value {
    json!({
        "type": "sensitive_tool_blocked",
        "blocked_tool_name": tool_name,
        "blocked_toolkit_name": policy.toolkit_name(),
        "blocked_toolkit_type": policy.toolkit_type(),
        "denial_reason": reason,
        "message": message,
    })
}

fn blocked_pipeline_message(node_name: &str, tool_name: &str, toolkit_type: &str) -> String {
    let details = if toolkit_type.is_empty() {
        format!("node: *{node_name}*")
    } else {
        format!("toolkit type: *{toolkit_type}*, node: *{node_name}*")
    };
    format!(
        "**Pipeline stopped** — the action **{tool_name}** ({details}) was **blocked** by user.\n\nDownstream nodes that depend on `{tool_name}` output were skipped to prevent invalid data.\n\n> **Tip:** Regenerate this message to re-trigger the approval request and try again."
    )
}

fn blocked_messages(call_id: &str, tool_name: &str, result: &Value, message: &str) -> Value {
    json!([
        {
            "role": "tool",
            "tool_call_id": call_id,
            "name": tool_name,
            "content": result,
        },
        {"role": "assistant", "content": message},
    ])
}

pub(super) fn pipeline_tool_context(
    context: &NodeContext,
    node_name: &str,
    tool_name: &str,
) -> Arc<dyn ToolContext> {
    // The graph step is checkpointed by ADK. Including it makes this identity
    // stable across resume while keeping two visits to a looped node distinct.
    let call_id = format!("pipeline:{node_name}:{}", context.step);
    match context.config.parent_context.clone() {
        Some(parent) => Arc::new(PipelineToolContext::new(parent, call_id, tool_name)),
        None => Arc::new(
            SimpleToolContext::new(node_name)
                .with_function_call_id(call_id)
                .with_session_id(&context.config.thread_id),
        ),
    }
}

/// How a scoped Agent call runs: an ordinary pipeline Agent node, a fixed
/// Parallel child, or a fixed Parallel child an earlier claim started (D2:
/// it resumes from its own model checkpoint).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ScopedCallKind {
    Node,
    FanoutChild,
    FanoutChildRecovery,
    /// FU-323a: a root Agent node re-entering the started call its claim's
    /// inspect step proved (`scoped_runtime.rs` `RootAgentReentry`).
    RootAgentRecovery,
}

pub(crate) fn scoped_pipeline_tool_context(
    context: &NodeContext,
    call_id: &str,
    tool_name: &str,
    invocation_id: &str,
    agent_name: &str,
    branch: &str,
    kind: ScopedCallKind,
) -> Result<Arc<dyn ToolContext>, GraphError> {
    let parent =
        context
            .config
            .parent_context
            .clone()
            .ok_or_else(|| GraphError::NodeExecutionFailed {
                node: "application_scope".to_owned(),
                message: "the scoped application has no owning invocation context".to_owned(),
            })?;
    let mut ctx = PipelineToolContext::new(parent, call_id.to_owned(), tool_name);
    if kind != ScopedCallKind::Node
        && let Some(marker) = ctx
            .actions
            .get_mut()
            .map_err(|_| GraphError::NodeExecutionFailed {
                node: "application_scope".to_owned(),
                message: "the scoped application call marker is unavailable".to_owned(),
            })?
            .state_delta
            .get_mut(PIPELINE_APPLICATION_NODE_METADATA_KEY)
            .and_then(Value::as_object_mut)
    {
        if kind == ScopedCallKind::RootAgentRecovery {
            marker.insert("root_recovery".to_owned(), Value::Bool(true));
        } else {
            marker.insert("fanout".to_owned(), Value::Bool(true));
        }
        if kind == ScopedCallKind::FanoutChildRecovery {
            marker.insert("recover_started".to_owned(), Value::Bool(true));
        }
    }
    ctx.scope = Some(PipelineToolScope {
        invocation_id: invocation_id.to_owned(),
        agent_name: agent_name.to_owned(),
        thread_id: context.config.thread_id.clone(),
        branch: branch.to_owned(),
    });
    Ok(Arc::new(ctx))
}

struct PipelineToolScope {
    invocation_id: String,
    agent_name: String,
    thread_id: String,
    branch: String,
}

struct PipelineToolContext {
    scope: Option<PipelineToolScope>,
    parent: Arc<dyn InvocationContext>,
    function_call_id: String,
    tool_name: String,
    actions: Mutex<EventActions>,
}

impl PipelineToolContext {
    fn new(parent: Arc<dyn InvocationContext>, function_call_id: String, tool_name: &str) -> Self {
        let mut actions = EventActions::default();
        actions.state_delta.insert(
            PIPELINE_APPLICATION_NODE_METADATA_KEY.to_owned(),
            json!({
                "call_id": function_call_id,
                "tool_name": tool_name,
            }),
        );
        Self {
            scope: None,
            parent,
            function_call_id,
            tool_name: tool_name.to_owned(),
            actions: Mutex::new(actions),
        }
    }

    async fn request_secret(
        &self,
        name: &str,
        purpose: Option<&str>,
    ) -> adk_rust::Result<Option<String>> {
        let mut request = SecretRequest::new(name)
            .with_identity(
                self.parent.app_name(),
                self.parent.user_id(),
                self.parent.session_id(),
            )
            .with_invocation_id(self.parent.invocation_id())
            .with_tool_name(&self.tool_name);
        if let Some(purpose) = purpose {
            request = request.with_purpose(purpose);
        }
        self.parent.get_secret_for(&request).await
    }
}

impl ReadonlyContext for PipelineToolContext {
    fn invocation_id(&self) -> &str {
        self.scope.as_ref().map_or_else(
            || self.parent.invocation_id(),
            |scope| scope.invocation_id.as_str(),
        )
    }

    fn agent_name(&self) -> &str {
        self.scope.as_ref().map_or_else(
            || self.parent.agent_name(),
            |scope| scope.agent_name.as_str(),
        )
    }

    fn user_id(&self) -> &str {
        self.parent.user_id()
    }

    fn app_name(&self) -> &str {
        self.parent.app_name()
    }

    fn session_id(&self) -> &str {
        self.scope.as_ref().map_or_else(
            || self.parent.session_id(),
            |scope| scope.thread_id.as_str(),
        )
    }

    fn branch(&self) -> &str {
        self.scope
            .as_ref()
            .map_or_else(|| self.parent.branch(), |scope| scope.branch.as_str())
    }

    fn user_content(&self) -> &Content {
        self.parent.user_content()
    }
}

#[async_trait]
impl CallbackContext for PipelineToolContext {
    fn artifacts(&self) -> Option<Arc<dyn adk_rust::Artifacts>> {
        self.parent.artifacts()
    }

    fn shared_state(&self) -> Option<Arc<adk_rust::SharedState>> {
        self.parent.shared_state()
    }
}

#[async_trait]
impl ToolContext for PipelineToolContext {
    fn function_call_id(&self) -> &str {
        &self.function_call_id
    }

    fn actions(&self) -> EventActions {
        self.actions
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .clone()
    }

    fn set_actions(&self, actions: EventActions) {
        *self
            .actions
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = actions;
    }

    async fn search_memory(&self, query: &str) -> adk_rust::Result<Vec<MemoryEntry>> {
        match self.parent.memory() {
            Some(memory) => memory.search(query).await,
            None => Ok(Vec::new()),
        }
    }

    fn user_scopes(&self) -> Vec<String> {
        self.parent.user_scopes()
    }

    async fn get_secret(&self, name: &str) -> adk_rust::Result<Option<String>> {
        self.request_secret(name, None).await
    }

    async fn get_secret_for_purpose(
        &self,
        name: &str,
        purpose: &str,
    ) -> adk_rust::Result<Option<String>> {
        self.request_secret(name, Some(purpose)).await
    }
}

// Omitted mappings retain legacy message input. Explicit empty maps send no arguments.
fn default_input_mapping() -> BTreeMap<String, RawInputMapping> {
    BTreeMap::from([(
        "messages".to_owned(),
        RawInputMapping {
            kind: "variable".to_owned(),
            value: Value::String("messages".to_owned()),
        },
    )])
}

fn validate_input_mapping(
    raw: BTreeMap<String, RawInputMapping>,
) -> Result<BTreeMap<String, DirectToolInputMapping>, DirectToolConfigurationError> {
    let mut mapping = BTreeMap::new();
    for (key, value) in raw {
        if !valid_tool_identity(&key) {
            return Err(DirectToolConfigurationError::Invalid(
                "a Toolkit argument name is malformed",
            ));
        }
        let admitted = match value.kind.as_str() {
            "fixed" => {
                ensure_bounded_mapping_value(&value.value)?;
                DirectToolInputMapping::Fixed(value.value)
            }
            "variable" => {
                let key = bounded_mapping_text(&value.value)?;
                if !valid_output_key(key) {
                    return Err(DirectToolConfigurationError::Invalid(
                        "a Toolkit variable mapping is malformed",
                    ));
                }
                DirectToolInputMapping::Variable(key.to_owned())
            }
            "fstring" => {
                DirectToolInputMapping::Template(bounded_mapping_text(&value.value)?.to_owned())
            }
            _ => {
                return Err(DirectToolConfigurationError::Unsupported(
                    "the direct-tool input mapping type is not supported",
                ));
            }
        };
        mapping.insert(key, admitted);
    }
    Ok(mapping)
}

fn validate_unique(values: &[String]) -> Result<(), DirectToolConfigurationError> {
    let mut seen = BTreeSet::new();
    if values.iter().any(|value| !seen.insert(value.as_str())) {
        return Err(DirectToolConfigurationError::Invalid(
            "direct-tool node variables must be unique within each field",
        ));
    }
    Ok(())
}

fn valid_tool_identity(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_TOOL_IDENTITY_BYTES
        && !value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n'))
}

fn bounded_mapping_text(value: &Value) -> Result<&str, DirectToolConfigurationError> {
    value
        .as_str()
        .filter(|value| value.len() <= MAX_MAPPING_VALUE_BYTES && !value.contains('\0'))
        .ok_or(DirectToolConfigurationError::Invalid(
            "a direct-tool input mapping value is malformed",
        ))
}

fn ensure_bounded_mapping_value(value: &Value) -> Result<(), DirectToolConfigurationError> {
    let encoded = serde_json::to_vec(value).map_err(|_| {
        DirectToolConfigurationError::Invalid("a Toolkit fixed input mapping value is malformed")
    })?;
    if encoded.len() > MAX_MAPPING_VALUE_BYTES {
        return Err(DirectToolConfigurationError::ResourceExhausted);
    }
    Ok(())
}

fn render_fstring(template: &str, state: &State) -> Result<String, DirectToolExecutionError> {
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
                let value = match value {
                    Value::String(value) => value.clone(),
                    value => serde_json::to_string(value)
                        .map_err(|_| DirectToolExecutionError::InvalidArguments)?,
                };
                rendered.push_str(&value);
            } else {
                rendered.push_str(&template[open..=end]);
            }
            cursor = end + 1;
        } else {
            rendered.push('{');
            cursor = open + 1;
        }
        if rendered.len() > MAX_MAPPING_VALUE_BYTES {
            return Err(DirectToolExecutionError::ResourceExhausted);
        }
    }
    if cursor < template.len() {
        rendered.push_str(&template[cursor..]);
    }
    if rendered.len() > MAX_MAPPING_VALUE_BYTES {
        return Err(DirectToolExecutionError::ResourceExhausted);
    }
    Ok(rendered)
}

fn projected_messages(result: &Value) -> Result<Value, DirectToolExecutionError> {
    if let Some(messages) = result.as_object().and_then(|object| object.get("messages")) {
        if !messages.is_array() {
            return Err(DirectToolExecutionError::InvalidResult);
        }
        ensure_bounded_value(messages)?;
        return Ok(messages.clone());
    }
    assistant_message(result)
}

fn assistant_message(result: &Value) -> Result<Value, DirectToolExecutionError> {
    let content = match result {
        Value::String(value) => value.clone(),
        value => {
            serde_json::to_string(value).map_err(|_| DirectToolExecutionError::InvalidResult)?
        }
    };
    if content.len() > MAX_RESULT_BYTES {
        return Err(DirectToolExecutionError::ResourceExhausted);
    }
    Ok(json!([{"role": "assistant", "content": content}]))
}

pub(super) fn ensure_state_type(
    key: &str,
    value: &Value,
    state_types: &BTreeMap<String, String>,
) -> Result<(), DirectToolExecutionError> {
    let valid = match state_types
        .get(key)
        .map(String::as_str)
        .or_else(|| builtin_state_type(key))
    {
        Some("str") => value.is_string(),
        Some("int") => value.as_i64().is_some() || value.as_u64().is_some(),
        Some("float") => value.as_f64().is_some(),
        Some("bool") => value.is_boolean(),
        Some("list") => value.is_array(),
        Some("dict") => value.is_object(),
        _ => false,
    };
    if valid {
        Ok(())
    } else {
        Err(DirectToolExecutionError::InvalidResult)
    }
}

fn builtin_state_type(key: &str) -> Option<&'static str> {
    match key {
        "input" | "output" | "result" | "router_output" | "elitea_response" | "printer_output"
        | "session_id" => Some("str"),
        "messages" | "hitl_decisions" => Some("list"),
        "context_info" | "parallel_tasks" => Some("dict"),
        _ => None,
    }
}

fn ensure_bounded_value(value: &Value) -> Result<(), DirectToolExecutionError> {
    let encoded = serde_json::to_vec(value).map_err(|_| DirectToolExecutionError::InvalidResult)?;
    if encoded.len() > MAX_RESULT_BYTES {
        return Err(DirectToolExecutionError::ResourceExhausted);
    }
    Ok(())
}

/// A tool with external effects found no fenced node writer (a saved child
/// pipeline has none yet, FU-248). It leads the node's failure message so a
/// fixed Parallel names it on the failed branch.
pub(crate) const EFFECT_WRITER_UNAVAILABLE_CODE: &str = "pipeline.effect_writer_unavailable";
const EFFECT_WRITER_UNAVAILABLE: &str =
    "A tool with external effects requires its current fenced node writer.";

/// Static diagnostic context. Never retains tool arguments, results, or provider messages.
struct DirectNodeFailure {
    stage: &'static str,
    cause: DirectNodeFailureCause,
    upstream_code: Option<&'static str>,
    category: Option<ErrorCategory>,
    retry_after_ms: Option<u64>,
}

#[derive(Debug, Error)]
enum DirectNodeFailureCause {
    #[error("{0}")]
    Execution(DirectToolExecutionError),
    #[error("{0}")]
    Policy(&'static str),
    #[error("The tool call failed. Inspect its error code before repeating actions.")]
    Tool,
    #[error("The tool call exceeded its execution timeout.")]
    Timeout,
    #[error("The execution deadline expired before the tool call started.")]
    DeadlineExceededBeforeDispatch,
    #[error("The execution deadline expired while the tool call was in flight.")]
    DeadlineExceededInFlight,
    /// Structured `GraphError` propagated verbatim (e.g. `pipeline.node_errors.exhausted`,
    /// `pipeline.node_settings.invalid`, `pipeline.node_recovery.cancelled`).
    #[error("{0}")]
    Graph(Box<GraphError>),
    /// A journaled call in a saved child started before the Worker was lost
    /// and may have reached its tool (FU-248).
    #[error("The tool call may already have run; it was not repeated.")]
    MayHaveStarted,
}

impl DirectNodeFailure {
    fn public_code(&self) -> &'static str {
        match (&self.cause, self.stage) {
            (
                DirectNodeFailureCause::Tool
                | DirectNodeFailureCause::Timeout
                | DirectNodeFailureCause::DeadlineExceededBeforeDispatch
                | DirectNodeFailureCause::DeadlineExceededInFlight
                | DirectNodeFailureCause::Graph(_),
                _,
            )
            | (DirectNodeFailureCause::Policy(_), "tool_execution") => "pipeline.tool_failed",
            (_, "tool_binding" | "authorization") => "pipeline.tool_unavailable",
            (
                DirectNodeFailureCause::Execution(DirectToolExecutionError::ResourceExhausted),
                "state_projection",
            ) => "pipeline.result_limit",
            (_, "state_projection") => "pipeline.result_invalid",
            (DirectNodeFailureCause::Execution(DirectToolExecutionError::ResourceExhausted), _) => {
                "pipeline.input_limit"
            }
            _ => "pipeline.input_invalid",
        }
    }

    fn failure_class(&self) -> NodeFailureClass {
        match &self.cause {
            DirectNodeFailureCause::DeadlineExceededBeforeDispatch
            | DirectNodeFailureCause::DeadlineExceededInFlight => {
                NodeFailureClass::DeadlineInsufficient
            }
            DirectNodeFailureCause::Timeout => NodeFailureClass::AttemptTimeout,
            DirectNodeFailureCause::MayHaveStarted => NodeFailureClass::WorkerInterrupted,
            DirectNodeFailureCause::Policy(_) if self.stage == "authorization" => {
                NodeFailureClass::AuthorizationDenied
            }
            DirectNodeFailureCause::Graph(_) | DirectNodeFailureCause::Policy(_) => {
                NodeFailureClass::Unknown
            }
            DirectNodeFailureCause::Execution(DirectToolExecutionError::InvalidArguments) => {
                NodeFailureClass::InvalidInput
            }
            DirectNodeFailureCause::Execution(
                DirectToolExecutionError::InvalidResult
                | DirectToolExecutionError::ResourceExhausted,
            ) => NodeFailureClass::InvalidResult,
            DirectNodeFailureCause::Execution(DirectToolExecutionError::Unavailable) => {
                NodeFailureClass::InvalidConfiguration
            }
            DirectNodeFailureCause::Tool => match self.category {
                Some(ErrorCategory::RateLimited) => NodeFailureClass::RateLimited,
                Some(ErrorCategory::Timeout) => NodeFailureClass::AttemptTimeout,
                Some(ErrorCategory::Unavailable) => NodeFailureClass::DependencyUnavailable,
                Some(ErrorCategory::Unauthorized) => NodeFailureClass::AuthenticationDenied,
                Some(ErrorCategory::Forbidden) => NodeFailureClass::AuthorizationDenied,
                Some(ErrorCategory::Cancelled) => NodeFailureClass::Cancelled,
                Some(ErrorCategory::InvalidInput) => NodeFailureClass::InvalidInput,
                Some(ErrorCategory::NotFound | ErrorCategory::Unsupported) => {
                    NodeFailureClass::InvalidConfiguration
                }
                Some(ErrorCategory::Internal) | None => NodeFailureClass::Unknown,
            },
        }
    }

    /// Pre-dispatch failures (`input_mapping` or non-effect-writer `tool_binding`)
    /// may be settled by `on_error` when no external effect was attempted.
    fn can_settle_before_dispatch(&self, on_error: OnNodeError) -> bool {
        if on_error == OnNodeError::Stop {
            return false;
        }
        matches!(self.stage, "input_mapping" | "tool_binding")
            && matches!(self.cause, DirectNodeFailureCause::Execution(_))
    }

    const fn new(stage: &'static str, cause: DirectToolExecutionError) -> Self {
        Self {
            stage,
            cause: DirectNodeFailureCause::Execution(cause),
            upstream_code: None,
            category: None,
            retry_after_ms: None,
        }
    }

    const fn policy(stage: &'static str, message: &'static str) -> Self {
        Self {
            stage,
            cause: DirectNodeFailureCause::Policy(message),
            upstream_code: None,
            category: None,
            retry_after_ms: None,
        }
    }

    const fn may_have_started() -> Self {
        Self {
            stage: "tool_execution",
            cause: DirectNodeFailureCause::MayHaveStarted,
            upstream_code: None,
            category: None,
            retry_after_ms: None,
        }
    }

    const fn timeout() -> Self {
        Self {
            stage: "tool_execution",
            cause: DirectNodeFailureCause::Timeout,
            upstream_code: Some("tool.timeout"),
            category: Some(ErrorCategory::Timeout),
            retry_after_ms: None,
        }
    }

    const fn deadline_exceeded_before_dispatch() -> Self {
        Self {
            stage: "tool_execution",
            cause: DirectNodeFailureCause::DeadlineExceededBeforeDispatch,
            upstream_code: Some("tool.deadline_exceeded"),
            category: Some(ErrorCategory::Timeout),
            retry_after_ms: None,
        }
    }

    const fn deadline_exceeded_in_flight() -> Self {
        Self {
            stage: "tool_execution",
            cause: DirectNodeFailureCause::DeadlineExceededInFlight,
            upstream_code: Some("tool.deadline_exceeded"),
            category: Some(ErrorCategory::Timeout),
            retry_after_ms: None,
        }
    }

    fn graph(error: GraphError) -> Self {
        Self {
            stage: "tool_execution",
            cause: DirectNodeFailureCause::Graph(Box::new(error)),
            upstream_code: None,
            category: None,
            retry_after_ms: None,
        }
    }

    fn tool_error(error: &adk_rust::AdkError) -> Self {
        Self {
            stage: "tool_execution",
            cause: DirectNodeFailureCause::Tool,
            upstream_code: Some(error.code),
            category: Some(error.category),
            retry_after_ms: error.retry.retry_after_ms,
        }
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
pub(crate) enum DirectToolConfigurationError {
    #[error("the direct-tool node YAML is malformed")]
    MalformedYaml {
        #[source]
        source: serde_yaml_ng::Error,
    },
    #[error("{0}")]
    Invalid(&'static str),
    #[error("{0}")]
    Unsupported(&'static str),
    #[error("the direct-tool node exceeds its resource bound")]
    ResourceExhausted,
}

#[derive(Clone, Copy, Debug, Eq, Error, PartialEq)]
pub(crate) enum DirectToolExecutionError {
    #[error("the direct-tool node arguments are invalid")]
    InvalidArguments,
    #[error("the direct-tool node result is invalid")]
    InvalidResult,
    #[error("the direct-tool node exceeds its resource bound")]
    ResourceExhausted,
    #[error("the direct-tool node runtime is unavailable")]
    Unavailable,
}

/// FU-250: a delegated sign-in pause names its frozen toolkit, so its fan-out
/// card can carry `display.toolkit_id` and be authorized with a credential
/// bound to that toolkit. Written only when the toolkit is known, and only by
/// this Worker: a pause stored earlier has no `toolkit_id`, so its card
/// re-raises byte-identical and stays skip-only.
pub(crate) fn with_pause_toolkit_id(
    data: &mut Value,
    requirement: &DelegatedAuthorizationRequirement,
) {
    if let (Some(id), Some(object)) = (requirement.toolkit_id(), data.as_object_mut()) {
        object.insert("toolkit_id".to_owned(), Value::from(id));
    }
}
