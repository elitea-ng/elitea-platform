use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::families::vcs_text::{MAX_BATCH_FILES, MAX_CONTEXT_LINES, MAX_PATTERN_BYTES};
use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{GitLabApi, GitLabClient, GitLabClientError, GitLabOperation};
use super::config::{GitLabConfigError, GitLabConfigErrorCode, GitLabToolkitConfig};

const MAX_ARGUMENT_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 1_000;

/// The SDK `gitlab` index tools. Indexing has no Rust runtime yet, so a
/// selection naming one is served without it (the capability snapshot marks
/// the family partial) instead of failing the whole toolkit.
const UNSERVED_INDEX_TOOLS: [&str; 6] = [
    "index_data",
    "list_indexes",
    "remove_index",
    "search_index",
    "stepback_search_index",
    "stepback_summary_index",
];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum GitLabToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

pub(crate) struct GitLabToolsetError {
    code: GitLabToolsetErrorCode,
}

impl GitLabToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> GitLabToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for GitLabToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("GitLabToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for GitLabToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            GitLabToolsetErrorCode::InvalidConfiguration => {
                "the GitLab toolkit configuration is invalid"
            }
            GitLabToolsetErrorCode::ResourceExhausted => {
                "the GitLab toolkit configuration exceeds its approved limit"
            }
            GitLabToolsetErrorCode::UnsupportedSelection => {
                "the selected GitLab tool profile is not supported"
            }
            GitLabToolsetErrorCode::Client => "the GitLab client could not be created",
            GitLabToolsetErrorCode::InvalidDefinition => {
                "the GitLab ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for GitLabToolsetError {}

impl From<GitLabConfigError> for GitLabToolsetError {
    fn from(source: GitLabConfigError) -> Self {
        Self {
            code: match source.code() {
                GitLabConfigErrorCode::InvalidConfiguration => {
                    GitLabToolsetErrorCode::InvalidConfiguration
                }
                GitLabConfigErrorCode::ResourceExhausted => {
                    GitLabToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<GitLabClientError> for GitLabToolsetError {
    fn from(_: GitLabClientError) -> Self {
        Self {
            code: GitLabToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for GitLabToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: GitLabToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the SDK `gitlab` toolkit's non-index tools for one project.
pub(crate) fn build_gitlab_toolset(
    toolkit_name: &str,
    config: GitLabToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, GitLabToolsetError> {
    let selected = validate_selection(config.selected_tools())?;
    let omitted_count = config.selected_tools().len() - selected.len();
    if omitted_count > 0 {
        tracing::warn!(
            event = "agent_toolkit_tools_skipped",
            reason_code = "unsupported_tool_selection",
            toolkit_type = "gitlab",
            toolkit_name,
            selected_count = config.selected_tools().len(),
            materialized_count = selected.len(),
            omitted_count,
            "GitLab index tools were omitted because this runtime has no indexing"
        );
    }
    let client: Arc<dyn GitLabApi> = Arc::new(GitLabClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

/// The selected names this family serves. An unknown name fails closed; a
/// known SDK index tool is dropped. A selection of only index tools is
/// refused, because the empty remainder would otherwise mean "all tools".
fn validate_selection(selected: &[Box<str>]) -> Result<Vec<String>, GitLabToolsetError> {
    let mut served = Vec::with_capacity(selected.len());
    for name in selected {
        if GitLabToolKind::ALL
            .iter()
            .any(|kind| kind.name() == name.as_ref())
        {
            served.push(name.to_string());
        } else if !UNSERVED_INDEX_TOOLS.contains(&name.as_ref()) {
            return Err(unsupported_selection());
        }
    }
    if !selected.is_empty() && served.is_empty() {
        return Err(unsupported_selection());
    }
    Ok(served)
}

const fn unsupported_selection() -> GitLabToolsetError {
    GitLabToolsetError {
        code: GitLabToolsetErrorCode::UnsupportedSelection,
    }
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn GitLabApi>,
) -> Result<BasicToolset, GitLabToolsetError> {
    let include_all = selected.is_empty();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(GitLabToolKind::ALL.len());
    for kind in GitLabToolKind::ALL {
        if include_all || selected.iter().any(|name| name == kind.name()) {
            tools.push(Arc::new(GitLabTool::new(
                kind,
                toolkit_name,
                Arc::clone(client),
            )));
        }
    }
    admit_materialized_toolset(toolkit_name, "gitlab", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    selected: &[Box<str>],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn GitLabApi>,
) -> Result<BasicToolset, GitLabToolsetError> {
    let selected = validate_selection(selected)?;
    build_with_api(toolkit_name, &selected, policy, client)
}

#[derive(Clone, Copy)]
enum GitLabToolKind {
    CreateBranch,
    DeleteBranch,
    ListBranches,
    ListFiles,
    ListFolders,
    GetIssues,
    GetIssue,
    CreatePullRequest,
    CommentOnIssue,
    CommentOnPr,
    CreateFile,
    ReadFile,
    UpdateFile,
    AppendFile,
    DeleteFile,
    SetActiveBranch,
    GetPrChanges,
    CreatePrChangeComment,
    GetCommits,
    ReadMultipleFiles,
    GrepFile,
}

impl GitLabToolKind {
    /// The SDK's `get_available_tools` order, then the file-operation tools
    /// its decorator appends.
    const ALL: [Self; 21] = [
        Self::CreateBranch,
        Self::DeleteBranch,
        Self::ListBranches,
        Self::ListFiles,
        Self::ListFolders,
        Self::GetIssues,
        Self::GetIssue,
        Self::CreatePullRequest,
        Self::CommentOnIssue,
        Self::CommentOnPr,
        Self::CreateFile,
        Self::ReadFile,
        Self::UpdateFile,
        Self::AppendFile,
        Self::DeleteFile,
        Self::SetActiveBranch,
        Self::GetPrChanges,
        Self::CreatePrChangeComment,
        Self::GetCommits,
        Self::ReadMultipleFiles,
        Self::GrepFile,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::CreateBranch => "create_branch",
            Self::DeleteBranch => "delete_branch",
            Self::ListBranches => "list_branches_in_repo",
            Self::ListFiles => "list_files",
            Self::ListFolders => "list_folders",
            Self::GetIssues => "get_issues",
            Self::GetIssue => "get_issue",
            Self::CreatePullRequest => "create_pull_request",
            Self::CommentOnIssue => "comment_on_issue",
            Self::CommentOnPr => "comment_on_pr",
            Self::CreateFile => "create_file",
            Self::ReadFile => "read_file",
            Self::UpdateFile => "update_file",
            Self::AppendFile => "append_file",
            Self::DeleteFile => "delete_file",
            Self::SetActiveBranch => "set_active_branch",
            Self::GetPrChanges => "get_pr_changes",
            Self::CreatePrChangeComment => "create_pr_change_comment",
            Self::GetCommits => "get_commits",
            Self::ReadMultipleFiles => "read_multiple_files",
            Self::GrepFile => "grep_file",
        }
    }

    const fn group(self) -> &'static str {
        match self {
            Self::ListBranches
            | Self::ListFiles
            | Self::ListFolders
            | Self::GetIssues
            | Self::GetIssue
            | Self::ReadFile
            | Self::GetPrChanges
            | Self::GetCommits
            | Self::ReadMultipleFiles
            | Self::GrepFile => "read",
            Self::DeleteBranch | Self::DeleteFile => "delete",
            _ => "write",
        }
    }

    const fn description(self) -> &'static str {
        match self {
            Self::CreateBranch => {
                "Create branch_name from the active branch (initially the toolkit's base branch) and make it active. If it already exists it is only made active. This is a remote write."
            }
            Self::DeleteBranch => {
                "Delete a branch. main, master and the toolkit's base branch are refused; deleting the active branch needs force=true and resets the active branch to the base branch. This is a remote destructive effect."
            }
            Self::ListBranches => {
                "List branch names, at most limit (default 20), optionally filtered by a shell-style branch_wildcard such as release/*. Reads at most 10 pages or 1000 branches."
            }
            Self::ListFiles => {
                "List repository file paths under path (default the root) on branch (default the active branch); recursive defaults to true. Reads at most 10 pages or 1000 entries."
            }
            Self::ListFolders => {
                "List repository folder paths under path (default the root) on branch (default the active branch); recursive defaults to true. Reads at most 10 pages or 1000 entries."
            }
            Self::GetIssues => {
                "Get the first page of open issues as `Found N issues:` followed by their titles and numbers."
            }
            Self::GetIssue => {
                "Get one issue by number: its title, body and its first page of comments as {body, user}."
            }
            Self::CreatePullRequest => {
                "Create a merge request from branch into the toolkit's base branch with pr_title, pr_body and the created-by-agent label. Returns the new merge request number. This is a remote write."
            }
            Self::CommentOnIssue => {
                "Comment on an issue. comment_query is the issue number, two newlines, then the comment, for example `42\\n\\nPlease add a test.` This is a remote write."
            }
            Self::CommentOnPr => {
                "Add a general comment to merge request pr_number. This is a remote write."
            }
            Self::CreateFile => {
                "Create file_path with file_contents on branch, which becomes the active branch. An existing file is not overwritten. This is a remote write."
            }
            Self::ReadFile => {
                "Read a UTF-8 text file from branch, which becomes the active branch. Optional start_line and end_line are 1-indexed and inclusive. Results over 200000 characters return content_too_large guidance with the valid line range."
            }
            Self::UpdateFile => {
                "Edit a file on branch. file_query's first non-empty line is the file path, followed by one or more OLD <<<< … >>>> OLD / NEW <<<< … >>>> NEW blocks, each marker on its own line; each OLD block must match exactly once. The base branch is protected. This is a remote write."
            }
            Self::AppendFile => {
                "Append content after a newline to an existing file on branch. The base branch is protected. This is a remote write."
            }
            Self::DeleteFile => {
                "Delete file_path from branch with an optional commit_message. This is a remote destructive effect."
            }
            Self::SetActiveBranch => {
                "Set the active branch used as create_branch's source and by listings when branch is omitted. This does not modify GitLab."
            }
            Self::GetPrChanges => {
                "Get a merge request's title, description and every change as a git diff."
            }
            Self::CreatePrChangeComment => {
                "Add an inline comment to a merge request. line_number is the 0-based line index within that file's diff as shown by get_pr_changes, not the file line number. This is a remote write."
            }
            Self::GetCommits => {
                "List the first page of commits, optionally filtered by sha (a ref name), path, since, until and author. Returns sha, author, createdAt, message and url."
            }
            Self::ReadMultipleFiles => {
                "Read several files from branch (default the active branch), each optionally limited to limit lines from offset. Returns a map of path to content; reading stops at a cumulative 200000 characters and later files are reported as skipped."
            }
            Self::GrepFile => {
                "Search for text or a regular expression INSIDE one file's content (like grep), case-insensitively, with context_lines of context. Not for finding files by name; use list_files for that."
            }
        }
    }
}

struct GitLabTool {
    kind: GitLabToolKind,
    client: Arc<dyn GitLabApi>,
    description: Box<str>,
}

impl GitLabTool {
    fn new(kind: GitLabToolKind, toolkit_name: &str, client: Arc<dyn GitLabApi>) -> Self {
        let description = format!("Toolkit: {toolkit_name}\n{}", kind.description());
        let mut end = description.len().min(MAX_DESCRIPTION_BYTES);
        while !description.is_char_boundary(end) {
            end -= 1;
        }
        Self {
            kind,
            client,
            description: description[..end].into(),
        }
    }
}

#[async_trait]
impl Tool for GitLabTool {
    fn name(&self) -> &str {
        self.kind.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        self.kind.group() == "read"
    }

    fn is_concurrency_safe(&self) -> bool {
        // Reads move the shared active branch, as the SDK's do.
        false
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(schema_for(self.kind))
    }

    #[allow(clippy::too_many_lines)] // Source-order argument mapping is one auditable ledger.
    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        if serde_json::to_vec(&arguments)
            .map_err(|_| invalid_arguments())?
            .len()
            > MAX_ARGUMENT_BYTES
        {
            return Err(resource_exhausted_arguments());
        }
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        let operation = match self.kind {
            GitLabToolKind::CreateBranch => {
                allow(arguments, &["branch_name"])?;
                GitLabOperation::CreateBranch {
                    branch_name: required_text(arguments, "branch_name")?,
                }
            }
            GitLabToolKind::DeleteBranch => {
                allow(arguments, &["branch_name", "force"])?;
                GitLabOperation::DeleteBranch {
                    branch_name: required_text(arguments, "branch_name")?,
                    force: optional_bool(arguments, "force")?.unwrap_or(false),
                }
            }
            GitLabToolKind::ListBranches => {
                allow(arguments, &["limit", "branch_wildcard"])?;
                GitLabOperation::ListBranches {
                    // The policy wrapper drops explicit nulls as BaseAction
                    // does, so the SDK default 20 also stands for `null`.
                    limit: match arguments.get("limit") {
                        None | Some(Value::Null) => 20,
                        Some(value) => positive_usize(value)?,
                    },
                    branch_wildcard: optional_text(arguments, "branch_wildcard")?,
                }
            }
            GitLabToolKind::ListFiles | GitLabToolKind::ListFolders => {
                allow(arguments, &["path", "recursive", "branch"])?;
                let path = optional_text(arguments, "path")?;
                let recursive = optional_bool(arguments, "recursive")?.unwrap_or(true);
                let branch = optional_text(arguments, "branch")?;
                if matches!(self.kind, GitLabToolKind::ListFiles) {
                    GitLabOperation::ListFiles {
                        path,
                        recursive,
                        branch,
                    }
                } else {
                    GitLabOperation::ListFolders {
                        path,
                        recursive,
                        branch,
                    }
                }
            }
            GitLabToolKind::GetIssues => {
                allow(arguments, &[])?;
                GitLabOperation::GetIssues
            }
            GitLabToolKind::GetIssue => {
                allow(arguments, &["issue_number"])?;
                GitLabOperation::GetIssue {
                    issue_number: required_u64(arguments, "issue_number")?,
                }
            }
            GitLabToolKind::CreatePullRequest => {
                allow(arguments, &["pr_title", "pr_body", "branch"])?;
                GitLabOperation::CreatePullRequest {
                    title: required_text(arguments, "pr_title")?,
                    body: required_string(arguments, "pr_body")?,
                    branch: required_text(arguments, "branch")?,
                }
            }
            GitLabToolKind::CommentOnIssue => {
                allow(arguments, &["comment_query"])?;
                GitLabOperation::CommentOnIssue {
                    comment_query: required_text(arguments, "comment_query")?,
                }
            }
            GitLabToolKind::CommentOnPr => {
                allow(arguments, &["pr_number", "comment"])?;
                GitLabOperation::CommentOnPr {
                    pr_number: required_u64(arguments, "pr_number")?,
                    comment: required_text(arguments, "comment")?,
                }
            }
            GitLabToolKind::CreateFile => {
                allow(arguments, &["file_path", "file_contents", "branch"])?;
                GitLabOperation::CreateFile {
                    file_path: required_text(arguments, "file_path")?,
                    contents: required_string(arguments, "file_contents")?,
                    branch: optional_text(arguments, "branch")?,
                }
            }
            GitLabToolKind::ReadFile => {
                allow(
                    arguments,
                    &["file_path", "branch", "start_line", "end_line"],
                )?;
                GitLabOperation::ReadFile {
                    file_path: required_text(arguments, "file_path")?,
                    branch: optional_text(arguments, "branch")?,
                    start_line: optional_positive_usize(arguments, "start_line")?,
                    end_line: optional_positive_usize(arguments, "end_line")?,
                }
            }
            GitLabToolKind::UpdateFile => {
                allow(arguments, &["file_query", "branch"])?;
                GitLabOperation::UpdateFile {
                    file_query: required_text(arguments, "file_query")?,
                    branch: required_text(arguments, "branch")?,
                }
            }
            GitLabToolKind::AppendFile => {
                allow(arguments, &["file_path", "content", "branch"])?;
                GitLabOperation::AppendFile {
                    file_path: required_text(arguments, "file_path")?,
                    content: required_string(arguments, "content")?,
                    branch: required_text(arguments, "branch")?,
                }
            }
            GitLabToolKind::DeleteFile => {
                allow(arguments, &["file_path", "branch", "commit_message"])?;
                GitLabOperation::DeleteFile {
                    file_path: required_text(arguments, "file_path")?,
                    branch: optional_text(arguments, "branch")?,
                    commit_message: optional_text(arguments, "commit_message")?,
                }
            }
            GitLabToolKind::SetActiveBranch => {
                allow(arguments, &["branch_name"])?;
                GitLabOperation::SetActiveBranch {
                    branch_name: required_text(arguments, "branch_name")?,
                }
            }
            GitLabToolKind::GetPrChanges => {
                allow(arguments, &["pr_number"])?;
                GitLabOperation::GetPrChanges {
                    pr_number: required_u64(arguments, "pr_number")?,
                }
            }
            GitLabToolKind::CreatePrChangeComment => {
                allow(
                    arguments,
                    &["pr_number", "file_path", "line_number", "comment"],
                )?;
                GitLabOperation::CreatePrChangeComment {
                    pr_number: required_u64(arguments, "pr_number")?,
                    file_path: required_text(arguments, "file_path")?,
                    line_number: usize::try_from(required_u64_allow_zero(
                        arguments,
                        "line_number",
                    )?)
                    .map_err(|_| invalid_arguments())?,
                    comment: required_text(arguments, "comment")?,
                }
            }
            GitLabToolKind::GetCommits => {
                allow(arguments, &["sha", "path", "since", "until", "author"])?;
                GitLabOperation::GetCommits {
                    sha: optional_text(arguments, "sha")?,
                    path: optional_text(arguments, "path")?,
                    since: optional_text(arguments, "since")?,
                    until: optional_text(arguments, "until")?,
                    author: optional_text(arguments, "author")?,
                }
            }
            GitLabToolKind::ReadMultipleFiles => {
                allow(arguments, &["file_paths", "branch", "offset", "limit"])?;
                GitLabOperation::ReadMultipleFiles {
                    file_paths: file_paths(arguments)?,
                    branch: optional_text(arguments, "branch")?,
                    offset: optional_positive_usize(arguments, "offset")?,
                    limit: optional_positive_usize(arguments, "limit")?,
                }
            }
            GitLabToolKind::GrepFile => {
                allow(
                    arguments,
                    &[
                        "file_path",
                        "pattern",
                        "branch",
                        "is_regex",
                        "context_lines",
                    ],
                )?;
                let pattern = required_text(arguments, "pattern")?;
                if pattern.len() > MAX_PATTERN_BYTES {
                    return Err(resource_exhausted_arguments());
                }
                let context_lines = match arguments.get("context_lines") {
                    None | Some(Value::Null) => 2,
                    Some(value) => nonnegative_usize(value)?,
                };
                if context_lines > MAX_CONTEXT_LINES {
                    return Err(invalid_arguments());
                }
                GitLabOperation::GrepFile {
                    file_path: required_text(arguments, "file_path")?,
                    pattern,
                    branch: optional_text(arguments, "branch")?,
                    is_regex: optional_bool(arguments, "is_regex")?.unwrap_or(true),
                    context_lines,
                }
            }
        };
        self.client
            .execute(operation)
            .await
            .map_err(GitLabClientError::into_adk)
    }
}

#[allow(clippy::too_many_lines)] // One schema per SDK args model, in source order.
fn schema_for(kind: GitLabToolKind) -> Value {
    match kind {
        GitLabToolKind::CreateBranch => object(
            "CreateBranchModel",
            &[(
                "branch_name",
                text("Branch Name", "The name of the branch, e.g. `my_branch`."),
            )],
            &["branch_name"],
        ),
        GitLabToolKind::DeleteBranch => object(
            "DeleteBranchModel",
            &[
                (
                    "branch_name",
                    text(
                        "Branch Name",
                        "The name of the branch to delete, e.g. `my_branch`. Cannot delete main or master branches.",
                    ),
                ),
                (
                    "force",
                    json!({"title":"Force","type":"boolean","default":false,"description":"If True, allows deleting the active branch (will auto-reset to base branch). Default is False."}),
                ),
            ],
            &["branch_name"],
        ),
        GitLabToolKind::ListBranches => object(
            "ListBranchesInRepoModel",
            &[
                (
                    "limit",
                    json!({"title":"Limit","type":["integer","null"],"minimum":1,"default":20,"description":"Maximum number of branches to return. If not provided, all branches will be returned."}),
                ),
                (
                    "branch_wildcard",
                    nullable_text(
                        "Branch Wildcard",
                        "Wildcard pattern to filter branches by name. If not provided, all branches will be returned.",
                    ),
                ),
            ],
            &[],
        ),
        GitLabToolKind::ListFiles => tree_schema("ListFilesModel", "files"),
        GitLabToolKind::ListFolders => tree_schema("ListFoldersModel", "folders"),
        GitLabToolKind::GetIssues => object("GetIssuesModel", &[], &[]),
        GitLabToolKind::GetIssue => object(
            "GetIssueModel",
            &[(
                "issue_number",
                integer("Issue Number", "The number of the issue", 1),
            )],
            &["issue_number"],
        ),
        GitLabToolKind::CreatePullRequest => object(
            "CreatePullRequestModel",
            &[
                (
                    "pr_title",
                    text("Pr Title", "The title of the pull request"),
                ),
                ("pr_body", text("Pr Body", "The body of the pull request")),
                (
                    "branch",
                    text("Branch", "The branch to create the pull request from"),
                ),
            ],
            &["pr_title", "pr_body", "branch"],
        ),
        GitLabToolKind::CommentOnIssue => object(
            "CommentOnIssueModel",
            &[(
                "comment_query",
                text(
                    "Comment Query",
                    "The issue number, two newlines, then the comment, e.g. `42\\n\\nLooks good.`",
                ),
            )],
            &["comment_query"],
        ),
        GitLabToolKind::CommentOnPr => object(
            "CommentOnPRModel",
            &[
                (
                    "pr_number",
                    integer(
                        "Pr Number",
                        "The number of the pull request/merge request",
                        1,
                    ),
                ),
                ("comment", text("Comment", "The comment text to add")),
            ],
            &["pr_number", "comment"],
        ),
        GitLabToolKind::CreateFile => object(
            "CreateFileModel",
            &[
                ("file_path", text("File Path", "The path of the file")),
                (
                    "file_contents",
                    text("File Contents", "The contents of the file"),
                ),
                ("branch", text("Branch", "The branch to create the file in")),
            ],
            &["file_path", "file_contents", "branch"],
        ),
        GitLabToolKind::ReadFile => object(
            "ReadFileModel",
            &[
                ("file_path", text("File Path", "The path of the file")),
                ("branch", text("Branch", "The branch to read the file from")),
                (
                    "start_line",
                    nullable_integer(
                        "Start Line",
                        "Starting line number (1-indexed, inclusive) for a partial read. Omit to read from the beginning.",
                    ),
                ),
                (
                    "end_line",
                    nullable_integer(
                        "End Line",
                        "Ending line number (1-indexed, inclusive) for a partial read. Omit to read to the end.",
                    ),
                ),
            ],
            &["file_path", "branch"],
        ),
        GitLabToolKind::UpdateFile => object(
            "UpdateFileModel",
            &[
                (
                    "file_query",
                    json!({"title":"File Query","type":"string","multiline":true,"description":"First non-empty line: the file path (not starting with a slash). Then one or more edits, each marker on its own line:\nOLD <<<<\nold content\n>>>> OLD\nNEW <<<<\nnew content\n>>>> NEW\nLeading/trailing whitespace in content is stripped."}),
                ),
                ("branch", text("Branch", "The branch to update the file in")),
            ],
            &["file_query", "branch"],
        ),
        GitLabToolKind::AppendFile => object(
            "AppendFileModel",
            &[
                ("file_path", text("File Path", "The path of the file")),
                (
                    "content",
                    text("Content", "The content to append to the file"),
                ),
                ("branch", text("Branch", "The branch to append the file in")),
            ],
            &["file_path", "content", "branch"],
        ),
        GitLabToolKind::DeleteFile => object(
            "DeleteFileModel",
            &[
                ("file_path", text("File Path", "The path of the file")),
                (
                    "branch",
                    text("Branch", "The branch to delete the file from"),
                ),
                (
                    "commit_message",
                    nullable_text(
                        "Commit Message",
                        "Commit message for deleting the file. Optional.",
                    ),
                ),
            ],
            &["file_path", "branch"],
        ),
        GitLabToolKind::SetActiveBranch => object(
            "SetActiveBranchModel",
            &[(
                "branch_name",
                text("Branch Name", "The name of the branch, e.g. `my_branch`."),
            )],
            &["branch_name"],
        ),
        GitLabToolKind::GetPrChanges => object(
            "GetPRChangesModel",
            &[(
                "pr_number",
                integer("Pr Number", "GitLab Merge Request (Pull Request) number", 1),
            )],
            &["pr_number"],
        ),
        GitLabToolKind::CreatePrChangeComment => object(
            "CreatePRChangeCommentModel",
            &[
                (
                    "pr_number",
                    integer("Pr Number", "GitLab Merge Request (Pull Request) number", 1),
                ),
                (
                    "file_path",
                    text(
                        "File Path",
                        "File path of the changed file as shown in the diff",
                    ),
                ),
                (
                    "line_number",
                    integer(
                        "Line Number",
                        "Line index (0-based) from the diff output. Use get_pr_changes first to see the diff and identify the correct line index to comment on.",
                        0,
                    ),
                ),
                (
                    "comment",
                    text("Comment", "Comment content to add to the specific line"),
                ),
            ],
            &["pr_number", "file_path", "line_number", "comment"],
        ),
        GitLabToolKind::GetCommits => object(
            "GetCommitsModel",
            &[
                ("sha", nullable_text("Sha", "Commit SHA or ref name")),
                ("path", nullable_text("Path", "File path")),
                ("since", nullable_text("Since", "Start date")),
                ("until", nullable_text("Until", "End date")),
                ("author", nullable_text("Author", "Author name")),
            ],
            &[],
        ),
        GitLabToolKind::ReadMultipleFiles => object(
            "ReadMultipleFilesInput",
            &[
                (
                    "file_paths",
                    json!({"title":"File Paths","type":"array","minItems":1,"maxItems":MAX_BATCH_FILES,"items":{"type":"string"},"description":"List of file paths to read"}),
                ),
                (
                    "branch",
                    nullable_text("Branch", "Branch name. If None, uses active branch."),
                ),
                (
                    "offset",
                    nullable_integer("Offset", "Starting line number for all files (1-indexed)"),
                ),
                (
                    "limit",
                    nullable_integer("Limit", "Number of lines to read from offset for all files"),
                ),
            ],
            &["file_paths"],
        ),
        GitLabToolKind::GrepFile => object(
            "GrepFileInput",
            &[
                (
                    "file_path",
                    text(
                        "File Path",
                        "Path to the specific FILE to search within, e.g. 'src/main.py'. Must be a file path, not a directory.",
                    ),
                ),
                (
                    "pattern",
                    text(
                        "Pattern",
                        "Text or regex pattern to find in the file's content. Works like grep/ripgrep.",
                    ),
                ),
                (
                    "branch",
                    nullable_text(
                        "Branch",
                        "Git branch to search in. Default: current active branch.",
                    ),
                ),
                (
                    "is_regex",
                    json!({"title":"Is Regex","type":"boolean","default":true,"description":"Treat pattern as regular expression (default: True). Set False for exact literal matching."}),
                ),
                (
                    "context_lines",
                    json!({"title":"Context Lines","type":"integer","minimum":0,"maximum":MAX_CONTEXT_LINES,"default":2,"description":"Lines of context to show before/after each match (default: 2)."}),
                ),
            ],
            &["file_path", "pattern"],
        ),
    }
}

fn tree_schema(title: &str, noun: &str) -> Value {
    object(
        title,
        &[
            (
                "path",
                nullable_text(
                    "Path",
                    &format!(
                        "The path to list {noun} from. Defaults to the repository root if omitted."
                    ),
                ),
            ),
            (
                "recursive",
                json!({"title":"Recursive","type":"boolean","default":true,"description":format!("Whether to list {noun} recursively")}),
            ),
            (
                "branch",
                nullable_text(
                    "Branch",
                    &format!(
                        "The branch to list {noun} from. Defaults to the active branch if omitted."
                    ),
                ),
            ),
        ],
        &[],
    )
}

fn object(title: &str, properties: &[(&str, Value)], required: &[&str]) -> Value {
    json!({
        "title": title,
        "type": "object",
        "properties": properties
            .iter()
            .map(|(name, schema)| ((*name).to_owned(), schema.clone()))
            .collect::<Map<String, Value>>(),
        "required": required,
        "additionalProperties": false
    })
}

fn text(title: &str, description: &str) -> Value {
    json!({"title": title, "type": "string", "description": description})
}

fn nullable_text(title: &str, description: &str) -> Value {
    json!({"title": title, "type": ["string", "null"], "default": null, "description": description})
}

fn integer(title: &str, description: &str, minimum: u64) -> Value {
    json!({"title": title, "type": "integer", "minimum": minimum, "description": description})
}

fn nullable_integer(title: &str, description: &str) -> Value {
    json!({"title": title, "type": ["integer", "null"], "minimum": 1, "default": null, "description": description})
}

fn allow(arguments: &Map<String, Value>, allowed: &[&str]) -> Result<(), AdkError> {
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

fn required_string<'a>(arguments: &'a Map<String, Value>, name: &str) -> Result<&'a str, AdkError> {
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

fn optional_bool(arguments: &Map<String, Value>, name: &str) -> Result<Option<bool>, AdkError> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Bool(value)) => Ok(Some(*value)),
        Some(_) => Err(invalid_arguments()),
    }
}

/// An integer, or a decimal string as pydantic's lax mode accepts.
fn integer_value(value: &Value) -> Option<u64> {
    match value {
        Value::Number(number) => number.as_u64(),
        Value::String(text) => text.trim().parse::<u64>().ok(),
        _ => None,
    }
}

fn required_u64(arguments: &Map<String, Value>, name: &str) -> Result<u64, AdkError> {
    arguments
        .get(name)
        .and_then(integer_value)
        .filter(|value| *value > 0)
        .ok_or_else(invalid_arguments)
}

fn required_u64_allow_zero(arguments: &Map<String, Value>, name: &str) -> Result<u64, AdkError> {
    arguments
        .get(name)
        .and_then(integer_value)
        .ok_or_else(invalid_arguments)
}

fn positive_usize(value: &Value) -> Result<usize, AdkError> {
    integer_value(value)
        .and_then(|value| usize::try_from(value).ok())
        .filter(|value| *value > 0)
        .ok_or_else(invalid_arguments)
}

fn nonnegative_usize(value: &Value) -> Result<usize, AdkError> {
    integer_value(value)
        .and_then(|value| usize::try_from(value).ok())
        .ok_or_else(invalid_arguments)
}

fn optional_positive_usize(
    arguments: &Map<String, Value>,
    name: &str,
) -> Result<Option<usize>, AdkError> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(value) => positive_usize(value).map(Some),
    }
}

fn file_paths(arguments: &Map<String, Value>) -> Result<Vec<&str>, AdkError> {
    let values = arguments
        .get("file_paths")
        .and_then(Value::as_array)
        .filter(|values| !values.is_empty())
        .ok_or_else(invalid_arguments)?;
    if values.len() > MAX_BATCH_FILES {
        return Err(resource_exhausted_arguments());
    }
    values
        .iter()
        .map(|value| {
            value
                .as_str()
                .filter(|value| !value.trim().is_empty())
                .ok_or_else(invalid_arguments)
        })
        .collect()
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "gitlab.arguments.invalid",
        "the GitLab tool arguments are invalid",
    )
}

fn resource_exhausted_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "gitlab.arguments.resource_exhausted",
        "the GitLab tool arguments exceed the approved limit",
    )
}

#[cfg(test)]
pub(in crate::toolkits) fn test_catalog() -> Vec<(&'static str, &'static str)> {
    GitLabToolKind::ALL
        .iter()
        .map(|kind| (kind.name(), kind.group()))
        .collect()
}
