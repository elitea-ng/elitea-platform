//! The effectful GitHub tools (15 of the SDK's 44): branches, files, pull
//! requests, issues, comments, workflow dispatch and patches.
//!
//! Names, arguments, answer texts and refusal sentences are the SDK's
//! (`elitea_sdk/tools/github/github_client.py` of the pinned revision). What
//! the SDK reports as text stays text (`Branch 'x' not found in repository`,
//! the protected-branch refusal); a failure of GitHub itself is
//! returned as the tool's result with one sentence per cause (credentials,
//! permission, not found, conflict, validation, rate limit, unreachable,
//! unknown outcome), built in [`Fail::sentence`].
//!
//! Every tool here is effectful: `is_read_only` is false, so the direct path
//! refuses it and the operator's `sensitive_tools` list can require
//! confirmation by name, exactly as for the other repository families. A
//! write is sent once. If it was sent and GitHub's answer was lost or was a
//! server error, the outcome is *unknown* and the sentence says to check the
//! repository, because the change may have been made.
//!
//! Differences from the SDK, each deliberate:
//!
//! * nothing is overwritten that the call did not name. `update_file` and
//!   `delete_file` send the sha they read, so a file that changed meanwhile is
//!   a conflict and not a lost update; `create_file` never replaces;
//! * `create_branch` finds the first free `_vN` name from the matching refs
//!   instead of trying up to 1000 names one request at a time;
//! * `apply_git_patch` reads and checks every file first and writes nothing if
//!   any file does not match, then commits file by file and, if a write fails,
//!   reports what was applied and what was not attempted (see `patch.rs` for
//!   how the diff is read);
//! * `update_issue` is one request, where the SDK edits labels and assignees
//!   one by one before the edit.

use std::fmt::Write as _;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use elitea_connectors::github::client::MAX_RESPONSE_BYTES;
use elitea_connectors::transport::Method;
use serde_json::{Map, Value, json};

use crate::toolkits::families::gitlab_org::edit::{EditErrorCode, apply_update};
use crate::toolkits::families::office_host::{OfficeFailure, SharedOfficeToolHost};

use super::client::{GitHubClient, GitHubClientError, GitHubClientErrorCode, error};
use super::patch::{FileChange, Operation, apply_hunks, created_content, parse_patch};

const MAX_DESCRIPTION_BYTES: usize = 1_000;
const MAX_CONTENTS_RESPONSE_BYTES: usize = 8 * 1_024 * 1_024;
/// The most text one write sends (the read side's own file cap).
const MAX_WRITE_BYTES: usize = 1_024 * 1_024;
/// The most bytes copied from a platform object into the repository.
const MAX_COPY_BYTES: u64 = 8 * 1_024 * 1_024;
const MAX_PATCH_BYTES: usize = 1_024 * 1_024;
const MAX_PATCH_FILES: usize = 100;
const MAX_BRANCH_ATTEMPTS: usize = 20;

const SET_ACTIVE_BRANCH: &str = "set_active_branch";
const CREATE_BRANCH: &str = "create_branch";
const DELETE_BRANCH: &str = "delete_branch";
const CREATE_FILE: &str = "create_file";
const UPDATE_FILE: &str = "update_file";
const DELETE_FILE: &str = "delete_file";
const CREATE_PULL_REQUEST: &str = "create_pull_request";
const CREATE_ISSUE: &str = "create_issue";
const UPDATE_ISSUE: &str = "update_issue";
const COMMENT_ON_ISSUE: &str = "comment_on_issue";
const TRIGGER_WORKFLOW: &str = "trigger_workflow";
const APPLY_GIT_PATCH: &str = "apply_git_patch";
const APPLY_GIT_PATCH_FROM_FILE: &str = "apply_git_patch_from_file";
const CREATE_ISSUE_ON_PROJECT: &str = "create_issue_on_project";
const UPDATE_ISSUE_ON_PROJECT: &str = "update_issue_on_project";

/// The effectful GitHub tools, in the SDK's order.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum WriteKind {
    CommentOnIssue,
    CreatePullRequest,
    CreateFile,
    UpdateFile,
    DeleteFile,
    SetActiveBranch,
    CreateBranch,
    DeleteBranch,
    CreateIssue,
    UpdateIssue,
    ApplyGitPatch,
    ApplyGitPatchFromFile,
    TriggerWorkflow,
    CreateIssueOnProject,
    UpdateIssueOnProject,
}

impl WriteKind {
    pub(super) const ALL: [Self; 15] = [
        Self::CommentOnIssue,
        Self::CreatePullRequest,
        Self::CreateFile,
        Self::UpdateFile,
        Self::DeleteFile,
        Self::SetActiveBranch,
        Self::CreateBranch,
        Self::DeleteBranch,
        Self::CreateIssue,
        Self::UpdateIssue,
        Self::ApplyGitPatch,
        Self::ApplyGitPatchFromFile,
        Self::TriggerWorkflow,
        Self::CreateIssueOnProject,
        Self::UpdateIssueOnProject,
    ];

