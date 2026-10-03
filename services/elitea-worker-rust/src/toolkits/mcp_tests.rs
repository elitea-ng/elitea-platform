use std::collections::BTreeMap;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::tool::{BasicToolset, SimpleToolContext};
use adk_rust::{ErrorCategory, ErrorComponent, ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use rmcp::transport::auth::AuthorizationMetadata;
use serde_json::{Map, Value, json};

use super::delegated_auth::delegated_authorization_requirement;
use super::mcp::{
    McpConnector, McpMaterializationError, McpMaterializationErrorCode, McpSession,
    RETIRED_SSE_MESSAGE, RemoteMcpConfig, authorization_resource_metadata,
    materialize_mcp_toolsets, materialize_mcp_toolsets_with_tokens,
    materialize_mcp_toolsets_with_tokens_and_authorization, mcp_authorization_required_fixture,
    names_sse_endpoint, redirect_allowed, retired_sse_failure_for_test,
    same_origin_redirect_policy,
};
use super::mcp_tool_cache::McpToolCaller;
use super::policy::ToolAdmissionPolicy;
use super::snapshot::FrozenToolSnapshot;

fn policy(blocked: &[&str]) -> Arc<ToolAdmissionPolicy> {
    let blocked_tools = if blocked.is_empty() {
        BTreeMap::new()
    } else {
        BTreeMap::from([(
            "mcp".to_owned(),
            blocked.iter().map(ToString::to_string).collect(),
        )])
    };
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked_tools).expect("MCP fixture policy"))
}

struct AuthorizationConnector;

#[async_trait]
impl McpConnector for AuthorizationConnector {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        Err(mcp_authorization_required_fixture(
            config,
            "Bearer resource_metadata=\"https://mcp.example.invalid/.well-known/oauth-protected-resource\"",
        ))
    }
}

struct TokenConnector;

#[async_trait]
impl McpConnector for TokenConnector {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        assert_eq!(config.access_token_for_test(), Some("runtime-secret"));
        let (tool, _) = FixtureTool::new("lookup_release", true, json!({"ok": true}));
        Ok(Arc::new(BasicToolset::new("token_mcp", vec![tool])))
    }
}

#[tokio::test]
async fn undiscovered_mcp_requires_toolkit_authorization_before_exposing_operations() {
    let version = frozen("mcp", &settings(&[]));
    let policy = policy(&[]);
    let snapshot = FrozenToolSnapshot::from_version_details(&version)
        .unwrap()
        .apply_policy(&policy);
    let (tools, authorization) = materialize_mcp_toolsets_with_tokens_and_authorization(
        &snapshot,
        &AuthorizationConnector,
        &policy,
        &Map::new(),
    )
    .await
    .expect("undiscovered toolkit authorization");
    assert!(tools.is_empty(), "no invented remote operation");
    assert!(!authorization.is_empty());

    let tokens = Map::from_iter([(
        "https://mcp.example.invalid/v1/mcp".to_owned(),
        json!({"access_token": "runtime-secret"}),
    )]);
    let (tools, authorization) = materialize_mcp_toolsets_with_tokens_and_authorization(
        &snapshot,
        &TokenConnector,
        &policy,
        &tokens,
    )
    .await
    .unwrap();
    assert!(authorization.is_empty());
    let tools = tools[0].tools(context()).await.unwrap();
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0].name(), "lookup_release");
}

struct PrebuiltConnector {
    expected_type: &'static str,
    expected_authorization: &'static str,
    tools: Vec<Arc<dyn Tool>>,
}

#[async_trait]
impl McpConnector for PrebuiltConnector {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        assert_eq!(config.toolkit_type(), self.expected_type);
        assert_eq!(config.excluded_tools(), ["publish_release"]);
        let headers = config
            .request_headers_for_test()
            .expect("prebuilt request headers");
        let authorization = headers
            .get("authorization")
            .expect("prebuilt authorization header");
        assert_eq!(
            authorization.to_str().expect("authorization text"),
            self.expected_authorization
        );
        assert!(authorization.is_sensitive());
        let platform = headers.get("x-platform").expect("fixed platform header");
        assert_eq!(platform, "fixed-value");
        assert!(platform.is_sensitive());
        Ok(Arc::new(BasicToolset::new(
            "prebuilt_mcp",
            self.tools.clone(),
        )))
    }
}

fn frozen(tool_type: &str, settings: &Value) -> serde_json::Map<String, Value> {
    json!({
        "tools": [{
            "id": 41,
            "type": tool_type,
            "toolkit_name": "release intelligence",
            "settings": settings
        }]
    })
    .as_object()
    .cloned()
    .expect("MCP version object")
}

fn settings(selected_tools: &[&str]) -> Value {
    json!({
        "url": "https://mcp.example.invalid/v1/mcp",
        "headers": null,
        "client_id": null,
        "client_secret": null,
        "scopes": null,
        "timeout": "12",
        "selected_tools": selected_tools,
        "enable_caching": true,
        "cache_ttl": "300",
        "ssl_verify": true
    })
}

