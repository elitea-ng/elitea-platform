//! Claim-scoped direct toolkit materialization and read execution.

use std::fmt;
use std::sync::Arc;

use serde_json::{Map, Value};

use super::direct_execution::{
    DirectToolkitExecutionErrorCode, DirectToolkitInvocation, execute_read_only_toolsets,
};
use super::{
    AdkHttpMcpConnector, DirectToolkitRequest, FrozenToolKind, McpConnector,
    McpMaterializationErrorCode, ToolsetMaterializationErrorCode,
    materialize_configured_toolsets_with_tokens_and_authorization,
    materialize_mcp_toolsets_with_tokens_and_authorization,
};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum DirectToolkitRuntimeErrorCode {
    InvalidInput,
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedToolkit,
    ToolNotSelected,
    ToolBlocked,
    SensitiveToolUnavailable,
    EffectfulToolUnavailable,
    AuthorizationRequired,
    DependencyUnavailable,
    DeadlineExceeded,
}

#[derive(Clone, Copy)]
pub(crate) struct DirectToolkitRuntimeError {
    code: DirectToolkitRuntimeErrorCode,
    retryable: bool,
}

impl DirectToolkitRuntimeError {
    #[must_use]
    pub(crate) const fn code(self) -> DirectToolkitRuntimeErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(self) -> bool {
        self.retryable
    }
}

impl fmt::Debug for DirectToolkitRuntimeError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("DirectToolkitRuntimeError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for DirectToolkitRuntimeError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            DirectToolkitRuntimeErrorCode::InvalidInput => "the direct toolkit input is invalid",
            DirectToolkitRuntimeErrorCode::InvalidConfiguration => {
                "the direct toolkit configuration is invalid"
            }
            DirectToolkitRuntimeErrorCode::ResourceExhausted => {
                "the direct toolkit execution exceeds its approved bounds"
            }
            DirectToolkitRuntimeErrorCode::UnsupportedToolkit => {
                "the direct toolkit family is not supported by this worker"
            }
            DirectToolkitRuntimeErrorCode::ToolNotSelected => {
                "the direct toolkit operation is not selected"
            }
            DirectToolkitRuntimeErrorCode::ToolBlocked => "the direct toolkit operation is blocked",
            DirectToolkitRuntimeErrorCode::SensitiveToolUnavailable => {
                "sensitive direct toolkit execution is unavailable"
            }
            DirectToolkitRuntimeErrorCode::EffectfulToolUnavailable => {
                "effectful direct toolkit execution is unavailable"
            }
            DirectToolkitRuntimeErrorCode::AuthorizationRequired => {
                "the direct toolkit operation requires authorization"
            }
            DirectToolkitRuntimeErrorCode::DependencyUnavailable => {
                "the direct toolkit dependency is unavailable"
            }
            DirectToolkitRuntimeErrorCode::DeadlineExceeded => {
                "the direct toolkit operation exceeded its deadline"
            }
        })
    }
}

impl std::error::Error for DirectToolkitRuntimeError {}

pub(crate) struct DirectToolkitRuntime {
    mcp_connector: Arc<dyn McpConnector>,
}

impl Default for DirectToolkitRuntime {
    fn default() -> Self {
        Self {
            mcp_connector: Arc::new(AdkHttpMcpConnector::new()),
        }
    }
}

impl DirectToolkitRuntime {
    #[cfg(test)]
    pub(crate) fn with_mcp_connector(mcp_connector: Arc<dyn McpConnector>) -> Self {
        Self { mcp_connector }
    }

