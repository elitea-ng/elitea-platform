//! Local transport fixtures. Cryptographic role checks live in protocol tests;
//! these servers represent recorded receipts, not `PostgreSQL` or Linux acceptance.
use super::*;
use crate::protocol::elitea::runtime::v1::sandbox_supervisor_service_server::{
    SandboxSupervisorService, SandboxSupervisorServiceServer,
};
use crate::{
    protocol::elitea::runtime::v1::{
        AuthorizeInvocationRequestV1, AuthorizeInvocationResponseV1,
        AuthorizeRustCompiledSnapshotRequestV1, AuthorizeRustCompiledSnapshotResponseV1,
        AuthorizeSandboxJobRequestV1, AuthorizeSandboxJobResponseV1, BeginExecutionRequestV1,
        BeginExecutionResponseV1, CancelSandboxJobRequestV1, CancelSandboxJobResponseV1,
        ClaimCommandRequestV1, ClaimCommandResponseV1, HydrateSandboxDependenciesRequestV1,
        HydrateSandboxDependenciesResponseV1, ObserveDesiredStateRequestV1,
        ObserveDesiredStateResponseV1, PrepareSandboxDependenciesRequestV1,
        PrepareSandboxDependenciesResponseV1, PrepareSettlementRequestV1,
        PrepareSettlementResponseV1, PublishRustCompiledSnapshotRequestV1,
        PublishRustCompiledSnapshotResponseV1, PublishSandboxDependenciesRequestV1,
        PublishSandboxDependenciesResponseV1, RenewLeaseRequestV1, RenewLeaseResponseV1,
        RustCompiledSnapshotPurposeV1, SandboxJobStatusV1, SignedSandboxJobGrantV1,
        SubmitRustCompiledSnapshotRequestV1, SubmitRustCompiledSnapshotResponseV1,
        SubmitSandboxJobRequestV1, SubmitSandboxJobResponseV1,
    },
    transport::control_grpc::ControlGrpcConfig,
};
use std::{
    sync::{Arc, Mutex},
    time::Duration,
};
use tonic::{Response, Status};
#[derive(Clone)]
struct ControlFixture {
    calls: Arc<Mutex<Vec<AuthorizeRustCompiledSnapshotRequestV1>>>,
    missing: bool,
    corrupt: bool,
    descriptor: Vec<u8>,
    content_calls: Arc<Mutex<Vec<AuthorizeSandboxJobRequestV1>>>,
    cold: Arc<Mutex<Option<ColdFixture>>>,
}
#[async_trait::async_trait]
impl ControlRpc for ControlFixture {
    async fn authorize_invocation(
        &self,
        _request: Request<AuthorizeInvocationRequestV1>,
    ) -> Result<Response<AuthorizeInvocationResponseV1>, Status> {
        Err(Status::unimplemented("outside fixture"))
    }
    async fn claim_command(
        &self,
        _request: Request<ClaimCommandRequestV1>,
    ) -> Result<Response<ClaimCommandResponseV1>, Status> {
        Err(Status::unimplemented("outside fixture"))
    }
    async fn begin_execution(
        &self,
        _request: Request<BeginExecutionRequestV1>,
    ) -> Result<Response<BeginExecutionResponseV1>, Status> {
        Err(Status::unimplemented("outside fixture"))
    }
    async fn renew_lease(
        &self,
        _request: Request<RenewLeaseRequestV1>,
    ) -> Result<Response<RenewLeaseResponseV1>, Status> {
        Err(Status::unimplemented("outside fixture"))
    }
    async fn observe_desired_state(
        &self,
        _request: Request<ObserveDesiredStateRequestV1>,
    ) -> Result<Response<ObserveDesiredStateResponseV1>, Status> {
        Err(Status::unimplemented("outside fixture"))
    }
    async fn prepare_settlement(
        &self,
        _request: Request<PrepareSettlementRequestV1>,
    ) -> Result<Response<PrepareSettlementResponseV1>, Status> {
        Err(Status::unimplemented("outside fixture"))
    }
    async fn authorize_sandbox_job(
        &self,
        request: Request<AuthorizeSandboxJobRequestV1>,
    ) -> Result<Response<AuthorizeSandboxJobResponseV1>, Status> {
        let mut calls = self.content_calls.lock().unwrap();
        calls.push(request.into_inner());
        Ok(Response::new(AuthorizeSandboxJobResponseV1 {
            grant: Some(SignedSandboxJobGrantV1 {
                key_id: "content-transport-fixture".into(),
                claims_bytes: vec![u8::try_from(calls.len()).unwrap()],
                signature: vec![0; 64],
            }),
            ..Default::default()
        }))
    }
    async fn authorize_rust_compiled_snapshot(
        &self,
        request: Request<AuthorizeRustCompiledSnapshotRequestV1>,
    ) -> Result<Response<AuthorizeRustCompiledSnapshotResponseV1>, Status> {
        let input = request.into_inner();
        let sequence = {
            let mut calls = self.calls.lock().unwrap();
            calls.push(input.clone());
            u8::try_from(calls.len()).unwrap()
        };
        if self.missing && input.purpose == RustCompiledSnapshotPurposeV1::Read as i32 {
            return Err(Status::not_found("fixture cache expired"));
        }
        let descriptor = if input.purpose == RustCompiledSnapshotPurposeV1::Compile as i32 {
            Vec::new()
        } else if self.corrupt {
            b"{}".to_vec()
        } else {
            self.cold
                .lock()
                .unwrap()
                .as_ref()
                .filter(|fixture| fixture.purpose == Purpose::Execute)
                .map_or_else(
                    || self.descriptor.clone(),
                    |fixture| fixture.canonical.clone(),
                )
        };
        Ok(Response::new(AuthorizeRustCompiledSnapshotResponseV1 {
            grant: Some(SignedSandboxJobGrantV1 {
                key_id: "transport-fixture".into(),
                claims_bytes: vec![sequence],
                signature: vec![0; 64],
            }),
            descriptor_json: descriptor,
        }))
    }
}
#[derive(Clone)]
struct SupervisorFixture {
    calls: Arc<Mutex<Vec<SubmitRustCompiledSnapshotRequestV1>>>,
    admission_required: bool,
    cold: Arc<Mutex<Option<ColdFixture>>>,
    content_calls: Arc<Mutex<Vec<AuthorizeSandboxJobRequestV1>>>,
}
struct ColdFixture {
    canonical: Vec<u8>,
    file_count: u32,
    expire_once_at: Option<u32>,
    cancel_at: Option<u32>,
    missing_final_ready: bool,
    recorded: bool,
    purpose: Purpose,
    lose_dispatch_once: bool,
}
#[tonic::async_trait]
impl SandboxSupervisorService for SupervisorFixture {
    async fn lookup_sandbox_dependencies(
        &self,
        _: Request<crate::protocol::elitea::runtime::v1::LookupSandboxDependenciesRequestV1>,
    ) -> Result<
        Response<crate::protocol::elitea::runtime::v1::LookupSandboxDependenciesResponseV1>,
        Status,
    > {
        Err(Status::unimplemented(
            "frozen lookup is outside this fixture",
        ))
    }

