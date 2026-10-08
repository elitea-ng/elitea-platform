//! Durable replay of `split_out` -> `state_modifier` -> `aggregate` across process replacement.
//!
//! The parent test computes an uninterrupted in-memory reference, then runs two child processes
//! of this test binary against one isolated database. The "write" process pauses before `enrich`
//! with the split output checkpointed; the "read" process is a replacement writer (claim attempt
//! 2) that resumes through the production text continuation and finishes the pipeline. Sessions
//! are PostgreSQL-backed because the static pause event that authorizes the resume lives there.

#![cfg(feature = "graph-extensions-rehearsal")]

use std::collections::HashMap;
use std::str::FromStr as _;
use std::sync::Arc;

use adk_rust::graph::interrupt::GraphInterruptPayload;
use adk_rust::graph::{Checkpoint, Checkpointer, MemoryCheckpointer};
use adk_rust::runner::Runner;
use adk_rust::session::{
    CreateRequest, GetRequest, InMemorySessionService, Session, SessionService,
};
use adk_rust::{Content, Event, SessionId, UserId};
use serde_json::{Map, Value, json};

use super::EliteaGraphAgent;
use super::compiler::PipelineDefinition;
use super::resume::PipelineResume;
use super::static_pause::{PipelineTextContinuation, STATIC_PAUSE_METADATA_KEY};
use crate::agents::request::{AgentExecutionPayload, NextInputSuggestionPolicy, UserInput};
use crate::agents::runtime::NativeAgentInvocation;
use crate::state::postgres_session_tests::{IsolatedPostgres, authority_for, install_schema};
use crate::state::{
    CheckpointLimits, CheckpointWriterAuthority, PostgresCheckpointer, PostgresSessionService,
    SessionLimits, TestStateWriterLease,
};

const APP: &str = "elitea-agent-v1";
const USER: &str = "user-1";
const SESSION: &str = "session-1";
const ROOT: &str = "pipeline-root";
const CHILD_TEST: &str = "agents::graph::shaping_pg_tests::postgres_shaping_replay_child";
const DATABASE_ENV: &str = "ELITEA_SHAPING_PG_TEST_DATABASE";
const PHASE_ENV: &str = "ELITEA_SHAPING_PG_TEST_PHASE";
const REFERENCE_ENV: &str = "ELITEA_SHAPING_PG_TEST_REFERENCE";
const PAUSE_ENV: &str = "ELITEA_SHAPING_PG_TEST_PAUSE";
const PAUSE_MARKER: &str = "SHAPING_PG_PAUSE=";
const CHECKPOINT_MIGRATION: &str =
    include_str!("../../../../elitea-main/migrations/agentstate/0001_agent_graph_checkpoints.sql");
// `enrich` re-parses the rendered list, so the fixture holds only values the template engine
// renders as JSON (it renders null and booleans as `none` and `True`).
const ORDERS: &str = r#"[{"id": "A", "items": [{"sku": "x", "qty": 2}, {"sku": "y", "qty": 1.5}]}, {"id": "B", "items": ["text", 3, "z"]}]"#;
const NODES: &str = r"entry_point: split
nodes:
  - id: split
    type: split_out
    source: orders
    output: [lines]
    split: {mode: rows_field, path: /items}
    destination: items
    retain: {mode: all}
    transition: enrich
  - id: enrich
    type: state_modifier
    template: '{{ lines }}'
    input: [lines]
    output: [lines]
    transition: join
  - id: join
    type: aggregate
    source: lines
    output: [orders_out]
    layout: split_out
    regroup: parent
    operations: [{operation: collect, field: {path: /items}, output: items}]
    transition: END
";

fn pipeline(paused: bool) -> PipelineDefinition {
    let pause = if paused {
        "interrupt_before: [enrich]\n"
    } else {
        ""
    };
    PipelineDefinition::from_yaml(&format!(
        "{pause}state:\n  orders: {{type: list, value: {ORDERS}}}\n  lines: {{type: list, value: []}}\n  orders_out: {{type: list, value: []}}\n{NODES}"
    ))
    .expect("shaping replay pipeline")
}

fn orders() -> Value {
    serde_json::from_str(ORDERS).expect("orders fixture")
}

