//! Unquoted integer graph identifiers (`id: 1`) and typed identifier refusals.
//!
//! The Web editor and older stored documents write numeric node ids. The Worker
//! normalizes an integer YAML scalar to its canonical decimal string before the
//! strict typed parse, so `id: 1` and `id: "1"` are the same pipeline.

use std::collections::HashMap;
use std::sync::Arc;

use adk_rust::graph::MemoryCheckpointer;
use adk_rust::session::{CreateRequest, InMemorySessionService, SessionService};
use adk_rust::{Content, Part, SessionId, UserId};

use super::agent::EliteaGraphAgent;
use super::compiler::PipelineDefinition;
use crate::agents::runtime::NativeAgentInvocation;

const INVALID_IDENTIFIER: &str = "graph.pipeline.invalid_identifier";

/// Render one node identifier either as an unquoted integer or as a string.
fn id(number: i64, quoted: bool) -> String {
    if quoted {
        format!("\"{number}\"")
    } else {
        number.to_string()
    }
}

/// Router, Decision, HITL, State Modifier and static interrupts: every graph
/// identifier position those families own.
fn core_pipeline(quoted: bool) -> String {
    let q = |number| id(number, quoted);
    format!(
        r"
state:
  messages: list
  choice: {{type: str, value: left}}
  answer: str
entry_point: {e}
interrupt_before: [{ib}]
interrupt_after: [{ia}]
nodes:
  - id: {e}
    type: state_modifier
    template: 'one'
    output: [answer]
    transition: {router}
  - id: {router}
    type: router
    condition: '{{{{ choice }}}}'
    routes: [{decision}, {hitl}]
    default_output: {tail}
    input: [choice]
  - id: {decision}
    type: decision
    nodes: [{hitl}, {tail}]
    default_output: {last}
  - id: {hitl}
    type: hitl
    routes:
      approve: {tail}
      reject: END
      edit: {last}
    edit_state_key: answer
  - id: {tail}
    type: state_modifier
    template: 'tail'
    output: [answer]
    transition: {last}
  - id: {last}
    type: state_modifier
    template: 'last'
    output: [answer]
    transition: END
",
        e = q(10),
        ib = q(14),
        ia = q(10),
        router = q(11),
        decision = q(12),
        hitl = q(13),
        tail = q(14),
        last = q(15),
    )
}

fn parallel_pipeline(quoted: bool) -> String {
    let q = |number| id(number, quoted);
    format!(
        r"
state:
  topic: string
  detail: string
  joined: list
entry_point: {gather}
nodes:
  - id: {gather}
    type: parallel
    branches: [{{id: {left}, node: {a}}}, {{id: {right}, node: {b}}}]
    max_concurrency: 2
    wait: all
    output: [joined]
    transition: {after}
  - id: {a}
    type: agent
    tool: Research Agent
    input: [topic]
    input_mapping:
      task: {{type: fixed, value: Research}}
    output: [detail]
  - id: {b}
    type: agent
    tool: Research Agent
    input: [topic]
    input_mapping:
      task: {{type: fixed, value: Compare}}
    output: [detail]
  - id: {after}
    type: state_modifier
    template: done
    transition: END
",
        gather = q(20),
        left = q(21),
        right = q(22),
        a = q(23),
        b = q(24),
        after = q(25),
    )
}

fn map_pipeline(quoted: bool) -> String {
    let q = |number| id(number, quoted);
    format!(
        "entry_point: {map}\nstate:\n  entities: {{type: list, value: []}}\n  item_result: {{type: str, value: ''}}\n  mapped: {{type: list, value: []}}\nnodes:\n  - {{id: {map}, type: map, worker: {worker}, source: entities, item: entity, index: item_index, outputs: [item_result], destination: mapped, max_items: 64, max_concurrency: 4, reduction: ordered_collection, transition: END}}\n  - {{id: {worker}, type: state_modifier, input: [entity, item_index], output: [item_result], template: '{{{{ item_index }}}}'}}\n",
        map = q(30),
        worker = q(31),
    )
}

