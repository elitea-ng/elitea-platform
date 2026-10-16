//! One cloud turn's workspace lifecycle (ADR-0034 decisions 7 and 9):
//! hydrate before the agent runs, publish exactly one segment after it.
//!
//! * **Begin** (outside the sandbox): fetch the plan and the segment over the
//!   runtime plane, check that the store is where the runtime context says it
//!   is, verify and check out, re-scan for secrets (a finding fails the
//!   turn), start the sandbox and move the tree in.
//! * **Finish**: on a completed turn or a pause for input, move the tree out,
//!   destroy the sandbox, commit (author `Elitea cloud executor`, trailers
//!   `Elitea-Execution-Id` and `Elitea-Epoch`), build a COMPACT segment,
//!   upload and publish with the expected old cloud ref. On a failed or
//!   cancelled turn, destroy the sandbox and publish nothing.
//!
//! Exactly one finish runs per turn; a second call returns the first result.

use std::collections::BTreeMap;
use std::path::PathBuf;
use std::sync::Arc;

use tokio::sync::Mutex;

use crate::protocol::elitea::runtime::v1::WorkspaceClaimV1;

use super::codec::{CodecError, SegmentExpectation, TurnCommit, WorkspaceCodec};
use super::context::WorkspaceRuntimeContext;
use super::redact::{RedactingSandbox, SecretEnv};
use super::sandbox::{
    SandboxRunError, WorkspaceRunDefaults, WorkspaceRunSpec, WorkspaceSandbox,
    WorkspaceSandboxLauncher,
};
use super::scan::{ScanError, ScanLimits, scan_tree};
use super::store::{
    PublishSegment, REF_CLOUD, REF_HANDOFF_PREFIX, RefusalCode, StoreCallError, WorkspaceStore,
};

/// The commit author of every cloud turn.
pub(crate) const COMMIT_AUTHOR_NAME: &str = "Elitea cloud executor";
pub(crate) const COMMIT_AUTHOR_EMAIL: &str = "cloud-executor@elitea.invalid";
/// The meta value Main stamps on a cloud turn's response.
pub(crate) const EXECUTED_BY_CLOUD: &str = "cloud_executor";

/// How the agent's part of the turn ended.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum TurnEnd {
    Completed,
    /// A HITL pause: the work so far is published, then the thread idles.
    Paused,
    /// Failed or cancelled: nothing is published.
    Failed,
}

/// What the turn's response is stamped with (decision 8).
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct WorkspaceStamp {
    pub(crate) executed_by: &'static str,
    pub(crate) workspace_epoch: u64,
    pub(crate) workspace_commit: String,
    pub(crate) seq: u64,
    /// The files whose harvested-env values were redacted (or whose change
    /// was dropped) before the commit: paths and names, never values.
    pub(crate) redactions: Vec<super::file_redact::FileRedaction>,
}

/// Why a workspace turn failed. Data-free.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) enum WorkspaceRunError {
    Store(StoreCallError),
    Codec(CodecError),
    Sandbox(SandboxRunError),
    /// The store is not where the runtime context says it is (another
    /// executor moved the cloud ref, or the plan lacks the snapshot).
    StaleState,
    /// The checked-out tree holds credential-shaped content.
    SecretFound {
        findings: usize,
    },
    /// The secret scan could not complete.
    ScanFailed,
    /// The changed files could not be scanned for harvested values; nothing
    /// is published unscanned.
    RedactionFailed,
    /// Blocking work did not complete.
    Internal,
}

impl WorkspaceRunError {
    #[must_use]
    pub(crate) fn code(&self) -> &'static str {
        match self {
            Self::Store(error) => match error {
                StoreCallError::Refused {
                    code: RefusalCode::StaleEpoch,
                    ..
                } => "workspace_run.stale_epoch",
                StoreCallError::Refused {
                    code: RefusalCode::RefConflict,
                    ..
                } => "workspace_run.ref_conflict",
                other => other.code(),
            },
            Self::Codec(error) => error.code(),
            Self::Sandbox(error) => error.code(),
            Self::StaleState => "workspace_run.stale_state",
            Self::SecretFound { .. } => "workspace_run.secret_found",
            Self::ScanFailed => "workspace_run.scan_failed",
            Self::RedactionFailed => "workspace_run.redaction_failed",
            Self::Internal => "workspace_run.internal",
        }
    }
}

