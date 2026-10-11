//! The Bitbucket family's operation ledger over the shared Bitbucket client
//! (`elitea_connectors::bitbucket`, ADR-0030 decision 3): one repository on
//! Bitbucket Cloud (REST 2.0) or Bitbucket Server / Data Center (REST 1.0).
//!
//! The SDK reaches both through `atlassian-python-api`; the shared client
//! speaks the same endpoints directly over a bounded HTTPS, no-redirect,
//! no-retry transport with a sensitive Basic credential.

#[cfg(test)]
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use serde_json::{Map, Value, json};
use tokio::sync::Mutex;

use elitea_connectors::bitbucket::client::{
    BitbucketRest, Body, CLIENT_POLICY, invalid_configuration, invalid_input, invalid_response,
    map_http_status, resource_exhausted, unknown_outcome,
};
// The suites build fixtures from these; the ledger itself uses a subset.
#[allow(unused_imports)]
pub(in crate::toolkits) use elitea_connectors::bitbucket::client::{
    BitbucketClientError, BitbucketClientErrorCode, BitbucketHttpResponse, BitbucketTransport,
};
use elitea_connectors::transport::{Method, StatusCode, Url};

use super::config::BitbucketToolkitConfig;
use crate::toolkits::families::connector_client::{IntoAdk, transport};
use crate::toolkits::families::gitlab_org::client::wildcard_matches;
use crate::toolkits::families::gitlab_org::edit::{EditErrorCode, apply_update};
use crate::toolkits::families::python_repr::repr as python_repr;
use crate::toolkits::families::vcs_text::{
    MAX_OUTPUT_CHARS, batch_skip_notice, compile_pattern, grep_content, guard_text_read,
    measure_result_chars, requested_label, slice_lines,
};

const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_FILE_BYTES: usize = 1_024 * 1_024;
const MAX_PATH_BYTES: usize = 1_024;
const MAX_BRANCH_BYTES: usize = 255;
const MAX_TEXT_BYTES: usize = 256 * 1_024;

impl IntoAdk for BitbucketClientError {
    fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code() {
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
            BitbucketClientErrorCode::EgressRefused => (
                ErrorCategory::Forbidden,
                "bitbucket.egress.refused",
                "the Bitbucket host is not on the egress allowlist",
            ),
            BitbucketClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "bitbucket.effect.unknown_outcome",
                "Bitbucket may have applied the requested effect; reconcile it before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable(),
            retry_after_ms: None,
            max_attempts: None,
        })
    }
}

/// The SDK's `bitbucket_constants.create_pr_data`, quoted in its guidance.
pub(in crate::toolkits) const CREATE_PR_DATA: &str = "JSON string describing pull requ\nest structure: i.e. for server side '{ \"title\":\"PR title\", \"description\":\"PR description\", \"state\":\"OPEN\", \"open\":true, \"closed\":false, \"fromRef\":{ \"id\":\"refs/heads/source_branch\" }, \"toRef\":{ \"id\":\"refs/heads/target_branch\" }, \"locked\":false }' and cloud version: '{ \"title\": \"PR title\", \"source\": { \"branch\": { \"name\": \"source_branch\" } }, \"destination\": { \"branch\": { \"name\": \"destination_branch\" } } }'";

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

/// One claim-scoped Bitbucket client with the SDK's invocation-local active
/// branch.
pub(crate) struct BitbucketClient {
    rest: BitbucketRest,
    operation_gate: Mutex<()>,
    active_branch: Mutex<Box<str>>,
}

impl BitbucketClient {
    pub(crate) fn new(config: BitbucketToolkitConfig) -> Result<Self, BitbucketClientError> {
        let transport = transport(&CLIENT_POLICY).map_err(|_| invalid_configuration())?;
        Ok(Self::with_rest(BitbucketRest::new(config, transport)))
    }