    pub(super) const fn name(self) -> &'static str {
        match self {
            Self::CommentOnIssue => COMMENT_ON_ISSUE,
            Self::CreatePullRequest => CREATE_PULL_REQUEST,
            Self::CreateFile => CREATE_FILE,
            Self::UpdateFile => UPDATE_FILE,
            Self::DeleteFile => DELETE_FILE,
            Self::SetActiveBranch => SET_ACTIVE_BRANCH,
            Self::CreateBranch => CREATE_BRANCH,
            Self::DeleteBranch => DELETE_BRANCH,
            Self::CreateIssue => CREATE_ISSUE,
            Self::UpdateIssue => UPDATE_ISSUE,
            Self::ApplyGitPatch => APPLY_GIT_PATCH,
            Self::ApplyGitPatchFromFile => APPLY_GIT_PATCH_FROM_FILE,
            Self::TriggerWorkflow => TRIGGER_WORKFLOW,
            Self::CreateIssueOnProject => CREATE_ISSUE_ON_PROJECT,
            Self::UpdateIssueOnProject => UPDATE_ISSUE_ON_PROJECT,
        }
    }

    pub(super) fn from_name(name: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|kind| kind.name() == name)
    }

    /// What the failure sentence says was being done.
    const fn action(self) -> &'static str {
        match self {
            Self::CommentOnIssue => "add the comment",
            Self::CreatePullRequest => "create the pull request",
            Self::CreateFile => "create the file",
            Self::UpdateFile => "update the file",
            Self::DeleteFile => "delete the file",
            Self::SetActiveBranch => "set the active branch",
            Self::CreateBranch => "create the branch",
            Self::DeleteBranch => "delete the branch",
            Self::CreateIssue => "create the issue",
            Self::UpdateIssue => "update the issue",
            Self::ApplyGitPatch | Self::ApplyGitPatchFromFile => "apply the patch",
            Self::TriggerWorkflow => "trigger the workflow",
            Self::CreateIssueOnProject => "create the issue on the project",
            Self::UpdateIssueOnProject => "update the issue on the project",
        }
    }

    const fn sentence(self) -> &'static str {
        match self {
            Self::CommentOnIssue => {
                "Add a comment to an issue or pull request. issue_number is the number as a string, for example `42`. Returns the comment URL. This is a remote write."
            }
            Self::CreatePullRequest => {
                "Create a pull request from head into base (default the toolkit's base branch) with title and body. Returns the PR number and URL. This is a remote write."
            }
            Self::CreateFile => {
                "Create a new file on branch from file_contents, or copy a platform artifact with filepath (/{bucket}/{filename}); give exactly one of the two. An existing file is never replaced (use update_file). The base branch is protected. This is a remote write."
            }
            Self::UpdateFile => {
                "Edit a file on branch. file_query's first non-empty line is the file path (no leading slash), followed by one or more OLD <<<< ... >>>> OLD / NEW <<<< ... >>>> NEW blocks, each marker on its own line; each OLD block must match exactly once. The base branch is protected. This is a remote write."
            }
            Self::DeleteFile => {
                "Delete file_path from branch. The base branch is protected. This is a remote destructive effect."
            }
            Self::SetActiveBranch => {
                "Set the active branch used as create_branch's source and by listings and reads when branch is omitted. Only for the default repository. This does not modify GitHub."
            }
            Self::CreateBranch => {
                "Create a branch from the active branch and make it active. If the name is taken, _v1, _v2 ... is appended until a free name is found. This is a remote write."
            }
            Self::DeleteBranch => {
                "Delete a branch. main, master and the toolkit's base branch are refused; deleting the active branch needs force=true and resets the active branch to the base branch. This is a remote destructive effect."
            }
            Self::CreateIssue => {
                "Create an issue with title and optional Markdown body, labels and assignees. Returns the issue number and URL. This is a remote write."
            }
            Self::UpdateIssue => {
                "Update an issue: title, body, state (open or closed). Labels and assignees REPLACE the current ones when given; pass an empty list [] to remove them all; leave them out to keep them. This is a remote write."
            }
            Self::ApplyGitPatch => {
                "Apply a unified diff (git patch) to branch, one commit per file. Every file is checked against the branch first and nothing is written if any file does not match. The base branch is protected. This is a remote write."
            }
            Self::ApplyGitPatchFromFile => {
                "Apply a unified diff stored in a platform artifact bucket (bucket_name, file_name) to branch, as apply_git_patch does. This is a remote write."
            }
            Self::TriggerWorkflow => {
                "Start a GitHub Actions workflow with a workflow_dispatch trigger on ref (a branch or tag), optionally with inputs. workflow_id is the workflow's id or file name. This is a remote write."
            }
            Self::CreateIssueOnProject => {
                "Create an issue on a GitHub Project V2 board found by project_title in board_repo, then set the board fields given in fields (single-select and date fields by name, Labels and Assignees as lists; an empty string clears a field). It is several requests: if a later one fails, the answer says what was set. This is a remote write."
            }
            Self::UpdateIssueOnProject => {
                "Update the title and body of issue issue_number on the board project_title in board_repo, and the board fields given in fields (an empty string clears a field; an empty Labels or Assignees list removes them all). This is a remote write."
            }
        }
    }

    #[allow(clippy::too_many_lines, reason = "one schema per SDK tool")]
    pub(super) fn schema(self) -> Value {
        let repo = json!({"type": ["string", "null"], "maxLength": 512, "description": "Name of the repository (e.g., 'owner/repo'). If None, uses the default repository."});
        let text = |max: usize, description: &str| json!({"type": "string", "minLength": 1, "maxLength": max, "description": description});
        match self {
            Self::CommentOnIssue => json!({
                "type": "object",
                "properties": {
                    "issue_number": text(32, "The issue or PR number as a string, e.g. `42`"),
                    "comment": {"type": "string", "minLength": 1, "maxLength": MAX_WRITE_BYTES, "description": "The comment text to add to the issue or PR"},
                    "repo_name": repo
                },
                "required": ["issue_number", "comment"],
                "additionalProperties": false
            }),
            Self::CreatePullRequest => json!({
                "type": "object",
                "properties": {
                    "title": text(1_024, "Title of the pull request"),
                    "body": {"type": "string", "maxLength": MAX_WRITE_BYTES, "description": "Body of the pull request"},
                    "head": text(1_024, "The active branch containing the changes"),
                    "base": {"type": ["string", "null"], "maxLength": 1_024, "description": "The target branch (defaults to github_base_branch)"},
                    "repo_name": repo
                },
                "required": ["title", "body", "head"],
                "additionalProperties": false
            }),
            Self::CreateFile => json!({
                "type": "object",
                "properties": {
                    "file_path": text(4_096, "The path of the file to create in the repository, e.g. `src/new_file.py` or `images/photo.png`"),
                    "branch": text(1_024, "The branch to create the file on."),
                    "repo_name": repo,
                    "file_contents": {"type": ["string", "null"], "maxLength": MAX_WRITE_BYTES, "description": "Content for creating new files. Use this for text, code, JSON, or structured data. Leave empty if using filepath to copy existing file."},
                    "filepath": {"type": ["string", "null"], "maxLength": 4_096, "description": "File path in format /{bucket}/{filename} to copy existing artifact. Use this to copy images, PDFs, or any binary files while preserving format. Leave empty if using file_contents to create new content."}
                },
                "required": ["file_path", "branch"],
                "additionalProperties": false
            }),
            Self::UpdateFile => json!({
                "type": "object",
                "properties": {
                    "file_query": {"type": "string", "multiline": true, "maxLength": MAX_WRITE_BYTES, "description": "First non-empty line: the file path (must not start with a slash). Then one or more edits, each marker on its own line:\nOLD <<<<\nold content\n>>>> OLD\nNEW <<<<\nnew content\n>>>> NEW\nLeading/trailing whitespace in content is stripped."},
                    "branch": text(1_024, "The branch to commit the update to."),
                    "repo_name": repo,
                    "commit_message": {"type": ["string", "null"], "maxLength": 4_096, "description": "Commit message for the update operation"}
                },
                "required": ["file_query", "branch"],
                "additionalProperties": false
            }),
            Self::DeleteFile => json!({
                "type": "object",
                "properties": {
                    "file_path": text(4_096, "The path of the file to delete, e.g. `src/old_file.py`"),
                    "branch": text(1_024, "The branch to delete the file from."),
                    "repo_name": repo
                },
                "required": ["file_path", "branch"],
                "additionalProperties": false
            }),
            Self::SetActiveBranch => json!({
                "type": "object",
                "properties": {
                    "branch_name": text(1_024, "The name of the branch, e.g. `main`"),
                    "repo_name": repo
                },
                "required": ["branch_name"],
                "additionalProperties": false
            }),
            Self::CreateBranch => json!({
                "type": "object",
                "properties": {
                    "proposed_branch_name": text(1_024, "The name of the new branch to create, e.g. `feature-branch`")
                },
                "required": ["proposed_branch_name"],
                "additionalProperties": false
            }),
            Self::DeleteBranch => json!({
                "type": "object",
                "properties": {
                    "branch_name": text(1_024, "The name of the branch to delete, e.g. `feature-branch`"),
                    "force": {"type": ["boolean", "null"], "default": false, "description": "Force deletion even if branch is the current active branch"}
                },
                "required": ["branch_name"],
                "additionalProperties": false
            }),
            Self::CreateIssue => json!({
                "type": "object",
                "properties": {
                    "title": text(1_024, "The title of the issue. Example: 'Fix authentication bug in login flow'"),
                    "body": {"type": ["string", "null"], "maxLength": MAX_WRITE_BYTES, "description": "The detailed description of the issue in Markdown format."},
                    "repo_name": repo,
                    "labels": {"type": ["array", "null"], "items": {"type": "string"}, "maxItems": 100, "description": "Labels to attach to the issue. Example: ['bug', 'high-priority']. Must be existing labels in the repository."},
                    "assignees": {"type": ["array", "null"], "items": {"type": "string"}, "maxItems": 10, "description": "GitHub usernames to assign the issue to. Example: ['octocat', 'hubot']. Users must have repository access."}
                },
                "required": ["title"],
                "additionalProperties": false
            }),
            Self::UpdateIssue => json!({
                "type": "object",
                "properties": {
                    "issue_id": {"type": "integer", "minimum": 1, "description": "Issue number to update. Example: 42. This is the issue number shown in the URL, not the internal ID."},
                    "title": {"type": ["string", "null"], "maxLength": 1_024, "description": "New title for the issue."},
                    "body": {"type": ["string", "null"], "maxLength": MAX_WRITE_BYTES, "description": "New description in Markdown format. Set to update the full issue body."},
                    "labels": {"type": ["array", "null"], "items": {"type": "string"}, "maxItems": 100, "description": "Replace all labels with this list. Example: ['bug', 'wontfix']. Pass empty list [] to remove all labels."},
                    "assignees": {"type": ["array", "null"], "items": {"type": "string"}, "maxItems": 10, "description": "Replace all assignees with this list. Example: ['octocat']. Pass empty list [] to unassign everyone."},
                    "state": {"type": ["string", "null"], "description": "Set issue state. Must be 'open' or 'closed'. Example: 'closed' to close the issue."},
                    "repo_name": repo
                },
                "required": ["issue_id"],
                "additionalProperties": false
            }),
            Self::ApplyGitPatch => json!({
                "type": "object",
                "properties": {
                    "patch_content": {"type": "string", "multiline": true, "maxLength": MAX_PATCH_BYTES, "description": "The git patch content in unified diff format"},
                    "branch": text(1_024, "The branch to apply the patch to."),
                    "commit_message": {"type": ["string", "null"], "default": "Apply git patch", "maxLength": 4_096, "description": "Commit message for the patch application"},
                    "repo_name": repo
                },
                "required": ["patch_content", "branch"],
                "additionalProperties": false
            }),
            Self::ApplyGitPatchFromFile => json!({
                "type": "object",
                "properties": {
                    "bucket_name": text(63, "Name of the artifact bucket containing the patch file"),
                    "file_name": text(1_024, "Name of the patch file to download and apply"),
                    "branch": text(1_024, "The branch to apply the patch to."),
                    "commit_message": {"type": ["string", "null"], "default": "Apply git patch from artifact", "maxLength": 4_096, "description": "Commit message for the patch application"},
                    "repo_name": repo
                },
                "required": ["bucket_name", "file_name", "branch"],
                "additionalProperties": false
            }),
            Self::CreateIssueOnProject => json!({
                "type": "object",
                "properties": {
                    "board_repo": text(512, "The organization and repository for the board (project). Example: 'org-name/repo-name'"),
                    "project_title": text(1_024, "The title of the project to which the issue will be added"),
                    "title": text(1_024, "Title for the newly created issue"),
                    "body": {"type": "string", "maxLength": MAX_WRITE_BYTES, "description": "Body text for the newly created issue"},
                    "fields": {"type": ["object", "null"], "description": "Additional key value pairs for issue field configurations"},
                    "issue_repo": {"type": ["string", "null"], "maxLength": 512, "description": "The issue's organization and repository to link issue on the board. Example: 'org-name/repo-name-2'"}
                },
                "required": ["board_repo", "project_title", "title", "body"],
                "additionalProperties": false
            }),
            Self::UpdateIssueOnProject => json!({
                "type": "object",
                "properties": {
                    "board_repo": text(512, "The organization and repository for the board (project). Example: 'org-name/repo-name'"),
                    "issue_number": text(32, "The unique number of the issue to update"),
                    "project_title": text(1_024, "The title of the project from which to fetch the issue"),
                    "title": text(1_024, "New title to set for the issue"),
                    "body": {"type": "string", "maxLength": MAX_WRITE_BYTES, "description": "New body content to set for the issue"},
                    "fields": {"type": ["object", "null"], "description": "A dictionary of additional field values by field names to update. Provide empty string to clear field"},
                    "issue_repo": {"type": ["string", "null"], "maxLength": 512, "description": "The issue's organization and repository to link issue on the board. Example: 'org-name/repo-name-2'"}
                },
                "required": ["board_repo", "issue_number", "project_title", "title", "body"],
                "additionalProperties": false
            }),
            Self::TriggerWorkflow => json!({
                "type": "object",
                "properties": {
                    "workflow_id": text(512, "The ID or file name of the workflow to trigger (e.g., 'build.yml', '1234567')"),
                    "ref": text(1_024, "The branch or tag reference to trigger the workflow on (e.g., 'main', 'v1.0.0')"),
                    "inputs": {"type": ["object", "null"], "description": "Optional inputs for the workflow, as defined in the workflow file"},
                    "repo_name": repo
                },
                "required": ["workflow_id", "ref"],
                "additionalProperties": false
            }),
        }
    }
}

