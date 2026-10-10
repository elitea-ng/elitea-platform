//! The single-project GitLab client.
//!
//! It reuses the GitLab Org family's wire layer (`reqwest_transport`, the
//! status and effect-confirmation mapping, the OLD/NEW editor and the
//! merge-request diff positions) and keeps its own operation ledger, because
//! the SDK `gitlab` toolkit differs from `gitlab_org` in messages, defaults,
//! pagination, protected-branch rules and output shapes.

use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use elitea_connectors::gitlab::wire::project_request;
use elitea_connectors::transport::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};
use tokio::sync::Mutex;

use super::config::GitLabToolkitConfig;
use crate::toolkits::families::connector_client::IntoAdk;
use crate::toolkits::families::gitlab_org::client::{
    GitLabOrgClientError, GitLabOrgClientErrorCode, GitLabOrgHttpResponse, GitLabOrgTransport,
    map_http_status, python_issue_list, reqwest_transport, validate_effect_status,
    wildcard_matches,
};
use crate::toolkits::families::gitlab_org::diff::{DiffErrorCode, discussion_position};
use crate::toolkits::families::gitlab_org::edit::{EditErrorCode, apply_update};
use crate::toolkits::families::vcs_text::{
    MAX_OUTPUT_CHARS, batch_skip_notice, compile_pattern, grep_content, guard_text_read,
    measure_result_chars, requested_label, slice_lines,
};

const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_FILE_BYTES: usize = 1_024 * 1_024;
const MAX_WRITABLE_FILE_BYTES: usize = 1_024 * 1_024;
const MAX_PATH_BYTES: usize = 1_024;
const MAX_BRANCH_BYTES: usize = 255;
const MAX_IDENTIFIER_BYTES: usize = 1_024;
const MAX_TEXT_BYTES: usize = 256 * 1_024;
const MAX_PAGES: usize = 10;
const MAX_ITEMS: usize = 1_000;
const MAX_DIFF_OUTPUT_BYTES: usize = 512 * 1_024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum GitLabClientErrorCode {
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

/// Stable provider failure without origin, project, path, body, or token.
pub(crate) struct GitLabClientError {
    code: GitLabClientErrorCode,
    retryable: bool,
}

impl GitLabClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> GitLabClientErrorCode {
        self.code
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(code: GitLabClientErrorCode) -> Self {
        Self {
            code,
            retryable: false,
        }
    }
}

impl From<GitLabOrgClientError> for GitLabClientError {
    fn from(source: GitLabOrgClientError) -> Self {
        Self {
            code: match source.code() {
                GitLabOrgClientErrorCode::InvalidConfiguration => {
                    GitLabClientErrorCode::InvalidConfiguration
                }
                GitLabOrgClientErrorCode::InvalidInput => GitLabClientErrorCode::InvalidInput,
                GitLabOrgClientErrorCode::Authentication => GitLabClientErrorCode::Authentication,
                GitLabOrgClientErrorCode::Authorization => GitLabClientErrorCode::Authorization,
                GitLabOrgClientErrorCode::NotFound => GitLabClientErrorCode::NotFound,
                GitLabOrgClientErrorCode::Conflict => GitLabClientErrorCode::Conflict,
                GitLabOrgClientErrorCode::RateLimited => GitLabClientErrorCode::RateLimited,
                GitLabOrgClientErrorCode::Timeout => GitLabClientErrorCode::Timeout,
                GitLabOrgClientErrorCode::DependencyUnavailable => {
                    GitLabClientErrorCode::DependencyUnavailable
                }
                GitLabOrgClientErrorCode::InvalidResponse => GitLabClientErrorCode::InvalidResponse,
                GitLabOrgClientErrorCode::ResourceExhausted => {
                    GitLabClientErrorCode::ResourceExhausted
                }
                GitLabOrgClientErrorCode::UnknownOutcome => GitLabClientErrorCode::UnknownOutcome,
            },
            retryable: source.retryable(),
        }
    }
}

impl fmt::Debug for GitLabClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("GitLabClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for GitLabClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            GitLabClientErrorCode::InvalidConfiguration => {
                "the GitLab client configuration is invalid"
            }
            GitLabClientErrorCode::InvalidInput => "the GitLab request is invalid",
            GitLabClientErrorCode::Authentication => "GitLab authentication failed",
            GitLabClientErrorCode::Authorization => "GitLab authorization failed",
            GitLabClientErrorCode::NotFound => "the GitLab resource was not found",
            GitLabClientErrorCode::Conflict => "the GitLab resource is in conflict",
            GitLabClientErrorCode::RateLimited => "GitLab rate limited the request",
            GitLabClientErrorCode::Timeout => "the GitLab request timed out",
            GitLabClientErrorCode::DependencyUnavailable => "GitLab is unavailable",
            GitLabClientErrorCode::InvalidResponse => "GitLab returned an invalid response",
            GitLabClientErrorCode::ResourceExhausted => {
                "the GitLab request or response exceeds its approved limit"
            }
            GitLabClientErrorCode::UnknownOutcome => {
                "the GitLab effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for GitLabClientError {}

impl IntoAdk for GitLabClientError {
    fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            GitLabClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "gitlab.configuration.invalid",
                "the GitLab toolkit configuration is invalid",
            ),
            GitLabClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "gitlab.request.invalid",
                "the GitLab request is invalid",
            ),
            GitLabClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "gitlab.authentication.failed",
                "GitLab authentication failed",
            ),
            GitLabClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "gitlab.authorization.failed",
                "GitLab did not authorize the request",
            ),
            GitLabClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "gitlab.resource.not_found",
                "the requested GitLab resource was not found",
            ),
            GitLabClientErrorCode::Conflict => (
                ErrorCategory::InvalidInput,
                "gitlab.resource.conflict",
                "the GitLab resource changed or already exists",
            ),
            GitLabClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "gitlab.rate_limited",
                "GitLab rate limited the request",
            ),
            GitLabClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "gitlab.timeout",
                "the GitLab request timed out",
            ),
            GitLabClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "gitlab.unavailable",
                "GitLab is unavailable",
            ),
            GitLabClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "gitlab.response.invalid",
                "GitLab returned an invalid response",
            ),
            GitLabClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "gitlab.resource_exhausted",
                "the GitLab request or response exceeds the approved limit",
            ),
            GitLabClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "gitlab.effect.unknown_outcome",
                "GitLab may have applied the requested effect; reconcile it before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }
}

