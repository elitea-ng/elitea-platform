//! The supervisor bridge end to end in one process: the worker's launcher
//! over real gRPC to the supervisor's workspace-run service, real Main-style
//! signed grants, and a fake container backend (a directory per run).

use std::collections::BTreeMap;
use std::path::Path;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use prost::Message as _;
use ring::signature::{self, KeyPair as _};
use serde_json::json;

use super::super::context::fixture;
use super::super::redact::SecretEnv;
use super::super::sandbox::{
    SandboxRunError, WorkspaceRunDefaults, WorkspaceRunGrants, WorkspaceRunSpec,
    WorkspaceSandboxLauncher,
};
use super::SupervisorWorkspaceLauncher;
use crate::protocol::command::Ed25519PublicKeyResolver;
use crate::protocol::elitea::runtime::v1::{SandboxJobGrantClaimsV1, SignedSandboxJobGrantV1};
use crate::protocol::sandbox_grant::GrantVerifier;
use crate::sandbox::workspace_run_service::fake::FakeBackend;
use crate::sandbox::workspace_run_service::{WorkspaceRunCeilings, WorkspaceRunSupervisor};

const AUDIENCE: &str = "dns:elitea-sandbox-workspace";
const PEER: &str = "dns:worker-1";
const GRANT_DOMAIN: &[u8] = b"elitea.sandbox.job-grant.ed25519.v1\0";

pub(crate) struct Keys([u8; 32]);

impl Ed25519PublicKeyResolver for Keys {
    fn resolve_ed25519_public_key(&self, id: &str) -> Option<[u8; 32]> {
        (id == "key-1").then_some(self.0)
    }
}

fn key() -> signature::Ed25519KeyPair {
    signature::Ed25519KeyPair::from_seed_unchecked(&[7; 32]).expect("key")
}

/// Main's side, as the test sees it: grants for one execution.
pub(crate) struct TestGrants {
    execution_id: String,
    peer: String,
    pub(crate) issued: AtomicUsize,
}

impl TestGrants {
    pub(crate) fn new(execution_id: &str) -> Arc<Self> {
        Arc::new(Self {
            execution_id: execution_id.to_owned(),
            peer: PEER.to_owned(),
            issued: AtomicUsize::new(0),
        })
    }
}

fn now_millis() -> i64 {
    i64::try_from(
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .expect("clock")
            .as_millis(),
    )
    .expect("millis")
}

#[async_trait]
impl WorkspaceRunGrants for TestGrants {
    async fn grant(
        &self,
        activation_id: &str,
        digest: [u8; 32],
    ) -> Result<SignedSandboxJobGrantV1, SandboxRunError> {
        self.issued.fetch_add(1, Ordering::SeqCst);
        let now = now_millis();
        let claims = SandboxJobGrantClaimsV1 {
            revision: 1,
            tenant_id: "tenant".into(),
            project_id: 2,
            execution_id: self.execution_id.clone(),
            activation_id: activation_id.to_owned(),
            request_digest: digest.to_vec(),
            submitter_workload_identity: self.peer.clone(),
            audience: AUDIENCE.into(),
            issued_at_unix_millis: now - 1_000,
            expires_at_unix_millis: now + 20_000,
            generation: 1,
            ..Default::default()
        };
        let bytes = claims.encode_to_vec();
        let mut input = GRANT_DOMAIN.to_vec();
        input.extend_from_slice(&(bytes.len() as u64).to_be_bytes());
        input.extend_from_slice(&bytes);
        Ok(SignedSandboxJobGrantV1 {
            key_id: "key-1".into(),
            claims_bytes: bytes,
            signature: key().sign(&input).as_ref().to_vec(),
        })
    }
}

/// A running supervisor on a loopback port.
pub(crate) struct Harness {
    pub(crate) backend: Arc<FakeBackend>,
    pub(crate) supervisor: Arc<WorkspaceRunSupervisor<Keys>>,
    pub(crate) launcher: SupervisorWorkspaceLauncher,
    _server: tokio::task::JoinHandle<()>,
}

pub(crate) async fn harness(concurrency: usize) -> Harness {
    let backend = Arc::new(FakeBackend::default());
    let (supervisor, launcher, server) = serve(
        Arc::clone(&backend) as Arc<dyn crate::sandbox::workspace_run_service::WorkspaceRunBackend>,
        format!("registry.example/runner@sha256:{}", "c".repeat(64)),
        concurrency,
    )
    .await;
    Harness {
        backend,
        supervisor,
        launcher,
        _server: server,
    }
}

