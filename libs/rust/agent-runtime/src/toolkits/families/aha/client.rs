use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use chrono::DateTime;
use reqwest::header::{ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderValue};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value, json};
use zeroize::Zeroizing;

use super::artifact::{AhaArtifactError, AhaArtifactErrorCode, AhaArtifactResolver};
use super::config::AhaToolkitConfig;
use super::format::{
    FormatError, FormatErrorCode, OutputFormat, bounded, project_record, project_records, render,
};

const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const ATTACHMENT_TIMEOUT: Duration = Duration::from_secs(40);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 8;
const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_REQUEST_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_IDENTIFIER_BYTES: usize = 1_024;
const MAX_QUERY_BYTES: usize = 16 * 1_024;
const MAX_COMMENT_BYTES: usize = 64 * 1_024;
const MAX_PROPERTIES_BYTES: usize = 240 * 1_024;
const MAX_PROPERTY_NODES: usize = 16_384;
const MAX_PROPERTY_DEPTH: usize = 32;
const MAX_PROPERTY_STRING_BYTES: usize = 64 * 1_024;
const MAX_PAGES: usize = 10;
const MAX_RECORDS: usize = 2_000;
const MAX_PER_PAGE: usize = 200;
const USER_AGENT: &str = "elitea-worker-rust/0.1";

const QUERY_GET_PAGE: &str = r"query GetPage($id: ID!, $includeParent: Boolean!) {
  page(id: $id) {
    id
    referenceNum
    name
    description { markdownBody }
    children { id referenceNum name }
    parent @include(if: $includeParent) { id referenceNum name }
  }
}";
const QUERY_GET_FEATURE: &str = r"query GetFeature($id: ID!) {
  feature(id: $id) {
    id
    referenceNum
    name
    description { markdownBody }
    workflowStatus { name }
  }
}";
const QUERY_GET_REQUIREMENT: &str = r"query GetRequirement($id: ID!) {
  requirement(id: $id) {
    id
    referenceNum
    name
    description { markdownBody }
    workflowStatus { name }
  }
}";
const QUERY_SEARCH_DOCUMENTS: &str = r"query SearchDocuments($query: String!, $searchableType: [String!]) {
  searchDocuments(filters: { query: $query, searchableType: $searchableType }) {
    nodes { name url searchableId searchableType }
  }
}";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AhaClientErrorCode {
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

/// Stable provider failure without origin, path, payload, body, or credential.
pub struct AhaClientError {
    code: AhaClientErrorCode,
    retryable: bool,
}

impl AhaClientError {
    #[must_use]
    pub const fn code(&self) -> AhaClientErrorCode {
        self.code
    }

    #[must_use]
    pub const fn retryable(&self) -> bool {
        self.retryable
    }

    pub fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            AhaClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "aha.configuration.invalid",
                "the Aha toolkit configuration is invalid",
            ),
            AhaClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "aha.request.invalid",
                "the Aha request is invalid",
            ),
            AhaClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "aha.authentication.failed",
                "Aha authentication failed",
            ),
            AhaClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "aha.authorization.failed",
                "Aha did not authorize the request",
            ),
            AhaClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "aha.resource.not_found",
                "the requested Aha resource was not found",
            ),
            AhaClientErrorCode::Conflict => (
                ErrorCategory::InvalidInput,
                "aha.resource.conflict",
                "the Aha resource is in conflict",
            ),
            AhaClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "aha.rate_limited",
                "Aha rate limited the request",
            ),
            AhaClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "aha.timeout",
                "the Aha request timed out",
            ),
            AhaClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "aha.unavailable",
                "Aha is unavailable",
            ),
            AhaClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "aha.response.invalid",
                "Aha returned an invalid response",
            ),
            AhaClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "aha.resource_exhausted",
                "the Aha request or response exceeds the approved limit",
            ),
            AhaClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "aha.effect.unknown_outcome",
                "Aha may have applied the requested effect; reconcile it before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(any(test, feature = "test-support"))]
    pub const fn fixture(code: AhaClientErrorCode, retryable: bool) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for AhaClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AhaClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for AhaClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            AhaClientErrorCode::InvalidConfiguration => "the Aha client configuration is invalid",
            AhaClientErrorCode::InvalidInput => "the Aha request is invalid",
            AhaClientErrorCode::Authentication => "Aha authentication failed",
            AhaClientErrorCode::Authorization => "Aha authorization failed",
            AhaClientErrorCode::NotFound => "the Aha resource was not found",
            AhaClientErrorCode::Conflict => "the Aha resource is in conflict",
            AhaClientErrorCode::RateLimited => "Aha rate limited the request",
            AhaClientErrorCode::Timeout => "the Aha request timed out",
            AhaClientErrorCode::DependencyUnavailable => "Aha is unavailable",
            AhaClientErrorCode::InvalidResponse => "Aha returned an invalid response",
            AhaClientErrorCode::ResourceExhausted => {
                "the Aha request or response exceeds its approved limit"
            }
            AhaClientErrorCode::UnknownOutcome => {
                "the Aha effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for AhaClientError {}

#[derive(Clone, Copy)]
pub struct ReadOptions<'a> {
    pub output_format: OutputFormat,
    pub fields: &'a [Box<str>],
}

#[derive(Clone, Copy)]
pub struct PageOptions {
    pub per_page: usize,
    pub max_records: usize,
}

