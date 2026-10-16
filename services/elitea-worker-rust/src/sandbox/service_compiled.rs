//! Authenticated revision 4 dispatch; disabled without the compiled profile and data plane.
use std::{
    sync::Arc,
    time::{Duration, Instant},
};

use super::{
    DependencyContentError, DependencyDelivery, Ed25519PublicKeyResolver, LedgerError, Phase,
    PreparedJob, Reconciliation, Request, Response, SandboxJobStatusV1, Status, SupervisorError,
    SupervisorService, authenticated_peer, response, service_error,
};
use crate::{
    protocol::elitea::runtime::v1::{
        PublishRustCompiledSnapshotRequestV1, PublishRustCompiledSnapshotResponseV1,
        SubmitRustCompiledSnapshotRequestV1, SubmitRustCompiledSnapshotResponseV1,
    },
    sandbox::{
        compiled_snapshot::{Control, DESCRIPTOR_LIMIT, Descriptor, Purpose},
        docker_supervisor::SnapshotReconciliation,
    },
};
impl<R: Ed25519PublicKeyResolver + 'static> SupervisorService<R> {
    #[allow(clippy::too_many_lines)] // Keep ordered authority checks and durable phase fences visible together.
    pub(super) async fn submit_snapshot(
        &self,
        request: Request<SubmitRustCompiledSnapshotRequestV1>,
    ) -> Result<Response<SubmitRustCompiledSnapshotResponseV1>, Status> {
        let peer = authenticated_peer(&request)?;
        let input = request.into_inner();
        let now = chrono::Utc::now().timestamp_millis();
        let grant = input
            .grant
            .as_ref()
            .ok_or_else(|| Status::unauthenticated("Compiled snapshot authority is required."))?;
        let job = PreparedJob::from_transport(&input.prepared_job_json)
            .map_err(|_| Status::invalid_argument("The compiled job is invalid."))?;
        let purpose = if input.descriptor_json.is_empty() {
            Purpose::Compile
        } else {
            Purpose::Execute
        };
        if purpose != Purpose::Execute && !input.code_execution_intent_json.is_empty() {
            return Err(Status::invalid_argument(
                "Only Execute may carry an original whole-Code intent.",
            ));
        }
        if input.reconcile_only && !input.code_execution_intent_json.is_empty() {
            return Err(Status::invalid_argument(
                "Receipt-only observation cannot register an original Execute intent.",
            ));
        }
        let control = Control::from_bytes(&input.control_json, purpose)
            .map_err(|_| Status::invalid_argument("The snapshot control is invalid."))?;
        validate_index_mode(&input, purpose)?;
        let content = Arc::clone(self.content.as_ref().ok_or_else(|| {
            Status::failed_precondition("The snapshot data plane is unavailable.")
        })?);
        let outcome = match purpose {
            Purpose::Execute => {
                if input.descriptor_json.len() > DESCRIPTOR_LIMIT {
                    return Err(Status::invalid_argument(
                        "Selected artifact metadata exceeds its bound.",
                    ));
                }
                let authority = self
                    .verifier
                    .verify_snapshot_execute(grant, &peer, &job, &control, now)
                    .map_err(|_| {
                        Status::permission_denied("Execute authority does not bind this snapshot.")
                    })?;
                if !input.code_execution_intent_json.is_empty() {
                    let (execution, generation, dispatch) = authority.original_execution();
                    let intent = self
                        .verifier
                        .verify_code_intent(
                            &input.code_execution_intent_json,
                            &peer,
                            authority.scope(),
                            execution,
                            generation,
                            dispatch,
                            &job,
                            now,
                        )
                        .map_err(|_| {
                            Status::permission_denied(
                                "The original whole-Code intent does not bind compiled Execute.",
                            )
                        })?;
                    self.supervisor
                        .register_compiled_code_intent(
                            &authority,
                            &intent,
                            &job,
                            &control,
                            &input.descriptor_json,
                        )
                        .await
                        .map_err(|error| service_error(&error))?;
                }
                if input.reconcile_only {
                    if input.read_grant.is_some()
                        || input.native_hydration_index.is_some()
                        || input.dependency_content_grant.is_some()
                        || !input.dependency_bundle_json.is_empty()
                    {
                        return Err(Status::invalid_argument(
                            "Receipt recovery cannot carry Read or native content authority.",
                        ));
                    }
                    let descriptor: Descriptor = serde_json::from_slice(&input.descriptor_json)
                        .map_err(|_| {
                            Status::invalid_argument("The selected descriptor is invalid.")
                        })?;
                    descriptor.validate(&control).map_err(|_| {
                        Status::invalid_argument("The selected descriptor changed.")
                    })?;
                    let supervisor = Arc::clone(&self.supervisor);
                    let outcome = self
                        .job_owners
                        .run(async move {
                            supervisor
                                .reconcile_snapshot_execute(
                                    &authority,
                                    &job,
                                    &control,
                                    &input.descriptor_json,
                                )
                                .await
                                .map_err(|error| service_error(&error))
                        })
                        .await?;
                    return Ok(Response::new(match outcome {
                        Some(outcome) => snapshot_response(outcome)?,
                        None => SubmitRustCompiledSnapshotResponseV1 {
                            status: SandboxJobStatusV1::Pending.into(),
                            needs_admission: true,
                            ..Default::default()
                        },
                    }));
                }
                let read_grant = input.read_grant.as_ref().ok_or_else(|| {
                    Status::unauthenticated("Separate snapshot Read authority is required.")
                })?;
                let read = self
                    .verifier
                    .verify_snapshot_read(read_grant, &peer, &job, &control, now)
                    .map_err(|_| {
                        Status::permission_denied("Read authority does not bind this snapshot.")
                    })?;
                let descriptor: Descriptor = serde_json::from_slice(&input.descriptor_json)
                    .map_err(|_| Status::invalid_argument("The selected descriptor is invalid."))?;
                let native = match (
                    input.dependency_content_grant.as_ref(),
                    job.dependency_bundle_root(),
                ) {
                    (None, None) if input.dependency_bundle_json.is_empty() => None,
                    (Some(grant), Some(_)) => Some(
                        self.verifier
                            .verify_content(grant, &peer, now)
                            .map_err(|_| {
                                Status::permission_denied("Native content authority is invalid.")
                            })?,
                    ),
                    _ => {
                        return Err(Status::invalid_argument(
                            "Native Execute content must bind the unchanged prepared request.",
                        ));
                    }
                };
                let bundle = job
                    .dependency_bundle_root()
                    .map(|root| {
                        super::super::dependency_bundle::DependencyBundle::parse(
                            &input.dependency_bundle_json,
                            root,
                        )
                    })
                    .transpose()
                    .map_err(|_| Status::data_loss("Native Execute metadata changed."))?;
                let delivery = match (native, bundle, input.dependency_content_grant.as_ref()) {
                    (Some(authorization), Some(bundle), Some(grant))
                        if bundle.native().is_some() =>
                    {
                        Some((authorization, bundle, grant.clone()))
                    }
                    (None, None, None) => None,
                    _ => {
                        return Err(Status::invalid_argument(
                            "Cached native Execute requires the exact native bundle.",
                        ));
                    }
                };
                if let Some(index) = input.native_hydration_index {
                    let (authorization, bundle, grant) = delivery.as_ref().ok_or_else(|| {
                        Status::invalid_argument(
                            "Indexed Execute requires exact native Content authority.",
                        )
                    })?;
                    self.supervisor
                        .hydrate_snapshot_execute(
                            &authority,
                            &read,
                            &job,
                            &control,
                            &descriptor,
                            &input.descriptor_json,
                            DependencyDelivery {
                                client: &content,
                                authorization,
                                bundle,
                                grant,
                            },
                            index,
                        )
                        .await
                } else {
                    let supervisor = Arc::clone(&self.supervisor);
                    self.job_owners
                        .run(async move {
                            let delivery =
                                delivery.as_ref().map(|(authorization, bundle, grant)| {
                                    DependencyDelivery {
                                        client: &content,
                                        authorization,
                                        bundle,
                                        grant,
                                    }
                                });
                            Ok(supervisor
                                .submit_snapshot_execute(
                                    &authority,
                                    &read,
                                    &job,
                                    &control,
                                    &descriptor,
                                    &input.descriptor_json,
                                    input.read_grant.as_ref().ok_or_else(|| {
                                        Status::unauthenticated(
                                            "Separate snapshot Read authority is required.",
                                        )
                                    })?,
                                    &content,
                                    delivery,
                                )
                                .await)
                        })
                        .await?
                }
            }
            Purpose::Compile => {
                if input.read_grant.is_some() {
                    return Err(Status::invalid_argument(
                        "Compile cannot carry Read authority.",
                    ));
                }
                let authority = self
                    .verifier
                    .verify_snapshot_compile(grant, &peer, &job, &control, now)
                    .map_err(|_| {
                        Status::permission_denied("Compile authority does not bind this request.")
                    })?;
                if input.reconcile_only {
                    if input.native_hydration_index.is_some()
                        || input.dependency_content_grant.is_some()
                        || !input.dependency_bundle_json.is_empty()
                    {
                        return Err(Status::invalid_argument(
                            "Compiler receipt recovery cannot import content.",
                        ));
                    }
                    let supervisor = Arc::clone(&self.supervisor);
                    let outcome = self
                        .job_owners
                        .run(async move {
                            supervisor
                                .reconcile_snapshot_compile(&authority, &job, &control, &content)
                                .await
                                .map_err(|error| service_error(&error))
                        })
                        .await?;
                    return Ok(Response::new(match outcome {
                        Some(outcome) => snapshot_response(outcome)?,
                        None => SubmitRustCompiledSnapshotResponseV1 {
                            status: SandboxJobStatusV1::Pending.into(),
                            needs_admission: true,
                            ..Default::default()
                        },
                    }));
                }
                let native = match (
                    input.dependency_content_grant.as_ref(),
                    job.dependency_bundle_root(),
                ) {
                    (None, None) if input.dependency_bundle_json.is_empty() => None,
                    (Some(grant), Some(_)) => Some(
                        self.verifier
                            .verify_content(grant, &peer, now)
                            .map_err(|_| {
                                Status::permission_denied("Native content authority is invalid.")
                            })?,
                    ),
                    _ => {
                        return Err(Status::invalid_argument(
                            "Native compile content must bind the unchanged prepared request.",
                        ));
                    }
                };
                let bundle = job
                    .dependency_bundle_root()
                    .map(|root| {
                        super::super::dependency_bundle::DependencyBundle::parse(
                            &input.dependency_bundle_json,
                            root,
                        )
                    })
                    .transpose()
                    .map_err(|_| Status::data_loss("Native compile metadata changed."))?;
                let delivery = match (native, bundle, input.dependency_content_grant.as_ref()) {
                    (Some(authorization), Some(bundle), Some(grant)) => {
                        Some((authorization, bundle, grant.clone()))
                    }
                    _ => None,
                };
                if let Some(index) = input.native_hydration_index {
                    let (authorization, bundle, grant) = delivery.as_ref().ok_or_else(|| {
                        Status::invalid_argument(
                            "Indexed compiler hydration requires exact native content authority.",
                        )
                    })?;
                    self.supervisor
                        .hydrate_snapshot_compile(
                            &authority,
                            &job,
                            &control,
                            DependencyDelivery {
                                client: &content,
                                authorization,
                                bundle,
                                grant,
                            },
                            index,
                        )
                        .await
                } else {
                    let supervisor = Arc::clone(&self.supervisor);
                    self.job_owners
                        .run(async move {
                            let delivery =
                                delivery.as_ref().map(|(authorization, bundle, grant)| {
                                    DependencyDelivery {
                                        client: &content,
                                        authorization,
                                        bundle,
                                        grant,
                                    }
                                });
                            Ok(supervisor
                                .submit_snapshot_compile(
                                    &authority, &job, &control, delivery, &content,
                                )
                                .await)
                        })
                        .await?
                }
            }
        }
        .map_err(|error| service_error(&error))?;
        Ok(Response::new(snapshot_response(outcome)?))
    }
    pub(super) async fn publish_snapshot(
        &self,
        request: Request<PublishRustCompiledSnapshotRequestV1>,
    ) -> Result<Response<PublishRustCompiledSnapshotResponseV1>, Status> {
        let peer = authenticated_peer(&request)?;
        let input = request.into_inner();
        let grant = input
            .publish_grant
            .as_ref()
            .ok_or_else(|| Status::unauthenticated("Snapshot Publish authority is required."))?;
        let authority = self
            .verifier
            .verify_snapshot_publish(grant, &peer, chrono::Utc::now().timestamp_millis())
            .map_err(|_| Status::permission_denied("Snapshot Publish authority is invalid."))?;
        let started = Instant::now();
        let phase = CompiledPublicationPhase::from_wire(input.phase);
        let scope = authority.scope();
        let export_epoch = authority.compilation_lease_epoch;
        let content = self.content.as_deref().ok_or_else(|| {
            publication_log(
                phase,
                scope,
                export_epoch,
                started.elapsed(),
                CompiledPublicationDisposition::Error,
                Some(CompiledPublicationFailure::Configuration),
                false,
            );
            Status::failed_precondition("The snapshot data plane is unavailable.")
        })?;
        let outcome = self
            .supervisor
            .publish_snapshot(&authority, grant, input.phase, content)
            .await
            .map_err(|error| {
                publication_log(
                    phase,
                    scope,
                    export_epoch,
                    started.elapsed(),
                    CompiledPublicationDisposition::Error,
                    Some(CompiledPublicationFailure::from_error(&error)),
                    false,
                );
                service_error(&error)
            })?;
        let disposition = CompiledPublicationDisposition::from_outcome(&outcome);
        let receipt = snapshot_response(outcome).inspect_err(|_| {
            publication_log(
                phase,
                scope,
                export_epoch,
                started.elapsed(),
                CompiledPublicationDisposition::Error,
                Some(CompiledPublicationFailure::Receipt),
                false,
            );
        })?;
        publication_log(
            phase,
            scope,
            export_epoch,
            started.elapsed(),
            disposition,
            None,
            receipt.cleanup_pending,
        );
        Ok(Response::new(PublishRustCompiledSnapshotResponseV1 {
            status: receipt.status,
            failure_code: receipt.failure_code,
            cleanup_pending: receipt.cleanup_pending,
        }))
    }
}
fn snapshot_response(
    outcome: SnapshotReconciliation,
) -> Result<SubmitRustCompiledSnapshotResponseV1, Status> {
    Ok(match outcome {
        SnapshotReconciliation::Hydrated { ready } => SubmitRustCompiledSnapshotResponseV1 {
            status: SandboxJobStatusV1::Pending.into(),
            native_hydration_ready: ready,
            ..Default::default()
        },
        SnapshotReconciliation::Captured {
            descriptor,
            job_key,
        } => SubmitRustCompiledSnapshotResponseV1 {
            status: SandboxJobStatusV1::Pending.into(),
            descriptor_json: descriptor,
            compilation_job_key: job_key.to_vec(),
            ..Default::default()
        },
        SnapshotReconciliation::Outcome(outcome) => {
            let receipt = response(outcome)?;
            SubmitRustCompiledSnapshotResponseV1 {
                status: receipt.status,
                result_json: receipt.result_json,
                failure_code: receipt.failure_code,
                cleanup_pending: receipt.cleanup_pending,
                ..Default::default()
            }
        }
    })
}

