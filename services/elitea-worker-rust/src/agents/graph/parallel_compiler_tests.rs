use super::*;
use crate::agents::graph::application::{
    ApplicationExecutionError, PipelineApplicationSelection, ResolvedApplicationParticipant,
};
use crate::agents::graph::parallel::{
    ParallelActivation, ParallelChildCheckpoint, ParallelChildCheckpointerFactory,
};
use adk_rust::graph::{Checkpoint, MemoryCheckpointer};
use serde_json::json;

const YAML: &str = r"
state:
  topic: string
  detail: string
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
    input: [topic]
    input_mapping:
      task: {type: fstring, value: 'Research {topic}'}
      copied: {type: variable, value: topic}
      constant: {type: fixed, value: {unknown: [1, two]}}
      rendered: {type: fstring, value: 'For {topic}'}
    output: [detail]
  - id: workerB
    type: agent
    tool: Research Agent
    input: [topic]
    input_mapping:
      task: {type: fixed, value: Compare sources}
    output: [detail]
";

#[test]
fn fixed_parallel_owns_distinct_declared_agents_and_excludes_parent_frontiers() {
    let definition = PipelineDefinition::from_yaml(YAML).unwrap();
    assert_eq!(
        definition.parallel_owned_nodes,
        BTreeSet::from(["workerA".into(), "workerB".into()])
    );
    assert!(definition.recovery_frontier_supported(&["gather".into()]));
    assert!(!definition.recovery_frontier_supported(&["workerA".into()]));
    // Decisions arrive per interrupt from Main, never through parent state.
    assert!(
        !definition
            .runtime_channels()
            .contains("__elitea_parallel_resume_v1")
    );
}

#[test]
fn owned_agents_refuse_parent_routes_static_interrupts_entry_and_sharing() {
    let cases = [
        YAML.replace("entry_point: gather", "entry_point: workerA"),
        YAML.replace(
            "entry_point: gather",
            "interrupt_before: [workerA]\nentry_point: gather",
        ),
        YAML.replace(
            "entry_point: gather",
            "interrupt_after: [workerB]\nentry_point: gather",
        ),
        YAML.replace("transition: END", "transition: workerA"),
        YAML.replace(
            "    output: [detail]\n  - id: workerB",
            "    output: [detail]\n    transition: END\n  - id: workerB",
        ),
        YAML.replace("{id: right, node: workerB}", "{id: right, node: workerA}"),
        YAML.replace("node: workerB", "node: missing"),
        YAML.replace(
            "  - id: workerB\n    type: agent",
            "  - id: workerB\n    type: parallel",
        ),
    ];
    for yaml in cases {
        assert!(PipelineDefinition::from_yaml(&yaml).is_err());
    }
}

#[test]
fn whole_pipeline_admission_enforces_fixed_bounds_and_typed_list_output() {
    assert!(
        PipelineDefinition::from_yaml(&YAML.replace("max_concurrency: 2", "max_concurrency: 9"))
            .is_err()
    );
    assert!(PipelineDefinition::from_yaml(&YAML.replace("joined: list", "joined: dict")).is_err());
    assert!(PipelineDefinition::from_yaml(&YAML.replace("  joined: list\n", "")).is_err());
    assert!(PipelineDefinition::from_yaml(&YAML.replace("joined", "messages")).is_err());
    assert!(
        PipelineDefinition::from_yaml(&YAML.replace("topic", "__elitea_parallel_agent_inputs_v1"))
            .is_err()
    );
    let branches = (0..17)
        .map(|index| format!("{{id: b{index}, node: n{index}}}"))
        .collect::<Vec<_>>()
        .join(", ");
    let yaml = YAML.replace(
        "[{id: left, node: workerA}, {id: right, node: workerB}]",
        &format!("[{branches}]"),
    );
    assert!(PipelineDefinition::from_yaml(&yaml).is_err());
}

