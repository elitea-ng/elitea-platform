//! Explicit infrastructure test. This does not prove ledger or browser recovery.
use super::{PodPolicy, runtime::KubernetesRuntime};
use crate::sandbox::{
    request::{Language, PreparedJob},
    runtime::CodeJobRuntime,
};
use ring::rand::{SecureRandom, SystemRandom};
use serde_json::Value;
use std::{collections::BTreeMap, time::Duration};

#[tokio::test]
#[ignore = "requires an isolated Kubernetes context, cached immutable images, and sandbox boundary resources"]
async fn live_four_language_state_chain() {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let context =
        std::env::var("ELITEA_TEST_KUBE_CONTEXT").expect("explicit isolated context required");
    let namespace =
        std::env::var("ELITEA_TEST_KUBE_NAMESPACE").expect("explicit execution namespace required");
    let deno_image =
        std::env::var("ELITEA_TEST_KUBE_DENO_IMAGE").expect("pinned Deno image required");
    let rust_image =
        std::env::var("ELITEA_TEST_KUBE_RUST_IMAGE").expect("pinned Rust image required");
    let config = kube::Config::from_kubeconfig(&kube::config::KubeConfigOptions {
        context: Some(context),
        ..Default::default()
    })
    .await
    .unwrap();
    let client = kube::Client::try_from(config).unwrap();
    let fixture: serde_yaml_ng::Value = serde_yaml_ng::from_str(include_str!(
        "../../../../../scripts/runtime/fixtures/code-multilanguage-state.yaml"
    ))
    .unwrap();
    let mut state = BTreeMap::from([(
        "input".to_owned(),
        Value::String(
            include_str!(
                "../../../../../scripts/runtime/fixtures/code-multilanguage-state-input.json"
            )
            .to_owned(),
        ),
    )]);
    for node in fixture["nodes"].as_sequence().unwrap() {
        let language = match node["language"].as_str().unwrap() {
            "python" => Language::Python,
            "javascript" => Language::JavaScript,
            "typescript" => Language::TypeScript,
            "rust" => Language::Rust,
            other => panic!("unknown fixture language: {other}"),
        };
        let image = if language == Language::Rust {
            &rust_image
        } else {
            &deno_image
        };
        let runtime = KubernetesRuntime::new(
            client.clone(),
            PodPolicy {
                namespace: namespace.clone(),
                image: image.clone(),
                runtime_class: "elitea-code".into(),
                node_selector: BTreeMap::from([("elitea.ai/sandbox".into(), "true".into())]),
                memory_bytes: 1024 * 1024 * 1024,
                cpu_millis: 1000,
                timeout_seconds: 120,
            },
            "live-test".into(),
            language == Language::Rust,
        )
        .unwrap();
        let inputs = node["input"]
            .as_sequence()
            .unwrap()
            .iter()
            .map(|key| {
                let key = key.as_str().unwrap();
                (key.to_owned(), state[key].clone())
            })
            .collect();
        let job = PreparedJob::new(
            language,
            node["code"]["value"].as_str().unwrap().into(),
            inputs,
            runtime.image_digest().into(),
            "live-test-v1".into(),
            120,
        )
        .unwrap();
        let output = execute(&runtime, &job).await;
        state.insert(node["output"][0].as_str().unwrap().into(), output);
        eprintln!(
            "{}: completed, immutable receipt, cleaned",
            node["id"].as_str().unwrap()
        );
    }
    let expected: Value = serde_json::from_str(include_str!(
        "../../../../../scripts/runtime/fixtures/code-multilanguage-state-expected.json"
    ))
    .unwrap();
    assert_eq!(state["messages"], expected);
}

