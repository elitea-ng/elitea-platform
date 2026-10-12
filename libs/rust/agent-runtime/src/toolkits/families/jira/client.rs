use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64_STANDARD;
use reqwest::header::{ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, COOKIE, HeaderValue};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value, json};
use tokio::sync::Mutex;
use zeroize::Zeroizing;

use super::config::{JiraApiVersion, JiraCredential, JiraToolkitConfig, MAX_SEARCH_RESULTS};

const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
/// One provider response. `search_using_jql` asks for `fields=*all`, so a
/// full 100-issue page needs more room than an ordinary read.
const MAX_RESPONSE_BYTES: usize = 4 * 1_024 * 1_024;
const MAX_REQUEST_BYTES: usize = 256 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_PROVIDER_MESSAGE_BYTES: usize = 2 * 1_024;
const MAX_IDENTIFIER_BYTES: usize = 255;
const MAX_RELATIVE_URL_BYTES: usize = 2 * 1_024;
/// Jira caps `maxResults` per request at 100 whatever is asked.
const SEARCH_PAGE_SIZE: usize = 100;
const MAX_SEARCH_PAGES: usize = 50;
const MAX_PROJECT_PAGES: usize = 20;
const USER_AGENT: &str = "elitea-worker-rust/0.1";
const TRUNCATED_SUFFIX: &str = "\n[output truncated at the approved result limit]";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum JiraClientErrorCode {
    InvalidConfiguration,
    InvalidInput,
    Authentication,
    RateLimited,
    Timeout,
    DependencyUnavailable,
    InvalidResponse,
    ResourceExhausted,
    UnknownOutcome,
}

/// Stable infrastructure failure without origin, path, payload, body or
/// credential. Model-actionable Jira refusals (a 4xx naming the bad field or
/// missing issue) are tool RESULTS in the SDK's text shape, not this type.
pub(crate) struct JiraClientError {
    code: JiraClientErrorCode,
    retryable: bool,
}

impl JiraClientError {
    #[cfg(test)]
    #[must_use]
    pub(crate) const fn code(&self) -> JiraClientErrorCode {
        self.code
    }

    #[cfg(test)]
    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            JiraClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "jira.configuration.invalid",
                "the Jira toolkit configuration is invalid",
            ),
            JiraClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "jira.request.invalid",
                "the Jira request is invalid",
            ),
            JiraClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "jira.authentication.failed",
                "Jira authentication failed",
            ),
            JiraClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "jira.rate_limited",
                "Jira rate limited the request",
            ),
            JiraClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "jira.timeout",
                "the Jira request timed out",
            ),
            JiraClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "jira.unavailable",
                "Jira is unavailable",
            ),
            JiraClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "jira.response.invalid",
                "Jira returned an invalid response",
            ),
            JiraClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "jira.resource_exhausted",
                "the Jira request or response exceeds the approved limit",
            ),
            JiraClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "jira.effect.unknown_outcome",
                "Jira may have applied the requested effect; reconcile it before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(code: JiraClientErrorCode, retryable: bool) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for JiraClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("JiraClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for JiraClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            JiraClientErrorCode::InvalidConfiguration => "the Jira client configuration is invalid",
            JiraClientErrorCode::InvalidInput => "the Jira request is invalid",
            JiraClientErrorCode::Authentication => "Jira authentication failed",
            JiraClientErrorCode::RateLimited => "Jira rate limited the request",
            JiraClientErrorCode::Timeout => "the Jira request timed out",
            JiraClientErrorCode::DependencyUnavailable => "Jira is unavailable",
            JiraClientErrorCode::InvalidResponse => "Jira returned an invalid response",
            JiraClientErrorCode::ResourceExhausted => {
                "the Jira request or response exceeds its approved limit"
            }
            JiraClientErrorCode::UnknownOutcome => {
                "the Jira effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for JiraClientError {}

/// One Jira tool call, already argument-checked by `tools.rs`.
pub(in crate::toolkits) enum JiraOperation<'a> {
    SearchUsingJql {
        jql: &'a str,
        limit: Option<i64>,
    },
    CreateIssue {
        issue_json: &'a str,
    },
    UpdateIssue {
        issue_json: &'a str,
    },
    ModifyLabels {
        issue_key: &'a str,
        add_labels: Option<Vec<&'a str>>,
        remove_labels: Option<Vec<&'a str>>,
    },
    ListComments {
        issue_key: &'a str,
    },
    AddComments {
        issue_key: &'a str,
        comment: &'a str,
    },
    ListProjects,
    SetIssueStatus {
        issue_key: &'a str,
        status_name: &'a str,
        mandatory_fields_json: &'a str,
    },
    GetSpecificFieldInfo {
        issue_key: &'a str,
        field_name: &'a str,
    },
    GetRemoteLinks {
        issue_key: &'a str,
    },
    LinkIssues {
        inward_issue_key: &'a str,
        outward_issue_key: &'a str,
        linktype: &'a str,
    },
    ExecuteGenericRq {
        method: &'a str,
        relative_url: &'a str,
        params: Option<&'a str>,
    },
}

#[async_trait]
pub(in crate::toolkits) trait JiraApi: Send + Sync {
    async fn execute(&self, operation: JiraOperation<'_>) -> Result<Value, JiraClientError>;
}

pub(in crate::toolkits) struct JiraHttpResponse {
    status: StatusCode,
    body: Vec<u8>,
    json_content_type: bool,
}

impl JiraHttpResponse {
    #[cfg(test)]
    pub(in crate::toolkits) fn fixture(status: StatusCode, body: Option<Value>) -> Self {
        Self {
            status,
            body: body
                .map(|value| serde_json::to_vec(&value).unwrap_or_default())
                .unwrap_or_default(),
            json_content_type: true,
        }
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn text_fixture(status: StatusCode, body: &str) -> Self {
        Self {
            status,
            body: body.as_bytes().to_vec(),
            json_content_type: false,
        }
    }

    fn json(&self) -> Option<Value> {
        if self.body.is_empty() {
            return None;
        }
        serde_json::from_slice(&self.body).ok()
    }
}

#[async_trait]
pub(in crate::toolkits) trait JiraTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<JiraHttpResponse, JiraClientError>;
}

struct ReqwestJiraTransport {
    http: reqwest::Client,
}

#[async_trait]
impl JiraTransport for ReqwestJiraTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<JiraHttpResponse, JiraClientError> {
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
            .is_some_and(|value| {
                let value = value.trim();
                value.eq_ignore_ascii_case("application/json") || value.ends_with("+json")
            });
        let mut body = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|source| map_reqwest_error(&source, effect))?
        {
            if body.len().saturating_add(chunk.len()) > MAX_RESPONSE_BYTES {
                return Err(response_bound_failure(effect));
            }
            body.extend_from_slice(&chunk);
        }
        Ok(JiraHttpResponse {
            status: response.status(),
            body,
            json_content_type,
        })
    }
}

