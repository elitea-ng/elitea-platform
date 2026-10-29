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

impl Fixture {
    fn new(child: &str, runtimes: PipelineNodeRuntimes) -> Self {
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
                owned,
                state_types: pipeline.state,
                resolver: Some(Arc::new(SavedPipeline {
                    yaml: child.to_owned(),
                    runtimes,
                })),
                llm: None,
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
    fixture.ledger.decide(&right.0, action, value);
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
