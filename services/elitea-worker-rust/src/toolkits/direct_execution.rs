//! Direct execution of one exact, already-materialized toolkit operation.
//!
//! External Elitea-as-MCP calls do not need an LLM planning turn. This kernel
//! therefore accepts the toolsets produced by the normal claim-scoped
//! materializers and invokes one original operation directly. It deliberately
//! permits only tools whose runtime implementation declares them read-only;
//! effectful calls need a durable effect receipt before Redis redelivery can be
//! allowed to repeat them safely.

use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ToolContext, Toolset};
use serde_json::Value;

use super::delegated_auth::delegated_authorization_requirement;
use super::policy::{ToolAdmissionDecision, ToolAdmissionPolicy};
use super::tool_binding::{ToolBindingError, freeze_toolsets};

const MAX_DIRECT_ARGUMENT_BYTES: usize = 256 * 1_024;
// A direct result is carried inline in one 64 KiB terminal output frame. Keep
// enough headroom for the frame identities, digests and settlement proposal;
// larger toolkit responses need the artifact-backed output path instead.
const MAX_DIRECT_RESULT_BYTES: usize = 48 * 1_024;
const MAX_DIRECT_JSON_DEPTH: usize = 64;
const MAX_DIRECT_JSON_NODES: usize = 65_536;
const MAX_DIRECT_IDENTITY_BYTES: usize = 1_024;
const DIRECT_EXECUTION_TIMEOUT: Duration = Duration::from_secs(90);

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum DirectToolkitExecutionErrorCode {
    InvalidInput,
    InvalidConfiguration,
    ResourceExhausted,
    ToolNotSelected,
    ToolBlocked,
    SensitiveToolUnavailable,
    EffectfulToolUnavailable,
    AuthorizationRequired,
    DependencyUnavailable,
    ToolError,
    DeadlineExceeded,
}

/// A redacted direct-tool failure.
///
/// Toolkit settings, arguments, results, provider bodies and delegated tokens
/// are intentionally absent from both `Debug` and `Display`.
pub(crate) struct DirectToolkitExecutionError {
    code: DirectToolkitExecutionErrorCode,
    retryable: bool,
    authorization: Option<Box<super::DelegatedAuthorizationRequirement>>,
}

impl DirectToolkitExecutionError {
    #[must_use]
    pub(crate) const fn code(&self) -> DirectToolkitExecutionErrorCode {
        self.code
    }

    pub(crate) fn authorization(&self) -> Option<&super::DelegatedAuthorizationRequirement> {
        self.authorization.as_deref()
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }
}

impl fmt::Debug for DirectToolkitExecutionError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("DirectToolkitExecutionError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for DirectToolkitExecutionError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            DirectToolkitExecutionErrorCode::InvalidInput => {
                "the direct toolkit invocation is invalid"
            }
            DirectToolkitExecutionErrorCode::InvalidConfiguration => {
                "the direct toolkit target is invalid"
            }
            DirectToolkitExecutionErrorCode::ResourceExhausted => {
                "the direct toolkit invocation exceeds its approved bounds"
            }
            DirectToolkitExecutionErrorCode::ToolNotSelected => {
                "the requested toolkit operation is not selected"
            }
            DirectToolkitExecutionErrorCode::ToolBlocked => {
                "the requested toolkit operation is blocked by policy"
            }
            DirectToolkitExecutionErrorCode::SensitiveToolUnavailable => {
                "sensitive toolkit execution is not available on this path"
            }
            DirectToolkitExecutionErrorCode::EffectfulToolUnavailable => {
                "effectful toolkit execution is not available on this path"
            }
            DirectToolkitExecutionErrorCode::AuthorizationRequired => {
                "the toolkit operation requires delegated authorization"
            }
            DirectToolkitExecutionErrorCode::ToolError => "the toolkit operation failed",
            DirectToolkitExecutionErrorCode::DependencyUnavailable => {
                "the toolkit operation is temporarily unavailable"
            }
            DirectToolkitExecutionErrorCode::DeadlineExceeded => {
                "the toolkit operation exceeded its execution deadline"
            }
        })
    }
}

impl std::error::Error for DirectToolkitExecutionError {}

