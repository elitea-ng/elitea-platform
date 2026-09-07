use std::fmt;

use reqwest::Url;
use reqwest::header::{AUTHORIZATION, HeaderMap, HeaderName, HeaderValue};
use serde_json::{Map, Value};
use zeroize::Zeroizing;

use crate::toolkits::DelegatedAuthorizationRequirement;

use super::spec::{OpenApiOperation, parse_operations};

const MAX_URL_BYTES: usize = 8 * 1_024;
const MAX_SECRET_BYTES: usize = 64 * 1_024;
const MAX_IDENTITY_BYTES: usize = 1_024;
const MAX_SCOPE_BYTES: usize = 16 * 1_024;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_ADDITIONAL_HEADERS: usize = 128;
const MAX_HEADER_VALUE_BYTES: usize = 16 * 1_024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum OpenApiConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedCapability,
}

pub(crate) struct OpenApiConfigError {
    code: OpenApiConfigErrorCode,
}

impl OpenApiConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> OpenApiConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for OpenApiConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("OpenApiConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for OpenApiConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            OpenApiConfigErrorCode::InvalidConfiguration => {
                "the OpenAPI toolkit configuration is invalid"
            }
            OpenApiConfigErrorCode::ResourceExhausted => {
                "the OpenAPI toolkit configuration exceeds its approved limit"
            }
            OpenApiConfigErrorCode::UnsupportedCapability => {
                "the OpenAPI toolkit requires an unavailable runtime authority"
            }
        })
    }
}

impl std::error::Error for OpenApiConfigError {}

pub(crate) struct OpenApiToolkitConfig {
    base_url: Url,
    operations: Vec<OpenApiOperation>,
    auth: OpenApiAuth,
    additional_headers: HeaderMap,
}

impl OpenApiToolkitConfig {
    pub(crate) fn parse(
        toolkit_name: &str,
        settings: &Map<String, Value>,
        delegated_tokens: &Map<String, Value>,
    ) -> Result<Self, OpenApiConfigError> {
        validate_text(toolkit_name, MAX_IDENTITY_BYTES)?;
        let selected_tools = selected_tools(settings)?;
        let base_override =
            optional_text(settings, &["base_url", "base_url_override"], MAX_URL_BYTES)?;
        let spec = settings
            .get("spec")
            .or_else(|| settings.get("schema_settings"))
            .or_else(|| settings.get("openapi_spec"))
            .ok_or_else(invalid_configuration)?;
        let parsed = parse_operations(spec, base_override, &selected_tools)
            .map_err(|error| Self::map_spec_error(error.code()))?;
        let auth_settings = merged_auth_settings(settings)?;
        let additional_headers = parse_additional_headers(&auth_settings)?;
        let auth = parse_auth(
            toolkit_name,
            &parsed.base_url,
            &auth_settings,
            delegated_tokens,
        )?;
        Ok(Self {
            base_url: parsed.base_url,
            operations: parsed.operations,
            auth,
            additional_headers,
        })
    }

    fn map_spec_error(code: super::spec::OpenApiSpecErrorCode) -> OpenApiConfigError {
        match code {
            super::spec::OpenApiSpecErrorCode::InvalidSpecification => invalid_configuration(),
            super::spec::OpenApiSpecErrorCode::ResourceExhausted => resource_exhausted(),
            super::spec::OpenApiSpecErrorCode::UnsupportedSource => unsupported_capability(),
        }
    }

    #[must_use]
    pub(crate) fn base_url(&self) -> &Url {
        &self.base_url
    }

    #[must_use]
    pub(crate) fn operations(&self) -> &[OpenApiOperation] {
        &self.operations
    }

    #[must_use]
    pub(crate) fn auth(&self) -> &OpenApiAuth {
        &self.auth
    }

    pub(crate) fn into_client_parts(self) -> OpenApiClientConfig {
        OpenApiClientConfig {
            base_url: self.base_url,
            auth: self.auth,
            additional_headers: self.additional_headers,
        }
    }

    pub(crate) fn with_toolkit_id(mut self, id: Option<u64>) -> Self {
        if let OpenApiAuth::Delegated { requirement, .. } = &mut self.auth {
            *requirement = requirement.clone().with_toolkit_id(id);
        }
        self
    }
}

pub(crate) enum OpenApiAuth {
    Anonymous,
    Header {
        name: HeaderName,
        value: Zeroizing<String>,
    },
    ClientCredentials {
        client_id: Box<str>,
        client_secret: Zeroizing<String>,
        token_url: Url,
        scope: Option<Box<str>>,
        basic: bool,
    },
    Delegated {
        access_token: Option<Zeroizing<String>>,
        requirement: DelegatedAuthorizationRequirement,
    },
}

impl OpenApiAuth {
    #[must_use]
    pub(crate) fn delegated_requirement(&self) -> Option<&DelegatedAuthorizationRequirement> {
        match self {
            Self::Delegated {
                access_token: None,
                requirement,
            } => Some(requirement),
            Self::Anonymous
            | Self::Header { .. }
            | Self::ClientCredentials { .. }
            | Self::Delegated {
                access_token: Some(_),
                ..
            } => None,
        }
    }
}

