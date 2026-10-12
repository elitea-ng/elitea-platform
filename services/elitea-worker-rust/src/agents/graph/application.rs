//! Direct saved-application execution for the active pipeline `agent` node.
//!
//! The stored node selects one exact frozen Application participant. Assembly
//! resolves that participant through the claim-bound platform boundary and
//! exposes its native ADK [`Tool`] here. The node maps one self-contained task,
//! invokes the child once and projects its final response without adding a
//! parent model turn or recreating a nested `LangGraph` wrapper.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;

use adk_rust::Tool;
use adk_rust::graph::subgraph::SubgraphNode;
use adk_rust::graph::{
    Checkpointer, CompiledGraph, GraphError, Node, NodeContext, NodeOutput, State, StateSchema,
};
use async_trait::async_trait;
use ring::digest;
use serde::Deserialize;
use serde_json::{Value, json};
use thiserror::Error;
use tracing::Instrument as _;

use super::application_activation::ApplicationActivationCheckpointer;
use super::compiler::PIPELINE_YAML_BUDGET;
use super::direct_tool::{ensure_state_type, pipeline_tool_context};
use super::llm::render_fstring;
use super::node_events::{
    PIPELINE_NODE_EVENT_SCOPE_STATE_KEY, PIPELINE_NODE_EVENT_SCOPE_WRAPPER_KEY,
    PipelineNodeEventScope, PipelineNodeEventSender,
};
use super::yaml::{valid_graph_id, valid_output_key};
use crate::agents::application_tools::nested_application_interrupt_ids;
use crate::agents::pipeline::scope_receipts::{GraphCallOutcome, PipelineGraphCallReceipt};
use crate::bounded_yaml;
use adk_rust::{Content, Event, Part};

const MAX_NODE_YAML_BYTES: usize = 64 * 1024;
const MAX_MAPPING_VALUE_BYTES: usize = 240 * 1024;
const MAX_NODE_VARIABLES: usize = 64;
const MAX_APPLICATION_ALIAS_BYTES: usize = 1_024;
const MAX_RESULT_BYTES: usize = 512 * 1024;
const CONFIG_DIGEST_DOMAIN: &[u8] = b"elitea.graph.application.config.v1\0";
pub(crate) const PIPELINE_APPLICATION_HITL_SCHEMA: &str =
    "elitea.graph.application-tool-confirmation.v1";
const PIPELINE_APPLICATION_HITL_MESSAGE: &str =
    "The saved agent is waiting for sensitive-tool approval.";
pub(super) const APPLICATION_TASK_STATE_KEY: &str = "__elitea_application_task_v1";
pub(super) const APPLICATION_MESSAGES_STATE_KEY: &str = "__elitea_application_messages_v1";
pub(super) const APPLICATION_RESULT_STATE_KEY: &str = "__elitea_application_result_v1";

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawApplicationNodeDefinition {
    id: String,
    #[serde(rename = "type")]
    node_type: String,
    tool: String,
    input_mapping: BTreeMap<String, RawInputMapping>,
    #[serde(default)]
    input: Vec<String>,
    #[serde(default)]
    output: Vec<String>,
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

#[derive(Clone)]
enum ApplicationInputMapping {
    Fixed(Value),
    Variable(String),
    Template(String),
}

/// Exact frozen Application participant selected by one graph node.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct PipelineApplicationSelection {
    alias: String,
}

impl PipelineApplicationSelection {
    pub(crate) fn alias(&self) -> &str {
        &self.alias
    }
}

/// Strict authority-free definition of one active Agent node.
#[derive(Clone)]
pub(crate) struct ApplicationNodeDefinition {
    id: String,
    selection: PipelineApplicationSelection,
    task: ApplicationInputMapping,
    variables: BTreeMap<String, ApplicationInputMapping>,
    input: Vec<String>,
    output: Vec<String>,
    transition: Option<String>,
}

impl ApplicationNodeDefinition {
    pub(super) fn from_yaml(yaml: &str) -> Result<Self, ApplicationConfigurationError> {
        if yaml.is_empty() || yaml.len() > MAX_NODE_YAML_BYTES {
            return Err(ApplicationConfigurationError::ResourceExhausted);
        }
        let raw = bounded_yaml::from_str_as_yaml_error::<RawApplicationNodeDefinition>(
            yaml,
            PIPELINE_YAML_BUDGET,
        )
        .map_err(|source| ApplicationConfigurationError::MalformedYaml { source })?;
        Self::from_raw(raw)
    }

