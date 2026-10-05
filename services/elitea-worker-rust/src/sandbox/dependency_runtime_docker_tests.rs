//! Opt-in binary transfer proof against the real Linux Docker runtime.
//! Test-owned jobs use a fixed holding process. The source sentinel never executes.
use super::runtime::CodeJobRuntime;
use adk_sandbox::workspace::{
    DockerClient, Manifest, ManifestEntry, SandboxClient, SandboxSession, docker::CodeJobIdentity,
};
use futures_util::FutureExt as _;
use ring::{
    digest,
    rand::{SecureRandom as _, SystemRandom},
};
use std::{fmt::Write as _, panic::AssertUnwindSafe, path::Path, time::Duration};
use tokio::io::AsyncReadExt as _;

const PAYLOAD_BYTES: usize = 2 * 1024 * 1024 + 17;
const MEMORY_BYTES: u64 = 128 * 1024 * 1024;

fn hexadecimal(bytes: &[u8]) -> String {
    bytes.iter().fold(
        String::with_capacity(bytes.len() * 2),
        |mut result, byte| {
            write!(&mut result, "{byte:02x}").unwrap();
            result
        },
    )
}

fn identity() -> CodeJobIdentity {
    let mut key = [0_u8; 32];
    SystemRandom::new().fill(&mut key).unwrap();
    CodeJobIdentity::new(hexadecimal(&key), "a".repeat(64)).unwrap()
}

fn manifest(payload: Option<&[u8]>) -> Manifest {
    let request = serde_json::json!({
        "argv": ["/bin/sh", "-c", "while [ ! -f /workspace/.elitea-python-preparation-release ]; do sleep 0.1; done"],
        "timeout_seconds": 120,
    });
    let mut entries = vec![
        ManifestEntry::File {
            path: ".elitea-job.json".into(),
            content: serde_json::to_vec(&request).unwrap(),
        },
        ManifestEntry::File {
            path: ".elitea-code.json".into(),
            content: br#"{"revision":1,"language":"python","source":"raise RuntimeError('source must never execute')"}"#.to_vec(),
        },
    ];
    if let Some(payload) = payload {
        entries.push(ManifestEntry::File {
            path: "python-dependencies/binary.whl".into(),
            content: payload.to_vec(),
        });
    }
    Manifest::new(entries)
}

async fn provision(
    client: &DockerClient,
    scope: &CodeJobIdentity,
    manifest: &Manifest,
    owned: &mut Vec<Option<CodeJobIdentity>>,
) -> (CodeJobIdentity, usize) {
    let slot = owned.len();
    owned.push(Some(scope.clone()));
    client.prepare(scope, manifest).await.unwrap();
    let id = client.instance(scope).await.unwrap().unwrap();
    let bound = scope.clone().with_runtime_id(id).unwrap();
    owned[slot] = Some(bound.clone());
    assert!(client.prepared(&bound).await.unwrap());
    (bound, slot)
}

async fn checksum(path: &Path) -> String {
    let mut file = tokio::fs::File::open(path).await.unwrap();
    let mut hash = digest::Context::new(&digest::SHA256);
    let mut buffer = vec![0; 64 * 1024];
    loop {
        let count = file.read(&mut buffer).await.unwrap();
        if count == 0 {
            break;
        }
        hash.update(&buffer[..count]);
    }
    hexadecimal(hash.finish().as_ref())
}

async fn execution_checksum(client: &DockerClient, scope: &CodeJobIdentity, path: &Path) -> String {
    let mut exported = tokio::fs::File::create(path).await.unwrap();
    client
        .export_execution_dependency(scope, "binary.whl", PAYLOAD_BYTES as u64, &mut exported)
        .await
        .unwrap();
    exported.sync_all().await.unwrap();
    drop(exported);
    checksum(path).await
}

async fn fixed_command(session: &dyn SandboxSession, command: &str) -> String {
    let output = session.exec_command(command, None).await.unwrap();
    assert_eq!(output.exit_code, 0, "fixed probe command failed");
    assert!(output.stderr.is_empty());
    assert!(!output.timed_out);
    output.stdout
}

