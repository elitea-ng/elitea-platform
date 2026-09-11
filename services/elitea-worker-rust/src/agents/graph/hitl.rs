//! Elitea pipeline HITL node on ADK-Rust dynamic graph interrupts.
//!
//! The browser decision is not read directly here. The future durable interrupt
//! owner must validate a one-use decision against the claim, graph checkpoint
//! and definition digest, then place it on the compiler-owned resume channel.

#![allow(dead_code)] // The stored-pipeline compiler remains capability-gated.

use std::collections::{BTreeSet, HashMap};

use adk_rust::graph::{END, GraphError, Node, NodeContext, NodeOutput};
use async_trait::async_trait;
use ring::digest;
use serde::Deserialize;
use serde_json::{Map, Value, json};
use thiserror::Error;

use super::yaml::{valid_graph_id, valid_output_key};

const MAX_YAML_NODE_BYTES: usize = 64 * 1024;
const MAX_INPUT_KEYS: usize = 64;
const MAX_MESSAGE_TEMPLATE_BYTES: usize = 8 * 1024;
const MAX_RENDERED_MESSAGE_BYTES: usize = 8 * 1024;
const MAX_EDIT_VALUE_BYTES: usize = 64 * 1024;
const HITL_CONFIG_DIGEST_DOMAIN: &[u8] = b"elitea.graph.hitl.config.v1\0";
const HITL_INTERRUPT_SCHEMA: &str = "elitea.graph.hitl-interrupt.v1";
const PIPELINE_HITL_INTERACTION_TYPE: &str = "pipeline_hitl_node";
const PIPELINE_HITL_HISTORY_CONTRACT_VERSION: u8 = 1;
pub(crate) const HITL_RESUME_STATE_KEY: &str = "__elitea_hitl_resume_v1";

#[derive(Clone, Copy, Debug, Deserialize, Eq, Ord, PartialEq, PartialOrd)]
#[serde(rename_all = "snake_case")]
pub(crate) enum HitlAction {
    Approve,
    Reject,
    Edit,
}

impl HitlAction {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Approve => "approve",
            Self::Reject => "reject",
            Self::Edit => "edit",
        }
    }
}

#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
pub(crate) enum HitlMessageKind {
    #[default]
    Fixed,
    Variable,
    Fstring,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
struct HitlMessageDefinition {
    #[serde(rename = "type", default)]
    kind: HitlMessageKind,
    #[serde(default = "default_message")]
    value: String,
}

impl Default for HitlMessageDefinition {
    fn default() -> Self {
        Self {
            kind: HitlMessageKind::Fixed,
            value: default_message(),
        }
    }
}

#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
struct HitlRoutes {
    #[serde(default)]
    approve: Option<String>,
    #[serde(default)]
    reject: Option<String>,
    #[serde(default)]
    edit: Option<String>,
}

impl HitlRoutes {
    fn get(&self, action: HitlAction) -> Option<&str> {
        match action {
            HitlAction::Approve => self.approve.as_deref(),
            HitlAction::Reject => self.reject.as_deref(),
            HitlAction::Edit => self.edit.as_deref(),
        }
    }

    fn iter(&self) -> impl Iterator<Item = (HitlAction, &str)> {
        [HitlAction::Approve, HitlAction::Reject, HitlAction::Edit]
            .into_iter()
            .filter_map(|action| self.get(action).map(|target| (action, target)))
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawHitlNodeDefinition {
    id: String,
    #[serde(rename = "type")]
    node_type: String,
    #[serde(default = "default_inputs")]
    input: Vec<String>,
    #[serde(default)]
    user_message: HitlMessageDefinition,
    #[serde(default)]
    routes: HitlRoutes,
    #[serde(default)]
    edit_state_key: Option<String>,
}

/// Strict, bounded source contract for one stored pipeline `type: hitl` node.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct HitlNodeDefinition {
    id: String,
    input: Vec<String>,
    user_message: HitlMessageDefinition,
    routes: HitlRoutes,
    edit_state_key: Option<String>,
    config_digest: [u8; 32],
}

impl HitlNodeDefinition {
    /// Parse the single-node YAML mapping selected by the pipeline compiler.
    pub(crate) fn from_yaml(yaml: &str) -> Result<Self, HitlConfigurationError> {
        if yaml.is_empty() || yaml.len() > MAX_YAML_NODE_BYTES {
            return Err(HitlConfigurationError::ResourceExhausted);
        }
        let mut raw = serde_yaml_ng::from_str::<RawHitlNodeDefinition>(yaml)
            .map_err(|source| HitlConfigurationError::MalformedYaml { source })?;
        if raw.input.is_empty() {
            raw.input = default_inputs();
        }
        Self::from_raw(raw)
    }

