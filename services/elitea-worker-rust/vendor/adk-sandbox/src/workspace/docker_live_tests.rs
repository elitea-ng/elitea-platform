//! Opt-in tests against a real local Docker daemon. Only test-owned containers
//! are removed. The image must already be cached; these tests never pull it.
use super::*;
use futures::FutureExt;
use std::panic::AssertUnwindSafe;

async fn fixture(timeout: Duration) -> (DockerClient, SessionHandle, String) {
    let image = std::env::var("ELITEA_SANDBOX_TEST_IMAGE")
        .expect("set ELITEA_SANDBOX_TEST_IMAGE to a cached immutable image ID with Python");
    assert!(image.starts_with("sha256:") || image.contains("@sha256:"));
    let client = DockerClient::with_image(image)
        .await
        .unwrap()
        .with_resource_limits(Some(64 * 1024 * 1024), Some(0.25))
        .with_code_job_policy(timeout)
        .unwrap();
    let handle = client.provision(&Manifest::new(vec![])).await.unwrap();
    let id = client.sessions.read().await[handle.as_str()].clone();
    (client, handle, id)
}

#[tokio::test]
#[ignore = "requires local Docker and ELITEA_SANDBOX_TEST_IMAGE"]
async fn live_code_job_isolation_and_process_termination() {
    for case in ["policy", "timeout", "overflow", "oom"] {
        let timeout = if case == "timeout" {
            Duration::from_secs(2)
        } else {
            Duration::from_secs(15)
        };
        let (client, handle, id) = fixture(timeout).await;
        let verified = AssertUnwindSafe(async {
            let session = client.start(&handle).await.unwrap();
            if case == "policy" {
                let info = client.client.inspect_container(&id, None).await.unwrap();
                let host = info.host_config.unwrap();
                assert_eq!(host.memory, Some(64 * 1024 * 1024));
                assert_eq!(host.memory_swap, host.memory);
                assert_eq!(host.nano_cpus, Some(250_000_000));
                assert_eq!(host.pids_limit, Some(128));
                assert_eq!(host.readonly_rootfs, Some(true));
                assert_eq!(host.network_mode.as_deref(), Some("none"));
                assert_eq!(info.config.unwrap().user.as_deref(), Some("10001:10001"));
                // Quoted names cannot become shell source.
                session.write_file("quoted'name.txt", b"literal value").await.unwrap();
                assert_eq!(session.read_file("quoted'name.txt").await.unwrap(), b"literal value");
                let result = session.exec_command(
                    r#"python -c 'import os; assert os.getuid() == 10001; assert not os.path.exists("/var/run/docker.sock"); print(open("/sys/fs/cgroup/memory.max").read().strip()); print(open("/sys/fs/cgroup/cpu.max").read().strip())'"#,
                    None).await.unwrap();
                assert_eq!(result.exit_code, 0, "{}", result.stderr);
                assert!(result.stdout.contains("67108864"));
                assert!(result.stdout.contains("25000 100000"));
                assert!(client.snapshot(&handle).await.is_err());
            } else {
                let command = if case == "timeout" {
                    "sleep 60 & wait"
                } else if case == "oom" {
                    "python -c 'x=bytearray(256*1024*1024)'"
                } else {
                    r#"python -c 'import sys; sys.stdout.write("x"*2000000); sys.stdout.flush(); import time; time.sleep(60)'"#
                };
                let result = session.exec_command(command, None).await;
                if case == "timeout" {
                    assert!(result.unwrap().timed_out);
                } else if case == "oom" {
                    assert_eq!(result.unwrap().exit_code, 137);
                } else {
                    assert!(result.unwrap_err().to_string().contains("capture limit"));
                }
                let state = client.client.inspect_container(&id, None).await.unwrap().state.unwrap();
                if case != "oom" {
                    assert_eq!(state.running, Some(false), "remote process must actually stop");
                }
            }
        }).catch_unwind().await;
        let cleanup = client.stop(&handle).await;
        cleanup.unwrap();
        assert!(!client.sessions.read().await.contains_key(handle.as_str()));
        verified.unwrap();
    }
}

