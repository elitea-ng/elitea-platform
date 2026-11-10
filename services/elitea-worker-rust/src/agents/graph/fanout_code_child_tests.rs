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
    /// Item indices (Map) whose attempt ends as the journal ends an attempt
    /// that may have started without owner proof (D1).
    effect_unknown_items: Vec<i64>,
    /// The JavaScript branch (Parallel) ends that way too.
    effect_unknown_javascript: bool,
    /// Every attempt, whatever its outcome.
    attempts: StdMutex<usize>,
    /// Item indices whose sandbox job fails (an ordinary Code failure).
    fail_items: Vec<i64>,
}

/// The item index a Map worker reads, whatever its index channel is named.
fn item_index(input: &Value) -> Option<i64> {
    input
        .get("item_index")
        .or_else(|| input.get("secret_index_marker"))
        .and_then(Value::as_i64)
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
        *self.attempts.lock().unwrap() += 1;
        if item_index(&input).is_some_and(|index| self.fail_items.contains(&index)) {
            return Err((GraphError::Other("sandbox job failed".into()), None));
        }
        if item_index(&input).is_some_and(|index| self.effect_unknown_items.contains(&index))
            || (self.effect_unknown_javascript
                && invocation.language == super::super::code::CodeLanguage::JavaScript)
        {
            return Err((
                GraphError::Other(
                    super::super::node_recovery_runtime::CHILD_EFFECT_UNKNOWN_CODE.into(),
                ),
                None,
            ));
        }
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
            // A large item answers with its length, so the result stays small.
            Some(Value::String(entity)) if entity.len() > 64 => json!(entity.len().to_string()),
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

/// R1: a Code child whose `input` is empty or exactly `[messages]` reads every
/// business root it is given, as a root Code node does: a Map worker gets its
/// item and index, a Parallel branch the parent's roots; never `{}`.
#[tokio::test]
async fn a_code_child_with_all_inputs_reads_its_item_or_roots() {
    // A Map worker reading `[messages]` is refused at parse time already
    // (it reads only its item, index and broadcast channels).
    let refused = MAP_CODE.replace("input: [entity, item_index]", "input: [messages]");
    assert!(PipelineDefinition::from_yaml(&refused).is_err());
    for selection in ["[]", "[messages]"] {
        let yaml = MAP_CODE.replace(
            "input: [entity, item_index]",
            &format!(
                "input: {}",
                if selection == "[]" {
                    "[]"
                } else {
                    "[entity, item_index]"
                }
            ),
        );
        let definition = PipelineDefinition::from_yaml(&yaml).expect("the Map is admitted");
        let authority = Arc::new(MemoryAuthority::default());
        let (checkpointer, runtimes) = bound(&authority);
        let code = Arc::new(FakeCode::default());
        let graph = definition
            .compile_subgraph_with_runtime(checkpointer, &with_code(runtimes, &code))
            .expect("the Map compiles");
        let state = graph
            .invoke(
                HashMap::from([("entities".to_owned(), json!(["a", 3]))]),
                ExecutionConfig::new(&format!("root-all-map-{}", selection.len())),
            )
            .await
            .expect("the Map runs");
        assert_eq!(
            state["mapped"],
            json!([
                {"index": 0, "outputs": {"item_result": "\"a\"-0"}},
                {"index": 1, "outputs": {"item_result": "3-1"}},
            ]),
            "Map input {selection}"
        );

        let yaml = PARALLEL_CODE.replace("input: [topic]", &format!("input: {selection}"));
        let definition = PipelineDefinition::from_yaml(&yaml).expect("the Parallel is admitted");
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
                ExecutionConfig::new(&format!("root-all-parallel-{}", selection.len())),
            )
            .await
            .expect("the Parallel runs");
        let joined = serde_json::to_string(&state["joined"]).unwrap();
        assert!(
            joined.contains("t:Python") && joined.contains("t:JavaScript"),
            "Parallel input {selection}: {joined}"
        );
    }
}

