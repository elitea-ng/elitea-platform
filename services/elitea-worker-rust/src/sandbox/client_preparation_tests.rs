//! Local transport proof; these fixtures do not represent Main or supervisor authorization.
use super::*;
use crate::protocol::elitea::runtime::v1::{
    AuthorizeSandboxJobResponseV1, BeginExecutionRequestV1, BeginExecutionResponseV1,
    CancelSandboxJobRequestV1, CancelSandboxJobResponseV1, ClaimCommandRequestV1,
    ClaimCommandResponseV1, HydrateSandboxDependenciesRequestV1,
    HydrateSandboxDependenciesResponseV1, ObserveDesiredStateRequestV1,
    ObserveDesiredStateResponseV1, PrepareSettlementRequestV1, PrepareSettlementResponseV1,
    RenewLeaseRequestV1, RenewLeaseResponseV1, SubmitSandboxJobRequestV1,
    SubmitSandboxJobResponseV1,
    sandbox_supervisor_service_server::{SandboxSupervisorService, SandboxSupervisorServiceServer},
};
use crate::{
    sandbox::request::{Language, PreparedJob},
    transport::control_grpc::ControlGrpcConfig,
};
use std::{
    sync::{Arc, Mutex},
    time::Duration,
};
use tonic::{Response, Status};

#[derive(Clone, Default)]
struct RecordingControl(Arc<Mutex<Vec<AuthorizeSandboxJobRequestV1>>>);

#[tonic::async_trait]
impl ControlRpc for RecordingControl {
    async fn authorize_sandbox_job(
        &self,
        request: Request<AuthorizeSandboxJobRequestV1>,
    ) -> Result<Response<AuthorizeSandboxJobResponseV1>, Status> {
        let mut requests = self.0.lock().unwrap();
        requests.push(request.into_inner());
        let generation = u8::try_from(requests.len()).unwrap();
        Ok(Response::new(AuthorizeSandboxJobResponseV1 {
            grant: Some(SignedSandboxJobGrantV1 {
                key_id: "fixture".into(),
                claims_bytes: vec![generation],
                signature: vec![generation; 64],
            }),
            rejection: None,
        }))
    }
    async fn claim_command(
        &self,
        _: Request<ClaimCommandRequestV1>,
    ) -> Result<Response<ClaimCommandResponseV1>, Status> {
        Err(Status::unimplemented("fixture"))
    }
    async fn begin_execution(
        &self,
        _: Request<BeginExecutionRequestV1>,
    ) -> Result<Response<BeginExecutionResponseV1>, Status> {
        Err(Status::unimplemented("fixture"))
    }
    async fn authorize_invocation(
        &self,
        _: Request<crate::protocol::elitea::runtime::v1::AuthorizeInvocationRequestV1>,
    ) -> Result<Response<crate::protocol::elitea::runtime::v1::AuthorizeInvocationResponseV1>, Status>
    {
        Err(Status::unimplemented("fixture"))
    }
    async fn renew_lease(
        &self,
        _: Request<RenewLeaseRequestV1>,
    ) -> Result<Response<RenewLeaseResponseV1>, Status> {
        Err(Status::unimplemented("fixture"))
    }
    async fn observe_desired_state(
        &self,
        _: Request<ObserveDesiredStateRequestV1>,
    ) -> Result<Response<ObserveDesiredStateResponseV1>, Status> {
        Err(Status::unimplemented("fixture"))
    }
    async fn prepare_settlement(
        &self,
        _: Request<PrepareSettlementRequestV1>,
    ) -> Result<Response<PrepareSettlementResponseV1>, Status> {
        Err(Status::unimplemented("fixture"))
    }
}

#[derive(Clone, Default)]
struct RecordingSupervisor {
    indices: Arc<Mutex<Vec<u32>>>,
    hydration_indices: Arc<Mutex<Vec<u32>>>,
    hydration_jobs: Arc<Mutex<Vec<Vec<u8>>>>,
    hydration_intents: Arc<Mutex<Vec<Vec<u8>>>>,
    grants: Arc<Mutex<Vec<Vec<u8>>>>,
}

#[tonic::async_trait]
impl SandboxSupervisorService for RecordingSupervisor {
    async fn submit_rust_compiled_snapshot(
        &self,
        _request: Request<
            crate::protocol::elitea::runtime::v1::SubmitRustCompiledSnapshotRequestV1,
        >,
    ) -> Result<
        Response<crate::protocol::elitea::runtime::v1::SubmitRustCompiledSnapshotResponseV1>,
        Status,
    > {
        Err(Status::unimplemented("compiled snapshots disabled"))
    }
    async fn publish_rust_compiled_snapshot(
        &self,
        _request: Request<
            crate::protocol::elitea::runtime::v1::PublishRustCompiledSnapshotRequestV1,
        >,
    ) -> Result<
        Response<crate::protocol::elitea::runtime::v1::PublishRustCompiledSnapshotResponseV1>,
        Status,
    > {
        Err(Status::unimplemented("compiled snapshots disabled"))
    }

