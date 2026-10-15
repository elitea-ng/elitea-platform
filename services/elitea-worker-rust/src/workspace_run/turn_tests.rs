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
use super::sandbox::{
    SandboxRunError, WorkspaceRunDefaults, WorkspaceRunSpec, WorkspaceSandbox,
    WorkspaceSandboxLauncher,
};
use super::store::{
    FetchedThread, PlannedSegment, PublishSegment, PublishedSegment, PullRequestDiff, REF_CLOUD,
    RefusalCode, StoreCallError, WorkspaceStore, sha256_hex,
};
use super::turn::{TurnEnd, WorkspaceRunError, WorkspaceRunFactory};
use crate::protocol::elitea::runtime::v1::WorkspaceClaimV1;

const HANDOFF: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const BASE: &str = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
const CLOUD: &str = "cccccccccccccccccccccccccccccccccccccccc";

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
            bytes: state.segment.clone(),
        })
    }

    async fn upload(
        &self,
        _claim: &WorkspaceClaimV1,
        sha256: &str,
        bytes: &[u8],
    ) -> Result<bool, StoreCallError> {
        let mut state = self.state();
        if state.upload_fails {
            return Err(StoreCallError::Transport { retryable: false });
        }
        assert_eq!(sha256_hex(bytes), sha256);
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

    async fn pull_request_diff(
        &self,
        _claim: &WorkspaceClaimV1,
    ) -> Result<PullRequestDiff, StoreCallError> {
        Err(StoreCallError::Refused {
            code: RefusalCode::NoUpstream,
            current: BTreeMap::new(),
        })
    }
}

#[derive(Default)]
struct FakeCodec {
    commits: Mutex<Vec<TurnCommit>>,
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
        segment: &[u8],
        expect: &SegmentExpectation,
        _checkout: &str,
        _git_dir: &Path,
        worktree: &Path,
    ) -> Result<HydratedTree, CodecError> {
        if sha256_hex(segment) != expect.sha256 || segment.len() as u64 != expect.bytes {
            return Err(CodecError::Integrity);
        }
        let files: BTreeMap<String, String> =
            serde_json::from_slice(segment).map_err(|_| CodecError::Integrity)?;
        write_tree(worktree, &files);
        Ok(HydratedTree {
            files: files.len() as u64,
            unpacked_bytes: 0,
        })
    }

    fn commit_and_bundle(
        &self,
        _git_dir: &Path,
        worktree: &Path,
        commit: &TurnCommit,
        refs: &BTreeMap<String, String>,
        _max: u64,
    ) -> Result<BuiltSegment, CodecError> {
        self.commits.lock().expect("commits").push(commit.clone());
        let files = read_tree(worktree);
        let bytes = serde_json::to_vec(&json!({"files": files, "refs": refs, "parent": commit.parent}))
            .expect("json");
        let commit_sha = sha256_hex(&bytes)[..40].to_owned();
        Ok(BuiltSegment {
            commit: commit_sha,
            sha256: sha256_hex(&bytes),
            bytes,
        })
    }
}

#[derive(Default)]
struct FakeSandbox {
    files: Mutex<BTreeMap<String, String>>,
    destroyed: Mutex<u32>,
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
    ) -> Result<Arc<dyn WorkspaceSandbox>, SandboxRunError> {
        self.launched.lock().expect("launched").push(spec.clone());
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
            turn_wall: Duration::from_secs(600),
            host_allowlist_proxy: false,
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
        .begin(&later_turn(), claim(2), "execution-1")
        .await
        .expect("hydrated");
    // The sandbox got the checked-out tree; a tool writes a file.
    assert_eq!(
        harness.launcher.sandbox.files.lock().expect("files").len(),
        1
    );
    turn.sandbox()
        .call_tool("write_file", "call-1", json!({"path": "README.md", "content": "hi\n"}))
        .await
        .expect("tool");

    let stamp = turn
        .finish(TurnEnd::Completed)
        .await
        .expect("published")
        .expect("a stamp");
    // A second finish is the first one's result, not a second publish.
    assert_eq!(turn.finish(TurnEnd::Completed).await.expect("same"), Some(stamp.clone()));

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
    assert!(commits[0].message.contains("\nElitea-Execution-Id: execution-1\n"));
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
        .begin(&first_turn(), claim(2), "execution-1")
        .await
        .expect("hydrated");
    turn.finish(TurnEnd::Completed).await.expect("published");
    assert_eq!(
        harness.codec.commits.lock().expect("c")[0].parent,
        HANDOFF
    );
    assert_eq!(harness.store.state().publishes[0].old_cloud, None);
}