    fn from_raw(
        mut raw: RawApplicationNodeDefinition,
    ) -> Result<Self, ApplicationConfigurationError> {
        if raw.node_type != "agent" {
            return Err(ApplicationConfigurationError::Invalid(
                "the node type must be agent",
            ));
        }
        if !valid_graph_id(&raw.id) {
            return Err(ApplicationConfigurationError::Invalid(
                "the Agent node ID is malformed",
            ));
        }
        if !valid_application_alias(&raw.tool) {
            return Err(ApplicationConfigurationError::Invalid(
                "the Agent participant identity is malformed",
            ));
        }
        if raw.input.len() > MAX_NODE_VARIABLES || raw.output.len() > MAX_NODE_VARIABLES {
            return Err(ApplicationConfigurationError::ResourceExhausted);
        }
        if raw.input.is_empty() {
            raw.input.push("messages".to_owned());
        }
        validate_state_keys(&raw.input)?;
        validate_state_keys(&raw.output)?;
        if raw
            .transition
            .as_deref()
            .is_some_and(|target| target != "END" && !valid_graph_id(target))
        {
            return Err(ApplicationConfigurationError::Invalid(
                "the Agent node transition is malformed",
            ));
        }
        if raw.input_mapping.is_empty() || raw.input_mapping.len() > MAX_NODE_VARIABLES {
            return Err(ApplicationConfigurationError::Invalid(
                "the Agent node input mapping must contain a task and bounded variables",
            ));
        }
        let task_mapping =
            raw.input_mapping
                .remove("task")
                .ok_or(ApplicationConfigurationError::Invalid(
                    "the Agent task mapping is missing",
                ))?;
        let task = parse_task_mapping(&task_mapping)?;
        let mut variables = BTreeMap::new();
        for (key, mapping) in raw.input_mapping {
            if !valid_output_key(&key)
                || super::compiler::reserved_user_state_key(&key)
                || matches!(key.as_str(), "input" | "messages")
            {
                return Err(ApplicationConfigurationError::Invalid(
                    "the child variable name is reserved or malformed",
                ));
            }
            let value = if mapping.kind == "fixed" {
                ApplicationInputMapping::Fixed(mapping.value)
            } else {
                parse_task_mapping(&mapping)?
            };
            variables.insert(key, value);
        }
        Ok(Self {
            id: raw.id,
            selection: PipelineApplicationSelection { alias: raw.tool },
            task,
            variables,
            input: raw.input,
            output: raw.output,
            transition: raw.transition,
        })
    }

    pub(crate) fn id(&self) -> &str {
        &self.id
    }

    pub(crate) const fn selection(&self) -> &PipelineApplicationSelection {
        &self.selection
    }

    pub(super) fn input_keys(&self) -> &[String] {
        &self.input
    }

    pub(super) fn output_keys(&self) -> &[String] {
        &self.output
    }

    pub(super) fn validate_map_fixed_inputs(&self) -> Result<(), GraphError> {
        super::map_reduce::validate_values(self.variables.values().filter_map(|mapping| {
            match mapping {
                ApplicationInputMapping::Fixed(value) => Some(value),
                _ => None,
            }
        }))
    }

    pub(super) fn mapped_variables(&self) -> impl Iterator<Item = &str> {
        std::iter::once(&self.task)
            .chain(self.variables.values())
            .filter_map(|mapping| match mapping {
                ApplicationInputMapping::Variable(key) => Some(key.as_str()),
                _ => None,
            })
    }

    fn variable_channel(&self, key: &str) -> String {
        format!(
            "__elitea_application_variable_{}_{}_{key}",
            self.id.len(),
            self.id
        )
    }

