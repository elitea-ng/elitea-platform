use std::collections::BTreeMap;
use std::collections::HashSet;
use std::collections::VecDeque;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::session::{GetRequest, InMemorySessionService, SessionService};
use adk_rust::tool::BasicToolset;
use adk_rust::{Tool, ToolContext, Toolset};
use async_trait::async_trait;
use bytes::Bytes;
use http::{Request, Response, StatusCode, Version};
use http_body_util::Full;
use tonic::body::Body;

use super::assembly::OrdinaryNoToolProfile;
use super::assembly_tests::{current_text_history, ordinary_request};
use super::ordinary::OrdinaryNativeAgentAssembler;
use super::request::AgentExecutionKind;
use super::runtime::{
    AuthorizedNativeAssembly, NativeAgentAssembler, NativeAgentAssemblyErrorCode,
    NativeAgentCompletionSelector,
};
use super::session::{AuthorizedNativeCommandBinding, OrdinaryNativeAgentPlan};
use crate::protocol::control::test_runtime_context_authority;
use crate::protocol::node_event::encode_current_node_event_json;
use crate::toolkits::{
    McpConnector, McpMaterializationError, RemoteMcpConfig, ToolAdmissionPolicy,
    mcp_authorization_required_fixture,
};
use crate::transport::model_facade::ModelFacade;
use crate::transport::openai_compatible_facade::{
    CapturedModelRequest, TestModelGatewayOutcome, test_model_gateway_client,
    test_model_gateway_config, test_model_gateway_response,
};
use crate::transport::platform_client::PlatformClient;
use crate::transport::runtime_context::{
    RuntimeContextClient, RuntimeContextConfig, RuntimeContextRpc, RuntimeContextTransportError,
};

const TOKEN: &str = "ephemeral-ordinary-fixture-token";

fn empty_tool_policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("empty toolkit policy"))
}

fn sensitive_mcp_policy() -> Arc<ToolAdmissionPolicy> {
    let runtime = serde_json::json!({
        "toolkit_security": {
            "sensitive_tools": {"mcp": ["lookup_release"]},
            "sensitive_action_company_name": "Example Org"
        }
    });
    Arc::new(
        ToolAdmissionPolicy::from_runtime_config(
            runtime.as_object().expect("runtime policy object"),
        )
        .expect("sensitive MCP policy"),
    )
}

fn platform_client(runtime_context: RuntimeContextClient) -> Arc<PlatformClient> {
    Arc::new(PlatformClient::new(Arc::new(runtime_context)))
}

struct RuntimeContextFixture {
    responses: Mutex<VecDeque<Response<Body>>>,
    calls: Arc<AtomicUsize>,
    paths: Arc<Mutex<Vec<String>>>,
}

