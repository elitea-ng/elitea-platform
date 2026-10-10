use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::families::vcs_text::{MAX_BATCH_FILES, MAX_CONTEXT_LINES, MAX_PATTERN_BYTES};
use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{
    BitbucketApi, BitbucketClient, BitbucketClientError, BitbucketOperation, CREATE_PR_DATA,
};
use super::config::{BitbucketConfigError, BitbucketConfigErrorCode, BitbucketToolkitConfig};

const MAX_ARGUMENT_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 1_000;

/// The SDK `bitbucket` index tools, which need the indexing runtime Rust
/// does not have yet; a selection naming one omits it.
const UNSERVED_INDEX_TOOLS: [&str; 6] = [
    "index_data",
    "list_indexes",
    "remove_index",
    "search_index",
    "stepback_search_index",
    "stepback_summary_index",
];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum BitbucketToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

pub(crate) struct BitbucketToolsetError {
    code: BitbucketToolsetErrorCode,
}

impl BitbucketToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> BitbucketToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for BitbucketToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("BitbucketToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for BitbucketToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            BitbucketToolsetErrorCode::InvalidConfiguration => {
                "the Bitbucket toolkit configuration is invalid"
            }
            BitbucketToolsetErrorCode::ResourceExhausted => {
                "the Bitbucket toolkit configuration exceeds its approved limit"
            }
            BitbucketToolsetErrorCode::UnsupportedSelection => {
                "the selected Bitbucket tool profile is not supported"
            }
            BitbucketToolsetErrorCode::Client => "the Bitbucket client could not be created",
            BitbucketToolsetErrorCode::InvalidDefinition => {
                "the Bitbucket ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for BitbucketToolsetError {}

impl From<BitbucketConfigError> for BitbucketToolsetError {
    fn from(source: BitbucketConfigError) -> Self {
        Self {
            code: match source.code() {
                BitbucketConfigErrorCode::InvalidConfiguration => {
                    BitbucketToolsetErrorCode::InvalidConfiguration
                }
                BitbucketConfigErrorCode::ResourceExhausted => {
                    BitbucketToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<BitbucketClientError> for BitbucketToolsetError {
    fn from(_: BitbucketClientError) -> Self {
        Self {
            code: BitbucketToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for BitbucketToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: BitbucketToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the SDK `bitbucket` toolkit's non-index tools for one repository.
pub(crate) fn build_bitbucket_toolset(
    toolkit_name: &str,
    config: BitbucketToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, BitbucketToolsetError> {
    let selected = validate_selection(config.selected_tools())?;
    let omitted_count = config.selected_tools().len() - selected.len();
    if omitted_count > 0 {
        tracing::warn!(
            event = "agent_toolkit_tools_skipped",
            reason_code = "unsupported_tool_selection",
            toolkit_type = "bitbucket",
            toolkit_name,
            selected_count = config.selected_tools().len(),
            materialized_count = selected.len(),
            omitted_count,
            "Bitbucket index tools were omitted because this runtime has no indexing"
        );
    }
    let client: Arc<dyn BitbucketApi> = Arc::new(BitbucketClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

/// The selected names this family serves: an unknown name fails closed, a
/// known SDK index tool is dropped, and an index-only selection is refused
/// because its empty remainder would otherwise mean "all tools".
fn validate_selection(selected: &[Box<str>]) -> Result<Vec<String>, BitbucketToolsetError> {
    let mut served = Vec::with_capacity(selected.len());
    for name in selected {
        if BitbucketToolKind::ALL
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

const fn unsupported_selection() -> BitbucketToolsetError {
    BitbucketToolsetError {
        code: BitbucketToolsetErrorCode::UnsupportedSelection,
    }
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn BitbucketApi>,
) -> Result<BasicToolset, BitbucketToolsetError> {
    let include_all = selected.is_empty();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(BitbucketToolKind::ALL.len());
    for kind in BitbucketToolKind::ALL {
        if include_all || selected.iter().any(|name| name == kind.name()) {
            tools.push(Arc::new(BitbucketTool::new(
                kind,
                toolkit_name,
                Arc::clone(client),
            )));
        }
    }
    admit_materialized_toolset(toolkit_name, "bitbucket", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    selected: &[Box<str>],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn BitbucketApi>,
) -> Result<BasicToolset, BitbucketToolsetError> {
    let selected = validate_selection(selected)?;
    build_with_api(toolkit_name, &selected, policy, client)
}

#[derive(Clone, Copy)]
enum BitbucketToolKind {
    CreateBranch,
    DeleteBranch,
    ListBranches,
    ListFiles,
    CreatePullRequest,
    CreateFile,
    ReadFile,
    UpdateFile,
    SetActiveBranch,
    GetPullRequestCommits,
    GetPullRequest,
    GetPullRequestChanges,
    AddPullRequestComment,
    ClosePullRequest,
    ReadMultipleFiles,
    GrepFile,
}

impl BitbucketToolKind {
    /// The SDK's `get_available_tools` order, then the file-operation tools
    /// its decorator appends.
    const ALL: [Self; 16] = [
        Self::CreateBranch,
        Self::DeleteBranch,
        Self::ListBranches,
        Self::ListFiles,
        Self::CreatePullRequest,
        Self::CreateFile,
        Self::ReadFile,
        Self::UpdateFile,
        Self::SetActiveBranch,
        Self::GetPullRequestCommits,
        Self::GetPullRequest,
        Self::GetPullRequestChanges,
        Self::AddPullRequestComment,
        Self::ClosePullRequest,
        Self::ReadMultipleFiles,
        Self::GrepFile,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::CreateBranch => "create_branch",
            Self::DeleteBranch => "delete_branch",
            Self::ListBranches => "list_branches_in_repo",
            Self::ListFiles => "list_files",
            Self::CreatePullRequest => "create_pull_request",
            Self::CreateFile => "create_file",
            Self::ReadFile => "read_file",
            Self::UpdateFile => "update_file",
            Self::SetActiveBranch => "set_active_branch",
            Self::GetPullRequestCommits => "get_pull_requests_commits",
            Self::GetPullRequest => "get_pull_request",
            Self::GetPullRequestChanges => "get_pull_requests_changes",
            Self::AddPullRequestComment => "add_pull_request_comment",
            Self::ClosePullRequest => "close_pull_request",
            Self::ReadMultipleFiles => "read_multiple_files",
            Self::GrepFile => "grep_file",
        }
    }

    const fn group(self) -> &'static str {
        match self {
            Self::ListBranches
            | Self::ListFiles
            | Self::ReadFile
            | Self::GetPullRequestCommits
            | Self::GetPullRequest
            | Self::GetPullRequestChanges
            | Self::ReadMultipleFiles
            | Self::GrepFile => "read",
            Self::DeleteBranch => "delete",
            _ => "write",
        }
    }

    const fn description(self) -> &'static str {
        match self {
            Self::CreateBranch => {
                "Create branch_name from the active branch (initially the toolkit's base branch) and make it active. If it already exists it is only made active. This is a remote write."
            }
            Self::DeleteBranch => {
                "Delete an existing branch. main and master are refused, and so is the active branch (switch first). This is a remote destructive effect."
            }
            Self::ListBranches => {
                "List branch names as `Found branches: a, b`, at most limit (default 20), optionally filtered by a shell-style branch_wildcard such as release/*. Reads at most 10 pages or 1000 branches."
            }
            Self::ListFiles => {
                "List repository file paths under path (default the root) on branch (default the active branch); recursive defaults to true. Reads at most 10 pages or 1000 files."
            }
            Self::CreatePullRequest => {
                "Create a pull request from pr_json_data, a JSON object string in the hosting's own shape (Server: title, description, fromRef.id, toRef.id; Cloud: title, source.branch.name, destination.branch.name). This is a remote write."
            }
            Self::CreateFile => {
                "Create file_path with file_contents on branch. An existing file is not overwritten; use update_file. This is a remote write."
            }
            Self::ReadFile => {
                "Read a text file from branch (blank means the active branch). Optional start_line and end_line are 1-indexed and inclusive. Results over 200000 characters return content_too_large guidance with the valid line range."
            }
            Self::UpdateFile => {
                "Edit file_path on branch with one or more OLD <<<< … >>>> OLD / NEW <<<< … >>>> NEW blocks in update_query, each marker on its own line; each OLD block must match exactly once. This is a remote write."
            }
            Self::SetActiveBranch => {
                "Make an existing branch the active branch used by create_branch and by reads and listings when branch is blank. This does not modify Bitbucket."
            }
            Self::GetPullRequestCommits => {
                "Get the commits of pull request pr_id as provider JSON objects."
            }
            Self::GetPullRequest => "Get pull request pr_id as the provider's JSON object.",
            Self::GetPullRequestChanges => {
                "Get the changes of pull request pr_id: Server returns its change objects; Cloud returns {raw_response: unified diff}."
            }
            Self::AddPullRequestComment => {
                "Add a comment to pull request pr_id. On Bitbucket Cloud, inline {from, to, path} anchors it to a line. This is a remote write."
            }
            Self::ClosePullRequest => {
                "Decline (close without merging) pull request pr_id, optionally leaving message as a comment. This is a remote write."
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

struct BitbucketTool {
    kind: BitbucketToolKind,
    client: Arc<dyn BitbucketApi>,
    description: Box<str>,
}

impl BitbucketTool {
    fn new(kind: BitbucketToolKind, toolkit_name: &str, client: Arc<dyn BitbucketApi>) -> Self {
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
impl Tool for BitbucketTool {
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
        // Branch defaults read the shared active branch.
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
            BitbucketToolKind::CreateBranch => {
                allow(arguments, &["branch_name"])?;
                BitbucketOperation::CreateBranch {
                    branch_name: required_text(arguments, "branch_name")?,
                }
            }
            BitbucketToolKind::DeleteBranch => {
                allow(arguments, &["branch_name"])?;
                BitbucketOperation::DeleteBranch {
                    branch_name: required_text(arguments, "branch_name")?,
                }
            }
            BitbucketToolKind::ListBranches => {
                allow(arguments, &["limit", "branch_wildcard"])?;
                BitbucketOperation::ListBranches {
                    // The policy wrapper drops explicit nulls as BaseAction
                    // does, so the SDK default 20 also stands for `null`.
                    limit: match arguments.get("limit") {
                        None | Some(Value::Null) => 20,
                        Some(value) => positive_usize(value)?,
                    },
                    branch_wildcard: optional_text(arguments, "branch_wildcard")?,
                }
            }
            BitbucketToolKind::ListFiles => {
                allow(arguments, &["path", "recursive", "branch"])?;
                BitbucketOperation::ListFiles {
                    path: optional_text(arguments, "path")?,
                    recursive: optional_bool(arguments, "recursive")?.unwrap_or(true),
                    branch: optional_text(arguments, "branch")?,
                }
            }
            BitbucketToolKind::CreatePullRequest => {
                allow(arguments, &["pr_json_data"])?;
                BitbucketOperation::CreatePullRequest {
                    pr_json_data: required_text(arguments, "pr_json_data")?,
                }
            }
            BitbucketToolKind::CreateFile => {
                allow(arguments, &["file_path", "file_contents", "branch"])?;
                BitbucketOperation::CreateFile {
                    file_path: required_text(arguments, "file_path")?,
                    contents: required_string(arguments, "file_contents")?,
                    branch: optional_text(arguments, "branch")?,
                }
            }
            BitbucketToolKind::ReadFile => {
                allow(
                    arguments,
                    &["file_path", "branch", "start_line", "end_line"],
                )?;
                BitbucketOperation::ReadFile {
                    file_path: required_text(arguments, "file_path")?,
                    branch: optional_text(arguments, "branch")?,
                    start_line: optional_positive_usize(arguments, "start_line")?,
                    end_line: optional_positive_usize(arguments, "end_line")?,
                }
            }
            BitbucketToolKind::UpdateFile => {
                allow(arguments, &["file_path", "update_query", "branch"])?;
                BitbucketOperation::UpdateFile {
                    file_path: required_text(arguments, "file_path")?,
                    update_query: required_text(arguments, "update_query")?,
                    branch: optional_text(arguments, "branch")?,
                }
            }
            BitbucketToolKind::SetActiveBranch => {
                allow(arguments, &["branch_name"])?;
                BitbucketOperation::SetActiveBranch {
                    branch_name: required_text(arguments, "branch_name")?,
                }
            }
            BitbucketToolKind::GetPullRequestCommits => {
                allow(arguments, &["pr_id"])?;
                BitbucketOperation::GetPullRequestCommits {
                    pr_id: pr_id(arguments)?,
                }
            }
            BitbucketToolKind::GetPullRequest => {
                allow(arguments, &["pr_id"])?;
                BitbucketOperation::GetPullRequest {
                    pr_id: pr_id(arguments)?,
                }
            }
            BitbucketToolKind::GetPullRequestChanges => {
                allow(arguments, &["pr_id"])?;
                BitbucketOperation::GetPullRequestChanges {
                    pr_id: pr_id(arguments)?,
                }
            }
            BitbucketToolKind::AddPullRequestComment => {
                allow(arguments, &["pr_id", "content", "inline"])?;
                BitbucketOperation::AddPullRequestComment {
                    pr_id: pr_id(arguments)?,
                    content: required_text(arguments, "content")?,
                    inline: match arguments.get("inline") {
                        None | Some(Value::Null) => None,
                        Some(Value::Object(inline)) => Some(inline),
                        Some(_) => return Err(invalid_arguments()),
                    },
                }
            }
            BitbucketToolKind::ClosePullRequest => {
                allow(arguments, &["pr_id", "message"])?;
                BitbucketOperation::ClosePullRequest {
                    pr_id: pr_id(arguments)?,
                    message: optional_text(arguments, "message")?,
                }
            }
            BitbucketToolKind::ReadMultipleFiles => {
                allow(arguments, &["file_paths", "branch", "offset", "limit"])?;
                BitbucketOperation::ReadMultipleFiles {
                    file_paths: file_paths(arguments)?,
                    branch: optional_text(arguments, "branch")?,
                    offset: optional_positive_usize(arguments, "offset")?,
                    limit: optional_positive_usize(arguments, "limit")?,
                }
            }
            BitbucketToolKind::GrepFile => {
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
                BitbucketOperation::GrepFile {
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
            .map_err(BitbucketClientError::into_adk)
    }
}

#[allow(clippy::too_many_lines)] // One schema per SDK args model, in source order.
fn schema_for(kind: BitbucketToolKind) -> Value {
    let pr_id = |purpose: &str| {
        (
            "pr_id",
            json!({"title":"Pr Id","type":"string","pattern":"^\\s*[1-9][0-9]*\\s*$","maxLength":20,"description":format!("The ID of the pull request {purpose}, for example `42`")}),
        )
    };
    match kind {
        BitbucketToolKind::CreateBranch => object(
            "CreateBranchModel",
            &[(
                "branch_name",
                text("Branch Name", "The name of the branch, e.g. `my_branch`."),
            )],
            &["branch_name"],
        ),
        BitbucketToolKind::DeleteBranch => object(
            "DeleteBranchModel",
            &[(
                "branch_name",
                text(
                    "Branch Name",
                    "The name of the branch to delete, e.g. `my_branch`. Cannot delete main or master branches.",
                ),
            )],
            &["branch_name"],
        ),
        BitbucketToolKind::ListBranches => object(
            "ListBranchesInRepoModel",
            &[
                (
                    "limit",
                    json!({"title":"Limit","type":["integer","null"],"minimum":1,"default":20,"description":"Maximum number of branches to return (default 20)."}),
                ),
                (
                    "branch_wildcard",
                    nullable_text(
                        "Branch Wildcard",
                        "Wildcard pattern to filter branches by name, e.g. `release/*`. If not provided, all branches are considered.",
                    ),
                ),
            ],
            &[],
        ),
        BitbucketToolKind::ListFiles => object(
            "ListFilesModel",
            &[
                (
                    "path",
                    nullable_text("Path", "The path to list files from (default the root)"),
                ),
                (
                    "recursive",
                    json!({"title":"Recursive","type":"boolean","default":true,"description":"Whether to list files recursively"}),
                ),
                (
                    "branch",
                    nullable_text(
                        "Branch",
                        "The branch to list files from (null means the active branch)",
                    ),
                ),
            ],
            &[],
        ),
        BitbucketToolKind::CreatePullRequest => object(
            "CreatePullRequestModel",
            &[("pr_json_data", text("Pr Json Data", CREATE_PR_DATA))],
            &["pr_json_data"],
        ),
        BitbucketToolKind::CreateFile => object(
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
        BitbucketToolKind::ReadFile => object(
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
        BitbucketToolKind::UpdateFile => object(
            "UpdateFileModel",
            &[
                ("file_path", text("File Path", "The path of the file")),
                (
                    "update_query",
                    json!({"title":"Update Query","type":"string","multiline":true,"description":"One or more edits, each marker on its own line:\nOLD <<<<\nold content\n>>>> OLD\nNEW <<<<\nnew content\n>>>> NEW\nLeading/trailing whitespace in content is stripped."}),
                ),
                ("branch", text("Branch", "The branch to update the file in")),
            ],
            &["file_path", "update_query", "branch"],
        ),
        BitbucketToolKind::SetActiveBranch => object(
            "SetActiveBranchModel",
            &[(
                "branch_name",
                text("Branch Name", "The name of the branch, e.g. `my_branch`."),
            )],
            &["branch_name"],
        ),
        BitbucketToolKind::GetPullRequestCommits => object(
            "GetPullRequestsCommitsModel",
            &[pr_id("to get commits from")],
            &["pr_id"],
        ),
        BitbucketToolKind::GetPullRequest => object(
            "GetPullRequestModel",
            &[pr_id("to get details from")],
            &["pr_id"],
        ),
        BitbucketToolKind::GetPullRequestChanges => object(
            "GetPullRequestsChangesModel",
            &[pr_id("to get changes from")],
            &["pr_id"],
        ),
        BitbucketToolKind::AddPullRequestComment => object(
            "AddPullRequestCommentModel",
            &[
                pr_id("to add a comment to"),
                ("content", text("Content", "The comment content")),
                (
                    "inline",
                    json!({"title":"Inline","type":["object","null"],"additionalProperties":true,"default":null,"description":"Bitbucket Cloud inline comment anchor, e.g. {'from': 57, 'to': 122, 'path': 'src/a.py'}"}),
                ),
            ],
            &["pr_id", "content"],
        ),
        BitbucketToolKind::ClosePullRequest => object(
            "ClosePullRequestModel",
            &[
                pr_id("to close"),
                (
                    "message",
                    nullable_text(
                        "Message",
                        "Optional message explaining why the pull request is being closed",
                    ),
                ),
            ],
            &["pr_id"],
        ),
        BitbucketToolKind::ReadMultipleFiles => object(
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
        BitbucketToolKind::GrepFile => object(
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

/// `pr_id` is a string in the SDK; Bitbucket ids are positive integers.
fn pr_id(arguments: &Map<String, Value>) -> Result<u64, AdkError> {
    arguments
        .get("pr_id")
        .and_then(integer_value)
        .filter(|value| *value > 0)
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
        "bitbucket.arguments.invalid",
        "the Bitbucket tool arguments are invalid",
    )
}

fn resource_exhausted_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "bitbucket.arguments.resource_exhausted",
        "the Bitbucket tool arguments exceed the approved limit",
    )
}

#[cfg(test)]
pub(in crate::toolkits) fn test_catalog() -> Vec<(&'static str, &'static str)> {
    BitbucketToolKind::ALL
        .iter()
        .map(|kind| (kind.name(), kind.group()))
        .collect()
}
