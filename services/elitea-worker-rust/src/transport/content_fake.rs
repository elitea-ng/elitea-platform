//! An in-process `RuntimeContentService` for the content clients' tests.
//!
//! A fake implements only the RPCs it serves; every other RPC answers
//! `Unimplemented`, exactly as a deployment without it would. The clients
//! reach it through the generated server, so each call is encoded, carries
//! its metadata as headers and comes back with a real gRPC status.

use std::pin::Pin;

use tokio_stream::Stream;
use tonic::body::Body;
use tonic::{Request, Response, Status, Streaming};
use tower::ServiceExt;

use super::{
    CLAIM_METADATA, ContentTransport, ContentTransportError, FENCE_METADATA,
    MAX_CONTENT_MESSAGE_BYTES,
};
use crate::protocol::elitea::runtime::v1 as pb;
use crate::protocol::elitea::runtime::v1::runtime_content_service_server::{
    RuntimeContentService, RuntimeContentServiceServer,
};

/// One server stream a fake answers with.
pub(crate) type Frames<T> = Pin<Box<dyn Stream<Item = Result<T, Status>> + Send>>;

/// A server stream of exactly these items.
pub(crate) fn frames<T: Send + 'static>(items: Vec<Result<T, Status>>) -> Frames<T> {
    Box::pin(tokio_stream::iter(items))
}

/// The claim id and the raw fence a request carried, after asserting that
/// each arrived exactly once.
pub(crate) fn claim_of<T>(request: &Request<T>) -> (String, Vec<u8>) {
    let metadata = request.metadata();
    let claims: Vec<_> = metadata.get_all(CLAIM_METADATA).iter().collect();
    let fences: Vec<_> = metadata.get_all_bin(FENCE_METADATA).iter().collect();
    assert_eq!(claims.len(), 1, "one claim id");
    assert_eq!(fences.len(), 1, "one fence");
    (
        claims[0].to_str().expect("ascii claim id").to_owned(),
        fences[0].to_bytes().expect("binary fence").to_vec(),
    )
}

/// The JSON response every unary content RPC answers with.
#[allow(
    clippy::unnecessary_wraps,
    reason = "a fixture reply is the Result the fake service answers with"
)]
pub(crate) fn reply<T>(message: T) -> Result<Response<T>, Status> {
    Ok(Response::new(message))
}

/// A content service over `fake`, with the production message caps.
pub(crate) fn in_process(fake: impl ContentFake) -> ContentTransport {
    let server = RuntimeContentServiceServer::new(Served(fake))
        .max_decoding_message_size(MAX_CONTENT_MESSAGE_BYTES)
        .max_encoding_message_size(MAX_CONTENT_MESSAGE_BYTES);
    ContentTransport::new(ServiceExt::<http::Request<Body>>::map_err(
        server,
        |never| -> ContentTransportError { match never {} },
    ))
}

/// The generated server's view of a fake.
pub(crate) struct Served<F>(pub(crate) F);

/// One runtime-context call as a route fixture sees it.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct ContextCall {
    /// A stable name for the call, the route under `runtime-context/` it
    /// replaced: `elitea-client-token`, `applications/{a}/versions/{v}`,
    /// `attachments/{bucket}/{name}`, `skills`, `project-context` or
    /// `artifacts/{list,read,write,delete}`.
    pub(crate) route: String,
    pub(crate) execution_id: String,
    pub(crate) generation: u64,
    pub(crate) claim: String,
    pub(crate) fence: Vec<u8>,
    /// The JSON document the call carried; empty for the reads.
    pub(crate) body: Vec<u8>,
    pub(crate) accept_fields: Vec<String>,
}

/// What a route fixture answers one call with.
pub(crate) enum ContextAnswer {
    /// The response document.
    Json(Vec<u8>),
    /// An attachment Main has no text for, and why.
    Unreadable(String),
}

impl From<Vec<u8>> for ContextAnswer {
    fn from(document: Vec<u8>) -> Self {
        Self::Json(document)
    }
}

impl From<&serde_json::Value> for ContextAnswer {
    fn from(document: &serde_json::Value) -> Self {
        Self::Json(document.to_string().into_bytes())
    }
}

