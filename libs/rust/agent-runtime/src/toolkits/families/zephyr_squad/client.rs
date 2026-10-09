use std::fmt;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderName, HeaderValue, USER_AGENT,
};
use reqwest::{Method, Request, StatusCode, Url};
use ring::hmac;
use serde_json::{Value, json};

use super::config::ZephyrSquadToolkitConfig;

const API_ROOT: &str = "https://prod-api.zephyr4jiracloud.com/connect";
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 8;
const MAX_REQUEST_BYTES: usize = 256 * 1_024;
const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";
const JSON_CONTENT_TYPE: &str = "application/json";
const JWT_HEADER: &[u8] = br#"{"alg":"HS256","typ":"JWT"}"#;
const JWT_LIFETIME_SECONDS: u64 = 300;
const ZAPI_ACCESS_KEY: HeaderName = HeaderName::from_static("zapiaccesskey");

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ZephyrSquadClientErrorCode {
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

/// Stable failure without JWTs, credentials, paths, payloads or provider text.
pub(crate) struct ZephyrSquadClientError {
    code: ZephyrSquadClientErrorCode,
    retryable: bool,
}

impl ZephyrSquadClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> ZephyrSquadClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            ZephyrSquadClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "zephyr_squad.configuration.invalid",
                "the Zephyr Squad toolkit configuration is invalid",
            ),
            ZephyrSquadClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "zephyr_squad.request.invalid",
                "the Zephyr Squad request is invalid",
            ),
            ZephyrSquadClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "zephyr_squad.authentication.failed",
                "Zephyr Squad authentication failed",
            ),
            ZephyrSquadClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "zephyr_squad.authorization.failed",
                "Zephyr Squad did not authorize the request",
            ),
            ZephyrSquadClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "zephyr_squad.resource.not_found",
                "the requested Zephyr Squad resource was not found",
            ),
            ZephyrSquadClientErrorCode::Conflict => (
                ErrorCategory::InvalidInput,
                "zephyr_squad.resource.conflict",
                "the Zephyr Squad request conflicts with current provider state",
            ),
            ZephyrSquadClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "zephyr_squad.rate_limited",
                "Zephyr Squad rate limited the request",
            ),
            ZephyrSquadClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "zephyr_squad.timeout",
                "the Zephyr Squad request timed out",
            ),
            ZephyrSquadClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "zephyr_squad.unavailable",
                "Zephyr Squad is unavailable",
            ),
            ZephyrSquadClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "zephyr_squad.response.invalid",
                "Zephyr Squad returned an invalid response",
            ),
            ZephyrSquadClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "zephyr_squad.response.resource_exhausted",
                "the Zephyr Squad response exceeds the approved limit",
            ),
            ZephyrSquadClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "zephyr_squad.effect.unknown_outcome",
                "Zephyr Squad may have applied the requested effect; reconcile it before retrying",
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
        code: ZephyrSquadClientErrorCode,
        retryable: bool,
    ) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for ZephyrSquadClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ZephyrSquadClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ZephyrSquadClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ZephyrSquadClientErrorCode::InvalidConfiguration => {
                "the Zephyr Squad client configuration is invalid"
            }
            ZephyrSquadClientErrorCode::InvalidInput => "the Zephyr Squad request is invalid",
            ZephyrSquadClientErrorCode::Authentication => "Zephyr Squad authentication failed",
            ZephyrSquadClientErrorCode::Authorization => "Zephyr Squad authorization failed",
            ZephyrSquadClientErrorCode::NotFound => "the Zephyr Squad resource was not found",
            ZephyrSquadClientErrorCode::Conflict => "the Zephyr Squad request conflicts",
            ZephyrSquadClientErrorCode::RateLimited => "Zephyr Squad rate limited the request",
            ZephyrSquadClientErrorCode::Timeout => "the Zephyr Squad request timed out",
            ZephyrSquadClientErrorCode::DependencyUnavailable => "Zephyr Squad is unavailable",
            ZephyrSquadClientErrorCode::InvalidResponse => {
                "Zephyr Squad returned an invalid response"
            }
            ZephyrSquadClientErrorCode::ResourceExhausted => {
                "the Zephyr Squad response exceeds its approved limit"
            }
            ZephyrSquadClientErrorCode::UnknownOutcome => {
                "the Zephyr Squad effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for ZephyrSquadClientError {}

#[async_trait]
pub(in crate::toolkits) trait ZephyrSquadApi: Send + Sync {
    async fn get_test_step(
        &self,
        issue_id: u64,
        step_id: &str,
        project_id: u64,
    ) -> Result<Value, ZephyrSquadClientError>;
    async fn update_test_step(
        &self,
        issue_id: u64,
        step_id: &str,
        project_id: u64,
        body: &Value,
    ) -> Result<Value, ZephyrSquadClientError>;
    async fn delete_test_step(
        &self,
        issue_id: u64,
        step_id: &str,
        project_id: u64,
    ) -> Result<Value, ZephyrSquadClientError>;
    async fn create_new_test_step(
        &self,
        issue_id: u64,
        project_id: u64,
        body: &Value,
    ) -> Result<Value, ZephyrSquadClientError>;
    async fn get_all_test_steps(
        &self,
        issue_id: u64,
        project_id: u64,
    ) -> Result<Value, ZephyrSquadClientError>;
    async fn get_all_test_step_statuses(&self) -> Result<Value, ZephyrSquadClientError>;
    async fn get_bdd_content(&self, issue_id: u64) -> Result<Value, ZephyrSquadClientError>;
    async fn update_bdd_content(
        &self,
        issue_id: u64,
        content: &str,
    ) -> Result<Value, ZephyrSquadClientError>;
    async fn delete_bdd_content(&self, issue_id: u64) -> Result<Value, ZephyrSquadClientError>;
    async fn create_new_cycle(&self, body: &Value) -> Result<Value, ZephyrSquadClientError>;
    async fn create_folder(&self, body: &Value) -> Result<Value, ZephyrSquadClientError>;
    async fn add_test_to_cycle(
        &self,
        cycle_id: &str,
        body: &Value,
    ) -> Result<Value, ZephyrSquadClientError>;
    async fn add_test_to_folder(
        &self,
        folder_id: &str,
        body: &Value,
    ) -> Result<Value, ZephyrSquadClientError>;
    async fn create_execution(&self, body: &Value) -> Result<Value, ZephyrSquadClientError>;
    async fn get_execution(
        &self,
        execution_id: &str,
        issue_id: u64,
        project_id: u64,
    ) -> Result<Value, ZephyrSquadClientError>;
}

pub(in crate::toolkits) struct ZephyrSquadHttpResponse {
    status: StatusCode,
    value: Value,
}

impl ZephyrSquadHttpResponse {
    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(status: StatusCode, value: Value) -> Self {
        Self { status, value }
    }
}

#[async_trait]
pub(in crate::toolkits) trait ZephyrSquadTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<ZephyrSquadHttpResponse, ZephyrSquadClientError>;
}

