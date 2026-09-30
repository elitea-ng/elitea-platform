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

async fn execute(runtime: &KubernetesRuntime, job: &PreparedJob) -> Value {
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
    // Repeated terminal dispatch must return the same receipt, never run again.
    runtime.dispatch(&bound).await.unwrap();
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