async fn assert_policy(client: &DockerClient, scope: &CodeJobIdentity) {
    let session = client
        .start(&adk_sandbox::workspace::SessionHandle::new(format!(
            "elitea-code-{}",
            scope.job_key()
        )))
        .await
        .unwrap();
    assert_eq!(
        fixed_command(session.as_ref(), "id -u").await.trim(),
        "10001"
    );
    assert_eq!(
        fixed_command(session.as_ref(), "id -g").await.trim(),
        "10001"
    );
    let limits = fixed_command(session.as_ref(), "cat /sys/fs/cgroup/memory.max /sys/fs/cgroup/memory.swap.max /sys/fs/cgroup/pids.max /sys/fs/cgroup/cpu.max").await;
    let limits: Vec<_> = limits.lines().collect();
    assert_eq!(limits, ["134217728", "0", "128", "50000 100000"]);
    let status = fixed_command(session.as_ref(), "cat /proc/self/status").await;
    assert!(status.lines().any(|line| line == "NoNewPrivs:\t1"));
    assert!(
        status
            .lines()
            .any(|line| line == "CapEff:\t0000000000000000")
    );
    let mounts = fixed_command(session.as_ref(), "cat /proc/mounts").await;
    let root = mounts
        .lines()
        .find_map(|line| {
            let columns: Vec<_> = line.split_whitespace().collect();
            (columns.get(1) == Some(&"/")).then(|| columns[3])
        })
        .unwrap();
    assert!(root.split(',').any(|flag| flag == "ro"));
    let workspace = mounts
        .lines()
        .find_map(|line| {
            let columns: Vec<_> = line.split_whitespace().collect();
            (columns.get(1) == Some(&"/workspace")).then(|| columns[3])
        })
        .unwrap();
    for required in ["rw", "noexec", "nosuid", "nodev"] {
        assert!(workspace.split(',').any(|flag| flag == required));
    }
    // Docker Desktop also exposes inactive tunnel templates in an isolated namespace.
    let network = std::process::Command::new("docker")
        .args([
            "inspect",
            "--format",
            "{{.HostConfig.NetworkMode}}",
            scope.runtime_id().unwrap(),
        ])
        .output()
        .unwrap();
    assert!(network.status.success());
    assert!(network.stderr.is_empty());
    assert_eq!(std::str::from_utf8(&network.stdout).unwrap().trim(), "none");
    fixed_command(session.as_ref(), "test ! -e /var/run/docker.sock").await;
}

