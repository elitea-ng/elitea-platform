#![allow(
    clippy::disallowed_methods,
    reason = "inline test fixtures and the contract file, not stored documents"
)]

//! 5b-1 node settings: admission (Worker side of the contract), the
//! deterministic settings layer, run once per turn, digests and logs.

use std::collections::{BTreeSet, HashMap};
use std::sync::Arc;

use adk_rust::graph::{ExecutionConfig, GraphError, MemoryCheckpointer, State};
use elitea_agent_runtime::graph::node_settings::{
    MAX_NODE_ERRORS, NodeSettingsRefusal, SettingsFamily,
};
use serde_json::{Value, json};

use super::compiler::{PipelineConfigurationError, PipelineDefinition, PipelineNodeRuntimes};

const CONTINUE: &str = r#"
state:
  value: str
  out: str
  count: str
entry_point: shape
nodes:
  - id: shape
    type: state_modifier
    template: "{{ value | nosuchfilter }}"
    input: [value]
    output: [out]
    settings: {on_error: continue, always_output_data: true}
    transition: after
  - id: after
    type: state_modifier
    template: "{{ node_errors | length }}"
    input: [node_errors]
    output: [count]
    transition: END
"#;

const ROUTER: &str = r#"
state:
  value: str
  out: str
entry_point: choose
nodes:
  - id: choose
    type: router
    condition: "{{ value | nosuchfilter }}"
    input: [value]
    routes: [left, right]
    default_output: right
    settings: {on_error: continue}
  - id: left
    type: state_modifier
    template: "left"
    output: [out]
    transition: END
  - id: right
    type: state_modifier
    template: "right"
    output: [out]
    transition: END
"#;

const ONCE: &str = r#"
state:
  visits: {type: int, value: 0}
  count: {type: int, value: 0}
entry_point: bump
nodes:
  - id: bump
    type: state_modifier
    template: "{{ visits + 1 }}"
    input: [visits]
    output: [visits]
    transition: tick
  - id: tick
    type: state_modifier
    template: "{{ count + 1 }}"
    input: [count]
    output: [count]
    settings: {execute_once: true}
    transition: choose
  - id: choose
    type: router
    condition: "{{ 'bump' if visits < 3 else 'END' }}"
    input: [visits]
    routes: [bump, END]
    default_output: END
"#;

const AGENT: &str = "  - id: delegate\n    type: agent\n    tool: child\n    input_mapping: {task: {type: fixed, value: work}}\n    output: [answer]\n    transition: END\n";

fn refused(yaml: &str) -> (String, NodeSettingsRefusal) {
    match PipelineDefinition::from_yaml(yaml) {
        Err(PipelineConfigurationError::NodeSettings { node, refusal }) => (node, refusal),
        Err(other) => panic!("expected a settings refusal, got {other}"),
        Ok(_) => panic!("expected a settings refusal, the document compiled"),
    }
}

async fn invoke(yaml: &str, input: State) -> Result<State, GraphError> {
    let definition = PipelineDefinition::from_yaml(yaml).expect("fixture compiles");
    definition
        .compile_with_runtime(
            "pipeline-root",
            Arc::new(MemoryCheckpointer::new()),
            None,
            &PipelineNodeRuntimes::default().with_execution_id("execution-1"),
        )
        .expect("graph")
        .invoke(input, ExecutionConfig::new("settings-thread"))
        .await
}

fn state(pairs: &[(&str, Value)]) -> State {
    pairs
        .iter()
        .map(|(key, value)| ((*key).to_owned(), value.clone()))
        .collect::<HashMap<_, _>>()
}

