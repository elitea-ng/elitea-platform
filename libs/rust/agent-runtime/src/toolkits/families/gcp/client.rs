use std::collections::HashSet;
use std::fmt::{self, Write as _};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use percent_encoding::{NON_ALPHANUMERIC, percent_decode_str, utf8_percent_encode};
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderName, HeaderValue, USER_AGENT,
};
use reqwest::{Method, Request, StatusCode, Url};
use ring::rand::SystemRandom;
use ring::signature::{RSA_PKCS1_SHA256, RsaKeyPair};
use serde_json::{Map, Value, json};
use tokio::time::timeout;
use zeroize::Zeroizing;

use super::config::GcpToolkitConfig;

const TOKEN_URL: &str = "https://oauth2.googleapis.com/token";
const TOKEN_AUDIENCE: &str = TOKEN_URL;
const GRANT_TYPE: &str = "urn:ietf:params:oauth:grant-type:jwt-bearer";
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const TOKEN_TIMEOUT: Duration = Duration::from_secs(10);
const API_TIMEOUT: Duration = Duration::from_secs(20);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_SCOPES: usize = 32;
const MAX_SCOPE_BYTES: usize = 256;
const MAX_SCOPE_TOTAL_BYTES: usize = 8 * 1_024;
const MAX_API_URL_BYTES: usize = 8 * 1_024;
const MAX_REQUEST_BYTES: usize = 256 * 1_024;
const MAX_BODY_BYTES: usize = 240 * 1_024;
const MAX_TOKEN_RESPONSE_BYTES: usize = 64 * 1_024;
const MAX_API_RESPONSE_BYTES: usize = 512 * 1_024;
const MAX_ACCESS_TOKEN_BYTES: usize = 32 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_HEADERS: usize = 64;
const MAX_HEADER_NAME_BYTES: usize = 128;
const MAX_HEADER_VALUE_BYTES: usize = 8 * 1_024;
const MAX_HEADER_BYTES: usize = 32 * 1_024;
const MAX_QUERY_PARAMETERS: usize = 256;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";
const JSON_CONTENT_TYPE: &str = "application/json";
const FORM_CONTENT_TYPE: &str = "application/x-www-form-urlencoded";
const EMPTY_SUCCESS: &str =
    "Success: The request has been fulfilled and resulted in a new resource being created.";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum GcpClientErrorCode {
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

/// Stable GCP failure without account, key, scope, URL, argument, or provider data.
pub(crate) struct GcpClientError {
    code: GcpClientErrorCode,
    retryable: bool,
}

impl GcpClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> GcpClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            GcpClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "gcp.configuration.invalid",
                "the GCP toolkit configuration is invalid",
            ),
            GcpClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "gcp.request.invalid",
                "the Google Cloud REST request is invalid",
            ),
            GcpClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "gcp.authentication.failed",
                "Google service-account authentication failed",
            ),
            GcpClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "gcp.authorization.failed",
                "Google Cloud did not authorize the API request",
            ),
            GcpClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "gcp.resource.not_found",
                "the requested Google Cloud resource was not found",
            ),
            GcpClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "gcp.rate_limited",
                "Google Cloud rate limited the request",
            ),
            GcpClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "gcp.timeout",
                "the Google Cloud request timed out before an effect was dispatched",
            ),
            GcpClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "gcp.unavailable",
                "Google Cloud is unavailable",
            ),
            GcpClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "gcp.response.invalid",
                "Google Cloud returned an invalid response",
            ),
            GcpClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "gcp.resource_exhausted",
                "the GCP request or response exceeds the approved limit",
            ),
            GcpClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "gcp.effect.unknown_outcome",
                "the Google Cloud effect may have occurred; reconcile resource state before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(code: GcpClientErrorCode, retryable: bool) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for GcpClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("GcpClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for GcpClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            GcpClientErrorCode::InvalidConfiguration => "the GCP client configuration is invalid",
            GcpClientErrorCode::InvalidInput => "the Google Cloud request is invalid",
            GcpClientErrorCode::Authentication => "Google service-account authentication failed",
            GcpClientErrorCode::Authorization => "Google Cloud authorization failed",
            GcpClientErrorCode::NotFound => "the Google Cloud resource was not found",
            GcpClientErrorCode::RateLimited => "Google Cloud rate limited the request",
            GcpClientErrorCode::Timeout => "the Google Cloud request timed out",
            GcpClientErrorCode::DependencyUnavailable => "Google Cloud is unavailable",
            GcpClientErrorCode::InvalidResponse => "Google Cloud returned an invalid response",
            GcpClientErrorCode::ResourceExhausted => {
                "the GCP request or response exceeds its approved limit"
            }
            GcpClientErrorCode::UnknownOutcome => {
                "the Google Cloud effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for GcpClientError {}

#[async_trait]
pub(in crate::toolkits) trait GcpApi: Send + Sync {
    async fn execute(
        &self,
        method: &str,
        scopes: &[String],
        url: &str,
        optional_args: &Map<String, Value>,
    ) -> Result<Value, GcpClientError>;
}

pub(in crate::toolkits) struct GcpHttpResponse {
    status: StatusCode,
    body: Vec<u8>,
}

impl GcpHttpResponse {
    #[cfg(test)]
    pub(in crate::toolkits) fn fixture(status: StatusCode, body: impl Into<Vec<u8>>) -> Self {
        Self {
            status,
            body: body.into(),
        }
    }
}

#[async_trait]
pub(in crate::toolkits) trait GcpTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
        response_limit: usize,
    ) -> Result<GcpHttpResponse, GcpClientError>;
}

