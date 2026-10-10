use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64_STANDARD;
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, ETAG, HeaderValue, IF_MATCH,
};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::Value;
use zeroize::Zeroizing;

use super::config::AdoConnection;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_REQUEST_BYTES: usize = 2 * 1_024 * 1_024;
/// One provider response. A repository file read is bounded by this too.
pub(crate) const MAX_RESPONSE_BYTES: usize = 4 * 1_024 * 1_024;
/// One tool result, serialized.
pub(crate) const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_PROVIDER_MESSAGE_BYTES: usize = 1_024;
const MAX_ETAG_BYTES: usize = 1_024;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";
const JSON_CONTENT_TYPE: &str = "application/json";
const JSON_PATCH_CONTENT_TYPE: &str = "application/json-patch+json";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum AdoClientErrorCode {
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

/// Stable Azure DevOps failure.
///
/// The provider's own error text is kept privately and bounded only so a
/// family can branch on a documented provider condition (the wiki's
/// "version either is invalid or does not exist" retry); it is never part of
/// `Debug`, `Display` or the model-visible error.
pub(crate) struct AdoClientError {
    code: AdoClientErrorCode,
    retryable: bool,
    provider_message: Option<Box<str>>,
}

impl AdoClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> AdoClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    /// Whether the provider's error text contains `needle`.
    pub(crate) fn provider_message_contains(&self, needle: &str) -> bool {
        self.provider_message
            .as_deref()
            .is_some_and(|message| message.contains(needle))
    }

    /// The provider's own text for a refused request (HTTP 400/422), which
    /// the SDK hands the model for create/update validation failures such as
    /// `TF401320: Rule Error for field ...`. Nothing else is ever exposed.
    pub(crate) fn model_visible_message(&self) -> Option<&str> {
        (self.code == AdoClientErrorCode::InvalidInput)
            .then_some(self.provider_message.as_deref())
            .flatten()
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            AdoClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "ado.configuration.invalid",
                "the Azure DevOps toolkit configuration is invalid",
            ),
            AdoClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "ado.request.invalid",
                "the Azure DevOps request is invalid",
            ),
            AdoClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "ado.authentication.failed",
                "Azure DevOps authentication failed",
            ),
            AdoClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "ado.authorization.failed",
                "Azure DevOps did not authorize the request",
            ),
            AdoClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "ado.resource.not_found",
                "the requested Azure DevOps resource was not found",
            ),
            AdoClientErrorCode::Conflict => (
                ErrorCategory::InvalidInput,
                "ado.resource.conflict",
                "the Azure DevOps request conflicts with current provider state",
            ),
            AdoClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "ado.rate_limited",
                "Azure DevOps rate limited the request",
            ),
            AdoClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "ado.timeout",
                "the Azure DevOps request timed out",
            ),
            AdoClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "ado.unavailable",
                "Azure DevOps is unavailable",
            ),
            AdoClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "ado.response.invalid",
                "Azure DevOps returned an invalid response",
            ),
            AdoClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "ado.response.resource_exhausted",
                "the Azure DevOps response exceeds the approved limit",
            ),
            AdoClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "ado.effect.unknown_outcome",
                "Azure DevOps may have applied the requested effect; reconcile it before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(code: AdoClientErrorCode, retryable: bool) -> Self {
        Self {
            code,
            retryable,
            provider_message: None,
        }
    }
}

