//! The Bitbucket client: one repository on Bitbucket Cloud (REST 2.0) or
//! Bitbucket Server / Data Center (REST 1.0), shared by the `bitbucket`
//! family and the Bitbucket connector (moved from agent-runtime, ADR-0030
//! decision 3).
//!
//! The SDK reaches both through `atlassian-python-api`; this client speaks
//! the same endpoints directly over a bounded HTTPS, no-redirect, no-retry
//! transport with a sensitive Basic credential.

use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use serde_json::Value;
use zeroize::Zeroizing;

use super::config::{BitbucketHosting, BitbucketToolkitConfig};
use crate::reqwest_adapter::ClientPolicy;
use crate::transport::header::{ACCEPT, AUTHORIZATION, CONTENT_TYPE, HeaderValue, LOCATION};
use crate::transport::{Method, Request, StatusCode, Transport, TransportError, Url};

const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 8;
/// One provider response.
pub const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_REQUEST_BYTES: usize = 2 * 1_024 * 1_024;
/// The tool listings' page and item bounds.
pub const MAX_PAGES: usize = 10;
pub const MAX_ITEMS: usize = 1_000;
const PAGE_SIZE: &str = "100";
const USER_AGENT: &str = "elitea-worker-rust/0.1";
const MULTIPART_BOUNDARY: &str = "elitea-bitbucket-form-boundary";

/// The client the family builds: HTTPS only, no redirect, no retry, these
/// timeouts. A host passes it to its reqwest adapter.
pub const CLIENT_POLICY: ClientPolicy = ClientPolicy {
    connect_timeout: CONNECT_TIMEOUT,
    request_timeout: REQUEST_TIMEOUT,
    pool_idle_timeout: POOL_IDLE_TIMEOUT,
    max_idle_per_host: MAX_IDLE_PER_HOST,
    user_agent: USER_AGENT,
    disable_retries: true,
};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum BitbucketClientErrorCode {
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

/// Stable provider failure without origin, repository, path, body or secret.
pub struct BitbucketClientError {
    code: BitbucketClientErrorCode,
    retryable: bool,
}

impl BitbucketClientError {
    #[must_use]
    pub const fn code(&self) -> BitbucketClientErrorCode {
        self.code
    }

    #[must_use]
    pub const fn retryable(&self) -> bool {
        self.retryable
    }

    /// A non-retryable failure with this code (also the fixture
    /// constructor).
    #[must_use]
    pub const fn fixture(code: BitbucketClientErrorCode) -> Self {
        Self {
            code,
            retryable: false,
        }
    }
}

impl fmt::Debug for BitbucketClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("BitbucketClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for BitbucketClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            BitbucketClientErrorCode::InvalidConfiguration => {
                "the Bitbucket client configuration is invalid"
            }
            BitbucketClientErrorCode::InvalidInput => "the Bitbucket request is invalid",
            BitbucketClientErrorCode::Authentication => "Bitbucket authentication failed",
            BitbucketClientErrorCode::Authorization => "Bitbucket authorization failed",
            BitbucketClientErrorCode::NotFound => "the Bitbucket resource was not found",
            BitbucketClientErrorCode::Conflict => "the Bitbucket resource is in conflict",
            BitbucketClientErrorCode::RateLimited => "Bitbucket rate limited the request",
            BitbucketClientErrorCode::Timeout => "the Bitbucket request timed out",
            BitbucketClientErrorCode::DependencyUnavailable => "Bitbucket is unavailable",
            BitbucketClientErrorCode::InvalidResponse => "Bitbucket returned an invalid response",
            BitbucketClientErrorCode::ResourceExhausted => {
                "the Bitbucket request or response exceeds its approved limit"
            }
            BitbucketClientErrorCode::UnknownOutcome => {
                "the Bitbucket effect outcome is unknown and must be reconciled"
            }
            BitbucketClientErrorCode::EgressRefused => {
                "the Bitbucket host is not on the egress allowlist"
            }
        })
    }
}

