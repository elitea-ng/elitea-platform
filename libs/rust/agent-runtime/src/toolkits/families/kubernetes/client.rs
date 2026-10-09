use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use percent_encoding::percent_decode_str;
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderName, HeaderValue, USER_AGENT,
};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value, json};
use tokio::time::timeout;
use zeroize::Zeroizing;

use super::config::KubernetesToolkitConfig;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const API_TIMEOUT: Duration = Duration::from_secs(20);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_SUBURL_BYTES: usize = 8 * 1_024;
const MAX_REQUEST_BYTES: usize = 256 * 1_024;
const MAX_BODY_BYTES: usize = 240 * 1_024;
const MAX_RESPONSE_BYTES: usize = 512 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_HEADERS: usize = 64;
const MAX_HEADER_NAME_BYTES: usize = 128;
const MAX_HEADER_VALUE_BYTES: usize = 8 * 1_024;
const MAX_HEADER_BYTES: usize = 32 * 1_024;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";
const JSON_CONTENT_TYPE: &str = "application/json";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum KubernetesClientErrorCode {
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

/// Stable Kubernetes failure without origin, token, path, headers, or body data.
pub(crate) struct KubernetesClientError {
    code: KubernetesClientErrorCode,
    retryable: bool,
}

impl KubernetesClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> KubernetesClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            KubernetesClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "kubernetes.configuration.invalid",
                "the Kubernetes toolkit configuration is invalid",
            ),
            KubernetesClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "kubernetes.request.invalid",
                "the Kubernetes API request is invalid",
            ),
            KubernetesClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "kubernetes.authentication.failed",
                "Kubernetes authentication failed",
            ),
            KubernetesClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "kubernetes.authorization.failed",
                "Kubernetes did not authorize the API request",
            ),
            KubernetesClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "kubernetes.resource.not_found",
                "the requested Kubernetes resource was not found",
            ),
            KubernetesClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "kubernetes.rate_limited",
                "Kubernetes rate limited the request",
            ),
            KubernetesClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "kubernetes.timeout",
                "the Kubernetes request timed out before an effect was dispatched",
            ),
            KubernetesClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "kubernetes.unavailable",
                "the Kubernetes API is unavailable",
            ),
            KubernetesClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "kubernetes.response.invalid",
                "Kubernetes returned an invalid response",
            ),
            KubernetesClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "kubernetes.resource_exhausted",
                "the Kubernetes request or response exceeds the approved limit",
            ),
            KubernetesClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "kubernetes.effect.unknown_outcome",
                "the Kubernetes effect may have occurred; reconcile cluster state before retrying",
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
        code: KubernetesClientErrorCode,
        retryable: bool,
    ) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for KubernetesClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("KubernetesClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for KubernetesClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            KubernetesClientErrorCode::InvalidConfiguration => {
                "the Kubernetes client configuration is invalid"
            }
            KubernetesClientErrorCode::InvalidInput => "the Kubernetes request is invalid",
            KubernetesClientErrorCode::Authentication => "Kubernetes authentication failed",
            KubernetesClientErrorCode::Authorization => "Kubernetes authorization failed",
            KubernetesClientErrorCode::NotFound => "the Kubernetes resource was not found",
            KubernetesClientErrorCode::RateLimited => "Kubernetes rate limited the request",
            KubernetesClientErrorCode::Timeout => "the Kubernetes request timed out",
            KubernetesClientErrorCode::DependencyUnavailable => "the Kubernetes API is unavailable",
            KubernetesClientErrorCode::InvalidResponse => "Kubernetes returned an invalid response",
            KubernetesClientErrorCode::ResourceExhausted => {
                "the Kubernetes request or response exceeds its approved limit"
            }
            KubernetesClientErrorCode::UnknownOutcome => {
                "the Kubernetes effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for KubernetesClientError {}

#[async_trait]
pub(in crate::toolkits) trait KubernetesApi: Send + Sync {
    async fn execute(
        &self,
        method: &str,
        suburl: &str,
        body: Option<&Map<String, Value>>,
        headers: &Map<String, Value>,
    ) -> Result<Value, KubernetesClientError>;

    async fn healthcheck(&self) -> Value;
}

pub(in crate::toolkits) struct KubernetesHttpResponse {
    status: StatusCode,
    body: Vec<u8>,
}

impl KubernetesHttpResponse {
    #[cfg(test)]
    pub(in crate::toolkits) fn fixture(status: StatusCode, body: impl Into<Vec<u8>>) -> Self {
        Self {
            status,
            body: body.into(),
        }
    }
}

#[async_trait]
pub(in crate::toolkits) trait KubernetesTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
        response_limit: usize,
    ) -> Result<KubernetesHttpResponse, KubernetesClientError>;
}