    /// Materialize and execute exactly one read-only selected operation.
    ///
    /// No retry occurs here. Redis redelivery may repeat the provider read only
    /// because the final tool object is independently required to declare
    /// `is_read_only()` immediately before invocation.
    pub(crate) async fn execute(
        &self,
        request: &DirectToolkitRequest,
        execution_id: &str,
        call_id: &str,
    ) -> Result<Value, DirectToolkitRuntimeError> {
        let snapshot = request.snapshot().map_err(|error| match error.code() {
            super::DirectToolkitRequestErrorCode::InvalidInput
            | super::DirectToolkitRequestErrorCode::IncompatibleVersion => {
                failure(DirectToolkitRuntimeErrorCode::InvalidInput, false)
            }
            super::DirectToolkitRequestErrorCode::ResourceExhausted => {
                failure(DirectToolkitRuntimeErrorCode::ResourceExhausted, false)
            }
        })?;
        let kind = snapshot
            .iter()
            .next()
            .map(super::snapshot::FrozenToolReference::kind)
            .ok_or_else(|| failure(DirectToolkitRuntimeErrorCode::InvalidInput, false))?;
        let admitted = snapshot.apply_policy(request.policy());
        if admitted.len() != 1 {
            return Err(failure(DirectToolkitRuntimeErrorCode::ToolBlocked, false));
        }
        let tokens = Map::new();
        let toolsets = match kind {
            FrozenToolKind::Configured => {
                let (toolsets, _) = materialize_configured_toolsets_with_tokens_and_authorization(
                    &admitted,
                    request.policy(),
                    &tokens,
                )
                .map_err(|error| match error.code() {
                    ToolsetMaterializationErrorCode::InvalidConfiguration => {
                        failure(DirectToolkitRuntimeErrorCode::InvalidConfiguration, false)
                    }
                    ToolsetMaterializationErrorCode::UnsupportedToolkit => {
                        failure(DirectToolkitRuntimeErrorCode::UnsupportedToolkit, false)
                    }
                    ToolsetMaterializationErrorCode::ResourceExhausted => {
                        failure(DirectToolkitRuntimeErrorCode::ResourceExhausted, false)
                    }
                })?;
                toolsets
            }
            FrozenToolKind::Mcp => {
                let (toolsets, _) = materialize_mcp_toolsets_with_tokens_and_authorization(
                    &admitted,
                    self.mcp_connector.as_ref(),
                    request.policy(),
                    &tokens,
                )
                .await
                .map_err(|error| match error.code() {
                    McpMaterializationErrorCode::InvalidConfiguration => {
                        failure(DirectToolkitRuntimeErrorCode::InvalidConfiguration, false)
                    }
                    McpMaterializationErrorCode::ResourceExhausted => {
                        failure(DirectToolkitRuntimeErrorCode::ResourceExhausted, false)
                    }
                    McpMaterializationErrorCode::AuthorizationRequired => {
                        failure(DirectToolkitRuntimeErrorCode::AuthorizationRequired, false)
                    }
                    McpMaterializationErrorCode::DependencyUnavailable => {
                        failure(DirectToolkitRuntimeErrorCode::DependencyUnavailable, true)
                    }
                    McpMaterializationErrorCode::UnsupportedAuthority => {
                        failure(DirectToolkitRuntimeErrorCode::UnsupportedToolkit, false)
                    }
                })?;
                toolsets
            }
            FrozenToolKind::Application => {
                return Err(failure(
                    DirectToolkitRuntimeErrorCode::UnsupportedToolkit,
                    false,
                ));
            }
        };

        let invocation = DirectToolkitInvocation::new(
            request.toolkit_type().to_owned(),
            request.toolkit_name().to_owned(),
            request.tool_name().to_owned(),
            execution_id.to_owned(),
            call_id.to_owned(),
            request.arguments().clone(),
        )
        .map_err(|error| map_execution_error(&error))?;
        execute_read_only_toolsets(toolsets, invocation, request.policy())
            .await
            .map_err(|error| map_execution_error(&error))
    }
}

fn map_execution_error(
    error: &super::direct_execution::DirectToolkitExecutionError,
) -> DirectToolkitRuntimeError {
    let code = match error.code() {
        DirectToolkitExecutionErrorCode::InvalidInput => {
            DirectToolkitRuntimeErrorCode::InvalidInput
        }
        DirectToolkitExecutionErrorCode::InvalidConfiguration => {
            DirectToolkitRuntimeErrorCode::InvalidConfiguration
        }
        DirectToolkitExecutionErrorCode::ResourceExhausted => {
            DirectToolkitRuntimeErrorCode::ResourceExhausted
        }
        DirectToolkitExecutionErrorCode::ToolNotSelected => {
            DirectToolkitRuntimeErrorCode::ToolNotSelected
        }
        DirectToolkitExecutionErrorCode::ToolBlocked => DirectToolkitRuntimeErrorCode::ToolBlocked,
        DirectToolkitExecutionErrorCode::SensitiveToolUnavailable => {
            DirectToolkitRuntimeErrorCode::SensitiveToolUnavailable
        }
        DirectToolkitExecutionErrorCode::EffectfulToolUnavailable => {
            DirectToolkitRuntimeErrorCode::EffectfulToolUnavailable
        }
        DirectToolkitExecutionErrorCode::AuthorizationRequired => {
            DirectToolkitRuntimeErrorCode::AuthorizationRequired
        }
        DirectToolkitExecutionErrorCode::DependencyUnavailable => {
            DirectToolkitRuntimeErrorCode::DependencyUnavailable
        }
        DirectToolkitExecutionErrorCode::DeadlineExceeded => {
            DirectToolkitRuntimeErrorCode::DeadlineExceeded
        }
    };
    failure(code, error.retryable())
}

const fn failure(
    code: DirectToolkitRuntimeErrorCode,
    retryable: bool,
) -> DirectToolkitRuntimeError {
    DirectToolkitRuntimeError { code, retryable }
}
