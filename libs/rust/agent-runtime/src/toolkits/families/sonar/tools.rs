use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{SonarApi, SonarClient, SonarClientError};
use super::config::{SonarConfigError, SonarToolkitConfig};

const GET_SONAR_DATA: &str = "get_sonar_data";
const ISSUE_SEARCH_PATH: &str = "/api/issues/search";
const MAX_PARAMS_BYTES: usize = 64 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 1_000;

/// Stable family-toolset construction failure category.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SonarToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Safe construction error for the complete Sonar read-only family.
pub(crate) struct SonarToolsetError {
    code: SonarToolsetErrorCode,
}

impl SonarToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> SonarToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for SonarToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SonarToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for SonarToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            SonarToolsetErrorCode::InvalidConfiguration => {
                "the Sonar toolkit configuration is invalid"
            }
            SonarToolsetErrorCode::ResourceExhausted => {
                "the Sonar toolkit configuration exceeds its approved limit"
            }
            SonarToolsetErrorCode::UnsupportedSelection => {
                "the selected Sonar tool profile is not supported"
            }
            SonarToolsetErrorCode::Client => "the Sonar client could not be created",
            SonarToolsetErrorCode::InvalidDefinition => "the Sonar ADK tool definition is invalid",
        })
    }
}

impl std::error::Error for SonarToolsetError {}

impl From<SonarConfigError> for SonarToolsetError {
    fn from(source: SonarConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::SonarConfigErrorCode::InvalidConfiguration => {
                    SonarToolsetErrorCode::InvalidConfiguration
                }
                super::config::SonarConfigErrorCode::ResourceExhausted => {
                    SonarToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<SonarClientError> for SonarToolsetError {
    fn from(_: SonarClientError) -> Self {
        Self {
            code: SonarToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for SonarToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: SonarToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the complete capability-disabled Sonar read-only toolset.
///
/// Empty selection means the one current public SDK tool. Deployment policy is
/// still applied to the concrete native ADK action.
pub(crate) fn build_sonar_read_only_toolset(
    toolkit_name: &str,
    config: SonarToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, SonarToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let client: Arc<dyn SonarApi> = Arc::new(SonarClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), SonarToolsetError> {
    if selected.iter().any(|name| name.as_ref() != GET_SONAR_DATA) {
        return Err(SonarToolsetError {
            code: SonarToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn SonarApi>,
) -> Result<BasicToolset, SonarToolsetError> {
    let include = selected.is_empty() || selected.iter().any(|name| name == GET_SONAR_DATA);
    let tools: Vec<Arc<dyn Tool>> = if include {
        vec![Arc::new(SonarTool::new(toolkit_name, Arc::clone(client)))]
    } else {
        Vec::new()
    };
    admit_materialized_toolset(toolkit_name, "sonar", policy, tools).map_err(Into::into)
}

struct SonarTool {
    client: Arc<dyn SonarApi>,
    description: Box<str>,
}

impl SonarTool {
    fn new(toolkit_name: &str, client: Arc<dyn SonarApi>) -> Self {
        let description = format!(
            "Toolkit: {toolkit_name}\nSearch the configured Sonar project through the bounded issue-search endpoint."
        );
        Self {
            client,
            description: description
                .chars()
                .take(MAX_DESCRIPTION_BYTES)
                .collect::<String>()
                .into_boxed_str(),
        }
    }
}

#[async_trait]
impl Tool for SonarTool {
    fn name(&self) -> &str {
        GET_SONAR_DATA
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        true
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(parameters_schema())
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        reject_unknown_keys(arguments)?;
        let relative_url = arguments
            .get("relative_url")
            .and_then(Value::as_str)
            .filter(|value| *value == ISSUE_SEARCH_PATH)
            .ok_or_else(invalid_arguments)?;
        let params = match arguments.get("params") {
            None | Some(Value::Null) => None,
            Some(Value::String(value)) if value.len() <= MAX_PARAMS_BYTES => Some(value.as_str()),
            _ => return Err(invalid_arguments()),
        };
        self.client
            .get_sonar_data(relative_url, params)
            .await
            .map_err(SonarClientError::into_adk)
    }
}

fn parameters_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "relative_url": {
                "type": "string",
                "enum": [ISSUE_SEARCH_PATH],
                "description": "The only supported Sonar REST endpoint."
            },
            "params": {
                "type": ["string", "null"],
                "maxLength": MAX_PARAMS_BYTES,
                "default": null,
                "description": "Optional Sonar issue-search query parameters encoded as a JSON object string. Common allowlisted filters include severities, statuses, types, tags, assignees, p, and ps; for example {\"severities\":[\"CRITICAL\"],\"p\":1,\"ps\":100}. The configured project always overrides componentKeys, and the result is one bounded raw Sonar issue-search object."
            }
        },
        "required": ["relative_url"],
        "additionalProperties": false
    })
}

fn reject_unknown_keys(arguments: &Map<String, Value>) -> Result<(), AdkError> {
    if arguments
        .keys()
        .any(|key| !matches!(key.as_str(), "relative_url" | "params"))
    {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "sonar.arguments.invalid",
        "the Sonar tool arguments are invalid",
    )
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn SonarApi>,
) -> Result<BasicToolset, SonarToolsetError> {
    build_with_api(toolkit_name, selected, policy, client)
}