    async fn hydrate_sandbox_dependencies(
        &self,
        request: Request<HydrateSandboxDependenciesRequestV1>,
    ) -> Result<Response<HydrateSandboxDependenciesResponseV1>, Status> {
        let request = request.into_inner();
        let job = PreparedJob::from_transport(&request.prepared_job_json).unwrap();
        let bundle = DependencyBundle::parse_record(&request.bundle_json).unwrap();
        assert_eq!(job.dependency_bundle_root(), Some(bundle.root()));
        assert_eq!(request.bundle_json, super::tests::bundle_json());
        self.hydration_indices.lock().unwrap().push(request.index);
        self.hydration_jobs
            .lock()
            .unwrap()
            .push(request.prepared_job_json);
        self.hydration_intents
            .lock()
            .unwrap()
            .push(request.code_execution_intent_json);
        let execute = request.execution_grant.unwrap();
        let content = request.content_grant.unwrap();
        assert_ne!(execute.claims_bytes, content.claims_bytes);
        self.grants
            .lock()
            .unwrap()
            .extend([content.claims_bytes, execute.claims_bytes]);
        Ok(Response::new(HydrateSandboxDependenciesResponseV1 {
            ready: request.index == 1,
        }))
    }
    async fn prepare_sandbox_dependencies(
        &self,
        request: Request<PrepareSandboxDependenciesRequestV1>,
    ) -> Result<Response<PrepareSandboxDependenciesResponseV1>, Status> {
        let request = request.into_inner();
        PreparationJob::from_transport(&request.preparation_job_json).unwrap();
        self.grants
            .lock()
            .unwrap()
            .push(request.grant.unwrap().claims_bytes);
        Ok(Response::new(PrepareSandboxDependenciesResponseV1 {
            status: SandboxJobStatusV1::Pending.into(),
            bundle_json: super::tests::bundle_json(),
            failure_code: String::new(),
            cleanup_pending: false,
        }))
    }
    async fn publish_sandbox_dependencies(
        &self,
        request: Request<PublishSandboxDependenciesRequestV1>,
    ) -> Result<Response<PublishSandboxDependenciesResponseV1>, Status> {
        let request = request.into_inner();
        self.indices.lock().unwrap().push(request.index);
        self.grants
            .lock()
            .unwrap()
            .push(request.content_grant.unwrap().claims_bytes);
        Ok(Response::new(PublishSandboxDependenciesResponseV1 {
            status: if request.index == 1 {
                SandboxJobStatusV1::Completed
            } else {
                SandboxJobStatusV1::Pending
            }
            .into(),
            failure_code: String::new(),
            cleanup_pending: false,
        }))
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
        request: Request<SubmitSandboxJobRequestV1>,
    ) -> Result<Response<SubmitSandboxJobResponseV1>, Status> {
        let request = request.into_inner();
        let job = PreparedJob::from_transport(&request.prepared_job_json).unwrap();
        assert!(job.dependency_bundle_root().is_some());
        let bundle = DependencyBundle::parse_record(&request.dependency_bundle_json).unwrap();
        assert_eq!(job.dependency_bundle_root(), Some(bundle.root()));
        assert_eq!(request.dependency_bundle_json, super::tests::bundle_json());
        let execute = request.grant.unwrap();
        let content = request.dependency_content_grant.unwrap();
        assert_ne!(execute.claims_bytes, content.claims_bytes);
        self.grants
            .lock()
            .unwrap()
            .extend([content.claims_bytes, execute.claims_bytes]);
        Ok(Response::new(SubmitSandboxJobResponseV1 {
            status: SandboxJobStatusV1::Completed.into(),
            result_json:
                br#"{"revision":1,"status":"completed","exit_code":0,"stdout":"{}","stderr":""}"#
                    .to_vec(),
            failure_code: String::new(),
            cleanup_pending: false,
        }))
    }
    async fn cancel_sandbox_job(
        &self,
        _: Request<CancelSandboxJobRequestV1>,
    ) -> Result<Response<CancelSandboxJobResponseV1>, Status> {
        Err(Status::unimplemented("fixture"))
    }
}