#[async_trait]
impl RuntimeContextRpc for RuntimeContextFixture {
    async fn post(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, RuntimeContextTransportError> {
        self.calls.fetch_add(1, Ordering::AcqRel);
        self.paths
            .lock()
            .map_err(|_| RuntimeContextTransportError::Unavailable)?
            .push(request.uri().path().to_owned());
        self.responses
            .lock()
            .map_err(|_| RuntimeContextTransportError::Unavailable)?
            .pop_front()
            .ok_or(RuntimeContextTransportError::Unavailable)
    }
}

fn runtime_context_client() -> (RuntimeContextClient, Arc<AtomicUsize>) {
    runtime_context_client_for_redemptions(1)
}

fn runtime_context_client_for_redemptions(
    count: usize,
) -> (RuntimeContextClient, Arc<AtomicUsize>) {
    let calls = Arc::new(AtomicUsize::new(0));
    let responses = (0..count)
        .map(|_| {
            runtime_context_response(&serde_json::json!({
                "schema_version": "elitea.runtime.elitea-client-token.v1",
                "project_id": 17,
                "token": TOKEN,
            }))
        })
        .collect();
    let paths = Arc::new(Mutex::new(Vec::new()));
    let client = runtime_context_client_from(responses, Arc::clone(&calls), paths);
    (client, calls)
}

fn runtime_context_response(value: &serde_json::Value) -> Response<Body> {
    let raw = value.to_string();
    Response::builder()
        .status(StatusCode::OK)
        .version(Version::HTTP_2)
        .header("content-type", "application/json")
        .header("cache-control", "private, no-cache, no-store")
        .header("pragma", "no-cache")
        .header("content-length", raw.len())
        .body(Body::new(Full::new(Bytes::from(raw))))
        .expect("runtime-context fixture response")
}

fn runtime_context_client_from(
    responses: VecDeque<Response<Body>>,
    calls: Arc<AtomicUsize>,
    paths: Arc<Mutex<Vec<String>>>,
) -> RuntimeContextClient {
    RuntimeContextClient::with_rpc(
        RuntimeContextFixture {
            responses: Mutex::new(responses),
            calls,
            paths,
        },
        RuntimeContextConfig {
            origin: "https://content.internal".to_owned(),
            deadline: Duration::from_secs(1),
            max_response_bytes: 32 * 1_024,
            max_application_response_bytes: 1_024 * 1_024,
            max_attachment_response_bytes: 1_024 * 1_024,
        },
    )
    .expect("runtime-context fixture client")
}

fn runtime_context_with_child() -> (
    RuntimeContextClient,
    Arc<AtomicUsize>,
    Arc<Mutex<Vec<String>>>,
) {
    let calls = Arc::new(AtomicUsize::new(0));
    let paths = Arc::new(Mutex::new(Vec::new()));
    let token = runtime_context_response(&serde_json::json!({
        "schema_version": "elitea.runtime.elitea-client-token.v1",
        "project_id": 17,
        "token": TOKEN,
    }));
    let child = application_version_response(
        31,
        41,
        serde_json::json!({
            "agent_type": "agent",
            "instructions": "Answer only the delegated task.",
            "meta": {},
            "variables": [],
            "tools": [],
            "llm_settings": {
                "model_name": "child-model",
                "model_project_id": 23,
                "max_tokens": 2048,
                "reasoning_effort": null,
                "temperature": 0.2,
                "openai_compatible": true
            }
        }),
    );
    let client = runtime_context_client_from(
        VecDeque::from([token, child]),
        Arc::clone(&calls),
        Arc::clone(&paths),
    );
    (client, calls, paths)
}

fn runtime_context_with_sensitive_child() -> (RuntimeContextClient, Arc<AtomicUsize>) {
    let calls = Arc::new(AtomicUsize::new(0));
    let paths = Arc::new(Mutex::new(Vec::new()));
    let responses = (0..3)
        .flat_map(|_| {
            [
                runtime_context_response(&serde_json::json!({
                    "schema_version": "elitea.runtime.elitea-client-token.v1",
                    "project_id": 17,
                    "token": TOKEN,
                })),
                application_version_response(
                    31,
                    41,
                    nested_agent_version(
                        "Resolve only the delegated name.",
                        "child-model",
                        23,
                        vec![remote_mcp_tool()],
                    ),
                ),
            ]
        })
        .collect();
    let client = runtime_context_client_from(responses, Arc::clone(&calls), paths);
    (client, calls)
}

fn runtime_context_with_ask_user_child() -> (RuntimeContextClient, Arc<AtomicUsize>) {
    let calls = Arc::new(AtomicUsize::new(0));
    let paths = Arc::new(Mutex::new(Vec::new()));
    let responses = (0..3)
        .flat_map(|_| {
            let mut child = nested_agent_version(
                "Resolve only the delegated name.",
                "child-model",
                23,
                vec![],
            );
            child["meta"] = serde_json::json!({"internal_tools": ["ask_user"]});
            [
                runtime_context_response(&serde_json::json!({
                    "schema_version": "elitea.runtime.elitea-client-token.v1",
                    "project_id": 17,
                    "token": TOKEN,
                })),
                application_version_response(31, 41, child),
            ]
        })
        .collect();
    let client = runtime_context_client_from(responses, Arc::clone(&calls), paths);
    (client, calls)
}

fn runtime_context_with_recursive_sensitive_child(
    cycles: usize,
) -> (RuntimeContextClient, Arc<AtomicUsize>) {
    let calls = Arc::new(AtomicUsize::new(0));
    let paths = Arc::new(Mutex::new(Vec::new()));
    let child_reference = stored_application_reference(45, 32, 42, "name-resolver");
    let responses = (0..cycles)
        .flat_map(|_| {
            [
                runtime_context_response(&serde_json::json!({
                    "schema_version": "elitea.runtime.elitea-client-token.v1",
                    "project_id": 17,
                    "token": TOKEN,
                })),
                application_version_response(
                    31,
                    41,
                    nested_agent_version(
                        "Delegate name resolution.",
                        "orchestrator-model",
                        23,
                        vec![child_reference.clone()],
                    ),
                ),
                application_version_response(
                    32,
                    42,
                    nested_agent_version(
                        "Resolve the delegated name.",
                        "resolver-model",
                        24,
                        vec![remote_mcp_tool()],
                    ),
                ),
            ]
        })
        .collect();
    let client = runtime_context_client_from(responses, Arc::clone(&calls), paths);
    (client, calls)
}

fn application_version_response(
    application_id: u64,
    version_id: u64,
    version_details: serde_json::Value,
) -> Response<Body> {
    let mut response = serde_json::json!({
        "schema_version": "elitea.runtime.application-version.v1",
        "project_id": 17,
        "application_id": application_id,
        "version_id": version_id,
        "version_details": null
    });
    response["version_details"] = version_details;
    runtime_context_response(&response)
}

fn model_response() -> Response<Body> {
    let raw = concat!(
        "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"native \"},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{\"content\":\"response\"},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
        "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n",
        "data: [DONE]\n\n",
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn tool_call_response() -> Response<Body> {
    let raw = concat!(
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_branch\",\"type\":\"function\",\"function\":{\"name\":\"set_active_branch\",\"arguments\":\"{\\\"branch\\\":\\\"release/1.2\\\"}\"}}]},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
        "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n",
        "data: [DONE]\n\n",
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn mcp_tool_call_response() -> Response<Body> {
    let raw = concat!(
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_mcp\",\"type\":\"function\",\"function\":{\"name\":\"lookup_release\",\"arguments\":\"{\\\"release\\\":\\\"1.2\\\"}\"}}]},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
        "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n",
        "data: [DONE]\n\n",
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

pub(super) fn mcp_tool_batch_response(count: usize) -> Response<Body> {
    named_mcp_batch_response("lookup_release", count, "call_operation")
}

pub(super) fn mcp_authorization_tool_name() -> String {
    crate::toolkits::DelegatedAuthorizationRequirement::new(
        "release intelligence".to_owned(),
        "mcp".to_owned(),
        "https://mcp.example.invalid/v1/mcp".to_owned(),
        None,
        None,
    )
    .expect("fixture authorization identity")
    .authorization_tool_name()
}

pub(super) fn mcp_authorization_response() -> Response<Body> {
    named_mcp_batch_response(&mcp_authorization_tool_name(), 1, "call_mcp")
}

fn named_mcp_batch_response(name: &str, count: usize, prefix: &str) -> Response<Body> {
    let calls: Vec<_> = (0..count).map(|index| serde_json::json!({
        "index": index,
        "id": if index == 0 { prefix.to_owned() } else { format!("{prefix}_{index}") },
        "type": "function",
        "function": {"name": if name == "lookup_release" && index % 2 == 1 { "inspect_release" } else { name }, "arguments": if name.starts_with("mcp_authorize_") { "{}".to_owned() } else { format!("{{\"release\":\"1.{index}\"}}") }}
    })).collect();
    let frame =
        serde_json::json!({"choices":[{"delta":{"tool_calls":calls},"finish_reason":null}]});
    let raw = format!(
        "data: {frame}\n\ndata: {{\"choices\":[{{\"delta\":{{}},\"finish_reason\":\"tool_calls\"}}]}}\n\ndata: [DONE]\n\n"
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn colliding_mcp_tool_call_response() -> Response<Body> {
    let raw = concat!(
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_release\",\"type\":\"function\",\"function\":{\"name\":\"release_intelligence__lookup_release\",\"arguments\":\"{\\\"release\\\":\\\"1.2\\\"}\"}},{\"index\":1,\"id\":\"call_audit\",\"type\":\"function\",\"function\":{\"name\":\"audit_intelligence__lookup_release\",\"arguments\":\"{\\\"release\\\":\\\"1.2\\\"}\"}}]},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
        "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n",
        "data: [DONE]\n\n",
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn ask_user_tool_call_response() -> Response<Body> {
    let arguments = serde_json::json!({
        "questions": [{
            "question": "Which full name should I resolve?",
            "header": "Name",
            "options": [
                {"label": "Olivia Lovelace"},
                {"label": "Sasha Grey"}
            ],
            "allow_other": true
        }]
    })
    .to_string();
    let chunk = serde_json::json!({
        "choices": [{
            "delta": {
                "tool_calls": [{
                    "index": 0,
                    "id": "call_ask_user",
                    "type": "function",
                    "function": {"name": "ask_user", "arguments": arguments}
                }]
            },
            "finish_reason": null
        }]
    });
    let raw = format!(
        "data: {chunk}\n\ndata: {{\"choices\":[{{\"delta\":{{}},\"finish_reason\":\"tool_calls\"}}]}}\n\ndata: {{\"choices\":[],\"usage\":{{\"prompt_tokens\":3,\"completion_tokens\":2}}}}\n\ndata: [DONE]\n\n"
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn nested_agent_call_response() -> Response<Body> {
    let raw = concat!(
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_child\",\"type\":\"function\",\"function\":{\"name\":\"elitea_agent_31_v_41\",\"arguments\":\"{\\\"task\\\":\\\"Summarize release risk\\\"}\"}}]},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
        "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n",
        "data: [DONE]\n\n",
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn saved_agent_call_response(call_id: &str, tool_name: &str, task: &str) -> Response<Body> {
    let arguments = serde_json::json!({"task": task}).to_string();
    let chunk = serde_json::json!({
        "choices": [{
            "delta": {
                "tool_calls": [{
                    "index": 0,
                    "id": call_id,
                    "type": "function",
                    "function": {"name": tool_name, "arguments": arguments}
                }]
            },
            "finish_reason": null
        }]
    });
    let raw = format!(
        "data: {chunk}\n\ndata: {{\"choices\":[{{\"delta\":{{}},\"finish_reason\":\"tool_calls\"}}]}}\n\ndata: {{\"choices\":[],\"usage\":{{\"prompt_tokens\":3,\"completion_tokens\":2}}}}\n\ndata: [DONE]\n\n"
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn parallel_nested_agent_call_response() -> Response<Body> {
    let raw = concat!(
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_olivia\",\"type\":\"function\",\"function\":{\"name\":\"elitea_agent_31_v_41\",\"arguments\":\"{\\\"task\\\":\\\"Resolve Olivia Lovelace\\\"}\"}},{\"index\":1,\"id\":\"call_sasha\",\"type\":\"function\",\"function\":{\"name\":\"elitea_agent_31_v_41\",\"arguments\":\"{\\\"task\\\":\\\"Resolve Sasha Grey\\\"}\"}}]},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
        "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n",
        "data: [DONE]\n\n",
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn parallel_resolver_call_response() -> Response<Body> {
    let raw = concat!(
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_first\",\"type\":\"function\",\"function\":{\"name\":\"elitea_agent_32_v_42\",\"arguments\":\"{\\\"task\\\":\\\"Resolve first name\\\"}\"}},{\"index\":1,\"id\":\"call_last\",\"type\":\"function\",\"function\":{\"name\":\"elitea_agent_32_v_42\",\"arguments\":\"{\\\"task\\\":\\\"Resolve last name\\\"}\"}}]},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
        "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n",
        "data: [DONE]\n\n",
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn text_response(text: &str) -> Response<Body> {
    // Exercise real delta collection, not a single chunk that masks truncation.
    use std::fmt::Write as _;
    let mut raw = String::new();
    for chunk in text.chars() {
        write!(
            &mut raw,
            "data: {{\"choices\":[{{\"delta\":{{\"content\":{:?}}},\"finish_reason\":null}}]}}\n\n",
            chunk.to_string()
        )
        .expect("SSE fixture");
    }
    raw.push_str("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n");
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn model_response_for(model: &'static str, response: Response<Body>) -> TestModelGatewayOutcome {
    TestModelGatewayOutcome::ResponseForModel { model, response }
}

fn recursive_sensitive_outcomes() -> Vec<TestModelGatewayOutcome> {
    vec![
        TestModelGatewayOutcome::Response(saved_agent_call_response(
            "call_orchestrator",
            "elitea_agent_31_v_41",
            "Resolve Olivia Lovelace",
        )),
        TestModelGatewayOutcome::Response(saved_agent_call_response(
            "call_resolver",
            "elitea_agent_32_v_42",
            "Resolve Olivia Lovelace",
        )),
        TestModelGatewayOutcome::Response(mcp_tool_call_response()),
        TestModelGatewayOutcome::Response(text_response("resolved leaf")),
        TestModelGatewayOutcome::Response(text_response("resolved orchestrator")),
        TestModelGatewayOutcome::Response(text_response("root recursive answer")),
    ]
}

fn attach_local_gitlab_tool(request: &mut super::request::AgentExecutionRequest) {
    let tool = serde_json::json!({
        "id": 91,
        "type": "gitlab_org",
        "toolkit_name": "release_repository",
        "settings": {
            "gitlab_configuration": {
                "url": "https://gitlab.example.invalid",
                "private_token": "claim-materialized-token"
            },
            "repositories": "group/project",
            "branch": "main",
            "selected_tools": ["set_active_branch"]
        }
    });
    match request.kind {
        AgentExecutionKind::Application => request
            .payload
            .application
            .get_mut("version_details")
            .and_then(serde_json::Value::as_object_mut)
            .expect("application version")
            .insert("tools".to_owned(), serde_json::json!([tool])),
        AgentExecutionKind::Adhoc => {
            request.payload.tools.push(tool);
            None
        }
    };
}

fn remote_mcp_tool() -> serde_json::Value {
    serde_json::json!({
        "id": 92,
        "type": "mcp",
        "toolkit_name": "release intelligence",
        "settings": {
            "url": "https://mcp.example.invalid/v1/mcp",
            "headers": null,
            "client_id": null,
            "client_secret": null,
            "scopes": null,
            "timeout": 30,
            "selected_tools": ["lookup_release"],
            "enable_caching": true,
            "cache_ttl": 300,
            "ssl_verify": true
        }
    })
}

fn attach_remote_mcp_tool(request: &mut super::request::AgentExecutionRequest) {
    let tool = remote_mcp_tool();
    match request.kind {
        AgentExecutionKind::Application => request
            .payload
            .application
            .get_mut("version_details")
            .and_then(serde_json::Value::as_object_mut)
            .expect("application version")
            .insert("tools".to_owned(), serde_json::json!([tool])),
        AgentExecutionKind::Adhoc => {
            request.payload.tools.push(tool);
            None
        }
    };
}

fn attach_colliding_remote_mcp_tools(request: &mut super::request::AgentExecutionRequest) {
    let tools = serde_json::json!([
        {
            "id": 92,
            "type": "mcp",
            "toolkit_name": "release intelligence",
            "settings": {
                "url": "https://release-mcp.example.invalid/v1/mcp",
                "timeout": 30,
                "selected_tools": ["lookup_release"],
                "enable_caching": true,
                "cache_ttl": 300,
                "ssl_verify": true
            }
        },
        {
            "id": 93,
            "type": "mcp",
            "toolkit_name": "audit intelligence",
            "settings": {
                "url": "https://audit-mcp.example.invalid/v1/mcp",
                "timeout": 30,
                "selected_tools": ["lookup_release"],
                "enable_caching": true,
                "cache_ttl": 300,
                "ssl_verify": true
            }
        }
    ]);
    match request.kind {
        AgentExecutionKind::Application => {
            request
                .payload
                .application
                .get_mut("version_details")
                .and_then(serde_json::Value::as_object_mut)
                .expect("application version")
                .insert("tools".to_owned(), tools);
        }
        AgentExecutionKind::Adhoc => {
            request.payload.tools = tools.as_array().expect("MCP tool array").clone();
        }
    }
}

struct AgentMcpConnector {
    calls: AtomicUsize,
    tool_calls: Arc<AtomicUsize>,
}

struct CollidingMcpConnector {
    connections: AtomicUsize,
    tool_calls: Arc<Mutex<Vec<String>>>,
}

#[async_trait]
impl McpConnector for CollidingMcpConnector {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        self.connections.fetch_add(1, Ordering::AcqRel);
        let source = if config.endpoint().contains("release-mcp") {
            "release intelligence"
        } else if config.endpoint().contains("audit-mcp") {
            "audit intelligence"
        } else {
            panic!("unexpected MCP fixture endpoint")
        };
        Ok(Arc::new(BasicToolset::new(
            "discovered_fixture",
            vec![Arc::new(SourcedMcpTool {
                source,
                calls: Arc::clone(&self.tool_calls),
            })],
        )))
    }
}

struct SourcedMcpTool {
    source: &'static str,
    calls: Arc<Mutex<Vec<String>>>,
}

#[async_trait]
impl Tool for SourcedMcpTool {
    fn name(&self) -> &'static str {
        "lookup_release"
    }

    fn description(&self) -> &'static str {
        "Read release evidence from one exact MCP toolkit."
    }

    fn is_read_only(&self) -> bool {
        true
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: serde_json::Value,
    ) -> adk_rust::Result<serde_json::Value> {
        self.calls
            .lock()
            .expect("tool call fixture lock")
            .push(self.source.to_owned());
        Ok(serde_json::json!({
            "release": arguments["release"],
            "source": self.source
        }))
    }
}

struct AgentDelegatedAuthorizationMcpConnector {
    calls: AtomicUsize,
    tool_calls: Arc<AtomicUsize>,
}

#[async_trait]
impl McpConnector for AgentDelegatedAuthorizationMcpConnector {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        self.calls.fetch_add(1, Ordering::AcqRel);
        if config.access_token_for_test() != Some("runtime-secret") {
            return Err(mcp_authorization_required_fixture(
                config,
                "Bearer resource_metadata=\"https://mcp.example.invalid/.well-known/oauth-protected-resource\"",
            ));
        }
        Ok(Arc::new(BasicToolset::new(
            "authorized_fixture_mcp",
            vec![
                Arc::new(AgentMcpTool {
                    name: "lookup_release",
                    calls: Arc::clone(&self.tool_calls),
                }),
                Arc::new(AgentMcpTool {
                    name: "inspect_release",
                    calls: Arc::clone(&self.tool_calls),
                }),
            ],
        )))
    }
}

#[async_trait]
impl McpConnector for AgentMcpConnector {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        self.calls.fetch_add(1, Ordering::AcqRel);
        assert_eq!(config.endpoint(), "https://mcp.example.invalid/v1/mcp");
        Ok(Arc::new(BasicToolset::new(
            "fixture_mcp",
            vec![Arc::new(AgentMcpTool {
                name: "lookup_release",
                calls: self.tool_calls.clone(),
            })],
        )))
    }
}

struct AgentMcpTool {
    name: &'static str,
    calls: Arc<AtomicUsize>,
}

#[async_trait]
impl Tool for AgentMcpTool {
    fn name(&self) -> &'static str {
        self.name
    }

    fn description(&self) -> &'static str {
        "Read release evidence for one release identifier."
    }

    fn parameters_schema(&self) -> Option<serde_json::Value> {
        Some(serde_json::json!({
            "type": "object",
            "properties": {"release": {"type": "string"}},
            "required": ["release"],
            "additionalProperties": false
        }))
    }

    fn is_read_only(&self) -> bool {
        true
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: serde_json::Value,
    ) -> adk_rust::Result<serde_json::Value> {
        self.calls.fetch_add(1, Ordering::AcqRel);
        Ok(serde_json::json!({
            "release": arguments["release"],
            "risk": "low"
        }))
    }
}

struct RecursiveSensitiveHarness {
    assembler: OrdinaryNativeAgentAssembler,
    sessions: Arc<InMemorySessionService>,
    tool_calls: Arc<AtomicUsize>,
    context_calls: Arc<AtomicUsize>,
    connector: Arc<AgentMcpConnector>,
    captured: Arc<Mutex<Vec<CapturedModelRequest>>>,
}

fn recursive_sensitive_harness(
    cycles: usize,
    outcomes: Vec<TestModelGatewayOutcome>,
) -> RecursiveSensitiveHarness {
    let (runtime_context, context_calls) = runtime_context_with_recursive_sensitive_child(cycles);
    let (model_gateway, captured) =
        test_model_gateway_client(outcomes, test_model_gateway_config())
            .expect("recursive nested model gateway");
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let connector = Arc::new(AgentMcpConnector {
        calls: AtomicUsize::new(0),
        tool_calls: Arc::clone(&tool_calls),
    });
    let sessions = Arc::new(InMemorySessionService::new());
    let injected_sessions: Arc<dyn SessionService> = sessions.clone();
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(runtime_context),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        sensitive_mcp_policy(),
    )
    .with_mcp_connector(connector.clone())
    .with_sessions(injected_sessions);
    RecursiveSensitiveHarness {
        assembler,
        sessions,
        tool_calls,
        context_calls,
        connector,
        captured,
    }
}

fn stored_application_reference(
    tool_id: u64,
    application_id: u64,
    version_id: u64,
    name: &str,
) -> serde_json::Value {
    serde_json::json!({
        "id": tool_id,
        "type": "application",
        "name": name,
        "description": "Evaluates release readiness and summarizes concrete risks.",
        "author_id": 11,
        "settings": {
            "application_id": application_id,
            "application_version_id": version_id
        },
        "meta": {},
        "created_at": "2026-08-19T10:00:00Z",
        "toolkit_name": name,
        "author": null,
        "agent_type": "agent",
        "online": null,
        "icon_meta": null,
        "variables": [],
        "is_pinned": false,
        "indexes_count": null
    })
}

fn adhoc_application_reference(
    application_id: u64,
    version_id: u64,
    project_id: u64,
) -> serde_json::Value {
    serde_json::json!({
        "id": null,
        "type": "application",
        "name": "cross-project-agent",
        "description": "A participant-bound saved agent.",
        "author_id": 11,
        "settings": {
            "application_id": application_id,
            "application_version_id": version_id,
            "variables": [],
            "selected_tools": []
        },
        "created_at": "2026-08-19T10:00:00Z",
        "toolkit_name": "cross-project-agent",
        "agent_type": "agent",
        "participant_id": 71,
        "project_id": project_id
    })
}

fn attach_application_tools(
    request: &mut super::request::AgentExecutionRequest,
    tools: Vec<serde_json::Value>,
) {
    request
        .payload
        .application
        .get_mut("version_details")
        .and_then(serde_json::Value::as_object_mut)
        .expect("application version")
        .insert("tools".to_owned(), serde_json::Value::Array(tools));
}

fn attach_nested_agent(request: &mut super::request::AgentExecutionRequest) {
    attach_application_tools(
        request,
        vec![stored_application_reference(
            44,
            31,
            41,
            "release-risk-agent",
        )],
    );
}

fn nested_agent_version(
    instructions: &str,
    model_name: &str,
    model_project_id: u64,
    tools: Vec<serde_json::Value>,
) -> serde_json::Value {
    let mut version = serde_json::json!({
        "agent_type": "agent",
        "instructions": instructions,
        "meta": {},
        "variables": [],
        "tools": [],
        "llm_settings": {
            "model_name": model_name,
            "model_project_id": model_project_id,
            "max_tokens": 2048,
            "reasoning_effort": null,
            "temperature": 0.2,
            "openai_compatible": true
        }
    });
    version["tools"] = serde_json::Value::Array(tools);
    version
}

fn assert_nested_agent_model_requests(captured: &[CapturedModelRequest]) {
    assert_eq!(captured.len(), 3);
    assert_eq!(captured[0].headers["x-project-id"], "17");
    assert_eq!(captured[1].headers["x-project-id"], "17");
    assert_eq!(captured[2].headers["x-project-id"], "17");
    let root_first: serde_json::Value =
        serde_json::from_slice(&captured[0].body).expect("root tool request");
    assert_eq!(
        root_first["tools"][0]["function"]["name"],
        "elitea_agent_31_v_41"
    );
    let declaration = &root_first["tools"][0]["function"];
    assert!(
        declaration["description"]
            .as_str()
            .is_some_and(|text| text.contains("release readiness"))
    );
    assert_eq!(
        declaration["parameters"]["required"],
        serde_json::json!(["task"])
    );
    let child: serde_json::Value =
        serde_json::from_slice(&captured[1].body).expect("child model request");
    assert_eq!(child["model"], "child-model");
    assert_eq!(
        child["messages"][0]["content"],
        "Answer only the delegated task."
    );
    assert_eq!(child["messages"][1]["content"], "Summarize release risk");
    let root_second: serde_json::Value =
        serde_json::from_slice(&captured[2].body).expect("root final request");
    assert_eq!(root_second["messages"][3]["role"], "tool");
    assert_eq!(root_second["messages"][3]["tool_call_id"], "call_child");
    assert!(
        root_second["messages"][3]["content"]
            .as_str()
            .is_some_and(|text| text.contains("child release risk"))
    );
}

fn assert_streamed_child_model_event(public_events: &[serde_json::Value]) {
    let child_model = public_events
        .iter()
        .find(|event| {
            event["type"] == "agent_llm_start"
                && event["response_metadata"]["parent_agent_path"]
                    .as_array()
                    .is_some_and(|path| !path.is_empty())
        })
        .expect("streamed child model event");
    assert_eq!(
        child_model["response_metadata"]["parent_agent_path"],
        serde_json::json!([{
            "name": "release-risk-agent",
            "call_id": "call_child",
            "sibling_ordinal": 1,
        }])
    );
    assert_eq!(
        child_model["response_metadata"]["parent_agent_call_id"],
        "call_child"
    );
}

fn anthropic_response() -> Response<Body> {
    let raw = concat!(
        "event: message_start\n",
        "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_ordinary\",\"content\":[],\"model\":\"claude-sonnet-4-5\",\"role\":\"assistant\",\"type\":\"message\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n",
        "event: content_block_start\n",
        "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
        "event: content_block_delta\n",
        "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"native Anthropic response\"}}\n\n",
        "event: content_block_stop\n",
        "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
        "event: message_delta\n",
        "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":3}}\n\n",
        "event: message_stop\n",
        "data: {\"type\":\"message_stop\"}\n\n",
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn use_native_anthropic(request: &mut super::request::AgentExecutionRequest) {
    match request.kind {
        AgentExecutionKind::Application => {
            request
                .payload
                .llm
                .get_mut("kwargs")
                .and_then(serde_json::Value::as_object_mut)
                .expect("application runtime model")
                .insert("openai_compatible".to_owned(), serde_json::json!(false));
            let settings = request
                .payload
                .application
                .get_mut("version_details")
                .and_then(serde_json::Value::as_object_mut)
                .and_then(|version| version.get_mut("llm_settings"))
                .and_then(serde_json::Value::as_object_mut)
                .expect("application model settings");
            settings.insert(
                "model_name".to_owned(),
                serde_json::json!("claude-sonnet-4-5"),
            );
            settings.insert("openai_compatible".to_owned(), serde_json::json!(false));
        }
        AgentExecutionKind::Adhoc => {
            let settings = request
                .payload
                .llm
                .get_mut("kwargs")
                .and_then(serde_json::Value::as_object_mut)
                .expect("ad-hoc model settings");
            settings.insert("model".to_owned(), serde_json::json!("claude-sonnet-4-5"));
            settings.insert("openai_compatible".to_owned(), serde_json::json!(false));
        }
    }
}

#[tokio::test(flavor = "current_thread")]
async fn application_and_adhoc_share_authorized_redemption_model_session_and_projection() {
    for kind in [AgentExecutionKind::Application, AgentExecutionKind::Adhoc] {
        let mut request = ordinary_request(kind);
        request.payload.chat_history = current_text_history();
        let (runtime_context, context_calls) = runtime_context_client();
        let (model_gateway, captured) = test_model_gateway_client(
            vec![TestModelGatewayOutcome::Response(model_response())],
            test_model_gateway_config(),
        )
        .expect("model gateway fixture client");
        let assembler = OrdinaryNativeAgentAssembler::new(
            platform_client(runtime_context),
            Arc::new(ModelFacade::from_gateway(model_gateway)),
            empty_tool_policy(),
        );
        let assembly = AuthorizedNativeAssembly::new(
            &request,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        );
        let mut invocation = assembler
            .assemble(assembly)
            .await
            .expect("authorized ordinary assembly");
        assert_eq!(context_calls.load(Ordering::Acquire), 1);
        assert_eq!(
            invocation.project_start(chrono::Utc::now()).unwrap().len(),
            1
        );

        let (mut native, mut projector, completion) = invocation.start().expect("native start");
        while let Some(event) = native.next_event().await.expect("native event") {
            let _batch = projector.project(&event).expect("projected native event");
        }
        let completed = completion.select().await.expect("selected completion");
        let finish = projector
            .finish_after_eos(completed, chrono::Utc::now())
            .expect("finished browser output");
        let full_message = finish
            .into_iter()
            .map(|event| {
                serde_json::from_slice::<serde_json::Value>(
                    &encode_current_node_event_json(&event).expect("canonical browser event"),
                )
                .expect("browser event JSON")
            })
            .find(|event| event["type"] == "full_message")
            .expect("full message event");
        assert_eq!(full_message["content"], "native response");

        let captured = captured.lock().expect("captured model request");
        assert_eq!(captured.len(), 1);
        let body: serde_json::Value =
            serde_json::from_slice(&captured[0].body).expect("model request JSON");
        assert_eq!(body["messages"][1]["role"], "user");
        assert_eq!(
            body["messages"][1]["content"][0]["text"],
            "earlier question"
        );
        assert_eq!(body["messages"][2]["role"], "assistant");
        assert_eq!(body["messages"][2]["content"][0]["text"], "earlier ");
        assert_eq!(body["messages"][2]["content"][1]["text"], "answer");
        assert_eq!(body["messages"][3]["content"], "current");
        assert_eq!(
            body["messages"][0]["content"],
            match kind {
                AgentExecutionKind::Application => "review carefully",
                AgentExecutionKind::Adhoc => "be concise",
            }
        );
        assert_eq!(captured[0].headers["x-project-id"], "17");
        assert!(captured[0].headers["authorization"].is_sensitive());
    }
}

#[tokio::test(flavor = "current_thread")]
async fn application_and_adhoc_select_native_anthropic_without_changing_the_lifecycle() {
    for kind in [AgentExecutionKind::Application, AgentExecutionKind::Adhoc] {
        let mut request = ordinary_request(kind);
        request.payload.chat_history = current_text_history();
        use_native_anthropic(&mut request);
        let (runtime_context, context_calls) = runtime_context_client();
        let (model_gateway, captured) = test_model_gateway_client(
            vec![TestModelGatewayOutcome::Response(anthropic_response())],
            test_model_gateway_config(),
        )
        .expect("model gateway fixture client");
        let assembler = OrdinaryNativeAgentAssembler::new(
            platform_client(runtime_context),
            Arc::new(ModelFacade::from_gateway(model_gateway)),
            empty_tool_policy(),
        );
        let assembly = AuthorizedNativeAssembly::new(
            &request,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        );
        let mut invocation = assembler
            .assemble(assembly)
            .await
            .expect("authorized native Anthropic assembly");
        assert_eq!(context_calls.load(Ordering::Acquire), 1);
        assert_eq!(
            invocation.project_start(chrono::Utc::now()).unwrap().len(),
            1
        );

        let (mut native, mut projector, completion) = invocation.start().expect("native start");
        while let Some(event) = native.next_event().await.expect("native event") {
            let _batch = projector.project(&event).expect("projected native event");
        }
        let completed = completion.select().await.expect("selected completion");
        let finish = projector
            .finish_after_eos(completed, chrono::Utc::now())
            .expect("finished browser output");
        let full_message = finish
            .into_iter()
            .map(|event| {
                serde_json::from_slice::<serde_json::Value>(
                    &encode_current_node_event_json(&event).expect("canonical browser event"),
                )
                .expect("browser event JSON")
            })
            .find(|event| event["type"] == "full_message")
            .expect("full message event");
        assert_eq!(full_message["content"], "native Anthropic response");

        let captured = captured.lock().expect("captured native request");
        assert_eq!(captured.len(), 1);
        assert_eq!(captured[0].uri.path(), "/llm/v1/messages");
        let body: serde_json::Value =
            serde_json::from_slice(&captured[0].body).expect("native request JSON");
        assert_eq!(body["model"], "claude-sonnet-4-5");
        assert_eq!(body["messages"][0]["role"], "user");
        assert_eq!(
            body["messages"][0]["content"][0]["text"],
            "earlier question"
        );
        assert_eq!(body["messages"][1]["role"], "assistant");
        assert_eq!(body["messages"][1]["content"][0]["text"], "earlier ");
        assert_eq!(body["messages"][1]["content"][1]["text"], "answer");
        assert_eq!(body["messages"][2]["content"], "current");
        assert_eq!(captured[0].headers["x-project-id"], "17");
        assert!(captured[0].headers["authorization"].is_sensitive());
        assert!(captured[0].headers["x-api-key"].is_sensitive());
    }
}

#[tokio::test(flavor = "current_thread")]
async fn application_and_adhoc_execute_materialized_tools_in_the_direct_adk_loop() {
    for kind in [AgentExecutionKind::Application, AgentExecutionKind::Adhoc] {
        let mut request = ordinary_request(kind);
        attach_local_gitlab_tool(&mut request);
        let (runtime_context, context_calls) = runtime_context_client();
        let (model_gateway, captured) = test_model_gateway_client(
            vec![
                TestModelGatewayOutcome::Response(tool_call_response()),
                TestModelGatewayOutcome::Response(model_response()),
            ],
            test_model_gateway_config(),
        )
        .expect("model gateway fixture client");
        let assembler = OrdinaryNativeAgentAssembler::new(
            platform_client(runtime_context),
            Arc::new(ModelFacade::from_gateway(model_gateway)),
            empty_tool_policy(),
        );
        let assembly = AuthorizedNativeAssembly::new(
            &request,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        );
        let mut invocation = assembler
            .assemble(assembly)
            .await
            .expect("authorized direct agent assembly");
        assert_eq!(context_calls.load(Ordering::Acquire), 1);
        let _ = invocation
            .project_start(chrono::Utc::now())
            .expect("agent start");

        let (mut native, mut projector, completion) = invocation.start().expect("native start");
        let mut event_types = Vec::new();
        while let Some(event) = native.next_event().await.expect("native event") {
            event_types.extend(
                projector
                    .project(&event)
                    .expect("projected tool-loop event")
                    .into_iter()
                    .map(|event| event.r#type),
            );
        }
        let completed = completion.select().await.expect("selected completion");
        let finish = projector
            .finish_after_eos(completed, chrono::Utc::now())
            .expect("finished browser output");
        assert!(event_types.iter().any(|event| event == "agent_tool_start"));
        assert!(event_types.iter().any(|event| event == "agent_tool_end"));
        assert_eq!(finish.into_iter().count(), 3);

        let captured = captured.lock().expect("captured model requests");
        assert_eq!(captured.len(), 2);
        let first: serde_json::Value =
            serde_json::from_slice(&captured[0].body).expect("first model request");
        assert_eq!(first["tools"][0]["function"]["name"], "set_active_branch");
        let second: serde_json::Value =
            serde_json::from_slice(&captured[1].body).expect("second model request");
        assert_eq!(second["messages"].as_array().map(Vec::len), Some(4));
        assert_eq!(second["messages"][2]["role"], "assistant");
        assert_eq!(second["messages"][3]["role"], "tool");
        assert_eq!(second["messages"][3]["tool_call_id"], "call_branch");
        assert_eq!(
            second["messages"][3]["content"],
            "\"Active branch set to release/1.2\""
        );
    }
}

#[tokio::test(flavor = "current_thread")]
async fn application_and_adhoc_execute_adk_mcp_tools_in_the_direct_llm_loop() {
    for kind in [AgentExecutionKind::Application, AgentExecutionKind::Adhoc] {
        let mut request = ordinary_request(kind);
        attach_remote_mcp_tool(&mut request);
        let (runtime_context, context_calls) = runtime_context_client();
        let (model_gateway, captured) = test_model_gateway_client(
            vec![
                TestModelGatewayOutcome::Response(mcp_tool_call_response()),
                TestModelGatewayOutcome::Response(model_response()),
            ],
            test_model_gateway_config(),
        )
        .expect("model gateway fixture client");
        let tool_calls = Arc::new(AtomicUsize::new(0));
        let connector = Arc::new(AgentMcpConnector {
            calls: AtomicUsize::new(0),
            tool_calls: tool_calls.clone(),
        });
        let assembler = OrdinaryNativeAgentAssembler::new(
            platform_client(runtime_context),
            Arc::new(ModelFacade::from_gateway(model_gateway)),
            empty_tool_policy(),
        )
        .with_mcp_connector(connector.clone());
        let assembly = AuthorizedNativeAssembly::new(
            &request,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        );
        let mut invocation = assembler
            .assemble(assembly)
            .await
            .expect("authorized MCP agent assembly");
        assert_eq!(context_calls.load(Ordering::Acquire), 1);
        assert_eq!(connector.calls.load(Ordering::Acquire), 1);
        let _ = invocation
            .project_start(chrono::Utc::now())
            .expect("agent start");

        let (mut native, mut projector, completion) = invocation.start().expect("native start");
        while let Some(event) = native.next_event().await.expect("native MCP event") {
            let _batch = projector.project(&event).expect("projected MCP event");
        }
        let completed = completion.select().await.expect("selected completion");
        let _finish = projector
            .finish_after_eos(completed, chrono::Utc::now())
            .expect("finished MCP browser output");

        assert_eq!(tool_calls.load(Ordering::Acquire), 1);
        let captured = captured.lock().expect("captured model requests");
        assert_eq!(captured.len(), 2);
        let first: serde_json::Value =
            serde_json::from_slice(&captured[0].body).expect("first model request");
        assert_eq!(first["tools"][0]["function"]["name"], "lookup_release");
        assert!(
            first["tools"][0]["function"]["description"]
                .as_str()
                .is_some_and(|description| description.contains("release intelligence"))
        );
        assert!(
            !captured[0]
                .body
                .windows("mcp.example.invalid".len())
                .any(|window| window == b"mcp.example.invalid")
        );
        let second: serde_json::Value =
            serde_json::from_slice(&captured[1].body).expect("second model request");
        assert_eq!(second["messages"][3]["role"], "tool");
        assert_eq!(second["messages"][3]["tool_call_id"], "call_mcp");
        assert_eq!(
            second["messages"][3]["content"],
            r#"{"release":"1.2","risk":"low"}"#
        );
    }
}

#[tokio::test(flavor = "current_thread")]
async fn application_and_adhoc_bind_same_named_tools_to_exact_toolkit_implementations() {
    for kind in [AgentExecutionKind::Application, AgentExecutionKind::Adhoc] {
        let mut request = ordinary_request(kind);
        attach_colliding_remote_mcp_tools(&mut request);
        let (runtime_context, context_calls) = runtime_context_client();
        let (model_gateway, captured) = test_model_gateway_client(
            vec![
                TestModelGatewayOutcome::Response(colliding_mcp_tool_call_response()),
                TestModelGatewayOutcome::Response(model_response()),
            ],
            test_model_gateway_config(),
        )
        .expect("model gateway fixture client");
        let tool_calls = Arc::new(Mutex::new(Vec::new()));
        let connector = Arc::new(CollidingMcpConnector {
            connections: AtomicUsize::new(0),
            tool_calls: Arc::clone(&tool_calls),
        });
        let assembler = OrdinaryNativeAgentAssembler::new(
            platform_client(runtime_context),
            Arc::new(ModelFacade::from_gateway(model_gateway)),
            empty_tool_policy(),
        )
        .with_mcp_connector(connector.clone());
        let assembly = AuthorizedNativeAssembly::new(
            &request,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        );
        let invocation = assembler
            .assemble(assembly)
            .await
            .expect("colliding-tool agent assembly");
        let (mut native, _projector, completion) = invocation.start().expect("native start");
        while native
            .next_event()
            .await
            .expect("colliding-tool event")
            .is_some()
        {}
        let _ = completion.select().await.expect("selected completion");

        assert_eq!(context_calls.load(Ordering::Acquire), 1);
        assert_eq!(connector.connections.load(Ordering::Acquire), 2);
        let mut invoked = tool_calls.lock().expect("tool call fixture lock").clone();
        invoked.sort();
        assert_eq!(invoked, ["audit intelligence", "release intelligence"]);

        let captured = captured.lock().expect("captured model requests");
        assert_eq!(captured.len(), 2);
        let first: serde_json::Value =
            serde_json::from_slice(&captured[0].body).expect("first model request");
        let visible_names = first["tools"]
            .as_array()
            .expect("provider tools")
            .iter()
            .map(|tool| {
                tool["function"]["name"]
                    .as_str()
                    .expect("provider tool name")
                    .to_owned()
            })
            .collect::<HashSet<_>>();
        assert_eq!(
            visible_names,
            HashSet::from([
                "audit_intelligence__lookup_release".to_owned(),
                "release_intelligence__lookup_release".to_owned(),
            ])
        );
        let second: serde_json::Value =
            serde_json::from_slice(&captured[1].body).expect("second model request");
        let tool_results = second["messages"]
            .as_array()
            .expect("continuation messages")
            .iter()
            .filter(|message| message["role"] == "tool")
            .map(|message| {
                (
                    message["tool_call_id"]
                        .as_str()
                        .expect("tool call ID")
                        .to_owned(),
                    message["content"].as_str().expect("tool result").to_owned(),
                )
            })
            .collect::<BTreeMap<_, _>>();
        assert!(tool_results["call_release"].contains("release intelligence"));
        assert!(tool_results["call_audit"].contains("audit intelligence"));
    }
}

#[tokio::test(flavor = "current_thread")]
async fn application_and_adhoc_resume_model_owned_delegated_authorization() {
    for kind in [AgentExecutionKind::Application, AgentExecutionKind::Adhoc] {
        for count in [1, 3] {
            run_delegated_authorization_resume(kind, true, count).await;
            run_delegated_authorization_resume(kind, false, count).await;
        }
    }
}

#[allow(clippy::too_many_lines)] // One initial pause and exact resume form one behavioral proof.
async fn run_delegated_authorization_resume(
    kind: AgentExecutionKind,
    authorize: bool,
    count: usize,
) {
    // One toolkit authorization decision covers the whole pending tool batch.
    let attempts = 1;
    let (runtime_context, context_calls) = runtime_context_client_for_redemptions(attempts + 1);
    let mut outcomes = vec![TestModelGatewayOutcome::Response(
        mcp_authorization_response(),
    )];
    if authorize {
        outcomes.push(TestModelGatewayOutcome::Response(mcp_tool_batch_response(
            count,
        )));
    } else if count > 1 {
        for prefix in ["call_retry_first", "call_retry_second"] {
            outcomes.push(TestModelGatewayOutcome::Response(named_mcp_batch_response(
                &mcp_authorization_tool_name(),
                count,
                prefix,
            )));
        }
    }
    outcomes.push(TestModelGatewayOutcome::Response(model_response()));
    let (model_gateway, captured) =
        test_model_gateway_client(outcomes, test_model_gateway_config())
            .expect("delegated authorization model gateway");
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let connector = Arc::new(AgentDelegatedAuthorizationMcpConnector {
        calls: AtomicUsize::new(0),
        tool_calls: Arc::clone(&tool_calls),
    });
    let sessions = Arc::new(InMemorySessionService::new());
    let shared_sessions: Arc<dyn SessionService> = sessions;
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(runtime_context),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        empty_tool_policy(),
    )
    .with_mcp_connector(connector.clone())
    .with_sessions(Arc::clone(&shared_sessions));

    let mut initial_request = ordinary_request(kind);
    attach_remote_mcp_tool(&mut initial_request);
    select_both_mcp_operations(&mut initial_request);
    let initial = AuthorizedNativeAssembly::new(
        &initial_request,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    );
    let mut paused = assembler
        .assemble(initial)
        .await
        .expect("initial delegated authorization assembly");
    paused
        .project_start(chrono::Utc::now())
        .expect("delegated authorization start");
    let (mut run, mut projector, _completion) = paused.start().expect("initial native start");
    let mut public_events = Vec::new();
    while let Some(event) = run.next_event().await.expect("initial native event") {
        for projected in projector.project(&event).expect("projected auth event") {
            public_events.push(
                serde_json::from_slice::<serde_json::Value>(
                    &encode_current_node_event_json(&projected)
                        .expect("canonical authorization browser event"),
                )
                .expect("authorization browser event JSON"),
            );
        }
    }
    assert!(projector.is_paused());
    let card = public_events
        .iter()
        .find(|event| event["type"] == "mcp_authorization_required")
        .expect("root model-owned authorization card");
    assert_eq!(card["response_metadata"]["tool_call_id"], "call_mcp");
    assert_eq!(
        card["response_metadata"]["tool_args"],
        serde_json::json!({})
    );
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);

    let mut resume_request = ordinary_request(kind);
    resume_request.binding.request_content_digest = if authorize { [13; 32] } else { [14; 32] };
    attach_remote_mcp_tool(&mut resume_request);
    select_both_mcp_operations(&mut resume_request);
    resume_request.payload.should_continue = true;
    if authorize {
        resume_request.payload.mcp_tokens.insert(
            "https://mcp.example.invalid/v1/mcp".to_owned(),
            serde_json::json!({"access_token": "runtime-secret"}),
        );
    } else {
        resume_request
            .payload
            .user_declined_mcp_servers
            .push(serde_json::json!({
                "server_url": "https://mcp.example.invalid/v1/mcp"
            }));
    }
    for attempt in 0..attempts {
        let resume = AuthorizedNativeAssembly::new(
            &resume_request,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        );
        let mut resumed = assembler
            .assemble(resume)
            .await
            .expect("delegated authorization continuation");
        resumed
            .project_start(chrono::Utc::now())
            .expect("resume start projection");
        let (mut run, mut projector, completion) = resumed.start().expect("resumed native start");
        while let Some(event) = run.next_event().await.unwrap_or_else(|error| {
            panic!("resumed authorization event {error:?}; authorize={authorize}, count={count}")
        }) {
            projector
                .project(&event)
                .expect("resumed authorization projection");
        }
        if attempt + 1 == attempts {
            let _completed = completion.select().await.unwrap_or_else(|error| {
                panic!("resumed completion: {error:?}; authorize={authorize}, count={count}, attempt={attempt}")
            });
            assert!(!projector.is_paused());
        } else {
            assert!(
                projector.is_paused(),
                "a distinct pending call retains its guard"
            );
        }
    }
    assert_eq!(context_calls.load(Ordering::Acquire), attempts + 1);
    assert_eq!(connector.calls.load(Ordering::Acquire), attempts + 1);
    assert_eq!(
        tool_calls.load(Ordering::Acquire),
        if authorize { count } else { 0 }
    );

    let captured = captured.lock().expect("captured model requests");
    assert_eq!(
        captured.len(),
        if authorize {
            3
        } else if count > 1 {
            4
        } else {
            2
        }
    );
    let initial: serde_json::Value = serde_json::from_slice(&captured[0].body).unwrap();
    assert_eq!(initial["tools"].as_array().unwrap().len(), 1);
    assert_eq!(
        initial["tools"][0]["function"]["name"],
        mcp_authorization_tool_name()
    );
    let resumed: serde_json::Value =
        serde_json::from_slice(&captured[1].body).expect("resumed model request");
    assert_complete_tool_history(&resumed);
    let tool_message = resumed["messages"]
        .as_array()
        .expect("resumed messages")
        .iter()
        .find(|message| message["role"] == "tool" && message["tool_call_id"] == "call_mcp")
        .expect("same original call result");
    if authorize {
        let result: serde_json::Value =
            serde_json::from_str(tool_message["content"].as_str().unwrap()).unwrap();
        assert_eq!(result["status"], "authorized");
        assert!(
            resumed["tools"]
                .as_array()
                .unwrap()
                .iter()
                .all(|tool| tool["function"]["name"] != mcp_authorization_tool_name())
        );
        assert!(
            resumed["tools"]
                .as_array()
                .unwrap()
                .iter()
                .any(|tool| tool["function"]["name"] == "lookup_release")
        );
    } else {
        let declined: serde_json::Value = serde_json::from_str(
            tool_message["content"]
                .as_str()
                .expect("structured declined result"),
        )
        .expect("declined result JSON");
        assert_eq!(declined["type"], "mcp_auth_decision");
        assert_eq!(declined["status"], "declined");
    }
}

fn select_both_mcp_operations(request: &mut super::request::AgentExecutionRequest) {
    let tool = match request.kind {
        AgentExecutionKind::Application => &mut request
            .payload
            .application
            .get_mut("version_details")
            .unwrap()["tools"][0],
        AgentExecutionKind::Adhoc => &mut request.payload.tools[0],
    };
    tool["settings"]["selected_tools"] = serde_json::json!(["lookup_release", "inspect_release"]);
}

pub(super) fn assert_complete_tool_history(request: &serde_json::Value) {
    let mut pending = HashSet::new();
    for message in request["messages"].as_array().expect("messages") {
        if message["role"] == "tool" {
            let id = message["tool_call_id"].as_str().expect("result call ID");
            assert!(
                pending.remove(id),
                "result must match one pending call: {id}"
            );
        } else {
            assert!(
                pending.is_empty(),
                "unanswered calls before next model message: {pending:?}"
            );
            if let Some(calls) = message["tool_calls"].as_array() {
                for call in calls {
                    assert!(pending.insert(call["id"].as_str().expect("call ID").to_owned()));
                }
            }
        }
    }
    assert!(
        pending.is_empty(),
        "provider request has unanswered calls: {pending:?}"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn delegated_authorization_does_not_approve_a_distinct_sensitive_action() {
    let (runtime_context, _) = runtime_context_client_for_redemptions(2);
    let (model_gateway, captured) = test_model_gateway_client(
        vec![
            TestModelGatewayOutcome::Response(mcp_authorization_response()),
            TestModelGatewayOutcome::Response(mcp_tool_batch_response(1)),
        ],
        test_model_gateway_config(),
    )
    .expect("authorization plus sensitive model gateway");
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let connector = Arc::new(AgentDelegatedAuthorizationMcpConnector {
        calls: AtomicUsize::new(0),
        tool_calls: Arc::clone(&tool_calls),
    });
    let sessions = Arc::new(InMemorySessionService::new());
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(runtime_context),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        sensitive_mcp_policy(),
    )
    .with_mcp_connector(connector.clone())
    .with_sessions(sessions as Arc<dyn SessionService>);

    let mut request = ordinary_request(AgentExecutionKind::Application);
    attach_remote_mcp_tool(&mut request);
    let mut invocation = assembler
        .assemble(AuthorizedNativeAssembly::new(
            &request,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        ))
        .await
        .expect("initial authorization assembly");
    invocation
        .project_start(chrono::Utc::now())
        .expect("initial authorization start");
    let (mut run, mut projector, _) = invocation.start().expect("initial authorization run");
    while let Some(event) = run.next_event().await.expect("initial authorization event") {
        let _ = projector.project(&event).expect("initial auth projection");
    }
    assert!(projector.is_paused());

    let mut resume = ordinary_request(AgentExecutionKind::Application);
    resume.binding.request_content_digest = [15; 32];
    attach_remote_mcp_tool(&mut resume);
    resume.payload.should_continue = true;
    resume.payload.mcp_tokens.insert(
        "https://mcp.example.invalid/v1/mcp".to_owned(),
        serde_json::json!({"access_token": "runtime-secret"}),
    );
    let mut invocation = assembler
        .assemble(AuthorizedNativeAssembly::new(
            &resume,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        ))
        .await
        .expect("authorized replay assembly");
    invocation
        .project_start(chrono::Utc::now())
        .expect("authorized replay start");
    let (mut run, mut projector, _) = invocation.start().expect("authorized replay run");
    let mut sensitive_cards = 0;
    while let Some(event) = run.next_event().await.expect("authorized replay event") {
        for projected in projector.project(&event).expect("sensitive projection") {
            let value: serde_json::Value = serde_json::from_slice(
                &encode_current_node_event_json(&projected).expect("sensitive browser event"),
            )
            .expect("sensitive browser JSON");
            sensitive_cards += usize::from(value["type"] == "agent_hitl_interrupt");
        }
    }
    assert!(projector.is_paused());
    assert_eq!(sensitive_cards, 1);
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);
    assert_eq!(connector.calls.load(Ordering::Acquire), 2);
    assert_eq!(
        captured.lock().expect("captured model requests").len(),
        2,
        "authorization exposes the operation, which retains its sensitive guard"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn application_and_adhoc_resume_exact_read_only_sensitive_mcp_call() {
    for kind in [AgentExecutionKind::Application, AgentExecutionKind::Adhoc] {
        run_sensitive_mcp_resume(kind).await;
    }
}

async fn run_sensitive_mcp_resume(kind: AgentExecutionKind) {
    let (runtime_context, context_calls) = runtime_context_client_for_redemptions(2);
    let (model_gateway, captured) = test_model_gateway_client(
        vec![
            TestModelGatewayOutcome::Response(mcp_tool_call_response()),
            TestModelGatewayOutcome::Response(model_response()),
        ],
        test_model_gateway_config(),
    )
    .expect("model gateway fixture client");
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let connector = Arc::new(AgentMcpConnector {
        calls: AtomicUsize::new(0),
        tool_calls: Arc::clone(&tool_calls),
    });
    let sessions = Arc::new(InMemorySessionService::new());
    let shared_sessions: Arc<dyn SessionService> = sessions;
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(runtime_context),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        sensitive_mcp_policy(),
    )
    .with_mcp_connector(connector.clone())
    .with_sessions(Arc::clone(&shared_sessions));

    let mut initial_request = ordinary_request(kind);
    attach_remote_mcp_tool(&mut initial_request);
    let initial = AuthorizedNativeAssembly::new(
        &initial_request,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    );
    let mut paused = assembler
        .assemble(initial)
        .await
        .expect("initial sensitive MCP assembly");
    paused
        .project_start(chrono::Utc::now())
        .expect("projected initial start");
    let (mut run, mut projector, _completion) = paused.start().expect("initial native start");
    let mut public_events = Vec::new();
    while let Some(event) = run.next_event().await.expect("initial native event") {
        for projected in projector.project(&event).expect("projected initial event") {
            public_events.push(
                serde_json::from_slice::<serde_json::Value>(
                    &encode_current_node_event_json(&projected)
                        .expect("canonical initial browser event"),
                )
                .expect("initial browser event JSON"),
            );
        }
    }
    assert!(projector.is_paused());
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);
    let interrupt_id = projected_interrupt_id(&public_events).to_owned();

    let mut resume_request = ordinary_request(kind);
    resume_request.binding.request_content_digest = [9; 32];
    attach_remote_mcp_tool(&mut resume_request);
    admit_approved_resume(&mut resume_request, &interrupt_id);
    let resume = AuthorizedNativeAssembly::new(
        &resume_request,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    );
    let resumed = assembler
        .assemble(resume)
        .await
        .expect("resumed sensitive MCP assembly");
    let (mut run, _projector, completion) = resumed.start().expect("resumed native start");
    while run
        .next_event()
        .await
        .expect("resumed native event")
        .is_some()
    {}
    let _completed = completion.select().await.expect("resumed completion");
    assert_eq!(context_calls.load(Ordering::Acquire), 2);
    assert_eq!(connector.calls.load(Ordering::Acquire), 2);
    assert_eq!(tool_calls.load(Ordering::Acquire), 1);
    assert_resumed_model_requests(&captured);
}

fn projected_interrupt_id(events: &[serde_json::Value]) -> &str {
    events
        .iter()
        .find(|event| event["type"] == "agent_hitl_interrupt")
        .and_then(|event| event["response_metadata"]["hitl_interrupts"][0]["interrupt_id"].as_str())
        .expect("public direct-tool interrupt identity")
}

fn admit_approved_resume(request: &mut super::request::AgentExecutionRequest, interrupt_id: &str) {
    request.payload.should_continue = true;
    request.payload.hitl_resume = true;
    request.payload.hitl_action = Some("approve".to_owned());
    request.payload.hitl_value = Some(String::new());
    request.payload.hitl_decisions = vec![serde_json::json!({
        "interrupt_id": interrupt_id,
        "tool_call_id": "call_mcp",
        "action": "approve",
        "value": ""
    })];
}

fn assert_resumed_model_requests(captured: &Arc<Mutex<Vec<CapturedModelRequest>>>) {
    let captured = captured.lock().expect("captured model requests");
    assert_eq!(captured.len(), 2);
    let resumed: serde_json::Value =
        serde_json::from_slice(&captured[1].body).expect("resumed model request");
    let messages = resumed["messages"].as_array().expect("resumed messages");
    assert!(
        messages
            .iter()
            .any(|message| { message["role"] == "tool" && message["tool_call_id"] == "call_mcp" })
    );
}

#[tokio::test(flavor = "current_thread")]
async fn saved_agent_is_resolved_once_and_runs_as_an_adk_agent_tool() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    attach_nested_agent(&mut request);
    let (runtime_context, context_calls, context_paths) = runtime_context_with_child();
    let (model_gateway, captured) = test_model_gateway_client(
        vec![
            TestModelGatewayOutcome::Response(nested_agent_call_response()),
            TestModelGatewayOutcome::Response(text_response("child release risk")),
            TestModelGatewayOutcome::Response(text_response("parent final answer")),
        ],
        test_model_gateway_config(),
    )
    .expect("model gateway fixture client");
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(runtime_context),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        empty_tool_policy(),
    );
    let assembly = AuthorizedNativeAssembly::new(
        &request,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    );
    let mut invocation = assembler
        .assemble(assembly)
        .await
        .expect("root plus nested agent assembly");

    assert_eq!(context_calls.load(Ordering::Acquire), 2);
    assert_eq!(
        *context_paths.lock().expect("runtime-context paths"),
        [
            "/executions/execution%2Fone/generations/2/runtime-context/elitea-client-token",
            "/executions/execution%2Fone/generations/2/runtime-context/applications/31/versions/41",
        ]
    );
    let _ = invocation
        .project_start(chrono::Utc::now())
        .expect("agent start");
    let (mut native, mut projector, completion) = invocation.start().expect("native start");
    let mut event_types = Vec::new();
    let mut public_events = Vec::new();
    while let Some(event) = native.next_event().await.expect("native event") {
        for event in projector
            .project(&event)
            .expect("projected nested-agent event")
        {
            event_types.push(event.r#type.clone());
            public_events.push(
                serde_json::from_slice::<serde_json::Value>(
                    &encode_current_node_event_json(&event).expect("canonical nested-agent event"),
                )
                .expect("nested-agent browser event JSON"),
            );
        }
    }
    let completed = completion.select().await.expect("selected root completion");
    let finish = projector
        .finish_after_eos(completed, chrono::Utc::now())
        .expect("finished browser output");
    let final_output = finish
        .into_iter()
        .find(|event| event.r#type == "full_message")
        .expect("root full message");
    let final_json: serde_json::Value = serde_json::from_slice(
        &encode_current_node_event_json(&final_output).expect("canonical browser event"),
    )
    .expect("browser JSON");
    assert_eq!(final_json["content"], "parent final answer");
    assert!(event_types.iter().any(|event| event == "agent_tool_start"));
    assert!(event_types.iter().any(|event| event == "agent_tool_end"));
    let wrapper = public_events
        .iter()
        .find(|event| event["type"] == "agent_tool_start")
        .expect("nested-agent wrapper event");
    assert_eq!(
        wrapper["response_metadata"]["metadata"]["original_name"],
        "release-risk-agent"
    );
    assert_eq!(
        wrapper["response_metadata"]["metadata"]["toolkit_type"],
        "application"
    );
    assert_eq!(
        wrapper["response_metadata"]["parent_agent_call_id"],
        "call_child"
    );
    assert_eq!(wrapper["response_metadata"]["sibling_ordinal"], 1);
    assert_streamed_child_model_event(&public_events);

    let captured = captured.lock().expect("captured model requests");
    assert_nested_agent_model_requests(&captured);
}

#[tokio::test(flavor = "current_thread")]
async fn parallel_nested_sensitive_calls_persist_distinct_hierarchical_interrupts() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    attach_nested_agent(&mut request);
    let binding = AuthorizedNativeCommandBinding::fixture();
    let profile = OrdinaryNoToolProfile::validate(&request).expect("root application profile");
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &binding,
        &request.payload.input_attachments,
    )
    .expect("root session plan");
    let user_id = plan.user_id().to_owned();
    let session_id = plan.session_id().to_owned();

    let (runtime_context, context_calls) = runtime_context_with_sensitive_child();
    let (model_gateway, captured) = test_model_gateway_client(
        vec![
            TestModelGatewayOutcome::Response(parallel_nested_agent_call_response()),
            TestModelGatewayOutcome::Response(mcp_tool_call_response()),
            TestModelGatewayOutcome::Response(mcp_tool_call_response()),
            TestModelGatewayOutcome::Response(text_response("resolved child")),
            TestModelGatewayOutcome::Response(text_response("resolved child")),
            TestModelGatewayOutcome::Response(text_response("root resumed answer")),
        ],
        test_model_gateway_config(),
    )
    .expect("parallel nested model gateway");
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let connector = Arc::new(AgentMcpConnector {
        calls: AtomicUsize::new(0),
        tool_calls: Arc::clone(&tool_calls),
    });
    let sessions = Arc::new(InMemorySessionService::new());
    let injected_sessions: Arc<dyn SessionService> = sessions.clone();
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(runtime_context),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        sensitive_mcp_policy(),
    )
    .with_mcp_connector(connector.clone())
    .with_sessions(injected_sessions);
    let assembly =
        AuthorizedNativeAssembly::new(&request, test_runtime_context_authority(), binding);
    let mut invocation = assembler
        .assemble(assembly)
        .await
        .expect("parallel nested sensitive assembly");
    invocation
        .project_start(chrono::Utc::now())
        .expect("parallel nested start");
    let (mut native, mut projector, _completion) = invocation.start().expect("native start");
    let mut public_interrupts = Vec::new();
    while let Some(event) = native.next_event().await.expect("nested sensitive event") {
        for projected in projector
            .project(&event)
            .expect("nested sensitive projection")
        {
            let value: serde_json::Value = serde_json::from_slice(
                &encode_current_node_event_json(&projected)
                    .expect("nested sensitive browser event"),
            )
            .expect("nested sensitive browser JSON");
            if value["type"] == "agent_hitl_interrupt" {
                public_interrupts.push(value);
            }
        }
    }

    assert!(projector.is_paused());
    let interrupt_ids = assert_parallel_nested_interrupts(&public_interrupts);
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);
    assert_eq!(context_calls.load(Ordering::Acquire), 2);
    assert_eq!(connector.calls.load(Ordering::Acquire), 1);
    assert_eq!(captured.lock().expect("captured model requests").len(), 3);

    assert_persisted_nested_interrupts(&sessions, &user_id, &session_id, 0).await;
    assert_partial_nested_resume_is_rejected(&assembler, &interrupt_ids[0]).await;
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);
    assert_eq!(context_calls.load(Ordering::Acquire), 4);
    assert_eq!(connector.calls.load(Ordering::Acquire), 2);
    assert_eq!(captured.lock().expect("captured model requests").len(), 3);
    let final_output = resume_parallel_nested_sensitive_calls(&assembler, interrupt_ids).await;
    assert_eq!(final_output, "root resumed answer");
    assert_eq!(tool_calls.load(Ordering::Acquire), 2);
    assert_eq!(context_calls.load(Ordering::Acquire), 6);
    assert_eq!(connector.calls.load(Ordering::Acquire), 3);
    assert_eq!(captured.lock().expect("captured model requests").len(), 6);
    assert_persisted_nested_interrupts(&sessions, &user_id, &session_id, 2).await;
}

#[tokio::test(flavor = "current_thread")]
#[allow(clippy::too_many_lines)] // Initial parallel pause and atomic exact-child resume are one proof.
async fn parallel_nested_ask_user_resumes_each_exact_child_without_replanning() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    attach_nested_agent(&mut request);
    let (runtime_context, context_calls) = runtime_context_with_ask_user_child();
    let (model_gateway, captured) = test_model_gateway_client(
        vec![
            TestModelGatewayOutcome::Response(parallel_nested_agent_call_response()),
            TestModelGatewayOutcome::Response(ask_user_tool_call_response()),
            TestModelGatewayOutcome::Response(ask_user_tool_call_response()),
            TestModelGatewayOutcome::Response(text_response("resolved clarified child")),
            TestModelGatewayOutcome::Response(text_response("resolved clarified child")),
            TestModelGatewayOutcome::Response(text_response("root clarified answer")),
        ],
        test_model_gateway_config(),
    )
    .expect("parallel nested ask_user model gateway");
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(runtime_context),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        empty_tool_policy(),
    )
    .with_sessions(sessions);

    let mut invocation = assembler
        .assemble(AuthorizedNativeAssembly::new(
            &request,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        ))
        .await
        .expect("parallel nested ask_user assembly");
    invocation
        .project_start(chrono::Utc::now())
        .expect("parallel nested ask_user start");
    let (mut native, mut projector, _) = invocation.start().expect("ask_user native start");
    let mut cards = Vec::new();
    while let Some(event) = native.next_event().await.expect("nested ask_user event") {
        for projected in projector
            .project(&event)
            .expect("nested ask_user projection")
        {
            let value: serde_json::Value = serde_json::from_slice(
                &encode_current_node_event_json(&projected).expect("nested ask_user browser event"),
            )
            .expect("nested ask_user browser JSON");
            if value["type"] == "agent_hitl_interrupt" {
                cards.push(value);
            }
        }
    }

    assert!(projector.is_paused());
    assert_parallel_nested_interrupts(&cards);
    let mut decisions = cards
        .iter()
        .map(|card| {
            let pending = &card["response_metadata"]["hitl_interrupts"][0];
            assert_eq!(pending["guardrail_type"], "clarifying_question");
            assert_eq!(pending["tool_call_id"], "call_ask_user");
            assert_eq!(pending["available_actions"], serde_json::json!(["answer"]));
            assert_eq!(pending["questions"][0]["id"], "q1");
            let parent_call = card["response_metadata"]["parent_agent_path"][0]["call_id"]
                .as_str()
                .expect("clarification parent call");
            let answer = match parent_call {
                "call_olivia" => "Olivia Lovelace",
                "call_sasha" => "Sasha Grey",
                other => panic!("unexpected clarification parent call: {other}"),
            };
            (
                pending["interrupt_id"]
                    .as_str()
                    .expect("clarification interrupt identity")
                    .to_owned(),
                serde_json::json!({"q1": answer}).to_string(),
            )
        })
        .collect::<Vec<_>>();
    decisions.sort_unstable();
    assert_eq!(decisions.len(), 2);
    assert_eq!(context_calls.load(Ordering::Acquire), 2);
    assert_eq!(captured.lock().expect("captured model requests").len(), 3);

    let mut partial = ordinary_request(AgentExecutionKind::Application);
    partial.binding.request_content_digest = [23; 32];
    attach_nested_agent(&mut partial);
    partial.payload.should_continue = true;
    partial.payload.hitl_resume = true;
    partial.payload.hitl_action = Some("answer".to_owned());
    partial.payload.hitl_value = Some(decisions[0].1.clone());
    partial.payload.hitl_decisions = vec![serde_json::json!({
        "interrupt_id": decisions[0].0,
        "tool_call_id": "call_ask_user",
        "guardrail_type": "clarifying_question",
        "action": "answer",
        "value": decisions[0].1,
    })];
    let Err(error) = assembler
        .assemble(AuthorizedNativeAssembly::new(
            &partial,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        ))
        .await
    else {
        panic!("partial parallel clarification set must fail closed");
    };
    assert_eq!(
        error.code(),
        NativeAgentAssemblyErrorCode::UnsupportedCapability
    );
    assert_eq!(context_calls.load(Ordering::Acquire), 4);
    assert_eq!(captured.lock().expect("captured model requests").len(), 3);

    let mut resume = ordinary_request(AgentExecutionKind::Application);
    resume.binding.request_content_digest = [24; 32];
    attach_nested_agent(&mut resume);
    resume.payload.should_continue = true;
    resume.payload.hitl_resume = true;
    resume.payload.hitl_action = None;
    resume.payload.hitl_value = None;
    resume.payload.hitl_decisions = decisions
        .into_iter()
        .map(|(interrupt_id, answer)| {
            serde_json::json!({
                "interrupt_id": interrupt_id,
                "tool_call_id": "call_ask_user",
                "guardrail_type": "clarifying_question",
                "action": "answer",
                "value": answer,
            })
        })
        .collect();
    let output = drain_nested_resume(&assembler, resume).await;
    assert_eq!(output, "root clarified answer");
    assert_eq!(context_calls.load(Ordering::Acquire), 6);

    let captured = captured.lock().expect("captured model requests");
    assert_eq!(captured.len(), 6, "resume must not replan child calls");
    let tool_results = captured
        .iter()
        .filter_map(|request| serde_json::from_slice::<serde_json::Value>(&request.body).ok())
        .flat_map(|request| request["messages"].as_array().cloned().unwrap_or_default())
        .filter(|message| message["role"] == "tool" && message["tool_call_id"] == "call_ask_user")
        .filter_map(|message| {
            message["content"]
                .as_str()
                .and_then(|content| serde_json::from_str::<String>(content).ok())
        })
        .collect::<HashSet<_>>();
    assert_eq!(
        tool_results,
        HashSet::from([
            "User answered:\n- Which full name should I resolve?: Olivia Lovelace".to_owned(),
            "User answered:\n- Which full name should I resolve?: Sasha Grey".to_owned(),
        ])
    );
}

#[tokio::test(flavor = "current_thread")]
async fn parallel_nested_authorization_resumes_exact_children_for_authorize_and_skip() {
    run_parallel_nested_authorization_resume(true).await;
    run_parallel_nested_authorization_resume(false).await;
}

#[allow(clippy::too_many_lines)] // Initial parallel pause and exact resume are one proof.
async fn run_parallel_nested_authorization_resume(authorize: bool) {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    attach_nested_agent(&mut request);
    let (runtime_context, context_calls) = runtime_context_with_sensitive_child();
    let mut outcomes = vec![
        TestModelGatewayOutcome::Response(parallel_nested_agent_call_response()),
        TestModelGatewayOutcome::Response(mcp_authorization_response()),
        TestModelGatewayOutcome::Response(mcp_authorization_response()),
    ];
    for _ in 0..2 {
        if authorize {
            outcomes.push(TestModelGatewayOutcome::Response(mcp_tool_batch_response(
                1,
            )));
        }
        outcomes.push(TestModelGatewayOutcome::Response(text_response(
            "resolved child",
        )));
    }
    outcomes.push(TestModelGatewayOutcome::Response(text_response(
        "root resumed answer",
    )));
    let (model_gateway, captured) =
        test_model_gateway_client(outcomes, test_model_gateway_config())
            .expect("parallel nested authorization model gateway");
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let connector = Arc::new(AgentDelegatedAuthorizationMcpConnector {
        calls: AtomicUsize::new(0),
        tool_calls: Arc::clone(&tool_calls),
    });
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(runtime_context),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        empty_tool_policy(),
    )
    .with_mcp_connector(connector.clone())
    .with_sessions(sessions);

    let mut invocation = assembler
        .assemble(AuthorizedNativeAssembly::new(
            &request,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        ))
        .await
        .expect("parallel nested authorization assembly");
    invocation
        .project_start(chrono::Utc::now())
        .expect("parallel nested authorization start");
    let (mut native, mut projector, _) = invocation.start().expect("authorization native start");
    let mut cards = Vec::new();
    while let Some(event) = native
        .next_event()
        .await
        .expect("nested authorization event")
    {
        for projected in projector
            .project(&event)
            .expect("nested authorization projection")
        {
            let value: serde_json::Value = serde_json::from_slice(
                &encode_current_node_event_json(&projected)
                    .expect("nested authorization browser event"),
            )
            .expect("nested authorization browser JSON");
            if value["type"] == "mcp_authorization_required" {
                cards.push(value);
            }
        }
    }
    assert!(projector.is_paused());
    assert_eq!(cards.len(), 2);
    let paths = cards
        .iter()
        .map(|event| event["response_metadata"]["parent_agent_path"].clone())
        .collect::<HashSet<_>>();
    assert_eq!(
        paths,
        HashSet::from([
            serde_json::json!([{
                "name": "release-risk-agent",
                "call_id": "call_olivia",
                "sibling_ordinal": 1,
            }]),
            serde_json::json!([{
                "name": "release-risk-agent",
                "call_id": "call_sasha",
                "sibling_ordinal": 2,
            }]),
        ])
    );
    let mut interrupt_ids = cards
        .iter()
        .map(|event| {
            event["response_metadata"]["interrupt_id"]
                .as_str()
                .expect("authorization interrupt identity")
                .to_owned()
        })
        .collect::<HashSet<_>>()
        .into_iter()
        .collect::<Vec<_>>();
    assert_eq!(interrupt_ids.len(), 2);
    interrupt_ids.sort_unstable();
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);

    let action = if authorize { "authorize" } else { "skip" };
    let mut partial = ordinary_request(AgentExecutionKind::Application);
    partial.binding.request_content_digest = if authorize { [19; 32] } else { [20; 32] };
    attach_nested_agent(&mut partial);
    partial.payload.should_continue = true;
    partial.payload.hitl_resume = true;
    partial.payload.hitl_action = Some(action.to_owned());
    partial.payload.hitl_value = Some(String::new());
    partial.payload.hitl_decisions = vec![serde_json::json!({
        "interrupt_id": interrupt_ids[0],
        "tool_call_id": "call_mcp",
        "guardrail_type": "mcp_auth",
        "action": action,
        "value": "",
    })];
    if authorize {
        partial.payload.mcp_tokens.insert(
            "https://mcp.example.invalid/v1/mcp".to_owned(),
            serde_json::json!({"access_token": "runtime-secret"}),
        );
    } else {
        partial
            .payload
            .user_declined_mcp_servers
            .push(serde_json::json!({
                "server_url": "https://mcp.example.invalid/v1/mcp"
            }));
    }
    let Err(error) = assembler
        .assemble(AuthorizedNativeAssembly::new(
            &partial,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        ))
        .await
    else {
        panic!("partial parallel authorization set must fail closed");
    };
    assert_eq!(
        error.code(),
        NativeAgentAssemblyErrorCode::UnsupportedCapability
    );
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);

    let mut resume = ordinary_request(AgentExecutionKind::Application);
    resume.binding.request_content_digest = if authorize { [21; 32] } else { [22; 32] };
    attach_nested_agent(&mut resume);
    resume.payload.should_continue = true;
    resume.payload.hitl_resume = true;
    resume.payload.hitl_action = None;
    resume.payload.hitl_value = None;
    resume.payload.hitl_decisions = interrupt_ids
        .iter()
        .map(|interrupt_id| {
            serde_json::json!({
                "interrupt_id": interrupt_id,
                "tool_call_id": "call_mcp",
                "guardrail_type": "mcp_auth",
                "action": action,
                "value": "",
            })
        })
        .collect();
    if authorize {
        resume.payload.mcp_tokens.insert(
            "https://mcp.example.invalid/v1/mcp".to_owned(),
            serde_json::json!({"access_token": "runtime-secret"}),
        );
    } else {
        resume
            .payload
            .user_declined_mcp_servers
            .push(serde_json::json!({
                "server_url": "https://mcp.example.invalid/v1/mcp"
            }));
    }
    let output = drain_nested_resume(&assembler, resume).await;
    assert_eq!(output, "root resumed answer");
    assert_eq!(
        tool_calls.load(Ordering::Acquire),
        usize::from(authorize) * 2
    );
    assert_eq!(connector.calls.load(Ordering::Acquire), 3);
    assert_eq!(context_calls.load(Ordering::Acquire), 6);
    let captured = captured.lock().expect("captured model requests");
    assert_eq!(captured.len(), if authorize { 8 } else { 6 });
    let parent: serde_json::Value = serde_json::from_slice(&captured.last().unwrap().body).unwrap();
    let child_results = parent["messages"]
        .as_array()
        .unwrap()
        .iter()
        .filter(|message| message["role"] == "tool")
        .map(|message| {
            serde_json::from_str::<serde_json::Value>(message["content"].as_str().unwrap()).unwrap()
        })
        .collect::<Vec<_>>();
    assert_eq!(
        child_results,
        vec![serde_json::json!({"response": "resolved child"}); 2]
    );
    let mut resumed_tasks = captured[3..captured.len() - 1]
        .iter()
        .map(|request| {
            let body: serde_json::Value =
                serde_json::from_slice(&request.body).expect("resumed child request");
            body["messages"]
                .as_array()
                .expect("model messages")
                .iter()
                .filter(|message| message["role"] == "user")
                .map(|message| message["content"].clone())
                .collect::<Vec<_>>()
        })
        .collect::<Vec<_>>();
    resumed_tasks.sort_by_cached_key(|task| serde_json::to_string(task).expect("user contents"));
    resumed_tasks.dedup();
    assert_eq!(
        resumed_tasks,
        vec![
            vec![serde_json::json!([{"type": "text", "text": "Resolve Olivia Lovelace"}])],
            vec![serde_json::json!([{"type": "text", "text": "Resolve Sasha Grey"}])],
        ],
        "resume must retain each child's original task and hide control markers"
    );
    if !authorize {
        assert_eq!(
            captured
                .iter()
                .filter(|request| request
                    .body
                    .windows("mcp_auth_decision".len())
                    .any(|window| { window == b"mcp_auth_decision" }))
                .count(),
            2,
            "each skipped child must receive a structured result under its original call"
        );
    }
}

#[tokio::test(flavor = "current_thread")]
async fn recursive_saved_agent_confirmation_persists_the_exact_two_tier_path() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    attach_nested_agent(&mut request);
    let binding = AuthorizedNativeCommandBinding::fixture();
    let profile = OrdinaryNoToolProfile::validate(&request).expect("root application profile");
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &binding,
        &request.payload.input_attachments,
    )
    .expect("root session plan");
    let user_id = plan.user_id().to_owned();
    let session_id = plan.session_id().to_owned();

    let harness = recursive_sensitive_harness(2, recursive_sensitive_outcomes());
    let RecursiveSensitiveHarness {
        assembler,
        sessions,
        tool_calls,
        context_calls,
        connector,
        captured,
    } = harness;
    let assembly =
        AuthorizedNativeAssembly::new(&request, test_runtime_context_authority(), binding);
    let mut invocation = assembler
        .assemble(assembly)
        .await
        .expect("recursive nested sensitive assembly");
    invocation
        .project_start(chrono::Utc::now())
        .expect("recursive nested start");
    let (mut native, mut projector, _completion) = invocation.start().expect("native start");
    let mut public_interrupt = None;
    while let Some(event) = native.next_event().await.expect("recursive nested event") {
        for projected in projector.project(&event).expect("recursive projection") {
            let value: serde_json::Value = serde_json::from_slice(
                &encode_current_node_event_json(&projected).expect("recursive browser event"),
            )
            .expect("recursive browser JSON");
            if value["type"] == "agent_hitl_interrupt" {
                public_interrupt = Some(value);
            }
        }
    }

    assert!(projector.is_paused());
    let interrupt = public_interrupt.expect("recursive confirmation");
    assert_eq!(
        interrupt["response_metadata"]["parent_agent_path"],
        serde_json::json!([
            {"name": "release-risk-agent", "call_id": "call_orchestrator", "sibling_ordinal": 1},
            {"name": "name-resolver", "call_id": "call_resolver", "sibling_ordinal": 1},
        ])
    );
    let interrupt_id = interrupt["response_metadata"]["hitl_interrupts"][0]["interrupt_id"]
        .as_str()
        .expect("recursive interrupt identity")
        .to_owned();
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);
    assert_eq!(context_calls.load(Ordering::Acquire), 3);
    assert_eq!(connector.calls.load(Ordering::Acquire), 1);
    assert_eq!(captured.lock().expect("captured model requests").len(), 3);
    let stored = sessions
        .get(GetRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id,
            session_id,
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("durable recursive root session");
    let confirmation = stored
        .events()
        .all()
        .into_iter()
        .find(|event| event.actions.tool_confirmation.is_some())
        .expect("persisted recursive confirmation");
    assert_eq!(
        confirmation.branch.split('.').count(),
        4,
        "root namespace plus two application branch segments"
    );
    assert_eq!(
        confirmation
            .provider_metadata
            .get("elitea.descendant.parent_call_id")
            .map(String::as_str),
        Some("call_resolver")
    );
    let final_output = resume_recursive_nested_sensitive_call(&assembler, &interrupt_id).await;
    assert_eq!(final_output, "root recursive answer");
    assert_eq!(tool_calls.load(Ordering::Acquire), 1);
    assert_eq!(context_calls.load(Ordering::Acquire), 6);
    assert_eq!(connector.calls.load(Ordering::Acquire), 2);
    assert_eq!(captured.lock().expect("captured model requests").len(), 6);
}

#[tokio::test(flavor = "current_thread")]
async fn recursive_parallel_saved_agents_resume_four_collision_safe_leaves() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    attach_nested_agent(&mut request);
    let outcomes = vec![
        model_response_for("fixture-model", parallel_nested_agent_call_response()),
        model_response_for("orchestrator-model", parallel_resolver_call_response()),
        model_response_for("orchestrator-model", parallel_resolver_call_response()),
        model_response_for("resolver-model", mcp_tool_call_response()),
        model_response_for("resolver-model", mcp_tool_call_response()),
        model_response_for("resolver-model", mcp_tool_call_response()),
        model_response_for("resolver-model", mcp_tool_call_response()),
        model_response_for("resolver-model", text_response("resolved leaf")),
        model_response_for("resolver-model", text_response("resolved leaf")),
        model_response_for("resolver-model", text_response("resolved leaf")),
        model_response_for("resolver-model", text_response("resolved leaf")),
        model_response_for("orchestrator-model", text_response("resolved orchestrator")),
        model_response_for("orchestrator-model", text_response("resolved orchestrator")),
        model_response_for(
            "fixture-model",
            text_response("root recursive parallel answer"),
        ),
    ];
    let RecursiveSensitiveHarness {
        assembler,
        sessions: _,
        tool_calls,
        context_calls,
        connector,
        captured,
    } = recursive_sensitive_harness(2, outcomes);
    let assembly = AuthorizedNativeAssembly::new(
        &request,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    );
    let mut invocation = assembler
        .assemble(assembly)
        .await
        .expect("recursive parallel assembly");
    invocation
        .project_start(chrono::Utc::now())
        .expect("recursive parallel start");
    let (mut native, mut projector, _completion) = invocation.start().expect("native start");
    let mut interrupts = Vec::new();
    while let Some(event) = native.next_event().await.expect("recursive parallel event") {
        for projected in projector
            .project(&event)
            .expect("recursive parallel projection")
        {
            let value: serde_json::Value = serde_json::from_slice(
                &encode_current_node_event_json(&projected)
                    .expect("recursive parallel browser event"),
            )
            .expect("recursive parallel browser JSON");
            if value["type"] == "agent_hitl_interrupt" {
                interrupts.push(value);
            }
        }
    }

