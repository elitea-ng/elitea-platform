//! Fresh separate Read/Execute grants for one immutable selected snapshot.
use super::{SandboxCallError, SandboxClient, SandboxOutcome, submission_code};
use crate::{
    protocol::elitea::runtime::v1::{
        AuthorizeRustCompiledSnapshotRequestV1, RustCompiledSnapshotPurposeV1, SandboxJobStatusV1,
        SubmitRustCompiledSnapshotRequestV1, SubmitRustCompiledSnapshotResponseV1,
        SubmitSandboxJobResponseV1,
    },
    sandbox::{
        compiled_snapshot::{Binding, Purpose, SelectedSnapshot},
        request::PreparedJob,
    },
    transport::control_grpc::{ControlGrpcClient, ControlRpc},
};
use tonic::Request;
impl SandboxClient {
    pub(crate) async fn read_compiled_snapshot<R: ControlRpc>(
        &self,
        control: &ControlGrpcClient<R>,
        mut authorization: AuthorizeRustCompiledSnapshotRequestV1,
        job: &PreparedJob,
        binding: Binding,
    ) -> Result<Option<SelectedSnapshot>, SandboxCallError> {
        binding
            .validate_job(job)
            .map_err(|_| SandboxCallError::Invalid)?;
        authorization.audience.clone_from(&self.audience);
        authorization.purpose = RustCompiledSnapshotPurposeV1::Read as i32;
        authorization.prepared_job_json =
            job.to_transport().map_err(|_| SandboxCallError::Invalid)?;
        authorization.binding_json =
            serde_json::to_vec(&binding).map_err(|_| SandboxCallError::Invalid)?;
        authorization.compilation_job_key.clear();
        authorization.selected_descriptor_sha256.clear();
        let Some(response) = control
            .authorize_rust_compiled_snapshot(authorization)
            .await?
        else {
            return Ok(None);
        };
        SelectedSnapshot::select(binding, response.descriptor_json)
            .map(Some)
            .map_err(|_| SandboxCallError::InvalidReceipt)
    }
    #[allow(clippy::too_many_arguments)] // Keep unchanged native authority and exact bundle beside selected Execute identity.
    pub(crate) async fn submit_compiled_snapshot<R: ControlRpc>(
        &self,
        control: &ControlGrpcClient<R>,
        authorization: AuthorizeRustCompiledSnapshotRequestV1,
        native_authorization: crate::protocol::elitea::runtime::v1::AuthorizeSandboxJobRequestV1,
        job: &PreparedJob,
        selected: &SelectedSnapshot,
        bundle: Option<&crate::sandbox::dependency_bundle::DependencyBundle>,
    ) -> Result<SandboxOutcome, SandboxCallError> {
        self.submit_compiled_snapshot_with_intent(
            control,
            authorization,
            native_authorization,
            job,
            selected,
            bundle,
            None,
        )
        .await
    }
    #[allow(
        clippy::too_many_arguments,
        reason = "Keep the existing authenticated identity fields explicit."
    )]
    pub(crate) async fn submit_compiled_snapshot_with_intent<R: ControlRpc>(
        &self,
        control: &ControlGrpcClient<R>,
        authorization: AuthorizeRustCompiledSnapshotRequestV1,
        native_authorization: crate::protocol::elitea::runtime::v1::AuthorizeSandboxJobRequestV1,
        job: &PreparedJob,
        selected: &SelectedSnapshot,
        bundle: Option<&crate::sandbox::dependency_bundle::DependencyBundle>,
        intent: Option<&[u8]>,
    ) -> Result<SandboxOutcome, SandboxCallError> {
        if intent.is_some_and(|bytes| bytes.is_empty() || bytes.len() > 16 * 1024) {
            return Err(SandboxCallError::Invalid);
        }
        selected
            .control
            .binding
            .validate_job(job)
            .map_err(|_| SandboxCallError::Invalid)?;
        selected
            .descriptor
            .validate(&selected.control)
            .map_err(|_| SandboxCallError::Invalid)?;
        if bundle.is_some_and(|bundle| !job.matches_bundle(bundle) || bundle.native().is_none())
            || job.dependency_bundle_root().is_some() != bundle.is_some()
        {
            return Err(SandboxCallError::Invalid);
        }
        // Every retry observes the exact original Execute before renewed content authority.
        let observed = Box::pin(self.send_execute_snapshot(
            control,
            authorization.clone(),
            native_authorization.clone(),
            job,
            selected,
            bundle,
            ExecuteStage::Reconcile,
            None,
        ))
        .await?;
        if !observed.needs_admission {
            return decode_execution(observed);
        }
        if !empty_pending(&observed) || observed.native_hydration_ready {
            return Err(SandboxCallError::InvalidReceipt);
        }
        if let Some(bundle) = bundle {
            for index in 0..=bundle.file_count() {
                let imported = Box::pin(self.send_execute_snapshot(
                    control,
                    authorization.clone(),
                    native_authorization.clone(),
                    job,
                    selected,
                    Some(bundle),
                    ExecuteStage::Index(
                        u32::try_from(index).map_err(|_| SandboxCallError::Invalid)?,
                    ),
                    intent,
                ))
                .await?;
                if imported.needs_admission {
                    return Err(SandboxCallError::InvalidReceipt);
                }
                if !empty_pending(&imported) {
                    return decode_execution(imported);
                }
                if imported.native_hydration_ready {
                    break;
                }
                if index == bundle.file_count() {
                    return Err(SandboxCallError::InvalidReceipt);
                }
            }
        }
        let response = Box::pin(self.send_execute_snapshot(
            control,
            authorization,
            native_authorization,
            job,
            selected,
            bundle,
            ExecuteStage::Dispatch,
            intent,
        ))
        .await?;
        decode_execution(response)
    }

    #[allow(clippy::too_many_arguments)] // Preserve independently renewed Execute, selected-root Read, and native Content authority.
    async fn send_execute_snapshot<R: ControlRpc>(
        &self,
        control: &ControlGrpcClient<R>,
        mut authorization: AuthorizeRustCompiledSnapshotRequestV1,
        native_authorization: crate::protocol::elitea::runtime::v1::AuthorizeSandboxJobRequestV1,
        job: &PreparedJob,
        selected: &SelectedSnapshot,
        bundle: Option<&crate::sandbox::dependency_bundle::DependencyBundle>,
        stage: ExecuteStage,
        intent: Option<&[u8]>,
    ) -> Result<SubmitRustCompiledSnapshotResponseV1, SandboxCallError> {
        authorization.audience.clone_from(&self.audience);
        authorization.prepared_job_json =
            job.to_transport().map_err(|_| SandboxCallError::Invalid)?;
        authorization.binding_json =
            serde_json::to_vec(&selected.control.binding).map_err(|_| SandboxCallError::Invalid)?;
        authorization.compilation_job_key.clear();
        authorization.selected_descriptor_sha256 = selected
            .control
            .descriptor_sha256
            .as_ref()
            .ok_or(SandboxCallError::Invalid)?
            .raw()
            .map_err(|_| SandboxCallError::Invalid)?
            .to_vec();
        authorization.purpose = RustCompiledSnapshotPurposeV1::Execute as i32;
        let execute = control
            .authorize_rust_compiled_snapshot(authorization.clone())
            .await?
            .ok_or(SandboxCallError::Rejected)?;
        selected
            .require_same_descriptor(&execute.descriptor_json)
            .map_err(|_| SandboxCallError::InvalidReceipt)?;
        let read_grant = if matches!(stage, ExecuteStage::Reconcile) {
            None
        } else {
            authorization.purpose = RustCompiledSnapshotPurposeV1::Read as i32;
            let read = control
                .authorize_rust_compiled_snapshot(authorization)
                .await?
                .ok_or(SandboxCallError::Rejected)?;
            selected
                .require_same_descriptor(&read.descriptor_json)
                .map_err(|_| SandboxCallError::InvalidReceipt)?;
            read.grant
        };
        let transfer = bundle.filter(|_| !matches!(stage, ExecuteStage::Reconcile));
        let content_grant = if let Some(bundle) = transfer {
            Some(
                self.authorize_content(
                    control,
                    native_authorization,
                    &job.fingerprint().map_err(|_| SandboxCallError::Invalid)?,
                    bundle.root(),
                )
                .await?,
            )
        } else {
            None
        };
        let mut request = Request::new(SubmitRustCompiledSnapshotRequestV1 {
            grant: execute.grant,
            read_grant,
            prepared_job_json: job.to_transport().map_err(|_| SandboxCallError::Invalid)?,
            control_json: selected
                .control
                .bytes(Purpose::Execute)
                .map_err(|_| SandboxCallError::Invalid)?,
            descriptor_json: selected.descriptor_bytes.clone(),
            dependency_content_grant: content_grant,
            dependency_bundle_json: transfer
                .map(|bundle| bundle.record_json().to_vec())
                .unwrap_or_default(),
            reconcile_only: matches!(stage, ExecuteStage::Reconcile),
            native_hydration_index: match stage {
                ExecuteStage::Index(index) => Some(index),
                ExecuteStage::Reconcile | ExecuteStage::Dispatch => None,
            },
            code_execution_intent_json: if matches!(stage, ExecuteStage::Index(0))
                || matches!(stage, ExecuteStage::Dispatch) && bundle.is_none()
            {
                intent.unwrap_or_default().to_vec()
            } else {
                Vec::new()
            },
        });
        request.set_timeout(self.deadline);
        tokio::time::timeout(
            self.deadline,
            self.rpc.clone().submit_rust_compiled_snapshot(request),
        )
        .await
        .map_err(|_| SandboxCallError::Submission {
            code: tonic::Code::DeadlineExceeded,
        })?
        .map_err(|status| SandboxCallError::Submission {
            code: submission_code(&status),
        })
        .map(tonic::Response::into_inner)
    }
}

