//! The GitLab wire layer the `gitlab` and `gitlab_org` families share, and
//! the GitLab connector reads through: one HTTPS, no-redirect, bounded
//! transport, the status and effect-confirmation mapping, and the request
//! builder that holds the private token.
//!
//! The error is named for the `gitlab_org` family that first owned this
//! layer; the single-project `gitlab` family converts it into its own.

use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use serde_json::Value;
use zeroize::Zeroizing;

use super::config::GitLabToolkitConfig;
use super::org_config::GitLabOrgToolkitConfig;
use crate::reqwest_adapter::ClientPolicy;
use crate::transport::header::{ACCEPT, CONTENT_TYPE, HeaderName, HeaderValue};
use crate::transport::{Method, Request, StatusCode, Transport, TransportError, Url};

const PRIVATE_TOKEN: HeaderName = HeaderName::from_static("private-token");
const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 8;
/// One provider response, decoded.
pub const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
/// One request body.
pub const MAX_REQUEST_BYTES: usize = 2 * 1_024 * 1_024;
const USER_AGENT: &str = "elitea-worker-rust/0.1";

/// The client both GitLab families build: HTTPS only, no redirect, no
/// retry, these timeouts. A host passes it to its reqwest adapter.
pub const CLIENT_POLICY: ClientPolicy = ClientPolicy {
    connect_timeout: CONNECT_TIMEOUT,
    request_timeout: REQUEST_TIMEOUT,
    pool_idle_timeout: POOL_IDLE_TIMEOUT,
    max_idle_per_host: MAX_IDLE_PER_HOST,
    user_agent: USER_AGENT,
    disable_retries: true,
};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum GitLabOrgClientErrorCode {
    InvalidConfiguration,
    InvalidInput,
    Authentication,
    Authorization,
    NotFound,
    Conflict,
    RateLimited,
    Timeout,
    DependencyUnavailable,
    InvalidResponse,
    ResourceExhausted,
    UnknownOutcome,
    /// The host is not on the egress allowlist; nothing was sent.
    EgressRefused,
}

/// Stable provider failure without origin, repository, path, body, or token.
pub struct GitLabOrgClientError {
    code: GitLabOrgClientErrorCode,
    retryable: bool,
}

impl GitLabOrgClientError {
    #[must_use]
    pub const fn code(&self) -> GitLabOrgClientErrorCode {
        self.code
    }

    #[must_use]
    pub const fn retryable(&self) -> bool {
        self.retryable
    }

    /// A failure with this code (also the fixture constructor).
    #[must_use]
    pub const fn fixture(code: GitLabOrgClientErrorCode, retryable: bool) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for GitLabOrgClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("GitLabOrgClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for GitLabOrgClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            GitLabOrgClientErrorCode::InvalidConfiguration => {
                "the GitLab Org client configuration is invalid"
            }
            GitLabOrgClientErrorCode::InvalidInput => "the GitLab Org request is invalid",
            GitLabOrgClientErrorCode::Authentication => "GitLab authentication failed",
            GitLabOrgClientErrorCode::Authorization => "GitLab authorization failed",
            GitLabOrgClientErrorCode::NotFound => "the GitLab resource was not found",
            GitLabOrgClientErrorCode::Conflict => "the GitLab resource is in conflict",
            GitLabOrgClientErrorCode::RateLimited => "GitLab rate limited the request",
            GitLabOrgClientErrorCode::Timeout => "the GitLab request timed out",
            GitLabOrgClientErrorCode::DependencyUnavailable => "GitLab is unavailable",
            GitLabOrgClientErrorCode::InvalidResponse => "GitLab returned an invalid response",
            GitLabOrgClientErrorCode::ResourceExhausted => {
                "the GitLab Org request or response exceeds its approved limit"
            }
            GitLabOrgClientErrorCode::UnknownOutcome => {
                "the GitLab effect outcome is unknown and must be reconciled"
            }
            GitLabOrgClientErrorCode::EgressRefused => {
                "the GitLab host is not on the egress allowlist"
            }
        })
    }
}

impl std::error::Error for GitLabOrgClientError {}

pub struct GitLabOrgHttpResponse {
    pub status: StatusCode,
    pub body: Option<Value>,
    pub json_content_type: bool,
    pub next_page: Option<Box<str>>,
}

impl GitLabOrgHttpResponse {
    #[cfg(any(test, feature = "test-support"))]
    pub fn fixture(status: StatusCode, body: Option<Value>, next_page: Option<&str>) -> Self {
        Self {
            status,
            body,
            json_content_type: true,
            next_page: next_page.map(Into::into),
        }
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn non_json_fixture(status: StatusCode) -> Self {
        Self {
            status,
            body: None,
            json_content_type: false,
            next_page: None,
        }
    }
}

#[async_trait]
pub trait GitLabOrgTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<GitLabOrgHttpResponse, GitLabOrgClientError>;
}

