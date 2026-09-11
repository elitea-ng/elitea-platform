//! Semantic projection of one claim-materialized direct toolkit request.

use std::fmt;
use std::sync::Arc;

use serde_json::{Map, Value};

use super::{FrozenToolKind, FrozenToolSnapshot, ToolAdmissionPolicy};
use crate::agents::protocol::parse_bounded_json_value;
use crate::protocol::ProtocolError;
use crate::protocol::elitea::runtime::v1::ToolkitExecuteReadInputV1;
use crate::protocol::toolkit_execution::parse_toolkit_execute_read_input;

const MAX_TOOLKIT_JSON_BYTES: usize = 640 * 1024;
const MAX_ARGUMENT_JSON_BYTES: usize = 256 * 1024;
const MAX_GUARDRAIL_JSON_BYTES: usize = 64 * 1024;
const MAX_IDENTITY_BYTES: usize = 1024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum DirectToolkitRequestErrorCode {
    InvalidInput,
    ResourceExhausted,
    IncompatibleVersion,
}

#[derive(Clone, Copy)]
pub(crate) struct DirectToolkitRequestError {
    code: DirectToolkitRequestErrorCode,
}

impl DirectToolkitRequestError {
    #[must_use]
    pub(crate) const fn code(self) -> DirectToolkitRequestErrorCode {
        self.code
    }
}

impl fmt::Debug for DirectToolkitRequestError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("DirectToolkitRequestError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for DirectToolkitRequestError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            DirectToolkitRequestErrorCode::InvalidInput => "the direct toolkit request is invalid",
            DirectToolkitRequestErrorCode::ResourceExhausted => {
                "the direct toolkit request exceeds its approved bounds"
            }
            DirectToolkitRequestErrorCode::IncompatibleVersion => {
                "the direct toolkit request version is not supported"
            }
        })
    }
}

impl std::error::Error for DirectToolkitRequestError {}

/// Fully owned, bounded input after claim-scoped secret materialization.
///
/// This type deliberately has no `Debug` or `Clone`: the frozen toolkit may
/// now contain short-lived plaintext credentials redeemed by Main.
pub(crate) struct DirectToolkitRequest {
    toolkit: Value,
    toolkit_type: String,
    toolkit_name: String,
    tool_name: String,
    arguments: Value,
    policy: Arc<ToolAdmissionPolicy>,
    tokens: Map<String, Value>,
    model_context: Map<String, Value>,
}

impl DirectToolkitRequest {
    pub(crate) fn parse(raw: &[u8]) -> Result<Self, DirectToolkitRequestError> {
        let input = parse_toolkit_execute_read_input(raw).map_err(protocol_error)?;
        Self::from_message(input)
    }

    /// Parse the existing shared Test settings and arguments data-plane entries.
    pub(crate) fn parse_call(
        toolkit_type: &str,
        toolkit_id: &str,
        tool_name: &str,
        settings: &[u8],
        arguments: &[u8],
        context: &[u8],
    ) -> Result<Self, DirectToolkitRequestError> {
        let toolkit = parse_json(settings, MAX_TOOLKIT_JSON_BYTES)?;
        let id = toolkit
            .get("id")
            .and_then(Value::as_u64)
            .ok_or_else(invalid_input)?;
        if id == 0 || id.to_string() != toolkit_id {
            return Err(invalid_input());
        }
        let name = toolkit
            .get("toolkit_name")
            .and_then(Value::as_str)
            .ok_or_else(invalid_input)?;
        let mut request = Self::from_message(ToolkitExecuteReadInputV1 {
            schema_revision: String::new(),
            toolkit: settings.to_vec(),
            toolkit_type: toolkit_type.to_owned(),
            toolkit_name: name.to_owned(),
            tool_name: tool_name.to_owned(),
            arguments: arguments.to_vec(),
            toolkit_guardrails: context_guardrails(context)?,
        })?;
        request.tokens = context_tokens(context)?;
        request.model_context = runtime_context(context)?
            .into_iter()
            .filter(|(key, _)| matches!(key.as_str(), "llm_model" | "llm_configuration"))
            .collect();
        Ok(request)
    }

