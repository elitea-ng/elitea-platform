use std::collections::{HashSet, VecDeque};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::graph::{Checkpointer, MemoryCheckpointer};
use adk_rust::session::{GetRequest, InMemorySessionService, SessionService};
use adk_rust::tool::{BasicToolset, SimpleToolContext};
use adk_rust::{Tool, ToolContext, Toolset};
use async_trait::async_trait;
use bytes::Bytes;
use chrono::{TimeZone, Utc};
use http::{Request, Response, StatusCode, Version};
use http_body_util::Full;
use serde_json::{Value, json};
use tonic::body::Body;

use super::assembly_tests::ordinary_request;
use super::events::{
    pipeline_application_event_binding, pipeline_clarifying_event_binding,
    pipeline_hitl_event_binding, pipeline_mcp_auth_event_binding, pipeline_tool_event_binding,
};
use super::internal_tools::{ASK_USER_GUARDRAIL_TYPE, ASK_USER_TOOL_NAME};
use super::pipeline::{PipelineExecutionProfile, PipelineNativeAgentAssembler, StrictNodeToolset};
use super::request::AgentExecutionKind;
use super::runtime::{
    AssembledNativeAgentInvocation, AuthorizedNativeAssembly, NativeAgentAssembler,
    NativeAgentAssemblyErrorCode, NativeAgentCompletionSelector, PipelineNativeStart,
};
use super::session::{
    AuthorizedNativeCommandBinding, OrdinaryNativeAgentPlan, PipelineAgentCompletion,
};
use crate::protocol::control::test_runtime_context_authority;
use crate::protocol::elitea::runtime::v1::NodeEventV1;
use crate::protocol::node_event::encode_current_node_event_json;
use crate::toolkits::{
    McpConnector, McpMaterializationError, RemoteMcpConfig, ToolAdmissionPolicy,
    mcp_authorization_required_fixture,
};
use crate::transport::model_facade::ModelFacade;
use crate::transport::openai_compatible_facade::{
    CapturedModelRequests, TestModelGatewayOutcome, test_model_gateway_client,
    test_model_gateway_config, test_model_gateway_response,
};
use crate::transport::platform_client::PlatformClient;
use crate::transport::runtime_context::{
    RuntimeContextClient, RuntimeContextConfig, RuntimeContextRpc, RuntimeContextTransportError,
};

const PIPELINE: &str = r"
state:
  answer: string
entry_point: review
nodes:
  - id: review
    type: hitl
    user_message:
      type: fixed
      value: Review the draft.
    routes:
      approve: END
      reject: END
";

const STATE_MODIFIER_PIPELINE: &str = r#"
state:
  input:
    type: str
  prefix:
    type: str
    value: Hello
  final_text:
    type: str
entry_point: transform
nodes:
  - id: transform
    type: state_modifier
    template: "{{ prefix }}, {{ input }}"
    input: [prefix, input]
    output: [final_text]
    transition: END
"#;

const PRINTER_PIPELINE: &str = r#"
state:
  answer:
    type: str
    value: Draft ready
  messages: list
entry_point: show
nodes:
  - id: show
    type: printer
    input_mapping:
      printer: {type: variable, value: answer}
    final_message: Continue when ready.
    transition: capture
  - id: capture
    type: state_modifier
    template: "{{ input }}"
    input: [input]
    output: [answer]
    transition: END
"#;

fn pipeline_request() -> super::request::AgentExecutionRequest {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    let version = request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture");
    version.insert("agent_type".to_owned(), json!("pipeline"));
    version.insert("instructions".to_owned(), json!(PIPELINE));
    request
}

fn printer_pipeline_request(
    input: &str,
    should_continue: bool,
) -> super::request::AgentExecutionRequest {
    let mut request = pipeline_request();
    request.payload.user_input = super::request::UserInput::Text(input.to_owned());
    request.payload.should_continue = should_continue;
    request.payload.hitl_resume = false;
    let version = request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture");
    version.insert("instructions".to_owned(), json!(PRINTER_PIPELINE));
    request
}

fn llm_pipeline_request(
    toolkit_alias: &str,
    configured_tools: &[&str],
    node_tools: &[&str],
) -> super::request::AgentExecutionRequest {
    let mut request = pipeline_request();
    let tool_names = node_tools
        .iter()
        .map(|name| format!("{name:?}"))
        .collect::<Vec<_>>()
        .join(", ");
    let definition = format!(
        "state:\n  answer: str\n  messages: list\nentry_point: answer\nnodes:\n  - id: answer\n    type: llm\n    output: [answer, messages]\n    tool_names:\n      {toolkit_alias}: [{tool_names}]\n    transition: END\n"
    );
    let version = request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture");
    version.insert("instructions".to_owned(), json!(definition));
    version.insert(
        "tools".to_owned(),
        json!([{
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
                "selected_tools": configured_tools
            }
        }]),
    );
    request
}

fn toolkit_pipeline_request(
    toolkit_alias: &str,
    configured_tools: &[&str],
    node_tool: &str,
) -> super::request::AgentExecutionRequest {
    let mut request = pipeline_request();
    let definition = format!(
        "state:\n  records: dict\n  messages: list\nentry_point: direct\nnodes:\n  - id: direct\n    type: toolkit\n    toolkit_name: {toolkit_alias:?}\n    tool: {node_tool:?}\n    input_mapping:\n      repository: {{type: fixed, value: group/project}}\n    output: [records, messages]\n    transition: END\n"
    );
    let version = request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture");
    version.insert("instructions".to_owned(), json!(definition));
    version.insert(
        "tools".to_owned(),
        json!([{
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
                "selected_tools": configured_tools
            }
        }]),
    );
    request
}

fn mcp_pipeline_request(
    toolkit_alias: &str,
    selected_tools: &[&str],
    node_tool: &str,
) -> super::request::AgentExecutionRequest {
    let mut request = pipeline_request();
    let definition = format!(
        "state:\n  records: dict\n  messages: list\nentry_point: direct\nnodes:\n  - id: direct\n    type: mcp\n    toolkit_name: {toolkit_alias:?}\n    tool: {node_tool:?}\n    input_mapping:\n      release: {{type: fixed, value: '1.2'}}\n    output: [records, messages]\n    structured_output: true\n    transition: END\n"
    );
    let version = request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture");
    version.insert("instructions".to_owned(), json!(definition));
    version.insert(
        "tools".to_owned(),
        json!([{
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
                "selected_tools": selected_tools,
                "enable_caching": true,
                "cache_ttl": 300,
                "ssl_verify": true
            }
        }]),
    );
    request
}

fn llm_mcp_pipeline_request(
    toolkit_alias: &str,
    selected_tools: &[&str],
    node_tools: &[&str],
) -> super::request::AgentExecutionRequest {
    let mut request = mcp_pipeline_request(toolkit_alias, selected_tools, "lookup_release");
    let tool_names = node_tools
        .iter()
        .map(|name| format!("{name:?}"))
        .collect::<Vec<_>>()
        .join(", ");
    let definition = format!(
        "state:\n  answer: str\n  messages: list\nentry_point: answer\nnodes:\n  - id: answer\n    type: llm\n    output: [answer, messages]\n    tool_names:\n      {toolkit_alias}: [{tool_names}]\n    transition: END\n"
    );
    request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture")
        .insert("instructions".to_owned(), json!(definition));
    request
}

fn colliding_llm_mcp_pipeline_request() -> super::request::AgentExecutionRequest {
    let mut request = pipeline_request();
    let version = request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture");
    version.insert(
        "instructions".to_owned(),
        json!(
            "state:\n  answer: str\n  messages: list\nentry_point: answer\nnodes:\n  - id: answer\n    type: llm\n    output: [answer, messages]\n    tool_names:\n      release intelligence: [lookup_release]\n      audit intelligence: [lookup_release]\n    transition: END\n"
        ),
    );
    version.insert(
        "tools".to_owned(),
        json!([
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
        ]),
    );
    request
}

fn ask_user_llm_pipeline_request() -> super::request::AgentExecutionRequest {
    let mut request = pipeline_request();
    request.payload.internal_tools = vec![ASK_USER_TOOL_NAME.to_owned()];
    let version = request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture");
    version.insert(
        "instructions".to_owned(),
        json!(
            "state:\n  answer: str\n  messages: list\nentry_point: answer\nnodes:\n  - id: answer\n    type: llm\n    output: [answer, messages]\n    tool_names:\n      ask_user: [\"ask_user\"]\n    transition: END\n"
        ),
    );
    request
}

fn agent_pipeline_request(
    alias: &str,
    child_agent_type: &str,
) -> super::request::AgentExecutionRequest {
    let mut request = pipeline_request();
    let definition = format!(
        "state:\n  answer: str\n  messages: list\nentry_point: delegate\nnodes:\n  - id: delegate\n    type: agent\n    tool: {alias:?}\n    input_mapping:\n      task: {{type: fixed, value: 'Summarize the release'}}\n    output: [answer, messages]\n    transition: END\n"
    );
    let version = request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture");
    version.insert("instructions".to_owned(), json!(definition));
    version.insert(
        "tools".to_owned(),
        json!([{
            "id": 44,
            "type": "application",
            "name": "release-agent",
            "description": "Summarizes one release.",
            "author_id": 11,
            "settings": {"application_id": 3, "application_version_id": 4},
            "meta": {},
            "created_at": "2026-08-21T10:00:00Z",
            "toolkit_name": "release-agent",
            "author": null,
            "agent_type": child_agent_type,
            "online": null,
            "icon_meta": null,
            "variables": [],
            "is_pinned": false,
            "indexes_count": null
        }]),
    );
    request
}

struct PipelineRuntimeContextFixture {
    responses: Mutex<VecDeque<Response<Body>>>,
    calls: Arc<AtomicUsize>,
    paths: Arc<Mutex<Vec<String>>>,
}

