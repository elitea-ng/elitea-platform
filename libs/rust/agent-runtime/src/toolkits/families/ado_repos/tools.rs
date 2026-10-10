use std::sync::Arc;

use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::families::ado::client::AdoClientError;
use crate::toolkits::families::ado::toolset::{
    AdoToolExecutor, AdoToolKind, AdoToolsetError, MAX_IDENTIFIER_BYTES, MAX_TEXT_BYTES,
    build_toolset, invalid_arguments, optional_i64, optional_id, optional_str, reject_unknown_keys,
    required_id, required_id_text, required_str, schema,
};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{AdoReposClient, GetCommits, inline_comments};
use super::config::AdoReposToolkitConfig;

pub(crate) const TOOLKIT_TYPE: &str = "ado_repos";

const BRANCH_NAME: &str = "The name of the branch, e.g. `my_branch`.";
const UPDATE_QUERY: &str = "Updates a file using OLD/NEW markers. Each marker must be on its own line:\nOLD <<<<\nold content\n>>>> OLD\nNEW <<<<\nnew content\n>>>> NEW\nSeveral OLD/NEW pairs may follow each other; each OLD block must match exactly one region of the file.";

/// Every non-index `ado_repos` tool.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum AdoReposToolKind {
    ListBranchesInRepo,
    SetActiveBranch,
    ListFiles,
    ListOpenPullRequests,
    GetPullRequest,
    ListPullRequestFiles,
    CreateBranch,
    ReadFile,
    CreateFile,
    UpdateFile,
    DeleteFile,
    GetWorkItems,
    CommentOnPullRequest,
    CreatePullRequest,
    GetCommits,
}

impl AdoReposToolKind {
    pub(crate) const ALL: [Self; 15] = [
        Self::ListBranchesInRepo,
        Self::SetActiveBranch,
        Self::ListFiles,
        Self::ListOpenPullRequests,
        Self::GetPullRequest,
        Self::ListPullRequestFiles,
        Self::CreateBranch,
        Self::ReadFile,
        Self::CreateFile,
        Self::UpdateFile,
        Self::DeleteFile,
        Self::GetWorkItems,
        Self::CommentOnPullRequest,
        Self::CreatePullRequest,
        Self::GetCommits,
    ];
}