#[tokio::test]
#[ignore = "requires local Docker and ELITEA_CODE_RUNNER_TEST_IMAGE with current native content helpers"]
#[allow(clippy::too_many_lines)] // One cleanup scope covers all owned runtime instances.
async fn live_docker_dependency_streams_binary_content_and_fences_replacement() {
    let image = std::env::var("ELITEA_CODE_RUNNER_TEST_IMAGE")
        .expect("provide a cached immutable runner image ID");
    assert!(image.starts_with("sha256:") || image.contains("@sha256:"));
    let client = DockerClient::with_image(image)
        .await
        .unwrap()
        .with_resource_limits(Some(MEMORY_BYTES), Some(0.5))
        .with_code_job_policy(Duration::from_mins(2))
        .unwrap();
    let payload: Vec<u8> = (0..PAYLOAD_BYTES)
        .map(|index| index.to_le_bytes()[0])
        .collect();
    assert!(std::str::from_utf8(&payload).is_err());
    let expected_digest = hexadecimal(digest::digest(&digest::SHA256, &payload).as_ref());
    let private = tempfile::tempdir().unwrap();
    let staged_path = private.path().join("binary.whl");
    let mut owned = Vec::new();
    let verified = AssertUnwindSafe(async {
        let source_scope = identity();
        let source_manifest = manifest(Some(&payload));
        let (source, source_slot) =
            provision(&client, &source_scope, &source_manifest, &mut owned).await;
        let (destination, destination_slot) =
            provision(&client, &identity(), &manifest(None), &mut owned).await;
        assert_policy(&client, &source).await;
        assert_policy(&client, &destination).await;
        client.dispatch(&source).await.unwrap();
        assert!(
            client
                .observe_code_job(&source)
                .await
                .unwrap()
                .unwrap()
                .running
        );
        let mut staged = tokio::fs::File::create(&staged_path).await.unwrap();
        client
            .export_dependency(&source, "binary.whl", PAYLOAD_BYTES as u64, &mut staged)
            .await
            .unwrap();
        staged.sync_all().await.unwrap();
        drop(staged);
        assert_eq!(
            tokio::fs::metadata(&staged_path).await.unwrap().len(),
            PAYLOAD_BYTES as u64
        );
        assert_eq!(checksum(&staged_path).await, expected_digest);
        let mut staged = tokio::fs::File::open(&staged_path).await.unwrap();
        client
            .import_dependency(
                &destination,
                "binary.whl",
                PAYLOAD_BYTES as u64,
                &mut staged,
            )
            .await
            .unwrap();
        let destination_session = client
            .start(&adk_sandbox::workspace::SessionHandle::new(format!(
                "elitea-code-{}",
                destination.job_key()
            )))
            .await
            .unwrap();
        let execution_export = private.path().join("execution-export.whl");
        assert_eq!(
            execution_checksum(&client, &destination, &execution_export).await,
            expected_digest
        );
        assert!(
            client
                .export_dependency(
                    &destination,
                    "binary.whl",
                    PAYLOAD_BYTES as u64,
                    &mut tokio::io::sink()
                )
                .await
                .is_err()
        );
        assert!(
            client
                .export_execution_dependency(
                    &source,
                    "binary.whl",
                    PAYLOAD_BYTES as u64,
                    &mut tokio::io::sink()
                )
                .await
                .is_err()
        );
        let inode = fixed_command(
            destination_session.as_ref(),
            "stat -c '%i' /workspace/wheels/binary.whl",
        )
        .await;
        let mut staged = tokio::fs::File::open(&staged_path).await.unwrap();
        client
            .import_dependency(
                &destination,
                "binary.whl",
                PAYLOAD_BYTES as u64,
                &mut staged,
            )
            .await
            .unwrap();
        assert_eq!(
            fixed_command(
                destination_session.as_ref(),
                "stat -c '%i' /workspace/wheels/binary.whl"
            )
            .await,
            inode
        );
        let mut changed = payload.clone();
        changed[PAYLOAD_BYTES - 1] ^= 1;
        let mut changed = changed.as_slice();
        assert!(
            client
                .import_dependency(
                    &destination,
                    "binary.whl",
                    PAYLOAD_BYTES as u64,
                    &mut changed
                )
                .await
                .is_err()
        );
        assert_eq!(
            execution_checksum(&client, &destination, &execution_export).await,
            expected_digest
        );
        assert!(client.cleanup(&source).await.is_err());
        client.release_preparation(&source).await.unwrap();
        let receipt = tokio::time::timeout(Duration::from_secs(15), async {
            loop {
                if let Some(receipt) = client.receipt(&source).await.unwrap() {
                    break receipt;
                }
                tokio::time::sleep(Duration::from_millis(25)).await;
            }
        })
        .await
        .unwrap();
        let receipt: serde_json::Value = serde_json::from_slice(&receipt).unwrap();
        assert_eq!(receipt["status"], "completed");
        assert_eq!(receipt["exit_code"], 0);
        assert_eq!(receipt["stdout"], "");
        assert_eq!(receipt["stderr"], "");
        assert!(
            !client
                .observe_code_job(&source)
                .await
                .unwrap()
                .unwrap()
                .running
        );
        client.cleanup(&source).await.unwrap();
        owned[source_slot] = None;
        let (replacement, _) =
            provision(&client, &source_scope, &source_manifest, &mut owned).await;
        assert_ne!(replacement.runtime_id(), source.runtime_id());
        let mut refused = tokio::fs::File::create(private.path().join("refused.whl"))
            .await
            .unwrap();
        assert!(
            client
                .export_dependency(&source, "binary.whl", PAYLOAD_BYTES as u64, &mut refused)
                .await
                .is_err()
        );
        assert_eq!(refused.metadata().await.unwrap().len(), 0);
        assert!(
            client
                .export_execution_dependency(
                    &source,
                    "binary.whl",
                    PAYLOAD_BYTES as u64,
                    &mut refused
                )
                .await
                .is_err()
        );
        assert_eq!(refused.metadata().await.unwrap().len(), 0);
        let mut incoming = payload.as_slice();
        assert!(
            client
                .import_dependency(
                    &source,
                    "replacement-blocked.whl",
                    PAYLOAD_BYTES as u64,
                    &mut incoming
                )
                .await
                .is_err()
        );
        assert_eq!(incoming.len(), payload.len());
        assert!(client.release_preparation(&source).await.is_err());
        assert!(
            client
                .observe_code_job(&replacement)
                .await
                .unwrap()
                .unwrap()
                .running
        );
        let replacement_session = client
            .start(&adk_sandbox::workspace::SessionHandle::new(format!(
                "elitea-code-{}",
                replacement.job_key()
            )))
            .await
            .unwrap();
        fixed_command(
            replacement_session.as_ref(),
            "test ! -e /workspace/wheels/replacement-blocked.whl",
        )
        .await;
        fixed_command(
            replacement_session.as_ref(),
            "test ! -e /workspace/.elitea-python-preparation-release",
        )
        .await;
        client.terminate(&destination).await.unwrap();
        assert!(
            !client
                .observe_code_job(&destination)
                .await
                .unwrap()
                .unwrap()
                .running
        );
        client.cleanup(&destination).await.unwrap();
        owned[destination_slot] = None;
        assert!(!client.exists(&destination).await.unwrap());
    })
    .catch_unwind()
    .await;
    let mut cleanup_failed = false;
    for job in owned.into_iter().rev().flatten() {
        cleanup_failed |= client.terminate(&job).await.is_err();
        cleanup_failed |= client.cleanup(&job).await.is_err();
    }
    assert!(!cleanup_failed, "test-owned runtime cleanup failed");
    if let Err(panic) = verified {
        std::panic::resume_unwind(panic);
    }
}
