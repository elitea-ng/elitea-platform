//! The cloud executor's `git_push` and `open_pull_request` host tools
//! (ADR-0034 decision 10, amendment 2026-10-26).
//!
//! They exist only on a cloud turn of a handed-off thread that has
//! `refs/elitea/upstream`, only when the policy allows pull requests, and
//! only with a credential: the GitHub or GitLab configuration the thread's
//! owner chose for the thread (the [`THREAD_CREDENTIAL`] target), or a bound
//! GitHub or GitLab toolkit. Neither tool takes the change as an argument:
//! the host prepares it from the turn's own repository, outside the sandbox.
//!
//! `git_push` pushes the thread's real commits (the cloud turns' history,
//! binaries included) re-parented onto the recorded upstream commit, as a
//! NEW branch of the upstream remote's repository: never a force push,
//! never an existing branch or the default branch (a taken name gets the
//! next free `_vN` suffix). It runs here, over HTTPS to the credential's own
//! host only ([`super::git_receive_pack`]); the credential is redeemed from
//! Main per call, sent in that call's `Authorization` header and nowhere
//! else: never the sandbox, a file, the model, a log or the audit.
//!
//! `open_pull_request` pushes the same way, then opens a pull request
//! (GitHub) or merge request (GitLab) from that branch into the upstream
//! branch; with `push: false` it opens one from a branch `git_push` pushed
//! earlier in the turn. Where a push is not possible (a GitHub App or
//! anonymous configuration has no git credential, or the host offers no
//! push source) a GitHub pull request falls back to the API path: a branch
//! from the upstream commit and the text diff applied through the contents
//! API, with that path's limits (1 MiB, 100 files, no binary).
//!
//! Scope: the push goes to `https://<the credential's host>/<the remote's
//! repository>.git`, whatever the recorded remote's scheme. An `ssh://` or
//! scp-like remote is mapped to that HTTPS URL; a remote on another host
//! than the credential's is refused, and so is an SSH alias (a host without
//! a dot, such as `github-work:o/r`) unless it is the credential's host
//! itself: an alias names no host to compare, so it is never taken to mean
//! the credential's. A bound toolkit's repository must be the remote's.
//!
//! Every call resolves its target once: the thread's credential is redeemed
//! from Main (which re-checks the policy switches) and scoped to the
//! upstream before anything is prepared, and that one redemption serves the
//! whole call. Nothing secret outlives a call: a branch `git_push` pushed is
//! remembered by its name, upstream and toolkit only, and `open_pull_request`
//! with `push: false` redeems the credential again.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use elitea_connectors::ClientPolicy;
use elitea_connectors::transport::{Transport, Url};
use serde_json::{Map, Value, json};
use zeroize::Zeroizing;

use super::families::connector_client::transport as host_transport;
use super::families::github::client::GitHubClient;
use super::families::github::config::GitHubToolkitConfig;
use super::families::github::writes::{
    PullRequestFromDiff, PullRequestFromDiffOutcome, open_pull_request_from_branch,
    open_pull_request_from_diff,
};
use super::families::gitlab::client::GitLabClient;
use super::families::gitlab::config::GitLabToolkitConfig;
pub use super::git_receive_pack::PushPack;
use super::git_receive_pack::{GitCredential, PushRequest, push_new_branch};
use super::snapshot::{AdmittedToolSnapshot, FrozenToolKind};

/// The pull (merge) request tool's name.
pub const OPEN_PULL_REQUEST: &str = "open_pull_request";
/// The push tool's name.
pub const GIT_PUSH: &str = "git_push";
/// The `toolkit` value naming the thread's own credential. Not a valid
/// toolkit name, so it never shadows one.
pub const THREAD_CREDENTIAL: &str = "@thread";
/// The most bytes of a redeemed configuration.
const MAX_CREDENTIAL_BYTES: usize = 32 * 1_024;
const TOOLSET_NAME: &str = "workspace_pull_request";
const MAX_TITLE_CHARS: usize = 256;
const MAX_BODY_BYTES: usize = 64 * 1_024;
const MAX_BRANCH_BYTES: usize = 200;
/// The push is one upload of up to the segment budget: no retry, a long
/// request timeout.
const PUSH_POLICY: ClientPolicy = ClientPolicy {
    connect_timeout: Duration::from_secs(30),
    request_timeout: Duration::from_mins(15),
    pool_idle_timeout: Duration::from_secs(30),
    max_idle_per_host: 0,
    user_agent: "elitea-cloud-executor",
    disable_retries: true,
};

/// The forge a configuration is for.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ForgeKind {
    GitHub,
    GitLab,
}

impl ForgeKind {
    /// `github` or `gitlab` (the configuration type).
    #[must_use]
    pub fn parse(value: &str) -> Option<Self> {
        match value {
            "github" => Some(Self::GitHub),
            "gitlab" => Some(Self::GitLab),
            _ => None,
        }
    }

    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::GitHub => "github",
            Self::GitLab => "gitlab",
        }
    }

    const fn name(self) -> &'static str {
        match self {
            Self::GitHub => "GitHub",
            Self::GitLab => "GitLab",
        }
    }

    const fn request_noun(self) -> &'static str {
        match self {
            Self::GitHub => "pull request",
            Self::GitLab => "merge request",
        }
    }
}

/// The upstream a change is opened against, as the desktop recorded it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Upstream {
    pub sha: String,
    pub branch: String,
    /// The desktop's remote URL, without credentials.
    pub remote_url: String,
}

/// The diff of the API fallback and what it is opened against.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct WorkspaceDiff {
    pub upstream_sha: String,
    pub upstream_branch: String,
    /// The desktop's remote URL, without credentials.
    pub remote_url: String,
    pub text: String,
}