pub(crate) enum CompilationOutcome {
    Pending,
    Captured {
        canonical: Vec<u8>,
        job_key: [u8; 32],
    },
    Failed {
        code: String,
    },
    Cancelled,
    Uncertain {
        code: String,
    },
}
impl SandboxClient {
    pub(crate) async fn compile_snapshot<R: ControlRpc>(
        &self,
        control: &ControlGrpcClient<R>,
        authorization: AuthorizeRustCompiledSnapshotRequestV1,
        native_authorization: crate::protocol::elitea::runtime::v1::AuthorizeSandboxJobRequestV1,
        job: &PreparedJob,
        binding: Binding,
        bundle: Option<&crate::sandbox::dependency_bundle::DependencyBundle>,
    ) -> Result<CompilationOutcome, SandboxCallError> {
        use crate::sandbox::compiled_snapshot::Control;
        let snapshot = Control {
            revision: 1,
            snapshot_key_sha256: binding.key().map_err(|_| SandboxCallError::Invalid)?,
            binding,
            descriptor_sha256: None,
        };
        snapshot
            .binding
            .validate_job(job)
            .map_err(|_| SandboxCallError::Invalid)?;
        if bundle.is_some_and(|bundle| !job.matches_bundle(bundle) || bundle.native().is_none())
            || job.dependency_bundle_root().is_some() != bundle.is_some()
        {
            return Err(SandboxCallError::Invalid);
        }
        // An original dispatched or terminal compiler requires no fresh content grant.
        let observed = self
            .send_compile_snapshot(
                control,
                authorization.clone(),
                native_authorization.clone(),
                job,
                &snapshot,
                bundle,
                CompileStage::Reconcile,
            )
            .await?;
        if !observed.needs_admission {
            return decode_compilation(&snapshot, observed);
        }
        if !empty_pending(&observed) || observed.native_hydration_ready {
            return Err(SandboxCallError::InvalidReceipt);
        }
        if let Some(bundle) = bundle {
            for index in 0..=bundle.file_count() {
                let imported = self
                    .send_compile_snapshot(
                        control,
                        authorization.clone(),
                        native_authorization.clone(),
                        job,
                        &snapshot,
                        Some(bundle),
                        CompileStage::Index(
                            u32::try_from(index).map_err(|_| SandboxCallError::Invalid)?,
                        ),
                    )
                    .await?;
                if !empty_pending(&imported) || imported.needs_admission {
                    return decode_compilation(&snapshot, imported);
                }
                if imported.native_hydration_ready {
                    break;
                }
                if index == bundle.file_count() {
                    return Err(SandboxCallError::InvalidReceipt);
                }
            }
        }
        let response = self
            .send_compile_snapshot(
                control,
                authorization,
                native_authorization,
                job,
                &snapshot,
                bundle,
                CompileStage::Dispatch,
            )
            .await?;
        decode_compilation(&snapshot, response)
    }

