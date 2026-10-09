//! Instruction authority (moved to elitea-agent-runtime) composed with the
//! worker's internal tools, Postgres sessions and durable compaction.

use std::sync::Arc;

use adk_rust::agent::LlmAgentBuilder;
use adk_rust::futures::StreamExt as _;
use adk_rust::{Content, Event};
use async_trait::async_trait;
use serde_json::{Value, json};

use super::instruction_authority::{InstructionPlan, STATE_PREFIX, content_digest};
use adk_rust::session::{CreateRequest, GetRequest, InMemorySessionService, SessionService};
use adk_rust::{Llm, LlmRequest, LlmResponse, LlmResponseStream, Part};
use std::sync::Mutex;

fn skill(content: &str) -> Value {
    json!({"id":"skill:1:version:2","name":"review","revision":content_digest(content),"scope":"project:1","instructions":content})
}

fn plan(content: &str) -> InstructionPlan {
    plan_for("run-1", false, content, &[])
}

fn plan_for(
    run_id: &str,
    resume: bool,
    content: &str,
    contexts: &[crate::agents::request::ProjectContextSnapshot],
) -> InstructionPlan {
    InstructionPlan::fixture(run_id, resume, &[skill(content)], contexts).unwrap()
}

struct RecordingModel {
    requests: Mutex<Vec<LlmRequest>>,
    load: bool,
    load_context: bool,
    pause: bool,
}
#[async_trait]
impl Llm for RecordingModel {
    fn name(&self) -> &'static str {
        "fixture"
    }
    async fn generate_content(
        &self,
        request: LlmRequest,
        _stream: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        let mut requests = self.requests.lock().unwrap();
        requests.push(request);
        let content = if self.load && requests.len() == 1 {
            let mut parts = vec![Part::FunctionCall {
                name: "load_skill".to_owned(),
                args: json!({"skill":"review"}),
                id: Some("load-1".to_owned()),
                thought_signature: None,
            }];
            if self.load_context {
                parts.push(Part::FunctionCall {
                    name: "read_project_context".to_owned(),
                    args: json!({}),
                    id: Some("context-1".to_owned()),
                    thought_signature: None,
                });
            }
            Content {
                role: "model".to_owned(),
                parts,
            }
        } else if self.pause {
            Content {
                role: "model".to_owned(),
                parts: vec![Part::FunctionCall {
                    name: "ask_user".to_owned(),
                    args: json!({"questions":[{"question":"Continue?","header":"Choice","options":[],"allow_other":true}]}),
                    id: Some("pause-1".to_owned()),
                    thought_signature: None,
                }],
            }
        } else {
            Content::new("model").with_text("done")
        };
        let response = LlmResponse {
            content: Some(content),
            turn_complete: true,
            ..Default::default()
        };
        Ok(Box::pin(adk_rust::futures::stream::iter([Ok(response)])))
    }
}

async fn run(
    plan: &InstructionPlan,
    sessions: Arc<dyn SessionService>,
    model: Arc<RecordingModel>,
) -> Vec<Event> {
    run_at(plan, sessions, model, "test", "user", "session").await
}

async fn run_at(
    plan: &InstructionPlan,
    sessions: Arc<dyn SessionService>,
    model: Arc<RecordingModel>,
    app: &str,
    user: &str,
    session: &str,
) -> Vec<Event> {
    let pause = model.pause;
    let mut builder = plan.bind_builder(LlmAgentBuilder::new("assistant").model(model));
    if pause {
        for toolset in
            crate::agents::internal_tools::InternalToolCatalog::from_names(&["ask_user".to_owned()])
                .unwrap()
                .toolsets(None)
        {
            builder = builder.toolset(toolset);
        }
        builder = builder.require_tool_confirmation("ask_user");
    }
    for tools in plan.toolsets() {
        builder = builder.toolset(tools);
    }
    let agent = plan.wrap(Arc::new(builder.build().unwrap()));
    let runner = adk_rust::runner::Runner::builder()
        .app_name(app)
        .agent(agent)
        .session_service(sessions)
        .build()
        .unwrap();
    let mut events = runner
        .run(
            adk_rust::UserId::new(user).unwrap(),
            adk_rust::SessionId::new(session).unwrap(),
            Content::new("user").with_text("continue"),
        )
        .await
        .unwrap();
    let mut result = Vec::new();
    while let Some(event) = events.next().await {
        result.push(event.unwrap());
    }
    result
}