// ----- the tool ------------------------------------------------------------

pub(super) struct GitHubWriteTool {
    kind: WriteKind,
    client: Arc<GitHubClient>,
    office: Option<SharedOfficeToolHost>,
    description: Box<str>,
}

impl GitHubWriteTool {
    pub(super) fn new(
        kind: WriteKind,
        toolkit_name: &str,
        repository: &str,
        client: Arc<GitHubClient>,
        office: Option<SharedOfficeToolHost>,
    ) -> Self {
        let description = format!(
            "Toolkit: {toolkit_name}\nRepository: {repository}\n{}",
            kind.sentence()
        );
        let mut end = description.len().min(MAX_DESCRIPTION_BYTES);
        while !description.is_char_boundary(end) {
            end -= 1;
        }
        Self {
            kind,
            client,
            office,
            description: description[..end].into(),
        }
    }
}

#[async_trait]
impl Tool for GitHubWriteTool {
    fn name(&self) -> &str {
        self.kind.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        false
    }

    fn is_concurrency_safe(&self) -> bool {
        false
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(self.kind.schema())
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        let arguments = arguments.as_object().ok_or_else(|| {
            AdkError::new(
                ErrorComponent::Tool,
                ErrorCategory::InvalidInput,
                "github.arguments.invalid",
                "the GitHub tool arguments are invalid",
            )
        })?;
        let client = self.client.as_ref();
        let result = match self.kind {
            WriteKind::SetActiveBranch => set_active_branch(client, arguments).await,
            WriteKind::CreateBranch => create_branch(client, arguments).await,
            WriteKind::DeleteBranch => delete_branch(client, arguments).await,
            WriteKind::CreateFile => create_file(client, self.office.as_ref(), arguments).await,
            WriteKind::UpdateFile => update_file(client, arguments).await,
            WriteKind::DeleteFile => delete_file(client, arguments).await,
            WriteKind::CreatePullRequest => create_pull_request(client, arguments).await,
            WriteKind::CreateIssue => create_issue(client, arguments).await,
            WriteKind::UpdateIssue => update_issue(client, arguments).await,
            WriteKind::CommentOnIssue => comment_on_issue(client, arguments).await,
            WriteKind::TriggerWorkflow => trigger_workflow(client, arguments).await,
            WriteKind::ApplyGitPatch => apply_git_patch_tool(client, arguments).await,
            WriteKind::ApplyGitPatchFromFile => {
                apply_git_patch_from_file(client, self.office.as_ref(), arguments).await
            }
            WriteKind::CreateIssueOnProject => {
                super::project_writes::create_issue_on_project(client, arguments).await
            }
            WriteKind::UpdateIssueOnProject => {
                super::project_writes::update_issue_on_project(client, arguments).await
            }
        };
        Ok(match result {
            Ok(value) => value,
            Err(fail) => {
                let (code, sentence) = fail.sentence(self.kind.action());
                tracing::warn!(
                    event = "github_write_tool_failed",
                    tool = self.kind.name(),
                    reason_code = code,
                    "a GitHub write tool did not complete"
                );
                Value::String(sentence)
            }
        })
    }
}

// ----- failures --------------------------------------------------------------

/// Why a tool did not finish: an argument the SDK would also refuse, or a
/// GitHub failure with the reason GitHub gave.
pub(super) enum Fail {
    Argument(String),
    Client(GitHubClientError, Option<String>),
}

impl From<GitHubClientError> for Fail {
    fn from(error: GitHubClientError) -> Self {
        Self::Client(error, None)
    }
}

impl Fail {
    fn code(&self) -> Option<GitHubClientErrorCode> {
        match self {
            Self::Client(error, _) => Some(error.code()),
            Self::Argument(_) => None,
        }
    }

    fn reason(&self) -> Option<&str> {
        match self {
            Self::Client(_, reason) => reason.as_deref(),
            Self::Argument(_) => None,
        }
    }

    /// One sentence per cause, as a stable log code and the text the model
    /// reads. Nothing here repeats a credential or a URL; GitHub's own reason
    /// (a validation message) is appended where it helps.
    ///
    /// It is a tool RESULT, not an `AdkError`: the policy wrapper redacts a
    /// tool's error to its category (`invocation::sanitize_tool_error`), which
    /// would turn a 403 and a 409 into the same "tool execution failed". The
    /// artifact family answers its failures the same way, for the same reason.
    pub(super) fn sentence(self, action: &str) -> (&'static str, String) {
        let (error, reason) = match self {
            Self::Argument(sentence) => return ("github.arguments.invalid", sentence),
            Self::Client(error, reason) => (error, reason),
        };
        let because = reason
            .map(|reason| format!(" GitHub said: {reason}"))
            .unwrap_or_default();
        match error.code() {
            GitHubClientErrorCode::Authentication => (
                "github.write.authentication",
                format!(
                    "Could not {action}: GitHub rejected the toolkit's credentials (401). Do not retry; tell the user to check the token."
                ),
            ),
            GitHubClientErrorCode::Authorization => (
                "github.write.authorization",
                format!(
                    "Could not {action}: GitHub refused (403). The token has no write permission for this repository or branch, or the branch is protected.{because} Nothing was changed."
                ),
            ),
            GitHubClientErrorCode::NotFound => (
                "github.write.not_found",
                format!(
                    "Could not {action}: GitHub did not find the repository, branch, file, issue or workflow named (404), or the token cannot see it. Check the names with the read tools.{because}"
                ),
            ),
            GitHubClientErrorCode::Conflict => (
                "github.write.conflict",
                format!(
                    "Could not {action}: GitHub reports a conflict (409): the target changed or already exists.{because} Read it again and redo the change. Nothing was changed."
                ),
            ),
            GitHubClientErrorCode::InvalidInput => (
                "github.write.validation",
                format!(
                    "Could not {action}: GitHub rejected the request as invalid (422).{because} Nothing was changed."
                ),
            ),
            GitHubClientErrorCode::RateLimited => (
                "github.write.rate_limited",
                format!(
                    "Could not {action}: GitHub rate limited the request. Nothing was changed; try again later."
                ),
            ),
            GitHubClientErrorCode::Timeout | GitHubClientErrorCode::DependencyUnavailable => (
                "github.write.unreachable",
                format!(
                    "Could not {action}: GitHub could not be reached. Nothing was changed; try again later."
                ),
            ),
            GitHubClientErrorCode::UnknownOutcome => (
                "github.effect.unknown_outcome",
                format!(
                    "Could not confirm that GitHub did {action}: the request was sent but GitHub's answer was lost or was a server error, so the change may or may not have been made.{because} Check the repository before trying again."
                ),
            ),
            GitHubClientErrorCode::UnsupportedAuthentication => (
                "github.write.authentication_unsupported",
                format!(
                    "Could not {action}: this toolkit authenticates as a GitHub App, which this runtime cannot use to change a repository. Use a token or a username and password."
                ),
            ),
            GitHubClientErrorCode::EgressRefused => (
                "github.egress.refused",
                format!(
                    "Could not {action}: the GitHub host is not on this runtime's egress allowlist. Nothing was sent."
                ),
            ),
            GitHubClientErrorCode::ResourceExhausted => (
                "github.write.too_large",
                format!(
                    "Could not {action}: the content or GitHub's answer is larger than this runtime handles. Nothing was changed."
                ),
            ),
            GitHubClientErrorCode::InvalidResponse => (
                "github.response.invalid",
                format!("Could not {action}: GitHub's answer could not be understood.{because}"),
            ),
            GitHubClientErrorCode::InvalidConfiguration => (
                "github.configuration.invalid",
                format!("Could not {action}: the GitHub toolkit configuration is invalid."),
            ),
        }
    }
}

pub(super) type Outcome = Result<Value, Fail>;

#[allow(
    clippy::unnecessary_wraps,
    reason = "the tools return `Outcome`; a refusal sentence is an Ok result"
)]
pub(super) fn text(sentence: impl Into<String>) -> Outcome {
    Ok(Value::String(sentence.into()))
}

