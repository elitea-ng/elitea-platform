//! Real Remote staging path with trusted transport/sink doubles and actual Started CAS.
//! No control/sandbox/database socket is contacted; this is not mTLS/PG proof.
use super::*;
use crate::{
    agents::graph::{
        code_debug::{
            CodeDebugAdmission, CodeDebugArtifactReference, CodeDebugArtifactSink, CodeDebugFailure,
        },
        code_remote::CodeRuntimeFactory,
    },
    protocol::control::ClaimBoundSandboxAuthority,
    transport::input_content::{InputContentRpc, InputContentTransportError},
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use http_body_util::BodyExt as _;

#[derive(Default)]
struct Calls {
    order: Vec<&'static str>,
    visit: Option<serde_json::Value>,
    admission: Option<serde_json::Value>,
    snapshot: Option<serde_json::Value>,
}
struct OriginalVisitRpc(Arc<std::sync::Mutex<Calls>>);
#[async_trait]
impl InputContentRpc for OriginalVisitRpc {
    async fn get(
        &self,
        request: http::Request<tonic::body::Body>,
    ) -> Result<http::Response<tonic::body::Body>, InputContentTransportError> {
        assert_eq!(request.method(), http::Method::POST);
        assert!(
            request.uri().path().ends_with("/code-sandbox/visits"),
            "test must never request a dispatch grant/final intent"
        );
        assert!(request.headers().contains_key("x-elitea-claim-id"));
        assert!(request.headers().contains_key("x-elitea-fence"));
        let segments: Vec<_> = request.uri().path().split('/').collect();
        let execution = segments[2].to_owned();
        let generation: u64 = segments[4].parse().unwrap();
        let bytes = request.into_body().collect().await.unwrap().to_bytes();
        let visit: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(
            visit["schema"],
            "elitea.sandbox.original-code-visit-request.v1"
        );
        assert!(visit["saved_child_scope"].is_null());
        let pre = URL_SAFE_NO_PAD
            .decode(
                visit["pre_workspace_prepared_job_json_base64url"]
                    .as_str()
                    .unwrap(),
            )
            .unwrap();
        let job: serde_json::Value = serde_json::from_slice(&pre).unwrap();
        assert_eq!(job["revision"], 1);
        let response = serde_json::json!({
            "schema":"elitea.sandbox.original-code-visit-response.v1",
            "original_visit":{"visit_id":"1".repeat(64),"revision":1,"digest_sha256":"2".repeat(64)},
            "execution_id":execution,"original_generation":generation,"activation_id":visit["activation_id"],
            "attempt":visit["attempt"],"node_digest":visit["node_digest"],
            "pre_workspace_prepared_sha256":crate::sandbox::code_recovery::sha256(&pre),
        });
        let mut calls = self.0.lock().unwrap();
        calls.order.push("visits");
        calls.visit = Some(visit);
        drop(calls);
        let raw = serde_json::to_vec(&response).unwrap();
        Ok(http::Response::builder()
            .status(http::StatusCode::OK)
            .version(http::Version::HTTP_2)
            .header(http::header::CONTENT_TYPE, "application/json")
            .header(http::header::CONTENT_LENGTH, raw.len())
            .body(tonic::body::Body::new(http_body_util::Full::new(
                bytes::Bytes::from(raw),
            )))
            .unwrap())
    }
}
struct Sink(Arc<std::sync::Mutex<Calls>>);
struct ChangedAuthorityInput {
    inner: Arc<dyn CodeSandboxRuntime>,
    config: bool,
}
#[async_trait]
impl CodeSandboxRuntime for ChangedAuthorityInput {
    async fn execute(&self, _: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
        panic!("raw invocation cannot substitute the Started authority")
    }
    async fn execute_attempt(
        &self,
        mut invocation: CodeInvocation<'_>,
        authority: &NodeAttemptAuthority,
    ) -> Result<Vec<u8>, CodeAttemptFailure> {
        if self.config {
            invocation
                .original
                .as_mut()
                .unwrap()
                .configuration_json
                .push(' ');
        } else {
            invocation.activation[0] ^= 1;
        }
        self.inner.execute_attempt(invocation, authority).await
    }
}
#[async_trait]
impl CodeDebugArtifactSink for Sink {
    async fn export(
        &self,
        authority: &ClaimBoundSandboxAuthority,
        admission: &CodeDebugAdmission,
        snapshot: &[u8],
    ) -> Result<CodeDebugArtifactReference, CodeDebugFailure> {
        let request = authority.request(&[7; 32]);
        let project: u64 = request
            .identity
            .as_ref()
            .unwrap()
            .resource_project_id
            .parse()
            .unwrap();
        let mut calls = self.0.lock().unwrap();
        assert_eq!(calls.order.as_slice(), ["visits"]);
        assert_eq!(
            admission.activation_id,
            calls.visit.as_ref().unwrap()["activation_id"]
        );
        assert_eq!(
            u64::from(admission.attempt),
            calls.visit.as_ref().unwrap()["attempt"]
        );
        calls.order.push("debug");
        calls.admission = Some(serde_json::to_value(admission).unwrap());
        calls.snapshot = Some(serde_json::from_slice(snapshot).unwrap());
        Ok(CodeDebugArtifactReference {
            schema_version: "elitea.runtime.code-debug-artifact.v1".into(),
            project_id: project,
            bucket: "code-debug".into(),
            name: format!("{}.json", "3".repeat(64)),
            media_type: "application/json".into(),
            byte_length: snapshot.len(),
            sha256: crate::sandbox::code_recovery::sha256(snapshot),
        })
    }
}

#[tokio::test]
#[allow(
    clippy::similar_names,
    reason = "Keep exact execution and request identity names in the fixture."
)]
async fn real_remote_original_visit_and_debug_precede_dependency_failure_on_default_and_recovered_started()
 {
    for recovering in [false, true] {
        let yaml = "entry_point: code\nstate:\n  count: int\n  value: int\n  private: str\nnodes:\n  - id: code\n    type: code\n    language: rust\n    code: 'pub fn main(){}'\n    dependencies: |\n      [dependencies]\n      itoa = '1'\n    input: [value]\n    output: [count]\n    debug: true\n    transition: END\n";
        let pipeline = crate::agents::graph::compiler::PipelineDefinition::from_yaml(yaml).unwrap();
        let definition = pipeline.original_code_definition_fixture("code").unwrap();
        let digest = definition.validated_digest();
        let calls = Arc::new(std::sync::Mutex::new(Calls::default()));
        let content = Arc::new(
            crate::transport::InputContentClient::with_original_code_visit_fixture_rpc(
                OriginalVisitRpc(calls.clone()),
            ),
        );
        let runtime = CodeRuntimeFactory::original_visit_debug_caller_fixture(
            content,
            Arc::new(Sink(calls.clone())),
        );
        let store = factory();
        let context = ctx();
        let before = context.state.clone();
        let policy = NodeRecoveryPolicy::default();
        let activation = NodeAttemptActivation::from_context("code", digest, &context).unwrap();
        let journal = store.open(&activation, &policy).await.unwrap();
        if recovering {
            let original = journal.load().await.unwrap();
            journal
                .append(&original, original.ledger.start_attempt(90).unwrap(), None)
                .await
                .unwrap();
        }
        let body = Arc::new(CodeNode::new(definition, types(), runtime).unwrap());
        let mut node =
            RecoverableNode::new(body, digest, policy, store, None).with_legacy_first_attempt();
        node.clock = Arc::new(Clock(AtomicU64::new(100)));
        let stopped = node.execute(&context).await;
        if recovering {
            assert!(
                stopped.unwrap().interrupt.is_some(),
                "recovered Started preparation uncertainty requires reconciliation"
            );
        } else {
            assert!(
                stopped.is_err(),
                "fresh pre-dispatch failure must stop Code"
            );
        }
        assert_eq!(context.state, before);
        {
            let calls = calls.lock().unwrap();
            assert_eq!(calls.order, ["visits", "debug"]);
            let admission = calls.admission.as_ref().unwrap();
            let pre = URL_SAFE_NO_PAD
                .decode(
                    calls.visit.as_ref().unwrap()["pre_workspace_prepared_job_json_base64url"]
                        .as_str()
                        .unwrap(),
                )
                .unwrap();
            assert_eq!(
                admission["request_sha256"],
                crate::sandbox::code_recovery::sha256(&pre)
            );
            assert_eq!(admission["attempt"], 1);
            assert_eq!(admission["original_visit"]["visit_id"], "1".repeat(64));
            let snapshot = calls.snapshot.as_ref().unwrap();
            assert_eq!(snapshot["source"], "pub fn main(){}");
            assert_eq!(snapshot["selected_input"], serde_json::json!({"value":7}));
        }
        let stored = journal.load().await.unwrap();
        assert_eq!(
            stored.ledger.history().len(),
            1,
            "no node replay or execute grant was issued"
        );
        assert!(matches!(
            stored.ledger.history()[0].outcome,
            crate::agents::graph::node_recovery::AttemptOutcome::Failed { .. }
        ));
    }
}

