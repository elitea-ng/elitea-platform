//! Graph Code admission and exact supervisor authority. No live services run.
use super::super::{code::CodeNodeDefinition, code_runtime::CodeNode};
use super::*;
use adk_rust::graph::{ExecutionConfig, Node, NodeContext, State};
use serde_json::json;
use std::{collections::BTreeMap, sync::Mutex};

fn profile(language: CodeLanguage) -> CodeRuntimeProfile {
    CodeRuntimeProfile {
        platform_client: None,
        language,
        image_digest: format!("sha256:{}", "a".repeat(64)),
        policy_revision: "isolated-v1".into(),
        timeout_seconds: 30,
        client: SandboxClient::from_channel(
            tonic::transport::Endpoint::from_static("http://127.0.0.1:1").connect_lazy(),
            "sandbox-test".into(),
            Duration::from_secs(30),
        )
        .unwrap(),
        preparation: None,
        preparation_client: None,
        compiled_profile: None,
    }
}
impl CodeRuntimeFactory {
    pub(in crate::agents) fn original_visit_debug_caller_fixture(
        content: Arc<crate::transport::InputContentClient>,
        sink: Arc<dyn super::super::code_debug::CodeDebugArtifactSink>,
    ) -> Arc<dyn CodeSandboxRuntime> {
        let authority =
            Arc::new(ClaimBoundSandboxAuthority::original_code_visit_conformance_fixture());
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
            .acquire_timeout(Duration::from_millis(50))
            .connect_lazy("postgres://fixture:fixture@127.0.0.1:1/never_connect")
            .unwrap();
        let factory = CodeRuntimeFactory {
            intent_content: Some(content),
            control,
            profiles: vec![profile(CodeLanguage::Rust)].into(),
            journal: Arc::new(crate::sandbox::dispatch::DispatchJournal::new(state)),
            compiled_profile: None,
            debug_sink: Some(sink),
        };
        factory.bind(authority)
    }
}

fn invocation(source: &str, provenance: CodeProvenance) -> CodeInvocation<'_> {
    CodeInvocation {
        activation: [7; 32],
        language: CodeLanguage::Python,
        platform_client: false,
        source,
        dependencies_toml: None,
        provenance,
        input_json: br#"{"count":2,"text":"line\\nexact"}"#.to_vec(),
        trace: None,
        original: None,
        debug: None,
        workspace: None,
    }
}

#[tokio::test]
async fn declared_resolvers_preserve_exact_source_and_selected_input() {
    let source = "# Unicode: \u{03bb}\r\n'escaped\\ntext'\n";
    let configured = profile(CodeLanguage::Python);
    let mut fingerprint = None;
    for provenance in [
        CodeProvenance::SavedLiteral,
        CodeProvenance::StateVariable,
        CodeProvenance::StateTemplate,
    ] {
        let invocation = invocation(source, provenance);
        validate_invocation(&invocation).unwrap();
        let job = prepare_job(&invocation, &configured).unwrap();
        let wire: serde_json::Value = serde_json::from_slice(&job.to_transport().unwrap()).unwrap();
        assert_eq!(
            wire["source"].as_str().unwrap().as_bytes(),
            source.as_bytes()
        );
        assert_eq!(wire["input"], json!({"count":2,"text":"line\\nexact"}));
        assert_eq!(wire["language"], "python");
        assert_eq!(wire["policy_revision"], "isolated-v1");
        let digest = job.fingerprint().unwrap();
        if let Some(original) = fingerprint {
            // Provenance describes the resolver. Authority binds its actual bytes.
            assert_eq!(digest, original);
        }
        fingerprint = Some(digest);
    }
}

#[test]
fn saved_dependencies_do_not_select_another_code_language() {
    for provenance in [
        CodeProvenance::SavedLiteral,
        CodeProvenance::StateVariable,
        CodeProvenance::StateTemplate,
    ] {
        let mut invocation = invocation("7", provenance);
        invocation.dependencies_toml = Some("[dependencies]\nitoa='1'\n");
        invocation.language = CodeLanguage::Rust;
        validate_invocation(&invocation).unwrap();
        for language in [
            CodeLanguage::Python,
            CodeLanguage::JavaScript,
            CodeLanguage::TypeScript,
        ] {
            invocation.language = language;
            assert!(validate_invocation(&invocation).is_err());
        }
    }
}

