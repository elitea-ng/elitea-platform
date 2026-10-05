//! Versioned node attempt records. Decode replays all admitted transitions.

use serde::{Deserialize, Serialize};
use serde_json::Value;

use super::{
    NodeAttemptLedger, NodeFailure, NodeFailureClass, NodeRecoveryError, NodeRecoveryPolicy,
    OperatorRetryRequest,
};

pub(crate) const MAX_RECOVERY_EVENTS: usize = 64;
pub(crate) const MAX_NODE_RECOVERY_BYTES: usize = 128 * 1024;
const SCHEMA: &str = "elitea.pipeline.node-recovery.v1";
const SCHEMA_V2: &str = "elitea.pipeline.node-recovery.v2";

#[derive(Clone, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub(crate) enum NodeRecoveryEvent {
    Start {
        now_ms: u64,
    },
    Failure {
        failure: NodeFailure,
        now_ms: u64,
    },
    Success {
        receipt_id: [u8; 32],
        now_ms: u64,
    },
    ControlStop {
        class: NodeFailureClass,
        now_ms: u64,
    },
    OperatorRetry {
        request: OperatorRetryRequest,
        now_ms: u64,
    },
    WaitExpired {
        now_ms: u64,
    },
    OwnerResultResumed {
        request: OperatorRetryRequest,
        owner_effect_id: [u8; 32],
        owner_receipt_sha256: [u8; 32],
        result_receipt_id: [u8; 32],
        now_ms: u64,
    },
    NoEffectReconciled {
        request: OperatorRetryRequest,
        owner_effect_id: [u8; 32],
        owner_receipt_sha256: [u8; 32],
        attempt: u16,
        now_ms: u64,
    },
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct StoredLedger {
    schema: String,
    ledger: NodeAttemptLedger,
}

pub(crate) fn encode(ledger: &NodeAttemptLedger) -> Result<Value, NodeRecoveryError> {
    validate(ledger)?;
    let value = serde_json::to_value(StoredLedger {
        schema: if ledger
            .events
            .iter()
            .any(|event| matches!(event, NodeRecoveryEvent::NoEffectReconciled { .. }))
        {
            SCHEMA_V2
        } else {
            SCHEMA
        }
        .to_owned(),
        ledger: ledger.clone(),
    })
    .map_err(|_| NodeRecoveryError::InvalidHistory)?;
    if serde_json::to_vec(&value)
        .map_err(|_| NodeRecoveryError::InvalidHistory)?
        .len()
        > MAX_NODE_RECOVERY_BYTES
    {
        return Err(NodeRecoveryError::InvalidHistory);
    }
    Ok(value)
}

pub(crate) fn decode(
    value: &Value,
    expected_activation: [u8; 32],
    expected_policy: &NodeRecoveryPolicy,
) -> Result<NodeAttemptLedger, NodeRecoveryError> {
    if serde_json::to_vec(value)
        .map_err(|_| NodeRecoveryError::InvalidHistory)?
        .len()
        > MAX_NODE_RECOVERY_BYTES
    {
        return Err(NodeRecoveryError::InvalidHistory);
    }
    let stored: StoredLedger =
        serde_json::from_value(value.clone()).map_err(|_| NodeRecoveryError::InvalidHistory)?;
    let requires_v2 = stored
        .ledger
        .events
        .iter()
        .any(|event| matches!(event, NodeRecoveryEvent::NoEffectReconciled { .. }));
    if stored.schema != (if requires_v2 { SCHEMA_V2 } else { SCHEMA })
        || stored.ledger.activation_id != expected_activation
        || stored.ledger.policy != *expected_policy
    {
        return Err(NodeRecoveryError::InvalidHistory);
    }
    validate(&stored.ledger)?;
    Ok(stored.ledger)
}

fn validate(ledger: &NodeAttemptLedger) -> Result<(), NodeRecoveryError> {
    if ledger.events.len() > MAX_RECOVERY_EVENTS {
        return Err(NodeRecoveryError::InvalidHistory);
    }
    let policy = NodeRecoveryPolicy::admit(
        ledger.policy.max_attempts,
        ledger.policy.retry_on.clone(),
        ledger.policy.backoff,
        ledger.policy.max_elapsed_ms,
        ledger.policy.error_route.clone(),
    )?
    .with_retry_mode(ledger.policy.retry_mode)?;
    let mut replay = NodeAttemptLedger::new(ledger.activation_id, policy)?;
    for event in &ledger.events {
        replay = apply_event(&replay, event)?;
    }
    if replay != *ledger {
        return Err(NodeRecoveryError::InvalidHistory);
    }
    Ok(())
}

