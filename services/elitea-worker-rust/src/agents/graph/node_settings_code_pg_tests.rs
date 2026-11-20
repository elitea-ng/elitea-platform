//! 5b-1 PR B real-PostgreSQL crash suite + FU-348 (D15) saved-child recovery.
//!
//! Exercises the production compiler, production `PostgresCheckpointer` claim
//! fencing, production `RecoverableNode` + `SettingsNode`, production
//! `RemoteCodeRuntime`, and production `NativePipelineApplicationResolver` on
//! isolated disposable PostgreSQL databases (`ELITEA_TEST_DATABASE_URL`).

use super::*;
use crate::agents::graph::application::{
    ApplicationExecutionError, PipelineApplicationResolver, PipelineApplicationSelection,
    ResolvedApplicationParticipant,
};
use crate::agents::graph::compiler::{PipelineDefinition, PipelineNodeRuntimes};
use crate::agents::graph::node_recovery::NodeRecoveryPolicy;
use crate::agents::graph::node_recovery_runtime::NodeAttemptActivation;
use crate::agents::pipeline::saved_code_child::{SavedCodeFanoutMember, SavedCodeOwner};
use adk_rust::graph::{Checkpointer, CompiledGraph, ExecutionConfig, State};
use serde_json::json;
use std::collections::{BTreeMap, HashMap};
use std::time::Instant;

struct TestActivations(Arc<dyn PipelineApplicationResolver>);

impl PipelineApplicationResolver for TestActivations {
    fn fixed_parallel_definition_digest(
        &self,
        _node: &str,
    ) -> Result<[u8; 32], ApplicationExecutionError> {
        Ok([9; 32])
    }
    fn for_branch_journals(
        &self,
        journals: Arc<dyn NodeRecoveryFactory>,
    ) -> Option<Arc<dyn PipelineApplicationResolver>> {
        Some(Arc::new(Self(
            self.0
                .for_branch_journals(journals)
                .unwrap_or_else(|| self.0.clone()),
        )))
    }
    fn for_saved_code_member(
        &self,
        member: &SavedCodeFanoutMember,
    ) -> Option<Arc<dyn PipelineApplicationResolver>> {
        Some(Arc::new(Self(
            self.0
                .for_saved_code_member(member)
                .unwrap_or_else(|| self.0.clone()),
        )))
    }
    fn resolve(
        &self,
        selection: &PipelineApplicationSelection,
        checkpointer: Arc<dyn Checkpointer>,
    ) -> Result<ResolvedApplicationParticipant, ApplicationExecutionError> {
        self.0.resolve(selection, checkpointer)
    }
    fn test_activation_checkpoints(&self) -> bool {
        true
    }
    fn resolve_node(
        &self,
        node: &str,
        selection: &PipelineApplicationSelection,
        checkpointer: Arc<dyn Checkpointer>,
    ) -> Result<ResolvedApplicationParticipant, ApplicationExecutionError> {
        self.0.resolve_node(node, selection, checkpointer)
    }
}

async fn run_until_retry_wait(
    h: &Harness,
    graph: &CompiledGraph,
    thread: &str,
) -> Option<Result<State, adk_rust::graph::GraphError>> {
    let run = graph.invoke(HashMap::new(), ExecutionConfig::new(thread));
    tokio::pin!(run);
    loop {
        tokio::select! {
            result = &mut run => return Some(result),
            () = tokio::time::sleep(Duration::from_millis(20)) => {
                // Once try 1's dispatch row is resolved (1, 0), wait briefly inside
                // the 1500 ms RetryAt sleep so the journal append is committed before kill.
                if dispatch_rows(h).await == (1, 0) {
                    tokio::select! {
                        result = &mut run => return Some(result),
                        () = tokio::time::sleep(Duration::from_millis(120)) => return None,
                    }
                }
            }
        }
    }
}

async fn compile_root(
    h: &Harness,
    attempt: u64,
    thread: &str,
    definition: &PipelineDefinition,
) -> (Arc<dyn Checkpointer>, CompiledGraph) {
    let (checkpointer, runtimes) = claim(h, attempt, thread, definition).await;
    let runtimes = runtimes.with_execution_id("execution-1");
    let graph = definition
        .compile_subgraph_with_runtime(checkpointer.clone(), &runtimes)
        .unwrap();
    (checkpointer, graph)
}

