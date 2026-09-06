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
}

impl DirectToolkitRequest {
    pub(crate) fn parse(raw: &[u8]) -> Result<Self, DirectToolkitRequestError> {
        let input = parse_toolkit_execute_read_input(raw).map_err(protocol_error)?;
        Self::from_message(input)
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

    #[must_use]
    pub(crate) fn policy(&self) -> &Arc<ToolAdmissionPolicy> {
        &self.policy
    }
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