#[tokio::test]
#[expect(
    clippy::too_many_lines,
    reason = "Keep the bounded transport lifecycle and phase assertions together"
)]
async fn preparation_publication_and_execution_keep_exact_identity_and_fresh_grants() {
    let service = RecordingSupervisor::default();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (shutdown, receive) = tokio::sync::oneshot::channel::<()>();
    let server = tokio::spawn(
        tonic::transport::Server::builder()
            .add_service(SandboxSupervisorServiceServer::new(service.clone()))
            .serve_with_incoming_shutdown(
                tokio_stream::wrappers::TcpListenerStream::new(listener),
                async {
                    let _ = receive.await;
                },
            ),
    );
    let channel = tonic::transport::Endpoint::from_shared(format!("http://{address}"))
        .unwrap()
        .connect()
        .await
        .unwrap();
    let sandbox =
        SandboxClient::from_channel(channel, "dns:sandbox.test".into(), Duration::from_secs(2))
            .unwrap();
    let control = RecordingControl::default();
    let client = ControlGrpcClient::new(
        control.clone(),
        ControlGrpcConfig {
            deadline: Duration::from_secs(2),
            workload_session_id: "workload".into(),
            producer_id: "worker".into(),
        },
    )
    .unwrap();
    let preparation = PreparationJob::new(
        "import humanize".into(),
        format!("sha256:{}", "a".repeat(64)),
        "prepare-v1".into(),
        60,
    )
    .unwrap();
    let authorization = AuthorizeSandboxJobRequestV1 {
        activation_id: "preparation-activation".into(),
        ..Default::default()
    };
    let outcome = sandbox
        .prepare(&client, authorization.clone(), &preparation)
        .await
        .unwrap();
    let PreparationOutcome::Pending(Some(bundle)) = outcome else {
        panic!("ready metadata must remain pending")
    };
    assert!(matches!(
        sandbox
            .publish(&client, authorization.clone(), &preparation, &bundle, 0)
            .await
            .unwrap(),
        PublicationOutcome::Pending
    ));
    assert!(matches!(
        sandbox
            .publish(&client, authorization.clone(), &preparation, &bundle, 1)
            .await
            .unwrap(),
        PublicationOutcome::Completed
    ));
    assert!(matches!(
        sandbox
            .publish(&client, authorization, &preparation, &bundle, 2)
            .await,
        Err(SandboxCallError::Invalid)
    ));
    let execution = PreparedJob::new(
        Language::Python,
        "import humanize".into(),
        std::collections::BTreeMap::new(),
        format!("sha256:{}", "b".repeat(64)),
        "execute-v1".into(),
        60,
    )
    .unwrap()
    .with_python_dependency_bundle(bundle.root().to_owned())
    .unwrap();
    let authorization = AuthorizeSandboxJobRequestV1 {
        activation_id: "execution-activation".into(),
        ..Default::default()
    };
    assert!(matches!(
        sandbox
            .submit(&client, authorization.clone(), &execution)
            .await,
        Err(SandboxCallError::Invalid)
    ));
    assert!(matches!(
        sandbox
            .hydrate_dependencies(&client, authorization.clone(), &execution, &bundle, 2)
            .await,
        Err(SandboxCallError::Invalid)
    ));
    let mismatched = PreparedJob::from_transport(&execution.to_transport().unwrap())
        .unwrap()
        .with_python_dependency_bundle("c".repeat(64))
        .unwrap();
    assert!(matches!(
        sandbox
            .hydrate_dependencies(&client, authorization.clone(), &mismatched, &bundle, 0)
            .await,
        Err(SandboxCallError::Invalid)
    ));
    for index in 0..=bundle.file_count() {
        assert_eq!(
            sandbox
                .hydrate_dependencies(&client, authorization.clone(), &execution, &bundle, index)
                .await
                .unwrap(),
            index == bundle.file_count()
        );
    }
    assert!(matches!(
        sandbox
            .submit_with_dependencies(&client, authorization, &execution, &bundle)
            .await
            .unwrap(),
        super::super::SandboxOutcome::Completed(_)
    ));
    {
        let requests = control.0.lock().unwrap().clone();
        assert_eq!(requests.len(), 9);
        let preparation_digest = preparation.fingerprint().unwrap().to_vec();
        let execution_digest = execution.fingerprint().unwrap().to_vec();
        assert_ne!(preparation_digest, execution_digest);
        let root = root_bytes(bundle.root()).unwrap().to_vec();
        for (index, request) in requests.iter().enumerate() {
            assert_eq!(request.audience, "dns:sandbox.test");
            assert!(!request.cancel_only);
            assert_eq!(
                request.activation_id,
                if index < 3 {
                    "preparation-activation"
                } else {
                    "execution-activation"
                }
            );
            assert_eq!(
                request.request_digest.as_slice(),
                if index < 3 {
                    preparation_digest.as_slice()
                } else {
                    execution_digest.as_slice()
                }
            );
            assert_eq!(
                request.dependency_bundle_sha256.as_slice(),
                if index == 0 || (index >= 3 && index % 2 == 0) {
                    &[]
                } else {
                    root.as_slice()
                }
            );
        }
        assert_eq!(*service.indices.lock().unwrap(), vec![0, 1]);
        assert_eq!(*service.hydration_indices.lock().unwrap(), vec![0, 1]);
        assert!(
            service
                .hydration_jobs
                .lock()
                .unwrap()
                .iter()
                .all(|job| *job == execution.to_transport().unwrap())
        );
        assert_eq!(
            *service.grants.lock().unwrap(),
            (1..=9).map(|grant| vec![grant]).collect::<Vec<_>>()
        );
        assert_eq!(
            *service.hydration_intents.lock().unwrap(),
            vec![Vec::<u8>::new(), Vec::<u8>::new()]
        );
        let broker = PreparedJob::from_transport(&execution.to_transport().unwrap())
            .unwrap()
            .with_platform_client(
                crate::sandbox::platform_client_binding::PlatformClientBinding::new(
                    "d".repeat(64),
                    8,
                    4096,
                )
                .unwrap(),
            )
            .unwrap();
        let intents = [
            b"fresh-original-intent-index-0".to_vec(),
            b"fresh-original-intent-index-1".to_vec(),
        ];
        for (index, intent) in intents.iter().enumerate() {
            assert_eq!(
                sandbox
                    .hydrate_dependencies_with_intent(
                        &client,
                        AuthorizeSandboxJobRequestV1 {
                            activation_id: "execution-activation".into(),
                            ..Default::default()
                        },
                        &broker,
                        &bundle,
                        index,
                        Some(intent),
                    )
                    .await
                    .unwrap(),
                index == bundle.file_count(),
            );
        }
        assert_eq!(*service.hydration_indices.lock().unwrap(), vec![0, 1, 0, 1]);
        assert_eq!(service.hydration_intents.lock().unwrap()[2..], intents);
        assert!(
            service.hydration_jobs.lock().unwrap()[2..]
                .iter()
                .all(|job| *job == broker.to_transport().unwrap())
        );
        let requests = control.0.lock().unwrap();
        assert_eq!(requests.len(), 13);
        for request in &requests[9..] {
            assert_eq!(request.activation_id, "execution-activation");
            assert_eq!(request.request_digest, broker.fingerprint().unwrap());
        }
    }
    shutdown.send(()).unwrap();
    tokio::time::timeout(Duration::from_secs(2), server)
        .await
        .unwrap()
        .unwrap()
        .unwrap();
}