    /// Discovery has no saved-row ID in its contract and never invokes a tool.
    pub(crate) fn parse_discovery(
        toolkit_type: &str,
        settings: &[u8],
        context: &[u8],
    ) -> Result<Self, DirectToolkitRequestError> {
        let settings = parse_json(settings, MAX_TOOLKIT_JSON_BYTES)?;
        if !settings.is_object() || !valid_identity(toolkit_type) {
            return Err(invalid_input());
        }
        let toolkit = serde_json::json!({
            "id": 1, "type": toolkit_type, "toolkit_name": toolkit_type, "settings": settings
        });
        let mut request = Self::from_message(ToolkitExecuteReadInputV1 {
            schema_revision: String::new(),
            toolkit: serde_json::to_vec(&toolkit).map_err(|_| invalid_input())?,
            toolkit_type: toolkit_type.to_owned(),
            toolkit_name: toolkit_type.to_owned(),
            tool_name: "discovery".to_owned(),
            arguments: b"{}".to_vec(),
            toolkit_guardrails: context_guardrails(context)?,
        })?;
        request.tokens = context_tokens(context)?;
        request.model_context = runtime_context(context)?
            .into_iter()
            .filter(|(key, _)| matches!(key.as_str(), "llm_model" | "llm_configuration"))
            .collect();
        Ok(request)
    }

    fn from_message(input: ToolkitExecuteReadInputV1) -> Result<Self, DirectToolkitRequestError> {
        for identity in [&input.toolkit_type, &input.toolkit_name, &input.tool_name] {
            if !valid_identity(identity) {
                return Err(invalid_input());
            }
        }
        let toolkit = parse_json(&input.toolkit, MAX_TOOLKIT_JSON_BYTES)?;
        if !toolkit.is_object() {
            return Err(invalid_input());
        }
        let arguments = parse_json(&input.arguments, MAX_ARGUMENT_JSON_BYTES)?;
        if !arguments.is_object() {
            return Err(invalid_input());
        }
        let guardrails = parse_json(&input.toolkit_guardrails, MAX_GUARDRAIL_JSON_BYTES)?;
        let Value::Object(guardrails) = guardrails else {
            return Err(invalid_input());
        };
        let runtime = Map::from_iter([("toolkit_security".to_owned(), Value::Object(guardrails))]);
        let policy = ToolAdmissionPolicy::from_runtime_config(&runtime)
            .map(Arc::new)
            .map_err(|error| match error.code() {
                super::ToolAdmissionPolicyErrorCode::InvalidConfiguration => invalid_input(),
                super::ToolAdmissionPolicyErrorCode::ResourceExhausted => resource_exhausted(),
            })?;

        let snapshot =
            FrozenToolSnapshot::from_toolkit(&toolkit).map_err(|error| match error.code() {
                super::FrozenToolSnapshotErrorCode::InvalidInput => invalid_input(),
                super::FrozenToolSnapshotErrorCode::ResourceExhausted => resource_exhausted(),
            })?;
        let Some(reference) = snapshot.iter().next() else {
            return Err(invalid_input());
        };
        if reference.kind() == FrozenToolKind::Application
            || reference.tool_type() != input.toolkit_type
            || reference.toolkit_name() != input.toolkit_name
        {
            return Err(invalid_input());
        }
        if snapshot.apply_policy(&policy).is_empty() {
            return Err(invalid_input());
        }

        Ok(Self {
            toolkit,
            toolkit_type: input.toolkit_type,
            toolkit_name: input.toolkit_name,
            tool_name: input.tool_name,
            arguments,
            policy,
            tokens: Map::new(),
            model_context: Map::new(),
        })
    }

    pub(crate) fn snapshot(&self) -> Result<FrozenToolSnapshot<'_>, DirectToolkitRequestError> {
        FrozenToolSnapshot::from_toolkit(&self.toolkit).map_err(|error| match error.code() {
            super::FrozenToolSnapshotErrorCode::InvalidInput => invalid_input(),
            super::FrozenToolSnapshotErrorCode::ResourceExhausted => resource_exhausted(),
        })
    }

    #[must_use]
    pub(crate) fn toolkit_type(&self) -> &str {
        &self.toolkit_type
    }

    #[must_use]
    pub(crate) fn toolkit_name(&self) -> &str {
        &self.toolkit_name
    }

    #[must_use]
    pub(crate) fn tool_name(&self) -> &str {
        &self.tool_name
    }

    #[must_use]
    pub(crate) fn arguments(&self) -> &Value {
        &self.arguments
    }

    pub(crate) fn tokens(&self) -> &Map<String, Value> {
        &self.tokens
    }

    #[must_use]
    pub(crate) fn policy(&self) -> &Arc<ToolAdmissionPolicy> {
        &self.policy
    }
}

