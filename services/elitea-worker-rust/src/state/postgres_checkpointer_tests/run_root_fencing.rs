//! Every thread a run opens below its root is fenced by the run's root writer, not
//! by its own parent: a claim superseded at the root cannot open new threads even
//! while its in-process lease and its older child threads are still current.
use super::*;
use crate::agents::graph::node_recovery::NodeRecoveryPolicy;
use crate::agents::graph::node_recovery_runtime::{NodeAttemptActivation, NodeRecoveryFactory};

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

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn superseded_claim_cannot_activate_application_child_threads() {
    let database = database().await;
    let claim_a = root(database.pool.clone(), 1).await;
    let claim_b = root(database.pool.clone(), 2).await;

    let Err(error) = claim_a
        .with_application_children(["delegate"].into_iter())
        .await
    else {
        panic!("a superseded claim activated an application child thread");
    };
    assert!(
        matches!(error, PostgresCheckpointError::WriterNotCurrent),
        "{error:?}"
    );
    assert_eq!(writers(&database.pool, "thread-1/%").await, 0);

    claim_b
        .with_application_children(["delegate"].into_iter())
        .await
        .expect("the current owner activates its child thread");
    assert_eq!(writers(&database.pool, "thread-1/%").await, 1);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn child_thread_of_a_superseded_run_cannot_open_new_threads() {
    let database = database().await;
    let family = root(database.pool.clone(), 2)
        .await
        .with_application_children(["delegate"].into_iter())
        .await
        .expect("activate the child thread before the takeover");
    let child = family
        .for_thread("thread-1/delegate")
        .expect("admitted child thread");

    // A newer claim takes over the run root only. The old child thread is still
    // owned by the old claim, so only the run root can refuse what it opens next.
    let _claim_c = root(database.pool.clone(), 3).await;

    let activation = NodeAttemptActivation {
        root_thread_id: "thread-1/delegate".to_owned(),
        node_id: "lookup".to_owned(),
        step: 2,
        node_digest: [0x07; 32],
        input_digest: [0x09; 32],
        visit: Some([0x0b; 32]),
    };
    let Err(error) = child
        .open(&activation, &NodeRecoveryPolicy::default())
        .await
    else {
        panic!("a child thread of a superseded run opened a node journal");
    };
    assert!(error.to_string().contains("writer_not_current"), "{error}");
    assert_eq!(writers(&database.pool, "n1:%").await, 0);
}

/// FU-248: a run root's node journal identity is unchanged by saved-child
/// journals (journals already written stay addressable), and a child's visit
/// changes the identity.
#[test]
fn a_root_node_journal_keeps_its_identity_and_a_child_visit_changes_it() {
    let authority = writer("execution-1", "claim-1", 1, 1, [0x41; 32], [0x61; 32]);
    let mut activation = NodeAttemptActivation {
        root_thread_id: "thread-1".to_owned(),
        node_id: "lookup".to_owned(),
        step: 2,
        node_digest: [0x07; 32],
        input_digest: [0x09; 32],
        visit: None,
    };
    let policy = NodeRecoveryPolicy::default();
    let root = crate::state::postgres_checkpointer::journal_activation_id(
        &authority,
        &activation,
        &policy,
    )
    .expect("root identity");
    let hex = root.iter().fold(String::new(), |mut hex, byte| {
        use std::fmt::Write as _;
        let _ = write!(hex, "{byte:02x}");
        hex
    });
    assert_eq!(hex, PINNED_ROOT_JOURNAL_ID);
    activation.visit = Some([0x0b; 32]);
    let visited = crate::state::postgres_checkpointer::journal_activation_id(
        &authority,
        &activation,
        &policy,
    )
    .expect("child identity");
    activation.visit = Some([0x0c; 32]);
    let other = crate::state::postgres_checkpointer::journal_activation_id(
        &authority,
        &activation,
        &policy,
    )
    .expect("other visit");
    assert_ne!(visited, root);
    assert_ne!(visited, other);
}

/// The identity main's root journals have (computed before FU-248).
const PINNED_ROOT_JOURNAL_ID: &str =
    "0e7d43752e08d145b1c6190933615269fe35380fe436de76e3e168e5a861917d";

/// FU-248: a child thread's journal opens only with its visit, and the run
/// root's only without one. There is no unscoped fallback either way.
#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn node_journals_are_visit_scoped_in_a_child_and_unscoped_on_the_root() {
    let database = database().await;
    let family = root(database.pool.clone(), 1)
        .await
        .with_application_children(["delegate"].into_iter())
        .await
        .expect("activate the child thread");
    let activation = |thread: &str, visit: Option<[u8; 32]>| NodeAttemptActivation {
        root_thread_id: thread.to_owned(),
        node_id: "lookup".to_owned(),
        step: 2,
        node_digest: [0x07; 32],
        input_digest: [0x09; 32],
        visit,
    };
    let policy = NodeRecoveryPolicy::default();
    let child = family
        .for_thread("thread-1/delegate")
        .expect("child thread");
    let run_root = family.for_thread("thread-1").expect("run root");
    for (opener, activation) in [
        (child, activation("thread-1/delegate", None)),
        (run_root, activation("thread-1", Some([0x0b; 32]))),
    ] {
        let Err(error) = opener.open(&activation, &policy).await else {
            panic!("a journal opened outside its visit scope");
        };
        assert!(error.to_string().contains("unscoped_child"), "{error}");
    }
    child
        .open(&activation("thread-1/delegate", Some([0x0b; 32])), &policy)
        .await
        .expect("a child journal opens with its visit");
    run_root
        .open(&activation("thread-1", None), &policy)
        .await
        .expect("a root journal opens without one");
}