    async fn hydrate_sandbox_workspace(
        &self,
        _request: Request<crate::protocol::elitea::runtime::v1::HydrateSandboxWorkspaceRequestV1>,
    ) -> Result<
        Response<crate::protocol::elitea::runtime::v1::HydrateSandboxWorkspaceResponseV1>,
        Status,
    > {
        Err(Status::unimplemented(
            "Workspace hydration is outside this fixture.",
        ))
    }
    async fn submit_sandbox_job(
        &self,
        _request: Request<SubmitSandboxJobRequestV1>,
    ) -> Result<Response<SubmitSandboxJobResponseV1>, Status> {
        panic!("ordinary execution/preparation must not be reached by snapshot fixtures")
    }
    async fn cancel_sandbox_job(
        &self,
        _request: Request<CancelSandboxJobRequestV1>,
    ) -> Result<Response<CancelSandboxJobResponseV1>, Status> {
        panic!("ordinary execution/preparation must not be reached by snapshot fixtures")
    }
    async fn prepare_sandbox_dependencies(
        &self,
        _request: Request<PrepareSandboxDependenciesRequestV1>,
    ) -> Result<Response<PrepareSandboxDependenciesResponseV1>, Status> {
        panic!("ordinary execution/preparation must not be reached by snapshot fixtures")
    }
    async fn publish_sandbox_dependencies(
        &self,
        _request: Request<PublishSandboxDependenciesRequestV1>,
    ) -> Result<Response<PublishSandboxDependenciesResponseV1>, Status> {
        panic!("ordinary execution/preparation must not be reached by snapshot fixtures")
    }
    async fn hydrate_sandbox_dependencies(
        &self,
        _request: Request<HydrateSandboxDependenciesRequestV1>,
    ) -> Result<Response<HydrateSandboxDependenciesResponseV1>, Status> {
        panic!("ordinary execution/preparation must not be reached by snapshot fixtures")
    }
    async fn publish_rust_compiled_snapshot(
        &self,
        _request: Request<PublishRustCompiledSnapshotRequestV1>,
    ) -> Result<Response<PublishRustCompiledSnapshotResponseV1>, Status> {
        Err(Status::unimplemented("outside fixture"))
    }
    async fn submit_rust_compiled_snapshot(
        &self,
        request: Request<SubmitRustCompiledSnapshotRequestV1>,
    ) -> Result<Response<SubmitRustCompiledSnapshotResponseV1>, Status> {
        let input = request.into_inner();
        self.calls.lock().unwrap().push(input.clone());
        if let Some(cold) = self.cold.lock().unwrap().as_mut() {
            assert_eq!(
                !input.descriptor_json.is_empty(),
                cold.purpose == Purpose::Execute
            );
            if input.reconcile_only {
                assert!(input.read_grant.is_none() && input.dependency_content_grant.is_none());
                assert!(input.dependency_bundle_json.is_empty());
            } else {
                assert_eq!(input.read_grant.is_some(), cold.purpose == Purpose::Execute);
                assert!(input.dependency_content_grant.is_some());
                assert!(!input.dependency_bundle_json.is_empty());
            }
            if input.reconcile_only && !cold.recorded {
                return Ok(Response::new(SubmitRustCompiledSnapshotResponseV1 {
                    status: SandboxJobStatusV1::Pending.into(),
                    needs_admission: true,
                    ..Default::default()
                }));
            }
            if let Some(index) = input.native_hydration_index {
                if cold.expire_once_at == Some(index) {
                    cold.expire_once_at = None;
                    return Err(Status::aborted(
                        "fixture native authority expired during transfer",
                    ));
                }
                if cold.cancel_at == Some(index) {
                    return Ok(Response::new(SubmitRustCompiledSnapshotResponseV1 {
                        status: SandboxJobStatusV1::Cancelled.into(),
                        ..Default::default()
                    }));
                }
                return Ok(Response::new(SubmitRustCompiledSnapshotResponseV1 {
                    status: SandboxJobStatusV1::Pending.into(),
                    native_hydration_ready: index == cold.file_count && !cold.missing_final_ready,
                    ..Default::default()
                }));
            }
            if cold.purpose == Purpose::Execute {
                if cold.lose_dispatch_once && !input.reconcile_only {
                    cold.lose_dispatch_once = false;
                    cold.recorded = true;
                    return Err(Status::unavailable(
                        "fixture lost acknowledgement after original dispatch",
                    ));
                }
                return Ok(Response::new(SubmitRustCompiledSnapshotResponseV1 {
                    status: SandboxJobStatusV1::Completed.into(),
                    result_json: br#"{"revision":1,"status":"completed","exit_code":0,"stdout":"{}","stderr":""}"#.to_vec(),
                    ..Default::default()
                }));
            }
            return Ok(Response::new(SubmitRustCompiledSnapshotResponseV1 {
                status: SandboxJobStatusV1::Pending.into(),
                descriptor_json: cold.canonical.clone(),
                compilation_job_key: vec![23; 32],
                ..Default::default()
            }));
        }
        if input.reconcile_only && self.admission_required {
            return Ok(Response::new(SubmitRustCompiledSnapshotResponseV1 {
                status: SandboxJobStatusV1::Pending.into(),
                needs_admission: true,
                ..Default::default()
            }));
        }
        Ok(Response::new(SubmitRustCompiledSnapshotResponseV1 {
            status: SandboxJobStatusV1::Completed.into(),
            result_json:
                br#"{"revision":1,"status":"completed","exit_code":0,"stdout":"{}","stderr":""}"#
                    .to_vec(),
            ..Default::default()
        }))
    }
}
async fn fixture(
    missing: bool,
    corrupt: bool,
    admission_required: bool,
) -> (
    SandboxClient,
    ControlGrpcClient<ControlFixture>,
    SupervisorFixture,
    tokio::task::JoinHandle<()>,
    Arc<Mutex<Vec<AuthorizeRustCompiledSnapshotRequestV1>>>,
) {
    let server = SupervisorFixture {
        calls: Arc::default(),
        admission_required,
        cold: Arc::default(),
        content_calls: Arc::default(),
    };
    // Keep real HTTP/2 framing without a host socket or external service.
    let (incoming_sender, incoming_receiver) = tokio::sync::mpsc::channel(1);
    let service = server.clone();
    let task = tokio::spawn(async move {
        tonic::transport::Server::builder()
            .add_service(SandboxSupervisorServiceServer::new(service))
            .serve_with_incoming(tokio_stream::wrappers::ReceiverStream::new(
                incoming_receiver,
            ))
            .await
            .unwrap();
    });
    let channel = tonic::transport::Endpoint::from_static("http://snapshot-fixture.invalid")
        .connect_with_connector(tower::service_fn(move |_| {
            let sender = incoming_sender.clone();
            async move {
                let (client, server) = tokio::io::duplex(64 * 1024);
                sender
                    .send(Ok::<_, std::io::Error>(server))
                    .await
                    .map_err(std::io::Error::other)?;
                Ok::<_, std::io::Error>(hyper_util::rt::TokioIo::new(client))
            }
        }))
        .await
        .unwrap();
    let client =
        SandboxClient::from_channel(channel, "sandbox-fixture".into(), Duration::from_secs(3))
            .unwrap();
    let calls = Arc::default();
    let control = ControlGrpcClient::new(
        ControlFixture {
            calls: Arc::clone(&calls),
            missing,
            corrupt,
            descriptor: rebound().1.descriptor_bytes,
            content_calls: Arc::clone(&server.content_calls),
            cold: Arc::clone(&server.cold),
        },
        ControlGrpcConfig {
            deadline: Duration::from_secs(3),
            workload_session_id: "fixture-session".into(),
            producer_id: "fixture-worker".into(),
        },
    )
    .unwrap();
    (client, control, server, task, calls)
}
fn selected() -> SelectedSnapshot {
    let control: crate::sandbox::compiled_snapshot::Control = serde_json::from_slice(
        include_bytes!("../../tests/fixtures/compiled-snapshot-v1/compile-control.json"),
    )
    .unwrap();
    SelectedSnapshot::select(
        control.binding,
        include_bytes!("../../tests/fixtures/compiled-snapshot-v1/descriptor.json").to_vec(),
    )
    .unwrap()
}
fn job() -> PreparedJob {
    use crate::sandbox::request::Language;
    let selected = selected();
    PreparedJob::new(
        Language::Rust,
        "pub fn run() {}".into(),
        std::collections::BTreeMap::new(),
        selected.control.binding.execution_image_digest,
        selected.control.binding.policy_revision,
        30,
    )
    .unwrap()
}
fn rebound() -> (PreparedJob, SelectedSnapshot) {
    let job = job();
    let original = selected();
    let profile =
        crate::sandbox::compiled_snapshot::SnapshotProfile::new(original.control.binding).unwrap();
    let binding = profile.binding(&job, "tenant", 2).unwrap();
    let mut descriptor = original.descriptor;
    descriptor.binding = binding.clone();
    descriptor.snapshot_key_sha256 = binding.key().unwrap();
    let selected = SelectedSnapshot::select(binding, descriptor.bytes().unwrap()).unwrap();
    (job, selected)
}

