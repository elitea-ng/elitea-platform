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
#[cfg(feature = "sandbox-supervisor")]
#[path = "node_recovery_code_wiring_tests.rs"]
mod wiring_tests;

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

/// CR-WK-P09-hyd: a claim lease lost during Python dependency preparation, then
/// a redelivery. The decision comes from the dispatch ledger, not the error kind.
mod preparation_recovery {
    use super::*;
    use crate::agents::graph::code_remote::attempt_remote::{classify, prior_dispatch_possible};
    use crate::agents::graph::code_runtime::CodePreparationFailure;
    use crate::protocol::elitea::runtime::v1::ExecutionIdentityV1;
    use crate::sandbox::dispatch::{DispatchJournal, DispatchScope};
    use crate::state::postgres_session_tests::IsolatedPostgres;

    /// Runs the production dispatch proof and classification for a preparation
    /// that fails with the same unclassified cause on every attempt.
    struct PreparationLost {
        journal: DispatchJournal,
        scope: DispatchScope,
        calls: AtomicU64,
    }
    #[async_trait]
    impl CodeSandboxRuntime for PreparationLost {
        async fn execute(&self, _: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
            panic!("raw Code invocation cannot enter preparation recovery")
        }
        async fn execute_attempt(
            &self,
            _: CodeInvocation<'_>,
            authority: &NodeAttemptAuthority,
        ) -> Result<Vec<u8>, CodeAttemptFailure> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            let possible = prior_dispatch_possible(
                &self.journal,
                Some(&self.scope),
                &authority.dispatch_activation(),
                authority.recovering_started(),
            )
            .await;
            Err(classify(
                CodeAttemptPhase::Preparation,
                NodeFailureClass::Unknown,
                authority.dispatch_activation(),
                possible,
            )
            .with_preparation_failure(CodePreparationFailure::Failed))
        }
    }

    struct Fixture {
        _database: IsolatedPostgres,
        runtime: Arc<PreparationLost>,
        factory: Arc<Factory>,
        context: NodeContext,
        dispatch_activation: [u8; 32],
    }

    async fn fixture(register_execute_dispatch: bool) -> Fixture {
        let url = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL")
            .expect("ELITEA_TEST_DATABASE_URL is required");
        let database = IsolatedPostgres::create(&url).await;
        sqlx::raw_sql("CREATE SCHEMA elitea_runtime")
            .execute(&database.pool)
            .await
            .unwrap();
        for migration in [
            include_str!(
                "../../../../elitea-main/migrations/agentstate/0007_sandbox_dispatch_journal.sql"
            ),
            include_str!(
                "../../../../elitea-main/migrations/agentstate/0014_sandbox_dispatch_submission_marker.sql"
            ),
        ] {
            sqlx::raw_sql(migration)
                .execute(&database.pool)
                .await
                .unwrap();
        }
        let scope = DispatchScope::from_identity(&ExecutionIdentityV1 {
            tenant_id: "dispatch-test".into(),
            resource_project_id: "2".into(),
            execution_id: "execution-a".into(),
            generation: 1,
            ..Default::default()
        })
        .unwrap();
        let factory = factory();
        let context = ctx();
        let definition = definition();
        let activation =
            NodeAttemptActivation::from_context("code", definition.validated_digest(), &context)
                .unwrap();
        let journal = factory.open(&activation, &policy()).await.unwrap();
        // The crash window: Started is durable, no outcome was ever recorded.
        let snapshot = journal.load().await.unwrap();
        let started = snapshot.ledger.start_attempt(100).unwrap();
        journal.append(&snapshot, started, None).await.unwrap();
        let dispatch_activation =
            NodeAttemptAuthority::committed(&activation, journal.activation_id, 1, true)
                .dispatch_activation();
        let dispatch = DispatchJournal::new(database.pool.clone());
        if register_execute_dispatch {
            // The earlier attempt reached the execute submission.
            dispatch
                .register(&scope, &dispatch_activation, &[2; 32], "dns:sandbox-a")
                .await
                .unwrap();
        }
        Fixture {
            _database: database,
            runtime: Arc::new(PreparationLost {
                journal: dispatch,
                scope,
                calls: AtomicU64::new(0),
            }),
            factory,
            context,
            dispatch_activation,
        }
    }

    fn node(
        fixture: &Fixture,
        sender: crate::agents::graph::node_events::PipelineNodeEventSender,
    ) -> RecoverableNode {
        let definition = definition();
        let digest = definition.validated_digest();
        let body = Arc::new(
            CodeNode::new(definition, types(), fixture.runtime.clone())
                .unwrap()
                .with_events(Some(sender)),
        );
        let mut node = RecoverableNode::new(body, digest, policy(), fixture.factory.clone(), None);
        node.clock = Arc::new(Clock(AtomicU64::new(200)));
        node
    }

    #[tokio::test]
    #[ignore = "requires disposable PostgreSQL"]
    async fn preparation_failure_in_recovery_without_a_dispatch_row_settles_typed_once() {
        let fixture = fixture(false).await;
        let (sender, receiver) = crate::agents::graph::node_events::pipeline_node_event_channel();
        let mut drain = receiver.drain("inv", "agent", "root").await.unwrap();
        let node = node(&fixture, sender);
        let error = node.execute(&fixture.context).await.err().unwrap();
        assert!(error.to_string().contains("pipeline.node_recovery.failed"));
        // One typed, readable settlement for this failure.
        let event = drain.try_recv().unwrap().unwrap_err();
        assert_eq!(event.code, "pipeline.code_preparation_failed");
        assert!(drain.try_recv().is_none());
        assert_eq!(fixture.runtime.calls.load(Ordering::SeqCst), 1);
        let activation = NodeAttemptActivation::from_context(
            "code",
            definition().validated_digest(),
            &fixture.context,
        )
        .unwrap();
        let journal = fixture.factory.open(&activation, &policy()).await.unwrap();
        let stored = journal.load().await.unwrap();
        assert!(matches!(
            stored.ledger.phase(),
            NodeAttemptPhase::Failed(RecoveryDecision::Stop(_))
        ));
        let failure = stored.ledger.effective_failure().unwrap();
        assert_eq!(failure.class, NodeFailureClass::Unknown);
        // The user's code never started, so the failure carries no effect doubt.
        assert_eq!(failure.replay, ReplaySafety::NoExternalEffect);
        // A further redelivery runs nothing and changes nothing.
        let revision = stored.ledger.revision();
        assert!(node.execute(&fixture.context).await.is_err());
        assert_eq!(fixture.runtime.calls.load(Ordering::SeqCst), 1);
        assert_eq!(journal.load().await.unwrap().ledger.revision(), revision);
    }

    #[tokio::test]
    #[ignore = "requires disposable PostgreSQL"]
    async fn preparation_failure_in_recovery_with_a_dispatch_row_stays_reconcile_with_an_accepted_receipt()
     {
        let fixture = fixture(true).await;
        let (sender, receiver) = crate::agents::graph::node_events::pipeline_node_event_channel();
        let mut drain = receiver.drain("inv", "agent", "root").await.unwrap();
        let node = node(&fixture, sender);
        let output = node.execute(&fixture.context).await.unwrap();
        // The pause is the operator card, never a silent failure or a repeat.
        let interrupt = output.interrupt.expect("effect reconciliation pause");
        assert!(drain.try_recv().is_none());
        assert_eq!(fixture.runtime.calls.load(Ordering::SeqCst), 1);
        let activation = NodeAttemptActivation::from_context(
            "code",
            definition().validated_digest(),
            &fixture.context,
        )
        .unwrap();
        let journal = fixture.factory.open(&activation, &policy()).await.unwrap();
        let stored = journal.load().await.unwrap();
        let NodeAttemptPhase::Failed(RecoveryDecision::Reconcile { effect_id, .. }) =
            stored.ledger.phase()
        else {
            panic!("a possibly dispatched execution must reconcile");
        };
        assert_eq!(effect_id, Some(fixture.dispatch_activation));
        let receipt =
            NodeRecoveryRequiredReceipt::from_ledger(&activation, &stored.ledger).unwrap();
        assert!(receipt.validate());
        let adk_rust::graph::interrupt::Interrupt::Dynamic {
            data: Some(data), ..
        } = interrupt
        else {
            panic!("dynamic recovery interrupt");
        };
        assert_eq!(data["receipt"], serde_json::to_value(&receipt).unwrap());
        // The receipt has exactly the shape of the golden vector Main accepts.
        let mut shaped = serde_json::to_value(&receipt).unwrap();
        let golden: serde_json::Value = serde_json::from_slice(
            &std::fs::read(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/../../libs/jsonschema/runtime/v1/fixtures/",
                "node-recovery-required-v1.reconcile-unknown-class.json"
            ))
            .unwrap(),
        )
        .unwrap();
        for key in [
            "activation_id",
            "journal_revision",
            "node_id",
            "graph_thread",
            "step",
        ] {
            shaped[key] = golden[key].clone();
        }
        shaped["replay_safety"]["effect_id"] = golden["replay_safety"]["effect_id"].clone();
        assert_eq!(shaped, golden);
    }

    #[tokio::test]
    async fn dispatch_proof_is_conservative_without_a_ledger_answer() {
        let lazy = crate::test_pg::pool_options()
            .acquire_timeout(std::time::Duration::from_millis(200))
            .connect_lazy("postgres://nobody@127.0.0.1:1/none")
            .unwrap();
        let journal = DispatchJournal::new(lazy);
        let scope = DispatchScope::from_identity(&ExecutionIdentityV1 {
            tenant_id: "t".into(),
            resource_project_id: "2".into(),
            execution_id: "e".into(),
            generation: 1,
            ..Default::default()
        })
        .unwrap();
        // A first attempt needs no ledger answer.
        assert!(!prior_dispatch_possible(&journal, Some(&scope), &[1; 32], false).await);
        // A recovered attempt without a scope or an answer may have dispatched.
        assert!(prior_dispatch_possible(&journal, None, &[1; 32], true).await);
        assert!(prior_dispatch_possible(&journal, Some(&scope), &[1; 32], true).await);
    }
}

