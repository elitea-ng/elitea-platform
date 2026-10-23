//! The workspace turn over a fake store, codec and sandbox: hydrate, one
//! publish per turn, none on failure, the HITL pause, the epoch fence.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use serde_json::{Value, json};

use super::codec::{
    BuiltSegment, CodecError, HydratedTree, SegmentExpectation, TurnCommit, WorkspaceCodec,
};
use super::context::{WorkspaceRuntimeContext, fixture};
use super::redact::SecretEnv;
use super::sandbox::{
    SandboxRunError, WorkspaceRunDefaults, WorkspaceRunSpec, WorkspaceSandbox,
    WorkspaceSandboxLauncher,
};
use super::store::{
    FetchedThread, PlannedSegment, PublishSegment, PublishedSegment, REF_CLOUD, RefusalCode,
    StoreCallError, WorkspaceStore, sha256_hex,
};
use super::turn::{TurnEnd, WorkspaceRunError, WorkspaceRunFactory};
use crate::protocol::elitea::runtime::v1::WorkspaceClaimV1;

const HANDOFF: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const BASE: &str = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
const CLOUD: &str = "cccccccccccccccccccccccccccccccccccccccc";
const PARENT: &str = "dddddddddddddddddddddddddddddddddddddddd";

/// A "segment" is the JSON of its files; the codec reads it back.
fn segment(files: &BTreeMap<String, String>) -> Vec<u8> {
    serde_json::to_vec(files).expect("json")
}

#[derive(Default)]
struct StoreState {
    epoch: u64,
    refs: BTreeMap<String, String>,
    segment: Vec<u8>,
    uploads: Vec<String>,
    publishes: Vec<PublishSegment>,
    lose_first_publish_ack: bool,
    upload_fails: bool,
    env: Vec<(String, String)>,
    env_reads: u32,
    /// Executor audit events Main recorded.
    events: Vec<super::audit::ExecutorEvent>,
    /// Answer this many record-event calls with a non-retryable refusal.
    events_refused: u32,
}

struct FakeStore(Mutex<StoreState>);

impl FakeStore {
    fn new(refs: &[(&str, &str)], files: &BTreeMap<String, String>) -> Arc<Self> {
        Arc::new(Self(Mutex::new(StoreState {
            epoch: 2,
            refs: refs
                .iter()
                .map(|(name, sha)| ((*name).to_owned(), (*sha).to_owned()))
                .collect(),
            segment: segment(files),
            ..StoreState::default()
        })))
    }

    fn state(&self) -> std::sync::MutexGuard<'_, StoreState> {
        self.0.lock().expect("store")
    }
}

#[async_trait]
impl WorkspaceStore for FakeStore {
    async fn fetch(
        &self,
        claim: &WorkspaceClaimV1,
        _since: u64,
        _max: u64,
        into: &Path,
    ) -> Result<FetchedThread, StoreCallError> {
        let state = self.state();
        if claim.epoch != state.epoch {
            return Err(StoreCallError::Refused {
                code: RefusalCode::StaleEpoch,
                current: BTreeMap::new(),
            });
        }
        Ok(FetchedThread {
            head_seq: 1,
            refs: state.refs.clone(),
            segment: PlannedSegment {
                seq: 1,
                sha256: sha256_hex(&state.segment),
                bytes: state.segment.len() as u64,
                refs: state.refs.clone(),
            },
        })
        .inspect(|_| std::fs::write(into, &state.segment).expect("segment file"))
    }

    async fn upload(
        &self,
        _claim: &WorkspaceClaimV1,
        sha256: &str,
        bytes: u64,
        segment: &Path,
    ) -> Result<bool, StoreCallError> {
        let mut state = self.state();
        if state.upload_fails {
            return Err(StoreCallError::Transport { retryable: false });
        }
        let content = std::fs::read(segment).expect("segment file");
        assert_eq!(content.len() as u64, bytes);
        assert_eq!(sha256_hex(&content), sha256);
        state.uploads.push(sha256.to_owned());
        Ok(false)
    }

    async fn publish(
        &self,
        claim: &WorkspaceClaimV1,
        request: &PublishSegment,
    ) -> Result<PublishedSegment, StoreCallError> {
        let mut state = self.state();
        if claim.epoch != state.epoch {
            return Err(StoreCallError::Refused {
                code: RefusalCode::StaleEpoch,
                current: BTreeMap::new(),
            });
        }
        // An identical replay returns the original result.
        if state.publishes.last() == Some(request) {
            return Ok(PublishedSegment {
                seq: 1 + state.publishes.len() as u64,
                refs: state.refs.clone(),
            });
        }
        if state.refs.get(REF_CLOUD) != request.old_cloud.as_ref() {
            return Err(StoreCallError::Refused {
                code: RefusalCode::RefConflict,
                current: state.refs.clone(),
            });
        }
        state
            .refs
            .insert(REF_CLOUD.to_owned(), request.new_cloud.clone());
        state.publishes.push(request.clone());
        if state.lose_first_publish_ack {
            state.lose_first_publish_ack = false;
            return Err(StoreCallError::Transport { retryable: true });
        }
        Ok(PublishedSegment {
            seq: 1 + state.publishes.len() as u64,
            refs: state.refs.clone(),
        })
    }