/// REVIEW-1541 R2: the journal factories lent to saved children, called as
/// they are lent (`Arc<dyn NodeRecoveryFactory>`), open journals only for
/// their own admitted threads, and only while the claim owns the run: the run
/// root's family refuses an unadmitted thread and a branch's thread, a fixed
/// Parallel branch family refuses the root family's threads, and both refuse
/// once the claim is superseded. No refused open writes a journal row.
#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn lent_journal_factories_open_only_their_own_admitted_threads_under_the_claim() {
    let database = database().await;
    let family = Arc::new(
        root(database.pool.clone(), 1)
            .await
            .with_application_children(["delegate", "fetch_short"].into_iter())
            .await
            .expect("activate the admitted child threads"),
    );
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
    let parallel_activation = ParallelActivation {
        root_thread_id: "thread-1".to_owned(),
        node_id: parallel.id().to_owned(),
        step: 4,
        config_digest: parallel.config_digest(),
        map: None,
    };
    let origin = family
        .child_origin(&parallel_activation)
        .expect("parallel child origin");
    let branch = family
        .for_branch(
            &parallel_activation,
            &parallel.branches()[0],
            0,
            &[0x42; 32],
            &origin,
        )
        .await
        .expect("activate the branch family");
    let branch_child = format!("{}/fetch_short", branch.thread_id);
    assert!(
        branch.admitted_threads.contains(&branch_child),
        "{branch_child}"
    );
    let root_lent: Arc<dyn NodeRecoveryFactory> = family.clone();
    let branch_lent = branch
        .node_recovery
        .clone()
        .expect("the branch family is lent");
    let activation = |thread: &str| NodeAttemptActivation {
        root_thread_id: thread.to_owned(),
        node_id: "lookup".to_owned(),
        step: 2,
        node_digest: [0x07; 32],
        input_digest: [0x09; 32],
        visit: Some([0x0b; 32]),
    };
    let policy = NodeRecoveryPolicy::default();
    for (lent, thread) in [
        (&root_lent, "thread-1/unadmitted".to_owned()),
        (&root_lent, branch_child.clone()),
        (&branch_lent, "thread-1/delegate".to_owned()),
        (&branch_lent, format!("{}/unadmitted", branch.thread_id)),
    ] {
        assert!(
            lent.open(&activation(&thread), &policy).await.is_err(),
            "a lent factory opened a journal for {thread}"
        );
    }
    assert_eq!(writers(&database.pool, "n1:%").await, 0);
    // Each opens its own admitted thread.
    root_lent
        .open(&activation("thread-1/delegate"), &policy)
        .await
        .expect("the root family opens its child's journal");
    branch_lent
        .open(&activation(&branch_child), &policy)
        .await
        .expect("the branch family opens its member child's journal");
    assert_eq!(writers(&database.pool, "n1:%").await, 2);

    // A newer claim takes the run root: neither lent factory opens again.
    let _claim_2 = root(database.pool.clone(), 2).await;
    let mut other = activation("thread-1/delegate");
    other.step = 3;
    let mut branch_other = activation(&branch_child);
    branch_other.step = 3;
    for (lent, activation) in [(&root_lent, other), (&branch_lent, branch_other)] {
        let Err(error) = lent.open(&activation, &policy).await else {
            panic!("a lent factory of a superseded claim opened a journal");
        };
        assert!(error.to_string().contains("writer_not_current"), "{error}");
    }
    assert_eq!(writers(&database.pool, "n1:%").await, 2);
}