fn prebuilt_settings() -> Value {
    json!({
        "server_name": "release_intelligence",
        "url": "https://mcp.example.invalid/v1/mcp",
        "headers": {
            "Authorization": "Static fixed-secret",
            "X-Platform": "fixed-value"
        },
        "timeout": 12,
        "selected_tools": ["lookup_release", "publish_release"],
        "excluded_tools": ["publish_release"],
        "enable_caching": true,
        "cache_ttl": 300,
        "ssl_verify": true
    })
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("mcp-test")
            .with_session_id("session-1")
            .with_function_call_id("call-1"),
    )
}

#[test]
fn discovered_oauth_metadata_is_bounded_to_the_browser_contract() {
    let metadata = serde_json::from_value::<AuthorizationMetadata>(json!({
        "authorization_endpoint": "https://login.example.invalid/oauth/authorize",
        "token_endpoint": "https://login.example.invalid/oauth/token",
        "registration_endpoint": "https://login.example.invalid/oauth/register",
        "issuer": "https://login.example.invalid",
        "jwks_uri": "https://login.example.invalid/oauth/keys",
        "scopes_supported": ["mcp:read", "offline_access"],
        "response_types_supported": ["code"],
        "code_challenge_methods_supported": ["S256"],
        "grant_types_supported": ["authorization_code", "refresh_token"],
        "revocation_endpoint": "https://login.example.invalid/oauth/revoke",
        "client_secret": "must-not-project",
        "unowned_provider_extension": {"value": true}
    }))
    .expect("authorization metadata fixture");

    let projected =
        authorization_resource_metadata(&metadata, "https://mcp.example.invalid/v1/mcp", &[])
            .expect("sanitized OAuth metadata");
    assert_eq!(
        projected["authorization_servers"],
        json!(["https://login.example.invalid/"])
    );
    assert_eq!(
        projected["oauth_authorization_server"]["authorization_endpoint"],
        "https://login.example.invalid/oauth/authorize"
    );
    assert_eq!(
        projected["oauth_authorization_server"]["registration_endpoint"],
        "https://login.example.invalid/oauth/register"
    );
    assert_eq!(
        projected["oauth_authorization_server"]["grant_types_supported"],
        json!(["authorization_code", "refresh_token"])
    );
    assert_eq!(
        projected["scopes_supported"],
        json!(["mcp:read", "offline_access"])
    );
    assert!(
        projected["oauth_authorization_server"]
            .get("client_secret")
            .is_none()
    );
    assert!(
        projected["oauth_authorization_server"]
            .get("unowned_provider_extension")
            .is_none()
    );
    let scoped = authorization_resource_metadata(
        &metadata,
        "https://mcp.example.invalid/v1/mcp",
        &["mcp:read".to_owned()],
    )
    .expect("configured consent scopes");
    assert_eq!(scoped["scopes_supported"], json!(["mcp:read"]));
    assert_eq!(
        scoped["oauth_authorization_server"],
        projected["oauth_authorization_server"]
    );
}

#[test]
fn legacy_oauth_metadata_uses_only_the_mcp_server_origin() {
    let metadata = serde_json::from_value::<AuthorizationMetadata>(json!({
        "authorization_endpoint": "https://mcp.example.invalid/authorize",
        "token_endpoint": "https://mcp.example.invalid/token",
        "registration_endpoint": "https://mcp.example.invalid/register"
    }))
    .expect("legacy authorization metadata fixture");

    let projected = authorization_resource_metadata(
        &metadata,
        "https://mcp.example.invalid/v1/mcp?tenant=hidden",
        &[],
    )
    .expect("legacy OAuth metadata");
    assert_eq!(
        projected["authorization_servers"],
        json!(["https://mcp.example.invalid"])
    );
    assert!(!projected.to_string().contains("tenant"));
}

struct FixtureConnector {
    calls: AtomicUsize,
    endpoints: Mutex<Vec<String>>,
    tools: Vec<Arc<dyn Tool>>,
}

impl FixtureConnector {
    fn new(tools: Vec<Arc<dyn Tool>>) -> Self {
        Self {
            calls: AtomicUsize::new(0),
            endpoints: Mutex::new(Vec::new()),
            tools,
        }
    }
}

#[async_trait]
impl McpConnector for FixtureConnector {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        self.calls.fetch_add(1, Ordering::AcqRel);
        self.endpoints
            .lock()
            .expect("fixture endpoints")
            .push(config.endpoint().to_owned());
        assert_eq!(config.timeout(), Duration::from_secs(12));
        Ok(Arc::new(BasicToolset::new(
            "fixture_mcp",
            self.tools.clone(),
        )))
    }
}

struct FixtureTool {
    name: &'static str,
    description: &'static str,
    read_only: bool,
    result: Value,
    calls: Arc<AtomicUsize>,
}

impl FixtureTool {
    fn new(name: &'static str, read_only: bool, result: Value) -> (Arc<Self>, Arc<AtomicUsize>) {
        let calls = Arc::new(AtomicUsize::new(0));
        (
            Arc::new(Self {
                name,
                description: "Look up release evidence for a concrete query.",
                read_only,
                result,
                calls: calls.clone(),
            }),
            calls,
        )
    }
}

