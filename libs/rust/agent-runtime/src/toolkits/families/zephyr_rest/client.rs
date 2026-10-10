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

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_REQUEST_BYTES: usize = 256 * 1_024;
const MAX_RESPONSE_BYTES: usize = 4 * 1_024 * 1_024;
const MAX_SEGMENT_BYTES: usize = 1_024;
/// The ceiling on one tool's model-visible text.
pub(in crate::toolkits) const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const JSON_CONTENT_TYPE: &str = "application/json";
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ZephyrRestErrorCode {
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

/// The error-code namespace and product label of one Zephyr REST family.
pub(in crate::toolkits) struct ZephyrRestFamily {
    pub(in crate::toolkits) label: &'static str,
    pub(in crate::toolkits) code: fn(ZephyrRestErrorCode) -> &'static str,
}

/// Stable failure without origin, credentials, identifiers, request bodies or
/// provider diagnostics.
pub(crate) struct ZephyrRestError {
    code: ZephyrRestErrorCode,
    retryable: bool,
}

impl ZephyrRestError {
    #[must_use]
    pub(crate) const fn code(&self) -> ZephyrRestErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    /// A failure after at least one confirmed remote effect: whatever the
    /// cause, the caller must reconcile before retrying.
    #[must_use]
    pub(crate) const fn after_confirmed_effect() -> Self {
        unknown_outcome()
    }

    pub(crate) fn into_adk(self, family: &ZephyrRestFamily) -> AdkError {
        let label = family.label;
        let (category, message) = match self.code {
            ZephyrRestErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                format!("the {label} toolkit configuration is invalid"),
            ),
            ZephyrRestErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                format!("the {label} request is invalid"),
            ),
            ZephyrRestErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                format!("{label} authentication failed: check the API token"),
            ),
            ZephyrRestErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                format!("{label} did not authorize the request"),
            ),
            ZephyrRestErrorCode::NotFound => (
                ErrorCategory::NotFound,
                format!("the requested {label} resource was not found"),
            ),
            ZephyrRestErrorCode::Conflict => (
                ErrorCategory::InvalidInput,
                format!("the {label} request conflicts with provider state"),
            ),
            ZephyrRestErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                format!("{label} rate limited the request"),
            ),
            ZephyrRestErrorCode::Timeout => (
                ErrorCategory::Timeout,
                format!("the {label} request timed out"),
            ),
            ZephyrRestErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                format!("{label} is unavailable"),
            ),
            ZephyrRestErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                format!("{label} returned an invalid response"),
            ),
            ZephyrRestErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                format!("the {label} response exceeds the approved limit"),
            ),
            ZephyrRestErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                format!(
                    "{label} may have applied the change; reconcile the current state before retrying"
                ),
            ),
        };
        AdkError::new(
            ErrorComponent::Tool,
            category,
            (family.code)(self.code),
            message,
        )
        .with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(code: ZephyrRestErrorCode, retryable: bool) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for ZephyrRestError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ZephyrRestError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ZephyrRestError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ZephyrRestErrorCode::InvalidConfiguration => {
                "the Zephyr client configuration is invalid"
            }
            ZephyrRestErrorCode::InvalidInput => "the Zephyr request is invalid",
            ZephyrRestErrorCode::Authentication => "Zephyr authentication failed",
            ZephyrRestErrorCode::Authorization => "Zephyr authorization failed",
            ZephyrRestErrorCode::NotFound => "the Zephyr resource was not found",
            ZephyrRestErrorCode::Conflict => "the Zephyr request conflicts",
            ZephyrRestErrorCode::RateLimited => "Zephyr rate limited the request",
            ZephyrRestErrorCode::Timeout => "the Zephyr request timed out",
            ZephyrRestErrorCode::DependencyUnavailable => "Zephyr is unavailable",
            ZephyrRestErrorCode::InvalidResponse => "Zephyr returned an invalid response",
            ZephyrRestErrorCode::ResourceExhausted => {
                "the Zephyr response exceeds its approved limit"
            }
            ZephyrRestErrorCode::UnknownOutcome => {
                "the Zephyr effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for ZephyrRestError {}

/// A successful provider answer, decoded the way the SDK clients decode it:
/// JSON when the body is JSON, text otherwise, and nothing for an empty body.
#[derive(Clone, Debug, PartialEq)]
pub(in crate::toolkits) enum ZephyrReply {
    Json(Value),
    Text(String),
    Empty,
}

impl ZephyrReply {
    /// The reply as the SDK hands it to the model: the JSON value, the text,
    /// or the empty string.
    #[must_use]
    pub(in crate::toolkits) fn into_value(self) -> Value {
        match self {
            Self::Json(value) => value,
            Self::Text(text) => Value::String(text),
            Self::Empty => Value::String(String::new()),
        }
    }

    #[must_use]
    pub(in crate::toolkits) const fn json(&self) -> Option<&Value> {
        match self {
            Self::Json(value) => Some(value),
            Self::Text(_) | Self::Empty => None,
        }
    }
}

pub(in crate::toolkits) struct ZephyrRestResponse {
    status: StatusCode,
    content_type: Option<Box<str>>,
    body: Vec<u8>,
}

impl ZephyrRestResponse {
    #[cfg(test)]
    pub(in crate::toolkits) fn fixture(
        status: StatusCode,
        content_type: Option<&str>,
        body: &[u8],
    ) -> Self {
        Self {
            status,
            content_type: content_type.map(Into::into),
            body: body.to_vec(),
        }
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn json(status: StatusCode, value: &Value) -> Self {
        Self::fixture(
            status,
            Some(JSON_CONTENT_TYPE),
            &serde_json::to_vec(value).unwrap_or_default(),
        )
    }
}

#[async_trait]
pub(in crate::toolkits) trait ZephyrRestTransport: Send + Sync {
    async fn execute(&self, request: Request) -> Result<ZephyrRestResponse, ZephyrRestError>;
}

struct ReqwestZephyrRestTransport {
    http: reqwest::Client,
}

#[async_trait]
impl ZephyrRestTransport for ReqwestZephyrRestTransport {
    async fn execute(&self, request: Request) -> Result<ZephyrRestResponse, ZephyrRestError> {
        let effect = request.method() != Method::GET;
        let mut response = self
            .http
            .execute(request)
            .await
            .map_err(|error| map_reqwest_error(&error, effect))?;
        let status = response.status();
        let content_type = response
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .map(Box::<str>::from);
        if response
            .content_length()
            .is_some_and(|length| length > u64::try_from(MAX_RESPONSE_BYTES).unwrap_or(u64::MAX))
        {
            return Err(resource_exhausted(effect));
        }
        let mut body = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|error| map_reqwest_error(&error, effect))?
        {
            let next = body
                .len()
                .checked_add(chunk.len())
                .ok_or_else(|| resource_exhausted(effect))?;
            if next > MAX_RESPONSE_BYTES {
                return Err(resource_exhausted(effect));
            }
            body.extend_from_slice(&chunk);
        }
        Ok(ZephyrRestResponse {
            status,
            content_type,
            body,
        })
    }
}

/// Invocation-owned bearer client for one exact Zephyr API base.
///
/// The base is fixed at construction; every request appends percent-encoded
/// path segments to it, so a tool argument can never change the origin or
/// climb out of the API prefix.
pub(crate) struct ZephyrRestClient {
    base: Url,
    token: Zeroizing<String>,
    transport: Arc<dyn ZephyrRestTransport>,
}

impl ZephyrRestClient {
    pub(crate) fn new(base: Url, token: Zeroizing<String>) -> Result<Self, ZephyrRestError> {
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
            base,
            token,
            transport: Arc::new(ReqwestZephyrRestTransport { http }),
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn with_transport(
        base: Url,
        token: &str,
        transport: Arc<dyn ZephyrRestTransport>,
    ) -> Self {
        Self {
            base,
            token: Zeroizing::new(token.to_owned()),
            transport,
        }
    }

    /// The configured base, for model-visible descriptions that name it.
    pub(in crate::toolkits) const fn base(&self) -> &Url {
        &self.base
    }

    fn request(
        &self,
        method: Method,
        segments: &[&str],
        query: &[(&str, String)],
        body: Option<&Value>,
    ) -> Result<Request, ZephyrRestError> {
        let mut url = self.base.clone();
        {
            let mut path = url
                .path_segments_mut()
                .map_err(|()| invalid_configuration())?;
            path.pop_if_empty();
            for (index, segment) in segments.iter().enumerate() {
                validate_segment(segment, index + 1 == segments.len())?;
                path.push(segment);
            }
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
            .insert(ACCEPT, HeaderValue::from_static(JSON_CONTENT_TYPE));
        request
            .headers_mut()
            .insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
        let mut authorization = Zeroizing::new(String::with_capacity(self.token.len() + 7));
        authorization.push_str("Bearer ");
        authorization.push_str(&self.token);
        let mut value =
            HeaderValue::from_str(&authorization).map_err(|_| invalid_configuration())?;
        value.set_sensitive(true);
        request.headers_mut().insert(AUTHORIZATION, value);
        if let Some(body) = body {
            let encoded = serde_json::to_vec(body).map_err(|_| invalid_input())?;
            if encoded.len() > MAX_REQUEST_BYTES {
                return Err(ZephyrRestError {
                    code: ZephyrRestErrorCode::InvalidInput,
                    retryable: false,
                });
            }
            request
                .headers_mut()
                .insert(CONTENT_TYPE, HeaderValue::from_static(JSON_CONTENT_TYPE));
            request.headers_mut().insert(
                CONTENT_LENGTH,
                HeaderValue::from_str(&encoded.len().to_string()).map_err(|_| invalid_input())?,
            );
            *request.body_mut() = Some(encoded.into());
        }
        Ok(request)
    }

    /// One request to `base/segments...?query`.
    ///
    /// Any method other than GET is an effect: once it is sent, a transport
    /// failure or an ambiguous status is an unknown outcome, never a retry.
    pub(in crate::toolkits) async fn call(
        &self,
        method: Method,
        segments: &[&str],
        query: &[(&str, String)],
        body: Option<&Value>,
    ) -> Result<ZephyrReply, ZephyrRestError> {
        let effect = method != Method::GET;
        self.call_with(method, segments, query, body, effect).await
    }

    /// A POST that only reads (a search): its failures are ordinary read
    /// failures, never an unknown remote effect.
    pub(in crate::toolkits) async fn post_read(
        &self,
        segments: &[&str],
        body: &Value,
    ) -> Result<ZephyrReply, ZephyrRestError> {
        self.call_with(Method::POST, segments, &[], Some(body), false)
            .await
    }

    async fn call_with(
        &self,
        method: Method,
        segments: &[&str],
        query: &[(&str, String)],
        body: Option<&Value>,
        effect: bool,
    ) -> Result<ZephyrReply, ZephyrRestError> {
        let request = self.request(method, segments, query, body)?;
        let response = match self.transport.execute(request).await {
            Ok(response) => response,
            // The transport classifies by method; a read-only POST that fails
            // in flight changed nothing and may be retried.
            Err(error) if !effect && error.code == ZephyrRestErrorCode::UnknownOutcome => {
                return Err(ZephyrRestError {
                    code: ZephyrRestErrorCode::DependencyUnavailable,
                    retryable: true,
                });
            }
            Err(error) => return Err(error),
        };
        map_http_status(response.status, effect)?;
        decode_reply(&response, effect)
    }

    /// A GET whose JSON body the caller needs to read.
    pub(in crate::toolkits) async fn get_json(
        &self,
        segments: &[&str],
        query: &[(&str, String)],
    ) -> Result<Value, ZephyrRestError> {
        match self.call(Method::GET, segments, query, None).await? {
            ZephyrReply::Json(value) => Ok(value),
            ZephyrReply::Text(_) | ZephyrReply::Empty => Err(invalid_response()),
        }
    }

    /// A GET whose body may legitimately be empty, as the Enterprise client
    /// reads it (`response.json() if response.content else None`).
    pub(in crate::toolkits) async fn get_optional_json(
        &self,
        segments: &[&str],
        query: &[(&str, String)],
    ) -> Result<Option<Value>, ZephyrRestError> {
        match self.call(Method::GET, segments, query, None).await? {
            ZephyrReply::Json(value) => Ok(Some(value)),
            ZephyrReply::Empty => Ok(None),
            ZephyrReply::Text(_) => Err(invalid_response()),
        }
    }

    /// Replace the transport's absolute next-page URL with a request on this
    /// base: only its query is used, so a provider-supplied `next` link can
    /// never redirect the bearer token to another origin.
    #[must_use]
    pub(in crate::toolkits) fn next_page_query(&self, next: &str) -> Option<Vec<(String, String)>> {
        let url = Url::parse(next).or_else(|_| self.base.join(next)).ok()?;
        Some(
            url.query_pairs()
                .filter(|(_, value)| !value.is_empty())
                .map(|(name, value)| (name.into_owned(), value.into_owned()))
                .collect(),
        )
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_request(
        &self,
        method: Method,
        segments: &[&str],
        query: &[(&str, String)],
        body: Option<&Value>,
    ) -> Result<Request, ZephyrRestError> {
        self.request(method, segments, query, body)
    }
}

/// A dynamic path segment from a tool argument.
///
/// It is percent-encoded when pushed, so `/`, `?` and `#` cannot change the
/// route; dot segments and control characters are refused outright.
fn validate_segment(segment: &str, last: bool) -> Result<(), ZephyrRestError> {
    if segment.is_empty() {
        // Only the final segment may be empty: it is the trailing slash some
        // Enterprise routes require.
        return if last { Ok(()) } else { Err(invalid_input()) };
    }
    if segment.len() > MAX_SEGMENT_BYTES
        || matches!(segment, "." | "..")
        || segment.chars().any(char::is_control)
    {
        return Err(invalid_input());
    }
    Ok(())
}

fn decode_reply(
    response: &ZephyrRestResponse,
    effect: bool,
) -> Result<ZephyrReply, ZephyrRestError> {
    let failure = || {
        if effect {
            unknown_outcome()
        } else {
            invalid_response()
        }
    };
    if response.body.iter().all(u8::is_ascii_whitespace) {
        return Ok(ZephyrReply::Empty);
    }
    let declared_json = response
        .content_type
        .as_deref()
        .is_some_and(is_json_content_type);
    match serde_json::from_slice::<Value>(&response.body) {
        Ok(value) => Ok(ZephyrReply::Json(value)),
        Err(_) if declared_json => Err(failure()),
        Err(_) => std::str::from_utf8(&response.body)
            .map(|text| ZephyrReply::Text(text.to_owned()))
            .map_err(|_| failure()),
    }
}

fn is_json_content_type(value: &str) -> bool {
    let media_type = value.split(';').next().unwrap_or_default().trim();
    media_type.eq_ignore_ascii_case(JSON_CONTENT_TYPE)
        || (media_type.starts_with("application/") && media_type.ends_with("+json"))
}

fn map_http_status(status: StatusCode, effect: bool) -> Result<(), ZephyrRestError> {
    if status.is_success() {
        return Ok(());
    }
    if effect
        && (status.is_server_error()
            || matches!(
                status,
                StatusCode::REQUEST_TIMEOUT | StatusCode::TOO_MANY_REQUESTS
            ))
    {
        return Err(unknown_outcome());
    }
    let code = match status {
        StatusCode::BAD_REQUEST | StatusCode::UNPROCESSABLE_ENTITY => {
            ZephyrRestErrorCode::InvalidInput
        }
        StatusCode::UNAUTHORIZED => ZephyrRestErrorCode::Authentication,
        StatusCode::FORBIDDEN => ZephyrRestErrorCode::Authorization,
        StatusCode::NOT_FOUND => ZephyrRestErrorCode::NotFound,
        StatusCode::CONFLICT => ZephyrRestErrorCode::Conflict,
        StatusCode::REQUEST_TIMEOUT => ZephyrRestErrorCode::Timeout,
        StatusCode::TOO_MANY_REQUESTS => ZephyrRestErrorCode::RateLimited,
        status if status.is_server_error() => ZephyrRestErrorCode::DependencyUnavailable,
        _ if effect => return Err(unknown_outcome()),
        _ => ZephyrRestErrorCode::InvalidResponse,
    };
    Err(ZephyrRestError {
        code,
        retryable: !effect
            && matches!(
                code,
                ZephyrRestErrorCode::Timeout
                    | ZephyrRestErrorCode::RateLimited
                    | ZephyrRestErrorCode::DependencyUnavailable
            ),
    })
}

fn map_reqwest_error(error: &reqwest::Error, effect: bool) -> ZephyrRestError {
    if effect {
        return unknown_outcome();
    }
    ZephyrRestError {
        code: if error.is_timeout() {
            ZephyrRestErrorCode::Timeout
        } else {
            ZephyrRestErrorCode::DependencyUnavailable
        },
        retryable: true,
    }
}

/// Canonical JSON text of `value`, the way the SDK's `str()`/`json.dumps`
/// renders a provider object inside a message.
pub(in crate::toolkits) fn render(value: &Value) -> Result<String, ZephyrRestError> {
    crate::canonical::to_string(value).map_err(|_| invalid_response())
}

/// Python's `f"{value}"` for a scalar read out of a provider object: strings
/// as-is, `None`/`True`/`False` spelled the Python way, anything else as JSON.
pub(in crate::toolkits) fn python_str(value: Option<&Value>) -> String {
    match value {
        None | Some(Value::Null) => "None".to_owned(),
        Some(Value::String(text)) => text.clone(),
        Some(Value::Bool(true)) => "True".to_owned(),
        Some(Value::Bool(false)) => "False".to_owned(),
        Some(Value::Number(number)) => number.to_string(),
        Some(other) => crate::canonical::to_string(other).unwrap_or_default(),
    }
}

/// A model-visible text result, refused when it would exceed the output bound.
pub(in crate::toolkits) fn bounded_text(
    text: String,
    effect: bool,
) -> Result<Value, ZephyrRestError> {
    if text.len() > MAX_OUTPUT_BYTES {
        return Err(resource_exhausted(effect));
    }
    Ok(Value::String(text))
}

/// A model-visible JSON result under the same bound.
pub(in crate::toolkits) fn bounded_value(
    value: Value,
    effect: bool,
) -> Result<Value, ZephyrRestError> {
    let rendered = crate::canonical::to_vec(&value).map_err(|_| invalid_response())?;
    if rendered.len() > MAX_OUTPUT_BYTES {
        return Err(resource_exhausted(effect));
    }
    Ok(value)
}

pub(in crate::toolkits) const fn invalid_configuration() -> ZephyrRestError {
    ZephyrRestError {
        code: ZephyrRestErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

pub(in crate::toolkits) const fn invalid_input() -> ZephyrRestError {
    ZephyrRestError {
        code: ZephyrRestErrorCode::InvalidInput,
        retryable: false,
    }
}

pub(in crate::toolkits) const fn invalid_response() -> ZephyrRestError {
    ZephyrRestError {
        code: ZephyrRestErrorCode::InvalidResponse,
        retryable: false,
    }
}

pub(in crate::toolkits) const fn resource_exhausted(effect: bool) -> ZephyrRestError {
    if effect {
        unknown_outcome()
    } else {
        ZephyrRestError {
            code: ZephyrRestErrorCode::ResourceExhausted,
            retryable: false,
        }
    }
}

pub(in crate::toolkits) const fn unknown_outcome() -> ZephyrRestError {
    ZephyrRestError {
        code: ZephyrRestErrorCode::UnknownOutcome,
        retryable: false,
    }
}