pub(super) fn argument(sentence: impl Into<String>) -> Fail {
    Fail::Argument(sentence.into())
}

// ----- arguments -------------------------------------------------------------

pub(super) fn optional_str<'a>(
    arguments: &'a Map<String, Value>,
    key: &str,
) -> Result<Option<&'a str>, Fail> {
    match arguments.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) => Ok(Some(value.as_str())),
        Some(_) => Err(argument(format!("`{key}` must be a string"))),
    }
}

pub(super) fn required_str<'a>(
    arguments: &'a Map<String, Value>,
    key: &str,
) -> Result<&'a str, Fail> {
    optional_str(arguments, key)?
        .filter(|value| !value.trim().is_empty())
        .ok_or_else(|| argument(format!("`{key}` is required and must not be empty")))
}

fn string_list(arguments: &Map<String, Value>, key: &str) -> Result<Option<Vec<String>>, Fail> {
    match arguments.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Array(items)) => items
            .iter()
            .map(|item| {
                item.as_str()
                    .map(str::to_owned)
                    .ok_or_else(|| argument(format!("`{key}` must be a list of strings")))
            })
            .collect::<Result<Vec<_>, _>>()
            .map(Some),
        Some(_) => Err(argument(format!("`{key}` must be a list of strings"))),
    }
}

fn plain_name(label: &str, value: &str, max: usize) -> Result<(), Fail> {
    if value.is_empty() {
        return Err(argument(format!("`{label}` must not be empty")));
    }
    if value.len() > max {
        return Err(argument(format!("`{label}` is longer than {max} bytes")));
    }
    if value.chars().any(char::is_control) {
        return Err(argument(format!(
            "`{label}` must not contain control characters"
        )));
    }
    Ok(())
}

/// A branch, tag or ref name. It ends up in a URL path and in a ref, so a
/// `.`, `..` or empty segment is refused (`./main` would otherwise address
/// another ref, `../main` the one above).
fn branch_name(label: &str, value: &str) -> Result<(), Fail> {
    if value.starts_with("refs/") {
        return Err(argument(format!(
            "`{label}` must be a branch name such as `main`, not a full ref such as `refs/heads/main`"
        )));
    }
    ref_name(label, value)
}

/// A branch or tag name, or a ref, as a URL path: no empty, `.` or `..`
/// segment.
fn ref_name(label: &str, value: &str) -> Result<(), Fail> {
    plain_name(label, value, 1_024)?;
    if value
        .split('/')
        .any(|segment| segment.is_empty() || matches!(segment, "." | ".."))
    {
        return Err(argument(format!(
            "`{label}` must not contain empty, `.` or `..` segments or end with a slash"
        )));
    }
    Ok(())
}

/// An integer argument the way the SDK's pydantic models read one (the shared
/// reading in `integer_like.rs`); anything else is "must be a number".
fn integer_like(arguments: &Map<String, Value>, key: &str) -> Result<Option<u64>, Fail> {
    match arguments.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(value) => crate::toolkits::families::integer_like::unsigned_like(value)
            .map(Some)
            .ok_or_else(|| argument(format!("`{key}` must be a number"))),
    }
}

fn file_path(label: &str, value: &str) -> Result<(), Fail> {
    plain_name(label, value, 4_096)?;
    if value.starts_with('/') {
        return Err(argument(format!("`{label}` must not start with a slash")));
    }
    if value
        .split('/')
        .any(|segment| segment.is_empty() || matches!(segment, "." | ".."))
    {
        return Err(argument(format!(
            "`{label}` must not contain empty, `.` or `..` segments or end with a slash"
        )));
    }
    Ok(())
}

/// `(owner, repository)` of `repo_name`, or of the toolkit's repository.
fn repository(
    client: &GitHubClient,
    arguments: &Map<String, Value>,
) -> Result<(String, String), Fail> {
    let repository = match optional_str(arguments, "repo_name")? {
        Some(name) if !name.trim().is_empty() => name.trim(),
        _ => client.rest.config().repository(),
    };
    elitea_connectors::github::client::validate_repository(repository)
        .map(|(owner, name)| (owner.to_owned(), name.to_owned()))
        .map_err(|_| argument("`repo_name` must look like `owner/repo`"))
}

fn protected(base: &str) -> String {
    format!(
        "You're attempting to commit directly to the {base} branch, which is protected. Please create a new branch and try again."
    )
}

fn base_branch(client: &GitHubClient) -> &str {
    client.rest.config().base_branch()
}

fn valid_commit_id(value: &str) -> bool {
    matches!(value.len(), 40 | 64) && value.bytes().all(|byte| byte.is_ascii_hexdigit())
}

// ----- requests ----------------------------------------------------------------

impl GitHubClient {
    /// One write (or any JSON request) with the SDK-visible failures mapped:
    /// the status decides the cause, GitHub's reason rides along.
    async fn write(
        &self,
        method: Method,
        path: &[&str],
        body: Option<&Value>,
    ) -> Result<Value, Fail> {
        let reply = self
            .rest
            .send_effect(method, path, &[], body, MAX_RESPONSE_BYTES)
            .await?;
        if let Some(error) = reply.failure() {
            return Err(Fail::Client(error, reply.reason()));
        }
        // A 2xx without a readable body is still a success (a 204, or an
        // answer the tool does not need).
        Ok(reply.json().unwrap_or(Value::Null))
    }

    /// One read inside a write tool (the file's sha, a branch's commit).
    async fn read(
        &self,
        path: &[&str],
        query: &[(&str, String)],
        max_bytes: usize,
    ) -> Result<Value, Fail> {
        self.rest
            .get_json(
                elitea_connectors::github::client::GitHubRequestKind::Repository,
                path,
                query,
                max_bytes,
            )
            .await
            .map_err(Fail::from)
    }
}

fn contents_path<'a>(owner: &'a str, repository: &'a str, file: &'a str) -> Vec<&'a str> {
    let mut path = vec!["repos", owner, repository, "contents"];
    path.extend(file.split('/'));
    path
}

/// A file read for editing: its sha and its UTF-8 text.
struct FileState {
    sha: String,
    content: String,
}

fn file_state(value: &Value, path: &str) -> Result<FileState, FileProblem> {
    let Some(object) = value.as_object() else {
        return Err(FileProblem::Other(format!("{path} is not a file")));
    };
    if object.get("type").and_then(Value::as_str) != Some("file") {
        return Err(FileProblem::Other(format!("{path} is not a file")));
    }
    let sha = object
        .get("sha")
        .and_then(Value::as_str)
        .filter(|sha| valid_commit_id(sha))
        .ok_or_else(|| FileProblem::Other("GitHub did not report the file's sha".to_owned()))?;
    if object.get("encoding").and_then(Value::as_str) != Some("base64") {
        return Err(FileProblem::TooLarge);
    }
    let encoded = object
        .get("content")
        .and_then(Value::as_str)
        .unwrap_or_default();
    let compact: Vec<u8> = encoded
        .bytes()
        .filter(|byte| !byte.is_ascii_whitespace())
        .collect();
    let decoded = STANDARD
        .decode(compact)
        .map_err(|_| FileProblem::Other("GitHub's file content could not be decoded".to_owned()))?;
    if decoded.len() > MAX_WRITE_BYTES {
        return Err(FileProblem::TooLarge);
    }
    let content = String::from_utf8(decoded).map_err(|_| FileProblem::NotText)?;
    Ok(FileState {
        sha: sha.to_ascii_lowercase(),
        content,
    })
}

enum FileProblem {
    TooLarge,
    NotText,
    Other(String),
}

impl FileProblem {
    fn sentence(&self, path: &str) -> String {
        match self {
            Self::TooLarge => {
                format!("{path} is larger than the {MAX_WRITE_BYTES} bytes this runtime edits.")
            }
            Self::NotText => format!("{path} is not UTF-8 text, so it cannot be edited here."),
            Self::Other(sentence) => sentence.clone(),
        }
    }
}

// ----- branches ------------------------------------------------------------------

async fn set_active_branch(client: &GitHubClient, arguments: &Map<String, Value>) -> Outcome {
    let name = required_str(arguments, "branch_name")?;
    branch_name("branch_name", name)?;
    if optional_str(arguments, "repo_name")?.is_some_and(|repo| !repo.trim().is_empty()) {
        return text(
            "Cannot set active branch for external repository. The active branch setting only applies to the default repository.",
        );
    }
    let (owner, repo) = repository(client, arguments)?;
    match client
        .read(
            &["repos", &owner, &repo, "branches", name],
            &[],
            MAX_RESPONSE_BYTES,
        )
        .await
    {
        Ok(_) => {
            client.set_active_branch(name);
            text(format!("Active branch set to '{name}'"))
        }
        Err(fail) if fail.code() == Some(GitHubClientErrorCode::NotFound) => {
            text(format!("Branch '{name}' not found in repository"))
        }
        Err(fail) => Err(fail),
    }
}