struct HttpGitLabTransport {
    inner: Arc<dyn Transport>,
}

#[async_trait]
impl GitLabOrgTransport for HttpGitLabTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<GitLabOrgHttpResponse, GitLabOrgClientError> {
        let mut response = self
            .inner
            .execute(request)
            .await
            .map_err(|source| map_transport_error(source, effect))?;
        if response.declares_more_than(MAX_RESPONSE_BYTES) {
            return Err(response_bound_failure(effect));
        }
        let json_content_type = response
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.split(';').next())
            .is_some_and(|value| value.trim().eq_ignore_ascii_case("application/json"));
        let next_page = parse_next_page(response.headers().get("x-next-page"), effect)?;
        let bytes = response
            .bytes_within(MAX_RESPONSE_BYTES)
            .await
            .map_err(|source| map_transport_error(source, effect))?
            .ok_or_else(|| response_bound_failure(effect))?;
        let body = if bytes.is_empty() {
            None
        } else if json_content_type {
            Some(serde_json::from_slice(&bytes).map_err(|_| response_shape_failure(effect))?)
        } else {
            None
        };
        Ok(GitLabOrgHttpResponse {
            status: response.status(),
            body,
            json_content_type,
            next_page,
        })
    }
}

/// The production GitLab transport over the host's [`Transport`] (built with
/// [`CLIENT_POLICY`]), shared by both families and the connector so all keep
/// one HTTPS, no-redirect, bounded wire policy.
#[must_use]
pub fn http_transport(inner: Arc<dyn Transport>) -> Arc<dyn GitLabOrgTransport> {
    Arc::new(HttpGitLabTransport { inner })
}

pub fn parse_next_page(
    value: Option<&HeaderValue>,
    effect: bool,
) -> Result<Option<Box<str>>, GitLabOrgClientError> {
    let Some(value) = value else {
        return Ok(None);
    };
    let value = value.to_str().map_err(|_| response_shape_failure(effect))?;
    if value.is_empty() {
        return Ok(None);
    }
    if value.len() > 20
        || !value.bytes().all(|byte| byte.is_ascii_digit())
        || !value.parse::<u64>().is_ok_and(|page| page > 0)
    {
        return Err(response_shape_failure(effect));
    }
    Ok(Some(value.into()))
}

pub fn validate_effect_status(
    method: &Method,
    status: StatusCode,
) -> Result<(), GitLabOrgClientError> {
    let expected = match *method {
        Method::POST => StatusCode::CREATED,
        Method::DELETE => StatusCode::NO_CONTENT,
        _ => return Err(invalid_configuration()),
    };
    if status != expected {
        return Err(unknown_outcome());
    }
    Ok(())
}

pub fn map_http_status(status: StatusCode, effect: bool) -> Result<(), GitLabOrgClientError> {
    if status.is_success() {
        return Ok(());
    }
    let code = match status {
        StatusCode::REQUEST_TIMEOUT | StatusCode::TOO_MANY_REQUESTS if effect => {
            GitLabOrgClientErrorCode::UnknownOutcome
        }
        status if status.is_server_error() && effect => GitLabOrgClientErrorCode::UnknownOutcome,
        StatusCode::BAD_REQUEST | StatusCode::UNPROCESSABLE_ENTITY => {
            GitLabOrgClientErrorCode::InvalidInput
        }
        StatusCode::UNAUTHORIZED => GitLabOrgClientErrorCode::Authentication,
        StatusCode::FORBIDDEN => GitLabOrgClientErrorCode::Authorization,
        StatusCode::NOT_FOUND => GitLabOrgClientErrorCode::NotFound,
        StatusCode::CONFLICT => GitLabOrgClientErrorCode::Conflict,
        StatusCode::REQUEST_TIMEOUT => GitLabOrgClientErrorCode::Timeout,
        StatusCode::TOO_MANY_REQUESTS => GitLabOrgClientErrorCode::RateLimited,
        status if status.is_server_error() => GitLabOrgClientErrorCode::DependencyUnavailable,
        _ if effect => GitLabOrgClientErrorCode::UnknownOutcome,
        _ => GitLabOrgClientErrorCode::InvalidResponse,
    };
    Err(error(
        code,
        !effect
            && matches!(
                code,
                GitLabOrgClientErrorCode::Timeout
                    | GitLabOrgClientErrorCode::RateLimited
                    | GitLabOrgClientErrorCode::DependencyUnavailable
            ),
    ))
}

