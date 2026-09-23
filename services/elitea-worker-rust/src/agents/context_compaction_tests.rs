use super::*;
use adk_rust::futures::stream;
use adk_rust::session::{
    AppendEventRequest, CreateRequest, DeleteRequest, GetRequest, InMemorySessionService,
    ListRequest, SessionService,
};
use adk_rust::{BeforeModelResult, LlmResponse, LlmResponseStream};
use async_trait::async_trait;
use serde_json::json;
use std::sync::atomic::{AtomicUsize, Ordering};

use crate::agents::context_budget::{RequestContextBudget, RequestContextUsage};
use crate::agents::context_status::ContextPhase;
use crate::agents::model_checkpoint::{CHECKPOINT_KEY, ModelCheckpointWriter};
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

#[derive(Default)]
enum ReferencePromotion {
    #[default]
    Never,
    Always,
    UntilCorrection,
}

#[derive(Default)]
struct Summary {
    requests: Mutex<Vec<LlmRequest>>,
    fail: bool,
    invalid_candidates: usize,
    input_byte_limit: Option<usize>,
    reference_promotion: ReferencePromotion,
    repair_reference_then_links: bool,
    pause: Option<Arc<tokio::sync::Notify>>,
}
#[async_trait]
impl Llm for Summary {
    fn name(&self) -> &'static str {
        "fixture"
    }
    async fn generate_content(
        &self,
        request: LlmRequest,
        streaming: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        assert!(!streaming);
        let is_correction = serde_json::to_string(&request)
            .unwrap()
            .contains("invalid_reference_values");
        let promote_label = match self.reference_promotion {
            ReferencePromotion::Never => false,
            ReferencePromotion::Always => true,
            ReferencePromotion::UntilCorrection => !is_correction,
        } && serde_json::to_string(&request)
            .unwrap()
            .contains("Validated earlier source summary");
        let exceeds_capacity = self
            .input_byte_limit
            .is_some_and(|limit| serde_json::to_vec(&request).unwrap().len() > limit);
        let call = {
            let mut requests = self.requests.lock().unwrap();
            requests.push(request);
            requests.len()
        };
        if exceeds_capacity {
            return Err(AdkError::new(
                ErrorComponent::Model,
                ErrorCategory::InvalidInput,
                "context_budget_exceeded",
                "Fixture summary input capacity.",
            ));
        }
        if self.repair_reference_then_links && call <= 2 {
            let mut candidate: Value =
                serde_json::from_str(&crate::agents::context_summary::fixture()).unwrap();
            if call == 1 {
                candidate["references"][0]["value"] = json!("invented-reference");
            }
            candidate["completed_work"][0]["evidence_refs"] = json!(["invented-reference"]);
            return Ok(Box::pin(stream::once(async move {
                Ok(LlmResponse::new(
                    Content::new("model").with_text(candidate.to_string()),
                ))
            })));
        }
        if call <= self.invalid_candidates {
            let mut candidate: Value =
                serde_json::from_str(&crate::agents::context_summary::fixture()).unwrap();
            candidate["completed_work"][0]["evidence_refs"] = json!(["invented-reference"]);
            if call == 2 {
                // The correction must not turn its own rejected candidate
                // into a source for invented reference values.
                candidate["references"] =
                    json!([{"label":"claimed evidence", "value":"invented-reference"}]);
            }
            return Ok(Box::pin(stream::once(async move {
                Ok(LlmResponse::new(
                    Content::new("model").with_text(candidate.to_string()),
                ))
            })));
        }
        if let Some(pause) = &self.pause {
            pause.notified().await;
        }
        if self.fail {
            return Err(AdkError::new(
                ErrorComponent::Model,
                ErrorCategory::Unavailable,
                "fixture_summary_interrupted",
                "Simulated interruption during summarization.",
            ));
        }
        if promote_label {
            let mut candidate: Value =
                serde_json::from_str(&crate::agents::context_summary::fixture()).unwrap();
            candidate["references"][0]["value"] = json!("Verified lookup");
            candidate["completed_work"][0]["evidence_refs"] = json!(["Verified lookup"]);
            return Ok(Box::pin(stream::once(async move {
                Ok(LlmResponse::new(
                    Content::new("model").with_text(candidate.to_string()),
                ))
            })));
        }
        Ok(Box::pin(stream::once(async {
            // Compatible providers can add prose and fences despite the prompt.
            // Exercise extraction through preparation, persistence, and recovery.
            let summary = format!(
                "Here is the continuation record:\n```json\n{}\n```",
                crate::agents::context_summary::fixture()
            );
            Ok(LlmResponse::new(Content::new("model").with_text(summary)))
        })))
    }
}

fn load_request() -> GetRequest {
    GetRequest {
        app_name: "elitea-agent-v1".into(),
        user_id: "user-1".into(),
        session_id: "session-1".into(),
        num_recent_events: None,
        after: None,
    }
}

async fn create(sessions: &dyn SessionService) -> Box<dyn Session> {
    sessions
        .create(CreateRequest {
            app_name: "elitea-agent-v1".into(),
            user_id: "user-1".into(),
            session_id: Some("session-1".into()),
            state: std::collections::HashMap::default(),
        })
        .await
        .unwrap()
}

fn plan() -> ContextCompactionPlan {
    ContextCompactionPlan {
        max_context_tokens: 8000,
        preserve_recent_messages: 2,
        preserve_system_messages: true,
        summary_instructions: "Preserve work and corrections. {messages}".into(),
    }
}

async fn checkpoint_ready(_: RequestContextUsage) -> adk_rust::Result<()> {
    Ok(())
}

fn history() -> LlmRequest {
    serde_json::from_value(json!({"model":"fixture", "contents": [
        {"role":"system", "parts":[{"text":"EXACT_SKILL_REVISION_AND_PROJECT_CONTEXT"}]},
        {"role":"user", "parts":[{"text":"Original task, preserve this exactly."}]},
        {"role":"model", "parts":[{"text":"earlier draft ".repeat(1400)}]},
        {"role":"user", "parts":[{"text":"Latest correction: use project two."}]},
        {"role":"model", "parts":[{"name":"lookup", "id":"call-one", "args":{"project_id":2}}]},
        {"role":"function", "parts":[{"id":"call-one", "functionResponse":{"name":"lookup", "response":{"status":"ok","output":"evidence ".repeat(900)}}}]},
        {"role":"model", "parts":[{"text":"Use the confirmed evidence."}]},
        {"role":"model", "parts":[{"name":"lookup", "id":"call-two", "args":{"project_id":2}}]},
        {"role":"function", "parts":[{"id":"call-two", "functionResponse":{"name":"lookup", "response":{"status":"failed","error":"not found"}}}]}
    ]})).unwrap()
}

