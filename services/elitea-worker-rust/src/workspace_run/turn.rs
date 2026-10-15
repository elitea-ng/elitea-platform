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
    SecretFound { findings: usize },
    /// The secret scan could not complete.
    ScanFailed,
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
}

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
        }
    }

    /// The store, for the `open_pull_request` tool's diff.
    #[must_use]
    pub(crate) fn store(&self) -> Arc<dyn WorkspaceStore> {
        Arc::clone(&self.store)
    }

    /// Hydrate the thread and start the turn's sandbox.
    ///
    /// # Errors
    ///
    /// Any failure before the sandbox holds the tree; nothing is left
    /// running.
    pub(crate) async fn begin(
        &self,
        context: &WorkspaceRuntimeContext,
        claim: WorkspaceClaimV1,
        execution_id: &str,
    ) -> Result<Arc<WorkspaceTurn>, WorkspaceRunError> {
        let limits = &context.limits;
        let fetched = self
            .store
            .fetch(&claim, 0, limits.max_segment_bytes)
            .await
            .map_err(WorkspaceRunError::Store)?;
        let checkout = checkout_commit(context, &fetched.segment.refs)?;
        let scratch = tempfile::Builder::new()
            .prefix("elitea-workspace-")
            .tempdir_in(&self.scratch_root)
            .map_err(|_| WorkspaceRunError::Internal)?;
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
        let segment = fetched.bytes;
        let hydrated = tokio::task::spawn_blocking(move || {
            codec.hydrate(
                &segment,
                &expect,
                &hydrate_checkout,
                &hydrate_git,
                &hydrate_tree,
            )
        })
        .await
        .map_err(|_| WorkspaceRunError::Internal)?
        .map_err(WorkspaceRunError::Codec)?;

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
        let findings = tokio::task::spawn_blocking(move || scan_tree(&scan_root, scan_limits, &skip))
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
        let raw = self
            .launcher
            .launch(&spec, &env)
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
            expected_old: context.expected_old_cloud.clone(),
            refs: fetched.segment.refs,
            epoch: context.epoch,
            execution_id: execution_id.to_owned(),
            max_segment_bytes: limits.max_segment_bytes,
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
    outcome: Mutex<Option<Result<Option<WorkspaceStamp>, WorkspaceRunError>>>,
}

impl WorkspaceTurn {
    /// The sandbox the turn's tools run in.
    #[must_use]
    pub(crate) fn sandbox(&self) -> Arc<dyn WorkspaceSandbox> {
        Arc::clone(&self.sandbox)
    }

    /// The claim binding the turn's store calls carry.
    #[must_use]
    pub(crate) const fn claim(&self) -> &WorkspaceClaimV1 {
        &self.claim
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

        let commit = TurnCommit {
            parent: self.parent.clone(),
            message: format!(
                "Elitea cloud turn\n\nElitea-Execution-Id: {}\nElitea-Epoch: {}\n",
                self.execution_id, self.epoch
            ),
            author_name: COMMIT_AUTHOR_NAME.to_owned(),
            author_email: COMMIT_AUTHOR_EMAIL.to_owned(),
            unix_seconds: chrono::Utc::now().timestamp(),
        };
        let codec = Arc::clone(&self.codec);
        let (git_dir, refs, max) = (self.git_dir.clone(), self.refs.clone(), self.max_segment_bytes);
        let built = tokio::task::spawn_blocking(move || {
            codec.commit_and_bundle(&git_dir, &out, &commit, &refs, max)
        })
        .await
        .map_err(|_| WorkspaceRunError::Internal)?
        .map_err(WorkspaceRunError::Codec)?;

        retry_once(|| self.store.upload(&self.claim, &built.sha256, &built.bytes))
            .await
            .map_err(WorkspaceRunError::Store)?;
        let request = PublishSegment {
            segment_sha256: built.sha256.clone(),
            bytes: built.bytes.len() as u64,
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
            .map(|outcome| outcome.is_some())
            .unwrap_or(true);
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