#[async_trait]
impl Tool for FixtureTool {
    fn name(&self) -> &str {
        self.name
    }

    fn description(&self) -> &str {
        self.description
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {"query": {"type": "string", "maxLength": 1024}},
            "required": ["query"],
            "additionalProperties": false
        }))
    }

    fn is_read_only(&self) -> bool {
        self.read_only
    }

    fn is_concurrency_safe(&self) -> bool {
        self.read_only
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_rust::Result<Value> {
        self.calls.fetch_add(1, Ordering::AcqRel);
        Ok(self.result.clone())
    }
}

#[tokio::test]
async fn direct_https_mcp_discovers_filters_describes_and_executes_native_adk_tools() {
    let (lookup, lookup_calls) = FixtureTool::new("lookup_release", true, json!({"risk": "low"}));
    let (mutate, mutate_calls) = FixtureTool::new("publish_release", false, json!({"ok": true}));
    let connector = FixtureConnector::new(vec![lookup, mutate]);
    let version = frozen("mcp", &settings(&["LOOKUP_RELEASE"]));
    let snapshot = FrozenToolSnapshot::from_version_details(&version)
        .expect("MCP snapshot")
        .apply_policy(policy(&[]).as_ref());
    let toolsets = materialize_mcp_toolsets(&snapshot, &connector, &policy(&[]))
        .await
        .expect("materialized MCP toolset");

    assert_eq!(connector.calls.load(Ordering::Acquire), 1);
    assert_eq!(
        connector
            .endpoints
            .lock()
            .expect("fixture endpoints")
            .as_slice(),
        ["https://mcp.example.invalid/v1/mcp"]
    );
    assert_eq!(toolsets.len(), 1);
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolsets[0].tools(readonly).await.expect("MCP ADK tools");
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0].name(), "lookup_release");
    assert!(tools[0].description().contains("release intelligence"));
    assert!(
        tools[0]
            .description()
            .contains("marks this operation read-only")
    );
    assert!(!tools[0].description().contains("mcp.example.invalid"));
    assert!(tools[0].is_read_only());
    assert!(tools[0].is_concurrency_safe());

    let result = tools[0]
        .execute(context(), json!({"query": "1.2"}))
        .await
        .expect("MCP tool result");
    assert_eq!(result, json!({"risk": "low"}));
    assert_eq!(lookup_calls.load(Ordering::Acquire), 1);
    assert_eq!(mutate_calls.load(Ordering::Acquire), 0);
}

#[tokio::test]
async fn claim_materialized_prebuilt_http_uses_fixed_headers_and_exclusions() {
    let (lookup, _) = FixtureTool::new("lookup_release", true, json!({"risk": "low"}));
    let (publish, publish_calls) = FixtureTool::new("publish_release", false, json!({"ok": true}));
    let connector = PrebuiltConnector {
        expected_type: "mcp_config",
        expected_authorization: "Static fixed-secret",
        tools: vec![lookup, publish],
    };
    let version = frozen("mcp_config", &prebuilt_settings());
    let snapshot = FrozenToolSnapshot::from_version_details(&version)
        .expect("prebuilt MCP snapshot")
        .apply_policy(policy(&[]).as_ref());
    let toolsets = materialize_mcp_toolsets(&snapshot, &connector, &policy(&[]))
        .await
        .expect("prebuilt MCP toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolsets[0]
        .tools(readonly)
        .await
        .expect("prebuilt MCP tools");
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0].name(), "lookup_release");
    assert_eq!(publish_calls.load(Ordering::Acquire), 0);
}

/// #6691: a configured Authorization header (a PAT) wins over the delegated
/// OAuth token, matching Load Tools, so both act as the same identity.
#[tokio::test]
async fn prebuilt_configured_authorization_wins_over_alias_token() {
    let (lookup, _) = FixtureTool::new("lookup_release", true, json!({"risk": "low"}));
    let connector = PrebuiltConnector {
        expected_type: "mcp_release_intelligence",
        expected_authorization: "Static fixed-secret",
        tools: vec![lookup],
    };
    let mut settings = prebuilt_settings();
    settings["selected_tools"] = json!(["lookup_release"]);
    let version = frozen("mcp_release_intelligence", &settings);
    let snapshot = FrozenToolSnapshot::from_version_details(&version)
        .expect("prebuilt MCP snapshot")
        .apply_policy(policy(&[]).as_ref());
    let tokens = Map::from_iter([(
        "release_intelligence".to_owned(),
        json!({"access_token": "runtime-secret"}),
    )]);
    let toolsets =
        materialize_mcp_toolsets_with_tokens(&snapshot, &connector, &policy(&[]), &tokens)
            .await
            .expect("token-authorized prebuilt MCP toolset");
    assert_eq!(toolsets.len(), 1);
}