#[tokio::test]
async fn a_continued_state_modifier_failure_appends_one_entry_and_runs_on() {
    let result = invoke(CONTINUE, state(&[("value", json!("SENTINEL-input"))]))
        .await
        .expect("the failure is continued");
    let entries = result["node_errors"].as_array().expect("node_errors list");
    assert_eq!(entries.len(), 1);
    let entry = &entries[0];
    assert_eq!(entry["node"], "shape");
    assert_eq!(entry["node_type"], "state_modifier");
    assert_eq!(entry["handled"], "continue");
    assert_eq!(entry["status"], "failed");
    assert_eq!(entry["attempts"], 1);
    assert_eq!(entry["retryable"], false);
    assert_eq!(entry["error"]["code"], "pipeline.state_modifier.failed");
    assert_eq!(
        entry["error"]["message_safe"],
        elitea_faults::codes::PIPELINE_STATE_MODIFIER_FAILED.sentence()
    );
    assert!(entry["input_step"].is_u64());
    // always_output_data: the declared output gets its neutral value.
    assert_eq!(result["out"], json!(""));
    // The next node ran and read the list.
    assert_eq!(result["count"], json!("1"));
    assert!(!entry.to_string().contains("SENTINEL"));
}

#[tokio::test]
async fn without_settings_the_same_failure_stops_the_run() {
    let yaml = CONTINUE.replace(
        "    settings: {on_error: continue, always_output_data: true}\n",
        "",
    );
    let yaml = yaml.replace("input: [node_errors]", "input: [value]");
    let error = invoke(&yaml, state(&[("value", json!("x"))]))
        .await
        .expect_err("stop is the default");
    assert!(!error.to_string().contains("node_errors"));
}

#[tokio::test]
async fn error_output_routes_to_the_handler_and_skips_the_normal_transition() {
    let yaml = CONTINUE
        .replace(
            "settings: {on_error: continue, always_output_data: true}",
            "settings: {on_error: error_output, error_transition: handler}",
        )
        .replace(
            "    transition: END\n",
            "    transition: END\n  - id: handler\n    type: state_modifier\n    template: \"{{ node_errors[-1].node }}\"\n    input: [node_errors]\n    output: [out]\n    transition: END\n",
        );
    let result = invoke(&yaml, state(&[("value", json!("x"))]))
        .await
        .expect("routed");
    assert_eq!(result["out"], json!("shape"));
    assert_eq!(result["node_errors"][0]["handled"], "error_output");
    // `after` never ran: its output keeps the default.
    assert_eq!(result["count"], json!(""));
}

#[tokio::test]
async fn a_failing_error_handler_stops_by_default() {
    let yaml = CONTINUE
        .replace(
            "settings: {on_error: continue, always_output_data: true}",
            "settings: {on_error: error_output, error_transition: handler}",
        )
        .replace(
            "    transition: END\n",
            "    transition: END\n  - id: handler\n    type: state_modifier\n    template: \"{{ x | nosuchfilter }}\"\n    output: [out]\n    transition: END\n",
        );
    assert!(
        invoke(&yaml, state(&[("value", json!("x"))]))
            .await
            .is_err()
    );
}

#[tokio::test]
async fn a_continued_router_failure_takes_its_default_route() {
    let result = invoke(ROUTER, state(&[("value", json!("x"))]))
        .await
        .expect("continued");
    assert_eq!(result["out"], json!("right"));
    assert_eq!(result["router_output"], json!("right"));
    assert_eq!(
        result["node_errors"][0]["error"]["code"],
        "pipeline.router.failed"
    );
}

/// Performance: a settled failure rides the step's own checkpoint write;
/// the run writes as many checkpoints as the same run succeeding.
#[tokio::test]
async fn a_continued_failure_writes_no_extra_checkpoint() {
    async fn checkpoints(yaml: &str) -> usize {
        let checkpointer = Arc::new(MemoryCheckpointer::new());
        PipelineDefinition::from_yaml(yaml)
            .expect("fixture")
            .compile_with_runtime(
                "pipeline-root",
                checkpointer.clone(),
                None,
                &PipelineNodeRuntimes::default(),
            )
            .expect("graph")
            .invoke(
                state(&[("value", json!("x"))]),
                ExecutionConfig::new("count-thread"),
            )
            .await
            .expect("run");
        adk_rust::graph::Checkpointer::list(checkpointer.as_ref(), "count-thread")
            .await
            .expect("history")
            .len()
    }
    let failing = checkpoints(CONTINUE).await;
    let succeeding =
        checkpoints(&CONTINUE.replace("{{ value | nosuchfilter }}", "{{ value }}")).await;
    assert!(failing > 0);
    assert_eq!(failing, succeeding);
}