struct ReqwestKubernetesTransport {
    http: reqwest::Client,
}

#[async_trait]
impl KubernetesTransport for ReqwestKubernetesTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
        response_limit: usize,
    ) -> Result<KubernetesHttpResponse, KubernetesClientError> {
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
        Ok(KubernetesHttpResponse { status, body })
    }
}

/// Invocation-owned Kubernetes Bearer client and exact cluster origin.
pub(crate) struct KubernetesClient {
    config: KubernetesToolkitConfig,
    transport: Arc<dyn KubernetesTransport>,
}

impl KubernetesClient {
    pub(crate) fn new(config: KubernetesToolkitConfig) -> Result<Self, KubernetesClientError> {
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
            transport: Arc::new(ReqwestKubernetesTransport { http }),
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn with_transport(
        config: KubernetesToolkitConfig,
        transport: Arc<dyn KubernetesTransport>,
    ) -> Self {
        Self { config, transport }
    }

    fn request(
        &self,
        method: Method,
        suburl: &str,
        body: Option<&Map<String, Value>>,
        headers: &Map<String, Value>,
    ) -> Result<Request, KubernetesClientError> {
        let url = validate_suburl(self.config.cluster_url(), suburl)?;
        let body = body
            .map(serde_json::to_vec)
            .transpose()
            .map_err(|_| invalid_input())?;
        if body
            .as_ref()
            .is_some_and(|body| body.len() > MAX_BODY_BYTES)
        {
            return Err(resource_exhausted());
        }
        let mut request = Request::new(method, url);
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static(JSON_CONTENT_TYPE));
        request
            .headers_mut()
            .insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
        request
            .headers_mut()
            .insert(AUTHORIZATION, bearer_header(self.config.token())?);
        apply_headers(request.headers_mut(), headers)?;
        if let Some(body) = body {
            if !request.headers().contains_key(CONTENT_TYPE) {
                request
                    .headers_mut()
                    .insert(CONTENT_TYPE, HeaderValue::from_static(JSON_CONTENT_TYPE));
            }
            request.headers_mut().insert(
                CONTENT_LENGTH,
                HeaderValue::from_str(&body.len().to_string()).map_err(|_| invalid_input())?,
            );
            *request.body_mut() = Some(body.into());
        }
        let encoded = request
            .body()
            .and_then(reqwest::Body::as_bytes)
            .map_or(0, <[u8]>::len);
        let request_size = request
            .url()
            .as_str()
            .len()
            .checked_add(encoded)
            .and_then(|size| size.checked_add(header_size(request.headers())))
            .ok_or_else(resource_exhausted)?;
        if request_size > MAX_REQUEST_BYTES {
            return Err(resource_exhausted());
        }
        Ok(request)
    }

    async fn execute_inner(
        &self,
        method: &str,
        suburl: &str,
        body: Option<&Map<String, Value>>,
        headers: &Map<String, Value>,
    ) -> Result<Value, KubernetesClientError> {
        let method = parse_method(method)?;
        let effect = is_effect(&method);
        let request = self.request(method, suburl, body, headers)?;
        let response = timeout(
            API_TIMEOUT,
            self.transport.execute(request, effect, MAX_RESPONSE_BYTES),
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
            return Err(map_status(response.status, effect));
        }
        let body = String::from_utf8(response.body).map_err(|_| post_accept_failure(effect))?;
        let result = Value::String(body);
        let size = serde_json::to_vec(&result)
            .map_err(|_| post_accept_failure(effect))?
            .len();
        if size > MAX_OUTPUT_BYTES {
            return Err(post_accept_bound_failure(effect));
        }
        Ok(result)
    }

    async fn healthcheck_inner(&self) -> Result<(), KubernetesClientError> {
        let empty_headers = Map::new();
        let response = self
            .execute_inner("GET", "/version", None, &empty_headers)
            .await?;
        let body = response.as_str().ok_or_else(invalid_response)?;
        serde_json::from_str::<Value>(body).map_err(|_| invalid_response())?;
        Ok(())
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_request(
        &self,
        method: &str,
        suburl: &str,
        body: Option<&Map<String, Value>>,
        headers: &Map<String, Value>,
    ) -> Result<Request, KubernetesClientError> {
        self.request(parse_method(method)?, suburl, body, headers)
    }
}

#[async_trait]
impl KubernetesApi for KubernetesClient {
    async fn execute(
        &self,
        method: &str,
        suburl: &str,
        body: Option<&Map<String, Value>>,
        headers: &Map<String, Value>,
    ) -> Result<Value, KubernetesClientError> {
        self.execute_inner(method, suburl, body, headers).await
    }

