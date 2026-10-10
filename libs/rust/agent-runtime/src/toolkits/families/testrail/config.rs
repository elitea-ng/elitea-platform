use std::collections::HashSet;
use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_URL_BYTES: usize = 2 * 1_024;
const MAX_EMAIL_BYTES: usize = 1_024;
const MAX_SECRET_BYTES: usize = 16 * 1_024;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum TestRailConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure that never carries the origin or credential.
pub(crate) struct TestRailConfigError {
    code: TestRailConfigErrorCode,
}

impl TestRailConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> TestRailConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for TestRailConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("TestRailConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for TestRailConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            TestRailConfigErrorCode::InvalidConfiguration => {
                "the TestRail toolkit configuration is invalid"
            }
            TestRailConfigErrorCode::ResourceExhausted => {
                "the TestRail toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for TestRailConfigError {}

/// Invocation-scoped `TestRail` authority materialized from one accepted
/// claim.
///
/// The SDK reads `testrail_configuration.{url,email,password}`; Main redeems
/// the vault-sealed password (or API key) before the worker receives it.
/// Non-`Clone` and non-`Debug` so the credential cannot be copied or logged.
pub(crate) struct TestRailToolkitConfig {
    base: Url,
    email: Box<str>,
    password: Zeroizing<String>,
    selected_tools: Vec<Box<str>>,
}

impl TestRailToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, TestRailConfigError> {
        let configuration = settings
            .get("testrail_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let base = parse_base(required_text(configuration, "url", MAX_URL_BYTES)?)?;
        let email = required_text(configuration, "email", MAX_EMAIL_BYTES)?;
        let password = required_text(configuration, "password", MAX_SECRET_BYTES)?;
        Ok(Self {
            base,
            email: email.into(),
            password: Zeroizing::new(password.to_owned()),
            selected_tools: selected_tools(settings)?,
        })
    }

    /// The instance root without a trailing slash, as the SDK prints it in
    /// every tool description.
    pub(super) fn instance(&self) -> &str {
        self.base.as_str().trim_end_matches('/')
    }

    pub(super) fn email(&self) -> &str {
        &self.email
    }

    pub(super) fn password(&self) -> &str {
        &self.password
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

/// `testrail_api` strips trailing slashes and appends
/// `/index.php?/api/v2/`; a path prefix (an instance under `/testrail`) is
/// kept. Only HTTPS origins without userinfo, query or fragment are admitted.
fn parse_base(value: &str) -> Result<Url, TestRailConfigError> {
    let value = value.trim();
    if value.chars().any(char::is_control) || value.contains(['\\', '%', '?', '#']) {
        return Err(invalid_configuration());
    }
    let mut url = Url::parse(value).map_err(|_| invalid_configuration())?;
    if url.scheme() != "https"
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
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

fn required_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, TestRailConfigError> {
    let value = object
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?;
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n')) {
        return Err(invalid_configuration());
    }
    Ok(value)
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, TestRailConfigError> {
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

const fn invalid_configuration() -> TestRailConfigError {
    TestRailConfigError {
        code: TestRailConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> TestRailConfigError {
    TestRailConfigError {
        code: TestRailConfigErrorCode::ResourceExhausted,
    }
}
