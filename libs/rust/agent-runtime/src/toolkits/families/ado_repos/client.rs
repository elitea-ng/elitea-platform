//! `ado_repos` operations (`tools/ado/repos/repos_wrapper.py`, `azure-devops`
//! `GitClient` v7.0) with the SDK's per-toolkit active-branch state.

use std::fmt::Write as _;

use futures::StreamExt as _;
use reqwest::Method;
use serde_json::{Map, Value, json};
use tokio::sync::Mutex;

use crate::toolkits::families::ado::client::{
    AdoBody, AdoClient, AdoClientError, AdoClientErrorCode, AdoRequest, AdoScope, bounded_output,
    invalid_response, resource_exhausted, response_shape_failure,
};
use crate::toolkits::families::ado::config::AdoConnection;
use crate::toolkits::families::ado::format::python_json_string;
use crate::toolkits::families::gitlab_org::edit::{EditErrorCode, apply_update};
use crate::toolkits::families::python_repr::{repr as python_repr, repr_str as python_str_repr};

use super::config::AdoRepository;
use super::diff::bounded_file_diff;

const GIT_API: &str = "7.0";
/// `DEFAULT_MAX_OUTPUT_CHARS` of the SDK's read guard.
const MAX_OUTPUT_CHARS: usize = 200_000;
const MAX_OUTPUT_BYTES: usize = crate::toolkits::families::ado::client::MAX_OUTPUT_BYTES;
/// Base/target reads one `list_pull_request_files` call keeps in flight.
const DIFF_FETCH_CONCURRENCY: usize = 8;
const MAX_PULL_REQUEST_CHANGES: usize = 100;
/// `list_open_pull_requests` reads threads and commits for every PR.
const MAX_OPEN_PULL_REQUESTS: usize = 100;
const MAX_FILE_LISTING: usize = 20_000;
const MAX_INLINE_COMMENTS: usize = 64;
const ZERO_OBJECT_ID: &str = "0000000000000000000000000000000000000000";

/// One inline pull request comment as the SDK reads it from a dict.
pub(crate) struct InlineComment {
    pub(crate) file_path: String,
    pub(crate) comment_text: String,
    pub(crate) left_line: Option<i64>,
    pub(crate) right_line: Option<i64>,
    pub(crate) left_range: Option<(i64, i64)>,
    pub(crate) right_range: Option<(i64, i64)>,
}

pub(crate) struct GetCommits<'a> {
    pub(crate) sha: Option<&'a str>,
    pub(crate) path: Option<&'a str>,
    pub(crate) since: Option<&'a str>,
    pub(crate) until: Option<&'a str>,
    pub(crate) author: Option<&'a str>,
}

pub(crate) struct AdoReposClient {
    ado: AdoClient,
    repository_id: Box<str>,
    base_branch: Box<str>,
    /// `ReposApiWrapper.active_branch`, which several tools move.
    active_branch: Mutex<Box<str>>,
}

impl AdoReposClient {
    pub(crate) fn new(
        connection: AdoConnection,
        repository: AdoRepository,
    ) -> Result<Self, AdoClientError> {
        Ok(Self::with_client(
            crate::toolkits::families::ado::client::client(connection)?,
            repository,
        ))
    }

    pub(crate) fn with_client(ado: AdoClient, repository: AdoRepository) -> Self {
        Self {
            ado,
            repository_id: repository.repository_id,
            base_branch: repository.base_branch,
            active_branch: Mutex::new(repository.active_branch),
        }
    }

    pub(crate) async fn active_branch(&self) -> String {
        self.active_branch.lock().await.to_string()
    }

    async fn set_active(&self, branch: &str) {
        *self.active_branch.lock().await = branch.into();
    }

