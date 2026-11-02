//! Governed redemption on the direct toolkit lane (#1474).
//!
//! A governed direct call runs its one toolkit through the command's
//! governed seam: `RedeemCredential` over the content service (a fake Main
//! broker here) and a mock MCP server. No claim-time credential reaches the
//! toolkit, and a toolkit the run cannot govern is unsupported.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::tool::BasicToolset;
use adk_rust::{Tool, ToolContext, Toolset};
use async_trait::async_trait;
use elitea_agent_runtime::toolkits::governed::canonical_resource_uri;
use serde_json::{Value, json};
use tonic::{Request, Response, Status};

use super::{
    DirectToolkitRequest, DirectToolkitRuntime, DirectToolkitRuntimeErrorCode, IndexToolSupport,
    IndexToolsWithheld, McpConnector, McpMaterializationError, RemoteMcpConfig,
};
use crate::protocol::control::test_runtime_context_authority;
use crate::protocol::elitea::runtime::v1 as pb;
use crate::transport::credential_redeemer::governed_client;
use crate::transport::input_content::content_fake::ContentFake;
use crate::transport::platform_client::PlatformClient;
use crate::transport::runtime_context::{RuntimeContextClient, RuntimeContextConfig};

const SERVER: &str = "https://records.mcp.example.test/mcp";
const DIGEST: [u8; 32] = [5; 32];
const CONTEXT: &[u8] = br#"{"toolkit_security":{}}"#;

/// A fake Main broker: toolkit 41 gets the server's token; it records every
/// redemption.
#[derive(Default)]
struct Broker {
    redeemed: Mutex<Vec<pb::BoundToolInvocationV1>>,
}

#[tonic::async_trait]
impl ContentFake for Arc<Broker> {
    async fn redeem_credential(
        &self,
        request: Request<pb::RedeemCredentialRequestV1>,
    ) -> Result<Response<pb::RedeemCredentialResponseV1>, Status> {
        let invocation = request
            .into_inner()
            .invocation
            .ok_or_else(|| Status::invalid_argument("no invocation"))?;
        self.redeemed
            .lock()
            .expect("redeemed")
            .push(invocation.clone());
        if invocation.toolkit_id != 41 {
            return Err(Status::permission_denied("not frozen"));
        }
        Ok(Response::new(pb::RedeemCredentialResponseV1 {
            result: Some(pb::redeem_credential_response_v1::Result::Envelope(
                pb::CredentialEnvelopeV1 {
                    redemption_id: "redemption-1".to_owned(),
                    kind: pb::CredentialEnvelopeKindV1::OauthAccess as i32,
                    material: [("access_token".to_owned(), "redeemed-token".to_owned())].into(),
                    not_after_unix_millis: 4_102_444_800_000,
                    target_identity: canonical_resource_uri(SERVER).expect("canonical"),
                    obligations: Vec::new(),
                    decision_id: "decision-1".to_owned(),
                },
            )),
        }))
    }
}

impl Broker {
    fn operations(&self) -> Vec<(String, String)> {
        self.redeemed
            .lock()
            .expect("redeemed")
            .iter()
            .map(|invocation| {
                (
                    invocation.operation_id.clone(),
                    invocation.resource_ref.clone(),
                )
            })
            .collect()
    }
}

/// One mock MCP server: it records the bearer of every session and serves
/// one read-only `lookup` tool.
#[derive(Default)]
struct Server {
    bearers: Mutex<Vec<Option<String>>>,
}

struct Connector(Arc<Server>);

#[async_trait]
impl McpConnector for Connector {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        self.0
            .bearers
            .lock()
            .expect("bearers")
            .push(config.access_token_for_test().map(str::to_owned));
        Ok(Arc::new(BasicToolset::new(
            "mock_mcp",
            vec![Arc::new(Lookup) as Arc<dyn Tool>],
        )))
    }
}

struct Lookup;

#[async_trait]
impl Tool for Lookup {
    fn name(&self) -> &'static str {
        "lookup"
    }

    fn description(&self) -> &'static str {
        "Look up a record."
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({"type": "object", "properties": {"query": {"type": "string"}}}))
    }

    fn is_read_only(&self) -> bool {
        true
    }

    async fn execute(&self, _ctx: Arc<dyn ToolContext>, _args: Value) -> adk_rust::Result<Value> {
        Ok(json!({"found": true}))
    }
}

fn platform(broker: &Arc<Broker>) -> Arc<PlatformClient> {
    let config = RuntimeContextConfig {
        origin: "https://CONTENT.internal:443".to_owned(),
        deadline: Duration::from_secs(5),
        max_response_bytes: 32 * 1024,
        max_application_response_bytes: 1024 * 1024,
        max_attachment_response_bytes: 1024 * 1024,
        max_artifact_response_bytes: 2 * 1024 * 1024,
    };
    Arc::new(PlatformClient::new(Arc::new(
        RuntimeContextClient::with_rpc(Arc::clone(broker), config).expect("client"),
    )))
}

fn mcp_call(settings: &Value) -> DirectToolkitRequest {
    let config = serde_json::to_vec(
        &json!({"id": 41, "type": "mcp", "toolkit_name": "records", "settings": settings}),
    )
    .expect("config");
    DirectToolkitRequest::parse_call("mcp", "41", "lookup", &config, b"{}", CONTEXT)
        .expect("request")
}