#[tokio::test]
async fn activation_survives_transcript_loss_and_changed_resume_snapshot() {
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "test".to_owned(),
            user_id: "user".to_owned(),
            session_id: Some("session".to_owned()),
            state: std::collections::HashMap::default(),
        })
        .await
        .unwrap();
    let first = Arc::new(RecordingModel {
        requests: Mutex::new(Vec::new()),
        load: true,
        load_context: false,
        pause: false,
    });
    run(
        &plan("Exact original instruction."),
        sessions.clone(),
        first.clone(),
    )
    .await;
    {
        let requests = first.requests.lock().unwrap();
        assert!(
            requests[0].contents[0].parts[0]
                .text()
                .unwrap()
                .contains("Available skill")
        );
        assert!(
            requests[1].contents[0].parts[0]
                .text()
                .unwrap()
                .contains("Exact original instruction."),
            "{:?}",
            requests[1].contents
        );
    }
    let stored = sessions
        .get(GetRequest {
            app_name: "test".to_owned(),
            user_id: "user".to_owned(),
            session_id: "session".to_owned(),
            num_recent_events: None,
            after: None,
        })
        .await
        .unwrap();
    // A new service receives only serialized state. No transcript can restore instructions.
    let replacement = Arc::new(InMemorySessionService::new());
    replacement
        .create(CreateRequest {
            app_name: "test".to_owned(),
            user_id: "user".to_owned(),
            session_id: Some("session".to_owned()),
            state: serde_json::from_str(&serde_json::to_string(&stored.state().all()).unwrap())
                .unwrap(),
        })
        .await
        .unwrap();
    let resumed = plan_for(
        "resume-2",
        true,
        "Mutated instructions must stay inactive.",
        &[],
    );
    let second = Arc::new(RecordingModel {
        requests: Mutex::new(Vec::new()),
        load: false,
        load_context: false,
        pause: false,
    });
    run(&resumed, replacement, second.clone()).await;
    let requests = second.requests.lock().unwrap();
    let text = requests[0].contents[0].parts[0].text().unwrap();
    assert!(text.contains("Exact original instruction."));
    assert!(!text.contains("Mutated instructions"));
}

