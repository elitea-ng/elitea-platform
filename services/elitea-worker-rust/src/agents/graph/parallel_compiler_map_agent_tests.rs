//! T-2A3: a Map's saved Agent and LLM workers on the shared fan-out adapter.
//! Each item pauses with its own card and resumes only its own child; a
//! delegated sign-in decided without its credential ends that item typed
//! (M1, the gate a Parallel member meets); an LLM worker runs per item.

use std::sync::atomic::{AtomicBool, Ordering};

use adk_rust::futures::stream;
use adk_rust::graph::interrupt::Interrupt;
use adk_rust::graph::{Checkpoint, ExecutionConfig, MemoryCheckpointer, Node, NodeContext};
use adk_rust::{Agent, Content, Event, EventStream, InvocationContext, Part};
use serde_json::json;

use super::*;
use crate::agents::graph::application::{
    ApplicationExecutionError, PipelineApplicationSelection, ResolvedApplicationParticipant,
};
use crate::agents::graph::direct_tool::PipelineDirectToolResolver;
use crate::agents::graph::direct_tool_tests::{
    AuthorizationFixtureTool, FixtureResolver, sensitive_pipeline_definition_yaml,
};
use crate::agents::graph::fanout_test_ledger::TestLedger;
use crate::agents::graph::llm::{
    LlmExecutionError, LlmExecutionInput, PipelineLlmAgentBinding, PipelineLlmReplayEnvelope,
    PipelineModelScope,
};
use crate::agents::graph::parallel::{
    ParallelActivation, ParallelCheckpointAppender, ParallelChildCheckpoint,
    ParallelChildCheckpointerFactory, ParallelChildOrigin,
};
use crate::agents::graph::parallel_cards::{FanoutCardKind, child_card};

const ROOT: &str = "root-map-agents";
const RECEIPT: &str = "elitea.graph.parallel.branch-receipt.v1";
const DELEGATED_SERVER: &str = "https://mcp.example.invalid/v1/mcp";
const TEST_CREDENTIAL_REF: &str = "test-only-reference-0123456789abcdef";

const AGENT_MAP: &str = r"
state:
  messages: list
  items: list
  answer: str
  mapped: list
entry_point: each
nodes:
  - id: each
    type: map
    worker: workerA
    source: items
    item: entity
    index: item_index
    outputs: [answer]
    destination: mapped
    max_items: 8
    max_concurrency: 2
    on_error: continue
    join: append
    transition: END
  - id: workerA
    type: agent
    tool: Research Agent
    input: [entity, item_index]
    input_mapping:
      task: {type: fixed, value: deployment}
    output: [answer]
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

/// One store holds the parent and every item thread, as one claim does.
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
            "item:{}:{}:{ordinal}:{}",
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
        })
    }
}

/// A saved pipeline participant compiled for each item's own checkpointer.
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

struct MapFixture {
    store: Arc<Store>,
    branches: Arc<CompilerParallelBranches>,
    source: FanoutSource,
    ledger: Arc<TestLedger>,
}

impl MapFixture {
    fn agent(child: &str, runtimes: PipelineNodeRuntimes, servers: &[&str]) -> Self {
        let pipeline = PipelineDefinition::from_yaml(AGENT_MAP).expect("Map pipeline");
        let worker = pipeline
            .nodes
            .iter()
            .find_map(|node| match node {
                PipelineNodeDefinition::Application(node) => Some(OwnedBranch::Agent(node.clone())),
                _ => None,
            })
            .expect("the Agent worker");
        let resolver: Arc<dyn PipelineApplicationResolver> = Arc::new(SavedPipeline {
            yaml: child.to_owned(),
            runtimes,
        });
        Self::new(&pipeline, &worker, Some(resolver), None, servers)
    }