async fn execute(runtime: &dyn CodeJobRuntime, job: &PreparedJob) -> Value {
    let mut activation = [0; 32];
    SystemRandom::new().fill(&mut activation).unwrap();
    let identity = job
        .scope("live-test".into(), 1, activation)
        .unwrap()
        .runtime_identity()
        .unwrap();
    runtime
        .prepare(&identity, &job.manifest().unwrap())
        .await
        .unwrap();
    let original = runtime.instance(&identity).await.unwrap().unwrap();
    let bound = identity.with_runtime_id(original.clone()).unwrap();
    assert!(runtime.prepared(&bound).await.unwrap());
    runtime.dispatch(&bound).await.unwrap();
    let receipt = tokio::time::timeout(Duration::from_secs(150), async {
        loop {
            if let Some(receipt) = runtime.receipt(&bound).await.unwrap() {
                break receipt;
            }
            tokio::time::sleep(Duration::from_millis(250)).await;
        }
    })
    .await
    .expect("terminal receipt deadline");
    assert_eq!(
        runtime.instance(&bound).await.unwrap().as_deref(),
        Some(original.as_str())
    );
    let envelope: Value = serde_json::from_slice(&receipt).unwrap();
    // Preserve failed workloads for diagnostics; clean successful fixtures only.
    assert_eq!(envelope["status"], "completed", "{envelope}");
    let output: Value = serde_json::from_str(envelope["stdout"].as_str().unwrap()).unwrap();
    assert_eq!(output["revision"], 1);
    // Docker rejects dispatch after exit; Kubernetes accepts the existing marker.
    // Both must retain the same terminal receipt without executing again.
    if let Err(error) = runtime.dispatch(&bound).await {
        eprintln!("terminal dispatch rejected: {error}");
    }
    assert_eq!(runtime.receipt(&bound).await.unwrap().unwrap(), receipt);
    runtime.cleanup(&bound).await.unwrap();
    tokio::time::timeout(Duration::from_secs(20), async {
        while runtime.exists(&bound).await.unwrap() {
            tokio::time::sleep(Duration::from_millis(250)).await;
        }
    })
    .await
    .expect("cleanup deadline");
    output["result"].clone()
}

#[tokio::test]
#[ignore = "requires an isolated Kubernetes context, cached Deno image, and sandbox boundary resources"]
async fn live_memory_exhaustion_has_terminal_receipt() {
    // Touch bounded pages so this exercises the cgroup, not lazy virtual allocation.
    live_failure_receipt(
        "export default () => { const pages = []; for (let i = 0; i < 64; i++) { const page = new Uint8Array(8 * 1024 * 1024); page.fill(1); pages.push(page); } return pages.length; };",
        30,
        "memory_limit",
    ).await;
}

#[tokio::test]
#[ignore = "requires an isolated Kubernetes context, cached Deno image, and sandbox boundary resources"]
async fn live_deadline_exhaustion_has_terminal_receipt() {
    live_failure_receipt(
        "export default async () => { await new Promise(resolve => setTimeout(resolve, 10000)); return {unexpected: true}; };",
        2,
        "timeout",
    )
    .await;
}

#[tokio::test]
#[ignore = "requires an isolated Kubernetes context, cached Deno image, and sandbox boundary resources"]
async fn live_output_flood_has_bounded_terminal_receipt() {
    live_failure_receipt(
        "export default () => { const chunk = 'x'.repeat(8192); for (let i = 0; i < 256; i++) console.error(chunk); return {unexpected: true}; };",
        30,
        "output_limit",
    )
    .await;
}

const WORKSPACE_EXHAUSTION: &str = "export default async () => { const file = await Deno.open('/workspace/fill', {write:true, createNew:true}); const chunk = new Uint8Array(1024 * 1024).fill(1); try { for (let i = 0; i < 300; i++) { let offset = 0; while (offset < chunk.length) offset += await file.write(chunk.subarray(offset)); } } finally { file.close(); } return {unexpected:true}; };";

#[tokio::test]
#[ignore = "requires an isolated Kubernetes context and cached Deno image"]
async fn live_workspace_exhaustion_has_terminal_receipt() {
    // Exceed the 256 MiB workspace while remaining below the memory ceiling.
    // Leave the file full: receipt delivery must not require writable workspace space.
    let envelope =
        live_failure_receipt_with_memory(WORKSPACE_EXHAUSTION, 30, "failed", 512 * 1024 * 1024)
            .await;
    assert!(
        envelope["stderr"]
            .as_str()
            .unwrap()
            .contains("No space left on device")
    );
}