#[async_trait]
impl RuntimeContextRpc for PipelineRuntimeContextFixture {
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

fn runtime_response(value: &Value) -> Response<Body> {
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

type PipelineChildRuntime = (
    Arc<PlatformClient>,
    Arc<ModelFacade>,
    Arc<AtomicUsize>,
    Arc<Mutex<Vec<String>>>,
);

fn pipeline_child_runtime() -> PipelineChildRuntime {
    pipeline_runtime(
        &json!({
            "agent_type": "pipeline",
            "instructions": "state:\n  input: str\n  messages: list\n  answer: str\nentry_point: answer\nnodes:\n  - id: answer\n    type: llm\n    input_mapping:\n      task: {type: variable, value: input}\n    input: [messages]\n    output: [answer, messages]\n    transition: END\n",
            "meta": {},
            "variables": [],
            "tools": [],
            "llm_settings": null
        }),
        vec![TestModelGatewayOutcome::Response(pipeline_text_response(
            "Child: Summarize the release",
        ))],
    )
}

fn pipeline_data_child_runtime() -> PipelineChildRuntime {
    pipeline_runtime(
        &json!({
            "agent_type": "pipeline",
            "instructions": "state:\n  input: str\n  messages: list\n  answer: str\nentry_point: answer\nnodes:\n  - id: answer\n    type: state_modifier\n    template: 'Child: {{ input }}'\n    input: [input]\n    output: [answer]\n    transition: END\n",
            "meta": {},
            "variables": [],
            "tools": [],
            "llm_settings": null
        }),
        Vec::new(),
    )
}

fn pipeline_hitl_child_runtime() -> PipelineChildRuntime {
    pipeline_runtime_cycles(
        &json!({
            "agent_type": "pipeline",
            "instructions": "state:\n  input: str\n  messages: list\n  answer: str\nentry_point: approve\nnodes:\n  - id: approve\n    type: hitl\n    input: [input]\n    user_message:\n      type: fstring\n      value: 'Approve {input}.'\n    routes:\n      approve: done\n      reject: END\n  - id: done\n    type: state_modifier\n    template: 'Approved: {{ input }}'\n    input: [input]\n    output: [answer]\n    transition: END\n",
            "meta": {},
            "variables": [],
            "tools": [],
            "llm_settings": null
        }),
        Vec::new(),
        2,
    )
}

fn sensitive_pipeline_child_runtime() -> (PipelineChildRuntime, CapturedModelRequests) {
    pipeline_runtime_cycles_with_capture(
        &json!({
            "agent_type": "pipeline",
            "instructions": "state:\n  input: str\n  messages: list\n  answer: str\nentry_point: answer\nnodes:\n  - id: answer\n    type: llm\n    input_mapping:\n      task: {type: variable, value: input}\n    input: [messages]\n    output: [answer, messages]\n    tool_names:\n      release intelligence: ['lookup_release']\n    transition: END\n",
            "meta": {},
            "variables": [],
            "tools": [{
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
            }],
            "llm_settings": null
        }),
        vec![
            TestModelGatewayOutcome::Response(pipeline_mcp_tool_call_response()),
            TestModelGatewayOutcome::Response(pipeline_text_response("nested block handled")),
        ],
        2,
    )
}

fn direct_agent_pipeline_runtime() -> PipelineChildRuntime {
    pipeline_runtime(
        &json!({
            "agent_type": "agent",
            "instructions": "Return the delegated release summary.",
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
        vec![TestModelGatewayOutcome::Response(pipeline_text_response(
            "direct child summary",
        ))],
    )
}

fn sensitive_direct_agent_pipeline_runtime() -> PipelineChildRuntime {
    pipeline_runtime_cycles(
        &json!({
            "agent_type": "agent",
            "instructions": "Read the release evidence and summarize it.",
            "meta": {},
            "variables": [],
            "tools": [{
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
            }],
            "llm_settings": {
                "model_name": "child-model",
                "model_project_id": 23,
                "max_tokens": 2048,
                "reasoning_effort": null,
                "temperature": 0.2,
                "openai_compatible": true
            }
        }),
        vec![
            TestModelGatewayOutcome::Response(pipeline_mcp_tool_call_response()),
            TestModelGatewayOutcome::Response(pipeline_text_response("child resumed summary")),
        ],
        2,
    )
}

fn pipeline_runtime(
    version_details: &Value,
    outcomes: Vec<TestModelGatewayOutcome>,
) -> PipelineChildRuntime {
    pipeline_runtime_cycles(version_details, outcomes, 1)
}

fn pipeline_runtime_cycles(
    version_details: &Value,
    outcomes: Vec<TestModelGatewayOutcome>,
    cycles: usize,
) -> PipelineChildRuntime {
    pipeline_runtime_cycles_with_capture(version_details, outcomes, cycles).0
}

fn pipeline_runtime_cycles_with_capture(
    version_details: &Value,
    outcomes: Vec<TestModelGatewayOutcome>,
    cycles: usize,
) -> (PipelineChildRuntime, CapturedModelRequests) {
    let calls = Arc::new(AtomicUsize::new(0));
    let paths = Arc::new(Mutex::new(Vec::new()));
    let responses = (0..cycles)
        .flat_map(|_| {
            [
                runtime_response(&json!({
                    "schema_version": "elitea.runtime.elitea-client-token.v1",
                    "project_id": 17,
                    "token": "ephemeral-pipeline-token"
                })),
                runtime_response(&json!({
                    "schema_version": "elitea.runtime.application-version.v1",
                    "project_id": 17,
                    "application_id": 3,
                    "version_id": 4,
                    "version_details": version_details
                })),
            ]
        })
        .collect::<VecDeque<_>>();
    pipeline_runtime_from_responses_with_capture(responses, outcomes, calls, paths)
}

fn pipeline_runtime_from_responses_with_capture(
    responses: VecDeque<Response<Body>>,
    outcomes: Vec<TestModelGatewayOutcome>,
    calls: Arc<AtomicUsize>,
    paths: Arc<Mutex<Vec<String>>>,
) -> (PipelineChildRuntime, CapturedModelRequests) {
    let runtime = RuntimeContextClient::with_rpc(
        PipelineRuntimeContextFixture {
            responses: Mutex::new(responses),
            calls: Arc::clone(&calls),
            paths: Arc::clone(&paths),
        },
        RuntimeContextConfig {
            origin: "https://content.internal".to_owned(),
            deadline: Duration::from_secs(1),
            max_response_bytes: 32 * 1_024,
            max_application_response_bytes: 1_024 * 1_024,
            max_attachment_response_bytes: 1_024 * 1_024,
        },
    )
    .expect("pipeline runtime-context fixture");
    let (gateway, captured) = test_model_gateway_client(outcomes, test_model_gateway_config())
        .expect("unused pipeline model gateway");
    (
        (
            Arc::new(PlatformClient::new(Arc::new(runtime))),
            Arc::new(ModelFacade::from_gateway(gateway)),
            calls,
            paths,
        ),
        captured,
    )
}

fn recursive_sensitive_pipeline_runtime() -> PipelineChildRuntime {
    recursive_sensitive_pipeline_runtime_with_capture().0
}

fn recursive_sensitive_pipeline_runtime_with_capture()
-> (PipelineChildRuntime, CapturedModelRequests) {
    let calls = Arc::new(AtomicUsize::new(0));
    let paths = Arc::new(Mutex::new(Vec::new()));
    let responses = (0..3)
        .flat_map(|_| {
            [
                runtime_response(&json!({
                    "schema_version": "elitea.runtime.elitea-client-token.v1",
                    "project_id": 17,
                    "token": "ephemeral-pipeline-token"
                })),
                application_runtime_response(
                    3,
                    4,
                    &json!({
                        "agent_type": "agent",
                        "instructions": "Delegate the two release checks in parallel.",
                        "meta": {},
                        "variables": [],
                        "tools": [saved_application_tool(32, 42, "name-resolver")],
                        "llm_settings": {
                            "model_name": "orchestrator-model",
                            "model_project_id": 23,
                            "max_tokens": 2048,
                            "reasoning_effort": null,
                            "temperature": 0.2,
                            "openai_compatible": true
                        }
                    }),
                ),
                application_runtime_response(
                    32,
                    42,
                    &json!({
                        "agent_type": "agent",
                        "instructions": "Read release evidence and return one result.",
                        "meta": {},
                        "variables": [],
                        "tools": [{
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
                        }],
                        "llm_settings": {
                            "model_name": "resolver-model",
                            "model_project_id": 24,
                            "max_tokens": 2048,
                            "reasoning_effort": null,
                            "temperature": 0.2,
                            "openai_compatible": true
                        }
                    }),
                ),
            ]
        })
        .collect::<VecDeque<_>>();
    let outcomes = vec![
        model_response_for(
            "orchestrator-model",
            pipeline_parallel_saved_agent_call_response(),
        ),
        model_response_for("resolver-model", pipeline_mcp_tool_call_response()),
        model_response_for("resolver-model", pipeline_mcp_tool_call_response()),
        model_response_for(
            "resolver-model",
            pipeline_text_response("first resolved leaf"),
        ),
        model_response_for(
            "resolver-model",
            pipeline_text_response("second resolved leaf"),
        ),
        model_response_for(
            "orchestrator-model",
            pipeline_text_response("parallel orchestrator summary"),
        ),
    ];
    pipeline_runtime_from_responses_with_capture(responses, outcomes, calls, paths)
}

fn mixed_guardrail_pipeline_runtime_with_capture() -> (PipelineChildRuntime, CapturedModelRequests)
{
    let calls = Arc::new(AtomicUsize::new(0));
    let paths = Arc::new(Mutex::new(Vec::new()));
    let responses = (0..2)
        .flat_map(|_| {
            [
                runtime_response(&json!({
                    "schema_version": "elitea.runtime.elitea-client-token.v1",
                    "project_id": 17,
                    "token": "ephemeral-pipeline-token"
                })),
                application_runtime_response(
                    3,
                    4,
                    &json!({
                        "agent_type": "agent",
                        "instructions": "Delegate both release checks in parallel.",
                        "meta": {},
                        "variables": [],
                        "tools": [
                            saved_application_tool_with_id(45, 32, 42, "sensitive-resolver"),
                            saved_application_tool_with_id(46, 33, 43, "auth-resolver"),
                        ],
                        "llm_settings": {
                            "model_name": "mixed-orchestrator-model",
                            "model_project_id": 23,
                            "max_tokens": 2048,
                            "reasoning_effort": null,
                            "temperature": 0.2,
                            "openai_compatible": true
                        }
                    }),
                ),
                application_runtime_response(
                    32,
                    42,
                    &mixed_guardrail_child_version(
                        "sensitive-model",
                        "sensitive evidence",
                        "https://sensitive.example.invalid/v1/mcp",
                        "lookup_sensitive",
                    ),
                ),
                application_runtime_response(
                    33,
                    43,
                    &mixed_guardrail_child_version(
                        "auth-model",
                        "authorized evidence",
                        "https://auth.example.invalid/v1/mcp",
                        "lookup_auth",
                    ),
                ),
            ]
        })
        .collect::<VecDeque<_>>();
    let outcomes = vec![
        model_response_for(
            "mixed-orchestrator-model",
            pipeline_mixed_saved_agent_call_response(),
        ),
        model_response_for(
            "sensitive-model",
            pipeline_named_mcp_tool_call_response("call_sensitive_mcp", "lookup_sensitive"),
        ),
        model_response_for(
            "auth-model",
            pipeline_named_mcp_tool_call_response("call_auth_mcp", "lookup_auth"),
        ),
        model_response_for(
            "sensitive-model",
            pipeline_text_response("sensitive leaf complete"),
        ),
        model_response_for("auth-model", pipeline_text_response("auth leaf complete")),
        model_response_for(
            "mixed-orchestrator-model",
            pipeline_text_response("mixed guardrails complete"),
        ),
    ];
    pipeline_runtime_from_responses_with_capture(responses, outcomes, calls, paths)
}

fn mixed_guardrail_child_version(
    model_name: &str,
    toolkit_name: &str,
    server_url: &str,
    tool_name: &str,
) -> Value {
    json!({
        "agent_type": "agent",
        "instructions": "Read the selected release evidence.",
        "meta": {},
        "variables": [],
        "tools": [{
            "id": 92,
            "type": "mcp",
            "toolkit_name": toolkit_name,
            "settings": {
                "url": server_url,
                "headers": null,
                "client_id": null,
                "client_secret": null,
                "scopes": null,
                "timeout": 30,
                "selected_tools": [tool_name],
                "enable_caching": true,
                "cache_ttl": 300,
                "ssl_verify": true
            }
        }],
        "llm_settings": {
            "model_name": model_name,
            "model_project_id": 24,
            "max_tokens": 2048,
            "reasoning_effort": null,
            "temperature": 0.2,
            "openai_compatible": true
        }
    })
}

fn application_runtime_response(
    application_id: u64,
    version_id: u64,
    version_details: &Value,
) -> Response<Body> {
    runtime_response(&json!({
        "schema_version": "elitea.runtime.application-version.v1",
        "project_id": 17,
        "application_id": application_id,
        "version_id": version_id,
        "version_details": version_details
    }))
}

async fn collect_pipeline_pause(
    mut invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
) -> (Vec<Value>, Option<adk_rust::Event>) {
    invocation
        .project_start(timestamp(0))
        .expect("pipeline pause browser start");
    let (mut run, mut projector, _completion) = invocation.start().expect("pipeline pause start");
    let mut browser_interrupts = Vec::new();
    let mut graph_interrupt = None;
    while let Some(event) = run.next_event().await.expect("pipeline pause event") {
        if event
            .provider_metadata
            .contains_key(adk_rust::graph::interrupt::INTERRUPT_METADATA_KEY)
        {
            graph_interrupt = Some(event.clone());
        }
        browser_interrupts.extend(
            projector
                .project(&event)
                .expect("pipeline pause projection")
                .into_iter()
                .map(|event| current(&event))
                .filter(|event| event["type"] == "agent_hitl_interrupt"),
        );
    }
    assert!(projector.is_paused());
    (browser_interrupts, graph_interrupt)
}

async fn collect_pipeline_pause_events(
    mut invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
) -> (Vec<Value>, Option<adk_rust::Event>) {
    invocation
        .project_start(timestamp(0))
        .expect("pipeline pause browser start");
    let (mut run, mut projector, _completion) = invocation.start().expect("pipeline pause start");
    let mut browser_events = Vec::new();
    let mut graph_interrupt = None;
    while let Some(event) = run.next_event().await.expect("pipeline pause event") {
        if event
            .provider_metadata
            .contains_key(adk_rust::graph::interrupt::INTERRUPT_METADATA_KEY)
        {
            graph_interrupt = Some(event.clone());
        }
        browser_events.extend(
            projector
                .project(&event)
                .expect("pipeline pause projection")
                .into_iter()
                .map(|event| current(&event)),
        );
    }
    assert!(projector.is_paused());
    (browser_events, graph_interrupt)
}

async fn collect_pipeline_completion(
    mut invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
) -> Vec<Value> {
    invocation
        .project_start(timestamp(1))
        .expect("pipeline resume browser start");
    let (mut run, mut projector, completion) = invocation.start().expect("pipeline resume start");
    let mut browser = Vec::new();
    while let Some(event) = run.next_event().await.expect("pipeline resume event") {
        browser.extend(
            projector
                .project(&event)
                .expect("pipeline resume projection")
                .into_iter()
                .map(|event| current(&event)),
        );
    }
    let completed = completion
        .select()
        .await
        .expect("pipeline resume completion");
    browser.extend(
        projector
            .finish_after_eos(completed, timestamp(2))
            .expect("pipeline resumed browser completion")
            .into_iter()
            .map(|event| current(&event)),
    );
    browser
}

fn pipeline_application_resume_request(
    interrupt_ids: Vec<String>,
    action: &str,
    value: &str,
    digest: [u8; 32],
) -> super::request::AgentExecutionRequest {
    let single = interrupt_ids.len() == 1;
    let mut resume = agent_pipeline_request("release-agent", "agent");
    resume.binding.request_content_digest = digest;
    resume.payload.should_continue = true;
    resume.payload.hitl_resume = true;
    resume.payload.hitl_action = single.then(|| action.to_owned());
    resume.payload.hitl_value = single.then(|| value.to_owned());
    resume.payload.hitl_decisions = interrupt_ids
        .into_iter()
        .map(|interrupt_id| {
            json!({
                "interrupt_id": interrupt_id,
                "tool_call_id": "call_mcp",
                "action": action,
                "value": value,
            })
        })
        .collect();
    resume
}

fn pipeline_application_authorization_resume_request(
    interrupt_ids: Vec<String>,
    action: &str,
    digest: [u8; 32],
) -> super::request::AgentExecutionRequest {
    let single = interrupt_ids.len() == 1;
    let mut resume = agent_pipeline_request("release-agent", "agent");
    resume.binding.request_content_digest = digest;
    resume.payload.should_continue = true;
    resume.payload.hitl_resume = true;
    resume.payload.hitl_action = single.then(|| action.to_owned());
    resume.payload.hitl_value = single.then(String::new);
    resume.payload.hitl_decisions = interrupt_ids
        .into_iter()
        .map(|interrupt_id| {
            json!({
                "interrupt_id": interrupt_id,
                "tool_call_id": "call_mcp",
                "guardrail_type": "mcp_auth",
                "action": action,
                "value": "",
            })
        })
        .collect();
    if action == "authorize" {
        resume.payload.mcp_tokens.insert(
            "https://mcp.example.invalid/v1/mcp".to_owned(),
            json!({"access_token": "runtime-secret"}),
        );
    } else {
        resume
            .payload
            .user_declined_mcp_servers
            .push(json!({"server_url": "https://mcp.example.invalid/v1/mcp"}));
    }
    resume
}

fn saved_application_tool(application_id: u64, version_id: u64, alias: &str) -> Value {
    saved_application_tool_with_id(45, application_id, version_id, alias)
}

fn saved_application_tool_with_id(
    id: u64,
    application_id: u64,
    version_id: u64,
    alias: &str,
) -> Value {
    json!({
        "id": id,
        "type": "application",
        "name": alias,
        "description": "Resolve one release name.",
        "author_id": 11,
        "settings": {
            "application_id": application_id,
            "application_version_id": version_id
        },
        "meta": {},
        "created_at": "2026-08-21T10:00:00Z",
        "toolkit_name": alias,
        "author": null,
        "agent_type": "agent",
        "online": null,
        "icon_meta": null,
        "variables": [],
        "is_pinned": false,
        "indexes_count": null
    })
}

fn model_response_for(model: &'static str, response: Response<Body>) -> TestModelGatewayOutcome {
    TestModelGatewayOutcome::ResponseForModel { model, response }
}

fn pipeline_parallel_saved_agent_call_response() -> Response<Body> {
    let raw = concat!(
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_first\",\"type\":\"function\",\"function\":{\"name\":\"elitea_agent_32_v_42\",\"arguments\":\"{\\\"task\\\":\\\"Resolve first release\\\"}\"}},{\"index\":1,\"id\":\"call_last\",\"type\":\"function\",\"function\":{\"name\":\"elitea_agent_32_v_42\",\"arguments\":\"{\\\"task\\\":\\\"Resolve second release\\\"}\"}}]},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
        "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n",
        "data: [DONE]\n\n",
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn pipeline_mixed_saved_agent_call_response() -> Response<Body> {
    let raw = concat!(
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_sensitive_child\",\"type\":\"function\",\"function\":{\"name\":\"elitea_agent_32_v_42\",\"arguments\":\"{\\\"task\\\":\\\"Resolve sensitive evidence\\\"}\"}},{\"index\":1,\"id\":\"call_auth_child\",\"type\":\"function\",\"function\":{\"name\":\"elitea_agent_33_v_43\",\"arguments\":\"{\\\"task\\\":\\\"Resolve authorized evidence\\\"}\"}}]},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
        "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n",
        "data: [DONE]\n\n",
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn pipeline_mcp_tool_call_response() -> Response<Body> {
    let raw = concat!(
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_mcp\",\"type\":\"function\",\"function\":{\"name\":\"lookup_release\",\"arguments\":\"{\\\"release\\\":\\\"1.2\\\"}\"}}]},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
        "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n",
        "data: [DONE]\n\n",
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn pipeline_colliding_mcp_tool_call_response() -> Response<Body> {
    let raw = concat!(
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_release\",\"type\":\"function\",\"function\":{\"name\":\"release_intelligence__lookup_release\",\"arguments\":\"{\\\"release\\\":\\\"1.2\\\"}\"}},{\"index\":1,\"id\":\"call_audit\",\"type\":\"function\",\"function\":{\"name\":\"audit_intelligence__lookup_release\",\"arguments\":\"{\\\"release\\\":\\\"1.2\\\"}\"}}]},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
        "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n",
        "data: [DONE]\n\n",
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn pipeline_ask_user_call_response() -> Response<Body> {
    let raw = concat!(
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_ask_user\",\"type\":\"function\",\"function\":{\"name\":\"ask_user\",\"arguments\":\"{\\\"questions\\\":[{\\\"question\\\":\\\"Which environment should I use?\\\",\\\"header\\\":\\\"Environment\\\",\\\"options\\\":[{\\\"label\\\":\\\"Staging\\\"},{\\\"label\\\":\\\"Production\\\"}]}]}\"}}]},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
        "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n",
        "data: [DONE]\n\n",
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn pipeline_named_mcp_tool_call_response(call_id: &str, tool_name: &str) -> Response<Body> {
    let raw = format!(
        "data: {{\"choices\":[{{\"delta\":{{\"tool_calls\":[{{\"index\":0,\"id\":{call_id:?},\"type\":\"function\",\"function\":{{\"name\":{tool_name:?},\"arguments\":\"{{\\\"release\\\":\\\"1.2\\\"}}\"}}}}]}},\"finish_reason\":null}}]}}\n\ndata: {{\"choices\":[{{\"delta\":{{}},\"finish_reason\":\"tool_calls\"}}]}}\n\ndata: {{\"choices\":[],\"usage\":{{\"prompt_tokens\":3,\"completion_tokens\":2}}}}\n\ndata: [DONE]\n\n"
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn sensitive_pipeline_mcp_policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(runtime_tool_policy(&json!({
        "toolkit_security": {
            "sensitive_tools": {"mcp": ["lookup_release"]},
            "sensitive_action_company_name": "Example Org"
        }
    })))
}

fn pipeline_text_response(text: &str) -> Response<Body> {
    let raw = format!(
        "data: {{\"choices\":[{{\"delta\":{{\"role\":\"assistant\",\"content\":{text:?}}},\"finish_reason\":null}}]}}\n\ndata: {{\"choices\":[{{\"delta\":{{}},\"finish_reason\":\"stop\"}}]}}\n\ndata: {{\"choices\":[],\"usage\":{{\"prompt_tokens\":3,\"completion_tokens\":2}}}}\n\ndata: [DONE]\n\n"
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn runtime_tool_policy(value: &Value) -> ToolAdmissionPolicy {
    let runtime = value.as_object().expect("runtime policy object");
    ToolAdmissionPolicy::from_runtime_config(runtime).expect("runtime tool policy")
}

fn authorized(request: &super::request::AgentExecutionRequest) -> AuthorizedNativeAssembly<'_> {
    AuthorizedNativeAssembly::new(
        request,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    )
}

fn admission_error(
    result: Result<
        super::runtime::AdmittedPipelineNativeAssembly<'_>,
        super::runtime::NativeAgentAssemblyError,
    >,
) -> super::runtime::NativeAgentAssemblyError {
    match result {
        Ok(_) => panic!("invalid pipeline admission succeeded"),
        Err(error) => error,
    }
}

fn timestamp(second: u32) -> chrono::DateTime<Utc> {
    Utc.with_ymd_and_hms(2026, 8, 20, 12, 0, second)
        .single()
        .expect("fixture timestamp")
}

fn current(event: &NodeEventV1) -> Value {
    serde_json::from_slice(
        &encode_current_node_event_json(event).expect("valid projected browser event"),
    )
    .expect("projected event JSON")
}

fn private_pipeline_session_id(request: &super::request::AgentExecutionRequest) -> String {
    let profile = PipelineExecutionProfile::validate(request, false).expect("pipeline profile");
    OrdinaryNativeAgentPlan::from_authorized_pipeline(
        request,
        profile.shell(),
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
        false,
        false,
    )
    .expect("pipeline plan")
    .session_id()
    .to_owned()
}

fn resume_request(interrupt_id: &str) -> super::request::AgentExecutionRequest {
    let mut request = pipeline_request();
    request.payload.should_continue = true;
    request.payload.hitl_resume = true;
    request.payload.hitl_action = Some("approve".to_owned());
    request.payload.hitl_value = Some(String::new());
    request.payload.hitl_decisions = vec![json!({
        "interrupt_id": interrupt_id,
        "tool_call_id": "",
        "action": "approve",
        "value": ""
    })];
    request.payload.user_input = super::request::UserInput::Text(
        "this transport marker must not become resumed graph input".to_owned(),
    );
    request
}

#[test]
fn authorized_pipeline_admission_is_distinct_from_direct_agent_admission() {
    let pipeline = pipeline_request();
    let admitted = authorized(&pipeline)
        .admit_pipeline()
        .expect("stored pipeline admission");
    assert!(!admitted.is_resume());
    assert_eq!(admitted.profile().definition().entry_point(), "review");
    assert_eq!(admitted.profile().definition().node_count(), 1);

    let direct = ordinary_request(AgentExecutionKind::Application);
    let error = admission_error(authorized(&direct).admit_pipeline());
    assert_eq!(
        error.code(),
        NativeAgentAssemblyErrorCode::UnsupportedCapability
    );
}

#[test]
fn pipeline_resume_admits_distinct_node_and_tool_decision_envelopes_before_checkpoint_join() {
    let mut pipeline = pipeline_request();
    pipeline.payload.should_continue = true;
    pipeline.payload.hitl_resume = true;
    pipeline.payload.hitl_action = Some("approve".to_owned());
    pipeline.payload.hitl_value = Some(String::new());
    pipeline.payload.hitl_decisions = vec![json!({
        "interrupt_id": "hitl_g1:checkpoint-bound",
        "tool_call_id": "",
        "action": "approve",
        "value": ""
    })];
    let admitted = authorized(&pipeline)
        .admit_pipeline()
        .expect("pipeline HITL admission");
    assert!(admitted.is_resume());

    pipeline.payload.hitl_decisions[0]["tool_call_id"] = json!("tool-call-1");
    let admitted = authorized(&pipeline)
        .admit_pipeline()
        .expect("Toolkit-call decision envelope admission");
    assert!(admitted.is_resume());
}

#[test]
fn pipeline_mcp_authorization_envelope_is_not_admitted_as_printer_text() {
    let mut pipeline = pipeline_request();
    pipeline.payload.should_continue = true;
    pipeline.payload.hitl_resume = false;
    pipeline.payload.mcp_tokens.insert(
        "https://mcp.example.invalid/v1/mcp".to_owned(),
        json!({"access_token": "runtime-secret"}),
    );
    let admitted = authorized(&pipeline)
        .admit_pipeline()
        .expect("MCP authorization admission");
    let (_, _, _, _, start, _, _, _) = admitted.into_parts();
    assert!(matches!(start, PipelineNativeStart::McpAuthorization(_)));

    let mut malformed = pipeline_request();
    malformed.payload.should_continue = true;
    malformed.payload.ignored_mcp_servers = vec![json!({
        "server_url": "https://mcp.example.invalid/v1/mcp"
    })];
    let error = admission_error(authorized(&malformed).admit_pipeline());
    assert_eq!(error.code(), NativeAgentAssemblyErrorCode::InvalidInput);
}

#[test]
fn malformed_pipeline_tools_fail_and_llm_yaml_is_admitted_without_authority() {
    let mut with_tools = pipeline_request();
    with_tools
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture")
        .insert("tools".to_owned(), json!([{"type": "github"}]));
    let error = admission_error(authorized(&with_tools).admit_pipeline());
    assert_eq!(error.code(), NativeAgentAssemblyErrorCode::InvalidInput);

    let mut llm_node = pipeline_request();
    llm_node
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture")
        .insert(
            "instructions".to_owned(),
            json!("entry_point: draft\nnodes:\n  - id: draft\n    type: llm\n"),
        );
    let admitted = authorized(&llm_node)
        .admit_pipeline()
        .expect("authority-free LLM definition admission");
    assert_eq!(admitted.profile().definition().node_count(), 1);
}

#[test]
fn llm_tool_scope_is_exact_sensitive_tools_bind_and_blocked_authority_fails_closed() {
    let allowed = llm_pipeline_request(
        "release_repository",
        &["list_branches_in_repo"],
        &["list_branches_in_repo"],
    );
    let empty_policy = runtime_tool_policy(&json!({}));
    authorized(&allowed)
        .admit_pipeline_with_policy(&empty_policy)
        .expect("exact frozen LLM tool scope");

    let unknown_alias = llm_pipeline_request(
        "other_repository",
        &["list_branches_in_repo"],
        &["list_branches_in_repo"],
    );
    let error =
        admission_error(authorized(&unknown_alias).admit_pipeline_with_policy(&empty_policy));
    assert_eq!(error.code(), NativeAgentAssemblyErrorCode::InvalidInput);

    let outside_selection = llm_pipeline_request(
        "release_repository",
        &["get_issues"],
        &["list_branches_in_repo"],
    );
    let error =
        admission_error(authorized(&outside_selection).admit_pipeline_with_policy(&empty_policy));
    assert_eq!(error.code(), NativeAgentAssemblyErrorCode::InvalidInput);

    for policy in [
        runtime_tool_policy(&json!({
            "toolkit_security": {"blocked_toolkits": ["gitlab_org"]}
        })),
        runtime_tool_policy(&json!({
            "toolkit_security": {
                "blocked_tools": {"gitlab_org": ["list_branches_in_repo"]}
            }
        })),
    ] {
        let error = admission_error(authorized(&allowed).admit_pipeline_with_policy(&policy));
        assert_eq!(
            error.code(),
            NativeAgentAssemblyErrorCode::UnsupportedCapability
        );
    }

    let sensitive = runtime_tool_policy(&json!({
        "toolkit_security": {
            "sensitive_tools": {"gitlab_org": ["list_branches_in_repo"]}
        }
    }));
    authorized(&allowed)
        .admit_pipeline_with_policy(&sensitive)
        .expect("sensitive LLM tool binds to native confirmation");
}

#[test]
fn toolkit_node_scope_is_exact_and_sensitive_read_is_bound_for_graph_confirmation() {
    let allowed = toolkit_pipeline_request("release_repository", &["get_issues"], "get_issues");
    let empty_policy = runtime_tool_policy(&json!({}));
    authorized(&allowed)
        .admit_pipeline_with_policy(&empty_policy)
        .expect("exact frozen direct Toolkit scope");

    for invalid in [
        toolkit_pipeline_request("other_repository", &["get_issues"], "get_issues"),
        toolkit_pipeline_request(
            "release_repository",
            &["list_branches_in_repo"],
            "get_issues",
        ),
    ] {
        let error = admission_error(authorized(&invalid).admit_pipeline_with_policy(&empty_policy));
        assert_eq!(error.code(), NativeAgentAssemblyErrorCode::InvalidInput);
    }

    for policy in [
        runtime_tool_policy(&json!({
            "toolkit_security": {"blocked_toolkits": ["gitlab_org"]}
        })),
        runtime_tool_policy(&json!({
            "toolkit_security": {"blocked_tools": {"gitlab_org": ["get_issues"]}}
        })),
    ] {
        let error = admission_error(authorized(&allowed).admit_pipeline_with_policy(&policy));
        assert_eq!(
            error.code(),
            NativeAgentAssemblyErrorCode::UnsupportedCapability
        );
    }
    let sensitive = runtime_tool_policy(&json!({
        "toolkit_security": {"sensitive_tools": {"gitlab_org": ["get_issues"]}}
    }));
    authorized(&allowed)
        .admit_pipeline_with_policy(&sensitive)
        .expect("sensitive direct read is admitted for checkpointed confirmation");
}

#[test]
fn mcp_node_scope_is_exact_and_sensitive_read_uses_the_graph_confirmation() {
    let allowed = mcp_pipeline_request(
        "release intelligence",
        &["lookup_release"],
        "lookup_release",
    );
    let empty_policy = runtime_tool_policy(&json!({}));
    authorized(&allowed)
        .admit_pipeline_with_policy(&empty_policy)
        .expect("exact frozen direct MCP scope");

    for invalid in [
        mcp_pipeline_request("other MCP", &["lookup_release"], "lookup_release"),
        mcp_pipeline_request(
            "release intelligence",
            &["other_release_tool"],
            "lookup_release",
        ),
    ] {
        let error = admission_error(authorized(&invalid).admit_pipeline_with_policy(&empty_policy));
        assert_eq!(error.code(), NativeAgentAssemblyErrorCode::InvalidInput);
    }

    for policy in [
        runtime_tool_policy(&json!({
            "toolkit_security": {"blocked_toolkits": ["mcp"]}
        })),
        runtime_tool_policy(&json!({
            "toolkit_security": {"blocked_tools": {"mcp": ["lookup_release"]}}
        })),
    ] {
        let error = admission_error(authorized(&allowed).admit_pipeline_with_policy(&policy));
        assert_eq!(
            error.code(),
            NativeAgentAssemblyErrorCode::UnsupportedCapability
        );
    }
    let sensitive = runtime_tool_policy(&json!({
        "toolkit_security": {"sensitive_tools": {"mcp": ["lookup_release"]}}
    }));
    authorized(&allowed)
        .admit_pipeline_with_policy(&sensitive)
        .expect("sensitive MCP read is admitted for checkpointed confirmation");

    let configured_as_mcp =
        toolkit_pipeline_request("release_repository", &["get_issues"], "get_issues");
    let mut configured_as_mcp = configured_as_mcp;
    let instructions = configured_as_mcp
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("pipeline version")
        .get("instructions")
        .and_then(Value::as_str)
        .expect("pipeline instructions")
        .replace("type: toolkit", "type: mcp");
    configured_as_mcp
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("pipeline version")
        .insert("instructions".to_owned(), json!(instructions));
    let error =
        admission_error(authorized(&configured_as_mcp).admit_pipeline_with_policy(&empty_policy));
    assert_eq!(error.code(), NativeAgentAssemblyErrorCode::InvalidInput);
}

#[test]
fn agent_node_scope_requires_one_exact_allowed_saved_application_or_pipeline() {
    let allowed = agent_pipeline_request("release-agent", "agent");
    let empty_policy = runtime_tool_policy(&json!({}));
    let admitted = authorized(&allowed)
        .admit_pipeline_with_policy(&empty_policy)
        .expect("exact frozen Agent participant");
    assert!(admitted.profile().definition().has_application_nodes());

    let child_pipeline = agent_pipeline_request("release-agent", "pipeline");
    authorized(&child_pipeline)
        .admit_pipeline_with_policy(&empty_policy)
        .expect("saved pipeline is a valid Agent-node participant kind");

    for invalid in [
        agent_pipeline_request("other-agent", "agent"),
        agent_pipeline_request("release-agent", "openai"),
    ] {
        let error = admission_error(authorized(&invalid).admit_pipeline_with_policy(&empty_policy));
        assert_eq!(error.code(), NativeAgentAssemblyErrorCode::InvalidInput);
    }

    let mut duplicate_identity = agent_pipeline_request("release-agent", "agent");
    let version = duplicate_identity
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture");
    let instructions = version
        .get("instructions")
        .and_then(Value::as_str)
        .expect("Agent pipeline YAML")
        .replace("transition: END", "transition: delegate_again");
    version.insert(
        "instructions".to_owned(),
        json!(format!(
            "{instructions}  - id: delegate_again\n    type: agent\n    tool: release-agent-copy\n    input_mapping:\n      task: {{type: fixed, value: 'Summarize again'}}\n    output: [answer, messages]\n    transition: END\n"
        )),
    );
    let tools = version
        .get_mut("tools")
        .and_then(Value::as_array_mut)
        .expect("saved participant list");
    let mut duplicate = tools[0].clone();
    duplicate["id"] = json!(45);
    duplicate["name"] = json!("release-agent-copy");
    duplicate["toolkit_name"] = json!("release-agent-copy");
    tools.push(duplicate);
    let error =
        admission_error(authorized(&duplicate_identity).admit_pipeline_with_policy(&empty_policy));
    assert_eq!(error.code(), NativeAgentAssemblyErrorCode::InvalidInput);

    let blocked = runtime_tool_policy(&json!({
        "toolkit_security": {"blocked_toolkits": ["application"]}
    }));
    let error = admission_error(authorized(&allowed).admit_pipeline_with_policy(&blocked));
    assert_eq!(
        error.code(),
        NativeAgentAssemblyErrorCode::UnsupportedCapability
    );
}

#[tokio::test]
async fn direct_saved_agent_node_streams_one_exact_pipeline_hierarchy() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let (platform, model_facade, calls, paths) = direct_agent_pipeline_runtime();
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::clone(&sessions),
        Arc::clone(&checkpointer) as Arc<dyn Checkpointer>,
    )
    .with_runtime_clients(platform, model_facade);
    let mut request = agent_pipeline_request("release-agent", "agent");
    let instructions = request.payload.application["version_details"]["instructions"]
        .as_str()
        .expect("saved pipeline YAML")
        .replace("delegate", "Agent 1");
    request.payload.application["version_details"]["instructions"] = json!(instructions);
    let mut invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("authorized direct-agent participant");
    invocation
        .project_start(timestamp(0))
        .expect("browser start");
    let (mut run, mut projector, completion) = invocation.start().expect("pipeline start");
    let mut browser = Vec::new();
    while let Some(event) = run.next_event().await.expect("pipeline Agent event") {
        let projected = projector
            .project(&event)
            .expect("pipeline Agent projection");
        browser.extend(projected.into_iter().map(|event| current(&event)));
    }
    let selected = completion
        .select()
        .await
        .expect("pipeline Agent result selection");
    browser.extend(
        projector
            .finish_after_eos(selected, timestamp(1))
            .expect("pipeline Agent browser completion")
            .into_iter()
            .map(|event| current(&event)),
    );

    let nested = browser
        .iter()
        .filter(|event| {
            event["response_metadata"]["parent_agent_path"]
                .as_array()
                .is_some_and(|path| !path.is_empty())
        })
        .collect::<Vec<_>>();
    assert!(!nested.is_empty(), "missing nested hierarchy: {browser:?}");
    for event in nested {
        let path = event["response_metadata"]["parent_agent_path"]
            .as_array()
            .expect("nested Agent path");
        assert_eq!(path.len(), 1);
        assert_eq!(path[0]["name"], "release-agent");
        assert_eq!(path[0]["sibling_ordinal"], 1);
        assert!(
            path[0]["call_id"]
                .as_str()
                .is_some_and(|call_id| call_id.starts_with("pipeline:Agent1:"))
        );
    }
    for event in &browser {
        for field in ["name", "tool_name"] {
            assert_ne!(
                event[field], "Agent1",
                "internal node ID became a display label"
            );
            assert_ne!(
                event[field], "Agent 1",
                "legacy node ID became a display label"
            );
        }
    }
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == "direct child summary"),
        "missing direct child summary: {browser:?}"
    );
    assert_eq!(calls.load(Ordering::Acquire), 2);
    assert_eq!(
        paths.lock().expect("runtime paths").as_slice(),
        [
            "/executions/execution%2Fone/generations/2/runtime-context/elitea-client-token",
            "/executions/execution%2Fone/generations/2/runtime-context/applications/3/versions/4"
        ]
    );
}

#[tokio::test(flavor = "current_thread")]
async fn direct_saved_agent_node_resumes_exact_descendant_confirmation_from_graph_checkpoint() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let (platform, model_facade, context_calls, _paths) = sensitive_direct_agent_pipeline_runtime();
    let connections = Arc::new(AtomicUsize::new(0));
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let connector = Arc::new(PipelineMcpConnector {
        connections: Arc::clone(&connections),
        tool_calls: Arc::clone(&tool_calls),
        read_only: true,
    });
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::clone(&sessions),
        Arc::clone(&checkpointer) as Arc<dyn Checkpointer>,
    )
    .with_runtime_clients(platform, model_facade)
    .with_mcp_connector(connector)
    .with_tool_policy(sensitive_pipeline_mcp_policy());
    let request = agent_pipeline_request("release-agent", "agent");
    let private_thread = private_pipeline_session_id(&request);
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("authorized sensitive direct-agent participant");
    let (browser_interrupts, graph_interrupt) = collect_pipeline_pause(invocation).await;
    assert_eq!(browser_interrupts.len(), 1);
    let public = &browser_interrupts[0];
    assert_eq!(
        public["response_metadata"]["parent_agent_path"],
        json!([{
            "name": "release-agent",
            "call_id": "pipeline:delegate:0",
            "sibling_ordinal": 1,
        }])
    );
    let interrupt_id = public["response_metadata"]["hitl_interrupts"][0]["interrupt_id"]
        .as_str()
        .expect("descendant interrupt identity")
        .to_owned();
    let graph_interrupt = graph_interrupt.expect("internal graph checkpoint event");
    let binding =
        pipeline_application_event_binding(&graph_interrupt, "elitea-agent", &private_thread)
            .expect("pipeline Application checkpoint binding");
    assert_eq!(binding.node_name(), "delegate");
    assert_eq!(binding.application_call_id(), "pipeline:delegate:0");
    assert_eq!(binding.interrupt_ids(), [interrupt_id.as_str()]);
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);

    let resume = pipeline_application_resume_request(vec![interrupt_id], "approve", "", [8; 32]);
    let invocation = assembler
        .assemble(authorized(&resume))
        .await
        .expect("checkpoint-bound descendant resume");
    let browser = collect_pipeline_completion(invocation).await;
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == "child resumed summary")
    );
    assert_eq!(tool_calls.load(Ordering::Acquire), 1);
    assert_eq!(connections.load(Ordering::Acquire), 2);
    assert_eq!(context_calls.load(Ordering::Acquire), 4);
}

#[tokio::test(flavor = "current_thread")]
async fn pipeline_agent_node_resumes_parallel_nested_confirmations_without_identity_collisions() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let (platform, model_facade, context_calls, _paths) = recursive_sensitive_pipeline_runtime();
    let connections = Arc::new(AtomicUsize::new(0));
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::clone(&sessions),
        Arc::clone(&checkpointer) as Arc<dyn Checkpointer>,
    )
    .with_runtime_clients(platform, model_facade)
    .with_mcp_connector(Arc::new(PipelineMcpConnector {
        connections: Arc::clone(&connections),
        tool_calls: Arc::clone(&tool_calls),
        read_only: true,
    }))
    .with_tool_policy(sensitive_pipeline_mcp_policy());
    let request = agent_pipeline_request("release-agent", "agent");
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("recursive pipeline Agent participant");
    let (interrupts, graph_interrupt) = collect_pipeline_pause(invocation).await;
    assert!(graph_interrupt.is_some());
    assert_eq!(interrupts.len(), 2);
    let paths = interrupts
        .iter()
        .map(|event| event["response_metadata"]["parent_agent_path"].clone())
        .collect::<HashSet<_>>();
    assert_eq!(
        paths,
        HashSet::from([
            json!([
                {"name": "release-agent", "call_id": "pipeline:delegate:0", "sibling_ordinal": 1},
                {"name": "name-resolver", "call_id": "call_first", "sibling_ordinal": 1},
            ]),
            json!([
                {"name": "release-agent", "call_id": "pipeline:delegate:0", "sibling_ordinal": 1},
                {"name": "name-resolver", "call_id": "call_last", "sibling_ordinal": 2},
            ]),
        ])
    );
    let mut interrupt_ids = interrupts
        .iter()
        .map(|event| {
            event["response_metadata"]["hitl_interrupts"][0]["interrupt_id"]
                .as_str()
                .expect("parallel descendant interrupt identity")
                .to_owned()
        })
        .collect::<Vec<_>>();
    interrupt_ids.sort_unstable();
    assert_ne!(interrupt_ids[0], interrupt_ids[1]);
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);

    let partial =
        pipeline_application_resume_request(vec![interrupt_ids[0].clone()], "approve", "", [5; 32]);
    let partial = assembler.assemble(authorized(&partial)).await;
    let Err(partial) = partial else {
        panic!("partial parallel descendant decision set resumed the graph");
    };
    assert_eq!(partial.code(), NativeAgentAssemblyErrorCode::InvalidInput);
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);

    let resume = pipeline_application_resume_request(interrupt_ids, "approve", "", [7; 32]);
    let invocation = assembler
        .assemble(authorized(&resume))
        .await
        .expect("parallel descendant checkpoint resume");
    let browser = collect_pipeline_completion(invocation).await;
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == "parallel orchestrator summary"),
        "missing parallel orchestrator summary: {browser:?}"
    );
    assert_eq!(tool_calls.load(Ordering::Acquire), 2);
    assert_eq!(connections.load(Ordering::Acquire), 3);
    assert_eq!(context_calls.load(Ordering::Acquire), 9);
}

#[tokio::test(flavor = "current_thread")]
async fn pipeline_agent_node_resumes_parallel_nested_authorization_for_authorize_and_skip() {
    run_pipeline_agent_node_nested_authorization(true).await;
    run_pipeline_agent_node_nested_authorization(false).await;
}

#[allow(clippy::too_many_lines)] // Pause, fail-closed partial set, and exact replay are one proof.
async fn run_pipeline_agent_node_nested_authorization(authorize: bool) {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let ((platform, model_facade, context_calls, _paths), captured) =
        recursive_sensitive_pipeline_runtime_with_capture();
    let connections = Arc::new(AtomicUsize::new(0));
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::clone(&sessions),
        Arc::clone(&checkpointer) as Arc<dyn Checkpointer>,
    )
    .with_runtime_clients(platform, model_facade)
    .with_mcp_connector(Arc::new(DelegatedAuthorizationPipelineMcpConnector {
        connections: Arc::clone(&connections),
        tool_calls: Arc::clone(&tool_calls),
    }));
    let request = agent_pipeline_request("release-agent", "agent");
    let private_thread = private_pipeline_session_id(&request);
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("recursive pipeline Agent authorization participant");
    let (paused, graph_interrupt) = collect_pipeline_pause_events(invocation).await;
    let cards = paused
        .iter()
        .filter(|event| event["type"] == "mcp_authorization_required")
        .collect::<Vec<_>>();
    assert_eq!(cards.len(), 2);
    let paths = cards
        .iter()
        .map(|event| event["response_metadata"]["parent_agent_path"].clone())
        .collect::<HashSet<_>>();
    assert_eq!(
        paths,
        HashSet::from([
            json!([
                {"name": "release-agent", "call_id": "pipeline:delegate:0", "sibling_ordinal": 1},
                {"name": "name-resolver", "call_id": "call_first", "sibling_ordinal": 1},
            ]),
            json!([
                {"name": "release-agent", "call_id": "pipeline:delegate:0", "sibling_ordinal": 1},
                {"name": "name-resolver", "call_id": "call_last", "sibling_ordinal": 2},
            ]),
        ])
    );
    let mut interrupt_ids = cards
        .iter()
        .map(|event| {
            assert_eq!(event["response_metadata"]["tool_call_id"], "call_mcp");
            event["response_metadata"]["interrupt_id"]
                .as_str()
                .expect("nested authorization identity")
                .to_owned()
        })
        .collect::<Vec<_>>();
    interrupt_ids.sort_unstable();
    assert_ne!(interrupt_ids[0], interrupt_ids[1]);
    let graph_interrupt = graph_interrupt.expect("private pipeline Application checkpoint");
    let binding =
        pipeline_application_event_binding(&graph_interrupt, "elitea-agent", &private_thread)
            .expect("pipeline Application authorization checkpoint binding");
    assert_eq!(binding.application_call_id(), "pipeline:delegate:0");
    assert_eq!(
        binding
            .interrupt_ids()
            .iter()
            .cloned()
            .collect::<HashSet<_>>(),
        interrupt_ids.iter().cloned().collect::<HashSet<_>>()
    );
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);

    let action = if authorize { "authorize" } else { "skip" };
    let partial = pipeline_application_authorization_resume_request(
        vec![interrupt_ids[0].clone()],
        action,
        if authorize { [14; 32] } else { [15; 32] },
    );
    let Err(partial) = assembler.assemble(authorized(&partial)).await else {
        panic!("partial parallel authorization set resumed the pipeline Agent node");
    };
    assert_eq!(partial.code(), NativeAgentAssemblyErrorCode::InvalidInput);
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);

    let resume = pipeline_application_authorization_resume_request(
        interrupt_ids,
        action,
        if authorize { [16; 32] } else { [17; 32] },
    );
    let invocation = assembler
        .assemble(authorized(&resume))
        .await
        .expect("parallel nested authorization checkpoint resume");
    let browser = collect_pipeline_completion(invocation).await;
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == "parallel orchestrator summary"),
        "missing parallel orchestrator summary: {browser:?}"
    );
    assert_eq!(
        tool_calls.load(Ordering::Acquire),
        usize::from(authorize) * 2
    );
    assert_eq!(connections.load(Ordering::Acquire), 2);
    assert_eq!(context_calls.load(Ordering::Acquire), 6);
    let captured = captured.lock().expect("captured model requests");
    assert_eq!(
        captured.len(),
        6,
        "resume must not replan saved-agent calls"
    );
    if !authorize {
        assert_eq!(
            captured
                .iter()
                .filter(|request| request
                    .body
                    .windows("mcp_auth_decision".len())
                    .any(|window| window == b"mcp_auth_decision"))
                .count(),
            2,
            "each skipped leaf must close the original call with a structured result"
        );
    }
}