/// What one provider call came back as.
enum Reply {
    /// 2xx, with the parsed JSON body (`Null` when empty or not JSON).
    Accepted(Value),
    /// A 4xx Jira refusal: the joined `errorMessages`/`errors` text the SDK's
    /// `raise_for_status` would have raised, bounded and control-free.
    Refused(String),
}

/// One lazy, invocation-scoped Jira REST client.
pub(in crate::toolkits) struct JiraClient {
    config: JiraToolkitConfig,
    transport: Arc<dyn JiraTransport>,
    /// The SDK validates the credential with `GET /myself` when it builds the
    /// client, because Jira answers an unauthenticated search with 200 and
    /// no issues. Rust does it once, before the first operation.
    authenticated: Mutex<bool>,
}

impl JiraClient {
    pub(in crate::toolkits) fn new(config: JiraToolkitConfig) -> Result<Self, JiraClientError> {
        let http = reqwest::Client::builder()
            .https_only(true)
            .redirect(reqwest::redirect::Policy::none())
            .retry(reqwest::retry::never())
            .connect_timeout(CONNECT_TIMEOUT)
            .timeout(REQUEST_TIMEOUT)
            .pool_idle_timeout(POOL_IDLE_TIMEOUT)
            .pool_max_idle_per_host(MAX_IDLE_PER_HOST)
            .referer(false)
            .user_agent(USER_AGENT)
            .build()
            .map_err(|_| invalid_configuration())?;
        Ok(Self::with_transport_inner(
            config,
            Arc::new(ReqwestJiraTransport { http }),
        ))
    }

    fn with_transport_inner(config: JiraToolkitConfig, transport: Arc<dyn JiraTransport>) -> Self {
        Self {
            config,
            transport,
            authenticated: Mutex::new(false),
        }
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn with_transport(
        config: JiraToolkitConfig,
        transport: Arc<dyn JiraTransport>,
    ) -> Self {
        Self::with_transport_inner(config, transport)
    }

    fn version(&self) -> &'static str {
        self.config.api_version().as_str()
    }

    fn base(&self) -> &str {
        self.config.base_url().as_str().trim_end_matches('/')
    }

    fn browse_url(&self, key: &str) -> String {
        format!("{}/browse/{key}", self.base())
    }

    fn api_url(&self, version: &str, segments: &[&str]) -> Result<Url, JiraClientError> {
        let mut url = self.config.base_url().clone();
        {
            let mut path = url
                .path_segments_mut()
                .map_err(|()| invalid_configuration())?;
            path.pop_if_empty();
            path.extend(["rest", "api", version]);
            path.extend(segments);
        }
        Ok(url)
    }

    fn request(
        &self,
        method: Method,
        url: Url,
        body: Option<&Value>,
    ) -> Result<Request, JiraClientError> {
        let mut request = Request::new(method, url);
        let headers = request.headers_mut();
        for (name, value) in self.config.custom_headers() {
            headers.insert(name.clone(), value.clone());
        }
        headers.insert(ACCEPT, HeaderValue::from_static("application/json"));
        match self.config.credential() {
            JiraCredential::Bearer(token) => {
                let header = Zeroizing::new(format!("Bearer {}", token.as_str()));
                let mut value =
                    HeaderValue::from_str(&header).map_err(|_| invalid_configuration())?;
                value.set_sensitive(true);
                headers.insert(AUTHORIZATION, value);
            }
            JiraCredential::Cookie(token) => {
                // `parse_cookie_string`: `name=value` pairs separated by "; ".
                let cookie = Zeroizing::new(
                    token
                        .split("; ")
                        .filter(|pair| pair.contains('='))
                        .collect::<Vec<_>>()
                        .join("; "),
                );
                let mut value =
                    HeaderValue::from_str(&cookie).map_err(|_| invalid_configuration())?;
                value.set_sensitive(true);
                headers.insert(COOKIE, value);
            }
            JiraCredential::Basic { username, password } => {
                let mut source = Zeroizing::new(String::with_capacity(
                    username
                        .len()
                        .saturating_add(password.len())
                        .saturating_add(1),
                ));
                source.push_str(username);
                source.push(':');
                source.push_str(password);
                let header = Zeroizing::new(format!(
                    "Basic {}",
                    BASE64_STANDARD.encode(source.as_bytes())
                ));
                let mut value =
                    HeaderValue::from_str(&header).map_err(|_| invalid_configuration())?;
                value.set_sensitive(true);
                headers.insert(AUTHORIZATION, value);
            }
        }
        if let Some(body) = body {
            let bytes = serde_json::to_vec(body).map_err(|_| invalid_input())?;
            if bytes.len() > MAX_REQUEST_BYTES {
                return Err(resource_exhausted());
            }
            request
                .headers_mut()
                .insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
            *request.body_mut() = Some(bytes.into());
        }
        Ok(request)
    }

    async fn send(
        &self,
        method: Method,
        url: Url,
        body: Option<&Value>,
        effect: bool,
    ) -> Result<Reply, JiraClientError> {
        let request = self.request(method, url, body)?;
        let response = self.transport.execute(request, effect).await?;
        classify(&response, effect)
    }

    /// `JiraClient._validate_credentials`, once. A refusal is returned as the
    /// SDK's own sentence so the model can tell the user what to fix.
    async fn ensure_authenticated(&self) -> Result<Option<String>, JiraClientError> {
        let mut authenticated = self.authenticated.lock().await;
        if *authenticated {
            return Ok(None);
        }
        let url = self.api_url(self.version(), &["myself"])?;
        let response = self
            .transport
            .execute(self.request(Method::GET, url, None)?, false)
            .await?;
        let status = response.status;
        if status.is_success() {
            *authenticated = true;
            return Ok(None);
        }
        Ok(Some(match status {
            StatusCode::UNAUTHORIZED => {
                "Authentication failed: Invalid username or API key.".to_owned()
            }
            StatusCode::FORBIDDEN => "Authentication failed: Access forbidden.".to_owned(),
            StatusCode::NOT_FOUND => format!(
                "Jira REST API v{} not found at {}. Check the Hosting setting on the linked credential — Cloud uses v3, Server uses v2.",
                self.version(),
                self.base()
            ),
            StatusCode::TOO_MANY_REQUESTS => return Err(rate_limited()),
            status if status.is_server_error() => return Err(unavailable()),
            status => format!(
                "Authentication failed: Unable to connect to Jira (HTTP {}).",
                status.as_u16()
            ),
        }))
    }

