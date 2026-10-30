//! Governed remote MCP connections (#1123): one redeemed listing per build,
//! one redeemed session per call, and the token only ever sent to the one
//! server its envelope names.

use std::collections::BTreeMap;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use serde_json::{Value, json};

use super::delegated_authorization_requirement;
use super::governed::adapters::mcp::{MCP_TOOL_CALL, MCP_TOOLS_LIST};
use super::governed::adapters::reference_registry;
use super::governed::{
    BoundToolInvocation, CredentialRedeemDenial, EnvelopeKind, GovernedOperationClient,
    RedeemOutcome, canonical_resource_uri,
};
use super::governed_tests::{RecordingRedeemer, Respond, context, envelope, policy};
use super::materialize::LeftOutReason;
use super::mcp::{
    McpConnector, McpMaterializationError, McpSession, RemoteMcpConfig,
    materialize_governed_mcp_toolsets,
};
use super::snapshot::FrozenToolSnapshot;

const SERVER_A: &str = "https://a.mcp.example.test/mcp";
const SERVER_B: &str = "https://b.mcp.example.test/v1/mcp";

/// Two mock MCP servers behind one connector. Each records every session
/// it is dialled with and the bearer that session carried, every
/// `tools/list`, and serves one `lookup` tool that records its calls.
#[derive(Default)]
struct MockServers {
    sessions: Mutex<Vec<(String, Option<String>)>>,
    calls: Mutex<Vec<String>>,
    listings: Mutex<Vec<String>>,
    /// Servers whose `tools/list` fails, as a 401 for a narrow scope does.
    refuse_listing: Mutex<Vec<String>>,
    /// Listings wait here, to prove they overlap.
    barrier: Mutex<Option<Arc<tokio::sync::Barrier>>>,
}

impl MockServers {
    fn sessions(&self) -> Vec<(String, Option<String>)> {
        self.sessions.lock().expect("sessions").clone()
    }

    fn calls(&self) -> Vec<String> {
        self.calls.lock().expect("calls").clone()
    }

    fn listings(&self) -> Vec<String> {
        self.listings.lock().expect("listings").clone()
    }
}

struct Connector(Arc<MockServers>);

#[async_trait]
impl McpConnector for Connector {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        self.connect_session(config)
            .await
            .map(|session| session.toolset)
    }

    async fn connect_session(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<McpSession, McpMaterializationError> {
        let endpoint = config.endpoint().to_owned();
        let token = config.access_token_for_test().map(str::to_owned);
        self.0
            .sessions
            .lock()
            .expect("sessions")
            .push((endpoint.clone(), token));
        let tool: Arc<dyn Tool> = Arc::new(Lookup {
            servers: Arc::clone(&self.0),
            endpoint: endpoint.clone(),
        });
        Ok(McpSession {
            toolset: Arc::new(Listed {
                servers: Arc::clone(&self.0),
                endpoint,
                tool,
            }),
            reuses_listings: false,
        })
    }
}

/// One session's `tools/list`.
struct Listed {
    servers: Arc<MockServers>,
    endpoint: String,
    tool: Arc<dyn Tool>,
}

#[async_trait]
impl Toolset for Listed {
    fn name(&self) -> &'static str {
        "mock_mcp"
    }

    async fn tools(&self, _ctx: Arc<dyn ReadonlyContext>) -> adk_core::Result<Vec<Arc<dyn Tool>>> {
        self.servers
            .listings
            .lock()
            .expect("listings")
            .push(self.endpoint.clone());
        let barrier = self.servers.barrier.lock().expect("barrier").clone();
        if let Some(barrier) = barrier {
            barrier.wait().await;
        }
        if self
            .servers
            .refuse_listing
            .lock()
            .expect("refused")
            .contains(&self.endpoint)
        {
            return Err(adk_core::AdkError::tool(
                "401 Unauthorized: insufficient_scope",
            ));
        }
        Ok(vec![Arc::clone(&self.tool)])
    }
}

struct Lookup {
    servers: Arc<MockServers>,
    endpoint: String,
}

#[async_trait]
impl Tool for Lookup {
    fn name(&self) -> &'static str {
        "lookup"
    }

    fn description(&self) -> &'static str {
        "Look up a release."
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({"type": "object", "properties": {"query": {"type": "string"}}}))
    }

    fn is_read_only(&self) -> bool {
        true
    }

    async fn execute(&self, _ctx: Arc<dyn ToolContext>, _args: Value) -> adk_core::Result<Value> {
        self.servers
            .calls
            .lock()
            .expect("calls")
            .push(self.endpoint.clone());
        Ok(json!({"server": self.endpoint}))
    }
}