#[tokio::test]
#[ignore = "requires local Docker and ELITEA_SANDBOX_TEST_IMAGE"]
async fn live_concurrent_jobs_keep_independent_state() {
    let ((left, lh, _), (right, rh, _)) = tokio::join!(
        fixture(Duration::from_secs(15)),
        fixture(Duration::from_secs(15))
    );
    let verified = AssertUnwindSafe(async {
        let ls = left.start(&lh).await.unwrap();
        let rs = right.start(&rh).await.unwrap();
        let (lw, rw) = tokio::join!(
            ls.write_file("state.json", br#"{"job":"left"}"#),
            rs.write_file("state.json", br#"{"job":"right"}"#)
        );
        lw.unwrap();
        rw.unwrap();
        let (lr, rr) = tokio::join!(
            ls.exec_command("cat state.json", None),
            rs.exec_command("cat state.json", None)
        );
        assert_eq!(lr.unwrap().stdout, r#"{"job":"left"}"#);
        assert_eq!(rr.unwrap().stdout, r#"{"job":"right"}"#);
    })
    .catch_unwind()
    .await;
    let (lc, rc) = tokio::join!(left.stop(&lh), right.stop(&rh));
    lc.unwrap();
    rc.unwrap();
    verified.unwrap();
}

#[tokio::test]
#[ignore = "requires local Docker and ELITEA_SANDBOX_TEST_IMAGE"]
async fn live_named_job_survives_client_recreation_without_reexecution() {
    let image = std::env::var("ELITEA_SANDBOX_TEST_IMAGE").unwrap();
    let client = DockerClient::with_image(image.clone())
        .await
        .unwrap()
        .with_resource_limits(Some(64 * 1024 * 1024), Some(0.25))
        .with_code_job_policy(Duration::from_secs(15))
        .unwrap();
    let key = format!(
        "{}{}",
        uuid::Uuid::new_v4().simple(),
        uuid::Uuid::new_v4().simple()
    );
    let identity = CodeJobIdentity::new(key.clone(), "a".repeat(64)).unwrap();
    let manifest = Manifest::new(vec![]);
    let (first, second) = tokio::join!(
        client.provision_code_job(&identity, &manifest),
        client.provision_code_job(&identity, &manifest)
    );
    assert_ne!(
        first.is_ok(),
        second.is_ok(),
        "exactly one concurrent create must win"
    );
    let handle = first.or(second).unwrap();
    let checked = AssertUnwindSafe(async {
        let session = client.start(&handle).await.unwrap();
        assert_eq!(
            session
                .exec_command("echo one > effect.txt", None)
                .await
                .unwrap()
                .exit_code,
            0
        );
        let before = client.observe_code_job(&identity).await.unwrap().unwrap();
        // Recreate the Docker client with no retained in-memory sessions.
        let recovered = DockerClient::with_image(image)
            .await
            .unwrap()
            .with_resource_limits(Some(64 * 1024 * 1024), Some(0.25))
            .with_code_job_policy(Duration::from_secs(15))
            .unwrap();
        assert!(recovered.sessions.read().await.is_empty());
        let after = recovered
            .observe_code_job(&identity)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(before.container_id, after.container_id);
        assert!(after.running);
        assert!(
            recovered
                .provision_code_job(&identity, &manifest)
                .await
                .is_err()
        );
        let conflicting = CodeJobIdentity::new(key, "b".repeat(64)).unwrap();
        assert!(recovered.observe_code_job(&conflicting).await.is_err());
        assert_eq!(session.read_file("effect.txt").await.unwrap(), b"one\n");
    })
    .catch_unwind()
    .await;
    client.stop(&handle).await.unwrap();
    assert!(client.observe_code_job(&identity).await.unwrap().is_none());
    checked.unwrap();
}

#[tokio::test]
#[ignore = "requires local Docker and ELITEA_SANDBOX_TEST_IMAGE"]
async fn live_failed_preparation_retains_stopped_identity() {
    let image = std::env::var("ELITEA_SANDBOX_TEST_IMAGE").unwrap();
    let client = DockerClient::with_image(image)
        .await
        .unwrap()
        .with_resource_limits(Some(64 * 1024 * 1024), Some(0.25))
        .with_code_job_policy(Duration::from_secs(15))
        .unwrap();
    let key = format!(
        "{}{}",
        uuid::Uuid::new_v4().simple(),
        uuid::Uuid::new_v4().simple()
    );
    let identity = CodeJobIdentity::new(key, "c".repeat(64)).unwrap();
    let manifest = Manifest::new(vec![ManifestEntry::File {
        path: "../escape".into(),
        content: vec![],
    }]);
    assert!(
        client
            .provision_code_job(&identity, &manifest)
            .await
            .is_err()
    );
    let observed = client.observe_code_job(&identity).await.unwrap().unwrap();
    let checked = AssertUnwindSafe(async {
        assert!(!observed.running);
        assert!(
            client
                .provision_code_job(&identity, &Manifest::new(vec![]))
                .await
                .is_err()
        );
    })
    .catch_unwind()
    .await;
    client
        .client
        .remove_container(
            &observed.container_id,
            Some(RemoveContainerOptions {
                force: true,
                ..Default::default()
            }),
        )
        .await
        .unwrap();
    checked.unwrap();
}

#[tokio::test]
#[ignore = "requires local Docker and ELITEA_CODE_RUNNER_TEST_IMAGE"]
async fn live_pid1_receipt_is_recovered_by_a_new_client() {
    let image = std::env::var("ELITEA_CODE_RUNNER_TEST_IMAGE").unwrap();
    let client = DockerClient::with_image(image.clone())
        .await
        .unwrap()
        .with_resource_limits(Some(64 * 1024 * 1024), Some(0.25))
        .with_code_job_policy(Duration::from_secs(15))
        .unwrap();
    let key = format!(
        "{}{}",
        uuid::Uuid::new_v4().simple(),
        uuid::Uuid::new_v4().simple()
    );
    let identity = CodeJobIdentity::new(key, "d".repeat(64)).unwrap();
    let request = serde_json::json!({
        "argv": ["python", "-c", "import time;time.sleep(0.5);print('recovered-result')"],
        "timeout_seconds": 5
    });
    let manifest = Manifest::new(vec![ManifestEntry::File {
        path: ".elitea-job.json".into(),
        content: serde_json::to_vec(&request).unwrap(),
    }]);
    let handle = client
        .provision_code_job(&identity, &manifest)
        .await
        .unwrap();
    let verified = AssertUnwindSafe(async {
        assert!(
            client
                .read_code_job_receipt(&identity)
                .await
                .unwrap()
                .is_none()
        );
        client.dispatch_code_job(&identity).await.unwrap();
        // Repeated signals do not restart the container's main process.
        client.dispatch_code_job(&identity).await.unwrap();
        let recovered = DockerClient::with_image(image).await.unwrap();
        let receipt = tokio::time::timeout(Duration::from_secs(15), async {
            loop {
                if let Some(receipt) = recovered.read_code_job_receipt(&identity).await.unwrap() {
                    break receipt;
                }
                tokio::time::sleep(Duration::from_millis(25)).await;
            }
        })
        .await
        .unwrap();
        let value: serde_json::Value = serde_json::from_slice(&receipt).unwrap();
        assert_eq!(value["status"], "completed");
        assert_eq!(value["stdout"], "recovered-result\n");
        assert_eq!(
            recovered
                .read_code_job_receipt(&identity)
                .await
                .unwrap()
                .unwrap(),
            receipt
        );
        assert!(recovered.dispatch_code_job(&identity).await.is_err());
    })
    .catch_unwind()
    .await;
    client.stop(&handle).await.unwrap();
    verified.unwrap();
}

#[tokio::test]
#[ignore = "requires Docker and ELITEA_CODE_RUNNER_TEST_IMAGE"]
async fn live_recovered_client_terminates_and_removes_named_job() {
    let image = std::env::var("ELITEA_CODE_RUNNER_TEST_IMAGE").unwrap();
    let client = DockerClient::with_image(image.clone())
        .await
        .unwrap()
        .with_resource_limits(Some(64 * 1024 * 1024), Some(0.25))
        .with_code_job_policy(Duration::from_secs(15))
        .unwrap();
    let key = format!(
        "{}{}",
        uuid::Uuid::new_v4().simple(),
        uuid::Uuid::new_v4().simple()
    );
    let identity = CodeJobIdentity::new(key.clone(), "c".repeat(64)).unwrap();
    let handle = client
        .provision_code_job(&identity, &Manifest::new(vec![]))
        .await
        .unwrap();
    let verified = AssertUnwindSafe(async {
        let recovered = DockerClient::with_image(image).await.unwrap();
        assert!(recovered.remove_code_job(&identity).await.is_err());
        let conflict = CodeJobIdentity::new(key, "b".repeat(64)).unwrap();
        assert!(recovered.terminate_code_job(&conflict).await.is_err());
        assert!(recovered.remove_code_job(&conflict).await.is_err());
        assert!(
            recovered
                .observe_code_job(&identity)
                .await
                .unwrap()
                .unwrap()
                .running
        );
        recovered.terminate_code_job(&identity).await.unwrap();
        assert!(
            !recovered
                .observe_code_job(&identity)
                .await
                .unwrap()
                .unwrap()
                .running
        );
        recovered.terminate_code_job(&identity).await.unwrap();
        recovered.remove_code_job(&identity).await.unwrap();
        recovered.remove_code_job(&identity).await.unwrap();
        assert!(
            recovered
                .observe_code_job(&identity)
                .await
                .unwrap()
                .is_none()
        );
    })
    .catch_unwind()
    .await;
    client.stop(&handle).await.unwrap();
    verified.unwrap();
}
