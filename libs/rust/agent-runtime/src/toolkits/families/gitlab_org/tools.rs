use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{GitLabOrgApi, GitLabOrgClient, GitLabOrgClientError, GitLabOrgOperation};
use super::config::{GitLabOrgConfigError, GitLabOrgToolkitConfig};

const MAX_ARGUMENT_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 2_000;
const MAX_BRANCH_BYTES: usize = 255;
const MAX_PATH_BYTES: usize = 1_024;
const MAX_TEXT_BYTES: usize = 256 * 1_024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum GitLabOrgToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

pub(crate) struct GitLabOrgToolsetError {
    code: GitLabOrgToolsetErrorCode,
}

impl GitLabOrgToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> GitLabOrgToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for GitLabOrgToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("GitLabOrgToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for GitLabOrgToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            GitLabOrgToolsetErrorCode::InvalidConfiguration => {
                "the GitLab Org toolkit configuration is invalid"
            }
            GitLabOrgToolsetErrorCode::ResourceExhausted => {
                "the GitLab Org toolkit configuration exceeds its approved limit"
            }
            GitLabOrgToolsetErrorCode::UnsupportedSelection => {
                "the selected GitLab Org tool profile is not supported"
            }
            GitLabOrgToolsetErrorCode::Client => "the GitLab Org client could not be created",
            GitLabOrgToolsetErrorCode::InvalidDefinition => {
                "the GitLab Org ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for GitLabOrgToolsetError {}

impl From<GitLabOrgConfigError> for GitLabOrgToolsetError {
    fn from(source: GitLabOrgConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::GitLabOrgConfigErrorCode::InvalidConfiguration => {
                    GitLabOrgToolsetErrorCode::InvalidConfiguration
                }
                super::config::GitLabOrgConfigErrorCode::ResourceExhausted => {
                    GitLabOrgToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<GitLabOrgClientError> for GitLabOrgToolsetError {
    fn from(_: GitLabOrgClientError) -> Self {
        Self {
            code: GitLabOrgToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for GitLabOrgToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: GitLabOrgToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build all seventeen capability-disabled GitLab Org tools.
///
/// Read/write/delete groups are model-selection cues only. Trusted sensitivity
/// policy is independent, and remote effects remain unavailable to production
/// activation until durable exact-interrupt ownership and receipts exist.
pub(crate) fn build_gitlab_org_toolset(
    toolkit_name: &str,
    config: GitLabOrgToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, GitLabOrgToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let client: Arc<dyn GitLabOrgApi> = Arc::new(GitLabOrgClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), GitLabOrgToolsetError> {
    if selected.iter().any(|name| {
        !GitLabOrgToolKind::ALL
            .iter()
            .any(|kind| kind.name() == name.as_ref())
    }) {
        return Err(GitLabOrgToolsetError {
            code: GitLabOrgToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn GitLabOrgApi>,
) -> Result<BasicToolset, GitLabOrgToolsetError> {
    let include_all = selected.is_empty();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(GitLabOrgToolKind::ALL.len());
    for kind in GitLabOrgToolKind::ALL {
        if include_all || selected.iter().any(|name| name == kind.name()) {
            tools.push(Arc::new(GitLabOrgTool::new(
                kind,
                toolkit_name,
                Arc::clone(client),
            )));
        }
    }
    admit_materialized_toolset(toolkit_name, "gitlab_org", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn GitLabOrgApi>,
) -> Result<BasicToolset, GitLabOrgToolsetError> {
    build_with_api(toolkit_name, selected, policy, client)
}

#[derive(Clone, Copy)]
enum GitLabOrgToolKind {
    CreateBranch,
    SetActiveBranch,
    ListBranches,
    GetIssues,
    GetIssue,
    CreatePullRequest,
    CommentOnIssue,
    CreateFile,
    ReadFile,
    UpdateFile,
    DeleteFile,
    GetPrChanges,
    CreatePrChangeComment,
    ListFiles,
    ListFolders,
    AppendFile,
    GetCommits,
}

impl GitLabOrgToolKind {
    const ALL: [Self; 17] = [
        Self::CreateBranch,
        Self::SetActiveBranch,
        Self::ListBranches,
        Self::GetIssues,
        Self::GetIssue,
        Self::CreatePullRequest,
        Self::CommentOnIssue,
        Self::CreateFile,
        Self::ReadFile,
        Self::UpdateFile,
        Self::DeleteFile,
        Self::GetPrChanges,
        Self::CreatePrChangeComment,
        Self::ListFiles,
        Self::ListFolders,
        Self::AppendFile,
        Self::GetCommits,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::CreateBranch => "create_branch",
            Self::SetActiveBranch => "set_active_branch",
            Self::ListBranches => "list_branches_in_repo",
            Self::GetIssues => "get_issues",
            Self::GetIssue => "get_issue",
            Self::CreatePullRequest => "create_pull_request",
            Self::CommentOnIssue => "comment_on_issue",
            Self::CreateFile => "create_file",
            Self::ReadFile => "read_file",
            Self::UpdateFile => "update_file",
            Self::DeleteFile => "delete_file",
            Self::GetPrChanges => "get_pr_changes",
            Self::CreatePrChangeComment => "create_pr_change_comment",
            Self::ListFiles => "list_files",
            Self::ListFolders => "list_folders",
            Self::AppendFile => "append_file",
            Self::GetCommits => "get_commits",
        }
    }

    const fn group(self) -> &'static str {
        match self {
            Self::ListBranches
            | Self::GetIssues
            | Self::GetIssue
            | Self::ReadFile
            | Self::GetPrChanges
            | Self::ListFiles
            | Self::ListFolders
            | Self::GetCommits => "read",
            Self::DeleteFile => "delete",
            _ => "write",
        }
    }

    const fn is_read_only(self) -> bool {
        matches!(
            self,
            Self::ListBranches
                | Self::GetIssues
                | Self::GetIssue
                | Self::ReadFile
                | Self::GetPrChanges
                | Self::ListFiles
                | Self::ListFolders
                | Self::GetCommits
        )
    }

    const fn description(self) -> &'static str {
        match self {
            Self::CreateBranch => {
                "Create branch_name, for example feature/login, in one GitLab repository from the invocation's active branch, initially the configured main branch. repository is a full project path such as group/project; omission uses the first configured project. This is a remote write and returns the created or already-existing status."
            }
            Self::SetActiveBranch => {
                "Set the invocation-local default branch used as create_branch's source ref and by tree listings when branch is omitted; for example main or release/1.2. This does not modify GitLab."
            }
            Self::ListBranches => {
                "List at most limit branch names, default 20 and range 1 through 100, from one repository. branch_wildcard uses shell-style matching such as release/*. Returns names only and reads at most 10 pages or 1000 provider items."
            }
            Self::GetIssues => {
                "Read the first bounded page of open issues from one repository. Returns up to 20 issue titles and iids; it does not follow later pages."
            }
            Self::GetIssue => {
                "Read one positive issue iid and up to its first 10 notes. Returns `{title, body, comments:[{body:string,user:username string}]}` with a nullable description body."
            }
            Self::CreatePullRequest => {
                "Create a GitLab merge request from source branch to the toolkit's configured base branch, with title, body, and the created-by-agent label. This is a remote write and returns the new iid."
            }
            Self::CommentOnIssue => {
                "Add a note to an issue. comment_query format is `<positive issue iid>\\n\\n<non-empty comment>`, for example `42\\n\\nPlease add a regression test.` This is a remote write."
            }
            Self::CreateFile => {
                "Create one UTF-8 text file, including an empty file, at repository-relative file_path on branch with a generated commit message. Refuses an existing path; writable content is limited to 256 KiB and this is a remote write."
            }
            Self::ReadFile => {
                "Read one UTF-8 repository file at branch. Optional start_line and end_line are 1-indexed inclusive; use ranges after content_too_large guidance. Plain content is limited to 200000 characters and a 512 KiB serialized result; source files are limited to 1 MiB."
            }
            Self::UpdateFile => {
                "Update one supported text file on branch using one or more unambiguous OLD/NEW marker pairs, then commit the complete result with an optimistic last-commit check. Markers occupy dedicated lines; the query and complete writable result are limited to 256 KiB and this is a remote write."
            }
            Self::DeleteFile => {
                "Delete one repository-relative file from branch in a new commit with an optimistic last-commit check. This is a remote destructive effect."
            }
            Self::GetPrChanges => {
                "Read one merge-request iid and return title, description, and bounded unified diffs up to 512 KiB. Each displayed diff row has an index used by create_pr_change_comment."
            }
            Self::CreatePrChangeComment => {
                "Add a discussion at one displayed zero-based diff-row index from get_pr_changes. Supply the merge-request iid, exact old or new file path, nonnegative line_number index, and comment. This is a remote write."
            }
            Self::ListFiles => {
                "List repository-relative blob paths under required path on branch, using the invocation's active branch if omitted; recursive defaults true. Reads at most 10 pages and 1000 entries."
            }
            Self::ListFolders => {
                "List repository-relative folder paths under required path on branch, using the invocation's active branch if omitted; recursive defaults true. Reads at most 10 pages and 1000 entries."
            }
            Self::AppendFile => {
                "Append non-empty UTF-8 content after a newline to one existing text file on branch and commit the complete result with an optimistic last-commit check. The resulting writable file is limited to 256 KiB; this is a remote write."
            }
            Self::GetCommits => {
                "List at most 1000 commits from at most 10 pages in one repository, optionally filtered by ref sha, path, timezone-bearing RFC3339 since and until values, and author. Returns sha, author, createdAt, message, and web URL; narrow filters if the bounded traversal is exhausted."
            }
        }
    }
}

struct GitLabOrgTool {
    kind: GitLabOrgToolKind,
    client: Arc<dyn GitLabOrgApi>,
    description: Box<str>,
}

impl GitLabOrgTool {
    fn new(kind: GitLabOrgToolKind, toolkit_name: &str, client: Arc<dyn GitLabOrgApi>) -> Self {
        let description = format!("Toolkit: {toolkit_name}\n{}", kind.description());
        Self {
            kind,
            client,
            description: description
                .chars()
                .take(MAX_DESCRIPTION_BYTES)
                .collect::<String>()
                .into_boxed_str(),
        }
    }
}

#[async_trait]
impl Tool for GitLabOrgTool {
    fn name(&self) -> &str {
        self.kind.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        self.kind.is_read_only()
    }

    fn is_concurrency_safe(&self) -> bool {
        false
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(schema_for(self.kind))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        validate_argument_size(&arguments)?;
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        execute_kind(self.kind, &self.client, arguments).await
    }
}

#[allow(clippy::too_many_lines)] // Source-order argument mapping is one auditable ledger.
async fn execute_kind(
    kind: GitLabOrgToolKind,
    client: &Arc<dyn GitLabOrgApi>,
    arguments: &Map<String, Value>,
) -> adk_core::Result<Value> {
    let operation = match kind {
        GitLabOrgToolKind::CreateBranch => {
            reject_unknown_keys(arguments, &["branch_name", "repository"])?;
            GitLabOrgOperation::CreateBranch {
                branch_name: required_text(arguments, "branch_name")?,
                repository: optional_text(arguments, "repository")?,
            }
        }
        GitLabOrgToolKind::SetActiveBranch => {
            reject_unknown_keys(arguments, &["branch"])?;
            GitLabOrgOperation::SetActiveBranch {
                branch: required_text(arguments, "branch")?,
            }
        }
        GitLabOrgToolKind::ListBranches => {
            reject_unknown_keys(arguments, &["repository", "limit", "branch_wildcard"])?;
            GitLabOrgOperation::ListBranches {
                repository: optional_text(arguments, "repository")?,
                limit: optional_positive_usize(arguments, "limit", 20, 100)?,
                branch_wildcard: optional_text(arguments, "branch_wildcard")?,
            }
        }
        GitLabOrgToolKind::GetIssues => {
            reject_unknown_keys(arguments, &["repository"])?;
            GitLabOrgOperation::GetIssues {
                repository: optional_text(arguments, "repository")?,
            }
        }
        GitLabOrgToolKind::GetIssue => {
            reject_unknown_keys(arguments, &["issue_number", "repository"])?;
            GitLabOrgOperation::GetIssue {
                issue_number: positive_u64(arguments, "issue_number")?,
                repository: optional_text(arguments, "repository")?,
            }
        }
        GitLabOrgToolKind::CreatePullRequest => {
            reject_unknown_keys(arguments, &["pr_title", "pr_body", "branch", "repository"])?;
            GitLabOrgOperation::CreatePullRequest {
                title: required_text(arguments, "pr_title")?,
                body: required_text(arguments, "pr_body")?,
                branch: required_text(arguments, "branch")?,
                repository: optional_text(arguments, "repository")?,
            }
        }
        GitLabOrgToolKind::CommentOnIssue => {
            reject_unknown_keys(arguments, &["comment_query", "repository"])?;
            let query = required_text(arguments, "comment_query")?;
            let (issue, comment) = parse_comment_query(query)?;
            GitLabOrgOperation::CommentOnIssue {
                issue_number: issue,
                comment,
                repository: optional_text(arguments, "repository")?,
            }
        }
        GitLabOrgToolKind::CreateFile => {
            reject_unknown_keys(
                arguments,
                &["file_path", "file_contents", "branch", "repository"],
            )?;
            GitLabOrgOperation::CreateFile {
                file_path: required_text(arguments, "file_path")?,
                contents: required_string_allow_empty(arguments, "file_contents")?,
                branch: required_text(arguments, "branch")?,
                repository: optional_text(arguments, "repository")?,
            }
        }
        GitLabOrgToolKind::ReadFile => {
            reject_unknown_keys(
                arguments,
                &[
                    "file_path",
                    "branch",
                    "repository",
                    "start_line",
                    "end_line",
                ],
            )?;
            GitLabOrgOperation::ReadFile {
                file_path: required_text(arguments, "file_path")?,
                branch: required_text(arguments, "branch")?,
                repository: optional_text(arguments, "repository")?,
                start_line: optional_usize(arguments, "start_line")?,
                end_line: optional_usize(arguments, "end_line")?,
            }
        }
        GitLabOrgToolKind::UpdateFile => {
            reject_unknown_keys(
                arguments,
                &["file_path", "update_query", "branch", "repository"],
            )?;
            GitLabOrgOperation::UpdateFile {
                file_path: required_text(arguments, "file_path")?,
                update_query: required_text(arguments, "update_query")?,
                branch: required_text(arguments, "branch")?,
                repository: optional_text(arguments, "repository")?,
            }
        }
        GitLabOrgToolKind::DeleteFile => {
            reject_unknown_keys(arguments, &["file_path", "branch", "repository"])?;
            GitLabOrgOperation::DeleteFile {
                file_path: required_text(arguments, "file_path")?,
                branch: required_text(arguments, "branch")?,
                repository: optional_text(arguments, "repository")?,
            }
        }
        GitLabOrgToolKind::GetPrChanges => {
            reject_unknown_keys(arguments, &["pr_number", "repository"])?;
            GitLabOrgOperation::GetPrChanges {
                pr_number: positive_string_id(arguments, "pr_number")?,
                repository: optional_text(arguments, "repository")?,
            }
        }
        GitLabOrgToolKind::CreatePrChangeComment => {
            reject_unknown_keys(
                arguments,
                &[
                    "pr_number",
                    "file_path",
                    "line_number",
                    "comment",
                    "repository",
                ],
            )?;
            GitLabOrgOperation::CreatePrChangeComment {
                pr_number: positive_string_id(arguments, "pr_number")?,
                file_path: required_text(arguments, "file_path")?,
                line_number: nonnegative_usize(arguments, "line_number")?,
                comment: required_text(arguments, "comment")?,
                repository: optional_text(arguments, "repository")?,
            }
        }
        GitLabOrgToolKind::ListFiles => tree_operation(arguments, false)?,
        GitLabOrgToolKind::ListFolders => tree_operation(arguments, true)?,
        GitLabOrgToolKind::AppendFile => {
            reject_unknown_keys(arguments, &["file_path", "content", "branch", "repository"])?;
            GitLabOrgOperation::AppendFile {
                file_path: required_text(arguments, "file_path")?,
                content: required_text(arguments, "content")?,
                branch: required_text(arguments, "branch")?,
                repository: optional_text(arguments, "repository")?,
            }
        }
        GitLabOrgToolKind::GetCommits => {
            reject_unknown_keys(
                arguments,
                &["sha", "path", "since", "until", "author", "repository"],
            )?;
            GitLabOrgOperation::GetCommits {
                sha: optional_text(arguments, "sha")?,
                path: optional_text(arguments, "path")?,
                since: optional_text(arguments, "since")?,
                until: optional_text(arguments, "until")?,
                author: optional_text(arguments, "author")?,
                repository: optional_text(arguments, "repository")?,
            }
        }
    };
    client
        .execute(operation)
        .await
        .map_err(GitLabOrgClientError::into_adk)
}

fn tree_operation(
    arguments: &Map<String, Value>,
    folders: bool,
) -> Result<GitLabOrgOperation<'_>, AdkError> {
    reject_unknown_keys(arguments, &["path", "recursive", "branch", "repository"])?;
    let path = required_string_allow_empty(arguments, "path")?;
    let recursive = optional_bool(arguments, "recursive", true)?;
    let branch = optional_text(arguments, "branch")?;
    let repository = optional_text(arguments, "repository")?;
    Ok(if folders {
        GitLabOrgOperation::ListFolders {
            path,
            recursive,
            branch,
            repository,
        }
    } else {
        GitLabOrgOperation::ListFiles {
            path,
            recursive,
            branch,
            repository,
        }
    })
}

fn schema_for(kind: GitLabOrgToolKind) -> Value {
    match kind {
        GitLabOrgToolKind::CreateBranch => object_schema(
            "CreateBranchModel",
            Map::from_iter([
                (
                    "branch_name".to_owned(),
                    string_property(
                        "Branch name to create, for example `feature/login`; maximum 255 bytes.",
                        Some(MAX_BRANCH_BYTES),
                        false,
                    ),
                ),
                ("repository".to_owned(), repository_property()),
            ]),
            &["branch_name"],
        ),
        GitLabOrgToolKind::SetActiveBranch => object_schema(
            "SetActiveBranchModel",
            Map::from_iter([(
                "branch".to_owned(),
                string_property(
                    "Invocation-local active branch, for example `main` or `release/1.2`; maximum 255 bytes. This does not modify GitLab.",
                    Some(MAX_BRANCH_BYTES),
                    false,
                ),
            )]),
            &["branch"],
        ),
        GitLabOrgToolKind::ListBranches => object_schema(
            "ListBranchesModel",
            Map::from_iter([
                ("repository".to_owned(), repository_property()),
                (
                    "limit".to_owned(),
                    json!({"type":["integer","null"],"minimum":1,"maximum":100,"default":20,"description":"Maximum branch names to return; default 20, range 1 through 100."}),
                ),
                (
                    "branch_wildcard".to_owned(),
                    nullable_string_property(
                        "Optional shell-style branch filter, for example `release/*`; null returns all names before limit is applied.",
                        Some(MAX_BRANCH_BYTES),
                    ),
                ),
            ]),
            &[],
        ),
        GitLabOrgToolKind::GetIssues => repository_only_schema("GetIssuesModel"),
        GitLabOrgToolKind::GetIssue => object_schema(
            "GetIssueModel",
            Map::from_iter([
                (
                    "issue_number".to_owned(),
                    json!({"type":"integer","minimum":1,"description":"Positive project issue iid, for example 42."}),
                ),
                ("repository".to_owned(), repository_property()),
            ]),
            &["issue_number"],
        ),
        GitLabOrgToolKind::CreatePullRequest => object_schema(
            "CreatePullRequestModel",
            Map::from_iter([
                (
                    "pr_title".to_owned(),
                    string_property(
                        "Non-empty merge-request title describing the proposed change.",
                        Some(8 * 1_024),
                        false,
                    ),
                ),
                (
                    "pr_body".to_owned(),
                    string_property(
                        "Non-empty multiline merge-request description; maximum 256 KiB.",
                        Some(MAX_TEXT_BYTES),
                        false,
                    ),
                ),
                (
                    "branch".to_owned(),
                    string_property(
                        "Existing source branch. The target is the toolkit's configured base branch.",
                        Some(MAX_BRANCH_BYTES),
                        false,
                    ),
                ),
                ("repository".to_owned(), repository_property()),
            ]),
            &["pr_title", "pr_body", "branch"],
        ),
        GitLabOrgToolKind::CommentOnIssue => object_schema(
            "CommentOnIssueModel",
            Map::from_iter([
                (
                    "comment_query".to_owned(),
                    string_property(
                        "Packed value in format `<positive issue iid>\\n\\n<non-empty comment>`, for example `42\\n\\nPlease add a regression test.`",
                        Some(MAX_TEXT_BYTES),
                        false,
                    ),
                ),
                ("repository".to_owned(), repository_property()),
            ]),
            &["comment_query"],
        ),
        GitLabOrgToolKind::CreateFile => object_schema(
            "CreateFileModel",
            file_write_properties(
                "UTF-8 file contents to create; empty is allowed and the maximum is 256 KiB.",
            ),
            &["file_path", "file_contents", "branch"],
        ),
        GitLabOrgToolKind::ReadFile => {
            let mut properties = Map::from_iter([
                ("file_path".to_owned(), file_path_property()),
                (
                    "branch".to_owned(),
                    branch_property("Branch containing the file."),
                ),
                ("repository".to_owned(), repository_property()),
                (
                    "start_line".to_owned(),
                    json!({"type":["integer","null"],"minimum":1,"default":null,"description":"Optional 1-indexed inclusive first line. Omit or pass null to begin at line 1."}),
                ),
                (
                    "end_line".to_owned(),
                    json!({"type":["integer","null"],"minimum":1,"default":null,"description":"Optional 1-indexed inclusive final line. It must be at least start_line and not exceed the file's line count."}),
                ),
            ]);
            object_schema(
                "ReadFileModel",
                std::mem::take(&mut properties),
                &["file_path", "branch"],
            )
        }
        GitLabOrgToolKind::UpdateFile => object_schema(
            "UpdateFileModel",
            Map::from_iter([
                ("file_path".to_owned(), file_path_property()),
                (
                    "update_query".to_owned(),
                    string_property(
                        "One or more OLD/NEW pairs whose markers occupy dedicated lines: `OLD<<<<`, `>>>>OLD`, `NEW<<<<`, `>>>>NEW`. Each OLD block must match uniquely; maximum 256 KiB.",
                        Some(MAX_TEXT_BYTES),
                        false,
                    ),
                ),
                (
                    "branch".to_owned(),
                    branch_property(
                        "Branch whose complete file content will be updated and committed.",
                    ),
                ),
                ("repository".to_owned(), repository_property()),
            ]),
            &["file_path", "update_query", "branch"],
        ),
        GitLabOrgToolKind::DeleteFile => object_schema(
            "DeleteFileModel",
            common_file_properties(),
            &["file_path", "branch"],
        ),
        GitLabOrgToolKind::GetPrChanges => object_schema(
            "GetPrChangesModel",
            Map::from_iter([
                ("pr_number".to_owned(), pr_number_property()),
                ("repository".to_owned(), repository_property()),
            ]),
            &["pr_number"],
        ),
        GitLabOrgToolKind::CreatePrChangeComment => object_schema(
            "CreatePrChangeCommentModel",
            Map::from_iter([
                ("pr_number".to_owned(), pr_number_property()),
                (
                    "file_path".to_owned(),
                    string_property(
                        "Exact old or new repository-relative path shown by get_pr_changes.",
                        Some(MAX_PATH_BYTES),
                        false,
                    ),
                ),
                (
                    "line_number".to_owned(),
                    json!({"type":"integer","minimum":0,"description":"Zero-based displayed diff-row index from get_pr_changes; for example 0 for the first displayed row."}),
                ),
                (
                    "comment".to_owned(),
                    string_property(
                        "Non-empty multiline discussion comment; maximum 256 KiB.",
                        Some(MAX_TEXT_BYTES),
                        false,
                    ),
                ),
                ("repository".to_owned(), repository_property()),
            ]),
            &["pr_number", "file_path", "line_number", "comment"],
        ),
        GitLabOrgToolKind::ListFiles | GitLabOrgToolKind::ListFolders => object_schema(
            // The pinned SDK accidentally publishes ListFoldersModel for both.
            "ListFoldersModel",
            Map::from_iter([
                (
                    "path".to_owned(),
                    string_property(
                        "Required repository-relative directory path; use an empty string for the repository root. Maximum 1024 bytes.",
                        Some(MAX_PATH_BYTES),
                        true,
                    ),
                ),
                (
                    "recursive".to_owned(),
                    json!({"type":["boolean","null"],"default":true,"description":"Whether to include descendants; defaults true."}),
                ),
                (
                    "branch".to_owned(),
                    nullable_string_property(
                        "Optional branch; omit or pass null to use the invocation-local active branch.",
                        Some(MAX_BRANCH_BYTES),
                    ),
                ),
                ("repository".to_owned(), repository_property()),
            ]),
            &["path"],
        ),
        GitLabOrgToolKind::AppendFile => object_schema(
            "AppendFileModel",
            Map::from_iter([
                ("file_path".to_owned(), file_path_property()),
                (
                    "content".to_owned(),
                    string_property(
                        "Non-empty UTF-8 multiline content appended after one newline; the complete result is limited to 256 KiB.",
                        Some(256 * 1_024),
                        false,
                    ),
                ),
                (
                    "branch".to_owned(),
                    branch_property("Branch containing the existing file."),
                ),
                ("repository".to_owned(), repository_property()),
            ]),
            &["file_path", "content", "branch"],
        ),
        GitLabOrgToolKind::GetCommits => object_schema(
            "GetCommitsModel",
            Map::from_iter([
                (
                    "sha".to_owned(),
                    nullable_string_property(
                        "Optional branch, tag, or commit ref used as ref_name.",
                        Some(MAX_PATH_BYTES),
                    ),
                ),
                (
                    "path".to_owned(),
                    nullable_string_property(
                        "Optional repository-relative path filter.",
                        Some(MAX_PATH_BYTES),
                    ),
                ),
                (
                    "since".to_owned(),
                    nullable_string_property(
                        "Optional timezone-bearing RFC3339 lower bound, for example `2025-01-01T00:00:00Z`.",
                        Some(128),
                    ),
                ),
                (
                    "until".to_owned(),
                    nullable_string_property(
                        "Optional timezone-bearing RFC3339 upper bound, for example `2025-01-31T23:59:59+00:00`.",
                        Some(128),
                    ),
                ),
                (
                    "author".to_owned(),
                    nullable_string_property(
                        "Optional author-name or email filter.",
                        Some(MAX_PATH_BYTES),
                    ),
                ),
                ("repository".to_owned(), repository_property()),
            ]),
            &[],
        ),
    }
}

fn object_schema(title: &str, properties: Map<String, Value>, required: &[&str]) -> Value {
    Value::Object(Map::from_iter([
        ("title".to_owned(), Value::String(title.to_owned())),
        ("type".to_owned(), Value::String("object".to_owned())),
        ("properties".to_owned(), Value::Object(properties)),
        ("required".to_owned(), json!(required)),
        ("additionalProperties".to_owned(), Value::Bool(false)),
    ]))
}

fn repository_only_schema(title: &str) -> Value {
    object_schema(
        title,
        Map::from_iter([("repository".to_owned(), repository_property())]),
        &[],
    )
}

fn repository_property() -> Value {
    nullable_string_property(
        "Optional full project path such as `group/project`. Omit or pass null to use the first configured repository; organization-wide mode requires an explicit path.",
        Some(MAX_PATH_BYTES),
    )
}

fn string_property(description: &str, max: Option<usize>, allow_empty: bool) -> Value {
    let mut property = Map::from_iter([
        ("type".to_owned(), Value::String("string".to_owned())),
        (
            "description".to_owned(),
            Value::String(description.to_owned()),
        ),
    ]);
    property.insert("minLength".to_owned(), Value::from(u8::from(!allow_empty)));
    if let Some(max) = max {
        property.insert("maxLength".to_owned(), Value::from(max));
    }
    Value::Object(property)
}

fn nullable_string_property(description: &str, max: Option<usize>) -> Value {
    let mut property = Map::from_iter([
        ("type".to_owned(), json!(["string", "null"])),
        ("default".to_owned(), Value::Null),
        (
            "description".to_owned(),
            Value::String(description.to_owned()),
        ),
    ]);
    if let Some(max) = max {
        property.insert("maxLength".to_owned(), Value::from(max));
    }
    Value::Object(property)
}

fn file_path_property() -> Value {
    string_property(
        "Repository-relative file path, for example `src/lib.rs`; maximum 1024 bytes.",
        Some(MAX_PATH_BYTES),
        false,
    )
}

fn branch_property(description: &str) -> Value {
    string_property(description, Some(MAX_BRANCH_BYTES), false)
}

fn common_file_properties() -> Map<String, Value> {
    Map::from_iter([
        ("file_path".to_owned(), file_path_property()),
        (
            "branch".to_owned(),
            branch_property("Branch containing the file."),
        ),
        ("repository".to_owned(), repository_property()),
    ])
}

fn file_write_properties(contents_description: &str) -> Map<String, Value> {
    let mut properties = common_file_properties();
    properties.insert(
        "file_contents".to_owned(),
        string_property(contents_description, Some(256 * 1_024), true),
    );
    properties
}

fn pr_number_property() -> Value {
    json!({"type":"string","pattern":"^[1-9][0-9]*$","maxLength":20,"description":"Positive merge-request iid encoded as a decimal string, for example `42`."})
}

fn validate_argument_size(arguments: &Value) -> Result<(), AdkError> {
    if serde_json::to_vec(arguments)
        .map_err(|_| invalid_arguments())?
        .len()
        > MAX_ARGUMENT_BYTES
    {
        return Err(resource_exhausted_arguments());
    }
    Ok(())
}

fn reject_unknown_keys(arguments: &Map<String, Value>, allowed: &[&str]) -> Result<(), AdkError> {
    if arguments
        .keys()
        .any(|name| !allowed.contains(&name.as_str()))
    {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn required_text<'a>(arguments: &'a Map<String, Value>, name: &str) -> Result<&'a str, AdkError> {
    arguments
        .get(name)
        .and_then(Value::as_str)
        .filter(|value| !value.trim().is_empty())
        .ok_or_else(invalid_arguments)
}

fn required_string_allow_empty<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
) -> Result<&'a str, AdkError> {
    arguments
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_arguments)
}

fn optional_text<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
) -> Result<Option<&'a str>, AdkError> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) if value.trim().is_empty() => Ok(None),
        Some(Value::String(value)) => Ok(Some(value)),
        Some(_) => Err(invalid_arguments()),
    }
}

fn positive_u64(arguments: &Map<String, Value>, name: &str) -> Result<u64, AdkError> {
    arguments
        .get(name)
        .and_then(Value::as_u64)
        .filter(|value| *value > 0)
        .ok_or_else(invalid_arguments)
}

fn positive_string_id(arguments: &Map<String, Value>, name: &str) -> Result<u64, AdkError> {
    arguments
        .get(name)
        .and_then(Value::as_str)
        .and_then(|value| value.parse::<u64>().ok())
        .filter(|value| *value > 0)
        .ok_or_else(invalid_arguments)
}

fn optional_usize(arguments: &Map<String, Value>, name: &str) -> Result<Option<usize>, AdkError> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(value) => value
            .as_u64()
            .and_then(|value| usize::try_from(value).ok())
            .filter(|value| *value > 0)
            .map(Some)
            .ok_or_else(invalid_arguments),
    }
}

