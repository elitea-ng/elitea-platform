use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64_STANDARD;
use percent_encoding::{AsciiSet, NON_ALPHANUMERIC, utf8_percent_encode};
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderValue, USER_AGENT,
};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::Value;
use zeroize::Zeroizing;

use super::config::TestRailToolkitConfig;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
/// `testrail_api`'s default request timeout.
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_REQUEST_BYTES: usize = 256 * 1_024;
const MAX_RESPONSE_BYTES: usize = 4 * 1_024 * 1_024;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";
const JSON_CONTENT_TYPE: &str = "application/json";
const API_PREFIX: &str = "index.php?/api/v2/";
/// What `urllib.parse.quote_plus` leaves unescaped.
const QUERY_VALUE: &AsciiSet = &NON_ALPHANUMERIC
    .remove(b'-')
    .remove(b'.')
    .remove(b'_')
    .remove(b'~');

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum TestRailClientErrorCode {
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

/// Stable `TestRail` failure without provider bodies, URLs or credentials.
pub(crate) struct TestRailClientError {
    code: TestRailClientErrorCode,
    retryable: bool,
}

impl TestRailClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> TestRailClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            TestRailClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "testrail.configuration.invalid",
                "the TestRail toolkit configuration is invalid",
            ),
            TestRailClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "testrail.request.invalid",
                "TestRail rejected the request as invalid",
            ),
            TestRailClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "testrail.authentication.failed",
                "TestRail authentication failed",
            ),
            TestRailClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "testrail.authorization.failed",
                "TestRail did not authorize the request",
            ),
            TestRailClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "testrail.resource.not_found",
                "the requested TestRail resource was not found",
            ),
            TestRailClientErrorCode::Conflict => (
                ErrorCategory::InvalidInput,
                "testrail.resource.conflict",
                "the TestRail request conflicts with current provider state",
            ),
            TestRailClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "testrail.rate_limited",
                "TestRail rate limited the request",
            ),
            TestRailClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "testrail.timeout",
                "the TestRail request timed out",
            ),
            TestRailClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "testrail.unavailable",
                "TestRail is unavailable",
            ),
            TestRailClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "testrail.response.invalid",
                "TestRail returned an invalid response",
            ),
            TestRailClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "testrail.response.resource_exhausted",
                "the TestRail response exceeds the approved limit",
            ),
            TestRailClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "testrail.effect.unknown_outcome",
                "TestRail may have applied the requested effect; reconcile it before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }
}

impl fmt::Debug for TestRailClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("TestRailClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for TestRailClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            TestRailClientErrorCode::InvalidConfiguration => {
                "the TestRail client configuration is invalid"
            }
            TestRailClientErrorCode::InvalidInput => "the TestRail request is invalid",
            TestRailClientErrorCode::Authentication => "TestRail authentication failed",
            TestRailClientErrorCode::Authorization => "TestRail authorization failed",
            TestRailClientErrorCode::NotFound => "the TestRail resource was not found",
            TestRailClientErrorCode::Conflict => "the TestRail request conflicts",
            TestRailClientErrorCode::RateLimited => "TestRail rate limited the request",
            TestRailClientErrorCode::Timeout => "the TestRail request timed out",
            TestRailClientErrorCode::DependencyUnavailable => "TestRail is unavailable",
            TestRailClientErrorCode::InvalidResponse => "TestRail returned an invalid response",
            TestRailClientErrorCode::ResourceExhausted => {
                "the TestRail response exceeds its approved limit"
            }
            TestRailClientErrorCode::UnknownOutcome => {
                "the TestRail effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for TestRailClientError {}

/// One bounded HTTP exchange: the status plus the decoded body (`None` when
/// empty), as `testrail_api`'s response handler returns it.
pub(in crate::toolkits) struct TestRailHttpResponse {
    status: StatusCode,
    body: Option<Value>,
}

impl TestRailHttpResponse {
    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(status: StatusCode, body: Option<Value>) -> Self {
        Self { status, body }
    }
}

/// The only network seam: production uses a redirect-free HTTPS reqwest
/// client, tests a recording fixture.
#[async_trait]
pub(in crate::toolkits) trait TestRailTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<TestRailHttpResponse, TestRailClientError>;
}

struct ReqwestTestRailTransport {
    http: reqwest::Client,
}

#[async_trait]
impl TestRailTransport for ReqwestTestRailTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<TestRailHttpResponse, TestRailClientError> {
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
        // `response.json()`, else `response.text or None`.
        let body =
            if bytes.is_empty() {
                None
            } else {
                Some(serde_json::from_slice(&bytes).unwrap_or_else(|_| {
                    Value::String(String::from_utf8_lossy(&bytes).into_owned())
                }))
            };
        Ok(TestRailHttpResponse {
            status: response.status(),
            body,
        })
    }
}

/// A GET/POST parameter after `testrail_api`'s converter: lists become
/// `1,2,3`, booleans `0`/`1`, everything else its text. `None` is dropped by
/// `requests`, so it is never a parameter.
pub(in crate::toolkits) type Params = Vec<(String, String)>;

/// `testrail_api.TestRailAPI`, reduced to its wire contract.
#[async_trait]
pub(in crate::toolkits) trait TestRailApi: Send + Sync {
    /// `GET {url}/index.php?/api/v2/{endpoint}&{params}`.
    async fn get(&self, endpoint: &str, params: &Params) -> Result<Value, TestRailClientError>;
    /// `POST` with a JSON body (`{}` when the caller sends none).
    async fn post(
        &self,
        endpoint: &str,
        params: &Params,
        body: &Value,
    ) -> Result<Value, TestRailClientError>;
}

/// Invocation-scoped `TestRail` API v2 client.
pub(in crate::toolkits) struct TestRailClient {
    config: TestRailToolkitConfig,
    transport: Arc<dyn TestRailTransport>,
}

