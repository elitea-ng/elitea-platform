use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use reqwest::header::{ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderValue};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value};
use zeroize::Zeroizing;

use super::config::SonarToolkitConfig;

const ISSUE_SEARCH_PATH: &str = "/api/issues/search";
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_PARAMS_BYTES: usize = 64 * 1_024;
const MAX_QUERY_BYTES: usize = 64 * 1_024;
const MAX_QUERY_KEYS: usize = 64;
const MAX_QUERY_KEY_BYTES: usize = 128;
const MAX_QUERY_VALUES_PER_KEY: usize = 64;
const MAX_QUERY_VALUE_BYTES: usize = 4 * 1_024;
const DEFAULT_PAGE_SIZE: usize = 100;
const MAX_PAGE_SIZE: u64 = 100;
const MAX_PAGE: u64 = 100;
const MAX_RESULT_WINDOW: u64 = 10_000;
const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_RESPONSE_ISSUES: usize = 100;
const USER_AGENT: &str = "elitea-worker-rust/0.1";

// Versioned against Sonar's public issue-search request. Project and component
// scope aliases are deliberately absent; only the claim-materialized
// `componentKeys` value can select the project.
const ALLOWED_QUERY_KEYS: &[&str] = &[
    "additionalFields",
    "asc",
    "assigned",
    "assignees",
    "author",
    "branch",
    "cleanCodeAttributeCategories",
    "codeVariants",
    "componentKeys",
    "createdAfter",
    "createdAt",
    "createdBefore",
    "createdInLast",
    "cwe",
    "directories",
    "facetMode",
    "facets",
    "files",
    "impactSeverities",
    "impactSoftwareQualities",
    "inNewCodePeriod",
    "issues",
    "issueStatuses",
    "languages",
    "onComponentOnly",
    "owaspAsvs40",
    "owaspMobileTop10",
    "owaspTop10",
    "p",
    "ps",
    "pullRequest",
    "resolutions",
    "resolved",
    "rules",
    "s",
    "sansTop25",
    "scopes",
    "severities",
    "sonarsourceSecurity",
    "statuses",
    "tags",
    "types",
];

/// Stable, secret-free Sonar request and provider failure categories.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SonarClientErrorCode {
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

/// One bounded Sonar failure that retains no URL, token, project, query, body,
/// or upstream error text.
pub(crate) struct SonarClientError {
    code: SonarClientErrorCode,
}

impl SonarClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> SonarClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        matches!(
            self.code,
            SonarClientErrorCode::RateLimited
                | SonarClientErrorCode::Timeout
                | SonarClientErrorCode::DependencyUnavailable
        )
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            SonarClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "sonar.configuration.invalid",
                "the Sonar toolkit configuration is invalid",
            ),
            SonarClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "sonar.request.invalid",
                "the Sonar request is invalid",
            ),
            SonarClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "sonar.authentication.failed",
                "Sonar authentication failed",
            ),
            SonarClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "sonar.authorization.failed",
                "Sonar did not authorize the request",
            ),
            SonarClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "sonar.resource.not_found",
                "the requested Sonar resource was not found",
            ),
            SonarClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "sonar.rate_limited",
                "Sonar rate limited the request",
            ),
            SonarClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "sonar.timeout",
                "the Sonar request timed out",
            ),
            SonarClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "sonar.unavailable",
                "Sonar is unavailable",
            ),
            SonarClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "sonar.response.invalid",
                "Sonar returned an invalid response",
            ),
            SonarClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "sonar.resource_exhausted",
                "the Sonar request or response exceeds the approved limit",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable(),
            retry_after_ms: None,
            max_attempts: None,
        })
    }
}