async fn create_branch(client: &GitHubClient, arguments: &Map<String, Value>) -> Outcome {
    let proposed = required_str(arguments, "proposed_branch_name")?;
    branch_name("proposed_branch_name", proposed)?;
    let (owner, repo) = repository(client, arguments)?;
    let active = client.active_branch();
    let source = if active.is_empty() {
        base_branch(client).to_owned()
    } else {
        active
    };
    let branch = client
        .read(
            &["repos", &owner, &repo, "branches", &source],
            &[],
            MAX_RESPONSE_BYTES,
        )
        .await?;
    let sha = branch
        .pointer("/commit/sha")
        .and_then(Value::as_str)
        .filter(|sha| valid_commit_id(sha))
        .ok_or(Fail::Client(
            error(GitHubClientErrorCode::InvalidResponse),
            None,
        ))?
        .to_owned();
    let mut candidate = proposed.to_owned();
    let mut next_suffix = 1_usize;
    let mut listed_taken = false;
    for _ in 0..MAX_BRANCH_ATTEMPTS {
        let reference = format!("refs/heads/{candidate}");
        let body = json!({"ref": reference, "sha": sha});
        match client
            .write(
                Method::POST,
                &["repos", &owner, &repo, "git", "refs"],
                Some(&body),
            )
            .await
        {
            Ok(_) => {
                client.set_active_branch(&candidate);
                return text(format!(
                    "Branch '{candidate}' created successfully, and set as current active branch."
                ));
            }
            Err(fail) if reference_exists(&fail) => {
                if listed_taken {
                    next_suffix += 1;
                } else {
                    listed_taken = true;
                    next_suffix = first_free_suffix(client, &owner, &repo, proposed)
                        .await
                        .max(1);
                }
                candidate = format!("{proposed}_v{next_suffix}");
            }
            Err(Fail::Client(error, reason))
                if error.code() == GitHubClientErrorCode::InvalidInput =>
            {
                return text(format!(
                    "Unable to create branch due to error: {}",
                    reason.unwrap_or_else(|| "GitHub rejected the branch name".to_owned())
                ));
            }
            Err(fail) => return Err(fail),
        }
    }
    text(format!(
        "Unable to create branch. The names derived from `{proposed}` (up to `{candidate}`) are all taken."
    ))
}

fn reference_exists(fail: &Fail) -> bool {
    fail.code() == Some(GitHubClientErrorCode::InvalidInput)
        && fail
            .reason()
            .is_some_and(|reason| reason.contains("Reference already exists"))
}

/// The smallest `N` such that `{proposed}_vN` is not a branch yet, from the
/// refs GitHub lists under that prefix (the first page). A listing that
/// fails only costs the extra attempts.
async fn first_free_suffix(
    client: &GitHubClient,
    owner: &str,
    repo: &str,
    proposed: &str,
) -> usize {
    let prefix = format!("{proposed}_v");
    let mut path = vec!["repos", owner, repo, "git", "matching-refs", "heads"];
    let pieces: Vec<&str> = prefix.split('/').collect();
    path.extend(pieces);
    let Ok(listed) = client.read(&path, &[], MAX_RESPONSE_BYTES).await else {
        return 1;
    };
    let taken: Vec<usize> = listed
        .as_array()
        .map(|refs| {
            refs.iter()
                .filter_map(|entry| entry.get("ref").and_then(Value::as_str))
                .filter_map(|reference| reference.strip_prefix("refs/heads/"))
                .filter_map(|name| name.strip_prefix(prefix.as_str()))
                .filter_map(|suffix| suffix.parse::<usize>().ok())
                .collect()
        })
        .unwrap_or_default();
    (1..=taken.len() + 1)
        .find(|number| !taken.contains(number))
        .unwrap_or(1)
}

async fn delete_branch(client: &GitHubClient, arguments: &Map<String, Value>) -> Outcome {
    let name = required_str(arguments, "branch_name")?;
    branch_name("branch_name", name)?;
    let force = match arguments.get("force") {
        None | Some(Value::Null) => false,
        Some(Value::Bool(value)) => *value,
        Some(_) => return Err(argument("`force` must be true or false")),
    };
    let base = base_branch(client).to_owned();
    let mut protected_names = vec!["main".to_owned(), "master".to_owned(), base.clone()];
    protected_names.sort();
    protected_names.dedup();
    if protected_names
        .iter()
        .any(|protected| protected.eq_ignore_ascii_case(name))
    {
        return text(format!(
            "Cannot delete branch '{name}': It is a protected branch (main, master, or base branch). Protected branches: {}",
            protected_names.join(", ")
        ));
    }
    let active = client.active_branch();
    if name == active && !force {
        return text(format!(
            "Cannot delete branch '{name}': It is currently the active branch. Use force=True to delete it anyway, or switch to a different branch first."
        ));
    }
    let (owner, repo) = repository(client, arguments)?;
    let mut path = vec![
        "repos",
        owner.as_str(),
        repo.as_str(),
        "git",
        "refs",
        "heads",
    ];
    path.extend(name.split('/'));
    match client.write(Method::DELETE, &path, None).await {
        Ok(_) => {
            if name == active {
                client.set_active_branch(&base);
                return text(format!(
                    "Branch '{name}' deleted successfully. Active branch has been reset to '{base}'."
                ));
            }
            text(format!("Branch '{name}' deleted successfully."))
        }
        Err(fail) => match fail.code() {
            Some(GitHubClientErrorCode::NotFound) => {
                text(format!("Branch '{name}' not found in the repository."))
            }
            Some(GitHubClientErrorCode::InvalidInput) => text(format!(
                "Cannot delete branch '{name}': {}",
                fail.reason().unwrap_or("GitHub rejected the request")
            )),
            _ => Err(fail),
        },
    }
}

// ----- files ------------------------------------------------------------------------

/// The contents of `path` on `branch`, `None` when it is not there.
async fn existing(
    client: &GitHubClient,
    owner: &str,
    repo: &str,
    path: &str,
    branch: &str,
) -> Result<Option<Value>, Fail> {
    match client
        .read(
            &contents_path(owner, repo, path),
            &[("ref", branch.to_owned())],
            MAX_CONTENTS_RESPONSE_BYTES,
        )
        .await
    {
        Ok(value) => Ok(Some(value)),
        Err(fail) if fail.code() == Some(GitHubClientErrorCode::NotFound) => Ok(None),
        Err(fail) => Err(fail),
    }
}

#[allow(clippy::too_many_arguments, reason = "one commit request, spelled out")]
async fn put_file(
    client: &GitHubClient,
    owner: &str,
    repo: &str,
    path: &str,
    message: &str,
    bytes: &[u8],
    branch: &str,
    sha: Option<&str>,
) -> Result<(), Fail> {
    let mut body = json!({
        "message": message,
        "content": STANDARD.encode(bytes),
        "branch": branch,
    });
    if let (Some(sha), Some(object)) = (sha, body.as_object_mut()) {
        object.insert("sha".to_owned(), json!(sha));
    }
    client
        .write(Method::PUT, &contents_path(owner, repo, path), Some(&body))
        .await
        .map(|_| ())
}

async fn create_file(
    client: &GitHubClient,
    office: Option<&SharedOfficeToolHost>,
    arguments: &Map<String, Value>,
) -> Outcome {
    let path = required_str(arguments, "file_path")?;
    let branch = required_str(arguments, "branch")?;
    file_path("file_path", path)?;
    branch_name("branch", branch)?;
    let contents = optional_str(arguments, "file_contents")?;
    let copy_from = optional_str(arguments, "filepath")?;
    if contents.is_none() && copy_from.is_none() {
        return text(
            "Must provide either 'file_contents' (to create new content) or 'filepath' (to copy existing file). Both parameters cannot be empty.",
        );
    }
    if contents.is_some() && copy_from.is_some() {
        return text(
            "Cannot provide both 'file_contents' and 'filepath'. Use 'filepath' to copy existing files preserving binary format, or 'file_contents' to create new content from text/code.",
        );
    }
    let base = base_branch(client);
    if branch == base {
        return text(protected(base));
    }
    if copy_from.is_some() && office.is_none() {
        // Said before any request: the SDK says it after the existence check,
        // but the answer is the same and nothing needs to be sent for it.
        return text(NO_FILE_STORE);
    }
    let (owner, repo) = repository(client, arguments)?;
    if existing(client, &owner, &repo, path, branch)
        .await?
        .is_some()
    {
        return text(format!(
            "File {path} already exists. Use update_file instead."
        ));
    }
    let (bytes, copied) = if let Some(source) = copy_from {
        match copied_artifact(office, source).await {
            Ok(bytes) => (bytes, true),
            Err(sentence) => return text(sentence),
        }
    } else {
        let content = contents.unwrap_or_default();
        if content.len() > MAX_WRITE_BYTES {
            return Err(argument(format!(
                "`file_contents` is {} bytes, over the {MAX_WRITE_BYTES} this tool writes",
                content.len()
            )));
        }
        (content.as_bytes().to_vec(), false)
    };
    let suffix = if copied {
        " (copied from artifact)"
    } else {
        ""
    };
    put_file(
        client,
        &owner,
        &repo,
        path,
        &format!("Create {path}{suffix}"),
        &bytes,
        branch,
        None,
    )
    .await?;
    text(format!("Created file {path}{suffix}"))
}

const NO_FILE_STORE: &str = "EliteA client not configured. Cannot retrieve artifact content. (This runtime has no access to the platform's file store; pass file_contents instead.)";