/// R4: a Map or Parallel of Code inside a saved child pipeline (compiled
/// below the run root) is refused at compile time with its registry code:
/// fan-out Code below the root is W3-2b. At the root it compiles.
#[test]
fn a_code_fanout_below_the_root_is_refused_at_compile_time() {
    let authority = Arc::new(MemoryAuthority::default());
    let (checkpointer, runtimes) = bound(&authority);
    let code = Arc::new(FakeCode::default());
    let child = with_code(runtimes, &code).with_application_scope_path("parent_agent".to_owned());
    let map = PipelineDefinition::from_yaml(MAP_CODE).unwrap();
    match map.compile_subgraph_with_runtime(checkpointer, &child) {
        Err(error @ PipelineConfigurationError::FanoutChildUnsupported { .. }) => {
            assert_eq!(
                error.code(),
                elitea_faults::codes::GRAPH_FANOUT_CODE_BELOW_ROOT.as_str()
            );
            assert!(error.to_string().contains("shout"), "{error}");
        }
        Err(other) => panic!("expected the below-root refusal, got {other:?}"),
        Ok(_) => panic!("a Map of Code below the root compiled"),
    }
    let parallel_authority: Arc<dyn ParallelCheckpointAuthority> =
        Arc::new(BranchAuthority::default());
    let parallel_checkpointer: Arc<dyn Checkpointer> = parallel_authority.clone();
    let runtimes =
        PipelineNodeRuntimes::default().with_parallel(ParallelCompilerBinding::for_tests(
            parallel_authority,
            tokio::time::Instant::now() + Duration::from_secs(30),
            None,
        ));
    let code = Arc::new(FakeCode::default());
    let child = with_code(runtimes, &code).with_application_scope_path("parent_agent".to_owned());
    let parallel = PipelineDefinition::from_yaml(PARALLEL_CODE).unwrap();
    match parallel.compile_subgraph_with_runtime(parallel_checkpointer, &child) {
        Err(error) => assert_eq!(
            error.code(),
            elitea_faults::codes::GRAPH_FANOUT_CODE_BELOW_ROOT.as_str()
        ),
        Ok(_) => panic!("a Parallel of Code below the root compiled"),
    }
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

/// D1, On Error `continue`: a Code branch that may have started without
/// owner proof fails typed with the registered code, its readable sentence and
/// its support reference; the other branch finishes and lands.
#[tokio::test]
async fn an_unproven_code_branch_fails_typed_and_the_others_finish_under_continue() {
    let yaml = PARALLEL_CODE.replace("wait: all,", "wait: all, on_error: continue,");
    let definition = PipelineDefinition::from_yaml(&yaml).unwrap();
    let authority: Arc<dyn ParallelCheckpointAuthority> = Arc::new(BranchAuthority::default());
    let checkpointer: Arc<dyn Checkpointer> = authority.clone();
    let runtimes =
        PipelineNodeRuntimes::default().with_parallel(ParallelCompilerBinding::for_tests(
            authority,
            tokio::time::Instant::now() + Duration::from_secs(30),
            None,
        ));
    let code = Arc::new(FakeCode {
        effect_unknown_javascript: true,
        ..FakeCode::default()
    });
    let graph = definition
        .compile_subgraph_with_runtime(checkpointer, &with_code(runtimes, &code))
        .unwrap();
    let state = graph
        .invoke(
            HashMap::from([("topic".to_owned(), json!("t"))]),
            ExecutionConfig::new("root-d1-continue"),
        )
        .await
        .expect("On Error continue lands both branches");
    let joined = state["joined"].as_array().unwrap();
    assert_eq!(joined.len(), 2);
    assert_eq!(joined[0]["status"], "succeeded");
    assert_eq!(joined[0]["outputs"]["detail"], "t:Python");
    assert_eq!(joined[1]["status"], "failed");
    assert_eq!(
        joined[1]["error"]["code"],
        crate::agents::graph::node_recovery_runtime::CHILD_EFFECT_UNKNOWN_CODE
    );
    assert_eq!(
        joined[1]["error"]["message_safe"],
        crate::agents::graph::fanout_code_child::CHILD_EFFECT_UNKNOWN_MESSAGE
    );
    assert_eq!(
        joined[1]["error"]["support_ref"].as_str().map(str::len),
        Some(32)
    );
    assert_eq!(*code.attempts.lock().unwrap(), 2);
}

/// D1, On Error `stop`: the run ends typed with the item's code and its
/// key; the items that completed keep their results in their own durable
/// child receipts (a recovery replays them, never re-runs them).
#[tokio::test]
async fn an_unproven_code_item_stops_the_map_typed_and_keeps_completed_items() {
    let yaml = MAP_CODE.replace("max_concurrency: 4", "max_concurrency: 1");
    let definition = PipelineDefinition::from_yaml(&yaml).unwrap();
    let authority = Arc::new(MemoryAuthority::default());
    let (checkpointer, runtimes) = bound(&authority);
    let code = Arc::new(FakeCode {
        effect_unknown_items: vec![2],
        ..FakeCode::default()
    });
    let graph = definition
        .compile_subgraph_with_runtime(checkpointer, &with_code(runtimes, &code))
        .unwrap();
    let error = graph
        .invoke(
            HashMap::from([("entities".to_owned(), json!(["a", "b", "c", "d"]))]),
            ExecutionConfig::new("root-d1-stop"),
        )
        .await
        .expect_err("On Error stop ends the Map");
    let message = error.to_string();
    assert!(
        message.contains("item '2'")
            && message
                .contains(crate::agents::graph::node_recovery_runtime::CHILD_EFFECT_UNKNOWN_CODE),
        "{message}"
    );
    // Items 0 and 1 ran before item 2 (concurrency 1) and completed.
    assert_eq!(code.activations.lock().unwrap().len(), 2);
    let stores = authority.children.stores.lock().await;
    let mut kept = 0;
    for (thread, store) in stores.iter() {
        if let Some(latest) = store.load(thread).await.unwrap()
            && latest
                .state
                .get("item_result")
                .and_then(Value::as_str)
                .is_some_and(|value| !value.is_empty())
        {
            kept += 1;
        }
    }
    assert_eq!(kept, 2, "both completed items keep their durable results");
}

/// D3: a Map item reaches Code only through the Code input byte bound
/// (512 KiB of input JSON): an item whose input is exactly at the bound runs,
/// one byte more fails that item typed before any Code attempt.
#[tokio::test]
async fn a_map_item_reaches_code_only_within_the_code_input_byte_bound() {
    // `{"entity":"<s>","item_index":0}` is 28 bytes plus the string.
    const LIMIT: usize = 512 * 1024;
    for (extra, admitted) in [(0, true), (1, false)] {
        let item = "x".repeat(LIMIT - 28 + extra);
        let definition = PipelineDefinition::from_yaml(MAP_CODE).unwrap();
        let authority = Arc::new(MemoryAuthority::default());
        let (checkpointer, runtimes) = bound(&authority);
        let code = Arc::new(FakeCode::default());
        let graph = definition
            .compile_subgraph_with_runtime(checkpointer, &with_code(runtimes, &code))
            .unwrap();
        let result = graph
            .invoke(
                HashMap::from([("entities".to_owned(), json!([item]))]),
                ExecutionConfig::new("root-code-bytes"),
            )
            .await;
        assert_eq!(result.is_ok(), admitted, "extra {extra}: {result:?}");
        assert_eq!(
            *code.attempts.lock().unwrap(),
            usize::from(admitted),
            "an item over the bound never reaches Code"
        );
    }
}

const LOG_CHILD_ENV: &str = "ELITEA_CODE_FANOUT_LOG_CHILD";

/// D3 log capture: a 64-item Code Map whose item values carry marker strings
/// (and whose index channel is named as one) logs neither, nor any item
/// value or input key, through ordinary failures and a D1 stop. The D1 stop
/// is one WARN fault line with its registered sentence and support reference.
#[test]
fn a_64_item_code_map_logs_no_item_values_or_index() {
    let output = elitea_faults::logtest::run_child(
        "agents::graph::map_fanout_tests::code_child_tests::code_map_log_child",
        LOG_CHILD_ENV,
    );
    for marker in ["secret_item_marker", "secret_index_marker"] {
        assert!(
            !output.stdout.contains(marker) && !output.stderr.contains(marker),
            "{marker} reached the log"
        );
    }
    let lines = output.operator_lines();
    let code = elitea_faults::codes::PIPELINE_NODE_RECOVERY_CHILD_EFFECT_UNKNOWN;
    let unknown: Vec<_> = lines
        .iter()
        .filter(|line| line.get("code").is_some_and(|value| value == code.as_str()))
        .collect();
    assert_eq!(unknown.len(), 1, "{}", output.stdout);
    let line = unknown[0];
    assert_eq!(line["level"], "WARN");
    assert_eq!(line["msg"], code.sentence());
    assert_eq!(line["event"], "code_child_effect_unknown");
    assert_eq!(line["node_id"], "shout");
    assert!(
        line["support_ref"]
            .as_str()
            .is_some_and(|value| value.len() == 32 && value.bytes().all(|b| b.is_ascii_hexdigit()))
    );
    assert!(
        lines.iter().any(|line| line
            .get("error_code")
            .is_some_and(|value| value == "pipeline.code_failed")),
        "the ordinary failures were logged: {}",
        output.stdout
    );
    // B2: each failed child writes one operator line naming its own code,
    // fixed sentence, safe cause chain, ids and support references.
    let child = elitea_faults::codes::GRAPH_FANOUT_CHILD_FAILED;
    let failed: Vec<_> = lines
        .iter()
        .filter(|line| {
            line.get("code")
                .is_some_and(|value| value == child.as_str())
        })
        .collect();
    assert_eq!(
        failed.len(),
        2,
        "one line per failed child: {}",
        output.stdout
    );
    for line in &failed {
        assert_eq!(line["level"], "WARN");
        assert_eq!(line["msg"], child.sentence());
        assert_eq!(line["event"], "fanout_child_failed");
        assert_eq!(line["node_id"], "map_entities");
        assert_eq!(line["item"], "0");
        assert!(
            line["member_sentence"]
                .as_str()
                .is_some_and(|text| !text.is_empty())
        );
        let execution = line["execution_id"].as_str().unwrap();
        assert_ne!(execution, "");
        assert_eq!(line["support_ref"], execution);
        assert!(
            line["item_ref"].as_str().is_some_and(
                |value| value.len() == 32 && value.bytes().all(|b| b.is_ascii_hexdigit())
            )
        );
        let cause = line["cause"].as_array().expect("cause is a JSON array");
        assert!(
            !cause.is_empty() && cause.len() <= elitea_faults::MAX_CAUSES,
            "{cause:?}"
        );
    }
    let codes: Vec<_> = failed
        .iter()
        .map(|line| line["member_code"].clone())
        .collect();
    assert!(
        codes.contains(&json!(
            crate::agents::graph::node_recovery_runtime::CHILD_EFFECT_UNKNOWN_CODE
        )),
        "{codes:?}"
    );
}

#[test]
fn code_map_log_child() {
    use tracing_subscriber::layer::SubscriberExt as _;
    use tracing_subscriber::util::SubscriberInitExt as _;
    if !elitea_faults::logtest::is_child(LOG_CHILD_ENV) {
        return;
    }
    tracing_subscriber::registry()
        .with(
            crate::diagnostics::log_layer(Some("debug"), std::io::stdout).expect("operator stream"),
        )
        .init();
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    runtime.block_on(async {
        let yaml = MAP_CODE
            .replace("index: item_index", "index: secret_index_marker")
            .replace(
                "input: [entity, item_index]",
                "input: [entity, secret_index_marker]",
            );
        let definition = PipelineDefinition::from_yaml(&yaml).unwrap();
        // 64 items that all run; then an ordinary failure; then a D1 stop.
        for (items, fake) in [
            (64, FakeCode::default()),
            (
                4,
                FakeCode {
                    fail_items: vec![0],
                    ..FakeCode::default()
                },
            ),
            (
                4,
                FakeCode {
                    effect_unknown_items: vec![0],
                    ..FakeCode::default()
                },
            ),
        ] {
            let authority = Arc::new(MemoryAuthority::default());
            let (checkpointer, runtimes) = bound(&authority);
            let code = Arc::new(fake);
            let graph = definition
                .compile_subgraph_with_runtime(checkpointer, &with_code(runtimes, &code))
                .unwrap();
            let values = (0..items)
                .map(|index| {
                    json!({
                        "name": format!("secret_item_marker_{index}"),
                        "nested": [format!("secret_item_marker_nested_{index}")],
                    })
                })
                .collect::<Vec<_>>();
            let result = graph
                .invoke(
                    HashMap::from([("entities".to_owned(), Value::Array(values))]),
                    ExecutionConfig::new("root-code-logs"),
                )
                .await;
            if items == 64 {
                assert!(result.is_ok(), "{result:?}");
                assert_eq!(*code.attempts.lock().unwrap(), 64, "every item ran");
            }
        }
    });
    elitea_faults::logtest::child_ran();
}

/// A Code child's visit is a pure function of its branch, node and frozen
/// input, whatever order the input map iterates in: a restored item derives
/// the same visit (and the same journal) in another process.
#[test]
fn a_code_child_visit_is_canonical_over_its_input_order() {
    let forward = State::from([
        ("a".to_owned(), json!(1)),
        ("b".to_owned(), json!({"y": 2, "x": 1})),
    ]);
    let visits = (0..32)
        .map(|_| {
            let mut input = State::new();
            for (key, value) in &forward {
                input.insert(key.clone(), value.clone());
            }
            crate::agents::graph::fanout_code_child::code_child_visit("3", "code", &input).unwrap()
        })
        .collect::<std::collections::BTreeSet<_>>();
    assert_eq!(visits.len(), 1);
}

/// W3-2b: the ETL `silver_partition` shape, a Map whose worker is an Agent
/// node running a saved pipeline `validate (Code) → route_quality (Router) →
/// normalize (Code)`. It compiles (the `ContainsCode` refusal is gone; a
/// Router inside the saved child is admitted), and the activation is admitted
/// against each item's Code time: Q2, its two Code nodes each once, though
/// the Router can re-enter `validate` (B6 guards that per attempt).
const SAVED_CODE_MAP: &str = "entry_point: map_partitions\nstate:\n  partitions: {type: list, value: []}\n  item_result: {type: str, value: ''}\n  mapped: {type: list, value: []}\nnodes:\n  - {id: map_partitions, type: map, worker: silver, source: partitions, item: entity, index: item_index, outputs: [item_result], destination: mapped, max_items: 8, max_concurrency: 2, reduction: ordered_collection, transition: END}\n  - {id: silver, type: agent, tool: silver_partition, input: [entity, item_index], input_mapping: {task: {type: fixed, value: partition}}, output: [item_result]}\n";
const SILVER_PARTITION: &str = "entry_point: validate\nstate:\n  messages: list\n  quality: str\nnodes:\n  - id: validate\n    type: code\n    code: 'print(1)'\n    input: [messages]\n    output: [quality]\n    transition: route_quality\n  - id: route_quality\n    type: router\n    condition: '{{ quality }}'\n    routes: [normalize, validate]\n    default_output: END\n    input: [quality]\n  - id: normalize\n    type: code\n    language: javascript\n    code: 'b'\n    input: [quality]\n    output: [quality]\n    transition: END\n";

#[tokio::test]
async fn a_saved_pipeline_with_code_and_a_router_runs_as_a_map_worker_admitted_per_code_node() {
    let child = PipelineDefinition::from_yaml(SILVER_PARTITION).expect("the saved child");
    assert_eq!(child.code_node_ids().len(), 2);
    let definition = PipelineDefinition::from_yaml(SAVED_CODE_MAP).unwrap();
    let authority: Arc<dyn ParallelCheckpointAuthority> = Arc::new(MemoryAuthority::default());
    let checkpointer: Arc<dyn Checkpointer> = authority.clone();
    let runtimes = PipelineNodeRuntimes::new(None, None, Some(Arc::new(super::ProvedResolver)))
        .with_parallel(ParallelCompilerBinding::for_tests(
            authority,
            tokio::time::Instant::now() + Duration::from_secs(30),
            None,
        ))
        .with_composition_definitions(std::collections::BTreeMap::from([(
            "silver".to_owned(),
            child,
        )]));
    let code = Arc::new(FakeCode {
        refuse_fanout: true,
        ..FakeCode::default()
    });
    let graph = definition
        .compile_subgraph_with_runtime(checkpointer, &with_code(runtimes, &code))
        .expect("a saved pipeline with Code compiles as a Map worker");
    let error = graph
        .invoke(
            HashMap::from([("partitions".to_owned(), json!(["p1", "p2", "p3"]))]),
            ExecutionConfig::new("root-saved-code-map"),
        )
        .await
        .expect_err("the refused activation ends the run");
    assert!(
        error
            .to_string()
            .contains(crate::protocol::output::FANOUT_DEADLINE_INSUFFICIENT_UPSTREAM_CODE),
        "{error}"
    );
    assert_eq!(
        *code.admissions.lock().unwrap(),
        vec![(vec![2, 2, 2], 2)],
        "each item counts its saved pipeline's two Code nodes once"
    );
    assert_eq!(*code.attempts.lock().unwrap(), 0, "no item started");
}
