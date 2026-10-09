use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_URL_BYTES: usize = 2_048;
const MAX_IDENTIFIER_BYTES: usize = 1_024;
const MAX_API_KEY_BYTES: usize = 16 * 1_024;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;
const MAX_ENVIRONMENT_BYTES: usize = 240 * 1_024;
const MAX_ENVIRONMENT_NODES: usize = 16_384;
const MAX_ENVIRONMENT_DEPTH: usize = 32;
const MAX_ENVIRONMENT_STRING_BYTES: usize = 64 * 1_024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PostmanConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure that retains no origin or credential data.
pub(crate) struct PostmanConfigError {
    code: PostmanConfigErrorCode,
}

impl PostmanConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> PostmanConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for PostmanConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("PostmanConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for PostmanConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            PostmanConfigErrorCode::InvalidConfiguration => {
                "the Postman toolkit configuration is invalid"
            }
            PostmanConfigErrorCode::ResourceExhausted => {
                "the Postman toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for PostmanConfigError {}

/// Invocation-scoped Postman authority and collection defaults.
pub(crate) struct PostmanToolkitConfig {
    base_url: Url,
    workspace_id: Box<str>,
    api_key: Zeroizing<String>,
    collection_id: Box<str>,
    dynamic_profile: DynamicExecutionProfile,
    selected_tools: Vec<Box<str>>,
}

impl PostmanToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, PostmanConfigError> {
        let configuration = settings
            .get("postman_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let base_url = parse_base_url(required_text(configuration, "base_url", MAX_URL_BYTES)?)?;
        let workspace_id = required_text(configuration, "workspace_id", MAX_IDENTIFIER_BYTES)?;
        let api_key = required_text(configuration, "api_key", MAX_API_KEY_BYTES)?;
        if !api_key.bytes().all(|byte| (0x21..=0x7e).contains(&byte)) {
            return Err(invalid_configuration());
        }
        let collection_id = required_text(settings, "collection_id", MAX_IDENTIFIER_BYTES)?;
        let empty_environment = Value::Object(Map::new());
        let environment_config = settings
            .get("environment_config")
            .unwrap_or(&empty_environment);
        let dynamic_profile = validate_environment(environment_config)?;
        Ok(Self {
            base_url,
            workspace_id: workspace_id.into(),
            api_key: Zeroizing::new(api_key.to_owned()),
            collection_id: collection_id.into(),
            dynamic_profile,
            selected_tools: selected_tools(settings)?,
        })
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }

    pub(super) fn into_client_parts(self) -> PostmanClientConfig {
        PostmanClientConfig {
            base_url: self.base_url,
            workspace_id: self.workspace_id,
            api_key: self.api_key,
            collection_id: self.collection_id,
            dynamic_profile: self.dynamic_profile,
        }
    }
}

/// Secret-bearing request-execution settings stay serialized and zeroized.
/// They are parsed only after a separate downstream egress grant is present.
pub(super) struct DynamicExecutionProfile {
    pub(super) canonical_json: Zeroizing<String>,
}

pub(super) struct PostmanClientConfig {
    pub(super) base_url: Url,
    pub(super) workspace_id: Box<str>,
    pub(super) api_key: Zeroizing<String>,
    pub(super) collection_id: Box<str>,
    pub(super) dynamic_profile: DynamicExecutionProfile,
}

fn parse_base_url(value: &str) -> Result<Url, PostmanConfigError> {
    if value.contains(['%', '\\']) {
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
    Ok(url)
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, PostmanConfigError> {
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
    let mut selected = Vec::with_capacity(values.len().min(31));
    for value in values {
        let name = value.as_str().ok_or_else(invalid_configuration)?;
        validate_text(name, MAX_TOOL_NAME_BYTES)?;
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

fn validate_environment(value: &Value) -> Result<DynamicExecutionProfile, PostmanConfigError> {
    if !value.is_object() {
        return Err(invalid_configuration());
    }
    let canonical_json = serde_json::to_string(value).map_err(|_| invalid_configuration())?;
    if canonical_json.len() > MAX_ENVIRONMENT_BYTES {
        return Err(resource_exhausted());
    }
    let mut nodes = 0usize;
    let mut stack = vec![(value, 1usize)];
    while let Some((node, depth)) = stack.pop() {
        nodes = nodes.checked_add(1).ok_or_else(resource_exhausted)?;
        if nodes > MAX_ENVIRONMENT_NODES || depth > MAX_ENVIRONMENT_DEPTH {
            return Err(resource_exhausted());
        }
        match node {
            Value::String(text) => {
                if text.len() > MAX_ENVIRONMENT_STRING_BYTES {
                    return Err(resource_exhausted());
                }
            }
            Value::Array(values) => {
                stack.extend(values.iter().map(|value| (value, depth + 1)));
            }
            Value::Object(values) => {
                for (key, value) in values {
                    if key.is_empty()
                        || key.len() > MAX_IDENTIFIER_BYTES
                        || key.chars().any(char::is_control)
                    {
                        return Err(invalid_configuration());
                    }
                    stack.push((value, depth + 1));
                }
            }
            Value::Null | Value::Bool(_) | Value::Number(_) => {}
        }
    }
    Ok(DynamicExecutionProfile {
        canonical_json: Zeroizing::new(canonical_json),
    })
}

fn required_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, PostmanConfigError> {
    let value = object
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?;
    validate_text(value, limit)?;
    Ok(value)
}

fn validate_text(value: &str, limit: usize) -> Result<(), PostmanConfigError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || value.chars().any(char::is_control) {
        return Err(invalid_configuration());
    }
    Ok(())
}

const fn invalid_configuration() -> PostmanConfigError {
    PostmanConfigError {
        code: PostmanConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> PostmanConfigError {
    PostmanConfigError {
        code: PostmanConfigErrorCode::ResourceExhausted,
    }
}
