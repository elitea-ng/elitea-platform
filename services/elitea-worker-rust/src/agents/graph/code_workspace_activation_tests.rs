//! Production workspace acquisition with trusted Main transport fixtures.
//! These tests use no database, control, sandbox, repository, or network service.
use super::*;
use crate::{
    agents::graph::{
        code::{CodeLanguage, CodeProvenance},
        code_workspace::{CodeOriginalDeclaration, CodeWorkspaceInvocation},
        node_recovery::NodeFailureClass,
    },
    protocol::control::{AgentControlClient, ClaimBoundSandboxAuthority},
    sandbox::{request::Language, workspace::WorkspacePolicy},
    transport::{
        InputContentClient,
        input_content::{InputContentRpc, InputContentTransportError},
    },
};
use async_trait::async_trait;
use base64::{
    Engine as _,
    engine::general_purpose::{STANDARD, URL_SAFE_NO_PAD},
};
use http_body_util::BodyExt as _;
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
    time::Duration,
};

const MANIFEST: &[u8] =
    include_bytes!("../../../../../libs/proto/elitea/runtime/v1/code_workspace_manifest_v1.json");
const ROOT: &str = "ccbf5e3ba22d5a09f07b063e4b8e65b7a7b1c6e6b4d0c6c4a5bb285e519c21c1";

#[derive(Clone, Copy)]
enum MainReply {
    Manifest,
    Status(http::StatusCode),
    ChangedDigest,
    ChangedRoot,
}
struct MainRpc {
    reply: MainReply,
    requests: Arc<Mutex<Vec<serde_json::Value>>>,
}
#[async_trait]
impl InputContentRpc for MainRpc {
    async fn get(
        &self,
        request: http::Request<tonic::body::Body>,
    ) -> Result<http::Response<tonic::body::Body>, InputContentTransportError> {
        assert_eq!(request.method(), http::Method::POST);
        assert!(
            request
                .uri()
                .path()
                .ends_with("/runtime-context/code-workspace")
        );
        assert!(request.uri().query().is_none());
        assert!(request.headers().contains_key("x-elitea-claim-id"));
        assert!(request.headers().contains_key("x-elitea-fence"));
        let body = request.into_body().collect().await.unwrap().to_bytes();
        let value: serde_json::Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(value.as_object().unwrap().len(), 2);
        assert_eq!(value["original_visit"]["visit_id"], "1".repeat(64));
        let base = URL_SAFE_NO_PAD
            .decode(
                value["selected_pre_workspace_prepared_job_json_base64url"]
                    .as_str()
                    .unwrap(),
            )
            .unwrap();
        assert_eq!(base, job().to_transport().unwrap());
        self.requests.lock().unwrap().push(value);
        let status = match self.reply {
            MainReply::Status(status) => status,
            _ => http::StatusCode::OK,
        };
        let digest = match self.reply {
            MainReply::ChangedDigest => [9; 32],
            _ => ring::digest::digest(&ring::digest::SHA256, MANIFEST)
                .as_ref()
                .try_into()
                .unwrap(),
        };
        let root = match self.reply {
            MainReply::ChangedRoot => "f".repeat(64),
            _ => ROOT.to_owned(),
        };
        Ok(http::Response::builder()
            .status(status)
            .version(http::Version::HTTP_2)
            .header(http::header::CONTENT_TYPE, "application/json")
            .header(http::header::CONTENT_LENGTH, MANIFEST.len())
            .header(
                "content-digest",
                format!("sha-256=:{}:", STANDARD.encode(digest)),
            )
            .header("x-elitea-workspace-root", root)
            .body(tonic::body::Body::new(http_body_util::Full::new(
                bytes::Bytes::from_static(MANIFEST),
            )))
            .unwrap())
    }
}
fn job() -> PreparedJob {
    PreparedJob::new(
        Language::Python,
        "7".into(),
        BTreeMap::new(),
        format!("sha256:{}", "a".repeat(64)),
        "isolated-v1".into(),
        10,
    )
    .unwrap()
}
fn visit() -> OriginalCodeVisitRef {
    OriginalCodeVisitRef {
        visit_id: "1".repeat(64),
        revision: 1,
        digest_sha256: "2".repeat(64),
    }
}
fn invocation() -> CodeInvocation<'static> {
    let manifest =
        WorkspaceManifest::from_transport(MANIFEST, ROOT, &WorkspacePolicy::default()).unwrap();
    CodeInvocation {
        activation: [7; 32],
        language: CodeLanguage::Python,
        platform_client: false,
        source: "7",
        dependencies_toml: None,
        provenance: CodeProvenance::SavedLiteral,
        input_json: b"{}".to_vec(),
        trace: None,
        original: None,
        debug: None,
        workspace: Some(CodeWorkspaceInvocation {
            selection: manifest.selection().clone(),
            declaration: CodeOriginalDeclaration {
                node_id: "run".into(),
                graph_thread_id: "original-thread".into(),
                graph_step: 3,
                configuration_json: "{}".into(),
                definition: [3; 32],
                yaml: [4; 32],
            },
        }),
    }
}
fn remote(content: Option<Arc<InputContentClient>>) -> RemoteCodeRuntime {
    let channel = tonic::transport::Endpoint::from_static("https://127.0.0.1:1").connect_lazy();
    let control = Arc::new(
        AgentControlClient::from_channel(
            channel,
            crate::transport::control_grpc::ControlGrpcConfig {
                deadline: Duration::from_secs(1),
                workload_session_id: "workload-1".into(),
                producer_id: "worker-1".into(),
            },
        )
        .unwrap(),
    );
    let pool = sqlx::postgres::PgPoolOptions::new()
        .max_connections(1)
        .connect_lazy("postgres://fixture:fixture@127.0.0.1:1/never_connect")
        .unwrap();
    RemoteCodeRuntime {
        saved_child_scope: None,
        intent_content: content,
        control,
        authority: Arc::new(ClaimBoundSandboxAuthority::original_code_visit_conformance_fixture()),
        profiles: Vec::new().into(),
        journal: Arc::new(crate::sandbox::dispatch::DispatchJournal::new(pool)),
        compiled_profile: None,
        debug_sink: None,
    }
}
fn fixture(reply: MainReply) -> (RemoteCodeRuntime, Arc<Mutex<Vec<serde_json::Value>>>) {
    let requests = Arc::new(Mutex::new(Vec::new()));
    let content = Arc::new(InputContentClient::with_original_code_visit_fixture_rpc(
        MainRpc {
            reply,
            requests: requests.clone(),
        },
    ));
    (remote(Some(content)), requests)
}