fn recovery_pipeline(quoted: bool) -> String {
    let q = |number| id(number, quoted);
    format!(
        "state:\n  answer: int\n  node_error: dict\nentry_point: {run}\nnodes:\n  - id: {run}\n    type: code\n    code: '7'\n    output: [answer]\n    recovery:\n      on_failure:\n        route: {fallback}\n        error_input: node_error\n        classes: [invalid_input]\n    transition: END\n  - id: {fallback}\n    type: code\n    code: '8'\n    input: [node_error]\n    output: [answer]\n    failure_handler: {{error_input: node_error}}\n    transition: END\n",
        run = q(40),
        fallback = q(41),
    )
}

fn printer_pipeline(quoted: bool) -> String {
    let q = |number| id(number, quoted);
    format!(
        "state:\n  answer: str\nentry_point: {show}\nnodes:\n  - id: {show}\n    type: printer\n    input_mapping:\n      printer: {{type: variable, value: answer}}\n    transition: {after}\n  - id: {after}\n    type: state_modifier\n    template: done\n    transition: END\n",
        show = q(50),
        after = q(51),
    )
}

fn assert_numeric_matches_quoted(family: &str, render: fn(bool) -> String) {
    let numeric = PipelineDefinition::from_yaml(&render(false))
        .unwrap_or_else(|error| panic!("{family}: numeric ids must be admitted: {}", error.code()));
    let quoted = PipelineDefinition::from_yaml(&render(true))
        .unwrap_or_else(|error| panic!("{family}: quoted ids must be admitted: {}", error.code()));
    assert_eq!(numeric.entry_point(), quoted.entry_point());
    assert_eq!(numeric.node_count(), quoted.node_count());
    assert_eq!(
        numeric.definition_digest(),
        quoted.definition_digest(),
        "an unquoted integer id must have the same definition digest as the quoted id"
    );
}

#[test]
fn numeric_identifiers_in_every_core_position_equal_their_quoted_pipeline() {
    assert_numeric_matches_quoted("core_pipeline", core_pipeline);
}

#[test]
fn numeric_identifiers_equal_their_quoted_pipeline_for_printer_parallel_map_and_recovery() {
    assert_numeric_matches_quoted("printer_pipeline", printer_pipeline);
    assert_numeric_matches_quoted("parallel_pipeline", parallel_pipeline);
    assert_numeric_matches_quoted("map_pipeline", map_pipeline);
    assert_numeric_matches_quoted("recovery_pipeline", recovery_pipeline);
}

#[test]
fn numeric_identifier_definition_keeps_its_canonical_string_form() {
    let definition = PipelineDefinition::from_yaml(&core_pipeline(false)).expect("numeric");
    assert_eq!(definition.entry_point(), "10");
    let mixed = core_pipeline(false).replace("entry_point: 10", "entry_point: \"10\"");
    assert_eq!(
        PipelineDefinition::from_yaml(&mixed)
            .expect("mixed numeric and quoted ids")
            .definition_digest(),
        definition.definition_digest()
    );
}

#[tokio::test]
async fn numeric_identifier_pipeline_compiles_and_routes_by_the_canonical_string() {
    // Pure-code families only: Decision and LLM need claim-bound model authority.
    let yaml = r"
state:
  messages: list
  choice: {type: str, value: '3'}
  final_text: str
entry_point: 1
nodes:
  - id: 1
    type: state_modifier
    template: first
    output: [final_text]
    transition: 2
  - id: 2
    type: router
    condition: '{{ choice }}'
    routes: [3, 4]
    default_output: 4
    input: [choice]
  - id: 3
    type: state_modifier
    template: routed-to-three
    output: [final_text]
    transition: END
  - id: 4
    type: state_modifier
    template: routed-to-four
    output: [final_text]
    transition: END
";
    let definition = PipelineDefinition::from_yaml(yaml).expect("numeric router pipeline");
    let graph = definition
        .compile(
            "numeric-pipeline",
            Arc::new(MemoryCheckpointer::new()),
            None,
        )
        .expect("numeric identifiers compile into the ADK graph");
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "elitea".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: Some("numeric-thread".to_owned()),
            state: HashMap::new(),
        })
        .await
        .expect("session");
    let session_service: Arc<dyn SessionService> = sessions;
    let runner = adk_rust::runner::Runner::builder()
        .app_name("elitea")
        .agent(Arc::new(EliteaGraphAgent::new(graph)))
        .session_service(session_service)
        .build()
        .expect("runner");
    let mut running = NativeAgentInvocation::new(
        runner,
        UserId::new("user-1").expect("user"),
        SessionId::new("numeric-thread").expect("session id"),
        Content::new("user").with_text("go"),
    )
    .start()
    .expect("invocation");
    let mut last = None;
    while let Some(event) = running.next_event().await.expect("event") {
        last = event.content().map(|content| {
            content
                .parts
                .iter()
                .filter_map(Part::text)
                .collect::<Vec<_>>()
                .join("")
        });
    }
    assert_eq!(last.as_deref(), Some("routed-to-three"));
}