    assert!(projector.is_paused());
    let interrupt_ids = assert_recursive_parallel_interrupts(&interrupts);
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);
    assert_eq!(context_calls.load(Ordering::Acquire), 3);
    assert_eq!(connector.calls.load(Ordering::Acquire), 1);
    assert_eq!(captured.lock().expect("captured model requests").len(), 7);
    let output = resume_parallel_nested_sensitive_calls(&assembler, interrupt_ids).await;
    assert_eq!(output, "root recursive parallel answer");
    assert_eq!(tool_calls.load(Ordering::Acquire), 4);
    assert_eq!(context_calls.load(Ordering::Acquire), 6);
    assert_eq!(connector.calls.load(Ordering::Acquire), 2);
    assert_eq!(captured.lock().expect("captured model requests").len(), 14);
}

fn assert_recursive_parallel_interrupts(events: &[serde_json::Value]) -> Vec<String> {
    assert_eq!(events.len(), 4);
    let paths = events
        .iter()
        .map(|event| event["response_metadata"]["parent_agent_path"].clone())
        .collect::<HashSet<_>>();
    let mut expected = HashSet::new();
    for (root_call, root_ordinal) in [("call_olivia", 1), ("call_sasha", 2)] {
        for (child_call, child_ordinal) in [("call_first", 1), ("call_last", 2)] {
            expected.insert(serde_json::json!([
                {"name": "release-risk-agent", "call_id": root_call, "sibling_ordinal": root_ordinal},
                {"name": "name-resolver", "call_id": child_call, "sibling_ordinal": child_ordinal},
            ]));
        }
    }
    assert_eq!(paths, expected);
    let interrupt_ids = events
        .iter()
        .map(|event| {
            event["response_metadata"]["hitl_interrupts"][0]["interrupt_id"]
                .as_str()
                .expect("recursive parallel interrupt identity")
                .to_owned()
        })
        .collect::<HashSet<_>>();
    assert_eq!(interrupt_ids.len(), 4);
    interrupt_ids.into_iter().collect()
}

