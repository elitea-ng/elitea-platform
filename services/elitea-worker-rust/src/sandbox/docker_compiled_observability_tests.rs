//! Pure stage observations. No runtime, database, network, timer, or file I/O occurs.
use super::{
    Context, DependencyContentError, JobScope, LedgerError, Observation, Stage, SupervisorError,
    duration_millis, observe, observe_export_record, observe_optional,
};
use std::{
    future::{Future as _, pending, ready},
    sync::{Arc, Mutex},
    task::{Context as TaskContext, Poll, Waker},
    time::Duration,
};

const CANARY: &str = "SENSITIVE_GRANT_SOURCE_RUNTIME_PATH_URL_DSN_BUSINESS_VALUE";

struct MemoryWriter(Arc<Mutex<Vec<u8>>>);
impl std::io::Write for MemoryWriter {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        self.0.lock().unwrap().write(bytes)
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}
fn capture_events<T>(emit: impl FnOnce() -> T) -> (T, String) {
    let bytes = Arc::new(Mutex::new(Vec::new()));
    let sink = Arc::clone(&bytes);
    let subscriber = tracing_subscriber::fmt()
        .with_max_level(tracing::Level::TRACE)
        .without_time()
        .with_ansi(false)
        .with_writer(move || MemoryWriter(Arc::clone(&sink)))
        .finish();
    let result = tracing::subscriber::with_default(subscriber, emit);
    let output = String::from_utf8(bytes.lock().unwrap().clone()).unwrap();
    (result, output)
}
fn assert_stage_event_count(output: &str, expected: usize) {
    assert_eq!(output.lines().count(), expected);
    assert_eq!(
        output.matches(r#"event="sandbox_compiled_stage""#).count(),
        expected
    );
}
fn fixture() -> JobScope {
    JobScope::new(CANARY.to_owned(), 7, [17; 32], [19; 32]).unwrap()
}
fn capture(scope: &JobScope) -> Context<'_> {
    // This is diagnostic test data, not a production lease or authority.
    Context {
        scope,
        writer_epoch: Some(11),
        export_epoch: None,
        phase: "compile_capture",
    }
}

