use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{GooglePlacesApi, GooglePlacesClient, GooglePlacesClientError};
use super::config::{GooglePlacesConfigError, GooglePlacesToolkitConfig};

const PLACES: &str = "places";
const FIND_NEAR: &str = "find_near";
const DEFAULT_RADIUS_METERS: u32 = 3_000;
const MAX_RADIUS_METERS: u32 = 50_000;
const MAX_QUERY_BYTES: usize = 4 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 1_000;

/// A stable family-toolset construction failure category.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum GooglePlacesToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Safe construction error for the complete Google Places read-only family.
pub(crate) struct GooglePlacesToolsetError {
    code: GooglePlacesToolsetErrorCode,
}

impl GooglePlacesToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> GooglePlacesToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for GooglePlacesToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("GooglePlacesToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for GooglePlacesToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            GooglePlacesToolsetErrorCode::InvalidConfiguration => {
                "the Google Places toolkit configuration is invalid"
            }
            GooglePlacesToolsetErrorCode::ResourceExhausted => {
                "the Google Places toolkit configuration exceeds its approved limit"
            }
            GooglePlacesToolsetErrorCode::UnsupportedSelection => {
                "the selected Google Places tool profile is not supported"
            }
            GooglePlacesToolsetErrorCode::Client => "the Google Places client could not be created",
            GooglePlacesToolsetErrorCode::InvalidDefinition => {
                "the Google Places ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for GooglePlacesToolsetError {}

impl From<GooglePlacesConfigError> for GooglePlacesToolsetError {
    fn from(source: GooglePlacesConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::GooglePlacesConfigErrorCode::InvalidConfiguration => {
                    GooglePlacesToolsetErrorCode::InvalidConfiguration
                }
                super::config::GooglePlacesConfigErrorCode::ResourceExhausted => {
                    GooglePlacesToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<GooglePlacesClientError> for GooglePlacesToolsetError {
    fn from(_: GooglePlacesClientError) -> Self {
        Self {
            code: GooglePlacesToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for GooglePlacesToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: GooglePlacesToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the complete capability-disabled Google Places toolset.
///
/// Empty selection has the current SDK meaning of both public tools. Concrete
/// deployment policy is still applied to every native ADK action.
pub(crate) fn build_google_places_read_only_toolset(
    toolkit_name: &str,
    config: GooglePlacesToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, GooglePlacesToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let client: Arc<dyn GooglePlacesApi> = Arc::new(GooglePlacesClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), GooglePlacesToolsetError> {
    if selected
        .iter()
        .any(|name| !matches!(name.as_ref(), PLACES | FIND_NEAR))
    {
        return Err(GooglePlacesToolsetError {
            code: GooglePlacesToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn GooglePlacesApi>,
) -> Result<BasicToolset, GooglePlacesToolsetError> {
    let include_all = selected.is_empty();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(2);
    for kind in [GooglePlacesToolKind::Places, GooglePlacesToolKind::FindNear] {
        if include_all || selected.iter().any(|name| name == kind.name()) {
            tools.push(Arc::new(GooglePlacesTool::new(
                kind,
                toolkit_name,
                Arc::clone(client),
            )));
        }
    }
    admit_materialized_toolset(toolkit_name, "google_places", policy, tools).map_err(Into::into)
}

#[derive(Clone, Copy)]
enum GooglePlacesToolKind {
    Places,
    FindNear,
}

impl GooglePlacesToolKind {
    const fn name(self) -> &'static str {
        match self {
            Self::Places => PLACES,
            Self::FindNear => FIND_NEAR,
        }
    }
}

struct GooglePlacesTool {
    kind: GooglePlacesToolKind,
    client: Arc<dyn GooglePlacesApi>,
    description: Box<str>,
}

impl GooglePlacesTool {
    fn new(
        kind: GooglePlacesToolKind,
        toolkit_name: &str,
        client: Arc<dyn GooglePlacesApi>,
    ) -> Self {
        let action = match kind {
            GooglePlacesToolKind::Places => {
                "Search Google Places from a free-text place query and return bounded identity, address, contact, and website details."
            }
            GooglePlacesToolKind::FindNear => {
                "Geocode a starting address or location description, then find places from a free-text target query using a bounded radius bias. Results may fall outside the circle."
            }
        };
        let description = format!("Toolkit: {toolkit_name}\n{action}");
        Self {
            kind,
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
impl Tool for GooglePlacesTool {
    fn name(&self) -> &str {
        self.kind.name()
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
        Some(match self.kind {
            GooglePlacesToolKind::Places => places_schema(),
            GooglePlacesToolKind::FindNear => find_near_schema(),
        })
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        match self.kind {
            GooglePlacesToolKind::Places => {
                reject_unknown_keys(arguments, &["query"])?;
                let query = required_text(arguments, "query")?;
                self.client
                    .places(query)
                    .await
                    .map_err(GooglePlacesClientError::into_adk)
            }
            GooglePlacesToolKind::FindNear => {
                reject_unknown_keys(arguments, &["current_location_query", "target", "radius"])?;
                let current = required_text(arguments, "current_location_query")?;
                let target = required_text(arguments, "target")?;
                let radius = optional_radius(arguments)?;
                self.client
                    .find_near(current, target, radius)
                    .await
                    .map_err(GooglePlacesClientError::into_adk)
            }
        }
    }
}

fn places_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "query": {
                "type": "string",
                "minLength": 1,
                "maxLength": MAX_QUERY_BYTES,
                "description": "Free-text place query, for example `coffee near Kyiv` or `pharmacy open now in Boston`."
            }
        },
        "required": ["query"],
        "additionalProperties": false
    })
}

fn find_near_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "current_location_query": {
                "type": "string",
                "minLength": 1,
                "maxLength": MAX_QUERY_BYTES,
                "description": "Address or location description to geocode."
            },
            "target": {
                "type": "string",
                "minLength": 1,
                "maxLength": MAX_QUERY_BYTES,
                "description": "A free-text place or category query near the starting location, for example `coffee`, `EV charging`, or `pharmacy open now`."
            },
            "radius": {
                "type": ["integer", "null"],
                "minimum": 1,
                "maximum": MAX_RADIUS_METERS,
                "default": DEFAULT_RADIUS_METERS,
                "description": "Location-bias radius in meters; results can fall outside the circle."
            }
        },
        "required": ["current_location_query", "target"],
        "additionalProperties": false
    })
}

fn reject_unknown_keys(arguments: &Map<String, Value>, allowed: &[&str]) -> Result<(), AdkError> {
    if arguments.keys().any(|key| !allowed.contains(&key.as_str())) {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn required_text<'a>(arguments: &'a Map<String, Value>, key: &str) -> Result<&'a str, AdkError> {
    arguments
        .get(key)
        .and_then(Value::as_str)
        .filter(|value| valid_text(value))
        .ok_or_else(invalid_arguments)
}

fn optional_radius(arguments: &Map<String, Value>) -> Result<u32, AdkError> {
    match arguments.get("radius") {
        None | Some(Value::Null) => Ok(DEFAULT_RADIUS_METERS),
        Some(value) => value
            .as_u64()
            .and_then(|value| u32::try_from(value).ok())
            .filter(|value| (1..=MAX_RADIUS_METERS).contains(value))
            .ok_or_else(invalid_arguments),
    }
}

fn valid_text(value: &str) -> bool {
    !value.trim().is_empty()
        && value.len() <= MAX_QUERY_BYTES
        && !value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n'))
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "google_places.arguments.invalid",
        "the Google Places tool arguments are invalid",
    )
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn GooglePlacesApi>,
) -> Result<BasicToolset, GooglePlacesToolsetError> {
    build_with_api(toolkit_name, selected, policy, client)
}