impl TestRailClient {
    pub(in crate::toolkits) fn new(
        config: TestRailToolkitConfig,
    ) -> Result<Self, TestRailClientError> {
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
            Arc::new(ReqwestTestRailTransport { http }),
        ))
    }

    pub(in crate::toolkits) fn with_transport(
        config: TestRailToolkitConfig,
        transport: Arc<dyn TestRailTransport>,
    ) -> Self {
        Self { config, transport }
    }

    pub(in crate::toolkits) fn instance(&self) -> &str {
        self.config.instance()
    }

    fn url(&self, endpoint: &str, params: &Params) -> Result<Url, TestRailClientError> {
        if endpoint.is_empty()
            || !endpoint
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'/'))
        {
            return Err(invalid_input());
        }
        let mut target = format!("{}/{API_PREFIX}{endpoint}", self.config.instance());
        for (name, value) in params {
            target.push('&');
            target.extend(utf8_percent_encode(name, QUERY_VALUE));
            target.push('=');
            target.push_str(
                &utf8_percent_encode(value, QUERY_VALUE)
                    .to_string()
                    .replace("%20", "+"),
            );
        }
        Url::parse(&target).map_err(|_| invalid_input())
    }

    async fn call(
        &self,
        method: Method,
        endpoint: &str,
        params: &Params,
        body: Option<&Value>,
    ) -> Result<Value, TestRailClientError> {
        let effect = method != Method::GET;
        let mut request = Request::new(method, self.url(endpoint, params)?);
        let mut source = Zeroizing::new(String::with_capacity(
            self.config.email().len() + self.config.password().len() + 1,
        ));
        source.push_str(self.config.email());
        source.push(':');
        source.push_str(self.config.password());
        let header = Zeroizing::new(format!(
            "Basic {}",
            BASE64_STANDARD.encode(source.as_bytes())
        ));
        let mut authorization =
            HeaderValue::from_str(&header).map_err(|_| invalid_configuration())?;
        authorization.set_sensitive(true);
        let headers = request.headers_mut();
        headers.insert(AUTHORIZATION, authorization);
        headers.insert(ACCEPT, HeaderValue::from_static(JSON_CONTENT_TYPE));
        // `testrail_api` sets this on every non-attachment request.
        headers.insert(CONTENT_TYPE, HeaderValue::from_static(JSON_CONTENT_TYPE));
        headers.insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
        if let Some(body) = body {
            let encoded = serde_json::to_vec(body).map_err(|_| invalid_input())?;
            if encoded.len() > MAX_REQUEST_BYTES {
                return Err(TestRailClientError {
                    code: TestRailClientErrorCode::ResourceExhausted,
                    retryable: false,
                });
            }
            *request.body_mut() = Some(encoded.into());
        }
        let response = self.transport.execute(request, effect).await?;
        if !response.status.is_success() {
            return Err(status_error(response.status, effect));
        }
        Ok(response.body.unwrap_or(Value::Null))
    }
}

#[async_trait]
impl TestRailApi for TestRailClient {
    async fn get(&self, endpoint: &str, params: &Params) -> Result<Value, TestRailClientError> {
        self.call(Method::GET, endpoint, params, None).await
    }

    async fn post(
        &self,
        endpoint: &str,
        params: &Params,
        body: &Value,
    ) -> Result<Value, TestRailClientError> {
        self.call(Method::POST, endpoint, params, Some(body)).await
    }
}

fn status_error(status: StatusCode, effect: bool) -> TestRailClientError {
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
            TestRailClientErrorCode::InvalidInput
        }
        StatusCode::UNAUTHORIZED => TestRailClientErrorCode::Authentication,
        StatusCode::FORBIDDEN => TestRailClientErrorCode::Authorization,
        StatusCode::NOT_FOUND => TestRailClientErrorCode::NotFound,
        StatusCode::CONFLICT => TestRailClientErrorCode::Conflict,
        StatusCode::REQUEST_TIMEOUT => TestRailClientErrorCode::Timeout,
        StatusCode::TOO_MANY_REQUESTS => TestRailClientErrorCode::RateLimited,
        status if status.is_server_error() => TestRailClientErrorCode::DependencyUnavailable,
        _ => TestRailClientErrorCode::InvalidResponse,
    };
    TestRailClientError {
        code,
        retryable: !effect
            && matches!(
                code,
                TestRailClientErrorCode::Timeout
                    | TestRailClientErrorCode::RateLimited
                    | TestRailClientErrorCode::DependencyUnavailable
            ),
    }
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> TestRailClientError {
    if effect {
        return unknown_outcome();
    }
    TestRailClientError {
        code: if source.is_timeout() {
            TestRailClientErrorCode::Timeout
        } else {
            TestRailClientErrorCode::DependencyUnavailable
        },
        retryable: true,
    }
}

const fn invalid_configuration() -> TestRailClientError {
    TestRailClientError {
        code: TestRailClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

pub(super) const fn invalid_input() -> TestRailClientError {
    TestRailClientError {
        code: TestRailClientErrorCode::InvalidInput,
        retryable: false,
    }
}

pub(super) const fn invalid_response() -> TestRailClientError {
    TestRailClientError {
        code: TestRailClientErrorCode::InvalidResponse,
        retryable: false,
    }
}

pub(super) const fn resource_exhausted(effect: bool) -> TestRailClientError {
    if effect {
        unknown_outcome()
    } else {
        TestRailClientError {
            code: TestRailClientErrorCode::ResourceExhausted,
            retryable: false,
        }
    }
}

const fn unknown_outcome() -> TestRailClientError {
    TestRailClientError {
        code: TestRailClientErrorCode::UnknownOutcome,
        retryable: false,
    }
}
