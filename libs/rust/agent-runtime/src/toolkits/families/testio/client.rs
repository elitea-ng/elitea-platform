use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use reqwest::header::{ACCEPT, AUTHORIZATION, CONTENT_LENGTH, HeaderValue, USER_AGENT};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::Value;
use zeroize::Zeroizing;

use super::config::TestIoToolkitConfig;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_RESPONSE_BYTES: usize = 4 * 1_024 * 1_024;
pub(super) const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";
const JSON_CONTENT_TYPE: &str = "application/json";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum TestIoClientErrorCode {
    InvalidConfiguration,
    InvalidInput,
    Authentication,
    Authorization,
    NotFound,
    RateLimited,
    Timeout,
    DependencyUnavailable,
    InvalidResponse,
    ResourceExhausted,
}

/// Stable Test IO failure without provider bodies, URLs or the token.
pub(crate) struct TestIoClientError {
    code: TestIoClientErrorCode,
    retryable: bool,
}

impl TestIoClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> TestIoClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        // The 401/404 sentences are the SDK's own `_handle_response` texts.
        let (category, code, message) = match self.code {
            TestIoClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "testio.configuration.invalid",
                "the TestIO toolkit configuration is invalid",
            ),
            TestIoClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "testio.request.invalid",
                "the TestIO request is invalid",
            ),
            TestIoClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "testio.authentication.failed",
                "Unauthorized: Invalid API key",
            ),
            TestIoClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "testio.authorization.failed",
                "TestIO did not authorize the request",
            ),
            TestIoClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "testio.resource.not_found",
                "Not Found: The requested resource does not exist",
            ),
            TestIoClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "testio.rate_limited",
                "TestIO rate limited the request",
            ),
            TestIoClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "testio.timeout",
                "the TestIO request timed out",
            ),
            TestIoClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "testio.unavailable",
                "TestIO is unavailable",
            ),
            TestIoClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "testio.response.invalid",
                "TestIO returned an invalid response",
            ),
            TestIoClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "testio.response.resource_exhausted",
                "the TestIO response exceeds the approved limit",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(code: TestIoClientErrorCode, retryable: bool) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for TestIoClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("TestIoClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for TestIoClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            TestIoClientErrorCode::InvalidConfiguration => {
                "the TestIO client configuration is invalid"
            }
            TestIoClientErrorCode::InvalidInput => "the TestIO request is invalid",
            TestIoClientErrorCode::Authentication => "TestIO authentication failed",
            TestIoClientErrorCode::Authorization => "TestIO authorization failed",
            TestIoClientErrorCode::NotFound => "the TestIO resource was not found",
            TestIoClientErrorCode::RateLimited => "TestIO rate limited the request",
            TestIoClientErrorCode::Timeout => "the TestIO request timed out",
            TestIoClientErrorCode::DependencyUnavailable => "TestIO is unavailable",
            TestIoClientErrorCode::InvalidResponse => "TestIO returned an invalid response",
            TestIoClientErrorCode::ResourceExhausted => {
                "the TestIO response exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for TestIoClientError {}

/// One bounded HTTP exchange: the status plus the body when it is JSON.
pub(in crate::toolkits) struct TestIoHttpResponse {
    status: StatusCode,
    body: Option<Value>,
}

impl TestIoHttpResponse {
    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(status: StatusCode, body: Option<Value>) -> Self {
        Self { status, body }
    }
}

/// The only network seam: production uses a redirect-free HTTPS reqwest
/// client, tests a recording fixture.
#[async_trait]
pub(in crate::toolkits) trait TestIoTransport: Send + Sync {
    async fn execute(&self, request: Request) -> Result<TestIoHttpResponse, TestIoClientError>;
}

struct ReqwestTestIoTransport {
    http: reqwest::Client,
}

#[async_trait]
impl TestIoTransport for ReqwestTestIoTransport {
    async fn execute(&self, request: Request) -> Result<TestIoHttpResponse, TestIoClientError> {
        let mut response = self
            .http
            .execute(request)
            .await
            .map_err(|source| map_reqwest_error(&source))?;
        if response
            .headers()
            .get(CONTENT_LENGTH)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.parse::<usize>().ok())
            .is_some_and(|length| length > MAX_RESPONSE_BYTES)
        {
            return Err(resource_exhausted());
        }
        let mut bytes = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|source| map_reqwest_error(&source))?
        {
            if bytes.len().saturating_add(chunk.len()) > MAX_RESPONSE_BYTES {
                return Err(resource_exhausted());
            }
            bytes.extend_from_slice(&chunk);
        }
        let body = if bytes.is_empty() {
            None
        } else {
            serde_json::from_slice(&bytes).ok()
        };
        Ok(TestIoHttpResponse {
            status: response.status(),
            body,
        })
    }
}