/// Everything a workspace turn needs from the deployment.
pub(crate) struct WorkspaceRunFactory {
    store: Arc<dyn WorkspaceStore>,
    codec: Arc<dyn WorkspaceCodec>,
    launcher: Arc<dyn WorkspaceSandboxLauncher>,
    defaults: WorkspaceRunDefaults,
    scratch_root: PathBuf,
    grant_binder: Option<GrantBinder>,
}

/// Turns a claim's sandbox authority into a grant source.
pub(crate) type GrantBinder = Arc<
    dyn Fn(
            Arc<crate::protocol::control::ClaimBoundSandboxAuthority>,
        ) -> Arc<dyn super::sandbox::WorkspaceRunGrants>
        + Send
        + Sync,
>;

impl WorkspaceRunFactory {
    #[must_use]
    pub(crate) fn new(
        store: Arc<dyn WorkspaceStore>,
        codec: Arc<dyn WorkspaceCodec>,
        launcher: Arc<dyn WorkspaceSandboxLauncher>,
        defaults: WorkspaceRunDefaults,
        scratch_root: PathBuf,
    ) -> Self {
        Self {
            store,
            codec,
            launcher,
            defaults,
            scratch_root,
            grant_binder: None,
        }
    }

    /// The claim-bound grant source of one turn (`None`: no authority, the
    /// launch is refused).
    #[must_use]
    pub(crate) fn grants(
        &self,
        authority: Option<Arc<crate::protocol::control::ClaimBoundSandboxAuthority>>,
    ) -> Arc<dyn super::sandbox::WorkspaceRunGrants> {
        match (&self.grant_binder, authority) {
            (Some(binder), Some(authority)) => binder(authority),
            _ => Arc::new(super::sandbox::NoGrants),
        }
    }

    /// Bind grants to claims through Main's control plane (production).
    #[must_use]
    pub(crate) fn with_grant_binder(mut self, binder: GrantBinder) -> Self {
        self.grant_binder = Some(binder);
        self
    }

