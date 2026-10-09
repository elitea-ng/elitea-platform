use std::collections::BTreeMap;
use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use reqwest::header::{ACCEPT, CONTENT_TYPE, HeaderName, HeaderValue};
use reqwest::{Method, StatusCode, Url};
use serde_json::{Map, Value, json};
use tokio::sync::Mutex;
use tokio_stream::StreamExt;
use zeroize::{Zeroize, Zeroizing};

use super::config::{DynamicExecutionProfile, PostmanToolkitConfig};

const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 8;
const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_REQUEST_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_IDENTIFIER_BYTES: usize = 1_024;
const MAX_PATH_BYTES: usize = 4 * 1_024;
const MAX_TEXT_BYTES: usize = 64 * 1_024;
const MAX_DYNAMIC_HEADERS: usize = 100;
const MAX_DYNAMIC_HEADER_BYTES: usize = 64 * 1_024;
const MAX_ITEMS: usize = 4_096;
const MAX_NODES: usize = 65_536;
const MAX_DEPTH: usize = 64;
const USER_AGENT: &str = "elitea-worker-rust/0.1";
const API_KEY_HEADER: HeaderName = HeaderName::from_static("x-api-key");

type DynamicQueryPairs = Vec<(String, String)>;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PostmanClientErrorCode {
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
pub(crate) struct PostmanClientError {
    code: PostmanClientErrorCode,
    retryable: bool,
}

impl PostmanClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> PostmanClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            PostmanClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "postman.configuration.invalid",
                "the Postman toolkit configuration is invalid",
            ),
            PostmanClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "postman.request.invalid",
                "the Postman request is invalid",
            ),
            PostmanClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "postman.authentication.failed",
                "Postman authentication failed",
            ),
            PostmanClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "postman.authorization.failed",
                "Postman did not authorize the request",
            ),
            PostmanClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "postman.resource.not_found",
                "the requested Postman resource was not found",
            ),
            PostmanClientErrorCode::Conflict => (
                ErrorCategory::InvalidInput,
                "postman.resource.conflict",
                "the Postman resource is in conflict",
            ),
            PostmanClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "postman.rate_limited",
                "Postman rate limited the request",
            ),
            PostmanClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "postman.timeout",
                "the Postman request timed out",
            ),
            PostmanClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "postman.unavailable",
                "Postman is unavailable",
            ),
            PostmanClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "postman.response.invalid",
                "Postman returned an invalid response",
            ),
            PostmanClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "postman.resource_exhausted",
                "the Postman request or response exceeds the approved limit",
            ),
            PostmanClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "postman.effect.unknown_outcome",
                "the remote effect may have been applied; reconcile it before retrying",
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
        code: PostmanClientErrorCode,
        retryable: bool,
    ) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for PostmanClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("PostmanClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for PostmanClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            PostmanClientErrorCode::InvalidConfiguration => {
                "the Postman client configuration is invalid"
            }
            PostmanClientErrorCode::InvalidInput => "the Postman request is invalid",
            PostmanClientErrorCode::Authentication => "Postman authentication failed",
            PostmanClientErrorCode::Authorization => "Postman authorization failed",
            PostmanClientErrorCode::NotFound => "the Postman resource was not found",
            PostmanClientErrorCode::Conflict => "the Postman resource is in conflict",
            PostmanClientErrorCode::RateLimited => "Postman rate limited the request",
            PostmanClientErrorCode::Timeout => "the Postman operation timed out",
            PostmanClientErrorCode::DependencyUnavailable => "Postman is unavailable",
            PostmanClientErrorCode::InvalidResponse => "Postman returned an invalid response",
            PostmanClientErrorCode::ResourceExhausted => {
                "the Postman request or response exceeds its approved limit"
            }
            PostmanClientErrorCode::UnknownOutcome => {
                "the remote effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for PostmanClientError {}

#[derive(Clone)]
pub(in crate::toolkits) enum PostmanOperation {
    GetCollections,
    GetCollection {
        collection_id: Option<String>,
    },
    GetFolder {
        folder_path: String,
    },
    GetRequestByPath {
        request_path: String,
    },
    GetRequestById {
        request_id: String,
    },
    GetRequestScript {
        request_path: String,
        script_type: String,
    },
    SearchRequests {
        query: String,
        search_in: String,
        method: Option<String>,
    },
    Analyze {
        scope: String,
        target_path: Option<String>,
        include_improvements: bool,
    },
    ExecuteRequest {
        request_path: String,
        override_variables: Map<String, Value>,
    },
    UpdateCollectionDescription {
        description: String,
        collection_id: Option<String>,
    },
    UpdateCollectionVariables {
        variables: Option<Vec<Value>>,
    },
    UpdateCollectionAuth {
        auth: Option<Value>,
    },
    DeleteCollection {
        collection_id: Option<String>,
    },
    DuplicateCollection {
        new_name: String,
    },
    CreateFolder {
        name: String,
        description: Option<String>,
        parent_path: Option<String>,
        auth: Option<Value>,
    },
    UpdateFolder {
        folder_path: String,
        name: Option<String>,
        description: Option<String>,
        auth: Option<Value>,
    },
    DeleteFolder {
        folder_path: String,
    },
    MoveFolder {
        source_path: String,
        target_path: Option<String>,
    },
    CreateRequest {
        folder_path: Option<String>,
        name: String,
        method: String,
        url: String,
        description: Option<String>,
        headers: Option<Vec<Value>>,
        body: Option<Value>,
        auth: Option<Value>,
        tests: Option<String>,
        pre_request_script: Option<String>,
    },
    UpdateRequestName {
        request_path: String,
        name: String,
    },
    UpdateRequestMethod {
        request_path: String,
        method: String,
    },
    UpdateRequestUrl {
        request_path: String,
        url: String,
    },
    UpdateRequestDescription {
        request_path: String,
        description: String,
    },
    UpdateRequestHeaders {
        request_path: String,
        headers: String,
    },
    UpdateRequestBody {
        request_path: String,
        body: Value,
    },
    UpdateRequestAuth {
        request_path: String,
        auth: Option<Value>,
    },
    UpdateRequestTests {
        request_path: String,
        tests: String,
    },
    UpdateRequestPreScript {
        request_path: String,
        pre_request_script: String,
    },
    DeleteRequest {
        request_path: String,
    },
    DuplicateRequest {
        source_path: String,
        new_name: String,
        target_path: Option<String>,
    },
    MoveRequest {
        source_path: String,
        target_path: Option<String>,
    },
}

#[async_trait]
pub(in crate::toolkits) trait PostmanApi: Send + Sync {
    async fn execute(&self, operation: PostmanOperation) -> Result<Value, PostmanClientError>;
}

pub(in crate::toolkits) struct DynamicRequest {
    pub(in crate::toolkits) method: Method,
    pub(in crate::toolkits) url: Url,
    pub(in crate::toolkits) headers: Vec<(HeaderName, HeaderValue)>,
    pub(in crate::toolkits) query: Vec<(String, String)>,
    pub(in crate::toolkits) body: Option<Vec<u8>>,
}

struct PreparedDynamicRequest {
    request: DynamicRequest,
    method: Box<str>,
    safe_url: String,
}

pub(in crate::toolkits) struct DynamicResponse {
    pub(in crate::toolkits) status: StatusCode,
    pub(in crate::toolkits) reason: Box<str>,
    pub(in crate::toolkits) headers: Map<String, Value>,
    pub(in crate::toolkits) body: Value,
    pub(in crate::toolkits) size_bytes: usize,
}

/// Separate downstream authority. Production has deliberately no constructor
/// or injection point until the platform can issue a claim-bound egress grant.
#[async_trait]
pub(in crate::toolkits) trait DynamicEgressAuthority: Send + Sync {
    async fn dispatch(
        &self,
        request: DynamicRequest,
    ) -> Result<DynamicResponse, PostmanClientError>;
}

pub(crate) struct PostmanClient {
    management: ManagementAuthority,
    dynamic: DynamicExecution,
    mutation_gate: Mutex<()>,
}

struct ManagementAuthority {
    base_url: Url,
    workspace_id: Box<str>,
    api_key: Zeroizing<String>,
    collection_id: Box<str>,
    request_builder: reqwest::Client,
    transport: Arc<dyn ManagementTransport>,
}

struct DynamicExecution {
    profile: DynamicExecutionProfile,
    authority: Option<Arc<dyn DynamicEgressAuthority>>,
}

struct ManagementJsonCall<'a> {
    method: Method,
    segments: &'a [&'a str],
    query: &'a [(&'a str, &'a str)],
    body: Option<&'a Value>,
    effect: bool,
    expected: StatusCode,
    allow_empty: bool,
}

pub(in crate::toolkits) struct ManagementHttpResponse {
    pub(in crate::toolkits) status: StatusCode,
    pub(in crate::toolkits) body: Vec<u8>,
}

#[derive(Clone, Copy)]
pub(in crate::toolkits) enum ManagementTransportError {
    Timeout,
    Connect,
    Response,
}

#[async_trait]
pub(in crate::toolkits) trait ManagementTransport: Send + Sync {
    async fn send(
        &self,
        request: reqwest::Request,
    ) -> Result<ManagementHttpResponse, ManagementTransportError>;
}

struct ReqwestManagementTransport {
    http: reqwest::Client,
}

#[async_trait]
impl ManagementTransport for ReqwestManagementTransport {
    async fn send(
        &self,
        request: reqwest::Request,
    ) -> Result<ManagementHttpResponse, ManagementTransportError> {
        let response = self.http.execute(request).await.map_err(|error| {
            if error.is_timeout() {
                ManagementTransportError::Timeout
            } else if error.is_connect() {
                ManagementTransportError::Connect
            } else {
                ManagementTransportError::Response
            }
        })?;
        let status = response.status();
        let body = read_response_body(response, false)
            .await
            .map_err(|_| ManagementTransportError::Response)?;
        Ok(ManagementHttpResponse { status, body })
    }
}

