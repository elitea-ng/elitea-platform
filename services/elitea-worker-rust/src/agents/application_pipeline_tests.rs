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

struct FixtureToolContext {
    /// Every TURN is a fresh invocation, and that is load-bearing rather than
    /// decorative: the resume's completeness check scopes itself by the event
    /// carrying the parent's tool call, and it is the invocation id that tells
    /// this turn's call event apart from the identical replayed call of the
    /// turn before — which is what stops an already-answered pause being
    /// demanded again.
    invocation_id: &'static str,
    user_content: Content,
    actions: std::sync::Mutex<EventActions>,
}

impl FixtureToolContext {
    fn new(invocation_id: &'static str) -> Self {
        Self {
            invocation_id,
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
    fn function_call_id(&self) -> &'static str {
        CALL_ID
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
    /// Kept alive so the node-event channel does not close under the tool.
    _node_events: super::graph::PipelineNodeEventSender,
}

fn harness(yaml: &str) -> Harness {
    let definition = PipelineDefinition::from_yaml(yaml).expect("pipeline definition");
    let (node_sender, node_receiver) = pipeline_node_event_channel();
    let (sender, events) = mpsc::channel(64);
    let resume = ApplicationResumeCoordinator::default();
    let tool = ApplicationPipelineTool::new(
        TOOL_NAME.to_owned(),
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
        _node_events: node_sender,
    }
}

fn context() -> Arc<dyn ToolContext> {
    turn_context(INVOCATION_ID)
}

fn turn_context(invocation_id: &'static str) -> Arc<dyn ToolContext> {
    Arc::new(FixtureToolContext::new(invocation_id))
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
