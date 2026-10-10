use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderValue, USER_AGENT,
};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value, json};
use tokio::sync::Mutex;
use zeroize::Zeroizing;

use super::config::XrayToolkitConfig;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
/// The SDK's authentication timeout; GraphQL calls had none.
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_REQUEST_BYTES: usize = 8 * 1_024 * 1_024;
const MAX_RESPONSE_BYTES: usize = 4 * 1_024 * 1_024;
const MAX_TOKEN_BYTES: usize = 16 * 1_024;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";
const JSON_CONTENT_TYPE: &str = "application/json";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum XrayClientErrorCode {
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
    UnknownOutcome,
}

/// Stable Xray failure without provider bodies, tokens or credentials.
pub(crate) struct XrayClientError {
    code: XrayClientErrorCode,
    retryable: bool,
}

impl XrayClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> XrayClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            XrayClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "xray.configuration.invalid",
                "the Xray Cloud toolkit configuration is invalid",
            ),
            XrayClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "xray.request.invalid",
                "Xray Cloud rejected the request as invalid",
            ),
            XrayClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "xray.authentication.failed",
                "Xray Cloud authentication failed",
            ),
            XrayClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "xray.authorization.failed",
                "Xray Cloud did not authorize the request",
            ),
            XrayClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "xray.resource.not_found",
                "the requested Xray Cloud resource was not found",
            ),
            XrayClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "xray.rate_limited",
                "Xray Cloud rate limited the request",
            ),
            XrayClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "xray.timeout",
                "the Xray Cloud request timed out",
            ),
            XrayClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "xray.unavailable",
                "Xray Cloud is unavailable",
            ),
            XrayClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "xray.response.invalid",
                "Xray Cloud returned an invalid response",
            ),
            XrayClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "xray.response.resource_exhausted",
                "the Xray Cloud request or response exceeds the approved limit",
            ),
            XrayClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "xray.effect.unknown_outcome",
                "Xray Cloud may have applied the requested effect; reconcile it before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }
}

impl fmt::Debug for XrayClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("XrayClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for XrayClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            XrayClientErrorCode::InvalidConfiguration => {
                "the Xray Cloud client configuration is invalid"
            }
            XrayClientErrorCode::InvalidInput => "the Xray Cloud request is invalid",
            XrayClientErrorCode::Authentication => "Xray Cloud authentication failed",
            XrayClientErrorCode::Authorization => "Xray Cloud authorization failed",
            XrayClientErrorCode::NotFound => "the Xray Cloud resource was not found",
            XrayClientErrorCode::RateLimited => "Xray Cloud rate limited the request",
            XrayClientErrorCode::Timeout => "the Xray Cloud request timed out",
            XrayClientErrorCode::DependencyUnavailable => "Xray Cloud is unavailable",
            XrayClientErrorCode::InvalidResponse => "Xray Cloud returned an invalid response",
            XrayClientErrorCode::ResourceExhausted => {
                "the Xray Cloud request or response exceeds its approved limit"
            }
            XrayClientErrorCode::UnknownOutcome => {
                "the Xray Cloud effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for XrayClientError {}

/// One bounded HTTP exchange: the status plus the JSON body, if any.
pub(in crate::toolkits) struct XrayHttpResponse {
    status: StatusCode,
    body: Option<Value>,
}

impl XrayHttpResponse {
    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(status: StatusCode, body: Option<Value>) -> Self {
        Self { status, body }
    }
}

/// The only network seam: production uses a redirect-free HTTPS reqwest
/// client, tests a recording fixture.
#[async_trait]
pub(in crate::toolkits) trait XrayTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<XrayHttpResponse, XrayClientError>;
}

struct ReqwestXrayTransport {
    http: reqwest::Client,
}

