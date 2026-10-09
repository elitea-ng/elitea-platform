use super::super::node_recovery::{NodeBackoff, ReplaySafety, RetryCondition};
use super::*;
use adk_rust::graph::checkpoint::RetentionPolicy;
use adk_rust::graph::{Checkpointer, ExecutionConfig, MemoryCheckpointer};
use std::collections::{BTreeSet, VecDeque};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use tokio::sync::Mutex;

#[path = "node_recovery_code_owner_tests.rs"]
mod code_owner_tests;
#[path = "node_recovery_direct_tool_tests.rs"]
pub(in crate::agents::graph) mod direct_tool_tests;

struct Lease(AtomicBool);
impl StateWriterLease for Lease {
    fn ensure_current(&self) -> Result<(), crate::state::StateWriterLeaseLost> {
        self.0
            .load(Ordering::SeqCst)
            .then_some(())
            .ok_or(crate::state::StateWriterLeaseLost)
    }
}

#[derive(Default)]
struct Store {
    inner: MemoryCheckpointer,
    append: Mutex<()>,
    reject_append: AtomicBool,
    revoke_after_append: Mutex<Option<Arc<Lease>>>,
}
#[async_trait]
impl Checkpointer for Store {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        self.inner.save(checkpoint).await
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.inner.load(thread).await
    }
    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.inner.load_by_id(id).await
    }
    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.inner.list(thread).await
    }
    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.inner.delete(thread).await
    }
    async fn prune(&self, thread: &str, policy: &RetentionPolicy) -> Result<usize, GraphError> {
        self.inner.prune(thread, policy).await
    }
}
#[async_trait]
impl ParallelCheckpointAppender for Store {
    async fn append_after(
        &self,
        expected: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        let _guard = self.append.lock().await;
        if self.reject_append.load(Ordering::SeqCst) {
            return Err(recovery_error("test.append_failed"));
        }
        let current = self.load(&candidate.thread_id).await?;
        if current.as_ref().map(|value| &value.checkpoint_id)
            != expected.map(|value| &value.checkpoint_id)
        {
            return Err(recovery_error("test.append_conflict"));
        }
        let saved = self.save(candidate).await?;
        if let Some(lease) = self.revoke_after_append.lock().await.take() {
            lease.0.store(false, Ordering::SeqCst);
        }
        Ok(saved)
    }
}

struct Factory {
    store: Arc<Store>,
    lease: Arc<Lease>,
}
#[async_trait]
impl NodeRecoveryFactory for Factory {
    async fn open(
        &self,
        _: &NodeAttemptActivation,
        policy: &NodeRecoveryPolicy,
    ) -> Result<NodeAttemptJournal, GraphError> {
        Ok(NodeAttemptJournal::bound(
            self.store.clone(),
            self.lease.clone(),
            "node-journal".to_owned(),
            [1; 32],
            policy.clone(),
        ))
    }
}

struct Clock(AtomicU64);
#[async_trait]
impl NodeRecoveryClock for Clock {
    fn now_ms(&self) -> Result<u64, GraphError> {
        Ok(self.0.load(Ordering::SeqCst))
    }
    async fn wait_until(&self, target: u64) -> Result<(), GraphError> {
        self.0.store(target, Ordering::SeqCst);
        Ok(())
    }
}

struct Body {
    outcomes: Mutex<VecDeque<Result<u64, NodeFailure>>>,
    calls: Mutex<Vec<([u8; 32], bool)>>,
}
#[async_trait]
impl Node for Body {
    #[allow(
        clippy::unnecessary_literal_bound,
        reason = "Match the existing runtime trait signature in this fixture."
    )]
    fn name(&self) -> &str {
        "code"
    }
    async fn execute(&self, _: &NodeContext) -> Result<NodeOutput, GraphError> {
        Err(recovery_error("test.raw_execute_forbidden"))
    }
}
#[async_trait]
impl NodeAttemptBody for Body {
    fn legacy_dispatch_identity(&self, _: &NodeContext) -> Result<Option<[u8; 32]>, GraphError> {
        Ok(Some([9; 32]))
    }
    async fn execute_attempt(
        &self,
        _: &NodeContext,
        authority: &NodeAttemptAuthority,
    ) -> Result<NodeOutput, NodeFailure> {
        self.calls.lock().await.push((
            authority.dispatch_activation(),
            authority.recovering_started(),
        ));
        self.outcomes
            .lock()
            .await
            .pop_front()
            .expect("scripted outcome")
            .map(|value| NodeOutput::new().with_update("result", serde_json::json!(value)))
    }
}

fn policy() -> NodeRecoveryPolicy {
    NodeRecoveryPolicy::admit(
        3,
        BTreeSet::from([RetryCondition::DependencyUnavailable]),
        Some(NodeBackoff {
            initial_ms: 10,
            maximum_ms: 50,
            multiplier: 2,
        }),
        Some(1000),
        None,
    )
    .unwrap()
}
fn context() -> NodeContext {
    NodeContext::new(
        State::from([("input".to_owned(), serde_json::json!(7))]),
        ExecutionConfig::new("root"),
        2,
    )
}
fn factory() -> Arc<Factory> {
    Arc::new(Factory {
        store: Arc::new(Store::default()),
        lease: Arc::new(Lease(AtomicBool::new(true))),
    })
}
fn wrapper(body: Arc<Body>, factory: Arc<Factory>, policy: NodeRecoveryPolicy) -> RecoverableNode {
    let mut node = RecoverableNode::new(body, [2; 32], policy, factory, None);
    node.clock = Arc::new(Clock(AtomicU64::new(100)));
    node
}
fn body(values: Vec<Result<u64, NodeFailure>>) -> Arc<Body> {
    Arc::new(Body {
        outcomes: Mutex::new(values.into()),
        calls: Mutex::new(Vec::new()),
    })
}

#[tokio::test]
async fn node_recovery_runtime_commits_started_before_dispatch_and_completion_before_return() {
    let factory = factory();
    let body = body(vec![Ok(42)]);
    let node = wrapper(body.clone(), factory.clone(), policy());
    let output = node.execute(&context()).await.unwrap();
    assert_eq!(output.updates["result"], serde_json::json!(42));
    let stored = factory
        .open(
            &NodeAttemptActivation::from_context("code", [2; 32], &context()).unwrap(),
            &policy(),
        )
        .await
        .unwrap()
        .load()
        .await
        .unwrap();
    assert!(matches!(
        stored.ledger.phase(),
        NodeAttemptPhase::Completed { .. }
    ));
    let recovered = wrapper(body.clone(), factory, policy())
        .execute(&context())
        .await
        .unwrap();
    assert_eq!(recovered.updates, output.updates);
    assert_eq!(body.calls.lock().await.len(), 1);
}