#[tokio::test]
#[ignore = "requires local Docker and a cached immutable Deno image"]
async fn live_docker_workspace_exhaustion_has_terminal_receipt() {
    let image =
        std::env::var("ELITEA_TEST_DOCKER_DENO_IMAGE").expect("explicit cached image required");
    assert!(image.starts_with("sha256:") || image.contains("@sha256:"));
    let runtime = adk_sandbox::workspace::DockerClient::with_image(image)
        .await
        .unwrap()
        .with_resource_limits(Some(512 * 1024 * 1024), Some(0.5))
        .with_code_job_policy(Duration::from_secs(30))
        .unwrap();
    let envelope = verify_failure_receipt(&runtime, WORKSPACE_EXHAUSTION, 30, "failed").await;
    assert!(
        envelope["stderr"]
            .as_str()
            .unwrap()
            .contains("No space left on device")
    );
}

async fn live_failure_receipt(source: &str, timeout_seconds: u32, expected: &str) {
    live_failure_receipt_with_memory(source, timeout_seconds, expected, 256 * 1024 * 1024).await;
}

async fn live_failure_receipt_with_memory(
    source: &str,
    timeout_seconds: u32,
    expected: &str,
    memory_bytes: u64,
) -> Value {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let context = std::env::var("ELITEA_TEST_KUBE_CONTEXT").expect("explicit context required");
    let namespace =
        std::env::var("ELITEA_TEST_KUBE_NAMESPACE").expect("explicit namespace required");
    let image = std::env::var("ELITEA_TEST_KUBE_DENO_IMAGE").expect("pinned Deno image required");
    let config = kube::Config::from_kubeconfig(&kube::config::KubeConfigOptions {
        context: Some(context),
        ..Default::default()
    })
    .await
    .unwrap();
    let runtime = KubernetesRuntime::new(
        kube::Client::try_from(config).unwrap(),
        PodPolicy {
            namespace,
            image,
            runtime_class: "elitea-code".into(),
            node_selector: BTreeMap::from([("elitea.ai/sandbox".into(), "true".into())]),
            memory_bytes,
            cpu_millis: 500,
            timeout_seconds: 30,
        },
        "live-test".into(),
        false,
    )
    .unwrap();
    verify_failure_receipt(&runtime, source, timeout_seconds, expected).await
}

async fn verify_failure_receipt(
    runtime: &dyn CodeJobRuntime,
    source: &str,
    timeout_seconds: u32,
    expected: &str,
) -> Value {
    let job = PreparedJob::new(
        Language::JavaScript,
        source.into(),
        BTreeMap::new(),
        runtime.image_digest().into(),
        "live-resource-v1".into(),
        timeout_seconds,
    )
    .unwrap();
    let mut activation = [0; 32];
    SystemRandom::new().fill(&mut activation).unwrap();
    let identity = job
        .scope("live-test".into(), 1, activation)
        .unwrap()
        .runtime_identity()
        .unwrap();
    runtime
        .prepare(&identity, &job.manifest().unwrap())
        .await
        .unwrap();
    let original = runtime.instance(&identity).await.unwrap().unwrap();
    let bound = identity.with_runtime_id(original).unwrap();
    runtime.dispatch(&bound).await.unwrap();
    let receipt = tokio::time::timeout(Duration::from_secs(45), async {
        loop {
            if let Some(receipt) = runtime.receipt(&bound).await.unwrap() {
                break receipt;
            }
            tokio::time::sleep(Duration::from_millis(250)).await;
        }
    })
    .await
    .expect("resource failure must terminate");
    let envelope: Value = serde_json::from_slice(&receipt).unwrap();
    assert_eq!(envelope["status"], expected, "{envelope}");
    assert_eq!(runtime.receipt(&bound).await.unwrap().unwrap(), receipt);
    let stdout_bytes = envelope["stdout"].as_str().unwrap().len();
    let stderr_bytes = envelope["stderr"].as_str().unwrap().len();
    assert!(stdout_bytes + stderr_bytes <= 512 * 1024);
    eprintln!(
        "resource exhaustion receipt: {expected}, stdout={stdout_bytes}, stderr={stderr_bytes}"
    );
    runtime.cleanup(&bound).await.unwrap();
    tokio::time::timeout(Duration::from_secs(20), async {
        while runtime.exists(&bound).await.unwrap() {
            tokio::time::sleep(Duration::from_millis(250)).await;
        }
    })
    .await
    .expect("resource fixture cleanup deadline");
    envelope
}