impl PostmanClient {
    pub(crate) fn new(config: PostmanToolkitConfig) -> Result<Self, PostmanClientError> {
        let config = config.into_client_parts();
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
            management: ManagementAuthority {
                base_url: config.base_url,
                workspace_id: config.workspace_id,
                api_key: config.api_key,
                collection_id: config.collection_id,
                request_builder: http.clone(),
                transport: Arc::new(ReqwestManagementTransport { http }),
            },
            dynamic: DynamicExecution {
                profile: config.dynamic_profile,
                authority: None,
            },
            mutation_gate: Mutex::new(()),
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn fixture_with_dynamic_authority(
        config: PostmanToolkitConfig,
        authority: Arc<dyn DynamicEgressAuthority>,
    ) -> Result<Self, PostmanClientError> {
        let mut client = Self::new(config)?;
        client.dynamic.authority = Some(authority);
        Ok(client)
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn fixture_with_management_transport(
        config: PostmanToolkitConfig,
        transport: Arc<dyn ManagementTransport>,
    ) -> Result<Self, PostmanClientError> {
        let mut client = Self::new(config)?;
        client.management.transport = transport;
        Ok(client)
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn fixture_with_management_and_dynamic(
        config: PostmanToolkitConfig,
        transport: Arc<dyn ManagementTransport>,
        authority: Arc<dyn DynamicEgressAuthority>,
    ) -> Result<Self, PostmanClientError> {
        let mut client = Self::new(config)?;
        client.management.transport = transport;
        client.dynamic.authority = Some(authority);
        Ok(client)
    }

    async fn get_collections(&self) -> Result<Value, PostmanClientError> {
        let value = self
            .management
            .json(ManagementJsonCall {
                method: Method::GET,
                segments: &["collections"],
                query: &[("workspace", &self.management.workspace_id)],
                body: None,
                effect: false,
                expected: StatusCode::OK,
                allow_empty: false,
            })
            .await?;
        if value
            .get("collections")
            .and_then(Value::as_array)
            .is_some_and(Vec::is_empty)
        {
            self.management
                .json(ManagementJsonCall {
                    method: Method::GET,
                    segments: &["workspaces", &self.management.workspace_id],
                    query: &[],
                    body: None,
                    effect: false,
                    expected: StatusCode::OK,
                    allow_empty: false,
                })
                .await?;
        }
        bounded_output(value, false)
    }

    async fn get_collection(&self, collection_id: &str) -> Result<Value, PostmanClientError> {
        validate_text(collection_id, MAX_IDENTIFIER_BYTES, false)?;
        self.management
            .json(ManagementJsonCall {
                method: Method::GET,
                segments: &["collections", collection_id],
                query: &[],
                body: None,
                effect: false,
                expected: StatusCode::OK,
                allow_empty: false,
            })
            .await
    }

    async fn get_collection_flat(
        &self,
        collection_id: Option<&str>,
    ) -> Result<Value, PostmanClientError> {
        let collection_id = collection_id.unwrap_or(&self.management.collection_id);
        let collection = self.get_collection(collection_id).await?;
        bounded_output(flatten_collection(&collection, None)?, false)
    }

    async fn get_folder_flat(&self, folder_path: &str) -> Result<Value, PostmanClientError> {
        validate_path(folder_path)?;
        let collection = self.get_collection(&self.management.collection_id).await?;
        bounded_output(flatten_collection(&collection, Some(folder_path))?, false)
    }

    async fn get_request_by_path(&self, request_path: &str) -> Result<Value, PostmanClientError> {
        validate_path(request_path)?;
        let collection = self.get_collection(&self.management.collection_id).await?;
        let request =
            find_request(collection_data(&collection)?, request_path)?.ok_or_else(not_found)?;
        let request_id = item_id(request)?;
        self.get_request_by_id(request_id).await
    }

    async fn get_request_by_id(&self, request_id: &str) -> Result<Value, PostmanClientError> {
        let value = self.get_raw_request_by_id(request_id).await?;
        bounded_output(redact_sensitive_fields(value), false)
    }

    async fn get_raw_request_by_id(&self, request_id: &str) -> Result<Value, PostmanClientError> {
        validate_text(request_id, MAX_IDENTIFIER_BYTES, false)?;
        self.management
            .json(ManagementJsonCall {
                method: Method::GET,
                segments: &[
                    "collections",
                    &self.management.collection_id,
                    "requests",
                    request_id,
                ],
                query: &[],
                body: None,
                effect: false,
                expected: StatusCode::OK,
                allow_empty: false,
            })
            .await
    }

    async fn get_request_script(
        &self,
        request_path: &str,
        script_type: &str,
    ) -> Result<Value, PostmanClientError> {
        validate_path(request_path)?;
        if !matches!(script_type, "test" | "prerequest") {
            return Err(invalid_input());
        }
        let collection = self.get_collection(&self.management.collection_id).await?;
        let request =
            find_request(collection_data(&collection)?, request_path)?.ok_or_else(not_found)?;
        let request_id = item_id(request)?;
        let mut script = script_from_events(request, script_type)?;
        if script.is_none()
            && let Ok(individual) = self.get_raw_request_by_id(request_id).await
        {
            let field = if script_type == "test" {
                "tests"
            } else {
                "preRequestScript"
            };
            script = individual
                .get(field)
                .and_then(Value::as_str)
                .map(str::to_owned);
        }
        let value = match script.filter(|value| !value.trim().is_empty()) {
            Some(script) => json!({
                "success": true,
                "script_type": script_type,
                "script_content": script.trim(),
                "request_path": request_path
            }),
            None => json!({
                "success": false,
                "message": format!("No {script_type} script found for request '{request_path}'")
            }),
        };
        bounded_output(value, false)
    }

    async fn search_requests(
        &self,
        query: &str,
        search_in: &str,
        method: Option<&str>,
    ) -> Result<Value, PostmanClientError> {
        validate_text(query, MAX_TEXT_BYTES, true)?;
        if !matches!(search_in, "all" | "name" | "url" | "description") {
            return Err(invalid_input());
        }
        if let Some(method) = method {
            validate_http_method(method)?;
        }
        let collection = self.get_collection(&self.management.collection_id).await?;
        let collection = collection_data(&collection)?;
        let query_lower = query.to_lowercase();
        let mut matches = Map::new();
        collect_search_matches(
            collection_items(collection)?,
            "",
            &query_lower,
            search_in,
            method,
            &mut matches,
        )?;
        bounded_output(
            json!({
                "query": query,
                "search_in": search_in,
                "method_filter": method,
                "results_count": matches.len(),
                "items": matches
            }),
            false,
        )
    }

    async fn analyze(
        &self,
        scope: &str,
        target_path: Option<&str>,
        include_improvements: bool,
    ) -> Result<Value, PostmanClientError> {
        if !matches!(scope, "collection" | "folder" | "request")
            || (scope != "collection" && target_path.is_none())
        {
            return Err(invalid_input());
        }
        if let Some(path) = target_path {
            validate_path(path)?;
        }
        let collection = self.get_collection(&self.management.collection_id).await?;
        let result = super::analysis::analyze(
            &collection,
            &self.management.collection_id,
            scope,
            target_path,
            include_improvements,
        )?;
        bounded_output(result, false)
    }
}

pub(super) fn collection_data(value: &Value) -> Result<&Map<String, Value>, PostmanClientError> {
    value
        .get("collection")
        .and_then(Value::as_object)
        .ok_or_else(invalid_response)
}

fn collection_data_mut(value: &mut Value) -> Result<&mut Map<String, Value>, PostmanClientError> {
    value
        .get_mut("collection")
        .and_then(Value::as_object_mut)
        .ok_or_else(invalid_response)
}

pub(super) fn collection_items(
    value: &Map<String, Value>,
) -> Result<&Vec<Value>, PostmanClientError> {
    value
        .get("item")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)
}

fn collection_items_mut(
    value: &mut Map<String, Value>,
) -> Result<&mut Vec<Value>, PostmanClientError> {
    value
        .get_mut("item")
        .and_then(Value::as_array_mut)
        .ok_or_else(invalid_response)
}

fn path_parts(path: &str) -> Vec<&str> {
    path.split('/')
        .map(str::trim)
        .filter(|part| !part.is_empty())
        .collect()
}

#[derive(Clone, Copy)]
pub(super) enum ItemKind {
    Folder,
    Request,
}

pub(super) fn resolve_item<'a>(
    collection: &'a Map<String, Value>,
    path: &str,
    kind: ItemKind,
) -> Result<Option<&'a Value>, PostmanClientError> {
    let parts = path_parts(path);
    let Some(mut items) = collection.get("item").and_then(Value::as_array) else {
        return Err(invalid_response());
    };
    for (index, part) in parts.iter().enumerate() {
        let mut matches = items.iter().filter(|item| {
            item.get("name")
                .and_then(Value::as_str)
                .is_some_and(|name| name.eq_ignore_ascii_case(part))
        });
        let Some(item) = matches.next() else {
            return Ok(None);
        };
        if matches.next().is_some() {
            return Err(conflict());
        }
        if index + 1 == parts.len() {
            let matches_kind = match kind {
                ItemKind::Folder => item.get("item").is_some(),
                ItemKind::Request => item.get("request").is_some(),
            };
            return Ok(matches_kind.then_some(item));
        }
        let Some(next) = item.get("item").and_then(Value::as_array) else {
            return Ok(None);
        };
        items = next;
    }
    Ok(None)
}

fn find_request<'a>(
    collection: &'a Map<String, Value>,
    path: &str,
) -> Result<Option<&'a Value>, PostmanClientError> {
    resolve_item(collection, path, ItemKind::Request)
}

fn find_folder<'a>(
    collection: &'a Map<String, Value>,
    path: &str,
) -> Result<Option<&'a Value>, PostmanClientError> {
    resolve_item(collection, path, ItemKind::Folder)
}

fn item_id(item: &Value) -> Result<&str, PostmanClientError> {
    item.get("id")
        .or_else(|| item.get("_postman_id"))
        .and_then(Value::as_str)
        .ok_or_else(invalid_response)
}

fn script_from_events(
    item: &Value,
    script_type: &str,
) -> Result<Option<String>, PostmanClientError> {
    let Some(events) = item.get("event") else {
        return Ok(None);
    };
    let events = events.as_array().ok_or_else(invalid_response)?;
    for event in events {
        if event.get("listen").and_then(Value::as_str) != Some(script_type) {
            continue;
        }
        let Some(exec) = event.get("script").and_then(|value| value.get("exec")) else {
            return Ok(None);
        };
        if let Some(lines) = exec.as_array() {
            let mut output = String::new();
            for (index, line) in lines.iter().enumerate() {
                let line = line.as_str().ok_or_else(invalid_response)?;
                if index != 0 {
                    output.push('\n');
                }
                output.push_str(line);
                if output.len() > MAX_TEXT_BYTES {
                    return Err(resource_exhausted());
                }
            }
            return Ok(Some(output));
        }
        return Ok(Some(exec.as_str().unwrap_or_default().to_owned()));
    }
    Ok(None)
}

fn flatten_collection(
    response: &Value,
    folder_path: Option<&str>,
) -> Result<Value, PostmanClientError> {
    let collection = collection_data(response)?;
    let info = collection
        .get("info")
        .and_then(Value::as_object)
        .ok_or_else(invalid_response)?;
    let mut result = if let Some(path) = folder_path {
        json!({"folder_path": path, "items": {}})
    } else {
        json!({
            "collection_postman_id": info.get("_postman_id").cloned().unwrap_or(Value::Null),
            "name": info.get("name").cloned().unwrap_or(Value::Null),
            "description": info.get("description").cloned().unwrap_or(Value::Null),
            "variables": redact_variables(collection.get("variable")),
            "updatedAt": info.get("updatedAt").cloned().unwrap_or(Value::Null),
            "createdAt": info.get("createdAt").cloned().unwrap_or(Value::Null),
            "lastUpdatedBy": info.get("lastUpdatedBy").cloned().unwrap_or(Value::Null),
            "uid": info.get("uid").cloned().unwrap_or(Value::Null),
            "items": {}
        })
    };
    let output = result
        .get_mut("items")
        .and_then(Value::as_object_mut)
        .ok_or_else(invalid_response)?;
    let mut stack = Vec::new();
    for item in collection_items(collection)?.iter().rev() {
        stack.push((String::new(), item));
    }
    let mut folder_found = folder_path.is_none();
    while let Some((parent, item)) = stack.pop() {
        let item = item.as_object().ok_or_else(invalid_response)?;
        let name = item
            .get("name")
            .and_then(Value::as_str)
            .ok_or_else(invalid_response)?;
        validate_text(name, MAX_IDENTIFIER_BYTES, true)?;
        let path = if parent.is_empty() {
            name.to_owned()
        } else {
            format!("{parent}/{name}")
        };
        if path.len() > MAX_PATH_BYTES {
            return Err(resource_exhausted());
        }
        let in_scope = folder_path.is_none_or(|target| {
            path.eq_ignore_ascii_case(target)
                || path
                    .get(..target.len())
                    .is_some_and(|prefix| prefix.eq_ignore_ascii_case(target))
                    && path.as_bytes().get(target.len()) == Some(&b'/')
        });
        let is_ancestor = folder_path.is_some_and(|target| {
            target
                .get(..path.len())
                .is_some_and(|prefix| prefix.eq_ignore_ascii_case(&path))
                && target.as_bytes().get(path.len()) == Some(&b'/')
        });
        if in_scope {
            if path.eq_ignore_ascii_case(folder_path.unwrap_or_default()) {
                folder_found = true;
            }
            let projected = if let Some(children) = item.get("item") {
                let children = children.as_array().ok_or_else(invalid_response)?;
                for child in children.iter().rev() {
                    stack.push((path.clone(), child));
                }
                json!({
                    "type": "folder",
                    "id": item.get("id").cloned().unwrap_or(Value::Null),
                    "uid": item.get("uid").cloned().unwrap_or(Value::Null),
                    "description": item.get("description").cloned().unwrap_or(Value::Null)
                })
            } else {
                flatten_request(item, &path)?
            };
            if output.insert(path, projected).is_some() {
                return Err(conflict());
            }
        } else if is_ancestor && let Some(children) = item.get("item").and_then(Value::as_array) {
            for child in children.iter().rev() {
                stack.push((path.clone(), child));
            }
        }
        if output.len() > MAX_ITEMS {
            return Err(resource_exhausted());
        }
    }
    if !folder_found || (folder_path.is_some() && output.is_empty()) {
        return Ok(Value::String(format!(
            "Folder '{}' not found in collection",
            folder_path.unwrap_or_default()
        )));
    }
    Ok(result)
}