#[test]
fn stage_failure_returns_the_original_error_once_without_sensitive_causes() {
    let scope = fixture();
    let (result, output) = capture_events(|| {
        Observation::start(capture(&scope), Stage::Export).finish::<()>(Err(
            SupervisorError::Ledger(LedgerError::Database(sqlx::Error::Protocol(
                CANARY.to_owned(),
            ))),
        ))
    });
    match result {
        Err(SupervisorError::Ledger(LedgerError::Database(sqlx::Error::Protocol(value)))) => {
            assert_eq!(value, CANARY);
        }
        _ => panic!("the original error did not return"),
    }
    assert_stage_event_count(&output, 2);
    assert_eq!(output.matches(r#"disposition="failed""#).count(), 1);
    // DefaultFields renders record_str with Debug quoting; empty capture cannot pass.
    assert_eq!(output.matches("disposition=failed").count(), 0);
    assert_eq!(output.matches(r#"disposition="started""#).count(), 1);
    let failed = output
        .lines()
        .find(|line| line.contains(r#"disposition="failed""#))
        .unwrap();
    assert!(failed.contains("WARN"));
    assert!(failed.contains(r#"error_class="ledger_unavailable""#));
    assert!(!output.contains("interrupted"));
    assert!(!output.contains(CANARY));
    assert!(output.contains(r#"operation="export""#));
    assert!(output.contains("ledger_unavailable"));
    assert!(output.contains("writer_lease_epoch=11"));
    assert!(output.contains(&format!("job_key={:?}", "11".repeat(32))));
    assert!(output.contains(&format!("request_digest={:?}", "13".repeat(32))));
}

#[test]
fn dropping_a_pending_stage_reports_interrupted_without_effect_claims() {
    let scope = fixture();
    let ((), output) = capture_events(|| {
        let mut operation = Box::pin(observe(
            capture(&scope),
            Stage::ContentStaging,
            pending::<Result<(), SupervisorError>>(),
        ));
        let mut task = TaskContext::from_waker(Waker::noop());
        assert!(operation.as_mut().poll(&mut task).is_pending());
        drop(operation);
    });
    assert_stage_event_count(&output, 2);
    assert_eq!(output.matches(r#"disposition="interrupted""#).count(), 1);
    assert_eq!(output.matches(r#"disposition="started""#).count(), 1);
    assert!(!output.contains(r#"disposition="failed""#));
    assert!(!output.contains(r#"disposition="succeeded""#));
    assert!(!output.contains("no_effect"));
    assert!(!output.contains("lease_released"));
    assert!(!output.contains(CANARY));
    assert!(output.contains(r#"error_class="none""#));
    assert!(
        output
            .lines()
            .any(|line| line.trim_start().starts_with("WARN ")
                && line.contains(r#"disposition="interrupted""#))
    );
}

#[test]
fn unpolled_future_creates_no_false_stage_or_interruption() {
    let scope = fixture();
    let ((), output) = capture_events(|| {
        drop(observe(
            capture(&scope),
            Stage::ContentStaging,
            pending::<Result<(), SupervisorError>>(),
        ));
    });
    assert!(output.is_empty());
}

#[test]
fn heartbeat_failure_and_dropped_stage_keep_different_meanings() {
    let scope = fixture();
    let ((), output) = capture_events(|| {
        let diagnostic = capture(&scope);
        let mut operation = Box::pin(observe(
            diagnostic,
            Stage::CaptureObservation,
            pending::<Result<(), SupervisorError>>(),
        ));
        let mut task = TaskContext::from_waker(Waker::noop());
        assert!(operation.as_mut().poll(&mut task).is_pending());
        let result = Observation::start(diagnostic, Stage::LeaseRenew)
            .finish::<()>(Err(SupervisorError::Ledger(LedgerError::Fenced)));
        assert!(matches!(
            result,
            Err(SupervisorError::Ledger(LedgerError::Fenced))
        ));
        drop(operation);
    });
    assert_stage_event_count(&output, 4);
    assert_eq!(output.matches(r#"disposition="failed""#).count(), 1);
    assert_eq!(output.matches(r#"disposition="interrupted""#).count(), 1);
    let failed = output
        .lines()
        .find(|line| line.contains(r#"disposition="failed""#))
        .unwrap();
    assert!(failed.contains("WARN"));
    assert!(failed.contains(r#"operation="lease_renew""#));
    assert!(failed.contains("ledger_fenced"));
    let interrupted = output
        .lines()
        .find(|line| line.contains(r#"disposition="interrupted""#))
        .unwrap();
    assert!(interrupted.contains("WARN"));
    assert!(interrupted.contains(r#"operation="capture_observation""#));
    assert!(interrupted.contains(r#"error_class="none""#));
}

#[test]
fn successful_stage_returns_business_value_without_logging_it() {
    let scope = fixture();
    let (result, output) = capture_events(|| {
        let mut operation = Box::pin(observe(
            capture(&scope),
            Stage::ReceiptObservation,
            ready(Ok(CANARY.to_owned())),
        ));
        let mut task = TaskContext::from_waker(Waker::noop());
        operation.as_mut().poll(&mut task)
    });
    assert!(matches!(result, Poll::Ready(Ok(value)) if value == CANARY));
    assert_stage_event_count(&output, 2);
    assert_eq!(output.matches(r#"disposition="succeeded""#).count(), 1);
    assert!(
        output
            .lines()
            .all(|line| line.trim_start().starts_with("DEBUG "))
    );
    assert!(!output.contains("interrupted"));
    assert!(!output.contains(CANARY));
}

#[test]
fn absent_context_emits_nothing_and_keeps_execution_behavior() {
    let (result, output) = capture_events(|| {
        let mut operation = Box::pin(observe_optional(
            None,
            Stage::LeaseRenew,
            ready(Err::<(), _>(SupervisorError::Ledger(LedgerError::Fenced))),
        ));
        let mut task = TaskContext::from_waker(Waker::noop());
        operation.as_mut().poll(&mut task)
    });
    assert!(matches!(
        result,
        Poll::Ready(Err(SupervisorError::Ledger(LedgerError::Fenced)))
    ));
    assert!(output.is_empty());
}

#[test]
fn immutable_export_epoch_enters_only_after_successful_recording() {
    let scope = fixture();
    for succeeds in [false, true] {
        let ((), output) = capture_events(|| {
            let result = if succeeds {
                Ok(())
            } else {
                Err(SupervisorError::Ledger(LedgerError::Fenced))
            };
            let mut operation = Box::pin(observe_export_record(
                capture(&scope),
                Some(4),
                ready(result),
            ));
            let mut task = TaskContext::from_waker(Waker::noop());
            assert!(operation.as_mut().poll(&mut task).is_ready());
        });
        assert_stage_event_count(&output, 2);
        let disposition = if succeeds { "succeeded" } else { "failed" };
        assert_eq!(
            output
                .matches(&format!("disposition={disposition:?}"))
                .count(),
            1
        );
        assert_eq!(output.matches(r#"disposition="started""#).count(), 1);
        assert_eq!(
            output.contains("compilation_export_lease_epoch=4"),
            succeeds
        );
        assert!(output.contains("writer_lease_epoch=11"));
        assert!(!output.contains(CANARY));
    }
}

#[test]
fn publication_keeps_writer_and_immutable_export_epochs_distinct() {
    use crate::protocol::elitea::runtime::v1::RustCompiledPublicationPhaseV1 as Wire;
    let scope = fixture();
    let ((), output) = capture_events(|| {
        let diagnostic = Context::publication(&scope, Wire::Release as i32, 4).observed_claim(11);
        Observation::start(diagnostic, Stage::LeaseRelease)
            .finish(Ok(()))
            .unwrap();
    });
    assert_stage_event_count(&output, 3);
    assert_eq!(output.matches(r#"disposition="acquired""#).count(), 1);
    assert!(
        output
            .lines()
            .any(|line| line.trim_start().starts_with("INFO ")
                && line.contains(r#"disposition="acquired""#))
    );
    assert!(output.contains(r#"phase="release""#));
    assert!(output.contains("writer_lease_epoch=11"));
    assert!(output.contains("compilation_export_lease_epoch=4"));
    assert!(!output.contains(CANARY));
    let ((), ready_output) = capture_events(|| {
        Observation::start(
            Context::publication(&scope, Wire::Ready as i32, 4),
            Stage::ReadyPublication,
        )
        .finish(Ok(()))
        .unwrap();
    });
    assert_stage_event_count(&ready_output, 2);
    assert_eq!(
        ready_output.matches(r#"disposition="succeeded""#).count(),
        1
    );
    assert!(!ready_output.contains("writer_lease_epoch="));
    assert!(!ready_output.contains("writer_lease_epoch=0"));
}

#[test]
fn capacity_after_claim_is_visible_without_promoting_unowned_ready_polls() {
    use crate::protocol::elitea::runtime::v1::RustCompiledPublicationPhaseV1 as Wire;
    let scope = fixture();
    let ((), owned) = capture_events(|| {
        let _ = Observation::start(capture(&scope), Stage::ContentStaging)
            .finish::<()>(Err(SupervisorError::Content(DependencyContentError::Busy)));
    });
    let ((), unowned) = capture_events(|| {
        let _ = Observation::start(
            Context::publication(&scope, Wire::Ready as i32, 4),
            Stage::ReadyPublication,
        )
        .finish::<()>(Err(SupervisorError::Content(DependencyContentError::Busy)));
    });
    assert_stage_event_count(&owned, 2);
    assert_stage_event_count(&unowned, 2);
    assert_eq!(owned.matches(r#"disposition="failed""#).count(), 1);
    assert_eq!(unowned.matches(r#"disposition="failed""#).count(), 1);
    assert!(
        owned
            .lines()
            .any(|line| line.trim_start().starts_with("WARN ")
                && line.contains(r#"disposition="failed""#))
    );
    assert!(
        unowned
            .lines()
            .all(|line| line.trim_start().starts_with("DEBUG "))
    );
    assert!(!owned.contains(CANARY));
    assert!(!unowned.contains(CANARY));
}

#[test]
fn nested_runtime_staging_and_receipt_causes_remain_safe() {
    let scope = fixture();
    let errors = [
        SupervisorError::Runtime(adk_sandbox::SandboxError::ExecutionFailed(
            CANARY.to_owned(),
        )),
        SupervisorError::Runtime(adk_sandbox::SandboxError::Timeout {
            timeout: Duration::from_secs(999),
        }),
        SupervisorError::Content(DependencyContentError::Staging(std::io::Error::other(
            CANARY,
        ))),
        SupervisorError::Content(DependencyContentError::Unavailable { status: 599 }),
        SupervisorError::Receipt,
    ];
    let ((), output) = capture_events(|| {
        for error in errors {
            let _ = Observation::start(capture(&scope), Stage::Export).finish::<()>(Err(error));
        }
    });
    assert_stage_event_count(&output, 10);
    assert_eq!(output.matches(r#"disposition="failed""#).count(), 5);
    assert!(!output.contains(CANARY));
    assert!(!output.contains("599"));
    assert!(!output.contains("999"));
    for class in [
        "runtime_observation",
        "runtime_timeout",
        "content_staging",
        "content_unavailable",
        "receipt_invalid",
    ] {
        let line = output
            .lines()
            .find(|line| line.contains(&format!("error_class={class:?}")))
            .unwrap();
        assert!(line.contains(r#"disposition="failed""#));
        assert!(line.contains(if class == "receipt_invalid" {
            "ERROR"
        } else {
            "WARN"
        }));
    }
}

#[test]
fn unknown_phase_and_duration_remain_bounded_diagnostic_values() {
    let scope = fixture();
    for phase in [0, -1, i32::MAX] {
        assert_eq!(Context::publication(&scope, phase, 4).phase, "unsupported");
    }
    assert_eq!(duration_millis(Duration::ZERO), 0);
    assert_eq!(duration_millis(Duration::from_micros(1999)), 1);
    assert_eq!(duration_millis(Duration::MAX), u64::MAX);
}

#[test]
fn schema_stage_failure_has_safe_sqlstate_and_keeps_the_original_class() {
    use crate::sandbox::ledger::SchemaMigrationRequired;
    for schema in [
        SchemaMigrationRequired::MissingColumn,
        SchemaMigrationRequired::MissingTable,
    ] {
        let scope = fixture();
        let (result, output) = capture_events(|| {
            Observation::start(capture(&scope), Stage::ReceiptRead).finish::<()>(Err(
                SupervisorError::Ledger(LedgerError::SchemaMigrationRequired(schema)),
            ))
        });
        assert!(
            matches!(result, Err(SupervisorError::Ledger(LedgerError::SchemaMigrationRequired(actual))) if actual == schema)
        );
        assert_stage_event_count(&output, 2);
        let failed = output
            .lines()
            .find(|line| line.contains(r#"disposition="failed""#))
            .unwrap();
        assert!(failed.trim_start().starts_with("ERROR "));
        assert!(failed.contains(r#"error_class="schema_migration_required""#));
        assert!(failed.contains(&format!("sqlstate={:?}", schema.sqlstate())));
        assert!(!output.contains(CANARY));
    }
}