#[tokio::test]
#[allow(
    clippy::similar_names,
    reason = "Keep exact execution and request identity names in the fixture."
)]
async fn real_remote_changed_dispatch_or_config_is_denied_before_visit_or_debug_upload() {
    for changed_config in [false, true] {
        let yaml = "entry_point: code\nstate:\n  count: int\n  value: int\n  private: str\nnodes:\n  - id: code\n    type: code\n    language: rust\n    code: 'pub fn main(){}'\n    dependencies: |\n      [dependencies]\n      itoa = '1'\n    input: [value]\n    output: [count]\n    debug: true\n    transition: END\n";
        let pipeline = crate::agents::graph::compiler::PipelineDefinition::from_yaml(yaml).unwrap();
        let definition = pipeline.original_code_definition_fixture("code").unwrap();
        let digest = definition.validated_digest();
        let calls = Arc::new(std::sync::Mutex::new(Calls::default()));
        let content = Arc::new(
            crate::transport::InputContentClient::with_original_code_visit_fixture_rpc(
                OriginalVisitRpc(calls.clone()),
            ),
        );
        let inner = CodeRuntimeFactory::original_visit_debug_caller_fixture(
            content,
            Arc::new(Sink(calls.clone())),
        );
        let runtime = Arc::new(ChangedAuthorityInput {
            inner,
            config: changed_config,
        });
        let store = factory();
        let context = ctx();
        let before = context.state.clone();
        let policy = NodeRecoveryPolicy::default();
        let activation = NodeAttemptActivation::from_context("code", digest, &context).unwrap();
        let journal = store.open(&activation, &policy).await.unwrap();
        let body = Arc::new(CodeNode::new(definition, types(), runtime).unwrap());
        let mut node =
            RecoverableNode::new(body, digest, policy, store, None).with_legacy_first_attempt();
        node.clock = Arc::new(Clock(AtomicU64::new(100)));
        assert!(node.execute(&context).await.is_err());
        assert!(calls.lock().unwrap().order.is_empty());
        assert_eq!(context.state, before);
        let stored = journal.load().await.unwrap();
        assert!(matches!(
            stored.ledger.history()[0].outcome,
            crate::agents::graph::node_recovery::AttemptOutcome::Failed {
                failure: NodeFailure {
                    class: NodeFailureClass::AuthorizationDenied,
                    replay: crate::agents::graph::node_recovery::ReplaySafety::NoExternalEffect,
                },
                decision: RecoveryDecision::Stop(
                    crate::agents::graph::node_recovery::StopReason::RetryDisabled
                ),
            }
        ));
    }
}