struct ReqwestGcpTransport {
    http: reqwest::Client,
}

#[async_trait]
impl GcpTransport for ReqwestGcpTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
        response_limit: usize,
    ) -> Result<GcpHttpResponse, GcpClientError> {
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
        Ok(GcpHttpResponse { status, body })
    }
}

/// Invocation-owned Google service-account signer and bounded REST client.
pub(crate) struct GcpClient {
    config: GcpToolkitConfig,
    transport: Arc<dyn GcpTransport>,
}

impl GcpClient {
    pub(crate) fn new(config: GcpToolkitConfig) -> Result<Self, GcpClientError> {
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
            transport: Arc::new(ReqwestGcpTransport { http }),
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn with_transport(
        config: GcpToolkitConfig,
        transport: Arc<dyn GcpTransport>,
    ) -> Self {
        Self { config, transport }
    }

    fn token_request(&self, scopes: &[String], now: SystemTime) -> Result<Request, GcpClientError> {
        service_account_token_request(&self.config, scopes, now)
    }

    async fn access_token(&self, scopes: &[String]) -> Result<Zeroizing<String>, GcpClientError> {
        let response = timeout(
            TOKEN_TIMEOUT,
            self.transport.execute(
                self.token_request(scopes, SystemTime::now())?,
                false,
                MAX_TOKEN_RESPONSE_BYTES,
            ),
        )
        .await
        .map_err(|_| timeout_error(true))??;
        access_token_from_response(response.status, response.body)
    }

    async fn execute_inner(
        &self,
        method: &str,
        scopes: &[String],
        url: &str,
        optional_args: &Map<String, Value>,
    ) -> Result<Value, GcpClientError> {
        validate_scopes(scopes)?;
        let method = parse_method(method)?;
        let effect = is_effect(&method);
        let mut request = prepare_api_request(method, url, optional_args)?;
        let token = self.access_token(scopes).await?;
        request
            .headers_mut()
            .insert(AUTHORIZATION, bearer_header(&token)?);
        let response = timeout(
            API_TIMEOUT,
            self.transport
                .execute(request, effect, MAX_API_RESPONSE_BYTES),
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
            return Err(map_api_status(response.status, effect));
        }
        project_success(&response.body, effect)
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_token_request(
        &self,
        scopes: &[String],
        now: SystemTime,
    ) -> Result<Request, GcpClientError> {
        self.token_request(scopes, now)
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_api_request(
        method: &str,
        url: &str,
        optional_args: &Map<String, Value>,
        token: &str,
    ) -> Result<Request, GcpClientError> {
        let mut request = prepare_api_request(parse_method(method)?, url, optional_args)?;
        request
            .headers_mut()
            .insert(AUTHORIZATION, bearer_header(token)?);
        Ok(request)
    }
}

#[async_trait]
impl GcpApi for GcpClient {
    async fn execute(
        &self,
        method: &str,
        scopes: &[String],
        url: &str,
        optional_args: &Map<String, Value>,
    ) -> Result<Value, GcpClientError> {
        self.execute_inner(method, scopes, url, optional_args).await
    }
}

/// The OAuth JWT-bearer token request for one service account and scope set.
///
/// Shared with the `bigquery` family, which signs with the same sealed
/// service-account authority but owns its own `BigQuery` API projection.
pub(in crate::toolkits) fn service_account_token_request(
    config: &GcpToolkitConfig,
    scopes: &[String],
    now: SystemTime,
) -> Result<Request, GcpClientError> {
    validate_scopes(scopes)?;
    let assertion = service_account_jwt(config, scopes, now)?;
    let mut body = Zeroizing::new(String::new());
    body.push_str("grant_type=");
    write!(
        body,
        "{}",
        utf8_percent_encode(GRANT_TYPE, NON_ALPHANUMERIC)
    )
    .map_err(|_| invalid_configuration())?;
    body.push_str("&assertion=");
    write!(
        body,
        "{}",
        utf8_percent_encode(assertion.as_str(), NON_ALPHANUMERIC)
    )
    .map_err(|_| invalid_configuration())?;
    if body.len() > MAX_REQUEST_BYTES {
        return Err(resource_exhausted());
    }
    request_with_body(
        Method::POST,
        Url::parse(TOKEN_URL).map_err(|_| invalid_configuration())?,
        body.as_bytes(),
        FORM_CONTENT_TYPE,
    )
}

/// The bounded access token from one OAuth token-endpoint response.
pub(in crate::toolkits) fn access_token_from_response(
    status: StatusCode,
    body: Vec<u8>,
) -> Result<Zeroizing<String>, GcpClientError> {
    map_token_status(status)?;
    let bytes = Zeroizing::new(body);
    let mut body: Value = serde_json::from_slice(&bytes).map_err(|_| invalid_response())?;
    let token = body
        .as_object_mut()
        .and_then(|body| body.remove("access_token"))
        .and_then(|value| value.as_str().map(str::to_owned))
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

fn service_account_jwt(
    config: &GcpToolkitConfig,
    scopes: &[String],
    now: SystemTime,
) -> Result<Zeroizing<String>, GcpClientError> {
    let issued_at = now
        .duration_since(UNIX_EPOCH)
        .map_err(|_| invalid_configuration())?
        .as_secs();
    let expires_at = issued_at
        .checked_add(3_600)
        .ok_or_else(invalid_configuration)?;
    let header = if let Some(key_id) = config.private_key_id() {
        json!({"alg":"RS256","typ":"JWT","kid":key_id})
    } else {
        json!({"alg":"RS256","typ":"JWT"})
    };
    let scope = scopes.join(" ");
    let claims = json!({
        "iss": config.client_email(),
        "scope": scope,
        "aud": TOKEN_AUDIENCE,
        "exp": expires_at,
        "iat": issued_at
    });
    let encoded_header = URL_SAFE_NO_PAD.encode(
        serde_json::to_vec(&header)
            .map_err(|_| invalid_configuration())?
            .as_slice(),
    );
    let claims = Zeroizing::new(serde_json::to_vec(&claims).map_err(|_| invalid_configuration())?);
    let encoded_claims = Zeroizing::new(URL_SAFE_NO_PAD.encode(claims.as_slice()));
    let mut signing_input = Zeroizing::new(format!("{encoded_header}.{}", encoded_claims.as_str()));
    let key_pair =
        RsaKeyPair::from_pkcs8(config.private_key_der()).map_err(|_| invalid_configuration())?;
    let random = SystemRandom::new();
    let mut signature = Zeroizing::new(vec![0_u8; key_pair.public().modulus_len()]);
    key_pair
        .sign(
            &RSA_PKCS1_SHA256,
            &random,
            signing_input.as_bytes(),
            &mut signature,
        )
        .map_err(|_| invalid_configuration())?;
    let signature = Zeroizing::new(URL_SAFE_NO_PAD.encode(signature.as_slice()));
    signing_input.push('.');
    signing_input.push_str(&signature);
    Ok(signing_input)
}

fn prepare_api_request(
    method: Method,
    url: &str,
    optional_args: &Map<String, Value>,
) -> Result<Request, GcpClientError> {
    validate_optional_args(optional_args)?;
    let mut url = validate_api_url(url)?;
    append_query(&mut url, optional_args.get("params"))?;
    let mut request = Request::new(method, url);
    request
        .headers_mut()
        .insert(ACCEPT, HeaderValue::from_static(JSON_CONTENT_TYPE));
    request
        .headers_mut()
        .insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
    apply_headers(request.headers_mut(), optional_args.get("headers"))?;
    let body = if let Some(value) = optional_args.get("json") {
        let body = serde_json::to_vec(value).map_err(|_| invalid_input())?;
        Some((body, Some(JSON_CONTENT_TYPE)))
    } else if let Some(value) = optional_args.get("data") {
        if let Some(value) = value.as_str() {
            Some((value.as_bytes().to_vec(), None))
        } else {
            Some((encode_form(value)?, Some(FORM_CONTENT_TYPE)))
        }
    } else {
        None
    };
    if let Some((body, default_content_type)) = body {
        if body.len() > MAX_BODY_BYTES {
            return Err(resource_exhausted());
        }
        if !request.headers().contains_key(CONTENT_TYPE)
            && let Some(default_content_type) = default_content_type
        {
            request
                .headers_mut()
                .insert(CONTENT_TYPE, HeaderValue::from_static(default_content_type));
        }
        request.headers_mut().insert(
            CONTENT_LENGTH,
            HeaderValue::from_str(&body.len().to_string()).map_err(|_| invalid_input())?,
        );
        *request.body_mut() = Some(body.into());
    }
    let body_size = request
        .body()
        .and_then(reqwest::Body::as_bytes)
        .map_or(0, <[u8]>::len);
    let size = request
        .url()
        .as_str()
        .len()
        .checked_add(body_size)
        .and_then(|size| size.checked_add(header_size(request.headers())))
        .ok_or_else(resource_exhausted)?;
    if size > MAX_REQUEST_BYTES {
        return Err(resource_exhausted());
    }
    Ok(request)
}

pub(in crate::toolkits) fn validate_optional_args(
    optional_args: &Map<String, Value>,
) -> Result<(), GcpClientError> {
    reject_unknown_options(optional_args)?;
    if optional_args.contains_key("json") && optional_args.contains_key("data") {
        return Err(invalid_input());
    }
    let mut validation_url =
        Url::parse("https://validation.googleapis.com/").map_err(|_| invalid_configuration())?;
    append_query(&mut validation_url, optional_args.get("params"))?;
    apply_headers(
        &mut reqwest::header::HeaderMap::new(),
        optional_args.get("headers"),
    )?;
    let body = if let Some(value) = optional_args.get("json") {
        serde_json::to_vec(value).map_err(|_| invalid_input())?
    } else if let Some(value) = optional_args.get("data") {
        value
            .as_str()
            .map_or_else(|| encode_form(value), |value| Ok(value.as_bytes().to_vec()))?
    } else {
        Vec::new()
    };
    if body.len() > MAX_BODY_BYTES {
        return Err(resource_exhausted());
    }
    Ok(())
}

fn request_with_body(
    method: Method,
    url: Url,
    body: &[u8],
    content_type: &'static str,
) -> Result<Request, GcpClientError> {
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
    *request.body_mut() = Some(body.to_vec().into());
    Ok(request)
}

pub(in crate::toolkits) fn validate_scopes(scopes: &[String]) -> Result<(), GcpClientError> {
    if scopes.is_empty() || scopes.len() > MAX_SCOPES {
        return Err(if scopes.len() > MAX_SCOPES {
            resource_exhausted()
        } else {
            invalid_input()
        });
    }
    let mut seen = HashSet::with_capacity(scopes.len());
    let mut total = 0usize;
    for scope in scopes {
        if scope.is_empty()
            || scope.len() > MAX_SCOPE_BYTES
            || !scope.starts_with("https://www.googleapis.com/auth/")
            || !scope.is_ascii()
            || scope
                .bytes()
                .any(|byte| byte.is_ascii_control() || byte.is_ascii_whitespace())
            || !seen.insert(scope)
        {
            return Err(if scope.len() > MAX_SCOPE_BYTES {
                resource_exhausted()
            } else {
                invalid_input()
            });
        }
        total = total
            .checked_add(scope.len())
            .and_then(|size| size.checked_add(1))
            .ok_or_else(resource_exhausted)?;
        if total > MAX_SCOPE_TOTAL_BYTES {
            return Err(resource_exhausted());
        }
    }
    Ok(())
}

fn reject_unknown_options(options: &Map<String, Value>) -> Result<(), GcpClientError> {
    if options
        .keys()
        .any(|key| !matches!(key.as_str(), "data" | "json" | "params" | "headers"))
    {
        return Err(invalid_input());
    }
    Ok(())
}

fn append_query(url: &mut Url, value: Option<&Value>) -> Result<(), GcpClientError> {
    let Some(value) = value else {
        return Ok(());
    };
    let values = value.as_object().ok_or_else(invalid_input)?;
    if values.len() > MAX_QUERY_PARAMETERS {
        return Err(resource_exhausted());
    }
    let mut pairs = url.query_pairs_mut();
    for (name, value) in values {
        if name.is_empty() || name.len() > MAX_SCOPE_BYTES {
            return Err(invalid_input());
        }
        match value {
            Value::Array(values) => {
                if values.len() > MAX_QUERY_PARAMETERS {
                    return Err(resource_exhausted());
                }
                for value in values {
                    pairs.append_pair(name, scalar_text(value)?.as_ref());
                }
            }
            value => {
                pairs.append_pair(name, scalar_text(value)?.as_ref());
            }
        }
    }
    Ok(())
}

fn encode_form(value: &Value) -> Result<Vec<u8>, GcpClientError> {
    let values = value.as_object().ok_or_else(invalid_input)?;
    if values.len() > MAX_QUERY_PARAMETERS {
        return Err(resource_exhausted());
    }
    let mut form = Url::parse("https://form.invalid/").map_err(|_| invalid_configuration())?;
    {
        let mut serializer = form.query_pairs_mut();
        for (name, value) in values {
            if name.is_empty() || name.len() > MAX_SCOPE_BYTES {
                return Err(invalid_input());
            }
            match value {
                Value::Array(values) => {
                    if values.len() > MAX_QUERY_PARAMETERS {
                        return Err(resource_exhausted());
                    }
                    for value in values {
                        serializer.append_pair(name, scalar_text(value)?.as_ref());
                    }
                }
                value => {
                    serializer.append_pair(name, scalar_text(value)?.as_ref());
                }
            }
        }
    }
    Ok(form.query().unwrap_or_default().as_bytes().to_vec())
}

fn scalar_text(value: &Value) -> Result<std::borrow::Cow<'_, str>, GcpClientError> {
    match value {
        Value::Null => Ok(std::borrow::Cow::Borrowed("")),
        Value::Bool(value) => Ok(std::borrow::Cow::Owned(value.to_string())),
        Value::Number(value) => Ok(std::borrow::Cow::Owned(value.to_string())),
        Value::String(value) if value.len() <= MAX_SCOPE_TOTAL_BYTES => {
            Ok(std::borrow::Cow::Borrowed(value))
        }
        Value::String(_) => Err(resource_exhausted()),
        Value::Array(_) | Value::Object(_) => Err(invalid_input()),
    }
}

fn apply_headers(
    target: &mut reqwest::header::HeaderMap,
    value: Option<&Value>,
) -> Result<(), GcpClientError> {
    let Some(value) = value else {
        return Ok(());
    };
    let headers = value.as_object().ok_or_else(invalid_input)?;
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
        target.insert(
            HeaderName::from_bytes(name.as_bytes()).map_err(|_| invalid_input())?,
            HeaderValue::from_str(value).map_err(|_| invalid_input())?,
        );
    }
    Ok(())
}

fn forbidden_header(name: &str) -> bool {
    crate::toolkits::is_reserved_platform_header(name)
        || matches!(
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

fn bearer_header(token: &str) -> Result<HeaderValue, GcpClientError> {
    let mut value = Zeroizing::new(String::with_capacity("Bearer ".len() + token.len()));
    value.push_str("Bearer ");
    value.push_str(token);
    HeaderValue::from_str(&value).map_err(|_| invalid_response())
}

pub(in crate::toolkits) fn parse_method(method: &str) -> Result<Method, GcpClientError> {
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

pub(in crate::toolkits) fn validate_api_url(value: &str) -> Result<Url, GcpClientError> {
    if value.is_empty()
        || value.len() > MAX_API_URL_BYTES
        || value.bytes().any(|byte| byte.is_ascii_control())
        || !valid_percent_encoding(value)
    {
        return Err(if value.len() > MAX_API_URL_BYTES {
            resource_exhausted()
        } else {
            invalid_input()
        });
    }
    validate_raw_path(value)?;
    let url = Url::parse(value).map_err(|_| invalid_input())?;
    let host = url.host_str().ok_or_else(invalid_input)?;
    if url.scheme() != "https"
        || !(host == "googleapis.com" || host.ends_with(".googleapis.com"))
        || !url.username().is_empty()
        || url.password().is_some()
        || url.port().is_some()
        || url.fragment().is_some()
    {
        return Err(invalid_input());
    }
    Ok(url)
}

fn validate_raw_path(value: &str) -> Result<(), GcpClientError> {
    let after_scheme = value
        .split_once("://")
        .map(|(_, remainder)| remainder)
        .ok_or_else(invalid_input)?;
    let path_and_query = after_scheme
        .find('/')
        .map_or("/", |index| &after_scheme[index..]);
    let path = path_and_query
        .split_once('?')
        .map_or(path_and_query, |(path, _)| path);
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
    Ok(())
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

fn project_success(body: &[u8], effect: bool) -> Result<Value, GcpClientError> {
    if body.is_empty() {
        return Ok(Value::String(EMPTY_SUCCESS.to_owned()));
    }
    let value: Value = serde_json::from_slice(body).map_err(|_| post_accept_failure(effect))?;
    let size = serde_json::to_vec(&value)
        .map_err(|_| post_accept_failure(effect))?
        .len();
    if size > MAX_OUTPUT_BYTES {
        return Err(post_accept_bound_failure(effect));
    }
    Ok(value)
}

fn map_token_status(status: StatusCode) -> Result<(), GcpClientError> {
    match status {
        status if status.is_success() => Ok(()),
        StatusCode::BAD_REQUEST | StatusCode::UNAUTHORIZED => Err(authentication()),
        StatusCode::FORBIDDEN => Err(authorization()),
        StatusCode::REQUEST_TIMEOUT => Err(timeout_error(true)),
        StatusCode::TOO_MANY_REQUESTS => Err(rate_limited()),
        status if status.is_server_error() => Err(dependency_unavailable()),
        _ => Err(invalid_response()),
    }
}

fn map_api_status(status: StatusCode, effect: bool) -> GcpClientError {
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
        status if status.is_server_error() => dependency_unavailable(),
        _ => invalid_response(),
    }
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> GcpClientError {
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

fn post_accept_failure(effect: bool) -> GcpClientError {
    if effect {
        unknown_outcome()
    } else {
        invalid_response()
    }
}

fn post_accept_bound_failure(effect: bool) -> GcpClientError {
    if effect {
        unknown_outcome()
    } else {
        resource_exhausted()
    }
}

fn response_bound_failure(effect: bool) -> GcpClientError {
    post_accept_bound_failure(effect)
}

const fn invalid_configuration() -> GcpClientError {
    GcpClientError {
        code: GcpClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

const fn invalid_input() -> GcpClientError {
    GcpClientError {
        code: GcpClientErrorCode::InvalidInput,
        retryable: false,
    }
}

const fn authentication() -> GcpClientError {
    GcpClientError {
        code: GcpClientErrorCode::Authentication,
        retryable: false,
    }
}

const fn authorization() -> GcpClientError {
    GcpClientError {
        code: GcpClientErrorCode::Authorization,
        retryable: false,
    }
}

const fn not_found() -> GcpClientError {
    GcpClientError {
        code: GcpClientErrorCode::NotFound,
        retryable: false,
    }
}

const fn rate_limited() -> GcpClientError {
    GcpClientError {
        code: GcpClientErrorCode::RateLimited,
        retryable: true,
    }
}

const fn timeout_error(retryable: bool) -> GcpClientError {
    GcpClientError {
        code: GcpClientErrorCode::Timeout,
        retryable,
    }
}

const fn dependency_unavailable() -> GcpClientError {
    GcpClientError {
        code: GcpClientErrorCode::DependencyUnavailable,
        retryable: true,
    }
}

const fn invalid_response() -> GcpClientError {
    GcpClientError {
        code: GcpClientErrorCode::InvalidResponse,
        retryable: false,
    }
}

const fn resource_exhausted() -> GcpClientError {
    GcpClientError {
        code: GcpClientErrorCode::ResourceExhausted,
        retryable: false,
    }
}

const fn unknown_outcome() -> GcpClientError {
    GcpClientError {
        code: GcpClientErrorCode::UnknownOutcome,
        retryable: false,
    }
}