fn map_transport_error(source: TransportError, effect: bool) -> GitLabOrgClientError {
    // Refused before anything was sent: the outcome is known even for a write.
    if source.is_refused() {
        return error(GitLabOrgClientErrorCode::EgressRefused, false);
    }
    if effect {
        return unknown_outcome();
    }
    if source.is_timeout() {
        return error(GitLabOrgClientErrorCode::Timeout, true);
    }
    if source.is_connect() || source.is_request() || source.is_body() || source.is_decode() {
        return error(GitLabOrgClientErrorCode::DependencyUnavailable, true);
    }
    invalid_response()
}

#[must_use]
pub fn response_bound_failure(effect: bool) -> GitLabOrgClientError {
    if effect {
        unknown_outcome()
    } else {
        resource_exhausted()
    }
}

#[must_use]
pub fn response_shape_failure(effect: bool) -> GitLabOrgClientError {
    if effect {
        unknown_outcome()
    } else {
        invalid_response()
    }
}

#[must_use]
pub const fn error(code: GitLabOrgClientErrorCode, retryable: bool) -> GitLabOrgClientError {
    GitLabOrgClientError { code, retryable }
}

#[must_use]
pub const fn invalid_configuration() -> GitLabOrgClientError {
    error(GitLabOrgClientErrorCode::InvalidConfiguration, false)
}

#[must_use]
pub const fn invalid_input() -> GitLabOrgClientError {
    error(GitLabOrgClientErrorCode::InvalidInput, false)
}

#[must_use]
pub const fn invalid_response() -> GitLabOrgClientError {
    error(GitLabOrgClientErrorCode::InvalidResponse, false)
}

#[must_use]
pub const fn resource_exhausted() -> GitLabOrgClientError {
    error(GitLabOrgClientErrorCode::ResourceExhausted, false)
}

#[must_use]
pub const fn unknown_outcome() -> GitLabOrgClientError {
    error(GitLabOrgClientErrorCode::UnknownOutcome, false)
}

/// A GitLab REST request under `{base}/api/v4/{segments}`, carrying the
/// private token as a sensitive header and, when there is one, a bounded
/// JSON body.
fn rest_request(
    base_url: &Url,
    private_token: &str,
    method: Method,
    segments: &[&str],
    query: &[(&str, String)],
    body: Option<&Value>,
) -> Result<Request, GitLabOrgClientError> {
    let mut url = base_url.clone();
    {
        let mut path = url
            .path_segments_mut()
            .map_err(|()| invalid_configuration())?;
        path.extend(["api", "v4"]);
        path.extend(segments.iter().copied());
    }
    if !query.is_empty() {
        let mut pairs = url.query_pairs_mut();
        for (name, value) in query {
            pairs.append_pair(name, value);
        }
    }
    let mut request = Request::new(method, url);
    request
        .headers_mut()
        .insert(ACCEPT, HeaderValue::from_static("application/json"));
    let mut token = HeaderValue::from_str(&Zeroizing::new(private_token.to_owned()))
        .map_err(|_| invalid_configuration())?;
    token.set_sensitive(true);
    request.headers_mut().insert(PRIVATE_TOKEN, token);
    if let Some(body) = body {
        let encoded = serde_json::to_vec(body).map_err(|_| invalid_input())?;
        if encoded.len() > MAX_REQUEST_BYTES {
            return Err(resource_exhausted());
        }
        request
            .headers_mut()
            .insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
        *request.body_mut() = Some(encoded.into());
    }
    Ok(request)
}

/// A `gitlab_org` request: `{base}/api/v4/{segments}`.
pub fn org_request(
    config: &GitLabOrgToolkitConfig,
    method: Method,
    segments: &[&str],
    query: &[(&str, String)],
    body: Option<&Value>,
) -> Result<Request, GitLabOrgClientError> {
    rest_request(
        config.base_url(),
        config.private_token(),
        method,
        segments,
        query,
        body,
    )
}

/// A single-project request: `{base}/api/v4/projects/{project}/{suffix}`.
pub fn project_request(
    config: &GitLabToolkitConfig,
    method: Method,
    suffix: &[&str],
    query: &[(&str, String)],
    body: Option<&Value>,
) -> Result<Request, GitLabOrgClientError> {
    let mut segments = Vec::with_capacity(suffix.len() + 2);
    segments.extend(["projects", config.repository()]);
    segments.extend_from_slice(suffix);
    rest_request(
        config.base_url(),
        config.private_token(),
        method,
        &segments,
        query,
        body,
    )
}
