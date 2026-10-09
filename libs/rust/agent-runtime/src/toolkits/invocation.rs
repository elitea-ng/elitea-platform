use std::fmt;
use std::io::{self, Write};
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::Value;
use tracing::Instrument as _;

use super::policy::{ToolAdmissionDecision, ToolAdmissionPolicy};

const MAX_MATERIALIZED_TOOLS: usize = 1_024;
const MAX_TOOL_IDENTITY_BYTES: usize = 1_024;
const MAX_TOOL_DESCRIPTION_BYTES: usize = 64 * 1_024;
const MAX_TOOL_SCHEMA_BYTES: usize = 256 * 1_024;
const MAX_TOOL_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_TOOL_JSON_DEPTH: usize = 64;
const MAX_TOOL_JSON_NODES: usize = 65_536;
const MAX_TOOL_JSON_STRING_BYTES: usize = 64 * 1_024;

/// Stable, data-free failure categories for one materialized toolset.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum MaterializedToolsetErrorCode {
    InvalidDefinition,
    ResourceExhausted,
}

/// A malformed or over-limit ADK tool definition.
///
/// Tool schemas and descriptions can contain provider or customer metadata.
/// Diagnostics therefore expose only a stable category and safe message.
pub(crate) struct MaterializedToolsetError {
    code: MaterializedToolsetErrorCode,
}

impl MaterializedToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> MaterializedToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for MaterializedToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("MaterializedToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for MaterializedToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self.code {
            MaterializedToolsetErrorCode::InvalidDefinition => {
                formatter.write_str("the materialized tool definition is invalid")
            }
            MaterializedToolsetErrorCode::ResourceExhausted => {
                formatter.write_str("the materialized toolset exceeds its approved limit")
            }
        }
    }
}

impl std::error::Error for MaterializedToolsetError {}

/// Admit already-materialized Elitea actions into ADK's native basic toolset.
///
/// Whole-toolkit policy is evaluated before a family creates clients or
/// credentials. This second stage filters concrete action names, freezes their
/// model-visible metadata, and wraps execution with the same immutable policy
/// generation. Blocked actions are omitted, matching the current SDK catalog.
///
/// This boundary intentionally accepts native ADK [`Tool`] values. Toolkit
/// families remain responsible for configuration validation, claim-scoped
/// credential redemption, connection checks, family-specific result bounds,
/// and mapping their business operations into those tools.
pub(crate) fn admit_materialized_toolset(
    toolset_name: &str,
    toolkit_type: &str,
    policy: &Arc<ToolAdmissionPolicy>,
    tools: Vec<Arc<dyn Tool>>,
) -> Result<BasicToolset, MaterializedToolsetError> {
    validate_identity(toolset_name)?;
    validate_identity(toolkit_type)?;
    if tools.len() > MAX_MATERIALIZED_TOOLS {
        return Err(resource_exhausted());
    }

    let toolkit_type: Arc<str> = Arc::from(toolkit_type);
    let mut admitted = Vec::with_capacity(tools.len());
    for tool in tools {
        validate_identity(tool.name())?;
        if policy.tool_decision(&toolkit_type, tool.name()) != ToolAdmissionDecision::Allowed {
            continue;
        }
        let wrapped: Arc<dyn Tool> = Arc::new(PolicyBoundTool::new(
            tool,
            Arc::clone(&toolkit_type),
            Arc::clone(policy),
        )?);
        admitted.push(wrapped);
    }

    Ok(BasicToolset::new(toolset_name, admitted))
}

/// One standard function tool bound to an immutable deployment policy.
///
/// The wrapper owns no task and performs no retry. Dropping its execution
/// future drops the delegated tool future. Toolkit implementations must still
/// make externally visible effects idempotent or fenced because cancellation
/// cannot prove that a remote effect did not happen. Expected, model-actionable
/// business failures are returned as bounded JSON tool results; `AdkError` is
/// reserved for infrastructure/control failures and is redacted here.
struct PolicyBoundTool {
    inner: Arc<dyn Tool>,
    toolkit_type: Arc<str>,
    policy: Arc<ToolAdmissionPolicy>,
    name: Box<str>,
    description: Box<str>,
    enhanced_description: Box<str>,
    parameters_schema: Option<Value>,
    response_schema: Option<Value>,
    long_running: bool,
    read_only: bool,
    concurrency_safe: bool,
}

