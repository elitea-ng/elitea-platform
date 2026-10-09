use super::*;

const ACTIVATION: [u8; 32] = [1; 32];
const EFFECT: [u8; 32] = [2; 32];
const RECEIPT: [u8; 32] = [3; 32];
const ROUTE: [u8; 32] = [4; 32];

fn retry_policy(max_attempts: u16) -> NodeRecoveryPolicy {
    NodeRecoveryPolicy::admit(
        max_attempts,
        BTreeSet::from([
            RetryCondition::DependencyUnavailable,
            RetryCondition::RateLimited,
            RetryCondition::AttemptTimeout,
            RetryCondition::WorkerInterrupted,
        ]),
        Some(NodeBackoff {
            initial_ms: 100,
            maximum_ms: 500,
            multiplier: 2,
        }),
        Some(10_000),
        None,
    )
    .unwrap()
}

fn transient() -> NodeFailure {
    NodeFailure {
        class: NodeFailureClass::DependencyUnavailable,
        replay: ReplaySafety::NoExternalEffect,
    }
}

fn route_for(classes: &[NodeFailureClass], denial: bool) -> NodeErrorRoute {
    NodeErrorRoute::admit(
        ROUTE,
        classes.iter().copied().collect(),
        ErrorRouteContract {
            dedicated_typed_error_input: true,
            requires_success_output: false,
            reexecutes_failed_operation: false,
            explicitly_handles_denial: denial,
        },
    )
    .unwrap()
}

fn decide(
    policy: &NodeRecoveryPolicy,
    failure: NodeFailure,
    attempts: u16,
    now: u64,
) -> RecoveryDecision {
    plan_after_failure(policy, ACTIVATION, attempts, 1_000, now, failure).unwrap()
}

#[test]
fn omitted_policy_stops_after_one_attempt_without_success_output() {
    let ledger = NodeAttemptLedger::new(ACTIVATION, NodeRecoveryPolicy::default()).unwrap();
    let started = ledger.start_attempt(1_000).unwrap();
    let failed = started.record_failure(transient(), 1_001).unwrap();
    assert_eq!(
        failed.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::Stop(StopReason::RetryDisabled))
    );
    assert_eq!(failed.history().len(), 1);
    assert_eq!(
        failed.start_attempt(1_002),
        Err(NodeRecoveryError::InvalidTransition)
    );
}

#[test]
fn policy_requires_explicit_conditions_attempts_backoff_and_elapsed_budget() {
    let backoff = Some(NodeBackoff {
        initial_ms: 100,
        maximum_ms: 500,
        multiplier: 2,
    });
    for (attempts, conditions, delay, elapsed) in [
        (0, BTreeSet::new(), None, None),
        (
            17,
            BTreeSet::from([RetryCondition::RateLimited]),
            backoff,
            Some(1000),
        ),
        (2, BTreeSet::new(), backoff, Some(1000)),
        (
            2,
            BTreeSet::from([RetryCondition::RateLimited]),
            None,
            Some(1000),
        ),
        (
            2,
            BTreeSet::from([RetryCondition::RateLimited]),
            backoff,
            None,
        ),
        (
            2,
            BTreeSet::from([RetryCondition::RateLimited]),
            backoff,
            Some(0),
        ),
        (
            2,
            BTreeSet::from([RetryCondition::RateLimited]),
            backoff,
            Some(MAX_ELAPSED_MS + 1),
        ),
        (1, BTreeSet::from([RetryCondition::RateLimited]), None, None),
    ] {
        assert_eq!(
            NodeRecoveryPolicy::admit(attempts, conditions, delay, elapsed, None),
            Err(NodeRecoveryError::InvalidPolicy)
        );
    }
    for invalid in [
        NodeBackoff {
            initial_ms: 0,
            maximum_ms: 100,
            multiplier: 2,
        },
        NodeBackoff {
            initial_ms: 200,
            maximum_ms: 100,
            multiplier: 2,
        },
        NodeBackoff {
            initial_ms: 1,
            maximum_ms: MAX_BACKOFF_MS + 1,
            multiplier: 2,
        },
        NodeBackoff {
            initial_ms: 1,
            maximum_ms: 100,
            multiplier: 0,
        },
        NodeBackoff {
            initial_ms: 1,
            maximum_ms: 100,
            multiplier: 17,
        },
    ] {
        assert_eq!(
            NodeRecoveryPolicy::admit(
                2,
                BTreeSet::from([RetryCondition::RateLimited]),
                Some(invalid),
                Some(1000),
                None
            ),
            Err(NodeRecoveryError::InvalidPolicy)
        );
    }
}

