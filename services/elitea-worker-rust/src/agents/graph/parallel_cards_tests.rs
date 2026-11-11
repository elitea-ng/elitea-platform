//! Per-interrupt cards from the child checkpoint, one test per card kind
//! (2a-2 task 7): the right card resumes only its own child, a card of an
//! older pause is refused, and an authored pause continues with `continue`
//! and the typed text as its value.

use std::sync::Mutex as StdMutex;
use std::sync::atomic::{AtomicBool, Ordering};

use adk_rust::graph::interrupt::Interrupt;
use adk_rust::graph::{Checkpoint, ExecutionConfig, MemoryCheckpointer, Node, NodeContext};
use serde_json::json;

use super::*;
use crate::agents::graph::application::{
    ApplicationExecutionError, PipelineApplicationSelection, ResolvedApplicationParticipant,
};
use crate::agents::graph::direct_tool::PipelineDirectToolResolver;
use crate::agents::graph::direct_tool_tests::{
    AuthorizationFixtureTool, FixtureResolver, InvocationCapture,
    sensitive_pipeline_definition_yaml, sensitive_runtime,
};
use crate::agents::graph::fanout_test_ledger::TestLedger;
use crate::agents::graph::parallel::{
    ParallelActivation, ParallelCheckpointAppender, ParallelChildCheckpoint,
    ParallelChildCheckpointerFactory, ParallelChildOrigin,
};
use crate::agents::graph::parallel_cards::{FanoutCardKind, child_card};

const ROOT: &str = "root-cards";
const RECEIPT: &str = "elitea.graph.parallel.branch-receipt.v1";

const PARENT: &str = r"
state:
  messages: list
  joined: list
entry_point: gather
nodes:
  - id: gather
    type: parallel
    branches: [{id: left, node: workerA}, {id: right, node: workerB}]
    max_concurrency: 2
    wait: all
    output: [joined]
    transition: END
  - id: workerA
    type: agent
    tool: Research Agent
    input_mapping:
      task: {type: fixed, value: deployment}
    output: [messages]
  - id: workerB
    type: agent
    tool: Research Agent
    input_mapping:
      task: {type: fixed, value: rollback}
    output: [messages]
";

const HITL_CHILD: &str = r#"
state:
  input: str
  messages: list
entry_point: approve
nodes:
  - id: approve
    type: hitl
    input: [input]
    user_message:
      type: fstring
      value: "Approve {input}."
    routes:
      approve: END
      reject: END
"#;

const STATIC_CHILD: &str = r#"
interrupt_before: [tidy]
state:
  input: str
  messages: list
  answer: str
entry_point: tidy
nodes:
  - id: tidy
    type: state_modifier
    template: "{{ input }}"
    input: [input]
    output: [answer]
    transition: END
"#;

/// One store holds the parent and every child thread, as one claim does.
#[derive(Default)]
struct Store(MemoryCheckpointer);

#[async_trait]
impl Checkpointer for Store {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        self.0.save(checkpoint).await
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.0.load(thread).await
    }
    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.0.load_by_id(id).await
    }
    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.0.list(thread).await
    }
    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.0.delete(thread).await
    }
}

#[async_trait]
impl ParallelCheckpointAppender for Store {
    async fn append_after(
        &self,
        expected: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        if self.0.load_by_id(&candidate.checkpoint_id).await?.is_some() {
            return Ok(candidate.checkpoint_id.clone());
        }
        let latest = self.0.load(&candidate.thread_id).await?;
        if latest.map(|row| row.checkpoint_id) != expected.map(|row| row.checkpoint_id.clone()) {
            return Err(GraphError::CheckpointError(
                "checkpoint.conflict: the parent moved".to_owned(),
            ));
        }
        self.0.save(candidate).await
    }
}

/// Every child checkpointer is the shared store (one claim's family).
struct Shared(Arc<Store>);

#[async_trait]
impl ParallelChildCheckpointerFactory for Shared {
    fn child_origin(&self, _: &ParallelActivation) -> Result<ParallelChildOrigin, GraphError> {
        Ok(ParallelChildOrigin {
            execution_id: "execution-1".to_owned(),
            generation: 1,
        })
    }

    fn branch_thread_id(
        &self,
        activation: &ParallelActivation,
        branch: &crate::agents::graph::ParallelBranchDefinition,
        ordinal: usize,
        _: &[u8; 32],
        _: &ParallelChildOrigin,
    ) -> Result<String, GraphError> {
        Ok(format!(
            "child:{}:{}:{ordinal}:{}",
            activation.node_id,
            branch.id(),
            activation.step
        ))
    }

    async fn for_branch(
        &self,
        activation: &ParallelActivation,
        branch: &crate::agents::graph::ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<ParallelChildCheckpoint, GraphError> {
        let thread_id = self.branch_thread_id(activation, branch, ordinal, input_digest, origin)?;
        Ok(ParallelChildCheckpoint {
            admitted_threads: BTreeSet::from([thread_id.clone()]),
            thread_id,
            checkpointer: self.0.clone(),
            node_recovery: None,
            tool_pipelines: None,
        })
    }
}

/// A saved pipeline participant compiled for each child's own checkpointer.
struct SavedPipeline {
    yaml: String,
    runtimes: PipelineNodeRuntimes,
}

impl PipelineApplicationResolver for SavedPipeline {
    fn fixed_parallel_definition_digest(
        &self,
        _node: &str,
    ) -> Result<[u8; 32], ApplicationExecutionError> {
        Ok([9; 32])
    }

