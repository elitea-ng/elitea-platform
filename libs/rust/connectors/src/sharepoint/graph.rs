#![allow(clippy::doc_markdown)] // `SharePoint`, `OneNote` are product names.
//! The Microsoft Graph client the SharePoint connector sends through: the
//! app-only token (redeemed, cached, renewed a minute before it expires and
//! once more when Graph answers 401), bounded JSON reads with `$top` and
//! `@odata.nextLink` paging left to the caller, and content downloads that
//! follow Graph's one redirect to the pre-authenticated download URL.
//!
//! Every request goes through [`SourceHttp`]: the egress allowlist (Graph,
//! the identity provider and the site host must all be on it), the 429
//! backoff and the listing and document caps.

use std::time::{Duration, Instant};

use serde_json::Value;
use tokio::sync::Mutex;
use zeroize::Zeroizing;

use super::config::SharePointCredential;
use crate::source::{Failure, SourceHttp};
use crate::transport::header::{ACCEPT, AUTHORIZATION, CONTENT_TYPE, LOCATION};
use crate::transport::{HeaderMap, HeaderValue, Method, Request, StatusCode, Url};

/// Graph's base, the only host a listing URL may name.
pub const GRAPH_BASE: &str = "https://graph.microsoft.com/v1.0/";
pub(super) const GRAPH_HOST: &str = "graph.microsoft.com";
/// The scope of an app-only Graph token.
const GRAPH_SCOPE: &str = "https://graph.microsoft.com/.default";
/// Renew a token this long before the provider says it expires.
const RENEWAL_MARGIN: Duration = Duration::from_mins(1);
/// A token the provider gave no lifetime for is kept this long.
const DEFAULT_LIFETIME: Duration = Duration::from_mins(50);
const MAX_TOKEN_BODY_BYTES: u64 = 64 * 1_024;
const MAX_SEGMENT_BYTES: usize = 8 * 1_024;
const MAX_NEXT_LINK_BYTES: usize = 16 * 1_024;

struct CachedToken {
    bearer: Zeroizing<String>,
    expires_at: Instant,
}

/// One site's Graph client.
pub struct GraphClient {
    http: SourceHttp,
    credential: SharePointCredential,
    token: Mutex<Option<CachedToken>>,
}

impl GraphClient {
    #[must_use]
    pub fn new(credential: SharePointCredential, http: SourceHttp) -> Self {
        Self {
            http,
            credential,
            token: Mutex::new(None),
        }
    }

    #[must_use]
    pub fn http(&self) -> &SourceHttp {
        &self.http
    }

    /// The credential and transport, to build a client with other limits.
    #[must_use]
    pub fn into_parts(self) -> (SharePointCredential, SourceHttp) {
        (self.credential, self.http)
    }