async fn assert_partial_nested_resume_is_rejected(
    assembler: &OrdinaryNativeAgentAssembler,
    interrupt_id: &str,
) {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    request.binding.request_content_digest = [8; 32];
    attach_nested_agent(&mut request);
    admit_approved_resume(&mut request, interrupt_id);
    let assembly = AuthorizedNativeAssembly::new(
        &request,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    );
    let Err(error) = assembler.assemble(assembly).await else {
        panic!("partial parallel nested resume must fail closed");
    };
    assert_eq!(
        error.code(),
        NativeAgentAssemblyErrorCode::UnsupportedCapability
    );
}

fn assert_parallel_nested_interrupts(public_interrupts: &[serde_json::Value]) -> Vec<String> {
    assert_eq!(public_interrupts.len(), 2);
    let paths = public_interrupts
        .iter()
        .map(|event| event["response_metadata"]["parent_agent_path"].clone())
        .collect::<HashSet<_>>();
    assert_eq!(
        paths,
        HashSet::from([
            serde_json::json!([{
                "name": "release-risk-agent",
                "call_id": "call_olivia",
                "sibling_ordinal": 1,
            }]),
            serde_json::json!([{
                "name": "release-risk-agent",
                "call_id": "call_sasha",
                "sibling_ordinal": 2,
            }]),
        ])
    );
    let interrupt_ids = public_interrupts
        .iter()
        .map(|event| {
            event["response_metadata"]["hitl_interrupts"][0]["interrupt_id"]
                .as_str()
                .expect("nested interrupt identity")
                .to_owned()
        })
        .collect::<HashSet<_>>();
    assert_eq!(interrupt_ids.len(), 2);
    interrupt_ids.into_iter().collect()
}

