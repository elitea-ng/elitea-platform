//! Compiler admission, state validation and graph execution of data shaping nodes.

use super::compiler::shaping_node_admission;

const SPLIT_NODE: &str = "id: split\ntype: split_out\nsource: orders\noutput: [lines]\nsplit: {mode: rows_field, path: /items}\ndestination: items\nretain: {mode: all}\ntransition: END\n";
const JOIN_NODE: &str = "id: join\ntype: aggregate\nsource: lines\noutput: [orders_out]\nlayout: split_out\nregroup: parent\noperations: [{operation: collect, field: {path: /items}, output: items}]\ntransition: END\n";

fn yaml(text: &str) -> serde_yaml_ng::Value {
    serde_yaml_ng::from_str(text).expect("fixture node YAML")
}

#[test]
fn shaping_nodes_are_refused_as_not_enabled_while_the_gate_is_off() {
    assert_eq!(
        shaping_node_admission(&yaml(SPLIT_NODE), false),
        Err("graph.pipeline.unsupported_capability")
    );
    assert_eq!(shaping_node_admission(&yaml(SPLIT_NODE), true), Ok(()));
    assert_eq!(
        shaping_node_admission(&yaml(JOIN_NODE), false),
        Err("graph.pipeline.unsupported_capability")
    );
    assert_eq!(shaping_node_admission(&yaml(JOIN_NODE), true), Ok(()));
}

#[cfg(not(feature = "graph-extensions-rehearsal"))]
#[test]
fn production_builds_refuse_shaping_yaml() {
    let document = format!(
        "state:\n  orders: list\n  lines: list\nentry_point: split\nnodes:\n  - {}",
        SPLIT_NODE.trim_end().replace('\n', "\n    ")
    );
    let Err(error) = super::compiler::PipelineDefinition::from_yaml(&document) else {
        panic!("a production build must not admit split_out");
    };
    assert_eq!(error.code(), "graph.pipeline.unsupported_capability");
}

#[cfg(feature = "graph-extensions-rehearsal")]
mod rehearsal {
    use std::sync::Arc;

    use adk_rust::graph::{ExecutionConfig, GraphError, MemoryCheckpointer, State};
    use serde_json::json;

    use super::super::compiler::PipelineDefinition;
    use super::{JOIN_NODE, SPLIT_NODE};

    fn pipeline(state: &str, nodes: &[&str]) -> String {
        let mut document = format!("state:\n{state}entry_point: split\nnodes:\n");
        for node in nodes {
            let mut lines = node.trim_end().lines();
            if let Some(first) = lines.next() {
                document.push_str("  - ");
                document.push_str(first);
                document.push('\n');
            }
            for line in lines {
                document.push_str("    ");
                document.push_str(line);
                document.push('\n');
            }
        }
        document
    }