#[test]
fn a_router_without_an_explicit_default_refuses_continue() {
    // C5: the key is absent and the parser fills END; that never counts.
    let yaml = ROUTER.replace("    default_output: right\n", "");
    assert_eq!(
        refused(&yaml),
        (
            "choose".to_owned(),
            NodeSettingsRefusal::RouterContinueNeedsDefault
        )
    );
    // An explicit `default_output: END` is a declared default.
    let explicit = ROUTER.replace("default_output: right", "default_output: END");
    assert!(PipelineDefinition::from_yaml(&explicit).is_ok());
    // error_output does not need a default.
    let routed = yaml.replace(
        "settings: {on_error: continue}",
        "settings: {on_error: error_output, error_transition: left}",
    );
    assert!(PipelineDefinition::from_yaml(&routed).is_ok());
}

#[tokio::test]
async fn a_run_once_node_runs_once_in_a_three_visit_loop() {
    let result = invoke(ONCE, State::new()).await.expect("loop");
    assert_eq!(result["visits"], json!(3));
    assert_eq!(result["count"], json!(1));
    assert_eq!(
        result["__elitea_executed_once_v1"],
        json!({"nodes": {"tick": null}})
    );
}

#[tokio::test]
async fn a_run_once_router_takes_its_first_route_again() {
    let yaml = r#"
state:
  visits: {type: int, value: 0}
entry_point: bump
nodes:
  - id: bump
    type: state_modifier
    template: "{{ visits + 1 }}"
    input: [visits]
    output: [visits]
    transition: gate
  - id: gate
    type: router
    condition: "{{ 'bump' if visits < 3 else 'END' }}"
    input: [visits]
    routes: [bump, END]
    default_output: END
    settings: {execute_once: true}
"#;
    // The first visit routes to bump; every revisit takes that route again
    // until the recursion bound stops the run: proof the body never re-ran.
    let error = invoke(yaml, State::new())
        .await
        .expect_err("loops on the first route");
    assert!(
        matches!(error, GraphError::RecursionLimitExceeded(_)),
        "{error}"
    );
}

#[tokio::test]
async fn the_node_errors_bound_holds_at_64_and_fails_typed_at_65() {
    let filled = |count: usize| {
        let entries: Vec<Value> = (0..count).map(|index| json!({"seed": index})).collect();
        format!(
            "state:\n  node_errors: {{type: list, value: {}}}\n",
            Value::Array(entries)
        )
    };
    let base = CONTINUE.replacen("state:\n", "", 1);
    let at_bound = invoke(
        &format!("{}{base}", filled(MAX_NODE_ERRORS - 1)),
        state(&[("value", json!("x"))]),
    )
    .await
    .expect("the 64th entry is recorded");
    assert_eq!(
        at_bound["node_errors"].as_array().map(Vec::len),
        Some(MAX_NODE_ERRORS)
    );
    let error = invoke(
        &format!("{}{base}", filled(MAX_NODE_ERRORS)),
        state(&[("value", json!("x"))]),
    )
    .await
    .expect_err("the 65th failure is not continued");
    assert!(
        error.to_string().contains("pipeline.node_errors.exhausted"),
        "{error}"
    );
}

#[test]
fn only_a_body_failure_is_settled() {
    use super::node_settings_runtime::settled_by_settings;
    assert!(settled_by_settings(&GraphError::NodeExecutionFailed {
        node: "n".to_owned(),
        message: "the pipeline Router node failed".to_owned(),
    }));
    assert!(settled_by_settings(&GraphError::Other(
        "state modifier template evaluation failed".to_owned()
    )));
    for never in [
        GraphError::RecursionLimitExceeded(100),
        GraphError::CheckpointError("lease lost".to_owned()),
        GraphError::Other("pipeline.node_recovery.invalid_activation".to_owned()),
        GraphError::Other("pipeline.node_errors.exhausted".to_owned()),
        GraphError::Other("graph.state.reducer_limit: node_errors".to_owned()),
        GraphError::NodeTimedOut {
            node: "n".to_owned(),
            elapsed: std::time::Duration::from_secs(1),
        },
    ] {
        assert!(!settled_by_settings(&never), "{never}");
    }
}

