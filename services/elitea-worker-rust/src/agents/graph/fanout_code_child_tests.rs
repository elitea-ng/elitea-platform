//! W3-2a: a Code node run directly as a Map worker or a Parallel branch.
//! The compiled graph runs on the in-memory fan-out authority of
//! `map_fanout_tests`; the Code runtime is a fake that records each attempt.

use std::sync::Mutex as StdMutex;

use elitea_agent_runtime::graph::fanout::admission::FanoutDeadlineInsufficient;

use super::*;
use crate::agents::graph::code_runtime::{CodeInvocation, CodeSandboxRuntime, FanoutCodeAdmission};

const MAP_CODE: &str = "entry_point: map_entities\nstate:\n  entities: {type: list, value: []}\n  item_result: {type: str, value: ''}\n  mapped: {type: list, value: []}\nnodes:\n  - {id: map_entities, type: map, worker: shout, source: entities, item: entity, index: item_index, outputs: [item_result], destination: mapped, max_items: 64, max_concurrency: 4, reduction: ordered_collection, transition: END}\n  - {id: shout, type: code, code: 'print(1)', input: [entity, item_index], output: [item_result]}\n";

const PARALLEL_CODE: &str = "entry_point: gather\nstate:\n  topic: str\n  detail: str\n  joined: list\nnodes:\n  - {id: gather, type: parallel, branches: [{id: left, node: code_a}, {id: right, node: code_b}], max_concurrency: 2, wait: all, output: [joined], transition: END}\n  - {id: code_a, type: code, code: 'a', input: [topic], output: [detail]}\n  - {id: code_b, type: code, language: javascript, code: 'b', input: [topic], output: [detail]}\n";

/// Records every Code attempt and every fan-out admission it is asked for.
#[derive(Default)]
struct FakeCode {
    activations: StdMutex<Vec<[u8; 32]>>,
    admissions: StdMutex<Vec<(Vec<usize>, u32)>>,
    refuse_fanout: bool,
    /// Item indices whose attempt is refused by its own deadline check.
    refuse_items: Vec<i64>,
}

fn receipt(result: &Value) -> Vec<u8> {
    serde_json::to_vec(&json!({
        "revision": 1, "status": "completed", "exit_code": 0,
        "stdout": json!({"revision": 1, "result": result}).to_string(), "stderr": ""
    }))
    .unwrap()
}

#[async_trait]
impl CodeSandboxRuntime for FakeCode {
    fn admit_fanout(
        &self,
        request: &FanoutCodeAdmission<'_>,
    ) -> Result<(), FanoutDeadlineInsufficient> {
        self.admissions.lock().unwrap().push((
            request.children.iter().map(Vec::len).collect(),
            request.concurrency,
        ));
        if self.refuse_fanout {
            return Err(FanoutDeadlineInsufficient {
                required: Duration::from_secs(120),
                remaining: Duration::from_secs(1),
            });
        }
        Ok(())
    }

    async fn execute(&self, invocation: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
        self.execute_node(invocation)
            .await
            .map_err(|(error, _)| error)
    }

    async fn execute_node(
        &self,
        invocation: CodeInvocation<'_>,
    ) -> Result<Vec<u8>, (GraphError, Option<&'static str>)> {
        let input: Value = serde_json::from_slice(&invocation.input_json).unwrap();
        if input["item_index"]
            .as_i64()
            .is_some_and(|index| self.refuse_items.contains(&index))
        {
            return Err((
                GraphError::Other(
                    crate::protocol::output::CODE_DEADLINE_INSUFFICIENT_MESSAGE.into(),
                ),
                Some(crate::protocol::output::CODE_DEADLINE_INSUFFICIENT_UPSTREAM_CODE),
            ));
        }
        self.activations.lock().unwrap().push(invocation.activation);
        let result = match input.get("entity") {
            Some(entity) => json!(format!("{}-{}", entity, input["item_index"])),
            None => json!(format!(
                "{}:{:?}",
                input["topic"].as_str().unwrap_or_default(),
                invocation.language
            )),
        };
        Ok(receipt(&result))
    }
}

/// Parent rows and branch children for a fixed Parallel (the Map fixture
/// derives only Map items).
#[derive(Default)]
struct BranchAuthority {
    parent: ParentRows,
    stores: Mutex<HashMap<String, Arc<MemoryCheckpointer>>>,
}

#[async_trait]
impl Checkpointer for BranchAuthority {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        self.parent.save(checkpoint).await
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.parent.load(thread).await
    }
    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.parent.load_by_id(id).await
    }
    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.parent.list(thread).await
    }
    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.parent.delete(thread).await
    }
}

#[async_trait]
impl ParallelCheckpointAppender for BranchAuthority {
    async fn append_after(
        &self,
        expected: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        self.parent.append_after(expected, candidate).await
    }
}

