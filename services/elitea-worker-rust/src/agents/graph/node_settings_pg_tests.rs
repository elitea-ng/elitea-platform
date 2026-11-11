#![allow(
    clippy::disallowed_methods,
    reason = "inline test fixtures, not stored or fetched documents"
)]

//! 5b-1 recovery rows on real `PostgreSQL`, across process replacement.
//!
//! Each scenario runs two child processes of this test binary against one
//! isolated database: a writer (claim attempt 1) that stops at a static pause,
//! which is where a crashed process leaves its durable frontier, and a
//! replacement writer (claim attempt 2) that continues through the production
//! text continuation.
//!
//! - `after`: the writer stops after the continued failure's step checkpoint.
//!   The replacement never re-runs the node: one entry, the same bytes.
//! - `before`: the writer stops before the failing node. The replacement runs
//!   it, settles it, and the entry is the deterministic one for its input.
//! - `once`: a run-once node in a three-visit loop; the writer stops after
//!   its first visit. The replacement's visits never run its body again.

use serde_json::{Value, json};

use super::compiler::PipelineDefinition;
use super::pipeline_pg_test_support::{
    CHECKPOINT_MIGRATION, SESSION, activate, continuation, create_session, history, run,
};
use crate::state::postgres_session_tests::{IsolatedPostgres, install_schema};
use elitea_agent_runtime::graph::node_settings::node_error_support_ref;

const CHILD_TEST: &str = "agents::graph::node_settings_pg_tests::postgres_node_settings_child";
const DATABASE_ENV: &str = "ELITEA_NODE_SETTINGS_PG_DATABASE";
const SCENARIO_ENV: &str = "ELITEA_NODE_SETTINGS_PG_SCENARIO";
const PHASE_ENV: &str = "ELITEA_NODE_SETTINGS_PG_PHASE";

const CONTINUED: &str = r#"
state:
  value: str
  out: str
  count: str
entry_point: shape
nodes:
  - id: shape
    type: state_modifier
    template: "{{ value | nosuchfilter }}"
    input: [value]
    output: [out]
    settings: {on_error: continue, always_output_data: true}
    transition: after
  - id: after
    type: state_modifier
    template: "{{ node_errors | length }}"
    input: [node_errors]
    output: [count]
    transition: END
"#;

const ONCE: &str = r#"
interrupt_after: [tick]
state:
  visits: {type: int, value: 0}
  count: {type: int, value: 0}
entry_point: bump
nodes:
  - id: bump
    type: state_modifier
    template: "{{ visits + 1 }}"
    input: [visits]
    output: [visits]
    transition: tick
  - id: tick
    type: state_modifier
    template: "{{ count + 1 }}"
    input: [count]
    output: [count]
    settings: {execute_once: true}
    transition: choose
  - id: choose
    type: router
    condition: "{{ 'bump' if visits < 3 else 'END' }}"
    input: [visits]
    routes: [bump, END]
    default_output: END
"#;

fn pipeline(scenario: &str) -> PipelineDefinition {
    let yaml = match scenario {
        "after" => format!("interrupt_after: [shape]\n{CONTINUED}"),
        "before" => format!("interrupt_before: [shape]\n{CONTINUED}"),
        "once" => ONCE.to_owned(),
        other => panic!("unknown scenario {other}"),
    };
    PipelineDefinition::from_yaml(&yaml).expect("settings replacement pipeline")
}

#[tokio::test]
async fn postgres_node_settings_survive_process_replacement() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the node settings PostgreSQL proof");
        return;
    };
    for scenario in ["after", "before", "once"] {
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
                .env(SCENARIO_ENV, scenario)
                .env(PHASE_ENV, phase)
                .output()
                .expect("spawn child test process");
            let stdout = String::from_utf8_lossy(&output.stdout);
            assert!(
                output.status.success(),
                "{scenario}/{phase} failed: {stdout} {}",
                String::from_utf8_lossy(&output.stderr)
            );
            assert!(stdout.contains("1 passed"), "{scenario}/{phase}: {stdout}");
        }
        db.pool.close().await;
    }
}

#[tokio::test]
async fn postgres_node_settings_child() {
    let Ok(database) = std::env::var(DATABASE_ENV) else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    let scenario = std::env::var(SCENARIO_ENV).expect("scenario");
    let definition = pipeline(&scenario);
    let attempt = match std::env::var(PHASE_ENV).expect("phase").as_str() {
        "write" => 1,
        "read" => 2,
        other => panic!("unknown phase {other}"),
    };
    let services = activate(&database, attempt, &definition).await;
    let before = history(services.checkpointer.as_ref()).await;
    if attempt == 1 {
        assert!(before.is_empty());
        create_session(services.sessions.as_ref()).await;
        run(
            &definition,
            services.checkpointer.clone(),
            services.sessions.clone(),
            None,
        )
        .await;
    } else {
        // The replacement continues until the graph finishes; the bound only
        // stops a runaway loop.
        for _ in 0..8 {
            let last = history(services.checkpointer.as_ref()).await;
            if last
                .last()
                .is_some_and(|checkpoint| checkpoint.pending_nodes.is_empty())
            {
                break;
            }
            let resume = continuation(
                &definition,
                services.checkpointer.as_ref(),
                services.sessions.as_ref(),
            )
            .await;
            run(
                &definition,
                services.checkpointer.clone(),
                services.sessions.clone(),
                Some(resume),
            )
            .await;
        }
    }
    let after = history(services.checkpointer.as_ref()).await;
    let last = after.last().expect("checkpoint");
    match (scenario.as_str(), attempt) {
        ("after", 1) => {
            // The crash window after the step: the entry is durable already.
            assert_eq!(last.state["node_errors"].as_array().map(Vec::len), Some(1));
            assert_eq!(last.state["count"], json!(""));
        }
        ("after", 2) => {
            let durable = &before.last().expect("paused").state["node_errors"];
            assert_eq!(
                &last.state["node_errors"], durable,
                "no second entry, same bytes"
            );
            assert_eq!(last.state["count"], json!("1"));
            assert_eq!(last.pending_nodes, Vec::<String>::new());
        }
        ("before", 1) => {
            assert_eq!(last.state["node_errors"], json!([]));
        }
        ("before", 2) => {
            let entries = last.state["node_errors"].as_array().expect("entries");
            assert_eq!(entries.len(), 1);
            let step = entries[0]["input_step"].as_u64().expect("input step");
            // Deterministic: the support reference is a function of the
            // thread, the node and the input step only.
            assert_eq!(
                entries[0]["error"]["support_ref"],
                Value::String(node_error_support_ref(
                    &format!("\0{SESSION}"),
                    "shape",
                    step
                ))
            );
            assert_eq!(last.state["count"], json!("1"));
        }
        ("once", 1) => {
            assert_eq!(last.state["count"], json!(1));
            assert_eq!(last.state["visits"], json!(1));
        }
        ("once", 2) => {
            assert_eq!(last.state["visits"], json!(3));
            assert_eq!(last.state["count"], json!(1), "the body ran once in all");
            assert_eq!(last.pending_nodes, Vec::<String>::new());
        }
        other => panic!("unexpected {other:?}"),
    }
    services.pool.close().await;
}
