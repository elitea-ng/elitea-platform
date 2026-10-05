use super::EliteaGraphAgent;
use super::compiler::PipelineDefinition;
use super::resume::{PipelineResume, PipelineResumeErrorCode};
use super::static_pause::{
    PipelineTextContinuation, STATIC_PAUSE_METADATA_KEY, StaticResumeCheckpointer,
};
use crate::agents::events::{
    AgentEventProjectionContext, AgentEventProjector, CompletedAgentBrowserOutput,
};
use crate::agents::request::{AgentExecutionPayload, NextInputSuggestionPolicy, UserInput};
use crate::agents::runtime::NativeAgentInvocation;
use adk_rust::graph::interrupt::GraphInterruptPayload;
use adk_rust::graph::{Checkpoint, Checkpointer, MemoryCheckpointer, State};
use adk_rust::runner::Runner;
use adk_rust::session::{CreateRequest, GetRequest, InMemorySessionService, SessionService};
use adk_rust::{Content, Event, SessionId, UserId};
use chrono::Utc;
use serde_json::{Map, json};
use std::collections::{BTreeMap, HashMap};
use std::sync::Arc;

const APP: &str = "elitea";
const ROOT: &str = "root-agent";
const USER: &str = "user-1";
const THREAD: &str = "thread-1";
const SINGLE: &str = r#"
state:
  count: {type: int, value: 0}
entry_point: tick
nodes:
  - id: tick
    type: state_modifier
    template: "{{ count + 1 }}"
    input: [count]
    output: [count]
    transition: END
"#;

#[test]
fn static_admission_is_bounded_exact_and_digest_bound() {
    let baseline = PipelineDefinition::from_yaml(SINGLE).unwrap();
    for field in ["interrupt_before", "interrupt_after"] {
        let admitted =
            PipelineDefinition::from_yaml(&format!("{field}: [tick]\n{SINGLE}")).unwrap();
        assert_ne!(admitted.definition_digest(), baseline.definition_digest());
        for values in ["[absent]", "[tick, tick]", "[END]", "[bad/id]"] {
            assert!(
                PipelineDefinition::from_yaml(&format!("{field}: {values}\n{SINGLE}")).is_err()
            );
        }
        assert!(
            PipelineDefinition::from_yaml(&format!(
                "{field}: [{}]\n{SINGLE}",
                vec!["tick"; 129].join(",")
            ))
            .is_err()
        );
    }
    assert_eq!(
        baseline.definition_digest(),
        PipelineDefinition::from_yaml(&format!(
            "interrupt_before: []\ninterrupt_after: []\n{SINGLE}"
        ))
        .unwrap()
        .definition_digest()
    );
}

#[tokio::test]
async fn before_continuation_executes_the_node_once() {
    let definition =
        PipelineDefinition::from_yaml(&format!("interrupt_before: [tick]\n{SINGLE}")).unwrap();
    let (checkpointer, sessions) = fixture().await;
    let paused = run(&definition, checkpointer.clone(), sessions.clone(), None).await;
    assert_eq!(
        GraphInterruptPayload::from_event(&paused[0]).unwrap().kind,
        "before"
    );
    assert_eq!(
        checkpointer.load(THREAD).await.unwrap().unwrap().state["count"],
        json!(0)
    );
    let paused_session = get_session(sessions.as_ref()).await;
    let original_selection = bound_resume_payload("again", paused_session.as_ref());
    let resume = continuation(&definition, checkpointer.as_ref(), sessions.as_ref()).await;
    let finished = run(
        &definition,
        checkpointer.clone(),
        sessions.clone(),
        Some(resume),
    )
    .await;
    assert!(finished.iter().all(|event| {
        !event
            .provider_metadata
            .contains_key(STATIC_PAUSE_METADATA_KEY)
    }));
    let final_checkpoint = checkpointer.load(THREAD).await.unwrap().unwrap();
    assert!(final_checkpoint.pending_nodes.is_empty());
    assert_eq!(final_checkpoint.state["count"], json!(1));
    let session = get_session(sessions.as_ref()).await;
    for payload in [
        bound_resume_payload("again", session.as_ref()),
        original_selection,
    ] {
        let result = PipelineTextContinuation::from_payload(&payload)
            .unwrap()
            .resolve(
                session.as_ref(),
                checkpointer.as_ref(),
                ROOT,
                THREAD,
                &definition.printer_pause_catalog(),
                &definition.static_pause_catalog(),
                "again",
            )
            .await;
        assert_eq!(
            result.err().unwrap().code(),
            PipelineResumeErrorCode::StaleDecision
        );
    }
}