/// FU-358: pipeline-tool node journals use a distinct domain tag from run-root
/// and subgraph node journals, so a tool call can never collide with a root or
/// subgraph journal even if a model picks a call id matching a graph node name.
#[test]
fn pipeline_tool_journal_identity_is_domain_separated_from_root_and_subgraph_journals() {
    let authority = writer("execution-1", "claim-1", 1, 1, [0x41; 32], [0x61; 32]);
    let mut activation = NodeAttemptActivation {
        root_thread_id: "thread-1/delegate".to_owned(),
        node_id: "lookup".to_owned(),
        step: 2,
        node_digest: [0x07; 32],
        input_digest: [0x09; 32],
        visit: Some([0x0b; 32]),
    };
    let policy = NodeRecoveryPolicy::default();
    let subgraph_id = crate::state::postgres_checkpointer::journal_activation_id(
        &authority,
        &activation,
        &policy,
    )
    .expect("subgraph identity");
    let tool_id = crate::state::postgres_checkpointer::pipeline_tool_journal_activation_id(
        &authority,
        &activation,
        &policy,
    )
    .expect("pipeline tool identity");
    assert_ne!(tool_id, subgraph_id);

    activation.visit = Some([0x0c; 32]);
    let other_visit = crate::state::postgres_checkpointer::pipeline_tool_journal_activation_id(
        &authority,
        &activation,
        &policy,
    )
    .expect("other visit identity");
    assert_ne!(tool_id, other_visit);

    activation.visit = Some([0x0b; 32]);
    activation.root_thread_id = "thread-1/call-2".to_owned();
    let other_call = crate::state::postgres_checkpointer::pipeline_tool_journal_activation_id(
        &authority,
        &activation,
        &policy,
    )
    .expect("other call identity");
    assert_ne!(tool_id, other_call);
}

struct CountAttemptBody {
    invocations: Arc<std::sync::atomic::AtomicUsize>,
}

#[async_trait::async_trait]
impl adk_rust::graph::Node for CountAttemptBody {
    fn name(&self) -> &'static str {
        "lookup"
    }
    async fn execute(
        &self,
        _: &adk_rust::graph::NodeContext,
    ) -> Result<adk_rust::graph::NodeOutput, adk_rust::graph::GraphError> {
        unreachable!()
    }
}

#[async_trait::async_trait]
impl crate::agents::graph::node_recovery_runtime::NodeAttemptBody for CountAttemptBody {
    async fn execute_attempt(
        &self,
        _: &adk_rust::graph::NodeContext,
        _: &crate::agents::graph::node_recovery_runtime::NodeAttemptAuthority,
    ) -> Result<adk_rust::graph::NodeOutput, crate::agents::graph::node_recovery::NodeFailure> {
        self.invocations
            .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        Ok(adk_rust::graph::NodeOutput::new().with_update("result", serde_json::json!("ok")))
    }
}

fn tool_activation(thread: &str, step: u64, visit: Option<[u8; 32]>) -> NodeAttemptActivation {
    NodeAttemptActivation {
        root_thread_id: thread.to_owned(),
        node_id: "lookup".to_owned(),
        step,
        node_digest: [0x07; 32],
        input_digest: [0x09; 32],
        visit,
    }
}

