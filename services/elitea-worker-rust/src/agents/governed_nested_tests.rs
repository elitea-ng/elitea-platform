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
            artifacts: None,
            index_tools: crate::toolkits::IndexToolSupport::Withheld(
                crate::toolkits::IndexToolsWithheld::NotServedOnThisPath,
            ),
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

#[derive(Default)]
struct StubPlatformWriter {
    reads: Mutex<Vec<(String, String)>>,
}

#[async_trait]
impl elitea_agent_runtime::host::PlatformWriter for StubPlatformWriter {
    async fn list_artifacts(
        &self,
        request: &elitea_agent_runtime::platform::ArtifactListRequest,
    ) -> Result<
        elitea_agent_runtime::platform::ArtifactListOutcome,
        elitea_agent_runtime::host::HostError,
    > {
        Ok(elitea_agent_runtime::platform::ArtifactListOutcome {
            bucket: request.bucket.clone(),
            files: Vec::new(),
            truncated: false,
        })
    }

    async fn read_artifact(
        &self,
        request: &elitea_agent_runtime::platform::ArtifactReadRequest,
    ) -> Result<
        elitea_agent_runtime::platform::ArtifactReadOutcome,
        elitea_agent_runtime::host::HostError,
    > {
        self.reads
            .lock()
            .expect("reads")
            .push((request.bucket.clone(), request.name.clone()));
        Ok(elitea_agent_runtime::platform::ArtifactReadOutcome {
            name: request.name.clone(),
            media_type: "text/plain".to_owned(),
            byte_length: 2,
            char_length: 2,
            total_lines: 1,
            max_chars: 200_000,
            over_limit: false,
            content: "ok".to_owned(),
        })
    }

    async fn write_artifact(
        &self,
        _request: &elitea_agent_runtime::platform::ArtifactWriteRequest,
    ) -> Result<
        elitea_agent_runtime::platform::ArtifactWriteOutcome,
        elitea_agent_runtime::host::HostError,
    > {
        Err(elitea_agent_runtime::host::HostError::new(
            elitea_agent_runtime::host::HostErrorCode::InvalidConfiguration,
            "unused",
        ))
    }

    async fn delete_artifact(
        &self,
        _request: &elitea_agent_runtime::platform::ArtifactDeleteRequest,
    ) -> Result<
        elitea_agent_runtime::platform::ArtifactDeleteOutcome,
        elitea_agent_runtime::host::HostError,
    > {
        Err(elitea_agent_runtime::host::HostError::new(
            elitea_agent_runtime::host::HostErrorCode::InvalidConfiguration,
            "unused",
        ))
    }

    async fn write_skill(
        &self,
        _request: &elitea_agent_runtime::platform::SkillWriteRequest,
    ) -> Result<
        elitea_agent_runtime::platform::SkillWriteOutcome,
        elitea_agent_runtime::host::HostError,
    > {
        Err(elitea_agent_runtime::host::HostError::new(
            elitea_agent_runtime::host::HostErrorCode::InvalidConfiguration,
            "unused",
        ))
    }

    async fn write_project_context(
        &self,
        _request: &elitea_agent_runtime::platform::ProjectContextWriteRequest,
    ) -> Result<
        elitea_agent_runtime::platform::ProjectContextWriteOutcome,
        elitea_agent_runtime::host::HostError,
    > {
        Err(elitea_agent_runtime::host::HostError::new(
            elitea_agent_runtime::host::HostErrorCode::InvalidConfiguration,
            "unused",
        ))
    }
}

#[derive(Default)]
struct StubMemoryHost {
    calls: Mutex<Vec<String>>,
}

#[async_trait]
impl elitea_agent_runtime::toolkits::IndexToolHost for StubMemoryHost {
    async fn call(
        &self,
        _call: elitea_agent_runtime::toolkits::IndexToolCall<'_>,
    ) -> Result<Value, elitea_agent_runtime::toolkits::IndexToolFailure> {
        Ok(json!("unused"))
    }

    fn serves_memory(&self) -> bool {
        true
    }

    async fn memory(
        &self,
        call: elitea_agent_runtime::toolkits::MemoryToolCall<'_>,
    ) -> Result<Value, elitea_agent_runtime::toolkits::IndexToolFailure> {
        self.calls
            .lock()
            .expect("calls")
            .push(format!("{}:{}", call.toolkit_id, call.namespace));
        Ok(json!("stored"))
    }
}