/// A supervisor over `backend` on a loopback port, and a launcher to it.
pub(crate) async fn serve(
    backend: Arc<dyn crate::sandbox::workspace_run_service::WorkspaceRunBackend>,
    image: String,
    concurrency: usize,
) -> (
    Arc<WorkspaceRunSupervisor<Keys>>,
    SupervisorWorkspaceLauncher,
    tokio::task::JoinHandle<()>,
) {
    let mut public = [0; 32];
    public.copy_from_slice(key().public_key().as_ref());
    let verifier = GrantVerifier::new(Keys(public), AUDIENCE.into()).expect("verifier");
    let supervisor = Arc::new(
        WorkspaceRunSupervisor::new(
            verifier,
            backend,
            WorkspaceRunCeilings {
                image,
                max_workspace_bytes: 64 << 30,
                max_cpu_millis: 8_000,
                max_memory_bytes: 16 << 30,
                max_wall_seconds: 7_200,
                concurrency,
            },
        )
        .with_fixed_peer(PEER),
    );
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    let address = listener.local_addr().expect("address");
    let service = Arc::clone(&supervisor).into_service();
    let server = tokio::spawn(async move {
        let _ = tonic::transport::Server::builder()
            .add_service(service)
            .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener))
            .await;
    });
    let channel = tonic::transport::Endpoint::from_shared(format!("http://{address}"))
        .expect("endpoint")
        .connect()
        .await
        .expect("connect");
    (
        supervisor,
        SupervisorWorkspaceLauncher::new(channel),
        server,
    )
}

pub(crate) fn spec(run: &str) -> WorkspaceRunSpec {
    WorkspaceRunSpec::from_context(
        format!("elitea-ws-{run}"),
        &fixture(),
        &WorkspaceRunDefaults {
            cpu_millis: 2_000,
            memory_bytes: 4 << 30,
            max_disk_bytes: 8 << 30,
            turn_wall: Duration::from_mins(10),
            host_allowlist_proxy: false,
        },
    )
    .expect("spec")
}

fn read_tree(root: &Path) -> BTreeMap<String, String> {
    let mut out = BTreeMap::new();
    let mut stack = vec![root.to_path_buf()];
    while let Some(directory) = stack.pop() {
        for entry in std::fs::read_dir(directory).expect("dir") {
            let path = entry.expect("entry").path();
            if path.is_dir() {
                stack.push(path);
            } else {
                out.insert(
                    path.strip_prefix(root)
                        .expect("rel")
                        .to_string_lossy()
                        .into_owned(),
                    std::fs::read_to_string(&path).expect("read"),
                );
            }
        }
    }
    out
}

/// Item 1: start, unpack, serve, pack and destroy through the supervisor,
/// every call under a fresh grant, the env on stdin only.
#[tokio::test(flavor = "multi_thread")]
async fn a_workspace_run_starts_hydrates_serves_packs_and_is_destroyed() {
    let harness = harness(4).await;
    let grants = TestGrants::new("execution-1");
    let spec = spec("0123456789abcdef01234567");
    let env = SecretEnv::new(vec![("API_TOKEN".into(), "tok-123456".into())]);
    let sandbox = harness
        .launcher
        .launch(
            &spec,
            &env,
            Arc::clone(&grants) as Arc<dyn WorkspaceRunGrants>,
        )
        .await
        .expect("launched");

    let tree = tempfile::tempdir().expect("tree");
    std::fs::create_dir_all(tree.path().join("src")).expect("mkdir");
    std::fs::write(tree.path().join("src/main.rs"), "fn main() {}\n").expect("w");
    // A file several stream chunks long.
    std::fs::write(tree.path().join("big.txt"), "x".repeat(200_000)).expect("w");
    sandbox.push_tree(tree.path()).await.expect("unpacked");
    let run_dir = harness.backend.dir(&spec.run_id).expect("run");
    assert_eq!(read_tree(&run_dir), read_tree(tree.path()));

    let written = sandbox
        .call_tool(
            "write_file",
            "call-1",
            json!({"path": "NEW.md", "content": "hi\n"}),
        )
        .await
        .expect("tool");
    assert_eq!(written["status"], "ok");
    let read = sandbox
        .call_tool("read_file", "call-2", json!({"path": "big.txt"}))
        .await
        .expect("tool");
    assert_eq!(read["content"].as_str().map(str::len), Some(200_000));

    let out = tempfile::tempdir().expect("out");
    sandbox.pull_tree(out.path()).await.expect("packed");
    let pulled = read_tree(out.path());
    assert_eq!(pulled["NEW.md"], "hi\n");
    assert_eq!(pulled["src/main.rs"], "fn main() {}\n");

    sandbox.destroy().await.expect("destroyed");
    sandbox.destroy().await.expect("idempotent");
    assert!(harness.backend.dir(&spec.run_id).is_none());

    // The env reached the serve exec's first stdin line, and no argv.
    let env_lines = harness.backend.env_seen.lock().expect("env").clone();
    assert_eq!(env_lines.len(), 1);
    assert!(env_lines[0].contains("tok-123456"));
    let argvs = harness.backend.argv_seen.lock().expect("argv").clone();
    let operations: Vec<&str> = argvs.iter().map(|argv| argv[1].as_str()).collect();
    assert_eq!(operations, ["serve", "unpack", "pack"]);
    for argv in &argvs {
        assert!(!argv.join(" ").contains("tok-123456"), "{argv:?}");
        assert!(argv.contains(&"--max-workspace-bytes".to_owned()));
        assert!(argv.contains(&spec.workspace_bytes.to_string()));
    }
    let serve = &argvs[0];
    assert!(serve.contains(&"--env-stdin".to_owned()));
    // start, serve, unpack, pack, destroy: each call its own grant.
    assert_eq!(grants.issued.load(Ordering::SeqCst), 5);
}

