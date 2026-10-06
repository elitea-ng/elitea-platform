//! Controlled admission states. No database or runtime access occurs.
use super::*;
fn reserved() -> PlatformAdmissionState {
    PlatformAdmissionState {
        digest: vec![7; 32],
        phase: Phase::Reserved,
        was_dispatched: false,
        cancelled: false,
        has_receipt: false,
        running_owner: false,
    }
}
#[test]
fn absence_and_reserved_are_only_read_only_admission_observations() {
    assert!(admission_not_ready(None, &[7; 32], true).unwrap());
    assert!(admission_not_ready(None, &[7; 32], false).is_err());
    assert!(admission_not_ready(Some(&reserved()), &[7; 32], true).unwrap());
    assert!(admission_not_ready(Some(&reserved()), &[7; 32], false).is_err());
}
#[test]
fn conflict_cancellation_dispatch_history_and_terminal_states_never_become_not_ready() {
    let state = reserved();
    assert!(matches!(
        admission_not_ready(Some(&state), &[8; 32], true),
        Err(LedgerError::Conflict)
    ));
    for changed in [
        PlatformAdmissionState {
            cancelled: true,
            ..reserved()
        },
        PlatformAdmissionState {
            has_receipt: true,
            ..reserved()
        },
        PlatformAdmissionState {
            was_dispatched: true,
            ..reserved()
        },
        PlatformAdmissionState {
            phase: Phase::Completed,
            ..reserved()
        },
        PlatformAdmissionState {
            phase: Phase::Failed,
            ..reserved()
        },
        PlatformAdmissionState {
            phase: Phase::Cancelled,
            ..reserved()
        },
        PlatformAdmissionState {
            phase: Phase::Uncertain,
            ..reserved()
        },
    ] {
        assert!(matches!(
            admission_not_ready(Some(&changed), &[7; 32], true),
            Err(LedgerError::Fenced)
        ));
    }
}
#[test]
fn dispatched_owner_requires_live_lease_and_keeps_the_strict_runtime_path() {
    let mut state = PlatformAdmissionState {
        phase: Phase::Dispatched,
        was_dispatched: true,
        running_owner: true,
        ..reserved()
    };
    assert!(!admission_not_ready(Some(&state), &[7; 32], false).unwrap());
    state.running_owner = false;
    assert!(admission_not_ready(Some(&state), &[7; 32], true).is_err());
    state.running_owner = true;
    state.was_dispatched = false;
    assert!(admission_not_ready(Some(&state), &[7; 32], true).is_err());
}

