//! Pure diagnostic checks. No transport, database, runtime, or clock I/O occurs.
use super::*;
use crate::sandbox::ledger::{JobRecord, JobScope};
use std::sync::{Arc, Mutex};

const CANARY: &str = "SENSITIVE_CAUSE_GRANT_SOURCE_PATH_PRIVATE_URL_DSN";

struct MemoryWriter(Arc<Mutex<Vec<u8>>>);
impl std::io::Write for MemoryWriter {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        self.0.lock().unwrap().write(bytes)
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

fn capture_events(emit: impl FnOnce()) -> String {
    let bytes = Arc::new(Mutex::new(Vec::new()));
    let sink = Arc::clone(&bytes);
    let subscriber = tracing_subscriber::fmt()
        .with_max_level(tracing::Level::TRACE)
        .without_time()
        .with_ansi(false)
        .with_writer(move || MemoryWriter(Arc::clone(&sink)))
        .finish();
    tracing::subscriber::with_default(subscriber, emit);
    let captured = bytes.lock().unwrap().clone();
    String::from_utf8(captured).unwrap()
}

fn scope() -> JobScope {
    JobScope::new(CANARY.to_owned(), 7, [17; 32], [19; 32]).unwrap()
}

#[test]
fn publication_phase_does_not_turn_unknown_wire_values_into_ready() {
    use crate::protocol::elitea::runtime::v1::RustCompiledPublicationPhaseV1 as Wire;
    assert_eq!(
        CompiledPublicationPhase::from_wire(Wire::Executable as i32),
        CompiledPublicationPhase::Executable
    );
    assert_eq!(
        CompiledPublicationPhase::from_wire(Wire::Release as i32),
        CompiledPublicationPhase::Release
    );
    assert_eq!(
        CompiledPublicationPhase::from_wire(Wire::Ready as i32),
        CompiledPublicationPhase::Ready
    );
    for value in [Wire::Unspecified as i32, -1, i32::MAX] {
        assert_eq!(
            CompiledPublicationPhase::from_wire(value),
            CompiledPublicationPhase::Unsupported
        );
    }
}

#[test]
fn typed_failures_keep_fatal_retryable_and_capacity_severity_separate() {
    let cases = [
        (
            SupervisorError::Ledger(LedgerError::Invalid),
            tracing::Level::ERROR,
            "ledger_invalid",
        ),
        (
            SupervisorError::Ledger(LedgerError::Conflict),
            tracing::Level::ERROR,
            "ledger_conflict",
        ),
        (
            SupervisorError::Ledger(LedgerError::Missing),
            tracing::Level::ERROR,
            "ledger_missing",
        ),
        (
            SupervisorError::Receipt,
            tracing::Level::ERROR,
            "receipt_invalid",
        ),
        (
            SupervisorError::Content(DependencyContentError::Integrity),
            tracing::Level::ERROR,
            "content_integrity",
        ),
        (
            SupervisorError::Content(DependencyContentError::Configuration),
            tracing::Level::ERROR,
            "configuration",
        ),
        (
            SupervisorError::Invalid,
            tracing::Level::WARN,
            "admission_invalid",
        ),
        (
            SupervisorError::Ledger(LedgerError::Fenced),
            tracing::Level::WARN,
            "ledger_fenced",
        ),
        (
            SupervisorError::Content(DependencyContentError::Authority),
            tracing::Level::WARN,
            "content_authority",
        ),
        (
            SupervisorError::Content(DependencyContentError::Unavailable { status: 503 }),
            tracing::Level::WARN,
            "content_unavailable",
        ),
        (
            SupervisorError::Content(DependencyContentError::Timeout),
            tracing::Level::WARN,
            "content_timeout",
        ),
        (SupervisorError::Busy, tracing::Level::DEBUG, "capacity"),
        (
            SupervisorError::Content(DependencyContentError::Busy),
            tracing::Level::DEBUG,
            "content_capacity",
        ),
    ];
    for (error, level, name) in cases {
        let diagnostic = CompiledPublicationFailure::from_error(&error);
        assert_eq!(diagnostic.level(), level);
        assert_eq!(diagnostic.as_str(), name);
    }
}

#[test]
fn nested_sensitive_causes_never_enter_the_owned_failure_event() {
    let errors = [
        SupervisorError::Ledger(LedgerError::Database(sqlx::Error::Protocol(
            CANARY.to_owned(),
        ))),
        SupervisorError::Runtime(adk_sandbox::SandboxError::ExecutionFailed(
            CANARY.to_owned(),
        )),
        SupervisorError::Content(DependencyContentError::Staging(std::io::Error::other(
            CANARY,
        ))),
    ];
    let output = capture_events(|| {
        for error in &errors {
            publication_log(
                CompiledPublicationPhase::Ready,
                &scope(),
                3,
                Duration::from_millis(35),
                CompiledPublicationDisposition::Error,
                Some(CompiledPublicationFailure::from_error(error)),
                false,
            );
        }
    });
    assert_eq!(
        output.matches("sandbox_compiled_publication").count(),
        errors.len()
    );
    assert_eq!(output.lines().count(), errors.len());
    assert_eq!(
        output
            .matches(r#"event="sandbox_compiled_publication""#)
            .count(),
        errors.len()
    );
    assert_eq!(output.matches("disposition=error").count(), 0);
    for class in [
        "ledger_unavailable",
        "runtime_observation",
        "content_staging",
    ] {
        let line = output
            .lines()
            .find(|line| line.contains(&format!("error_class={class:?}")))
            .unwrap();
        assert!(line.trim_start().starts_with("WARN "));
        assert!(line.contains(r#"phase="ready""#));
        assert!(line.contains(r#"disposition="error""#));
        assert!(line.contains(&format!("job_key={:?}", "11".repeat(32))));
    }
    assert!(!output.contains(CANARY));
    assert!(output.contains("ledger_unavailable"));
    assert!(output.contains("runtime_observation"));
    assert!(output.contains("content_staging"));
    assert!(output.contains("WARN"));
    assert!(output.contains("1111111111111111111111111111111111111111111111111111111111111111"));
    assert!(output.contains("project_id=7"));
    assert!(output.contains("compilation_export_lease_epoch=3"));
    assert!(output.contains("duration_ms=35"));
}

#[test]
fn terminal_business_payload_does_not_change_diagnostic_fields() {
    let outcome = SnapshotReconciliation::Outcome(Reconciliation::Terminal {
        record: JobRecord {
            phase: Phase::Failed,
            result_json: Some(CANARY.to_owned()),
            failure_code: Some(CANARY.to_owned()),
            runtime_id: Some(CANARY.to_owned()),
        },
        cleanup_pending: true,
    });
    let disposition = CompiledPublicationDisposition::from_outcome(&outcome);
    assert_eq!(disposition, CompiledPublicationDisposition::Failed);
    let output = capture_events(|| {
        publication_log(
            CompiledPublicationPhase::Release,
            &scope(),
            5,
            Duration::ZERO,
            disposition,
            None,
            true,
        );
    });
    assert!(!output.contains(CANARY));
    assert_eq!(output.matches("sandbox_compiled_publication").count(), 1);
    assert_eq!(output.lines().count(), 1);
    assert_eq!(
        output
            .matches(r#"event="sandbox_compiled_publication""#)
            .count(),
        1
    );
    assert!(output.contains(r#"phase="release""#));
    assert!(output.contains(r#"disposition="failed""#));
    assert!(output.contains(r#"error_class="none""#));
    assert!(output.contains("ERROR"));
    assert!(output.contains("cleanup_pending=true"));
}

#[test]
fn pending_and_staged_results_do_not_emit_fatal_failures() {
    let pending = SnapshotReconciliation::Outcome(Reconciliation::OwnedElsewhere);
    let staged = SnapshotReconciliation::Captured {
        descriptor: CANARY.as_bytes().to_vec(),
        job_key: [17; 32],
    };
    assert_eq!(
        CompiledPublicationDisposition::from_outcome(&pending).level(),
        tracing::Level::DEBUG
    );
    assert_eq!(
        CompiledPublicationDisposition::from_outcome(&staged).level(),
        tracing::Level::INFO
    );
    let ready = SnapshotReconciliation::Outcome(Reconciliation::Terminal {
        record: JobRecord {
            phase: Phase::Completed,
            result_json: Some(CANARY.to_owned()),
            failure_code: None,
            runtime_id: Some(CANARY.to_owned()),
        },
        cleanup_pending: false,
    });
    assert_eq!(
        CompiledPublicationDisposition::from_outcome(&ready).level(),
        tracing::Level::INFO
    );
}

#[test]
fn duration_is_a_bounded_observation_without_a_new_deadline() {
    assert_eq!(publication_duration_millis(Duration::from_micros(1999)), 1);
    assert_eq!(publication_duration_millis(Duration::MAX), u64::MAX);
    assert_eq!(publication_duration_millis(Duration::ZERO), 0);
}

#[test]
fn schema_failure_publication_event_has_safe_sqlstate_and_operator_class() {
    use crate::sandbox::ledger::SchemaMigrationRequired;
    for schema in [
        SchemaMigrationRequired::MissingColumn,
        SchemaMigrationRequired::MissingTable,
    ] {
        let error = SupervisorError::Ledger(LedgerError::SchemaMigrationRequired(schema));
        let output = capture_events(|| {
            publication_log(
                CompiledPublicationPhase::Ready,
                &scope(),
                3,
                Duration::from_millis(1),
                CompiledPublicationDisposition::Error,
                Some(CompiledPublicationFailure::from_error(&error)),
                false,
            );
        });
        assert_eq!(output.lines().count(), 1);
        assert!(output.trim_start().starts_with("ERROR "));
        assert!(output.contains(r#"error_class="schema_migration_required""#));
        assert!(output.contains(&format!("sqlstate={:?}", schema.sqlstate())));
        assert!(!output.contains(CANARY));
    }
}