/// FU-358 (pipeline root): `ApplicationCheckpointers` lends a call-scoped
/// journal factory that admits only the call's own thread and statically
/// admitted descendants, enforces a non-empty visit, and caps distinct
/// journals per call at `MAX_PIPELINE_TOOL_JOURNALS_PER_CALL` (64).
#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn pipeline_tool_journals_enforce_scope_and_per_call_cap() {
    use crate::agents::graph::node_recovery_runtime::PipelineToolJournalAuthority as _;
    use std::collections::BTreeSet;

    let database = database().await;
    let family = Arc::new(
        root(database.pool.clone(), 1)
            .await
            .with_application_children(["delegate"].into_iter())
            .await
            .expect("activate root family"),
    );
    let lent = family
        .for_pipeline_tool_call(
            "thread-1/call-1".to_owned(),
            BTreeSet::from(["sub".to_owned()]),
        )
        .expect("call-scoped journal factory");
    let policy = NodeRecoveryPolicy::default();

    for bad in [
        tool_activation("thread-1", 0, None),
        tool_activation("thread-1", 0, Some([0x0b; 32])),
        tool_activation("thread-1/delegate", 0, Some([0x0b; 32])),
        tool_activation("thread-1/call-2", 0, Some([0x0b; 32])),
        tool_activation("thread-1/call-1/unadmitted", 0, Some([0x0b; 32])),
        tool_activation("thread-1/call-1", 0, None),
    ] {
        assert!(lent.open(&bad, &policy).await.is_err(), "{bad:?}");
    }
    assert_eq!(writers(&database.pool, "n1:%").await, 0);

    lent.open(
        &tool_activation("thread-1/call-1/sub", 0, Some([0x0c; 32])),
        &policy,
    )
    .await
    .expect("descendant opens");
    for step in 0..63_u64 {
        lent.open(
            &tool_activation("thread-1/call-1", step, Some([0x0b; 32])),
            &policy,
        )
        .await
        .expect("within per-call cap");
    }
    lent.open(
        &tool_activation("thread-1/call-1", 0, Some([0x0b; 32])),
        &policy,
    )
    .await
    .expect("re-opening same journal stays within cap");
    let cap_err = lent
        .open(
            &tool_activation("thread-1/call-1", 63, Some([0x0b; 32])),
            &policy,
        )
        .await
        .err()
        .expect("65th journal refused");
    assert!(cap_err.to_string().contains("journal_limit"), "{cap_err}");
    assert_eq!(writers(&database.pool, "n1:%").await, 64);
}

/// FU-358 (pipeline root): takeover of the run root fences both new journal
/// opens and already-opened journal handles under the superseded claim.
#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn pipeline_tool_journals_fence_new_and_opened_handles_after_root_takeover() {
    use crate::agents::graph::node_recovery_runtime::{
        PipelineToolJournalAuthority as _, RecoverableNode,
    };
    use adk_rust::graph::{ExecutionConfig, Node as _, NodeContext};
    use std::collections::{BTreeSet, HashMap};
    use std::sync::atomic::{AtomicUsize, Ordering};

    let database = database().await;
    let family = Arc::new(
        root(database.pool.clone(), 1)
            .await
            .with_application_children(["delegate"].into_iter())
            .await
            .expect("activate root family"),
    );
    let policy = NodeRecoveryPolicy::default();
    let second_call = family
        .for_pipeline_tool_call("thread-1/call-2".to_owned(), BTreeSet::new())
        .expect("second call factory");
    let opened_before_takeover = second_call
        .open(
            &tool_activation("thread-1/call-2", 0, Some([0x0d; 32])),
            &policy,
        )
        .await
        .expect("claim 1 opens before takeover");
    let _claim_2 = root(database.pool.clone(), 2).await;

    let open_err = second_call
        .open(
            &tool_activation("thread-1/call-2", 1, Some([0x0d; 32])),
            &policy,
        )
        .await
        .err()
        .expect("superseded claim cannot open");
    assert!(
        open_err.to_string().contains("writer_not_current"),
        "{open_err}"
    );
    let stale_handle_err = opened_before_takeover
        .verify_applied_revision(0)
        .await
        .expect_err("already-opened journal handle must also be fenced after root takeover");
    assert!(
        stale_handle_err.to_string().contains("writer_not_current"),
        "{stale_handle_err}"
    );

    let invocations = Arc::new(AtomicUsize::new(0));
    let recoverable = RecoverableNode::new(
        Arc::new(CountAttemptBody {
            invocations: Arc::clone(&invocations),
        }),
        [0x07; 32],
        policy,
        Arc::clone(&second_call),
        None,
    );
    let ctx = NodeContext::new(
        HashMap::from([(
            crate::agents::graph::APPLICATION_VISIT_STATE_KEY.to_owned(),
            serde_json::json!("0d".repeat(32)),
        )]),
        ExecutionConfig::new("thread-1/call-2"),
        0,
    );
    let exec_err = recoverable
        .execute(&ctx)
        .await
        .err()
        .expect("superseded claim cannot execute");
    assert!(
        exec_err.to_string().contains("writer_not_current"),
        "{exec_err}"
    );
    assert_eq!(invocations.load(Ordering::SeqCst), 0);
}