fn flatten_request(item: &Map<String, Value>, path: &str) -> Result<Value, PostmanClientError> {
    let request = item
        .get("request")
        .and_then(Value::as_object)
        .ok_or_else(invalid_response)?;
    let raw_url = match request.get("url") {
        Some(Value::String(url)) => url.clone(),
        Some(Value::Object(url)) => url
            .get("raw")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned(),
        None | Some(_) => String::new(),
    };
    let url = sanitize_url(&raw_url)?;
    let params = request
        .get("url")
        .and_then(Value::as_object)
        .and_then(|url| url.get("query"))
        .and_then(Value::as_array)
        .map(|values| {
            values
                .iter()
                .filter_map(|value| value.as_object())
                .map(|value| {
                    json!({
                        "key": value.get("key").cloned().unwrap_or(Value::String(String::new())),
                        "value": "<redacted>",
                        "disabled": value.get("disabled").cloned().unwrap_or(Value::Bool(false))
                    })
                })
                .collect::<Vec<_>>()
        })
        .unwrap_or_default();
    let headers = redact_headers(request.get("header"));
    let mut value = json!({
        "id": item.get("id").cloned().unwrap_or(Value::Null),
        "uid": item.get("uid").cloned().unwrap_or(Value::Null),
        "full_postman_path": path,
        "type": "request",
        "method": request.get("method").cloned().unwrap_or(Value::String("GET".to_owned())),
        "request_url": url,
        "headers": headers,
        "params": params
    });
    if request.get("body").is_some_and(|body| !body.is_null()) {
        value["body"] = json!({"type":"redacted","data":"<redacted>"});
    }
    if let Some(description) = request.get("description") {
        value["description"] = description.clone();
    }
    Ok(value)
}

fn collect_search_matches(
    items: &[Value],
    parent: &str,
    query: &str,
    search_in: &str,
    method: Option<&str>,
    output: &mut Map<String, Value>,
) -> Result<(), PostmanClientError> {
    let mut stack = Vec::new();
    for item in items.iter().rev() {
        stack.push((parent.to_owned(), item));
    }
    while let Some((parent, item)) = stack.pop() {
        let object = item.as_object().ok_or_else(invalid_response)?;
        let name = object
            .get("name")
            .and_then(Value::as_str)
            .ok_or_else(invalid_response)?;
        let path = if parent.is_empty() {
            name.to_owned()
        } else {
            format!("{parent}/{name}")
        };
        if path.len() > MAX_PATH_BYTES {
            return Err(resource_exhausted());
        }
        if let Some(children) = object.get("item") {
            let children = children.as_array().ok_or_else(invalid_response)?;
            for child in children.iter().rev() {
                stack.push((path.clone(), child));
            }
            continue;
        }
        let request = object
            .get("request")
            .and_then(Value::as_object)
            .ok_or_else(invalid_response)?;
        if method.is_some_and(|method| {
            !request
                .get("method")
                .and_then(Value::as_str)
                .is_some_and(|candidate| candidate.eq_ignore_ascii_case(method))
        }) {
            continue;
        }
        let raw_url = match request.get("url") {
            Some(Value::String(value)) => value.as_str(),
            Some(Value::Object(value)) => {
                value.get("raw").and_then(Value::as_str).unwrap_or_default()
            }
            None | Some(_) => "",
        };
        let description = object
            .get("description")
            .or_else(|| request.get("description"))
            .and_then(Value::as_str)
            .unwrap_or_default();
        let found = (matches!(search_in, "all" | "name") && name.to_lowercase().contains(query))
            || (matches!(search_in, "all" | "url") && raw_url.to_lowercase().contains(query))
            || (matches!(search_in, "all" | "description")
                && description.to_lowercase().contains(query));
        if found {
            if output.len() >= MAX_ITEMS || output.contains_key(&path) {
                return Err(if output.contains_key(&path) {
                    conflict()
                } else {
                    resource_exhausted()
                });
            }
            output.insert(path.clone(), flatten_request(object, &path)?);
        }
    }
    Ok(())
}

fn redact_variables(value: Option<&Value>) -> Value {
    let Some(values) = value.and_then(Value::as_array) else {
        return Value::Null;
    };
    Value::Array(
        values
            .iter()
            .map(|value| {
                let Some(object) = value.as_object() else {
                    return Value::Null;
                };
                let mut redacted = object.clone();
                if redacted.contains_key("value") {
                    redacted.insert("value".to_owned(), Value::String("<redacted>".to_owned()));
                }
                Value::Object(redacted)
            })
            .collect(),
    )
}

fn redact_headers(value: Option<&Value>) -> Value {
    let Some(values) = value.and_then(Value::as_array) else {
        return Value::Array(Vec::new());
    };
    Value::Array(
        values
            .iter()
            .map(|value| {
                let Some(object) = value.as_object() else {
                    return Value::Null;
                };
                let mut redacted = object.clone();
                if redacted.contains_key("value") {
                    redacted.insert("value".to_owned(), Value::String("<redacted>".to_owned()));
                }
                Value::Object(redacted)
            })
            .collect(),
    )
}

fn redact_sensitive_fields(mut value: Value) -> Value {
    redact_request_node(&mut value, RedactionContext::Ordinary);
    value
}

#[derive(Clone, Copy)]
enum RedactionContext {
    Ordinary,
    Header,
    Variable,
}

fn redact_request_node(value: &mut Value, context: RedactionContext) {
    match value {
        Value::Object(object) => {
            for (key, child) in object.iter_mut() {
                let lower = key.to_ascii_lowercase();
                if matches!(
                    lower.as_str(),
                    "auth"
                        | "authorization"
                        | "body"
                        | "raw"
                        | "script"
                        | "event"
                        | "events"
                        | "tests"
                        | "prerequestscript"
                ) {
                    *child = Value::String("<redacted>".to_owned());
                    continue;
                }
                if lower == "url" || lower == "request_url" {
                    if let Some(raw) = child
                        .as_str()
                        .or_else(|| child.get("raw").and_then(Value::as_str))
                    {
                        *child = Value::String(
                            sanitize_url(raw).unwrap_or_else(|_| "<invalid-url>".to_owned()),
                        );
                    }
                    continue;
                }
                let next = match lower.as_str() {
                    "header" | "headers" => RedactionContext::Header,
                    "variable" | "variables" | "query" => RedactionContext::Variable,
                    _ => context,
                };
                if lower == "value"
                    && matches!(
                        context,
                        RedactionContext::Header | RedactionContext::Variable
                    )
                {
                    *child = Value::String("<redacted>".to_owned());
                } else {
                    redact_request_node(child, next);
                }
            }
        }
        Value::Array(values) => {
            for child in values {
                redact_request_node(child, context);
            }
        }
        Value::Null | Value::Bool(_) | Value::Number(_) | Value::String(_) => {}
    }
}

pub(super) fn sanitize_url(raw: &str) -> Result<String, PostmanClientError> {
    let Ok(url) = Url::parse(raw) else {
        return Ok(if raw.contains("{{") {
            "<templated-url>".to_owned()
        } else {
            "<relative-or-invalid-url>".to_owned()
        });
    };
    let host = url.host_str().ok_or_else(invalid_response)?;
    let host = if host.contains(':') {
        format!("[{host}]")
    } else {
        host.to_owned()
    };
    let mut output = format!("{}://{host}", url.scheme());
    if let Some(port) = url.port() {
        output.push(':');
        output.push_str(&port.to_string());
    }
    let segment_count = url
        .path_segments()
        .map(|segments| segments.filter(|segment| !segment.is_empty()).count())
        .unwrap_or_default();
    if segment_count == 0 {
        output.push('/');
    } else {
        for _ in 0..segment_count {
            output.push_str("/<segment>");
        }
    }
    let query_keys = url
        .query_pairs()
        .map(|(key, _)| key.into_owned())
        .collect::<Vec<_>>();
    if !query_keys.is_empty() {
        output.push('?');
        for (index, key) in query_keys.iter().enumerate() {
            if index != 0 {
                output.push('&');
            }
            output.push_str(key);
            output.push_str("=<redacted>");
        }
    }
    if output.len() > MAX_TEXT_BYTES {
        return Err(resource_exhausted());
    }
    Ok(output)
}

#[async_trait]
impl PostmanApi for PostmanClient {
    async fn execute(&self, operation: PostmanOperation) -> Result<Value, PostmanClientError> {
        match operation {
            PostmanOperation::GetCollections => self.get_collections().await,
            PostmanOperation::GetCollection { collection_id } => {
                self.get_collection_flat(collection_id.as_deref()).await
            }
            PostmanOperation::GetFolder { folder_path } => self.get_folder_flat(&folder_path).await,
            PostmanOperation::GetRequestByPath { request_path } => {
                self.get_request_by_path(&request_path).await
            }
            PostmanOperation::GetRequestById { request_id } => {
                self.get_request_by_id(&request_id).await
            }
            PostmanOperation::GetRequestScript {
                request_path,
                script_type,
            } => self.get_request_script(&request_path, &script_type).await,
            PostmanOperation::SearchRequests {
                query,
                search_in,
                method,
            } => {
                self.search_requests(&query, &search_in, method.as_deref())
                    .await
            }
            PostmanOperation::Analyze {
                scope,
                target_path,
                include_improvements,
            } => {
                self.analyze(&scope, target_path.as_deref(), include_improvements)
                    .await
            }
            PostmanOperation::ExecuteRequest {
                request_path,
                override_variables,
            } => {
                self.execute_stored_request(&request_path, override_variables)
                    .await
            }
            operation => {
                let _guard = self.mutation_gate.lock().await;
                self.execute_management_effect(operation).await
            }
        }
    }
}