#[tokio::test]
async fn a_failed_turn_publishes_nothing_and_destroys_the_sandbox() {
    let harness = harness(CLOUD_REFS, &[("a.txt", "a\n")]);
    let turn = harness
        .factory
        .begin(&later_turn(), claim(2), "execution-1")
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
        .begin(&later_turn(), claim(2), "execution-1")
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
        .begin(&later_turn(), claim(2), "execution-1")
        .await
        .expect("hydrated");
    // Take back happened meanwhile: the thread's epoch moved on.
    harness.store.state().epoch = 3;
    let error = turn.finish(TurnEnd::Completed).await.expect_err("refused");
    assert_eq!(error.code(), "workspace_run.stale_epoch");
    let state = harness.store.state();
    assert!(state.publishes.is_empty());
    assert_eq!(state.refs.get(REF_CLOUD).map(String::as_str), Some(CLOUD));
}

#[tokio::test]
async fn a_lost_publish_acknowledgement_is_retried_as_the_same_request() {
    let harness = harness(CLOUD_REFS, &[("a.txt", "a\n")]);
    harness.store.state().lose_first_publish_ack = true;
    let turn = harness
        .factory
        .begin(&later_turn(), claim(2), "execution-1")
        .await
        .expect("hydrated");
    let stamp = turn
        .finish(TurnEnd::Completed)
        .await
        .expect("published")
        .expect("stamp");
    let state = harness.store.state();
    assert_eq!(state.publishes.len(), 1, "one segment, even across the retry");
    assert_eq!(state.refs.get(REF_CLOUD), Some(&stamp.workspace_commit));
}

#[tokio::test]
async fn an_upload_failure_publishes_nothing() {
    let harness = harness(CLOUD_REFS, &[("a.txt", "a\n")]);
    harness.store.state().upload_fails = true;
    let turn = harness
        .factory
        .begin(&later_turn(), claim(2), "execution-1")
        .await
        .expect("hydrated");
    assert!(turn.finish(TurnEnd::Completed).await.is_err());
    assert!(harness.store.state().publishes.is_empty());
}

#[tokio::test]
async fn a_secret_in_the_tree_fails_the_turn_before_any_sandbox_starts() {
    let harness = harness(
        CLOUD_REFS,
        &[("config/app.env", "AWS_ACCESS_KEY_ID=AKIAABCDEFGHIJKLMNOP\n")],
    );
    let error = harness
        .factory
        .begin(&later_turn(), claim(2), "execution-1")
        .await
        .err()
        .expect("refused");
    assert_eq!(error, WorkspaceRunError::SecretFound { findings: 1 });
    assert!(harness.launcher.launched.lock().expect("l").is_empty());
}

#[tokio::test]
async fn a_cloud_ref_other_than_the_context_says_is_stale_state() {
    // The context believes the thread has no cloud turn yet.
    let harness = harness(CLOUD_REFS, &[("a.txt", "a\n")]);
    let error = harness
        .factory
        .begin(&first_turn(), claim(2), "execution-1")
        .await
        .err()
        .expect("refused");
    assert_eq!(error, WorkspaceRunError::StaleState);
    // A stale epoch is refused by the fetch itself.
    let error = harness
        .factory
        .begin(&later_turn(), claim(1), "execution-1")
        .await
        .err()
        .expect("refused");
    assert_eq!(error.code(), "workspace_run.stale_epoch");
    assert!(harness.launcher.launched.lock().expect("l").is_empty());
}
