//! #973: a saved pipeline attached to an ordinary agent as a tool.
//!
//! These drive the TOOL directly rather than a whole assembly, because the
//! parts that are new are all inside it: what the model is offered, what one
//! call returns, what a `hitl` node's pause puts on the parent's event, what
//! the browser is allowed to see of it, and what a decision must prove before
//! the child graph is re-entered. The assembly-level half — that the child is
//! resolved and bound at all — is asserted in `ordinary_tests.rs`.

use std::collections::HashMap;
use std::sync::Arc;

use adk_rust::graph::interrupt::INTERRUPT_METADATA_KEY;
use adk_rust::{
    Content, Event, EventActions, MemoryEntry, Part, Tool, ToolContext,
    context::{Artifacts, CallbackContext, ReadonlyContext},
};
use serde_json::{Value, json};
use tokio::sync::mpsc;

use super::application_pipeline::{
    ApplicationPipelineTool, MAX_PIPELINE_TOOL_PENDING_BYTES, pipeline_pause_identity,
    pipeline_tool_description,
};
use super::application_tools::{
    ApplicationEventSignal, ApplicationResumeCoordinator, application_signal_event,
    install_nested_application_resume, nested_application_interrupt_ids,
};
use super::assembly_tests::ordinary_request;
use super::direct_hitl::DirectHitlDecisionSet;
use super::events::{
    AgentEventProjectionContext, AgentEventProjector, ApplicationToolGuardCatalogs,
    ApplicationToolPresentationCatalog, DESCENDANT_CHECKPOINT_THREAD_KEY,
    PIPELINE_TOOL_PENDING_METADATA_KEY,
};
use super::graph::compiler::{PipelineDefinition, PipelineNodeRuntimes};
use super::graph::pipeline_node_event_channel;
use super::request::{AgentExecutionKind, AgentExecutionPayload};
use super::sensitive_tools::SensitiveToolCatalog;

const TOOL_NAME: &str = "elitea_agent_31_v_41";
const CALL_ID: &str = "call-1";
const INVOCATION_ID: &str = "invocation-1";
/// `AgentEventProjectionContext::fixture` names this conversation thread, and
/// the child's checkpoint thread is derived from it.
const CONVERSATION_THREAD: &str = "thread-1";

/// One `hitl` node routing approve/reject to two distinguishable terminals.
/// No `llm` node, so the child needs no model: what is under test is the
/// pause, not what a pipeline does around it.
const REVIEW_PIPELINE: &str = r#"
state:
  input:
    type: str
  messages:
    type: list
  verdict:
    type: str
entry_point: review
nodes:
  - id: review
    type: hitl
    input:
      - input
    user_message:
      type: fstring
      value: "Review: {input}"
    routes:
      approve: approved
      reject: rejected
  - id: approved
    type: state_modifier
    template: "APPROVED {{ input }}"
    input: [input]
    output: [verdict]
    transition: END
  - id: rejected
    type: state_modifier
    template: "REJECTED {{ input }}"
    input: [input]
    output: [verdict]
    transition: END
"#;

/// Two `hitl` nodes in sequence: the second pause must be a fresh card bound
/// to its own checkpoint, not a replay of the first.
const TWO_GATE_PIPELINE: &str = r#"
state:
  input:
    type: str
  messages:
    type: list
  verdict:
    type: str
entry_point: first
nodes:
  - id: first
    type: hitl
    input:
      - input
    user_message:
      type: fstring
      value: "First: {input}"
    routes:
      approve: second
      reject: rejected
  - id: second
    type: hitl
    input:
      - input
    user_message:
      type: fstring
      value: "Second: {input}"
    routes:
      approve: approved
      reject: rejected
  - id: approved
    type: state_modifier
    template: "APPROVED {{ input }}"
    input: [input]
    output: [verdict]
    transition: END
  - id: rejected
    type: state_modifier
    template: "REJECTED {{ input }}"
    input: [input]
    output: [verdict]
    transition: END
"#;

/// A pipeline with no gate at all, for the plain-call case.
const PLAIN_PIPELINE: &str = r#"
state:
  input:
    type: str
  messages:
    type: list
  verdict:
    type: str
entry_point: decide
nodes:
  - id: decide
    type: state_modifier
    template: "HANDLED {{ input }}"
    input: [input]
    output: [verdict]
    transition: END
"#;

const SECOND_TOOL_NAME: &str = "elitea_agent_32_v_42";
const SECOND_CALL_ID: &str = "call-2";

struct FixtureToolContext {
    /// Every TURN is a fresh invocation, and that is load-bearing rather than
    /// decorative: the resume's completeness check scopes itself by the event
    /// carrying the parent's tool call, and it is the invocation id that tells
    /// this turn's call event apart from the identical replayed call of the
    /// turn before — which is what stops an already-answered pause being
    /// demanded again.
    invocation_id: &'static str,
    call_id: &'static str,
    user_content: Content,
    actions: std::sync::Mutex<EventActions>,
}

