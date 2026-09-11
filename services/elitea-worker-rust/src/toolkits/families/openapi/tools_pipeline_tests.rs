use std::collections::{HashMap, VecDeque};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use adk_rust::futures::StreamExt as _;
use adk_rust::graph::{Checkpointer, GraphAgent, MemoryCheckpointer};
use adk_rust::runner::Runner;
use adk_rust::session::{CreateRequest, GetRequest, InMemorySessionService, SessionService};
use adk_rust::{Content, Event, SessionId, Tool, UserId};
use reqwest::StatusCode;
use serde_json::{Map, Value, json};

use super::{RejectedTokenTransport, operation_tool};
use crate::agents::events::pipeline_mcp_auth_event_binding;
use crate::agents::graph::compiler::{PipelineDefinition, PipelineNodeRuntimes};
use crate::agents::graph::resume::PipelineMcpAuthorizationContinuation;
use crate::agents::graph::{
    DirectToolExecutionError, DirectToolSelection, EliteaGraphAgent, PipelineDirectToolResolver,
    ResolvedDirectTool,
};
use crate::agents::request::{AgentExecutionPayload, NextInputSuggestionPolicy, UserInput};

const ROOT: &str = "openapi-root";
const THREAD: &str = "openapi-expiry";

const PIPELINE: &str = r"
state:
  first_result: str
  second_result: str
  third_result: str
  messages: list
entry_point: first
nodes:
  - id: first
    type: toolkit
    toolkit_name: Records API
    tool: list_records
    input: []
    input_mapping:
      marker: {type: fixed, value: first}
    output: [first_result]
    transition: second
  - id: second
    type: toolkit
    toolkit_name: Records API
    tool: list_records
    input: []
    input_mapping:
      marker: {type: fixed, value: second}
    output: [second_result]
    transition: third
  - id: third
    type: toolkit
    toolkit_name: Records API
    tool: list_records
    input: []
    input_mapping:
      marker: {type: fixed, value: third}
    output: [third_result]
    transition: END
";

struct Resolver(Arc<dyn Tool>);

impl PipelineDirectToolResolver for Resolver {
    fn resolve(
        &self,
        selection: &DirectToolSelection,
    ) -> Result<ResolvedDirectTool, DirectToolExecutionError> {
        if selection.alias() != "Records API" || selection.tool() != self.0.name() {
            return Err(DirectToolExecutionError::Unavailable);
        }
        Ok(ResolvedDirectTool::new(Arc::clone(&self.0), None))
    }
}

#[tokio::test]
async fn openapi_expiry_resumes_only_the_paused_node_or_stops_on_skip() {
    for (authorize, refreshed_status) in [
        (true, StatusCode::OK),
        (false, StatusCode::OK),
        (true, StatusCode::UNAUTHORIZED),
    ] {
        let transport = pipeline_transport(refreshed_status);
        let definition = PipelineDefinition::from_yaml(PIPELINE).unwrap();
        let checkpointer = Arc::new(MemoryCheckpointer::new());
        let sessions = session_service().await;
        let original = operation_tool(true, "private-expired-token", Arc::clone(&transport)).await;
        let graph = definition
            .compile_with_runtime(
                ROOT,
                checkpointer.clone(),
                None,
                &PipelineNodeRuntimes::new(None, Some(Arc::new(Resolver(original))), None),
            )
            .unwrap();
        let paused = run_graph(graph, sessions.clone(), "read the records").await;
        let binding = pipeline_mcp_auth_event_binding(paused.last().unwrap(), ROOT, THREAD)
            .expect("real OpenAPI 401 must pause its owning node");
        assert_eq!(binding.pending_node_name(), "second");
        assert_eq!(transport.calls.load(Ordering::SeqCst), 2);
        let session = sessions
            .get(GetRequest {
                app_name: "elitea".into(),
                user_id: "user-1".into(),
                session_id: THREAD.into(),
                num_recent_events: None,
                after: None,
            })
            .await
            .unwrap();
        let mut payload = resume_payload(authorize, binding.server_url());
        if authorize {
            // Same issuer, different configuration must not resume this call.
            payload.mcp_tokens = Map::from_iter([(
                "config-2:https://issuer.example.test/tenant".into(),
                json!({"access_token":"wrong-token"}),
            )]);
            assert!(
                PipelineMcpAuthorizationContinuation::from_payload(&payload)
                    .unwrap()
                    .resolve(session.as_ref(), checkpointer.as_ref(), ROOT, THREAD)
                    .await
                    .is_err()
            );
            assert_eq!(transport.calls.load(Ordering::SeqCst), 2);
            payload = resume_payload(true, binding.server_url());
        }
        let resume = PipelineMcpAuthorizationContinuation::from_payload(&payload)
            .unwrap()
            .resolve(session.as_ref(), checkpointer.as_ref(), ROOT, THREAD)
            .await
            .unwrap();
        let rebuilt = operation_tool(true, "private-rotated-token", Arc::clone(&transport)).await;
        let graph = definition
            .compile_with_runtime(
                ROOT,
                checkpointer.clone(),
                Some(resume),
                &PipelineNodeRuntimes::new(None, Some(Arc::new(Resolver(rebuilt))), None),
            )
            .unwrap();
        run_graph(graph, sessions, "continue").await;
        let checkpoint = checkpointer.load(THREAD).await.unwrap().unwrap();
        let refreshed = authorize && refreshed_status == StatusCode::OK;
        assert_requests(&transport, authorize, refreshed);
        assert!(checkpoint.pending_nodes.is_empty());
        assert_eq!(checkpoint.state["first_result"], "private-provider-body");
        if refreshed {
            assert_eq!(
                transport.calls.load(Ordering::SeqCst),
                4,
                "completed first node must not run again"
            );
            assert_eq!(checkpoint.state["second_result"], "private-provider-body");
            assert_eq!(checkpoint.state["third_result"], "private-provider-body");
        } else {
            assert_eq!(
                transport.calls.load(Ordering::SeqCst),
                if authorize { 3 } else { 2 },
                "Skip or a rejected refreshed token must not dispatch downstream work"
            );
            assert_eq!(checkpoint.state["second_result"], Value::Null);
            assert!(
                checkpoint.state["_pipeline_blocked"]
                    .as_str()
                    .unwrap()
                    .contains(if authorize {
                        "not available"
                    } else {
                        "was skipped"
                    })
            );
        }
    }
}