#[tokio::test]
async fn recorded_execution_reuses_completed_receipt_before_expired_cache_lookup() {
    let (client, control, server, task, control_calls) = fixture(true, false, false).await;
    let (job, mut selected) = rebound();
    selected.recovery = true;
    assert!(matches!(
        client
            .submit_compiled_snapshot(
                &control,
                AuthorizeRustCompiledSnapshotRequestV1::default(),
                native_authorization(),
                &job,
                &selected,
                None,
            )
            .await
            .unwrap(),
        SandboxOutcome::Completed(_)
    ));
    let calls = server.calls.lock().unwrap();
    assert_eq!(calls.len(), 1);
    assert!(calls[0].reconcile_only);
    assert!(calls[0].read_grant.is_none());
    drop(calls);
    let requests = control_calls.lock().unwrap();
    assert_eq!(requests.len(), 1);
    assert_eq!(
        requests[0].purpose,
        RustCompiledSnapshotPurposeV1::Execute as i32
    );
    assert_eq!(requests[0].selected_descriptor_sha256.len(), 32);
    drop(requests);
    task.abort();
}
#[tokio::test]
async fn predispatch_recovery_requires_fresh_read_and_keeps_same_selected_identity() {
    let (client, control, server, task, control_calls) = fixture(false, false, true).await;
    let (job, mut selected) = rebound();
    selected.recovery = true;
    assert!(matches!(
        client
            .submit_compiled_snapshot(
                &control,
                AuthorizeRustCompiledSnapshotRequestV1::default(),
                native_authorization(),
                &job,
                &selected,
                None,
            )
            .await
            .unwrap(),
        SandboxOutcome::Completed(_)
    ));
    let calls = server.calls.lock().unwrap();
    assert_eq!(calls.len(), 2);
    assert!(calls[0].reconcile_only);
    assert!(!calls[1].reconcile_only);
    assert!(calls[1].read_grant.is_some());
    assert_eq!(calls[0].control_json, calls[1].control_json);
    assert_eq!(calls[0].descriptor_json, calls[1].descriptor_json);
    drop(calls);
    let requests = control_calls.lock().unwrap();
    assert_eq!(
        requests
            .iter()
            .map(|request| request.purpose)
            .collect::<Vec<_>>(),
        vec![
            RustCompiledSnapshotPurposeV1::Execute as i32,
            RustCompiledSnapshotPurposeV1::Execute as i32,
            RustCompiledSnapshotPurposeV1::Read as i32
        ]
    );
    assert!(
        requests
            .iter()
            .all(|request| request.selected_descriptor_sha256.len() == 32)
    );
    drop(requests);
    task.abort();
}
#[tokio::test]
async fn selected_read_absence_does_not_fall_back_or_dispatch() {
    let (client, control, server, task, _control_calls) = fixture(true, false, true).await;
    let (job, selected) = rebound();
    assert!(
        client
            .submit_compiled_snapshot(
                &control,
                AuthorizeRustCompiledSnapshotRequestV1::default(),
                native_authorization(),
                &job,
                &selected,
                None,
            )
            .await
            .is_err()
    );
    {
        let calls = server.calls.lock().unwrap();
        assert_eq!(calls.len(), 1);
        assert!(calls[0].reconcile_only);
    }
    assert!(
        client
            .read_compiled_snapshot(
                &control,
                AuthorizeRustCompiledSnapshotRequestV1::default(),
                &job,
                selected.control.binding
            )
            .await
            .unwrap()
            .is_none()
    );
    task.abort();
}
#[tokio::test]
async fn changed_selected_descriptor_stops_before_supervisor_admission() {
    let (client, control, server, task, _control_calls) = fixture(false, true, false).await;
    let (job, mut selected) = rebound();
    selected.recovery = true;
    assert!(matches!(
        client
            .submit_compiled_snapshot(
                &control,
                AuthorizeRustCompiledSnapshotRequestV1::default(),
                native_authorization(),
                &job,
                &selected,
                None,
            )
            .await,
        Err(SandboxCallError::InvalidReceipt)
    ));
    assert!(server.calls.lock().unwrap().is_empty());
    task.abort();
}