impl fmt::Debug for AdoClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AdoClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for AdoClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            AdoClientErrorCode::InvalidConfiguration => {
                "the Azure DevOps client configuration is invalid"
            }
            AdoClientErrorCode::InvalidInput => "the Azure DevOps request is invalid",
            AdoClientErrorCode::Authentication => "Azure DevOps authentication failed",
            AdoClientErrorCode::Authorization => "Azure DevOps authorization failed",
            AdoClientErrorCode::NotFound => "the Azure DevOps resource was not found",
            AdoClientErrorCode::Conflict => "the Azure DevOps request conflicts",
            AdoClientErrorCode::RateLimited => "Azure DevOps rate limited the request",
            AdoClientErrorCode::Timeout => "the Azure DevOps request timed out",
            AdoClientErrorCode::DependencyUnavailable => "Azure DevOps is unavailable",
            AdoClientErrorCode::InvalidResponse => "Azure DevOps returned an invalid response",
            AdoClientErrorCode::ResourceExhausted => {
                "the Azure DevOps response exceeds its approved limit"
            }
            AdoClientErrorCode::UnknownOutcome => {
                "the Azure DevOps effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for AdoClientError {}

/// A decoded provider body.
#[derive(Clone, Debug, PartialEq)]
pub(crate) enum AdoBody {
    Empty,
    Json(Value),
    Text(String),
}

pub(crate) struct AdoHttpResponse {
    status: StatusCode,
    body: AdoBody,
    etag: Option<Box<str>>,
}

impl AdoHttpResponse {
    pub(crate) fn body(&self) -> &AdoBody {
        &self.body
    }

    pub(crate) fn into_body(self) -> AdoBody {
        self.body
    }

    pub(crate) fn etag(&self) -> Option<&str> {
        self.etag.as_deref()
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn fixture(status: StatusCode, body: AdoBody) -> Self {
        Self {
            status,
            body,
            etag: None,
        }
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn with_etag(mut self, etag: &str) -> Self {
        self.etag = Some(etag.into());
        self
    }
}

/// The single outbound path of every ADO family. The production
/// implementation is HTTPS-only, follows no redirect, retries nothing and
/// bounds every body; tests replace it with recorded provider fixtures.
#[async_trait]
pub(crate) trait AdoTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<AdoHttpResponse, AdoClientError>;
}

struct ReqwestAdoTransport {
    http: reqwest::Client,
}

#[async_trait]
impl AdoTransport for ReqwestAdoTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<AdoHttpResponse, AdoClientError> {
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
        let etag = response
            .headers()
            .get(ETAG)
            .and_then(|value| value.to_str().ok())
            .filter(|value| value.len() <= MAX_ETAG_BYTES)
            .map(Into::into);
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
        let body = if bytes.is_empty() {
            AdoBody::Empty
        } else if json_content_type {
            match serde_json::from_slice(&bytes) {
                Ok(value) => AdoBody::Json(value),
                Err(_) if !status.is_success() => AdoBody::Empty,
                Err(_) => return Err(response_shape_failure(effect)),
            }
        } else {
            match String::from_utf8(bytes) {
                Ok(text) => AdoBody::Text(text),
                Err(_) if !status.is_success() => AdoBody::Empty,
                Err(_) => return Err(response_shape_failure(effect)),
            }
        };
        Ok(AdoHttpResponse { status, body, etag })
    }
}

/// Whether a path is scoped by the configured project.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum AdoScope {
    Organization,
    Project,
}

pub(crate) enum AdoRequestBody {
    None,
    Json(Value),
    JsonPatch(Value),
}

/// One Azure DevOps REST call: `{organization}[/{project}]/_apis/{segments}`
/// with the route's `api-version`, exactly as the `azure-devops` client
/// builds it.
pub(crate) struct AdoRequest<'a> {
    pub(crate) method: Method,
    pub(crate) scope: AdoScope,
    pub(crate) segments: &'a [&'a str],
    pub(crate) query: Vec<(&'static str, String)>,
    pub(crate) api_version: &'static str,
    pub(crate) body: AdoRequestBody,
    pub(crate) accept_text: bool,
    pub(crate) if_match: Option<&'a str>,
}

impl<'a> AdoRequest<'a> {
    pub(crate) fn new(
        method: Method,
        scope: AdoScope,
        segments: &'a [&'a str],
        api_version: &'static str,
    ) -> Self {
        Self {
            method,
            scope,
            segments,
            query: Vec::new(),
            api_version,
            body: AdoRequestBody::None,
            accept_text: false,
            if_match: None,
        }
    }

    pub(crate) fn get(segments: &'a [&'a str], api_version: &'static str) -> Self {
        Self::new(Method::GET, AdoScope::Project, segments, api_version)
    }

    #[must_use]
    pub(crate) fn query(mut self, name: &'static str, value: impl Into<String>) -> Self {
        self.query.push((name, value.into()));
        self
    }

    #[must_use]
    pub(crate) fn query_opt(self, name: &'static str, value: Option<impl Into<String>>) -> Self {
        match value {
            Some(value) => self.query(name, value),
            None => self,
        }
    }

    #[must_use]
    pub(crate) fn json(mut self, body: Value) -> Self {
        self.body = AdoRequestBody::Json(body);
        self
    }

    #[must_use]
    pub(crate) fn json_patch(mut self, body: Value) -> Self {
        self.body = AdoRequestBody::JsonPatch(body);
        self
    }

    #[must_use]
    pub(crate) const fn organization(mut self) -> Self {
        self.scope = AdoScope::Organization;
        self
    }

    #[must_use]
    pub(crate) const fn text(mut self) -> Self {
        self.accept_text = true;
        self
    }

    #[must_use]
    pub(crate) const fn if_match(mut self, version: Option<&'a str>) -> Self {
        self.if_match = version;
        self
    }
}