    async fn search_using_jql(
        &self,
        jql: &str,
        limit: Option<i64>,
    ) -> Result<Value, JiraClientError> {
        // A per-call limit overrides the toolkit's; the SDK's `0` meant "no
        // cap", which here is the bounded maximum.
        let total = match limit {
            None => self.config.limit(),
            Some(value) if value <= 0 => MAX_SEARCH_RESULTS,
            Some(value) => usize::try_from(value)
                .unwrap_or(MAX_SEARCH_RESULTS)
                .min(MAX_SEARCH_RESULTS),
        };
        let page_size = SEARCH_PAGE_SIZE.min(total);
        let token_pagination = self.config.api_version() == JiraApiVersion::V3;
        let segments: &[&str] = if token_pagination {
            &["search", "jql"]
        } else {
            &["search"]
        };
        let mut issues: Vec<Value> = Vec::new();
        let mut next_page_token: Option<String> = None;
        for _ in 0..MAX_SEARCH_PAGES {
            let mut url = self.api_url(self.version(), segments)?;
            {
                let mut query = url.query_pairs_mut();
                query.append_pair("maxResults", &page_size.to_string());
                query.append_pair("fields", "*all");
                query.append_pair("jql", jql);
                if token_pagination {
                    if let Some(token) = &next_page_token {
                        query.append_pair("nextPageToken", token);
                    }
                } else {
                    query.append_pair("startAt", &issues.len().to_string());
                }
            }
            let document = match self.send(Method::GET, url, None, false).await? {
                Reply::Accepted(document) => document,
                Reply::Refused(message) => {
                    return text(
                        format!("Failed to fetch issues from Jira: Jira API error: {message}"),
                        false,
                    );
                }
            };
            let Some(page) = document
                .get("issues")
                .and_then(Value::as_array)
                .filter(|page| !page.is_empty())
            else {
                break;
            };
            let remaining = total.saturating_sub(issues.len());
            let returned = page.len();
            issues.extend(page.iter().take(remaining).cloned());
            if issues.len() >= total {
                break;
            }
            if token_pagination {
                next_page_token = document
                    .get("nextPageToken")
                    .and_then(Value::as_str)
                    .filter(|token| !token.is_empty())
                    .map(ToOwned::to_owned);
                if document.get("isLast").and_then(Value::as_bool) == Some(true)
                    || next_page_token.is_none()
                {
                    break;
                }
            } else if returned < page_size {
                break;
            }
        }
        if issues.is_empty() {
            return text("No Jira issues found".to_owned(), false);
        }
        let parsed = issues
            .iter()
            .map(|issue| self.parse_issue(issue))
            .collect::<Vec<_>>();
        text(
            format!(
                "Found {} Jira issues:\n{}",
                parsed.len(),
                render(&Value::Array(parsed))
            ),
            false,
        )
    }

    /// `JiraApiWrapper._parse_issues` for one issue.
    fn parse_issue(&self, issue: &Value) -> Value {
        let empty = Map::new();
        let fields = issue
            .get("fields")
            .and_then(Value::as_object)
            .unwrap_or(&empty);
        let key = issue.get("key").and_then(Value::as_str).unwrap_or_default();
        let nested_name = |field: &str, name: &str, default: &str| {
            fields
                .get(field)
                .and_then(|value| value.get(name))
                .filter(|value| truthy(value))
                .cloned()
                .unwrap_or_else(|| Value::String(default.to_owned()))
        };
        let mut related = Value::Object(Map::new());
        for link in fields
            .get("issuelinks")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
        {
            let (kind, other) = if link.get("inwardIssue").is_some_and(truthy) {
                (
                    link.get("type").and_then(|kind| kind.get("inward")),
                    link.get("inwardIssue"),
                )
            } else if link.get("outwardIssue").is_some_and(truthy) {
                (
                    link.get("type").and_then(|kind| kind.get("outward")),
                    link.get("outwardIssue"),
                )
            } else {
                continue;
            };
            let other_key = other
                .and_then(|issue| issue.get("key"))
                .and_then(Value::as_str);
            if let (Some(kind), Some(other_key)) = (kind.filter(|kind| truthy(kind)), other_key) {
                // The SDK overwrites, so the last link wins.
                related = json!({
                    "type": kind,
                    "key": other_key,
                    "url": self.browse_url(other_key),
                });
            }
        }
        let created = fields
            .get("created")
            .and_then(Value::as_str)
            .map(|value| value.chars().take(10).collect::<String>())
            .unwrap_or_default();
        let mut parsed = Map::new();
        parsed.insert("key".into(), Value::String(key.to_owned()));
        parsed.insert(
            "id".into(),
            issue
                .get("id")
                .cloned()
                .unwrap_or_else(|| Value::String(String::new())),
        );
        parsed.insert("projectId".into(), nested_name("project", "id", ""));
        parsed.insert("summary".into(), or_default(fields.get("summary"), ""));
        parsed.insert(
            "description".into(),
            or_default(fields.get("description"), ""),
        );
        parsed.insert("created".into(), Value::String(created));
        parsed.insert(
            "assignee".into(),
            nested_name("assignee", "displayName", "None"),
        );
        parsed.insert("priority".into(), nested_name("priority", "name", "None"));
        parsed.insert("status".into(), nested_name("status", "name", "Unknown"));
        parsed.insert("updated".into(), or_default(fields.get("updated"), ""));
        parsed.insert(
            "duedate".into(),
            fields.get("duedate").cloned().unwrap_or(Value::Null),
        );
        parsed.insert(
            "url".into(),
            Value::String(if key.is_empty() {
                self.base().to_owned()
            } else {
                self.browse_url(key)
            }),
        );
        parsed.insert("related_issues".into(), related);
        for field in self.config.additional_fields() {
            parsed.insert(
                field.to_string(),
                fields.get(field.as_ref()).cloned().unwrap_or(Value::Null),
            );
        }
        Value::Object(parsed)
    }