/// A runtime-context fixture that answers by route. Wrap it in [`Routes`].
#[tonic::async_trait]
pub(crate) trait ContextRoutes: Send + Sync + 'static {
    async fn answer(&self, call: ContextCall) -> Result<ContextAnswer, Status>;
}

/// The content service of a [`ContextRoutes`] fixture.
pub(crate) struct Routes<R>(pub(crate) R);

impl<R: ContextRoutes> Routes<R> {
    async fn json<T>(
        &self,
        request: Request<T>,
        route: String,
        scope: impl Fn(&T) -> Option<pb::RuntimeContentScopeV1>,
        body: impl Fn(T) -> (Vec<u8>, Vec<String>),
    ) -> Result<Vec<u8>, Status> {
        match self.call(request, route, scope, body).await? {
            ContextAnswer::Json(document) => Ok(document),
            ContextAnswer::Unreadable(_) => {
                Err(Status::internal("fixture: only attachments are unreadable"))
            }
        }
    }

    async fn call<T>(
        &self,
        request: Request<T>,
        route: String,
        scope: impl Fn(&T) -> Option<pb::RuntimeContentScopeV1>,
        body: impl Fn(T) -> (Vec<u8>, Vec<String>),
    ) -> Result<ContextAnswer, Status> {
        let (claim, fence) = claim_of(&request);
        let message = request.into_inner();
        let scope = scope(&message).ok_or_else(|| Status::invalid_argument("scope"))?;
        let (body, accept_fields) = body(message);
        self.0
            .answer(ContextCall {
                route,
                execution_id: scope.execution_id,
                generation: scope.generation,
                claim,
                fence,
                body,
                accept_fields,
            })
            .await
    }
}

#[tonic::async_trait]
impl<R: ContextRoutes> ContentFake for Routes<R> {
    async fn get_elitea_client_token(
        &self,
        request: Request<pb::GetEliteaClientTokenRequestV1>,
    ) -> Result<Response<pb::GetEliteaClientTokenResponseV1>, Status> {
        let context_json = self
            .json(
                request,
                "elitea-client-token".to_owned(),
                |message| message.scope.clone(),
                |message| (Vec::new(), message.accept_fields),
            )
            .await?;
        reply(pb::GetEliteaClientTokenResponseV1 { context_json })
    }
    async fn get_application_version(
        &self,
        request: Request<pb::GetApplicationVersionRequestV1>,
    ) -> Result<Response<pb::GetApplicationVersionResponseV1>, Status> {
        let route = format!(
            "applications/{}/versions/{}",
            request.get_ref().application_id,
            request.get_ref().version_id
        );
        let version_json = self
            .json(
                request,
                route,
                |message| message.scope.clone(),
                |_| (Vec::new(), Vec::new()),
            )
            .await?;
        reply(pb::GetApplicationVersionResponseV1 { version_json })
    }
    async fn get_attachment_object(
        &self,
        request: Request<pb::GetAttachmentObjectRequestV1>,
    ) -> Result<Response<pb::GetAttachmentObjectResponseV1>, Status> {
        let route = format!(
            "attachments/{}/{}",
            request.get_ref().bucket,
            request.get_ref().name
        );
        let answer = self
            .call(
                request,
                route,
                |message| message.scope.clone(),
                |_| (Vec::new(), Vec::new()),
            )
            .await?;
        reply(pb::GetAttachmentObjectResponseV1 {
            outcome: Some(match answer {
                ContextAnswer::Json(document) => {
                    pb::get_attachment_object_response_v1::Outcome::DocumentJson(document)
                }
                ContextAnswer::Unreadable(reason) => {
                    pb::get_attachment_object_response_v1::Outcome::UnreadableReason(reason)
                }
            }),
        })
    }
    async fn write_skill(
        &self,
        request: Request<pb::WriteSkillRequestV1>,
    ) -> Result<Response<pb::WriteSkillResponseV1>, Status> {
        let response_json = self
            .json(
                request,
                "skills".to_owned(),
                |message| message.scope.clone(),
                |message| (message.request_json, Vec::new()),
            )
            .await?;
        reply(pb::WriteSkillResponseV1 { response_json })
    }
    async fn write_project_context(
        &self,
        request: Request<pb::WriteProjectContextRequestV1>,
    ) -> Result<Response<pb::WriteProjectContextResponseV1>, Status> {
        let response_json = self
            .json(
                request,
                "project-context".to_owned(),
                |message| message.scope.clone(),
                |message| (message.request_json, Vec::new()),
            )
            .await?;
        reply(pb::WriteProjectContextResponseV1 { response_json })
    }
    async fn list_artifacts(
        &self,
        request: Request<pb::ListArtifactsRequestV1>,
    ) -> Result<Response<pb::ListArtifactsResponseV1>, Status> {
        let response_json = self
            .json(
                request,
                "artifacts/list".to_owned(),
                |message| message.scope.clone(),
                |message| (message.request_json, Vec::new()),
            )
            .await?;
        reply(pb::ListArtifactsResponseV1 { response_json })
    }
    async fn read_artifact(
        &self,
        request: Request<pb::ReadArtifactRequestV1>,
    ) -> Result<Response<pb::ReadArtifactResponseV1>, Status> {
        let response_json = self
            .json(
                request,
                "artifacts/read".to_owned(),
                |message| message.scope.clone(),
                |message| (message.request_json, Vec::new()),
            )
            .await?;
        reply(pb::ReadArtifactResponseV1 { response_json })
    }
    async fn write_artifact(
        &self,
        request: Request<pb::WriteArtifactRequestV1>,
    ) -> Result<Response<pb::WriteArtifactResponseV1>, Status> {
        let response_json = self
            .json(
                request,
                "artifacts/write".to_owned(),
                |message| message.scope.clone(),
                |message| (message.request_json, Vec::new()),
            )
            .await?;
        reply(pb::WriteArtifactResponseV1 { response_json })
    }
    async fn delete_artifact(
        &self,
        request: Request<pb::DeleteArtifactRequestV1>,
    ) -> Result<Response<pb::DeleteArtifactResponseV1>, Status> {
        let response_json = self
            .json(
                request,
                "artifacts/delete".to_owned(),
                |message| message.scope.clone(),
                |message| (message.request_json, Vec::new()),
            )
            .await?;
        reply(pb::DeleteArtifactResponseV1 { response_json })
    }
}

