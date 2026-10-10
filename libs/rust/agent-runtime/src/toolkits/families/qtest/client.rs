use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderValue, USER_AGENT,
};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::Value;
use zeroize::Zeroizing;

use super::config::QtestToolkitConfig;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
/// `DEFAULT_QTEST_API_TIMEOUT_SECONDS`: qTest searches can be slow.
const REQUEST_TIMEOUT: Duration = Duration::from_mins(3);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_REQUEST_BYTES: usize = 1_024 * 1_024;
const MAX_RESPONSE_BYTES: usize = 8 * 1_024 * 1_024;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";
const JSON_CONTENT_TYPE: &str = "application/json";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum QtestClientErrorCode {
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
}

/// Stable qTest failure without provider bodies, URLs or credentials.
pub(crate) struct QtestClientError {
    code: QtestClientErrorCode,
    retryable: bool,
}

impl QtestClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> QtestClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            QtestClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "qtest.configuration.invalid",
                "the qTest toolkit configuration is invalid",
            ),
            QtestClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "qtest.request.invalid",
                "qTest rejected the request as invalid",
            ),
            QtestClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "qtest.authentication.failed",
                "qTest authentication failed",
            ),
            QtestClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "qtest.authorization.failed",
                "qTest did not authorize the request",
            ),
            QtestClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "qtest.resource.not_found",
                "the requested qTest resource was not found",
            ),
            QtestClientErrorCode::Conflict => (
                ErrorCategory::InvalidInput,
                "qtest.resource.conflict",
                "the qTest request conflicts with current provider state",
            ),
            QtestClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "qtest.rate_limited",
                "qTest rate limited the request",
            ),
            QtestClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "qtest.timeout",
                "the qTest request timed out",
            ),
            QtestClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "qtest.unavailable",
                "qTest is unavailable",
            ),
            QtestClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "qtest.response.invalid",
                "qTest returned an invalid response",
            ),
            QtestClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "qtest.response.resource_exhausted",
                "the qTest response exceeds the approved limit",
            ),
            QtestClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "qtest.effect.unknown_outcome",
                "qTest may have applied the requested effect; reconcile it before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }
}

impl fmt::Debug for QtestClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("QtestClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for QtestClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            QtestClientErrorCode::InvalidConfiguration => {
                "the qTest client configuration is invalid"
            }
            QtestClientErrorCode::InvalidInput => "the qTest request is invalid",
            QtestClientErrorCode::Authentication => "qTest authentication failed",
            QtestClientErrorCode::Authorization => "qTest authorization failed",
            QtestClientErrorCode::NotFound => "the qTest resource was not found",
            QtestClientErrorCode::Conflict => "the qTest request conflicts",
            QtestClientErrorCode::RateLimited => "qTest rate limited the request",
            QtestClientErrorCode::Timeout => "the qTest request timed out",
            QtestClientErrorCode::DependencyUnavailable => "qTest is unavailable",
            QtestClientErrorCode::InvalidResponse => "qTest returned an invalid response",
            QtestClientErrorCode::ResourceExhausted => {
                "the qTest response exceeds its approved limit"
            }
            QtestClientErrorCode::UnknownOutcome => {
                "the qTest effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for QtestClientError {}

/// One bounded HTTP exchange: the status plus the decoded JSON body.
pub(in crate::toolkits) struct QtestHttpResponse {
    pub(super) status: StatusCode,
    pub(super) body: Option<Value>,
}

impl QtestHttpResponse {
    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(status: StatusCode, body: Option<Value>) -> Self {
        Self { status, body }
    }
}

/// The only network seam: production uses a redirect-free HTTPS reqwest
/// client, tests a recording fixture.
#[async_trait]
pub(in crate::toolkits) trait QtestTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<QtestHttpResponse, QtestClientError>;
}

struct ReqwestQtestTransport {
    http: reqwest::Client,
}

