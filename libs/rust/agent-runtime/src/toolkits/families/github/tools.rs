use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use chrono::{DateTime, NaiveDate, SecondsFormat, Utc};
use regex::{Regex, RegexBuilder};
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{
    GitHubApi, GitHubClient, GitHubClientError, GitHubCodeSearchQuery, GitHubCommitQuery,
    GitHubFileScope, validate_repository,
};
use super::config::{GitHubToolkitConfig, GitHubToolkitConfigError};

const GET_ME: &str = "get_me";
const LIST_BRANCHES: &str = "list_branches_in_repo";
const READ_FILE: &str = "read_file";
const READ_MULTIPLE_FILES: &str = "read_multiple_files";
const GREP_FILE: &str = "grep_file";
const LIST_MAIN_FILES: &str = "list_files_in_main_branch";
const LIST_ACTIVE_FILES: &str = "list_files_in_bot_branch";
const LIST_DIRECTORY_FILES: &str = "get_files_from_directory";
const LIST_ISSUES: &str = "get_issues";
const GET_ISSUE: &str = "get_issue";
const SEARCH_ISSUES: &str = "search_issues";
const LIST_PULL_REQUESTS: &str = "list_open_pull_requests";
const GET_PULL_REQUEST: &str = "get_pull_request";
const LIST_PULL_REQUEST_DIFFS: &str = "list_pull_request_diffs";
const LIST_COMMITS: &str = "get_commits";
const GET_COMMIT_CHANGES: &str = "get_commit_changes";
const COMPARE_COMMITS: &str = "get_commits_diff";
const SEARCH_CODE: &str = "search_code";
const GET_WORKFLOW_STATUS: &str = "get_workflow_status";
const LIST_PROJECT_ISSUES: &str = "list_project_issues";
const MAX_DESCRIPTION_BYTES: usize = 1_000;
const MAX_OUTPUT_CHARS: usize = 200_000;
const MAX_BATCH_FILES: usize = 32;
const MAX_PATTERN_BYTES: usize = 4 * 1_024;
const MAX_CONTEXT_LINES: usize = 32;
const MAX_GREP_MATCHES: usize = 2_000;
const REGEX_SIZE_LIMIT: usize = 2 * 1_024 * 1_024;
const REGEX_DFA_SIZE_LIMIT: usize = 2 * 1_024 * 1_024;