#[tokio::test]
async fn broker_hydration_rejects_missing_or_excess_intent_before_requesting_authority() {
    let channel = tonic::transport::Endpoint::from_static("http://127.0.0.1:1").connect_lazy();
    let sandbox =
        SandboxClient::from_channel(channel, "dns:sandbox.test".into(), Duration::from_secs(1))
            .unwrap();
    let control = RecordingControl::default();
    let client = ControlGrpcClient::new(
        control.clone(),
        ControlGrpcConfig {
            deadline: Duration::from_secs(1),
            workload_session_id: "workload".into(),
            producer_id: "worker".into(),
        },
    )
    .unwrap();
    let bundle = DependencyBundle::parse_record(&super::tests::bundle_json()).unwrap();
    let job = PreparedJob::new(
        Language::Python,
        "return 1".into(),
        std::collections::BTreeMap::default(),
        format!("sha256:{}", "a".repeat(64)),
        "execute-v1".into(),
        60,
    )
    .unwrap()
    .with_python_dependency_bundle(bundle.root().into())
    .unwrap()
    .with_platform_client(
        crate::sandbox::platform_client_binding::PlatformClientBinding::new(
            "d".repeat(64),
            8,
            4096,
        )
        .unwrap(),
    )
    .unwrap();
    let oversized = vec![b'x'; 16 * 1024 + 1];
    for intent in [None, Some(&[][..]), Some(oversized.as_slice())] {
        assert!(matches!(
            sandbox
                .hydrate_dependencies_with_intent(
                    &client,
                    AuthorizeSandboxJobRequestV1::default(),
                    &job,
                    &bundle,
                    0,
                    intent
                )
                .await,
            Err(SandboxCallError::Invalid)
        ));
    }
    assert!(control.0.lock().unwrap().is_empty());
}