#[tokio::test(flavor = "current_thread")]
#[allow(clippy::too_many_lines)] // Mixed public cards and one atomic replay are one proof.
async fn pipeline_agent_node_resumes_parallel_mixed_sensitive_and_authorization_guards() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let ((platform, model_facade, context_calls, _paths), captured) =
        mixed_guardrail_pipeline_runtime_with_capture();
    let connections = Arc::new(AtomicUsize::new(0));
    let sensitive_calls = Arc::new(AtomicUsize::new(0));
    let authorization_calls = Arc::new(AtomicUsize::new(0));
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::clone(&sessions),
        Arc::clone(&checkpointer) as Arc<dyn Checkpointer>,
    )
    .with_runtime_clients(platform, model_facade)
    .with_mcp_connector(Arc::new(MixedGuardrailPipelineMcpConnector {
        connections: Arc::clone(&connections),
        sensitive_calls: Arc::clone(&sensitive_calls),
        authorization_calls: Arc::clone(&authorization_calls),
    }))
    .with_tool_policy(Arc::new(runtime_tool_policy(&json!({
        "toolkit_security": {
            "sensitive_tools": {"mcp": ["lookup_sensitive"]},
            "sensitive_action_company_name": "Example Org"
        }
    }))));
    let request = agent_pipeline_request("release-agent", "agent");
    let private_thread = private_pipeline_session_id(&request);
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("mixed nested guardrail participant");
    let (paused, graph_interrupt) = collect_pipeline_pause_events(invocation).await;
    let sensitive = paused
        .iter()
        .find(|event| event["type"] == "agent_hitl_interrupt")
        .expect("nested sensitive card");
    let authorization = paused
        .iter()
        .find(|event| event["type"] == "mcp_authorization_required")
        .expect("nested authorization card");
    assert_eq!(
        sensitive["response_metadata"]["parent_agent_path"],
        json!([
            {"name": "release-agent", "call_id": "pipeline:delegate:0", "sibling_ordinal": 1},
            {"name": "sensitive-resolver", "call_id": "call_sensitive_child", "sibling_ordinal": 1},
        ])
    );
    assert_eq!(
        authorization["response_metadata"]["parent_agent_path"],
        json!([
            {"name": "release-agent", "call_id": "pipeline:delegate:0", "sibling_ordinal": 1},
            {"name": "auth-resolver", "call_id": "call_auth_child", "sibling_ordinal": 2},
        ])
    );
    let sensitive_interrupt_id =
        sensitive["response_metadata"]["hitl_interrupts"][0]["interrupt_id"]
            .as_str()
            .expect("nested sensitive identity")
            .to_owned();
    let authorization_interrupt_id = authorization["response_metadata"]["interrupt_id"]
        .as_str()
        .expect("nested authorization identity")
        .to_owned();
    let binding = pipeline_application_event_binding(
        &graph_interrupt.expect("private mixed-guardrail graph checkpoint"),
        "elitea-agent",
        &private_thread,
    )
    .expect("mixed-guardrail pipeline Application binding");
    assert_eq!(
        binding
            .interrupt_ids()
            .iter()
            .cloned()
            .collect::<HashSet<_>>(),
        HashSet::from([
            sensitive_interrupt_id.clone(),
            authorization_interrupt_id.clone(),
        ])
    );
    assert_eq!(sensitive_calls.load(Ordering::Acquire), 0);
    assert_eq!(authorization_calls.load(Ordering::Acquire), 0);

    let mut resume = agent_pipeline_request("release-agent", "agent");
    resume.binding.request_content_digest = [18; 32];
    resume.payload.should_continue = true;
    resume.payload.hitl_resume = true;
    resume.payload.hitl_decisions = vec![
        json!({
            "interrupt_id": sensitive_interrupt_id,
            "tool_call_id": "call_sensitive_mcp",
            "guardrail_type": "sensitive_tool",
            "action": "approve",
            "value": "",
        }),
        json!({
            "interrupt_id": authorization_interrupt_id,
            "tool_call_id": "call_auth_mcp",
            "guardrail_type": "mcp_auth",
            "action": "authorize",
            "value": "",
        }),
    ];
    resume.payload.mcp_tokens.insert(
        "https://auth.example.invalid/v1/mcp".to_owned(),
        json!({"access_token": "runtime-secret"}),
    );
    let invocation = assembler
        .assemble(authorized(&resume))
        .await
        .expect("atomic mixed-guardrail checkpoint resume");
    let browser = collect_pipeline_completion(invocation).await;
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == "mixed guardrails complete"),
        "missing mixed guardrails completion: {browser:?}"
    );
    assert_eq!(sensitive_calls.load(Ordering::Acquire), 1);
    assert_eq!(authorization_calls.load(Ordering::Acquire), 1);
    assert_eq!(connections.load(Ordering::Acquire), 4);
    assert_eq!(context_calls.load(Ordering::Acquire), 8);
    assert_eq!(
        captured.lock().expect("captured model requests").len(),
        6,
        "resume must replay both exact child calls without replanning"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn pipeline_agent_node_block_preserves_same_call_structured_result_without_dispatch() {
    let sessions = Arc::new(InMemorySessionService::new());
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let (platform, model_facade, _context_calls, _paths) =
        sensitive_direct_agent_pipeline_runtime();
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let assembler = PipelineNativeAgentAssembler::with_state(
        sessions.clone(),
        Arc::clone(&checkpointer) as Arc<dyn Checkpointer>,
    )
    .with_runtime_clients(platform, model_facade)
    .with_mcp_connector(Arc::new(PipelineMcpConnector {
        connections: Arc::new(AtomicUsize::new(0)),
        tool_calls: Arc::clone(&tool_calls),
        read_only: true,
    }))
    .with_tool_policy(sensitive_pipeline_mcp_policy());
    let request = agent_pipeline_request("release-agent", "agent");
    let profile = PipelineExecutionProfile::validate(&request, false).expect("pipeline profile");
    let plan = OrdinaryNativeAgentPlan::from_authorized_pipeline(
        &request,
        profile.shell(),
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
        false,
        false,
    )
    .expect("pipeline identity plan");
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("blocking pipeline Agent participant");
    let (interrupts, _) = collect_pipeline_pause(invocation).await;
    let interrupt_id = interrupts[0]["response_metadata"]["hitl_interrupts"][0]["interrupt_id"]
        .as_str()
        .expect("blocked descendant interrupt identity")
        .to_owned();
    let resume = pipeline_application_resume_request(
        vec![interrupt_id],
        "block_with_comment",
        "keep customer data private",
        [6; 32],
    );
    let invocation = assembler
        .assemble(authorized(&resume))
        .await
        .expect("blocked descendant checkpoint resume");
    let _browser = collect_pipeline_completion(invocation).await;
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);

    let stored = sessions
        .get(GetRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id: plan.user_id().to_owned(),
            session_id: plan.session_id().to_owned(),
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("blocked pipeline session");
    let blocked = stored
        .events()
        .all()
        .into_iter()
        .find_map(|event| {
            (event.actions.tool_confirmation_decision
                == Some(adk_rust::ToolConfirmationDecision::Deny))
            .then(|| {
                event
                    .tool_results()
                    .into_iter()
                    .find(|result| {
                        result.call_id == Some("call_mcp") && result.name == "lookup_release"
                    })
                    .map(|result| result.response.clone())
            })
            .flatten()
        })
        .expect("same-call structured blocked result");
    assert_eq!(blocked["type"], "sensitive_tool_blocked");
    assert_eq!(blocked["blocked_tool_name"], "lookup_release");
    assert_eq!(blocked["denial_reason"], "keep customer data private");
}

#[tokio::test]
async fn saved_pipeline_participant_loads_exact_version_and_runs_as_child_subgraph() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let (platform, model_facade, calls, paths) = pipeline_child_runtime();
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::clone(&sessions),
        Arc::clone(&checkpointer) as Arc<dyn Checkpointer>,
    )
    .with_runtime_clients(platform, model_facade);
    let request = agent_pipeline_request("release-agent", "pipeline");
    let private_thread = private_pipeline_session_id(&request);
    let mut invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("authorized saved-pipeline participant");
    invocation
        .project_start(timestamp(0))
        .expect("browser start");
    let (mut run, mut projector, completion) = invocation.start().expect("pipeline start");
    let mut browser = Vec::new();
    while let Some(event) = run.next_event().await.expect("pipeline event") {
        browser.extend(
            projector
                .project(&event)
                .expect("pipeline event projection")
                .into_iter()
                .map(|event| current(&event)),
        );
    }
    let selected = completion
        .select()
        .await
        .expect("pipeline result selection");
    browser.extend(
        projector
            .finish_after_eos(selected, timestamp(1))
            .expect("pipeline browser completion")
            .into_iter()
            .map(|event| current(&event)),
    );
    let wrapper_start = browser
        .iter()
        .find(|event| {
            event["type"] == "agent_tool_start"
                && event["response_metadata"]["tool_name"] == "release-agent"
        })
        .expect("saved-pipeline wrapper start");
    assert_eq!(
        wrapper_start["response_metadata"]["parent_agent_call_id"],
        "pipeline:delegate:0"
    );
    assert_eq!(
        wrapper_start["response_metadata"]["metadata"]["toolkit_type"],
        "pipeline"
    );
    let child_model = browser
        .iter()
        .find(|event| {
            event["type"] == "agent_llm_start"
                && event["response_metadata"]["metadata"]["langgraph_node"] == "answer"
        })
        .expect("saved-pipeline child LLM progress");
    assert_eq!(
        child_model["response_metadata"]["parent_agent_path"],
        json!([{
            "name": "release-agent",
            "call_id": "pipeline:delegate:0",
            "sibling_ordinal": 1,
        }])
    );
    assert!(browser.iter().any(|event| {
        event["type"] == "agent_tool_end"
            && event["response_metadata"]["tool_run_id"] == "pipeline:delegate:0"
    }));
    assert!(
        browser
            .iter()
            .any(|event| { event["content"] == "Child: Summarize the release" })
    );
    assert_eq!(calls.load(Ordering::Acquire), 2);
    assert_eq!(
        paths.lock().expect("runtime paths").as_slice(),
        [
            "/executions/execution%2Fone/generations/2/runtime-context/elitea-client-token",
            "/executions/execution%2Fone/generations/2/runtime-context/applications/3/versions/4"
        ]
    );
    assert!(
        checkpointer
            .load(&format!("{private_thread}/delegate"))
            .await
            .expect("child checkpoint lookup")
            .is_some()
    );
}

