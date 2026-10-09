use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use percent_encoding::{NON_ALPHANUMERIC, utf8_percent_encode};
use reqwest::header::{ACCEPT, CONTENT_LENGTH, CONTENT_TYPE, HeaderName, HeaderValue};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value, json};

use super::config::AzureSearchToolkitConfig;

const API_VERSION: &str = "2024-07-01";
const ACCEPT_VALUE: &str = "application/json;odata.metadata=none";
const API_KEY: HeaderName = HeaderName::from_static("api-key");
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_REQUEST_BYTES: usize = 128 * 1_024;
const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_SEARCH_TEXT_BYTES: usize = 64 * 1_024;
const MAX_DOCUMENT_ID_BYTES: usize = 4 * 1_024;
const MAX_RESULTS: usize = 100;
const MAX_ORDER_BY: usize = 32;
const MAX_ORDER_BY_BYTES: usize = 2 * 1_024;
const MAX_ORDER_BY_TOTAL_BYTES: usize = 16 * 1_024;
const MAX_SELECTED_FIELDS: usize = 128;
const MAX_SELECTED_FIELD_BYTES: usize = 512;
const MAX_SELECTED_FIELDS_TOTAL_BYTES: usize = 32 * 1_024;
const USER_AGENT: &str = "elitea-worker-rust/0.1";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum AzureSearchClientErrorCode {
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

/// One bounded Azure Search failure without provider text or authority data.
pub(crate) struct AzureSearchClientError {
    code: AzureSearchClientErrorCode,
}

impl AzureSearchClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> AzureSearchClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        matches!(
            self.code,
            AzureSearchClientErrorCode::RateLimited
                | AzureSearchClientErrorCode::Timeout
                | AzureSearchClientErrorCode::DependencyUnavailable
        )
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            AzureSearchClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "azure_search.configuration.invalid",
                "the Azure Search toolkit configuration is invalid",
            ),
            AzureSearchClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "azure_search.request.invalid",
                "the Azure Search request is invalid",
            ),
            AzureSearchClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "azure_search.authentication.failed",
                "Azure Search authentication failed",
            ),
            AzureSearchClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "azure_search.authorization.failed",
                "Azure Search did not authorize the request",
            ),
            AzureSearchClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "azure_search.resource.not_found",
                "the requested Azure Search resource was not found",
            ),
            AzureSearchClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "azure_search.rate_limited",
                "Azure Search rate limited the request",
            ),
            AzureSearchClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "azure_search.timeout",
                "the Azure Search request timed out",
            ),
            AzureSearchClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "azure_search.unavailable",
                "Azure Search is unavailable",
            ),
            AzureSearchClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "azure_search.response.invalid",
                "Azure Search returned an invalid response",
            ),
            AzureSearchClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "azure_search.response.resource_exhausted",
                "the Azure Search response exceeds the approved limit",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable(),
            retry_after_ms: None,
            max_attempts: None,
        })
    }
}

impl fmt::Debug for AzureSearchClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AzureSearchClientError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for AzureSearchClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            AzureSearchClientErrorCode::InvalidConfiguration => {
                "the Azure Search client configuration is invalid"
            }
            AzureSearchClientErrorCode::InvalidInput => "the Azure Search request is invalid",
            AzureSearchClientErrorCode::Authentication => "Azure Search authentication failed",
            AzureSearchClientErrorCode::Authorization => "Azure Search authorization failed",
            AzureSearchClientErrorCode::NotFound => "the Azure Search resource was not found",
            AzureSearchClientErrorCode::RateLimited => "Azure Search rate limited the request",
            AzureSearchClientErrorCode::Timeout => "the Azure Search request timed out",
            AzureSearchClientErrorCode::DependencyUnavailable => "Azure Search is unavailable",
            AzureSearchClientErrorCode::InvalidResponse => {
                "Azure Search returned an invalid response"
            }
            AzureSearchClientErrorCode::ResourceExhausted => {
                "the Azure Search response exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for AzureSearchClientError {}

#[async_trait]
pub(in crate::toolkits) trait AzureSearchApi: Send + Sync {
    async fn text_search(
        &self,
        search_text: &str,
        limit: usize,
        order_by: &[String],
        selected_fields: &[String],
    ) -> Result<Value, AzureSearchClientError>;

    async fn get_document(
        &self,
        document_id: &str,
        selected_fields: &[String],
    ) -> Result<Value, AzureSearchClientError>;
}

#[derive(Clone, Copy)]
pub(in crate::toolkits) enum AzureSearchRequestKind<'a> {
    TextSearch {
        search_text: &'a str,
        limit: usize,
        order_by: &'a [String],
        selected_fields: &'a [String],
    },
    GetDocument {
        document_id: &'a str,
        selected_fields: &'a [String],
    },
}

