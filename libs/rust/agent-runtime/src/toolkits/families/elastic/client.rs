use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use reqwest::header::{ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderValue};
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value};
use zeroize::Zeroizing;

use super::config::ElasticToolkitConfig;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_INDEX_BYTES: usize = 1_024;
const MAX_QUERY_BYTES: usize = 64 * 1_024;
const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_QUERY_NODES: usize = 16 * 1_024;
const MAX_QUERY_DEPTH: usize = 32;
const MAX_QUERY_STRING_BYTES: usize = 64 * 1_024;
const MAX_RESULT_SIZE: u64 = 100;
const MAX_RESULT_OFFSET: u64 = 10_000;
const USER_AGENT: &str = "elitea-worker-rust/0.1";
const JSON_CONTENT_TYPE: &str = "application/json";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ElasticClientErrorCode {
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

/// One stable Elasticsearch failure without cluster, key, index, query, or
/// provider body data.
pub struct ElasticClientError {
    code: ElasticClientErrorCode,
}

impl ElasticClientError {
    #[must_use]
    pub const fn code(&self) -> ElasticClientErrorCode {
        self.code
    }

    #[must_use]
    pub const fn retryable(&self) -> bool {
        matches!(
            self.code,
            ElasticClientErrorCode::RateLimited
                | ElasticClientErrorCode::Timeout
                | ElasticClientErrorCode::DependencyUnavailable
        )
    }

    pub fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            ElasticClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "elastic.configuration.invalid",
                "the Elasticsearch toolkit configuration is invalid",
            ),
            ElasticClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "elastic.request.invalid",
                "the Elasticsearch search request is invalid",
            ),
            ElasticClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "elastic.authentication.failed",
                "Elasticsearch authentication failed",
            ),
            ElasticClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "elastic.authorization.failed",
                "Elasticsearch did not authorize the search",
            ),
            ElasticClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "elastic.resource.not_found",
                "the requested Elasticsearch index or search resource was not found",
            ),
            ElasticClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "elastic.rate_limited",
                "Elasticsearch rate limited the search",
            ),
            ElasticClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "elastic.timeout",
                "the Elasticsearch search timed out",
            ),
            ElasticClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "elastic.unavailable",
                "Elasticsearch is unavailable",
            ),
            ElasticClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "elastic.response.invalid",
                "Elasticsearch returned an invalid response",
            ),
            ElasticClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "elastic.resource_exhausted",
                "the Elasticsearch search or response exceeds the approved limit",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable(),
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(any(test, feature = "test-support"))]
    pub const fn fixture(code: ElasticClientErrorCode) -> Self {
        Self { code }
    }
}

impl fmt::Debug for ElasticClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ElasticClientError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ElasticClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ElasticClientErrorCode::InvalidConfiguration => {
                "the Elasticsearch client configuration is invalid"
            }
            ElasticClientErrorCode::InvalidInput => "the Elasticsearch search is invalid",
            ElasticClientErrorCode::Authentication => "Elasticsearch authentication failed",
            ElasticClientErrorCode::Authorization => "Elasticsearch authorization failed",
            ElasticClientErrorCode::NotFound => "the Elasticsearch resource was not found",
            ElasticClientErrorCode::RateLimited => "Elasticsearch rate limited the search",
            ElasticClientErrorCode::Timeout => "the Elasticsearch search timed out",
            ElasticClientErrorCode::DependencyUnavailable => "Elasticsearch is unavailable",
            ElasticClientErrorCode::InvalidResponse => "Elasticsearch returned an invalid response",
            ElasticClientErrorCode::ResourceExhausted => {
                "the Elasticsearch search or response exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for ElasticClientError {}

#[async_trait]
pub trait ElasticApi: Send + Sync {
    async fn search(&self, index: &str, query: &Value) -> Result<Value, ElasticClientError>;
}

pub struct ElasticHttpResponse {
    status: StatusCode,
    content_type: Option<Box<str>>,
    body: Vec<u8>,
}

impl ElasticHttpResponse {
    #[cfg(any(test, feature = "test-support"))]
    pub fn fixture(
        status: StatusCode,
        content_type: Option<&str>,
        body: impl Into<Vec<u8>>,
    ) -> Self {
        Self {
            status,
            content_type: content_type.map(Into::into),
            body: body.into(),
        }
    }
}

#[async_trait]
pub trait ElasticTransport: Send + Sync {
    async fn execute(&self, request: Request) -> Result<ElasticHttpResponse, ElasticClientError>;
}

struct ReqwestElasticTransport {
    http: reqwest::Client,
}

#[async_trait]
impl ElasticTransport for ReqwestElasticTransport {
    async fn execute(&self, request: Request) -> Result<ElasticHttpResponse, ElasticClientError> {
        let mut response = self
            .http
            .execute(request)
            .await
            .map_err(|source| map_reqwest_error(&source))?;
        let status = response.status();
        let content_type = response
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .map(Into::into);
        if response
            .headers()
            .get(CONTENT_LENGTH)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.parse::<usize>().ok())
            .is_some_and(|length| length > MAX_RESPONSE_BYTES)
        {
            return Err(resource_exhausted());
        }
        let mut body = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|source| map_reqwest_error(&source))?
        {
            let next = body
                .len()
                .checked_add(chunk.len())
                .ok_or_else(resource_exhausted)?;
            if next > MAX_RESPONSE_BYTES {
                return Err(resource_exhausted());
            }
            body.extend_from_slice(&chunk);
        }
        Ok(ElasticHttpResponse {
            status,
            content_type,
            body,
        })
    }
}