/// Safe failure returned while constructing the first GitHub tool subset.
pub struct GitHubToolsetError {
    code: GitHubToolsetErrorCode,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum GitHubToolsetErrorCode {
    InvalidConfiguration,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

impl GitHubToolsetError {
    #[must_use]
    pub const fn code(&self) -> GitHubToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for GitHubToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("GitHubToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for GitHubToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            GitHubToolsetErrorCode::InvalidConfiguration => {
                "the GitHub toolkit configuration is invalid"
            }
            GitHubToolsetErrorCode::UnsupportedSelection => {
                "the selected GitHub tool profile is not supported"
            }
            GitHubToolsetErrorCode::Client => "the GitHub toolkit client could not be created",
            GitHubToolsetErrorCode::InvalidDefinition => {
                "the GitHub ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for GitHubToolsetError {}

impl From<GitHubToolkitConfigError> for GitHubToolsetError {
    fn from(_: GitHubToolkitConfigError) -> Self {
        Self {
            code: GitHubToolsetErrorCode::InvalidConfiguration,
        }
    }
}

impl From<GitHubClientError> for GitHubToolsetError {
    fn from(_: GitHubClientError) -> Self {
        Self {
            code: GitHubToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for GitHubToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: GitHubToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the first GitHub read-only profile.
///
/// Empty selection still means all 44 current SDK tools, so it is rejected
/// until the full family is ported. A mixed explicit selection keeps only the
/// implemented ordinary reads. This permits a restored SDK toolkit to retain
/// its read capability without exposing an unimplemented effect.
pub fn build_github_read_only_toolset(
    toolkit_name: &str,
    config: GitHubToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, GitHubToolsetError> {
    let (selected, omitted_count) = supported_selection(config.selected_tools())?;
    if omitted_count > 0 {
        tracing::warn!(
            event = "agent_toolkit_tools_skipped",
            reason_code = "unsupported_tool_selection",
            toolkit_type = "github",
            toolkit_name,
            selected_count = config.selected_tools().len(),
            materialized_count = selected.len(),
            omitted_count,
            "unsupported GitHub tools were omitted from the native read-only toolset"
        );
    }
    let repository = config.repository().to_owned();
    let client: Arc<dyn GitHubApi> = Arc::new(GitHubClient::new(config)?);
    build_with_api(toolkit_name, &repository, &selected, policy, &client)
}

fn supported_selection(selected: &[Box<str>]) -> Result<(Vec<String>, usize), GitHubToolsetError> {
    if selected.is_empty() {
        return Err(GitHubToolsetError {
            code: GitHubToolsetErrorCode::UnsupportedSelection,
        });
    }
    let supported = selected
        .iter()
        .filter(|name| is_supported_read(name))
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    if supported.is_empty() {
        return Err(GitHubToolsetError {
            code: GitHubToolsetErrorCode::UnsupportedSelection,
        });
    }
    let omitted_count = selected.len() - supported.len();
    Ok((supported, omitted_count))
}

fn is_supported_read(name: &str) -> bool {
    matches!(
        name,
        GET_ME
            | LIST_BRANCHES
            | READ_FILE
            | READ_MULTIPLE_FILES
            | GREP_FILE
            | LIST_MAIN_FILES
            | LIST_ACTIVE_FILES
            | LIST_DIRECTORY_FILES
            | LIST_ISSUES
            | GET_ISSUE
            | SEARCH_ISSUES
            | LIST_PULL_REQUESTS
            | GET_PULL_REQUEST
            | LIST_PULL_REQUEST_DIFFS
            | LIST_COMMITS
            | GET_COMMIT_CHANGES
            | COMPARE_COMMITS
            | SEARCH_CODE
            | GET_WORKFLOW_STATUS
            | LIST_PROJECT_ISSUES
    )
}

fn build_with_api(
    toolkit_name: &str,
    repository: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn GitHubApi>,
) -> Result<BasicToolset, GitHubToolsetError> {
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(selected.len());
    for name in selected {
        let kind = match name.as_str() {
            GET_ME => GitHubReadToolKind::GetMe,
            LIST_BRANCHES => GitHubReadToolKind::ListBranches,
            READ_FILE => GitHubReadToolKind::ReadFile,
            READ_MULTIPLE_FILES => GitHubReadToolKind::ReadMultipleFiles,
            GREP_FILE => GitHubReadToolKind::GrepFile,
            LIST_MAIN_FILES => GitHubReadToolKind::ListMainFiles,
            LIST_ACTIVE_FILES => GitHubReadToolKind::ListActiveFiles,
            LIST_DIRECTORY_FILES => GitHubReadToolKind::ListDirectoryFiles,
            LIST_ISSUES => GitHubReadToolKind::ListIssues,
            GET_ISSUE => GitHubReadToolKind::GetIssue,
            SEARCH_ISSUES => GitHubReadToolKind::SearchIssues,
            LIST_PULL_REQUESTS => GitHubReadToolKind::ListPullRequests,
            GET_PULL_REQUEST => GitHubReadToolKind::GetPullRequest,
            LIST_PULL_REQUEST_DIFFS => GitHubReadToolKind::ListPullRequestDiffs,
            LIST_COMMITS => GitHubReadToolKind::ListCommits,
            GET_COMMIT_CHANGES => GitHubReadToolKind::GetCommitChanges,
            COMPARE_COMMITS => GitHubReadToolKind::CompareCommits,
            SEARCH_CODE => GitHubReadToolKind::SearchCode,
            GET_WORKFLOW_STATUS => GitHubReadToolKind::GetWorkflowStatus,
            LIST_PROJECT_ISSUES => GitHubReadToolKind::ListProjectIssues,
            _ => {
                return Err(GitHubToolsetError {
                    code: GitHubToolsetErrorCode::UnsupportedSelection,
                });
            }
        };
        tools.push(Arc::new(GitHubReadTool::new(
            kind,
            toolkit_name,
            repository,
            Arc::clone(client),
        )));
    }
    admit_materialized_toolset(toolkit_name, "github", policy, tools).map_err(Into::into)
}

#[derive(Clone, Copy)]
enum GitHubReadToolKind {
    GetMe,
    ListBranches,
    ReadFile,
    ReadMultipleFiles,
    GrepFile,
    ListMainFiles,
    ListActiveFiles,
    ListDirectoryFiles,
    ListIssues,
    GetIssue,
    SearchIssues,
    ListPullRequests,
    GetPullRequest,
    ListPullRequestDiffs,
    ListCommits,
    GetCommitChanges,
    CompareCommits,
    SearchCode,
    GetWorkflowStatus,
    ListProjectIssues,
}

struct GitHubReadTool {
    kind: GitHubReadToolKind,
    client: Arc<dyn GitHubApi>,
    description: Box<str>,
}

impl GitHubReadTool {
    fn new(
        kind: GitHubReadToolKind,
        toolkit_name: &str,
        repository: &str,
        client: Arc<dyn GitHubApi>,
    ) -> Self {
        let action = match kind {
            GitHubReadToolKind::GetMe => {
                "Get details of the authenticated GitHub user and account context."
            }
            GitHubReadToolKind::ListBranches => {
                "List up to 100 branches in the configured GitHub repository."
            }
            GitHubReadToolKind::ReadFile => "Read a UTF-8 file or a bounded inclusive line range.",
            GitHubReadToolKind::ReadMultipleFiles => {
                "Read up to 32 UTF-8 files with one cumulative output budget."
            }
            GitHubReadToolKind::GrepFile => {
                "Search one UTF-8 file with a bounded literal or regular expression."
            }
            GitHubReadToolKind::ListMainFiles => {
                "List all bounded file paths in the configured base branch."
            }
            GitHubReadToolKind::ListActiveFiles => {
                "List all bounded file paths in the configured active branch."
            }
            GitHubReadToolKind::ListDirectoryFiles => {
                "Recursively list bounded file paths below one active-branch directory."
            }
            GitHubReadToolKind::ListIssues => {
                "List up to 100 open issues and pull requests in the configured repository."
            }
            GitHubReadToolKind::GetIssue => "Get one issue or pull request with bounded metadata.",
            GitHubReadToolKind::SearchIssues => {
                "Search issues and pull requests with bounded GitHub query syntax."
            }
            GitHubReadToolKind::ListPullRequests => {
                "List up to 100 open pull requests in the configured repository."
            }
            GitHubReadToolKind::GetPullRequest => {
                "Inspect one pull request with bounded comments and commit summaries."
            }
            GitHubReadToolKind::ListPullRequestDiffs => {
                "List bounded changed-file metadata and GitHub patch fragments for one pull request."
            }
            GitHubReadToolKind::ListCommits => {
                "List up to 100 commits using bounded repository, ref, path, date and author filters."
            }
            GitHubReadToolKind::GetCommitChanges => {
                "Inspect one commit and its bounded, complete changed-file projection."
            }
            GitHubReadToolKind::CompareCommits => {
                "Compare two refs with bounded commit and changed-file summaries."
            }
            GitHubReadToolKind::SearchCode => {
                "Search GitHub code with native qualifiers through one bounded provider page without per-result content fetches."
            }
            GitHubReadToolKind::GetWorkflowStatus => {
                "Inspect one GitHub Actions run and its first bounded page of job status details."
            }
            GitHubReadToolKind::ListProjectIssues => {
                "List one bounded GitHub Project V2 snapshot with fields and items."
            }
        };
        let description = bounded_description(&format!(
            "Toolkit: {toolkit_name}\nRepository: {repository}\n{action}"
        ));
        Self {
            kind,
            client,
            description,
        }
    }
}

#[async_trait]
impl Tool for GitHubReadTool {
    fn name(&self) -> &str {
        match self.kind {
            GitHubReadToolKind::GetMe => GET_ME,
            GitHubReadToolKind::ListBranches => LIST_BRANCHES,
            GitHubReadToolKind::ReadFile => READ_FILE,
            GitHubReadToolKind::ReadMultipleFiles => READ_MULTIPLE_FILES,
            GitHubReadToolKind::GrepFile => GREP_FILE,
            GitHubReadToolKind::ListMainFiles => LIST_MAIN_FILES,
            GitHubReadToolKind::ListActiveFiles => LIST_ACTIVE_FILES,
            GitHubReadToolKind::ListDirectoryFiles => LIST_DIRECTORY_FILES,
            GitHubReadToolKind::ListIssues => LIST_ISSUES,
            GitHubReadToolKind::GetIssue => GET_ISSUE,
            GitHubReadToolKind::SearchIssues => SEARCH_ISSUES,
            GitHubReadToolKind::ListPullRequests => LIST_PULL_REQUESTS,
            GitHubReadToolKind::GetPullRequest => GET_PULL_REQUEST,
            GitHubReadToolKind::ListPullRequestDiffs => LIST_PULL_REQUEST_DIFFS,
            GitHubReadToolKind::ListCommits => LIST_COMMITS,
            GitHubReadToolKind::GetCommitChanges => GET_COMMIT_CHANGES,
            GitHubReadToolKind::CompareCommits => COMPARE_COMMITS,
            GitHubReadToolKind::SearchCode => SEARCH_CODE,
            GitHubReadToolKind::GetWorkflowStatus => GET_WORKFLOW_STATUS,
            GitHubReadToolKind::ListProjectIssues => LIST_PROJECT_ISSUES,
        }
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        true
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(match self.kind {
            GitHubReadToolKind::GetMe | GitHubReadToolKind::ListIssues => get_me_schema(),
            GitHubReadToolKind::ListBranches => list_branches_schema(),
            GitHubReadToolKind::ReadFile => read_file_schema(),
            GitHubReadToolKind::ReadMultipleFiles => read_multiple_files_schema(),
            GitHubReadToolKind::GrepFile => grep_file_schema(),
            GitHubReadToolKind::ListMainFiles | GitHubReadToolKind::ListActiveFiles => {
                get_me_schema()
            }
            GitHubReadToolKind::ListDirectoryFiles => list_directory_files_schema(),
            GitHubReadToolKind::GetIssue => get_issue_schema(),
            GitHubReadToolKind::SearchIssues => search_issues_schema(),
            GitHubReadToolKind::ListPullRequests => list_pull_requests_schema(),
            GitHubReadToolKind::GetPullRequest | GitHubReadToolKind::ListPullRequestDiffs => {
                get_pull_request_schema()
            }
            GitHubReadToolKind::ListCommits => list_commits_schema(),
            GitHubReadToolKind::GetCommitChanges => get_commit_changes_schema(),
            GitHubReadToolKind::CompareCommits => compare_commits_schema(),
            GitHubReadToolKind::SearchCode => search_code_schema(),
            GitHubReadToolKind::GetWorkflowStatus => get_workflow_status_schema(),
            GitHubReadToolKind::ListProjectIssues => list_project_issues_schema(),
        })
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        match self.kind {
            GitHubReadToolKind::GetMe => {
                if !arguments.is_empty() {
                    return Err(invalid_arguments());
                }
                self.client
                    .get_authenticated_user()
                    .await
                    .map_err(GitHubClientError::into_adk)
            }
            GitHubReadToolKind::ListBranches => {
                if arguments.keys().any(|key| key != "max_count") {
                    return Err(invalid_arguments());
                }
                let max_count = match arguments.get("max_count") {
                    None => 100,
                    Some(value) => value
                        .as_u64()
                        .and_then(|value| usize::try_from(value).ok())
                        .filter(|value| (1..=100).contains(value))
                        .ok_or_else(invalid_arguments)?,
                };
                self.client
                    .list_branches(max_count)
                    .await
                    .map_err(GitHubClientError::into_adk)
            }
            GitHubReadToolKind::ReadFile => self.execute_read_file(arguments).await,
            GitHubReadToolKind::ReadMultipleFiles => {
                self.execute_read_multiple_files(arguments).await
            }
            GitHubReadToolKind::GrepFile => self.execute_grep_file(arguments).await,
            GitHubReadToolKind::ListMainFiles => {
                self.execute_list_files(arguments, GitHubFileScope::BaseBranch, None)
                    .await
            }
            GitHubReadToolKind::ListActiveFiles => {
                self.execute_list_files(arguments, GitHubFileScope::ActiveBranch, None)
                    .await
            }
            GitHubReadToolKind::ListDirectoryFiles => {
                reject_unknown_keys(arguments, &["directory_path"])?;
                let directory = required_directory(arguments, "directory_path")?;
                self.execute_list_files(arguments, GitHubFileScope::ActiveBranch, Some(directory))
                    .await
            }
            GitHubReadToolKind::ListIssues => {
                if !arguments.is_empty() {
                    return Err(invalid_arguments());
                }
                self.client
                    .list_open_issues()
                    .await
                    .map_err(GitHubClientError::into_adk)
            }
            GitHubReadToolKind::GetIssue => self.execute_get_issue(arguments).await,
            GitHubReadToolKind::SearchIssues => self.execute_search_issues(arguments).await,
            GitHubReadToolKind::ListPullRequests => {
                self.execute_list_pull_requests(arguments).await
            }
            GitHubReadToolKind::GetPullRequest => self.execute_get_pull_request(arguments).await,
            GitHubReadToolKind::ListPullRequestDiffs => {
                self.execute_list_pull_request_diffs(arguments).await
            }
            GitHubReadToolKind::ListCommits => self.execute_list_commits(arguments).await,
            GitHubReadToolKind::GetCommitChanges => {
                self.execute_get_commit_changes(arguments).await
            }
            GitHubReadToolKind::CompareCommits => self.execute_compare_commits(arguments).await,
            GitHubReadToolKind::SearchCode => self.execute_search_code(arguments).await,
            GitHubReadToolKind::GetWorkflowStatus => {
                self.execute_get_workflow_status(arguments).await
            }
            GitHubReadToolKind::ListProjectIssues => {
                self.execute_list_project_issues(arguments).await
            }
        }
    }
}

fn get_me_schema() -> Value {
    json!({
        "type": "object",
        "properties": {},
        "additionalProperties": false
    })
}

fn list_branches_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "max_count": {
                "type": "integer",
                "minimum": 1,
                "maximum": 100,
                "default": 100,
                "description": "Maximum number of branches to return."
            }
        },
        "additionalProperties": false
    })
}

fn read_file_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "file_path": {
                "type": "string",
                "minLength": 1,
                "maxLength": 4096,
                "description": "Path to the file to read."
            },
            "branch": {
                "type": ["string", "null"],
                "minLength": 1,
                "maxLength": 1024,
                "description": "Branch name. Defaults to the configured active branch."
            },
            "repo_name": {
                "type": ["string", "null"],
                "minLength": 3,
                "maxLength": 512,
                "description": "Optional repository in owner/name form."
            },
            "start_line": {
                "type": ["integer", "null"],
                "minimum": 1,
                "description": "First line to read, 1-indexed and inclusive."
            },
            "end_line": {
                "type": ["integer", "null"],
                "minimum": 1,
                "description": "Last line to read, 1-indexed and inclusive."
            }
        },
        "required": ["file_path"],
        "additionalProperties": false
    })
}