/// Immutable identity and arguments for one direct toolkit invocation.
///
/// `toolset_name` and `tool_name` are the original, unsanitized identities
/// retained beside the external MCP descriptor. The MCP-visible name is never
/// reversed to find a runtime target.
pub(crate) struct DirectToolkitInvocation {
    toolkit_type: String,
    toolset_name: String,
    tool_name: String,
    execution_id: String,
    call_id: String,
    arguments: Value,
}

impl DirectToolkitInvocation {
    pub(crate) fn new(
        toolkit_type: String,
        toolset_name: String,
        tool_name: String,
        execution_id: String,
        call_id: String,
        arguments: Value,
    ) -> Result<Self, DirectToolkitExecutionError> {
        for identity in [
            &toolkit_type,
            &toolset_name,
            &tool_name,
            &execution_id,
            &call_id,
        ] {
            if !valid_identity(identity) {
                return Err(failure(
                    DirectToolkitExecutionErrorCode::InvalidInput,
                    false,
                ));
            }
        }
        if !arguments.is_object() {
            return Err(failure(
                DirectToolkitExecutionErrorCode::InvalidInput,
                false,
            ));
        }
        validate_json(&arguments, MAX_DIRECT_ARGUMENT_BYTES)?;
        Ok(Self {
            toolkit_type,
            toolset_name,
            tool_name,
            execution_id,
            call_id,
            arguments,
        })
    }
}

/// Invoke one exact read-only operation from one exact materialized toolkit.
///
/// The function neither spawns nor retries. Cancelling the returned future
/// drops enumeration or provider execution in place. A provider-side read may
/// still have completed before cancellation, which is acceptable only because
/// the runtime independently verifies `Tool::is_read_only()` before dispatch.
pub(crate) async fn execute_read_only_toolsets(
    toolsets: Vec<Arc<dyn Toolset>>,
    invocation: DirectToolkitInvocation,
    policy: &ToolAdmissionPolicy,
) -> Result<Value, DirectToolkitExecutionError> {
    execute_toolsets(toolsets, invocation, policy, false).await
}

pub(crate) async fn execute_test_toolsets(
    toolsets: Vec<Arc<dyn Toolset>>,
    invocation: DirectToolkitInvocation,
    policy: &ToolAdmissionPolicy,
) -> Result<Value, DirectToolkitExecutionError> {
    execute_toolsets(toolsets, invocation, policy, true).await
}

async fn execute_toolsets(
    toolsets: Vec<Arc<dyn Toolset>>,
    invocation: DirectToolkitInvocation,
    policy: &ToolAdmissionPolicy,
    test_outcome: bool,
) -> Result<Value, DirectToolkitExecutionError> {
    if toolsets.len() != 1 {
        return Err(failure(
            DirectToolkitExecutionErrorCode::InvalidConfiguration,
            false,
        ));
    }
    let frozen = freeze_toolsets(toolsets, "elitea-external-mcp")
        .await
        .map_err(binding_failure)?;
    let Some(toolset) = frozen.first() else {
        return Err(failure(
            DirectToolkitExecutionErrorCode::InvalidConfiguration,
            false,
        ));
    };
    if toolset.name() != invocation.toolset_name {
        return Err(failure(
            DirectToolkitExecutionErrorCode::InvalidConfiguration,
            false,
        ));
    }

    let mut matching = toolset
        .tools()
        .iter()
        .filter(|tool| tool.name() == invocation.tool_name);
    let Some(tool) = matching.next().cloned() else {
        return Err(failure(
            DirectToolkitExecutionErrorCode::ToolNotSelected,
            false,
        ));
    };
    if matching.next().is_some() {
        return Err(failure(
            DirectToolkitExecutionErrorCode::InvalidConfiguration,
            false,
        ));
    }
    if policy.tool_decision(&invocation.toolkit_type, &invocation.tool_name)
        != ToolAdmissionDecision::Allowed
    {
        return Err(failure(DirectToolkitExecutionErrorCode::ToolBlocked, false));
    }
    if policy
        .sensitive_tool(
            &invocation.toolkit_type,
            &invocation.toolset_name,
            &invocation.tool_name,
        )
        .is_some()
    {
        return Err(failure(
            DirectToolkitExecutionErrorCode::SensitiveToolUnavailable,
            false,
        ));
    }
    if !tool.is_read_only() {
        return Err(failure(
            DirectToolkitExecutionErrorCode::EffectfulToolUnavailable,
            false,
        ));
    }
    // The out-of-loop ADK context cannot inherit a model Runner's user-scope
    // grants. A tool that declares such a grant must not bypass it merely
    // because this entry point is authenticated at the MCP transport.
    if !tool.required_scopes().is_empty() {
        return Err(failure(
            DirectToolkitExecutionErrorCode::AuthorizationRequired,
            false,
        ));
    }

    let context = Arc::new(
        SimpleToolContext::new("elitea-external-mcp")
            .with_session_id(invocation.execution_id)
            .with_function_call_id(invocation.call_id),
    );
    let context: Arc<dyn ToolContext> = context;
    let result = tokio::time::timeout(
        DIRECT_EXECUTION_TIMEOUT,
        tool.execute(context, invocation.arguments),
    )
    .await
    .map_err(|_| failure(DirectToolkitExecutionErrorCode::DeadlineExceeded, true))?
    .map_err(|error| invocation_failure(&error, test_outcome))?;
    validate_json(
        &result,
        if test_outcome {
            1024 * 1024
        } else {
            MAX_DIRECT_RESULT_BYTES
        },
    )?;
    Ok(result)
}