    /// Hydrate the thread and start the turn's sandbox.
    ///
    /// # Errors
    ///
    /// Any failure before the sandbox holds the tree; nothing is left
    /// running.
    #[allow(
        clippy::too_many_lines,
        reason = "hydrate is one ordered sequence: fetch, verify, scan, start"
    )]
    pub(crate) async fn begin(
        &self,
        context: &WorkspaceRuntimeContext,
        claim: WorkspaceClaimV1,
        execution_id: &str,
        grants: Arc<dyn super::sandbox::WorkspaceRunGrants>,
    ) -> Result<Arc<WorkspaceTurn>, WorkspaceRunError> {
        let limits = &context.limits;
        // The segment goes to the worker's disk (the turn's scratch), never
        // into one buffer (item 4 of the executor follow-up).
        let scratch = tempfile::Builder::new()
            .prefix("elitea-workspace-")
            .tempdir_in(&self.scratch_root)
            .map_err(|_| WorkspaceRunError::Internal)?;
        let segment_file = scratch.path().join("fetched.bundle");
        let fetched = self
            .store
            .fetch(&claim, 0, limits.max_segment_bytes, &segment_file)
            .await
            .map_err(WorkspaceRunError::Store)?;
        let checkout = checkout_commit(context, &fetched.segment.refs)?;
        let git_dir = scratch.path().join("store.git");
        let worktree = scratch.path().join("in");
        for directory in [&git_dir, &worktree] {
            std::fs::create_dir_all(directory).map_err(|_| WorkspaceRunError::Internal)?;
        }
        let expect = SegmentExpectation {
            sha256: fetched.segment.sha256.clone(),
            bytes: fetched.segment.bytes,
            refs: fetched.segment.refs.clone(),
            max_unpacked_bytes: limits.max_unpacked_bytes,
            max_files: limits.max_files,
            max_file_bytes: limits.max_file_bytes,
        };
        let codec = Arc::clone(&self.codec);
        let (hydrate_git, hydrate_tree, hydrate_checkout) =
            (git_dir.clone(), worktree.clone(), checkout.clone());
        let segment = segment_file.clone();
        let hydrate_expect = expect.clone();
        let hydrated = tokio::task::spawn_blocking(move || {
            codec.hydrate(
                &segment,
                &hydrate_expect,
                &hydrate_checkout,
                &hydrate_git,
                &hydrate_tree,
            )
        })
        .await
        .map_err(|_| WorkspaceRunError::Internal)?
        .map_err(WorkspaceRunError::Codec)?;
        // Verified and indexed into the quarantine: the file is not needed.
        let _ = std::fs::remove_file(&segment_file);
        let mut refs = fetched.segment.refs;
        let mut expected_old = context.expected_old_cloud.clone();
        let (checkout, worktree) = if context.rewind {
            // Regenerate of the newest cloud turn (decision 7): the cloud
            // ref moves back to that turn's parent through the normal
            // publish compare-and-set, before anything runs, and this turn
            // starts from the parent.
            let cloud = expected_old.clone().ok_or(WorkspaceRunError::StaleState)?;
            let rewound = scratch.path().join("rewound");
            std::fs::create_dir_all(&rewound).map_err(|_| WorkspaceRunError::Internal)?;
            let out = scratch.path().join("rewind.bundle");
            let (codec, git, tree, file, rewind_expect, from, to) = (
                Arc::clone(&self.codec),
                git_dir.clone(),
                rewound.clone(),
                out.clone(),
                expect.clone(),
                cloud.clone(),
                context.rewind_to.clone(),
            );
            let max = limits.max_segment_bytes;
            let (parent, built) = tokio::task::spawn_blocking(move || {
                codec.rewind(
                    &git,
                    &rewind_expect,
                    &from,
                    to.as_deref(),
                    &tree,
                    max,
                    &file,
                )
            })
            .await
            .map_err(|_| WorkspaceRunError::Internal)?
            .map_err(WorkspaceRunError::Codec)?;
            retry_once(|| {
                self.store
                    .upload(&claim, &built.sha256, built.bytes, &built.path)
            })
            .await
            .map_err(WorkspaceRunError::Store)?;
            let request = PublishSegment {
                segment_sha256: built.sha256.clone(),
                bytes: built.bytes,
                old_cloud: Some(cloud),
                new_cloud: parent.clone(),
            };
            retry_once(|| self.store.publish(&claim, &request))
                .await
                .map_err(WorkspaceRunError::Store)?;
            let _ = std::fs::remove_file(&out);
            tracing::info!(
                event = "workspace.rewound",
                execution_id,
                epoch = context.epoch,
                "regenerate: the cloud ref moved back to the regenerated turn's parent"
            );
            refs.insert(REF_CLOUD.to_owned(), parent.clone());
            expected_old = Some(parent.clone());
            (parent, rewound)
        } else {
            (checkout, worktree)
        };

        // Defence in depth (decision 5): the desktop scanned before upload.
        let scan_root = worktree.clone();
        let scan_limits = ScanLimits {
            max_file_bytes: limits.max_file_bytes,
            max_files: limits.max_files,
        };
        let skip: std::collections::BTreeSet<PathBuf> = hydrated
            .included_excluded
            .iter()
            .map(|path| PathBuf::from(path.trim_start_matches("./")))
            .collect();
        let findings =
            tokio::task::spawn_blocking(move || scan_tree(&scan_root, scan_limits, &skip))
                .await
                .map_err(|_| WorkspaceRunError::Internal)?
                .map_err(|error| match error {
                    ScanError::Io | ScanError::TooManyFiles => WorkspaceRunError::ScanFailed,
                })?;
        if !findings.is_empty() {
            // The audit event (`handoff.scan_blocked`) records counts and the
            // rules only, never paths or content (decision 12).
            let mut rules: Vec<&str> = findings.iter().map(|finding| finding.rule).collect();
            rules.sort_unstable();
            rules.dedup();
            tracing::warn!(
                event = "workspace.secret_scan_blocked",
                audit = "handoff.scan_blocked",
                execution_id,
                findings = findings.len(),
                rules = rules.join(","),
                "the handed-off tree holds credential-shaped content; the cloud turn is refused"
            );
            return Err(WorkspaceRunError::SecretFound {
                findings: findings.len(),
            });
        }

        let spec = WorkspaceRunSpec::from_context(
            run_id(execution_id, context.epoch),
            context,
            &self.defaults,
        )
        .map_err(WorkspaceRunError::Sandbox)?;
        // Contract 6.4: the harvested env, redeemed as a claim-bound secret,
        // goes to the sandbox's run_command environment and nowhere else.
        let env = if context.env_secret {
            self.store
                .env_secret(&claim)
                .await
                .map_err(WorkspaceRunError::Store)?
        } else {
            SecretEnv::default()
        };
        let redactor = env.redactor();
        let file_redactor = Arc::new(super::file_redact::FileRedactor::new(&env));
        let raw = self
            .launcher
            .launch(&spec, &env, grants)
            .await
            .map_err(WorkspaceRunError::Sandbox)?;
        drop(env);
        let sandbox: Arc<dyn WorkspaceSandbox> = Arc::new(RedactingSandbox::new(raw, redactor));
        if let Err(error) = sandbox.push_tree(&worktree).await {
            let _ = sandbox.destroy().await;
            return Err(WorkspaceRunError::Sandbox(error));
        }
        Ok(Arc::new(WorkspaceTurn {
            store: Arc::clone(&self.store),
            codec: Arc::clone(&self.codec),
            sandbox,
            claim,
            scratch,
            git_dir,
            parent: checkout,
            expected_old,
            refs,
            epoch: context.epoch,
            execution_id: execution_id.to_owned(),
            max_segment_bytes: limits.max_segment_bytes,
            file_redactor,
            outcome: Mutex::new(None),
        }))
    }
}