fn read_multiple_files_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "file_paths": {
                "type": "array",
                "minItems": 1,
                "maxItems": MAX_BATCH_FILES,
                "items": {"type": "string", "minLength": 1, "maxLength": 4096},
                "description": "File paths to read in order."
            },
            "branch": {
                "type": ["string", "null"],
                "minLength": 1,
                "maxLength": 1024,
                "description": "Branch name. Defaults to the configured active branch."
            },
            "offset": {
                "type": ["integer", "null"],
                "minimum": 1,
                "description": "First line to read from every file, 1-indexed."
            },
            "limit": {
                "type": ["integer", "null"],
                "minimum": 1,
                "description": "Maximum number of lines to read from every file."
            }
        },
        "required": ["file_paths"],
        "additionalProperties": false
    })
}

fn grep_file_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "file_path": {
                "type": "string",
                "minLength": 1,
                "maxLength": 4096,
                "description": "Path to the file whose contents are searched."
            },
            "pattern": {
                "type": "string",
                "minLength": 1,
                "maxLength": MAX_PATTERN_BYTES,
                "description": "Literal text or regular expression to find."
            },
            "branch": {
                "type": ["string", "null"],
                "minLength": 1,
                "maxLength": 1024,
                "description": "Branch name. Defaults to the configured active branch."
            },
            "is_regex": {
                "type": "boolean",
                "default": true,
                "description": "Treat pattern as a regular expression."
            },
            "context_lines": {
                "type": "integer",
                "minimum": 0,
                "maximum": MAX_CONTEXT_LINES,
                "default": 2,
                "description": "Lines of context before and after each match."
            }
        },
        "required": ["file_path", "pattern"],
        "additionalProperties": false
    })
}

