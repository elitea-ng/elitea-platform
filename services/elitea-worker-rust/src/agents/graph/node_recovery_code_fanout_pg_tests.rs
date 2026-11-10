//! W3-2a on real PostgreSQL: a Map of Code items compiled by the production
//! compiler over the production checkpoint family (`PostgresCheckpointer` with
//! its application family, which lends every branch its node journals), with
//! the wiring harness's production `RemoteCodeRuntime`, fake Main content
//! service and fake Supervisor. A Worker replacement is a dropped run and a
//! second claim (a later fence) on the same execution, which resumes from the
//! frozen occurrence. No mTLS and no container runtime.

use super::*;
use crate::agents::graph::compiler::{
    ParallelCompilerBinding, PipelineDefinition, PipelineNodeRuntimes,
};
use crate::agents::graph::parallel::ParallelCheckpointAuthority;
use crate::state::{
    CheckpointLimits, CheckpointWriterAuthority, PostgresCheckpointer, TestStateWriterLease,
};
use adk_rust::graph::{Checkpointer, CompiledGraph};
use std::collections::HashMap;
use std::fmt::Write as _;

fn map_yaml(concurrency: usize, platform_client: bool) -> String {
    let platform = if platform_client {
        ", platform_client: true"
    } else {
        ""
    };
    format!(
        "entry_point: each\nstate:\n  items: {{type: list, value: []}}\n  count: {{type: int, value: 0}}\n  mapped: {{type: list, value: []}}\nnodes:\n  - {{id: each, type: map, worker: code, source: items, item: entity, index: item_index, outputs: [count], destination: mapped, max_items: 8, max_concurrency: {concurrency}, reduction: ordered_collection, transition: END}}\n  - {{id: code, type: code, language: python, code: 'return 41', input: [entity, item_index], output: [count]{platform}}}\n"
    )
}

async fn install(h: &Harness) {
    crate::state::postgres_session_tests::install_schema(&h.pool).await;
    sqlx::raw_sql(crate::agents::graph::pipeline_pg_test_support::CHECKPOINT_MIGRATION)
        .execute(&h.pool)
        .await
        .unwrap();
}

/// One claim's fenced writer family on `thread`, and the runtimes the
/// production composition binds: the run's Code runtime, its node journals
/// and the fan-out binding over the same authority.
async fn claim(
    h: &Harness,
    attempt: u64,
    thread: &str,
    definition: &PipelineDefinition,
) -> (Arc<dyn Checkpointer>, PipelineNodeRuntimes) {
    let fence = [u8::try_from(attempt).unwrap(); 32];
    let writer = CheckpointWriterAuthority::new(
        "tenant-1".to_owned(),
        1,
        2,
        "agent.execute.application.v1",
        definition.definition_digest(),
        thread.to_owned(),
        "execution-1".to_owned(),
        1,
        format!("claim-{attempt}"),
        attempt,
        attempt,
        1_700_000_000_000_000 + i64::try_from(attempt).unwrap() * 1_000_000,
        "workload-1".to_owned(),
        "producer-1".to_owned(),
        fence,
    )
    .unwrap();
    let family = Arc::new(
        PostgresCheckpointer::activate(
            h.pool.clone(),
            writer,
            CheckpointLimits::default(),
            Arc::new(TestStateWriterLease::current()),
        )
        .await
        .unwrap()
        .with_application_paths(&[])
        .await
        .unwrap(),
    );
    let authority: Arc<dyn ParallelCheckpointAuthority> = family.clone();
    let journals: Arc<dyn NodeRecoveryFactory> = family;
    let checkpointer: Arc<dyn Checkpointer> = authority.clone();
    let runtimes = PipelineNodeRuntimes::default()
        .with_code(h.runtime.clone())
        .with_node_recovery_authority(journals)
        .with_parallel(ParallelCompilerBinding::for_tests(
            authority,
            tokio::time::Instant::now() + Duration::from_secs(3600),
            None,
        ));
    (checkpointer, runtimes)
}

async fn compiled(
    h: &Harness,
    attempt: u64,
    thread: &str,
    definition: &PipelineDefinition,
) -> CompiledGraph {
    let (checkpointer, runtimes) = claim(h, attempt, thread, definition).await;
    definition
        .compile_subgraph_with_runtime(checkpointer, &runtimes)
        .unwrap()
}