#[tokio::test]
async fn prebuilt_alias_token_applies_when_no_authorization_is_configured() {
    let (lookup, _) = FixtureTool::new("lookup_release", true, json!({"risk": "low"}));
    let connector = PrebuiltConnector {
        expected_type: "mcp_release_intelligence",
        expected_authorization: "Bearer runtime-secret",
        tools: vec![lookup],
    };
    let mut settings = prebuilt_settings();
    settings["selected_tools"] = json!(["lookup_release"]);
    settings["headers"] = json!({"X-Platform": "fixed-value"});
    let version = frozen("mcp_release_intelligence", &settings);
    let snapshot = FrozenToolSnapshot::from_version_details(&version)
        .expect("prebuilt MCP snapshot")
        .apply_policy(policy(&[]).as_ref());
    let tokens = Map::from_iter([(
        "release_intelligence".to_owned(),
        json!({"access_token": "runtime-secret"}),
    )]);
    materialize_mcp_toolsets_with_tokens(&snapshot, &connector, &policy(&[]), &tokens)
        .await
        .expect("token-authorized prebuilt MCP toolset");
}

#[tokio::test]
async fn prebuilt_authorization_preserves_dynamic_toolkit_type() {
    let version = frozen("mcp_release_intelligence", &prebuilt_settings());
    let snapshot = FrozenToolSnapshot::from_version_details(&version)
        .expect("prebuilt MCP snapshot")
        .apply_policy(policy(&[]).as_ref());
    let guarded = materialize_mcp_toolsets(&snapshot, &AuthorizationConnector, &policy(&[]))
        .await
        .expect("prebuilt authorization placeholder");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tool = guarded[0]
        .tools(readonly)
        .await
        .expect("guarded prebuilt tools")
        .pop()
        .expect("selected prebuilt placeholder");
    let error = tool
        .execute(context(), json!({}))
        .await
        .expect_err("prebuilt authorization required");
    let requirement =
        delegated_authorization_requirement(&error).expect("typed prebuilt authorization metadata");
    assert_eq!(requirement.toolkit_type(), "mcp_release_intelligence");
    assert_eq!(requirement.toolkit_name(), "release intelligence");
}

#[tokio::test]
async fn mcp_auth_challenge_materializes_selected_placeholder_and_exact_token_rebuild() {
    let mut configured = settings(&["lookup_release"]);
    configured["scopes"] = json!(["mcp:read", "offline_access"]);
    let version = frozen("mcp", &configured);
    let snapshot = FrozenToolSnapshot::from_version_details(&version)
        .expect("MCP snapshot")
        .apply_policy(policy(&[]).as_ref());
    let guarded = materialize_mcp_toolsets(&snapshot, &AuthorizationConnector, &policy(&[]))
        .await
        .expect("authorization placeholder");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tool = guarded[0]
        .tools(readonly)
        .await
        .expect("guarded tools")
        .pop()
        .expect("selected placeholder");
    let error = tool
        .execute(context(), json!({"private": "not-projected"}))
        .await
        .expect_err("authorization required");
    let requirement = delegated_authorization_requirement(&error)
        .unwrap_or_else(|| panic!("typed authorization metadata={:?}", error.details.metadata));
    assert_eq!(requirement.toolkit_name(), "release intelligence");
    assert_eq!(requirement.toolkit_type(), "mcp");
    assert_eq!(
        requirement.server_url(),
        "https://mcp.example.invalid/v1/mcp"
    );
    assert_eq!(
        requirement.resource_metadata_url(),
        Some("https://mcp.example.invalid/.well-known/oauth-protected-resource")
    );
    assert!(!format!("{error:?} {error}").contains("not-projected"));

    let (_, authorization) = materialize_mcp_toolsets_with_tokens_and_authorization(
        &snapshot,
        &AuthorizationConnector,
        &policy(&[]),
        &Map::new(),
    )
    .await
    .expect("authorization catalog");
    let guarded = authorization
        .requirement_for("lookup_release")
        .expect("selected guarded tool");
    assert_eq!(guarded.server_url(), requirement.server_url());

    let tokens = Map::from_iter([(
        "https://mcp.example.invalid/v1/mcp".to_owned(),
        json!({"access_token": "runtime-secret"}),
    )]);
    let rebuilt =
        materialize_mcp_toolsets_with_tokens(&snapshot, &TokenConnector, &policy(&[]), &tokens)
            .await
            .expect("token-bound rebuild");
    let (_, authorization) = materialize_mcp_toolsets_with_tokens_and_authorization(
        &snapshot,
        &TokenConnector,
        &policy(&[]),
        &tokens,
    )
    .await
    .expect("authorized catalog rebuild");
    assert!(authorization.requirement_for("lookup_release").is_none());
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert_eq!(
        rebuilt[0]
            .tools(readonly)
            .await
            .expect("rebuilt tools")
            .len(),
        1
    );

    let wrong_server = Map::from_iter([(
        "https://other.example.invalid/v1/mcp".to_owned(),
        json!({"access_token": "runtime-secret"}),
    )]);
    let guarded = materialize_mcp_toolsets_with_tokens(
        &snapshot,
        &AuthorizationConnector,
        &policy(&[]),
        &wrong_server,
    )
    .await
    .expect("wrong-server token must not be applied");
    assert_eq!(guarded.len(), 1);
}