fn list_directory_files_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "directory_path": {
                "type": "string",
                "maxLength": 4096,
                "description": "Directory path to list recursively, for example src/my_dir."
            }
        },
        "required": ["directory_path"],
        "additionalProperties": false
    })
}

fn get_issue_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "issue_number": {
                "type": "integer",
                "minimum": 1,
                "description": "Issue or pull-request number."
            },
            "repo_name": {
                "type": ["string", "null"],
                "minLength": 3,
                "maxLength": 512,
                "description": "Optional repository in owner/name form."
            }
        },
        "required": ["issue_number"],
        "additionalProperties": false
    })
}

fn search_issues_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "search_query": {
                "type": "string",
                "minLength": 1,
                "maxLength": 4096,
                "description": "GitHub issue-search query, for example is:open label:bug."
            },
            "repo_name": {
                "type": ["string", "null"],
                "minLength": 3,
                "maxLength": 512,
                "description": "Optional repository in owner/name form."
            },
            "max_count": {
                "type": ["integer", "null"],
                "minimum": 1,
                "maximum": 100,
                "default": 30,
                "description": "Maximum number of matches to return."
            }
        },
        "required": ["search_query"],
        "additionalProperties": false
    })
}

fn list_pull_requests_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "max_count": {
                "type": "integer",
                "minimum": 1,
                "maximum": 100,
                "default": 100,
                "description": "Maximum number of open pull requests to return."
            }
        },
        "additionalProperties": false
    })
}

fn get_pull_request_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "pr_number": {
                "type": "integer",
                "minimum": 1,
                "description": "Pull-request number."
            },
            "repo_name": {
                "type": ["string", "null"],
                "minLength": 3,
                "maxLength": 512,
                "description": "Optional repository in owner/name form."
            }
        },
        "required": ["pr_number"],
        "additionalProperties": false
    })
}

fn list_commits_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "repo_name": optional_string_schema(512, "Optional repository in owner/name form."),
            "sha": optional_string_schema(1024, "Commit SHA, branch, or tag to list from."),
            "path": optional_string_schema(4096, "File or directory path filter."),
            "since": optional_string_schema(64, "Inclusive lower ISO-8601 date or timestamp."),
            "until": optional_string_schema(64, "Inclusive upper ISO-8601 date or timestamp."),
            "author": optional_string_schema(1024, "GitHub username or email author filter."),
            "max_count": {
                "type": ["integer", "null"],
                "minimum": 1,
                "maximum": 100,
                "default": 30,
                "description": "Maximum number of commits to return."
            }
        },
        "additionalProperties": false
    })
}

fn get_commit_changes_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "sha": {
                "type": "string",
                "minLength": 1,
                "maxLength": 1024,
                "description": "Commit SHA, branch, or tag to inspect."
            },
            "repo_name": optional_string_schema(512, "Optional repository in owner/name form.")
        },
        "required": ["sha"],
        "additionalProperties": false
    })
}

fn compare_commits_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "base_sha": {
                "type": "string",
                "minLength": 1,
                "maxLength": 1024,
                "description": "Base commit SHA, branch, or tag."
            },
            "head_sha": {
                "type": "string",
                "minLength": 1,
                "maxLength": 1024,
                "description": "Head commit SHA, branch, or tag."
            },
            "repo_name": optional_string_schema(512, "Optional repository in owner/name form.")
        },
        "required": ["base_sha", "head_sha"],
        "additionalProperties": false
    })
}

fn search_code_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "query": {
                "type": "string",
                "minLength": 1,
                "maxLength": 4096,
                "description": "GitHub code-search query using qualifiers such as `language:rust`, `path:src`, `filename:Cargo.toml`, `extension:rs`, or an explicit `repo:owner/name`, `org:name`, or `user:name` scope. The configured repository scope is added only when no explicit scope is present."
            },
            "sort": {
                "type": ["string", "null"],
                "enum": ["indexed", null],
                "default": null,
                "description": "Optional GitHub code-search sort field."
            },
            "order": {
                "type": ["string", "null"],
                "enum": ["asc", "desc", null],
                "default": null,
                "description": "Optional result order. Omit it for GitHub's default descending order."
            },
            "per_page": {
                "type": ["integer", "null"],
                "minimum": 1,
                "maximum": 100,
                "default": 30,
                "description": "Results requested from one provider page. Together with page, the requested window must end at or before GitHub's first 1,000 accessible results: (page - 1) * per_page + per_page <= 1000."
            },
            "page": {
                "type": ["integer", "null"],
                "minimum": 1,
                "maximum": 1000,
                "default": 1,
                "description": "One-indexed provider page. Together with per_page, the requested window must end at or before GitHub's first 1,000 accessible results: (page - 1) * per_page + per_page <= 1000."
            }
        },
        "required": ["query"],
        "additionalProperties": false
    })
}

fn get_workflow_status_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "run_id": {
                "type": "string",
                "minLength": 1,
                "maxLength": 19,
                "pattern": "^[0-9]+$",
                "description": "Decimal GitHub Actions workflow-run ID."
            },
            "repo_name": optional_string_schema(512, "Optional repository in owner/name form.")
        },
        "required": ["run_id"],
        "additionalProperties": false
    })
}