impl PolicyBoundTool {
    fn new(
        inner: Arc<dyn Tool>,
        toolkit_type: Arc<str>,
        policy: Arc<ToolAdmissionPolicy>,
    ) -> Result<Self, MaterializedToolsetError> {
        if inner.is_builtin() {
            return Err(invalid_definition());
        }
        let description = inner.description();
        let enhanced_description = inner.enhanced_description();
        if description.len() > MAX_TOOL_DESCRIPTION_BYTES
            || enhanced_description.len() > MAX_TOOL_DESCRIPTION_BYTES
        {
            return Err(resource_exhausted());
        }

        let parameters_schema = inner.parameters_schema();
        let response_schema = inner.response_schema();
        let schema_bytes = parameters_schema
            .as_ref()
            .map_or(Ok(0), |schema| validate_json(schema, MAX_TOOL_SCHEMA_BYTES))?
            .checked_add(
                response_schema
                    .as_ref()
                    .map_or(Ok(0), |schema| validate_json(schema, MAX_TOOL_SCHEMA_BYTES))?,
            )
            .ok_or_else(resource_exhausted)?;
        if schema_bytes > MAX_TOOL_SCHEMA_BYTES {
            return Err(resource_exhausted());
        }

        Ok(Self {
            name: inner.name().into(),
            description: description.into(),
            enhanced_description: enhanced_description.into(),
            parameters_schema,
            response_schema,
            long_running: inner.is_long_running(),
            read_only: inner.is_read_only(),
            concurrency_safe: inner.is_concurrency_safe(),
            inner,
            toolkit_type,
            policy,
        })
    }
}

