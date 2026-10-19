//! FU-214: an attempt whose sandbox runtime was lost is re-run by a bounded,
//! platform-owned allowance inside the node's deadline. A user failure, an
//! effect that may have happened, and an exhausted allowance stay typed stops.
use super::*;

const ACTIVATION: [u8; 32] = [1; 32];
const FIRST_MS: u64 = 1_000;

fn lost() -> NodeFailure {
    NodeFailure::new(
        NodeFailureClass::RuntimeLost,
        ReplaySafety::NoExternalEffect,
    )
}

fn user_error() -> NodeFailure {
    NodeFailure::new(
        NodeFailureClass::InvalidResult,
        ReplaySafety::NoExternalEffect,
    )
}

fn started(policy: NodeRecoveryPolicy) -> NodeAttemptLedger {
    NodeAttemptLedger::new(ACTIVATION, policy)
        .unwrap()
        .start_attempt(FIRST_MS)
        .unwrap()
}

/// Fail the running attempt as lost and start the re-run at its retry time.
fn rerun(ledger: &NodeAttemptLedger, now: u64) -> NodeAttemptLedger {
    let failed = ledger.record_failure(lost(), now).unwrap();
    let NodeAttemptPhase::Failed(RecoveryDecision::RetryAt {
        not_before_ms,
        next_attempt,
        effect_id: None,
    }) = failed.phase()
    else {
        panic!("a lost runtime was not re-run: {:?}", failed.phase());
    };
    assert_eq!(usize::from(next_attempt), failed.history().len() + 1);
    failed.start_attempt(not_before_ms).unwrap()
}

#[test]
fn a_lost_runtime_of_a_default_policy_node_is_rerun_and_completes_once() {
    let first = started(NodeRecoveryPolicy::default());
    let second = rerun(&first, FIRST_MS + 30_000);
    assert_eq!(second.history().len(), 2);
    let done = second.record_success([9; 32], FIRST_MS + 60_000).unwrap();
    assert_eq!(
        done.phase(),
        NodeAttemptPhase::Completed {
            receipt_id: [9; 32]
        }
    );
    // The record replays and round-trips under the explicit v3 schema.
    let wire = codec::encode(&done).unwrap();
    assert_eq!(wire["schema"], "elitea.pipeline.node-recovery.v3");
    assert_eq!(
        codec::decode(&wire, ACTIVATION, &NodeRecoveryPolicy::default()).unwrap(),
        done
    );
    // A completed node cannot start another attempt.
    assert_eq!(
        done.start_attempt(FIRST_MS + 70_000),
        Err(NodeRecoveryError::InvalidTransition)
    );
}

#[test]
fn a_user_code_failure_is_never_rerun() {
    let failed = started(NodeRecoveryPolicy::default())
        .record_failure(user_error(), FIRST_MS + 10)
        .unwrap();
    assert_eq!(
        failed.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::Stop(StopReason::RetryDisabled))
    );
    // Nor after a lost runtime was re-run: the re-run spent no user attempt,
    // and the user failure still stops at once.
    let failed = rerun(&started(NodeRecoveryPolicy::default()), FIRST_MS + 10)
        .record_failure(user_error(), FIRST_MS + 2_000)
        .unwrap();
    assert_eq!(
        failed.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::Stop(StopReason::RetryDisabled))
    );
    assert_eq!(
        failed.start_attempt(FIRST_MS + 3_000),
        Err(NodeRecoveryError::InvalidTransition)
    );
    let wire = codec::encode(&failed).unwrap();
    codec::decode(&wire, ACTIVATION, &NodeRecoveryPolicy::default()).unwrap();
}

#[test]
fn lost_reruns_are_bounded_and_then_stop_typed() {
    let mut ledger = started(NodeRecoveryPolicy::default());
    let mut now = FIRST_MS;
    for _ in 0..MAX_RUNTIME_LOST_RETRIES {
        now += 5_000;
        ledger = rerun(&ledger, now);
    }
    assert_eq!(
        ledger.history().len(),
        usize::from(MAX_RUNTIME_LOST_RETRIES) + 1
    );
    let exhausted = ledger.record_failure(lost(), now + 5_000).unwrap();
    assert_eq!(
        exhausted.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::Stop(StopReason::RetryDisabled))
    );
    assert_eq!(
        exhausted.start_attempt(now + 10_000),
        Err(NodeRecoveryError::InvalidTransition)
    );
    let wire = codec::encode(&exhausted).unwrap();
    codec::decode(&wire, ACTIVATION, &NodeRecoveryPolicy::default()).unwrap();
}