fn list_project_issues_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "board_repo": {
                "type": "string",
                "minLength": 3,
                "maxLength": 512,
                "description": "Organization and repository that owns the project, in owner/name form."
            },
            "project_number": {
                "type": "integer",
                "minimum": 1,
                "maximum": i32::MAX,
                "description": "Project V2 number shown in the GitHub project URL."
            },
            "items_count": {
                "type": ["integer", "null"],
                "minimum": 1,
                "maximum": 100,
                "default": 100,
                "description": "Maximum project items returned by one bounded GraphQL request."
            }
        },
        "required": ["board_repo", "project_number"],
        "additionalProperties": false
    })
}

fn optional_string_schema(max_length: usize, description: &str) -> Value {
    json!({
        "type": ["string", "null"],
        "minLength": 1,
        "maxLength": max_length,
        "description": description
    })
}

impl GitHubReadTool {
    async fn execute_read_file(&self, arguments: &Map<String, Value>) -> adk_core::Result<Value> {
        reject_unknown_keys(
            arguments,
            &["file_path", "branch", "repo_name", "start_line", "end_line"],
        )?;
        let file_path = required_text(arguments, "file_path", 4 * 1_024)?;
        let branch = optional_text(arguments, "branch", 1_024)?;
        let repository = optional_text(arguments, "repo_name", 512)?;
        let start_line = optional_positive_usize(arguments, "start_line")?;
        let end_line = optional_positive_usize(arguments, "end_line")?;
        if start_line
            .zip(end_line)
            .is_some_and(|(start, end)| end < start)
        {
            return Err(invalid_arguments());
        }
        let full_content = self
            .client
            .read_text_file(file_path, branch, repository)
            .await
            .map_err(GitHubClientError::into_adk)?;
        let content = slice_lines(&full_content, start_line, end_line);
        let requested = if start_line.is_some() || end_line.is_some() {
            format!(
                "start_line={}, end_line={}",
                optional_number_label(start_line),
                optional_number_label(end_line)
            )
        } else {
            "full file read".to_owned()
        };
        Ok(guard_text_read(
            content,
            file_path,
            &requested,
            &full_content,
        ))
    }

    async fn execute_read_multiple_files(
        &self,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        reject_unknown_keys(arguments, &["file_paths", "branch", "offset", "limit"])?;
        let file_paths = arguments
            .get("file_paths")
            .and_then(Value::as_array)
            .filter(|paths| !paths.is_empty() && paths.len() <= MAX_BATCH_FILES)
            .ok_or_else(invalid_arguments)?;
        let mut validated_paths = Vec::with_capacity(file_paths.len());
        for path in file_paths {
            validated_paths.push(validate_text_value(path, 4 * 1_024)?);
        }
        let branch = optional_text(arguments, "branch", 1_024)?;
        let offset = optional_positive_usize(arguments, "offset")?;
        let limit = optional_positive_usize(arguments, "limit")?;
        let end_line = match (offset, limit) {
            (Some(start), Some(count)) => Some(
                start
                    .checked_add(count.saturating_sub(1))
                    .ok_or_else(invalid_arguments)?,
            ),
            _ => None,
        };
        let mut results = Map::new();
        let mut cumulative_chars = 0_usize;
        for file_path in validated_paths {
            if cumulative_chars >= MAX_OUTPUT_CHARS {
                results.insert(file_path.to_owned(), Value::String(batch_skip_notice()));
                continue;
            }
            let result = self
                .client
                .read_text_file(file_path, branch, None)
                .await
                .map(|full_content| {
                    let content = slice_lines(&full_content, offset, end_line);
                    let requested = if offset.is_some() || end_line.is_some() {
                        format!(
                            "start_line={}, end_line={}",
                            optional_number_label(offset),
                            optional_number_label(end_line)
                        )
                    } else {
                        "full file read".to_owned()
                    };
                    guard_text_read(content, file_path, &requested, &full_content)
                });
            let (value, measured) = match result {
                Ok(value) => (value, true),
                Err(_) => (
                    Value::String(
                        "Error reading file: the GitHub file could not be read".to_owned(),
                    ),
                    false,
                ),
            };
            if measured {
                cumulative_chars = cumulative_chars.saturating_add(measure_result_chars(&value));
            }
            results.insert(file_path.to_owned(), value);
        }
        Ok(Value::Object(results))
    }

    async fn execute_grep_file(&self, arguments: &Map<String, Value>) -> adk_core::Result<Value> {
        reject_unknown_keys(
            arguments,
            &[
                "file_path",
                "pattern",
                "branch",
                "is_regex",
                "context_lines",
            ],
        )?;
        let file_path = required_text(arguments, "file_path", 4 * 1_024)?;
        let pattern = required_text(arguments, "pattern", MAX_PATTERN_BYTES)?;
        let branch = optional_text(arguments, "branch", 1_024)?;
        let is_regex = optional_bool(arguments, "is_regex")?.unwrap_or(true);
        let context_lines = optional_nonnegative_usize(arguments, "context_lines")?.unwrap_or(2);
        if context_lines > MAX_CONTEXT_LINES {
            return Err(invalid_arguments());
        }
        let expression = compile_pattern(pattern, is_regex)?;
        let content = self
            .client
            .read_text_file(file_path, branch, None)
            .await
            .map_err(GitHubClientError::into_adk)?;
        Ok(Value::String(search_file_content(
            &content,
            file_path,
            pattern,
            &expression,
            context_lines,
        )?))
    }

    async fn execute_list_files(
        &self,
        arguments: &Map<String, Value>,
        scope: GitHubFileScope,
        directory: Option<&str>,
    ) -> adk_core::Result<Value> {
        if directory.is_none() && !arguments.is_empty() {
            return Err(invalid_arguments());
        }
        self.client
            .list_repository_files(scope, directory)
            .await
            .map_err(GitHubClientError::into_adk)
    }

    async fn execute_get_issue(&self, arguments: &Map<String, Value>) -> adk_core::Result<Value> {
        reject_unknown_keys(arguments, &["issue_number", "repo_name"])?;
        let issue_number = required_positive_u64(arguments, "issue_number")?;
        let repository = optional_text(arguments, "repo_name", 512)?;
        self.client
            .get_issue(issue_number, repository)
            .await
            .map_err(GitHubClientError::into_adk)
    }

    async fn execute_search_issues(
        &self,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        reject_unknown_keys(arguments, &["search_query", "repo_name", "max_count"])?;
        let search_query = required_text(arguments, "search_query", 4 * 1_024)?;
        if !valid_issue_search_query(search_query) {
            return Err(invalid_arguments());
        }
        let repository = optional_text(arguments, "repo_name", 512)?;
        let max_count = optional_positive_usize(arguments, "max_count")?.unwrap_or(30);
        if max_count > 100 {
            return Err(invalid_arguments());
        }
        self.client
            .search_issues(search_query, repository, max_count)
            .await
            .map_err(GitHubClientError::into_adk)
    }