struct Capture {
    activation: [u8; 32],
    fingerprint: [u8; 32],
}

struct CapturingRuntime {
    configured: CodeRuntimeProfile,
    calls: Mutex<Vec<Capture>>,
}

#[async_trait]
impl CodeSandboxRuntime for CapturingRuntime {
    async fn execute(&self, invocation: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
        validate_invocation(&invocation)?;
        let job = prepare_job(&invocation, &self.configured)?;
        self.calls.lock().unwrap().push(Capture {
            activation: invocation.activation,
            fingerprint: job.fingerprint().unwrap(),
        });
        Ok(serde_json::to_vec(&json!({
            "revision":1,"status":"completed","exit_code":0,
            "stdout":"{\"revision\":1,\"result\":7}","stderr":""
        }))
        .unwrap())
    }
}

fn node(runtime: Arc<CapturingRuntime>, additional: &str) -> CodeNode {
    CodeNode::new(
        CodeNodeDefinition::from_yaml(&format!(
            "id: run\ntype: code\ncode: {{type: variable, value: source_text}}\ninput: [count]\noutput: [count]\ntransition: END\n{additional}"
        ))
        .unwrap(),
        BTreeMap::from([
            ("source_text".into(), "str".into()),
            ("count".into(), "int".into()),
        ]),
        runtime,
    )
    .unwrap()
}

fn context(thread: &str, step: usize) -> NodeContext {
    NodeContext::new(
        State::from([
            ("source_text".into(), json!("7\r\n")),
            ("count".into(), json!(2)),
        ]),
        ExecutionConfig::new(thread),
        step,
    )
}

#[tokio::test]
async fn invalid_dynamic_source_or_selected_input_never_enters_runtime() {
    let runtime = Arc::new(CapturingRuntime {
        configured: profile(CodeLanguage::Python),
        calls: Mutex::new(Vec::new()),
    });
    let node = node(runtime.clone(), "");
    for field in ["source_text", "count"] {
        let mut invalid = context("root", 4);
        invalid.state.insert(field.into(), json!(false));
        assert!(node.execute(&invalid).await.is_err());
        assert!(runtime.calls.lock().unwrap().is_empty());
    }
    let mut missing = context("root", 4);
    missing.state.remove("source_text");
    assert!(node.execute(&missing).await.is_err());
    assert!(runtime.calls.lock().unwrap().is_empty());
    assert!(node.execute(&context("", 4)).await.is_err());
    assert!(runtime.calls.lock().unwrap().is_empty());
    let output = node.execute(&context("root", 4)).await.unwrap();
    assert_eq!(output.updates, State::from([("count".into(), json!(7))]));
    let calls = runtime.calls.lock().unwrap();
    assert_eq!(calls.len(), 1);
    assert_ne!(calls[0].activation, [0; 32]);
    let expected = CodeInvocation {
        input_json: br#"{"count":2}"#.to_vec(),
        ..invocation("7\r\n", CodeProvenance::StateVariable)
    };
    assert_eq!(
        calls[0].fingerprint,
        prepare_job(&expected, &runtime.configured)
            .unwrap()
            .fingerprint()
            .unwrap()
    );
}

#[cfg(feature = "sandbox-supervisor")]
mod signed_grants {
    use super::*;
    use crate::protocol::{
        command::Ed25519PublicKeyResolver,
        elitea::runtime::v1::{SandboxJobGrantClaimsV1, SignedSandboxJobGrantV1},
        sandbox_grant::GrantVerifier,
    };
    use prost::Message;
    use ring::signature::{self, KeyPair};

    struct Keys([u8; 32]);
    impl Ed25519PublicKeyResolver for Keys {
        fn resolve_ed25519_public_key(&self, key_id: &str) -> Option<[u8; 32]> {
            (key_id == "code-test-key").then_some(self.0)
        }
    }

