use std::collections::{BTreeMap, HashMap};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use adk_rust::futures::StreamExt as _;
use adk_rust::graph::interrupt::Interrupt;
use adk_rust::graph::{Checkpointer, END, ExecutionConfig, MemoryCheckpointer, Node, NodeContext};
use adk_rust::runner::Runner;
use adk_rust::session::{CreateRequest, GetRequest, InMemorySessionService, SessionService};
use adk_rust::{Content, Part, SessionId, Tool, ToolContext, UserId};
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use super::compiler::PipelineNodeRuntimes;
use super::direct_tool::{
    DIRECT_TOOL_RESUME_STATE_KEY, DirectToolExecutionError, DirectToolInputMapping, DirectToolNode,
    DirectToolNodeDefinition, DirectToolNodeKind, DirectToolSelection, PipelineDirectToolResolver,
    ResolvedDirectTool,
};
use super::{EliteaGraphAgent, compiler::PipelineDefinition};
use crate::agents::events::{pipeline_mcp_auth_event_binding, pipeline_tool_event_binding};
use crate::agents::graph::resume::{
    PipelineContinuationDecision, PipelineMcpAuthorizationContinuation, PipelineResumeErrorCode,
};
use crate::agents::request::{AgentExecutionPayload, NextInputSuggestionPolicy, UserInput};
use crate::agents::runtime::NativeAgentInvocation;
use crate::toolkits::{
    SensitiveToolPolicy, ToolAdmissionPolicy, delegated_authorization_error_fixture,
};

const TOOLKIT_NODE: &str = r#"
id: lookup
type: toolkit
toolkit_name: Customer Support
tool: search_records
input_mapping:
  query:
    type: fstring
    value: "ticket {ticket_id}"
  limit:
    type: fixed
    value: 5
  filters:
    type: variable
    value: filters
input: [ticket_id, filters]
output: [report, messages]
structured_output: true
transition: END
"#;

fn state_types() -> BTreeMap<String, String> {
    BTreeMap::from([
        ("ticket_id".to_owned(), "int".to_owned()),
        ("filters".to_owned(), "dict".to_owned()),
        ("report".to_owned(), "dict".to_owned()),
        ("messages".to_owned(), "list".to_owned()),
    ])
}

#[test]
fn toolkit_node_admits_ui_yaml_typed_mapping_and_structured_output() {
    let node = DirectToolNodeDefinition::from_yaml(TOOLKIT_NODE).expect("Toolkit node");
    assert_eq!(node.id(), "lookup");
    assert_eq!(node.selection().alias(), "Customer Support");
    assert_eq!(node.selection().tool(), "search_records");
    assert_eq!(node.input_keys(), ["ticket_id", "filters"]);
    assert_eq!(node.output_keys(), ["report", "messages"]);
    assert!(node.structured_output());
    assert_eq!(node.transition(), Some("END"));
    assert!(matches!(
        node.input_mapping().get("limit"),
        Some(DirectToolInputMapping::Fixed(value)) if value == &json!(5)
    ));
    assert!(matches!(
        node.input_mapping().get("filters"),
        Some(DirectToolInputMapping::Variable(value)) if value == "filters"
    ));
    assert!(node.config_digest().iter().any(|byte| *byte != 0));

    let mcp =
        DirectToolNodeDefinition::from_yaml(&TOOLKIT_NODE.replace("type: toolkit", "type: mcp"))
            .expect("MCP node shares the direct-call mapping contract");
    assert_eq!(mcp.selection().kind(), DirectToolNodeKind::Mcp);
    assert_ne!(mcp.config_digest(), node.config_digest());

    let legacy = DirectToolNodeDefinition::from_yaml(
        "id: lookup\ntype: toolkit\ntoolkit_name: support\ntool: search\ntransition: END\n",
    )
    .expect("legacy default mapping");
    assert_eq!(legacy.input_keys(), ["messages"]);
    assert!(matches!(
        legacy.input_mapping().get("messages"),
        Some(DirectToolInputMapping::Variable(value)) if value == "messages"
    ));
}

#[test]
fn toolkit_node_rejects_ambiguous_or_unbounded_configuration() {
    for yaml in [
        TOOLKIT_NODE.replace("type: toolkit", "type: function"),
        TOOLKIT_NODE.replace("tool: search_records", "tool: ''"),
        TOOLKIT_NODE.replace("output: [report, messages]", "output: [report, report]"),
        TOOLKIT_NODE.replace("type: variable", "type: expression"),
        TOOLKIT_NODE.replace("output: [report, messages]", "output: [messages]"),
        TOOLKIT_NODE.replace("transition: END", "transition: 'bad/path'"),
    ] {
        assert!(DirectToolNodeDefinition::from_yaml(&yaml).is_err());
    }
}

#[derive(Default)]
struct InvocationCapture {
    calls: usize,
    arguments: Value,
    function_call_id: String,
    session_id: String,
}