#[tokio::test]
async fn node_recovery_runtime_failed_started_append_dispatches_nothing() {
    let factory = factory();
    factory.store.reject_append.store(true, Ordering::SeqCst);
    let body = body(vec![Ok(42)]);
    assert!(
        wrapper(body.clone(), factory, policy())
            .execute(&context())
            .await
            .is_err()
    );
    assert!(body.calls.lock().await.is_empty());
}

#[tokio::test]
async fn node_recovery_runtime_retry_has_distinct_attempt_identity_and_exact_history() {
    let factory = factory();
    let body = body(vec![
        Err(NodeFailure {
            class: NodeFailureClass::DependencyUnavailable,
            replay: ReplaySafety::NoExternalEffect,
        }),
        Ok(42),
    ]);
    wrapper(body.clone(), factory.clone(), policy())
        .execute(&context())
        .await
        .unwrap();
    let calls = body.calls.lock().await;
    assert_eq!(calls.len(), 2);
    assert_ne!(calls[0].0, calls[1].0);
    assert!(!calls[0].1 && !calls[1].1);
    let stored = factory
        .open(
            &NodeAttemptActivation::from_context("code", [2; 32], &context()).unwrap(),
            &policy(),
        )
        .await
        .unwrap()
        .load()
        .await
        .unwrap();
    assert_eq!(stored.ledger.history().len(), 2);
    assert_eq!(stored.ledger.history()[1].started_ms, 110);
}

#[tokio::test]
async fn node_recovery_runtime_resumed_started_reconciles_same_attempt_identity() {
    let factory = factory();
    let activation = NodeAttemptActivation::from_context("code", [2; 32], &context()).unwrap();
    let journal = factory.open(&activation, &policy()).await.unwrap();
    let initial = journal.load().await.unwrap();
    journal
        .append(&initial, initial.ledger.start_attempt(90).unwrap(), None)
        .await
        .unwrap();
    let body = body(vec![Ok(42)]);
    wrapper(body.clone(), factory, policy())
        .execute(&context())
        .await
        .unwrap();
    let calls = body.calls.lock().await;
    assert_eq!(
        calls.as_slice(),
        &[(
            NodeAttemptAuthority::committed(&activation, [1; 32], 1, true).dispatch_activation(),
            true
        )]
    );
}

#[tokio::test]
async fn node_recovery_runtime_denial_and_unknown_effect_commit_failure_without_updates() {
    for failure in [
        NodeFailure {
            class: NodeFailureClass::AuthorizationDenied,
            replay: ReplaySafety::NoExternalEffect,
        },
        NodeFailure {
            class: NodeFailureClass::Cancelled,
            replay: ReplaySafety::NoExternalEffect,
        },
        NodeFailure {
            class: NodeFailureClass::AttemptTimeout,
            replay: ReplaySafety::UnknownExternalEffect { effect_id: [7; 32] },
        },
    ] {
        let factory = factory();
        let body = body(vec![Err(failure)]);
        let outcome = wrapper(body.clone(), factory.clone(), policy())
            .execute(&context())
            .await;
        if matches!(failure.replay, ReplaySafety::UnknownExternalEffect { .. }) {
            assert!(outcome.unwrap().interrupt.is_some());
        } else {
            assert!(outcome.is_err());
        }
        assert_eq!(body.calls.lock().await.len(), 1);
        let snapshot = factory
            .open(
                &NodeAttemptActivation::from_context("code", [2; 32], &context()).unwrap(),
                &policy(),
            )
            .await
            .unwrap()
            .load()
            .await
            .unwrap();
        assert!(snapshot.updates.is_none());
        assert_eq!(snapshot.ledger.history().len(), 1);
    }
}

#[tokio::test]
async fn node_recovery_runtime_revoked_writer_dispatches_nothing() {
    let factory = factory();
    factory.lease.0.store(false, Ordering::SeqCst);
    let body = body(vec![Ok(42)]);
    assert!(
        wrapper(body.clone(), factory, policy())
            .execute(&context())
            .await
            .is_err()
    );
    assert!(body.calls.lock().await.is_empty());
}

struct Authorizer(bool);
#[async_trait]
impl NodeRecoveryOperatorAuthorizer for Authorizer {
    async fn authorize_retry(
        &self,
        _: &super::super::node_recovery_receipt::NodeRecoveryRequiredReceipt,
        _: &super::super::node_recovery::OperatorRetryRequest,
    ) -> Result<(), GraphError> {
        if self.0 {
            Ok(())
        } else {
            Err(recovery_error("test.operator_denied"))
        }
    }
}

#[tokio::test]
async fn node_recovery_runtime_operator_mode_resumes_same_visit_after_authorized_cas_without_skipping_backoff()
 {
    use super::super::node_recovery::{NodeRetryMode, OperatorRetryRequest};
    let policy = policy().with_retry_mode(NodeRetryMode::Operator).unwrap();
    let factory = factory();
    let body = body(vec![
        Err(NodeFailure::new(
            NodeFailureClass::DependencyUnavailable,
            ReplaySafety::NoExternalEffect,
        )),
        Ok(42),
    ]);
    let first = wrapper(body.clone(), factory.clone(), policy.clone())
        .execute(&context())
        .await
        .unwrap();
    assert!(first.updates.is_empty());
    assert!(first.interrupt.is_some());
    assert_eq!(body.calls.lock().await.len(), 1);
    let activation = NodeAttemptActivation::from_context("code", [2; 32], &context()).unwrap();
    let journal = factory.open(&activation, &policy).await.unwrap();
    let before = journal.load().await.unwrap();
    let request = OperatorRetryRequest {
        request_id: [9; 32],
        activation_id: [1; 32],
        expected_revision: before.ledger.revision(),
    };
    assert!(
        journal
            .resume_operator_retry(&activation, request, &Authorizer(false), 105)
            .await
            .is_err()
    );
    assert_eq!(journal.load().await.unwrap().ledger, before.ledger);
    assert!(
        journal
            .resume_operator_retry(
                &activation,
                OperatorRetryRequest {
                    expected_revision: 1,
                    ..request
                },
                &Authorizer(true),
                105
            )
            .await
            .is_err()
    );
    journal
        .resume_operator_retry(&activation, request, &Authorizer(true), 105)
        .await
        .unwrap();
    let after = journal.load().await.unwrap();
    assert_eq!(after.ledger.revision(), before.ledger.revision() + 1);
    journal
        .resume_operator_retry(&activation, request, &Authorizer(true), 106)
        .await
        .unwrap();
    assert_eq!(journal.load().await.unwrap().ledger, after.ledger);
    let mut resumed = wrapper(body.clone(), factory, policy);
    resumed.clock = Arc::new(Clock(AtomicU64::new(105)));
    assert_eq!(
        resumed.execute(&context()).await.unwrap().updates["result"],
        serde_json::json!(42)
    );
    let complete = journal.load().await.unwrap();
    assert_eq!(complete.ledger.history()[1].started_ms, 110);
    assert_eq!(complete.ledger.operator_audit().len(), 1);
    assert_eq!(body.calls.lock().await.len(), 2);
}

