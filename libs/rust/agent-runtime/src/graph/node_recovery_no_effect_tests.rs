use super::*;
use std::collections::BTreeSet;
fn policy(max: u16) -> NodeRecoveryPolicy {
    NodeRecoveryPolicy::admit(
        max,
        BTreeSet::from([RetryCondition::DependencyUnavailable]),
        Some(NodeBackoff {
            initial_ms: 10,
            maximum_ms: 50,
            multiplier: 2,
        }),
        Some(100),
        None,
    )
    .unwrap()
}
fn failed(policy: NodeRecoveryPolicy, class: NodeFailureClass) -> NodeAttemptLedger {
    NodeAttemptLedger::new([1; 32], policy)
        .unwrap()
        .start_attempt(100)
        .unwrap()
        .record_failure(
            NodeFailure::new(
                class,
                ReplaySafety::UnknownExternalEffect { effect_id: [2; 32] },
            ),
            101,
        )
        .unwrap()
}
#[test]
fn no_effect_stop_and_error_route_are_exclusive_and_retain_original_ambiguous_outcome() {
    let route = NodeErrorRoute::admit(
        [7; 32],
        BTreeSet::from([NodeFailureClass::DependencyUnavailable]),
        ErrorRouteContract {
            dedicated_typed_error_input: true,
            requires_success_output: false,
            reexecutes_failed_operation: false,
            explicitly_handles_denial: false,
        },
    )
    .unwrap();
    let policy = NodeRecoveryPolicy::admit(1, BTreeSet::new(), None, None, Some(route)).unwrap();
    let before = failed(policy, NodeFailureClass::DependencyUnavailable);
    let after = before
        .reconcile_verified_no_effect(request(&before), [2; 32], [4; 32], 102)
        .unwrap();
    assert!(
        matches!(after.phase(),NodeAttemptPhase::Failed(RecoveryDecision::ErrorRoute{route_id,failed}) if route_id==[7;32] && failed.reason==StopReason::RetryDisabled)
    );
    assert_eq!(after.history(), before.history());
    assert!(after.start_attempt(103).is_err());
    assert!(after.operator_retry(request(&after), 103).is_err());
    let before = failed(
        NodeRecoveryPolicy::default(),
        NodeFailureClass::DependencyUnavailable,
    );
    let after = before
        .reconcile_verified_no_effect(request(&before), [2; 32], [4; 32], 102)
        .unwrap();
    assert_eq!(
        after.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::Stop(StopReason::RetryDisabled))
    );
}
fn request(ledger: &NodeAttemptLedger) -> OperatorRetryRequest {
    OperatorRetryRequest {
        request_id: [3; 32],
        activation_id: ledger.logical_activation(),
        expected_revision: ledger.revision(),
    }
}
#[test]
fn no_effect_reconciliation_preserves_failure_and_requires_separate_exact_operator_retry() {
    let before = failed(policy(3), NodeFailureClass::DependencyUnavailable);
    let sealed = before
        .reconcile_verified_no_effect(request(&before), [2; 32], [4; 32], 102)
        .unwrap();
    assert_eq!(sealed.history(), before.history());
    assert_eq!(
        sealed.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::Stop(StopReason::OperatorApprovalRequired))
    );
    assert_eq!(
        sealed.effective_failure().unwrap().replay,
        ReplaySafety::NoExternalEffect
    );
    assert!(sealed.start_attempt(103).is_err());
    assert!(sealed.operator_retry(request(&before), 103).is_err());
    let retry = sealed.operator_retry(request(&sealed), 103).unwrap();
    assert!(matches!(
        retry.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::RetryAt { .. })
    ));
    assert_eq!(retry.history(), before.history());
    let wire = codec::encode(&sealed).unwrap();
    assert_eq!(wire["schema"], "elitea.pipeline.node-recovery.v2");
    assert_eq!(codec::decode(&wire, [1; 32], &policy(3)).unwrap(), sealed);
    codec::require_successor(&before, &sealed).unwrap();
    let mut downgrade = wire.clone();
    downgrade["schema"] = serde_json::json!("elitea.pipeline.node-recovery.v1");
    assert!(codec::decode(&downgrade, [1; 32], &policy(3)).is_err());
}
#[test]
fn no_effect_reconciliation_never_relaxes_control_denial_identity_clock_or_budget() {
    for class in [
        NodeFailureClass::AuthorizationDenied,
        NodeFailureClass::SensitiveRejected,
        NodeFailureClass::Cancelled,
    ] {
        let ledger = failed(policy(3), class);
        assert!(
            ledger
                .reconcile_verified_no_effect(request(&ledger), [2; 32], [4; 32], 102)
                .is_err()
        );
    }
    let ledger = failed(policy(3), NodeFailureClass::DependencyUnavailable);
    for (effect, receipt, now) in [
        ([5; 32], [4; 32], 102),
        ([2; 32], [0; 32], 102),
        ([2; 32], [4; 32], 99),
    ] {
        assert!(
            ledger
                .reconcile_verified_no_effect(request(&ledger), effect, receipt, now)
                .is_err()
        );
    }
    let expired = ledger
        .reconcile_verified_no_effect(request(&ledger), [2; 32], [4; 32], 201)
        .unwrap();
    assert_eq!(
        expired.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::Stop(StopReason::ElapsedLimit))
    );
    assert_eq!(expired.history(), ledger.history());
    assert!(expired.operator_retry(request(&expired), 202).is_err());
}