/// FU-353 D4(a) / D6(a): nested `artifact` toolkit materializes under a pinned
/// authority on both legacy and governed runs, allows its configured bucket,
/// and refuses cross-bucket overrides without touching `PlatformWriter`.
#[tokio::test]
async fn nested_artifact_toolkit_materializes_pinned_and_refuses_cross_bucket() {
    let broker = Arc::new(Broker::default());
    let platform = platform(&broker);
    let authority = Arc::new(test_runtime_context_authority().governed_for_test(DIGEST));
    let path = ApplicationMaterializationPath::default()
        .with_governed(governed_client(&platform, &authority));
    let writer = Arc::new(StubPlatformWriter::default());
    let pinned = crate::toolkits::ArtifactToolAuthority::new(writer.clone()).with_pinned_bucket();
    let version = json!({
        "tools": [{
            "id": 50,
            "type": "artifact",
            "toolkit_name": "docs_kit",
            "settings": {
                "bucket": "docs",
                "selected_tools": ["list_files", "read_file"]
            }
        }]
    });
    let policy =
        Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("tool admission policy"));
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy.as_ref());
    let servers = Arc::new(Servers::default());
    let connector: Arc<dyn McpConnector> = Arc::new(Connector(servers));
    let built = materialize_nested_toolsets(
        &snapshot,
        NestedToolsetInputs {
            authority: &authority,
            governed: path.governed(),
            policy: &policy,
            connector: &connector,
            mcp_tokens: &Map::new(),
            artifacts: Some(&pinned),
            index_tools: crate::toolkits::IndexToolSupport::Withheld(
                crate::toolkits::IndexToolsWithheld::NotRequested,
            ),
        },
        &errors(),
    )
    .await
    .expect("nested artifact build");
    assert_eq!(built.configured.len(), 1);
    let context: Arc<dyn ToolContext> =
        Arc::new(SimpleToolContext::new("nested").with_function_call_id("call-art"));
    let readonly: Arc<dyn ReadonlyContext> = context.clone();
    let tools = built.configured[0].tools(readonly).await.expect("tools");
    let read_file = tools
        .into_iter()
        .find(|tool| tool.name() == "read_file")
        .expect("read_file tool");
    let ok = read_file
        .execute(context.clone(), json!({"filepath": "/docs/note.txt"}))
        .await
        .expect("ok read");
    assert!(ok.as_str().is_some_and(|s| s.contains("ok")), "{ok}");
    let refused = read_file
        .execute(context, json!({"filepath": "/other/secret.txt"}))
        .await
        .expect("refused read");
    assert!(
        refused
            .as_str()
            .is_some_and(|s| s.contains("restricted to its configured bucket 'docs'")),
        "{refused}"
    );
    assert_eq!(
        *writer.reads.lock().expect("reads"),
        vec![("docs".to_owned(), "note.txt".to_owned())]
    );
}

/// FU-353 D6(a): nested `memory` toolkit materializes when claim-bound index
/// support is lent (on governed and legacy runs) and fails closed when withheld.
#[tokio::test]
async fn nested_memory_toolkit_materializes_when_lent_and_withholds_when_absent() {
    let broker = Arc::new(Broker::default());
    let platform = platform(&broker);
    let authority = Arc::new(test_runtime_context_authority().governed_for_test(DIGEST));
    let path = ApplicationMaterializationPath::default()
        .with_governed(governed_client(&platform, &authority));
    let version = json!({
        "tools": [{
            "id": 51,
            "type": "memory",
            "toolkit_name": "notes_mem",
            "settings": {
                "namespace": "notes",
                "selected_tools": ["manage_memory", "search_memory"]
            }
        }]
    });
    let policy =
        Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("tool admission policy"));
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy.as_ref());
    let servers = Arc::new(Servers::default());
    let connector: Arc<dyn McpConnector> = Arc::new(Connector(servers));

    let host = Arc::new(StubMemoryHost::default());
    let index_auth = crate::toolkits::IndexToolAuthority::new(host.clone());
    let built = materialize_nested_toolsets(
        &snapshot,
        NestedToolsetInputs {
            authority: &authority,
            governed: path.governed(),
            policy: &policy,
            connector: &connector,
            mcp_tokens: &Map::new(),
            artifacts: None,
            index_tools: crate::toolkits::IndexToolSupport::Lent(&index_auth),
        },
        &errors(),
    )
    .await
    .expect("nested memory build");
    assert_eq!(built.configured.len(), 1);
    let context: Arc<dyn ToolContext> =
        Arc::new(SimpleToolContext::new("nested").with_function_call_id("call-mem"));
    let readonly: Arc<dyn ReadonlyContext> = context.clone();
    let tools = built.configured[0].tools(readonly).await.expect("tools");
    let store = tools
        .into_iter()
        .find(|tool| tool.name() == "manage_memory")
        .expect("manage_memory tool");
    let res = store
        .execute(
            context,
            json!({"action": "create", "content": "remember this"}),
        )
        .await
        .expect("manage_memory");
    assert_eq!(res, json!("stored"));
    assert_eq!(
        *host.calls.lock().expect("calls"),
        vec!["51:notes".to_owned()]
    );

    // Without a vector token, the nested memory toolkit is withheld cleanly.
    let withheld = materialize_nested_toolsets(
        &snapshot,
        NestedToolsetInputs {
            authority: &authority,
            governed: path.governed(),
            policy: &policy,
            connector: &connector,
            mcp_tokens: &Map::new(),
            artifacts: None,
            index_tools: crate::toolkits::IndexToolSupport::Withheld(
                crate::toolkits::IndexToolsWithheld::NoVectorToken,
            ),
        },
        &errors(),
    )
    .await
    .expect("withheld build");
    assert!(withheld.configured.is_empty());
}