/// The bytes of the platform object `/{bucket}/{filename}`, or the SDK's
/// sentence for why they are not available.
async fn copied_artifact(
    office: Option<&SharedOfficeToolHost>,
    filepath: &str,
) -> Result<Vec<u8>, String> {
    let Some(office) = office else {
        return Err(NO_FILE_STORE.to_owned());
    };
    let trimmed = filepath.trim().trim_start_matches('/');
    let Some((bucket, key)) = trimmed
        .split_once('/')
        .filter(|(bucket, key)| !bucket.is_empty() && !key.is_empty())
    else {
        return Err(format!(
            "Failed to retrieve artifact '{filepath}': filepath must be in /{{bucket}}/{{filename}} form."
        ));
    };
    office
        .read_object(bucket, key, MAX_COPY_BYTES)
        .await
        .map_err(|error| {
            format!(
                "Failed to retrieve artifact '{filepath}': {}",
                match error.kind() {
                    OfficeFailure::NotFound => "no such file or bucket.",
                    OfficeFailure::AccessRefused =>
                        "the platform refused access to the file store.",
                    OfficeFailure::TooLarge => "the file is larger than this runtime copies.",
                    OfficeFailure::InvalidRequest => "the name is not valid.",
                    _ => "the platform could not be reached.",
                }
            )
        })
}

#[allow(clippy::too_many_lines, reason = "one SDK flow, in the SDK's order")]
async fn update_file(client: &GitHubClient, arguments: &Map<String, Value>) -> Outcome {
    let query = optional_str(arguments, "file_query")?
        .ok_or_else(|| argument("`file_query` is required"))?;
    let branch = required_str(arguments, "branch")?;
    branch_name("branch", branch)?;
    let base = base_branch(client);
    if branch == base {
        return text(protected(base));
    }
    let mut lines = query.split('\n');
    let Some(path_line) = lines.by_ref().find(|line| !line.trim().is_empty()) else {
        return text(
            "Invalid file_query format. Expected first non-empty line to be the file path followed by OLD/NEW blocks.",
        );
    };
    let path = path_line.trim();
    let edit = lines.collect::<Vec<_>>().join("\n");
    file_path("the file path on the first line of file_query", path)?;
    let commit_message = match optional_str(arguments, "commit_message")? {
        Some(message) if !message.trim().is_empty() => message.to_owned(),
        _ => format!("Update {path}"),
    };
    let (owner, repo) = repository(client, arguments)?;
    let failed = |detail: &str| text(format!("Unable to update file due to error:\n{detail}"));
    // The SDK refuses a path it cannot edit and a query it cannot read before
    // it reads the file; an empty probe answers both offline.
    match apply_update(path, "", &edit) {
        Err(EditErrorCode::UnsupportedFile) => {
            return failed(&format!(
                "Cannot edit binary/document file '{path}'. Supported text formats: markdown, txt, csv, json, xml, html, yaml, code files."
            ));
        }
        Err(EditErrorCode::InvalidMarkers) => {
            return failed(
                "No OLD/NEW marker pairs found in file_query. Format: Each marker must be on its own line:\nOLD <<<<\nold text\n>>>> OLD\nNEW <<<<\nnew text\n>>>> NEW",
            );
        }
        Err(EditErrorCode::ResourceExhausted) => {
            return Err(Fail::Client(
                error(GitHubClientErrorCode::ResourceExhausted),
                None,
            ));
        }
        _ => {}
    }
    let Some(value) = existing(client, &owner, &repo, path, branch).await? else {
        return failed(&format!(
            "Failed to read file {path}: not found on branch {branch}"
        ));
    };
    let state = match file_state(&value, path) {
        Ok(state) => state,
        Err(problem) => {
            return failed(&format!(
                "Failed to read file {path}: {}",
                problem.sentence(path)
            ));
        }
    };
    let updated = match apply_update(path, &state.content, &edit) {
        Ok(updated) => updated,
        Err(EditErrorCode::NotFound) => {
            return text(format!("Normalized OLD block not found in {path}."));
        }
        Err(EditErrorCode::Ambiguous) => {
            return text(format!(
                "Multiple candidate regions for OLD block in {path}; no change applied to avoid ambiguity."
            ));
        }
        Err(EditErrorCode::NoChange) => {
            return text(format!(
                "Edits for {path} were applied but the final content is identical to the original. The sequence of OLD/NEW pairs appears to be redundant or self-cancelling. Please simplify or review the update_query."
            ));
        }
        Err(EditErrorCode::ResourceExhausted) => {
            return Err(Fail::Client(
                error(GitHubClientErrorCode::ResourceExhausted),
                None,
            ));
        }
        Err(EditErrorCode::UnsupportedFile | EditErrorCode::InvalidMarkers) => {
            return failed("the edit could not be read");
        }
    };
    // The sha of the version that was edited: a file that changed since is a
    // conflict, not a lost update.
    put_file(
        client,
        &owner,
        &repo,
        path,
        &commit_message,
        updated.as_bytes(),
        branch,
        Some(&state.sha),
    )
    .await
    .map_err(|fail| match fail {
        Fail::Client(cause, reason)
            if matches!(
                cause.code(),
                GitHubClientErrorCode::Conflict | GitHubClientErrorCode::InvalidInput
            ) && reason
                .as_deref()
                .is_some_and(|reason| reason.contains("does not match")) =>
        {
            Fail::Client(error(GitHubClientErrorCode::Conflict), reason)
        }
        other => other,
    })?;
    text(format!("Updated file {path}"))
}

async fn delete_file(client: &GitHubClient, arguments: &Map<String, Value>) -> Outcome {
    let path = required_str(arguments, "file_path")?;
    let branch = required_str(arguments, "branch")?;
    file_path("file_path", path)?;
    branch_name("branch", branch)?;
    let base = base_branch(client);
    if branch == base {
        return text(protected(base));
    }
    let (owner, repo) = repository(client, arguments)?;
    let Some(value) = existing(client, &owner, &repo, path, branch).await? else {
        return text(format!("File {path} not found."));
    };
    let Some(sha) = value.get("sha").and_then(Value::as_str).filter(|sha| {
        valid_commit_id(sha) && value.get("type").and_then(Value::as_str) == Some("file")
    }) else {
        return text(format!(
            "Unable to delete file due to error:\n{path} is a directory, not a file. Delete its files one by one."
        ));
    };
    let body = json!({"message": format!("Delete {path}"), "sha": sha, "branch": branch});
    client
        .write(
            Method::DELETE,
            &contents_path(&owner, &repo, path),
            Some(&body),
        )
        .await?;
    text(format!("Deleted file {path}"))
}

// ----- pull requests, issues, comments, workflows ---------------------------------

fn created(reply: &Value, key: &str) -> Result<(u64, String), Fail> {
    let number = reply.get(key).and_then(Value::as_u64);
    let url = reply.get("html_url").and_then(Value::as_str);
    match (number, url) {
        (Some(number), Some(url)) => Ok((number, url.to_owned())),
        _ => Err(Fail::Client(
            error(GitHubClientErrorCode::UnknownOutcome),
            Some("GitHub accepted the request but its answer did not name what it made".to_owned()),
        )),
    }
}

async fn create_pull_request(client: &GitHubClient, arguments: &Map<String, Value>) -> Outcome {
    let title = required_str(arguments, "title")?;
    let head = required_str(arguments, "head")?;
    let body = optional_str(arguments, "body")?.unwrap_or_default();
    let base = match optional_str(arguments, "base")? {
        Some(base) if !base.trim().is_empty() => base,
        _ => base_branch(client),
    };
    branch_name("head", head)?;
    branch_name("base", base)?;
    let (owner, repo) = repository(client, arguments)?;
    let request = json!({"title": title, "body": body, "head": head, "base": base});
    let reply = client
        .write(
            Method::POST,
            &["repos", &owner, &repo, "pulls"],
            Some(&request),
        )
        .await?;
    let (number, url) = created(&reply, "number")?;
    text(format!(
        "Pull request created successfully! PR #{number}, URL: {url}"
    ))
}

async fn create_issue(client: &GitHubClient, arguments: &Map<String, Value>) -> Outcome {
    let title = required_str(arguments, "title")?;
    let (owner, repo) = repository(client, arguments)?;
    let mut request = json!({
        "title": title,
        "labels": string_list(arguments, "labels")?.unwrap_or_default(),
        "assignees": string_list(arguments, "assignees")?.unwrap_or_default(),
    });
    if let (Some(body), Some(object)) = (optional_str(arguments, "body")?, request.as_object_mut())
    {
        object.insert("body".to_owned(), json!(body));
    }
    let reply = client
        .write(
            Method::POST,
            &["repos", &owner, &repo, "issues"],
            Some(&request),
        )
        .await?;
    let (number, url) = created(&reply, "number")?;
    text(format!(
        "Issue created successfully! ID:{number}, URL: {url}"
    ))
}

