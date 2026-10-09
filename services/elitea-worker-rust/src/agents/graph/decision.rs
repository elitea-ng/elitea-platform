//! Model-backed routing for the active stored-pipeline `decision` node.
//!
//! The node receives the same claim-bound model factory as an `llm` node but
//! no tools. Its response is never exposed as a free graph target: only one
//! prevalidated normalized label can select a declared node, otherwise the
//! configured default route wins.

use std::collections::BTreeSet;
use std::sync::Arc;

use adk_rust::graph::{GraphError, Node, NodeContext, NodeOutput, State};
use async_trait::async_trait;
use ring::digest;
use serde::Deserialize;
use serde_json::Value;
use thiserror::Error;
use tracing::Instrument as _;

use super::compiler::PIPELINE_YAML_BUDGET;
use super::llm::{
    LlmExecutionInput, LlmNodeDefinition, PipelineLlmAgentFactory, normalize_mapping_value,
    parse_messages, render_fstring, run_model_agent_text,
};
use super::router::{RouteTargets, graph_target};
use super::yaml::{valid_graph_id, valid_output_key};
use crate::bounded_yaml;

const MAX_NODE_YAML_BYTES: usize = 64 * 1024;
const MAX_DESCRIPTION_BYTES: usize = 32 * 1024;
const MAX_PROMPT_BYTES: usize = 64 * 1024;
const MAX_INPUTS: usize = 64;
const CONFIG_DIGEST_DOMAIN: &[u8] = b"elitea.graph.decision.config.v1\0";

const DECISION_PROMPT: &str = "Based on chat history and additional_info make a decision what step need to be next.\nSteps available: {steps}\nExplanation: {description}\n\n{additional_info}\n\n### Expected output:\nAnswer only with step name, no need to add descrip in case none of the steps are applibcable answer with 'END'\n";

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawDecisionNodeDefinition {
    id: String,
    #[serde(rename = "type")]
    node_type: String,
    nodes: Vec<String>,
    #[serde(default)]
    description: String,
    #[serde(default)]
    input: Option<Vec<String>>,
    #[serde(default)]
    decisional_inputs: Option<Vec<String>>,
    #[serde(default = "default_output")]
    default_output: String,
}

fn default_output() -> String {
    "END".to_owned()
}

/// Strict, authority-free definition of one model-backed route choice.
#[derive(Clone)]
pub(super) struct DecisionNodeDefinition {
    id: String,
    description: String,
    inputs: Vec<String>,
    routes: RouteTargets,
}

impl DecisionNodeDefinition {
    pub(super) fn from_yaml(yaml: &str) -> Result<Self, DecisionConfigurationError> {
        if yaml.is_empty() || yaml.len() > MAX_NODE_YAML_BYTES {
            return Err(DecisionConfigurationError::ResourceExhausted);
        }
        let raw = bounded_yaml::from_str_as_yaml_error::<RawDecisionNodeDefinition>(
            yaml,
            PIPELINE_YAML_BUDGET,
        )
        .map_err(|source| DecisionConfigurationError::MalformedYaml { source })?;
        Self::from_raw(raw)
    }

    fn from_raw(raw: RawDecisionNodeDefinition) -> Result<Self, DecisionConfigurationError> {
        if raw.node_type != "decision" {
            return Err(DecisionConfigurationError::Invalid(
                "the node type must be decision",
            ));
        }
        if !valid_graph_id(&raw.id) {
            return Err(DecisionConfigurationError::Invalid(
                "the Decision node ID is malformed",
            ));
        }
        if raw.description.len() > MAX_DESCRIPTION_BYTES
            || raw
                .input
                .as_ref()
                .is_some_and(|value| value.len() > MAX_INPUTS)
            || raw
                .decisional_inputs
                .as_ref()
                .is_some_and(|value| value.len() > MAX_INPUTS)
        {
            return Err(DecisionConfigurationError::ResourceExhausted);
        }
        let inputs = raw
            .decisional_inputs
            .filter(|values| !values.is_empty())
            .or(raw.input)
            .unwrap_or_else(|| vec!["messages".to_owned()]);
        validate_inputs(&inputs)?;
        let routes = RouteTargets::new(raw.nodes, raw.default_output)
            .map_err(|_| DecisionConfigurationError::Invalid("Decision routes are invalid"))?;
        Ok(Self {
            id: raw.id,
            description: raw.description,
            inputs,
            routes,
        })
    }

    pub(super) fn id(&self) -> &str {
        &self.id
    }

    pub(super) fn input_keys(&self) -> &[String] {
        &self.inputs
    }

    pub(super) fn route_targets(&self) -> impl Iterator<Item = &str> {
        self.routes.declared()
    }