fn apply_event(
    ledger: &NodeAttemptLedger,
    event: &NodeRecoveryEvent,
) -> Result<NodeAttemptLedger, NodeRecoveryError> {
    match *event {
        NodeRecoveryEvent::Start { now_ms } => ledger.start_attempt(now_ms),
        NodeRecoveryEvent::Failure { failure, now_ms } => ledger.record_failure(failure, now_ms),
        NodeRecoveryEvent::Success { receipt_id, now_ms } => {
            ledger.record_success(receipt_id, now_ms)
        }
        NodeRecoveryEvent::ControlStop { class, now_ms } => {
            ledger.record_control_stop(class, now_ms)
        }
        NodeRecoveryEvent::OperatorRetry { request, now_ms } => {
            ledger.operator_retry(request, now_ms)
        }
        NodeRecoveryEvent::WaitExpired { now_ms } => ledger
            .expire_wait(now_ms)?
            .ok_or(NodeRecoveryError::InvalidTransition),
        NodeRecoveryEvent::OwnerResultResumed {
            request,
            owner_effect_id,
            owner_receipt_sha256,
            result_receipt_id,
            now_ms,
        } => ledger.resume_owner_result(
            request,
            owner_effect_id,
            owner_receipt_sha256,
            result_receipt_id,
            now_ms,
        ),
        NodeRecoveryEvent::NoEffectReconciled {
            request,
            owner_effect_id,
            owner_receipt_sha256,
            attempt,
            now_ms,
        } => {
            if ledger
                .history
                .last()
                .is_none_or(|last| last.attempt != attempt)
            {
                return Err(NodeRecoveryError::InvalidHistory);
            }
            ledger.reconcile_verified_no_effect(
                request,
                owner_effect_id,
                owner_receipt_sha256,
                now_ms,
            )
        }
    }
}

/// Restore a receipt's immutable revision without trusting serialized history.
pub(crate) fn at_revision(
    ledger: &NodeAttemptLedger,
    revision: u64,
) -> Result<NodeAttemptLedger, NodeRecoveryError> {
    validate(ledger)?;
    let mut replay = NodeAttemptLedger::new(ledger.activation_id, ledger.policy.clone())?;
    for event in &ledger.events {
        if replay.revision() == revision {
            return Ok(replay);
        }
        replay = apply_event(&replay, event)?;
    }
    if replay.revision() == revision {
        Ok(replay)
    } else {
        Err(NodeRecoveryError::InvalidHistory)
    }
}

pub(crate) fn is_owner_result_replay(
    ledger: &NodeAttemptLedger,
    request: OperatorRetryRequest,
    owner_effect_id: [u8; 32],
    owner_receipt_sha256: [u8; 32],
    result_receipt_id: [u8; 32],
) -> Result<bool, NodeRecoveryError> {
    validate(ledger)?;
    Ok(
        request.expected_revision.checked_add(1) == Some(ledger.revision)
            && matches!(ledger.events.last(),Some(NodeRecoveryEvent::OwnerResultResumed{request:prior,owner_effect_id:effect,owner_receipt_sha256:owner,result_receipt_id:result,..})
            if *prior==request && *effect==owner_effect_id && *owner==owner_receipt_sha256 && *result==result_receipt_id),
    )
}
pub(crate) fn is_no_effect_replay(
    ledger: &NodeAttemptLedger,
    request: OperatorRetryRequest,
    owner_effect_id: [u8; 32],
    owner_receipt_sha256: [u8; 32],
) -> Result<bool, NodeRecoveryError> {
    validate(ledger)?;
    Ok(
        request.expected_revision.checked_add(1) == Some(ledger.revision)
            && matches!(ledger.events.last(),Some(NodeRecoveryEvent::NoEffectReconciled {
            request: prior, owner_effect_id: effect, owner_receipt_sha256: owner, ..
        }) if *prior == request && *effect == owner_effect_id && *owner == owner_receipt_sha256),
    )
}

pub(crate) fn before_operator_request(
    ledger: &NodeAttemptLedger,
    request: OperatorRetryRequest,
) -> Result<Option<NodeAttemptLedger>, NodeRecoveryError> {
    validate(ledger)?;
    let mut replay = NodeAttemptLedger::new(ledger.activation_id, ledger.policy.clone())?;
    for event in &ledger.events {
        if matches!(event, NodeRecoveryEvent::OperatorRetry { request: prior, .. } if *prior == request)
        {
            return Ok(Some(replay));
        }
        replay = apply_event(&replay, event)?;
    }
    Ok(None)
}

