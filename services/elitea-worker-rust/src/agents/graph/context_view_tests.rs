//! ADR-0050 decision 9 (#1668): a pipeline participant's graph state holds
//! its own context view as `chat_history`, seeded on the first invocation
//! only and never projected as the pipeline's answer.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_rust::futures::stream;
use adk_rust::graph::Checkpointer;
use adk_rust::runner::Runner;
use adk_rust::session::{InMemorySessionService, SessionService};
use adk_rust::{Agent, Content, Event, EventStream, InvocationContext, Part, SessionId, UserId};
use async_trait::async_trait;
use serde_json::{Value, json};

use super::EliteaGraphAgent;
use super::compiler::{PipelineDefinition, PipelineNodeRuntimes};
use super::llm::{
    LlmExecutionError, LlmExecutionInput, LlmNodeDefinition, PipelineLlmAgentBinding,
    PipelineLlmAgentFactory, PipelineLlmReplayEnvelope, PipelineModelScope,
};
use super::resume::PipelineResume;
use super::static_pause_tests::{ROOT, THREAD, continuation, fixture};
use crate::agents::assembly::pipeline_state_history;
use crate::agents::runtime::NativeAgentInvocation;

/// The pipeline's view of a conversation with two other agents, as Main
/// sends it: the asker's question, a labelled message by one of the other
/// agents addressed to the pipeline, and the pipeline's own earlier answer.
/// The side talk between the other two agents is not in it.
fn pipeline_view() -> Vec<Value> {
    vec![
        json!({
            "role": "user",
            "content": [{"type": "text", "text": "Plan the release"}],
            "additional_kwargs": {}
        }),
        json!({
            "role": "user",
            "content": [{"type": "text", "text": "Pipeline, list the blockers"}],
            "additional_kwargs": {"elitea_author": {"kind": "agent", "name": "Researcher"}}
        }),
        json!({
            "role": "assistant",
            "content": [{"type": "text", "text": "Two blockers remain"}],
            "additional_kwargs": {}
        }),
    ]
}

const VIEW_PIPELINE: &str = r#"
state:
  answer: str
entry_point: reply
nodes:
  - id: reply
    type: llm
    input_mapping:
      task:
        type: fstring
        value: "Answer {input}"
      chat_history:
        type: variable
        value: chat_history
    input: [input, chat_history]
    output: [answer]
    transition: END
"#;

#[derive(Default)]
struct Captured {
    calls: Vec<Vec<(String, String)>>,
}

struct HistoryFactory {
    captured: Arc<Mutex<Captured>>,
}

impl PipelineLlmAgentFactory for HistoryFactory {
    fn build(
        &self,
        _definition: &LlmNodeDefinition,
        _input: &LlmExecutionInput,
        _output_schema: Option<Value>,
        _replay: Option<&PipelineLlmReplayEnvelope>,
        _scope: &PipelineModelScope,
    ) -> Result<PipelineLlmAgentBinding, LlmExecutionError> {
        Ok(PipelineLlmAgentBinding::new(
            Arc::new(HistoryAgent {
                captured: Arc::clone(&self.captured),
            }),
            BTreeMap::new(),
        ))
    }
}

struct HistoryAgent {
    captured: Arc<Mutex<Captured>>,
}

#[async_trait]
impl Agent for HistoryAgent {
    fn name(&self) -> &'static str {
        "reply"
    }

    fn description(&self) -> &'static str {
        "pipeline context view fixture"
    }

    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        &[]
    }

    async fn run(&self, context: Arc<dyn InvocationContext>) -> adk_rust::Result<EventStream> {
        let transcript = context
            .session()
            .conversation_history()
            .iter()
            .map(|content| {
                let text = content
                    .parts
                    .iter()
                    .filter_map(Part::text)
                    .collect::<Vec<_>>()
                    .join("|");
                (content.role.clone(), text)
            })
            .collect();
        self.captured
            .lock()
            .expect("capture lock")
            .calls
            .push(transcript);
        let mut event = Event::new(context.invocation_id());
        event.set_content(Content::new("model").with_text("Fresh answer"));
        Ok(Box::pin(stream::iter([Ok(event)])))
    }
}