impl fmt::Debug for SonarClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SonarClientError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for SonarClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            SonarClientErrorCode::InvalidConfiguration => {
                "the Sonar client configuration is invalid"
            }
            SonarClientErrorCode::InvalidInput => "the Sonar request is invalid",
            SonarClientErrorCode::Authentication => "Sonar authentication failed",
            SonarClientErrorCode::Authorization => "Sonar authorization failed",
            SonarClientErrorCode::NotFound => "the Sonar resource was not found",
            SonarClientErrorCode::RateLimited => "Sonar rate limited the request",
            SonarClientErrorCode::Timeout => "the Sonar request timed out",
            SonarClientErrorCode::DependencyUnavailable => "Sonar is unavailable",
            SonarClientErrorCode::InvalidResponse => "Sonar returned an invalid response",
            SonarClientErrorCode::ResourceExhausted => {
                "the Sonar request or response exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for SonarClientError {}

/// The complete read operation exposed by the current SDK Sonar family.
#[async_trait]
pub(in crate::toolkits) trait SonarApi: Send + Sync {
    async fn get_sonar_data(
        &self,
        relative_url: &str,
        params: Option<&str>,
    ) -> Result<Value, SonarClientError>;
}

#[async_trait]
pub(in crate::toolkits) trait SonarTransport: Send + Sync {
    async fn execute_json(&self, request: Request) -> Result<Value, SonarClientError>;
}

struct ReqwestSonarTransport {
    http: reqwest::Client,
}

#[async_trait]
impl SonarTransport for ReqwestSonarTransport {
    async fn execute_json(&self, request: Request) -> Result<Value, SonarClientError> {
        let mut response = self
            .http
            .execute(request)
            .await
            .map_err(|source| map_reqwest_error(&source))?;
        map_http_status(response.status())?;
        if !response
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.split(';').next())
            .is_some_and(|value| value.trim().eq_ignore_ascii_case("application/json"))
        {
            return Err(invalid_response());
        }
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
        serde_json::from_slice(&body).map_err(|_| invalid_response())
    }
}

/// One invocation-scoped Sonar client and HTTP connection pool.
pub(crate) struct SonarClient {
    config: SonarToolkitConfig,
    transport: Arc<dyn SonarTransport>,
}

impl SonarClient {
    pub(crate) fn new(config: SonarToolkitConfig) -> Result<Self, SonarClientError> {
        let http = reqwest::Client::builder()
            .https_only(true)
            .redirect(reqwest::redirect::Policy::none())
            .connect_timeout(CONNECT_TIMEOUT)
            .timeout(REQUEST_TIMEOUT)
            .pool_idle_timeout(POOL_IDLE_TIMEOUT)
            .pool_max_idle_per_host(MAX_IDLE_PER_HOST)
            .user_agent(USER_AGENT)
            .build()
            .map_err(|_| invalid_configuration())?;
        Ok(Self {
            config,
            transport: Arc::new(ReqwestSonarTransport { http }),
        })
    }

    fn build_request(
        &self,
        relative_url: &str,
        params: Option<&str>,
    ) -> Result<(Request, usize), SonarClientError> {
        validate_relative_url(relative_url)?;
        let query = parse_query(params)?;
        let page_size = page_size(&query)?;
        validate_result_window(&query, page_size)?;
        let has_page_size = query.iter().any(|(key, _)| key == "ps");
        let mut endpoint = issue_search_endpoint(self.config.base_url())?;
        {
            let mut pairs = endpoint.query_pairs_mut();
            for (key, values) in query {
                if key == "componentKeys" {
                    continue;
                }
                for value in values {
                    pairs.append_pair(&key, &value);
                }
            }
            pairs.append_pair("componentKeys", self.config.project());
            if !has_page_size {
                pairs.append_pair("ps", &DEFAULT_PAGE_SIZE.to_string());
            }
        }
        if endpoint
            .query()
            .is_none_or(|query| query.len() > MAX_QUERY_BYTES)
        {
            return Err(resource_exhausted());
        }
        let mut authorization = basic_authorization(self.config.token())?;
        authorization.set_sensitive(true);
        let mut request = Request::new(Method::GET, endpoint);
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static("application/json"));
        request.headers_mut().insert(AUTHORIZATION, authorization);
        *request.timeout_mut() = Some(REQUEST_TIMEOUT);
        Ok((request, page_size))
    }
}

#[async_trait]
impl SonarApi for SonarClient {
    async fn get_sonar_data(
        &self,
        relative_url: &str,
        params: Option<&str>,
    ) -> Result<Value, SonarClientError> {
        let (request, page_size) = self.build_request(relative_url, params)?;
        let response = self.transport.execute_json(request).await?;
        validate_payload(response, page_size)
    }
}

