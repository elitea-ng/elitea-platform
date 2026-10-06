//! Pure compatibility, exact broker policy, and separate cache attestations.
use super::*;
use crate::config::SandboxRuntimeConfig;
use crate::sandbox::{
    compiled_snapshot::{Binding, SnapshotProfile},
    platform_client_binding::PlatformClientPolicy,
};
use serde_json::json;

fn config(language: &str, broker: bool) -> SandboxRuntimeConfig {
    serde_json::from_value(json!({
        "language":language, "target":"sandbox.internal:9446",
        "audience":if broker {"broker-owner"} else {"pure-owner"},
        "image_digest":format!("sha256:{}",if broker {"b".repeat(64)} else {"a".repeat(64)}),
        "policy_revision":if language == "rust" && broker {"cargo-broker-execute-v1"} else {"isolated-v1"},
        "timeout_seconds":30
    }))
    .unwrap()
}

fn client(config: &SandboxRuntimeConfig) -> SandboxClient {
    SandboxClient::from_channel(
        tonic::transport::Endpoint::from_static("https://127.0.0.1:1").connect_lazy(),
        config.audience.clone(),
        Duration::from_secs(30),
    )
    .unwrap()
}

fn factory(language: &str) -> CodeRuntimeFactory {
    let channel = tonic::transport::Endpoint::from_static("https://127.0.0.1:1").connect_lazy();
    let control = Arc::new(
        AgentControlClient::from_channel(
            channel,
            crate::transport::control_grpc::ControlGrpcConfig {
                deadline: Duration::from_secs(1),
                workload_session_id: "workload-1".into(),
                producer_id: "worker-1".into(),
            },
        )
        .unwrap(),
    );
    let state = sqlx::postgres::PgPoolOptions::new()
        .max_connections(1)
        .connect_lazy("postgres://fixture:fixture@127.0.0.1:1/never_connect")
        .unwrap();
    let configured = config(language, false);
    let transport = client(&configured);
    CodeRuntimeFactory::new(control, vec![(configured, transport, None)], state)
}

fn invocation(language: CodeLanguage) -> CodeInvocation<'static> {
    CodeInvocation {
        activation: [7; 32],
        language,
        platform_client: false,
        source: "fn main() {}",
        dependencies_toml: None,
        provenance: CodeProvenance::SavedLiteral,
        input_json: b"{}".to_vec(),
        trace: None,
        original: None,
        debug: None,
        workspace: None,
    }
}

fn snapshot(config: &SandboxRuntimeConfig) -> Arc<SnapshotProfile> {
    let mut binding: Binding = serde_json::from_slice(include_bytes!(
        "../../../tests/fixtures/compiled-snapshot-v1/binding.json"
    ))
    .unwrap();
    binding
        .compilation_image_digest
        .clone_from(&config.image_digest);
    binding
        .execution_image_digest
        .clone_from(&config.image_digest);
    binding.policy_revision.clone_from(&config.policy_revision);
    Arc::new(SnapshotProfile::new(binding).unwrap())
}

fn runtime(factory: CodeRuntimeFactory) -> RemoteCodeRuntime {
    RemoteCodeRuntime {
        saved_child_scope: None,
        intent_content: None,
        control: factory.control,
        authority: Arc::new(ClaimBoundSandboxAuthority::original_code_visit_conformance_fixture()),
        profiles: factory.profiles,
        journal: factory.journal,
        compiled_profile: factory.compiled_profile,
        debug_sink: None,
    }
}

