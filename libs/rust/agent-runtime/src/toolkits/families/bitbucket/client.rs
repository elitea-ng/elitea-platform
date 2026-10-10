//! The Bitbucket client: one repository on Bitbucket Cloud (REST 2.0) or
//! Bitbucket Server / Data Center (REST 1.0), behind one operation ledger.
//!
//! The SDK reaches both through `atlassian-python-api`; this client speaks the
//! same endpoints directly over a bounded HTTPS, no-redirect, no-retry
//! transport with a sensitive Basic credential.

use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use reqwest::header::{ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderValue, LOCATION};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value, json};
use tokio::sync::Mutex;
use zeroize::Zeroizing;

use super::config::{BitbucketHosting, BitbucketToolkitConfig};
use crate::toolkits::families::gitlab_org::client::wildcard_matches;
use crate::toolkits::families::gitlab_org::edit::{EditErrorCode, apply_update};
use crate::toolkits::families::vcs_text::{
    MAX_OUTPUT_CHARS, batch_skip_notice, compile_pattern, grep_content, guard_text_read,
    measure_result_chars, python_repr, requested_label, slice_lines,
};

const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 8;
const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_REQUEST_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_FILE_BYTES: usize = 1_024 * 1_024;
const MAX_PATH_BYTES: usize = 1_024;
const MAX_BRANCH_BYTES: usize = 255;
const MAX_TEXT_BYTES: usize = 256 * 1_024;
const MAX_PAGES: usize = 10;
const MAX_ITEMS: usize = 1_000;
const PAGE_SIZE: &str = "100";
const USER_AGENT: &str = "elitea-worker-rust/0.1";
const MULTIPART_BOUNDARY: &str = "elitea-bitbucket-form-boundary";

/// The SDK's `bitbucket_constants.create_pr_data`, quoted in its guidance.
pub(in crate::toolkits) const CREATE_PR_DATA: &str = "JSON string describing pull requ\nest structure: i.e. for server side '{ \"title\":\"PR title\", \"description\":\"PR description\", \"state\":\"OPEN\", \"open\":true, \"closed\":false, \"fromRef\":{ \"id\":\"refs/heads/source_branch\" }, \"toRef\":{ \"id\":\"refs/heads/target_branch\" }, \"locked\":false }' and cloud version: '{ \"title\": \"PR title\", \"source\": { \"branch\": { \"name\": \"source_branch\" } }, \"destination\": { \"branch\": { \"name\": \"destination_branch\" } } }'";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum BitbucketClientErrorCode {
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

/// Stable provider failure without origin, repository, path, body or secret.
pub(crate) struct BitbucketClientError {
    code: BitbucketClientErrorCode,
    retryable: bool,
}

impl BitbucketClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> BitbucketClientErrorCode {
        self.code
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            BitbucketClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "bitbucket.configuration.invalid",
                "the Bitbucket toolkit configuration is invalid",
            ),
            BitbucketClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "bitbucket.request.invalid",
                "the Bitbucket request is invalid",
            ),
            BitbucketClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "bitbucket.authentication.failed",
                "Bitbucket authentication failed",
            ),
            BitbucketClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "bitbucket.authorization.failed",
                "Bitbucket did not authorize the request",
            ),
            BitbucketClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "bitbucket.resource.not_found",
                "the requested Bitbucket resource was not found",
            ),
            BitbucketClientErrorCode::Conflict => (
                ErrorCategory::InvalidInput,
                "bitbucket.resource.conflict",
                "the Bitbucket resource changed or already exists",
            ),
            BitbucketClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "bitbucket.rate_limited",
                "Bitbucket rate limited the request",
            ),
            BitbucketClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "bitbucket.timeout",
                "the Bitbucket request timed out",
            ),
            BitbucketClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "bitbucket.unavailable",
                "Bitbucket is unavailable",
            ),
            BitbucketClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "bitbucket.response.invalid",
                "Bitbucket returned an invalid response",
            ),
            BitbucketClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "bitbucket.resource_exhausted",
                "the Bitbucket request or response exceeds the approved limit",
            ),
            BitbucketClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "bitbucket.effect.unknown_outcome",
                "Bitbucket may have applied the requested effect; reconcile it before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(code: BitbucketClientErrorCode) -> Self {
        Self {
            code,
            retryable: false,
        }
    }
}

impl fmt::Debug for BitbucketClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("BitbucketClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for BitbucketClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            BitbucketClientErrorCode::InvalidConfiguration => {
                "the Bitbucket client configuration is invalid"
            }
            BitbucketClientErrorCode::InvalidInput => "the Bitbucket request is invalid",
            BitbucketClientErrorCode::Authentication => "Bitbucket authentication failed",
            BitbucketClientErrorCode::Authorization => "Bitbucket authorization failed",
            BitbucketClientErrorCode::NotFound => "the Bitbucket resource was not found",
            BitbucketClientErrorCode::Conflict => "the Bitbucket resource is in conflict",
            BitbucketClientErrorCode::RateLimited => "Bitbucket rate limited the request",
            BitbucketClientErrorCode::Timeout => "the Bitbucket request timed out",
            BitbucketClientErrorCode::DependencyUnavailable => "Bitbucket is unavailable",
            BitbucketClientErrorCode::InvalidResponse => "Bitbucket returned an invalid response",
            BitbucketClientErrorCode::ResourceExhausted => {
                "the Bitbucket request or response exceeds its approved limit"
            }
            BitbucketClientErrorCode::UnknownOutcome => {
                "the Bitbucket effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for BitbucketClientError {}

/// One SDK `bitbucket` tool call, with arguments already shape-checked.
pub(in crate::toolkits) enum BitbucketOperation<'a> {
    CreateBranch {
        branch_name: &'a str,
    },
    DeleteBranch {
        branch_name: &'a str,
    },
    ListBranches {
        limit: usize,
        branch_wildcard: Option<&'a str>,
    },
    ListFiles {
        path: Option<&'a str>,
        recursive: bool,
        branch: Option<&'a str>,
    },
    CreatePullRequest {
        pr_json_data: &'a str,
    },
    CreateFile {
        file_path: &'a str,
        contents: &'a str,
        branch: Option<&'a str>,
    },
    ReadFile {
        file_path: &'a str,
        branch: Option<&'a str>,
        start_line: Option<usize>,
        end_line: Option<usize>,
    },
    UpdateFile {
        file_path: &'a str,
        update_query: &'a str,
        branch: Option<&'a str>,
    },
    SetActiveBranch {
        branch_name: &'a str,
    },
    GetPullRequestCommits {
        pr_id: u64,
    },
    GetPullRequest {
        pr_id: u64,
    },
    GetPullRequestChanges {
        pr_id: u64,
    },
    AddPullRequestComment {
        pr_id: u64,
        content: &'a str,
        inline: Option<&'a Map<String, Value>>,
    },
    ClosePullRequest {
        pr_id: u64,
        message: Option<&'a str>,
    },
    ReadMultipleFiles {
        file_paths: Vec<&'a str>,
        branch: Option<&'a str>,
        offset: Option<usize>,
        limit: Option<usize>,
    },
    GrepFile {
        file_path: &'a str,
        pattern: &'a str,
        branch: Option<&'a str>,
        is_regex: bool,
        context_lines: usize,
    },
}