impl PostmanClient {
    async fn execute_management_effect(
        &self,
        operation: PostmanOperation,
    ) -> Result<Value, PostmanClientError> {
        match operation {
            operation @ (PostmanOperation::UpdateCollectionDescription { .. }
            | PostmanOperation::UpdateCollectionVariables { .. }
            | PostmanOperation::UpdateCollectionAuth { .. }
            | PostmanOperation::DeleteCollection { .. }
            | PostmanOperation::DuplicateCollection { .. }) => {
                self.execute_collection_effect(operation).await
            }
            operation @ (PostmanOperation::CreateFolder { .. }
            | PostmanOperation::UpdateFolder { .. }
            | PostmanOperation::DeleteFolder { .. }
            | PostmanOperation::MoveFolder { .. }) => self.execute_folder_effect(operation).await,
            operation @ (PostmanOperation::CreateRequest { .. }
            | PostmanOperation::DeleteRequest { .. }
            | PostmanOperation::DuplicateRequest { .. }
            | PostmanOperation::MoveRequest { .. }) => {
                self.execute_request_tree_effect(operation).await
            }
            operation @ (PostmanOperation::UpdateRequestName { .. }
            | PostmanOperation::UpdateRequestMethod { .. }
            | PostmanOperation::UpdateRequestUrl { .. }
            | PostmanOperation::UpdateRequestDescription { .. }
            | PostmanOperation::UpdateRequestHeaders { .. }
            | PostmanOperation::UpdateRequestBody { .. }
            | PostmanOperation::UpdateRequestAuth { .. }
            | PostmanOperation::UpdateRequestTests { .. }
            | PostmanOperation::UpdateRequestPreScript { .. }) => {
                self.execute_request_update_effect(operation).await
            }
            _ => Err(invalid_input()),
        }
    }

    async fn execute_collection_effect(
        &self,
        operation: PostmanOperation,
    ) -> Result<Value, PostmanClientError> {
        match operation {
            PostmanOperation::UpdateCollectionDescription {
                description,
                collection_id,
            } => {
                validate_text(&description, MAX_TEXT_BYTES, true)?;
                let collection_id = collection_id
                    .as_deref()
                    .unwrap_or(&self.management.collection_id);
                self.update_collection_field(
                    collection_id,
                    CollectionField::Description(description),
                )
                .await
            }
            PostmanOperation::UpdateCollectionVariables { variables } => {
                self.update_collection_field(
                    &self.management.collection_id,
                    CollectionField::Variables(variables),
                )
                .await
            }
            PostmanOperation::UpdateCollectionAuth { auth } => {
                self.update_collection_field(
                    &self.management.collection_id,
                    CollectionField::Auth(auth),
                )
                .await
            }
            PostmanOperation::DeleteCollection { collection_id } => {
                let collection_id = collection_id
                    .as_deref()
                    .unwrap_or(&self.management.collection_id);
                validate_text(collection_id, MAX_IDENTIFIER_BYTES, false)?;
                self.management
                    .request(
                        Method::DELETE,
                        &["collections", collection_id],
                        &[],
                        None,
                        true,
                        StatusCode::OK,
                    )
                    .await?;
                bounded_output(
                    json!({"message": format!("Collection {collection_id} deleted successfully")}),
                    true,
                )
            }
            PostmanOperation::DuplicateCollection { new_name } => {
                self.duplicate_collection(&new_name).await
            }
            _ => Err(invalid_input()),
        }
    }

    async fn execute_folder_effect(
        &self,
        operation: PostmanOperation,
    ) -> Result<Value, PostmanClientError> {
        match operation {
            PostmanOperation::CreateFolder {
                name,
                description,
                parent_path,
                auth,
            } => {
                self.create_folder(&name, description.as_deref(), parent_path.as_deref(), auth)
                    .await
            }
            PostmanOperation::UpdateFolder {
                folder_path,
                name,
                description,
                auth,
            } => {
                self.update_folder(&folder_path, name.as_deref(), description.as_deref(), auth)
                    .await
            }
            PostmanOperation::DeleteFolder { folder_path } => {
                self.remove_tree_item(&folder_path, super::collection::TreeKind::Folder)
                    .await?;
                bounded_output(
                    json!({"message": format!("Folder '{folder_path}' deleted successfully")}),
                    true,
                )
            }
            PostmanOperation::MoveFolder {
                source_path,
                target_path,
            } => {
                self.move_tree_item(
                    &source_path,
                    target_path.as_deref(),
                    super::collection::TreeKind::Folder,
                )
                .await?;
                bounded_output(
                    json!({"message": format!("Folder moved from '{source_path}' to '{}'", target_path.as_deref().unwrap_or("root"))}),
                    true,
                )
            }
            _ => Err(invalid_input()),
        }
    }

    async fn execute_request_tree_effect(
        &self,
        operation: PostmanOperation,
    ) -> Result<Value, PostmanClientError> {
        match operation {
            PostmanOperation::CreateRequest {
                folder_path,
                name,
                method,
                url,
                description,
                headers,
                body,
                auth,
                tests,
                pre_request_script,
            } => {
                self.create_request(
                    folder_path.as_deref(),
                    &name,
                    &method,
                    &url,
                    description.as_deref(),
                    headers,
                    body,
                    auth,
                    tests.as_deref(),
                    pre_request_script.as_deref(),
                )
                .await
            }
            PostmanOperation::DeleteRequest { request_path } => {
                self.remove_tree_item(&request_path, super::collection::TreeKind::Request)
                    .await?;
                bounded_output(
                    json!({"message": format!("Request '{request_path}' deleted successfully")}),
                    true,
                )
            }
            PostmanOperation::DuplicateRequest {
                source_path,
                new_name,
                target_path,
            } => {
                self.duplicate_request(&source_path, &new_name, target_path.as_deref())
                    .await
            }
            PostmanOperation::MoveRequest {
                source_path,
                target_path,
            } => {
                self.move_tree_item(
                    &source_path,
                    target_path.as_deref(),
                    super::collection::TreeKind::Request,
                )
                .await?;
                bounded_output(
                    json!({"message": format!("Request moved from '{source_path}' to '{}'", target_path.as_deref().unwrap_or("root"))}),
                    true,
                )
            }
            _ => Err(invalid_input()),
        }
    }

    async fn execute_request_update_effect(
        &self,
        operation: PostmanOperation,
    ) -> Result<Value, PostmanClientError> {
        match operation {
            PostmanOperation::UpdateRequestName { request_path, name } => {
                self.update_request_field(&request_path, "name", Value::String(name), "name")
                    .await
            }
            PostmanOperation::UpdateRequestMethod {
                request_path,
                method,
            } => {
                let method = validate_http_method(&method)?.as_str().to_owned();
                self.update_request_field(&request_path, "method", Value::String(method), "method")
                    .await
            }
            PostmanOperation::UpdateRequestUrl { request_path, url } => {
                validate_text(&url, MAX_TEXT_BYTES, false)?;
                self.update_request_field(&request_path, "url", Value::String(url), "URL")
                    .await
            }
            PostmanOperation::UpdateRequestDescription {
                request_path,
                description,
            } => {
                self.update_request_field(
                    &request_path,
                    "description",
                    Value::String(description),
                    "description",
                )
                .await
            }
            PostmanOperation::UpdateRequestHeaders {
                request_path,
                headers,
            } => {
                validate_text(&headers, MAX_TEXT_BYTES, true)?;
                self.update_request_field(
                    &request_path,
                    "headers",
                    Value::String(headers),
                    "headers",
                )
                .await
            }
            PostmanOperation::UpdateRequestBody { request_path, body } => {
                self.update_request_body(&request_path, body).await
            }
            PostmanOperation::UpdateRequestAuth { request_path, auth } => {
                self.update_request_field(
                    &request_path,
                    "auth",
                    auth.unwrap_or(Value::Null),
                    "auth",
                )
                .await
            }
            PostmanOperation::UpdateRequestTests {
                request_path,
                tests,
            } => {
                self.update_request_script(&request_path, &tests, "test", "tests")
                    .await
            }
            PostmanOperation::UpdateRequestPreScript {
                request_path,
                pre_request_script,
            } => {
                self.update_request_script(
                    &request_path,
                    &pre_request_script,
                    "prerequest",
                    "pre-script",
                )
                .await
            }
            _ => Err(invalid_input()),
        }
    }

    async fn update_collection_field(
        &self,
        collection_id: &str,
        field: CollectionField,
    ) -> Result<Value, PostmanClientError> {
        validate_text(collection_id, MAX_IDENTIFIER_BYTES, false)?;
        let mut current = self.get_collection(collection_id).await?;
        let collection = collection_data_mut(&mut current)?;
        match &field {
            CollectionField::Description(description) => {
                collection
                    .get_mut("info")
                    .and_then(Value::as_object_mut)
                    .ok_or_else(invalid_response)?
                    .insert("description".to_owned(), Value::String(description.clone()));
            }
            CollectionField::Variables(variables) => {
                collection.insert(
                    "variable".to_owned(),
                    variables.clone().map_or(Value::Null, Value::Array),
                );
            }
            CollectionField::Auth(auth) => {
                collection.insert("auth".to_owned(), auth.clone().unwrap_or(Value::Null));
            }
        }
        let intended = Value::Object(collection.clone());
        self.put_full_collection(collection_id, intended).await?;
        let confirmed = self
            .get_collection(collection_id)
            .await
            .map_err(|_| unknown_outcome())?;
        let confirmed = collection_data(&confirmed).map_err(|_| unknown_outcome())?;
        let info = confirmed
            .get("info")
            .and_then(Value::as_object)
            .ok_or_else(unknown_outcome)?;
        let common = json!({
            "id": info.get("_postman_id").cloned().unwrap_or(Value::Null),
            "name": info.get("name").cloned().unwrap_or(Value::Null)
        });
        let result = match field {
            CollectionField::Description(_) => json!({
                "collection": common,
                "description": info.get("description").cloned().unwrap_or(Value::Null)
            }),
            CollectionField::Variables(_) => json!({
                "collection": common,
                "variable": confirmed.get("variable").cloned().unwrap_or_else(|| Value::Array(Vec::new()))
            }),
            CollectionField::Auth(_) => json!({
                "collection": common,
                "auth": confirmed.get("auth").cloned().unwrap_or(Value::Null)
            }),
        };
        bounded_output(redact_collection_confirmation(result), true)
    }

    async fn put_full_collection(
        &self,
        collection_id: &str,
        intended: Value,
    ) -> Result<Value, PostmanClientError> {
        let body = json!({"collection": intended.clone()});
        match self
            .management
            .json(ManagementJsonCall {
                method: Method::PUT,
                segments: &["collections", collection_id],
                query: &[],
                body: Some(&body),
                effect: true,
                expected: StatusCode::OK,
                allow_empty: true,
            })
            .await
        {
            Ok(value) => Ok(value),
            Err(error) if error.code() == PostmanClientErrorCode::UnknownOutcome => {
                let current = self
                    .get_collection(collection_id)
                    .await
                    .map_err(|_| unknown_outcome())?;
                if current.get("collection") == Some(&intended) {
                    Ok(Value::Object(Map::new()))
                } else {
                    Err(unknown_outcome())
                }
            }
            Err(error) => Err(error),
        }
    }

    async fn duplicate_collection(&self, new_name: &str) -> Result<Value, PostmanClientError> {
        validate_text(new_name, MAX_IDENTIFIER_BYTES, false)?;
        let mut original = self.get_collection(&self.management.collection_id).await?;
        {
            let collection = collection_data_mut(&mut original)?;
            collection
                .get_mut("info")
                .and_then(Value::as_object_mut)
                .ok_or_else(invalid_response)?
                .insert("name".to_owned(), Value::String(new_name.to_owned()));
        }
        super::collection::strip_ids(&mut original)?;
        let body = json!({"collection": collection_data(&original)?.clone()});
        let result = self
            .management
            .json(ManagementJsonCall {
                method: Method::POST,
                segments: &["collections"],
                query: &[("workspace", &self.management.workspace_id)],
                body: Some(&body),
                effect: true,
                expected: StatusCode::OK,
                allow_empty: false,
            })
            .await?;
        bounded_output(redact_sensitive_fields(result), true)
    }