/// The read operations the tools need. One generic GET keeps the request
/// shape (path, query, auth) in one place for every tool.
#[async_trait]
pub(in crate::toolkits) trait TestIoApi: Send + Sync {
    /// `GET {endpoint}/customer/v2/{segments...}?{query}` returning the JSON
    /// document.
    async fn get(
        &self,
        segments: &[String],
        query: &[(&'static str, String)],
    ) -> Result<Value, TestIoClientError>;
}

/// Invocation-scoped Test IO customer-API v2 client.
pub(in crate::toolkits) struct TestIoClient {
    config: TestIoToolkitConfig,
    transport: Arc<dyn TestIoTransport>,
}

impl TestIoClient {
    pub(in crate::toolkits) fn new(config: TestIoToolkitConfig) -> Result<Self, TestIoClientError> {
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
            Arc::new(ReqwestTestIoTransport { http }),
        ))
    }

    pub(in crate::toolkits) fn with_transport(
        config: TestIoToolkitConfig,
        transport: Arc<dyn TestIoTransport>,
    ) -> Self {
        Self { config, transport }
    }
}

#[async_trait]
impl TestIoApi for TestIoClient {
    async fn get(
        &self,
        segments: &[String],
        query: &[(&'static str, String)],
    ) -> Result<Value, TestIoClientError> {
        let mut url: Url = self.config.endpoint().clone();
        {
            let mut path = url
                .path_segments_mut()
                .map_err(|()| invalid_configuration())?;
            path.pop_if_empty().push("customer").push("v2");
            for segment in segments {
                path.push(segment);
            }
        }
        if !query.is_empty() {
            let mut pairs = url.query_pairs_mut();
            for (name, value) in query {
                pairs.append_pair(name, value);
            }
        }
        let mut request = Request::new(Method::GET, url);
        // TestIO's customer API and the SDK's own connection check
        // authenticate with `Token`; the SDK runtime's `Bearer` spelling is
        // the drift recorded in SOURCE_PARITY.md and is not copied.
        let header = Zeroizing::new(format!("Token {}", self.config.api_key()));
        let mut authorization =
            HeaderValue::from_str(&header).map_err(|_| invalid_configuration())?;
        authorization.set_sensitive(true);
        let headers = request.headers_mut();
        headers.insert(AUTHORIZATION, authorization);
        headers.insert(ACCEPT, HeaderValue::from_static(JSON_CONTENT_TYPE));
        headers.insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
        let response = self.transport.execute(request).await?;
        if !response.status.is_success() {
            return Err(status_error(response.status));
        }
        response.body.ok_or_else(invalid_response)
    }
}

fn status_error(status: StatusCode) -> TestIoClientError {
    let code = match status {
        StatusCode::BAD_REQUEST | StatusCode::UNPROCESSABLE_ENTITY => {
            TestIoClientErrorCode::InvalidInput
        }
        StatusCode::UNAUTHORIZED => TestIoClientErrorCode::Authentication,
        StatusCode::FORBIDDEN => TestIoClientErrorCode::Authorization,
        StatusCode::NOT_FOUND => TestIoClientErrorCode::NotFound,
        StatusCode::REQUEST_TIMEOUT => TestIoClientErrorCode::Timeout,
        StatusCode::TOO_MANY_REQUESTS => TestIoClientErrorCode::RateLimited,
        status if status.is_server_error() => TestIoClientErrorCode::DependencyUnavailable,
        _ => TestIoClientErrorCode::InvalidResponse,
    };
    TestIoClientError {
        code,
        retryable: matches!(
            code,
            TestIoClientErrorCode::Timeout
                | TestIoClientErrorCode::RateLimited
                | TestIoClientErrorCode::DependencyUnavailable
        ),
    }
}

fn map_reqwest_error(source: &reqwest::Error) -> TestIoClientError {
    TestIoClientError {
        code: if source.is_timeout() {
            TestIoClientErrorCode::Timeout
        } else {
            TestIoClientErrorCode::DependencyUnavailable
        },
        retryable: true,
    }
}

const fn invalid_configuration() -> TestIoClientError {
    TestIoClientError {
        code: TestIoClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

pub(super) const fn invalid_response() -> TestIoClientError {
    TestIoClientError {
        code: TestIoClientErrorCode::InvalidResponse,
        retryable: false,
    }
}

pub(super) const fn resource_exhausted() -> TestIoClientError {
    TestIoClientError {
        code: TestIoClientErrorCode::ResourceExhausted,
        retryable: false,
    }
}