/// Another claim's grant cannot reach a run, and a grant for another spec
/// is no grant at all.
#[tokio::test(flavor = "multi_thread")]
async fn a_run_belongs_to_the_claim_that_started_it() {
    let harness = harness(4).await;
    let spec = spec("aaaaaaaaaaaaaaaaaaaaaaaa");
    let owner = TestGrants::new("execution-1");
    let _sandbox = harness
        .launcher
        .launch(
            &spec,
            &SecretEnv::default(),
            owner as Arc<dyn WorkspaceRunGrants>,
        )
        .await
        .expect("launched");
    // A second execution names the same run: refused.
    let intruder = TestGrants::new("execution-2");
    assert!(
        harness
            .launcher
            .launch(
                &spec,
                &SecretEnv::default(),
                intruder as Arc<dyn WorkspaceRunGrants>
            )
            .await
            .is_err()
    );

    let other = spec_other();
    assert_eq!(
        harness
            .launcher
            .launch(
                &other,
                &SecretEnv::default(),
                Arc::new(WrongDigest(TestGrants::new("execution-3")))
            )
            .await
            .err(),
        Some(SandboxRunError::PolicyUnenforceable)
    );
    assert!(
        harness.backend.dir(&other.run_id).is_none(),
        "nothing started"
    );
}

/// Grants over a digest other than the request's.
struct WrongDigest(Arc<TestGrants>);
#[async_trait]
impl WorkspaceRunGrants for WrongDigest {
    async fn grant(
        &self,
        activation_id: &str,
        _digest: [u8; 32],
    ) -> Result<SignedSandboxJobGrantV1, SandboxRunError> {
        self.0.grant(activation_id, [9; 32]).await
    }
}

fn spec_other() -> WorkspaceRunSpec {
    spec("bbbbbbbbbbbbbbbbbbbbbbbb")
}

/// The profile's concurrency bounds live runs, and the sweep removes a run
/// past its wall even when no worker destroys it.
#[tokio::test(flavor = "multi_thread")]
async fn runs_are_bounded_and_swept_past_their_wall() {
    let harness = harness(1).await;
    let first = harness
        .launcher
        .launch(
            &spec("cccccccccccccccccccccccc"),
            &SecretEnv::default(),
            TestGrants::new("execution-1") as Arc<dyn WorkspaceRunGrants>,
        )
        .await
        .expect("first");
    assert!(
        harness
            .launcher
            .launch(
                &spec("dddddddddddddddddddddddd"),
                &SecretEnv::default(),
                TestGrants::new("execution-2") as Arc<dyn WorkspaceRunGrants>,
            )
            .await
            .is_err(),
        "over the profile's concurrency"
    );
    // A crashed worker never destroys its run: the sweep does, after the wall.
    drop(first);
    let far_future = u64::MAX / 2;
    assert_eq!(
        harness.supervisor.sweep(far_future).await.expect("swept"),
        1
    );
    assert!(harness.backend.runs.lock().expect("runs").is_empty());
    assert_eq!(harness.supervisor.sweep(0).await.expect("swept"), 0);
}

/// A run whose tool stream fails is refused, not retried silently.
#[tokio::test(flavor = "multi_thread")]
async fn no_grant_no_run() {
    let harness = harness(4).await;
    assert_eq!(
        harness
            .launcher
            .launch(
                &spec("eeeeeeeeeeeeeeeeeeeeeeee"),
                &SecretEnv::default(),
                Arc::new(super::super::sandbox::NoGrants),
            )
            .await
            .err(),
        Some(SandboxRunError::Unavailable)
    );
    let _ = Mutex::new(());
}
