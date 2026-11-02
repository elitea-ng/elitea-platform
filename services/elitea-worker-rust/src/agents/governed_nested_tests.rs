//! Governed MCP for nested agents and saved pipelines (#1474).
//!
//! Both paths build their toolsets through `materialize_nested_toolsets`
//! from the child version's own frozen tool list. These tests run it with
//! the worker's real governed seam: `RedeemCredential` over the content
//! service (a fake Main broker) and two mock MCP servers.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::tool::{BasicToolset, SimpleToolContext};
use adk_rust::{ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use elitea_agent_runtime::toolkits::governed::canonical_resource_uri;
use serde_json::{Map, Value, json};
use tonic::{Request, Response, Status};

use super::application_tools::{ApplicationContinuation, ApplicationMaterializationPath};
use super::governed_nested::{
    NestedErrorMapping, NestedToolsetInputs, NestedToolsets, materialize_nested_toolsets,
};
use super::runtime::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};
use crate::protocol::control::{ClaimBoundRuntimeContextAuthority, test_runtime_context_authority};
use crate::protocol::elitea::runtime::v1 as pb;
use crate::toolkits::{
    FrozenToolSnapshot, McpConnector, McpMaterializationError, RemoteMcpConfig,
    ToolAdmissionPolicy, ToolsetMaterializationError,
};
use crate::transport::credential_redeemer::governed_client;
use crate::transport::input_content::content_fake::ContentFake;
use crate::transport::platform_client::PlatformClient;
use crate::transport::runtime_context::{RuntimeContextClient, RuntimeContextConfig};

const DIGEST: [u8; 32] = [9; 32];
const SERVER_A: &str = "https://a.mcp.example.test/mcp";
const SERVER_B: &str = "https://b.mcp.example.test/v1/mcp";

/// A fake Main broker: it answers each toolkit with ITS server's token and
/// records every redemption.
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
        let (server, token) = match invocation.toolkit_id {
            41 => (SERVER_A, "token-for-a"),
            42 => (SERVER_B, "token-for-b"),
            _ => return Err(Status::permission_denied("not frozen")),
        };
        self.redeemed.lock().expect("redeemed").push(invocation);
        Ok(Response::new(pb::RedeemCredentialResponseV1 {
            result: Some(pb::redeem_credential_response_v1::Result::Envelope(
                pb::CredentialEnvelopeV1 {
                    redemption_id: "redemption-1".to_owned(),
                    kind: pb::CredentialEnvelopeKindV1::OauthAccess as i32,
                    material: [("access_token".to_owned(), token.to_owned())].into(),
                    not_after_unix_millis: 4_102_444_800_000,
                    target_identity: canonical_resource_uri(server).expect("canonical"),
                    obligations: Vec::new(),
                    decision_id: "decision-1".to_owned(),
                },
            )),
        }))
    }
}

impl Broker {
    fn operations(&self) -> Vec<(i64, String, String)> {
        self.redeemed
            .lock()
            .expect("redeemed")
            .iter()
            .map(|invocation| {
                (
                    invocation.toolkit_id,
                    invocation.operation_id.clone(),
                    invocation.resource_ref.clone(),
                )
            })
            .collect()
    }
}

/// Two mock MCP servers. Each records the bearer every session carried and
/// every call; each serves one `lookup` tool.
#[derive(Default)]
struct Servers {
    sessions: Mutex<Vec<(String, Option<String>)>>,
    calls: Mutex<Vec<String>>,
}

struct Connector(Arc<Servers>);

#[async_trait]
impl McpConnector for Connector {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        let endpoint = config.endpoint().to_owned();
        self.0.sessions.lock().expect("sessions").push((
            endpoint.clone(),
            config.access_token_for_test().map(str::to_owned),
        ));
        Ok(Arc::new(BasicToolset::new(
            "mock_mcp",
            vec![Arc::new(Lookup {
                servers: Arc::clone(&self.0),
                endpoint,
            }) as Arc<dyn Tool>],
        )))
    }
}

struct Lookup {
    servers: Arc<Servers>,
    endpoint: String,
}

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
        self.servers
            .calls
            .lock()
            .expect("calls")
            .push(self.endpoint.clone());
        Ok(json!({"server": self.endpoint}))
    }
}