#[test]
fn classified_backoff_has_exact_cap_and_total_attempt_count() {
    let policy = retry_policy(5);
    for (attempt, delay) in [(1, 100), (2, 200), (3, 400), (4, 500)] {
        assert_eq!(
            decide(&policy, transient(), attempt, 1_000),
            RecoveryDecision::RetryAt {
                not_before_ms: 1_000 + delay,
                next_attempt: attempt + 1,
                effect_id: None,
            }
        );
    }
    assert_eq!(
        decide(&policy, transient(), 5, 1_000),
        RecoveryDecision::Stop(StopReason::AttemptsExhausted)
    );
    let policy = retry_policy(16);
    assert_eq!(
        decide(&policy, transient(), 15, 1_000),
        RecoveryDecision::RetryAt {
            not_before_ms: 1_500,
            next_attempt: 16,
            effect_id: None
        }
    );
}

#[test]
fn delay_and_attempt_duration_consume_elapsed_budget() {
    let policy = NodeRecoveryPolicy::admit(
        3,
        BTreeSet::from([RetryCondition::DependencyUnavailable]),
        Some(NodeBackoff {
            initial_ms: 100,
            maximum_ms: 200,
            multiplier: 2,
        }),
        Some(500),
        None,
    )
    .unwrap();
    assert!(matches!(
        decide(&policy, transient(), 1, 1_399),
        RecoveryDecision::RetryAt {
            not_before_ms: 1_499,
            ..
        }
    ));
    assert_eq!(
        decide(&policy, transient(), 1, 1_400),
        RecoveryDecision::Stop(StopReason::ElapsedLimit)
    );
    assert_eq!(
        decide(&policy, transient(), 1, 1_501),
        RecoveryDecision::Stop(StopReason::ElapsedLimit)
    );
}

#[test]
fn denial_cancel_and_permanent_failures_cannot_be_retried() {
    let policy = retry_policy(16);
    for class in [
        NodeFailureClass::InvalidConfiguration,
        NodeFailureClass::InvalidInput,
        NodeFailureClass::InvalidResult,
        NodeFailureClass::ModelOutputIncomplete,
        NodeFailureClass::AuthenticationDenied,
        NodeFailureClass::AuthorizationDenied,
        NodeFailureClass::SensitiveRejected,
        NodeFailureClass::Unknown,
    ] {
        assert_eq!(
            decide(
                &policy,
                NodeFailure {
                    class,
                    replay: ReplaySafety::NoExternalEffect
                },
                1,
                1_000
            ),
            RecoveryDecision::Stop(StopReason::NotRetryable)
        );
    }
    for class in [NodeFailureClass::Cancelled, NodeFailureClass::LeaseLost] {
        for replay in [
            ReplaySafety::NoExternalEffect,
            ReplaySafety::Unclassified,
            ReplaySafety::UnknownExternalEffect { effect_id: EFFECT },
            ReplaySafety::CompletedExternalEffect {
                receipt_id: RECEIPT,
            },
        ] {
            assert_eq!(
                decide(&policy, NodeFailure { class, replay }, 1, 1_000),
                RecoveryDecision::Stop(StopReason::ControlDecision)
            );
        }
    }
}

#[test]
fn missing_failure_condition_does_not_become_a_general_retry() {
    let policy = NodeRecoveryPolicy::admit(
        3,
        BTreeSet::from([RetryCondition::RateLimited]),
        Some(NodeBackoff {
            initial_ms: 100,
            maximum_ms: 100,
            multiplier: 1,
        }),
        Some(1000),
        None,
    )
    .unwrap();
    assert_eq!(
        decide(&policy, transient(), 1, 1_000),
        RecoveryDecision::Stop(StopReason::NotRetryable)
    );
    assert!(matches!(
        decide(
            &policy,
            NodeFailure {
                class: NodeFailureClass::RateLimited,
                replay: ReplaySafety::NoExternalEffect
            },
            1,
            1_000
        ),
        RecoveryDecision::RetryAt {
            next_attempt: 2,
            ..
        }
    ));
}

