use super::*;
use adk_rust::agent::LlmAgentBuilder;
use adk_rust::futures::StreamExt as _;
use adk_rust::session::{CreateRequest, InMemorySessionService, Session, SessionService};
use adk_rust::{Tool, ToolContext};
use serde_json::Value;
use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

use crate::agents::context_budget::{
    ModelRequestBudget, RequestContextBudget, RequestContextUsage,
};
use crate::agents::context_management::ContextCompactionPlan;
use crate::agents::model_scope::{ModelScopeBackend, ModelScopeSessions};
use crate::agents::request::ModelContextLimits;

struct Budget;
impl ModelRequestBudget for Budget {
    fn measure(&self, request: &LlmRequest) -> adk_rust::Result<RequestContextUsage> {
        RequestContextBudget::resolve(
            Some(ModelContextLimits {
                context_window_tokens: 8000,
                max_output_tokens: 1000,
                context_window_fallback: false,
                max_output_fallback: false,
                max_input_tokens: None,
            }),
            &serde_json::Map::new(),
            Some(1000),
        )
        .unwrap()
        .unwrap()
        .measure_provider_request(&serde_json::to_vec(request).unwrap(), 1_048_576)
    }
}

fn scoped_runner(
    storage: &ModelScopeSessions,
    probe: Arc<Probe>,
    sessions: Arc<dyn SessionService>,
) -> adk_rust::runner::Runner {
    let scope = storage.checkpoint(
        Some(ContextCompactionPlan {
            max_context_tokens: 8000,
            preserve_recent_messages: 2,
            preserve_system_messages: true,
            summary_instructions: "Preserve delegated work. {messages}".into(),
        }),
        Some(Arc::new(Budget)),
        probe.clone(),
        None,
        None,
    );
    let model = scope.clone().delegation_model(probe.clone());
    let agent = scope
        .clone()
        .bind(LlmAgentBuilder::new("parent").model(model).tool(probe))
        .build()
        .unwrap();
    adk_rust::runner::Runner::builder()
        .app_name("delegation-test")
        .agent(scope.wrap(Arc::new(agent)))
        .session_service(sessions)
        .build()
        .unwrap()
}

struct Probe {
    models: AtomicUsize,
    calls: AtomicUsize,
    block: AtomicBool,
    entered: tokio::sync::Notify,
}

fn calls() -> Content {
    Content {
        role: "model".into(),
        parts: vec![Part::FunctionCall {
            name: "saved_agent".into(),
            args: json!({"task":"Inspect Cedar"}),
            id: Some("stable-child-call".into()),
            thought_signature: None,
        }],
    }
}

#[async_trait]
impl Llm for Probe {
    fn name(&self) -> &'static str {
        "fixture"
    }
    async fn generate_content(
        &self,
        request: LlmRequest,
        _: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        let count = self.models.fetch_add(1, Ordering::SeqCst);
        let content = if count == 0 {
            calls()
        } else {
            assert_eq!(count, 1, "recovery must not select another child");
            assert!(
                !serde_json::to_string(&request)
                    .unwrap()
                    .contains("RECOVERY_INPUT")
            );
            let results = request.contents.iter().flat_map(|content| &content.parts).filter(|part| {
                matches!(part, Part::FunctionResponse { id: Some(id), .. } if id == "stable-child-call")
            }).count();
            assert_eq!(results, 1);
            Content::new("model").with_text("Parent received the child result")
        };
        Ok(Box::pin(adk_rust::futures::stream::once(async {
            Ok(LlmResponse::new(content))
        })))
    }
}

#[async_trait]
impl Tool for Probe {
    fn name(&self) -> &'static str {
        "saved_agent"
    }
    fn description(&self) -> &'static str {
        "Saved child fixture"
    }
    async fn execute(&self, ctx: Arc<dyn ToolContext>, args: Value) -> adk_rust::Result<Value> {
        assert_eq!(ctx.function_call_id(), "stable-child-call");
        assert_eq!(args, json!({"task":"Inspect Cedar"}));
        self.calls.fetch_add(1, Ordering::SeqCst);
        self.entered.notify_one();
        if self.block.load(Ordering::SeqCst) {
            std::future::pending::<()>().await;
        }
        Ok(json!({"response":"Saved child result"}))
    }
}