fn native_rebound() -> (
    PreparedJob,
    SelectedSnapshot,
    crate::sandbox::dependency_bundle::DependencyBundle,
) {
    use crate::sandbox::{
        native_bundle::{NativeKind, NativePlatform, canonical},
        request::{Language, NativeDependencies},
    };
    let declaration = "[dependencies]\n";
    let source = crate::sandbox::compiled_snapshot::ContentSha256::of(declaration.as_bytes());
    let mut value: serde_json::Value =
        serde_json::from_slice(include_bytes!("native-cargo-v2.json")).unwrap();
    value["source_sha256"] = serde_json::json!(source.as_str());
    value["payload"]["declaration_sha256"] = serde_json::json!(source.as_str());
    value.as_object_mut().unwrap().remove("digest");
    let root = crate::sandbox::compiled_snapshot::ContentSha256::of(&canonical(&value).unwrap());
    value["digest"] = serde_json::json!(root.as_str());
    let bundle = crate::sandbox::dependency_bundle::DependencyBundle::parse_record(
        &canonical(&value).unwrap(),
    )
    .unwrap();
    let native = bundle.native().unwrap();
    let job = PreparedJob::new(
        Language::Rust,
        "pub fn run() {}".into(),
        std::collections::BTreeMap::new(),
        native.record.execution_image_digest.clone(),
        native.record.execution_policy_revision.clone(),
        30,
    )
    .unwrap()
    .with_native_dependency_bundle(
        root.as_str().into(),
        NativeDependencies {
            kind: NativeKind::Cargo,
            platform: NativePlatform {
                os: "linux".into(),
                arch: "amd64".into(),
                abi: "gnu".into(),
            },
            preparation_sha256: native.record.preparation_sha256.clone(),
            source_sha256: source.as_str().into(),
            dependencies_toml: Some(declaration.into()),
        },
    )
    .unwrap();
    assert!(job.matches_bundle(&bundle));
    let original = selected();
    let mut template = original.control.binding;
    template
        .compilation_image_digest
        .clone_from(&native.record.execution_image_digest);
    template
        .execution_image_digest
        .clone_from(&native.record.execution_image_digest);
    template
        .policy_revision
        .clone_from(&native.record.execution_policy_revision);
    let binding = crate::sandbox::compiled_snapshot::SnapshotProfile::new(template)
        .unwrap()
        .binding(&job, "tenant", 2)
        .unwrap();
    let mut descriptor = original.descriptor;
    descriptor.binding = binding.clone();
    descriptor.snapshot_key_sha256 = binding.key().unwrap();
    let selected = SelectedSnapshot::select(binding, descriptor.bytes().unwrap()).unwrap();
    (job, selected, bundle)
}