#[allow(clippy::large_enum_variant)]
pub enum AhaOperation<'a> {
    GetFeature {
        reference: &'a str,
        read: ReadOptions<'a>,
    },
    GetRequirement {
        reference: &'a str,
        read: ReadOptions<'a>,
    },
    GetRelease {
        reference: &'a str,
        read: ReadOptions<'a>,
    },
    GetInitiative {
        reference: &'a str,
        read: ReadOptions<'a>,
    },
    GetEpic {
        reference: &'a str,
        read: ReadOptions<'a>,
    },
    GetIdea {
        reference: &'a str,
        read: ReadOptions<'a>,
    },
    GetProduct {
        reference: &'a str,
        read: ReadOptions<'a>,
    },
    ListProducts {
        updated_since: Option<&'a str>,
        page: PageOptions,
        read: ReadOptions<'a>,
    },
    ListFeatures {
        product_id: Option<&'a str>,
        release_id: Option<&'a str>,
        query: Option<&'a str>,
        updated_since: Option<&'a str>,
        page: PageOptions,
        read: ReadOptions<'a>,
    },
    ListRequirements {
        feature_id: &'a str,
        query: Option<&'a str>,
        page: PageOptions,
        read: ReadOptions<'a>,
    },
    ListReleases {
        product_id: Option<&'a str>,
        parking_lot: Option<bool>,
        page: PageOptions,
        read: ReadOptions<'a>,
    },
    ListInitiatives {
        product_id: Option<&'a str>,
        page: PageOptions,
        read: ReadOptions<'a>,
    },
    ListEpics {
        product_id: Option<&'a str>,
        release_id: Option<&'a str>,
        page: PageOptions,
        read: ReadOptions<'a>,
    },
    ListIdeas {
        product_id: Option<&'a str>,
        query: Option<&'a str>,
        page: PageOptions,
        read: ReadOptions<'a>,
    },
    Search {
        query: &'a str,
        record_type: Option<&'a str>,
        page: PageOptions,
        read: ReadOptions<'a>,
    },
    GetPage {
        reference: &'a str,
        include_parent: bool,
    },
    SearchDocuments {
        query: &'a str,
        searchable_type: Option<&'a str>,
    },
    GetFeatureGql {
        reference: &'a str,
    },
    GetRequirementGql {
        reference: &'a str,
    },
    FindProject {
        query: Option<&'a str>,
        page: PageOptions,
        read: ReadOptions<'a>,
    },
    SearchRecords {
        record_type: &'a str,
        query: Option<&'a str>,
        feature_id: Option<&'a str>,
        product_id: Option<&'a str>,
        release_id: Option<&'a str>,
        updated_since: Option<&'a str>,
        page: PageOptions,
        read: ReadOptions<'a>,
    },
    ReadRecords {
        record_type: &'a str,
        reference: &'a str,
        read: ReadOptions<'a>,
    },
    AddComment {
        resource_type: &'a str,
        resource_id: &'a str,
        body: &'a str,
    },
    ListComments {
        resource_type: &'a str,
        resource_id: &'a str,
        page: PageOptions,
        read: ReadOptions<'a>,
    },
    ManageRecord {
        action: &'a str,
        record_type: &'a str,
        record_id: Option<&'a str>,
        parent_id: Option<&'a str>,
        properties: &'a Map<String, Value>,
    },
    CreateRecord {
        record_type: &'a str,
        parent_id: &'a str,
        properties: &'a Map<String, Value>,
    },
    UpdateRecord {
        record_type: &'a str,
        record_id: &'a str,
        parent_id: Option<&'a str>,
        properties: &'a Map<String, Value>,
    },
    DeleteRecord {
        record_type: &'a str,
        record_id: &'a str,
        parent_id: Option<&'a str>,
    },
    CreateRecordLink {
        from_record_type: &'a str,
        from_id: &'a str,
        to_record_type: &'a str,
        to_id: &'a str,
        link_type: u16,
    },
    CopyRecord {
        record_type: &'a str,
        record_id: &'a str,
    },
    FieldsMetadata {
        read: ReadOptions<'a>,
    },
    FieldOptionsMetadata {
        field_id: &'a str,
        read: ReadOptions<'a>,
    },
    AttachFile {
        resource_type: &'a str,
        resource_id: &'a str,
        filepath: &'a str,
        filename: Option<&'a str>,
    },
}

#[async_trait]
pub trait AhaApi: Send + Sync {
    async fn execute(&self, operation: AhaOperation<'_>) -> Result<Value, AhaClientError>;
}

pub struct AhaHttpResponse {
    status: StatusCode,
    body: Option<Value>,
    json_content_type: bool,
}

impl AhaHttpResponse {
    #[cfg(any(test, feature = "test-support"))]
    pub fn fixture(status: StatusCode, body: Option<Value>) -> Self {
        Self {
            status,
            body,
            json_content_type: true,
        }
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn non_json_fixture(status: StatusCode) -> Self {
        Self {
            status,
            body: None,
            json_content_type: false,
        }
    }
}

#[async_trait]
pub trait AhaTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<AhaHttpResponse, AhaClientError>;
}

struct ReqwestAhaTransport {
    http: reqwest::Client,
}

#[async_trait]
impl AhaTransport for ReqwestAhaTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<AhaHttpResponse, AhaClientError> {
        let mut response = self
            .http
            .execute(request)
            .await
            .map_err(|source| map_reqwest_error(&source, effect))?;
        if let Some(value) = response.headers().get(CONTENT_LENGTH) {
            let length = value
                .to_str()
                .ok()
                .and_then(|value| value.parse::<usize>().ok())
                .ok_or_else(|| response_shape_failure(effect))?;
            if length > MAX_RESPONSE_BYTES {
                return Err(response_bound_failure(effect));
            }
        }
        let json_content_type = response
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.split(';').next())
            .is_some_and(|value| {
                let value = value.trim();
                value.eq_ignore_ascii_case("application/json") || value.ends_with("+json")
            });
        let mut bytes = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|source| map_reqwest_error(&source, effect))?
        {
            let next = bytes
                .len()
                .checked_add(chunk.len())
                .ok_or_else(|| response_bound_failure(effect))?;
            if next > MAX_RESPONSE_BYTES {
                return Err(response_bound_failure(effect));
            }
            bytes.extend_from_slice(&chunk);
        }
        let body = if bytes.is_empty() {
            None
        } else if json_content_type {
            Some(serde_json::from_slice(&bytes).map_err(|_| response_shape_failure(effect))?)
        } else {
            None
        };
        Ok(AhaHttpResponse {
            status: response.status(),
            body,
            json_content_type,
        })
    }
}

pub struct AhaClient {
    base_url: Url,
    api_key: Zeroizing<String>,
    request_client: reqwest::Client,
    transport: Arc<dyn AhaTransport>,
    artifacts: Arc<AhaArtifactResolver>,
}

impl AhaClient {
    pub fn new(
        config: &AhaToolkitConfig,
        artifacts: Arc<AhaArtifactResolver>,
    ) -> Result<Self, AhaClientError> {
        let http = build_http_client()?;
        Ok(Self {
            base_url: config.base_url().clone(),
            api_key: Zeroizing::new(config.api_key().to_owned()),
            request_client: http.clone(),
            transport: Arc::new(ReqwestAhaTransport { http }),
            artifacts,
        })
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn with_transport(
        config: &AhaToolkitConfig,
        transport: Arc<dyn AhaTransport>,
        artifacts: Arc<AhaArtifactResolver>,
    ) -> Self {
        Self {
            base_url: config.base_url().clone(),
            api_key: Zeroizing::new(config.api_key().to_owned()),
            request_client: reqwest::Client::new(),
            transport,
            artifacts,
        }
    }
}

fn build_http_client() -> Result<reqwest::Client, AhaClientError> {
    reqwest::Client::builder()
        .connect_timeout(CONNECT_TIMEOUT)
        .timeout(REQUEST_TIMEOUT)
        .https_only(true)
        .retry(reqwest::retry::never())
        .pool_idle_timeout(POOL_IDLE_TIMEOUT)
        .pool_max_idle_per_host(MAX_IDLE_PER_HOST)
        .redirect(reqwest::redirect::Policy::none())
        .referer(false)
        .user_agent(USER_AGENT)
        .build()
        .map_err(|_| invalid_configuration())
}

fn response_object(
    response: AhaHttpResponse,
    effect: bool,
    expected: &[StatusCode],
    allow_empty: bool,
) -> Result<Map<String, Value>, AhaClientError> {
    if !expected.contains(&response.status) {
        return Err(map_http_status(response.status, effect));
    }
    if response.body.is_some() && !response.json_content_type {
        return Err(response_shape_failure(effect));
    }
    match response.body {
        Some(Value::Object(body)) => Ok(body),
        None if allow_empty => Ok(Map::new()),
        _ => Err(response_shape_failure(effect)),
    }
}

fn validate_request_bound(request: &Request) -> Result<(), AhaClientError> {
    if let Some(value) = request.headers().get(CONTENT_LENGTH) {
        let length = value
            .to_str()
            .ok()
            .and_then(|value| value.parse::<u64>().ok())
            .ok_or_else(invalid_input)?;
        // Multipart attachments have their own stricter file bound and may be
        // larger than the ordinary JSON envelope.
        let is_multipart = request
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .is_some_and(|value| value.starts_with("multipart/form-data"));
        if !is_multipart
            && length > u64::try_from(MAX_REQUEST_BYTES).map_err(|_| resource_exhausted())?
        {
            return Err(resource_exhausted());
        }
    }
    Ok(())
}

fn render_records(
    records: &[Value],
    read: ReadOptions<'_>,
    empty_message: Option<String>,
) -> Result<Value, AhaClientError> {
    render(
        Value::Array(project_records(records, read.fields)),
        read.output_format,
        empty_message,
    )
    .map_err(Into::into)
}

fn effect_bounded(value: Value) -> Result<Value, AhaClientError> {
    bounded(value).map_err(|_| unknown_outcome())
}

fn validate_page_options(options: PageOptions) -> Result<(), AhaClientError> {
    if !(1..=MAX_PER_PAGE).contains(&options.per_page)
        || !(1..=MAX_RECORDS).contains(&options.max_records)
    {
        return Err(invalid_input());
    }
    Ok(())
}

fn validate_identifier(value: &str) -> Result<(), AhaClientError> {
    if value.is_empty() || value.len() > MAX_IDENTIFIER_BYTES || value.chars().any(char::is_control)
    {
        return Err(if value.len() > MAX_IDENTIFIER_BYTES {
            resource_exhausted()
        } else {
            invalid_input()
        });
    }
    Ok(())
}

fn validate_query(value: &str) -> Result<(), AhaClientError> {
    if value.trim().is_empty()
        || value.len() > MAX_QUERY_BYTES
        || contains_bad_multiline_control(value)
    {
        return Err(if value.len() > MAX_QUERY_BYTES {
            resource_exhausted()
        } else {
            invalid_input()
        });
    }
    Ok(())
}

fn validate_optional_query(value: &str) -> Result<(), AhaClientError> {
    if value.len() > MAX_QUERY_BYTES || contains_bad_multiline_control(value) {
        return Err(if value.len() > MAX_QUERY_BYTES {
            resource_exhausted()
        } else {
            invalid_input()
        });
    }
    Ok(())
}

fn validate_multiline(value: &str, limit: usize, allow_empty: bool) -> Result<(), AhaClientError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if (!allow_empty && value.trim().is_empty()) || contains_bad_multiline_control(value) {
        return Err(invalid_input());
    }
    Ok(())
}

