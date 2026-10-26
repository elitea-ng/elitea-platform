//! The cloud executor's `open_pull_request` host tool (ADR-0034 decision
//! 10).
//!
//! It exists only on a cloud turn of a handed-off thread that has
//! `refs/elitea/upstream`, and only when the policy allows pull requests and
//! there is a credential to open one with: the GitHub configuration the
//! thread's owner chose for the thread (amendment 2026-10-26, the
//! [`THREAD_CREDENTIAL`] target), or a bound GitHub toolkit. Its arguments
//! are `toolkit`, `title`, `body` and `branch`; the diff
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
//! repository than the toolkit's is refused. The thread's credential is
//! scoped by the upstream itself: the pull request goes to the upstream
//! remote's repository, and a credential for another GitHub host than the
//! remote's is refused. The credential is redeemed from Main only when the
//! tool runs, kept in memory for that call and never reaches the sandbox,
//! the model, a log or the audit.

use std::sync::Arc;

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use serde_json::{Map, Value, json};
use zeroize::Zeroizing;

use super::families::github::client::GitHubClient;
use super::families::github::config::GitHubToolkitConfig;
use super::families::github::writes::{
    PullRequestFromDiff, PullRequestFromDiffOutcome, open_pull_request_from_diff,
};
use super::snapshot::{AdmittedToolSnapshot, FrozenToolKind};

/// The tool's name.
pub const OPEN_PULL_REQUEST: &str = "open_pull_request";
/// The `toolkit` value naming the thread's own pull-request credential. Not
/// a valid toolkit name, so it never shadows one.
pub const THREAD_CREDENTIAL: &str = "@thread";
/// The most bytes of a redeemed GitHub configuration.
const MAX_CREDENTIAL_BYTES: usize = 32 * 1_024;
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
    /// The toolkit, or [`THREAD_CREDENTIAL`].
    pub toolkit: String,
    pub repository: String,
    pub branch: String,
    pub base: String,
    pub number: u64,
    /// The pull request's web URL, as GitHub answered it.
    pub url: String,
    pub files: usize,
    pub upstream_sha: String,
}

/// Records `pull_request.opened`.
pub trait PullRequestAudit: Send + Sync {
    fn opened(&self, event: &PullRequestOpened);
}

/// Why the thread's credential cannot be used. Data-free.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CredentialUnavailable {
    /// The owner has not chosen one (or cleared it).
    NotChosen,
    /// The policy no longer allows pull requests from the cloud.
    Disabled,
    /// The configuration no longer resolves to a GitHub configuration.
    Invalid,
    /// Main could not answer.
    Failed,
}

impl CredentialUnavailable {
    const fn sentence(self) -> &'static str {
        match self {
            Self::NotChosen => {
                "This thread has no GitHub credential for pull requests; its owner can choose one in the thread's cloud settings."
            }
            Self::Disabled => "Pull requests from the cloud are turned off for this project.",
            Self::Invalid => {
                "The thread's GitHub credential is no longer a GitHub configuration of the project; its owner can choose another."
            }
            Self::Failed => "The thread's GitHub credential could not be read; try again later.",
        }
    }
}

/// Where the thread's credential comes from: Main's claim-bound
/// `WorkspaceForgeCredential`, redeemed per call.
#[async_trait]
pub trait ThreadCredentialSource: Send + Sync {
    /// The GitHub configuration's data (a JSON object: `base_url` and the
    /// redeemed secret fields), as text that is zeroized on drop.
    async fn github_configuration(&self) -> Result<Zeroizing<String>, CredentialUnavailable>;
}

type ClientFactory = Arc<dyn Fn(GitHubToolkitConfig) -> Option<GitHubClient> + Send + Sync>;

/// The thread's own pull-request credential (the [`THREAD_CREDENTIAL`]
/// target). Offered when the runtime context says the owner chose one.
pub struct ThreadCredential {
    source: Arc<dyn ThreadCredentialSource>,
    client: ClientFactory,
}