#[tokio::test]
async fn saved_data_pipeline_emits_wrapper_progress_without_an_llm_node() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let (platform, model_facade, calls, _paths) = pipeline_data_child_runtime();
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::clone(&sessions),
        Arc::clone(&checkpointer) as Arc<dyn Checkpointer>,
    )
    .with_runtime_clients(platform, model_facade);
    let request = agent_pipeline_request("release-agent", "pipeline");
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("authorized data-pipeline participant");
    let browser = collect_pipeline_completion(invocation).await;
    assert!(browser.iter().any(|event| {
        event["type"] == "agent_tool_start"
            && event["response_metadata"]["tool_run_id"] == "pipeline:delegate:0"
            && event["response_metadata"]["metadata"]["toolkit_type"] == "pipeline"
    }));
    assert!(browser.iter().any(|event| {
        event["type"] == "agent_tool_end"
            && event["response_metadata"]["tool_run_id"] == "pipeline:delegate:0"
    }));
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == "Child: Summarize the release")
    );
    assert_eq!(calls.load(Ordering::Acquire), 2);
}

#[tokio::test]
async fn saved_pipeline_hitl_projects_under_the_exact_wrapper_and_resumes() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let (platform, model_facade, calls, _paths) = pipeline_hitl_child_runtime();
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::clone(&sessions),
        Arc::clone(&checkpointer) as Arc<dyn Checkpointer>,
    )
    .with_runtime_clients(platform, model_facade);
    let mut request = agent_pipeline_request("release-agent", "pipeline");
    let private_thread = private_pipeline_session_id(&request);
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("authorized interrupting saved-pipeline participant");
    let (browser, graph_interrupt) = collect_pipeline_pause_events(invocation).await;
    let interrupt = browser
        .iter()
        .find(|event| event["type"] == "agent_hitl_interrupt")
        .expect("nested pipeline HITL card");
    let expected_path = json!([{
        "name": "release-agent",
        "call_id": "pipeline:delegate:0",
        "sibling_ordinal": 1,
    }]);
    assert_eq!(
        interrupt["response_metadata"]["parent_agent_path"],
        expected_path
    );
    assert_eq!(
        interrupt["response_metadata"]["parent_agent_call_id"],
        "pipeline:delegate:0"
    );
    assert_eq!(
        interrupt["response_metadata"]["hitl_interrupts"][0]["parent_agent_path"],
        expected_path
    );
    let public_interrupt_id = interrupt["response_metadata"]["hitl_interrupts"][0]["interrupt_id"]
        .as_str()
        .expect("public nested pipeline interrupt identity")
        .to_owned();
    let graph_interrupt = graph_interrupt.expect("private nested graph interruption");
    let binding = pipeline_hitl_event_binding(&graph_interrupt, "elitea-agent", &private_thread)
        .expect("root and child checkpoint binding");
    assert_eq!(binding.interrupt_id(), public_interrupt_id);
    assert_eq!(binding.nested_checkpoints().len(), 1);
    assert_eq!(
        binding.nested_checkpoints()[0].thread_id(),
        format!("{private_thread}/delegate")
    );
    request.binding.request_content_digest = [6; 32];
    request.payload.should_continue = true;
    request.payload.hitl_resume = true;
    request.payload.hitl_action = Some("approve".to_owned());
    request.payload.hitl_value = Some(String::new());
    request.payload.hitl_decisions = vec![json!({
        "interrupt_id": public_interrupt_id,
        "tool_call_id": "",
        "action": "approve",
        "value": ""
    })];
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("nested pipeline checkpoint continuation");
    let resumed = collect_pipeline_completion(invocation).await;
    assert!(
        resumed
            .iter()
            .any(|event| event["content"] == "Approved: Summarize the release")
    );
    assert!(resumed.iter().any(|event| {
        event["type"] == "agent_tool_end"
            && event["response_metadata"]["tool_run_id"] == "pipeline:delegate:0"
    }));
    assert_eq!(calls.load(Ordering::Acquire), 4);
}

