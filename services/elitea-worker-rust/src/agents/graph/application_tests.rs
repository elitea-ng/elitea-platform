use std::collections::{BTreeMap, HashMap};
use std::sync::{Arc, Mutex};

use adk_rust::graph::interrupt::INTERRUPT_METADATA_KEY;
use adk_rust::graph::{
    Checkpointer, CompiledGraph, ExecutionConfig, GraphError, MemoryCheckpointer, Node,
    NodeContext, State,
};
use adk_rust::runner::Runner;
use adk_rust::session::{CreateRequest, GetRequest, InMemorySessionService, SessionService};
use adk_rust::{Content, Event, Part, SessionId, Tool, ToolContext, UserId};
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use super::EliteaGraphAgent;
use super::application::{
    ApplicationExecutionError, ApplicationNode, ApplicationNodeDefinition,
    PipelineApplicationResolver, PipelineApplicationSelection, ResolvedApplicationParticipant,
};
use super::compiler::{PipelineDefinition, PipelineNodeRuntimes};
use super::resume::{PipelineHitlDecision, PipelineResumeErrorCode};
use crate::agents::events::pipeline_hitl_event_binding;
use crate::agents::request::{AgentExecutionPayload, NextInputSuggestionPolicy, UserInput};
use crate::agents::runtime::NativeAgentInvocation;

#[derive(Default)]
struct InvocationCapture {
    arguments: Value,
    function_call_id: String,
    calls: usize,
}

struct FixtureApplicationTool {
    response: Value,
    capture: Arc<Mutex<InvocationCapture>>,
}

#[async_trait]
impl Tool for FixtureApplicationTool {
    fn name(&self) -> &'static str {
        "application_41_7"
    }

    fn description(&self) -> &'static str {
        "saved Application fixture"
    }

    async fn execute(
        &self,
        context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        let mut capture = self.capture.lock().expect("capture lock");
        capture.arguments = arguments;
        capture.function_call_id = context.function_call_id().to_owned();
        capture.calls += 1;
        Ok(self.response.clone())
    }
}

struct FixtureApplicationResolver {
    alias: String,
    participant: ResolvedApplicationParticipant,
}

impl PipelineApplicationResolver for FixtureApplicationResolver {
    fn resolve(
        &self,
        selection: &PipelineApplicationSelection,
        _checkpointer: Arc<dyn Checkpointer>,
    ) -> Result<ResolvedApplicationParticipant, ApplicationExecutionError> {
        if selection.alias() == self.alias {
            Ok(self.participant.clone())
        } else {
            Err(ApplicationExecutionError::Unavailable)
        }
    }
}

fn fixture_resolver(
    response: Value,
) -> (
    Arc<dyn PipelineApplicationResolver>,
    Arc<Mutex<InvocationCapture>>,
) {
    let capture = Arc::new(Mutex::new(InvocationCapture::default()));
    let tool: Arc<dyn Tool> = Arc::new(FixtureApplicationTool {
        response,
        capture: Arc::clone(&capture),
    });
    (
        Arc::new(FixtureApplicationResolver {
            alias: "Research Agent".to_owned(),
            participant: ResolvedApplicationParticipant::Agent(tool),
        }),
        capture,
    )
}

fn pipeline_resolver(graph: CompiledGraph) -> Arc<dyn PipelineApplicationResolver> {
    shared_pipeline_resolver(Arc::new(graph))
}

fn shared_pipeline_resolver(graph: Arc<CompiledGraph>) -> Arc<dyn PipelineApplicationResolver> {
    Arc::new(FixtureApplicationResolver {
        alias: "Research Agent".to_owned(),
        participant: ResolvedApplicationParticipant::Pipeline {
            graph,
            events: None,
            display_name: "Research Agent".to_owned(),
        },
    })
}

const AGENT_NODE: &str = r#"
id: delegate
type: agent
tool: Research Agent
input_mapping:
  task: {type: fstring, value: "Investigate {topic}"}
input: [topic]
output: [answer, messages]
transition: END
"#;