    async fn create_folder(
        &self,
        name: &str,
        description: Option<&str>,
        parent_path: Option<&str>,
        auth: Option<Value>,
    ) -> Result<Value, PostmanClientError> {
        validate_text(name, MAX_IDENTIFIER_BYTES, false)?;
        if let Some(value) = description {
            validate_text(value, MAX_TEXT_BYTES, true)?;
        }
        let mut current = self.get_collection(&self.management.collection_id).await?;
        let collection = collection_data_mut(&mut current)?;
        let parent = if let Some(path) = parent_path {
            validate_path(path)?;
            Some(
                super::collection::resolve_indices(
                    collection,
                    path,
                    super::collection::TreeKind::Folder,
                )?
                .ok_or_else(not_found)?,
            )
        } else {
            None
        };
        let mut folder = Map::from_iter([
            ("name".to_owned(), Value::String(name.to_owned())),
            ("item".to_owned(), Value::Array(Vec::new())),
        ]);
        if let Some(description) = description.filter(|value| !value.is_empty()) {
            folder.insert(
                "description".to_owned(),
                Value::String(description.to_owned()),
            );
        }
        if let Some(auth) = auth {
            folder.insert("auth".to_owned(), auth);
        }
        super::collection::append_at(collection, parent.as_deref(), Value::Object(folder))?;
        self.put_full_collection(
            &self.management.collection_id,
            Value::Object(collection.clone()),
        )
        .await?;
        bounded_output(
            json!({"message": format!("Folder '{name}' created successfully")}),
            true,
        )
    }

    async fn update_folder(
        &self,
        path: &str,
        name: Option<&str>,
        description: Option<&str>,
        auth: Option<Value>,
    ) -> Result<Value, PostmanClientError> {
        validate_path(path)?;
        if let Some(value) = name {
            validate_text(value, MAX_IDENTIFIER_BYTES, false)?;
        }
        if let Some(value) = description {
            validate_text(value, MAX_TEXT_BYTES, true)?;
        }
        let collection = self.get_collection(&self.management.collection_id).await?;
        let folder = find_folder(collection_data(&collection)?, path)?.ok_or_else(not_found)?;
        let id = item_id(folder)?.to_owned();
        let mut update = Map::new();
        if let Some(name) = name {
            update.insert("name".to_owned(), Value::String(name.to_owned()));
        }
        if let Some(description) = description {
            update.insert(
                "description".to_owned(),
                Value::String(description.to_owned()),
            );
        }
        if let Some(auth) = auth {
            update.insert("auth".to_owned(), auth);
        }
        if update.is_empty() {
            return bounded_output(
                json!({"success": true, "message": format!("No changes requested for folder '{path}'")}),
                true,
            );
        }
        self.management
            .request(
                Method::PUT,
                &[
                    "collections",
                    &self.management.collection_id,
                    "folders",
                    &id,
                ],
                &[],
                Some(&Value::Object(update)),
                true,
                StatusCode::OK,
            )
            .await?;
        bounded_output(
            json!({"success": true, "message": format!("Folder '{path}' updated successfully")}),
            true,
        )
    }

    async fn remove_tree_item(
        &self,
        path: &str,
        kind: super::collection::TreeKind,
    ) -> Result<(), PostmanClientError> {
        validate_path(path)?;
        let mut current = self.get_collection(&self.management.collection_id).await?;
        let collection = collection_data_mut(&mut current)?;
        let indexes =
            super::collection::resolve_indices(collection, path, kind)?.ok_or_else(not_found)?;
        super::collection::remove_at(collection, &indexes)?;
        self.put_full_collection(
            &self.management.collection_id,
            Value::Object(collection.clone()),
        )
        .await?;
        Ok(())
    }

    async fn move_tree_item(
        &self,
        source: &str,
        target: Option<&str>,
        kind: super::collection::TreeKind,
    ) -> Result<(), PostmanClientError> {
        validate_path(source)?;
        if let Some(target) = target {
            validate_path(target)?;
            if source.eq_ignore_ascii_case(target)
                || target
                    .get(..source.len())
                    .is_some_and(|prefix| prefix.eq_ignore_ascii_case(source))
                    && target.as_bytes().get(source.len()) == Some(&b'/')
            {
                return Err(invalid_input());
            }
        }
        let mut current = self.get_collection(&self.management.collection_id).await?;
        let collection = collection_data_mut(&mut current)?;
        let source_indexes =
            super::collection::resolve_indices(collection, source, kind)?.ok_or_else(not_found)?;
        let item = super::collection::remove_at(collection, &source_indexes)?;
        let target_indexes = if let Some(target) = target {
            Some(
                super::collection::resolve_indices(
                    collection,
                    target,
                    super::collection::TreeKind::Folder,
                )?
                .ok_or_else(not_found)?,
            )
        } else {
            None
        };
        super::collection::append_at(collection, target_indexes.as_deref(), item)?;
        self.put_full_collection(
            &self.management.collection_id,
            Value::Object(collection.clone()),
        )
        .await?;
        Ok(())
    }

    #[allow(clippy::too_many_arguments)]
    async fn create_request(
        &self,
        folder_path: Option<&str>,
        name: &str,
        method: &str,
        url: &str,
        description: Option<&str>,
        headers: Option<Vec<Value>>,
        body: Option<Value>,
        auth: Option<Value>,
        tests: Option<&str>,
        pre_request_script: Option<&str>,
    ) -> Result<Value, PostmanClientError> {
        validate_text(name, MAX_IDENTIFIER_BYTES, false)?;
        let method = validate_http_method(method)?.as_str().to_owned();
        validate_text(url, MAX_TEXT_BYTES, false)?;
        for text in [description, tests, pre_request_script]
            .into_iter()
            .flatten()
        {
            validate_text(text, MAX_TEXT_BYTES, true)?;
        }
        let mut current = self.get_collection(&self.management.collection_id).await?;
        let collection = collection_data_mut(&mut current)?;
        let parent = if let Some(path) = folder_path {
            validate_path(path)?;
            Some(
                super::collection::resolve_indices(
                    collection,
                    path,
                    super::collection::TreeKind::Folder,
                )?
                .ok_or_else(not_found)?,
            )
        } else {
            None
        };
        let mut request = Map::from_iter([
            ("method".to_owned(), Value::String(method)),
            (
                "header".to_owned(),
                headers.map_or_else(|| Value::Array(Vec::new()), Value::Array),
            ),
            ("url".to_owned(), Value::String(url.to_owned())),
        ]);
        if let Some(description) = description.filter(|value| !value.is_empty()) {
            request.insert(
                "description".to_owned(),
                Value::String(description.to_owned()),
            );
        }
        if let Some(body) = body {
            request.insert("body".to_owned(), body);
        }
        if let Some(auth) = auth {
            request.insert("auth".to_owned(), auth);
        }
        let mut item = Map::from_iter([
            ("name".to_owned(), Value::String(name.to_owned())),
            ("request".to_owned(), Value::Object(request)),
        ]);
        let mut events = Vec::new();
        if let Some(script) = pre_request_script.filter(|value| !value.is_empty()) {
            events.push(script_event("prerequest", script));
        }
        if let Some(script) = tests.filter(|value| !value.is_empty()) {
            events.push(script_event("test", script));
        }
        if !events.is_empty() {
            item.insert("event".to_owned(), Value::Array(events));
        }
        super::collection::append_at(collection, parent.as_deref(), Value::Object(item))?;
        self.put_full_collection(
            &self.management.collection_id,
            Value::Object(collection.clone()),
        )
        .await?;
        let confirmed = self
            .get_collection(&self.management.collection_id)
            .await
            .map_err(|_| unknown_outcome())?;
        let path = folder_path.map_or_else(|| name.to_owned(), |folder| format!("{folder}/{name}"));
        let created = find_request(
            collection_data(&confirmed).map_err(|_| unknown_outcome())?,
            &path,
        )
        .map_err(|_| unknown_outcome())?
        .ok_or_else(unknown_outcome)?;
        let mut result = Map::from_iter([(
            "message".to_owned(),
            Value::String(format!("Request '{name}' created successfully")),
        )]);
        if let Some(id) = created.get("id") {
            result.insert("id".to_owned(), id.clone());
        }
        if let Some(uid) = created.get("uid") {
            result.insert("uid".to_owned(), uid.clone());
        }
        bounded_output(Value::Object(result), true)
    }

    async fn resolve_request_for_update(
        &self,
        path: &str,
    ) -> Result<(Value, String), PostmanClientError> {
        validate_path(path)?;
        let collection = self.get_collection(&self.management.collection_id).await?;
        let request = find_request(collection_data(&collection)?, path)?.ok_or_else(not_found)?;
        Ok((request.clone(), item_id(request)?.to_owned()))
    }

    async fn update_request_field(
        &self,
        path: &str,
        field: &str,
        value: Value,
        label: &str,
    ) -> Result<Value, PostmanClientError> {
        let (_, id) = self.resolve_request_for_update(path).await?;
        self.management
            .request(
                Method::PUT,
                &[
                    "collections",
                    &self.management.collection_id,
                    "requests",
                    &id,
                ],
                &[],
                Some(&json!({(field): value})),
                true,
                StatusCode::OK,
            )
            .await?;
        bounded_output(
            json!({"success": true, "message": format!("Request '{path}' {label} updated successfully")}),
            true,
        )
    }

    async fn update_request_body(
        &self,
        path: &str,
        body: Value,
    ) -> Result<Value, PostmanClientError> {
        let body = body.as_object().ok_or_else(invalid_input)?;
        let mode = body.get("mode").and_then(Value::as_str).unwrap_or("raw");
        let update = match mode {
            "raw" => {
                let raw = match body.get("raw") {
                    Some(Value::String(value)) => value.clone(),
                    Some(value) => serde_json::to_string(value).map_err(|_| invalid_input())?,
                    None => String::new(),
                };
                let language = body
                    .get("options")
                    .and_then(|value| value.get("raw"))
                    .and_then(|value| value.get("language"))
                    .and_then(Value::as_str)
                    .unwrap_or("json");
                json!({"dataMode":"raw", "rawModeData":raw, "dataOptions":{"raw":{"language":language}}})
            }
            "urlencoded" => json!({
                "dataMode":"urlencoded",
                "data": body.get("urlencoded").cloned().unwrap_or_else(|| Value::Array(Vec::new()))
            }),
            "formdata" => json!({
                "dataMode":"formdata",
                "data": body.get("formdata").cloned().unwrap_or_else(|| Value::Array(Vec::new()))
            }),
            _ => return Err(invalid_input()),
        };
        let (_, id) = self.resolve_request_for_update(path).await?;
        let response = self
            .management
            .json(ManagementJsonCall {
                method: Method::PUT,
                segments: &[
                    "collections",
                    &self.management.collection_id,
                    "requests",
                    &id,
                ],
                query: &[],
                body: Some(&update),
                effect: true,
                expected: StatusCode::OK,
                allow_empty: false,
            })
            .await?;
        if response
            .get("meta")
            .and_then(|value| value.get("action"))
            .and_then(Value::as_str)
            != Some("update")
            || !response.get("data").is_some_and(nonempty_json)
        {
            return Err(unknown_outcome());
        }
        bounded_output(
            json!({"success": true, "message": format!("Request '{path}' body updated successfully")}),
            true,
        )
    }

    async fn update_request_script(
        &self,
        path: &str,
        script: &str,
        kind: &str,
        label: &str,
    ) -> Result<Value, PostmanClientError> {
        validate_text(script, MAX_TEXT_BYTES, true)?;
        let (request, id) = self.resolve_request_for_update(path).await?;
        let mut events = request
            .get("event")
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default();
        events.retain(|event| event.get("listen").and_then(Value::as_str) != Some(kind));
        events.push(script_event(kind, script.trim()));
        self.management
            .request(
                Method::PUT,
                &[
                    "collections",
                    &self.management.collection_id,
                    "requests",
                    &id,
                ],
                &[],
                Some(&json!({"events": events})),
                true,
                StatusCode::OK,
            )
            .await?;
        bounded_output(
            json!({"success": true, "message": format!("Request '{path}' {label} updated successfully")}),
            true,
        )
    }