async fn session(sessions: &dyn SessionService) -> Box<dyn Session> {
    sessions
        .get(GetRequest {
            app_name: "delegation-test".into(),
            user_id: "user".into(),
            session_id: "parent".into(),
            num_recent_events: None,
            after: None,
        })
        .await
        .unwrap()
}

fn runner(
    writer: ModelCheckpointWriter,
    probe: Arc<Probe>,
    sessions: Arc<dyn SessionService>,
) -> adk_rust::runner::Runner {
    let model = writer.delegation_model(probe.clone());
    let agent = writer
        .bind(LlmAgentBuilder::new("parent").model(model).tool(probe))
        .build()
        .unwrap();
    adk_rust::runner::Runner::builder()
        .app_name("delegation-test")
        .agent(Arc::new(agent))
        .session_service(sessions)
        .build()
        .unwrap()
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One interruption story checks authority and exact call recovery.
async fn interrupted_parent_replays_exact_saved_child_call_before_requesting_final_answer() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "delegation-test".into(),
            user_id: "user".into(),
            session_id: Some("parent".into()),
            state: HashMap::new(),
        })
        .await
        .unwrap();
    let probe = Arc::new(Probe {
        models: AtomicUsize::new(0),
        calls: AtomicUsize::new(0),
        block: AtomicBool::new(true),
        entered: tokio::sync::Notify::new(),
    });
    let writer = ModelCheckpointWriter::new(sessions.clone(), "execution".into(), 1, [1; 32])
        .with_application_tools(["saved_agent"].into_iter());
    let first = runner(writer.clone(), probe.clone(), sessions.clone());
    let task = tokio::spawn(async move {
        let mut events = first
            .run(
                "user".try_into().unwrap(),
                "parent".try_into().unwrap(),
                Content::new("user").with_text("Delegate the inspection"),
            )
            .await
            .unwrap();
        while let Some(event) = events.next().await {
            event.unwrap();
        }
    });
    tokio::time::timeout(std::time::Duration::from_secs(5), probe.entered.notified())
        .await
        .unwrap();
    task.abort();
    assert!(task.await.unwrap_err().is_cancelled());
    let stored = session(sessions.as_ref()).await;
    assert_eq!(
        stored.state().get(CHECKPOINT_KEY).unwrap()["phase"],
        "delegation_pending"
    );
    let value = stored.state().get(CHECKPOINT_KEY).unwrap();
    let snapshot: Checkpoint = serde_json::from_value(value).unwrap();
    let mut saved = snapshot.model.unwrap();
    saved.request.tools = saved.tools;
    let removed = writer
        .clone()
        .with_application_tools(std::iter::empty())
        .restore(stored.as_ref())
        .unwrap();
    assert!(
        removed
            .before_model(
                stored.try_identity().unwrap(),
                "replacement",
                "parent",
                saved.request.clone()
            )
            .await
            .is_err()
    );
    let changed = writer.clone().restore(stored.as_ref()).unwrap();
    let mut changed_request = saved.request;
    changed_request.tools.clear();
    assert!(
        changed
            .before_model(
                stored.try_identity().unwrap(),
                "replacement",
                "parent",
                changed_request
            )
            .await
            .is_err()
    );
    let restored = writer.restore(stored.as_ref()).unwrap();
    assert!(restored.validated_checkpoint().is_some());
    probe.block.store(false, Ordering::SeqCst);
    let replacement = runner(restored, probe.clone(), sessions);
    let mut events = replacement
        .run(
            "user".try_into().unwrap(),
            "parent".try_into().unwrap(),
            Content::new("user").with_text("RECOVERY_INPUT"),
        )
        .await
        .unwrap();
    let mut final_text = false;
    while let Some(event) = events.next().await {
        final_text |= serde_json::to_string(&event.unwrap())
            .unwrap()
            .contains("Parent received the child result");
    }
    assert!(final_text);
    assert_eq!(probe.models.load(Ordering::SeqCst), 2);
    assert_eq!(
        probe.calls.load(Ordering::SeqCst),
        2,
        "dispatch resumes the same child identity"
    );
}