impl FixtureToolContext {
    fn new(invocation_id: &'static str) -> Self {
        Self::for_call(invocation_id, CALL_ID)
    }

    fn for_call(invocation_id: &'static str, call_id: &'static str) -> Self {
        Self {
            invocation_id,
            call_id,
            user_content: Content::new("user"),
            actions: std::sync::Mutex::new(EventActions::default()),
        }
    }
}

#[async_trait::async_trait]
impl ReadonlyContext for FixtureToolContext {
    fn invocation_id(&self) -> &str {
        self.invocation_id
    }

    fn agent_name(&self) -> &'static str {
        "root-agent"
    }

    fn user_id(&self) -> &'static str {
        "user-1"
    }

    fn app_name(&self) -> &'static str {
        "elitea-agent-v1"
    }

    fn session_id(&self) -> &'static str {
        CONVERSATION_THREAD
    }

    fn branch(&self) -> &'static str {
        ""
    }

    fn user_content(&self) -> &Content {
        &self.user_content
    }
}

#[async_trait::async_trait]
impl CallbackContext for FixtureToolContext {
    fn artifacts(&self) -> Option<Arc<dyn Artifacts>> {
        None
    }
}

#[async_trait::async_trait]
impl ToolContext for FixtureToolContext {
    fn function_call_id(&self) -> &str {
        self.call_id
    }

    fn actions(&self) -> EventActions {
        self.actions
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .clone()
    }

    fn set_actions(&self, actions: EventActions) {
        *self
            .actions
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = actions;
    }

    async fn search_memory(&self, _query: &str) -> adk_rust::Result<Vec<MemoryEntry>> {
        Ok(Vec::new())
    }
}

struct FixtureSession {
    events: Vec<Event>,
}

impl adk_rust::session::Session for FixtureSession {
    fn id(&self) -> &'static str {
        CONVERSATION_THREAD
    }

    fn app_name(&self) -> &'static str {
        "elitea-agent-v1"
    }

    fn user_id(&self) -> &'static str {
        "user-1"
    }

    fn state(&self) -> &dyn adk_rust::session::State {
        self
    }

    fn events(&self) -> &dyn adk_rust::session::Events {
        self
    }

    fn last_update_time(&self) -> chrono::DateTime<chrono::Utc> {
        chrono::Utc::now()
    }
}

impl adk_rust::session::State for FixtureSession {
    fn get(&self, _key: &str) -> Option<Value> {
        None
    }

    fn set(&mut self, _key: String, _value: Value) {}

    fn all(&self) -> HashMap<String, Value> {
        HashMap::new()
    }
}

impl adk_rust::session::Events for FixtureSession {
    fn all(&self) -> Vec<Event> {
        self.events.clone()
    }

    fn len(&self) -> usize {
        self.events.len()
    }

    fn at(&self, index: usize) -> Option<&Event> {
        self.events.get(index)
    }
}

struct Harness {
    tool: ApplicationPipelineTool,
    events: mpsc::Receiver<ApplicationEventSignal>,
    resume: ApplicationResumeCoordinator,
    /// Also the way a test puts a node event on the child's channel — the
    /// real producer is the child's `llm` node factory.
    node_events: super::graph::PipelineNodeEventSender,
}

fn harness(yaml: &str) -> Harness {
    harness_named(yaml, TOOL_NAME)
}

fn harness_named(yaml: &str, tool_name: &str) -> Harness {
    let definition = PipelineDefinition::from_yaml(yaml).expect("pipeline definition");
    let (node_sender, node_receiver) = pipeline_node_event_channel();
    let (sender, events) = mpsc::channel(64);
    let resume = ApplicationResumeCoordinator::default();
    let tool = ApplicationPipelineTool::new(
        tool_name.to_owned(),
        pipeline_tool_description("review-pipeline", Some("Reviews a change.")),
        definition,
        PipelineNodeRuntimes::default(),
        node_receiver,
        super::application_pipeline::PipelineToolParentBinding {
            conversation_thread_id: CONVERSATION_THREAD.to_owned(),
            event_sender: Some(sender),
            resume: Some(resume.clone()),
        },
    );
    Harness {
        tool,
        events,
        resume,
        node_events: node_sender,
    }
}

fn context() -> Arc<dyn ToolContext> {
    turn_context(INVOCATION_ID)
}

fn turn_context(invocation_id: &'static str) -> Arc<dyn ToolContext> {
    Arc::new(FixtureToolContext::new(invocation_id))
}