/// A runtime that reached a terminal `failed` phase is a known outcome. Only an
/// unreceipted platform-client effect leaves it unknown (CR-SK-P09-hyd H).
mod terminal_runtime_failure {
    use super::*;
    use crate::agents::graph::code_remote::attempt_remote::{classify, platform_effects_possible};
    use crate::transport::InputContentError;
    use crate::transport::input_content::code_platform_content::CodePlatformEffects;

    struct FailedRuntime {
        effect_possible: bool,
        calls: AtomicU64,
    }
    #[async_trait]
    impl CodeSandboxRuntime for FailedRuntime {
        async fn execute(&self, _: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
            panic!("typed attempt only")
        }
        async fn execute_attempt(
            &self,
            _: CodeInvocation<'_>,
            authority: &NodeAttemptAuthority,
        ) -> Result<Vec<u8>, CodeAttemptFailure> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            Err(classify(
                CodeAttemptPhase::Observation,
                NodeFailureClass::InvalidResult,
                authority.dispatch_activation(),
                self.effect_possible,
            ))
        }
    }

    fn run(
        effect_possible: bool,
        sender: crate::agents::graph::node_events::PipelineNodeEventSender,
    ) -> (RecoverableNode, NodeContext, Arc<FailedRuntime>) {
        let runtime = Arc::new(FailedRuntime {
            effect_possible,
            calls: AtomicU64::new(0),
        });
        let definition = definition();
        let digest = definition.validated_digest();
        let body = Arc::new(
            CodeNode::new(definition, types(), runtime.clone())
                .unwrap()
                .with_events(Some(sender)),
        );
        let mut node = RecoverableNode::new(body, digest, policy(), factory(), None);
        node.clock = Arc::new(Clock(AtomicU64::new(100)));
        (node, ctx(), runtime)
    }

    #[tokio::test]
    async fn failed_runtime_without_a_possible_effect_settles_typed_and_needs_no_operator() {
        let (sender, receiver) = crate::agents::graph::node_events::pipeline_node_event_channel();
        let mut drain = receiver.drain("inv", "agent", "root").await.unwrap();
        let (node, context, runtime) = run(false, sender);
        let error = node.execute(&context).await.err().unwrap();
        assert!(error.to_string().contains("pipeline.node_recovery.failed"));
        assert_eq!(runtime.calls.load(Ordering::SeqCst), 1);
        // One readable, typed settlement; no operator card.
        let event = drain.try_recv().unwrap().unwrap_err();
        assert_eq!(event.code, "pipeline.code_failed");
        assert!(drain.try_recv().is_none());
    }

    #[tokio::test]
    async fn failed_runtime_with_an_unreceipted_platform_effect_still_reconciles() {
        let (sender, receiver) = crate::agents::graph::node_events::pipeline_node_event_channel();
        let mut drain = receiver.drain("inv", "agent", "root").await.unwrap();
        let (node, context, runtime) = run(true, sender);
        let output = node.execute(&context).await.unwrap();
        assert!(output.interrupt.is_some());
        assert!(drain.try_recv().is_none());
        assert_eq!(runtime.calls.load(Ordering::SeqCst), 1);
    }

    /// Main's journal answers "nothing unresolved" or the effect stays possible.
    #[tokio::test(start_paused = true)]
    async fn platform_journal_proof_clears_the_effect_only_on_an_explicit_none() {
        // The only answer that proves a known outcome.
        assert!(!platform_effects_possible(|| async { Ok(CodePlatformEffects::None) }).await);
        // A call dispatched or uncertain in the journal, a refusal of the query
        // and an unusable answer all keep the effect possible.
        assert!(platform_effects_possible(|| async { Ok(CodePlatformEffects::Possible) }).await);
        assert!(
            platform_effects_possible(|| async {
                Err(InputContentError::AuthorizationFailed("refused"))
            })
            .await
        );
        // Main briefly unavailable is retried, not a verdict.
        let mut calls = 0;
        let recovered = platform_effects_possible(|| {
            calls += 1;
            let attempt = calls;
            async move {
                if attempt < 3 {
                    Err(InputContentError::DependencyUnavailable("main restarting"))
                } else {
                    Ok(CodePlatformEffects::None)
                }
            }
        })
        .await;
        assert!(!recovered);
        assert_eq!(calls, 3);
        // Main down for the whole budget never proves the outcome.
        let started = tokio::time::Instant::now();
        assert!(
            platform_effects_possible(|| async {
                Err(InputContentError::DependencyUnavailable("main down"))
            })
            .await
        );
        assert!(tokio::time::Instant::now() - started >= std::time::Duration::from_secs(30));
    }
}
