use std::collections::HashSet;
use std::fmt;

use percent_encoding::percent_decode_str;
use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_BASE_URL_BYTES: usize = 2_048;
const MAX_REALM_BYTES: usize = 255;
const MAX_CLIENT_ID_BYTES: usize = 512;
const MAX_CLIENT_SECRET_BYTES: usize = 16 * 1_024;
const MAX_SELECTED_TOOLS: usize = 16;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum KeycloakConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure without origin, realm, client, or secret data.
pub(crate) struct KeycloakConfigError {
    code: KeycloakConfigErrorCode,
}

impl KeycloakConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> KeycloakConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for KeycloakConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("KeycloakConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for KeycloakConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            KeycloakConfigErrorCode::InvalidConfiguration => {
                "the Keycloak toolkit configuration is invalid"
            }
            KeycloakConfigErrorCode::ResourceExhausted => {
                "the Keycloak toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for KeycloakConfigError {}

/// Claim-scoped Keycloak service-account authority.
///
/// Main marks only `client_secret` as secret and redeems it for the accepted
/// execution. The value is deliberately non-cloneable and non-debuggable.
pub(crate) struct KeycloakToolkitConfig {
    base_url: Box<str>,
    realm: Box<str>,
    client_id: Box<str>,
    client_secret: Zeroizing<String>,
    selected_tools: Vec<Box<str>>,
}

impl KeycloakToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, KeycloakConfigError> {
        let base_url = parse_base_url(required_text(settings, "base_url", MAX_BASE_URL_BYTES)?)?;
        let realm = required_text(settings, "realm", MAX_REALM_BYTES)?;
        let client_id = required_text(settings, "client_id", MAX_CLIENT_ID_BYTES)?;
        let client_secret = required_secret(settings, "client_secret", MAX_CLIENT_SECRET_BYTES)?;
        Ok(Self {
            base_url: base_url.into(),
            realm: realm.into(),
            client_id: client_id.into(),
            client_secret: Zeroizing::new(client_secret.to_owned()),
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) fn base_url(&self) -> &str {
        &self.base_url
    }

    pub(super) fn realm(&self) -> &str {
        &self.realm
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

fn parse_base_url(value: &str) -> Result<String, KeycloakConfigError> {
    validate_raw_url_path(value)?;
    let mut url = Url::parse(value).map_err(|_| invalid_configuration())?;
    if url.scheme() != "https"
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err(invalid_configuration());
    }
    validate_path(url.path())?;
    let path = url.path().trim_end_matches('/').to_owned();
    url.set_path(&path);
    let normalized = url.to_string().trim_end_matches('/').to_owned();
    if normalized.len() > MAX_BASE_URL_BYTES {
        return Err(resource_exhausted());
    }
    Ok(normalized)
}

fn validate_raw_url_path(value: &str) -> Result<(), KeycloakConfigError> {
    let after_scheme = value
        .split_once("://")
        .map(|(_, remainder)| remainder)
        .ok_or_else(invalid_configuration)?;
    let path = after_scheme
        .find('/')
        .map_or("/", |index| &after_scheme[index..]);
    validate_path(path)
}

fn validate_path(path: &str) -> Result<(), KeycloakConfigError> {
    if path
        .bytes()
        .any(|byte| byte.is_ascii_control() || byte == b'\\')
    {
        return Err(invalid_configuration());
    }
    for segment in path.split('/') {
        validate_percent_encoding(segment)?;
        let decoded = percent_decode_str(segment)
            .decode_utf8()
            .map_err(|_| invalid_configuration())?;
        if decoded
            .bytes()
            .any(|byte| byte.is_ascii_control() || matches!(byte, b'/' | b'\\'))
            || matches!(decoded.as_ref(), "." | "..")
            || contains_percent_escape(&decoded)
        {
            return Err(invalid_configuration());
        }
    }
    Ok(())
}

fn validate_percent_encoding(value: &str) -> Result<(), KeycloakConfigError> {
    let bytes = value.as_bytes();
    let mut index = 0usize;
    while index < bytes.len() {
        if bytes[index] == b'%'
            && (index + 2 >= bytes.len()
                || !bytes[index + 1].is_ascii_hexdigit()
                || !bytes[index + 2].is_ascii_hexdigit())
        {
            return Err(invalid_configuration());
        }
        index += if bytes[index] == b'%' { 3 } else { 1 };
    }
    Ok(())
}

fn contains_percent_escape(value: &str) -> bool {
    value
        .as_bytes()
        .windows(3)
        .any(|window| window[0] == b'%' && window[1..].iter().all(u8::is_ascii_hexdigit))
}

fn required_text<'a>(
    settings: &'a Map<String, Value>,
    name: &str,
    max_bytes: usize,
) -> Result<&'a str, KeycloakConfigError> {
    let value = settings
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?;
    if value.len() > max_bytes {
        return Err(resource_exhausted());
    }
    let value = value.trim();
    if value.is_empty() || value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n')) {
        return Err(invalid_configuration());
    }
    Ok(value)
}

fn required_secret<'a>(
    settings: &'a Map<String, Value>,
    name: &str,
    max_bytes: usize,
) -> Result<&'a str, KeycloakConfigError> {
    let value = settings
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?;
    if value.len() > max_bytes {
        return Err(resource_exhausted());
    }
    if value.is_empty() || value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n')) {
        return Err(invalid_configuration());
    }
    Ok(value)
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, KeycloakConfigError> {
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

const fn invalid_configuration() -> KeycloakConfigError {
    KeycloakConfigError {
        code: KeycloakConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> KeycloakConfigError {
    KeycloakConfigError {
        code: KeycloakConfigErrorCode::ResourceExhausted,
    }
}