#[test]
fn numeric_and_quoted_spellings_of_one_identifier_are_a_duplicate_node() {
    let yaml = "entry_point: 1\nnodes:\n  - id: 1\n    type: state_modifier\n    transition: END\n  - id: \"1\"\n    type: state_modifier\n    transition: END\n";
    let Err(error) = PipelineDefinition::from_yaml(yaml) else {
        panic!("a numeric id and its quoted twin are one node and must collide");
    };
    assert_eq!(error.code(), "graph.pipeline.invalid_configuration");
}

#[test]
fn integers_normalize_only_inside_the_javascript_safe_range() {
    let template =
        "entry_point: ID\nnodes:\n  - id: ID\n    type: state_modifier\n    transition: END\n";
    for (spelling, expected) in [
        ("9007199254740991", Some("9007199254740991")),
        ("-9007199254740991", Some("-9007199254740991")),
        ("0", Some("0")),
        ("9007199254740992", None),
        ("-9007199254740992", None),
        ("18446744073709551615", None),
    ] {
        let result = PipelineDefinition::from_yaml(&template.replace("ID", spelling));
        match (expected, result) {
            (Some(canonical), Ok(definition)) => assert_eq!(definition.entry_point(), canonical),
            (None, Err(error)) => assert_eq!(
                error.code(),
                INVALID_IDENTIFIER,
                "out-of-range integer {spelling} needs the typed identifier code"
            ),
            (Some(_), Err(error)) => panic!("{spelling} was refused: {}", error.code()),
            (None, Ok(_)) => panic!("{spelling} is outside the safe range and was admitted"),
        }
    }
}

#[test]
fn exotic_integer_spellings_normalize_to_the_recorded_canonical_strings() {
    let template =
        "entry_point: ID\nnodes:\n  - id: ID\n    type: state_modifier\n    transition: END\n";
    // Recorded behaviour of serde_yaml_ng (YAML 1.2 core schema). `010` and
    // `1_000` are plain strings there, so they stay strings; the others are ints.
    for (spelling, expected) in [
        ("0x1A", "26"),
        ("0o17", "15"),
        ("0b11", "3"),
        ("010", "010"),
        ("+1", "1"),
        ("-1", "-1"),
        ("1_000", "1_000"),
        ("-0", "0"),
        ("9007199254740991", "9007199254740991"),
    ] {
        let definition = PipelineDefinition::from_yaml(&template.replace("ID", spelling))
            .unwrap_or_else(|error| panic!("{spelling} was refused: {}", error.code()));
        assert_eq!(definition.entry_point(), expected, "spelling {spelling}");
    }
}