    async fn execute_list_pull_requests(
        &self,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        reject_unknown_keys(arguments, &["max_count"])?;
        if arguments.get("max_count").is_some_and(Value::is_null) {
            return Err(invalid_arguments());
        }
        let max_count = optional_positive_usize(arguments, "max_count")?.unwrap_or(100);
        if max_count > 100 {
            return Err(invalid_arguments());
        }
        self.client
            .list_open_pull_requests(max_count)
            .await
            .map_err(GitHubClientError::into_adk)
    }

    async fn execute_get_pull_request(
        &self,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        let (number, repository) = pull_request_arguments(arguments)?;
        self.client
            .get_pull_request(number, repository)
            .await
            .map_err(GitHubClientError::into_adk)
    }

    async fn execute_list_pull_request_diffs(
        &self,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        let (number, repository) = pull_request_arguments(arguments)?;
        self.client
            .list_pull_request_files(number, repository)
            .await
            .map_err(GitHubClientError::into_adk)
    }

    async fn execute_list_commits(
        &self,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        reject_unknown_keys(
            arguments,
            &[
                "repo_name",
                "sha",
                "path",
                "since",
                "until",
                "author",
                "max_count",
            ],
        )?;
        let repository = optional_text(arguments, "repo_name", 512)?.map(ToOwned::to_owned);
        let reference = optional_text(arguments, "sha", 1_024)?.map(ToOwned::to_owned);
        let path = optional_text(arguments, "path", 4 * 1_024)?.map(ToOwned::to_owned);
        let author = optional_text(arguments, "author", 1_024)?.map(ToOwned::to_owned);
        let since = optional_commit_boundary(arguments, "since")?;
        let until = optional_commit_boundary(arguments, "until")?;
        if since
            .as_ref()
            .zip(until.as_ref())
            .is_some_and(|(since, until)| since.0 > until.0)
        {
            return Err(invalid_arguments());
        }
        let max_count = optional_positive_usize(arguments, "max_count")?.unwrap_or(30);
        if max_count > 100 {
            return Err(invalid_arguments());
        }
        self.client
            .list_commits(GitHubCommitQuery {
                repository,
                reference,
                path,
                since: since.map(|boundary| boundary.1),
                until: until.map(|boundary| boundary.1),
                author,
                max_count,
            })
            .await
            .map_err(GitHubClientError::into_adk)
    }

    async fn execute_get_commit_changes(
        &self,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        reject_unknown_keys(arguments, &["sha", "repo_name"])?;
        let reference = required_text(arguments, "sha", 1_024)?;
        let repository = optional_text(arguments, "repo_name", 512)?;
        self.client
            .get_commit_changes(reference, repository)
            .await
            .map_err(GitHubClientError::into_adk)
    }

    async fn execute_compare_commits(
        &self,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        reject_unknown_keys(arguments, &["base_sha", "head_sha", "repo_name"])?;
        let base = required_text(arguments, "base_sha", 1_024)?;
        let head = required_text(arguments, "head_sha", 1_024)?;
        let repository = optional_text(arguments, "repo_name", 512)?;
        self.client
            .compare_commits(base, head, repository)
            .await
            .map_err(GitHubClientError::into_adk)
    }

    async fn execute_search_code(&self, arguments: &Map<String, Value>) -> adk_core::Result<Value> {
        reject_unknown_keys(arguments, &["query", "sort", "order", "per_page", "page"])?;
        let query = required_text(arguments, "query", 4 * 1_024)?;
        if query.trim().is_empty() {
            return Err(invalid_arguments());
        }
        let query = query.to_owned();
        let sort = optional_text(arguments, "sort", 32)?.map(ToOwned::to_owned);
        if sort.as_deref().is_some_and(|sort| sort != "indexed") {
            return Err(invalid_arguments());
        }
        let order = optional_text(arguments, "order", 32)?.map(ToOwned::to_owned);
        if order
            .as_deref()
            .is_some_and(|order| !matches!(order, "asc" | "desc"))
        {
            return Err(invalid_arguments());
        }
        let per_page = optional_positive_usize(arguments, "per_page")?.unwrap_or(30);
        let page = optional_positive_usize(arguments, "page")?.unwrap_or(1);
        let within_window = page
            .checked_sub(1)
            .and_then(|page| page.checked_mul(per_page))
            .and_then(|offset| offset.checked_add(per_page))
            .is_some_and(|end| per_page <= 100 && end <= 1_000);
        if !within_window {
            return Err(invalid_arguments());
        }
        self.client
            .search_code(GitHubCodeSearchQuery {
                query,
                sort,
                order,
                per_page,
                page,
            })
            .await
            .map_err(GitHubClientError::into_adk)
    }

    async fn execute_get_workflow_status(
        &self,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        reject_unknown_keys(arguments, &["run_id", "repo_name"])?;
        let run_id = required_text(arguments, "run_id", 19)?;
        if !run_id.bytes().all(|byte| byte.is_ascii_digit()) {
            return Err(invalid_arguments());
        }
        let run_id = run_id
            .parse::<u64>()
            .ok()
            .filter(|value| *value > 0 && i64::try_from(*value).is_ok())
            .ok_or_else(invalid_arguments)?;
        let repository = optional_text(arguments, "repo_name", 512)?;
        self.client
            .get_workflow_status(run_id, repository)
            .await
            .map_err(GitHubClientError::into_adk)
    }

    async fn execute_list_project_issues(
        &self,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        reject_unknown_keys(arguments, &["board_repo", "project_number", "items_count"])?;
        let board_repository = required_text(arguments, "board_repo", 512)?;
        validate_repository(board_repository).map_err(GitHubClientError::into_adk)?;
        let project_number = required_positive_u64(arguments, "project_number")?;
        let project_number = u32::try_from(project_number)
            .ok()
            .filter(|number| i32::try_from(*number).is_ok())
            .ok_or_else(invalid_arguments)?;
        let items_count = optional_positive_usize(arguments, "items_count")?.unwrap_or(100);
        if items_count > 100 {
            return Err(invalid_arguments());
        }
        self.client
            .list_project_issues(board_repository, project_number, items_count)
            .await
            .map_err(GitHubClientError::into_adk)
    }
}

fn reject_unknown_keys(arguments: &Map<String, Value>, allowed: &[&str]) -> adk_core::Result<()> {
    if arguments.keys().any(|key| !allowed.contains(&key.as_str())) {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn required_text<'a>(
    arguments: &'a Map<String, Value>,
    key: &str,
    max_bytes: usize,
) -> adk_core::Result<&'a str> {
    let value = arguments.get(key).ok_or_else(invalid_arguments)?;
    validate_text_value(value, max_bytes)
}

