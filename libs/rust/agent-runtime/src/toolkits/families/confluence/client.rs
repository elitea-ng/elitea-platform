use std::collections::{HashSet, VecDeque};
use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64_STANDARD;
use html_to_markdown_rs::{ConversionOptions, PreprocessingOptions};
use reqwest::header::{ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, COOKIE, HeaderValue};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value, json};
use zeroize::Zeroizing;

use super::config::{ConfluenceApiVersion, ConfluenceCredential, ConfluenceToolkitConfig};

const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_RESPONSE_BYTES: usize = 4 * 1_024 * 1_024;
const MAX_REQUEST_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_PROVIDER_MESSAGE_BYTES: usize = 2 * 1_024;
const MAX_IDENTIFIER_BYTES: usize = 255;
const MAX_RELATIVE_URL_BYTES: usize = 2 * 1_024;
/// `get_all_descendants` reads children 100 at a time.
const CHILD_PAGE_SIZE: usize = 100;
const MAX_TREE_PAGES: usize = 1_000;
const MAX_TREE_DEPTH: usize = 32;
/// `create_pages` and the batch updates act on at most this many pages.
const MAX_BATCH_PAGES: usize = 50;
const MAX_LABELS: usize = 64;
const USER_AGENT: &str = "elitea-worker-rust/0.1";
const TRUNCATED_SUFFIX: &str = "\n[output truncated at the approved result limit]";
const V1_REPRESENTATIONS: [&str; 6] = [
    "atlas_doc_format",
    "editor",
    "export_view",
    "view",
    "storage",
    "wiki",
];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ConfluenceClientErrorCode {
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
/// credential. Model-actionable Confluence refusals are tool RESULTS.
pub(crate) struct ConfluenceClientError {
    code: ConfluenceClientErrorCode,
    retryable: bool,
}

impl ConfluenceClientError {
    #[cfg(test)]
    #[must_use]
    pub(crate) const fn code(&self) -> ConfluenceClientErrorCode {
        self.code
    }

    #[cfg(test)]
    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            ConfluenceClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "confluence.configuration.invalid",
                "the Confluence toolkit configuration is invalid",
            ),
            ConfluenceClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "confluence.request.invalid",
                "the Confluence request is invalid",
            ),
            ConfluenceClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "confluence.authentication.failed",
                "Confluence authentication failed",
            ),
            ConfluenceClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "confluence.rate_limited",
                "Confluence rate limited the request",
            ),
            ConfluenceClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "confluence.timeout",
                "the Confluence request timed out",
            ),
            ConfluenceClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "confluence.unavailable",
                "Confluence is unavailable",
            ),
            ConfluenceClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "confluence.response.invalid",
                "Confluence returned an invalid response",
            ),
            ConfluenceClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "confluence.resource_exhausted",
                "the Confluence request or response exceeds the approved limit",
            ),
            ConfluenceClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "confluence.effect.unknown_outcome",
                "Confluence may have applied the requested effect; reconcile it before retrying",
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
        code: ConfluenceClientErrorCode,
        retryable: bool,
    ) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for ConfluenceClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ConfluenceClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ConfluenceClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ConfluenceClientErrorCode::InvalidConfiguration => {
                "the Confluence client configuration is invalid"
            }
            ConfluenceClientErrorCode::InvalidInput => "the Confluence request is invalid",
            ConfluenceClientErrorCode::Authentication => "Confluence authentication failed",
            ConfluenceClientErrorCode::RateLimited => "Confluence rate limited the request",
            ConfluenceClientErrorCode::Timeout => "the Confluence request timed out",
            ConfluenceClientErrorCode::DependencyUnavailable => "Confluence is unavailable",
            ConfluenceClientErrorCode::InvalidResponse => "Confluence returned an invalid response",
            ConfluenceClientErrorCode::ResourceExhausted => {
                "the Confluence request or response exceeds its approved limit"
            }
            ConfluenceClientErrorCode::UnknownOutcome => {
                "the Confluence effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for ConfluenceClientError {}

/// One Confluence tool call, already argument-checked by `tools.rs`.
pub(in crate::toolkits) enum ConfluenceOperation<'a> {
    CreatePage {
        title: &'a str,
        body: &'a str,
        status: Option<&'a str>,
        space: Option<&'a str>,
        parent_id: Option<&'a str>,
        representation: Option<&'a str>,
        label: Option<&'a str>,
    },
    CreatePages {
        pages_info: &'a str,
        status: Option<&'a str>,
        space: Option<&'a str>,
        parent_id: Option<&'a str>,
    },
    DeletePage {
        page_id: Option<&'a str>,
        page_title: Option<&'a str>,
    },
    UpdatePageById {
        page_id: &'a str,
        representation: Option<&'a str>,
        new_title: Option<&'a str>,
        new_body: Option<&'a str>,
        new_labels: Option<Vec<String>>,
    },
    UpdatePageByTitle {
        page_title: &'a str,
        representation: Option<&'a str>,
        new_title: Option<&'a str>,
        new_body: Option<&'a str>,
        new_labels: Option<Vec<String>>,
    },
    UpdatePages {
        page_ids: Option<Vec<String>>,
        new_contents: Option<Vec<String>>,
        new_labels: Option<Vec<String>>,
    },
    UpdateLabels {
        page_ids: Option<Vec<String>>,
        new_labels: Option<Vec<String>>,
    },
    GetPageTree {
        page_id: &'a str,
    },
    GetPagesWithLabel {
        label: &'a str,
    },
    ListPagesWithLabel {
        label: &'a str,
    },
    ReadPageById {
        page_id: &'a str,
        skip_images: bool,
        content_format: Option<&'a str>,
    },
    SearchPages {
        query: &'a str,
        skip_images: bool,
    },
    SearchByTitle {
        query: &'a str,
        skip_images: bool,
    },
    SiteSearch {
        query: &'a str,
    },
    ExecuteGenericConfluence {
        method: &'a str,
        relative_url: &'a str,
        params: Option<&'a str>,
    },
    GetPageIdByTitle {
        title: &'a str,
        page_type: Option<&'a str>,
    },
}

#[async_trait]
pub(in crate::toolkits) trait ConfluenceApi: Send + Sync {
    async fn execute(
        &self,
        operation: ConfluenceOperation<'_>,
    ) -> Result<Value, ConfluenceClientError>;
}

pub(in crate::toolkits) struct ConfluenceHttpResponse {
    status: StatusCode,
    body: Vec<u8>,
    json_content_type: bool,
}

impl ConfluenceHttpResponse {
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

    fn json(&self) -> Option<Value> {
        if self.body.is_empty() {
            return None;
        }
        serde_json::from_slice(&self.body).ok()
    }
}

#[async_trait]
pub(in crate::toolkits) trait ConfluenceTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<ConfluenceHttpResponse, ConfluenceClientError>;
}

struct ReqwestConfluenceTransport {
    http: reqwest::Client,
}

#[async_trait]
impl ConfluenceTransport for ReqwestConfluenceTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<ConfluenceHttpResponse, ConfluenceClientError> {
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
        Ok(ConfluenceHttpResponse {
            status: response.status(),
            body,
            json_content_type,
        })
    }
}