impl WorkspaceDiff {
    fn upstream(&self) -> Upstream {
        Upstream {
            sha: self.upstream_sha.clone(),
            branch: self.upstream_branch.clone(),
            remote_url: self.remote_url.clone(),
        }
    }
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
                "The change is over the 1 MiB or 100 files a pull request through the GitHub API carries; nothing was written. A credential that can push over git has no such limit."
            }
            Self::Binary => {
                "The change has binary files, which a pull request through the GitHub API cannot carry; nothing was written. A credential that can push over git carries them."
            }
            Self::Empty => "There is no change to open a pull request with.",
            Self::Failed(message) => message,
        }
    }
}

/// Where the API fallback gets the diff: the turn's own repository.
#[async_trait]
pub trait WorkspaceDiffSource: Send + Sync {
    async fn pull_request_diff(&self) -> Result<WorkspaceDiff, DiffUnavailable>;
}

/// The thread's commits re-parented onto the upstream commit, ready to push.
pub struct PreparedPush {
    pub upstream: Upstream,
    /// The new history's tip.
    pub tip: String,
    /// Commits from the upstream commit to `tip`.
    pub commits: u32,
    /// Files that differ from the upstream commit.
    pub files: u32,
    /// A git pack of every object the remote needs beyond the upstream
    /// commit, with room in front for the push command (never copied).
    pub pack: PushPack,
}

impl std::fmt::Debug for PreparedPush {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("PreparedPush")
            .field("upstream", &self.upstream)
            .field("tip", &self.tip)
            .field("commits", &self.commits)
            .field("files", &self.files)
            .field("pack_bytes", &self.pack.len())
            .finish()
    }
}

/// Why there is nothing to push.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PushUnavailable {
    NoUpstream,
    Empty,
    /// Over the segment budget (the bytes one turn may carry).
    TooLarge,
    /// The thread's stored history is damaged: an object is missing or
    /// corrupt (not a size limit; a retry does not help).
    StoreIntegrity,
    /// A tracked file the handoff left out (excluded or over the size cap)
    /// would be deleted or published by the push; nothing was pushed.
    ExcludedTracked,
    /// The workspace could not be read; the message is data-free.
    Failed(&'static str),
}

impl PushUnavailable {
    const fn sentence(self) -> &'static str {
        match self {
            Self::NoUpstream => {
                "This folder has no upstream branch, so nothing can be pushed from the cloud."
            }
            Self::Empty => "There is no change from the upstream commit to push.",
            Self::TooLarge => {
                "The change is over the bytes one cloud push carries; take the thread back and push it from the desktop."
            }
            Self::StoreIntegrity => {
                "The thread's stored history is incomplete or damaged (an object is missing or corrupt), so nothing was pushed; take the thread back and push it from the desktop."
            }
            Self::ExcludedTracked => {
                "A tracked file the handoff left out (excluded or too large) would be lost or published by this push, so nothing was pushed; take the thread back and push it from the desktop."
            }
            Self::Failed(message) => message,
        }
    }
}

/// Where the push comes from: the turn's own repository, prepared by the
/// host outside the sandbox.
#[async_trait]
pub trait WorkspacePushSource: Send + Sync {
    /// The recorded upstream alone (cheap: no pack is built), so a call can
    /// resolve and scope its target before it prepares anything.
    async fn upstream(&self) -> Result<Upstream, PushUnavailable>;
    async fn prepare_push(&self) -> Result<PreparedPush, PushUnavailable>;
}

/// An opened pull (merge) request, for the `pull_request.opened` audit
/// event: identifiers and counts, never file contents.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PullRequestOpened {
    /// The toolkit, or [`THREAD_CREDENTIAL`].
    pub toolkit: String,
    pub repository: String,
    pub branch: String,
    pub base: String,
    pub number: u64,
    /// The request's web URL, as the forge answered it.
    pub url: String,
    pub files: usize,
    pub upstream_sha: String,
}

/// A pushed branch, for the `branch.pushed` audit event.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BranchPushed {
    /// The toolkit, or [`THREAD_CREDENTIAL`].
    pub toolkit: String,
    pub forge: ForgeKind,
    pub repository: String,
    /// The name it was created under.
    pub branch: String,
    /// The upstream branch it sits on.
    pub base: String,
    /// The branch's web page.
    pub url: String,
    pub commits: u32,
    pub upstream_sha: String,
}

/// Records `pull_request.opened` and `branch.pushed`.
pub trait PullRequestAudit: Send + Sync {
    fn opened(&self, event: &PullRequestOpened);
    fn pushed(&self, _event: &BranchPushed) {}
}

/// Why the thread's credential cannot be used. Data-free.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CredentialUnavailable {
    /// The owner has not chosen one (or cleared it).
    NotChosen,
    /// The policy no longer allows pull requests from the cloud.
    Disabled,
    /// The configuration no longer resolves.
    Invalid,
    /// Main could not answer.
    Failed,
}

impl CredentialUnavailable {
    const fn sentence(self) -> &'static str {
        match self {
            Self::NotChosen => {
                "This thread has no GitHub or GitLab credential; its owner can choose one in the thread's cloud settings."
            }
            Self::Disabled => {
                "Pushes and pull requests from the cloud are turned off for this project."
            }
            Self::Invalid => {
                "The thread's credential is no longer a configuration of the project; its owner can choose another."
            }
            Self::Failed => "The thread's credential could not be read; try again later.",
        }
    }
}

/// A redeemed configuration: its forge and its data (a JSON object: the
/// URL and the redeemed secret fields), zeroized on drop.
pub struct ThreadConfiguration {
    pub forge: ForgeKind,
    pub json: Zeroizing<String>,
}