/// The join lands on the output key's declared type and reducer; a mismatch
/// is refused when the pipeline is saved, never discovered at run time.
#[test]
fn the_join_mode_is_checked_against_the_output_type_and_reducer_at_save_time() {
    let with = |state: &str, policy: &str| {
        YAML.replace("  joined: list\n", state)
            .replace("    wait: all\n", &format!("    wait: all\n{policy}"))
    };
    let accepted = [
        with("  joined: list\n", ""),
        with("  joined: list\n", "    join: append\n"),
        with("  joined: {type: list, reducer: append}\n", ""),
        with("  joined: dict\n", "    join: keyed\n"),
        with(
            "  joined: {type: dict, reducer: merge}\n",
            "    join: keyed\n",
        ),
        with("  joined: dict\n", "    on_error: stop\n    join: merge\n"),
        with("  joined: list\n", "    error_policy: fail_after_drain\n"),
    ];
    for yaml in &accepted {
        assert!(PipelineDefinition::from_yaml(yaml).is_ok(), "{yaml}");
    }
    let refused = [
        // Append lands a list; keyed and merge land an object.
        with("  joined: dict\n", ""),
        with("  joined: list\n", "    join: keyed\n"),
        // A declared reducer must fit the join.
        with("  joined: {type: list, reducer: sum_int}\n", ""),
        with(
            "  joined: {type: dict, reducer: merge}\n",
            "    on_error: continue\n    join: merge\n",
        ),
        with(
            "  joined: {type: list, reducer: append}\n",
            "    join: keyed\n",
        ),
        // Merge has no place for a failed branch's entry.
        with("  joined: dict\n", "    join: merge\n"),
    ];
    for yaml in &refused {
        assert!(PipelineDefinition::from_yaml(yaml).is_err(), "{yaml}");
    }
}

#[test]
fn fixed_parallel_preserves_legacy_names_and_state_declaration_sequence() {
    let legacy = YAML
        .replace("workerA", "Worker A")
        .replace("gather", "Gather all");
    let canonical = YAML
        .replace("workerA", "WorkerA")
        .replace("gather", "Gatherall");
    assert_eq!(
        PipelineDefinition::from_yaml(&legacy)
            .unwrap()
            .definition_digest(),
        PipelineDefinition::from_yaml(&canonical)
            .unwrap()
            .definition_digest()
    );
    let definition = PipelineDefinition::from_yaml(YAML).unwrap();
    assert_eq!(
        definition.state_declaration_order,
        ["topic", "detail", "joined"]
    );
    let reordered = YAML.replace(
        "  topic: string\n  detail: string",
        "  detail: string\n  topic: string",
    );
    assert_ne!(
        definition.definition_digest(),
        PipelineDefinition::from_yaml(&reordered)
            .unwrap()
            .definition_digest()
    );
}

struct UnavailableResolver;
impl PipelineApplicationResolver for UnavailableResolver {
    fn resolve(
        &self,
        _: &PipelineApplicationSelection,
        _: Arc<dyn Checkpointer>,
    ) -> Result<ResolvedApplicationParticipant, ApplicationExecutionError> {
        Err(ApplicationExecutionError::Unavailable)
    }
}

fn factory() -> (CompilerParallelBranches, ParallelNodeDefinition) {
    let pipeline = PipelineDefinition::from_yaml(YAML).unwrap();
    let parallel = pipeline
        .nodes
        .iter()
        .find_map(|node| {
            if let PipelineNodeDefinition::Parallel(node) = node {
                Some(node.clone())
            } else {
                None
            }
        })
        .unwrap();
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
                .unwrap();
            (branch.id().into(), node)
        })
        .collect();
    (
        CompilerParallelBranches {
            owned,
            state_types: pipeline.state,
            resolver: Arc::new(UnavailableResolver),
            events: None,
        },
        parallel,
    )
}

#[test]
fn agent_input_mapping_is_frozen_and_unmapped_parent_state_is_isolated() {
    let (factory, parallel) = factory();
    let parent = State::from([
        ("topic".into(), json!("rust")),
        ("unmapped".into(), json!({"secret": true})),
    ]);
    let frozen = factory
        .project_input(&parallel.branches()[0], &parent)
        .unwrap();
    assert_eq!(frozen["topic"], json!("rust"));
    assert!(!frozen.contains_key("unmapped"));
    let arguments = &frozen[super::super::super::application::PARALLEL_AGENT_INPUTS_STATE_KEY];
    assert_eq!(arguments["task"], "Research rust");
    assert_eq!(arguments["variables"]["copied"], "rust");
    assert_eq!(arguments["variables"]["rendered"], "For rust");
    assert_eq!(
        arguments["variables"]["constant"],
        json!({"unknown": [1,"two"]})
    );
    assert!(
        factory
            .project_input(&parallel.branches()[0], &State::new())
            .is_err()
    );
}

