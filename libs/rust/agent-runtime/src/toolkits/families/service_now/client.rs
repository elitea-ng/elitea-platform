use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64_STANDARD;
use percent_encoding::{NON_ALPHANUMERIC, utf8_percent_encode};
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderValue, USER_AGENT,
};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value};
use zeroize::Zeroizing;

use super::config::ServiceNowToolkitConfig;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_REQUEST_BYTES: usize = 128 * 1_024;
const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_INCIDENTS: usize = 100;
const MAX_RECORD_FIELDS: usize = 1_024;
const MAX_FILTERS: usize = 16;
const MAX_FILTER_VALUE_BYTES: usize = 4 * 1_024;
const MAX_ENCODED_QUERY_BYTES: usize = 32 * 1_024;
const SERVICE_NOW_SYS_ID_BYTES: usize = 32;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";
const JSON_CONTENT_TYPE: &str = "application/json";
const INCIDENT_PATH: &str = "api/now/table/incident";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ServiceNowClientErrorCode {
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

/// One bounded `ServiceNow` failure without provider text or credentials.
pub struct ServiceNowClientError {
    code: ServiceNowClientErrorCode,
    retryable: bool,
}

impl ServiceNowClientError {
    #[must_use]
    pub const fn code(&self) -> ServiceNowClientErrorCode {
        self.code
    }

    #[must_use]
    pub const fn retryable(&self) -> bool {
        self.retryable
    }

    pub fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            ServiceNowClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "service_now.configuration.invalid",
                "the ServiceNow toolkit configuration is invalid",
            ),
            ServiceNowClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "service_now.request.invalid",
                "the ServiceNow request is invalid",
            ),
            ServiceNowClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "service_now.authentication.failed",
                "ServiceNow authentication failed",
            ),
            ServiceNowClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "service_now.authorization.failed",
                "ServiceNow did not authorize the request",
            ),
            ServiceNowClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "service_now.incident.not_found",
                "the requested ServiceNow incident was not found",
            ),
            ServiceNowClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "service_now.rate_limited",
                "ServiceNow rate limited the request",
            ),
            ServiceNowClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "service_now.timeout",
                "the ServiceNow request timed out",
            ),
            ServiceNowClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "service_now.unavailable",
                "ServiceNow is unavailable",
            ),
            ServiceNowClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "service_now.response.invalid",
                "ServiceNow returned an invalid response",
            ),
            ServiceNowClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "service_now.response.resource_exhausted",
                "the ServiceNow response exceeds the approved limit",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(any(test, feature = "test-support"))]
    pub const fn fixture(code: ServiceNowClientErrorCode, retryable: bool) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for ServiceNowClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ServiceNowClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ServiceNowClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ServiceNowClientErrorCode::InvalidConfiguration => {
                "the ServiceNow client configuration is invalid"
            }
            ServiceNowClientErrorCode::InvalidInput => "the ServiceNow request is invalid",
            ServiceNowClientErrorCode::Authentication => "ServiceNow authentication failed",
            ServiceNowClientErrorCode::Authorization => "ServiceNow authorization failed",
            ServiceNowClientErrorCode::NotFound => "the ServiceNow incident was not found",
            ServiceNowClientErrorCode::RateLimited => "ServiceNow rate limited the request",
            ServiceNowClientErrorCode::Timeout => "the ServiceNow request timed out",
            ServiceNowClientErrorCode::DependencyUnavailable => "ServiceNow is unavailable",
            ServiceNowClientErrorCode::InvalidResponse => "ServiceNow returned an invalid response",
            ServiceNowClientErrorCode::ResourceExhausted => {
                "the ServiceNow response exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for ServiceNowClientError {}

#[async_trait]
pub trait ServiceNowApi: Send + Sync {
    async fn get_incidents(
        &self,
        filters: &Map<String, Value>,
        limit: usize,
    ) -> Result<Value, ServiceNowClientError>;

    async fn create_incident(
        &self,
        fields: &Map<String, Value>,
    ) -> Result<Value, ServiceNowClientError>;

    async fn update_incident(
        &self,
        sys_id: &str,
        fields: &Map<String, Value>,
    ) -> Result<Value, ServiceNowClientError>;
}

#[derive(Clone, Copy)]
pub enum ServiceNowRequestKind<'a> {
    GetIncidents {
        filters: &'a Map<String, Value>,
        limit: usize,
    },
    GetIncident {
        sys_id: &'a str,
    },
    CreateIncident {
        fields: &'a Map<String, Value>,
    },
    UpdateIncident {
        sys_id: &'a str,
        fields: &'a Map<String, Value>,
    },
}