/// One claim-scoped Azure DevOps client shared by the four ADO families.
pub(crate) struct AdoClient {
    connection: AdoConnection,
    transport: Arc<dyn AdoTransport>,
}

impl AdoClient {
    pub(crate) fn new(connection: AdoConnection) -> Result<Self, AdoClientError> {
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
            connection,
            transport: Arc::new(ReqwestAdoTransport { http }),
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn with_transport(
        connection: AdoConnection,
        transport: Arc<dyn AdoTransport>,
    ) -> Self {
        Self {
            connection,
            transport,
        }
    }

    pub(crate) fn organization_text(&self) -> &str {
        self.connection.organization_text()
    }

    pub(crate) fn project(&self) -> &str {
        self.connection.project()
    }

    fn url(&self, request: &AdoRequest<'_>) -> Result<Url, AdoClientError> {
        let mut url = self.connection.organization_url().clone();
        {
            let mut path = url
                .path_segments_mut()
                .map_err(|()| invalid_configuration())?;
            path.pop_if_empty();
            if request.scope == AdoScope::Project {
                path.push(self.connection.project());
            }
            path.push("_apis");
            for segment in request.segments {
                if segment.is_empty() || matches!(*segment, "." | "..") {
                    return Err(invalid_input());
                }
                path.push(segment);
            }
        }
        {
            let mut pairs = url.query_pairs_mut();
            for (name, value) in &request.query {
                pairs.append_pair(name, value);
            }
            pairs.append_pair("api-version", request.api_version);
        }
        Ok(url)
    }

    /// Send one request and map its status. Only a 2xx response comes back.
    pub(crate) async fn send(
        &self,
        request: AdoRequest<'_>,
        effect: bool,
    ) -> Result<AdoHttpResponse, AdoClientError> {
        let url = self.url(&request)?;
        let mut http = Request::new(request.method.clone(), url);
        let credential =
            Zeroizing::new(BASE64_STANDARD.encode(format!(":{}", self.connection.token())));
        let mut authorization = HeaderValue::from_str(&format!("Basic {}", credential.as_str()))
            .map_err(|_| invalid_configuration())?;
        authorization.set_sensitive(true);
        http.headers_mut().insert(AUTHORIZATION, authorization);
        http.headers_mut().insert(
            ACCEPT,
            HeaderValue::from_static(if request.accept_text {
                "text/plain"
            } else {
                JSON_CONTENT_TYPE
            }),
        );
        if let Some(version) = request.if_match {
            http.headers_mut().insert(
                IF_MATCH,
                HeaderValue::from_str(version).map_err(|_| invalid_input())?,
            );
        }
        let (content_type, body) = match request.body {
            AdoRequestBody::None => (None, None),
            AdoRequestBody::Json(body) => (Some(JSON_CONTENT_TYPE), Some(body)),
            AdoRequestBody::JsonPatch(body) => (Some(JSON_PATCH_CONTENT_TYPE), Some(body)),
        };
        if let (Some(content_type), Some(body)) = (content_type, body) {
            let encoded = serde_json::to_vec(&body).map_err(|_| invalid_input())?;
            if encoded.len() > MAX_REQUEST_BYTES {
                return Err(AdoClientError {
                    code: AdoClientErrorCode::ResourceExhausted,
                    retryable: false,
                    provider_message: None,
                });
            }
            http.headers_mut()
                .insert(CONTENT_TYPE, HeaderValue::from_static(content_type));
            *http.body_mut() = Some(encoded.into());
        }
        let response = self.transport.execute(http, effect).await?;
        if !response.status.is_success()
            || response.status == StatusCode::NON_AUTHORITATIVE_INFORMATION
        {
            return Err(status_error(response.status, &response.body, effect));
        }
        Ok(response)
    }

    /// Send one request whose successful response must be JSON.
    pub(crate) async fn json(
        &self,
        request: AdoRequest<'_>,
        effect: bool,
    ) -> Result<Value, AdoClientError> {
        match self.send(request, effect).await?.into_body() {
            AdoBody::Json(value) => Ok(value),
            AdoBody::Empty | AdoBody::Text(_) => Err(response_shape_failure(effect)),
        }
    }

    /// A list endpoint's `value` array (`Client._unwrap_collection`).
    pub(crate) async fn collection(
        &self,
        request: AdoRequest<'_>,
    ) -> Result<Vec<Value>, AdoClientError> {
        match self.json(request, false).await? {
            Value::Object(mut object) => match object.remove("value") {
                Some(Value::Array(values)) => Ok(values),
                _ => Err(invalid_response()),
            },
            Value::Array(values) => Ok(values),
            _ => Err(invalid_response()),
        }
    }
}

fn status_error(status: StatusCode, body: &AdoBody, effect: bool) -> AdoClientError {
    let provider_message = match body {
        AdoBody::Json(value) => value
            .get("message")
            .and_then(Value::as_str)
            .map(|message| truncate(message, MAX_PROVIDER_MESSAGE_BYTES).into()),
        AdoBody::Empty | AdoBody::Text(_) => None,
    };
    if effect
        && (status.is_server_error()
            || matches!(
                status,
                StatusCode::REQUEST_TIMEOUT | StatusCode::TOO_MANY_REQUESTS
            ))
    {
        return AdoClientError {
            code: AdoClientErrorCode::UnknownOutcome,
            retryable: false,
            provider_message,
        };
    }
    let code = match status {
        StatusCode::BAD_REQUEST | StatusCode::UNPROCESSABLE_ENTITY => {
            AdoClientErrorCode::InvalidInput
        }
        // A PAT Azure DevOps does not accept answers 203 with the sign-in
        // page, or redirects to it; neither is a usable result.
        StatusCode::UNAUTHORIZED | StatusCode::NON_AUTHORITATIVE_INFORMATION => {
            AdoClientErrorCode::Authentication
        }
        status if status.is_redirection() => AdoClientErrorCode::Authentication,
        StatusCode::FORBIDDEN => AdoClientErrorCode::Authorization,
        StatusCode::NOT_FOUND => AdoClientErrorCode::NotFound,
        StatusCode::CONFLICT | StatusCode::PRECONDITION_FAILED => AdoClientErrorCode::Conflict,
        StatusCode::REQUEST_TIMEOUT => AdoClientErrorCode::Timeout,
        StatusCode::TOO_MANY_REQUESTS => AdoClientErrorCode::RateLimited,
        status if status.is_server_error() => AdoClientErrorCode::DependencyUnavailable,
        _ => AdoClientErrorCode::InvalidResponse,
    };
    AdoClientError {
        code,
        retryable: !effect
            && matches!(
                code,
                AdoClientErrorCode::Timeout
                    | AdoClientErrorCode::RateLimited
                    | AdoClientErrorCode::DependencyUnavailable
            ),
        provider_message,
    }
}

fn truncate(value: &str, limit: usize) -> &str {
    if value.len() <= limit {
        return value;
    }
    let mut end = limit;
    while !value.is_char_boundary(end) {
        end -= 1;
    }
    &value[..end]
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> AdoClientError {
    if effect {
        return unknown_outcome();
    }
    AdoClientError {
        code: if source.is_timeout() {
            AdoClientErrorCode::Timeout
        } else {
            AdoClientErrorCode::DependencyUnavailable
        },
        retryable: true,
        provider_message: None,
    }
}

pub(crate) const fn invalid_configuration() -> AdoClientError {
    AdoClientError {
        code: AdoClientErrorCode::InvalidConfiguration,
        retryable: false,
        provider_message: None,
    }
}

pub(crate) const fn invalid_input() -> AdoClientError {
    AdoClientError {
        code: AdoClientErrorCode::InvalidInput,
        retryable: false,
        provider_message: None,
    }
}

pub(crate) const fn invalid_response() -> AdoClientError {
    AdoClientError {
        code: AdoClientErrorCode::InvalidResponse,
        retryable: false,
        provider_message: None,
    }
}

pub(crate) const fn resource_exhausted() -> AdoClientError {
    AdoClientError {
        code: AdoClientErrorCode::ResourceExhausted,
        retryable: false,
        provider_message: None,
    }
}

pub(crate) const fn unknown_outcome() -> AdoClientError {
    AdoClientError {
        code: AdoClientErrorCode::UnknownOutcome,
        retryable: false,
        provider_message: None,
    }
}

pub(crate) const fn response_bound_failure(effect: bool) -> AdoClientError {
    if effect {
        unknown_outcome()
    } else {
        resource_exhausted()
    }
}

pub(crate) const fn response_shape_failure(effect: bool) -> AdoClientError {
    if effect {
        unknown_outcome()
    } else {
        invalid_response()
    }
}

/// Refuse a result larger than one tool result may be.
pub(crate) fn bounded_output(value: Value) -> Result<Value, AdoClientError> {
    let encoded = serde_json::to_vec(&value).map_err(|_| invalid_response())?;
    if encoded.len() > MAX_OUTPUT_BYTES {
        return Err(resource_exhausted());
    }
    Ok(value)
}