struct FixtureTool {
    name: String,
    read_only: bool,
    response: Value,
    capture: Arc<Mutex<InvocationCapture>>,
}

#[async_trait]
impl Tool for FixtureTool {
    fn name(&self) -> &str {
        &self.name
    }

    fn description(&self) -> &'static str {
        "direct Toolkit node fixture"
    }

    fn is_read_only(&self) -> bool {
        self.read_only
    }

    async fn execute(
        &self,
        context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        let mut capture = self.capture.lock().expect("capture lock");
        capture.calls += 1;
        capture.arguments = arguments;
        capture.function_call_id = context.function_call_id().to_owned();
        capture.session_id = context.session_id().to_owned();
        Ok(self.response.clone())
    }
}

struct FixtureResolver {
    alias: String,
    tool: Arc<dyn Tool>,
    sensitive: Option<SensitiveToolPolicy>,
}

struct AuthorizationFixtureTool {
    authorized: Arc<AtomicBool>,
    toolkit_type: &'static str,
}

#[async_trait]
impl Tool for AuthorizationFixtureTool {
    fn name(&self) -> &'static str {
        "search_records"
    }

    fn description(&self) -> &'static str {
        "authorization fixture"
    }

    fn is_read_only(&self) -> bool {
        true
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_rust::Result<Value> {
        if self.authorized.load(Ordering::Acquire) {
            Ok(json!({
                "report": {"authorized": true},
                "messages": [{"role": "assistant", "content": "authorized result"}],
            }))
        } else {
            Err(delegated_authorization_error_fixture(self.toolkit_type))
        }
    }
}

impl PipelineDirectToolResolver for FixtureResolver {
    fn resolve(
        &self,
        selection: &DirectToolSelection,
    ) -> Result<ResolvedDirectTool, DirectToolExecutionError> {
        if selection.alias() == self.alias && selection.tool() == self.tool.name() {
            Ok(ResolvedDirectTool::new(
                Arc::clone(&self.tool),
                self.sensitive.clone(),
            ))
        } else {
            Err(DirectToolExecutionError::Unavailable)
        }
    }
}

fn fixture_runtime(
    response: Value,
    read_only: bool,
) -> (
    Arc<dyn PipelineDirectToolResolver>,
    Arc<Mutex<InvocationCapture>>,
) {
    let capture = Arc::new(Mutex::new(InvocationCapture::default()));
    let tool: Arc<dyn Tool> = Arc::new(FixtureTool {
        name: "search_records".to_owned(),
        read_only,
        response,
        capture: Arc::clone(&capture),
    });
    (
        Arc::new(FixtureResolver {
            alias: "Customer Support".to_owned(),
            tool,
            sensitive: None,
        }),
        capture,
    )
}

fn sensitive_fixture_runtime(
    response: Value,
) -> (
    Arc<dyn PipelineDirectToolResolver>,
    Arc<Mutex<InvocationCapture>>,
) {
    sensitive_fixture_runtime_for(response, "customer_support")
}

fn sensitive_fixture_runtime_for(
    response: Value,
    toolkit_type: &str,
) -> (
    Arc<dyn PipelineDirectToolResolver>,
    Arc<Mutex<InvocationCapture>>,
) {
    let capture = Arc::new(Mutex::new(InvocationCapture::default()));
    let tool: Arc<dyn Tool> = Arc::new(FixtureTool {
        name: "search_records".to_owned(),
        read_only: true,
        response,
        capture: Arc::clone(&capture),
    });
    let mut sensitive = Map::new();
    sensitive.insert(toolkit_type.to_owned(), json!(["search_records"]));
    let policy_config = json!({
        "toolkit_security": {
            "sensitive_tools": sensitive,
            "sensitive_action_company_name": "Example Corp"
        }
    });
    let policy = ToolAdmissionPolicy::from_runtime_config(
        policy_config.as_object().expect("runtime policy"),
    )
    .expect("sensitive policy")
    .sensitive_tool(toolkit_type, "Customer Support", "search_records")
    .expect("sensitive action");
    (
        Arc::new(FixtureResolver {
            alias: "Customer Support".to_owned(),
            tool,
            sensitive: Some(policy),
        }),
        capture,
    )
}

#[tokio::test]
async fn native_toolkit_node_executes_once_with_checkpoint_step_identity_and_projects_state() {
    let definition = DirectToolNodeDefinition::from_yaml(TOOLKIT_NODE).expect("Toolkit node");
    let (resolver, capture) = fixture_runtime(
        json!({
            "report": {"id": 7, "status": "open"},
            "messages": [{"role": "assistant", "content": "found"}]
        }),
        true,
    );
    let node = DirectToolNode::new(definition, state_types(), resolver);
    let state = HashMap::from([
        ("ticket_id".to_owned(), json!(42)),
        ("filters".to_owned(), json!({"active": true})),
    ]);
    let output = node
        .execute(&NodeContext::new(
            state,
            ExecutionConfig::new("pipeline-thread"),
            7,
        ))
        .await
        .expect("direct Toolkit node");
    assert_eq!(
        output.updates.get("report"),
        Some(&json!({"id": 7, "status": "open"}))
    );
    assert_eq!(
        output.updates.get("messages"),
        Some(&json!([{"role": "assistant", "content": "found"}]))
    );
    let capture = capture.lock().expect("capture lock");
    assert_eq!(capture.calls, 1);
    assert_eq!(
        capture.arguments,
        json!({"filters":{"active":true},"limit":5,"query":"ticket 42"})
    );
    assert_eq!(capture.function_call_id, "pipeline:lookup:7");
    assert_eq!(capture.session_id, "pipeline-thread");
}