#[tokio::test]
async fn agent_node_maps_one_task_and_projects_child_response() {
    let definition = ApplicationNodeDefinition::from_yaml(AGENT_NODE).expect("Agent node");
    assert_eq!(definition.id(), "delegate");
    assert_eq!(definition.selection().alias(), "Research Agent");
    assert_eq!(definition.input_keys(), ["topic"]);
    assert_eq!(definition.output_keys(), ["answer", "messages"]);
    assert_eq!(definition.transition(), Some("END"));
    assert!(definition.config_digest().iter().any(|byte| *byte != 0));

    let (resolver, capture) = fixture_resolver(json!({"response":"child result"}));
    let node = ApplicationNode::new(
        definition,
        BTreeMap::from([
            ("answer".to_owned(), "str".to_owned()),
            ("messages".to_owned(), "list".to_owned()),
        ]),
        resolver.as_ref(),
        Arc::new(MemoryCheckpointer::new()),
    )
    .expect("resolved Agent node");
    let output = node
        .execute(&NodeContext::new(
            HashMap::from([("topic".to_owned(), json!("payment failure"))]),
            ExecutionConfig::new("agent-node-thread"),
            0,
        ))
        .await
        .expect("Agent node execution");
    assert_eq!(output.updates.get("answer"), Some(&json!("child result")));
    assert_eq!(
        output.updates.get("messages"),
        Some(&json!([{"role":"assistant","content":"child result"}]))
    );
    let capture = capture.lock().expect("capture lock");
    assert_eq!(capture.calls, 1);
    assert_eq!(
        capture.arguments,
        json!({"task":"Investigate payment failure"})
    );
    assert_eq!(capture.function_call_id, "pipeline:delegate:0");
}

#[test]
fn agent_node_rejects_ambiguous_or_undeclared_contracts() {
    for yaml in [
        AGENT_NODE.replace("type: agent", "type: application"),
        AGENT_NODE.replace("tool: Research Agent", "tool: ''"),
        AGENT_NODE.replace(
            "task: {type: fstring, value: \"Investigate {topic}\"}",
            "prompt: {type: fixed, value: hi}",
        ),
        AGENT_NODE.replace(
            "task: {type: fstring, value: \"Investigate {topic}\"}",
            "task: {type: expression, value: topic}",
        ),
        AGENT_NODE.replace("output: [answer, messages]", "output: [answer, answer]"),
        AGENT_NODE.replace("transition: END", "transition: 'bad/target'"),
        format!("{AGENT_NODE}\nvariables: {{}}"),
    ] {
        assert!(ApplicationNodeDefinition::from_yaml(&yaml).is_err());
    }

    let undeclared = PipelineDefinition::from_yaml(&format!(
        "state:\n  answer: str\nentry_point: delegate\nnodes:\n{}",
        indent_yaml(&AGENT_NODE.replace("input: [topic]", "input: [missing]"), 2)
    ));
    assert!(undeclared.is_err());
}

#[tokio::test]
async fn compiler_runs_saved_agent_through_the_common_runner() {
    let definition = PipelineDefinition::from_yaml(
        r#"
state:
  topic: {type: str, value: payment failure}
  answer: str
  messages: list
entry_point: delegate
nodes:
  - id: delegate
    type: agent
    tool: Research Agent
    input_mapping:
      task: {type: fstring, value: "Investigate {topic}"}
    input: [topic]
    output: [answer, messages]
    transition: END
"#,
    )
    .expect("pipeline with Agent node");
    assert!(definition.has_application_nodes());
    assert!(!definition.has_llm_nodes());
    assert_eq!(
        definition
            .application_selections()
            .map(PipelineApplicationSelection::alias)
            .collect::<Vec<_>>(),
        ["Research Agent"]
    );
    assert_eq!(
        definition.runtime_toolkit_aliases(),
        ["Research Agent".to_owned()].into_iter().collect()
    );
    assert!(
        definition
            .compile("pipeline-root", Arc::new(MemoryCheckpointer::new()), None,)
            .is_err()
    );

    let (resolver, capture) = fixture_resolver(json!({"response":"verified child answer"}));
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let graph = definition
        .compile_with_runtime(
            "pipeline-root",
            checkpointer.clone(),
            None,
            &PipelineNodeRuntimes::new(None, None, Some(resolver)),
        )
        .expect("compiled Agent graph");
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "elitea".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: Some("agent-pipeline-thread".to_owned()),
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
    let mut invocation = NativeAgentInvocation::new(
        runner,
        UserId::new("user-1").expect("fixture user"),
        SessionId::new("agent-pipeline-thread").expect("fixture session"),
        Content::new("user").with_text("delegate"),
    )
    .start()
    .expect("Agent pipeline invocation");
    let mut final_text = None;
    while let Some(event) = invocation.next_event().await.expect("pipeline event") {
        final_text = event.content().map(|content| {
            content
                .parts
                .iter()
                .filter_map(Part::text)
                .collect::<Vec<_>>()
                .join("")
        });
    }
    assert_eq!(final_text.as_deref(), Some("verified child answer"));
    assert_eq!(capture.lock().expect("capture lock").calls, 1);
    let checkpoint = checkpointer
        .load("agent-pipeline-thread")
        .await
        .expect("checkpoint load")
        .expect("Agent graph checkpoint");
    assert_eq!(
        checkpoint.state.get("answer"),
        Some(&json!("verified child answer"))
    );
}