#[tokio::test]
#[ignore = "requires an isolated Kubernetes context, cached Deno image, and cgroup v2"]
async fn live_cpu_quota_throttles_execution() {
    use k8s_openapi::api::core::v1::Pod;
    use kube::Api;

    let _ = rustls::crypto::ring::default_provider().install_default();
    let context = std::env::var("ELITEA_TEST_KUBE_CONTEXT").expect("explicit context required");
    let namespace =
        std::env::var("ELITEA_TEST_KUBE_NAMESPACE").expect("explicit namespace required");
    let image = std::env::var("ELITEA_TEST_KUBE_DENO_IMAGE").expect("pinned image required");
    let config = kube::Config::from_kubeconfig(&kube::config::KubeConfigOptions {
        context: Some(context),
        ..Default::default()
    })
    .await
    .unwrap();
    let client = kube::Client::try_from(config).unwrap();
    let pods: Api<Pod> = Api::namespaced(client.clone(), &namespace);
    let runtime = KubernetesRuntime::new(
        client,
        PodPolicy {
            namespace,
            image,
            runtime_class: "elitea-code".into(),
            node_selector: BTreeMap::from([("elitea.ai/sandbox".into(), "true".into())]),
            memory_bytes: 256 * 1024 * 1024,
            cpu_millis: 250,
            timeout_seconds: 30,
        },
        "live-test".into(),
        false,
    )
    .unwrap();
    let job = PreparedJob::new(Language::JavaScript,
        "export default () => { const end = Date.now() + 6000; let n = 0; while (Date.now() < end) { n++; } return {iterations: n}; };".into(),
        BTreeMap::new(), runtime.image_digest().into(), "live-cpu-v1".into(), 20).unwrap();
    let mut activation = [0; 32];
    SystemRandom::new().fill(&mut activation).unwrap();
    let identity = job
        .scope("live-test".into(), 1, activation)
        .unwrap()
        .runtime_identity()
        .unwrap();
    runtime
        .prepare(&identity, &job.manifest().unwrap())
        .await
        .unwrap();
    let original = runtime.instance(&identity).await.unwrap().unwrap();
    let bound = identity.with_runtime_id(original).unwrap();
    let name = format!("elitea-code-{}", &bound.job_key()[..48]);
    let before = parse_cpu_quota(
        &tokio::time::timeout(Duration::from_secs(10), read_cpu_cgroup(&pods, &name))
            .await
            .unwrap(),
    );
    runtime.dispatch(&bound).await.unwrap();
    let after = tokio::time::timeout(Duration::from_secs(10), async {
        loop {
            let current = parse_cpu_quota(&read_cpu_cgroup(&pods, &name).await);
            if current > before {
                break current;
            }
            tokio::time::sleep(Duration::from_millis(250)).await;
        }
    })
    .await
    .expect("CPU quota must throttle the busy workload");
    let receipt = tokio::time::timeout(Duration::from_secs(30), async {
        loop {
            if let Some(receipt) = runtime.receipt(&bound).await.unwrap() {
                break receipt;
            }
            tokio::time::sleep(Duration::from_millis(250)).await;
        }
    })
    .await
    .unwrap();
    let envelope: Value = serde_json::from_slice(&receipt).unwrap();
    assert_eq!(envelope["status"], "completed", "{envelope}");
    let output: Value = serde_json::from_str(envelope["stdout"].as_str().unwrap()).unwrap();
    assert!(output["result"]["iterations"].as_u64().unwrap() > 0);
    runtime.cleanup(&bound).await.unwrap();
    tokio::time::timeout(Duration::from_secs(20), async {
        while runtime.exists(&bound).await.unwrap() {
            tokio::time::sleep(Duration::from_millis(250)).await;
        }
    })
    .await
    .unwrap();
    eprintln!(
        "CPU quota 250m; throttled periods increased from {before} to {after}; completed and cleaned"
    );
}

// Operator-side observation does not give untrusted source access to cgroups.
async fn read_cpu_cgroup(pods: &kube::Api<k8s_openapi::api::core::v1::Pod>, name: &str) -> String {
    use kube::api::AttachParams;
    use tokio::io::AsyncReadExt;
    let mut process = pods
        .exec(
            name,
            [
                "/bin/sh",
                "-c",
                "cat /sys/fs/cgroup/cpu.max; cat /sys/fs/cgroup/cpu.stat",
            ],
            &AttachParams::default()
                .container("code")
                .stdout(true)
                .stderr(false),
        )
        .await
        .unwrap();
    let status = process.take_status().unwrap();
    let mut bytes = Vec::new();
    process
        .stdout()
        .unwrap()
        .take(4097)
        .read_to_end(&mut bytes)
        .await
        .unwrap();
    assert!(bytes.len() <= 4096);
    assert_eq!(status.await.unwrap().status.as_deref(), Some("Success"));
    String::from_utf8(bytes).unwrap()
}