    fn new(
        pipeline: &PipelineDefinition,
        worker: &OwnedBranch,
        resolver: Option<Arc<dyn PipelineApplicationResolver>>,
        llm: Option<Arc<dyn PipelineLlmAgentFactory>>,
        servers: &[&str],
    ) -> Self {
        let map = pipeline
            .nodes
            .iter()
            .find_map(|node| match node {
                PipelineNodeDefinition::Map(node) => Some(node.runtime().clone()),
                _ => None,
            })
            .expect("the Map node");
        let branches = Arc::new(CompilerParallelBranches {
            delegated_servers: Arc::new(servers.iter().map(|&url| url.to_owned()).collect()),
            owned: (0..map.max_items)
                .map(|index| (index.to_string(), worker.clone()))
                .collect(),
            state_types: pipeline.state.clone(),
            resolver,
            llm,
            events: None,
            root_session: None,
        });
        let worker_digest = branches
            .owned_definition_digest(&crate::agents::graph::ParallelBranchDefinition::map_item(
                0,
                &map.worker,
            ))
            .expect("the worker identity");
        Self {
            store: Arc::new(Store::default()),
            branches,
            source: FanoutSource::Map(MapSource::new(map, worker_digest).expect("Map source")),
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
        DurableParallelNode::from_source(self.source.clone(), runtime)
            .with_decisions(self.ledger.client(claim))
    }

    fn thread(index: usize) -> String {
        format!("item:each:{index}:{index}:0")
    }

    async fn pause_of(&self, index: usize) -> ParallelBranchPause {
        let thread = Self::thread(index);
        let saved = self
            .store
            .load(&thread)
            .await
            .unwrap()
            .expect("a paused item checkpoint");
        let receipt = &saved.metadata[RECEIPT];
        assert_eq!(receipt["status"], "paused");
        ParallelBranchPause {
            thread_id: thread,
            checkpoint_id: saved.checkpoint_id,
            interrupt: serde_json::from_value::<Interrupt>(receipt["interrupt"].clone()).unwrap(),
            checkpointer: self.store.clone(),
        }
    }

    async fn settled(&self, index: usize) -> bool {
        self.store
            .load(&Self::thread(index))
            .await
            .unwrap()
            .is_some_and(|saved| saved.pending_nodes.is_empty())
    }

    /// Both items pause with one card each, of `kind`: `[item 0, item 1]`.
    async fn both_paused(&self, kind: FanoutCardKind) -> [(String, String); 2] {
        let cards = parked(
            &self
                .node(1)
                .execute(&context())
                .await
                .expect("both items pause"),
        );
        assert_eq!(cards.len(), 2, "one card per item");
        let mut by_item = Vec::new();
        for index in 0..2 {
            let proven = child_card(&self.pause_of(index).await).expect("the item pause is proven");
            assert_eq!(proven.kind, kind);
            by_item.push(
                cards
                    .iter()
                    .find(|(_, id)| *id == proven.card.interrupt_id)
                    .cloned()
                    .expect("the item's card is parked"),
            );
        }
        assert_ne!(by_item[0].0, by_item[1].0, "items share no card");
        [by_item[0].clone(), by_item[1].clone()]
    }
}

fn context() -> NodeContext {
    NodeContext::new(
        State::from([
            ("messages".to_owned(), json!([])),
            ("items".to_owned(), json!(["a", "b"])),
            ("mapped".to_owned(), json!([])),
        ]),
        ExecutionConfig::new(ROOT),
        0,
    )
}

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

/// A saved Agent worker pauses once per item; answering item 1's card
/// resumes only item 1, then item 0's card settles the Map in item order.
#[tokio::test]
async fn a_map_agent_worker_pauses_per_item_and_each_card_resumes_only_its_item() {
    let fixture = MapFixture::agent(HITL_CHILD, PipelineNodeRuntimes::default(), &[]);
    let [first, second] = fixture.both_paused(FanoutCardKind::HitlNode).await;
    fixture.ledger.decide(&second.0, "approve", None);
    let remaining = parked(
        &fixture
            .node(2)
            .execute(&context())
            .await
            .expect("item 1 resumed"),
    );
    assert_eq!(remaining, vec![first.clone()]);
    assert!(fixture.settled(1).await, "the answered item did not resume");
    assert!(!fixture.settled(0).await, "the sibling item resumed");
    fixture.ledger.decide(&first.0, "reject", None);
    let output = fixture
        .node(3)
        .execute(&context())
        .await
        .expect("both items done");
    assert!(output.interrupt.is_none());
    let mapped = output.updates["mapped"].as_array().expect("joined entries");
    assert_eq!(
        mapped
            .iter()
            .map(|entry| (entry["index"].clone(), entry["status"].clone()))
            .collect::<Vec<_>>(),
        vec![
            (json!(0), json!("succeeded")),
            (json!(1), json!("succeeded"))
        ]
    );
    assert_eq!(fixture.ledger.open_decisions(), 0);
}

/// M1: a Map item's delegated sign-in meets the gate a Parallel member
/// meets. Authorized without a run-held token for its server, or with a
/// reference for a server the run holds none for, the item ends
/// `graph.fanout.credential_unavailable` and its call never runs authorized.
#[tokio::test(flavor = "current_thread")]
async fn a_map_item_authorized_without_its_credential_ends_typed() {
    for (servers, with_reference) in [(&[DELEGATED_SERVER][..], false), (&[][..], true)] {
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
        let fixture = MapFixture::agent(
            &child,
            PipelineNodeRuntimes::new(None, Some(resolver), None),
            servers,
        );
        let [first, second] = fixture.both_paused(FanoutCardKind::DelegatedAuth).await;
        authorized.store(true, Ordering::Release);
        if with_reference {
            fixture
                .ledger
                .decide_with_credential(&second.0, TEST_CREDENTIAL_REF);
        } else {
            fixture.ledger.decide(&second.0, "authorize", None);
        }
        fixture.ledger.decide(&first.0, "skip", None);
        let output = fixture
            .node(2)
            .execute(&context())
            .await
            .expect("both items settle");
        let mapped = output.updates["mapped"].as_array().expect("joined entries");
        let refused = mapped
            .iter()
            .find(|entry| entry["status"] == "failed")
            .unwrap_or_else(|| {
                panic!("servers {servers:?}, reference {with_reference}: {mapped:?}")
            });
        assert_eq!(refused["index"], json!(1), "{mapped:?}");
        assert_eq!(
            refused["error"]["code"], "graph.fanout.credential_unavailable",
            "{mapped:?}"
        );
        let leaf = fixture
            .store
            .load(&format!("{}/workerA", MapFixture::thread(1)))
            .await
            .unwrap()
            .expect("item 1's pipeline checkpoint");
        assert_ne!(
            leaf.state.get("report"),
            Some(&json!({"authorized": true})),
            "the call never ran authorized"
        );
    }
}

const LLM_MAP: &str = r"
state:
  messages: list
  items: list
  answer: str
  mapped: list
entry_point: each
nodes:
  - id: each
    type: map
    worker: draft
    source: items
    item: entity
    index: item_index
    outputs: [answer]
    destination: mapped
    max_items: 8
    max_concurrency: 2
    on_error: continue
    join: append
    transition: END
  - id: draft
    type: llm
    input: [entity, item_index]
    input_mapping:
      task: {type: fstring, value: 'Draft {entity}'}
    output: [answer]
";

/// An inline LLM worker: the model answers with its task text.
struct EchoModel;

impl PipelineLlmAgentFactory for EchoModel {
    fn build(
        &self,
        _definition: &LlmNodeDefinition,
        _input: &LlmExecutionInput,
        _output_schema: Option<serde_json::Value>,
        _replay: Option<&PipelineLlmReplayEnvelope>,
        _scope: &PipelineModelScope,
    ) -> Result<PipelineLlmAgentBinding, LlmExecutionError> {
        Ok(PipelineLlmAgentBinding::new(
            Arc::new(EchoAgent),
            BTreeMap::new(),
        ))
    }

