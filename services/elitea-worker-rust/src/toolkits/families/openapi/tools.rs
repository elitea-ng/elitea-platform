use std::fmt;
use std::sync::Arc;

use adk_rust::tool::BasicToolset;
use adk_rust::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use async_trait::async_trait;
use serde_json::Value;

use crate::toolkits::delegated_auth::{
    DelegatedAuthorizationCatalog, DelegatedAuthorizationRequirement, delegated_authorization_error,
};
use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::{ToolAdmissionDecision, ToolAdmissionPolicy};

use super::client::{OpenApiApi, OpenApiClient, OpenApiClientError, OpenApiClientErrorCode};
use super::config::{OpenApiConfigError, OpenApiConfigErrorCode, OpenApiToolkitConfig};
use super::spec::OpenApiOperation;

const MAX_DESCRIPTION_BYTES: usize = 1_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum OpenApiToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedCapability,
    Client,
    InvalidDefinition,
}

pub(crate) struct OpenApiToolsetError {
    code: OpenApiToolsetErrorCode,
}

impl OpenApiToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> OpenApiToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for OpenApiToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("OpenApiToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for OpenApiToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            OpenApiToolsetErrorCode::InvalidConfiguration => {
                "the OpenAPI toolkit configuration is invalid"
            }
            OpenApiToolsetErrorCode::ResourceExhausted => {
                "the OpenAPI toolkit exceeds its approved limit"
            }
            OpenApiToolsetErrorCode::UnsupportedCapability => {
                "the OpenAPI toolkit requires an unavailable runtime authority"
            }
            OpenApiToolsetErrorCode::Client => "the OpenAPI client could not be created",
            OpenApiToolsetErrorCode::InvalidDefinition => {
                "the OpenAPI ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for OpenApiToolsetError {}

impl From<OpenApiConfigError> for OpenApiToolsetError {
    fn from(source: OpenApiConfigError) -> Self {
        Self {
            code: match source.code() {
                OpenApiConfigErrorCode::InvalidConfiguration => {
                    OpenApiToolsetErrorCode::InvalidConfiguration
                }
                OpenApiConfigErrorCode::ResourceExhausted => {
                    OpenApiToolsetErrorCode::ResourceExhausted
                }
                OpenApiConfigErrorCode::UnsupportedCapability => {
                    OpenApiToolsetErrorCode::UnsupportedCapability
                }
            },
        }
    }
}

impl From<OpenApiClientError> for OpenApiToolsetError {
    fn from(_: OpenApiClientError) -> Self {
        Self {
            code: OpenApiToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for OpenApiToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: OpenApiToolsetErrorCode::InvalidDefinition,
        }
    }
}

pub(crate) struct MaterializedOpenApiToolset {
    pub(crate) toolset: BasicToolset,
    pub(crate) delegated_authorization: DelegatedAuthorizationCatalog,
}

/// Build one dynamic `OpenAPI` tool per selected operation.
///
/// A missing delegated token materializes schema-complete guarded tools. The
/// normal ADK confirmation path therefore pauses the original model call; the
/// claim-fetched exact-base-URL token rebuild replaces those tools on resume.
pub(crate) fn build_openapi_toolset(
    toolkit_name: &str,
    config: OpenApiToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<MaterializedOpenApiToolset, OpenApiToolsetError> {
    let base_url = config.base_url().to_string();
    let requirement = config.auth().delegated_requirement().cloned();
    let operations = config
        .operations()
        .iter()
        .cloned()
        .map(Arc::new)
        .collect::<Vec<_>>();
    let mut delegated_authorization = DelegatedAuthorizationCatalog::default();
    let tools: Vec<Arc<dyn Tool>> = if let Some(requirement) = requirement {
        operations
            .into_iter()
            .map(|operation| {
                if policy.tool_decision("openapi", operation.name())
                    == ToolAdmissionDecision::Allowed
                {
                    delegated_authorization
                        .insert(operation.name(), requirement.clone())
                        .map_err(|()| OpenApiToolsetError {
                            code: OpenApiToolsetErrorCode::InvalidDefinition,
                        })?;
                }
                Ok(Arc::new(OpenApiAuthorizationRequiredTool::new(
                    operation,
                    toolkit_name,
                    &base_url,
                    requirement.clone(),
                )) as Arc<dyn Tool>)
            })
            .collect::<Result<Vec<_>, OpenApiToolsetError>>()?
    } else {
        let client: Arc<dyn OpenApiApi> = Arc::new(OpenApiClient::new(config.into_client_parts())?);
        operations
            .into_iter()
            .map(|operation| {
                Arc::new(OpenApiOperationTool::new(
                    operation,
                    toolkit_name,
                    &base_url,
                    Arc::clone(&client),
                )) as Arc<dyn Tool>
            })
            .collect()
    };
    let toolset = admit_materialized_toolset(toolkit_name, "openapi", policy, tools)?;
    Ok(MaterializedOpenApiToolset {
        toolset,
        delegated_authorization,
    })
}

struct OpenApiOperationTool {
    operation: Arc<OpenApiOperation>,
    client: Arc<dyn OpenApiApi>,
    description: Box<str>,
}

impl OpenApiOperationTool {
    fn new(
        operation: Arc<OpenApiOperation>,
        toolkit_name: &str,
        base_url: &str,
        client: Arc<dyn OpenApiApi>,
    ) -> Self {
        let description = bounded_description(toolkit_name, base_url, operation.description());
        Self {
            operation,
            client,
            description,
        }
    }
}

#[async_trait]
impl Tool for OpenApiOperationTool {
    fn name(&self) -> &str {
        self.operation.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        self.operation.is_read_only()
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(self.operation.parameters_schema())
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        self.client
            .execute(self.operation.as_ref(), arguments)
            .await
            .map_err(|error| {
                if error.code() == OpenApiClientErrorCode::Authentication
                    && let Some(requirement) = self.client.authorization()
                {
                    // A rejected delegated token needs the existing exact-call
                    // guard. Other auth modes and forbidden operations do not.
                    delegated_authorization_error(requirement)
                } else {
                    error.into_adk()
                }
            })
    }
}

struct OpenApiAuthorizationRequiredTool {
    operation: Arc<OpenApiOperation>,
    requirement: DelegatedAuthorizationRequirement,
    description: Box<str>,
}

impl OpenApiAuthorizationRequiredTool {
    fn new(
        operation: Arc<OpenApiOperation>,
        toolkit_name: &str,
        base_url: &str,
        requirement: DelegatedAuthorizationRequirement,
    ) -> Self {
        let description = bounded_description(toolkit_name, base_url, operation.description());
        Self {
            operation,
            requirement,
            description,
        }
    }
}

#[async_trait]
impl Tool for OpenApiAuthorizationRequiredTool {
    fn name(&self) -> &str {
        self.operation.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        self.operation.is_read_only()
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(self.operation.parameters_schema())
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_rust::Result<Value> {
        Err(delegated_authorization_error(&self.requirement))
    }
}

fn bounded_description(toolkit_name: &str, base_url: &str, operation: &str) -> Box<str> {
    let description = format!("{operation}\nToolkit: {toolkit_name}\nBase URL: {base_url}");
    let boundary = description
        .char_indices()
        .map(|(index, _)| index)
        .take_while(|index| *index <= MAX_DESCRIPTION_BYTES)
        .last()
        .unwrap_or(0);
    if description.len() <= MAX_DESCRIPTION_BYTES {
        description.into_boxed_str()
    } else {
        description[..boundary].to_owned().into_boxed_str()
    }
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "openapi.arguments.invalid",
        "the OpenAPI operation arguments are invalid",
    )
}

#[cfg(test)]
#[path = "tools_error_tests.rs"]
mod error_tests;
