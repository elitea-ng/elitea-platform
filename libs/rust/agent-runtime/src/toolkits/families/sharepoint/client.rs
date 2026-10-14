use std::collections::{BTreeSet, VecDeque};
use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use percent_encoding::percent_decode_str;
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderName, HeaderValue, LOCATION,
    USER_AGENT,
};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value, json};

use crate::toolkits::DelegatedAuthorizationRequirement;

use super::config::SharePointClientConfig;

mod content;
mod onenote_ops;

pub(in crate::toolkits) use content::{
    MAX_READ_BYTES, SharePointFile, SharePointUpload, names_no_file, not_a_file,
};

const GRAPH_BASE: &str = "https://graph.microsoft.com/v1.0/";
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
/// The most raw markup of a `OneNote` page the readers take (they cut the text
/// at their own limit; only a page over this is refused).
const MAX_PAGE_MARKUP_BYTES: usize = 8 * 1_024 * 1_024;
const MAX_PAGES: usize = 64;
const MAX_DRIVES: usize = 64;
const MAX_FOLDER_REQUESTS: usize = 512;
const MAX_LIST_ITEMS: usize = 1_000;
pub(in crate::toolkits) const MAX_FILES: usize = 1_000;
pub(in crate::toolkits) const MAX_ONENOTE_PAGES: usize = 100;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SharePointClientErrorCode {
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
    /// The request may have reached Graph, and the answer was lost.
    ConnectionLost,
    /// Graph refused a change because the target is in a conflicting state
    /// (a taken name, a locked file, a stale version).
    Conflict,
    /// A change was sent and Graph did not confirm it: it may or may not have
    /// been applied.
    OutcomeUnknown,
    /// A file this tool will not read; [`SharePointClientError::detail`] says
    /// which of its fixed reasons.
    FileRefused,
}

pub(crate) struct SharePointClientError {
    code: SharePointClientErrorCode,
    /// A fixed sentence for the one cause behind `code` (never data from the
    /// provider or the arguments).
    detail: Option<&'static str>,
}

impl SharePointClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> SharePointClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        matches!(
            self.code,
            SharePointClientErrorCode::RateLimited
                | SharePointClientErrorCode::Timeout
                | SharePointClientErrorCode::DependencyUnavailable
                | SharePointClientErrorCode::ConnectionLost
        )
    }

    /// This failure of a CHANGE: a request that may have reached Graph and
    /// got no answer (a timeout, a lost connection) leaves the outcome
    /// unknown, which is not a failure to retry.
    #[must_use]
    pub(crate) const fn for_effect(self) -> Self {
        match self.code {
            SharePointClientErrorCode::Timeout | SharePointClientErrorCode::ConnectionLost => {
                outcome_unknown()
            }
            _ => self,
        }
    }

    /// The one sentence for this cause, for a tool that tells the model
    /// itself (never data from the provider or the arguments).
    pub(crate) fn sentence(&self) -> &'static str {
        self.detail.unwrap_or_else(|| self.parts().2)
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = self.parts();
        let message = self.detail.unwrap_or(message);
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable(),
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    const fn parts(&self) -> (ErrorCategory, &'static str, &'static str) {
        match self.code {
            SharePointClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "sharepoint.configuration.invalid",
                "the SharePoint toolkit configuration is invalid",
            ),
            SharePointClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "sharepoint.request.invalid",
                "SharePoint rejected the request as invalid, for example a name or a value it does not accept",
            ),
            SharePointClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "sharepoint.authentication.failed",
                "SharePoint authentication failed",
            ),
            SharePointClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "sharepoint.authorization.failed",
                "SharePoint did not authorize the request",
            ),
            SharePointClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "sharepoint.resource.not_found",
                "the requested SharePoint resource was not found",
            ),
            SharePointClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "sharepoint.rate_limited",
                "Microsoft Graph rate limited the SharePoint request",
            ),
            SharePointClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "sharepoint.timeout",
                "the SharePoint request timed out",
            ),
            SharePointClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "sharepoint.unavailable",
                "Microsoft Graph is unavailable",
            ),
            SharePointClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "sharepoint.response.invalid",
                "Microsoft Graph returned an invalid SharePoint response",
            ),
            SharePointClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "sharepoint.resource_exhausted",
                "the SharePoint request or response exceeds its approved limit",
            ),
            SharePointClientErrorCode::ConnectionLost => (
                ErrorCategory::Unavailable,
                "sharepoint.connection_lost",
                "the connection to Microsoft Graph was lost before it answered",
            ),
            SharePointClientErrorCode::Conflict => (
                ErrorCategory::InvalidInput,
                "sharepoint.conflict",
                "SharePoint refused the change because it conflicts with the current state of the target, for example a name that is already taken",
            ),
            SharePointClientErrorCode::OutcomeUnknown => (
                ErrorCategory::Unavailable,
                "sharepoint.outcome_unknown",
                "SharePoint did not confirm the change: it may or may not have been applied, so check the target before repeating it",
            ),
            SharePointClientErrorCode::FileRefused => (
                ErrorCategory::InvalidInput,
                "sharepoint.file.refused",
                "this tool does not read that file",
            ),
        }
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(code: SharePointClientErrorCode) -> Self {
        Self { code, detail: None }
    }
}

impl fmt::Debug for SharePointClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SharePointClientError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for SharePointClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            SharePointClientErrorCode::InvalidConfiguration => {
                "the SharePoint client configuration is invalid"
            }
            SharePointClientErrorCode::InvalidInput => "the SharePoint request is invalid",
            SharePointClientErrorCode::Authentication => "SharePoint authentication failed",
            SharePointClientErrorCode::Authorization => "SharePoint authorization failed",
            SharePointClientErrorCode::NotFound => "the SharePoint resource was not found",
            SharePointClientErrorCode::RateLimited => {
                "Microsoft Graph rate limited the SharePoint request"
            }
            SharePointClientErrorCode::Timeout => "the SharePoint request timed out",
            SharePointClientErrorCode::DependencyUnavailable => "Microsoft Graph is unavailable",
            SharePointClientErrorCode::InvalidResponse => {
                "Microsoft Graph returned an invalid SharePoint response"
            }
            SharePointClientErrorCode::ResourceExhausted => {
                "the SharePoint request or response exceeds its approved limit"
            }
            SharePointClientErrorCode::ConnectionLost => {
                "the connection to Microsoft Graph was lost"
            }
            SharePointClientErrorCode::Conflict => "SharePoint reported a conflicting state",
            SharePointClientErrorCode::OutcomeUnknown => "SharePoint did not confirm the change",
            SharePointClientErrorCode::FileRefused => "the file is not read by this tool",
        })
    }
}

impl std::error::Error for SharePointClientError {}

#[derive(Clone)]
pub(in crate::toolkits) struct SharePointFileListOptions {
    pub(in crate::toolkits) folder_name: Option<String>,
    pub(in crate::toolkits) form_name: Option<String>,
    pub(in crate::toolkits) limit: usize,
    pub(in crate::toolkits) include_patterns: Vec<String>,
    pub(in crate::toolkits) skip_patterns: Vec<String>,
}