/// The SECOND pipeline call of one assistant message — same invocation, its
/// own call id, which is what makes the two children's threads and identities
/// distinct.
fn second_call_context() -> Arc<dyn ToolContext> {
    Arc::new(FixtureToolContext::for_call(INVOCATION_ID, SECOND_CALL_ID))
}

/// One assistant message calling TWO different saved pipelines.
fn two_pipeline_call_event() -> Event {
    let mut event = turn_root_call_event("root-call", INVOCATION_ID);
    if let Some(content) = event.llm_response.content.as_mut() {
        content.parts.push(Part::FunctionCall {
            name: SECOND_TOOL_NAME.to_owned(),
            args: json!({"task": "ship it"}),
            id: Some(SECOND_CALL_ID.to_owned()),
            thought_signature: None,
        });
    }
    event
}

fn two_pipeline_presentations() -> ApplicationToolPresentationCatalog {
    let mut applications = presentations();
    applications
        .insert_runtime(
            SECOND_TOOL_NAME.to_owned(),
            "second-pipeline".to_owned(),
            "pipeline".to_owned(),
            "pipeline".to_owned(),
            ApplicationToolPresentationCatalog::default(),
            ApplicationToolGuardCatalogs::default(),
        )
        .expect("second pipeline presentation");
    applications
}

/// Both cards of one pause, answered together — the shape Main sends when a
/// message raised more than one.
fn two_decision_payload(first: &str, second: &str) -> AgentExecutionPayload {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    request.payload.should_continue = true;
    request.payload.hitl_resume = true;
    request.payload.hitl_decisions = vec![
        json!({"interrupt_id": first, "action": "approve"}),
        json!({"interrupt_id": second, "action": "reject"}),
    ];
    request.payload
}

/// The parent model's own message: the call the pause and the resume are both
/// bound to.
fn root_call_event() -> Event {
    turn_root_call_event("root-call", INVOCATION_ID)
}

/// The tool call one TURN's model message carries. The replayed call of a
/// later turn is byte-identical except for the event and invocation ids.
fn turn_root_call_event(event_id: &str, invocation_id: &'static str) -> Event {
    let mut event = Event::with_id(event_id, invocation_id);
    event.author = "root-agent".to_owned();
    event.llm_response.content = Some(Content {
        role: "model".to_owned(),
        parts: vec![Part::FunctionCall {
            name: TOOL_NAME.to_owned(),
            args: json!({"task": "ship it"}),
            id: Some(CALL_ID.to_owned()),
            thought_signature: None,
        }],
    });
    event.llm_response.turn_complete = true;
    event.llm_response.finish_reason = Some(adk_rust::FinishReason::Stop);
    event
}

/// Everything the tool forwarded, stamped exactly as the parent's streaming
/// agent stamps it before the Runner persists it.
fn persisted(events: &mut mpsc::Receiver<ApplicationEventSignal>) -> Vec<Event> {
    let mut persisted = Vec::new();
    while let Ok(signal) = events.try_recv() {
        persisted.push(application_signal_event(signal).expect("descendant event"));
    }
    persisted
}

fn pause_event(events: &[Event]) -> &Event {
    events
        .iter()
        .find(|event| event.provider_metadata.contains_key(INTERRUPT_METADATA_KEY))
        .expect("the child pipeline's pause must reach the parent's event channel")
}

fn presentations() -> ApplicationToolPresentationCatalog {
    let mut applications = ApplicationToolPresentationCatalog::default();
    applications
        .insert_runtime(
            TOOL_NAME.to_owned(),
            "review-pipeline".to_owned(),
            "pipeline".to_owned(),
            "pipeline".to_owned(),
            ApplicationToolPresentationCatalog::default(),
            ApplicationToolGuardCatalogs::default(),
        )
        .expect("pipeline presentation");
    applications
}

/// One assistant message carrying the pipeline call AND one ordinary tool
/// call beside it, as the model really emits them.
fn root_call_event_with_sibling(event_id: &str, invocation_id: &'static str) -> Event {
    let mut event = turn_root_call_event(event_id, invocation_id);
    if let Some(content) = event.llm_response.content.as_mut() {
        content.parts.insert(
            0,
            Part::FunctionCall {
                name: "some_tool".to_owned(),
                args: json!({"q": "anything"}),
                id: Some("call-0".to_owned()),
                thought_signature: None,
            },
        );
    }
    event
}

/// The response event ADK persists once every call of a message has finished
/// — it lands AFTER the forwarded pause, which is exactly what a "the pause
/// must be the last event" rule would have choked on (#990 review 2).
fn sibling_response_event(event_id: &str, invocation_id: &'static str) -> Event {
    let mut event = Event::with_id(event_id, invocation_id);
    event.author = "root-agent".to_owned();
    event.llm_response.content = Some(Content {
        role: "function".to_owned(),
        parts: vec![Part::FunctionResponse {
            function_response: adk_rust::FunctionResponseData::new(
                "some_tool",
                json!({"ok": true}),
            ),
            id: Some("call-0".to_owned()),
            annotations: None,
        }],
    });
    event
}