fn mcp_tool(id: Option<u64>, name: &str, url: &str, extra: &Value) -> Value {
    let mut settings = json!({
        "url": url, "headers": null, "client_id": null, "client_secret": null,
        "timeout": 12, "selected_tools": [], "enable_caching": true, "cache_ttl": 300,
        "ssl_verify": true
    });
    if let (Some(settings), Some(extra)) = (settings.as_object_mut(), extra.as_object()) {
        settings.extend(extra.clone());
    }
    let mut tool = json!({"type": "mcp", "toolkit_name": name, "settings": settings});
    if let Some(id) = id {
        tool["id"] = json!(id);
    }
    tool
}

/// A redeemer that answers each toolkit with its own server's envelope.
fn per_server() -> Box<Respond> {
    Box::new(|invocation: &BoundToolInvocation| {
        let (target, token) = match invocation.toolkit_id {
            41 => (SERVER_A, "token-for-a"),
            42 => (SERVER_B, "token-for-b"),
            other => panic!("unexpected toolkit {other}"),
        };
        Ok(RedeemOutcome::Envelope(envelope(
            EnvelopeKind::OauthAccess,
            &[("access_token", token)],
            &canonical_resource_uri(target).expect("canonical"),
        )))
    })
}

fn governed(respond: Box<Respond>) -> (Arc<GovernedOperationClient>, Arc<RecordingRedeemer>) {
    let redeemer = RecordingRedeemer::new(respond);
    let client = Arc::new(GovernedOperationClient::new(
        Arc::new(reference_registry()),
        Arc::clone(&redeemer) as Arc<dyn super::governed::CredentialRedeemer>,
    ));
    (client, redeemer)
}

async fn build(
    tools: Vec<Value>,
    respond: Box<Respond>,
) -> (
    super::mcp::GovernedMcpToolsets,
    Arc<MockServers>,
    Arc<RecordingRedeemer>,
) {
    build_on(Arc::new(MockServers::default()), tools, respond).await
}

async fn build_on(
    servers: Arc<MockServers>,
    tools: Vec<Value>,
    respond: Box<Respond>,
) -> (
    super::mcp::GovernedMcpToolsets,
    Arc<MockServers>,
    Arc<RecordingRedeemer>,
) {
    let version = json!({"tools": tools});
    let policy = policy();
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy.as_ref());
    let connector: Arc<dyn McpConnector> = Arc::new(Connector(Arc::clone(&servers)));
    let (client, redeemer) = governed(respond);
    let built = materialize_governed_mcp_toolsets(&snapshot, &connector, &policy, &client)
        .await
        .expect("governed MCP");
    (built, servers, redeemer)
}

async fn lookup(toolset: &Arc<dyn Toolset>) -> Arc<dyn Tool> {
    let readonly: Arc<dyn ReadonlyContext> = context();
    toolset
        .tools(readonly)
        .await
        .expect("tools")
        .into_iter()
        .find(|tool| tool.name() == "lookup")
        .expect("lookup")
}