fn invocation_failure(
    error: &adk_rust::AdkError,
    test_outcome: bool,
) -> DirectToolkitExecutionError {
    if let Some(requirement) = delegated_authorization_requirement(error) {
        DirectToolkitExecutionError {
            code: DirectToolkitExecutionErrorCode::AuthorizationRequired,
            retryable: false,
            authorization: Some(Box::new(requirement)),
        }
    } else {
        failure(
            if test_outcome {
                DirectToolkitExecutionErrorCode::ToolError
            } else {
                DirectToolkitExecutionErrorCode::DependencyUnavailable
            },
            error.is_retryable(),
        )
    }
}

fn binding_failure(error: ToolBindingError) -> DirectToolkitExecutionError {
    match error {
        ToolBindingError::InvalidConfiguration => {
            failure(DirectToolkitExecutionErrorCode::InvalidConfiguration, false)
        }
        ToolBindingError::ResourceExhausted => {
            failure(DirectToolkitExecutionErrorCode::ResourceExhausted, false)
        }
        ToolBindingError::DependencyUnavailable => {
            failure(DirectToolkitExecutionErrorCode::DependencyUnavailable, true)
        }
    }
}

fn validate_json(value: &Value, max_bytes: usize) -> Result<(), DirectToolkitExecutionError> {
    let mut nodes = 0_usize;
    let mut stack = vec![(value, 1_usize)];
    while let Some((current, depth)) = stack.pop() {
        nodes = nodes
            .checked_add(1)
            .ok_or_else(|| failure(DirectToolkitExecutionErrorCode::ResourceExhausted, false))?;
        if nodes > MAX_DIRECT_JSON_NODES || depth > MAX_DIRECT_JSON_DEPTH {
            return Err(failure(
                DirectToolkitExecutionErrorCode::ResourceExhausted,
                false,
            ));
        }
        match current {
            Value::Array(values) => {
                stack.extend(values.iter().map(|value| (value, depth + 1)));
            }
            Value::Object(values) => {
                stack.extend(values.values().map(|value| (value, depth + 1)));
            }
            Value::Null | Value::Bool(_) | Value::Number(_) | Value::String(_) => {}
        }
    }
    let encoded = serde_json::to_vec(value)
        .map_err(|_| failure(DirectToolkitExecutionErrorCode::InvalidInput, false))?;
    if encoded.len() > max_bytes {
        return Err(failure(
            DirectToolkitExecutionErrorCode::ResourceExhausted,
            false,
        ));
    }
    Ok(())
}

fn valid_identity(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_DIRECT_IDENTITY_BYTES
        && !value.chars().any(char::is_control)
}

const fn failure(
    code: DirectToolkitExecutionErrorCode,
    retryable: bool,
) -> DirectToolkitExecutionError {
    DirectToolkitExecutionError {
        code,
        retryable,
        authorization: None,
    }
}