fn mcp_settings() -> Value {
    json!({"url": SERVER, "headers": null, "client_id": null, "client_secret": null,
        "timeout": 12, "selected_tools": [], "enable_caching": true, "cache_ttl": 300,
        "ssl_verify": true})
}

/// A governed runtime over `server`, and the fake broker behind it.
fn governed_runtime(server: &Arc<Server>) -> (DirectToolkitRuntime, Arc<Broker>) {
    let broker = Arc::new(Broker::default());
    let authority = Arc::new(test_runtime_context_authority().governed_for_test(DIGEST));
    let client = governed_client(&platform(&broker), &authority).expect("a governed command");
    let base = DirectToolkitRuntime::new(Arc::new(Connector(Arc::clone(server))));
    (base.for_command(Some(client)), broker)
}

async fn call(
    runtime: &DirectToolkitRuntime,
    request: &DirectToolkitRequest,
) -> Result<Value, super::DirectToolkitRuntimeError> {
    runtime
        .execute_test(
            request,
            "execution-one",
            "call-1",
            IndexToolSupport::Withheld(IndexToolsWithheld::NotServedOnThisPath),
            None,
        )
        .await
}

/// Acceptance (#1474): a governed direct call of a saved MCP toolkit
/// redeems `mcp.tools.list` and `mcp.tool.call` through the broker, and
/// every session carries only the redeemed token.
#[tokio::test]
async fn a_governed_direct_mcp_call_redeems_through_the_broker() {
    let server = Arc::new(Server::default());
    let (runtime, broker) = governed_runtime(&server);
    let result = call(&runtime, &mcp_call(&mcp_settings()))
        .await
        .expect("governed call");
    assert!(result.to_string().contains("found"), "{result}");
    assert!(!result.to_string().contains("redeemed-token"));
    assert_eq!(
        broker.operations(),
        [
            ("mcp.tools.list".to_owned(), String::new()),
            ("mcp.tool.call".to_owned(), "mcp:tool/lookup".to_owned()),
        ]
    );
    let bearers = server.bearers.lock().expect("bearers").clone();
    assert_ne!(bearers.len(), 0);
    assert!(
        bearers
            .iter()
            .all(|bearer| bearer.as_deref() == Some("redeemed-token")),
        "{bearers:?}"
    );
}

/// A governed direct call of an MCP toolkit whose own headers carry a
/// credential cannot be governed: it is unsupported, and nothing is
/// redeemed or dialled.
#[tokio::test]
async fn a_self_credentialed_direct_mcp_call_is_unsupported_when_governed() {
    let server = Arc::new(Server::default());
    let (runtime, broker) = governed_runtime(&server);
    let mut settings = mcp_settings();
    settings["headers"] = json!({"Authorization": "Bearer claim-time-pat"});
    let error = call(&runtime, &mcp_call(&settings))
        .await
        .expect_err("not governable");
    assert_eq!(
        error.code(),
        DirectToolkitRuntimeErrorCode::UnsupportedToolkit
    );
    assert_eq!(broker.operations().len(), 0);
    assert_eq!(server.bearers.lock().expect("bearers").len(), 0);
}

/// A governed direct call of a credentialed configured toolkit no adapter
/// governs is unsupported; its claim-time credential is never used.
#[tokio::test]
async fn a_credentialed_toolkit_no_adapter_governs_is_unsupported_when_governed() {
    let server = Arc::new(Server::default());
    let (runtime, broker) = governed_runtime(&server);
    let config = serde_json::to_vec(&json!({"id": 7, "type": "openapi", "toolkit_name": "saved",
        "settings": {
            "openapi_configuration": {"api_key": "claim-time-secret"},
            "spec": {"openapi": "3.0.3", "servers": [{"url": "https://example.invalid"}],
                "paths": {"/read": {"get": {"operationId": "read",
                    "responses": {"200": {"description": "ok"}}}}}},
            "selected_tools": ["read"]}}))
    .expect("config");
    let request = DirectToolkitRequest::parse_call("openapi", "7", "read", &config, b"{}", CONTEXT)
        .expect("request");
    let error = call(&runtime, &request).await.expect_err("not governable");
    assert_eq!(
        error.code(),
        DirectToolkitRuntimeErrorCode::UnsupportedToolkit
    );
    assert!(!format!("{error:?} {error}").contains("claim-time-secret"));
    assert_eq!(broker.operations().len(), 0);
}

/// A legacy command is unchanged: no redemption, and the MCP session
/// carries no redeemed token.
#[tokio::test]
async fn a_legacy_direct_mcp_call_never_redeems() {
    let server = Arc::new(Server::default());
    let broker = Arc::new(Broker::default());
    let legacy = Arc::new(test_runtime_context_authority());
    let runtime = DirectToolkitRuntime::new(Arc::new(Connector(Arc::clone(&server))))
        .for_command(governed_client(&platform(&broker), &legacy));
    call(&runtime, &mcp_call(&mcp_settings()))
        .await
        .expect("legacy call");
    assert_eq!(broker.operations().len(), 0);
    assert!(
        server
            .bearers
            .lock()
            .expect("bearers")
            .iter()
            .all(Option::is_none)
    );
}