async fn update_issue(client: &GitHubClient, arguments: &Map<String, Value>) -> Outcome {
    let number = integer_like(arguments, "issue_id")?
        .filter(|number| *number > 0)
        .ok_or_else(|| argument("Issue ID is required."))?;
    let title = optional_str(arguments, "title")?.filter(|value| !value.is_empty());
    let body = optional_str(arguments, "body")?.filter(|value| !value.is_empty());
    let state = optional_str(arguments, "state")?.filter(|value| !value.is_empty());
    if let Some(state) = state
        && !matches!(state, "open" | "closed")
    {
        return Err(argument("`state` must be 'open' or 'closed'"));
    }
    // Labels and assignees REPLACE the current ones when given; an explicit
    // empty list clears them; absent leaves them as they are (the SDK's schema
    // text; its code removes them whenever they are absent, which a close
    // would silently turn into stripping an issue: owner decision).
    let labels = string_list(arguments, "labels")?;
    let assignees = string_list(arguments, "assignees")?;
    let (owner, repo) = repository(client, arguments)?;
    let mut request = Map::new();
    if let Some(labels) = &labels {
        request.insert("labels".to_owned(), json!(labels));
    }
    if let Some(assignees) = &assignees {
        request.insert("assignees".to_owned(), json!(assignees));
    }
    if let Some(title) = title {
        request.insert("title".to_owned(), json!(title));
    }
    if let Some(body) = body {
        request.insert("body".to_owned(), json!(body));
    }
    if let Some(state) = state {
        request.insert("state".to_owned(), json!(state));
    }
    if request.is_empty() {
        return Err(argument(
            "Nothing to update: pass at least one of title, body, state, labels or assignees.",
        ));
    }
    let number_text = number.to_string();
    let reply = client
        .write(
            Method::PATCH,
            &["repos", &owner, &repo, "issues", &number_text],
            Some(&Value::Object(request)),
        )
        .await?;
    let (number, url) = created(&reply, "number")?;
    let mut answer =
        format!("Issue updated successfully! Updated details: ID: {number}, URL: {url}");
    let mut cleared = Vec::new();
    if labels.as_ref().is_some_and(Vec::is_empty) {
        cleared.push("labels");
    }
    if assignees.as_ref().is_some_and(Vec::is_empty) {
        cleared.push("assignees");
    }
    if !cleared.is_empty() {
        let _ = write!(
            answer,
            " An empty list was passed, so all {} were removed from the issue.",
            cleared.join(" and ")
        );
    }
    text(answer)
}

async fn comment_on_issue(client: &GitHubClient, arguments: &Map<String, Value>) -> Outcome {
    let issue = required_str(arguments, "issue_number")?.trim();
    let number: u64 = issue
        .parse()
        .ok()
        .filter(|number| *number > 0)
        .ok_or_else(|| {
            argument("`issue_number` must be the issue or PR number as digits, e.g. `42`")
        })?;
    let comment = required_str(arguments, "comment")?;
    let (owner, repo) = repository(client, arguments)?;
    let number_text = number.to_string();
    let reply = client
        .write(
            Method::POST,
            &["repos", &owner, &repo, "issues", &number_text, "comments"],
            Some(&json!({"body": comment})),
        )
        .await?;
    let url = reply
        .get("html_url")
        .and_then(Value::as_str)
        .ok_or_else(|| {
            Fail::Client(
                error(GitHubClientErrorCode::UnknownOutcome),
                Some("GitHub accepted the comment but its answer did not name it".to_owned()),
            )
        })?;
    text(format!("Comment added successfully! URL: {url}"))
}

async fn trigger_workflow(client: &GitHubClient, arguments: &Map<String, Value>) -> Outcome {
    let workflow_id = required_str(arguments, "workflow_id")?.trim();
    let reference = required_str(arguments, "ref")?;
    ref_name("ref", reference)?;
    let inputs = match arguments.get("inputs") {
        None | Some(Value::Null) => json!({}),
        Some(value @ Value::Object(_)) => value.clone(),
        Some(_) => return Err(argument("`inputs` must be an object")),
    };
    let (owner, repo) = repository(client, arguments)?;
    let workflow = match client
        .read(
            &["repos", &owner, &repo, "actions", "workflows", workflow_id],
            &[],
            MAX_RESPONSE_BYTES,
        )
        .await
    {
        Ok(workflow) => workflow,
        Err(fail) if fail.code() == Some(GitHubClientErrorCode::NotFound) => {
            return text(format!(
                "Workflow with ID or filename '{workflow_id}' not found in repository {owner}/{repo}"
            ));
        }
        Err(fail) => return Err(fail),
    };
    let id = workflow.get("id").and_then(Value::as_u64);
    let name = workflow.get("name").and_then(Value::as_str);
    let url = workflow.get("html_url").and_then(Value::as_str);
    let (Some(id), Some(name), Some(url)) = (id, name, url) else {
        return Err(Fail::Client(
            error(GitHubClientErrorCode::InvalidResponse),
            None,
        ));
    };
    let id_text = id.to_string();
    client
        .write(
            Method::POST,
            &[
                "repos",
                &owner,
                &repo,
                "actions",
                "workflows",
                &id_text,
                "dispatches",
            ],
            Some(&json!({"ref": reference, "inputs": inputs})),
        )
        .await?;
    Ok(json!({
        "success": true,
        "message": format!("Workflow '{name}' triggered successfully on ref '{reference}'"),
        "workflow_id": id,
        "workflow_name": name,
        "workflow_url": url,
        "ref": reference,
        "inputs": inputs
    }))
}

// ----- patches -------------------------------------------------------------------------

#[allow(
    clippy::unnecessary_wraps,
    reason = "the tools return `Outcome`; a patch refusal is an Ok result"
)]
fn patch_error(error: &str, message: impl Into<String>) -> Outcome {
    Ok(json!({"error": error, "message": message.into()}))
}

async fn apply_git_patch_tool(client: &GitHubClient, arguments: &Map<String, Value>) -> Outcome {
    let patch = optional_str(arguments, "patch_content")?
        .ok_or_else(|| argument("`patch_content` is required"))?;
    let branch = required_str(arguments, "branch")?;
    let message = match optional_str(arguments, "commit_message")? {
        Some(message) if !message.trim().is_empty() => message,
        _ => "Apply git patch",
    };
    apply_patch(client, patch, branch, message, arguments).await
}

async fn apply_git_patch_from_file(
    client: &GitHubClient,
    office: Option<&SharedOfficeToolHost>,
    arguments: &Map<String, Value>,
) -> Outcome {
    let bucket = required_str(arguments, "bucket_name")?;
    let file = required_str(arguments, "file_name")?;
    let branch = required_str(arguments, "branch")?;
    let message = match optional_str(arguments, "commit_message")? {
        Some(message) if !message.trim().is_empty() => message,
        _ => "Apply git patch from artifact",
    };
    let Some(office) = office else {
        return patch_error(
            "Patch file not available",
            "Unable to download patch file: this runtime has no access to the platform's file store.",
        );
    };
    let bytes = match office
        .read_object(bucket, file.trim_start_matches('/'), MAX_PATCH_BYTES as u64)
        .await
    {
        Ok(bytes) => bytes,
        Err(error) => {
            return match error.kind() {
                OfficeFailure::NotFound => patch_error(
                    "Patch file not found",
                    format!("Patch file '{file}' not found in bucket '{bucket}'."),
                ),
                OfficeFailure::TooLarge => patch_error(
                    "Patch file too large",
                    format!(
                        "Patch file '{file}' is larger than the {MAX_PATCH_BYTES} bytes this runtime applies."
                    ),
                ),
                OfficeFailure::AccessRefused => patch_error(
                    "Unable to download patch file",
                    "Unable to download patch file: the platform refused access to the file store.",
                ),
                _ => patch_error(
                    "Unable to download patch file",
                    "Unable to download patch file: the platform could not be reached.",
                ),
            };
        }
    };
    if bytes.is_empty() {
        return patch_error(
            "Patch file not found",
            format!("Patch file '{file}' not found in bucket '{bucket}'."),
        );
    }
    let Ok(patch) = String::from_utf8(bytes) else {
        return patch_error(
            "Invalid patch content",
            format!("Patch file '{file}' contains invalid content type."),
        );
    };
    apply_patch(client, &patch, branch, message, arguments).await
}

/// One planned write of a patch, fully checked.
enum Planned {
    Create {
        path: String,
        content: String,
    },
    Delete {
        path: String,
        sha: String,
    },
    Modify {
        path: String,
        sha: String,
        content: String,
    },
    Rename {
        old_path: String,
        path: String,
        old_sha: String,
        content: String,
    },
}