async fn store(sessions: &dyn SessionService, record: Option<&CompactionRecord>) {
    let session = sessions.get(load_request()).await.unwrap();
    let mut event = Event::new("persist-summary");
    event.actions.state_delta.insert(
        STATE_KEY.into(),
        DurableContextCompaction::state_value(record).unwrap(),
    );
    sessions
        .append_event_for_identity(AppendEventRequest {
            identity: session.try_identity().unwrap(),
            event,
        })
        .await
        .unwrap();
}

#[tokio::test]
async fn repeated_compaction_reuses_exact_coverage_and_preserves_tool_groups_and_authority() {
    let sessions = InMemorySessionService::new();
    let session = create(&sessions).await;
    let summary = Arc::new(Summary::default());
    let compaction = DurableContextCompaction::new(
        plan(),
        Arc::new(Budget),
        summary.clone(),
        [7; 32],
        session.as_ref(),
    )
    .unwrap();
    let original = history();
    let (prepared, record) = compaction
        .prepare(original.clone(), checkpoint_ready)
        .await
        .unwrap();
    let usage = Budget.measure(&prepared).unwrap();
    assert!(usage.estimated_input <= usage.budget.compaction_target());
    assert_eq!(prepared.contents[0].parts, original.contents[0].parts);
    assert_eq!(prepared.contents[1].parts, original.contents[1].parts);
    assert_eq!(prepared.contents[3].parts, original.contents[3].parts);
    assert_eq!(
        prepared.contents[prepared.contents.len() - 2].parts,
        original.contents[7].parts
    );
    assert_eq!(
        prepared.contents.last().unwrap().parts,
        original.contents[8].parts
    );
    let summary_input = serde_json::to_string(&summary.requests.lock().unwrap()[0]).unwrap();
    assert!(summary_input.contains("call-one"));
    assert!(summary_input.contains("evidence"));
    assert!(summary_input.contains("Original task, preserve this exactly."));
    assert!(!summary_input.contains("call-two"));
    assert!(!summary_input.contains("EXACT_SKILL_REVISION"));
    store(&sessions, record.as_ref()).await;
    compaction.committed(record).unwrap();
    let session = sessions.get(load_request()).await.unwrap();
    let restored_summary = Arc::new(Summary::default());
    let restored = DurableContextCompaction::new(
        plan(),
        Arc::new(Budget),
        restored_summary.clone(),
        [7; 32],
        session.as_ref(),
    )
    .unwrap();
    for request in [original.clone(), prepared.clone()] {
        let (after, _) = restored.prepare(request, checkpoint_ready).await.unwrap();
        assert_eq!(
            serde_json::to_value(after).unwrap(),
            serde_json::to_value(&prepared).unwrap()
        );
    }
    assert!(restored_summary.requests.lock().unwrap().is_empty());
    let mut extended = original.clone();
    extended
        .contents
        .push(Content::new("model").with_text("new work ".repeat(3100)));
    extended
        .contents
        .push(Content::new("user").with_text("Next correction: retain the failed lookup."));
    extended
        .contents
        .push(Content::new("model").with_text("review"));
    extended
        .contents
        .push(Content::new("user").with_text("continue"));
    let (twice, record) = restored
        .prepare(extended.clone(), checkpoint_ready)
        .await
        .unwrap();
    restored.committed(record).unwrap();
    let (repeated, _) = restored.prepare(extended, checkpoint_ready).await.unwrap();
    assert_eq!(
        serde_json::to_value(twice).unwrap(),
        serde_json::to_value(repeated).unwrap()
    );
    assert_eq!(restored_summary.requests.lock().unwrap().len(), 1);
    let mut edited = original;
    edited.contents[2] = Content::new("model").with_text("edited history ".repeat(1400));
    restored.prepare(edited, checkpoint_ready).await.unwrap();
    assert_eq!(restored_summary.requests.lock().unwrap().len(), 2);
}

#[test]
fn cutoff_never_splits_a_tool_group() {
    let history = history();
    let working = &history.contents[2..];
    assert_eq!(complete_prefix(working, 4).unwrap(), 2);
    assert_eq!(complete_prefix(working, 1).unwrap(), 5);
    assert!(complete_prefix(&working[3..], 1).is_err());
}