/// Invocation-owned, fixed-origin Elasticsearch Search API client.
pub struct ElasticClient {
    config: ElasticToolkitConfig,
    transport: Arc<dyn ElasticTransport>,
}

impl ElasticClient {
    pub fn new(config: ElasticToolkitConfig) -> Result<Self, ElasticClientError> {
        let http = reqwest::Client::builder()
            .https_only(true)
            .no_proxy()
            .redirect(reqwest::redirect::Policy::none())
            .retry(reqwest::retry::never())
            .connect_timeout(CONNECT_TIMEOUT)
            .timeout(REQUEST_TIMEOUT)
            .pool_idle_timeout(POOL_IDLE_TIMEOUT)
            .pool_max_idle_per_host(MAX_IDLE_PER_HOST)
            .user_agent(USER_AGENT)
            .build()
            .map_err(|_| invalid_configuration())?;
        Ok(Self {
            config,
            transport: Arc::new(ReqwestElasticTransport { http }),
        })
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn with_transport(
        config: ElasticToolkitConfig,
        transport: Arc<dyn ElasticTransport>,
    ) -> Self {
        Self { config, transport }
    }

    fn request(&self, index: &str, query: &Value) -> Result<Request, ElasticClientError> {
        validate_index(index)?;
        validate_query(query)?;
        let body = serde_json::to_vec(query).map_err(|_| invalid_input())?;
        let mut url = self.config.base_url().clone();
        {
            let mut segments = url
                .path_segments_mut()
                .map_err(|()| invalid_configuration())?;
            segments.clear();
            segments.push(index);
            segments.push("_search");
        }
        let mut request = Request::new(Method::POST, url);
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static(JSON_CONTENT_TYPE));
        request
            .headers_mut()
            .insert(CONTENT_TYPE, HeaderValue::from_static(JSON_CONTENT_TYPE));
        request.headers_mut().insert(
            CONTENT_LENGTH,
            HeaderValue::from_str(&body.len().to_string()).map_err(|_| invalid_input())?,
        );
        if let Some(api_key) = self.config.api_key() {
            request
                .headers_mut()
                .insert(AUTHORIZATION, api_key_header(api_key)?);
        }
        *request.body_mut() = Some(body.into());
        Ok(request)
    }

    async fn search_inner(&self, index: &str, query: &Value) -> Result<Value, ElasticClientError> {
        let response = self.transport.execute(self.request(index, query)?).await?;
        map_http_status(response.status)?;
        if !response
            .content_type
            .as_deref()
            .is_some_and(is_json_content_type)
        {
            return Err(invalid_response());
        }
        let value: Value =
            serde_json::from_slice(&response.body).map_err(|_| invalid_response())?;
        if !value.is_object() {
            return Err(invalid_response());
        }
        if serde_json::to_vec(&value)
            .map_err(|_| invalid_response())?
            .len()
            > MAX_OUTPUT_BYTES
        {
            return Err(resource_exhausted());
        }
        Ok(value)
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn test_request(&self, index: &str, query: &Value) -> Result<Request, ElasticClientError> {
        self.request(index, query)
    }
}

#[async_trait]
impl ElasticApi for ElasticClient {
    async fn search(&self, index: &str, query: &Value) -> Result<Value, ElasticClientError> {
        self.search_inner(index, query).await
    }
}

pub fn validate_index(index: &str) -> Result<(), ElasticClientError> {
    if index.is_empty() || index.len() > MAX_INDEX_BYTES {
        return Err(if index.len() > MAX_INDEX_BYTES {
            resource_exhausted()
        } else {
            invalid_input()
        });
    }
    if !index.bytes().all(|byte| {
        byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'-' | b'_' | b'*' | b'?' | b',')
    }) || index.split(',').any(|expression| {
        expression.is_empty()
            || matches!(expression, "." | "..")
            || expression.starts_with(['-', '_', '+', '*', '?'])
    }) {
        return Err(invalid_input());
    }
    Ok(())
}