#[test]
fn cancellation_during_backoff_preserves_failed_attempt_and_stops_dispatch() {
    let pending = NodeAttemptLedger::new(ACTIVATION, retry_policy(3))
        .unwrap()
        .start_attempt(1_000)
        .unwrap()
        .record_failure(transient(), 1_010)
        .unwrap();
    let stopped = pending
        .record_control_stop(NodeFailureClass::Cancelled, 1_020)
        .unwrap();
    assert_eq!(stopped.history(), pending.history());
    assert_eq!(
        stopped.phase(),
        NodeAttemptPhase::ControlStopped {
            class: NodeFailureClass::Cancelled,
            observed_ms: 1_020
        }
    );
    assert_eq!(
        stopped.start_attempt(1_110),
        Err(NodeRecoveryError::InvalidTransition)
    );
    let request = OperatorRetryRequest {
        request_id: [5; 32],
        activation_id: ACTIVATION,
        expected_revision: stopped.revision(),
    };
    assert_eq!(
        stopped.operator_retry(request, 1_021),
        Err(NodeRecoveryError::InvalidTransition)
    );
}

#[test]
fn lease_loss_during_running_attempt_records_failure_without_success() {
    let started = NodeAttemptLedger::new(ACTIVATION, retry_policy(3))
        .unwrap()
        .start_attempt(1_000)
        .unwrap();
    let stopped = started
        .record_control_stop(NodeFailureClass::LeaseLost, 1_001)
        .unwrap();
    assert!(matches!(
        stopped.history()[0].outcome,
        AttemptOutcome::Failed {
            failure: NodeFailure {
                class: NodeFailureClass::LeaseLost,
                ..
            },
            decision: RecoveryDecision::Stop(StopReason::ControlDecision)
        }
    ));
    assert_eq!(
        stopped.record_success(RECEIPT, 1_002),
        Err(NodeRecoveryError::InvalidTransition)
    );
}

#[test]
fn separate_branch_or_item_activations_keep_attempts_and_completion_isolated() {
    let complete = NodeAttemptLedger::new(ACTIVATION, retry_policy(3))
        .unwrap()
        .start_attempt(1_000)
        .unwrap()
        .record_success(RECEIPT, 1_001)
        .unwrap();
    let failing = NodeAttemptLedger::new([7; 32], retry_policy(3))
        .unwrap()
        .start_attempt(1_000)
        .unwrap()
        .record_failure(transient(), 1_001)
        .unwrap();
    let request = OperatorRetryRequest {
        request_id: [5; 32],
        activation_id: ACTIVATION,
        expected_revision: failing.revision(),
    };
    assert_eq!(
        failing.operator_retry(request, 1_002),
        Err(NodeRecoveryError::StaleOperatorRequest)
    );
    let resumed = failing.start_attempt(1_101).unwrap();
    assert_eq!(resumed.history().len(), 2);
    assert_eq!(complete.history().len(), 1);
    assert_eq!(
        complete.phase(),
        NodeAttemptPhase::Completed {
            receipt_id: RECEIPT
        }
    );
}

#[test]
fn unknown_or_completed_effects_require_reconciliation_before_retry_or_route() {
    let route = route_for(&[NodeFailureClass::DependencyUnavailable], false);
    let mut policy = retry_policy(3);
    policy.error_route = Some(route);
    for (replay, expected) in [
        (
            ReplaySafety::UnknownExternalEffect { effect_id: EFFECT },
            RecoveryDecision::Reconcile {
                effect_id: Some(EFFECT),
                completed_receipt: None,
            },
        ),
        (
            ReplaySafety::CompletedExternalEffect {
                receipt_id: RECEIPT,
            },
            RecoveryDecision::Reconcile {
                effect_id: None,
                completed_receipt: Some(RECEIPT),
            },
        ),
        (
            ReplaySafety::Unclassified,
            RecoveryDecision::Reconcile {
                effect_id: None,
                completed_receipt: None,
            },
        ),
    ] {
        assert_eq!(
            decide(
                &policy,
                NodeFailure {
                    class: NodeFailureClass::DependencyUnavailable,
                    replay
                },
                1,
                1_000
            ),
            expected
        );
    }
}

