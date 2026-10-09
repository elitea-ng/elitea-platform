use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderValue, USER_AGENT,
};
use reqwest::{Method, Request, StatusCode};
use serde_json::{Value, json};
use zeroize::Zeroizing;

use super::config::ZephyrToolkitConfig;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_REQUEST_BYTES: usize = 256 * 1_024;
const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_STEPS_RETURNED: usize = 1_000;
const JSON_CONTENT_TYPE: &str = "application/json";
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ZephyrClientErrorCode {
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

/// Stable failure without origin, credentials, identifiers, request bodies, or
/// provider diagnostics.
pub(crate) struct ZephyrClientError {
    code: ZephyrClientErrorCode,
    retryable: bool,
}

impl ZephyrClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> ZephyrClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    #[must_use]
    pub(crate) const fn after_confirmed_effect() -> Self {
        unknown_outcome()
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            ZephyrClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "zephyr.configuration.invalid",
                "the legacy Zephyr toolkit configuration is invalid",
            ),
            ZephyrClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "zephyr.request.invalid",
                "the legacy Zephyr request is invalid",
            ),
            ZephyrClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "zephyr.authentication.failed",
                "legacy Zephyr authentication failed",
            ),
            ZephyrClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "zephyr.authorization.failed",
                "legacy Zephyr did not authorize the request",
            ),
            ZephyrClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "zephyr.resource.not_found",
                "the requested legacy Zephyr resource was not found",
            ),
            ZephyrClientErrorCode::Conflict => (
                ErrorCategory::InvalidInput,
                "zephyr.resource.conflict",
                "the legacy Zephyr request conflicts with provider state",
            ),
            ZephyrClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "zephyr.rate_limited",
                "legacy Zephyr rate limited the request",
            ),
            ZephyrClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "zephyr.timeout",
                "the legacy Zephyr request timed out",
            ),
            ZephyrClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "zephyr.unavailable",
                "legacy Zephyr is unavailable",
            ),
            ZephyrClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "zephyr.response.invalid",
                "legacy Zephyr returned an invalid response",
            ),
            ZephyrClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "zephyr.response.resource_exhausted",
                "the legacy Zephyr response exceeds the approved limit",
            ),
            ZephyrClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "zephyr.effect.unknown_outcome",
                "legacy Zephyr may have created test steps; reconcile them before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(code: ZephyrClientErrorCode, retryable: bool) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for ZephyrClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ZephyrClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ZephyrClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ZephyrClientErrorCode::InvalidConfiguration => {
                "the legacy Zephyr client configuration is invalid"
            }
            ZephyrClientErrorCode::InvalidInput => "the legacy Zephyr request is invalid",
            ZephyrClientErrorCode::Authentication => "legacy Zephyr authentication failed",
            ZephyrClientErrorCode::Authorization => "legacy Zephyr authorization failed",
            ZephyrClientErrorCode::NotFound => "the legacy Zephyr resource was not found",
            ZephyrClientErrorCode::Conflict => "the legacy Zephyr request conflicts",
            ZephyrClientErrorCode::RateLimited => "legacy Zephyr rate limited the request",
            ZephyrClientErrorCode::Timeout => "the legacy Zephyr request timed out",
            ZephyrClientErrorCode::DependencyUnavailable => "legacy Zephyr is unavailable",
            ZephyrClientErrorCode::InvalidResponse => "legacy Zephyr returned an invalid response",
            ZephyrClientErrorCode::ResourceExhausted => {
                "the legacy Zephyr response exceeds its approved limit"
            }
            ZephyrClientErrorCode::UnknownOutcome => {
                "the legacy Zephyr effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for ZephyrClientError {}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(in crate::toolkits) struct ZephyrStep {
    pub(in crate::toolkits) step: Box<str>,
    pub(in crate::toolkits) data: Box<str>,
    pub(in crate::toolkits) result: Box<str>,
}

impl ZephyrStep {
    pub(in crate::toolkits) fn new(step: &str, data: &str, result: &str) -> Self {
        Self {
            step: step.into(),
            data: data.into(),
            result: result.into(),
        }
    }
}

#[async_trait]
pub(in crate::toolkits) trait ZephyrApi: Send + Sync {
    async fn get_test_case_steps(
        &self,
        issue_id: u64,
        project_id: u64,
    ) -> Result<Value, ZephyrClientError>;

    async fn add_new_test_case_step(
        &self,
        issue_id: u64,
        project_id: u64,
        step: &ZephyrStep,
    ) -> Result<Value, ZephyrClientError>;
}

pub(in crate::toolkits) struct ZephyrHttpResponse {
    status: StatusCode,
    content_type: Option<Box<str>>,
    body: Vec<u8>,
}

impl ZephyrHttpResponse {
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
}

#[async_trait]
pub(in crate::toolkits) trait ZephyrTransport: Send + Sync {
    async fn execute(&self, request: Request) -> Result<ZephyrHttpResponse, ZephyrClientError>;
}

