use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_DISPOSITION, CONTENT_LENGTH, CONTENT_TYPE, HeaderValue,
};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Value, json};
use zeroize::Zeroizing;

use super::config::ReportPortalToolkitConfig;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 8;
const MAX_IDENTIFIER_BYTES: usize = 1_024;
pub(in crate::toolkits) const MAX_PAGE_NUMBER: u64 = 10_000;
const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_PDF_SOURCE_BYTES: usize = 383 * 1_024;
const MAX_CONTENT_DISPOSITION_BYTES: usize = 4 * 1_024;
const USER_AGENT: &str = "elitea-worker-rust/0.1";
const JSON_CONTENT_TYPE: &str = "application/json";
const HTML_CONTENT_TYPE: &str = "text/html";
const PDF_CONTENT_TYPE: &str = "application/pdf";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(in crate::toolkits) enum ReportFormat {
    Html,
    Pdf,
}

impl ReportFormat {
    pub(in crate::toolkits) const fn as_str(self) -> &'static str {
        match self {
            Self::Html => "html",
            Self::Pdf => "pdf",
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ReportPortalClientErrorCode {
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

/// Stable `ReportPortal` failure without endpoint, project, token, path, or body.
pub(crate) struct ReportPortalClientError {
    code: ReportPortalClientErrorCode,
}

impl ReportPortalClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> ReportPortalClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        matches!(
            self.code,
            ReportPortalClientErrorCode::RateLimited
                | ReportPortalClientErrorCode::Timeout
                | ReportPortalClientErrorCode::DependencyUnavailable
        )
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            ReportPortalClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "report_portal.configuration.invalid",
                "the ReportPortal toolkit configuration is invalid",
            ),
            ReportPortalClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "report_portal.request.invalid",
                "the ReportPortal request is invalid",
            ),
            ReportPortalClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "report_portal.authentication.failed",
                "ReportPortal authentication failed",
            ),
            ReportPortalClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "report_portal.authorization.failed",
                "ReportPortal did not authorize the request",
            ),
            ReportPortalClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "report_portal.resource.not_found",
                "the requested ReportPortal resource was not found",
            ),
            ReportPortalClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "report_portal.rate_limited",
                "ReportPortal rate limited the request",
            ),
            ReportPortalClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "report_portal.timeout",
                "the ReportPortal request timed out",
            ),
            ReportPortalClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "report_portal.unavailable",
                "ReportPortal is unavailable",
            ),
            ReportPortalClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "report_portal.response.invalid",
                "ReportPortal returned an invalid response",
            ),
            ReportPortalClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "report_portal.response.resource_exhausted",
                "the ReportPortal request or response exceeds the approved limit",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable(),
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(code: ReportPortalClientErrorCode) -> Self {
        Self { code }
    }
}