#[async_trait]
pub(in crate::toolkits) trait AzureSearchTransport: Send + Sync {
    async fn execute_json(&self, request: Request) -> Result<Value, AzureSearchClientError>;
}

struct ReqwestAzureSearchTransport {
    http: reqwest::Client,
}

#[async_trait]
impl AzureSearchTransport for ReqwestAzureSearchTransport {
    async fn execute_json(&self, request: Request) -> Result<Value, AzureSearchClientError> {
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

/// One invocation-scoped Azure Search client and connection pool.
pub(crate) struct AzureSearchClient {
    config: AzureSearchToolkitConfig,
    transport: Arc<dyn AzureSearchTransport>,
}

impl AzureSearchClient {
    pub(crate) fn new(config: AzureSearchToolkitConfig) -> Result<Self, AzureSearchClientError> {
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
            transport: Arc::new(ReqwestAzureSearchTransport { http }),
        })
    }

    fn build_request(
        &self,
        kind: AzureSearchRequestKind<'_>,
    ) -> Result<Request, AzureSearchClientError> {
        let mut request = match kind {
            AzureSearchRequestKind::TextSearch {
                search_text,
                limit,
                order_by,
                selected_fields,
            } => {
                validate_search_text(search_text)?;
                validate_limit(limit)?;
                validate_order_by(order_by)?;
                validate_selected_fields(selected_fields)?;
                let endpoint = self.search_endpoint();
                let mut body = Map::new();
                body.insert("search".to_owned(), Value::String(search_text.to_owned()));
                body.insert("top".to_owned(), json!(limit));
                if !order_by.is_empty() {
                    body.insert("orderby".to_owned(), Value::String(order_by.join(",")));
                }
                if !selected_fields.is_empty() {
                    body.insert(
                        "select".to_owned(),
                        Value::String(selected_fields.join(",")),
                    );
                }
                let encoded = serde_json::to_vec(&body).map_err(|_| invalid_input())?;
                if encoded.len() > MAX_REQUEST_BYTES {
                    return Err(resource_exhausted());
                }
                let mut request = Request::new(Method::POST, endpoint);
                request
                    .headers_mut()
                    .insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
                *request.body_mut() = Some(encoded.into());
                request
            }
            AzureSearchRequestKind::GetDocument {
                document_id,
                selected_fields,
            } => {
                validate_document_id(document_id)?;
                validate_selected_fields(selected_fields)?;
                Request::new(
                    Method::GET,
                    self.document_endpoint(document_id, selected_fields)?,
                )
            }
        };
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static(ACCEPT_VALUE));
        let mut api_key =
            HeaderValue::from_str(self.config.api_key()).map_err(|_| invalid_configuration())?;
        api_key.set_sensitive(true);
        request.headers_mut().insert(API_KEY, api_key);
        *request.timeout_mut() = Some(REQUEST_TIMEOUT);
        Ok(request)
    }

    fn search_endpoint(&self) -> Url {
        let path = format!(
            "/indexes('{}')/docs/search.post.search",
            self.config.index_name()
        );
        let mut endpoint = self.config.endpoint().clone();
        endpoint.set_path(&path);
        endpoint
            .query_pairs_mut()
            .append_pair("api-version", API_VERSION);
        endpoint
    }

    fn document_endpoint(
        &self,
        document_id: &str,
        selected_fields: &[String],
    ) -> Result<Url, AzureSearchClientError> {
        let encoded_id = utf8_percent_encode(document_id, NON_ALPHANUMERIC);
        let mut endpoint = Url::parse(&format!(
            "{}/indexes('{}')/docs('{}')",
            self.config.endpoint().as_str().trim_end_matches('/'),
            self.config.index_name(),
            encoded_id
        ))
        .map_err(|_| invalid_input())?;
        if !selected_fields.is_empty() {
            endpoint
                .query_pairs_mut()
                .append_pair("$select", &selected_fields.join(","));
        }
        endpoint
            .query_pairs_mut()
            .append_pair("api-version", API_VERSION);
        Ok(endpoint)
    }
}