    #[allow(clippy::too_many_arguments)] // Keep separate role requests beside exact compiler intent and inert index.
    async fn send_compile_snapshot<R: ControlRpc>(
        &self,
        control: &ControlGrpcClient<R>,
        mut authorization: AuthorizeRustCompiledSnapshotRequestV1,
        native_authorization: crate::protocol::elitea::runtime::v1::AuthorizeSandboxJobRequestV1,
        job: &PreparedJob,
        snapshot: &crate::sandbox::compiled_snapshot::Control,
        bundle: Option<&crate::sandbox::dependency_bundle::DependencyBundle>,
        stage: CompileStage,
    ) -> Result<SubmitRustCompiledSnapshotResponseV1, SandboxCallError> {
        authorization.audience.clone_from(&self.audience);
        authorization.purpose = RustCompiledSnapshotPurposeV1::Compile as i32;
        authorization.prepared_job_json =
            job.to_transport().map_err(|_| SandboxCallError::Invalid)?;
        authorization.binding_json =
            serde_json::to_vec(&snapshot.binding).map_err(|_| SandboxCallError::Invalid)?;
        authorization.compilation_job_key.clear();
        authorization.selected_descriptor_sha256.clear();
        let grant = control
            .authorize_rust_compiled_snapshot(authorization)
            .await?
            .ok_or(SandboxCallError::Rejected)?;
        if !grant.descriptor_json.is_empty() {
            return Err(SandboxCallError::InvalidReceipt);
        }
        let transfer = bundle.filter(|_| !matches!(stage, CompileStage::Reconcile));
        let content_grant = if let Some(bundle) = transfer {
            Some(
                self.authorize_content(
                    control,
                    native_authorization,
                    &job.fingerprint().map_err(|_| SandboxCallError::Invalid)?,
                    bundle.root(),
                )
                .await?,
            )
        } else {
            None
        };
        let mut request = Request::new(SubmitRustCompiledSnapshotRequestV1 {
            grant: grant.grant,
            prepared_job_json: job.to_transport().map_err(|_| SandboxCallError::Invalid)?,
            control_json: snapshot
                .bytes(Purpose::Compile)
                .map_err(|_| SandboxCallError::Invalid)?,
            dependency_content_grant: content_grant,
            dependency_bundle_json: transfer
                .map(|bundle| bundle.record_json().to_vec())
                .unwrap_or_default(),
            reconcile_only: matches!(stage, CompileStage::Reconcile),
            native_hydration_index: match stage {
                CompileStage::Index(index) => Some(index),
                CompileStage::Reconcile | CompileStage::Dispatch => None,
            },
            ..Default::default()
        });
        request.set_timeout(self.deadline);
        tokio::time::timeout(
            self.deadline,
            self.rpc.clone().submit_rust_compiled_snapshot(request),
        )
        .await
        .map_err(|_| SandboxCallError::Submission {
            code: tonic::Code::DeadlineExceeded,
        })?
        .map_err(|status| SandboxCallError::Submission {
            code: submission_code(&status),
        })
        .map(tonic::Response::into_inner)
    }
    #[allow(clippy::too_many_arguments)] // The publisher binds the original compile job and exact captured descriptor.
    pub(crate) async fn publish_snapshot<R: ControlRpc>(
        &self,
        control: &ControlGrpcClient<R>,
        mut authorization: AuthorizeRustCompiledSnapshotRequestV1,
        job: &PreparedJob,
        binding: &Binding,
        canonical: &[u8],
        compilation_job_key: &[u8; 32],
        phase: crate::protocol::elitea::runtime::v1::RustCompiledPublicationPhaseV1,
    ) -> Result<super::PublicationOutcome, SandboxCallError> {
        use crate::protocol::elitea::runtime::v1::PublishRustCompiledSnapshotRequestV1;
        authorization.audience.clone_from(&self.audience);
        authorization.purpose = RustCompiledSnapshotPurposeV1::Publish as i32;
        authorization.prepared_job_json =
            job.to_transport().map_err(|_| SandboxCallError::Invalid)?;
        authorization.binding_json =
            serde_json::to_vec(binding).map_err(|_| SandboxCallError::Invalid)?;
        authorization.compilation_job_key = compilation_job_key.to_vec();
        authorization.selected_descriptor_sha256.clear();
        let grant = control
            .authorize_rust_compiled_snapshot(authorization)
            .await?
            .ok_or(SandboxCallError::Rejected)?;
        if grant.descriptor_json != canonical {
            return Err(SandboxCallError::InvalidReceipt);
        }
        let mut request = Request::new(PublishRustCompiledSnapshotRequestV1 {
            publish_grant: grant.grant,
            phase: phase as i32,
        });
        request.set_timeout(self.deadline);
        let response = tokio::time::timeout(
            self.deadline,
            self.rpc.clone().publish_rust_compiled_snapshot(request),
        )
        .await
        .map_err(|_| SandboxCallError::Submission {
            code: tonic::Code::DeadlineExceeded,
        })?
        .map_err(|status| SandboxCallError::Submission {
            code: submission_code(&status),
        })?
        .into_inner();
        super::preparation::decode_publication(
            crate::protocol::elitea::runtime::v1::PublishSandboxDependenciesResponseV1 {
                status: response.status,
                failure_code: response.failure_code,
                cleanup_pending: response.cleanup_pending,
            },
        )
    }
}