#[async_trait]
pub(in crate::toolkits) trait SharePointApi: Send + Sync {
    fn authorization(&self) -> &DelegatedAuthorizationRequirement;

    async fn read_list(
        &self,
        list_title: &str,
        limit: usize,
    ) -> Result<Value, SharePointClientError>;
    async fn get_lists(&self) -> Result<Value, SharePointClientError>;
    async fn get_list_columns(&self, list_title: &str) -> Result<Value, SharePointClientError>;
    async fn get_files_list(
        &self,
        options: SharePointFileListOptions,
    ) -> Result<Value, SharePointClientError>;
    async fn onenote_get_notebooks(
        &self,
        select: Option<&[String]>,
    ) -> Result<Value, SharePointClientError>;
    async fn onenote_get_sections(
        &self,
        notebook_id: &str,
        select: Option<&[String]>,
    ) -> Result<Value, SharePointClientError>;
    async fn onenote_get_pages(
        &self,
        section_id: &str,
        limit: usize,
        select: Option<&[String]>,
    ) -> Result<Value, SharePointClientError>;
    async fn onenote_get_page_content(&self, page_id: &str)
    -> Result<Value, SharePointClientError>;

    /// The page's markup for the readers, up to a raw cap well above what
    /// `onenote_get_page_content` hands the model; the reader cuts the TEXT
    /// at its own limit. Over the raw cap is the only refusal.
    async fn onenote_page_markup(&self, page_id: &str) -> Result<String, SharePointClientError>;

    /// A new item of one exact list: `{id, fields, webUrl}`.
    async fn create_list_item(
        &self,
        list_title: &str,
        fields: &Map<String, Value>,
    ) -> Result<Value, SharePointClientError>;
    /// One file into a document library folder: `{id, webUrl, path, size,
    /// mime_type}`.
    async fn upload_file(&self, upload: SharePointUpload) -> Result<Value, SharePointClientError>;
    /// The bytes of one file by server-relative or Graph drive path, bounded
    /// by `max_bytes` before they are buffered.
    async fn download_document(
        &self,
        path: &str,
        max_bytes: usize,
    ) -> Result<SharePointFile, SharePointClientError>;
    /// The bytes of the file a sharing link names, resolved by Graph (the
    /// link itself is never fetched).
    async fn download_shared_file(
        &self,
        sharing_url: &str,
        readable: &ReadableCheck,
    ) -> Result<SharePointFile, SharePointClientError>;
    /// One `OneNote` embedded resource (an image or an attachment) of this
    /// site, by its resource id.
    async fn onenote_download_resource(
        &self,
        resource_id: &str,
        max_bytes: usize,
    ) -> Result<Vec<u8>, SharePointClientError>;
    /// The canonical content address of an embedded `OneNote` resource named by
    /// the address a page carries (shown to the model; never fetched from
    /// here). An address that is no `OneNote` resource comes back unchanged.
    async fn onenote_resource_url(&self, src: &str) -> Result<String, SharePointClientError>;
    /// The resource id inside an address a page carries, only when that
    /// address is a Graph `OneNote` resource of THIS site; `None` for anything
    /// else (a CDN picture, another site's resource), which is never fetched.
    async fn onenote_resource_id(&self, src: &str)
    -> Result<Option<String>, SharePointClientError>;
    async fn onenote_create_notebook(
        &self,
        display_name: &str,
    ) -> Result<Value, SharePointClientError>;
    async fn onenote_create_section(
        &self,
        notebook_id: &str,
        display_name: &str,
    ) -> Result<Value, SharePointClientError>;
    async fn onenote_create_page(
        &self,
        section_id: &str,
        html_content: &str,
    ) -> Result<Value, SharePointClientError>;
    async fn onenote_update_page(
        &self,
        page_id: &str,
        patch_commands: &[Value],
    ) -> Result<(), SharePointClientError>;
    async fn onenote_delete_page(&self, page_id: &str) -> Result<(), SharePointClientError>;
}

/// Whether the host can read a file of this name: `None`, or the sentence of
/// the refusal. Explicitly higher-ranked (an elided lifetime would be bound
/// to the method by `async_trait`).
pub(in crate::toolkits) type ReadableCheck =
    dyn for<'name> Fn(&'name str) -> Option<&'static str> + Send + Sync;

pub(in crate::toolkits) struct SharePointHttpResponse {
    pub(in crate::toolkits) status: StatusCode,
    pub(in crate::toolkits) content_type: Option<Box<str>>,
    /// The `Location` of a redirect (a drive item's content is one).
    pub(in crate::toolkits) location: Option<Box<str>>,
    /// The answer was larger than the cap and its body was not buffered
    /// (`body` is empty). The status is still true: a caller that changed
    /// something reads it before it reads the size.
    pub(in crate::toolkits) oversize: bool,
    /// The status arrived and the body did not (a stalled or dropped
    /// connection): `Timeout` or `ConnectionLost`. The status is still true;
    /// what the caller makes of it depends on whether it changed anything.
    pub(in crate::toolkits) body_lost: Option<SharePointClientErrorCode>,
    pub(in crate::toolkits) body: Vec<u8>,
}

#[async_trait]
pub(in crate::toolkits) trait SharePointTransport: Send + Sync {
    /// One request, its answer bounded at [`MAX_RESPONSE_BYTES`].
    async fn execute(
        &self,
        request: Request,
    ) -> Result<SharePointHttpResponse, SharePointClientError>;

    /// One request whose answer may be as large as `max_body` bytes (a file's
    /// content). The real transport refuses a larger answer before it
    /// buffers it; a transport that cannot bound a stream checks afterwards.
    async fn execute_bounded(
        &self,
        request: Request,
        max_body: usize,
    ) -> Result<SharePointHttpResponse, SharePointClientError> {
        let mut response = self.execute(request).await?;
        if response.body.len() > max_body {
            response.body = Vec::new();
            response.oversize = true;
        }
        Ok(response)
    }
}

struct ReqwestSharePointTransport {
    http: reqwest::Client,
}

#[async_trait]
impl SharePointTransport for ReqwestSharePointTransport {
    async fn execute(
        &self,
        request: Request,
    ) -> Result<SharePointHttpResponse, SharePointClientError> {
        self.execute_bounded(request, MAX_RESPONSE_BYTES).await
    }