fn pipeline_transport(refreshed_status: StatusCode) -> Arc<RejectedTokenTransport> {
    Arc::new(RejectedTokenTransport {
        responses: Mutex::new(VecDeque::from([
            StatusCode::OK,
            StatusCode::UNAUTHORIZED,
            refreshed_status,
            StatusCode::OK,
        ])),
        calls: AtomicUsize::new(0),
        requests: Mutex::new(Vec::new()),
    })
}

fn assert_requests(transport: &RejectedTokenTransport, authorize: bool, refreshed: bool) {
    let requests = transport.requests.lock().unwrap();
    let mut expected = vec![
        ("first", "Bearer private-expired-token"),
        ("second", "Bearer private-expired-token"),
    ];
    if authorize {
        expected.push(("second", "Bearer private-rotated-token"));
    }
    if refreshed {
        expected.push(("third", "Bearer private-rotated-token"));
    }
    assert_eq!(requests.len(), expected.len());
    for ((url, bearer), (node, token)) in requests.iter().zip(expected) {
        assert_eq!(
            url,
            &format!("https://api.example.test/v1/records?marker={node}")
        );
        assert_eq!(bearer, token);
    }
}

async fn session_service() -> Arc<InMemorySessionService> {
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "elitea".into(),
            user_id: "user-1".into(),
            session_id: Some(THREAD.into()),
            state: HashMap::new(),
        })
        .await
        .unwrap();
    sessions
}

async fn run_graph(
    graph: GraphAgent,
    sessions: Arc<InMemorySessionService>,
    input: &str,
) -> Vec<Event> {
    let service: Arc<dyn SessionService> = sessions;
    let runner = Runner::builder()
        .app_name("elitea")
        .agent(Arc::new(EliteaGraphAgent::new(graph)))
        .session_service(service)
        .build()
        .unwrap();
    let mut stream = runner
        .run(
            UserId::new("user-1").unwrap(),
            SessionId::new(THREAD).unwrap(),
            Content::new("user").with_text(input),
        )
        .await
        .unwrap();
    let mut events = Vec::new();
    while let Some(event) = stream.next().await {
        events.push(event.unwrap());
    }
    events
}

fn resume_payload(authorize: bool, resource: &str) -> AgentExecutionPayload {
    AgentExecutionPayload {
        llm: Map::new(),
        chat_history: Vec::new(),
        user_input: UserInput::Text("continue".into()),
        thread_id: Some(THREAD.into()),
        checkpoint_id: None,
        debug: false,
        tools: Vec::new(),
        application: Map::new(),
        internal_tools: Vec::new(),
        steps_limit: None,
        mcp_tokens: if authorize {
            Map::from_iter([(
                resource.into(),
                json!({"access_token":"private-rotated-token"}),
            )])
        } else {
            Map::new()
        },
        ignored_mcp_servers: Vec::new(),
        user_declined_mcp_servers: if authorize {
            Vec::new()
        } else {
            vec![json!({"server_url":resource})]
        },
        should_continue: true,
        hitl_resume: false,
        hitl_action: None,
        hitl_value: None,
        hitl_decisions: Vec::new(),
        execution_generation: Some("generation-1".into()),
        is_regenerate: false,
        meta: Map::new(),
        conversation_id: Some("conversation-1".into()),
        persona: "generic".into(),
        context_settings: Map::new(),
        supports_vision: false,
        return_chat_history: false,
        invoked_skills: Vec::new(),
        applied_skills: Vec::new(),
        auto_approve_sensitive_actions: false,
        attached_skills: Vec::new(),
        input_attachments: Vec::new(),
        parallel_reconcile: None,
        parallel_terminal_errors: Vec::new(),
        exception_handling_enabled: None,
        debug_mode: None,
        next_input_suggestion: NextInputSuggestionPolicy::default(),
        toolkit_guardrails: None,
        truncated_content: None,
    }
}