#[async_trait]
pub trait ServiceNowTransport: Send + Sync {
    async fn execute_json(
        &self,
        request: Request,
        expected_status: StatusCode,
        retryable_transport: bool,
    ) -> Result<Value, ServiceNowClientError>;
}

struct ReqwestServiceNowTransport {
    http: reqwest::Client,
}

#[async_trait]
impl ServiceNowTransport for ReqwestServiceNowTransport {
    async fn execute_json(
        &self,
        request: Request,
        expected_status: StatusCode,
        retryable_transport: bool,
    ) -> Result<Value, ServiceNowClientError> {
        let mut response = self
            .http
            .execute(request)
            .await
            .map_err(|source| map_reqwest_error(&source, retryable_transport))?;
        map_http_status(response.status(), expected_status, retryable_transport)?;
        if !response
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.split(';').next())
            .is_some_and(|value| value.trim().eq_ignore_ascii_case(JSON_CONTENT_TYPE))
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
            .map_err(|source| map_reqwest_error(&source, retryable_transport))?
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

/// One invocation-scoped `ServiceNow` client and connection pool.
pub struct ServiceNowClient {
    config: ServiceNowToolkitConfig,
    transport: Arc<dyn ServiceNowTransport>,
}

impl ServiceNowClient {
    pub fn new(config: ServiceNowToolkitConfig) -> Result<Self, ServiceNowClientError> {
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
            transport: Arc::new(ReqwestServiceNowTransport { http }),
        })
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn with_transport(
        config: ServiceNowToolkitConfig,
        transport: Arc<dyn ServiceNowTransport>,
    ) -> Self {
        Self { config, transport }
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn test_request(
        &self,
        kind: ServiceNowRequestKind<'_>,
    ) -> Result<Request, ServiceNowClientError> {
        self.build_request(kind)
    }

    fn build_request(
        &self,
        kind: ServiceNowRequestKind<'_>,
    ) -> Result<Request, ServiceNowClientError> {
        let (method, url, body) = match kind {
            ServiceNowRequestKind::GetIncidents { filters, limit } => {
                if limit == 0 || limit > MAX_INCIDENTS || filters.len() > MAX_FILTERS {
                    return Err(invalid_input());
                }
                let mut url = self.incident_url(None)?;
                append_common_query(&mut url);
                let fields = response_fields_with_sys_id(self.config.response_fields());
                url.query_pairs_mut()
                    .append_pair("sysparm_query", &encoded_query(filters)?)
                    .append_pair("sysparm_fields", &fields.join(","))
                    .append_pair("sysparm_limit", &limit.to_string())
                    .append_pair("sysparm_offset", "0");
                (Method::GET, url, None)
            }
            ServiceNowRequestKind::GetIncident { sys_id } => {
                validate_sys_id(sys_id)?;
                let mut url = self.incident_url(Some(sys_id))?;
                append_common_query(&mut url);
                let fields = response_fields_with_sys_id(self.config.response_fields());
                url.query_pairs_mut()
                    .append_pair("sysparm_query", "ORDERBYsys_id")
                    .append_pair("sysparm_fields", &fields.join(","));
                (Method::GET, url, None)
            }
            ServiceNowRequestKind::CreateIncident { fields } => {
                let mut url = self.incident_url(None)?;
                append_common_query(&mut url);
                (
                    Method::POST,
                    url,
                    Some(encode_fields(fields).map_err(ServiceNowClientError::with_effect_risk)?),
                )
            }
            ServiceNowRequestKind::UpdateIncident { sys_id, fields } => {
                validate_sys_id(sys_id)?;
                let mut url = self.incident_url(Some(sys_id))?;
                append_common_query(&mut url);
                (
                    Method::PATCH,
                    url,
                    Some(encode_fields(fields).map_err(ServiceNowClientError::with_effect_risk)?),
                )
            }
        };

        let mut request = Request::new(method, url);
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static(JSON_CONTENT_TYPE));
        request
            .headers_mut()
            .insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
        request
            .headers_mut()
            .insert(AUTHORIZATION, self.authorization_header()?);
        if let Some(body) = body {
            let content_length = HeaderValue::from_str(&body.len().to_string())
                .map_err(|_| invalid_configuration())?;
            request
                .headers_mut()
                .insert(CONTENT_TYPE, HeaderValue::from_static(JSON_CONTENT_TYPE));
            request.headers_mut().insert(CONTENT_LENGTH, content_length);
            *request.body_mut() = Some(body.into());
        }
        Ok(request)
    }

