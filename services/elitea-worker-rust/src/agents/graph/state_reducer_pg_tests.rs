//! Exactly-once accumulation of typed reducers across process replacement.
//!
//! The parent test runs two child processes of this test binary against one
//! isolated `PostgreSQL` database. The "write" process (claim attempt 1) runs a
//! Router loop that pauses after each `collect` visit and stops after the
//! second visit, mid-loop. The "read" process is a replacement writer (claim
//! attempt 2) that resumes from the durable checkpoint through the production
//! text continuation until the loop finishes. Every visit must append exactly
//! one element and add exactly one to the counter, whichever process ran it.

#![cfg(feature = "graph-extensions-rehearsal")]

use adk_rust::graph::Checkpoint;
use serde_json::{Value, json};

use super::compiler::PipelineDefinition;
use super::shaping_pg_tests::{
    CHECKPOINT_MIGRATION, activate, continuation, create_session, history, run,
};
use crate::state::postgres_session_tests::{IsolatedPostgres, install_schema};

const CHILD_TEST: &str =
    "agents::graph::state_reducer_pg_tests::postgres_reducer_replacement_child";
const DATABASE_ENV: &str = "ELITEA_REDUCER_PG_TEST_DATABASE";
const PHASE_ENV: &str = "ELITEA_REDUCER_PG_TEST_PHASE";
const VISITS: i64 = 3;
const WRITE_VISITS: i64 = 2;

fn pipeline() -> PipelineDefinition {
    PipelineDefinition::from_yaml(
        r#"
interrupt_after: [collect]
state:
  count: {type: int, value: 0, reducer: sum_int}
  findings: {type: list, value: [seed], reducer: append}
entry_point: tick
nodes:
  - id: tick
    type: state_modifier
    template: "1"
    output: [count]
    transition: collect
  - id: collect
    type: state_modifier
    template: '["visit {{ count }}"]'
    input: [count]
    output: [findings]
    transition: choose
  - id: choose
    type: router
    condition: "{{ 'tick' if count < 3 else 'END' }}"
    input: [count]
    routes: [tick, END]
    default_output: END
"#,
    )
    .expect("typed reducer replacement pipeline")
}

fn visits(count: i64) -> Value {
    let mut findings = vec![json!("seed")];
    findings.extend((1..=count).map(|visit| json!(format!("visit {visit}"))));
    Value::Array(findings)
}

#[tokio::test]
async fn postgres_typed_reducers_accumulate_once_across_process_replacement() {
    let Ok(url) = std::env::var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the typed reducer PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    for phase in ["write", "read"] {
        let output = std::process::Command::new(std::env::current_exe().expect("test binary"))
            .args(["--exact", CHILD_TEST, "--nocapture", "--test-threads=1"])
            .env(DATABASE_ENV, &db.database_name)
            .env(PHASE_ENV, phase)
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
    }
    db.pool.close().await;
}

#[tokio::test]
async fn postgres_reducer_replacement_child() {
    let Ok(database) = std::env::var(DATABASE_ENV) else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    let definition = pipeline();
    let attempt = match std::env::var(PHASE_ENV).expect("phase").as_str() {
        "write" => 1,
        "read" => 2,
        other => panic!("unknown phase {other}"),
    };
    let stop_at = if attempt == 1 { WRITE_VISITS } else { VISITS };
    let services = activate(&database, attempt, &definition).await;
    let before = history(services.checkpointer.as_ref()).await;
    if attempt == 1 {
        assert!(before.is_empty());
        create_session(services.sessions.as_ref()).await;
    } else {
        let paused = before.last().expect("durable pause checkpoint");
        assert_eq!(paused.state["findings"], visits(WRITE_VISITS));
        assert_eq!(paused.state["count"], json!(WRITE_VISITS));
    }
    // Each visit pauses once after `collect`; the bound only stops a runaway loop.
    let mut resume = attempt == 2;
    for _ in 0..=2 * VISITS {
        let continuation = if resume {
            Some(
                continuation(
                    &definition,
                    services.checkpointer.as_ref(),
                    services.sessions.as_ref(),
                )
                .await,
            )
        } else {
            None
        };
        run(
            &definition,
            services.checkpointer.clone(),
            services.sessions.clone(),
            continuation,
        )
        .await;
        let latest = history(services.checkpointer.as_ref()).await;
        let last = latest.last().expect("checkpoint");
        let count = last.state["count"].as_i64().expect("count");
        let finished = last.pending_nodes.is_empty() && count == VISITS;
        if finished || (attempt == 1 && count == WRITE_VISITS) {
            break;
        }
        resume = true;
    }
    let after = history(services.checkpointer.as_ref()).await;
    assert_eq!(
        ids(&after[..before.len()]),
        ids(&before),
        "the replacement keeps the durable history"
    );
    assert_accumulated_once(&after);
    let last = after.last().expect("checkpoint");
    assert_eq!(last.state["findings"], visits(stop_at));
    assert_eq!(last.state["count"], json!(stop_at));
    assert_eq!(last.pending_nodes.is_empty(), attempt == 2);
    services.pool.close().await;
}

fn ids(checkpoints: &[Checkpoint]) -> Vec<String> {
    checkpoints
        .iter()
        .map(|checkpoint| checkpoint.checkpoint_id.clone())
        .collect()
}

/// Each checkpoint's findings extend the previous one by at most one visit,
/// in order, and the counter never runs ahead of the appended visits.
fn assert_accumulated_once(history: &[Checkpoint]) {
    let mut previous = 0_i64;
    for checkpoint in history {
        let findings = checkpoint.state["findings"].as_array().expect("findings");
        let appended = i64::try_from(findings.len()).expect("length") - 1;
        assert!(
            appended == previous || appended == previous + 1,
            "findings advanced from {previous} to {appended} in one checkpoint"
        );
        assert_eq!(&Value::Array(findings.clone()), &visits(appended));
        let count = checkpoint.state["count"].as_i64().expect("count");
        assert!(count == appended || count == appended + 1);
        previous = appended;
    }
}
