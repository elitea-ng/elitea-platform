//! W3-2b on real PostgreSQL: Code inside a saved pipeline, run by an Agent
//! node of the run root (the root case) and by a Map's Agent worker (depth 2:
//! a fan-out member, then the saved pipeline, then Code). The graph is
//! compiled by the production compiler with the production saved-pipeline
//! resolver (`NativePipelineApplicationResolver`, which binds each use's
//! owner scope) over the production checkpoint family, which lends the saved
//! child its node journals; the wiring harness's production
//! `RemoteCodeRuntime`, fake Main and fake Supervisor answer. A Worker
//! replacement is a dropped run and a later claim resuming. Main's half of
//! the same shapes runs on Main's production repository path
//! (`saved_code_child_real_path_postgres_integration_test.go`).

use super::*;
use crate::agents::graph::application::{
    ApplicationExecutionError, PipelineApplicationResolver, PipelineApplicationSelection,
    ResolvedApplicationParticipant,
};
use crate::agents::pipeline::saved_code_child::{
    SAVED_CODE_CHILD_SCOPE_SCHEMA, SavedCodeFanoutMember, SavedCodeOwner,
};

/// The saved pipeline (23, 29): `validate (Code) → route_quality (Router) →
/// normalize (Code)`, the ETL `silver_partition` shape.
fn saved_yaml(platform_client: bool) -> String {
    let platform = if platform_client {
        ", platform_client: true"
    } else {
        ""
    };
    format!(
        "entry_point: validate\nstate:\n  input: {{type: str, value: ''}}\n  messages: {{type: list, value: []}}\n  count: {{type: int, value: 0}}\nnodes:\n  - {{id: validate, type: code, language: python, code: 'return 41', input: [input], output: [count]{platform}, transition: route_quality}}\n  - {{id: route_quality, type: router, condition: normalize, routes: [normalize], default_output: END, input: [count]}}\n  - {{id: normalize, type: code, language: python, code: 'return 41', input: [count], output: [count], transition: END}}\n"
    )
}

/// The root case: the root's Agent node runs the saved pipeline.
const ROOT_YAML: &str = "entry_point: run_saved\nstate:\n  input: {type: str, value: p0}\n  messages: {type: list, value: []}\n  count: {type: int, value: 0}\nnodes:\n  - {id: run_saved, type: agent, tool: silver_partition, input_mapping: {task: {type: fixed, value: partition}}, output: [count], transition: END}\n";

/// Depth 2: a Parallel whose two Agent branches each run the saved pipeline.
const PARALLEL_YAML: &str = "entry_point: gather\nstate:\n  input: {type: str, value: p0}\n  count: {type: int, value: 0}\n  joined: {type: list, value: []}\nnodes:\n  - {id: gather, type: parallel, branches: [{id: left, node: left_saved}, {id: right, node: right_saved}], max_concurrency: 2, wait: all, output: [joined], transition: END}\n  - {id: left_saved, type: agent, tool: silver_partition, input: [input], input_mapping: {task: {type: fixed, value: partition}}, output: [count]}\n  - {id: right_saved, type: agent, tool: silver_partition, input: [input], input_mapping: {task: {type: fixed, value: partition}}, output: [count]}\n";

/// Depth 2: a Map whose Agent worker runs the saved pipeline.
const MAP_YAML: &str = "entry_point: each\nstate:\n  items: {type: list, value: []}\n  count: {type: int, value: 0}\n  mapped: {type: list, value: []}\nnodes:\n  - {id: each, type: map, worker: silver, source: items, item: entity, index: item_index, outputs: [count], destination: mapped, max_items: 8, max_concurrency: 2, reduction: ordered_collection, transition: END}\n  - {id: silver, type: agent, tool: silver_partition, input: [entity, item_index], input_mapping: {task: {type: fixed, value: partition}}, output: [count]}\n";

/// Main's frozen digest of (23, 29), as its nested version route answers it.
const FROZEN: [u8; 32] = [0xab; 32];

/// The production resolver, with the activation checkpoints a scoped graph
/// call owner gives it in production (the saved child's visit), and a proved
/// participant fingerprint for the Map worker.
struct Activations(Arc<dyn PipelineApplicationResolver>);