impl fmt::Debug for ReportPortalClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ReportPortalClientError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ReportPortalClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ReportPortalClientErrorCode::InvalidConfiguration => {
                "the ReportPortal client configuration is invalid"
            }
            ReportPortalClientErrorCode::InvalidInput => "the ReportPortal request is invalid",
            ReportPortalClientErrorCode::Authentication => "ReportPortal authentication failed",
            ReportPortalClientErrorCode::Authorization => "ReportPortal authorization failed",
            ReportPortalClientErrorCode::NotFound => "the ReportPortal resource was not found",
            ReportPortalClientErrorCode::RateLimited => "ReportPortal rate limited the request",
            ReportPortalClientErrorCode::Timeout => "the ReportPortal request timed out",
            ReportPortalClientErrorCode::DependencyUnavailable => "ReportPortal is unavailable",
            ReportPortalClientErrorCode::InvalidResponse => {
                "ReportPortal returned an invalid response"
            }
            ReportPortalClientErrorCode::ResourceExhausted => {
                "the ReportPortal request or response exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for ReportPortalClientError {}

#[async_trait]
pub(in crate::toolkits) trait ReportPortalApi: Send + Sync {
    async fn get_extended_launch_data_as_raw(
        &self,
        launch_id: &str,
        format: ReportFormat,
    ) -> Result<Value, ReportPortalClientError>;
    async fn get_extended_launch_data(
        &self,
        launch_id: &str,
    ) -> Result<Value, ReportPortalClientError>;
    async fn get_launch_details(&self, launch_id: &str) -> Result<Value, ReportPortalClientError>;
    async fn get_all_launches(&self, page_number: u64) -> Result<Value, ReportPortalClientError>;
    async fn find_test_item_by_id(&self, item_id: &str) -> Result<Value, ReportPortalClientError>;
    async fn get_test_items_for_launch(
        &self,
        launch_id: &str,
        page_number: u64,
    ) -> Result<Value, ReportPortalClientError>;
    async fn get_logs_for_test_items(
        &self,
        item_id: &str,
        page_number: u64,
    ) -> Result<Value, ReportPortalClientError>;
    async fn get_user_information(&self, username: &str) -> Result<Value, ReportPortalClientError>;
    async fn get_dashboard_data(
        &self,
        dashboard_id: &str,
    ) -> Result<Value, ReportPortalClientError>;
}

pub(in crate::toolkits) struct ReportPortalHttpResponse {
    status: StatusCode,
    content_type: Option<Box<str>>,
    attachment: bool,
    body: Vec<u8>,
}

impl ReportPortalHttpResponse {
    #[cfg(test)]
    pub(in crate::toolkits) fn fixture(
        status: StatusCode,
        content_type: Option<&str>,
        content_disposition: Option<&str>,
        body: impl Into<Vec<u8>>,
    ) -> Self {
        Self {
            status,
            content_type: content_type.map(Into::into),
            attachment: content_disposition.is_some_and(valid_attachment_disposition),
            body: body.into(),
        }
    }
}

#[async_trait]
pub(in crate::toolkits) trait ReportPortalTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
    ) -> Result<ReportPortalHttpResponse, ReportPortalClientError>;
}

struct ReqwestReportPortalTransport {
    http: reqwest::Client,
}

#[async_trait]
impl ReportPortalTransport for ReqwestReportPortalTransport {
    async fn execute(
        &self,
        request: Request,
    ) -> Result<ReportPortalHttpResponse, ReportPortalClientError> {
        let mut response = self
            .http
            .execute(request)
            .await
            .map_err(|source| map_reqwest_error(&source))?;
        if response
            .headers()
            .get(CONTENT_LENGTH)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.parse::<usize>().ok())
            .is_some_and(|length| length > MAX_RESPONSE_BYTES)
        {
            return Err(resource_exhausted());
        }
        let status = response.status();
        let content_type = response
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .map(Into::into);
        let attachment = response
            .headers()
            .get(CONTENT_DISPOSITION)
            .and_then(|value| value.to_str().ok())
            .is_some_and(valid_attachment_disposition);
        let mut body = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|source| map_reqwest_error(&source))?
        {
            if body.len().saturating_add(chunk.len()) > MAX_RESPONSE_BYTES {
                return Err(resource_exhausted());
            }
            body.extend_from_slice(&chunk);
        }
        Ok(ReportPortalHttpResponse {
            status,
            content_type,
            attachment,
            body,
        })
    }
}

#[derive(Clone, Copy)]
pub(in crate::toolkits) enum ReportPortalRequestKind<'a> {
    RawExport {
        launch_id: &'a str,
        format: ReportFormat,
    },
    ReadableExport {
        launch_id: &'a str,
    },
    LaunchDetails {
        launch_id: &'a str,
    },
    AllLaunches {
        page_number: u64,
    },
    TestItem {
        item_id: &'a str,
    },
    TestItemsForLaunch {
        launch_id: &'a str,
        page_number: u64,
    },
    LogsForTestItem {
        item_id: &'a str,
        page_number: u64,
    },
    UserInformation {
        username: &'a str,
    },
    Dashboard {
        dashboard_id: &'a str,
    },
}