/// Run until `stop` holds, then drop the run as a dying Worker does.
async fn run_until(
    graph: &CompiledGraph,
    input: adk_rust::graph::State,
    thread: &str,
    stop: impl Fn() -> bool,
) -> Option<Result<adk_rust::graph::State, GraphError>> {
    let run = graph.invoke(input, ExecutionConfig::new(thread));
    tokio::pin!(run);
    loop {
        tokio::select! {
            result = &mut run => return Some(result),
            () = tokio::time::sleep(Duration::from_millis(5)) => {
                if stop() {
                    return None;
                }
            }
        }
    }
}

fn items(count: usize) -> adk_rust::graph::State {
    HashMap::from([(
        "items".to_owned(),
        serde_json::json!(
            (0..count)
                .map(|index| format!("p{index}"))
                .collect::<Vec<_>>()
        ),
    )])
}

async fn dispatch_rows(h: &Harness) -> (i64, i64) {
    sqlx::query_as(
        "SELECT count(*), count(*) FILTER (WHERE NOT resolved) FROM elitea_runtime.sandbox_dispatches",
    )
    .fetch_one(&h.pool)
    .await
    .unwrap()
}

fn identities(h: &Harness) -> Vec<String> {
    distinct(&h.main.lock().unwrap().intent_dispatches)
}

fn assert_all_landed(state: &adk_rust::graph::State, count: usize) {
    let mapped = state["mapped"].as_array().unwrap();
    assert_eq!(mapped.len(), count, "{mapped:?}");
    for (index, entry) in mapped.iter().enumerate() {
        assert_eq!(entry["index"], index);
        assert_eq!(entry["outputs"]["count"], 41);
    }
}

/// Plan §4 test 1: a Map of 8 Code items (concurrency 2); the Worker dies with
/// four items completed and two submitted. A replacement Worker (a later
/// claim) resumes the frozen occurrence: the completed items replay their
/// receipts and are not submitted again, the two in-flight items re-attach
/// to their own dispatch identity (R), the last two run fresh, and the Map
/// lands exactly one answer per item.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_a_map_of_8_code_items_survives_worker_replacement() {
    let h = harness_with(
        QUIET,
        Duration::from_millis(400),
        Some(2),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    install(&h).await;
    h.answers
        .lock()
        .unwrap()
        .extend((0..32).map(|_| completed()));
    let definition = PipelineDefinition::from_yaml(&map_yaml(2, false)).unwrap();
    let graph = compiled(&h, 1, "fanout-root", &definition).await;
    let killed = run_until(&graph, items(8), "fanout-root", || {
        h.supervisor.lock().unwrap().submits >= 6
    })
    .await;
    assert!(killed.is_none(), "the first Worker died mid-Map");
    drop(graph);
    let before = identities(&h);
    assert_eq!(before.len(), 6, "six items reached their intent");
    let graph = compiled(&h, 2, "fanout-root", &definition).await;
    let state = graph
        .invoke(HashMap::new(), ExecutionConfig::new("fanout-root"))
        .await
        .expect("the replacement Worker completes the Map");
    assert_all_landed(&state, 8);
    let after = identities(&h);
    assert_eq!(
        after.len(),
        8,
        "one dispatch identity per item: nothing re-ran"
    );
    assert_eq!(
        &after[..6],
        &before[..],
        "the first six identities were kept"
    );
    assert_eq!(
        dispatch_rows(&h).await,
        (8, 0),
        "one dispatch per item, all resolved"
    );
    // Four completed + two re-attached in-flight + two fresh: the completed
    // items were never submitted again.
    assert_eq!(h.supervisor.lock().unwrap().submits, 10);
}