    async fn healthcheck(&self) -> Value {
        match self.healthcheck_inner().await {
            Ok(()) => json!([true, ""]),
            Err(error) => json!([false, health_reason(error.code())]),
        }
    }
}

fn health_reason(code: KubernetesClientErrorCode) -> &'static str {
    match code {
        KubernetesClientErrorCode::Authentication => "authentication failed",
        KubernetesClientErrorCode::Authorization => "authorization failed",
        KubernetesClientErrorCode::NotFound => "version endpoint not found",
        KubernetesClientErrorCode::RateLimited => "rate limited",
        KubernetesClientErrorCode::Timeout => "request timed out",
        KubernetesClientErrorCode::DependencyUnavailable => "cluster unavailable",
        KubernetesClientErrorCode::ResourceExhausted => "response limit exceeded",
        KubernetesClientErrorCode::InvalidConfiguration
        | KubernetesClientErrorCode::InvalidInput
        | KubernetesClientErrorCode::InvalidResponse
        | KubernetesClientErrorCode::UnknownOutcome => "invalid cluster response",
    }
}

fn bearer_header(token: &str) -> Result<HeaderValue, KubernetesClientError> {
    let mut value = Zeroizing::new(String::with_capacity("Bearer ".len() + token.len()));
    value.push_str("Bearer ");
    value.push_str(token);
    HeaderValue::from_str(&value).map_err(|_| invalid_configuration())
}

fn apply_headers(
    target: &mut reqwest::header::HeaderMap,
    headers: &Map<String, Value>,
) -> Result<(), KubernetesClientError> {
    if headers.len() > MAX_HEADERS {
        return Err(resource_exhausted());
    }
    let mut total = 0usize;
    for (name, value) in headers {
        let value = value.as_str().ok_or_else(invalid_input)?;
        if name.is_empty()
            || name.len() > MAX_HEADER_NAME_BYTES
            || value.len() > MAX_HEADER_VALUE_BYTES
            || forbidden_header(name)
        {
            return Err(
                if name.len() > MAX_HEADER_NAME_BYTES || value.len() > MAX_HEADER_VALUE_BYTES {
                    resource_exhausted()
                } else {
                    invalid_input()
                },
            );
        }
        total = total
            .checked_add(name.len())
            .and_then(|size| size.checked_add(value.len()))
            .ok_or_else(resource_exhausted)?;
        if total > MAX_HEADER_BYTES {
            return Err(resource_exhausted());
        }
        let name = HeaderName::from_bytes(name.as_bytes()).map_err(|_| invalid_input())?;
        let value = HeaderValue::from_str(value).map_err(|_| invalid_input())?;
        target.insert(name, value);
    }
    Ok(())
}

fn forbidden_header(name: &str) -> bool {
    matches!(
        name.to_ascii_lowercase().as_str(),
        "authorization"
            | "host"
            | "content-length"
            | "transfer-encoding"
            | "connection"
            | "proxy-authorization"
            | "proxy-connection"
            | "te"
            | "trailer"
            | "upgrade"
    )
}

fn header_size(headers: &reqwest::header::HeaderMap) -> usize {
    headers.iter().fold(0usize, |size, (name, value)| {
        size.saturating_add(name.as_str().len())
            .saturating_add(value.as_bytes().len())
    })
}

