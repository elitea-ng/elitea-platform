//! Bounded diagnostic observations. They grant no authority and change no lease.
use super::{DependencyContentError, JobLease, JobScope, LedgerError, SupervisorError};
use std::{future::Future, time::Instant};

#[derive(Clone, Copy)]
pub(super) struct Context<'a> {
    scope: &'a JobScope,
    writer_epoch: Option<i64>,
    export_epoch: Option<u64>,
    phase: &'static str,
}
impl<'a> Context<'a> {
    pub(super) fn capture(scope: &'a JobScope, lease: &JobLease) -> Self {
        Self {
            scope,
            writer_epoch: Some(lease.epoch()),
            export_epoch: None,
            phase: "compile_capture",
        }
    }
    pub(super) fn publication(scope: &'a JobScope, phase: i32, export_epoch: u64) -> Self {
        use crate::protocol::elitea::runtime::v1::RustCompiledPublicationPhaseV1 as Wire;
        let phase = match Wire::try_from(phase) {
            Ok(Wire::Executable) => "executable",
            Ok(Wire::Release) => "release",
            Ok(Wire::Ready) => "ready",
            Ok(Wire::Unspecified) | Err(_) => "unsupported",
        };
        Self {
            scope,
            writer_epoch: None,
            export_epoch: Some(export_epoch),
            phase,
        }
    }
    pub(super) fn with_export_epoch(mut self, export_epoch: Option<u64>) -> Self {
        self.export_epoch = export_epoch;
        self
    }
    pub(super) fn claimed(self, lease: &JobLease) -> Self {
        self.observed_claim(lease.epoch())
    }
    fn observed_claim(mut self, epoch: i64) -> Self {
        self.writer_epoch = Some(epoch);
        emit(
            self,
            Stage::Ownership,
            "acquired",
            None,
            0,
            tracing::Level::INFO,
        );
        self
    }
}

#[derive(Clone, Copy)]
pub(super) enum Stage {
    Ownership,
    StopCheck,
    IntentRecord,
    Provision,
    ControlImport,
    DependencyHydration,
    DispatchRecord,
    RuntimeDispatch,
    CompiledRecordRead,
    CaptureObservation,
    DescriptorDecode,
    RuntimeBinding,
    LaunchVerification,
    Export,
    ExportRecord,
    StagingClose,
    ContentStaging,
    ReceiptRead,
    ReleaseTransfer,
    ReceiptObservation,
    ReadyPublication,
    LeaseRelease,
    LeaseRenew,
    HydrationAgeRead,
    HydrationAgeLimit,
    PhaseRead,
}
impl Stage {
    fn as_str(self) -> &'static str {
        match self {
            Self::Ownership => "ownership_acquired",
            Self::StopCheck => "stop_check",
            Self::IntentRecord => "intent_record",
            Self::Provision => "provision",
            Self::ControlImport => "control_import",
            Self::DependencyHydration => "dependency_hydration",
            Self::DispatchRecord => "dispatch_record",
            Self::RuntimeDispatch => "runtime_dispatch",
            Self::CompiledRecordRead => "compiled_record_read",
            Self::CaptureObservation => "capture_observation",
            Self::DescriptorDecode => "descriptor_decode",
            Self::RuntimeBinding => "runtime_binding",
            Self::LaunchVerification => "launch_verification",
            Self::Export => "export",
            Self::ExportRecord => "export_record",
            Self::StagingClose => "staging_close",
            Self::ContentStaging => "stage",
            Self::ReceiptRead => "receipt_read",
            Self::ReleaseTransfer => "release_transfer",
            Self::ReceiptObservation => "receipt_observation",
            Self::ReadyPublication => "ready_publication",
            Self::LeaseRelease => "lease_release",
            Self::LeaseRenew => "lease_renew",
            Self::HydrationAgeRead => "hydration_age_read",
            Self::HydrationAgeLimit => "hydration_age_limit",
            Self::PhaseRead => "phase_read",
        }
    }
}

#[derive(Clone, Copy)]
struct Failure {
    class: &'static str,
    sqlstate: &'static str,
    level: tracing::Level,
}
impl Failure {
    fn from_error(error: &SupervisorError) -> Self {
        use tracing::Level;
        let (class, level) = match error {
            SupervisorError::Invalid => ("admission_invalid", Level::WARN),
            SupervisorError::Busy => ("capacity", Level::DEBUG),
            SupervisorError::Ledger(error) => match error {
                LedgerError::Invalid => ("ledger_invalid", Level::ERROR),
                LedgerError::Conflict => ("ledger_conflict", Level::ERROR),
                LedgerError::Missing => ("ledger_missing", Level::ERROR),
                LedgerError::Fenced => ("ledger_fenced", Level::WARN),
                LedgerError::SchemaMigrationRequired(_) => {
                    ("schema_migration_required", Level::ERROR)
                }
                LedgerError::Database(_) => ("ledger_unavailable", Level::WARN),
            },
            SupervisorError::Runtime(adk_sandbox::SandboxError::Timeout { .. }) => {
                ("runtime_timeout", Level::WARN)
            }
            SupervisorError::Runtime(_) => ("runtime_observation", Level::WARN),
            SupervisorError::Receipt => ("receipt_invalid", Level::ERROR),
            SupervisorError::Content(error) => match error {
                DependencyContentError::Configuration => ("configuration", Level::ERROR),
                DependencyContentError::Integrity => ("content_integrity", Level::ERROR),
                DependencyContentError::Authority => ("content_authority", Level::WARN),
                DependencyContentError::Busy => ("content_capacity", Level::DEBUG),
                DependencyContentError::Unavailable { .. } => ("content_unavailable", Level::WARN),
                DependencyContentError::Timeout => ("content_timeout", Level::WARN),
                DependencyContentError::Transport(_) => ("content_transport", Level::WARN),
                DependencyContentError::Staging(_) => ("content_staging", Level::WARN),
            },
        };
        let sqlstate = match error {
            SupervisorError::Ledger(LedgerError::SchemaMigrationRequired(schema)) => {
                schema.sqlstate()
            }
            _ => "none",
        };
        Self {
            class,
            sqlstate,
            level,
        }
    }
    fn level(self, owns_lease: bool) -> tracing::Level {
        if owns_lease && self.level == tracing::Level::DEBUG {
            // A late capacity error retains claimed ownership and can explain the wait.
            tracing::Level::WARN
        } else {
            self.level
        }
    }
}