#[test]
fn coverage_digest_is_stable_across_json_object_key_order() {
    let left: Content = serde_json::from_str(r#"{"role":"model","parts":[{"name":"lookup","id":"one","args":{"z":1,"a":{"y":2,"b":3}}}]}"#).unwrap();
    let right: Content = serde_json::from_str(r#"{"role":"model","parts":[{"name":"lookup","id":"one","args":{"a":{"b":3,"y":2},"z":1}}]}"#).unwrap();
    assert_eq!(
        extend_digest([0; 32], &[left]).unwrap(),
        extend_digest([0; 32], &[right]).unwrap()
    );
}

#[tokio::test]
async fn oversized_protected_input_fails_without_spending_a_summary_call() {
    let sessions = InMemorySessionService::new();
    let session = create(&sessions).await;
    let summary = Arc::new(Summary::default());
    let compaction = DurableContextCompaction::new(
        plan(),
        Arc::new(Budget),
        summary.clone(),
        [7; 32],
        session.as_ref(),
    )
    .unwrap();
    let mut request = history();
    request.contents[0] = Content::new("system").with_text("protected ".repeat(4000));
    let result = compaction
        .prepare(request, |_| async {
            panic!("must fail before summary preparation")
        })
        .await;
    assert!(matches!(result, Err(error) if error.code == "context_budget_exceeded"));
    assert!(summary.requests.lock().unwrap().is_empty());
}

#[tokio::test]
async fn repeated_tool_loop_compaction_does_not_pin_a_previous_summary_as_user_input() {
    let sessions = InMemorySessionService::new();
    let session = create(&sessions).await;
    let summary = Arc::new(Summary::default());
    let compaction = DurableContextCompaction::new(
        plan(),
        Arc::new(Budget),
        summary.clone(),
        [7; 32],
        session.as_ref(),
    )
    .unwrap();
    let mut original = history();
    original.contents.remove(3);
    let (_, record) = compaction
        .prepare(original.clone(), checkpoint_ready)
        .await
        .unwrap();
    assert_eq!(record.as_ref().unwrap().replacement.len(), 1);
    compaction.committed(record).unwrap();
    for cycle in 1..10 {
        original
            .contents
            .push(Content::new("model").with_text("new work ".repeat(3100)));
        let mut pair = history().contents[7..].to_vec();
        for content in &mut pair {
            for part in &mut content.parts {
                match part {
                    Part::FunctionCall { id, .. } | Part::FunctionResponse { id, .. } => {
                        *id = Some(format!("call-cycle-{cycle}"));
                    }
                    _ => {}
                }
            }
        }
        original.contents.extend(pair);
        let (prepared, record) = compaction
            .prepare(original.clone(), checkpoint_ready)
            .await
            .unwrap();
        assert_eq!(record.as_ref().unwrap().replacement.len(), 1);
        assert_eq!(
            prepared
                .contents
                .iter()
                .filter(|content| content.role == "user")
                .count(),
            2
        );
        assert_eq!(prepared.contents[0].parts, original.contents[0].parts);
        compaction.committed(record).unwrap();
    }
    assert_eq!(summary.requests.lock().unwrap().len(), 10);
}

struct ModelAfterCheckpoint {
    sessions: Arc<dyn SessionService>,
    calls: AtomicUsize,
}
#[async_trait]
impl Llm for ModelAfterCheckpoint {
    fn name(&self) -> &'static str {
        "fixture"
    }
    async fn generate_content(
        &self,
        request: LlmRequest,
        _: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        let session = self.sessions.get(load_request()).await.unwrap();
        assert!(
            session
                .state()
                .get(STATE_KEY)
                .is_some_and(|value| !value.is_null())
        );
        let checkpoint = session.state().get(CHECKPOINT_KEY).unwrap();
        assert_eq!(
            checkpoint["model"]["request"],
            serde_json::to_value(&request).unwrap()
        );
        let encoded = serde_json::to_string(&request).unwrap();
        assert!(encoded.contains("EXACT_SKILL_REVISION_AND_PROJECT_CONTEXT"));
        assert!(encoded.contains("Original task, preserve this exactly."));
        assert!(!encoded.contains(&"earlier draft ".repeat(30)));
        Err(AdkError::new(
            ErrorComponent::Model,
            ErrorCategory::Unavailable,
            "fixture_after_checkpoint",
            "Simulated process loss before a model result.",
        ))
    }
}

async fn run(sessions: Arc<dyn SessionService>, recover: bool) -> (usize, usize, Vec<String>) {
    run_with_summary_failure(sessions, recover, false).await
}

#[tokio::test]
async fn compaction_progress_is_observable_while_the_summary_model_is_pending() {
    use crate::agents::context_status::ModelContextStatus;
    use crate::agents::graph::{PipelineNodeEventStreamingAgent, pipeline_node_event_channel};
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    seed(sessions.as_ref()).await;
    let stored = sessions.get(load_request()).await.unwrap();
    let release = Arc::new(tokio::sync::Notify::new());
    let summary = Arc::new(Summary {
        pause: Some(release.clone()),
        ..Summary::default()
    });
    let compaction = Arc::new(
        DurableContextCompaction::new(plan(), Arc::new(Budget), summary, [7; 32], stored.as_ref())
            .unwrap(),
    );
    let (sender, receiver) = pipeline_node_event_channel();
    let writer = ModelCheckpointWriter::new(sessions.clone(), "execution".into(), 7, [7; 32])
        .with_request_budget(Some(Arc::new(Budget)))
        .with_context_compaction(Some(compaction))
        .with_context_events(sender);
    let model = Arc::new(ModelAfterCheckpoint {
        sessions: sessions.clone(),
        calls: AtomicUsize::new(0),
    });
    let builder = adk_rust::agent::LlmAgentBuilder::new("agent")
        .model(model.clone())
        .before_model_callback(Box::new(|_, mut request| {
            Box::pin(async move {
                request.contents.insert(
                    0,
                    Content::new("system").with_text("EXACT_SKILL_REVISION_AND_PROJECT_CONTEXT"),
                );
                Ok(BeforeModelResult::Continue(request))
            })
        }));
    let runner = adk_rust::runner::Runner::builder()
        .app_name("elitea-agent-v1")
        .agent(Arc::new(PipelineNodeEventStreamingAgent::new(
            Arc::new(writer.bind(builder).build().unwrap()),
            receiver,
        )))
        .session_service(sessions.clone())
        .build()
        .unwrap();
    let mut events = runner
        .run(
            "user-1".try_into().unwrap(),
            "session-1".try_into().unwrap(),
            Content::new("user").with_text("Continue."),
        )
        .await
        .unwrap();
    let first = tokio::time::timeout(std::time::Duration::from_secs(2), events.next())
        .await
        .expect("status must arrive before summary completion")
        .unwrap()
        .unwrap();
    assert_eq!(
        ModelContextStatus::from_event(&first)
            .unwrap()
            .unwrap()
            .phase,
        ContextPhase::Compacting
    );
    let pending = sessions
        .get(load_request())
        .await
        .unwrap()
        .state()
        .get(CHECKPOINT_KEY)
        .unwrap();
    assert_eq!(pending["phase"], "context_pending");
    assert_eq!(model.calls.load(Ordering::SeqCst), 0);
    release.notify_one();
    let finished = events.next().await.unwrap().unwrap();
    assert_eq!(
        ModelContextStatus::from_event(&finished)
            .unwrap()
            .unwrap()
            .phase,
        ContextPhase::Compacted
    );
    assert_eq!(
        sessions
            .get(load_request())
            .await
            .unwrap()
            .state()
            .get(CHECKPOINT_KEY)
            .unwrap()["phase"],
        "model_pending"
    );
    assert_eq!(
        events.next().await.unwrap().unwrap_err().code,
        "fixture_after_checkpoint"
    );
    assert!(events.next().await.is_none());
}

async fn run_with_summary_failure(
    sessions: Arc<dyn SessionService>,
    recover: bool,
    fail_summary: bool,
) -> (usize, usize, Vec<String>) {
    let session = sessions.get(load_request()).await.unwrap();
    let summary = Arc::new(Summary {
        fail: fail_summary,
        ..Summary::default()
    });
    let compaction = Arc::new(
        DurableContextCompaction::new(
            plan(),
            Arc::new(Budget),
            summary.clone(),
            [7; 32],
            session.as_ref(),
        )
        .unwrap(),
    );
    let mut writer = ModelCheckpointWriter::new(sessions.clone(), "execution".into(), 7, [7; 32])
        .with_request_budget(Some(Arc::new(Budget)))
        .with_context_compaction(Some(compaction));
    let (context_events, context_receiver) = crate::agents::graph::pipeline_node_event_channel();
    writer = writer.with_context_events(context_events);
    if recover {
        // Production validates evidence before it can construct a summary model.
        let evidence = ModelCheckpointWriter::new(sessions.clone(), "execution".into(), 7, [7; 32])
            .inspect(session.as_ref(), true)
            .unwrap()
            .unwrap();
        writer = writer.restore(session.as_ref()).unwrap();
        assert!(
            writer
                .validated_checkpoint()
                .unwrap()
                .matches_checkpoint(&evidence)
        );
    }
    let model = Arc::new(ModelAfterCheckpoint {
        sessions: sessions.clone(),
        calls: AtomicUsize::new(0),
    });
    let builder = adk_rust::agent::LlmAgentBuilder::new("agent")
        .model(model.clone())
        .before_model_callback(Box::new(|_, mut request| {
            Box::pin(async move {
                request.contents.insert(
                    0,
                    Content::new("system").with_text("EXACT_SKILL_REVISION_AND_PROJECT_CONTEXT"),
                );
                Ok(BeforeModelResult::Continue(request))
            })
        }));
    let runner = adk_rust::runner::Runner::builder()
        .app_name("elitea-agent-v1")
        .agent(Arc::new(
            crate::agents::graph::PipelineNodeEventStreamingAgent::new(
                Arc::new(writer.bind(builder).build().unwrap()),
                context_receiver,
            ),
        ))
        .session_service(sessions)
        .build()
        .unwrap();
    let mut stream = runner
        .run(
            "user-1".try_into().unwrap(),
            "session-1".try_into().unwrap(),
            Content::new("user").with_text("Continue the actual task."),
        )
        .await
        .unwrap();
    let mut errors = Vec::new();
    let mut phases = Vec::new();
    while let Some(event) = stream.next().await {
        match event {
            Err(error) => errors.push(error.code.to_string()),
            Ok(event) => {
                if let Some(status) =
                    crate::agents::context_status::ModelContextStatus::from_event(&event).unwrap()
                {
                    assert_eq!(event.author, "agent");
                    assert!(event.content().is_none());
                    phases.push(status.phase);
                }
            }
        }
    }
    let summaries = summary.requests.lock().unwrap().len();
    assert_context_phases(summaries, model.calls.load(Ordering::SeqCst), &phases);
    (summaries, model.calls.load(Ordering::SeqCst), errors)
}

fn assert_context_phases(summaries: usize, model_calls: usize, phases: &[ContextPhase]) {
    if summaries > 0 {
        assert_eq!(phases.first(), Some(&ContextPhase::Compacting));
    }
    if model_calls > 0 {
        assert_eq!(
            phases.last(),
            Some(&if summaries > 0 {
                ContextPhase::Compacted
            } else {
                ContextPhase::Measured
            })
        );
        assert_eq!(phases.len(), if summaries > 0 { 2 } else { 1 });
    } else {
        assert!(!phases.contains(&ContextPhase::Compacted));
    }
}

async fn seed(sessions: &dyn SessionService) {
    let session = create(sessions).await;
    for content in history().contents.into_iter().skip(1) {
        let mut event = Event::new("prior-turn");
        event.author = if content.role == "user" {
            "user"
        } else {
            "agent"
        }
        .into();
        event.set_content(content);
        sessions
            .append_event_for_identity(AppendEventRequest {
                identity: session.try_identity().unwrap(),
                event,
            })
            .await
            .unwrap();
    }
}

#[tokio::test]
async fn runner_persists_summary_and_request_before_dispatch_and_restores_both() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    seed(sessions.as_ref()).await;
    assert_eq!(
        run(sessions.clone(), false).await,
        (1, 1, vec!["fixture_after_checkpoint".into()])
    );
    let stored = sessions.get(load_request()).await.unwrap();
    assert!(stored.events().all().iter().any(|event| {
        serde_json::to_string(event)
            .unwrap()
            .contains(&"earlier draft ".repeat(30))
    }));
    assert_eq!(
        run(sessions, true).await,
        (0, 1, vec!["fixture_after_checkpoint".into()])
    );
}