    async fn create_issue(&self, issue_json: &str) -> Result<Value, JiraClientError> {
        const PREFIX: &str = "Error creating Jira issue";
        let params = match parse_object(issue_json) {
            Ok(params) => params,
            Err(message) => return text(format!("{PREFIX}: {message}"), false),
        };
        let Some(fields) = params.get("fields").filter(|value| !value.is_null()) else {
            return text(CREATE_FIELDS_HINT.to_owned(), false);
        };
        let Some(fields) = fields.as_object() else {
            return text(format!("{PREFIX}: 'fields' must be a JSON object"), false);
        };
        if fields.get("project").is_none_or(Value::is_null) {
            return text(
                "Jira project key is required to create an issue. Ask user to provide it."
                    .to_owned(),
                false,
            );
        }
        let mut body = Map::new();
        body.insert("fields".into(), Value::Object(fields.clone()));
        if let Some(update) = params.get("update").filter(|value| !value.is_null()) {
            if !update.is_object() {
                return text(format!("{PREFIX}: 'update' must be a JSON object"), false);
            }
            body.insert("update".into(), update.clone());
        }
        let mut url = self.api_url(self.version(), &["issue"])?;
        url.query_pairs_mut().append_pair("updateHistory", "false");
        let issue = match self
            .send(Method::POST, url, Some(&Value::Object(body)), true)
            .await?
        {
            Reply::Accepted(issue) => issue,
            Reply::Refused(message) => return text(format!("{PREFIX}: {message}"), false),
        };
        let key = issue
            .get("key")
            .and_then(Value::as_str)
            .ok_or_else(unknown_outcome)?
            .to_owned();
        let mut output = format!(
            "Done. Issue {key} is created successfully. You can view it at {}. Details: {}",
            self.browse_url(&key),
            render(&issue)
        );
        self.append_default_labels(&key, &mut output).await;
        text(output, true)
    }

    async fn update_issue(&self, issue_json: &str) -> Result<Value, JiraClientError> {
        const PREFIX: &str = "Error updating Jira issue";
        let params = match parse_object(issue_json) {
            Ok(params) => params,
            Err(message) => return text(format!("{PREFIX}: {message}"), false),
        };
        let Some(key) = params.get("key").filter(|value| !value.is_null()) else {
            return text(
                "Jira issue key is required to update an issue. Ask user to provide it.".to_owned(),
                false,
            );
        };
        let Some(key) = key.as_str() else {
            return text(format!("{PREFIX}: 'key' must be a string"), false);
        };
        let fields = params.get("fields").filter(|value| !value.is_null());
        let update = params.get("update").filter(|value| !value.is_null());
        if fields.is_none() && update.is_none() {
            return text(UPDATE_FIELDS_HINT.to_owned(), false);
        }
        let mut output = match self.put_issue(key, fields, update).await? {
            Ok(output) => output,
            Err(message) => return text(format!("{PREFIX}: {message}"), false),
        };
        self.append_default_labels(key, &mut output).await;
        text(output, true)
    }

    /// `JiraApiWrapper._update_issue`: `PUT /rest/api/2/issue/{key}`. The
    /// SDK's client method hardcodes REST v2 for this one call whatever the
    /// toolkit's version, and Rust keeps that route.
    async fn put_issue(
        &self,
        key: &str,
        fields: Option<&Value>,
        update: Option<&Value>,
    ) -> Result<Result<String, String>, JiraClientError> {
        validate_identifier(key)?;
        let mut body = Map::new();
        for (name, value) in [("fields", fields), ("update", update)] {
            if let Some(value) = value {
                if !value.is_object() {
                    return Ok(Err(format!("'{name}' must be a JSON object")));
                }
                if truthy(value) {
                    body.insert(name.into(), value.clone());
                }
            }
        }
        let url = self.api_url(JiraApiVersion::V2.as_str(), &["issue", key])?;
        Ok(
            match self
                .send(Method::PUT, url, Some(&Value::Object(body)), true)
                .await?
            {
                Reply::Accepted(details) => Ok(format!(
                    "Done. Issue {key} has been updated successfully. You can view it at {}. Details: {}",
                    self.browse_url(key),
                    render(&details)
                )),
                Reply::Refused(message) => Err(message),
            },
        )
    }

    async fn modify_labels(
        &self,
        issue_key: &str,
        add_labels: Option<&[&str]>,
        remove_labels: Option<&[&str]>,
    ) -> Result<Value, JiraClientError> {
        if add_labels.is_none() && remove_labels.is_none() {
            return text(
                "You must provide at least 1 label to be added or removed".to_owned(),
                false,
            );
        }
        let update = labels_update(
            add_labels.unwrap_or_default(),
            remove_labels.unwrap_or_default(),
        );
        match self.put_issue(issue_key, None, Some(&update)).await? {
            Ok(output) => text(output, true),
            Err(message) => text(format!("Error updating Jira issue: {message}"), false),
        }
    }

    /// `_add_default_labels`: the toolkit's configured labels follow every
    /// create, update, comment and transition. The primary effect has
    /// already succeeded, so a label failure is reported, not raised.
    async fn append_default_labels(&self, key: &str, output: &mut String) {
        if self.config.labels().is_empty() {
            return;
        }
        let labels = self
            .config
            .labels()
            .iter()
            .map(AsRef::as_ref)
            .collect::<Vec<&str>>();
        let update = labels_update(&labels, &[]);
        let failure = match self.put_issue(key, None, Some(&update)).await {
            Ok(Ok(_)) => return,
            Ok(Err(message)) => message,
            Err(error) => error.to_string(),
        };
        output.push_str("\nWarning: the toolkit's default labels were not applied: ");
        output.push_str(&failure);
    }

    async fn list_comments(&self, issue_key: &str) -> Result<Value, JiraClientError> {
        validate_identifier(issue_key)?;
        let url = self.api_url(self.version(), &["issue", issue_key, "comment"])?;
        let document = match self.send(Method::GET, url, None, false).await? {
            Reply::Accepted(document) => document,
            Reply::Refused(message) => {
                return text(
                    format!("Error during the attempt to extract available comments: {message}"),
                    false,
                );
            }
        };
        let comments = document
            .get("comments")
            .and_then(Value::as_array)
            .ok_or_else(invalid_response)?
            .iter()
            .map(|comment| {
                json!({
                    "author": comment
                        .get("author")
                        .and_then(|author| author.get("displayName"))
                        .cloned()
                        .unwrap_or(Value::Null),
                    "comment": comment.get("body").cloned().unwrap_or(Value::Null),
                    "id": comment.get("id").cloned().unwrap_or(Value::Null),
                    "url": comment.get("self").cloned().unwrap_or(Value::Null),
                })
            })
            .collect::<Vec<_>>();
        text(
            format!(
                "Done. Comments were found for issue '{issue_key}': {}",
                render(&Value::Array(comments))
            ),
            false,
        )
    }