#[tokio::test]
async fn malformed_static_pause_is_not_classified_as_a_completed_run() {
    let definition =
        PipelineDefinition::from_yaml(&format!("interrupt_before: [tick]\n{SINGLE}")).unwrap();
    let (checkpointer, sessions) = fixture().await;
    let _paused = run(&definition, checkpointer.clone(), sessions.clone(), None).await;
    let paused_session = get_session(sessions.as_ref()).await;
    let payload = bound_resume_payload("continue", paused_session.as_ref());
    let mut malformed = Event::new("malformed-static-pause");
    malformed.author = ROOT.to_owned();
    malformed.set_content(Content::new("assistant").with_text("Pipeline paused."));
    malformed
        .provider_metadata
        .insert(STATIC_PAUSE_METADATA_KEY.to_owned(), "{invalid".to_owned());
    sessions.append_event(THREAD, malformed).await.unwrap();
    let session = get_session(sessions.as_ref()).await;
    let result = PipelineTextContinuation::from_payload(&payload)
        .unwrap()
        .resolve(
            session.as_ref(),
            checkpointer.as_ref(),
            ROOT,
            THREAD,
            &definition.printer_pause_catalog(),
            &definition.static_pause_catalog(),
            "continue",
        )
        .await;
    assert_eq!(
        result.err().unwrap().code(),
        PipelineResumeErrorCode::CorruptSession
    );
    assert_eq!(
        checkpointer.load(THREAD).await.unwrap().unwrap().state["count"],
        json!(0)
    );
}

#[tokio::test]
async fn after_continuation_does_not_repeat_completed_effects() {
    let definition =
        PipelineDefinition::from_yaml(&format!("interrupt_after: [tick]\n{SINGLE}")).unwrap();
    let (checkpointer, sessions) = fixture().await;
    let paused = run(&definition, checkpointer.clone(), sessions.clone(), None).await;
    assert_eq!(
        GraphInterruptPayload::from_event(&paused[0]).unwrap().kind,
        "after"
    );
    let checkpoint = checkpointer.load(THREAD).await.unwrap().unwrap();
    assert!(checkpoint.pending_nodes.is_empty());
    assert_eq!(checkpoint.state["count"], json!(1));
    let resume = continuation(&definition, checkpointer.as_ref(), sessions.as_ref()).await;
    let _finished = run(&definition, checkpointer.clone(), sessions, Some(resume)).await;
    assert_eq!(
        checkpointer.load(THREAD).await.unwrap().unwrap().state["count"],
        json!(1)
    );
}