#[tokio::test]
async fn node_recovery_runtime_late_backoff_wake_commits_expiry_without_new_dispatch() {
    let factory = factory();
    let activation = NodeAttemptActivation::from_context("code", [2; 32], &context()).unwrap();
    let journal = factory.open(&activation, &policy()).await.unwrap();
    let fresh = journal.load().await.unwrap();
    let started = fresh.ledger.start_attempt(90).unwrap();
    let first = journal.append(&fresh, started, None).await.unwrap();
    let failed = first
        .ledger
        .record_failure(
            NodeFailure::new(
                NodeFailureClass::DependencyUnavailable,
                ReplaySafety::NoExternalEffect,
            ),
            91,
        )
        .unwrap();
    let saved = journal.append(&first, failed, None).await.unwrap();
    let body = body(vec![Ok(42)]);
    let mut node = wrapper(body.clone(), factory, policy());
    node.clock = Arc::new(Clock(AtomicU64::new(2000)));
    assert!(node.execute(&context()).await.is_err());
    assert!(body.calls.lock().await.is_empty());
    let late = journal.load().await.unwrap();
    assert_eq!(late.ledger.history(), saved.ledger.history());
    assert_eq!(
        late.ledger.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::Stop(
            super::super::node_recovery::StopReason::ElapsedLimit
        ))
    );
}

#[tokio::test]
async fn node_recovery_runtime_failure_route_emits_only_dedicated_typed_error() {
    use super::super::node_recovery::{ErrorRouteContract, NodeErrorRoute};
    let route = NodeErrorRoute::admit(
        [8; 32],
        BTreeSet::from([NodeFailureClass::InvalidInput]),
        ErrorRouteContract {
            dedicated_typed_error_input: true,
            requires_success_output: false,
            reexecutes_failed_operation: false,
            explicitly_handles_denial: false,
        },
    )
    .unwrap();
    let policy = NodeRecoveryPolicy::admit(1, BTreeSet::new(), None, None, Some(route)).unwrap();
    let factory = factory();
    let body = body(vec![Err(NodeFailure::new(
        NodeFailureClass::InvalidInput,
        ReplaySafety::NoExternalEffect,
    ))]);
    let mut node = wrapper(body.clone(), factory, policy);
    node.error_route = Some(("handler".into(), "node_error".into()));
    let output = node.execute(&context()).await.unwrap();
    assert_eq!(output.updates.len(), 1);
    assert_eq!(
        output.updates["node_error"]["class"],
        serde_json::json!("invalid_input")
    );
    assert_eq!(output.goto, Some(vec!["handler".into()]));
    assert!(output.interrupt.is_none());
}

#[tokio::test]
async fn node_recovery_runtime_ambiguous_effect_suspends_with_reconcile_only_and_never_dispatches_again()
 {
    let factory = factory();
    let body = body(vec![Err(NodeFailure::new(
        NodeFailureClass::AttemptTimeout,
        ReplaySafety::UnknownExternalEffect { effect_id: [7; 32] },
    ))]);
    let first = wrapper(body.clone(), factory.clone(), policy())
        .execute(&context())
        .await
        .unwrap();
    assert!(first.updates.is_empty());
    assert!(first.interrupt.is_some());
    let second = wrapper(body.clone(), factory.clone(), policy())
        .execute(&context())
        .await
        .unwrap();
    assert!(second.updates.is_empty());
    assert_eq!(body.calls.lock().await.len(), 1);
    let Some(adk_rust::graph::interrupt::Interrupt::Dynamic {
        data: Some(data), ..
    }) = first.interrupt
    else {
        panic!("recovery receipt");
    };
    let wire = data["receipt"].clone();
    assert_eq!(wire["allowed_actions"], serde_json::json!(["reconcile"]));
    assert_eq!(
        wire["replay_safety"]["effect_id"],
        serde_json::json!("07".repeat(32))
    );
    assert_eq!(wire["failure_class"], serde_json::json!("attempt_timeout"));
}

#[tokio::test]
async fn node_recovery_default_stop_preserves_legacy_effect_identity_and_reuses_result() {
    let factory = factory();
    let body = body(vec![Ok(42)]);
    let node = wrapper(body.clone(), factory.clone(), NodeRecoveryPolicy::default())
        .with_legacy_first_attempt();
    let output = node.execute(&context()).await.unwrap();
    assert_eq!(body.calls.lock().await.as_slice(), &[([9; 32], false)]);
    let recovered = wrapper(body.clone(), factory, NodeRecoveryPolicy::default())
        .with_legacy_first_attempt()
        .execute(&context())
        .await
        .unwrap();
    assert_eq!(output.updates, recovered.updates);
    assert_eq!(body.calls.lock().await.len(), 1);
}

#[tokio::test]
async fn node_recovery_operator_retry_expiry_commits_terminal_stop_without_new_attempt() {
    use super::super::node_recovery::{NodeRetryMode, OperatorRetryRequest, StopReason};
    struct Allow;
    #[async_trait]
    impl NodeRecoveryOperatorAuthorizer for Allow {
        async fn authorize_retry(
            &self,
            _: &super::super::node_recovery_receipt::NodeRecoveryRequiredReceipt,
            _: &OperatorRetryRequest,
        ) -> Result<(), GraphError> {
            Ok(())
        }
    }
    let factory = factory();
    let body = body(vec![Err(NodeFailure::new(
        NodeFailureClass::DependencyUnavailable,
        ReplaySafety::NoExternalEffect,
    ))]);
    let policy = policy().with_retry_mode(NodeRetryMode::Operator).unwrap();
    wrapper(body.clone(), factory.clone(), policy.clone())
        .execute(&context())
        .await
        .unwrap();
    let activation = NodeAttemptActivation::from_context("code", [2; 32], &context()).unwrap();
    let journal = factory.open(&activation, &policy).await.unwrap();
    let before = journal.load().await.unwrap();
    let request = OperatorRetryRequest {
        request_id: [8; 32],
        activation_id: [1; 32],
        expected_revision: before.ledger.revision(),
    };
    let proof = journal
        .resume_operator_retry(&activation, request, &Allow, 1100)
        .await
        .unwrap();
    assert_eq!(proof.terminal_stop_reason(), Some(StopReason::ElapsedLimit));
    assert!(proof.matches(request));
    let after = journal.load().await.unwrap();
    assert_eq!(after.ledger.history(), before.ledger.history());
    assert_eq!(
        after.ledger.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::Stop(StopReason::ElapsedLimit))
    );
    let replay = journal
        .resume_operator_retry(&activation, request, &Allow, 1101)
        .await
        .unwrap();
    assert_eq!(replay.applied_revision(), proof.applied_revision());
    assert_eq!(body.calls.lock().await.len(), 1);
}