    fn resolve(
        &self,
        _selection: &PipelineApplicationSelection,
        checkpointer: Arc<dyn Checkpointer>,
    ) -> Result<ResolvedApplicationParticipant, ApplicationExecutionError> {
        let definition = PipelineDefinition::from_yaml(&self.yaml)
            .map_err(|_| ApplicationExecutionError::Unavailable)?;
        let graph = definition
            .compile_subgraph_with_runtime(checkpointer, &self.runtimes)
            .map_err(|_| ApplicationExecutionError::Unavailable)?;
        Ok(ResolvedApplicationParticipant::Pipeline {
            graph: Arc::new(graph),
            variable_types: definition.declared_variable_types(),
            static_pauses: definition.static_pause_catalog(),
            events: None,
            display_name: "Research Agent".to_owned(),
        })
    }
}

struct Fixture {
    store: Arc<Store>,
    branches: Arc<CompilerParallelBranches>,
    parallel: ParallelNodeDefinition,
    ledger: Arc<TestLedger>,
}

/// The delegated sign-in fixture's MCP server
/// (`delegated_authorization_error_fixture`).
const DELEGATED_SERVER: &str = "https://mcp.example.invalid/v1/mcp";

impl Fixture {
    fn new(child: &str, runtimes: PipelineNodeRuntimes) -> Self {
        Self::with_servers(child, runtimes, &[DELEGATED_SERVER])
    }

    /// FU-250: the servers the run holds a Main-resolved token for.
    fn with_servers(child: &str, runtimes: PipelineNodeRuntimes, servers: &[&str]) -> Self {
        let pipeline = PipelineDefinition::from_yaml(PARENT).expect("parent pipeline");
        let parallel = pipeline
            .nodes
            .iter()
            .find_map(|node| match node {
                PipelineNodeDefinition::Parallel(node) => Some(node.clone()),
                _ => None,
            })
            .expect("parallel node");
        let owned = parallel
            .branches()
            .iter()
            .map(|branch| {
                let node = pipeline
                    .nodes
                    .iter()
                    .find_map(|node| match node {
                        PipelineNodeDefinition::Application(node) if node.id() == branch.node() => {
                            Some(node.clone())
                        }
                        _ => None,
                    })
                    .expect("owned agent");
                (branch.id().to_owned(), OwnedBranch::Agent(node))
            })
            .collect();
        Self {
            store: Arc::new(Store::default()),
            branches: Arc::new(CompilerParallelBranches {
                delegated_servers: Arc::new(servers.iter().map(|&url| url.to_owned()).collect()),
                owned,
                state_types: pipeline.state,
                resolver: Some(Arc::new(SavedPipeline {
                    yaml: child.to_owned(),
                    runtimes,
                })),
                llm: None,
                code: None,
                code_recovery: false,
                saved_code: std::collections::BTreeMap::new(),
                events: None,
                root_session: None,
            }),
            parallel,
            ledger: TestLedger::new(),
        }
    }

    fn node(&self, claim: u64) -> DurableParallelNode {
        let parent: Arc<dyn ParallelCheckpointAppender> = self.store.clone();
        let runtime = Arc::new(AdkParallelBranchRuntime::new(
            Arc::new(Shared(Arc::clone(&self.store))),
            self.branches.clone(),
            Arc::new(ParallelOccurrenceCheckpointer::new(parent)),
        ));
        DurableParallelNode::new(self.parallel.clone(), runtime)
            .with_decisions(self.ledger.client(claim))
    }

    fn thread(branch: &str) -> String {
        format!("child:gather:{branch}:{}:0", usize::from(branch == "right"))
    }

    /// The child's own pause, rebuilt from its pause receipt.
    async fn pause_of(&self, branch: &str) -> ParallelBranchPause {
        let thread = Self::thread(branch);
        let saved = self
            .store
            .load(&thread)
            .await
            .unwrap()
            .expect("a paused child checkpoint");
        let receipt = &saved.metadata[RECEIPT];
        assert_eq!(receipt["status"], "paused");
        ParallelBranchPause {
            thread_id: thread,
            checkpoint_id: saved.checkpoint_id,
            interrupt: serde_json::from_value::<Interrupt>(receipt["interrupt"].clone()).unwrap(),
            checkpointer: self.store.clone(),
        }
    }