fn bytes(value: &Value) -> Vec<u8> {
    serde_json::to_vec(value).expect("canonical JSON bytes")
}

async fn run(
    definition: &PipelineDefinition,
    checkpointer: Arc<dyn Checkpointer>,
    sessions: Arc<dyn SessionService>,
    resume: Option<PipelineResume>,
) -> Vec<Event> {
    let graph = definition
        .compile(ROOT, checkpointer.clone(), resume)
        .expect("compile shaping replay pipeline");
    let agent = EliteaGraphAgent::new(graph)
        .with_printer_interrupts(checkpointer.clone(), definition.printer_pause_catalog())
        .with_static_interrupts(checkpointer, definition.static_pause_catalog());
    let runner = Runner::builder()
        .app_name(APP)
        .agent(Arc::new(agent))
        .session_service(sessions)
        .build()
        .expect("build runner");
    let mut invocation = NativeAgentInvocation::new(
        runner,
        UserId::new(USER).expect("user id"),
        SessionId::new(SESSION).expect("session id"),
        Content::new("user").with_text("continue"),
    )
    .start()
    .expect("start invocation");
    let mut events = Vec::new();
    while let Some(event) = invocation.next_event().await.expect("pipeline event") {
        events.push(event);
    }
    events
}

async fn create_session(sessions: &dyn SessionService) {
    sessions
        .create(CreateRequest {
            app_name: APP.to_owned(),
            user_id: USER.to_owned(),
            session_id: Some(SESSION.to_owned()),
            state: HashMap::new(),
        })
        .await
        .expect("create session");
}