/// Acceptance (#1123): the listing redeems `mcp.tools.list` once per server
/// with an empty resource; a call redeems `mcp.tool.call` on the tool and
/// dials a fresh session to ITS server only, carrying only that server's
/// token. Server B never receives server A's token.
#[tokio::test]
async fn a_redeemed_token_reaches_only_its_own_server() {
    let (built, servers, redeemer) = build(
        vec![
            mcp_tool(Some(41), "alpha", SERVER_A, &json!({})),
            mcp_tool(
                Some(42),
                "beta",
                SERVER_B,
                &json!({"credential_reference_id": "ref-b"}),
            ),
        ],
        per_server(),
    )
    .await;
    assert_eq!(built.toolsets.len(), 2);
    assert_eq!(built.left_out.len(), 0);
    let listings = redeemer.calls();
    assert_eq!(listings.len(), 2, "one listing per server");
    for listing in &listings {
        assert_eq!(listing.operation_id, MCP_TOOLS_LIST);
        assert_eq!(listing.resource_ref, "");
        assert_eq!(listing.tool_call_id, "elitea.mcp.tools.list");
    }
    assert_eq!(listings[1].credential_reference_id, "ref-b");
    assert_eq!(
        servers.sessions(),
        [
            (SERVER_A.to_owned(), Some("token-for-a".to_owned())),
            (SERVER_B.to_owned(), Some("token-for-b".to_owned())),
        ]
    );
    for toolset in &built.toolsets {
        assert_eq!(
            super::mcp::governed_declared_names_for_test(toolset).await,
            ["lookup"]
        );
    }

    let result = lookup(&built.toolsets[0])
        .await
        .execute(context(), json!({"query": "1.2"}))
        .await
        .expect("governed call");
    assert_eq!(result, json!({"server": SERVER_A}));
    let calls = redeemer.calls();
    let call = calls.last().expect("call redemption");
    assert_eq!(call.operation_id, MCP_TOOL_CALL);
    assert_eq!(call.resource_ref, "mcp:tool/lookup");
    assert_eq!(call.toolkit_id, 41);
    assert_eq!(call.tool_call_id, "call-1");
    let sessions = servers.sessions();
    assert_eq!(
        sessions.last(),
        Some(&(SERVER_A.to_owned(), Some("token-for-a".to_owned())))
    );
    assert!(
        sessions
            .iter()
            .all(|(server, token)| server != SERVER_B || token.as_deref() == Some("token-for-b")),
        "server B received another server's token: {sessions:?}"
    );
    assert_eq!(servers.calls(), [SERVER_A]);
    assert!(!result.to_string().contains("token-for-a"));
}

/// Acceptance (#1123): an envelope for another server is refused before the
/// session is dialled, so no byte (and no token) reaches either server.
#[tokio::test]
async fn an_envelope_for_another_server_is_refused_before_any_session() {
    let answers = Arc::new(AtomicUsize::new(0));
    let counter = Arc::clone(&answers);
    let (built, servers, _) = build(
        vec![mcp_tool(Some(41), "alpha", SERVER_A, &json!({}))],
        Box::new(move |_| {
            // The listing is answered for A; the call with B's envelope.
            let target = if counter.fetch_add(1, Ordering::SeqCst) == 0 {
                SERVER_A
            } else {
                SERVER_B
            };
            Ok(RedeemOutcome::Envelope(envelope(
                EnvelopeKind::OauthAccess,
                &[("access_token", "token-for-b")],
                &canonical_resource_uri(target).expect("canonical"),
            )))
        }),
    )
    .await;
    let dialled = servers.sessions().len();
    let error = lookup(&built.toolsets[0])
        .await
        .execute(context(), json!({}))
        .await
        .expect_err("another server's envelope");
    // The admitted toolset reports `governed.target_mismatch` (Forbidden) by
    // its category only, as it does every tool error.
    assert_eq!(error.code, "tool.execution.forbidden");
    assert_eq!(servers.sessions().len(), dialled, "no session was dialled");
    assert_eq!(servers.calls().len(), 0);
}

/// Acceptance (#1123): a connection that cannot be bound to a canonical
/// resource URI, or whose dialled endpoint is another resource than its
/// configured one, is left out as not governable BEFORE any redemption.
#[tokio::test]
async fn a_server_uri_off_the_canonical_target_is_refused_before_redemption() {
    let (built, servers, redeemer) = build(
        vec![
            // `%` is refused by the resource_uri rule on both sides.
            mcp_tool(
                Some(41),
                "alpha",
                "https://a.mcp.example.test/a%20b/mcp",
                &json!({}),
            ),
            // The dialled endpoint resolves `..`; the target refuses it.
            mcp_tool(
                Some(42),
                "beta",
                "https://b.mcp.example.test/x/../mcp",
                &json!({}),
            ),
        ],
        per_server(),
    )
    .await;
    assert_eq!(built.toolsets.len(), 0);
    assert_eq!(built.left_out.len(), 2);
    assert!(
        built
            .left_out
            .iter()
            .all(|left| left.cause() == LeftOutReason::ToolkitNotGovernable)
    );
    assert_eq!(redeemer.calls().len(), 0, "nothing was redeemed");
    assert_eq!(servers.sessions().len(), 0, "nothing was dialled");
}

