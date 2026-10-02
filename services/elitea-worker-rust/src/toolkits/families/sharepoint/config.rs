use std::fmt;

use reqwest::Url;
use reqwest::header::HeaderValue;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

use crate::toolkits::DelegatedAuthorizationRequirement;

const MAX_URL_BYTES: usize = 8 * 1_024;
const MAX_IDENTITY_BYTES: usize = 1_024;
const MAX_SECRET_BYTES: usize = 64 * 1_024;
const MAX_SCOPE_BYTES: usize = 2 * 1_024;
const MAX_SCOPES: usize = 128;
const MAX_SELECTED_TOOLS: usize = 1_024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SharePointConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedCapability,
}

pub(crate) struct SharePointConfigError {
    code: SharePointConfigErrorCode,
}

impl SharePointConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> SharePointConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for SharePointConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SharePointConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for SharePointConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            SharePointConfigErrorCode::InvalidConfiguration => {
                "the SharePoint toolkit configuration is invalid"
            }
            SharePointConfigErrorCode::ResourceExhausted => {
                "the SharePoint toolkit configuration exceeds its approved limit"
            }
            SharePointConfigErrorCode::UnsupportedCapability => {
                "the SharePoint toolkit requires an unavailable runtime authority"
            }
        })
    }
}

impl std::error::Error for SharePointConfigError {}

pub(crate) struct SharePointToolkitConfig {
    site_url: Url,
    site_hostname: Box<str>,
    site_path: Box<str>,
    selected_tools: Vec<Box<str>>,
    access_token: Option<Zeroizing<String>>,
    authorization: DelegatedAuthorizationRequirement,
}

impl SharePointToolkitConfig {
    pub(crate) fn parse(
        toolkit_name: &str,
        settings: &Map<String, Value>,
        delegated_tokens: &Map<String, Value>,
    ) -> Result<Self, SharePointConfigError> {
        validate_text(toolkit_name, MAX_IDENTITY_BYTES)?;
        let configuration = settings
            .get("sharepoint_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        required_text(configuration, "client_id", MAX_IDENTITY_BYTES)?;
        required_text(configuration, "client_secret", MAX_SECRET_BYTES)?;
        validate_scopes(configuration.get("scopes"))?;

        let configured_site_url = required_text(configuration, "site_url", MAX_URL_BYTES)?;
        let configured_site_path = optional_text(settings, "site_path", MAX_URL_BYTES)?;
        let site_url = resolve_site_url(configured_site_url, configured_site_path)?;
        let site_hostname = site_url
            .host_str()
            .ok_or_else(invalid_configuration)?
            .to_owned()
            .into_boxed_str();
        let site_path = site_url
            .path()
            .trim_matches('/')
            .to_owned()
            .into_boxed_str();

        let configured_discovery =
            optional_text(configuration, "oauth_discovery_endpoint", MAX_URL_BYTES)?
                .ok_or_else(unsupported_capability)?;
        let discovery = parse_discovery_url(configured_discovery)?;
        let discovery_key = canonical_url(&discovery);
        let configuration_uuid =
            optional_text(configuration, "configuration_uuid", MAX_IDENTITY_BYTES)?;
        let site_key = canonical_url(&site_url);
        let configured_site_key =
            if configured_site_path.is_some_and(|path| !path.trim().trim_matches('/').is_empty()) {
                site_key.as_str()
            } else {
                configured_site_url
            };
        let access_token = resolve_access_token(
            delegated_tokens,
            configuration_uuid,
            configured_discovery,
            &discovery_key,
            configured_site_key,
            &site_key,
        )?;

        let resource_metadata_url =
            format!("{discovery_key}/v2.0/.well-known/openid-configuration");
        let authorization_url = format!("{discovery_key}/oauth2/v2.0/authorize");
        if resource_metadata_url.len() > MAX_URL_BYTES || authorization_url.len() > MAX_URL_BYTES {
            return Err(resource_exhausted());
        }
        let authorization = DelegatedAuthorizationRequirement::new(
            toolkit_name.to_owned(),
            "sharepoint".to_owned(),
            site_key,
            Some(resource_metadata_url.clone()),
            Some(format!(
                "Bearer error=\"unauthorized_client\", resource_metadata=\"{resource_metadata_url}\", authorization_uri=\"{authorization_url}\""
            )),
        )
        .and_then(|requirement| requirement.with_configured_oauth(&discovery_key, configuration))
        .ok_or_else(invalid_configuration)?;

        Ok(Self {
            site_url,
            site_hostname,
            site_path,
            selected_tools: selected_tools(settings)?,
            access_token,
            authorization,
        })
    }

    #[must_use]
    pub(crate) fn site_url(&self) -> &Url {
        &self.site_url
    }

    pub(crate) fn with_toolkit_id(mut self, id: Option<u64>) -> Self {
        self.authorization = self.authorization.with_toolkit_id(id);
        self
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }

    #[must_use]
    pub(crate) fn authorization(&self) -> &DelegatedAuthorizationRequirement {
        &self.authorization
    }

    #[must_use]
    pub(crate) fn requires_authorization(&self) -> bool {
        self.access_token.is_none()
    }

    pub(crate) fn into_client_parts(self) -> Result<SharePointClientConfig, SharePointConfigError> {
        let access_token = self.access_token.ok_or_else(invalid_configuration)?;
        Ok(SharePointClientConfig {
            site_hostname: self.site_hostname,
            site_path: self.site_path,
            access_token,
            authorization: self.authorization,
        })
    }
}

pub(crate) struct SharePointClientConfig {
    pub(super) site_hostname: Box<str>,
    pub(super) site_path: Box<str>,
    pub(super) access_token: Zeroizing<String>,
    pub(super) authorization: DelegatedAuthorizationRequirement,
}

fn resolve_site_url(value: &str, site_path: Option<&str>) -> Result<Url, SharePointConfigError> {
    let mut site_url = parse_site_url(value)?;
    if let Some(site_path) = site_path {
        let normalized = site_path.trim().trim_matches('/');
        let mut parts = normalized.splitn(2, '/');
        let prefix = parts.next().unwrap_or_default();
        let suffix = parts.next().unwrap_or_default();
        if !matches!(prefix, "sites" | "teams")
            || suffix.is_empty()
            || suffix
                .split('/')
                .any(|part| part.is_empty() || part == "..")
            || normalized.contains(['?', '#', '\\', '%'])
            || normalized.chars().any(char::is_control)
        {
            return Err(invalid_configuration());
        }
        site_url.set_path(&format!("/{normalized}"));
    }
    if site_url.path().ends_with('/') && site_url.path() != "/" {
        let path = site_url.path().trim_end_matches('/').to_owned();
        site_url.set_path(&path);
    }
    Ok(site_url)
}

fn parse_site_url(value: &str) -> Result<Url, SharePointConfigError> {
    if value.contains(['\\', '%']) {
        return Err(invalid_configuration());
    }
    let url = Url::parse(value).map_err(|_| invalid_configuration())?;
    if url.scheme() != "https"
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.port().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || url.path().split('/').any(|part| part == "..")
    {
        return Err(invalid_configuration());
    }
    Ok(url)
}

fn parse_discovery_url(value: &str) -> Result<Url, SharePointConfigError> {
    if value.contains(['\\', '%']) {
        return Err(invalid_configuration());
    }
    let url = Url::parse(value).map_err(|_| invalid_configuration())?;
    if url.scheme() != "https"
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.port().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err(invalid_configuration());
    }
    Ok(url)
}

