//! Native direct Toolkit pipeline node.
//!
//! The stored YAML selects one concrete action from one frozen toolkit alias.
//! Assembly resolves that pair after claim-authorized materialization; the graph
//! node then maps state into JSON arguments and invokes the native ADK [`Tool`]
//! exactly once. It never creates a second agent/model turn.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::{Arc, Mutex};

use adk_rust::graph::{END, GraphError, Node, NodeContext, NodeOutput, State};
use adk_rust::tool::SimpleToolContext;
use adk_rust::{
    CallbackContext, Content, EventActions, InvocationContext, MemoryEntry, ReadonlyContext,
    SecretRequest, Tool, ToolContext,
};
use async_trait::async_trait;
use ring::digest;
use serde::Deserialize;
use serde_json::{Map, Value, json};
use thiserror::Error;
use tracing::Instrument as _;

use super::yaml::{valid_graph_id, valid_output_key};
use crate::agents::application_tools::PIPELINE_APPLICATION_NODE_METADATA_KEY;
use crate::agents::events::mask_sensitive_arguments;
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
    #[serde(default)]
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
    pub(super) fn from_yaml(yaml: &str) -> Result<Self, DirectToolConfigurationError> {
        if yaml.is_empty() || yaml.len() > MAX_NODE_YAML_BYTES {
            return Err(DirectToolConfigurationError::ResourceExhausted);
        }
        let raw = serde_yaml_ng::from_str::<RawDirectToolNodeDefinition>(yaml)
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
pub(super) struct DirectToolNode {
    definition: DirectToolNodeDefinition,
    state_types: BTreeMap<String, String>,
    resolver: Arc<dyn PipelineDirectToolResolver>,
}

impl DirectToolNode {
    pub(super) fn new(
        definition: DirectToolNodeDefinition,
        state_types: BTreeMap<String, String>,
        resolver: Arc<dyn PipelineDirectToolResolver>,
    ) -> Self {
        Self {
            definition,
            state_types,
            resolver,
        }
    }

    async fn execute_mapped(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        tracing::Span::current().record("stage", "input_mapping");
        let arguments = self
            .definition
            .map_arguments(&context.state)
            .map_err(|_| node_failure(self.name()))?;
        tracing::Span::current().record("stage", "tool_binding");
        let ResolvedDirectTool { tool, sensitive } = self
            .resolver
            .resolve(self.definition.selection())
            .map_err(|_| node_failure(self.name()))?;
        if !tool.is_read_only() {
            return Err(node_failure(self.name()));
        }
        let tool_context = pipeline_tool_context(context, self.name(), tool.name());
        let granted_scopes = tool_context.user_scopes();
        if tool
            .required_scopes()
            .iter()
            .any(|required| !granted_scopes.iter().any(|granted| granted == required))
        {
            return Err(node_failure(self.name()));
        }
        if self.has_mcp_authorization_resume(context)
            && let Some(decision) = self
                .mcp_authorization_decision(context, &arguments, tool_context.function_call_id())
                .map_err(|_| node_failure(self.name()))?
        {
            return match decision {
                McpAuthorizationDecision::Authorize(remaining) => {
                    self.invoke_and_project(
                        tool.as_ref(),
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
                .execute_sensitive(context, tool.as_ref(), tool_context, arguments, policy)
                .await;
        }
        if context
            .state
            .get(DIRECT_TOOL_RESUME_STATE_KEY)
            .and_then(Value::as_object)
            .is_some_and(|resumes| resumes.contains_key(self.name()))
        {
            return Err(node_failure(self.name()));
        }
        self.invoke_and_project(tool.as_ref(), tool_context, arguments, None, false)
            .await
    }

    async fn execute_sensitive(
        &self,
        context: &NodeContext,
        tool: &dyn Tool,
        tool_context: Arc<dyn ToolContext>,
        arguments: Value,
        policy: &SensitiveToolPolicy,
    ) -> Result<NodeOutput, GraphError> {
        tracing::Span::current().record("stage", "tool_confirmation");
        match self
            .sensitive_decision(context, &arguments, tool_context.function_call_id(), policy)
            .map_err(|_| node_failure(self.name()))?
        {
            SensitiveDirectToolDecision::Pause(data) => Ok(NodeOutput::interrupt_with_data(
                policy.policy_message(),
                data,
            )),
            SensitiveDirectToolDecision::Approve(remaining) => {
                self.invoke_and_project(tool, tool_context, arguments, Some(remaining), false)
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

    async fn invoke_and_project(
        &self,
        tool: &dyn Tool,
        tool_context: Arc<dyn ToolContext>,
        arguments: Value,
        remaining: Option<Value>,
        authorization_refresh: bool,
    ) -> Result<NodeOutput, GraphError> {
        tracing::Span::current().record("stage", "tool_execution");
        let call_id = tool_context.function_call_id().to_owned();
        let argument_digest = argument_digest(&call_id, tool.name(), &arguments)
            .map_err(|_| node_failure(self.name()))?;
        let result = match tool.execute(tool_context, arguments).await {
            Ok(result) => result,
            Err(error) => {
                let Some(requirement) = delegated_authorization_requirement(&error) else {
                    return Err(node_failure(self.name()));
                };
                if authorization_refresh {
                    return Ok(self
                        .mcp_authorization_stopped(remaining.unwrap_or_else(|| json!({})), true));
                }
                let requirement = requirement.resolve_public_metadata().await;
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
            .map_err(|_| node_failure(self.name()))?;
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
        NodeOutput::interrupt_with_data(
            &message,
            json!({
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
            }),
        )
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
        if result.is_ok() {
            span.record("outcome", "completed");
        } else {
            span.record("outcome", "failed");
            span.record("error_code", "pipeline.toolkit_node.failed");
        }
        result
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
            return Ok(SensitiveDirectToolDecision::Pause(json!({
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
            })));
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
    if encoded.len() > MAX_CONFIRMATION_ARGUMENT_BYTES {
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

struct PipelineToolContext {
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
        self.parent.invocation_id()
    }

    fn agent_name(&self) -> &str {
        self.parent.agent_name()
    }

    fn user_id(&self) -> &str {
        self.parent.user_id()
    }

    fn app_name(&self) -> &str {
        self.parent.app_name()
    }

    fn session_id(&self) -> &str {
        self.parent.session_id()
    }

    fn branch(&self) -> &str {
        self.parent.branch()
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

fn validate_input_mapping(
    raw: BTreeMap<String, RawInputMapping>,
) -> Result<BTreeMap<String, DirectToolInputMapping>, DirectToolConfigurationError> {
    if raw.is_empty() {
        return Ok(BTreeMap::from([(
            "messages".to_owned(),
            DirectToolInputMapping::Variable("messages".to_owned()),
        )]));
    }
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

fn node_failure(node: &str) -> GraphError {
    GraphError::NodeExecutionFailed {
        node: node.to_owned(),
        message: "the pipeline direct-tool node failed".to_owned(),
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