#[test]
fn every_admission_refusal_is_typed_and_names_the_node() {
    let modifier = |settings: &str| {
        format!(
            "state:\n  out: str\n  total: {{type: int, value: 0, reducer: sum_int}}\nentry_point: a\nnodes:\n  - id: a\n    type: state_modifier\n    template: x\n    output: [out]\n    settings: {settings}\n    transition: b\n  - id: b\n    type: state_modifier\n    template: '1'\n    output: [total]\n    transition: END\n"
        )
    };
    for (settings, expected) in [
        (
            "{retry_on_fail: true}",
            NodeSettingsRefusal::RetryNotAllowed,
        ),
        ("{timeout_ms: 2000}", NodeSettingsRefusal::TimeoutNotAllowed),
        (
            "{retry_on_fail: true, max_tries: 6}",
            NodeSettingsRefusal::MaxTriesRange,
        ),
        ("{max_tries: 3}", NodeSettingsRefusal::RetryFieldsNeedRetry),
        (
            "{on_error: error_output}",
            NodeSettingsRefusal::ErrorTransitionRequired,
        ),
        (
            "{on_error: error_output, error_transition: a}",
            NodeSettingsRefusal::ErrorTransitionTarget,
        ),
        (
            "{on_error: error_output, error_transition: nowhere}",
            NodeSettingsRefusal::ErrorTransitionTarget,
        ),
        (
            "{on_error: continue, error_transition: b}",
            NodeSettingsRefusal::ErrorTransitionUnexpected,
        ),
        ("{bogus: 1}", NodeSettingsRefusal::Malformed),
    ] {
        assert_eq!(
            refused(&modifier(settings)),
            ("a".to_owned(), expected),
            "{settings}"
        );
    }
    // always_output_data on a sum_int output.
    let summing = modifier("{always_output_data: true}").replace(
        "  - id: a\n    type: state_modifier\n    template: x\n    output: [out]",
        "  - id: a\n    type: state_modifier\n    template: x\n    output: [total]",
    );
    assert_eq!(
        refused(&summing),
        ("a".to_owned(), NodeSettingsRefusal::AlwaysOutputReducer)
    );
    // Families not wired yet and pauses refuse the whole block.
    let code = "state:\n  answer: int\nentry_point: run\nnodes:\n  - id: run\n    type: code\n    code: '7'\n    output: [answer]\n    settings: {on_error: continue}\n    transition: END\n";
    assert_eq!(
        refused(code),
        ("run".to_owned(), NodeSettingsRefusal::FamilyNotYet)
    );
    let hitl = "state:\n  summary: {type: dict, value: {}}\nentry_point: review\nnodes:\n  - id: review\n    type: hitl\n    input: [summary]\n    user_message: {type: fixed, value: Review.}\n    routes: {approve: END, reject: END, edit: END}\n    edit_state_key: summary\n    settings: {execute_once: true}\n";
    assert_eq!(
        refused(hitl),
        ("review".to_owned(), NodeSettingsRefusal::FamilyPause)
    );
    // Both settings and recovery on one node.
    let recovered = "state:\n  answer: int\nentry_point: run\nnodes:\n  - id: run\n    type: code\n    code: '7'\n    output: [answer]\n    settings: {on_error: continue}\n    recovery:\n      max_attempts: 3\n      retry_on: [dependency_unavailable]\n      backoff: {initial_ms: 10, maximum_ms: 50, multiplier: 2}\n      max_elapsed_ms: 1000\n    transition: END\n";
    assert_eq!(
        refused(recovered),
        ("run".to_owned(), NodeSettingsRefusal::WithRecovery)
    );
    // A Map-owned node, and an error route into a Map-owned node.
    let map = "entry_point: map_entities\nstate:\n  entities: {type: list, value: []}\n  item_result: {type: str, value: ''}\n  mapped: {type: list, value: []}\n  tail_out: str\nnodes:\n  - {id: map_entities, type: map, worker: render, source: entities, item: entity, index: item_index, outputs: [item_result], destination: mapped, max_items: 64, max_concurrency: 4, reduction: ordered_collection, transition: tail}\n  - {id: render, type: state_modifier, input: [entity, item_index], output: [item_result], template: '{{ item_index }}'}\n  - {id: tail, type: state_modifier, template: x, output: [tail_out], transition: END}\n";
    let owned = map.replace(
        "template: '{{ item_index }}'}",
        "template: '{{ item_index }}', settings: {on_error: continue}}",
    );
    assert_eq!(
        refused(&owned),
        ("render".to_owned(), NodeSettingsRefusal::FanoutOwned)
    );
    let into_owned = map.replace(
        "template: x, output",
        "template: x, settings: {on_error: error_output, error_transition: render}, output",
    );
    assert_eq!(
        refused(&into_owned),
        (
            "tail".to_owned(),
            NodeSettingsRefusal::ErrorTransitionTarget
        )
    );
}