fn validate_properties(properties: &Map<String, Value>) -> Result<(), AhaClientError> {
    if serde_json::to_vec(properties)
        .map_err(|_| invalid_input())?
        .len()
        > MAX_PROPERTIES_BYTES
    {
        return Err(resource_exhausted());
    }
    let mut nodes = 0_usize;
    let mut pending = properties
        .iter()
        .map(|(key, value)| (1_usize, Some(key.as_str()), value))
        .collect::<Vec<_>>();
    while let Some((depth, key, value)) = pending.pop() {
        nodes = nodes
            .checked_add(1 + usize::from(key.is_some()))
            .ok_or_else(resource_exhausted)?;
        if nodes > MAX_PROPERTY_NODES || depth > MAX_PROPERTY_DEPTH {
            return Err(resource_exhausted());
        }
        if key.is_some_and(|key| key.len() > MAX_PROPERTY_STRING_BYTES) {
            return Err(resource_exhausted());
        }
        match value {
            Value::String(value) if value.len() > MAX_PROPERTY_STRING_BYTES => {
                return Err(resource_exhausted());
            }
            Value::Array(values) => {
                pending.extend(values.iter().map(|value| (depth + 1, None, value)));
            }
            Value::Object(values) => pending.extend(
                values
                    .iter()
                    .map(|(key, value)| (depth + 1, Some(key.as_str()), value)),
            ),
            _ => {}
        }
    }
    Ok(())
}

fn contains_bad_multiline_control(value: &str) -> bool {
    value
        .chars()
        .any(|character| character.is_control() && !matches!(character, '\n' | '\t'))
}

fn validate_optional_rfc3339(value: Option<&str>) -> Result<(), AhaClientError> {
    if let Some(value) = nonempty(value)
        && (value.len() > MAX_IDENTIFIER_BYTES || DateTime::parse_from_rfc3339(value).is_err())
    {
        return Err(invalid_input());
    }
    Ok(())
}

#[derive(Clone, Copy)]
enum ReferenceKind {
    Feature,
    Requirement,
    Page,
}

fn validate_reference(value: &str, kind: ReferenceKind) -> Result<(), AhaClientError> {
    validate_identifier(value)?;
    let mut pieces = value.split('-');
    let prefix = pieces.next().unwrap_or_default();
    if prefix.is_empty()
        || !prefix.as_bytes()[0].is_ascii_uppercase()
        || !prefix
            .bytes()
            .all(|byte| byte.is_ascii_uppercase() || byte.is_ascii_digit())
    {
        return Err(invalid_input());
    }
    let valid = match kind {
        ReferenceKind::Feature => {
            pieces.next().is_some_and(ascii_digits) && pieces.next().is_none()
        }
        ReferenceKind::Requirement => {
            pieces.next().is_some_and(ascii_digits)
                && pieces.next().is_some_and(ascii_digits)
                && pieces.next().is_none()
        }
        ReferenceKind::Page => {
            pieces.next() == Some("N")
                && pieces.next().is_some_and(ascii_digits)
                && pieces.next().is_none()
        }
    };
    if !valid {
        return Err(invalid_input());
    }
    Ok(())
}

fn ascii_digits(value: &str) -> bool {
    !value.is_empty() && value.bytes().all(|byte| byte.is_ascii_digit())
}

fn nonempty(value: Option<&str>) -> Option<&str> {
    value.map(str::trim).filter(|value| !value.is_empty())
}

fn normalize_resource(value: &str) -> String {
    match value.trim().to_ascii_lowercase().as_str() {
        "todo" | "to-do" | "to-dos" | "to_dos" | "task" | "tasks" => "to_do".to_owned(),
        normalized => normalized.to_owned(),
    }
}

fn resource_plural(value: &str) -> Option<&'static str> {
    Some(match value {
        "feature" => "features",
        "requirement" => "requirements",
        "idea" => "ideas",
        "release" => "releases",
        "release_phase" => "release_phases",
        "epic" => "epics",
        "initiative" => "initiatives",
        "product" => "products",
        "goal" => "goals",
        "page" => "pages",
        "to_do" => "tasks",
        _ => return None,
    })
}

fn comment_resource(value: &str) -> Result<(String, &'static str), AhaClientError> {
    let canonical = normalize_resource(value);
    if !matches!(
        canonical.as_str(),
        "feature"
            | "requirement"
            | "idea"
            | "release"
            | "release_phase"
            | "epic"
            | "initiative"
            | "goal"
            | "page"
            | "to_do"
    ) {
        return Err(invalid_input());
    }
    let plural = resource_plural(&canonical).ok_or_else(invalid_input)?;
    Ok((canonical, plural))
}

fn manageable_record(value: &str) -> Result<String, AhaClientError> {
    let value = value.trim().to_ascii_lowercase();
    if matches!(
        value.as_str(),
        "feature" | "requirement" | "idea" | "release" | "initiative" | "epic" | "page"
    ) {
        Ok(value)
    } else {
        Err(invalid_input())
    }
}

fn create_parent(record_type: &str) -> Option<&'static str> {
    Some(match record_type {
        "feature" | "epic" => "releases",
        "requirement" => "features",
        "idea" | "release" | "initiative" | "page" => "products",
        _ => return None,
    })
}

fn mutation_path<'a>(
    record_type: &'a str,
    record_id: &'a str,
    parent_id: Option<&'a str>,
) -> Result<Vec<&'a str>, AhaClientError> {
    let plural = resource_plural(record_type).ok_or_else(invalid_input)?;
    if matches!(record_type, "release" | "initiative") {
        let parent = nonempty(parent_id).ok_or_else(invalid_input)?;
        validate_identifier(parent)?;
        Ok(vec!["products", parent, plural, record_id])
    } else {
        Ok(vec![plural, record_id])
    }
}