    async fn env_secret(&self, _claim: &WorkspaceClaimV1) -> Result<SecretEnv, StoreCallError> {
        let mut state = self.state();
        state.env_reads += 1;
        Ok(SecretEnv::new(state.env.clone()))
    }

    async fn record_event(
        &self,
        _claim: &WorkspaceClaimV1,
        event: &super::audit::ExecutorEvent,
    ) -> Result<(), StoreCallError> {
        let mut state = self.state();
        if state.events_refused > 0 {
            state.events_refused -= 1;
            return Err(StoreCallError::Refused {
                code: super::store::RefusalCode::StaleEpoch,
                current: BTreeMap::new(),
            });
        }
        state.events.push(event.clone());
        Ok(())
    }
}

#[derive(Default)]
struct FakeCodec {
    commits: Mutex<Vec<TurnCommit>>,
    included: Mutex<Vec<String>>,
    /// The tree hydrated (the parent's files) and the last committed tree.
    hydrated: Mutex<BTreeMap<String, String>>,
    committed: Mutex<BTreeMap<String, String>>,
}

struct FakeParent(BTreeMap<String, String>);

impl super::file_redact::ParentTree for FakeParent {
    fn unchanged(&self, path: &str, content: &[u8]) -> bool {
        self.0
            .get(path)
            .is_some_and(|seen| seen.as_bytes() == content)
    }
    fn unchanged_file(&self, path: &str, file: &Path, _: u64) -> bool {
        self.unchanged(path, &std::fs::read(file).expect("read"))
    }
    fn content(&self, path: &str) -> Option<(Vec<u8>, bool)> {
        self.0
            .get(path)
            .map(|content| (content.clone().into_bytes(), false))
    }
}

fn read_tree(root: &Path) -> BTreeMap<String, String> {
    let mut files = BTreeMap::new();
    let mut stack = vec![root.to_path_buf()];
    while let Some(directory) = stack.pop() {
        for entry in std::fs::read_dir(directory).expect("dir") {
            let entry = entry.expect("entry");
            let path = entry.path();
            if path.is_dir() {
                stack.push(path);
            } else {
                let name = path
                    .strip_prefix(root)
                    .expect("rel")
                    .to_string_lossy()
                    .into_owned();
                files.insert(name, std::fs::read_to_string(&path).expect("read"));
            }
        }
    }
    files
}

fn write_tree(root: &Path, files: &BTreeMap<String, String>) {
    for (name, content) in files {
        let path = root.join(name);
        std::fs::create_dir_all(path.parent().expect("parent")).expect("mkdir");
        std::fs::write(path, content).expect("write");
    }
}

impl WorkspaceCodec for FakeCodec {
    fn hydrate(
        &self,
        segment: &Path,
        expect: &SegmentExpectation,
        _checkout: &str,
        _git_dir: &Path,
        worktree: &Path,
    ) -> Result<HydratedTree, CodecError> {
        let segment = std::fs::read(segment).map_err(|_| CodecError::Io)?;
        let segment = segment.as_slice();
        if sha256_hex(segment) != expect.sha256 || segment.len() as u64 != expect.bytes {
            return Err(CodecError::Integrity);
        }
        let files: BTreeMap<String, String> =
            serde_json::from_slice(segment).map_err(|_| CodecError::Integrity)?;
        write_tree(worktree, &files);
        *self.hydrated.lock().expect("hydrated") = files.clone();
        Ok(HydratedTree {
            files: files.len() as u64,
            unpacked_bytes: 0,
            included_excluded: self.included.lock().expect("included").clone(),
        })
    }

    fn commit_and_bundle(
        &self,
        _git_dir: &Path,
        worktree: &Path,
        commit: &TurnCommit,
        refs: &BTreeMap<String, String>,
        _max: u64,
        out: &Path,
    ) -> Result<BuiltSegment, CodecError> {
        self.commits.lock().expect("commits").push(commit.clone());
        let files = read_tree(worktree);
        *self.committed.lock().expect("committed") = files.clone();
        let bytes =
            serde_json::to_vec(&json!({"files": files, "refs": refs, "parent": commit.parent}))
                .expect("json");
        let commit_sha = sha256_hex(&bytes)[..40].to_owned();
        std::fs::write(out, &bytes).map_err(|_| CodecError::Io)?;
        Ok(BuiltSegment {
            commit: commit_sha,
            path: out.to_path_buf(),
            sha256: sha256_hex(&bytes),
            bytes: bytes.len() as u64,
        })
    }