fn optional_text<'a>(
    arguments: &'a Map<String, Value>,
    key: &str,
    max_bytes: usize,
) -> adk_core::Result<Option<&'a str>> {
    match arguments.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(value) => validate_text_value(value, max_bytes).map(Some),
    }
}

fn optional_commit_boundary(
    arguments: &Map<String, Value>,
    key: &str,
) -> adk_core::Result<Option<(DateTime<Utc>, String)>> {
    let Some(raw) = optional_text(arguments, key, 64)? else {
        return Ok(None);
    };
    let timestamp = if let Ok(date) = NaiveDate::parse_from_str(raw, "%Y-%m-%d") {
        let midnight = date.and_hms_opt(0, 0, 0).ok_or_else(invalid_arguments)?;
        DateTime::<Utc>::from_naive_utc_and_offset(midnight, Utc)
    } else {
        DateTime::parse_from_rfc3339(raw)
            .map_err(|_| invalid_arguments())?
            .with_timezone(&Utc)
    };
    let canonical = timestamp.to_rfc3339_opts(SecondsFormat::Secs, true);
    Ok(Some((timestamp, canonical)))
}

fn validate_text_value(value: &Value, max_bytes: usize) -> adk_core::Result<&str> {
    value
        .as_str()
        .filter(|value| {
            !value.is_empty()
                && value.len() <= max_bytes
                && !value.chars().any(|character| character.is_ascii_control())
        })
        .ok_or_else(invalid_arguments)
}

fn required_directory<'a>(
    arguments: &'a Map<String, Value>,
    key: &str,
) -> adk_core::Result<&'a str> {
    arguments
        .get(key)
        .and_then(Value::as_str)
        .filter(|value| {
            value.len() <= 4 * 1_024 && !value.chars().any(|character| character.is_ascii_control())
        })
        .ok_or_else(invalid_arguments)
}

fn optional_positive_usize(
    arguments: &Map<String, Value>,
    key: &str,
) -> adk_core::Result<Option<usize>> {
    match arguments.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(value) => value
            .as_u64()
            .and_then(|value| usize::try_from(value).ok())
            .filter(|value| *value > 0)
            .map(Some)
            .ok_or_else(invalid_arguments),
    }
}

fn required_positive_u64(arguments: &Map<String, Value>, key: &str) -> adk_core::Result<u64> {
    arguments
        .get(key)
        .and_then(Value::as_u64)
        .filter(|value| *value > 0 && i64::try_from(*value).is_ok())
        .ok_or_else(invalid_arguments)
}

fn pull_request_arguments(arguments: &Map<String, Value>) -> adk_core::Result<(u64, Option<&str>)> {
    reject_unknown_keys(arguments, &["pr_number", "repo_name"])?;
    let number = required_positive_u64(arguments, "pr_number")?;
    let repository = optional_text(arguments, "repo_name", 512)?;
    Ok((number, repository))
}

fn valid_issue_search_query(query: &str) -> bool {
    const DANGEROUS: &[&str] = &[
        "<script",
        "javascript:",
        "onerror=",
        "onclick=",
        "data:text/html",
        "alert(",
        "eval(",
    ];
    let normalized = query.to_ascii_lowercase();
    !query.trim().is_empty() && !DANGEROUS.iter().any(|pattern| normalized.contains(pattern))
}

fn optional_nonnegative_usize(
    arguments: &Map<String, Value>,
    key: &str,
) -> adk_core::Result<Option<usize>> {
    match arguments.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(value) => value
            .as_u64()
            .and_then(|value| usize::try_from(value).ok())
            .map(Some)
            .ok_or_else(invalid_arguments),
    }
}

fn optional_bool(arguments: &Map<String, Value>, key: &str) -> adk_core::Result<Option<bool>> {
    match arguments.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(value) => value.as_bool().map(Some).ok_or_else(invalid_arguments),
    }
}

fn optional_number_label(value: Option<usize>) -> String {
    value.map_or_else(|| "None".to_owned(), |value| value.to_string())
}

fn python_line_ranges(content: &str) -> Vec<(usize, usize)> {
    let mut ranges = Vec::new();
    let mut start = 0_usize;
    let mut characters = content.char_indices().peekable();
    while let Some((index, character)) = characters.next() {
        let end = if character == '\r' {
            if let Some((next_index, '\n')) = characters.peek().copied() {
                let _ = characters.next();
                Some(next_index.saturating_add('\n'.len_utf8()))
            } else {
                Some(index.saturating_add(character.len_utf8()))
            }
        } else if is_python_line_break(character) {
            Some(index.saturating_add(character.len_utf8()))
        } else {
            None
        };
        if let Some(end) = end {
            ranges.push((start, end));
            start = end;
        }
    }
    if start < content.len() {
        ranges.push((start, content.len()));
    }
    ranges
}

fn is_python_line_break(character: char) -> bool {
    matches!(
        character,
        '\n' | '\r'
            | '\u{000B}'
            | '\u{000C}'
            | '\u{001C}'
            | '\u{001D}'
            | '\u{001E}'
            | '\u{0085}'
            | '\u{2028}'
            | '\u{2029}'
    )
}

fn slice_lines(content: &str, start_line: Option<usize>, end_line: Option<usize>) -> &str {
    if start_line.is_none() && end_line.is_none() {
        return content;
    }
    let ranges = python_line_ranges(content);
    let first = start_line.unwrap_or(1).saturating_sub(1);
    if first >= ranges.len() {
        return "";
    }
    let last_exclusive = end_line.unwrap_or(ranges.len()).min(ranges.len());
    if first >= last_exclusive {
        return "";
    }
    &content[ranges[first].0..ranges[last_exclusive - 1].1]
}

fn guard_text_read(content: &str, file_path: &str, requested: &str, full_content: &str) -> Value {
    let actual_chars = content.chars().count();
    if actual_chars <= MAX_OUTPUT_CHARS {
        return Value::String(content.to_owned());
    }
    let total_lines = python_line_ranges(full_content).len();
    let extension = file_extension(file_path);
    let file_type = mime_type(extension);
    let (first_class_params, notes, full_read_allowed) = if total_lines <= 1 {
        (
            Map::new(),
            format!(
                "This file has no usable line breaks ({actual_chars} characters on a single line) and exceeds the {MAX_OUTPUT_CHARS}-character read limit. Line slicing would return the whole file, so a bounded read is not possible — the full read is refused."
            ),
            false,
        )
    } else {
        let mut params = Map::new();
        params.insert(
            "start_line".to_owned(),
            Value::String(format!(
                "integer (1-indexed, inclusive) — first line to read. Valid range 1..{total_lines}. Omit to read from the beginning."
            )),
        );
        params.insert(
            "end_line".to_owned(),
            Value::String(format!(
                "integer (1-indexed, inclusive) — last line to read. Valid range 1..{total_lines}. Omit to read to the end."
            )),
        );
        (
            params,
            "Use start_line/end_line together to read a bounded slice of a large file and keep tokens bounded."
                .to_owned(),
            full_content.chars().count() <= MAX_OUTPUT_CHARS,
        )
    };
    json!({
        "__result_status__": "content_too_large",
        "context": {
            "actual_chars": actual_chars,
            "limit_chars": MAX_OUTPUT_CHARS,
            "requested": requested
        },
        "extension": extension,
        "filename": file_path,
        "instruction_for_readFile": {
            "extra_params": {},
            "first_class_params": first_class_params,
            "notes": notes
        },
        "read_limits": {
            "full_read_allowed": full_read_allowed,
            "max_output_chars": MAX_OUTPUT_CHARS
        },
        "schema_version": "1.0",
        "total_lines": total_lines,
        "type": file_type,
        "unit": "lines"
    })
}