fn enable_cold(server: &SupervisorFixture, selected: &SelectedSnapshot, files: usize) {
    *server.cold.lock().unwrap() = Some(ColdFixture {
        canonical: selected.descriptor_bytes.clone(),
        file_count: u32::try_from(files).unwrap(),
        expire_once_at: None,
        cancel_at: None,
        missing_final_ready: false,
        recorded: false,
        purpose: Purpose::Compile,
        lose_dispatch_once: false,
    });
}
fn compile_authorization() -> AuthorizeRustCompiledSnapshotRequestV1 {
    AuthorizeRustCompiledSnapshotRequestV1 {
        activation_id: "original-compiler".into(),
        ..Default::default()
    }
}
fn native_authorization() -> AuthorizeSandboxJobRequestV1 {
    AuthorizeSandboxJobRequestV1 {
        activation_id: "original-compiler".into(),
        ..Default::default()
    }
}

#[tokio::test]
async fn cold_native_indices_renew_both_roles_and_preserve_original_compiler_intent() {
    let (client, control, server, task, compiled_calls) = fixture(false, false, false).await;
    let (job, selected, bundle) = native_rebound();
    enable_cold(&server, &selected, bundle.file_count());
    assert!(matches!(
        client
            .compile_snapshot(
                &control,
                compile_authorization(),
                native_authorization(),
                &job,
                selected.control.binding.clone(),
                Some(&bundle)
            )
            .await
            .unwrap(),
        CompilationOutcome::Captured { .. }
    ));
    let calls = server.calls.lock().unwrap();
    assert_eq!(calls.len(), bundle.file_count() + 3);
    assert!(calls[0].reconcile_only);
    assert!(calls[0].dependency_content_grant.is_none());
    assert!(calls[0].dependency_bundle_json.is_empty());
    let indices = &calls[1..=bundle.file_count() + 1];
    assert_eq!(
        indices
            .iter()
            .map(|call| call.native_hydration_index.unwrap())
            .collect::<Vec<_>>(),
        (0..=u32::try_from(bundle.file_count()).unwrap()).collect::<Vec<_>>()
    );
    let fresh: std::collections::BTreeSet<_> = indices
        .iter()
        .map(|call| {
            call.dependency_content_grant
                .as_ref()
                .unwrap()
                .claims_bytes
                .clone()
        })
        .collect();
    assert_eq!(fresh.len(), indices.len());
    assert!(
        calls
            .iter()
            .all(|call| call.control_json == calls[0].control_json
                && call.prepared_job_json == calls[0].prepared_job_json)
    );
    assert!(calls.last().unwrap().native_hydration_index.is_none());
    assert!(!calls.last().unwrap().reconcile_only);
    assert!(
        compiled_calls
            .lock()
            .unwrap()
            .iter()
            .all(|call| call.activation_id == "original-compiler"
                && call.purpose == RustCompiledSnapshotPurposeV1::Compile as i32)
    );
    let content_calls = server.content_calls.lock().unwrap();
    assert!(
        content_calls
            .iter()
            .all(|call| call.activation_id == "original-compiler"
                && call.request_digest == job.fingerprint().unwrap()
                && call.dependency_bundle_sha256.len() == 32
                && !call.cancel_only)
    );
    drop(content_calls);
    task.abort();
}