macro_rules! content_fake {
    ($($method:ident($request:ident) -> $response:ident;)*) => {
        /// The unary and server-streaming content RPCs, each `Unimplemented`
        /// until a fake serves it.
        #[tonic::async_trait]
        pub(crate) trait ContentFake: Send + Sync + 'static {
            async fn get_input(
                &self,
                _request: Request<pb::GetInputRequestV1>,
            ) -> Result<Response<Frames<pb::GetInputResponseV1>>, Status> {
                Err(Status::unimplemented("GetInput"))
            }
            $(
                async fn $method(
                    &self,
                    _request: Request<pb::$request>,
                ) -> Result<Response<pb::$response>, Status> {
                    Err(Status::unimplemented(stringify!($method)))
                }
            )*
        }

        #[tonic::async_trait]
        impl<F: ContentFake> RuntimeContentService for Served<F> {
            type GetInputStream = Frames<pb::GetInputResponseV1>;
            type FetchSandboxBundleFileStream = Frames<pb::FetchSandboxBundleFileResponseV1>;
            type FetchCompiledSnapshotFileStream = Frames<pb::FetchCompiledSnapshotFileResponseV1>;

            async fn get_input(
                &self,
                request: Request<pb::GetInputRequestV1>,
            ) -> Result<Response<Self::GetInputStream>, Status> {
                self.0.get_input(request).await
            }
            $(
                async fn $method(
                    &self,
                    request: Request<pb::$request>,
                ) -> Result<Response<pb::$response>, Status> {
                    self.0.$method(request).await
                }
            )*
            async fn fetch_sandbox_bundle_file(
                &self,
                _request: Request<pb::FetchSandboxBundleFileRequestV1>,
            ) -> Result<Response<Self::FetchSandboxBundleFileStream>, Status> {
                Err(Status::unimplemented("FetchSandboxBundleFile"))
            }
            async fn upload_sandbox_bundle_file(
                &self,
                _request: Request<Streaming<pb::UploadSandboxBundleFileRequestV1>>,
            ) -> Result<Response<pb::UploadSandboxBundleFileResponseV1>, Status> {
                Err(Status::unimplemented("UploadSandboxBundleFile"))
            }
            async fn fetch_compiled_snapshot_file(
                &self,
                _request: Request<pb::FetchCompiledSnapshotFileRequestV1>,
            ) -> Result<Response<Self::FetchCompiledSnapshotFileStream>, Status> {
                Err(Status::unimplemented("FetchCompiledSnapshotFile"))
            }
            async fn upload_compiled_snapshot_file(
                &self,
                _request: Request<Streaming<pb::UploadCompiledSnapshotFileRequestV1>>,
            ) -> Result<Response<pb::UploadCompiledSnapshotFileResponseV1>, Status> {
                Err(Status::unimplemented("UploadCompiledSnapshotFile"))
            }
        }
    };
}

