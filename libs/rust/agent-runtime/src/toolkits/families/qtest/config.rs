use std::collections::HashSet;
use std::fmt;

use crate::toolkits::families::https_base_url::{self, BasePath, BaseUrlError};
use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

/// `QtestApiWrapper.no_of_tests_shown_in_dql_search`'s default.
const DEFAULT_SHOWN_RESULTS: usize = 10;
const MAX_SHOWN_RESULTS: usize = 1_000;
const MAX_URL_BYTES: usize = 2 * 1_024;
const MAX_TOKEN_BYTES: usize = 16 * 1_024;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum QtestConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure that never carries the origin or token.
pub(crate) struct QtestConfigError {
    code: QtestConfigErrorCode,
}

impl QtestConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> QtestConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for QtestConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("QtestConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for QtestConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            QtestConfigErrorCode::InvalidConfiguration => {
                "the qTest toolkit configuration is invalid"
            }
            QtestConfigErrorCode::ResourceExhausted => {
                "the qTest toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for QtestConfigError {}

/// Invocation-scoped qTest Manager authority materialized from one accepted
/// claim: `qtest_configuration.{base_url,qtest_api_token}`, the
/// `qtest_project_id` (or legacy `project_id`) and the result count the DQL
/// tools show.
///
/// Non-`Clone` and non-`Debug` so the token cannot be copied or logged.
pub(crate) struct QtestToolkitConfig {
    base: Url,
    token: Zeroizing<String>,
    project_id: u64,
    shown_results: usize,
    selected_tools: Vec<Box<str>>,
}

impl QtestToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, QtestConfigError> {
        let configuration = settings
            .get("qtest_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let base = parse_base(
            configuration
                .get("base_url")
                .and_then(Value::as_str)
                .ok_or_else(invalid_configuration)?,
        )?;
        let token = configuration
            .get("qtest_api_token")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)?;
        if token.len() > MAX_TOKEN_BYTES {
            return Err(resource_exhausted());
        }
        if token.trim().is_empty() || !token.bytes().all(|byte| (0x21..=0x7e).contains(&byte)) {
            return Err(invalid_configuration());
        }
        // `settings.get('qtest_project_id', settings.get('project_id'))`.
        let project_id = settings
            .get("qtest_project_id")
            .or_else(|| settings.get("project_id"))
            .and_then(|value| match value {
                Value::Number(number) => number.as_u64(),
                Value::String(text) => text.trim().parse::<u64>().ok(),
                _ => None,
            })
            .filter(|project| *project > 0)
            .ok_or_else(invalid_configuration)?;
        let shown_results = match settings.get("no_of_tests_shown_in_dql_search") {
            None | Some(Value::Null) => DEFAULT_SHOWN_RESULTS,
            Some(value) => value
                .as_u64()
                .and_then(|value| usize::try_from(value).ok())
                .filter(|value| *value <= MAX_SHOWN_RESULTS)
                .ok_or_else(invalid_configuration)?,
        };
        Ok(Self {
            base,
            token: Zeroizing::new(token.to_owned()),
            project_id,
            shown_results,
            selected_tools: selected_tools(settings)?,
        })
    }

    /// The origin without a trailing slash (the SDK strips it because the
    /// generated client concatenates paths onto the host).
    pub(super) fn base_url(&self) -> &str {
        self.base.as_str().trim_end_matches('/')
    }

    pub(super) fn token(&self) -> &str {
        &self.token
    }

    pub(super) const fn project_id(&self) -> u64 {
        self.project_id
    }

    pub(super) const fn shown_results(&self) -> usize {
        self.shown_results
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

/// HTTPS origin, optional path prefix (the shared [`https_base_url`] rule).
fn parse_base(value: &str) -> Result<Url, QtestConfigError> {
    https_base_url::parse(value, BasePath::Prefix).map_err(|error| match error {
        BaseUrlError::TooLong => resource_exhausted(),
        BaseUrlError::Invalid | BaseUrlError::PlainHttp => invalid_configuration(),
    })
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, QtestConfigError> {
    let Some(value) = settings.get("selected_tools") else {
        return Ok(Vec::new());
    };
    if value.is_null() {
        return Ok(Vec::new());
    }
    let values = value.as_array().ok_or_else(invalid_configuration)?;
    if values.len() > MAX_SELECTED_TOOLS {
        return Err(resource_exhausted());
    }
    let mut seen = HashSet::with_capacity(values.len());
    let mut selected = Vec::with_capacity(values.len());
    for value in values {
        let value = value.as_str().ok_or_else(invalid_configuration)?;
        if value.is_empty() || value.len() > MAX_TOOL_NAME_BYTES || value.contains('\0') {
            return Err(invalid_configuration());
        }
        if seen.insert(value) {
            selected.push(value.into());
        }
    }
    Ok(selected)
}

const fn invalid_configuration() -> QtestConfigError {
    QtestConfigError {
        code: QtestConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> QtestConfigError {
    QtestConfigError {
        code: QtestConfigErrorCode::ResourceExhausted,
    }
}