/// One invocation-scoped `ReportPortal` client and connection pool.
pub(crate) struct ReportPortalClient {
    config: ReportPortalToolkitConfig,
    transport: Arc<dyn ReportPortalTransport>,
}

impl ReportPortalClient {
    pub(crate) fn new(config: ReportPortalToolkitConfig) -> Result<Self, ReportPortalClientError> {
        let http = reqwest::Client::builder()
            .https_only(true)
            .redirect(reqwest::redirect::Policy::none())
            .retry(reqwest::retry::never())
            .connect_timeout(CONNECT_TIMEOUT)
            .timeout(REQUEST_TIMEOUT)
            .pool_idle_timeout(POOL_IDLE_TIMEOUT)
            .pool_max_idle_per_host(MAX_IDLE_PER_HOST)
            .user_agent(USER_AGENT)
            .build()
            .map_err(|_| invalid_configuration())?;
        Ok(Self {
            config,
            transport: Arc::new(ReqwestReportPortalTransport { http }),
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn fixture(
        config: ReportPortalToolkitConfig,
        transport: Arc<dyn ReportPortalTransport>,
    ) -> Self {
        Self { config, transport }
    }

    fn build_request(
        &self,
        kind: ReportPortalRequestKind<'_>,
    ) -> Result<Request, ReportPortalClientError> {
        let mut endpoint = self.config.endpoint().clone();
        let mut query = Vec::new();
        let path: Vec<&str> = match kind {
            ReportPortalRequestKind::RawExport { launch_id, format } => {
                validate_identifier(launch_id)?;
                query.push(("view", format.as_str().to_owned()));
                vec![
                    "api",
                    "v1",
                    self.config.project(),
                    "launch",
                    launch_id,
                    "report",
                ]
            }
            ReportPortalRequestKind::ReadableExport { launch_id } => {
                validate_identifier(launch_id)?;
                query.push(("view", "html".to_owned()));
                vec![
                    "api",
                    "v1",
                    self.config.project(),
                    "launch",
                    launch_id,
                    "report",
                ]
            }
            ReportPortalRequestKind::LaunchDetails { launch_id } => {
                validate_identifier(launch_id)?;
                vec!["api", "v1", self.config.project(), "launch", launch_id]
            }
            ReportPortalRequestKind::AllLaunches { page_number } => {
                validate_page_number(page_number)?;
                query.push(("page.page", page_number.to_string()));
                vec!["api", "v1", self.config.project(), "launch"]
            }
            ReportPortalRequestKind::TestItem { item_id } => {
                validate_identifier(item_id)?;
                vec!["api", "v1", self.config.project(), "item", item_id]
            }
            ReportPortalRequestKind::TestItemsForLaunch {
                launch_id,
                page_number,
            } => {
                validate_identifier(launch_id)?;
                validate_page_number(page_number)?;
                query.push(("filter.eq.launchId", launch_id.to_owned()));
                query.push(("page.page", page_number.to_string()));
                vec!["api", "v1", self.config.project(), "item"]
            }
            ReportPortalRequestKind::LogsForTestItem {
                item_id,
                page_number,
            } => {
                validate_identifier(item_id)?;
                validate_page_number(page_number)?;
                query.push(("filter.eq.item", item_id.to_owned()));
                query.push(("page.page", page_number.to_string()));
                vec!["api", "v1", self.config.project(), "log"]
            }
            ReportPortalRequestKind::UserInformation { username } => {
                validate_identifier(username)?;
                vec!["api", "users", username]
            }
            ReportPortalRequestKind::Dashboard { dashboard_id } => {
                validate_identifier(dashboard_id)?;
                vec![
                    "api",
                    "v1",
                    self.config.project(),
                    "dashboard",
                    dashboard_id,
                ]
            }
        };
        {
            let mut segments = endpoint
                .path_segments_mut()
                .map_err(|()| invalid_configuration())?;
            segments.clear();
            segments.extend(path);
        }
        if !query.is_empty() {
            let mut pairs = endpoint.query_pairs_mut();
            for (key, value) in query {
                pairs.append_pair(key, &value);
            }
        }
        ensure_same_origin(self.config.endpoint(), &endpoint)?;

        self.authorized_request(endpoint)
    }

    fn authorized_request(&self, endpoint: Url) -> Result<Request, ReportPortalClientError> {
        let mut request = Request::new(Method::GET, endpoint);
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static(JSON_CONTENT_TYPE));
        let mut bearer = Zeroizing::new(String::with_capacity(self.config.api_key().len() + 7));
        bearer.push_str("Bearer ");
        bearer.push_str(self.config.api_key());
        let mut authorization =
            HeaderValue::from_str(&bearer).map_err(|_| invalid_configuration())?;
        authorization.set_sensitive(true);
        request.headers_mut().insert(AUTHORIZATION, authorization);
        *request.timeout_mut() = Some(REQUEST_TIMEOUT);
        Ok(request)
    }

    async fn request(
        &self,
        kind: ReportPortalRequestKind<'_>,
    ) -> Result<ReportPortalHttpResponse, ReportPortalClientError> {
        let request = self.build_request(kind)?;
        let response = self.transport.execute(request).await?;
        map_http_status(response.status)?;
        Ok(response)
    }

    async fn request_json(
        &self,
        kind: ReportPortalRequestKind<'_>,
    ) -> Result<Value, ReportPortalClientError> {
        let response = self.request(kind).await?;
        require_content_type(response.content_type.as_deref(), JSON_CONTENT_TYPE)?;
        let value: Value =
            serde_json::from_slice(&response.body).map_err(|_| invalid_response())?;
        if !value.is_object() {
            return Err(invalid_response());
        }
        bound_output(value)
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_request(
        &self,
        kind: ReportPortalRequestKind<'_>,
    ) -> Result<Request, ReportPortalClientError> {
        self.build_request(kind)
    }
}

#[async_trait]
impl ReportPortalApi for ReportPortalClient {
    async fn get_extended_launch_data_as_raw(
        &self,
        launch_id: &str,
        format: ReportFormat,
    ) -> Result<Value, ReportPortalClientError> {
        let response = self
            .request(ReportPortalRequestKind::RawExport { launch_id, format })
            .await?;
        project_raw_export(response, format)
    }

    async fn get_extended_launch_data(
        &self,
        launch_id: &str,
    ) -> Result<Value, ReportPortalClientError> {
        let response = self
            .request(ReportPortalRequestKind::ReadableExport { launch_id })
            .await?;
        require_html(&response)?;
        let html = String::from_utf8(response.body).map_err(|_| invalid_response())?;
        let text = html_to_text(&html)?;
        bound_output(Value::String(text))
    }

    async fn get_launch_details(&self, launch_id: &str) -> Result<Value, ReportPortalClientError> {
        self.request_json(ReportPortalRequestKind::LaunchDetails { launch_id })
            .await
    }

    async fn get_all_launches(&self, page_number: u64) -> Result<Value, ReportPortalClientError> {
        self.request_json(ReportPortalRequestKind::AllLaunches { page_number })
            .await
    }

    async fn find_test_item_by_id(&self, item_id: &str) -> Result<Value, ReportPortalClientError> {
        self.request_json(ReportPortalRequestKind::TestItem { item_id })
            .await
    }

    async fn get_test_items_for_launch(
        &self,
        launch_id: &str,
        page_number: u64,
    ) -> Result<Value, ReportPortalClientError> {
        self.request_json(ReportPortalRequestKind::TestItemsForLaunch {
            launch_id,
            page_number,
        })
        .await
    }

    async fn get_logs_for_test_items(
        &self,
        item_id: &str,
        page_number: u64,
    ) -> Result<Value, ReportPortalClientError> {
        self.request_json(ReportPortalRequestKind::LogsForTestItem {
            item_id,
            page_number,
        })
        .await
    }

    async fn get_user_information(&self, username: &str) -> Result<Value, ReportPortalClientError> {
        self.request_json(ReportPortalRequestKind::UserInformation { username })
            .await
    }

    async fn get_dashboard_data(
        &self,
        dashboard_id: &str,
    ) -> Result<Value, ReportPortalClientError> {
        self.request_json(ReportPortalRequestKind::Dashboard { dashboard_id })
            .await
    }
}

fn project_raw_export(
    response: ReportPortalHttpResponse,
    format: ReportFormat,
) -> Result<Value, ReportPortalClientError> {
    if !response.attachment {
        return Err(invalid_response());
    }
    let byte_length = response.body.len();
    let (content_type, encoding, content) = match format {
        ReportFormat::Html => {
            require_html(&response)?;
            let html = String::from_utf8(response.body).map_err(|_| invalid_response())?;
            (HTML_CONTENT_TYPE, "utf-8", html)
        }
        ReportFormat::Pdf => {
            require_content_type(response.content_type.as_deref(), PDF_CONTENT_TYPE)?;
            if response.body.len() > MAX_PDF_SOURCE_BYTES || !response.body.starts_with(b"%PDF-") {
                return Err(if response.body.len() > MAX_PDF_SOURCE_BYTES {
                    resource_exhausted()
                } else {
                    invalid_response()
                });
            }
            (PDF_CONTENT_TYPE, "base64", STANDARD.encode(response.body))
        }
    };
    bound_output(json!({
        "format": format.as_str(),
        "content_type": content_type,
        "encoding": encoding,
        "byte_length": byte_length,
        "content": content,
    }))
}

fn require_html(response: &ReportPortalHttpResponse) -> Result<(), ReportPortalClientError> {
    if !response.attachment {
        return Err(invalid_response());
    }
    require_content_type(response.content_type.as_deref(), HTML_CONTENT_TYPE)?;
    if response.body.starts_with(b"%PDF-") {
        return Err(invalid_response());
    }
    Ok(())
}

fn valid_attachment_disposition(value: &str) -> bool {
    if value.len() > MAX_CONTENT_DISPOSITION_BYTES || value.chars().any(char::is_control) {
        return false;
    }
    let mut parts = value.split(';');
    if !parts
        .next()
        .is_some_and(|kind| kind.trim().eq_ignore_ascii_case("attachment"))
    {
        return false;
    }
    parts.all(valid_disposition_parameter)
}

fn valid_disposition_parameter(parameter: &str) -> bool {
    let Some((name, value)) = parameter.trim().split_once('=') else {
        return false;
    };
    let name = name.trim();
    let value = value.trim();
    if name.is_empty()
        || !name
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
        || value.is_empty()
    {
        return false;
    }
    if value.starts_with('"') || value.ends_with('"') {
        value.len() >= 2
            && value.starts_with('"')
            && value.ends_with('"')
            && !value[1..value.len() - 1].contains('"')
    } else {
        !value.chars().any(char::is_whitespace)
    }
}

fn require_content_type(
    actual: Option<&str>,
    expected: &str,
) -> Result<(), ReportPortalClientError> {
    if actual
        .and_then(|value| value.split(';').next())
        .is_some_and(|value| value.trim().eq_ignore_ascii_case(expected))
    {
        Ok(())
    } else {
        Err(invalid_response())
    }
}

fn html_to_text(html: &str) -> Result<String, ReportPortalClientError> {
    let without_hidden = remove_hidden_html(html);
    let mut text = String::with_capacity(without_hidden.len().min(MAX_OUTPUT_BYTES));
    let bytes = without_hidden.as_bytes();
    let mut cursor = 0;
    while cursor < bytes.len() {
        if bytes[cursor] == b'<' {
            let Some(relative_end) = without_hidden[cursor..].find('>') else {
                break;
            };
            push_separator(&mut text);
            cursor += relative_end + 1;
            continue;
        }
        let next_tag = without_hidden[cursor..]
            .find('<')
            .map_or(bytes.len(), |offset| cursor + offset);
        decode_entities(&without_hidden[cursor..next_tag], &mut text)?;
        cursor = next_tag;
        if text.len() > MAX_OUTPUT_BYTES {
            return Err(resource_exhausted());
        }
    }
    let normalized = text.split_whitespace().collect::<Vec<_>>().join(" ");
    if normalized.len() > MAX_OUTPUT_BYTES {
        return Err(resource_exhausted());
    }
    Ok(normalized)
}

fn remove_hidden_html(html: &str) -> String {
    let mut visible = String::with_capacity(html.len());
    let bytes = html.as_bytes();
    let mut cursor = 0;
    while cursor < bytes.len() {
        let Some(relative_start) = html[cursor..].find('<') else {
            visible.push_str(&html[cursor..]);
            break;
        };
        let start = cursor + relative_start;
        visible.push_str(&html[cursor..start]);

        let (opening, closing): (&[u8], &[u8]) =
            if starts_with_ascii_case_insensitive(&bytes[start..], b"<script") {
                (b"<script", b"</script>")
            } else if starts_with_ascii_case_insensitive(&bytes[start..], b"<style") {
                (b"<style", b"</style>")
            } else if bytes[start..].starts_with(b"<!--") {
                (b"<!--", b"-->")
            } else {
                visible.push('<');
                cursor = start + 1;
                continue;
            };

        let search_from = start + opening.len();
        let Some(relative_end) = find_ascii_case_insensitive(&bytes[search_from..], closing) else {
            break;
        };
        cursor = search_from + relative_end + closing.len();
        visible.push(' ');
    }
    visible
}

fn starts_with_ascii_case_insensitive(haystack: &[u8], needle: &[u8]) -> bool {
    haystack
        .get(..needle.len())
        .is_some_and(|prefix| prefix.eq_ignore_ascii_case(needle))
}

fn find_ascii_case_insensitive(haystack: &[u8], needle: &[u8]) -> Option<usize> {
    haystack
        .windows(needle.len())
        .position(|window| window.eq_ignore_ascii_case(needle))
}

fn decode_entities(source: &str, output: &mut String) -> Result<(), ReportPortalClientError> {
    let mut cursor = 0;
    while cursor < source.len() {
        let Some(relative_amp) = source[cursor..].find('&') else {
            output.push_str(&source[cursor..]);
            break;
        };
        let amp = cursor + relative_amp;
        output.push_str(&source[cursor..amp]);
        let Some(relative_semicolon) = source[amp..].find(';') else {
            output.push_str(&source[amp..]);
            break;
        };
        let semicolon = amp + relative_semicolon;
        if semicolon.saturating_sub(amp) > 16 {
            output.push('&');
            cursor = amp + 1;
            continue;
        }
        let entity = &source[amp + 1..semicolon];
        if let Some(decoded) = decode_entity(entity) {
            output.push(decoded);
            cursor = semicolon + 1;
        } else if let Some(decoded) = decode_named_entity(entity) {
            output.push_str(decoded);
            cursor = semicolon + 1;
        } else {
            output.push('&');
            cursor = amp + 1;
        }
        if output.len() > MAX_OUTPUT_BYTES {
            return Err(resource_exhausted());
        }
    }
    Ok(())
}

fn decode_entity(entity: &str) -> Option<char> {
    let value = if let Some(hex) = entity
        .strip_prefix("#x")
        .or_else(|| entity.strip_prefix("#X"))
    {
        u32::from_str_radix(hex, 16).ok()?
    } else {
        entity.strip_prefix('#')?.parse().ok()?
    };
    char::from_u32(value).filter(|value| !value.is_control() || value.is_whitespace())
}

fn decode_named_entity(entity: &str) -> Option<&'static str> {
    match entity {
        "amp" => Some("&"),
        "lt" => Some("<"),
        "gt" => Some(">"),
        "quot" => Some("\""),
        "apos" => Some("'"),
        "nbsp" | "ensp" | "emsp" => Some(" "),
        "ndash" => Some("–"),
        "mdash" => Some("—"),
        "hellip" => Some("…"),
        "copy" => Some("©"),
        "reg" => Some("®"),
        _ => None,
    }
}

fn push_separator(text: &mut String) {
    if !text.chars().last().is_some_and(char::is_whitespace) {
        text.push(' ');
    }
}

fn validate_identifier(value: &str) -> Result<(), ReportPortalClientError> {
    if value.len() > MAX_IDENTIFIER_BYTES {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || value.chars().any(char::is_control) {
        return Err(invalid_input());
    }
    Ok(())
}

const fn validate_page_number(page_number: u64) -> Result<(), ReportPortalClientError> {
    if page_number <= MAX_PAGE_NUMBER {
        Ok(())
    } else {
        Err(resource_exhausted())
    }
}

fn ensure_same_origin(base: &Url, endpoint: &Url) -> Result<(), ReportPortalClientError> {
    if base.scheme() == endpoint.scheme()
        && base.host_str() == endpoint.host_str()
        && base.port_or_known_default() == endpoint.port_or_known_default()
        && endpoint.username().is_empty()
        && endpoint.password().is_none()
    {
        Ok(())
    } else {
        Err(invalid_configuration())
    }
}

fn bound_output(value: Value) -> Result<Value, ReportPortalClientError> {
    let size = serde_json::to_vec(&value)
        .map_err(|_| invalid_response())?
        .len();
    if size > MAX_OUTPUT_BYTES {
        Err(resource_exhausted())
    } else {
        Ok(value)
    }
}

fn map_http_status(status: StatusCode) -> Result<(), ReportPortalClientError> {
    match status.as_u16() {
        200..=299 => Ok(()),
        400 | 405 | 406 | 409 | 422 => Err(invalid_input()),
        401 => Err(authentication()),
        403 => Err(authorization()),
        404 => Err(not_found()),
        408 | 504 => Err(timeout()),
        429 => Err(rate_limited()),
        500..=599 => Err(dependency_unavailable()),
        _ => Err(invalid_response()),
    }
}

fn map_reqwest_error(source: &reqwest::Error) -> ReportPortalClientError {
    if source.is_timeout() {
        timeout()
    } else if source.is_connect() || source.is_request() || source.is_body() {
        dependency_unavailable()
    } else {
        invalid_response()
    }
}

const fn invalid_configuration() -> ReportPortalClientError {
    ReportPortalClientError {
        code: ReportPortalClientErrorCode::InvalidConfiguration,
    }
}

const fn invalid_input() -> ReportPortalClientError {
    ReportPortalClientError {
        code: ReportPortalClientErrorCode::InvalidInput,
    }
}

const fn authentication() -> ReportPortalClientError {
    ReportPortalClientError {
        code: ReportPortalClientErrorCode::Authentication,
    }
}

const fn authorization() -> ReportPortalClientError {
    ReportPortalClientError {
        code: ReportPortalClientErrorCode::Authorization,
    }
}

const fn not_found() -> ReportPortalClientError {
    ReportPortalClientError {
        code: ReportPortalClientErrorCode::NotFound,
    }
}

const fn rate_limited() -> ReportPortalClientError {
    ReportPortalClientError {
        code: ReportPortalClientErrorCode::RateLimited,
    }
}

const fn timeout() -> ReportPortalClientError {
    ReportPortalClientError {
        code: ReportPortalClientErrorCode::Timeout,
    }
}

const fn dependency_unavailable() -> ReportPortalClientError {
    ReportPortalClientError {
        code: ReportPortalClientErrorCode::DependencyUnavailable,
    }
}

const fn invalid_response() -> ReportPortalClientError {
    ReportPortalClientError {
        code: ReportPortalClientErrorCode::InvalidResponse,
    }
}

const fn resource_exhausted() -> ReportPortalClientError {
    ReportPortalClientError {
        code: ReportPortalClientErrorCode::ResourceExhausted,
    }
}

#[cfg(test)]
pub(in crate::toolkits) fn test_http_status(
    status: StatusCode,
) -> Result<(), ReportPortalClientError> {
    map_http_status(status)
}