pub(crate) fn require_successor(
    parent: &NodeAttemptLedger,
    next: &NodeAttemptLedger,
) -> Result<(), NodeRecoveryError> {
    validate(parent)?;
    validate(next)?;
    if parent.activation_id != next.activation_id
        || parent.policy != next.policy
        || next.revision
            != parent
                .revision
                .checked_add(1)
                .ok_or(NodeRecoveryError::InvalidHistory)?
        || next.events.len() != parent.events.len() + 1
        || !next.events.starts_with(&parent.events)
    {
        return Err(NodeRecoveryError::InvalidHistory);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::agents::graph::node_recovery::{
        NodeAttemptPhase, NodeBackoff, ReplaySafety, RetryCondition,
    };
    use std::collections::BTreeSet;

    fn policy() -> NodeRecoveryPolicy {
        NodeRecoveryPolicy::admit(
            3,
            BTreeSet::from([RetryCondition::RateLimited]),
            Some(NodeBackoff {
                initial_ms: 10,
                maximum_ms: 100,
                multiplier: 2,
            }),
            Some(1000),
            None,
        )
        .unwrap()
    }

    #[test]
    fn node_recovery_codec_roundtrip_preserves_exact_backoff_and_budget() {
        let policy = policy();
        let ledger = NodeAttemptLedger::new([1; 32], policy.clone())
            .unwrap()
            .start_attempt(100)
            .unwrap()
            .record_failure(
                NodeFailure {
                    class: NodeFailureClass::RateLimited,
                    replay: ReplaySafety::NoExternalEffect,
                },
                101,
            )
            .unwrap();
        let raw = encode(&ledger).unwrap();
        let recovered = decode(&raw, [1; 32], &policy).unwrap();
        assert_eq!(recovered, ledger);
        assert!(matches!(
            recovered.phase(),
            NodeAttemptPhase::Failed(super::super::RecoveryDecision::RetryAt {
                not_before_ms: 111,
                next_attempt: 2,
                ..
            })
        ));
        assert!(decode(&raw, [2; 32], &policy).is_err());
        assert!(decode(&raw, [1; 32], &NodeRecoveryPolicy::default()).is_err());
    }

    #[test]
    fn node_recovery_codec_rejects_rewritten_budget_phase_history_and_events() {
        let policy = policy();
        let ledger = NodeAttemptLedger::new([1; 32], policy.clone())
            .unwrap()
            .start_attempt(100)
            .unwrap();
        for mutation in [
            "revision", "phase", "history", "events", "schema", "foreign",
        ] {
            let mut raw = encode(&ledger).unwrap();
            match mutation {
                "revision" => raw["ledger"]["revision"] = serde_json::json!(90),
                "phase" => raw["ledger"]["phase"] = serde_json::json!("Ready"),
                "history" => raw["ledger"]["history"] = serde_json::json!([]),
                "events" => raw["ledger"]["events"] = serde_json::json!([]),
                "schema" => raw["schema"] = serde_json::json!("future"),
                _ => raw["foreign"] = serde_json::json!(true),
            }
            assert!(decode(&raw, [1; 32], &policy).is_err(), "{mutation}");
        }
    }

    #[test]
    fn node_recovery_codec_immutable_append_rejects_history_replacement() {
        let first = NodeAttemptLedger::new([1; 32], policy()).unwrap();
        let started = first.start_attempt(100).unwrap();
        require_successor(&first, &started).unwrap();
        assert!(require_successor(&started, &first).is_err());
        let replacement = NodeAttemptLedger::new([2; 32], policy())
            .unwrap()
            .start_attempt(100)
            .unwrap();
        assert!(require_successor(&first, &replacement).is_err());
    }
}

/// Strict public identity form. No uppercase, short, or zero identities.
pub(crate) mod hex_id {
    use serde::{Deserialize, Deserializer, Serializer};
    pub(crate) fn serialize<S: Serializer>(
        value: &[u8; 32],
        serializer: S,
    ) -> Result<S::Ok, S::Error> {
        use std::fmt::Write;
        let mut text = String::with_capacity(64);
        for byte in value {
            let _ = write!(text, "{byte:02x}");
        }
        serializer.serialize_str(&text)
    }
    pub(crate) fn deserialize<'de, D: Deserializer<'de>>(
        deserializer: D,
    ) -> Result<[u8; 32], D::Error> {
        let text = String::deserialize(deserializer)?;
        if text.len() != 64
            || !text
                .bytes()
                .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
        {
            return Err(serde::de::Error::custom("invalid recovery identity"));
        }
        let mut value = [0; 32];
        for (index, part) in text.as_bytes().chunks_exact(2).enumerate() {
            let digit = |c: u8| if c <= b'9' { c - b'0' } else { c - b'a' + 10 };
            value[index] = (digit(part[0]) << 4) | digit(part[1]);
        }
        if value == [0; 32] {
            return Err(serde::de::Error::custom("invalid recovery identity"));
        }
        Ok(value)
    }
}