#[test]
fn delegation_validation_rejects_mixed_tools_and_duplicate_call_ids() {
    let mut content = calls();
    assert!(validate_calls(&content, |name| name == "saved_agent").is_ok());
    content.parts.push(content.parts[0].clone());
    assert!(validate_calls(&content, |_| true).is_err());
    assert!(validate_calls(&calls(), |_| false).is_err());
    let mut mixed = calls();
    mixed.parts.push(Part::FunctionCall {
        name: "ordinary_effect".into(),
        args: json!({}),
        id: Some("different-call".into()),
        thought_signature: None,
    });
    assert!(validate_calls(&mixed, |name| name == "saved_agent").is_err());
}

#[tokio::test]
async fn scoped_child_recovers_its_own_pending_delegation() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "delegation-test".into(),
            user_id: "user".into(),
            session_id: Some("parent".into()),
            state: HashMap::new(),
        })
        .await
        .unwrap();
    let storage = ModelScopeSessions::new(
        ModelScopeBackend::Local(sessions.clone()),
        "execution".into(),
        1,
        [1; 32],
    )
    .with_application_tools(["saved_agent"].into_iter());
    let probe = Arc::new(Probe {
        models: AtomicUsize::new(0),
        calls: AtomicUsize::new(0),
        block: AtomicBool::new(true),
        entered: tokio::sync::Notify::new(),
    });
    let first = scoped_runner(&storage, probe.clone(), sessions.clone());
    let task = tokio::spawn(async move {
        let mut events = first
            .run(
                "user".try_into().unwrap(),
                "parent".try_into().unwrap(),
                Content::new("user").with_text("Delegate the inspection"),
            )
            .await
            .unwrap();
        while let Some(event) = events.next().await {
            event.unwrap();
        }
    });
    tokio::time::timeout(std::time::Duration::from_secs(5), probe.entered.notified())
        .await
        .unwrap();
    task.abort();
    assert!(task.await.unwrap_err().is_cancelled());
    probe.block.store(false, Ordering::SeqCst);
    let replacement = scoped_runner(
        &storage.with_pending_model_recovery(),
        probe.clone(),
        sessions,
    );
    let mut events = replacement
        .run(
            "user".try_into().unwrap(),
            "parent".try_into().unwrap(),
            Content::new("user").with_text("RECOVERY_INPUT"),
        )
        .await
        .unwrap();
    let mut final_text = false;
    while let Some(event) = events.next().await {
        final_text |= serde_json::to_string(&event.unwrap())
            .unwrap()
            .contains("Parent received the child result");
    }
    assert!(final_text);
    assert_eq!(probe.models.load(Ordering::SeqCst), 2);
    assert_eq!(probe.calls.load(Ordering::SeqCst), 2);
}

/// The probe offered as a read-only tool of a toolset (D2 records it there).
struct ReadOnlyProbe(Arc<Probe>);

#[async_trait]
impl Tool for ReadOnlyProbe {
    fn name(&self) -> &'static str {
        "saved_agent"
    }
    fn description(&self) -> &'static str {
        "Read-only lookup fixture"
    }
    fn is_read_only(&self) -> bool {
        true
    }
    async fn execute(&self, ctx: Arc<dyn ToolContext>, args: Value) -> adk_rust::Result<Value> {
        self.0.execute(ctx, args).await
    }
}

fn read_only_runner(
    storage: &ModelScopeSessions,
    probe: Arc<Probe>,
    sessions: Arc<dyn SessionService>,
    guarded: bool,
) -> adk_rust::runner::Runner {
    let scope = storage.checkpoint(None, Some(Arc::new(Budget)), probe.clone(), None, None);
    let toolset: Arc<dyn adk_rust::Toolset> = Arc::new(adk_rust::tool::BasicToolset::new(
        "fixture",
        vec![Arc::new(ReadOnlyProbe(probe.clone())) as Arc<dyn Tool>],
    ));
    let guarded = if guarded {
        std::collections::HashSet::from(["saved_agent".to_owned()])
    } else {
        std::collections::HashSet::new()
    };
    let mut builder = LlmAgentBuilder::new("parent").model(scope.clone().delegation_model(probe));
    for toolset in scope.record_read_only(vec![toolset], &guarded) {
        builder = builder.toolset(toolset);
    }
    let agent = scope.clone().bind(builder).build().unwrap();
    adk_rust::runner::Runner::builder()
        .app_name("delegation-test")
        .agent(scope.wrap(Arc::new(agent)))
        .session_service(sessions)
        .build()
        .unwrap()
}

