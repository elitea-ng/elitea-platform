use std::collections::HashSet;

use prost::Message;
use serde_json::{Map, Value};

use super::request::{
    AgentExecutionKind, AgentExecutionPayload, AgentExecutionRequest, AgentInputBinding,
    NextInputSuggestionPolicy, UserInput,
};
use crate::protocol::{
    ProtocolError as AgentProtocolError, elitea::runtime::v1::AgentExecutionInputV1,
};
use crate::toolkits::{ToolAdmissionPolicy, ToolAdmissionPolicyErrorCode};

pub const AGENT_INPUT_SCHEMA_REVISION: &str = "elitea.runtime.agent-execution-input.v1";
const MAX_AGENT_INPUT_BYTES: usize = 1024 * 1024;
const MAX_JSON_VALUE_BYTES: usize = 256 * 1024;
const MAX_JSON_DEPTH: usize = 64;
const MAX_JSON_STRING_BYTES: usize = 64 * 1024;

/// Decode only the canonical protobuf representation admitted by protocol v1.
///
/// This rejects unknown fields, duplicate scalar encodings, explicit default
/// encodings and non-canonical field ordering because decoding and canonical
/// re-encoding must reproduce the exact fetched bytes.
///
/// # Errors
///
/// Returns [`AgentProtocolError::ResourceExhausted`] for an empty or oversized
/// payload and [`AgentProtocolError::InvalidInput`] for malformed,
/// non-canonical, or unsupported input.
pub fn parse_agent_execution_input(
    raw: &[u8],
) -> Result<AgentExecutionInputV1, AgentProtocolError> {
    if raw.is_empty() || raw.len() > MAX_AGENT_INPUT_BYTES {
        return Err(AgentProtocolError::ResourceExhausted(
            "the agent execution input exceeds its limit",
        ));
    }
    let message = AgentExecutionInputV1::decode(raw)
        .map_err(|_| AgentProtocolError::InvalidInput("the agent execution input is malformed"))?;
    if message.encode_to_vec() != raw {
        return Err(AgentProtocolError::InvalidInput(
            "the agent execution input is not canonical protocol v1",
        ));
    }
    if message.schema_revision != AGENT_INPUT_SCHEMA_REVISION {
        return Err(AgentProtocolError::InvalidInput(
            "the agent execution input revision is not supported",
        ));
    }
    Ok(message)
}