impl std::error::Error for BitbucketClientError {}

/// A bounded provider answer: JSON when the provider labelled it JSON, and
/// the body as UTF-8 text whenever it is text.
pub struct BitbucketHttpResponse {
    pub status: StatusCode,
    pub json: Option<Value>,
    pub text: Option<String>,
    pub location: Option<String>,
}

impl BitbucketHttpResponse {
    #[cfg(any(test, feature = "test-support"))]
    pub fn json(status: StatusCode, body: Value) -> Self {
        Self {
            status,
            text: Some(body.to_string()),
            json: Some(body),
            location: None,
        }
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn text(status: StatusCode, body: &str) -> Self {
        Self {
            status,
            json: None,
            text: Some(body.to_owned()),
            location: None,
        }
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn redirect(location: &str) -> Self {
        Self {
            status: StatusCode::FOUND,
            json: None,
            text: None,
            location: Some(location.to_owned()),
        }
    }
}

#[async_trait]
pub trait BitbucketTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<BitbucketHttpResponse, BitbucketClientError>;
}

/// The production [`BitbucketTransport`]: the host's [`Transport`], every
/// body bounded.
struct HttpBitbucketTransport {
    inner: Arc<dyn Transport>,
}

#[async_trait]
impl BitbucketTransport for HttpBitbucketTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<BitbucketHttpResponse, BitbucketClientError> {
        let mut response = self
            .inner
            .execute(request)
            .await
            .map_err(|source| map_transport_error(source, effect))?;
        if response.declares_more_than(MAX_RESPONSE_BYTES) {
            return Err(response_failure(effect, resource_exhausted()));
        }
        let json_content_type = response
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.split(';').next())
            .is_some_and(|value| value.trim().eq_ignore_ascii_case("application/json"));
        let location = response
            .headers()
            .get(LOCATION)
            .and_then(|value| value.to_str().ok())
            .map(ToOwned::to_owned);
        let bytes = response
            .bytes_within(MAX_RESPONSE_BYTES)
            .await
            .map_err(|source| map_transport_error(source, effect))?
            .ok_or_else(|| response_failure(effect, resource_exhausted()))?;
        let json = if json_content_type && !bytes.is_empty() {
            serde_json::from_slice(&bytes).ok()
        } else {
            None
        };
        Ok(BitbucketHttpResponse {
            status: response.status(),
            json,
            text: String::from_utf8(bytes).ok(),
            location,
        })
    }
}

