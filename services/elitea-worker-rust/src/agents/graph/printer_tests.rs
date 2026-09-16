use std::collections::HashMap;
use std::sync::Arc;

use adk_rust::graph::interrupt::INTERRUPT_METADATA_KEY;
use adk_rust::graph::{Checkpointer, ExecutionConfig, MemoryCheckpointer, Node, NodeContext};
use adk_rust::runner::Runner;
use adk_rust::session::{CreateRequest, GetRequest, InMemorySessionService, SessionService};
use adk_rust::{Content, Event, Part, SessionId, UserId};
use chrono::Utc;
use serde_json::{Map, Value, json};

use super::agent::EliteaGraphAgent;
use super::compiler::PipelineDefinition;
use super::printer::{
    DEFAULT_FINAL_MESSAGE, PRINTER_COMPLETED_STATE, PRINTER_OUTPUT_STATE_KEY,
    PRINTER_PAUSE_METADATA_KEY, PrinterNode, PrinterNodeDefinition,
};
use super::resume::{
    PipelineResume, PipelineResumeErrorCode, PrinterContinuation, PrinterResumeContext,
};
use crate::agents::events::{
    AgentEventProjectionContext, AgentEventProjector, CompletedAgentBrowserOutput,
    pipeline_printer_event_binding,
};
use crate::agents::request::{AgentExecutionPayload, NextInputSuggestionPolicy, UserInput};
use crate::agents::runtime::NativeAgentInvocation;

const APP: &str = "elitea";
const ROOT: &str = "root-agent";
const USER: &str = "user-1";
const THREAD: &str = "thread-1";

const PIPELINE: &str = r#"
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
    final_message: " Continue when ready. "
    transition: capture
  - id: capture
    type: state_modifier
    template: "{{ input }}"
    input: [input]
    output: [answer]
    transition: END
"#;

#[test]
fn printer_yaml_is_strict_bounded_and_uses_the_sdk_default_message() {
    let default = PrinterNodeDefinition::from_yaml("id: show\ntype: printer\ntransition: END\n")
        .expect("default Printer definition");
    assert_eq!(default.id(), "show");
    assert_eq!(default.final_message(), DEFAULT_FINAL_MESSAGE);
    assert_eq!(default.transition(), "END");

    let trimmed = PrinterNodeDefinition::from_yaml(
        "id: show\ntype: printer\ninput_mapping:\n  printer: {type: fixed, value: ready}\nfinal_message: '  Continue.  '\ntransition: next\n",
    )
    .expect("trimmed final message");
    assert_eq!(trimmed.final_message(), "Continue.");
    assert_ne!(default.config_digest(), trimmed.config_digest());

    for invalid in [
        "id: show\ntype: printer\n",
        "id: bad/id\ntype: printer\ntransition: END\n",
        "id: show\ntype: printer\ninput_mapping:\n  other: {type: fixed, value: x}\ntransition: END\n",
        "id: show\ntype: printer\ninput_mapping:\n  printer: {type: expression, value: x}\ntransition: END\n",
        "id: show\ntype: printer\ninput_mapping:\n  printer: {type: fixed, value: x, source: request}\ntransition: END\n",
        "id: show\ntype: printer\nunknown: true\ntransition: END\n",
    ] {
        assert!(
            PrinterNodeDefinition::from_yaml(invalid).is_err(),
            "{invalid}"
        );
    }
}

#[tokio::test]
async fn printer_projects_fixed_variable_list_and_fstring_values() {
    for (mapping, state, expected) in [
        (
            "{type: fixed, value: ready}",
            HashMap::new(),
            "ready".to_owned(),
        ),
        (
            "{type: variable, value: values}",
            HashMap::from([("values".to_owned(), json!(["a", 2, true, null]))]),
            "a, 2, True, None".to_owned(),
        ),
        (
            "{type: fstring, value: 'Ticket {ticket}: {status}'}",
            HashMap::from([
                ("ticket".to_owned(), json!(7)),
                ("status".to_owned(), json!("ready")),
            ]),
            "Ticket 7: ready".to_owned(),
        ),
    ] {
        let definition = PrinterNodeDefinition::from_yaml(&format!(
            "id: show\ntype: printer\ninput_mapping:\n  printer: {mapping}\nfinal_message: Continue.\ntransition: END\n"
        ))
        .expect("Printer mapping");
        let output = PrinterNode::new(definition)
            .execute(&NodeContext::new(
                state,
                ExecutionConfig::new("printer-node"),
                0,
            ))
            .await
            .expect("Printer output");
        assert_eq!(
            output.updates.get(PRINTER_OUTPUT_STATE_KEY),
            Some(&json!(format!("{expected}\n\n-----\n*Continue.*")))
        );
    }
}