/// Where the thread's credential comes from: Main's claim-bound
/// `WorkspaceForgeCredential`, redeemed per call.
#[async_trait]
pub trait ThreadCredentialSource: Send + Sync {
    async fn configuration(&self) -> Result<ThreadConfiguration, CredentialUnavailable>;
}

/// The thread's own credential (the [`THREAD_CREDENTIAL`] target). Offered
/// when the runtime context says the owner chose one.
pub struct ThreadCredential {
    source: Arc<dyn ThreadCredentialSource>,
    /// The forge APIs' transport; `None` is this host's.
    transport: Option<Arc<dyn Transport>>,
}

impl ThreadCredential {
    #[must_use]
    pub fn new(source: Arc<dyn ThreadCredentialSource>) -> Self {
        Self {
            source,
            transport: None,
        }
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test(
        source: Arc<dyn ThreadCredentialSource>,
        transport: Arc<dyn Transport>,
    ) -> Self {
        Self {
            source,
            transport: Some(transport),
        }
    }

    /// A target for the upstream's repository, or the sentence refusing it.
    async fn target(&self, upstream: &Upstream) -> Result<PullRequestTarget, String> {
        let configuration = self
            .source
            .configuration()
            .await
            .map_err(|unavailable| unavailable.sentence().to_owned())?;
        let forge = configuration.forge;
        let unusable = || {
            format!(
                "The thread's {} credential is not usable; its owner can choose another.",
                forge.name()
            )
        };
        if configuration.json.len() > MAX_CREDENTIAL_BYTES {
            return Err(unusable());
        }
        let Ok(Value::Object(data)) = serde_json::from_str::<Value>(&configuration.json) else {
            return Err(unusable());
        };
        drop(configuration);
        let repository = match forge {
            ForgeKind::GitHub => remote_repository_as_written(&upstream.remote_url),
            ForgeKind::GitLab => remote_path_as_written(&upstream.remote_url),
        }
        .ok_or_else(|| {
            "This folder's upstream remote does not name a repository, so nothing can be pushed."
                .to_owned()
        })?;
        let mut settings = Map::new();
        settings.insert("repository".to_owned(), Value::String(repository.clone()));
        let target = match forge {
            ForgeKind::GitHub => {
                settings.insert("github_configuration".to_owned(), Value::Object(data));
                for key in ["active_branch", "base_branch"] {
                    settings.insert(key.to_owned(), Value::String(upstream.branch.clone()));
                }
                let config = GitHubToolkitConfig::parse(&settings);
                // The parsed config holds its own zeroizing copy.
                drop(settings);
                let config = config.map_err(|_| unusable())?;
                PullRequestTarget::github(
                    THREAD_CREDENTIAL,
                    repository,
                    config,
                    self.transport.as_ref(),
                )
            }
            ForgeKind::GitLab => {
                settings.insert("gitlab_configuration".to_owned(), Value::Object(data));
                settings.insert("branch".to_owned(), Value::String(upstream.branch.clone()));
                let config = GitLabToolkitConfig::parse(&settings);
                drop(settings);
                let config = config.map_err(|_| unusable())?;
                PullRequestTarget::gitlab(
                    THREAD_CREDENTIAL,
                    repository,
                    config,
                    self.transport.as_ref(),
                )
            }
        };
        target.ok_or_else(unusable)
    }
}

enum Opener {
    GitHub(Arc<GitHubClient>),
    GitLab(Arc<GitLabClient>),
}

/// One credential-bearing forge for one repository: a bound GitHub or
/// GitLab toolkit, or the thread's own credential.
pub struct PullRequestTarget {
    toolkit_name: String,
    kind: ForgeKind,
    repository: String,
    /// `https://host[:port]/`, the forge's web and git origin.
    web_origin: Url,
    /// `None`: the configuration cannot push over git.
    git: Option<GitCredential>,
    opener: Opener,
}

impl PullRequestTarget {
    #[must_use]
    pub fn toolkit_name(&self) -> &str {
        &self.toolkit_name
    }

    #[must_use]
    pub fn repository(&self) -> &str {
        &self.repository
    }

    #[must_use]
    pub const fn forge(&self) -> ForgeKind {
        self.kind
    }

    fn github(
        name: &str,
        repository: String,
        config: GitHubToolkitConfig,
        transport: Option<&Arc<dyn Transport>>,
    ) -> Option<Self> {
        let web_origin = github_web_origin(config.base_url())?;
        let git = config
            .git_http_credential()
            .map(|(user, secret)| GitCredential::basic(user, secret));
        let client = match transport {
            #[cfg(test)]
            Some(transport) => {
                GitHubClient::test_with_transport(config, Arc::clone(transport)).ok()?
            }
            _ => GitHubClient::new(config).ok()?,
        };
        Some(Self {
            toolkit_name: name.to_owned(),
            kind: ForgeKind::GitHub,
            repository,
            web_origin,
            git,
            opener: Opener::GitHub(Arc::new(client)),
        })
    }

    fn gitlab(
        name: &str,
        repository: String,
        config: GitLabToolkitConfig,
        transport: Option<&Arc<dyn Transport>>,
    ) -> Option<Self> {
        let mut web_origin = config.base_url().clone();
        web_origin.set_path("/");
        let (user, secret) = config.git_http_credential();
        let git = Some(GitCredential::basic(user, secret));
        let client = match transport {
            #[cfg(test)]
            Some(transport) => GitLabClient::fixture(
                config,
                elitea_connectors::gitlab::wire::http_transport(Arc::clone(transport)),
            ),
            _ => GitLabClient::new(config).ok()?,
        };
        Some(Self {
            toolkit_name: name.to_owned(),
            kind: ForgeKind::GitLab,
            repository,
            web_origin,
            git,
            opener: Opener::GitLab(Arc::new(client)),
        })
    }