impl AdoToolKind for AdoReposToolKind {
    fn name(self) -> &'static str {
        match self {
            Self::ListBranchesInRepo => "list_branches_in_repo",
            Self::SetActiveBranch => "set_active_branch",
            Self::ListFiles => "list_files",
            Self::ListOpenPullRequests => "list_open_pull_requests",
            Self::GetPullRequest => "get_pull_request",
            Self::ListPullRequestFiles => "list_pull_request_files",
            Self::CreateBranch => "create_branch",
            Self::ReadFile => "read_file",
            Self::CreateFile => "create_file",
            Self::UpdateFile => "update_file",
            Self::DeleteFile => "delete_file",
            Self::GetWorkItems => "get_work_items",
            Self::CommentOnPullRequest => "comment_on_pull_request",
            Self::CreatePullRequest => "create_pull_request",
            Self::GetCommits => "get_commits",
        }
    }

    fn description(self) -> &'static str {
        match self {
            Self::ListBranchesInRepo => {
                "Fetches a list of all branches in the repository. Returns a plaintext report containing the names of the branches."
            }
            Self::SetActiveBranch => {
                "Equivalent to `git checkout branch_name` for this Agent. Returns an error text if the branch doesn't exist."
            }
            Self::ListFiles => {
                "Recursively fetches files from a directory in the repo on branch_name (default: the active branch, which it then becomes). Returns the list of file paths."
            }
            Self::ListOpenPullRequests => {
                "Fetches all open pull requests from the Azure DevOps repository with their title, description, id, commits, comments and branches."
            }
            Self::GetPullRequest => {
                "Fetches particular pull request from the Azure DevOps repository by id, with its commits and comments."
            }
            Self::ListPullRequestFiles => {
                "Fetches the files and their diffs included in a pull request: a JSON list of {path, diff} where edited files carry a unified diff."
            }
            Self::CreateBranch => {
                "Create a new branch in Azure DevOps from the active branch, and set it as the active bot branch. Equivalent to `git switch -c branch_name`."
            }
            Self::ReadFile => {
                "Read a file from the given branch in Azure DevOps. offset (1-indexed) and limit read a line range; a file over the read limit returns content_too_large guidance with its line count."
            }
            Self::CreateFile => {
                "Creates a new file on the Azure DevOps repo on branch_name (default: the base branch, which is protected). Fails if the file already exists."
            }
            Self::UpdateFile => {
                "Edit file by path using OLD/NEW markers for precise replacements. Only works with text files (markdown, txt, csv, json, xml, html, yaml, code files). The base branch is protected."
            }
            Self::DeleteFile => "Deletes a file from the repository in Azure DevOps.",
            Self::GetWorkItems => {
                "Fetches the ids of the first ten work items linked to a pull request."
            }
            Self::CommentOnPullRequest => {
                "Adds a comment to a pull request in Azure DevOps. Supports both general pull request comments (comment_query \"<PR id>\\n\\n<comment>\") and inline comments (pull_request_id plus inline_comments with file_path, comment_text and left_line/right_line or left_range/right_range)."
            }
            Self::CreatePullRequest => {
                "Creates a pull request in Azure DevOps from source_branch to target_branch."
            }
            Self::GetCommits => {
                "Retrieves a list of commits from the repository, optionally from a commit SHA, for a path, between ISO 8601 dates, or by author."
            }
        }
    }

    #[allow(clippy::too_many_lines)] // One schema per SDK tool keeps the catalogue auditable.
    fn schema(self) -> Value {
        match self {
            Self::ListBranchesInRepo | Self::ListOpenPullRequests => schema::object(&[], &[]),
            Self::SetActiveBranch | Self::CreateBranch => schema::object(
                &[("branch_name", schema::string(BRANCH_NAME))],
                &["branch_name"],
            ),
            Self::ListFiles => schema::object(
                &[
                    (
                        "directory_path",
                        schema::optional_string(
                            "The path of the directory, e.g. `some_dir/inner_dir`. Only input a string, do not include the parameter name.",
                            Some(""),
                        ),
                    ),
                    (
                        "branch_name",
                        schema::optional_string(
                            "Repository branch. If None then active branch will be selected.",
                            None,
                        ),
                    ),
                ],
                &[],
            ),
            Self::GetPullRequest | Self::ListPullRequestFiles => schema::object(
                &[(
                    "pull_request_id",
                    schema::string("The PR number as a string, e.g. `12`"),
                )],
                &["pull_request_id"],
            ),
            Self::ReadFile => schema::object(
                &[
                    (
                        "file_path",
                        schema::string(
                            "The full file path of the file you would like to read where the path must NOT start with a slash, e.g. `some_dir/my_file.py`.",
                        ),
                    ),
                    (
                        "branch",
                        schema::string("Branch to be used for read file operation."),
                    ),
                    (
                        "offset",
                        json!({
                            "type":["integer","null"],"minimum":1,"default":null,
                            "description":"Starting line number (1-indexed, inclusive). When provided, reading starts from this line. Use together with 'limit' to read a specific range."
                        }),
                    ),
                    (
                        "limit",
                        json!({
                            "type":["integer","null"],"minimum":1,"default":null,
                            "description":"Maximum number of lines to return from the offset. If not provided, returns all lines from offset to end of file."
                        }),
                    ),
                ],
                &["file_path", "branch"],
            ),
            Self::CreateFile => schema::object(
                &[
                    ("branch_name", schema::optional_string(BRANCH_NAME, None)),
                    ("file_path", schema::string("Path of a file to be created.")),
                    (
                        "file_contents",
                        schema::string("Content of a file to be put into chat."),
                    ),
                ],
                &["file_path", "file_contents"],
            ),
            Self::UpdateFile => schema::object(
                &[
                    ("branch_name", schema::string(BRANCH_NAME)),
                    ("file_path", schema::string("Path of a file to be updated.")),
                    ("update_query", schema::string(UPDATE_QUERY)),
                ],
                &["branch_name", "file_path", "update_query"],
            ),
            Self::DeleteFile => schema::object(
                &[
                    ("branch_name", schema::string(BRANCH_NAME)),
                    (
                        "file_path",
                        schema::string(
                            "The full file path of the file you would like to delete where the path must NOT start with a slash, e.g. `some_dir/my_file.py`. Only input a string, not the param name.",
                        ),
                    ),
                ],
                &["branch_name", "file_path"],
            ),
            Self::GetWorkItems => schema::object(
                &[(
                    "pull_request_id",
                    schema::integer("The PR number as an integer, e.g. `12`"),
                )],
                &["pull_request_id"],
            ),
            Self::CommentOnPullRequest => schema::object(
                &[
                    (
                        "comment_query",
                        schema::optional_string(
                            "Follow the required formatting. Example: '1\n\nThis is a test comment' (PR number and comment)",
                            None,
                        ),
                    ),
                    (
                        "pull_request_id",
                        schema::optional_integer("ID of pull request as integer."),
                    ),
                    (
                        "inline_comments",
                        json!({
                            "type":["array","null"],
                            "items":{"type":"object"},
                            "default":null,
                            "description":"List of comments, where each comment is a dictionary specifying details about the comment, e.g. [{'file_path': 'src/main.py', 'comment_text': 'Logic needs improvement', 'right_line': 20}]. left_range/right_range are [start, end] line pairs."
                        }),
                    ),
                ],
                &[],
            ),
            Self::CreatePullRequest => schema::object(
                &[
                    (
                        "pull_request_title",
                        schema::string("Title of the pull request"),
                    ),
                    (
                        "pull_request_body",
                        schema::string("Body of the pull request"),
                    ),
                    (
                        "target_branch",
                        schema::string("The name of the target branch, e.g. `my_branch`."),
                    ),
                    (
                        "source_branch",
                        schema::string("The name of the source branch, e.g. `feature_branch`."),
                    ),
                ],
                &[
                    "pull_request_title",
                    "pull_request_body",
                    "target_branch",
                    "source_branch",
                ],
            ),
            Self::GetCommits => schema::object(
                &[
                    (
                        "sha",
                        schema::optional_string(
                            "The commit SHA to start listing commits from. If not provided, the default branch is used.",
                            None,
                        ),
                    ),
                    (
                        "path",
                        schema::optional_string(
                            "The file path to filter commits by. Only commits affecting this path will be returned.",
                            None,
                        ),
                    ),
                    (
                        "since",
                        schema::optional_string(
                            "Only commits after this date will be returned. Use ISO 8601 format (e.g., '2023-01-01T00:00:00Z').",
                            None,
                        ),
                    ),
                    (
                        "until",
                        schema::optional_string(
                            "Only commits before this date will be returned. Use ISO 8601 format (e.g., '2023-12-31T23:59:59Z').",
                            None,
                        ),
                    ),
                    (
                        "author",
                        schema::optional_string(
                            "The author of the commits. Can be a username (string)",
                            None,
                        ),
                    ),
                ],
                &[],
            ),
        }
    }

    fn is_read_only(self) -> bool {
        matches!(
            self,
            Self::ListBranchesInRepo
                | Self::ListFiles
                | Self::ListOpenPullRequests
                | Self::GetPullRequest
                | Self::ListPullRequestFiles
                | Self::ReadFile
                | Self::GetWorkItems
                | Self::GetCommits
        )
    }
}