#[tokio::test]
async fn postgres_instruction_pause_survives_process_replacement() {
    let Ok(url) = std::env::var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for PostgreSQL process replacement proof");
        return;
    };
    let db = crate::state::postgres_session_tests::IsolatedPostgres::create(&url).await;
    crate::state::postgres_session_tests::install_schema(&db.pool).await;
    for phase in ["write", "read"] {
        let output = std::process::Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "agents::instruction_authority::tests::postgres_replacement_child",
                "--nocapture",
            ])
            .env("ELITEA_INSTRUCTION_TEST_DATABASE", &db.database_name)
            .env("ELITEA_INSTRUCTION_TEST_PHASE", phase)
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
async fn postgres_replacement_child() {
    use crate::state::{PostgresSessionService, SessionLimits, TestStateWriterLease};
    use std::str::FromStr as _;
    let Ok(database) = std::env::var("ELITEA_INSTRUCTION_TEST_DATABASE") else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    let phase = std::env::var("ELITEA_INSTRUCTION_TEST_PHASE").unwrap();
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
    let attempt = if phase == "write" { 1 } else { 2 };
    let sessions = Arc::new(
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
        sessions
            .create(CreateRequest {
                app_name: "elitea-agent-v1".to_owned(),
                user_id: "user-1".to_owned(),
                session_id: Some("session-1".to_owned()),
                state: std::collections::HashMap::default(),
            })
            .await
            .unwrap();
        let model = Arc::new(RecordingModel {
            requests: Mutex::new(Vec::new()),
            load: true,
            load_context: false,
            pause: true,
        });
        let events = run_at(
            &plan("Original paused instruction."),
            sessions,
            model,
            "elitea-agent-v1",
            "user-1",
            "session-1",
        )
        .await;
        assert!(
            events
                .iter()
                .any(|event| event.actions.tool_confirmation.is_some())
        );
    } else {
        let changed = plan_for(
            "resume-2",
            true,
            "Changed after pause; never use this revision.",
            &[],
        );
        let model = Arc::new(RecordingModel {
            requests: Mutex::new(Vec::new()),
            load: false,
            load_context: false,
            pause: false,
        });
        run_at(
            &changed,
            sessions,
            model.clone(),
            "elitea-agent-v1",
            "user-1",
            "session-1",
        )
        .await;
        let requests = model.requests.lock().unwrap();
        let text = requests[0].contents[0].parts[0].text().unwrap();
        assert!(text.contains("Original paused instruction."));
        assert!(!text.contains("Changed after pause"));
    }
    pool.close().await;
}