    /// A bound GitHub toolkit over a recorded fixture.
    #[cfg(test)]
    pub(in crate::toolkits) fn github_test(
        name: &str,
        settings: &Map<String, Value>,
        transport: &Arc<dyn Transport>,
    ) -> Self {
        let config = GitHubToolkitConfig::parse(settings).expect("a GitHub configuration");
        let repository = config.repository().to_owned();
        Self::github(name, repository, config, Some(transport)).expect("a target")
    }

    /// A bound GitLab toolkit over a recorded fixture.
    #[cfg(test)]
    pub(in crate::toolkits) fn gitlab_test(
        name: &str,
        settings: &Map<String, Value>,
        transport: &Arc<dyn Transport>,
    ) -> Self {
        let config = GitLabToolkitConfig::parse(settings).expect("a GitLab configuration");
        let repository = config.repository().to_owned();
        Self::gitlab(name, repository, config, Some(transport)).expect("a target")
    }

    /// The web host, lowercased, without port.
    fn web_host(&self) -> String {
        self.web_origin
            .host_str()
            .unwrap_or_default()
            .to_ascii_lowercase()
    }

    fn page(&self, suffix: &str) -> String {
        format!(
            "{}{}{suffix}",
            self.web_origin.as_str(),
            self.repository.trim_matches('/')
        )
    }

    fn git_url(&self) -> Option<Url> {
        Url::parse(&self.page(".git")).ok()
    }

    fn branch_url(&self, branch: &str) -> String {
        match self.kind {
            ForgeKind::GitHub => self.page(&format!("/tree/{branch}")),
            ForgeKind::GitLab => self.page(&format!("/-/tree/{branch}")),
        }
    }

    /// Whether this target may write to the recorded upstream: the same
    /// repository on the same host. An SSH alias (a host without a dot)
    /// names no host, so it matches only when it is the credential's host
    /// spelled the same; otherwise it is refused, never assumed to be the
    /// credential's host.
    fn scope(&self, upstream: &Upstream) -> Result<(), String> {
        let remote =
            remote_path_as_written(&upstream.remote_url).map(|path| path.to_ascii_lowercase());
        if remote.as_deref() != Some(self.repository.to_ascii_lowercase().as_str()) {
            return Err(if self.toolkit_name == THREAD_CREDENTIAL {
                "This folder's upstream remote does not name a repository on this forge.".to_owned()
            } else {
                format!(
                    "This folder's upstream is not {}; use the toolkit of that repository.",
                    self.repository
                )
            });
        }
        let host = self.web_host();
        let Some(remote) = remote_host(&upstream.remote_url) else {
            return Err(
                "This folder's upstream remote names no host, so nothing can be pushed.".to_owned(),
            );
        };
        if remote == host {
            return Ok(());
        }
        if !remote.contains('.') {
            // `github-work:o/r`: an alias from the desktop's SSH config,
            // which the cloud cannot read.
            return Err(format!(
                "This folder's upstream remote uses the SSH alias `{remote}`, which names no host the {} credential (for {host}) can be checked against; set the remote's URL to {host} on the desktop and send the thread again.",
                self.kind.name()
            ));
        }
        Err(format!(
            "The {} credential is for {host}, but this folder's upstream is on {remote}; choose a credential for {remote}.",
            self.kind.name()
        ))
    }