    fn incident_url(&self, sys_id: Option<&str>) -> Result<Url, ServiceNowClientError> {
        let mut url = self
            .config
            .origin()
            .join(INCIDENT_PATH)
            .map_err(|_| invalid_configuration())?;
        if let Some(sys_id) = sys_id {
            let path = format!(
                "{INCIDENT_PATH}/{}",
                utf8_percent_encode(sys_id, NON_ALPHANUMERIC)
            );
            url = self
                .config
                .origin()
                .join(&path)
                .map_err(|_| invalid_configuration())?;
        }
        Ok(url)
    }

    fn authorization_header(&self) -> Result<HeaderValue, ServiceNowClientError> {
        let mut credentials = Zeroizing::new(String::with_capacity(
            self.config.username().len() + self.config.password().len() + 1,
        ));
        credentials.push_str(self.config.username());
        credentials.push(':');
        credentials.push_str(self.config.password());
        let encoded = Zeroizing::new(BASE64_STANDARD.encode(credentials.as_bytes()));
        let mut header = Zeroizing::new(String::with_capacity(encoded.len() + 6));
        header.push_str("Basic ");
        header.push_str(encoded.as_str());
        let mut value = HeaderValue::from_str(&header).map_err(|_| invalid_configuration())?;
        value.set_sensitive(true);
        Ok(value)
    }

    async fn execute(
        &self,
        kind: ServiceNowRequestKind<'_>,
        expected_status: StatusCode,
        retryable_transport: bool,
    ) -> Result<Value, ServiceNowClientError> {
        let request = self.build_request(kind)?;
        self.transport
            .execute_json(request, expected_status, retryable_transport)
            .await
    }
}

#[async_trait]
impl ServiceNowApi for ServiceNowClient {
    async fn get_incidents(
        &self,
        filters: &Map<String, Value>,
        limit: usize,
    ) -> Result<Value, ServiceNowClientError> {
        let response = self
            .execute(
                ServiceNowRequestKind::GetIncidents { filters, limit },
                StatusCode::OK,
                true,
            )
            .await?;
        project_result_list(&response, limit)
    }

    async fn create_incident(
        &self,
        fields: &Map<String, Value>,
    ) -> Result<Value, ServiceNowClientError> {
        let response = self
            .execute(
                ServiceNowRequestKind::CreateIncident { fields },
                StatusCode::CREATED,
                false,
            )
            .await?;
        project_single_result(&response)
    }

    async fn update_incident(
        &self,
        sys_id: &str,
        fields: &Map<String, Value>,
    ) -> Result<Value, ServiceNowClientError> {
        let existing = self
            .execute(
                ServiceNowRequestKind::GetIncident { sys_id },
                StatusCode::OK,
                true,
            )
            .await?;
        let _ = project_single_result(&existing)?;
        let response = self
            .execute(
                ServiceNowRequestKind::UpdateIncident { sys_id, fields },
                StatusCode::OK,
                false,
            )
            .await?;
        project_single_result(&response)
    }
}