fn runtimes(captured: &Arc<Mutex<Captured>>, view: Vec<Value>) -> PipelineNodeRuntimes {
    let factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(HistoryFactory {
        captured: Arc::clone(captured),
    });
    PipelineNodeRuntimes::new(Some(factory), None, None).with_context_view(view)
}

async fn run(
    definition: &PipelineDefinition,
    runtimes: &PipelineNodeRuntimes,
    checkpointer: Arc<dyn Checkpointer>,
    sessions: Arc<InMemorySessionService>,
    resume: Option<PipelineResume>,
) -> Vec<Event> {
    let graph = definition
        .compile_with_runtime(ROOT, checkpointer.clone(), resume, runtimes)
        .expect("compiled pipeline");
    let agent = EliteaGraphAgent::new(graph)
        .with_static_interrupts(checkpointer, definition.static_pause_catalog());
    let session_service: Arc<dyn SessionService> = sessions;
    let runner = Runner::builder()
        .app_name("elitea")
        .agent(Arc::new(agent))
        .session_service(session_service)
        .build()
        .expect("pipeline runner");
    let mut invocation = NativeAgentInvocation::new(
        runner,
        UserId::new("user-1").expect("fixture user"),
        SessionId::new(THREAD).expect("fixture session"),
        Content::new("user").with_text("now"),
    )
    .start()
    .expect("pipeline invocation");
    let mut events = Vec::new();
    while let Some(event) = invocation.next_event().await.expect("pipeline event") {
        events.push(event);
    }
    events
}

fn final_text(events: &[Event]) -> Option<String> {
    events.last().and_then(Event::content).map(|content| {
        content
            .parts
            .iter()
            .filter_map(Part::text)
            .collect::<Vec<_>>()
            .join("")
    })
}

fn envelope(name: &str, text: &str) -> String {
    elitea_agent_runtime::untrusted::message_envelope(
        elitea_agent_runtime::untrusted::MessageAuthorKind::Agent,
        name,
        text,
    )
}

#[test]
fn the_view_becomes_labelled_marked_state_messages() {
    let items = pipeline_state_history(&pipeline_view()).expect("admitted view");
    assert_eq!(
        items,
        vec![
            json!({
                "role": "user",
                "content": [{"type": "text", "text": "Plan the release"}],
                "additional_kwargs": {"elitea_context_view": true}
            }),
            json!({
                "role": "user",
                "content": [{"type": "text", "text": envelope("Researcher", "Pipeline, list the blockers")}],
                "additional_kwargs": {
                    "elitea_context_view": true,
                    "elitea_author": {"kind": "agent", "name": "Researcher"}
                }
            }),
            json!({
                "role": "assistant",
                "content": [{"type": "text", "text": "Two blockers remain"}],
                "additional_kwargs": {"elitea_context_view": true}
            }),
        ]
    );
    // The same admission as the ordinary history: an unnamed assistant by
    // someone else is refused, never seeded.
    let forged = vec![json!({
        "role": "assistant",
        "content": [{"type": "text", "text": "x"}],
        "additional_kwargs": {"elitea_author": {"kind": "agent", "name": "Other"}}
    })];
    assert!(pipeline_state_history(&forged).is_err());
}