    async fn open_request(
        &self,
        head: &str,
        base: &str,
        title: &str,
        body: &str,
    ) -> Result<(u64, String), String> {
        match &self.opener {
            Opener::GitHub(client) => {
                open_pull_request_from_branch(client, title, body, head, base).await
            }
            Opener::GitLab(client) => client
                .open_merge_request(head, base, title, body)
                .await
                .map_err(|error| format!("GitLab refused it ({:?})", error.code())),
        }
    }
}

/// `https://host[:port]/` of a GitHub API base URL: `api.github.com` is
/// `github.com`; GitHub Enterprise's `https://host/api/v3` is `host`.
fn github_web_origin(base: &Url) -> Option<Url> {
    let mut origin = base.clone();
    origin.set_path("/");
    origin.set_query(None);
    if origin
        .host_str()
        .is_some_and(|host| host.eq_ignore_ascii_case("api.github.com"))
    {
        origin.set_host(Some("github.com")).ok()?;
        origin.set_port(None).ok()?;
    }
    Some(origin)
}

/// The GitHub and GitLab toolkits of a turn's frozen snapshot, each with its
/// own client (its own credentials, never shared with the toolkit's
/// toolset). A toolkit whose settings do not parse is left out.
#[must_use]
pub fn pull_request_targets(snapshot: &AdmittedToolSnapshot<'_>) -> Vec<PullRequestTarget> {
    snapshot
        .iter()
        .filter(|reference| reference.kind() == FrozenToolKind::Configured)
        .filter_map(|reference| {
            let settings = reference.settings()?;
            match reference.tool_type() {
                "github" => {
                    let config = GitHubToolkitConfig::parse(settings).ok()?;
                    let repository = config.repository().to_owned();
                    PullRequestTarget::github(reference.toolkit_name(), repository, config, None)
                }
                "gitlab" => {
                    let config = GitLabToolkitConfig::parse(settings).ok()?;
                    let repository = config.repository().to_owned();
                    PullRequestTarget::gitlab(reference.toolkit_name(), repository, config, None)
                }
                _ => None,
            }
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

/// The whole repository path of a remote URL (`group/sub/repo`), in the
/// remote's own case, without `.git`.
fn remote_path_as_written(remote: &str) -> Option<String> {
    let trimmed = remote.trim().trim_end_matches('/');
    let without_suffix = trimmed.strip_suffix(".git").unwrap_or(trimmed);
    let path = match without_suffix.split_once("://") {
        Some((_, rest)) => rest.split_once('/').map(|(_, path)| path)?,
        None => without_suffix.split_once(':').map(|(_, path)| path)?,
    };
    // `ssh://git@host:22/o/r` keeps its port in the authority; a scp-like
    // `host:o/r` has none.
    let path = path.trim_matches('/');
    let parts: Vec<&str> = path.split('/').collect();
    (parts.len() >= 2
        && parts
            .iter()
            .all(|part| !part.is_empty() && !matches!(*part, "." | "..")))
    .then(|| path.to_owned())
}

/// `owner/repo` of a remote URL, in the remote's own case.
fn remote_repository_as_written(remote: &str) -> Option<String> {
    let path = remote_path_as_written(remote)?;
    let mut parts = path.rsplitn(3, '/');
    let repo = parts.next()?;
    let owner = parts.next()?;
    Some(format!("{owner}/{repo}"))
}

/// `.lock`, which git reserves (compared as git does, case-sensitively).
fn has_lock_suffix(name: &str) -> bool {
    name.as_bytes().ends_with(b".lock")
}

/// git's branch-name rules (`git check-ref-format --branch`), narrowed.
fn valid_branch(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= MAX_BRANCH_BYTES
        && name != "HEAD"
        && name != "@"
        && !name.starts_with(['-', '/'])
        && !name.ends_with(['/', '.'])
        && !has_lock_suffix(name)
        && !name.contains("..")
        && !name.contains("@{")
        && !name.contains("//")
        && !name
            .split('/')
            .any(|part| part.starts_with('.') || has_lock_suffix(part))
        && !name.chars().any(|character| {
            character.is_control()
                || character.is_whitespace()
                || matches!(character, '~' | '^' | ':' | '?' | '*' | '[' | '\\')
        })
}

/// The tools' toolset, or `None` when there is neither a thread credential
/// nor a bound GitHub or GitLab toolkit. Without a push source only
/// `open_pull_request` is offered, through the GitHub API path.
#[must_use]
pub fn open_pull_request_toolset(
    thread: Option<ThreadCredential>,
    targets: Vec<PullRequestTarget>,
    diff: Arc<dyn WorkspaceDiffSource>,
    audit: Arc<dyn PullRequestAudit>,
) -> Option<Arc<dyn Toolset>> {
    WorkspaceForgeTools {
        thread,
        targets,
        diff,
        push: None,
        push_transport: None,
        audit,
    }
    .toolset()
}

/// `git_push` and `open_pull_request` (the cloud executor's tools).
pub struct WorkspaceForgeTools {
    pub thread: Option<ThreadCredential>,
    pub targets: Vec<PullRequestTarget>,
    pub diff: Arc<dyn WorkspaceDiffSource>,
    pub push: Option<Arc<dyn WorkspacePushSource>>,
    /// The push's transport; `None` is this host's.
    pub push_transport: Option<Arc<dyn Transport>>,
    pub audit: Arc<dyn PullRequestAudit>,
}

impl WorkspaceForgeTools {
    /// The toolset, or `None` when there is no credential at all.
    #[must_use]
    pub fn toolset(self) -> Option<Arc<dyn Toolset>> {
        if self.thread.is_none() && self.targets.is_empty() {
            return None;
        }
        let pushes = self.push.is_some();
        let shared = Arc::new(Shared {
            thread: self.thread,
            targets: self.targets.into_iter().map(Arc::new).collect(),
            diff: self.diff,
            push: self.push,
            push_transport: self.push_transport,
            audit: self.audit,
            pushed: Mutex::new(Vec::new()),
        });
        let mut tools: Vec<Arc<dyn Tool>> =
            vec![Arc::new(OpenPullRequestTool(Arc::clone(&shared)))];
        if pushes {
            tools.push(Arc::new(GitPushTool(shared)));
        }
        Some(Arc::new(PullRequestToolset { tools }))
    }
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

/// A branch `git_push` created this turn: non-secret facts only. The
/// target (its client, its credential) is never kept across calls.
struct PushedBranch {
    /// The toolkit it was pushed through, or [`THREAD_CREDENTIAL`].
    toolkit: String,
    branch: String,
    upstream: Upstream,
    files: u32,
}

struct Shared {
    thread: Option<ThreadCredential>,
    targets: Vec<Arc<PullRequestTarget>>,
    diff: Arc<dyn WorkspaceDiffSource>,
    push: Option<Arc<dyn WorkspacePushSource>>,
    push_transport: Option<Arc<dyn Transport>>,
    audit: Arc<dyn PullRequestAudit>,
    pushed: Mutex<Vec<PushedBranch>>,
}

enum Choice<'a> {
    Thread(&'a ThreadCredential),
    Toolkit(&'a Arc<PullRequestTarget>),
}

fn argument<'a>(arguments: &'a Map<String, Value>, key: &str) -> Option<&'a str> {
    arguments.get(key).and_then(Value::as_str).map(str::trim)
}

fn answer(status: &str, message: impl Into<String>) -> Value {
    json!({"status": status, "message": message.into()})
}

/// A pushed branch, as a tool answers it.
struct PushOutcome {
    target: Arc<PullRequestTarget>,
    branch: String,
    url: String,
    commits: u32,
    files: u32,
    upstream: Upstream,
}

impl Shared {
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

    /// The chosen target, scoped to the upstream: the thread's credential
    /// is redeemed here (Main re-checks the policy switches on each
    /// redemption), once per call; a bound toolkit is the turn's own.
    async fn target(
        &self,
        choice: &Choice<'_>,
        upstream: &Upstream,
    ) -> Result<Arc<PullRequestTarget>, Value> {
        let target = match choice {
            Choice::Toolkit(target) => Arc::clone(target),
            Choice::Thread(thread) => Arc::new(
                thread
                    .target(upstream)
                    .await
                    .map_err(|sentence| answer("refused", sentence))?,
            ),
        };
        target
            .scope(upstream)
            .map_err(|sentence| answer("refused", sentence))?;
        Ok(target)
    }

    fn toolkit_schema(&self, what: &str) -> Value {
        json!({"type": "string", "description": format!("What to {what} through: `@thread` (the thread's own GitHub or GitLab credential, the default when it has one) or a GitHub or GitLab toolkit's name. Optional when there is only one choice."), "enum": self.choices()})
    }

    /// The recorded upstream, without preparing a push.
    async fn upstream(&self) -> Result<Upstream, Value> {
        let Some(source) = &self.push else {
            return Err(answer("refused", "This host cannot push."));
        };
        source
            .upstream()
            .await
            .map_err(|unavailable| answer("refused", unavailable.sentence()))
    }

    /// The thread's commits, re-parented onto `upstream` (the one the
    /// target was scoped to) and packed.
    async fn prepare(&self, upstream: &Upstream) -> Result<PreparedPush, Value> {
        let Some(source) = &self.push else {
            return Err(answer("refused", "This host cannot push."));
        };
        let prepared = source
            .prepare_push()
            .await
            .map_err(|unavailable| answer("refused", unavailable.sentence()))?;
        if prepared.upstream != *upstream {
            return Err(answer(
                "error",
                "The folder's upstream changed while the push was prepared; nothing was pushed.",
            ));
        }
        Ok(prepared)
    }

    /// Push the prepared commits as a new branch through `target` (already
    /// resolved and scoped to `prepared.upstream`).
    async fn push(
        &self,
        target: Arc<PullRequestTarget>,
        branch: &str,
        prepared: PreparedPush,
    ) -> Result<PushOutcome, Value> {
        let Some(credential) = target.git.as_ref() else {
            return Err(cannot_push(&target));
        };
        let Some(repository_url) = target.git_url() else {
            return Err(answer("refused", "The repository's git URL is not valid."));
        };
        let transport = match &self.push_transport {
            Some(transport) => Arc::clone(transport),
            None => host_transport(&PUSH_POLICY)
                .map_err(|_| answer("error", "The push client could not be built."))?,
        };
        let PreparedPush {
            upstream,
            tip,
            commits,
            files,
            pack,
        } = prepared;
        let pushed = push_new_branch(
            transport,
            credential,
            &PushRequest {
                repository_url: &repository_url,
                branch,
                tip: &tip,
            },
            pack,
        )
        .await
        .map_err(|failure| {
            answer(
                "error",
                format!("Nothing was pushed: {}.", failure.sentence()),
            )
        })?;
        let url = target.branch_url(&pushed.branch);
        self.audit.pushed(&BranchPushed {
            toolkit: target.toolkit_name.clone(),
            forge: target.kind,
            repository: target.repository.clone(),
            branch: pushed.branch.clone(),
            base: upstream.branch.clone(),
            url: url.clone(),
            commits,
            upstream_sha: upstream.sha.clone(),
        });
        if let Ok(mut pushed_branches) = self.pushed.lock() {
            pushed_branches.push(PushedBranch {
                toolkit: target.toolkit_name.clone(),
                branch: pushed.branch.clone(),
                upstream: upstream.clone(),
                files,
            });
        }
        Ok(PushOutcome {
            target,
            branch: pushed.branch,
            url,
            commits,
            files,
            upstream,
        })
    }
}

/// A configuration without a git credential (a GitHub App, anonymous).
fn cannot_push(target: &PullRequestTarget) -> Value {
    answer(
        "refused",
        format!(
            "`{}` cannot push over git (a GitHub App or anonymous configuration); choose a credential with a token or a password.",
            target.toolkit_name
        ),
    )
}

/// The common arguments: the choice and a branch that is not the upstream's.
fn checked_branch(arguments: &Map<String, Value>) -> Result<&str, Value> {
    let Some(branch) = argument(arguments, "branch") else {
        return Err(answer("error", "`branch` is required."));
    };
    let branch = branch.strip_prefix("refs/heads/").unwrap_or(branch);
    if !valid_branch(branch) {
        return Err(answer(
            "error",
            "`branch` is not a valid branch name (no spaces, `..`, `~^:?*[\\`, or a leading `-`).",
        ));
    }
    Ok(branch)
}

/// The new branch is created, never moved: it cannot be the branch the
/// change goes into.
fn not_upstream(branch: &str, upstream: &Upstream) -> Result<(), Value> {
    if branch.eq_ignore_ascii_case(&upstream.branch) {
        return Err(answer(
            "refused",
            format!(
                "The new branch must not be the upstream branch `{}`; name a new one.",
                upstream.branch
            ),
        ));
    }
    Ok(())
}

struct GitPushTool(Arc<Shared>);

#[async_trait]
impl Tool for GitPushTool {
    fn name(&self) -> &str {
        GIT_PUSH
    }

    fn description(&self) -> &'static str {
        "Push this cloud thread's commits (its history, binary files included) to the folder's real git remote as a NEW branch, based on the upstream commit. Never force-pushes and never touches an existing branch: a suffix is added when the name is taken. The change is taken from the workspace, not from an argument. A person may be asked to approve it first. Use open_pull_request to push and open a pull request in one step."
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "toolkit": self.0.toolkit_schema("push"),
                "branch": {"type": "string", "minLength": 1, "maxLength": MAX_BRANCH_BYTES, "description": "The name of the new branch, e.g. `feature/cloud-change`."}
            },
            "required": ["branch"],
            "additionalProperties": false
        }))
    }

    fn is_read_only(&self) -> bool {
        false
    }

    async fn execute(&self, _ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        Ok(self.run(&args).await.unwrap_or_else(|refusal| refusal))
    }
}

