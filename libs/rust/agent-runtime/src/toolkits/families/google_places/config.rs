use std::fmt;

use serde_json::{Map, Value};
use zeroize::Zeroizing;

const DEFAULT_RESULTS_COUNT: usize = 20;
const DEFAULT_RESULTS_COUNT_U64: u64 = 20;
const MAX_API_KEY_BYTES: usize = 8 * 1_024;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

/// A stable, data-free configuration failure category.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum GooglePlacesConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Invalid claim-materialized Google Places settings.
///
/// The API key is never retained in diagnostics, serialized, or cloned into a
/// long-lived public value. Places API (New) carries it in a sensitive request
/// header. Geocoding still requires a bounded query parameter; redirects are
/// disabled and diagnostics never retain the resulting URL.
pub(crate) struct GooglePlacesConfigError {
    code: GooglePlacesConfigErrorCode,
}

impl GooglePlacesConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> GooglePlacesConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for GooglePlacesConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("GooglePlacesConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for GooglePlacesConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            GooglePlacesConfigErrorCode::InvalidConfiguration => {
                "the Google Places toolkit configuration is invalid"
            }
            GooglePlacesConfigErrorCode::ResourceExhausted => {
                "the Google Places toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for GooglePlacesConfigError {}

/// Claim-scoped Google Places configuration for one authorized invocation.
///
/// This value is intentionally neither `Clone` nor `Debug`. One invocation
/// shares one client/pool and drops this secret owner when its toolset ends.
pub(crate) struct GooglePlacesToolkitConfig {
    api_key: Zeroizing<String>,
    results_count: usize,
    selected_tools: Vec<Box<str>>,
}

impl GooglePlacesToolkitConfig {
    /// Parse the exact materialized settings shape produced for the current SDK
    /// Google Places toolkit, with explicit resource bounds.
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, GooglePlacesConfigError> {
        let configuration = settings
            .get("google_places_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let api_key = configuration
            .get("api_key")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)?;
        validate_api_key(api_key)?;
        let results_count = match settings.get("results_count") {
            None | Some(Value::Null) => DEFAULT_RESULTS_COUNT,
            Some(value) => value
                .as_u64()
                .and_then(|value| usize::try_from(value.min(DEFAULT_RESULTS_COUNT_U64)).ok())
                .map(|value| {
                    if value == 0 {
                        DEFAULT_RESULTS_COUNT
                    } else {
                        value.min(DEFAULT_RESULTS_COUNT)
                    }
                })
                .ok_or_else(invalid_configuration)?,
        };
        let selected_tools = selected_tools(settings)?;
        Ok(Self {
            api_key: Zeroizing::new(api_key.to_owned()),
            results_count,
            selected_tools,
        })
    }

    #[must_use]
    pub(crate) const fn results_count(&self) -> usize {
        self.results_count
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }

    pub(super) fn api_key(&self) -> &str {
        &self.api_key
    }
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, GooglePlacesConfigError> {
    let Some(value) = settings.get("selected_tools") else {
        return Ok(Vec::new());
    };
    let values = value.as_array().ok_or_else(invalid_configuration)?;
    if values.len() > MAX_SELECTED_TOOLS {
        return Err(resource_exhausted());
    }
    let mut selected = Vec::with_capacity(values.len().min(2));
    for value in values {
        let name = value.as_str().ok_or_else(invalid_configuration)?;
        if name.is_empty()
            || name.len() > MAX_TOOL_NAME_BYTES
            || name.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n'))
        {
            return Err(if name.len() > MAX_TOOL_NAME_BYTES {
                resource_exhausted()
            } else {
                invalid_configuration()
            });
        }
        if !selected
            .iter()
            .map(AsRef::as_ref)
            .any(|existing: &str| existing == name)
        {
            selected.push(name.into());
        }
    }
    Ok(selected)
}

fn validate_api_key(value: &str) -> Result<(), GooglePlacesConfigError> {
    if value.len() > MAX_API_KEY_BYTES {
        return Err(resource_exhausted());
    }
    if value.is_empty() || !value.bytes().all(|byte| (0x21..=0x7e).contains(&byte)) {
        return Err(invalid_configuration());
    }
    Ok(())
}

const fn invalid_configuration() -> GooglePlacesConfigError {
    GooglePlacesConfigError {
        code: GooglePlacesConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> GooglePlacesConfigError {
    GooglePlacesConfigError {
        code: GooglePlacesConfigErrorCode::ResourceExhausted,
    }
}