#[tokio::test]
async fn malformed_scope_hints_and_inline_clients_fail_before_connecting() {
    let mut cases = Vec::new();
    for scopes in [
        json!("read"),
        json!({"read": true}),
        json!([false]),
        json!([""]),
        json!(["read write"]),
        json!(["read\nwrite"]),
        json!(["read\"write"]),
        json!(["read\\write"]),
        json!(vec!["read"; 65]),
        json!(["x".repeat(4097)]),
    ] {
        let mut value = settings(&[]);
        value["scopes"] = scopes;
        cases.push(value);
    }
    for field in ["client_id", "client_secret"] {
        let mut value = settings(&[]);
        value[field] = json!("unowned-client-value");
        cases.push(value);
    }
    for configured in cases {
        let version = frozen("mcp", &configured);
        let snapshot = FrozenToolSnapshot::from_version_details(&version)
            .expect("MCP snapshot")
            .apply_policy(policy(&[]).as_ref());
        let connector = FixtureConnector::new(Vec::new());
        assert!(
            materialize_mcp_toolsets(&snapshot, &connector, &policy(&[]))
                .await
                .is_err()
        );
        assert_eq!(connector.calls.load(Ordering::SeqCst), 0);
    }
}

#[tokio::test]
async fn invalid_or_unowned_mcp_authority_fails_before_connecting() {
    let cases = [
        ("mcp_config", settings(&[])),
        ("mcp_release_intelligence", settings(&[])),
        (
            "mcp",
            json!({
                "url": "http://mcp.example.invalid/mcp",
                "selected_tools": []
            }),
        ),
        (
            "mcp",
            json!({
                "url": "https://mcp.example.invalid/mcp",
                "headers": {"Authorization": "Bearer secret"},
                "selected_tools": []
            }),
        ),
        (
            "mcp",
            json!({
                "url": "https://mcp.example.invalid/mcp",
                "ssl_verify": false,
                "selected_tools": []
            }),
        ),
        (
            "mcp_config",
            json!({
                "server_name": "release_intelligence",
                "url": "https://mcp.example.invalid/mcp",
                "headers": {"Content-Type": "text/plain"},
                "selected_tools": []
            }),
        ),
        (
            "mcp_release_intelligence",
            json!({
                "server_name": "release_intelligence",
                "url": "https://mcp.example.invalid/{org_name}/mcp",
                "selected_tools": []
            }),
        ),
        (
            "mcp_release_intelligence",
            json!({
                "server_name": "release_intelligence",
                "url": "https://mcp.example.invalid/mcp",
                "headers": {"Authorization": "Bearer {api_token}"},
                "selected_tools": []
            }),
        ),
    ];
    for (tool_type, settings) in cases {
        let connector = FixtureConnector::new(Vec::new());
        let version = frozen(tool_type, &settings);
        let snapshot = FrozenToolSnapshot::from_version_details(&version)
            .expect("MCP snapshot")
            .apply_policy(policy(&[]).as_ref());
        let Err(error) = materialize_mcp_toolsets(&snapshot, &connector, &policy(&[])).await else {
            panic!("unowned MCP authority must fail");
        };
        assert!(matches!(
            error.code(),
            McpMaterializationErrorCode::InvalidConfiguration
                | McpMaterializationErrorCode::UnsupportedAuthority
        ));
        assert_eq!(connector.calls.load(Ordering::Acquire), 0);
        let diagnostics = format!("{error:?} {error}");
        assert!(!diagnostics.contains("secret"));
        assert!(!diagnostics.contains("mcp.example.invalid"));
    }
}

#[tokio::test]
async fn unknown_selection_and_blocked_tools_fail_or_filter_before_execution() {
    let (lookup, calls) = FixtureTool::new("lookup_release", true, json!({"risk": "low"}));
    let connector = FixtureConnector::new(vec![lookup]);
    let unknown = frozen("mcp", &settings(&["missing_tool"]));
    let snapshot = FrozenToolSnapshot::from_version_details(&unknown)
        .expect("MCP snapshot")
        .apply_policy(policy(&[]).as_ref());
    let Err(error) = materialize_mcp_toolsets(&snapshot, &connector, &policy(&[])).await else {
        panic!("unknown MCP selection must fail");
    };
    assert_eq!(
        error.code(),
        McpMaterializationErrorCode::InvalidConfiguration
    );

    let blocked = frozen("mcp", &settings(&[]));
    let snapshot = FrozenToolSnapshot::from_version_details(&blocked)
        .expect("MCP snapshot")
        .apply_policy(policy(&["lookup_release"]).as_ref());
    let toolsets = materialize_mcp_toolsets(&snapshot, &connector, &policy(&["lookup_release"]))
        .await
        .expect("blocked MCP toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert!(
        toolsets[0]
            .tools(readonly)
            .await
            .expect("blocked tools")
            .is_empty()
    );
    assert_eq!(calls.load(Ordering::Acquire), 0);
}

