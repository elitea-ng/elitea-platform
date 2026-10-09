use std::collections::HashSet;
use std::fmt;

use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use ring::signature::RsaKeyPair;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const TOKEN_URI: &str = "https://oauth2.googleapis.com/token";
const MAX_SERVICE_ACCOUNT_JSON_BYTES: usize = 128 * 1_024;
const MAX_PRIVATE_KEY_PEM_BYTES: usize = 128 * 1_024;
const MAX_PRIVATE_KEY_DER_BYTES: usize = 96 * 1_024;
const MAX_CLIENT_EMAIL_BYTES: usize = 320;
const MAX_PRIVATE_KEY_ID_BYTES: usize = 256;
const MAX_SELECTED_TOOLS: usize = 16;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum GcpConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure without service-account identity or key data.
pub(crate) struct GcpConfigError {
    code: GcpConfigErrorCode,
}

impl GcpConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> GcpConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for GcpConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("GcpConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for GcpConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            GcpConfigErrorCode::InvalidConfiguration => "the GCP toolkit configuration is invalid",
            GcpConfigErrorCode::ResourceExhausted => {
                "the GCP toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for GcpConfigError {}

/// Claim-scoped Google service-account signing identity.
///
/// Main seals the complete `api_key` JSON string. Rust extracts only the fields
/// required for the official JWT bearer grant and retains the DER key in a
/// non-cloneable, non-debuggable zeroizing buffer.
pub(crate) struct GcpToolkitConfig {
    client_email: Box<str>,
    private_key_id: Option<Box<str>>,
    private_key_der: Zeroizing<Vec<u8>>,
    selected_tools: Vec<Box<str>>,
}

impl GcpToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, GcpConfigError> {
        let raw = settings
            .get("api_key")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)?;
        if raw.is_empty() || raw.len() > MAX_SERVICE_ACCOUNT_JSON_BYTES {
            return Err(if raw.len() > MAX_SERVICE_ACCOUNT_JSON_BYTES {
                resource_exhausted()
            } else {
                invalid_configuration()
            });
        }
        let mut service_account: Map<String, Value> =
            serde_json::from_str(raw).map_err(|_| invalid_configuration())?;
        require_exact_string(&service_account, "type", "service_account")?;
        require_exact_string(&service_account, "token_uri", TOKEN_URI)?;
        let client_email = take_string(&mut service_account, "client_email")?;
        validate_client_email(&client_email)?;
        let private_key_id = take_optional_string(&mut service_account, "private_key_id")?;
        if private_key_id
            .as_deref()
            .is_some_and(|value| !valid_private_key_id(value))
        {
            return Err(invalid_configuration());
        }
        let private_key = Zeroizing::new(take_string(&mut service_account, "private_key")?);
        let private_key_der = decode_private_key(&private_key)?;
        Ok(Self {
            client_email: client_email.into(),
            private_key_id: private_key_id.map(Into::into),
            private_key_der,
            selected_tools: selected_tools(settings.get("selected_tools"))?,
        })
    }

    pub(super) fn client_email(&self) -> &str {
        &self.client_email
    }

    pub(super) fn private_key_id(&self) -> Option<&str> {
        self.private_key_id.as_deref()
    }

    pub(super) fn private_key_der(&self) -> &[u8] {
        &self.private_key_der
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

fn require_exact_string(
    values: &Map<String, Value>,
    name: &str,
    expected: &str,
) -> Result<(), GcpConfigError> {
    if values.get(name).and_then(Value::as_str) != Some(expected) {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn take_string(values: &mut Map<String, Value>, name: &str) -> Result<String, GcpConfigError> {
    match values.remove(name) {
        Some(Value::String(value)) if !value.is_empty() => Ok(value),
        _ => Err(invalid_configuration()),
    }
}

fn take_optional_string(
    values: &mut Map<String, Value>,
    name: &str,
) -> Result<Option<String>, GcpConfigError> {
    match values.remove(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) if !value.is_empty() => Ok(Some(value)),
        _ => Err(invalid_configuration()),
    }
}

fn validate_client_email(value: &str) -> Result<(), GcpConfigError> {
    if value.len() > MAX_CLIENT_EMAIL_BYTES {
        return Err(resource_exhausted());
    }
    let Some((local, domain)) = value.split_once('@') else {
        return Err(invalid_configuration());
    };
    if local.is_empty()
        || (domain != "gserviceaccount.com" && !domain.ends_with(".gserviceaccount.com"))
        || value.bytes().any(|byte| byte.is_ascii_control())
    {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn valid_private_key_id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_PRIVATE_KEY_ID_BYTES
        && value.bytes().all(|byte| byte.is_ascii_alphanumeric())
}

fn decode_private_key(value: &str) -> Result<Zeroizing<Vec<u8>>, GcpConfigError> {
    const BEGIN: &str = "-----BEGIN PRIVATE KEY-----";
    const END: &str = "-----END PRIVATE KEY-----";
    if value.len() > MAX_PRIVATE_KEY_PEM_BYTES {
        return Err(resource_exhausted());
    }
    let value = value.trim();
    let body = value
        .strip_prefix(BEGIN)
        .and_then(|value| value.strip_suffix(END))
        .ok_or_else(invalid_configuration)?;
    let compact = Zeroizing::new(
        body.chars()
            .filter(|character| !character.is_ascii_whitespace())
            .collect::<String>(),
    );
    if compact.is_empty() || compact.len() > MAX_PRIVATE_KEY_PEM_BYTES {
        return Err(invalid_configuration());
    }
    let der = Zeroizing::new(
        STANDARD
            .decode(compact.as_bytes())
            .map_err(|_| invalid_configuration())?,
    );
    if der.is_empty() || der.len() > MAX_PRIVATE_KEY_DER_BYTES {
        return Err(if der.len() > MAX_PRIVATE_KEY_DER_BYTES {
            resource_exhausted()
        } else {
            invalid_configuration()
        });
    }
    let key = RsaKeyPair::from_pkcs8(&der).map_err(|_| invalid_configuration())?;
    if key.public().modulus_len() < 256 {
        return Err(invalid_configuration());
    }
    Ok(der)
}

fn selected_tools(value: Option<&Value>) -> Result<Vec<Box<str>>, GcpConfigError> {
    let Some(value) = value else {
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

const fn invalid_configuration() -> GcpConfigError {
    GcpConfigError {
        code: GcpConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> GcpConfigError {
    GcpConfigError {
        code: GcpConfigErrorCode::ResourceExhausted,
    }
}