#[async_trait]
impl XrayTransport for ReqwestXrayTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<XrayHttpResponse, XrayClientError> {
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
            return Err(response_too_large(effect));
        }
        let mut bytes = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|source| map_reqwest_error(&source, effect))?
        {
            if bytes.len().saturating_add(chunk.len()) > MAX_RESPONSE_BYTES {
                return Err(response_too_large(effect));
            }
            bytes.extend_from_slice(&chunk);
        }
        let body = if bytes.is_empty() {
            None
        } else {
            serde_json::from_slice(&bytes).ok()
        };
        Ok(XrayHttpResponse {
            status: response.status(),
            body,
        })
    }
}

/// Xray Cloud's GraphQL API, after client-credential authentication.
#[async_trait]
pub(in crate::toolkits) trait XrayApi: Send + Sync {
    /// `python_graphql_client.GraphqlClient.execute`: POST `{query,
    /// variables?}` and return the decoded response document, `errors`
    /// included.
    async fn graphql(
        &self,
        query: &str,
        variables: Option<Value>,
        effect: bool,
    ) -> Result<Map<String, Value>, XrayClientError>;
}

/// Invocation-scoped Xray Cloud client; the token is fetched on first use
/// and kept for the invocation, as the SDK keeps it for its wrapper.
pub(in crate::toolkits) struct XrayClient {
    config: XrayToolkitConfig,
    transport: Arc<dyn XrayTransport>,
    token: Mutex<Option<Zeroizing<String>>>,
}

impl XrayClient {
    pub(in crate::toolkits) fn new(config: XrayToolkitConfig) -> Result<Self, XrayClientError> {
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
            Arc::new(ReqwestXrayTransport { http }),
        ))
    }

    pub(in crate::toolkits) fn with_transport(
        config: XrayToolkitConfig,
        transport: Arc<dyn XrayTransport>,
    ) -> Self {
        Self {
            config,
            transport,
            token: Mutex::new(None),
        }
    }

    fn url(&self, path: &str) -> Result<Url, XrayClientError> {
        Url::parse(&format!("{}{path}", self.config.base_url()))
            .map_err(|_| invalid_configuration())
    }

    fn json_request(
        &self,
        path: &str,
        body: &Value,
        bearer: Option<&str>,
    ) -> Result<Request, XrayClientError> {
        let encoded = serde_json::to_vec(body).map_err(|_| invalid_input())?;
        if encoded.len() > MAX_REQUEST_BYTES {
            return Err(XrayClientError {
                code: XrayClientErrorCode::ResourceExhausted,
                retryable: false,
            });
        }
        let mut request = Request::new(Method::POST, self.url(path)?);
        let headers = request.headers_mut();
        headers.insert(ACCEPT, HeaderValue::from_static(JSON_CONTENT_TYPE));
        headers.insert(CONTENT_TYPE, HeaderValue::from_static(JSON_CONTENT_TYPE));
        headers.insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
        if let Some(token) = bearer {
            let header = Zeroizing::new(format!("Bearer {token}"));
            let mut authorization = HeaderValue::from_str(&header).map_err(|_| authentication())?;
            authorization.set_sensitive(true);
            headers.insert(AUTHORIZATION, authorization);
        }
        *request.body_mut() = Some(encoded.into());
        Ok(request)
    }

    /// `POST {base}/api/v1/authenticate` with the client credentials; the
    /// response body is the token as a JSON string.
    async fn token(&self) -> Result<Zeroizing<String>, XrayClientError> {
        let mut cached = self.token.lock().await;
        if let Some(token) = cached.as_ref() {
            return Ok(token.clone());
        }
        let credentials = json!({
            "client_id": self.config.client_id(),
            "client_secret": self.config.client_secret(),
        });
        let response = self
            .transport
            .execute(
                self.json_request("/api/v1/authenticate", &credentials, None)?,
                false,
            )
            .await?;
        if !response.status.is_success() {
            return Err(match response.status {
                StatusCode::BAD_REQUEST | StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN => {
                    authentication()
                }
                status => status_error(status, false),
            });
        }
        let token = response
            .body
            .as_ref()
            .and_then(Value::as_str)
            .filter(|token| {
                !token.is_empty()
                    && token.len() <= MAX_TOKEN_BYTES
                    && token.bytes().all(|byte| (0x21..=0x7e).contains(&byte))
            })
            .ok_or_else(invalid_response)?;
        let token = Zeroizing::new(token.to_owned());
        *cached = Some(token.clone());
        Ok(token)
    }
}