struct RejectCompaction(InMemorySessionService);
#[async_trait]
impl SessionService for RejectCompaction {
    async fn create(&self, req: CreateRequest) -> adk_rust::Result<Box<dyn Session>> {
        self.0.create(req).await
    }
    async fn get(&self, req: GetRequest) -> adk_rust::Result<Box<dyn Session>> {
        self.0.get(req).await
    }
    async fn list(&self, req: ListRequest) -> adk_rust::Result<Vec<Box<dyn Session>>> {
        self.0.list(req).await
    }
    async fn delete(&self, req: DeleteRequest) -> adk_rust::Result<()> {
        self.0.delete(req).await
    }
    async fn append_event(&self, session: &str, event: Event) -> adk_rust::Result<()> {
        if event.actions.state_delta.contains_key(STATE_KEY) {
            return Err(invalid_compaction());
        }
        self.0.append_event(session, event).await
    }
}

#[tokio::test]
async fn failed_summary_persistence_prevents_model_dispatch() {
    let sessions: Arc<dyn SessionService> =
        Arc::new(RejectCompaction(InMemorySessionService::new()));
    seed(sessions.as_ref()).await;
    assert_eq!(
        run(sessions.clone(), false).await,
        (1, 0, vec!["context_compaction_invalid".into()])
    );
    let stored = sessions.get(load_request()).await.unwrap();
    assert!(stored.state().get(STATE_KEY).is_none());
    assert_eq!(
        stored.state().get(CHECKPOINT_KEY).unwrap()["phase"],
        "context_pending"
    );
    assert!(
        ModelCheckpointWriter::new(sessions.clone(), "execution".into(), 7, [7; 32])
            .inspect(stored.as_ref(), false)
            .is_err()
    );
    assert!(
        ModelCheckpointWriter::new(sessions, "execution".into(), 7, [7; 32])
            .restore(stored.as_ref())
            .is_err()
    );
}

#[tokio::test]
async fn postgres_summary_checkpoint_survives_process_replacement() {
    let Ok(url) = std::env::var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!(
            "SKIP: set ELITEA_TEST_DATABASE_URL for durable summary process replacement proof"
        );
        return;
    };
    let db = crate::state::postgres_session_tests::IsolatedPostgres::create(&url).await;
    crate::state::postgres_session_tests::install_schema(&db.pool).await;
    for phase in ["write", "resume", "read"] {
        let output = std::process::Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "agents::context_compaction::tests::postgres_summary_child",
                "--nocapture",
            ])
            .env("ELITEA_COMPACTION_TEST_DATABASE", &db.database_name)
            .env("ELITEA_COMPACTION_TEST_PHASE", phase)
            .output()
            .unwrap();
        assert!(
            output.status.success(),
            "{phase} process failed: {} {}",
            String::from_utf8_lossy(&output.stdout),
            String::from_utf8_lossy(&output.stderr)
        );
    }
    db.pool.close().await;
}