fn decision_payload(interrupt_id: &str, action: &str, value: &str) -> AgentExecutionPayload {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    request.payload.should_continue = true;
    request.payload.hitl_resume = true;
    request.payload.hitl_action = Some(action.to_owned());
    request.payload.hitl_value = Some(value.to_owned());
    request.payload.hitl_decisions = vec![json!({
        "interrupt_id": interrupt_id,
        "action": action,
        "value": value,
    })];
    request.payload
}

/// Answer the pause the way Main does: admit the browser decision, resolve it
/// against the persisted events, and install the continuation the replayed
/// call will pick up.
async fn answer(
    harness: &Harness,
    events: &[Event],
    interrupt_id: &str,
    action: &str,
    value: &str,
) {
    let session = FixtureSession {
        events: events.to_vec(),
    };
    let start = DirectHitlDecisionSet::from_payload(&decision_payload(interrupt_id, action, value))
        .expect("decision admission")
        .resolve(&session)
        .expect("decision resolution");
    let super::direct_hitl::ResolvedDirectHitlStart::Nested(decisions) = start else {
        panic!("a child pipeline's pause must resolve as a nested continuation");
    };
    install_nested_application_resume(events, decisions, &presentations(), &harness.resume)
        .await
        .expect("installed continuation");
}

#[tokio::test(flavor = "current_thread")]
async fn the_child_pipeline_is_offered_as_one_task_tool() {
    let harness = harness(REVIEW_PIPELINE);
    assert_eq!(harness.tool.name(), TOOL_NAME);
    assert!(
        harness.tool.description().contains("review-pipeline"),
        "the model must be told which saved pipeline this is: {}",
        harness.tool.description()
    );
    assert!(
        harness.tool.description().contains("Reviews a change."),
        "the child's own stored purpose must reach the model"
    );
    let schema = harness.tool.parameters_schema().expect("parameters schema");
    assert_eq!(schema["required"], json!(["task"]));
    assert_eq!(schema["properties"]["task"]["type"], "string");
    assert_eq!(schema["additionalProperties"], json!(false));
}