#[tokio::test]
async fn oversized_mcp_results_are_redacted_and_never_retried() {
    let secret = "provider-result-secret";
    let (tool, calls) = FixtureTool::new(
        "lookup_release",
        false,
        json!({"data": format!("{secret}{}", "x".repeat(512 * 1_024))}),
    );
    let connector = FixtureConnector::new(vec![tool]);
    let version = frozen("mcp", &settings(&[]));
    let snapshot = FrozenToolSnapshot::from_version_details(&version)
        .expect("MCP snapshot")
        .apply_policy(policy(&[]).as_ref());
    let toolsets = materialize_mcp_toolsets(&snapshot, &connector, &policy(&[]))
        .await
        .expect("MCP toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tool = toolsets[0]
        .tools(readonly)
        .await
        .expect("MCP tools")
        .pop()
        .expect("MCP fixture tool");
    let error = tool
        .execute(context(), json!({"query": "risk"}))
        .await
        .expect_err("oversized MCP result");

    assert_eq!(calls.load(Ordering::Acquire), 1);
    assert_eq!(error.component, ErrorComponent::Tool);
    assert_eq!(error.category, ErrorCategory::Internal);
    assert!(!error.is_retryable());
    assert!(!format!("{error:?} {error}").contains(secret));
}

// ---- #6691: tool-list cache honours enable_caching / cache_ttl ----

struct CountingToolset {
    lists: Arc<AtomicUsize>,
    tools: Vec<Arc<dyn Tool>>,
}

#[async_trait]
impl Toolset for CountingToolset {
    fn name(&self) -> &'static str {
        "counting_mcp"
    }

    async fn tools(&self, _ctx: Arc<dyn ReadonlyContext>) -> adk_rust::Result<Vec<Arc<dyn Tool>>> {
        self.lists.fetch_add(1, Ordering::AcqRel);
        Ok(self.tools.clone())
    }
}

type RecordedCalls = Arc<Mutex<Vec<(String, Map<String, Value>)>>>;

struct CountingCaller {
    calls: RecordedCalls,
}

#[async_trait]
impl McpToolCaller for CountingCaller {
    async fn call_tool(
        &self,
        name: &str,
        arguments: Map<String, Value>,
    ) -> adk_rust::Result<Value> {
        self.calls
            .lock()
            .expect("caller calls")
            .push((name.to_owned(), arguments));
        Ok(json!({"cached": true}))
    }
}

struct CachingConnector {
    lists: Arc<AtomicUsize>,
    calls: RecordedCalls,
    tools: Vec<Arc<dyn Tool>>,
}

impl CachingConnector {
    fn new() -> Self {
        let (lookup, _) = FixtureTool::new("lookup_release", true, json!({"risk": "low"}));
        Self {
            lists: Arc::new(AtomicUsize::new(0)),
            calls: Arc::new(Mutex::new(Vec::new())),
            tools: vec![lookup],
        }
    }

    fn lists(&self) -> usize {
        self.lists.load(Ordering::Acquire)
    }
}

#[async_trait]
impl McpConnector for CachingConnector {
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
        _config: &RemoteMcpConfig,
    ) -> Result<McpSession, McpMaterializationError> {
        Ok(McpSession {
            toolset: Arc::new(CountingToolset {
                lists: self.lists.clone(),
                tools: self.tools.clone(),
            }),
            caller: Some(Arc::new(CountingCaller {
                calls: self.calls.clone(),
            })),
        })
    }
}

fn caching_settings(endpoint: &str, enable_caching: bool, cache_ttl: u64) -> Value {
    let mut configured = settings(&["lookup_release"]);
    configured["url"] = json!(endpoint);
    configured["enable_caching"] = json!(enable_caching);
    configured["cache_ttl"] = json!(cache_ttl);
    configured
}

