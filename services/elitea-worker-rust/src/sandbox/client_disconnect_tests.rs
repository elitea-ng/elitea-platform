use super::*;
use crate::protocol::elitea::runtime::v1::sandbox_supervisor_service_server::{
    SandboxSupervisorService, SandboxSupervisorServiceServer,
};
use std::sync::Arc;
use tokio::{net::TcpListener, sync::Notify};
use tonic::{Response, Status};

struct PendingSupervisor(Arc<Notify>);

#[tonic::async_trait]
impl SandboxSupervisorService for PendingSupervisor {
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
        _: Request<crate::protocol::elitea::runtime::v1::HydrateSandboxDependenciesRequestV1>,
    ) -> Result<
        tonic::Response<crate::protocol::elitea::runtime::v1::HydrateSandboxDependenciesResponseV1>,
        tonic::Status,
    > {
        Err(tonic::Status::failed_precondition(
            "dependency hydration is outside this submission fixture",
        ))
    }
    async fn prepare_sandbox_dependencies(
        &self,
        _: Request<crate::protocol::elitea::runtime::v1::PrepareSandboxDependenciesRequestV1>,
    ) -> Result<
        tonic::Response<crate::protocol::elitea::runtime::v1::PrepareSandboxDependenciesResponseV1>,
        tonic::Status,
    > {
        Err(tonic::Status::failed_precondition(
            "dependency preparation is outside this submission fixture",
        ))
    }
    async fn publish_sandbox_dependencies(
        &self,
        _: Request<crate::protocol::elitea::runtime::v1::PublishSandboxDependenciesRequestV1>,
    ) -> Result<
        tonic::Response<crate::protocol::elitea::runtime::v1::PublishSandboxDependenciesResponseV1>,
        tonic::Status,
    > {
        Err(tonic::Status::failed_precondition(
            "dependency publication is outside this submission fixture",
        ))
    }

    async fn cancel_sandbox_job(
        &self,
        _: Request<crate::protocol::elitea::runtime::v1::CancelSandboxJobRequestV1>,
    ) -> Result<
        tonic::Response<crate::protocol::elitea::runtime::v1::CancelSandboxJobResponseV1>,
        tonic::Status,
    > {
        Err(tonic::Status::unimplemented(
            "not part of the submission fixture",
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
        _: Request<SubmitSandboxJobRequestV1>,
    ) -> Result<tonic::Response<SubmitSandboxJobResponseV1>, tonic::Status> {
        self.0.notify_one();
        std::future::pending().await
    }
}

#[tokio::test]
async fn connection_loss_after_submission_is_reconcilable() {
    let entered = Arc::new(Notify::new());
    let upstream = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let upstream_address = upstream.local_addr().unwrap();
    let server = tokio::spawn(
        tonic::transport::Server::builder()
            .add_service(SandboxSupervisorServiceServer::new(PendingSupervisor(
                entered.clone(),
            )))
            .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(upstream)),
    );
    let proxy = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = proxy.local_addr().unwrap();
    let connection = tokio::spawn(async move {
        let (mut client, _) = proxy.accept().await.unwrap();
        let mut upstream = tokio::net::TcpStream::connect(upstream_address)
            .await
            .unwrap();
        tokio::select! {
            () = entered.notified() => {},
            () = tokio::time::sleep(Duration::from_secs(5)) => {},
            result = tokio::io::copy_bidirectional(&mut client, &mut upstream) => {
                panic!("connection closed before submission: {result:?}");
            }
        }
        // Drop both sockets after the service receives the request, like a crash.
    });
    let channel = tonic::transport::Endpoint::from_shared(format!("http://{address}"))
        .unwrap()
        .connect()
        .await
        .unwrap();
    let client =
        SandboxClient::from_channel(channel, "test".into(), Duration::from_secs(5)).unwrap();
    let job = PreparedJob::new(
        crate::sandbox::request::Language::Rust,
        "test".into(),
        std::collections::BTreeMap::default(),
        format!("sha256:{}", "a".repeat(64)),
        "test".into(),
        5,
    )
    .unwrap();
    let result = client
        .submit_granted(
            SignedSandboxJobGrantV1 {
                key_id: "test".into(),
                claims_bytes: vec![1],
                signature: vec![0; 64],
            },
            &job,
        )
        .await;
    connection.await.unwrap();
    server.abort();
    let _ = server.await;
    let Err(SandboxCallError::Submission { code }) = result else {
        panic!("expected interrupted transport");
    };
    assert_eq!(code, Code::Unavailable);
}
