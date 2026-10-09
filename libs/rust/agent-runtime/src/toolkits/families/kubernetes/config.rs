use std::collections::HashSet;
use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_CLUSTER_URL_BYTES: usize = 2_048;
const MAX_TOKEN_BYTES: usize = 32 * 1_024;
const MAX_SELECTED_TOOLS: usize = 16;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum KubernetesConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure without cluster authority or credential data.
pub(crate) struct KubernetesConfigError {
    code: KubernetesConfigErrorCode,
}

impl KubernetesConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> KubernetesConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for KubernetesConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("KubernetesConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for KubernetesConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            KubernetesConfigErrorCode::InvalidConfiguration => {
                "the Kubernetes toolkit configuration is invalid"
            }
            KubernetesConfigErrorCode::ResourceExhausted => {
                "the Kubernetes toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for KubernetesConfigError {}

/// Claim-scoped Kubernetes API origin and Bearer credential.
///
/// Main seals only `token`. Rust requires the exact HTTPS origin and token in
/// the accepted execution instead of reading ambient kubeconfig state.
pub(crate) struct KubernetesToolkitConfig {
    cluster_url: Box<str>,
    token: Zeroizing<String>,
    selected_tools: Vec<Box<str>>,
}

impl KubernetesToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, KubernetesConfigError> {
        let cluster_url = parse_cluster_url(settings.get("url"))?;
        let token = required_token(settings.get("token"))?;
        Ok(Self {
            cluster_url: cluster_url.into(),
            token: Zeroizing::new(token.to_owned()),
            selected_tools: selected_tools(settings.get("selected_tools"))?,
        })
    }

    pub(super) fn cluster_url(&self) -> &str {
        &self.cluster_url
    }

    pub(super) fn token(&self) -> &str {
        &self.token
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

fn parse_cluster_url(value: Option<&Value>) -> Result<String, KubernetesConfigError> {
    let value = value
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?
        .trim();
    if value.is_empty() || value.len() > MAX_CLUSTER_URL_BYTES {
        return Err(if value.len() > MAX_CLUSTER_URL_BYTES {
            resource_exhausted()
        } else {
            invalid_configuration()
        });
    }
    if value.bytes().any(|byte| byte.is_ascii_control()) {
        return Err(invalid_configuration());
    }
    let mut url = Url::parse(value).map_err(|_| invalid_configuration())?;
    if url.scheme() != "https"
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || !matches!(url.path(), "" | "/")
    {
        return Err(invalid_configuration());
    }
    url.set_path("");
    let normalized = url.to_string().trim_end_matches('/').to_owned();
    if normalized.len() > MAX_CLUSTER_URL_BYTES {
        return Err(resource_exhausted());
    }
    Ok(normalized)
}

fn required_token(value: Option<&Value>) -> Result<&str, KubernetesConfigError> {
    let value = value
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?;
    if value.len() > MAX_TOKEN_BYTES {
        return Err(resource_exhausted());
    }
    if value.is_empty() || value.bytes().any(|byte| byte.is_ascii_control()) {
        return Err(invalid_configuration());
    }
    Ok(value)
}

fn selected_tools(value: Option<&Value>) -> Result<Vec<Box<str>>, KubernetesConfigError> {
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

const fn invalid_configuration() -> KubernetesConfigError {
    KubernetesConfigError {
        code: KubernetesConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> KubernetesConfigError {
    KubernetesConfigError {
        code: KubernetesConfigErrorCode::ResourceExhausted,
    }
}