#[async_trait]
impl ParallelChildCheckpointerFactory for BranchAuthority {
    fn child_origin(&self, _: &ParallelActivation) -> Result<ParallelChildOrigin, GraphError> {
        Ok(ParallelChildOrigin {
            execution_id: "parallel-execution".to_owned(),
            generation: 1,
        })
    }
    fn branch_thread_id(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        _: &ParallelChildOrigin,
    ) -> Result<String, GraphError> {
        Ok(format!(
            "branch:{}:{}:{ordinal}:{input_digest:?}",
            activation.root_thread_id,
            branch.id()
        ))
    }
    async fn for_branch(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<ParallelChildCheckpoint, GraphError> {
        let thread_id = self.branch_thread_id(activation, branch, ordinal, input_digest, origin)?;
        let store = Arc::clone(
            self.stores
                .lock()
                .await
                .entry(thread_id.clone())
                .or_insert_with(|| Arc::new(MemoryCheckpointer::new())),
        );
        Ok(ParallelChildCheckpoint {
            admitted_threads: std::collections::BTreeSet::from([thread_id.clone()]),
            thread_id,
            checkpointer: store,
            node_recovery: None,
        })
    }
}

impl ParallelCheckpointAuthority for BranchAuthority {}

fn with_code(runtimes: PipelineNodeRuntimes, code: &Arc<FakeCode>) -> PipelineNodeRuntimes {
    runtimes.with_code(code.clone())
}

/// W3-2: each Map item runs the Code worker once, on its own child thread
/// (distinct attempts), and lands in source order. The activation was
/// admitted once, before freezing, with one Code node per item.
#[tokio::test]
async fn a_map_runs_its_code_worker_once_per_item() {
    let definition = PipelineDefinition::from_yaml(MAP_CODE).expect("a Code worker is admitted");
    let authority = Arc::new(MemoryAuthority::default());
    let (checkpointer, runtimes) = bound(&authority);
    let code = Arc::new(FakeCode::default());
    let graph = definition
        .compile_subgraph_with_runtime(checkpointer, &with_code(runtimes, &code))
        .expect("the Map compiles");
    let state = graph
        .invoke(
            HashMap::from([("entities".to_owned(), json!(["a", {"k": 1}, 3]))]),
            ExecutionConfig::new("root-code-map"),
        )
        .await
        .expect("the Map runs");
    assert_eq!(
        state["mapped"],
        json!([
            {"index": 0, "outputs": {"item_result": "\"a\"-0"}},
            {"index": 1, "outputs": {"item_result": "{\"k\":1}-1"}},
            {"index": 2, "outputs": {"item_result": "3-2"}},
        ])
    );
    let activations = code.activations.lock().unwrap().clone();
    assert_eq!(activations.len(), 3);
    assert_eq!(
        activations
            .iter()
            .collect::<std::collections::BTreeSet<_>>()
            .len(),
        3,
        "every item is its own attempt"
    );
    assert_eq!(*code.admissions.lock().unwrap(), [(vec![1, 1, 1], 4)]);
    assert_eq!(authority.children.issued.lock().await.len(), 3);
}

/// W3-2: two Code branches of a Parallel run, each in its own language.
#[tokio::test]
async fn a_parallel_runs_its_code_branches() {
    let definition =
        PipelineDefinition::from_yaml(PARALLEL_CODE).expect("Code branches are admitted");
    let authority: Arc<dyn ParallelCheckpointAuthority> = Arc::new(BranchAuthority::default());
    let checkpointer: Arc<dyn Checkpointer> = authority.clone();
    let runtimes =
        PipelineNodeRuntimes::default().with_parallel(ParallelCompilerBinding::for_tests(
            authority,
            tokio::time::Instant::now() + Duration::from_secs(30),
            None,
        ));
    let code = Arc::new(FakeCode::default());
    let graph = definition
        .compile_subgraph_with_runtime(checkpointer, &with_code(runtimes, &code))
        .expect("the Parallel compiles");
    let state = graph
        .invoke(
            HashMap::from([("topic".to_owned(), json!("t"))]),
            ExecutionConfig::new("root-code-parallel"),
        )
        .await
        .expect("the Parallel runs");
    let joined = serde_json::to_string(&state["joined"]).unwrap();
    assert!(
        joined.contains("t:Python") && joined.contains("t:JavaScript"),
        "{joined}"
    );
    assert_eq!(code.activations.lock().unwrap().len(), 2);
    assert_eq!(*code.admissions.lock().unwrap(), [(vec![1, 1], 2)]);
}

/// W3-2 D3: an activation whose Code items cannot fit the run's deadline is
/// refused whole, before freezing: no item starts, no child thread is
/// activated, and the run ends with the registered fan-out failure.
#[tokio::test]
async fn a_refused_code_fanout_starts_no_item_and_ends_typed() {
    let definition = PipelineDefinition::from_yaml(MAP_CODE).unwrap();
    let authority = Arc::new(MemoryAuthority::default());
    let (checkpointer, runtimes) = bound(&authority);
    let code = Arc::new(FakeCode {
        refuse_fanout: true,
        ..FakeCode::default()
    });
    let (sender, receiver) = crate::agents::graph::node_events::pipeline_node_event_channel();
    let graph = definition
        .compile_subgraph_with_runtime(
            checkpointer,
            &with_code(runtimes, &code).with_events(sender),
        )
        .expect("the Map compiles");
    let error = graph
        .invoke(
            HashMap::from([("entities".to_owned(), json!(["a", "b"]))]),
            ExecutionConfig::new("root-code-refused"),
        )
        .await
        .expect_err("the activation is refused");
    assert!(
        error
            .to_string()
            .contains(crate::protocol::output::FANOUT_DEADLINE_INSUFFICIENT_UPSTREAM_CODE),
        "{error}"
    );
    drop(graph);
    assert_eq!(
        receiver.try_failure_code().await,
        Some(crate::protocol::output::FANOUT_DEADLINE_INSUFFICIENT_UPSTREAM_CODE)
    );
    assert!(code.activations.lock().unwrap().is_empty());
    assert!(authority.children.issued.lock().await.is_empty());
}

/// W3-2 D3: a child without Code has no Code time to bound; a refusing Code
/// runtime never sees the State Modifier Map.
#[tokio::test]
async fn a_map_without_code_children_is_not_admitted_against_code_time() {
    let definition = PipelineDefinition::from_yaml(PIPELINE).unwrap();
    let authority = Arc::new(MemoryAuthority::default());
    let (checkpointer, runtimes) = bound(&authority);
    let code = Arc::new(FakeCode {
        refuse_fanout: true,
        ..FakeCode::default()
    });
    let graph = definition
        .compile_subgraph_with_runtime(checkpointer, &with_code(runtimes, &code))
        .unwrap();
    graph
        .invoke(
            HashMap::from([("entities".to_owned(), json!(["a"]))]),
            ExecutionConfig::new("root-no-code"),
        )
        .await
        .expect("no Code item, nothing to refuse");
    assert!(code.admissions.lock().unwrap().is_empty());
}

/// N1: an item whose own Code attempt is refused by the per-attempt deadline
/// check (the activation estimate was off) is a member failure that names the
/// Code deadline refusal and the item, not a generic node failure.
#[tokio::test]
async fn a_per_attempt_refusal_names_the_failed_item() {
    let definition = PipelineDefinition::from_yaml(MAP_CODE).unwrap();
    let authority = Arc::new(MemoryAuthority::default());
    let (checkpointer, runtimes) = bound(&authority);
    let code = Arc::new(FakeCode {
        refuse_items: vec![1],
        ..FakeCode::default()
    });
    let graph = definition
        .compile_subgraph_with_runtime(checkpointer, &with_code(runtimes, &code))
        .unwrap();
    let error = graph
        .invoke(
            HashMap::from([("entities".to_owned(), json!(["a", "b", "c"]))]),
            ExecutionConfig::new("root-code-n1"),
        )
        .await
        .expect_err("the v1 Map stops on the refused item");
    let message = error.to_string();
    assert!(
        message.contains(crate::protocol::output::CODE_DEADLINE_INSUFFICIENT_UPSTREAM_CODE)
            && message.contains('1'),
        "{message}"
    );
}

/// A Code child needs the run's admitted sandbox runtime.
#[test]
fn a_code_child_without_a_sandbox_runtime_is_refused() {
    for yaml in [MAP_CODE, PARALLEL_CODE] {
        let definition = PipelineDefinition::from_yaml(yaml).unwrap();
        let authority = Arc::new(MemoryAuthority::default());
        let (checkpointer, runtimes) = bound(&authority);
        match definition.compile_subgraph_with_runtime(checkpointer, &runtimes) {
            Err(PipelineConfigurationError::Unsupported(message)) => {
                assert_eq!(
                    message,
                    "Code execution requires an admitted sandbox backend"
                );
            }
            Err(other) => panic!("expected the sandbox refusal, got {other:?}"),
            Ok(_) => panic!("a Code child compiled without a sandbox runtime"),
        }
    }
}

/// Q7: a Router as the direct Map worker or Parallel branch stays refused,
/// typed as before, with its own readable text.
#[test]
fn a_direct_router_child_is_refused_with_its_own_text() {
    let router = "type: router, condition: 'END', routes: [END], default_output: END";
    for yaml in [
        MAP_CODE.replace(
            "type: code, code: 'print(1)', input: [entity, item_index], output: [item_result]",
            &format!("{router}, input: [entity, item_index]"),
        ),
        PARALLEL_CODE.replace(
            "type: code, code: 'a', input: [topic], output: [detail]",
            &format!("{router}, input: [topic]"),
        ),
    ] {
        match PipelineDefinition::from_yaml(&yaml) {
            Err(error @ PipelineConfigurationError::FanoutChildUnsupported { .. }) => {
                assert_eq!(error.code(), "graph.fanout.child_kind_unsupported");
                assert_eq!(error.cause_detail(), Some("fanout_child_router"));
                assert!(
                    error
                        .to_string()
                        .contains("a Router runs inside a child pipeline"),
                    "{error}"
                );
            }
            Err(other) => panic!("expected the Router refusal, got {other:?}"),
            Ok(_) => panic!("a direct Router child compiled"),
        }
    }
}