#[tokio::test]
async fn compiler_runs_saved_pipeline_as_a_native_checkpointed_subgraph() {
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let child = PipelineDefinition::from_yaml(
        r#"
state:
  input: str
  messages: list
  answer: str
entry_point: answer
nodes:
  - id: answer
    type: state_modifier
    template: "Nested: {{ input }}"
    input: [input]
    output: [answer]
    transition: END
"#,
    )
    .expect("saved child pipeline")
    .compile_subgraph_with_runtime(checkpointer.clone(), &PipelineNodeRuntimes::default())
    .expect("compiled child subgraph");
    let parent = PipelineDefinition::from_yaml(
        r#"
state:
  topic: {type: str, value: payment failure}
  answer: str
  messages: list
entry_point: delegate
nodes:
  - id: delegate
    type: agent
    tool: Research Agent
    input_mapping:
      task: {type: fstring, value: "Investigate {topic}"}
    input: [topic]
    output: [answer, messages]
    transition: END
"#,
    )
    .expect("parent pipeline");
    let graph = parent
        .compile_with_runtime(
            "pipeline-root",
            checkpointer.clone(),
            None,
            &PipelineNodeRuntimes::new(None, None, Some(pipeline_resolver(child))),
        )
        .expect("parent with saved-pipeline participant");
    let mut input = State::new();
    input.insert("topic".to_owned(), json!("payment failure"));
    let state = graph
        .invoke(input, ExecutionConfig::new("parent-thread"))
        .await
        .expect("nested pipeline completion");
    assert_eq!(
        state.get("answer"),
        Some(&json!("Nested: Investigate payment failure"))
    );
    assert_eq!(
        state.get("messages"),
        Some(&json!([{"role":"assistant","content":"Nested: Investigate payment failure"}]))
    );
    assert!(
        checkpointer
            .load("parent-thread/delegate")
            .await
            .expect("child checkpoint load")
            .is_some(),
        "the child pipeline owns a namespaced checkpoint thread"
    );
}

