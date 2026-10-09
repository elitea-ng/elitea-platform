//! Bounded implementation of the active pipeline `state_modifier` node.

use std::collections::{BTreeMap, BTreeSet};
use std::io;

use adk_graph::{GraphError, Node, NodeContext, NodeOutput};
use async_trait::async_trait;
use base64::Engine as _;
use minijinja::value::Value as JinjaValue;
use minijinja::{Environment, Error as JinjaError, ErrorKind, UndefinedBehavior};
use regex::Regex;
use ring::digest;
use serde::Deserialize;
use serde_json::{Value, json};
use thiserror::Error;

use super::yaml::{valid_graph_id, valid_output_key};

const MAX_NODE_YAML_BYTES: usize = 64 * 1024;
const MAX_TEMPLATE_BYTES: usize = 64 * 1024;
// The rendered value becomes a browser-facing `NodeEventV1`; 8 KiB leaves
// deterministic room for JSON escaping and the event envelope under 64 KiB.
const MAX_RENDERED_BYTES: usize = 8 * 1024;
const MAX_VARIABLES: usize = 64;
const MAX_REGEX_BYTES: usize = 4 * 1024;
const TEMPLATE_FUEL: u64 = 250_000;
const CONFIG_DIGEST_DOMAIN: &[u8] = b"elitea.graph.state_modifier.config.v1\0";

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawStateModifierNodeDefinition {
    id: String,
    #[serde(rename = "type")]
    node_type: String,
    #[serde(default)]
    template: String,
    #[serde(default)]
    variables_to_clean: Vec<String>,
    #[serde(default)]
    input: Vec<String>,
    #[serde(default)]
    output: Vec<String>,
    #[serde(default)]
    transition: Option<String>,
}

/// Strict, authority-free YAML definition for one state transformation.
#[derive(Clone)]
pub struct StateModifierNodeDefinition {
    id: String,
    template: String,
    variables_to_clean: Vec<String>,
    input: Vec<String>,
    output: Vec<String>,
    transition: Option<String>,
}

impl StateModifierNodeDefinition {
    pub fn from_yaml(yaml: &str) -> Result<Self, StateModifierConfigurationError> {
        if yaml.is_empty() || yaml.len() > MAX_NODE_YAML_BYTES {
            return Err(StateModifierConfigurationError::ResourceExhausted);
        }
        let raw = serde_yaml_ng::from_str::<RawStateModifierNodeDefinition>(yaml)
            .map_err(|source| StateModifierConfigurationError::MalformedYaml { source })?;
        Self::from_raw(raw)
    }