/// Validate every semantic field and bind it to the exact materialized input.
///
/// # Errors
///
/// Returns [`AgentProtocolError::InvalidInput`] for a malformed binding,
/// semantic shape, or invariant. Bounded JSON input may also return
/// [`AgentProtocolError::ResourceExhausted`].
#[allow(clippy::too_many_lines)] // A linear field projection is easier to audit against the proto.
pub fn request_from(
    message: AgentExecutionInputV1,
    kind: AgentExecutionKind,
    binding: AgentInputBinding,
) -> Result<AgentExecutionRequest, AgentProtocolError> {
    validate_binding(&binding)?;

    let llm = json_object(&message.llm, "the agent llm must be an object")?;
    let chat_history = json_list(
        &message.chat_history,
        "the agent chat history must be a list",
    )?;
    let user_input = match parse_json_value(&message.user_input)? {
        Value::String(value) => UserInput::Text(value),
        Value::Array(value) => UserInput::ContentBlocks(value),
        _ => {
            return Err(AgentProtocolError::InvalidInput(
                "the agent user input must be text or content blocks",
            ));
        }
    };
    let tools = json_list(&message.tools, "the agent tools must be a list")?;
    let application = json_object(
        &message.application,
        "the agent application must be an object",
    )?;
    let internal_tools = string_list(
        &message.internal_tools,
        "the agent internal tools must contain only strings",
    )?;
    let mcp_tokens = json_object(
        &message.mcp_tokens,
        "the agent MCP tokens must be an object",
    )?;
    let ignored_mcp_servers = json_list(
        &message.ignored_mcp_servers,
        "the agent ignored MCP servers must be a list",
    )?;
    let user_declined_mcp_servers = json_list(
        &message.user_declined_mcp_servers,
        "the agent declined MCP servers must be a list",
    )?;
    let hitl_decisions = json_list(
        &message.hitl_decisions,
        "the agent HITL decisions must be a list",
    )?;
    let meta = json_object(&message.meta, "the agent metadata must be an object")?;
    let context_settings = json_object(
        &message.context_settings,
        "the agent context settings must be an object",
    )?;
    let invoked_skills = json_list(
        &message.invoked_skills,
        "the agent invoked skills must be a list",
    )?;
    let applied_skills = json_list(
        &message.applied_skills,
        "the agent applied skills must be a list",
    )?;
    let attached_skills = json_list(
        &message.attached_skills,
        "the agent attached skills must be a list",
    )?;
    let input_attachments = json_list(
        &message.input_attachments,
        "the agent input attachments must be a list",
    )?;
    let parallel_reconcile = optional_json_object(
        &message.parallel_reconcile,
        "the agent parallel reconcile must be an object or null",
    )?;
    let parallel_terminal_errors = json_list(
        &message.parallel_terminal_errors,
        "the agent parallel terminal errors must be a list",
    )?;
    let next_input_suggestion = next_input_suggestion_policy(&message.next_input_suggestion)?;
    let toolkit_guardrails = toolkit_guardrails_policy(&message.toolkit_guardrails)?;
    let truncated_content = optional_json_string(
        &message.truncated_content,
        "the truncated agent content must be text",
    )?;

    let steps_limit = match message.steps_limit {
        Some(value) if value > 0 => Some(value.cast_unsigned()),
        Some(_) => {
            return Err(AgentProtocolError::InvalidInput(
                "the agent step limit must be positive",
            ));
        }
        None => None,
    };
    if message.hitl_resume && message.hitl_action.is_none() && hitl_decisions.is_empty() {
        return Err(AgentProtocolError::InvalidInput(
            "a HITL resume decision is required",
        ));
    }
    match kind {
        AgentExecutionKind::Application => validate_application_identity(&application)?,
        AgentExecutionKind::Adhoc if model_name(&llm).is_none() => {
            return Err(AgentProtocolError::InvalidInput(
                "the ad-hoc agent model is required",
            ));
        }
        AgentExecutionKind::Adhoc => {}
    }

    Ok(AgentExecutionRequest {
        kind,
        binding,
        payload: AgentExecutionPayload {
            llm,
            chat_history,
            user_input,
            thread_id: message.thread_id,
            checkpoint_id: message.checkpoint_id,
            debug: message.debug,
            tools,
            application,
            internal_tools,
            steps_limit,
            mcp_tokens,
            ignored_mcp_servers,
            user_declined_mcp_servers,
            should_continue: message.should_continue,
            hitl_resume: message.hitl_resume,
            hitl_action: message.hitl_action,
            hitl_value: message.hitl_value,
            hitl_decisions,
            execution_generation: message.execution_generation,
            is_regenerate: message.is_regenerate,
            meta,
            conversation_id: message.conversation_id,
            persona: if message.persona.is_empty() {
                "generic".to_owned()
            } else {
                message.persona
            },
            context_settings,
            supports_vision: message.supports_vision,
            return_chat_history: message.return_chat_history,
            invoked_skills,
            applied_skills,
            auto_approve_sensitive_actions: message.auto_approve_sensitive_actions,
            attached_skills,
            input_attachments,
            parallel_reconcile,
            parallel_terminal_errors,
            exception_handling_enabled: message.exception_handling_enabled,
            debug_mode: message.debug_mode,
            next_input_suggestion,
            toolkit_guardrails,
            truncated_content,
        },
    })
}