fn runtime_context(raw: &[u8]) -> Result<Map<String, Value>, DirectToolkitRequestError> {
    let Value::Object(context) = parse_json(raw, MAX_TOOLKIT_JSON_BYTES)? else {
        return Err(invalid_input());
    };
    if context.keys().any(|key| {
        !matches!(
            key.as_str(),
            "toolkit_security" | "mcp_tokens" | "llm_model" | "llm_configuration"
        )
    }) {
        return Err(invalid_input());
    }
    if context.get("llm_model").is_some_and(|value| {
        !value.is_null()
            && value.as_str().is_none_or(|name| {
                name.len() > MAX_IDENTITY_BYTES || name.chars().any(char::is_control)
            })
    }) || context
        .get("llm_configuration")
        .is_some_and(|value| !value.is_null() && !value.is_object())
    {
        return Err(invalid_input());
    }
    Ok(context)
}

fn context_guardrails(raw: &[u8]) -> Result<Vec<u8>, DirectToolkitRequestError> {
    let context = runtime_context(raw)?;
    let guardrails = context
        .get("toolkit_security")
        .filter(|value| value.is_object())
        .ok_or_else(invalid_input)?;
    serde_json::to_vec(guardrails).map_err(|_| invalid_input())
}

fn context_tokens(raw: &[u8]) -> Result<Map<String, Value>, DirectToolkitRequestError> {
    match runtime_context(raw)?.remove("mcp_tokens") {
        Some(Value::Object(tokens)) => {
            if tokens.iter().any(|(key, value)| {
                !super::DelegatedAuthorizationRequirement::valid_token_key(key)
                    || !valid_materialized_token(value)
            }) {
                return Err(invalid_input());
            }
            Ok(tokens)
        }
        None | Some(Value::Null) => Ok(Map::new()),
        _ => Err(invalid_input()),
    }
}

fn valid_materialized_token(value: &Value) -> bool {
    let token = match value {
        Value::String(value) => Some(value.as_str()),
        Value::Object(value)
            if value
                .keys()
                .all(|key| matches!(key.as_str(), "access_token" | "session_id")) =>
        {
            value.get("access_token").and_then(Value::as_str)
        }
        _ => None,
    };
    token.is_some_and(|token| {
        !token.is_empty()
            && token.len() <= 16 * 1024
            && !token.chars().any(char::is_control)
            && !token.contains("{{")
            && !token.contains("}}")
    })
}

fn parse_json(raw: &[u8], maximum: usize) -> Result<Value, DirectToolkitRequestError> {
    parse_bounded_json_value(raw, maximum).map_err(protocol_error)
}

fn protocol_error(error: ProtocolError) -> DirectToolkitRequestError {
    match error {
        ProtocolError::ResourceExhausted(_) => resource_exhausted(),
        ProtocolError::IncompatibleVersion(_) => incompatible_version(),
        ProtocolError::InvalidInput(_)
        | ProtocolError::AuthorizationFailed(_)
        | ProtocolError::UnsupportedCapability(_) => invalid_input(),
    }
}

fn valid_identity(value: &str) -> bool {
    !value.is_empty() && value.len() <= MAX_IDENTITY_BYTES && !value.chars().any(char::is_control)
}

const fn invalid_input() -> DirectToolkitRequestError {
    DirectToolkitRequestError {
        code: DirectToolkitRequestErrorCode::InvalidInput,
    }
}

const fn resource_exhausted() -> DirectToolkitRequestError {
    DirectToolkitRequestError {
        code: DirectToolkitRequestErrorCode::ResourceExhausted,
    }
}

const fn incompatible_version() -> DirectToolkitRequestError {
    DirectToolkitRequestError {
        code: DirectToolkitRequestErrorCode::IncompatibleVersion,
    }
}

#[cfg(test)]
mod tests {
    use prost::Message;
    use serde_json::json;

    use super::*;
    use crate::protocol::toolkit_execution::TOOLKIT_EXECUTE_READ_INPUT_SCHEMA_REVISION;