    pub(super) fn config_digest(&self) -> [u8; 32] {
        let mut context = digest::Context::new(&digest::SHA256);
        context.update(CONFIG_DIGEST_DOMAIN);
        digest_field(&mut context, self.id.as_bytes());
        digest_field(&mut context, self.description.as_bytes());
        for input in &self.inputs {
            digest_field(&mut context, input.as_bytes());
        }
        self.routes.digest_into(&mut context);
        copy_digest(context.finish().as_ref())
    }

    fn execution_input(&self, state: &State) -> Result<LlmExecutionInput, DecisionExecutionError> {
        let mut history = Vec::new();
        let mut additional = String::new();
        for field in &self.inputs {
            if field == "messages" {
                history = state
                    .get(field)
                    .map(parse_messages)
                    .transpose()
                    .map_err(|_| DecisionExecutionError::InvalidInput)?
                    .unwrap_or_default();
                continue;
            }
            if additional.is_empty() {
                additional.push_str("### Additional info: ");
            }
            let value = state
                .get(field)
                .map(normalize_mapping_value)
                .transpose()
                .map_err(|_| DecisionExecutionError::InvalidInput)?
                .unwrap_or_default();
            additional.push_str(field);
            additional.push_str(": ");
            additional.push_str(&value);
            additional.push('\n');
            ensure_prompt_bound(&additional)?;
        }
        let description = render_fstring(&self.description, state)
            .map_err(|_| DecisionExecutionError::InvalidInput)?;
        let steps = self.routes.labels().collect::<Vec<_>>().join(",");
        let prompt = DECISION_PROMPT
            .replace("{steps}", &steps)
            .replace("{description}", &description)
            .replace("{additional_info}", &additional);
        ensure_prompt_bound(&prompt)?;
        Ok(LlmExecutionInput::for_decision(history, prompt))
    }
}

pub(super) struct DecisionNode {
    definition: DecisionNodeDefinition,
    model_definition: LlmNodeDefinition,
    factory: Arc<dyn PipelineLlmAgentFactory>,
}

impl DecisionNode {
    pub(super) fn new(
        definition: DecisionNodeDefinition,
        factory: Arc<dyn PipelineLlmAgentFactory>,
    ) -> Self {
        let model_definition = LlmNodeDefinition::for_decision(definition.id());
        Self {
            definition,
            model_definition,
            factory,
        }
    }
}

#[async_trait]
impl Node for DecisionNode {
    fn name(&self) -> &str {
        self.definition.id()
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let span = tracing::info_span!(
            "agent.pipeline.decision_node",
            node_id = self.name(),
            route_count = self.definition.routes.route_count(),
            stage = tracing::field::Empty,
            outcome = tracing::field::Empty,
            error_code = tracing::field::Empty,
        );
        let result = async {
            tracing::Span::current().record("stage", "prompt_projection");
            let input = self
                .definition
                .execution_input(&context.state)
                .map_err(|_| node_failure(self.name()))?;
            tracing::Span::current().record("stage", "agent_binding");
            let answer =
                run_model_agent_text(&self.model_definition, input, None, &self.factory, context)
                    .await
                    .map_err(|_| node_failure(self.name()))?;
            tracing::Span::current().record("stage", "route_selection");
            let (target, label) = self.definition.routes.resolve(&answer);
            Ok(NodeOutput::new()
                .with_update("router_output", Value::String(label.to_owned()))
                .with_goto([graph_target(target)]))
        }
        .instrument(span.clone())
        .await;
        if result.is_ok() {
            span.record("outcome", "completed");
        } else {
            span.record("outcome", "failed");
            span.record("error_code", "pipeline.decision_node.failed");
        }
        result
    }
}

fn validate_inputs(inputs: &[String]) -> Result<(), DecisionConfigurationError> {
    let mut seen = BTreeSet::new();
    for input in inputs {
        if !valid_output_key(input) || !seen.insert(input) {
            return Err(DecisionConfigurationError::Invalid(
                "Decision input variables must be valid and unique",
            ));
        }
    }
    Ok(())
}

fn ensure_prompt_bound(value: &str) -> Result<(), DecisionExecutionError> {
    if value.len() > MAX_PROMPT_BYTES || value.contains('\0') {
        return Err(DecisionExecutionError::ResourceExhausted);
    }
    Ok(())
}

fn node_failure(node: &str) -> GraphError {
    GraphError::NodeExecutionFailed {
        node: node.to_owned(),
        message: "the pipeline Decision node failed".to_owned(),
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

#[derive(Debug, Error)]
pub(super) enum DecisionConfigurationError {
    #[error("the Decision YAML is malformed")]
    MalformedYaml {
        #[source]
        source: serde_yaml_ng::Error,
    },
    #[error("{0}")]
    Invalid(&'static str),
    #[error("the Decision configuration exceeds its resource bound")]
    ResourceExhausted,
}

#[derive(Clone, Copy, Debug, Error)]
enum DecisionExecutionError {
    #[error("the Decision input is malformed")]
    InvalidInput,
    #[error("the Decision prompt exceeds its resource bound")]
    ResourceExhausted,
}