    fn with_rest(rest: BitbucketRest) -> Self {
        let active_branch = Mutex::new(rest.config().branch().into());
        Self {
            rest,
            operation_gate: Mutex::new(()),
            active_branch,
        }
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn fixture(
        config: BitbucketToolkitConfig,
        transport: Arc<dyn BitbucketTransport>,
    ) -> Self {
        Self::with_rest(BitbucketRest::with_transport(config, transport))
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

    async fn list_branches(&self) -> Result<Vec<String>, BitbucketClientError> {
        let values = if self.rest.cloud() {
            self.rest
                .paged(self.rest.repo_url(&["refs", "branches"])?, &[])
                .await?
        } else {
            self.rest
                .paged(
                    self.rest.repo_url(&["branches"])?,
                    &[
                        ("orderBy", "MODIFICATION"),
                        ("details", "true"),
                        ("boostMatches", "false"),
                    ],
                )
                .await?
        };
        let field = if self.rest.cloud() {
            "name"
        } else {
            "displayId"
        };
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

    /// `_read_file`: the file's raw text on `branch`.
    async fn read_text(
        &self,
        file_path: &str,
        branch: &str,
    ) -> Result<String, BitbucketClientError> {
        let segments = path_segments(file_path)?;
        let (url, query) = if self.rest.cloud() {
            let hash = self.rest.branch_hash(branch).await?;
            let mut suffix = vec!["src", hash.as_str()];
            suffix.extend(segments.iter().copied());
            (self.rest.repo_url(&suffix)?, Vec::new())
        } else {
            let mut suffix = vec!["raw"];
            suffix.extend(segments.iter().copied());
            (self.rest.repo_url(&suffix)?, vec![("at", branch)])
        };
        let response = self
            .rest
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
        if self.rest.cloud() {
            let fields = [
                ("branch", branch),
                ("message", message),
                (file_path, content),
            ];
            self.rest
                .send(
                    Method::POST,
                    self.rest.repo_url(&["src"])?,
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
            self.rest
                .send(
                    Method::PUT,
                    self.rest.repo_url(&suffix)?,
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
            .rest
            .get_json(
                self.rest.repo_url(&["commits"])?,
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
            if self.rest.cloud() {
                "pullrequests"
            } else {
                "pull-requests"
            },
            id.as_str(),
        ];
        segments.extend(suffix.iter().copied());
        self.rest.repo_url(&segments)
    }

    async fn post_comment(
        &self,
        pr_id: u64,
        content: &str,
        inline: Option<&Map<String, Value>>,
    ) -> Result<Value, BitbucketClientError> {
        let body = if self.rest.cloud() {
            let mut body = json!({"content": {"raw": content}});
            if let Some(inline) = inline.filter(|inline| !inline.is_empty()) {
                body["inline"] = Value::Object(inline.clone());
            }
            body
        } else {
            json!({"text": content})
        };
        self.rest
            .send(
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
                let (url, body) = if self.rest.cloud() {
                    let hash = self.rest.branch_hash(&source).await?;
                    (
                        self.rest.repo_url(&["refs", "branches"])?,
                        json!({"name": branch_name, "target": {"hash": hash}}),
                    )
                } else {
                    (
                        self.rest.repo_url(&["branches"])?,
                        json!({"name": branch_name, "startPoint": source, "message": ""}),
                    )
                };
                let request = self
                    .rest
                    .request(Method::POST, url, &[], &Body::Json(&body))?;
                let response = self.rest.transport().execute(request, true).await?;
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
                if self.rest.cloud() {
                    self.rest
                        .send(
                            Method::DELETE,
                            self.rest.repo_url(&["refs", "branches", branch_name])?,
                            &[],
                            &Body::None,
                            true,
                        )
                        .await?;
                } else {
                    let body = json!({"name": branch_name});
                    self.rest
                        .send(
                            Method::DELETE,
                            self.rest.branch_utils_url()?,
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
                if self.rest.cloud() {
                    let hash = self.rest.branch_hash(&branch).await?;
                    let mut suffix = vec!["src", hash.as_str()];
                    suffix.extend(segments.iter().copied());
                    // A trailing slash lists a directory.
                    suffix.push("");
                    let values = self
                        .rest
                        .paged(
                            self.rest.repo_url(&suffix)?,
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
                        .rest
                        .paged(self.rest.repo_url(&suffix)?, &[("at", branch.as_str())])
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
                let url = self.rest.repo_url(&[if self.rest.cloud() {
                    "pullrequests"
                } else {
                    "pull-requests"
                }])?;
                let request = self
                    .rest
                    .request(Method::POST, url, &[], &Body::Json(&data))?;
                let response = self.rest.transport().execute(request, true).await?;
                if response.status == StatusCode::BAD_REQUEST {
                    return Ok(Value::String(format!(
                        "Make sure your pr_json matches to data json format {CREATE_PR_DATA}.\nOrigin exception: Bitbucket answered 400 Bad Request"
                    )));
                }
                map_http_status(response.status, true)?;
                let created = response.json.ok_or_else(unknown_outcome)?;
                let detail = if self.rest.cloud() {
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
                let source_commit = if self.rest.cloud() {
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
                let commits = if self.rest.cloud() {
                    // The SDK reads the first page of `…/commits`.
                    let body = self.rest.get_json(url, &[]).await?;
                    body.get("values")
                        .and_then(Value::as_array)
                        .cloned()
                        .ok_or_else(invalid_response)?
                } else {
                    self.rest.paged(url, &[]).await?
                };
                bounded_output(Value::Array(commits))
            }
            BitbucketOperation::GetPullRequest { pr_id } => {
                let pull_request = self
                    .rest
                    .get_json(self.pull_request_url(pr_id, &[])?, &[])
                    .await?;
                if !pull_request.is_object() {
                    return Err(invalid_response());
                }
                bounded_output(pull_request)
            }
            BitbucketOperation::GetPullRequestChanges { pr_id } => {
                if self.rest.cloud() {
                    // `…/diff` redirects to the repository diff resource.
                    let mut url = self.pull_request_url(pr_id, &["diff"])?;
                    let mut followed = false;
                    loop {
                        let request = self.rest.request(Method::GET, url, &[], &Body::None)?;
                        let response = self.rest.transport().execute(request, false).await?;
                        if response.status.is_redirection() && !followed {
                            let location = response.location.ok_or_else(invalid_response)?;
                            url = self.rest.same_repository_link(&location)?;
                            followed = true;
                            continue;
                        }
                        map_http_status(response.status, false)?;
                        let diff = response.text.ok_or_else(invalid_response)?;
                        return bounded_output(json!({"raw_response": diff}));
                    }
                }
                let changes = self
                    .rest
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
                if !self.rest.cloud() && inline.is_some_and(|inline| !inline.is_empty()) {
                    return Ok(Value::String(format!(
                        "Can't add comment to pull request `{pr_id}` due to error:\ninline comments use the Bitbucket Cloud {{from, to, path}} shape and are not supported on Bitbucket Server; omit inline to add a general comment"
                    )));
                }
                let created = self.post_comment(pr_id, content, inline).await?;
                if self.rest.cloud() {
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
                    .rest
                    .get_json(self.pull_request_url(pr_id, &[])?, &[])
                    .await?;
                let declined = if self.rest.cloud() {
                    if pull_request.get("state").and_then(Value::as_str) != Some("OPEN") {
                        return Ok(Value::String(format!(
                            "Can't close pull request `{pr_id}` due to error:\nPull Request isn't open"
                        )));
                    }
                    if let Some(message) = message {
                        self.post_comment(pr_id, message, None).await?;
                    }
                    let body = json!({"id": pr_id});
                    self.rest
                        .send(
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
                        .rest
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