#[test]
fn keyed_results_preserve_declared_names_and_refuse_missing_or_wrong_typed_outputs() {
    let (factory, parallel) = factory();
    let branch = &parallel.branches()[0];
    let output = State::from([
        ("detail".into(), json!("answer")),
        ("undeclared".into(), json!(7)),
    ]);
    let ParallelBranchTerminal::Completed(result) =
        factory.project_result(branch, &output).unwrap()
    else {
        panic!("completed");
    };
    assert_eq!(
        result,
        serde_json::Map::from_iter([("detail".into(), json!("answer"))])
    );
    assert!(factory.project_result(branch, &State::new()).is_err());
    assert!(
        factory
            .project_result(branch, &State::from([("detail".into(), json!(12))]))
            .is_err()
    );
    assert!(matches!(
        factory
            .project_result(
                branch,
                &State::from([("_pipeline_blocked".into(), json!(true))])
            )
            .unwrap(),
        ParallelBranchTerminal::Blocked
    ));
}

#[derive(Default)]
struct TestAuthority(MemoryCheckpointer);
#[async_trait]
impl Checkpointer for TestAuthority {
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
impl crate::agents::graph::ParallelCheckpointAppender for TestAuthority {
    async fn append_after(
        &self,
        _: Option<&Checkpoint>,
        _: &Checkpoint,
    ) -> Result<String, GraphError> {
        Err(GraphError::CheckpointError("test does not append".into()))
    }
}
#[async_trait]
impl ParallelChildCheckpointerFactory for TestAuthority {
    fn child_origin(
        &self,
        _: &ParallelActivation,
    ) -> Result<crate::agents::graph::ParallelChildOrigin, GraphError> {
        Err(GraphError::CheckpointError("test does not mint".into()))
    }

    fn branch_thread_id(
        &self,
        _: &ParallelActivation,
        _: &crate::agents::graph::ParallelBranchDefinition,
        _: usize,
        _: &[u8; 32],
        _: &crate::agents::graph::ParallelChildOrigin,
    ) -> Result<String, GraphError> {
        Err(GraphError::CheckpointError("test does not mint".into()))
    }