struct ReqwestZephyrTransport {
    http: reqwest::Client,
}

#[async_trait]
impl ZephyrTransport for ReqwestZephyrTransport {
    async fn execute(&self, request: Request) -> Result<ZephyrHttpResponse, ZephyrClientError> {
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
            return Err(response_bound_failure(effect));
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
                .ok_or_else(|| response_bound_failure(effect))?;
            if next > MAX_RESPONSE_BYTES {
                return Err(response_bound_failure(effect));
            }
            body.extend_from_slice(&chunk);
        }
        Ok(ZephyrHttpResponse {
            status,
            content_type,
            body,
        })
    }
}

/// Invocation-owned Basic-auth client for one exact legacy Zephyr ZAPI base.
pub(crate) struct ZephyrClient {
    config: ZephyrToolkitConfig,
    transport: Arc<dyn ZephyrTransport>,
}

impl ZephyrClient {
    pub(crate) fn new(config: ZephyrToolkitConfig) -> Result<Self, ZephyrClientError> {
        let http = reqwest::Client::builder()
            .https_only(true)
            .no_proxy()
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
            transport: Arc::new(ReqwestZephyrTransport { http }),
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn with_transport(
        config: ZephyrToolkitConfig,
        transport: Arc<dyn ZephyrTransport>,
    ) -> Self {
        Self { config, transport }
    }

    fn request(
        &self,
        method: Method,
        issue_id: u64,
        project_id: u64,
        body: Option<&ZephyrStep>,
    ) -> Result<Request, ZephyrClientError> {
        if issue_id == 0 || project_id == 0 {
            return Err(invalid_input());
        }
        let mut url = self.config.base_url().clone();
        {
            let mut segments = url
                .path_segments_mut()
                .map_err(|()| invalid_configuration())?;
            segments.pop_if_empty();
            segments.push("teststep");
            segments.push(&issue_id.to_string());
        }
        url.query_pairs_mut()
            .append_pair("projectId", &project_id.to_string());
        let mut request = Request::new(method, url);
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static(JSON_CONTENT_TYPE));
        request
            .headers_mut()
            .insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
        request.headers_mut().insert(
            AUTHORIZATION,
            basic_auth_header(self.config.username(), self.config.password())?,
        );
        if let Some(step) = body {
            let body = serde_json::to_vec(&json!({
                "step": step.step.as_ref(),
                "data": step.data.as_ref(),
                "result": step.result.as_ref(),
            }))
            .map_err(|_| invalid_input())?;
            if body.len() > MAX_REQUEST_BYTES {
                return Err(resource_exhausted(false));
            }
            request
                .headers_mut()
                .insert(CONTENT_TYPE, HeaderValue::from_static(JSON_CONTENT_TYPE));
            request.headers_mut().insert(
                CONTENT_LENGTH,
                HeaderValue::from_str(&body.len().to_string()).map_err(|_| invalid_input())?,
            );
            *request.body_mut() = Some(body.into());
        }
        Ok(request)
    }

    async fn call(
        &self,
        method: Method,
        issue_id: u64,
        project_id: u64,
        body: Option<&ZephyrStep>,
    ) -> Result<ZephyrHttpResponse, ZephyrClientError> {
        let effect = method != Method::GET;
        let response = self
            .transport
            .execute(self.request(method, issue_id, project_id, body)?)
            .await?;
        map_http_status(response.status, effect)?;
        Ok(response)
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_request(
        &self,
        method: Method,
        issue_id: u64,
        project_id: u64,
        body: Option<&ZephyrStep>,
    ) -> Result<Request, ZephyrClientError> {
        self.request(method, issue_id, project_id, body)
    }
}

#[async_trait]
impl ZephyrApi for ZephyrClient {
    async fn get_test_case_steps(
        &self,
        issue_id: u64,
        project_id: u64,
    ) -> Result<Value, ZephyrClientError> {
        let response = self.call(Method::GET, issue_id, project_id, None).await?;
        project_test_steps(&response)
    }

