//! Two sequential nested `agent` nodes that call the same saved agent, each pausing on
//! `ask_user`, resumed across Worker process replacement against real `PostgreSQL`.
//!
//! The nested application tool names every child invocation from a process-global counter, so a
//! replacement Worker restarts that counter. The counter is a process static, so each phase here
//! runs in its own OS process (same pattern as `graph::shaping_pg_tests`):
//!
//! 1. `start`: node `override_call` runs its child, which pauses on `ask_user` (card 1).
//! 2. `answer_first`: a replacement Worker process answers card 1. Node `override_call` completes
//!    and node `defaults_call` runs its child, which pauses (card 2).
//! 3. `answer_second`: another replacement Worker process answers card 2. The pipeline must
//!    complete from the persisted history of all earlier processes.
//!
//! As in the browser, every answer is its own execution with its own claim on the same session.
//! The assembler is the production `PipelineNativeAgentAssembler::postgres` path, so session and
//! checkpoint writers are activated by `activate_pipeline_postgres` from that claim.

use std::str::FromStr as _;

use super::*;
use crate::protocol::control::test_session_authority_for_claim;
use crate::state::postgres_session_tests::{IsolatedPostgres, install_schema};
use crate::state::{CheckpointLimits, SessionLimits, TestStateWriterLease};

const CHILD_TEST: &str = "agents::pipeline_tests::restart_pg_tests::postgres_nested_restart_child";
const DATABASE_ENV: &str = "ELITEA_NESTED_RESTART_PG_DATABASE";
const PHASE_ENV: &str = "ELITEA_NESTED_RESTART_PG_PHASE";
const PENDING_ENV: &str = "ELITEA_NESTED_RESTART_PG_PENDING";
const PENDING_MARKER: &str = "NESTED_RESTART_PENDING=";
const GENERATION: u64 = 3;
const CHECKPOINT_MIGRATION: &str =
    include_str!("../../../elitea-main/migrations/agentstate/0001_agent_graph_checkpoints.sql");

const FIRST_CHILD_ANSWER: &str = "first child answer";
const SECOND_CHILD_ANSWER: &str = "second child answer";

const TWO_NODE_PIPELINE: &str = r"state:
  answer: str
  second_answer: str
  messages: list
entry_point: override_call
nodes:
  - id: override_call
    type: agent
    tool: release-agent
    input_mapping:
      task: {type: fixed, value: 'Summarize the override release'}
    output: [answer, messages]
    transition: defaults_call
  - id: defaults_call
    type: agent
    tool: release-agent
    input_mapping:
      task: {type: fixed, value: 'Summarize the defaults release'}
    output: [second_answer, messages]
    transition: END
";

/// A real provider returns a distinct tool-call id for every `ask_user` call.
fn ask_user_call_response(call_id: &str) -> http::Response<tonic::body::Body> {
    let raw = format!(
        "data: {{\"choices\":[{{\"delta\":{{\"tool_calls\":[{{\"index\":0,\"id\":{call_id:?},\"type\":\"function\",\"function\":{{\"name\":\"ask_user\",\"arguments\":\"{{\\\"questions\\\":[{{\\\"question\\\":\\\"Which environment should I use?\\\",\\\"header\\\":\\\"Environment\\\",\\\"options\\\":[{{\\\"label\\\":\\\"Staging\\\"}},{{\\\"label\\\":\\\"Production\\\"}}]}}]}}\"}}}}]}},\"finish_reason\":null}}]}}\n\ndata: {{\"choices\":[{{\"delta\":{{}},\"finish_reason\":\"tool_calls\"}}]}}\n\ndata: {{\"choices\":[],\"usage\":{{\"prompt_tokens\":3,\"completion_tokens\":2}}}}\n\ndata: [DONE]\n\n"
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn two_node_request() -> super::super::request::AgentExecutionRequest {
    let mut request = agent_pipeline_request("release-agent", "agent");
    request.payload.application["version_details"]["instructions"] = json!(TWO_NODE_PIPELINE);
    request
}

fn answer_request(
    pending: &Value,
    digest_byte: u8,
) -> super::super::request::AgentExecutionRequest {
    let answer = r#"{"q1":"Staging"}"#;
    let mut request = two_node_request();
    request.binding.request_content_digest = [digest_byte; 32];
    request.payload.should_continue = true;
    request.payload.hitl_resume = true;
    request.payload.hitl_action = Some("answer".to_owned());
    request.payload.hitl_value = Some(answer.to_owned());
    request.payload.hitl_decisions = vec![json!({
        "interrupt_id": pending["interrupt_id"], "tool_call_id": pending["tool_call_id"],
        "action": "answer", "value": answer
    })];
    request
}

/// The authorized assembly of one turn in its own Worker process: a new execution and claim.
fn authorized_turn<'a>(
    request: &'a super::super::request::AgentExecutionRequest,
    execution_id: &str,
    claim_ordinal: u8,
) -> AuthorizedNativeAssembly<'a> {
    AuthorizedNativeAssembly::from_authorized(
        request,
        test_runtime_context_authority(),
        test_session_authority_for_claim(execution_id, GENERATION, claim_ordinal),
        Arc::new(TestStateWriterLease::current()),
        AuthorizedNativeCommandBinding::fixture_for_execution(execution_id),
    )
}