/// Build the served `ado_repos` tools.
pub(crate) fn build_ado_repos_toolset(
    toolkit_name: &str,
    config: AdoReposToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, AdoToolsetError> {
    let selected = config.selected_tools().to_vec();
    let (connection, repository) = config.into_parts();
    let client = Arc::new(AdoReposClient::new(connection, repository)?);
    build_with_client(toolkit_name, &selected, policy, client)
}

pub(in crate::toolkits) fn build_with_client(
    toolkit_name: &str,
    selected: &[Box<str>],
    policy: &Arc<ToolAdmissionPolicy>,
    client: Arc<AdoReposClient>,
) -> Result<BasicToolset, AdoToolsetError> {
    let executor: Arc<dyn AdoToolExecutor<AdoReposToolKind>> = client;
    build_toolset(
        toolkit_name,
        TOOLKIT_TYPE,
        &AdoReposToolKind::ALL,
        selected,
        policy,
        &executor,
        "",
    )
}

#[async_trait]
impl AdoToolExecutor<AdoReposToolKind> for AdoReposClient {
    async fn execute(
        &self,
        kind: AdoReposToolKind,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        dispatch(self, kind, arguments)
            .await?
            .map_err(AdoClientError::into_adk)
    }
}

/// A repository path or branch: non-empty, single-line.
fn name_argument<'a>(arguments: &'a Map<String, Value>, key: &str) -> adk_core::Result<&'a str> {
    let value = required_str(arguments, key, MAX_IDENTIFIER_BYTES)?;
    if value.trim().is_empty() || value.contains(['\n', '\r']) {
        return Err(invalid_arguments());
    }
    Ok(value)
}