/// What one provider call came back as.
enum Reply {
    /// 2xx, with the parsed JSON body (`Null` when empty).
    Accepted(Value),
    /// A 4xx Confluence refusal, rendered for the model.
    Refused(StatusCode, String),
}

/// One page as the SDK's `process_page` returns it.
struct PageDocument {
    id: Value,
    title: Value,
    source: String,
    content: String,
}

/// One lazy, invocation-scoped Confluence REST client.
pub(in crate::toolkits) struct ConfluenceClient {
    config: ConfluenceToolkitConfig,
    transport: Arc<dyn ConfluenceTransport>,
}

impl ConfluenceClient {
    pub(in crate::toolkits) fn new(
        config: ConfluenceToolkitConfig,
    ) -> Result<Self, ConfluenceClientError> {
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
        Ok(Self {
            config,
            transport: Arc::new(ReqwestConfluenceTransport { http }),
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn with_transport(
        config: ConfluenceToolkitConfig,
        transport: Arc<dyn ConfluenceTransport>,
    ) -> Self {
        Self { config, transport }
    }

    /// `{api_root}/{segments...}` with every segment percent-encoded.
    fn url(&self, segments: &[&str]) -> Result<Url, ConfluenceClientError> {
        let mut url = self.config.api_root().clone();
        {
            let mut path = url
                .path_segments_mut()
                .map_err(|()| invalid_configuration())?;
            path.pop_if_empty();
            path.extend(segments);
        }
        Ok(url)
    }

    fn v1(&self, segments: &[&str]) -> Result<Url, ConfluenceClientError> {
        let mut all = vec!["rest", "api"];
        all.extend_from_slice(segments);
        self.url(&all)
    }

    /// `_build_page_url`: Cloud page links carry `/wiki` exactly once.
    fn page_url(&self, webui_path: &str) -> String {
        let base = self.config.base_url().as_str().trim_end_matches('/');
        let mut path = webui_path.to_owned();
        if self.config.cloud() {
            if base.ends_with("/wiki") {
                if let Some(rest) = path.strip_prefix("/wiki/") {
                    path = format!("/{rest}");
                }
            } else if !path.starts_with("/wiki/") {
                path = format!("/wiki{path}");
            }
        }
        format!("{base}{path}")
    }

    fn request(
        &self,
        method: Method,
        url: Url,
        body: Option<&Value>,
    ) -> Result<Request, ConfluenceClientError> {
        let mut request = Request::new(method, url);
        let headers = request.headers_mut();
        for (name, value) in self.config.custom_headers() {
            headers.insert(name.clone(), value.clone());
        }
        headers.insert(ACCEPT, HeaderValue::from_static("application/json"));
        let (name, value) = match self.config.credential() {
            ConfluenceCredential::Bearer(token) => (
                AUTHORIZATION,
                Zeroizing::new(format!("Bearer {}", token.as_str())),
            ),
            ConfluenceCredential::Cookie(token) => (
                COOKIE,
                Zeroizing::new(
                    token
                        .split("; ")
                        .filter(|pair| pair.contains('='))
                        .collect::<Vec<_>>()
                        .join("; "),
                ),
            ),
            ConfluenceCredential::Basic { username, password } => {
                let mut source = Zeroizing::new(String::with_capacity(
                    username
                        .len()
                        .saturating_add(password.len())
                        .saturating_add(1),
                ));
                source.push_str(username);
                source.push(':');
                source.push_str(password);
                (
                    AUTHORIZATION,
                    Zeroizing::new(format!(
                        "Basic {}",
                        BASE64_STANDARD.encode(source.as_bytes())
                    )),
                )
            }
        };
        let mut value = HeaderValue::from_str(&value).map_err(|_| invalid_configuration())?;
        value.set_sensitive(true);
        headers.insert(name, value);
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
    ) -> Result<Reply, ConfluenceClientError> {
        let request = self.request(method, url, body)?;
        let response = self.transport.execute(request, effect).await?;
        classify(&response, effect)
    }

    async fn get(&self, url: Url) -> Result<Reply, ConfluenceClientError> {
        self.send(Method::GET, url, None, false).await
    }

    /// `Confluence.get_page_by_title`: the first page with this exact title.
    async fn page_by_title(
        &self,
        space: Option<&str>,
        title: &str,
        page_type: &str,
    ) -> Result<Result<Option<Value>, String>, ConfluenceClientError> {
        let mut url = self.v1(&["content"])?;
        {
            let mut query = url.query_pairs_mut();
            query.append_pair("type", page_type);
            query.append_pair("start", "0");
            query.append_pair("limit", "1");
            if let Some(space) = space {
                query.append_pair("spaceKey", space);
            }
            query.append_pair("title", title);
        }
        Ok(match self.get(url).await? {
            Reply::Accepted(document) => Ok(document
                .get("results")
                .and_then(Value::as_array)
                .and_then(|results| results.first())
                .cloned()),
            Reply::Refused(_, message) => Err(message),
        })
    }

    #[allow(clippy::too_many_arguments, clippy::too_many_lines)] // The SDK tool's seven arguments and steps in order.
    async fn create_page(
        &self,
        title: &str,
        body: &str,
        status: Option<&str>,
        space: Option<&str>,
        parent_id: Option<&str>,
        representation: Option<&str>,
        label: Option<&str>,
    ) -> Result<String, ConfluenceClientError> {
        let status = status.unwrap_or("current");
        let representation = representation.unwrap_or("storage");
        let Some(user_space) = space.or_else(|| self.config.space()) else {
            return Ok(
                "A Confluence space is required: set the toolkit's space or pass space.".to_owned(),
            );
        };
        match self.page_by_title(Some(user_space), title, "page").await? {
            Ok(Some(_)) => {
                return Ok(format!(
                    "Page with title {title} already exists, please use other title."
                ));
            }
            Ok(None) => {}
            Err(message) => return Ok(api_error(&message)),
        }
        let parent = match parent_id {
            Some(parent_id) => Some(parent_id.to_owned()),
            None => self.space_homepage(user_space).await?,
        };
        let created = if self.config.api_version() == ConfluenceApiVersion::V2 {
            match self
                .create_page_v2(
                    user_space,
                    title,
                    body,
                    parent.as_deref(),
                    representation,
                    status,
                )
                .await?
            {
                Ok(created) => created,
                Err(message) => return Ok(message),
            }
        } else {
            if !V1_REPRESENTATIONS.contains(&representation) {
                return Ok(
                    "Wrong value for representation, it should be either wiki or storage"
                        .to_owned(),
                );
            }
            let mut payload = json!({
                "type": "page",
                "title": title,
                "status": status,
                "space": {"key": user_space},
                "body": {representation: {"value": body, "representation": representation}},
                "metadata": {"properties": fixed_width()},
            });
            if let Some(parent) = &parent {
                payload["ancestors"] = json!([{"type": "page", "id": parent}]);
            }
            match self
                .send(Method::POST, self.v1(&["content"])?, Some(&payload), true)
                .await?
            {
                Reply::Accepted(created) if created.is_object() => created,
                Reply::Accepted(_) => return Err(unknown_outcome()),
                Reply::Refused(_, message) => return Ok(api_error(&message)),
            }
        };
        let page_id = id_text(created.get("id")).ok_or_else(unknown_outcome)?;
        let links = created.get("_links");
        let webui = links
            .and_then(|links| links.get(if status == "draft" { "edit" } else { "webui" }))
            .and_then(Value::as_str)
            .unwrap_or_default();
        let link = self.page_url(webui);
        let mut details = Map::new();
        details.insert(
            "title".into(),
            created.get("title").cloned().unwrap_or(Value::Null),
        );
        details.insert("id".into(), Value::String(page_id.clone()));
        details.insert(
            "space key".into(),
            created
                .pointer("/space/key")
                .cloned()
                .unwrap_or(Value::Null),
        );
        details.insert(
            "author".into(),
            created
                .pointer("/version/by/displayName")
                .cloned()
                .unwrap_or(Value::Null),
        );
        details.insert("link".into(), Value::String(link.clone()));
        let mut warnings = String::new();
        if let Some(label) = label {
            if let Some(failure) = self.set_label(&page_id, label).await? {
                warnings.push_str("\nWarning: label '");
                warnings.push_str(label);
                warnings.push_str("' was not added: ");
                warnings.push_str(&failure);
            } else {
                details.insert("label".into(), Value::String(label.to_owned()));
            }
        }
        warnings.push_str(&self.default_labels(&page_id).await?);
        let parent_display = parent.map_or_else(
            || "space root level".to_owned(),
            |parent| format!("parent page '{parent}'"),
        );
        Ok(format!(
            "The page '{title}' was created under {parent_display}: '{link}'. \nDetails: {}{warnings}",
            render(&Value::Object(details))
        ))
    }

    /// `get_space(...)["homepage"]["id"]`, or none when it cannot be read.
    async fn space_homepage(&self, space: &str) -> Result<Option<String>, ConfluenceClientError> {
        validate_identifier(space)?;
        let mut url = self.v1(&["space", space])?;
        url.query_pairs_mut()
            .append_pair("expand", "description.plain,homepage");
        Ok(match self.get(url).await? {
            Reply::Accepted(space) => id_text(space.pointer("/homepage/id")),
            Reply::Refused(..) => None,
        })
    }

    /// `_create_page_v2`, reshaped like a v1 reply as the SDK does.
    async fn create_page_v2(
        &self,
        space: &str,
        title: &str,
        body: &str,
        parent: Option<&str>,
        representation: &str,
        status: &str,
    ) -> Result<Result<Value, String>, ConfluenceClientError> {
        let Some(space_id) = self.space_id_v2(space).await? else {
            return Ok(Err(format!(
                "Could not resolve spaceId for space key '{space}' via v2 API. Confirm the space exists and the credentials have access."
            )));
        };
        let representation = match representation.trim().to_ascii_lowercase().as_str() {
            "atlas_doc_format" | "adf" => "atlas_doc_format",
            "wiki" => "wiki",
            _ => "storage",
        };
        let mut payload = json!({
            "spaceId": space_id,
            "status": status,
            "title": title,
            "body": {"representation": representation, "value": body},
        });
        if let Some(parent) = parent {
            payload["parentId"] = Value::String(parent.to_owned());
        }
        let response = match self
            .send(
                Method::POST,
                self.url(&["api", "v2", "pages"])?,
                Some(&payload),
                true,
            )
            .await?
        {
            Reply::Accepted(response) if response.is_object() => response,
            Reply::Accepted(_) => return Err(unknown_outcome()),
            Reply::Refused(_, message) => return Ok(Err(api_error(&message))),
        };
        let page_id = id_text(response.get("id")).unwrap_or_default();
        let link = |name: &str| {
            response
                .pointer(&format!("/_links/{name}"))
                .and_then(Value::as_str)
                .filter(|value| !value.is_empty())
                .map(ToOwned::to_owned)
        };
        let webui = link("webui").unwrap_or_else(|| format!("/spaces/{space}/pages/{page_id}"));
        let edit =
            link("editui").unwrap_or_else(|| format!("/pages/edit-v2.action?pageId={page_id}"));
        let author = response
            .pointer("/version/authorId")
            .or_else(|| response.get("authorId"))
            .cloned()
            .unwrap_or_else(|| Value::String(String::new()));
        Ok(Ok(json!({
            "id": page_id,
            "title": response.get("title").cloned().unwrap_or(Value::Null),
            "space": {"key": space},
            "version": {"by": {"displayName": author}},
            "_links": {"webui": webui, "edit": edit},
        })))
    }

    /// `_resolve_space_id_v2`: v2 lookup by key, then the v1 space read.
    async fn space_id_v2(&self, space: &str) -> Result<Option<String>, ConfluenceClientError> {
        let mut url = self.url(&["api", "v2", "spaces"])?;
        url.query_pairs_mut()
            .append_pair("keys", space)
            .append_pair("limit", "1");
        if let Reply::Accepted(document) = self.get(url).await?
            && let Some(id) = id_text(document.pointer("/results/0/id"))
        {
            return Ok(Some(id));
        }
        validate_identifier(space)?;
        let url = self.v1(&["space", space])?;
        Ok(match self.get(url).await? {
            Reply::Accepted(space) => id_text(space.get("id")),
            Reply::Refused(..) => None,
        })
    }

    async fn set_label(
        &self,
        page_id: &str,
        label: &str,
    ) -> Result<Option<String>, ConfluenceClientError> {
        let url = self.v1(&["content", page_id, "label"])?;
        Ok(
            match self
                .send(
                    Method::POST,
                    url,
                    Some(&json!({"prefix": "global", "name": label})),
                    true,
                )
                .await?
            {
                Reply::Accepted(_) => None,
                Reply::Refused(_, message) => Some(message),
            },
        )
    }

    /// `_add_default_labels`; failures are reported, the page effect stands.
    async fn default_labels(&self, page_id: &str) -> Result<String, ConfluenceClientError> {
        let mut warnings = String::new();
        for label in self.config.labels() {
            if let Some(failure) = self.set_label(page_id, label).await? {
                warnings.push_str("\nWarning: default label '");
                warnings.push_str(label);
                warnings.push_str("' was not added: ");
                warnings.push_str(&failure);
            }
        }
        Ok(warnings)
    }

    async fn create_pages(
        &self,
        pages_info: &str,
        status: Option<&str>,
        space: Option<&str>,
        parent_id: Option<&str>,
    ) -> Result<String, ConfluenceClientError> {
        let pages = match serde_json::from_str::<Value>(pages_info) {
            Ok(Value::Array(pages)) => pages,
            Ok(_) => {
                return Ok(
                    "Confluence tool exception. pages_info must be a JSON list of {\"title\": \"content\"} objects."
                        .to_owned(),
                );
            }
            Err(error) => {
                return Ok(format!(
                    "Confluence tool exception. pages_info is not valid JSON: {error}"
                ));
            }
        };
        let mut items = Vec::new();
        for page in &pages {
            let Some(page) = page.as_object() else {
                return Ok(
                    "Confluence tool exception. Each pages_info entry must be a {\"title\": \"content\"} object."
                        .to_owned(),
                );
            };
            for (title, body) in page {
                let Some(body) = body.as_str() else {
                    return Ok(format!(
                        "Confluence tool exception. The content of page '{title}' must be a string."
                    ));
                };
                items.push((title.as_str(), body));
            }
        }
        if items.len() > MAX_BATCH_PAGES {
            return Err(resource_exhausted());
        }
        let user_space = space.or_else(|| self.config.space());
        let parent = match (parent_id, user_space) {
            (Some(parent_id), _) => Some(parent_id.to_owned()),
            (None, Some(space)) => self.space_homepage(space).await?,
            (None, None) => None,
        };
        let mut created = Vec::with_capacity(items.len());
        for (title, body) in items {
            created.push(Value::String(
                self.create_page(
                    title,
                    body,
                    status,
                    user_space,
                    parent.as_deref(),
                    None,
                    None,
                )
                .await?,
            ));
        }
        Ok(render(&Value::Array(created)))
    }

    async fn delete_page(
        &self,
        page_id: Option<&str>,
        page_title: Option<&str>,
    ) -> Result<String, ConfluenceClientError> {
        let resolved = match (page_id, page_title) {
            (None, None) => {
                return Ok(
                    "Either page_id or page_title is required to delete the page".to_owned(),
                );
            }
            (Some(page_id), _) => Some(page_id.to_owned()),
            (None, Some(title)) => {
                match self
                    .page_by_title(self.config.space(), title, "page")
                    .await?
                {
                    Ok(page) => page.and_then(|page| id_text(page.get("id"))),
                    Err(message) => return Ok(api_error(&message)),
                }
            }
        };
        let Some(resolved) = resolved else {
            return Ok(format!(
                "Page instance could not be resolved with id '{}' and/or title '{}'",
                page_id.unwrap_or("None"),
                page_title.unwrap_or("None")
            ));
        };
        validate_identifier(&resolved)?;
        match self
            .send(
                Method::DELETE,
                self.v1(&["content", &resolved])?,
                None,
                true,
            )
            .await?
        {
            Reply::Accepted(_) => Ok(format!(
                "Page with ID '{resolved}' has been successfully deleted."
            )),
            Reply::Refused(_, message) => Ok(api_error(&message)),
        }
    }

    #[allow(clippy::too_many_lines)] // The SDK's update, version, label and detail steps in order.
    async fn update_page_by_id(
        &self,
        page_id: &str,
        representation: Option<&str>,
        new_title: Option<&str>,
        new_body: Option<&str>,
        new_labels: Option<&[String]>,
    ) -> Result<String, ConfluenceClientError> {
        validate_identifier(page_id)?;
        let mut url = self.v1(&["content", page_id])?;
        url.query_pairs_mut()
            .append_pair("expand", "version,body.storage,space");
        let current = match self.get(url).await? {
            Reply::Accepted(current) if current.is_object() => current,
            Reply::Accepted(_) | Reply::Refused(StatusCode::NOT_FOUND, _) => {
                return Ok(format!("Page with ID {page_id} not found."));
            }
            Reply::Refused(_, message) => return Ok(api_error(&message)),
        };
        let current_title = current
            .get("title")
            .and_then(Value::as_str)
            .unwrap_or_default();
        if let Some(new_title) = new_title
            && new_title != current_title
        {
            match self
                .page_by_title(self.config.space(), new_title, "page")
                .await?
            {
                Ok(Some(_)) => return Ok(format!("Page with title {new_title} already exists.")),
                Ok(None) => {}
                Err(message) => return Ok(api_error(&message)),
            }
        }
        let current_version = current
            .pointer("/version/number")
            .and_then(Value::as_u64)
            .ok_or_else(invalid_response)?;
        let title = new_title.unwrap_or(current_title).to_owned();
        let current_storage = current
            .pointer("/body/storage/value")
            .and_then(Value::as_str)
            .unwrap_or_default();
        // Without a new body the page keeps its own STORAGE content. The SDK
        // resent the rendered `view` HTML as storage, which flattens macros.
        let (body, representation) = match new_body {
            Some(body) => (body, representation.unwrap_or("storage")),
            None => (current_storage, "storage"),
        };
        if !V1_REPRESENTATIONS.contains(&representation) {
            return Ok(
                "Wrong value for representation, it should be either wiki or storage".to_owned(),
            );
        }
        // `is_page_content_is_already_updated`: same title and same storage
        // body means no new version.
        let unchanged = title == current_title
            && current_storage.trim().to_lowercase() == body.trim().to_lowercase();
        let updated = if unchanged {
            current.clone()
        } else {
            let history = match self.get(self.v1(&["content", page_id, "history"])?).await? {
                Reply::Accepted(history) => history,
                Reply::Refused(_, message) => return Ok(api_error(&message)),
            };
            let version = history
                .pointer("/lastUpdated/number")
                .and_then(Value::as_u64)
                .unwrap_or(current_version)
                .saturating_add(1);
            let payload = json!({
                "id": page_id,
                "type": "page",
                "title": title,
                "version": {"number": version, "minorEdit": false},
                "metadata": {"properties": fixed_width()},
                "body": {representation: {"value": body, "representation": representation}},
            });
            let mut url = self.v1(&["content", page_id])?;
            url.query_pairs_mut().append_pair("status", "current");
            match self.send(Method::PUT, url, Some(&payload), true).await? {
                Reply::Accepted(updated) if updated.is_object() => updated,
                Reply::Accepted(_) => return Err(unknown_outcome()),
                Reply::Refused(_, message) => return Ok(api_error(&message)),
            }
        };
        let webui_link = self.page_url(
            updated
                .pointer("/_links/webui")
                .and_then(Value::as_str)
                .unwrap_or_default(),
        );
        let next_version = updated
            .pointer("/version/number")
            .and_then(Value::as_u64)
            .unwrap_or(current_version);
        let links_base = updated
            .pointer("/_links/base")
            .and_then(Value::as_str)
            .unwrap_or_default();
        let diff_link = format!(
            "{links_base}/pages/diffpagesbyversion.action?pageId={page_id}&selectedPageVersions={current_version}&selectedPageVersions={next_version}"
        );
        let mut details = Map::new();
        details.insert(
            "title".into(),
            updated.get("title").cloned().unwrap_or(Value::Null),
        );
        details.insert(
            "id".into(),
            updated.get("id").cloned().unwrap_or(Value::Null),
        );
        details.insert(
            "space key".into(),
            updated
                .pointer("/space/key")
                .or_else(|| current.pointer("/space/key"))
                .cloned()
                .unwrap_or(Value::Null),
        );
        details.insert(
            "author".into(),
            updated
                .pointer("/version/by/displayName")
                .cloned()
                .unwrap_or(Value::Null),
        );
        details.insert("link".into(), Value::String(webui_link.clone()));
        details.insert("version".into(), Value::from(next_version));
        details.insert("diff".into(), Value::String(diff_link));
        let mut warnings = String::new();
        if let Some(new_labels) = new_labels {
            if new_labels.len() > MAX_LABELS {
                return Err(resource_exhausted());
            }
            match self.get(self.v1(&["content", page_id, "label"])?).await? {
                Reply::Accepted(labels) => {
                    for name in labels
                        .get("results")
                        .and_then(Value::as_array)
                        .into_iter()
                        .flatten()
                        .filter_map(|label| label.get("name").and_then(Value::as_str))
                    {
                        let mut url = self.v1(&["content", page_id, "label"])?;
                        url.query_pairs_mut()
                            .append_pair("id", page_id)
                            .append_pair("name", name);
                        if let Reply::Refused(_, message) =
                            self.send(Method::DELETE, url, None, true).await?
                        {
                            warnings.push_str("\nWarning: label '");
                            warnings.push_str(name);
                            warnings.push_str("' was not removed: ");
                            warnings.push_str(&message);
                        }
                    }
                }
                Reply::Refused(_, message) => {
                    warnings.push_str("\nWarning: the current labels could not be read: ");
                    warnings.push_str(&message);
                }
            }
            for label in new_labels {
                if let Some(failure) = self.set_label(page_id, label).await? {
                    warnings.push_str("\nWarning: label '");
                    warnings.push_str(label);
                    warnings.push_str("' was not added: ");
                    warnings.push_str(&failure);
                }
            }
            details.insert(
                "labels".into(),
                Value::Array(new_labels.iter().cloned().map(Value::String).collect()),
            );
        }
        warnings.push_str(&self.default_labels(page_id).await?);
        Ok(format!(
            "The page '{page_id}' was updated successfully: '{webui_link}'. \nDetails: {}{warnings}",
            render(&Value::Object(details))
        ))
    }

    async fn update_page_by_title(
        &self,
        page_title: &str,
        representation: Option<&str>,
        new_title: Option<&str>,
        new_body: Option<&str>,
        new_labels: Option<&[String]>,
    ) -> Result<String, ConfluenceClientError> {
        let page = match self
            .page_by_title(self.config.space(), page_title, "page")
            .await?
        {
            Ok(page) => page,
            Err(message) => return Ok(api_error(&message)),
        };
        let Some(page_id) = page.and_then(|page| id_text(page.get("id"))) else {
            return Ok(format!("Page with title {page_title} not found."));
        };
        self.update_page_by_id(&page_id, representation, new_title, new_body, new_labels)
            .await
    }

    async fn update_pages(
        &self,
        page_ids: Option<&[String]>,
        new_contents: Option<&[String]>,
        new_labels: Option<&[String]>,
    ) -> Result<String, ConfluenceClientError> {
        let page_ids = page_ids.unwrap_or_default();
        let new_contents = new_contents.unwrap_or_default();
        if page_ids.len() != new_contents.len() && new_contents.len() != 1 {
            return Ok(
                "New content should be provided for all the pages or it should contain only 1 new body for bulk update"
                    .to_owned(),
            );
        }
        if page_ids.is_empty() {
            return Ok(
                "Either list of page_ids or parent_id (to update descendants) should be provided."
                    .to_owned(),
            );
        }
        if page_ids.len() > MAX_BATCH_PAGES {
            return Err(resource_exhausted());
        }
        let mut statuses = Vec::with_capacity(page_ids.len());
        for (index, page_id) in page_ids.iter().enumerate() {
            let body = if new_contents.len() == 1 {
                &new_contents[0]
            } else {
                &new_contents[index]
            };
            statuses.push(Value::String(
                self.update_page_by_id(page_id, None, None, Some(body), new_labels)
                    .await?,
            ));
        }
        Ok(render(&Value::Array(statuses)))
    }

    async fn update_labels(
        &self,
        page_ids: Option<&[String]>,
        new_labels: Option<&[String]>,
    ) -> Result<String, ConfluenceClientError> {
        let Some(page_ids) = page_ids.filter(|ids| !ids.is_empty()) else {
            return Ok("Either list of page_ids should be provided.".to_owned());
        };
        if page_ids.len() > MAX_BATCH_PAGES {
            return Err(resource_exhausted());
        }
        let mut statuses = Vec::with_capacity(page_ids.len());
        for page_id in page_ids {
            statuses.push(Value::String(
                self.update_page_by_id(page_id, None, None, None, new_labels)
                    .await?,
            ));
        }
        Ok(render(&Value::Array(statuses)))
    }

    /// `get_all_descendants`, pre-order, as `{id: [title, parent_id]}`.
    async fn get_page_tree(&self, page_id: &str) -> Result<String, ConfluenceClientError> {
        validate_identifier(page_id)?;
        let mut descendants = Map::new();
        let root = match self.children(page_id).await? {
            Ok(children) => children,
            Err(message) => return Ok(api_error(&message)),
        };
        let mut stack: Vec<(String, VecDeque<(String, Value)>)> = vec![(page_id.to_owned(), root)];
        while let Some((parent, queue)) = stack.last_mut() {
            let Some((id, title)) = queue.pop_front() else {
                stack.pop();
                continue;
            };
            let parent = parent.clone();
            descendants.insert(id.clone(), json!([title, parent]));
            if descendants.len() > MAX_TREE_PAGES {
                return Err(resource_exhausted());
            }
            if stack.len() < MAX_TREE_DEPTH {
                let children = match self.children(&id).await? {
                    Ok(children) => children,
                    Err(message) => return Ok(api_error(&message)),
                };
                stack.push((id, children));
            }
        }
        Ok(format!(
            "The list of pages under the '{page_id}' was extracted: {}",
            render(&Value::Object(descendants))
        ))
    }

    async fn children(
        &self,
        page_id: &str,
    ) -> Result<Result<VecDeque<(String, Value)>, String>, ConfluenceClientError> {
        validate_identifier(page_id)?;
        let mut children = VecDeque::new();
        let mut start = 0_usize;
        loop {
            let mut url = self.v1(&["content", page_id, "child", "page"])?;
            url.query_pairs_mut()
                .append_pair("start", &start.to_string())
                .append_pair("limit", &CHILD_PAGE_SIZE.to_string());
            let page = match self.get(url).await? {
                Reply::Accepted(page) => page,
                Reply::Refused(_, message) => return Ok(Err(message)),
            };
            let results = page
                .get("results")
                .and_then(Value::as_array)
                .cloned()
                .unwrap_or_default();
            let count = results.len();
            for child in results {
                if let Some(id) = id_text(child.get("id")) {
                    children.push_back((id, child.get("title").cloned().unwrap_or(Value::Null)));
                }
            }
            if count < CHILD_PAGE_SIZE || children.len() > MAX_TREE_PAGES {
                return Ok(Ok(children));
            }
            start += CHILD_PAGE_SIZE;
        }
    }

    /// `get_pages_by_id` + `process_page` for one page.
    async fn page_document(
        &self,
        page_id: &str,
        format: ContentFormat,
        skip_images: bool,
    ) -> Result<Result<PageDocument, String>, ConfluenceClientError> {
        validate_identifier(page_id)?;
        let mut url = self.v1(&["content", page_id])?;
        url.query_pairs_mut()
            .append_pair("expand", &format!("{},version", format.expand()));
        let page = match self.get(url).await? {
            Reply::Accepted(page) if page.is_object() => page,
            Reply::Accepted(_) => return Err(invalid_response()),
            Reply::Refused(_, message) => return Ok(Err(message)),
        };
        let raw = page
            .pointer(&format!("/body/{}/value", format.key()))
            .and_then(Value::as_str)
            .unwrap_or_default();
        let content = if format == ContentFormat::AtlasDocFormat {
            raw.to_owned()
        } else {
            markdown(raw)
        };
        let content = if skip_images {
            strip_base64_images(&content)
        } else {
            content
        };
        Ok(Ok(PageDocument {
            id: page.get("id").cloned().unwrap_or(Value::Null),
            title: page.get("title").cloned().unwrap_or(Value::Null),
            source: self.page_url(
                page.pointer("/_links/webui")
                    .and_then(Value::as_str)
                    .unwrap_or_default(),
            ),
            content,
        }))
    }

    async fn read_page_by_id(
        &self,
        page_id: &str,
        skip_images: bool,
        content_format: Option<&str>,
    ) -> Result<String, ConfluenceClientError> {
        let format = match content_format {
            None => ContentFormat::View,
            Some(name) => ContentFormat::parse(name).unwrap_or(ContentFormat::View),
        };
        Ok(
            match self.page_document(page_id, format, skip_images).await? {
                Ok(page) => page.content,
                Err(message) => format!(
                    "Pages not found. Errors: {}",
                    render(&json!([format!(
                        "Confluence API Error: cannot fetch the page with ID {page_id}: {message}"
                    )]))
                ),
            },
        )
    }

    /// `_process_search`: CQL pages of `limit`, each page read and rendered.
    async fn process_search(
        &self,
        cql: &str,
        skip_images: bool,
    ) -> Result<String, ConfluenceClientError> {
        let limit = self.config.limit();
        let iterations = self.config.max_pages().div_ceil(limit);
        let mut seen = HashSet::new();
        let mut pages = Vec::new();
        let mut start = 0_usize;
        for _ in 0..iterations {
            let results = match self.cql(cql, start, limit).await? {
                Ok(results) => results,
                Err(message) => return Ok(api_error(&message)),
            };
            if results.is_empty() {
                break;
            }
            let ids = results
                .iter()
                .filter_map(|result| id_text(result.pointer("/content/id")))
                .filter(|id| seen.insert(id.clone()))
                .collect::<Vec<_>>();
            if ids.is_empty() {
                break;
            }
            for id in ids {
                // The SDK stopped the whole batch at the first unreadable
                // page; Rust skips only that page.
                if let Ok(page) = self
                    .page_document(&id, ContentFormat::View, skip_images)
                    .await?
                {
                    pages.push(json!({
                        "content": page.content,
                        "page_id": page.id,
                        "page_title": page.title,
                        "page_url": page.source,
                    }));
                }
            }
            start += limit;
        }
        Ok(if pages.is_empty() {
            format!("Unable to find anything using query {cql}. Check space or query.")
        } else {
            render(&Value::Array(pages))
        })
    }

    async fn cql(
        &self,
        cql: &str,
        start: usize,
        limit: usize,
    ) -> Result<Result<Vec<Value>, String>, ConfluenceClientError> {
        let mut url = self.v1(&["search"])?;
        url.query_pairs_mut()
            .append_pair("start", &start.to_string())
            .append_pair("limit", &limit.to_string())
            .append_pair("cql", cql);
        Ok(match self.get(url).await? {
            Reply::Accepted(document) => Ok(document
                .get("results")
                .and_then(Value::as_array)
                .cloned()
                .unwrap_or_default()),
            Reply::Refused(_, message) => Err(message),
        })
    }

    /// `__sanitize_confluence_space`: unquote '...' or `...`, then wrap in
    /// double quotes.
    fn space_clause(&self) -> Option<String> {
        let space = self.config.space()?;
        let unquoted = [('\'', '\''), ('`', '`')]
            .iter()
            .find_map(|(open, close)| {
                space
                    .strip_prefix(*open)
                    .and_then(|rest| rest.strip_suffix(*close))
            })
            .unwrap_or(space);
        Some(
            if unquoted.starts_with('"') && unquoted.ends_with('"') && unquoted.len() > 1 {
                unquoted.to_owned()
            } else {
                format!("\"{unquoted}\"")
            },
        )
    }

    fn page_cql(&self, condition: &str) -> String {
        match self.space_clause() {
            None => format!("(type=page) and ({condition})"),
            Some(space) => format!("(type=page and space={space}) and ({condition})"),
        }
    }

    async fn labeled_page_ids(
        &self,
        label: &str,
    ) -> Result<Result<Vec<(String, Value)>, String>, ConfluenceClientError> {
        let limit = self.config.limit();
        let max_pages = self.config.max_pages();
        let max_iterations = max_pages.div_ceil(limit) + 1;
        let cql = format!("type=page AND label=\"{}\"", escape_cql(label));
        let mut seen = HashSet::new();
        let mut pages = Vec::new();
        let mut start = 0_usize;
        for _ in 0..max_iterations {
            if pages.len() >= max_pages {
                break;
            }
            let mut url = self.v1(&["content", "search"])?;
            {
                let mut query = url.query_pairs_mut();
                query.append_pair("cql", &cql);
                if start > 0 {
                    query.append_pair("start", &start.to_string());
                }
                query.append_pair("limit", &limit.to_string());
            }
            let results = match self.get(url).await? {
                Reply::Accepted(document) => document
                    .get("results")
                    .and_then(Value::as_array)
                    .cloned()
                    .unwrap_or_default(),
                Reply::Refused(_, message) => return Ok(Err(message)),
            };
            if results.is_empty() {
                break;
            }
            let mut new = false;
            for result in results {
                if let Some(id) = id_text(result.get("id"))
                    && seen.insert(id.clone())
                {
                    new = true;
                    if pages.len() < max_pages {
                        pages.push((id, result.get("title").cloned().unwrap_or(Value::Null)));
                    }
                }
            }
            if !new {
                break;
            }
            start += limit;
        }
        Ok(Ok(pages))
    }

    async fn get_pages_with_label(&self, label: &str) -> Result<String, ConfluenceClientError> {
        let ids = match self.labeled_page_ids(label).await? {
            Ok(ids) => ids,
            Err(message) => return Ok(api_error(&message)),
        };
        let mut pages = Vec::with_capacity(ids.len());
        for (id, _) in ids {
            if let Ok(page) = self.page_document(&id, ContentFormat::View, false).await? {
                pages.push(json!({
                    "page_id": page.id,
                    "page_title": page.title,
                    "page_url": page.source,
                    "content": page.content,
                }));
            }
        }
        Ok(render(&Value::Array(pages)))
    }

    /// The SDK read every page body just to keep its title; the search
    /// result already carries it.
    async fn list_pages_with_label(&self, label: &str) -> Result<String, ConfluenceClientError> {
        Ok(match self.labeled_page_ids(label).await? {
            Ok(ids) => render(&Value::Array(
                ids.into_iter()
                    .map(|(id, title)| json!({"id": id, "title": title}))
                    .collect(),
            )),
            Err(message) => api_error(&message),
        })
    }

    async fn site_search(&self, query: &str) -> Result<String, ConfluenceClientError> {
        let escaped = escape_cql(query);
        let cql = self.page_cql(&format!("siteSearch~\"{escaped}\""));
        let results = match self.cql(&cql, 0, 10).await? {
            Ok(results) => results,
            Err(message) => return Ok(api_error(&message)),
        };
        if results.is_empty() {
            return Ok(format!("Unable to find anything using query {escaped}"));
        }
        Ok(results
            .iter()
            .map(|page| {
                render(&json!({
                    "page_id": page.pointer("/content/id").cloned().unwrap_or(Value::Null),
                    "page_title": page.pointer("/content/title").cloned().unwrap_or(Value::Null),
                    "page_url": page.pointer("/content/_links/self").cloned().unwrap_or(Value::Null),
                    "preview": page.get("excerpt").filter(|excerpt| truthy(excerpt)).cloned()
                        .unwrap_or_else(|| Value::String(String::new())),
                }))
            })
            .collect::<Vec<_>>()
            .join("---"))
    }

    async fn get_page_id_by_title(
        &self,
        title: &str,
        page_type: Option<&str>,
    ) -> Result<String, ConfluenceClientError> {
        Ok(
            match self
                .page_by_title(self.config.space(), title, page_type.unwrap_or("page"))
                .await?
            {
                Ok(page) => page
                    .and_then(|page| id_text(page.get("id")))
                    .unwrap_or_else(|| "Page not found. Check the title or space.".to_owned()),
                Err(message) => api_error(&message),
            },
        )
    }

    async fn execute_generic(
        &self,
        method: &str,
        relative_url: &str,
        params: Option<&str>,
    ) -> Result<(String, bool), ConfluenceClientError> {
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
                return Ok((
                    format!(
                        "Confluence tool exception. Unsupported HTTP method '{method}'; use GET, POST, PUT, PATCH or DELETE."
                    ),
                    false,
                ));
            }
        };
        let Some(mut url) = self.relative_url(relative_url) else {
            return Ok((
                "Confluence tool exception. relative_url must be a path on this Confluence instance that starts with '/', for example '/rest/api/content/123', without a query string, fragment or '..' segment."
                    .to_owned(),
                false,
            ));
        };
        let payload = match params.filter(|params| !params.trim().is_empty()) {
            None => Map::new(),
            Some(params) => match serde_json::from_str::<Value>(params) {
                Ok(Value::Object(payload)) => payload,
                Ok(_) => {
                    return Ok((
                        "Confluence tool exception. Passed params must be a JSON object."
                            .to_owned(),
                        false,
                    ));
                }
                Err(error) => {
                    return Ok((
                        format!(
                            "Confluence tool exception. Passed params are not valid JSON. {error}"
                        ),
                        false,
                    ));
                }
            },
        };
        let body = if method_name == "GET" {
            append_query(&mut url, &payload);
            None
        } else {
            Some(Value::Object(payload))
        };
        let request = self.request(http_method, url, body.as_ref())?;
        let response = self.transport.execute(request, effect).await?;
        let status = response.status;
        // The SDK calls this in the client's advanced mode, which raises on
        // nothing: every status comes back as text. Only an effect's 5xx is
        // held back, because it may have been applied.
        if effect && status.is_server_error() {
            return Err(unknown_outcome());
        }
        let text = String::from_utf8_lossy(&response.body).into_owned();
        let text = if method_name == "GET" && is_content_path(relative_url) {
            markdown(&text)
        } else {
            text
        };
        Ok((
            format!(
                "HTTP: {method}{relative_url} -> {}{}{text}",
                status.as_u16(),
                status.canonical_reason().unwrap_or_default()
            ),
            effect,
        ))
    }

    /// A model-supplied path joined onto the client root (the SDK's
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
        let root = self.config.api_root();
        let url = Url::parse(&format!(
            "{}{relative_url}",
            root.as_str().trim_end_matches('/')
        ))
        .ok()?;
        (url.origin() == root.origin() && url.path().starts_with(root.path().trim_end_matches('/')))
            .then_some(url)
    }
}

