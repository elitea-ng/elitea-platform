use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use reqwest::header::{ACCEPT, CONTENT_LENGTH, CONTENT_TYPE, HeaderName, HeaderValue, USER_AGENT};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::Value;

use super::config::FigmaToolkitConfig;

/// `FigmaPy`'s fixed `api_uri`.
const API_ROOT: &str = "https://api.figma.com/v1/";
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_mins(1);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_REQUEST_BYTES: usize = 64 * 1_024;
/// A whole Figma file document can be large; the SDK reads it unbounded.
const MAX_RESPONSE_BYTES: usize = 16 * 1_024 * 1_024;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";
const JSON_CONTENT_TYPE: &str = "application/json";
const X_FIGMA_TOKEN: HeaderName = HeaderName::from_static("x-figma-token");

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum FigmaClientErrorCode {
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

/// Stable Figma failure without provider bodies, URLs, tokens or payloads.
pub(crate) struct FigmaClientError {
    code: FigmaClientErrorCode,
    retryable: bool,
}

impl FigmaClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> FigmaClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            FigmaClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "figma.configuration.invalid",
                "the Figma toolkit configuration is invalid",
            ),
            FigmaClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "figma.request.invalid",
                "the Figma request is invalid",
            ),
            FigmaClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "figma.authentication.failed",
                "Figma authentication failed",
            ),
            FigmaClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "figma.authorization.failed",
                "Figma did not authorize the request",
            ),
            FigmaClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "figma.resource.not_found",
                "the requested Figma resource was not found",
            ),
            FigmaClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "figma.rate_limited",
                "Figma rate limited the request",
            ),
            FigmaClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "figma.timeout",
                "the Figma request timed out",
            ),
            FigmaClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "figma.unavailable",
                "Figma is unavailable",
            ),
            FigmaClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "figma.response.invalid",
                "Figma returned an invalid response",
            ),
            FigmaClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "figma.response.resource_exhausted",
                "the Figma response exceeds the approved limit",
            ),
            FigmaClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "figma.effect.unknown_outcome",
                "Figma may have applied the requested effect; reconcile it before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(code: FigmaClientErrorCode, retryable: bool) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for FigmaClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("FigmaClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for FigmaClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            FigmaClientErrorCode::InvalidConfiguration => {
                "the Figma client configuration is invalid"
            }
            FigmaClientErrorCode::InvalidInput => "the Figma request is invalid",
            FigmaClientErrorCode::Authentication => "Figma authentication failed",
            FigmaClientErrorCode::Authorization => "Figma authorization failed",
            FigmaClientErrorCode::NotFound => "the Figma resource was not found",
            FigmaClientErrorCode::RateLimited => "Figma rate limited the request",
            FigmaClientErrorCode::Timeout => "the Figma request timed out",
            FigmaClientErrorCode::DependencyUnavailable => "Figma is unavailable",
            FigmaClientErrorCode::InvalidResponse => "Figma returned an invalid response",
            FigmaClientErrorCode::ResourceExhausted => {
                "the Figma response exceeds its approved limit"
            }
            FigmaClientErrorCode::UnknownOutcome => {
                "the Figma effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for FigmaClientError {}

/// One bounded Figma HTTP answer.
pub(in crate::toolkits) struct FigmaHttpResponse {
    status: StatusCode,
    body: Vec<u8>,
}

impl FigmaHttpResponse {
    #[cfg(test)]
    pub(in crate::toolkits) fn fixture(status: StatusCode, body: &Value) -> Self {
        Self {
            status,
            body: serde_json::to_vec(body).unwrap_or_default(),
        }
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn fixture_text(status: StatusCode, body: &str) -> Self {
        Self {
            status,
            body: body.as_bytes().to_vec(),
        }
    }
}

#[async_trait]
pub(in crate::toolkits) trait FigmaTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<FigmaHttpResponse, FigmaClientError>;
}

struct ReqwestFigmaTransport {
    http: reqwest::Client,
}

#[async_trait]
impl FigmaTransport for ReqwestFigmaTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<FigmaHttpResponse, FigmaClientError> {
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
            return Err(response_bound_failure(effect));
        }
        let mut body = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|source| map_reqwest_error(&source, effect))?
        {
            let next = body
                .len()
                .checked_add(chunk.len())
                .ok_or_else(|| response_bound_failure(effect))?;
            if next > MAX_RESPONSE_BYTES {
                return Err(response_bound_failure(effect));
            }
            body.extend_from_slice(&chunk);
        }
        Ok(FigmaHttpResponse {
            status: response.status(),
            body,
        })
    }
}

/// One invocation-scoped Figma REST authority over the fixed API origin.
pub(crate) struct FigmaClient {
    config: FigmaToolkitConfig,
    transport: Arc<dyn FigmaTransport>,
}