#[async_trait]
impl AzureSearchApi for AzureSearchClient {
    async fn text_search(
        &self,
        search_text: &str,
        limit: usize,
        order_by: &[String],
        selected_fields: &[String],
    ) -> Result<Value, AzureSearchClientError> {
        let request = self.build_request(AzureSearchRequestKind::TextSearch {
            search_text,
            limit,
            order_by,
            selected_fields,
        })?;
        let response = self.transport.execute_json(request).await?;
        project_search_results(response, limit)
    }

    async fn get_document(
        &self,
        document_id: &str,
        selected_fields: &[String],
    ) -> Result<Value, AzureSearchClientError> {
        let request = self.build_request(AzureSearchRequestKind::GetDocument {
            document_id,
            selected_fields,
        })?;
        let response = self.transport.execute_json(request).await?;
        if !response.is_object() {
            return Err(invalid_response());
        }
        ensure_output_bound(&response)?;
        Ok(response)
    }
}

fn project_search_results(response: Value, limit: usize) -> Result<Value, AzureSearchClientError> {
    let Value::Object(mut payload) = response else {
        return Err(invalid_response());
    };
    let results = payload
        .remove("value")
        .and_then(|value| value.as_array().cloned())
        .ok_or_else(invalid_response)?;
    if results.len() > limit || results.len() > MAX_RESULTS {
        return Err(resource_exhausted());
    }
    let projected = results
        .into_iter()
        .map(|result| {
            let mut document = result.as_object().ok_or_else(invalid_response)?.clone();
            let reranker = document
                .remove("@search.rerankerScore")
                .unwrap_or(Value::Null);
            document
                .entry("@search.score".to_owned())
                .or_insert(Value::Null);
            document.insert("@search.reranker_score".to_owned(), reranker);
            document
                .entry("@search.highlights".to_owned())
                .or_insert(Value::Null);
            document
                .entry("@search.captions".to_owned())
                .or_insert(Value::Null);
            Ok(Value::Object(document))
        })
        .collect::<Result<Vec<_>, AzureSearchClientError>>()?;
    let projected = Value::Array(projected);
    ensure_output_bound(&projected)?;
    Ok(projected)
}

fn ensure_output_bound(value: &Value) -> Result<(), AzureSearchClientError> {
    let encoded = serde_json::to_vec(value).map_err(|_| invalid_response())?;
    if encoded.len() > MAX_OUTPUT_BYTES {
        return Err(resource_exhausted());
    }
    Ok(())
}

fn validate_search_text(value: &str) -> Result<(), AzureSearchClientError> {
    validate_text(value, MAX_SEARCH_TEXT_BYTES, true)
}

fn validate_document_id(value: &str) -> Result<(), AzureSearchClientError> {
    validate_text(value, MAX_DOCUMENT_ID_BYTES, false)
}

fn validate_text(
    value: &str,
    maximum: usize,
    allow_empty: bool,
) -> Result<(), AzureSearchClientError> {
    if value.len() > maximum {
        return Err(resource_exhausted());
    }
    if (!allow_empty && value.is_empty()) || value.chars().any(char::is_control) {
        return Err(invalid_input());
    }
    Ok(())
}

fn validate_limit(limit: usize) -> Result<(), AzureSearchClientError> {
    if !(1..=MAX_RESULTS).contains(&limit) {
        return Err(invalid_input());
    }
    Ok(())
}

fn validate_order_by(values: &[String]) -> Result<(), AzureSearchClientError> {
    validate_string_list(
        values,
        MAX_ORDER_BY,
        MAX_ORDER_BY_BYTES,
        MAX_ORDER_BY_TOTAL_BYTES,
        false,
    )
}

fn validate_selected_fields(values: &[String]) -> Result<(), AzureSearchClientError> {
    validate_string_list(
        values,
        MAX_SELECTED_FIELDS,
        MAX_SELECTED_FIELD_BYTES,
        MAX_SELECTED_FIELDS_TOTAL_BYTES,
        true,
    )?;
    if values.iter().any(|field| !valid_selected_field(field)) {
        return Err(invalid_input());
    }
    Ok(())
}