#[async_trait]
impl Tool for PolicyBoundTool {
    fn name(&self) -> &str {
        &self.name
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn enhanced_description(&self) -> String {
        self.enhanced_description.to_string()
    }

    fn is_long_running(&self) -> bool {
        self.long_running
    }

    fn parameters_schema(&self) -> Option<Value> {
        self.parameters_schema.clone()
    }

    fn response_schema(&self) -> Option<Value> {
        self.response_schema.clone()
    }

    fn required_scopes(&self) -> &[&str] {
        self.inner.required_scopes()
    }

    fn is_read_only(&self) -> bool {
        self.read_only
    }

    fn is_concurrency_safe(&self) -> bool {
        self.concurrency_safe
    }

    async fn execute(
        &self,
        context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        let span = tracing::info_span!(
            "agent.tool.invoke",
            toolkit_type = %self.toolkit_type,
            tool_name = %self.name,
            invocation_id = %context.invocation_id(),
            function_call_id = %context.function_call_id(),
            read_only = self.read_only,
            concurrency_safe = self.concurrency_safe,
            outcome = tracing::field::Empty,
            error_code = tracing::field::Empty,
            retryable = tracing::field::Empty,
        );
        let result = async {
            if self.policy.tool_decision(&self.toolkit_type, &self.name)
                != ToolAdmissionDecision::Allowed
            {
                return Err(AdkError::new(
                    ErrorComponent::Guardrail,
                    ErrorCategory::Forbidden,
                    "tool.policy.blocked",
                    "tool execution is not permitted by deployment policy",
                ));
            }

            validate_json(&arguments, MAX_TOOL_ARGUMENT_BYTES)
                .map_err(|error| tool_argument_error(&error))?;
            let Value::Object(mut arguments) = arguments else {
                return Err(invalid_tool_arguments());
            };
            // Current BaseAction semantics omit explicit nulls for optional
            // top-level fields before dispatch. Nested values remain untouched.
            arguments.retain(|_, value| !value.is_null());

            self.inner
                .execute(context, Value::Object(arguments))
                .await
                .map_err(|error| sanitize_tool_error(&error))
        }
        .instrument(span.clone())
        .await;
        match &result {
            Ok(_) => {
                span.record("outcome", "succeeded");
            }
            Err(error) => {
                span.record("outcome", "failed");
                span.record("error_code", error.code);
                span.record("retryable", error.is_retryable());
            }
        }
        result
    }
}

fn validate_identity(value: &str) -> Result<(), MaterializedToolsetError> {
    if value.is_empty()
        || value.len() > MAX_TOOL_IDENTITY_BYTES
        || value.chars().any(|character| character.is_ascii_control())
    {
        return Err(if value.len() > MAX_TOOL_IDENTITY_BYTES {
            resource_exhausted()
        } else {
            invalid_definition()
        });
    }
    Ok(())
}

fn validate_json(value: &Value, max_bytes: usize) -> Result<usize, MaterializedToolsetError> {
    let mut nodes = 0_usize;
    let mut stack = vec![(value, 1_usize)];
    while let Some((current, depth)) = stack.pop() {
        nodes = nodes.checked_add(1).ok_or_else(resource_exhausted)?;
        if nodes > MAX_TOOL_JSON_NODES || depth > MAX_TOOL_JSON_DEPTH {
            return Err(resource_exhausted());
        }
        match current {
            Value::String(text) if text.len() > MAX_TOOL_JSON_STRING_BYTES => {
                return Err(resource_exhausted());
            }
            Value::Array(values) => {
                stack.extend(values.iter().map(|value| (value, depth + 1)));
            }
            Value::Object(values) => {
                if values
                    .keys()
                    .any(|key| key.len() > MAX_TOOL_JSON_STRING_BYTES)
                {
                    return Err(resource_exhausted());
                }
                stack.extend(values.values().map(|value| (value, depth + 1)));
            }
            Value::Null | Value::Bool(_) | Value::Number(_) | Value::String(_) => {}
        }
    }

    let mut writer = BoundedJsonWriter::new(max_bytes);
    serde_json::to_writer(&mut writer, value).map_err(|_| {
        if writer.exceeded {
            resource_exhausted()
        } else {
            invalid_definition()
        }
    })?;
    Ok(writer.written)
}

struct BoundedJsonWriter {
    written: usize,
    limit: usize,
    exceeded: bool,
}

impl BoundedJsonWriter {
    const fn new(limit: usize) -> Self {
        Self {
            written: 0,
            limit,
            exceeded: false,
        }
    }
}

impl Write for BoundedJsonWriter {
    fn write(&mut self, buffer: &[u8]) -> io::Result<usize> {
        let Some(next) = self.written.checked_add(buffer.len()) else {
            self.exceeded = true;
            return Err(io::Error::other("JSON byte limit exceeded"));
        };
        if next > self.limit {
            self.exceeded = true;
            return Err(io::Error::other("JSON byte limit exceeded"));
        }
        self.written = next;
        Ok(buffer.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

fn sanitize_tool_error(error: &AdkError) -> AdkError {
    if let Some(authorization) =
        super::delegated_auth::preserve_delegated_authorization_error(error)
    {
        return authorization;
    }
    // Demo issue 3: a remote MCP failure keeps its bounded, redacted
    // description (`mcp_error.rs`), so the model can explain it or recover.
    if super::mcp_error::is_model_visible_mcp_error(error) {
        return AdkError::new(
            ErrorComponent::Tool,
            error.category,
            super::mcp_error::MCP_TOOL_ERROR_RESULT_CODE,
            error.message.clone(),
        )
        .with_retry(RetryHint {
            should_retry: false,
            retry_after_ms: None,
            max_attempts: Some(1),
        });
    }
    let (code, message) = match error.category {
        ErrorCategory::InvalidInput => (
            "tool.execution.invalid_input",
            "tool execution rejected invalid input",
        ),
        ErrorCategory::Unauthorized => (
            "tool.execution.unauthorized",
            "tool dependency authentication failed",
        ),
        ErrorCategory::Forbidden => (
            "tool.execution.forbidden",
            "tool execution is not authorized",
        ),
        ErrorCategory::NotFound => (
            "tool.execution.not_found",
            "the requested tool resource was not found",
        ),
        ErrorCategory::RateLimited => (
            "tool.execution.rate_limited",
            "tool dependency is rate limited",
        ),
        ErrorCategory::Timeout => ("tool.execution.timeout", "tool execution timed out"),
        ErrorCategory::Unavailable => (
            "tool.execution.unavailable",
            "tool dependency is unavailable",
        ),
        ErrorCategory::Cancelled => ("tool.execution.cancelled", "tool execution was cancelled"),
        ErrorCategory::Internal => ("tool.execution.internal", "tool execution failed"),
        ErrorCategory::Unsupported => (
            "tool.execution.unsupported",
            "the tool operation is not supported",
        ),
    };
    AdkError::new(ErrorComponent::Tool, error.category, code, message).with_retry(RetryHint {
        should_retry: error.retry.should_retry,
        retry_after_ms: error.retry.retry_after_ms,
        max_attempts: error.retry.max_attempts,
    })
}

fn tool_argument_error(error: &MaterializedToolsetError) -> AdkError {
    match error.code {
        MaterializedToolsetErrorCode::InvalidDefinition => invalid_tool_arguments(),
        MaterializedToolsetErrorCode::ResourceExhausted => AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::InvalidInput,
            "tool.arguments.resource_exhausted",
            "tool arguments exceed the approved limit",
        ),
    }
}

fn invalid_tool_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "tool.arguments.invalid",
        "tool arguments must be a bounded JSON object",
    )
}

const fn invalid_definition() -> MaterializedToolsetError {
    MaterializedToolsetError {
        code: MaterializedToolsetErrorCode::InvalidDefinition,
    }
}

const fn resource_exhausted() -> MaterializedToolsetError {
    MaterializedToolsetError {
        code: MaterializedToolsetErrorCode::ResourceExhausted,
    }
}