    async fn add_comments(&self, issue_key: &str, comment: &str) -> Result<Value, JiraClientError> {
        validate_identifier(issue_key)?;
        let body = if self.config.api_version() == JiraApiVersion::V3 {
            adf_paragraph(comment)
        } else {
            Value::String(comment.to_owned())
        };
        let url = self.api_url(self.version(), &["issue", issue_key, "comment"])?;
        let response = match self
            .send(Method::POST, url, Some(&json!({ "body": body })), true)
            .await?
        {
            Reply::Accepted(response) => response,
            Reply::Refused(message) => {
                return text(
                    format!("Error adding comment to Jira issue: {message}"),
                    false,
                );
            }
        };
        let comment_id = response.get("id").map_or_else(
            || "unknown".to_owned(),
            |id| {
                id.as_str()
                    .map_or_else(|| id.to_string(), ToOwned::to_owned)
            },
        );
        let mut output = format!(
            "Done. Comment {comment_id} is added for issue {issue_key}. You can view it at {}",
            self.browse_url(issue_key)
        );
        self.append_default_labels(issue_key, &mut output).await;
        text(output, true)
    }

    async fn list_projects(&self) -> Result<Value, JiraClientError> {
        const PREFIX: &str = "Error listing Jira projects";
        let mut projects: Vec<Value> = Vec::new();
        if self.config.cloud() {
            // `Jira._get_paged` over `project/search`, following `nextPage`
            // only while it stays on this toolkit's own REST root.
            let mut url = self.api_url(self.version(), &["project", "search"])?;
            let root = self.api_url(self.version(), &[])?;
            for _ in 0..MAX_PROJECT_PAGES {
                let document = match self.send(Method::GET, url, None, false).await? {
                    Reply::Accepted(document) => document,
                    Reply::Refused(message) => return text(format!("{PREFIX}: {message}"), false),
                };
                let values = document
                    .get("values")
                    .and_then(Value::as_array)
                    .cloned()
                    .unwrap_or_default();
                let empty = values.is_empty();
                projects.extend(values);
                if empty || document.get("isLast").and_then(Value::as_bool) == Some(true) {
                    break;
                }
                let Some(next) = document
                    .get("nextPage")
                    .and_then(Value::as_str)
                    .and_then(|next| Url::parse(next).ok())
                    .filter(|next| same_root(next, &root))
                else {
                    break;
                };
                url = next;
            }
        } else {
            let url = self.api_url(self.version(), &["project"])?;
            match self.send(Method::GET, url, None, false).await? {
                Reply::Accepted(Value::Array(values)) => projects = values,
                Reply::Accepted(_) => return Err(invalid_response()),
                Reply::Refused(message) => return text(format!("{PREFIX}: {message}"), false),
            }
        }
        let parsed = projects
            .iter()
            .map(|project| {
                json!({
                    "id": project.get("id").cloned().unwrap_or(Value::Null),
                    "key": project.get("key").cloned().unwrap_or(Value::Null),
                    "name": project.get("name").cloned().unwrap_or(Value::Null),
                    "type": project.get("projectTypeKey").cloned().unwrap_or(Value::Null),
                    "style": "",
                })
            })
            .collect::<Vec<_>>();
        text(
            format!(
                "Found {} projects:\n{}",
                parsed.len(),
                render(&Value::Array(parsed))
            ),
            false,
        )
    }

    async fn set_issue_status(
        &self,
        issue_key: &str,
        status_name: &str,
        mandatory_fields_json: &str,
    ) -> Result<Value, JiraClientError> {
        // The SDK reports every failure here under its create prefix.
        const PREFIX: &str = "Error creating Jira issue";
        validate_identifier(issue_key)?;
        let params = match parse_object(mandatory_fields_json) {
            Ok(params) => params,
            Err(message) => return text(format!("{PREFIX}: {message}"), false),
        };
        let url = self.api_url(self.version(), &["issue", issue_key, "transitions"])?;
        let document = match self.send(Method::GET, url, None, false).await? {
            Reply::Accepted(document) => document,
            Reply::Refused(message) => return text(format!("{PREFIX}: {message}"), false),
        };
        let transitions = document
            .get("transitions")
            .and_then(Value::as_array)
            .ok_or_else(invalid_response)?;
        let wanted = status_name.to_lowercase();
        let transition_id = transitions.iter().find_map(|transition| {
            let target = transition
                .get("to")
                .and_then(|to| to.get("name"))
                .and_then(Value::as_str)?;
            if target.to_lowercase() != wanted {
                return None;
            }
            let id = transition.get("id")?;
            id.as_str()
                .and_then(|id| id.parse::<i64>().ok())
                .or_else(|| id.as_i64())
        });
        let Some(transition_id) = transition_id else {
            let available = transitions
                .iter()
                .filter_map(|transition| {
                    transition
                        .get("to")
                        .and_then(|to| to.get("name"))
                        .and_then(Value::as_str)
                })
                .collect::<Vec<_>>()
                .join(", ");
            return text(
                format!(
                    "{PREFIX}: no transition to status '{status_name}' is available for {issue_key}; available target statuses: {available}"
                ),
                false,
            );
        };
        let mut body = Map::new();
        body.insert("transition".into(), json!({ "id": transition_id }));
        for name in ["fields", "update"] {
            if let Some(value) = params.get(name).filter(|value| !value.is_null()) {
                if !value.is_object() {
                    return text(format!("{PREFIX}: '{name}' must be a JSON object"), false);
                }
                body.insert(name.into(), value.clone());
            }
        }
        let url = self.api_url(self.version(), &["issue", issue_key, "transitions"])?;
        if let Reply::Refused(message) = self
            .send(Method::POST, url, Some(&Value::Object(body)), true)
            .await?
        {
            return text(format!("{PREFIX}: {message}"), false);
        }
        let mut output = format!(
            "Done. Status for issue {issue_key} was updated successfully. You can view it at {}.",
            self.browse_url(issue_key)
        );
        self.append_default_labels(issue_key, &mut output).await;
        text(output, true)
    }