pub(in crate::toolkits) fn parse_method(method: &str) -> Result<Method, KubernetesClientError> {
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

pub(in crate::toolkits) fn validate_suburl(
    cluster_url: &str,
    suburl: &str,
) -> Result<Url, KubernetesClientError> {
    if suburl.is_empty()
        || suburl.len() > MAX_SUBURL_BYTES
        || !suburl.starts_with('/')
        || suburl.starts_with("//")
        || suburl.contains('#')
        || suburl.contains('\\')
        || suburl.bytes().any(|byte| byte.is_ascii_control())
        || !valid_percent_encoding(suburl)
    {
        return Err(if suburl.len() > MAX_SUBURL_BYTES {
            resource_exhausted()
        } else {
            invalid_input()
        });
    }
    let path = suburl.split_once('?').map_or(suburl, |(path, _)| path);
    for segment in path.split('/').skip(1) {
        if segment.is_empty() && path != "/" {
            return Err(invalid_input());
        }
        let decoded = percent_decode_str(segment)
            .decode_utf8()
            .map_err(|_| invalid_input())?;
        if matches!(decoded.as_ref(), "." | "..")
            || decoded.contains('/')
            || decoded.contains('\\')
            || contains_percent_escape(decoded.as_bytes())
            || decoded.bytes().any(|byte| byte.is_ascii_control())
        {
            return Err(invalid_input());
        }
    }
    let expected = Url::parse(cluster_url).map_err(|_| invalid_configuration())?;
    let url = Url::parse(&format!("{cluster_url}{suburl}")).map_err(|_| invalid_input())?;
    if url.scheme() != expected.scheme()
        || url.host_str() != expected.host_str()
        || url.port_or_known_default() != expected.port_or_known_default()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.fragment().is_some()
    {
        return Err(invalid_input());
    }
    Ok(url)
}

fn valid_percent_encoding(value: &str) -> bool {
    let bytes = value.as_bytes();
    let mut index = 0usize;
    while index < bytes.len() {
        if bytes[index] == b'%' {
            if index + 2 >= bytes.len()
                || !bytes[index + 1].is_ascii_hexdigit()
                || !bytes[index + 2].is_ascii_hexdigit()
            {
                return false;
            }
            index += 3;
        } else {
            index += 1;
        }
    }
    true
}

fn contains_percent_escape(value: &[u8]) -> bool {
    value.windows(3).any(|window| {
        window[0] == b'%' && window[1].is_ascii_hexdigit() && window[2].is_ascii_hexdigit()
    })
}

fn map_status(status: StatusCode, effect: bool) -> KubernetesClientError {
    if effect && (status == StatusCode::REQUEST_TIMEOUT || status == StatusCode::TOO_MANY_REQUESTS)
    {
        return unknown_outcome();
    }
    if effect && (status.is_server_error() || status.is_redirection()) {
        return unknown_outcome();
    }
    match status {
        StatusCode::UNAUTHORIZED => authentication(),
        StatusCode::FORBIDDEN => authorization(),
        StatusCode::NOT_FOUND => not_found(),
        StatusCode::REQUEST_TIMEOUT => timeout_error(true),
        StatusCode::TOO_MANY_REQUESTS => rate_limited(),
        _ if status.is_server_error() => dependency_unavailable(),
        _ => invalid_response(),
    }
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> KubernetesClientError {
    if effect {
        return unknown_outcome();
    }
    if source.is_timeout() {
        timeout_error(true)
    } else if source.is_connect() {
        dependency_unavailable()
    } else {
        invalid_response()
    }
}

fn post_accept_failure(effect: bool) -> KubernetesClientError {
    if effect {
        unknown_outcome()
    } else {
        invalid_response()
    }
}

fn post_accept_bound_failure(effect: bool) -> KubernetesClientError {
    if effect {
        unknown_outcome()
    } else {
        resource_exhausted()
    }
}

fn response_bound_failure(effect: bool) -> KubernetesClientError {
    post_accept_bound_failure(effect)
}

const fn invalid_configuration() -> KubernetesClientError {
    KubernetesClientError {
        code: KubernetesClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

const fn invalid_input() -> KubernetesClientError {
    KubernetesClientError {
        code: KubernetesClientErrorCode::InvalidInput,
        retryable: false,
    }
}

const fn authentication() -> KubernetesClientError {
    KubernetesClientError {
        code: KubernetesClientErrorCode::Authentication,
        retryable: false,
    }
}

const fn authorization() -> KubernetesClientError {
    KubernetesClientError {
        code: KubernetesClientErrorCode::Authorization,
        retryable: false,
    }
}

const fn not_found() -> KubernetesClientError {
    KubernetesClientError {
        code: KubernetesClientErrorCode::NotFound,
        retryable: false,
    }
}

const fn rate_limited() -> KubernetesClientError {
    KubernetesClientError {
        code: KubernetesClientErrorCode::RateLimited,
        retryable: true,
    }
}

const fn timeout_error(retryable: bool) -> KubernetesClientError {
    KubernetesClientError {
        code: KubernetesClientErrorCode::Timeout,
        retryable,
    }
}

const fn dependency_unavailable() -> KubernetesClientError {
    KubernetesClientError {
        code: KubernetesClientErrorCode::DependencyUnavailable,
        retryable: true,
    }
}

const fn invalid_response() -> KubernetesClientError {
    KubernetesClientError {
        code: KubernetesClientErrorCode::InvalidResponse,
        retryable: false,
    }
}

const fn resource_exhausted() -> KubernetesClientError {
    KubernetesClientError {
        code: KubernetesClientErrorCode::ResourceExhausted,
        retryable: false,
    }
}

const fn unknown_outcome() -> KubernetesClientError {
    KubernetesClientError {
        code: KubernetesClientErrorCode::UnknownOutcome,
        retryable: false,
    }
}