    async fn duplicate_request(
        &self,
        source: &str,
        new_name: &str,
        target: Option<&str>,
    ) -> Result<Value, PostmanClientError> {
        validate_path(source)?;
        validate_text(new_name, MAX_IDENTIFIER_BYTES, false)?;
        let mut current = self.get_collection(&self.management.collection_id).await?;
        let collection = collection_data_mut(&mut current)?;
        let source_indexes = super::collection::resolve_indices(
            collection,
            source,
            super::collection::TreeKind::Request,
        )?
        .ok_or_else(not_found)?;
        let mut copy = super::collection::item_at(collection, &source_indexes)?.clone();
        super::collection::strip_ids(&mut copy)?;
        copy.as_object_mut()
            .ok_or_else(invalid_response)?
            .insert("name".to_owned(), Value::String(new_name.to_owned()));
        let default_parent = source.rsplit_once('/').map(|(parent, _)| parent);
        let parent = target.or(default_parent);
        let parent_indexes = if let Some(path) = parent {
            Some(
                super::collection::resolve_indices(
                    collection,
                    path,
                    super::collection::TreeKind::Folder,
                )?
                .ok_or_else(not_found)?,
            )
        } else {
            None
        };
        super::collection::append_at(collection, parent_indexes.as_deref(), copy)?;
        self.put_full_collection(
            &self.management.collection_id,
            Value::Object(collection.clone()),
        )
        .await?;
        bounded_output(
            json!({"message": format!("Request duplicated as '{new_name}'")}),
            true,
        )
    }

    async fn execute_stored_request(
        &self,
        request_path: &str,
        override_variables: Map<String, Value>,
    ) -> Result<Value, PostmanClientError> {
        validate_path(request_path)?;
        let authority = self.dynamic.authority.as_ref().ok_or_else(authorization)?;
        let prepared = self
            .prepare_dynamic_request(request_path, &override_variables)
            .await?;
        let PreparedDynamicRequest {
            request,
            method,
            safe_url,
        } = prepared;
        let response = authority
            .dispatch(request)
            .await
            .map_err(|_| unknown_outcome())?;
        project_dynamic_response(request_path, &method, &safe_url, response)
    }

    async fn prepare_dynamic_request(
        &self,
        request_path: &str,
        override_variables: &Map<String, Value>,
    ) -> Result<PreparedDynamicRequest, PostmanClientError> {
        let mut profile = SensitiveJson(
            serde_json::from_str(&self.dynamic.profile.canonical_json)
                .map_err(|_| invalid_configuration())?,
        );
        let profile = profile
            .0
            .as_object_mut()
            .ok_or_else(invalid_configuration)?;
        let collection_response = self.get_collection(&self.management.collection_id).await?;
        let collection = collection_data(&collection_response)?;
        let request_item = find_request(collection, request_path)?.ok_or_else(not_found)?;
        let request = request_item
            .get("request")
            .and_then(Value::as_object)
            .ok_or_else(invalid_response)?;
        let variables =
            SensitiveVariables(collect_variables(profile, collection, override_variables)?);
        let method = validate_http_method(
            request
                .get("method")
                .and_then(Value::as_str)
                .unwrap_or("GET"),
        )?;
        let (raw_url, mut query) = dynamic_url(request, &variables.0)?;
        let url_text = expand_variables(raw_url, &variables.0)?;
        let url = Url::parse(&url_text).map_err(|_| invalid_input())?;
        if url.scheme() != "https"
            || url.host_str().is_none()
            || !url.username().is_empty()
            || url.password().is_some()
            || url.fragment().is_some()
        {
            return Err(invalid_input());
        }
        for (key, value) in &mut query {
            *key = expand_variables(key, &variables.0)?;
            *value = expand_variables(value, &variables.0)?;
            validate_text(key, MAX_IDENTIFIER_BYTES, false)?;
            validate_text(value, MAX_TEXT_BYTES, true)?;
        }
        let native_auth = request.get("auth");
        let auth = profile.get("auth").or(native_auth);
        let mut headers = Vec::new();
        apply_dynamic_auth(auth, &variables.0, &mut headers, &mut query)?;
        if let Some(stored) = request.get("header").and_then(Value::as_array) {
            if stored.len() > MAX_ITEMS {
                return Err(resource_exhausted());
            }
            for header in stored {
                let header = header.as_object().ok_or_else(invalid_response)?;
                if header.get("disabled").and_then(Value::as_bool) == Some(true) {
                    continue;
                }
                let name = expand_variables(
                    header
                        .get("key")
                        .and_then(Value::as_str)
                        .ok_or_else(invalid_response)?,
                    &variables.0,
                )?;
                let value = expand_variables(
                    header
                        .get("value")
                        .and_then(Value::as_str)
                        .unwrap_or_default(),
                    &variables.0,
                )?;
                insert_dynamic_header(&mut headers, &name, &value)?;
            }
        }
        let body = dynamic_body(request.get("body"), &method, &variables.0, &mut headers)?;
        let request = DynamicRequest {
            method: method.clone(),
            url,
            headers,
            query,
            body,
        };
        Ok(PreparedDynamicRequest {
            request,
            method: Box::from(method.as_str()),
            safe_url: sanitize_url(&url_text)?,
        })
    }
}

fn project_dynamic_response(
    request_path: &str,
    method: &str,
    safe_url: &str,
    response: DynamicResponse,
) -> Result<Value, PostmanClientError> {
    let DynamicResponse {
        status,
        reason,
        headers,
        body,
        size_bytes,
    } = response;
    if size_bytes > MAX_RESPONSE_BYTES {
        return Err(unknown_outcome());
    }
    validate_json(&body).map_err(|_| unknown_outcome())?;
    let header_names = headers
        .keys()
        .take(MAX_ITEMS + 1)
        .cloned()
        .map(Value::String)
        .collect::<Vec<_>>();
    if header_names.len() > MAX_ITEMS {
        return Err(unknown_outcome());
    }
    bounded_output(
        json!({
            "request": {"path":request_path,"method":method,"url":safe_url},
            "response": {
                "status_code":status.as_u16(),
                "status_text":reason,
                "header_names":header_names,
                "size_bytes":size_bytes,
                "body":body
            },
            "success":status.is_success()
        }),
        true,
    )
}

enum CollectionField {
    Description(String),
    Variables(Option<Vec<Value>>),
    Auth(Option<Value>),
}

fn script_event(kind: &str, script: &str) -> Value {
    json!({
        "listen": kind,
        "script": {
            "exec": script.split('\n').collect::<Vec<_>>(),
            "type": "text/javascript"
        }
    })
}

fn redact_collection_confirmation(mut value: Value) -> Value {
    if let Some(variables) = value.get_mut("variable") {
        *variables = redact_variables(Some(variables));
    }
    if value.get("auth").is_some() {
        value["auth"] = Value::String("<redacted>".to_owned());
    }
    value
}

fn nonempty_json(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(value) => *value,
        Value::String(value) => !value.is_empty(),
        Value::Array(value) => !value.is_empty(),
        Value::Object(value) => !value.is_empty(),
        Value::Number(_) => true,
    }
}

struct SensitiveJson(Value);

impl Drop for SensitiveJson {
    fn drop(&mut self) {
        let mut stack = vec![&mut self.0];
        while let Some(value) = stack.pop() {
            match value {
                Value::String(text) => text.zeroize(),
                Value::Array(values) => stack.extend(values.iter_mut()),
                Value::Object(values) => stack.extend(values.values_mut()),
                Value::Null | Value::Bool(_) | Value::Number(_) => {}
            }
        }
    }
}

struct SensitiveVariables(BTreeMap<String, String>);

impl Drop for SensitiveVariables {
    fn drop(&mut self) {
        for (mut key, mut value) in std::mem::take(&mut self.0) {
            key.zeroize();
            value.zeroize();
        }
    }
}

fn collect_variables(
    profile: &Map<String, Value>,
    collection: &Map<String, Value>,
    overrides: &Map<String, Value>,
) -> Result<BTreeMap<String, String>, PostmanClientError> {
    let mut variables = BTreeMap::new();
    if let Some(values) = profile.get("values").and_then(Value::as_array) {
        insert_variables(values, &mut variables)?;
    }
    if let Some(values) = collection.get("variable").and_then(Value::as_array) {
        insert_variables(values, &mut variables)?;
    }
    if overrides.len() > MAX_ITEMS {
        return Err(resource_exhausted());
    }
    for (key, value) in overrides {
        validate_text(key, MAX_IDENTIFIER_BYTES, false)?;
        let value = scalar_text(value)?;
        variables.insert(key.clone(), value);
    }
    Ok(variables)
}

fn insert_variables(
    values: &[Value],
    output: &mut BTreeMap<String, String>,
) -> Result<(), PostmanClientError> {
    if values.len() > MAX_ITEMS {
        return Err(resource_exhausted());
    }
    for value in values {
        let value = value.as_object().ok_or_else(invalid_response)?;
        if value.get("enabled").and_then(Value::as_bool) == Some(false) {
            continue;
        }
        let Some(key) = value.get("key").and_then(Value::as_str) else {
            continue;
        };
        let Some(raw) = value.get("value") else {
            continue;
        };
        validate_text(key, MAX_IDENTIFIER_BYTES, false)?;
        output.insert(key.to_owned(), scalar_text(raw)?);
    }
    Ok(())
}

fn scalar_text(value: &Value) -> Result<String, PostmanClientError> {
    let text = match value {
        Value::String(value) => value.clone(),
        Value::Null => "None".to_owned(),
        Value::Bool(value) => value.to_string(),
        Value::Number(value) => value.to_string(),
        Value::Array(_) | Value::Object(_) => return Err(invalid_input()),
    };
    validate_text(&text, MAX_TEXT_BYTES, true)?;
    Ok(text)
}

fn expand_variables(
    input: &str,
    variables: &BTreeMap<String, String>,
) -> Result<String, PostmanClientError> {
    validate_text(input, MAX_TEXT_BYTES, true)?;
    let mut output = input.to_owned();
    for _ in 0..8 {
        let mut next = String::with_capacity(output.len());
        let mut rest = output.as_str();
        let mut changed = false;
        while let Some(start) = rest.find("{{") {
            next.push_str(&rest[..start]);
            let tail = &rest[start + 2..];
            let Some(end) = tail.find("}}") else {
                return Err(invalid_input());
            };
            let key = tail[..end].trim();
            validate_text(key, MAX_IDENTIFIER_BYTES, false)?;
            let Some(value) = variables.get(key) else {
                return Err(invalid_input());
            };
            next.push_str(value);
            if next.len() > MAX_TEXT_BYTES {
                return Err(resource_exhausted());
            }
            rest = &tail[end + 2..];
            changed = true;
        }
        next.push_str(rest);
        if next.len() > MAX_TEXT_BYTES {
            return Err(resource_exhausted());
        }
        output = next;
        if !changed || !output.contains("{{") {
            return Ok(output);
        }
    }
    Err(invalid_input())
}