    async fn for_branch(
        &self,
        _: &ParallelActivation,
        _: &crate::agents::graph::ParallelBranchDefinition,
        _: usize,
        _: &[u8; 32],
        _: &crate::agents::graph::ParallelChildOrigin,
    ) -> Result<ParallelChildCheckpoint, GraphError> {
        Err(GraphError::CheckpointError("test does not mint".into()))
    }
}
impl ParallelCheckpointAuthority for TestAuthority {}

#[test]
fn compiler_refuses_missing_or_different_underlying_parent_authority() {
    let definition = PipelineDefinition::from_yaml(YAML).unwrap();
    let authority: Arc<dyn ParallelCheckpointAuthority> = Arc::new(TestAuthority::default());
    let same: Arc<dyn Checkpointer> = authority.clone();
    assert!(
        definition
            .bind_parallel_authority(same.clone(), &PipelineNodeRuntimes::default())
            .is_err()
    );
    let binding = ParallelCompilerBinding::for_tests(authority, tokio::time::Instant::now(), None);
    let runtimes = PipelineNodeRuntimes::default().with_parallel(binding);
    assert!(
        definition
            .bind_parallel_authority(Arc::new(MemoryCheckpointer::new()), &runtimes)
            .is_err()
    );
    let (_, bound) = definition.bind_parallel_authority(same, &runtimes).unwrap();
    assert!(bound.parallel_authority_bound);
}

#[test]
fn parallel_binds_through_the_fanout_turn_authority() {
    // The turn boundary must not erase the typed authority: the root graph's
    // checkpointer and the binding's parent are the same Arc (finding 2).
    let definition = PipelineDefinition::from_yaml(YAML).unwrap();
    let inner: Arc<dyn ParallelCheckpointAuthority> = Arc::new(TestAuthority::default());
    let authority: Arc<dyn ParallelCheckpointAuthority> = Arc::new(
        crate::agents::graph::fanout_turn::FanoutTurnAuthority::new(
            inner,
            "root-thread",
            "execution-1",
            1,
            true,
        )
        .unwrap(),
    );
    let root: Arc<dyn Checkpointer> = authority.clone();
    let runtimes = PipelineNodeRuntimes::default().with_parallel(
        ParallelCompilerBinding::for_tests(authority, tokio::time::Instant::now(), None),
    );
    let (_, bound) = definition.bind_parallel_authority(root, &runtimes).unwrap();
    assert!(bound.parallel_authority_bound);
}

#[test]
fn the_production_binding_carries_the_claims_decision_ledger() {
    let authority: Arc<dyn ParallelCheckpointAuthority> = Arc::new(TestAuthority::default());
    let binding = ParallelCompilerBinding::new(
        authority,
        tokio::time::Instant::now(),
        Arc::new(elitea_agent_runtime::graph::fanout::runner::NoDecisions),
    );
    assert!(binding.decisions.is_some());
}

#[test]
fn frozen_agent_mapping_and_keyed_projection_reject_deep_values_before_cloning() {
    let (factory, parallel) = factory();
    let mut deep = serde_json::Value::Null;
    for _ in 0..256 {
        deep = serde_json::Value::Array(vec![deep]);
    }
    let parent = State::from([
        ("topic".into(), json!("valid task source")),
        ("unmapped".into(), deep.clone()),
    ]);
    assert!(
        factory
            .project_input(&parallel.branches()[0], &parent)
            .is_err()
    );
    assert!(
        factory
            .project_result(
                &parallel.branches()[0],
                &State::from([("detail".into(), deep)])
            )
            .is_err()
    );
}

/// A branch that names a node kind not admitted as a fan-out child yet is
/// refused by name with a typed code, not a generic string.
#[test]
fn a_branch_of_an_unadmitted_kind_is_refused_typed_by_name() {
    let yaml = YAML.replace(
        "  - id: workerB\n    type: agent\n    tool: Research Agent\n    input: [topic]\n    input_mapping:\n      task: {type: fixed, value: Compare sources}\n    output: [detail]\n",
        "  - id: workerB\n    type: state_modifier\n    input: [topic]\n    output: [detail]\n    template: '{{ topic }}'\n",
    );
    assert_ne!(yaml, YAML, "fixture replacement applied");
    match PipelineDefinition::from_yaml(&yaml) {
        Err(error @ PipelineConfigurationError::FanoutChildUnsupported { .. }) => {
            assert_eq!(error.code(), "graph.fanout.child_kind_unsupported");
            assert!(error.to_string().contains("workerB"), "{error}");
        }
        Err(other) => panic!("expected a typed child-kind refusal, got {other:?}"),
        Ok(_) => panic!("an unadmitted child kind compiled"),
    }
}

/// An inline LLM node is not a Parallel branch yet (FU-247): refused typed
/// by name; run it from a saved pipeline through an Agent node instead.
#[test]
fn an_inline_llm_branch_is_refused_typed_by_name() {
    let yaml = YAML.replace(
        "  - id: workerB\n    type: agent\n    tool: Research Agent\n",
        "  - id: workerB\n    type: llm\n",
    );
    assert_ne!(yaml, YAML, "fixture replacement applied");
    match PipelineDefinition::from_yaml(&yaml) {
        Err(error @ PipelineConfigurationError::FanoutChildUnsupported { .. }) => {
            assert_eq!(error.code(), "graph.fanout.child_kind_unsupported");
            assert!(error.to_string().contains("workerB"), "{error}");
        }
        Err(other) => panic!("expected a typed child-kind refusal, got {other:?}"),
        Ok(_) => panic!("an inline LLM branch compiled"),
    }
}

/// A branch whose saved pipeline contains a Code node is refused by name
/// until Wave 3 (Map refuses the same through its effect-free cohort).
#[test]
fn a_branch_whose_saved_pipeline_contains_code_is_refused_typed() {
    let definition = PipelineDefinition::from_yaml(YAML).unwrap();
    let child = PipelineDefinition::from_yaml(
        "entry_point: execute\nstate:\n  messages: list\nnodes:\n  - id: execute\n    type: code\n    code: {type: fixed, value: 'result = 1'}\n    input: [messages]\n    output: [messages]\n    structured_output: false\n    transition: END\n",
    )
    .expect("saved child pipeline with Code");
    assert!(child.has_code_nodes());
    let authority: Arc<dyn ParallelCheckpointAuthority> = Arc::new(TestAuthority::default());
    let checkpointer: Arc<dyn Checkpointer> = authority.clone();
    let runtimes = PipelineNodeRuntimes::new(None, None, Some(Arc::new(UnavailableResolver)))
        .with_parallel(ParallelCompilerBinding::for_tests(
            authority,
            tokio::time::Instant::now() + std::time::Duration::from_secs(10),
            None,
        ))
        .with_composition_definitions(std::collections::BTreeMap::from([(
            "workerB".to_owned(),
            child,
        )]));
    match definition.compile_subgraph_with_runtime(checkpointer, &runtimes) {
        Err(error @ PipelineConfigurationError::FanoutChildUnsupported { .. }) => {
            assert_eq!(error.code(), "graph.fanout.child_contains_code");
            assert!(error.to_string().contains("workerB"), "{error}");
        }
        Err(other) => panic!("expected a typed Code refusal, got {other:?}"),
        Ok(_) => panic!("a branch with Code compiled"),
    }
}
