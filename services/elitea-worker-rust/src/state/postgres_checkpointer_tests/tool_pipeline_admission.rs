//! FU-291 D-3: a fan-out member admits the nested thread of a saved pipeline
//! its Agent calls as a tool only through its own fenced family, before the
//! nested graph's first effect. The thread name grants nothing.
use super::*;
use crate::agents::application_pipeline::PipelineToolCallLineage;
use crate::agents::graph::ParallelChildCheckpoint;
use crate::agents::graph::tool_pipeline_threads::{
    MAX_TOOL_PIPELINE_THREADS_PER_MEMBER, is_tool_pipeline_limit, tool_pipeline_thread,
};

async fn database() -> IsolatedPostgres {
    let url = crate::test_pg::database_url_var(TEST_DATABASE_URL)
        .expect("ELITEA_TEST_DATABASE_URL is required");
    let database = IsolatedPostgres::create(&url).await;
    install_test_schema(&database.pool).await;
    database
}

async fn root(pool: PgPool, claim_attempt: u64) -> PostgresCheckpointer {
    PostgresCheckpointer::activate(
        pool,
        writer(
            "execution-1",
            &format!("claim-{claim_attempt}"),
            claim_attempt,
            claim_attempt,
            [0x41; 32],
            [0x61; 32],
        ),
        CheckpointLimits::default(),
        Arc::new(TestStateWriterLease::current()),
    )
    .await
    .expect("activate the run root writer")
}

async fn writers(pool: &PgPool, thread_pattern: &str) -> i64 {
    sqlx::query_scalar(
        "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoint_writers WHERE thread_id LIKE $1",
    )
    .bind(thread_pattern)
    .fetch_one(pool)
    .await
    .expect("count checkpoint writers")
}

fn lineage(call: &str) -> PipelineToolCallLineage {
    PipelineToolCallLineage::for_test(
        "batch-1",
        1,
        call,
        "summarize",
        &json!({"task": "summarize the topic"}),
    )
}

/// One Map-free Parallel member family whose owned node is `fetch_short`,
/// under the claim of `root_family`.
async fn member(root_family: &impl ParallelChildCheckpointerFactory) -> ParallelChildCheckpoint {
    let parallel = ParallelNodeDefinition::from_yaml(
        r"
id: gather
type: parallel
branches:
  - id: short
    node: fetch_short
  - id: long
    node: fetch_long
max_concurrency: 2
wait: all
error_policy: fail_after_drain
output: [gathered]
transition: END
        ",
    )
    .expect("parallel fixture");
    let activation = ParallelActivation {
        root_thread_id: "thread-1".to_owned(),
        node_id: parallel.id().to_owned(),
        step: 4,
        config_digest: parallel.config_digest(),
        map: None,
    };
    let origin = root_family
        .child_origin(&activation)
        .expect("parallel child origin");
    root_family
        .for_branch(
            &activation,
            &parallel.branches()[0],
            0,
            &[0x42; 32],
            &origin,
        )
        .await
        .expect("activate the member family")
}

/// The member admits its Agent's nested thread under its claim: one writer
/// row, durable before anything runs on it, registered in the one set the
/// member's projector reads. Re-admission returns the same writer and adds no
/// row; the writer saves and loads the nested graph's checkpoints.
#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn a_member_admits_its_agent_nested_pipeline_thread_under_its_claim() {
    let database = database().await;
    let family = root(database.pool.clone(), 1)
        .await
        .with_application_children(["fetch_short", "fetch_long"].into_iter())
        .await
        .expect("activate the root family");
    let member = member(&family).await;
    let admission = member
        .tool_pipelines
        .clone()
        .expect("the member is lent its admission");
    let agent = format!("{}/fetch_short", member.thread_id);
    let call = lineage("call-1");
    let expected = tool_pipeline_thread(&agent, &call).unwrap();
    assert_eq!(writers(&database.pool, &format!("{agent}/pt1:%")).await, 0);

    let admitted = admission
        .admit_tool_pipeline(&agent, &call)
        .await
        .expect("admit the nested thread");
    assert_eq!(admitted.thread_id, expected);
    assert_eq!(writers(&database.pool, &format!("{agent}/pt1:%")).await, 1);
    assert!(
        admission
            .tool_threads()
            .admitted(&expected, &agent)
            .is_some(),
        "the projector's set holds exactly what the checkpointer admitted"
    );
    let again = admission.admit_tool_pipeline(&agent, &call).await.unwrap();
    assert_eq!(again.thread_id, expected);
    assert_eq!(writers(&database.pool, &format!("{agent}/pt1:%")).await, 1);

    let mut saved = Checkpoint::new(&expected, State::new(), 0, vec!["first".to_owned()]);
    saved.checkpoint_id = "nested-1".to_owned();
    admitted
        .writer
        .save(&saved)
        .await
        .expect("save on the nested thread");
    assert_eq!(
        admitted
            .writer
            .load(&expected)
            .await
            .unwrap()
            .map(|row| row.pending_nodes),
        Some(vec!["first".to_owned()])
    );
}

