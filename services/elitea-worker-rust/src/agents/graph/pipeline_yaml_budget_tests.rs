use std::time::{Duration, Instant};

use super::compiler::{MAX_PIPELINE_YAML_BYTES, PIPELINE_YAML_BUDGET, PipelineDefinition};

const EXPANSION: &str = "graph.pipeline.yaml_expansion_exceeded";

const PIPELINE: &str = "state: {count: int}\nentry_point: tick\nnodes:\n  - id: tick\n    type: state_modifier\n    template: '1'\n    output: [count]\n    transition: END\n";

/// A valid pipeline followed by one anchored value that is referenced `references` times.
fn pipeline_with_shared_value(shared: &str, references: usize) -> String {
    let mut yaml = format!("{PIPELINE}shared: &s {shared}\nrepeated: [");
    for _ in 0..references {
        yaml.push_str("*s,");
    }
    yaml.push_str("]\n");
    yaml
}

fn shared_list(items: usize) -> String {
    format!("[{}]", "v,".repeat(items))
}

fn pipeline_error_code(yaml: &str) -> &'static str {
    PipelineDefinition::from_yaml(yaml)
        .err()
        .expect("pipeline admission must fail")
        .code()
}

#[test]
fn pipeline_reference_expansion_beyond_node_budget_is_resource_exhausted() {
    let yaml = pipeline_with_shared_value(&shared_list(1_000), 200);
    assert_eq!(pipeline_error_code(&yaml), EXPANSION);
}

#[test]
fn pipeline_nesting_beyond_depth_budget_is_resource_exhausted() {
    let depth = PIPELINE_YAML_BUDGET.depth + 1;
    let yaml = format!(
        "{PIPELINE}nested: {}{}\n",
        "[".repeat(depth),
        "]".repeat(depth)
    );
    assert_eq!(pipeline_error_code(&yaml), EXPANSION);
}

#[test]
fn pipeline_scalar_bytes_beyond_budget_are_resource_exhausted() {
    let yaml = pipeline_with_shared_value(&"a".repeat(256 * 1024), 5);
    assert!(yaml.len() < MAX_PIPELINE_YAML_BYTES);
    assert_eq!(pipeline_error_code(&yaml), EXPANSION);
}

#[test]
fn pipeline_reference_expansion_refusal_is_fast_and_identical_on_replay() {
    let yaml = pipeline_with_shared_value(&shared_list(50_000), 60_000);
    assert!(yaml.len() < MAX_PIPELINE_YAML_BYTES);
    let started = Instant::now();
    let first = pipeline_error_code(&yaml);
    let second = pipeline_error_code(&yaml);
    let elapsed = started.elapsed();
    assert_eq!(first, EXPANSION);
    assert_eq!(first, second);
    // Debug builds are much slower than release; release timing is in the source mapping.
    assert!(
        elapsed < Duration::from_secs(3),
        "two refusals took {elapsed:?}"
    );
}

#[test]
fn pipeline_references_within_budget_still_compile() {
    let yaml = "state: {count: int}\nentry_point: first\nnodes:\n  - id: first\n    type: state_modifier\n    template: '1'\n    output: &keys [count]\n    transition: second\n  - id: second\n    type: state_modifier\n    template: '2'\n    output: *keys\n    transition: END\n";
    assert!(PipelineDefinition::from_yaml(yaml).is_ok());
}