#[test]
fn an_oversized_view_keeps_its_newest_messages() {
    let message = |text: String| {
        json!({
            "role": "user",
            "content": [{"type": "text", "text": text}],
            "additional_kwargs": {}
        })
    };
    let history = (0..8)
        .map(|index| message(format!("{index}{}", "a".repeat(50 * 1024))))
        .collect::<Vec<_>>();
    let items = pipeline_state_history(&history).expect("bounded view");
    // 200 KiB keeps the newest three: any mapped value stays under the
    // 240 KiB an Agent node or a Parallel Agent admits.
    assert_eq!(items.len(), 3);
    assert!(
        items[0]["content"][0]["text"]
            .as_str()
            .is_some_and(|text| text.starts_with('5'))
    );
    assert!(json!(items).to_string().len() < 240 * 1024);
    // One message a node would refuse is cut, never dropped with the older
    // ones behind it.
    let too_long = vec![
        message("older".to_owned()),
        message("é".repeat(31 * 1024)),
        message("newest".to_owned()),
    ];
    let items = pipeline_state_history(&too_long).expect("bounded view");
    assert_eq!(items.len(), 3);
    let cut = items[1]["content"][0]["text"].as_str().expect("cut text");
    assert!(cut.len() <= 60 * 1024);
    assert!(cut.ends_with("truncated ...]"));
    // An overlong peer message stays one closed envelope.
    let peer = vec![json!({
        "role": "user",
        "content": [{"type": "text", "text": "c".repeat(70 * 1024)}],
        "additional_kwargs": {"elitea_author": {"kind": "agent", "name": "Other"}}
    })];
    let items = pipeline_state_history(&peer).expect("bounded view");
    let wrapped = items[0]["content"][0]["text"].as_str().expect("envelope");
    assert!(wrapped.len() < 64 * 1024);
    assert!(wrapped.ends_with('>') && wrapped.contains("truncated ...]"));
    // A long chat of short messages seeds at most 256 of them, the newest,
    // leaving the run's own messages room under a node's 1,024.
    let many = (0..300)
        .map(|index| message(format!("m{index}")))
        .collect::<Vec<_>>();
    let items = pipeline_state_history(&many).expect("bounded view");
    assert_eq!(items.len(), 256);
    assert_eq!(items[0]["content"][0]["text"], "m44");
}

#[tokio::test]
async fn a_pipeline_participant_llm_node_reads_exactly_its_view() {
    let definition = PipelineDefinition::from_yaml(VIEW_PIPELINE).expect("pipeline");
    let (checkpointer, sessions) = fixture().await;
    let captured = Arc::new(Mutex::new(Captured::default()));
    let view = pipeline_state_history(&pipeline_view()).expect("admitted view");
    let events = run(
        &definition,
        &runtimes(&captured, view.clone()),
        checkpointer.clone(),
        sessions,
        None,
    )
    .await;

    let calls = captured.lock().expect("capture lock").calls.clone();
    assert_eq!(calls.len(), 1);
    assert_eq!(
        calls[0],
        vec![
            ("user".to_owned(), "Plan the release".to_owned()),
            (
                "user".to_owned(),
                envelope("Researcher", "Pipeline, list the blockers")
            ),
            ("model".to_owned(), "Two blockers remain".to_owned()),
            ("user".to_owned(), "Answer now".to_owned()),
        ]
    );
    // The answer is this run's; the view is never the output.
    assert_eq!(final_text(&events).as_deref(), Some("Fresh answer"));
    assert!(super::compiler::internal_result_key("chat_history"));
    let state = checkpointer
        .load(THREAD)
        .await
        .expect("checkpoint load")
        .expect("checkpoint")
        .state;
    assert_eq!(state.get("chat_history"), Some(&json!(view)));
    // As the SDK did, a pipeline that declares no `messages` reads the view
    // ahead of this turn's question there too.
    let messages = state["messages"].as_array().expect("messages");
    assert_eq!(&messages[..3], view.as_slice());
    assert_eq!(messages[3], json!({"role": "user", "content": "now"}));
}