    fn parent_files(
        &self,
        _git_dir: &Path,
        _parent: &str,
    ) -> Result<Box<dyn super::file_redact::ParentTree + Send>, CodecError> {
        Ok(Box::new(FakeParent(
            self.hydrated.lock().expect("hydrated").clone(),
        )))
    }

    fn rewind(
        &self,
        _git_dir: &Path,
        expect: &SegmentExpectation,
        cloud: &str,
        to: Option<&str>,
        worktree: &Path,
        _max: u64,
        out: &Path,
    ) -> Result<(String, BuiltSegment), CodecError> {
        assert_eq!(expect.refs.get(REF_CLOUD).map(String::as_str), Some(cloud));
        // The parent's tree: the fake keeps one tree, the hydrated one.
        write_tree(worktree, &self.hydrated.lock().expect("hydrated"));
        let bytes = format!("rewind {cloud}").into_bytes();
        std::fs::write(out, &bytes).map_err(|_| CodecError::Io)?;
        let target = to.unwrap_or(PARENT).to_owned();
        Ok((
            target.clone(),
            BuiltSegment {
                commit: target,
                path: out.to_path_buf(),
                sha256: sha256_hex(&bytes),
                bytes: bytes.len() as u64,
            },
        ))
    }
}

#[derive(Default)]
struct FakeSandbox {
    files: Mutex<BTreeMap<String, String>>,
    destroyed: Mutex<u32>,
    /// What the runner's `run_command` environment holds.
    env: Mutex<Vec<(String, String)>>,
    args_seen: Mutex<Vec<Value>>,
}

#[async_trait]
impl WorkspaceSandbox for FakeSandbox {
    async fn push_tree(&self, worktree: &Path) -> Result<(), SandboxRunError> {
        *self.files.lock().expect("files") = read_tree(worktree);
        Ok(())
    }

    async fn call_tool(
        &self,
        tool: &str,
        _call_id: &str,
        args: Value,
    ) -> Result<Value, SandboxRunError> {
        self.args_seen.lock().expect("args").push(args.clone());
        if tool == "run_command" {
            // A command that prints its environment.
            let env = self.env.lock().expect("env");
            let printed: Vec<String> = env
                .iter()
                .map(|(name, value)| format!("{name}={value}"))
                .collect();
            return Ok(json!({"status": "ok", "stdout": printed.join("\n")}));
        }
        if tool == "write_file" {
            self.files.lock().expect("files").insert(
                args["path"].as_str().unwrap_or_default().to_owned(),
                args["content"].as_str().unwrap_or_default().to_owned(),
            );
        }
        Ok(json!({"status": "ok"}))
    }

    async fn pull_tree(&self, worktree: &Path) -> Result<(), SandboxRunError> {
        write_tree(worktree, &self.files.lock().expect("files"));
        Ok(())
    }

    async fn destroy(&self) -> Result<(), SandboxRunError> {
        *self.destroyed.lock().expect("destroyed") += 1;
        Ok(())
    }
}

#[derive(Default)]
struct FakeLauncher {
    launched: Mutex<Vec<WorkspaceRunSpec>>,
    sandbox: Arc<FakeSandbox>,
}

#[async_trait]
impl WorkspaceSandboxLauncher for FakeLauncher {
    async fn launch(
        &self,
        spec: &WorkspaceRunSpec,
        env: &SecretEnv,
        _grants: Arc<dyn super::sandbox::WorkspaceRunGrants>,
    ) -> Result<Arc<dyn WorkspaceSandbox>, SandboxRunError> {
        self.launched.lock().expect("launched").push(spec.clone());
        *self.sandbox.env.lock().expect("env") = env
            .pairs()
            .map(|(name, value)| (name.to_owned(), value.to_owned()))
            .collect();
        Ok(Arc::clone(&self.sandbox) as Arc<dyn WorkspaceSandbox>)
    }
}

struct Harness {
    store: Arc<FakeStore>,
    codec: Arc<FakeCodec>,
    launcher: Arc<FakeLauncher>,
    factory: WorkspaceRunFactory,
    _scratch: tempfile::TempDir,
}