fn nonnegative_usize(arguments: &Map<String, Value>, name: &str) -> Result<usize, AdkError> {
    arguments
        .get(name)
        .and_then(Value::as_u64)
        .and_then(|value| usize::try_from(value).ok())
        .ok_or_else(invalid_arguments)
}

fn optional_positive_usize(
    arguments: &Map<String, Value>,
    name: &str,
    default: usize,
    max: usize,
) -> Result<usize, AdkError> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(default),
        Some(value) => value
            .as_u64()
            .and_then(|value| usize::try_from(value).ok())
            .filter(|value| (1..=max).contains(value))
            .ok_or_else(invalid_arguments),
    }
}

fn optional_bool(
    arguments: &Map<String, Value>,
    name: &str,
    default: bool,
) -> Result<bool, AdkError> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(default),
        Some(Value::Bool(value)) => Ok(*value),
        Some(_) => Err(invalid_arguments()),
    }
}

fn parse_comment_query(value: &str) -> Result<(u64, &str), AdkError> {
    let (issue, comment) = value.split_once("\n\n").ok_or_else(invalid_arguments)?;
    let issue = issue
        .parse::<u64>()
        .ok()
        .filter(|value| *value > 0)
        .ok_or_else(invalid_arguments)?;
    if comment.trim().is_empty() {
        return Err(invalid_arguments());
    }
    Ok((issue, comment))
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "gitlab_org.arguments.invalid",
        "the GitLab Org tool arguments are invalid",
    )
}

fn resource_exhausted_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "gitlab_org.arguments.resource_exhausted",
        "the GitLab Org tool arguments exceed the approved limit",
    )
}

#[cfg(test)]
pub(in crate::toolkits) fn test_catalog() -> Vec<(&'static str, &'static str)> {
    GitLabOrgToolKind::ALL
        .iter()
        .map(|kind| (kind.name(), kind.group()))
        .collect()
}