/// The commit the turn starts from: the cloud tip the runtime context names,
/// or the handoff snapshot before the first cloud turn.
fn checkout_commit(
    context: &WorkspaceRuntimeContext,
    refs: &BTreeMap<String, String>,
) -> Result<String, WorkspaceRunError> {
    match (&context.expected_old_cloud, refs.get(REF_CLOUD)) {
        (Some(expected), Some(current)) if expected == current => Ok(current.clone()),
        (None, None) => {
            let mut snapshots = refs
                .iter()
                .filter(|(name, _)| name.starts_with(REF_HANDOFF_PREFIX));
            match (snapshots.next(), snapshots.next()) {
                (Some((_, sha)), None) => Ok(sha.clone()),
                _ => Err(WorkspaceRunError::StaleState),
            }
        }
        _ => Err(WorkspaceRunError::StaleState),
    }
}

fn run_id(execution_id: &str, epoch: u64) -> String {
    let digest = super::store::sha256_hex(format!("{execution_id}\0{epoch}").as_bytes());
    format!("elitea-ws-{}", &digest[..24])
}

/// One running workspace turn.
pub(crate) struct WorkspaceTurn {
    store: Arc<dyn WorkspaceStore>,
    codec: Arc<dyn WorkspaceCodec>,
    sandbox: Arc<dyn WorkspaceSandbox>,
    claim: WorkspaceClaimV1,
    /// Removed on drop.
    scratch: tempfile::TempDir,
    git_dir: PathBuf,
    parent: String,
    expected_old: Option<String>,
    refs: BTreeMap<String, String>,
    epoch: u64,
    execution_id: String,
    max_segment_bytes: u64,
    /// The harvested values, for the end-of-turn file scan.
    file_redactor: Arc<super::file_redact::FileRedactor>,
    outcome: Mutex<Option<Result<Option<WorkspaceStamp>, WorkspaceRunError>>>,
}