fn validate_binding(binding: &AgentInputBinding) -> Result<(), AgentProtocolError> {
    if binding.input_bundle_id.is_empty()
        || binding.request_entry_id.is_empty()
        || binding.request_immutable_version.is_empty()
        || binding.input_bundle_digest.iter().all(|byte| *byte == 0)
        || binding.request_content_digest.iter().all(|byte| *byte == 0)
    {
        return Err(AgentProtocolError::InvalidInput(
            "the agent execution input binding is malformed",
        ));
    }
    Ok(())
}

pub(crate) fn parse_json_value(raw: &[u8]) -> Result<Value, AgentProtocolError> {
    parse_bounded_json_value(raw, MAX_JSON_VALUE_BYTES)
}

pub(crate) fn parse_bounded_json_value(
    raw: &[u8],
    maximum: usize,
) -> Result<Value, AgentProtocolError> {
    if raw.is_empty() || raw.len() > maximum {
        return Err(AgentProtocolError::ResourceExhausted(
            "the agent JSON input exceeds the approved limit",
        ));
    }
    let value = serde_json::from_slice(raw)
        .map_err(|_| AgentProtocolError::InvalidInput("the agent JSON input is malformed"))?;
    JsonLimits::new(raw).validate()?;
    Ok(value)
}

fn json_object(
    raw: &[u8],
    shape_error: &'static str,
) -> Result<Map<String, Value>, AgentProtocolError> {
    match parse_json_value(raw)? {
        Value::Object(value) => Ok(value),
        _ => Err(AgentProtocolError::InvalidInput(shape_error)),
    }
}

fn optional_json_object(
    raw: &[u8],
    shape_error: &'static str,
) -> Result<Option<Map<String, Value>>, AgentProtocolError> {
    match parse_json_value(raw)? {
        Value::Null => Ok(None),
        Value::Object(value) => Ok(Some(value)),
        _ => Err(AgentProtocolError::InvalidInput(shape_error)),
    }
}

fn optional_json_string(
    raw: &[u8],
    shape_error: &'static str,
) -> Result<Option<String>, AgentProtocolError> {
    if raw.is_empty() {
        return Ok(None);
    }
    match parse_json_value(raw)? {
        Value::String(value) => Ok(Some(value)),
        _ => Err(AgentProtocolError::InvalidInput(shape_error)),
    }
}

fn json_list(raw: &[u8], shape_error: &'static str) -> Result<Vec<Value>, AgentProtocolError> {
    match parse_json_value(raw)? {
        Value::Array(value) => Ok(value),
        _ => Err(AgentProtocolError::InvalidInput(shape_error)),
    }
}

fn string_list(raw: &[u8], shape_error: &'static str) -> Result<Vec<String>, AgentProtocolError> {
    json_list(raw, shape_error)?
        .into_iter()
        .map(|value| match value {
            Value::String(value) => Ok(value),
            _ => Err(AgentProtocolError::InvalidInput(shape_error)),
        })
        .collect()
}

fn validate_application_identity(
    application: &Map<String, Value>,
) -> Result<(), AgentProtocolError> {
    let ids_present = application.get("id").is_some_and(is_positive_integer)
        && application
            .get("version_id")
            .is_some_and(is_positive_integer);
    if !ids_present
        && !application
            .get("version_details")
            .is_some_and(Value::is_object)
    {
        return Err(AgentProtocolError::InvalidInput(
            "the configured application identity is required",
        ));
    }
    Ok(())
}

fn is_positive_integer(value: &Value) -> bool {
    let Value::Number(value) = value else {
        return false;
    };
    let text = value.to_string();
    !text.starts_with('-')
        && text != "0"
        && !text.bytes().any(|byte| matches!(byte, b'.' | b'e' | b'E'))
}

fn model_name(llm: &Map<String, Value>) -> Option<&str> {
    llm.get("kwargs")?
        .as_object()?
        .get("model")?
        .as_str()
        .filter(|value| !value.is_empty())
}

