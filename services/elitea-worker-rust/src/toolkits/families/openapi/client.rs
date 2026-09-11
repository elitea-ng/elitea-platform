use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_rust::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use bytes::Bytes;
use http::{Request as HttpRequest, Uri};
use http_body_util::Full;
use hyper::body::{Body as _, Incoming};
use hyper_rustls::HttpsConnector;
use hyper_util::client::legacy::Client as HyperClient;
use hyper_util::client::legacy::connect::HttpConnector;
use hyper_util::rt::{TokioExecutor, TokioTimer};
use percent_encoding::{AsciiSet, NON_ALPHANUMERIC, utf8_percent_encode};
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, COOKIE, HeaderMap, HeaderName,
    HeaderValue, USER_AGENT,
};
use reqwest::{Method, Request as ReqwestRequest, StatusCode};
use serde_json::{Map, Value};
use tokio::time::Instant;
use zeroize::Zeroizing;

use super::config::{OpenApiAuth, OpenApiClientConfig};
use super::response_selection::ResponseSelection;
use super::spec::{OpenApiOperation, OpenApiParameter, OpenApiParameterLocation};
use crate::toolkits::DelegatedAuthorizationRequirement;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const TOKEN_TIMEOUT: Duration = Duration::from_secs(20);
const DEFAULT_TOKEN_LIFETIME: Duration = Duration::from_hours(1);
const TOKEN_EXPIRY_BUFFER: Duration = Duration::from_mins(1);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_REQUEST_BYTES: usize = 256 * 1024;
const MAX_RESPONSE_BYTES: usize = 2 * 1024 * 1024;
const MAX_OUTPUT_BYTES: usize = 512 * 1024;
const MAX_TOKEN_RESPONSE_BYTES: usize = 128 * 1024;
const MAX_TOKEN_BYTES: usize = 64 * 1024;
const MAX_HEADER_COUNT: usize = 128;
const MAX_HEADER_VALUE_BYTES: usize = 16 * 1024;
const MAX_QUERY_BYTES: usize = 128 * 1024;
const MAX_REGEXP_BYTES: usize = 4 * 1024;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";

const COMPONENT_ENCODE_SET: &AsciiSet = &NON_ALPHANUMERIC
    .remove(b'-')
    .remove(b'.')
    .remove(b'_')
    .remove(b'~');
const RESERVED_VALUE_ENCODE_SET: &AsciiSet = &COMPONENT_ENCODE_SET
    .remove(b':')
    .remove(b'/')
    .remove(b'?')
    .remove(b'[')
    .remove(b']')
    .remove(b'@')
    .remove(b'!')
    .remove(b'$')
    .remove(b'&')
    .remove(b'\'')
    .remove(b'(')
    .remove(b')')
    .remove(b'*')
    .remove(b',')
    .remove(b';')
    .remove(b'=');
const RESERVED_NAME_ENCODE_SET: &AsciiSet = &COMPONENT_ENCODE_SET
    .remove(b':')
    .remove(b'/')
    .remove(b'?')
    .remove(b'[')
    .remove(b']')
    .remove(b'@')
    .remove(b'!')
    .remove(b'$')
    .remove(b'\'')
    .remove(b'(')
    .remove(b')')
    .remove(b'*')
    .remove(b',')
    .remove(b';');

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum OpenApiClientErrorCode {
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

pub(crate) struct OpenApiClientError {
    code: OpenApiClientErrorCode,
}

impl OpenApiClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> OpenApiClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        matches!(
            self.code,
            OpenApiClientErrorCode::RateLimited
                | OpenApiClientErrorCode::Timeout
                | OpenApiClientErrorCode::DependencyUnavailable
        )
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            OpenApiClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "openapi.configuration.invalid",
                "the OpenAPI toolkit configuration is invalid",
            ),
            OpenApiClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "openapi.request.invalid",
                "the OpenAPI operation arguments are invalid",
            ),
            OpenApiClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "openapi.authentication.failed",
                "OpenAPI authentication failed",
            ),
            OpenApiClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "openapi.authorization.failed",
                "the OpenAPI endpoint did not authorize the operation",
            ),
            OpenApiClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "openapi.resource.not_found",
                "the OpenAPI resource was not found",
            ),
            OpenApiClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "openapi.rate_limited",
                "the OpenAPI endpoint rate limited the operation",
            ),
            OpenApiClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "openapi.timeout",
                "the OpenAPI operation timed out",
            ),
            OpenApiClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "openapi.unavailable",
                "the OpenAPI endpoint is unavailable",
            ),
            OpenApiClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "openapi.response.invalid",
                "the OpenAPI endpoint returned an invalid response",
            ),
            OpenApiClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "openapi.resource_exhausted",
                "the OpenAPI request or response exceeds its approved limit",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable(),
            retry_after_ms: None,
            max_attempts: None,
        })
    }
}

