#![allow(
    clippy::disallowed_methods,
    reason = "inline test fixtures, not stored or fetched documents"
)]

//! Admission, compile-time rules, the node guard and graph execution of typed
//! state reducers.

use std::collections::{BTreeMap, HashMap};
use std::sync::Arc;

use adk_rust::graph::{ExecutionConfig, GraphError, Node, NodeContext, NodeOutput};
use async_trait::async_trait;
use serde_json::{Value, json};

use super::state_reducers::{ReducerGuard, StateReducer};

/// Emits a fixed update, optionally as an interrupted node.
struct Emit {
    updates: Vec<(&'static str, Value)>,
    interrupt: bool,
}

#[async_trait]
impl Node for Emit {
    fn name(&self) -> &str {
        "emit"
    }

    async fn execute(&self, _context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let mut output = NodeOutput::new();
        for (key, value) in &self.updates {
            output = output.with_update(key, value.clone());
        }
        if self.interrupt {
            output = output.with_interrupt(adk_rust::graph::interrupt::Interrupt::Dynamic {
                message: "pause".to_owned(),
                data: None,
            });
        }
        Ok(output)
    }
}

fn guard(updates: Vec<(&'static str, Value)>, interrupt: bool) -> ReducerGuard<Emit> {
    ReducerGuard::new(
        Emit { updates, interrupt },
        Arc::new(BTreeMap::from([
            ("findings".to_owned(), StateReducer::Append),
            ("total".to_owned(), StateReducer::SumInt),
        ])),
    )
}

fn context() -> NodeContext {
    NodeContext::new(
        HashMap::from([
            ("findings".to_owned(), json!(["a"])),
            ("total".to_owned(), json!(i64::MAX)),
            ("plain".to_owned(), json!("kept")),
        ]),
        ExecutionConfig::new("guard-thread"),
        0,
    )
}

#[tokio::test]
async fn the_guard_passes_valid_typed_and_untyped_updates_unchanged() {
    let output = guard(
        vec![("findings", json!(["b"])), ("plain", json!(42))],
        false,
    )
    .execute(&context())
    .await
    .expect("valid updates");
    // The guard only checks; ADK's channel reducer performs the reduction.
    assert_eq!(output.updates.get("findings"), Some(&json!(["b"])));
    assert_eq!(output.updates.get("plain"), Some(&json!(42)));
}

#[tokio::test]
async fn the_guard_fails_the_node_naming_only_code_and_channel() {
    for (updates, code, channel) in [
        (
            vec![("findings", json!("SENTINEL-scalar"))],
            "graph.state.reducer_type_mismatch",
            "findings",
        ),
        (
            vec![("total", json!(1))],
            "graph.state.reducer_overflow",
            "total",
        ),
        (
            vec![(
                "findings",
                Value::Array(vec![json!("SENTINEL"); super::state_reducers::MAX_APPEND_ELEMENTS]),
            )],
            "graph.state.reducer_limit",
            "findings",
        ),
    ] {
        let Err(GraphError::NodeExecutionFailed { node, message }) =
            guard(updates, false).execute(&context()).await
        else {
            panic!("{code} must fail the node");
        };
        assert_eq!(node, "emit");
        assert_eq!(message, format!("{code}: {channel}"));
        assert!(!message.contains("SENTINEL"));
    }
}

#[tokio::test]
async fn the_guard_ignores_updates_an_interrupt_discards() {
    let output = guard(vec![("findings", json!("scalar"))], true)
        .execute(&context())
        .await
        .expect("an interrupted node's updates are dropped by ADK");
    assert!(output.interrupt.is_some());
}

#[cfg(not(feature = "graph-extensions-rehearsal"))]
#[test]
fn production_builds_refuse_any_reducer_key() {
    for reducer in ["append", "overwrite"] {
        let yaml = format!(
            "state:\n  findings: {{type: list, value: [], reducer: {reducer}}}\nentry_point: tick\nnodes:\n  - {{id: tick, type: state_modifier, template: '[]', output: [findings], transition: END}}\n"
        );
        let Err(error) = super::compiler::PipelineDefinition::from_yaml(&yaml) else {
            panic!("a production build must not admit reducer: {reducer}");
        };
        assert_eq!(error.code(), "graph.pipeline.unsupported_capability");
    }
}

#[cfg(feature = "graph-extensions-rehearsal")]
mod rehearsal {
    use std::sync::Arc;

    use adk_rust::graph::{Checkpointer, ExecutionConfig, MemoryCheckpointer, State};
    use serde_json::{Value, json};

    use super::super::compiler::{PipelineDefinition, PipelineNodeRuntimes};
    use super::super::static_pause_tests::{THREAD, continuation, fixture, run};

    pub(super) const LOOP: &str = r#"
state:
  count: {type: int, value: 0, reducer: sum_int}
  findings: {type: list, value: [seed], reducer: append}
entry_point: tick
nodes:
  - id: tick
    type: state_modifier
    template: "1"
    output: [count]
    transition: collect
  - id: collect
    type: state_modifier
    template: '["visit {{ count }}"]'
    input: [count]
    output: [findings]
    transition: choose
  - id: choose
    type: router
    condition: "{{ 'tick' if count < 3 else 'END' }}"
    input: [count]
    routes: [tick, END]
    default_output: END
"#;

    fn refusal(yaml: &str) -> String {
        match PipelineDefinition::from_yaml(yaml) {
            Ok(_) => panic!("the declaration must be refused:\n{yaml}"),
            Err(error) => format!("{}|{error}", error.code()),
        }
    }

    fn single(state: &str) -> String {
        format!(
            "state:\n{state}entry_point: tick\nnodes:\n  - {{id: tick, type: state_modifier, template: '1', transition: END}}\n"
        )
    }

    #[test]
    fn reducers_must_match_their_state_type_and_default() {
        for state in [
            "  v: {type: dict, value: {}, reducer: append}\n",
            "  v: {type: str, reducer: append}\n",
            "  v: {type: list, value: [], reducer: sum_int}\n",
            "  v: {type: float, value: 0.0, reducer: sum_int}\n",
            "  v: {type: list, value: [], reducer: merge}\n",
        ] {
            assert!(
                refusal(&single(state)).contains("does not match its state type"),
                "{state}"
            );
        }
        for state in [
            "  v: {type: int, value: 18446744073709551615, reducer: sum_int}\n",
            &format!(
                "  v: {{type: list, value: [{}], reducer: append}}\n",
                vec!["0"; super::super::state_reducers::MAX_APPEND_ELEMENTS + 1].join(",")
            ),
        ] {
            let refused = refusal(&single(state));
            assert!(
                refused.contains("does not match its state type")
                    || refused.contains("exceeds its reducer bounds"),
                "{refused}"
            );
        }
        assert!(
            refusal(&single("  v: {type: list, value: [], reducer: extend}\n"))
                .contains("reducer is not supported")
        );
    }

    #[test]
    fn builtin_channels_never_take_a_reducer() {
        for state in [
            "  input: {type: str, reducer: overwrite}\n",
            "  messages: {type: list, reducer: append}\n",
        ] {
            assert!(
                refusal(&single(state)).contains("cannot declare a reducer"),
                "{state}"
            );
        }
    }

    #[test]
    fn replacing_channels_must_overwrite() {
        let map = "entry_point: map_entities\nstate:\n  entities: {type: list, value: []}\n  item_result: {type: str, value: ''}\n  mapped: {type: list, value: []}\nnodes:\n  - {id: map_entities, type: map, worker: render, source: entities, item: entity, index: item_index, outputs: [item_result], destination: mapped, max_items: 64, max_concurrency: 4, reduction: ordered_collection, transition: END}\n  - {id: render, type: state_modifier, input: [entity, item_index], output: [item_result], template: '{{ item_index }}'}\n";
        let parallel = "state:\n  topic: string\n  detail: {type: dict, value: {}}\n  joined: list\nentry_point: gather\nnodes:\n  - id: gather\n    type: parallel\n    branches: [{id: left, node: workerA}, {id: right, node: workerB}]\n    max_concurrency: 2\n    wait: all\n    output: [joined]\n    transition: END\n  - id: workerA\n    type: agent\n    tool: Research Agent\n    input: [topic]\n    input_mapping:\n      task: {type: fixed, value: Research}\n    output: [detail]\n  - id: workerB\n    type: agent\n    tool: Research Agent\n    input: [topic]\n    input_mapping:\n      task: {type: fixed, value: Compare sources}\n    output: [detail]\n";
        let shaping = "state:\n  orders: list\n  lines: list\nentry_point: split\nnodes:\n  - id: split\n    type: split_out\n    source: orders\n    output: [lines]\n    split: {mode: rows_field, path: /items}\n    destination: items\n    retain: {mode: all}\n    transition: END\n";
        let hitl = "state:\n  summary: {type: dict, value: {}}\nentry_point: review\nnodes:\n  - id: review\n    type: hitl\n    input: [summary]\n    user_message: {type: fixed, value: Review.}\n    routes: {approve: END, reject: END, edit: END}\n    edit_state_key: summary\n";
        let clean = "state:\n  notes: list\n  out: str\nentry_point: tick\nnodes:\n  - {id: tick, type: state_modifier, template: x, output: [out], variables_to_clean: [notes], transition: END}\n";
        for (base, from, to) in [
            (map, "mapped: {type: list, value: []}", "mapped: {type: list, value: [], reducer: append}"),
            (parallel, "joined: list", "joined: {type: list, reducer: append}"),
            (parallel, "detail: {type: dict, value: {}}", "detail: {type: dict, value: {}, reducer: merge}"),
            (shaping, "lines: list", "lines: {type: list, reducer: append}"),
            (hitl, "summary: {type: dict, value: {}}", "summary: {type: dict, value: {}, reducer: merge}"),
            (clean, "notes: list", "notes: {type: list, reducer: append}"),
        ] {
            if let Err(error) = PipelineDefinition::from_yaml(base) {
                panic!("the overwrite control for {from} must compile: {error}");
            }
            let typed = base.replace(from, to);
            assert_ne!(typed, base, "fixture substitution {from}");
            let refused = refusal(&typed);
            assert!(refused.contains("must use the overwrite reducer"), "{refused}");
        }
    }

    #[test]
    fn a_map_owned_output_must_overwrite() {
        let map = "entry_point: map_entities\nstate:\n  entities: {type: list, value: []}\n  item_result: {type: dict, value: {}, reducer: merge}\n  mapped: {type: list, value: []}\nnodes:\n  - {id: map_entities, type: map, worker: render, source: entities, item: entity, index: item_index, outputs: [item_result], destination: mapped, max_items: 64, max_concurrency: 4, reduction: ordered_collection, transition: END}\n  - {id: render, type: state_modifier, input: [entity, item_index], output: [item_result], template: '{}'}\n";
        assert!(refusal(map).contains("must use the overwrite reducer"));
    }

    #[test]
    fn nested_child_pipelines_refuse_typed_reducers_in_v1() {
        let definition = PipelineDefinition::from_yaml(LOOP).expect("typed loop");
        let Err(error) = definition.compile_subgraph_with_runtime(
            Arc::new(MemoryCheckpointer::new()),
            &PipelineNodeRuntimes::default(),
        ) else {
            panic!("a nested child must refuse typed reducers");
        };
        assert_eq!(error.code(), "graph.pipeline.unsupported_capability");
    }

    fn visits(count: usize) -> Value {
        let mut findings = vec![json!("seed")];
        findings.extend((1..=count).map(|visit| json!(format!("visit {visit}"))));
        Value::Array(findings)
    }

    #[tokio::test]
    async fn a_router_loop_appends_once_per_visit_from_a_single_default() {
        let definition = PipelineDefinition::from_yaml(LOOP).expect("typed loop");
        let (checkpointer, sessions) = fixture().await;
        run(&definition, checkpointer.clone(), sessions, None).await;
        let history = checkpointer.list(THREAD).await.expect("history");
        // The initial checkpoint holds the default once, not `[seed, seed]`.
        assert_eq!(history[0].state["findings"], json!(["seed"]));
        assert_eq!(history[0].state["count"], json!(0));
        let last = history.last().expect("terminal checkpoint");
        assert!(last.pending_nodes.is_empty());
        assert_eq!(last.state["count"], json!(3));
        assert_eq!(last.state["findings"], visits(3));
    }

    #[tokio::test]
    async fn static_pauses_around_the_reducing_node_reduce_once_per_visit() {
        let definition = PipelineDefinition::from_yaml(&format!(
            "interrupt_before: [collect]\ninterrupt_after: [collect]\n{LOOP}"
        ))
        .expect("paused typed loop");
        let (checkpointer, sessions) = fixture().await;
        let mut resume = None;
        let mut appended = Vec::new();
        for _ in 0..7 {
            run(
                &definition,
                checkpointer.clone(),
                sessions.clone(),
                resume.take(),
            )
            .await;
            let checkpoint = checkpointer.load(THREAD).await.unwrap().unwrap();
            appended.push(checkpoint.state["findings"].as_array().map(Vec::len));
            if checkpoint.pending_nodes.is_empty() && checkpoint.state["count"] == json!(3) {
                break;
            }
            resume =
                Some(continuation(&definition, checkpointer.as_ref(), sessions.as_ref()).await);
        }
        // before/after pairs: each `after` pause and its resume append exactly one element.
        assert_eq!(
            appended,
            [Some(1), Some(2), Some(2), Some(3), Some(3), Some(4), Some(4)]
        );
        let last = checkpointer.load(THREAD).await.unwrap().unwrap();
        assert_eq!(last.state["findings"], visits(3));
        assert_eq!(last.state["count"], json!(3));
    }

    #[tokio::test]
    async fn sum_int_overflow_fails_the_node_and_keeps_the_last_state() {
        let yaml = LOOP
            .replace("value: 0, reducer: sum_int", "value: 9223372036854775806, reducer: sum_int")
            .replace("count < 3", "true");
        let definition = PipelineDefinition::from_yaml(&yaml).expect("overflow fixture");
        let checkpointer = Arc::new(MemoryCheckpointer::new());
        let error = definition
            .compile("pipeline-root", checkpointer.clone(), None)
            .expect("overflow graph")
            .invoke(State::new(), ExecutionConfig::new("overflow-thread"))
            .await
            .expect_err("the second visit overflows");
        let message = error.to_string();
        assert!(message.contains("graph.state.reducer_overflow: count"), "{message}");
        assert!(!message.contains("9223372036854775"), "{message}");
        let history = checkpointer.list("overflow-thread").await.expect("history");
        let last = history.last().expect("checkpoint before the failure");
        assert_eq!(last.state["count"], json!(i64::MAX));
        assert_eq!(last.state["findings"], json!(["seed", format!("visit {}", i64::MAX)]));
    }
}
