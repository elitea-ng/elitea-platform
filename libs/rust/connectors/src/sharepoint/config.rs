#![allow(clippy::doc_markdown)] // `SharePoint`, `OneNote` are product names.
//! The `sharepoint` toolkit settings as the indexing connector reads them
//! (`sharepoint_configuration`: `site_url`, `client_id`, `client_secret`,
//! optional `scopes`, `oauth_discovery_endpoint`, `tenant_id` and
//! `access_token`; the toolkit's optional `site_path`).
//!
//! An index run has no user in front of it, so the delegated-token path the
//! agent toolkit takes (`SharePointToolkitConfig` in agent-runtime) is not
//! available here. The connector authenticates **app-only**: the client
//! credential of `sharepoint_configuration` is redeemed for a Microsoft Graph
//! token at the tenant's `oauth2/v2.0/token` endpoint. The tenant is taken,
//! in order, from `oauth_discovery_endpoint` (the toolkit's
//! `https://login.microsoftonline.com/<tenant>`), from `tenant_id`, or from
//! the site host (`contoso.sharepoint.com` → `contoso.onmicrosoft.com`, the
//! tenant's initial domain). A pre-issued `access_token` in the
//! configuration is used as is when present.

use std::fmt;

use serde_json::{Map, Value};
use url::Url;
use zeroize::Zeroizing;

const MAX_URL_BYTES: usize = 8 * 1_024;
const MAX_IDENTITY_BYTES: usize = 1_024;
const MAX_SECRET_BYTES: usize = 64 * 1_024;
const LOGIN_HOST: &str = "login.microsoftonline.com";
const SHAREPOINT_SUFFIX: &str = ".sharepoint.com";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SharePointConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure that never carries a credential or a URL.
pub struct SharePointConfigError {
    code: SharePointConfigErrorCode,
}

impl SharePointConfigError {
    #[must_use]
    pub const fn code(&self) -> SharePointConfigErrorCode {
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
        })
    }
}

impl std::error::Error for SharePointConfigError {}

/// How the connector obtains its Graph bearer.
#[derive(Clone)]
pub enum SharePointCredential {
    /// A token the configuration already carries.
    AccessToken(Zeroizing<String>),
    /// The app's client credential, redeemed at `token_endpoint`.
    ClientCredential {
        client_id: Box<str>,
        client_secret: Zeroizing<String>,
        token_endpoint: Url,
    },
}

/// The one SharePoint site a toolkit indexes and how to reach it.
///
/// `Debug` prints the site and the credential's kind, never a secret.
pub struct SharePointIndexConfig {
    site_url: Url,
    site_hostname: Box<str>,
    site_path: Box<str>,
    credential: SharePointCredential,
}

impl fmt::Debug for SharePointIndexConfig {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SharePointIndexConfig")
            .field("site_url", &self.site_url.as_str())
            .field(
                "credential",
                &match self.credential {
                    SharePointCredential::AccessToken(_) => "access_token",
                    SharePointCredential::ClientCredential { .. } => "client_credential",
                },
            )
            .finish_non_exhaustive()
    }
}

