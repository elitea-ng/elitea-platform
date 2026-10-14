//! A pipeline with a `map` node, assembled by the production
//! `PipelineNativeAgentAssembler::postgres` path against real `PostgreSQL`.
//!
//! The Map node's parent turn authority, item threads and deadline come from the
//! claim-fenced checkpoint holder `activate_pipeline_postgres` builds, so these
//! tests cover the composition the in-memory graph tests cannot see:
//!
//! - a fresh turn runs every item and collects them in input order;
//! - a continuation (a new execution) after a later `hitl` pause does not run the
//!   items again, and reads the collected destination from the paused frontier;
//! - a second turn on the same conversation runs its own items under its own
//!   execution instead of reusing the first turn's receipts;
//! - an already-expired deadline refuses the Map node before any item starts.

use super::*;
use crate::protocol::control::test_session_authority_for_claim;
use crate::state::postgres_session_tests::{IsolatedPostgres, install_schema};
use crate::state::{CheckpointLimits, SessionLimits, TestStateWriterLease};

const GENERATION: u64 = 3;
const CHECKPOINT_MIGRATION: &str =
    include_str!("../../../elitea-main/migrations/agentstate/0001_agent_graph_checkpoints.sql");

const MAP_THEN_REVIEW: &str = r#"
state:
  items:
    type: list
    value: ["alpha", "beta", "gamma"]
  tagged:
    type: str
    value: ""
  mapped:
    type: list
    value: []
  final_text:
    type: str
    value: ""
entry_point: fan_out
nodes:
  - id: fan_out
    type: map
    worker: tag
    source: items
    item: entity
    index: position
    outputs: [tagged]
    destination: mapped
    max_items: 8
    max_concurrency: 2
    reduction: ordered_collection
    transition: review
  - id: tag
    type: state_modifier
    template: "{{ entity }}#{{ position }}"
    input: [entity, position]
    output: [tagged]
  - id: review
    type: hitl
    user_message:
      type: fixed
      value: Review the tagged items.
    routes:
      approve: finish
      reject: END
  - id: finish
    type: state_modifier
    template: "done"
    input: []
    output: [final_text]
    transition: END
"#;

const MAP_ONLY: &str = r#"
state:
  items:
    type: list
    value: ["one", "two"]
  tagged:
    type: str
    value: ""
  mapped:
    type: list
    value: []
  final_text:
    type: str
    value: ""
entry_point: fan_out
nodes:
  - id: fan_out
    type: map
    worker: tag
    source: items
    item: entity
    index: position
    outputs: [tagged]
    destination: mapped
    max_items: 8
    max_concurrency: 2
    reduction: ordered_collection
    transition: finish
  - id: tag
    type: state_modifier
    template: "{{ entity }}#{{ position }}"
    input: [entity, position]
    output: [tagged]
  - id: finish
    type: state_modifier
    template: "ok"
    input: []
    output: [final_text]
    transition: END
"#;

fn request_for(
    pipeline: &str,
    digest_byte: u8,
    decisions: Option<&Value>,
) -> super::super::request::AgentExecutionRequest {
    let mut request = pipeline_request();
    request.payload.application["version_details"]["instructions"] = json!(pipeline);
    request.binding.request_content_digest = [digest_byte; 32];
    if let Some(card) = decisions {
        request.payload.should_continue = true;
        request.payload.hitl_resume = true;
        request.payload.hitl_action = Some("approve".to_owned());
        request.payload.hitl_value = Some(String::new());
        request.payload.hitl_decisions = vec![json!({
            "interrupt_id": card["interrupt_id"],
            "tool_call_id": card["tool_call_id"].as_str().unwrap_or_default(),
            "action": "approve",
            "value": ""
        })];
        request.payload.user_input = super::super::request::UserInput::Text(
            "this transport marker must not become resumed graph input".to_owned(),
        );
    }
    request
}

fn authorized_turn<'a>(
    request: &'a super::super::request::AgentExecutionRequest,
    execution_id: &str,
    claim_ordinal: u8,
    deadline_unix_millis: Option<i64>,
) -> AuthorizedNativeAssembly<'a> {
    let mut command = AuthorizedNativeCommandBinding::fixture_for_execution(execution_id);
    if let Some(deadline) = deadline_unix_millis {
        command = command.with_deadline(deadline);
    }
    AuthorizedNativeAssembly::from_authorized(
        request,
        test_runtime_context_authority(),
        test_session_authority_for_claim(execution_id, GENERATION, claim_ordinal),
        Arc::new(TestStateWriterLease::current()),
        command,
    )
}

async fn database() -> Option<IsolatedPostgres> {
    let Ok(url) = std::env::var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the Map PostgreSQL proof");
        return None;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    Some(db)
}