#[test]
fn a_user_node_errors_key_is_checked_only_when_a_node_continues() {
    // C3: with a continuing node, a wrong type or reducer is refused.
    for declared in [
        "node_errors: str",
        "node_errors: {type: dict, value: {}, reducer: merge}",
    ] {
        let yaml = CONTINUE.replacen("state:\n", &format!("state:\n  {declared}\n"), 1);
        assert_eq!(
            refused(&yaml).1,
            NodeSettingsRefusal::NodeErrorsType,
            "{declared}"
        );
    }
    // A list with no reducer, or append, is admitted.
    for declared in [
        "node_errors: list",
        "node_errors: {type: list, reducer: append}",
    ] {
        let yaml = CONTINUE.replacen("state:\n", &format!("state:\n  {declared}\n"), 1);
        assert!(PipelineDefinition::from_yaml(&yaml).is_ok(), "{declared}");
    }
    // Without any continuing node the same key is never refused.
    let plain = "state:\n  node_errors: str\n  out: str\nentry_point: a\nnodes:\n  - id: a\n    type: state_modifier\n    template: x\n    input: [node_errors]\n    output: [out]\n    transition: END\n";
    assert!(PipelineDefinition::from_yaml(plain).is_ok());
    let once = plain.replace(
        "    transition: END\n",
        "    settings: {execute_once: true}\n    transition: END\n",
    );
    assert!(PipelineDefinition::from_yaml(&once).is_ok());
}

#[test]
fn a_continuing_parent_never_maps_node_errors_into_or_out_of_a_child() {
    let parent = |agent: &str| {
        format!(
            "state:\n  value: str\n  out: str\n  answer: str\nentry_point: shape\nnodes:\n  - id: shape\n    type: state_modifier\n    template: x\n    output: [out]\n    settings: {{on_error: continue}}\n    transition: delegate\n{agent}"
        )
    };
    assert!(PipelineDefinition::from_yaml(&parent(AGENT)).is_ok());
    // Out of the child into the parent's list.
    let out = AGENT.replace("output: [answer]", "output: [node_errors]");
    assert_eq!(
        refused(&parent(&out)),
        (
            "delegate".to_owned(),
            NodeSettingsRefusal::NodeErrorsNestedMapping
        )
    );
    // Into the child.
    let into = AGENT.replace(
        "input_mapping: {task: {type: fixed, value: work}}",
        "input_mapping: {task: {type: fixed, value: work}, node_errors: {type: variable, value: node_errors}}",
    );
    assert_eq!(
        refused(&parent(&into)),
        (
            "delegate".to_owned(),
            NodeSettingsRefusal::NodeErrorsNestedMapping
        )
    );
}