fn validate_scopes(value: Option<&Value>) -> Result<(), SharePointConfigError> {
    let Some(value) = value else {
        return Ok(());
    };
    if value.is_null() {
        return Ok(());
    }
    let values = value.as_array().ok_or_else(invalid_configuration)?;
    if values.len() > MAX_SCOPES {
        return Err(resource_exhausted());
    }
    for value in values {
        let scope = value.as_str().ok_or_else(invalid_configuration)?;
        validate_text(scope, MAX_SCOPE_BYTES)?;
        if scope.chars().any(char::is_whitespace) {
            return Err(invalid_configuration());
        }
    }
    Ok(())
}

fn resolve_access_token(
    tokens: &Map<String, Value>,
    configuration_uuid: Option<&str>,
    configured_discovery: &str,
    canonical_discovery: &str,
    configured_site_url: &str,
    canonical_site_url: &str,
) -> Result<Option<Zeroizing<String>>, SharePointConfigError> {
    let mut lookup_keys = Vec::with_capacity(6);
    if let Some(uuid) = configuration_uuid {
        push_unique(&mut lookup_keys, format!("{uuid}:{configured_discovery}"));
    }
    push_unique(&mut lookup_keys, configured_discovery.to_owned());
    push_unique(&mut lookup_keys, configured_site_url.to_owned());
    if let Some(uuid) = configuration_uuid {
        push_unique(&mut lookup_keys, format!("{uuid}:{canonical_discovery}"));
    }
    push_unique(&mut lookup_keys, canonical_discovery.to_owned());
    push_unique(&mut lookup_keys, canonical_site_url.to_owned());
    let value = lookup_keys.iter().find_map(|key| tokens.get(key));
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
    HeaderValue::from_str(token).map_err(|_| invalid_configuration())?;
    Ok(Some(Zeroizing::new(token.to_owned())))
}

fn push_unique(values: &mut Vec<String>, value: String) {
    if !values.contains(&value) {
        values.push(value);
    }
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, SharePointConfigError> {
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
    let mut selected = Vec::with_capacity(values.len().min(8));
    for value in values {
        let name = value.as_str().ok_or_else(invalid_configuration)?;
        validate_text(name, 64)?;
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

fn required_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, SharePointConfigError> {
    optional_text(object, name, limit)?.ok_or_else(invalid_configuration)
}

fn optional_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<Option<&'a str>, SharePointConfigError> {
    match object.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) if value.is_empty() => Ok(None),
        Some(Value::String(value)) => {
            validate_text(value, limit)?;
            Ok(Some(value))
        }
        Some(_) => Err(invalid_configuration()),
    }
}

fn validate_text(value: &str, limit: usize) -> Result<(), SharePointConfigError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || value.chars().any(char::is_control) {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn canonical_url(url: &Url) -> String {
    let mut canonical = url.clone();
    if canonical.path() == "/" {
        canonical.set_path("");
    }
    canonical.to_string().trim_end_matches('/').to_owned()
}

const fn invalid_configuration() -> SharePointConfigError {
    SharePointConfigError {
        code: SharePointConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> SharePointConfigError {
    SharePointConfigError {
        code: SharePointConfigErrorCode::ResourceExhausted,
    }
}

const fn unsupported_capability() -> SharePointConfigError {
    SharePointConfigError {
        code: SharePointConfigErrorCode::UnsupportedCapability,
    }
}