impl GitPushTool {
    async fn run(&self, args: &Value) -> Result<Value, Value> {
        let shared = &self.0;
        let arguments = args
            .as_object()
            .ok_or_else(|| answer("error", "The arguments must be an object."))?;
        let choice = shared.choose(argument(arguments, "toolkit")).ok_or_else(|| {
            answer(
                "error",
                "Name what to push through (`toolkit`): `@thread` or a GitHub or GitLab toolkit.",
            )
        })?;
        let branch = checked_branch(arguments)?;
        let upstream = shared.upstream().await?;
        not_upstream(branch, &upstream)?;
        // One redemption for the call, before anything is prepared.
        let target = shared.target(&choice, &upstream).await?;
        if target.git.is_none() {
            return Err(cannot_push(&target));
        }
        let prepared = shared.prepare(&upstream).await?;
        let pushed = shared.push(target, branch, prepared).await?;
        Ok(json!({
            "status": "ok",
            "message": format!(
                "Pushed {} commit(s) to the new branch `{}`: {}. Open a {} from it with open_pull_request and `push: false`.",
                pushed.commits, pushed.branch, pushed.url, pushed.target.kind.request_noun()
            ),
            "branch": pushed.branch, "url": pushed.url, "commits": pushed.commits,
            "base": pushed.upstream.branch,
        }))
    }
}