    fn repository_request<'a>(segments: &'a [&'a str]) -> AdoRequest<'a> {
        AdoRequest::get(segments, GIT_API)
    }

    async fn branch_names(&self) -> Result<Vec<String>, AdoClientError> {
        let segments = [
            "git",
            "repositories",
            self.repository_id.as_ref(),
            "stats",
            "branches",
        ];
        let branches = self
            .ado
            .collection(Self::repository_request(&segments))
            .await?;
        branches
            .iter()
            .map(|branch| {
                branch
                    .get("name")
                    .and_then(Value::as_str)
                    .map(ToOwned::to_owned)
                    .ok_or_else(invalid_response)
            })
            .collect()
    }

    /// `get_branch`: the branch's head commit id.
    async fn branch_head(&self, branch: &str) -> Result<String, AdoClientError> {
        let segments = [
            "git",
            "repositories",
            self.repository_id.as_ref(),
            "stats",
            "branches",
        ];
        let stats = self
            .ado
            .json(
                Self::repository_request(&segments).query("name", branch),
                false,
            )
            .await?;
        stats
            .get("commit")
            .and_then(|commit| commit.get("commitId"))
            .and_then(Value::as_str)
            .map(ToOwned::to_owned)
            .ok_or_else(invalid_response)
    }

    pub(crate) async fn list_branches_in_repo(&self) -> Result<Value, AdoClientError> {
        let names = self.branch_names().await?;
        if names.is_empty() {
            return Ok(Value::String(
                "No branches found in the repository".to_owned(),
            ));
        }
        bounded_output(Value::String(format!(
            "Found {} branches in the repository:\n{}",
            names.len(),
            names.join("\n")
        )))
    }

    pub(crate) async fn set_active_branch(
        &self,
        branch_name: &str,
    ) -> Result<Value, AdoClientError> {
        let names = self.branch_names().await?;
        if names.iter().any(|name| name == branch_name) {
            self.set_active(branch_name).await;
            return Ok(Value::String(format!("Switched to branch `{branch_name}`")));
        }
        bounded_output(Value::String(format!(
            "Error {branch_name} does not exist, in repo with current branches: [{}]",
            names
                .iter()
                .map(|name| python_str_repr(name))
                .collect::<Vec<_>>()
                .join(", ")
        )))
    }

    /// `list_files`: every blob under `directory_path` (full recursion) on
    /// the given branch, which becomes the active branch.
    pub(crate) async fn list_files(
        &self,
        directory_path: &str,
        branch_name: Option<&str>,
    ) -> Result<Value, AdoClientError> {
        if let Some(branch) = branch_name.filter(|branch| !branch.is_empty()) {
            self.set_active(branch).await;
        }
        let branch = self.active_branch().await;
        let segments = ["git", "repositories", self.repository_id.as_ref(), "items"];
        let items = self
            .ado
            .collection(
                Self::repository_request(&segments)
                    .query("scopePath", directory_path)
                    .query("recursionLevel", "Full")
                    .query("includeContentMetadata", "true")
                    .query("versionDescriptor.versionType", "branch")
                    .query("versionDescriptor.version", branch),
            )
            .await?;
        let files = items
            .iter()
            .filter(|item| item.get("gitObjectType").and_then(Value::as_str) == Some("blob"))
            .filter_map(|item| item.get("path").cloned())
            .collect::<Vec<_>>();
        if files.len() > MAX_FILE_LISTING {
            return Ok(Value::String(format!(
                "The directory holds {} files, more than one listing returns. List a subdirectory.",
                files.len()
            )));
        }
        let listing = Value::Array(files);
        if serde_json::to_vec(&listing).map_or(usize::MAX, |encoded| encoded.len())
            > MAX_OUTPUT_BYTES
        {
            return Ok(Value::String(
                "The file listing is larger than one result carries. List a subdirectory."
                    .to_owned(),
            ));
        }
        Ok(listing)
    }

    /// The text of `path` at a branch or commit (`get_item_text`).
    async fn item_text(
        &self,
        path: &str,
        version: &str,
        version_type: &'static str,
    ) -> Result<String, AdoClientError> {
        let segments = ["git", "repositories", self.repository_id.as_ref(), "items"];
        let response = self
            .ado
            .send(
                Self::repository_request(&segments)
                    .query("path", path)
                    .query("versionDescriptor.versionType", version_type)
                    .query("versionDescriptor.version", version)
                    .text(),
                false,
            )
            .await?;
        match response.into_body() {
            AdoBody::Text(text) => Ok(text),
            AdoBody::Empty => Ok(String::new()),
            // A JSON file can still come back typed as JSON.
            AdoBody::Json(value) => serde_json::to_string(&value).map_err(|_| invalid_response()),
        }
    }

    /// `read_file`: the whole file, an optional 1-indexed `offset`/`limit`
    /// slice, and the SDK's `content_too_large` guidance above the limit.
    pub(crate) async fn read_file(
        &self,
        file_path: &str,
        branch: &str,
        offset: Option<i64>,
        limit: Option<i64>,
    ) -> Result<Value, AdoClientError> {
        let branch = if branch.is_empty() {
            self.active_branch().await
        } else {
            branch.to_owned()
        };
        self.set_active(&branch).await;
        let full = match self.item_text(file_path, &branch, "branch").await {
            Ok(full) => full,
            Err(error) if error.code() == AdoClientErrorCode::NotFound => {
                return Ok(Value::String(format!(
                    "File not found `{file_path}` on branch `{branch}`."
                )));
            }
            Err(error) => return Err(error),
        };
        let content = if offset.is_some() || limit.is_some() {
            line_slice(&full, offset.unwrap_or(1), limit)
        } else {
            full.clone()
        };
        let requested = if offset.is_some() || limit.is_some() {
            format!(
                "offset={}, limit={}",
                offset.map_or_else(|| "None".to_owned(), |value| value.to_string()),
                limit.map_or_else(|| "None".to_owned(), |value| value.to_string())
            )
        } else {
            "full file read".to_owned()
        };
        Ok(guard_text_read(&content, file_path, &requested, &full))
    }

    async fn push(
        &self,
        branch: &str,
        old_object_id: &str,
        comment: &str,
        change: Value,
    ) -> Result<Value, AdoClientError> {
        let segments = ["git", "repositories", self.repository_id.as_ref(), "pushes"];
        self.ado
            .json(
                AdoRequest::new(Method::POST, AdoScope::Project, &segments, GIT_API).json(json!({
                    "commits":[{"comment":comment,"changes":[change]}],
                    "refUpdates":[{"name":format!("refs/heads/{branch}"),"oldObjectId":old_object_id}]
                })),
                true,
            )
            .await
    }

    pub(crate) async fn create_file(
        &self,
        file_path: &str,
        file_contents: &str,
        branch_name: Option<&str>,
    ) -> Result<Value, AdoClientError> {
        let branch = branch_name
            .filter(|branch| !branch.is_empty())
            .unwrap_or(&self.base_branch)
            .to_owned();
        self.set_active(&branch).await;
        if branch == self.base_branch.as_ref() {
            return Ok(Value::String(protected_message(&self.base_branch)));
        }
        let segments = ["git", "repositories", self.repository_id.as_ref(), "items"];
        // The SDK treats any failure of this existence probe as "absent".
        if self
            .ado
            .json(
                Self::repository_request(&segments)
                    .query("path", file_path)
                    .query("versionDescriptor.versionType", "branch")
                    .query("versionDescriptor.version", branch.as_str()),
                false,
            )
            .await
            .is_ok()
        {
            return Ok(Value::String(format!(
                "File already exists at `{file_path}` on branch `{branch}`. You must use `update_file` to modify it."
            )));
        }
        let head = self.branch_head(&branch).await?;
        let mut change = json!({"changeType":"add","item":{"path":file_path}});
        if !file_contents.is_empty() {
            change["newContent"] = json!({"content":file_contents,"contentType":"rawtext"});
        }
        self.push(&branch, &head, &format!("Create {file_path}"), change)
            .await?;
        Ok(Value::String(format!("Created file {file_path}")))
    }

    /// `update_file`: OLD/NEW marker edits through the shared edit helper,
    /// then one `edit` push.
    pub(crate) async fn update_file(
        &self,
        branch_name: &str,
        file_path: &str,
        update_query: &str,
    ) -> Result<Value, AdoClientError> {
        let branch = if branch_name.is_empty() {
            self.base_branch.to_string()
        } else {
            branch_name.to_owned()
        };
        self.set_active(&branch).await;
        if branch == self.base_branch.as_ref() {
            return Ok(Value::String(protected_message(&self.base_branch)));
        }
        let current = match self.item_text(file_path, &branch, "branch").await {
            Ok(current) => current,
            Err(error) if error.code() == AdoClientErrorCode::NotFound => {
                return Ok(Value::String(format!(
                    "Failed to read file {file_path}: File not found `{file_path}` on branch `{branch}`."
                )));
            }
            Err(error) => return Err(error),
        };
        let updated = match apply_update(file_path, &current, update_query) {
            Ok(updated) => updated,
            Err(EditErrorCode::UnsupportedFile) => {
                return Ok(Value::String(format!(
                    "Cannot edit binary/document file '{file_path}'. Supported text formats: markdown, txt, csv, json, xml, html, yaml, code files."
                )));
            }
            Err(EditErrorCode::InvalidMarkers) => {
                return Ok(Value::String(
                    "No OLD/NEW marker pairs found in file_query. Format: Each marker must be on its own line:\nOLD <<<<\nold text\n>>>> OLD\nNEW <<<<\nnew text\n>>>> NEW".to_owned(),
                ));
            }
            Err(EditErrorCode::Ambiguous) => {
                return Ok(Value::String(
                    "Update not applied because the OLD block matched more than once".to_owned(),
                ));
            }
            Err(EditErrorCode::NotFound) => {
                return Ok(Value::String(
                    "Update not applied because the OLD block was not found".to_owned(),
                ));
            }
            Err(EditErrorCode::NoChange) => {
                return Ok(Value::String(format!(
                    "Edits for {file_path} were applied but the final content is identical to the original. The sequence of OLD/NEW pairs appears to be redundant or self-cancelling. Please simplify or review the update_query."
                )));
            }
            Err(EditErrorCode::ResourceExhausted) => return Err(resource_exhausted()),
        };
        let head = self.branch_head(&branch).await?;
        self.push(
            &branch,
            &head,
            &format!("Update {file_path}"),
            json!({
                "changeType":"edit",
                "item":{"path":file_path},
                "newContent":{"content":updated,"contentType":"rawtext"}
            }),
        )
        .await?;
        Ok(Value::String(format!("Updated file {file_path}")))
    }

    pub(crate) async fn delete_file(
        &self,
        branch_name: &str,
        file_path: &str,
    ) -> Result<Value, AdoClientError> {
        let head = self.branch_head(branch_name).await?;
        self.push(
            branch_name,
            &head,
            &format!("Delete {file_path}"),
            json!({"changeType":"delete","item":{"path":file_path}}),
        )
        .await?;
        Ok(Value::String(format!("Deleted file {file_path}")))
    }

    /// `create_branch`: from the active branch's head, then made active.
    pub(crate) async fn create_branch(&self, branch_name: &str) -> Result<Value, AdoClientError> {
        if branch_name.chars().any(char::is_whitespace) {
            return Ok(Value::String(format!(
                "Branch '{branch_name}' contains spaces. Please remove them or use special characters"
            )));
        }
        // The SDK treats a failed lookup as "does not exist yet".
        if self.branch_head(branch_name).await.is_ok() {
            return Ok(Value::String(format!(
                "Branch '{branch_name}' already exists."
            )));
        }
        let source = self.active_branch().await;
        let head = self.branch_head(&source).await?;
        let segments = ["git", "repositories", self.repository_id.as_ref(), "refs"];
        let response = self
            .ado
            .json(
                AdoRequest::new(Method::POST, AdoScope::Project, &segments, GIT_API).json(json!([{
                    "name":format!("refs/heads/{branch_name}"),
                    "oldObjectId":ZERO_OBJECT_ID,
                    "newObjectId":head
                }])),
                true,
            )
            .await?;
        // The provider answers 200 with `success: false` for a refused ref
        // update; the SDK ignores that and reports success.
        if let Some(refused) = response
            .get("value")
            .and_then(Value::as_array)
            .and_then(|updates| {
                updates
                    .iter()
                    .find(|update| update.get("success").and_then(Value::as_bool) == Some(false))
            })
        {
            let status = refused
                .get("updateStatus")
                .and_then(Value::as_str)
                .unwrap_or("unknown");
            return Ok(Value::String(format!(
                "Failed to create branch. Error: {status}"
            )));
        }
        self.set_active(branch_name).await;
        Ok(Value::String(format!(
            "Branch '{branch_name}' created successfully, and set as current active branch."
        )))
    }

    async fn pull_request_parts(
        &self,
        pull_request_id: &str,
    ) -> Result<(Vec<Value>, Vec<Value>), AdoClientError> {
        let threads_segments = [
            "git",
            "repositories",
            self.repository_id.as_ref(),
            "pullRequests",
            pull_request_id,
            "threads",
        ];
        let threads = self
            .ado
            .collection(Self::repository_request(&threads_segments))
            .await?;
        let commits_segments = [
            "git",
            "repositories",
            self.repository_id.as_ref(),
            "pullRequests",
            pull_request_id,
            "commits",
        ];
        let commits = self
            .ado
            .collection(Self::repository_request(&commits_segments))
            .await?;
        Ok((threads, commits))
    }

    /// `parse_pull_requests` for one pull request, as an ordered Python dict.
    async fn parse_pull_request(&self, pull_request: &Value) -> Result<PyDict, AdoClientError> {
        let id = pull_request
            .get("pullRequestId")
            .and_then(Value::as_u64)
            .ok_or_else(invalid_response)?;
        let (threads, commits) = self.pull_request_parts(&id.to_string()).await?;
        let commits = commits
            .iter()
            .map(|commit| {
                PyValue::Dict(vec![
                    ("commit_id", json_value(commit.get("commitId"))),
                    ("comment", json_value(commit.get("comment"))),
                ])
            })
            .collect();
        let mut comments = Vec::new();
        for thread in &threads {
            let status = thread
                .get("status")
                .filter(|status| status.as_str().is_some_and(|status| !status.is_empty()))
                .cloned()
                .unwrap_or(Value::Null);
            for comment in thread
                .get("comments")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
            {
                comments.push(PyValue::Dict(vec![
                    ("id", json_value(comment.get("id"))),
                    (
                        "author",
                        json_value(
                            comment
                                .get("author")
                                .and_then(|author| author.get("displayName")),
                        ),
                    ),
                    ("content", json_value(comment.get("content"))),
                    (
                        "published_date",
                        PyValue::Json(comment.get("publishedDate").and_then(Value::as_str).map_or(
                            Value::Null,
                            |date| {
                                Value::String(strftime_utc(date).unwrap_or_else(|| date.to_owned()))
                            },
                        )),
                    ),
                    ("status", PyValue::Json(status.clone())),
                ]));
            }
        }
        let description = pull_request
            .get("description")
            .filter(|description| !description.is_null())
            .cloned()
            .unwrap_or_else(|| Value::String(String::new()));
        Ok(vec![
            ("title", json_value(pull_request.get("title"))),
            ("description", PyValue::Json(description)),
            ("pull_request_id", PyValue::Json(Value::from(id))),
            ("commits", PyValue::List(commits)),
            ("comments", PyValue::List(comments)),
            (
                "source_branch",
                json_value(pull_request.get("sourceRefName")),
            ),
            (
                "target_branch",
                json_value(pull_request.get("targetRefName")),
            ),
        ])
    }

    pub(crate) async fn list_open_pull_requests(&self) -> Result<Value, AdoClientError> {
        let segments = [
            "git",
            "repositories",
            self.repository_id.as_ref(),
            "pullrequests",
        ];
        let pull_requests = self
            .ado
            .collection(
                Self::repository_request(&segments)
                    .query("searchCriteria.repositoryId", self.repository_id.as_ref())
                    .query("searchCriteria.status", "active"),
            )
            .await?;
        if pull_requests.is_empty() {
            return Ok(Value::String("No open pull requests available".to_owned()));
        }
        if pull_requests.len() > MAX_OPEN_PULL_REQUESTS {
            return Ok(Value::String(format!(
                "Found {} open pull requests, more than the {MAX_OPEN_PULL_REQUESTS} one call reads with their threads and commits. Read them by id with get_pull_request.",
                pull_requests.len()
            )));
        }
        let mut parsed = Vec::with_capacity(pull_requests.len());
        for pull_request in &pull_requests {
            parsed.push(PyValue::Dict(self.parse_pull_request(pull_request).await?));
        }
        bounded_output(Value::String(format!(
            "Found {} open pull requests:\n{}",
            parsed.len(),
            PyValue::List(parsed).repr()
        )))
    }

    pub(crate) async fn get_pull_request(
        &self,
        pull_request_id: &str,
    ) -> Result<Value, AdoClientError> {
        let segments = ["git", "pullrequests", pull_request_id];
        let pull_request = match self
            .ado
            .json(AdoRequest::get(&segments, GIT_API), false)
            .await
        {
            Ok(pull_request) => pull_request,
            Err(error) if error.code() == AdoClientErrorCode::NotFound => {
                return Ok(Value::String(format!(
                    "Failed to find pull request with '{pull_request_id}' ID."
                )));
            }
            Err(error) => return Err(error),
        };
        let parsed = self.parse_pull_request(&pull_request).await?;
        bounded_output(Value::Array(vec![PyValue::Dict(parsed).json()]))
    }

    /// `list_pull_request_diffs` (served as `list_pull_request_files`): the
    /// last iteration's changes, each `edit` as a unified diff between the
    /// target and source commits, as the SDK's `json.dumps` string.
    #[allow(clippy::too_many_lines)] // One source-ordered ledger of the SDK's diff walk.
    pub(crate) async fn list_pull_request_files(
        &self,
        pull_request_id: &str,
    ) -> Result<Value, AdoClientError> {
        let Ok(id) = pull_request_id.trim().parse::<i64>() else {
            return Ok(Value::String(format!(
                "Passed argument is not INT type: {pull_request_id}.\nError: invalid literal for int() with base 10: {}",
                python_str_repr(pull_request_id)
            )));
        };
        let id = id.to_string();
        let iterations_segments = [
            "git",
            "repositories",
            self.repository_id.as_ref(),
            "pullRequests",
            id.as_str(),
            "iterations",
        ];
        let iterations = self
            .ado
            .collection(Self::repository_request(&iterations_segments))
            .await?;
        let last = iterations.last().ok_or_else(invalid_response)?;
        let iteration = last
            .get("id")
            .and_then(Value::as_u64)
            .ok_or_else(invalid_response)?
            .to_string();
        let commit = |name: &str| {
            last.get(name)
                .and_then(|reference| reference.get("commitId"))
                .and_then(Value::as_str)
                .map(ToOwned::to_owned)
                .ok_or_else(invalid_response)
        };
        let source_commit = commit("sourceRefCommit")?;
        let target_commit = commit("targetRefCommit")?;
        let changes_segments = [
            "git",
            "repositories",
            self.repository_id.as_ref(),
            "pullRequests",
            id.as_str(),
            "iterations",
            iteration.as_str(),
            "changes",
        ];
        let changes = self
            .ado
            .json(Self::repository_request(&changes_segments), false)
            .await?;
        let entries = changes
            .get("changeEntries")
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default();
        if entries.len() > MAX_PULL_REQUEST_CHANGES {
            return Ok(Value::String(format!(
                "Pull request {id} changes {} files, more than the {MAX_PULL_REQUEST_CHANGES} one call diffs.",
                entries.len()
            )));
        }
        let mut changes = Vec::with_capacity(entries.len());
        for change in &entries {
            let path = change
                .get("item")
                .and_then(|item| item.get("path"))
                .and_then(Value::as_str)
                .ok_or_else(invalid_response)?;
            let change_type = change
                .get("changeType")
                .and_then(Value::as_str)
                .ok_or_else(invalid_response)?;
            changes.push((path.to_owned(), change_type.to_owned()));
        }
        // Both sides of every `edit` are read with bounded concurrency
        // (`buffered` keeps the entry order), instead of 2 x 100 serial
        // round-trips. The first failure IN ENTRY ORDER — base before target
        // — answers, as the SDK's serial walk would.
        let (target_commit, source_commit) = (&target_commit, &source_commit);
        let texts = futures::stream::iter(changes.clone())
            .map(|(path, change_type)| async move {
                if change_type != "edit" {
                    return None;
                }
                Some(futures::join!(
                    self.item_text(&path, target_commit, "commit"),
                    self.item_text(&path, source_commit, "commit"),
                ))
            })
            .buffered(DIFF_FETCH_CONCURRENCY)
            .collect::<Vec<_>>()
            .await;
        let mut pending = Vec::with_capacity(changes.len());
        for ((path, change_type), texts) in changes.into_iter().zip(texts) {
            let texts = match texts {
                None => None,
                Some((Err(error), _)) => {
                    return Ok(Value::String(format!(
                        "Failed to process base file content for path: {path}: Failed to get item text. Error: {error}"
                    )));
                }
                Some((Ok(_), Err(error))) => {
                    return Ok(Value::String(format!(
                        "Failed to process target file content for path: {path}: Failed to get item text. Error: {error}"
                    )));
                }
                Some((Ok(base), Ok(target))) => Some((base, target)),
            };
            pending.push((path, change_type, texts));
        }
        // `generate_diff` is a `difflib.SequenceMatcher` port, O(n·m) in
        // lines: it runs on the blocking pool, never on the async executor.
        let data = tokio::task::spawn_blocking(move || {
            pending
                .into_iter()
                .map(|(path, change_type, texts)| {
                    let diff = match texts {
                        Some((base, target)) => {
                            bounded_file_diff(&change_type, &base, &target, &path)
                        }
                        None => format!("Change Type: {change_type}"),
                    };
                    (path, diff)
                })
                .collect::<Vec<_>>()
        })
        .await
        .map_err(|_| invalid_response())?;
        let mut output = String::from("[");
        for (index, (path, diff)) in data.iter().enumerate() {
            if index > 0 {
                output.push_str(", ");
            }
            let _ = write!(
                output,
                "{{\"path\": {}, \"diff\": {}}}",
                python_json_string(path),
                python_json_string(diff)
            );
        }
        output.push(']');
        bounded_output(Value::String(output))
    }

    /// `get_work_items`: the first ten work item ids linked to the PR.
    pub(crate) async fn get_work_items(
        &self,
        pull_request_id: u64,
    ) -> Result<Value, AdoClientError> {
        let id = pull_request_id.to_string();
        let segments = [
            "git",
            "repositories",
            self.repository_id.as_ref(),
            "pullRequests",
            id.as_str(),
            "workitems",
        ];
        let references = self
            .ado
            .collection(Self::repository_request(&segments))
            .await?;
        Ok(Value::Array(
            references
                .iter()
                .take(10)
                .map(|reference| reference.get("id").cloned().unwrap_or(Value::Null))
                .collect(),
        ))
    }

    async fn create_thread(
        &self,
        pull_request_id: &str,
        thread: Value,
    ) -> Result<Value, AdoClientError> {
        let segments = [
            "git",
            "repositories",
            self.repository_id.as_ref(),
            "pullRequests",
            pull_request_id,
            "threads",
        ];
        self.ado
            .json(
                AdoRequest::new(Method::POST, AdoScope::Project, &segments, GIT_API).json(thread),
                true,
            )
            .await
    }

    /// `comment_on_pull_request` with inline comments: every comment is
    /// checked before the first thread is created.
    pub(crate) async fn comment_inline(
        &self,
        pull_request_id: u64,
        comments: &[InlineComment],
    ) -> Result<Value, AdoClientError> {
        let id = pull_request_id.to_string();
        let mut results = Vec::with_capacity(comments.len());
        for comment in comments {
            let position = |line: i64| json!({"line":line,"offset":1});
            let mut context = Map::new();
            context.insert(
                "filePath".to_owned(),
                Value::String(comment.file_path.clone()),
            );
            let mut message = format!("Comment added to file '{}'", comment.file_path);
            if let Some((start, end)) = comment.right_range {
                context.insert("rightFileStart".to_owned(), position(start));
                context.insert("rightFileEnd".to_owned(), position(end));
                let _ = write!(message, " (right file lines {start}-{end})");
            } else if let Some(line) = comment.right_line {
                context.insert("rightFileStart".to_owned(), position(line));
                context.insert("rightFileEnd".to_owned(), position(line));
                let _ = write!(message, " (right file line {line})");
            }
            if let Some((start, end)) = comment.left_range {
                context.insert("leftFileStart".to_owned(), position(start));
                context.insert("leftFileEnd".to_owned(), position(end));
                let _ = write!(message, " (left file lines {start}-{end})");
            } else if let Some(line) = comment.left_line {
                context.insert("leftFileStart".to_owned(), position(line));
                context.insert("leftFileEnd".to_owned(), position(line));
                let _ = write!(message, " (left file line {line})");
            }
            self.create_thread(
                &id,
                json!({
                    "comments":[{"commentType":"text","content":comment.comment_text}],
                    "status":"active",
                    "threadContext":context
                }),
            )
            .await?;
            results.push(message);
        }
        Ok(Value::String(format!(
            "Successfully added {} comments:\n{}",
            results.len(),
            results.join("\n")
        )))
    }

    /// `comment_on_pull_request` with `comment_query` `"<id>\n\n<text>"`.
    pub(crate) async fn comment_query(&self, comment_query: &str) -> Result<Value, AdoClientError> {
        let head = comment_query.split("\n\n").next().unwrap_or_default();
        let Some(id) = python_int(head) else {
            return Ok(Value::String(format!(
                "Invalid input parameters: invalid literal for int() with base 10: {}",
                python_str_repr(head)
            )));
        };
        let id_text = id.to_string();
        // `comment_query[len(str(pull_request_id)) + 2:]`, by code point.
        let text = comment_query
            .chars()
            .skip(id_text.chars().count() + 2)
            .collect::<String>();
        self.create_thread(
            &id_text,
            json!({"comments":[{"commentType":"text","content":text}],"status":"active"}),
        )
        .await?;
        Ok(Value::String(format!("Commented on pull request {id}")))
    }

    pub(crate) async fn create_pull_request(
        &self,
        title: &str,
        body: &str,
        target_branch: &str,
        source_branch: &str,
    ) -> Result<Value, AdoClientError> {
        if source_branch == target_branch {
            return Ok(Value::String(format!(
                "Cannot create a pull request because the source branch '{source_branch}' is the same as the target branch '{target_branch}'"
            )));
        }
        let segments = [
            "git",
            "repositories",
            self.repository_id.as_ref(),
            "pullrequests",
        ];
        match self
            .ado
            .json(
                AdoRequest::new(Method::POST, AdoScope::Project, &segments, GIT_API).json(json!({
                    "sourceRefName":format!("refs/heads/{source_branch}"),
                    "targetRefName":format!("refs/heads/{target_branch}"),
                    "title":title,
                    "description":body,
                    "reviewers":[]
                })),
                true,
            )
            .await
        {
            Ok(created) => Ok(Value::String(format!(
                "Successfully created PR with ID {}",
                created
                    .get("pullRequestId")
                    .and_then(Value::as_u64)
                    .ok_or_else(|| response_shape_failure(true))?
            ))),
            Err(error) => match error.model_visible_message() {
                Some(message) => Ok(Value::String(format!(
                    "Unable to create pull request due to error: {message}"
                ))),
                None => Err(error),
            },
        }
    }

    pub(crate) async fn get_commits(
        &self,
        request: GetCommits<'_>,
    ) -> Result<Value, AdoClientError> {
        let convert = |value: Option<&str>| -> Result<Option<String>, String> {
            value
                .map(|value| {
                    python_datetime_str(value).ok_or_else(|| {
                        format!("Invalid isoformat string: {}", python_str_repr(value))
                    })
                })
                .transpose()
        };
        let (from, to) = match (convert(request.since), convert(request.until)) {
            (Ok(from), Ok(to)) => (from, to),
            (Err(message), _) | (_, Err(message)) => {
                return Ok(Value::String(format!(
                    "Unable to retrieve commits due to error:\n{message}"
                )));
            }
        };
        let segments = [
            "git",
            "repositories",
            self.repository_id.as_ref(),
            "commits",
        ];
        let mut http = Self::repository_request(&segments);
        if let Some(sha) = request.sha {
            http = http
                .query("searchCriteria.itemVersion.versionType", "commit")
                .query("searchCriteria.itemVersion.version", sha);
        }
        let commits = self
            .ado
            .collection(
                http.query_opt("searchCriteria.itemPath", request.path)
                    .query_opt("searchCriteria.fromDate", from)
                    .query_opt("searchCriteria.toDate", to)
                    .query_opt("searchCriteria.author", request.author),
            )
            .await?;
        bounded_output(Value::Array(
            commits
                .iter()
                .map(|commit| {
                    let author = commit.get("author");
                    json!({
                        "sha":commit.get("commitId").cloned().unwrap_or(Value::Null),
                        "author":author.and_then(|author| author.get("name")).cloned().unwrap_or(Value::Null),
                        "createdAt":author
                            .and_then(|author| author.get("date"))
                            .and_then(Value::as_str)
                            .map_or(Value::Null, |date| {
                                Value::String(python_datetime_str(date).unwrap_or_else(|| date.to_owned()))
                            }),
                        "message":commit.get("comment").cloned().unwrap_or(Value::Null),
                        "url":commit.get("remoteUrl").cloned().unwrap_or(Value::Null)
                    })
                })
                .collect(),
        ))
    }
}

/// Validated inline comments, or the SDK's model-visible refusal.
pub(crate) fn inline_comments(values: &[Value]) -> Result<Vec<InlineComment>, String> {
    if values.len() > MAX_INLINE_COMMENTS {
        return Err(format!(
            "Invalid input parameters: at most {MAX_INLINE_COMMENTS} inline comments per call."
        ));
    }
    let mut comments = Vec::with_capacity(values.len());
    for value in values {
        let Some(object) = value.as_object() else {
            return Err("An error occurred:\ninline comments must be objects".to_owned());
        };
        let text = |key: &str| -> Result<String, String> {
            match object.get(key) {
                Some(Value::String(value)) => Ok(value.clone()),
                Some(other) => Ok(other.to_string()),
                None => Err(format!("An error occurred:\n'{key}'")),
            }
        };
        let line = |key: &str| {
            object
                .get(key)
                .and_then(Value::as_i64)
                .filter(|line| *line != 0)
        };
        let range = |key: &str| -> Result<Option<(i64, i64)>, String> {
            match object.get(key) {
                None | Some(Value::Null) => Ok(None),
                Some(Value::Array(values)) if values.is_empty() => Ok(None),
                Some(Value::Array(values)) => match values.as_slice() {
                    [start, end] => match (start.as_i64(), end.as_i64()) {
                        (Some(start), Some(end)) => Ok(Some((start, end))),
                        _ => Err(format!(
                            "Invalid input parameters: `{key}` must be a tuple (line_start, line_end)"
                        )),
                    },
                    _ => Err(format!(
                        "Invalid input parameters: `{key}` must be a tuple (line_start, line_end)"
                    )),
                },
                Some(_) => Err(format!(
                    "Invalid input parameters: `{key}` must be a tuple (line_start, line_end)"
                )),
            }
        };
        let comment = InlineComment {
            file_path: text("file_path")?,
            comment_text: text("comment_text")?,
            left_line: line("left_line"),
            right_line: line("right_line"),
            right_range: range("right_range")?,
            left_range: range("left_range")?,
        };
        if comment.left_line.is_none()
            && comment.right_line.is_none()
            && comment.left_range.is_none()
            && comment.right_range.is_none()
        {
            return Err("Invalid input parameters: Comment must specify either `left_line`, `right_line`, `left_range`, or `right_range`.".to_owned());
        }
        comments.push(comment);
    }
    Ok(comments)
}

fn protected_message(base_branch: &str) -> String {
    format!(
        "You're attempting to commit directly to the {base_branch} branch, which is protected. Please create a new branch and try again."
    )
}

/// `apply_line_slice(content, offset, limit)` over `splitlines(keepends=True)`.
fn line_slice(content: &str, offset: i64, limit: Option<i64>) -> String {
    if content.is_empty() {
        return String::new();
    }
    let lines = crate::toolkits::families::ado::format::python_lines(content);
    let start = usize::try_from(offset.saturating_sub(1).max(0)).unwrap_or(usize::MAX);
    let end = match limit {
        Some(limit) => start.saturating_add(usize::try_from(limit.max(0)).unwrap_or(usize::MAX)),
        None => lines.len(),
    };
    lines
        .get(start.min(lines.len())..end.min(lines.len()))
        .map(<[&str]>::concat)
        .unwrap_or_default()
}

/// `guard_text_read` with the repos wrapper's offset/limit relabelling.
fn guard_text_read(content: &str, file_path: &str, requested: &str, full_content: &str) -> Value {
    let actual_chars = content.chars().count();
    let serialized = serde_json::to_vec(&Value::String(content.to_owned()))
        .map_or(usize::MAX, |encoded| encoded.len());
    if actual_chars <= MAX_OUTPUT_CHARS && serialized <= MAX_OUTPUT_BYTES {
        return Value::String(content.to_owned());
    }
    // `_count_lines`: newlines, plus one for an unterminated last line.
    let total_lines = full_content.matches('\n').count()
        + usize::from(!full_content.is_empty() && !full_content.ends_with('\n'));
    let name = file_path.rsplit('/').next().unwrap_or(file_path);
    let extension = name
        .rfind('.')
        .filter(|index| *index > 0)
        .map_or(String::new(), |index| name[index..].to_ascii_lowercase());
    let full_chars = full_content.chars().count();
    let instruction = if total_lines <= 1 && full_chars > MAX_OUTPUT_CHARS {
        json!({
            "first_class_params":{},
            "extra_params":{},
            "notes":format!(
                "This file has no usable line breaks ({full_chars} characters on a single line) and exceeds the {MAX_OUTPUT_CHARS}-character read limit. Line slicing would return the whole file, so a bounded read is not possible — the full read is refused."
            )
        })
    } else {
        let range_hint = if total_lines > 0 {
            format!("Valid range 1..{total_lines}. ")
        } else {
            String::new()
        };
        json!({
            "first_class_params":{
                "offset":format!("integer (1-indexed, inclusive) — first line to read. {range_hint}Omit to read from the beginning."),
                "limit":"integer — maximum number of lines to return from offset. Omit to read to the end."
            },
            "extra_params":{},
            "notes":"Use start_line/end_line together to read a bounded slice of a large file and keep tokens bounded."
        })
    };
    json!({
        "__result_status__":"content_too_large",
        "schema_version":"1.0",
        "filename":file_path,
        "type":mime_type(&extension),
        "extension":extension,
        "unit":"lines",
        "total_lines":total_lines,
        "read_limits":{"max_output_chars":MAX_OUTPUT_CHARS,"full_read_allowed":false},
        "instruction_for_readFile":instruction,
        "context":{"limit_chars":MAX_OUTPUT_CHARS,"actual_chars":actual_chars,"requested":requested}
    })
}

/// Python `mimetypes.guess_type` for the common source extensions.
fn mime_type(extension: &str) -> &'static str {
    match extension {
        ".py" => "text/x-python",
        ".md" => "text/markdown",
        ".txt" | ".log" => "text/plain",
        ".csv" => "text/csv",
        ".json" => "application/json",
        ".js" | ".mjs" => "text/javascript",
        ".html" | ".htm" => "text/html",
        ".css" => "text/css",
        ".xml" => "application/xml",
        ".sh" => "application/x-sh",
        ".c" | ".h" => "text/x-c",
        ".java" => "text/x-java",
        _ => "application/octet-stream",
    }
}