#[async_trait]
pub(in crate::toolkits) trait BitbucketApi: Send + Sync {
    async fn execute(
        &self,
        operation: BitbucketOperation<'_>,
    ) -> Result<Value, BitbucketClientError>;
}

/// A bounded provider answer: JSON when the provider labelled it JSON, and
/// the body as UTF-8 text whenever it is text.
pub(in crate::toolkits) struct BitbucketHttpResponse {
    pub(in crate::toolkits) status: StatusCode,
    pub(in crate::toolkits) json: Option<Value>,
    pub(in crate::toolkits) text: Option<String>,
    pub(in crate::toolkits) location: Option<String>,
}

impl BitbucketHttpResponse {
    #[cfg(test)]
    pub(in crate::toolkits) fn json(status: StatusCode, body: Value) -> Self {
        Self {
            status,
            text: Some(body.to_string()),
            json: Some(body),
            location: None,
        }
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn text(status: StatusCode, body: &str) -> Self {
        Self {
            status,
            json: None,
            text: Some(body.to_owned()),
            location: None,
        }
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn redirect(location: &str) -> Self {
        Self {
            status: StatusCode::FOUND,
            json: None,
            text: None,
            location: Some(location.to_owned()),
        }
    }
}

#[async_trait]
pub(in crate::toolkits) trait BitbucketTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<BitbucketHttpResponse, BitbucketClientError>;
}

struct ReqwestBitbucketTransport {
    http: reqwest::Client,
}

#[async_trait]
impl BitbucketTransport for ReqwestBitbucketTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<BitbucketHttpResponse, BitbucketClientError> {
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
            return Err(response_failure(effect, resource_exhausted()));
        }
        let json_content_type = response
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.split(';').next())
            .is_some_and(|value| value.trim().eq_ignore_ascii_case("application/json"));
        let location = response
            .headers()
            .get(LOCATION)
            .and_then(|value| value.to_str().ok())
            .map(ToOwned::to_owned);
        let mut bytes = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|source| map_reqwest_error(&source, effect))?
        {
            if bytes.len().saturating_add(chunk.len()) > MAX_RESPONSE_BYTES {
                return Err(response_failure(effect, resource_exhausted()));
            }
            bytes.extend_from_slice(&chunk);
        }
        let json = if json_content_type && !bytes.is_empty() {
            serde_json::from_slice(&bytes).ok()
        } else {
            None
        };
        Ok(BitbucketHttpResponse {
            status: response.status(),
            json,
            text: String::from_utf8(bytes).ok(),
            location,
        })
    }
}