#[tokio::test]
async fn printer_output_bound_includes_the_public_suffix_and_event_envelope() {
    let suffix = "\n\n-----\n*Continue.*";
    let admitted = "x".repeat(8 * 1024 - suffix.len());
    let definition = PrinterNodeDefinition::from_yaml(&format!(
        "id: show\ntype: printer\ninput_mapping:\n  printer: {{type: fixed, value: {admitted}}}\nfinal_message: Continue.\ntransition: END\n"
    ))
    .expect("bounded Printer definition");
    PrinterNode::new(definition)
        .execute(&NodeContext::new(
            HashMap::new(),
            ExecutionConfig::new("printer-bound"),
            0,
        ))
        .await
        .expect("exact Printer output bound");

    let exhausted = format!("{admitted}x");
    let definition = PrinterNodeDefinition::from_yaml(&format!(
        "id: show\ntype: printer\ninput_mapping:\n  printer: {{type: fixed, value: {exhausted}}}\nfinal_message: Continue.\ntransition: END\n"
    ))
    .expect("mapping remains within its own bound");
    assert!(
        PrinterNode::new(definition)
            .execute(&NodeContext::new(
                HashMap::new(),
                ExecutionConfig::new("printer-bound"),
                0,
            ))
            .await
            .is_err()
    );
}

#[tokio::test]
async fn native_static_printer_pause_resumes_once_at_reset_with_the_user_message() {
    let definition = PipelineDefinition::from_yaml(PIPELINE).expect("Printer pipeline");
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let sessions = Arc::new(InMemorySessionService::new());
    create_session(sessions.as_ref()).await;

    let first = run_pipeline(
        &definition,
        checkpointer.clone(),
        sessions.clone(),
        None,
        "start",
    )
    .await;
    let [interrupt] = first.as_slice() else {
        panic!("Printer run emitted an unexpected event count");
    };
    assert!(
        interrupt
            .provider_metadata
            .contains_key(INTERRUPT_METADATA_KEY)
    );
    assert!(
        interrupt
            .provider_metadata
            .contains_key(PRINTER_PAUSE_METADATA_KEY)
    );
    let binding = pipeline_printer_event_binding(interrupt, ROOT, THREAD)
        .expect("bound Printer checkpoint event");
    assert_eq!(binding.node_name(), "show");
    assert_eq!(binding.reset_node_name(), "show_reset");
    assert_eq!(
        binding.output(),
        "Draft ready\n\n-----\n*Continue when ready.*"
    );

    let session = get_session(sessions.as_ref()).await;
    let resume = PrinterContinuation::from_payload(&printer_resume_payload("continue now"))
        .expect("Printer continuation envelope")
        .resolve(
            PrinterResumeContext::new(
                session.as_ref(),
                checkpointer.as_ref(),
                ROOT,
                THREAD,
                &definition.printer_pause_catalog(),
            ),
            "continue now",
        )
        .await
        .expect("checkpoint-bound Printer continuation");
    let completed = run_pipeline(
        &definition,
        checkpointer.clone(),
        sessions.clone(),
        Some(resume),
        "continue now",
    )
    .await;
    let [completed] = completed.as_slice() else {
        panic!("resumed Printer emitted an unexpected event count");
    };
    assert!(
        !completed
            .provider_metadata
            .contains_key(INTERRUPT_METADATA_KEY)
    );
    assert!(matches!(
        completed.content().map(|content| content.parts.as_slice()),
        Some([Part::Text { text }]) if text == "continue now"
    ));

    let checkpoint = checkpointer
        .load(THREAD)
        .await
        .expect("checkpoint read")
        .expect("completed checkpoint");
    assert_eq!(
        checkpoint.state.get(PRINTER_OUTPUT_STATE_KEY),
        Some(&json!(PRINTER_COMPLETED_STATE))
    );
    assert_eq!(
        checkpoint.state.get("messages"),
        Some(&json!([
            {"role": "user", "content": "start"},
            {"role": "user", "content": "continue now"}
        ]))
    );

    assert_printer_replay_rejected(&definition, checkpointer.as_ref(), sessions.as_ref()).await;
}

async fn assert_printer_replay_rejected(
    definition: &PipelineDefinition,
    checkpointer: &MemoryCheckpointer,
    sessions: &InMemorySessionService,
) {
    let completed_session = get_session(sessions).await;
    let catalog = definition.printer_pause_catalog();
    let replay = PrinterContinuation::from_payload(&printer_resume_payload("again"))
        .expect("same continuation shape")
        .resolve(
            PrinterResumeContext::new(
                completed_session.as_ref(),
                checkpointer,
                ROOT,
                THREAD,
                &catalog,
            ),
            "again",
        )
        .await;
    let Err(replay) = replay else {
        panic!("completed Printer resumed twice");
    };
    assert_eq!(replay.code(), PipelineResumeErrorCode::StaleDecision);
}