/// A connection with no saved toolkit id (Main binds every grant to one; in
/// a frozen snapshot only Main's internal builder servers have none), or
/// whose own headers carry a credential, is not governable: left out, never
/// redeemed, never dialled.
#[tokio::test]
async fn an_unsaved_or_self_credentialed_connection_is_not_governable() {
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
    let (built, servers, redeemer) = build(
        vec![
            internal_builder,
            mcp_tool(
                Some(42),
                "pat",
                SERVER_B,
                &json!({"headers": {"Authorization": "Bearer claim-time-pat"}}),
            ),
        ],
        per_server(),
    )
    .await;
    assert_eq!(built.toolsets.len(), 0);
    let names: Vec<_> = built
        .left_out
        .iter()
        .map(super::materialize::LeftOutToolkit::toolkit_name)
        .collect();
    assert_eq!(names, ["Elitea Applications", "pat"]);
    assert!(
        built
            .left_out
            .iter()
            .all(|left| left.reason_code() == "toolkit_not_governable")
    );
    assert_eq!(redeemer.calls().len(), 0);
    assert_eq!(servers.sessions().len(), 0);
}

/// Acceptance (#1123, compatible with #1061): `AUTHORIZATION_REQUIRED`
/// surfaces the broker's `authorization_reference` through the existing
/// delegated authorization path: at the listing as the toolkit's
/// requirement (placeholders for a selection), at a call as the signal the
/// run pauses on. Nothing is dialled.
#[tokio::test]
async fn authorization_required_surfaces_the_authorization_reference() {
    let denial: Box<Respond> = Box::new(|_| {
        Ok(RedeemOutcome::Denial(CredentialRedeemDenial::new(
            "AUTHORIZATION_REQUIRED",
            "decision-1".to_owned(),
            "mar_opaque-reference".to_owned(),
        )))
    });
    let (built, servers, _) = build(
        vec![mcp_tool(
            Some(41),
            "alpha",
            SERVER_A,
            &json!({"selected_tools": ["lookup"]}),
        )],
        denial,
    )
    .await;
    assert_eq!(built.toolsets.len(), 1);
    let requirement = built
        .delegated_authorization
        .requirement_for_direct("alpha", "lookup")
        .expect("the selected tool's requirement");
    assert_eq!(
        requirement.authorization_reference(),
        Some("mar_opaque-reference")
    );
    assert_eq!(requirement.toolkit_type(), "mcp");
    assert_eq!(
        requirement.server_url(),
        canonical_resource_uri(SERVER_A).expect("canonical")
    );
    let error = lookup(&built.toolsets[0])
        .await
        .execute(context(), json!({}))
        .await
        .expect_err("authorization required");
    let signal = delegated_authorization_requirement(&error).expect("the delegated signal");
    assert_eq!(
        signal.authorization_reference(),
        Some("mar_opaque-reference")
    );
    assert_eq!(servers.sessions().len(), 0);

    // With no selection the requirement is a discovery one, and no toolset
    // is built.
    let (built, _, _) = build(
        vec![mcp_tool(Some(41), "alpha", SERVER_A, &json!({}))],
        Box::new(|_| {
            Ok(RedeemOutcome::Denial(CredentialRedeemDenial::new(
                "AUTHORIZATION_REQUIRED",
                "decision-2".to_owned(),
                "mar_discovery".to_owned(),
            )))
        }),
    )
    .await;
    assert_eq!(built.toolsets.len(), 0);
    assert_eq!(built.left_out.len(), 0);

    // At a call: listing granted, the call asks for consent.
    let answers = Arc::new(AtomicUsize::new(0));
    let counter = Arc::clone(&answers);
    let (built, servers, _) = build(
        vec![mcp_tool(Some(41), "alpha", SERVER_A, &json!({}))],
        Box::new(move |_| {
            if counter.fetch_add(1, Ordering::SeqCst) == 0 {
                return Ok(RedeemOutcome::Envelope(envelope(
                    EnvelopeKind::OauthAccess,
                    &[("access_token", "token-for-a")],
                    &canonical_resource_uri(SERVER_A).expect("canonical"),
                )));
            }
            Ok(RedeemOutcome::Denial(CredentialRedeemDenial::new(
                "AUTHORIZATION_REQUIRED",
                "decision-3".to_owned(),
                "mar_call".to_owned(),
            )))
        }),
    )
    .await;
    let dialled = servers.sessions().len();
    let error = lookup(&built.toolsets[0])
        .await
        .execute(context(), json!({}))
        .await
        .expect_err("authorization required at the call");
    let signal = delegated_authorization_requirement(&error).expect("the delegated signal");
    assert_eq!(signal.authorization_reference(), Some("mar_call"));
    assert_eq!(servers.sessions().len(), dialled);
}

