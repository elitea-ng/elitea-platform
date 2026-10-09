//! Native implementation of the active pipeline `printer` node.
//!
//! A Printer is a public-output checkpoint, not a model turn and not a HITL
//! decision card. The graph executes the node, checkpoints its bounded output,
//! and uses ADK's native `interrupt_after` support. A later ordinary user
//! message resumes the same checkpoint at the compiler-owned reset node.

use std::collections::BTreeMap;

use adk_graph::{GraphError, Node, NodeContext, NodeOutput, State};
use async_trait::async_trait;
use ring::digest;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use thiserror::Error;

use super::PIPELINE_YAML_BUDGET;
use super::yaml::{valid_graph_id, valid_output_key};
use crate::bounded_yaml;

const MAX_NODE_YAML_BYTES: usize = 64 * 1024;
const MAX_MAPPING_VALUE_BYTES: usize = 8 * 1024;
const MAX_FINAL_MESSAGE_BYTES: usize = 4 * 1024;
pub const MAX_PRINTER_OUTPUT_BYTES: usize = 8 * 1024;
const CONFIG_DIGEST_DOMAIN: &[u8] = b"elitea.graph.printer.config.v1\0";
pub const PRINTER_COMPLETED_STATE: &str = "PRINTER_COMPLETED";
pub const PRINTER_OUTPUT_STATE_KEY: &str = "printer_output";
pub const PRINTER_PAUSE_METADATA_KEY: &str = "elitea.pipeline.printer_pause";
pub const PRINTER_PAUSE_SCHEMA: &str = "elitea.pipeline.printer-pause.v1";
pub const DEFAULT_FINAL_MESSAGE: &str = "How to proceed? To resume the pipeline - type anything...";

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawPrinterNodeDefinition {
    id: String,
    #[serde(rename = "type")]
    node_type: String,
    #[serde(default)]
    input_mapping: BTreeMap<String, RawPrinterInputMapping>,
    #[serde(default)]
    final_message: Option<String>,
    transition: String,
}

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawPrinterInputMapping {
    #[serde(rename = "type")]
    kind: String,
    value: Value,
    #[serde(default)]
    source: Option<String>,
}

#[derive(Clone)]
pub enum PrinterInputMapping {
    Fixed(Value),
    Variable(String),
    Template(String),
}

/// Strict, authority-free definition for one Printer checkpoint.
#[derive(Clone)]
pub struct PrinterNodeDefinition {
    id: String,
    mapping: PrinterInputMapping,
    final_message: String,
    transition: String,
}

impl PrinterNodeDefinition {
    pub fn from_yaml(yaml: &str) -> Result<Self, PrinterConfigurationError> {
        if yaml.is_empty() || yaml.len() > MAX_NODE_YAML_BYTES {
            return Err(PrinterConfigurationError::ResourceExhausted);
        }
        let raw = bounded_yaml::from_str_as_yaml_error::<RawPrinterNodeDefinition>(
            yaml,
            PIPELINE_YAML_BUDGET,
        )
        .map_err(|source| PrinterConfigurationError::MalformedYaml { source })?;
        Self::from_raw(raw)
    }