struct OwnerAuthorizer(bool);
#[async_trait]
impl NodeRecoveryOwnerAuthorizer for OwnerAuthorizer {
    async fn authorize_committed_result(
        &self,
        _: &super::super::node_recovery_receipt::NodeRecoveryRequiredReceipt,
        _: &super::super::node_recovery::OperatorRetryRequest,
        _: &super::super::node_recovery_owner::NodeRecoveryOwnerProof,
    ) -> Result<(), GraphError> {
        if self.0 {
            Ok(())
        } else {
            Err(recovery_error("test.owner_denied"))
        }
    }
}
struct ResultProjector;
#[async_trait]
impl Node for ResultProjector {
    #[allow(
        clippy::unnecessary_literal_bound,
        reason = "Match the existing runtime trait signature in this fixture."
    )]
    fn name(&self) -> &str {
        "code"
    }
    async fn execute(&self, _: &NodeContext) -> Result<NodeOutput, GraphError> {
        Err(recovery_error("test.result_dispatch_forbidden"))
    }
}
impl NodeResultRecovery for ResultProjector {
    fn project_committed_result(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
        proof: &super::super::node_recovery_owner::NodeRecoveryOwnerProof,
        wire: &[u8],
    ) -> Result<NodeOutput, NodeFailure> {
        if !authority.matches("code", [2; 32], context.step)
            || authority.attempt() != proof.attempt
            || !proof.matches_result_bytes(wire)
        {
            return Err(NodeFailure::new(
                NodeFailureClass::AuthorizationDenied,
                ReplaySafety::NoExternalEffect,
            ));
        }
        let result: Value = serde_json::from_slice(wire).map_err(|_| {
            NodeFailure::new(NodeFailureClass::InvalidResult, ReplaySafety::Unclassified)
        })?;
        Ok(NodeOutput::new().with_update("result", result["result"].clone()))
    }
}
#[tokio::test]
#[allow(
    clippy::format_collect,
    reason = "Keep independent hexadecimal fixture generation separate from production helpers."
)]
#[allow(
    clippy::too_many_lines,
    reason = "Keep the complete identity and failure assertions in one fixture."
)]
async fn node_recovery_owner_result_requires_exact_proof_and_preserves_failed_attempt_without_dispatch()
 {
    use super::super::node_recovery::OperatorRetryRequest;
    use super::super::node_recovery_owner::*;
    let factory = factory();
    let body = body(vec![Err(NodeFailure::new(
        NodeFailureClass::WorkerInterrupted,
        ReplaySafety::UnknownExternalEffect { effect_id: [7; 32] },
    ))]);
    wrapper(body.clone(), factory.clone(), policy())
        .execute(&context())
        .await
        .unwrap();
    let activation = NodeAttemptActivation::from_context("code", [2; 32], &context()).unwrap();
    let journal = factory.open(&activation, &policy()).await.unwrap();
    let before = journal.load().await.unwrap();
    let request = OperatorRetryRequest {
        request_id: [8; 32],
        activation_id: [1; 32],
        expected_revision: before.ledger.revision(),
    };
    let wire = br#"{"result":42}"#;
    let hash = sha256(wire);
    let digest = hash.iter().map(|b| format!("{b:02x}")).collect::<String>();
    let proof = NodeRecoveryOwnerProof {
        owner_receipt_ref: None,
        schema: "elitea.pipeline.node-recovery-owner-proof.v1".into(),
        kind: NodeRecoveryOwnerProofKind::CommittedResult,
        execution_id: "execution-one".into(),
        generation: 1,
        activation_id: "01".repeat(32),
        attempt: 1,
        expected_revision: request.expected_revision,
        effect_id: "07".repeat(32),
        owner_receipt_sha256: digest.clone(),
        result_ref: Some(NodeRecoveryResultReference {
            content_id: "07".repeat(32),
            immutable_version: digest.clone(),
            digest_sha256: digest,
            byte_length: wire.len() as u64,
            media_type: "application/json".into(),
            required_grant_audience: HTTP_RECEIPT_AUDIENCE.into(),
        }),
    };
    assert!(
        journal
            .resume_owner_result(
                &activation,
                &context(),
                request,
                &proof,
                wire,
                &ResultProjector,
                &OwnerAuthorizer(false),
                101
            )
            .await
            .is_err()
    );
    assert_eq!(journal.load().await.unwrap().ledger, before.ledger);
    let mut wrong = proof.clone();
    wrong.effect_id = "08".repeat(32);
    assert!(
        journal
            .resume_owner_result(
                &activation,
                &context(),
                request,
                &wrong,
                wire,
                &ResultProjector,
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
                &context(),
                request,
                &proof,
                b"altered",
                &ResultProjector,
                &OwnerAuthorizer(true),
                101
            )
            .await
            .is_err()
    );
    let applied = journal
        .resume_owner_result(
            &activation,
            &context(),
            request,
            &proof,
            wire,
            &ResultProjector,
            &OwnerAuthorizer(true),
            101,
        )
        .await
        .unwrap();
    let after = journal.load().await.unwrap();
    assert_eq!(after.ledger.history(), before.ledger.history());
    assert_eq!(after.ledger.revision(), before.ledger.revision() + 1);
    assert!(matches!(
        after.ledger.phase(),
        NodeAttemptPhase::Completed { .. }
    ));
    assert_eq!(
        after.updates.as_ref().unwrap()["result"],
        serde_json::json!(42)
    );
    let replay = journal
        .resume_owner_result(
            &activation,
            &context(),
            request,
            &proof,
            wire,
            &ResultProjector,
            &OwnerAuthorizer(true),
            102,
        )
        .await
        .unwrap();
    assert_eq!(replay.applied_revision(), applied.applied_revision());
    assert_eq!(journal.load().await.unwrap().ledger, after.ledger);
    let output = wrapper(body.clone(), factory, policy())
        .execute(&context())
        .await
        .unwrap();
    assert_eq!(output.updates["result"], serde_json::json!(42));
    assert_eq!(body.calls.lock().await.len(), 1);
}