/// Plan §4 test 2: the Supervisor loses the runtime of the first two items.
/// Each is re-run once with a new identity (I, FU-214) and completes; no item
/// lands twice and every dispatch is resolved.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_a_supervisor_losing_code_items_reruns_each_once() {
    let h = harness_with(
        QUIET,
        Duration::ZERO,
        Some(2),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    install(&h).await;
    h.answers.lock().unwrap().extend(
        [
            terminal("sandbox.runtime_lost"),
            terminal("sandbox.runtime_lost"),
        ]
        .into_iter()
        .chain((0..8).map(|_| completed())),
    );
    let definition = PipelineDefinition::from_yaml(&map_yaml(2, false)).unwrap();
    let graph = compiled(&h, 1, "fanout-root", &definition).await;
    let state = graph
        .invoke(items(4), ExecutionConfig::new("fanout-root"))
        .await
        .expect("the lost items re-run once");
    assert_all_landed(&state, 4);
    assert_eq!(h.supervisor.lock().unwrap().submits, 6);
    assert_eq!(
        identities(&h).len(),
        6,
        "two lost items, one new identity each"
    );
    assert_eq!(dispatch_rows(&h).await, (6, 0));
}

/// Plan §4 test 5: a platform-client (effectful) Code item is killed after its
/// dispatch, and the Supervisor then reports the runtime lost. A platform
/// call may have run, so it is never re-run with a new identity. With the
/// owner's journal proving nothing unresolved it is reconciled (C) and settles
/// typed; without that proof it ends `child_effect_unknown` (F, owner proof
/// missing). A later claim repeats neither.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_a_platform_client_code_item_killed_after_dispatch_is_reconciled_never_rerun() {
    for effects in [Effects::None, Effects::Possible] {
        let h = harness_with(
            MainScript { effects, ..QUIET },
            Duration::from_millis(1500),
            Some(2),
            tokio::time::Instant::now() + Duration::from_secs(3600),
        )
        .await;
        install(&h).await;
        // The dead claim's own Supervisor call also takes an answer.
        h.answers.lock().unwrap().extend([
            terminal("sandbox.runtime_lost"),
            terminal("sandbox.runtime_lost"),
            terminal("sandbox.runtime_lost"),
        ]);
        let definition = PipelineDefinition::from_yaml(&map_yaml(1, true)).unwrap();
        let graph = compiled(&h, 1, "fanout-root", &definition).await;
        let killed = run_until(&graph, items(1), "fanout-root", || {
            h.supervisor.lock().unwrap().submits >= 1
        })
        .await;
        assert!(killed.is_none());
        drop(graph);
        for attempt in [2, 3] {
            let graph = compiled(&h, attempt, "fanout-root", &definition).await;
            let error = graph
                .invoke(HashMap::new(), ExecutionConfig::new("fanout-root"))
                .await
                .expect_err("the item ends typed");
            let unknown = error
                .to_string()
                .contains(crate::agents::graph::node_recovery_runtime::CHILD_EFFECT_UNKNOWN_CODE);
            assert_eq!(
                unknown,
                matches!(effects, Effects::Possible),
                "claim {attempt}: {error}"
            );
            assert!(
                error.to_string().contains("item '0'")
                    && !error.to_string().contains("stale_activation"),
                "the restored item itself ended typed: {error}"
            );
            assert!(
                h.main.lock().unwrap().effects >= 1,
                "the owner journal was asked"
            );
            assert_eq!(identities(&h).len(), 1, "never re-run with a new identity");
        }
        let (rows, _) = dispatch_rows(&h).await;
        assert_eq!(rows, 1, "one dispatch identity");
    }
}

fn map_of(count: usize) -> String {
    map_yaml(1, false).replace("max_items: 8", &format!("max_items: {count}"))
}

/// `count` root Code nodes in a chain, each its own journaled attempt.
fn root_chain(count: usize) -> String {
    let mut yaml = "entry_point: code0\nstate:\n  items: {type: list, value: []}\n  count: {type: int, value: 0}\nnodes:\n".to_owned();
    for index in 0..count {
        let next = if index + 1 == count {
            "END".to_owned()
        } else {
            format!("code{}", index + 1)
        };
        std::fmt::Write::write_fmt(
            &mut yaml,
            format_args!(
            "  - {{id: code{index}, type: code, language: python, code: 'return 41', input: [items], output: [count], transition: {next}}}\n"
            ),
        )
        .unwrap();
    }
    yaml
}