/// Python `int(text)`: surrounding whitespace, an optional sign, digits and
/// single underscores between digits.
fn python_int(text: &str) -> Option<i64> {
    let trimmed = text.trim();
    let (sign, digits) = match trimmed.strip_prefix('-') {
        Some(rest) => (-1, rest),
        None => (1, trimmed.strip_prefix('+').unwrap_or(trimmed)),
    };
    if digits.is_empty()
        || digits.starts_with('_')
        || digits.ends_with('_')
        || digits.contains("__")
        || !digits
            .chars()
            .all(|character| character.is_ascii_digit() || character == '_')
    {
        return None;
    }
    digits
        .replace('_', "")
        .parse::<i64>()
        .ok()
        .map(|value| sign * value)
}

struct ParsedDateTime {
    date: String,
    time: String,
    micros: u32,
    offset: Option<String>,
    utc_name: bool,
}

/// `datetime.fromisoformat` for the ISO-8601 forms a caller or Azure DevOps
/// sends: a date, `T` or space, `HH[:MM[:SS[.ffffff]]]`, and `Z` or `±HH[:MM]`.
fn parse_iso(value: &str) -> Option<ParsedDateTime> {
    let value = value.trim();
    let date = value.get(..10)?;
    let bytes = date.as_bytes();
    if !(bytes[4] == b'-'
        && bytes[7] == b'-'
        && date
            .bytes()
            .enumerate()
            .all(|(index, byte)| matches!(index, 4 | 7) || byte.is_ascii_digit()))
    {
        return None;
    }
    let month = date[5..7].parse::<u32>().ok()?;
    let day = date[8..10].parse::<u32>().ok()?;
    if !(1..=12).contains(&month) || !(1..=31).contains(&day) {
        return None;
    }
    let rest = &value[10..];
    if rest.is_empty() {
        return Some(ParsedDateTime {
            date: date.to_owned(),
            time: "00:00:00".to_owned(),
            micros: 0,
            offset: None,
            utc_name: false,
        });
    }
    let rest = rest.strip_prefix(['T', ' '])?;
    let (clock, zone) = match rest.find(['Z', 'z', '+', '-']) {
        Some(index) => (&rest[..index], Some(&rest[index..])),
        None => (rest, None),
    };
    let (whole, fraction) = clock
        .split_once(['.', ','])
        .map_or((clock, None), |(whole, fraction)| (whole, Some(fraction)));
    let parts = whole.split(':').collect::<Vec<_>>();
    let field = |index: usize, limit: u32| -> Option<u32> {
        match parts.get(index) {
            None => Some(0),
            Some(part) if part.len() == 2 && part.bytes().all(|byte| byte.is_ascii_digit()) => {
                part.parse::<u32>().ok().filter(|value| *value <= limit)
            }
            Some(_) => None,
        }
    };
    if parts.is_empty() || parts.len() > 3 {
        return None;
    }
    let (hour, minute, second) = (field(0, 23)?, field(1, 59)?, field(2, 59)?);
    let micros = match fraction {
        None => 0,
        Some(fraction)
            if !fraction.is_empty() && fraction.bytes().all(|byte| byte.is_ascii_digit()) =>
        {
            let mut digits = fraction.chars().take(6).collect::<String>();
            while digits.len() < 6 {
                digits.push('0');
            }
            digits.parse::<u32>().ok()?
        }
        Some(_) => return None,
    };
    let (offset, utc_name) = match zone {
        None => (None, false),
        Some("Z" | "z") => (Some("+00:00".to_owned()), true),
        Some(zone) => {
            let sign = &zone[..1];
            let digits = zone[1..].replace(':', "");
            if digits.len() != 2 && digits.len() != 4
                || !digits.bytes().all(|byte| byte.is_ascii_digit())
            {
                return None;
            }
            let hours = &digits[..2];
            let minutes = digits.get(2..4).unwrap_or("00");
            (Some(format!("{sign}{hours}:{minutes}")), false)
        }
    };
    Some(ParsedDateTime {
        date: date.to_owned(),
        time: format!("{hour:02}:{minute:02}:{second:02}"),
        micros,
        offset,
        utc_name,
    })
}