    fn toolkit() -> Value {
        json!({
            "id": 52,
            "type": "mcp",
            "name": "documentation-mcp",
            "description": "Saved external MCP server",
            "author_id": 11,
            "settings": {
                "url": "https://mcp.example.invalid/events",
                "selected_tools": ["search_docs"]
            },
            "meta": {"mcp": true},
            "is_pinned": false,
            "toolkit_name": "documentation-mcp"
        })
    }

    fn input() -> ToolkitExecuteReadInputV1 {
        ToolkitExecuteReadInputV1 {
            schema_revision: TOOLKIT_EXECUTE_READ_INPUT_SCHEMA_REVISION.to_owned(),
            toolkit: serde_json::to_vec(&toolkit()).expect("toolkit fixture"),
            toolkit_type: "mcp".to_owned(),
            toolkit_name: "documentation-mcp".to_owned(),
            tool_name: "search_docs".to_owned(),
            arguments: br#"{"query":"durability"}"#.to_vec(),
            toolkit_guardrails: b"{}".to_vec(),
        }
    }

    #[test]
    fn rejects_unredeemed_token_references_before_native_materialization() {
        let raw = serde_json::to_vec(&toolkit()).expect("toolkit");
        let result = DirectToolkitRequest::parse_call("mcp", "52", "search_docs", &raw, b"{}",
            br#"{"toolkit_security":{},"mcp_tokens":{"https://mcp.example.invalid/events":{"access_token":"{{secret.TOKEN}}"}}}"#);
        assert!(result.is_err());
    }

    #[test]
    fn keeps_supplied_model_settings_for_model_independent_tools() {
        let raw = serde_json::to_vec(&toolkit()).expect("toolkit");
        let request = DirectToolkitRequest::parse_call("mcp", "52", "search_docs", &raw, b"{}",
            br#"{"toolkit_security":{},"llm_model":"chosen-model","llm_configuration":{"temperature":0.25,"max_tokens":256}}"#).expect("model context");
        assert_eq!(request.model_context["llm_model"], "chosen-model");
        assert_eq!(
            request.model_context["llm_configuration"],
            json!({"temperature":0.25,"max_tokens":256})
        );
    }

    #[test]
    fn accepts_one_exact_frozen_toolkit_and_preserves_invocation_identity() {
        let request = DirectToolkitRequest::parse(&input().encode_to_vec()).expect("valid input");

        assert_eq!(request.toolkit_type(), "mcp");
        assert_eq!(request.toolkit_name(), "documentation-mcp");
        assert_eq!(request.tool_name(), "search_docs");
        assert_eq!(request.arguments(), &json!({"query": "durability"}));
        assert_eq!(request.snapshot().expect("snapshot").len(), 1);
    }

    #[test]
    fn rejects_claim_identity_that_does_not_match_the_frozen_toolkit() {
        let mut input = input();
        input.toolkit_name = "another-toolkit".to_owned();

        let error = DirectToolkitRequest::parse(&input.encode_to_vec())
            .err()
            .expect("mismatched identity must fail");

        assert_eq!(error.code(), DirectToolkitRequestErrorCode::InvalidInput);
    }

    #[test]
    fn rejects_duplicate_json_members_before_materialization() {
        let mut input = input();
        input.arguments = br#"{"query":"first","query":"second"}"#.to_vec();

        let error = DirectToolkitRequest::parse(&input.encode_to_vec())
            .err()
            .expect("duplicate argument must fail");

        assert_eq!(error.code(), DirectToolkitRequestErrorCode::InvalidInput);
    }

    #[test]
    fn applies_operator_toolkit_guardrails_before_materialization() {
        let mut input = input();
        input.toolkit_guardrails = br#"{"blocked_toolkits":["mcp"]}"#.to_vec();

        let error = DirectToolkitRequest::parse(&input.encode_to_vec())
            .err()
            .expect("blocked toolkit must fail");

        assert_eq!(error.code(), DirectToolkitRequestErrorCode::InvalidInput);
    }

    #[test]
    fn reports_oversized_arguments_without_retaining_their_content() {
        let mut input = input();
        input.arguments = serde_json::to_vec(&json!({"query": "x".repeat(256 * 1_024)}))
            .expect("oversized argument fixture");

        let error = DirectToolkitRequest::parse(&input.encode_to_vec())
            .err()
            .expect("oversized arguments must fail");

        assert_eq!(
            error.code(),
            DirectToolkitRequestErrorCode::ResourceExhausted
        );
        assert!(!format!("{error:?} {error}").contains("xxxxxxxx"));
    }
}