    fn from_raw(mut raw: RawPrinterNodeDefinition) -> Result<Self, PrinterConfigurationError> {
        if raw.node_type != "printer" {
            return Err(PrinterConfigurationError::Invalid(
                "the node type must be printer",
            ));
        }
        if !valid_graph_id(&raw.id) {
            return Err(PrinterConfigurationError::Invalid(
                "the Printer node ID is malformed",
            ));
        }
        if raw.transition != "END" && !valid_graph_id(&raw.transition) {
            return Err(PrinterConfigurationError::Invalid(
                "the Printer transition is malformed",
            ));
        }
        if raw.input_mapping.is_empty() {
            raw.input_mapping.insert(
                "printer".to_owned(),
                RawPrinterInputMapping {
                    kind: "fixed".to_owned(),
                    value: Value::String(String::new()),
                    source: None,
                },
            );
        }
        if raw.input_mapping.len() != 1 || !raw.input_mapping.contains_key("printer") {
            return Err(PrinterConfigurationError::Invalid(
                "the Printer input mapping must contain only printer",
            ));
        }
        let raw_mapping =
            raw.input_mapping
                .remove("printer")
                .ok_or(PrinterConfigurationError::Invalid(
                    "the Printer input mapping is malformed",
                ))?;
        if raw_mapping
            .source
            .as_deref()
            .is_some_and(|source| source != "state")
        {
            return Err(PrinterConfigurationError::Unsupported(
                "the Printer input source is not supported",
            ));
        }
        let mapping = match raw_mapping.kind.as_str() {
            "fixed" => {
                ensure_bounded_value(&raw_mapping.value)?;
                PrinterInputMapping::Fixed(raw_mapping.value)
            }
            "variable" => {
                let key = bounded_text(&raw_mapping.value)?;
                if !valid_output_key(key) {
                    return Err(PrinterConfigurationError::Invalid(
                        "the Printer variable mapping is malformed",
                    ));
                }
                PrinterInputMapping::Variable(key.to_owned())
            }
            "fstring" => {
                PrinterInputMapping::Template(bounded_text(&raw_mapping.value)?.to_owned())
            }
            _ => {
                return Err(PrinterConfigurationError::Unsupported(
                    "the Printer input mapping type is not supported",
                ));
            }
        };
        let final_message = raw
            .final_message
            .as_deref()
            .map(str::trim)
            .filter(|value| !value.is_empty())
            .unwrap_or(DEFAULT_FINAL_MESSAGE)
            .to_owned();
        validate_public_text::<PrinterConfigurationError>(&final_message, MAX_FINAL_MESSAGE_BYTES)?;
        Ok(Self {
            id: raw.id,
            mapping,
            final_message,
            transition: raw.transition,
        })
    }

    #[must_use]
    pub fn id(&self) -> &str {
        &self.id
    }

    #[must_use]
    pub fn mapping(&self) -> &PrinterInputMapping {
        &self.mapping
    }

    #[must_use]
    pub fn final_message(&self) -> &str {
        &self.final_message
    }

    #[must_use]
    pub fn transition(&self) -> &str {
        &self.transition
    }

    #[must_use]
    pub fn reset_node_id(&self) -> String {
        format!("{}_reset", self.id)
    }

    #[must_use]
    pub fn config_digest(&self) -> [u8; 32] {
        let mut context = digest::Context::new(&digest::SHA256);
        context.update(CONFIG_DIGEST_DOMAIN);
        digest_field(&mut context, self.id.as_bytes());
        match &self.mapping {
            PrinterInputMapping::Fixed(value) => {
                digest_field(&mut context, b"fixed");
                // Sorted members whatever serde_json's features are (crate::canonical).
                digest_field(
                    &mut context,
                    &crate::canonical::to_vec(value).unwrap_or_default(),
                );
            }
            PrinterInputMapping::Variable(value) => {
                digest_field(&mut context, b"variable");
                digest_field(&mut context, value.as_bytes());
            }
            PrinterInputMapping::Template(value) => {
                digest_field(&mut context, b"fstring");
                digest_field(&mut context, value.as_bytes());
            }
        }
        digest_field(&mut context, self.final_message.as_bytes());
        digest_field(&mut context, self.transition.as_bytes());
        copy_digest(context.finish().as_ref())
    }
}

/// Public metadata accompanying one native static Printer interruption.
#[derive(Clone, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct PrinterPauseMetadata {
    pub schema: String,
    pub node_name: String,
    pub reset_node_name: String,
    pub definition_digest: String,
    pub node_digest: String,
}

impl PrinterPauseMetadata {
    #[must_use]
    pub fn validate(&self) -> bool {
        self.schema == PRINTER_PAUSE_SCHEMA
            && valid_graph_id(&self.node_name)
            && self.reset_node_name == format!("{}_reset", self.node_name)
            && valid_sha256_label(&self.definition_digest)
            && valid_sha256_label(&self.node_digest)
    }
}

/// Immutable catalog used to enrich only compiler-owned Printer interruptions.
#[derive(Clone, Default)]
pub struct PrinterPauseCatalog {
    entries: BTreeMap<String, PrinterPauseMetadata>,
}

impl PrinterPauseCatalog {
    pub fn from_definition(
        definition_digest: [u8; 32],
        nodes: impl Iterator<Item = PrinterNodeDefinition>,
    ) -> Self {
        let definition_digest = sha256_label(&definition_digest);
        let entries = nodes
            .map(|node| {
                let metadata = PrinterPauseMetadata {
                    schema: PRINTER_PAUSE_SCHEMA.to_owned(),
                    node_name: node.id().to_owned(),
                    reset_node_name: node.reset_node_id(),
                    definition_digest: definition_digest.clone(),
                    node_digest: sha256_label(&node.config_digest()),
                };
                (node.id().to_owned(), metadata)
            })
            .collect();
        Self { entries }
    }