/// A request body in one of the encodings the two Bitbucket APIs take.
enum Body<'a> {
    None,
    Json(&'a Value),
    Form(&'a [(&'a str, &'a str)]),
    Multipart(&'a [(&'a str, &'a str)]),
}

/// One claim-scoped Bitbucket client with the SDK's invocation-local active
/// branch.
pub(crate) struct BitbucketClient {
    config: BitbucketToolkitConfig,
    transport: Arc<dyn BitbucketTransport>,
    operation_gate: Mutex<()>,
    active_branch: Mutex<Box<str>>,
}

impl BitbucketClient {
    pub(crate) fn new(config: BitbucketToolkitConfig) -> Result<Self, BitbucketClientError> {
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
        Ok(Self::with_transport(
            config,
            Arc::new(ReqwestBitbucketTransport { http }),
        ))
    }

    fn with_transport(
        config: BitbucketToolkitConfig,
        transport: Arc<dyn BitbucketTransport>,
    ) -> Self {
        let active_branch = Mutex::new(config.branch().into());
        Self {
            config,
            transport,
            operation_gate: Mutex::new(()),
            active_branch,
        }
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn fixture(
        config: BitbucketToolkitConfig,
        transport: Arc<dyn BitbucketTransport>,
    ) -> Self {
        Self::with_transport(config, transport)
    }

    fn cloud(&self) -> bool {
        self.config.hosting() == BitbucketHosting::Cloud
    }

    async fn active(&self) -> String {
        self.active_branch.lock().await.to_string()
    }

    /// `branch.strip() if branch and branch.strip() else self._active_branch`.
    async fn branch_or_active(&self, branch: Option<&str>) -> Result<String, BitbucketClientError> {
        match branch.map(str::trim).filter(|branch| !branch.is_empty()) {
            Some(branch) => {
                validate_text(branch, MAX_BRANCH_BYTES)?;
                Ok(branch.to_owned())
            }
            None => Ok(self.active().await),
        }
    }

    /// The repository resource: `/2.0/repositories/{workspace}/{repo}` on
    /// Cloud, `{context}/rest/api/1.0/projects/{project}/repos/{repo}` on
    /// Server, then `suffix`.
    fn repo_url(&self, suffix: &[&str]) -> Result<Url, BitbucketClientError> {
        let mut url = self.config.base_url().clone();
        {
            let mut path = url
                .path_segments_mut()
                .map_err(|()| invalid_configuration())?;
            path.pop_if_empty();
            if self.cloud() {
                path.extend(["2.0", "repositories"]);
                path.extend([self.config.project(), self.config.repository()]);
            } else {
                path.extend(["rest", "api", "1.0", "projects"]);
                path.extend([self.config.project(), "repos", self.config.repository()]);
            }
            path.extend(suffix.iter().copied());
        }
        Ok(url)
    }

    /// Server's branch-utils resource, which owns branch deletion.
    fn branch_utils_url(&self) -> Result<Url, BitbucketClientError> {
        let mut url = self.config.base_url().clone();
        {
            let mut path = url
                .path_segments_mut()
                .map_err(|()| invalid_configuration())?;
            path.pop_if_empty();
            path.extend(["rest", "branch-utils", "1.0", "projects"]);
            path.extend([
                self.config.project(),
                "repos",
                self.config.repository(),
                "branches",
            ]);
        }
        Ok(url)
    }

    fn request(
        &self,
        method: Method,
        mut url: Url,
        query: &[(&str, &str)],
        body: &Body<'_>,
    ) -> Result<Request, BitbucketClientError> {
        if !query.is_empty() {
            let mut pairs = url.query_pairs_mut();
            for (name, value) in query {
                pairs.append_pair(name, value);
            }
        }
        let mut request = Request::new(method, url);
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static("application/json"));
        let credential = Zeroizing::new(format!(
            "{}:{}",
            self.config.username(),
            self.config.password()
        ));
        let encoded = Zeroizing::new(format!("Basic {}", STANDARD.encode(credential.as_bytes())));
        let mut authorization =
            HeaderValue::from_str(&encoded).map_err(|_| invalid_configuration())?;
        authorization.set_sensitive(true);
        request.headers_mut().insert(AUTHORIZATION, authorization);
        let (content_type, bytes) = match body {
            Body::None => return Ok(request),
            Body::Json(value) => (
                "application/json".to_owned(),
                serde_json::to_vec(value).map_err(|_| invalid_input())?,
            ),
            Body::Form(fields) => {
                // `Url`'s query serializer is application/x-www-form-urlencoded.
                let mut scratch =
                    Url::parse("https://form.invalid/").map_err(|_| invalid_input())?;
                scratch.query_pairs_mut().extend_pairs(fields.iter());
                (
                    "application/x-www-form-urlencoded".to_owned(),
                    scratch.query().unwrap_or_default().as_bytes().to_vec(),
                )
            }
            Body::Multipart(fields) => multipart(fields)?,
        };
        if bytes.len() > MAX_REQUEST_BYTES {
            return Err(resource_exhausted());
        }
        request.headers_mut().insert(
            CONTENT_TYPE,
            HeaderValue::from_str(&content_type).map_err(|_| invalid_input())?,
        );
        *request.body_mut() = Some(bytes.into());
        Ok(request)
    }

    async fn send(
        &self,
        method: Method,
        url: Url,
        query: &[(&str, &str)],
        body: &Body<'_>,
        effect: bool,
    ) -> Result<BitbucketHttpResponse, BitbucketClientError> {
        let request = self.request(method, url, query, body)?;
        let response = self.transport.execute(request, effect).await?;
        map_http_status(response.status, effect)?;
        Ok(response)
    }

    async fn get_json(
        &self,
        url: Url,
        query: &[(&str, &str)],
    ) -> Result<Value, BitbucketClientError> {
        self.send(Method::GET, url, query, &Body::None, false)
            .await?
            .json
            .ok_or_else(invalid_response)
    }

    /// Every `values` item across a paged listing, bounded.
    async fn paged(
        &self,
        url: Url,
        filters: &[(&str, &str)],
    ) -> Result<Vec<Value>, BitbucketClientError> {
        let mut output = Vec::new();
        if self.cloud() {
            let mut query = filters.to_vec();
            query.push(("pagelen", PAGE_SIZE));
            let mut next = {
                let mut first = url;
                {
                    let mut pairs = first.query_pairs_mut();
                    for (name, value) in &query {
                        pairs.append_pair(name, value);
                    }
                }
                first
            };
            for page in 0.. {
                if page >= MAX_PAGES {
                    return Err(resource_exhausted());
                }
                let body = self.get_json(next.clone(), &[]).await?;
                push_values(&mut output, &body)?;
                match body.get("next") {
                    None | Some(Value::Null) => break,
                    Some(Value::String(link)) => next = self.same_repository_link(link)?,
                    Some(_) => return Err(invalid_response()),
                }
            }
        } else {
            let mut start = String::from("0");
            for page in 0.. {
                if page >= MAX_PAGES {
                    return Err(resource_exhausted());
                }
                let mut query = filters.to_vec();
                query.push(("limit", PAGE_SIZE));
                query.push(("start", &start));
                let body = self.get_json(url.clone(), &query).await?;
                push_values(&mut output, &body)?;
                if body.get("isLastPage").and_then(Value::as_bool) != Some(false) {
                    break;
                }
                let next = body
                    .get("nextPageStart")
                    .and_then(Value::as_u64)
                    .ok_or_else(invalid_response)?;
                start = next.to_string();
            }
        }
        Ok(output)
    }

    /// A provider-supplied absolute link (Cloud `next`, a diff redirect) is
    /// followed only when it stays on this repository's API resource.
    fn same_repository_link(&self, link: &str) -> Result<Url, BitbucketClientError> {
        let base = self.repo_url(&[])?;
        let candidate = base.join(link).map_err(|_| invalid_response())?;
        let prefix = format!("{}/", base.path().trim_end_matches('/'));
        if candidate.origin() != base.origin()
            || !candidate.path().starts_with(&prefix)
            || !candidate.username().is_empty()
            || candidate.password().is_some()
        {
            return Err(invalid_response());
        }
        Ok(candidate)
    }

    async fn list_branches(&self) -> Result<Vec<String>, BitbucketClientError> {
        let values = if self.cloud() {
            self.paged(self.repo_url(&["refs", "branches"])?, &[])
                .await?
        } else {
            self.paged(
                self.repo_url(&["branches"])?,
                &[
                    ("orderBy", "MODIFICATION"),
                    ("details", "true"),
                    ("boostMatches", "false"),
                ],
            )
            .await?
        };
        let field = if self.cloud() { "name" } else { "displayId" };
        values
            .iter()
            .map(|value| {
                value
                    .get(field)
                    .and_then(Value::as_str)
                    .map(ToOwned::to_owned)
                    .ok_or_else(invalid_response)
            })
            .collect()
    }

    /// Cloud addresses source by the branch head's commit hash.
    async fn branch_hash(&self, branch: &str) -> Result<String, BitbucketClientError> {
        let body = self
            .get_json(self.repo_url(&["refs", "branches", branch])?, &[])
            .await?;
        body.get("target")
            .and_then(|target| target.get("hash"))
            .and_then(Value::as_str)
            .filter(|hash| valid_hash(hash))
            .map(ToOwned::to_owned)
            .ok_or_else(invalid_response)
    }

    /// `_read_file`: the file's raw text on `branch`.
    async fn read_text(
        &self,
        file_path: &str,
        branch: &str,
    ) -> Result<String, BitbucketClientError> {
        let segments = path_segments(file_path)?;
        let (url, query) = if self.cloud() {
            let hash = self.branch_hash(branch).await?;
            let mut suffix = vec!["src", hash.as_str()];
            suffix.extend(segments.iter().copied());
            (self.repo_url(&suffix)?, Vec::new())
        } else {
            let mut suffix = vec!["raw"];
            suffix.extend(segments.iter().copied());
            (self.repo_url(&suffix)?, vec![("at", branch)])
        };
        let response = self
            .send(Method::GET, url, &query, &Body::None, false)
            .await?;
        let text = response.text.ok_or_else(invalid_response)?;
        if text.len() > MAX_FILE_BYTES {
            return Err(resource_exhausted());
        }
        Ok(text)
    }

    async fn read_file(
        &self,
        file_path: &str,
        branch: Option<&str>,
        start_line: Option<usize>,
        end_line: Option<usize>,
    ) -> Result<Value, BitbucketClientError> {
        let branch = self.branch_or_active(branch).await?;
        let content = self.read_text(file_path, &branch).await?;
        let slice = slice_lines(&content, start_line, end_line);
        Ok(guard_text_read(
            slice,
            file_path,
            &requested_label(start_line, end_line),
            &content,
        ))
    }

    /// `create_file` and Cloud's `_write_file`: one commit creating or
    /// replacing `file_path`.
    async fn put_file(
        &self,
        file_path: &str,
        content: &str,
        branch: &str,
        message: &str,
        source_commit_id: Option<&str>,
    ) -> Result<(), BitbucketClientError> {
        let segments = path_segments(file_path)?;
        if self.cloud() {
            let fields = [
                ("branch", branch),
                ("message", message),
                (file_path, content),
            ];
            self.send(
                Method::POST,
                self.repo_url(&["src"])?,
                &[],
                &Body::Form(&fields),
                true,
            )
            .await?;
        } else {
            let mut suffix = vec!["browse"];
            suffix.extend(segments.iter().copied());
            let mut fields = vec![
                ("content", content),
                ("message", message),
                ("branch", branch),
            ];
            if let Some(commit) = source_commit_id {
                fields.push(("sourceCommitId", commit));
            }
            self.send(
                Method::PUT,
                self.repo_url(&suffix)?,
                &[],
                &Body::Multipart(&fields),
                true,
            )
            .await?;
        }
        Ok(())
    }

    /// Server's `_write_file` needs the branch head as `sourceCommitId`.
    async fn server_head(&self, branch: &str) -> Result<String, BitbucketClientError> {
        let body = self
            .get_json(
                self.repo_url(&["commits"])?,
                &[("merges", "include"), ("until", branch), ("limit", "1")],
            )
            .await?;
        body.get("values")
            .and_then(Value::as_array)
            .and_then(|values| values.first())
            .and_then(|commit| commit.get("id"))
            .and_then(Value::as_str)
            .map(ToOwned::to_owned)
            .ok_or_else(invalid_response)
    }

    fn pull_request_url(&self, pr_id: u64, suffix: &[&str]) -> Result<Url, BitbucketClientError> {
        let id = pr_id.to_string();
        let mut segments = vec![
            if self.cloud() {
                "pullrequests"
            } else {
                "pull-requests"
            },
            id.as_str(),
        ];
        segments.extend(suffix.iter().copied());
        self.repo_url(&segments)
    }

    async fn post_comment(
        &self,
        pr_id: u64,
        content: &str,
        inline: Option<&Map<String, Value>>,
    ) -> Result<Value, BitbucketClientError> {
        let body = if self.cloud() {
            let mut body = json!({"content": {"raw": content}});
            if let Some(inline) = inline.filter(|inline| !inline.is_empty()) {
                body["inline"] = Value::Object(inline.clone());
            }
            body
        } else {
            json!({"text": content})
        };
        self.send(
            Method::POST,
            self.pull_request_url(pr_id, &["comments"])?,
            &[],
            &Body::Json(&body),
            true,
        )
        .await?
        .json
        .ok_or_else(unknown_outcome)
    }
}

#[async_trait]
impl BitbucketApi for BitbucketClient {
    #[allow(clippy::too_many_lines)] // One source-ordered provider ledger is easier to audit.
    async fn execute(
        &self,
        operation: BitbucketOperation<'_>,
    ) -> Result<Value, BitbucketClientError> {
        let _operation_guard = self.operation_gate.lock().await;
        match operation {
            BitbucketOperation::CreateBranch { branch_name } => {
                validate_text(branch_name, MAX_BRANCH_BYTES)?;
                let source = self.active().await;
                let (url, body) = if self.cloud() {
                    let hash = self.branch_hash(&source).await?;
                    (
                        self.repo_url(&["refs", "branches"])?,
                        json!({"name": branch_name, "target": {"hash": hash}}),
                    )
                } else {
                    (
                        self.repo_url(&["branches"])?,
                        json!({"name": branch_name, "startPoint": source, "message": ""}),
                    )
                };
                let request = self.request(Method::POST, url, &[], &Body::Json(&body))?;
                let response = self.transport.execute(request, true).await?;
                let exists = matches!(
                    response.status,
                    StatusCode::BAD_REQUEST | StatusCode::CONFLICT
                ) && response
                    .text
                    .as_deref()
                    .is_some_and(|text| text.contains("already exists"));
                if !exists {
                    map_http_status(response.status, true)?;
                }
                *self.active_branch.lock().await = branch_name.into();
                Ok(Value::String(if exists {
                    format!("Branch {branch_name} already exists. set it as active")
                } else {
                    format!("Branch {branch_name} created successfully and set as active")
                }))
            }
            BitbucketOperation::DeleteBranch { branch_name } => {
                validate_text(branch_name, MAX_BRANCH_BYTES)?;
                if matches!(branch_name.to_lowercase().as_str(), "main" | "master") {
                    return Ok(Value::String(format!(
                        "Cannot delete branch '{branch_name}'. Deletion of main or master branches is forbidden for safety reasons."
                    )));
                }
                let branches = self.list_branches().await?;
                if !branches.iter().any(|name| name == branch_name) {
                    return bounded_output(Value::String(format!(
                        "Branch '{branch_name}' does not exist in the repository. Available branches: {}",
                        branches
                            .iter()
                            .take(20)
                            .map(String::as_str)
                            .collect::<Vec<_>>()
                            .join(", ")
                    )));
                }
                if self.active().await == branch_name {
                    return Ok(Value::String(format!(
                        "Branch '{branch_name}' cannot be deleted because it is currently the active branch. Please switch to a different branch before deleting."
                    )));
                }
                if self.cloud() {
                    self.send(
                        Method::DELETE,
                        self.repo_url(&["refs", "branches", branch_name])?,
                        &[],
                        &Body::None,
                        true,
                    )
                    .await?;
                } else {
                    let body = json!({"name": branch_name});
                    self.send(
                        Method::DELETE,
                        self.branch_utils_url()?,
                        &[],
                        &Body::Json(&body),
                        true,
                    )
                    .await?;
                }
                Ok(Value::String(format!(
                    "Branch '{branch_name}' has been deleted successfully."
                )))
            }
            BitbucketOperation::ListBranches {
                limit,
                branch_wildcard,
            } => {
                if limit == 0 {
                    return Err(invalid_input());
                }
                if let Some(wildcard) = branch_wildcard {
                    validate_text(wildcard, MAX_BRANCH_BYTES)?;
                }
                let names = self
                    .list_branches()
                    .await?
                    .into_iter()
                    .filter(|name| {
                        branch_wildcard.is_none_or(|pattern| wildcard_matches(pattern, name))
                    })
                    .take(limit)
                    .collect::<Vec<_>>();
                bounded_output(Value::String(format!(
                    "Found branches: {}",
                    names.join(", ")
                )))
            }
            BitbucketOperation::ListFiles {
                path,
                recursive,
                branch,
            } => {
                let branch = self.branch_or_active(branch).await?;
                let base = path.map_or("", |path| path.trim_matches('/'));
                let segments = if base.is_empty() {
                    Vec::new()
                } else {
                    path_segments(base)?
                };
                let mut files = Vec::new();
                if self.cloud() {
                    let hash = self.branch_hash(&branch).await?;
                    let mut suffix = vec!["src", hash.as_str()];
                    suffix.extend(segments.iter().copied());
                    // A trailing slash lists a directory.
                    suffix.push("");
                    let values = self
                        .paged(
                            self.repo_url(&suffix)?,
                            &[
                                ("max_depth", "100"),
                                ("fields", "values.path,next"),
                                ("q", "type=\"commit_file\""),
                            ],
                        )
                        .await?;
                    for value in &values {
                        files.push(required_string(value, "path")?.to_owned());
                    }
                } else {
                    let mut suffix = vec!["files"];
                    suffix.extend(segments.iter().copied());
                    let values = self
                        .paged(self.repo_url(&suffix)?, &[("at", branch.as_str())])
                        .await?;
                    for value in &values {
                        // Server lists paths relative to the requested folder.
                        let relative = value.as_str().ok_or_else(invalid_response)?;
                        files.push(if base.is_empty() {
                            relative.to_owned()
                        } else {
                            format!("{base}/{relative}")
                        });
                    }
                }
                if !recursive {
                    files.retain(|file| {
                        if base.is_empty() {
                            !file.contains('/')
                        } else {
                            file.strip_prefix(base)
                                .and_then(|rest| rest.strip_prefix('/'))
                                .is_some_and(|rest| !rest.contains('/'))
                        }
                    });
                }
                bounded_output(Value::Array(files.into_iter().map(Value::String).collect()))
            }
            BitbucketOperation::CreatePullRequest { pr_json_data } => {
                if pr_json_data.len() > MAX_TEXT_BYTES {
                    return Err(resource_exhausted());
                }
                let data: Value = match serde_json::from_str(pr_json_data) {
                    Ok(data @ Value::Object(_)) => data,
                    _ => {
                        return Ok(Value::String(format!(
                            "Make sure your pr_json matches to data json format {CREATE_PR_DATA}.\nOrigin exception: pr_json_data is not a JSON object"
                        )));
                    }
                };
                let url = self.repo_url(&[if self.cloud() {
                    "pullrequests"
                } else {
                    "pull-requests"
                }])?;
                let request = self.request(Method::POST, url, &[], &Body::Json(&data))?;
                let response = self.transport.execute(request, true).await?;
                if response.status == StatusCode::BAD_REQUEST {
                    return Ok(Value::String(format!(
                        "Make sure your pr_json matches to data json format {CREATE_PR_DATA}.\nOrigin exception: Bitbucket answered 400 Bad Request"
                    )));
                }
                map_http_status(response.status, true)?;
                let created = response.json.ok_or_else(unknown_outcome)?;
                let detail = if self.cloud() {
                    created
                        .pointer("/links/self/href")
                        .and_then(Value::as_str)
                        .map(ToOwned::to_owned)
                        .ok_or_else(unknown_outcome)?
                } else {
                    python_repr(&created)
                };
                bounded_output(Value::String(format!("Successfully created PR\n{detail}")))
            }
            BitbucketOperation::CreateFile {
                file_path,
                contents,
                branch,
            } => {
                if contents.len() > MAX_FILE_BYTES {
                    return Err(resource_exhausted());
                }
                let branch = self.branch_or_active(branch).await?;
                match self.read_text(file_path, &branch).await {
                    Ok(_) => {
                        return Ok(Value::String(format!(
                            "File already exists: {file_path}. Use update_file() to modify existing files."
                        )));
                    }
                    Err(error) if error.code() == BitbucketClientErrorCode::NotFound => {}
                    Err(error) => return Err(error),
                }
                self.put_file(
                    file_path,
                    contents,
                    &branch,
                    &format!("Create {file_path}"),
                    None,
                )
                .await?;
                Ok(Value::String(format!(
                    "File has been created: {file_path}."
                )))
            }
            BitbucketOperation::ReadFile {
                file_path,
                branch,
                start_line,
                end_line,
            } => bounded_output(
                self.read_file(file_path, branch, start_line, end_line)
                    .await?,
            ),
            BitbucketOperation::UpdateFile {
                file_path,
                update_query,
                branch,
            } => {
                if update_query.len() > MAX_TEXT_BYTES {
                    return Err(resource_exhausted());
                }
                path_segments(file_path)?;
                // `edit_file` refuses a non-text path and a query without
                // markers before it reads; an empty probe answers both.
                match apply_update(file_path, "", update_query) {
                    Err(EditErrorCode::UnsupportedFile) => {
                        return Ok(Value::String(format!(
                            "Cannot edit binary/document file '{file_path}'. Supported text formats: markdown, txt, csv, json, xml, html, yaml, code files."
                        )));
                    }
                    Err(EditErrorCode::InvalidMarkers) => {
                        return Ok(Value::String(
                            "No OLD/NEW marker pairs found in file_query. Format: Each marker must be on its own line:\nOLD <<<<\nold text\n>>>> OLD\nNEW <<<<\nnew text\n>>>> NEW"
                                .to_owned(),
                        ));
                    }
                    Err(EditErrorCode::ResourceExhausted) => return Err(resource_exhausted()),
                    _ => {}
                }
                let branch = self.branch_or_active(branch).await?;
                let content = self.read_text(file_path, &branch).await?;
                let updated = match apply_update(file_path, &content, update_query) {
                    Ok(updated) => updated,
                    Err(EditErrorCode::NotFound) => {
                        return Ok(Value::String(format!(
                            "Normalized OLD block not found in {file_path}."
                        )));
                    }
                    Err(EditErrorCode::Ambiguous) => {
                        return Ok(Value::String(format!(
                            "Multiple candidate regions for OLD block in {file_path}; no change applied to avoid ambiguity."
                        )));
                    }
                    Err(EditErrorCode::NoChange) => {
                        return Ok(Value::String(format!(
                            "Edits for {file_path} were applied but the final content is identical to the original. The sequence of OLD/NEW pairs appears to be redundant or self-cancelling. Please simplify or review the update_query."
                        )));
                    }
                    Err(EditErrorCode::ResourceExhausted) => return Err(resource_exhausted()),
                    Err(EditErrorCode::UnsupportedFile | EditErrorCode::InvalidMarkers) => {
                        return Err(invalid_input());
                    }
                };
                let source_commit = if self.cloud() {
                    None
                } else {
                    Some(self.server_head(&branch).await?)
                };
                self.put_file(
                    file_path,
                    &updated,
                    &branch,
                    &format!("Update {file_path}"),
                    source_commit.as_deref(),
                )
                .await?;
                Ok(Value::String(format!("Update {file_path}")))
            }
            BitbucketOperation::SetActiveBranch { branch_name } => {
                validate_text(branch_name, MAX_BRANCH_BYTES)?;
                let branches = self.list_branches().await?;
                if branches.iter().any(|name| name == branch_name) {
                    *self.active_branch.lock().await = branch_name.into();
                    return Ok(Value::String(format!("Switched to branch `{branch_name}`")));
                }
                let listed = |names: &[String]| {
                    python_repr(&Value::Array(
                        names.iter().cloned().map(Value::String).collect(),
                    ))
                };
                let branch_list = if branches.len() <= 30 {
                    listed(&branches)
                } else {
                    let mut head = listed(&branches[..30]);
                    head.pop();
                    format!("{head}, and {} more]", branches.len() - 30)
                };
                bounded_output(Value::String(format!(
                    "Error branch `{branch_name}` does not exist, in repo with current branches: {branch_list}"
                )))
            }
            BitbucketOperation::GetPullRequestCommits { pr_id } => {
                let url = self.pull_request_url(pr_id, &["commits"])?;
                let commits = if self.cloud() {
                    // The SDK reads the first page of `…/commits`.
                    let body = self.get_json(url, &[]).await?;
                    body.get("values")
                        .and_then(Value::as_array)
                        .cloned()
                        .ok_or_else(invalid_response)?
                } else {
                    self.paged(url, &[]).await?
                };
                bounded_output(Value::Array(commits))
            }
            BitbucketOperation::GetPullRequest { pr_id } => {
                let pull_request = self
                    .get_json(self.pull_request_url(pr_id, &[])?, &[])
                    .await?;
                if !pull_request.is_object() {
                    return Err(invalid_response());
                }
                bounded_output(pull_request)
            }
            BitbucketOperation::GetPullRequestChanges { pr_id } => {
                if self.cloud() {
                    // `…/diff` redirects to the repository diff resource.
                    let mut url = self.pull_request_url(pr_id, &["diff"])?;
                    let mut followed = false;
                    loop {
                        let request = self.request(Method::GET, url, &[], &Body::None)?;
                        let response = self.transport.execute(request, false).await?;
                        if response.status.is_redirection() && !followed {
                            let location = response.location.ok_or_else(invalid_response)?;
                            url = self.same_repository_link(&location)?;
                            followed = true;
                            continue;
                        }
                        map_http_status(response.status, false)?;
                        let diff = response.text.ok_or_else(invalid_response)?;
                        return bounded_output(json!({"raw_response": diff}));
                    }
                }
                let changes = self
                    .paged(self.pull_request_url(pr_id, &["changes"])?, &[])
                    .await?;
                bounded_output(Value::Array(changes))
            }
            BitbucketOperation::AddPullRequestComment {
                pr_id,
                content,
                inline,
            } => {
                if content.len() > MAX_TEXT_BYTES {
                    return Err(resource_exhausted());
                }
                if !self.cloud() && inline.is_some_and(|inline| !inline.is_empty()) {
                    return Ok(Value::String(format!(
                        "Can't add comment to pull request `{pr_id}` due to error:\ninline comments use the Bitbucket Cloud {{from, to, path}} shape and are not supported on Bitbucket Server; omit inline to add a general comment"
                    )));
                }
                let created = self.post_comment(pr_id, content, inline).await?;
                if self.cloud() {
                    return created
                        .pointer("/links/html/href")
                        .and_then(Value::as_str)
                        .map(|href| Value::String(href.to_owned()))
                        .ok_or_else(unknown_outcome);
                }
                bounded_output(created).map_err(|_| unknown_outcome())
            }
            BitbucketOperation::ClosePullRequest { pr_id, message } => {
                if let Some(message) = message
                    && message.len() > MAX_TEXT_BYTES
                {
                    return Err(resource_exhausted());
                }
                let pull_request = self
                    .get_json(self.pull_request_url(pr_id, &[])?, &[])
                    .await?;
                let declined = if self.cloud() {
                    if pull_request.get("state").and_then(Value::as_str) != Some("OPEN") {
                        return Ok(Value::String(format!(
                            "Can't close pull request `{pr_id}` due to error:\nPull Request isn't open"
                        )));
                    }
                    if let Some(message) = message {
                        self.post_comment(pr_id, message, None).await?;
                    }
                    let body = json!({"id": pr_id});
                    self.send(
                        Method::POST,
                        self.pull_request_url(pr_id, &["decline"])?,
                        &[],
                        &Body::Json(&body),
                        true,
                    )
                    .await?
                    .json
                } else {
                    let version = pull_request
                        .get("version")
                        .and_then(Value::as_u64)
                        .ok_or_else(invalid_response)?
                        .to_string();
                    let response = self
                        .send(
                            Method::POST,
                            self.pull_request_url(pr_id, &["decline"])?,
                            &[("version", version.as_str())],
                            &Body::None,
                            true,
                        )
                        .await?;
                    if let Some(message) = message {
                        self.post_comment(pr_id, message, None).await?;
                    }
                    response.json
                };
                let declined = declined.ok_or_else(unknown_outcome)?;
                bounded_output(Value::String(format!(
                    "Successfully closed pull request {pr_id}\n{}",
                    python_repr(&declined)
                )))
                .map_err(|_| unknown_outcome())
            }
            BitbucketOperation::ReadMultipleFiles {
                file_paths,
                branch,
                offset,
                limit,
            } => {
                let end_line = match (offset, limit) {
                    (Some(start), Some(count)) => Some(
                        start
                            .checked_add(count.saturating_sub(1))
                            .ok_or_else(invalid_input)?,
                    ),
                    _ => None,
                };
                let mut results = Map::new();
                let mut cumulative_chars = 0usize;
                for file_path in file_paths {
                    if cumulative_chars >= MAX_OUTPUT_CHARS {
                        results.insert(file_path.to_owned(), Value::String(batch_skip_notice()));
                        continue;
                    }
                    match self.read_file(file_path, branch, offset, end_line).await {
                        Ok(value) => {
                            cumulative_chars =
                                cumulative_chars.saturating_add(measure_result_chars(&value));
                            results.insert(file_path.to_owned(), value);
                        }
                        Err(error) => {
                            results.insert(
                                file_path.to_owned(),
                                Value::String(format!(
                                    "Error reading file: Failed to read file {file_path}: {error}"
                                )),
                            );
                        }
                    }
                }
                bounded_output(Value::Object(results))
            }
            BitbucketOperation::GrepFile {
                file_path,
                pattern,
                branch,
                is_regex,
                context_lines,
            } => {
                let branch = self.branch_or_active(branch).await?;
                let content = self.read_text(file_path, &branch).await?;
                let expression = compile_pattern(pattern, is_regex);
                let found = grep_content(
                    &content,
                    file_path,
                    pattern,
                    expression.as_ref(),
                    context_lines,
                )
                .map_err(|_| resource_exhausted())?;
                Ok(Value::String(found))
            }
        }
    }
}

/// `multipart/form-data` with text fields, as `requests` sends `files=`.
fn multipart(fields: &[(&str, &str)]) -> Result<(String, Vec<u8>), BitbucketClientError> {
    let boundary = if fields.iter().any(|(name, value)| {
        name.contains(MULTIPART_BOUNDARY) || value.contains(MULTIPART_BOUNDARY)
    }) {
        return Err(invalid_input());
    } else {
        MULTIPART_BOUNDARY
    };
    let mut body = Vec::new();
    for (name, value) in fields {
        if name.contains(['"', '\r', '\n']) {
            return Err(invalid_input());
        }
        body.extend_from_slice(
            format!("--{boundary}\r\nContent-Disposition: form-data; name=\"{name}\"\r\n\r\n")
                .as_bytes(),
        );
        body.extend_from_slice(value.as_bytes());
        body.extend_from_slice(b"\r\n");
    }
    body.extend_from_slice(format!("--{boundary}--\r\n").as_bytes());
    Ok((format!("multipart/form-data; boundary={boundary}"), body))
}

fn push_values(output: &mut Vec<Value>, body: &Value) -> Result<(), BitbucketClientError> {
    let values = body
        .get("values")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    if output.len().saturating_add(values.len()) > MAX_ITEMS {
        return Err(resource_exhausted());
    }
    output.extend(values.iter().cloned());
    Ok(())
}

/// A repository-relative file path as URL path segments.
fn path_segments(file_path: &str) -> Result<Vec<&str>, BitbucketClientError> {
    let trimmed = file_path.trim().trim_start_matches('/');
    validate_text(trimmed, MAX_PATH_BYTES)?;
    let segments = trimmed.split('/').collect::<Vec<_>>();
    if segments
        .iter()
        .any(|segment| segment.is_empty() || matches!(*segment, "." | ".."))
    {
        return Err(invalid_input());
    }
    Ok(segments)
}

fn valid_hash(value: &str) -> bool {
    (7..=64).contains(&value.len()) && value.bytes().all(|byte| byte.is_ascii_hexdigit())
}

fn validate_text(value: &str, limit: usize) -> Result<(), BitbucketClientError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || value.chars().any(char::is_control) {
        return Err(invalid_input());
    }
    Ok(())
}

fn required_string<'a>(value: &'a Value, name: &str) -> Result<&'a str, BitbucketClientError> {
    value
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_response)
}