impl ServiceNowClientError {
    const fn with_effect_risk(mut self) -> Self {
        self.retryable = false;
        self
    }
}

fn append_common_query(url: &mut Url) {
    url.query_pairs_mut()
        .append_pair("sysparm_display_value", "all")
        .append_pair("sysparm_exclude_reference_link", "true")
        .append_pair("sysparm_suppress_pagination_header", "true");
}

fn response_fields_with_sys_id(fields: &[Box<str>]) -> Vec<&str> {
    let mut response = Vec::with_capacity(fields.len() + 1);
    if !fields.iter().any(|field| field.as_ref() == "sys_id") {
        response.push("sys_id");
    }
    response.extend(fields.iter().map(AsRef::as_ref));
    response
}

fn encoded_query(filters: &Map<String, Value>) -> Result<String, ServiceNowClientError> {
    let mut conditions = Vec::with_capacity(filters.len() + 1);
    for (field, value) in filters {
        if field == "number_of_entries" || value.is_null() {
            continue;
        }
        if !matches!(
            field.as_str(),
            "category" | "description" | "creation_date" | "sys_id" | "number"
        ) {
            return Err(invalid_input());
        }
        let value = filter_value(value)?;
        if value.len() > MAX_FILTER_VALUE_BYTES
            || value
                .bytes()
                .any(|byte| matches!(byte, 0 | b'\r' | b'\n' | b'^'))
        {
            return Err(if value.len() > MAX_FILTER_VALUE_BYTES {
                resource_exhausted()
            } else {
                invalid_input()
            });
        }
        let operator = if field == "description" {
            "CONTAINS"
        } else {
            "="
        };
        conditions.push(format!("{field}{operator}{value}"));
    }
    conditions.push("ORDERBYsys_id".to_owned());
    let query = conditions.join("^");
    if query.len() > MAX_ENCODED_QUERY_BYTES {
        return Err(resource_exhausted());
    }
    Ok(query)
}

fn filter_value(value: &Value) -> Result<String, ServiceNowClientError> {
    match value {
        Value::String(value) => Ok(value.clone()),
        Value::Bool(value) => Ok(value.to_string()),
        Value::Number(value) => Ok(value.to_string()),
        Value::Null | Value::Array(_) | Value::Object(_) => Err(invalid_input()),
    }
}

fn validate_sys_id(value: &str) -> Result<(), ServiceNowClientError> {
    if value.len() != SERVICE_NOW_SYS_ID_BYTES
        || !value.bytes().all(|byte| byte.is_ascii_alphanumeric())
    {
        return Err(invalid_input());
    }
    Ok(())
}

fn encode_fields(fields: &Map<String, Value>) -> Result<Vec<u8>, ServiceNowClientError> {
    if fields.len() > MAX_RECORD_FIELDS {
        return Err(resource_exhausted());
    }
    let mut filtered = Map::new();
    for (field, value) in fields {
        if field.is_empty()
            || field.len() > 256
            || !field
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'.'))
        {
            return Err(if field.len() > 256 {
                resource_exhausted()
            } else {
                invalid_input()
            });
        }
        if !value.is_null() {
            filtered.insert(field.clone(), value.clone());
        }
    }
    let encoded = serde_json::to_vec(&filtered).map_err(|_| invalid_input())?;
    if encoded.len() > MAX_REQUEST_BYTES {
        return Err(resource_exhausted());
    }
    Ok(encoded)
}

fn project_result_list(response: &Value, limit: usize) -> Result<Value, ServiceNowClientError> {
    let results = response
        .as_object()
        .and_then(|object| object.get("result"))
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    if results.len() > limit || results.len() > MAX_INCIDENTS {
        return Err(resource_exhausted());
    }
    let projected = results
        .iter()
        .map(project_record)
        .collect::<Result<Vec<_>, _>>()?;
    bounded_output(Value::Array(projected))
}