#[async_trait]
impl ConfluenceApi for ConfluenceClient {
    #[allow(clippy::too_many_lines)] // One arm per SDK tool.
    async fn execute(
        &self,
        operation: ConfluenceOperation<'_>,
    ) -> Result<Value, ConfluenceClientError> {
        let (output, effect) = match operation {
            ConfluenceOperation::CreatePage {
                title,
                body,
                status,
                space,
                parent_id,
                representation,
                label,
            } => (
                self.create_page(title, body, status, space, parent_id, representation, label)
                    .await?,
                true,
            ),
            ConfluenceOperation::CreatePages {
                pages_info,
                status,
                space,
                parent_id,
            } => (
                self.create_pages(pages_info, status, space, parent_id)
                    .await?,
                true,
            ),
            ConfluenceOperation::DeletePage {
                page_id,
                page_title,
            } => (self.delete_page(page_id, page_title).await?, true),
            ConfluenceOperation::UpdatePageById {
                page_id,
                representation,
                new_title,
                new_body,
                new_labels,
            } => (
                self.update_page_by_id(
                    page_id,
                    representation,
                    new_title,
                    new_body,
                    new_labels.as_deref(),
                )
                .await?,
                true,
            ),
            ConfluenceOperation::UpdatePageByTitle {
                page_title,
                representation,
                new_title,
                new_body,
                new_labels,
            } => (
                self.update_page_by_title(
                    page_title,
                    representation,
                    new_title,
                    new_body,
                    new_labels.as_deref(),
                )
                .await?,
                true,
            ),
            ConfluenceOperation::UpdatePages {
                page_ids,
                new_contents,
                new_labels,
            } => (
                self.update_pages(
                    page_ids.as_deref(),
                    new_contents.as_deref(),
                    new_labels.as_deref(),
                )
                .await?,
                true,
            ),
            ConfluenceOperation::UpdateLabels {
                page_ids,
                new_labels,
            } => (
                self.update_labels(page_ids.as_deref(), new_labels.as_deref())
                    .await?,
                true,
            ),
            ConfluenceOperation::GetPageTree { page_id } => {
                (self.get_page_tree(page_id).await?, false)
            }
            ConfluenceOperation::GetPagesWithLabel { label } => {
                (self.get_pages_with_label(label).await?, false)
            }
            ConfluenceOperation::ListPagesWithLabel { label } => {
                (self.list_pages_with_label(label).await?, false)
            }
            ConfluenceOperation::ReadPageById {
                page_id,
                skip_images,
                content_format,
            } => (
                self.read_page_by_id(page_id, skip_images, content_format)
                    .await?,
                false,
            ),
            ConfluenceOperation::SearchPages { query, skip_images } => {
                if query.is_empty() {
                    (
                        "Search query text is empty. Query parameter is required for Confluence search."
                            .to_owned(),
                        false,
                    )
                } else {
                    let escaped = escape_cql(query);
                    let cql = self.page_cql(&format!("title~\"{escaped}\" or text~\"{escaped}\""));
                    (self.process_search(&cql, skip_images).await?, false)
                }
            }
            ConfluenceOperation::SearchByTitle { query, skip_images } => {
                let cql = self.page_cql(&format!("title~\"{}\"", escape_cql(query)));
                (self.process_search(&cql, skip_images).await?, false)
            }
            ConfluenceOperation::SiteSearch { query } => (self.site_search(query).await?, false),
            ConfluenceOperation::ExecuteGenericConfluence {
                method,
                relative_url,
                params,
            } => self.execute_generic(method, relative_url, params).await?,
            ConfluenceOperation::GetPageIdByTitle { title, page_type } => {
                (self.get_page_id_by_title(title, page_type).await?, false)
            }
        };
        text(output, effect)
    }
}