async fn resume_parallel_nested_sensitive_calls(
    assembler: &OrdinaryNativeAgentAssembler,
    mut interrupt_ids: Vec<String>,
) -> String {
    interrupt_ids.sort_unstable();
    let mut request = ordinary_request(AgentExecutionKind::Application);
    request.binding.request_content_digest = [9; 32];
    attach_nested_agent(&mut request);
    request.payload.should_continue = true;
    request.payload.hitl_resume = true;
    request.payload.hitl_action = None;
    request.payload.hitl_value = None;
    request.payload.hitl_decisions = interrupt_ids
        .into_iter()
        .map(|interrupt_id| {
            serde_json::json!({
                "interrupt_id": interrupt_id,
                "tool_call_id": "call_mcp",
                "action": "approve",
                "value": "",
            })
        })
        .collect();
    drain_nested_resume(assembler, request).await
}

async fn resume_recursive_nested_sensitive_call(
    assembler: &OrdinaryNativeAgentAssembler,
    interrupt_id: &str,
) -> String {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    request.binding.request_content_digest = [7; 32];
    attach_nested_agent(&mut request);
    admit_approved_resume(&mut request, interrupt_id);
    drain_nested_resume(assembler, request).await
}

async fn drain_nested_resume(
    assembler: &OrdinaryNativeAgentAssembler,
    request: super::request::AgentExecutionRequest,
) -> String {
    let assembly = AuthorizedNativeAssembly::new(
        &request,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    );
    let mut invocation = assembler
        .assemble(assembly)
        .await
        .expect("parallel nested resume assembly");
    invocation
        .project_start(chrono::Utc::now())
        .expect("parallel nested resume start");
    let (mut native, mut projector, completion) = invocation.start().expect("resume native start");
    while let Some(event) = native.next_event().await.expect("nested resume event") {
        if let Err(error) = projector.project(&event) {
            panic!(
                "nested resume projection: {error:?}; author={}; branch={}; invocation={}; calls={:?}; results={:?}; confirmation={}",
                event.author,
                event.branch,
                event.invocation_id,
                event
                    .tool_calls()
                    .iter()
                    .map(|call| (call.name, call.call_id))
                    .collect::<Vec<_>>(),
                event
                    .tool_results()
                    .iter()
                    .map(|result| result.call_id)
                    .collect::<Vec<_>>(),
                event.actions.tool_confirmation.is_some(),
            );
        }
    }
    assert!(!projector.is_paused());
    let completed = completion.select().await.expect("nested resume completion");
    projector
        .finish_after_eos(completed, chrono::Utc::now())
        .expect("nested resume output")
        .into_iter()
        .find(|event| event.r#type == "full_message")
        .and_then(|event| serde_json::from_slice::<serde_json::Value>(&event.content).ok())
        .and_then(|content| content.as_str().map(str::to_owned))
        .expect("nested resumed full message")
}

async fn assert_persisted_nested_interrupts(
    sessions: &InMemorySessionService,
    user_id: &str,
    session_id: &str,
    expected_approved_results: usize,
) {
    let stored = sessions
        .get(GetRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id: user_id.to_owned(),
            session_id: session_id.to_owned(),
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("durable root session");
    let stored_events = stored.events().all();
    let confirmations = stored_events
        .iter()
        .filter(|event| event.actions.tool_confirmation.is_some())
        .collect::<Vec<_>>();
    assert_eq!(confirmations.len(), 2);
    assert!(confirmations.iter().all(|event| {
        event
            .branch
            .starts_with("elitea.saved_applications.application_")
            && event
                .provider_metadata
                .contains_key("elitea.descendant.container_invocation_id")
            && event
                .provider_metadata
                .contains_key("elitea.descendant.parent_call_id")
    }));
    let persisted = serde_json::to_string(&stored_events).expect("serialized root transcript");
    assert!(!persisted.contains("__elitea_nested_interrupt_v1"));
    let approved_results = stored_events
        .iter()
        .filter(|event| {
            event.actions.tool_confirmation_decision
                == Some(adk_rust::ToolConfirmationDecision::Approve)
                && event
                    .tool_results()
                    .iter()
                    .any(|result| result.name == "lookup_release")
        })
        .count();
    assert_eq!(approved_results, expected_approved_results);
}

#[tokio::test(flavor = "current_thread")]
async fn nested_agent_cycle_fails_after_one_exact_resolution_and_before_model_dispatch() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    attach_nested_agent(&mut request);
    let calls = Arc::new(AtomicUsize::new(0));
    let paths = Arc::new(Mutex::new(Vec::new()));
    let responses = VecDeque::from([
        runtime_context_response(&serde_json::json!({
            "schema_version": "elitea.runtime.elitea-client-token.v1",
            "project_id": 17,
            "token": TOKEN,
        })),
        application_version_response(
            31,
            41,
            nested_agent_version(
                "Cycle fixture.",
                "child-model",
                23,
                vec![stored_application_reference(
                    45,
                    31,
                    41,
                    "release-risk-agent",
                )],
            ),
        ),
    ]);
    let runtime_context =
        runtime_context_client_from(responses, Arc::clone(&calls), Arc::clone(&paths));
    let (model_gateway, captured) =
        test_model_gateway_client(Vec::new(), test_model_gateway_config())
            .expect("model gateway fixture client");
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(runtime_context),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        empty_tool_policy(),
    );
    let assembly = AuthorizedNativeAssembly::new(
        &request,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    );

    let Err(error) = assembler.assemble(assembly).await else {
        panic!("cycle must fail during nested-agent assembly")
    };
    assert_eq!(
        error.code(),
        NativeAgentAssemblyErrorCode::InvalidConfiguration
    );
    assert_eq!(calls.load(Ordering::Acquire), 2);
    assert_eq!(paths.lock().expect("runtime-context paths").len(), 2);
    assert!(captured.lock().expect("captured model requests").is_empty());
}