struct ReqwestZephyrSquadTransport {
    http: reqwest::Client,
}

#[async_trait]
impl ZephyrSquadTransport for ReqwestZephyrSquadTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<ZephyrSquadHttpResponse, ZephyrSquadClientError> {
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
        let json_content_type = response
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.split(';').next())
            .is_some_and(|value| value.trim().eq_ignore_ascii_case(JSON_CONTENT_TYPE));
        let mut bytes = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|source| map_reqwest_error(&source, effect))?
        {
            if bytes.len().saturating_add(chunk.len()) > MAX_RESPONSE_BYTES {
                return Err(response_bound_failure(effect));
            }
            bytes.extend_from_slice(&chunk);
        }
        let status = response.status();
        let value = if !status.is_success() || bytes.is_empty() {
            Value::String(String::new())
        } else if json_content_type {
            serde_json::from_slice(&bytes).map_err(|_| response_shape_failure(effect))?
        } else {
            String::from_utf8(bytes)
                .map(Value::String)
                .map_err(|_| response_shape_failure(effect))?
        };
        Ok(ZephyrSquadHttpResponse { status, value })
    }
}

trait ZephyrSquadClock: Send + Sync {
    fn epoch_seconds(&self) -> Result<u64, ZephyrSquadClientError>;
}

struct SystemClock;

impl ZephyrSquadClock for SystemClock {
    fn epoch_seconds(&self) -> Result<u64, ZephyrSquadClientError> {
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|duration| duration.as_secs())
            .map_err(|_| invalid_configuration())
    }
}

pub(in crate::toolkits) struct ZephyrSquadClient {
    config: ZephyrSquadToolkitConfig,
    transport: Arc<dyn ZephyrSquadTransport>,
    clock: Arc<dyn ZephyrSquadClock>,
}