impl WorkspaceTurn {
    /// The sandbox the turn's tools run in.
    #[must_use]
    pub(crate) fn sandbox(&self) -> Arc<dyn WorkspaceSandbox> {
        Arc::clone(&self.sandbox)
    }

    /// The pull-request diff (ADR-0034 decision 10), computed here from the
    /// turn's own repository: U from the verified quarantine, the workspace
    /// as it is now, moved out of the sandbox and redacted exactly as the
    /// end-of-turn commit would be, so no harvested value reaches a pull
    /// request either.
    ///
    /// # Errors
    ///
    /// No upstream, a diff over its limits, binary, or empty; or the
    /// workspace could not be read.
    pub(crate) async fn pull_request_diff(
        &self,
    ) -> Result<
        elitea_agent_runtime::toolkits::workspace_pull_request::WorkspaceDiff,
        elitea_agent_runtime::toolkits::workspace_pull_request::DiffUnavailable,
    > {
        use elitea_agent_runtime::toolkits::workspace_pull_request::DiffUnavailable;
        let failed = || {
            DiffUnavailable::Failed("The workspace could not be read; no pull request was opened.")
        };
        let upstream = self
            .refs
            .get(super::store::REF_UPSTREAM)
            .cloned()
            .ok_or(DiffUnavailable::NoUpstream)?;
        let view = tempfile::Builder::new()
            .prefix("pull-request-")
            .tempdir_in(self.scratch.path())
            .map_err(|_| failed())?;
        self.sandbox
            .pull_tree(view.path())
            .await
            .map_err(|_| failed())?;
        let (redactor, codec, git_dir, parent, tree) = (
            Arc::clone(&self.file_redactor),
            Arc::clone(&self.codec),
            self.git_dir.clone(),
            self.parent.clone(),
            view.path().to_path_buf(),
        );
        tokio::task::spawn_blocking(move || {
            if !redactor.is_empty() {
                let parent = codec
                    .parent_files(&git_dir, &parent)
                    .map_err(|_| failed())?;
                redactor
                    .redact_tree(&tree, parent.as_ref())
                    .map_err(|_| failed())?;
            }
            let db =
                elitea_workspace_store::objects::ObjectDb::open_objects(&git_dir.join("objects"))
                    .map_err(|_| failed())?;
            super::pr_diff::pull_request_diff(&db, &upstream, &tree)
        })
        .await
        .map_err(|_| failed())?
    }

    /// End the turn. Returns the stamp of the published commit, or `None`
    /// when nothing was published (a failed turn).
    ///
    /// # Errors
    ///
    /// Any failure of the publish path: nothing was published, or Main
    /// refused the move (a stale epoch, a conflicting cloud ref).
    pub(crate) async fn finish(
        &self,
        end: TurnEnd,
    ) -> Result<Option<WorkspaceStamp>, WorkspaceRunError> {
        let mut outcome = self.outcome.lock().await;
        if let Some(previous) = outcome.as_ref() {
            return previous.clone();
        }
        let result = self.finish_once(end).await;
        *outcome = Some(result.clone());
        result
    }