    #[must_use]
    pub fn get(&self, node: &str) -> Option<&PrinterPauseMetadata> {
        self.entries.get(node)
    }

    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    #[must_use]
    pub fn contains_exact(&self, metadata: &PrinterPauseMetadata) -> bool {
        self.entries
            .get(&metadata.node_name)
            .is_some_and(|expected| expected == metadata)
    }
}

pub struct PrinterNode {
    definition: PrinterNodeDefinition,
}

impl PrinterNode {
    #[must_use]
    pub const fn new(definition: PrinterNodeDefinition) -> Self {
        Self { definition }
    }
}

#[async_trait]
impl Node for PrinterNode {
    fn name(&self) -> &str {
        self.definition.id()
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let value =
            resolve_mapping(self.definition.mapping(), &context.state).map_err(|error| {
                GraphError::Other(format!("Printer input projection failed: {}", error.code()))
            })?;
        if value == PRINTER_COMPLETED_STATE {
            return Err(GraphError::Other(
                "Printer output uses a reserved runtime value".to_owned(),
            ));
        }
        let mut output = value;
        output.push_str("\n\n-----\n*");
        output.push_str(self.definition.final_message());
        output.push('*');
        validate_public_text::<PrinterExecutionError>(&output, MAX_PRINTER_OUTPUT_BYTES).map_err(
            |error| {
                GraphError::Other(format!(
                    "Printer output projection failed: {}",
                    error.code()
                ))
            },
        )?;
        Ok(NodeOutput::new().with_update(PRINTER_OUTPUT_STATE_KEY, json!(output)))
    }
}

pub struct PrinterResetNode {
    id: String,
}

impl PrinterResetNode {
    #[must_use]
    pub const fn new(id: String) -> Self {
        Self { id }
    }
}

#[async_trait]
impl Node for PrinterResetNode {
    fn name(&self) -> &str {
        &self.id
    }

    async fn execute(&self, _context: &NodeContext) -> Result<NodeOutput, GraphError> {
        Ok(NodeOutput::new().with_update(PRINTER_OUTPUT_STATE_KEY, json!(PRINTER_COMPLETED_STATE)))
    }
}

fn resolve_mapping(
    mapping: &PrinterInputMapping,
    state: &State,
) -> Result<String, PrinterExecutionError> {
    let value = match mapping {
        PrinterInputMapping::Fixed(value) => value.clone(),
        PrinterInputMapping::Variable(key) => state.get(key).cloned().unwrap_or(Value::Null),
        PrinterInputMapping::Template(template) => Value::String(render_template(template, state)?),
    };
    let output = match value {
        Value::Array(values) => values
            .iter()
            .map(pythonish_value)
            .collect::<Result<Vec<_>, _>>()?
            .join(", "),
        value => pythonish_value(&value)?,
    };
    validate_public_text::<PrinterExecutionError>(&output, MAX_PRINTER_OUTPUT_BYTES)?;
    Ok(output)
}

fn pythonish_value(value: &Value) -> Result<String, PrinterExecutionError> {
    Ok(match value {
        Value::Null => "None".to_owned(),
        Value::Bool(true) => "True".to_owned(),
        Value::Bool(false) => "False".to_owned(),
        Value::String(value) => value.clone(),
        value => {
            crate::canonical::to_string(value).map_err(|_| PrinterExecutionError::InvalidOutput)?
        }
    })
}

fn render_template(template: &str, state: &State) -> Result<String, PrinterExecutionError> {
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
                rendered.push_str(&pythonish_value(value)?);
            } else {
                rendered.push_str(&template[open..=end]);
            }
            cursor = end + 1;
        } else {
            rendered.push('{');
            cursor = open + 1;
        }
        if rendered.len() > MAX_MAPPING_VALUE_BYTES {
            return Err(PrinterExecutionError::ResourceExhausted);
        }
    }
    if cursor < template.len() {
        rendered.push_str(&template[cursor..]);
    }
    validate_public_text::<PrinterExecutionError>(&rendered, MAX_MAPPING_VALUE_BYTES)?;
    Ok(rendered)
}