impl FigmaClient {
    pub(crate) fn new(config: FigmaToolkitConfig) -> Result<Self, FigmaClientError> {
        let http = reqwest::Client::builder()
            .https_only(true)
            .redirect(reqwest::redirect::Policy::none())
            .retry(reqwest::retry::never())
            .connect_timeout(CONNECT_TIMEOUT)
            .timeout(REQUEST_TIMEOUT)
            .pool_idle_timeout(POOL_IDLE_TIMEOUT)
            .pool_max_idle_per_host(MAX_IDLE_PER_HOST)
            .user_agent(USER_AGENT_VALUE)
            .build()
            .map_err(|_| invalid_configuration())?;
        Ok(Self {
            config,
            transport: Arc::new(ReqwestFigmaTransport { http }),
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn with_transport(
        config: FigmaToolkitConfig,
        transport: Arc<dyn FigmaTransport>,
    ) -> Self {
        Self { config, transport }
    }

    pub(super) fn config(&self) -> &FigmaToolkitConfig {
        &self.config
    }

    /// `https://api.figma.com/v1/<segments...>?<query>` with each segment
    /// percent-encoded, so a model-supplied key never adds a path segment.
    fn endpoint(segments: &[&str], query: &[(&str, &str)]) -> Result<Url, FigmaClientError> {
        let mut url = Url::parse(API_ROOT).map_err(|_| invalid_configuration())?;
        {
            let mut path = url
                .path_segments_mut()
                .map_err(|()| invalid_configuration())?;
            path.pop_if_empty();
            for segment in segments {
                if segment.is_empty() || matches!(*segment, "." | "..") {
                    return Err(invalid_input());
                }
                path.push(segment);
            }
        }
        if !query.is_empty() {
            url.query_pairs_mut().extend_pairs(query.iter().copied());
        }
        Ok(url)
    }

    /// `FigmaPy`'s `api_request` as the SDK overrides it: a 2xx JSON body, or
    /// `{"raw": text}` when a 2xx body is not JSON; any other status fails.
    pub(super) async fn request(
        &self,
        method: Method,
        segments: &[&str],
        query: &[(&str, &str)],
        body: Option<&Value>,
        effect: bool,
    ) -> Result<Value, FigmaClientError> {
        let mut request = Request::new(method, Self::endpoint(segments, query)?);
        let headers = request.headers_mut();
        let mut token =
            HeaderValue::from_str(self.config.token()).map_err(|_| invalid_configuration())?;
        token.set_sensitive(true);
        headers.insert(X_FIGMA_TOKEN, token);
        headers.insert(ACCEPT, HeaderValue::from_static(JSON_CONTENT_TYPE));
        headers.insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
        headers.insert(CONTENT_TYPE, HeaderValue::from_static(JSON_CONTENT_TYPE));
        if let Some(body) = body {
            let encoded = serde_json::to_vec(body).map_err(|_| invalid_input())?;
            if encoded.len() > MAX_REQUEST_BYTES {
                return Err(FigmaClientError {
                    code: FigmaClientErrorCode::ResourceExhausted,
                    retryable: false,
                });
            }
            *request.body_mut() = Some(encoded.into());
        }
        let response = self.transport.execute(request, effect).await?;
        if !response.status.is_success() {
            return Err(status_error(response.status, effect));
        }
        Ok(serde_json::from_slice(&response.body).unwrap_or_else(|_| {
            let mut raw = serde_json::Map::new();
            raw.insert(
                "raw".to_owned(),
                Value::String(String::from_utf8_lossy(&response.body).into_owned()),
            );
            Value::Object(raw)
        }))
    }
}

pub(super) fn status_error(status: StatusCode, effect: bool) -> FigmaClientError {
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
        StatusCode::BAD_REQUEST | StatusCode::UNPROCESSABLE_ENTITY | StatusCode::CONFLICT => {
            FigmaClientErrorCode::InvalidInput
        }
        StatusCode::UNAUTHORIZED => FigmaClientErrorCode::Authentication,
        StatusCode::FORBIDDEN => FigmaClientErrorCode::Authorization,
        StatusCode::NOT_FOUND => FigmaClientErrorCode::NotFound,
        StatusCode::REQUEST_TIMEOUT => FigmaClientErrorCode::Timeout,
        StatusCode::TOO_MANY_REQUESTS => FigmaClientErrorCode::RateLimited,
        status if status.is_server_error() => FigmaClientErrorCode::DependencyUnavailable,
        _ => FigmaClientErrorCode::InvalidResponse,
    };
    FigmaClientError {
        code,
        retryable: !effect
            && matches!(
                code,
                FigmaClientErrorCode::Timeout
                    | FigmaClientErrorCode::RateLimited
                    | FigmaClientErrorCode::DependencyUnavailable
            ),
    }
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> FigmaClientError {
    if effect {
        return unknown_outcome();
    }
    FigmaClientError {
        code: if source.is_timeout() {
            FigmaClientErrorCode::Timeout
        } else {
            FigmaClientErrorCode::DependencyUnavailable
        },
        retryable: true,
    }
}

const fn response_bound_failure(effect: bool) -> FigmaClientError {
    if effect {
        unknown_outcome()
    } else {
        FigmaClientError {
            code: FigmaClientErrorCode::ResourceExhausted,
            retryable: false,
        }
    }
}

pub(super) const fn response_shape_failure(effect: bool) -> FigmaClientError {
    if effect {
        unknown_outcome()
    } else {
        invalid_response()
    }
}

const fn invalid_configuration() -> FigmaClientError {
    FigmaClientError {
        code: FigmaClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

pub(super) const fn invalid_input() -> FigmaClientError {
    FigmaClientError {
        code: FigmaClientErrorCode::InvalidInput,
        retryable: false,
    }
}

pub(super) const fn invalid_response() -> FigmaClientError {
    FigmaClientError {
        code: FigmaClientErrorCode::InvalidResponse,
        retryable: false,
    }
}

const fn unknown_outcome() -> FigmaClientError {
    FigmaClientError {
        code: FigmaClientErrorCode::UnknownOutcome,
        retryable: false,
    }
}