    async fn execute_bounded(
        &self,
        request: Request,
        max_body: usize,
    ) -> Result<SharePointHttpResponse, SharePointClientError> {
        let mut response = self
            .http
            .execute(request)
            .await
            .map_err(|error| map_reqwest_error(&error))?;
        let declared_oversize = response
            .headers()
            .get(CONTENT_LENGTH)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.parse::<usize>().ok())
            .is_some_and(|length| length > max_body);
        let status = response.status();
        let content_type = response
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .map(Into::into);
        let location = response
            .headers()
            .get(LOCATION)
            .and_then(|value| value.to_str().ok())
            .filter(|value| value.len() <= 16 * 1_024)
            .map(Into::into);
        if declared_oversize {
            return Ok(SharePointHttpResponse {
                status,
                content_type,
                location,
                oversize: true,
                body_lost: None,
                body: Vec::new(),
            });
        }
        let mut body = Vec::new();
        // The status is known: a failure from here on is an answer that did
        // not arrive whole, not a request that never left. The status is kept
        // and handed back with the loss, so a change that was confirmed (2xx)
        // is not reported as lost and a refusal (4xx) stays a refusal.
        let mut body_lost = None;
        loop {
            match response.chunk().await {
                Ok(Some(chunk)) => {
                    let next = body.len().saturating_add(chunk.len());
                    if next > max_body {
                        return Ok(SharePointHttpResponse {
                            status,
                            content_type,
                            location,
                            oversize: true,
                            body_lost: None,
                            body: Vec::new(),
                        });
                    }
                    body.extend_from_slice(&chunk);
                }
                Ok(None) => break,
                Err(error) => {
                    body_lost = Some(if error.is_timeout() {
                        SharePointClientErrorCode::Timeout
                    } else {
                        SharePointClientErrorCode::ConnectionLost
                    });
                    body = Vec::new();
                    break;
                }
            }
        }
        Ok(SharePointHttpResponse {
            status,
            content_type,
            location,
            oversize: false,
            body_lost,
            body,
        })
    }
}

/// The one way this family builds its HTTP client. No redirect is followed
/// (a Graph content redirect is read and validated by hand), and reqwest's own
/// protocol-level retries are off: a change is sent exactly once, and a
/// request that may have reached Graph is never silently sent again.
fn base_client_builder() -> reqwest::ClientBuilder {
    reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .retry(reqwest::retry::never())
        .connect_timeout(CONNECT_TIMEOUT)
        .timeout(REQUEST_TIMEOUT)
        .pool_idle_timeout(POOL_IDLE_TIMEOUT)
        .pool_max_idle_per_host(MAX_IDLE_PER_HOST)
        .user_agent(USER_AGENT_VALUE)
}

/// A transport to a plain-HTTP server on this machine, for the tests that
/// need a real socket (size caps and failure causes at the boundary). It is
/// the production client but for TLS.
#[cfg(test)]
pub(in crate::toolkits) fn plain_http_transport() -> Arc<dyn SharePointTransport> {
    let http = base_client_builder()
        .no_proxy()
        .build()
        .expect("plain HTTP test client");
    Arc::new(ReqwestSharePointTransport { http })
}

pub(crate) struct SharePointClient {
    config: SharePointClientConfig,
    transport: Arc<dyn SharePointTransport>,
    site_id: tokio::sync::OnceCell<String>,
    drives: tokio::sync::OnceCell<Vec<Value>>,
    default_drive: tokio::sync::OnceCell<String>,
}

