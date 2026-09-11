//! Claim-scoped direct toolkit materialization and read execution.

use std::fmt;
use std::sync::Arc;

use serde_json::{Map, Value};

use super::direct_execution::{
    DirectToolkitExecutionErrorCode, DirectToolkitInvocation, execute_read_only_toolsets,
    execute_test_toolsets,
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
    ToolError,
    DeadlineExceeded,
}

#[derive(Clone)]
pub(crate) struct DirectToolkitRuntimeError {
    code: DirectToolkitRuntimeErrorCode,
    retryable: bool,
    authorization: Option<Box<super::DelegatedAuthorizationRequirement>>,
}

impl DirectToolkitRuntimeError {
    #[must_use]
    pub(crate) const fn code(&self) -> DirectToolkitRuntimeErrorCode {
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
            DirectToolkitRuntimeErrorCode::ToolError => "the toolkit operation failed",
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
        let toolsets = self.materialize(request).await?;
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
    pub(crate) async fn execute_test(
        &self,
        request: &DirectToolkitRequest,
        execution_id: &str,
        call_id: &str,
    ) -> Result<Value, DirectToolkitRuntimeError> {
        let toolsets = self.materialize(request).await?;
        let invocation = DirectToolkitInvocation::new(
            request.toolkit_type().to_owned(),
            request.toolkit_name().to_owned(),
            request.tool_name().to_owned(),
            execution_id.to_owned(),
            call_id.to_owned(),
            request.arguments().clone(),
        )
        .map_err(|error| map_execution_error(&error))?;
        execute_test_toolsets(toolsets, invocation, request.policy())
            .await
            .map_err(|error| map_execution_error(&error))
    }

    pub(crate) async fn discover(
        &self,
        request: &DirectToolkitRequest,
    ) -> Result<Value, DirectToolkitRuntimeError> {
        let toolsets = self.materialize(request).await?;
        let frozen = super::tool_binding::freeze_toolsets(toolsets, "elitea-toolkit-discovery")
            .await
            .map_err(|_| failure(DirectToolkitRuntimeErrorCode::DependencyUnavailable, true))?;
        let mut tools = Vec::new();
        let mut schemas = Map::new();
        for toolset in frozen {
            for tool in toolset.tools() {
                if schemas.contains_key(tool.name()) {
                    return Err(failure(
                        DirectToolkitRuntimeErrorCode::InvalidConfiguration,
                        false,
                    ));
                }
                tools
                    .push(serde_json::json!({"name":tool.name(),"description":tool.description()}));
                schemas.insert(
                    tool.name().to_owned(),
                    tool.parameters_schema()
                        .unwrap_or_else(|| serde_json::json!({})),
                );
            }
        }
        let result = serde_json::json!({"tools":tools,"args_schemas":schemas});
        if serde_json::to_vec(&result)
            .map_err(|_| failure(DirectToolkitRuntimeErrorCode::InvalidInput, false))?
            .len()
            > 1024 * 1024
        {
            return Err(failure(
                DirectToolkitRuntimeErrorCode::ResourceExhausted,
                false,
            ));
        }
        Ok(result)
    }

    async fn materialize(
        &self,
        request: &DirectToolkitRequest,
    ) -> Result<Vec<Arc<dyn adk_rust::Toolset>>, DirectToolkitRuntimeError> {
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
        let tokens = request.tokens();
        let toolsets = match kind {
            FrozenToolKind::Configured => {
                let (toolsets, authorization) =
                    materialize_configured_toolsets_with_tokens_and_authorization(
                        &admitted,
                        request.policy(),
                        tokens,
                    )
                    .await
                    .map_err(|error| match error.code() {
                        ToolsetMaterializationErrorCode::InvalidConfiguration => {
                            failure(DirectToolkitRuntimeErrorCode::InvalidConfiguration, false)
                        }
                        ToolsetMaterializationErrorCode::UnsupportedToolkit => {
                            failure(DirectToolkitRuntimeErrorCode::UnsupportedToolkit, false)
                        }
                        ToolsetMaterializationErrorCode::DependencyUnavailable => {
                            failure(DirectToolkitRuntimeErrorCode::DependencyUnavailable, true)
                        }
                        ToolsetMaterializationErrorCode::ResourceExhausted => {
                            failure(DirectToolkitRuntimeErrorCode::ResourceExhausted, false)
                        }
                    })?;
                if let Some(requirement) = authorization
                    .requirement_for_direct(request.toolkit_name(), request.tool_name())
                {
                    return Err(authorization_failure(requirement.clone()));
                }
                toolsets
            }
            FrozenToolKind::Mcp => {
                let (toolsets, authorization) =
                    materialize_mcp_toolsets_with_tokens_and_authorization(
                        &admitted,
                        self.mcp_connector.as_ref(),
                        request.policy(),
                        tokens,
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
                if let Some(requirement) = authorization
                    .requirement_for_direct(request.toolkit_name(), request.tool_name())
                {
                    return Err(authorization_failure(requirement.clone()));
                }
                toolsets
            }
            FrozenToolKind::Application => {
                return Err(failure(
                    DirectToolkitRuntimeErrorCode::UnsupportedToolkit,
                    false,
                ));
            }
        };

        if toolsets.is_empty() {
            return Err(failure(
                DirectToolkitRuntimeErrorCode::UnsupportedToolkit,
                false,
            ));
        }
        Ok(toolsets)
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
        DirectToolkitExecutionErrorCode::ToolError => DirectToolkitRuntimeErrorCode::ToolError,
        DirectToolkitExecutionErrorCode::DependencyUnavailable => {
            DirectToolkitRuntimeErrorCode::DependencyUnavailable
        }
        DirectToolkitExecutionErrorCode::DeadlineExceeded => {
            DirectToolkitRuntimeErrorCode::DeadlineExceeded
        }
    };
    if let Some(requirement) = error.authorization() {
        return authorization_failure(requirement.clone());
    }
    failure(code, error.retryable())
}

fn authorization_failure(
    requirement: super::DelegatedAuthorizationRequirement,
) -> DirectToolkitRuntimeError {
    DirectToolkitRuntimeError {
        code: DirectToolkitRuntimeErrorCode::AuthorizationRequired,
        retryable: false,
        authorization: Some(Box::new(requirement)),
    }
}

const fn failure(
    code: DirectToolkitRuntimeErrorCode,
    retryable: bool,
) -> DirectToolkitRuntimeError {
    DirectToolkitRuntimeError {
        code,
        retryable,
        authorization: None,
    }
}

#[cfg(test)]
mod shared_tests {
    use super::*;
    use serde_json::json;

    fn settings() -> Vec<u8> {
        serde_json::to_vec(&json!({
            "openapi_configuration": {},
            "spec": {"openapi":"3.0.3", "servers":[{"url":"https://example.invalid"}],
                "paths": {"/read":{"get":{"operationId":"read","responses":{"200":{"description":"ok"}}}},
                    "/write":{"post":{"operationId":"write","responses":{"200":{"description":"ok"}}}}}},
            "selected_tools": ["read"]
        })).expect("settings")
    }

    #[tokio::test]
    async fn standalone_discovery_reuses_native_selection_without_provider_execution() {
        let request = DirectToolkitRequest::parse_discovery(
            "openapi",
            &settings(),
            br#"{"toolkit_security":{}}"#,
        )
        .expect("request");
        let result = DirectToolkitRuntime::default()
            .discover(&request)
            .await
            .expect("descriptors");
        assert_eq!(result["tools"].as_array().expect("tools").len(), 1);
        assert_eq!(result["tools"][0]["name"], "read");
        assert!(result["args_schemas"].get("read").is_some());
        assert!(result["args_schemas"].get("write").is_none());
    }

    #[tokio::test]
    async fn test_authentication_guard_retains_exact_configuration_before_tool_execution() {
        let mut settings: Value = serde_json::from_slice(&settings()).expect("settings");
        settings["openapi_configuration"] = json!({
            "client_id":"client-id", "client_secret":"stored-secret", "configuration_uuid":"config-1",
            "oauth_discovery_endpoint":"https://login.example.test/tenant", "scope":"records.read"
        });
        let config = serde_json::to_vec(
            &json!({"id":7,"type":"openapi","toolkit_name":"saved","settings":settings}),
        )
        .expect("config");
        let request = DirectToolkitRequest::parse_call("openapi", "7", "read", &config, b"{}",
            br#"{"toolkit_security":{},"llm_model":"selected-model","llm_configuration":{"temperature":0.2}}"#).expect("request");
        let error = DirectToolkitRuntime::default()
            .execute_test(&request, "execution", "call")
            .await
            .expect_err("pre-invocation auth");
        let requirement = error.authorization().expect("safe challenge");
        assert_eq!(requirement.toolkit_name(), "saved");
        assert_eq!(requirement.toolkit_type(), "openapi");
        assert_eq!(requirement.server_url(), "https://example.invalid");
        assert_eq!(
            requirement.resource_metadata().expect("metadata")["configuration_uuid"],
            "config-1"
        );
        assert!(!format!("{error:?} {error}").contains("stored-secret"));
        let wrong_context = br#"{"toolkit_security":{},"mcp_tokens":{"config-2:https://login.example.test/tenant":{"access_token":"token"}}}"#;
        let wrong_request =
            DirectToolkitRequest::parse_call("openapi", "7", "read", &config, b"{}", wrong_context)
                .expect("request");
        assert!(
            DirectToolkitRuntime::default()
                .materialize(&wrong_request)
                .await
                .err()
                .expect("wrong authority")
                .authorization()
                .is_some()
        );
        let context = br#"{"toolkit_security":{},"mcp_tokens":{"config-1:https://login.example.test/tenant":{"access_token":"token"}}}"#;
        let authorized =
            DirectToolkitRequest::parse_call("openapi", "7", "read", &config, b"{}", context)
                .expect("request");
        assert_eq!(
            DirectToolkitRuntime::default()
                .materialize(&authorized)
                .await
                .expect("authorized toolset")
                .len(),
            1
        );
    }

    #[test]
    fn shared_requests_require_identity_and_authoritative_policy() {
        let config = serde_json::to_vec(&json!({"id":7,"type":"openapi","toolkit_name":"saved",
            "settings":serde_json::from_slice::<Value>(&settings()).expect("settings")}))
        .expect("config");
        assert!(
            DirectToolkitRequest::parse_call(
                "openapi",
                "8",
                "read",
                &config,
                b"{}",
                br#"{"toolkit_security":{}}"#
            )
            .is_err()
        );
        assert!(
            DirectToolkitRequest::parse_call("openapi", "7", "read", &config, b"{}", b"{}")
                .is_err()
        );
        assert!(
            DirectToolkitRequest::parse_call(
                "openapi",
                "7",
                "read",
                &config,
                b"{}",
                br#"{"toolkit_security":{"blocked_toolkits":["openapi"]}}"#
            )
            .is_err()
        );
        assert!(
            DirectToolkitRequest::parse_call(
                "openapi",
                "7",
                "read",
                &config,
                b"{}",
                br#"{"toolkit_security":{},"llm_model":"supplied-model"}"#
            )
            .is_ok()
        );
        assert!(
            DirectToolkitRequest::parse_call(
                "openapi",
                "7",
                "read",
                &config,
                b"{}",
                br#"{"toolkit_security":{}}"#
            )
            .is_ok()
        );
    }
}