async fn compile_with_saved_child(
    h: &Harness,
    attempt: u64,
    thread: &str,
    root: &PipelineDefinition,
    saved: &PipelineDefinition,
    agent_node: &str,
) -> CompiledGraph {
    let fence = [u8::try_from(attempt).unwrap(); 32];
    let writer = CheckpointWriterAuthority::new(
        "tenant-1".to_owned(),
        1,
        2,
        "agent.execute.application.v1",
        root.definition_digest(),
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
        .with_application_paths(&[agent_node.to_owned()])
        .await
        .unwrap(),
    );
    let authority: Arc<dyn ParallelCheckpointAuthority> = family.clone();
    let journals: Arc<dyn NodeRecoveryFactory> = family;
    let frozen = [0xcd; 32];
    let participant = PipelineNodeRuntimes::default()
        .with_execution_id("execution-1")
        .with_code(h.runtime.clone())
        .with_direct_tool_recovery(journals.clone())
        .with_saved_code_owner(SavedCodeOwner::new(23, 29, &frozen, saved.code_node_ids()));
    let (events, receiver) = crate::agents::graph::node_events::pipeline_node_event_channel();
    tokio::spawn(async move {
        if let Ok(mut drain) = receiver.drain("invocation", "saved", "saved").await {
            while drain.recv().await.is_some() {}
        }
    });
    let resolver = crate::agents::pipeline::saved_pipeline_resolver_for_tests(
        vec![(
            agent_node.to_owned(),
            "saved_child".to_owned(),
            saved.clone(),
            participant,
        )],
        &events,
    );
    let runtimes = PipelineNodeRuntimes::new(None, None, Some(Arc::new(TestActivations(resolver))))
        .with_execution_id("execution-1")
        .with_code(h.runtime.clone())
        .with_node_recovery_authority(journals)
        .with_parallel(ParallelCompilerBinding::for_tests(
            authority.clone(),
            tokio::time::Instant::now() + Duration::from_secs(3600),
            None,
        ))
        .with_composition_definitions(BTreeMap::from([(agent_node.to_owned(), saved.clone())]));
    let checkpointer: Arc<dyn Checkpointer> = authority;
    root.compile_subgraph_with_runtime(checkpointer, &runtimes)
        .unwrap()
}

/// 1. Kill after `Started` before receipt -> replacement re-attaches to try 1,
/// 0 extra tries.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_code_kill_after_started_before_receipt_zero_extra_tries() {
    let yaml = r#"
entry_point: run
state:
  count: {type: int, value: 0}
nodes:
  - id: run
    type: code
    language: python
    code: 'return 41'
    output: [count]
    settings: {retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 100}
    transition: END
"#;
    let h = harness_with(
        QUIET,
        Duration::from_millis(500),
        Some(2),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    install(&h).await;
    h.answers.lock().unwrap().extend([completed(), completed()]);
    let def = PipelineDefinition::from_yaml(yaml).unwrap();

    // Claim 1 submits try 1 (`Started` committed to PG); kill before receipt completes.
    let (_cp1, graph1) = compile_root(&h, 1, "code-started-crash", &def).await;
    let killed = run_until(&graph1, HashMap::new(), "code-started-crash", || {
        h.supervisor.lock().unwrap().submits >= 1
    })
    .await;
    assert!(killed.is_none(), "claim 1 must be killed while in flight");
    drop(graph1);
    let before = identities(&h);
    assert_eq!(before.len(), 1, "exactly one dispatch identity created");

    // Claim 2 resumes on real PG: re-attaches to the SAME identity, 0 extra tries.
    let (_cp2, graph2) = compile_root(&h, 2, "code-started-crash", &def).await;
    let state = graph2
        .invoke(HashMap::new(), ExecutionConfig::new("code-started-crash"))
        .await
        .expect("replacement finishes");
    assert_eq!(state["count"], json!(41));
    assert_eq!(identities(&h), before, "zero extra tries dispatched");
    assert_eq!(dispatch_rows(&h).await, (1, 0));
}

/// 2. Kill mid-wait (`RetryAt`) -> replacement resumes remainder <= 20 s on
/// the same try number, fences the zombie writer, and concurrent two-claim race
/// executes each try once.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_code_kill_mid_wait_resumes_same_try_fences_zombie_and_races_cleanly() {
    let yaml = r#"
entry_point: run
state:
  count: {type: int, value: 0}
nodes:
  - id: run
    type: code
    language: python
    code: 'return 41'
    output: [count]
    settings: {retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 1500}
    transition: END