    /// The bearer to send: the configured token, or the cached app token
    /// (redeemed or renewed as needed).
    async fn bearer(&self) -> Result<Zeroizing<String>, Failure> {
        let (client_id, client_secret, token_endpoint) = match &self.credential {
            SharePointCredential::AccessToken(token) => return Ok(token.clone()),
            SharePointCredential::ClientCredential {
                client_id,
                client_secret,
                token_endpoint,
            } => (client_id, client_secret, token_endpoint),
        };
        let mut cached = self.token.lock().await;
        if let Some(token) = cached.as_ref()
            && token.expires_at > Instant::now() + RENEWAL_MARGIN
        {
            return Ok(token.bearer.clone());
        }
        let body = Zeroizing::new(
            url::form_urlencoded::Serializer::new(String::new())
                .append_pair("grant_type", "client_credentials")
                .append_pair("client_id", client_id)
                .append_pair("client_secret", client_secret)
                .append_pair("scope", GRAPH_SCOPE)
                .finish(),
        );
        let mut request = Request::new(Method::POST, token_endpoint.clone());
        request.headers_mut().insert(
            CONTENT_TYPE,
            HeaderValue::from_static("application/x-www-form-urlencoded"),
        );
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static("application/json"));
        *request.body_mut() = Some(body.as_bytes().to_vec().into());
        let mut response = self.http.send(request).await?;
        let status = response.status();
        if status == StatusCode::BAD_REQUEST
            || status == StatusCode::UNAUTHORIZED
            || status == StatusCode::FORBIDDEN
        {
            return Err(Failure::Client(format!(
                "the identity provider refused the SharePoint app credential (HTTP {})",
                status.as_u16()
            )));
        }
        if !status.is_success() {
            return Err(Failure::Status(status));
        }
        let bytes = SourceHttp::body(&mut response, MAX_TOKEN_BODY_BYTES).await?;
        let parsed: Value = serde_json::from_slice(&bytes)
            .map_err(|_| Failure::InvalidResponse("the token answer is not JSON"))?;
        let access_token = parsed
            .get("access_token")
            .and_then(Value::as_str)
            .filter(|token| !token.is_empty() && HeaderValue::from_str(token).is_ok())
            .ok_or(Failure::InvalidResponse(
                "the token answer carries no usable access_token",
            ))?;
        let lifetime = parsed
            .get("expires_in")
            .and_then(|value| {
                value
                    .as_u64()
                    .or_else(|| value.as_str().and_then(|text| text.parse().ok()))
            })
            .map_or(DEFAULT_LIFETIME, Duration::from_secs);
        let bearer = Zeroizing::new(access_token.to_owned());
        *cached = Some(CachedToken {
            bearer: bearer.clone(),
            expires_at: Instant::now() + lifetime,
        });
        Ok(bearer)
    }

    async fn forget_token(&self) {
        *self.token.lock().await = None;
    }

    async fn authorized(
        &self,
        method: Method,
        url: Url,
        accept: &'static str,
    ) -> Result<Request, Failure> {
        validate_graph_url(&url)?;
        let bearer = self.bearer().await?;
        let mut request = Request::new(method, url);
        let mut authorization = HeaderValue::from_str(&format!("Bearer {}", bearer.as_str()))
            .map_err(|_| {
                Failure::Client("the Graph token cannot be sent as a header".to_owned())
            })?;
        authorization.set_sensitive(true);
        request.headers_mut().insert(AUTHORIZATION, authorization);
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static(accept));
        Ok(request)
    }

    /// One JSON answer (a listing page). A 401 renews the token and tries
    /// once more; a 404 is [`Failure::Status`] for the caller to interpret.
    ///
    /// # Errors
    ///
    /// See [`SourceHttp::json`].
    pub async fn get_json(&self, url: Url) -> Result<(HeaderMap, Value), Failure> {
        let request = self
            .authorized(Method::GET, url.clone(), "application/json")
            .await?;
        match self.http.json(request).await {
            Err(Failure::Status(StatusCode::UNAUTHORIZED))
                if matches!(
                    self.credential,
                    SharePointCredential::ClientCredential { .. }
                ) =>
            {
                self.forget_token().await;
                let request = self
                    .authorized(Method::GET, url, "application/json")
                    .await?;
                self.http.json(request).await
            }
            other => other,
        }
    }

    /// A drive item's content: Graph answers `/content` with a redirect to a
    /// pre-authenticated URL on the site host, which is fetched without the
    /// bearer (the allowlist decides whether that host is reachable).
    ///
    /// # Errors
    ///
    /// See [`SourceHttp::raw`]; a redirect without a usable `Location`.
    pub async fn get_content(&self, url: Url) -> Result<Vec<u8>, Failure> {
        let request = self
            .authorized(Method::GET, url.clone(), "application/octet-stream")
            .await?;
        let mut response = self.http.send(request).await?;
        if response.status() == StatusCode::UNAUTHORIZED
            && matches!(
                self.credential,
                SharePointCredential::ClientCredential { .. }
            )
        {
            drop(response);
            self.forget_token().await;
            let request = self
                .authorized(Method::GET, url, "application/octet-stream")
                .await?;
            response = self.http.send(request).await?;
        }
        let status = response.status();
        if status.is_redirection() {
            let location = response
                .headers()
                .get(LOCATION)
                .and_then(|value| value.to_str().ok())
                .filter(|value| value.len() <= MAX_NEXT_LINK_BYTES)
                .and_then(|value| Url::parse(value).ok())
                .filter(|url| url.scheme() == "https")
                .ok_or(Failure::InvalidResponse(
                    "the content redirect names no https location",
                ))?;
            drop(response);
            let mut request = Request::new(Method::GET, location);
            request
                .headers_mut()
                .insert(ACCEPT, HeaderValue::from_static("application/octet-stream"));
            return self.http.raw(request).await;
        }
        if !status.is_success() {
            return Err(Failure::Status(status));
        }
        SourceHttp::body(&mut response, self.http.limits().max_document_bytes).await
    }
}