#[tokio::test]
async fn postgres_summary_child() {
    use crate::state::{PostgresSessionService, SessionLimits, TestStateWriterLease};
    use std::str::FromStr as _;
    let Ok(database) = std::env::var("ELITEA_COMPACTION_TEST_DATABASE") else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    let phase = std::env::var("ELITEA_COMPACTION_TEST_PHASE").unwrap();
    let options = sqlx::postgres::PgConnectOptions::from_str(
        &std::env::var("ELITEA_TEST_DATABASE_URL").unwrap(),
    )
    .unwrap()
    .database(&database);
    let pool = sqlx::postgres::PgPoolOptions::new()
        .max_connections(2)
        .connect_with(options)
        .await
        .unwrap();
    let attempt = match phase.as_str() {
        "write" => 1,
        "resume" => 2,
        "read" => 3,
        _ => panic!("invalid phase"),
    };
    let sessions: Arc<dyn SessionService> = Arc::new(
        PostgresSessionService::activate(
            pool.clone(),
            crate::state::postgres_session_tests::authority_for(
                &format!("claim-{attempt}"),
                attempt,
                attempt,
                [u8::try_from(attempt).unwrap(); 32],
            ),
            SessionLimits::default(),
            Arc::new(TestStateWriterLease::current()),
        )
        .await
        .unwrap(),
    );
    if phase == "write" {
        seed(sessions.as_ref()).await;
        assert_eq!(
            run_with_summary_failure(sessions, false, true).await,
            (1, 0, vec!["fixture_summary_interrupted".into()])
        );
    } else {
        assert_eq!(
            run(sessions.clone(), true).await,
            (
                usize::from(phase == "resume"),
                1,
                vec!["fixture_after_checkpoint".into()]
            )
        );
        let retained: bool = sqlx::query_scalar(
            "SELECT EXISTS (SELECT 1 FROM elitea_runtime.agent_session_events WHERE strpos(event_payload, $1) > 0)",
        ).bind("earlier draft ".repeat(30)).fetch_one(&pool).await.unwrap();
        assert!(
            retained,
            "the immutable ledger retains original summary evidence"
        );
    }
    pool.close().await;
}

#[tokio::test]
async fn summary_correction_is_bounded_and_keeps_original_evidence() {
    for invalid_candidates in [1, 2] {
        let sessions = InMemorySessionService::new();
        let session = create(&sessions).await;
        let summary = Arc::new(Summary {
            invalid_candidates,
            ..Summary::default()
        });
        let compaction = DurableContextCompaction::new(
            plan(),
            Arc::new(Budget),
            summary.clone(),
            [7; 32],
            session.as_ref(),
        )
        .unwrap();
        let result = compaction.prepare(history(), checkpoint_ready).await;
        assert_eq!(summary.requests.lock().unwrap().len(), 2);
        assert!(compaction.record.lock().unwrap().is_none());
        assert!(session.state().get(STATE_KEY).is_none());
        if invalid_candidates == 1 {
            let (_, record) = result.unwrap();
            assert!(record.is_some());
        } else {
            assert_eq!(result.err().unwrap().code, "context_summary_evidence");
        }
        let calls = summary.requests.lock().unwrap();
        let corrected_prompt = serde_json::to_string(&calls[1]).unwrap();
        assert!(corrected_prompt.contains("context_summary_evidence"));
        assert!(corrected_prompt.contains("evidence_refs_only"));
        assert!(corrected_prompt.contains("call-one"));
        assert!(!corrected_prompt.contains("earlier draft"));
        assert!(corrected_prompt.len() < 8_000);
    }
}

#[tokio::test]
async fn smaller_summary_model_splits_and_merges_without_committing_partial_records() {
    let sessions = InMemorySessionService::new();
    let session = create(&sessions).await;
    let summary = Arc::new(Summary {
        input_byte_limit: Some(16_000),
        ..Summary::default()
    });
    let compaction = DurableContextCompaction::new(
        plan(),
        Arc::new(Budget),
        summary.clone(),
        [7; 32],
        session.as_ref(),
    )
    .unwrap();
    let objective = Content::new("user").with_text("Preserve evidence call-one.");
    let records = (0..4)
        .map(|index| {
            Content::new("model").with_text(format!("record-{index}: {}", "x".repeat(9_000)))
        })
        .collect::<Vec<_>>();
    let result = compaction.summarize(&objective, &records).await.unwrap();
    assert!(result.contains("call-one"));
    assert!(compaction.record.lock().unwrap().is_none());
    assert!(session.state().get(STATE_KEY).is_none());
    let calls = summary.requests.lock().unwrap();
    assert_eq!(
        calls.len(),
        10,
        "three failed admissions, four leaves, three merges"
    );
    for index in 0..4 {
        assert!(
            calls.iter().any(|request| {
                let encoded = serde_json::to_string(request).unwrap();
                encoded.len() <= 16_000 && encoded.contains(&format!("record-{index}:"))
            }),
            "each source record reaches an admitted leaf"
        );
    }
    let last = serde_json::to_string(calls.last().unwrap()).unwrap();
    assert!(last.contains("Validated earlier source summary"));
    assert!(last.contains("Later corrections take precedence"));
}

#[tokio::test]
async fn summary_batching_stops_for_indivisible_input_and_attempt_exhaustion() {
    let sessions = InMemorySessionService::new();
    let session = create(&sessions).await;
    let summary = Arc::new(Summary {
        input_byte_limit: Some(16_000),
        ..Summary::default()
    });
    let compaction = DurableContextCompaction::new(
        plan(),
        Arc::new(Budget),
        summary.clone(),
        [7; 32],
        session.as_ref(),
    )
    .unwrap();
    let objective = Content::new("user").with_text("Preserve evidence call-one.");
    let records = [Content::new("model").with_text("x".repeat(32_000))];
    assert_eq!(
        compaction
            .summarize(&objective, &records)
            .await
            .unwrap_err()
            .code,
        "context_summary_capacity"
    );
    let mut remaining = 0;
    assert_eq!(
        compaction
            .summarize_bounded(&objective, &records, &mut remaining, None)
            .await
            .unwrap_err()
            .code,
        "context_summary_capacity"
    );
    assert_eq!(summary.requests.lock().unwrap().len(), 1);
    assert!(compaction.record.lock().unwrap().is_none());
}