async fn connect(database: &str) -> sqlx::PgPool {
    let options = sqlx::postgres::PgConnectOptions::from_str(
        &std::env::var("ELITEA_TEST_DATABASE_URL").expect("database URL"),
    )
    .expect("parse database URL")
    .database(database);
    sqlx::postgres::PgPoolOptions::new()
        .max_connections(4)
        .connect_with(options)
        .await
        .expect("connect isolated database")
}

/// One saved-agent child version plus the fake model script for this process.
fn process_runtime(
    outcomes: Vec<TestModelGatewayOutcome>,
) -> (PipelineChildRuntime, CapturedModelRequests) {
    pipeline_runtime_cycles_with_capture(
        &json!({
            "agent_type": "agent", "instructions": "Resolve the release.",
            "variables": [],
            "meta": {"internal_tools": ["ask_user"]}, "tools": [],
            "llm_settings": {"model_name": "child-model", "model_project_id": 23,
                "max_tokens": 2048, "openai_compatible": true}
        }),
        outcomes,
        6,
    )
}

fn pending_card(interrupts: &[Value]) -> Value {
    assert_eq!(
        interrupts.len(),
        1,
        "exactly one pending ask_user card: {interrupts:?}"
    );
    interrupts[0]["response_metadata"]["hitl_interrupts"][0].clone()
}