    async fn child_settled(&self, branch: &str) -> bool {
        self.store
            .load(&Self::thread(branch))
            .await
            .unwrap()
            .is_some_and(|saved| saved.pending_nodes.is_empty())
    }
}

fn context() -> NodeContext {
    NodeContext::new(
        State::from([("messages".to_owned(), json!([]))]),
        ExecutionConfig::new(ROOT),
        0,
    )
}

/// `(interrupt_key, interrupt_id)` of every parked card.
fn parked(output: &adk_rust::graph::NodeOutput) -> Vec<(String, String)> {
    let Some(Interrupt::Dynamic {
        data: Some(data), ..
    }) = output.interrupt.as_ref()
    else {
        panic!(
            "expected the parked fan-out interrupt: {:?}",
            output.updates
        );
    };
    data["pending"]
        .as_array()
        .expect("parked cards")
        .iter()
        .map(|card| {
            (
                card["interrupt_key"].as_str().unwrap().to_owned(),
                card["interrupt_id"].as_str().unwrap().to_owned(),
            )
        })
        .collect()
}

/// Both children pause with one card each, of `kind`, keyed by the frozen
/// child: returns `(left card, right card)`.
async fn both_paused(
    fixture: &Fixture,
    kind: FanoutCardKind,
) -> ((String, String), (String, String)) {
    let cards = parked(
        &fixture
            .node(1)
            .execute(&context())
            .await
            .expect("both children pause"),
    );
    assert_eq!(cards.len(), 2, "one card per child");
    for branch in ["left", "right"] {
        let pause = fixture.pause_of(branch).await;
        let proven = child_card(&pause).expect("the child pause is proven");
        assert_eq!(proven.kind, kind);
        assert!(cards.iter().any(|(_, id)| *id == proven.card.interrupt_id));
        assert_card_frame(&pause, kind);
    }
    let left = fixture.pause_of("left").await;
    let left_id = child_card(&left).unwrap().card.interrupt_id;
    let right = cards
        .iter()
        .find(|(_, id)| *id != left_id)
        .cloned()
        .unwrap();
    let left = cards
        .iter()
        .find(|(_, id)| *id == left_id)
        .cloned()
        .unwrap();
    assert_ne!(left.0, right.0, "children share no card");
    (left, right)
}

/// The pause also renders as its card frame (contract §4): kind, guardrail
/// and actions agree with the proven kind, the digest is a function of the
/// pause, and the full card frame builds and encodes.
fn assert_card_frame(pause: &ParallelBranchPause, kind: FanoutCardKind) {
    use crate::agents::graph::fanout_frames::{
        AgentPathTier, CardInput, CardKind, FanoutMemberV1, InterruptCardV1, MemberHierarchy,
        interrupt_pending_event, member_call_id,
    };
    let parts = crate::agents::graph::parallel_cards::card_parts(pause).expect("card parts");
    assert_eq!(
        parts,
        crate::agents::graph::parallel_cards::card_parts(pause).unwrap()
    );
    let (expected, guardrail) = match kind {
        FanoutCardKind::ToolGuard => (CardKind::ToolGuard, "sensitive_tool"),
        FanoutCardKind::HitlNode => (CardKind::HitlNode, "pipeline_hitl"),
        FanoutCardKind::StaticPause => (CardKind::HitlNode, "pipeline_static"),
        FanoutCardKind::AskUser => (CardKind::AskUser, "clarifying_question"),
        FanoutCardKind::DelegatedAuth => (CardKind::DelegatedAuth, "mcp_auth"),
    };
    assert_eq!(
        (parts.kind, parts.display.guardrail_type),
        (expected, guardrail)
    );
    assert!(
        parts
            .available_actions
            .iter()
            .all(|action| kind.accepts(action))
    );
    let member = FanoutMemberV1 {
        activation: "0123456789abcdef".to_owned(),
        kind: "parallel",
        member: "left".to_owned(),
        node: "fan".to_owned(),
        ordinal: 1,
        total: 2,
    };
    let hierarchy = MemberHierarchy {
        path: vec![AgentPathTier {
            call_id: member_call_id(&pause.thread_id),
            name: "worker".to_owned(),
            sibling_ordinal: Some(1),
        }],
    };
    let card = InterruptCardV1::new(CardInput {
        interrupt_key: &"a".repeat(64),
        interrupt_id: &child_card(pause).unwrap().card.interrupt_id,
        kind: parts.kind,
        available_actions: &parts.available_actions,
        payload_sha256: &parts.payload_sha256,
        display: parts.display.clone(),
        hierarchy: &hierarchy,
        member: Some(&member),
    })
    .expect("the card frame builds");
    interrupt_pending_event(&card).expect("the frame encodes");
}

/// The right card resumes only its own child: the sibling keeps its card.
async fn answer_right_first(
    fixture: &Fixture,
    right: &(String, String),
    action: &str,
    value: Option<&str>,
) {
    if action == "authorize" {
        fixture
            .ledger
            .decide_with_credential(&right.0, TEST_CREDENTIAL_REF);
    } else {
        fixture.ledger.decide(&right.0, action, value);
    }
    let remaining = parked(
        &fixture
            .node(2)
            .execute(&context())
            .await
            .expect("one child resumed"),
    );
    assert_eq!(remaining.len(), 1);
    assert_ne!(remaining[0].0, right.0);
    assert!(
        fixture.child_settled("right").await,
        "the answered child did not resume"
    );
    assert!(!fixture.child_settled("left").await, "the sibling resumed");
    assert_eq!(fixture.ledger.open_decisions(), 0);
}

/// A decision built for an older pause of a child that has since moved on is
/// refused by the child checkpoint proof, and runs nothing.
async fn assert_older_pause_refused(
    fixture: &Fixture,
    older: &ParallelBranchPause,
    action: &str,
    value: &str,
) {
    let card = child_card(older)
        .expect("the older pause still parses")
        .card;
    let branch = &fixture.parallel.branches()[1];
    let error = fixture
        .branches
        .resume_input(
            branch,
            older,
            &[ParallelDecision {
                credential: false,
                interrupt_id: card.interrupt_id,
                tool_call_id: card.tool_call_id,
                action: action.to_owned(),
                value: value.to_owned(),
            }],
        )
        .await
        .expect_err("an older pause must not resume the child");
    assert!(
        error
            .to_string()
            .contains("graph.parallel.decision_refused"),
        "{error}"
    );
}

/// A card refuses an action its kind does not offer.
async fn assert_wrong_action_refused(fixture: &Fixture, branch: &str, action: &str) {
    let pause = fixture.pause_of(branch).await;
    let card = child_card(&pause).unwrap().card;
    let error = fixture
        .branches
        .resume_input(
            &fixture.parallel.branches()[0],
            &pause,
            &[ParallelDecision {
                credential: false,
                interrupt_id: card.interrupt_id,
                tool_call_id: card.tool_call_id,
                action: action.to_owned(),
                value: "text".to_owned(),
            }],
        )
        .await
        .expect_err("the action is not offered by the card");
    assert!(
        error
            .to_string()
            .contains("graph.parallel.invalid_decision"),
        "{error}"
    );
}

async fn finish(fixture: &Fixture, left: &(String, String), action: &str, value: Option<&str>) {
    fixture.ledger.decide(&left.0, action, value);
    let output = fixture
        .node(3)
        .execute(&context())
        .await
        .expect("both children done");
    assert!(output.interrupt.is_none());
    let joined = output.updates["joined"].as_array().expect("joined entries");
    assert_eq!(joined.len(), 2);
    assert!(
        joined.iter().all(|entry| entry["status"] == "succeeded"),
        "{joined:?}"
    );
    assert_eq!(fixture.ledger.open_decisions(), 0);
}

#[tokio::test]
async fn a_hitl_node_card_resumes_only_its_child_and_an_older_pause_is_refused() {
    let fixture = Fixture::new(HITL_CHILD, PipelineNodeRuntimes::default());
    let (left, right) = both_paused(&fixture, FanoutCardKind::HitlNode).await;
    assert_wrong_action_refused(&fixture, "left", "continue").await;
    let older = fixture.pause_of("right").await;
    answer_right_first(&fixture, &right, "approve", None).await;
    assert_older_pause_refused(&fixture, &older, "approve", "").await;
    finish(&fixture, &left, "reject", None).await;
}

#[tokio::test]
async fn an_authored_pause_continues_with_the_typed_text_as_its_value() {
    let fixture = Fixture::new(STATIC_CHILD, PipelineNodeRuntimes::default());
    let (left, right) = both_paused(&fixture, FanoutCardKind::StaticPause).await;
    assert_wrong_action_refused(&fixture, "left", "approve").await;
    let older = fixture.pause_of("right").await;
    answer_right_first(&fixture, &right, "continue", Some("ship the patch")).await;
    // The typed text reached exactly the paused leaf as its input.
    let leaf = fixture
        .store
        .load(&format!("{}/workerB", Fixture::thread("right")))
        .await
        .unwrap()
        .expect("the right child's pipeline checkpoint");
    assert_eq!(leaf.state["answer"], json!("ship the patch"));
    assert_older_pause_refused(&fixture, &older, "continue", "again").await;
    finish(&fixture, &left, "continue", Some("roll back")).await;
}

fn direct_tool_runtimes(resolver: Arc<dyn PipelineDirectToolResolver>) -> PipelineNodeRuntimes {
    PipelineNodeRuntimes::new(None, Some(resolver), None)
}

#[tokio::test]
async fn a_tool_guard_card_resumes_only_its_child_and_an_older_pause_is_refused() {
    let (resolver, capture): (_, Arc<StdMutex<InvocationCapture>>) = sensitive_runtime(
        json!({"report": {"risk": "low"}, "messages": []}),
        "customer_support",
        true,
    );
    let fixture = Fixture::new(
        &sensitive_pipeline_definition_yaml(),
        direct_tool_runtimes(resolver),
    );
    let (left, right) = both_paused(&fixture, FanoutCardKind::ToolGuard).await;
    assert_eq!(
        capture.lock().unwrap().calls,
        0,
        "a guarded tool ran before approval"
    );
    assert_wrong_action_refused(&fixture, "left", "answer").await;
    let older = fixture.pause_of("right").await;
    answer_right_first(&fixture, &right, "approve", None).await;
    assert_eq!(
        capture.lock().unwrap().calls,
        1,
        "only the approved child's tool ran"
    );
    assert_older_pause_refused(&fixture, &older, "approve", "").await;
    finish(&fixture, &left, "block_with_comment", Some("not this one")).await;
    assert_eq!(capture.lock().unwrap().calls, 1, "a blocked tool ran");
}

#[tokio::test]
async fn a_delegated_auth_card_resumes_only_its_child_and_an_older_pause_is_refused() {
    if std::env::var_os(TRACE_CHILD_ENV).is_some() {
        // Run by the log-capture test in its own process: every event and
        // span of the authorize wake, at TRACE, to stdout.
        tracing::subscriber::set_global_default(
            tracing_subscriber::fmt()
                .with_max_level(tracing::Level::TRACE)
                .with_span_events(tracing_subscriber::fmt::format::FmtSpan::FULL)
                .with_ansi(false)
                .finish(),
        )
        .expect("the child process's subscriber");
        tracing::trace!(target: "elitea_worker::fu250", "trace capture installed");
    }
    let authorized = Arc::new(AtomicBool::new(false));
    let resolver: Arc<dyn PipelineDirectToolResolver> = Arc::new(FixtureResolver {
        alias: "Customer Support".to_owned(),
        tool: Arc::new(AuthorizationFixtureTool {
            authorized: Arc::clone(&authorized),
            toolkit_type: "mcp",
        }),
        sensitive: None,
    });
    let child = sensitive_pipeline_definition_yaml().replace("type: toolkit", "type: mcp");
    let fixture = Fixture::new(&child, direct_tool_runtimes(resolver));
    let (left, right) = both_paused(&fixture, FanoutCardKind::DelegatedAuth).await;
    assert_wrong_action_refused(&fixture, "left", "approve").await;
    let older = fixture.pause_of("right").await;
    // The credential is materialized for the child's next call (by reference).
    authorized.store(true, Ordering::Release);
    answer_right_first(&fixture, &right, "authorize", None).await;
    let leaf = fixture
        .store
        .load(&format!("{}/workerB", Fixture::thread("right")))
        .await
        .unwrap()
        .expect("the right child's pipeline checkpoint");
    assert_eq!(leaf.state["report"], json!({"authorized": true}));
    assert_older_pause_refused(&fixture, &older, "authorize", "").await;
    finish(&fixture, &left, "skip", None).await;
    // FU-250 (brief item 2): the credential reference reaches no checkpoint
    // of either child or their pipelines, and no fetched decision prints it.
    for branch in ["left", "right"] {
        for thread in [
            Fixture::thread(branch),
            format!("{}/workerB", Fixture::thread(branch)),
        ] {
            for saved in fixture.store.list(&thread).await.expect("list") {
                let text = serde_json::to_string(&saved.state).expect("state")
                    + &serde_json::to_string(&saved.metadata).expect("metadata");
                assert!(
                    !text.contains(TEST_CREDENTIAL_REF),
                    "{thread} persisted the reference"
                );
            }
        }
    }
    let decision = elitea_agent_runtime::graph::fanout::runner::FetchedDecision {
        interrupt_key: "k".to_owned(),
        request_id: "r".to_owned(),
        revision: 2,
        decision_sha256: "d".to_owned(),
        action: "authorize".to_owned(),
        value: None,
        credential_ref: Some(TEST_CREDENTIAL_REF.to_owned()),
    };
    assert!(!format!("{decision:?}").contains(TEST_CREDENTIAL_REF));
}

/// A credential reference only a test uses.
const TEST_CREDENTIAL_REF: &str = "test-only-reference-0123456789abcdef";

/// Set when the log-capture test runs the authorize wake in a child process.
const TRACE_CHILD_ENV: &str = "ELITEA_TEST_FU250_TRACE_CHILD";

/// FU-250 (E2): the authorize wake, run in its own process under a global
/// TRACE subscriber with span events, logs no credential reference. The
/// child process keeps other tests' events out of the capture.
#[test]
fn an_authorize_wake_logs_no_credential_reference_at_trace() {
    let module = module_path!().split_once("::").expect("crate path").1;
    let test = format!(
        "{module}::a_delegated_auth_card_resumes_only_its_child_and_an_older_pause_is_refused"
    );
    let output = std::process::Command::new(std::env::current_exe().expect("test binary"))
        .args(["--exact", test.as_str(), "--nocapture", "--test-threads=1"])
        .env(TRACE_CHILD_ENV, "1")
        .output()
        .expect("the child test runs");
    let stdout = String::from_utf8_lossy(&output.stdout);
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(output.status.success(), "{stdout} {stderr}");
    assert!(
        stdout.contains("1 passed") && stdout.contains("trace capture installed"),
        "the child ran the wake under the capture: {stdout}"
    );
    assert!(
        !stdout.contains(TEST_CREDENTIAL_REF) && !stderr.contains(TEST_CREDENTIAL_REF),
        "a log line carries the credential reference"
    );
}

/// FU-250: an `authorize` resumes only with its credential. Without a named
/// reference, or without the run holding the card's server's token, the
/// branch ends typed `graph.fanout.credential_unavailable` and nothing runs.
#[tokio::test(flavor = "current_thread")]
async fn an_authorize_without_its_credential_ends_the_branch_typed() {
    for (servers, with_reference) in [(&[DELEGATED_SERVER][..], false), (&[][..], true)] {
        let authorized = Arc::new(AtomicBool::new(true));
        let resolver: Arc<dyn PipelineDirectToolResolver> = Arc::new(FixtureResolver {
            alias: "Customer Support".to_owned(),
            tool: Arc::new(AuthorizationFixtureTool {
                authorized: Arc::clone(&authorized),
                toolkit_type: "mcp",
            }),
            sensitive: None,
        });
        let child = sensitive_pipeline_definition_yaml().replace("type: toolkit", "type: mcp");
        let fixture = Fixture::with_servers(&child, direct_tool_runtimes(resolver), servers);
        authorized.store(false, Ordering::Release);
        let (left, right) = both_paused(&fixture, FanoutCardKind::DelegatedAuth).await;
        authorized.store(true, Ordering::Release);
        if with_reference {
            fixture
                .ledger
                .decide_with_credential(&right.0, TEST_CREDENTIAL_REF);
        } else {
            fixture.ledger.decide(&right.0, "authorize", None);
        }
        fixture.ledger.decide(&left.0, "skip", None);
        let output = fixture
            .node(2)
            .execute(&context())
            .await
            .expect("both children settle");
        let joined = output.updates["joined"].as_array().expect("joined entries");
        let refused = joined
            .iter()
            .find(|entry| entry["status"] == "failed")
            .unwrap_or_else(|| {
                panic!("servers {servers:?}, reference {with_reference}: {joined:?}")
            });
        assert_eq!(
            refused["error"]["code"], "graph.fanout.credential_unavailable",
            "{joined:?}"
        );
        let leaf = fixture
            .store
            .load(&format!("{}/workerB", Fixture::thread("right")))
            .await
            .unwrap()
            .expect("the right child's pipeline checkpoint");
        assert_ne!(
            leaf.state.get("report"),
            Some(&json!({"authorized": true})),
            "the call never ran authorized"
        );
    }
}

#[test]
fn every_card_kind_offers_exactly_its_contract_actions() {
    let all = [
        "approve",
        "reject",
        "edit",
        "block_with_comment",
        "authorize",
        "skip",
        "answer",
        "continue",
    ];
    for (kind, offered) in [
        (
            FanoutCardKind::ToolGuard,
            &["approve", "reject", "block_with_comment"][..],
        ),
        (
            FanoutCardKind::HitlNode,
            &["approve", "reject", "edit", "block_with_comment"][..],
        ),
        (FanoutCardKind::StaticPause, &["continue"][..]),
        (FanoutCardKind::AskUser, &["answer"][..]),
        (FanoutCardKind::DelegatedAuth, &["authorize", "skip"][..]),
    ] {
        for action in all {
            assert_eq!(
                kind.accepts(action),
                offered.contains(&action),
                "{kind:?} {action}"
            );
        }
    }
}

/// A saved Agent child raises its cards through its nested Application
/// pause, which needs the Application decision registry: until that is
/// composed its pause is refused typed instead of being answered unproven.
#[test]
fn a_nested_application_pause_is_refused_typed() {
    let pause = ParallelBranchPause {
        thread_id: "child:gather:left:0:0".to_owned(),
        checkpoint_id: "checkpoint-1".to_owned(),
        interrupt: Interrupt::Dynamic {
            message: "Waiting for approval.".to_owned(),
            data: Some(json!({
                "schema_revision": crate::agents::graph::PIPELINE_APPLICATION_HITL_SCHEMA,
                "type": "hitl_checkpoint",
                "guardrail_type": "application_sensitive_tool",
                "interrupt_ids": ["card"],
            })),
        },
        checkpointer: Arc::new(MemoryCheckpointer::new()),
    };
    let error = child_card(&pause).expect_err("an Application pause is not proven here");
    assert!(error.to_string().contains("graph.parallel.pause_unproved"));
}

/// The card frame a `ToolGuard` `pause` raises, as the Worker serialises it.
fn tool_guard_card(pause: &ParallelBranchPause) -> serde_json::Value {
    use crate::agents::graph::fanout_frames::{
        AgentPathTier, CardInput, FanoutMemberV1, InterruptCardV1, MemberHierarchy, member_call_id,
    };
    let parts = crate::agents::graph::parallel_cards::card_parts(pause).expect("card parts");
    let member = FanoutMemberV1 {
        activation: "0123456789abcdef".to_owned(),
        kind: "parallel",
        member: "left".to_owned(),
        node: "fan".to_owned(),
        ordinal: 1,
        total: 2,
    };
    let hierarchy = MemberHierarchy {
        path: vec![AgentPathTier {
            call_id: member_call_id(&pause.thread_id),
            name: "worker".to_owned(),
            sibling_ordinal: Some(1),
        }],
    };
    let card = InterruptCardV1::new(CardInput {
        interrupt_key: &"a".repeat(64),
        interrupt_id: &child_card(pause).unwrap().card.interrupt_id,
        kind: parts.kind,
        available_actions: &parts.available_actions,
        payload_sha256: &parts.payload_sha256,
        display: parts.display.clone(),
        hierarchy: &hierarchy,
        member: Some(&member),
    })
    .expect("the card frame builds");
    serde_json::to_value(&card).expect("the card encodes")
}

/// The card fixture Main's upgrade replay test raises (FU-268).
fn upgrade_card_fixture(name: &str) -> serde_json::Value {
    let path = format!(
        "{}/../elitea-main/internal/infra/db/repos/testdata/fu268/tool-guard-card-{name}.json",
        env!("CARGO_MANIFEST_DIR")
    );
    serde_json::from_slice(&std::fs::read(&path).expect("read the card fixture"))
        .expect("card json")
}

/// The pipeline sensitive-tool pause a Worker from before the approval-rule
/// release (#1540) stored (FU-268), as its fan-out receipt holds it.
fn pre_1540_tool_guard_pause() -> ParallelBranchPause {
    let path = format!(
        "{}/tests/fixtures/fu268/tool-guard-pause-pre-1540.json",
        env!("CARGO_MANIFEST_DIR")
    );
    let stored: serde_json::Value =
        serde_json::from_slice(&std::fs::read(&path).expect("read the pause fixture"))
            .expect("pause json");
    ParallelBranchPause {
        thread_id: stored["thread_id"].as_str().expect("thread").to_owned(),
        checkpoint_id: stored["checkpoint_id"]
            .as_str()
            .expect("checkpoint")
            .to_owned(),
        interrupt: serde_json::from_value(stored["interrupt"].clone()).expect("interrupt"),
        checkpointer: Arc::new(MemoryCheckpointer::new()),
    }
}

/// FU-268: the upgraded Worker re-projects a card from a pause an earlier
/// Worker stored (a restore or a wake raises every open card again) with the
/// earlier card's bytes: the same payload digest, only the display argument
/// digest Main tolerates is new. A pause that carries an approval rule keeps
/// it in the card's bytes.
#[test]
fn a_tool_guard_card_from_an_earlier_pause_keeps_its_bytes_and_a_recorded_rule_is_kept() {
    let pause = pre_1540_tool_guard_pause();
    let card = tool_guard_card(&pause);
    let earlier = upgrade_card_fixture("pre-1540");
    let mut without_digest = card.clone();
    without_digest["display"]
        .as_object_mut()
        .expect("display")
        .remove("argument_digest");
    assert_eq!(without_digest, earlier, "the earlier Worker's card");
    assert_eq!(card, upgrade_card_fixture("fu268"), "the card Main replays");

    let digest_with = |rule: serde_json::Value| {
        let Interrupt::Dynamic {
            message,
            data: Some(mut data),
        } = pause.interrupt.clone()
        else {
            panic!("a dynamic tool pause");
        };
        data["data"]["approval_rule"] = rule;
        let ruled = ParallelBranchPause {
            thread_id: pause.thread_id.clone(),
            checkpoint_id: pause.checkpoint_id.clone(),
            interrupt: Interrupt::Dynamic {
                message,
                data: Some(data),
            },
            checkpointer: pause.checkpointer.clone(),
        };
        crate::agents::graph::parallel_cards::card_parts(&ruled)
            .expect("a ruled pause still renders")
            .payload_sha256
    };
    let policy =
        digest_with(json!({"source": "policy", "risk_tier": "high", "category": "delete"}));
    let platform = digest_with(json!({"source": "platform"}));
    let plain = card["payload_sha256"].as_str().expect("digest");
    assert_ne!(policy, plain, "a policy rule is part of the card");
    assert_ne!(platform, plain, "a recorded rule is part of the card");
    assert_ne!(policy, platform);
}

/// T-2A3 golden (Map on the shared fan-out adapter): one Parallel run that
/// pauses on a card in each child, then settles, captured on main
/// `0c41d5b65` before the adapter was generalized. It pins the child thread
/// ids and receipts, the parked cards and their parts, the join entries and
/// the `kind` of every fan-out span. The refactor must leave it unchanged.
mod shared_adapter_golden {
    use super::*;
    use elitea_agent_runtime::graph::fanout::identity::lowercase_hex;