impl PipelineApplicationResolver for Activations {
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

/// One claim's fenced writer family on `thread`, admitting `paths` (the Agent
/// nodes running the saved child), and the runtimes the production composition binds:
/// the root's Code runtime, journals and fan-out binding, and the saved
/// pipeline as the Agent node's participant with Main's freeze as its owner.
async fn compiled(
    h: &Harness,
    attempt: u64,
    thread: &str,
    root: &PipelineDefinition,
    paths: &[&str],
    platform_client: bool,
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
        .with_application_paths(
            &paths
                .iter()
                .map(|path| (*path).to_owned())
                .collect::<Vec<_>>(),
        )
        .await
        .unwrap(),
    );
    let authority: Arc<dyn ParallelCheckpointAuthority> = family.clone();
    let journals: Arc<dyn NodeRecoveryFactory> = family;
    let saved = PipelineDefinition::from_yaml(&saved_yaml(platform_client)).unwrap();
    let participant = PipelineNodeRuntimes::default()
        .with_code(h.runtime.clone())
        .with_direct_tool_recovery(journals.clone())
        .with_saved_code_owner(SavedCodeOwner::new(23, 29, &FROZEN, saved.code_node_ids()));
    let (events, receiver) = crate::agents::graph::node_events::pipeline_node_event_channel();
    // The chat projector's part: the saved child's node events are drained.
    tokio::spawn(async move {
        if let Ok(mut drain) = receiver.drain("invocation", "saved", "saved").await {
            while drain.recv().await.is_some() {}
        }
    });
    let resolver = crate::agents::pipeline::saved_pipeline_resolver_for_tests(
        paths
            .iter()
            .map(|path| {
                (
                    (*path).to_owned(),
                    "silver_partition".to_owned(),
                    saved.clone(),
                    participant.clone(),
                )
            })
            .collect(),
        &events,
    );
    let runtimes = PipelineNodeRuntimes::new(None, None, Some(Arc::new(Activations(resolver))))
        .with_code(h.runtime.clone())
        .with_node_recovery_authority(journals)
        .with_parallel(ParallelCompilerBinding::for_tests(
            authority.clone(),
            tokio::time::Instant::now() + Duration::from_secs(3600),
            None,
        ))
        .with_composition_definitions(
            paths
                .iter()
                .map(|path| ((*path).to_owned(), saved.clone()))
                .collect(),
        );
    let checkpointer: Arc<dyn Checkpointer> = authority;
    root.compile_subgraph_with_runtime(checkpointer, &runtimes)
        .unwrap()
}

fn scopes(h: &Harness) -> Vec<serde_json::Value> {
    h.main.lock().unwrap().visit_scopes.clone()
}

/// Every visit names the saved pipeline Main froze, its Agent node and its
/// own subgraph thread under `parent`; `fanout` is the member's family kind
/// and fan-out node, or none.
fn assert_saved_scopes(h: &Harness, agents: &[&str], fanout: Option<(&str, &str)>) {
    let scopes = scopes(h);
    assert!(!scopes.is_empty(), "no visit was registered");
    for scope in scopes {
        assert_eq!(scope["schema"], SAVED_CODE_CHILD_SCOPE_SCHEMA, "{scope}");
        assert_eq!(scope["application_id"], 23);
        assert_eq!(scope["version_id"], 29);
        assert_eq!(scope["definition_sha256"], "ab".repeat(32));
        let agent = scope["agent_node_id"].as_str().unwrap();
        assert!(agents.contains(&agent), "{scope}");
        let parent = scope["parent_thread_id"].as_str().unwrap();
        assert_eq!(scope["child_thread_id"], format!("{parent}/{agent}"));
        assert!(
            matches!(
                scope["code_node_id"].as_str(),
                Some("validate" | "normalize")
            ),
            "{scope}"
        );
        match fanout {
            None => assert!(scope["fanout"].is_null(), "{scope}"),
            Some((kind, node)) => {
                assert_eq!(scope["fanout"]["kind"], kind, "{scope}");
                assert_eq!(scope["fanout"]["node_id"], node);
                let prefix = if kind == "map_item" { "m1:" } else { "p1:" };
                assert!(parent.starts_with(prefix), "{scope}");
            }
        }
    }
}