#[tokio::test]
async fn nested_pipeline_interrupt_bubbles_with_exact_child_thread_identity() {
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let child = PipelineDefinition::from_yaml(
        r#"
state:
  input: str
  messages: list
entry_point: approve
nodes:
  - id: approve
    type: hitl
    input: [input]
    user_message:
      type: fstring
      value: "Approve {input}."
    routes:
      approve: END
      reject: END
"#,
    )
    .expect("interrupting child pipeline")
    .compile_subgraph_with_runtime(checkpointer.clone(), &PipelineNodeRuntimes::default())
    .expect("compiled interrupting child");
    let parent = PipelineDefinition::from_yaml(
        r"
state:
  messages: list
entry_point: delegate
nodes:
  - id: delegate
    type: agent
    tool: Research Agent
    input_mapping:
      task: {type: fixed, value: deployment}
    output: [messages]
    transition: END
",
    )
    .expect("parent pipeline");
    let graph = parent
        .compile_with_runtime(
            "pipeline-root",
            checkpointer.clone(),
            None,
            &PipelineNodeRuntimes::new(None, None, Some(pipeline_resolver(child))),
        )
        .expect("parent with interrupting child");
    let error = graph
        .invoke(State::new(), ExecutionConfig::new("nested-hitl"))
        .await
        .expect_err("child HITL must pause the parent");
    let GraphError::Interrupted(interrupted) = error else {
        panic!("nested HITL must use the ADK interrupted result");
    };
    let adk_rust::graph::interrupt::Interrupt::Dynamic { message, data } = interrupted.interrupt
    else {
        panic!("nested HITL must remain a dynamic interrupt");
    };
    assert_eq!(message, "delegate: Approve deployment.");
    assert_eq!(
        data.as_ref().and_then(|value| value["subgraph"].as_str()),
        Some("delegate")
    );
    assert_eq!(
        data.as_ref().and_then(|value| value["thread"].as_str()),
        Some("nested-hitl/delegate")
    );
    let child_checkpoint = checkpointer
        .load("nested-hitl/delegate")
        .await
        .expect("child checkpoint load")
        .expect("paused child checkpoint");
    assert_eq!(
        data.as_ref()
            .and_then(|value| value["checkpoint_id"].as_str()),
        Some(child_checkpoint.checkpoint_id.as_str())
    );
    assert_eq!(child_checkpoint.pending_nodes, ["approve"]);
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // The full pause/resume lineage is one behavioral proof.
async fn nested_pipeline_hitl_resumes_the_exact_child_checkpoint_once() {
    const ROOT: &str = "pipeline-root";
    const THREAD: &str = "nested-resume";
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let child = Arc::new(
        PipelineDefinition::from_yaml(
            r#"
state:
  input: str
  messages: list
entry_point: approve
nodes:
  - id: approve
    type: hitl
    input: [input]
    user_message:
      type: fstring
      value: "Approve {input}."
    routes:
      approve: END
      reject: END
"#,
        )
        .expect("interrupting child pipeline")
        .compile_subgraph_with_runtime(checkpointer.clone(), &PipelineNodeRuntimes::default())
        .expect("compiled interrupting child"),
    );
    let parent = PipelineDefinition::from_yaml(
        r"
state:
  messages: list
entry_point: delegate
nodes:
  - id: delegate
    type: agent
    tool: Research Agent
    input_mapping:
      task: {type: fixed, value: deployment}
    output: [messages]
    transition: END
",
    )
    .expect("parent pipeline");
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "elitea".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: Some(THREAD.to_owned()),
            state: HashMap::new(),
        })
        .await
        .expect("pipeline session");

    let first_graph = parent
        .compile_with_runtime(
            ROOT,
            checkpointer.clone(),
            None,
            &PipelineNodeRuntimes::new(
                None,
                None,
                Some(shared_pipeline_resolver(Arc::clone(&child))),
            ),
        )
        .expect("first parent graph");
    let first_events = run_pipeline(first_graph, Arc::clone(&sessions), THREAD, "start").await;
    assert_eq!(first_events.len(), 1);
    let binding = pipeline_hitl_event_binding(&first_events[0], ROOT, THREAD)
        .expect("nested public interrupt identity");
    assert_eq!(binding.node_name(), "approve");
    assert_eq!(binding.pending_node_name(), "delegate");
    assert_eq!(
        binding
            .nested_checkpoints()
            .first()
            .map(super::super::events::NestedPipelineCheckpoint::thread_id),
        Some("nested-resume/delegate")
    );
    let interrupt_id = binding.interrupt_id().to_owned();
    let session = get_pipeline_session(&sessions, THREAD).await;
    let resume = PipelineHitlDecision::from_payload(&resume_payload(THREAD, &interrupt_id))
        .expect("approved nested decision")
        .resolve(session.as_ref(), checkpointer.as_ref(), ROOT, THREAD)
        .await
        .expect("both parent and child checkpoints authorize resume");

    let resumed_graph = parent
        .compile_with_runtime(
            ROOT,
            checkpointer.clone(),
            Some(resume),
            &PipelineNodeRuntimes::new(
                None,
                None,
                Some(shared_pipeline_resolver(Arc::clone(&child))),
            ),
        )
        .expect("resumed parent graph");
    let resumed_events =
        run_pipeline(resumed_graph, Arc::clone(&sessions), THREAD, "continue").await;
    assert!(
        resumed_events
            .iter()
            .all(|event| !event.provider_metadata.contains_key(INTERRUPT_METADATA_KEY))
    );
    let child_checkpoint = checkpointer
        .load("nested-resume/delegate")
        .await
        .expect("child checkpoint reload")
        .expect("completed child checkpoint");
    assert!(child_checkpoint.pending_nodes.is_empty());

    let completed_session = get_pipeline_session(&sessions, THREAD).await;
    let replay = PipelineHitlDecision::from_payload(&resume_payload(THREAD, &interrupt_id))
        .expect("same decision envelope")
        .resolve(
            completed_session.as_ref(),
            checkpointer.as_ref(),
            ROOT,
            THREAD,
        )
        .await;
    let Err(replay) = replay else {
        panic!("completed nested interrupt was accepted twice");
    };
    assert_eq!(replay.code(), PipelineResumeErrorCode::StaleDecision);
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // The complete two-subgraph checkpoint chain is the proof.
async fn deeply_nested_pipeline_hitl_resumes_every_exact_child_checkpoint() {
    const ROOT: &str = "pipeline-root";
    const THREAD: &str = "deep-nested-resume";
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let leaf = Arc::new(
        PipelineDefinition::from_yaml(
            r#"
state:
  input: str
  messages: list
entry_point: approve
nodes:
  - id: approve
    type: hitl
    input: [input]
    user_message:
      type: fstring
      value: "Approve {input}."
    routes:
      approve: END
      reject: END
"#,
        )
        .expect("interrupting leaf pipeline")
        .compile_subgraph_with_runtime(checkpointer.clone(), &PipelineNodeRuntimes::default())
        .expect("compiled interrupting leaf"),
    );
    let middle = Arc::new(
        PipelineDefinition::from_yaml(
            r"
state:
  input: str
  messages: list
entry_point: specialist
nodes:
  - id: specialist
    type: agent
    tool: Research Agent
    input_mapping:
      task: {type: variable, value: input}
    input: [input]
    output: [messages]
    transition: END
",
        )
        .expect("middle pipeline")
        .compile_subgraph_with_runtime(
            checkpointer.clone(),
            &PipelineNodeRuntimes::new(
                None,
                None,
                Some(shared_pipeline_resolver(Arc::clone(&leaf))),
            ),
        )
        .expect("compiled middle pipeline"),
    );
    let parent = PipelineDefinition::from_yaml(
        r"
state:
  messages: list
entry_point: delegate
nodes:
  - id: delegate
    type: agent
    tool: Research Agent
    input_mapping:
      task: {type: fixed, value: deployment}
    output: [messages]
    transition: END
",
    )
    .expect("parent pipeline");
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "elitea".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: Some(THREAD.to_owned()),
            state: HashMap::new(),
        })
        .await
        .expect("pipeline session");

    let first_graph = parent
        .compile_with_runtime(
            ROOT,
            checkpointer.clone(),
            None,
            &PipelineNodeRuntimes::new(
                None,
                None,
                Some(shared_pipeline_resolver(Arc::clone(&middle))),
            ),
        )
        .expect("first deeply nested graph");
    let first_events = run_pipeline(first_graph, Arc::clone(&sessions), THREAD, "start").await;
    assert_eq!(first_events.len(), 1);
    let binding = pipeline_hitl_event_binding(&first_events[0], ROOT, THREAD)
        .expect("deep public interrupt identity");
    assert_eq!(binding.node_name(), "approve");
    assert_eq!(binding.pending_node_name(), "delegate");
    assert_eq!(binding.nested_checkpoints().len(), 2);
    assert_eq!(
        binding.nested_checkpoints()[0].thread_id(),
        "deep-nested-resume/delegate"
    );
    assert_eq!(
        binding.nested_checkpoints()[1].thread_id(),
        "deep-nested-resume/delegate/specialist"
    );
    let interrupt_id = binding.interrupt_id().to_owned();
    let session = get_pipeline_session(&sessions, THREAD).await;
    let resume = PipelineHitlDecision::from_payload(&resume_payload(THREAD, &interrupt_id))
        .expect("approved deep decision")
        .resolve(session.as_ref(), checkpointer.as_ref(), ROOT, THREAD)
        .await
        .expect("root and both child checkpoints authorize resume");

    let resumed_graph = parent
        .compile_with_runtime(
            ROOT,
            checkpointer.clone(),
            Some(resume),
            &PipelineNodeRuntimes::new(
                None,
                None,
                Some(shared_pipeline_resolver(Arc::clone(&middle))),
            ),
        )
        .expect("resumed deeply nested graph");
    let resumed_events =
        run_pipeline(resumed_graph, Arc::clone(&sessions), THREAD, "continue").await;
    assert!(
        resumed_events
            .iter()
            .all(|event| !event.provider_metadata.contains_key(INTERRUPT_METADATA_KEY))
    );
    for thread in [
        "deep-nested-resume/delegate",
        "deep-nested-resume/delegate/specialist",
    ] {
        let checkpoint = checkpointer
            .load(thread)
            .await
            .expect("child checkpoint reload")
            .expect("completed child checkpoint");
        assert!(checkpoint.pending_nodes.is_empty());
    }
}