    const CHILD_ENV: &str = "ELITEA_PARALLEL_GOLDEN_CHILD";
    const SUMMARY: &str = "PARALLEL_GOLDEN_SUMMARY ";

    async fn scenario() -> serde_json::Value {
        let fixture = Fixture::new(HITL_CHILD, PipelineNodeRuntimes::default());
        let paused = fixture
            .node(1)
            .execute(&context())
            .await
            .expect("both children pause");
        let Some(Interrupt::Dynamic {
            data: Some(data), ..
        }) = paused.interrupt.as_ref()
        else {
            panic!("expected the parked fan-out interrupt");
        };
        let mut children = Vec::new();
        for branch in ["left", "right"] {
            let thread = Fixture::thread(branch);
            let saved = fixture
                .store
                .load(&thread)
                .await
                .unwrap()
                .expect("paused child");
            let pause = fixture.pause_of(branch).await;
            let card = child_card(&pause).expect("proven").card;
            let parts = crate::agents::graph::parallel_cards::card_parts(&pause).expect("parts");
            children.push(json!({
                "thread": thread,
                "paused_checkpoint_id": pause.checkpoint_id,
                "receipt": saved.metadata[RECEIPT],
                "card": {"interrupt_id": card.interrupt_id, "tool_call_id": card.tool_call_id},
                "parts": {
                    "kind": format!("{:?}", parts.kind),
                    "available_actions": parts.available_actions,
                    "display": serde_json::to_value(&parts.display).expect("display json"),
                    "payload_sha256": parts.payload_sha256,
                },
            }));
        }
        let cards = parked(&paused);
        for (key, _) in &cards {
            fixture.ledger.decide(key, "approve", None);
        }
        let done = fixture
            .node(3)
            .execute(&context())
            .await
            .expect("both children done");
        // The activation's owned config digest, as main's `prepare` folds it
        // (`parallel.rs` owned-config): the node's config digest, then each
        // owned branch definition digest in declared order.
        let mut owned = ring::digest::Context::new(&ring::digest::SHA256);
        owned.update(b"elitea.graph.parallel.owned-config.v1\0");
        owned.update(&fixture.parallel.config_digest());
        for branch in fixture.parallel.branches() {
            owned.update(
                &fixture
                    .branches
                    .owned_definition_digest(branch)
                    .expect("owned digest"),
            );
        }
        let config_digest = lowercase_hex(owned.finish().as_ref());
        json!({
            "config_digest": config_digest,
            "pending": data["pending"],
            "children": children,
            "joined": done.updates["joined"],
        })
    }

