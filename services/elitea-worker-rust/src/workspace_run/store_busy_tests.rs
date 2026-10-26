//! `thread_busy` over a real gRPC channel (kept out of `store.rs`: the
//! keepalive gate scans production files for endpoint builders).

use std::time::Duration;

use super::*;
use crate::protocol::elitea::runtime::v1::{
    WorkspaceEnvSecretFrameV1, WorkspaceEnvSecretRequestV1, WorkspaceFetchFrameV1,
    WorkspaceFetchPlanRequestV1, WorkspacePublishRequestV1, WorkspacePublishResponseV1,
    WorkspaceRefV1, WorkspaceUploadFrameV1, WorkspaceUploadResponseV1,
};

const CLOUD: &str = "1111111111111111111111111111111111111111";
const QUICK: BusyRetry = BusyRetry {
    first: Duration::from_millis(1),
    cap: Duration::from_millis(4),
    attempts: 5,
};

/// Over a real gRPC channel: Main answers ABORTED `thread_busy`, then the
/// same publish applies. The store sees two identical requests and the
/// ref moves once.
#[cfg(feature = "sandbox-supervisor")]
#[tokio::test]
#[allow(clippy::too_many_lines, reason = "a whole in-process gRPC service")]
async fn the_grpc_store_resends_a_busy_publish_and_it_applies_once() {
    use crate::protocol::elitea::runtime::v1::workspace_store_service_server::{
        WorkspaceStoreService, WorkspaceStoreServiceServer,
    };
    use std::pin::Pin;
    use std::sync::{Arc, Mutex};
    use tokio_stream::Stream;

    #[derive(Default)]
    struct BusyOnce {
        requests: Mutex<Vec<WorkspacePublishRequestV1>>,
        applied: Mutex<u32>,
    }
    type Frames<T> = Pin<Box<dyn Stream<Item = Result<T, tonic::Status>> + Send>>;
    #[tonic::async_trait]
    impl WorkspaceStoreService for Arc<BusyOnce> {
        type WorkspaceFetchPlanStream = Frames<WorkspaceFetchFrameV1>;
        type WorkspaceEnvSecretStream = Frames<WorkspaceEnvSecretFrameV1>;
        async fn workspace_fetch_plan(
            &self,
            _: tonic::Request<WorkspaceFetchPlanRequestV1>,
        ) -> Result<tonic::Response<Self::WorkspaceFetchPlanStream>, tonic::Status> {
            Err(tonic::Status::unimplemented("fetch"))
        }
        async fn workspace_upload(
            &self,
            _: tonic::Request<tonic::Streaming<WorkspaceUploadFrameV1>>,
        ) -> Result<tonic::Response<WorkspaceUploadResponseV1>, tonic::Status> {
            Err(tonic::Status::unimplemented("upload"))
        }
        async fn workspace_publish(
            &self,
            request: tonic::Request<WorkspacePublishRequestV1>,
        ) -> Result<tonic::Response<WorkspacePublishResponseV1>, tonic::Status> {
            let request = request.into_inner();
            let first = {
                let mut requests = self.requests.lock().expect("lock");
                requests.push(request.clone());
                requests.len() == 1
            };
            if first {
                return Err(tonic::Status::aborted("thread_busy"));
            }
            *self.applied.lock().expect("lock") += 1;
            Ok(tonic::Response::new(WorkspacePublishResponseV1 {
                seq: 4,
                refs: vec![WorkspaceRefV1 {
                    name: REF_CLOUD.to_owned(),
                    sha: request.refs[0].new.clone(),
                }],
                ..Default::default()
            }))
        }
        async fn workspace_record_event(
            &self,
            _: tonic::Request<crate::protocol::elitea::runtime::v1::WorkspaceRecordEventRequestV1>,
        ) -> Result<
            tonic::Response<crate::protocol::elitea::runtime::v1::WorkspaceRecordEventResponseV1>,
            tonic::Status,
        > {
            Err(tonic::Status::unimplemented("record event"))
        }
        async fn workspace_record_changes(
            &self,
            _: tonic::Request<
                tonic::Streaming<crate::protocol::elitea::runtime::v1::WorkspaceChangesFrameV1>,
            >,
        ) -> Result<
            tonic::Response<crate::protocol::elitea::runtime::v1::WorkspaceRecordChangesResponseV1>,
            tonic::Status,
        > {
            Err(tonic::Status::unimplemented("record changes"))
        }
        async fn workspace_env_secret(
            &self,
            _: tonic::Request<WorkspaceEnvSecretRequestV1>,
        ) -> Result<tonic::Response<Self::WorkspaceEnvSecretStream>, tonic::Status> {
            Err(tonic::Status::unimplemented("env"))
        }
    }

    let service = Arc::new(BusyOnce::default());
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    let address = listener.local_addr().expect("address");
    let server = tokio::spawn({
        let service = Arc::clone(&service);
        async move {
            let _ = tonic::transport::Server::builder()
                .add_service(WorkspaceStoreServiceServer::new(service))
                .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener))
                .await;
        }
    });
    let channel = tonic::transport::Endpoint::from_shared(format!("http://{address}"))
        .expect("endpoint")
        .connect()
        .await
        .expect("connect");
    let store = GrpcWorkspaceStore::new(channel, Duration::from_secs(5)).with_busy_retry(QUICK);
    let next = "2222222222222222222222222222222222222222".to_owned();
    let published = store
        .publish(
            &WorkspaceClaimV1::default(),
            &PublishSegment {
                kind: super::KIND_COMPACT,
                segment_sha256: "a".repeat(64),
                bytes: 3,
                prerequisites: Vec::new(),
                old_cloud: Some(CLOUD.to_owned()),
                new_cloud: next.clone(),
            },
        )
        .await
        .expect("published after the busy answer");
    assert_eq!(published.seq, 4);
    assert_eq!(published.refs.get(REF_CLOUD), Some(&next));
    let requests = service.requests.lock().expect("lock").clone();
    assert_eq!(requests.len(), 2);
    assert_eq!(
        requests[0], requests[1],
        "the busy publish is resent unchanged"
    );
    assert_eq!(*service.applied.lock().expect("lock"), 1);
    server.abort();
}