    fn signer() -> (signature::Ed25519KeyPair, GrantVerifier<Keys>) {
        let key = signature::Ed25519KeyPair::from_seed_unchecked(&[17; 32]).unwrap();
        let public: [u8; 32] = key.public_key().as_ref().try_into().unwrap();
        let verifier = GrantVerifier::new(Keys(public), "sandbox-test".into()).unwrap();
        (key, verifier)
    }

    fn claims(activation: &[u8; 32], fingerprint: &[u8; 32]) -> SandboxJobGrantClaimsV1 {
        SandboxJobGrantClaimsV1 {
            revision: 1,
            tenant_id: "tenant-test".into(),
            project_id: 2,
            execution_id: "execution-test".into(),
            activation_id: crate::sandbox::dependency_bundle::hex(activation),
            request_digest: fingerprint.to_vec(),
            submitter_workload_identity: "worker-test".into(),
            audience: "sandbox-test".into(),
            issued_at_unix_millis: 1000,
            expires_at_unix_millis: 31_000,
            generation: 1,
            ..Default::default()
        }
    }

    fn sign(
        key: &signature::Ed25519KeyPair,
        claims: &SandboxJobGrantClaimsV1,
    ) -> SignedSandboxJobGrantV1 {
        let bytes = claims.encode_to_vec();
        let mut body = b"elitea.sandbox.job-grant.ed25519.v1\0".to_vec();
        body.extend_from_slice(&(bytes.len() as u64).to_be_bytes());
        body.extend_from_slice(&bytes);
        SignedSandboxJobGrantV1 {
            key_id: "code-test-key".into(),
            claims_bytes: bytes,
            signature: key.sign(&body).as_ref().to_vec(),
        }
    }

    #[tokio::test]
    async fn changed_dynamic_request_cannot_use_the_original_execution_grant() {
        let baseline = invocation("7\r\n", CodeProvenance::StateVariable);
        let mut configured = profile(CodeLanguage::Python);
        let original = prepare_job(&baseline, &configured).unwrap();
        let (key, verifier) = signer();
        let grant = sign(
            &key,
            &claims(&baseline.activation, &original.fingerprint().unwrap()),
        );
        let authorized = verifier
            .verify(&grant, "worker-test", &original, 1000)
            .unwrap();
        assert!(authorized.permits(&original, 1000));
        let mut changed = vec![
            prepare_job(
                &CodeInvocation {
                    source: "7\n",
                    ..invocation("7\r\n", CodeProvenance::StateTemplate)
                },
                &configured,
            )
            .unwrap(),
            prepare_job(
                &CodeInvocation {
                    input_json: br#"{"count":3}"#.to_vec(),
                    ..invocation("7\r\n", CodeProvenance::StateVariable)
                },
                &configured,
            )
            .unwrap(),
            prepare_job(
                &CodeInvocation {
                    language: CodeLanguage::JavaScript,
                    platform_client: false,
                    ..invocation("7\r\n", CodeProvenance::StateVariable)
                },
                &configured,
            )
            .unwrap(),
        ];
        configured.image_digest = format!("sha256:{}", "b".repeat(64));
        changed.push(prepare_job(&baseline, &configured).unwrap());
        configured.image_digest = format!("sha256:{}", "a".repeat(64));
        configured.policy_revision = "isolated-v2".into();
        changed.push(prepare_job(&baseline, &configured).unwrap());
        configured.policy_revision = "isolated-v1".into();
        configured.timeout_seconds = 31;
        changed.push(prepare_job(&baseline, &configured).unwrap());
        for request in changed {
            assert!(!authorized.permits(&request, 1000));
            assert!(
                verifier
                    .verify(&grant, "worker-test", &request, 1000)
                    .is_err()
            );
        }
    }