#[tokio::test]
async fn sensitive_toolkit_node_pauses_then_approval_returns_the_normal_tool_result() {
    let definition = DirectToolNodeDefinition::from_yaml(TOOLKIT_NODE).expect("Toolkit node");
    let (resolver, capture) = sensitive_fixture_runtime(json!({
        "report": {"id": 42},
        "messages": [{"role":"assistant","content":"approved read"}]
    }));
    let node = DirectToolNode::new(definition, state_types(), resolver);
    let state = HashMap::from([
        ("ticket_id".to_owned(), json!(42)),
        ("filters".to_owned(), json!({"token": "must-not-leak"})),
    ]);
    let paused = node
        .execute(&NodeContext::new(
            state.clone(),
            ExecutionConfig::new("thread-sensitive"),
            7,
        ))
        .await
        .expect("sensitive pause");
    assert_eq!(capture.lock().expect("capture lock").calls, 0);
    let Interrupt::Dynamic { data, .. } = paused.interrupt.expect("dynamic interrupt") else {
        panic!("Toolkit confirmation must use a dynamic interrupt");
    };
    let data = data.expect("interrupt data");
    assert_eq!(data["tool_call_id"], "pipeline:lookup:7");
    assert_eq!(data["tool_args"]["filters"]["token"], "***");
    assert_eq!(data["guardrail_type"], "sensitive_tool");

    let mut resumed = state;
    resumed.insert(
        DIRECT_TOOL_RESUME_STATE_KEY.to_owned(),
        json!({
            "lookup": {
                "definition_digest": data["definition_digest"],
                "tool_call_id": data["tool_call_id"],
                "argument_digest": data["argument_digest"],
                "action": "approve",
                "value": "",
            }
        }),
    );
    let output = node
        .execute(&NodeContext::new(
            resumed,
            ExecutionConfig::new("thread-sensitive"),
            7,
        ))
        .await
        .expect("approved execution");
    assert_eq!(capture.lock().expect("capture lock").calls, 1);
    assert_eq!(output.updates[DIRECT_TOOL_RESUME_STATE_KEY], json!({}));
    assert_eq!(output.updates["report"], json!({"id": 42}));
    assert_eq!(
        output.updates["messages"],
        json!([{"role":"assistant","content":"approved read"}])
    );
}

#[tokio::test]
async fn direct_nodes_share_one_delegated_authorization_interrupt_and_safe_resume() {
    for (node_type, toolkit_type) in [
        ("mcp", "mcp"),
        ("toolkit", "sharepoint"),
        ("toolkit", "openapi"),
    ] {
        assert_direct_delegated_authorization(node_type, toolkit_type).await;
    }
}