#[tokio::test]
async fn saved_pipeline_sensitive_llm_keeps_call_identity_and_wrapper_hierarchy() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let ((platform, model_facade, context_calls, _paths), captured) =
        sensitive_pipeline_child_runtime();
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::clone(&sessions),
        Arc::clone(&checkpointer) as Arc<dyn Checkpointer>,
    )
    .with_runtime_clients(platform, model_facade)
    .with_mcp_connector(Arc::new(PipelineMcpConnector {
        connections: Arc::new(AtomicUsize::new(0)),
        tool_calls: Arc::clone(&tool_calls),
        read_only: true,
    }))
    .with_tool_policy(sensitive_pipeline_mcp_policy());
    let mut request = agent_pipeline_request("release-agent", "pipeline");
    let private_thread = private_pipeline_session_id(&request);
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("sensitive saved-pipeline participant");
    let (paused, graph_interrupt) = collect_pipeline_pause_events(invocation).await;
    let interrupt = paused
        .iter()
        .find(|event| event["type"] == "agent_hitl_interrupt")
        .expect("nested sensitive-tool card");
    let expected_path = json!([{
        "name": "release-agent",
        "call_id": "pipeline:delegate:0",
        "sibling_ordinal": 1,
    }]);
    assert_eq!(
        interrupt["response_metadata"]["parent_agent_path"],
        expected_path
    );
    let public_interrupt_id = interrupt["response_metadata"]["hitl_interrupts"][0]["interrupt_id"]
        .as_str()
        .expect("nested sensitive-tool identity")
        .to_owned();
    let graph_interrupt = graph_interrupt.expect("private nested tool interruption");
    let binding = pipeline_tool_event_binding(&graph_interrupt, "elitea-agent", &private_thread)
        .expect("root and nested tool checkpoint binding");
    assert_eq!(binding.interrupt_id(), public_interrupt_id);
    assert_eq!(binding.tool_call_id(), "call_mcp");
    assert_eq!(binding.nested_checkpoints().len(), 1);
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);

    request.binding.request_content_digest = [7; 32];
    request.payload.should_continue = true;
    request.payload.hitl_resume = true;
    request.payload.hitl_action = Some("block_with_comment".to_owned());
    request.payload.hitl_value = Some("release is frozen".to_owned());
    request.payload.hitl_decisions = vec![json!({
        "interrupt_id": public_interrupt_id,
        "tool_call_id": "call_mcp",
        "action": "block_with_comment",
        "value": "release is frozen"
    })];
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("nested sensitive-tool continuation");
    let resumed = collect_pipeline_completion(invocation).await;
    assert_nested_sensitive_browser_completion(&resumed, &expected_path);
    assert_llm_blocked_continuation(&captured, "release is frozen");
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);
    assert_eq!(context_calls.load(Ordering::Acquire), 4);
}