#[test]
fn known_uncommitted_idempotent_effect_keeps_the_original_effect_identity() {
    let failure = NodeFailure {
        class: NodeFailureClass::AttemptTimeout,
        replay: ReplaySafety::IdempotentEffectNotCommitted { effect_id: EFFECT },
    };
    assert_eq!(
        decide(&retry_policy(3), failure, 1, 1_000),
        RecoveryDecision::RetryAt {
            not_before_ms: 1_100,
            next_attempt: 2,
            effect_id: Some(EFFECT)
        }
    );
    let ledger = NodeAttemptLedger::new(ACTIVATION, retry_policy(3))
        .unwrap()
        .start_attempt(1_000)
        .unwrap()
        .record_failure(failure, 1_001)
        .unwrap()
        .start_attempt(1_101)
        .unwrap();
    for replay in [
        ReplaySafety::NoExternalEffect,
        ReplaySafety::IdempotentEffectNotCommitted { effect_id: [8; 32] },
        ReplaySafety::UnknownExternalEffect { effect_id: [8; 32] },
    ] {
        assert_eq!(
            ledger.record_failure(
                NodeFailure {
                    class: failure.class,
                    replay
                },
                1_102
            ),
            Err(NodeRecoveryError::InvalidEffectIdentity)
        );
    }
    assert!(ledger.record_failure(failure, 1_102).is_ok());
}

#[test]
fn unsafe_error_route_contracts_fail_admission() {
    let valid = ErrorRouteContract {
        dedicated_typed_error_input: true,
        requires_success_output: false,
        reexecutes_failed_operation: false,
        explicitly_handles_denial: true,
    };
    for contract in [
        ErrorRouteContract {
            dedicated_typed_error_input: false,
            ..valid
        },
        ErrorRouteContract {
            requires_success_output: true,
            ..valid
        },
        ErrorRouteContract {
            reexecutes_failed_operation: true,
            ..valid
        },
        ErrorRouteContract {
            explicitly_handles_denial: false,
            ..valid
        },
    ] {
        assert_eq!(
            NodeErrorRoute::admit(
                ROUTE,
                BTreeSet::from([NodeFailureClass::AuthorizationDenied]),
                contract
            ),
            Err(NodeRecoveryError::InvalidErrorRoute)
        );
    }
    for class in [NodeFailureClass::Cancelled, NodeFailureClass::LeaseLost] {
        assert_eq!(
            NodeErrorRoute::admit(ROUTE, BTreeSet::from([class]), valid),
            Err(NodeRecoveryError::InvalidErrorRoute)
        );
    }
}

#[test]
fn explicitly_admitted_denial_route_contains_only_the_failed_node_envelope() {
    let route = route_for(&[NodeFailureClass::AuthorizationDenied], true);
    let policy = NodeRecoveryPolicy::admit(1, BTreeSet::new(), None, None, Some(route)).unwrap();
    let failure = NodeFailure {
        class: NodeFailureClass::AuthorizationDenied,
        replay: ReplaySafety::NoExternalEffect,
    };
    let ledger = NodeAttemptLedger::new(ACTIVATION, policy)
        .unwrap()
        .start_attempt(1_000)
        .unwrap()
        .record_failure(failure, 1_001)
        .unwrap();
    assert_eq!(
        ledger.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::ErrorRoute {
            route_id: ROUTE,
            failed: FailedNodeOutput {
                activation_id: ACTIVATION,
                attempt: 1,
                class: failure.class,
                reason: StopReason::RetryDisabled
            },
        })
    );
    assert!(matches!(
        ledger.history()[0].outcome,
        AttemptOutcome::Failed { .. }
    ));
    assert_eq!(
        ledger.start_attempt(1_002),
        Err(NodeRecoveryError::InvalidTransition)
    );
}

