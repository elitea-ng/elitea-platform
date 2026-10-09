use std::collections::HashSet;
use std::fmt;

use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_TENANT_ID_BYTES: usize = 253;
const MAX_CLIENT_SECRET_BYTES: usize = 16 * 1_024;
const MAX_SELECTED_TOOLS: usize = 16;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum AzureConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure without tenant, subscription, client, or secret data.
pub(crate) struct AzureConfigError {
    code: AzureConfigErrorCode,
}

impl AzureConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> AzureConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for AzureConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AzureConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for AzureConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            AzureConfigErrorCode::InvalidConfiguration => {
                "the Azure toolkit configuration is invalid"
            }
            AzureConfigErrorCode::ResourceExhausted => {
                "the Azure toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for AzureConfigError {}

/// Claim-scoped Microsoft Entra client credential and ARM subscription authority.
///
/// Main marks only `client_secret` as secret and redeems it for the accepted
/// execution. The secret is deliberately non-cloneable and non-debuggable.
pub(crate) struct AzureToolkitConfig {
    subscription_id: Box<str>,
    tenant_id: Box<str>,
    client_id: Box<str>,
    client_secret: Zeroizing<String>,
    selected_tools: Vec<Box<str>>,
}

impl AzureToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, AzureConfigError> {
        let subscription_id = required_uuid(settings, "subscription_id")?;
        let client_id = required_uuid(settings, "client_id")?;
        let tenant_id = required_tenant(settings)?;
        let client_secret = required_secret(settings)?;
        Ok(Self {
            subscription_id: subscription_id.into(),
            tenant_id: tenant_id.into(),
            client_id: client_id.into(),
            client_secret: Zeroizing::new(client_secret.to_owned()),
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) fn subscription_id(&self) -> &str {
        &self.subscription_id
    }

    pub(super) fn tenant_id(&self) -> &str {
        &self.tenant_id
    }

    pub(super) fn client_id(&self) -> &str {
        &self.client_id
    }

    pub(super) fn client_secret(&self) -> &str {
        &self.client_secret
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

fn required_uuid<'a>(
    settings: &'a Map<String, Value>,
    name: &str,
) -> Result<&'a str, AzureConfigError> {
    let value = settings
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?
        .trim();
    if !is_uuid(value) {
        return Err(invalid_configuration());
    }
    Ok(value)
}

fn is_uuid(value: &str) -> bool {
    value.len() == 36
        && value.bytes().enumerate().all(|(index, byte)| {
            if matches!(index, 8 | 13 | 18 | 23) {
                byte == b'-'
            } else {
                byte.is_ascii_hexdigit()
            }
        })
}

fn required_tenant(settings: &Map<String, Value>) -> Result<&str, AzureConfigError> {
    let value = settings
        .get("tenant_id")
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?
        .trim();
    if value.len() > MAX_TENANT_ID_BYTES {
        return Err(resource_exhausted());
    }
    if !is_uuid(value) && !is_tenant_domain(value) {
        return Err(invalid_configuration());
    }
    Ok(value)
}

fn is_tenant_domain(value: &str) -> bool {
    !value.is_empty()
        && value.split('.').all(|label| {
            !label.is_empty()
                && label.len() <= 63
                && label
                    .as_bytes()
                    .first()
                    .is_some_and(u8::is_ascii_alphanumeric)
                && label
                    .as_bytes()
                    .last()
                    .is_some_and(u8::is_ascii_alphanumeric)
                && label
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
        })
}

fn required_secret(settings: &Map<String, Value>) -> Result<&str, AzureConfigError> {
    let value = settings
        .get("client_secret")
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?;
    if value.len() > MAX_CLIENT_SECRET_BYTES {
        return Err(resource_exhausted());
    }
    if value.is_empty() || value.bytes().any(|byte| byte.is_ascii_control()) {
        return Err(invalid_configuration());
    }
    Ok(value)
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, AzureConfigError> {
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

const fn invalid_configuration() -> AzureConfigError {
    AzureConfigError {
        code: AzureConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> AzureConfigError {
    AzureConfigError {
        code: AzureConfigErrorCode::ResourceExhausted,
    }
}
