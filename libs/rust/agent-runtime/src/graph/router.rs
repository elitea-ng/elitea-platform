//! Bounded implementation of the active stored-pipeline `router` node.
//!
//! The SDK renders one Jinja condition and then lets a conditional edge route
//! on `router_output`. Rust keeps the same public state value but performs the
//! declared-target check and ADK `goto` atomically in one node. Rendered text
//! can therefore never name an undeclared graph target.

use std::collections::{BTreeMap, BTreeSet};
use std::io;

use adk_graph::{END, GraphError, Node, NodeContext, NodeOutput, State};
use async_trait::async_trait;
use minijinja::value::Value as JinjaValue;
use minijinja::{Environment, Error as JinjaError, ErrorKind, UndefinedBehavior};
use ring::digest;
use serde::Deserialize;
use serde_json::Value;
use thiserror::Error;
use tracing::Instrument as _;

use super::state_modifier::template_value;
use super::yaml::{valid_graph_id, valid_output_key};

const MAX_NODE_YAML_BYTES: usize = 64 * 1024;
const MAX_CONDITION_BYTES: usize = 64 * 1024;
const MAX_RENDERED_BYTES: usize = 8 * 1024;
const MAX_INPUTS: usize = 64;
const MAX_ROUTES: usize = 64;
const TEMPLATE_FUEL: u64 = 250_000;
const CONFIG_DIGEST_DOMAIN: &[u8] = b"elitea.graph.router.config.v1\0";

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawRouterNodeDefinition {
    id: String,
    #[serde(rename = "type")]
    node_type: String,
    #[serde(default)]
    condition: String,
    #[serde(default)]
    routes: Vec<String>,
    #[serde(default = "default_output")]
    default_output: String,
    #[serde(default)]
    input: Option<Vec<String>>,
}

fn default_output() -> String {
    "END".to_owned()
}

/// One exact declared route and the SDK-compatible public label for it.
#[derive(Clone)]
struct RouteTarget {
    target: String,
    label: String,
}

/// Validated route table shared by deterministic and model-backed routing.
#[derive(Clone)]
pub struct RouteTargets {
    routes: Vec<RouteTarget>,
    default: RouteTarget,
}

impl RouteTargets {
    pub fn new(routes: Vec<String>, default: String) -> Result<Self, RouterConfigurationError> {
        if routes.len() > MAX_ROUTES {
            return Err(RouterConfigurationError::ResourceExhausted);
        }
        let mut labels = BTreeSet::new();
        let mut exact_targets = BTreeSet::new();
        let routes = routes
            .into_iter()
            .map(|target| {
                validate_target(&target)?;
                if !exact_targets.insert(target.clone()) {
                    return Err(RouterConfigurationError::Invalid(
                        "router targets must be unique",
                    ));
                }
                let label = normalized_route_label(&target);
                if label.is_empty() || !labels.insert(label.clone()) {
                    return Err(RouterConfigurationError::Invalid(
                        "router targets collide after SDK label normalization",
                    ));
                }
                Ok(RouteTarget { target, label })
            })
            .collect::<Result<Vec<_>, _>>()?;
        validate_target(&default)?;
        let default = RouteTarget {
            label: normalized_route_label(&default),
            target: default,
        };
        if default.label.is_empty() {
            return Err(RouterConfigurationError::Invalid(
                "the router default target is malformed",
            ));
        }
        Ok(Self { routes, default })
    }

    #[must_use]
    pub fn resolve(&self, rendered: &str) -> (&str, &str) {
        let label = normalized_route_label(rendered.trim());
        self.routes
            .iter()
            .find(|route| route.label == label)
            .map_or(
                (self.default.target.as_str(), self.default.label.as_str()),
                |route| (route.target.as_str(), route.label.as_str()),
            )
    }

    pub fn declared(&self) -> impl Iterator<Item = &str> {
        self.routes
            .iter()
            .map(|route| route.target.as_str())
            .chain(std::iter::once(self.default.target.as_str()))
    }

    pub fn labels(&self) -> impl Iterator<Item = &str> {
        self.routes.iter().map(|route| route.label.as_str())
    }