#[tokio::test]
async fn workspace_activation_uses_the_exact_main_manifest_and_original_visit() {
    let (runtime, requests) = fixture(MainReply::Manifest);
    let original = job();
    let before = original.to_transport().unwrap();
    let (selected, manifest) = runtime
        .acquire_execution_workspace(&invocation(), &visit(), original)
        .await
        .unwrap();
    let manifest = manifest.unwrap();
    assert_eq!(manifest.root(), ROOT);
    assert!(selected.workspace().unwrap().matches(&manifest).unwrap());
    assert_eq!(
        selected.pre_workspace().unwrap().to_transport().unwrap(),
        before
    );
    assert_eq!(requests.lock().unwrap().len(), 1);
}
#[tokio::test]
async fn workspace_activation_denied_or_absent_main_owner_never_returns_a_job() {
    for status in [http::StatusCode::FORBIDDEN, http::StatusCode::NOT_FOUND] {
        let (runtime, requests) = fixture(MainReply::Status(status));
        let failure = runtime
            .acquire_execution_workspace(&invocation(), &visit(), job())
            .await
            .err()
            .unwrap();
        assert_eq!(
            failure.class(),
            if status == http::StatusCode::FORBIDDEN {
                NodeFailureClass::AuthorizationDenied
            } else {
                NodeFailureClass::DependencyUnavailable
            }
        );
        assert_eq!(requests.lock().unwrap().len(), 1);
    }
    let failure = remote(None)
        .acquire_execution_workspace(&invocation(), &visit(), job())
        .await
        .err()
        .unwrap();
    assert_eq!(failure.class(), NodeFailureClass::InvalidConfiguration);
}
#[tokio::test]
async fn workspace_activation_omission_preserves_bytes_without_an_owner() {
    let mut call = invocation();
    call.workspace = None;
    let original = job();
    let bytes = original.to_transport().unwrap();
    let fingerprint = original.fingerprint().unwrap();
    let (selected, manifest) = remote(None)
        .acquire_execution_workspace(&call, &visit(), original)
        .await
        .unwrap();
    assert!(manifest.is_none());
    assert_eq!(selected.to_transport().unwrap(), bytes);
    assert_eq!(selected.fingerprint().unwrap(), fingerprint);
}
#[tokio::test]
async fn workspace_activation_readwrite_and_invalid_visit_refuse_before_owner_contact() {
    let (runtime, requests) = fixture(MainReply::Manifest);
    let mut call = invocation();
    call.workspace.as_mut().unwrap().selection.mode = WorkspaceMode::Readwrite;
    let failure = runtime
        .acquire_execution_workspace(&call, &visit(), job())
        .await
        .err()
        .unwrap();
    assert_eq!(failure.class(), NodeFailureClass::InvalidConfiguration);
    let mut invalid = visit();
    invalid.revision = 0;
    let failure = runtime
        .acquire_execution_workspace(&invocation(), &invalid, job())
        .await
        .err()
        .unwrap();
    assert_eq!(failure.class(), NodeFailureClass::AuthorizationDenied);
    assert!(requests.lock().unwrap().is_empty());
}
#[tokio::test]
async fn workspace_activation_changed_digest_root_or_selection_cannot_bind_execution() {
    for reply in [MainReply::ChangedDigest, MainReply::ChangedRoot] {
        let (runtime, requests) = fixture(reply);
        let failure = runtime
            .acquire_execution_workspace(&invocation(), &visit(), job())
            .await
            .err()
            .unwrap();
        assert_eq!(failure.class(), NodeFailureClass::AuthorizationDenied);
        assert_eq!(requests.lock().unwrap().len(), 1);
    }
    let (runtime, requests) = fixture(MainReply::Manifest);
    let mut call = invocation();
    call.workspace.as_mut().unwrap().selection.commit = "9".repeat(40);
    let failure = runtime
        .acquire_execution_workspace(&call, &visit(), job())
        .await
        .err()
        .unwrap();
    assert_eq!(failure.class(), NodeFailureClass::AuthorizationDenied);
    assert_eq!(requests.lock().unwrap().len(), 1);
}

#[tokio::test]
async fn workspace_activation_raw_invocation_cannot_replace_original_attempt_authority() {
    use crate::agents::graph::code_runtime::CodeSandboxRuntime;
    let (runtime, requests) = fixture(MainReply::Manifest);
    let error = runtime.execute(invocation()).await.err().unwrap();
    assert!(matches!(error, adk_rust::graph::GraphError::Other(message)
        if message == "graph.code.execution_failed: Code original-visit authority is unavailable on the raw invocation path."));
    assert!(requests.lock().unwrap().is_empty());
}