struct OpenPullRequestTool(Arc<Shared>);

#[async_trait]
impl Tool for OpenPullRequestTool {
    fn name(&self) -> &str {
        OPEN_PULL_REQUEST
    }

    fn description(&self) -> &'static str {
        "Open a pull request (a merge request on GitLab) with this cloud thread's changes against the folder's upstream branch. The change is taken from the workspace, not from an argument: the thread's commits are pushed as a NEW branch (a suffix is added when the name is taken; an existing branch is never overwritten), then the request is opened from it. With `push: false`, it opens one from a branch git_push already pushed this turn. A person may be asked to approve it first."
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "toolkit": self.0.toolkit_schema("open it"),
                "title": {"type": "string", "minLength": 1, "maxLength": MAX_TITLE_CHARS},
                "body": {"type": "string", "maxLength": MAX_BODY_BYTES},
                "branch": {"type": "string", "minLength": 1, "maxLength": MAX_BRANCH_BYTES, "description": "The name of the new branch, e.g. `feature/cloud-change`; with `push: false`, the branch git_push answered."},
                "push": {"type": "boolean", "description": "false: open from the branch git_push already pushed (default true)."}
            },
            "required": ["title", "branch"],
            "additionalProperties": false
        }))
    }

    fn is_read_only(&self) -> bool {
        false
    }

    async fn execute(&self, _ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        Ok(self.run(&args).await.unwrap_or_else(|refusal| refusal))
    }
}

impl OpenPullRequestTool {
    async fn run(&self, args: &Value) -> Result<Value, Value> {
        let shared = &self.0;
        let arguments = args
            .as_object()
            .ok_or_else(|| answer("error", "The arguments must be an object."))?;
        let choice = shared.choose(argument(arguments, "toolkit")).ok_or_else(|| {
            answer(
                "error",
                "Name what to open the pull request through (`toolkit`): `@thread` or a GitHub or GitLab toolkit.",
            )
        })?;
        let title = argument(arguments, "title").unwrap_or_default();
        if title.is_empty() || title.chars().count() > MAX_TITLE_CHARS {
            return Err(answer("error", "`title` must be non-empty and bounded."));
        }
        let branch = checked_branch(arguments)?;
        let body = argument(arguments, "body").unwrap_or_default();
        if body.len() > MAX_BODY_BYTES {
            return Err(answer("error", "`body` is too long."));
        }
        let push = arguments
            .get("push")
            .and_then(Value::as_bool)
            .unwrap_or(true);
        if !push {
            return self.open_pushed(&choice, branch, title, body).await;
        }
        if shared.push.is_none() {
            return self.through_api(&choice, branch, title, body).await;
        }
        let upstream = shared.upstream().await?;
        not_upstream(branch, &upstream)?;
        // One redemption for the call, before anything is prepared.
        let target = shared.target(&choice, &upstream).await?;
        if target.git.is_none() {
            // A configuration that cannot push over git opens through the
            // API: no pack is prepared for it.
            let diff = shared
                .diff
                .pull_request_diff()
                .await
                .map_err(|unavailable| answer("refused", unavailable.sentence()))?;
            if diff.upstream() != upstream {
                return Err(answer(
                    "error",
                    "The folder's upstream changed while the change was read; no pull request was opened.",
                ));
            }
            return self.api(&target, &diff, branch, title, body).await;
        }
        let prepared = shared.prepare(&upstream).await?;
        let pushed = shared.push(target, branch, prepared).await?;
        self.open(&pushed, title, body).await
    }

    async fn open_pushed(
        &self,
        choice: &Choice<'_>,
        branch: &str,
        title: &str,
        body: &str,
    ) -> Result<Value, Value> {
        let found = {
            let pushed = self
                .0
                .pushed
                .lock()
                .map_err(|_| answer("error", "The pushed branches could not be read."))?;
            let wanted = match choice {
                Choice::Thread(_) => THREAD_CREDENTIAL,
                Choice::Toolkit(target) => target.toolkit_name.as_str(),
            };
            pushed
                .iter()
                .rev()
                .find(|entry| entry.branch == branch && entry.toolkit == wanted)
                .map(|entry| (entry.upstream.clone(), entry.files))
        };
        let Some((upstream, files)) = found else {
            return Err(answer(
                "refused",
                format!(
                    "`{branch}` is not a branch git_push pushed this turn through that credential; push it first, or leave `push` out."
                ),
            ));
        };
        // Redeemed again for this call: the credential (and the policy
        // switches Main checks when it redeems it) may have changed since
        // the push.
        let target = self.0.target(choice, &upstream).await?;
        let pushed = PushOutcome {
            url: target.branch_url(branch),
            target,
            branch: branch.to_owned(),
            commits: 0,
            files,
            upstream,
        };
        self.open(&pushed, title, body).await
    }