fn assert_nested_sensitive_browser_completion(resumed: &[Value], expected_path: &Value) {
    let tool_end = resumed
        .iter()
        .find(|event| {
            event["type"] == "agent_tool_end"
                && event["response_metadata"]["tool_run_id"] == "call_mcp"
        })
        .expect("same nested tool-call completion");
    assert_eq!(
        &tool_end["response_metadata"]["parent_agent_path"],
        expected_path
    );
    let browser_blocked: Value = serde_json::from_str(
        tool_end["response_metadata"]["tool_output"]
            .as_str()
            .expect("nested browser block result"),
    )
    .expect("nested browser block result JSON");
    assert_eq!(browser_blocked["type"], "sensitive_tool_blocked");
    assert_eq!(browser_blocked["denial_reason"], "release is frozen");
    assert!(
        resumed
            .iter()
            .any(|event| event["content"] == "nested block handled")
    );
    let nested_model = resumed
        .iter()
        .find(|event| {
            event["type"] == "agent_llm_start"
                && event["response_metadata"]["metadata"]["langgraph_node"] == "answer"
        })
        .expect("nested continuation model progress");
    assert_eq!(
        &nested_model["response_metadata"]["parent_agent_path"],
        expected_path
    );
}

#[tokio::test]
async fn toolkit_node_materializes_read_only_action_but_rejects_remote_effect() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::clone(&sessions),
        Arc::new(MemoryCheckpointer::new()),
    );
    let read = toolkit_pipeline_request("release_repository", &["get_issues"], "get_issues");
    assembler
        .assemble(authorized(&read))
        .await
        .expect("read-only direct Toolkit assembly");

    let effect =
        toolkit_pipeline_request("release_repository", &["create_branch"], "create_branch");
    let result = assembler.assemble(authorized(&effect)).await;
    let Err(error) = result else {
        panic!("effectful direct Toolkit node was assembled");
    };
    assert_eq!(
        error.code(),
        NativeAgentAssemblyErrorCode::UnsupportedCapability
    );
}

#[tokio::test(flavor = "current_thread")]
async fn pipeline_llm_node_binds_same_named_tools_to_exact_toolkit_implementations() {
    let context_calls = Arc::new(AtomicUsize::new(0));
    let paths = Arc::new(Mutex::new(Vec::new()));
    let token = runtime_response(&json!({
        "schema_version": "elitea.runtime.elitea-client-token.v1",
        "project_id": 17,
        "token": "ephemeral-pipeline-token"
    }));
    let ((platform, model_facade, _, _), captured) = pipeline_runtime_from_responses_with_capture(
        VecDeque::from([token]),
        vec![
            TestModelGatewayOutcome::Response(pipeline_colliding_mcp_tool_call_response()),
            TestModelGatewayOutcome::Response(pipeline_text_response(
                "both exact sources completed",
            )),
        ],
        Arc::clone(&context_calls),
        paths,
    );
    let connections = Arc::new(AtomicUsize::new(0));
    let tool_calls = Arc::new(Mutex::new(Vec::new()));
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let assembler =
        PipelineNativeAgentAssembler::with_state(sessions, Arc::new(MemoryCheckpointer::new()))
            .with_runtime_clients(platform, model_facade)
            .with_mcp_connector(Arc::new(CollidingPipelineMcpConnector {
                connections: Arc::clone(&connections),
                tool_calls: Arc::clone(&tool_calls),
            }));
    let request = colliding_llm_mcp_pipeline_request();
    authorized(&request)
        .admit_pipeline_with_policy(&runtime_tool_policy(&json!({})))
        .unwrap_or_else(|error| panic!("collision request admission failed: {error}"));
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("colliding-tool pipeline assembly");
    let browser = collect_pipeline_completion(invocation).await;
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == "both exact sources completed")
    );
    assert_eq!(context_calls.load(Ordering::Acquire), 1);
    assert_eq!(connections.load(Ordering::Acquire), 2);
    let mut invoked = tool_calls
        .lock()
        .expect("pipeline tool fixture lock")
        .clone();
    invoked.sort();
    assert_eq!(invoked, ["audit intelligence", "release intelligence"]);

    let captured = captured.lock().expect("captured model requests");
    assert_eq!(captured.len(), 2);
    let first: Value = serde_json::from_slice(&captured[0].body).expect("first model request");
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
    let second: Value = serde_json::from_slice(&captured[1].body).expect("second model request");
    let tool_messages = second["messages"]
        .as_array()
        .expect("continuation messages")
        .iter()
        .filter(|message| message["role"] == "tool")
        .collect::<Vec<_>>();
    assert_eq!(tool_messages.len(), 2);
    assert!(tool_messages.iter().any(|message| {
        message["tool_call_id"] == "call_release"
            && message["content"]
                .as_str()
                .is_some_and(|content| content.contains("release intelligence"))
    }));
    assert!(tool_messages.iter().any(|message| {
        message["tool_call_id"] == "call_audit"
            && message["content"]
                .as_str()
                .is_some_and(|content| content.contains("audit intelligence"))
    }));
}

struct PipelineMcpConnector {
    connections: Arc<AtomicUsize>,
    tool_calls: Arc<AtomicUsize>,
    read_only: bool,
}

struct CollidingPipelineMcpConnector {
    connections: Arc<AtomicUsize>,
    tool_calls: Arc<Mutex<Vec<String>>>,
}

#[async_trait]
impl McpConnector for CollidingPipelineMcpConnector {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        self.connections.fetch_add(1, Ordering::AcqRel);
        let source = match config.endpoint() {
            "https://release-mcp.example.invalid/v1/mcp" => "release intelligence",
            "https://audit-mcp.example.invalid/v1/mcp" => "audit intelligence",
            _ => unreachable!("collision fixture received an unexpected MCP endpoint"),
        };
        Ok(Arc::new(BasicToolset::new(
            "colliding_pipeline_fixture",
            vec![Arc::new(SourcedPipelineMcpTool {
                source,
                calls: Arc::clone(&self.tool_calls),
            })],
        )))
    }
}

struct SourcedPipelineMcpTool {
    source: &'static str,
    calls: Arc<Mutex<Vec<String>>>,
}

#[async_trait]
impl Tool for SourcedPipelineMcpTool {
    fn name(&self) -> &'static str {
        "lookup_release"
    }

    fn description(&self) -> &'static str {
        "Read release evidence from one exact pipeline toolkit."
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
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        self.calls
            .lock()
            .expect("pipeline tool fixture lock")
            .push(self.source.to_owned());
        Ok(json!({"release": arguments["release"], "source": self.source}))
    }
}

struct DelegatedAuthorizationPipelineMcpConnector {
    connections: Arc<AtomicUsize>,
    tool_calls: Arc<AtomicUsize>,
}

struct MixedGuardrailPipelineMcpConnector {
    connections: Arc<AtomicUsize>,
    sensitive_calls: Arc<AtomicUsize>,
    authorization_calls: Arc<AtomicUsize>,
}

#[async_trait]
impl McpConnector for MixedGuardrailPipelineMcpConnector {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        self.connections.fetch_add(1, Ordering::AcqRel);
        let (name, calls) = match config.endpoint() {
            "https://sensitive.example.invalid/v1/mcp" => {
                ("lookup_sensitive", Arc::clone(&self.sensitive_calls))
            }
            "https://auth.example.invalid/v1/mcp"
                if config.access_token_for_test() == Some("runtime-secret") =>
            {
                ("lookup_auth", Arc::clone(&self.authorization_calls))
            }
            "https://auth.example.invalid/v1/mcp" => {
                return Err(mcp_authorization_required_fixture(
                    config,
                    "Bearer resource_metadata=\"https://auth.example.invalid/.well-known/oauth-protected-resource\"",
                ));
            }
            _ => unreachable!("mixed-guardrail fixture received an unexpected MCP endpoint"),
        };
        Ok(Arc::new(BasicToolset::new(
            "mixed_guardrail_fixture_mcp",
            vec![Arc::new(NamedPipelineMcpTool { name, calls })],
        )))
    }
}

#[async_trait]
impl McpConnector for DelegatedAuthorizationPipelineMcpConnector {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        self.connections.fetch_add(1, Ordering::AcqRel);
        if config.access_token_for_test() != Some("runtime-secret") {
            return Err(mcp_authorization_required_fixture(
                config,
                "Bearer resource_metadata=\"https://mcp.example.invalid/.well-known/oauth-protected-resource\"",
            ));
        }
        Ok(Arc::new(BasicToolset::new(
            "authorized_fixture_mcp",
            vec![Arc::new(PipelineMcpTool {
                calls: Arc::clone(&self.tool_calls),
                read_only: true,
            })],
        )))
    }
}

#[async_trait]
impl McpConnector for PipelineMcpConnector {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        self.connections.fetch_add(1, Ordering::AcqRel);
        assert_eq!(config.endpoint(), "https://mcp.example.invalid/v1/mcp");
        Ok(Arc::new(BasicToolset::new(
            "fixture_mcp",
            vec![Arc::new(PipelineMcpTool {
                calls: Arc::clone(&self.tool_calls),
                read_only: self.read_only,
            })],
        )))
    }
}

struct PipelineMcpTool {
    calls: Arc<AtomicUsize>,
    read_only: bool,
}

struct NamedPipelineMcpTool {
    name: &'static str,
    calls: Arc<AtomicUsize>,
}

#[async_trait]
impl Tool for NamedPipelineMcpTool {
    fn name(&self) -> &str {
        self.name
    }

    fn description(&self) -> &'static str {
        "Read one mixed-guardrail release fixture."
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
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
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        self.calls.fetch_add(1, Ordering::AcqRel);
        Ok(json!({"release": arguments["release"], "source": self.name}))
    }
}

#[async_trait]
impl Tool for PipelineMcpTool {
    fn name(&self) -> &'static str {
        "lookup_release"
    }

    fn description(&self) -> &'static str {
        "Read release evidence for one release identifier."
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {"release": {"type": "string"}},
            "required": ["release"],
            "additionalProperties": false
        }))
    }

    fn is_read_only(&self) -> bool {
        self.read_only
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        self.calls.fetch_add(1, Ordering::AcqRel);
        Ok(json!({
            "records": {"release": arguments["release"], "risk": "low"},
            "messages": [{"role": "assistant", "content": "MCP read complete"}]
        }))
    }
}

#[tokio::test]
async fn llm_node_block_actions_replay_same_call_as_structured_tool_result() {
    run_llm_node_block("reject", None, "denied by user").await;
    run_llm_node_block(
        "block_with_comment",
        Some("release is under legal hold"),
        "release is under legal hold",
    )
    .await;
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // Full pause-to-same-call replay is one behavioral proof.
async fn llm_node_ask_user_resumes_the_checkpointed_call_with_the_answer_result() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let token_response = || {
        runtime_response(&json!({
            "schema_version": "elitea.runtime.elitea-client-token.v1",
            "project_id": 17,
            "token": "ephemeral-pipeline-token"
        }))
    };
    let runtime = RuntimeContextClient::with_rpc(
        PipelineRuntimeContextFixture {
            responses: Mutex::new(VecDeque::from([token_response(), token_response()])),
            calls: Arc::new(AtomicUsize::new(0)),
            paths: Arc::new(Mutex::new(Vec::new())),
        },
        RuntimeContextConfig {
            origin: "https://content.internal".to_owned(),
            deadline: Duration::from_secs(1),
            max_response_bytes: 32 * 1_024,
            max_application_response_bytes: 1_024 * 1_024,
            max_attachment_response_bytes: 1_024 * 1_024,
        },
    )
    .expect("ask_user runtime-context fixture");
    let (gateway, captured) = test_model_gateway_client(
        vec![
            TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()),
            TestModelGatewayOutcome::Response(pipeline_text_response(
                "continued after clarification",
            )),
        ],
        test_model_gateway_config(),
    )
    .expect("ask_user model gateway fixture");
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::clone(&sessions),
        Arc::clone(&checkpointer) as Arc<dyn Checkpointer>,
    )
    .with_runtime_clients(
        Arc::new(PlatformClient::new(Arc::new(runtime))),
        Arc::new(ModelFacade::from_gateway(gateway)),
    );
    let mut request = ask_user_llm_pipeline_request();
    let private_thread = private_pipeline_session_id(&request);
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("ask_user LLM-node invocation");
    let (paused, graph_interrupt) = collect_pipeline_pause_events(invocation).await;
    let clarification = paused
        .iter()
        .find(|event| event["type"] == "agent_hitl_interrupt")
        .expect("ask_user browser interrupt");
    let pending = &clarification["response_metadata"]["hitl_interrupts"][0];
    assert_eq!(pending["guardrail_type"], ASK_USER_GUARDRAIL_TYPE);
    assert_eq!(pending["available_actions"], json!(["answer"]));
    assert_eq!(pending["tool_call_id"], "call_ask_user");
    assert_eq!(pending["questions"][0]["id"], "q1");
    let interrupt_id = pending["interrupt_id"]
        .as_str()
        .expect("ask_user interrupt identity")
        .to_owned();
    let binding = pipeline_clarifying_event_binding(
        &graph_interrupt.expect("private ask_user graph interrupt"),
        "elitea-agent",
        &private_thread,
    )
    .expect("checkpoint-bound ask_user interruption");
    assert_eq!(binding.tool_call_id(), "call_ask_user");
    assert_eq!(binding.tool_name(), ASK_USER_TOOL_NAME);

    let encoded_answer = r#"{"q1":"Staging"}"#;
    request.binding.request_content_digest = [14; 32];
    request.payload.should_continue = true;
    request.payload.hitl_resume = true;
    request.payload.hitl_action = Some("answer".to_owned());
    request.payload.hitl_value = Some(encoded_answer.to_owned());
    request.payload.hitl_decisions = vec![json!({
        "interrupt_id": interrupt_id,
        "tool_call_id": "call_ask_user",
        "action": "answer",
        "value": encoded_answer,
    })];
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("ask_user LLM-node continuation");
    let browser = collect_pipeline_completion(invocation).await;
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == "continued after clarification")
    );
    let captured = captured.lock().expect("ask_user model requests");
    assert_eq!(captured.len(), 2, "resume must not ask the model to replan");
    let continuation: Value =
        serde_json::from_slice(&captured[1].body).expect("ask_user continuation JSON");
    let result = continuation["messages"]
        .as_array()
        .expect("ask_user continuation messages")
        .iter()
        .find(|message| message["role"] == "tool" && message["tool_call_id"] == "call_ask_user")
        .expect("same-call ask_user result");
    let decoded_result: String = serde_json::from_str(
        result["content"]
            .as_str()
            .expect("serialized ask_user tool content"),
    )
    .expect("ask_user string result");
    assert_eq!(
        decoded_result,
        "User answered:\n- Which environment should I use?: Staging"
    );
}