async fn get_session(sessions: &dyn SessionService) -> Box<dyn Session> {
    sessions
        .get(GetRequest {
            app_name: APP.to_owned(),
            user_id: USER.to_owned(),
            session_id: SESSION.to_owned(),
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("load session")
}

/// Builds the browser resume request bound to the latest public static pause in the session.
fn bound_resume_payload(session: &dyn Session) -> AgentExecutionPayload {
    let event = session.events().all().last().cloned().expect("pause event");
    let binding = crate::agents::events::pipeline_static_event_binding(&event, ROOT, SESSION)
        .expect("static pause binding");
    let proof = binding.public_proof(&event).expect("public pause proof");
    let mut meta = Map::new();
    meta.insert(
        "pipeline_static_resume_v1".to_owned(),
        json!({"revision": 1, "pause_id": proof["pause_id"]}),
    );
    AgentExecutionPayload {
        llm: Map::new(),
        chat_history: Vec::new(),
        user_input: UserInput::Text("continue".to_owned()),
        thread_id: Some(SESSION.to_owned()),
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
        meta,
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

async fn continuation(
    definition: &PipelineDefinition,
    checkpointer: &dyn Checkpointer,
    sessions: &dyn SessionService,
) -> PipelineResume {
    let session = get_session(sessions).await;
    PipelineTextContinuation::from_payload(&bound_resume_payload(session.as_ref()))
        .expect("static continuation payload")
        .resolve(
            session.as_ref(),
            checkpointer,
            ROOT,
            SESSION,
            &definition.printer_pause_catalog(),
            &definition.static_pause_catalog(),
            "continue",
        )
        .await
        .expect("resolve static continuation")
}

/// Uninterrupted in-memory run of the same nodes; returns `{lines, orders_out}`.
async fn reference() -> Value {
    let definition = pipeline(false);
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let sessions = Arc::new(InMemorySessionService::new());
    create_session(sessions.as_ref()).await;
    let events = run(&definition, checkpointer.clone(), sessions, None).await;
    assert!(events.iter().all(|event| {
        !event
            .provider_metadata
            .contains_key(STATIC_PAUSE_METADATA_KEY)
    }));
    let state = checkpointer
        .load(SESSION)
        .await
        .expect("load reference checkpoint")
        .expect("reference checkpoint")
        .state;
    assert_eq!(state["orders_out"], orders());
    assert_eq!(state["lines"].as_array().map(Vec::len), Some(5));
    json!({"lines": state["lines"], "orders_out": state["orders_out"]})
}

#[tokio::test]
async fn postgres_shaping_pause_replays_across_process_replacement() {
    let Ok(url) = std::env::var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the shaping PostgreSQL replay proof");
        return;
    };
    let reference = serde_json::to_string(&reference().await).expect("reference JSON");
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let mut pause = String::new();
    for phase in ["write", "read"] {
        let output = std::process::Command::new(std::env::current_exe().expect("test binary"))
            .args(["--exact", CHILD_TEST, "--nocapture", "--test-threads=1"])
            .env(DATABASE_ENV, &db.database_name)
            .env(PHASE_ENV, phase)
            .env(REFERENCE_ENV, &reference)
            .env(PAUSE_ENV, &pause)
            .output()
            .expect("spawn child test process");
        let stdout = String::from_utf8_lossy(&output.stdout);
        assert!(
            output.status.success(),
            "{phase} process failed: {stdout} {}",
            String::from_utf8_lossy(&output.stderr)
        );
        assert!(
            stdout.contains("1 passed"),
            "{phase} process did not run the child: {stdout}"
        );
        if phase == "write" {
            pause = stdout
                .lines()
                .find_map(|line| line.split_once(PAUSE_MARKER).map(|(_, value)| value))
                .expect("write process reports its pause checkpoint")
                .to_owned();
        }
    }
    db.pool.close().await;
}

struct Services {
    pool: sqlx::PgPool,
    sessions: Arc<PostgresSessionService>,
    checkpointer: Arc<dyn Checkpointer>,
}

/// Activates the writer pair exactly like `activate_pipeline_postgres` for one claim attempt.
async fn activate(database: &str, attempt: u64, definition: &PipelineDefinition) -> Services {
    let options = sqlx::postgres::PgConnectOptions::from_str(
        &std::env::var("ELITEA_TEST_DATABASE_URL").expect("database URL"),
    )
    .expect("parse database URL")
    .database(database);
    let pool = sqlx::postgres::PgPoolOptions::new()
        .max_connections(2)
        .connect_with(options)
        .await
        .expect("connect isolated database");
    let fence = [u8::try_from(attempt).expect("attempt"); 32];
    let lease = Arc::new(TestStateWriterLease::current());
    let sessions = PostgresSessionService::activate(
        pool.clone(),
        authority_for(&format!("claim-{attempt}"), attempt, attempt, fence),
        SessionLimits::default(),
        lease.clone(),
    )
    .await
    .expect("activate session writer");
    let writer = CheckpointWriterAuthority::new(
        "tenant-1".to_owned(),
        1,
        2,
        "agent.execute.application.v1",
        definition.definition_digest(),
        SESSION.to_owned(),
        "execution-1".to_owned(),
        1,
        format!("claim-{attempt}"),
        attempt,
        attempt,
        1_700_000_000_000_000 + i64::try_from(attempt).expect("attempt") * 1_000_000,
        "workload-1".to_owned(),
        "producer-1".to_owned(),
        fence,
    )
    .expect("checkpoint writer authority");
    let checkpointer =
        PostgresCheckpointer::activate(pool.clone(), writer, CheckpointLimits::default(), lease)
            .await
            .expect("activate checkpoint writer")
            .with_application_paths(&[])
            .await
            .expect("activate checkpoint family");
    Services {
        pool,
        sessions: Arc::new(sessions),
        checkpointer: Arc::new(checkpointer),
    }
}

async fn history(checkpointer: &dyn Checkpointer) -> Vec<Checkpoint> {
    checkpointer
        .list(SESSION)
        .await
        .expect("list checkpoint history")
}

#[tokio::test]
async fn postgres_shaping_replay_child() {
    let Ok(database) = std::env::var(DATABASE_ENV) else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    let reference: Value = serde_json::from_str(&std::env::var(REFERENCE_ENV).expect("reference"))
        .expect("reference JSON");
    let definition = pipeline(true);
    match std::env::var(PHASE_ENV).expect("phase").as_str() {
        "write" => write_phase(&database, &definition, &reference).await,
        "read" => {
            let pause = std::env::var(PAUSE_ENV).expect("pause checkpoint");
            read_phase(&database, &definition, &reference, &pause).await;
        }
        other => panic!("unknown phase {other}"),
    }
}

async fn write_phase(database: &str, definition: &PipelineDefinition, reference: &Value) {
    let services = activate(database, 1, definition).await;
    create_session(services.sessions.as_ref()).await;
    let events = run(
        definition,
        services.checkpointer.clone(),
        services.sessions.clone(),
        None,
    )
    .await;
    let last = events.last().expect("pause event");
    assert!(
        last.provider_metadata
            .contains_key(STATIC_PAUSE_METADATA_KEY)
    );
    assert_eq!(
        GraphInterruptPayload::from_event(last)
            .expect("interrupt payload")
            .kind,
        "before"
    );
    let checkpoints = history(services.checkpointer.as_ref()).await;
    let paused = checkpoints.last().expect("pause checkpoint");
    assert_eq!(paused.pending_nodes, ["enrich"]);
    assert_eq!(bytes(&paused.state["lines"]), bytes(&reference["lines"]));
    assert_eq!(paused.state["orders"], orders());
    for checkpoint in &checkpoints {
        assert_eq!(checkpoint.state["orders_out"], json!([]));
    }
    println!(
        "{PAUSE_MARKER}{}:{}",
        paused.checkpoint_id,
        checkpoints.len()
    );
    services.pool.close().await;
}

async fn read_phase(
    database: &str,
    definition: &PipelineDefinition,
    reference: &Value,
    pause: &str,
) {
    let (pause_id, pause_count) = pause.split_once(':').expect("pause marker");
    let pause_count: usize = pause_count.parse().expect("pause checkpoint count");
    let services = activate(database, 2, definition).await;
    let before = history(services.checkpointer.as_ref()).await;
    assert_eq!(before.len(), pause_count);
    let paused = before.last().expect("pause checkpoint").clone();
    assert_eq!(paused.checkpoint_id, pause_id);
    assert_eq!(paused.pending_nodes, ["enrich"]);
    let resume = continuation(
        definition,
        services.checkpointer.as_ref(),
        services.sessions.as_ref(),
    )
    .await;
    let events = run(
        definition,
        services.checkpointer.clone(),
        services.sessions.clone(),
        Some(resume),
    )
    .await;
    assert!(events.iter().all(|event| {
        !event
            .provider_metadata
            .contains_key(STATIC_PAUSE_METADATA_KEY)
    }));
    let after = history(services.checkpointer.as_ref()).await;
    assert_replayed_once(&before, &after, &paused, reference);
    services.pool.close().await;
}

/// Proves the replacement process ran only `enrich` and `join` and wrote `orders_out` once.
///
/// The executor checkpoints after every super-step, so a re-run of `split` (whose only
/// transition is `enrich`) would leave a post-pause checkpoint whose frontier is `[enrich]`.
fn assert_replayed_once(
    before: &[Checkpoint],
    after: &[Checkpoint],
    paused: &Checkpoint,
    reference: &Value,
) {
    let ids = |checkpoints: &[Checkpoint]| {
        checkpoints
            .iter()
            .map(|checkpoint| checkpoint.checkpoint_id.clone())
            .collect::<Vec<_>>()
    };
    assert_eq!(ids(&after[..before.len()]), ids(before));
    let resumed = &after[before.len()..];
    let frontiers = resumed
        .iter()
        .map(|checkpoint| checkpoint.pending_nodes.clone())
        .collect::<Vec<_>>();
    assert_eq!(frontiers, [vec!["join".to_owned()], Vec::new()]);
    let mut step = paused.step;
    for checkpoint in resumed {
        assert!(checkpoint.step > step, "post-resume steps must advance");
        step = checkpoint.step;
        assert_eq!(
            bytes(&checkpoint.state["lines"]),
            bytes(&paused.state["lines"])
        );
        assert_eq!(checkpoint.state["orders"], orders());
    }
    let last = resumed.last().expect("terminal checkpoint");
    let output = &last.state["orders_out"];
    assert_eq!(bytes(output), bytes(&reference["orders_out"]));
    assert_eq!(output, &orders());
    let mut previous = json!([]);
    let mut writes = 0;
    for checkpoint in after {
        let current = &checkpoint.state["orders_out"];
        if current != &previous {
            writes += 1;
            assert_eq!(current, output);
            previous = current.clone();
        }
    }
    assert_eq!(
        writes, 1,
        "orders_out must change exactly once in the history"
    );
}