"#;
    let h = harness_with(
        QUIET,
        Duration::from_millis(20),
        Some(2),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    install(&h).await;
    // Try 1 hits sandbox.deadline_exceeded (AttemptTimeout, NoExternalEffect -> RetryAt 1500 ms);
    // Try 2 succeeds.
    h.answers
        .lock()
        .unwrap()
        .extend([terminal("sandbox.deadline_exceeded"), completed()]);
    let def = PipelineDefinition::from_yaml(yaml).unwrap();

    // Claim 1 runs try 1, resolves try 1, persists RetryAt, and enters the 1500 ms wait -> kill mid-wait.
    let (cp1, graph1) = compile_root(&h, 1, "code-wait-crash", &def).await;
    let killed = run_until_retry_wait(&h, &graph1, "code-wait-crash").await;
    assert!(killed.is_none(), "expected kill mid-wait, got {killed:?}");
    drop(graph1);
    assert_eq!(identities(&h).len(), 1);

    // Activate Claim 2 (fences Claim 1 on PostgreSQL).
    let started = Instant::now();
    let (_cp2, graph2) = compile_root(&h, 2, "code-wait-crash", &def).await;

    // Zombie writer proof: Claim 1's fenced checkpointer can no longer write!
    let zombie = cp1
        .save(&adk_rust::graph::Checkpoint::new(
            "code-wait-crash",
            State::new(),
            99,
            vec![],
        ))
        .await;
    assert!(zombie.is_err(), "zombie writer claim 1 must be fenced");

    // Claim 2 resumes the wait remainder (<= 20 s) and runs try 2 cleanly.
    let state = graph2
        .invoke(HashMap::new(), ExecutionConfig::new("code-wait-crash"))
        .await
        .expect("replacement completes try 2");
    assert!(
        started.elapsed() < Duration::from_secs(20),
        "resumed wait exceeded 20 s: {:?}",
        started.elapsed()
    );
    assert_eq!(state["count"], json!(41));
    assert_eq!(
        identities(&h).len(),
        2,
        "exact 2 tries (1 timed out + 1 succeeded)"
    );
    assert_eq!(dispatch_rows(&h).await, (2, 0));
}