#[async_trait]
impl QtestTransport for ReqwestQtestTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<QtestHttpResponse, QtestClientError> {
        let mut response = self
            .http
            .execute(request)
            .await
            .map_err(|source| map_reqwest_error(&source, effect))?;
        if response
            .headers()
            .get(CONTENT_LENGTH)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.parse::<usize>().ok())
            .is_some_and(|length| length > MAX_RESPONSE_BYTES)
        {
            return Err(resource_exhausted(effect));
        }
        let mut bytes = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|source| map_reqwest_error(&source, effect))?
        {
            if bytes.len().saturating_add(chunk.len()) > MAX_RESPONSE_BYTES {
                return Err(resource_exhausted(effect));
            }
            bytes.extend_from_slice(&chunk);
        }
        let body = if bytes.is_empty() {
            None
        } else {
            serde_json::from_slice(&bytes).ok()
        };
        Ok(QtestHttpResponse {
            status: response.status(),
            body,
        })
    }
}

/// One project-scoped qTest Manager v3 request.
pub(in crate::toolkits) struct QtestCall<'a> {
    pub(super) method: Method,
    /// The path below `/api/v3/projects/{projectId}/`.
    pub(super) path: &'a str,
    pub(super) query: Vec<(&'static str, String)>,
    pub(super) body: Option<Value>,
    pub(super) effect: bool,
}

impl<'a> QtestCall<'a> {
    pub(super) const fn get(path: &'a str) -> Self {
        Self {
            method: Method::GET,
            path,
            query: Vec::new(),
            body: None,
            effect: false,
        }
    }

    pub(super) fn query(mut self, name: &'static str, value: impl Into<String>) -> Self {
        self.query.push((name, value.into()));
        self
    }

    /// A body-carrying request. `effect` marks a request that may change
    /// provider state, whose failure after sending is an unknown outcome.
    pub(super) const fn send(method: Method, path: &'a str, body: Value, effect: bool) -> Self {
        Self {
            method,
            path,
            query: Vec::new(),
            body: Some(body),
            effect,
        }
    }
}

/// qTest Manager's v3 REST API for one project.
#[async_trait]
pub(in crate::toolkits) trait QtestApi: Send + Sync {
    /// The response whatever its status, for callers that read a 400/404.
    async fn exchange(&self, call: QtestCall<'_>) -> Result<QtestHttpResponse, QtestClientError>;

    /// A successful response's body; any other status is an error.
    async fn call(&self, call: QtestCall<'_>) -> Result<Value, QtestClientError> {
        let effect = call.effect;
        let response = self.exchange(call).await?;
        if !response.status.is_success() {
            return Err(status_error(response.status, effect));
        }
        Ok(response.body.unwrap_or(Value::Null))
    }
}

/// Invocation-scoped qTest client (`Authorization: Bearer <token>`).
pub(in crate::toolkits) struct QtestClient {
    config: QtestToolkitConfig,
    transport: Arc<dyn QtestTransport>,
}

impl QtestClient {
    pub(in crate::toolkits) fn new(config: QtestToolkitConfig) -> Result<Self, QtestClientError> {
        let http = reqwest::Client::builder()
            .https_only(true)
            .redirect(reqwest::redirect::Policy::none())
            .retry(reqwest::retry::never())
            .connect_timeout(CONNECT_TIMEOUT)
            .timeout(REQUEST_TIMEOUT)
            .pool_idle_timeout(POOL_IDLE_TIMEOUT)
            .pool_max_idle_per_host(MAX_IDLE_PER_HOST)
            .build()
            .map_err(|_| invalid_configuration())?;
        Ok(Self::with_transport(
            config,
            Arc::new(ReqwestQtestTransport { http }),
        ))
    }