/// A pull request id typed `str` in the SDK; the route needs an integer.
fn pull_request_text(arguments: &Map<String, Value>) -> adk_core::Result<String> {
    let value = required_id_text(arguments, "pull_request_id")?;
    Ok(value.trim().to_owned())
}

#[allow(clippy::too_many_lines)] // One source-ordered argument ledger per tool.
async fn dispatch(
    client: &AdoReposClient,
    kind: AdoReposToolKind,
    arguments: &Map<String, Value>,
) -> adk_core::Result<Result<Value, AdoClientError>> {
    Ok(match kind {
        AdoReposToolKind::ListBranchesInRepo => {
            reject_unknown_keys(arguments, &[])?;
            client.list_branches_in_repo().await
        }
        AdoReposToolKind::SetActiveBranch => {
            reject_unknown_keys(arguments, &["branch_name"])?;
            client
                .set_active_branch(name_argument(arguments, "branch_name")?)
                .await
        }
        AdoReposToolKind::ListFiles => {
            reject_unknown_keys(arguments, &["directory_path", "branch_name"])?;
            client
                .list_files(
                    optional_str(arguments, "directory_path", MAX_IDENTIFIER_BYTES)?.unwrap_or(""),
                    optional_str(arguments, "branch_name", MAX_IDENTIFIER_BYTES)?,
                )
                .await
        }
        AdoReposToolKind::ListOpenPullRequests => {
            reject_unknown_keys(arguments, &[])?;
            client.list_open_pull_requests().await
        }
        AdoReposToolKind::GetPullRequest => {
            reject_unknown_keys(arguments, &["pull_request_id"])?;
            let id = pull_request_text(arguments)?;
            if id.parse::<u64>().is_err() {
                return Ok(Ok(Value::String(format!(
                    "Failed to find pull request with '{id}' ID."
                ))));
            }
            client.get_pull_request(&id).await
        }
        AdoReposToolKind::ListPullRequestFiles => {
            reject_unknown_keys(arguments, &["pull_request_id"])?;
            client
                .list_pull_request_files(&pull_request_text(arguments)?)
                .await
        }
        AdoReposToolKind::CreateBranch => {
            reject_unknown_keys(arguments, &["branch_name"])?;
            client
                .create_branch(name_argument(arguments, "branch_name")?)
                .await
        }
        AdoReposToolKind::ReadFile => {
            reject_unknown_keys(arguments, &["file_path", "branch", "offset", "limit"])?;
            let offset = optional_i64(arguments, "offset")?;
            let limit = optional_i64(arguments, "limit")?;
            if offset.is_some_and(|offset| offset < 1) || limit.is_some_and(|limit| limit < 1) {
                return Err(invalid_arguments());
            }
            client
                .read_file(
                    name_argument(arguments, "file_path")?,
                    required_str(arguments, "branch", MAX_IDENTIFIER_BYTES)?,
                    offset,
                    limit,
                )
                .await
        }
        AdoReposToolKind::CreateFile => {
            reject_unknown_keys(arguments, &["branch_name", "file_path", "file_contents"])?;
            client
                .create_file(
                    name_argument(arguments, "file_path")?,
                    required_str(arguments, "file_contents", MAX_TEXT_BYTES)?,
                    optional_str(arguments, "branch_name", MAX_IDENTIFIER_BYTES)?,
                )
                .await
        }
        AdoReposToolKind::UpdateFile => {
            reject_unknown_keys(arguments, &["branch_name", "file_path", "update_query"])?;
            client
                .update_file(
                    required_str(arguments, "branch_name", MAX_IDENTIFIER_BYTES)?,
                    name_argument(arguments, "file_path")?,
                    required_str(arguments, "update_query", MAX_TEXT_BYTES)?,
                )
                .await
        }
        AdoReposToolKind::DeleteFile => {
            reject_unknown_keys(arguments, &["branch_name", "file_path"])?;
            client
                .delete_file(
                    name_argument(arguments, "branch_name")?,
                    name_argument(arguments, "file_path")?,
                )
                .await
        }
        AdoReposToolKind::GetWorkItems => {
            reject_unknown_keys(arguments, &["pull_request_id"])?;
            client
                .get_work_items(required_id(arguments, "pull_request_id")?)
                .await
        }
        AdoReposToolKind::CommentOnPullRequest => {
            reject_unknown_keys(
                arguments,
                &["comment_query", "pull_request_id", "inline_comments"],
            )?;
            let inline = match arguments.get("inline_comments") {
                None => Vec::new(),
                Some(Value::Array(values)) => values.clone(),
                Some(_) => return Err(invalid_arguments()),
            };
            if !inline.is_empty() {
                let Some(pull_request_id) = optional_id(arguments, "pull_request_id")? else {
                    return Ok(Ok(Value::String(
                        "Invalid input parameters: `pull_request_id` must be provided when using `comments` for inline commenting.".to_owned(),
                    )));
                };
                match inline_comments(&inline) {
                    Ok(comments) => client.comment_inline(pull_request_id, &comments).await,
                    Err(message) => Ok(Value::String(message)),
                }
            } else if let Some(query) = optional_str(arguments, "comment_query", MAX_TEXT_BYTES)?
                .filter(|query| !query.is_empty())
            {
                client.comment_query(query).await
            } else {
                Ok(Value::String(
                    "Invalid input parameters: Either `comment_query` or `comments` must be provided."
                        .to_owned(),
                ))
            }
        }
        AdoReposToolKind::CreatePullRequest => {
            reject_unknown_keys(
                arguments,
                &[
                    "pull_request_title",
                    "pull_request_body",
                    "target_branch",
                    "source_branch",
                ],
            )?;
            client
                .create_pull_request(
                    required_str(arguments, "pull_request_title", MAX_TEXT_BYTES)?,
                    required_str(arguments, "pull_request_body", MAX_TEXT_BYTES)?,
                    name_argument(arguments, "target_branch")?,
                    name_argument(arguments, "source_branch")?,
                )
                .await
        }
        AdoReposToolKind::GetCommits => {
            reject_unknown_keys(arguments, &["sha", "path", "since", "until", "author"])?;
            client
                .get_commits(GetCommits {
                    sha: optional_str(arguments, "sha", MAX_IDENTIFIER_BYTES)?
                        .filter(|value| !value.is_empty()),
                    path: optional_str(arguments, "path", MAX_IDENTIFIER_BYTES)?
                        .filter(|value| !value.is_empty()),
                    since: optional_str(arguments, "since", MAX_IDENTIFIER_BYTES)?
                        .filter(|value| !value.is_empty()),
                    until: optional_str(arguments, "until", MAX_IDENTIFIER_BYTES)?
                        .filter(|value| !value.is_empty()),
                    author: optional_str(arguments, "author", MAX_IDENTIFIER_BYTES)?,
                })
                .await
        }
    })
}