#[async_trait]
impl XrayApi for XrayClient {
    async fn graphql(
        &self,
        query: &str,
        variables: Option<Value>,
        effect: bool,
    ) -> Result<Map<String, Value>, XrayClientError> {
        let token = self.token().await?;
        let mut body = Map::new();
        body.insert("query".to_owned(), Value::String(query.to_owned()));
        // `if variables:` — an empty or absent mapping is not sent.
        if let Some(variables) = variables.filter(|variables| {
            variables
                .as_object()
                .is_none_or(|variables| !variables.is_empty())
        }) {
            body.insert("variables".to_owned(), variables);
        }
        let request = self.json_request("/api/v2/graphql", &Value::Object(body), Some(&token))?;
        let response = self.transport.execute(request, effect).await?;
        if !response.status.is_success() {
            return Err(status_error(response.status, effect));
        }
        match response.body {
            Some(Value::Object(document)) => Ok(document),
            _ if effect => Err(unknown_outcome()),
            _ => Err(invalid_response()),
        }
    }
}

fn status_error(status: StatusCode, effect: bool) -> XrayClientError {
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
            XrayClientErrorCode::InvalidInput
        }
        StatusCode::UNAUTHORIZED => XrayClientErrorCode::Authentication,
        StatusCode::FORBIDDEN => XrayClientErrorCode::Authorization,
        StatusCode::NOT_FOUND => XrayClientErrorCode::NotFound,
        StatusCode::REQUEST_TIMEOUT => XrayClientErrorCode::Timeout,
        StatusCode::TOO_MANY_REQUESTS => XrayClientErrorCode::RateLimited,
        status if status.is_server_error() => XrayClientErrorCode::DependencyUnavailable,
        _ => XrayClientErrorCode::InvalidResponse,
    };
    XrayClientError {
        code,
        retryable: !effect
            && matches!(
                code,
                XrayClientErrorCode::Timeout
                    | XrayClientErrorCode::RateLimited
                    | XrayClientErrorCode::DependencyUnavailable
            ),
    }
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> XrayClientError {
    if effect {
        return unknown_outcome();
    }
    XrayClientError {
        code: if source.is_timeout() {
            XrayClientErrorCode::Timeout
        } else {
            XrayClientErrorCode::DependencyUnavailable
        },
        retryable: true,
    }
}

const fn response_too_large(effect: bool) -> XrayClientError {
    if effect {
        unknown_outcome()
    } else {
        XrayClientError {
            code: XrayClientErrorCode::ResourceExhausted,
            retryable: false,
        }
    }
}

const fn authentication() -> XrayClientError {
    XrayClientError {
        code: XrayClientErrorCode::Authentication,
        retryable: false,
    }
}

const fn invalid_configuration() -> XrayClientError {
    XrayClientError {
        code: XrayClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

const fn invalid_input() -> XrayClientError {
    XrayClientError {
        code: XrayClientErrorCode::InvalidInput,
        retryable: false,
    }
}

pub(super) const fn invalid_response() -> XrayClientError {
    XrayClientError {
        code: XrayClientErrorCode::InvalidResponse,
        retryable: false,
    }
}

pub(super) const fn resource_exhausted() -> XrayClientError {
    XrayClientError {
        code: XrayClientErrorCode::ResourceExhausted,
        retryable: false,
    }
}

const fn unknown_outcome() -> XrayClientError {
    XrayClientError {
        code: XrayClientErrorCode::UnknownOutcome,
        retryable: false,
    }
}