fn validate_relative_url(value: &str) -> Result<(), SonarClientError> {
    if value == ISSUE_SEARCH_PATH {
        Ok(())
    } else {
        Err(invalid_input())
    }
}

fn issue_search_endpoint(base_url: &Url) -> Result<Url, SonarClientError> {
    let mut endpoint = base_url.clone();
    let mut segments = endpoint
        .path_segments_mut()
        .map_err(|()| invalid_configuration())?;
    segments.pop_if_empty();
    segments.extend(["api", "issues", "search"]);
    drop(segments);
    Ok(endpoint)
}

fn basic_authorization(token: &str) -> Result<HeaderValue, SonarClientError> {
    let mut plaintext = Zeroizing::new(String::with_capacity(token.len() + 1));
    plaintext.push_str(token);
    plaintext.push(':');
    let encoded = Zeroizing::new(STANDARD.encode(plaintext.as_bytes()));
    let mut header = Zeroizing::new(String::with_capacity(encoded.len() + 6));
    header.push_str("Basic ");
    header.push_str(&encoded);
    HeaderValue::from_str(&header).map_err(|_| invalid_configuration())
}

fn parse_query(params: Option<&str>) -> Result<Vec<(String, Vec<String>)>, SonarClientError> {
    let Some(params) = params.filter(|value| !value.is_empty()) else {
        return Ok(Vec::new());
    };
    if params.len() > MAX_PARAMS_BYTES {
        return Err(resource_exhausted());
    }
    let object = serde_json::from_str::<Map<String, Value>>(params).map_err(|_| invalid_input())?;
    if object.len() > MAX_QUERY_KEYS {
        return Err(resource_exhausted());
    }
    let mut query = Vec::with_capacity(object.len());
    for (key, value) in object {
        validate_query_key(&key)?;
        let values = query_values(&key, value)?;
        if !values.is_empty() {
            query.push((key, values));
        }
    }
    Ok(query)
}

fn validate_query_key(key: &str) -> Result<(), SonarClientError> {
    if key.len() > MAX_QUERY_KEY_BYTES {
        return Err(resource_exhausted());
    }
    if key.is_empty()
        || !key
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-' | b'.'))
        || !ALLOWED_QUERY_KEYS.contains(&key)
    {
        return Err(invalid_input());
    }
    Ok(())
}

fn query_values(key: &str, value: Value) -> Result<Vec<String>, SonarClientError> {
    let values = match value {
        Value::Null => Vec::new(),
        Value::Array(values) => {
            if values.len() > MAX_QUERY_VALUES_PER_KEY {
                return Err(resource_exhausted());
            }
            values
                .into_iter()
                .map(query_scalar)
                .collect::<Result<Vec<_>, _>>()?
        }
        value => vec![query_scalar(value)?],
    };
    if !values.is_empty() {
        validate_paging(key, &values)?;
    }
    Ok(values)
}

fn query_scalar(value: Value) -> Result<String, SonarClientError> {
    let value = match value {
        Value::String(value) => value,
        Value::Bool(value) => value.to_string(),
        Value::Number(value) => value.to_string(),
        _ => return Err(invalid_input()),
    };
    if value.len() > MAX_QUERY_VALUE_BYTES {
        return Err(resource_exhausted());
    }
    if value.chars().any(char::is_control) {
        return Err(invalid_input());
    }
    Ok(value)
}

fn validate_paging(key: &str, values: &[String]) -> Result<(), SonarClientError> {
    let maximum = match key {
        "p" => MAX_PAGE,
        "ps" => MAX_PAGE_SIZE,
        _ => return Ok(()),
    };
    if values.len() != 1
        || values[0]
            .parse::<u64>()
            .ok()
            .is_none_or(|value| value == 0 || value > maximum)
    {
        return Err(invalid_input());
    }
    Ok(())
}