#[test]
fn non_integer_scalars_and_containers_in_identifier_positions_get_the_typed_code() {
    // Every position the normalizer owns, with a value that is not a string, an
    // integer or null. Each of these failed as a generic malformed YAML.
    let node = "\nnodes:\n  - id: a\n    type: state_modifier\n    transition: END\n";
    let base = format!("entry_point: a{node}");
    let cases: Vec<(String, &str)> = vec![
        (format!("entry_point: 1.0{node}"), "entry_point"),
        (format!("entry_point: true{node}"), "entry_point"),
        (format!("entry_point: [a]{node}"), "entry_point"),
        (format!("entry_point: {{k: a}}{node}"), "entry_point"),
        (format!("entry_point: !custom a{node}"), "entry_point"),
        (format!("entry_point: .inf{node}"), "entry_point"),
        (format!("entry_point: .nan{node}"), "entry_point"),
        (format!("entry_point: 1e3{node}"), "entry_point"),
        (base.replace("  - id: a", "  - id: 2.5"), "nodes[].id"),
        (
            base.replace("transition: END", "transition: false"),
            "nodes[].transition",
        ),
        (
            base.replace("entry_point: a", "entry_point: a\ninterrupt_before: [1.5]"),
            "interrupt_before[]",
        ),
        (
            base.replace("entry_point: a", "entry_point: a\ninterrupt_after: [true]"),
            "interrupt_after[]",
        ),
        (
            "entry_point: a\nnodes:\n  - id: a\n    type: router\n    routes: [1.5]\n".into(),
            "nodes[].routes[]",
        ),
        (
            "entry_point: a\nnodes:\n  - id: a\n    type: router\n    routes: [x]\n    default_output: 2.5\n".into(),
            "nodes[].default_output",
        ),
        (
            "entry_point: a\nnodes:\n  - id: a\n    type: decision\n    nodes: [true]\n".into(),
            "nodes[].nodes[]",
        ),
        (
            "entry_point: a\nnodes:\n  - id: a\n    type: hitl\n    routes:\n      approve: 1.5\n".into(),
            "nodes[].routes.*",
        ),
        (
            "entry_point: a\nnodes:\n  - {id: a, type: map, worker: 1.5, source: s, item: i, index: n, outputs: [o], destination: d, max_items: 1, max_concurrency: 1, reduction: ordered_collection}\n".into(),
            "nodes[].worker",
        ),
        (
            "entry_point: a\nnodes:\n  - {id: a, type: parallel, branches: [{id: 1.5, node: b}], max_concurrency: 1, wait: all, output: [o]}\n".into(),
            "nodes[].branches[].id",
        ),
        (
            "entry_point: a\nnodes:\n  - {id: a, type: parallel, branches: [{id: x, node: [b]}], max_concurrency: 1, wait: all, output: [o]}\n".into(),
            "nodes[].branches[].node",
        ),
        (
            "entry_point: a\nnodes:\n  - id: a\n    type: code\n    code: '1'\n    recovery:\n      on_failure: {route: 2.5, error_input: e, classes: [invalid_input]}\n".into(),
            "nodes[].recovery.on_failure.route",
        ),
    ];
    for (yaml, field) in cases {
        let Err(error) = PipelineDefinition::from_yaml(&yaml) else {
            panic!("a non-integer identifier was admitted for {field}");
        };
        assert_eq!(error.code(), INVALID_IDENTIFIER, "field {field}");
        let text = error.to_string();
        assert!(text.contains(field), "{text} must name {field}");
    }
}

#[test]
fn identifier_refusals_never_carry_the_offending_value() {
    let secret = "9007199254740993";
    let yaml = format!(
        "entry_point: {secret}\nnodes:\n  - id: a\n    type: state_modifier\n    template: 'secret-prompt-text'\n    transition: END\n"
    );
    let Err(error) = PipelineDefinition::from_yaml(&yaml) else {
        panic!("out-of-range id must be refused");
    };
    assert_eq!(error.code(), INVALID_IDENTIFIER);
    for rendered in [error.to_string(), format!("{error:?}")] {
        assert!(!rendered.contains(secret), "value leaked: {rendered}");
        assert!(!rendered.contains("secret-prompt-text"), "{rendered}");
    }
}