impl SharePointClient {
    pub(crate) fn new(config: SharePointClientConfig) -> Result<Self, SharePointClientError> {
        let http = base_client_builder()
            .https_only(true)
            .build()
            .map_err(|_| invalid_configuration())?;
        Ok(Self {
            config,
            transport: Arc::new(ReqwestSharePointTransport { http }),
            site_id: tokio::sync::OnceCell::new(),
            drives: tokio::sync::OnceCell::new(),
            default_drive: tokio::sync::OnceCell::new(),
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn with_transport(
        config: SharePointClientConfig,
        transport: Arc<dyn SharePointTransport>,
    ) -> Self {
        Self {
            config,
            transport,
            site_id: tokio::sync::OnceCell::new(),
            drives: tokio::sync::OnceCell::new(),
            default_drive: tokio::sync::OnceCell::new(),
        }
    }

    async fn resolve_site_id(&self) -> Result<&str, SharePointClientError> {
        self.site_id
            .get_or_try_init(|| async {
                let mut segments = vec!["sites", self.config.site_hostname.as_ref()];
                segments.extend(
                    self.config
                        .site_path
                        .split('/')
                        .filter(|segment| !segment.is_empty()),
                );
                let mut url = graph_url(&segments)?;
                let hostname_segment = format!("{}:", self.config.site_hostname);
                replace_path_segment(&mut url, 2, &hostname_segment)?;
                let object = self.get_json(url).await?;
                required_json_text(&object, "id", 4 * 1_024).map(str::to_owned)
            })
            .await
            .map(String::as_str)
    }

    async fn list_drives(&self) -> Result<&[Value], SharePointClientError> {
        self.drives
            .get_or_try_init(|| async {
                let site_id = self.resolve_site_id().await?;
                let mut url = graph_url(&["sites", site_id, "drives"])?;
                set_query(&mut url, &[("$select", "id,name,webUrl"), ("$top", "64")]);
                let object = self.get_json(url).await?;
                let values = required_values(&object)?;
                if values.len() > MAX_DRIVES {
                    return Err(resource_exhausted());
                }
                Ok(values.to_vec())
            })
            .await
            .map(Vec::as_slice)
    }

    async fn resolve_list_id(&self, list_title: &str) -> Result<String, SharePointClientError> {
        let site_id = self.resolve_site_id().await?;
        let mut url = graph_url(&["sites", site_id, "lists"])?;
        set_query(
            &mut url,
            &[("$select", "id,displayName,name"), ("$top", "999")],
        );
        let object = self.get_json(url).await?;
        for list in required_values(&object)? {
            let list = list.as_object().ok_or_else(invalid_response)?;
            let display = list
                .get("displayName")
                .or_else(|| list.get("name"))
                .and_then(Value::as_str)
                .unwrap_or_default();
            if display.eq_ignore_ascii_case(list_title) {
                return required_json_text(list, "id", 4 * 1_024).map(str::to_owned);
            }
        }
        Err(not_found())
    }

    async fn get_json(&self, url: Url) -> Result<Map<String, Value>, SharePointClientError> {
        let response = self.execute(url, "application/json").await?;
        require_success(response.status)?;
        require_content_type(response.content_type.as_deref(), "application/json")?;
        serde_json::from_slice::<Value>(&response.body)
            .map_err(|_| invalid_response())?
            .as_object()
            .cloned()
            .ok_or_else(invalid_response)
    }

    async fn execute(
        &self,
        url: Url,
        accept: &'static str,
    ) -> Result<SharePointHttpResponse, SharePointClientError> {
        self.send(GraphCall::get(url, accept)).await
    }

    /// One call to Graph under the delegated bearer (and only there: the
    /// URL must be a Graph v1.0 address).
    async fn send(&self, call: GraphCall) -> Result<SharePointHttpResponse, SharePointClientError> {
        validate_graph_url(&call.url)?;
        let mut request = base_request(call.method, call.url, call.accept);
        let bearer =
            zeroize::Zeroizing::new(format!("Bearer {}", self.config.access_token.as_str()));
        let mut authorization =
            HeaderValue::from_str(&bearer).map_err(|_| invalid_configuration())?;
        authorization.set_sensitive(true);
        request.headers_mut().insert(AUTHORIZATION, authorization);
        if let Some(prefer) = call.prefer {
            request.headers_mut().insert(
                HeaderName::from_static("prefer"),
                HeaderValue::from_static(prefer),
            );
        }
        attach_body(&mut request, call.body, call.timeout);
        let response = self
            .transport
            .execute_bounded(request, call.max_body)
            .await?;
        refuse_oversize(response, call.keep_oversize)
    }

    /// One call to an address Graph handed back (a pre-authenticated download
    /// or upload session URL): no bearer, and only a `SharePoint` host.
    async fn send_transfer(
        &self,
        call: GraphCall,
    ) -> Result<SharePointHttpResponse, SharePointClientError> {
        validate_transfer_url(&call.url)?;
        let mut request = base_request(call.method, call.url, call.accept);
        for (name, value) in call.headers {
            request.headers_mut().insert(name, value);
        }
        attach_body(&mut request, call.body, call.timeout);
        let response = self
            .transport
            .execute_bounded(request, call.max_body)
            .await?;
        refuse_oversize(response, call.keep_oversize)
    }

    async fn page_values(
        &self,
        mut url: Url,
        limit: usize,
        project_fields: bool,
    ) -> Result<Vec<Value>, SharePointClientError> {
        let mut output = Vec::with_capacity(limit.min(64));
        for _ in 0..MAX_PAGES {
            let object = self.get_json(url).await?;
            for item in required_values(&object)? {
                if project_fields {
                    let fields = item
                        .as_object()
                        .and_then(|item| item.get("fields"))
                        .and_then(Value::as_object)
                        .cloned()
                        .ok_or_else(invalid_response)?;
                    output.push(Value::Object(fields));
                } else {
                    output.push(item.clone());
                }
                if output.len() >= limit {
                    validate_output(&Value::Array(output.clone()))?;
                    return Ok(output);
                }
            }
            let Some(next) = object.get("@odata.nextLink").and_then(Value::as_str) else {
                validate_output(&Value::Array(output.clone()))?;
                return Ok(output);
            };
            url = parse_next_link(next)?;
        }
        Err(resource_exhausted())
    }

    async fn onenote_prefix(&self) -> Result<Vec<String>, SharePointClientError> {
        Ok(vec![
            "sites".to_owned(),
            self.resolve_site_id().await?.to_owned(),
            "onenote".to_owned(),
        ])
    }

    async fn onenote_collection(
        &self,
        tail: &[&str],
        select: Option<&[String]>,
        default_select: &str,
        limit: Option<usize>,
    ) -> Result<Value, SharePointClientError> {
        let mut segments = self.onenote_prefix().await?;
        segments.extend(tail.iter().map(|segment| (*segment).to_owned()));
        let refs = segments.iter().map(String::as_str).collect::<Vec<_>>();
        let mut url = graph_url(&refs)?;
        let mut query = Vec::new();
        if let Some(limit) = limit {
            query.push(("$top", limit.to_string()));
        }
        match select {
            None => query.push(("$select", default_select.to_owned())),
            Some(fields) if !fields.is_empty() => {
                let selected = fields
                    .iter()
                    .filter(|field| field.as_str() != "contentUrl")
                    .cloned()
                    .collect::<Vec<_>>()
                    .join(",");
                if !selected.is_empty() {
                    query.push(("$select", selected));
                }
            }
            Some(_) => {}
        }
        set_owned_query(&mut url, &query);
        let object = self.get_json(url).await?;
        let result = Value::Array(required_values(&object)?.to_vec());
        validate_output(&result)?;
        Ok(result)
    }

    fn file_walk_seeds(
        &self,
        drives: &[Value],
        folder_name: Option<&str>,
        form_name: Option<&str>,
    ) -> Result<Vec<(String, Option<String>)>, SharePointClientError> {
        let folder = folder_name
            .map(normalize_folder_path)
            .transpose()?
            .filter(|folder| !folder.is_empty());
        let mut candidates = Vec::new();
        for drive in drives {
            let drive = drive.as_object().ok_or_else(invalid_response)?;
            let drive_id = required_json_text(drive, "id", 4 * 1_024)?;
            let library =
                drive_library_path(drive, &self.config.site_hostname, &self.config.site_path)?;
            if form_name
                .is_some_and(|expected| !drive_name_matches(drive, library.as_deref(), expected))
            {
                continue;
            }
            candidates.push((drive_id.to_owned(), library));
        }
        if candidates.is_empty() {
            return Err(not_found());
        }

        let Some(folder) = folder else {
            return Ok(candidates
                .into_iter()
                .map(|(drive_id, _)| (drive_id, None))
                .collect());
        };
        if form_name.is_some() {
            return Ok(candidates
                .into_iter()
                .map(|(drive_id, _)| (drive_id, Some(folder.clone())))
                .collect());
        }

        let folder = strip_site_prefix(&folder, &self.config.site_path);
        if folder.is_empty() {
            return Ok(candidates
                .into_iter()
                .map(|(drive_id, _)| (drive_id, None))
                .collect());
        }
        let matched = candidates
            .iter()
            .filter_map(|(drive_id, library)| {
                let library = library.as_deref()?;
                strip_path_prefix(folder, library).map(|remainder| {
                    (
                        drive_id.clone(),
                        (!remainder.is_empty()).then(|| remainder.to_owned()),
                    )
                })
            })
            .collect::<Vec<_>>();
        if !matched.is_empty() {
            return Ok(matched);
        }

        // A bare folder name can exist in more than one library. Walking the
        // same relative path in every admitted drive combines the SDK's probe
        // and subsequent list request without downloading any content.
        Ok(candidates
            .into_iter()
            .map(|(drive_id, _)| (drive_id, Some(folder.to_owned())))
            .collect())
    }
}

#[async_trait]
impl SharePointApi for SharePointClient {
    fn authorization(&self) -> &DelegatedAuthorizationRequirement {
        &self.config.authorization
    }

    async fn read_list(
        &self,
        list_title: &str,
        limit: usize,
    ) -> Result<Value, SharePointClientError> {
        if limit == 0 || limit > MAX_LIST_ITEMS {
            return Err(invalid_input());
        }
        let site_id = self.resolve_site_id().await?;
        let list_id = self.resolve_list_id(list_title).await?;
        let mut url = graph_url(&["sites", site_id, "lists", &list_id, "items"])?;
        let top = limit.min(999).to_string();
        set_query(&mut url, &[("$top", &top), ("$expand", "fields")]);
        self.page_values(url, limit, true).await.map(Value::Array)
    }

    async fn get_lists(&self) -> Result<Value, SharePointClientError> {
        let site_id = self.resolve_site_id().await?;
        let mut url = graph_url(&["sites", site_id, "lists"])?;
        set_query(
            &mut url,
            &[("$select", "id,displayName,description,list,createdDateTime")],
        );
        let object = self.get_json(url).await?;
        let mut output = Vec::new();
        for list in required_values(&object)? {
            let list = list.as_object().ok_or_else(invalid_response)?;
            let metadata = list
                .get("list")
                .and_then(Value::as_object)
                .ok_or_else(invalid_response)?;
            if metadata
                .get("hidden")
                .and_then(Value::as_bool)
                .unwrap_or(false)
                || matches!(
                    metadata.get("template").and_then(Value::as_str),
                    Some("documentLibrary" | "webPageLibrary")
                )
            {
                continue;
            }
            output.push(json!({
                "Title": optional_json_text(list, "displayName", 8 * 1_024)?,
                "Id": required_json_text(list, "id", 4 * 1_024)?,
                "Description": optional_json_text(list, "description", 64 * 1_024)?,
                "ItemCount": metadata.get("itemCount").and_then(Value::as_u64).unwrap_or(0),
                "BaseTemplate": optional_json_text(metadata, "template", 1_024)?,
            }));
        }
        let result = Value::Array(output);
        validate_output(&result)?;
        Ok(result)
    }

    async fn get_list_columns(&self, list_title: &str) -> Result<Value, SharePointClientError> {
        let site_id = self.resolve_site_id().await?;
        let list_id = self.resolve_list_id(list_title).await?;
        let url = graph_url(&["sites", site_id, "lists", &list_id, "columns"])?;
        let object = self.get_json(url).await?;
        let mut output = Vec::new();
        for column in required_values(&object)? {
            let column = column.as_object().ok_or_else(invalid_response)?;
            if column
                .get("hidden")
                .and_then(Value::as_bool)
                .unwrap_or(false)
                || column
                    .get("readOnly")
                    .and_then(Value::as_bool)
                    .unwrap_or(false)
                || column.contains_key("lookup")
            {
                continue;
            }
            let column_type = if column.contains_key("number") || column.contains_key("currency") {
                "number"
            } else if column.contains_key("boolean") {
                "boolean"
            } else if column.contains_key("dateTime") {
                "dateTime"
            } else if column.contains_key("choice") {
                "choice"
            } else {
                "text"
            };
            let name = required_json_text(column, "name", 4 * 1_024)?;
            let mut item = json!({
                "name": name,
                "displayName": optional_json_text(column, "displayName", 8 * 1_024)?.to_owned(),
                "columnType": column_type,
                "required": column.get("required").and_then(Value::as_bool).unwrap_or(false),
            });
            if item["displayName"] == "" {
                item["displayName"] = Value::String(name.to_owned());
            }
            if column_type == "choice"
                && let Some(choices) = column
                    .get("choice")
                    .and_then(Value::as_object)
                    .and_then(|choice| choice.get("choices"))
                    .and_then(Value::as_array)
            {
                item["choice"] = json!({"choices": choices});
            }
            output.push(item);
        }
        let result = Value::Array(output);
        validate_output(&result)?;
        Ok(result)
    }

    async fn get_files_list(
        &self,
        options: SharePointFileListOptions,
    ) -> Result<Value, SharePointClientError> {
        if options.limit == 0 || options.limit > MAX_FILES {
            return Err(invalid_input());
        }
        let drives = self.list_drives().await?;
        let mut queue = VecDeque::new();
        for (drive_id, folder) in self.file_walk_seeds(
            drives,
            options.folder_name.as_deref(),
            options.form_name.as_deref(),
        )? {
            let url = match folder {
                Some(folder) => drive_folder_children_url(&drive_id, &folder)?,
                None => graph_url(&["drives", &drive_id, "root", "children"])?,
            };
            queue.push_back((drive_id, url));
        }

        let mut output = Vec::with_capacity(options.limit.min(64));
        let mut visited = BTreeSet::new();
        let mut requests = 0usize;
        while let Some((drive_id, mut url)) = queue.pop_front() {
            if !visited.insert(url.as_str().to_owned()) {
                continue;
            }
            loop {
                requests = requests.checked_add(1).ok_or_else(resource_exhausted)?;
                if requests > MAX_FOLDER_REQUESTS {
                    return Err(resource_exhausted());
                }
                if url.query().is_none() {
                    set_query(
                        &mut url,
                        &[
                            ("$top", "999"),
                            (
                                "$select",
                                "id,name,file,folder,webUrl,createdDateTime,lastModifiedDateTime,parentReference,size",
                            ),
                        ],
                    );
                }
                let object = match self.get_json(url.clone()).await {
                    Ok(object) => object,
                    Err(error) if error.code() == SharePointClientErrorCode::NotFound => break,
                    Err(error) => return Err(error),
                };
                for item in required_values(&object)? {
                    let item = item.as_object().ok_or_else(invalid_response)?;
                    let item_id = required_json_text(item, "id", 4 * 1_024)?;
                    if item.contains_key("folder") {
                        queue.push_back((
                            drive_id.clone(),
                            graph_url(&["drives", &drive_id, "items", item_id, "children"])?,
                        ));
                        continue;
                    }
                    if !item.contains_key("file") {
                        continue;
                    }
                    let name = required_json_text(item, "name", 8 * 1_024)?;
                    if matches_any(name, &options.skip_patterns)
                        || (!options.include_patterns.is_empty()
                            && !matches_any(name, &options.include_patterns))
                    {
                        continue;
                    }
                    let parent = item
                        .get("parentReference")
                        .and_then(Value::as_object)
                        .and_then(|parent| parent.get("path"))
                        .and_then(Value::as_str)
                        .unwrap_or_default();
                    output.push(json!({
                        "Name": name,
                        "Path": format!("{parent}/{name}"),
                        "Created": optional_json_text(item, "createdDateTime", 8 * 1_024)?,
                        "Modified": optional_json_text(item, "lastModifiedDateTime", 8 * 1_024)?,
                        "Link": optional_json_text(item, "webUrl", 16 * 1_024)?,
                        "id": item_id,
                    }));
                    if output.len() >= options.limit {
                        let result = Value::Array(output);
                        validate_output(&result)?;
                        return Ok(result);
                    }
                }
                let Some(next) = object.get("@odata.nextLink").and_then(Value::as_str) else {
                    break;
                };
                url = parse_next_link(next)?;
            }
        }
        if output.is_empty() {
            return Err(not_found());
        }
        let result = Value::Array(output);
        validate_output(&result)?;
        Ok(result)
    }

    async fn onenote_get_notebooks(
        &self,
        select: Option<&[String]>,
    ) -> Result<Value, SharePointClientError> {
        self.onenote_collection(
            &["notebooks"],
            select,
            "id,displayName,createdDateTime,lastModifiedDateTime,links,isDefault,isShared",
            None,
        )
        .await
    }

    async fn onenote_get_sections(
        &self,
        notebook_id: &str,
        select: Option<&[String]>,
    ) -> Result<Value, SharePointClientError> {
        self.onenote_collection(
            &["notebooks", notebook_id, "sections"],
            select,
            "id,displayName,createdDateTime,lastModifiedDateTime,pagesUrl,isDefault",
            None,
        )
        .await
    }

    async fn onenote_get_pages(
        &self,
        section_id: &str,
        limit: usize,
        select: Option<&[String]>,
    ) -> Result<Value, SharePointClientError> {
        if limit == 0 || limit > MAX_ONENOTE_PAGES {
            return Err(invalid_input());
        }
        self.onenote_collection(
            &["sections", section_id, "pages"],
            select,
            "id,title,createdDateTime,lastModifiedDateTime",
            Some(limit),
        )
        .await
    }

    async fn onenote_get_page_content(
        &self,
        page_id: &str,
    ) -> Result<Value, SharePointClientError> {
        let mut segments = self.onenote_prefix().await?;
        segments.extend(["pages".to_owned(), page_id.to_owned(), "content".to_owned()]);
        let refs = segments.iter().map(String::as_str).collect::<Vec<_>>();
        let response = self.execute(graph_url(&refs)?, "text/html").await?;
        require_success(response.status)?;
        require_html(response.content_type.as_deref())?;
        if response.body.len() > MAX_OUTPUT_BYTES {
            return Err(resource_exhausted());
        }
        String::from_utf8(response.body)
            .map(Value::String)
            .map_err(|_| invalid_response())
    }

    async fn onenote_page_markup(&self, page_id: &str) -> Result<String, SharePointClientError> {
        let mut segments = self.onenote_prefix().await?;
        segments.extend(["pages".to_owned(), page_id.to_owned(), "content".to_owned()]);
        let refs = segments.iter().map(String::as_str).collect::<Vec<_>>();
        let response = self
            .send(GraphCall::get(graph_url(&refs)?, "text/html").max_body(MAX_PAGE_MARKUP_BYTES))
            .await?;
        require_success(response.status)?;
        require_html(response.content_type.as_deref())?;
        String::from_utf8(response.body).map_err(|_| invalid_response())
    }

    async fn create_list_item(
        &self,
        list_title: &str,
        fields: &Map<String, Value>,
    ) -> Result<Value, SharePointClientError> {
        self.create_list_item_impl(list_title, fields).await
    }

    async fn upload_file(&self, upload: SharePointUpload) -> Result<Value, SharePointClientError> {
        self.upload_file_impl(upload).await
    }

    async fn download_document(
        &self,
        path: &str,
        max_bytes: usize,
    ) -> Result<SharePointFile, SharePointClientError> {
        self.download_document_impl(path, max_bytes).await
    }

    async fn download_shared_file(
        &self,
        sharing_url: &str,
        readable: &ReadableCheck,
    ) -> Result<SharePointFile, SharePointClientError> {
        self.download_shared_file_impl(sharing_url, readable).await
    }

    async fn onenote_download_resource(
        &self,
        resource_id: &str,
        max_bytes: usize,
    ) -> Result<Vec<u8>, SharePointClientError> {
        self.onenote_download_resource_impl(resource_id, max_bytes)
            .await
    }

    async fn onenote_resource_id(
        &self,
        src: &str,
    ) -> Result<Option<String>, SharePointClientError> {
        let site_id = self.resolve_site_id().await?;
        Ok(super::html::resource_id(src, site_id))
    }

    async fn onenote_resource_url(&self, src: &str) -> Result<String, SharePointClientError> {
        match self.onenote_resource_id(src).await? {
            Some(resource) => Ok(format!(
                "{GRAPH_BASE}sites/{}/onenote/resources/{resource}/content",
                self.resolve_site_id().await?
            )),
            None => Ok(src.to_owned()),
        }
    }

    async fn onenote_create_notebook(
        &self,
        display_name: &str,
    ) -> Result<Value, SharePointClientError> {
        self.onenote_create_notebook_impl(display_name).await
    }

    async fn onenote_create_section(
        &self,
        notebook_id: &str,
        display_name: &str,
    ) -> Result<Value, SharePointClientError> {
        self.onenote_create_section_impl(notebook_id, display_name)
            .await
    }

    async fn onenote_create_page(
        &self,
        section_id: &str,
        html_content: &str,
    ) -> Result<Value, SharePointClientError> {
        self.onenote_create_page_impl(section_id, html_content)
            .await
    }

    async fn onenote_update_page(
        &self,
        page_id: &str,
        patch_commands: &[Value],
    ) -> Result<(), SharePointClientError> {
        self.onenote_update_page_impl(page_id, patch_commands).await
    }

    async fn onenote_delete_page(&self, page_id: &str) -> Result<(), SharePointClientError> {
        self.onenote_delete_page_impl(page_id).await
    }
}

/// One call to Graph or to an address it returned.
struct GraphCall {
    method: Method,
    url: Url,
    accept: &'static str,
    /// The body and its media type.
    body: Option<(&'static str, Vec<u8>)>,
    prefer: Option<&'static str>,
    /// Extra headers of a transfer call (`Content-Range`).
    headers: Vec<(HeaderName, HeaderValue)>,
    timeout: Option<Duration>,
    max_body: usize,
    /// A change keeps an oversize answer so it can read the status first.
    keep_oversize: bool,
}

impl GraphCall {
    fn get(url: Url, accept: &'static str) -> Self {
        Self::new(Method::GET, url, accept)
    }

    fn new(method: Method, url: Url, accept: &'static str) -> Self {
        Self {
            method,
            url,
            accept,
            body: None,
            prefer: None,
            headers: Vec::new(),
            timeout: None,
            max_body: MAX_RESPONSE_BYTES,
            keep_oversize: false,
        }
    }

    /// An answer over the cap comes back flagged instead of as an error: for
    /// a call that changes something, whose status is true whatever the size.
    fn tolerate_oversize(mut self) -> Self {
        self.keep_oversize = true;
        self
    }

    fn body(mut self, content_type: &'static str, bytes: Vec<u8>) -> Self {
        self.body = Some((content_type, bytes));
        self
    }

    fn timeout(mut self, timeout: Duration) -> Self {
        self.timeout = Some(timeout);
        self
    }

    fn max_body(mut self, max_body: usize) -> Self {
        self.max_body = max_body;
        self
    }
}

/// What a call makes of an answer that is over the cap or lost its body. A
/// change keeps both (its status decides). A read cannot use either: an
/// oversize answer is an error, and so is a lost body behind a success status;
/// a lost body behind a refusal keeps the refusal's status for the caller.
fn refuse_oversize(
    response: SharePointHttpResponse,
    keep: bool,
) -> Result<SharePointHttpResponse, SharePointClientError> {
    if keep {
        return Ok(response);
    }
    if response.oversize {
        return Err(resource_exhausted());
    }
    match response.body_lost {
        Some(SharePointClientErrorCode::Timeout) if response.status.is_success() => Err(timeout()),
        Some(_) if response.status.is_success() => Err(connection_lost()),
        _ => Ok(response),
    }
}

fn base_request(method: Method, url: Url, accept: &'static str) -> Request {
    let mut request = Request::new(method, url);
    request
        .headers_mut()
        .insert(ACCEPT, HeaderValue::from_static(accept));
    request
        .headers_mut()
        .insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
    request
}

fn attach_body(
    request: &mut Request,
    body: Option<(&'static str, Vec<u8>)>,
    timeout: Option<Duration>,
) {
    if let Some((content_type, bytes)) = body {
        request
            .headers_mut()
            .insert(CONTENT_TYPE, HeaderValue::from_static(content_type));
        *request.body_mut() = Some(reqwest::Body::from(bytes));
    }
    if let Some(timeout) = timeout {
        *request.timeout_mut() = Some(timeout);
    }
}

/// The address of a file transfer Graph handed back: `https`, on a
/// `*.sharepoint.com` host, with no credentials, port or fragment. Anything
/// else is refused, so a crafted answer cannot make this process fetch
/// elsewhere.
fn validate_transfer_url(url: &Url) -> Result<(), SharePointClientError> {
    let on_sharepoint = url.host_str().is_some_and(|host| {
        let host = host.to_ascii_lowercase();
        host.len() > ".sharepoint.com".len() && host.ends_with(".sharepoint.com")
    });
    if url.scheme() != "https"
        || !on_sharepoint
        || url.port().is_some()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.fragment().is_some()
        || url.as_str().len() > 16 * 1_024
    {
        return Err(invalid_response());
    }
    Ok(())
}

fn graph_url(segments: &[&str]) -> Result<Url, SharePointClientError> {
    let mut url = Url::parse(GRAPH_BASE).map_err(|_| invalid_configuration())?;
    {
        let mut path = url
            .path_segments_mut()
            .map_err(|()| invalid_configuration())?;
        path.pop_if_empty();
        for segment in segments {
            validate_segment(segment)?;
            path.push(segment);
        }
    }
    Ok(url)
}

fn drive_folder_children_url(drive_id: &str, folder: &str) -> Result<Url, SharePointClientError> {
    let parts = folder
        .trim_matches('/')
        .split('/')
        .filter(|part| !part.is_empty())
        .collect::<Vec<_>>();
    if parts.is_empty() || parts.contains(&"..") {
        return Err(invalid_input());
    }
    let mut url = Url::parse(GRAPH_BASE).map_err(|_| invalid_configuration())?;
    {
        let mut path = url
            .path_segments_mut()
            .map_err(|()| invalid_configuration())?;
        path.pop_if_empty();
        path.push("drives").push(drive_id).push("root:");
        for (index, part) in parts.iter().enumerate() {
            validate_segment(part)?;
            if index + 1 == parts.len() {
                path.push(&format!("{part}:"));
            } else {
                path.push(part);
            }
        }
        path.push("children");
    }
    Ok(url)
}

fn replace_path_segment(
    url: &mut Url,
    index: usize,
    replacement: &str,
) -> Result<(), SharePointClientError> {
    let mut segments = url
        .path_segments()
        .ok_or_else(invalid_configuration)?
        .map(str::to_owned)
        .collect::<Vec<_>>();
    let segment = segments.get_mut(index).ok_or_else(invalid_configuration)?;
    replacement.clone_into(segment);
    url.set_path("");
    {
        let mut path = url
            .path_segments_mut()
            .map_err(|()| invalid_configuration())?;
        path.pop_if_empty();
        for segment in &segments {
            path.push(segment);
        }
    }
    Ok(())
}

fn validate_segment(value: &str) -> Result<(), SharePointClientError> {
    if value.is_empty()
        || value.len() > 8 * 1_024
        || value == ".."
        || value.chars().any(char::is_control)
    {
        return Err(invalid_input());
    }
    Ok(())
}

fn set_query(url: &mut Url, values: &[(&str, &str)]) {
    url.query_pairs_mut().clear().extend_pairs(values);
}

fn set_owned_query(url: &mut Url, values: &[(&str, String)]) {
    url.query_pairs_mut()
        .clear()
        .extend_pairs(values.iter().map(|(name, value)| (*name, value.as_str())));
}

fn parse_next_link(value: &str) -> Result<Url, SharePointClientError> {
    if value.len() > 16 * 1_024 || value.contains('\\') {
        return Err(invalid_response());
    }
    let url = Url::parse(value).map_err(|_| invalid_response())?;
    validate_graph_url(&url)?;
    Ok(url)
}

fn validate_graph_url(url: &Url) -> Result<(), SharePointClientError> {
    if url.scheme() != "https"
        || url.host_str() != Some("graph.microsoft.com")
        || url.port().is_some()
        || !url.username().is_empty()
        || url.password().is_some()
        || !url.path().starts_with("/v1.0/")
        || url.fragment().is_some()
    {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn required_values(object: &Map<String, Value>) -> Result<&[Value], SharePointClientError> {
    object
        .get("value")
        .and_then(Value::as_array)
        .map(Vec::as_slice)
        .ok_or_else(invalid_response)
}

fn required_json_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, SharePointClientError> {
    let value = object
        .get(name)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty() && value.len() <= limit)
        .ok_or_else(invalid_response)?;
    if value.chars().any(char::is_control) {
        return Err(invalid_response());
    }
    Ok(value)
}

fn optional_json_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, SharePointClientError> {
    match object.get(name) {
        None | Some(Value::Null) => Ok(""),
        Some(Value::String(value))
            if value.len() <= limit && !value.chars().any(char::is_control) =>
        {
            Ok(value)
        }
        Some(_) => Err(invalid_response()),
    }
}

fn require_success(status: StatusCode) -> Result<(), SharePointClientError> {
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

/// The status of a CHANGE, one cause each. Throttling (429, 503) was not
/// applied; a gateway or server failure after the request went out (408,
/// 500, 502, 504, anything unlisted) may have been.
fn require_effect_success(status: StatusCode) -> Result<(), SharePointClientError> {
    match status.as_u16() {
        200..=299 => Ok(()),
        401 => Err(authentication()),
        403 => Err(authorization()),
        404 => Err(not_found()),
        409 | 412 | 423 => Err(conflict()),
        413 => Err(resource_exhausted()),
        429 => Err(rate_limited()),
        503 => Err(dependency_unavailable()),
        // Any other refusal (400, 415, 422, 410 Gone, 416 Range Not Satisfiable, ...) is a
        // definite one: Graph looked at the request and said no.
        400..=499 => Err(invalid_input()),
        _ => Err(outcome_unknown()),
    }
}

fn require_content_type(value: Option<&str>, expected: &str) -> Result<(), SharePointClientError> {
    value
        .and_then(|value| value.split(';').next())
        .map(str::trim)
        .is_some_and(|value| value.eq_ignore_ascii_case(expected))
        .then_some(())
        .ok_or_else(invalid_response)
}

fn require_html(value: Option<&str>) -> Result<(), SharePointClientError> {
    value
        .and_then(|value| value.split(';').next())
        .map(str::trim)
        .is_some_and(|value| {
            value.eq_ignore_ascii_case("text/html")
                || value.eq_ignore_ascii_case("application/xhtml+xml")
        })
        .then_some(())
        .ok_or_else(invalid_response)
}

fn validate_output(value: &Value) -> Result<(), SharePointClientError> {
    let bytes = serde_json::to_vec(value).map_err(|_| invalid_response())?;
    if bytes.len() > MAX_OUTPUT_BYTES {
        return Err(resource_exhausted());
    }
    Ok(())
}

fn normalize_folder_path(value: &str) -> Result<String, SharePointClientError> {
    if value.contains('\\') || value.chars().any(char::is_control) {
        return Err(invalid_input());
    }
    let decoded = percent_decode_str(value)
        .decode_utf8()
        .map_err(|_| invalid_input())?;
    let parts = decoded
        .trim_matches('/')
        .split('/')
        .filter(|part| !part.is_empty())
        .collect::<Vec<_>>();
    if parts.iter().any(|part| matches!(*part, "." | "..")) {
        return Err(invalid_input());
    }
    Ok(parts.join("/"))
}

fn drive_library_path(
    drive: &Map<String, Value>,
    site_hostname: &str,
    site_path: &str,
) -> Result<Option<String>, SharePointClientError> {
    let Some(web_url) = drive.get("webUrl").and_then(Value::as_str) else {
        return Ok(None);
    };
    if web_url.len() > 16 * 1_024 || web_url.contains('\\') {
        return Err(invalid_response());
    }
    let url = Url::parse(web_url).map_err(|_| invalid_response())?;
    if url.scheme() != "https"
        || url.host_str() != Some(site_hostname)
        || url.port().is_some()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err(invalid_response());
    }
    let decoded = percent_decode_str(url.path())
        .decode_utf8()
        .map_err(|_| invalid_response())?;
    let path = decoded.trim_matches('/');
    let relative = strip_path_prefix(path, site_path).ok_or_else(invalid_response)?;
    if relative.is_empty() {
        return Err(invalid_response());
    }
    Ok(Some(relative.to_owned()))
}

fn drive_name_matches(
    drive: &Map<String, Value>,
    library_path: Option<&str>,
    expected: &str,
) -> bool {
    drive
        .get("name")
        .and_then(Value::as_str)
        .is_some_and(|name| name.eq_ignore_ascii_case(expected))
        || library_path
            .and_then(|path| path.rsplit('/').next())
            .is_some_and(|name| name.eq_ignore_ascii_case(expected))
}

fn strip_site_prefix<'a>(path: &'a str, site_path: &str) -> &'a str {
    strip_path_prefix(path, site_path).unwrap_or(path)
}

fn strip_path_prefix<'a>(path: &'a str, prefix: &str) -> Option<&'a str> {
    if prefix.is_empty() {
        return Some(path);
    }
    if path.eq_ignore_ascii_case(prefix) {
        return Some("");
    }
    let boundary = prefix.len().checked_add(1)?;
    path.get(..prefix.len())
        .filter(|candidate| candidate.eq_ignore_ascii_case(prefix))?;
    (path.as_bytes().get(prefix.len()) == Some(&b'/')).then(|| &path[boundary..])
}

pub(in crate::toolkits) fn normalize_patterns(
    values: Option<&[String]>,
) -> Result<Vec<String>, SharePointClientError> {
    let Some(values) = values else {
        return Ok(Vec::new());
    };
    if values.len() > 128 {
        return Err(resource_exhausted());
    }
    let mut output = Vec::new();
    for value in values {
        let value = value.trim();
        if value.is_empty() {
            continue;
        }
        if value.len() > 1_024 || value.contains(['/', '\\']) || value.chars().any(char::is_control)
        {
            return Err(invalid_input());
        }
        let pattern = if value.starts_with('*') {
            if value.starts_with("*.") || value.contains('.') {
                value.to_owned()
            } else {
                format!("*.{}", value.trim_start_matches('*'))
            }
        } else if value.starts_with('.') {
            format!("*{value}")
        } else if value.contains('.') {
            value.to_owned()
        } else {
            format!("*.{value}")
        }
        .to_ascii_lowercase();
        if !output.contains(&pattern) {
            output.push(pattern);
        }
    }
    Ok(output)
}

fn matches_any(filename: &str, patterns: &[String]) -> bool {
    let filename = filename.to_ascii_lowercase();
    patterns
        .iter()
        .any(|pattern| wildcard_match(pattern.as_bytes(), filename.as_bytes()))
}

fn wildcard_match(pattern: &[u8], value: &[u8]) -> bool {
    let (mut pattern_index, mut value_index) = (0usize, 0usize);
    let (mut star_index, mut star_value_index) = (None, 0usize);
    while value_index < value.len() {
        if pattern_index < pattern.len() && pattern[pattern_index] == value[value_index] {
            pattern_index += 1;
            value_index += 1;
        } else if pattern_index < pattern.len() && pattern[pattern_index] == b'*' {
            star_index = Some(pattern_index);
            pattern_index += 1;
            star_value_index = value_index;
        } else if let Some(star) = star_index {
            pattern_index = star + 1;
            star_value_index += 1;
            value_index = star_value_index;
        } else {
            return false;
        }
    }
    pattern[pattern_index..].iter().all(|byte| *byte == b'*')
}

/// A transport failure by what is known of how far the request got: a
/// refused or failed connection never carried it (Graph is unavailable); a
/// timeout, or any failure after the connection was up, may have (the answer
/// was lost), which a change reports as an unknown outcome.
fn map_reqwest_error(error: &reqwest::Error) -> SharePointClientError {
    if error.is_connect() {
        dependency_unavailable()
    } else if error.is_timeout() {
        timeout()
    } else if error.is_request() || error.is_body() {
        connection_lost()
    } else {
        invalid_response()
    }
}

const fn invalid_configuration() -> SharePointClientError {
    SharePointClientError {
        code: SharePointClientErrorCode::InvalidConfiguration,
        detail: None,
    }
}

const fn invalid_input() -> SharePointClientError {
    SharePointClientError {
        code: SharePointClientErrorCode::InvalidInput,
        detail: None,
    }
}

const fn authentication() -> SharePointClientError {
    SharePointClientError {
        code: SharePointClientErrorCode::Authentication,
        detail: None,
    }
}

const fn authorization() -> SharePointClientError {
    SharePointClientError {
        code: SharePointClientErrorCode::Authorization,
        detail: None,
    }
}

const fn not_found() -> SharePointClientError {
    SharePointClientError {
        code: SharePointClientErrorCode::NotFound,
        detail: None,
    }
}

const fn rate_limited() -> SharePointClientError {
    SharePointClientError {
        code: SharePointClientErrorCode::RateLimited,
        detail: None,
    }
}

const fn timeout() -> SharePointClientError {
    SharePointClientError {
        code: SharePointClientErrorCode::Timeout,
        detail: None,
    }
}

const fn dependency_unavailable() -> SharePointClientError {
    SharePointClientError {
        code: SharePointClientErrorCode::DependencyUnavailable,
        detail: None,
    }
}

const fn invalid_response() -> SharePointClientError {
    SharePointClientError {
        code: SharePointClientErrorCode::InvalidResponse,
        detail: None,
    }
}

const fn conflict() -> SharePointClientError {
    SharePointClientError {
        code: SharePointClientErrorCode::Conflict,
        detail: None,
    }
}

const fn outcome_unknown() -> SharePointClientError {
    SharePointClientError {
        code: SharePointClientErrorCode::OutcomeUnknown,
        detail: None,
    }
}

const fn connection_lost() -> SharePointClientError {
    SharePointClientError {
        code: SharePointClientErrorCode::ConnectionLost,
        detail: None,
    }
}

/// A resumable upload the session would not complete: a definite failure
/// (nothing is saved; the session was cancelled).
const fn upload_incomplete() -> SharePointClientError {
    SharePointClientError {
        code: SharePointClientErrorCode::DependencyUnavailable,
        detail: Some(
            "SharePoint did not accept the whole file, so the upload was cancelled and nothing was saved",
        ),
    }
}

const fn file_refused(detail: &'static str) -> SharePointClientError {
    SharePointClientError {
        code: SharePointClientErrorCode::FileRefused,
        detail: Some(detail),
    }
}

const fn resource_exhausted() -> SharePointClientError {
    SharePointClientError {
        code: SharePointClientErrorCode::ResourceExhausted,
        detail: None,
    }
}
