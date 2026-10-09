use super::*;
use crate::agents::graph::application::{
    ApplicationExecutionError, PipelineApplicationSelection, ResolvedApplicationParticipant,
};
use crate::agents::graph::parallel::{
    PARALLEL_RESUME_STATE_KEY, ParallelActivation, ParallelChildCheckpoint,
    ParallelChildCheckpointerFactory,
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
    assert!(
        definition
            .runtime_channels()
            .contains(PARALLEL_RESUME_STATE_KEY)
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
        PipelineDefinition::from_yaml(&YAML.replace("topic", "__elitea_parallel_resume_v1"))
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

struct NoContinuation;
#[async_trait]
impl ParallelBranchContinuation for NoContinuation {
    fn cards(
        &self,
        _: &ApplicationNodeDefinition,
        _: &ParallelBranchPause,
    ) -> Result<Vec<ParallelPauseCard>, GraphError> {
        Err(GraphError::InvalidGraph("unproved".into()))
    }
    async fn input(
        &self,
        _: &ApplicationNodeDefinition,
        _: &ParallelBranchPause,
        _: &[ParallelDecision],
    ) -> Result<State, GraphError> {
        Err(GraphError::InvalidGraph("unproved".into()))
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
            continuation: Arc::new(NoContinuation),
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
    let binding = ParallelCompilerBinding::for_tests(
        authority,
        Arc::new(NoContinuation),
        tokio::time::Instant::now(),
    );
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
fn production_binding_remains_gated_before_composed_restart_acceptance() {
    let authority: Arc<dyn ParallelCheckpointAuthority> = Arc::new(TestAuthority::default());
    assert!(
        ParallelCompilerBinding::new(
            authority,
            Arc::new(NoContinuation),
            tokio::time::Instant::now()
        )
        .is_err()
    );
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