fn project_single_result(response: &Value) -> Result<Value, ServiceNowClientError> {
    let result = response
        .as_object()
        .and_then(|object| object.get("result"))
        .ok_or_else(invalid_response)?;
    bounded_output(Value::Array(vec![project_record(result)?]))
}

fn project_record(value: &Value) -> Result<Value, ServiceNowClientError> {
    let record = value.as_object().ok_or_else(invalid_response)?;
    if record.len() > MAX_RECORD_FIELDS {
        return Err(resource_exhausted());
    }
    let projected = record
        .iter()
        .map(|(key, value)| {
            let value = value.as_object().map_or_else(
                || value.clone(),
                |object| {
                    object
                        .get("value")
                        .filter(|value| !value.is_null())
                        .or_else(|| object.get("display_value"))
                        .or_else(|| object.get("value"))
                        .cloned()
                        .unwrap_or(Value::Null)
                },
            );
            (key.clone(), value)
        })
        .collect::<Map<_, _>>();
    Ok(Value::Object(projected))
}

fn bounded_output(value: Value) -> Result<Value, ServiceNowClientError> {
    if serde_json::to_vec(&value)
        .map_err(|_| invalid_response())?
        .len()
        > MAX_OUTPUT_BYTES
    {
        return Err(resource_exhausted());
    }
    Ok(value)
}

fn map_http_status(
    actual: StatusCode,
    expected: StatusCode,
    retryable_transport: bool,
) -> Result<(), ServiceNowClientError> {
    if actual == expected {
        return Ok(());
    }
    let code = match actual {
        StatusCode::BAD_REQUEST | StatusCode::UNPROCESSABLE_ENTITY => {
            ServiceNowClientErrorCode::InvalidInput
        }
        StatusCode::UNAUTHORIZED => ServiceNowClientErrorCode::Authentication,
        StatusCode::FORBIDDEN => ServiceNowClientErrorCode::Authorization,
        StatusCode::NOT_FOUND => ServiceNowClientErrorCode::NotFound,
        StatusCode::TOO_MANY_REQUESTS => ServiceNowClientErrorCode::RateLimited,
        StatusCode::REQUEST_TIMEOUT | StatusCode::GATEWAY_TIMEOUT => {
            ServiceNowClientErrorCode::Timeout
        }
        status if status.is_server_error() => ServiceNowClientErrorCode::DependencyUnavailable,
        _ => ServiceNowClientErrorCode::InvalidResponse,
    };
    let transient = matches!(
        code,
        ServiceNowClientErrorCode::RateLimited
            | ServiceNowClientErrorCode::Timeout
            | ServiceNowClientErrorCode::DependencyUnavailable
    );
    Err(ServiceNowClientError {
        code,
        retryable: transient && retryable_transport,
    })
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_http_status(
    actual: StatusCode,
    expected: StatusCode,
    retryable_transport: bool,
) -> Result<(), ServiceNowClientError> {
    map_http_status(actual, expected, retryable_transport)
}

fn map_reqwest_error(source: &reqwest::Error, retryable: bool) -> ServiceNowClientError {
    ServiceNowClientError {
        code: if source.is_timeout() {
            ServiceNowClientErrorCode::Timeout
        } else {
            ServiceNowClientErrorCode::DependencyUnavailable
        },
        retryable,
    }
}

const fn invalid_configuration() -> ServiceNowClientError {
    ServiceNowClientError {
        code: ServiceNowClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

const fn invalid_input() -> ServiceNowClientError {
    ServiceNowClientError {
        code: ServiceNowClientErrorCode::InvalidInput,
        retryable: false,
    }
}

const fn invalid_response() -> ServiceNowClientError {
    ServiceNowClientError {
        code: ServiceNowClientErrorCode::InvalidResponse,
        retryable: false,
    }
}

const fn resource_exhausted() -> ServiceNowClientError {
    ServiceNowClientError {
        code: ServiceNowClientErrorCode::ResourceExhausted,
        retryable: false,
    }
}