/// A denied listing leaves the connection out; nothing is dialled.
#[tokio::test]
async fn a_denied_listing_leaves_the_connection_out() {
    let (built, servers, _) = build(
        vec![mcp_tool(Some(41), "alpha", SERVER_A, &json!({}))],
        Box::new(|_| {
            Ok(RedeemOutcome::Denial(CredentialRedeemDenial::new(
                "DENIED",
                "decision-1".to_owned(),
                String::new(),
            )))
        }),
    )
    .await;
    assert_eq!(built.toolsets.len(), 0);
    assert_eq!(built.left_out.len(), 1);
    assert_eq!(servers.sessions().len(), 0);
}

/// Review of #1123: a server whose `tools/list` fails (here a 401 for a
/// narrow scope) leaves only that connection out, named in the run notice;
/// the other server's tools are built and the run goes on.
#[tokio::test]
async fn a_failed_listing_leaves_only_that_connection_out() {
    let servers = Arc::new(MockServers::default());
    servers
        .refuse_listing
        .lock()
        .expect("refused")
        .push(SERVER_B.to_owned());
    let (built, servers, _) = build_on(
        servers,
        vec![
            mcp_tool(Some(41), "alpha", SERVER_A, &json!({})),
            mcp_tool(Some(42), "beta", SERVER_B, &json!({})),
        ],
        per_server(),
    )
    .await;
    assert_eq!(built.toolsets.len(), 1);
    let left: Vec<_> = built
        .left_out
        .iter()
        .map(super::materialize::LeftOutToolkit::toolkit_name)
        .collect();
    assert_eq!(left, ["beta"]);
    assert_eq!(
        super::mcp::governed_declared_names_for_test(&built.toolsets[0]).await,
        ["lookup"]
    );
    assert_eq!(servers.listings(), [SERVER_A, SERVER_B]);
}

/// Review of #1123: the build's listings run concurrently (both wait on
/// one barrier, which a one-at-a-time build would never pass), in the
/// snapshot's order. A call still lists on its own fresh session and runs
/// through the listed ADK tool, the one execution path every MCP tool uses.
#[tokio::test]
async fn listings_overlap_and_a_call_lists_on_its_own_session() {
    let servers = Arc::new(MockServers::default());
    *servers.barrier.lock().expect("barrier") = Some(Arc::new(tokio::sync::Barrier::new(2)));
    let (built, servers, _) = tokio::time::timeout(
        std::time::Duration::from_secs(10),
        build_on(
            servers,
            vec![
                mcp_tool(Some(41), "alpha", SERVER_A, &json!({})),
                mcp_tool(Some(42), "beta", SERVER_B, &json!({})),
            ],
            per_server(),
        ),
    )
    .await
    .expect("the two listings ran concurrently");
    assert_eq!(built.toolsets.len(), 2);
    *servers.barrier.lock().expect("barrier") = None;
    let listed = servers.listings().len();

    let result = lookup(&built.toolsets[1])
        .await
        .execute(context(), json!({"query": "1.2"}))
        .await
        .expect("governed call");
    assert_eq!(result, json!({"server": SERVER_B}));
    let listings = servers.listings();
    assert_eq!(listings.len(), listed + 1, "the call lists on its session");
    assert_eq!(listings.last().map(String::as_str), Some(SERVER_B));
    assert_eq!(
        servers.sessions().last(),
        Some(&(SERVER_B.to_owned(), Some("token-for-b".to_owned())))
    );
    assert_eq!(servers.calls(), [SERVER_B]);
}

/// The MCP adapter's two operations are the shared catalogue's `mcp`
/// operations exactly, with their resource kinds and the `resource_uri`
/// target kind (the general contract test covers classes and keys).
#[test]
fn the_mcp_operations_are_the_shared_catalogue_entry() {
    use super::governed::adapters::mcp::{MCP_SERVER_RESOURCE_KIND, MCP_TOOL_RESOURCE_KIND};
    let path = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../../testdata/proto/runtime/v1/governed-operations/catalogue.json"
    );
    let catalogue: Value =
        serde_json::from_str(&std::fs::read_to_string(path).expect("catalogue")).expect("JSON");
    let mcp = catalogue["providers"]
        .as_array()
        .expect("providers")
        .iter()
        .find(|provider| provider["provider_type"] == "mcp")
        .expect("the mcp provider");
    assert_eq!(mcp["target_kind"], "resource_uri");
    let operations: BTreeMap<&str, (&str, &str)> = mcp["operations"]
        .as_array()
        .expect("operations")
        .iter()
        .map(|op| {
            (
                op["id"].as_str().expect("id"),
                (
                    op["class"].as_str().expect("class"),
                    op["resource_kind"].as_str().expect("kind"),
                ),
            )
        })
        .collect();
    assert_eq!(
        operations,
        BTreeMap::from([
            (MCP_TOOL_CALL, ("write", MCP_TOOL_RESOURCE_KIND)),
            (MCP_TOOLS_LIST, ("read", MCP_SERVER_RESOURCE_KIND)),
        ])
    );
}