#[test]
fn reference_correction_identifies_only_values_absent_from_original_source() {
    let mut candidate: Value =
        serde_json::from_str(&super::super::context_summary::fixture()).unwrap();
    candidate["references"]
        .as_array_mut()
        .unwrap()
        .push(json!({"label":"Fabricated", "value":"missing-evidence"}));
    let source = json!({"tool_call":"call-one"});
    let feedback =
        super::super::context_summary::reference_correction_input(&candidate.to_string(), &source)
            .unwrap();
    assert_eq!(
        feedback["invalid_reference_values"],
        json!(["missing-evidence"])
    );
    assert_eq!(feedback["rejected_candidate"], candidate);
    assert_eq!(
        super::super::context_summary::validate(&candidate.to_string(), &source)
            .unwrap_err()
            .code,
        "context_summary_reference"
    );
}

#[tokio::test]
async fn merged_summary_cannot_promote_a_generated_label_into_source_evidence() {
    let sessions = InMemorySessionService::new();
    let session = create(&sessions).await;
    let summary = Arc::new(Summary {
        input_byte_limit: Some(16_000),
        reference_promotion: ReferencePromotion::Always,
        ..Summary::default()
    });
    let compaction = DurableContextCompaction::new(
        plan(),
        Arc::new(Budget),
        summary.clone(),
        [7; 32],
        session.as_ref(),
    )
    .unwrap();
    let objective = Content::new("user").with_text("Preserve evidence call-one.");
    let records = [
        Content::new("model").with_text("x".repeat(9_000)),
        Content::new("model").with_text("y".repeat(9_000)),
    ];
    assert_eq!(
        compaction
            .summarize(&objective, &records)
            .await
            .unwrap_err()
            .code,
        "context_summary_reference"
    );
    assert_eq!(summary.requests.lock().unwrap().len(), 5);
    assert!(compaction.record.lock().unwrap().is_none());
}

#[tokio::test]
async fn merged_summary_repairs_references_against_original_records() {
    let sessions = InMemorySessionService::new();
    let session = create(&sessions).await;
    let summary = Arc::new(Summary {
        input_byte_limit: Some(16_000),
        reference_promotion: ReferencePromotion::UntilCorrection,
        ..Summary::default()
    });
    let compaction = DurableContextCompaction::new(
        plan(),
        Arc::new(Budget),
        summary.clone(),
        [7; 32],
        session.as_ref(),
    )
    .unwrap();
    let objective = Content::new("user").with_text("Preserve evidence call-one.");
    let records = [
        Content::new("model").with_text("x".repeat(9_000)),
        Content::new("model").with_text("y".repeat(9_000)),
    ];
    let result = compaction.summarize(&objective, &records).await.unwrap();
    assert!(result.contains("call-one"));
    let requests = summary.requests.lock().unwrap();
    let repair = serde_json::to_string(requests.last().unwrap()).unwrap();
    assert!(repair.contains("invalid_reference_values"));
    drop(requests);
    assert_eq!(summary.requests.lock().unwrap().len(), 5);
    assert!(compaction.record.lock().unwrap().is_none());
}

#[tokio::test]
async fn bulky_recent_messages_are_summarized_while_the_current_request_stays_exact() {
    let sessions = InMemorySessionService::new();
    let session = create(&sessions).await;
    let mut settings = plan();
    settings.preserve_recent_messages = 5;
    let compaction = DurableContextCompaction::new(
        settings,
        Arc::new(Budget),
        Arc::new(Summary::default()),
        [7; 32],
        session.as_ref(),
    )
    .unwrap();
    let current = Content::new("user")
        .with_text("Continue with the corrected teal delivery; prepare the handoff note.");
    let mut contents = vec![Content::new("user").with_text("Preserve evidence call-one.")];
    for index in 0..4 {
        contents.push(
            Content::new("model").with_text(format!("archive-{index}: {}", "x".repeat(8_000))),
        );
    }
    contents.push(current.clone());
    let request: LlmRequest =
        serde_json::from_value(json!({"model":"fixture", "contents":contents})).unwrap();
    let (prepared, record) = compaction.prepare(request, checkpoint_ready).await.unwrap();
    assert_eq!(record.unwrap().covered_count, 4);
    assert_eq!(prepared.contents.last().unwrap().parts, current.parts);
    let usage = Budget.measure(&prepared).unwrap();
    assert!(usage.estimated_input <= usage.budget.compaction_target());
    assert!(
        !serde_json::to_string(&prepared)
            .unwrap()
            .contains("archive-")
    );
}

#[tokio::test]
async fn reference_repair_allows_one_final_evidence_only_correction() {
    let sessions = InMemorySessionService::new();
    let session = create(&sessions).await;
    let summary = Arc::new(Summary {
        repair_reference_then_links: true,
        ..Summary::default()
    });
    let compaction = DurableContextCompaction::new(
        plan(),
        Arc::new(Budget),
        summary.clone(),
        [7; 32],
        session.as_ref(),
    )
    .unwrap();
    let objective = Content::new("user").with_text("Preserve evidence call-one.");
    let records = [Content::new("model").with_text("bulk-original-source ".repeat(500))];
    let result = compaction.summarize(&objective, &records).await.unwrap();
    assert!(result.contains("call-one"));
    assert!(!result.contains("invented-reference"));
    let calls = summary.requests.lock().unwrap();
    assert_eq!(calls.len(), 3);
    let correction = serde_json::to_string(calls.last().unwrap()).unwrap();
    assert!(correction.contains("evidence_refs_only"));
    assert!(!correction.contains("bulk-original-source"));
    assert!(compaction.record.lock().unwrap().is_none());
}

struct MillionTokenBudget;
impl ModelRequestBudget for MillionTokenBudget {
    fn measure(&self, request: &LlmRequest) -> adk_rust::Result<RequestContextUsage> {
        let settings = json!({"budget_mode":"full"}).as_object().unwrap().clone();
        RequestContextBudget::resolve(
            Some(ModelContextLimits {
                context_window_tokens: 1_000_000,
                max_output_tokens: 128_000,
                context_window_fallback: false,
                max_output_fallback: false,
                max_input_tokens: None,
            }),
            &settings,
            Some(128_000),
        )
        .unwrap()
        .unwrap()
        .measure_provider_request(&serde_json::to_vec(request).unwrap(), 8 * 1024 * 1024)
    }
}