    async fn get_specific_field_info(
        &self,
        issue_key: &str,
        field_name: &str,
    ) -> Result<Value, JiraClientError> {
        validate_identifier(issue_key)?;
        let mut url = self.api_url(self.version(), &["issue", issue_key])?;
        url.query_pairs_mut().append_pair("fields", field_name);
        let issue = match self.send(Method::GET, url, None, false).await? {
            Reply::Accepted(issue) => issue,
            Reply::Refused(message) => return text(format!("Jira API error: {message}"), false),
        };
        if let Some(value) = issue
            .get("fields")
            .and_then(|fields| fields.get(field_name))
            .filter(|value| truthy(value))
        {
            return text(
                format!(
                    "Got the data from following Jira issue - {issue_key} and field - {field_name}. The data is:\n{}",
                    render(value)
                ),
                false,
            );
        }
        let mut url = self.api_url(self.version(), &["issue", issue_key])?;
        url.query_pairs_mut().append_pair("fields", "*all");
        let issue = match self.send(Method::GET, url, None, false).await? {
            Reply::Accepted(issue) => issue,
            Reply::Refused(message) => return text(format!("Jira API error: {message}"), false),
        };
        let existing = issue
            .get("fields")
            .and_then(Value::as_object)
            .map(|fields| {
                fields
                    .iter()
                    .filter(|(_, value)| !value.is_null())
                    .map(|(name, _)| name.as_str())
                    .collect::<Vec<_>>()
                    .join(", ")
            })
            .unwrap_or_default();
        text(
            format!("Unable to find field '{field_name}'. All available fields are '{existing}'"),
            false,
        )
    }

    async fn get_remote_links(&self, issue_key: &str) -> Result<Value, JiraClientError> {
        validate_identifier(issue_key)?;
        let url = self.api_url(self.version(), &["issue", issue_key, "remotelink"])?;
        let links = match self.send(Method::GET, url, None, false).await? {
            Reply::Accepted(links) => links,
            Reply::Refused(message) => return text(format!("Jira API error: {message}"), false),
        };
        text(
            format!(
                "Jira issue - {issue_key} has the following remote links:\n{}",
                render(&links)
            ),
            false,
        )
    }

    async fn link_issues(
        &self,
        inward_issue_key: &str,
        outward_issue_key: &str,
        linktype: &str,
    ) -> Result<Value, JiraClientError> {
        validate_identifier(inward_issue_key)?;
        validate_identifier(outward_issue_key)?;
        let comment = format!("Issue {inward_issue_key} was linked to {outward_issue_key}.");
        let comment_body = if self.config.api_version() == JiraApiVersion::V3 {
            adf_paragraph(&comment)
        } else {
            Value::String(comment)
        };
        let link_data = json!({
            "type": {"name": linktype},
            "inwardIssue": {"key": inward_issue_key},
            "outwardIssue": {"key": outward_issue_key},
            "comment": {"body": comment_body},
        });
        let url = self.api_url(self.version(), &["issueLink"])?;
        if let Reply::Refused(message) =
            self.send(Method::POST, url, Some(&link_data), true).await?
        {
            return text(format!("Jira API error: {message}"), false);
        }
        text(
            format!("Link created using following data: {}.", render(&link_data)),
            true,
        )
    }

    async fn execute_generic_rq(
        &self,
        method: &str,
        relative_url: &str,
        params: Option<&str>,
    ) -> Result<Value, JiraClientError> {
        let method_name = method.trim().to_ascii_uppercase();
        let (http_method, effect) = match method_name.as_str() {
            "GET" => (Method::GET, false),
            "HEAD" => (Method::HEAD, false),
            "OPTIONS" => (Method::OPTIONS, false),
            "POST" => (Method::POST, true),
            "PUT" => (Method::PUT, true),
            "PATCH" => (Method::PATCH, true),
            "DELETE" => (Method::DELETE, true),
            _ => {
                return text(
                    format!(
                        "JIRA tool exception. Unsupported HTTP method '{method}'; use GET, POST, PUT, PATCH or DELETE."
                    ),
                    false,
                );
            }
        };
        let Some(mut url) = self.relative_url(relative_url) else {
            return text(
                "JIRA tool exception. relative_url must be a path on this Jira instance that starts with '/', for example '/rest/api/2/issue/PROJ-1', without a query string, fragment or '..' segment."
                    .to_owned(),
                false,
            );
        };
        let payload = match parse_payload_params(params) {
            Ok(payload) => payload,
            Err(message) => {
                return text(
                    format!("JIRA tool exception. Passed params are not valid JSON. {message}"),
                    false,
                );
            }
        };
        let body = if effect {
            Some(Value::Object(payload.clone()))
        } else {
            append_query(&mut url, &payload);
            None
        };
        let request = self.request(http_method, url, body.as_ref())?;
        let response = self.transport.execute(request, effect).await?;
        let status = response.status;
        match status {
            StatusCode::UNAUTHORIZED => return Err(authentication()),
            StatusCode::TOO_MANY_REQUESTS => return Err(rate_limited()),
            status if status.is_server_error() => {
                return Err(if effect {
                    unknown_outcome()
                } else {
                    unavailable()
                });
            }
            status if status.is_client_error() => {
                return text(
                    format!("Jira API error: {}", provider_message(&response)),
                    false,
                );
            }
            _ => {}
        }
        let response_text = if method_name == "GET" && is_search_path(relative_url) {
            process_search_response(self, &response, &payload)
        } else {
            String::from_utf8_lossy(&response.body).into_owned()
        };
        text(
            format!(
                "HTTP: {method} {relative_url} -> {} {} {response_text}",
                status.as_u16(),
                status.canonical_reason().unwrap_or_default()
            ),
            effect,
        )
    }

    /// A model-supplied path joined onto this toolkit's base (the SDK's
    /// `url_joiner`), refused unless it stays a plain path on that origin.
    fn relative_url(&self, relative_url: &str) -> Option<Url> {
        if relative_url.is_empty()
            || relative_url.len() > MAX_RELATIVE_URL_BYTES
            || !relative_url.starts_with('/')
            || relative_url.starts_with("//")
            || relative_url.contains(['?', '#', '\\'])
            || relative_url.contains("://")
            || relative_url
                .chars()
                .any(|character| character.is_control() || character.is_whitespace())
            || relative_url
                .split('/')
                .any(|segment| matches!(segment, "." | ".."))
            || relative_url.to_ascii_lowercase().contains("%2e")
        {
            return None;
        }
        let url = Url::parse(&format!("{}{relative_url}", self.base())).ok()?;
        let base = self.config.base_url();
        (url.origin() == base.origin() && url.path().starts_with(base.path().trim_end_matches('/')))
            .then_some(url)
    }
}