#[test]
fn exhausted_retry_can_route_typed_error_while_preserving_each_failed_attempt() {
    let mut policy = retry_policy(2);
    policy.error_route = Some(route_for(&[NodeFailureClass::DependencyUnavailable], false));
    let ledger = NodeAttemptLedger::new(ACTIVATION, policy)
        .unwrap()
        .start_attempt(1_000)
        .unwrap()
        .record_failure(transient(), 1_010)
        .unwrap()
        .start_attempt(1_110)
        .unwrap()
        .record_failure(transient(), 1_120)
        .unwrap();
    assert_eq!(ledger.history().len(), 2);
    assert!(
        ledger
            .history()
            .iter()
            .all(|record| matches!(record.outcome, AttemptOutcome::Failed { .. }))
    );
    assert!(matches!(
        ledger.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::ErrorRoute {
            failed: FailedNodeOutput {
                attempt: 2,
                reason: StopReason::AttemptsExhausted,
                ..
            },
            ..
        })
    ));
}

#[test]
fn backoff_restore_keeps_exact_deadline_and_spent_attempts() {
    let started = NodeAttemptLedger::new(ACTIVATION, retry_policy(3))
        .unwrap()
        .start_attempt(1_000)
        .unwrap();
    let saved = started.record_failure(transient(), 1_050).unwrap();
    let restored = saved.clone();
    assert_eq!(
        restored.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::RetryAt {
            not_before_ms: 1_150,
            next_attempt: 2,
            effect_id: None
        })
    );
    assert_eq!(
        restored.start_attempt(1_149),
        Err(NodeRecoveryError::InvalidTransition)
    );
    let second = restored.start_attempt(1_150).unwrap();
    assert_eq!(second.history()[1].attempt, 2);
    let restored_second = second.clone();
    let failed = restored_second.record_failure(transient(), 1_151).unwrap();
    assert!(matches!(
        failed.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::RetryAt {
            not_before_ms: 1_351,
            next_attempt: 3,
            ..
        })
    ));
}

#[test]
fn started_attempt_recovery_consumes_its_original_budget_and_reconciles_effects() {
    let started = NodeAttemptLedger::new(ACTIVATION, retry_policy(2))
        .unwrap()
        .start_attempt(1_000)
        .unwrap();
    assert_eq!(
        started.start_attempt(1_001),
        Err(NodeRecoveryError::InvalidTransition)
    );
    let recovered = started
        .clone()
        .recover_interrupted(
            ReplaySafety::UnknownExternalEffect { effect_id: EFFECT },
            1_001,
        )
        .unwrap();
    assert_eq!(recovered.history().len(), 1);
    assert_eq!(
        recovered.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::Reconcile {
            effect_id: Some(EFFECT),
            completed_receipt: None
        })
    );
    assert_eq!(
        recovered.start_attempt(1_002),
        Err(NodeRecoveryError::InvalidTransition)
    );
    let safe = started
        .recover_interrupted(ReplaySafety::NoExternalEffect, 1_001)
        .unwrap();
    assert!(matches!(
        safe.phase(),
        NodeAttemptPhase::Failed(RecoveryDecision::RetryAt {
            next_attempt: 2,
            ..
        })
    ));
}

#[test]
fn completed_node_receipt_is_reused_without_a_new_attempt() {
    let completed = NodeAttemptLedger::new(ACTIVATION, retry_policy(3))
        .unwrap()
        .start_attempt(1_000)
        .unwrap()
        .record_success(RECEIPT, 1_010)
        .unwrap();
    let restored = completed.clone();
    assert_eq!(
        restored.phase(),
        NodeAttemptPhase::Completed {
            receipt_id: RECEIPT
        }
    );
    assert_eq!(restored.history().len(), 1);
    assert_eq!(
        restored.start_attempt(1_011),
        Err(NodeRecoveryError::InvalidTransition)
    );
    assert_eq!(
        restored.record_failure(transient(), 1_011),
        Err(NodeRecoveryError::InvalidTransition)
    );
}

#[test]
fn timestamps_and_identity_overflow_fail_closed() {
    let policy = retry_policy(3);
    assert_eq!(
        plan_after_failure(&policy, ACTIVATION, 1, 1_000, 999, transient()),
        Err(NodeRecoveryError::InvalidClock)
    );
    assert_eq!(
        plan_after_failure(
            &policy,
            ACTIVATION,
            1,
            u64::MAX - 10,
            u64::MAX - 1,
            transient()
        ),
        Err(NodeRecoveryError::InvalidClock)
    );
    assert_eq!(
        plan_after_failure(&policy, [0; 32], 1, 1_000, 1_001, transient()),
        Err(NodeRecoveryError::InvalidHistory)
    );
    for replay in [
        ReplaySafety::IdempotentEffectNotCommitted { effect_id: [0; 32] },
        ReplaySafety::UnknownExternalEffect { effect_id: [0; 32] },
        ReplaySafety::CompletedExternalEffect {
            receipt_id: [0; 32],
        },
    ] {
        assert_eq!(
            plan_after_failure(
                &policy,
                ACTIVATION,
                1,
                1_000,
                1_001,
                NodeFailure {
                    class: NodeFailureClass::AttemptTimeout,
                    replay
                }
            ),
            Err(NodeRecoveryError::InvalidEffectIdentity)
        );
    }
}

