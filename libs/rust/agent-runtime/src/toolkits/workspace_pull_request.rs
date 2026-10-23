//! The cloud executor's `open_pull_request` host tool (ADR-0034 decision
//! 10).
//!
//! It exists only on a cloud turn of a handed-off thread that has
//! `refs/elitea/upstream`, and only when the agent has a GitHub toolkit
//! bound. Its arguments are `toolkit`, `title`, `body` and `branch`; the diff
//! is NOT one of them. The host's [`WorkspaceDiffSource`] fetches it from Main
//! (refs/elitea/upstream → refs/elitea/cloud) and it is handed to the GitHub
//! family's patch code in process. With the toolkit's server-side
//! credentials, the tool then:
//!
//! 1. creates a branch from the recorded upstream commit (`create_branch`
//!    with `from_sha`), using the name GitHub accepted (`_vN` when taken);
//! 2. applies the diff onto it (one commit per file, every file checked
//!    first; a diff with binary changes is refused before any write);
//! 3. opens the pull request with `base` = the recorded upstream branch.
//!
//! A write that fails partway leaves the branch and opens no pull request;
//! the answer names the branch. A folder whose upstream is another
//! repository than the toolkit's is refused.

use std::sync::Arc;

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use super::families::github::client::GitHubClient;
use super::families::github::config::GitHubToolkitConfig;
use super::families::github::writes::{
    PullRequestFromDiff, PullRequestFromDiffOutcome, open_pull_request_from_diff,
};
use super::snapshot::{AdmittedToolSnapshot, FrozenToolKind};

/// The tool's name.
pub const OPEN_PULL_REQUEST: &str = "open_pull_request";
const TOOLSET_NAME: &str = "workspace_pull_request";
const MAX_TITLE_CHARS: usize = 256;
const MAX_BODY_BYTES: usize = 64 * 1_024;

/// The diff Main computed and what the pull request is opened against.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct WorkspaceDiff {
    pub upstream_sha: String,
    pub upstream_branch: String,
    /// The desktop's remote URL, without credentials.
    pub remote_url: String,
    pub text: String,
}

/// Why there is no diff to open a pull request with.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum DiffUnavailable {
    NoUpstream,
    TooLarge,
    Binary,
    Empty,
    /// The store could not answer; the message is data-free.
    Failed(&'static str),
}

impl DiffUnavailable {
    fn sentence(self) -> &'static str {
        match self {
            Self::NoUpstream => {
                "This folder has no upstream branch, so no pull request can be opened from the cloud."
            }
            Self::TooLarge => {
                "The change is over the 1 MiB or 100 files a pull request from the cloud carries; nothing was written."
            }
            Self::Binary => {
                "The change has binary files, which a pull request from the cloud cannot carry; nothing was written."
            }
            Self::Empty => "There is no change to open a pull request with.",
            Self::Failed(message) => message,
        }
    }
}

/// Where the tool gets the diff: Main's `WorkspacePullRequestDiff`, over the
/// runtime plane, under the turn's claim.
#[async_trait]
pub trait WorkspaceDiffSource: Send + Sync {
    async fn pull_request_diff(&self) -> Result<WorkspaceDiff, DiffUnavailable>;
}

/// An opened pull request, for the `pull_request.opened` audit event:
/// identifiers and counts, never file contents.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PullRequestOpened {
    pub toolkit: String,
    pub repository: String,
    pub branch: String,
    pub base: String,
    pub number: u64,
    pub files: usize,
    pub upstream_sha: String,
}

/// Records `pull_request.opened`.
pub trait PullRequestAudit: Send + Sync {
    fn opened(&self, event: &PullRequestOpened);
}

/// One bound GitHub toolkit the tool may open a pull request through.
pub struct GitHubPullRequestTarget {
    toolkit_name: String,
    repository: String,
    client: Arc<GitHubClient>,
}

impl GitHubPullRequestTarget {
    #[must_use]
    pub fn toolkit_name(&self) -> &str {
        &self.toolkit_name
    }