/// Root case, Worker kill (R): the Worker dies with `validate` submitted; the
/// replacement resumes the saved child at `validate`, which re-attaches to its
/// own dispatch identity (never a new one), then `normalize` runs once, and
/// the root lands one answer.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_a_saved_child_code_at_the_root_survives_worker_replacement() {
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
    let root = PipelineDefinition::from_yaml(ROOT_YAML).unwrap();
    let graph = compiled(&h, 1, "saved-root", &root, &["run_saved"], false).await;
    let killed = run_until(&graph, HashMap::new(), "saved-root", || {
        h.supervisor.lock().unwrap().submits >= 1
    })
    .await;
    assert!(
        killed.is_none(),
        "the first Worker died inside the saved child's Code"
    );
    drop(graph);
    let before = identities(&h);
    assert_eq!(before.len(), 1, "validate reached its intent");
    let graph = compiled(&h, 2, "saved-root", &root, &["run_saved"], false).await;
    let state = graph
        .invoke(HashMap::new(), ExecutionConfig::new("saved-root"))
        .await
        .expect("the replacement Worker completes the saved child");
    assert_eq!(state["count"], 41);
    let after = identities(&h);
    assert_eq!(after.len(), 2, "validate and normalize, one identity each");
    assert_eq!(after[0], before[0], "validate re-attached to its identity");
    assert_eq!(dispatch_rows(&h).await, (2, 0), "all resolved");
    // validate, its re-attachment, normalize: nothing ran twice.
    assert_eq!(h.supervisor.lock().unwrap().submits, 3);
    assert_saved_scopes(&h, &["run_saved"], None);
}

/// Root case, Supervisor kill (I): the Supervisor loses `validate`'s runtime;
/// it is re-run once with a new identity (FU-214) and the child completes.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_a_supervisor_losing_a_saved_child_code_attempt_at_the_root_reruns_it_once() {
    let h = harness_with(
        QUIET,
        Duration::ZERO,
        Some(2),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    install(&h).await;
    h.answers.lock().unwrap().extend(
        std::iter::once(terminal("sandbox.runtime_lost")).chain((0..4).map(|_| completed())),
    );
    let root = PipelineDefinition::from_yaml(ROOT_YAML).unwrap();
    let graph = compiled(&h, 1, "saved-root", &root, &["run_saved"], false).await;
    let state = graph
        .invoke(HashMap::new(), ExecutionConfig::new("saved-root"))
        .await
        .expect("the lost attempt re-runs once");
    assert_eq!(state["count"], 41);
    assert_eq!(h.supervisor.lock().unwrap().submits, 3);
    assert_eq!(
        identities(&h).len(),
        3,
        "one new identity for the lost attempt"
    );
    assert_eq!(dispatch_rows(&h).await, (3, 0));
    assert_saved_scopes(&h, &["run_saved"], None);
}

/// Root case, a platform-client Code attempt killed after dispatch, then its
/// runtime lost (C): a platform call may have run, so it is never re-run with
/// a new identity. With the owner's journal proving nothing unresolved it is
/// reconciled and settles typed; without that proof it ends
/// `child_effect_unknown` (the saved child has no operator route). A later
/// claim repeats neither.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_a_saved_child_platform_client_code_at_the_root_is_reconciled_never_rerun() {
    for effects in [Effects::None, Effects::Possible] {
        let h = harness_with(
            MainScript { effects, ..QUIET },
            Duration::from_millis(1500),
            Some(2),
            tokio::time::Instant::now() + Duration::from_secs(3600),
        )
        .await;
        install(&h).await;
        h.answers.lock().unwrap().extend([
            terminal("sandbox.runtime_lost"),
            terminal("sandbox.runtime_lost"),
            terminal("sandbox.runtime_lost"),
        ]);
        let root = PipelineDefinition::from_yaml(ROOT_YAML).unwrap();
        let graph = compiled(&h, 1, "saved-root", &root, &["run_saved"], true).await;
        let killed = run_until(&graph, HashMap::new(), "saved-root", || {
            h.supervisor.lock().unwrap().submits >= 1
        })
        .await;
        assert!(killed.is_none());
        drop(graph);
        for attempt in [2, 3] {
            let graph = compiled(&h, attempt, "saved-root", &root, &["run_saved"], true).await;
            let error = graph
                .invoke(HashMap::new(), ExecutionConfig::new("saved-root"))
                .await
                .expect_err("the saved child's Code ends typed");
            let unknown = error
                .to_string()
                .contains(crate::agents::graph::node_recovery_runtime::CHILD_EFFECT_UNKNOWN_CODE);
            assert_eq!(
                unknown,
                matches!(effects, Effects::Possible),
                "claim {attempt}: {error}"
            );
            assert!(
                h.main.lock().unwrap().effects >= 1,
                "the owner journal was asked"
            );
            assert_eq!(identities(&h).len(), 1, "never re-run with a new identity");
        }
        assert_eq!(dispatch_rows(&h).await.0, 1, "one dispatch identity");
        assert_saved_scopes(&h, &["run_saved"], None);
    }
}

