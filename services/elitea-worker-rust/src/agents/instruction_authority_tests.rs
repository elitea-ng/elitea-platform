use super::*;
use adk_rust::session::{CreateRequest, GetRequest, InMemorySessionService, SessionService};
use adk_rust::{Llm, LlmRequest, LlmResponse, LlmResponseStream, Part};
use std::sync::Mutex;

fn plan(content: &str) -> InstructionPlan {
    let mut plan = InstructionPlan {
        run_id: "run-1".to_owned(),
        ..InstructionPlan::default()
    };
    plan.add_skills(&[json!({"id":"skill:1:version:2","name":"review","revision":content_digest(content),"scope":"project:1","instructions":content})]).unwrap();
    plan
}

struct RecordingModel {
    requests: Mutex<Vec<LlmRequest>>,
    load: bool,
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
            Content {
                role: "model".to_owned(),
                parts: vec![Part::FunctionCall {
                    name: "load_skill".to_owned(),
                    args: json!({"skill":"review"}),
                    id: Some("load-1".to_owned()),
                    thought_signature: None,
                }],
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
    let mut resumed = plan("Mutated instructions must stay inactive.");
    resumed.resume = true;
    resumed.run_id = "resume-2".to_owned();
    let second = Arc::new(RecordingModel {
        requests: Mutex::new(Vec::new()),
        load: false,
        pause: false,
    });
    run(&resumed, replacement, second.clone()).await;
    let requests = second.requests.lock().unwrap();
    let text = requests[0].contents[0].parts[0].text().unwrap();
    assert!(text.contains("Exact original instruction."));
    assert!(!text.contains("Mutated instructions"));
}

#[test]
fn duplicate_revision_collision_scope_and_missing_state_are_checked() {
    let mut original = plan("old");
    let skill = json!({"id":"skill:1:version:2","name":"review","revision":content_digest("old"),"scope":"project:1","instructions":"old"});
    original.add_skills(&[skill]).unwrap();
    assert_eq!(original.catalog.len(), 1);
    let collision = json!({"id":"skill:9","name":"review","revision":content_digest("new"),"scope":"project:1","instructions":"new"});
    assert!(original.add_skills(&[collision]).is_err());
    assert!(original.resolve_skill(&json!({"name":"unknown"})).is_err());
    let state = original.start(None, "scope-a".to_owned()).unwrap();
    assert!(state.validate("scope-b").is_err());
    original.resume = true;
    assert!(original.start(None, "scope-a".to_owned()).is_err());
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
        let mut changed = plan("Changed after pause; never use this revision.");
        changed.run_id = "resume-2".to_owned();
        changed.resume = true;
        let model = Arc::new(RecordingModel {
            requests: Mutex::new(Vec::new()),
            load: false,
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

#[test]
fn project_context_eager_and_on_demand_activation_and_fresh_turn_reset() {
    let context = |description: &str| crate::agents::request::ProjectContextSnapshot {
        id: "project:17".to_owned(),
        revision: content_digest("Verbatim context.\n"),
        scope: "project:17".to_owned(),
        content: "Verbatim context.\n".to_owned(),
        activation_description: description.to_owned(),
    };
    let mut eager = InstructionPlan {
        run_id: "run-1".to_owned(),
        ..Default::default()
    };
    eager.add_project_context(&context("")).unwrap();
    assert_eq!(eager.active.len(), 1);
    let state = eager.start(None, "scope".to_owned()).unwrap();
    assert!(state.render().contains("Verbatim context.\n"));
    let mut next = InstructionPlan {
        run_id: "run-2".to_owned(),
        ..Default::default()
    };
    next.add_project_context(&context("release requests"))
        .unwrap();
    let reset = next
        .start(
            Some(serde_json::to_value(state).unwrap()),
            "scope".to_owned(),
        )
        .unwrap();
    assert!(reset.active.is_empty());
    assert!(!reset.render().contains("Verbatim context."));
    assert!(reset.render().contains("release requests"));
    let mut corrupt_context = context("");
    corrupt_context.content.push('!');
    assert!(next.add_project_context(&corrupt_context).is_err());
}

#[test]
fn fresh_nested_scope_does_not_inherit_parent_activation_or_catalog() {
    let mut parent = plan("Parent instructions.");
    parent.active.insert("skill:1:version:2".to_owned());
    parent.resume = true;
    let child = InstructionPlan::nested(&serde_json::Map::new(), &parent).unwrap();
    assert!(child.active.is_empty() && child.catalog.is_empty());
    assert!(child.start(None, "new-child-scope".to_owned()).is_ok());
}