    fn from_raw(raw: RawHitlNodeDefinition) -> Result<Self, HitlConfigurationError> {
        if raw.node_type != "hitl" {
            return Err(HitlConfigurationError::Invalid(
                "the node type must be hitl",
            ));
        }
        if !valid_graph_id(&raw.id) {
            return Err(HitlConfigurationError::Invalid(
                "the HITL node ID is malformed",
            ));
        }
        validate_inputs(&raw.input)?;
        validate_message(&raw.user_message)?;
        validate_routes(&raw.routes, raw.edit_state_key.as_deref())?;
        if raw
            .edit_state_key
            .as_deref()
            .is_some_and(|key| !valid_output_key(key) || key == HITL_RESUME_STATE_KEY)
        {
            return Err(HitlConfigurationError::Invalid(
                "the HITL edit state key is malformed",
            ));
        }
        let config_digest = config_digest(&raw);
        Ok(Self {
            id: raw.id,
            input: raw.input,
            user_message: raw.user_message,
            routes: raw.routes,
            edit_state_key: raw.edit_state_key,
            config_digest,
        })
    }

    #[must_use]
    pub(crate) fn id(&self) -> &str {
        &self.id
    }

    #[must_use]
    pub(crate) fn input_keys(&self) -> &[String] {
        &self.input
    }

    #[must_use]
    pub(crate) const fn message_kind(&self) -> HitlMessageKind {
        self.user_message.kind
    }

    #[must_use]
    pub(crate) fn message_template(&self) -> &str {
        &self.user_message.value
    }

    #[must_use]
    pub(crate) fn edit_state_key(&self) -> Option<&str> {
        self.edit_state_key.as_deref()
    }

    #[must_use]
    pub(crate) fn available_actions(&self) -> Vec<HitlAction> {
        [HitlAction::Approve, HitlAction::Reject, HitlAction::Edit]
            .into_iter()
            .filter(|action| self.action_is_available(*action))
            .collect()
    }

    /// Configured successors used by the whole-document compiler.
    pub(crate) fn route_targets(&self) -> impl Iterator<Item = &str> {
        self.routes.iter().map(|(_, target)| target)
    }

    /// Stable node-definition digest used by the pipeline definition lineage.
    pub(crate) const fn config_digest(&self) -> [u8; 32] {
        self.config_digest
    }

    fn action_is_available(&self, action: HitlAction) -> bool {
        match action {
            HitlAction::Edit => {
                self.edit_state_key.is_some()
                    && self
                        .routes
                        .edit
                        .as_deref()
                        .is_some_and(|target| target != "END")
            }
            _ => self.routes.get(action).is_some(),
        }
    }

    pub(super) fn route(&self, action: HitlAction) -> Option<&str> {
        self.action_is_available(action)
            .then(|| self.routes.get(action))
            .flatten()
    }

    fn digest_label(&self) -> String {
        format!("sha256:{}", hex(&self.config_digest))
    }

    fn interrupt_data(&self, message: &str) -> Value {
        let routes = self
            .routes
            .iter()
            .map(|(action, target)| (action.as_str().to_owned(), Value::String(target.to_owned())))
            .collect::<Map<_, _>>();
        json!({
            "schema_revision": HITL_INTERRUPT_SCHEMA,
            "type": "hitl",
            "interaction_type": PIPELINE_HITL_INTERACTION_TYPE,
            "history_contract_version": PIPELINE_HITL_HISTORY_CONTRACT_VERSION,
            "guardrail_type": "pipeline_hitl",
            "node_name": self.id,
            "message": message,
            "available_actions": self
                .available_actions()
                .into_iter()
                .map(HitlAction::as_str)
                .collect::<Vec<_>>(),
            "routes": routes,
            "edit_state_key": self.edit_state_key,
            "definition_digest": self.digest_label(),
        })
    }
}

/// ADK-Rust node that pauses and later performs one authorized route decision.
pub(crate) struct HitlNode {
    definition: HitlNodeDefinition,
}

impl HitlNode {
    #[must_use]
    pub(crate) const fn new(definition: HitlNodeDefinition) -> Self {
        Self { definition }
    }