#[test]
fn null_and_absent_identifier_positions_keep_their_previous_outcomes() {
    // Optional position: null is today's `None` and must still be admitted.
    let ok = "entry_point: a\nnodes:\n  - id: a\n    type: state_modifier\n    transition: ~\n";
    PipelineDefinition::from_yaml(ok).expect("null transition stays an absent transition");
    let hitl_null = "entry_point: a\nnodes:\n  - id: a\n    type: hitl\n    routes:\n      approve: END\n      reject: ~\n";
    PipelineDefinition::from_yaml(hitl_null).expect("null HITL route stays absent");
    let null_interrupts = "entry_point: a\ninterrupt_before: ~\nnodes:\n  - id: a\n    type: state_modifier\n    transition: END\n";
    PipelineDefinition::from_yaml(null_interrupts).expect("null interrupt list is unchanged");

    // Positions that fail today keep failing with today's codes, never with
    // the new identifier code.
    for (yaml, code) in [
        (
            "entry_point: ~\nnodes:\n  - id: a\n    type: state_modifier\n    transition: END\n",
            "graph.pipeline.malformed_yaml",
        ),
        (
            "entry_point: a\nnodes:\n  - id: ~\n    type: state_modifier\n    transition: END\n",
            "graph.pipeline.invalid_configuration",
        ),
        (
            "entry_point: a\nnodes:\n  - id: a\n    type: router\n    default_output: ~\n",
            "graph.pipeline.invalid_configuration",
        ),
        (
            "entry_point: a\nnodes:\n  - id: a\n    type: decision\n    nodes: [~]\n",
            "graph.pipeline.invalid_configuration",
        ),
        (
            "entry_point: a\nnodes:\n  - id: a\n    type: router\n    routes: [~]\n",
            "graph.pipeline.invalid_configuration",
        ),
    ] {
        let Err(error) = PipelineDefinition::from_yaml(yaml) else {
            panic!("null in a required identifier position was admitted");
        };
        assert_eq!(error.code(), code, "{yaml}");
    }
}

#[test]
fn identifier_normalization_is_idempotent_and_legacy_labels_keep_their_rewrite() {
    let numeric = PipelineDefinition::from_yaml(&core_pipeline(false)).expect("numeric");
    let quoted = PipelineDefinition::from_yaml(&core_pipeline(true)).expect("quoted");
    assert_eq!(numeric.definition_digest(), quoted.definition_digest());
    let legacy = "entry_point: Agent 1\nnodes:\n  - id: Agent 1\n    type: state_modifier\n    transition: END\n";
    assert_eq!(
        PipelineDefinition::from_yaml(legacy)
            .expect("legacy label")
            .entry_point(),
        "Agent1"
    );
}

const LOG_CHILD_ENV: &str = "ELITEA_IDENTIFIER_LOG_CHILD";

/// Numeric normalization logs counts only, never identifier or template text.
/// A child process owns a global subscriber, so parallel tests cannot
/// change callsite interest while the logs are captured.
#[test]
fn numeric_normalization_is_logged_as_counts_only() {
    let output = std::process::Command::new(std::env::current_exe().expect("test binary"))
        .args([
            "--exact",
            "agents::graph::compiler_identifier_tests::numeric_normalization_log_child",
            "--nocapture",
            "--test-threads=1",
        ])
        .env(LOG_CHILD_ENV, "1")
        .output()
        .expect("log child process");
    let logged = String::from_utf8_lossy(&output.stdout);
    assert!(
        output.status.success(),
        "{logged}\n{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert!(logged.contains("CHILD-RAN"), "{logged}");
    assert!(
        logged.contains("pipeline_legacy_identifier_normalized"),
        "{logged}"
    );
    assert!(logged.contains("numeric_identifier_count=2"), "{logged}");
    assert!(!logged.contains("7777"), "an identifier leaked: {logged}");
    assert!(!logged.contains("private-template-text"), "{logged}");
}

#[test]
fn numeric_normalization_log_child() {
    if std::env::var(LOG_CHILD_ENV).is_err() {
        return;
    }
    tracing_subscriber::fmt()
        .without_time()
        .with_ansi(false)
        .with_target(false)
        .with_writer(std::io::stdout)
        .init();
    let yaml = "entry_point: 7777\nnodes:\n  - id: 7777\n    type: state_modifier\n    template: 'private-template-text'\n    transition: END\n";
    PipelineDefinition::from_yaml(yaml).expect("numeric pipeline");
    println!("CHILD-RAN");
}