pub(crate) struct OpenApiClientConfig {
    pub(super) base_url: Url,
    pub(super) auth: OpenApiAuth,
    pub(super) additional_headers: HeaderMap,
}

fn parse_additional_headers(
    settings: &Map<String, Value>,
) -> Result<HeaderMap, OpenApiConfigError> {
    let Some(raw) = settings.get("headers") else {
        return Ok(HeaderMap::new());
    };
    if raw.is_null() {
        return Ok(HeaderMap::new());
    }
    let raw = raw.as_object().ok_or_else(invalid_configuration)?;
    if raw.len() > MAX_ADDITIONAL_HEADERS {
        return Err(resource_exhausted());
    }
    let mut headers = HeaderMap::with_capacity(raw.len());
    for (name, value) in raw {
        let name = HeaderName::from_bytes(name.as_bytes()).map_err(|_| invalid_configuration())?;
        if restricted_configured_header(&name) {
            return Err(invalid_configuration());
        }
        let value = value
            .as_str()
            .filter(|value| !value.is_empty() && value.len() <= MAX_HEADER_VALUE_BYTES)
            .ok_or_else(invalid_configuration)?;
        let mut value = HeaderValue::from_str(value).map_err(|_| invalid_configuration())?;
        value.set_sensitive(true);
        headers.insert(name, value);
    }
    Ok(headers)
}

fn merged_auth_settings(
    settings: &Map<String, Value>,
) -> Result<Map<String, Value>, OpenApiConfigError> {
    let mut merged = settings.clone();
    if let Some(configuration) = settings.get("openapi_configuration") {
        let configuration = configuration
            .as_object()
            .ok_or_else(invalid_configuration)?;
        for (key, value) in configuration {
            merged.insert(key.clone(), value.clone());
        }
    }
    Ok(merged)
}

fn parse_auth(
    toolkit_name: &str,
    base_url: &Url,
    settings: &Map<String, Value>,
    delegated_tokens: &Map<String, Value>,
) -> Result<OpenApiAuth, OpenApiConfigError> {
    if let Some(discovery) = optional_text(settings, &["oauth_discovery_endpoint"], MAX_URL_BYTES)?
    {
        required_text(settings, "client_id", MAX_IDENTITY_BYTES)?;
        required_text(settings, "client_secret", MAX_SECRET_BYTES)?;
        let discovery = parse_https_url(discovery, true)?;
        let mut resource_metadata_url = discovery.clone();
        append_path(
            &mut resource_metadata_url,
            "/.well-known/openid-configuration",
        )?;
        let mut authorization_url = discovery.clone();
        append_path(&mut authorization_url, "/oauth2/v2.0/authorize")?;
        let server_url = canonical_url(base_url);
        let requirement = DelegatedAuthorizationRequirement::new(
            toolkit_name.to_owned(),
            "openapi".to_owned(),
            server_url.clone(),
            Some(resource_metadata_url.to_string()),
            Some(format!(
                "Bearer error=\"unauthorized_client\", resource_metadata=\"{resource_metadata_url}\", authorization_uri=\"{authorization_url}\""
            )),
        )
        .and_then(|requirement| requirement.with_configured_oauth(discovery.as_str(), settings))
        .ok_or_else(invalid_configuration)?;
        let access_token = resolve_access_token(delegated_tokens, &requirement)?;
        return Ok(OpenApiAuth::Delegated {
            access_token,
            requirement,
        });
    }

    let client_id = optional_text(settings, &["client_id"], MAX_IDENTITY_BYTES)?;
    let client_secret = optional_text(settings, &["client_secret"], MAX_SECRET_BYTES)?;
    let token_url = optional_text(settings, &["token_url"], MAX_URL_BYTES)?;
    if client_id.is_some() || client_secret.is_some() || token_url.is_some() {
        let client_id = client_id.ok_or_else(invalid_configuration)?;
        let client_secret = client_secret.ok_or_else(invalid_configuration)?;
        let token_url = parse_https_url(token_url.ok_or_else(invalid_configuration)?, true)?;
        let scope = optional_text(settings, &["scope"], MAX_SCOPE_BYTES)?.map(Into::into);
        let basic = match optional_text(settings, &["method"], 32)? {
            None | Some("default") => false,
            Some("Basic") => true,
            Some(_) => return Err(invalid_configuration()),
        };
        return Ok(OpenApiAuth::ClientCredentials {
            client_id: client_id.into(),
            client_secret: Zeroizing::new(client_secret.to_owned()),
            token_url,
            scope,
            basic,
        });
    }

    let Some(api_key) = optional_text(settings, &["api_key"], MAX_SECRET_BYTES)? else {
        return Ok(OpenApiAuth::Anonymous);
    };
    let auth_type = optional_text(settings, &["auth_type"], 32)?.unwrap_or("Bearer");
    let (name, value) = match auth_type {
        "Bearer" => (AUTHORIZATION, format!("Bearer {api_key}")),
        "Basic" => (AUTHORIZATION, format!("Basic {api_key}")),
        "Custom" | "custom" => {
            let header = required_text(settings, "custom_header_name", 256)?;
            let name =
                HeaderName::from_bytes(header.as_bytes()).map_err(|_| invalid_configuration())?;
            if restricted_header(&name) {
                return Err(invalid_configuration());
            }
            (name, api_key.to_owned())
        }
        _ => return Err(invalid_configuration()),
    };
    HeaderValue::from_str(&value).map_err(|_| invalid_configuration())?;
    Ok(OpenApiAuth::Header {
        name,
        value: Zeroizing::new(value),
    })
}