fn dynamic_url<'a>(
    request: &'a Map<String, Value>,
    variables: &BTreeMap<String, String>,
) -> Result<(&'a str, DynamicQueryPairs), PostmanClientError> {
    match request.get("url") {
        Some(Value::String(value)) => Ok((value, Vec::new())),
        Some(Value::Object(value)) => {
            let raw = value
                .get("raw")
                .and_then(Value::as_str)
                .ok_or_else(invalid_response)?;
            let mut query = Vec::new();
            if let Some(values) = value.get("query").and_then(Value::as_array) {
                if values.len() > MAX_ITEMS {
                    return Err(resource_exhausted());
                }
                for pair in values {
                    let pair = pair.as_object().ok_or_else(invalid_response)?;
                    if pair.get("disabled").and_then(Value::as_bool) == Some(true) {
                        continue;
                    }
                    let key = pair
                        .get("key")
                        .and_then(Value::as_str)
                        .ok_or_else(invalid_response)?;
                    let value = pair
                        .get("value")
                        .and_then(Value::as_str)
                        .unwrap_or_default();
                    query.push((
                        expand_variables(key, variables)?,
                        expand_variables(value, variables)?,
                    ));
                }
            }
            Ok((raw, query))
        }
        None | Some(_) => Err(invalid_response()),
    }
}

fn apply_dynamic_auth(
    auth: Option<&Value>,
    variables: &BTreeMap<String, String>,
    headers: &mut Vec<(HeaderName, HeaderValue)>,
    query: &mut Vec<(String, String)>,
) -> Result<(), PostmanClientError> {
    let Some(auth) = auth.and_then(Value::as_object) else {
        return Ok(());
    };
    let kind = auth
        .get("type")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_ascii_lowercase();
    let params = auth.get("params").and_then(Value::as_object);
    match kind.as_str() {
        "" | "noauth" => {}
        "bearer" | "oauth2" => {
            let token = if kind == "oauth2" {
                params
                    .and_then(|values| values.get("access_token"))
                    .and_then(Value::as_str)
            } else {
                auth_array_value(auth, "bearer", "token").or_else(|| {
                    params
                        .and_then(|values| values.get("token"))
                        .and_then(Value::as_str)
                })
            }
            .ok_or_else(invalid_input)?;
            let token = expand_variables(token, variables)?;
            insert_dynamic_header(headers, "authorization", &format!("Bearer {token}"))?;
        }
        "basic" => {
            let username = params
                .and_then(|values| values.get("username"))
                .and_then(Value::as_str)
                .or_else(|| auth_array_value(auth, "basic", "username"))
                .ok_or_else(invalid_input)?;
            let password = params
                .and_then(|values| values.get("password"))
                .and_then(Value::as_str)
                .or_else(|| auth_array_value(auth, "basic", "password"))
                .ok_or_else(invalid_input)?;
            let credentials = format!(
                "{}:{}",
                expand_variables(username, variables)?,
                expand_variables(password, variables)?
            );
            let encoded = base64::engine::general_purpose::STANDARD.encode(credentials.as_bytes());
            insert_dynamic_header(headers, "authorization", &format!("Basic {encoded}"))?;
        }
        "api_key" | "apikey" => {
            let key = params
                .and_then(|values| values.get("key"))
                .and_then(Value::as_str)
                .or_else(|| auth_array_value(auth, "apikey", "key"))
                .ok_or_else(invalid_input)?;
            let value = params
                .and_then(|values| values.get("value"))
                .and_then(Value::as_str)
                .or_else(|| auth_array_value(auth, "apikey", "value"))
                .ok_or_else(invalid_input)?;
            let location = params
                .and_then(|values| values.get("in"))
                .and_then(Value::as_str)
                .unwrap_or("header");
            let key = expand_variables(key, variables)?;
            let value = expand_variables(value, variables)?;
            match location.to_ascii_lowercase().as_str() {
                "header" => insert_dynamic_header(headers, &key, &value)?,
                "query" => query.push((key, value)),
                "cookie" => append_dynamic_cookie(headers, &key, &value)?,
                _ => return Err(invalid_input()),
            }
        }
        "custom" => {
            let params = params.ok_or_else(invalid_input)?;
            for (name, target) in [
                ("headers", "header"),
                ("query", "query"),
                ("cookies", "cookie"),
            ] {
                let Some(values) = params.get(name).and_then(Value::as_object) else {
                    continue;
                };
                for (key, value) in values {
                    let value = value.as_str().ok_or_else(invalid_input)?;
                    let key = expand_variables(key, variables)?;
                    let value = expand_variables(value, variables)?;
                    match target {
                        "header" => insert_dynamic_header(headers, &key, &value)?,
                        "query" => query.push((key, value)),
                        "cookie" => append_dynamic_cookie(headers, &key, &value)?,
                        _ => return Err(invalid_input()),
                    }
                }
            }
        }
        _ => return Err(invalid_input()),
    }
    Ok(())
}

fn auth_array_value<'a>(auth: &'a Map<String, Value>, field: &str, key: &str) -> Option<&'a str> {
    auth.get(field)
        .and_then(Value::as_array)?
        .iter()
        .find(|value| value.get("key").and_then(Value::as_str) == Some(key))?
        .get("value")?
        .as_str()
}

fn insert_dynamic_header(
    headers: &mut Vec<(HeaderName, HeaderValue)>,
    name: &str,
    value: &str,
) -> Result<(), PostmanClientError> {
    let name = HeaderName::from_bytes(name.as_bytes()).map_err(|_| invalid_input())?;
    if matches!(
        name.as_str(),
        "host"
            | "content-length"
            | "transfer-encoding"
            | "connection"
            | "upgrade"
            | "forwarded"
            | "via"
    ) || name.as_str().starts_with("proxy-")
        || name.as_str().starts_with("x-forwarded-")
    {
        return Err(invalid_input());
    }
    let value = HeaderValue::from_str(value).map_err(|_| invalid_input())?;
    let replacing = headers.iter().position(|(existing, _)| existing == name);
    if replacing.is_none() && headers.len() >= MAX_DYNAMIC_HEADERS {
        return Err(resource_exhausted());
    }
    let replaced_bytes = replacing.map_or(0, |index| {
        headers[index].0.as_str().len() + headers[index].1.as_bytes().len()
    });
    let current_bytes = headers.iter().try_fold(0usize, |total, (name, value)| {
        total
            .checked_add(name.as_str().len())
            .and_then(|total| total.checked_add(value.as_bytes().len()))
            .ok_or_else(resource_exhausted)
    })?;
    let next_bytes = current_bytes
        .checked_sub(replaced_bytes)
        .and_then(|total| total.checked_add(name.as_str().len()))
        .and_then(|total| total.checked_add(value.as_bytes().len()))
        .ok_or_else(resource_exhausted)?;
    if next_bytes > MAX_DYNAMIC_HEADER_BYTES {
        return Err(resource_exhausted());
    }
    if let Some(index) = replacing {
        headers.remove(index);
    }
    headers.push((name, value));
    Ok(())
}

fn append_dynamic_cookie(
    headers: &mut Vec<(HeaderName, HeaderValue)>,
    key: &str,
    value: &str,
) -> Result<(), PostmanClientError> {
    if key.trim().is_empty()
        || key.contains([';', '=', '\r', '\n'])
        || value.contains([';', '\r', '\n'])
    {
        return Err(invalid_input());
    }
    let existing = headers
        .iter()
        .find(|(name, _)| name.as_str() == "cookie")
        .map(|(_, value)| value.to_str().map(str::to_owned))
        .transpose()
        .map_err(|_| invalid_input())?;
    let next = existing.map_or_else(
        || format!("{key}={value}"),
        |existing| format!("{existing}; {key}={value}"),
    );
    insert_dynamic_header(headers, "cookie", &next)
}

fn dynamic_body(
    body: Option<&Value>,
    method: &Method,
    variables: &BTreeMap<String, String>,
    headers: &mut Vec<(HeaderName, HeaderValue)>,
) -> Result<Option<Vec<u8>>, PostmanClientError> {
    let Some(body) = body.and_then(Value::as_object) else {
        return Ok(None);
    };
    if !matches!(*method, Method::POST | Method::PUT | Method::PATCH) {
        return Ok(None);
    }
    let mode = body.get("mode").and_then(Value::as_str).unwrap_or_default();
    let bytes = match mode {
        "" => return Ok(None),
        "raw" => {
            let expanded = expand_variables(
                body.get("raw").and_then(Value::as_str).unwrap_or_default(),
                variables,
            )?;
            match normalized_json_body(&expanded)? {
                Some(bytes) => {
                    if !headers.iter().any(|(name, _)| name == CONTENT_TYPE) {
                        insert_dynamic_header(headers, "content-type", "application/json")?;
                    }
                    bytes
                }
                None => expanded.into_bytes(),
            }
        }
        "formdata" | "urlencoded" => {
            let values = body
                .get(mode)
                .and_then(Value::as_array)
                .ok_or_else(invalid_response)?;
            if values.len() > MAX_ITEMS {
                return Err(resource_exhausted());
            }
            let mut encoded =
                Url::parse("https://form.invalid/").map_err(|_| invalid_configuration())?;
            {
                let mut pairs = encoded.query_pairs_mut();
                for value in values {
                    let value = value.as_object().ok_or_else(invalid_response)?;
                    if value.get("disabled").and_then(Value::as_bool) == Some(true) {
                        continue;
                    }
                    pairs.append_pair(
                        &expand_variables(
                            value
                                .get("key")
                                .and_then(Value::as_str)
                                .ok_or_else(invalid_response)?,
                            variables,
                        )?,
                        &expand_variables(
                            value
                                .get("value")
                                .and_then(Value::as_str)
                                .unwrap_or_default(),
                            variables,
                        )?,
                    );
                }
            }
            if !headers.iter().any(|(name, _)| name == CONTENT_TYPE) {
                insert_dynamic_header(
                    headers,
                    "content-type",
                    "application/x-www-form-urlencoded",
                )?;
            }
            encoded.query().unwrap_or_default().as_bytes().to_vec()
        }
        _ => return Err(invalid_input()),
    };
    if bytes.len() > MAX_REQUEST_BYTES {
        return Err(resource_exhausted());
    }
    Ok(Some(bytes))
}

/// Parses ordinary JSON or Postman's JSON-with-line-comments convention.
///
/// The SDK removes `//` comments before parsing raw JSON. This scanner preserves
/// that behavior without corrupting `//` inside quoted strings such as URLs.
fn normalized_json_body(input: &str) -> Result<Option<Vec<u8>>, PostmanClientError> {
    let parsed = if let Ok(value) = serde_json::from_str::<Value>(input) {
        Some(value)
    } else {
        let (cleaned, changed) = strip_json_line_comments(input);
        changed
            .then(|| serde_json::from_slice::<Value>(&cleaned).ok())
            .flatten()
    };
    let Some(value) = parsed else {
        return Ok(None);
    };
    validate_json(&value)?;
    let encoded = serde_json::to_vec(&value).map_err(|_| invalid_input())?;
    if encoded.len() > MAX_TEXT_BYTES {
        return Err(resource_exhausted());
    }
    Ok(Some(encoded))
}

fn strip_json_line_comments(input: &str) -> (Vec<u8>, bool) {
    let bytes = input.as_bytes();
    let mut output = Vec::with_capacity(bytes.len());
    let mut index = 0usize;
    let mut in_string = false;
    let mut escaped = false;
    let mut changed = false;

    while index < bytes.len() {
        let byte = bytes[index];
        if in_string {
            output.push(byte);
            if escaped {
                escaped = false;
            } else if byte == b'\\' {
                escaped = true;
            } else if byte == b'"' {
                in_string = false;
            }
            index += 1;
            continue;
        }
        if byte == b'"' {
            in_string = true;
            output.push(byte);
            index += 1;
            continue;
        }
        if byte == b'/' && bytes.get(index + 1) == Some(&b'/') {
            changed = true;
            index += 2;
            while index < bytes.len() && !matches!(bytes[index], b'\n' | b'\r') {
                index += 1;
            }
            continue;
        }
        output.push(byte);
        index += 1;
    }
    (output, changed)
}

