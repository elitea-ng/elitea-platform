use std::collections::HashMap;
use std::sync::Arc;

use adk_rust::graph::interrupt::INTERRUPT_METADATA_KEY;
use adk_rust::graph::{Checkpointer, MemoryCheckpointer};
use adk_rust::runner::Runner;
use adk_rust::session::{CreateRequest, GetRequest, InMemorySessionService, SessionService};
use adk_rust::{Content, Event, Part, SessionId, UserId};
use serde_json::{Map, json};

use super::agent::EliteaGraphAgent;
use super::compiler::{
    PipelineDefinition, PipelineResultPolicy, append_or_clear_list, merge_or_clear_object,
    select_pipeline_result,
};
use super::resume::{PipelineHitlDecision, PipelineResumeErrorCode};
use crate::agents::events::pipeline_hitl_event_binding;
use crate::agents::request::{AgentExecutionPayload, NextInputSuggestionPolicy, UserInput};
use crate::agents::runtime::NativeAgentInvocation;

const APP: &str = "elitea";
const ROOT: &str = "pipeline-root";
const USER: &str = "user-1";
const THREAD: &str = "pipeline-thread-1";

const PIPELINE: &str = r"
state:
  answer: string
entry_point: review
nodes:
  - id: review
    type: hitl
    input: [messages]
    user_message:
      type: fixed
      value: Review the draft.
    routes:
      approve: publish
      reject: END
      edit: publish
    edit_state_key: answer
  - id: publish
    type: hitl
    user_message:
      type: fixed
      value: Publish the approved draft?
    routes:
      approve: END
      reject: END
";

#[test]
fn whole_pipeline_yaml_is_bounded_strict_and_digest_stable() {
    let definition = PipelineDefinition::from_yaml(PIPELINE).expect("pipeline definition");
    assert_eq!(definition.entry_point(), "review");
    assert_eq!(definition.node_count(), 2);
    assert!(definition.definition_digest().iter().any(|byte| *byte != 0));

    let equivalent = PIPELINE.replace("answer: string", "answer:\n    type: string");
    assert_eq!(
        PipelineDefinition::from_yaml(&equivalent)
            .expect("equivalent state syntax")
            .definition_digest(),
        definition.definition_digest()
    );

    let declaration_order_changes_fallback = PIPELINE.replace(
        "state:\n  answer: string",
        "state:\n  later: string\n  answer: string",
    );
    let reversed_declaration_order = PIPELINE.replace(
        "state:\n  answer: string",
        "state:\n  answer: string\n  later: string",
    );
    assert_ne!(
        PipelineDefinition::from_yaml(&declaration_order_changes_fallback)
            .expect("ordered state")
            .definition_digest(),
        PipelineDefinition::from_yaml(&reversed_declaration_order)
            .expect("reordered state")
            .definition_digest()
    );

    for (yaml, code) in [
        (
            "entry_point: missing\nnodes:\n  - id: review\n    type: hitl\n    routes:\n      approve: END\n",
            "graph.pipeline.invalid_configuration",
        ),
        (
            "entry_point: review\ninterrupt_before: [review]\nnodes:\n  - id: review\n    type: hitl\n    routes:\n      approve: END\n",
            "graph.pipeline.unsupported_capability",
        ),
        (
            "entry_point: review\nnodes:\n  - id: review\n    type: hitl\n    routes:\n      approve: absent\n",
            "graph.pipeline.invalid_configuration",
        ),
        (
            "state:\n  context_info: dict\nentry_point: review\nnodes:\n  - id: review\n    type: hitl\n    routes:\n      approve: END\n",
            "graph.pipeline.invalid_configuration",
        ),
        (
            "entry_point: review\nunknown: true\nnodes:\n  - id: review\n    type: hitl\n    routes:\n      approve: END\n",
            "graph.pipeline.malformed_yaml",
        ),
    ] {
        let Err(error) = PipelineDefinition::from_yaml(yaml) else {
            panic!("invalid pipeline was accepted");
        };
        assert_eq!(error.code(), code);
    }

    let oversized = "x".repeat(512 * 1024 + 1);
    let Err(error) = PipelineDefinition::from_yaml(&oversized) else {
        panic!("oversized pipeline was accepted");
    };
    assert_eq!(
        error.code(),
        "graph.pipeline.configuration_resource_exhausted"
    );
}