    fn for_fanout_branch(&self) -> Option<Arc<dyn PipelineLlmAgentFactory>> {
        Some(Arc::new(Self))
    }
}

struct EchoAgent;

#[async_trait]
impl Agent for EchoAgent {
    fn name(&self) -> &'static str {
        "draft"
    }

    fn description(&self) -> &'static str {
        "Map LLM worker fixture"
    }

    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        &[]
    }

    async fn run(&self, context: Arc<dyn InvocationContext>) -> adk_rust::Result<EventStream> {
        let task = context
            .user_content()
            .parts
            .iter()
            .filter_map(Part::text)
            .collect::<String>();
        let mut event = Event::new(context.invocation_id());
        event.set_content(Content::new("model").with_text(format!("drafted: {task}")));
        Ok(Box::pin(stream::iter([Ok(event)])))
    }
}

/// An inline LLM worker runs once per item on its own frozen mapped input.
#[tokio::test]
async fn a_map_llm_worker_runs_once_per_item_on_its_frozen_input() {
    let pipeline = PipelineDefinition::from_yaml(LLM_MAP).expect("Map pipeline");
    let worker = pipeline
        .nodes
        .iter()
        .find_map(|node| match node {
            PipelineNodeDefinition::Llm(node) => Some(OwnedBranch::Llm(node.clone())),
            _ => None,
        })
        .expect("the LLM worker");
    let fixture = MapFixture::new(&pipeline, &worker, None, Some(Arc::new(EchoModel)), &[]);
    let output = fixture
        .node(1)
        .execute(&context())
        .await
        .expect("both items answer");
    assert!(output.interrupt.is_none());
    let mapped = output.updates["mapped"].as_array().expect("joined entries");
    assert_eq!(mapped.len(), 2, "{mapped:?}");
    for (index, entity) in ["a", "b"].iter().enumerate() {
        assert_eq!(mapped[index]["index"], json!(index));
        assert_eq!(mapped[index]["status"], "succeeded", "{mapped:?}");
        let answer = mapped[index]["outputs"]["answer"]
            .as_str()
            .unwrap_or_default();
        assert!(answer.contains(&format!("Draft {entity}")), "{mapped:?}");
    }
}