#[tokio::test]
async fn expired_index_retries_original_compiler_and_replays_only_immutable_imports() {
    let (client, control, server, task, _) = fixture(false, false, false).await;
    let (job, selected, bundle) = native_rebound();
    enable_cold(&server, &selected, bundle.file_count());
    server.cold.lock().unwrap().as_mut().unwrap().expire_once_at = Some(1);
    assert!(matches!(
        client
            .compile_snapshot(
                &control,
                compile_authorization(),
                native_authorization(),
                &job,
                selected.control.binding.clone(),
                Some(&bundle)
            )
            .await,
        Err(SandboxCallError::Submission {
            code: tonic::Code::Aborted
        })
    ));
    assert!(matches!(
        client
            .compile_snapshot(
                &control,
                compile_authorization(),
                native_authorization(),
                &job,
                selected.control.binding.clone(),
                Some(&bundle)
            )
            .await
            .unwrap(),
        CompilationOutcome::Captured { .. }
    ));
    let calls = server.calls.lock().unwrap();
    assert!(
        calls
            .iter()
            .all(|call| call.control_json == calls[0].control_json
                && call.prepared_job_json == calls[0].prepared_job_json)
    );
    let index_zero: Vec<_> = calls
        .iter()
        .filter(|call| call.native_hydration_index == Some(0))
        .collect();
    assert_eq!(index_zero.len(), 2);
    assert_ne!(
        index_zero[0]
            .dependency_content_grant
            .as_ref()
            .unwrap()
            .claims_bytes,
        index_zero[1]
            .dependency_content_grant
            .as_ref()
            .unwrap()
            .claims_bytes
    );
    assert_eq!(
        calls
            .iter()
            .filter(|call| !call.reconcile_only && call.native_hydration_index.is_none())
            .count(),
        1
    );
    task.abort();
}

#[tokio::test]
async fn compiler_receipt_recovery_does_not_renew_or_transfer_native_content() {
    let (client, control, server, task, _) = fixture(false, false, false).await;
    let (job, selected, bundle) = native_rebound();
    enable_cold(&server, &selected, bundle.file_count());
    server.cold.lock().unwrap().as_mut().unwrap().recorded = true;
    assert!(matches!(
        client
            .compile_snapshot(
                &control,
                compile_authorization(),
                native_authorization(),
                &job,
                selected.control.binding.clone(),
                Some(&bundle)
            )
            .await
            .unwrap(),
        CompilationOutcome::Captured { .. }
    ));
    assert_eq!(server.calls.lock().unwrap().len(), 1);
    assert!(server.content_calls.lock().unwrap().is_empty());
    task.abort();
}

#[tokio::test]
async fn native_index_cancellation_stops_remaining_imports_and_dispatch() {
    let (client, control, server, task, _) = fixture(false, false, false).await;
    let (job, selected, bundle) = native_rebound();
    enable_cold(&server, &selected, bundle.file_count());
    server.cold.lock().unwrap().as_mut().unwrap().cancel_at = Some(1);
    assert!(matches!(
        client
            .compile_snapshot(
                &control,
                compile_authorization(),
                native_authorization(),
                &job,
                selected.control.binding.clone(),
                Some(&bundle)
            )
            .await
            .unwrap(),
        CompilationOutcome::Cancelled
    ));
    let calls = server.calls.lock().unwrap();
    assert_eq!(calls.len(), 3);
    assert!(
        calls
            .iter()
            .all(|call| call.reconcile_only || call.native_hydration_index.is_some())
    );
    task.abort();
}

#[tokio::test]
async fn missing_final_native_readiness_never_dispatches_compilation() {
    let (client, control, server, task, _) = fixture(false, false, false).await;
    let (job, selected, bundle) = native_rebound();
    enable_cold(&server, &selected, bundle.file_count());
    server
        .cold
        .lock()
        .unwrap()
        .as_mut()
        .unwrap()
        .missing_final_ready = true;
    assert!(matches!(
        client
            .compile_snapshot(
                &control,
                compile_authorization(),
                native_authorization(),
                &job,
                selected.control.binding.clone(),
                Some(&bundle)
            )
            .await,
        Err(SandboxCallError::InvalidReceipt)
    ));
    assert!(
        server
            .calls
            .lock()
            .unwrap()
            .iter()
            .all(|call| call.reconcile_only || call.native_hydration_index.is_some())
    );
    task.abort();
}

