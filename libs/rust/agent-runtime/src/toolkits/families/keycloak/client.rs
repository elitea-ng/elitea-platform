use std::fmt::{self, Write as _};
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use percent_encoding::{NON_ALPHANUMERIC, percent_decode_str, utf8_percent_encode};
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderValue, USER_AGENT,
};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value};
use tokio::time::timeout;
use zeroize::Zeroizing;

use super::config::KeycloakToolkitConfig;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const TOKEN_TIMEOUT: Duration = Duration::from_secs(10);
const ADMIN_TIMEOUT: Duration = Duration::from_secs(20);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_RELATIVE_URL_BYTES: usize = 4 * 1_024;
const MAX_REQUEST_BYTES: usize = 256 * 1_024;
const MAX_TOKEN_RESPONSE_BYTES: usize = 64 * 1_024;
const MAX_ADMIN_RESPONSE_BYTES: usize = 512 * 1_024;
const MAX_ACCESS_TOKEN_BYTES: usize = 32 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";
const FORM_CONTENT_TYPE: &str = "application/x-www-form-urlencoded";
const JSON_CONTENT_TYPE: &str = "application/json";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum KeycloakClientErrorCode {
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

/// Stable Keycloak failure without authority, realm, identifiers, bodies, or credentials.
pub(crate) struct KeycloakClientError {
    code: KeycloakClientErrorCode,
    retryable: bool,
}

impl KeycloakClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> KeycloakClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            KeycloakClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "keycloak.configuration.invalid",
                "the Keycloak toolkit configuration is invalid",
            ),
            KeycloakClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "keycloak.request.invalid",
                "the Keycloak Admin API request is invalid",
            ),
            KeycloakClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "keycloak.authentication.failed",
                "Keycloak service-account authentication failed",
            ),
            KeycloakClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "keycloak.authorization.failed",
                "Keycloak did not authorize the Admin API request",
            ),
            KeycloakClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "keycloak.resource.not_found",
                "the requested Keycloak resource was not found",
            ),
            KeycloakClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "keycloak.rate_limited",
                "Keycloak rate limited the request",
            ),
            KeycloakClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "keycloak.timeout",
                "the Keycloak request timed out before an effect was dispatched",
            ),
            KeycloakClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "keycloak.unavailable",
                "Keycloak is unavailable",
            ),
            KeycloakClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "keycloak.response.invalid",
                "Keycloak returned an invalid response",
            ),
            KeycloakClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "keycloak.resource_exhausted",
                "the Keycloak request or response exceeds the approved limit",
            ),
            KeycloakClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "keycloak.effect.unknown_outcome",
                "the Keycloak effect may have occurred; reconcile state before retrying",
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
        code: KeycloakClientErrorCode,
        retryable: bool,
    ) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for KeycloakClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("KeycloakClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for KeycloakClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            KeycloakClientErrorCode::InvalidConfiguration => {
                "the Keycloak client configuration is invalid"
            }
            KeycloakClientErrorCode::InvalidInput => "the Keycloak request is invalid",
            KeycloakClientErrorCode::Authentication => "Keycloak authentication failed",
            KeycloakClientErrorCode::Authorization => "Keycloak authorization failed",
            KeycloakClientErrorCode::NotFound => "the Keycloak resource was not found",
            KeycloakClientErrorCode::RateLimited => "Keycloak rate limited the request",
            KeycloakClientErrorCode::Timeout => "the Keycloak request timed out",
            KeycloakClientErrorCode::DependencyUnavailable => "Keycloak is unavailable",
            KeycloakClientErrorCode::InvalidResponse => "Keycloak returned an invalid response",
            KeycloakClientErrorCode::ResourceExhausted => {
                "the Keycloak request or response exceeds its approved limit"
            }
            KeycloakClientErrorCode::UnknownOutcome => {
                "the Keycloak effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for KeycloakClientError {}

#[async_trait]
pub(in crate::toolkits) trait KeycloakApi: Send + Sync {
    async fn execute(
        &self,
        method: &str,
        relative_url: &str,
        params: &Map<String, Value>,
    ) -> Result<Value, KeycloakClientError>;
}

pub(in crate::toolkits) struct KeycloakHttpResponse {
    status: StatusCode,
    body: Vec<u8>,
}

impl KeycloakHttpResponse {
    #[cfg(test)]
    pub(in crate::toolkits) fn fixture(status: StatusCode, body: impl Into<Vec<u8>>) -> Self {
        Self {
            status,
            body: body.into(),
        }
    }
}

#[async_trait]
pub(in crate::toolkits) trait KeycloakTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
        response_limit: usize,
    ) -> Result<KeycloakHttpResponse, KeycloakClientError>;
}