/// Interrupt a fan-out child (`with_read_only_replay`) inside its read-only
/// tool, then run its replacement; `guarded` puts the tool behind a confirmation.
async fn interrupted_read_only_child(
    guarded: bool,
) -> (Arc<Probe>, Vec<adk_rust::Result<adk_rust::Event>>) {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "delegation-test".into(),
            user_id: "user".into(),
            session_id: Some("parent".into()),
            state: HashMap::new(),
        })
        .await
        .unwrap();
    let storage = ModelScopeSessions::new(
        ModelScopeBackend::Local(sessions.clone()),
        "execution".into(),
        1,
        [1; 32],
    )
    .with_read_only_replay();
    let probe = Arc::new(Probe {
        models: AtomicUsize::new(0),
        calls: AtomicUsize::new(0),
        block: AtomicBool::new(true),
        entered: tokio::sync::Notify::new(),
    });
    let first = read_only_runner(&storage, probe.clone(), sessions.clone(), guarded);
    let task = tokio::spawn(async move {
        let mut events = first
            .run(
                "user".try_into().unwrap(),
                "parent".try_into().unwrap(),
                Content::new("user").with_text("Look it up"),
            )
            .await
            .unwrap();
        while let Some(event) = events.next().await {
            event.unwrap();
        }
    });
    tokio::time::timeout(std::time::Duration::from_secs(5), probe.entered.notified())
        .await
        .unwrap();
    task.abort();
    assert!(task.await.unwrap_err().is_cancelled());
    probe.block.store(false, Ordering::SeqCst);
    let replacement = read_only_runner(
        &storage.with_pending_model_recovery(),
        probe.clone(),
        sessions,
        guarded,
    );
    let events = replacement
        .run(
            "user".try_into().unwrap(),
            "parent".try_into().unwrap(),
            Content::new("user").with_text("RECOVERY_INPUT"),
        )
        .await
        .unwrap()
        .collect()
        .await;
    (probe, events)
}

#[tokio::test]
async fn a_fanout_childs_interrupted_read_only_tool_runs_again() {
    let (probe, events) = interrupted_read_only_child(false).await;
    let text = events
        .into_iter()
        .map(|event| serde_json::to_string(&event.unwrap()).unwrap())
        .collect::<String>();
    assert!(text.contains("Parent received the child result"));
    // The saved call batch is replayed without a model call; the tool runs again.
    assert_eq!(probe.models.load(Ordering::SeqCst), 2);
    assert_eq!(probe.calls.load(Ordering::SeqCst), 2);
}

#[tokio::test]
async fn a_guarded_read_only_tool_in_flight_is_refused_as_may_have_started() {
    let (probe, events) = interrupted_read_only_child(true).await;
    let error = events
        .into_iter()
        .find_map(Result::err)
        .expect("the replacement refuses");
    assert_eq!(
        error.code,
        crate::protocol::output::RECOVERY_REFUSED_UPSTREAM_CODE
    );
    assert_eq!(probe.models.load(Ordering::SeqCst), 1);
    assert_eq!(probe.calls.load(Ordering::SeqCst), 1);
}

/// FU-291 C4: the Phase and Checkpoint decode of the Worker before FU-291
/// (`e77b467fd`, `model_checkpoint.rs:20-49`), copied verbatim. An older
/// Worker reads a stored record with exactly this strict decode in
/// `restore_validated` (`model_checkpoint.rs:331-332` there), reached from the
/// member's model scope (`model_scope.rs:466-470` there) before any model call
/// or tool dispatch.
mod pre_fu291 {
    use crate::agents::model_checkpoint::output;
    use adk_rust::LlmRequest;
    use serde::Deserialize;
    use serde_json::Value;
    use std::collections::HashMap;

    #[derive(Clone, Copy, Deserialize)]
    #[serde(rename_all = "snake_case")]
    #[allow(dead_code)] // The variants are the decode contract.
    pub(super) enum Phase {
        ContextPending,
        ModelPending,
        OutputFailed,
        ToolMayHaveStarted,
        DelegationPending,
    }

