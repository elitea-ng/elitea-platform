use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderName, HeaderValue, USER_AGENT,
};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::Value;
use zeroize::Zeroizing;

use super::config::CarrierToolkitConfig;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_REQUEST_BYTES: usize = 256 * 1_024;
const MAX_RESPONSE_BYTES: usize = 4 * 1_024 * 1_024;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";
const JSON_CONTENT_TYPE: &str = "application/json";
const FORM_CONTENT_TYPE: &str = "application/x-www-form-urlencoded";
const X_ORGANIZATION: HeaderName = HeaderName::from_static("x-organization");

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum CarrierClientErrorCode {
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

/// Stable Carrier failure without provider bodies, URLs, tokens or payloads.
pub(crate) struct CarrierClientError {
    code: CarrierClientErrorCode,
    retryable: bool,
}

impl CarrierClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> CarrierClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            CarrierClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "carrier.configuration.invalid",
                "the Carrier toolkit configuration is invalid",
            ),
            CarrierClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "carrier.request.invalid",
                "the Carrier request is invalid",
            ),
            CarrierClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "carrier.authentication.failed",
                "Carrier authentication failed",
            ),
            CarrierClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "carrier.authorization.failed",
                "Carrier did not authorize the request",
            ),
            CarrierClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "carrier.resource.not_found",
                "the requested Carrier resource was not found",
            ),
            CarrierClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "carrier.rate_limited",
                "Carrier rate limited the request",
            ),
            CarrierClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "carrier.timeout",
                "the Carrier request timed out",
            ),
            CarrierClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "carrier.unavailable",
                "Carrier is unavailable",
            ),
            CarrierClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "carrier.response.invalid",
                "Carrier returned an invalid response",
            ),
            CarrierClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "carrier.response.resource_exhausted",
                "the Carrier response exceeds the approved limit",
            ),
            CarrierClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "carrier.effect.unknown_outcome",
                "Carrier may have applied the requested effect; reconcile it before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(
        code: CarrierClientErrorCode,
        retryable: bool,
    ) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for CarrierClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("CarrierClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for CarrierClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            CarrierClientErrorCode::InvalidConfiguration => {
                "the Carrier client configuration is invalid"
            }
            CarrierClientErrorCode::InvalidInput => "the Carrier request is invalid",
            CarrierClientErrorCode::Authentication => "Carrier authentication failed",
            CarrierClientErrorCode::Authorization => "Carrier authorization failed",
            CarrierClientErrorCode::NotFound => "the Carrier resource was not found",
            CarrierClientErrorCode::RateLimited => "Carrier rate limited the request",
            CarrierClientErrorCode::Timeout => "the Carrier request timed out",
            CarrierClientErrorCode::DependencyUnavailable => "Carrier is unavailable",
            CarrierClientErrorCode::InvalidResponse => "Carrier returned an invalid response",
            CarrierClientErrorCode::ResourceExhausted => {
                "the Carrier response exceeds its approved limit"
            }
            CarrierClientErrorCode::UnknownOutcome => {
                "the Carrier effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for CarrierClientError {}

/// One bounded Carrier HTTP answer: the status and the raw body, which the
/// caller interprets the way the SDK call site it ports does (most raise on a
/// non-2xx status, two read the body whatever the status).
pub(in crate::toolkits) struct CarrierHttpResponse {
    status: StatusCode,
    body: Vec<u8>,
}

impl CarrierHttpResponse {
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

    pub(super) const fn status(&self) -> StatusCode {
        self.status
    }

    /// The body as text, lossily: the SDK reads `response.text`.
    pub(super) fn text(&self) -> String {
        String::from_utf8_lossy(&self.body).into_owned()
    }

    /// The body as JSON, or `None` when it is empty or not JSON (the SDK's
    /// `response.json()` raising).
    pub(super) fn json(&self) -> Option<Value> {
        if self.body.is_empty() {
            return None;
        }
        serde_json::from_slice(&self.body).ok()
    }
}

#[async_trait]
pub(in crate::toolkits) trait CarrierTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<CarrierHttpResponse, CarrierClientError>;
}

struct ReqwestCarrierTransport {
    http: reqwest::Client,
}

#[async_trait]
impl CarrierTransport for ReqwestCarrierTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<CarrierHttpResponse, CarrierClientError> {
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
        Ok(CarrierHttpResponse {
            status: response.status(),
            body,
        })
    }
}