    #[must_use]
    pub fn route_count(&self) -> usize {
        self.routes.len()
    }

    pub fn digest_into(&self, context: &mut digest::Context) {
        for route in &self.routes {
            digest_field(context, route.target.as_bytes());
        }
        digest_field(context, self.default.target.as_bytes());
    }
}

/// Strict authority-free definition of one deterministic Router node.
#[derive(Clone)]
pub struct RouterNodeDefinition {
    id: String,
    condition: String,
    routes: RouteTargets,
    input: Vec<String>,
}

impl RouterNodeDefinition {
    pub fn from_yaml(yaml: &str) -> Result<Self, RouterConfigurationError> {
        if yaml.is_empty() || yaml.len() > MAX_NODE_YAML_BYTES {
            return Err(RouterConfigurationError::ResourceExhausted);
        }
        let raw = serde_yaml_ng::from_str::<RawRouterNodeDefinition>(yaml)
            .map_err(|source| RouterConfigurationError::MalformedYaml { source })?;
        Self::from_raw(raw)
    }

    fn from_raw(raw: RawRouterNodeDefinition) -> Result<Self, RouterConfigurationError> {
        if raw.node_type != "router" {
            return Err(RouterConfigurationError::Invalid(
                "the node type must be router",
            ));
        }
        if !valid_graph_id(&raw.id) {
            return Err(RouterConfigurationError::Invalid(
                "the Router node ID is malformed",
            ));
        }
        if raw.condition.len() > MAX_CONDITION_BYTES
            || raw.input.as_ref().is_some_and(|v| v.len() > MAX_INPUTS)
        {
            return Err(RouterConfigurationError::ResourceExhausted);
        }
        let input = raw.input.unwrap_or_else(|| vec!["messages".to_owned()]);
        validate_inputs(&input)?;
        Ok(Self {
            id: raw.id,
            condition: raw.condition,
            routes: RouteTargets::new(raw.routes, raw.default_output)?,
            input,
        })
    }

    #[must_use]
    pub fn id(&self) -> &str {
        &self.id
    }

    #[must_use]
    pub fn input_keys(&self) -> &[String] {
        &self.input
    }

    pub fn route_targets(&self) -> impl Iterator<Item = &str> {
        self.routes.declared()
    }

    #[must_use]
    pub fn config_digest(&self) -> [u8; 32] {
        let mut context = digest::Context::new(&digest::SHA256);
        context.update(CONFIG_DIGEST_DOMAIN);
        digest_field(&mut context, self.id.as_bytes());
        digest_field(&mut context, self.condition.as_bytes());
        for key in &self.input {
            digest_field(&mut context, key.as_bytes());
        }
        self.routes.digest_into(&mut context);
        copy_digest(context.finish().as_ref())
    }
}

pub struct RouterNode {
    definition: RouterNodeDefinition,
}

impl RouterNode {
    #[must_use]
    pub const fn new(definition: RouterNodeDefinition) -> Self {
        Self { definition }
    }
}

#[async_trait]
impl Node for RouterNode {
    fn name(&self) -> &str {
        self.definition.id()
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let span = tracing::info_span!(
            "agent.pipeline.router_node",
            node_id = self.name(),
            route_count = self.definition.routes.route_count(),
            stage = tracing::field::Empty,
            outcome = tracing::field::Empty,
            error_code = tracing::field::Empty,
        );
        let result = async {
            tracing::Span::current().record("stage", "condition_render");
            let rendered = render_condition(
                &self.definition.condition,
                &selected_state(&context.state, &self.definition.input),
            )
            .map_err(|_| node_failure(self.name()))?;
            tracing::Span::current().record("stage", "route_selection");
            let (target, label) = self.definition.routes.resolve(&rendered);
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
            span.record("error_code", "pipeline.router_node.failed");
        }
        result
    }
}

fn selected_state(state: &State, inputs: &[String]) -> BTreeMap<String, Value> {
    if inputs.is_empty() {
        return state
            .iter()
            .map(|(key, value)| (key.clone(), value.clone()))
            .collect();
    }
    inputs
        .iter()
        .map(|key| {
            (
                key.clone(),
                state
                    .get(key)
                    .cloned()
                    .unwrap_or_else(|| Value::String(String::new())),
            )
        })
        .collect()
}