#[tokio::test]
async fn llm_node_delegated_authorization_replays_original_call_after_token_rebuild() {
    let (assembler, captured, connections, tool_calls) =
        delegated_authorization_llm_assembler("continued after authorization", false);
    let mut request = llm_mcp_pipeline_request(
        "release intelligence",
        &["lookup_release"],
        &["lookup_release"],
    );
    let private_thread = private_pipeline_session_id(&request);
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("guarded LLM-node invocation");
    let (paused, graph_interrupt) = collect_pipeline_pause_events(invocation).await;
    let authorization = paused
        .iter()
        .find(|event| event["type"] == "mcp_authorization_required")
        .expect("model-owned authorization card");
    assert_eq!(
        authorization["response_metadata"]["tool_call_id"],
        "call_mcp"
    );
    assert_eq!(authorization["response_metadata"]["tool_args"], json!({}));
    let binding = pipeline_mcp_auth_event_binding(
        &graph_interrupt.expect("private LLM authorization interrupt"),
        "elitea-agent",
        &private_thread,
    )
    .expect("checkpoint-bound LLM authorization");
    assert_eq!(binding.tool_call_id(), "call_mcp");
    assert!(binding.llm_replay().is_some());
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);

    request.binding.request_content_digest = [11; 32];
    request.payload.should_continue = true;
    request.payload.mcp_tokens.insert(
        "https://mcp.example.invalid/v1/mcp".to_owned(),
        json!({"access_token": "runtime-secret"}),
    );
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("authorized same-call continuation");
    let browser = collect_pipeline_completion(invocation).await;
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == "continued after authorization")
    );
    assert_eq!(connections.load(Ordering::Acquire), 2);
    assert_eq!(tool_calls.load(Ordering::Acquire), 1);

    let captured = captured.lock().expect("captured model requests");
    assert_eq!(captured.len(), 2, "resume must not ask the model to replan");
    let continuation: Value =
        serde_json::from_slice(&captured[1].body).expect("continuation request JSON");
    let tool_message = continuation["messages"]
        .as_array()
        .expect("continuation messages")
        .iter()
        .find(|message| message["role"] == "tool" && message["tool_call_id"] == "call_mcp")
        .expect("normal result closes original call");
    assert!(
        tool_message["content"]
            .as_str()
            .is_some_and(|content| content.contains("risk"))
    );
}

#[tokio::test]
async fn llm_node_delegated_authorization_skip_closes_original_call_without_dispatch() {
    let (assembler, captured, connections, tool_calls) =
        delegated_authorization_llm_assembler("continued after authorization skip", false);
    let mut request = llm_mcp_pipeline_request(
        "release intelligence",
        &["lookup_release"],
        &["lookup_release"],
    );
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("guarded LLM-node invocation");
    let (paused, _) = collect_pipeline_pause_events(invocation).await;
    assert!(
        paused
            .iter()
            .any(|event| event["type"] == "mcp_authorization_required")
    );

    request.binding.request_content_digest = [12; 32];
    request.payload.should_continue = true;
    request
        .payload
        .user_declined_mcp_servers
        .push(json!({"server_url": "https://mcp.example.invalid/v1/mcp"}));
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("declined same-call continuation");
    let browser = collect_pipeline_completion(invocation).await;
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == "continued after authorization skip")
    );
    assert_eq!(connections.load(Ordering::Acquire), 2);
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);

    let captured = captured.lock().expect("captured model requests");
    assert_eq!(captured.len(), 2, "resume must not ask the model to replan");
    let continuation: Value =
        serde_json::from_slice(&captured[1].body).expect("continuation request JSON");
    let tool_message = continuation["messages"]
        .as_array()
        .expect("continuation messages")
        .iter()
        .find(|message| message["role"] == "tool" && message["tool_call_id"] == "call_mcp")
        .expect("decline result closes original call");
    let declined: Value = serde_json::from_str(
        tool_message["content"]
            .as_str()
            .expect("structured decline result"),
    )
    .expect("decline result JSON");
    assert_eq!(declined["type"], "mcp_auth_decision");
    assert_eq!(declined["status"], "declined");
    assert_eq!(declined["tool_name"], "lookup_release");
}

#[tokio::test]
async fn llm_node_authorization_does_not_approve_distinct_sensitive_guard() {
    let (assembler, captured, connections, tool_calls) =
        delegated_authorization_llm_assembler("must remain unreachable", true);
    let mut request = llm_mcp_pipeline_request(
        "release intelligence",
        &["lookup_release"],
        &["lookup_release"],
    );
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("guarded LLM-node invocation");
    let (paused, _) = collect_pipeline_pause_events(invocation).await;
    assert!(
        paused
            .iter()
            .any(|event| event["type"] == "mcp_authorization_required")
    );

    request.binding.request_content_digest = [13; 32];
    request.payload.should_continue = true;
    request.payload.mcp_tokens.insert(
        "https://mcp.example.invalid/v1/mcp".to_owned(),
        json!({"access_token": "runtime-secret"}),
    );
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("authorized same-call continuation");
    let (paused, _) = collect_pipeline_pause_events(invocation).await;
    assert_eq!(
        paused
            .iter()
            .filter(|event| event["type"] == "agent_hitl_interrupt")
            .count(),
        1,
        "the distinct sensitive policy must still pause the exact call"
    );
    assert!(
        paused
            .iter()
            .all(|event| event["type"] != "mcp_authorization_required")
    );
    assert_eq!(connections.load(Ordering::Acquire), 2);
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);
    assert_eq!(
        captured.lock().expect("captured model requests").len(),
        1,
        "neither confirmation pause may replan the model-selected call"
    );
}

fn delegated_authorization_llm_assembler(
    final_text: &str,
    sensitive: bool,
) -> (
    PipelineNativeAgentAssembler,
    CapturedModelRequests,
    Arc<AtomicUsize>,
    Arc<AtomicUsize>,
) {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let token_response = || {
        runtime_response(&json!({
            "schema_version": "elitea.runtime.elitea-client-token.v1",
            "project_id": 17,
            "token": "ephemeral-pipeline-token"
        }))
    };
    let runtime = RuntimeContextClient::with_rpc(
        PipelineRuntimeContextFixture {
            responses: Mutex::new(VecDeque::from([token_response(), token_response()])),
            calls: Arc::new(AtomicUsize::new(0)),
            paths: Arc::new(Mutex::new(Vec::new())),
        },
        RuntimeContextConfig {
            origin: "https://content.internal".to_owned(),
            deadline: Duration::from_secs(1),
            max_response_bytes: 32 * 1_024,
            max_application_response_bytes: 1_024 * 1_024,
            max_attachment_response_bytes: 1_024 * 1_024,
        },
    )
    .expect("pipeline runtime-context fixture");
    let (gateway, captured) = test_model_gateway_client(
        vec![
            TestModelGatewayOutcome::Response(pipeline_mcp_tool_call_response()),
            TestModelGatewayOutcome::Response(pipeline_text_response(final_text)),
        ],
        test_model_gateway_config(),
    )
    .expect("pipeline model gateway fixture");
    let connections = Arc::new(AtomicUsize::new(0));
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let mut assembler = PipelineNativeAgentAssembler::with_state(
        Arc::clone(&sessions),
        Arc::clone(&checkpointer) as Arc<dyn Checkpointer>,
    )
    .with_runtime_clients(
        Arc::new(PlatformClient::new(Arc::new(runtime))),
        Arc::new(ModelFacade::from_gateway(gateway)),
    )
    .with_mcp_connector(Arc::new(DelegatedAuthorizationPipelineMcpConnector {
        connections: Arc::clone(&connections),
        tool_calls: Arc::clone(&tool_calls),
    }));
    if sensitive {
        assembler = assembler.with_tool_policy(sensitive_pipeline_mcp_policy());
    }
    (assembler, captured, connections, tool_calls)
}

async fn run_llm_node_block(action: &str, comment: Option<&str>, expected_reason: &str) {
    let (assembler, captured, tool_calls) = sensitive_llm_node_assembler();
    let mut request = llm_mcp_pipeline_request(
        "release intelligence",
        &["lookup_release"],
        &["lookup_release"],
    );
    let profile = PipelineExecutionProfile::validate(&request, false).expect("pipeline profile");
    let plan = OrdinaryNativeAgentPlan::from_authorized_pipeline(
        &request,
        profile.shell(),
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
        false,
        false,
    )
    .expect("pipeline identity plan");

    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("sensitive LLM-node invocation");
    let (pause_events, graph_interrupt) = collect_pipeline_pause_events(invocation).await;
    let interrupts = assert_llm_node_pause_progress(&pause_events);
    let interrupt_id = interrupts[0]["response_metadata"]["hitl_interrupts"][0]["interrupt_id"]
        .as_str()
        .expect("LLM-node interrupt identity")
        .to_owned();
    let graph_interrupt = graph_interrupt.expect("private graph interruption");
    let binding = pipeline_tool_event_binding(&graph_interrupt, "elitea-agent", plan.session_id())
        .expect("LLM-node confirmation binding");
    assert_eq!(binding.tool_call_id(), "call_mcp");
    assert!(binding.llm_replay().is_some());
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);

    request.binding.request_content_digest = [9; 32];
    request.payload.should_continue = true;
    request.payload.hitl_resume = true;
    request.payload.hitl_action = Some(action.to_owned());
    request.payload.hitl_value = Some(comment.unwrap_or_default().to_owned());
    request.payload.hitl_decisions = vec![json!({
        "interrupt_id": interrupt_id,
        "tool_call_id": "call_mcp",
        "action": action,
        "value": comment.unwrap_or_default(),
    })];
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("same-call LLM-node continuation");
    let browser = collect_pipeline_completion(invocation).await;
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == "continued after block")
    );
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);
    assert_llm_node_blocked_progress(&browser, expected_reason);
    assert_llm_blocked_continuation(&captured, expected_reason);
}

fn assert_llm_node_pause_progress(pause_events: &[Value]) -> Vec<Value> {
    let interrupts = pause_events
        .iter()
        .filter(|event| event["type"] == "agent_hitl_interrupt")
        .cloned()
        .collect::<Vec<_>>();
    assert_eq!(interrupts.len(), 1);
    let model_start = pause_events
        .iter()
        .find(|event| event["type"] == "agent_llm_start")
        .expect("pipeline LLM-node model start");
    assert_eq!(model_start["response_metadata"]["tool_name"], "answer");
    assert_eq!(
        model_start["response_metadata"]["metadata"]["langgraph_node"],
        "answer"
    );
    assert!(
        model_start["response_metadata"]
            .get("parent_agent_path")
            .is_none()
    );
    let tool_start = pause_events
        .iter()
        .find(|event| event["type"] == "agent_tool_start")
        .expect("pipeline LLM-node tool start");
    assert_eq!(tool_start["response_metadata"]["tool_run_id"], "call_mcp");
    assert_eq!(
        tool_start["response_metadata"]["metadata"]["langgraph_node"],
        "answer"
    );
    assert!(
        tool_start["response_metadata"]
            .get("parent_agent_path")
            .is_none()
    );
    interrupts
}

fn assert_llm_node_blocked_progress(browser: &[Value], expected_reason: &str) {
    let tool_end = browser
        .iter()
        .find(|event| {
            event["type"] == "agent_tool_end"
                && event["response_metadata"]["tool_run_id"] == "call_mcp"
        })
        .expect("same-call blocked tool completion");
    assert_eq!(
        tool_end["response_metadata"]["metadata"]["langgraph_node"],
        "answer"
    );
    let projected_blocked: Value = serde_json::from_str(
        tool_end["response_metadata"]["tool_output"]
            .as_str()
            .expect("structured blocked browser tool result"),
    )
    .expect("blocked browser tool result JSON");
    assert_eq!(projected_blocked["type"], "sensitive_tool_blocked");
    assert_eq!(projected_blocked["denial_reason"], expected_reason);
    assert_eq!(
        browser
            .iter()
            .filter(|event| {
                event["type"] == "agent_llm_start"
                    && event["response_metadata"]["tool_name"] == "answer"
            })
            .count(),
        2,
        "replayed call and final answer must not gain a duplicate graph-completion turn"
    );
}

fn sensitive_llm_node_assembler() -> (
    PipelineNativeAgentAssembler,
    CapturedModelRequests,
    Arc<AtomicUsize>,
) {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let token_response = || {
        runtime_response(&json!({
            "schema_version": "elitea.runtime.elitea-client-token.v1",
            "project_id": 17,
            "token": "ephemeral-pipeline-token"
        }))
    };
    let runtime = RuntimeContextClient::with_rpc(
        PipelineRuntimeContextFixture {
            responses: Mutex::new(VecDeque::from([token_response(), token_response()])),
            calls: Arc::new(AtomicUsize::new(0)),
            paths: Arc::new(Mutex::new(Vec::new())),
        },
        RuntimeContextConfig {
            origin: "https://content.internal".to_owned(),
            deadline: Duration::from_secs(1),
            max_response_bytes: 32 * 1_024,
            max_application_response_bytes: 1_024 * 1_024,
            max_attachment_response_bytes: 1_024 * 1_024,
        },
    )
    .expect("pipeline runtime-context fixture");
    let (gateway, captured) = test_model_gateway_client(
        vec![
            TestModelGatewayOutcome::Response(pipeline_mcp_tool_call_response()),
            TestModelGatewayOutcome::Response(pipeline_text_response("continued after block")),
        ],
        test_model_gateway_config(),
    )
    .expect("pipeline model gateway fixture");
    let platform = Arc::new(PlatformClient::new(Arc::new(runtime)));
    let model_facade = Arc::new(ModelFacade::from_gateway(gateway));
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::clone(&sessions),
        Arc::clone(&checkpointer) as Arc<dyn Checkpointer>,
    )
    .with_runtime_clients(platform, model_facade)
    .with_mcp_connector(Arc::new(PipelineMcpConnector {
        connections: Arc::new(AtomicUsize::new(0)),
        tool_calls: Arc::clone(&tool_calls),
        read_only: true,
    }))
    .with_tool_policy(sensitive_pipeline_mcp_policy());
    (assembler, captured, tool_calls)
}