#[test]
fn legacy_ui_graph_identifiers_are_normalized_without_rewriting_storage() {
    let legacy = r"
entry_point: Agent 1
nodes:
  - id: Agent 1
    type: agent
    input: [input]
    input_mapping:
      task:
        type: variable
        value: input
    output: [messages]
    tool: Full Name Resolver
    transition: END
";
    let canonical = legacy.replace("Agent 1", "Agent1");
    let legacy = PipelineDefinition::from_yaml(legacy).expect("legacy UI pipeline");
    let canonical = PipelineDefinition::from_yaml(&canonical).expect("canonical pipeline");

    assert_eq!(legacy.entry_point(), "Agent1");
    assert_eq!(legacy.definition_digest(), canonical.definition_digest());

    let collision = r"
entry_point: Agent 1
nodes:
  - id: Agent 1
    type: state_modifier
    transition: END
  - id: Agent1
    type: state_modifier
    transition: END
";
    let Err(error) = PipelineDefinition::from_yaml(collision) else {
        panic!("normalized node identifier collision must be rejected");
    };
    assert_eq!(error.code(), "graph.pipeline.invalid_configuration");
}

#[tokio::test]
async fn active_state_modifier_yaml_runs_natively_and_surfaces_terminal_output() {
    let definition = PipelineDefinition::from_yaml(
        r#"
state:
  input:
    type: str
  messages:
    type: list
  prefix:
    type: str
    value: Hello
  final_text:
    type: str
    value: ""
entry_point: transform
nodes:
  - id: transform
    type: state_modifier
    template: "{{ prefix }}, {{ input }}"
    input: [prefix, input]
    output: [final_text]
    variables_to_clean: [prefix]
    transition: END
"#,
    )
    .expect("UI-compatible state modifier pipeline");
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: APP.to_owned(),
            user_id: USER.to_owned(),
            session_id: Some(THREAD.to_owned()),
            state: HashMap::new(),
        })
        .await
        .expect("pipeline session");
    let graph = definition
        .compile(ROOT, Arc::new(MemoryCheckpointer::new()), None)
        .expect("state modifier graph");
    let events = run_graph(graph, sessions, "world").await;
    let [event] = events.as_slice() else {
        panic!("state modifier graph emitted an unexpected event count");
    };
    let Some(content) = event.content() else {
        panic!("terminal state modifier result had no content");
    };
    assert!(matches!(
        content.parts.as_slice(),
        [Part::Text { text }] if text == "Hello, world"
    ));
}

#[tokio::test]
async fn messages_use_the_adk_append_reducer_and_surface_the_last_nonempty_ai_message() {
    let definition = PipelineDefinition::from_yaml(
        r#"
state:
  messages:
    type: list
entry_point: first
nodes:
  - id: first
    type: state_modifier
    template: '[{"role":"assistant","content":"first answer"}]'
    output: [messages]
    transition: second
  - id: second
    type: state_modifier
    template: '[{"role":"assistant","content":""}]'
    output: [messages]
    transition: END
"#,
    )
    .expect("message-producing pipeline");
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: APP.to_owned(),
            user_id: USER.to_owned(),
            session_id: Some(THREAD.to_owned()),
            state: HashMap::new(),
        })
        .await
        .expect("pipeline session");
    let graph = definition
        .compile(ROOT, Arc::new(MemoryCheckpointer::new()), None)
        .expect("message graph");

    let events = run_graph(graph, sessions, "question").await;
    let [event] = events.as_slice() else {
        panic!("message graph emitted an unexpected event count");
    };
    let Some(content) = event.content() else {
        panic!("message graph result had no content");
    };
    assert!(matches!(
        content.parts.as_slice(),
        [Part::Text { text }] if text == "first answer"
    ));
}

#[tokio::test]
async fn zero_llm_pipeline_falls_back_to_the_last_declared_public_state_value() {
    let definition = PipelineDefinition::from_yaml(
        r#"
state:
  z_first:
    type: str
    value: first
  a_last:
    type: str
    value: last
entry_point: finish
nodes:
  - id: finish
    type: state_modifier
    template: ""
    transition: END
"#,
    )
    .expect("zero-LLM data pipeline");
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: APP.to_owned(),
            user_id: USER.to_owned(),
            session_id: Some(THREAD.to_owned()),
            state: HashMap::new(),
        })
        .await
        .expect("pipeline session");
    let graph = definition
        .compile(ROOT, Arc::new(MemoryCheckpointer::new()), None)
        .expect("zero-LLM graph");

    let events = run_graph(graph, sessions, "ignored").await;
    let [event] = events.as_slice() else {
        panic!("zero-LLM graph emitted an unexpected event count");
    };
    let Some(content) = event.content() else {
        panic!("zero-LLM graph result had no content");
    };
    assert!(matches!(
        content.parts.as_slice(),
        [Part::Text { text }] if text == "last"
    ));
}