#[tokio::test(flavor = "current_thread")]
async fn nested_agent_tier_limit_fails_before_fetching_the_fourth_tier() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    attach_nested_agent(&mut request);
    let calls = Arc::new(AtomicUsize::new(0));
    let paths = Arc::new(Mutex::new(Vec::new()));
    let agent_b = stored_application_reference(45, 32, 42, "agent-b");
    let agent_c = stored_application_reference(46, 33, 43, "agent-c");
    let responses = VecDeque::from([
        runtime_context_response(&serde_json::json!({
            "schema_version": "elitea.runtime.elitea-client-token.v1",
            "project_id": 17,
            "token": TOKEN,
        })),
        application_version_response(
            31,
            41,
            nested_agent_version("Agent A.", "model-a", 23, vec![agent_b]),
        ),
        application_version_response(
            32,
            42,
            nested_agent_version("Agent B.", "model-b", 23, vec![agent_c]),
        ),
    ]);
    let runtime_context =
        runtime_context_client_from(responses, Arc::clone(&calls), Arc::clone(&paths));
    let (model_gateway, captured) =
        test_model_gateway_client(Vec::new(), test_model_gateway_config())
            .expect("model gateway fixture client");
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(runtime_context),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        empty_tool_policy(),
    );
    let assembly = AuthorizedNativeAssembly::new(
        &request,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    );

    let Err(error) = assembler.assemble(assembly).await else {
        panic!("fourth agent tier must fail before resolution")
    };
    assert_eq!(
        error.code(),
        NativeAgentAssemblyErrorCode::ResourceExhausted
    );
    assert_eq!(calls.load(Ordering::Acquire), 3);
    let paths = paths.lock().expect("runtime-context paths");
    assert!(paths.iter().all(|path| !path.contains("/applications/33/")));
    assert!(captured.lock().expect("captured model requests").is_empty());
}