    #[must_use]
    pub fn repository(&self) -> &str {
        &self.repository
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test(
        name: &str,
        repository: &str,
        client: Arc<GitHubClient>,
    ) -> Self {
        Self {
            toolkit_name: name.to_owned(),
            repository: repository.to_owned(),
            client,
        }
    }
}

/// The GitHub toolkits of a turn's frozen snapshot, each with its own
/// client (its own credentials, never shared with the toolkit's toolset).
/// A toolkit whose settings do not parse is left out.
#[must_use]
pub fn github_pull_request_targets(
    snapshot: &AdmittedToolSnapshot<'_>,
) -> Vec<GitHubPullRequestTarget> {
    snapshot
        .iter()
        .filter(|reference| {
            reference.kind() == FrozenToolKind::Configured && reference.tool_type() == "github"
        })
        .filter_map(|reference| {
            let settings = reference.settings()?;
            let config = GitHubToolkitConfig::parse(settings).ok()?;
            let repository = config.repository().to_owned();
            let client = GitHubClient::new(config).ok()?;
            Some(GitHubPullRequestTarget {
                toolkit_name: reference.toolkit_name().to_owned(),
                repository,
                client: Arc::new(client),
            })
        })
        .collect()
}

/// `owner/repo` of a remote URL (`https://host/owner/repo(.git)`,
/// `ssh://git@host/owner/repo.git`, `host:owner/repo.git`), lowercased.
fn remote_repository(remote: &str) -> Option<String> {
    let trimmed = remote.trim().trim_end_matches('/');
    let without_suffix = trimmed.strip_suffix(".git").unwrap_or(trimmed);
    let path = match without_suffix.split_once("://") {
        Some((_, rest)) => rest.split_once('/').map(|(_, path)| path)?,
        None => without_suffix.split_once(':').map(|(_, path)| path)?,
    };
    let mut parts = path.rsplitn(3, '/');
    let repo = parts.next()?;
    let owner = parts.next()?;
    (!owner.is_empty() && !repo.is_empty()).then(|| format!("{owner}/{repo}").to_ascii_lowercase())
}

/// The tool's toolset, or `None` when no GitHub toolkit is bound (the tool
/// is then absent). The host offers it only when the runtime context says
/// the thread has an upstream.
#[must_use]
pub fn open_pull_request_toolset(
    targets: Vec<GitHubPullRequestTarget>,
    source: Arc<dyn WorkspaceDiffSource>,
    audit: Arc<dyn PullRequestAudit>,
) -> Option<Arc<dyn Toolset>> {
    if targets.is_empty() {
        return None;
    }
    let tool: Arc<dyn Tool> = Arc::new(OpenPullRequestTool {
        targets,
        source,
        audit,
    });
    Some(Arc::new(PullRequestToolset { tools: vec![tool] }))
}

struct PullRequestToolset {
    tools: Vec<Arc<dyn Tool>>,
}

#[async_trait]
impl Toolset for PullRequestToolset {
    fn name(&self) -> &str {
        TOOLSET_NAME
    }

    async fn tools(&self, _ctx: Arc<dyn ReadonlyContext>) -> adk_core::Result<Vec<Arc<dyn Tool>>> {
        Ok(self.tools.clone())
    }
}

struct OpenPullRequestTool {
    targets: Vec<GitHubPullRequestTarget>,
    source: Arc<dyn WorkspaceDiffSource>,
    audit: Arc<dyn PullRequestAudit>,
}

fn argument<'a>(arguments: &'a Map<String, Value>, key: &str) -> Option<&'a str> {
    arguments.get(key).and_then(Value::as_str).map(str::trim)
}

fn answer(status: &str, message: impl Into<String>) -> Value {
    json!({"status": status, "message": message.into()})
}

#[async_trait]
impl Tool for OpenPullRequestTool {
    fn name(&self) -> &str {
        OPEN_PULL_REQUEST
    }

    fn description(&self) -> &'static str {
        "Open a GitHub pull request with this cloud thread's changes: everything that differs from the folder's upstream branch. The change is taken from the workspace, not from an argument. Creates a new branch from the upstream commit (a suffix is added when the name is taken), commits the changes there and opens the pull request against the upstream branch."
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "toolkit": {"type": "string", "description": "The GitHub toolkit to open it through (its name). Optional when only one is bound.", "enum": self.targets.iter().map(|target| target.toolkit_name.clone()).collect::<Vec<_>>()},
                "title": {"type": "string", "minLength": 1, "maxLength": MAX_TITLE_CHARS},
                "body": {"type": "string", "maxLength": MAX_BODY_BYTES},
                "branch": {"type": "string", "minLength": 1, "maxLength": 200, "description": "The name of the new branch, e.g. `feature/cloud-change`."}
            },
            "required": ["title", "branch"],
            "additionalProperties": false
        }))
    }

    fn is_read_only(&self) -> bool {
        false
    }

    async fn execute(&self, _ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        Ok(self.run(&args).await)
    }
}