/// Plan §4 budget: the per-child Code overhead against root Code, user code
/// excluded (the Supervisor answers at once). One sample runs, each on a
/// fresh thread and claim, a root chain of 1 and of 9 Code nodes and a Map
/// (concurrency 1) of 1 and of 9 Code items. The marginal cost of one more
/// root Code node is `(root9 − root1) / 8`, of one more Code child
/// `(map9 − map1) / 8`; their difference is the per-child overhead, so the
/// Map's one-time activation (freeze, join) is not charged to its children.
/// The gate is p99(overhead) ≤ 30 ms (`CHILD_OVERHEAD_P99`).
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_per_child_code_overhead_is_within_its_budget() {
    const SAMPLES: usize = 20;
    let h = harness_with(
        QUIET,
        Duration::ZERO,
        Some(4),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    install(&h).await;
    h.answers
        .lock()
        .unwrap()
        .extend((0..21 * (SAMPLES + 1)).map(|_| completed()));
    let shapes = [
        PipelineDefinition::from_yaml(&root_chain(1)).unwrap(),
        PipelineDefinition::from_yaml(&root_chain(9)).unwrap(),
        PipelineDefinition::from_yaml(&map_of(1)).unwrap(),
        PipelineDefinition::from_yaml(&map_of(9)).unwrap(),
    ];
    let inputs = [1, 1, 1, 9];
    let (mut roots, mut children, mut overheads, mut activations) =
        (Vec::new(), Vec::new(), Vec::new(), Vec::new());
    // One warm-up sample is discarded.
    for sample in 0..=SAMPLES {
        let mut times = [Duration::ZERO; 4];
        for (shape, definition) in shapes.iter().enumerate() {
            let thread = format!("budget-{sample}-{shape}");
            let graph = compiled(&h, 1, &thread, definition).await;
            let started = std::time::Instant::now();
            graph
                .invoke(items(inputs[shape]), ExecutionConfig::new(&thread))
                .await
                .unwrap();
            times[shape] = started.elapsed();
        }
        if sample == 0 {
            continue;
        }
        let root = times[1].saturating_sub(times[0]).as_secs_f64() / 8.0;
        let child = times[3].saturating_sub(times[2]).as_secs_f64() / 8.0;
        roots.push(root);
        children.push(child);
        overheads.push(child - root);
        activations.push(times[2].as_secs_f64() - times[0].as_secs_f64());
    }
    let p = |values: &mut Vec<f64>, percentile: usize| {
        values.sort_by(f64::total_cmp);
        values[(values.len() * percentile).div_ceil(100).saturating_sub(1)] * 1e3
    };
    let overhead_p99 = p(&mut overheads, 99);
    println!(
        "W3-2A-CODE-CHILD-OVERHEAD samples={SAMPLES} root_code_p50_ms={:.1} root_code_p99_ms={:.1} code_child_p50_ms={:.1} code_child_p99_ms={:.1} overhead_p50_ms={:.1} overhead_p99_ms={overhead_p99:.1} map_activation_p50_ms={:.1} map_activation_p99_ms={:.1}",
        p(&mut roots, 50),
        p(&mut roots, 99),
        p(&mut children, 50),
        p(&mut children, 99),
        p(&mut overheads, 50),
        p(&mut activations, 50),
        p(&mut activations, 99),
    );
    assert!(
        overhead_p99
            <= elitea_agent_runtime::graph::fanout_budget::CHILD_OVERHEAD_P99.as_secs_f64() * 1e3,
        "per-child Code overhead p99 {overhead_p99:.1} ms"
    );
}

/// B1: a Parallel whose branches are Code nodes (two languages) runs on the
/// production checkpoint family: each branch is a path-free child (no
/// application subgraph), runs its Code attempt and lands.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_a_parallel_of_code_branches_runs_on_the_production_family() {
    let h = harness_with(
        QUIET,
        Duration::ZERO,
        Some(2),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    install(&h).await;
    h.answers
        .lock()
        .unwrap()
        .extend((0..4).map(|_| completed()));
    let definition = PipelineDefinition::from_yaml(
        "entry_point: gather\nstate:\n  topic: {type: str, value: slots}\n  count: {type: int, value: 0}\n  joined: {type: list, value: []}\nnodes:\n  - {id: gather, type: parallel, branches: [{id: left, node: code_a}, {id: right, node: code_b}], max_concurrency: 2, wait: all, output: [joined], transition: END}\n  - {id: code_a, type: code, language: python, code: 'return 41', input: [topic], output: [count]}\n  - {id: code_b, type: code, language: python, code: 'return 41', input: [topic], output: [count]}\n",
    )
    .unwrap();
    let graph = compiled(&h, 1, "parallel-root", &definition).await;
    let state = graph
        .invoke(HashMap::new(), ExecutionConfig::new("parallel-root"))
        .await
        .expect("a Parallel of Code branches runs");
    let joined = state["joined"].as_array().unwrap();
    assert_eq!(joined.len(), 2, "{joined:?}");
    for entry in joined {
        assert_eq!(entry["status"], "succeeded", "{entry}");
        assert_eq!(entry["outputs"]["count"], 41);
    }
    assert_eq!(h.supervisor.lock().unwrap().submits, 2);
}

/// B5 shapes: the first Worker dies while its children are still registering
/// visits (Main answering `UNAVAILABLE` to the first ones) or after their
/// intents and before their submissions; the replacement completes every
/// item exactly once.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_a_worker_killed_mid_registration_is_resumed_by_the_replacement() {
    for (shape, stop) in [("visits", 0_usize), ("intents", 1_usize)] {
        let h = harness_with(
            MainScript {
                visit_unavailable: 4,
                intent_delay: Some(Duration::from_millis(300)),
                ..QUIET
            },
            Duration::from_millis(400),
            Some(4),
            tokio::time::Instant::now() + Duration::from_secs(3600),
        )
        .await;
        install(&h).await;
        h.answers
            .lock()
            .unwrap()
            .extend((0..32).map(|_| completed()));
        let definition = PipelineDefinition::from_yaml(&map_yaml(4, false)).unwrap();
        let graph = compiled(&h, 1, "fanout-root", &definition).await;
        let killed = run_until(&graph, items(8), "fanout-root", || {
            let main = h.main.lock().unwrap();
            if stop == 0 {
                !main.visit_attempts.is_empty()
            } else {
                main.intent_dispatches.len() >= stop
            }
        })
        .await;
        assert!(killed.is_none(), "{shape}: the first Worker died");
        drop(graph);
        let graph = compiled(&h, 2, "fanout-root", &definition).await;
        let state = graph
            .invoke(HashMap::new(), ExecutionConfig::new("fanout-root"))
            .await
            .unwrap_or_else(|error| panic!("{shape}: the replacement completes: {error}"));
        assert_all_landed(&state, 8);
        assert_eq!(identities(&h).len(), 8, "{shape}: one identity per item");
        assert_eq!(dispatch_rows(&h).await, (8, 0), "{shape}");
    }
}