#[tokio::test]
async fn node_recovery_operator_expiry_restores_only_admitted_typed_error_route_with_zero_retry() {
    use super::super::node_recovery::{
        ErrorRouteContract, NodeErrorRoute, NodeRetryMode, OperatorRetryRequest,
    };
    let route = NodeErrorRoute::admit(
        [8; 32],
        BTreeSet::from([NodeFailureClass::DependencyUnavailable]),
        ErrorRouteContract {
            dedicated_typed_error_input: true,
            requires_success_output: false,
            reexecutes_failed_operation: false,
            explicitly_handles_denial: false,
        },
    )
    .unwrap();
    let policy = NodeRecoveryPolicy::admit(
        3,
        BTreeSet::from([RetryCondition::DependencyUnavailable]),
        Some(NodeBackoff {
            initial_ms: 10,
            maximum_ms: 50,
            multiplier: 2,
        }),
        Some(1000),
        Some(route),
    )
    .unwrap()
    .with_retry_mode(NodeRetryMode::Operator)
    .unwrap();
    let factory = factory();
    let body = body(vec![Err(NodeFailure::new(
        NodeFailureClass::DependencyUnavailable,
        ReplaySafety::NoExternalEffect,
    ))]);
    wrapper(body.clone(), factory.clone(), policy.clone())
        .execute(&context())
        .await
        .unwrap();
    let activation = NodeAttemptActivation::from_context("code", [2; 32], &context()).unwrap();
    let journal = factory.open(&activation, &policy).await.unwrap();
    let before = journal.load().await.unwrap();
    let request = OperatorRetryRequest {
        request_id: [9; 32],
        activation_id: [1; 32],
        expected_revision: before.ledger.revision(),
    };
    let applied = journal
        .resume_operator_retry(&activation, request, &Authorizer(true), 1100)
        .await
        .unwrap();
    assert!(applied.terminal_stop_reason().is_none());
    assert!(applied.matches(request));
    let after = journal.load().await.unwrap();
    assert_eq!(after.ledger.history(), before.ledger.history());
    assert!(matches!(
        after.ledger.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::ErrorRoute { .. })
    ));
    let replay = journal
        .resume_operator_retry(&activation, request, &Authorizer(true), 1101)
        .await
        .unwrap();
    assert_eq!(replay.applied_revision(), applied.applied_revision());
    assert_eq!(journal.load().await.unwrap().ledger, after.ledger);
    let mut restored = wrapper(body.clone(), factory, policy);
    restored.error_route = Some(("handler".into(), "node_error".into()));
    let output = restored.execute(&context()).await.unwrap();
    assert_eq!(output.goto, Some(vec!["handler".into()]));
    assert_eq!(output.updates.len(), 1);
    assert_eq!(
        output.updates["node_error"]["reason"],
        serde_json::json!("elapsed_limit")
    );
    assert!(!output.updates.contains_key("result"));
    assert_eq!(body.calls.lock().await.len(), 1);
}

struct ReportBody {
    body: Arc<Body>,
    factory: Arc<Factory>,
    reports: AtomicU64,
    reject_failure_append: bool,
    revoke_before_failure_append: bool,
}

#[async_trait]
impl Node for ReportBody {
    #[allow(
        clippy::unnecessary_literal_bound,
        reason = "Match the existing runtime trait signature in this fixture."
    )]
    fn name(&self) -> &str {
        "code"
    }
    async fn execute(&self, _: &NodeContext) -> Result<NodeOutput, GraphError> {
        panic!("use the current-writer attempt path");
    }
}

#[async_trait]
impl NodeAttemptBody for ReportBody {
    async fn execute_attempt(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
    ) -> Result<NodeOutput, NodeFailure> {
        self.body.execute_attempt(context, authority).await
    }
    async fn execute_attempt_reported(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
    ) -> Result<NodeOutput, NodeAttemptReportedFailure> {
        let result = self.execute_attempt(context, authority).await;
        if result.is_err() {
            self.factory
                .store
                .reject_append
                .store(self.reject_failure_append, Ordering::SeqCst);
            if self.revoke_before_failure_append {
                self.factory.lease.0.store(false, Ordering::SeqCst);
            }
        }
        result.map_err(|failure| NodeAttemptReportedFailure {
            failure,
            terminal_code: Some("pipeline.code_preparation_unconfirmed"),
        })
    }
    async fn report_terminal_failure(
        &self,
        _: &NodeContext,
        _: &NodeAttemptAuthority,
        code: &'static str,
    ) -> Result<(), GraphError> {
        // Publication observes the committed failure, not a local return value.
        let stored = NodeAttemptJournal::bound(
            self.factory.store.clone(),
            self.factory.lease.clone(),
            "node-journal".into(),
            [1; 32],
            NodeRecoveryPolicy::default(),
        )
        .load()
        .await?;
        assert!(matches!(
            stored.ledger.phase(),
            NodeAttemptPhase::Failed(RecoveryDecision::Stop(_))
        ));
        assert_eq!(code, "pipeline.code_preparation_unconfirmed");
        self.reports.fetch_add(1, Ordering::SeqCst);
        Ok(())
    }
}

fn report_body(factory: Arc<Factory>, outcomes: Vec<Result<u64, NodeFailure>>) -> Arc<ReportBody> {
    Arc::new(ReportBody {
        body: body(outcomes),
        factory,
        reports: AtomicU64::new(0),
        reject_failure_append: false,
        revoke_before_failure_append: false,
    })
}