    /// Runs in its own process under a global TRACE subscriber, so other
    /// tests' spans stay out of the capture (#1172).
    #[test]
    fn parallel_golden_child() {
        if std::env::var_os(CHILD_ENV).is_none() {
            return;
        }
        tracing_subscriber::fmt()
            .with_max_level(tracing::Level::TRACE)
            .with_span_events(tracing_subscriber::fmt::format::FmtSpan::NEW)
            .with_ansi(false)
            .with_writer(std::io::stdout)
            .init();
        let runtime = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(4)
            .enable_all()
            .build()
            .unwrap();
        let summary = runtime.block_on(scenario());
        println!("{SUMMARY}{summary}");
    }

    #[test]
    fn a_parallel_run_keeps_its_threads_cards_join_and_trace_kind() {
        let output = std::process::Command::new(std::env::current_exe().expect("test binary"))
            .args([
                "--exact",
                "agents::graph::compiler::parallel_compiler::card_tests::shared_adapter_golden::parallel_golden_child",
                "--nocapture",
                "--test-threads=1",
            ])
            .env(CHILD_ENV, "1")
            .output()
            .expect("the golden child runs");
        let stdout = String::from_utf8_lossy(&output.stdout);
        assert!(
            output.status.success(),
            "{stdout}\n{}",
            String::from_utf8_lossy(&output.stderr)
        );
        let summary = stdout
            .lines()
            .find_map(|line| line.strip_prefix(SUMMARY))
            .expect("the summary line");
        let mut summary: serde_json::Value = serde_json::from_str(summary).expect("summary JSON");
        // The node's config digest is frozen into every card key: pin it.
        assert_eq!(summary["config_digest"], GOLDEN_CONFIG_DIGEST);
        // Each card key is the contract function of its frozen inputs: the
        // node, step, config digest, 1-based ordinal and child thread pinned
        // here, plus this run's paused checkpoint and interrupt ids.
        for (ordinal, child) in summary["children"].as_array().unwrap().iter().enumerate() {
            let interrupt_id = child["card"]["interrupt_id"].as_str().unwrap();
            let expected = elitea_agent_runtime::graph::fanout::identity::interrupt_key(
                &elitea_agent_runtime::graph::fanout::identity::InterruptKeyInput {
                    root_thread: ROOT,
                    fanout_node_id: "gather",
                    step: 0,
                    config_digest: GOLDEN_CONFIG_DIGEST,
                    ordinal: u64::try_from(ordinal + 1).unwrap(),
                    child_thread: child["thread"].as_str().unwrap(),
                    child_paused_checkpoint_id: child["paused_checkpoint_id"].as_str().unwrap(),
                    interrupt_id,
                    tool_call_id: child["card"]["tool_call_id"].as_str().unwrap(),
                },
            );
            let pending = summary["pending"]
                .as_array()
                .unwrap()
                .iter()
                .find(|card| card["interrupt_id"] == interrupt_id)
                .expect("the child's card is parked");
            assert_eq!(
                pending["interrupt_key"],
                expected.as_str(),
                "ordinal {}",
                ordinal + 1
            );
        }
        mask_run_ids(&mut summary);
        let digest = lowercase_hex(
            ring::digest::digest(&ring::digest::SHA256, summary.to_string().as_bytes()).as_ref(),
        );
        // Readable pins first, so a change says what moved.
        let threads: Vec<&str> = summary["children"]
            .as_array()
            .unwrap()
            .iter()
            .map(|child| child["thread"].as_str().unwrap())
            .collect();
        assert_eq!(threads, ["child:gather:left:0:0", "child:gather:right:1:0"]);
        // Two cards, keyed as recomputed above.
        assert_eq!(summary["pending"].as_array().unwrap().len(), 2);
        assert_eq!(
            summary["joined"],
            json!([
                {"branch_id": "left", "node": "workerA", "outputs": {}, "status": "succeeded"},
                {"branch_id": "right", "node": "workerB", "outputs": {}, "status": "succeeded"},
            ])
        );
        let fanout_spans: Vec<&str> = stdout
            .lines()
            .filter(|line| line.contains("graph.fanout."))
            .collect();
        assert!(
            fanout_spans
                .iter()
                .any(|line| line.contains("graph.fanout.child{kind=\"parallel\""))
        );
        assert!(
            fanout_spans
                .iter()
                .any(|line| line.contains("graph.fanout.activation{kind=\"parallel\""))
        );
        assert!(
            !stdout.contains("kind=\"map\""),
            "a Parallel run traced as Map"
        );
        // Every byte of the threads, receipts, cards, parts and join entries.
        assert_eq!(
            digest, GOLDEN_DIGEST,
            "the Parallel run changed; see the readable pins above"
        );
    }