fn harness(refs: &[(&str, &str)], files: &[(&str, &str)]) -> Harness {
    let files: BTreeMap<String, String> = files
        .iter()
        .map(|(name, content)| ((*name).to_owned(), (*content).to_owned()))
        .collect();
    let store = FakeStore::new(refs, &files);
    let codec = Arc::new(FakeCodec::default());
    let launcher = Arc::new(FakeLauncher::default());
    let scratch = tempfile::tempdir().expect("scratch");
    let factory = WorkspaceRunFactory::new(
        Arc::clone(&store) as Arc<dyn WorkspaceStore>,
        Arc::clone(&codec) as Arc<dyn WorkspaceCodec>,
        Arc::clone(&launcher) as Arc<dyn WorkspaceSandboxLauncher>,
        WorkspaceRunDefaults {
            cpu_millis: 2_000,
            memory_bytes: 4 << 30,
            max_disk_bytes: 8 << 30,
            turn_wall: Duration::from_mins(10),
        },
        PathBuf::from(scratch.path()),
    );
    Harness {
        store,
        codec,
        launcher,
        factory,
        _scratch: scratch,
    }
}

fn claim(epoch: u64) -> WorkspaceClaimV1 {
    WorkspaceClaimV1 {
        thread_uuid: fixture().thread,
        epoch,
        claim_id: "claim-1".to_owned(),
        ..WorkspaceClaimV1::default()
    }
}

fn first_turn() -> WorkspaceRuntimeContext {
    fixture()
}

fn later_turn() -> WorkspaceRuntimeContext {
    WorkspaceRuntimeContext {
        expected_old_cloud: Some(CLOUD.to_owned()),
        ..fixture()
    }
}

const HANDOFF_REFS: &[(&str, &str)] = &[
    ("refs/elitea/base", BASE),
    ("refs/elitea/handoff/1", HANDOFF),
];
const CLOUD_REFS: &[(&str, &str)] = &[
    ("refs/elitea/base", BASE),
    ("refs/elitea/handoff/1", HANDOFF),
    ("refs/elitea/cloud", CLOUD),
];