#[tokio::test]
#[allow(
    clippy::similar_names,
    reason = "Keep exact execution and request identity names in the fixture."
)]
async fn workspace_activation_preserves_visit_debug_and_typed_preparation_refusal() {
    for recovering in [false, true] {
        let yaml = format!(
            "entry_point: code\nstate:\n  count: int\n  value: int\n  private: str\nnodes:\n  - id: code\n    type: code\n    language: rust\n    code: 'pub fn main(){{}}'\n    dependencies: |\n      [dependencies]\n      itoa = '1'\n    input: [value]\n    output: [count]\n    debug: true\n    workspace:\n      toolkit_id: 12\n      toolkit_reference_sha256: '{}'\n      repository_id: '123'\n      commit: '{}'\n      mode: read\n      include: [src]\n    transition: END\n",
            "1".repeat(64),
            "2".repeat(40),
        );
        let pipeline =
            crate::agents::graph::compiler::PipelineDefinition::from_yaml(&yaml).unwrap();
        let definition = pipeline.original_code_definition_fixture("code").unwrap();
        let digest = definition.validated_digest();
        let calls = Arc::new(std::sync::Mutex::new(Calls::default()));
        let content = Arc::new(
            crate::transport::InputContentClient::with_original_code_visit_fixture_rpc(
                OriginalVisitRpc(calls.clone()),
            ),
        );
        let runtime = CodeRuntimeFactory::original_visit_debug_caller_fixture(
            content,
            Arc::new(Sink(calls.clone())),
        );
        let store = factory();
        let context = ctx();
        let before = context.state.clone();
        let policy = NodeRecoveryPolicy::default();
        let activation = NodeAttemptActivation::from_context("code", digest, &context).unwrap();
        let journal = store.open(&activation, &policy).await.unwrap();
        if recovering {
            let original = journal.load().await.unwrap();
            journal
                .append(&original, original.ledger.start_attempt(90).unwrap(), None)
                .await
                .unwrap();
        }
        let body = Arc::new(CodeNode::new(definition, types(), runtime).unwrap());
        let mut node =
            RecoverableNode::new(body, digest, policy, store, None).with_legacy_first_attempt();
        node.clock = Arc::new(Clock(AtomicU64::new(100)));
        let output = node.execute(&context).await;
        if recovering {
            assert!(output.unwrap().interrupt.is_some());
        } else {
            assert!(output.is_err());
        }
        assert_eq!(context.state, before);
        assert_eq!(calls.lock().unwrap().order, ["visits", "debug"]);
        let stored = journal.load().await.unwrap();
        assert_eq!(stored.ledger.history().len(), 1);
        // Missing dependency preparation remains the real refusal. A saved
        // workspace does not impose an artificial earlier Admission failure.
        assert!(matches!(
            stored.ledger.history()[0].outcome,
            crate::agents::graph::node_recovery::AttemptOutcome::Failed {
                failure: NodeFailure {
                    class: NodeFailureClass::Unknown,
                    ..
                },
                ..
            }
        ));
    }
}