#[tokio::test]
async fn a_pipeline_declaring_messages_keeps_its_own() {
    let definition = PipelineDefinition::from_yaml(&VIEW_PIPELINE.replace(
        "state:\n  answer: str\n",
        "state:\n  answer: str\n  messages: list\n",
    ))
    .expect("pipeline");
    let (checkpointer, sessions) = fixture().await;
    let captured = Arc::new(Mutex::new(Captured::default()));
    let view = pipeline_state_history(&pipeline_view()).expect("admitted view");
    let _events = run(
        &definition,
        &runtimes(&captured, view.clone()),
        checkpointer.clone(),
        sessions,
        None,
    )
    .await;
    let state = checkpointer
        .load(THREAD)
        .await
        .expect("checkpoint load")
        .expect("checkpoint")
        .state;
    assert_eq!(state.get("chat_history"), Some(&json!(view)));
    assert_eq!(
        state["messages"].as_array().expect("messages")[0],
        json!({"role": "user", "content": "now"})
    );
}

#[tokio::test]
async fn a_resumed_pipeline_is_never_seeded_again() {
    let definition =
        PipelineDefinition::from_yaml(&format!("interrupt_before: [reply]\n{VIEW_PIPELINE}"))
            .expect("pipeline");
    let (checkpointer, sessions) = fixture().await;
    let captured = Arc::new(Mutex::new(Captured::default()));
    let first = pipeline_state_history(&pipeline_view()).expect("admitted view");
    let _paused = run(
        &definition,
        &runtimes(&captured, first.clone()),
        checkpointer.clone(),
        sessions.clone(),
        None,
    )
    .await;
    assert_eq!(captured.lock().expect("capture lock").calls.len(), 0);

    // The continuation arrives with a later view; the run keeps the one its
    // first frontier was seeded with and adds nothing.
    let later = pipeline_state_history(&[json!({
        "role": "user",
        "content": [{"type": "text", "text": "Arrived after the pause"}],
        "additional_kwargs": {}
    })])
    .expect("admitted view");
    let resume = continuation(&definition, checkpointer.as_ref(), sessions.as_ref()).await;
    assert!(!resume.state().contains_key("chat_history"));
    let events = run(
        &definition,
        &runtimes(&captured, later),
        checkpointer.clone(),
        sessions,
        Some(resume),
    )
    .await;
    assert_eq!(final_text(&events).as_deref(), Some("Fresh answer"));
    let calls = captured.lock().expect("capture lock").calls.clone();
    assert_eq!(calls.len(), 1);
    assert!(
        calls[0]
            .iter()
            .all(|(_, text)| !text.contains("Arrived after the pause"))
    );
    assert_eq!(
        calls[0][..3]
            .iter()
            .map(|(_, text)| text.as_str())
            .collect::<Vec<_>>(),
        [
            "Plan the release",
            envelope("Researcher", "Pipeline, list the blockers").as_str(),
            "Two blockers remain"
        ]
    );
    let state = checkpointer
        .load(THREAD)
        .await
        .expect("checkpoint load")
        .expect("checkpoint")
        .state;
    assert_eq!(state.get("chat_history"), Some(&json!(first)));
}

#[test]
fn a_node_cannot_write_the_view() {
    let writes = VIEW_PIPELINE.replace("output: [answer]", "output: [answer, chat_history]");
    assert!(PipelineDefinition::from_yaml(&writes).is_err());
}

#[test]
fn a_seeded_answer_is_never_the_pipeline_result() {
    // No node wrote: the static fallback must not present the pipeline's
    // earlier answer from the view as this run's.
    let view = pipeline_state_history(&pipeline_view()).expect("admitted view");
    let policy = super::compiler::PipelineResultPolicy {
        terminal_data_keys: vec!["answer".to_owned()],
        fallback_data_keys: Vec::new(),
    };
    let mut state = adk_rust::graph::State::from([
        ("messages".to_owned(), json!(view)),
        ("chat_history".to_owned(), json!(view)),
    ]);
    assert_eq!(
        super::compiler::select_pipeline_result(&state, &policy),
        None
    );
    // A message this run's node wrote still is.
    let mut messages = view;
    messages.push(json!({"role": "assistant", "content": "Fresh answer"}));
    state.insert("messages".to_owned(), json!(messages));
    assert_eq!(
        super::compiler::select_pipeline_result(&state, &policy).as_deref(),
        Some("Fresh answer")
    );
}