pub fn validate_query(query: &Value) -> Result<(), ElasticClientError> {
    let object = query.as_object().ok_or_else(invalid_input)?;
    let body = serde_json::to_vec(query).map_err(|_| invalid_input())?;
    if body.len() > MAX_QUERY_BYTES {
        return Err(resource_exhausted());
    }
    validate_result_window(object)?;
    let mut nodes = 0usize;
    let mut stack = vec![(query, 0usize)];
    while let Some((value, depth)) = stack.pop() {
        nodes = nodes.checked_add(1).ok_or_else(resource_exhausted)?;
        if nodes > MAX_QUERY_NODES || depth > MAX_QUERY_DEPTH {
            return Err(resource_exhausted());
        }
        match value {
            Value::String(value) if value.len() > MAX_QUERY_STRING_BYTES => {
                return Err(resource_exhausted());
            }
            Value::Array(values) => {
                stack.extend(values.iter().map(|value| (value, depth + 1)));
            }
            Value::Object(values) => {
                if values.keys().any(|key| key.len() > MAX_QUERY_STRING_BYTES) {
                    return Err(resource_exhausted());
                }
                stack.extend(values.values().map(|value| (value, depth + 1)));
            }
            Value::Null | Value::Bool(_) | Value::Number(_) | Value::String(_) => {}
        }
    }
    Ok(())
}

fn validate_result_window(query: &Map<String, Value>) -> Result<(), ElasticClientError> {
    if let Some(size) = query.get("size") {
        let size = size.as_u64().ok_or_else(invalid_input)?;
        if size > MAX_RESULT_SIZE {
            return Err(resource_exhausted());
        }
    }
    if let Some(offset) = query.get("from") {
        let offset = offset.as_u64().ok_or_else(invalid_input)?;
        if offset > MAX_RESULT_OFFSET {
            return Err(resource_exhausted());
        }
    }
    Ok(())
}

fn api_key_header(api_key: &str) -> Result<HeaderValue, ElasticClientError> {
    let mut value = Zeroizing::new(String::with_capacity("ApiKey ".len() + api_key.len()));
    value.push_str("ApiKey ");
    value.push_str(api_key);
    HeaderValue::from_str(&value).map_err(|_| invalid_configuration())
}

fn is_json_content_type(value: &str) -> bool {
    let media_type = value.split(';').next().unwrap_or_default().trim();
    media_type.eq_ignore_ascii_case("application/json")
        || media_type.to_ascii_lowercase().ends_with("+json")
}

fn map_http_status(status: StatusCode) -> Result<(), ElasticClientError> {
    match status {
        status if status.is_success() => Ok(()),
        StatusCode::BAD_REQUEST | StatusCode::UNPROCESSABLE_ENTITY => Err(invalid_input()),
        StatusCode::UNAUTHORIZED => Err(authentication()),
        StatusCode::FORBIDDEN => Err(authorization()),
        StatusCode::NOT_FOUND => Err(not_found()),
        StatusCode::REQUEST_TIMEOUT | StatusCode::GATEWAY_TIMEOUT => Err(timeout_error()),
        StatusCode::TOO_MANY_REQUESTS => Err(rate_limited()),
        StatusCode::PAYLOAD_TOO_LARGE => Err(resource_exhausted()),
        status if status.is_server_error() => Err(dependency_unavailable()),
        _ => Err(invalid_response()),
    }
}

fn map_reqwest_error(source: &reqwest::Error) -> ElasticClientError {
    if source.is_timeout() {
        timeout_error()
    } else if source.is_connect() {
        dependency_unavailable()
    } else {
        invalid_response()
    }
}

const fn invalid_configuration() -> ElasticClientError {
    ElasticClientError {
        code: ElasticClientErrorCode::InvalidConfiguration,
    }
}

const fn invalid_input() -> ElasticClientError {
    ElasticClientError {
        code: ElasticClientErrorCode::InvalidInput,
    }
}

const fn authentication() -> ElasticClientError {
    ElasticClientError {
        code: ElasticClientErrorCode::Authentication,
    }
}

const fn authorization() -> ElasticClientError {
    ElasticClientError {
        code: ElasticClientErrorCode::Authorization,
    }
}

const fn not_found() -> ElasticClientError {
    ElasticClientError {
        code: ElasticClientErrorCode::NotFound,
    }
}

const fn rate_limited() -> ElasticClientError {
    ElasticClientError {
        code: ElasticClientErrorCode::RateLimited,
    }
}

const fn timeout_error() -> ElasticClientError {
    ElasticClientError {
        code: ElasticClientErrorCode::Timeout,
    }
}

const fn dependency_unavailable() -> ElasticClientError {
    ElasticClientError {
        code: ElasticClientErrorCode::DependencyUnavailable,
    }
}

const fn invalid_response() -> ElasticClientError {
    ElasticClientError {
        code: ElasticClientErrorCode::InvalidResponse,
    }
}

const fn resource_exhausted() -> ElasticClientError {
    ElasticClientError {
        code: ElasticClientErrorCode::ResourceExhausted,
    }
}