    #[derive(Deserialize)]
    #[serde(deny_unknown_fields)]
    #[allow(dead_code)] // The fields are the decode contract.
    pub(super) struct Checkpoint {
        version: u8,
        execution_id: String,
        generation: u64,
        definition_digest: [u8; 32],
        invocation_id: String,
        phase: Phase,
        model: Option<ModelSnapshot>,
        #[serde(default)]
        delegation: Option<adk_rust::Content>,
        #[serde(default)]
        output_continuation: Option<output::OutputContinuation>,
        #[serde(default)]
        steering_ids: Vec<String>,
    }

    #[derive(Deserialize)]
    #[serde(deny_unknown_fields)]
    #[allow(dead_code)] // The fields are the decode contract.
    struct ModelSnapshot {
        request: LlmRequest,
        tools: HashMap<String, Value>,
    }
}

/// Interrupts a batch of `saved_agent` (here the durable pipeline tool)
/// inside its tool and returns the stored session.
async fn interrupted_durable_batch(
    sessions: Arc<dyn SessionService>,
    writer: &ModelCheckpointWriter,
    probe: &Arc<Probe>,
) -> Box<dyn Session> {
    sessions
        .create(CreateRequest {
            app_name: "delegation-test".into(),
            user_id: "user".into(),
            session_id: Some("parent".into()),
            state: HashMap::new(),
        })
        .await
        .unwrap();
    let first = runner(writer.clone(), probe.clone(), sessions.clone());
    let task = tokio::spawn(async move {
        let mut events = first
            .run(
                "user".try_into().unwrap(),
                "parent".try_into().unwrap(),
                Content::new("user").with_text("Delegate the inspection"),
            )
            .await
            .unwrap();
        while let Some(event) = events.next().await {
            event.unwrap();
        }
    });
    tokio::time::timeout(std::time::Duration::from_secs(5), probe.entered.notified())
        .await
        .unwrap();
    task.abort();
    assert!(task.await.unwrap_err().is_cancelled());
    session(sessions.as_ref()).await
}

fn blocking_probe() -> Arc<Probe> {
    Arc::new(Probe {
        models: AtomicUsize::new(0),
        calls: AtomicUsize::new(0),
        block: AtomicBool::new(true),
        entered: tokio::sync::Notify::new(),
    })
}

/// C4: a batch holding a durable pipeline call is recorded as
/// `durable_delegation_pending`, and a Worker that binds the call durably
/// replays the stored batch with no new model completion (C2).
#[tokio::test]
async fn a_durable_pipeline_batch_is_recorded_under_its_own_phase_and_replayed() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let probe = blocking_probe();
    let writer = ModelCheckpointWriter::new(sessions.clone(), "execution".into(), 1, [1; 32])
        .with_durable_pipeline_tools(["saved_agent"].into_iter());
    let stored = interrupted_durable_batch(sessions.clone(), &writer, &probe).await;
    assert_eq!(
        stored.state().get(CHECKPOINT_KEY).unwrap()["phase"],
        "durable_delegation_pending"
    );
    let restored = writer.restore(stored.as_ref()).unwrap();
    probe.block.store(false, Ordering::SeqCst);
    let mut events = runner(restored, probe.clone(), sessions)
        .run(
            "user".try_into().unwrap(),
            "parent".try_into().unwrap(),
            Content::new("user").with_text("RECOVERY_INPUT"),
        )
        .await
        .unwrap();
    while let Some(event) = events.next().await {
        event.unwrap();
    }
    // The first completion chose the call; the replay re-emitted it; the
    // second completion is the final answer after the tool result.
    assert_eq!(probe.models.load(Ordering::SeqCst), 2);
    assert_eq!(
        probe.calls.load(Ordering::SeqCst),
        2,
        "the same call, once more"
    );
}

/// C4, fail closed on this Worker too: without the durable binding for the
/// call (it would run on a memory checkpointer), the record is refused.
#[tokio::test]
async fn a_durable_delegation_record_without_its_durable_binding_is_refused() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let probe = blocking_probe();
    let writer = ModelCheckpointWriter::new(sessions.clone(), "execution".into(), 1, [1; 32])
        .with_durable_pipeline_tools(["saved_agent"].into_iter());
    let stored = interrupted_durable_batch(sessions.clone(), &writer, &probe).await;
    let unbound = ModelCheckpointWriter::new(sessions, "execution".into(), 1, [1; 32])
        .with_application_tools(["saved_agent"].into_iter());
    let Err(error) = unbound.restore(stored.as_ref()) else {
        panic!("a durable record restored without its durable binding");
    };
    assert_eq!(error.code, "agent_recovery.invalid_checkpoint");
}