    fn definition(state: &str, nodes: &[&str]) -> Result<PipelineDefinition, &'static str> {
        PipelineDefinition::from_yaml(&pipeline(state, nodes)).map_err(|error| error.code())
    }

    async fn run(definition: &PipelineDefinition, input: State) -> Result<State, GraphError> {
        definition
            .compile("pipeline-root", Arc::new(MemoryCheckpointer::new()), None)
            .expect("shaping graph compiles")
            .invoke(input, ExecutionConfig::new("shaping-thread"))
            .await
    }

    const STATE: &str = "  orders: list\n  lines: list\n  note: str\n";
    const ROUND_TRIP_STATE: &str = "  orders: list\n  lines: list\n  orders_out: list\n";

    #[tokio::test]
    async fn split_then_regroup_restores_the_parents_in_one_pipeline() {
        let split = SPLIT_NODE.replace("transition: END", "transition: join");
        let definition =
            definition(ROUND_TRIP_STATE, &[&split, JOIN_NODE]).expect("round-trip pipeline");
        let orders = json!([
            {"id": "A", "items": [{"sku": "x", "qty": 2}, {"sku": "y", "qty": 1.0}]},
            {"id": "B", "items": ["text", null, 3]}
        ]);
        let mut input = State::new();
        input.insert("orders".to_owned(), orders.clone());
        let state = run(&definition, input).await.expect("round-trip run");
        assert_eq!(state.get("orders_out"), Some(&orders));
        assert_eq!(state.get("orders"), Some(&orders));
        assert!(definition.recovery_frontier_supported(&["split".to_owned(), "join".to_owned()]));
    }

    #[tokio::test]
    async fn empty_aggregate_input_follows_the_identity_table() {
        let node = "id: split\ntype: aggregate\nsource: lines\noutput: [summary]\noperations:\n  - {operation: count_rows, output: n}\n  - {operation: collect, field: {path: /v}, output: all}\n  - {operation: sum_int, field: {path: /v}, output: s}\n  - {operation: max_int, field: {path: /v}, output: m}\n  - {operation: first, field: {path: /v}, output: f}\ntransition: END\n";
        let definition = definition("  lines: list\n  summary: list\n", &[node])
            .expect("empty aggregate pipeline");
        let mut input = State::new();
        input.insert("lines".to_owned(), json!([]));
        let state = run(&definition, input).await.expect("empty aggregate run");
        assert_eq!(
            state.get("summary"),
            Some(&json!([{"n": 0, "all": [], "s": 0, "m": null, "f": null}]))
        );
    }

    #[tokio::test]
    async fn split_out_runs_in_a_compiled_pipeline_and_writes_only_its_output() {
        let definition = definition(STATE, &[SPLIT_NODE]).expect("split pipeline");
        let mut input = State::new();
        let orders = json!([{"id": "A", "items": [1, 2]}, {"id": "B", "items": [3]}]);
        input.insert("orders".to_owned(), orders.clone());
        input.insert("note".to_owned(), json!("kept"));
        let state = run(&definition, input).await.expect("split pipeline run");
        assert_eq!(
            state.get("lines"),
            Some(&json!([
                {"parent_index": 0, "position": 0, "data": {"id": "A", "items": 1}},
                {"parent_index": 0, "position": 1, "data": {"id": "A", "items": 2}},
                {"parent_index": 1, "position": 0, "data": {"id": "B", "items": 3}},
            ]))
        );
        assert_eq!(state.get("orders"), Some(&orders));
        assert_eq!(state.get("note"), Some(&json!("kept")));
        assert!(definition.recovery_frontier_supported(&["split".to_owned()]));
    }

    #[tokio::test]
    async fn runtime_failures_surface_typed_codes_without_data() {
        let definition = definition(STATE, &[SPLIT_NODE]).expect("split pipeline");
        let mut input = State::new();
        input.insert(
            "orders".to_owned(),
            json!([{"id": "SECRET-SENTINEL-42", "items": "SECRET-SENTINEL-42"}]),
        );
        let Err(error) = run(&definition, input).await else {
            panic!("a scalar split value must fail");
        };
        let message = error.to_string();
        assert!(message.contains("graph.shaping.type_mismatch: split.path at item 0"));
        assert!(!message.contains("SECRET-SENTINEL-42"));
    }

    #[test]
    fn state_channels_are_validated_at_compile_time() {
        let invalid = "graph.pipeline.invalid_configuration";
        let cases: &[(&str, &str)] = &[
            ("  orders: list\n", SPLIT_NODE),
            ("  orders: str\n  lines: list\n", SPLIT_NODE),
            ("  orders: list\n  lines: dict\n", SPLIT_NODE),
            (
                "  orders: list\n",
                &SPLIT_NODE.replace("output: [lines]", "output: [orders]"),
            ),
            (
                "  orders: list\n",
                &SPLIT_NODE.replace("output: [lines]", "output: [messages]"),
            ),
            (
                "  lines: list\n",
                &SPLIT_NODE.replace("source: orders", "source: messages"),
            ),
            (
                "  orders: list\n  lines: list\n",
                &SPLIT_NODE.replace("mode: rows_field", "mode: row_field"),
            ),
        ];
        for (state, node) in cases {
            assert_eq!(definition(state, &[node]).err(), Some(invalid), "{node}");
        }
        let join_cases: &[&str] = &[
            "  lines: list\n",
            "  lines: dict\n  orders_out: list\n",
            "  lines: list\n  orders_out: str\n",
        ];
        for state in join_cases {
            let join = JOIN_NODE.replace("id: join", "id: split");
            assert_eq!(definition(state, &[&join]).err(), Some(invalid), "{state}");
        }
        assert!(
            definition(
                "  orders: dict\n  lines: list\n",
                &[&SPLIT_NODE.replace("mode: rows_field", "mode: row_field")]
            )
            .is_ok()
        );
    }

    #[test]
    fn pipeline_digest_binds_the_shaping_configuration() {
        let first = definition(STATE, &[SPLIT_NODE]).expect("split pipeline");
        let same = definition(STATE, &[SPLIT_NODE]).expect("split pipeline");
        let changed = definition(STATE, &[&SPLIT_NODE.replace("mode: all", "mode: none")])
            .expect("changed split pipeline");
        assert_eq!(first.definition_digest(), same.definition_digest());
        assert_ne!(first.definition_digest(), changed.definition_digest());
    }
}