fn mcp_tool(id: Option<u64>, name: &str, url: &str) -> Value {
    let mut tool = json!({"type": "mcp", "toolkit_name": name, "settings": {
        "url": url, "headers": null, "client_id": null, "client_secret": null,
        "timeout": 12, "selected_tools": [], "enable_caching": true, "cache_ttl": 300,
        "ssl_verify": true}});
    if let Some(id) = id {
        tool["id"] = json!(id);
    }
    tool
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

fn errors() -> NestedErrorMapping {
    fn configured(_: ToolsetMaterializationError) -> NativeAgentAssemblyError {
        NativeAgentAssemblyError::new(
            NativeAgentAssemblyErrorCode::InvalidConfiguration,
            "configured",
        )
    }
    fn mcp(_: &McpMaterializationError) -> NativeAgentAssemblyError {
        NativeAgentAssemblyError::new(NativeAgentAssemblyErrorCode::InvalidConfiguration, "mcp")
    }
    NestedErrorMapping { configured, mcp }
}

/// Build the child version `tools` under `authority`, with the governed
/// seam the run's materialization path carries.
async fn build(
    tools: Vec<Value>,
    authority: &Arc<ClaimBoundRuntimeContextAuthority>,
    path: &ApplicationMaterializationPath,
    servers: &Arc<Servers>,
) -> Result<NestedToolsets, NativeAgentAssemblyError> {
    let version = json!({"tools": tools});
    let policy =
        Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("tool admission policy"));
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy.as_ref());
    let connector: Arc<dyn McpConnector> = Arc::new(Connector(Arc::clone(servers)));
    materialize_nested_toolsets(
        &snapshot,
        NestedToolsetInputs {
            authority,
            governed: path.governed(),
            policy: &policy,
            connector: &connector,
            mcp_tokens: &Map::new(),
        },
        &errors(),
    )
    .await
}

async fn call_lookup(toolset: &Arc<dyn Toolset>) -> Value {
    let context: Arc<dyn ToolContext> =
        Arc::new(SimpleToolContext::new("nested").with_function_call_id("call-7"));
    let readonly: Arc<dyn ReadonlyContext> = context.clone();
    let tool = toolset
        .tools(readonly)
        .await
        .expect("tools")
        .into_iter()
        .find(|tool| tool.name() == "lookup")
        .expect("lookup");
    tool.execute(context, json!({"query": "x"}))
        .await
        .expect("governed call")
}

/// The governed run's root path, as the ordinary and pipeline roots build
/// it, and the paths a nested agent and a saved pipeline below it get.
fn governed_paths(
    platform: &Arc<PlatformClient>,
    authority: &Arc<ClaimBoundRuntimeContextAuthority>,
) -> (
    ApplicationMaterializationPath,
    ApplicationMaterializationPath,
) {
    let root = ApplicationMaterializationPath::new(ApplicationContinuation::Scoped)
        .with_governed(governed_client(platform, authority));
    let nested_agent = root.enter_agent((7, 70)).expect("nested agent path");
    let saved_pipeline = nested_agent
        .enter_pipeline((8, 80))
        .expect("saved pipeline path");
    (nested_agent, saved_pipeline)
}

/// Acceptance (#1474): a governed nested agent and a governed saved
/// pipeline call a remote MCP tool through the broker: the listing redeems
/// `mcp.tools.list` once, the call redeems `mcp.tool.call` on the tool, and
/// the session carries only the redeemed token.
#[tokio::test]
async fn governed_nested_agent_and_saved_pipeline_call_mcp_through_the_broker() {
    let broker = Arc::new(Broker::default());
    let platform = platform(&broker);
    let authority = Arc::new(test_runtime_context_authority().governed_for_test(DIGEST));
    let (nested_agent, saved_pipeline) = governed_paths(&platform, &authority);
    for path in [&nested_agent, &saved_pipeline] {
        let servers = Arc::new(Servers::default());
        let before = broker.operations().len();
        let built = build(
            vec![mcp_tool(Some(41), "records", SERVER_A)],
            &authority,
            path,
            &servers,
        )
        .await
        .expect("governed build");
        assert_eq!(built.mcp.len(), 1);
        assert_eq!(
            call_lookup(&built.mcp[0]).await,
            json!({"server": SERVER_A})
        );
        assert_eq!(
            broker.operations()[before..],
            [
                (41, "mcp.tools.list".to_owned(), String::new()),
                (41, "mcp.tool.call".to_owned(), "mcp:tool/lookup".to_owned()),
            ]
        );
        let sessions = servers.sessions.lock().expect("sessions").clone();
        assert!(
            sessions
                .iter()
                .all(|session| *session == (SERVER_A.to_owned(), Some("token-for-a".to_owned()))),
            "{sessions:?}"
        );
        assert_eq!(*servers.calls.lock().expect("calls"), [SERVER_A]);
    }
}