#[test]
fn a_child_that_owns_node_errors_compiles_nested_and_hides_its_list() {
    use adk_rust::graph::MemoryCheckpointer;
    let child = PipelineDefinition::from_yaml(CONTINUE).expect("child");
    // D3: the settings channel is exempt from the nested reducer refusal.
    assert!(
        child
            .compile_subgraph_with_runtime(
                Arc::new(MemoryCheckpointer::new()),
                &PipelineNodeRuntimes::default()
            )
            .is_ok()
    );
    // C4: the parent can neither map into it nor read it back.
    assert!(!child.declared_variable_types().contains_key("node_errors"));
    // Another typed reducer in the same child keeps the X3 refusal.
    let typed = CONTINUE.replace(
        "  count: str\n",
        "  count: str\n  seen: {type: list, reducer: append}\n",
    );
    let typed = PipelineDefinition::from_yaml(&typed).expect("typed child");
    assert!(
        typed
            .compile_subgraph_with_runtime(
                Arc::new(MemoryCheckpointer::new()),
                &PipelineNodeRuntimes::default()
            )
            .is_err()
    );
    // A user's own list without continuing nodes stays a mappable variable.
    let plain = "state:\n  node_errors: list\n  out: str\nentry_point: a\nnodes:\n  - id: a\n    type: state_modifier\n    template: x\n    output: [out]\n    transition: END\n";
    let plain = PipelineDefinition::from_yaml(plain).expect("plain child");
    assert!(plain.declared_variable_types().contains_key("node_errors"));
}

#[test]
fn documents_without_settings_keep_their_digest() {
    for yaml in [CONTINUE, ROUTER, ONCE] {
        let with = PipelineDefinition::from_yaml(yaml)
            .expect("fixture")
            .definition_digest();
        let without_yaml = yaml
            .replace(
                "    settings: {on_error: continue, always_output_data: true}\n",
                "",
            )
            .replace("    settings: {on_error: continue}\n", "")
            .replace("    settings: {execute_once: true}\n", "")
            .replace("input: [node_errors]", "input: [value]");
        let without = PipelineDefinition::from_yaml(&without_yaml).expect("plain");
        assert_ne!(
            with,
            without.definition_digest(),
            "settings fold into the digest"
        );
        // An all-default block is the same document as no block.
        let empty = without_yaml.replacen(
            "    transition:",
            "    settings: {on_error: stop, execute_once: false}\n    transition:",
            1,
        );
        assert_ne!(empty, without_yaml);
        assert_eq!(
            PipelineDefinition::from_yaml(&empty)
                .expect("default block")
                .definition_digest(),
            without.definition_digest()
        );
    }
}

#[test]
fn the_run_once_channel_is_reserved() {
    let yaml = "state:\n  __elitea_executed_once_v1: dict\n  out: str\nentry_point: a\nnodes:\n  - id: a\n    type: state_modifier\n    template: x\n    output: [out]\n    transition: END\n";
    assert!(PipelineDefinition::from_yaml(yaml).is_err());
}

/// Worker side of the settings contract: every admitted family compiles
/// with a block, every other admitted node type refuses it.
#[test]
fn the_admitted_families_are_the_contracts() {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../libs/proto/contracts/pipeline-node-settings-v1.json");
    let contract: Value =
        serde_json::from_str(&std::fs::read_to_string(path).expect("contract")).expect("JSON");
    let admission_path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../libs/proto/contracts/pipeline-node-admission-v1.json");
    let admission: Value =
        serde_json::from_str(&std::fs::read_to_string(admission_path).expect("admission contract"))
            .expect("JSON");
    let families: BTreeSet<&str> = contract["families"]
        .as_object()
        .expect("families")
        .keys()
        .map(String::as_str)
        .collect();
    let pauses: BTreeSet<&str> = contract["pause_families"]
        .as_array()
        .expect("pauses")
        .iter()
        .filter_map(Value::as_str)
        .collect();
    for node_type in admission["admitted"].as_array().expect("admitted") {
        let node_type = node_type.as_str().expect("type");
        let family = SettingsFamily::of(node_type);
        if families.contains(node_type) {
            assert!(
                matches!(family, SettingsFamily::Deterministic(_)),
                "{node_type}"
            );
        } else if pauses.contains(node_type) {
            assert_eq!(family, SettingsFamily::Pause, "{node_type}");
        } else {
            assert_eq!(family, SettingsFamily::NotYet, "{node_type}");
        }
    }
    for refusal in NodeSettingsRefusal::ALL {
        let error = PipelineConfigurationError::NodeSettings {
            node: "n".to_owned(),
            refusal,
        };
        assert_eq!(error.code(), "pipeline.node_settings.invalid");
        assert_eq!(error.cause_detail(), Some(refusal.key()));
        assert_eq!(
            contract["refusals"][refusal.key()].as_str(),
            Some(refusal.text())
        );
    }
}