#[cfg(all(test, feature = "sandbox-supervisor"))]
#[path = "client_compiled_tests.rs"]
mod tests;

#[derive(Clone, Copy)]
enum ExecuteStage {
    Reconcile,
    Index(u32),
    Dispatch,
}

fn decode_execution(
    response: SubmitRustCompiledSnapshotResponseV1,
) -> Result<SandboxOutcome, SandboxCallError> {
    if response.needs_admission
        || response.native_hydration_ready
        || !response.descriptor_json.is_empty()
        || !response.compilation_job_key.is_empty()
    {
        return Err(SandboxCallError::InvalidReceipt);
    }
    super::decode(SubmitSandboxJobResponseV1 {
        status: response.status,
        result_json: response.result_json,
        failure_code: response.failure_code,
        cleanup_pending: response.cleanup_pending,
    })
}

#[derive(Clone, Copy)]
enum CompileStage {
    Reconcile,
    Index(u32),
    Dispatch,
}

fn empty_pending(response: &SubmitRustCompiledSnapshotResponseV1) -> bool {
    response.status == i32::from(SandboxJobStatusV1::Pending)
        && response.result_json.is_empty()
        && response.descriptor_json.is_empty()
        && response.compilation_job_key.is_empty()
        && response.failure_code.is_empty()
        && !response.cleanup_pending
}

