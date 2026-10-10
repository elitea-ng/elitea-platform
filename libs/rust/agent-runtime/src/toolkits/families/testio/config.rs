use std::collections::HashSet;
use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_URL_BYTES: usize = 2 * 1_024;
const MAX_TOKEN_BYTES: usize = 16 * 1_024;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum TestIoConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure that never carries the endpoint or token.
pub(crate) struct TestIoConfigError {
    code: TestIoConfigErrorCode,
}

impl TestIoConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> TestIoConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for TestIoConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("TestIoConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for TestIoConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            TestIoConfigErrorCode::InvalidConfiguration => {
                "the TestIO toolkit configuration is invalid"
            }
            TestIoConfigErrorCode::ResourceExhausted => {
                "the TestIO toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for TestIoConfigError {}

/// Invocation-scoped Test IO authority materialized from one accepted claim.
///
/// The SDK reads `testio_configuration.{endpoint,api_key}`; Main redeems the
/// vault-sealed `api_key` before the worker receives it. Non-`Clone` and
/// non-`Debug` so the token cannot be copied or logged.
pub(crate) struct TestIoToolkitConfig {
    endpoint: Url,
    api_key: Zeroizing<String>,
    selected_tools: Vec<Box<str>>,
}

impl TestIoToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, TestIoConfigError> {
        let configuration = settings
            .get("testio_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let endpoint = configuration
            .get("endpoint")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)
            .and_then(parse_endpoint)?;
        let api_key = configuration
            .get("api_key")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)?;
        validate_token(api_key)?;
        Ok(Self {
            endpoint,
            api_key: Zeroizing::new(api_key.to_owned()),
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) const fn endpoint(&self) -> &Url {
        &self.endpoint
    }

    pub(super) fn api_key(&self) -> &str {
        &self.api_key
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

/// The SDK strips trailing slashes and appends `/customer/v2/...`; a path
/// prefix is kept. Only HTTPS origins without userinfo, query or fragment
/// are admitted (the egress policy every Rust family applies).
fn parse_endpoint(value: &str) -> Result<Url, TestIoConfigError> {
    let value = value.trim();
    if value.len() > MAX_URL_BYTES {
        return Err(resource_exhausted());
    }
    if value.chars().any(char::is_control) || value.contains(['\\', '%']) {
        return Err(invalid_configuration());
    }
    let mut url = Url::parse(value).map_err(|_| invalid_configuration())?;
    if url.scheme() != "https"
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || url
            .path()
            .split('/')
            .any(|segment| matches!(segment, "." | ".."))
    {
        return Err(invalid_configuration());
    }
    let path = url.path().trim_end_matches('/').to_owned();
    url.set_path(&path);
    Ok(url)
}

fn validate_token(value: &str) -> Result<(), TestIoConfigError> {
    if value.len() > MAX_TOKEN_BYTES {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || !value.bytes().all(|byte| (0x21..=0x7e).contains(&byte)) {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, TestIoConfigError> {
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

const fn invalid_configuration() -> TestIoConfigError {
    TestIoConfigError {
        code: TestIoConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> TestIoConfigError {
    TestIoConfigError {
        code: TestIoConfigErrorCode::ResourceExhausted,
    }
}