fn assert_llm_blocked_continuation(captured: &CapturedModelRequests, expected_reason: &str) {
    let captured = captured.lock().expect("captured model requests");
    assert_eq!(
        captured.len(),
        2,
        "resume must not replan before tool replay"
    );
    let continuation: Value =
        serde_json::from_slice(&captured[1].body).expect("continuation request JSON");
    let tool_message = continuation["messages"]
        .as_array()
        .expect("continuation messages")
        .iter()
        .find(|message| message["role"] == "tool" && message["tool_call_id"] == "call_mcp")
        .expect("same-call tool response delivered to model");
    let blocked: Value = serde_json::from_str(
        tool_message["content"]
            .as_str()
            .expect("structured tool response JSON"),
    )
    .expect("structured blocked result");
    assert_eq!(blocked["type"], "sensitive_tool_blocked");
    assert_eq!(blocked["blocked_tool_name"], "lookup_release");
    assert_eq!(blocked["denial_reason"], expected_reason);
}

#[tokio::test]
async fn mcp_node_discovers_and_executes_one_read_without_a_model_turn() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let connections = Arc::new(AtomicUsize::new(0));
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let connector = Arc::new(PipelineMcpConnector {
        connections: Arc::clone(&connections),
        tool_calls: Arc::clone(&tool_calls),
        read_only: true,
    });
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::clone(&sessions),
        Arc::clone(&checkpointer) as Arc<dyn Checkpointer>,
    )
    .with_mcp_connector(connector);
    let request = mcp_pipeline_request(
        "release intelligence",
        &["lookup_release"],
        "lookup_release",
    );
    let private_thread = private_pipeline_session_id(&request);
    let mut invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("authorized direct MCP pipeline");
    invocation
        .project_start(timestamp(0))
        .expect("browser start");
    let (mut run, mut projector, completion) = invocation.start().expect("MCP pipeline start");
    while let Some(event) = run.next_event().await.expect("MCP pipeline event") {
        let _ = projector.project(&event).expect("MCP event projection");
    }
    let selected = completion.select().await.expect("MCP result selection");
    let browser = projector
        .finish_after_eos(selected, timestamp(1))
        .expect("MCP browser completion");
    assert_eq!(connections.load(Ordering::Acquire), 1);
    assert_eq!(tool_calls.load(Ordering::Acquire), 1);
    let browser_content = browser
        .into_iter()
        .map(|event| current(&event)["content"].clone())
        .collect::<Vec<_>>();
    assert!(
        browser_content
            .iter()
            .any(|content| content == "{\"release\":\"1.2\",\"risk\":\"low\"}"),
        "unexpected MCP completion: {browser_content:?}"
    );
    let checkpoint = checkpointer
        .load(&private_thread)
        .await
        .expect("checkpoint read")
        .expect("terminal MCP checkpoint");
    assert_eq!(
        checkpoint.state.get("records"),
        Some(&json!({"release": "1.2", "risk": "low"}))
    );
    assert_eq!(
        checkpoint.state.get("messages"),
        Some(&json!([
            {"role": "user", "content": "current"},
            {"role": "assistant", "content": "MCP read complete"}
        ]))
    );
}

#[tokio::test]
async fn mcp_node_rejects_server_declared_effect_before_tool_execution() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let connector = Arc::new(PipelineMcpConnector {
        connections: Arc::new(AtomicUsize::new(0)),
        tool_calls: Arc::clone(&tool_calls),
        read_only: false,
    });
    let assembler =
        PipelineNativeAgentAssembler::with_state(sessions, Arc::new(MemoryCheckpointer::new()))
            .with_mcp_connector(connector);
    let request = mcp_pipeline_request(
        "release intelligence",
        &["lookup_release"],
        "lookup_release",
    );
    let result = assembler.assemble(authorized(&request)).await;
    let Err(error) = result else {
        panic!("effectful direct MCP node was assembled");
    };
    assert_eq!(
        error.code(),
        NativeAgentAssemblyErrorCode::UnsupportedCapability
    );
    assert_eq!(tool_calls.load(Ordering::Acquire), 0);
}

struct NamedReadTool(&'static str);

#[async_trait]
impl Tool for NamedReadTool {
    fn name(&self) -> &str {
        self.0
    }

    fn description(&self) -> &'static str {
        "pipeline tool selection fixture"
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
        _arguments: Value,
    ) -> adk_rust::Result<Value> {
        Ok(json!({"ok": true}))
    }
}

#[tokio::test]
async fn node_toolset_exposes_only_exact_selected_names_in_declared_order() {
    let available: Arc<dyn Toolset> = Arc::new(BasicToolset::new(
        "release_repository",
        vec![
            Arc::new(NamedReadTool("get_issues")),
            Arc::new(NamedReadTool("list_branches_in_repo")),
            Arc::new(NamedReadTool("get_issue")),
        ],
    ));
    let context = Arc::new(SimpleToolContext::new("pipeline-toolset-test"));
    let selected = StrictNodeToolset::new(
        "release_repository",
        available,
        &["get_issue".to_owned(), "get_issues".to_owned()],
    );
    let tools = selected
        .tools(context.clone())
        .await
        .expect("exact selected toolset");
    assert_eq!(
        tools.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        ["get_issue", "get_issues"]
    );

    let missing = StrictNodeToolset::new(
        "release_repository",
        Arc::new(BasicToolset::new(
            "release_repository",
            vec![Arc::new(NamedReadTool("get_issues"))],
        )),
        &["not_available".to_owned()],
    );
    assert!(missing.tools(context).await.is_err());
}

#[tokio::test]
async fn admitted_pipeline_uses_common_runner_and_resumes_exact_private_checkpoint() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let pipeline_assembler =
        PipelineNativeAgentAssembler::with_state(Arc::clone(&sessions), checkpointer.clone());
    let request = pipeline_request();
    let private_thread = private_pipeline_session_id(&request);
    assert_ne!(private_thread, "thread-1");

    let mut fresh_invocation = pipeline_assembler
        .assemble(authorized(&request))
        .await
        .expect("fresh pipeline assembly");
    assert_eq!(
        fresh_invocation
            .project_start(timestamp(0))
            .expect("start")
            .len(),
        1
    );
    let (mut run, mut projector, completion) = fresh_invocation.start().expect("pipeline start");
    let interrupt = run
        .next_event()
        .await
        .expect("pipeline event")
        .expect("HITL interrupt");
    let binding = pipeline_hitl_event_binding(&interrupt, "elitea-agent", &private_thread)
        .expect("private checkpoint binding");
    let interrupt_id = binding.interrupt_id().to_owned();
    let projected = projector
        .project(&interrupt)
        .expect("public interrupt projection")
        .into_iter()
        .map(|event| current(&event))
        .collect::<Vec<_>>();
    assert_eq!(projected.len(), 1);
    assert_eq!(projected[0]["response_metadata"]["thread_id"], "thread-1");
    assert_eq!(
        projected[0]["response_metadata"]["hitl_interrupt"]["interrupt_id"],
        interrupt_id
    );
    assert!(!projected[0].to_string().contains(&private_thread));
    assert!(projector.is_paused());
    assert!(run.next_event().await.expect("paused EOS").is_none());
    drop(completion);

    let resume = resume_request(&interrupt_id);
    let mut resumed_invocation = pipeline_assembler
        .assemble(authorized(&resume))
        .await
        .expect("checkpoint-bound resume assembly");
    resumed_invocation
        .project_start(timestamp(1))
        .expect("resume browser start");
    let (mut run, mut projector, completion) = resumed_invocation.start().expect("resume start");
    let completed = run
        .next_event()
        .await
        .expect("completion event")
        .expect("pipeline completion marker");
    let projected_completion = projector
        .project(&completed)
        .expect("internal completion projection");
    assert!(
        projected_completion.is_empty(),
        "unexpected completion event {} projected {} browser events",
        completed.id,
        projected_completion.len()
    );
    assert!(run.next_event().await.expect("completed EOS").is_none());
    let browser_completion = completion.select().await.expect("completion selection");
    let final_events = projector
        .finish_after_eos(browser_completion, timestamp(2))
        .expect("terminal browser events")
        .into_iter()
        .map(|event| current(&event))
        .collect::<Vec<_>>();
    assert_eq!(final_events.len(), 3);
    assert_eq!(final_events[0]["type"], "pipeline_finish");
    assert_eq!(final_events[0]["content"], "Pipeline completed.");
    assert!(
        final_events
            .iter()
            .all(|event| !event.to_string().contains(&private_thread))
    );

    let checkpoint = checkpointer
        .load(&private_thread)
        .await
        .expect("checkpoint read")
        .expect("terminal checkpoint");
    assert!(
        !serde_json::to_string(&checkpoint.state)
            .expect("checkpoint JSON")
            .contains("pipeline-resume")
    );
    let replay = pipeline_assembler.assemble(authorized(&resume)).await;
    let Err(replay) = replay else {
        panic!("completed interrupt replay was admitted");
    };
    assert_eq!(replay.code(), NativeAgentAssemblyErrorCode::InvalidInput);
}

async fn assert_fresh_printer_pause(
    assembler: &PipelineNativeAgentAssembler,
    fresh: &super::request::AgentExecutionRequest,
) {
    let mut invocation = assembler
        .assemble(authorized(fresh))
        .await
        .expect("fresh Printer assembly");
    invocation
        .project_start(timestamp(0))
        .expect("Printer browser start");
    let (mut run, mut projector, completion) = invocation.start().expect("Printer start");
    let interrupt = run
        .next_event()
        .await
        .expect("Printer event")
        .expect("Printer checkpoint event");
    assert!(
        projector
            .project(&interrupt)
            .expect("Printer projection")
            .is_empty()
    );
    assert!(!projector.is_paused());
    assert!(run.next_event().await.expect("Printer EOS").is_none());
    let selected = completion
        .select()
        .await
        .expect("Printer fallback selection");
    let output = projector
        .finish_after_eos(selected, timestamp(1))
        .expect("Printer public result")
        .into_iter()
        .map(|event| current(&event))
        .collect::<Vec<_>>();
    assert_eq!(
        output
            .iter()
            .map(|event| event["type"].as_str())
            .collect::<Vec<_>>(),
        [Some("agent_response"), Some("full_message")]
    );
    assert!(
        output
            .iter()
            .all(|event| { event["content"] == "Draft ready\n\n-----\n*Continue when ready.*" })
    );
    assert_eq!(output[1]["response_metadata"]["should_continue"], false);
    assert_eq!(output[1]["response_metadata"]["hitl_resume"], false);
}

#[tokio::test]
async fn printer_uses_the_common_runner_as_a_nonterminal_result_then_resumes_from_user_text() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let assembler =
        PipelineNativeAgentAssembler::with_state(Arc::clone(&sessions), checkpointer.clone());
    let fresh = printer_pipeline_request("start", false);
    let private_thread = private_pipeline_session_id(&fresh);
    assert_fresh_printer_pause(&assembler, &fresh).await;

    let resume = printer_pipeline_request("continue now", true);
    let mut invocation = assembler
        .assemble(authorized(&resume))
        .await
        .expect("checkpoint-bound Printer continuation");
    invocation
        .project_start(timestamp(2))
        .expect("Printer resume browser start");
    let (mut run, mut projector, completion) = invocation.start().expect("Printer resume start");
    let completed = run
        .next_event()
        .await
        .expect("Printer completion event")
        .expect("Printer completion marker");
    let progress = projector
        .project(&completed)
        .expect("Printer completion projection")
        .into_iter()
        .map(|event| current(&event))
        .collect::<Vec<_>>();
    assert_eq!(progress.len(), 4);
    assert_eq!(progress[1]["content"], "continue now");
    assert!(
        run.next_event()
            .await
            .expect("Printer completed EOS")
            .is_none()
    );
    let selected = completion
        .select()
        .await
        .expect("Printer completion selection");
    let output = projector
        .finish_after_eos(selected, timestamp(3))
        .expect("Printer terminal result")
        .into_iter()
        .map(|event| current(&event))
        .collect::<Vec<_>>();
    assert_eq!(output[0]["type"], "pipeline_finish");
    assert_eq!(output[0]["content"], "continue now");
    assert_eq!(output[2]["response_metadata"]["should_continue"], true);
    assert_eq!(output[2]["response_metadata"]["hitl_resume"], false);

    let checkpoint = checkpointer
        .load(&private_thread)
        .await
        .expect("Printer checkpoint read")
        .expect("Printer terminal checkpoint");
    assert_eq!(
        checkpoint.state.get("messages"),
        Some(&json!([
            {"role": "user", "content": "start"},
            {"role": "user", "content": "continue now"}
        ]))
    );
    let replay = assembler.assemble(authorized(&resume)).await;
    let Err(replay) = replay else {
        panic!("completed Printer continuation was replayed");
    };
    assert_eq!(replay.code(), NativeAgentAssemblyErrorCode::InvalidInput);
}

#[tokio::test]
async fn state_modifier_result_survives_common_runner_projection_and_eos_completion() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let pipeline_assembler = PipelineNativeAgentAssembler::with_state(
        Arc::clone(&sessions),
        Arc::new(MemoryCheckpointer::new()),
    );
    let mut request = pipeline_request();
    request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("pipeline version")
        .insert("instructions".to_owned(), json!(STATE_MODIFIER_PIPELINE));

    let mut invocation = pipeline_assembler
        .assemble(authorized(&request))
        .await
        .expect("state modifier pipeline assembly");
    invocation
        .project_start(timestamp(0))
        .expect("browser start");
    let (mut run, mut projector, completion) = invocation.start().expect("pipeline start");
    let result = run
        .next_event()
        .await
        .expect("pipeline result read")
        .expect("pipeline result event");
    let progress = projector
        .project(&result)
        .expect("pipeline result projection")
        .into_iter()
        .map(|event| current(&event))
        .collect::<Vec<_>>();
    assert_eq!(progress.len(), 4);
    assert_eq!(progress[1]["content"], "Hello, current");
    assert!(run.next_event().await.expect("pipeline EOS").is_none());

    let selected = completion.select().await.expect("completion selection");
    let completed = projector
        .finish_after_eos(selected, timestamp(1))
        .expect("browser completion")
        .into_iter()
        .map(|event| current(&event))
        .collect::<Vec<_>>();
    assert_eq!(completed.len(), 3);
    assert!(
        completed
            .iter()
            .all(|event| event["content"] == "Hello, current")
    );
}