impl ZephyrSquadClient {
    pub(in crate::toolkits) fn new(
        config: ZephyrSquadToolkitConfig,
    ) -> Result<Self, ZephyrSquadClientError> {
        let http = reqwest::Client::builder()
            .connect_timeout(CONNECT_TIMEOUT)
            .timeout(REQUEST_TIMEOUT)
            .pool_idle_timeout(POOL_IDLE_TIMEOUT)
            .pool_max_idle_per_host(MAX_IDLE_PER_HOST)
            .redirect(reqwest::redirect::Policy::none())
            .retry(reqwest::retry::never())
            .build()
            .map_err(|_| invalid_configuration())?;
        Self::with_parts(
            config,
            Arc::new(ReqwestZephyrSquadTransport { http }),
            Arc::new(SystemClock),
        )
    }

    fn with_parts(
        config: ZephyrSquadToolkitConfig,
        transport: Arc<dyn ZephyrSquadTransport>,
        clock: Arc<dyn ZephyrSquadClock>,
    ) -> Result<Self, ZephyrSquadClientError> {
        Url::parse(API_ROOT).map_err(|_| invalid_configuration())?;
        Ok(Self {
            config,
            transport,
            clock,
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn fixture(
        config: ZephyrSquadToolkitConfig,
        transport: Arc<dyn ZephyrSquadTransport>,
        epoch_seconds: u64,
    ) -> Result<Self, ZephyrSquadClientError> {
        struct FixedClock(u64);
        impl ZephyrSquadClock for FixedClock {
            fn epoch_seconds(&self) -> Result<u64, ZephyrSquadClientError> {
                Ok(self.0)
            }
        }
        Self::with_parts(config, transport, Arc::new(FixedClock(epoch_seconds)))
    }

    async fn call(
        &self,
        method: Method,
        api_path: &str,
        body: Option<&Value>,
        effect: bool,
    ) -> Result<Value, ZephyrSquadClientError> {
        let target = format!("{API_ROOT}{api_path}");
        let url = Url::parse(&target).map_err(|_| invalid_input())?;
        if url.as_str() != target {
            return Err(invalid_input());
        }
        let mut request = Request::new(method.clone(), url);
        let jwt = self.jwt(&method, api_path)?;
        let mut authorization =
            HeaderValue::from_str(&format!("JWT {jwt}")).map_err(|_| invalid_configuration())?;
        authorization.set_sensitive(true);
        let mut access_key =
            HeaderValue::from_str(self.config.access_key()).map_err(|_| invalid_configuration())?;
        access_key.set_sensitive(true);
        request.headers_mut().insert(AUTHORIZATION, authorization);
        request.headers_mut().insert(ZAPI_ACCESS_KEY, access_key);
        request.headers_mut().insert(
            ACCEPT,
            HeaderValue::from_static("application/json, text/plain"),
        );
        request
            .headers_mut()
            .insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
        if let Some(body) = body {
            let encoded = serde_json::to_vec(body).map_err(|_| invalid_input())?;
            if encoded.len() > MAX_REQUEST_BYTES {
                return Err(resource_exhausted(false));
            }
            request
                .headers_mut()
                .insert(CONTENT_TYPE, HeaderValue::from_static(JSON_CONTENT_TYPE));
            *request.body_mut() = Some(encoded.into());
        }
        let response = self.transport.execute(request, effect).await?;
        if !response.status.is_success() {
            return Err(status_error(response.status, effect));
        }
        if serde_json::to_vec(&response.value)
            .map_err(|_| response_shape_failure(effect))?
            .len()
            > MAX_OUTPUT_BYTES
        {
            return Err(response_bound_failure(effect));
        }
        Ok(response.value)
    }

    fn jwt(&self, method: &Method, api_path: &str) -> Result<String, ZephyrSquadClientError> {
        let now = self.clock.epoch_seconds()?;
        let expiry = now
            .checked_add(JWT_LIFETIME_SECONDS)
            .ok_or_else(invalid_configuration)?;
        let canonical = canonical_request(method, api_path);
        let qsh = ring::digest::digest(&ring::digest::SHA256, canonical.as_bytes());
        // The signed claims are written with sorted members whatever
        // `preserve_order` a build unifies, so the token is the SDK's byte
        // for byte (`crate::canonical`).
        let payload = crate::canonical::to_vec(&json!({
            "sub":self.config.account_id(),
            "qsh":hex_lower(qsh.as_ref()),
            "iss":self.config.access_key(),
            "exp":expiry,
            "iat":now
        }))
        .map_err(|_| invalid_configuration())?;
        let header = URL_SAFE_NO_PAD.encode(JWT_HEADER);
        let payload = URL_SAFE_NO_PAD.encode(payload);
        let signing_input = format!("{header}.{payload}");
        let key = hmac::Key::new(hmac::HMAC_SHA256, self.config.secret_key().as_bytes());
        let signature = URL_SAFE_NO_PAD.encode(hmac::sign(&key, signing_input.as_bytes()));
        Ok(format!("{signing_input}.{signature}"))
    }
}

#[async_trait]
impl ZephyrSquadApi for ZephyrSquadClient {
    async fn get_test_step(
        &self,
        issue_id: u64,
        step_id: &str,
        project_id: u64,
    ) -> Result<Value, ZephyrSquadClientError> {
        validate_path_segment(step_id)?;
        self.call(
            Method::GET,
            &format!("/public/rest/api/1.0/teststep/{issue_id}/{step_id}?projectId={project_id}"),
            None,
            false,
        )
        .await
    }

    async fn update_test_step(
        &self,
        issue_id: u64,
        step_id: &str,
        project_id: u64,
        body: &Value,
    ) -> Result<Value, ZephyrSquadClientError> {
        validate_path_segment(step_id)?;
        self.call(
            Method::PUT,
            &format!("/public/rest/api/1.0/teststep/{issue_id}/{step_id}?projectId={project_id}"),
            Some(body),
            true,
        )
        .await
    }

    async fn delete_test_step(
        &self,
        issue_id: u64,
        step_id: &str,
        project_id: u64,
    ) -> Result<Value, ZephyrSquadClientError> {
        validate_path_segment(step_id)?;
        self.call(
            Method::DELETE,
            &format!("/public/rest/api/1.0/teststep/{issue_id}/{step_id}?projectId={project_id}"),
            None,
            true,
        )
        .await
    }

    async fn create_new_test_step(
        &self,
        issue_id: u64,
        project_id: u64,
        body: &Value,
    ) -> Result<Value, ZephyrSquadClientError> {
        self.call(
            Method::POST,
            &format!("/public/rest/api/1.0/teststep/{issue_id}?projectId={project_id}"),
            Some(body),
            true,
        )
        .await
    }

    async fn get_all_test_steps(
        &self,
        issue_id: u64,
        project_id: u64,
    ) -> Result<Value, ZephyrSquadClientError> {
        self.call(
            Method::GET,
            &format!("/public/rest/api/2.0/teststep/{issue_id}?projectId={project_id}"),
            None,
            false,
        )
        .await
    }

    async fn get_all_test_step_statuses(&self) -> Result<Value, ZephyrSquadClientError> {
        self.call(
            Method::GET,
            "/public/rest/api/1.0/teststep/statuses",
            None,
            false,
        )
        .await
    }

    async fn get_bdd_content(&self, issue_id: u64) -> Result<Value, ZephyrSquadClientError> {
        self.call(
            Method::GET,
            &format!("/public/rest/api/1.0/integration/bddcontent/{issue_id}"),
            None,
            false,
        )
        .await
    }

    async fn update_bdd_content(
        &self,
        issue_id: u64,
        content: &str,
    ) -> Result<Value, ZephyrSquadClientError> {
        self.call(
            Method::POST,
            &format!("/public/rest/api/1.0/integration/bddcontent/{issue_id}"),
            Some(&json!({"content":content})),
            true,
        )
        .await
    }

    async fn delete_bdd_content(&self, issue_id: u64) -> Result<Value, ZephyrSquadClientError> {
        self.call(
            Method::DELETE,
            &format!("/public/rest/api/1.0/integration/bddcontent/{issue_id}"),
            Some(&json!([])),
            true,
        )
        .await
    }

    async fn create_new_cycle(&self, body: &Value) -> Result<Value, ZephyrSquadClientError> {
        self.call(Method::POST, "/public/rest/api/1.0/cycle", Some(body), true)
            .await
    }

    async fn create_folder(&self, body: &Value) -> Result<Value, ZephyrSquadClientError> {
        self.call(
            Method::POST,
            "/public/rest/api/1.0/folder",
            Some(body),
            true,
        )
        .await
    }

    async fn add_test_to_cycle(
        &self,
        cycle_id: &str,
        body: &Value,
    ) -> Result<Value, ZephyrSquadClientError> {
        validate_path_segment(cycle_id)?;
        self.call(
            Method::POST,
            &format!("/public/rest/api/1.0/executions/add/cycle/{cycle_id}"),
            Some(body),
            true,
        )
        .await
    }

    async fn add_test_to_folder(
        &self,
        folder_id: &str,
        body: &Value,
    ) -> Result<Value, ZephyrSquadClientError> {
        validate_path_segment(folder_id)?;
        self.call(
            Method::POST,
            &format!("/public/rest/api/1.0/executions/add/folder/{folder_id}"),
            Some(body),
            true,
        )
        .await
    }

    async fn create_execution(&self, body: &Value) -> Result<Value, ZephyrSquadClientError> {
        self.call(
            Method::POST,
            "/public/rest/api/1.0/execution",
            Some(body),
            true,
        )
        .await
    }

    async fn get_execution(
        &self,
        execution_id: &str,
        issue_id: u64,
        project_id: u64,
    ) -> Result<Value, ZephyrSquadClientError> {
        validate_path_segment(execution_id)?;
        self.call(
            Method::GET,
            &format!(
                "/public/rest/api/1.0/execution/{execution_id}?issueId={issue_id}&projectId={project_id}"
            ),
            None,
            false,
        )
        .await
    }
}

fn canonical_request(method: &Method, api_path: &str) -> String {
    let normalized = api_path.replace('?', "&");
    if api_path.contains(['&', '?']) {
        format!("{}&{normalized}", method.as_str())
    } else {
        format!("{}&{normalized}&", method.as_str())
    }
}

fn validate_path_segment(value: &str) -> Result<(), ZephyrSquadClientError> {
    if value.is_empty()
        || value.len() > 256
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
    {
        return Err(invalid_input());
    }
    Ok(())
}

fn hex_lower(value: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut result = String::with_capacity(value.len() * 2);
    for byte in value {
        result.push(char::from(HEX[usize::from(byte >> 4)]));
        result.push(char::from(HEX[usize::from(byte & 0x0f)]));
    }
    result
}

fn status_error(status: StatusCode, effect: bool) -> ZephyrSquadClientError {
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
            ZephyrSquadClientErrorCode::InvalidInput
        }
        StatusCode::UNAUTHORIZED => ZephyrSquadClientErrorCode::Authentication,
        StatusCode::FORBIDDEN => ZephyrSquadClientErrorCode::Authorization,
        StatusCode::NOT_FOUND => ZephyrSquadClientErrorCode::NotFound,
        StatusCode::CONFLICT => ZephyrSquadClientErrorCode::Conflict,
        StatusCode::REQUEST_TIMEOUT => ZephyrSquadClientErrorCode::Timeout,
        StatusCode::TOO_MANY_REQUESTS => ZephyrSquadClientErrorCode::RateLimited,
        status if status.is_server_error() => ZephyrSquadClientErrorCode::DependencyUnavailable,
        _ => ZephyrSquadClientErrorCode::InvalidResponse,
    };
    ZephyrSquadClientError {
        code,
        retryable: !effect
            && matches!(
                code,
                ZephyrSquadClientErrorCode::Timeout
                    | ZephyrSquadClientErrorCode::RateLimited
                    | ZephyrSquadClientErrorCode::DependencyUnavailable
            ),
    }
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> ZephyrSquadClientError {
    if effect {
        return unknown_outcome();
    }
    if source.is_timeout() {
        ZephyrSquadClientError {
            code: ZephyrSquadClientErrorCode::Timeout,
            retryable: true,
        }
    } else {
        ZephyrSquadClientError {
            code: ZephyrSquadClientErrorCode::DependencyUnavailable,
            retryable: true,
        }
    }
}

const fn invalid_configuration() -> ZephyrSquadClientError {
    ZephyrSquadClientError {
        code: ZephyrSquadClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

const fn invalid_input() -> ZephyrSquadClientError {
    ZephyrSquadClientError {
        code: ZephyrSquadClientErrorCode::InvalidInput,
        retryable: false,
    }
}

const fn resource_exhausted(effect: bool) -> ZephyrSquadClientError {
    if effect {
        unknown_outcome()
    } else {
        ZephyrSquadClientError {
            code: ZephyrSquadClientErrorCode::ResourceExhausted,
            retryable: false,
        }
    }
}

const fn response_bound_failure(effect: bool) -> ZephyrSquadClientError {
    resource_exhausted(effect)
}

const fn response_shape_failure(effect: bool) -> ZephyrSquadClientError {
    if effect {
        unknown_outcome()
    } else {
        ZephyrSquadClientError {
            code: ZephyrSquadClientErrorCode::InvalidResponse,
            retryable: false,
        }
    }
}

const fn unknown_outcome() -> ZephyrSquadClientError {
    ZephyrSquadClientError {
        code: ZephyrSquadClientErrorCode::UnknownOutcome,
        retryable: false,
    }
}