#[tokio::test]
async fn mixed_before_after_rearms_each_loop_visit_and_advances_effect_step() {
    let loop_source = SINGLE.replace("transition: END", "transition: choose")
        + r#"
  - id: choose
    type: router
    condition: "{{ 'tick' if count < 3 else 'END' }}"
    input: [count]
    routes: [tick, END]
    default_output: END
"#;
    let definition = PipelineDefinition::from_yaml(&format!(
        "interrupt_before: [tick]\ninterrupt_after: [tick]\n{loop_source}"
    ))
    .unwrap();
    let (checkpointer, sessions) = fixture().await;
    let mut resume = None;
    let mut kinds = Vec::new();
    let mut visit_steps = Vec::new();
    for index in 0_usize..7 {
        let events = run(
            &definition,
            checkpointer.clone(),
            sessions.clone(),
            resume.take(),
        )
        .await;
        let checkpoint = checkpointer.load(THREAD).await.unwrap().unwrap();
        if index == 6 {
            assert!(checkpoint.pending_nodes.is_empty());
            break;
        }
        let interrupt = events
            .iter()
            .find_map(GraphInterruptPayload::from_event)
            .unwrap_or_else(|| {
                panic!(
                    "missing pause at invocation {index}: count={}, pending={:?}, step={}, cleared={:?}",
                    checkpoint.state["count"],
                    checkpoint.pending_nodes,
                    checkpoint.step,
                    checkpoint.cleared_interrupt,
                )
            });
        kinds.push(interrupt.kind.clone());
        if interrupt.kind == "before" {
            visit_steps.push(checkpoint.step);
        }
        assert_eq!(checkpoint.state["count"], json!(index.div_ceil(2)));
        resume = Some(continuation(&definition, checkpointer.as_ref(), sessions.as_ref()).await);
    }
    assert_eq!(
        kinds,
        ["before", "after", "before", "after", "before", "after"]
    );
    assert!(visit_steps.windows(2).all(|pair| pair[1] > pair[0]));
    assert_eq!(
        checkpointer.load(THREAD).await.unwrap().unwrap().state["count"],
        json!(3)
    );
}

#[tokio::test]
async fn self_loop_rearms_before_and_advances_the_completed_visit() {
    let definition = PipelineDefinition::from_yaml(&format!(
        "interrupt_before: [tick]\ninterrupt_after: [tick]\n{}",
        SINGLE.replace("transition: END", "transition: tick")
    ))
    .unwrap();
    let (checkpointer, sessions) = fixture().await;
    let mut resume = None;
    for (index, kind) in ["before", "after", "before", "after", "before"]
        .iter()
        .enumerate()
    {
        let events = run(
            &definition,
            checkpointer.clone(),
            sessions.clone(),
            resume.take(),
        )
        .await;
        let checkpoint = checkpointer.load(THREAD).await.unwrap().unwrap();
        assert_eq!(
            GraphInterruptPayload::from_event(&events[0]).unwrap().kind,
            *kind
        );
        assert_eq!(checkpoint.step, index / 2);
        assert_eq!(checkpoint.state["count"], json!(index.div_ceil(2)));
        resume = Some(continuation(&definition, checkpointer.as_ref(), sessions.as_ref()).await);
    }
}

#[tokio::test]
async fn continuation_rejects_changed_version_and_stale_frontier() {
    let definition =
        PipelineDefinition::from_yaml(&format!("interrupt_before: [tick]\n{SINGLE}")).unwrap();
    let (checkpointer, sessions) = fixture().await;
    let _paused = run(&definition, checkpointer.clone(), sessions.clone(), None).await;
    let changed = PipelineDefinition::from_yaml(&format!(
        "interrupt_before: [tick]\n{}",
        SINGLE.replace("count + 1", "count + 2")
    ))
    .unwrap();
    let session = get_session(sessions.as_ref()).await;
    let result =
        PipelineTextContinuation::from_payload(&bound_resume_payload("continue", session.as_ref()))
            .unwrap()
            .resolve(
                session.as_ref(),
                checkpointer.as_ref(),
                ROOT,
                THREAD,
                &changed.printer_pause_catalog(),
                &changed.static_pause_catalog(),
                "continue",
            )
            .await;
    assert_eq!(
        result.err().unwrap().code(),
        PipelineResumeErrorCode::StaleDecision
    );
    let mut replacement = checkpointer.load(THREAD).await.unwrap().unwrap();
    replacement.checkpoint_id = "replacement-frontier".to_owned();
    checkpointer.save(&replacement).await.unwrap();
    let result =
        PipelineTextContinuation::from_payload(&bound_resume_payload("continue", session.as_ref()))
            .unwrap()
            .resolve(
                session.as_ref(),
                checkpointer.as_ref(),
                ROOT,
                THREAD,
                &definition.printer_pause_catalog(),
                &definition.static_pause_catalog(),
                "continue",
            )
            .await;
    assert_eq!(
        result.err().unwrap().code(),
        PipelineResumeErrorCode::StaleDecision
    );
}