struct ReqwestKeycloakTransport {
    http: reqwest::Client,
}

#[async_trait]
impl KeycloakTransport for ReqwestKeycloakTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
        response_limit: usize,
    ) -> Result<KeycloakHttpResponse, KeycloakClientError> {
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
            .is_some_and(|length| length > response_limit)
        {
            return Err(response_bound_failure(effect));
        }
        let status = response.status();
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
            if next > response_limit {
                return Err(response_bound_failure(effect));
            }
            body.extend_from_slice(&chunk);
        }
        Ok(KeycloakHttpResponse { status, body })
    }
}

/// Invocation-owned Keycloak service-account client and fixed realm authority.
pub(crate) struct KeycloakClient {
    config: KeycloakToolkitConfig,
    transport: Arc<dyn KeycloakTransport>,
}

impl KeycloakClient {
    pub(crate) fn new(config: KeycloakToolkitConfig) -> Result<Self, KeycloakClientError> {
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
            transport: Arc::new(ReqwestKeycloakTransport { http }),
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn with_transport(
        config: KeycloakToolkitConfig,
        transport: Arc<dyn KeycloakTransport>,
    ) -> Self {
        Self { config, transport }
    }

    fn token_url(&self) -> Result<Url, KeycloakClientError> {
        Url::parse(&format!(
            "{}/realms/{}/protocol/openid-connect/token",
            self.config.base_url(),
            utf8_percent_encode(self.config.realm(), NON_ALPHANUMERIC)
        ))
        .map_err(|_| invalid_configuration())
    }

    fn admin_url(&self, relative_url: &str) -> Result<Url, KeycloakClientError> {
        validate_relative_url(relative_url)?;
        Url::parse(&format!(
            "{}/admin/realms/{}{}",
            self.config.base_url(),
            utf8_percent_encode(self.config.realm(), NON_ALPHANUMERIC),
            relative_url
        ))
        .map_err(|_| invalid_input())
    }

    fn token_request(&self) -> Result<Request, KeycloakClientError> {
        let mut body = Zeroizing::new(String::new());
        body.push_str("client_id=");
        write!(
            body,
            "{}",
            utf8_percent_encode(self.config.client_id(), NON_ALPHANUMERIC)
        )
        .map_err(|_| invalid_configuration())?;
        body.push_str("&client_secret=");
        write!(
            body,
            "{}",
            utf8_percent_encode(self.config.client_secret(), NON_ALPHANUMERIC)
        )
        .map_err(|_| invalid_configuration())?;
        body.push_str("&grant_type=client_credentials");
        if body.len() > MAX_REQUEST_BYTES {
            return Err(resource_exhausted());
        }
        request_with_body(
            Method::POST,
            self.token_url()?,
            body.as_bytes(),
            FORM_CONTENT_TYPE,
            None,
        )
    }

    fn admin_request(
        &self,
        method: Method,
        relative_url: &str,
        params: &Map<String, Value>,
        token: &str,
    ) -> Result<Request, KeycloakClientError> {
        let body = serde_json::to_vec(params).map_err(|_| invalid_input())?;
        if body.len() > MAX_REQUEST_BYTES {
            return Err(resource_exhausted());
        }
        request_with_body(
            method,
            self.admin_url(relative_url)?,
            &body,
            JSON_CONTENT_TYPE,
            Some(bearer_header(token)?),
        )
    }

    async fn access_token(&self) -> Result<Zeroizing<String>, KeycloakClientError> {
        let response = timeout(
            TOKEN_TIMEOUT,
            self.transport
                .execute(self.token_request()?, false, MAX_TOKEN_RESPONSE_BYTES),
        )
        .await
        .map_err(|_| timeout_error(true))??;
        let status = response.status;
        let body_bytes = Zeroizing::new(response.body);
        map_token_status(status)?;
        let mut body: Value =
            serde_json::from_slice(&body_bytes).map_err(|_| invalid_response())?;
        let token = body
            .as_object_mut()
            .and_then(|body| body.remove("access_token"))
            .and_then(|value| match value {
                Value::String(token) => Some(token),
                _ => None,
            })
            .ok_or_else(invalid_response)?;
        if token.is_empty()
            || token.len() > MAX_ACCESS_TOKEN_BYTES
            || token.bytes().any(|byte| byte.is_ascii_control())
        {
            return Err(if token.len() > MAX_ACCESS_TOKEN_BYTES {
                resource_exhausted()
            } else {
                invalid_response()
            });
        }
        Ok(Zeroizing::new(token))
    }

    async fn execute_inner(
        &self,
        method: &str,
        relative_url: &str,
        params: &Map<String, Value>,
    ) -> Result<Value, KeycloakClientError> {
        let method = parse_method(method)?;
        let effect = is_effect(&method);
        let token = self.access_token().await?;
        let request = self.admin_request(method, relative_url, params, &token)?;
        let response = timeout(
            ADMIN_TIMEOUT,
            self.transport
                .execute(request, effect, MAX_ADMIN_RESPONSE_BYTES),
        )
        .await
        .map_err(|_| {
            if effect {
                unknown_outcome()
            } else {
                timeout_error(true)
            }
        })??;
        if !response.status.is_success() {
            return Err(map_admin_status(response.status, effect));
        }
        let body = String::from_utf8(response.body).map_err(|_| post_accept_failure(effect))?;
        let result = Value::String(body);
        if serde_json::to_vec(&result)
            .map_err(|_| post_accept_failure(effect))?
            .len()
            > MAX_OUTPUT_BYTES
        {
            return Err(post_accept_bound_failure(effect));
        }
        Ok(result)
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_token_request(&self) -> Result<Request, KeycloakClientError> {
        self.token_request()
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_admin_request(
        &self,
        method: &str,
        relative_url: &str,
        params: &Map<String, Value>,
        token: &str,
    ) -> Result<Request, KeycloakClientError> {
        self.admin_request(parse_method(method)?, relative_url, params, token)
    }
}

#[async_trait]
impl KeycloakApi for KeycloakClient {
    async fn execute(
        &self,
        method: &str,
        relative_url: &str,
        params: &Map<String, Value>,
    ) -> Result<Value, KeycloakClientError> {
        self.execute_inner(method, relative_url, params).await
    }
}

fn request_with_body(
    method: Method,
    url: Url,
    body: &[u8],
    content_type: &'static str,
    authorization: Option<HeaderValue>,
) -> Result<Request, KeycloakClientError> {
    let mut request = Request::new(method, url);
    request
        .headers_mut()
        .insert(ACCEPT, HeaderValue::from_static(JSON_CONTENT_TYPE));
    request
        .headers_mut()
        .insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
    request
        .headers_mut()
        .insert(CONTENT_TYPE, HeaderValue::from_static(content_type));
    request.headers_mut().insert(
        CONTENT_LENGTH,
        HeaderValue::from_str(&body.len().to_string()).map_err(|_| invalid_input())?,
    );
    if let Some(authorization) = authorization {
        request.headers_mut().insert(AUTHORIZATION, authorization);
    }
    *request.body_mut() = Some(body.to_vec().into());
    Ok(request)
}

fn bearer_header(token: &str) -> Result<HeaderValue, KeycloakClientError> {
    let mut value = Zeroizing::new(String::with_capacity("Bearer ".len() + token.len()));
    value.push_str("Bearer ");
    value.push_str(token);
    HeaderValue::from_str(&value).map_err(|_| invalid_response())
}

pub(in crate::toolkits) fn parse_method(method: &str) -> Result<Method, KeycloakClientError> {
    let method = method.trim();
    if method.is_empty() || method.len() > 32 || !method.bytes().all(is_http_token_byte) {
        return Err(invalid_input());
    }
    Method::from_bytes(method.to_ascii_uppercase().as_bytes()).map_err(|_| invalid_input())
}

fn is_http_token_byte(byte: u8) -> bool {
    byte.is_ascii_alphanumeric()
        || matches!(
            byte,
            b'!' | b'#'
                | b'$'
                | b'%'
                | b'&'
                | b'\''
                | b'*'
                | b'+'
                | b'-'
                | b'.'
                | b'^'
                | b'_'
                | b'`'
                | b'|'
                | b'~'
        )
}

fn is_effect(method: &Method) -> bool {
    !matches!(*method, Method::GET | Method::HEAD | Method::OPTIONS)
}

pub(in crate::toolkits) fn validate_relative_url(
    relative_url: &str,
) -> Result<(), KeycloakClientError> {
    if relative_url.is_empty()
        || relative_url.len() > MAX_RELATIVE_URL_BYTES
        || !relative_url.starts_with('/')
        || relative_url.starts_with("//")
        || relative_url.contains('#')
        || relative_url
            .bytes()
            .any(|byte| byte.is_ascii_control() || byte == b'\\')
    {
        return Err(if relative_url.len() > MAX_RELATIVE_URL_BYTES {
            resource_exhausted()
        } else {
            invalid_input()
        });
    }
    validate_percent_encoding(relative_url)?;
    let path = relative_url
        .split_once('?')
        .map_or(relative_url, |(path, _)| path);
    for segment in path.split('/') {
        let decoded = percent_decode_str(segment)
            .decode_utf8()
            .map_err(|_| invalid_input())?;
        if matches!(decoded.as_ref(), "." | "..")
            || decoded
                .bytes()
                .any(|byte| byte.is_ascii_control() || matches!(byte, b'/' | b'\\'))
            || contains_percent_escape(&decoded)
        {
            return Err(invalid_input());
        }
    }
    Ok(())
}

fn validate_percent_encoding(value: &str) -> Result<(), KeycloakClientError> {
    let bytes = value.as_bytes();
    let mut index = 0usize;
    while index < bytes.len() {
        if bytes[index] == b'%'
            && (index + 2 >= bytes.len()
                || !bytes[index + 1].is_ascii_hexdigit()
                || !bytes[index + 2].is_ascii_hexdigit())
        {
            return Err(invalid_input());
        }
        index += if bytes[index] == b'%' { 3 } else { 1 };
    }
    Ok(())
}

fn contains_percent_escape(value: &str) -> bool {
    value
        .as_bytes()
        .windows(3)
        .any(|window| window[0] == b'%' && window[1..].iter().all(u8::is_ascii_hexdigit))
}

fn map_token_status(status: StatusCode) -> Result<(), KeycloakClientError> {
    if status == StatusCode::OK {
        return Ok(());
    }
    Err(match status {
        StatusCode::BAD_REQUEST | StatusCode::UNAUTHORIZED => authentication(),
        StatusCode::FORBIDDEN => authorization(),
        StatusCode::TOO_MANY_REQUESTS => rate_limited(true),
        StatusCode::REQUEST_TIMEOUT | StatusCode::GATEWAY_TIMEOUT => timeout_error(true),
        status if status.is_server_error() => unavailable(true),
        _ => invalid_response(),
    })
}

fn map_admin_status(status: StatusCode, effect: bool) -> KeycloakClientError {
    match status {
        StatusCode::BAD_REQUEST | StatusCode::CONFLICT | StatusCode::UNPROCESSABLE_ENTITY => {
            invalid_input()
        }
        StatusCode::UNAUTHORIZED => authentication(),
        StatusCode::FORBIDDEN => authorization(),
        StatusCode::NOT_FOUND => not_found(),
        StatusCode::TOO_MANY_REQUESTS
        | StatusCode::REQUEST_TIMEOUT
        | StatusCode::GATEWAY_TIMEOUT
            if effect =>
        {
            unknown_outcome()
        }
        StatusCode::TOO_MANY_REQUESTS => rate_limited(true),
        StatusCode::REQUEST_TIMEOUT | StatusCode::GATEWAY_TIMEOUT => timeout_error(true),
        status if status.is_server_error() && effect => unknown_outcome(),
        status if status.is_server_error() => unavailable(true),
        _ if effect => unknown_outcome(),
        _ => invalid_response(),
    }
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> KeycloakClientError {
    if effect {
        unknown_outcome()
    } else if source.is_timeout() {
        timeout_error(true)
    } else {
        unavailable(true)
    }
}

const fn response_bound_failure(effect: bool) -> KeycloakClientError {
    if effect {
        unknown_outcome()
    } else {
        resource_exhausted()
    }
}

const fn post_accept_failure(effect: bool) -> KeycloakClientError {
    if effect {
        unknown_outcome()
    } else {
        invalid_response()
    }
}

const fn post_accept_bound_failure(effect: bool) -> KeycloakClientError {
    if effect {
        unknown_outcome()
    } else {
        resource_exhausted()
    }
}

const fn invalid_configuration() -> KeycloakClientError {
    KeycloakClientError {
        code: KeycloakClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

const fn invalid_input() -> KeycloakClientError {
    KeycloakClientError {
        code: KeycloakClientErrorCode::InvalidInput,
        retryable: false,
    }
}

const fn authentication() -> KeycloakClientError {
    KeycloakClientError {
        code: KeycloakClientErrorCode::Authentication,
        retryable: false,
    }
}

const fn authorization() -> KeycloakClientError {
    KeycloakClientError {
        code: KeycloakClientErrorCode::Authorization,
        retryable: false,
    }
}

const fn not_found() -> KeycloakClientError {
    KeycloakClientError {
        code: KeycloakClientErrorCode::NotFound,
        retryable: false,
    }
}

const fn rate_limited(retryable: bool) -> KeycloakClientError {
    KeycloakClientError {
        code: KeycloakClientErrorCode::RateLimited,
        retryable,
    }
}

const fn timeout_error(retryable: bool) -> KeycloakClientError {
    KeycloakClientError {
        code: KeycloakClientErrorCode::Timeout,
        retryable,
    }
}

const fn unavailable(retryable: bool) -> KeycloakClientError {
    KeycloakClientError {
        code: KeycloakClientErrorCode::DependencyUnavailable,
        retryable,
    }
}

const fn invalid_response() -> KeycloakClientError {
    KeycloakClientError {
        code: KeycloakClientErrorCode::InvalidResponse,
        retryable: false,
    }
}

const fn resource_exhausted() -> KeycloakClientError {
    KeycloakClientError {
        code: KeycloakClientErrorCode::ResourceExhausted,
        retryable: false,
    }
}

const fn unknown_outcome() -> KeycloakClientError {
    KeycloakClientError {
        code: KeycloakClientErrorCode::UnknownOutcome,
        retryable: false,
    }
}

#[cfg(test)]
pub(in crate::toolkits) fn test_map_admin_status(
    status: StatusCode,
    effect: bool,
) -> KeycloakClientError {
    map_admin_status(status, effect)
}