fn next_input_suggestion_policy(
    raw: &[u8],
) -> Result<NextInputSuggestionPolicy, AgentProtocolError> {
    if raw.is_empty() {
        return Ok(NextInputSuggestionPolicy::default());
    }
    let value = parse_json_value(raw)?;
    let Value::Object(value) = value else {
        if value.is_null() {
            return Ok(NextInputSuggestionPolicy::default());
        }
        return Err(next_input_suggestion_error());
    };
    if value.keys().any(|key| {
        !matches!(
            key.as_str(),
            "enabled" | "min_response_chars" | "timeout_seconds"
        )
    }) {
        return Err(next_input_suggestion_error());
    }
    let enabled = match value.get("enabled") {
        Some(Value::Bool(value)) => *value,
        None => false,
        Some(_) => return Err(next_input_suggestion_error()),
    };
    let min_response_chars = bounded_integer(value.get("min_response_chars"), 150, 100_000)?;
    let timeout_seconds = bounded_integer(value.get("timeout_seconds"), 15, 300)?;
    Ok(NextInputSuggestionPolicy {
        enabled,
        min_response_chars,
        timeout_seconds: timeout_seconds
            .try_into()
            .map_err(|_| next_input_suggestion_error())?,
    })
}

fn bounded_integer(
    value: Option<&Value>,
    default: u32,
    maximum: u32,
) -> Result<u32, AgentProtocolError> {
    match value {
        None => Ok(default),
        Some(Value::Number(value)) => value
            .as_u64()
            .and_then(|value| u32::try_from(value).ok())
            .filter(|value| (1..=maximum).contains(value))
            .ok_or_else(next_input_suggestion_error),
        Some(_) => Err(next_input_suggestion_error()),
    }
}

const fn next_input_suggestion_error() -> AgentProtocolError {
    AgentProtocolError::InvalidInput("the agent next input suggestion policy is invalid")
}

fn toolkit_guardrails_policy(raw: &[u8]) -> Result<Option<Map<String, Value>>, AgentProtocolError> {
    if raw.is_empty() {
        return Ok(None);
    }
    let value = parse_json_value(raw)?;
    let Value::Object(policy) = value else {
        if value.is_null() {
            return Ok(None);
        }
        return Err(toolkit_guardrails_error());
    };
    let runtime = Map::from_iter([("toolkit_security".to_owned(), Value::Object(policy.clone()))]);
    ToolAdmissionPolicy::from_runtime_config(&runtime).map_err(|error| match error.code() {
        ToolAdmissionPolicyErrorCode::InvalidConfiguration => toolkit_guardrails_error(),
        ToolAdmissionPolicyErrorCode::ResourceExhausted => AgentProtocolError::ResourceExhausted(
            "the agent toolkit guardrails exceed the approved limit",
        ),
    })?;
    Ok(Some(policy))
}

const fn toolkit_guardrails_error() -> AgentProtocolError {
    AgentProtocolError::InvalidInput("the agent toolkit guardrails are invalid")
}

/// A second, allocation-bounded structural pass catches duplicate object
/// members and applies Python-worker-compatible decoded string/depth limits.
/// `serde_json` performs the JSON grammar validation and preserves arbitrary
/// precision numbers; this pass only walks already-valid syntax.
struct JsonLimits<'a> {
    raw: &'a [u8],
    cursor: usize,
}

impl<'a> JsonLimits<'a> {
    const fn new(raw: &'a [u8]) -> Self {
        Self { raw, cursor: 0 }
    }

    fn validate(mut self) -> Result<(), AgentProtocolError> {
        self.skip_whitespace();
        self.value(0)?;
        self.skip_whitespace();
        if self.cursor != self.raw.len() {
            return Err(malformed_json());
        }
        Ok(())
    }

    fn value(&mut self, depth: usize) -> Result<(), AgentProtocolError> {
        if depth > MAX_JSON_DEPTH {
            return Err(AgentProtocolError::ResourceExhausted(
                "the agent JSON input exceeds the nesting limit",
            ));
        }
        self.skip_whitespace();
        match self.peek() {
            Some(b'{') => self.object(depth),
            Some(b'[') => self.array(depth),
            Some(b'\"') => self.string().map(|_| ()),
            Some(b't') => self.literal(b"true"),
            Some(b'f') => self.literal(b"false"),
            Some(b'n') => self.literal(b"null"),
            Some(b'-' | b'0'..=b'9') => self.number(),
            _ => Err(malformed_json()),
        }
    }