fn decode_compilation(
    snapshot: &crate::sandbox::compiled_snapshot::Control,
    response: SubmitRustCompiledSnapshotResponseV1,
) -> Result<CompilationOutcome, SandboxCallError> {
    if response.needs_admission || response.native_hydration_ready {
        return Err(SandboxCallError::InvalidReceipt);
    }
    let status = SandboxJobStatusV1::try_from(response.status)
        .map_err(|_| SandboxCallError::InvalidReceipt)?;
    if !response.descriptor_json.is_empty() {
        if status != SandboxJobStatusV1::Pending
            || !response.result_json.is_empty()
            || !response.failure_code.is_empty()
            || response.cleanup_pending
        {
            return Err(SandboxCallError::InvalidReceipt);
        }
        SelectedSnapshot::select(snapshot.binding.clone(), response.descriptor_json.clone())
            .map_err(|_| SandboxCallError::InvalidReceipt)?;
        let job_key = response
            .compilation_job_key
            .as_slice()
            .try_into()
            .map_err(|_| SandboxCallError::InvalidReceipt)?;
        return Ok(CompilationOutcome::Captured {
            canonical: response.descriptor_json,
            job_key,
        });
    }
    if !response.compilation_job_key.is_empty() {
        return Err(SandboxCallError::InvalidReceipt);
    }
    Ok(
        match super::decode(SubmitSandboxJobResponseV1 {
            status: response.status,
            result_json: response.result_json,
            failure_code: response.failure_code,
            cleanup_pending: response.cleanup_pending,
        })? {
            SandboxOutcome::Pending => CompilationOutcome::Pending,
            SandboxOutcome::Failed { code } => CompilationOutcome::Failed { code },
            SandboxOutcome::Cancelled => CompilationOutcome::Cancelled,
            SandboxOutcome::Uncertain { code } => CompilationOutcome::Uncertain { code },
            SandboxOutcome::Completed(_) => return Err(SandboxCallError::InvalidReceipt),
        },
    )
}