    fn from_raw(
        mut raw: RawStateModifierNodeDefinition,
    ) -> Result<Self, StateModifierConfigurationError> {
        if raw.node_type != "state_modifier" {
            return Err(StateModifierConfigurationError::Invalid(
                "the node type must be state_modifier",
            ));
        }
        if !valid_graph_id(&raw.id) {
            return Err(StateModifierConfigurationError::Invalid(
                "the state modifier node ID is malformed",
            ));
        }
        if raw.template.len() > MAX_TEMPLATE_BYTES {
            return Err(StateModifierConfigurationError::ResourceExhausted);
        }
        if raw.input.len() > MAX_VARIABLES
            || raw.output.len() > MAX_VARIABLES
            || raw.variables_to_clean.len() > MAX_VARIABLES
        {
            return Err(StateModifierConfigurationError::ResourceExhausted);
        }
        if raw.input.is_empty() {
            raw.input.push("messages".to_owned());
        }
        for key in raw
            .input
            .iter()
            .chain(&raw.output)
            .chain(&raw.variables_to_clean)
        {
            if !valid_output_key(key) {
                return Err(StateModifierConfigurationError::Invalid(
                    "a state modifier variable is malformed",
                ));
            }
        }
        if raw
            .transition
            .as_deref()
            .is_some_and(|target| target != "END" && !valid_graph_id(target))
        {
            return Err(StateModifierConfigurationError::Invalid(
                "the state modifier transition is malformed",
            ));
        }
        validate_unique(&raw.input)?;
        validate_unique(&raw.output)?;
        validate_unique(&raw.variables_to_clean)?;

        Ok(Self {
            id: raw.id,
            template: raw.template,
            variables_to_clean: raw.variables_to_clean,
            input: raw.input,
            output: raw.output,
            transition: raw.transition,
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

    #[must_use]
    pub fn output_keys(&self) -> &[String] {
        &self.output
    }

    #[must_use]
    pub fn variables_to_clean(&self) -> &[String] {
        &self.variables_to_clean
    }

    #[must_use]
    pub fn transition(&self) -> Option<&str> {
        self.transition.as_deref()
    }

    #[must_use]
    pub fn config_digest(&self) -> [u8; 32] {
        let mut context = digest::Context::new(&digest::SHA256);
        context.update(CONFIG_DIGEST_DOMAIN);
        digest_field(&mut context, self.id.as_bytes());
        digest_field(&mut context, self.template.as_bytes());
        for key in &self.input {
            digest_field(&mut context, key.as_bytes());
        }
        for key in &self.output {
            digest_field(&mut context, key.as_bytes());
        }
        for key in &self.variables_to_clean {
            digest_field(&mut context, key.as_bytes());
        }
        digest_field(
            &mut context,
            self.transition.as_deref().unwrap_or_default().as_bytes(),
        );
        copy_digest(context.finish().as_ref())
    }
}

fn validate_unique(values: &[String]) -> Result<(), StateModifierConfigurationError> {
    let mut seen = BTreeSet::new();
    if values.iter().any(|value| !seen.insert(value)) {
        return Err(StateModifierConfigurationError::Invalid(
            "state modifier variables must be unique within each field",
        ));
    }
    Ok(())
}

/// ADK graph node that renders a bounded template and emits state updates.
pub struct StateModifierNode {
    definition: StateModifierNodeDefinition,
}

impl StateModifierNode {
    #[must_use]
    pub const fn new(definition: StateModifierNodeDefinition) -> Self {
        Self { definition }
    }
}

#[async_trait]
impl Node for StateModifierNode {
    fn name(&self) -> &str {
        self.definition.id()
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let mut input = BTreeMap::new();
        for key in self.definition.input_keys() {
            if let Some(value) = context.get(key) {
                input.insert(key.clone(), value.clone());
            }
        }
        let rendered = render_template(&self.definition.template, &input).map_err(|_| {
            GraphError::Other("state modifier template evaluation failed".to_owned())
        })?;

        let mut output = NodeOutput::new();
        if let Some(key) = self.definition.output_keys().first() {
            let projected = project_rendered(context.get(key), rendered);
            ensure_bounded_value(&projected).map_err(|_| {
                GraphError::Other("state modifier output exceeds its resource bound".to_owned())
            })?;
            output = output.with_update(key, projected);
        }
        for key in self.definition.variables_to_clean() {
            if let Some(value) = context.get(key)
                && let Some(cleaned) = cleaned_value(value)
            {
                output = output.with_update(key, cleaned);
            }
        }
        Ok(output)
    }
}

fn render_template(
    source: &str,
    input: &BTreeMap<String, Value>,
) -> Result<String, StateModifierExecutionError> {
    let mut environment = Environment::new();
    environment.set_undefined_behavior(UndefinedBehavior::Lenient);
    environment.set_fuel(Some(TEMPLATE_FUEL));
    environment.add_filter("from_json", from_json_filter);
    environment.add_filter("base64_to_string", base64_to_string_filter);
    environment.add_filter("split_by_words", split_by_words_filter);
    environment.add_filter("split_by_regex", split_by_regex_filter);
    let template = environment
        .template_from_str(source)
        .map_err(|_| StateModifierExecutionError::Template)?;
    let mut writer = BoundedWriter::new(MAX_RENDERED_BYTES);
    template
        .render_captured_to(
            input
                .iter()
                .map(|(key, value)| (key.as_str(), template_value(value)))
                .collect::<BTreeMap<_, _>>(),
            &mut writer,
        )
        .map_err(|_| StateModifierExecutionError::Template)?;
    String::from_utf8(writer.into_inner()).map_err(|_| StateModifierExecutionError::Template)
}

// serde_json arbitrary-precision numbers serialize as private maps. Convert
// numeric primitives explicitly so templates receive numbers instead of maps.
pub fn template_value(value: &Value) -> JinjaValue {
    match value {
        Value::Null => JinjaValue::from(()),
        Value::Bool(value) => JinjaValue::from(*value),
        Value::String(value) => JinjaValue::from(value.as_str()),
        Value::Number(value) => {
            if let Some(number) = value.as_i64() {
                JinjaValue::from(number)
            } else if let Some(number) = value.as_u64() {
                JinjaValue::from(number)
            } else if let Some(number) = value.as_f64().filter(|number| number.is_finite()) {
                JinjaValue::from(number)
            } else {
                JinjaValue::from(value.to_string())
            }
        }
        Value::Array(values) => values.iter().map(template_value).collect(),
        Value::Object(values) => values
            .iter()
            .map(|(key, value)| (JinjaValue::from(key.as_str()), template_value(value)))
            .collect(),
    }
}

fn from_json_filter(value: JinjaValue) -> JinjaValue {
    let Some(text) = value.as_str() else {
        return value;
    };
    serde_json::from_str::<Value>(text)
        .map(|value| template_value(&value))
        .unwrap_or(value)
}

fn base64_to_string_filter(value: JinjaValue) -> JinjaValue {
    let Some(text) = value.as_str() else {
        return value;
    };
    base64::engine::general_purpose::STANDARD
        .decode(text)
        .ok()
        .and_then(|decoded| String::from_utf8(decoded).ok())
        .map(JinjaValue::from)
        .unwrap_or(value)
}

fn split_by_words_filter(
    value: &str,
    chunk_size: Option<usize>,
) -> Result<Vec<String>, JinjaError> {
    let chunk_size = chunk_size.unwrap_or(100);
    if chunk_size == 0 || chunk_size > MAX_RENDERED_BYTES {
        return Err(JinjaError::new(
            ErrorKind::InvalidOperation,
            "split_by_words chunk size is outside its bound",
        ));
    }
    let words = value.split_whitespace().collect::<Vec<_>>();
    Ok(words
        .chunks(chunk_size)
        .map(|chunk| chunk.join(" "))
        .collect())
}

fn split_by_regex_filter(value: &str, pattern: &str) -> Result<Vec<String>, JinjaError> {
    if pattern.is_empty() || pattern.len() > MAX_REGEX_BYTES {
        return Err(JinjaError::new(
            ErrorKind::InvalidOperation,
            "split_by_regex pattern is outside its bound",
        ));
    }
    let regex = Regex::new(pattern).map_err(|_| {
        JinjaError::new(
            ErrorKind::InvalidOperation,
            "split_by_regex pattern is invalid",
        )
    })?;
    Ok(regex.split(value).map(ToOwned::to_owned).collect())
}

fn project_rendered(existing: Option<&Value>, rendered: String) -> Value {
    match existing {
        Some(Value::Object(_)) => serde_json::from_str::<Value>(&rendered)
            .ok()
            .filter(Value::is_object)
            .unwrap_or(Value::String(rendered)),
        Some(Value::Array(_)) => serde_json::from_str::<Value>(&rendered)
            .ok()
            .filter(Value::is_array)
            .unwrap_or(Value::String(rendered)),
        Some(Value::Number(number)) if number.is_i64() || number.is_u64() => rendered
            .parse::<i64>()
            .map_or_else(|_| Value::String(rendered), Value::from),
        Some(Value::Number(_)) => rendered
            .parse::<f64>()
            .ok()
            .filter(|value| value.is_finite())
            .map_or_else(|| Value::String(rendered), Value::from),
        Some(Value::Bool(_)) => Value::Bool(matches!(
            rendered.to_ascii_lowercase().as_str(),
            "true" | "1" | "yes" | "on"
        )),
        Some(Value::Null) => Value::Null,
        Some(Value::String(_)) | None => Value::String(rendered),
    }
}

fn cleaned_value(value: &Value) -> Option<Value> {
    match value {
        Value::Array(_) => Some(json!([])),
        Value::Object(_) => Some(json!({})),
        Value::String(_) => Some(Value::String(String::new())),
        Value::Number(_) => Some(json!(0)),
        Value::Bool(_) => Some(Value::Bool(false)),
        Value::Null => None,
    }
}

fn ensure_bounded_value(value: &Value) -> Result<(), StateModifierExecutionError> {
    let bytes = serde_json::to_vec(value).map_err(|_| StateModifierExecutionError::Output)?;
    if bytes.len() > MAX_RENDERED_BYTES {
        return Err(StateModifierExecutionError::Output);
    }
    Ok(())
}

struct BoundedWriter {
    bytes: Vec<u8>,
    limit: usize,
}

impl BoundedWriter {
    fn new(limit: usize) -> Self {
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
            .ok_or_else(|| io::Error::other("template output exceeds its resource bound"))?;
        if next > self.limit {
            return Err(io::Error::other(
                "template output exceeds its resource bound",
            ));
        }
        self.bytes.extend_from_slice(buffer);
        Ok(buffer.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

#[derive(Debug, Error)]
pub enum StateModifierConfigurationError {
    #[error("the state modifier node exceeds its resource bound")]
    ResourceExhausted,
    #[error("the state modifier YAML is malformed")]
    MalformedYaml {
        #[source]
        source: serde_yaml_ng::Error,
    },
    #[error("the state modifier node is invalid: {0}")]
    Invalid(&'static str),
}

#[derive(Debug, Error)]
enum StateModifierExecutionError {
    #[error("state modifier template evaluation failed")]
    Template,
    #[error("state modifier output exceeds its resource bound")]
    Output,
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