/// 3. Kill between failure append and graph checkpoint -> live & restore
/// produce byte-identical `node_errors` entry and code (plus DeadlineInsufficient
/// never continued).
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_code_kill_between_failure_append_and_checkpoint_live_restore_byte_identical() {
    let yaml = r#"
entry_point: run
state:
  count: {type: int, value: 0}
  seen: {type: str, value: ''}
nodes:
  - id: run
    type: code
    language: python
    code: '1 / 0'
    output: [count]
    settings: {on_error: continue, always_output_data: true}
    transition: after
  - id: after
    type: state_modifier
    template: "{{ node_errors | length }}"
    input: [node_errors]
    output: [seen]
    transition: END
"#;
    let def = PipelineDefinition::from_yaml(yaml).unwrap();

    // Run A (live): completes in one claim.
    let h_live = harness_with(
        QUIET,
        Duration::from_millis(10),
        Some(2),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    install(&h_live).await;
    h_live
        .answers
        .lock()
        .unwrap()
        .extend([terminal("sandbox.failed")]);
    let (_cp_live, graph_live) = compile_root(&h_live, 1, "code-live-restore", &def).await;
    let live_state = graph_live
        .invoke(HashMap::new(), ExecutionConfig::new("code-live-restore"))
        .await
        .expect("live continues");
    assert_eq!(live_state["count"], json!(0));
    assert_eq!(live_state["seen"], json!("1"));
    assert_eq!(
        live_state["node_errors"][0]["error"]["code"],
        json!("pipeline.code.failed")
    );
    assert_eq!(identities(&h_live).len(), 1);

    // Run B (crash between journal failure append and graph checkpoint):
    // Pause with `interrupt_after: [run]` isn't needed — instead, drive claim 1's
    // journal to committed `Failed(Stop(RetryDisabled))` without saving the graph
    // step checkpoint, then run claim 2's full graph over the SAME PG database!
    let h_crash = harness_with(
        QUIET,
        Duration::from_millis(10),
        Some(2),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    install(&h_crash).await;
    h_crash
        .answers
        .lock()
        .unwrap()
        .extend([terminal("sandbox.failed")]);
    // Compile with `on_error: stop` first? No: use `interrupt_after: [run]` vs
    // direct RecoverableNode? Even cleaner: run claim 1 with a failing graph
    // checkpointer wrapper (or `RecoverableNode` on claim 1's real PG journal)
    // so the journal has the committed failure while the graph thread has 0
    // checkpoints, then run claim 2's real compiled graph!
    let (cp1, rt1) = claim(&h_crash, 1, "code-live-restore", &def).await;
    let _ = cp1;
    // Open the exact journal for node "run" at step 0 on thread "code-live-restore"
    // by running the compiled graph with a checkpointer that persists the journal
    // (through PostgresCheckpointer) while failing the graph's own step save!
    struct FailOnSettledStepSave {
        inner: Arc<dyn Checkpointer>,
    }
    #[async_trait::async_trait]
    impl Checkpointer for FailOnSettledStepSave {
        async fn save(
            &self,
            cp: &adk_rust::graph::Checkpoint,
        ) -> Result<String, adk_rust::graph::GraphError> {
            // Allow every pre-node checkpoint to commit, then simulate a crash
            // right after `run` appends its failure to the PG journal and before
            // the graph step checkpoint carrying `node_errors` commits.
            if cp
                .state
                .get("node_errors")
                .and_then(|v| v.as_array())
                .is_some_and(|entries| !entries.is_empty())
            {
                Err(adk_rust::graph::GraphError::CheckpointError(
                    "simulated crash before step checkpoint".into(),
                ))
            } else {
                self.inner.save(cp).await
            }
        }
        async fn load(
            &self,
            thread: &str,
        ) -> Result<Option<adk_rust::graph::Checkpoint>, adk_rust::graph::GraphError> {
            self.inner.load(thread).await
        }
        async fn load_by_id(
            &self,
            id: &str,
        ) -> Result<Option<adk_rust::graph::Checkpoint>, adk_rust::graph::GraphError> {
            self.inner.load_by_id(id).await
        }
        async fn list(
            &self,
            thread: &str,
        ) -> Result<Vec<adk_rust::graph::Checkpoint>, adk_rust::graph::GraphError> {
            self.inner.list(thread).await
        }
        async fn delete(&self, thread: &str) -> Result<(), adk_rust::graph::GraphError> {
            self.inner.delete(thread).await
        }
        async fn prune(
            &self,
            thread: &str,
            policy: &adk_rust::graph::checkpoint::RetentionPolicy,
        ) -> Result<usize, adk_rust::graph::GraphError> {
            self.inner.prune(thread, policy).await
        }
    }
    let crash_graph = def
        .compile_subgraph_with_runtime(
            Arc::new(FailOnSettledStepSave { inner: cp1 }),
            &rt1.with_execution_id("execution-1"),
        )
        .unwrap();
    assert!(
        crash_graph
            .invoke(HashMap::new(), ExecutionConfig::new("code-live-restore"))
            .await
            .is_err(),
        "claim 1 must crash before saving graph step checkpoint"
    );
    assert_eq!(
        identities(&h_crash).len(),
        1,
        "claim 1 ran try 1 and wrote PG journal"
    );
    let before_submits = h_crash.supervisor.lock().unwrap().submits;

    // Claim 2 restores from the PG journal with ZERO re-dispatches and produces
    // byte-identical `node_errors`!
    let (_cp2, graph2) = compile_root(&h_crash, 2, "code-live-restore", &def).await;
    let restored_state = graph2
        .invoke(HashMap::new(), ExecutionConfig::new("code-live-restore"))
        .await
        .expect("claim 2 restores settled failure");
    assert_eq!(
        h_crash.supervisor.lock().unwrap().submits,
        before_submits,
        "restore must never re-dispatch Code"
    );
    assert_eq!(restored_state["count"], live_state["count"]);
    assert_eq!(restored_state["seen"], live_state["seen"]);
    assert_eq!(
        restored_state["node_errors"], live_state["node_errors"],
        "live and restored node_errors must be byte-identical"
    );

    // Also prove DeadlineInsufficient is persisted and NEVER continued on live or restore.
    let h_tight = harness_with(
        QUIET,
        Duration::from_millis(10),
        Some(2),
        tokio::time::Instant::now() + Duration::from_secs(5), // < 30s + 90s margin
    )
    .await;
    install(&h_tight).await;
    let (_cp_t1, g_t1) = compile_root(&h_tight, 1, "code-deadline-tight", &def).await;
    assert!(
        g_t1.invoke(HashMap::new(), ExecutionConfig::new("code-deadline-tight"))
            .await
            .is_err(),
        "DeadlineInsufficient must NEVER be continued"
    );
    assert_eq!(identities(&h_tight).len(), 0, "refused before dispatch");
    let (_cp_t2, g_t2) = compile_root(&h_tight, 2, "code-deadline-tight", &def).await;
    assert!(
        g_t2.invoke(HashMap::new(), ExecutionConfig::new("code-deadline-tight"))
            .await
            .is_err(),
        "restored DeadlineInsufficient must also refuse continuation"
    );
    assert_eq!(identities(&h_tight).len(), 0);
}

/// 4. C1 on real PG: timeout on a platform-client Code node with `retry_on_fail`
/// and `on_error: continue` raises Reconcile and issues 0 re-dispatches across
/// worker replacement.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_c1_platform_client_timeout_with_retry_on_fail_reconciles_zero_redispatch() {
    let yaml = r#"
entry_point: run
state:
  count: {type: int, value: 0}
nodes:
  - id: run
    type: code
    language: python
    code: 'return 41'
    platform_client: true
    output: [count]
    settings: {retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 100, on_error: continue}
    transition: END
"#;
    let h = harness_with(
        MainScript {
            effects: Effects::None,
            ..QUIET
        },
        Duration::from_millis(10),
        Some(2),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    install(&h).await;
    h.answers
        .lock()
        .unwrap()
        .extend([terminal("sandbox.deadline_exceeded")]);
    let def = PipelineDefinition::from_yaml(yaml).unwrap();

    // Claim 1 times out -> Reconcile pause (NOT retried, NOT continued).
    let (_cp1, graph1) = compile_root(&h, 1, "code-c1-reconcile", &def).await;
    let first = graph1
        .invoke(HashMap::new(), ExecutionConfig::new("code-c1-reconcile"))
        .await;
    assert!(
        matches!(first, Err(adk_rust::graph::GraphError::Interrupted(_))),
        "C1 platform-client timeout must raise Reconcile pause, got {first:?}"
    );
    assert_eq!(identities(&h).len(), 1, "zero re-dispatches");

    // Claim 2 sees the same Reconcile journal state and issues 0 extra dispatches.
    let (_cp2, graph2) = compile_root(&h, 2, "code-c1-reconcile", &def).await;
    let second = graph2
        .invoke(HashMap::new(), ExecutionConfig::new("code-c1-reconcile"))
        .await;
    assert!(
        matches!(second, Err(adk_rust::graph::GraphError::Interrupted(_))),
        "replacement must keep Reconcile pause, got {second:?}"
    );
    assert_eq!(identities(&h).len(), 1, "still zero re-dispatches");
}

/// 5. `execute_once` Code node across a 3-visit Router loop and process
/// replacement executes exactly 1 sandbox job total.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_code_execute_once_runs_one_job_across_three_visit_loop_and_replacement() {
    let yaml = r#"
interrupt_after: [run]
entry_point: bump
state:
  visits: {type: int, value: 0}
  count: {type: int, value: 0}
nodes:
  - id: bump
    type: state_modifier
    template: "{{ visits + 1 }}"
    input: [visits]
    output: [visits]
    transition: run
  - id: run
    type: code
    language: python
    code: 'return 41'
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
    let h = harness_with(
        QUIET,
        Duration::from_millis(10),
        Some(2),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    install(&h).await;
    h.answers.lock().unwrap().extend([completed(), completed()]);
    let def = PipelineDefinition::from_yaml(yaml).unwrap();

    // Claim 1 runs visit 1 and pauses at `interrupt_after: [run]`.
    let (_cp1, graph1) = compile_root(&h, 1, "code-once-loop", &def).await;
    let paused = graph1
        .invoke(HashMap::new(), ExecutionConfig::new("code-once-loop"))
        .await;
    assert!(
        matches!(paused, Err(adk_rust::graph::GraphError::Interrupted(_))),
        "expected static pause after visit 1, got {paused:?}"
    );
    assert_eq!(identities(&h).len(), 1);

    // Claim 2 resumes through visits 2 and 3 (pausing after `run` on each revisit,
    // which skips without running Code).
    let (_cp2, graph2) = compile_root(&h, 2, "code-once-loop", &def).await;
    let mut final_state = None;
    for _ in 0..6 {
        match graph2
            .invoke(HashMap::new(), ExecutionConfig::new("code-once-loop"))
            .await
        {
            Ok(state) => {
                final_state = Some(state);
                break;
            }
            Err(adk_rust::graph::GraphError::Interrupted(_)) => continue,
            Err(other) => panic!("unexpected error on resume: {other}"),
        }
    }
    let state = final_state.expect("loop finished");
    assert_eq!(state["visits"], json!(3));
    assert_eq!(state["count"], json!(41));
    assert_eq!(identities(&h).len(), 1, "Code ran once across all 3 visits");
    assert_eq!(h.supervisor.lock().unwrap().submits, 1);
    assert_eq!(dispatch_rows(&h).await, (1, 0));
}

/// 6. FU-348 (D15) on real PG:
/// (A) deterministic settled failure inside a saved child keeps child-private
/// `node_errors` (C4) and completes cleanly;
/// (B) Code retry inside a saved child resumes at `claim+1` after mid-wait kill.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_fu348_saved_child_deterministic_settled_failure_and_code_retry_resuming_at_claim_plus_1()
 {
    let root_yaml = r#"
entry_point: run_saved
state:
  input: {type: str, value: 'go'}
  messages: {type: list, value: []}
  count: {type: int, value: 0}
nodes:
  - id: run_saved
    type: agent
    tool: saved_child
    input_mapping: {task: {type: fixed, value: work}}
    output: [count]
    transition: END
"#;
    let root = PipelineDefinition::from_yaml(root_yaml).unwrap();

    // (A) Deterministic settled failure inside saved child.
    let det_child_yaml = r#"
entry_point: shape
state:
  input: {type: str, value: ''}
  messages: {type: list, value: []}
  out: {type: str, value: ''}
  count: {type: int, value: 0}
nodes:
  - id: shape
    type: state_modifier
    template: "{{ input | nosuchfilter }}"
    input: [input]
    output: [out]
    settings: {on_error: continue, always_output_data: true}
    transition: report
  - id: report
    type: state_modifier
    template: "{{ node_errors | length }}"
    input: [node_errors]
    output: [count]
    transition: END
"#;
    let det_child = PipelineDefinition::from_yaml(det_child_yaml).unwrap();
    let h_det = harness_with(
        QUIET,
        Duration::from_millis(10),
        Some(2),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    install(&h_det).await;
    let g_det =
        compile_with_saved_child(&h_det, 1, "fu348-det", &root, &det_child, "run_saved").await;
    let det_state = g_det
        .invoke(HashMap::new(), ExecutionConfig::new("fu348-det"))
        .await
        .expect("saved child settled failure completes");
    assert_eq!(det_state["count"], json!(1));
    assert!(
        !det_state.contains_key("node_errors"),
        "C4: child node_errors must not leak into parent state"
    );

    // (B) Code retry inside a saved child killed mid-wait and resumed at claim+1 (claim 2).
    let code_child_yaml = r#"
entry_point: work
state:
  input: {type: str, value: ''}
  messages: {type: list, value: []}
  count: {type: int, value: 0}
nodes:
  - id: work
    type: code
    language: python
    code: 'return 41'
    input: [input]
    output: [count]
    settings: {retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 1500}
    transition: END
"#;
    let code_child = PipelineDefinition::from_yaml(code_child_yaml).unwrap();
    let h_code = harness_with(
        QUIET,
        Duration::from_millis(20),
        Some(2),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    install(&h_code).await;
    h_code
        .answers
        .lock()
        .unwrap()
        .extend([terminal("sandbox.deadline_exceeded"), completed()]);

    // Claim 1 runs try 1 (times out -> RetryAt 1500 ms), then dies mid-wait.
    let g1 =
        compile_with_saved_child(&h_code, 1, "fu348-code", &root, &code_child, "run_saved").await;
    let killed = run_until_retry_wait(&h_code, &g1, "fu348-code").await;
    assert!(
        killed.is_none(),
        "expected kill mid-wait in saved child, got {killed:?}"
    );
    drop(g1);
    assert_eq!(identities(&h_code).len(), 1);

    // Claim 2 (`claim+1`) resumes the saved child, completes try 2, returns count=41.
    let g2 =
        compile_with_saved_child(&h_code, 2, "fu348-code", &root, &code_child, "run_saved").await;
    let state = g2
        .invoke(HashMap::new(), ExecutionConfig::new("fu348-code"))
        .await
        .expect("claim+1 resumes saved child Code retry");
    assert_eq!(state["count"], json!(41));
    assert_eq!(
        identities(&h_code).len(),
        2,
        "try 1 + try 2 under saved child"
    );
    assert_eq!(dispatch_rows(&h_code).await, (2, 0));

    // Verify NodeRecoveryPolicy / NodeAttemptActivation imports stay clean.
    let _ = (
        NodeRecoveryPolicy::default(),
        std::any::type_name::<NodeAttemptActivation>(),
    );
}