#[tokio::test]
async fn code_terminal_diagnostic_requires_a_committed_failure_and_never_restarts_the_attempt() {
    let factory = factory();
    let body = report_body(
        factory.clone(),
        vec![Err(NodeFailure::new(
            NodeFailureClass::Unknown,
            ReplaySafety::NoExternalEffect,
        ))],
    );
    let node = RecoverableNode::new(
        body.clone(),
        [2; 32],
        NodeRecoveryPolicy::default(),
        factory,
        None,
    );
    assert!(node.execute(&context()).await.is_err());
    assert_eq!(body.reports.load(Ordering::SeqCst), 1);
    assert_eq!(body.body.calls.lock().await.len(), 1);
    assert!(node.execute(&context()).await.is_err());
    assert_eq!(body.body.calls.lock().await.len(), 1);
    assert_eq!(body.reports.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn code_terminal_diagnostic_is_not_published_after_append_failure_or_lease_loss() {
    for revoke in [false, true] {
        let factory = factory();
        let mut body = report_body(
            factory.clone(),
            vec![Err(NodeFailure::new(
                NodeFailureClass::Unknown,
                ReplaySafety::NoExternalEffect,
            ))],
        );
        let mutable = Arc::get_mut(&mut body).unwrap();
        mutable.reject_failure_append = !revoke;
        mutable.revoke_before_failure_append = revoke;
        let node = RecoverableNode::new(
            body.clone(),
            [2; 32],
            NodeRecoveryPolicy::default(),
            factory.clone(),
            None,
        );
        assert!(node.execute(&context()).await.is_err());
        assert_eq!(body.reports.load(Ordering::SeqCst), 0);
        assert_eq!(body.body.calls.lock().await.len(), 1);
        // The committed checkpoint still contains Started, not a false terminal cause.
        factory.lease.0.store(true, Ordering::SeqCst);
        let stored = NodeAttemptJournal::bound(
            factory.store.clone(),
            factory.lease.clone(),
            "node-journal".into(),
            [1; 32],
            NodeRecoveryPolicy::default(),
        )
        .load()
        .await
        .unwrap();
        assert_eq!(stored.ledger.phase(), NodeAttemptPhase::Running);
    }
}

#[tokio::test]
async fn code_terminal_diagnostic_does_not_replace_retry_route_or_operator_reconciliation() {
    use super::super::node_recovery::{ErrorRouteContract, NodeErrorRoute, NodeRetryMode};
    let route = NodeErrorRoute::admit(
        [8; 32],
        BTreeSet::from([NodeFailureClass::InvalidInput]),
        ErrorRouteContract {
            dedicated_typed_error_input: true,
            requires_success_output: false,
            reexecutes_failed_operation: false,
            explicitly_handles_denial: false,
        },
    )
    .unwrap();
    let route_policy =
        NodeRecoveryPolicy::admit(1, BTreeSet::new(), None, None, Some(route)).unwrap();
    let operator_policy = policy().with_retry_mode(NodeRetryMode::Operator).unwrap();
    for (policy, failure, expected) in [
        (
            policy(),
            NodeFailure::new(
                NodeFailureClass::DependencyUnavailable,
                ReplaySafety::NoExternalEffect,
            ),
            "retry",
        ),
        (
            route_policy,
            NodeFailure::new(
                NodeFailureClass::InvalidInput,
                ReplaySafety::NoExternalEffect,
            ),
            "route",
        ),
        (
            operator_policy,
            NodeFailure::new(
                NodeFailureClass::DependencyUnavailable,
                ReplaySafety::NoExternalEffect,
            ),
            "approval",
        ),
        (
            policy(),
            NodeFailure::new(
                NodeFailureClass::Unknown,
                ReplaySafety::UnknownExternalEffect { effect_id: [7; 32] },
            ),
            "reconcile",
        ),
    ] {
        let factory = factory();
        let body = report_body(factory.clone(), vec![Err(failure), Ok(42)]);
        let mut node = RecoverableNode::new(
            body.clone(),
            [2; 32],
            policy,
            factory,
            Some(("handler".into(), "node_error".into())),
        );
        node.clock = Arc::new(Clock(AtomicU64::new(100)));
        let output = node.execute(&context()).await.unwrap();
        match expected {
            "retry" => {
                assert_eq!(output.updates["result"], serde_json::json!(42));
                assert_eq!(body.body.calls.lock().await.len(), 2);
            }
            "route" => assert_eq!(output.goto, Some(vec!["handler".into()])),
            _ => {
                assert!(output.interrupt.is_some());
                assert_eq!(body.body.calls.lock().await.len(), 1);
            }
        }
        assert_eq!(body.reports.load(Ordering::SeqCst), 0);
    }
}

#[derive(Default)]
struct UnconfirmedPreparation(std::sync::atomic::AtomicU64);
#[async_trait]
impl super::super::code_runtime::CodeSandboxRuntime for UnconfirmedPreparation {
    async fn execute(
        &self,
        _: super::super::code_runtime::CodeInvocation<'_>,
    ) -> Result<Vec<u8>, GraphError> {
        panic!("use the typed current-writer Code attempt");
    }
    async fn execute_attempt(
        &self,
        _: super::super::code_runtime::CodeInvocation<'_>,
        _: &NodeAttemptAuthority,
    ) -> Result<Vec<u8>, super::super::code_runtime::CodeAttemptFailure> {
        self.0.fetch_add(1, Ordering::SeqCst);
        Err(super::super::code_runtime::CodeAttemptFailure::new(
            super::super::code_runtime::CodeAttemptPhase::Preparation,
            NodeFailureClass::Unknown,
            ReplaySafety::NoExternalEffect,
        )
        .with_preparation_failure(super::super::code_runtime::CodePreparationFailure::Unconfirmed))
    }
}

#[tokio::test]
async fn code_preparation_cause_survives_the_graph_wrapper_and_native_runner() {
    use super::super::{
        EliteaGraphAgent,
        compiler::{PipelineDefinition, PipelineNodeRuntimes},
        node_events::{PipelineNodeEventStreamingAgent, pipeline_node_event_channel},
    };
    use crate::agents::runtime::NativeAgentInvocation;
    use adk_rust::runner::Runner;
    use adk_rust::session::{CreateRequest, InMemorySessionService, SessionService};
    use adk_rust::{Content, SessionId, UserId};
    let definition = PipelineDefinition::from_yaml("state:\n  count: {type: int, value: 2}\nentry_point: run\nnodes:\n  - id: run\n    type: code\n    code: '7'\n    input: [count]\n    output: [count]\n    transition: END\n").unwrap();
    let sandbox = Arc::new(UnconfirmedPreparation::default());
    let factory = factory();
    let (sender, receiver) = pipeline_node_event_channel();
    let graph = definition
        .compile_with_runtime(
            "preparation-failure",
            Arc::new(MemoryCheckpointer::new()),
            None,
            &PipelineNodeRuntimes::default()
                .with_code(sandbox.clone())
                .with_node_recovery_authority(factory.clone())
                .with_events(sender),
        )
        .unwrap();
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "elitea".into(),
            user_id: "user-1".into(),
            session_id: Some("preparation-failure-thread".into()),
            state: std::collections::HashMap::default(),
        })
        .await
        .unwrap();
    let runner = Runner::builder()
        .app_name("elitea")
        .agent(Arc::new(PipelineNodeEventStreamingAgent::new(
            Arc::new(EliteaGraphAgent::new(graph)),
            receiver.clone(),
        )))
        .session_service(sessions)
        .build()
        .unwrap();
    let mut running = NativeAgentInvocation::new(
        runner,
        UserId::new("user-1").unwrap(),
        SessionId::new("preparation-failure-thread").unwrap(),
        Content::new("user").with_text("run"),
    )
    .start()
    .unwrap();
    let error = loop {
        match running.next_event().await {
            Err(error) => break error,
            Ok(Some(_)) => {}
            Ok(None) => panic!("unconfirmed preparation cannot complete"),
        }
    };
    assert_eq!(
        error.upstream_code(),
        Some("pipeline.code_preparation_unconfirmed")
    );
    let kind = crate::protocol::output::model_failure(error.upstream_code());
    assert_eq!(
        kind,
        crate::protocol::output::RuntimeFailureKind::CodePreparationUnconfirmed
    );
    assert!(
        kind.safe_message()
            .contains("preparation could not be confirmed")
    );
    assert!(kind.safe_message().contains("was not restarted"));
    assert_eq!(sandbox.0.load(Ordering::SeqCst), 1);
    let stored = NodeAttemptJournal::bound(
        factory.store.clone(),
        factory.lease.clone(),
        "node-journal".into(),
        [1; 32],
        NodeRecoveryPolicy::default(),
    )
    .load()
    .await
    .unwrap();
    assert!(matches!(
        stored.ledger.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::Stop(_))
    ));
    assert_eq!(stored.ledger.history().len(), 1);
}