#[test]
fn operator_retry_is_scoped_audited_idempotent_and_keeps_budget() {
    let ledger = NodeAttemptLedger::new(ACTIVATION, retry_policy(2))
        .unwrap()
        .start_attempt(1_000)
        .unwrap()
        .record_failure(transient(), 1_010)
        .unwrap();
    let request = OperatorRetryRequest {
        request_id: [5; 32],
        activation_id: ACTIVATION,
        expected_revision: ledger.revision(),
    };
    let recovered = ledger.operator_retry(request, 1_011).unwrap();
    assert_eq!(
        recovered.operator_audit(),
        &[OperatorRetryAudit {
            request,
            requested_ms: 1_011
        }]
    );
    assert_eq!(recovered.operator_retry(request, 1_012).unwrap(), recovered);
    let second = recovered
        .start_attempt(1_110)
        .unwrap()
        .record_failure(transient(), 1_111)
        .unwrap();
    assert_eq!(second.history().len(), 2);
    assert_eq!(
        second.operator_retry(
            OperatorRetryRequest {
                request_id: [6; 32],
                expected_revision: second.revision(),
                ..request
            },
            1_112
        ),
        Err(NodeRecoveryError::OperatorRetryDenied)
    );
    assert_eq!(
        ledger.operator_retry(
            OperatorRetryRequest {
                activation_id: [9; 32],
                ..request
            },
            1_011
        ),
        Err(NodeRecoveryError::StaleOperatorRequest)
    );
    assert_eq!(
        ledger.operator_retry(
            OperatorRetryRequest {
                expected_revision: ledger.revision() + 1,
                ..request
            },
            1_011
        ),
        Err(NodeRecoveryError::StaleOperatorRequest)
    );
    assert_eq!(
        recovered.operator_retry(
            OperatorRetryRequest {
                activation_id: [9; 32],
                ..request
            },
            1_011
        ),
        Err(NodeRecoveryError::StaleOperatorRequest)
    );
}

#[test]
fn operator_cannot_override_denial_cancel_lease_or_unknown_effects() {
    for class in [
        NodeFailureClass::AuthorizationDenied,
        NodeFailureClass::AuthenticationDenied,
        NodeFailureClass::SensitiveRejected,
        NodeFailureClass::Cancelled,
        NodeFailureClass::LeaseLost,
    ] {
        let ledger = NodeAttemptLedger::new(ACTIVATION, retry_policy(3))
            .unwrap()
            .start_attempt(1_000)
            .unwrap()
            .record_failure(
                NodeFailure {
                    class,
                    replay: ReplaySafety::NoExternalEffect,
                },
                1_001,
            )
            .unwrap();
        let request = OperatorRetryRequest {
            request_id: [5; 32],
            activation_id: ACTIVATION,
            expected_revision: ledger.revision(),
        };
        assert_eq!(
            ledger.operator_retry(request, 1_002),
            Err(NodeRecoveryError::OperatorRetryDenied)
        );
    }
    let ledger = NodeAttemptLedger::new(ACTIVATION, retry_policy(3))
        .unwrap()
        .start_attempt(1_000)
        .unwrap()
        .record_failure(
            NodeFailure {
                class: NodeFailureClass::AttemptTimeout,
                replay: ReplaySafety::UnknownExternalEffect { effect_id: EFFECT },
            },
            1_001,
        )
        .unwrap();
    let request = OperatorRetryRequest {
        request_id: [5; 32],
        activation_id: ACTIVATION,
        expected_revision: ledger.revision(),
    };
    assert_eq!(
        ledger.operator_retry(request, 1_002),
        Err(NodeRecoveryError::InvalidTransition)
    );
}