fn validate_index_mode(
    input: &SubmitRustCompiledSnapshotRequestV1,
    purpose: Purpose,
) -> Result<(), Status> {
    if input.native_hydration_index.is_some() {
        let role_shape = match purpose {
            Purpose::Compile => input.read_grant.is_none() && input.descriptor_json.is_empty(),
            Purpose::Execute => input.read_grant.is_some() && !input.descriptor_json.is_empty(),
        };
        if !role_shape
            || input.reconcile_only
            || input.dependency_content_grant.is_none()
            || input.dependency_bundle_json.is_empty()
        {
            return Err(Status::invalid_argument(
                "An inert native index requires exact role-specific authority and metadata.",
            ));
        }
    }
    Ok(())
}

#[cfg(test)]
mod indexed_tests {
    use super::*;
    use crate::protocol::elitea::runtime::v1::SignedSandboxJobGrantV1;
    #[test]
    fn cached_execution_requires_selected_read_while_receipt_recovery_cannot_import_indices() {
        let request = SubmitRustCompiledSnapshotRequestV1 {
            native_hydration_index: Some(0),
            dependency_content_grant: Some(SignedSandboxJobGrantV1::default()),
            dependency_bundle_json: vec![1],
            ..Default::default()
        };
        assert!(validate_index_mode(&request, Purpose::Compile).is_ok());
        assert_eq!(
            validate_index_mode(&request, Purpose::Execute)
                .unwrap_err()
                .code(),
            tonic::Code::InvalidArgument
        );
        let mut execute = request.clone();
        execute.read_grant = Some(SignedSandboxJobGrantV1::default());
        execute.descriptor_json = vec![1];
        assert!(validate_index_mode(&execute, Purpose::Execute).is_ok());
        execute.reconcile_only = true;
        assert!(validate_index_mode(&execute, Purpose::Execute).is_err());
        let mut replay = request.clone();
        replay.reconcile_only = true;
        assert!(validate_index_mode(&replay, Purpose::Compile).is_err());
        let mut read = request;
        read.read_grant = Some(SignedSandboxJobGrantV1::default());
        assert!(validate_index_mode(&read, Purpose::Compile).is_err());
    }
    #[test]
    fn native_index_requires_separate_content_authority_and_original_metadata() {
        let mut request = SubmitRustCompiledSnapshotRequestV1 {
            native_hydration_index: Some(1),
            ..Default::default()
        };
        assert!(validate_index_mode(&request, Purpose::Compile).is_err());
        request.dependency_content_grant = Some(SignedSandboxJobGrantV1::default());
        assert!(validate_index_mode(&request, Purpose::Compile).is_err());
        request.dependency_bundle_json = vec![1];
        assert!(validate_index_mode(&request, Purpose::Compile).is_ok());
    }
}