async fn assert_direct_delegated_authorization(node_type: &str, toolkit_type: &'static str) {
    let yaml = TOOLKIT_NODE.replace("type: toolkit", &format!("type: {node_type}"));
    let definition = DirectToolNodeDefinition::from_yaml(&yaml).expect("direct node");
    let authorized = Arc::new(AtomicBool::new(false));
    let resolver: Arc<dyn PipelineDirectToolResolver> = Arc::new(FixtureResolver {
        alias: "Customer Support".to_owned(),
        tool: Arc::new(AuthorizationFixtureTool {
            authorized: Arc::clone(&authorized),
            toolkit_type,
        }),
        sensitive: None,
    });
    let node = DirectToolNode::new(definition, state_types(), resolver);
    let state = HashMap::from([
        ("ticket_id".to_owned(), json!(42)),
        ("filters".to_owned(), json!({"private": "must-not-leak"})),
    ]);
    let paused = node
        .execute(&NodeContext::new(
            state.clone(),
            ExecutionConfig::new("thread-mcp-auth"),
            4,
        ))
        .await
        .expect("delegated authorization pause");
    let Interrupt::Dynamic { data, .. } = paused.interrupt.expect("dynamic auth interrupt") else {
        panic!("delegated authorization must use a dynamic interrupt");
    };
    let data = data.expect("authorization interrupt data");
    assert_eq!(data["guardrail_type"], "mcp_auth");
    assert_eq!(data["toolkit_type"], toolkit_type);
    assert_eq!(data["available_actions"], json!(["authorize", "skip"]));
    assert_eq!(data["tool_call_id"], "pipeline:lookup:4");
    assert_eq!(data["tool_args"], json!({}));
    assert!(!data.to_string().contains("must-not-leak"));

    let decision = |action: &str| {
        json!({
            "lookup": {
                "definition_digest": data["definition_digest"],
                "tool_call_id": data["tool_call_id"],
                "argument_digest": data["argument_digest"],
                "action": action,
            }
        })
    };
    let mut skipped = state.clone();
    skipped.insert(DIRECT_TOOL_RESUME_STATE_KEY.to_owned(), decision("skip"));
    let skipped = node
        .execute(&NodeContext::new(
            skipped,
            ExecutionConfig::new("thread-mcp-auth"),
            4,
        ))
        .await
        .expect("delegated authorization skip");
    assert_eq!(skipped.goto, Some(vec![END.to_owned()]));
    assert_eq!(skipped.updates["report"], Value::Null);
    assert!(
        skipped.updates["_pipeline_blocked"]
            .as_str()
            .is_some_and(|message| message.contains("was skipped"))
    );

    authorized.store(true, Ordering::Release);
    let mut resumed = state;
    resumed.insert(
        DIRECT_TOOL_RESUME_STATE_KEY.to_owned(),
        decision("authorize"),
    );
    let resumed = node
        .execute(&NodeContext::new(
            resumed,
            ExecutionConfig::new("thread-mcp-auth"),
            4,
        ))
        .await
        .expect("authorized direct execution");
    assert_eq!(resumed.updates[DIRECT_TOOL_RESUME_STATE_KEY], json!({}));
    assert_eq!(resumed.updates["report"], json!({"authorized": true}));
}

#[tokio::test]
async fn blocked_sensitive_direct_node_stops_with_correlated_result_and_clean_state() {
    for (node_type, toolkit_type) in [("toolkit", "customer_support"), ("mcp", "mcp")] {
        for (action, comment, expected_reason) in [
            (
                "reject",
                None,
                "This exact sensitive tool call was declined and was not executed.",
            ),
            (
                "block_with_comment",
                Some("Do not read this customer record."),
                "Do not read this customer record.",
            ),
        ] {
            assert_blocked_direct_node(node_type, toolkit_type, action, comment, expected_reason)
                .await;
        }
    }
}

async fn assert_blocked_direct_node(
    node_type: &str,
    toolkit_type: &str,
    action: &str,
    comment: Option<&str>,
    expected_reason: &str,
) {
    let yaml = TOOLKIT_NODE.replace("type: toolkit", &format!("type: {node_type}"));
    let definition = DirectToolNodeDefinition::from_yaml(&yaml).expect("direct-tool node");
    let (resolver, capture) =
        sensitive_fixture_runtime_for(json!({"report": {"unexpected": true}}), toolkit_type);
    let node = DirectToolNode::new(definition, state_types(), resolver);
    let state = HashMap::from([
        ("ticket_id".to_owned(), json!(42)),
        ("filters".to_owned(), json!({})),
    ]);
    let paused = node
        .execute(&NodeContext::new(
            state.clone(),
            ExecutionConfig::new("thread-blocked"),
            3,
        ))
        .await
        .expect("sensitive pause");
    let Interrupt::Dynamic { data, .. } = paused.interrupt.expect("dynamic interrupt") else {
        panic!("direct confirmation must use a dynamic interrupt");
    };
    let data = data.expect("interrupt data");
    let mut decision = json!({
        "definition_digest": data["definition_digest"],
        "tool_call_id": data["tool_call_id"],
        "argument_digest": data["argument_digest"],
        "action": action,
    });
    if let Some(comment) = comment {
        decision
            .as_object_mut()
            .expect("decision object")
            .insert("value".to_owned(), json!(comment));
    }
    let mut resumed = state;
    resumed.insert(
        DIRECT_TOOL_RESUME_STATE_KEY.to_owned(),
        json!({"lookup": decision}),
    );
    let output = node
        .execute(&NodeContext::new(
            resumed,
            ExecutionConfig::new("thread-blocked"),
            3,
        ))
        .await
        .expect("blocked terminal result");
    assert_eq!(capture.lock().expect("capture lock").calls, 0);
    assert_eq!(output.goto, Some(vec![END.to_owned()]));
    assert!(!output.updates.contains_key("report"));
    let public = output.updates["_pipeline_blocked"]
        .as_str()
        .expect("formatted blocked message");
    let expected_public = format!(
        "**Pipeline stopped** — the action **search_records** (toolkit type: *{toolkit_type}*, node: *lookup*) was **blocked** by user.\n\nDownstream nodes that depend on `search_records` output were skipped to prevent invalid data.\n\n> **Tip:** Regenerate this message to re-trigger the approval request and try again."
    );
    assert_eq!(public, expected_public);
    let messages = output.updates["messages"]
        .as_array()
        .expect("blocked messages");
    assert_eq!(messages[0]["role"], "tool");
    assert_eq!(messages[0]["tool_call_id"], "pipeline:lookup:3");
    assert_eq!(messages[0]["content"]["type"], "sensitive_tool_blocked");
    assert_eq!(messages[0]["content"]["blocked_toolkit_type"], toolkit_type);
    assert_eq!(messages[0]["content"]["message"], public);
    assert_eq!(messages[0]["content"]["denial_reason"], expected_reason);
    assert_eq!(messages[1]["role"], "assistant");
    assert_eq!(messages[1]["content"], public);
}