struct InstructionBudget;
impl crate::agents::context_budget::ModelRequestBudget for InstructionBudget {
    fn measure(
        &self,
        request: &LlmRequest,
    ) -> adk_rust::Result<crate::agents::context_budget::RequestContextUsage> {
        crate::agents::context_budget::RequestContextBudget::resolve(
            Some(crate::agents::request::ModelContextLimits {
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
struct InstructionSummary(Mutex<usize>);
#[async_trait]
impl Llm for InstructionSummary {
    fn name(&self) -> &'static str {
        "instruction-summary"
    }
    async fn generate_content(
        &self,
        _: LlmRequest,
        _: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        *self.0.lock().unwrap() += 1;
        let mut summary: Value =
            serde_json::from_str(&crate::agents::context_summary::fixture()).unwrap();
        summary["references"] = json!([]);
        summary["completed_work"] = json!([]);
        Ok(Box::pin(adk_rust::futures::stream::iter([Ok(
            LlmResponse::new(Content::new("model").with_text(summary.to_string())),
        )])))
    }
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // Exercise authority, compaction, and checkpoint composition together.
async fn compaction_keeps_original_skill_and_project_revisions_after_source_edits() {
    use crate::agents::context_compaction::DurableContextCompaction;
    use crate::agents::context_management::ContextCompactionPlan;
    use crate::agents::model_checkpoint::{CHECKPOINT_KEY, ModelCheckpointWriter};
    use adk_rust::session::AppendEventRequest;
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let identity = sessions
        .create(CreateRequest {
            app_name: "test".into(),
            user_id: "user".into(),
            session_id: Some("session".into()),
            state: std::collections::HashMap::default(),
        })
        .await
        .unwrap()
        .try_identity()
        .unwrap();
    let context = |text: &str| crate::agents::request::ProjectContextSnapshot {
        id: "project:1".into(),
        revision: content_digest(text),
        scope: "project:1".into(),
        content: text.into(),
        activation_description: String::new(),
    };
    let original = plan_for(
        "run-1",
        false,
        "Original skill: preserve CEDAR-731 exactly.\n",
        &[context("Original project: use teal.\n")],
    );
    run(
        &original,
        sessions.clone(),
        Arc::new(RecordingModel {
            requests: Mutex::new(Vec::new()),
            load: true,
            load_context: true,
            pause: false,
        }),
    )
    .await;
    for _ in 0..3 {
        let mut event = Event::new("prior-work");
        event.author = "assistant".into();
        event.set_content(Content::new("model").with_text("Old work detail. ".repeat(1200)));
        sessions
            .append_event_for_identity(AppendEventRequest {
                identity: identity.clone(),
                event,
            })
            .await
            .unwrap();
    }
    let load = || GetRequest {
        app_name: "test".into(),
        user_id: "user".into(),
        session_id: "session".into(),
        num_recent_events: None,
        after: None,
    };
    let stored = sessions.get(load()).await.unwrap();
    let authority_key = stored
        .state()
        .all()
        .keys()
        .find(|key| key.starts_with(STATE_PREFIX))
        .unwrap()
        .clone();
    let authority_before = stored.state().get(&authority_key).unwrap();
    assert_eq!(
        authority_before["active"].as_array().map(Vec::len).unwrap(),
        2,
        "both tools in one model response must remain active"
    );
    let changed_skill = "Changed skill: use AMBER-999 instead.";
    let changed_context = [context("Changed project: use orange.")];
    let changed = plan_for("replacement-run", true, changed_skill, &changed_context);
    let summary = Arc::new(InstructionSummary::default());
    let compactor = Arc::new(
        DurableContextCompaction::new(
            ContextCompactionPlan {
                max_context_tokens: 8000,
                preserve_recent_messages: 2,
                preserve_system_messages: true,
                summary_instructions: String::new(),
            },
            Arc::new(InstructionBudget),
            summary.clone(),
            [7; 32],
            stored.as_ref(),
        )
        .unwrap(),
    );
    let writer = ModelCheckpointWriter::new(sessions.clone(), "execution".into(), 7, [7; 32])
        .with_request_budget(Some(Arc::new(InstructionBudget)))
        .with_context_compaction(Some(compactor));
    let model = Arc::new(RecordingModel {
        requests: Mutex::new(Vec::new()),
        load: false,
        load_context: false,
        pause: false,
    });
    let builder = changed.bind_builder(LlmAgentBuilder::new("assistant").model(model.clone()));
    let runner = adk_rust::runner::Runner::builder()
        .app_name("test")
        .agent(changed.wrap(Arc::new(writer.bind(builder).build().unwrap())))
        .session_service(sessions.clone())
        .build()
        .unwrap();
    let mut events = runner
        .run(
            "user".try_into().unwrap(),
            "session".try_into().unwrap(),
            Content::new("user").with_text("Continue the original work."),
        )
        .await
        .unwrap();
    while let Some(event) = events.next().await {
        event.unwrap();
    }
    assert!(*summary.0.lock().unwrap() > 0, "must actually compact");
    {
        let requests = model.requests.lock().unwrap();
        assert_eq!(requests.len(), 1);
        let dispatched = serde_json::to_string(&requests[0]).unwrap();
        for (id, revision, source) in original.sources_for_test() {
            assert!(dispatched.contains(&revision));
            assert!(dispatched.contains(&id));
            assert!(requests[0].contents.iter().any(|content| {
                content
                    .parts
                    .iter()
                    .any(|part| part.text().is_some_and(|text| text.contains(&source)))
            }));
        }
        assert!(!dispatched.contains("AMBER-999"));
        assert!(!dispatched.contains("orange"));
        assert!(!dispatched.contains(&"Old work detail. ".repeat(100)));
    }
    let after = sessions.get(load()).await.unwrap();
    assert_eq!(after.state().get(&authority_key).unwrap(), authority_before);
    let checkpoint = after.state().get(CHECKPOINT_KEY).unwrap();
    assert!(checkpoint.to_string().contains("CEDAR-731"));
    assert!(!checkpoint.to_string().contains("AMBER-999"));
    // Only a fresh independent turn may adopt edited source revisions.
    let fresh_turn = plan_for("replacement-run", false, changed_skill, &changed_context);
    let scope = authority_before["scope"].as_str().unwrap().to_owned();
    let fresh = fresh_turn
        .render_for_test(Some(authority_before), scope)
        .unwrap();
    assert!(fresh.contains("orange"));
    assert!(!fresh.contains("use teal"));
}