/// C4, rollback: the record decoded through the pre-FU-291 strict decode is a
/// typed failure, so an older Worker's restore refuses it
/// (`agent_recovery.invalid_checkpoint`) before any model call or tool
/// dispatch: it resumes nothing and never re-runs the pipeline. The same
/// record with the pre-FU-291 phase and without its batch id (C2) decodes, so
/// only those two refuse it, each on its own.
#[tokio::test]
async fn an_older_worker_refuses_a_durable_delegation_record_typed() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let probe = blocking_probe();
    let writer = ModelCheckpointWriter::new(sessions.clone(), "execution".into(), 1, [1; 32])
        .with_durable_pipeline_tools(["saved_agent"].into_iter());
    let stored = interrupted_durable_batch(sessions, &writer, &probe).await;
    let mut record = stored.state().get(CHECKPOINT_KEY).unwrap();
    assert!(serde_json::from_value::<pre_fu291::Checkpoint>(record.clone()).is_err());
    let batch = record
        .as_object_mut()
        .unwrap()
        .remove("batch_event_id")
        .expect("a durable record names its batch");
    let error = serde_json::from_value::<pre_fu291::Checkpoint>(record.clone())
        .err()
        .expect("the older decode refuses the durable phase");
    assert!(
        error.to_string().contains("durable_delegation_pending"),
        "{error}"
    );
    record["phase"] = json!("delegation_pending");
    assert!(serde_json::from_value::<pre_fu291::Checkpoint>(record.clone()).is_ok());
    record["batch_event_id"] = batch;
    let error = serde_json::from_value::<pre_fu291::Checkpoint>(record)
        .err()
        .expect("the older decode refuses the batch id");
    assert!(error.to_string().contains("batch_event_id"), "{error}");
    assert_eq!(
        probe.calls.load(Ordering::SeqCst),
        1,
        "only the original dispatch"
    );
}

/// FU-291 C2: a durable pipeline batch replayed after a crash is re-emitted
/// from the stored record, with no new model completion, under its original
/// model event: every call keeps its lineage (batch, ordinal, call id, name,
/// arguments), so the nested thread derived from it is the same.
#[tokio::test]
async fn a_replayed_durable_batch_keeps_its_original_event_and_call_lineage() {
    use crate::agents::application_pipeline::PipelineToolCallLineage;
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let probe = blocking_probe();
    let writer = ModelCheckpointWriter::new(sessions.clone(), "execution".into(), 1, [1; 32])
        .with_durable_pipeline_tools(["saved_agent"].into_iter());
    let stored = interrupted_durable_batch(sessions.clone(), &writer, &probe).await;
    let batches = |session: &dyn Session| {
        session
            .events()
            .all()
            .into_iter()
            .filter(|event| !event.tool_calls().is_empty())
            .collect::<Vec<_>>()
    };
    let original = batches(stored.as_ref());
    assert_eq!(original.len(), 1);
    assert_eq!(
        stored.state().get(CHECKPOINT_KEY).unwrap()["batch_event_id"],
        json!(original[0].id)
    );
    let restored = writer.restore(stored.as_ref()).unwrap();
    probe.block.store(false, Ordering::SeqCst);
    let mut events = runner(restored, probe.clone(), sessions.clone())
        .run(
            "user".try_into().unwrap(),
            "parent".try_into().unwrap(),
            Content::new("user").with_text("RECOVERY_INPUT"),
        )
        .await
        .unwrap();
    while let Some(event) = events.next().await {
        event.unwrap();
    }
    assert_eq!(
        probe.models.load(Ordering::SeqCst),
        2,
        "no new completion chose the call"
    );
    let replayed = batches(session(sessions.as_ref()).await.as_ref());
    assert_eq!(replayed.len(), 2);
    assert_ne!(replayed[1].id, original[0].id, "ADK mints a new event");
    let lineage = |event: &Event| {
        serde_json::to_value(PipelineToolCallLineage::from_call(event, 0).ok().unwrap()).unwrap()
    };
    assert_eq!(
        lineage(&replayed[1]),
        lineage(&original[0]),
        "the replayed call keeps its lineage"
    );
}