#[async_trait]
impl JiraApi for JiraClient {
    async fn execute(&self, operation: JiraOperation<'_>) -> Result<Value, JiraClientError> {
        if let Some(refusal) = self.ensure_authenticated().await? {
            return text(refusal, false);
        }
        match operation {
            JiraOperation::SearchUsingJql { jql, limit } => self.search_using_jql(jql, limit).await,
            JiraOperation::CreateIssue { issue_json } => self.create_issue(issue_json).await,
            JiraOperation::UpdateIssue { issue_json } => self.update_issue(issue_json).await,
            JiraOperation::ModifyLabels {
                issue_key,
                add_labels,
                remove_labels,
            } => {
                self.modify_labels(issue_key, add_labels.as_deref(), remove_labels.as_deref())
                    .await
            }
            JiraOperation::ListComments { issue_key } => self.list_comments(issue_key).await,
            JiraOperation::AddComments { issue_key, comment } => {
                self.add_comments(issue_key, comment).await
            }
            JiraOperation::ListProjects => self.list_projects().await,
            JiraOperation::SetIssueStatus {
                issue_key,
                status_name,
                mandatory_fields_json,
            } => {
                self.set_issue_status(issue_key, status_name, mandatory_fields_json)
                    .await
            }
            JiraOperation::GetSpecificFieldInfo {
                issue_key,
                field_name,
            } => self.get_specific_field_info(issue_key, field_name).await,
            JiraOperation::GetRemoteLinks { issue_key } => self.get_remote_links(issue_key).await,
            JiraOperation::LinkIssues {
                inward_issue_key,
                outward_issue_key,
                linktype,
            } => {
                self.link_issues(inward_issue_key, outward_issue_key, linktype)
                    .await
            }
            JiraOperation::ExecuteGenericRq {
                method,
                relative_url,
                params,
            } => self.execute_generic_rq(method, relative_url, params).await,
        }
    }
}

const CREATE_FIELDS_HINT: &str = "Jira fields are provided in a wrong way.\nFor example, to create a low priority task called \"test issue\" with description \"test description\", you would pass in the following STRING dictionary:\n{\"fields\": {\"project\": {\"key\": \"project_key\"}, \"summary\": \"test issue\", \"description\": \"test description\", \"issuetype\": {\"name\": \"Task\"}, \"priority\": {\"name\": \"Major\"}}}";

const UPDATE_FIELDS_HINT: &str = "Jira fields are provided in a wrong way. It should have at least any of nodes `fields` or `update`\nFor example, to update a task with key XXX-123 with new summary, description and custom field, you would pass in the following STRING dictionary:\n{\"key\": \"issue key\", \"fields\": {\"summary\": \"updated issue\", \"description\": \"updated description\", \"customfield_xxx\": \"updated custom field\"}}";

fn classify(response: &JiraHttpResponse, effect: bool) -> Result<Reply, JiraClientError> {
    let status = response.status;
    if status.is_success() {
        let value = if response.json_content_type {
            match response.json() {
                Some(value) => value,
                None if response.body.is_empty() => Value::Null,
                None => return Err(response_shape_failure(effect)),
            }
        } else {
            Value::Null
        };
        return Ok(Reply::Accepted(value));
    }
    match status {
        StatusCode::UNAUTHORIZED => Err(authentication()),
        StatusCode::TOO_MANY_REQUESTS => Err(rate_limited()),
        status if status.is_server_error() => Err(if effect {
            unknown_outcome()
        } else {
            unavailable()
        }),
        status if status.is_client_error() => Ok(Reply::Refused(provider_message(response))),
        // Redirects are not followed; anything else is not Jira's REST shape.
        _ => Err(response_shape_failure(effect)),
    }
}

/// `AtlassianRestAPI.raise_for_status`: `errorMessages` plus the values of
/// `errors`, joined by newlines; the status line when the body has neither.
fn provider_message(response: &JiraHttpResponse) -> String {
    let mut messages: Vec<String> = Vec::new();
    if let Some(document) = response.json() {
        if let Some(list) = document.get("errorMessages").and_then(Value::as_array) {
            messages.extend(list.iter().map(display_value));
        }
        match document.get("errors") {
            Some(Value::Object(errors)) => {
                if let Some(message) = errors.get("message") {
                    messages.push(display_value(message));
                } else {
                    messages.extend(
                        errors
                            .iter()
                            .map(|(name, value)| format!("{name}: {}", display_value(value))),
                    );
                }
            }
            Some(Value::Array(errors)) => messages.extend(errors.iter().map(|error| {
                error
                    .get("message")
                    .map_or_else(|| display_value(error), display_value)
            })),
            _ => {}
        }
    }
    let status = format!(
        "HTTP {} {}",
        response.status.as_u16(),
        response.status.canonical_reason().unwrap_or_default()
    );
    let joined = messages
        .into_iter()
        .filter(|message| !message.is_empty())
        .collect::<Vec<_>>()
        .join("\n");
    let message = if joined.is_empty() {
        status
    } else {
        format!("{status}: {joined}")
    };
    sanitize(&message, MAX_PROVIDER_MESSAGE_BYTES)
}

fn display_value(value: &Value) -> String {
    value
        .as_str()
        .map_or_else(|| value.to_string(), ToOwned::to_owned)
}

fn sanitize(value: &str, limit: usize) -> String {
    let mut out = String::with_capacity(value.len().min(limit));
    for character in value.chars() {
        let character = if character.is_control() && character != '\n' {
            ' '
        } else {
            character
        };
        if out.len() + character.len_utf8() > limit {
            break;
        }
        out.push(character);
    }
    out
}

/// `process_search_response` for `execute_generic_rq` searches: the SDK's
/// six-field projection plus any `fields` the call's params named.
fn process_search_response(
    client: &JiraClient,
    response: &JiraHttpResponse,
    payload: &Map<String, Value>,
) -> String {
    let Some(document) = response.json() else {
        return String::from_utf8_lossy(&response.body).into_owned();
    };
    let extra: Vec<String> = match payload.get("fields") {
        Some(Value::String(fields)) if !fields.trim().is_empty() => {
            fields.split(',').map(ToOwned::to_owned).collect()
        }
        Some(Value::Array(fields)) => fields
            .iter()
            .filter_map(Value::as_str)
            .map(ToOwned::to_owned)
            .collect(),
        _ => Vec::new(),
    };
    let issues = document
        .get("issues")
        .and_then(Value::as_array)
        .map(|issues| {
            issues
                .iter()
                .map(|issue| {
                    let key = issue.get("key").and_then(Value::as_str).unwrap_or_default();
                    let field = |name: &str| {
                        issue
                            .get("fields")
                            .and_then(|fields| fields.get(name))
                            .filter(|value| truthy(value))
                    };
                    let nested = |name: &str, inner: &str, default: &str| {
                        field(name)
                            .and_then(|value| value.get(inner))
                            .cloned()
                            .unwrap_or_else(|| Value::String(default.to_owned()))
                    };
                    let mut parsed = Map::new();
                    parsed.insert("key".into(), Value::String(key.to_owned()));
                    parsed.insert("url".into(), Value::String(client.browse_url(key)));
                    parsed.insert(
                        "summary".into(),
                        field("summary")
                            .cloned()
                            .unwrap_or_else(|| Value::String(String::new())),
                    );
                    parsed.insert("assignee".into(), nested("assignee", "displayName", "None"));
                    parsed.insert("status".into(), nested("status", "name", ""));
                    parsed.insert("issuetype".into(), nested("issuetype", "name", ""));
                    for name in &extra {
                        if !parsed.contains_key(name)
                            && let Some(value) = field(name)
                        {
                            parsed.insert(name.clone(), value.clone());
                        }
                    }
                    Value::Object(parsed)
                })
                .collect::<Vec<_>>()
        })
        .unwrap_or_default();
    render(&Value::Array(issues))
}