#[tokio::test]
async fn direct_tool_output_modes_match_function_node_business_projection() {
    let no_outputs = DirectToolNodeDefinition::from_yaml(
        "id: lookup\ntype: toolkit\ntoolkit_name: Customer Support\ntool: search_records\n",
    )
    .expect("no-output node");
    let (resolver, _) = fixture_runtime(json!({"count": 2}), true);
    let output = DirectToolNode::new(no_outputs, state_types(), resolver)
        .execute(&NodeContext::new(
            HashMap::from([("messages".to_owned(), json!([]))]),
            ExecutionConfig::new("thread"),
            0,
        ))
        .await
        .expect("message projection");
    assert_eq!(
        output.updates.get("messages"),
        Some(&json!([{"role":"assistant","content":"{\"count\":2}"}]))
    );

    let first_output = DirectToolNodeDefinition::from_yaml(
        "id: lookup\ntype: toolkit\ntoolkit_name: Customer Support\ntool: search_records\noutput: [report]\ntransition: END\n",
    )
    .expect("first-output node");
    let (resolver, _) = fixture_runtime(json!({"count": 2}), true);
    let output = DirectToolNode::new(first_output, state_types(), resolver)
        .execute(&NodeContext::new(
            HashMap::from([("messages".to_owned(), json!([]))]),
            ExecutionConfig::new("thread"),
            0,
        ))
        .await
        .expect("data projection");
    assert_eq!(output.updates.get("report"), Some(&json!({"count": 2})));
}

#[tokio::test]
async fn effect_or_wrong_structured_shape_fails_without_checkpoint_corruption() {
    let definition = DirectToolNodeDefinition::from_yaml(TOOLKIT_NODE).expect("Toolkit node");
    let (effect_resolver, effect_capture) = fixture_runtime(json!({"report": {}}), false);
    let effect = DirectToolNode::new(definition.clone(), state_types(), effect_resolver)
        .execute(&NodeContext::new(
            HashMap::from([
                ("ticket_id".to_owned(), json!(42)),
                ("filters".to_owned(), json!({})),
            ]),
            ExecutionConfig::new("thread"),
            0,
        ))
        .await;
    assert!(effect.is_err());
    assert_eq!(effect_capture.lock().expect("capture lock").calls, 0);

    let (wrong_resolver, _) = fixture_runtime(json!({"report": []}), true);
    let wrong = DirectToolNode::new(definition, state_types(), wrong_resolver)
        .execute(&NodeContext::new(
            HashMap::from([
                ("ticket_id".to_owned(), json!(42)),
                ("filters".to_owned(), json!({})),
            ]),
            ExecutionConfig::new("thread"),
            0,
        ))
        .await;
    assert!(wrong.is_err());
}

#[tokio::test]
async fn compiled_toolkit_graph_checkpoints_result_without_model_turn() {
    let definition = PipelineDefinition::from_yaml(
        r#"
state:
  ticket_id:
    type: int
    value: 9
  filters:
    type: dict
    value: {active: true}
  report: dict
  messages: list
entry_point: lookup
nodes:
  - id: lookup
    type: toolkit
    toolkit_name: Customer Support
    tool: search_records
    input_mapping:
      query: {type: fstring, value: "ticket {ticket_id}"}
      filters: {type: variable, value: filters}
    input: [ticket_id, filters]
    output: [report, messages]
    structured_output: true
    transition: END
"#,
    )
    .expect("pipeline with Toolkit node");
    let (resolver, capture) = fixture_runtime(
        json!({
            "report": {"id": 9},
            "messages": [{"role":"assistant","content":"read complete"}]
        }),
        true,
    );
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let graph = definition
        .compile_with_runtime(
            "pipeline-root",
            checkpointer.clone(),
            None,
            &PipelineNodeRuntimes::new(None, Some(resolver), None),
        )
        .expect("compiled Toolkit graph");
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "elitea".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: Some("toolkit-pipeline-thread".to_owned()),
            state: HashMap::new(),
        })
        .await
        .expect("pipeline session");
    let session_service: Arc<dyn SessionService> = sessions;
    let runner = Runner::builder()
        .app_name("elitea")
        .agent(Arc::new(EliteaGraphAgent::new(graph)))
        .session_service(session_service)
        .build()
        .expect("pipeline runner");
    let mut running = NativeAgentInvocation::new(
        runner,
        UserId::new("user-1").expect("fixture user"),
        SessionId::new("toolkit-pipeline-thread").expect("fixture session"),
        Content::new("user").with_text("lookup"),
    )
    .start()
    .expect("Toolkit pipeline invocation");
    let mut final_text = None;
    while let Some(event) = running.next_event().await.expect("pipeline event") {
        final_text = event.content().map(|content| {
            content
                .parts
                .iter()
                .filter_map(Part::text)
                .collect::<Vec<_>>()
                .join("")
        });
    }
    assert_eq!(final_text.as_deref(), Some("{\"id\":9}"));
    assert_eq!(capture.lock().expect("capture lock").calls, 1);
    let checkpoint = checkpointer
        .load("toolkit-pipeline-thread")
        .await
        .expect("checkpoint load")
        .expect("Toolkit graph checkpoint");
    assert_eq!(checkpoint.state.get("report"), Some(&json!({"id": 9})));
    assert_eq!(
        checkpoint.state.get("messages"),
        Some(&json!([
            {"role":"user","content":"lookup"},
            {"role":"assistant","content":"read complete"}
        ]))
    );
}