#[tokio::test]
async fn code_preparation_cause_survives_the_application_child_event_drain() {
    use super::super::node_events::pipeline_node_event_channel;
    let (sender, receiver) = pipeline_node_event_channel();
    let mut drain = receiver
        .drain("child-invocation", "child-agent", "root.child")
        .await
        .unwrap();
    sender
        .send_execution_failure("pipeline.code_preparation_unconfirmed")
        .await
        .unwrap();
    let error = drain.try_recv().unwrap().unwrap_err();
    assert_eq!(error.code, "pipeline.code_preparation_unconfirmed");
    let report =
        crate::agents::application_tools::child_failure_report(&error, None, false).unwrap();
    assert!(
        report["failure"]["message"]
            .as_str()
            .unwrap()
            .contains("reconcile the existing attempt")
    );
    assert_eq!(report["failure"]["retryable"], serde_json::json!(false));
    assert_eq!(
        report["failure"]["recovery_action"],
        serde_json::json!("ask_administrator")
    );
    assert!(crate::agents::application_tools::child_failure_report(&error, None, true).is_none());
}

struct TypedCodeFailure {
    factory: Arc<Factory>,
    calls: AtomicU64,
    class: NodeFailureClass,
    preparation: Option<super::super::code_runtime::CodePreparationFailure>,
    reject_append: bool,
    revoke_after_append: bool,
}

#[async_trait]
impl super::super::code_runtime::CodeSandboxRuntime for TypedCodeFailure {
    async fn execute(
        &self,
        _: super::super::code_runtime::CodeInvocation<'_>,
    ) -> Result<Vec<u8>, GraphError> {
        panic!("use the typed Code attempt");
    }
    async fn execute_attempt(
        &self,
        _: super::super::code_runtime::CodeInvocation<'_>,
        authority: &NodeAttemptAuthority,
    ) -> Result<Vec<u8>, super::super::code_runtime::CodeAttemptFailure> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        self.factory
            .store
            .reject_append
            .store(self.reject_append, Ordering::SeqCst);
        if self.revoke_after_append {
            *self.factory.store.revoke_after_append.lock().await = Some(self.factory.lease.clone());
        }
        let mut failure = super::super::code_runtime::CodeAttemptFailure::new(
            super::super::code_runtime::CodeAttemptPhase::Observation,
            self.class,
            if self.class == NodeFailureClass::AuthorizationDenied {
                ReplaySafety::UnknownExternalEffect {
                    effect_id: authority.dispatch_activation(),
                }
            } else {
                ReplaySafety::NoExternalEffect
            },
        );
        failure.preparation = self.preparation;
        Err(failure)
    }
}

fn typed_code_failure(factory: Arc<Factory>, class: NodeFailureClass) -> Arc<TypedCodeFailure> {
    Arc::new(TypedCodeFailure {
        factory,
        calls: AtomicU64::new(0),
        class,
        preparation: None,
        reject_append: false,
        revoke_after_append: false,
    })
}

async fn typed_code_native_failure(
    factory: Arc<Factory>,
    runtime: Arc<TypedCodeFailure>,
    empty_source: bool,
) -> String {
    use super::super::{
        EliteaGraphAgent,
        compiler::{PipelineDefinition, PipelineNodeRuntimes},
        node_events::{PipelineNodeEventStreamingAgent, pipeline_node_event_channel},
    };
    use crate::agents::runtime::NativeAgentInvocation;
    use adk_rust::runner::Runner;
    use adk_rust::session::{CreateRequest, InMemorySessionService, SessionService};
    use adk_rust::{Content, SessionId, UserId};
    let source = if empty_source {
        "{type: variable, value: source}"
    } else {
        "'7'"
    };
    let yaml = format!(
        "state:\n  count: {{type: int, value: 2}}\n  source: {{type: str, value: ''}}\nentry_point: run\nnodes:\n  - id: run\n    type: code\n    code: {source}\n    input: [count]\n    output: [count]\n    transition: END\n"
    );
    let definition = PipelineDefinition::from_yaml(&yaml).unwrap();
    let (sender, receiver) = pipeline_node_event_channel();
    let graph = definition
        .compile_with_runtime(
            "typed-code-failure",
            Arc::new(MemoryCheckpointer::new()),
            None,
            &PipelineNodeRuntimes::default()
                .with_code(runtime)
                .with_node_recovery_authority(factory)
                .with_events(sender),
        )
        .unwrap();
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "elitea".into(),
            user_id: "user-1".into(),
            session_id: Some("typed-code-failure-thread".into()),
            state: std::collections::HashMap::default(),
        })
        .await
        .unwrap();
    let runner = Runner::builder()
        .app_name("elitea")
        .agent(Arc::new(PipelineNodeEventStreamingAgent::new(
            Arc::new(EliteaGraphAgent::new(graph)),
            receiver,
        )))
        .session_service(sessions)
        .build()
        .unwrap();
    let mut running = NativeAgentInvocation::new(
        runner,
        UserId::new("user-1").unwrap(),
        SessionId::new("typed-code-failure-thread").unwrap(),
        Content::new("user").with_text("run"),
    )
    .start()
    .unwrap();
    loop {
        match running.next_event().await {
            Err(error) => {
                return error
                    .upstream_code()
                    .expect("typed upstream code")
                    .to_owned();
            }
            Ok(Some(_)) => {}
            Ok(None) => panic!("failed Code cannot complete"),
        }
    }
}

fn code_journal(factory: &Factory) -> NodeAttemptJournal {
    NodeAttemptJournal::bound(
        factory.store.clone(),
        factory.lease.clone(),
        "node-journal".into(),
        [1; 32],
        NodeRecoveryPolicy::default(),
    )
}

#[tokio::test]
async fn code_typed_fresh_and_historical_failures_keep_category_without_reexecution() {
    for (class, expected) in [
        (NodeFailureClass::InvalidInput, "pipeline.code_failed"),
        (
            NodeFailureClass::AuthorizationDenied,
            "pipeline.code_authorization_failed",
        ),
        (NodeFailureClass::Cancelled, "pipeline.code_cancelled"),
    ] {
        let factory = factory();
        let runtime = typed_code_failure(factory.clone(), class);
        assert_eq!(
            typed_code_native_failure(factory.clone(), runtime.clone(), false).await,
            expected
        );
        let journal = code_journal(&factory);
        let before = journal.load().await.unwrap();
        let checkpoint = factory.store.load("node-journal").await.unwrap().unwrap();
        assert!(matches!(
            before.ledger.phase(),
            NodeAttemptPhase::Failed(RecoveryDecision::Stop(_))
        ));
        assert_eq!(before.ledger.effective_failure().unwrap().class, class);
        assert_eq!(
            typed_code_native_failure(factory.clone(), runtime.clone(), false).await,
            expected
        );
        let after = journal.load().await.unwrap();
        assert_eq!(after.ledger.revision(), before.ledger.revision());
        assert_eq!(after.ledger.history(), before.ledger.history());
        assert_eq!(
            factory
                .store
                .load("node-journal")
                .await
                .unwrap()
                .unwrap()
                .checkpoint_id,
            checkpoint.checkpoint_id
        );
        assert_eq!(runtime.calls.load(Ordering::SeqCst), 1);
    }
}

