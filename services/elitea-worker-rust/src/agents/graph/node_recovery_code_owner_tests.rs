//! Actual Code boundary/projector + actual journal transitions. Main authority
//! is an explicit fixture; these checks do not establish `PostgreSQL` or TLS proof.
use super::*;
use crate::agents::graph::{
    code::CodeNodeDefinition,
    code_runtime::{
        CodeAttemptFailure, CodeAttemptPhase, CodeCommittedProjector, CodeInvocation, CodeNode,
        CodeSandboxRuntime,
    },
    node_recovery_owner::*,
    node_recovery_receipt::NodeRecoveryRequiredReceipt,
};
use crate::sandbox::code_recovery::{
    CODE_RESULT_AUDIENCE, CodeRecoveryVisit, WholeCodeBinding, WholeCodeRecoveryReceipt,
    original_job_key,
};
use std::collections::BTreeMap;
struct InterruptedCode(AtomicU64);
#[path = "node_recovery_code_debug_caller_tests.rs"]
mod debug_caller_tests;

struct CaptureOriginalVisit(Mutex<Vec<Vec<u8>>>);
#[async_trait]
impl CodeSandboxRuntime for CaptureOriginalVisit {
    async fn execute(&self, _: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
        panic!("ordinary Code must use its actual Started journal authority")
    }
    async fn execute_attempt(
        &self,
        invocation: CodeInvocation<'_>,
        authority: &NodeAttemptAuthority,
    ) -> Result<Vec<u8>, CodeAttemptFailure> {
        assert!(!authority.recovering_started());
        assert_eq!(invocation.activation, authority.dispatch_activation());
        let job =
            crate::agents::graph::code_remote::original_prepared_job_fixture(&invocation).unwrap();
        let original = invocation
            .original
            .as_ref()
            .expect("actual compiler-owned declaration");
        let declaration = crate::sandbox::code_recovery::OriginalCodeDeclarationInput {
            node_id: &original.node_id,
            graph_thread: &original.graph_thread_id,
            step: original.graph_step,
            configuration_json: &original.configuration_json,
            owning_yaml_sha256: original.yaml,
        };
        let request = crate::transport::input_content::actual_original_code_visit_fixture(
            &declaration,
            authority,
            &job,
        )
        .unwrap();
        self.0.lock().await.push(request);
        Ok(br#"{"revision":1,"status":"completed","exit_code":0,"stdout":"{\"revision\":1,\"result\":41}","stderr":""}"#.to_vec())
    }
}

#[tokio::test]
async fn actual_code_node_all_inputs_preserve_business_roots_in_original_visit_request() {
    use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
    let mut fixtures = Vec::new();
    for selector in ["[]", "[messages]"] {
        let yaml = format!(
            "entry_point: code\nstate:\n  count: int\n  value: int\n  private: str\nnodes:\n  - id: code\n    type: code\n    language: python\n    code: 'return 41'\n    input: {selector}\n    output: [count]\n    transition: END\n"
        );
        let pipeline =
            crate::agents::graph::compiler::PipelineDefinition::from_yaml(&yaml).unwrap();
        let definition = pipeline.original_code_definition_fixture("code").unwrap();
        let digest = definition.validated_digest();
        let runtime = Arc::new(CaptureOriginalVisit(Mutex::new(Vec::new())));
        let factory = factory();
        let mut context = ctx();
        context.state.insert(
            "messages".into(),
            serde_json::json!([{"role":"assistant","content":"must not enter Code"}]),
        );
        context
            .state
            .insert("session_id".into(), serde_json::json!("framework-only"));
        context
            .state
            .insert("undeclared".into(), serde_json::json!("not approved"));
        let stop = NodeRecoveryPolicy::default();
        let body = Arc::new(CodeNode::new(definition, types(), runtime.clone()).unwrap());
        let mut node = RecoverableNode::new(body, digest, stop.clone(), factory.clone(), None)
            .with_legacy_first_attempt();
        node.clock = Arc::new(Clock(AtomicU64::new(100)));
        assert_eq!(node.execute(&context).await.unwrap().updates["count"], 41);
        let requests = runtime.0.lock().await;
        assert_eq!(requests.len(), 1);
        let request: serde_json::Value = serde_json::from_slice(&requests[0]).unwrap();
        let prepared = URL_SAFE_NO_PAD
            .decode(
                request["pre_workspace_prepared_job_json_base64url"]
                    .as_str()
                    .unwrap(),
            )
            .unwrap();
        let prepared_value: serde_json::Value = serde_json::from_slice(&prepared).unwrap();
        assert_eq!(
            prepared_value["input"],
            serde_json::json!({"count":-1,"input":"original user input","private":"preserved","value":7})
        );
        let configuration = URL_SAFE_NO_PAD
            .decode(
                request["exact_configuration_json_base64url"]
                    .as_str()
                    .unwrap(),
            )
            .unwrap();
        let configuration: serde_json::Value = serde_json::from_slice(&configuration).unwrap();
        assert_eq!(
            configuration["input"],
            if selector == "[]" {
                serde_json::json!([])
            } else {
                serde_json::json!(["messages"])
            }
        );
        assert_eq!(
            request["owning_yaml_sha256"],
            crate::sandbox::code_recovery::sha256(yaml.as_bytes())
        );
        assert!(request["saved_child_scope"].is_null());
        let activation = NodeAttemptActivation::from_context("code", digest, &context).unwrap();
        let journal = factory.open(&activation, &stop).await.unwrap();
        assert_eq!(
            request["activation_id"],
            crate::sandbox::code_recovery::hex(&journal.activation_id)
        );
        assert!(matches!(
            journal.load().await.unwrap().ledger.phase(),
            super::super::super::node_recovery::NodeAttemptPhase::Completed { .. }
        ));
        fixtures.push(serde_json::json!({"selector":selector,"owning_yaml":yaml,"request":request,
            "prepared_job_json_base64url":URL_SAFE_NO_PAD.encode(&prepared),"prepared_job_sha256":crate::sandbox::code_recovery::sha256(&prepared)}));
    }
    if let Some(path) = std::env::var_os("ELITEA_CODE_ORIGINAL_VISIT_FIXTURE_OUTPUT") {
        std::fs::write(path, serde_json::to_vec_pretty(&fixtures).unwrap()).unwrap();
    }
}
#[async_trait]
impl CodeSandboxRuntime for InterruptedCode {
    async fn execute(&self, _: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
        panic!("raw Code invocation cannot enter owner recovery")
    }
    async fn execute_attempt(
        &self,
        _: CodeInvocation<'_>,
        authority: &NodeAttemptAuthority,
    ) -> Result<Vec<u8>, CodeAttemptFailure> {
        self.0.fetch_add(1, Ordering::SeqCst);
        Err(CodeAttemptFailure::new(
            CodeAttemptPhase::Observation,
            NodeFailureClass::DependencyUnavailable,
            ReplaySafety::UnknownExternalEffect {
                effect_id: authority.dispatch_activation(),
            },
        ))
    }
}
#[async_trait]
impl NodeRecoveryOwnerAuthorizer for OwnerAuthorizer {
    async fn authorize_verified_no_effect(
        &self,
        _: &NodeRecoveryRequiredReceipt,
        _: &super::super::super::node_recovery::OperatorRetryRequest,
        _: &NodeRecoveryOwnerProof,
    ) -> Result<(), GraphError> {
        self.0
            .then_some(())
            .ok_or_else(|| recovery_error("test.owner_denied"))
    }
    async fn authorize_committed_result(
        &self,
        _: &NodeRecoveryRequiredReceipt,
        _: &super::super::super::node_recovery::OperatorRetryRequest,
        _: &NodeRecoveryOwnerProof,
    ) -> Result<(), GraphError> {
        self.0
            .then_some(())
            .ok_or_else(|| recovery_error("test.owner_denied"))
    }
}
struct OwnerAuthorizer(bool);
fn ctx() -> NodeContext {
    NodeContext::new(
        State::from([
            ("value".into(), serde_json::json!(7)),
            ("input".into(), serde_json::json!("original user input")),
            ("count".into(), serde_json::json!(-1)),
            ("private".into(), serde_json::json!("preserved")),
        ]),
        ExecutionConfig::new("root"),
        2,
    )
}
fn definition() -> CodeNodeDefinition {
    CodeNodeDefinition::from_yaml("id: code\ntype: code\nlanguage: python\ncode: 'return 41'\ninput: [value]\noutput: [count]\ntransition: END\n").unwrap()
}
fn types() -> BTreeMap<String, String> {
    BTreeMap::from([
        ("value".into(), "int".into()),
        ("count".into(), "int".into()),
        ("private".into(), "str".into()),
    ])
}
fn recovery_node(runtime: Arc<InterruptedCode>, factory: Arc<Factory>) -> RecoverableNode {
    recovery_node_with_policy(runtime, factory, policy(), false)
}
fn recovery_node_with_policy(
    runtime: Arc<InterruptedCode>,
    factory: Arc<Factory>,
    policy: NodeRecoveryPolicy,
    legacy: bool,
) -> RecoverableNode {
    let definition = definition();
    let digest = definition.validated_digest();
    let body = Arc::new(CodeNode::new(definition, types(), runtime).unwrap());
    let mut node = RecoverableNode::new(body, digest, policy, factory, None);
    if legacy {
        node = node.with_legacy_first_attempt();
    }
    node.clock = Arc::new(Clock(AtomicU64::new(100)));
    node
}

fn repin_owner_wire(
    proof: &NodeRecoveryOwnerProof,
    value: &serde_json::Value,
) -> (NodeRecoveryOwnerProof, Vec<u8>) {
    let wire = crate::sandbox::code_recovery::canonical(value).unwrap();
    // Exercise semantic binding rejection after a valid reference digest, not merely hash rejection.
    assert!(WholeCodeRecoveryReceipt::parse(&wire).is_ok());
    let mut proof = proof.clone();
    let hash = crate::sandbox::code_recovery::sha256(&wire);
    proof.owner_receipt_sha256 = hash.clone();
    let reference = proof.result_ref.as_mut().unwrap();
    reference.immutable_version = hash.clone();
    reference.digest_sha256 = hash;
    reference.byte_length = wire.len() as u64;
    (proof, wire)
}
#[allow(
    clippy::manual_let_else,
    reason = "Keep explicit typed owner outcomes beside fixture assertions."
)]
fn owner_wire(
    receipt: &NodeRecoveryRequiredReceipt,
    kind: NodeRecoveryOwnerProofKind,
) -> (NodeRecoveryOwnerProof, Vec<u8>) {
    let dispatch = match receipt.replay_safety {
        ReplaySafety::UnknownExternalEffect { effect_id } => effect_id,
        _ => panic!("exact unknown effect"),
    };
    let effect = crate::sandbox::code_recovery::hex(&dispatch);
    let execution = "1".repeat(32);
    let binding = WholeCodeBinding {
        schema: "elitea.sandbox.whole-code-binding.v1".into(),
        purpose: "whole_code_execute".into(),
        execution_id: execution.clone(),
        original_generation: 1,
        dispatch_activation: effect.clone(),
        job_key: original_job_key(&execution, &effect),
        request_digest: "2".repeat(64),
        supervisor_audience: "dns:original.supervisor".into(),
        node_digest: crate::sandbox::code_recovery::hex(&definition().validated_digest()),
        activation_id: receipt.activation_id.clone(),
        node_id: receipt.node_id.clone(),
        graph_thread: receipt.graph_thread.clone(),
        step: receipt.step,
        attempt: receipt.attempt,
        language: "python".into(),
        prepared_job_sha256: "3".repeat(64),
        source_sha256: crate::sandbox::code_recovery::sha256(b"return 41"),
        input_sha256: crate::sandbox::code_recovery::sha256(br#"{"value":7}"#),
    };
    let visit = CodeRecoveryVisit {
        activation_id: receipt.activation_id.clone(),
        node_id: receipt.node_id.clone(),
        graph_thread: receipt.graph_thread.clone(),
        step: receipt.step,
        attempt: receipt.attempt,
        expected_revision: receipt.journal_revision,
        receipt_sha256: crate::sandbox::code_recovery::hex(
            &recovery_receipt_hash(receipt).unwrap(),
        ),
    };
    let owner=match kind {
        NodeRecoveryOwnerProofKind::VerifiedNoEffect=>WholeCodeRecoveryReceipt::sealed_no_effect(binding,visit).unwrap(),
        NodeRecoveryOwnerProofKind::CommittedResult=>WholeCodeRecoveryReceipt::committed(binding,visit,br#"{"revision":1,"status":"completed","exit_code":0,"stdout":"{\"revision\":1,\"result\":41}","stderr":"private diagnostic"}"#).unwrap(),
    };
    let wire = owner.canonical_bytes().unwrap();
    let hash = crate::sandbox::code_recovery::sha256(&wire);
    let reference = NodeRecoveryResultReference {
        content_id: effect.clone(),
        immutable_version: hash.clone(),
        digest_sha256: hash.clone(),
        byte_length: wire.len() as u64,
        media_type: "application/json".into(),
        required_grant_audience: CODE_RESULT_AUDIENCE.into(),
    };
    (
        NodeRecoveryOwnerProof {
            schema: "elitea.pipeline.node-recovery-owner-proof.v1".into(),
            kind,
            execution_id: execution,
            generation: 1,
            activation_id: receipt.activation_id.clone(),
            attempt: receipt.attempt,
            expected_revision: receipt.journal_revision,
            effect_id: effect,
            owner_receipt_sha256: hash,
            result_ref: (kind == NodeRecoveryOwnerProofKind::CommittedResult)
                .then_some(reference.clone()),
            owner_receipt_ref: (kind == NodeRecoveryOwnerProofKind::VerifiedNoEffect)
                .then_some(reference),
        },
        wire,
    )
}
#[tokio::test]
#[allow(
    clippy::too_many_lines,
    reason = "Keep the complete identity and failure assertions in one fixture."
)]
async fn actual_code_owner_result_projects_original_outputs_without_second_execution() {
    let runtime = Arc::new(InterruptedCode(AtomicU64::new(0)));
    let factory = factory();
    let context = ctx();
    recovery_node(runtime.clone(), factory.clone())
        .execute(&context)
        .await
        .unwrap();
    let activation =
        NodeAttemptActivation::from_context("code", definition().validated_digest(), &context)
            .unwrap();
    let journal = factory.open(&activation, &policy()).await.unwrap();
    let before = journal.load().await.unwrap();
    let receipt = NodeRecoveryRequiredReceipt::from_ledger(&activation, &before.ledger).unwrap();
    let (proof, wire) = owner_wire(&receipt, NodeRecoveryOwnerProofKind::CommittedResult);
    let request = super::super::super::node_recovery::OperatorRetryRequest {
        request_id: [8; 32],
        activation_id: [1; 32],
        expected_revision: before.ledger.revision(),
    };
    let projector =
        CodeCommittedProjector::new(definition(), types(), "1".repeat(32), 1, false).unwrap();
    for field in [
        "source_sha256",
        "input_sha256",
        "node_digest",
        "graph_thread",
    ] {
        let mut wrong_binding: serde_json::Value = serde_json::from_slice(&wire).unwrap();
        if field == "graph_thread" {
            wrong_binding["binding"][field] = serde_json::json!("different-original-thread");
            wrong_binding["visit"][field] = serde_json::json!("different-original-thread");
        } else {
            wrong_binding["binding"][field] = serde_json::json!("a".repeat(64));
        }
        let (wrong_proof, wrong_wire) = repin_owner_wire(&proof, &wrong_binding);
        assert!(
            journal
                .resume_owner_result(
                    &activation,
                    &context,
                    request,
                    &wrong_proof,
                    &wrong_wire,
                    &projector,
                    &OwnerAuthorizer(true),
                    101,
                )
                .await
                .is_err(),
            "exact original {field}"
        );
        assert_eq!(journal.load().await.unwrap().ledger, before.ledger);
    }
    let mut wrong = proof.clone();
    wrong.attempt = 2;
    assert!(
        journal
            .resume_owner_result(
                &activation,
                &context,
                request,
                &wrong,
                &wire,
                &projector,
                &OwnerAuthorizer(true),
                101
            )
            .await
            .is_err()
    );
    assert!(
        journal
            .resume_owner_result(
                &activation,
                &context,
                request,
                &proof,
                &wire,
                &projector,
                &OwnerAuthorizer(false),
                101
            )
            .await
            .is_err()
    );
    assert_eq!(journal.load().await.unwrap().ledger, before.ledger);
    let applied = journal
        .resume_owner_result(
            &activation,
            &context,
            request,
            &proof,
            &wire,
            &projector,
            &OwnerAuthorizer(true),
            101,
        )
        .await
        .unwrap();
    let after = journal.load().await.unwrap();
    assert_eq!(after.ledger.history(), before.ledger.history());
    assert_eq!(
        after.updates.as_ref().unwrap(),
        &BTreeMap::from([("count".into(), serde_json::json!(41))])
    );
    assert_eq!(applied.applied_revision(), before.ledger.revision() + 1);
    let output = recovery_node(runtime.clone(), factory)
        .execute(&context)
        .await
        .unwrap();
    assert_eq!(output.updates["count"], 41);
    assert_eq!(runtime.0.load(Ordering::SeqCst), 1);
    assert_eq!(context.state["private"], "preserved");
    assert!(projector.execute(&context).await.is_err());
}