/// One SDK `gitlab` tool call, with arguments already shape-checked.
pub(in crate::toolkits) enum GitLabOperation<'a> {
    CreateBranch {
        branch_name: &'a str,
    },
    DeleteBranch {
        branch_name: &'a str,
        force: bool,
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
    ListFolders {
        path: Option<&'a str>,
        recursive: bool,
        branch: Option<&'a str>,
    },
    GetIssues,
    GetIssue {
        issue_number: u64,
    },
    CreatePullRequest {
        title: &'a str,
        body: &'a str,
        branch: &'a str,
    },
    CommentOnIssue {
        comment_query: &'a str,
    },
    CommentOnPr {
        pr_number: u64,
        comment: &'a str,
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
    UpdateFile {
        file_query: &'a str,
        branch: &'a str,
    },
    AppendFile {
        file_path: &'a str,
        content: &'a str,
        branch: &'a str,
    },
    DeleteFile {
        file_path: &'a str,
        branch: Option<&'a str>,
        commit_message: Option<&'a str>,
    },
    SetActiveBranch {
        branch_name: &'a str,
    },
    GetPrChanges {
        pr_number: u64,
    },
    CreatePrChangeComment {
        pr_number: u64,
        file_path: &'a str,
        line_number: usize,
        comment: &'a str,
    },
    GetCommits {
        sha: Option<&'a str>,
        path: Option<&'a str>,
        since: Option<&'a str>,
        until: Option<&'a str>,
        author: Option<&'a str>,
    },
}

#[async_trait]
pub(in crate::toolkits) trait GitLabApi: Send + Sync {
    async fn execute(&self, operation: GitLabOperation<'_>) -> Result<Value, GitLabClientError>;
}

/// One claim-scoped single-project GitLab client with the SDK's
/// invocation-local active branch.
pub(crate) struct GitLabClient {
    config: GitLabToolkitConfig,
    transport: Arc<dyn GitLabOrgTransport>,
    operation_gate: Mutex<()>,
    active_branch: Mutex<Box<str>>,
}

/// A repository file read through the files API.
struct ProviderFile {
    /// `None` when the bytes are not UTF-8 text.
    content: Option<String>,
    last_commit_id: String,
}

impl GitLabClient {
    pub(crate) fn new(config: GitLabToolkitConfig) -> Result<Self, GitLabClientError> {
        let transport = reqwest_transport()?;
        Ok(Self::with_transport(config, transport))
    }

    fn with_transport(config: GitLabToolkitConfig, transport: Arc<dyn GitLabOrgTransport>) -> Self {
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
        config: GitLabToolkitConfig,
        transport: Arc<dyn GitLabOrgTransport>,
    ) -> Self {
        Self::with_transport(config, transport)
    }

    async fn active(&self) -> String {
        self.active_branch.lock().await.to_string()
    }

    async fn set_active(&self, branch: &str) {
        *self.active_branch.lock().await = branch.into();
    }

    /// `branch if branch else self._active_branch`.
    async fn branch_or_active(&self, branch: Option<&str>) -> Result<String, GitLabClientError> {
        match branch {
            Some(branch) => {
                validate_text(branch, MAX_BRANCH_BYTES)?;
                Ok(branch.to_owned())
            }
            None => Ok(self.active().await),
        }
    }

    fn request(
        &self,
        method: Method,
        suffix: &[&str],
        query: &[(&str, String)],
        body: Option<&Value>,
    ) -> Result<Request, GitLabClientError> {
        Ok(project_request(&self.config, method, suffix, query, body)?)
    }

    async fn call(
        &self,
        method: Method,
        suffix: &[&str],
        query: &[(&str, String)],
        body: Option<&Value>,
        effect: bool,
    ) -> Result<GitLabOrgHttpResponse, GitLabClientError> {
        let effect_method = method.clone();
        let request = self.request(method, suffix, query, body)?;
        let response = self.transport.execute(request, effect).await?;
        map_http_status(response.status, effect)?;
        if effect {
            validate_effect_status(&effect_method, response.status)?;
            // A DELETE answers 204 with no body.
            if effect_method == Method::DELETE {
                return Ok(response);
            }
        }
        if !response.json_content_type || response.body.is_none() {
            return Err(if effect {
                unknown_outcome()
            } else {
                invalid_response()
            });
        }
        Ok(response)
    }

    async fn get_object(
        &self,
        suffix: &[&str],
        query: &[(&str, String)],
    ) -> Result<Value, GitLabClientError> {
        let body = self
            .call(Method::GET, suffix, query, None, false)
            .await?
            .body
            .ok_or_else(invalid_response)?;
        if !body.is_object() {
            return Err(invalid_response());
        }
        Ok(body)
    }

    async fn get_page(
        &self,
        suffix: &[&str],
        query: &[(&str, String)],
        max_items: usize,
    ) -> Result<Vec<Value>, GitLabClientError> {
        let body = self
            .call(Method::GET, suffix, query, None, false)
            .await?
            .body
            .ok_or_else(invalid_response)?;
        let Value::Array(values) = body else {
            return Err(invalid_response());
        };
        if values.len() > max_items {
            return Err(resource_exhausted());
        }
        Ok(values)
    }

    /// python-gitlab `list(get_all=True)`: follow `X-Next-Page`, bounded.
    async fn get_all(
        &self,
        suffix: &[&str],
        filters: &[(&str, String)],
    ) -> Result<Vec<Value>, GitLabClientError> {
        let mut output = Vec::new();
        let mut page = 1usize;
        loop {
            if page > MAX_PAGES {
                return Err(resource_exhausted());
            }
            let mut query = filters.to_vec();
            query.push(("per_page", "100".to_owned()));
            query.push(("page", page.to_string()));
            let response = self.call(Method::GET, suffix, &query, None, false).await?;
            let values = response
                .body
                .as_ref()
                .and_then(Value::as_array)
                .ok_or_else(invalid_response)?;
            if output
                .len()
                .checked_add(values.len())
                .is_none_or(|size| size > MAX_ITEMS)
            {
                return Err(resource_exhausted());
            }
            output.extend(values.iter().cloned());
            let Some(next) = response.next_page.as_deref() else {
                break;
            };
            let next = next.parse::<usize>().map_err(|_| invalid_response())?;
            if next <= page {
                return Err(invalid_response());
            }
            page = next;
        }
        Ok(output)
    }

    async fn read_provider_file(
        &self,
        file_path: &str,
        branch: &str,
    ) -> Result<ProviderFile, GitLabClientError> {
        let file = self
            .get_object(
                &["repository", "files", file_path],
                &[("ref", branch.to_owned())],
            )
            .await?;
        let encoded = required_string(&file, "content")?;
        let decoded = STANDARD
            .decode(encoded.as_bytes())
            .map_err(|_| invalid_response())?;
        if decoded.len() > MAX_FILE_BYTES {
            return Err(resource_exhausted());
        }
        let last_commit_id = required_string(&file, "last_commit_id")?;
        validate_text(last_commit_id, MAX_IDENTIFIER_BYTES)?;
        Ok(ProviderFile {
            content: String::from_utf8(decoded).ok(),
            last_commit_id: last_commit_id.to_owned(),
        })
    }

    /// The SDK `read_file`: sets the active branch, then reads and guards.
    async fn read_file(
        &self,
        file_path: &str,
        branch: Option<&str>,
        start_line: Option<usize>,
        end_line: Option<usize>,
    ) -> Result<Value, GitLabClientError> {
        validate_file_path(file_path)?;
        let branch = self.branch_or_active(branch).await?;
        self.set_active(&branch).await;
        let file = self.read_provider_file(file_path, &branch).await?;
        let Some(content) = file.content else {
            return Ok(Value::String(not_text_message(file_path)));
        };
        let slice = slice_lines(&content, start_line, end_line);
        Ok(guard_text_read(
            slice,
            file_path,
            &requested_label(start_line, end_line),
            &content,
        ))
    }

    async fn list_tree(
        &self,
        path: Option<&str>,
        recursive: bool,
        branch: Option<&str>,
        wanted_type: &str,
    ) -> Result<Value, GitLabClientError> {
        let branch = self.branch_or_active(branch).await?;
        let mut filters = vec![("recursive", recursive.to_string())];
        if let Some(path) = path.filter(|path| !path.is_empty()) {
            validate_text(path, MAX_PATH_BYTES)?;
            filters.push(("path", path.to_owned()));
        }
        filters.push(("ref", branch));
        let entries = self.get_all(&["repository", "tree"], &filters).await?;
        let mut paths = Vec::new();
        for entry in entries {
            if required_string(&entry, "type")? == wanted_type {
                paths.push(Value::String(required_string(&entry, "path")?.to_owned()));
            }
        }
        bounded_output(Value::Array(paths))
    }

    /// `_write_file`'s update path: one commit with an `update` action. The
    /// last commit id read with the file fences a concurrent change.
    async fn commit_update(
        &self,
        file_path: &str,
        branch: &str,
        message: &str,
        content: &str,
        last_commit_id: &str,
    ) -> Result<(), GitLabClientError> {
        if content.len() > MAX_WRITABLE_FILE_BYTES {
            return Err(resource_exhausted());
        }
        let payload = json!({
            "branch": branch,
            "commit_message": message,
            "actions": [{
                "action": "update",
                "file_path": file_path,
                "content": content,
                "last_commit_id": last_commit_id
            }]
        });
        self.call(
            Method::POST,
            &["repository", "commits"],
            &[],
            Some(&payload),
            true,
        )
        .await?;
        Ok(())
    }
}

#[async_trait]
impl GitLabApi for GitLabClient {
    #[allow(clippy::too_many_lines)] // One source-ordered provider ledger is easier to audit.
    async fn execute(&self, operation: GitLabOperation<'_>) -> Result<Value, GitLabClientError> {
        let _operation_guard = self.operation_gate.lock().await;
        match operation {
            GitLabOperation::CreateBranch { branch_name } => {
                validate_text(branch_name, MAX_BRANCH_BYTES)?;
                let source = self.active().await;
                let body = json!({"branch": branch_name, "ref": source});
                let request =
                    self.request(Method::POST, &["repository", "branches"], &[], Some(&body))?;
                let response = self.transport.execute(request, true).await?;
                let already_exists = response.status == StatusCode::BAD_REQUEST
                    && response.body.as_ref().is_some_and(branch_already_exists);
                if !already_exists {
                    map_http_status(response.status, true)?;
                    validate_effect_status(&Method::POST, response.status)?;
                }
                self.set_active(branch_name).await;
                Ok(Value::String(if already_exists {
                    format!("Branch {branch_name} already exists. set it as active")
                } else {
                    format!("Branch {branch_name} created successfully and set as active")
                }))
            }
            GitLabOperation::DeleteBranch { branch_name, force } => {
                validate_text(branch_name, MAX_BRANCH_BYTES)?;
                let base = self.config.branch();
                let mut protected = vec!["main", "master"];
                if !protected.contains(&base) {
                    protected.push(base);
                }
                if protected
                    .iter()
                    .any(|name| name.to_lowercase() == branch_name.to_lowercase())
                {
                    return Ok(Value::String(format!(
                        "Cannot delete protected branch '{branch_name}'. Protected branches: {}",
                        protected.join(", ")
                    )));
                }
                let active = self.active().await;
                if active == branch_name && !force {
                    return Ok(Value::String(format!(
                        "Cannot delete active branch '{branch_name}'. Either switch branches first or use force=True parameter."
                    )));
                }
                self.call(
                    Method::DELETE,
                    &["repository", "branches", branch_name],
                    &[],
                    None,
                    true,
                )
                .await?;
                if active == branch_name {
                    self.set_active(base).await;
                    return Ok(Value::String(format!(
                        "Successfully deleted branch '{branch_name}' and reset active branch to '{base}'"
                    )));
                }
                Ok(Value::String(format!(
                    "Successfully deleted branch '{branch_name}'"
                )))
            }
            GitLabOperation::ListBranches {
                limit,
                branch_wildcard,
            } => {
                if limit == 0 {
                    return Err(invalid_input());
                }
                if let Some(wildcard) = branch_wildcard {
                    validate_text(wildcard, MAX_BRANCH_BYTES)?;
                }
                let branches = self.get_all(&["repository", "branches"], &[]).await?;
                let mut names = Vec::new();
                for branch in &branches {
                    let name = required_string(branch, "name")?;
                    if branch_wildcard.is_none_or(|pattern| wildcard_matches(pattern, name)) {
                        if names.len() >= limit {
                            break;
                        }
                        names.push(Value::String(name.to_owned()));
                    }
                }
                bounded_output(Value::Array(names))
            }
            GitLabOperation::ListFiles {
                path,
                recursive,
                branch,
            } => self.list_tree(path, recursive, branch, "blob").await,
            GitLabOperation::ListFolders {
                path,
                recursive,
                branch,
            } => self.list_tree(path, recursive, branch, "tree").await,
            GitLabOperation::GetIssues => {
                // python-gitlab `issues.list(state="opened")`: the first page.
                let issues = self
                    .get_page(&["issues"], &[("state", "opened".to_owned())], 100)
                    .await?;
                if issues.is_empty() {
                    return Ok(Value::String("No open issues available".to_owned()));
                }
                let mut projected = Vec::with_capacity(issues.len());
                for issue in &issues {
                    projected.push((
                        required_string(issue, "title")?,
                        required_u64(issue, "iid")?,
                    ));
                }
                bounded_output(Value::String(format!(
                    "Found {} issues:\n{}",
                    projected.len(),
                    python_issue_list(&projected)
                )))
            }
            GitLabOperation::GetIssue { issue_number } => {
                let issue_id = positive_id(issue_number)?;
                let issue = self.get_object(&["issues", &issue_id], &[]).await?;
                // The SDK reads note pages until it holds more than ten
                // notes; with python-gitlab's 20-item pages that is exactly
                // the first page.
                let notes = self
                    .get_page(
                        &["issues", &issue_id, "notes"],
                        &[("page", "1".to_owned())],
                        100,
                    )
                    .await?;
                let mut comments = Vec::with_capacity(notes.len());
                for note in &notes {
                    let author = note
                        .get("author")
                        .and_then(Value::as_object)
                        .ok_or_else(invalid_response)?;
                    comments.push(json!({
                        "body": required_string(note, "body")?,
                        "user": author.get("username").and_then(Value::as_str).ok_or_else(invalid_response)?
                    }));
                }
                bounded_output(json!({
                    "title": required_string(&issue, "title")?,
                    "body": nullable(&issue, "description")?,
                    "comments": comments
                }))
            }
            GitLabOperation::CreatePullRequest {
                title,
                body,
                branch,
            } => {
                validate_text(branch, MAX_BRANCH_BYTES)?;
                let base = self.config.branch();
                if branch == base {
                    return Ok(Value::String(format!(
                        "Cannot make a pull request because \n            commits are already in the {base} branch"
                    )));
                }
                validate_text(title, 8 * 1_024)?;
                validate_size(body, MAX_TEXT_BYTES)?;
                let payload = json!({
                    "source_branch": branch,
                    "target_branch": base,
                    "title": title,
                    "description": body,
                    "labels": ["created-by-agent"]
                });
                let response = self
                    .call(Method::POST, &["merge_requests"], &[], Some(&payload), true)
                    .await?;
                let iid = response
                    .body
                    .as_ref()
                    .and_then(|value| value.get("iid"))
                    .and_then(Value::as_u64)
                    .ok_or_else(unknown_outcome)?;
                Ok(Value::String(format!(
                    "Successfully created PR number {iid}"
                )))
            }
            GitLabOperation::CommentOnIssue { comment_query } => {
                validate_size(comment_query, MAX_TEXT_BYTES)?;
                let (issue, comment) = comment_query
                    .split_once("\n\n")
                    .map_or((comment_query, ""), |(issue, comment)| (issue, comment));
                let issue_number = issue
                    .trim()
                    .parse::<u64>()
                    .ok()
                    .filter(|number| *number > 0)
                    .ok_or_else(invalid_input)?;
                let issue_id = issue_number.to_string();
                self.get_object(&["issues", &issue_id], &[]).await?;
                let payload = json!({"body": comment});
                self.call(
                    Method::POST,
                    &["issues", &issue_id, "notes"],
                    &[],
                    Some(&payload),
                    true,
                )
                .await?;
                Ok(Value::String(format!("Commented on issue {issue_number}")))
            }
            GitLabOperation::CommentOnPr { pr_number, comment } => {
                let pr_id = positive_id(pr_number)?;
                validate_size(comment, MAX_TEXT_BYTES)?;
                self.get_object(&["merge_requests", &pr_id], &[]).await?;
                let payload = json!({"body": comment});
                self.call(
                    Method::POST,
                    &["merge_requests", &pr_id, "notes"],
                    &[],
                    Some(&payload),
                    true,
                )
                .await?;
                Ok(Value::String(format!(
                    "Commented on merge request {pr_number}"
                )))
            }
            GitLabOperation::CreateFile {
                file_path,
                contents,
                branch,
            } => {
                validate_file_path(file_path)?;
                validate_size(contents, MAX_WRITABLE_FILE_BYTES)?;
                let branch = self.branch_or_active(branch).await?;
                self.set_active(&branch).await;
                match self
                    .get_object(
                        &["repository", "files", file_path],
                        &[("ref", branch.clone())],
                    )
                    .await
                {
                    Ok(_) => {
                        return Ok(Value::String(format!(
                            "File already exists at {file_path}. Use update_file instead"
                        )));
                    }
                    Err(error) if error.code() == GitLabClientErrorCode::NotFound => {}
                    Err(error) => return Err(error),
                }
                let payload = json!({
                    "branch": branch,
                    "commit_message": format!("Create {file_path}"),
                    "content": contents
                });
                self.call(
                    Method::POST,
                    &["repository", "files", file_path],
                    &[],
                    Some(&payload),
                    true,
                )
                .await?;
                Ok(Value::String(format!("Created file {file_path}")))
            }
            GitLabOperation::ReadFile {
                file_path,
                branch,
                start_line,
                end_line,
            } => bounded_output(
                self.read_file(file_path, branch, start_line, end_line)
                    .await?,
            ),
            GitLabOperation::ReadMultipleFiles {
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
                                Value::String(format!("Error reading file: {error}")),
                            );
                        }
                    }
                }
                bounded_output(Value::Object(results))
            }
            GitLabOperation::GrepFile {
                file_path,
                pattern,
                branch,
                is_regex,
                context_lines,
            } => {
                validate_file_path(file_path)?;
                let branch = self.branch_or_active(branch).await?;
                self.set_active(&branch).await;
                let file = self.read_provider_file(file_path, &branch).await?;
                let Some(content) = file.content else {
                    return Ok(Value::String(not_text_message(file_path)));
                };
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
            GitLabOperation::UpdateFile { file_query, branch } => {
                let base = self.config.branch();
                if branch == base {
                    return Ok(Value::String(format!(
                        "You're attempting to commit directly to the {base} branch, which is protected. Please create a new branch and try again."
                    )));
                }
                validate_text(branch, MAX_BRANCH_BYTES)?;
                validate_size(file_query, MAX_TEXT_BYTES)?;
                let lines = file_query.split('\n').collect::<Vec<_>>();
                let Some(first) = lines.iter().position(|line| !line.trim().is_empty()) else {
                    return Ok(Value::String(
                        "Invalid file_query format. Expected first non-empty line to be the file path followed by OLD/NEW blocks."
                            .to_owned(),
                    ));
                };
                let file_path = lines[first].trim();
                let edit = lines[first + 1..].join("\n");
                self.edit_and_commit(file_path, &edit, branch, &format!("Update {file_path}"))
                    .await
            }
            GitLabOperation::AppendFile {
                file_path,
                content,
                branch,
            } => {
                let base = self.config.branch();
                if branch == base {
                    return Ok(Value::String(format!(
                        "You're attempting to commit to the directlyto the {base} branch, which is protected. Please create a new branch and try again."
                    )));
                }
                if content.is_empty() {
                    return Ok(Value::String(
                        "Content to be added is empty. Append file won't be completed".to_owned(),
                    ));
                }
                validate_file_path(file_path)?;
                validate_text(branch, MAX_BRANCH_BYTES)?;
                validate_size(content, MAX_WRITABLE_FILE_BYTES)?;
                self.set_active(branch).await;
                let file = self.read_provider_file(file_path, branch).await?;
                let Some(existing) = file.content else {
                    return Ok(Value::String(not_text_message(file_path)));
                };
                let updated = format!("{existing}\n{content}");
                self.commit_update(
                    file_path,
                    branch,
                    &format!("Append {file_path}"),
                    &updated,
                    &file.last_commit_id,
                )
                .await?;
                Ok(Value::String(format!("Updated file {file_path}")))
            }
            GitLabOperation::DeleteFile {
                file_path,
                branch,
                commit_message,
            } => {
                validate_file_path(file_path)?;
                let branch = self.branch_or_active(branch).await?;
                self.set_active(&branch).await;
                let message = match commit_message {
                    Some(message) => {
                        validate_size(message, MAX_TEXT_BYTES)?;
                        message.to_owned()
                    }
                    None => format!("Delete {file_path}"),
                };
                self.call(
                    Method::DELETE,
                    &["repository", "files", file_path],
                    &[("branch", branch), ("commit_message", message)],
                    None,
                    true,
                )
                .await?;
                Ok(Value::String(format!("Deleted file {file_path}")))
            }
            GitLabOperation::SetActiveBranch { branch_name } => {
                validate_text(branch_name, MAX_BRANCH_BYTES)?;
                self.set_active(branch_name).await;
                Ok(Value::String(format!("Active branch set to {branch_name}")))
            }
            GitLabOperation::GetPrChanges { pr_number } => {
                let pr_id = positive_id(pr_number)?;
                let mr = self.get_object(&["merge_requests", &pr_id], &[]).await?;
                let changes = self
                    .get_object(&["merge_requests", &pr_id, "changes"], &[])
                    .await?;
                Ok(Value::String(format_changes(&mr, &changes)?))
            }
            GitLabOperation::CreatePrChangeComment {
                pr_number,
                file_path,
                line_number,
                comment,
            } => {
                let pr_id = positive_id(pr_number)?;
                validate_text(file_path, MAX_PATH_BYTES)?;
                validate_size(comment, MAX_TEXT_BYTES)?;
                let mr = self.get_object(&["merge_requests", &pr_id], &[]).await?;
                let changes = self
                    .get_object(&["merge_requests", &pr_id, "changes"], &[])
                    .await?;
                let position = match discussion_position(&mr, &changes, file_path, line_number) {
                    Ok(position) => position,
                    Err(DiffErrorCode::InvalidIndex) => {
                        return Ok(Value::String(format!(
                            "Failed to create comment on MR #{pr_number}: change for file {file_path} at diff line {line_number} wasn't found in PR"
                        )));
                    }
                    Err(DiffErrorCode::ResourceExhausted) => return Err(resource_exhausted()),
                    Err(DiffErrorCode::InvalidShape) => return Err(invalid_response()),
                };
                let payload = json!({"body": comment, "position": position});
                self.call(
                    Method::POST,
                    &["merge_requests", &pr_id, "discussions"],
                    &[],
                    Some(&payload),
                    true,
                )
                .await?;
                Ok(Value::String(format!(
                    "Comment added successfully to line {line_number} in {file_path} on MR #{pr_number}"
                )))
            }
            GitLabOperation::GetCommits {
                sha,
                path,
                since,
                until,
                author,
            } => {
                let mut query = Vec::new();
                for (name, value) in [
                    ("ref_name", sha),
                    ("path", path),
                    ("since", since),
                    ("until", until),
                    ("author", author),
                ] {
                    if let Some(value) = value {
                        validate_text(value, MAX_IDENTIFIER_BYTES)?;
                        query.push((name, value.to_owned()));
                    }
                }
                // python-gitlab `commits.list(**params)`: the first page only.
                let commits = self
                    .get_page(&["repository", "commits"], &query, 100)
                    .await?;
                let mut projected = Vec::with_capacity(commits.len());
                for commit in &commits {
                    projected.push(json!({
                        "sha": required_string(commit, "id")?,
                        "author": nullable(commit, "author_name")?,
                        "createdAt": nullable(commit, "created_at")?,
                        "message": nullable(commit, "message")?,
                        "url": nullable(commit, "web_url")?
                    }));
                }
                bounded_output(Value::Array(projected))
            }
        }
    }
}