#[tokio::test]
async fn code_source_failure_is_typed_before_any_runtime_call() {
    let factory = factory();
    let runtime = typed_code_failure(factory.clone(), NodeFailureClass::Unknown);
    assert_eq!(
        typed_code_native_failure(factory.clone(), runtime.clone(), true).await,
        "pipeline.code_failed"
    );
    assert_eq!(runtime.calls.load(Ordering::SeqCst), 0);
    assert_eq!(
        code_journal(&factory)
            .load()
            .await
            .unwrap()
            .ledger
            .effective_failure()
            .unwrap()
            .class,
        NodeFailureClass::InvalidInput
    );
}

#[tokio::test]
async fn code_typed_signal_requires_accepted_append_and_current_writer_after_commit() {
    for revoked in [false, true] {
        let factory = factory();
        let mut runtime = typed_code_failure(factory.clone(), NodeFailureClass::InvalidInput);
        let mutable = Arc::get_mut(&mut runtime).unwrap();
        mutable.reject_append = !revoked;
        mutable.revoke_after_append = revoked;
        assert_eq!(
            typed_code_native_failure(factory.clone(), runtime.clone(), false).await,
            "agent.legacy"
        );
        factory.lease.0.store(true, Ordering::SeqCst);
        let phase = code_journal(&factory).load().await.unwrap().ledger.phase();
        if revoked {
            assert!(matches!(
                phase,
                NodeAttemptPhase::Failed(RecoveryDecision::Stop(_))
            ));
        } else {
            assert_eq!(phase, NodeAttemptPhase::Running);
        }
        assert_eq!(runtime.calls.load(Ordering::SeqCst), 1);
    }
}

#[tokio::test]
async fn code_preparation_cancel_is_failure_and_unstored_phase_is_not_inferred_on_replay() {
    let factory = factory();
    let mut runtime = typed_code_failure(factory.clone(), NodeFailureClass::Unknown);
    Arc::get_mut(&mut runtime).unwrap().preparation =
        Some(super::super::code_runtime::CodePreparationFailure::Cancelled);
    assert_eq!(
        typed_code_native_failure(factory.clone(), runtime.clone(), false).await,
        "pipeline.code_preparation_cancelled"
    );
    let before = code_journal(&factory).load().await.unwrap();
    assert_eq!(
        typed_code_native_failure(factory.clone(), runtime.clone(), false).await,
        "pipeline.code_failed"
    );
    assert_eq!(
        code_journal(&factory)
            .load()
            .await
            .unwrap()
            .ledger
            .revision(),
        before.ledger.revision()
    );
    assert_eq!(runtime.calls.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn code_control_stop_without_attempt_preserves_cancel_and_lost_lease_distinction() {
    for (class, expected) in [
        (NodeFailureClass::Cancelled, "pipeline.code_cancelled"),
        (NodeFailureClass::LeaseLost, "agent.legacy"),
    ] {
        let factory = factory();
        let journal = code_journal(&factory);
        let empty = journal.load().await.unwrap();
        let stopped = empty.ledger.record_control_stop(class, 100).unwrap();
        let before = journal.append(&empty, stopped, None).await.unwrap();
        let runtime = typed_code_failure(factory.clone(), NodeFailureClass::Unknown);
        assert_eq!(
            typed_code_native_failure(factory.clone(), runtime.clone(), false).await,
            expected
        );
        let after = journal.load().await.unwrap();
        assert_eq!(after.ledger.revision(), before.ledger.revision());
        assert!(after.ledger.history().is_empty());
        assert_eq!(runtime.calls.load(Ordering::SeqCst), 0);
    }
}

#[tokio::test]
async fn non_code_body_keeps_generic_failure_and_default_replay_projection() {
    let factory = factory();
    let body = body(vec![Err(NodeFailure::new(
        NodeFailureClass::InvalidInput,
        ReplaySafety::NoExternalEffect,
    ))]);
    let node = wrapper(body.clone(), factory.clone(), NodeRecoveryPolicy::default());
    assert!(
        node.execute(&context())
            .await
            .err()
            .unwrap()
            .to_string()
            .contains("pipeline.node_recovery.failed")
    );
    let before = code_journal(&factory).load().await.unwrap();
    assert!(
        node.execute(&context())
            .await
            .err()
            .unwrap()
            .to_string()
            .contains("pipeline.node_recovery.failed")
    );
    assert_eq!(body.calls.lock().await.len(), 1);
    assert_eq!(
        code_journal(&factory)
            .load()
            .await
            .unwrap()
            .ledger
            .revision(),
        before.ledger.revision()
    );
}

#[tokio::test]
async fn parent_cancellation_keeps_control_stop_without_attempt_or_code_projection() {
    use adk_rust::Content;
    use adk_rust::agent::SequentialAgent;
    use adk_rust::session::{CreateRequest, InMemorySessionService, SessionService};
    let factory = factory();
    let body = report_body(factory.clone(), vec![]);
    let node = RecoverableNode::new(
        body.clone(),
        [2; 32],
        NodeRecoveryPolicy::default(),
        factory.clone(),
        None,
    );
    let sessions = InMemorySessionService::new();
    let session = sessions
        .create(CreateRequest {
            app_name: "elitea".into(),
            user_id: "user-1".into(),
            session_id: Some("parent".into()),
            state: std::collections::HashMap::default(),
        })
        .await
        .unwrap();
    let mut config = adk_rust::runner::Runner::builder()
        .app_name("elitea")
        .agent(Arc::new(SequentialAgent::new("parent", vec![])))
        .session_service(Arc::new(sessions))
        .build_config();
    let token = config.cancellation_token.get_or_insert_default().clone();
    token.cancel();
    let parent = adk_rust::runner::InvocationContext::new(
        "parent-invocation".into(),
        Arc::new(SequentialAgent::new("parent", vec![])),
        "user-1".into(),
        "elitea".into(),
        "parent".into(),
        Content::new("user"),
        session.into(),
    )
    .unwrap()
    .with_cancellation_token(token);
    let mut context = context();
    context.config.parent_context = Some(Arc::new(parent));
    assert!(
        node.execute(&context)
            .await
            .err()
            .unwrap()
            .to_string()
            .contains("pipeline.node_recovery.cancelled")
    );
    let stored = code_journal(&factory).load().await.unwrap();
    assert!(matches!(
        stored.ledger.phase(),
        NodeAttemptPhase::ControlStopped {
            class: NodeFailureClass::Cancelled,
            ..
        }
    ));
    assert!(stored.ledger.history().is_empty());
    assert!(body.body.calls.lock().await.is_empty());
    assert_eq!(body.reports.load(Ordering::SeqCst), 0);
}