async fn materialize_once(
    connector: &CachingConnector,
    configured: &Value,
    tokens: &Map<String, Value>,
) -> Vec<Arc<dyn Tool>> {
    let version = frozen("mcp", configured);
    let snapshot = FrozenToolSnapshot::from_version_details(&version)
        .expect("MCP snapshot")
        .apply_policy(policy(&[]).as_ref());
    let toolsets = materialize_mcp_toolsets_with_tokens(&snapshot, connector, &policy(&[]), tokens)
        .await
        .expect("materialized MCP toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    toolsets[0].tools(readonly).await.expect("MCP tools")
}

#[tokio::test(start_paused = true)]
async fn tool_listing_is_reused_within_cache_ttl_and_refreshed_after_it() {
    let connector = CachingConnector::new();
    let configured = caching_settings("https://mcp.example.invalid/ttl/mcp", true, 120);

    let first = materialize_once(&connector, &configured, &Map::new()).await;
    assert_eq!(connector.lists(), 1);
    assert_eq!(first[0].name(), "lookup_release");

    tokio::time::advance(Duration::from_secs(119)).await;
    let second = materialize_once(&connector, &configured, &Map::new()).await;
    assert_eq!(
        connector.lists(),
        1,
        "a run inside the TTL issues no tools/list"
    );
    assert_eq!(second[0].name(), "lookup_release");
    assert!(
        second[0].is_read_only(),
        "cached descriptors keep annotations"
    );
    assert!(second[0].description().contains("release intelligence"));
    assert_eq!(second[0].parameters_schema(), first[0].parameters_schema());
    let result = second[0]
        .execute(context(), json!({"query": "1.2"}))
        .await
        .expect("cached tool executes through the current session");
    assert_eq!(result, json!({"cached": true}));
    let calls = connector.calls.lock().expect("calls").clone();
    assert_eq!(calls.len(), 1);
    assert_eq!(calls[0].0, "lookup_release");
    assert_eq!(calls[0].1.get("query"), Some(&json!("1.2")));

    tokio::time::advance(Duration::from_secs(2)).await;
    materialize_once(&connector, &configured, &Map::new()).await;
    assert_eq!(connector.lists(), 2, "a run after the TTL rediscovers");
}

#[tokio::test(start_paused = true)]
async fn disabled_caching_lists_on_every_run() {
    let connector = CachingConnector::new();
    let configured = caching_settings("https://mcp.example.invalid/nocache/mcp", false, 3_600);
    materialize_once(&connector, &configured, &Map::new()).await;
    materialize_once(&connector, &configured, &Map::new()).await;
    assert_eq!(connector.lists(), 2);
}

#[tokio::test(start_paused = true)]
async fn cached_listing_is_keyed_by_the_credential_sent() {
    let connector = CachingConnector::new();
    let endpoint = "https://mcp.example.invalid/keyed/mcp";
    let configured = caching_settings(endpoint, true, 3_600);
    let alice = Map::from_iter([(endpoint.to_owned(), json!({"access_token": "alice"}))]);
    let bob = Map::from_iter([(endpoint.to_owned(), json!({"access_token": "bob"}))]);
    materialize_once(&connector, &configured, &alice).await;
    materialize_once(&connector, &configured, &alice).await;
    assert_eq!(connector.lists(), 1);
    materialize_once(&connector, &configured, &bob).await;
    assert_eq!(
        connector.lists(),
        2,
        "another identity never reuses a listing"
    );
}

#[tokio::test]
async fn connectors_without_a_caller_never_serve_a_cached_listing() {
    let (lookup, _) = FixtureTool::new("lookup_release", true, json!({"risk": "low"}));
    let connector = FixtureConnector::new(vec![lookup]);
    let version = frozen(
        "mcp",
        &caching_settings("https://mcp.example.invalid/nocaller/mcp", true, 3_600),
    );
    let snapshot = FrozenToolSnapshot::from_version_details(&version)
        .expect("MCP snapshot")
        .apply_policy(policy(&[]).as_ref());
    for _ in 0..2 {
        let toolsets = materialize_mcp_toolsets(&snapshot, &connector, &policy(&[]))
            .await
            .expect("materialized");
        let readonly: Arc<dyn ReadonlyContext> = context();
        toolsets[0].tools(readonly).await.expect("tools");
    }
    assert_eq!(connector.calls.load(Ordering::Acquire), 2);
}

#[tokio::test]
async fn out_of_range_cache_ttl_is_rejected_before_connecting() {
    for ttl in [json!(59), json!(3_601), json!("soon")] {
        let connector = CachingConnector::new();
        let mut configured = caching_settings("https://mcp.example.invalid/range/mcp", true, 300);
        configured["cache_ttl"] = ttl;
        let version = frozen("mcp", &configured);
        let snapshot = FrozenToolSnapshot::from_version_details(&version)
            .expect("MCP snapshot")
            .apply_policy(policy(&[]).as_ref());
        let error = materialize_mcp_toolsets(&snapshot, &connector, &policy(&[]))
            .await
            .err()
            .expect("invalid TTL");
        assert_eq!(
            error.code(),
            McpMaterializationErrorCode::InvalidConfiguration
        );
        assert_eq!(connector.lists(), 0);
    }
}

// ---- #6688: same-origin redirects only, bounded ----

#[test]
fn redirect_decision_keeps_the_frozen_origin_and_bounds_hops() {
    let origin = reqwest_mcp::Url::parse("https://mcp.example.invalid/mcp").unwrap();
    let url = |value: &str| reqwest_mcp::Url::parse(value).unwrap();
    assert!(redirect_allowed(
        &origin,
        &url("https://mcp.example.invalid/mcp/"),
        1
    ));
    assert!(redirect_allowed(
        &origin,
        &url("https://mcp.example.invalid:443/x"),
        3
    ));
    assert!(!redirect_allowed(
        &origin,
        &url("https://mcp.example.invalid/mcp/"),
        4
    ));
    assert!(!redirect_allowed(
        &origin,
        &url("https://login.example.invalid/mcp"),
        1
    ));
    assert!(!redirect_allowed(
        &origin,
        &url("http://mcp.example.invalid/mcp"),
        1
    ));
    assert!(!redirect_allowed(
        &origin,
        &url("https://mcp.example.invalid:8443/mcp"),
        1
    ));
    assert!(!redirect_allowed(
        &origin,
        &url("https://user:pw@mcp.example.invalid/mcp"),
        1
    ));
}

/// A plain-HTTP server that redirects by path; the policy under test only
/// compares origins, so HTTP exercises the same wiring as HTTPS.
async fn redirect_server(other_origin: String) -> (String, Arc<Mutex<Vec<String>>>) {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base = format!("http://{}", listener.local_addr().unwrap());
    let seen = Arc::new(Mutex::new(Vec::new()));
    let log = seen.clone();
    tokio::spawn(async move {
        loop {
            let Ok((mut socket, _)) = listener.accept().await else {
                return;
            };
            let mut buffer = vec![0_u8; 8_192];
            let read = socket.read(&mut buffer).await.unwrap_or(0);
            let request = String::from_utf8_lossy(&buffer[..read]).to_string();
            let path = request.split_whitespace().nth(1).unwrap_or("/").to_owned();
            log.lock().unwrap().push(path.clone());
            let location = match path.as_str() {
                "/mcp" => Some("/mcp/".to_owned()),
                "/cross" => Some(format!("{other_origin}/stolen")),
                "/loop" => Some("/loop".to_owned()),
                _ => None,
            };
            let response = location.map_or_else(
                || "HTTP/1.1 200 OK\r\ncontent-length: 2\r\nconnection: close\r\n\r\nok".to_owned(),
                |location| {
                    format!(
                        "HTTP/1.1 307 Temporary Redirect\r\nlocation: {location}\r\ncontent-length: 0\r\nconnection: close\r\n\r\n"
                    )
                },
            );
            let _ = socket.write_all(response.as_bytes()).await;
        }
    });
    (base, seen)
}

#[tokio::test]
async fn same_origin_redirect_is_followed_and_cross_origin_is_refused() {
    let (other, other_seen) = redirect_server(String::new()).await;
    let (base, seen) = redirect_server(other.clone()).await;
    let client_for = |path: &str| {
        reqwest_mcp::Client::builder()
            .redirect(same_origin_redirect_policy(
                reqwest_mcp::Url::parse(&format!("{base}{path}")).unwrap(),
            ))
            .build()
            .unwrap()
    };

    let followed = client_for("/mcp")
        .post(format!("{base}/mcp"))
        .body("{}")
        .send()
        .await
        .unwrap();
    assert_eq!(followed.status(), 200);
    assert_eq!(followed.url().path(), "/mcp/");

    let refused = client_for("/cross")
        .post(format!("{base}/cross"))
        .body("{}")
        .send()
        .await
        .unwrap();
    assert_eq!(
        refused.status(),
        307,
        "the cross-origin hop is not followed"
    );
    assert!(other_seen.lock().unwrap().is_empty());

    let looped = client_for("/loop")
        .get(format!("{base}/loop"))
        .send()
        .await
        .unwrap();
    assert_eq!(looped.status(), 307, "hops stop after the bound");
    let loops = seen
        .lock()
        .unwrap()
        .iter()
        .filter(|path| *path == "/loop")
        .count();
    assert_eq!(loops, 4, "the original request plus three same-origin hops");
}

// ---- #6688: retired HTTP+SSE endpoints get an actionable error ----

#[derive(Debug)]
struct ChainError(&'static str, Option<Box<ChainError>>);

impl std::fmt::Display for ChainError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(self.0)
    }
}