/// Only an Agent thread of this member admits: not the member root, not a
/// thread of the root family or another member, not a thread outside any
/// family. A refusal writes no row and registers nothing.
#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn a_member_refuses_every_agent_thread_that_is_not_its_own() {
    let database = database().await;
    let family = root(database.pool.clone(), 1)
        .await
        .with_application_children(["fetch_short", "fetch_long"].into_iter())
        .await
        .expect("activate the root family");
    let member = member(&family).await;
    let admission = member.tool_pipelines.clone().unwrap();
    let call = lineage("call-1");
    for agent in [
        member.thread_id.clone(),
        "thread-1/fetch_short".to_owned(),
        "thread-1".to_owned(),
        format!("{}/unadmitted", member.thread_id),
        "p1:another-member/fetch_short".to_owned(),
    ] {
        assert!(
            admission.admit_tool_pipeline(&agent, &call).await.is_err(),
            "{agent} admitted a nested thread"
        );
    }
    assert_eq!(writers(&database.pool, "%/pt1:%").await, 0);
    assert_eq!(admission.tool_threads().len(), 0);
}

/// D3 at the database: the member's 9th nested call is refused typed before
/// any activation, so it leaves no writer row.
#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn a_member_activates_8_nested_threads_and_refuses_the_9th_without_a_row() {
    let database = database().await;
    let family = root(database.pool.clone(), 1)
        .await
        .with_application_children(["fetch_short", "fetch_long"].into_iter())
        .await
        .expect("activate the root family");
    let member = member(&family).await;
    let admission = member.tool_pipelines.clone().unwrap();
    let agent = format!("{}/fetch_short", member.thread_id);
    for call in 0..MAX_TOOL_PIPELINE_THREADS_PER_MEMBER {
        admission
            .admit_tool_pipeline(&agent, &lineage(&format!("call-{call}")))
            .await
            .expect("admit within the bound");
    }
    let Err(refused) = admission
        .admit_tool_pipeline(&agent, &lineage("call-8"))
        .await
    else {
        panic!("the 9th nested call was admitted");
    };
    assert!(is_tool_pipeline_limit(&refused), "{refused}");
    assert_eq!(writers(&database.pool, &format!("{agent}/pt1:%")).await, 8);
}

/// A newer claim at the run root denies the older member's admission
/// (`writer_not_current`, no row). The newer claim's member re-derives and
/// re-activates the same nested thread; the older writer is fenced.
#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn a_takeover_re_admits_the_same_nested_thread_and_fences_the_older_claim() {
    let database = database().await;
    let family_1 = root(database.pool.clone(), 1)
        .await
        .with_application_children(["fetch_short", "fetch_long"].into_iter())
        .await
        .expect("activate the root family");
    let member_1 = member(&family_1).await;
    let admission_1 = member_1.tool_pipelines.clone().unwrap();
    let agent = format!("{}/fetch_short", member_1.thread_id);
    let older = admission_1
        .admit_tool_pipeline(&agent, &lineage("call-1"))
        .await
        .expect("claim 1 admits");

    let family_2 = root(database.pool.clone(), 2)
        .await
        .with_application_children(["fetch_short", "fetch_long"].into_iter())
        .await
        .expect("claim 2 takes the run");
    let Err(denied) = admission_1
        .admit_tool_pipeline(&agent, &lineage("call-2"))
        .await
    else {
        panic!("a superseded claim admitted a nested thread");
    };
    assert!(
        denied.to_string().contains("writer_not_current"),
        "{denied}"
    );

    let member_2 = member(&family_2).await;
    assert_eq!(
        member_2.thread_id, member_1.thread_id,
        "the member re-derives"
    );
    let newer = member_2
        .tool_pipelines
        .clone()
        .unwrap()
        .admit_tool_pipeline(&agent, &lineage("call-1"))
        .await
        .expect("claim 2 re-admits");
    assert_eq!(
        newer.thread_id, older.thread_id,
        "the nested thread re-derives"
    );
    assert_eq!(writers(&database.pool, &format!("{agent}/pt1:%")).await, 1);
    let mut stale = Checkpoint::new(&older.thread_id, State::new(), 0, Vec::new());
    stale.checkpoint_id = "stale".to_owned();
    let Err(fenced) = older.writer.save(&stale).await else {
        panic!("the older claim's nested writer saved after the takeover");
    };
    assert!(
        fenced.to_string().contains("writer_not_current"),
        "{fenced}"
    );
}