fn resolve_access_token(
    tokens: &Map<String, Value>,
    requirement: &DelegatedAuthorizationRequirement,
) -> Result<Option<Zeroizing<String>>, OpenApiConfigError> {
    // Prefer the exact configuration identity over the legacy resource URL.
    let value = tokens
        .iter()
        .find(|(key, _)| {
            key.as_str() != requirement.server_url() && requirement.matches_token_key(key)
        })
        .map(|(_, value)| value)
        .or_else(|| tokens.get(requirement.server_url()));
    let Some(value) = value else {
        return Ok(None);
    };
    let token = match value {
        Value::String(token) => token.as_str(),
        Value::Object(object) => object
            .get("access_token")
            .or_else(|| object.get("token"))
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)?,
        _ => return Err(invalid_configuration()),
    };
    validate_text(token, MAX_SECRET_BYTES)?;
    HeaderValue::from_str(&format!("Bearer {token}")).map_err(|_| invalid_configuration())?;
    Ok(Some(Zeroizing::new(token.to_owned())))
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<String>, OpenApiConfigError> {
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
        let name = match value {
            Value::String(name) => name.as_str(),
            Value::Object(object) => object
                .get("name")
                .and_then(Value::as_str)
                .ok_or_else(invalid_configuration)?,
            _ => return Err(invalid_configuration()),
        };
        validate_text(name, 64)?;
        if !selected.iter().any(|existing| existing == name) {
            selected.push(name.to_owned());
        }
    }
    Ok(selected)
}

fn optional_text<'a>(
    settings: &'a Map<String, Value>,
    names: &[&str],
    limit: usize,
) -> Result<Option<&'a str>, OpenApiConfigError> {
    for name in names {
        match settings.get(*name) {
            None | Some(Value::Null) => {}
            Some(Value::String(value)) if value.is_empty() => {}
            Some(Value::String(value)) => {
                validate_text(value, limit)?;
                return Ok(Some(value));
            }
            Some(_) => return Err(invalid_configuration()),
        }
    }
    Ok(None)
}

fn required_text<'a>(
    settings: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, OpenApiConfigError> {
    optional_text(settings, &[name], limit)?.ok_or_else(invalid_configuration)
}

fn validate_text(value: &str, limit: usize) -> Result<(), OpenApiConfigError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || value.chars().any(char::is_control) {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn parse_https_url(value: &str, allow_path: bool) -> Result<Url, OpenApiConfigError> {
    if value.len() > MAX_URL_BYTES || value.contains(['\\', '%']) {
        return Err(invalid_configuration());
    }
    let url = Url::parse(value).map_err(|_| invalid_configuration())?;
    if url.scheme() != "https"
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || (!allow_path && !matches!(url.path(), "" | "/"))
    {
        return Err(invalid_configuration());
    }
    Ok(url)
}

fn append_path(url: &mut Url, suffix: &str) -> Result<(), OpenApiConfigError> {
    let current = url.path().trim_end_matches('/');
    let path = format!("{current}{suffix}");
    if path.len() > MAX_URL_BYTES {
        return Err(resource_exhausted());
    }
    url.set_path(&path);
    Ok(())
}

fn canonical_url(url: &Url) -> String {
    let mut canonical = url.clone();
    if canonical.path() == "/" {
        canonical.set_path("");
    }
    canonical.to_string().trim_end_matches('/').to_owned()
}

fn restricted_header(name: &HeaderName) -> bool {
    matches!(
        name.as_str(),
        "authorization"
            | "proxy-authorization"
            | "host"
            | "content-length"
            | "transfer-encoding"
            | "connection"
            | "cookie"
            | "set-cookie"
    )
}

fn restricted_configured_header(name: &HeaderName) -> bool {
    matches!(
        name.as_str(),
        "proxy-authorization"
            | "host"
            | "content-length"
            | "transfer-encoding"
            | "connection"
            | "set-cookie"
    )
}

const fn invalid_configuration() -> OpenApiConfigError {
    OpenApiConfigError {
        code: OpenApiConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> OpenApiConfigError {
    OpenApiConfigError {
        code: OpenApiConfigErrorCode::ResourceExhausted,
    }
}

const fn unsupported_capability() -> OpenApiConfigError {
    OpenApiConfigError {
        code: OpenApiConfigErrorCode::UnsupportedCapability,
    }
}