    /// The in-memory store mints a random checkpoint id per save, and the
    /// interrupt id, card key and payload digest derive from it; their
    /// derivation is checked above, so only these values are masked.
    fn mask_run_ids(value: &mut serde_json::Value) {
        match value {
            serde_json::Value::Object(map) => {
                for (key, entry) in map.iter_mut() {
                    if matches!(
                        key.as_str(),
                        "checkpoint_id"
                            | "paused_checkpoint_id"
                            | "interrupt_id"
                            | "interrupt_key"
                            | "payload_sha256"
                    ) {
                        *entry = json!("<run>");
                    } else {
                        mask_run_ids(entry);
                    }
                }
            }
            serde_json::Value::Array(items) => items.iter_mut().for_each(mask_run_ids),
            _ => {}
        }
    }

    const GOLDEN_CONFIG_DIGEST: &str =
        "609ff69f650951a18b0cb7e33c026ab68117c3b62db37f5dc3c9c5b7d57c6f00";
    // `display` is pinned as its serialized bytes (what is stored and sent), not as Debug text.
    const GOLDEN_DIGEST: &str = "7ea1ad1952ce8a08aac8e948266833584900dc5d587595e4e448d2eec89bbc9a";
}

/// FU-250 (M2, the FU-268 rule): a delegated sign-in pause stored without a
/// toolkit id (by an earlier Worker) re-raises its card with no
/// `display.toolkit_id`, so its bytes are the earlier ones and Main keeps it
/// skip-only; a pause that stored its toolkit id carries it in the card.
#[tokio::test(flavor = "current_thread")]
async fn a_delegated_auth_card_names_its_toolkit_only_when_its_pause_stored_it() {
    let authorized = Arc::new(AtomicBool::new(false));
    let resolver: Arc<dyn PipelineDirectToolResolver> = Arc::new(FixtureResolver {
        alias: "Customer Support".to_owned(),
        tool: Arc::new(AuthorizationFixtureTool {
            authorized: Arc::clone(&authorized),
            toolkit_type: "mcp",
        }),
        sensitive: None,
    });
    let child = sensitive_pipeline_definition_yaml().replace("type: toolkit", "type: mcp");
    let fixture = Fixture::new(&child, direct_tool_runtimes(resolver));
    both_paused(&fixture, FanoutCardKind::DelegatedAuth).await;
    let earlier = fixture.pause_of("right").await;
    let earlier_parts = crate::agents::graph::parallel_cards::card_parts(&earlier)
        .expect("an earlier pause renders");
    let earlier_display = serde_json::to_value(&earlier_parts.display).expect("display json");
    assert!(
        earlier_display.get("toolkit_id").is_none(),
        "{earlier_display}"
    );

    let Interrupt::Dynamic {
        message,
        data: Some(mut data),
    } = earlier.interrupt.clone()
    else {
        panic!("a dynamic sign-in pause");
    };
    data["data"]["toolkit_id"] = json!(17);
    let stored = ParallelBranchPause {
        thread_id: earlier.thread_id.clone(),
        checkpoint_id: earlier.checkpoint_id.clone(),
        interrupt: Interrupt::Dynamic {
            message,
            data: Some(data),
        },
        checkpointer: earlier.checkpointer.clone(),
    };
    let parts = crate::agents::graph::parallel_cards::card_parts(&stored)
        .expect("a pause with its toolkit renders");
    let mut display = serde_json::to_value(&parts.display).expect("display json");
    assert_eq!(display["toolkit_id"], json!(17), "{display}");
    display
        .as_object_mut()
        .expect("display")
        .remove("toolkit_id");
    assert_eq!(display, earlier_display, "only the toolkit id is new");
}