    pub(super) fn variable_channels(&self) -> impl Iterator<Item = String> + '_ {
        self.variables.keys().map(|key| self.variable_channel(key))
    }

    pub(super) fn transition(&self) -> Option<&str> {
        self.transition.as_deref()
    }

    pub(super) fn config_digest(&self) -> [u8; 32] {
        let mut context = digest::Context::new(&digest::SHA256);
        context.update(CONFIG_DIGEST_DOMAIN);
        digest_field(&mut context, self.id.as_bytes());
        digest_field(&mut context, self.selection.alias.as_bytes());
        match &self.task {
            ApplicationInputMapping::Fixed(value) => {
                digest_field(&mut context, b"fixed");
                digest_field(&mut context, value.as_str().unwrap_or_default().as_bytes());
            }
            ApplicationInputMapping::Variable(value) => {
                digest_field(&mut context, b"variable");
                digest_field(&mut context, value.as_bytes());
            }
            ApplicationInputMapping::Template(value) => {
                digest_field(&mut context, b"fstring");
                digest_field(&mut context, value.as_bytes());
            }
        }
        for (key, mapping) in &self.variables {
            digest_field(&mut context, b"child-variable");
            digest_field(&mut context, key.as_bytes());
            match mapping {
                ApplicationInputMapping::Fixed(value) => {
                    digest_field(&mut context, b"fixed");
                    digest_field(&mut context, value.to_string().as_bytes());
                }
                ApplicationInputMapping::Variable(value) => {
                    digest_field(&mut context, b"variable");
                    digest_field(&mut context, value.as_bytes());
                }
                ApplicationInputMapping::Template(value) => {
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
        digest_field(
            &mut context,
            self.transition.as_deref().unwrap_or_default().as_bytes(),
        );
        copy_digest(context.finish().as_ref())
    }

    fn digest_label(&self) -> String {
        format!("sha256:{}", hex(&self.config_digest()))
    }

    fn map_task(&self, state: &State) -> Result<String, ApplicationExecutionError> {
        let task = match &self.task {
            ApplicationInputMapping::Fixed(value) => value
                .as_str()
                .ok_or(ApplicationExecutionError::InvalidTask)?
                .to_owned(),
            ApplicationInputMapping::Variable(key) => state
                .get(key)
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_owned(),
            ApplicationInputMapping::Template(value) => {
                render_fstring(value, state).map_err(|_| ApplicationExecutionError::InvalidTask)?
            }
        };
        if task.is_empty() || task.len() > MAX_MAPPING_VALUE_BYTES || task.contains('\0') {
            return Err(ApplicationExecutionError::InvalidTask);
        }
        Ok(task)
    }

    fn project_response(
        &self,
        response: &str,
        state_types: &BTreeMap<String, String>,
        child_values: &State,
    ) -> Result<BTreeMap<String, Value>, ApplicationExecutionError> {
        if response.len() > MAX_RESULT_BYTES || response.contains('\0') {
            return Err(ApplicationExecutionError::InvalidResult);
        }
        let mut updates = BTreeMap::from([(
            "messages".to_owned(),
            json!([{"role": "assistant", "content": response}]),
        )]);
        for key in self.output.iter().filter(|key| key.as_str() != "messages") {
            let value = child_values
                .get(key)
                .filter(|value| !value.is_null())
                .cloned()
                .unwrap_or_else(|| Value::String(response.to_owned()));
            ensure_state_type(key, &value, state_types)
                .map_err(|_| ApplicationExecutionError::InvalidResult)?;
            updates.insert(key.clone(), value);
        }
        let encoded =
            serde_json::to_vec(&updates).map_err(|_| ApplicationExecutionError::InvalidResult)?;
        if encoded.len() > MAX_RESULT_BYTES {
            return Err(ApplicationExecutionError::InvalidResult);
        }
        Ok(updates)
    }
}

#[path = "parallel_application.rs"]
mod parallel_application;
pub(super) use parallel_application::PARALLEL_AGENT_INPUTS_STATE_KEY;

/// Invocation-owned lookup of an already resolved saved Application tool.
pub(crate) trait PipelineApplicationResolver: Send + Sync {
    /// Admit only the original saved identity and an effect-free, nonpausing cohort.
    fn map_worker_definition_digest(
        &self,
        _node: &str,
    ) -> Result<[u8; 32], ApplicationExecutionError> {
        Err(ApplicationExecutionError::Unavailable)
    }
    fn for_map_events(
        &self,
        _events: PipelineNodeEventSender,
    ) -> Result<Arc<dyn PipelineApplicationResolver>, ApplicationExecutionError> {
        Err(ApplicationExecutionError::Unavailable)
    }

    /// One frozen saved participant fingerprint retained by admitted assembly.
    /// Aliases, tool names, and containing graph digests are not substitutes.
    fn fixed_parallel_definition_digest(
        &self,
        _node: &str,
    ) -> Result<[u8; 32], ApplicationExecutionError> {
        Err(ApplicationExecutionError::Unavailable)
    }

    /// Rebind the existing frozen resolver to one exact branch event sender.
    fn for_parallel_events(
        &self,
        _events: PipelineNodeEventSender,
    ) -> Result<Arc<dyn PipelineApplicationResolver>, ApplicationExecutionError> {
        Err(ApplicationExecutionError::Unavailable)
    }

    fn resolve(
        &self,
        selection: &PipelineApplicationSelection,
        checkpointer: Arc<dyn Checkpointer>,
    ) -> Result<ResolvedApplicationParticipant, ApplicationExecutionError>;
    // Explicit test opt-in; production occurrence ownership is supplied by graph_call_owner.
    #[cfg(test)]
    fn test_activation_checkpoints(&self) -> bool {
        false
    }
    fn graph_call_owner(
        &self,
        _node: &str,
    ) -> Option<Arc<crate::agents::pipeline::scoped_runtime::PipelineGraphCallOwner>> {
        None
    }
    fn resolve_node(
        &self,
        _node_id: &str,
        selection: &PipelineApplicationSelection,
        checkpointer: Arc<dyn Checkpointer>,
    ) -> Result<ResolvedApplicationParticipant, ApplicationExecutionError> {
        self.resolve(selection, checkpointer)
    }
}

/// One exact saved participant after claim-bound materialization.
#[derive(Clone)]
pub(crate) enum ResolvedApplicationParticipant {
    /// A direct saved agent invoked through ADK `AgentTool` semantics.
    Agent(Arc<dyn Tool>),
    ScopedAgent {
        tool: Arc<dyn Tool>,
        scope: Arc<crate::agents::pipeline::scoped_runtime::PipelineApplicationNodeRuntime>,
    },
    /// A saved pipeline invoked as a checkpointed native ADK subgraph.
    Pipeline {
        graph: Arc<CompiledGraph>,
        variable_types: BTreeMap<String, String>,
        static_pauses: super::static_pause::StaticPauseCatalog,
        events: Option<PipelineNodeEventSender>,
        display_name: String,
    },
}

pub(super) struct ApplicationNode {
    definition: ApplicationNodeDefinition,
    state_types: BTreeMap<String, String>,
    participant: ResolvedApplicationParticipant,
    checkpointer: Arc<dyn Checkpointer>,
    activation_checkpointer: Option<Arc<ApplicationActivationCheckpointer>>,
    graph_call_owner: Option<Arc<crate::agents::pipeline::scoped_runtime::PipelineGraphCallOwner>>,
    parallel_inputs: bool,
}

impl ApplicationNode {
    pub(super) fn new(
        definition: ApplicationNodeDefinition,
        state_types: BTreeMap<String, String>,
        resolver: &dyn PipelineApplicationResolver,
        checkpointer: Arc<dyn Checkpointer>,
    ) -> Result<Self, ApplicationExecutionError> {
        let graph_call_owner = resolver.graph_call_owner(definition.id());
        let activation_enabled = graph_call_owner.is_some();
        #[cfg(test)]
        let activation_enabled = activation_enabled || resolver.test_activation_checkpoints();
        let activation_checkpointer = activation_enabled.then(|| {
            Arc::new(ApplicationActivationCheckpointer::new(Arc::clone(
                &checkpointer,
            )))
        });
        let child_checkpointer: Arc<dyn Checkpointer> = activation_checkpointer
            .as_ref()
            .map_or_else(|| checkpointer.clone(), |owner| owner.clone());
        let participant =
            resolver.resolve_node(definition.id(), definition.selection(), child_checkpointer)?;
        if !definition.variables.is_empty() {
            match &participant {
                ResolvedApplicationParticipant::Pipeline { variable_types, .. } => {
                    if definition
                        .variables
                        .keys()
                        .any(|key| !variable_types.contains_key(key))
                    {
                        return Err(ApplicationExecutionError::InvalidTask);
                    }
                }
                ResolvedApplicationParticipant::Agent(tool)
                | ResolvedApplicationParticipant::ScopedAgent { tool, .. } => {
                    let schema = tool
                        .parameters_schema()
                        .ok_or(ApplicationExecutionError::InvalidTask)?;
                    let properties = schema
                        .get("properties")
                        .and_then(Value::as_object)
                        .ok_or(ApplicationExecutionError::InvalidTask)?;
                    if definition
                        .variables
                        .keys()
                        .any(|key| !properties.contains_key(key))
                    {
                        return Err(ApplicationExecutionError::InvalidTask);
                    }
                }
            }
        }
        Ok(Self {
            definition,
            state_types,
            participant,
            checkpointer,
            activation_checkpointer,
            graph_call_owner,
            parallel_inputs: false,
        })
    }

    fn pipeline_subgraph(&self, graph: Arc<CompiledGraph>) -> SubgraphNode {
        let mut node = SubgraphNode::new(self.definition.id(), graph)
            .isolated()
            .with_input(APPLICATION_TASK_STATE_KEY, "input")
            .with_input(APPLICATION_MESSAGES_STATE_KEY, "messages")
            .with_output("elitea_response", APPLICATION_RESULT_STATE_KEY);
        if let ResolvedApplicationParticipant::Pipeline { variable_types, .. } = &self.participant {
            for key in self
                .definition
                .output
                .iter()
                .filter(|key| variable_types.contains_key(*key))
            {
                node = node.with_output(key, key);
            }
        }
        for key in self.definition.variables.keys() {
            node = node.with_input(self.definition.variable_channel(key), key);
        }
        for channel in [
            "session_id",
            "hitl_decisions",
            super::hitl::HITL_RESUME_STATE_KEY,
            super::direct_tool::DIRECT_TOOL_RESUME_STATE_KEY,
            super::llm::LLM_TOOL_RESUME_STATE_KEY,
            super::static_pause::STATIC_AFTER_CHECKPOINTS_STATE_KEY,
            super::static_pause::STATIC_TEXT_RESUME_STATE_KEY,
            PIPELINE_NODE_EVENT_SCOPE_STATE_KEY,
        ] {
            if node.graph().schema().channels.contains_key(channel) {
                node = node.with_input(channel, channel);
            }
        }
        node
    }

    #[allow(clippy::too_many_lines)] // Keep original activation, durable completion, and event emission visibly ordered.
    async fn execute_pipeline(
        &self,
        context: &NodeContext,
        task: &str,
        graph: Arc<CompiledGraph>,
        events: Option<&PipelineNodeEventSender>,
        display_name: &str,
    ) -> Result<NodeOutput, GraphError> {
        let parent_schema = context
            .parent_schema()
            .ok_or_else(|| node_failure(self.name()))?;
        let mut state = context.state.clone();
        if let ResolvedApplicationParticipant::Pipeline { variable_types, .. } = &self.participant
            && !self.definition.variables.is_empty()
        {
            let user_state: State = context
                .state
                .iter()
                .filter(|(key, _)| {
                    (key.as_str() == "input" || self.state_types.contains_key(*key))
                        && !super::compiler::reserved_user_state_key(key)
                        && key.as_str() != "messages"
                })
                .map(|(key, value)| (key.clone(), value.clone()))
                .collect();
            let mut mapped = BTreeMap::new();
            for (key, mapping) in &self.definition.variables {
                let value = if self.parallel_inputs {
                    self.parallel_variable(&context.state, key)?
                } else {
                    match mapping {
                        ApplicationInputMapping::Fixed(value) => value.clone(),
                        ApplicationInputMapping::Variable(source) => user_state
                            .get(source)
                            .cloned()
                            .ok_or_else(|| node_failure(self.name()))?,
                        ApplicationInputMapping::Template(template) => Value::String(
                            render_fstring(template, &user_state)
                                .map_err(|_| node_failure(self.name()))?,
                        ),
                    }
                };
                validate_child_input(self.name(), key, &value, variable_types, events).await?;
                mapped.insert(self.definition.variable_channel(key), value);
            }
            if serde_json::to_vec(&mapped)
                .map_err(|_| node_failure(self.name()))?
                .len()
                > MAX_MAPPING_VALUE_BYTES
            {
                return Err(node_failure(self.name()));
            }
            state.extend(mapped);
        }
        let mut activation_inputs = serde_json::Map::from_iter([("task".to_owned(), json!(task))]);
        for key in self.definition.variables.keys() {
            let value = state
                .get(&self.definition.variable_channel(key))
                .cloned()
                .ok_or_else(|| node_failure(self.name()))?;
            activation_inputs.insert(key.clone(), value);
        }
        let _activation = self
            .activation_checkpointer
            .as_ref()
            .map(|owner| {
                owner.enter(
                    &context.config.thread_id,
                    self.name(),
                    context.step,
                    self.definition.config_digest(),
                    &Value::Object(activation_inputs.clone()),
                )
            })
            .transpose()?;
        let parent_scope = PipelineNodeEventScope::from_state(
            context.state.get(PIPELINE_NODE_EVENT_SCOPE_STATE_KEY),
        )
        .map_err(|_| node_failure(self.name()))?;
        let graph_receipt = match &self.graph_call_owner {
            Some(owner) => Some(
                owner
                    .prepare(
                        context,
                        self.checkpointer.as_ref(),
                        display_name,
                        &Value::Object(activation_inputs.clone()),
                    )
                    .await?,
            ),
            None => None,
        };
        let call_id = graph_receipt.as_ref().map_or_else(
            || {
                if parent_scope.is_some() {
                    crate::agents::pipeline::scoped_applications::scoped_graph_call_id(
                        &context.config.thread_id,
                        self.name(),
                        context.step,
                        self.definition.config_digest(),
                    )
                } else {
                    format!("pipeline:{}:{}", self.name(), context.step)
                }
            },
            |receipt| receipt.activation().call_id().to_owned(),
        );
        let checkpoint_thread_id = format!("{}/{}", context.config.thread_id, self.name());
        let task = state
            .get(super::static_pause::STATIC_TEXT_RESUME_STATE_KEY)
            .and_then(|value| value.get(&checkpoint_thread_id))
            .and_then(Value::as_str)
            .unwrap_or(task);
        let task = task.to_owned();
        state.insert(APPLICATION_TASK_STATE_KEY.to_owned(), json!(task));
        state.insert(
            APPLICATION_MESSAGES_STATE_KEY.to_owned(),
            json!([{"role": "user", "content": task}]),
        );
        let event_scope = events
            .map(|_| {
                PipelineNodeEventScope::new(&call_id, display_name, &checkpoint_thread_id)
                    .and_then(|scope| scope.with_parent(parent_scope.clone()))
            })
            .transpose()
            .map_err(|_| node_failure(self.name()))?;
        if let (Some(events), Some(event_scope)) = (events, event_scope.as_ref()) {
            state.insert(
                PIPELINE_NODE_EVENT_SCOPE_STATE_KEY.to_owned(),
                event_scope
                    .to_state_value()
                    .map_err(|_| node_failure(self.name()))?,
            );
            if let Some(receipt) = &graph_receipt {
                events
                    .send_original_application_event(
                        receipt.original_start().clone(),
                        parent_scope.as_ref(),
                        context
                            .config
                            .parent_context
                            .as_ref()
                            .ok_or_else(|| node_failure(self.name()))?
                            .invocation_id(),
                    )
                    .await
                    .map_err(|_| node_failure(self.name()))?;
            } else {
                events
                    .send_application_start_scoped(
                        display_name,
                        &call_id,
                        &Value::Object(activation_inputs),
                        parent_scope.as_ref(),
                    )
                    .await
                    .map_err(|_| node_failure(self.name()))?;
            }
        }
        if let Some(receipt) = &graph_receipt
            && let GraphCallOutcome::Completed { result, terminal } = receipt.outcome()
        {
            let response = result
                .get("response")
                .and_then(Value::as_str)
                .ok_or_else(|| node_failure(self.name()))?;
            let values: State = serde_json::from_value(
                result
                    .get("values")
                    .ok_or_else(|| node_failure(self.name()))?
                    .clone(),
            )
            .map_err(|_| node_failure(self.name()))?;
            if result.as_object().is_none_or(|object| object.len() != 2)
                || values
                    .keys()
                    .any(|key| !self.definition.output.contains(key))
            {
                return Err(node_failure(self.name()));
            }
            let output = self.projected_output_with_values(response, &values)?;
            if let Some(events) = events {
                events
                    .send_original_application_event(
                        terminal.clone(),
                        parent_scope.as_ref(),
                        context
                            .config
                            .parent_context
                            .as_ref()
                            .ok_or_else(|| node_failure(self.name()))?
                            .invocation_id(),
                    )
                    .await
                    .map_err(|_| node_failure(self.name()))?;
            }
            return Ok(output);
        }
        let mut child_context = NodeContext::new(state, context.config.clone(), context.step);
        child_context.set_parent_schema(parent_schema);
        let mut output = self
            .pipeline_subgraph(graph)
            .execute(&child_context)
            .await?;
        if let Some(interrupt) = output.interrupt.as_mut() {
            self.bind_child_checkpoint(context, interrupt, event_scope.as_ref())
                .await?;
            return Ok(output);
        }
        if output.goto.is_some() || output.goto_parent.is_some() || !output.events.is_empty() {
            return Err(node_failure(self.name()));
        }
        self.finish_pipeline_output(
            output,
            events,
            display_name,
            &call_id,
            parent_scope.as_ref(),
            context,
            graph_receipt.as_ref(),
        )
        .await
    }

    #[allow(clippy::too_many_arguments)] // Exact occurrence context and original receipt stay visible at commit.
    async fn finish_pipeline_output(
        &self,
        mut output: NodeOutput,
        events: Option<&PipelineNodeEventSender>,
        display_name: &str,
        call_id: &str,
        parent_scope: Option<&PipelineNodeEventScope>,
        context: &NodeContext,
        receipt: Option<&PipelineGraphCallReceipt>,
    ) -> Result<NodeOutput, GraphError> {
        let response = output
            .updates
            .remove(APPLICATION_RESULT_STATE_KEY)
            .and_then(|value| value.as_str().map(str::to_owned))
            .ok_or_else(|| node_failure(self.name()))?;
        if output
            .updates
            .keys()
            .any(|key| !self.definition.output.contains(key))
        {
            return Err(node_failure(self.name()));
        }
        if let Some(receipt) = receipt {
            // Validate the mapping before committing completion; replay needs no child poll.
            let projected = self.projected_output_with_values(&response, &output.updates)?;
            let result = json!({"response":response,"values":output.updates});
            let mut terminal = Event::new(receipt.invocation_id());
            terminal.author = receipt.author().to_owned();
            terminal.branch = receipt.branch().to_owned();
            terminal.llm_response.content = Some(Content {
                role: "function".to_owned(),
                parts: vec![Part::FunctionResponse {
                    function_response: adk_rust::FunctionResponseData::new(
                        display_name,
                        result.clone(),
                    ),
                    id: Some(receipt.activation().call_id().to_owned()),
                    annotations: None,
                }],
            });
            receipt
                .activation()
                .stamp_with_branch(&mut terminal, receipt.branch().to_owned())
                .map_err(|_| node_failure(self.name()))?;
            let completed = self
                .graph_call_owner
                .as_ref()
                .ok_or_else(|| node_failure(self.name()))?
                .finish(
                    context,
                    self.checkpointer.as_ref(),
                    receipt,
                    GraphCallOutcome::Completed { result, terminal },
                )
                .await?;
            if let Some(events) = events
                && let GraphCallOutcome::Completed { terminal, .. } = completed.outcome()
            {
                events
                    .send_original_application_event(
                        terminal.clone(),
                        parent_scope,
                        context
                            .config
                            .parent_context
                            .as_ref()
                            .ok_or_else(|| node_failure(self.name()))?
                            .invocation_id(),
                    )
                    .await
                    .map_err(|_| node_failure(self.name()))?;
            }
            return Ok(projected);
        }
        if let Some(events) = events {
            events
                .send_application_end_scoped(display_name, call_id, &response, parent_scope)
                .await
                .map_err(|_| node_failure(self.name()))?;
        }
        let projected = self.projected_output_with_values(&response, &output.updates);
        if projected.is_err() {
            tracing::error!(
                node_id = self.name(),
                error_code = "pipeline.result_invalid",
                "Child pipeline result does not match the parent output mapping; no parent updates applied"
            );
            if let Some(events) = events {
                events
                    .send_execution_failure("pipeline.result_invalid")
                    .await
                    .map_err(|_| node_failure(self.name()))?;
            }
        }
        projected
    }

    async fn bind_child_checkpoint(
        &self,
        context: &NodeContext,
        interrupt: &mut adk_rust::graph::interrupt::Interrupt,
        event_scope: Option<&PipelineNodeEventScope>,
    ) -> Result<(), GraphError> {
        let adk_rust::graph::interrupt::Interrupt::Dynamic {
            message,
            data: Some(data),
        } = interrupt
        else {
            return Err(node_failure(self.name()));
        };
        let wrapper = data
            .as_object_mut()
            .ok_or_else(|| node_failure(self.name()))?;
        let expected_thread = format!("{}/{}", context.config.thread_id, self.name());
        if wrapper.get("subgraph").and_then(Value::as_str) != Some(self.name())
            || wrapper.get("thread").and_then(Value::as_str) != Some(expected_thread.as_str())
            || wrapper.contains_key("checkpoint_id")
            || wrapper.contains_key(PIPELINE_NODE_EVENT_SCOPE_WRAPPER_KEY)
        {
            return Err(node_failure(self.name()));
        }
        let checkpoint = self
            .checkpointer
            .load(&expected_thread)
            .await
            .map_err(|_| node_failure(self.name()))?
            .ok_or_else(|| node_failure(self.name()))?;
        if checkpoint.thread_id != expected_thread || checkpoint.pending_nodes.len() != 1 {
            return Err(node_failure(self.name()));
        }
        if !wrapper.contains_key("data") {
            let ResolvedApplicationParticipant::Pipeline { static_pauses, .. } = &self.participant
            else {
                return Err(node_failure(self.name()));
            };
            let inner_message = message
                .strip_prefix(&format!("{}: ", self.name()))
                .ok_or_else(|| node_failure(self.name()))?;
            let metadata = static_pauses
                .bind_native_message(inner_message, &checkpoint)
                .ok_or_else(|| node_failure(self.name()))?;
            wrapper.insert(
                "data".to_owned(),
                serde_json::to_value(metadata).map_err(|_| node_failure(self.name()))?,
            );
        }
        wrapper.insert(
            "checkpoint_id".to_owned(),
            Value::String(checkpoint.checkpoint_id),
        );
        if let Some(event_scope) = event_scope {
            if event_scope.checkpoint_thread_id() != expected_thread {
                return Err(node_failure(self.name()));
            }
            wrapper.insert(
                PIPELINE_NODE_EVENT_SCOPE_WRAPPER_KEY.to_owned(),
                event_scope
                    .to_state_value()
                    .map_err(|_| node_failure(self.name()))?,
            );
        }
        Ok(())
    }

    fn projected_output(&self, response: &str) -> Result<NodeOutput, GraphError> {
        self.projected_output_with_values(response, &State::new())
    }

    fn projected_output_with_values(
        &self,
        response: &str,
        child_values: &State,
    ) -> Result<NodeOutput, GraphError> {
        let updates = self
            .definition
            .project_response(response, &self.state_types, child_values)
            .map_err(|_| node_failure(self.name()))?;
        let mut output = NodeOutput::new();
        for (key, value) in updates {
            output = output.with_update(&key, value);
        }
        Ok(output)
    }
}

#[async_trait]
impl Node for ApplicationNode {
    fn name(&self) -> &str {
        self.definition.id()
    }

    fn validate_against(&self, parent: &StateSchema) -> Result<(), GraphError> {
        if let ResolvedApplicationParticipant::Pipeline { graph, .. } = &self.participant {
            self.pipeline_subgraph(Arc::clone(graph))
                .validate_against(parent)?;
        }
        Ok(())
    }

    #[allow(clippy::too_many_lines)] // Keep original activation, durable completion, and event emission visibly ordered.
    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let span = tracing::info_span!(
            "agent.pipeline.application_node",
            node_id = self.name(),
            application_alias = self.definition.selection.alias(),
            output_count = self.definition.output.len(),
            stage = tracing::field::Empty,
            outcome = tracing::field::Empty,
            error_code = tracing::field::Empty,
        );
        let result = async {
            tracing::Span::current().record("stage", "input_mapping");
            let task = self.mapped_task(&context.state)?;
            tracing::Span::current().record("stage", "child_execution");
            match &self.participant {
                ResolvedApplicationParticipant::Agent(tool)
                | ResolvedApplicationParticipant::ScopedAgent { tool, .. } => {
                    let tool_context = pipeline_tool_context(context, self.name(), tool.name());
                    let mut arguments =
                        serde_json::Map::from_iter([("task".to_owned(), json!(task))]);
                    for (key, mapping) in &self.definition.variables {
                        let value = if self.parallel_inputs {
                            self.parallel_variable(&context.state, key)?
                        } else {
                            match mapping {
                                ApplicationInputMapping::Fixed(value) => value.clone(),
                                ApplicationInputMapping::Variable(source) => context
                                    .state
                                    .get(source)
                                    .cloned()
                                    .ok_or_else(|| node_failure(self.name()))?,
                                ApplicationInputMapping::Template(template) => Value::String(
                                    render_fstring(template, &context.state)
                                        .map_err(|_| node_failure(self.name()))?,
                                ),
                            }
                        };
                        arguments.insert(key.clone(), value);
                    }
                    let (result, call_id) = match &self.participant {
                        ResolvedApplicationParticipant::ScopedAgent { scope, .. } => {
                            scope
                                .execute(
                                    context,
                                    self.checkpointer.as_ref(),
                                    tool,
                                    Value::Object(arguments),
                                )
                                .await?
                        }
                        _ => (
                            tool.execute(Arc::clone(&tool_context), Value::Object(arguments))
                                .await
                                .map_err(|_| node_failure(self.name()))?,
                            tool_context.function_call_id().to_owned(),
                        ),
                    };
                    if let Some(interrupt_ids) = nested_application_interrupt_ids(&result) {
                        return Ok(NodeOutput::interrupt_with_data(
                            PIPELINE_APPLICATION_HITL_MESSAGE,
                            json!({
                                "schema_revision": PIPELINE_APPLICATION_HITL_SCHEMA,
                                "type": "hitl_checkpoint",
                                "guardrail_type": "application_sensitive_tool",
                                "node_name": self.name(),
                                "message": PIPELINE_APPLICATION_HITL_MESSAGE,
                                "definition_digest": self.definition.digest_label(),
                                "application_call_id": call_id,
                                "application_tool_name": tool.name(),
                                "interrupt_ids": interrupt_ids,
                            }),
                        ));
                    }
                    let response = result
                        .as_object()
                        .filter(|object| object.len() == 1)
                        .and_then(|object| object.get("response"))
                        .and_then(Value::as_str)
                        .ok_or_else(|| node_failure(self.name()))?;
                    tracing::Span::current().record("stage", "state_projection");
                    self.projected_output(response)
                }
                ResolvedApplicationParticipant::Pipeline {
                    graph,
                    events,
                    display_name,
                    ..
                } => {
                    self.execute_pipeline(
                        context,
                        &task,
                        Arc::clone(graph),
                        events.as_ref(),
                        display_name,
                    )
                    .await
                }
            }
        }
        .instrument(span.clone())
        .await;
        if result.is_ok() {
            span.record("outcome", "completed");
        } else {
            span.record("outcome", "failed");
            span.record("error_code", "pipeline.application_node.failed");
        }
        result
    }
}

fn parse_task_mapping(
    raw: &RawInputMapping,
) -> Result<ApplicationInputMapping, ApplicationConfigurationError> {
    match raw.kind.as_str() {
        "fixed" => bounded_mapping_text(&raw.value)
            .map(|value| ApplicationInputMapping::Fixed(Value::String(value.to_owned()))),
        "variable" => {
            let key = bounded_mapping_text(&raw.value)?;
            if !valid_output_key(key) {
                return Err(ApplicationConfigurationError::Invalid(
                    "the Agent task variable is malformed",
                ));
            }
            Ok(ApplicationInputMapping::Variable(key.to_owned()))
        }
        "fstring" => bounded_mapping_text(&raw.value)
            .map(str::to_owned)
            .map(ApplicationInputMapping::Template),
        _ => Err(ApplicationConfigurationError::Unsupported(
            "the Agent input mapping type is not supported",
        )),
    }
}

fn bounded_mapping_text(value: &Value) -> Result<&str, ApplicationConfigurationError> {
    value
        .as_str()
        .filter(|value| value.len() <= MAX_MAPPING_VALUE_BYTES && !value.contains('\0'))
        .ok_or(ApplicationConfigurationError::Invalid(
            "the Agent task mapping is malformed",
        ))
}

fn validate_state_keys(keys: &[String]) -> Result<(), ApplicationConfigurationError> {
    let mut seen = BTreeSet::new();
    if keys
        .iter()
        .any(|key| !valid_output_key(key) || !seen.insert(key.as_str()))
    {
        return Err(ApplicationConfigurationError::Invalid(
            "Agent node variables must be valid and unique",
        ));
    }
    Ok(())
}

fn valid_application_alias(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_APPLICATION_ALIAS_BYTES
        && !value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n'))
}

async fn validate_child_input(
    node: &str,
    key: &str,
    value: &Value,
    variable_types: &BTreeMap<String, String>,
    events: Option<&PipelineNodeEventSender>,
) -> Result<(), GraphError> {
    if ensure_state_type(key, value, variable_types).is_ok() {
        return Ok(());
    }
    tracing::error!(
        node_id = node,
        input_key = key,
        expected_type = variable_types.get(key).map_or("unknown", String::as_str),
        error_code = "pipeline.child_input_type_invalid",
        "Saved child pipeline input does not match its declared state type; the child did not start"
    );
    if let Some(events) = events {
        events
            .send_execution_failure("pipeline.child_input_type_invalid")
            .await
            .map_err(|_| node_failure(node))?;
    }
    Err(GraphError::NodeExecutionFailed {
        node: node.to_owned(),
        message: format!(
            "Child pipeline input '{key}' does not match its declared type; the child did not start."
        ),
    })
}

fn node_failure(node: &str) -> GraphError {
    GraphError::NodeExecutionFailed {
        node: node.to_owned(),
        message: "the pipeline Agent node failed".to_owned(),
    }
}

fn digest_field(context: &mut digest::Context, value: &[u8]) {
    context.update(&(value.len() as u64).to_be_bytes());
    context.update(value);
}

fn copy_digest(value: &[u8]) -> [u8; 32] {
    let mut output = [0_u8; 32];
    output.copy_from_slice(value);
    output
}

fn hex(value: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut output = String::with_capacity(value.len() * 2);
    for byte in value {
        output.push(char::from(HEX[usize::from(byte >> 4)]));
        output.push(char::from(HEX[usize::from(byte & 0x0f)]));
    }
    output
}

#[derive(Debug, Error)]
pub(crate) enum ApplicationConfigurationError {
    #[error("the Agent node YAML is malformed")]
    MalformedYaml {
        #[source]
        source: serde_yaml_ng::Error,
    },
    #[error("{0}")]
    Invalid(&'static str),
    #[error("{0}")]
    Unsupported(&'static str),
    #[error("the Agent node exceeds its resource bound")]
    ResourceExhausted,
}

#[derive(Clone, Copy, Debug, Eq, Error, PartialEq)]
pub(crate) enum ApplicationExecutionError {
    #[error("the Agent task is invalid")]
    InvalidTask,
    #[error("the Agent result is invalid")]
    InvalidResult,
    #[error("the selected Agent is unavailable")]
    Unavailable,
}