fn enable_warm(server: &SupervisorFixture, selected: &SelectedSnapshot, files: usize) {
    enable_cold(server, selected, files);
    server.cold.lock().unwrap().as_mut().unwrap().purpose = Purpose::Execute;
}
fn execute_authorization() -> AuthorizeRustCompiledSnapshotRequestV1 {
    AuthorizeRustCompiledSnapshotRequestV1 {
        activation_id: "original-execute".into(),
        ..Default::default()
    }
}
fn execution_native_authorization() -> AuthorizeSandboxJobRequestV1 {
    AuthorizeSandboxJobRequestV1 {
        activation_id: "original-execute".into(),
        ..Default::default()
    }
}

#[tokio::test]
async fn warm_native_indices_renew_execute_read_and_content_with_exact_selected_identity() {
    let (client, control, server, task, compiled_calls) = fixture(false, false, false).await;
    let (job, selected, bundle) = native_rebound();
    enable_warm(&server, &selected, bundle.file_count());
    assert!(matches!(
        client
            .submit_compiled_snapshot(
                &control,
                execute_authorization(),
                execution_native_authorization(),
                &job,
                &selected,
                Some(&bundle)
            )
            .await
            .unwrap(),
        SandboxOutcome::Completed(_)
    ));
    let calls = server.calls.lock().unwrap();
    assert_eq!(calls.len(), bundle.file_count() + 3);
    assert!(
        calls[0].reconcile_only
            && calls[0].read_grant.is_none()
            && calls[0].dependency_content_grant.is_none()
    );
    for (index, call) in calls[1..=bundle.file_count() + 1].iter().enumerate() {
        assert_eq!(
            call.native_hydration_index,
            Some(u32::try_from(index).unwrap())
        );
        assert!(call.read_grant.is_some() && call.dependency_content_grant.is_some());
    }
    assert!(
        calls
            .iter()
            .all(|call| call.control_json == calls[0].control_json
                && call.prepared_job_json == calls[0].prepared_job_json
                && call.descriptor_json == selected.descriptor_bytes)
    );
    assert!(calls.last().unwrap().native_hydration_index.is_none());
    drop(calls);
    let requests = compiled_calls.lock().unwrap();
    assert_eq!(
        requests
            .iter()
            .filter(|request| request.purpose == RustCompiledSnapshotPurposeV1::Execute as i32)
            .count(),
        bundle.file_count() + 3
    );
    assert_eq!(
        requests
            .iter()
            .filter(|request| request.purpose == RustCompiledSnapshotPurposeV1::Read as i32)
            .count(),
        bundle.file_count() + 2
    );
    assert!(requests.iter().all(|request| {
        request.activation_id == "original-execute"
            && request.selected_descriptor_sha256
                == selected
                    .control
                    .descriptor_sha256
                    .as_ref()
                    .unwrap()
                    .raw()
                    .unwrap()
    }));
    drop(requests);
    let content = server.content_calls.lock().unwrap();
    assert_eq!(content.len(), bundle.file_count() + 2);
    assert!(content.iter().all(|request| {
        request.activation_id == "original-execute"
            && request.request_digest == job.fingerprint().unwrap()
            && request.dependency_bundle_sha256
                == crate::sandbox::compiled_snapshot::ContentSha256::parse(bundle.root().into())
                    .unwrap()
                    .raw()
                    .unwrap()
    }));
    drop(content);
    task.abort();
}

#[tokio::test]
async fn warm_expired_or_fenced_index_retries_original_execute_with_fresh_three_roles() {
    let (client, control, server, task, compiled_calls) = fixture(false, false, false).await;
    let (job, selected, bundle) = native_rebound();
    enable_warm(&server, &selected, bundle.file_count());
    server.cold.lock().unwrap().as_mut().unwrap().expire_once_at = Some(1);
    assert!(matches!(
        client
            .submit_compiled_snapshot(
                &control,
                execute_authorization(),
                execution_native_authorization(),
                &job,
                &selected,
                Some(&bundle)
            )
            .await,
        Err(SandboxCallError::Submission {
            code: tonic::Code::Aborted
        })
    ));
    assert!(matches!(
        client
            .submit_compiled_snapshot(
                &control,
                execute_authorization(),
                execution_native_authorization(),
                &job,
                &selected,
                Some(&bundle)
            )
            .await
            .unwrap(),
        SandboxOutcome::Completed(_)
    ));
    let calls = server.calls.lock().unwrap();
    assert!(
        calls
            .iter()
            .all(|call| call.control_json == calls[0].control_json
                && call.descriptor_json == calls[0].descriptor_json
                && call.prepared_job_json == calls[0].prepared_job_json)
    );
    let repeated: Vec<_> = calls
        .iter()
        .filter(|call| call.native_hydration_index == Some(0))
        .collect();
    assert_eq!(repeated.len(), 2);
    for grants in [
        (
            repeated[0].grant.as_ref().unwrap(),
            repeated[1].grant.as_ref().unwrap(),
        ),
        (
            repeated[0].read_grant.as_ref().unwrap(),
            repeated[1].read_grant.as_ref().unwrap(),
        ),
        (
            repeated[0].dependency_content_grant.as_ref().unwrap(),
            repeated[1].dependency_content_grant.as_ref().unwrap(),
        ),
    ] {
        assert_ne!(grants.0.claims_bytes, grants.1.claims_bytes);
    }
    assert_eq!(
        calls
            .iter()
            .filter(|call| !call.reconcile_only && call.native_hydration_index.is_none())
            .count(),
        1
    );
    drop(calls);
    assert!(
        compiled_calls
            .lock()
            .unwrap()
            .iter()
            .all(|call| call.purpose != RustCompiledSnapshotPurposeV1::Compile as i32)
    );
    task.abort();
}