#[tokio::test(flavor = "current_thread")]
async fn participant_project_mismatch_fails_before_child_resolution() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    attach_application_tools(&mut request, vec![adhoc_application_reference(31, 41, 99)]);
    let (runtime_context, calls) = runtime_context_client();
    let (model_gateway, captured) =
        test_model_gateway_client(Vec::new(), test_model_gateway_config())
            .expect("model gateway fixture client");
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(runtime_context),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        empty_tool_policy(),
    );
    let assembly = AuthorizedNativeAssembly::new(
        &request,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    );

    let Err(error) = assembler.assemble(assembly).await else {
        panic!("cross-project nested agent must fail before resolution")
    };
    assert_eq!(
        error.code(),
        NativeAgentAssemblyErrorCode::InvalidConfiguration
    );
    assert_eq!(calls.load(Ordering::Acquire), 1);
    assert!(captured.lock().expect("captured model requests").is_empty());
}

#[tokio::test(flavor = "current_thread")]
async fn duplicate_exact_application_identity_resolves_and_declares_once() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    attach_application_tools(
        &mut request,
        vec![
            stored_application_reference(44, 31, 41, "release-risk-agent"),
            stored_application_reference(45, 31, 41, "release-risk-agent"),
        ],
    );
    let (runtime_context, calls, _) = runtime_context_with_child();
    let (model_gateway, captured) = test_model_gateway_client(
        vec![TestModelGatewayOutcome::Response(text_response(
            "root answer",
        ))],
        test_model_gateway_config(),
    )
    .expect("model gateway fixture client");
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(runtime_context),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        empty_tool_policy(),
    );
    let assembly = AuthorizedNativeAssembly::new(
        &request,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    );
    let mut invocation = assembler
        .assemble(assembly)
        .await
        .expect("deduplicated nested-agent assembly");
    assert_eq!(calls.load(Ordering::Acquire), 2);
    let _ = invocation
        .project_start(chrono::Utc::now())
        .expect("agent start");
    let (mut native, mut projector, completion) = invocation.start().expect("native start");
    while let Some(event) = native.next_event().await.expect("native event") {
        let _ = projector.project(&event).expect("projected event");
    }
    let completed = completion.select().await.expect("selected completion");
    let _ = projector
        .finish_after_eos(completed, chrono::Utc::now())
        .expect("finished browser output");

    let captured = captured.lock().expect("captured model request");
    assert_eq!(captured.len(), 1);
    let body: serde_json::Value =
        serde_json::from_slice(&captured[0].body).expect("model request JSON");
    assert_eq!(body["tools"].as_array().map(Vec::len), Some(1));
    assert_eq!(body["tools"][0]["function"]["name"], "elitea_agent_31_v_41");
}