/// The page body formats `read_page_by_id` can expand.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum ContentFormat {
    View,
    Storage,
    ExportView,
    Editor,
    Anonymous,
    StyledView,
    AtlasDocFormat,
}

impl ContentFormat {
    fn parse(name: &str) -> Option<Self> {
        Some(match name.to_ascii_lowercase().as_str() {
            "view" => Self::View,
            "storage" => Self::Storage,
            "export_view" => Self::ExportView,
            "editor" => Self::Editor,
            "anonymous" => Self::Anonymous,
            "styled_view" => Self::StyledView,
            "atlas_doc_format" => Self::AtlasDocFormat,
            _ => return None,
        })
    }

    /// The `body.<key>` the format reads.
    const fn key(self) -> &'static str {
        match self {
            Self::View => "view",
            Self::Storage => "storage",
            Self::ExportView => "export_view",
            Self::Editor => "editor",
            Self::Anonymous => "anonymous_export_view",
            Self::StyledView => "styled_view",
            Self::AtlasDocFormat => "atlas_doc_format",
        }
    }

    fn expand(self) -> String {
        format!("body.{}", self.key())
    }
}

/// `markdownify(content, heading_style="ATX")`.
fn markdown(html: &str) -> String {
    let options = ConversionOptions {
        extract_metadata: false,
        preprocessing: PreprocessingOptions {
            enabled: false,
            ..PreprocessingOptions::default()
        },
        ..ConversionOptions::default()
    };
    html_to_markdown_rs::convert(html, options)
        .ok()
        .and_then(|result| result.content)
        .unwrap_or_else(|| html.to_owned())
}