impl std::error::Error for ChainError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        self.1
            .as_deref()
            .map(|error| error as &(dyn std::error::Error + 'static))
    }
}

#[test]
fn retired_sse_is_recognised_only_for_sse_urls_refusing_with_405_or_410() {
    let nested = |text: &'static str| {
        ChainError(
            "Transport error: send initialize request",
            Some(Box::new(ChainError(text, None))),
        )
    };
    for status in ["HTTP 405 Method Not Allowed: ", "HTTP 410 Gone: "] {
        let error = nested(status);
        assert!(retired_sse_failure_for_test(
            "https://mcp.deepwiki.com/sse",
            &error
        ));
        assert!(retired_sse_failure_for_test(
            "https://mcp.example.invalid/v1/sse/",
            &error
        ));
        assert!(!retired_sse_failure_for_test(
            "https://mcp.deepwiki.com/mcp",
            &error
        ));
    }
    let other = nested("HTTP 500 Internal Server Error: ");
    assert!(!retired_sse_failure_for_test(
        "https://mcp.deepwiki.com/sse",
        &other
    ));
    assert!(names_sse_endpoint("https://docs.mcp.cloudflare.com/SSE"));
    assert!(!names_sse_endpoint(
        "https://mcp.example.invalid/sse-tools/mcp"
    ));
    let display = McpMaterializationErrorCode::RetiredSseEndpoint;
    assert_ne!(display, McpMaterializationErrorCode::DependencyUnavailable);
    assert!(RETIRED_SSE_MESSAGE.contains("HTTP+SSE is not supported"));
    assert!(RETIRED_SSE_MESSAGE.contains("/mcp"));
}