#[tokio::test]
async fn a_completed_turn_publishes_exactly_one_segment_on_the_expected_cloud_ref() {
    let harness = harness(CLOUD_REFS, &[("src/main.rs", "fn main() {}\n")]);
    let turn = harness
        .factory
        .begin(
            &later_turn(),
            claim(2),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .expect("hydrated");
    // The sandbox got the checked-out tree; a tool writes a file.
    assert_eq!(
        harness.launcher.sandbox.files.lock().expect("files").len(),
        1
    );
    turn.sandbox()
        .call_tool(
            "write_file",
            "call-1",
            json!({"path": "README.md", "content": "hi\n"}),
        )
        .await
        .expect("tool");

    let stamp = turn
        .finish(TurnEnd::Completed)
        .await
        .expect("published")
        .expect("a stamp");
    // A second finish is the first one's result, not a second publish.
    assert_eq!(
        turn.finish(TurnEnd::Completed).await.expect("same"),
        Some(stamp.clone())
    );

    let state = harness.store.state();
    assert_eq!(state.uploads.len(), 1);
    assert_eq!(state.publishes.len(), 1);
    assert_eq!(state.publishes[0].old_cloud.as_deref(), Some(CLOUD));
    assert_eq!(state.refs.get(REF_CLOUD), Some(&stamp.workspace_commit));
    assert_eq!(stamp.executed_by, "cloud_executor");
    assert_eq!(stamp.workspace_epoch, 2);

    let commits = harness.codec.commits.lock().expect("commits");
    assert_eq!(commits.len(), 1);
    assert_eq!(commits[0].parent, CLOUD);
    assert_eq!(commits[0].author_name, "Elitea cloud executor");
    assert!(
        commits[0]
            .message
            .contains("\nElitea-Execution-Id: execution-1\n")
    );
    assert!(commits[0].message.contains("\nElitea-Epoch: 2\n"));
    assert_eq!(*harness.launcher.sandbox.destroyed.lock().expect("d"), 1);
    // The spec carries the policy: network on, disk sized from the tree.
    let launched = harness.launcher.launched.lock().expect("launched");
    assert_eq!(launched.len(), 1);
    assert!(launched[0].workspace_bytes >= fixture().limits.unpacked_bytes);
}

#[tokio::test]
async fn the_first_cloud_turn_starts_from_the_handoff_snapshot() {
    let harness = harness(HANDOFF_REFS, &[("a.txt", "a\n")]);
    let turn = harness
        .factory
        .begin(
            &first_turn(),
            claim(2),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .expect("hydrated");
    turn.finish(TurnEnd::Completed).await.expect("published");
    assert_eq!(harness.codec.commits.lock().expect("c")[0].parent, HANDOFF);
    assert_eq!(harness.store.state().publishes[0].old_cloud, None);
}

#[tokio::test]
async fn a_failed_turn_publishes_nothing_and_destroys_the_sandbox() {
    let harness = harness(CLOUD_REFS, &[("a.txt", "a\n")]);
    let turn = harness
        .factory
        .begin(
            &later_turn(),
            claim(2),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .expect("hydrated");
    assert_eq!(turn.finish(TurnEnd::Failed).await.expect("ok"), None);
    let state = harness.store.state();
    assert!(state.uploads.is_empty() && state.publishes.is_empty());
    assert_eq!(state.refs.get(REF_CLOUD).map(String::as_str), Some(CLOUD));
    assert_eq!(*harness.launcher.sandbox.destroyed.lock().expect("d"), 1);
}

#[tokio::test]
async fn a_pause_for_input_publishes_the_work_so_far() {
    let harness = harness(CLOUD_REFS, &[("a.txt", "a\n")]);
    let turn = harness
        .factory
        .begin(
            &later_turn(),
            claim(2),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .expect("hydrated");
    let stamp = turn.finish(TurnEnd::Paused).await.expect("published");
    assert!(stamp.is_some());
    assert_eq!(harness.store.state().publishes.len(), 1);
}

#[tokio::test]
async fn a_stale_epoch_moves_nothing() {
    let harness = harness(CLOUD_REFS, &[("a.txt", "a\n")]);
    let turn = harness
        .factory
        .begin(
            &later_turn(),
            claim(2),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .expect("hydrated");
    // Take back happened meanwhile: the thread's epoch moved on.
    harness.store.state().epoch = 3;
    let error = turn.finish(TurnEnd::Completed).await.expect_err("refused");
    assert_eq!(error.code(), "workspace_run.stale_epoch");
    let state = harness.store.state();
    assert_eq!(state.publishes, []);
    assert_eq!(state.refs.get(REF_CLOUD).map(String::as_str), Some(CLOUD));
}

#[tokio::test]
async fn a_lost_publish_acknowledgement_is_retried_as_the_same_request() {
    let harness = harness(CLOUD_REFS, &[("a.txt", "a\n")]);
    harness.store.state().lose_first_publish_ack = true;
    let turn = harness
        .factory
        .begin(
            &later_turn(),
            claim(2),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .expect("hydrated");
    let stamp = turn
        .finish(TurnEnd::Completed)
        .await
        .expect("published")
        .expect("stamp");
    let state = harness.store.state();
    assert_eq!(
        state.publishes.len(),
        1,
        "one segment, even across the retry"
    );
    assert_eq!(state.refs.get(REF_CLOUD), Some(&stamp.workspace_commit));
}

#[tokio::test]
async fn an_upload_failure_publishes_nothing() {
    let harness = harness(CLOUD_REFS, &[("a.txt", "a\n")]);
    harness.store.state().upload_fails = true;
    let turn = harness
        .factory
        .begin(
            &later_turn(),
            claim(2),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .expect("hydrated");
    assert!(turn.finish(TurnEnd::Completed).await.is_err());
    assert_eq!(harness.store.state().publishes, []);
}

#[tokio::test]
async fn a_secret_in_the_tree_fails_the_turn_before_any_sandbox_starts() {
    let harness = harness(
        CLOUD_REFS,
        &[("config/app.env", "AWS_ACCESS_KEY_ID=AKIAABCDEFGHIJKLMNOP\n")],
    );
    let error = harness
        .factory
        .begin(
            &later_turn(),
            claim(2),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .err()
        .expect("refused");
    assert_eq!(error, WorkspaceRunError::SecretFound { findings: 1 });
    assert!(harness.launcher.launched.lock().expect("l").is_empty());
    // Decision 12: Main recorded handoff.scan_blocked before the turn failed,
    // with the count and the rule ids, never the path.
    let events = harness.store.state().events.clone();
    assert_eq!(events.len(), 1);
    let super::audit::ExecutorEvent::ScanBlocked { findings, rules } = &events[0] else {
        panic!("scan_blocked expected: {events:?}");
    };
    assert_eq!(*findings, 1);
    assert_ne!(rules.as_slice(), [] as [String; 0]);
    assert!(
        rules
            .iter()
            .all(|rule| !rule.contains('/') && !rule.contains("app.env"))
    );
}

/// `pull_request.opened` reaches Main before the turn ends, with the file
/// count; an audit refusal is logged and never fails the turn.
#[tokio::test]
async fn executor_audit_events_reach_main_and_never_fail_the_turn() {
    use elitea_agent_runtime::toolkits::workspace_pull_request::{
        PullRequestAudit, PullRequestOpened,
    };
    let harness = harness(CLOUD_REFS, &[("src/main.rs", "fn main() {}\n")]);
    let turn = harness
        .factory
        .begin(
            &later_turn(),
            claim(2),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .expect("hydrated");
    let audit = super::tools::LogPullRequestAudit::new("execution-1").with_audit(turn.audit());
    let opened = PullRequestOpened {
        toolkit: "github".to_owned(),
        repository: "o/r".to_owned(),
        branch: "elitea/cloud-1".to_owned(),
        base: "main".to_owned(),
        number: 42,
        files: 3,
        upstream_sha: "u".repeat(40),
    };
    harness.store.state().events_refused = 1;
    audit.opened(&opened); // refused: logged, not retried, the turn goes on
    audit.opened(&opened);
    turn.finish(TurnEnd::Completed).await.expect("published");
    let state = harness.store.state();
    assert_eq!(state.publishes.len(), 1, "the turn published");
    assert_eq!(state.events.len(), 1, "flushed before the turn ended");
    assert!(matches!(
        &state.events[0],
        super::audit::ExecutorEvent::PullRequestOpened {
            files: 3,
            number: 42,
            ..
        }
    ));
}

#[tokio::test]
async fn a_cloud_ref_other_than_the_context_says_is_stale_state() {
    // The context believes the thread has no cloud turn yet.
    let harness = harness(CLOUD_REFS, &[("a.txt", "a\n")]);
    let error = harness
        .factory
        .begin(
            &first_turn(),
            claim(2),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .err()
        .expect("refused");
    assert_eq!(error, WorkspaceRunError::StaleState);
    // A stale epoch is refused by the fetch itself.
    let error = harness
        .factory
        .begin(
            &later_turn(),
            claim(1),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .err()
        .expect("refused");
    assert_eq!(error.code(), "workspace_run.stale_epoch");
    assert!(harness.launcher.launched.lock().expect("l").is_empty());
}

/// Contract 6.4: with `env_secret`, the env is redeemed once, reaches the
/// sandbox's `run_command` environment, is never in a tool argument, and is
/// redacted (base64 included) from what the tool returns.
#[tokio::test]
async fn the_harvested_env_reaches_run_command_and_is_redacted_from_its_output() {
    let harness = harness(CLOUD_REFS, &[("a.txt", "a\n")]);
    harness.store.state().env = vec![
        (
            "DATABASE_URL".to_owned(),
            "postgres://app:s3cret@db/app".to_owned(),
        ),
        ("SHORT".to_owned(), "abc".to_owned()),
    ];
    let context = WorkspaceRuntimeContext {
        env_secret: true,
        ..later_turn()
    };
    let turn = harness
        .factory
        .begin(
            &context,
            claim(2),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .expect("hydrated");
    assert_eq!(harness.store.state().env_reads, 1);
    assert!(
        harness
            .launcher
            .sandbox
            .env
            .lock()
            .expect("env")
            .iter()
            .any(|(name, _)| name == "DATABASE_URL"),
        "the command environment holds the value"
    );
    let result = turn
        .sandbox()
        .call_tool("run_command", "call-1", json!({"command": "env"}))
        .await
        .expect("tool");
    let text = result.to_string();
    assert!(!text.contains("s3cret"), "{text}");
    assert!(text.contains("[REDACTED]"));
    assert!(
        text.contains("SHORT=abc"),
        "under 4 characters is not redacted"
    );
    // Base64 of a value is redacted too.
    harness.launcher.sandbox.env.lock().expect("env").push((
        "ENCODED".to_owned(),
        base64::Engine::encode(
            &base64::engine::general_purpose::STANDARD,
            "postgres://app:s3cret@db/app",
        ),
    ));
    let encoded = turn
        .sandbox()
        .call_tool("run_command", "call-2", json!({"command": "env"}))
        .await
        .expect("tool")
        .to_string();
    assert!(encoded.contains("ENCODED=[REDACTED]"), "{encoded}");
    for args in harness
        .launcher
        .sandbox
        .args_seen
        .lock()
        .expect("args")
        .iter()
    {
        assert!(
            !args.to_string().contains("s3cret"),
            "a tool argument never carries the value"
        );
    }
    turn.finish(TurnEnd::Completed).await.expect("published");
}

#[tokio::test]
async fn without_env_secret_no_env_is_redeemed_or_injected() {
    let harness = harness(CLOUD_REFS, &[("a.txt", "a\n")]);
    harness.store.state().env = vec![("DATABASE_URL".to_owned(), "postgres://x:y@z/w".to_owned())];
    let turn = harness
        .factory
        .begin(
            &later_turn(),
            claim(2),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .expect("hydrated");
    assert_eq!(harness.store.state().env_reads, 0);
    assert!(harness.launcher.sandbox.env.lock().expect("env").is_empty());
    turn.finish(TurnEnd::Failed).await.expect("ok");
}

/// An explicit include of the send is not re-scanned (contract 6.4).
#[tokio::test]
async fn an_explicit_include_is_not_re_scanned() {
    let harness = harness(
        CLOUD_REFS,
        &[(".env", "AWS_ACCESS_KEY_ID=AKIAABCDEFGHIJKLMNOP\n")],
    );
    harness
        .codec
        .included
        .lock()
        .expect("included")
        .push("./.env".to_owned());
    let turn = harness
        .factory
        .begin(
            &later_turn(),
            claim(2),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .expect("the included file is the person's choice");
    turn.finish(TurnEnd::Failed).await.expect("ok");
}

/// Item 3: a file a command wrote with the harvested env in it (`env >
/// out.txt`) is published redacted, by NAME, and the stamp carries the
/// notice; an unchanged file is left as it was.
#[tokio::test]
async fn harvested_values_in_written_files_are_redacted_before_the_commit() {
    let harness = harness(CLOUD_REFS, &[("kept.txt", "API_TOKEN=tok-9f8e7d6c5b\n")]);
    harness.store.state().env = vec![("API_TOKEN".to_owned(), "tok-9f8e7d6c5b".to_owned())];
    let context = WorkspaceRuntimeContext {
        env_secret: true,
        ..later_turn()
    };
    let turn = harness
        .factory
        .begin(
            &context,
            claim(2),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .expect("hydrated");
    turn.sandbox()
        .call_tool(
            "write_file",
            "call-1",
            json!({"path": "out.txt", "content": "API_TOKEN=tok-9f8e7d6c5b\nPATH=/bin\n"}),
        )
        .await
        .expect("tool");
    let stamp = turn
        .finish(TurnEnd::Completed)
        .await
        .expect("published")
        .expect("stamp");
    let committed = harness.codec.committed.lock().expect("committed").clone();
    assert_eq!(
        committed["out.txt"],
        "API_TOKEN=[REDACTED:API_TOKEN]\nPATH=/bin\n"
    );
    assert_eq!(
        committed["kept.txt"], "API_TOKEN=tok-9f8e7d6c5b\n",
        "unchanged files stay"
    );
    assert_eq!(stamp.redactions.len(), 1);
    assert_eq!(stamp.redactions[0].path, "out.txt");
    assert_eq!(stamp.redactions[0].names, ["API_TOKEN"]);
    let message = &harness.codec.commits.lock().expect("commits")[0].message;
    assert!(message.contains("Elitea-Redacted: out.txt"), "{message}");
    assert!(!message.contains("tok-9f8e7d6c5b"));
    let notice = super::file_redact::notice(&stamp.redactions).expect("notice");
    assert!(notice.contains("out.txt") && notice.contains("API_TOKEN"));
    assert!(!notice.contains("tok-9f8e7d6c5b"));
}

/// Regenerate of the newest cloud turn (decision 7): before anything runs,
/// the cloud ref moves back from the regenerated turn's commit to its parent
/// through the normal publish compare-and-set, and the new turn publishes
/// on top of the parent.
#[tokio::test]
async fn a_regenerate_rewinds_the_cloud_ref_before_the_turn_runs() {
    let harness = harness(CLOUD_REFS, &[("a.txt", "a\n")]);
    let context = WorkspaceRuntimeContext {
        rewind: true,
        ..later_turn()
    };
    let turn = harness
        .factory
        .begin(
            &context,
            claim(2),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .expect("rewound and hydrated");
    {
        let state = harness.store.state();
        assert_eq!(state.publishes.len(), 1, "the rewind is published first");
        assert_eq!(state.publishes[0].old_cloud.as_deref(), Some(CLOUD));
        assert_eq!(state.publishes[0].new_cloud, PARENT);
        assert_eq!(state.refs.get(REF_CLOUD).map(String::as_str), Some(PARENT));
    }
    turn.finish(TurnEnd::Completed).await.expect("published");
    let state = harness.store.state();
    assert_eq!(state.publishes.len(), 2);
    assert_eq!(state.publishes[1].old_cloud.as_deref(), Some(PARENT));
    assert_eq!(
        harness.codec.commits.lock().expect("commits")[0].parent,
        PARENT
    );

    // A rewind without a cloud turn to rewind is malformed.
    let mut bad = first_turn();
    bad.rewind = true;
    assert!(bad.validate().is_err());
}

/// Host-allowlist mode (decision 9, "Host allowlist: the egress proxy"):
/// the run token reaches `run_command` only inside the proxy URLs, with an
/// empty `NO_PROXY`, the proxy as an address literal, and the token is
/// redacted from tool output and from written files like a harvested value.
#[tokio::test]
async fn proxy_mode_points_run_command_at_the_proxy_and_redacts_the_token() {
    use super::context::{TEST_TOKEN, proxy_fixture};
    let harness = harness(CLOUD_REFS, &[]);
    let context = WorkspaceRuntimeContext {
        expected_old_cloud: Some(CLOUD.to_owned()),
        ..proxy_fixture()
    };
    let turn = harness
        .factory
        .begin(
            &context,
            claim(2),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .expect("hydrated");
    assert_eq!(
        harness.launcher.launched.lock().expect("launched")[0].network,
        super::sandbox::WorkspaceNetwork::Proxy
    );
    let env: BTreeMap<String, String> = harness
        .launcher
        .sandbox
        .env
        .lock()
        .expect("env")
        .iter()
        .cloned()
        .collect();
    let url = format!("http://run:{TEST_TOKEN}@10.96.0.40:3128");
    for name in ["HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"] {
        assert_eq!(env[name], url, "{name}");
    }
    assert_eq!(env["NO_PROXY"], "");
    assert_eq!(env["no_proxy"], "");
    let output = turn
        .sandbox()
        .call_tool("run_command", "call-1", json!({"command": "env"}))
        .await
        .expect("tool")
        .to_string();
    assert!(!output.contains(TEST_TOKEN), "{output}");
    assert!(output.contains("HTTPS_PROXY=[REDACTED]"), "{output}");

    turn.sandbox()
        .call_tool(
            "write_file",
            "call-2",
            json!({"path": "leak.txt", "content": format!("token={TEST_TOKEN}\n")}),
        )
        .await
        .expect("tool");
    let stamp = turn
        .finish(TurnEnd::Completed)
        .await
        .expect("published")
        .expect("stamp");
    let committed = harness.codec.committed.lock().expect("committed").clone();
    assert!(!committed["leak.txt"].contains(TEST_TOKEN), "{committed:?}");
    assert_eq!(stamp.redactions[0].names, ["ELITEA_EGRESS_PROXY_TOKEN"]);
}

/// An allowlist this deployment cannot enforce fails the turn before any
/// sandbox starts, with its own code.
#[tokio::test]
async fn an_unavailable_egress_proxy_fails_the_turn_before_any_sandbox_starts() {
    use super::context::{EgressMode, ProxyToken, proxy_fixture};
    let harness = harness(CLOUD_REFS, &[]);
    let mut context = WorkspaceRuntimeContext {
        expected_old_cloud: Some(CLOUD.to_owned()),
        ..proxy_fixture()
    };
    let egress = context.egress.as_mut().expect("egress");
    egress.mode = EgressMode::Unavailable;
    egress.proxy_url.clear();
    egress.token = ProxyToken::empty();
    egress.reason = "egress_proxy_not_enabled".to_owned();
    let error = harness
        .factory
        .begin(
            &context,
            claim(2),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .err()
        .expect("refused");
    assert_eq!(error.code(), "workspace_run.egress_proxy_unavailable");
    assert!(
        harness
            .launcher
            .launched
            .lock()
            .expect("launched")
            .is_empty()
    );
}

#[tokio::test]
async fn the_proxy_name_is_resolved_to_an_address_literal() {
    use super::context::TEST_TOKEN;
    let url = super::turn::proxy_env_url("http://localhost:3128", TEST_TOKEN)
        .await
        .expect("resolved");
    assert!(
        url.as_str() == format!("http://run:{TEST_TOKEN}@127.0.0.1:3128")
            || url.as_str() == format!("http://run:{TEST_TOKEN}@[::1]:3128"),
        "an address literal"
    );
    let v6 = super::turn::proxy_env_url("http://[fd00::5]:3128", TEST_TOKEN)
        .await
        .expect("literal");
    assert_eq!(
        v6.as_str(),
        format!("http://run:{TEST_TOKEN}@[fd00::5]:3128")
    );
    assert_eq!(
        super::turn::proxy_env_url("http://run:x@p:3128", TEST_TOKEN).await,
        Err(SandboxRunError::EgressProxyUnavailable)
    );
}

/// Main names the rewind target (the commit the regenerated turn started
/// from, past a pause's commit): the rewind publishes exactly that move.
#[tokio::test]
async fn a_regenerate_rewinds_to_the_target_main_names() {
    const STARTED_FROM: &str = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee";
    let harness = harness(CLOUD_REFS, &[("a.txt", "a\n")]);
    let context = WorkspaceRuntimeContext {
        rewind: true,
        rewind_to: Some(STARTED_FROM.to_owned()),
        ..later_turn()
    };
    let turn = harness
        .factory
        .begin(
            &context,
            claim(2),
            "execution-1",
            Arc::new(super::sandbox::NoGrants),
        )
        .await
        .expect("rewound");
    {
        let state = harness.store.state();
        assert_eq!(state.publishes.len(), 1);
        assert_eq!(state.publishes[0].old_cloud.as_deref(), Some(CLOUD));
        assert_eq!(state.publishes[0].new_cloud, STARTED_FROM);
    }
    turn.finish(TurnEnd::Completed).await.expect("published");
    let state = harness.store.state();
    assert_eq!(state.publishes.len(), 2);
    assert_eq!(state.publishes[1].old_cloud.as_deref(), Some(STARTED_FROM));
}