#[tokio::test]
async fn sensitive_toolkit_graph_resumes_exact_checkpoint_and_executes_once_after_approval() {
    const ROOT: &str = "pipeline-root";
    const THREAD: &str = "sensitive-toolkit-thread";
    let definition = sensitive_pipeline_definition();
    let (resolver, capture) = sensitive_fixture_runtime(json!({
        "report": {"id": 9},
        "messages": [{"role":"assistant","content":"approved read"}]
    }));
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let sessions = sensitive_pipeline_session(THREAD).await;
    let first = definition
        .compile_with_runtime(
            ROOT,
            checkpointer.clone(),
            None,
            &PipelineNodeRuntimes::new(None, Some(Arc::clone(&resolver)), None),
        )
        .expect("first graph");
    let first_events = run_direct_graph(first, sessions.clone(), THREAD, "lookup").await;
    assert_eq!(first_events.len(), 1);
    assert_eq!(capture.lock().expect("capture lock").calls, 0);
    let binding = pipeline_tool_event_binding(&first_events[0], ROOT, THREAD)
        .expect("checkpoint-bound Toolkit interrupt");
    assert_eq!(binding.tool_call_id(), "pipeline:lookup:0");
    let session = sessions
        .get(GetRequest {
            app_name: "elitea".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: THREAD.to_owned(),
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("persisted session");
    let wrong_call = PipelineContinuationDecision::from_payload(&tool_resume_payload(
        binding.interrupt_id(),
        "pipeline:lookup:wrong",
        "approve",
        "",
        THREAD,
    ))
    .expect("bounded wrong call")
    .resolve(session.as_ref(), checkpointer.as_ref(), ROOT, THREAD)
    .await;
    let Err(wrong_call) = wrong_call else {
        panic!("wrong call must be stale");
    };
    assert_eq!(wrong_call.code(), PipelineResumeErrorCode::StaleDecision);
    let resume = PipelineContinuationDecision::from_payload(&tool_resume_payload(
        binding.interrupt_id(),
        binding.tool_call_id(),
        "approve",
        "",
        THREAD,
    ))
    .expect("approved Toolkit decision")
    .resolve(session.as_ref(), checkpointer.as_ref(), ROOT, THREAD)
    .await
    .expect("checkpoint-bound Toolkit resume")
    .into_parts()
    .0;
    let resumed = definition
        .compile_with_runtime(
            ROOT,
            checkpointer.clone(),
            Some(resume),
            &PipelineNodeRuntimes::new(None, Some(resolver), None),
        )
        .expect("resumed graph");
    let events = run_direct_graph(resumed, sessions.clone(), THREAD, "continue").await;
    assert_eq!(capture.lock().expect("capture lock").calls, 1);
    assert_eq!(events.len(), 1);
    assert!(
        !events[0]
            .provider_metadata
            .contains_key(adk_rust::graph::interrupt::INTERRUPT_METADATA_KEY)
    );
    let completed = sessions
        .get(GetRequest {
            app_name: "elitea".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: THREAD.to_owned(),
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("completed session");
    let replay = PipelineContinuationDecision::from_payload(&tool_resume_payload(
        binding.interrupt_id(),
        binding.tool_call_id(),
        "approve",
        "",
        THREAD,
    ))
    .expect("same decision")
    .resolve(completed.as_ref(), checkpointer.as_ref(), ROOT, THREAD)
    .await;
    let Err(replay) = replay else {
        panic!("completed call cannot replay");
    };
    assert_eq!(replay.code(), PipelineResumeErrorCode::StaleDecision);
}

#[tokio::test]
async fn mcp_graph_authorization_resumes_latest_exact_server_checkpoint() {
    const ROOT: &str = "pipeline-root";
    const THREAD: &str = "mcp-authorization-thread";
    const SERVER: &str = "https://mcp.example.invalid/v1/mcp";
    let definition = PipelineDefinition::from_yaml(
        &sensitive_pipeline_definition_yaml().replace("type: toolkit", "type: mcp"),
    )
    .expect("MCP pipeline");
    let authorized = Arc::new(AtomicBool::new(false));
    let resolver: Arc<dyn PipelineDirectToolResolver> = Arc::new(FixtureResolver {
        alias: "Customer Support".to_owned(),
        tool: Arc::new(AuthorizationFixtureTool {
            authorized: Arc::clone(&authorized),
            toolkit_type: "mcp",
        }),
        sensitive: None,
    });
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let sessions = sensitive_pipeline_session(THREAD).await;
    let graph = definition
        .compile_with_runtime(
            ROOT,
            checkpointer.clone(),
            None,
            &PipelineNodeRuntimes::new(None, Some(Arc::clone(&resolver)), None),
        )
        .expect("MCP graph");
    let events = run_direct_graph(graph, sessions.clone(), THREAD, "lookup").await;
    assert_eq!(events.len(), 1);
    let binding = pipeline_mcp_auth_event_binding(&events[0], ROOT, THREAD)
        .expect("checkpoint-bound MCP authorization");
    assert_eq!(binding.server_url(), SERVER);
    let session = sessions
        .get(GetRequest {
            app_name: "elitea".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: THREAD.to_owned(),
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("persisted MCP session");
    let wrong = PipelineMcpAuthorizationContinuation::from_payload(&mcp_resume_payload(
        "https://other.example.invalid/v1/mcp",
        "authorize",
        THREAD,
    ))
    .expect("bounded wrong-server continuation")
    .resolve(session.as_ref(), checkpointer.as_ref(), ROOT, THREAD)
    .await;
    let Err(wrong) = wrong else {
        panic!("wrong MCP server must not resume the checkpoint");
    };
    assert_eq!(wrong.code(), PipelineResumeErrorCode::StaleDecision);

    let resume = PipelineMcpAuthorizationContinuation::from_payload(&mcp_resume_payload(
        SERVER,
        "authorize",
        THREAD,
    ))
    .expect("MCP authorize continuation")
    .resolve(session.as_ref(), checkpointer.as_ref(), ROOT, THREAD)
    .await
    .expect("exact MCP checkpoint resume");
    authorized.store(true, Ordering::Release);
    let resumed = definition
        .compile_with_runtime(
            ROOT,
            checkpointer.clone(),
            Some(resume),
            &PipelineNodeRuntimes::new(None, Some(resolver), None),
        )
        .expect("resumed MCP graph");
    let events = run_direct_graph(resumed, sessions, THREAD, "continue").await;
    assert_eq!(events.len(), 1);
    assert!(
        !events[0]
            .provider_metadata
            .contains_key(adk_rust::graph::interrupt::INTERRUPT_METADATA_KEY)
    );
}

#[tokio::test]
async fn direct_delegated_authorization_skip_reaches_end_with_sdk_terminal_state() {
    const ROOT: &str = "pipeline-root";
    for (node_type, toolkit_type, thread) in [
        ("mcp", "mcp", "mcp-authorization-skip-thread"),
        (
            "toolkit",
            "sharepoint",
            "sharepoint-authorization-skip-thread",
        ),
        ("toolkit", "openapi", "openapi-authorization-skip-thread"),
    ] {
        let definition = PipelineDefinition::from_yaml(
            &sensitive_pipeline_definition_yaml()
                .replace("type: toolkit", &format!("type: {node_type}")),
        )
        .expect("direct delegated-auth pipeline");
        let authorized = Arc::new(AtomicBool::new(false));
        let resolver: Arc<dyn PipelineDirectToolResolver> = Arc::new(FixtureResolver {
            alias: "Customer Support".to_owned(),
            tool: Arc::new(AuthorizationFixtureTool {
                authorized,
                toolkit_type,
            }),
            sensitive: None,
        });
        let checkpointer = Arc::new(MemoryCheckpointer::new());
        let sessions = sensitive_pipeline_session(thread).await;
        let graph = definition
            .compile_with_runtime(
                ROOT,
                checkpointer.clone(),
                None,
                &PipelineNodeRuntimes::new(None, Some(Arc::clone(&resolver)), None),
            )
            .expect("delegated-auth graph");
        let events = run_direct_graph(graph, sessions.clone(), thread, "lookup").await;
        let binding = pipeline_mcp_auth_event_binding(&events[0], ROOT, thread)
            .expect("checkpoint-bound delegated authorization");
        let session = sessions
            .get(GetRequest {
                app_name: "elitea".to_owned(),
                user_id: "user-1".to_owned(),
                session_id: thread.to_owned(),
                num_recent_events: None,
                after: None,
            })
            .await
            .expect("persisted delegated-auth session");
        let resume = PipelineMcpAuthorizationContinuation::from_payload(&mcp_resume_payload(
            binding.server_url(),
            "skip",
            thread,
        ))
        .expect("skip continuation")
        .resolve(session.as_ref(), checkpointer.as_ref(), ROOT, thread)
        .await
        .expect("checkpoint-bound skip");
        let resumed = definition
            .compile_with_runtime(
                ROOT,
                checkpointer.clone(),
                Some(resume),
                &PipelineNodeRuntimes::new(None, Some(resolver), None),
            )
            .expect("resumed delegated-auth graph");
        let completed = run_direct_graph(resumed, sessions, thread, "continue").await;
        assert_eq!(completed.len(), 1);
        assert!(
            !completed[0]
                .provider_metadata
                .contains_key(adk_rust::graph::interrupt::INTERRUPT_METADATA_KEY)
        );
        let checkpoint = checkpointer
            .load(thread)
            .await
            .expect("load terminal checkpoint")
            .expect("terminal checkpoint");
        assert!(checkpoint.pending_nodes.is_empty());
        assert_eq!(checkpoint.state.get("report"), Some(&Value::Null));
        assert!(
            checkpoint
                .state
                .get("_pipeline_blocked")
                .and_then(Value::as_str)
                .is_some_and(|message| message.contains("was skipped"))
        );
    }
}

fn sensitive_pipeline_definition() -> PipelineDefinition {
    PipelineDefinition::from_yaml(&sensitive_pipeline_definition_yaml())
        .expect("sensitive Toolkit pipeline")
}

fn sensitive_pipeline_definition_yaml() -> String {
    r#"
state:
  ticket_id: {type: int, value: 9}
  filters: {type: dict, value: {active: true}}
  report: dict
  messages: list
entry_point: lookup
nodes:
  - id: lookup
    type: toolkit
    toolkit_name: Customer Support
    tool: search_records
    input_mapping:
      query: {type: fstring, value: "ticket {ticket_id}"}
      filters: {type: variable, value: filters}
    input: [ticket_id, filters]
    output: [report, messages]
    structured_output: true
    transition: END
"#
    .to_owned()
}

async fn sensitive_pipeline_session(thread: &str) -> Arc<InMemorySessionService> {
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "elitea".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: Some(thread.to_owned()),
            state: HashMap::new(),
        })
        .await
        .expect("pipeline session");
    sessions
}

async fn run_direct_graph(
    graph: adk_rust::graph::GraphAgent,
    sessions: Arc<InMemorySessionService>,
    thread: &str,
    input: &str,
) -> Vec<adk_rust::Event> {
    let session_service: Arc<dyn SessionService> = sessions;
    let runner = Runner::builder()
        .app_name("elitea")
        .agent(Arc::new(EliteaGraphAgent::new(graph)))
        .session_service(session_service)
        .build()
        .expect("pipeline runner");
    let mut running = runner
        .run(
            UserId::new("user-1").expect("fixture user"),
            SessionId::new(thread).expect("fixture session"),
            Content::new("user").with_text(input),
        )
        .await
        .expect("Toolkit pipeline invocation");
    let mut events = Vec::new();
    while let Some(event) = running.next().await {
        events.push(event.unwrap_or_else(|error| panic!("pipeline event: {error}")));
    }
    events
}

fn tool_resume_payload(
    interrupt_id: &str,
    tool_call_id: &str,
    action: &str,
    value: &str,
    thread: &str,
) -> AgentExecutionPayload {
    AgentExecutionPayload {
        llm: Map::new(),
        chat_history: Vec::new(),
        user_input: UserInput::Text("continue".to_owned()),
        thread_id: Some(thread.to_owned()),
        checkpoint_id: None,
        debug: false,
        tools: Vec::new(),
        application: Map::new(),
        internal_tools: Vec::new(),
        steps_limit: None,
        mcp_tokens: Map::new(),
        ignored_mcp_servers: Vec::new(),
        user_declined_mcp_servers: Vec::new(),
        should_continue: true,
        hitl_resume: true,
        hitl_action: Some(action.to_owned()),
        hitl_value: Some(value.to_owned()),
        hitl_decisions: vec![json!({
            "interrupt_id": interrupt_id,
            "tool_call_id": tool_call_id,
            "action": action,
            "value": value,
        })],
        execution_generation: Some("generation-1".to_owned()),
        is_regenerate: false,
        meta: Map::new(),
        conversation_id: Some("conversation-1".to_owned()),
        persona: "generic".to_owned(),
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

fn mcp_resume_payload(server_url: &str, action: &str, thread: &str) -> AgentExecutionPayload {
    let mut payload = tool_resume_payload("unused", "unused", "approve", "", thread);
    payload.hitl_resume = false;
    payload.hitl_action = None;
    payload.hitl_value = None;
    payload.hitl_decisions.clear();
    if action == "authorize" {
        payload.mcp_tokens.insert(
            server_url.to_owned(),
            json!({"access_token": "runtime-secret"}),
        );
    } else {
        payload
            .user_declined_mcp_servers
            .push(json!({"server_url": server_url}));
    }
    payload
}