#[tokio::test]
async fn static_projection_keeps_nonterminal_text_without_a_hitl_card() {
    let definition =
        PipelineDefinition::from_yaml(&format!("interrupt_before: [tick]\n{SINGLE}")).unwrap();
    let (checkpointer, sessions) = fixture().await;
    let paused = run(&definition, checkpointer, sessions, None).await;
    let mut projector =
        AgentEventProjector::new(AgentEventProjectionContext::pipeline_fixture(json!({}))).unwrap();
    projector.start(Utc::now()).unwrap();
    assert_eq!(
        projector.project(&paused[0]).unwrap().into_iter().count(),
        0
    );
    let output = projector
        .finish_after_eos(CompletedAgentBrowserOutput::fixture("fallback"), Utc::now())
        .unwrap()
        .into_iter()
        .collect::<Vec<_>>();
    assert_eq!(
        output
            .iter()
            .map(|event| event.r#type.as_str())
            .collect::<Vec<_>>(),
        ["agent_response", "full_message"]
    );
    assert!(output.iter().all(|event| !event.r#type.contains("hitl")));
    assert!(output.iter().all(|event| event.r#type != "pipeline_finish"));
}

#[tokio::test]
async fn after_rearm_keeps_saved_bytes_and_excludes_other_checkpoint_ids() {
    let inner: Arc<dyn Checkpointer> = Arc::new(MemoryCheckpointer::new());
    let checkpoint = Checkpoint::new(THREAD, State::new(), 4, vec!["tick".to_owned()])
        .with_cleared_interrupt("tick");
    inner.save(&checkpoint).await.unwrap();
    let adapter = StaticResumeCheckpointer::new(
        inner.clone(),
        BTreeMap::from([(THREAD.to_owned(), checkpoint.checkpoint_id.clone())]),
    );
    let loaded = adapter.load(THREAD).await.unwrap().unwrap();
    assert!(loaded.cleared_interrupt.is_none());
    assert_eq!(loaded.step, 5);
    let original = inner.load(THREAD).await.unwrap().unwrap();
    assert_eq!(original.cleared_interrupt.as_deref(), Some("tick"));
    assert_eq!(original.step, 4);
    let other = Checkpoint::new(THREAD, State::new(), 6, vec!["tick".to_owned()])
        .with_cleared_interrupt("tick");
    inner.save(&other).await.unwrap();
    assert_eq!(
        adapter
            .load(THREAD)
            .await
            .unwrap()
            .unwrap()
            .cleared_interrupt
            .as_deref(),
        Some("tick")
    );
}

#[tokio::test]
async fn authored_before_printer_keeps_printer_output_and_reset_semantics() {
    let definition = PipelineDefinition::from_yaml(
        r#"
interrupt_before: [show]
interrupt_after: [show]
state:
  count: {type: int, value: 0}
entry_point: show
nodes:
  - id: show
    type: printer
    input_mapping:
      printer: {type: fixed, value: ready}
    transition: tick
  - id: tick
    type: state_modifier
    template: "{{ count + 1 }}"
    input: [count]
    output: [count]
    transition: END
"#,
    )
    .unwrap();
    let (checkpointer, sessions) = fixture().await;
    let before = run(&definition, checkpointer.clone(), sessions.clone(), None).await;
    assert!(
        before[0]
            .provider_metadata
            .contains_key(STATIC_PAUSE_METADATA_KEY)
    );
    let resume = continuation(&definition, checkpointer.as_ref(), sessions.as_ref()).await;
    let printer = run(
        &definition,
        checkpointer.clone(),
        sessions.clone(),
        Some(resume),
    )
    .await;
    assert!(
        printer[0]
            .provider_metadata
            .contains_key(super::printer::PRINTER_PAUSE_METADATA_KEY)
    );
    assert!(
        !printer[0]
            .provider_metadata
            .contains_key(STATIC_PAUSE_METADATA_KEY)
    );
    assert_eq!(
        checkpointer
            .load(THREAD)
            .await
            .unwrap()
            .unwrap()
            .pending_nodes,
        ["show_reset"]
    );
    let resume = continuation(&definition, checkpointer.as_ref(), sessions.as_ref()).await;
    run(&definition, checkpointer.clone(), sessions, Some(resume)).await;
    assert_eq!(
        checkpointer.load(THREAD).await.unwrap().unwrap().state["count"],
        json!(1)
    );
}

#[tokio::test]
async fn dynamic_hitl_still_precedes_authored_after_and_keeps_actions_distinct() {
    let definition = PipelineDefinition::from_yaml(
        r#"
interrupt_after: [review]
state:
  count: {type: int, value: 0}
entry_point: review
nodes:
  - id: review
    type: hitl
    user_message: {type: fixed, value: Approve.}
    routes: {approve: tick, reject: END}
  - id: tick
    type: state_modifier
    template: "{{ count + 1 }}"
    input: [count]
    output: [count]
    transition: END
"#,
    )
    .unwrap();
    let (checkpointer, sessions) = fixture().await;
    let hitl = run(&definition, checkpointer.clone(), sessions.clone(), None).await;
    let binding =
        crate::agents::events::pipeline_hitl_event_binding(&hitl[0], ROOT, THREAD).unwrap();
    assert!(
        !hitl[0]
            .provider_metadata
            .contains_key(STATIC_PAUSE_METADATA_KEY)
    );
    let mut payload = resume_payload("approve");
    payload.hitl_resume = true;
    payload.hitl_action = Some("approve".to_owned());
    payload.hitl_value = Some(String::new());
    payload.hitl_decisions = vec![
        json!({"interrupt_id":binding.interrupt_id(),"tool_call_id":"","action":"approve","value":""}),
    ];
    let session = get_session(sessions.as_ref()).await;
    let resume = super::resume::PipelineHitlDecision::from_payload(&payload)
        .unwrap()
        .resolve(session.as_ref(), checkpointer.as_ref(), ROOT, THREAD)
        .await
        .unwrap();
    let after = run(
        &definition,
        checkpointer.clone(),
        sessions.clone(),
        Some(resume),
    )
    .await;
    assert!(
        after[0]
            .provider_metadata
            .contains_key(STATIC_PAUSE_METADATA_KEY)
    );
    assert_eq!(
        checkpointer
            .load(THREAD)
            .await
            .unwrap()
            .unwrap()
            .pending_nodes,
        ["tick"]
    );
    let resume = continuation(&definition, checkpointer.as_ref(), sessions.as_ref()).await;
    run(&definition, checkpointer.clone(), sessions, Some(resume)).await;
    assert_eq!(
        checkpointer.load(THREAD).await.unwrap().unwrap().state["count"],
        json!(1)
    );
}

const PARENT: &str = r"
state:
  input: str
  messages: list
entry_point: delegate
nodes:
  - id: delegate
    type: agent
    tool: Research Agent
    input_mapping:
      task: {type: fixed, value: original-task}
    output: [messages]
    transition: END
";

struct StaticChildResolver {
    definition: PipelineDefinition,
    runtimes: super::compiler::PipelineNodeRuntimes,
}
impl super::application::PipelineApplicationResolver for StaticChildResolver {
    fn resolve(
        &self,
        _selection: &super::application::PipelineApplicationSelection,
        checkpointer: Arc<dyn Checkpointer>,
    ) -> Result<
        super::application::ResolvedApplicationParticipant,
        super::application::ApplicationExecutionError,
    > {
        let graph = self
            .definition
            .compile_subgraph_with_runtime(checkpointer, &self.runtimes)
            .map_err(|_| super::application::ApplicationExecutionError::Unavailable)?;
        Ok(
            super::application::ResolvedApplicationParticipant::Pipeline {
                graph: Arc::new(graph),
                variable_types: self.definition.declared_variable_types(),
                static_pauses: self.definition.static_pause_catalog(),
                events: None,
                display_name: "Research Agent".to_owned(),
            },
        )
    }
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // Keep exact before/after cutpoints and effect counts in one recovery scenario.
async fn nested_before_and_after_resume_the_exact_leaf_without_repeating_effects() {
    let child = PipelineDefinition::from_yaml(&format!(
        "interrupt_before: [tick]\ninterrupt_after: [tick]\n{SINGLE}"
    ))
    .unwrap();
    let parent = PipelineDefinition::from_yaml(PARENT).unwrap();
    let runtimes = super::compiler::PipelineNodeRuntimes::new(
        None,
        None,
        Some(Arc::new(StaticChildResolver {
            definition: child.clone(),
            runtimes: super::compiler::PipelineNodeRuntimes::default(),
        })),
    );
    let family = super::static_pause::StaticPauseFamily::new(
        parent.static_pause_catalog(),
        BTreeMap::from([("delegate".to_owned(), child.static_pause_catalog())]),
    );
    let (checkpointer, sessions) = fixture().await;
    let before = run_nested(
        &parent,
        &runtimes,
        checkpointer.clone(),
        sessions.clone(),
        None,
    )
    .await;
    let binding =
        crate::agents::events::pipeline_static_event_binding(&before[0], ROOT, THREAD).unwrap();
    assert_eq!(binding.metadata.kind, "before");
    assert_eq!(
        binding.nested_checkpoints[0].thread_id(),
        format!("{THREAD}/delegate")
    );
    let session = get_session(sessions.as_ref()).await;
    let wrong_family =
        super::static_pause::StaticPauseFamily::new(parent.static_pause_catalog(), BTreeMap::new());
    let result =
        PipelineTextContinuation::from_payload(&bound_resume_payload("continue", session.as_ref()))
            .unwrap()
            .resolve_with_family(
                session.as_ref(),
                checkpointer.as_ref(),
                ROOT,
                THREAD,
                &parent.printer_pause_catalog(),
                &wrong_family,
                "continue",
            )
            .await;
    assert_eq!(
        result.err().unwrap().code(),
        PipelineResumeErrorCode::StaleDecision
    );
    let resume =
        PipelineTextContinuation::from_payload(&bound_resume_payload("continue", session.as_ref()))
            .unwrap()
            .resolve_with_family(
                session.as_ref(),
                checkpointer.as_ref(),
                ROOT,
                THREAD,
                &parent.printer_pause_catalog(),
                &family,
                "continue",
            )
            .await
            .unwrap();
    let after = run_nested(
        &parent,
        &runtimes,
        checkpointer.clone(),
        sessions.clone(),
        Some(resume),
    )
    .await;
    assert_eq!(
        crate::agents::events::pipeline_static_event_binding(&after[0], ROOT, THREAD)
            .unwrap()
            .metadata
            .kind,
        "after"
    );
    let leaf = checkpointer
        .load(&format!("{THREAD}/delegate"))
        .await
        .unwrap()
        .unwrap();
    assert_eq!(leaf.state["count"], json!(1));
    assert_eq!(leaf.state["input"], json!("continue"));
    let session = get_session(sessions.as_ref()).await;
    let resume =
        PipelineTextContinuation::from_payload(&bound_resume_payload("finish", session.as_ref()))
            .unwrap()
            .resolve_with_family(
                session.as_ref(),
                checkpointer.as_ref(),
                ROOT,
                THREAD,
                &parent.printer_pause_catalog(),
                &family,
                "finish",
            )
            .await
            .unwrap();
    run_nested(
        &parent,
        &runtimes,
        checkpointer.clone(),
        sessions,
        Some(resume),
    )
    .await;
    let leaf = checkpointer
        .load(&format!("{THREAD}/delegate"))
        .await
        .unwrap()
        .unwrap();
    assert_eq!(leaf.state["count"], json!(1));
    assert!(leaf.pending_nodes.is_empty());
    assert!(
        checkpointer
            .load(THREAD)
            .await
            .unwrap()
            .unwrap()
            .pending_nodes
            .is_empty()
    );
}

#[tokio::test]
async fn deeper_static_pause_validates_the_full_descendant_path() {
    let leaf =
        PipelineDefinition::from_yaml(&format!("interrupt_before: [tick]\n{SINGLE}")).unwrap();
    let middle = PipelineDefinition::from_yaml(&PARENT.replace("delegate", "specialist")).unwrap();
    let root = PipelineDefinition::from_yaml(PARENT).unwrap();
    let middle_runtime = super::compiler::PipelineNodeRuntimes::new(
        None,
        None,
        Some(Arc::new(StaticChildResolver {
            definition: leaf.clone(),
            runtimes: super::compiler::PipelineNodeRuntimes::default(),
        })),
    );
    let runtime = super::compiler::PipelineNodeRuntimes::new(
        None,
        None,
        Some(Arc::new(StaticChildResolver {
            definition: middle.clone(),
            runtimes: middle_runtime,
        })),
    );
    let family = super::static_pause::StaticPauseFamily::new(
        root.static_pause_catalog(),
        BTreeMap::from([
            ("delegate".to_owned(), middle.static_pause_catalog()),
            (
                "delegate/specialist".to_owned(),
                leaf.static_pause_catalog(),
            ),
        ]),
    );
    let (checkpointer, sessions) = fixture().await;
    let paused = run_nested(
        &root,
        &runtime,
        checkpointer.clone(),
        sessions.clone(),
        None,
    )
    .await;
    let binding =
        crate::agents::events::pipeline_static_event_binding(&paused[0], ROOT, THREAD).unwrap();
    assert_eq!(binding.nested_checkpoints.len(), 2);
    assert_eq!(
        binding.nested_checkpoints[1].thread_id(),
        format!("{THREAD}/delegate/specialist")
    );
    let session = get_session(sessions.as_ref()).await;
    let resume =
        PipelineTextContinuation::from_payload(&bound_resume_payload("continue", session.as_ref()))
            .unwrap()
            .resolve_with_family(
                session.as_ref(),
                checkpointer.as_ref(),
                ROOT,
                THREAD,
                &root.printer_pause_catalog(),
                &family,
                "continue",
            )
            .await
            .unwrap();
    run_nested(
        &root,
        &runtime,
        checkpointer.clone(),
        sessions,
        Some(resume),
    )
    .await;
    assert_eq!(
        checkpointer
            .load(&format!("{THREAD}/delegate/specialist"))
            .await
            .unwrap()
            .unwrap()
            .state["count"],
        json!(1)
    );
}

async fn run_nested(
    definition: &PipelineDefinition,
    runtimes: &super::compiler::PipelineNodeRuntimes,
    checkpointer: Arc<dyn Checkpointer>,
    sessions: Arc<InMemorySessionService>,
    resume: Option<PipelineResume>,
) -> Vec<Event> {
    let graph = definition
        .compile_with_runtime(ROOT, checkpointer.clone(), resume, runtimes)
        .unwrap();
    let agent = EliteaGraphAgent::new(graph)
        .with_static_interrupts(checkpointer, definition.static_pause_catalog());
    let session_service: Arc<dyn SessionService> = sessions;
    let runner = Runner::builder()
        .app_name(APP)
        .agent(Arc::new(agent))
        .session_service(session_service)
        .build()
        .unwrap();
    let mut invocation = NativeAgentInvocation::new(
        runner,
        UserId::new(USER).unwrap(),
        SessionId::new(THREAD).unwrap(),
        Content::new("user").with_text("continue"),
    )
    .start()
    .unwrap();
    let mut events = Vec::new();
    while let Some(event) = invocation.next_event().await.unwrap() {
        events.push(event);
    }
    events
}

async fn fixture() -> (Arc<MemoryCheckpointer>, Arc<InMemorySessionService>) {
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: APP.to_owned(),
            user_id: USER.to_owned(),
            session_id: Some(THREAD.to_owned()),
            state: HashMap::new(),
        })
        .await
        .unwrap();
    (Arc::new(MemoryCheckpointer::new()), sessions)
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
        .unwrap()
}
async fn continuation(
    definition: &PipelineDefinition,
    checkpointer: &dyn Checkpointer,
    sessions: &InMemorySessionService,
) -> PipelineResume {
    let session = get_session(sessions).await;
    PipelineTextContinuation::from_payload(&bound_resume_payload("continue", session.as_ref()))
        .unwrap()
        .resolve(
            session.as_ref(),
            checkpointer,
            ROOT,
            THREAD,
            &definition.printer_pause_catalog(),
            &definition.static_pause_catalog(),
            "continue",
        )
        .await
        .unwrap()
}
async fn run(
    definition: &PipelineDefinition,
    checkpointer: Arc<dyn Checkpointer>,
    sessions: Arc<InMemorySessionService>,
    resume: Option<PipelineResume>,
) -> Vec<Event> {
    let graph = definition
        .compile(ROOT, checkpointer.clone(), resume)
        .unwrap();
    let agent = EliteaGraphAgent::new(graph)
        .with_printer_interrupts(checkpointer.clone(), definition.printer_pause_catalog())
        .with_static_interrupts(checkpointer, definition.static_pause_catalog());
    let session_service: Arc<dyn SessionService> = sessions;
    let runner = Runner::builder()
        .app_name(APP)
        .agent(Arc::new(agent))
        .session_service(session_service)
        .build()
        .unwrap();
    let mut invocation = NativeAgentInvocation::new(
        runner,
        UserId::new(USER).unwrap(),
        SessionId::new(THREAD).unwrap(),
        Content::new("user").with_text("continue"),
    )
    .start()
    .unwrap();
    let mut events = Vec::new();
    while let Some(event) = invocation.next_event().await.unwrap() {
        events.push(event);
    }
    events
}

fn resume_payload(input: &str) -> AgentExecutionPayload {
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
        summary_model: None,
    }
}

fn bound_resume_payload(
    text: &str,
    session: &dyn adk_rust::session::Session,
) -> AgentExecutionPayload {
    let mut payload = resume_payload(text);
    if let Some(event) = session.events().all().last()
        && let Ok(binding) =
            crate::agents::events::pipeline_static_event_binding(event, ROOT, THREAD)
    {
        let proof = binding.public_proof(event).unwrap();
        payload.meta.insert(
            "pipeline_static_resume_v1".to_owned(),
            json!({"revision": 1, "pause_id": proof["pause_id"]}),
        );
    }
    payload
}

#[tokio::test]
async fn public_pause_identity_rejects_another_occurrence_and_missing_static_selection() {
    let definition =
        PipelineDefinition::from_yaml(&format!("interrupt_before: [tick]\n{SINGLE}")).unwrap();
    let (checkpointer, sessions) = fixture().await;
    let paused = run(&definition, checkpointer.clone(), sessions.clone(), None).await;
    let session = get_session(sessions.as_ref()).await;
    let binding =
        crate::agents::events::pipeline_static_event_binding(&paused[0], ROOT, THREAD).unwrap();
    let proof = binding.public_proof(&paused[0]).unwrap();
    assert_eq!(proof["node_name"], json!("tick"));
    assert_eq!(proof["pending_nodes"], json!(["tick"]));
    assert_eq!(proof["descendant_path"], json!([]));
    for selected in [
        None,
        Some(
            "pipeline-static:sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        ),
    ] {
        let mut payload = resume_payload("continue");
        if let Some(id) = selected {
            payload.meta.insert(
                "pipeline_static_resume_v1".to_owned(),
                json!({"revision": 1, "pause_id": id}),
            );
        }
        let result = PipelineTextContinuation::from_payload(&payload)
            .unwrap()
            .resolve(
                session.as_ref(),
                checkpointer.as_ref(),
                ROOT,
                THREAD,
                &definition.printer_pause_catalog(),
                &definition.static_pause_catalog(),
                "continue",
            )
            .await;
        assert_eq!(
            result.err().unwrap().code(),
            PipelineResumeErrorCode::StaleDecision
        );
    }
    assert_eq!(
        checkpointer.load(THREAD).await.unwrap().unwrap().state["count"],
        json!(0)
    );
}