/// FU-358 (ordinary chat root): `PostgresSessionService::pipeline_tool_journals`
/// fences `n1:` journals against `agent_session_writers` with zero upfront
/// checkpoint rows, and takeover of `agent_session_writers` alone fences claim 1
/// on both new opens and already-opened journals.
#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn ordinary_session_pipeline_tool_journals_enforce_session_root_fence_and_takeover() {
    use crate::state::postgres_session_tests::install_schema as install_session_schema;
    use crate::state::{PostgresSessionService, SessionLimits, SessionWriterAuthority};
    use std::collections::BTreeSet;

    let database = database().await;
    install_session_schema(&database.pool).await;

    let activate_session = |attempt: u64| {
        let pool = database.pool.clone();
        async move {
            let auth = SessionWriterAuthority::new(
                "tenant-1".to_owned(),
                11,
                11,
                APPLICATION_CAPABILITY_ID,
                [0x41; 32],
                "thread-1".to_owned(),
                "elitea-agent".to_owned(),
                "user-1".to_owned(),
                "session-1".to_owned(),
                "execution-1".to_owned(),
                1,
                format!("claim-{attempt}"),
                attempt,
                attempt,
                1_700_000_000_000_000 + i64::try_from(attempt).unwrap(),
                format!("workload-{attempt}"),
                format!("producer-{attempt}"),
                [0x61; 32],
            )
            .expect("session writer authority");
            Arc::new(
                PostgresSessionService::activate(
                    pool,
                    auth,
                    SessionLimits::default(),
                    Arc::new(TestStateWriterLease::current()),
                )
                .await
                .expect("activate session"),
            )
        }
    };

    let session_1 = activate_session(1).await;
    // Zero checkpoint writer rows exist before any pipeline tool runs.
    assert_eq!(writers(&database.pool, "%").await, 0);

    let authority_1 = session_1.pipeline_tool_journals(CheckpointLimits::default());
    assert!(
        authority_1
            .for_pipeline_tool_call("thread-1".to_owned(), BTreeSet::new())
            .is_err(),
        "must refuse session root thread"
    );
    let factory_1 = authority_1
        .for_pipeline_tool_call("thread-1/call-1".to_owned(), BTreeSet::new())
        .expect("call factory");
    let policy = NodeRecoveryPolicy::default();
    let activation = NodeAttemptActivation {
        root_thread_id: "thread-1/call-1".to_owned(),
        node_id: "rev".to_owned(),
        step: 0,
        node_digest: [0x11; 32],
        input_digest: [0x22; 32],
        visit: Some([0x33; 32]),
    };
    let mut unscoped = activation.clone();
    unscoped.visit = None;
    assert!(factory_1.open(&unscoped, &policy).await.is_err());

    let opened_1 = factory_1
        .open(&activation, &policy)
        .await
        .expect("claim 1 opens journal under session writer");
    assert_eq!(writers(&database.pool, "n1:%").await, 1);

    // Claim 2 takes over ONLY `agent_session_writers`.
    let session_2 = activate_session(2).await;

    // Claim 1 is now fenced on both already-opened handles and new opens against `agent_session_writers`.
    let stale_err = opened_1
        .verify_applied_revision(0)
        .await
        .expect_err("already-opened session journal fenced after session takeover");
    assert!(
        stale_err.to_string().contains("writer_not_current"),
        "{stale_err}"
    );
    let mut next_step = activation.clone();
    next_step.step = 1;
    let err = factory_1
        .open(&next_step, &policy)
        .await
        .err()
        .expect("claim 1 fenced after session takeover");
    assert!(err.to_string().contains("writer_not_current"), "{err}");

    // Claim 2 opens the exact same journal cleanly under its own session claim.
    let factory_2 = session_2
        .pipeline_tool_journals(CheckpointLimits::default())
        .for_pipeline_tool_call("thread-1/call-1".to_owned(), BTreeSet::new())
        .expect("claim 2 call factory");
    factory_2
        .open(&activation, &policy)
        .await
        .expect("claim 2 takes over the same journal");
    assert_eq!(writers(&database.pool, "n1:%").await, 1);
}