/// `https://graph.microsoft.com/v1.0/{segments}`, each segment checked.
///
/// # Errors
///
/// An empty, over-long or `..` segment.
pub fn graph_url(segments: &[&str]) -> Result<Url, Failure> {
    let mut url = Url::parse(GRAPH_BASE).map_err(|_| Failure::Client("Graph base".to_owned()))?;
    {
        let mut path = url
            .path_segments_mut()
            .map_err(|()| Failure::Client("Graph base".to_owned()))?;
        path.pop_if_empty();
        for segment in segments {
            validate_segment(segment)?;
            path.push(segment);
        }
    }
    Ok(url)
}

/// A Graph URL whose path is given whole (the `sites/{host}:/{path}` and
/// `root:/{folder}:` address forms keep their colons).
///
/// # Errors
///
/// A path segment that is empty, over-long or `..`.
pub fn graph_path_url(path: &str) -> Result<Url, Failure> {
    for segment in path.split('/') {
        if segment.is_empty() {
            continue;
        }
        validate_segment(segment)?;
    }
    let mut url = Url::parse(GRAPH_BASE).map_err(|_| Failure::Client("Graph base".to_owned()))?;
    url.set_path(&format!("/v1.0/{}", path.trim_start_matches('/')));
    Ok(url)
}

fn validate_segment(value: &str) -> Result<(), Failure> {
    if value.is_empty()
        || value.len() > MAX_SEGMENT_BYTES
        || value == ".."
        || value.chars().any(char::is_control)
    {
        return Err(Failure::Client(
            "a Graph address segment is unusable".to_owned(),
        ));
    }
    Ok(())
}

/// A `@odata.nextLink` or `@odata.deltaLink`, admitted only on Graph.
///
/// # Errors
///
/// Not an https Graph v1.0 URL.
pub fn parse_graph_link(value: &str) -> Result<Url, Failure> {
    if value.len() > MAX_NEXT_LINK_BYTES || value.contains('\\') {
        return Err(Failure::InvalidResponse("a Graph link is malformed"));
    }
    let url =
        Url::parse(value).map_err(|_| Failure::InvalidResponse("a Graph link is malformed"))?;
    validate_graph_url(&url)?;
    Ok(url)
}

fn validate_graph_url(url: &Url) -> Result<(), Failure> {
    if url.scheme() != "https"
        || url.host_str() != Some(GRAPH_HOST)
        || url.port().is_some()
        || !url.username().is_empty()
        || url.password().is_some()
        || !url.path().starts_with("/v1.0/")
        || url.fragment().is_some()
    {
        return Err(Failure::InvalidResponse(
            "a URL is not a Graph v1.0 address",
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn graph_addresses_keep_their_colon_forms_and_refuse_other_hosts() {
        assert_eq!(
            graph_path_url("sites/contoso.sharepoint.com:/sites/eng")
                .expect("url")
                .as_str(),
            "https://graph.microsoft.com/v1.0/sites/contoso.sharepoint.com:/sites/eng"
        );
        assert_eq!(
            graph_url(&["drives", "d 1", "root", "delta"])
                .expect("url")
                .as_str(),
            "https://graph.microsoft.com/v1.0/drives/d%201/root/delta"
        );
        assert!(graph_url(&["drives", ".."]).is_err());
        assert!(
            parse_graph_link("https://graph.microsoft.com/v1.0/drives/x/root/delta?token=1")
                .is_ok()
        );
        assert!(parse_graph_link("https://evil.example/v1.0/drives").is_err());
        assert!(parse_graph_link("https://graph.microsoft.com/beta/drives").is_err());
    }
}