pub(super) struct Observation<'a> {
    context: Context<'a>,
    stage: Stage,
    started: Instant,
    finished: bool,
}
impl<'a> Observation<'a> {
    pub(super) fn start(context: Context<'a>, stage: Stage) -> Self {
        emit(context, stage, "started", None, 0, tracing::Level::DEBUG);
        Self {
            context,
            stage,
            started: Instant::now(),
            finished: false,
        }
    }
    pub(super) fn finish<T>(
        mut self,
        result: Result<T, SupervisorError>,
    ) -> Result<T, SupervisorError> {
        let failure = result.as_ref().err().map(Failure::from_error);
        self.finished = true;
        emit(
            self.context,
            self.stage,
            if failure.is_some() {
                "failed"
            } else {
                "succeeded"
            },
            failure,
            duration_millis(self.started.elapsed()),
            failure.map_or(tracing::Level::DEBUG, |value| {
                value.level(self.context.writer_epoch.is_some())
            }),
        );
        result
    }
}
impl Drop for Observation<'_> {
    fn drop(&mut self) {
        if !self.finished {
            // Dropped observation proves neither failed effects nor released ownership.
            emit(
                self.context,
                self.stage,
                "interrupted",
                None,
                duration_millis(self.started.elapsed()),
                tracing::Level::WARN,
            );
        }
    }
}

pub(super) async fn observe<T>(
    context: Context<'_>,
    stage: Stage,
    operation: impl Future<Output = Result<T, SupervisorError>>,
) -> Result<T, SupervisorError> {
    let observation = Observation::start(context, stage);
    observation.finish(operation.await)
}

pub(super) async fn observe_optional<T>(
    context: Option<Context<'_>>,
    stage: Stage,
    operation: impl Future<Output = Result<T, SupervisorError>>,
) -> Result<T, SupervisorError> {
    match context {
        Some(context) => observe(context, stage, operation).await,
        None => operation.await,
    }
}

pub(super) async fn observe_export_record(
    context: Context<'_>,
    export_epoch: Option<u64>,
    operation: impl Future<Output = Result<(), SupervisorError>>,
) -> Result<(), SupervisorError> {
    let mut observation = Observation::start(context, Stage::ExportRecord);
    let result = operation.await;
    if result.is_ok() {
        // This epoch becomes diagnostic fact only after the existing durable fence succeeds.
        observation.context.export_epoch = export_epoch;
    }
    observation.finish(result)
}

fn duration_millis(duration: std::time::Duration) -> u64 {
    u64::try_from(duration.as_millis()).unwrap_or(u64::MAX)
}

fn emit(
    context: Context<'_>,
    stage: Stage,
    disposition: &'static str,
    failure: Option<Failure>,
    duration_ms: u64,
    level: tracing::Level,
) {
    let job_key = crate::sandbox::dependency_bundle::hex(&context.scope.key);
    let request_digest = crate::sandbox::dependency_bundle::hex(&context.scope.digest);
    let error_class = failure.map_or("none", |value| value.class);
    let sqlstate = failure.map_or("none", |value| value.sqlstate);
    macro_rules! record {
        ($level:expr) => {
            tracing::event!(
                $level,
                event = "sandbox_compiled_stage",
                phase = context.phase,
                operation = stage.as_str(),
                disposition,
                error_class,
                sqlstate,
                project_id = context.scope.project,
                job_key = job_key.as_str(),
                request_digest = request_digest.as_str(),
                writer_lease_epoch = context.writer_epoch,
                compilation_export_lease_epoch = context.export_epoch,
                duration_ms,
            )
        };
    }
    match level {
        tracing::Level::ERROR => record!(tracing::Level::ERROR),
        tracing::Level::WARN => record!(tracing::Level::WARN),
        tracing::Level::INFO => record!(tracing::Level::INFO),
        _ => record!(tracing::Level::DEBUG),
    }
}

#[cfg(test)]
#[path = "docker_compiled_observability_tests.rs"]
mod tests;