fn parallel_yaml(branches: usize, concurrency: usize) -> String {
    let declared = (0..branches)
        .map(|index| format!("{{id: b{index}, node: code_{index}}}"))
        .collect::<Vec<_>>()
        .join(", ");
    let mut nodes = String::new();
    for index in 0..branches {
        let _ = writeln!(
            nodes,
            "  - {{id: code_{index}, type: code, language: python, code: 'return 41', input: [topic], output: [count]}}"
        );
    }
    format!(
        "entry_point: gather\nstate:\n  topic: {{type: str, value: slots}}\n  count: {{type: int, value: 0}}\n  joined: {{type: list, value: []}}\nnodes:\n  - {{id: gather, type: parallel, branches: [{declared}], max_concurrency: {concurrency}, wait: all, output: [joined], transition: END}}\n{nodes}"
    )
}

fn assert_branches_landed(state: &adk_rust::graph::State, count: usize) {
    let joined = state["joined"].as_array().unwrap();
    assert_eq!(joined.len(), count, "{joined:?}");
    for entry in joined {
        assert_eq!(entry["status"], "succeeded", "{entry}");
        assert_eq!(entry["outputs"]["count"], 41);
    }
}

/// M1, Worker kill x Parallel Code branch: a Parallel of 4 Code branches
/// (concurrency 2); the Worker dies with two branches submitted. The
/// replacement resumes the frozen occurrence: the in-flight branches
/// re-attach to their own identities (R), the others run fresh, and every
/// branch lands once.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_a_parallel_of_code_branches_survives_worker_replacement() {
    let h = harness_with(
        QUIET,
        Duration::from_millis(400),
        Some(2),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    install(&h).await;
    h.answers
        .lock()
        .unwrap()
        .extend((0..16).map(|_| completed()));
    let definition = PipelineDefinition::from_yaml(&parallel_yaml(4, 2)).unwrap();
    let graph = compiled(&h, 1, "parallel-root", &definition).await;
    let killed = run_until(&graph, HashMap::new(), "parallel-root", || {
        h.supervisor.lock().unwrap().submits >= 2
    })
    .await;
    assert!(killed.is_none(), "the first Worker died mid-Parallel");
    drop(graph);
    let before = identities(&h);
    assert_eq!(before.len(), 2, "two branches reached their intent");
    let graph = compiled(&h, 2, "parallel-root", &definition).await;
    let state = graph
        .invoke(HashMap::new(), ExecutionConfig::new("parallel-root"))
        .await
        .expect("the replacement Worker completes the Parallel");
    assert_branches_landed(&state, 4);
    let after = identities(&h);
    assert_eq!(after.len(), 4, "one dispatch identity per branch");
    assert_eq!(
        &after[..2],
        &before[..],
        "the in-flight identities were kept"
    );
    assert_eq!(dispatch_rows(&h).await, (4, 0));
}