impl GitLabClient {
    /// `update_file` → `edit_file` → `_write_file`, with the SDK's messages.
    async fn edit_and_commit(
        &self,
        file_path: &str,
        edit: &str,
        branch: &str,
        message: &str,
    ) -> Result<Value, GitLabClientError> {
        validate_file_path(file_path)?;
        // `edit_file` refuses a non-text path and a query without markers
        // before it reads the file; an empty probe answers both offline.
        match apply_update(file_path, "", edit) {
            Err(EditErrorCode::UnsupportedFile) => {
                return Ok(Value::String(format!(
                    "Unable to update file due to error:\nCannot edit binary/document file '{file_path}'. Supported text formats: markdown, txt, csv, json, xml, html, yaml, code files."
                )));
            }
            Err(EditErrorCode::InvalidMarkers) => {
                return Ok(Value::String(
                    "Unable to update file due to error:\nNo OLD/NEW marker pairs found in file_query. Format: Each marker must be on its own line:\nOLD <<<<\nold text\n>>>> OLD\nNEW <<<<\nnew text\n>>>> NEW"
                        .to_owned(),
                ));
            }
            Err(EditErrorCode::ResourceExhausted) => return Err(resource_exhausted()),
            _ => {}
        }
        self.set_active(branch).await;
        let file = self.read_provider_file(file_path, branch).await?;
        let Some(content) = file.content else {
            return Ok(Value::String(not_text_message(file_path)));
        };
        let updated = match apply_update(file_path, &content, edit) {
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
        self.commit_update(file_path, branch, message, &updated, &file.last_commit_id)
            .await?;
        Ok(Value::String(format!("Updated file {file_path}")))
    }
}

/// `get_pr_changes`: `title: …\ndescription: …\n\n` then each change's raw
/// unified diff behind a `diff --git` header.
fn format_changes(merge_request: &Value, changes: &Value) -> Result<String, GitLabClientError> {
    let title = required_string(merge_request, "title")?;
    let description = match merge_request.get("description") {
        None | Some(Value::Null) => "None",
        Some(Value::String(value)) => value,
        Some(_) => return Err(invalid_response()),
    };
    let changes = changes
        .get("changes")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    let mut output = format!("title: {title}\ndescription: {description}\n\n");
    for change in changes {
        let old_path = required_string(change, "old_path")?;
        let new_path = required_string(change, "new_path")?;
        let diff = required_string(change, "diff")?;
        output.push_str("diff --git a/");
        output.push_str(old_path);
        output.push_str(" b/");
        output.push_str(new_path);
        output.push('\n');
        output.push_str(diff);
        output.push('\n');
        if output.len() > MAX_DIFF_OUTPUT_BYTES {
            return Err(resource_exhausted());
        }
    }
    Ok(output)
}

fn not_text_message(file_path: &str) -> String {
    format!(
        "File {file_path} is not UTF-8 text; this runtime reads text files only (the SDK's document and image parsers are not available here)."
    )
}

fn branch_already_exists(value: &Value) -> bool {
    match value {
        Value::String(value) => value.contains("Branch already exists"),
        Value::Array(values) => values.iter().any(branch_already_exists),
        Value::Object(values) => values.values().any(branch_already_exists),
        _ => false,
    }
}

fn positive_id(value: u64) -> Result<String, GitLabClientError> {
    if value == 0 {
        return Err(invalid_input());
    }
    Ok(value.to_string())
}

fn validate_file_path(value: &str) -> Result<(), GitLabClientError> {
    validate_text(value, MAX_PATH_BYTES)?;
    if value.starts_with('/')
        || value
            .split('/')
            .any(|segment| segment.is_empty() || matches!(segment, "." | ".."))
    {
        return Err(invalid_input());
    }
    Ok(())
}

fn validate_text(value: &str, limit: usize) -> Result<(), GitLabClientError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || value.chars().any(char::is_control) {
        return Err(invalid_input());
    }
    Ok(())
}

