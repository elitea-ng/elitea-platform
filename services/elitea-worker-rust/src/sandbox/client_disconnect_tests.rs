use super::*;
use crate::protocol::elitea::runtime::v1::sandbox_supervisor_service_server::{
    SandboxSupervisorService, SandboxSupervisorServiceServer,
};
use std::sync::Arc;
use tokio::{net::TcpListener, sync::Notify};

struct PendingSupervisor(Arc<Notify>);

#[tonic::async_trait]
impl SandboxSupervisorService for PendingSupervisor {
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
            _ = entered.notified() => {},
            _ = tokio::time::sleep(Duration::from_secs(5)) => {},
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
        Default::default(),
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