/// T6, C5: a writer table held locked during admission is a retryable
/// dependency failure (`checkpoint.storage_unavailable`), never a refusal,
/// and registers nothing; once released the same admission succeeds.
#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn a_locked_writer_table_answers_retryable_and_admits_once_released() {
    let database = database().await;
    let family = root(database.pool.clone(), 1)
        .await
        .with_application_children(["fetch_short", "fetch_long"].into_iter())
        .await
        .expect("activate the root family");
    let member = member(&family).await;
    let admission = member.tool_pipelines.clone().unwrap();
    let agent = format!("{}/fetch_short", member.thread_id);
    let mut holder = database.pool.begin().await.expect("begin holder");
    sqlx::query(
        "SELECT 1 FROM elitea_runtime.agent_graph_checkpoint_writers WHERE thread_id = 'thread-1' FOR UPDATE",
    )
    .fetch_one(&mut *holder)
    .await
    .expect("hold the run root writer row");
    let Err(blocked) = admission
        .admit_tool_pipeline(&agent, &lineage("call-1"))
        .await
    else {
        panic!("admission ran behind a held run root writer");
    };
    assert!(
        blocked
            .to_string()
            .contains("checkpoint.storage_unavailable"),
        "a held lock is a retryable dependency failure: {blocked}"
    );
    assert!(!is_tool_pipeline_limit(&blocked));
    assert_eq!(admission.tool_threads().len(), 0);
    holder.rollback().await.expect("release the holder");
    admission
        .admit_tool_pipeline(&agent, &lineage("call-1"))
        .await
        .expect("the same admission succeeds once released");
    assert_eq!(writers(&database.pool, &format!("{agent}/pt1:%")).await, 1);
}

/// C7 budget: the per-call cost a durable nested pipeline call adds over the
/// same pipeline run at the root. The nested graph's own rows are the same
/// a root run saves (same compiler, same checkpointer, one row per step), and
/// its `durable_delegation_pending` record replaces the root's
/// `tool_may_have_started` record one for one; what is new per call is the
/// fenced admission (one activation transaction) and the latest-row read
/// (C3). Each sample is a fresh call on a fresh member family, the minimum of
/// `REPEATS` runs; the gate is p99 ≤ `CHILD_OVERHEAD_P99` (30 ms).
#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn the_per_call_nested_admission_cost_is_within_the_child_overhead_budget() {
    const SAMPLES: usize = 20;
    const REPEATS: usize = 3;
    let database = database().await;
    let family = root(database.pool.clone(), 1)
        .await
        .with_application_children(["fetch_short", "fetch_long"].into_iter())
        .await
        .expect("activate the root family");
    let first = member(&family).await;
    let agent = format!("{}/fetch_short", first.thread_id);
    let mut admission = first.tool_pipelines.clone().unwrap();
    let mut costs = Vec::new();
    let mut call = 0_usize;
    // One warm-up sample is discarded.
    for sample in 0..=SAMPLES {
        let mut best = std::time::Duration::MAX;
        for _ in 0..REPEATS {
            // A fresh member family object every 8 calls keeps each within D3.
            if call > 0 && call % MAX_TOOL_PIPELINE_THREADS_PER_MEMBER == 0 {
                admission = member(&family).await.tool_pipelines.unwrap();
            }
            let lineage = lineage(&format!("call-{call}"));
            call += 1;
            let started = std::time::Instant::now();
            let admitted = admission
                .admit_tool_pipeline(&agent, &lineage)
                .await
                .expect("admit");
            let latest = admitted
                .writer
                .load(&admitted.thread_id)
                .await
                .expect("read");
            best = best.min(started.elapsed());
            assert!(latest.is_none());
        }
        if sample > 0 {
            costs.push(best.as_secs_f64() * 1e3);
        }
    }
    costs.sort_by(f64::total_cmp);
    let p = |percentile: usize| costs[(costs.len() * percentile).div_ceil(100).saturating_sub(1)];
    let (p50, p99) = (p(50), p(99));
    println!(
        "FU-291-NESTED-CALL-OVERHEAD samples={SAMPLES} repeats={REPEATS} p50_ms={p50:.1} p99_ms={p99:.1}"
    );
    assert!(
        p99 <= crate::agents::graph::fanout_budget::CHILD_OVERHEAD_P99.as_secs_f64() * 1e3,
        "per-call nested admission p99 {p99:.1} ms"
    );
}