/// M1 (positive): a Map item's delegated sign-in authorized with a resolved
/// token for a server the run holds resumes only that item, which then
/// calls authorized; the sibling is skipped; the credential reference
/// reaches no checkpoint of either item or their pipelines. That the token
/// is presented only to the toolset of its own resource is the shared MCP
/// toolset's rule (`a_projected_token_reaches_only_the_toolset_of_its_resource`,
/// agent-runtime `mcp_tests.rs`), the same for every fan-out family.
#[tokio::test(flavor = "current_thread")]
async fn a_map_item_authorized_with_its_credential_resumes_only_that_item() {
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
    let fixture = MapFixture::agent(
        &child,
        PipelineNodeRuntimes::new(None, Some(resolver), None),
        &[DELEGATED_SERVER],
    );
    let [first, second] = fixture.both_paused(FanoutCardKind::DelegatedAuth).await;
    authorized.store(true, Ordering::Release);
    fixture
        .ledger
        .decide_with_credential(&second.0, TEST_CREDENTIAL_REF);
    let remaining = parked(
        &fixture
            .node(2)
            .execute(&context())
            .await
            .expect("item 1 resumed"),
    );
    assert_eq!(remaining, vec![first.clone()]);
    assert!(
        fixture.settled(1).await,
        "the authorized item did not resume"
    );
    assert!(!fixture.settled(0).await, "the sibling item resumed");
    let leaf = fixture
        .store
        .load(&format!("{}/workerA", MapFixture::thread(1)))
        .await
        .unwrap()
        .expect("item 1's pipeline checkpoint");
    assert_eq!(leaf.state["report"], json!({"authorized": true}));
    fixture.ledger.decide(&first.0, "skip", None);
    let output = fixture
        .node(3)
        .execute(&context())
        .await
        .expect("both items done");
    assert!(output.interrupt.is_none());
    for index in 0..2 {
        for thread in [
            MapFixture::thread(index),
            format!("{}/workerA", MapFixture::thread(index)),
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
}