#[tokio::test(flavor = "current_thread")]
async fn a_plain_child_run_returns_the_pipelines_own_result() {
    let mut harness = harness(PLAIN_PIPELINE);
    let result = harness
        .tool
        .execute(context(), json!({"task": "ship it"}))
        .await
        .expect("the child pipeline must run");
    assert_eq!(result, json!({"response": "HANDLED ship it"}));
    assert!(
        nested_application_interrupt_ids(&result).is_none(),
        "a pipeline that never pauses must not report an interrupt"
    );
    assert!(
        persisted(&mut harness.events)
            .iter()
            .all(|event| !event.provider_metadata.contains_key(INTERRUPT_METADATA_KEY)),
        "a pipeline that never pauses must forward no interrupt"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn a_child_hitl_pause_reaches_the_parent_and_resumes_through_it() {
    let mut harness = harness(REVIEW_PIPELINE);
    let paused = harness
        .tool
        .execute(context(), json!({"task": "ship it"}))
        .await
        .expect("the child pipeline must pause rather than fail");
    let interrupt_ids =
        nested_application_interrupt_ids(&paused).expect("the call must report its pause");
    assert_eq!(interrupt_ids.len(), 1);
    let interrupt_id = interrupt_ids.iter().next().expect("interrupt id").clone();

    let mut events = vec![root_call_event()];
    events.extend(persisted(&mut harness.events));
    let pause = pause_event(&events);
    assert_eq!(
        pause
            .provider_metadata
            .get(DESCENDANT_CHECKPOINT_THREAD_KEY),
        Some(&format!("{CONVERSATION_THREAD}/{CALL_ID}")),
        "the child's checkpoint thread must be derived from the conversation and the call"
    );
    assert!(
        pause
            .provider_metadata
            .contains_key(PIPELINE_TOOL_PENDING_METADATA_KEY),
        "the pause must carry the pending checkpoint the resume re-enters"
    );

    answer(&harness, &events, &interrupt_id, "reject", "").await;
    let resumed = harness
        .tool
        .execute(context(), json!({"task": "ship it"}))
        .await
        .expect("the answered pause must re-enter the child graph");
    assert_eq!(resumed, json!({"response": "REJECTED ship it"}));
}

#[tokio::test(flavor = "current_thread")]
async fn the_approve_route_completes_through_the_parent() {
    let mut harness = harness(REVIEW_PIPELINE);
    let paused = harness
        .tool
        .execute(context(), json!({"task": "ship it"}))
        .await
        .expect("paused child");
    let interrupt_id = nested_application_interrupt_ids(&paused)
        .expect("pause")
        .iter()
        .next()
        .expect("interrupt id")
        .clone();
    let mut events = vec![root_call_event()];
    events.extend(persisted(&mut harness.events));

    answer(&harness, &events, &interrupt_id, "approve", "").await;
    let resumed = harness
        .tool
        .execute(context(), json!({"task": "ship it"}))
        .await
        .expect("resumed child");
    assert_eq!(resumed, json!({"response": "APPROVED ship it"}));
}

/// Two gates, driven the way production drives them: the durable session
/// ACCUMULATES, so when the second gate pauses the first gate's answered card
/// is still sitting in it. Each turn is its own invocation with its own
/// replayed call event, and that is what keeps the answered card from being
/// demanded again — the check that requires every card of a pause to be
/// answered scopes itself by THIS turn's call event.
#[tokio::test(flavor = "current_thread")]
async fn two_sequential_gates_each_raise_their_own_card() {
    let mut harness = harness(TWO_GATE_PIPELINE);
    let mut session = vec![turn_root_call_event("root-call-1", "invocation-1")];

    let first = harness
        .tool
        .execute(turn_context("invocation-1"), json!({"task": "ship it"}))
        .await
        .expect("first pause");
    let first_id = nested_application_interrupt_ids(&first)
        .expect("first pause")
        .iter()
        .next()
        .expect("first interrupt id")
        .clone();
    session.extend(persisted(&mut harness.events));
    answer(&harness, &session, &first_id, "approve", "").await;

    session.push(turn_root_call_event("root-call-2", "invocation-2"));
    let second = harness
        .tool
        .execute(turn_context("invocation-2"), json!({"task": "ship it"}))
        .await
        .expect("second pause");
    let second_id = nested_application_interrupt_ids(&second)
        .expect("the second gate must pause too")
        .iter()
        .next()
        .expect("second interrupt id")
        .clone();
    assert_ne!(
        first_id, second_id,
        "each gate must raise its own card, or the second answer would resolve the first"
    );
    session.extend(persisted(&mut harness.events));
    assert_eq!(
        session
            .iter()
            .filter(|event| event
                .provider_metadata
                .contains_key(PIPELINE_TOOL_PENDING_METADATA_KEY))
            .count(),
        2,
        "the answered card must still be in the durable session — that is the case under test"
    );

    answer(&harness, &session, &second_id, "approve", "").await;
    let resumed = harness
        .tool
        .execute(turn_context("invocation-2"), json!({"task": "ship it"}))
        .await
        .expect("resumed after the second gate");
    assert_eq!(resumed, json!({"response": "APPROVED ship it"}));
}

/// The answered card of an EARLIER turn is not answerable again: it is no
/// longer the session's last event, and its own turn is over.
#[tokio::test(flavor = "current_thread")]
async fn an_answered_card_cannot_be_answered_a_second_time() {
    let mut harness = harness(TWO_GATE_PIPELINE);
    let mut session = vec![turn_root_call_event("root-call-1", "invocation-1")];
    let first = harness
        .tool
        .execute(turn_context("invocation-1"), json!({"task": "ship it"}))
        .await
        .expect("first pause");
    let first_id = nested_application_interrupt_ids(&first)
        .expect("first pause")
        .iter()
        .next()
        .expect("first interrupt id")
        .clone();
    session.extend(persisted(&mut harness.events));
    answer(&harness, &session, &first_id, "approve", "").await;

    session.push(turn_root_call_event("root-call-2", "invocation-2"));
    let _second = harness
        .tool
        .execute(turn_context("invocation-2"), json!({"task": "ship it"}))
        .await
        .expect("second pause");
    session.extend(persisted(&mut harness.events));

    let stale = FixtureSession {
        events: session.clone(),
    };
    let result = DirectHitlDecisionSet::from_payload(&decision_payload(&first_id, "approve", ""))
        .expect("decision admission")
        .resolve(&stale);
    assert!(
        result.is_err(),
        "the first gate's card was already answered and must not resume anything"
    );
}

/// The pending checkpoint is worker-private. It rides the PERSISTED event so a
/// later turn can re-enter the child graph, and it must not reach a browser
/// card or a stored trace step.
#[tokio::test(flavor = "current_thread")]
async fn the_pending_checkpoint_never_reaches_a_projected_event() {
    let mut harness = harness(REVIEW_PIPELINE);
    let _paused = harness
        .tool
        .execute(context(), json!({"task": "ship it"}))
        .await
        .expect("paused child");
    let forwarded = persisted(&mut harness.events);
    let pause = pause_event(&forwarded).clone();

    let mut projector = AgentEventProjector::with_tool_catalogs(
        AgentEventProjectionContext::fixture(json!({})),
        SensitiveToolCatalog::default(),
        presentations(),
    )
    .expect("projector");
    projector.start(chrono::Utc::now()).expect("agent start");
    let mut projected = projector
        .project(&root_call_event())
        .expect("root delegation")
        .into_iter()
        .collect::<Vec<_>>();
    projected.extend(projector.project(&pause).expect("projected pause"));

    let rendered = projected
        .iter()
        .map(|event| {
            String::from_utf8(
                crate::protocol::node_event::encode_current_node_event_json(event)
                    .expect("valid projected browser event"),
            )
            .expect("projected event JSON")
        })
        .collect::<Vec<_>>();
    assert!(
        rendered
            .iter()
            .any(|event| event.contains("agent_hitl_interrupt")),
        "the child pipeline's HITL card must reach the browser: {rendered:?}"
    );
    for event in &rendered {
        assert!(
            !event.contains(PIPELINE_TOOL_PENDING_METADATA_KEY),
            "a projected event carried the private pending marker: {event}"
        );
        assert!(
            !event.contains("pending_nodes"),
            "a projected event carried the child's raw checkpoint: {event}"
        );
    }
}

/// A pause whose own pending state was truncated or edited is refused, not
/// re-entered: the envelope's identity and the checkpoint inside it have to
/// agree, and both have to describe the card the browser answered.
#[tokio::test(flavor = "current_thread")]
async fn a_tampered_pending_checkpoint_is_refused() {
    let mut harness = harness(REVIEW_PIPELINE);
    let paused = harness
        .tool
        .execute(context(), json!({"task": "ship it"}))
        .await
        .expect("paused child");
    let interrupt_id = nested_application_interrupt_ids(&paused)
        .expect("pause")
        .iter()
        .next()
        .expect("interrupt id")
        .clone();
    let mut events = vec![root_call_event()];
    events.extend(persisted(&mut harness.events));
    let index = events
        .iter()
        .position(|event| {
            event
                .provider_metadata
                .contains_key(PIPELINE_TOOL_PENDING_METADATA_KEY)
        })
        .expect("pause index");
    let original = events[index]
        .provider_metadata
        .get(PIPELINE_TOOL_PENDING_METADATA_KEY)
        .expect("pending state")
        .clone();

    for (label, tampered) in [
        ("truncated", original[..original.len() / 2].to_owned()),
        ("empty", String::new()),
        (
            "another thread",
            original.replace(
                &format!("{CONVERSATION_THREAD}/{CALL_ID}"),
                "thread-1/call-9",
            ),
        ),
        (
            "another node",
            original.replacen("\"node_name\":\"review\"", "\"node_name\":\"approved\"", 1),
        ),
    ] {
        let mut broken = events.clone();
        broken[index]
            .provider_metadata
            .insert(PIPELINE_TOOL_PENDING_METADATA_KEY.to_owned(), tampered);
        let session = FixtureSession { events: broken };
        let result =
            DirectHitlDecisionSet::from_payload(&decision_payload(&interrupt_id, "approve", ""))
                .expect("decision admission")
                .resolve(&session);
        assert!(
            result.is_err(),
            "a {label} pending checkpoint must not resume the child graph"
        );
    }
}

/// A gate whose message is FIXED, so the state can be large without the
/// rendered card being large: the only way to reach the oversize arm.
const WIDE_STATE_PIPELINE: &str = r#"
state:
  input:
    type: str
  messages:
    type: list
  verdict:
    type: str
entry_point: review
nodes:
  - id: review
    type: hitl
    input:
      - input
    user_message:
      type: fixed
      value: "Approve?"
    routes:
      approve: approved
      reject: rejected
  - id: approved
    type: state_modifier
    template: "APPROVED"
    input: [input]
    output: [verdict]
    transition: END
  - id: rejected
    type: state_modifier
    template: "REJECTED"
    input: [input]
    output: [verdict]
    transition: END
"#;

/// A pause whose pending state cannot be carried must not be SHOWN.
///
/// The card would be unanswerable — nothing could re-enter the child graph —
/// so the call ends as a result the parent model reads and reports instead,
/// naming the child and the limit. The bound itself is one order of magnitude
/// above what a stored pipeline's own node limits can carry into a `hitl`
/// node, so this arm is reached only by a child that accumulated far more.
#[tokio::test(flavor = "current_thread")]
async fn an_oversize_pause_is_refused_rather_than_shown() {
    let mut harness = harness(WIDE_STATE_PIPELINE);
    // `input` and `messages` both hold the task, so the pending state is about
    // twice this — comfortably past the bound, and still inside the task limit.
    let task = "x".repeat(200_000);
    let result = harness
        .tool
        .execute(context(), json!({"task": task}))
        .await
        .expect("the refusal must end the call, not the turn");

    assert!(
        nested_application_interrupt_ids(&result).is_none(),
        "a refused pause must not report an interrupt the parent would wait on"
    );
    let response = result["response"].as_str().expect("a textual result");
    assert!(
        response.contains(TOOL_NAME)
            && response.contains(&MAX_PIPELINE_TOOL_PENDING_BYTES.to_string()),
        "the refusal must name the child and the limit: {response}"
    );
    assert!(
        persisted(&mut harness.events)
            .iter()
            .all(|event| !event.provider_metadata.contains_key(INTERRUPT_METADATA_KEY)),
        "a refused pause must forward no card"
    );
}

/// The bound is a refusal, not a truncation, and its value is deliberate.
#[test]
fn the_oversize_bound_refuses_rather_than_truncates() {
    // One order of magnitude above what the graph's own node limits can carry
    // into a `hitl` node, so a legitimate pause is never refused by it.
    assert_eq!(MAX_PIPELINE_TOOL_PENDING_BYTES, 256 * 1_024);
}

#[test]
fn a_pause_marker_is_the_only_thing_that_makes_an_event_a_pause() {
    let ordinary = root_call_event();
    assert!(
        pipeline_pause_identity(&ordinary)
            .expect("an ordinary event is not a pause")
            .is_none(),
        "only an event this worker wrote may be read as a child-pipeline pause"
    );
}

/// #990 review 1 — THE HISTORY LEAK.
///
/// ADK's `event_belongs_to_branch` treats an EMPTY branch as visible to every
/// branch, so a child event persisted without one is re-read into the ORDINARY
/// parent's own conversation on its next turn: the child's monologue becomes
/// the parent's, and a `tool_use` with no matching `tool_result` in the
/// parent's own history makes an Anthropic-shaped provider refuse the turn.
/// The child's events must therefore sit on a strictly deeper branch, exactly
/// as an agent child's do — and the assertion is made against ADK's own
/// visibility function rather than against the branch STRING, because the
/// string is not the contract.
#[tokio::test(flavor = "current_thread")]
async fn the_childs_events_are_invisible_to_the_parents_next_turn() {
    let mut harness = harness(REVIEW_PIPELINE);
    let _paused = harness
        .tool
        .execute(context(), json!({"task": "ship it"}))
        .await
        .expect("paused child");
    let forwarded = persisted(&mut harness.events);
    assert!(
        !forwarded.is_empty(),
        "the child forwarded nothing, so this proves nothing"
    );
    for event in &forwarded {
        assert!(
            !event.branch.is_empty(),
            "a child event carried no branch, which ADK treats as visible everywhere: {:?}",
            event.id
        );
        assert!(
            !adk_rust::event_belongs_to_branch(
                super::events::APPLICATION_BRANCH_ROOT,
                &event.branch
            ),
            "the parent's own turn can still see the child event {:?} on branch {:?}",
            event.id,
            event.branch
        );
        // …and the child's own run still sees it, or the child could not read
        // its own transcript.
        assert!(adk_rust::event_belongs_to_branch(
            &event.branch,
            &event.branch
        ));
    }
}

/// #990 review 2 — a pipeline call with ANOTHER call beside it in the same
/// assistant message. ADK persists that tool's response after the forwarded
/// pause, so a "the pause must be the last event" rule made the card
/// permanently unanswerable.
#[tokio::test(flavor = "current_thread")]
async fn a_pause_is_answerable_with_another_call_beside_it() {
    let mut harness = harness(REVIEW_PIPELINE);
    let paused = harness
        .tool
        .execute(context(), json!({"task": "ship it"}))
        .await
        .expect("paused child");
    let interrupt_id = nested_application_interrupt_ids(&paused)
        .expect("pause")
        .iter()
        .next()
        .expect("interrupt id")
        .clone();

    let mut events = vec![root_call_event_with_sibling("root-call", INVOCATION_ID)];
    events.extend(persisted(&mut harness.events));
    // The sibling's result lands AFTER the pause, which is the whole point.
    events.push(sibling_response_event("sibling-response", INVOCATION_ID));

    answer(&harness, &events, &interrupt_id, "reject", "").await;
    let resumed = harness
        .tool
        .execute(context(), json!({"task": "ship it"}))
        .await
        .expect("the answered pause must resume despite the sibling's later result");
    assert_eq!(resumed, json!({"response": "REJECTED ship it"}));
}

/// #990 review 2 — TWO pipeline calls in one message. Both cards must be
/// answerable, and the completeness rule demands both be answered together.
#[tokio::test(flavor = "current_thread")]
async fn two_pipeline_calls_in_one_message_are_both_answerable() {
    let mut first = harness(REVIEW_PIPELINE);
    let mut second = harness_named(REVIEW_PIPELINE, SECOND_TOOL_NAME);
    let first_paused = first
        .tool
        .execute(turn_context(INVOCATION_ID), json!({"task": "ship it"}))
        .await
        .expect("first pipeline paused");
    let second_paused = second
        .tool
        .execute(second_call_context(), json!({"task": "ship it"}))
        .await
        .expect("second pipeline paused");
    let first_id = only_interrupt_id(&first_paused);
    let second_id = only_interrupt_id(&second_paused);
    assert_ne!(first_id, second_id);

    let mut events = vec![two_pipeline_call_event()];
    events.extend(persisted(&mut first.events));
    events.extend(persisted(&mut second.events));

    let session = FixtureSession {
        events: events.clone(),
    };
    let start = DirectHitlDecisionSet::from_payload(&two_decision_payload(&first_id, &second_id))
        .expect("both decisions admitted")
        .resolve(&session)
        .expect("both decisions resolved");
    let super::direct_hitl::ResolvedDirectHitlStart::Nested(decisions) = start else {
        panic!("two pipeline pauses must resolve as a nested continuation");
    };
    assert_eq!(decisions.len(), 2);
    install_nested_application_resume(
        &events,
        decisions,
        &two_pipeline_presentations(),
        &first.resume,
    )
    .await
    .expect("installed both continuations");
}

fn only_interrupt_id(result: &Value) -> String {
    nested_application_interrupt_ids(result)
        .expect("a pause")
        .iter()
        .next()
        .expect("interrupt id")
        .clone()
}

/// One node event, the shape a child's `llm` node puts on the channel.
fn node_event(text: &str) -> Event {
    let mut event = Event::new("child-node");
    event.set_content(Content::new("assistant").with_text(text));
    event
}

/// #990 review 5 — no node event is ever attributed to the WRONG call.
///
/// The channel belongs to the tool, not to one call, and `drain_child` returns
/// as soon as an interrupt arrives. Two rules keep that safe and this pins the
/// first: whatever is already queued when a call STARTS belongs to an earlier
/// call that paused, so it is discarded rather than stamped with this call's
/// invocation and function-call ids. The second rule — the pause forwards what
/// its OWN run produced before it returns — is the `try_recv` sweep on the
/// interrupt path, and the test below proves its consequence: nothing an
/// earlier call left behind ever reappears.
#[tokio::test(flavor = "current_thread")]
async fn a_node_event_queued_before_a_call_is_never_attributed_to_it() {
    let mut harness = harness(REVIEW_PIPELINE);
    harness
        .node_events
        .send("review", None, node_event("an earlier call's progress"))
        .await
        .expect("queued node event");

    let paused = harness
        .tool
        .execute(context(), json!({"task": "ship it"}))
        .await
        .expect("paused child");
    assert!(nested_application_interrupt_ids(&paused).is_some());

    let forwarded = persisted(&mut harness.events);
    assert!(
        !forwarded
            .iter()
            .any(|event| event
                .content()
                .is_some_and(|content| content.parts.iter().any(|part| matches!(
                    part,
                    Part::Text { text } if text == "an earlier call's progress"
                )))),
        "a call forwarded a node event that was queued before it started"
    );
    // The pause itself still reached the parent — the discard is a filter on
    // stale progress, not on the card.
    assert!(
        forwarded
            .iter()
            .any(|event| event.provider_metadata.contains_key(INTERRUPT_METADATA_KEY)),
        "discarding stale progress must not swallow the pause"
    );
}

/// The other half: what one call did NOT drain must not be attributed to the
/// next call of the same tool.
#[tokio::test(flavor = "current_thread")]
async fn a_later_call_never_replays_an_earlier_calls_node_events() {
    let mut harness = harness(PLAIN_PIPELINE);
    // Queued but never claimed — the sender outlives any one call, which is
    // exactly the hazard.
    harness
        .node_events
        .send("decide", None, node_event("stale progress"))
        .await
        .expect("queued node event");
    // A call that consumes it, and a SECOND call that must not see it again.
    let _first = harness
        .tool
        .execute(context(), json!({"task": "ship it"}))
        .await
        .expect("first call");
    let _drained = persisted(&mut harness.events);

    harness
        .node_events
        .send("decide", None, node_event("stale progress"))
        .await
        .expect("queued node event");
    // The second call starts by discarding whatever the first left behind.
    let _second = harness
        .tool
        .execute(second_call_context(), json!({"task": "ship it"}))
        .await
        .expect("second call");
    let forwarded = persisted(&mut harness.events);
    assert!(
        !forwarded
            .iter()
            .any(|event| event
                .content()
                .is_some_and(|content| content.parts.iter().any(|part| matches!(
                    part,
                    Part::Text { text } if text == "stale progress"
                )))),
        "a later call replayed an earlier call's node event under its own identity"
    );
}