impl OpenPullRequestTool {
    async fn run(&self, args: &Value) -> Value {
        let Some(arguments) = args.as_object() else {
            return answer("error", "The arguments must be an object.");
        };
        let target = match argument(arguments, "toolkit") {
            Some(name) if !name.is_empty() => self
                .targets
                .iter()
                .find(|target| target.toolkit_name == name),
            _ if self.targets.len() == 1 => self.targets.first(),
            _ => None,
        };
        let Some(target) = target else {
            return answer(
                "error",
                "Name the GitHub toolkit to open the pull request through (`toolkit`).",
            );
        };
        let (Some(title), Some(branch)) =
            (argument(arguments, "title"), argument(arguments, "branch"))
        else {
            return answer("error", "`title` and `branch` are required.");
        };
        if title.is_empty() || title.chars().count() > MAX_TITLE_CHARS || branch.is_empty() {
            return answer(
                "error",
                "`title` and `branch` must be non-empty and bounded.",
            );
        }
        let body = argument(arguments, "body").unwrap_or_default();
        if body.len() > MAX_BODY_BYTES {
            return answer("error", "`body` is too long.");
        }
        let diff = match self.source.pull_request_diff().await {
            Ok(diff) => diff,
            Err(unavailable) => return answer("refused", unavailable.sentence()),
        };
        if remote_repository(&diff.remote_url).as_deref()
            != Some(target.repository.to_ascii_lowercase().as_str())
        {
            return answer(
                "refused",
                format!(
                    "This folder's upstream is not {}; open the pull request through the toolkit of that repository.",
                    target.repository
                ),
            );
        }
        let outcome = open_pull_request_from_diff(
            &target.client,
            &PullRequestFromDiff {
                title,
                body,
                branch,
                base: &diff.upstream_branch,
                from_sha: &diff.upstream_sha,
                diff: &diff.text,
            },
        )
        .await;
        match outcome {
            Ok(PullRequestFromDiffOutcome::Opened {
                branch,
                number,
                url,
                files,
            }) => {
                self.audit.opened(&PullRequestOpened {
                    toolkit: target.toolkit_name.clone(),
                    repository: target.repository.clone(),
                    branch: branch.clone(),
                    base: diff.upstream_branch.clone(),
                    number,
                    files,
                    upstream_sha: diff.upstream_sha.clone(),
                });
                json!({
                    "status": "ok",
                    "message": format!("Pull request #{number} opened: {url}"),
                    "pull_request": number, "url": url, "branch": branch,
                    "base": diff.upstream_branch, "files": files,
                })
            }
            Ok(PullRequestFromDiffOutcome::Refused { reason }) => answer("refused", reason),
            Ok(PullRequestFromDiffOutcome::Partial {
                branch,
                applied,
                failed,
                reason,
            }) => json!({
                "status": "partial",
                "message": format!("No pull request was opened: {reason}. The branch `{branch}` keeps the {} change(s) already written; a retry uses a fresh branch.", applied.len()),
                "branch": branch, "applied_changes": applied, "failed_changes": failed,
            }),
            Err(sentence) => answer("error", format!("No pull request was opened: {sentence}.")),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::remote_repository;

    #[test]
    fn remote_urls_name_their_repository() {
        for (url, want) in [
            (
                "https://github.com/Elitea-NG/elitea-platform.git",
                "elitea-ng/elitea-platform",
            ),
            ("https://github.com/o/r", "o/r"),
            ("git@github.com:o/r.git", "o/r"),
            ("ssh://git@github.com/o/r.git", "o/r"),
        ] {
            assert_eq!(remote_repository(url).as_deref(), Some(want), "{url}");
        }
        assert_eq!(remote_repository("not a url"), None);
    }
}