#[test]
fn result_policy_prefers_terminal_data_then_ai_messages_then_declared_state() {
    let policy = PipelineResultPolicy {
        terminal_data_keys: vec!["terminal_a".to_owned(), "terminal_b".to_owned()],
        fallback_data_keys: vec!["z_first".to_owned(), "a_last".to_owned()],
    };
    let mut state = HashMap::from([
        ("terminal_a".to_owned(), json!("first terminal")),
        ("terminal_b".to_owned(), json!("last terminal")),
        ("z_first".to_owned(), json!("first declared")),
        ("a_last".to_owned(), json!("last declared")),
        (
            "messages".to_owned(),
            json!([
                {"role": "user", "content": "do not surface me"},
                {"role": "assistant", "content": [{"type": "text", "text": "model answer"}]},
                {"role": "assistant", "content": [{"type": "thinking", "thinking": "private"}]}
            ]),
        ),
        ("context_info".to_owned(), json!({"private": true})),
    ]);

    assert_eq!(
        select_pipeline_result(&state, &policy).as_deref(),
        Some("last terminal")
    );
    state.insert("terminal_a".to_owned(), json!(""));
    state.insert("terminal_b".to_owned(), json!(""));
    assert_eq!(
        select_pipeline_result(&state, &policy).as_deref(),
        Some("model answer")
    );
    state.insert("messages".to_owned(), json!([]));
    assert_eq!(
        select_pipeline_result(&state, &policy).as_deref(),
        Some("last declared")
    );
}

#[test]
fn runtime_owned_custom_reducers_append_merge_and_clear() {
    assert_eq!(
        append_or_clear_list(json!([{"id": 1}]), json!([{"id": 2}])),
        json!([{"id": 1}, {"id": 2}])
    );
    assert_eq!(
        append_or_clear_list(json!([1]), serde_json::Value::Null),
        json!([])
    );
    assert_eq!(
        merge_or_clear_object(json!({"a": 1}), json!({"b": 2})),
        json!({"a": 1, "b": 2})
    );
    assert_eq!(
        merge_or_clear_object(json!({"a": 1}), serde_json::Value::Null),
        json!({})
    );
}

#[tokio::test]
async fn stored_pipeline_pauses_and_resumes_twice_through_runner_session_and_checkpoint() {
    let definition = PipelineDefinition::from_yaml(PIPELINE).expect("pipeline definition");
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: APP.to_owned(),
            user_id: USER.to_owned(),
            session_id: Some(THREAD.to_owned()),
            state: HashMap::new(),
        })
        .await
        .expect("pipeline session");

    let first = definition
        .compile(ROOT, checkpointer.clone(), None)
        .expect("first graph");
    let first_events = run_graph(first, sessions.clone(), "draft").await;
    assert_eq!(first_events.len(), 1);
    let first_binding = pipeline_hitl_event_binding(&first_events[0], ROOT, THREAD)
        .expect("first public interrupt identity");
    assert_eq!(first_binding.node_name(), "review");
    let first_interrupt_id = first_binding.interrupt_id().to_owned();

    let first_session = get_session(sessions.as_ref()).await;
    let first_resume =
        PipelineHitlDecision::from_payload(&resume_payload(&first_interrupt_id, "approve", ""))
            .expect("approved decision")
            .resolve(first_session.as_ref(), checkpointer.as_ref(), ROOT, THREAD)
            .await
            .expect("checkpoint-bound first resume");

    let second = definition
        .compile(ROOT, checkpointer.clone(), Some(first_resume))
        .expect("second graph");
    let second_events = run_graph(second, sessions.clone(), "continue").await;
    assert_eq!(second_events.len(), 1);
    let second_binding = pipeline_hitl_event_binding(&second_events[0], ROOT, THREAD)
        .expect("second public interrupt identity");
    assert_eq!(second_binding.node_name(), "publish");
    assert_ne!(second_binding.interrupt_id(), first_interrupt_id);
    let second_interrupt_id = second_binding.interrupt_id().to_owned();

    let second_session = get_session(sessions.as_ref()).await;
    let final_resume =
        PipelineHitlDecision::from_payload(&resume_payload(&second_interrupt_id, "approve", ""))
            .expect("final approved decision")
            .resolve(second_session.as_ref(), checkpointer.as_ref(), ROOT, THREAD)
            .await
            .expect("checkpoint-bound final resume");
    let final_graph = definition
        .compile(ROOT, checkpointer.clone(), Some(final_resume))
        .expect("final graph");
    let final_events = run_graph(final_graph, sessions.clone(), "continue").await;
    assert_eq!(final_events.len(), 1);
    assert!(
        !final_events[0]
            .provider_metadata
            .contains_key(INTERRUPT_METADATA_KEY)
    );

    let completed_session = get_session(sessions.as_ref()).await;
    let replay =
        PipelineHitlDecision::from_payload(&resume_payload(&second_interrupt_id, "approve", ""))
            .expect("same browser decision")
            .resolve(
                completed_session.as_ref(),
                checkpointer.as_ref(),
                ROOT,
                THREAD,
            )
            .await;
    let Err(replay) = replay else {
        panic!("completed interrupt was accepted twice");
    };
    assert_eq!(replay.code(), PipelineResumeErrorCode::StaleDecision);
}