#[tokio::test]
async fn code_platform_startup_preserves_pure_request_bytes_and_binds_exact_operator_policy() {
    let factory = factory("python");
    let input = invocation(CodeLanguage::Python);
    let before = prepare_job(&input, &factory.profiles[0])
        .unwrap()
        .to_transport()
        .unwrap();
    let broker = config("python", true);
    let transport = client(&broker);
    let factory = factory
        .with_platform_client(
            broker,
            transport,
            None,
            PlatformClientPolicy::new(32, 1_048_576).unwrap(),
            None,
        )
        .unwrap();
    let after = prepare_job(&input, &factory.profiles[0])
        .unwrap()
        .to_transport()
        .unwrap();
    assert_eq!(before, after);
    assert!(
        serde_json::from_slice::<serde_json::Value>(&after)
            .unwrap()
            .get("platform_client")
            .is_none()
    );
    let broker = &factory.profiles[1];
    let job = prepare_job(&input, broker)
        .unwrap()
        .with_platform_client(broker.platform_client.clone().unwrap())
        .unwrap();
    let wire: serde_json::Value = serde_json::from_slice(&job.to_transport().unwrap()).unwrap();
    assert_eq!(
        wire["platform_client"]["policy_sha256"],
        "a299856c1b28b5aee8488da91e8dd2cb33251c89b677733b63447179144d03da"
    );
    assert_eq!(wire["platform_client"]["max_calls"], 32);
    assert_eq!(wire["source"], input.source);
    assert_eq!(wire["image_digest"], broker.image_digest);
}

#[tokio::test]
async fn code_platform_startup_rejects_duplicate_fixed_or_incompatible_broker_profiles() {
    let configured = config("rust", false);
    let transport = client(&configured);
    assert!(
        factory("rust")
            .with_platform_client(
                configured,
                transport,
                None,
                PlatformClientPolicy::new(32, 4096).unwrap(),
                None
            )
            .is_err()
    );
    let configured = config("rust", true);
    let transport = client(&configured);
    let wrong = snapshot(&config("rust", false));
    assert!(
        factory("rust")
            .with_platform_client(
                configured.clone(),
                transport.clone(),
                None,
                PlatformClientPolicy::new(32, 4096).unwrap(),
                Some(wrong)
            )
            .is_err()
    );
    let factory = factory("rust")
        .with_platform_client(
            configured.clone(),
            transport.clone(),
            None,
            PlatformClientPolicy::new(32, 4096).unwrap(),
            None,
        )
        .unwrap();
    assert!(
        factory
            .with_platform_client(
                configured,
                transport,
                None,
                PlatformClientPolicy::new(32, 4096).unwrap(),
                None
            )
            .is_err()
    );
}

#[tokio::test]
async fn code_platform_startup_never_uses_pure_cache_attestation_for_broker_execute() {
    let pure = snapshot(&config("rust", false));
    let configured = config("rust", true);
    let broker = snapshot(&configured);
    let transport = client(&configured);
    let bound = runtime(
        factory("rust")
            .with_compiled_snapshots(pure.clone())
            .with_platform_client(
                configured.clone(),
                transport.clone(),
                None,
                PlatformClientPolicy::new(32, 4096).unwrap(),
                Some(broker.clone()),
            )
            .unwrap(),
    );
    assert!(Arc::ptr_eq(
        bound.selected_compiled_profile(&bound.profiles[0]).unwrap(),
        &pure
    ));
    let job = prepare_job(&invocation(CodeLanguage::Rust), &bound.profiles[1]).unwrap();
    assert!(
        bound
            .selected_compiled_profile(&bound.profiles[1])
            .unwrap()
            .binding(&job, "tenant", 1)
            .is_ok()
    );
    assert!(pure.binding(&job, "tenant", 1).is_err());
    let omitted = runtime(
        factory("rust")
            .with_compiled_snapshots(pure.clone())
            .with_platform_client(
                configured,
                transport,
                None,
                PlatformClientPolicy::new(32, 4096).unwrap(),
                None,
            )
            .unwrap(),
    );
    assert!(
        omitted
            .selected_compiled_profile(&omitted.profiles[1])
            .is_none()
    );
    assert!(Arc::ptr_eq(
        omitted
            .selected_compiled_profile(&omitted.profiles[0])
            .unwrap(),
        &pure
    ));
}