fn assembler(pool: &sqlx::PgPool) -> PipelineNativeAgentAssembler {
    let ((platform, model_facade, _, _), _) = pipeline_runtime_cycles_with_capture(
        &json!({
            "agent_type": "agent", "instructions": "unused", "variables": [], "tools": [],
            "llm_settings": {"model_name": "unused-model", "model_project_id": 23,
                "max_tokens": 16, "openai_compatible": true}
        }),
        Vec::new(),
        1,
    );
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

/// Every root-conversation checkpoint of the pipeline, latest last.
async fn root_states(pool: &sqlx::PgPool) -> Vec<Value> {
    sqlx::query_scalar::<_, String>(
        "SELECT state FROM elitea_runtime.agent_graph_checkpoints \
         WHERE state LIKE '%\"mapped\"%' ORDER BY 1",
    )
    .fetch_all(pool)
    .await
    .expect("read pipeline checkpoints")
    .iter()
    .map(|state| serde_json::from_str(state).expect("checkpoint state JSON"))
    .collect()
}

async fn checkpoint_count(pool: &sqlx::PgPool) -> i64 {
    sqlx::query_scalar::<_, i64>("SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints")
        .fetch_one(pool)
        .await
        .expect("count checkpoints")
}

fn expected(items: &[&str]) -> Value {
    Value::Array(
        items
            .iter()
            .enumerate()
            .map(|(index, item)| json!({"index": index, "outputs": {"tagged": format!("{item}#{index}")}}))
            .collect(),
    )
}

fn has_mapped(states: &[Value], want: &Value) -> bool {
    states.iter().any(|state| &state["mapped"] == want)
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_map_runs_every_item_in_order_and_a_continuation_does_not_rerun_them() {
    let Some(db) = database().await else { return };
    let assembler = assembler(&db.pool);

    let first = request_for(MAP_THEN_REVIEW, 31, None);
    let invocation = assembler
        .assemble(authorized_turn(&first, "execution-map-1", 1, None))
        .await
        .expect("assemble the Map pipeline");
    let (interrupts, _) = collect_pipeline_pause(invocation).await;
    assert_eq!(
        interrupts.len(),
        1,
        "the review node pauses: {interrupts:?}"
    );
    let card = interrupts[0]["response_metadata"]["hitl_interrupts"][0].clone();

    let want = expected(&["alpha", "beta", "gamma"]);
    let paused = root_states(&db.pool).await;
    assert!(
        has_mapped(&paused, &want),
        "items collected in input order before the pause: {paused:?}"
    );
    let after_first = checkpoint_count(&db.pool).await;

    // A continuation is a new execution on the same conversation.
    let resume = request_for(MAP_THEN_REVIEW, 32, Some(&card));
    let invocation = assembler
        .assemble(authorized_turn(&resume, "execution-map-2", 2, None))
        .await
        .expect("assemble the continuation of a Map pipeline");
    let browser = collect_pipeline_completion(invocation).await;
    assert!(
        !browser.is_empty(),
        "the continuation completes: {browser:?}"
    );
    let finished = root_states(&db.pool).await;
    assert!(
        finished
            .iter()
            .any(|state| state["final_text"] == "done" && state["mapped"] == want),
        "the continuation finishes with the original collection: {finished:?}"
    );
    // The continuation added only the review/finish steps: no item thread was
    // opened again (item checkpoints carry a Map thread key).
    let item_threads = sqlx::query_scalar::<_, i64>(
        "SELECT count(DISTINCT thread_id) FROM elitea_runtime.agent_graph_checkpoints \
         WHERE thread_id LIKE 'm1:%'",
    )
    .fetch_one(&db.pool)
    .await
    .expect("count item threads");
    assert_eq!(
        item_threads, 3,
        "one thread per item, none after the resume"
    );
    assert!(checkpoint_count(&db.pool).await > after_first);
    db.pool.close().await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_map_second_turn_runs_its_own_items() {
    let Some(db) = database().await else { return };
    let assembler = assembler(&db.pool);
    for (execution, digest, claim) in [("execution-turn-1", 41, 1), ("execution-turn-2", 42, 2)] {
        let request = request_for(MAP_ONLY, digest, None);
        let invocation = assembler
            .assemble(authorized_turn(&request, execution, claim, None))
            .await
            .expect("assemble the Map turn");
        let browser = collect_pipeline_completion(invocation).await;
        assert!(!browser.is_empty(), "{execution} completes: {browser:?}");
    }
    let want = expected(&["one", "two"]);
    let states = root_states(&db.pool).await;
    assert!(has_mapped(&states, &want), "{states:?}");
    let item_threads = sqlx::query_scalar::<_, i64>(
        "SELECT count(DISTINCT thread_id) FROM elitea_runtime.agent_graph_checkpoints \
         WHERE thread_id LIKE 'm1:%'",
    )
    .fetch_one(&db.pool)
    .await
    .expect("count item threads");
    assert_eq!(
        item_threads, 4,
        "each turn owns its own two item threads under its own execution"
    );
    db.pool.close().await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_map_with_an_expired_deadline_refuses_before_any_item_starts() {
    let Some(db) = database().await else { return };
    let assembler = assembler(&db.pool);
    let request = request_for(MAP_ONLY, 51, None);
    let refused = assembler
        .assemble(authorized_turn(&request, "execution-expired", 1, Some(1)))
        .await;
    assert!(
        refused.is_err(),
        "an expired execution deadline must refuse the Map pipeline"
    );
    assert_eq!(checkpoint_count(&db.pool).await, 0);
    db.pool.close().await;
}

/// A Map node runs only in the pipeline a conversation starts. A saved pipeline
/// that an Agent node calls is refused while the stored tree is bound, with the
/// capability named, rather than failing later as an unavailable participant.
#[tokio::test]
async fn a_saved_pipeline_with_a_map_node_is_refused_where_an_agent_node_calls_it() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let (platform, model_facade, _calls, _paths) = pipeline_runtime(
        &json!({
            "agent_type": "pipeline",
            "instructions": MAP_ONLY,
            "meta": {}, "variables": [], "tools": [], "llm_settings": null
        }),
        Vec::new(),
    );
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::clone(&sessions),
        Arc::clone(&checkpointer) as Arc<dyn Checkpointer>,
    )
    .with_runtime_clients(platform, model_facade);
    let request = agent_pipeline_request("release-agent", "pipeline");
    let Err(error) = assembler.assemble(authorized(&request)).await else {
        panic!("a Map pipeline cannot be called by an Agent node");
    };
    assert!(
        error.to_string().contains("has a Map node"),
        "the refusal names the Map capability: {error}"
    );
}