fn bounded_text(value: &Value) -> Result<&str, PrinterConfigurationError> {
    value
        .as_str()
        .filter(|value| value.len() <= MAX_MAPPING_VALUE_BYTES && !value.contains('\0'))
        .ok_or(PrinterConfigurationError::Invalid(
            "the Printer input mapping value is malformed",
        ))
}

fn ensure_bounded_value(value: &Value) -> Result<(), PrinterConfigurationError> {
    let encoded = serde_json::to_vec(value).map_err(|_| {
        PrinterConfigurationError::Invalid("the Printer fixed input mapping is malformed")
    })?;
    if encoded.len() > MAX_MAPPING_VALUE_BYTES {
        return Err(PrinterConfigurationError::ResourceExhausted);
    }
    Ok(())
}

fn validate_public_text<E>(value: &str, max_bytes: usize) -> Result<(), E>
where
    E: From<PrinterExecutionError>,
{
    if value.len() > max_bytes {
        return Err(PrinterExecutionError::ResourceExhausted.into());
    }
    if value.chars().any(|character| {
        character == '\0' || (character.is_control() && !matches!(character, '\n' | '\r' | '\t'))
    }) {
        return Err(PrinterExecutionError::InvalidOutput.into());
    }
    Ok(())
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

fn sha256_label(value: &[u8; 32]) -> String {
    format!("sha256:{}", hex(value))
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

fn valid_sha256_label(value: &str) -> bool {
    value.len() == 71
        && value.starts_with("sha256:")
        && value[7..].bytes().all(|byte| byte.is_ascii_hexdigit())
}

#[derive(Debug, Error)]
pub enum PrinterConfigurationError {
    #[error("malformed Printer YAML")]
    MalformedYaml {
        #[source]
        source: serde_yaml_ng::Error,
    },
    #[error("{0}")]
    Invalid(&'static str),
    #[error("{0}")]
    Unsupported(&'static str),
    #[error("the Printer configuration exceeds its resource bound")]
    ResourceExhausted,
}

impl From<PrinterExecutionError> for PrinterConfigurationError {
    fn from(error: PrinterExecutionError) -> Self {
        match error {
            PrinterExecutionError::ResourceExhausted => Self::ResourceExhausted,
            PrinterExecutionError::InvalidOutput => {
                Self::Invalid("the Printer public text is malformed")
            }
        }
    }
}

#[derive(Clone, Copy, Debug, Error)]
pub enum PrinterExecutionError {
    #[error("the Printer output is malformed")]
    InvalidOutput,
    #[error("the Printer output exceeds its resource bound")]
    ResourceExhausted,
}

impl PrinterExecutionError {
    const fn code(self) -> &'static str {
        match self {
            Self::InvalidOutput => "graph.printer.invalid_output",
            Self::ResourceExhausted => "graph.printer.resource_exhausted",
        }
    }
}

/// Order-explicit digests and rendering (ADR-0029 decision 2). The pinned
/// values are what the worker produced before the move, when `serde_json` was
/// always built without `preserve_order`; `--features test-preserve-order`
/// runs the same assertions with it.
#[cfg(test)]
mod order_tests {
    use serde_json::Value;

    use super::{PrinterNodeDefinition, hex, pythonish_value};

    // Members deliberately out of order at two depths.
    const UNORDERED_FIXED: &str = "id: show\ntype: printer\ninput_mapping:\n  printer:\n    type: fixed\n    value: {zeta: {b: 2, a: [1, {y: true, x: null}]}, alpha: one}\ntransition: END\n";

    #[test]
    fn fixed_mapping_digest_is_independent_of_map_order() {
        let definition = PrinterNodeDefinition::from_yaml(UNORDERED_FIXED).expect("definition");
        assert_eq!(
            hex(&definition.config_digest()),
            "31533601b5e9a1cb3317b833bba661da58277da4e1a9b1a35c959be03a0666fd"
        );
    }

    #[test]
    fn rendered_objects_list_members_in_key_order() {
        let value: Value =
            serde_json::from_str(r#"{"zeta":{"b":2,"a":1},"alpha":"one"}"#).expect("fixture");
        assert_eq!(
            pythonish_value(&value).expect("render"),
            r#"{"alpha":"one","zeta":{"a":1,"b":2}}"#
        );
    }
}