fn bounded_output(value: Value) -> Result<Value, BitbucketClientError> {
    if serde_json::to_vec(&value)
        .map_err(|_| invalid_response())?
        .len()
        > MAX_OUTPUT_BYTES
    {
        return Err(resource_exhausted());
    }
    Ok(value)
}

fn map_http_status(status: StatusCode, effect: bool) -> Result<(), BitbucketClientError> {
    if status.is_success() {
        return Ok(());
    }
    let code = match status {
        StatusCode::REQUEST_TIMEOUT | StatusCode::TOO_MANY_REQUESTS if effect => {
            BitbucketClientErrorCode::UnknownOutcome
        }
        status if status.is_server_error() && effect => BitbucketClientErrorCode::UnknownOutcome,
        StatusCode::BAD_REQUEST | StatusCode::UNPROCESSABLE_ENTITY => {
            BitbucketClientErrorCode::InvalidInput
        }
        StatusCode::UNAUTHORIZED => BitbucketClientErrorCode::Authentication,
        StatusCode::FORBIDDEN => BitbucketClientErrorCode::Authorization,
        StatusCode::NOT_FOUND => BitbucketClientErrorCode::NotFound,
        StatusCode::CONFLICT => BitbucketClientErrorCode::Conflict,
        StatusCode::REQUEST_TIMEOUT => BitbucketClientErrorCode::Timeout,
        StatusCode::TOO_MANY_REQUESTS => BitbucketClientErrorCode::RateLimited,
        status if status.is_server_error() => BitbucketClientErrorCode::DependencyUnavailable,
        _ if effect => BitbucketClientErrorCode::UnknownOutcome,
        _ => BitbucketClientErrorCode::InvalidResponse,
    };
    Err(BitbucketClientError {
        code,
        retryable: !effect
            && matches!(
                code,
                BitbucketClientErrorCode::Timeout
                    | BitbucketClientErrorCode::RateLimited
                    | BitbucketClientErrorCode::DependencyUnavailable
            ),
    })
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> BitbucketClientError {
    if effect {
        return unknown_outcome();
    }
    if source.is_timeout() {
        return BitbucketClientError {
            code: BitbucketClientErrorCode::Timeout,
            retryable: true,
        };
    }
    if source.is_connect() || source.is_request() || source.is_body() || source.is_decode() {
        return BitbucketClientError {
            code: BitbucketClientErrorCode::DependencyUnavailable,
            retryable: true,
        };
    }
    invalid_response()
}

const fn response_failure(effect: bool, error: BitbucketClientError) -> BitbucketClientError {
    if effect { unknown_outcome() } else { error }
}

const fn error(code: BitbucketClientErrorCode) -> BitbucketClientError {
    BitbucketClientError {
        code,
        retryable: false,
    }
}

const fn invalid_configuration() -> BitbucketClientError {
    error(BitbucketClientErrorCode::InvalidConfiguration)
}

const fn invalid_input() -> BitbucketClientError {
    error(BitbucketClientErrorCode::InvalidInput)
}

const fn invalid_response() -> BitbucketClientError {
    error(BitbucketClientErrorCode::InvalidResponse)
}

const fn resource_exhausted() -> BitbucketClientError {
    error(BitbucketClientErrorCode::ResourceExhausted)
}

const fn unknown_outcome() -> BitbucketClientError {
    error(BitbucketClientErrorCode::UnknownOutcome)
}