#[tokio::test(flavor = "current_thread")]
async fn unsupported_profile_fails_before_pat_redemption_or_model_request() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    request
        .payload
        .tools
        .push(serde_json::json!({"type": "github"}));
    let (runtime_context, context_calls) = runtime_context_client();
    let (model_gateway, captured) =
        test_model_gateway_client(Vec::new(), test_model_gateway_config())
            .expect("model gateway fixture client");
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(runtime_context),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        empty_tool_policy(),
    );
    let assembly = AuthorizedNativeAssembly::new(
        &request,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    );

    let result = assembler.assemble(assembly).await;
    assert!(result.is_err());
    assert_eq!(context_calls.load(Ordering::Acquire), 0);
    assert!(captured.lock().expect("captured model requests").is_empty());
}

#[tokio::test(flavor = "current_thread")]
async fn direct_resume_requires_restorable_sessions_before_pat_redemption() {
    let mut request = ordinary_request(AgentExecutionKind::Adhoc);
    admit_approved_resume(&mut request, "hitl_e1:fixture");
    let (runtime_context, context_calls) = runtime_context_client();
    let (model_gateway, captured) =
        test_model_gateway_client(Vec::new(), test_model_gateway_config())
            .expect("model gateway fixture client");
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(runtime_context),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        empty_tool_policy(),
    );
    let assembly = AuthorizedNativeAssembly::new(
        &request,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    );

    let Err(error) = assembler.assemble(assembly).await else {
        panic!("invocation-local continuation must remain closed")
    };
    assert_eq!(
        error.code(),
        NativeAgentAssemblyErrorCode::UnsupportedCapability
    );
    assert_eq!(context_calls.load(Ordering::Acquire), 0);
    assert!(captured.lock().expect("captured model requests").is_empty());
}