fn completed() -> PlatformAdmissionState {
    PlatformAdmissionState {
        phase: Phase::Completed,
        was_dispatched: true,
        ..reserved()
    }
}
fn result() -> String {
    serde_json::json!({
        "revision":1, "status":"completed", "exit_code":0,
        "stdout":r#"{"revision":1,"result":{"status":"PASS"}}"#,
        "stderr":"Loading micropip\nLoaded micropip"
    })
    .to_string()
}
#[test]
fn completed_observation_requires_dispatch_and_exact_authority_without_live_runtime() {
    let result = result();
    assert!(completed_observation(Some(&completed()), &[7; 32], true, Some(&result)).unwrap());
    // Completion requires no sealed recovery receipt or live owner lease.
    let state = PlatformAdmissionState {
        has_receipt: true,
        ..completed()
    };
    assert!(completed_observation(Some(&state), &[7; 32], true, Some(&result)).unwrap());
    assert!(matches!(
        completed_observation(Some(&state), &[8; 32], true, Some(&result)),
        Err(LedgerError::Conflict)
    ));
    assert!(completed_observation(Some(&state), &[7; 32], false, Some(&result)).is_err());
    for state in [
        PlatformAdmissionState {
            cancelled: true,
            ..completed()
        },
        PlatformAdmissionState {
            was_dispatched: false,
            ..completed()
        },
    ] {
        assert!(completed_observation(Some(&state), &[7; 32], true, Some(&result)).is_err());
    }
}
#[test]
fn absent_failed_cancelled_uncertain_and_inflight_rows_never_observe_completion() {
    let result = result();
    assert!(!completed_observation(None, &[7; 32], true, Some(&result)).unwrap());
    for phase in [
        Phase::Reserved,
        Phase::Dispatched,
        Phase::Failed,
        Phase::Cancelled,
        Phase::Uncertain,
    ] {
        let state = PlatformAdmissionState {
            phase,
            ..completed()
        };
        assert!(!completed_observation(Some(&state), &[7; 32], true, Some(&result)).unwrap());
    }
}
#[test]
fn completed_observation_refuses_missing_malformed_failed_and_oversized_results() {
    let original: serde_json::Value = serde_json::from_str(&result()).unwrap();
    let mut invalid = vec![
        "{}".to_owned(),
        "null".to_owned(),
        "[0]".to_owned(),
        "{".to_owned(),
    ];
    for (field, replacement) in [
        ("revision", serde_json::json!(2)),
        ("status", serde_json::json!("failed")),
        ("exit_code", serde_json::json!(1)),
        ("exit_code", serde_json::Value::Null),
        ("stdout", serde_json::json!(r#"{"revision":2,"result":{}}"#)),
        (
            "stdout",
            serde_json::json!(r#"{"revision":1,"result":{},"runtime_id":"chosen"}"#),
        ),
        ("stdout", serde_json::json!("x".repeat(256 * 1024 + 65))),
        ("stderr", serde_json::json!("x".repeat(512 * 1024))),
        ("runtime_id", serde_json::json!("chosen")),
    ] {
        let mut value = original.clone();
        value[field] = replacement;
        invalid.push(value.to_string());
    }
    assert!(completed_observation(Some(&completed()), &[7; 32], true, None).is_err());
    for result in invalid {
        assert!(completed_observation(Some(&completed()), &[7; 32], true, Some(&result)).is_err());
    }
}
#[test]
fn fresh_completed_recheck_closes_cleanup_race_but_no_other_change_closes_it() {
    let result = result();
    let initial = PlatformAdmissionState {
        phase: Phase::Dispatched,
        running_owner: true,
        ..completed()
    };
    assert!(!admission_not_ready(Some(&initial), &[7; 32], false).unwrap());
    assert!(!completed_observation(Some(&initial), &[7; 32], true, Some(&result)).unwrap());
    // The runtime read fails after normal completion commits and cleanup starts.
    // A fresh observation can close the read without another runtime operation.
    assert!(completed_observation(Some(&completed()), &[7; 32], true, Some(&result)).unwrap());
    let expired = PlatformAdmissionState {
        running_owner: false,
        ..initial
    };
    assert!(!completed_observation(Some(&expired), &[7; 32], true, Some(&result)).unwrap());
    let cancelled = PlatformAdmissionState {
        cancelled: true,
        ..completed()
    };
    assert!(completed_observation(Some(&cancelled), &[7; 32], true, Some(&result)).is_err());
}
#[test]
fn stopped_receipt_recheck_refuses_owner_epoch_runtime_and_binding_drift() {
    let fixture = crate::protocol::sandbox_grant::code_owner_fixture("1".repeat(32));
    let whole = fixture
        .intent
        .binding(&fixture.scope, chrono::Utc::now().timestamp_millis())
        .unwrap()
        .clone();
    let original = OwnedCodePlatformRuntime {
        runtime_id: "original-runtime".into(),
        owner: "original-owner".into(),
        epoch: 1,
        broker: CodePlatformBinding {
            schema: "elitea.sandbox.code-platform-binding.v1".into(),
            prepared_job_sha256: whole.prepared_job_sha256.clone(),
            prepared_fingerprint: whole.request_digest.clone(),
            policy_sha256: "a".repeat(64),
            max_calls: 8,
            max_total_bytes: 4096,
            compiled_execute: None,
        },
        binding: whole,
    };
    verify_unchanged_platform_owner(&original, &original).unwrap();
    for index in 0..5 {
        let mut changed = original.clone();
        match index {
            0 => changed.runtime_id = "replacement-runtime".into(),
            1 => changed.owner = "replacement-owner".into(),
            2 => changed.epoch += 1,
            3 => changed.binding.source_sha256 = "b".repeat(64),
            4 => changed.broker.policy_sha256 = "b".repeat(64),
            _ => unreachable!(),
        }
        assert!(matches!(
            verify_unchanged_platform_owner(&changed, &original),
            Err(LedgerError::Fenced)
        ));
    }
}