fn file_extension(file_path: &str) -> &str {
    let filename = file_path.rsplit('/').next().unwrap_or(file_path);
    filename
        .rfind('.')
        .filter(|index| *index > 0)
        .map_or("", |index| &filename[index..])
}

fn mime_type(extension: &str) -> &'static str {
    match extension.to_ascii_lowercase().as_str() {
        ".py" => "text/x-python",
        ".rs" => "text/x-rust",
        ".js" => "text/javascript",
        ".ts" => "text/typescript",
        ".json" => "application/json",
        ".md" => "text/markdown",
        ".yaml" | ".yml" => "application/yaml",
        ".html" => "text/html",
        ".css" => "text/css",
        ".csv" => "text/csv",
        ".txt" | ".log" => "text/plain",
        _ => "application/octet-stream",
    }
}

fn measure_result_chars(value: &Value) -> usize {
    match value {
        Value::String(value) => value.chars().count(),
        _ => serde_json::to_string(value).map_or(MAX_OUTPUT_CHARS, |value| value.chars().count()),
    }
}

fn batch_skip_notice() -> String {
    format!(
        "Skipped: the batch's cumulative {MAX_OUTPUT_CHARS}-character read limit was already reached by earlier files in this call. Read this file individually with read_file."
    )
}

fn compile_pattern(pattern: &str, is_regex: bool) -> adk_core::Result<Regex> {
    let source = if is_regex {
        pattern.to_owned()
    } else {
        regex::escape(pattern)
    };
    RegexBuilder::new(&source)
        .case_insensitive(true)
        .size_limit(REGEX_SIZE_LIMIT)
        .dfa_size_limit(REGEX_DFA_SIZE_LIMIT)
        .build()
        .map_err(|_| invalid_arguments())
}

fn search_file_content(
    content: &str,
    file_path: &str,
    pattern: &str,
    expression: &Regex,
    context_lines: usize,
) -> adk_core::Result<String> {
    let ranges = python_line_ranges(content);
    let lines = ranges
        .iter()
        .map(|(start, end)| content[*start..*end].trim_end_matches(is_python_line_break))
        .collect::<Vec<_>>();
    let mut matches = Vec::new();
    for (index, line) in lines.iter().enumerate() {
        if expression.is_match(line) {
            if matches.len() >= MAX_GREP_MATCHES {
                return Err(output_resource_exhausted());
            }
            matches.push(index);
        }
    }
    if matches.is_empty() {
        return Ok(format!(
            "No matches found for pattern '{pattern}' in {file_path}"
        ));
    }
    let mut output = format!(
        "Found {} match(es) for pattern '{pattern}' in {file_path}:\n",
        matches.len()
    );
    let mut output_chars = output.chars().count();
    if output_chars > MAX_OUTPUT_CHARS {
        return Err(output_resource_exhausted());
    }
    for (match_number, line_index) in matches.into_iter().enumerate() {
        push_bounded(
            &mut output,
            &mut output_chars,
            &format!(
                "\n\n--- Match {} at line {} ---",
                match_number + 1,
                line_index + 1
            ),
        )?;
        let context_start = line_index.saturating_sub(context_lines);
        for line in &lines[context_start..line_index] {
            push_prefixed_line(&mut output, &mut output_chars, "\n  ", line)?;
        }
        push_prefixed_line(&mut output, &mut output_chars, "\n> ", lines[line_index])?;
        let context_end = line_index
            .saturating_add(context_lines)
            .saturating_add(1)
            .min(lines.len());
        for line in &lines[line_index.saturating_add(1)..context_end] {
            push_prefixed_line(&mut output, &mut output_chars, "\n  ", line)?;
        }
    }
    Ok(output)
}

fn push_bounded(
    output: &mut String,
    output_chars: &mut usize,
    value: &str,
) -> adk_core::Result<()> {
    let next = output_chars
        .checked_add(value.chars().count())
        .ok_or_else(output_resource_exhausted)?;
    if next > MAX_OUTPUT_CHARS {
        return Err(output_resource_exhausted());
    }
    output.push_str(value);
    *output_chars = next;
    Ok(())
}

fn push_prefixed_line(
    output: &mut String,
    output_chars: &mut usize,
    prefix: &str,
    line: &str,
) -> adk_core::Result<()> {
    let next = output_chars
        .checked_add(prefix.chars().count())
        .and_then(|value| value.checked_add(line.chars().count()))
        .ok_or_else(output_resource_exhausted)?;
    if next > MAX_OUTPUT_CHARS {
        return Err(output_resource_exhausted());
    }
    output.push_str(prefix);
    output.push_str(line);
    *output_chars = next;
    Ok(())
}

fn output_resource_exhausted() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "github.output.resource_exhausted",
        "the GitHub tool output exceeds the approved limit",
    )
}

fn bounded_description(value: &str) -> Box<str> {
    if value.len() <= MAX_DESCRIPTION_BYTES {
        return value.to_owned().into_boxed_str();
    }
    let mut end = MAX_DESCRIPTION_BYTES;
    while !value.is_char_boundary(end) {
        end -= 1;
    }
    value[..end].to_owned().into_boxed_str()
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "github.arguments.invalid",
        "the GitHub tool arguments are invalid",
    )
}

/// The names of `names` this family serves (the SDK gate selects them).
#[cfg(any(test, feature = "test-support"))]
pub fn test_supported_names(names: &[String]) -> Vec<String> {
    names
        .iter()
        .filter(|name| is_supported_read(name))
        .cloned()
        .collect()
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_build_with_api(
    toolkit_name: &str,
    repository: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn GitHubApi>,
) -> Result<BasicToolset, GitHubToolsetError> {
    build_with_api(toolkit_name, repository, selected, policy, client)
}