#[tokio::test]
async fn stale_identity_is_rejected_and_block_with_comment_uses_the_reject_route() {
    let definition = PipelineDefinition::from_yaml(PIPELINE).expect("pipeline definition");
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: APP.to_owned(),
            user_id: USER.to_owned(),
            session_id: Some(THREAD.to_owned()),
            state: HashMap::new(),
        })
        .await
        .expect("pipeline session");
    let graph = definition
        .compile(ROOT, checkpointer.clone(), None)
        .expect("pipeline graph");
    let events = run_graph(graph, sessions.clone(), "draft").await;
    let binding = pipeline_hitl_event_binding(&events[0], ROOT, THREAD).expect("interrupt binding");
    let checkpoint_before = checkpointer
        .load(THREAD)
        .await
        .expect("checkpoint read")
        .expect("pause checkpoint");
    let session = get_session(sessions.as_ref()).await;

    let wrong_identity =
        PipelineHitlDecision::from_payload(&resume_payload("hitl_g1:wrong", "approve", ""))
            .expect("bounded wrong identity")
            .resolve(session.as_ref(), checkpointer.as_ref(), ROOT, THREAD)
            .await;
    let Err(wrong_identity) = wrong_identity else {
        panic!("wrong interrupt identity was accepted");
    };
    assert_eq!(
        wrong_identity.code(),
        PipelineResumeErrorCode::StaleDecision
    );

    let blocked = PipelineHitlDecision::from_payload(&resume_payload(
        binding.interrupt_id(),
        "block_with_comment",
        "stop here",
    ))
    .expect("bounded block decision")
    .resolve(session.as_ref(), checkpointer.as_ref(), ROOT, THREAD)
    .await
    .expect("block with comment is the authorized reject route");
    drop(blocked);

    let checkpoint_after = checkpointer
        .load(THREAD)
        .await
        .expect("checkpoint reread")
        .expect("same pause checkpoint");
    assert_eq!(
        checkpoint_after.checkpoint_id,
        checkpoint_before.checkpoint_id
    );
    assert_eq!(checkpoint_after.state, checkpoint_before.state);
}

async fn run_graph(
    graph: adk_rust::graph::GraphAgent,
    sessions: Arc<InMemorySessionService>,
    input: &str,
) -> Vec<Event> {
    let session_service: Arc<dyn SessionService> = sessions;
    let runner = Runner::builder()
        .app_name(APP)
        .agent(Arc::new(EliteaGraphAgent::new(graph)))
        .session_service(session_service)
        .build()
        .expect("pipeline runner");
    let invocation = NativeAgentInvocation::new(
        runner,
        UserId::new(USER).expect("fixture user"),
        SessionId::new(THREAD).expect("fixture session"),
        Content::new("user").with_text(input),
    );
    let mut running = invocation.start().expect("pipeline invocation");
    let mut events = Vec::new();
    loop {
        match running.next_event().await {
            Ok(Some(event)) => events.push(event),
            Ok(None) => break,
            Err(error) => panic!("pipeline event failed: {error}"),
        }
    }
    events
}

async fn get_session(sessions: &InMemorySessionService) -> Box<dyn adk_rust::session::Session> {
    sessions
        .get(GetRequest {
            app_name: APP.to_owned(),
            user_id: USER.to_owned(),
            session_id: THREAD.to_owned(),
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("persisted pipeline session")
}

fn resume_payload(interrupt_id: &str, action: &str, value: &str) -> AgentExecutionPayload {
    AgentExecutionPayload {
        llm: Map::new(),
        chat_history: Vec::new(),
        user_input: UserInput::Text("continue".to_owned()),
        thread_id: Some(THREAD.to_owned()),
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