/// `_strip_base64_images`.
fn strip_base64_images(content: &str) -> String {
    const MARKER: &str = "data:image/";
    let mut out = String::with_capacity(content.len());
    let mut rest = content;
    while let Some(position) = rest.find(MARKER) {
        out.push_str(&rest[..position]);
        let candidate = &rest[position + MARKER.len()..];
        let matched = ["png", "jpeg", "gif"].iter().find_map(|kind| {
            candidate
                .strip_prefix(kind)
                .and_then(|tail| tail.strip_prefix(";base64,"))
        });
        if let Some(tail) = matched {
            let data = tail
                .bytes()
                .take_while(|byte| {
                    byte.is_ascii_alphanumeric() || matches!(byte, b'+' | b'/' | b'=')
                })
                .count();
            if data == 0 {
                out.push_str(MARKER);
                rest = candidate;
            } else {
                out.push_str("[Image Removed]");
                rest = &tail[data..];
            }
        } else {
            out.push_str(MARKER);
            rest = candidate;
        }
    }
    out.push_str(rest);
    out
}

fn is_content_path(relative_url: &str) -> bool {
    // `re.match(r'/rest/api/content/\d+', relative_url)`
    relative_url
        .strip_prefix("/rest/api/content/")
        .is_some_and(|rest| {
            rest.bytes()
                .next()
                .is_some_and(|byte| byte.is_ascii_digit())
        })
}