    #[tokio::test]
    async fn forged_changed_scope_and_stale_grants_fail_before_execution() {
        let invocation = invocation("7", CodeProvenance::StateTemplate);
        let job = prepare_job(&invocation, &profile(CodeLanguage::Python)).unwrap();
        let (key, verifier) = signer();
        let original = claims(&invocation.activation, &job.fingerprint().unwrap());
        let grant = sign(&key, &original);
        let mut rejected = Vec::new();
        let mut forged = grant.clone();
        forged.signature[0] ^= 1;
        rejected.push(forged);
        for field in 0..6 {
            let mut substituted = original.clone();
            match field {
                0 => substituted.tenant_id = "other-tenant".into(),
                1 => substituted.project_id = 3,
                2 => substituted.execution_id = "other-execution".into(),
                3 => substituted.activation_id = "08".repeat(32),
                4 => substituted.generation = 2,
                _ => substituted.request_digest = vec![8; 32],
            }
            let mut substituted_grant = grant.clone();
            substituted_grant.claims_bytes = substituted.encode_to_vec();
            rejected.push(substituted_grant);
        }
        let mut wrong_audience = original.clone();
        wrong_audience.audience = "other-supervisor".into();
        rejected.push(sign(&key, &wrong_audience));
        let mut stop_only = original.clone();
        stop_only.revision = 2;
        stop_only.cancel_only = true;
        rejected.push(sign(&key, &stop_only));
        let mut effects = 0;
        for rejected in rejected {
            if verifier
                .verify(&rejected, "worker-test", &job, 1000)
                .is_ok()
            {
                effects += 1;
            }
        }
        for (peer, now) in [
            ("other-worker", 1000),
            ("worker-test", 999),
            ("worker-test", 31_000),
        ] {
            if verifier.verify(&grant, peer, &job, now).is_ok() {
                effects += 1;
            }
        }
        assert_eq!(effects, 0);
    }

    #[tokio::test]
    async fn dynamic_recovery_keeps_original_scope_but_thread_step_or_definition_changes_it() {
        let runtime = Arc::new(CapturingRuntime {
            configured: profile(CodeLanguage::Python),
            calls: Mutex::new(Vec::new()),
        });
        let node = node(runtime.clone(), "");
        for (thread, step) in [("root", 4), ("root", 4), ("root", 5), ("child", 4)] {
            node.execute(&context(thread, step)).await.unwrap();
        }
        super::node(runtime.clone(), "debug: true\n")
            .execute(&context("root", 4))
            .await
            .unwrap();
        let calls = runtime.calls.lock().unwrap();
        assert_eq!(calls[0].activation, calls[1].activation);
        let invocation = CodeInvocation {
            input_json: br#"{"count":2}"#.to_vec(),
            ..invocation("7\r\n", CodeProvenance::StateVariable)
        };
        let job = prepare_job(&invocation, &runtime.configured).unwrap();
        let (key, verifier) = signer();
        let original_claims = claims(&calls[0].activation, &calls[0].fingerprint);
        let original_grant = sign(&key, &original_claims);
        let original = verifier
            .verify(&original_grant, "worker-test", &job, 1000)
            .unwrap();
        let mut replacement = original_claims.clone();
        replacement.generation = 2;
        replacement.submitter_workload_identity = "replacement-worker".into();
        let recovered = verifier
            .verify(&sign(&key, &replacement), "replacement-worker", &job, 1000)
            .unwrap();
        assert!(recovered.scope() == original.scope());
        assert!(
            verifier
                .verify(&original_grant, "replacement-worker", &job, 1000)
                .is_err()
        );
        for changed in &calls[2..] {
            assert_ne!(changed.activation, calls[0].activation);
            assert_eq!(changed.fingerprint, calls[0].fingerprint);
            let updated = claims(&changed.activation, &changed.fingerprint);
            let mut reused = original_grant.clone();
            reused.claims_bytes = updated.encode_to_vec();
            assert!(verifier.verify(&reused, "worker-test", &job, 1000).is_err());
            let fresh = verifier
                .verify(&sign(&key, &updated), "worker-test", &job, 1000)
                .unwrap();
            assert!(fresh.scope() != original.scope());
        }
    }
}