/// Acceptance (#1474): a token for one server is never used for another,
/// in a nested build with two governed servers.
#[tokio::test]
async fn a_nested_token_for_one_server_never_reaches_another() {
    let broker = Arc::new(Broker::default());
    let platform = platform(&broker);
    let authority = Arc::new(test_runtime_context_authority().governed_for_test(DIGEST));
    let (nested_agent, _) = governed_paths(&platform, &authority);
    let servers = Arc::new(Servers::default());
    let built = build(
        vec![
            mcp_tool(Some(41), "alpha", SERVER_A),
            mcp_tool(Some(42), "beta", SERVER_B),
        ],
        &authority,
        &nested_agent,
        &servers,
    )
    .await
    .expect("governed build");
    assert_eq!(built.mcp.len(), 2);
    for _ in 0..2 {
        assert_eq!(
            call_lookup(&built.mcp[0]).await,
            json!({"server": SERVER_A})
        );
        assert_eq!(
            call_lookup(&built.mcp[1]).await,
            json!({"server": SERVER_B})
        );
    }
    let sessions = servers.sessions.lock().expect("sessions").clone();
    for (server, token) in &sessions {
        let own = if server == SERVER_A {
            "token-for-a"
        } else {
            "token-for-b"
        };
        assert_eq!(token.as_deref(), Some(own), "{sessions:?}");
    }
}

/// An MCP connection with no saved toolkit id (in a frozen snapshot, only
/// Main's internal builder servers have none) cannot be governed: a
/// governed nested build fails closed and redeems nothing.
#[tokio::test]
async fn a_governed_nested_mcp_connection_without_a_toolkit_id_fails_closed() {
    let broker = Arc::new(Broker::default());
    let platform = platform(&broker);
    let authority = Arc::new(test_runtime_context_authority().governed_for_test(DIGEST));
    let (nested_agent, saved_pipeline) = governed_paths(&platform, &authority);
    for path in [&nested_agent, &saved_pipeline] {
        let servers = Arc::new(Servers::default());
        let internal_builder = json!({
            "type": "mcp_elitea_internal_applications",
            "toolkit_name": "Elitea Applications",
            "name": "Elitea Applications",
            "meta": {"mcp": true, "internal_builder": true},
            "settings": {
                "server_name": "mcp_elitea_internal_applications",
                "url": SERVER_A,
                "headers": {"Authorization": "Bearer actor-token"},
                "ssl_verify": true
            }
        });
        let Err(error) = build(vec![internal_builder], &authority, path, &servers).await else {
            panic!("refused");
        };
        assert_eq!(
            error.code(),
            NativeAgentAssemblyErrorCode::UnsupportedCapability
        );
        assert_eq!(servers.sessions.lock().expect("sessions").len(), 0);
    }
    assert_eq!(broker.operations().len(), 0);
}

/// A governed run that reaches a nested build without its governed seam
/// is refused, so no claim-time credential is used.
#[tokio::test]
async fn a_governed_nested_build_without_its_seam_is_refused() {
    let authority = Arc::new(test_runtime_context_authority().governed_for_test(DIGEST));
    let servers = Arc::new(Servers::default());
    let Err(error) = build(
        vec![mcp_tool(Some(41), "records", SERVER_A)],
        &authority,
        &ApplicationMaterializationPath::default(),
        &servers,
    )
    .await
    else {
        panic!("refused");
    };
    assert_eq!(
        error.code(),
        NativeAgentAssemblyErrorCode::UnsupportedCapability
    );
    assert_eq!(servers.sessions.lock().expect("sessions").len(), 0);
}

/// A legacy run is unchanged: no governed seam, no redemption, and the MCP
/// connection is built with its claim-time settings.
#[tokio::test]
async fn a_legacy_nested_build_never_redeems() {
    let broker = Arc::new(Broker::default());
    let platform = platform(&broker);
    let legacy = Arc::new(test_runtime_context_authority());
    let path = ApplicationMaterializationPath::default()
        .with_governed(governed_client(&platform, &legacy));
    assert!(path.governed().is_none());
    let servers = Arc::new(Servers::default());
    let built = build(
        vec![mcp_tool(Some(41), "records", SERVER_A)],
        &legacy,
        &path,
        &servers,
    )
    .await
    .expect("legacy build");
    assert_eq!(built.mcp.len(), 1);
    assert_eq!(
        call_lookup(&built.mcp[0]).await,
        json!({"server": SERVER_A})
    );
    assert_eq!(broker.operations().len(), 0);
    let sessions = servers.sessions.lock().expect("sessions").clone();
    assert!(
        sessions.iter().all(|(_, token)| token.is_none()),
        "{sessions:?}"
    );
}