async fn apply_patch(
    client: &GitHubClient,
    patch: &str,
    branch: &str,
    commit_message: &str,
    arguments: &Map<String, Value>,
) -> Outcome {
    branch_name("branch", branch)?;
    let base = base_branch(client);
    if branch == base {
        return patch_error("Cannot apply patch", protected(base));
    }
    if patch.len() > MAX_PATCH_BYTES {
        return Err(argument(format!(
            "`patch_content` is {} bytes, over the {MAX_PATCH_BYTES} this tool applies",
            patch.len()
        )));
    }
    let (owner, repo) = repository(client, arguments)?;
    let changes = parse_patch(patch);
    if changes.is_empty() {
        return patch_error(
            "No valid changes found",
            "The patch content does not contain any valid file changes.",
        );
    }
    if changes.len() > MAX_PATCH_FILES {
        return Err(argument(format!(
            "the patch changes {} files, over the {MAX_PATCH_FILES} this tool applies at once",
            changes.len()
        )));
    }
    // A path in a diff is as untrusted as an argument: `a/../x` or an empty
    // segment would address another file in the contents URL. Nothing is read
    // or written until every path is clean.
    for change in &changes {
        file_path("a file path in the patch", &change.path)?;
        file_path("a file path in the patch", &change.old_path)?;
    }
    // Each file is planned from the branch as it is now, so a patch that
    // touches one path twice (two diffs of it, a rename whose target is also
    // modified, a delete and a create) cannot be applied in order. Refused
    // whole, before anything is read or written.
    let mut touched: Vec<&str> = Vec::with_capacity(changes.len() + 1);
    for change in &changes {
        let paths = if change.operation == Operation::Rename {
            vec![change.old_path.as_str(), change.path.as_str()]
        } else {
            vec![change.path.as_str()]
        };
        for path in paths {
            if touched.contains(&path) {
                return Err(argument(format!(
                    "the patch changes `{path}` more than once; split it into separate patches, one per change to that file"
                )));
            }
            touched.push(path);
        }
    }

    // Phase 1: read every file the patch touches and check that it applies.
    // Nothing is written until every file has passed.
    let mut planned = Vec::with_capacity(changes.len());
    let mut failed = Vec::new();
    for change in &changes {
        match plan_change(client, &owner, &repo, branch, change).await? {
            Ok(step) => planned.push(step),
            Err(sentence) => failed.push(sentence),
        }
    }
    if !failed.is_empty() {
        return Ok(patch_result(
            &[],
            &failed,
            changes.len(),
            &format!(
                "Patch not applied: {} of {} file(s) do not match the branch, so nothing was changed.",
                failed.len(),
                changes.len()
            ),
        ));
    }

    // Phase 2: one commit per file, in patch order. A write that fails stops
    // the rest, and the answer lists what was applied and what was not tried.
    let mut applied = Vec::new();
    let mut failures = Vec::new();
    let mut stopped = false;
    for step in &planned {
        let label = step_label(step);
        if stopped {
            failures.push(format!("Not attempted: {label}"));
            continue;
        }
        match write_step(client, &owner, &repo, branch, commit_message, step).await {
            Ok(done) => applied.push(done),
            Err(fail) => {
                failures.push(format!("Failed to apply {label}: {}", fail_sentence(&fail)));
                stopped = true;
            }
        }
    }
    let message = if failures.is_empty() {
        format!(
            "Patch successfully applied. {} changes made.",
            applied.len()
        )
    } else {
        format!(
            "Patch partially applied. {} successful, {} failed or not attempted. Check the branch before applying the rest.",
            applied.len(),
            failures.len()
        )
    };
    Ok(patch_result(&applied, &failures, changes.len(), &message))
}

fn patch_result(applied: &[String], failed: &[String], total: usize, message: &str) -> Value {
    json!({
        "success": !applied.is_empty(),
        "applied_changes": applied,
        "failed_changes": failed,
        "total_changes": total,
        "successful_changes": applied.len(),
        "failed_count": failed.len(),
        "message": message
    })
}

fn step_label(step: &Planned) -> String {
    match step {
        Planned::Create { path, .. } => format!("create {path}"),
        Planned::Delete { path, .. } => format!("delete {path}"),
        Planned::Modify { path, .. } => format!("modify {path}"),
        Planned::Rename { old_path, path, .. } => format!("rename {old_path} to {path}"),
    }
}

fn fail_sentence(fail: &Fail) -> String {
    let because = fail
        .reason()
        .map(|reason| format!(" ({reason})"))
        .unwrap_or_default();
    match fail.code() {
        Some(GitHubClientErrorCode::Authorization) => {
            format!("GitHub refused the write (403){because}")
        }
        Some(GitHubClientErrorCode::Conflict | GitHubClientErrorCode::InvalidInput) => {
            format!("the file changed or already exists on the branch{because}")
        }
        Some(GitHubClientErrorCode::UnknownOutcome) => {
            "GitHub did not confirm the change; it may have been made".to_owned()
        }
        Some(GitHubClientErrorCode::NotFound) => format!("not found on the branch{because}"),
        Some(GitHubClientErrorCode::RateLimited) => "GitHub rate limited the request".to_owned(),
        Some(GitHubClientErrorCode::Authentication) => "GitHub rejected the credentials".to_owned(),
        _ => "GitHub could not be reached or refused the request".to_owned(),
    }
}

/// Reads what one change needs and checks it. The inner error is the
/// per-file sentence of a patch that does not apply; the outer one is a
/// failure that stops the whole tool (credentials, an outage).
async fn plan_change(
    client: &GitHubClient,
    owner: &str,
    repo: &str,
    branch: &str,
    change: &FileChange,
) -> Result<Result<Planned, String>, Fail> {
    match change.operation {
        Operation::Create => {
            if existing(client, owner, repo, &change.path, branch)
                .await?
                .is_some()
            {
                return Ok(Err(format!(
                    "Failed to process {}: the file already exists on the branch",
                    change.path
                )));
            }
            Ok(Ok(Planned::Create {
                path: change.path.clone(),
                content: created_content(change),
            }))
        }
        Operation::Delete => {
            let Some(value) = existing(client, owner, repo, &change.path, branch).await? else {
                return Ok(Err(format!(
                    "Failed to delete {} - file not found",
                    change.path
                )));
            };
            match value.get("sha").and_then(Value::as_str) {
                Some(sha) if valid_commit_id(sha) => Ok(Ok(Planned::Delete {
                    path: change.path.clone(),
                    sha: sha.to_ascii_lowercase(),
                })),
                _ => Ok(Err(format!(
                    "Failed to delete {} - not a file",
                    change.path
                ))),
            }
        }
        Operation::Modify | Operation::Rename => {
            let source = if change.operation == Operation::Rename {
                &change.old_path
            } else {
                &change.path
            };
            let verb = if change.operation == Operation::Rename {
                "rename"
            } else {
                "modify"
            };
            let Some(value) = existing(client, owner, repo, source, branch).await? else {
                return Ok(Err(format!("Failed to {verb} {source}: file not found")));
            };
            let state = match file_state(&value, source) {
                Ok(state) => state,
                Err(problem) => {
                    return Ok(Err(format!(
                        "Failed to {verb} {source}: {}",
                        problem.sentence(source)
                    )));
                }
            };
            let updated = match apply_hunks(&state.content, &change.hunks) {
                Ok(updated) => updated,
                Err(error) => return Ok(Err(format!("Failed to {verb} {source}: {error}"))),
            };
            if change.operation == Operation::Modify {
                if updated == state.content {
                    return Ok(Err(format!(
                        "No changes applied to {source} - patch may not match current content"
                    )));
                }
                return Ok(Ok(Planned::Modify {
                    path: change.path.clone(),
                    sha: state.sha,
                    content: updated,
                }));
            }
            if existing(client, owner, repo, &change.path, branch)
                .await?
                .is_some()
            {
                return Ok(Err(format!(
                    "Failed to rename {source} to {}: the target already exists on the branch",
                    change.path
                )));
            }
            Ok(Ok(Planned::Rename {
                old_path: change.old_path.clone(),
                path: change.path.clone(),
                old_sha: state.sha,
                content: updated,
            }))
        }
    }
}

async fn write_step(
    client: &GitHubClient,
    owner: &str,
    repo: &str,
    branch: &str,
    commit_message: &str,
    step: &Planned,
) -> Result<String, Fail> {
    match step {
        Planned::Create { path, content } => {
            put_file(
                client,
                owner,
                repo,
                path,
                &format!("{commit_message} - Create {path}"),
                content.as_bytes(),
                branch,
                None,
            )
            .await?;
            Ok(format!("Created: {path}"))
        }
        Planned::Delete { path, sha } => {
            let body = json!({"message": format!("{commit_message} - Delete {path}"), "sha": sha, "branch": branch});
            client
                .write(
                    Method::DELETE,
                    &contents_path(owner, repo, path),
                    Some(&body),
                )
                .await?;
            Ok(format!("Deleted: {path}"))
        }
        Planned::Modify { path, sha, content } => {
            put_file(
                client,
                owner,
                repo,
                path,
                &format!("{commit_message} - Update {path}"),
                content.as_bytes(),
                branch,
                Some(sha),
            )
            .await?;
            Ok(format!("Modified: {path}"))
        }
        Planned::Rename {
            old_path,
            path,
            old_sha,
            content,
        } => {
            // The new file first: a failure after it leaves a copy, never a
            // loss.
            put_file(
                client,
                owner,
                repo,
                path,
                &format!("{commit_message} - Rename {old_path} to {path}"),
                content.as_bytes(),
                branch,
                None,
            )
            .await?;
            let body = json!({"message": format!("{commit_message} - Remove old file {old_path}"), "sha": old_sha, "branch": branch});
            client
                .write(Method::DELETE, &contents_path(owner, repo, old_path), Some(&body))
                .await
                .map_err(|fail| match fail {
                    Fail::Client(error, reason) => Fail::Client(error, Some(format!("the new file {path} was created but the old file {old_path} was not removed{}", reason.map(|reason| format!("; GitHub said: {reason}")).unwrap_or_default()))),
                    other @ Fail::Argument(_) => other,
                })?;
            Ok(format!("Renamed: {old_path} \u{2192} {path}"))
        }
    }
}