impl ManagementAuthority {
    fn url(&self, segments: &[&str], query: &[(&str, &str)]) -> Result<Url, PostmanClientError> {
        let mut url = self.base_url.clone();
        {
            let mut path = url
                .path_segments_mut()
                .map_err(|()| invalid_configuration())?;
            path.clear();
            path.extend(segments);
        }
        if !query.is_empty() {
            let mut pairs = url.query_pairs_mut();
            for (name, value) in query {
                pairs.append_pair(name, value);
            }
        }
        Ok(url)
    }

    async fn request(
        &self,
        method: Method,
        segments: &[&str],
        query: &[(&str, &str)],
        body: Option<&Value>,
        effect: bool,
        expected: StatusCode,
    ) -> Result<Vec<u8>, PostmanClientError> {
        let request = self.build_request(method, segments, query, body)?;
        let response = self.transport.send(request).await.map_err(|error| {
            if effect {
                unknown_outcome()
            } else {
                match error {
                    ManagementTransportError::Timeout => timeout(true),
                    ManagementTransportError::Connect => dependency_unavailable(true),
                    ManagementTransportError::Response => invalid_response(),
                }
            }
        })?;
        if response.status != expected {
            return Err(map_status(response.status, effect));
        }
        if response.body.len() > MAX_RESPONSE_BYTES {
            return Err(post_accept_error(effect));
        }
        Ok(response.body)
    }

    fn build_request(
        &self,
        method: Method,
        segments: &[&str],
        query: &[(&str, &str)],
        body: Option<&Value>,
    ) -> Result<reqwest::Request, PostmanClientError> {
        let url = self.url(segments, query)?;
        let key = HeaderValue::from_str(&self.api_key).map_err(|_| invalid_configuration())?;
        let mut request = self
            .request_builder
            .request(method, url)
            .header(API_KEY_HEADER, key)
            .header(ACCEPT, "application/json");
        if let Some(body) = body {
            let encoded = serde_json::to_vec(body).map_err(|_| invalid_input())?;
            if encoded.len() > MAX_REQUEST_BYTES {
                return Err(resource_exhausted());
            }
            request = request
                .header(CONTENT_TYPE, "application/json")
                .body(encoded);
        }
        request.build().map_err(|_| invalid_input())
    }

    async fn json(&self, call: ManagementJsonCall<'_>) -> Result<Value, PostmanClientError> {
        let bytes = self
            .request(
                call.method,
                call.segments,
                call.query,
                call.body,
                call.effect,
                call.expected,
            )
            .await?;
        if bytes.is_empty() && call.allow_empty {
            return Ok(Value::Object(Map::new()));
        }
        let value = serde_json::from_slice(&bytes).map_err(|_| post_accept_error(call.effect))?;
        validate_json(&value).map_err(|_| post_accept_error(call.effect))?;
        if !value.is_object() {
            return Err(post_accept_error(call.effect));
        }
        Ok(value)
    }
}

#[cfg(test)]
pub(in crate::toolkits) fn test_management_request(
    config: PostmanToolkitConfig,
    method: Method,
    segments: &[&str],
    query: &[(&str, &str)],
    body: Option<&Value>,
) -> Result<reqwest::Request, PostmanClientError> {
    PostmanClient::new(config)?
        .management
        .build_request(method, segments, query, body)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_map_status(status: StatusCode, effect: bool) -> PostmanClientError {
    map_status(status, effect)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_redact_request(value: Value) -> Value {
    redact_sensitive_fields(value)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_expand_variables(
    input: &str,
    values: &[(&str, &str)],
) -> Result<String, PostmanClientError> {
    let values = values
        .iter()
        .map(|(key, value)| ((*key).to_owned(), (*value).to_owned()))
        .collect();
    expand_variables(input, &values)
}

#[cfg(test)]
pub(in crate::toolkits) struct DynamicPartsFixture {
    pub(in crate::toolkits) headers: Vec<(String, String)>,
    pub(in crate::toolkits) query: Vec<(String, String)>,
    pub(in crate::toolkits) body: Option<Vec<u8>>,
}

#[cfg(test)]
pub(in crate::toolkits) fn test_dynamic_parts(
    auth: Option<&Value>,
    body: Option<&Value>,
    stored_headers: &[(&str, &str)],
    variables: &[(&str, &str)],
) -> Result<DynamicPartsFixture, PostmanClientError> {
    let variables = variables
        .iter()
        .map(|(key, value)| ((*key).to_owned(), (*value).to_owned()))
        .collect::<BTreeMap<_, _>>();
    let mut headers = Vec::new();
    let mut query = Vec::new();
    apply_dynamic_auth(auth, &variables, &mut headers, &mut query)?;
    for (name, value) in stored_headers {
        insert_dynamic_header(&mut headers, name, value)?;
    }
    let body = dynamic_body(body, &Method::POST, &variables, &mut headers)?;
    let headers = headers
        .into_iter()
        .map(|(name, value)| {
            value
                .to_str()
                .map(|value| (name.to_string(), value.to_owned()))
                .map_err(|_| invalid_response())
        })
        .collect::<Result<Vec<_>, _>>()?;
    Ok(DynamicPartsFixture {
        headers,
        query,
        body,
    })
}

#[cfg(test)]
pub(in crate::toolkits) fn test_dynamic_header_value(
    value_bytes: usize,
) -> Result<(), PostmanClientError> {
    let mut headers = Vec::new();
    insert_dynamic_header(&mut headers, "x-test", &"v".repeat(value_bytes))
}

async fn read_response_body(
    response: reqwest::Response,
    effect: bool,
) -> Result<Vec<u8>, PostmanClientError> {
    if response
        .content_length()
        .is_some_and(|length| length > MAX_RESPONSE_BYTES as u64)
    {
        return Err(post_accept_error(effect));
    }
    let mut body = Vec::new();
    let mut stream = response.bytes_stream();
    while let Some(chunk) = stream.next().await {
        let chunk = chunk.map_err(|_| post_accept_error(effect))?;
        if body.len().saturating_add(chunk.len()) > MAX_RESPONSE_BYTES {
            return Err(post_accept_error(effect));
        }
        body.extend_from_slice(&chunk);
    }
    Ok(body)
}

fn validate_json(value: &Value) -> Result<(), PostmanClientError> {
    let mut nodes = 0usize;
    let mut stack = vec![(value, 1usize)];
    while let Some((node, depth)) = stack.pop() {
        nodes = nodes.checked_add(1).ok_or_else(resource_exhausted)?;
        if nodes > MAX_NODES || depth > MAX_DEPTH {
            return Err(resource_exhausted());
        }
        match node {
            Value::String(text) if text.len() > MAX_TEXT_BYTES => {
                return Err(resource_exhausted());
            }
            Value::Array(values) => {
                if values.len() > MAX_ITEMS {
                    return Err(resource_exhausted());
                }
                stack.extend(values.iter().map(|value| (value, depth + 1)));
            }
            Value::Object(values) => {
                if values.len() > MAX_ITEMS {
                    return Err(resource_exhausted());
                }
                stack.extend(values.values().map(|value| (value, depth + 1)));
            }
            Value::Null | Value::Bool(_) | Value::Number(_) | Value::String(_) => {}
        }
    }
    Ok(())
}

fn bounded_output(value: Value, effect: bool) -> Result<Value, PostmanClientError> {
    validate_json(&value).map_err(|_| post_accept_error(effect))?;
    let size = serde_json::to_vec(&value)
        .map_err(|_| post_accept_error(effect))?
        .len();
    if size > MAX_OUTPUT_BYTES {
        return Err(post_accept_error(effect));
    }
    Ok(value)
}

fn map_status(status: StatusCode, effect: bool) -> PostmanClientError {
    if effect
        && (status == StatusCode::REQUEST_TIMEOUT
            || status == StatusCode::TOO_MANY_REQUESTS
            || status.is_server_error()
            || status.is_success())
    {
        return unknown_outcome();
    }
    match status {
        StatusCode::UNAUTHORIZED => authentication(),
        StatusCode::FORBIDDEN => authorization(),
        StatusCode::NOT_FOUND => not_found(),
        StatusCode::CONFLICT | StatusCode::UNPROCESSABLE_ENTITY | StatusCode::BAD_REQUEST => {
            conflict()
        }
        StatusCode::REQUEST_TIMEOUT => timeout(true),
        StatusCode::TOO_MANY_REQUESTS => rate_limited(true),
        status if status.is_server_error() => dependency_unavailable(true),
        _ => invalid_response(),
    }
}

const fn post_accept_error(effect: bool) -> PostmanClientError {
    if effect {
        unknown_outcome()
    } else {
        invalid_response()
    }
}

fn validate_path(value: &str) -> Result<(), PostmanClientError> {
    validate_text(value, MAX_PATH_BYTES, false)?;
    let parts = path_parts(value);
    if parts.is_empty()
        || parts
            .iter()
            .any(|part| matches!(*part, "." | "..") || part.len() > MAX_IDENTIFIER_BYTES)
    {
        return Err(invalid_input());
    }
    Ok(())
}

fn validate_text(value: &str, limit: usize, allow_empty: bool) -> Result<(), PostmanClientError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if (!allow_empty && value.trim().is_empty())
        || value
            .chars()
            .any(|character| character.is_control() && !matches!(character, '\n' | '\t'))
    {
        return Err(invalid_input());
    }
    Ok(())
}

fn validate_http_method(value: &str) -> Result<Method, PostmanClientError> {
    if value.is_empty() || value.len() > 32 || !value.is_ascii() {
        return Err(invalid_input());
    }
    Method::from_bytes(value.to_ascii_uppercase().as_bytes()).map_err(|_| invalid_input())
}

const fn invalid_configuration() -> PostmanClientError {
    PostmanClientError {
        code: PostmanClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

pub(super) const fn invalid_input() -> PostmanClientError {
    PostmanClientError {
        code: PostmanClientErrorCode::InvalidInput,
        retryable: false,
    }
}

const fn authentication() -> PostmanClientError {
    PostmanClientError {
        code: PostmanClientErrorCode::Authentication,
        retryable: false,
    }
}

const fn authorization() -> PostmanClientError {
    PostmanClientError {
        code: PostmanClientErrorCode::Authorization,
        retryable: false,
    }
}

const fn not_found() -> PostmanClientError {
    PostmanClientError {
        code: PostmanClientErrorCode::NotFound,
        retryable: false,
    }
}

pub(super) const fn conflict() -> PostmanClientError {
    PostmanClientError {
        code: PostmanClientErrorCode::Conflict,
        retryable: false,
    }
}

const fn rate_limited(retryable: bool) -> PostmanClientError {
    PostmanClientError {
        code: PostmanClientErrorCode::RateLimited,
        retryable,
    }
}

const fn timeout(retryable: bool) -> PostmanClientError {
    PostmanClientError {
        code: PostmanClientErrorCode::Timeout,
        retryable,
    }
}

const fn dependency_unavailable(retryable: bool) -> PostmanClientError {
    PostmanClientError {
        code: PostmanClientErrorCode::DependencyUnavailable,
        retryable,
    }
}

pub(super) const fn invalid_response() -> PostmanClientError {
    PostmanClientError {
        code: PostmanClientErrorCode::InvalidResponse,
        retryable: false,
    }
}

pub(super) const fn resource_exhausted() -> PostmanClientError {
    PostmanClientError {
        code: PostmanClientErrorCode::ResourceExhausted,
        retryable: false,
    }
}

const fn unknown_outcome() -> PostmanClientError {
    PostmanClientError {
        code: PostmanClientErrorCode::UnknownOutcome,
        retryable: false,
    }
}