#[tokio::test]
async fn actual_default_stop_owner_result_retains_original_legacy_dispatch_and_logical_visit() {
    let runtime = Arc::new(InterruptedCode(AtomicU64::new(0)));
    let factory = factory();
    let context = ctx();
    let stop = NodeRecoveryPolicy::default();
    recovery_node_with_policy(runtime.clone(), factory.clone(), stop.clone(), true)
        .execute(&context)
        .await
        .unwrap();
    let activation =
        NodeAttemptActivation::from_context("code", definition().validated_digest(), &context)
            .unwrap();
    let journal = factory.open(&activation, &stop).await.unwrap();
    let before = journal.load().await.unwrap();
    let receipt = NodeRecoveryRequiredReceipt::from_ledger(&activation, &before.ledger).unwrap();
    let (proof, wire) = owner_wire(&receipt, NodeRecoveryOwnerProofKind::CommittedResult);
    assert_eq!(
        proof.activation_id,
        crate::sandbox::code_recovery::hex(&journal.activation_id)
    );
    let attempt_authority =
        NodeAttemptAuthority::committed(&activation, journal.activation_id, 1, true);
    assert_ne!(
        proof.effect_id,
        crate::sandbox::code_recovery::hex(&attempt_authority.dispatch_activation())
    );
    let request = super::super::super::node_recovery::OperatorRetryRequest {
        request_id: [8; 32],
        activation_id: [1; 32],
        expected_revision: before.ledger.revision(),
    };
    let wrong =
        CodeCommittedProjector::new(definition(), types(), "1".repeat(32), 1, false).unwrap();
    assert!(
        journal
            .resume_owner_result(
                &activation,
                &context,
                request,
                &proof,
                &wire,
                &wrong,
                &OwnerAuthorizer(true),
                101
            )
            .await
            .is_err()
    );
    assert_eq!(journal.load().await.unwrap().ledger, before.ledger);
    let projector =
        CodeCommittedProjector::new(definition(), types(), "1".repeat(32), 1, true).unwrap();
    journal
        .resume_owner_result(
            &activation,
            &context,
            request,
            &proof,
            &wire,
            &projector,
            &OwnerAuthorizer(true),
            101,
        )
        .await
        .unwrap();
    let restored = recovery_node_with_policy(runtime.clone(), factory, stop, true)
        .execute(&context)
        .await
        .unwrap();
    assert_eq!(
        restored.updates,
        std::collections::HashMap::from([("count".into(), serde_json::json!(41))])
    );
    assert_eq!(context.state["private"], "preserved");
    assert_eq!(runtime.0.load(Ordering::SeqCst), 1);
}
#[tokio::test]
async fn actual_code_owner_tombstone_preserves_history_and_stays_suspended_until_new_retry() {
    let runtime = Arc::new(InterruptedCode(AtomicU64::new(0)));
    let factory = factory();
    let context = ctx();
    recovery_node(runtime.clone(), factory.clone())
        .execute(&context)
        .await
        .unwrap();
    let activation =
        NodeAttemptActivation::from_context("code", definition().validated_digest(), &context)
            .unwrap();
    let journal = factory.open(&activation, &policy()).await.unwrap();
    let before = journal.load().await.unwrap();
    let receipt = NodeRecoveryRequiredReceipt::from_ledger(&activation, &before.ledger).unwrap();
    let (proof, wire) = owner_wire(&receipt, NodeRecoveryOwnerProofKind::VerifiedNoEffect);
    let request = super::super::super::node_recovery::OperatorRetryRequest {
        request_id: [8; 32],
        activation_id: [1; 32],
        expected_revision: before.ledger.revision(),
    };
    let projector =
        CodeCommittedProjector::new(definition(), types(), "1".repeat(32), 1, false).unwrap();
    let mut modified: serde_json::Value = serde_json::from_slice(&wire).unwrap();
    modified["binding"]["source_sha256"] = serde_json::json!("4".repeat(64));
    assert!(
        journal
            .reconcile_verified_no_effect(
                &activation,
                &context,
                request,
                &proof,
                &crate::sandbox::code_recovery::canonical(&modified).unwrap(),
                &projector,
                &OwnerAuthorizer(true),
                101
            )
            .await
            .is_err()
    );
    assert!(
        journal
            .reconcile_verified_no_effect(
                &activation,
                &context,
                request,
                &proof,
                &wire,
                &projector,
                &OwnerAuthorizer(false),
                101
            )
            .await
            .is_err()
    );
    let applied = journal
        .reconcile_verified_no_effect(
            &activation,
            &context,
            request,
            &proof,
            &wire,
            &projector,
            &OwnerAuthorizer(true),
            101,
        )
        .await
        .unwrap();
    let after = journal.load().await.unwrap();
    assert_eq!(after.ledger.history(), before.ledger.history());
    assert!(after.updates.is_none());
    let continuation = applied.continuation_receipt().unwrap();
    assert_eq!(continuation.activation_id, receipt.activation_id);
    assert_eq!(continuation.attempt, receipt.attempt);
    assert_eq!(continuation.journal_revision, receipt.journal_revision + 1);
    assert_eq!(continuation.replay_safety, ReplaySafety::NoExternalEffect);
    let replay = journal
        .reconcile_verified_no_effect(
            &activation,
            &context,
            request,
            &proof,
            &wire,
            &projector,
            &OwnerAuthorizer(true),
            102,
        )
        .await
        .unwrap();
    assert_eq!(replay.applied_revision(), applied.applied_revision());
    assert_eq!(journal.load().await.unwrap().ledger, after.ledger);
    let mut reconciled = recovery_node(runtime.clone(), factory);
    // Recovery must advance the same monotonic fixture clock as reconciliation.
    reconciled.clock = Arc::new(Clock(AtomicU64::new(103)));
    reconciled.execute(&context).await.unwrap();
    assert_eq!(runtime.0.load(Ordering::SeqCst), 1);
    assert_eq!(context.state["count"], -1);
    assert_eq!(context.state["private"], "preserved");
}