/// Depth 2, Worker kill (R): a Map of 4 items whose Agent worker runs the
/// saved pipeline; the Worker dies with Code attempts in flight. The
/// replacement resumes the frozen occurrence: every started attempt keeps its
/// identity, every item lands once, and each visit names its own member.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_a_map_of_saved_children_with_code_survives_worker_replacement() {
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
    let root = PipelineDefinition::from_yaml(MAP_YAML).unwrap();
    let graph = compiled(&h, 1, "saved-map", &root, &["silver"], false).await;
    let killed = run_until(&graph, items(4), "saved-map", || {
        h.supervisor.lock().unwrap().submits >= 4
    })
    .await;
    assert!(killed.is_none(), "the first Worker died mid-Map");
    drop(graph);
    let before = identities(&h);
    let first_visits = scopes(&h).len();
    let graph = compiled(&h, 2, "saved-map", &root, &["silver"], false).await;
    let state = graph
        .invoke(HashMap::new(), ExecutionConfig::new("saved-map"))
        .await
        .expect("the replacement Worker completes the Map");
    let mapped = state["mapped"].as_array().unwrap();
    assert_eq!(mapped.len(), 4, "{mapped:?}");
    for (index, entry) in mapped.iter().enumerate() {
        assert_eq!(entry["index"], index);
        assert_eq!(entry["outputs"]["count"], 41);
    }
    let after = identities(&h);
    assert_eq!(after.len(), 8, "two Code attempts per item, none re-run");
    assert!(
        before.iter().all(|identity| after.contains(identity)),
        "every started attempt kept its identity"
    );
    assert_eq!(dispatch_rows(&h).await, (8, 0), "all resolved");
    assert_saved_scopes(&h, &["silver"], Some(("map_item", "each")));
    // C4: the recovered occurrence knows its frozen family: every visit the
    // replacement registered names its member and the frozen item count.
    let resumed = &scopes(&h)[first_visits..];
    assert!(!resumed.is_empty(), "the replacement registered visits");
    for scope in resumed {
        assert_eq!(scope["fanout"]["item_count"], 4, "{scope}");
    }
    let members = scopes(&h)
        .iter()
        .map(|scope| scope["fanout"]["ordinal"].as_u64().unwrap())
        .collect::<std::collections::BTreeSet<_>>();
    assert_eq!(members, std::collections::BTreeSet::from([0, 1, 2, 3]));
}

/// Depth 2, Supervisor kill (I): the Supervisor loses two items' first
/// runtime; each is re-run once with a new identity and every item lands.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_a_supervisor_losing_saved_child_code_in_a_map_reruns_each_once() {
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
    let root = PipelineDefinition::from_yaml(MAP_YAML).unwrap();
    let graph = compiled(&h, 1, "saved-map", &root, &["silver"], false).await;
    let state = graph
        .invoke(items(2), ExecutionConfig::new("saved-map"))
        .await
        .expect("the lost attempts re-run once");
    let mapped = state["mapped"].as_array().unwrap();
    assert_eq!(mapped.len(), 2, "{mapped:?}");
    assert_eq!(h.supervisor.lock().unwrap().submits, 6);
    assert_eq!(
        identities(&h).len(),
        6,
        "two lost attempts, one new identity each"
    );
    assert_eq!(dispatch_rows(&h).await, (6, 0));
    assert_saved_scopes(&h, &["silver"], Some(("map_item", "each")));
}