/// CQL string delimiters only (`_escape_cql_query`).
fn escape_cql(value: &str) -> String {
    value.replace('\\', "\\\\").replace('"', "\\\"")
}

fn fixed_width() -> Value {
    json!({
        "content-appearance-draft": {"value": "fixed-width"},
        "content-appearance-published": {"value": "fixed-width"},
    })
}

fn classify(
    response: &ConfluenceHttpResponse,
    effect: bool,
) -> Result<Reply, ConfluenceClientError> {
    let status = response.status;
    if status.is_success() {
        let value = if response.body.is_empty() || !response.json_content_type {
            Value::Null
        } else {
            response
                .json()
                .ok_or_else(|| response_shape_failure(effect))?
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
        status if status.is_client_error() => {
            Ok(Reply::Refused(status, provider_message(response)))
        }
        _ => Err(response_shape_failure(effect)),
    }
}

/// Confluence's error text: v1 `message`, `errorMessages`, and v2
/// `errors[].title/detail`, joined after the status line.
fn provider_message(response: &ConfluenceHttpResponse) -> String {
    let mut messages: Vec<String> = Vec::new();
    if let Some(document) = response.json() {
        if let Some(message) = document.get("message").and_then(Value::as_str) {
            messages.push(message.to_owned());
        }
        if let Some(list) = document.get("errorMessages").and_then(Value::as_array) {
            messages.extend(list.iter().map(display_value));
        }
        if let Some(errors) = document.get("errors").and_then(Value::as_array) {
            messages.extend(errors.iter().map(|error| {
                error
                    .get("detail")
                    .or_else(|| error.get("title"))
                    .or_else(|| error.get("message"))
                    .map_or_else(|| display_value(error), display_value)
            }));
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

fn api_error(message: &str) -> String {
    format!("Confluence API error: {message}")
}

fn display_value(value: &Value) -> String {
    value
        .as_str()
        .map_or_else(|| value.to_string(), ToOwned::to_owned)
}

fn id_text(value: Option<&Value>) -> Option<String> {
    match value? {
        Value::String(id) if !id.is_empty() => Some(id.clone()),
        Value::Number(id) => Some(id.to_string()),
        _ => None,
    }
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

/// Structured values are compact JSON where the SDK printed a Python `repr`.
fn render(value: &Value) -> String {
    display_value(value)
}

fn validate_identifier(value: &str) -> Result<(), ConfluenceClientError> {
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
fn text(value: String, effect: bool) -> Result<Value, ConfluenceClientError> {
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

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> ConfluenceClientError {
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

const fn response_shape_failure(effect: bool) -> ConfluenceClientError {
    if effect {
        unknown_outcome()
    } else {
        invalid_response()
    }
}

const fn response_bound_failure(effect: bool) -> ConfluenceClientError {
    if effect {
        unknown_outcome()
    } else {
        resource_exhausted()
    }
}

const fn error(code: ConfluenceClientErrorCode, retryable: bool) -> ConfluenceClientError {
    ConfluenceClientError { code, retryable }
}

const fn invalid_configuration() -> ConfluenceClientError {
    error(ConfluenceClientErrorCode::InvalidConfiguration, false)
}

const fn invalid_input() -> ConfluenceClientError {
    error(ConfluenceClientErrorCode::InvalidInput, false)
}

const fn authentication() -> ConfluenceClientError {
    error(ConfluenceClientErrorCode::Authentication, false)
}

const fn rate_limited() -> ConfluenceClientError {
    error(ConfluenceClientErrorCode::RateLimited, true)
}

const fn timeout() -> ConfluenceClientError {
    error(ConfluenceClientErrorCode::Timeout, true)
}

const fn unavailable() -> ConfluenceClientError {
    error(ConfluenceClientErrorCode::DependencyUnavailable, true)
}

const fn invalid_response() -> ConfluenceClientError {
    error(ConfluenceClientErrorCode::InvalidResponse, false)
}

const fn resource_exhausted() -> ConfluenceClientError {
    error(ConfluenceClientErrorCode::ResourceExhausted, false)
}

const fn unknown_outcome() -> ConfluenceClientError {
    error(ConfluenceClientErrorCode::UnknownOutcome, false)
}