    #[allow(
        clippy::too_many_lines,
        reason = "the publish path in order: pull, destroy, redact, commit, upload, publish"
    )]
    async fn finish_once(&self, end: TurnEnd) -> Result<Option<WorkspaceStamp>, WorkspaceRunError> {
        if end == TurnEnd::Failed {
            let _ = self.sandbox.destroy().await;
            return Ok(None);
        }
        let out = self.scratch.path().join("out");
        std::fs::create_dir_all(&out).map_err(|_| WorkspaceRunError::Internal)?;
        let pulled = self.sandbox.pull_tree(&out).await;
        // The sandbox lives for one turn whatever happens next.
        let _ = self.sandbox.destroy().await;
        pulled.map_err(WorkspaceRunError::Sandbox)?;

        // Contract 6.4: no harvested value leaves in a file. Every changed
        // or added file is scanned and redacted (binaries holding one are
        // dropped) before anything is committed.
        let redactions = {
            let (redactor, codec, git_dir, parent, tree) = (
                Arc::clone(&self.file_redactor),
                Arc::clone(&self.codec),
                self.git_dir.clone(),
                self.parent.clone(),
                out.clone(),
            );
            tokio::task::spawn_blocking(move || {
                if redactor.is_empty() {
                    return Ok(Vec::new());
                }
                let parent = codec
                    .parent_files(&git_dir, &parent)
                    .map_err(|_| super::file_redact::FileRedactError::Io)?;
                redactor.redact_tree(&tree, parent.as_ref())
            })
            .await
            .map_err(|_| WorkspaceRunError::Internal)?
            .map_err(|_| WorkspaceRunError::RedactionFailed)?
        };
        if !redactions.is_empty() {
            tracing::warn!(
                event = "workspace.env_redacted_in_files",
                execution_id = self.execution_id.as_str(),
                files = redactions.len(),
                dropped = redactions
                    .iter()
                    .filter(|redaction| redaction.dropped)
                    .count(),
                "harvested env values were found in files the turn wrote and were redacted"
            );
        }
        let mut trailers = String::new();
        for redaction in &redactions {
            trailers.push_str("Elitea-Redacted: ");
            trailers.push_str(&redaction.path);
            trailers.push('\n');
        }

        let commit = TurnCommit {
            parent: self.parent.clone(),
            message: format!(
                "Elitea cloud turn\n\nElitea-Execution-Id: {}\nElitea-Epoch: {}\n{trailers}",
                self.execution_id, self.epoch
            ),
            author_name: COMMIT_AUTHOR_NAME.to_owned(),
            author_email: COMMIT_AUTHOR_EMAIL.to_owned(),
            unix_seconds: chrono::Utc::now().timestamp(),
        };
        let codec = Arc::clone(&self.codec);
        let (git_dir, refs, max) = (
            self.git_dir.clone(),
            self.refs.clone(),
            self.max_segment_bytes,
        );
        let segment_out = self.scratch.path().join("turn.bundle");
        let built = tokio::task::spawn_blocking(move || {
            codec.commit_and_bundle(&git_dir, &out, &commit, &refs, max, &segment_out)
        })
        .await
        .map_err(|_| WorkspaceRunError::Internal)?
        .map_err(WorkspaceRunError::Codec)?;

        retry_once(|| {
            self.store
                .upload(&self.claim, &built.sha256, built.bytes, &built.path)
        })
        .await
        .map_err(WorkspaceRunError::Store)?;
        let request = PublishSegment {
            segment_sha256: built.sha256.clone(),
            bytes: built.bytes,
            old_cloud: self.expected_old.clone(),
            new_cloud: built.commit.clone(),
        };
        // A lost acknowledgement is retried as the SAME request: Main answers
        // an identical replay with the original result.
        let published = retry_once(|| self.store.publish(&self.claim, &request))
            .await
            .map_err(WorkspaceRunError::Store)?;
        tracing::info!(
            event = "workspace.published",
            execution_id = self.execution_id.as_str(),
            epoch = self.epoch,
            seq = published.seq,
            paused = end == TurnEnd::Paused,
        );
        Ok(Some(WorkspaceStamp {
            executed_by: EXECUTED_BY_CLOUD,
            workspace_epoch: self.epoch,
            workspace_commit: built.commit,
            seq: published.seq,
            redactions,
        }))
    }
}

impl Drop for WorkspaceTurn {
    fn drop(&mut self) {
        // A turn dropped without a finish (a panic, an abandoned assembly)
        // must not leave its container running.
        let finished = self
            .outcome
            .try_lock()
            .map_or(true, |outcome| outcome.is_some());
        if !finished && let Ok(handle) = tokio::runtime::Handle::try_current() {
            let sandbox = Arc::clone(&self.sandbox);
            handle.spawn(async move {
                let _ = sandbox.destroy().await;
            });
        }
    }
}

async fn retry_once<T, F, Fut>(call: F) -> Result<T, StoreCallError>
where
    F: Fn() -> Fut,
    Fut: std::future::Future<Output = Result<T, StoreCallError>>,
{
    match call().await {
        Err(error) if error.retryable() => call().await,
        other => other,
    }
}