/// Depth 2, a platform-client Code attempt of a Map item killed after
/// dispatch, then its runtime lost (C), as at the root.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_a_saved_child_platform_client_code_in_a_map_is_reconciled_never_rerun() {
    for effects in [Effects::None, Effects::Possible] {
        let h = harness_with(
            MainScript { effects, ..QUIET },
            Duration::from_millis(1500),
            Some(2),
            tokio::time::Instant::now() + Duration::from_secs(3600),
        )
        .await;
        install(&h).await;
        h.answers.lock().unwrap().extend([
            terminal("sandbox.runtime_lost"),
            terminal("sandbox.runtime_lost"),
            terminal("sandbox.runtime_lost"),
        ]);
        let root = PipelineDefinition::from_yaml(MAP_YAML).unwrap();
        let graph = compiled(&h, 1, "saved-map", &root, &["silver"], true).await;
        let killed = run_until(&graph, items(1), "saved-map", || {
            h.supervisor.lock().unwrap().submits >= 1
        })
        .await;
        assert!(killed.is_none());
        drop(graph);
        for attempt in [2, 3] {
            let graph = compiled(&h, attempt, "saved-map", &root, &["silver"], true).await;
            let error = graph
                .invoke(HashMap::new(), ExecutionConfig::new("saved-map"))
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
        assert_eq!(dispatch_rows(&h).await.0, 1, "one dispatch identity");
        assert_saved_scopes(&h, &["silver"], Some(("map_item", "each")));
    }
}

/// Depth 2, Parallel branches, Worker kill (R/I): each branch runs the saved
/// pipeline; the Worker dies once both `validate` attempts completed and the
/// `normalize` attempts were submitted. The replacement resumes the frozen
/// occurrence: the completed attempts replay and are never dispatched again,
/// the in-flight ones re-attach to their own identity, and each branch lands
/// one answer under its own member scope.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_a_parallel_of_saved_children_with_code_survives_worker_replacement() {
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
    let root = PipelineDefinition::from_yaml(PARALLEL_YAML).unwrap();
    let paths = ["left_saved", "right_saved"];
    let graph = compiled(&h, 1, "saved-parallel", &root, &paths, false).await;
    let killed = run_until(&graph, HashMap::new(), "saved-parallel", || {
        h.supervisor.lock().unwrap().submits >= 4
    })
    .await;
    assert!(killed.is_none(), "the first Worker died mid-Parallel");
    drop(graph);
    let before = identities(&h);
    assert_eq!(before.len(), 4, "both branches reached normalize");
    let graph = compiled(&h, 2, "saved-parallel", &root, &paths, false).await;
    let state = graph
        .invoke(HashMap::new(), ExecutionConfig::new("saved-parallel"))
        .await
        .expect("the replacement Worker completes the Parallel");
    let joined = state["joined"].as_array().unwrap();
    assert_eq!(joined.len(), 2, "one answer per branch: {joined:?}");
    for entry in joined {
        assert_eq!(entry["status"], "succeeded", "{entry}");
        assert_eq!(entry["outputs"]["count"], 41);
    }
    assert_eq!(
        identities(&h),
        before,
        "nothing was dispatched again under a new identity"
    );
    assert_eq!(dispatch_rows(&h).await, (4, 0), "all resolved");
    // 4 first submissions + the 2 in-flight re-attachments: the completed
    // validate attempts were never submitted again.
    assert_eq!(h.supervisor.lock().unwrap().submits, 6);
    assert_saved_scopes(
        &h,
        &["left_saved", "right_saved"],
        Some(("parallel_branch", "gather")),
    );
    let members = scopes(&h)
        .iter()
        .map(|scope| scope["fanout"]["ordinal"].as_u64().unwrap())
        .collect::<std::collections::BTreeSet<_>>();
    assert_eq!(members, std::collections::BTreeSet::from([0, 1]));
}
