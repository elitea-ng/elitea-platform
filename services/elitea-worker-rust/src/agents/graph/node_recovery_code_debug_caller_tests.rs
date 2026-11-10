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
    protocol::elitea::runtime::v1::{RegisterCodeVisitRequestV1, RegisterCodeVisitResponseV1},
    transport::input_content::content_fake::{ContentFake, claim_of},
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};

#[derive(Default)]
struct Calls {
    order: Vec<&'static str>,
    visit: Option<serde_json::Value>,
    admission: Option<serde_json::Value>,
    snapshot: Option<serde_json::Value>,
}
struct OriginalVisitRpc(Arc<std::sync::Mutex<Calls>>);
/// Only the visit RPC is served: the test must never request a dispatch grant
/// or a final intent, and every other RPC answers Unimplemented.
#[tonic::async_trait]
impl ContentFake for OriginalVisitRpc {
    async fn register_code_visit(
        &self,
        request: tonic::Request<RegisterCodeVisitRequestV1>,
    ) -> Result<tonic::Response<RegisterCodeVisitResponseV1>, tonic::Status> {
        let (claim, fence) = claim_of(&request);
        assert_ne!(claim, "");
        assert_eq!(fence.len(), 32);
        let request = request.into_inner();
        let scope = request.scope.unwrap();
        let execution = scope.execution_id;
        let generation = scope.generation;
        let visit: serde_json::Value = serde_json::from_slice(&request.visit_json).unwrap();
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
        Ok(tonic::Response::new(RegisterCodeVisitResponseV1 {
            visit_json: serde_json::to_vec(&response).unwrap(),
        }))
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
        assert_eq!(calls.lock().unwrap().order, [] as [&str; 0]);
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

// ---- W3-1: deadline admission and Code slots on the journaled path ----

const ADMISSION_YAML: &str = "entry_point: code\nstate:\n  count: int\n  value: int\n  private: str\nnodes:\n  - id: code\n    type: code\n    language: rust\n    code: 'pub fn main(){}'\n    dependencies: |\n      [dependencies]\n      itoa = '1'\n    input: [value]\n    output: [count]\n    debug: true\n    transition: END\n";

/// The fixture profile's `timeout_seconds` (30) plus the observation margin.
fn admission_required() -> std::time::Duration {
    std::time::Duration::from_secs(30)
        + elitea_agent_runtime::graph::fanout::admission::CODE_OBSERVATION_MARGIN
}

struct AdmissionRun {
    stopped: Result<adk_rust::graph::NodeOutput, GraphError>,
    calls: Arc<std::sync::Mutex<Calls>>,
    history: Vec<crate::agents::graph::node_recovery::AttemptOutcome>,
    failure_code: Option<&'static str>,
}

/// One Code visit on the real remote journaled path. `recovering` marks the
/// attempt Started first: its dispatch journal never answers, so the earlier
/// claim may have submitted (the C2 case).
async fn admission_run(
    execution_deadline: tokio::time::Instant,
    slots: &elitea_agent_runtime::graph::fanout::admission::CodeSlots,
    recovering: bool,
) -> AdmissionRun {
    let pipeline =
        crate::agents::graph::compiler::PipelineDefinition::from_yaml(ADMISSION_YAML).unwrap();
    let definition = pipeline.original_code_definition_fixture("code").unwrap();
    let digest = definition.validated_digest();
    let calls = Arc::new(std::sync::Mutex::new(Calls::default()));
    let main_rpc = Arc::new(
        crate::transport::InputContentClient::with_original_code_visit_fixture_rpc(
            OriginalVisitRpc(calls.clone()),
        ),
    );
    let runtime = CodeRuntimeFactory::original_visit_admission_fixture(
        main_rpc,
        Some(Arc::new(Sink(calls.clone()))),
        execution_deadline,
        slots,
    );
    let store = factory();
    let context = ctx();
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
    let (sender, receiver) = crate::agents::graph::node_events::pipeline_node_event_channel();
    let body = Arc::new(
        CodeNode::new(definition, types(), runtime)
            .unwrap()
            .with_events(Some(sender)),
    );
    let mut node =
        RecoverableNode::new(body, digest, policy, store, None).with_legacy_first_attempt();
    node.clock = Arc::new(Clock(AtomicU64::new(100)));
    let stopped = tokio::time::timeout(std::time::Duration::from_secs(600), node.execute(&context))
        .await
        .expect("the attempt ends; one slot is never taken twice");
    let history = journal
        .load()
        .await
        .unwrap()
        .ledger
        .history()
        .iter()
        .map(|entry| entry.outcome)
        .collect();
    AdmissionRun {
        stopped,
        calls,
        history,
        failure_code: receiver.try_failure_code().await,
    }
}

/// D4 on the journaled path: exactly `timeout + margin` left is admitted (the
/// original visit and the debug export run, and the attempt stops later at
/// preparation); one millisecond less is refused typed before the visit, the
/// journal or any submission, and no slot stays held.
#[tokio::test(start_paused = true)]
async fn journaled_path_admits_exactly_required_and_refuses_one_ms_less() {
    let slots = elitea_agent_runtime::graph::fanout::admission::CodeSlots::new(1);
    let now = tokio::time::Instant::now();
    let admitted = admission_run(now + admission_required(), &slots, false).await;
    assert_eq!(admitted.calls.lock().unwrap().order, ["visits", "debug"]);
    assert_ne!(
        admitted.failure_code,
        Some(crate::protocol::output::CODE_DEADLINE_INSUFFICIENT_UPSTREAM_CODE)
    );
    assert_eq!(slots.available(), 1);

    let refused = admission_run(
        now + admission_required() - std::time::Duration::from_millis(1),
        &slots,
        false,
    )
    .await;
    assert!(refused.stopped.is_err(), "a refused attempt stops the node");
    assert!(
        refused.calls.lock().unwrap().order.is_empty(),
        "no original visit, debug export, journal row or submission"
    );
    assert_eq!(
        refused.failure_code,
        Some(crate::protocol::output::CODE_DEADLINE_INSUFFICIENT_UPSTREAM_CODE)
    );
    assert_eq!(refused.history.len(), 1, "one attempt, never retried");
    assert!(matches!(
        refused.history[0],
        crate::agents::graph::node_recovery::AttemptOutcome::Failed { .. }
    ));
    assert_eq!(slots.available(), 1, "a refusal holds nothing");
}

/// C2: an attempt that may already have started (recovered Started, its
/// dispatch journal unreadable, so a submission is possible) is reconciled,
/// never refused by deadline admission, even with far less than
/// `timeout + margin` left: it takes its slot and continues to the visit.
#[tokio::test(start_paused = true)]
async fn a_started_attempt_recovered_with_too_little_time_is_reconciled_not_refused() {
    let slots = elitea_agent_runtime::graph::fanout::admission::CodeSlots::new(1);
    let run = admission_run(
        tokio::time::Instant::now() + std::time::Duration::from_secs(5),
        &slots,
        true,
    )
    .await;
    assert_eq!(run.calls.lock().unwrap().order, ["visits", "debug"]);
    assert_ne!(
        run.failure_code,
        Some(crate::protocol::output::CODE_DEADLINE_INSUFFICIENT_UPSTREAM_CODE)
    );
    assert!(
        run.stopped.unwrap().interrupt.is_some(),
        "a possibly started attempt goes to reconciliation"
    );
    assert_eq!(slots.available(), 1);
}

/// The deadlock regression: with `slots = 1` one attempt takes its one slot
/// once and keeps it through preparation; it ends (here at the preparation
/// step) instead of waiting on itself.
#[tokio::test(start_paused = true)]
async fn one_attempt_takes_one_slot_through_preparation() {
    let slots = elitea_agent_runtime::graph::fanout::admission::CodeSlots::new(1);
    let run = admission_run(
        tokio::time::Instant::now() + std::time::Duration::from_secs(3600),
        &slots,
        false,
    )
    .await;
    assert_eq!(run.calls.lock().unwrap().order, ["visits", "debug"]);
    assert!(
        run.stopped.is_err(),
        "the fixture has no preparation backend"
    );
    assert_eq!(slots.available(), 1);
}

// ---- W3-2: a Code child's visit names its fan-out owner scope (C1, C4) ----

/// Main's visit answer for a fan-out child, recording the visit it got.
struct ScopedVisitRpc(Arc<std::sync::Mutex<Option<serde_json::Value>>>);
#[tonic::async_trait]
impl ContentFake for ScopedVisitRpc {
    async fn register_code_visit(
        &self,
        request: tonic::Request<RegisterCodeVisitRequestV1>,
    ) -> Result<tonic::Response<RegisterCodeVisitResponseV1>, tonic::Status> {
        let request = request.into_inner();
        let scope = request.scope.unwrap();
        let visit: serde_json::Value = serde_json::from_slice(&request.visit_json).unwrap();
        let pre = URL_SAFE_NO_PAD
            .decode(
                visit["pre_workspace_prepared_job_json_base64url"]
                    .as_str()
                    .unwrap(),
            )
            .unwrap();
        let response = serde_json::json!({
            "schema":"elitea.sandbox.original-code-visit-response.v1",
            "original_visit":{"visit_id":"1".repeat(64),"revision":1,"digest_sha256":"2".repeat(64)},
            "execution_id":scope.execution_id,"original_generation":scope.generation,
            "activation_id":visit["activation_id"],"attempt":visit["attempt"],
            "node_digest":visit["node_digest"],
            "pre_workspace_prepared_sha256":crate::sandbox::code_recovery::sha256(&pre),
        });
        *self.0.lock().unwrap() = Some(visit);
        Ok(tonic::Response::new(RegisterCodeVisitResponseV1 {
            visit_json: serde_json::to_vec(&response).unwrap(),
        }))
    }
}

fn child_scope() -> crate::agents::pipeline::saved_child_http::FanoutCodeChildScope {
    crate::agents::pipeline::saved_child_http::FanoutCodeChildScope {
        schema: crate::agents::pipeline::saved_child_http::FANOUT_CODE_CHILD_SCOPE_SCHEMA,
        kind: "map_item",
        node_id: "map_items".to_owned(),
        owned_node_id: "code".to_owned(),
        ordinal: 1,
        item_count: 3,
        parent_thread_id: "root".to_owned(),
        child_thread_id: "m1:child-one".to_owned(),
    }
}

fn child_ctx() -> NodeContext {
    let mut state = ctx().state;
    state.insert(
        crate::agents::graph::application::APPLICATION_VISIT_STATE_KEY.to_owned(),
        serde_json::json!("ab".repeat(32)),
    );
    NodeContext::new(state, ExecutionConfig::new("m1:child-one"), 0)
}

struct ChildRun {
    stopped: Result<adk_rust::graph::NodeOutput, GraphError>,
    visit: Option<serde_json::Value>,
}

/// One Code child's journaled attempt on the real remote staging path, its
/// runtime bound to its fan-out owner scope as the compiler binds it.
async fn child_run(
    execution_deadline: tokio::time::Instant,
    slots: &elitea_agent_runtime::graph::fanout::admission::CodeSlots,
    recovering: bool,
) -> ChildRun {
    let pipeline =
        crate::agents::graph::compiler::PipelineDefinition::from_yaml(ADMISSION_YAML).unwrap();
    let definition = pipeline.original_code_definition_fixture("code").unwrap();
    let digest = definition.validated_digest();
    let captured = Arc::new(std::sync::Mutex::new(None));
    let main_rpc = Arc::new(
        crate::transport::InputContentClient::with_original_code_visit_fixture_rpc(ScopedVisitRpc(
            captured.clone(),
        )),
    );
    let runtime = CodeRuntimeFactory::original_visit_admission_fixture(
        main_rpc,
        None,
        execution_deadline,
        slots,
    )
    .bind_saved_child_scope(Arc::new(
        crate::agents::graph::fanout_code_child::FanoutCodeChildScopeProvider::from_scope_for_tests(
            child_scope(),
        ),
    ))
    .unwrap();
    let store = factory();
    let context = child_ctx();
    let policy = NodeRecoveryPolicy::default();
    let activation = NodeAttemptActivation::from_context("code", digest, &context).unwrap();
    assert!(
        activation.visit.is_some(),
        "a Code child's journal is visit-scoped"
    );
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
    let stopped = tokio::time::timeout(std::time::Duration::from_secs(600), node.execute(&context))
        .await
        .expect("the attempt ends");
    let visit = captured.lock().unwrap().take();
    ChildRun { stopped, visit }
}

/// C1 (Worker half): a Code child's Main visit names exactly its owner scope
/// (family, fan-out node, owned Code node, ordinal inside the frozen count,
/// the run's root thread and its own thread), which Main resolves in the
/// visit's transaction; a root visit names none (`null`).
#[tokio::test(start_paused = true)]
async fn a_code_child_visit_names_its_fanout_owner_scope() {
    let slots = elitea_agent_runtime::graph::fanout::admission::CodeSlots::new(1);
    let run = child_run(
        tokio::time::Instant::now() + std::time::Duration::from_secs(3600),
        &slots,
        false,
    )
    .await;
    let visit = run.visit.expect("the child registered its visit");
    assert_eq!(visit["graph_thread"], "m1:child-one");
    assert_eq!(
        visit["saved_child_scope"],
        serde_json::json!({
            "schema": "elitea.runtime.fanout-code-child-scope.v1",
            "kind": "map_item",
            "node_id": "map_items",
            "owned_node_id": "code",
            "ordinal": 1,
            "item_count": 3,
            "parent_thread_id": "root",
            "child_thread_id": "m1:child-one",
        })
    );
    assert_eq!(slots.available(), 1);
}

/// The scope serves only its own child's Code visit.
#[tokio::test]
async fn a_code_child_scope_serves_only_its_own_visit() {
    use crate::agents::pipeline::saved_child_scope_provider::{
        SavedChildPurpose, SavedChildScopeProvider,
    };
    let provider =
        crate::agents::graph::fanout_code_child::FanoutCodeChildScopeProvider::from_scope_for_tests(
            child_scope(),
        );
    assert!(
        provider
            .for_visit("m1:child-one", "code", SavedChildPurpose::CodeRecovery)
            .await
            .is_ok()
    );
    for (thread, node, purpose) in [
        ("m1:child-two", "code", SavedChildPurpose::CodeRecovery),
        ("root", "code", SavedChildPurpose::CodeRecovery),
        ("m1:child-one", "other", SavedChildPurpose::CodeRecovery),
        ("m1:child-one", "code", SavedChildPurpose::CodeDebug),
    ] {
        assert!(
            provider.for_visit(thread, node, purpose).await.is_err(),
            "{thread} {node} {purpose:?}"
        );
    }
}

/// C4: a Code child whose attempt may have started and needs an operator
/// does not pause the run holding a slot: it ends typed in its own visit
/// (never repeated), its slot is free, and another attempt takes it at once.
#[tokio::test(start_paused = true)]
async fn a_code_child_that_needs_an_operator_ends_typed_and_holds_no_slot() {
    let slots = elitea_agent_runtime::graph::fanout::admission::CodeSlots::new(1);
    let run = child_run(
        tokio::time::Instant::now() + std::time::Duration::from_secs(3600),
        &slots,
        true,
    )
    .await;
    let error = run
        .stopped
        .err()
        .expect("no operator pause inside a member");
    assert!(
        error
            .to_string()
            .contains(crate::agents::graph::node_recovery_runtime::CHILD_EFFECT_UNKNOWN_CODE),
        "{error}"
    );
    assert_eq!(slots.available(), 1);
    let before = tokio::time::Instant::now();
    let next = slots
        .admit_fresh(
            before + std::time::Duration::from_secs(3600),
            admission_required(),
        )
        .await
        .expect("the next attempt takes the free slot");
    assert_eq!(tokio::time::Instant::now(), before, "it never waited");
    drop(next);
}