fn validate_string_list(
    values: &[String],
    maximum_items: usize,
    maximum_item_bytes: usize,
    maximum_total_bytes: usize,
    reject_commas: bool,
) -> Result<(), AzureSearchClientError> {
    if values.len() > maximum_items {
        return Err(resource_exhausted());
    }
    let total = values.iter().try_fold(0_usize, |total, value| {
        if value.len() > maximum_item_bytes {
            return Err(resource_exhausted());
        }
        if value.trim().is_empty()
            || value.chars().any(char::is_control)
            || (reject_commas && value.contains(','))
        {
            return Err(invalid_input());
        }
        total
            .checked_add(value.len())
            .ok_or_else(resource_exhausted)
    })?;
    if total > maximum_total_bytes {
        return Err(resource_exhausted());
    }
    Ok(())
}

fn valid_selected_field(value: &str) -> bool {
    value == "*"
        || value.split('/').all(|segment| {
            let mut characters = segment.chars();
            characters
                .next()
                .is_some_and(|character| character.is_ascii_alphabetic() || character == '_')
                && characters.all(|character| character.is_ascii_alphanumeric() || character == '_')
        })
}

fn map_http_status(status: StatusCode) -> Result<(), AzureSearchClientError> {
    match status {
        status if status.is_success() => Ok(()),
        StatusCode::BAD_REQUEST | StatusCode::UNPROCESSABLE_ENTITY => Err(invalid_input()),
        StatusCode::UNAUTHORIZED => Err(authentication()),
        StatusCode::FORBIDDEN => Err(authorization()),
        StatusCode::NOT_FOUND => Err(not_found()),
        StatusCode::REQUEST_TIMEOUT | StatusCode::GATEWAY_TIMEOUT => Err(timeout()),
        StatusCode::TOO_MANY_REQUESTS => Err(rate_limited()),
        StatusCode::PAYLOAD_TOO_LARGE => Err(resource_exhausted()),
        StatusCode::CONFLICT
        | StatusCode::BAD_GATEWAY
        | StatusCode::SERVICE_UNAVAILABLE
        | StatusCode::INTERNAL_SERVER_ERROR => Err(dependency_unavailable()),
        status if status.is_server_error() => Err(dependency_unavailable()),
        _ => Err(invalid_response()),
    }
}

fn map_reqwest_error(source: &reqwest::Error) -> AzureSearchClientError {
    if source.is_timeout() {
        timeout()
    } else if source.is_connect() || source.is_request() || source.is_body() || source.is_decode() {
        dependency_unavailable()
    } else {
        invalid_response()
    }
}

const fn error(code: AzureSearchClientErrorCode) -> AzureSearchClientError {
    AzureSearchClientError { code }
}

const fn invalid_configuration() -> AzureSearchClientError {
    error(AzureSearchClientErrorCode::InvalidConfiguration)
}

const fn invalid_input() -> AzureSearchClientError {
    error(AzureSearchClientErrorCode::InvalidInput)
}

const fn authentication() -> AzureSearchClientError {
    error(AzureSearchClientErrorCode::Authentication)
}

const fn authorization() -> AzureSearchClientError {
    error(AzureSearchClientErrorCode::Authorization)
}

const fn not_found() -> AzureSearchClientError {
    error(AzureSearchClientErrorCode::NotFound)
}

const fn rate_limited() -> AzureSearchClientError {
    error(AzureSearchClientErrorCode::RateLimited)
}

const fn timeout() -> AzureSearchClientError {
    error(AzureSearchClientErrorCode::Timeout)
}

const fn dependency_unavailable() -> AzureSearchClientError {
    error(AzureSearchClientErrorCode::DependencyUnavailable)
}

const fn invalid_response() -> AzureSearchClientError {
    error(AzureSearchClientErrorCode::InvalidResponse)
}

const fn resource_exhausted() -> AzureSearchClientError {
    error(AzureSearchClientErrorCode::ResourceExhausted)
}

#[cfg(test)]
impl AzureSearchClient {
    pub(in crate::toolkits) fn test_with_transport(
        config: AzureSearchToolkitConfig,
        transport: Arc<dyn AzureSearchTransport>,
    ) -> Self {
        Self { config, transport }
    }

    pub(in crate::toolkits) fn test_request(
        &self,
        kind: AzureSearchRequestKind<'_>,
    ) -> Result<Request, AzureSearchClientError> {
        self.build_request(kind)
    }
}

#[cfg(test)]
pub(in crate::toolkits) fn test_http_status(
    status: StatusCode,
) -> Result<(), AzureSearchClientError> {
    map_http_status(status)
}