    async fn open(&self, pushed: &PushOutcome, title: &str, body: &str) -> Result<Value, Value> {
        let target = &pushed.target;
        let noun = target.kind.request_noun();
        let (number, url) = target
            .open_request(&pushed.branch, &pushed.upstream.branch, title, body)
            .await
            .map_err(|sentence| {
                json!({
                    "status": "partial",
                    "message": format!("The branch `{}` was pushed ({}), but no {noun} was opened: {sentence}.", pushed.branch, pushed.url),
                    "branch": pushed.branch, "branch_url": pushed.url,
                })
            })?;
        self.0.audit.opened(&PullRequestOpened {
            toolkit: target.toolkit_name.clone(),
            repository: target.repository.clone(),
            branch: pushed.branch.clone(),
            base: pushed.upstream.branch.clone(),
            number,
            url: url.clone(),
            files: usize::try_from(pushed.files).unwrap_or(usize::MAX),
            upstream_sha: pushed.upstream.sha.clone(),
        });
        let marker = match target.kind {
            ForgeKind::GitHub => "#",
            ForgeKind::GitLab => "!",
        };
        Ok(json!({
            "status": "ok",
            "message": format!("{} {marker}{number} opened: {url}", capitalized(noun)),
            "pull_request": number, "url": url, "branch": pushed.branch,
            "base": pushed.upstream.branch, "files": pushed.files,
            "forge": target.kind.as_str(),
        }))
    }

    /// The GitHub API path: no git push, the text diff through the contents
    /// API, with its limits.
    async fn through_api(
        &self,
        choice: &Choice<'_>,
        branch: &str,
        title: &str,
        body: &str,
    ) -> Result<Value, Value> {
        let shared = &self.0;
        let diff = shared
            .diff
            .pull_request_diff()
            .await
            .map_err(|unavailable| answer("refused", unavailable.sentence()))?;
        let upstream = diff.upstream();
        not_upstream(branch, &upstream)?;
        let target = shared.target(choice, &upstream).await?;
        self.api(&target, &diff, branch, title, body).await
    }

    /// The API path through `target`, already resolved and scoped to the
    /// diff's upstream.
    async fn api(
        &self,
        target: &PullRequestTarget,
        diff: &WorkspaceDiff,
        branch: &str,
        title: &str,
        body: &str,
    ) -> Result<Value, Value> {
        let shared = &self.0;
        let Opener::GitHub(client) = &target.opener else {
            return Err(answer(
                "refused",
                "A merge request needs a push, which this credential or host cannot make.",
            ));
        };
        let outcome = open_pull_request_from_diff(
            client,
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
        Ok(match outcome {
            Ok(PullRequestFromDiffOutcome::Opened {
                branch,
                number,
                url,
                files,
            }) => {
                shared.audit.opened(&PullRequestOpened {
                    toolkit: target.toolkit_name.clone(),
                    repository: target.repository.clone(),
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
                    "base": diff.upstream_branch, "files": files, "forge": "github",
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
        })
    }
}

fn capitalized(text: &str) -> String {
    let mut characters = text.chars();
    characters
        .next()
        .map(|first| first.to_uppercase().chain(characters).collect())
        .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::{
        github_web_origin, remote_host, remote_path_as_written, remote_repository_as_written,
        valid_branch,
    };
    use elitea_connectors::transport::Url;

    #[test]
    fn remote_hosts_compare() {
        for (url, want) in [
            ("https://github.com/o/r.git", "github.com"),
            ("https://user@GHE.example.com:8443/o/r", "ghe.example.com"),
            ("git@github.com:o/r.git", "github.com"),
            ("ssh://git@github.com:22/o/r.git", "github.com"),
            ("github-work:o/r.git", "github-work"),
        ] {
            assert_eq!(remote_host(url).as_deref(), Some(want), "{url}");
        }
        let origin = |base: &str| {
            github_web_origin(&Url::parse(base).expect("url"))
                .expect("origin")
                .to_string()
        };
        assert_eq!(origin("https://api.github.com"), "https://github.com/");
        assert_eq!(
            origin("https://ghe.example.com:8443/api/v3"),
            "https://ghe.example.com:8443/"
        );
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
            assert_eq!(
                remote_repository_as_written(url).as_deref(),
                Some(want),
                "{url}"
            );
        }
        assert_eq!(remote_repository_as_written("not a url"), None);
        // GitLab keeps the whole group path.
        assert_eq!(
            remote_path_as_written("git@gitlab.example.com:Group/Sub/Repo.git").as_deref(),
            Some("Group/Sub/Repo")
        );
        assert_eq!(
            remote_path_as_written("ssh://git@gitlab.example.com:2222/g/r.git").as_deref(),
            Some("g/r")
        );
        assert_eq!(
            remote_path_as_written("https://gitlab.example.com/g/../r"),
            None
        );
    }

    #[test]
    fn branch_names_follow_git() {
        for good in ["feature/cloud", "fix-1", "a.b", "user/x_y"] {
            assert!(valid_branch(good), "{good}");
        }
        for bad in [
            "", "-x", "/x", "x/", "x.", "x.lock", "a..b", "a b", "a~1", "a^", "a:b", "a?", "a*",
            "a[", "a\\b", "@", "HEAD", "a@{1}", "a//b", ".hidden", "a/.b",
        ] {
            assert!(!valid_branch(bad), "{bad:?}");
        }
    }
}