/// The worker's `resource_uri` target equals Main's for every shared
/// vector, and both refuse the same inputs.
#[test]
fn canonical_resource_uri_matches_the_shared_vectors() {
    let path = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../../testdata/proto/runtime/v1/governed-operations/targets.json"
    );
    let vectors: Value =
        serde_json::from_str(&std::fs::read_to_string(path).expect("targets")).expect("JSON");
    let vectors = vectors["resource_uri_vectors"]
        .as_array()
        .expect("resource_uri vectors");
    assert_ne!(vectors.len(), 0);
    let mut seen = BTreeMap::new();
    for vector in vectors {
        let input = vector["input"].as_str().expect("input");
        let want = vector["canonical"].as_str();
        assert_eq!(canonical_resource_uri(input).as_deref(), want, "{input:?}");
        seen.insert(input, want);
    }
    assert!(
        seen.values().any(Option::is_none),
        "refusals are pinned too"
    );
}

/// A governed run is always under an allowlist (#1490). A server whose host
/// is not on it is never contacted, as on the legacy path: no redemption, no
/// listing and no session; its selected tools stay listed and fail as
/// `egress_host_not_allowed` naming the host. A server on the list is served
/// and counted as reached.
#[tokio::test]
async fn a_server_outside_the_runs_allowlist_is_never_contacted() {
    use crate::egress::{EGRESS_HOST_NOT_ALLOWED, PublicPolicy, RunEgress};

    let egress =
        RunEgress::new(PublicPolicy::allowlist(["a.mcp.example.test"]).expect("allowlist"));
    let policy = Arc::new(
        super::policy::ToolAdmissionPolicy::new(&[], &BTreeMap::new())
            .expect("policy")
            .with_egress(Arc::clone(&egress)),
    );
    let version = json!({"tools": [
        mcp_tool(Some(41), "alpha", SERVER_A, &json!({})),
        mcp_tool(Some(42), "beta", SERVER_B, &json!({"selected_tools": ["lookup"]})),
    ]});
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy.as_ref());
    let servers = Arc::new(MockServers::default());
    let connector: Arc<dyn McpConnector> = Arc::new(Connector(Arc::clone(&servers)));
    let (client, redeemer) = governed(per_server());
    let built = materialize_governed_mcp_toolsets(&snapshot, &connector, &policy, &client)
        .await
        .expect("governed MCP");

    assert!(
        redeemer.calls().iter().all(|call| call.toolkit_id == 41),
        "nothing is redeemed for the refused server"
    );
    assert!(
        servers
            .sessions()
            .iter()
            .all(|(endpoint, _)| endpoint == SERVER_A),
        "the refused server is never dialled: {:?}",
        servers.sessions()
    );
    assert_eq!(built.toolsets.len(), 2);
    let mut refused = None;
    for toolset in &built.toolsets {
        let readonly: Arc<dyn ReadonlyContext> = context();
        for tool in toolset.tools(readonly).await.expect("tools") {
            if let Err(error) = tool.execute(context(), json!({"query": "1"})).await
                && error.code == EGRESS_HOST_NOT_ALLOWED
            {
                refused = Some(error);
            }
        }
    }
    let refused = refused.expect("the refused server's tool fails as egress_host_not_allowed");
    assert!(
        refused.message.contains("b.mcp.example.test:443"),
        "{}",
        refused.message
    );
    assert!(
        servers
            .sessions()
            .iter()
            .all(|(endpoint, _)| endpoint == SERVER_A),
        "a call never dials the refused server either"
    );
    let summary = egress.summary();
    assert!(
        summary
            .reached
            .iter()
            .any(|host| host.host == "a.mcp.example.test"),
        "{summary:?}"
    );
    assert!(
        summary
            .refused
            .iter()
            .any(|host| host.host == "b.mcp.example.test"),
        "{summary:?}"
    );
}