/// `str(datetime.fromisoformat(value))`.
pub(crate) fn python_datetime_str(value: &str) -> Option<String> {
    let parsed = parse_iso(value)?;
    let mut output = format!("{} {}", parsed.date, parsed.time);
    if parsed.micros != 0 {
        let _ = write!(output, ".{:06}", parsed.micros);
    }
    if let Some(offset) = parsed.offset {
        output.push_str(&offset);
    }
    Some(output)
}

/// msrest's datetime with `strftime("%Y-%m-%d %H:%M:%S %Z")`.
fn strftime_utc(value: &str) -> Option<String> {
    let parsed = parse_iso(value)?;
    let zone = if parsed.utc_name {
        "UTC".to_owned()
    } else {
        parsed.offset.unwrap_or_default()
    };
    Some(
        format!("{} {} {zone}", parsed.date, parsed.time)
            .trim_end()
            .to_owned(),
    )
}

/// An insertion-ordered Python value, for results the SDK prints with
/// `str()` (dict order is part of that text).
enum PyValue {
    Json(Value),
    List(Vec<PyValue>),
    Dict(PyDict),
}

type PyDict = Vec<(&'static str, PyValue)>;

fn json_value(value: Option<&Value>) -> PyValue {
    PyValue::Json(value.cloned().unwrap_or(Value::Null))
}

impl PyValue {
    fn repr(&self) -> String {
        match self {
            Self::Json(value) => python_repr(value),
            Self::List(values) => format!(
                "[{}]",
                values.iter().map(Self::repr).collect::<Vec<_>>().join(", ")
            ),
            Self::Dict(entries) => format!(
                "{{{}}}",
                entries
                    .iter()
                    .map(|(key, value)| format!("{}: {}", python_str_repr(key), value.repr()))
                    .collect::<Vec<_>>()
                    .join(", ")
            ),
        }
    }

    fn json(self) -> Value {
        match self {
            Self::Json(value) => value,
            Self::List(values) => Value::Array(values.into_iter().map(Self::json).collect()),
            Self::Dict(entries) => Value::Object(
                entries
                    .into_iter()
                    .map(|(key, value)| (key.to_owned(), value.json()))
                    .collect(),
            ),
        }
    }
}

#[cfg(test)]
pub(in crate::toolkits) fn test_python_datetime_str(value: &str) -> Option<String> {
    python_datetime_str(value)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_line_slice(
    content: &str,
    offset: i64,
    limit: Option<i64>,
) -> String {
    line_slice(content, offset, limit)
}