    fn message(&self, state: &HashMap<String, Value>) -> Result<String, GraphError> {
        let message = match self.definition.user_message.kind {
            HitlMessageKind::Fixed => self.definition.user_message.value.clone(),
            HitlMessageKind::Variable => {
                state.get(&self.definition.user_message.value).map_or_else(
                    || self.definition.user_message.value.clone(),
                    display_state_value,
                )
            }
            HitlMessageKind::Fstring => render_template(
                &self.definition.user_message.value,
                &self.definition.input,
                state,
            )?,
        };
        if message.is_empty()
            || message.len() > MAX_RENDERED_MESSAGE_BYTES
            || message.contains('\0')
        {
            return Err(self.execution_error("graph.hitl.message_invalid"));
        }
        Ok(message)
    }

    fn execution_error(&self, message: &'static str) -> GraphError {
        GraphError::NodeExecutionFailed {
            node: self.definition.id.clone(),
            message: message.to_owned(),
        }
    }

    fn decision(&self, state: &HashMap<String, Value>) -> Result<Option<HitlDecision>, GraphError> {
        let Some(resumes) = state.get(HITL_RESUME_STATE_KEY) else {
            return Ok(None);
        };
        let resumes = resumes
            .as_object()
            .ok_or_else(|| self.execution_error("graph.hitl.resume_invalid"))?;
        let Some(raw) = resumes.get(self.definition.id()) else {
            return Ok(None);
        };
        HitlDecision::parse(raw, &self.definition)
            .map(Some)
            .map_err(|code| self.execution_error(code))
    }