/// A request body in one of the encodings the two Bitbucket APIs take.
pub enum Body<'a> {
    None,
    Json(&'a Value),
    Form(&'a [(&'a str, &'a str)]),
    Multipart(&'a [(&'a str, &'a str)]),
}

/// One repository's REST surface: the configuration, the transport, the
/// URL builders, the Basic-authenticated request builder and the status
/// mapping. The family adds its operation ledger and active branch on top.
pub struct BitbucketRest {
    config: BitbucketToolkitConfig,
    transport: Arc<dyn BitbucketTransport>,
}

impl BitbucketRest {
    /// One client over the host's transport (built with [`CLIENT_POLICY`]).
    #[must_use]
    pub fn new(config: BitbucketToolkitConfig, transport: Arc<dyn Transport>) -> Self {
        Self::with_transport(
            config,
            Arc::new(HttpBitbucketTransport { inner: transport }),
        )
    }

    /// One client over a provider-level transport (a recorded fixture).
    #[must_use]
    pub fn with_transport(
        config: BitbucketToolkitConfig,
        transport: Arc<dyn BitbucketTransport>,
    ) -> Self {
        Self { config, transport }
    }

    pub fn config(&self) -> &BitbucketToolkitConfig {
        &self.config
    }

    pub fn transport(&self) -> &Arc<dyn BitbucketTransport> {
        &self.transport
    }

    pub fn cloud(&self) -> bool {
        self.config.hosting() == BitbucketHosting::Cloud
    }

    /// The repository resource: `/2.0/repositories/{workspace}/{repo}` on
    /// Cloud, `{context}/rest/api/1.0/projects/{project}/repos/{repo}` on
    /// Server, then `suffix`.
    pub fn repo_url(&self, suffix: &[&str]) -> Result<Url, BitbucketClientError> {
        let mut url = self.config.base_url().clone();
        {
            let mut path = url
                .path_segments_mut()
                .map_err(|()| invalid_configuration())?;
            path.pop_if_empty();
            if self.cloud() {
                path.extend(["2.0", "repositories"]);
                path.extend([self.config.project(), self.config.repository()]);
            } else {
                path.extend(["rest", "api", "1.0", "projects"]);
                path.extend([self.config.project(), "repos", self.config.repository()]);
            }
            path.extend(suffix.iter().copied());
        }
        Ok(url)
    }

    /// Server's branch-utils resource, which owns branch deletion.
    pub fn branch_utils_url(&self) -> Result<Url, BitbucketClientError> {
        let mut url = self.config.base_url().clone();
        {
            let mut path = url
                .path_segments_mut()
                .map_err(|()| invalid_configuration())?;
            path.pop_if_empty();
            path.extend(["rest", "branch-utils", "1.0", "projects"]);
            path.extend([
                self.config.project(),
                "repos",
                self.config.repository(),
                "branches",
            ]);
        }
        Ok(url)
    }

    pub fn request(
        &self,
        method: Method,
        mut url: Url,
        query: &[(&str, &str)],
        body: &Body<'_>,
    ) -> Result<Request, BitbucketClientError> {
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
        let credential = Zeroizing::new(format!(
            "{}:{}",
            self.config.username(),
            self.config.password()
        ));
        let encoded = Zeroizing::new(format!("Basic {}", STANDARD.encode(credential.as_bytes())));
        let mut authorization =
            HeaderValue::from_str(&encoded).map_err(|_| invalid_configuration())?;
        authorization.set_sensitive(true);
        request.headers_mut().insert(AUTHORIZATION, authorization);
        let (content_type, bytes) = match body {
            Body::None => return Ok(request),
            Body::Json(value) => (
                "application/json".to_owned(),
                serde_json::to_vec(value).map_err(|_| invalid_input())?,
            ),
            Body::Form(fields) => {
                // `Url`'s query serializer is application/x-www-form-urlencoded.
                let mut scratch =
                    Url::parse("https://form.invalid/").map_err(|_| invalid_input())?;
                scratch.query_pairs_mut().extend_pairs(fields.iter());
                (
                    "application/x-www-form-urlencoded".to_owned(),
                    scratch.query().unwrap_or_default().as_bytes().to_vec(),
                )
            }
            Body::Multipart(fields) => multipart(fields)?,
        };
        if bytes.len() > MAX_REQUEST_BYTES {
            return Err(resource_exhausted());
        }
        request.headers_mut().insert(
            CONTENT_TYPE,
            HeaderValue::from_str(&content_type).map_err(|_| invalid_input())?,
        );
        *request.body_mut() = Some(bytes.into());
        Ok(request)
    }

    pub async fn send(
        &self,
        method: Method,
        url: Url,
        query: &[(&str, &str)],
        body: &Body<'_>,
        effect: bool,
    ) -> Result<BitbucketHttpResponse, BitbucketClientError> {
        let request = self.request(method, url, query, body)?;
        let response = self.transport.execute(request, effect).await?;
        map_http_status(response.status, effect)?;
        Ok(response)
    }

    pub async fn get_json(
        &self,
        url: Url,
        query: &[(&str, &str)],
    ) -> Result<Value, BitbucketClientError> {
        self.send(Method::GET, url, query, &Body::None, false)
            .await?
            .json
            .ok_or_else(invalid_response)
    }

    /// Every `values` item across a paged listing, bounded.
    pub async fn paged(
        &self,
        url: Url,
        filters: &[(&str, &str)],
    ) -> Result<Vec<Value>, BitbucketClientError> {
        let mut output = Vec::new();
        if self.cloud() {
            let mut query = filters.to_vec();
            query.push(("pagelen", PAGE_SIZE));
            let mut next = {
                let mut first = url;
                {
                    let mut pairs = first.query_pairs_mut();
                    for (name, value) in &query {
                        pairs.append_pair(name, value);
                    }
                }
                first
            };
            for page in 0.. {
                if page >= MAX_PAGES {
                    return Err(resource_exhausted());
                }
                let body = self.get_json(next.clone(), &[]).await?;
                push_values(&mut output, &body)?;
                match body.get("next") {
                    None | Some(Value::Null) => break,
                    Some(Value::String(link)) => next = self.same_repository_link(link)?,
                    Some(_) => return Err(invalid_response()),
                }
            }
        } else {
            let mut start = String::from("0");
            for page in 0.. {
                if page >= MAX_PAGES {
                    return Err(resource_exhausted());
                }
                let mut query = filters.to_vec();
                query.push(("limit", PAGE_SIZE));
                query.push(("start", &start));
                let body = self.get_json(url.clone(), &query).await?;
                push_values(&mut output, &body)?;
                if body.get("isLastPage").and_then(Value::as_bool) != Some(false) {
                    break;
                }
                let next = body
                    .get("nextPageStart")
                    .and_then(Value::as_u64)
                    .ok_or_else(invalid_response)?;
                start = next.to_string();
            }
        }
        Ok(output)
    }

    /// A provider-supplied absolute link (Cloud `next`, a diff redirect) is
    /// followed only when it stays on this repository's API resource.
    pub fn same_repository_link(&self, link: &str) -> Result<Url, BitbucketClientError> {
        let base = self.repo_url(&[])?;
        let candidate = base.join(link).map_err(|_| invalid_response())?;
        let prefix = format!("{}/", base.path().trim_end_matches('/'));
        if candidate.origin() != base.origin()
            || !candidate.path().starts_with(&prefix)
            || !candidate.username().is_empty()
            || candidate.password().is_some()
        {
            return Err(invalid_response());
        }
        Ok(candidate)
    }

    /// Cloud addresses source by the branch head's commit hash.
    pub async fn branch_hash(&self, branch: &str) -> Result<String, BitbucketClientError> {
        let body = self
            .get_json(self.repo_url(&["refs", "branches", branch])?, &[])
            .await?;
        body.get("target")
            .and_then(|target| target.get("hash"))
            .and_then(Value::as_str)
            .filter(|hash| valid_hash(hash))
            .map(ToOwned::to_owned)
            .ok_or_else(invalid_response)
    }
}

/// `multipart/form-data` with text fields, as `requests` sends `files=`.
fn multipart(fields: &[(&str, &str)]) -> Result<(String, Vec<u8>), BitbucketClientError> {
    let boundary = if fields.iter().any(|(name, value)| {
        name.contains(MULTIPART_BOUNDARY) || value.contains(MULTIPART_BOUNDARY)
    }) {
        return Err(invalid_input());
    } else {
        MULTIPART_BOUNDARY
    };
    let mut body = Vec::new();
    for (name, value) in fields {
        if name.contains(['"', '\r', '\n']) {
            return Err(invalid_input());
        }
        body.extend_from_slice(
            format!("--{boundary}\r\nContent-Disposition: form-data; name=\"{name}\"\r\n\r\n")
                .as_bytes(),
        );
        body.extend_from_slice(value.as_bytes());
        body.extend_from_slice(b"\r\n");
    }
    body.extend_from_slice(format!("--{boundary}--\r\n").as_bytes());
    Ok((format!("multipart/form-data; boundary={boundary}"), body))
}

/// Append a listing page's `values`, bounded by the tool limit.
pub fn push_values(output: &mut Vec<Value>, body: &Value) -> Result<(), BitbucketClientError> {
    let values = body
        .get("values")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    if output.len().saturating_add(values.len()) > MAX_ITEMS {
        return Err(resource_exhausted());
    }
    output.extend(values.iter().cloned());
    Ok(())
}

/// A commit hash Bitbucket may answer with (7–64 hex digits).
#[must_use]
pub fn valid_hash(value: &str) -> bool {
    (7..=64).contains(&value.len()) && value.bytes().all(|byte| byte.is_ascii_hexdigit())
}

pub fn map_http_status(status: StatusCode, effect: bool) -> Result<(), BitbucketClientError> {
    if status.is_success() {
        return Ok(());
    }
    let code = match status {
        StatusCode::REQUEST_TIMEOUT | StatusCode::TOO_MANY_REQUESTS if effect => {
            BitbucketClientErrorCode::UnknownOutcome
        }
        status if status.is_server_error() && effect => BitbucketClientErrorCode::UnknownOutcome,
        StatusCode::BAD_REQUEST | StatusCode::UNPROCESSABLE_ENTITY => {
            BitbucketClientErrorCode::InvalidInput
        }
        StatusCode::UNAUTHORIZED => BitbucketClientErrorCode::Authentication,
        StatusCode::FORBIDDEN => BitbucketClientErrorCode::Authorization,
        StatusCode::NOT_FOUND => BitbucketClientErrorCode::NotFound,
        StatusCode::CONFLICT => BitbucketClientErrorCode::Conflict,
        StatusCode::REQUEST_TIMEOUT => BitbucketClientErrorCode::Timeout,
        StatusCode::TOO_MANY_REQUESTS => BitbucketClientErrorCode::RateLimited,
        status if status.is_server_error() => BitbucketClientErrorCode::DependencyUnavailable,
        _ if effect => BitbucketClientErrorCode::UnknownOutcome,
        _ => BitbucketClientErrorCode::InvalidResponse,
    };
    Err(BitbucketClientError {
        code,
        retryable: !effect
            && matches!(
                code,
                BitbucketClientErrorCode::Timeout
                    | BitbucketClientErrorCode::RateLimited
                    | BitbucketClientErrorCode::DependencyUnavailable
            ),
    })
}

fn map_transport_error(source: TransportError, effect: bool) -> BitbucketClientError {
    // Refused before anything was sent: the outcome is known even for a write.
    if source.is_refused() {
        return error(BitbucketClientErrorCode::EgressRefused);
    }
    if effect {
        return unknown_outcome();
    }
    if source.is_timeout() {
        return BitbucketClientError {
            code: BitbucketClientErrorCode::Timeout,
            retryable: true,
        };
    }
    if source.is_connect() || source.is_request() || source.is_body() || source.is_decode() {
        return BitbucketClientError {
            code: BitbucketClientErrorCode::DependencyUnavailable,
            retryable: true,
        };
    }
    invalid_response()
}

#[must_use]
pub const fn response_failure(effect: bool, error: BitbucketClientError) -> BitbucketClientError {
    if effect { unknown_outcome() } else { error }
}

#[must_use]
pub const fn error(code: BitbucketClientErrorCode) -> BitbucketClientError {
    BitbucketClientError {
        code,
        retryable: false,
    }
}

#[must_use]
pub const fn invalid_configuration() -> BitbucketClientError {
    error(BitbucketClientErrorCode::InvalidConfiguration)
}

#[must_use]
pub const fn invalid_input() -> BitbucketClientError {
    error(BitbucketClientErrorCode::InvalidInput)
}

#[must_use]
pub const fn invalid_response() -> BitbucketClientError {
    error(BitbucketClientErrorCode::InvalidResponse)
}

#[must_use]
pub const fn resource_exhausted() -> BitbucketClientError {
    error(BitbucketClientErrorCode::ResourceExhausted)
}

#[must_use]
pub const fn unknown_outcome() -> BitbucketClientError {
    error(BitbucketClientErrorCode::UnknownOutcome)
}