#[tokio::test]
async fn printer_projects_as_a_nonterminal_chat_result_not_a_hitl_card() {
    let definition = PipelineDefinition::from_yaml(PIPELINE).expect("Printer pipeline");
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let sessions = Arc::new(InMemorySessionService::new());
    create_session(sessions.as_ref()).await;
    let events = run_pipeline(&definition, checkpointer, sessions, None, "start").await;
    let [interrupt] = events.as_slice() else {
        panic!("Printer run emitted an unexpected event count");
    };
    let mut projector =
        AgentEventProjector::new(AgentEventProjectionContext::pipeline_fixture(json!({})))
            .expect("pipeline projector");
    let _start = projector.start(Utc::now()).expect("projected start");
    assert_eq!(
        projector
            .project(interrupt)
            .expect("projected Printer pause")
            .into_iter()
            .count(),
        0
    );
    assert!(
        !projector.is_paused(),
        "Printer is not a HITL decision card"
    );
    let projected = projector
        .finish_after_eos(CompletedAgentBrowserOutput::fixture("fallback"), Utc::now())
        .expect("Printer browser result")
        .into_iter()
        .collect::<Vec<_>>();
    assert_eq!(
        projected
            .iter()
            .map(|event| event.r#type.as_str())
            .collect::<Vec<_>>(),
        ["agent_response", "full_message"]
    );
    assert!(projected.iter().all(|event| {
        serde_json::from_slice::<Value>(&event.content).ok()
            == Some(json!("Draft ready\n\n-----\n*Continue when ready.*"))
    }));
}

#[tokio::test]
async fn continuation_rejects_a_changed_definition() {
    let definition = PipelineDefinition::from_yaml(PIPELINE).expect("Printer pipeline");
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let sessions = Arc::new(InMemorySessionService::new());
    create_session(sessions.as_ref()).await;
    let _events = run_pipeline(
        &definition,
        checkpointer.clone(),
        sessions.clone(),
        None,
        "start",
    )
    .await;
    let changed = PipelineDefinition::from_yaml(
        &PIPELINE.replace("Continue when ready.", "Continue after review."),
    )
    .expect("changed Printer pipeline");
    let session = get_session(sessions.as_ref()).await;
    let result = PrinterContinuation::from_payload(&printer_resume_payload("continue"))
        .expect("continuation envelope")
        .resolve(
            PrinterResumeContext::new(
                session.as_ref(),
                checkpointer.as_ref(),
                ROOT,
                THREAD,
                &changed.printer_pause_catalog(),
            ),
            "continue",
        )
        .await;
    let Err(result) = result else {
        panic!("changed definition resumed");
    };
    assert_eq!(result.code(), PipelineResumeErrorCode::StaleDecision);
}

async fn run_pipeline(
    definition: &PipelineDefinition,
    checkpointer: Arc<MemoryCheckpointer>,
    sessions: Arc<InMemorySessionService>,
    resume: Option<PipelineResume>,
    input: &str,
) -> Vec<Event> {
    let graph = definition
        .compile(ROOT, checkpointer.clone(), resume)
        .expect("compiled Printer graph");
    let agent = EliteaGraphAgent::new(graph)
        .with_printer_interrupts(checkpointer, definition.printer_pause_catalog());
    let session_service: Arc<dyn SessionService> = sessions;
    let runner = Runner::builder()
        .app_name(APP)
        .agent(Arc::new(agent))
        .session_service(session_service)
        .build()
        .expect("Printer runner");
    let invocation = NativeAgentInvocation::new(
        runner,
        UserId::new(USER).expect("fixture user"),
        SessionId::new(THREAD).expect("fixture session"),
        Content::new("user").with_text(input),
    );
    let mut running = invocation.start().expect("Printer invocation");
    let mut events = Vec::new();
    loop {
        match running.next_event().await {
            Ok(Some(event)) => events.push(event),
            Ok(None) => break,
            Err(error) => panic!("Printer event failed: {error}"),
        }
    }
    events
}

async fn create_session(sessions: &InMemorySessionService) {
    sessions
        .create(CreateRequest {
            app_name: APP.to_owned(),
            user_id: USER.to_owned(),
            session_id: Some(THREAD.to_owned()),
            state: HashMap::new(),
        })
        .await
        .expect("Printer session");
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
        .expect("persisted Printer session")
}

fn printer_resume_payload(input: &str) -> AgentExecutionPayload {
    AgentExecutionPayload {
        llm: Map::new(),
        chat_history: Vec::new(),
        user_input: UserInput::Text(input.to_owned()),
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
        hitl_resume: false,
        hitl_action: None,
        hitl_value: None,
        hitl_decisions: Vec::new(),
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
        project_context: None,
        model_context_limits: None,
    }
}