#[test]
fn a_lost_runtime_after_the_window_stops_with_the_elapsed_limit() {
    let late = FIRST_MS + RUNTIME_LOST_RETRY_WINDOW_MS;
    assert_eq!(
        started(NodeRecoveryPolicy::default())
            .record_failure(lost(), late)
            .unwrap()
            .phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::Stop(StopReason::RetryDisabled))
    );
    // The window edge: the re-run must be able to start before the deadline.
    let edge = late - RUNTIME_LOST_RETRY_DELAY_MS - 1;
    assert!(matches!(
        started(NodeRecoveryPolicy::default())
            .record_failure(lost(), edge)
            .unwrap()
            .phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::RetryAt { .. })
    ));
    assert_eq!(
        started(NodeRecoveryPolicy::default())
            .record_failure(lost(), edge + 1)
            .unwrap()
            .phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::Stop(StopReason::RetryDisabled))
    );
}

#[test]
fn a_node_elapsed_limit_shorter_than_the_window_is_the_deadline() {
    let policy = NodeRecoveryPolicy::admit(
        2,
        BTreeSet::from([RetryCondition::DependencyUnavailable]),
        Some(NodeBackoff {
            initial_ms: 100,
            maximum_ms: 500,
            multiplier: 2,
        }),
        Some(10_000),
        None,
    )
    .unwrap();
    assert_eq!(
        runtime_lost_deadline(&policy, FIRST_MS).unwrap(),
        FIRST_MS + 10_000
    );
    assert_eq!(
        started(policy.clone())
            .record_failure(lost(), FIRST_MS + 9_500)
            .unwrap()
            .phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::Stop(StopReason::NotRetryable))
    );
    // The lost re-run does not consume the policy's own retry.
    let rerun = rerun(&started(policy.clone()), FIRST_MS + 100);
    let failed = rerun
        .record_failure(
            NodeFailure::new(
                NodeFailureClass::DependencyUnavailable,
                ReplaySafety::NoExternalEffect,
            ),
            FIRST_MS + 2_000,
        )
        .unwrap();
    assert!(matches!(
        failed.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::RetryAt {
            next_attempt: 3,
            ..
        })
    ));
    let third = failed.start_attempt(FIRST_MS + 3_000).unwrap();
    assert_eq!(third.history().len(), 3);
    let wire = codec::encode(&third).unwrap();
    codec::decode(&wire, ACTIVATION, &policy).unwrap();
}

#[test]
fn a_pending_rerun_expires_at_the_lost_runtime_deadline() {
    let failed = started(NodeRecoveryPolicy::default())
        .record_failure(lost(), FIRST_MS + 100)
        .unwrap();
    assert_eq!(failed.expire_wait(FIRST_MS + 200).unwrap(), None);
    // The Worker came back after the window: the re-run never starts.
    let expired = failed
        .expire_wait(FIRST_MS + RUNTIME_LOST_RETRY_WINDOW_MS)
        .unwrap()
        .unwrap();
    assert_eq!(
        expired.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::Stop(StopReason::RetryDisabled))
    );
    assert_eq!(
        failed.start_attempt(FIRST_MS + RUNTIME_LOST_RETRY_WINDOW_MS),
        Err(NodeRecoveryError::ElapsedLimit)
    );
    codec::decode(
        &codec::encode(&expired).unwrap(),
        ACTIVATION,
        &NodeRecoveryPolicy::default(),
    )
    .unwrap();
}

#[test]
fn a_lost_runtime_that_may_have_left_an_effect_is_not_this_class() {
    for replay in [
        ReplaySafety::UnknownExternalEffect { effect_id: [2; 32] },
        ReplaySafety::IdempotentEffectNotCommitted { effect_id: [2; 32] },
        ReplaySafety::CompletedExternalEffect {
            receipt_id: [2; 32],
        },
        ReplaySafety::Unclassified,
    ] {
        assert_eq!(
            started(NodeRecoveryPolicy::default()).record_failure(
                NodeFailure::new(NodeFailureClass::RuntimeLost, replay),
                FIRST_MS + 10
            ),
            Err(NodeRecoveryError::InvalidEffectIdentity)
        );
    }
}