// Diagnostic values describe the existing owner result, not a new state machine.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum CompiledPublicationPhase {
    Executable,
    Release,
    Ready,
    Unsupported,
}
impl CompiledPublicationPhase {
    fn from_wire(value: i32) -> Self {
        use crate::protocol::elitea::runtime::v1::RustCompiledPublicationPhaseV1;
        match RustCompiledPublicationPhaseV1::try_from(value) {
            Ok(RustCompiledPublicationPhaseV1::Executable) => Self::Executable,
            Ok(RustCompiledPublicationPhaseV1::Release) => Self::Release,
            Ok(RustCompiledPublicationPhaseV1::Ready) => Self::Ready,
            Ok(RustCompiledPublicationPhaseV1::Unspecified) | Err(_) => Self::Unsupported,
        }
    }
    fn as_str(self) -> &'static str {
        match self {
            Self::Executable => "executable",
            Self::Release => "release",
            Self::Ready => "ready",
            Self::Unsupported => "unsupported",
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum CompiledPublicationFailure {
    Admission,
    Capacity,
    LedgerInvalid,
    LedgerConflict,
    LedgerMissing,
    LedgerFenced,
    LedgerSchemaRequired(crate::sandbox::ledger::SchemaMigrationRequired),
    LedgerUnavailable,
    RuntimeObservation,
    Receipt,
    Configuration,
    ContentIntegrity,
    ContentAuthority,
    ContentCapacity,
    ContentUnavailable,
    ContentTimeout,
    ContentTransport,
    ContentStaging,
}
impl CompiledPublicationFailure {
    fn from_error(error: &SupervisorError) -> Self {
        match error {
            SupervisorError::Invalid => Self::Admission,
            SupervisorError::Busy => Self::Capacity,
            SupervisorError::Ledger(error) => match error {
                LedgerError::Invalid => Self::LedgerInvalid,
                LedgerError::Conflict => Self::LedgerConflict,
                LedgerError::Missing => Self::LedgerMissing,
                LedgerError::Fenced => Self::LedgerFenced,
                LedgerError::SchemaMigrationRequired(schema) => Self::LedgerSchemaRequired(*schema),
                LedgerError::Database(_) => Self::LedgerUnavailable,
            },
            SupervisorError::Runtime(_) => Self::RuntimeObservation,
            SupervisorError::Receipt => Self::Receipt,
            SupervisorError::Content(error) => match error {
                DependencyContentError::Configuration => Self::Configuration,
                DependencyContentError::Integrity => Self::ContentIntegrity,
                DependencyContentError::Authority => Self::ContentAuthority,
                DependencyContentError::Busy => Self::ContentCapacity,
                DependencyContentError::Unavailable { .. } => Self::ContentUnavailable,
                DependencyContentError::Timeout => Self::ContentTimeout,
                DependencyContentError::Transport(_) => Self::ContentTransport,
                DependencyContentError::Staging(_) => Self::ContentStaging,
            },
        }
    }
    fn as_str(self) -> &'static str {
        match self {
            Self::Admission => "admission_invalid",
            Self::Capacity => "capacity",
            Self::LedgerInvalid => "ledger_invalid",
            Self::LedgerConflict => "ledger_conflict",
            Self::LedgerMissing => "ledger_missing",
            Self::LedgerFenced => "ledger_fenced",
            Self::LedgerSchemaRequired(_) => "schema_migration_required",
            Self::LedgerUnavailable => "ledger_unavailable",
            Self::RuntimeObservation => "runtime_observation",
            Self::Receipt => "receipt_invalid",
            Self::Configuration => "configuration",
            Self::ContentIntegrity => "content_integrity",
            Self::ContentAuthority => "content_authority",
            Self::ContentCapacity => "content_capacity",
            Self::ContentUnavailable => "content_unavailable",
            Self::ContentTimeout => "content_timeout",
            Self::ContentTransport => "content_transport",
            Self::ContentStaging => "content_staging",
        }
    }
    fn sqlstate(self) -> &'static str {
        match self {
            Self::LedgerSchemaRequired(schema) => schema.sqlstate(),
            _ => "none",
        }
    }
    fn level(self) -> tracing::Level {
        match self {
            Self::Capacity | Self::ContentCapacity => tracing::Level::DEBUG,
            Self::LedgerSchemaRequired(_)
            | Self::LedgerInvalid
            | Self::LedgerConflict
            | Self::LedgerMissing
            | Self::Receipt
            | Self::Configuration
            | Self::ContentIntegrity => tracing::Level::ERROR,
            Self::Admission
            | Self::LedgerFenced
            | Self::LedgerUnavailable
            | Self::RuntimeObservation
            | Self::ContentAuthority
            | Self::ContentUnavailable
            | Self::ContentTimeout
            | Self::ContentTransport
            | Self::ContentStaging => tracing::Level::WARN,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum CompiledPublicationDisposition {
    Error,
    Staged,
    Hydrated,
    NeedsDispatch,
    OwnedElsewhere,
    Completed,
    Failed,
    Cancelled,
    Uncertain,
    InvalidReceipt,
}
impl CompiledPublicationDisposition {
    fn from_outcome(outcome: &SnapshotReconciliation) -> Self {
        match outcome {
            SnapshotReconciliation::Captured { .. } => Self::Staged,
            SnapshotReconciliation::Hydrated { .. } => Self::Hydrated,
            SnapshotReconciliation::Outcome(Reconciliation::NeedsDispatch) => Self::NeedsDispatch,
            SnapshotReconciliation::Outcome(Reconciliation::OwnedElsewhere) => Self::OwnedElsewhere,
            SnapshotReconciliation::Outcome(Reconciliation::Terminal { record, .. }) => {
                match record.phase {
                    Phase::Completed => Self::Completed,
                    Phase::Failed => Self::Failed,
                    Phase::Cancelled => Self::Cancelled,
                    Phase::Uncertain => Self::Uncertain,
                    Phase::Reserved | Phase::Dispatched => Self::InvalidReceipt,
                }
            }
        }
    }
    fn as_str(self) -> &'static str {
        match self {
            Self::Error => "error",
            Self::Staged => "staged",
            Self::Hydrated => "hydrated",
            Self::NeedsDispatch => "needs_dispatch",
            Self::OwnedElsewhere => "owned_elsewhere",
            Self::Completed => "completed",
            Self::Failed => "failed",
            Self::Cancelled => "cancelled",
            Self::Uncertain => "uncertain",
            Self::InvalidReceipt => "invalid_receipt",
        }
    }
    fn level(self) -> tracing::Level {
        match self {
            Self::Hydrated | Self::NeedsDispatch | Self::OwnedElsewhere => tracing::Level::DEBUG,
            Self::Error | Self::Failed | Self::Uncertain | Self::InvalidReceipt => {
                tracing::Level::ERROR
            }
            Self::Staged | Self::Completed | Self::Cancelled => tracing::Level::INFO,
        }
    }
}

fn publication_duration_millis(duration: Duration) -> u64 {
    u64::try_from(duration.as_millis()).unwrap_or(u64::MAX)
}

fn publication_log(
    phase: CompiledPublicationPhase,
    scope: &crate::sandbox::ledger::JobScope,
    export_epoch: u64,
    elapsed: Duration,
    disposition: CompiledPublicationDisposition,
    failure: Option<CompiledPublicationFailure>,
    cleanup_pending: bool,
) {
    let level = failure.map_or_else(|| disposition.level(), CompiledPublicationFailure::level);
    // No request, authority, record, error source, or runtime path enters this event.
    let job_key = crate::sandbox::dependency_bundle::hex(&scope.key);
    let duration_ms = publication_duration_millis(elapsed);
    let error_class = failure.map_or("none", CompiledPublicationFailure::as_str);
    let sqlstate = failure.map_or("none", CompiledPublicationFailure::sqlstate);
    macro_rules! emit {
        ($level:expr) => {
            tracing::event!(
                $level,
                event = "sandbox_compiled_publication",
                phase = phase.as_str(),
                disposition = disposition.as_str(),
                error_class,
                sqlstate,
                project_id = scope.project,
                job_key = job_key.as_str(),
                compilation_export_lease_epoch = export_epoch,
                duration_ms,
                cleanup_pending,
            )
        };
    }
    match level {
        tracing::Level::DEBUG => emit!(tracing::Level::DEBUG),
        tracing::Level::WARN => emit!(tracing::Level::WARN),
        tracing::Level::ERROR => emit!(tracing::Level::ERROR),
        _ => emit!(tracing::Level::INFO),
    }
}

#[cfg(test)]
#[path = "service_compiled_observability_tests.rs"]
mod observability_tests;