impl fmt::Debug for OpenApiClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("OpenApiClientError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for OpenApiClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            OpenApiClientErrorCode::InvalidConfiguration => {
                "the OpenAPI client configuration is invalid"
            }
            OpenApiClientErrorCode::InvalidInput => "the OpenAPI request is invalid",
            OpenApiClientErrorCode::Authentication => "OpenAPI authentication failed",
            OpenApiClientErrorCode::Authorization => "OpenAPI authorization failed",
            OpenApiClientErrorCode::NotFound => "the OpenAPI resource was not found",
            OpenApiClientErrorCode::RateLimited => "the OpenAPI endpoint rate limited the request",
            OpenApiClientErrorCode::Timeout => "the OpenAPI request timed out",
            OpenApiClientErrorCode::DependencyUnavailable => "the OpenAPI endpoint is unavailable",
            OpenApiClientErrorCode::InvalidResponse => {
                "the OpenAPI endpoint returned an invalid response"
            }
            OpenApiClientErrorCode::ResourceExhausted => {
                "the OpenAPI request or response exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for OpenApiClientError {}

#[async_trait]
pub(in crate::toolkits) trait OpenApiApi: Send + Sync {
    fn authorization(&self) -> Option<&DelegatedAuthorizationRequirement>;

    async fn execute(
        &self,
        operation: &OpenApiOperation,
        arguments: &Map<String, Value>,
    ) -> Result<Value, OpenApiClientError>;
}

#[async_trait]
pub(in crate::toolkits) trait OpenApiTransport: Send + Sync {
    async fn execute(&self, request: OpenApiRequest)
    -> Result<OpenApiResponse, OpenApiClientError>;

    async fn token(
        &self,
        request: ReqwestRequest,
    ) -> Result<OpenApiAccessToken, OpenApiClientError>;
}

pub(in crate::toolkits) struct OpenApiRequest {
    request: HttpRequest<Full<Bytes>>,
    body: Bytes,
}

impl OpenApiRequest {
    #[must_use]
    pub(in crate::toolkits) fn uri(&self) -> &Uri {
        self.request.uri()
    }

    #[must_use]
    pub(in crate::toolkits) fn headers(&self) -> &HeaderMap {
        self.request.headers()
    }

    #[must_use]
    pub(in crate::toolkits) fn body(&self) -> &[u8] {
        &self.body
    }

    fn into_inner(self) -> HttpRequest<Full<Bytes>> {
        self.request
    }
}

pub(in crate::toolkits) struct OpenApiResponse {
    pub(in crate::toolkits) status: StatusCode,
    pub(in crate::toolkits) body: Vec<u8>,
}

pub(in crate::toolkits) struct OpenApiAccessToken {
    pub(in crate::toolkits) value: Zeroizing<String>,
    pub(in crate::toolkits) expires_in: Duration,
}

struct CachedOpenApiAccessToken {
    value: Zeroizing<String>,
    expires_at: Instant,
}

type OpenApiHttpClient = HyperClient<HttpsConnector<HttpConnector>, Full<Bytes>>;

struct RustlsOpenApiTransport {
    http: tokio::sync::OnceCell<OpenApiHttpClient>,
    token_http: reqwest::Client,
}

#[async_trait]
impl OpenApiTransport for RustlsOpenApiTransport {
    async fn execute(
        &self,
        request: OpenApiRequest,
    ) -> Result<OpenApiResponse, OpenApiClientError> {
        let http = self
            .http
            .get_or_try_init(|| async { build_http_client() })
            .await?;
        let response =
            tokio::time::timeout(REQUEST_TIMEOUT, execute_http(http, request.into_inner()))
                .await
                .map_err(|_| timeout())??;
        Ok(response)
    }

    async fn token(
        &self,
        request: ReqwestRequest,
    ) -> Result<OpenApiAccessToken, OpenApiClientError> {
        let response = tokio::time::timeout(TOKEN_TIMEOUT, self.token_http.execute(request))
            .await
            .map_err(|_| timeout())?
            .map_err(|error| map_reqwest_error(&error))?;
        if response.status() != StatusCode::OK {
            return Err(authentication());
        }
        let body = bounded_body(response, MAX_TOKEN_RESPONSE_BYTES).await?;
        let payload = serde_json::from_slice::<Value>(&body).map_err(|_| invalid_response())?;
        let object = payload.as_object().ok_or_else(invalid_response)?;
        let token = object
            .get("access_token")
            .and_then(Value::as_str)
            .filter(|token| !token.is_empty() && token.len() <= MAX_TOKEN_BYTES)
            .ok_or_else(authentication)?
            .to_owned();
        HeaderValue::from_str(&format!("Bearer {token}")).map_err(|_| invalid_response())?;
        Ok(OpenApiAccessToken {
            value: Zeroizing::new(token),
            expires_in: oauth_token_lifetime(object.get("expires_in")),
        })
    }
}

async fn execute_http(
    client: &OpenApiHttpClient,
    request: HttpRequest<Full<Bytes>>,
) -> Result<OpenApiResponse, OpenApiClientError> {
    let response = client
        .request(request)
        .await
        .map_err(|_| dependency_unavailable())?;
    let status = response.status();
    let body = bounded_hyper_body(response.into_body(), MAX_RESPONSE_BYTES).await?;
    Ok(OpenApiResponse { status, body })
}

fn build_http_client() -> Result<OpenApiHttpClient, OpenApiClientError> {
    let mut connector = HttpConnector::new();
    // The outer Rustls connector enforces HTTPS. The TCP connector must accept
    // the original `https` URI so the outer layer can perform TLS.
    connector.enforce_http(false);
    connector.set_connect_timeout(Some(CONNECT_TIMEOUT));
    let https = hyper_rustls::HttpsConnectorBuilder::new()
        .with_provider_and_native_roots(rustls::crypto::ring::default_provider())
        .map_err(|_| invalid_configuration())?
        .https_only()
        .enable_http1()
        .enable_http2()
        .wrap_connector(connector);
    let mut builder = HyperClient::builder(TokioExecutor::new());
    builder
        .pool_idle_timeout(POOL_IDLE_TIMEOUT)
        .pool_max_idle_per_host(MAX_IDLE_PER_HOST)
        .pool_timer(TokioTimer::new());
    Ok(builder.build(https))
}

pub(crate) struct OpenApiClient {
    config: OpenApiClientConfig,
    transport: Arc<dyn OpenApiTransport>,
    access_token: tokio::sync::Mutex<Option<CachedOpenApiAccessToken>>,
}

impl OpenApiClient {
    pub(crate) fn new(config: OpenApiClientConfig) -> Result<Self, OpenApiClientError> {
        if matches!(
            config.auth,
            OpenApiAuth::Delegated {
                access_token: None,
                ..
            }
        ) {
            return Err(invalid_configuration());
        }
        let token_http = reqwest::Client::builder()
            .https_only(true)
            .redirect(reqwest::redirect::Policy::none())
            .connect_timeout(CONNECT_TIMEOUT)
            .timeout(TOKEN_TIMEOUT)
            .pool_idle_timeout(POOL_IDLE_TIMEOUT)
            .pool_max_idle_per_host(MAX_IDLE_PER_HOST)
            .user_agent(USER_AGENT_VALUE)
            .build()
            .map_err(|_| invalid_configuration())?;
        Ok(Self {
            config,
            transport: Arc::new(RustlsOpenApiTransport {
                http: tokio::sync::OnceCell::new(),
                token_http,
            }),
            access_token: tokio::sync::Mutex::new(None),
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn with_transport(
        config: OpenApiClientConfig,
        transport: Arc<dyn OpenApiTransport>,
    ) -> Self {
        Self {
            config,
            transport,
            access_token: tokio::sync::Mutex::new(None),
        }
    }

    async fn build_request(
        &self,
        operation: &OpenApiOperation,
        arguments: &Map<String, Value>,
    ) -> Result<(OpenApiRequest, Option<String>, Option<ResponseSelection>), OpenApiClientError>
    {
        let allowed = operation
            .parameters()
            .iter()
            .map(OpenApiParameter::name)
            .chain([
                "body_json",
                "headers",
                "regexp",
                "response_search",
                "response_limit",
            ])
            .collect::<std::collections::HashSet<_>>();
        if arguments.keys().any(|key| !allowed.contains(key.as_str())) {
            return Err(invalid_input());
        }
        let mut path = operation.path().to_owned();
        let mut query = Vec::new();
        let mut headers = HeaderMap::new();
        let mut cookies = Vec::new();
        for parameter in operation.parameters() {
            let value = arguments.get(parameter.name());
            if value.is_none_or(Value::is_null) {
                if parameter.required() {
                    return Err(invalid_input());
                }
                continue;
            }
            let value = value.ok_or_else(invalid_input)?;
            match parameter.location() {
                OpenApiParameterLocation::Path => {
                    let placeholder = format!("{{{}}}", parameter.name());
                    if !path.contains(&placeholder) {
                        return Err(invalid_configuration());
                    }
                    let encoded = encode_component(&serialize_simple(value, false)?, false);
                    path = path.replace(&placeholder, &encoded);
                }
                OpenApiParameterLocation::Query => {
                    query.extend(serialize_query(parameter, value)?);
                }
                OpenApiParameterLocation::Header => {
                    let name = HeaderName::from_bytes(parameter.name().as_bytes())
                        .map_err(|_| invalid_configuration())?;
                    if restricted_header(&name) {
                        return Err(invalid_configuration());
                    }
                    let value = serialize_simple(value, parameter.explode())?;
                    insert_header(&mut headers, name, &value)?;
                }
                OpenApiParameterLocation::Cookie => {
                    let value = serialize_simple(value, parameter.explode())?;
                    cookies.push(format!(
                        "{}={}",
                        encode_component(parameter.name(), false),
                        encode_component(&value, false)
                    ));
                }
            }
        }
        if path.contains(['{', '}']) || path.len() > MAX_QUERY_BYTES {
            return Err(invalid_input());
        }
        let mut endpoint = self.config.base_url.clone();
        let base_path = endpoint.path().trim_end_matches('/');
        let combined_path = format!("{base_path}{}", path.as_str());
        if combined_path.len() > MAX_QUERY_BYTES {
            return Err(resource_exhausted());
        }
        endpoint.set_path(&combined_path);
        let query = query.join("&");
        if query.len() > MAX_QUERY_BYTES {
            return Err(resource_exhausted());
        }
        if !cookies.is_empty() {
            insert_header(&mut headers, COOKIE, &cookies.join("; "))?;
        }
        apply_extra_headers(&mut headers, arguments.get("headers"))?;
        headers.extend(self.config.additional_headers.clone());
        apply_auth(
            &mut headers,
            &self.config.auth,
            self.transport.as_ref(),
            &self.access_token,
        )
        .await?;
        headers.insert(ACCEPT, HeaderValue::from_static("*/*"));
        headers.insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
        let mut body = Vec::new();
        if let Some(body_spec) = operation.body() {
            let body_argument = arguments.get("body_json");
            if body_argument.is_none_or(Value::is_null) {
                if body_spec.required() {
                    return Err(invalid_input());
                }
            } else {
                let raw = body_argument
                    .and_then(Value::as_str)
                    .filter(|raw| !raw.is_empty() && raw.len() <= MAX_REQUEST_BYTES)
                    .ok_or_else(invalid_input)?;
                let value: Value = serde_json::from_str(raw).map_err(|_| invalid_input())?;
                body = serde_json::to_vec(&value).map_err(|_| invalid_input())?;
                if body.len() > MAX_REQUEST_BYTES {
                    return Err(resource_exhausted());
                }
                headers.insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
            }
        }
        let uri = operation_uri(&endpoint, &query)?;
        let body = Bytes::from(body);
        let mut request = HttpRequest::builder()
            .method(operation.method().clone())
            .uri(uri)
            .body(Full::new(body.clone()))
            .map_err(|_| invalid_input())?;
        *request.headers_mut() = headers;
        let regexp = match arguments.get("regexp") {
            None | Some(Value::Null) => None,
            Some(Value::String(value))
                if !value.is_empty()
                    && value.len() <= MAX_REGEXP_BYTES
                    && !value.contains('\0') =>
            {
                Some(value.clone())
            }
            Some(_) => return Err(invalid_input()),
        };
        let response_selection =
            ResponseSelection::parse(arguments).map_err(|()| invalid_input())?;
        if regexp.is_some() && response_selection.is_some() {
            return Err(invalid_input());
        }
        Ok((OpenApiRequest { request, body }, regexp, response_selection))
    }
}

#[async_trait]
impl OpenApiApi for OpenApiClient {
    fn authorization(&self) -> Option<&DelegatedAuthorizationRequirement> {
        match &self.config.auth {
            OpenApiAuth::Delegated { requirement, .. } => Some(requirement),
            OpenApiAuth::Anonymous
            | OpenApiAuth::Header { .. }
            | OpenApiAuth::ClientCredentials { .. } => None,
        }
    }

    async fn execute(
        &self,
        operation: &OpenApiOperation,
        arguments: &Map<String, Value>,
    ) -> Result<Value, OpenApiClientError> {
        let (request, regexp, response_selection) =
            self.build_request(operation, arguments).await?;
        let response = self.transport.execute(request).await?;
        map_status(response.status)?;
        if response_selection.is_none() && response.body.len() > MAX_OUTPUT_BYTES {
            return Err(resource_exhausted());
        }
        let mut output = String::from_utf8(response.body).map_err(|_| invalid_response())?;
        if let Some(response_selection) = response_selection {
            output = response_selection.apply(&output, operation.response_collection_paths());
        }
        if let Some(regexp) = regexp {
            let expression = regex::Regex::new(&regexp).map_err(|_| invalid_input())?;
            output = expression.replace_all(&output, "").into_owned();
        }
        Ok(Value::String(output))
    }
}

async fn apply_auth(
    headers: &mut HeaderMap,
    auth: &OpenApiAuth,
    transport: &dyn OpenApiTransport,
    access_token: &tokio::sync::Mutex<Option<CachedOpenApiAccessToken>>,
) -> Result<(), OpenApiClientError> {
    match auth {
        OpenApiAuth::Anonymous => Ok(()),
        OpenApiAuth::Header { name, value } => {
            insert_sensitive_header(headers, name.clone(), value)
        }
        OpenApiAuth::Delegated {
            access_token: Some(token),
            ..
        } => insert_sensitive_header(
            headers,
            AUTHORIZATION,
            &format!("Bearer {}", token.as_str()),
        ),
        OpenApiAuth::Delegated {
            access_token: None, ..
        } => Err(invalid_configuration()),
        OpenApiAuth::ClientCredentials {
            client_id,
            client_secret,
            token_url,
            scope,
            basic,
        } => {
            let mut form = vec![("grant_type", "client_credentials")];
            if !basic {
                form.push(("client_id", client_id.as_ref()));
                form.push(("client_secret", client_secret.as_str()));
            }
            if let Some(scope) = scope {
                form.push(("scope", scope.as_ref()));
            }
            let encoded = serde_urlencoded_form(&form)?;
            let mut request = ReqwestRequest::new(Method::POST, token_url.clone());
            request.headers_mut().insert(
                CONTENT_TYPE,
                HeaderValue::from_static("application/x-www-form-urlencoded"),
            );
            request
                .headers_mut()
                .insert(ACCEPT, HeaderValue::from_static("application/json"));
            if *basic {
                let encoded = STANDARD.encode(format!("{client_id}:{}", client_secret.as_str()));
                insert_sensitive_header(
                    request.headers_mut(),
                    AUTHORIZATION,
                    &format!("Basic {encoded}"),
                )?;
            }
            *request.body_mut() = Some(encoded.into());
            let mut cached = access_token.lock().await;
            let now = Instant::now();
            let needs_refresh = cached.as_ref().is_none_or(|token| {
                token.expires_at.saturating_duration_since(now) <= TOKEN_EXPIRY_BUFFER
            });
            if needs_refresh {
                let token = transport.token(request).await?;
                let expires_at = Instant::now()
                    .checked_add(token.expires_in)
                    .ok_or_else(invalid_response)?;
                *cached = Some(CachedOpenApiAccessToken {
                    value: token.value,
                    expires_at,
                });
            }
            let token = cached.as_ref().ok_or_else(invalid_response)?;
            insert_sensitive_header(
                headers,
                AUTHORIZATION,
                &format!("Bearer {}", token.value.as_str()),
            )
        }
    }
}

pub(in crate::toolkits) fn oauth_token_lifetime(value: Option<&Value>) -> Duration {
    let seconds = value.and_then(|value| match value {
        Value::Number(value) => value.as_f64(),
        Value::String(value) => value.parse::<f64>().ok(),
        Value::Null | Value::Bool(_) | Value::Array(_) | Value::Object(_) => None,
    });
    seconds
        .filter(|seconds| seconds.is_finite() && *seconds > 0.0)
        .and_then(|seconds| Duration::try_from_secs_f64(seconds).ok())
        .unwrap_or(DEFAULT_TOKEN_LIFETIME)
}

fn serde_urlencoded_form(values: &[(&str, &str)]) -> Result<Vec<u8>, OpenApiClientError> {
    let encoded = values
        .iter()
        .map(|(name, value)| {
            format!(
                "{}={}",
                encode_component(name, false),
                encode_component(value, false)
            )
        })
        .collect::<Vec<_>>()
        .join("&")
        .into_bytes();
    if encoded.len() > MAX_REQUEST_BYTES {
        return Err(resource_exhausted());
    }
    Ok(encoded)
}

fn serialize_query(
    parameter: &OpenApiParameter,
    value: &Value,
) -> Result<Vec<String>, OpenApiClientError> {
    let name = encode_query_name(parameter.name(), parameter.allow_reserved());
    let safe = parameter.allow_reserved();
    match value {
        Value::Array(values) => {
            if values.is_empty() {
                return Ok(Vec::new());
            }
            let values = values
                .iter()
                .map(|value| scalar(value).map(|value| encode_component(&value, safe)))
                .collect::<Result<Vec<_>, _>>()?;
            if parameter.explode() {
                Ok(values
                    .into_iter()
                    .map(|value| format!("{name}={value}"))
                    .collect())
            } else {
                let separator = match parameter.style() {
                    "spaceDelimited" => "%20",
                    "pipeDelimited" => "|",
                    "form" | "simple" => ",",
                    _ => return Err(invalid_configuration()),
                };
                Ok(vec![format!("{name}={}", values.join(separator))])
            }
        }
        Value::Object(values) => {
            if values.is_empty() {
                return Ok(Vec::new());
            }
            if parameter.explode() {
                values
                    .iter()
                    .map(|(key, value)| {
                        Ok(format!(
                            "{}={}",
                            encode_component(key, false),
                            encode_component(&scalar(value)?, safe)
                        ))
                    })
                    .collect()
            } else {
                let mut flattened = Vec::with_capacity(values.len() * 2);
                for (key, value) in values {
                    flattened.push(encode_component(key, false));
                    flattened.push(encode_component(&scalar(value)?, safe));
                }
                Ok(vec![format!("{name}={}", flattened.join(","))])
            }
        }
        Value::Null => Ok(Vec::new()),
        Value::Bool(_) | Value::Number(_) | Value::String(_) => Ok(vec![format!(
            "{name}={}",
            encode_component(&scalar(value)?, safe)
        )]),
    }
}

fn serialize_simple(value: &Value, explode: bool) -> Result<String, OpenApiClientError> {
    match value {
        Value::Array(values) => values
            .iter()
            .map(scalar)
            .collect::<Result<Vec<_>, _>>()
            .map(|values| values.join(",")),
        Value::Object(values) => {
            let mut flattened = Vec::with_capacity(values.len() * 2);
            for (key, value) in values {
                if explode {
                    flattened.push(format!("{key}={}", scalar(value)?));
                } else {
                    flattened.push(key.clone());
                    flattened.push(scalar(value)?);
                }
            }
            Ok(flattened.join(","))
        }
        Value::Null => Err(invalid_input()),
        Value::Bool(_) | Value::Number(_) | Value::String(_) => scalar(value),
    }
}

fn scalar(value: &Value) -> Result<String, OpenApiClientError> {
    match value {
        Value::String(value) => Ok(value.clone()),
        Value::Bool(value) => Ok(value.to_string()),
        Value::Number(value) => Ok(value.to_string()),
        Value::Null | Value::Array(_) | Value::Object(_) => Err(invalid_input()),
    }
}

fn encode_component(value: &str, allow_reserved: bool) -> String {
    utf8_percent_encode(
        value,
        if allow_reserved {
            RESERVED_VALUE_ENCODE_SET
        } else {
            COMPONENT_ENCODE_SET
        },
    )
    .to_string()
}

fn encode_query_name(value: &str, allow_reserved: bool) -> String {
    utf8_percent_encode(
        value,
        if allow_reserved {
            RESERVED_NAME_ENCODE_SET
        } else {
            COMPONENT_ENCODE_SET
        },
    )
    .to_string()
}

fn operation_uri(endpoint: &reqwest::Url, query: &str) -> Result<Uri, OpenApiClientError> {
    let mut raw = endpoint.as_str().to_owned();
    if !query.is_empty() {
        raw.push('?');
        raw.push_str(query);
    }
    raw.parse::<Uri>().map_err(|_| invalid_input())
}

fn apply_extra_headers(
    headers: &mut HeaderMap,
    value: Option<&Value>,
) -> Result<(), OpenApiClientError> {
    let Some(value) = value else {
        return Ok(());
    };
    if value.is_null() {
        return Ok(());
    }
    let values = value.as_object().ok_or_else(invalid_input)?;
    if values.len() > MAX_HEADER_COUNT {
        return Err(resource_exhausted());
    }
    for (name, value) in values {
        let name = HeaderName::from_bytes(name.as_bytes()).map_err(|_| invalid_input())?;
        if restricted_header(&name) {
            return Err(invalid_input());
        }
        let value = value.as_str().ok_or_else(invalid_input)?;
        insert_header(headers, name, value)?;
    }
    Ok(())
}

fn insert_header(
    headers: &mut HeaderMap,
    name: HeaderName,
    value: &str,
) -> Result<(), OpenApiClientError> {
    if value.len() > MAX_HEADER_VALUE_BYTES {
        return Err(resource_exhausted());
    }
    let value = HeaderValue::from_str(value).map_err(|_| invalid_input())?;
    headers.insert(name, value);
    Ok(())
}

fn insert_sensitive_header(
    headers: &mut HeaderMap,
    name: HeaderName,
    value: &str,
) -> Result<(), OpenApiClientError> {
    if value.len() > MAX_HEADER_VALUE_BYTES {
        return Err(resource_exhausted());
    }
    let mut value = HeaderValue::from_str(value).map_err(|_| invalid_configuration())?;
    value.set_sensitive(true);
    headers.insert(name, value);
    Ok(())
}

fn restricted_header(name: &HeaderName) -> bool {
    matches!(
        name.as_str(),
        "authorization"
            | "proxy-authorization"
            | "host"
            | "content-length"
            | "transfer-encoding"
            | "connection"
            | "cookie"
            | "set-cookie"
    )
}

async fn bounded_body(
    mut response: reqwest::Response,
    limit: usize,
) -> Result<Vec<u8>, OpenApiClientError> {
    if response
        .headers()
        .get(CONTENT_LENGTH)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.parse::<usize>().ok())
        .is_some_and(|length| length > limit)
    {
        return Err(resource_exhausted());
    }
    let mut body = Vec::new();
    while let Some(chunk) = response
        .chunk()
        .await
        .map_err(|error| map_reqwest_error(&error))?
    {
        let next = body
            .len()
            .checked_add(chunk.len())
            .ok_or_else(resource_exhausted)?;
        if next > limit {
            return Err(resource_exhausted());
        }
        body.extend_from_slice(&chunk);
    }
    Ok(body)
}

async fn bounded_hyper_body(
    mut body: Incoming,
    limit: usize,
) -> Result<Vec<u8>, OpenApiClientError> {
    use http_body_util::BodyExt as _;

    if body
        .size_hint()
        .upper()
        .is_some_and(|length| length > limit as u64)
    {
        return Err(resource_exhausted());
    }
    let mut output = Vec::new();
    while let Some(frame) = body.frame().await {
        let frame = frame.map_err(|_| dependency_unavailable())?;
        let Ok(data) = frame.into_data() else {
            continue;
        };
        let next = output
            .len()
            .checked_add(data.len())
            .ok_or_else(resource_exhausted)?;
        if next > limit {
            return Err(resource_exhausted());
        }
        output.extend_from_slice(&data);
    }
    Ok(output)
}

fn map_status(status: StatusCode) -> Result<(), OpenApiClientError> {
    match status.as_u16() {
        200..=299 => Ok(()),
        401 => Err(authentication()),
        403 => Err(authorization()),
        404 => Err(not_found()),
        408 | 504 => Err(timeout()),
        429 => Err(rate_limited()),
        500..=599 => Err(dependency_unavailable()),
        _ => Err(invalid_response()),
    }
}

fn map_reqwest_error(error: &reqwest::Error) -> OpenApiClientError {
    if error.is_timeout() {
        timeout()
    } else if error.is_connect() || error.is_request() {
        dependency_unavailable()
    } else {
        invalid_response()
    }
}

const fn invalid_configuration() -> OpenApiClientError {
    OpenApiClientError {
        code: OpenApiClientErrorCode::InvalidConfiguration,
    }
}

const fn invalid_input() -> OpenApiClientError {
    OpenApiClientError {
        code: OpenApiClientErrorCode::InvalidInput,
    }
}

const fn authentication() -> OpenApiClientError {
    OpenApiClientError {
        code: OpenApiClientErrorCode::Authentication,
    }
}

const fn authorization() -> OpenApiClientError {
    OpenApiClientError {
        code: OpenApiClientErrorCode::Authorization,
    }
}

const fn not_found() -> OpenApiClientError {
    OpenApiClientError {
        code: OpenApiClientErrorCode::NotFound,
    }
}

const fn rate_limited() -> OpenApiClientError {
    OpenApiClientError {
        code: OpenApiClientErrorCode::RateLimited,
    }
}

const fn timeout() -> OpenApiClientError {
    OpenApiClientError {
        code: OpenApiClientErrorCode::Timeout,
    }
}

const fn dependency_unavailable() -> OpenApiClientError {
    OpenApiClientError {
        code: OpenApiClientErrorCode::DependencyUnavailable,
    }
}

const fn invalid_response() -> OpenApiClientError {
    OpenApiClientError {
        code: OpenApiClientErrorCode::InvalidResponse,
    }
}

const fn resource_exhausted() -> OpenApiClientError {
    OpenApiClientError {
        code: OpenApiClientErrorCode::ResourceExhausted,
    }
}