    fn remaining_decisions(&self, state: &HashMap<String, Value>) -> Result<Value, GraphError> {
        let mut resumes = state
            .get(HITL_RESUME_STATE_KEY)
            .and_then(Value::as_object)
            .cloned()
            .ok_or_else(|| self.execution_error("graph.hitl.resume_invalid"))?;
        if resumes.remove(self.definition.id()).is_none() {
            return Err(self.execution_error("graph.hitl.resume_invalid"));
        }
        Ok(Value::Object(resumes))
    }
}

#[async_trait]
impl Node for HitlNode {
    fn name(&self) -> &str {
        self.definition.id()
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let Some(decision) = self.decision(&context.state)? else {
            let message = self.message(&context.state)?;
            return Ok(NodeOutput::interrupt_with_data(
                &message,
                self.definition.interrupt_data(&message),
            ));
        };
        let target = self
            .definition
            .route(decision.action)
            .ok_or_else(|| self.execution_error("graph.hitl.action_not_configured"))?;
        let target = if target == "END" { END } else { target };
        let mut output = NodeOutput::new()
            .with_update(
                HITL_RESUME_STATE_KEY,
                self.remaining_decisions(&context.state)?,
            )
            .with_goto([target]);
        if decision.action == HitlAction::Edit {
            let key = self
                .definition
                .edit_state_key()
                .ok_or_else(|| self.execution_error("graph.hitl.edit_not_configured"))?;
            output = output.with_update(key, decision.value);
        }
        Ok(output)
    }
}

struct HitlDecision {
    action: HitlAction,
    value: Value,
}

impl HitlDecision {
    fn parse(raw: &Value, definition: &HitlNodeDefinition) -> Result<Self, &'static str> {
        let object = raw.as_object().ok_or("graph.hitl.resume_invalid")?;
        if object.len() > 3
            || object
                .keys()
                .any(|key| !matches!(key.as_str(), "definition_digest" | "action" | "value"))
        {
            return Err("graph.hitl.resume_invalid");
        }
        let digest = object
            .get("definition_digest")
            .and_then(Value::as_str)
            .ok_or("graph.hitl.resume_invalid")?;
        if digest != definition.digest_label() {
            return Err("graph.hitl.resume_identity_mismatch");
        }
        let action = match object
            .get("action")
            .and_then(Value::as_str)
            .map(str::trim)
            .map(str::to_ascii_lowercase)
            .as_deref()
        {
            Some("approve") => HitlAction::Approve,
            Some("reject" | "block_with_comment" | "reject_with_comment") => HitlAction::Reject,
            Some("edit") => HitlAction::Edit,
            _ => return Err("graph.hitl.action_invalid"),
        };
        let value = object
            .get("value")
            .cloned()
            .unwrap_or(Value::String(String::new()));
        if serde_json::to_vec(&value).map_or(true, |encoded| encoded.len() > MAX_EDIT_VALUE_BYTES) {
            return Err("graph.hitl.edit_value_exhausted");
        }
        Ok(Self { action, value })
    }
}

fn validate_inputs(inputs: &[String]) -> Result<(), HitlConfigurationError> {
    if inputs.is_empty() || inputs.len() > MAX_INPUT_KEYS {
        return Err(HitlConfigurationError::Invalid(
            "the HITL input list is outside the supported bound",
        ));
    }
    let mut unique = BTreeSet::new();
    for input in inputs {
        if !valid_output_key(input) || input == HITL_RESUME_STATE_KEY || !unique.insert(input) {
            return Err(HitlConfigurationError::Invalid(
                "the HITL input key is malformed or duplicated",
            ));
        }
    }
    Ok(())
}

fn validate_message(message: &HitlMessageDefinition) -> Result<(), HitlConfigurationError> {
    if message.value.is_empty()
        || message.value.len() > MAX_MESSAGE_TEMPLATE_BYTES
        || message.value.contains('\0')
    {
        return Err(HitlConfigurationError::Invalid(
            "the HITL user message is malformed",
        ));
    }
    if message.kind == HitlMessageKind::Variable && !valid_output_key(&message.value) {
        return Err(HitlConfigurationError::Invalid(
            "the HITL variable message key is malformed",
        ));
    }
    if message.kind == HitlMessageKind::Fstring {
        validate_template(&message.value)?;
    }
    Ok(())
}

fn validate_routes(
    routes: &HitlRoutes,
    edit_state_key: Option<&str>,
) -> Result<(), HitlConfigurationError> {
    for (_, target) in routes.iter() {
        if target != "END" && !valid_graph_id(target) {
            return Err(HitlConfigurationError::Invalid(
                "a HITL route target is malformed",
            ));
        }
    }
    let has_action = routes.approve.is_some()
        || routes.reject.is_some()
        || (routes.edit.as_deref().is_some_and(|target| target != "END")
            && edit_state_key.is_some());
    if !has_action {
        return Err(HitlConfigurationError::Invalid(
            "the HITL node has no executable action",
        ));
    }
    Ok(())
}

fn validate_template(template: &str) -> Result<(), HitlConfigurationError> {
    let bytes = template.as_bytes();
    let mut cursor = 0;
    while cursor < bytes.len() {
        match bytes[cursor] {
            b'{' if bytes.get(cursor + 1) == Some(&b'{') => cursor += 2,
            b'}' if bytes.get(cursor + 1) == Some(&b'}') => cursor += 2,
            b'{' => {
                let end = bytes[cursor + 1..]
                    .iter()
                    .position(|byte| *byte == b'}')
                    .map(|offset| cursor + 1 + offset)
                    .ok_or(HitlConfigurationError::Invalid(
                        "the HITL fstring template is malformed",
                    ))?;
                let key = &template[cursor + 1..end];
                if !valid_output_key(key) || key.contains(['!', ':', '[', ']', '.']) {
                    return Err(HitlConfigurationError::Invalid(
                        "the HITL fstring placeholder is unsupported",
                    ));
                }
                cursor = end + 1;
            }
            b'}' => {
                return Err(HitlConfigurationError::Invalid(
                    "the HITL fstring template is malformed",
                ));
            }
            _ => cursor += 1,
        }
    }
    Ok(())
}

fn render_template(
    template: &str,
    input_keys: &[String],
    state: &HashMap<String, Value>,
) -> Result<String, GraphError> {
    let preferred = input_keys
        .iter()
        .filter(|key| key.as_str() != "messages")
        .collect::<BTreeSet<_>>();
    let bytes = template.as_bytes();
    let mut output = String::with_capacity(template.len());
    let mut cursor = 0;
    while cursor < bytes.len() {
        if bytes[cursor] == b'{' && bytes.get(cursor + 1) == Some(&b'{') {
            output.push('{');
            cursor += 2;
            continue;
        }
        if bytes[cursor] == b'}' && bytes.get(cursor + 1) == Some(&b'}') {
            output.push('}');
            cursor += 2;
            continue;
        }
        if bytes[cursor] == b'{' {
            let end = bytes[cursor + 1..]
                .iter()
                .position(|byte| *byte == b'}')
                .map(|offset| cursor + 1 + offset)
                .ok_or_else(|| GraphError::Other("graph.hitl.template_invalid".to_owned()))?;
            let key = &template[cursor + 1..end];
            let value = if preferred.contains(&key.to_owned()) {
                state.get(key)
            } else {
                state.get(key).filter(|_| key != "messages")
            };
            let Some(value) = value else {
                return Ok(template.to_owned());
            };
            output.push_str(&display_state_value(value));
            cursor = end + 1;
            continue;
        }
        let character = template[cursor..]
            .chars()
            .next()
            .ok_or_else(|| GraphError::Other("graph.hitl.template_invalid".to_owned()))?;
        output.push(character);
        cursor += character.len_utf8();
        if output.len() > MAX_RENDERED_MESSAGE_BYTES {
            return Err(GraphError::Other("graph.hitl.message_exhausted".to_owned()));
        }
    }
    Ok(output)
}

fn display_state_value(value: &Value) -> String {
    value
        .as_str()
        .map_or_else(|| value.to_string(), ToOwned::to_owned)
}

fn config_digest(raw: &RawHitlNodeDefinition) -> [u8; 32] {
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(HITL_CONFIG_DIGEST_DOMAIN);
    digest_field(&mut context, raw.id.as_bytes());
    digest_field(&mut context, raw.user_message.kind_name().as_bytes());
    digest_field(&mut context, raw.user_message.value.as_bytes());
    for input in &raw.input {
        digest_field(&mut context, input.as_bytes());
    }
    for (action, target) in raw.routes.iter() {
        digest_field(&mut context, action.as_str().as_bytes());
        digest_field(&mut context, target.as_bytes());
    }
    digest_field(
        &mut context,
        raw.edit_state_key.as_deref().unwrap_or_default().as_bytes(),
    );
    let digest = context.finish();
    let mut output = [0_u8; 32];
    output.copy_from_slice(digest.as_ref());
    output
}

impl HitlMessageDefinition {
    const fn kind_name(&self) -> &'static str {
        match self.kind {
            HitlMessageKind::Fixed => "fixed",
            HitlMessageKind::Variable => "variable",
            HitlMessageKind::Fstring => "fstring",
        }
    }
}

