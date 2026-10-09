//! Exclusive pipeline branches that meet at one node must not become ADK
//! wait-for-all joins. A stored pipeline advances one node at a time, so only
//! one of several transitions into a node can ever arrive in a pass.

use std::collections::HashMap;
use std::sync::Arc;

use adk_rust::graph::{ExecutionConfig, MemoryCheckpointer, State};
use serde_json::json;

use super::compiler::{PipelineDefinition, PipelineNodeRuntimes};

/// Two routed branches each finish the pipeline: in a saved child both
/// transitions are redirected to the one compiler-owned result node.
const TWO_TERMINAL_BRANCHES: &str = r#"
state:
  input: {type: str}
  messages: {type: list}
  choice: {type: str}
  final_text: {type: str, value: ""}
entry_point: pick
nodes:
  - id: pick
    type: router
    condition: "{{ choice }}"
    routes: [left, right]
    default_output: left
    input: [choice]
  - id: left
    type: state_modifier
    template: "left {{ input }}"
    input: [input]
    output: [final_text]
    transition: END
  - id: right
    type: state_modifier
    template: "right {{ input }}"
    input: [input]
    output: [final_text]
    transition: END
"#;

/// Two routed branches converge on one shared node through direct transitions.
const SHARED_NEXT_NODE: &str = r#"
state:
  input: {type: str}
  messages: {type: list}
  choice: {type: str}
  picked: {type: str, value: ""}
  final_text: {type: str, value: ""}
entry_point: pick
nodes:
  - id: pick
    type: router
    condition: "{{ choice }}"
    routes: [left, right]
    default_output: left
    input: [choice]
  - id: left
    type: state_modifier
    template: "left"
    output: [picked]
    transition: finish
  - id: right
    type: state_modifier
    template: "right"
    output: [picked]
    transition: finish
  - id: finish
    type: state_modifier
    template: "{{ picked }} {{ input }}"
    input: [picked, input]
    output: [final_text]
    transition: END
"#;

/// A loop transitions back to the entry node, which a saved child also
/// reaches from its compiler-owned entry node.
const LOOP_TO_ENTRY: &str = r#"
state:
  input: {type: str}
  messages: {type: list}
  visited: {type: str, value: ""}
  final_text: {type: str, value: ""}
entry_point: gate
nodes:
  - id: gate
    type: router
    condition: "{% if visited %}finish{% else %}mark{% endif %}"
    routes: [mark, finish]
    default_output: finish
    input: [visited]
  - id: mark
    type: state_modifier
    template: "once"
    output: [visited]
    transition: gate
  - id: finish
    type: state_modifier
    template: "{{ visited }} {{ input }}"
    input: [visited, input]
    output: [final_text]
    transition: END
"#;

fn input_state(input: &str, choice: Option<&str>) -> State {
    let mut state = HashMap::from([
        ("input".to_owned(), json!(input)),
        ("messages".to_owned(), json!([])),
    ]);
    if let Some(choice) = choice {
        state.insert("choice".to_owned(), json!(choice));
    }
    state
}

async fn run_saved_child(yaml: &str, thread: &str, input: State) -> State {
    let graph = PipelineDefinition::from_yaml(yaml)
        .expect("saved child pipeline")
        .compile_subgraph_with_runtime(
            Arc::new(MemoryCheckpointer::new()),
            &PipelineNodeRuntimes::default(),
        )
        .expect("saved child graph");
    graph
        .invoke(input, ExecutionConfig::new(thread))
        .await
        .expect("saved child run")
}

async fn run_root(yaml: &str, thread: &str, input: State) -> State {
    let graph = PipelineDefinition::from_yaml(yaml)
        .expect("root pipeline")
        .compile("pipeline-root", Arc::new(MemoryCheckpointer::new()), None)
        .expect("root graph");
    graph
        .invoke(input, ExecutionConfig::new(thread))
        .await
        .expect("root run")
}

#[tokio::test]
async fn saved_child_with_two_terminal_branches_publishes_the_taken_branch_result() {
    for choice in ["left", "right"] {
        let state = run_saved_child(
            TWO_TERMINAL_BRANCHES,
            &format!("child-terminal-{choice}"),
            input_state("world", Some(choice)),
        )
        .await;
        assert_eq!(
            state.get("final_text"),
            Some(&json!(format!("{choice} world")))
        );
        assert_eq!(
            state.get("elitea_response"),
            Some(&json!(format!("{choice} world"))),
            "the saved child result node must run after the {choice} branch"
        );
    }
}

#[tokio::test]
async fn saved_child_branches_converging_on_a_shared_node_run_the_shared_node() {
    for choice in ["left", "right"] {
        let state = run_saved_child(
            SHARED_NEXT_NODE,
            &format!("child-shared-{choice}"),
            input_state("world", Some(choice)),
        )
        .await;
        assert_eq!(
            state.get("final_text"),
            Some(&json!(format!("{choice} world")))
        );
        assert_eq!(
            state.get("elitea_response"),
            Some(&json!(format!("{choice} world")))
        );
    }
}

#[tokio::test]
async fn saved_child_loop_back_to_its_entry_node_continues() {
    let state = run_saved_child(LOOP_TO_ENTRY, "child-loop", input_state("world", None)).await;
    assert_eq!(state.get("final_text"), Some(&json!("once world")));
    assert_eq!(state.get("elitea_response"), Some(&json!("once world")));
}

#[tokio::test]
async fn root_branches_converging_on_a_shared_node_run_the_shared_node() {
    for choice in ["left", "right"] {
        let state = run_root(
            SHARED_NEXT_NODE,
            &format!("root-shared-{choice}"),
            input_state("world", Some(choice)),
        )
        .await;
        assert_eq!(
            state.get("final_text"),
            Some(&json!(format!("{choice} world")))
        );
    }
}

#[tokio::test]
async fn root_loop_back_to_its_entry_node_continues() {
    let state = run_root(LOOP_TO_ENTRY, "root-loop", input_state("world", None)).await;
    assert_eq!(state.get("final_text"), Some(&json!("once world")));
}

#[tokio::test]
async fn root_with_two_terminal_branches_keeps_the_taken_branch_result() {
    for choice in ["left", "right"] {
        let state = run_root(
            TWO_TERMINAL_BRANCHES,
            &format!("root-terminal-{choice}"),
            input_state("world", Some(choice)),
        )
        .await;
        assert_eq!(
            state.get("final_text"),
            Some(&json!(format!("{choice} world")))
        );
    }
}