fn page_size(query: &[(String, Vec<String>)]) -> Result<usize, SonarClientError> {
    query
        .iter()
        .find(|(key, _)| key == "ps")
        .map_or(Ok(DEFAULT_PAGE_SIZE), |(_, values)| {
            values
                .first()
                .and_then(|value| value.parse::<usize>().ok())
                .ok_or_else(invalid_input)
        })
}

fn validate_result_window(
    query: &[(String, Vec<String>)],
    page_size: usize,
) -> Result<(), SonarClientError> {
    let page = query
        .iter()
        .find(|(key, _)| key == "p")
        .and_then(|(_, values)| values.first())
        .and_then(|value| value.parse::<u64>().ok())
        .unwrap_or(1);
    let page_size = u64::try_from(page_size).map_err(|_| resource_exhausted())?;
    if page
        .checked_mul(page_size)
        .is_none_or(|window| window > MAX_RESULT_WINDOW)
    {
        return Err(invalid_input());
    }
    Ok(())
}

fn validate_payload(response: Value, page_size: usize) -> Result<Value, SonarClientError> {
    let object = response.as_object().ok_or_else(invalid_response)?;
    if object
        .get("issues")
        .is_some_and(|issues| !issues.is_array())
    {
        return Err(invalid_response());
    }
    if object
        .get("issues")
        .and_then(Value::as_array)
        .is_some_and(|issues| issues.len() > page_size || issues.len() > MAX_RESPONSE_ISSUES)
    {
        return Err(resource_exhausted());
    }
    let output = serde_json::to_vec(&response).map_err(|_| invalid_response())?;
    if output.len() > MAX_OUTPUT_BYTES {
        return Err(resource_exhausted());
    }
    Ok(response)
}

fn map_http_status(status: StatusCode) -> Result<(), SonarClientError> {
    match status {
        StatusCode::OK => Ok(()),
        StatusCode::BAD_REQUEST => Err(invalid_input()),
        StatusCode::UNAUTHORIZED => Err(error(SonarClientErrorCode::Authentication)),
        StatusCode::FORBIDDEN => Err(error(SonarClientErrorCode::Authorization)),
        StatusCode::NOT_FOUND => Err(error(SonarClientErrorCode::NotFound)),
        StatusCode::REQUEST_TIMEOUT | StatusCode::GATEWAY_TIMEOUT => {
            Err(error(SonarClientErrorCode::Timeout))
        }
        StatusCode::TOO_MANY_REQUESTS => Err(error(SonarClientErrorCode::RateLimited)),
        status if status.is_server_error() => Err(dependency_unavailable()),
        _ => Err(invalid_response()),
    }
}

fn map_reqwest_error(source: &reqwest::Error) -> SonarClientError {
    if source.is_timeout() {
        return error(SonarClientErrorCode::Timeout);
    }
    if source.is_connect() || source.is_request() || source.is_body() {
        return dependency_unavailable();
    }
    invalid_response()
}

const fn error(code: SonarClientErrorCode) -> SonarClientError {
    SonarClientError { code }
}

const fn invalid_configuration() -> SonarClientError {
    error(SonarClientErrorCode::InvalidConfiguration)
}

const fn invalid_input() -> SonarClientError {
    error(SonarClientErrorCode::InvalidInput)
}

const fn invalid_response() -> SonarClientError {
    error(SonarClientErrorCode::InvalidResponse)
}

const fn resource_exhausted() -> SonarClientError {
    error(SonarClientErrorCode::ResourceExhausted)
}

const fn dependency_unavailable() -> SonarClientError {
    error(SonarClientErrorCode::DependencyUnavailable)
}

#[cfg(test)]
impl SonarClient {
    pub(in crate::toolkits) fn test_with_transport(
        config: SonarToolkitConfig,
        transport: Arc<dyn SonarTransport>,
    ) -> Self {
        Self { config, transport }
    }

    pub(in crate::toolkits) fn test_request(
        &self,
        relative_url: &str,
        params: Option<&str>,
    ) -> Result<Request, SonarClientError> {
        self.build_request(relative_url, params)
            .map(|(request, _)| request)
    }
}

#[cfg(test)]
pub(in crate::toolkits) fn test_http_status(status: StatusCode) -> Result<(), SonarClientError> {
    map_http_status(status)
}
