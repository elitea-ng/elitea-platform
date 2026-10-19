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
    let Err(SandboxCallError::Submission { code, message }) = result else {
        panic!("expected interrupted transport");
    };
    assert_eq!(code, Code::Unavailable);
    // A broken connection carries no supervisor instruction.
    assert_eq!(
        message, "",
        "a local transport failure reported a server message"
    );
}

/// A peer that stops answering without closing (a force-deleted Pod) is found by
/// the HTTP/2 keepalive within its bound, the call fails `Unavailable`, and the
/// channel reconnects once the peer answers again.
#[tokio::test]
#[allow(
    clippy::too_many_lines,
    reason = "One ordered fault scenario: proxy, black hole, bound, reconnect."
)]
async fn black_holed_peer_fails_within_the_keepalive_bound_and_the_channel_reconnects() {
    use crate::transport::keepalive::{KeepAlive, with_schedule};
    use std::sync::atomic::{AtomicBool, Ordering};
    use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};

    let schedule = KeepAlive {
        http2_interval: Duration::from_millis(300),
        http2_timeout: Duration::from_millis(300),
        tcp_idle: Duration::from_secs(1),
    };
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
    // A forwarding proxy that can silently stop delivering in both directions
    // without closing either socket, like a dead Pod IP behind conntrack.
    let blackhole = Arc::new(AtomicBool::new(false));
    let proxy = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = proxy.local_addr().unwrap();
    let proxy_flag = blackhole.clone();
    let accept = tokio::spawn(async move {
        let mut held = Vec::new();
        while let Ok((client, _)) = proxy.accept().await {
            let upstream = tokio::net::TcpStream::connect(upstream_address)
                .await
                .unwrap();
            let (mut client_read, mut client_write) = client.into_split();
            let (mut upstream_read, mut upstream_write) = upstream.into_split();
            let up = proxy_flag.clone();
            let down = proxy_flag.clone();
            held.push(tokio::spawn(async move {
                let mut buffer = [0_u8; 4096];
                while let Ok(read) = client_read.read(&mut buffer).await {
                    if read == 0 {
                        break;
                    }
                    if !up.load(Ordering::SeqCst) {
                        let _ = upstream_write.write_all(&buffer[..read]).await;
                    }
                }
            }));
            held.push(tokio::spawn(async move {
                let mut buffer = [0_u8; 4096];
                while let Ok(read) = upstream_read.read(&mut buffer).await {
                    if read == 0 {
                        break;
                    }
                    if !down.load(Ordering::SeqCst) {
                        let _ = client_write.write_all(&buffer[..read]).await;
                    }
                }
            }));
        }
    });
    let channel = with_schedule(
        tonic::transport::Endpoint::from_shared(format!("http://{address}")).unwrap(),
        schedule,
    )
    .connect()
    .await
    .unwrap();
    // The call deadline is far beyond the bound: only keepalive can end it.
    let client =
        SandboxClient::from_channel(channel, "test".into(), Duration::from_secs(30)).unwrap();
    let job = || {
        PreparedJob::new(
            crate::sandbox::request::Language::Rust,
            "test".into(),
            std::collections::BTreeMap::default(),
            format!("sha256:{}", "a".repeat(64)),
            "test".into(),
            5,
        )
        .unwrap()
    };
    let grant = || SignedSandboxJobGrantV1 {
        key_id: "test".into(),
        claims_bytes: vec![1],
        signature: vec![0; 64],
    };
    let call = {
        let client = client.clone();
        let job = job();
        let grant = grant();
        tokio::spawn(async move { client.submit_granted(grant, &job).await })
    };
    entered.notified().await;
    blackhole.store(true, Ordering::SeqCst);
    let started = tokio::time::Instant::now();
    let result = tokio::time::timeout(Duration::from_secs(10), call)
        .await
        .expect("the black-holed call must end by keepalive, not by its 30 s deadline")
        .unwrap();
    let elapsed = started.elapsed();
    let Err(SandboxCallError::Submission { code, message }) = result else {
        panic!("expected a transport failure");
    };
    assert_eq!(code, Code::Unavailable);
    // A broken connection carries no supervisor instruction.
    assert_eq!(
        message, "",
        "a local transport failure reported a server message"
    );
    assert!(
        elapsed < schedule.detection_bound() * 4,
        "detected after {elapsed:?}"
    );
    // The peer answers again: the same channel reconnects and reaches the server.
    blackhole.store(false, Ordering::SeqCst);
    let again = {
        let client = client.clone();
        let job = job();
        let grant = grant();
        tokio::spawn(async move { client.submit_granted(grant, &job).await })
    };
    tokio::time::timeout(Duration::from_secs(10), entered.notified())
        .await
        .expect("the channel must reconnect and reach the server");
    again.abort();
    accept.abort();
    server.abort();
}