fn link_source(value: &str) -> Result<String, AhaClientError> {
    let value = value.trim().to_ascii_lowercase();
    if matches!(
        value.as_str(),
        "feature"
            | "release"
            | "idea"
            | "epic"
            | "release_phase"
            | "initiative"
            | "page"
            | "goal"
            | "requirement"
    ) {
        Ok(value)
    } else {
        Err(invalid_input())
    }
}

fn link_target(value: &str) -> Result<String, AhaClientError> {
    let value = value.trim().to_ascii_lowercase();
    if matches!(
        value.as_str(),
        "feature" | "release" | "idea" | "epic" | "release_phase" | "initiative" | "page" | "goal"
    ) {
        Ok(value)
    } else {
        Err(invalid_input())
    }
}

const fn link_type_name(value: u16) -> &'static str {
    match value {
        10 => "relates to",
        20 => "depends on",
        30 => "duplicated by",
        40 => "contained by",
        50 => "impacted by",
        60 => "blocked by",
        80 => "research for",
        _ => "unknown",
    }
}

fn value_identifier(value: &Value) -> Option<&str> {
    match value {
        Value::String(value) => Some(value),
        _ => None,
    }
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> AhaClientError {
    if effect {
        return unknown_outcome();
    }
    if source.is_timeout() {
        timeout(true)
    } else if source.is_connect() || source.is_request() || source.is_body() {
        dependency_unavailable(true)
    } else {
        invalid_response()
    }
}

fn map_http_status(status: StatusCode, effect: bool) -> AhaClientError {
    if effect
        && (status.is_success()
            || status == StatusCode::REQUEST_TIMEOUT
            || status == StatusCode::TOO_MANY_REQUESTS
            || status.is_server_error())
    {
        return unknown_outcome();
    }
    match status {
        StatusCode::UNAUTHORIZED => authentication(),
        StatusCode::FORBIDDEN => authorization(),
        StatusCode::NOT_FOUND => not_found(),
        StatusCode::CONFLICT | StatusCode::UNPROCESSABLE_ENTITY => conflict(),
        StatusCode::REQUEST_TIMEOUT => timeout(true),
        StatusCode::TOO_MANY_REQUESTS => rate_limited(),
        status if status.is_server_error() => dependency_unavailable(true),
        _ => invalid_response(),
    }
}

fn response_shape_failure(effect: bool) -> AhaClientError {
    if effect {
        unknown_outcome()
    } else {
        invalid_response()
    }
}

fn response_bound_failure(effect: bool) -> AhaClientError {
    if effect {
        unknown_outcome()
    } else {
        resource_exhausted()
    }
}

impl From<FormatError> for AhaClientError {
    fn from(source: FormatError) -> Self {
        match source.code() {
            FormatErrorCode::InvalidInput => invalid_input(),
            FormatErrorCode::ResourceExhausted => resource_exhausted(),
        }
    }
}

impl From<AhaArtifactError> for AhaClientError {
    fn from(source: AhaArtifactError) -> Self {
        match source.code() {
            AhaArtifactErrorCode::InvalidInput => invalid_input(),
            AhaArtifactErrorCode::Authorization => authorization(),
            AhaArtifactErrorCode::NotFound => not_found(),
            AhaArtifactErrorCode::Timeout => timeout(true),
            AhaArtifactErrorCode::DependencyUnavailable => dependency_unavailable(true),
            AhaArtifactErrorCode::ResourceExhausted => resource_exhausted(),
            AhaArtifactErrorCode::InvalidResponse => invalid_response(),
        }
    }
}

const fn invalid_configuration() -> AhaClientError {
    AhaClientError {
        code: AhaClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}
const fn invalid_input() -> AhaClientError {
    AhaClientError {
        code: AhaClientErrorCode::InvalidInput,
        retryable: false,
    }
}
const fn authentication() -> AhaClientError {
    AhaClientError {
        code: AhaClientErrorCode::Authentication,
        retryable: false,
    }
}
const fn authorization() -> AhaClientError {
    AhaClientError {
        code: AhaClientErrorCode::Authorization,
        retryable: false,
    }
}
const fn not_found() -> AhaClientError {
    AhaClientError {
        code: AhaClientErrorCode::NotFound,
        retryable: false,
    }
}
const fn conflict() -> AhaClientError {
    AhaClientError {
        code: AhaClientErrorCode::Conflict,
        retryable: false,
    }
}
const fn rate_limited() -> AhaClientError {
    AhaClientError {
        code: AhaClientErrorCode::RateLimited,
        retryable: true,
    }
}
const fn timeout(retryable: bool) -> AhaClientError {
    AhaClientError {
        code: AhaClientErrorCode::Timeout,
        retryable,
    }
}
const fn dependency_unavailable(retryable: bool) -> AhaClientError {
    AhaClientError {
        code: AhaClientErrorCode::DependencyUnavailable,
        retryable,
    }
}
const fn invalid_response() -> AhaClientError {
    AhaClientError {
        code: AhaClientErrorCode::InvalidResponse,
        retryable: false,
    }
}
const fn resource_exhausted() -> AhaClientError {
    AhaClientError {
        code: AhaClientErrorCode::ResourceExhausted,
        retryable: false,
    }
}
const fn unknown_outcome() -> AhaClientError {
    AhaClientError {
        code: AhaClientErrorCode::UnknownOutcome,
        retryable: false,
    }
}

#[async_trait]
impl AhaApi for AhaClient {
    #[allow(clippy::too_many_lines)]
    async fn execute(&self, operation: AhaOperation<'_>) -> Result<Value, AhaClientError> {
        match operation {
            AhaOperation::GetFeature { reference, read } => {
                self.get_record("features", "feature", reference, read)
                    .await
            }
            AhaOperation::GetRequirement { reference, read } => {
                self.get_record("requirements", "requirement", reference, read)
                    .await
            }
            AhaOperation::GetRelease { reference, read } => {
                self.get_record("releases", "release", reference, read)
                    .await
            }
            AhaOperation::GetInitiative { reference, read } => {
                self.get_record("initiatives", "initiative", reference, read)
                    .await
            }
            AhaOperation::GetEpic { reference, read } => {
                self.get_record("epics", "epic", reference, read).await
            }
            AhaOperation::GetIdea { reference, read } => {
                self.get_record("ideas", "idea", reference, read).await
            }
            AhaOperation::GetProduct { reference, read } => {
                self.get_record("products", "product", reference, read)
                    .await
            }
            AhaOperation::ListProducts {
                updated_since,
                page,
                read,
            } => {
                validate_optional_rfc3339(updated_since)?;
                let records = self
                    .collect(
                        &["products"],
                        "products",
                        page,
                        &[("updated_since", updated_since)],
                    )
                    .await?;
                let detail = updated_since
                    .filter(|value| !value.trim().is_empty())
                    .map_or_else(String::new, |value| format!(" updated since '{value}'"));
                render_records(
                    &records,
                    read,
                    Some(format!("Aha! API returned no products{detail}.")),
                )
            }
            AhaOperation::ListFeatures {
                product_id,
                release_id,
                query,
                updated_since,
                page,
                read,
            } => {
                validate_optional_rfc3339(updated_since)?;
                let path = if let Some(release) = nonempty(release_id) {
                    vec!["releases", release, "features"]
                } else if let Some(product) = nonempty(product_id) {
                    vec!["products", product, "features"]
                } else {
                    vec!["features"]
                };
                let records = self
                    .collect(
                        &path,
                        "features",
                        page,
                        &[("q", query), ("updated_since", updated_since)],
                    )
                    .await?;
                let scope = if let Some(value) = nonempty(release_id) {
                    format!(" for release '{value}'")
                } else if let Some(value) = nonempty(product_id) {
                    format!(" for product '{value}'")
                } else {
                    String::new()
                };
                let query_detail = nonempty(query)
                    .map_or_else(String::new, |value| format!(" matching query '{value}'"));
                let updated = nonempty(updated_since)
                    .map_or_else(String::new, |value| format!(" updated since '{value}'"));
                render_records(
                    &records,
                    read,
                    Some(format!(
                        "Aha! API returned no features{scope}{query_detail}{updated}."
                    )),
                )
            }
            AhaOperation::ListRequirements {
                feature_id,
                query,
                page,
                read,
            } => {
                validate_identifier(feature_id)?;
                let records = self
                    .collect(
                        &["features", feature_id, "requirements"],
                        "requirements",
                        page,
                        &[("q", query)],
                    )
                    .await?;
                let query_detail = nonempty(query)
                    .map_or_else(String::new, |value| format!(" matching query '{value}'"));
                render_records(
                    &records,
                    read,
                    Some(format!(
                        "Aha! API returned no requirements for feature '{feature_id}'{query_detail}."
                    )),
                )
            }
            AhaOperation::ListReleases {
                product_id,
                parking_lot,
                page,
                read,
            } => {
                let path = nonempty(product_id).map_or_else(
                    || vec!["releases"],
                    |product| vec!["products", product, "releases"],
                );
                let parking = parking_lot.map(|value| if value { "true" } else { "false" });
                let records = self
                    .collect(&path, "releases", page, &[("parking_lot", parking)])
                    .await?;
                let product = nonempty(product_id)
                    .map_or_else(String::new, |value| format!(" for product '{value}'"));
                let parking = parking_lot
                    .map_or_else(String::new, |value| format!(" with parking_lot={value}"));
                render_records(
                    &records,
                    read,
                    Some(format!("Aha! API returned no releases{product}{parking}.")),
                )
            }
            AhaOperation::ListInitiatives {
                product_id,
                page,
                read,
            } => {
                let path = nonempty(product_id).map_or_else(
                    || vec!["initiatives"],
                    |product| vec!["products", product, "initiatives"],
                );
                let records = self.collect(&path, "initiatives", page, &[]).await?;
                let detail = nonempty(product_id)
                    .map_or_else(String::new, |value| format!(" for product '{value}'"));
                render_records(
                    &records,
                    read,
                    Some(format!("Aha! API returned no initiatives{detail}.")),
                )
            }
            AhaOperation::ListEpics {
                product_id,
                release_id,
                page,
                read,
            } => {
                let path = if let Some(release) = nonempty(release_id) {
                    vec!["releases", release, "epics"]
                } else if let Some(product) = nonempty(product_id) {
                    vec!["products", product, "epics"]
                } else {
                    vec!["epics"]
                };
                let records = self.collect(&path, "epics", page, &[]).await?;
                let detail = if let Some(value) = nonempty(release_id) {
                    format!(" for release '{value}'")
                } else if let Some(value) = nonempty(product_id) {
                    format!(" for product '{value}'")
                } else {
                    String::new()
                };
                render_records(
                    &records,
                    read,
                    Some(format!("Aha! API returned no epics{detail}.")),
                )
            }
            AhaOperation::ListIdeas {
                product_id,
                query,
                page,
                read,
            } => {
                let path = nonempty(product_id).map_or_else(
                    || vec!["ideas"],
                    |product| vec!["products", product, "ideas"],
                );
                let records = self.collect(&path, "ideas", page, &[("q", query)]).await?;
                let product = nonempty(product_id)
                    .map_or_else(String::new, |value| format!(" for product '{value}'"));
                let query = nonempty(query)
                    .map_or_else(String::new, |value| format!(" matching query '{value}'"));
                render_records(
                    &records,
                    read,
                    Some(format!("Aha! API returned no ideas{product}{query}.")),
                )
            }
            AhaOperation::Search {
                query,
                record_type,
                page,
                read,
            } => {
                validate_query(query)?;
                let records = self
                    .collect(
                        &["search"],
                        "records",
                        page,
                        &[("q", Some(query)), ("type", record_type)],
                    )
                    .await?;
                let label = nonempty(record_type)
                    .map_or_else(|| "records".to_owned(), |value| format!("{value} records"));
                render_records(
                    &records,
                    read,
                    Some(format!("Aha! API returned no {label} for query '{query}'.")),
                )
            }
            AhaOperation::GetPage {
                reference,
                include_parent,
            } => self.get_page(reference, include_parent).await,
            AhaOperation::SearchDocuments {
                query,
                searchable_type,
            } => self.search_documents(query, searchable_type).await,
            AhaOperation::GetFeatureGql { reference } => {
                self.get_gql_record(reference, ReferenceKind::Feature).await
            }
            AhaOperation::GetRequirementGql { reference } => {
                self.get_gql_record(reference, ReferenceKind::Requirement)
                    .await
            }
            AhaOperation::FindProject { query, page, read } => {
                let records = self
                    .collect(&["products"], "products", page, &[("q", query)])
                    .await?;
                render_records(&records, read, None)
            }
            AhaOperation::SearchRecords {
                record_type,
                query,
                feature_id,
                product_id,
                release_id,
                updated_since,
                page,
                read,
            } => {
                self.search_records(
                    record_type,
                    query,
                    feature_id,
                    product_id,
                    release_id,
                    updated_since,
                    page,
                    read,
                )
                .await
            }
            AhaOperation::ReadRecords {
                record_type,
                reference,
                read,
            } => self.read_records(record_type, reference, read).await,
            AhaOperation::AddComment {
                resource_type,
                resource_id,
                body,
            } => self.add_comment(resource_type, resource_id, body).await,
            AhaOperation::ListComments {
                resource_type,
                resource_id,
                page,
                read,
            } => {
                self.list_comments(resource_type, resource_id, page, read)
                    .await
            }
            AhaOperation::ManageRecord {
                action,
                record_type,
                record_id,
                parent_id,
                properties,
            } => {
                self.manage_record(action, record_type, record_id, parent_id, properties)
                    .await
            }
            AhaOperation::CreateRecord {
                record_type,
                parent_id,
                properties,
            } => {
                self.manage_record("create", record_type, None, Some(parent_id), properties)
                    .await
            }
            AhaOperation::UpdateRecord {
                record_type,
                record_id,
                parent_id,
                properties,
            } => {
                self.manage_record(
                    "update",
                    record_type,
                    Some(record_id),
                    parent_id,
                    properties,
                )
                .await
            }
            AhaOperation::DeleteRecord {
                record_type,
                record_id,
                parent_id,
            } => {
                self.manage_record(
                    "delete",
                    record_type,
                    Some(record_id),
                    parent_id,
                    &Map::new(),
                )
                .await
            }
            AhaOperation::CreateRecordLink {
                from_record_type,
                from_id,
                to_record_type,
                to_id,
                link_type,
            } => {
                self.create_record_link(from_record_type, from_id, to_record_type, to_id, link_type)
                    .await
            }
            AhaOperation::CopyRecord {
                record_type,
                record_id,
            } => self.copy_record(record_type, record_id).await,
            AhaOperation::FieldsMetadata { read } => self.fields_metadata(read).await,
            AhaOperation::FieldOptionsMetadata { field_id, read } => {
                self.field_options_metadata(field_id, read).await
            }
            AhaOperation::AttachFile {
                resource_type,
                resource_id,
                filepath,
                filename,
            } => {
                self.attach_file(resource_type, resource_id, filepath, filename)
                    .await
            }
        }
    }
}

impl AhaClient {
    async fn get_record(
        &self,
        plural: &str,
        singular: &str,
        reference: &str,
        read: ReadOptions<'_>,
    ) -> Result<Value, AhaClientError> {
        validate_identifier(reference)?;
        let payload = self.rest_get(&[plural, reference], &[]).await?;
        let record = payload.get(singular).cloned().unwrap_or_else(|| json!({}));
        render(
            project_record(&record, read.fields),
            read.output_format,
            None,
        )
        .map_err(Into::into)
    }

    async fn get_page(
        &self,
        reference: &str,
        include_parent: bool,
    ) -> Result<Value, AhaClientError> {
        validate_reference(reference, ReferenceKind::Page)?;
        let data = self
            .graphql(
                QUERY_GET_PAGE,
                json!({"id": reference, "includeParent": include_parent}),
            )
            .await?;
        bounded(
            data.get("page")
                .filter(|value| !value.is_null())
                .cloned()
                .unwrap_or_else(|| json!({})),
        )
        .map_err(Into::into)
    }

    async fn search_documents(
        &self,
        query: &str,
        searchable_type: Option<&str>,
    ) -> Result<Value, AhaClientError> {
        validate_query(query)?;
        let searchable_type = nonempty(searchable_type).unwrap_or("Page");
        validate_identifier(searchable_type)?;
        let data = self
            .graphql(
                QUERY_SEARCH_DOCUMENTS,
                json!({"query": query, "searchableType": [searchable_type]}),
            )
            .await?;
        let nodes = data
            .get("searchDocuments")
            .and_then(Value::as_object)
            .and_then(|search| search.get("nodes"))
            .filter(|value| !value.is_null())
            .cloned()
            .unwrap_or_else(|| json!([]));
        if !nodes.is_array() {
            return Err(invalid_response());
        }
        bounded(nodes).map_err(Into::into)
    }

    async fn get_gql_record(
        &self,
        reference: &str,
        kind: ReferenceKind,
    ) -> Result<Value, AhaClientError> {
        validate_reference(reference, kind)?;
        let (query, key) = match kind {
            ReferenceKind::Feature => (QUERY_GET_FEATURE, "feature"),
            ReferenceKind::Requirement => (QUERY_GET_REQUIREMENT, "requirement"),
            ReferenceKind::Page => return Err(invalid_input()),
        };
        let data = self.graphql(query, json!({"id": reference})).await?;
        bounded(
            data.get(key)
                .filter(|value| !value.is_null())
                .cloned()
                .unwrap_or_else(|| json!({})),
        )
        .map_err(Into::into)
    }

    #[allow(clippy::too_many_arguments)]
    async fn search_records(
        &self,
        record_type: &str,
        query: Option<&str>,
        feature_id: Option<&str>,
        product_id: Option<&str>,
        release_id: Option<&str>,
        updated_since: Option<&str>,
        page: PageOptions,
        read: ReadOptions<'_>,
    ) -> Result<Value, AhaClientError> {
        match record_type.trim().to_ascii_lowercase().as_str() {
            "feature" => {
                self.execute(AhaOperation::ListFeatures {
                    product_id,
                    release_id,
                    query,
                    updated_since,
                    page,
                    read,
                })
                .await
            }
            "requirement" => {
                let feature_id = nonempty(feature_id).ok_or_else(invalid_input)?;
                self.execute(AhaOperation::ListRequirements {
                    feature_id,
                    query,
                    page,
                    read,
                })
                .await
            }
            "release" => {
                self.execute(AhaOperation::ListReleases {
                    product_id,
                    parking_lot: None,
                    page,
                    read,
                })
                .await
            }
            "idea" => {
                self.execute(AhaOperation::ListIdeas {
                    product_id,
                    query,
                    page,
                    read,
                })
                .await
            }
            "epic" => {
                self.execute(AhaOperation::ListEpics {
                    product_id,
                    release_id,
                    page,
                    read,
                })
                .await
            }
            "initiative" => {
                self.execute(AhaOperation::ListInitiatives {
                    product_id,
                    page,
                    read,
                })
                .await
            }
            "product" => {
                self.execute(AhaOperation::ListProducts {
                    updated_since,
                    page,
                    read,
                })
                .await
            }
            _ => Err(invalid_input()),
        }
    }

    async fn read_records(
        &self,
        record_type: &str,
        reference: &str,
        read: ReadOptions<'_>,
    ) -> Result<Value, AhaClientError> {
        match record_type.trim().to_ascii_lowercase().as_str() {
            "feature" => {
                self.get_record("features", "feature", reference, read)
                    .await
            }
            "requirement" => {
                self.get_record("requirements", "requirement", reference, read)
                    .await
            }
            "release" => {
                self.get_record("releases", "release", reference, read)
                    .await
            }
            "initiative" => {
                self.get_record("initiatives", "initiative", reference, read)
                    .await
            }
            "epic" => self.get_record("epics", "epic", reference, read).await,
            "idea" => self.get_record("ideas", "idea", reference, read).await,
            "product" => {
                self.get_record("products", "product", reference, read)
                    .await
            }
            "page" => {
                let page = self.get_page(reference, false).await?;
                render(project_record(&page, read.fields), read.output_format, None)
                    .map_err(Into::into)
            }
            _ => Err(invalid_input()),
        }
    }

    async fn add_comment(
        &self,
        resource_type: &str,
        resource_id: &str,
        body: &str,
    ) -> Result<Value, AhaClientError> {
        let (_, plural) = comment_resource(resource_type)?;
        validate_identifier(resource_id)?;
        validate_multiline(body, MAX_COMMENT_BYTES, false)?;
        let payload = self
            .rest_effect(
                Method::POST,
                &[plural, resource_id, "comments"],
                Some(json!({"comment": {"body": body}})),
                &[StatusCode::OK, StatusCode::CREATED],
                false,
            )
            .await?;
        effect_bounded(payload.get("comment").cloned().unwrap_or(payload))
    }

    async fn list_comments(
        &self,
        resource_type: &str,
        resource_id: &str,
        page: PageOptions,
        read: ReadOptions<'_>,
    ) -> Result<Value, AhaClientError> {
        let (canonical, plural) = comment_resource(resource_type)?;
        validate_identifier(resource_id)?;
        let records = self
            .collect(&[plural, resource_id, "comments"], "comments", page, &[])
            .await?;
        render_records(
            &records,
            read,
            Some(format!(
                "Aha! API returned no comments for {canonical} '{resource_id}'."
            )),
        )
    }

    async fn manage_record(
        &self,
        action: &str,
        record_type: &str,
        record_id: Option<&str>,
        parent_id: Option<&str>,
        properties: &Map<String, Value>,
    ) -> Result<Value, AhaClientError> {
        let action = action.trim().to_ascii_lowercase();
        let record_type = manageable_record(record_type)?;
        validate_properties(properties)?;
        let plural = resource_plural(&record_type).ok_or_else(invalid_input)?;
        let path;
        let (method, body, expected) = match action.as_str() {
            "create" => {
                let parent = nonempty(parent_id)
                    .or_else(|| nonempty(record_id))
                    .ok_or_else(invalid_input)?;
                validate_identifier(parent)?;
                let parent_plural = create_parent(&record_type).ok_or_else(invalid_input)?;
                path = vec![parent_plural, parent, plural];
                (
                    Method::POST,
                    Some(json!({record_type.as_str(): properties})),
                    &[StatusCode::OK, StatusCode::CREATED][..],
                )
            }
            "update" => {
                let record = nonempty(record_id).ok_or_else(invalid_input)?;
                validate_identifier(record)?;
                path = mutation_path(&record_type, record, parent_id)?;
                (
                    Method::PUT,
                    Some(json!({record_type.as_str(): properties})),
                    &[StatusCode::OK][..],
                )
            }
            "delete" => {
                let record = nonempty(record_id).ok_or_else(invalid_input)?;
                validate_identifier(record)?;
                path = mutation_path(&record_type, record, parent_id)?;
                (Method::DELETE, None, &[StatusCode::NO_CONTENT][..])
            }
            _ => return Err(invalid_input()),
        };
        let payload = self
            .rest_effect(method, &path, body, expected, action == "delete")
            .await?;
        if action == "delete" {
            let mut result = Map::new();
            result.insert("deleted".to_owned(), Value::Bool(true));
            result.insert("record_type".to_owned(), Value::String(record_type.clone()));
            result.insert(
                "record_id".to_owned(),
                Value::String(record_id.unwrap_or_default().to_owned()),
            );
            if let Some(extra) = payload.as_object() {
                result.extend(extra.clone());
            }
            effect_bounded(Value::Object(result))
        } else {
            effect_bounded(
                payload
                    .get(record_type.as_str())
                    .cloned()
                    .unwrap_or(payload),
            )
        }
    }

    async fn create_record_link(
        &self,
        from_record_type: &str,
        from_id: &str,
        to_record_type: &str,
        to_id: &str,
        link_type: u16,
    ) -> Result<Value, AhaClientError> {
        let source = link_source(from_record_type)?;
        let target = link_target(to_record_type)?;
        if !matches!(link_type, 10 | 20 | 30 | 40 | 50 | 60 | 80) {
            return Err(invalid_input());
        }
        let source_id = self.resolve_link_id(&source, from_id).await?;
        let target_id = self.resolve_link_id(&target, to_id).await?;
        let plural = resource_plural(&source).ok_or_else(invalid_input)?;
        let payload = self.rest_effect(
            Method::POST,
            &[plural, &source_id, "record_links"],
            Some(json!({"record_link": {"record_type": target, "record_id": target_id.parse::<u64>().map_err(|_| invalid_response())?, "link_type": link_type}})),
            &[StatusCode::OK, StatusCode::CREATED, StatusCode::NO_CONTENT],
            true,
        ).await?;
        if let Some(link) = payload.get("record_link") {
            return effect_bounded(link.clone());
        }
        if payload.as_object().is_some_and(Map::is_empty) {
            return effect_bounded(json!({
                "created": true,
                "from_record_type": source,
                "from_reference_or_id": from_id,
                "from_record_id": source_id,
                "to_record_type": target,
                "to_reference_or_id": to_id,
                "to_record_id": target_id,
                "link_type": link_type,
                "link_type_name": link_type_name(link_type),
            }));
        }
        effect_bounded(payload)
    }

    async fn copy_record(
        &self,
        record_type: &str,
        record_id: &str,
    ) -> Result<Value, AhaClientError> {
        if !record_type.trim().eq_ignore_ascii_case("release") {
            return Err(invalid_input());
        }
        validate_identifier(record_id)?;
        let payload = self
            .rest_effect(
                Method::POST,
                &["releases", record_id, "duplicate"],
                None,
                &[StatusCode::OK, StatusCode::CREATED],
                false,
            )
            .await?;
        effect_bounded(payload.get("release").cloned().unwrap_or(payload))
    }

    async fn fields_metadata(&self, read: ReadOptions<'_>) -> Result<Value, AhaClientError> {
        let payload = self.rest_get(&["custom_field_definitions"], &[]).await?;
        let records = payload
            .get("custom_field_definitions")
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default();
        render_records(
            &records,
            read,
            Some("Aha! API returned no custom-field definitions.".to_owned()),
        )
    }

    async fn field_options_metadata(
        &self,
        field_id: &str,
        read: ReadOptions<'_>,
    ) -> Result<Value, AhaClientError> {
        if field_id.is_empty()
            || !field_id.bytes().all(|byte| byte.is_ascii_digit())
            || field_id.len() > MAX_IDENTIFIER_BYTES
        {
            return Err(invalid_input());
        }
        let payload = self
            .rest_get(&["custom_field_definitions", field_id, "options"], &[])
            .await?;
        let records = payload
            .get("options")
            .or_else(|| payload.get("custom_field_options"))
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default();
        render_records(
            &records,
            read,
            Some(format!(
                "Aha! API returned no options for custom-field definition '{field_id}'."
            )),
        )
    }

    async fn attach_file(
        &self,
        resource_type: &str,
        resource_id: &str,
        filepath: &str,
        filename: Option<&str>,
    ) -> Result<Value, AhaClientError> {
        tokio::time::timeout(
            ATTACHMENT_TIMEOUT,
            self.attach_file_inner(resource_type, resource_id, filepath, filename),
        )
        .await
        .map_err(|_| unknown_outcome())?
    }

    async fn attach_file_inner(
        &self,
        resource_type: &str,
        resource_id: &str,
        filepath: &str,
        filename: Option<&str>,
    ) -> Result<Value, AhaClientError> {
        let canonical = normalize_resource(resource_type);
        let plural = resource_plural(&canonical).ok_or_else(invalid_input)?;
        validate_identifier(resource_id)?;
        let form = self
            .artifacts
            .multipart(filepath, nonempty(filename))
            .await
            .map_err(AhaClientError::from)?;
        let note_id = if canonical == "to_do" {
            None
        } else {
            let record = self.rest_get(&[plural, resource_id], &[]).await?;
            let record = record
                .get(canonical.as_str())
                .and_then(Value::as_object)
                .unwrap_or(&record);
            let note_id = record
                .get("description")
                .and_then(Value::as_object)
                .and_then(|description| description.get("id"))
                .and_then(value_identifier)
                .ok_or_else(invalid_response)?;
            Some(note_id.to_owned())
        };
        let path = note_id.as_deref().map_or_else(
            || vec!["tasks", resource_id, "attachments"],
            |note_id| vec!["notes", note_id, "attachments"],
        );
        let payload = self.rest_multipart_effect(&path, form).await?;
        effect_bounded(payload.get("attachment").cloned().unwrap_or(payload))
    }

    async fn resolve_link_id(
        &self,
        record_type: &str,
        reference: &str,
    ) -> Result<String, AhaClientError> {
        validate_identifier(reference)?;
        if reference.bytes().all(|byte| byte.is_ascii_digit()) {
            return Ok(reference.to_owned());
        }
        if matches!(record_type, "goal" | "initiative") {
            let plural = resource_plural(record_type).ok_or_else(invalid_input)?;
            let records = self
                .collect(
                    &[plural],
                    plural,
                    PageOptions {
                        per_page: 100,
                        max_records: 1_000,
                    },
                    &[],
                )
                .await?;
            return records
                .iter()
                .find_map(|record| {
                    let object = record.as_object()?;
                    if !object
                        .get("reference_num")?
                        .as_str()?
                        .eq_ignore_ascii_case(reference)
                    {
                        return None;
                    }
                    value_identifier(object.get("id")?)
                        .filter(|value| value.bytes().all(|byte| byte.is_ascii_digit()))
                        .map(ToOwned::to_owned)
                })
                .ok_or_else(not_found);
        }
        if record_type == "page" {
            validate_reference(reference, ReferenceKind::Page)?;
            let data = self
                .graphql(
                    QUERY_GET_PAGE,
                    json!({"id": reference, "includeParent": false}),
                )
                .await?;
            return data
                .get("page")
                .and_then(Value::as_object)
                .and_then(|page| page.get("id"))
                .and_then(value_identifier)
                .filter(|value| value.bytes().all(|byte| byte.is_ascii_digit()))
                .map(ToOwned::to_owned)
                .ok_or_else(invalid_response);
        }
        if record_type == "release_phase" {
            return Err(invalid_input());
        }
        let plural = resource_plural(record_type).ok_or_else(invalid_input)?;
        let payload = self.rest_get(&[plural, reference], &[]).await?;
        payload
            .get(record_type)
            .and_then(Value::as_object)
            .and_then(|record| record.get("id"))
            .and_then(value_identifier)
            .filter(|value| value.bytes().all(|byte| byte.is_ascii_digit()))
            .map(ToOwned::to_owned)
            .ok_or_else(invalid_response)
    }

    async fn collect(
        &self,
        path: &[&str],
        collection_key: &str,
        page_options: PageOptions,
        query: &[(&str, Option<&str>)],
    ) -> Result<Vec<Value>, AhaClientError> {
        validate_page_options(page_options)?;
        for (_, value) in query {
            if let Some(value) = value {
                validate_optional_query(value)?;
            }
        }
        let mut records = Vec::with_capacity(page_options.max_records.min(256));
        for page in 1..=MAX_PAGES {
            let page_string = page.to_string();
            let per_page = page_options.per_page.to_string();
            let mut parameters = Vec::with_capacity(query.len() + 2);
            parameters.push(("page", Some(page_string.as_str())));
            parameters.push(("per_page", Some(per_page.as_str())));
            parameters.extend_from_slice(query);
            let payload = self.rest_get(path, &parameters).await?;
            let collection = payload
                .get(collection_key)
                .and_then(Value::as_array)
                .ok_or_else(invalid_response)?;
            for record in collection {
                if !record.is_object() {
                    return Err(invalid_response());
                }
                records.push(record.clone());
                if records.len() >= page_options.max_records {
                    return Ok(records);
                }
            }
            let Some(pagination) = payload.get("pagination") else {
                break;
            };
            let pagination = pagination.as_object().ok_or_else(invalid_response)?;
            let current = pagination
                .get("current_page")
                .and_then(Value::as_u64)
                .ok_or_else(invalid_response)?;
            let total = pagination
                .get("total_pages")
                .and_then(Value::as_u64)
                .ok_or_else(invalid_response)?;
            if current != u64::try_from(page).map_err(|_| invalid_response())? || total < current {
                return Err(invalid_response());
            }
            if current >= total {
                break;
            }
            if page == MAX_PAGES {
                return Err(resource_exhausted());
            }
        }
        Ok(records)
    }

    async fn rest_get(
        &self,
        path: &[&str],
        query: &[(&str, Option<&str>)],
    ) -> Result<Map<String, Value>, AhaClientError> {
        let request = self.request(Method::GET, "api/v1", path, query, None)?;
        let response = self.transport.execute(request, false).await?;
        response_object(response, false, &[StatusCode::OK], false)
    }

    async fn rest_effect(
        &self,
        method: Method,
        path: &[&str],
        body: Option<Value>,
        expected: &[StatusCode],
        allow_empty: bool,
    ) -> Result<Value, AhaClientError> {
        let request = self.request(method, "api/v1", path, &[], body)?;
        let response = self.transport.execute(request, true).await?;
        Ok(Value::Object(response_object(
            response,
            true,
            expected,
            allow_empty,
        )?))
    }

    async fn rest_multipart_effect(
        &self,
        path: &[&str],
        form: reqwest::multipart::Form,
    ) -> Result<Value, AhaClientError> {
        let url = self.endpoint("api/v1", path, &[])?;
        let authorization = self.authorization_header()?;
        let mut request = self
            .request_client
            .post(url)
            .header(AUTHORIZATION, authorization)
            .header(ACCEPT, "application/json")
            .multipart(form)
            .build()
            .map_err(|_| invalid_input())?;
        *request.timeout_mut() = Some(ATTACHMENT_TIMEOUT);
        validate_request_bound(&request)?;
        let response = self.transport.execute(request, true).await?;
        Ok(Value::Object(response_object(
            response,
            true,
            &[StatusCode::OK, StatusCode::CREATED, StatusCode::NO_CONTENT],
            true,
        )?))
    }

    async fn graphql(
        &self,
        query: &str,
        variables: Value,
    ) -> Result<Map<String, Value>, AhaClientError> {
        let request = self.request(
            Method::POST,
            "api/v2",
            &["graphql"],
            &[],
            Some(json!({"query": query, "variables": variables})),
        )?;
        let response = self.transport.execute(request, false).await?;
        let payload = response_object(response, false, &[StatusCode::OK], false)?;
        if payload.get("errors").is_some_and(|errors| {
            !errors.is_null() && !errors.as_array().is_some_and(Vec::is_empty)
        }) {
            return Err(invalid_response());
        }
        payload
            .get("data")
            .and_then(Value::as_object)
            .cloned()
            .ok_or_else(invalid_response)
    }

    fn request(
        &self,
        method: Method,
        prefix: &str,
        path: &[&str],
        query: &[(&str, Option<&str>)],
        body: Option<Value>,
    ) -> Result<Request, AhaClientError> {
        let url = self.endpoint(prefix, path, query)?;
        let authorization = self.authorization_header()?;
        let mut builder = self
            .request_client
            .request(method, url)
            .header(AUTHORIZATION, authorization)
            .header(ACCEPT, "application/json");
        if let Some(body) = body {
            let encoded = serde_json::to_vec(&body).map_err(|_| invalid_input())?;
            if encoded.len() > MAX_REQUEST_BYTES {
                return Err(resource_exhausted());
            }
            builder = builder
                .header(CONTENT_TYPE, "application/json")
                .body(encoded);
        }
        let request = builder.build().map_err(|_| invalid_input())?;
        validate_request_bound(&request)?;
        Ok(request)
    }

    fn endpoint(
        &self,
        prefix: &str,
        path: &[&str],
        query: &[(&str, Option<&str>)],
    ) -> Result<Url, AhaClientError> {
        let mut url = self.base_url.clone();
        {
            let mut segments = url
                .path_segments_mut()
                .map_err(|()| invalid_configuration())?;
            segments.clear();
            for segment in prefix.split('/') {
                segments.push(segment);
            }
            for segment in path {
                validate_identifier(segment)?;
                segments.push(segment);
            }
        }
        {
            let mut pairs = url.query_pairs_mut();
            for (name, value) in query {
                if let Some(value) = nonempty(*value) {
                    pairs.append_pair(name, value);
                }
            }
        }
        Ok(url)
    }

    fn authorization_header(&self) -> Result<HeaderValue, AhaClientError> {
        let value = Zeroizing::new(format!("Bearer {}", self.api_key.as_str()));
        let mut header = HeaderValue::from_str(&value).map_err(|_| invalid_configuration())?;
        header.set_sensitive(true);
        Ok(header)
    }
}