impl SharePointIndexConfig {
    /// Read the toolkit settings.
    ///
    /// # Errors
    ///
    /// A missing or malformed field, or one over its size limit.
    pub fn parse(settings: &Map<String, Value>) -> Result<Self, SharePointConfigError> {
        let configuration = settings
            .get("sharepoint_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let configured_site_url = required_text(configuration, "site_url", MAX_URL_BYTES)?;
        let configured_site_path = optional_text(settings, "site_path", MAX_URL_BYTES)?;
        let site_url = resolve_site_url(configured_site_url, configured_site_path)?;
        let site_hostname = site_url
            .host_str()
            .ok_or_else(invalid_configuration)?
            .to_owned();
        let site_path = site_url.path().trim_matches('/').to_owned();

        let credential = if let Some(token) =
            optional_text(configuration, "access_token", MAX_SECRET_BYTES)?
        {
            SharePointCredential::AccessToken(Zeroizing::new(token.to_owned()))
        } else {
            let client_id = required_text(configuration, "client_id", MAX_IDENTITY_BYTES)?;
            let client_secret = required_text(configuration, "client_secret", MAX_SECRET_BYTES)?;
            let authority = if let Some(discovery) =
                optional_text(configuration, "oauth_discovery_endpoint", MAX_URL_BYTES)?
            {
                parse_authority(discovery)?
            } else if let Some(tenant) =
                optional_text(configuration, "tenant_id", MAX_IDENTITY_BYTES)?
            {
                tenant_authority(tenant)?
            } else {
                let tenant = site_hostname
                    .strip_suffix(SHAREPOINT_SUFFIX)
                    .filter(|prefix| !prefix.is_empty() && !prefix.contains('.'))
                    .ok_or_else(invalid_configuration)?;
                tenant_authority(&format!("{tenant}.onmicrosoft.com"))?
            };
            let token_endpoint = authority
                .join("oauth2/v2.0/token")
                .map_err(|_| invalid_configuration())?;
            SharePointCredential::ClientCredential {
                client_id: client_id.into(),
                client_secret: Zeroizing::new(client_secret.to_owned()),
                token_endpoint,
            }
        };
        Ok(Self {
            site_url,
            site_hostname: site_hostname.into(),
            site_path: site_path.into(),
            credential,
        })
    }

    /// The site, `https://<host>/sites/<name>` or the root site.
    #[must_use]
    pub fn site_url(&self) -> &Url {
        &self.site_url
    }

    #[must_use]
    pub fn site_hostname(&self) -> &str {
        &self.site_hostname
    }

    /// The site's server-relative path without slashes (`sites/engineering`),
    /// empty for the root site.
    #[must_use]
    pub fn site_path(&self) -> &str {
        &self.site_path
    }

    #[must_use]
    pub fn credential(&self) -> &SharePointCredential {
        &self.credential
    }

    /// The hosts this connector sends to besides Graph: the identity provider
    /// (when the credential is redeemed) and the site host (file downloads
    /// redirect there).
    #[must_use]
    pub fn hosts(&self) -> Vec<&str> {
        let mut hosts = vec![self.site_hostname.as_ref()];
        if let SharePointCredential::ClientCredential { token_endpoint, .. } = &self.credential
            && let Some(host) = token_endpoint.host_str()
        {
            hosts.push(host);
        }
        hosts
    }
}

/// `https://login.microsoftonline.com/<tenant>/`, the authority a tenant id
/// or initial domain names.
fn tenant_authority(tenant: &str) -> Result<Url, SharePointConfigError> {
    if tenant.is_empty()
        || tenant.len() > MAX_IDENTITY_BYTES
        || !tenant
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '.'))
    {
        return Err(invalid_configuration());
    }
    Url::parse(&format!("https://{LOGIN_HOST}/{tenant}/")).map_err(|_| invalid_configuration())
}