#[tokio::test]
async fn million_token_window_repeated_compaction_preserves_current_authority() {
    let sessions = InMemorySessionService::new();
    let session = create(&sessions).await;
    let summary = Arc::new(Summary::default());
    let compaction = DurableContextCompaction::new(
        ContextCompactionPlan {
            max_context_tokens: 1_000_000,
            ..plan()
        },
        Arc::new(MillionTokenBudget),
        summary.clone(),
        [7; 32],
        session.as_ref(),
    )
    .unwrap();
    let mut original = history();
    let exact_authority = original.contents[0].clone();
    let exact_correction = original.contents[3].clone();
    for cycle in 0..3 {
        let at = original.contents.len() - 2;
        for index in 0..16 {
            original.contents.insert(
                at,
                Content::new("model").with_text(format!(
                    "Cycle {cycle}, record {index}: {}",
                    "completed work ".repeat(15_000)
                )),
            );
        }
        let before = MillionTokenBudget.measure(&original).unwrap();
        assert_eq!(before.budget.input_limit, 863_808);
        assert_eq!(before.budget.output_reservation, 128_000);
        assert_eq!(before.budget.margin_tokens, 8_192);
        assert!(before.needs_compaction());
        let (prepared, record) = compaction
            .prepare(original.clone(), checkpoint_ready)
            .await
            .unwrap();
        let after = MillionTokenBudget.measure(&prepared).unwrap();
        assert!(after.fits());
        assert!(after.estimated_input <= after.budget.compaction_target());
        assert_eq!(prepared.contents[0].parts, exact_authority.parts);
        assert!(
            prepared
                .contents
                .iter()
                .any(|content| content.parts == exact_correction.parts)
        );
        assert_eq!(
            prepared.contents.last().unwrap().parts,
            original.contents.last().unwrap().parts
        );
        assert!(record.is_some());
        compaction.committed(record).unwrap();
    }
    assert_eq!(summary.requests.lock().unwrap().len(), 3);
}

#[tokio::test]
async fn runner_history_projects_exact_summary_without_rewriting_control_or_durable_events() {
    let service = InMemorySessionService::new();
    let definition = [7; 32];
    let anchor = Content::new("user").with_text("Prepare the handoff; send nothing externally.");
    let earlier = Content::new("model").with_text("Verified archive. ".repeat(400));
    let recent = Content::new("user").with_text("Use teal in the handoff.");
    let summary = Content::new("user").with_text("Archive verified. Handoff pending.");
    let record = CompactionRecord {
        version: 1,
        definition_digest: definition,
        anchor: extend_digest([0; 32], std::slice::from_ref(&anchor)).unwrap(),
        covered_count: 1,
        covered_digest: extend_digest([0; 32], std::slice::from_ref(&earlier)).unwrap(),
        replacement: vec![summary.clone()],
    };
    let session = service
        .create(CreateRequest {
            app_name: "projection-test".into(),
            user_id: "user-1".into(),
            session_id: Some("session-1".into()),
            state: [(STATE_KEY.to_owned(), serde_json::to_value(&record).unwrap())].into(),
        })
        .await
        .unwrap();
    let identity = session.try_identity().unwrap();
    for (id, author, content) in [
        ("anchor", "user", anchor.clone()),
        ("covered", "elitea-agent", earlier.clone()),
        ("recent", "user", recent.clone()),
    ] {
        let mut event = Event::with_id(id, "invocation-1");
        event.author = author.into();
        event.set_content(content);
        if id == "covered" {
            event.actions.transfer_to_agent = Some("child-agent".into());
        }
        service
            .append_event_for_identity(AppendEventRequest {
                identity: identity.clone(),
                event,
            })
            .await
            .unwrap();
    }
    let request = GetRequest {
        app_name: "projection-test".into(),
        user_id: "user-1".into(),
        session_id: "session-1".into(),
        num_recent_events: None,
        after: None,
    };
    let projected = crate::agents::runner_history::project(
        service.get(request.clone()).await.unwrap(),
        Some((definition, "elitea-agent")),
    )
    .unwrap();
    let projected =
        crate::agents::runner_history::project(projected, Some((definition, "elitea-agent")))
            .unwrap();
    let events = projected.events().all();
    let native = adk_rust::runner::MutableSession::new(Arc::from(projected));
    let history = native.conversation_history_for_agent_impl(Some("elitea-agent"), "");
    assert_eq!(
        serde_json::to_value(history).unwrap(),
        serde_json::to_value(vec![anchor.clone(), summary, recent]).unwrap()
    );
    let control = events.iter().find(|event| event.id == "covered").unwrap();
    assert_eq!(
        control.actions.transfer_to_agent.as_deref(),
        Some("child-agent")
    );
    assert!(control.llm_response.content.is_none());
    let original = service.get(request.clone()).await.unwrap();
    assert_eq!(original.events().len(), 3);
    assert_original_covered_event(original.as_ref(), &earlier);
    let foreign = crate::agents::runner_history::project(
        service.get(request.clone()).await.unwrap(),
        Some(([8; 32], "elitea-agent")),
    )
    .unwrap();
    assert_original_covered_event(foreign.as_ref(), &earlier);
    assert_branch_projection_refused(&service, request, identity, definition, &earlier).await;
    let changed = vec![anchor, Content::new("model").with_text("Changed evidence")];
    assert!(
        history_replacement(serde_json::to_value(&record).unwrap(), definition, &changed)
            .unwrap()
            .is_none()
    );
}

fn assert_original_covered_event(session: &dyn Session, expected: &Content) {
    assert_eq!(session.events().len(), 3);
    let content = session
        .events()
        .at(1)
        .unwrap()
        .llm_response
        .content
        .as_ref()
        .unwrap();
    assert_eq!(content.parts, expected.parts);
}

async fn assert_branch_projection_refused(
    service: &InMemorySessionService,
    request: GetRequest,
    identity: adk_rust::AdkIdentity,
    definition: [u8; 32],
    earlier: &Content,
) {
    let mut event = Event::with_id("sibling", "invocation-1");
    event.author = "sibling-agent".into();
    event.branch = "sibling".into();
    event.set_content(Content::new("model").with_text("Sibling result stays separate."));
    service
        .append_event_for_identity(AppendEventRequest { identity, event })
        .await
        .unwrap();
    let view = crate::agents::runner_history::project(
        service.get(request).await.unwrap(),
        Some((definition, "elitea-agent")),
    )
    .unwrap();
    assert_eq!(view.events().len(), 4);
    assert_eq!(
        view.events()
            .at(1)
            .unwrap()
            .llm_response
            .content
            .as_ref()
            .unwrap()
            .parts,
        earlier.parts
    );
    assert_eq!(view.events().at(3).unwrap().branch, "sibling");
}

struct RepeatedLoopProbe {
    calls: AtomicUsize,
}

