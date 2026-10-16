//! PostgreSQL-backed pipeline runs shared by process-replacement tests.
//!
//! One isolated database per parent test; child processes of the test binary
//! activate the session and checkpoint writer pair exactly like
//! `activate_pipeline_postgres` for one claim attempt, run the pipeline through
//! the production runner, and resume through the production text continuation.

use std::collections::HashMap;
use std::str::FromStr as _;
use std::sync::Arc;

use adk_rust::graph::{Checkpoint, Checkpointer};
use adk_rust::runner::Runner;
use adk_rust::session::{CreateRequest, GetRequest, Session, SessionService};
use adk_rust::{Content, Event, SessionId, UserId};
use serde_json::{Map, json};

use super::EliteaGraphAgent;
use super::compiler::PipelineDefinition;
use super::resume::PipelineResume;
use super::static_pause::PipelineTextContinuation;
use crate::agents::request::{AgentExecutionPayload, NextInputSuggestionPolicy, UserInput};
use crate::agents::runtime::NativeAgentInvocation;
use crate::state::postgres_session_tests::authority_for;
use crate::state::{
    CheckpointLimits, CheckpointWriterAuthority, PostgresCheckpointer, PostgresSessionService,
    SessionLimits, TestStateWriterLease,
};

pub(super) const APP: &str = "elitea-agent-v1";
pub(super) const USER: &str = "user-1";
pub(super) const SESSION: &str = "session-1";
pub(super) const ROOT: &str = "pipeline-root";
pub(super) const CHECKPOINT_MIGRATION: &str =
    include_str!("../../../../elitea-main/migrations/agentstate/0001_agent_graph_checkpoints.sql");

pub(super) async fn run(
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

pub(super) async fn create_session(sessions: &dyn SessionService) {
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

pub(super) async fn continuation(
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

pub(super) struct Services {
    pub(super) pool: sqlx::PgPool,
    pub(super) sessions: Arc<PostgresSessionService>,
    pub(super) checkpointer: Arc<dyn Checkpointer>,
}

/// Activates the writer pair exactly like `activate_pipeline_postgres` for one claim attempt.
pub(super) async fn activate(
    database: &str,
    attempt: u64,
    definition: &PipelineDefinition,
) -> Services {
    let options = sqlx::postgres::PgConnectOptions::from_str(
        &crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL").expect("database URL"),
    )
    .expect("parse database URL")
    .database(database);
    let pool = crate::test_pg::pool_options()
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

pub(super) async fn history(checkpointer: &dyn Checkpointer) -> Vec<Checkpoint> {
    checkpointer
        .list(SESSION)
        .await
        .expect("list checkpoint history")
}