/// The toolkit's `oauth_discovery_endpoint`
/// (`https://login.microsoftonline.com/<tenant>[/v2.0]`), with a trailing
/// slash so `join` appends.
fn parse_authority(value: &str) -> Result<Url, SharePointConfigError> {
    if value.contains(['\\', '%']) {
        return Err(invalid_configuration());
    }
    let mut url = Url::parse(value.trim()).map_err(|_| invalid_configuration())?;
    if url.scheme() != "https"
        || url.host_str().is_none_or(str::is_empty)
        || !url.username().is_empty()
        || url.password().is_some()
        || url.port().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || url.cannot_be_a_base()
    {
        return Err(invalid_configuration());
    }
    let mut path = url.path().trim_end_matches('/').to_owned();
    if let Some(stripped) = path.strip_suffix("/v2.0") {
        path = stripped.to_owned();
    }
    if path.is_empty()
        || path
            .split('/')
            .skip(1)
            .any(|part| part.is_empty() || part == "..")
    {
        return Err(invalid_configuration());
    }
    url.set_path(&format!("{path}/"));
    Ok(url)
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
    let url = Url::parse(value.trim()).map_err(|_| invalid_configuration())?;
    if url.scheme() != "https"
        || url.host_str().is_none_or(str::is_empty)
        || !url.username().is_empty()
        || url.password().is_some()
        || url.port().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || url.cannot_be_a_base()
        || url.path().split('/').any(|part| part == "..")
    {
        return Err(invalid_configuration());
    }
    Ok(url)
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
        Some(Value::String(value)) if value.trim().is_empty() => Ok(None),
        Some(Value::String(value)) => {
            if value.len() > limit {
                return Err(resource_exhausted());
            }
            if value.chars().any(char::is_control) {
                return Err(invalid_configuration());
            }
            Ok(Some(value))
        }
        Some(_) => Err(invalid_configuration()),
    }
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

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn parse(settings: &Value) -> Result<SharePointIndexConfig, SharePointConfigError> {
        SharePointIndexConfig::parse(settings.as_object().expect("settings"))
    }

    #[test]
    fn the_tenant_comes_from_the_discovery_endpoint_then_tenant_id_then_the_site_host() {
        let discovery = parse(&json!({
            "sharepoint_configuration": {
                "site_url": "https://contoso.sharepoint.com/sites/eng/",
                "client_id": "app", "client_secret": "s3cret",
                "oauth_discovery_endpoint": "https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555/v2.0",
            }
        }))
        .expect("config");
        let SharePointCredential::ClientCredential { token_endpoint, .. } = discovery.credential()
        else {
            panic!("client credential")
        };
        assert_eq!(
            token_endpoint.as_str(),
            "https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555/oauth2/v2.0/token"
        );
        assert_eq!(discovery.site_path(), "sites/eng");
        assert_eq!(
            discovery.site_url().as_str(),
            "https://contoso.sharepoint.com/sites/eng"
        );

        let tenant = parse(&json!({
            "sharepoint_configuration": {
                "site_url": "https://contoso.sharepoint.com",
                "client_id": "app", "client_secret": "s3cret", "tenant_id": "contoso.onmicrosoft.com",
            },
            "site_path": "/sites/docs/",
        }))
        .expect("config");
        let SharePointCredential::ClientCredential { token_endpoint, .. } = tenant.credential()
        else {
            panic!("client credential")
        };
        assert_eq!(
            token_endpoint.as_str(),
            "https://login.microsoftonline.com/contoso.onmicrosoft.com/oauth2/v2.0/token"
        );
        assert_eq!(tenant.site_path(), "sites/docs");

        let host = parse(&json!({
            "sharepoint_configuration": {
                "site_url": "https://contoso.sharepoint.com/sites/eng",
                "client_id": "app", "client_secret": "s3cret",
            }
        }))
        .expect("config");
        let SharePointCredential::ClientCredential { token_endpoint, .. } = host.credential()
        else {
            panic!("client credential")
        };
        assert_eq!(
            token_endpoint.as_str(),
            "https://login.microsoftonline.com/contoso.onmicrosoft.com/oauth2/v2.0/token"
        );
        assert_eq!(
            host.hosts(),
            ["contoso.sharepoint.com", "login.microsoftonline.com"]
        );
    }

    #[test]
    fn a_pre_issued_token_needs_no_client_credential_and_an_unknown_tenant_is_refused() {
        let token = parse(&json!({
            "sharepoint_configuration": {
                "site_url": "https://sp.example.com/sites/eng",
                "access_token": "eyJ...",
            }
        }))
        .expect("config");
        assert!(matches!(
            token.credential(),
            SharePointCredential::AccessToken(_)
        ));
        assert_eq!(token.hosts(), ["sp.example.com"]);
        // An on-premises host with a client credential names no tenant.
        let refused = parse(&json!({
            "sharepoint_configuration": {
                "site_url": "https://sp.example.com/sites/eng",
                "client_id": "app", "client_secret": "s3cret",
            }
        }))
        .expect_err("refused");
        assert_eq!(
            refused.code(),
            SharePointConfigErrorCode::InvalidConfiguration
        );
        for bad in [
            json!({"sharepoint_configuration": {"site_url": "http://contoso.sharepoint.com", "client_id": "a", "client_secret": "b"}}),
            json!({"sharepoint_configuration": {"site_url": "https://contoso.sharepoint.com", "client_id": "a", "client_secret": "b"}, "site_path": "docs"}),
            json!({"sharepoint_configuration": {"site_url": "https://contoso.sharepoint.com", "client_id": "a"}}),
            json!({}),
        ] {
            assert!(parse(&bad).is_err());
        }
    }
}