fn digest_field(context: &mut digest::Context, value: &[u8]) {
    context.update(&(value.len() as u64).to_be_bytes());
    context.update(value);
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

fn default_message() -> String {
    "Please review and approve to continue.".to_owned()
}

fn default_inputs() -> Vec<String> {
    vec!["messages".to_owned()]
}

/// Stable, data-free compiler error for a stored pipeline HITL node.
#[derive(Debug, Error)]
pub(crate) enum HitlConfigurationError {
    #[error("the HITL YAML node exceeds its resource bound")]
    ResourceExhausted,
    #[error("the HITL YAML node is malformed")]
    MalformedYaml {
        #[source]
        source: serde_yaml_ng::Error,
    },
    #[error("the HITL YAML node is invalid: {0}")]
    Invalid(&'static str),
}

impl HitlConfigurationError {
    #[must_use]
    pub(crate) const fn code(&self) -> &'static str {
        match self {
            Self::ResourceExhausted => "graph.hitl.configuration_resource_exhausted",
            Self::MalformedYaml { .. } => "graph.hitl.malformed_yaml",
            Self::Invalid(_) => "graph.hitl.invalid_configuration",
        }
    }
}

#[cfg(test)]
pub(crate) fn authorized_resume_fixture(
    definition: &HitlNodeDefinition,
    action: &str,
    value: &Value,
) -> Value {
    json!({
        HITL_RESUME_STATE_KEY: {
            definition.id(): {
                "definition_digest": definition.digest_label(),
                "action": action,
                "value": value,
            }
        }
    })
}