/// M1, Supervisor kill x Code child, the job survives: the Supervisor
/// answers busy while it restarts, then answers the same job. Every child
/// re-attaches to its own identity (R): no new identity, nothing re-run, for
/// a Map and for a Parallel.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_a_restarted_supervisor_reattaches_map_and_parallel_code_children() {
    for (shape, yaml, input, count) in [
        ("map", map_yaml(2, false), items(4), 4),
        ("parallel", parallel_yaml(4, 2), HashMap::new(), 4),
    ] {
        let h = harness_with(
            QUIET,
            Duration::ZERO,
            Some(2),
            tokio::time::Instant::now() + Duration::from_secs(3600),
        )
        .await;
        install(&h).await;
        h.answers
            .lock()
            .unwrap()
            .extend((0..8).map(|_| completed()));
        *h.busy.lock().unwrap() = 3;
        let definition = PipelineDefinition::from_yaml(&yaml).unwrap();
        let graph = compiled(&h, 1, "fanout-root", &definition).await;
        let state = graph
            .invoke(input, ExecutionConfig::new("fanout-root"))
            .await
            .unwrap_or_else(|error| panic!("{shape}: the children re-attach: {error}"));
        if shape == "map" {
            assert_all_landed(&state, count);
        } else {
            assert_branches_landed(&state, count);
        }
        assert_eq!(identities(&h).len(), count, "{shape}: no child re-ran");
        assert_eq!(
            dispatch_rows(&h).await,
            (i64::try_from(count).unwrap(), 0),
            "{shape}"
        );
        assert_eq!(*h.busy.lock().unwrap(), 0, "{shape}: the restart was met");
    }
}

/// M1, Supervisor kill x Parallel Code branch, runtime lost: each lost
/// branch is re-run once with a new identity (I, FU-214) and lands once.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_a_supervisor_losing_parallel_code_branches_reruns_each_once() {
    let h = harness_with(
        QUIET,
        Duration::ZERO,
        Some(2),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    install(&h).await;
    h.answers.lock().unwrap().extend(
        [
            terminal("sandbox.runtime_lost"),
            terminal("sandbox.runtime_lost"),
        ]
        .into_iter()
        .chain((0..8).map(|_| completed())),
    );
    let definition = PipelineDefinition::from_yaml(&parallel_yaml(4, 2)).unwrap();
    let graph = compiled(&h, 1, "parallel-root", &definition).await;
    let state = graph
        .invoke(HashMap::new(), ExecutionConfig::new("parallel-root"))
        .await
        .expect("the lost branches re-run once");
    assert_branches_landed(&state, 4);
    assert_eq!(h.supervisor.lock().unwrap().submits, 6);
    assert_eq!(
        identities(&h).len(),
        6,
        "two lost branches, one new identity each"
    );
    assert_eq!(dispatch_rows(&h).await, (6, 0));
}