#[tokio::test]
async fn agent_node_rejects_wrong_child_result_without_partial_state() {
    let definition = ApplicationNodeDefinition::from_yaml(AGENT_NODE).expect("Agent node");
    for response in [
        json!("plain response"),
        json!({"response": null}),
        json!({"response":"answer","extra":true}),
    ] {
        let (resolver, _) = fixture_resolver(response);
        let result = ApplicationNode::new(
            definition.clone(),
            BTreeMap::new(),
            resolver.as_ref(),
            Arc::new(MemoryCheckpointer::new()),
        )
        .expect("resolved Agent node")
        .execute(&NodeContext::new(
            HashMap::from([("topic".to_owned(), json!("case"))]),
            ExecutionConfig::new("bad-agent-result"),
            0,
        ))
        .await;
        assert!(result.is_err());
    }
}

async fn run_pipeline(
    graph: adk_rust::graph::GraphAgent,
    sessions: Arc<InMemorySessionService>,
    thread_id: &str,
    input: &str,
) -> Vec<Event> {
    let session_service: Arc<dyn SessionService> = sessions;
    let runner = Runner::builder()
        .app_name("elitea")
        .agent(Arc::new(EliteaGraphAgent::new(graph)))
        .session_service(session_service)
        .build()
        .expect("pipeline runner");
    let mut invocation = NativeAgentInvocation::new(
        runner,
        UserId::new("user-1").expect("fixture user"),
        SessionId::new(thread_id).expect("fixture session"),
        Content::new("user").with_text(input),
    )
    .start()
    .expect("pipeline invocation");
    let mut events = Vec::new();
    while let Some(event) = invocation.next_event().await.expect("pipeline event") {
        events.push(event);
    }
    events
}

async fn get_pipeline_session(
    sessions: &InMemorySessionService,
    thread_id: &str,
) -> Box<dyn adk_rust::session::Session> {
    sessions
        .get(GetRequest {
            app_name: "elitea".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: thread_id.to_owned(),
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("persisted pipeline session")
}

fn resume_payload(thread_id: &str, interrupt_id: &str) -> AgentExecutionPayload {
    AgentExecutionPayload {
        llm: Map::new(),
        chat_history: Vec::new(),
        user_input: UserInput::Text("continue".to_owned()),
        thread_id: Some(thread_id.to_owned()),
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
        hitl_action: Some("approve".to_owned()),
        hitl_value: Some(String::new()),
        hitl_decisions: vec![json!({
            "interrupt_id": interrupt_id,
            "action": "approve",
            "value": "",
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

fn indent_yaml(value: &str, count: usize) -> String {
    let prefix = " ".repeat(count);
    value
        .lines()
        .map(|line| format!("{prefix}{line}"))
        .collect::<Vec<_>>()
        .join("\n")
}