content_fake! {
    get_elitea_client_token(GetEliteaClientTokenRequestV1) -> GetEliteaClientTokenResponseV1;
    get_application_version(GetApplicationVersionRequestV1) -> GetApplicationVersionResponseV1;
    get_attachment_object(GetAttachmentObjectRequestV1) -> GetAttachmentObjectResponseV1;
    execute_http_action(ExecuteHttpActionRequestV1) -> ExecuteHttpActionResponseV1;
    write_skill(WriteSkillRequestV1) -> WriteSkillResponseV1;
    write_project_context(WriteProjectContextRequestV1) -> WriteProjectContextResponseV1;
    list_artifacts(ListArtifactsRequestV1) -> ListArtifactsResponseV1;
    read_artifact(ReadArtifactRequestV1) -> ReadArtifactResponseV1;
    write_artifact(WriteArtifactRequestV1) -> WriteArtifactResponseV1;
    delete_artifact(DeleteArtifactRequestV1) -> DeleteArtifactResponseV1;
    resolve_code_workspace(ResolveCodeWorkspaceRequestV1) -> ResolveCodeWorkspaceResponseV1;
    admit_code_debug(AdmitCodeDebugRequestV1) -> AdmitCodeDebugResponseV1;
    commit_code_debug(CommitCodeDebugRequestV1) -> CommitCodeDebugResponseV1;
    step_code_platform(StepCodePlatformRequestV1) -> StepCodePlatformResponseV1;
    read_code_platform_effects(ReadCodePlatformEffectsRequestV1) -> ReadCodePlatformEffectsResponseV1;
    poll_node_recovery(PollNodeRecoveryRequestV1) -> PollNodeRecoveryResponseV1;
    acknowledge_node_recovery(AcknowledgeNodeRecoveryRequestV1) -> AcknowledgeNodeRecoveryResponseV1;
    read_node_recovery_result(ReadNodeRecoveryResultRequestV1) -> ReadNodeRecoveryResultResponseV1;
    fetch_execution_interrupts(FetchExecutionInterruptsRequestV1) -> FetchExecutionInterruptsResponseV1;
    acknowledge_execution_interrupt(AcknowledgeExecutionInterruptRequestV1) -> AcknowledgeExecutionInterruptResponseV1;
    register_code_visit(RegisterCodeVisitRequestV1) -> RegisterCodeVisitResponseV1;
    register_code_intent(RegisterCodeIntentRequestV1) -> RegisterCodeIntentResponseV1;
    put_toolkit_discovery_result(PutToolkitDiscoveryResultRequestV1) -> PutToolkitDiscoveryResultResponseV1;
    get_toolkit_discovery_result(GetToolkitDiscoveryResultRequestV1) -> GetToolkitDiscoveryResultResponseV1;
    read_sandbox_workspace_manifest(ReadSandboxWorkspaceManifestRequestV1) -> ReadSandboxWorkspaceManifestResponseV1;
    read_sandbox_workspace_file(ReadSandboxWorkspaceFileRequestV1) -> ReadSandboxWorkspaceFileResponseV1;
    get_sandbox_bundle(GetSandboxBundleRequestV1) -> GetSandboxBundleResponseV1;
    publish_sandbox_bundle(PublishSandboxBundleRequestV1) -> PublishSandboxBundleResponseV1;
    get_compiled_snapshot(GetCompiledSnapshotRequestV1) -> GetCompiledSnapshotResponseV1;
    publish_compiled_snapshot(PublishCompiledSnapshotRequestV1) -> PublishCompiledSnapshotResponseV1;
}