/// Which of the SDK's two header sets a call carries.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum CarrierAuth {
    /// `CarrierClient.session`: `Authorization: Bearer`, `X-Organization`.
    Session,
    /// The SDK's bare `requests.post` calls (`add_tag_to_report`,
    /// `create_test`): lower-case `bearer`, no organization header.
    Bare,
}

/// A request body in the encoding the SDK call site sends.
pub(super) enum CarrierBody<'a> {
    Empty,
    Json(&'a Value),
    /// `requests.post(data={...})`: URL-encoded form fields.
    Form(&'a [(&'a str, &'a str)]),
}

/// One invocation-scoped Carrier REST authority.
pub(crate) struct CarrierClient {
    config: CarrierToolkitConfig,
    transport: Arc<dyn CarrierTransport>,
}

impl CarrierClient {
    pub(crate) fn new(config: CarrierToolkitConfig) -> Result<Self, CarrierClientError> {
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
            transport: Arc::new(ReqwestCarrierTransport { http }),
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn with_transport(
        config: CarrierToolkitConfig,
        transport: Arc<dyn CarrierTransport>,
    ) -> Self {
        Self { config, transport }
    }

    pub(super) fn project_id(&self) -> &str {
        self.config.project_id()
    }

    pub(super) fn display_url(&self) -> &str {
        self.config.display_url()
    }

    /// `{url}/api/v1/<segments...>` with each segment percent-encoded, so a
    /// model-supplied id can never add a path segment or a query.
    fn endpoint(
        &self,
        segments: &[&str],
        query: &[(&str, &str)],
    ) -> Result<Url, CarrierClientError> {
        let mut url = self.config.base().clone();
        {
            let mut path = url
                .path_segments_mut()
                .map_err(|()| invalid_configuration())?;
            path.pop_if_empty();
            path.extend(["api", "v1"]);
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
        if !url.as_str().starts_with(self.config.base().as_str()) {
            return Err(invalid_input());
        }
        Ok(url)
    }

    /// Send one request and return the bounded answer whatever its status.
    pub(super) async fn send(
        &self,
        method: Method,
        segments: &[&str],
        query: &[(&str, &str)],
        body: CarrierBody<'_>,
        auth: CarrierAuth,
        effect: bool,
    ) -> Result<CarrierHttpResponse, CarrierClientError> {
        let url = self.endpoint(segments, query)?;
        let mut request = Request::new(method, url);
        let headers = request.headers_mut();
        headers.insert(
            ACCEPT,
            HeaderValue::from_static("application/json, text/plain, */*"),
        );
        headers.insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
        let scheme = match auth {
            CarrierAuth::Session => "Bearer ",
            CarrierAuth::Bare => "bearer ",
        };
        let mut authorization = Zeroizing::new(String::with_capacity(
            scheme.len().saturating_add(self.config.token().len()),
        ));
        authorization.push_str(scheme);
        authorization.push_str(self.config.token());
        let mut authorization =
            HeaderValue::from_str(&authorization).map_err(|_| invalid_configuration())?;
        authorization.set_sensitive(true);
        headers.insert(AUTHORIZATION, authorization);
        if auth == CarrierAuth::Session {
            headers.insert(
                X_ORGANIZATION,
                HeaderValue::from_str(self.config.organization())
                    .map_err(|_| invalid_configuration())?,
            );
        }
        let encoded = match body {
            CarrierBody::Empty => None,
            CarrierBody::Json(value) => {
                headers.insert(CONTENT_TYPE, HeaderValue::from_static(JSON_CONTENT_TYPE));
                Some(serde_json::to_vec(value).map_err(|_| invalid_input())?)
            }
            CarrierBody::Form(fields) => {
                headers.insert(CONTENT_TYPE, HeaderValue::from_static(FORM_CONTENT_TYPE));
                // `query_pairs_mut` is the WHATWG form-urlencoded serializer,
                // the encoding `requests` uses for `data={...}`.
                let mut form = self.config.base().clone();
                form.set_query(None);
                form.query_pairs_mut().extend_pairs(fields.iter().copied());
                Some(form.query().unwrap_or_default().as_bytes().to_vec())
            }
        };
        if let Some(encoded) = encoded {
            if encoded.len() > MAX_REQUEST_BYTES {
                return Err(CarrierClientError {
                    code: CarrierClientErrorCode::ResourceExhausted,
                    retryable: false,
                });
            }
            *request.body_mut() = Some(encoded.into());
        }
        self.transport.execute(request, effect).await
    }

    /// The SDK's `CarrierClient.request`: session headers, a raised non-2xx
    /// status and a body that must be JSON.
    pub(super) async fn request_json(
        &self,
        method: Method,
        segments: &[&str],
        query: &[(&str, &str)],
        body: Option<&Value>,
        effect: bool,
    ) -> Result<Value, CarrierClientError> {
        let body = body.map_or(CarrierBody::Empty, CarrierBody::Json);
        let response = self
            .send(method, segments, query, body, CarrierAuth::Session, effect)
            .await?;
        json_success(&response, effect)
    }

    /// GET a session endpoint whose JSON object carries a `rows` list
    /// (`.get("rows", [])`).
    pub(super) async fn rows(
        &self,
        segments: &[&str],
        query: &[(&str, &str)],
    ) -> Result<Vec<Value>, CarrierClientError> {
        let document = self
            .request_json(Method::GET, segments, query, None, false)
            .await?;
        list_field(&document, "rows")
    }
}

/// A raised non-2xx status, then a body that must be JSON.
pub(super) fn json_success(
    response: &CarrierHttpResponse,
    effect: bool,
) -> Result<Value, CarrierClientError> {
    if !response.status().is_success() {
        return Err(status_error(response.status(), effect));
    }
    response
        .json()
        .ok_or_else(|| response_shape_failure(effect))
}

/// `document.get(name, [])` on a JSON object; any other shape is invalid.
pub(super) fn list_field(document: &Value, name: &str) -> Result<Vec<Value>, CarrierClientError> {
    let object = document.as_object().ok_or_else(invalid_response)?;
    match object.get(name) {
        None => Ok(Vec::new()),
        Some(Value::Array(values)) => Ok(values.clone()),
        Some(_) => Err(invalid_response()),
    }
}

pub(super) fn status_error(status: StatusCode, effect: bool) -> CarrierClientError {
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
            CarrierClientErrorCode::InvalidInput
        }
        StatusCode::UNAUTHORIZED => CarrierClientErrorCode::Authentication,
        StatusCode::FORBIDDEN => CarrierClientErrorCode::Authorization,
        StatusCode::NOT_FOUND => CarrierClientErrorCode::NotFound,
        StatusCode::REQUEST_TIMEOUT => CarrierClientErrorCode::Timeout,
        StatusCode::TOO_MANY_REQUESTS => CarrierClientErrorCode::RateLimited,
        status if status.is_server_error() => CarrierClientErrorCode::DependencyUnavailable,
        _ => CarrierClientErrorCode::InvalidResponse,
    };
    CarrierClientError {
        code,
        retryable: !effect
            && matches!(
                code,
                CarrierClientErrorCode::Timeout
                    | CarrierClientErrorCode::RateLimited
                    | CarrierClientErrorCode::DependencyUnavailable
            ),
    }
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> CarrierClientError {
    if effect {
        return unknown_outcome();
    }
    CarrierClientError {
        code: if source.is_timeout() {
            CarrierClientErrorCode::Timeout
        } else {
            CarrierClientErrorCode::DependencyUnavailable
        },
        retryable: true,
    }
}

const fn response_bound_failure(effect: bool) -> CarrierClientError {
    if effect {
        unknown_outcome()
    } else {
        CarrierClientError {
            code: CarrierClientErrorCode::ResourceExhausted,
            retryable: false,
        }
    }
}

pub(super) const fn response_shape_failure(effect: bool) -> CarrierClientError {
    if effect {
        unknown_outcome()
    } else {
        invalid_response()
    }
}

const fn invalid_configuration() -> CarrierClientError {
    CarrierClientError {
        code: CarrierClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

pub(super) const fn invalid_input() -> CarrierClientError {
    CarrierClientError {
        code: CarrierClientErrorCode::InvalidInput,
        retryable: false,
    }
}

pub(super) const fn invalid_response() -> CarrierClientError {
    CarrierClientError {
        code: CarrierClientErrorCode::InvalidResponse,
        retryable: false,
    }
}

const fn unknown_outcome() -> CarrierClientError {
    CarrierClientError {
        code: CarrierClientErrorCode::UnknownOutcome,
        retryable: false,
    }
}