#[tokio::test]
async fn warm_completed_receipt_precedes_expired_read_lookup_and_content_renewal() {
    let (client, control, server, task, compiled_calls) = fixture(true, false, false).await;
    let (job, selected, bundle) = native_rebound();
    enable_warm(&server, &selected, bundle.file_count());
    server.cold.lock().unwrap().as_mut().unwrap().recorded = true;
    assert!(matches!(
        client
            .submit_compiled_snapshot(
                &control,
                execute_authorization(),
                execution_native_authorization(),
                &job,
                &selected,
                Some(&bundle)
            )
            .await
            .unwrap(),
        SandboxOutcome::Completed(_)
    ));
    assert_eq!(server.calls.lock().unwrap().len(), 1);
    assert!(server.content_calls.lock().unwrap().is_empty());
    let calls = compiled_calls.lock().unwrap();
    assert_eq!(calls.len(), 1);
    assert_eq!(
        calls[0].purpose,
        RustCompiledSnapshotPurposeV1::Execute as i32
    );
    drop(calls);
    task.abort();
}

#[tokio::test]
async fn warm_cancellation_or_missing_final_readiness_cannot_dispatch() {
    for cancelled in [true, false] {
        let (client, control, server, task, _) = fixture(false, false, false).await;
        let (job, selected, bundle) = native_rebound();
        enable_warm(&server, &selected, bundle.file_count());
        {
            let mut cold = server.cold.lock().unwrap();
            if cancelled {
                cold.as_mut().unwrap().cancel_at = Some(1);
            } else {
                cold.as_mut().unwrap().missing_final_ready = true;
            }
        }
        let result = client
            .submit_compiled_snapshot(
                &control,
                execute_authorization(),
                execution_native_authorization(),
                &job,
                &selected,
                Some(&bundle),
            )
            .await;
        if cancelled {
            assert!(matches!(result, Ok(SandboxOutcome::Cancelled)));
        } else {
            assert!(matches!(result, Err(SandboxCallError::InvalidReceipt)));
        }
        assert!(
            server
                .calls
                .lock()
                .unwrap()
                .iter()
                .all(|call| call.reconcile_only || call.native_hydration_index.is_some())
        );
        task.abort();
    }
}

#[tokio::test]
async fn warm_unknown_dispatch_reconciles_original_receipt_without_reimport_or_recompile() {
    let (client, control, server, task, compiled_calls) = fixture(false, false, false).await;
    let (job, selected, bundle) = native_rebound();
    enable_warm(&server, &selected, bundle.file_count());
    server
        .cold
        .lock()
        .unwrap()
        .as_mut()
        .unwrap()
        .lose_dispatch_once = true;
    assert!(matches!(
        client
            .submit_compiled_snapshot(
                &control,
                execute_authorization(),
                execution_native_authorization(),
                &job,
                &selected,
                Some(&bundle)
            )
            .await,
        Err(SandboxCallError::Submission {
            code: tonic::Code::Unavailable
        })
    ));
    let previous = server.content_calls.lock().unwrap().len();
    let previous_roles = compiled_calls.lock().unwrap().len();
    assert!(matches!(
        client
            .submit_compiled_snapshot(
                &control,
                execute_authorization(),
                execution_native_authorization(),
                &job,
                &selected,
                Some(&bundle)
            )
            .await
            .unwrap(),
        SandboxOutcome::Completed(_)
    ));
    assert_eq!(server.content_calls.lock().unwrap().len(), previous);
    let calls = compiled_calls.lock().unwrap();
    assert_eq!(calls.len(), previous_roles + 1);
    assert_eq!(
        calls.last().unwrap().purpose,
        RustCompiledSnapshotPurposeV1::Execute as i32
    );
    drop(calls);
    let calls = server.calls.lock().unwrap();
    assert_eq!(
        calls
            .iter()
            .filter(|call| !call.reconcile_only && call.native_hydration_index.is_none())
            .count(),
        1
    );
    assert!(calls.last().unwrap().reconcile_only);
    drop(calls);
    task.abort();
}

#[tokio::test]
async fn warm_changed_native_root_fails_before_authority_or_runtime_admission() {
    let (client, control, server, task, compiled_calls) = fixture(false, false, false).await;
    let (job, selected, _) = native_rebound();
    let changed = crate::sandbox::dependency_bundle::DependencyBundle::parse_record(
        include_bytes!("native-cargo-v2.json"),
    )
    .unwrap();
    assert!(matches!(
        client
            .submit_compiled_snapshot(
                &control,
                execute_authorization(),
                execution_native_authorization(),
                &job,
                &selected,
                Some(&changed)
            )
            .await,
        Err(SandboxCallError::Invalid)
    ));
    assert!(
        compiled_calls.lock().unwrap().is_empty()
            && server.content_calls.lock().unwrap().is_empty()
            && server.calls.lock().unwrap().is_empty()
    );
    task.abort();
}