fn validate_size(value: &str, limit: usize) -> Result<(), GitLabClientError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    Ok(())
}

fn required_string<'a>(value: &'a Value, name: &str) -> Result<&'a str, GitLabClientError> {
    value
        .get(name)
        .and_then(Value::as_str)
        .filter(|value| value.len() <= MAX_RESPONSE_BYTES)
        .ok_or_else(invalid_response)
}

fn nullable(value: &Value, name: &str) -> Result<Value, GitLabClientError> {
    match value.get(name) {
        None | Some(Value::Null) => Ok(Value::Null),
        Some(Value::String(text)) if text.len() <= MAX_RESPONSE_BYTES => {
            Ok(Value::String(text.clone()))
        }
        Some(_) => Err(invalid_response()),
    }
}

fn required_u64(value: &Value, name: &str) -> Result<u64, GitLabClientError> {
    value
        .get(name)
        .and_then(Value::as_u64)
        .ok_or_else(invalid_response)
}

fn bounded_output(value: Value) -> Result<Value, GitLabClientError> {
    if serde_json::to_vec(&value)
        .map_err(|_| invalid_response())?
        .len()
        > MAX_OUTPUT_BYTES
    {
        return Err(resource_exhausted());
    }
    Ok(value)
}

const fn error(code: GitLabClientErrorCode) -> GitLabClientError {
    GitLabClientError {
        code,
        retryable: false,
    }
}

const fn invalid_input() -> GitLabClientError {
    error(GitLabClientErrorCode::InvalidInput)
}

const fn invalid_response() -> GitLabClientError {
    error(GitLabClientErrorCode::InvalidResponse)
}

const fn resource_exhausted() -> GitLabClientError {
    error(GitLabClientErrorCode::ResourceExhausted)
}

const fn unknown_outcome() -> GitLabClientError {
    error(GitLabClientErrorCode::UnknownOutcome)
}