fn render_condition(
    source: &str,
    input: &BTreeMap<String, Value>,
) -> Result<String, RouterExecutionError> {
    let mut environment = Environment::new();
    environment.set_undefined_behavior(UndefinedBehavior::Lenient);
    environment.set_fuel(Some(TEMPLATE_FUEL));
    environment.add_filter("json_loads", json_loads_filter);
    let template = environment
        .template_from_str(source)
        .map_err(|_| RouterExecutionError::Template)?;
    let mut writer = BoundedWriter::new(MAX_RENDERED_BYTES);
    template
        .render_captured_to(
            input
                .iter()
                .map(|(key, value)| (key.as_str(), template_value(value)))
                .collect::<BTreeMap<_, _>>(),
            &mut writer,
        )
        .map_err(|_| RouterExecutionError::Template)?;
    String::from_utf8(writer.into_inner()).map_err(|_| RouterExecutionError::Template)
}

fn json_loads_filter(
    value: &JinjaValue,
    replace_quotes: Option<bool>,
) -> Result<JinjaValue, JinjaError> {
    let text = value.as_str().ok_or_else(|| {
        JinjaError::new(ErrorKind::InvalidOperation, "json_loads expects a string")
    })?;
    let repaired;
    let text = if replace_quotes.unwrap_or(false) {
        repaired = text.replace('\'', "\"");
        repaired.as_str()
    } else {
        text
    };
    serde_json::from_str::<Value>(text)
        .map(|value| template_value(&value))
        .map_err(|_| JinjaError::new(ErrorKind::InvalidOperation, "json_loads input is invalid"))
}

fn validate_inputs(inputs: &[String]) -> Result<(), RouterConfigurationError> {
    let mut seen = BTreeSet::new();
    for key in inputs {
        if !valid_output_key(key) || !seen.insert(key) {
            return Err(RouterConfigurationError::Invalid(
                "Router input variables must be valid and unique",
            ));
        }
    }
    Ok(())
}

fn validate_target(target: &str) -> Result<(), RouterConfigurationError> {
    if target != "END" && !valid_graph_id(target) {
        return Err(RouterConfigurationError::Invalid(
            "a router target is malformed",
        ));
    }
    Ok(())
}

#[must_use]
pub fn normalized_route_label(value: &str) -> String {
    value
        .bytes()
        .filter(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-' | b'.'))
        .map(|byte| if byte == b'.' { '_' } else { byte as char })
        .collect()
}

#[must_use]
pub fn graph_target(target: &str) -> &str {
    if target == "END" { END } else { target }
}

fn node_failure(node: &str) -> GraphError {
    GraphError::NodeExecutionFailed {
        node: node.to_owned(),
        message: "the pipeline Router node failed".to_owned(),
    }
}

struct BoundedWriter {
    bytes: Vec<u8>,
    limit: usize,
}

impl BoundedWriter {
    const fn new(limit: usize) -> Self {
        Self {
            bytes: Vec::new(),
            limit,
        }
    }

    fn into_inner(self) -> Vec<u8> {
        self.bytes
    }
}

impl io::Write for BoundedWriter {
    fn write(&mut self, buffer: &[u8]) -> io::Result<usize> {
        let next = self
            .bytes
            .len()
            .checked_add(buffer.len())
            .ok_or_else(|| io::Error::other("Router output exceeds its resource bound"))?;
        if next > self.limit {
            return Err(io::Error::other("Router output exceeds its resource bound"));
        }
        self.bytes.extend_from_slice(buffer);
        Ok(buffer.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
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
pub enum RouterConfigurationError {
    #[error("the Router YAML is malformed")]
    MalformedYaml {
        #[source]
        source: serde_yaml_ng::Error,
    },
    #[error("{0}")]
    Invalid(&'static str),
    #[error("the Router configuration exceeds its resource bound")]
    ResourceExhausted,
}

#[derive(Clone, Copy, Debug, Error)]
enum RouterExecutionError {
    #[error("the Router condition could not be rendered")]
    Template,
}