#[async_trait]
impl Llm for RepeatedLoopProbe {
    fn name(&self) -> &'static str {
        "repeated-compaction-probe"
    }
    async fn generate_content(
        &self,
        request: LlmRequest,
        _: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        assert!(Budget.measure(&request).unwrap().fits());
        assert!(request.contents.iter().any(|content| {
            content.parts
                == Content::new("user")
                    .with_text("Original task, preserve this exactly.")
                    .parts
        }));
        let index = self.calls.fetch_add(1, Ordering::SeqCst);
        let content = if index < 24 {
            Content {
                role: "model".into(),
                parts: vec![Part::FunctionCall {
                    name: "repeated_lookup".into(),
                    args: json!({"index": index}),
                    id: Some(if index == 0 {
                        "call-one".into()
                    } else {
                        format!("lookup-{index}")
                    }),
                    thought_signature: None,
                }],
            }
        } else {
            Content::new("model").with_text("Completed 24 lookups.")
        };
        Ok(Box::pin(stream::once(async move {
            Ok(LlmResponse::new(content))
        })))
    }
}

#[async_trait]
impl adk_rust::Tool for RepeatedLoopProbe {
    fn name(&self) -> &'static str {
        "repeated_lookup"
    }
    fn description(&self) -> &'static str {
        "Return fictional lookup evidence."
    }
    async fn execute(
        &self,
        _: Arc<dyn adk_rust::ToolContext>,
        args: Value,
    ) -> adk_rust::Result<Value> {
        Ok(json!({"index":args["index"], "output":"archive evidence ".repeat(320)}))
    }
}

#[tokio::test]
async fn native_tool_loop_compacts_repeatedly_with_persisted_coverage() {
    use adk_rust::agent::LlmAgentBuilder;
    let sessions = Arc::new(InMemorySessionService::new());
    let session = create(sessions.as_ref()).await;
    let summary = Arc::new(Summary::default());
    let compaction = Arc::new(
        DurableContextCompaction::new(
            plan(),
            Arc::new(Budget),
            summary.clone(),
            [7; 32],
            session.as_ref(),
        )
        .unwrap(),
    );
    let probe = Arc::new(RepeatedLoopProbe {
        calls: AtomicUsize::new(0),
    });
    let observed = Arc::new(Mutex::new(Vec::new()));
    let committed_cycles = Arc::new(AtomicUsize::new(0));
    let agent = LlmAgentBuilder::new("elitea-agent")
        .retain_prepared_history(true)
        .model(probe.clone())
        .tool(probe.clone())
        .before_model_callback(Box::new({
            let sessions = sessions.clone();
            let compaction = compaction.clone();
            let observed = observed.clone();
            let committed_cycles = committed_cycles.clone();
            move |_, request| {
                let sessions = sessions.clone();
                let compaction = compaction.clone();
                let observed = observed.clone();
                let committed_cycles = committed_cycles.clone();
                Box::pin(async move {
                    observed
                        .lock()
                        .unwrap()
                        .push(serde_json::to_vec(&request).unwrap().len());
                    let (prepared, record) = compaction.prepare(request, checkpoint_ready).await?;
                    if record.is_some() {
                        store(sessions.as_ref(), record.as_ref()).await;
                        compaction.committed(record)?;
                        committed_cycles.fetch_add(1, Ordering::SeqCst);
                    }
                    Ok(BeforeModelResult::Continue(prepared))
                })
            }
        }))
        .build()
        .unwrap();
    let runner = adk_rust::runner::Runner::builder()
        .app_name("elitea-agent-v1")
        .agent(Arc::new(agent))
        .session_service(sessions.clone())
        .build()
        .unwrap();
    let mut events = runner
        .run(
            "user-1".try_into().unwrap(),
            "session-1".try_into().unwrap(),
            Content::new("user").with_text("Original task, preserve this exactly."),
        )
        .await
        .unwrap();
    while let Some(event) = events.next().await {
        event.unwrap();
    }
    let sizes = observed.lock().unwrap().clone();
    let cycles = summary.requests.lock().unwrap().len();
    assert!(committed_cycles.load(Ordering::SeqCst) >= cycles);
    println!("native loop compactions={cycles}, callback request bytes={sizes:?}");
    assert_eq!(probe.calls.load(Ordering::SeqCst), 25);
    assert!(
        cycles >= 3,
        "the native loop must cross the compaction threshold repeatedly"
    );
    assert!(
        sizes.iter().max().unwrap() < &40_000,
        "obsolete tool results accumulated in the model loop"
    );
    let restored = sessions.get(load_request()).await.unwrap();
    assert!(restored.state().get(STATE_KEY).is_some());
}

#[tokio::test]
async fn completed_tool_batch_larger_than_recent_budget_can_be_compacted_whole() {
    let sessions = InMemorySessionService::new();
    let session = create(&sessions).await;
    let summary = Arc::new(Summary::default());
    let compaction = DurableContextCompaction::new(
        plan(),
        Arc::new(Budget),
        summary.clone(),
        [7; 32],
        session.as_ref(),
    )
    .unwrap();
    let mut request = history();
    request.contents.truncate(2);
    let mut calls = Vec::new();
    let mut results = Vec::new();
    for index in 0..12 {
        let id = if index == 0 {
            "call-one".into()
        } else {
            format!("batch-{index}")
        };
        calls.push(Part::FunctionCall {
            name: "lookup".into(),
            args: json!({"index":index}),
            id: Some(id.clone()),
            thought_signature: None,
        });
        let content: Content = serde_json::from_value(json!({"role":"function","parts":[{"id":id,"functionResponse":{"name":"lookup","response":{"output":"evidence ".repeat(400)}}}]})).unwrap();
        results.extend(content.parts);
    }
    request.contents.push(Content {
        role: "model".into(),
        parts: calls,
    });
    request.contents.push(Content {
        role: "function".into(),
        parts: results,
    });
    assert!(!Budget.measure(&request).unwrap().fits());
    let mut pending = request.clone();
    pending.contents.last_mut().unwrap().parts.pop();
    let error = compaction
        .prepare(pending, checkpoint_ready)
        .await
        .err()
        .unwrap();
    assert_eq!(error.code, "context_budget_exceeded");
    assert!(summary.requests.lock().unwrap().is_empty());
    let original_authority = request.contents[..2].to_vec();
    let (prepared, record) = compaction.prepare(request, checkpoint_ready).await.unwrap();
    assert!(Budget.measure(&prepared).unwrap().fits());
    assert_eq!(
        serde_json::to_value(&prepared.contents[..2]).unwrap(),
        serde_json::to_value(original_authority).unwrap()
    );
    assert!(record.is_some());
    let requests = summary.requests.lock().unwrap();
    assert_eq!(requests.len(), 1);
    let source = serde_json::to_string(&requests[0]).unwrap();
    assert!(source.contains("call-one") && source.contains("batch-11"));
}