fn parse_cpu_quota(text: &str) -> u64 {
    let mut lines = text.lines();
    let quota: Vec<_> = lines.next().unwrap().split_whitespace().collect();
    assert_eq!(quota.len(), 2);
    assert_eq!(
        quota[0].parse::<u64>().unwrap() * 4,
        quota[1].parse::<u64>().unwrap()
    );
    lines
        .find_map(|line| line.strip_prefix("nr_throttled "))
        .unwrap()
        .parse()
        .unwrap()
}

// Bounded attempts prevent an absent PID policy from becoming a fork bomb.
const PROCESS_EXHAUSTION: &str = r#"
pub fn run(_: serde_json::Value) -> Result<serde_json::Value, Box<dyn std::error::Error>> {
    let mut children = Vec::new();
    let mut errno = None;
    for _ in 0..160 {
        match std::process::Command::new("/bin/sleep").arg("30").spawn() {
            Ok(child) => children.push(child),
            Err(error) => { errno = error.raw_os_error(); break; }
        }
    }
    let count = children.len();
    for child in &mut children { let _ = child.kill(); }
    for child in &mut children { child.wait()?; }
    Ok(serde_json::json!({"children":count,"errno":errno}))
}
"#;

async fn verify_process_limit(runtime: &dyn CodeJobRuntime) {
    let job = PreparedJob::new(
        Language::Rust,
        PROCESS_EXHAUSTION.into(),
        BTreeMap::new(),
        runtime.image_digest().into(),
        "live-pids-v1".into(),
        120,
    )
    .unwrap();
    let output = execute(runtime, &job).await;
    assert_eq!(
        output["errno"], 11,
        "Linux must deny process creation with EAGAIN"
    );
    let count = output["children"].as_u64().unwrap();
    assert!(count > 0 && count < 128, "unexpected child count: {count}");
    eprintln!("PID ceiling enforced after {count} child processes; completed and cleaned");
}

#[tokio::test]
#[ignore = "requires isolated Kubernetes with podPidsLimit=128 and cached Rust image"]
async fn live_kubernetes_process_limit() {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let context = std::env::var("ELITEA_TEST_KUBE_CONTEXT").expect("explicit context required");
    let namespace =
        std::env::var("ELITEA_TEST_KUBE_NAMESPACE").expect("explicit namespace required");
    let image = std::env::var("ELITEA_TEST_KUBE_RUST_IMAGE").expect("pinned Rust image required");
    let config = kube::Config::from_kubeconfig(&kube::config::KubeConfigOptions {
        context: Some(context),
        ..Default::default()
    })
    .await
    .unwrap();
    let runtime = KubernetesRuntime::new(
        kube::Client::try_from(config).unwrap(),
        PodPolicy {
            namespace,
            image,
            runtime_class: "elitea-code".into(),
            node_selector: BTreeMap::from([("elitea.ai/sandbox".into(), "true".into())]),
            memory_bytes: 1024 * 1024 * 1024,
            cpu_millis: 1000,
            timeout_seconds: 120,
        },
        "live-test".into(),
        true,
    )
    .unwrap();
    verify_process_limit(&runtime).await;
}

#[tokio::test]
#[ignore = "requires local Docker and cached immutable Rust image"]
async fn live_docker_process_limit() {
    let image =
        std::env::var("ELITEA_TEST_DOCKER_RUST_IMAGE").expect("explicit cached image required");
    assert!(image.starts_with("sha256:") || image.contains("@sha256:"));
    let runtime = adk_sandbox::workspace::DockerClient::with_image(image)
        .await
        .unwrap()
        .with_resource_limits(Some(1024 * 1024 * 1024), Some(1.0))
        .with_code_job_policy(Duration::from_mins(2))
        .unwrap()
        .with_code_compilation()
        .unwrap();
    verify_process_limit(&runtime).await;
}