fn model_requests(captured: &CapturedModelRequests) -> usize {
    captured.lock().expect("captured model requests").len()
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_two_nested_agent_pauses_resume_across_process_replacement() {
    let Ok(url) = std::env::var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!(
            "SKIP: set ELITEA_TEST_DATABASE_URL for the nested application restart PostgreSQL proof"
        );
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let mut pending = String::new();
    let mut cards = Vec::new();
    for phase in ["start", "answer_first", "answer_second"] {
        let output = std::process::Command::new(std::env::current_exe().expect("test binary"))
            .args(["--exact", CHILD_TEST, "--nocapture", "--test-threads=1"])
            .env(DATABASE_ENV, &db.database_name)
            .env(PHASE_ENV, phase)
            .env(PENDING_ENV, &pending)
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
        if phase != "answer_second" {
            pending = stdout
                .lines()
                .find_map(|line| line.split_once(PENDING_MARKER).map(|(_, value)| value))
                .unwrap_or_else(|| panic!("{phase} process reports its pending card: {stdout}"))
                .to_owned();
            cards.push(serde_json::from_str::<Value>(&pending).expect("pending card JSON"));
        }
    }
    assert_ne!(
        cards[0]["interrupt_id"], cards[1]["interrupt_id"],
        "the second pause is a new card"
    );
    assert_ne!(cards[0]["tool_call_id"], cards[1]["tool_call_id"]);
    assert_distinct_child_invocations(&db.pool).await;
    db.pool.close().await;
}

/// The child activations that wrote to the persisted session need distinct invocation ids:
/// the original node-1 child, the resumed node-1 child, node-2's child and its resumed child.
async fn assert_distinct_child_invocations(pool: &sqlx::PgPool) {
    let payloads = sqlx::query_scalar::<_, String>(
        "SELECT event_payload FROM elitea_runtime.agent_session_events ORDER BY event_ordinal",
    )
    .fetch_all(pool)
    .await
    .expect("read persisted session events");
    let mut activations = Vec::new();
    for payload in &payloads {
        let event: Value = serde_json::from_str(payload).expect("persisted event JSON");
        let invocation = event["invocation_id"].as_str().unwrap_or_default();
        if invocation.starts_with("elitea-child-") && !activations.contains(&invocation.to_owned())
        {
            activations.push(invocation.to_owned());
        }
    }
    eprintln!("child invocation ids in persisted order: {activations:?}");
    assert_eq!(
        activations.len(),
        4,
        "original and resumed node-1 child plus node-2 child and its resume must carry four \
         distinct invocation ids, got {activations:?}"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_nested_restart_child() {
    let Ok(database) = std::env::var(DATABASE_ENV) else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    let pending = std::env::var(PENDING_ENV).unwrap_or_default();
    let pool = connect(&database).await;
    match std::env::var(PHASE_ENV).expect("phase").as_str() {
        "start" => start_phase(&pool).await,
        "answer_first" => answer_first_phase(&pool, &pending).await,
        "answer_second" => answer_second_phase(&pool, &pending).await,
        other => panic!("unknown phase {other}"),
    }
    pool.close().await;
}

/// A fresh Worker process: new pool users, platform client, model facade and assembler.
fn assembler(
    pool: &sqlx::PgPool,
    platform: Arc<PlatformClient>,
    model_facade: Arc<ModelFacade>,
) -> PipelineNativeAgentAssembler {
    PipelineNativeAgentAssembler::postgres(
        pool.clone(),
        SessionLimits::default(),
        CheckpointLimits::default(),
        Arc::new(
            ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new())
                .expect("empty tool policy"),
        ),
        platform,
        model_facade,
    )
}

async fn start_phase(pool: &sqlx::PgPool) {
    let ((platform, model_facade, _, _), captured) =
        process_runtime(vec![TestModelGatewayOutcome::Response(
            ask_user_call_response("call_ask_user_1"),
        )]);
    let assembler = assembler(pool, platform, model_facade);
    let request = two_node_request();
    let invocation = assembler
        .assemble(authorized_turn(&request, "execution-start", 1))
        .await
        .expect("start: assemble the two-node pipeline");
    let (interrupts, _) = collect_pipeline_pause(invocation).await;
    let card = pending_card(&interrupts);
    assert_eq!(model_requests(&captured), 1, "start: one child model call");
    println!("{PENDING_MARKER}{card}");
}

async fn answer_first_phase(pool: &sqlx::PgPool, pending: &str) {
    let first: Value = serde_json::from_str(pending).expect("first pending card");
    let ((platform, model_facade, _, _), captured) = process_runtime(vec![
        TestModelGatewayOutcome::Response(pipeline_text_response(FIRST_CHILD_ANSWER)),
        TestModelGatewayOutcome::Response(ask_user_call_response("call_ask_user_2")),
    ]);
    let assembler = assembler(pool, platform, model_facade);
    let request = answer_request(&first, 17);
    let invocation = assembler
        .assemble(authorized_turn(&request, "execution-answer-first", 2))
        .await
        .expect("answer_first: assemble the replacement resume");
    let (interrupts, _) = collect_pipeline_pause(invocation).await;
    let second = pending_card(&interrupts);
    assert_ne!(
        second["interrupt_id"], first["interrupt_id"],
        "node defaults_call raises a new card"
    );
    assert_eq!(
        model_requests(&captured),
        2,
        "answer_first: resumed node-1 child answer, then node-2 child pause; node 1 is not re-run"
    );
    println!("{PENDING_MARKER}{second}");
}

async fn answer_second_phase(pool: &sqlx::PgPool, pending: &str) {
    let second: Value = serde_json::from_str(pending).expect("second pending card");
    let ((platform, model_facade, _, _), captured) =
        process_runtime(vec![TestModelGatewayOutcome::Response(
            pipeline_text_response(SECOND_CHILD_ANSWER),
        )]);
    let assembler = assembler(pool, platform, model_facade);
    let request = answer_request(&second, 18);
    let invocation = assembler
        .assemble(authorized_turn(&request, "execution-answer-second", 3))
        .await
        .expect("answer_second: assemble the second replacement resume");
    let browser = collect_pipeline_completion(invocation).await;
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == SECOND_CHILD_ANSWER),
        "the second child answer reaches the browser: {browser:?}"
    );
    assert_eq!(
        model_requests(&captured),
        1,
        "answer_second: only the resumed node-2 child calls the model"
    );
    let complete = sqlx::query_scalar::<_, i64>(
        "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints \
         WHERE state LIKE '%' || $1 || '%' AND state LIKE '%' || $2 || '%'",
    )
    .bind(FIRST_CHILD_ANSWER)
    .bind(SECOND_CHILD_ANSWER)
    .fetch_one(pool)
    .await
    .expect("count checkpoints carrying both child answers");
    assert!(
        complete > 0,
        "a pipeline checkpoint carries both child answers"
    );
}