const LOG_CHILD_ENV: &str = "ELITEA_NODE_SETTINGS_LOG_CHILD";

/// One operator line per continued failure, with the code, the node and the
/// support reference, and no state, template or input value.
#[test]
fn a_continued_failure_logs_one_data_free_operator_line() {
    let output = elitea_faults::logtest::run_child(
        "agents::graph::node_settings_tests::log_child",
        LOG_CHILD_ENV,
    );
    let lines = output.operator_lines();
    let continued: Vec<_> = lines
        .iter()
        .filter(|line| {
            line.get("code").and_then(Value::as_str) == Some("pipeline.node.failed_continued")
        })
        .collect();
    assert_eq!(continued.len(), 1, "{}", output.stdout);
    let line = continued[0];
    assert_eq!(line["node_id"], "shape");
    assert_eq!(line["failure_code"], "pipeline.state_modifier.failed");
    assert_eq!(line["handled"], "continue");
    assert_eq!(line["execution_id"], "execution-1");
    assert_eq!(line["support_ref"].as_str().map(str::len), Some(32));
    for stream in [&output.stdout, &output.stderr] {
        assert!(!stream.contains("SENTINEL"), "{stream}");
        assert!(!stream.contains("nosuchfilter"), "{stream}");
    }
}

#[test]
fn log_child() {
    use tracing_subscriber::layer::SubscriberExt as _;
    use tracing_subscriber::util::SubscriberInitExt as _;
    if !elitea_faults::logtest::is_child(LOG_CHILD_ENV) {
        return;
    }
    tracing_subscriber::registry()
        .with(crate::diagnostics::log_layer(Some("debug"), std::io::stdout).expect("stream"))
        .init();
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("runtime");
    runtime
        .block_on(invoke(
            CONTINUE,
            state(&[("value", json!("SENTINEL-input"))]),
        ))
        .expect("continued");
    elitea_faults::logtest::child_ran();
}

/// The shared admission vectors: every layer admits or refuses each
/// document with the same refusal, naming the same node.
#[test]
fn the_shared_admission_vectors_hold_in_the_worker() {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../libs/proto/contracts/pipeline-node-settings-v1-vectors.json");
    let vectors: Value =
        serde_json::from_str(&std::fs::read_to_string(path).expect("vectors")).expect("JSON");
    let cases = vectors["cases"].as_array().expect("cases");
    assert!(cases.len() >= 40);
    for case in cases {
        let name = case["name"].as_str().expect("name");
        let yaml = case["yaml"].as_str().expect("yaml");
        match (PipelineDefinition::from_yaml(yaml), case["refusal"].as_str()) {
            (Ok(_), None) => {}
            (Err(PipelineConfigurationError::NodeSettings { node, refusal }), Some(expected)) => {
                assert_eq!(refusal.key(), expected, "{name}");
                if let Some(expected_node) = case["node"].as_str() {
                    assert_eq!(node, expected_node, "{name}");
                } else {
                    assert!(refusal.document_level(), "{name}");
                }
            }
            (Ok(_), Some(expected)) => panic!("{name}: admitted, expected {expected}"),
            (Err(error), expected) => panic!("{name}: {error} ({}), expected {expected:?}", error.code()),
        }
    }
}