    fn object(&mut self, depth: usize) -> Result<(), AgentProtocolError> {
        self.cursor += 1;
        self.skip_whitespace();
        if self.take(b'}') {
            return Ok(());
        }
        let mut keys = HashSet::new();
        loop {
            let key = self.string()?;
            if !keys.insert(key) {
                return Err(AgentProtocolError::InvalidInput(
                    "the agent JSON input contains a duplicate member",
                ));
            }
            self.skip_whitespace();
            if !self.take(b':') {
                return Err(malformed_json());
            }
            self.value(depth + 1)?;
            self.skip_whitespace();
            if self.take(b'}') {
                return Ok(());
            }
            if !self.take(b',') {
                return Err(malformed_json());
            }
            self.skip_whitespace();
        }
    }

    fn array(&mut self, depth: usize) -> Result<(), AgentProtocolError> {
        self.cursor += 1;
        self.skip_whitespace();
        if self.take(b']') {
            return Ok(());
        }
        loop {
            self.value(depth + 1)?;
            self.skip_whitespace();
            if self.take(b']') {
                return Ok(());
            }
            if !self.take(b',') {
                return Err(malformed_json());
            }
            self.skip_whitespace();
        }
    }

    fn string(&mut self) -> Result<String, AgentProtocolError> {
        let start = self.cursor;
        if !self.take(b'\"') {
            return Err(malformed_json());
        }
        let mut escaped = false;
        while let Some(byte) = self.peek() {
            self.cursor += 1;
            if escaped {
                escaped = false;
            } else if byte == b'\\' {
                escaped = true;
            } else if byte == b'\"' {
                let value: String = serde_json::from_slice(&self.raw[start..self.cursor])
                    .map_err(|_| malformed_json())?;
                if value.len() > MAX_JSON_STRING_BYTES {
                    return Err(AgentProtocolError::ResourceExhausted(
                        "an agent JSON string exceeds the approved limit",
                    ));
                }
                return Ok(value);
            }
        }
        Err(malformed_json())
    }

    fn literal(&mut self, expected: &[u8]) -> Result<(), AgentProtocolError> {
        let end = self.cursor.saturating_add(expected.len());
        if self.raw.get(self.cursor..end) != Some(expected) {
            return Err(malformed_json());
        }
        self.cursor = end;
        Ok(())
    }

    fn number(&mut self) -> Result<(), AgentProtocolError> {
        let start = self.cursor;
        while self
            .peek()
            .is_some_and(|byte| !matches!(byte, b' ' | b'\t' | b'\r' | b'\n' | b',' | b']' | b'}'))
        {
            self.cursor += 1;
        }
        let raw = self
            .raw
            .get(start..self.cursor)
            .ok_or_else(malformed_json)?;
        if raw.iter().any(|byte| matches!(byte, b'.' | b'e' | b'E')) {
            let number = std::str::from_utf8(raw)
                .ok()
                .and_then(|value| value.parse::<f64>().ok());
            if !number.is_some_and(f64::is_finite) {
                return Err(AgentProtocolError::InvalidInput(
                    "the agent JSON input contains a non-finite number",
                ));
            }
        }
        Ok(())
    }

    fn skip_whitespace(&mut self) {
        while self
            .peek()
            .is_some_and(|byte| matches!(byte, b' ' | b'\t' | b'\r' | b'\n'))
        {
            self.cursor += 1;
        }
    }

    fn take(&mut self, expected: u8) -> bool {
        if self.peek() == Some(expected) {
            self.cursor += 1;
            true
        } else {
            false
        }
    }

    fn peek(&self) -> Option<u8> {
        self.raw.get(self.cursor).copied()
    }
}

const fn malformed_json() -> AgentProtocolError {
    AgentProtocolError::InvalidInput("the agent JSON input is malformed")
}