impl ThreadCredential {
    #[must_use]
    pub fn new(source: Arc<dyn ThreadCredentialSource>) -> Self {
        Self {
            source,
            client: Arc::new(|config| GitHubClient::new(config).ok()),
        }
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test(
        source: Arc<dyn ThreadCredentialSource>,
        client: ClientFactory,
    ) -> Self {
        Self { source, client }
    }

    /// A client for the upstream's repository, or the sentence refusing it.
    async fn client(&self, diff: &WorkspaceDiff) -> Result<(String, Arc<GitHubClient>), String> {
        let Some(repository) = remote_repository_as_written(&diff.remote_url) else {
            return Err(
                "This folder's upstream remote does not name a repository, so no pull request can be opened.".to_owned(),
            );
        };
        let text = self
            .source
            .github_configuration()
            .await
            .map_err(|unavailable| unavailable.sentence().to_owned())?;
        let unusable = || {
            "The thread's GitHub credential is not usable; its owner can choose another.".to_owned()
        };
        if text.len() > MAX_CREDENTIAL_BYTES {
            return Err(unusable());
        }
        let Ok(Value::Object(configuration)) = serde_json::from_str::<Value>(&text) else {
            return Err(unusable());
        };
        drop(text);
        let mut settings = Map::new();
        settings.insert(
            "github_configuration".to_owned(),
            Value::Object(configuration),
        );
        settings.insert("repository".to_owned(), Value::String(repository.clone()));
        settings.insert(
            "active_branch".to_owned(),
            Value::String(diff.upstream_branch.clone()),
        );
        settings.insert(
            "base_branch".to_owned(),
            Value::String(diff.upstream_branch.clone()),
        );
        let config = GitHubToolkitConfig::parse(&settings);
        // The parsed config holds its own zeroizing copy; the map goes now.
        drop(settings);
        let config = config.map_err(|_| unusable())?;
        if let (Some(remote), Some(api)) = (
            remote_host(&diff.remote_url),
            config.base_url().host_str().map(api_host),
        ) {
            // A remote host without a dot is an SSH alias (`github-work:o/r`),
            // which names no host to compare: the repository decides.
            if remote.contains('.') && remote != api {
                return Err(format!(
                    "The thread's GitHub credential is for {api}, but this folder's upstream is on {remote}; its owner can choose a credential for {remote}."
                ));
            }
        }
        let client = (self.client)(config).ok_or_else(unusable)?;
        Ok((repository, Arc::new(client)))
    }
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

/// The host of a remote URL, lowercased, without user or port.
fn remote_host(remote: &str) -> Option<String> {
    let trimmed = remote.trim();
    let authority = match trimmed.split_once("://") {
        Some((_, rest)) => rest.split('/').next()?,
        None => trimmed.split_once(':')?.0,
    };
    let host = authority.rsplit('@').next()?.split(':').next()?;
    (!host.is_empty()).then(|| host.to_ascii_lowercase())
}

/// The web host of a GitHub API base URL: `api.github.com` is `github.com`;
/// GitHub Enterprise's `https://host/api/v3` is `host`.
fn api_host(base_host: &str) -> String {
    let host = base_host.to_ascii_lowercase();
    if host == "api.github.com" {
        "github.com".to_owned()
    } else {
        host
    }
}

/// `owner/repo` of a remote URL (`https://host/owner/repo(.git)`,
/// `ssh://git@host/owner/repo.git`, `host:owner/repo.git`), lowercased.
fn remote_repository(remote: &str) -> Option<String> {
    remote_repository_as_written(remote).map(|repository| repository.to_ascii_lowercase())
}

/// `owner/repo` of a remote URL, in the remote's own case.
fn remote_repository_as_written(remote: &str) -> Option<String> {
    let trimmed = remote.trim().trim_end_matches('/');
    let without_suffix = trimmed.strip_suffix(".git").unwrap_or(trimmed);
    let path = match without_suffix.split_once("://") {
        Some((_, rest)) => rest.split_once('/').map(|(_, path)| path)?,
        None => without_suffix.split_once(':').map(|(_, path)| path)?,
    };
    let mut parts = path.rsplitn(3, '/');
    let repo = parts.next()?;
    let owner = parts.next()?;
    (!owner.is_empty() && !repo.is_empty()).then(|| format!("{owner}/{repo}"))
}

/// The tool's toolset, or `None` when there is neither a thread credential
/// nor a bound GitHub toolkit (the tool is then absent). The host offers it
/// only when the runtime context says the thread has an upstream and the
/// policy allows pull requests.
#[must_use]
pub fn open_pull_request_toolset(
    thread: Option<ThreadCredential>,
    targets: Vec<GitHubPullRequestTarget>,
    source: Arc<dyn WorkspaceDiffSource>,
    audit: Arc<dyn PullRequestAudit>,
) -> Option<Arc<dyn Toolset>> {
    if thread.is_none() && targets.is_empty() {
        return None;
    }
    let tool: Arc<dyn Tool> = Arc::new(OpenPullRequestTool {
        thread,
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
    thread: Option<ThreadCredential>,
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
        "Open a GitHub pull request with this cloud thread's changes: everything that differs from the folder's upstream branch. The change is taken from the workspace, not from an argument. Creates a new branch from the upstream commit (a suffix is added when the name is taken; an existing branch is never overwritten), commits the changes there and opens the pull request against the upstream branch. A person may be asked to approve it first."
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "toolkit": {"type": "string", "description": "What to open it through: `@thread` (the thread's own GitHub credential, the default when it has one) or a GitHub toolkit's name. Optional when there is only one choice.", "enum": self.choices()},
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

enum Choice<'a> {
    Thread(&'a ThreadCredential),
    Toolkit(&'a GitHubPullRequestTarget),
}

impl OpenPullRequestTool {
    fn choices(&self) -> Vec<String> {
        self.thread
            .iter()
            .map(|_| THREAD_CREDENTIAL.to_owned())
            .chain(
                self.targets
                    .iter()
                    .map(|target| target.toolkit_name.clone()),
            )
            .collect()
    }

    fn choose(&self, name: Option<&str>) -> Option<Choice<'_>> {
        match name {
            Some(THREAD_CREDENTIAL) => self.thread.as_ref().map(Choice::Thread),
            Some(name) if !name.is_empty() => self
                .targets
                .iter()
                .find(|target| target.toolkit_name == name)
                .map(Choice::Toolkit),
            // The owner's choice for the thread comes first.
            _ => match (&self.thread, self.targets.as_slice()) {
                (Some(thread), _) => Some(Choice::Thread(thread)),
                (None, [only]) => Some(Choice::Toolkit(only)),
                (None, _) => None,
            },
        }
    }

    #[allow(
        clippy::too_many_lines,
        reason = "one ordered flow: check, diff, credential, open, audit"
    )]
    async fn run(&self, args: &Value) -> Value {
        let Some(arguments) = args.as_object() else {
            return answer("error", "The arguments must be an object.");
        };
        let Some(choice) = self.choose(argument(arguments, "toolkit")) else {
            return answer(
                "error",
                "Name what to open the pull request through (`toolkit`): `@thread` or a GitHub toolkit.",
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
        // The new branch is created, never moved: it cannot be the branch
        // the pull request goes into.
        if branch
            .trim_start_matches("refs/heads/")
            .eq_ignore_ascii_case(&diff.upstream_branch)
        {
            return answer(
                "refused",
                format!(
                    "The new branch must not be the upstream branch `{}`; name a new one.",
                    diff.upstream_branch
                ),
            );
        }
        let (toolkit, repository, client) = match choice {
            Choice::Toolkit(target) => {
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
                (
                    target.toolkit_name.clone(),
                    target.repository.clone(),
                    Arc::clone(&target.client),
                )
            }
            Choice::Thread(thread) => match thread.client(&diff).await {
                Ok((repository, client)) => (THREAD_CREDENTIAL.to_owned(), repository, client),
                Err(sentence) => return answer("refused", sentence),
            },
        };
        let outcome = open_pull_request_from_diff(
            &client,
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
                    toolkit,
                    repository,
                    branch: branch.clone(),
                    base: diff.upstream_branch.clone(),
                    number,
                    url: url.clone(),
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
    use super::{api_host, remote_host, remote_repository};

    #[test]
    fn remote_and_api_hosts_compare() {
        for (url, want) in [
            ("https://github.com/o/r.git", "github.com"),
            ("https://user@GHE.example.com:8443/o/r", "ghe.example.com"),
            ("git@github.com:o/r.git", "github.com"),
            ("ssh://git@github.com:22/o/r.git", "github.com"),
            ("github-work:o/r.git", "github-work"),
        ] {
            assert_eq!(remote_host(url).as_deref(), Some(want), "{url}");
        }
        assert_eq!(api_host("api.github.com"), "github.com");
        assert_eq!(api_host("GHE.example.com"), "ghe.example.com");
    }

    #[test]
    fn remote_urls_name_their_repository() {
        for (url, want) in [
            (
                "https://github.com/elitea-ng/elitea-platform.git",
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
