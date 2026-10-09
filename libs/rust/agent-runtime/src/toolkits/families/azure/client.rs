use std::fmt::{self, Write as _};
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use percent_encoding::{AsciiSet, NON_ALPHANUMERIC, percent_decode_str, utf8_percent_encode};
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderMap, HeaderName, HeaderValue,
    USER_AGENT,
};
use reqwest::{Method, StatusCode, Url};
use serde_json::{Map, Value};
use tokio::time::timeout;
use zeroize::Zeroizing;

use super::config::AzureToolkitConfig;

const ARM_ORIGIN: &str = "https://management.azure.com";
const TOKEN_ORIGIN: &str = "https://login.microsoftonline.com";
const ARM_SCOPE: &str = "https://management.azure.com/.default";
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const TOKEN_TIMEOUT: Duration = Duration::from_secs(10);
const ARM_TIMEOUT: Duration = Duration::from_secs(20);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_URL_BYTES: usize = 8 * 1_024;
const MAX_REQUEST_BYTES: usize = 256 * 1_024;
const MAX_TOKEN_RESPONSE_BYTES: usize = 64 * 1_024;
const MAX_ARM_RESPONSE_BYTES: usize = 512 * 1_024;
const MAX_ACCESS_TOKEN_BYTES: usize = 32 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_HEADERS: usize = 64;
const MAX_HEADER_VALUE_BYTES: usize = 8 * 1_024;
const MAX_QUERY_VALUES: usize = 256;
const MAX_FILES: usize = 16;
const MAX_FILE_NAME_BYTES: usize = 255;
const MAX_FILE_CONTENT_BYTES: usize = 240 * 1_024;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";
const FORM_CONTENT_TYPE: &str = "application/x-www-form-urlencoded";
const JSON_CONTENT_TYPE: &str = "application/json";
const FORM_COMPONENT: &AsciiSet = &NON_ALPHANUMERIC
    .remove(b'-')
    .remove(b'.')
    .remove(b'_')
    .remove(b'~');

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AzureClientErrorCode {
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

/// Stable Azure failure without tenant, subscription, URL, headers, bodies, or credentials.
pub struct AzureClientError {
    code: AzureClientErrorCode,
    retryable: bool,
}

impl AzureClientError {
    #[must_use]
    pub const fn code(&self) -> AzureClientErrorCode {
        self.code
    }

    #[must_use]
    pub const fn retryable(&self) -> bool {
        self.retryable
    }

    pub fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            AzureClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "azure.configuration.invalid",
                "the Azure toolkit configuration is invalid",
            ),
            AzureClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "azure.request.invalid",
                "the Azure Resource Manager request is invalid",
            ),
            AzureClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "azure.authentication.failed",
                "Microsoft Entra client authentication failed",
            ),
            AzureClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "azure.authorization.failed",
                "Azure did not authorize the Resource Manager request",
            ),
            AzureClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "azure.resource.not_found",
                "the requested Azure resource was not found",
            ),
            AzureClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "azure.rate_limited",
                "Azure Resource Manager rate limited the request",
            ),
            AzureClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "azure.timeout",
                "the Azure request timed out before an effect was dispatched",
            ),
            AzureClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "azure.unavailable",
                "Microsoft Entra or Azure Resource Manager is unavailable",
            ),
            AzureClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "azure.response.invalid",
                "Azure returned an invalid response",
            ),
            AzureClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "azure.resource_exhausted",
                "the Azure request or response exceeds the approved limit",
            ),
            AzureClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "azure.effect.unknown_outcome",
                "the Azure effect may have occurred; reconcile resource state before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    fn health_message(&self) -> &'static str {
        match self.code {
            AzureClientErrorCode::Authentication => "authentication failed",
            AzureClientErrorCode::Authorization => "resource-group listing is not authorized",
            AzureClientErrorCode::NotFound => "the configured subscription was not found",
            AzureClientErrorCode::RateLimited => "Azure rate limited the health request",
            AzureClientErrorCode::Timeout => "the health request timed out",
            AzureClientErrorCode::DependencyUnavailable => "Azure is unavailable",
            AzureClientErrorCode::InvalidConfiguration
            | AzureClientErrorCode::InvalidInput
            | AzureClientErrorCode::InvalidResponse
            | AzureClientErrorCode::ResourceExhausted
            | AzureClientErrorCode::UnknownOutcome => "the health response was invalid",
        }
    }

    #[cfg(any(test, feature = "test-support"))]
    pub const fn fixture(code: AzureClientErrorCode, retryable: bool) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for AzureClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AzureClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for AzureClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            AzureClientErrorCode::InvalidConfiguration => {
                "the Azure client configuration is invalid"
            }
            AzureClientErrorCode::InvalidInput => "the Azure request is invalid",
            AzureClientErrorCode::Authentication => "Azure authentication failed",
            AzureClientErrorCode::Authorization => "Azure authorization failed",
            AzureClientErrorCode::NotFound => "the Azure resource was not found",
            AzureClientErrorCode::RateLimited => "Azure rate limited the request",
            AzureClientErrorCode::Timeout => "the Azure request timed out",
            AzureClientErrorCode::DependencyUnavailable => "Azure is unavailable",
            AzureClientErrorCode::InvalidResponse => "Azure returned an invalid response",
            AzureClientErrorCode::ResourceExhausted => {
                "the Azure request or response exceeds its approved limit"
            }
            AzureClientErrorCode::UnknownOutcome => {
                "the Azure effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for AzureClientError {}

#[async_trait]
pub trait AzureApi: Send + Sync {
    async fn execute(
        &self,
        method: &str,
        url: &str,
        optional_args: &Map<String, Value>,
    ) -> Result<Value, AzureClientError>;

    async fn healthcheck(&self) -> Value;
}

pub struct AzureHttpResponse {
    status: StatusCode,
    body: Vec<u8>,
}

impl AzureHttpResponse {
    #[cfg(any(test, feature = "test-support"))]
    pub fn fixture(status: StatusCode, body: impl Into<Vec<u8>>) -> Self {
        Self {
            status,
            body: body.into(),
        }
    }
}

pub enum AzureRequestBody {
    Empty,
    Bytes(Vec<u8>),
    Multipart {
        fields: Vec<(String, String)>,
        files: Vec<InlineFile>,
    },
}

pub struct InlineFile {
    pub field: String,
    pub filename: String,
    pub content: Vec<u8>,
    pub content_type: Option<String>,
    pub headers: HeaderMap,
}

pub struct AzureRequest {
    method: Method,
    url: Url,
    headers: HeaderMap,
    body: AzureRequestBody,
}

impl AzureRequest {
    #[cfg(any(test, feature = "test-support"))]
    pub fn method(&self) -> &Method {
        &self.method
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn url(&self) -> &Url {
        &self.url
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn headers(&self) -> &HeaderMap {
        &self.headers
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn body(&self) -> &AzureRequestBody {
        &self.body
    }
}

#[async_trait]
pub trait AzureTransport: Send + Sync {
    async fn execute(
        &self,
        request: AzureRequest,
        effect: bool,
        response_limit: usize,
    ) -> Result<AzureHttpResponse, AzureClientError>;
}

struct ReqwestAzureTransport {
    http: reqwest::Client,
}

#[async_trait]
impl AzureTransport for ReqwestAzureTransport {
    async fn execute(
        &self,
        request: AzureRequest,
        effect: bool,
        response_limit: usize,
    ) -> Result<AzureHttpResponse, AzureClientError> {
        let mut builder = self
            .http
            .request(request.method, request.url)
            .headers(request.headers);
        builder = match request.body {
            AzureRequestBody::Empty => builder,
            AzureRequestBody::Bytes(body) => builder.body(body),
            AzureRequestBody::Multipart { fields, files } => {
                let mut form = reqwest::multipart::Form::new();
                for (name, value) in fields {
                    form = form.text(name, value);
                }
                for file in files {
                    let mut part = reqwest::multipart::Part::bytes(file.content)
                        .file_name(file.filename)
                        .headers(file.headers);
                    if let Some(content_type) = file.content_type {
                        part = part.mime_str(&content_type).map_err(|_| invalid_input())?;
                    }
                    form = form.part(file.field, part);
                }
                builder.multipart(form)
            }
        };
        let request = builder.build().map_err(|_| invalid_input())?;
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
        Ok(AzureHttpResponse { status, body })
    }
}

/// Invocation-owned Microsoft Entra credential and fixed ARM subscription authority.
pub struct AzureClient {
    config: AzureToolkitConfig,
    transport: Arc<dyn AzureTransport>,
}

impl AzureClient {
    pub fn new(config: AzureToolkitConfig) -> Result<Self, AzureClientError> {
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
            transport: Arc::new(ReqwestAzureTransport { http }),
        })
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn with_transport(config: AzureToolkitConfig, transport: Arc<dyn AzureTransport>) -> Self {
        Self { config, transport }
    }

    fn token_url(&self) -> Result<Url, AzureClientError> {
        Url::parse(&format!(
            "{}/{}/oauth2/v2.0/token",
            TOKEN_ORIGIN,
            utf8_percent_encode(self.config.tenant_id(), FORM_COMPONENT)
        ))
        .map_err(|_| invalid_configuration())
    }

    fn token_request(&self) -> Result<AzureRequest, AzureClientError> {
        let mut body = Zeroizing::new(String::new());
        append_form_pair(&mut body, "client_id", self.config.client_id())?;
        append_form_pair(&mut body, "client_secret", self.config.client_secret())?;
        append_form_pair(&mut body, "grant_type", "client_credentials")?;
        append_form_pair(&mut body, "scope", ARM_SCOPE)?;
        if body.len() > MAX_REQUEST_BYTES {
            return Err(resource_exhausted());
        }
        let mut headers = default_headers();
        headers.insert(CONTENT_TYPE, HeaderValue::from_static(FORM_CONTENT_TYPE));
        Ok(AzureRequest {
            method: Method::POST,
            url: self.token_url()?,
            headers,
            body: AzureRequestBody::Bytes(body.as_bytes().to_vec()),
        })
    }

    async fn access_token(&self) -> Result<Zeroizing<String>, AzureClientError> {
        let response = timeout(
            TOKEN_TIMEOUT,
            self.transport
                .execute(self.token_request()?, false, MAX_TOKEN_RESPONSE_BYTES),
        )
        .await
        .map_err(|_| timeout_error(true))??;
        map_token_status(response.status)?;
        let body = Zeroizing::new(response.body);
        let mut value: Value = serde_json::from_slice(&body).map_err(|_| invalid_response())?;
        let token = value
            .as_object_mut()
            .and_then(|object| object.remove("access_token"))
            .and_then(|token| token.as_str().map(ToOwned::to_owned))
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

    fn arm_request(
        &self,
        method: Method,
        url: &str,
        optional_args: &Map<String, Value>,
        token: Option<&str>,
    ) -> Result<AzureRequest, AzureClientError> {
        let mut url = validate_arm_url(url, self.config.subscription_id())?;
        reject_unknown_options(optional_args)?;
        append_query(&mut url, optional_args.get("params"))?;
        let mut headers = parse_headers(optional_args.get("headers"))?;
        if let Some(token) = token {
            headers.insert(AUTHORIZATION, bearer_header(token)?);
        }
        headers.insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
        headers
            .entry(ACCEPT)
            .or_insert(HeaderValue::from_static(JSON_CONTENT_TYPE));
        let body = build_body(optional_args, &mut headers)?;
        Ok(AzureRequest {
            method,
            url,
            headers,
            body,
        })
    }

    async fn execute_inner(
        &self,
        method: &str,
        url: &str,
        optional_args: &Map<String, Value>,
    ) -> Result<Value, AzureClientError> {
        let method = parse_method(method)?;
        let effect = is_effect(&method);
        let mut request = self.arm_request(method, url, optional_args, None)?;
        let token = self.access_token().await?;
        request
            .headers
            .insert(AUTHORIZATION, bearer_header(&token)?);
        let response = timeout(
            ARM_TIMEOUT,
            self.transport
                .execute(request, effect, MAX_ARM_RESPONSE_BYTES),
        )
        .await
        .map_err(|_| {
            if effect {
                unknown_outcome()
            } else {
                timeout_error(true)
            }
        })??;
        map_arm_status(response.status, effect)?;
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

    fn health_url(&self) -> String {
        format!(
            "{ARM_ORIGIN}/subscriptions/{}/resourcegroups?api-version=2021-04-01",
            self.config.subscription_id()
        )
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn test_token_request(&self) -> Result<AzureRequest, AzureClientError> {
        self.token_request()
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn test_arm_request(
        &self,
        method: &str,
        url: &str,
        optional_args: &Map<String, Value>,
        token: &str,
    ) -> Result<AzureRequest, AzureClientError> {
        self.arm_request(parse_method(method)?, url, optional_args, Some(token))
    }
}

#[async_trait]
impl AzureApi for AzureClient {
    async fn execute(
        &self,
        method: &str,
        url: &str,
        optional_args: &Map<String, Value>,
    ) -> Result<Value, AzureClientError> {
        self.execute_inner(method, url, optional_args).await
    }

    async fn healthcheck(&self) -> Value {
        match self
            .execute_inner("GET", &self.health_url(), &Map::new())
            .await
        {
            Ok(_) => serde_json::json!([true, ""]),
            Err(error) => serde_json::json!([false, error.health_message()]),
        }
    }
}

fn default_headers() -> HeaderMap {
    let mut headers = HeaderMap::new();
    headers.insert(ACCEPT, HeaderValue::from_static(JSON_CONTENT_TYPE));
    headers.insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
    headers
}

fn append_form_pair(body: &mut String, name: &str, value: &str) -> Result<(), AzureClientError> {
    if !body.is_empty() {
        body.push('&');
    }
    write!(
        body,
        "{}={}",
        utf8_percent_encode(name, FORM_COMPONENT),
        utf8_percent_encode(value, FORM_COMPONENT)
    )
    .map_err(|_| invalid_input())
}

fn reject_unknown_options(options: &Map<String, Value>) -> Result<(), AzureClientError> {
    if options.keys().any(|key| {
        !matches!(
            key.as_str(),
            "headers" | "params" | "json" | "data" | "files"
        )
    }) {
        return Err(invalid_input());
    }
    Ok(())
}

fn parse_headers(value: Option<&Value>) -> Result<HeaderMap, AzureClientError> {
    let Some(value) = value else {
        return Ok(HeaderMap::new());
    };
    let object = value.as_object().ok_or_else(invalid_input)?;
    if object.len() > MAX_HEADERS {
        return Err(resource_exhausted());
    }
    let mut headers = HeaderMap::with_capacity(object.len() + 3);
    let mut total_bytes = 0usize;
    for (name, value) in object {
        if is_reserved_header(name) {
            return Err(invalid_input());
        }
        let value = value.as_str().ok_or_else(invalid_input)?;
        if value.len() > MAX_HEADER_VALUE_BYTES {
            return Err(resource_exhausted());
        }
        total_bytes = total_bytes
            .checked_add(name.len())
            .and_then(|total| total.checked_add(value.len()))
            .ok_or_else(resource_exhausted)?;
        if total_bytes > MAX_REQUEST_BYTES {
            return Err(resource_exhausted());
        }
        let name = HeaderName::from_bytes(name.as_bytes()).map_err(|_| invalid_input())?;
        let value = HeaderValue::from_str(value).map_err(|_| invalid_input())?;
        headers.insert(name, value);
    }
    Ok(headers)
}

fn is_reserved_header(name: &str) -> bool {
    matches!(
        name.to_ascii_lowercase().as_str(),
        "authorization"
            | "host"
            | "content-length"
            | "transfer-encoding"
            | "connection"
            | "proxy-authorization"
            | "proxy-authenticate"
            | "te"
            | "trailer"
            | "upgrade"
    )
}

fn append_query(url: &mut Url, value: Option<&Value>) -> Result<(), AzureClientError> {
    let Some(value) = value else {
        return Ok(());
    };
    let pairs = scalar_pairs(value)?;
    if pairs.len() > MAX_QUERY_VALUES {
        return Err(resource_exhausted());
    }
    {
        let mut query = url.query_pairs_mut();
        for (name, value) in pairs {
            query.append_pair(&name, &value);
        }
    }
    if url.as_str().len() > MAX_URL_BYTES {
        return Err(resource_exhausted());
    }
    Ok(())
}

fn build_body(
    options: &Map<String, Value>,
    headers: &mut HeaderMap,
) -> Result<AzureRequestBody, AzureClientError> {
    let data = options.get("data");
    let json = options.get("json");
    let files = options.get("files");
    if json.is_some() && (data.is_some() || files.is_some()) {
        return Err(invalid_input());
    }
    if let Some(files) = files {
        let fields = match data {
            None => Vec::new(),
            Some(data) if data.is_object() => scalar_pairs(data)?,
            Some(_) => return Err(invalid_input()),
        };
        let files = parse_files(files)?;
        if headers.contains_key(CONTENT_TYPE) {
            return Err(invalid_input());
        }
        enforce_multipart_bound(&fields, &files)?;
        return Ok(AzureRequestBody::Multipart { fields, files });
    }
    if let Some(value) = json {
        let body = serde_json::to_vec(value).map_err(|_| invalid_input())?;
        enforce_request_bound(&body)?;
        headers
            .entry(CONTENT_TYPE)
            .or_insert(HeaderValue::from_static(JSON_CONTENT_TYPE));
        return Ok(AzureRequestBody::Bytes(body));
    }
    let Some(data) = data else {
        return Ok(AzureRequestBody::Empty);
    };
    if let Some(text) = data.as_str() {
        enforce_request_bound(text.as_bytes())?;
        return Ok(AzureRequestBody::Bytes(text.as_bytes().to_vec()));
    }
    let pairs = scalar_pairs(data)?;
    let mut body = String::new();
    for (name, value) in pairs {
        append_form_pair(&mut body, &name, &value)?;
    }
    enforce_request_bound(body.as_bytes())?;
    headers
        .entry(CONTENT_TYPE)
        .or_insert(HeaderValue::from_static(FORM_CONTENT_TYPE));
    Ok(AzureRequestBody::Bytes(body.into_bytes()))
}

fn scalar_pairs(value: &Value) -> Result<Vec<(String, String)>, AzureClientError> {
    let object = value.as_object().ok_or_else(invalid_input)?;
    let mut pairs = Vec::new();
    for (name, value) in object {
        if name.is_empty() || name.len() > MAX_HEADER_VALUE_BYTES {
            return Err(invalid_input());
        }
        match value {
            Value::Array(values) => {
                for value in values {
                    pairs.push((name.clone(), scalar_text(value)?));
                }
            }
            Value::Null => {}
            value => pairs.push((name.clone(), scalar_text(value)?)),
        }
        if pairs.len() > MAX_QUERY_VALUES {
            return Err(resource_exhausted());
        }
    }
    Ok(pairs)
}

fn scalar_text(value: &Value) -> Result<String, AzureClientError> {
    let value = match value {
        Value::String(value) => value.clone(),
        Value::Bool(value) => value.to_string(),
        Value::Number(value) => value.to_string(),
        Value::Null | Value::Array(_) | Value::Object(_) => return Err(invalid_input()),
    };
    if value.len() > MAX_HEADER_VALUE_BYTES {
        return Err(resource_exhausted());
    }
    Ok(value)
}

fn parse_files(value: &Value) -> Result<Vec<InlineFile>, AzureClientError> {
    let object = value.as_object().ok_or_else(invalid_input)?;
    if object.is_empty() || object.len() > MAX_FILES {
        return Err(if object.len() > MAX_FILES {
            resource_exhausted()
        } else {
            invalid_input()
        });
    }
    let mut total = 0usize;
    let mut files = Vec::with_capacity(object.len());
    for (field, value) in object {
        validate_multipart_name(field)?;
        let (filename, content, content_type, headers) = parse_file_value(field, value)?;
        total = total
            .checked_add(content.len())
            .ok_or_else(resource_exhausted)?;
        if total > MAX_FILE_CONTENT_BYTES {
            return Err(resource_exhausted());
        }
        files.push(InlineFile {
            field: field.clone(),
            filename,
            content,
            content_type,
            headers,
        });
    }
    Ok(files)
}

type ParsedFileValue = (String, Vec<u8>, Option<String>, HeaderMap);

fn parse_file_value(field: &str, value: &Value) -> Result<ParsedFileValue, AzureClientError> {
    if let Some(content) = value.as_str() {
        enforce_file_content(content)?;
        return Ok((
            field.to_owned(),
            content.as_bytes().to_vec(),
            None,
            HeaderMap::new(),
        ));
    }
    let values = value.as_array().ok_or_else(invalid_input)?;
    if !(2..=4).contains(&values.len()) {
        return Err(invalid_input());
    }
    let filename = values[0].as_str().ok_or_else(invalid_input)?;
    validate_filename(filename)?;
    let content = values[1].as_str().ok_or_else(invalid_input)?;
    enforce_file_content(content)?;
    let content_type = values
        .get(2)
        .filter(|value| !value.is_null())
        .map(|value| value.as_str().ok_or_else(invalid_input))
        .transpose()?
        .map(ToOwned::to_owned);
    if content_type
        .as_ref()
        .is_some_and(|value| value.len() > 255 || value.bytes().any(|byte| byte.is_ascii_control()))
    {
        return Err(invalid_input());
    }
    let headers = parse_headers(values.get(3))?;
    if headers.contains_key(reqwest::header::CONTENT_DISPOSITION)
        || headers.contains_key(CONTENT_TYPE)
    {
        return Err(invalid_input());
    }
    if let Some(content_type) = &content_type {
        reqwest::multipart::Part::bytes(Vec::new())
            .mime_str(content_type)
            .map_err(|_| invalid_input())?;
    }
    Ok((
        filename.to_owned(),
        content.as_bytes().to_vec(),
        content_type,
        headers,
    ))
}

fn validate_multipart_name(value: &str) -> Result<(), AzureClientError> {
    if value.is_empty()
        || value.len() > MAX_FILE_NAME_BYTES
        || value
            .bytes()
            .any(|byte| byte.is_ascii_control() || matches!(byte, b'"' | b'\\'))
    {
        return Err(invalid_input());
    }
    Ok(())
}

fn validate_filename(value: &str) -> Result<(), AzureClientError> {
    validate_multipart_name(value)?;
    if value.contains('/') || value.contains("..") {
        return Err(invalid_input());
    }
    Ok(())
}

fn enforce_file_content(value: &str) -> Result<(), AzureClientError> {
    if value.len() > MAX_FILE_CONTENT_BYTES {
        return Err(resource_exhausted());
    }
    Ok(())
}

fn enforce_request_bound(body: &[u8]) -> Result<(), AzureClientError> {
    if body.len() > MAX_REQUEST_BYTES {
        return Err(resource_exhausted());
    }
    Ok(())
}

fn enforce_multipart_bound(
    fields: &[(String, String)],
    files: &[InlineFile],
) -> Result<(), AzureClientError> {
    let mut total = files
        .len()
        .checked_mul(1_024)
        .and_then(|value| {
            fields
                .len()
                .checked_mul(256)
                .and_then(|overhead| value.checked_add(overhead))
        })
        .ok_or_else(resource_exhausted)?;
    for (name, value) in fields {
        total = total
            .checked_add(name.len())
            .and_then(|total| total.checked_add(value.len()))
            .ok_or_else(resource_exhausted)?;
    }
    for file in files {
        total = total
            .checked_add(file.field.len())
            .and_then(|value| value.checked_add(file.filename.len()))
            .and_then(|value| value.checked_add(file.content.len()))
            .and_then(|value| value.checked_add(file.content_type.as_ref().map_or(0, String::len)))
            .ok_or_else(resource_exhausted)?;
        for (name, value) in &file.headers {
            total = total
                .checked_add(name.as_str().len())
                .and_then(|total| total.checked_add(value.as_bytes().len()))
                .ok_or_else(resource_exhausted)?;
        }
    }
    if total > MAX_REQUEST_BYTES {
        return Err(resource_exhausted());
    }
    Ok(())
}

fn bearer_header(token: &str) -> Result<HeaderValue, AzureClientError> {
    let mut value = Zeroizing::new(String::with_capacity("Bearer ".len() + token.len()));
    value.push_str("Bearer ");
    value.push_str(token);
    HeaderValue::from_str(&value).map_err(|_| invalid_response())
}

pub fn parse_method(method: &str) -> Result<Method, AzureClientError> {
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

pub fn validate_arm_url(raw: &str, subscription_id: &str) -> Result<Url, AzureClientError> {
    if raw.is_empty()
        || raw.len() > MAX_URL_BYTES
        || raw
            .bytes()
            .any(|byte| byte.is_ascii_control() || byte == b'\\')
    {
        return Err(if raw.len() > MAX_URL_BYTES {
            resource_exhausted()
        } else {
            invalid_input()
        });
    }
    if raw
        .strip_prefix("https://")
        .and_then(|rest| rest.split('/').next())
        != Some("management.azure.com")
    {
        return Err(invalid_input());
    }
    validate_percent_encoding(raw)?;
    let url = Url::parse(raw).map_err(|_| invalid_input())?;
    if url.scheme() != "https"
        || url.host_str() != Some("management.azure.com")
        || url.port().is_some()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.fragment().is_some()
    {
        return Err(invalid_input());
    }
    let segments = url
        .path_segments()
        .ok_or_else(invalid_input)?
        .map(percent_decode_str)
        .map(|segment| segment.decode_utf8().map_err(|_| invalid_input()))
        .collect::<Result<Vec<_>, _>>()?;
    if segments.len() < 2
        || !segments[0].eq_ignore_ascii_case("subscriptions")
        || !segments[1].eq_ignore_ascii_case(subscription_id)
    {
        return Err(invalid_input());
    }
    for segment in &segments {
        if matches!(segment.as_ref(), "." | "..")
            || segment
                .bytes()
                .any(|byte| byte.is_ascii_control() || matches!(byte, b'/' | b'\\'))
            || contains_percent_escape(segment)
        {
            return Err(invalid_input());
        }
    }
    Ok(url)
}

fn validate_percent_encoding(value: &str) -> Result<(), AzureClientError> {
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

fn map_token_status(status: StatusCode) -> Result<(), AzureClientError> {
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

fn map_arm_status(status: StatusCode, effect: bool) -> Result<(), AzureClientError> {
    if status.is_success() {
        return Ok(());
    }
    Err(match status {
        StatusCode::BAD_REQUEST
        | StatusCode::CONFLICT
        | StatusCode::PRECONDITION_FAILED
        | StatusCode::UNPROCESSABLE_ENTITY => invalid_input(),
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
        status if status.is_redirection() && effect => unknown_outcome(),
        _ => invalid_response(),
    })
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> AzureClientError {
    if effect {
        return unknown_outcome();
    }
    if source.is_timeout() {
        timeout_error(true)
    } else if source.is_connect() || source.is_request() || source.is_body() || source.is_decode() {
        unavailable(true)
    } else {
        invalid_response()
    }
}

const fn post_accept_failure(effect: bool) -> AzureClientError {
    if effect {
        unknown_outcome()
    } else {
        invalid_response()
    }
}

const fn post_accept_bound_failure(effect: bool) -> AzureClientError {
    if effect {
        unknown_outcome()
    } else {
        resource_exhausted()
    }
}

const fn response_bound_failure(effect: bool) -> AzureClientError {
    post_accept_bound_failure(effect)
}

const fn invalid_configuration() -> AzureClientError {
    AzureClientError {
        code: AzureClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

const fn invalid_input() -> AzureClientError {
    AzureClientError {
        code: AzureClientErrorCode::InvalidInput,
        retryable: false,
    }
}

const fn authentication() -> AzureClientError {
    AzureClientError {
        code: AzureClientErrorCode::Authentication,
        retryable: false,
    }
}

const fn authorization() -> AzureClientError {
    AzureClientError {
        code: AzureClientErrorCode::Authorization,
        retryable: false,
    }
}

const fn not_found() -> AzureClientError {
    AzureClientError {
        code: AzureClientErrorCode::NotFound,
        retryable: false,
    }
}

const fn rate_limited(retryable: bool) -> AzureClientError {
    AzureClientError {
        code: AzureClientErrorCode::RateLimited,
        retryable,
    }
}

const fn timeout_error(retryable: bool) -> AzureClientError {
    AzureClientError {
        code: AzureClientErrorCode::Timeout,
        retryable,
    }
}

const fn unavailable(retryable: bool) -> AzureClientError {
    AzureClientError {
        code: AzureClientErrorCode::DependencyUnavailable,
        retryable,
    }
}

const fn invalid_response() -> AzureClientError {
    AzureClientError {
        code: AzureClientErrorCode::InvalidResponse,
        retryable: false,
    }
}

const fn resource_exhausted() -> AzureClientError {
    AzureClientError {
        code: AzureClientErrorCode::ResourceExhausted,
        retryable: false,
    }
}

const fn unknown_outcome() -> AzureClientError {
    AzureClientError {
        code: AzureClientErrorCode::UnknownOutcome,
        retryable: false,
    }
}
