use std::collections::HashSet;
use std::fmt;
use std::net::Ipv4Addr;

use serde_json::{Map, Value};
use zeroize::Zeroizing;

pub(in crate::toolkits) const DEFAULT_SMTP_HOST: &str = "smtp.gmail.com";
pub(in crate::toolkits) const SMTP_PORT: u16 = 465;
const MAX_HOST_BYTES: usize = 253;
const MAX_USERNAME_BYTES: usize = 320;
pub(in crate::toolkits) const MAX_PASSWORD_BYTES: usize = 8 * 1_024;
const MAX_SELECTED_TOOLS: usize = 16;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum YagmailConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure that never carries SMTP authority or secrets.
pub(crate) struct YagmailConfigError {
    code: YagmailConfigErrorCode,
}

impl YagmailConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> YagmailConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for YagmailConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("YagmailConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for YagmailConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            YagmailConfigErrorCode::InvalidConfiguration => {
                "the Yagmail toolkit configuration is invalid"
            }
            YagmailConfigErrorCode::ResourceExhausted => {
                "the Yagmail toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for YagmailConfigError {}

/// Claim-scoped SMTP authority materialized from one frozen toolkit.
///
/// Main marks `password` as the only inline secret and redeems it only for the
/// accepted claim. This value is deliberately non-cloneable and non-debuggable.
pub(crate) struct YagmailToolkitConfig {
    host: Box<str>,
    username: Box<str>,
    password: Zeroizing<String>,
    selected_tools: Vec<Box<str>>,
}

impl YagmailToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, YagmailConfigError> {
        let host = optional_host(settings)?;
        let username =
            normalize_username(required_text(settings, "username", MAX_USERNAME_BYTES)?)?;
        let password = required_text(settings, "password", MAX_PASSWORD_BYTES)?;
        Ok(Self {
            host: host.into(),
            username: username.into(),
            password: Zeroizing::new(password.to_owned()),
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) fn host(&self) -> &str {
        &self.host
    }

    pub(super) fn username(&self) -> &str {
        &self.username
    }

    pub(super) fn password(&self) -> &str {
        &self.password
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

fn optional_host(settings: &Map<String, Value>) -> Result<&str, YagmailConfigError> {
    match settings.get("host") {
        None | Some(Value::Null) => Ok(DEFAULT_SMTP_HOST),
        Some(Value::String(value)) => {
            let value = value.trim();
            validate_host(value)?;
            Ok(value)
        }
        Some(_) => Err(invalid_configuration()),
    }
}

fn validate_host(value: &str) -> Result<(), YagmailConfigError> {
    if value.len() > MAX_HOST_BYTES {
        return Err(resource_exhausted());
    }
    if value.is_empty()
        || !value.is_ascii()
        || value.parse::<Ipv4Addr>().is_ok()
        || value.starts_with('.')
        || value.ends_with('.')
        || value.split('.').any(|label| {
            label.is_empty()
                || label.len() > 63
                || label.starts_with('-')
                || label.ends_with('-')
                || !label
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
        })
    {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn required_text<'a>(
    settings: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, YagmailConfigError> {
    let value = settings
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?;
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.is_empty() || value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n')) {
        return Err(invalid_configuration());
    }
    Ok(value)
}

fn normalize_username(value: &str) -> Result<String, YagmailConfigError> {
    let normalized = if value.contains('@') {
        value.to_owned()
    } else {
        let mut normalized = String::with_capacity(value.len() + "@gmail.com".len());
        normalized.push_str(value);
        normalized.push_str("@gmail.com");
        normalized
    };
    if normalized.len() > MAX_USERNAME_BYTES {
        return Err(resource_exhausted());
    }
    super::client::validate_mailbox(&normalized).map_err(|error| match error.code() {
        super::client::YagmailClientErrorCode::ResourceExhausted => resource_exhausted(),
        _ => invalid_configuration(),
    })?;
    Ok(normalized)
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, YagmailConfigError> {
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
        if value.is_empty()
            || value.len() > MAX_TOOL_NAME_BYTES
            || value.bytes().any(|byte| byte.is_ascii_control())
        {
            return Err(if value.len() > MAX_TOOL_NAME_BYTES {
                resource_exhausted()
            } else {
                invalid_configuration()
            });
        }
        if seen.insert(value) {
            selected.push(value.into());
        }
    }
    Ok(selected)
}

const fn invalid_configuration() -> YagmailConfigError {
    YagmailConfigError {
        code: YagmailConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> YagmailConfigError {
    YagmailConfigError {
        code: YagmailConfigErrorCode::ResourceExhausted,
    }
}