    pub(in crate::toolkits) fn with_transport(
        config: QtestToolkitConfig,
        transport: Arc<dyn QtestTransport>,
    ) -> Self {
        Self { config, transport }
    }
}

#[async_trait]
impl QtestApi for QtestClient {
    async fn exchange(&self, call: QtestCall<'_>) -> Result<QtestHttpResponse, QtestClientError> {
        if call.path.is_empty()
            || call.path.starts_with('/')
            || call.path.contains("..")
            || !call
                .path
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'/'))
        {
            return Err(invalid_input());
        }
        let mut url = Url::parse(&format!(
            "{}/api/v3/projects/{}/{}",
            self.config.base_url(),
            self.config.project_id(),
            call.path
        ))
        .map_err(|_| invalid_configuration())?;
        if !call.query.is_empty() {
            let mut pairs = url.query_pairs_mut();
            for (name, value) in &call.query {
                pairs.append_pair(name, value);
            }
        }
        let mut request = Request::new(call.method, url);
        let header = Zeroizing::new(format!("Bearer {}", self.config.token()));
        let mut authorization =
            HeaderValue::from_str(&header).map_err(|_| invalid_configuration())?;
        authorization.set_sensitive(true);
        let headers = request.headers_mut();
        headers.insert(AUTHORIZATION, authorization);
        headers.insert(ACCEPT, HeaderValue::from_static(JSON_CONTENT_TYPE));
        headers.insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
        if let Some(body) = &call.body {
            let encoded = serde_json::to_vec(body).map_err(|_| invalid_input())?;
            if encoded.len() > MAX_REQUEST_BYTES {
                return Err(QtestClientError {
                    code: QtestClientErrorCode::ResourceExhausted,
                    retryable: false,
                });
            }
            headers.insert(CONTENT_TYPE, HeaderValue::from_static(JSON_CONTENT_TYPE));
            *request.body_mut() = Some(encoded.into());
        }
        self.transport.execute(request, call.effect).await
    }
}

pub(super) fn status_error(status: StatusCode, effect: bool) -> QtestClientError {
    if effect
        && (status.is_server_error()
            || matches!(
                status,
                StatusCode::REQUEST_TIMEOUT | StatusCode::TOO_MANY_REQUESTS
            ))
    {
        return unknown_outcome();
    }
    let code = match status {
        StatusCode::BAD_REQUEST | StatusCode::UNPROCESSABLE_ENTITY => {
            QtestClientErrorCode::InvalidInput
        }
        StatusCode::UNAUTHORIZED => QtestClientErrorCode::Authentication,
        StatusCode::FORBIDDEN => QtestClientErrorCode::Authorization,
        StatusCode::NOT_FOUND => QtestClientErrorCode::NotFound,
        StatusCode::CONFLICT => QtestClientErrorCode::Conflict,
        StatusCode::REQUEST_TIMEOUT => QtestClientErrorCode::Timeout,
        StatusCode::TOO_MANY_REQUESTS => QtestClientErrorCode::RateLimited,
        status if status.is_server_error() => QtestClientErrorCode::DependencyUnavailable,
        _ => QtestClientErrorCode::InvalidResponse,
    };
    QtestClientError {
        code,
        retryable: !effect
            && matches!(
                code,
                QtestClientErrorCode::Timeout
                    | QtestClientErrorCode::RateLimited
                    | QtestClientErrorCode::DependencyUnavailable
            ),
    }
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> QtestClientError {
    if effect {
        return unknown_outcome();
    }
    QtestClientError {
        code: if source.is_timeout() {
            QtestClientErrorCode::Timeout
        } else {
            QtestClientErrorCode::DependencyUnavailable
        },
        retryable: true,
    }
}

const fn invalid_configuration() -> QtestClientError {
    QtestClientError {
        code: QtestClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

pub(super) const fn invalid_input() -> QtestClientError {
    QtestClientError {
        code: QtestClientErrorCode::InvalidInput,
        retryable: false,
    }
}

pub(super) const fn invalid_response() -> QtestClientError {
    QtestClientError {
        code: QtestClientErrorCode::InvalidResponse,
        retryable: false,
    }
}

pub(super) const fn resource_exhausted(effect: bool) -> QtestClientError {
    if effect {
        unknown_outcome()
    } else {
        QtestClientError {
            code: QtestClientErrorCode::ResourceExhausted,
            retryable: false,
        }
    }
}

const fn unknown_outcome() -> QtestClientError {
    QtestClientError {
        code: QtestClientErrorCode::UnknownOutcome,
        retryable: false,
    }
}
