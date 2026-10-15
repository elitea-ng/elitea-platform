//! The runtime-context `workspace` field (ADR-0034): asked for, parsed
//! strictly, and absent for an ordinary turn.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use bytes::Bytes;
use http::{Request, Response, StatusCode, Version};
use http_body_util::Full;
use tonic::body::Body;

use super::runtime_context::{
    RuntimeContextClient, RuntimeContextConfig, RuntimeContextError, RuntimeContextRpc,
    RuntimeContextTransportError,
};
use crate::protocol::control::test_runtime_context_authority;

struct AcceptCapture {
    accept: Arc<Mutex<Vec<String>>>,
    body: String,
}

#[async_trait]
impl RuntimeContextRpc for AcceptCapture {
    async fn post(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, RuntimeContextTransportError> {
        self.accept.lock().expect("accept").extend(
            request
                .headers()
                .get_all("x-elitea-runtime-context-accept")
                .iter()
                .map(|value| value.to_str().expect("ascii").to_owned()),
        );
        Ok(Response::builder()
            .status(StatusCode::OK)
            .version(Version::HTTP_2)
            .header("content-type", "application/json")
            .header("cache-control", "private, no-cache, no-store")
            .header("pragma", "no-cache")
            .header("content-length", self.body.len())
            .body(Body::new(Full::new(Bytes::from(self.body.clone()))))
            .expect("response"))
    }
}

fn fake(body: serde_json::Value) -> (RuntimeContextClient, Arc<Mutex<Vec<String>>>) {
    let accept = Arc::new(Mutex::new(Vec::new()));
    let client = RuntimeContextClient::with_rpc(
        AcceptCapture {
            accept: Arc::clone(&accept),
            body: body.to_string(),
        },
        RuntimeContextConfig {
            origin: "https://content.internal:443".to_owned(),
            deadline: Duration::from_secs(1),
            max_response_bytes: 32 * 1_024,
            max_application_response_bytes: 1024 * 1024,
            max_attachment_response_bytes: 1024 * 1024,
            max_artifact_response_bytes: 2 * 1024 * 1024,
        },
    )
    .expect("client");
    (client, accept)
}

fn token(workspace: Option<serde_json::Value>) -> serde_json::Value {
    let mut body = serde_json::json!({
        "schema_version": "elitea.runtime.elitea-client-token.v1",
        "project_id": 17,
        "token": "fixture-token",
    });
    if let Some(workspace) = workspace {
        body["workspace"] = workspace;
    }
    body
}

fn workspace() -> serde_json::Value {
    serde_json::json!({
        "thread": "0b9c6f4e-9a7f-4d4e-8f0e-1d2c3b4a5f60",
        "epoch": 2,
        "expected_old_cloud": "1111111111111111111111111111111111111111",
        "limits": {
            "max_segment_bytes": 1000, "max_file_bytes": 100, "max_files": 10,
            "max_unpacked_bytes": 5000, "unpacked_bytes": 4000, "turn_wall_seconds": 0,
            "cpu_millis": 0, "memory_bytes": 0, "disk_bytes": 0
        },
        "network": false, "shell": true, "network_allowlist": [], "has_upstream": false,
        "env_secret": true
    })
}

#[tokio::test(flavor = "current_thread")]
async fn redemption_asks_for_the_workspace_field_and_an_ordinary_turn_has_none() {
    let (client, accept) = fake(token(None));
    let context = client
        .redeem(&test_runtime_context_authority())
        .await
        .expect("an ordinary turn redeems as before");
    assert!(context.workspace().is_none());
    assert_eq!(*accept.lock().expect("accept"), ["workspace"]);
}

#[tokio::test(flavor = "current_thread")]
async fn a_cloud_turn_carries_the_parsed_field() {
    let (client, _) = fake(token(Some(workspace())));
    let context = client
        .redeem(&test_runtime_context_authority())
        .await
        .expect("a cloud turn redeems");
    let field = context.workspace().expect("the workspace field");
    assert_eq!(field.epoch, 2);
    assert!(!field.network);
    assert!(field.env_secret);
    assert_eq!(
        field.expected_old_cloud.as_deref(),
        Some("1111111111111111111111111111111111111111")
    );
}

#[tokio::test(flavor = "current_thread")]
async fn strict_parsing_is_unaffected_and_extends_into_the_field() {
    // An unknown top-level key is still refused.
    let mut unknown = token(None);
    unknown["future"] = serde_json::json!(true);
    let (client, _) = fake(unknown);
    assert!(matches!(
        client.redeem(&test_runtime_context_authority()).await,
        Err(RuntimeContextError::InvalidResponse(_))
    ));
    // An unknown key inside the field is refused too.
    let mut inner = workspace();
    inner["future"] = serde_json::json!(true);
    let (client, _) = fake(token(Some(inner)));
    assert!(matches!(
        client.redeem(&test_runtime_context_authority()).await,
        Err(RuntimeContextError::InvalidResponse(_))
    ));
    // A field that parses but is malformed (stale-looking epoch 0) is refused.
    let mut zero = workspace();
    zero["epoch"] = serde_json::json!(0);
    let (client, _) = fake(token(Some(zero)));
    assert!(matches!(
        client.redeem(&test_runtime_context_authority()).await,
        Err(RuntimeContextError::InvalidResponse(_))
    ));
}