    async fn add_new_test_case_step(
        &self,
        issue_id: u64,
        project_id: u64,
        step: &ZephyrStep,
    ) -> Result<Value, ZephyrClientError> {
        let response = self
            .call(Method::POST, issue_id, project_id, Some(step))
            .await?;
        project_created_step(&response)
    }
}

fn project_test_steps(response: &ZephyrHttpResponse) -> Result<Value, ZephyrClientError> {
    if !response
        .content_type
        .as_deref()
        .is_some_and(is_json_content_type)
    {
        return Err(invalid_response());
    }
    let value: Value = serde_json::from_slice(&response.body).map_err(|_| invalid_response())?;
    let steps = value
        .get("stepBeanCollection")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    if steps.len() > MAX_STEPS_RETURNED {
        return Err(resource_exhausted(false));
    }
    let mut projected = Vec::with_capacity(steps.len());
    for step in steps {
        let object = step.as_object().ok_or_else(invalid_response)?;
        projected.push(json!({
            "order_id": object.get("orderId").ok_or_else(invalid_response)?,
            "step": object.get("step").ok_or_else(invalid_response)?,
            "data": object.get("data").ok_or_else(invalid_response)?,
            "result": object.get("result").ok_or_else(invalid_response)?,
        }));
    }
    if projected.is_empty() {
        return Ok(Value::String("No Zephyr test steps found".to_owned()));
    }
    // Sorted members whatever `preserve_order` a build unifies: the text the
    // model reads is the SDK's (`crate::canonical`).
    let count = projected.len();
    let records =
        crate::canonical::to_string(&Value::Array(projected)).map_err(|_| invalid_response())?;
    let output = format!("Found {count} test steps:\n{records}");
    if output.len() > MAX_OUTPUT_BYTES {
        return Err(resource_exhausted(false));
    }
    Ok(Value::String(output))
}

fn project_created_step(response: &ZephyrHttpResponse) -> Result<Value, ZephyrClientError> {
    let text = std::str::from_utf8(&response.body).map_err(|_| unknown_outcome())?;
    let output = format!("New test step created: {text}");
    if output.len() > MAX_OUTPUT_BYTES {
        return Err(unknown_outcome());
    }
    Ok(Value::String(output))
}

fn basic_auth_header(username: &str, password: &str) -> Result<HeaderValue, ZephyrClientError> {
    let mut plain = Zeroizing::new(Vec::with_capacity(username.len() + password.len() + 1));
    plain.extend_from_slice(username.as_bytes());
    plain.push(b':');
    plain.extend_from_slice(password.as_bytes());
    let encoded = Zeroizing::new(STANDARD.encode(plain.as_slice()));
    let mut value = Zeroizing::new(String::with_capacity("Basic ".len() + encoded.len()));
    value.push_str("Basic ");
    value.push_str(encoded.as_str());
    HeaderValue::from_str(value.as_str()).map_err(|_| invalid_configuration())
}

fn is_json_content_type(value: &str) -> bool {
    let media_type = value.split(';').next().unwrap_or_default().trim();
    media_type.eq_ignore_ascii_case(JSON_CONTENT_TYPE)
        || (media_type.starts_with("application/") && media_type.ends_with("+json"))
}

fn map_http_status(status: StatusCode, effect: bool) -> Result<(), ZephyrClientError> {
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
            ZephyrClientErrorCode::InvalidInput
        }
        StatusCode::UNAUTHORIZED => ZephyrClientErrorCode::Authentication,
        StatusCode::FORBIDDEN => ZephyrClientErrorCode::Authorization,
        StatusCode::NOT_FOUND => ZephyrClientErrorCode::NotFound,
        StatusCode::CONFLICT => ZephyrClientErrorCode::Conflict,
        StatusCode::REQUEST_TIMEOUT => ZephyrClientErrorCode::Timeout,
        StatusCode::TOO_MANY_REQUESTS => ZephyrClientErrorCode::RateLimited,
        status if status.is_server_error() => ZephyrClientErrorCode::DependencyUnavailable,
        _ if effect => return Err(unknown_outcome()),
        _ => ZephyrClientErrorCode::InvalidResponse,
    };
    Err(ZephyrClientError {
        code,
        retryable: !effect
            && matches!(
                code,
                ZephyrClientErrorCode::Timeout
                    | ZephyrClientErrorCode::RateLimited
                    | ZephyrClientErrorCode::DependencyUnavailable
            ),
    })
}

fn map_reqwest_error(error: &reqwest::Error, effect: bool) -> ZephyrClientError {
    if effect {
        return unknown_outcome();
    }
    if error.is_timeout() {
        ZephyrClientError {
            code: ZephyrClientErrorCode::Timeout,
            retryable: true,
        }
    } else {
        ZephyrClientError {
            code: ZephyrClientErrorCode::DependencyUnavailable,
            retryable: true,
        }
    }
}

const fn invalid_configuration() -> ZephyrClientError {
    ZephyrClientError {
        code: ZephyrClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

const fn invalid_input() -> ZephyrClientError {
    ZephyrClientError {
        code: ZephyrClientErrorCode::InvalidInput,
        retryable: false,
    }
}

const fn invalid_response() -> ZephyrClientError {
    ZephyrClientError {
        code: ZephyrClientErrorCode::InvalidResponse,
        retryable: false,
    }
}

const fn resource_exhausted(effect: bool) -> ZephyrClientError {
    if effect {
        unknown_outcome()
    } else {
        ZephyrClientError {
            code: ZephyrClientErrorCode::ResourceExhausted,
            retryable: false,
        }
    }
}

const fn response_bound_failure(effect: bool) -> ZephyrClientError {
    resource_exhausted(effect)
}

const fn unknown_outcome() -> ZephyrClientError {
    ZephyrClientError {
        code: ZephyrClientErrorCode::UnknownOutcome,
        retryable: false,
    }
}