fn is_search_path(relative_url: &str) -> bool {
    // `re.match(r'/rest/api/\d+/search', relative_url)`
    relative_url
        .strip_prefix("/rest/api/")
        .and_then(|rest| {
            let digits = rest.bytes().take_while(u8::is_ascii_digit).count();
            (digits > 0).then(|| &rest[digits..])
        })
        .is_some_and(|rest| rest.starts_with("/search"))
}

/// `parse_payload_params`: the outermost `{...}` of the text (the SDK's
/// `clean_json_string`), parsed as a JSON object; empty means none.
fn parse_payload_params(params: Option<&str>) -> Result<Map<String, Value>, String> {
    let Some(params) = params.filter(|params| !params.trim().is_empty()) else {
        return Ok(Map::new());
    };
    let candidate = match (params.find('{'), params.rfind('}')) {
        (Some(start), Some(end)) if start < end => &params[start..=end],
        _ => params,
    };
    match serde_json::from_str::<Value>(candidate) {
        Ok(Value::Object(object)) => Ok(object),
        Ok(_) => Err("params must be a JSON object".to_owned()),
        Err(error) => Err(error.to_string()),
    }
}

fn append_query(url: &mut Url, payload: &Map<String, Value>) {
    if payload.is_empty() {
        return;
    }
    let mut query = url.query_pairs_mut();
    for (name, value) in payload {
        match value {
            Value::Null => {}
            Value::Array(values) => {
                for value in values {
                    query.append_pair(name, &display_value(value));
                }
            }
            value => {
                query.append_pair(name, &display_value(value));
            }
        }
    }
}

fn same_root(candidate: &Url, root: &Url) -> bool {
    candidate.origin() == root.origin()
        && candidate.fragment().is_none()
        && candidate
            .path()
            .starts_with(root.path().trim_end_matches('/'))
}

fn parse_object(source: &str) -> Result<Map<String, Value>, String> {
    match serde_json::from_str::<Value>(source) {
        Ok(Value::Object(object)) => Ok(object),
        Ok(_) => Err("the JSON must be an object".to_owned()),
        Err(error) => Err(format!("the JSON is not valid: {error}")),
    }
}

fn labels_update(add: &[&str], remove: &[&str]) -> Value {
    let operations = add
        .iter()
        .map(|label| json!({ "add": label }))
        .chain(remove.iter().map(|label| json!({ "remove": label })))
        .collect::<Vec<_>>();
    json!({ "labels": operations })
}

fn adf_paragraph(text: &str) -> Value {
    json!({
        "type": "doc",
        "version": 1,
        "content": [{"type": "paragraph", "content": [{"type": "text", "text": text}]}],
    })
}

/// Python truthiness, which the SDK's `x or default` projections rely on.
fn truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(value) => *value,
        Value::Number(value) => value.as_f64().is_some_and(|value| value != 0.0),
        Value::String(value) => !value.is_empty(),
        Value::Array(value) => !value.is_empty(),
        Value::Object(value) => !value.is_empty(),
    }
}

fn or_default(value: Option<&Value>, default: &str) -> Value {
    value
        .filter(|value| truthy(value))
        .cloned()
        .unwrap_or_else(|| Value::String(default.to_owned()))
}

/// Structured values are rendered as compact JSON where the SDK printed a
/// Python `repr`; a string stays the string.
fn render(value: &Value) -> String {
    display_value(value)
}

fn validate_identifier(value: &str) -> Result<(), JiraClientError> {
    if value.len() > MAX_IDENTIFIER_BYTES {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || matches!(value, "." | "..") || value.chars().any(char::is_control)
    {
        return Err(invalid_input());
    }
    Ok(())
}

/// Results are text, as the SDK's are. A read over the bound is refused; an
/// effect has already happened, so its confirmation is cut short instead.
fn text(value: String, effect: bool) -> Result<Value, JiraClientError> {
    if value.len() <= MAX_OUTPUT_BYTES {
        return Ok(Value::String(value));
    }
    if !effect {
        return Err(resource_exhausted());
    }
    let mut cut = MAX_OUTPUT_BYTES - TRUNCATED_SUFFIX.len();
    while !value.is_char_boundary(cut) {
        cut -= 1;
    }
    Ok(Value::String(format!(
        "{}{TRUNCATED_SUFFIX}",
        &value[..cut]
    )))
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> JiraClientError {
    if effect {
        return unknown_outcome();
    }
    if source.is_timeout() {
        timeout()
    } else if source.is_connect() || source.is_request() || source.is_body() {
        unavailable()
    } else {
        invalid_response()
    }
}

const fn response_shape_failure(effect: bool) -> JiraClientError {
    if effect {
        unknown_outcome()
    } else {
        invalid_response()
    }
}

const fn response_bound_failure(effect: bool) -> JiraClientError {
    if effect {
        unknown_outcome()
    } else {
        resource_exhausted()
    }
}

const fn error(code: JiraClientErrorCode, retryable: bool) -> JiraClientError {
    JiraClientError { code, retryable }
}

const fn invalid_configuration() -> JiraClientError {
    error(JiraClientErrorCode::InvalidConfiguration, false)
}

const fn invalid_input() -> JiraClientError {
    error(JiraClientErrorCode::InvalidInput, false)
}

const fn authentication() -> JiraClientError {
    error(JiraClientErrorCode::Authentication, false)
}

const fn rate_limited() -> JiraClientError {
    error(JiraClientErrorCode::RateLimited, true)
}

const fn timeout() -> JiraClientError {
    error(JiraClientErrorCode::Timeout, true)
}

const fn unavailable() -> JiraClientError {
    error(JiraClientErrorCode::DependencyUnavailable, true)
}

const fn invalid_response() -> JiraClientError {
    error(JiraClientErrorCode::InvalidResponse, false)
}

const fn resource_exhausted() -> JiraClientError {
    error(JiraClientErrorCode::ResourceExhausted, false)
}

const fn unknown_outcome() -> JiraClientError {
    error(JiraClientErrorCode::UnknownOutcome, false)
}