#[test]
fn the_class_is_platform_owned_and_never_routable_or_operator_retried() {
    assert_eq!(
        NodeErrorRoute::admit(
            [4; 32],
            BTreeSet::from([NodeFailureClass::RuntimeLost]),
            ErrorRouteContract {
                dedicated_typed_error_input: true,
                requires_success_output: false,
                reexecutes_failed_operation: false,
                explicitly_handles_denial: false,
            },
        ),
        Err(NodeRecoveryError::InvalidErrorRoute)
    );
    let mut ledger = started(NodeRecoveryPolicy::default());
    let mut now = FIRST_MS;
    for _ in 0..MAX_RUNTIME_LOST_RETRIES {
        now += 10_000;
        ledger = rerun(&ledger, now);
    }
    let exhausted = ledger.record_failure(lost(), now + 10_000).unwrap();
    assert_eq!(
        exhausted.operator_retry(
            OperatorRetryRequest {
                request_id: [5; 32],
                activation_id: ACTIVATION,
                expected_revision: exhausted.revision(),
            },
            now + 20_000,
        ),
        Err(NodeRecoveryError::OperatorRetryDenied)
    );
}

#[test]
fn a_forged_v1_record_of_a_lost_runtime_is_refused() {
    let failed = started(NodeRecoveryPolicy::default())
        .record_failure(lost(), FIRST_MS + 10)
        .unwrap();
    let mut wire = codec::encode(&failed).unwrap();
    wire["schema"] = serde_json::json!("elitea.pipeline.node-recovery.v1");
    assert_eq!(
        codec::decode(&wire, ACTIVATION, &NodeRecoveryPolicy::default()),
        Err(NodeRecoveryError::InvalidHistory)
    );
}

#[test]
fn planning_rejects_impossible_histories() {
    let policy = NodeRecoveryPolicy::default();
    assert_eq!(
        plan_runtime_lost(&policy, 0, 0, FIRST_MS, FIRST_MS, lost()),
        Err(NodeRecoveryError::InvalidHistory)
    );
    assert_eq!(
        plan_runtime_lost(&policy, 2, 2, FIRST_MS, FIRST_MS, lost()),
        Err(NodeRecoveryError::InvalidHistory)
    );
    assert_eq!(
        plan_runtime_lost(&policy, 1, 0, FIRST_MS, FIRST_MS - 1, lost()),
        Err(NodeRecoveryError::InvalidClock)
    );
    assert_eq!(
        plan_runtime_lost(&policy, MAX_NODE_ATTEMPTS, 0, FIRST_MS, FIRST_MS, lost()),
        Ok(RecoveryDecision::Stop(StopReason::AttemptsExhausted))
    );
    assert_eq!(
        plan_runtime_lost(&policy, 1, 0, FIRST_MS, FIRST_MS, user_error()),
        Err(NodeRecoveryError::InvalidEffectIdentity)
    );
}

/// Past the allowance a lost runtime is decided as a failed result, the way it
/// was before the allowance existed: a configured error route still gets it.
#[test]
fn an_exhausted_lost_runtime_still_reaches_the_configured_error_route() {
    let route = NodeErrorRoute::admit(
        [4; 32],
        BTreeSet::from([NodeFailureClass::InvalidResult]),
        ErrorRouteContract {
            dedicated_typed_error_input: true,
            requires_success_output: false,
            reexecutes_failed_operation: false,
            explicitly_handles_denial: false,
        },
    )
    .unwrap();
    let policy = NodeRecoveryPolicy::admit(1, BTreeSet::new(), None, None, Some(route)).unwrap();
    let mut ledger = started(policy.clone());
    let mut now = FIRST_MS;
    for _ in 0..MAX_RUNTIME_LOST_RETRIES {
        now += 5_000;
        ledger = rerun(&ledger, now);
    }
    let routed = ledger.record_failure(lost(), now + 5_000).unwrap();
    let NodeAttemptPhase::Failed(RecoveryDecision::ErrorRoute { route_id, failed }) =
        routed.phase()
    else {
        panic!("not routed: {:?}", routed.phase());
    };
    assert_eq!(route_id, [4; 32]);
    assert_eq!(failed.class, NodeFailureClass::InvalidResult);
    assert_eq!(failed.attempt, MAX_RUNTIME_LOST_RETRIES + 1);
    codec::decode(&codec::encode(&routed).unwrap(), ACTIVATION, &policy).unwrap();
}
