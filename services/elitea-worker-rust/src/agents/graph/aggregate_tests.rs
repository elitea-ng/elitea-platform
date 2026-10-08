use std::collections::{BTreeSet, HashMap};

use adk_rust::graph::{ExecutionConfig, GraphError, Node, NodeContext};
use serde_json::{Value, json};

use super::aggregate::{AggregateNode, AggregateNodeDefinition, run};
use super::data_shaping::{ShapingCode, ShapingConfigurationError};

const HEAD: &str = "id: summarize\ntype: aggregate\nsource: rows\noutput: [summary]\n";
const SENTINEL: &str = "SECRET-SENTINEL-42";

fn value(text: &str) -> Value {
    serde_json::from_str(text).expect("json literal")
}

fn parse(body: &str) -> Result<AggregateNodeDefinition, ShapingConfigurationError> {
    AggregateNodeDefinition::from_yaml(&format!("{HEAD}{body}"))
}

fn definition(body: &str) -> AggregateNodeDefinition {
    parse(body).unwrap_or_else(|error| panic!("{body:?} must compile: {error}"))
}

fn run_ok(body: &str, source: &str) -> Value {
    run(&definition(body), Some(&value(source)))
        .unwrap_or_else(|error| panic!("{body:?} over {source} failed: {}", error.message()))
}

fn run_err(body: &str, source: &str) -> String {
    let error = run(&definition(body), Some(&value(source))).expect_err("must fail");
    let message = error.message();
    assert!(!message.contains(SENTINEL), "{message}");
    message
}

fn envelope(parent: u64, position: u64, data: &Value) -> Value {
    json!({"parent_index": parent, "position": position, "data": data})
}

// ---------------------------------------------------------------- configuration

#[test]
fn every_key_is_accepted_and_exposed() {
    let parsed = AggregateNodeDefinition::from_yaml(
        r"
id: summarize
type: aggregate
source: order_lines
output: [summary]
layout: split_out
regroup: none
group_by:
  - {path: /line/sku, output: sku, missing: error}
  - {path: /line/kind, output: kind, missing: null}
operations:
  - {operation: count_rows, output: lines}
  - {operation: collect_rows, output: rows, retain: {mode: except, fields: [secret]}}
  - {operation: collect, output: all, field: {path: /line/qty, missing: skip, null: skip}, merge_lists: false}
  - {operation: sum_int, output: quantity, field: {path: /line/qty, missing: null, null: skip}}
  - {operation: min_int, output: low, field: {path: /line/qty, null: error}}
  - {operation: max_int, output: high, field: {path: /line/qty}}
  - {operation: first, output: head, field: {path: /line/qty, null: keep}}
  - {operation: last, output: tail, field: {path: /line/qty, missing: error}}
limits: {input_items: 10, output_items: 10, groups: 5, bytes: 1000, depth: 8, values: 100}
transition: process
",
    )
    .expect("full definition");
    assert_eq!(parsed.id(), "summarize");
    assert_eq!(parsed.input_keys(), &["order_lines"]);
    assert_eq!(parsed.output_keys(), &["summary"]);
    assert_eq!(parsed.transition(), Some("process"));

    let minimal = definition("operations: [{operation: count_rows, output: n}]\n");
    assert_eq!(minimal.input_keys(), &["rows"]);
    assert_eq!(minimal.transition(), None);
    assert_eq!(
        definition("operations: [{operation: count_rows, output: n}]\ntransition: END\n")
            .transition(),
        Some("END")
    );
}

#[test]
fn boundaries_and_optional_policies_are_accepted() {
    let names = (0..64).map(|index| format!("g{index}")).collect::<Vec<_>>();
    let group_by = names
        .iter()
        .map(|name| format!("{{path: /{name}, output: {name}}}"))
        .collect::<Vec<_>>()
        .join(", ");
    let operations = (0..64)
        .map(|index| format!("{{operation: count_rows, output: o{index}}}"))
        .collect::<Vec<_>>()
        .join(", ");
    definition(&format!(
        "group_by: [{group_by}]\noperations: [{operations}]\n"
    ));
    for body in [
        "operations: [{operation: sum_int, output: s, field: {path: /v, null: skip}}]\n",
        "operations: [{operation: sum_int, output: s, field: {path: /v, null: error}}]\n",
        "operations: [{operation: collect, output: c, field: {path: /v, null: error}, merge_lists: true}]\n",
        "operations: [{operation: collect_rows, output: r, retain: {mode: all}}]\n",
        "operations: [{operation: collect_rows, output: r, retain: {mode: only, fields: [{path: /a, output: a}]}}]\n",
        "group_by: [{path: /k, output: k, missing: null}]\noperations: [{operation: count_rows, output: n}]\n",
        "layout: split_out\nregroup: parent\noperations: [{operation: collect, output: f, field: {path: /f}}]\n",
        "layout: split_out\nregroup: parent\ngroup_by: []\noperations: [{operation: count_rows, output: n}]\n",
        "layout: plain\nregroup: none\noperations: [{operation: count_rows, output: n}]\n",
    ] {
        definition(body);
    }
}

#[test]
#[allow(clippy::too_many_lines)] // One table keeps every configuration refusal visible.
fn invalid_configurations_are_refused() {
    let many_operations = (0..65)
        .map(|index| format!("{{operation: count_rows, output: o{index}}}"))
        .collect::<Vec<_>>()
        .join(", ");
    let many_groups = (0..65)
        .map(|index| format!("{{path: /g, output: g{index}}}"))
        .collect::<Vec<_>>()
        .join(", ");
    let oversized = format!(
        "{HEAD}operations: [{{operation: count_rows, output: n}}]\n#{}",
        "x".repeat(64 * 1024)
    );
    let ops = "operations: [{operation: count_rows, output: n}]\n";
    let cases: Vec<(String, &str)> = vec![
        (
            format!("id: summarize\ntype: split_out\nsource: rows\noutput: [summary]\n{ops}"),
            "type",
        ),
        (
            format!("id: bad/id\ntype: aggregate\nsource: rows\noutput: [summary]\n{ops}"),
            "id",
        ),
        (
            format!("id: summarize\ntype: aggregate\nsource: ''\noutput: [summary]\n{ops}"),
            "source",
        ),
        (
            format!("id: summarize\ntype: aggregate\nsource: rows\noutput: []\n{ops}"),
            "no output",
        ),
        (
            format!("id: summarize\ntype: aggregate\nsource: rows\noutput: [a, b]\n{ops}"),
            "two outputs",
        ),
        (
            format!("id: summarize\ntype: aggregate\nsource: rows\noutput: ['']\n{ops}"),
            "empty output",
        ),
        (
            format!("id: summarize\ntype: aggregate\nsource: rows\noutput: [rows]\n{ops}"),
            "output is source",
        ),
        (
            format!("id: summarize\ntype: aggregate\noutput: [summary]\n{ops}"),
            "missing source",
        ),
        (format!("{HEAD}{ops}transition: bad/id\n"), "transition"),
        (format!("{HEAD}{ops}unknown: 1\n"), "unknown key"),
        (format!("{HEAD}{ops}layout: rows\n"), "layout"),
        (format!("{HEAD}{ops}regroup: child\n"), "regroup"),
        (
            format!("{HEAD}{ops}regroup: parent\n"),
            "regroup with plain layout",
        ),
        (
            format!(
                "{HEAD}{ops}layout: split_out\nregroup: parent\ngroup_by: [{{path: /k, output: k}}]\n"
            ),
            "regroup with group_by",
        ),
        (
            format!(
                "{HEAD}layout: split_out\nregroup: parent\noperations: [{{operation: collect_rows, output: r}}]\n"
            ),
            "regroup with collect_rows",
        ),
        (
            format!("{HEAD}{ops}group_by: [{{path: /k, output: k, missing: skip}}]\n"),
            "group_by skip",
        ),
        (
            format!("{HEAD}{ops}group_by: [{{path: k, output: k}}]\n"),
            "group_by pointer",
        ),
        (
            format!("{HEAD}{ops}group_by: [{{path: /k, output: ''}}]\n"),
            "group_by name",
        ),
        (
            format!("{HEAD}{ops}group_by: [{{path: /k, output: k, extra: 1}}]\n"),
            "group_by unknown key",
        ),
        (
            format!("{HEAD}{ops}group_by: [{many_groups}]\n"),
            "65 group_by",
        ),
        (HEAD.to_owned(), "no operations"),
        (format!("{HEAD}operations: []\n"), "empty operations"),
        (
            format!("{HEAD}operations: [{many_operations}]\n"),
            "65 operations",
        ),
        (
            format!("{HEAD}operations: [{{operation: median, output: m}}]\n"),
            "unknown operation",
        ),
        (
            format!("{HEAD}operations: [{{operation: count_rows, output: n, extra: 1}}]\n"),
            "operation unknown key",
        ),
        (
            format!("{HEAD}operations: [{{operation: count_rows, output: ''}}]\n"),
            "operation name",
        ),
        (
            format!(
                "{HEAD}operations: [{{operation: count_rows, output: n, field: {{path: /v}}}}]\n"
            ),
            "count_rows field",
        ),
        (
            format!(
                "{HEAD}operations: [{{operation: collect_rows, output: r, field: {{path: /v}}}}]\n"
            ),
            "collect_rows field",
        ),
        (
            format!("{HEAD}operations: [{{operation: collect, output: c}}]\n"),
            "collect without field",
        ),
        (
            format!("{HEAD}operations: [{{operation: sum_int, output: s}}]\n"),
            "sum_int without field",
        ),
        (
            format!("{HEAD}operations: [{{operation: first, output: f}}]\n"),
            "first without field",
        ),
        (
            format!("{HEAD}operations: [{{operation: collect, output: c, field: {{path: v}}}}]\n"),
            "field pointer",
        ),
        (
            format!(
                "{HEAD}operations: [{{operation: collect, output: c, field: {{path: /v, extra: 1}}}}]\n"
            ),
            "field unknown key",
        ),
        (
            format!(
                "{HEAD}operations: [{{operation: collect, output: c, field: {{path: /v, null: drop}}}}]\n"
            ),
            "field null policy",
        ),
        (
            format!(
                "{HEAD}operations: [{{operation: sum_int, output: s, field: {{path: /v, null: keep}}}}]\n"
            ),
            "sum_int keep",
        ),
        (
            format!(
                "{HEAD}operations: [{{operation: min_int, output: s, field: {{path: /v, null: keep}}}}]\n"
            ),
            "min_int keep",
        ),
        (
            format!(
                "{HEAD}operations: [{{operation: max_int, output: s, field: {{path: /v, null: keep}}}}]\n"
            ),
            "max_int keep",
        ),
        (
            format!(
                "{HEAD}operations: [{{operation: collect_rows, output: r, retain: {{mode: none}}}}]\n"
            ),
            "collect_rows retain none",
        ),
        (
            format!(
                "{HEAD}operations: [{{operation: collect, output: c, field: {{path: /v}}, retain: {{mode: all}}}}]\n"
            ),
            "retain on collect",
        ),
        (
            format!(
                "{HEAD}operations: [{{operation: sum_int, output: s, field: {{path: /v}}, merge_lists: true}}]\n"
            ),
            "merge_lists on sum_int",
        ),
        (
            format!(
                "{HEAD}operations: [{{operation: collect_rows, output: r, merge_lists: false}}]\n"
            ),
            "merge_lists on collect_rows",
        ),
        (
            format!(
                "{HEAD}operations: [{{operation: count_rows, output: n}}, {{operation: count_rows, output: n}}]\n"
            ),
            "duplicate operation output",
        ),
        (
            format!("{HEAD}{ops}group_by: [{{path: /a, output: k}}, {{path: /b, output: k}}]\n"),
            "duplicate group output",
        ),
        (
            format!("{HEAD}{ops}group_by: [{{path: /n, output: n}}]\n"),
            "cross-list duplicate",
        ),
        (format!("{HEAD}{ops}limits: {{groups: 0}}\n"), "zero limit"),
        (
            format!("{HEAD}{ops}limits: {{bytes: 524289}}\n"),
            "limit over ceiling",
        ),
        (format!("{HEAD}{ops}limits: {{rows: 1}}\n"), "unknown limit"),
        (oversized, "oversized yaml"),
    ];
    for (yaml, label) in cases {
        assert!(
            AggregateNodeDefinition::from_yaml(&yaml).is_err(),
            "{label} must be refused"
        );
    }

    let boundary = format!("{HEAD}operations: [{{operation: count_rows, output: n}}]\n");
    let padded = format!("{boundary}#{}", "x".repeat(64 * 1024 - boundary.len() - 1));
    assert_eq!(padded.len(), 64 * 1024);
    assert!(AggregateNodeDefinition::from_yaml(&padded).is_ok());
    assert!(matches!(
        AggregateNodeDefinition::from_yaml(&format!("{padded}x")),
        Err(ShapingConfigurationError::ResourceExhausted)
    ));
    assert!(matches!(
        parse(&format!("operations: [{many_operations}]\n")),
        Err(ShapingConfigurationError::ResourceExhausted)
    ));
    assert!(matches!(
        parse(&format!("{ops}regroup: parent\n")),
        Err(ShapingConfigurationError::Invalid(_))
    ));
    assert!(matches!(
        parse(&format!("{ops}unknown: 1\n")),
        Err(ShapingConfigurationError::MalformedYaml { .. })
    ));
}

// ---------------------------------------------------------------- worked examples

const SUMMARY: &str = "group_by: [{path: /sku, output: sku}]\noperations:\n  - {operation: count_rows, output: rows}\n  - {operation: sum_int, field: {path: /qty}, output: qty}\n";

#[test]
fn group_by_summary_matches_the_contract_example() {
    let message = run_err(
        SUMMARY,
        r#"[{"sku":"x","qty":2},{"sku":"y","qty":"1"},{"sku":"x","qty":3.0}]"#,
    );
    assert_eq!(
        message,
        "graph.shaping.type_mismatch: operations[1].field at item 1"
    );
    let output = run_ok(
        SUMMARY,
        r#"[{"sku":"x","qty":2},{"sku":"y","qty":1},{"sku":"x","qty":3.0}]"#,
    );
    assert_eq!(
        serde_json::to_string(&output).expect("serialize"),
        r#"[{"qty":5,"rows":2,"sku":"x"},{"qty":1,"rows":1,"sku":"y"}]"#
    );
}

#[test]
fn empty_input_follows_decision_u2() {
    let operations = "operations:\n  - {operation: count_rows, output: n}\n  - {operation: collect, field: {path: /v}, output: all}\n  - {operation: sum_int, field: {path: /v}, output: s}\n  - {operation: max_int, field: {path: /v}, output: m}\n  - {operation: first, field: {path: /v}, output: f}\n";
    assert_eq!(
        serde_json::to_string(&run_ok(operations, "[]")).expect("serialize"),
        r#"[{"all":[],"f":null,"m":null,"n":0,"s":0}]"#
    );
    let every = "operations:\n  - {operation: count_rows, output: n}\n  - {operation: collect_rows, output: r}\n  - {operation: collect, field: {path: /v}, output: c}\n  - {operation: sum_int, field: {path: /v}, output: s}\n  - {operation: min_int, field: {path: /v}, output: lo}\n  - {operation: max_int, field: {path: /v}, output: hi}\n  - {operation: first, field: {path: /v}, output: f}\n  - {operation: last, field: {path: /v}, output: l}\n";
    assert_eq!(
        run_ok(every, "[]"),
        json!([{"n": 0, "r": [], "c": [], "s": 0, "lo": null, "hi": null, "f": null, "l": null}])
    );
    assert_eq!(
        run_ok(&format!("{every}layout: split_out\n"), "[]"),
        json!([{"n": 0, "r": [], "c": [], "s": 0, "lo": null, "hi": null, "f": null, "l": null}])
    );
    assert_eq!(
        run_ok(
            &format!("{every}group_by: [{{path: /k, output: k}}]\n"),
            "[]"
        ),
        json!([])
    );
    assert_eq!(
        run_ok(
            "layout: split_out\nregroup: parent\noperations: [{operation: collect, field: {path: /f}, output: f}]\n",
            "[]"
        ),
        json!([])
    );
}

// ---------------------------------------------------------------- operations

#[test]
fn operations_apply_the_missing_and_null_policies() {
    let source = r#"[{"v":1},{"w":0},{"v":null},{"v":4}]"#;
    let op = |operation: &str, field: &str| {
        format!(
            "operations: [{{operation: {operation}, output: out, field: {{path: /v{field}}}}}]\n"
        )
    };
    let cases: Vec<(&str, &str, Result<Value, &str>)> = vec![
        (
            "collect",
            "",
            Err("missing_field: operations[0].field at item 1"),
        ),
        ("collect", ", missing: null", Ok(json!([1, null, null, 4]))),
        ("collect", ", missing: skip", Ok(json!([1, null, 4]))),
        ("collect", ", missing: skip, null: skip", Ok(json!([1, 4]))),
        (
            "collect",
            ", missing: skip, null: error",
            Err("null_value: operations[0].field at item 2"),
        ),
        (
            "collect",
            ", missing: null, null: error",
            Err("null_value: operations[0].field at item 1"),
        ),
        ("first", ", missing: skip", Ok(json!(1))),
        ("first", ", missing: null", Ok(json!(1))),
        ("last", ", missing: skip", Ok(json!(4))),
        ("last", ", missing: skip, null: skip", Ok(json!(4))),
        (
            "last",
            "",
            Err("missing_field: operations[0].field at item 1"),
        ),
        (
            "sum_int",
            ", missing: skip",
            Err("null_value: operations[0].field at item 2"),
        ),
        ("sum_int", ", missing: skip, null: skip", Ok(json!(5))),
        ("sum_int", ", missing: null, null: skip", Ok(json!(5))),
        (
            "sum_int",
            ", missing: null",
            Err("null_value: operations[0].field at item 1"),
        ),
        ("min_int", ", missing: skip, null: skip", Ok(json!(1))),
        ("max_int", ", missing: skip, null: skip", Ok(json!(4))),
        (
            "max_int",
            ", missing: skip, null: error",
            Err("null_value: operations[0].field at item 2"),
        ),
        (
            "min_int",
            "",
            Err("missing_field: operations[0].field at item 1"),
        ),
    ];
    for (operation, field, expected) in cases {
        let body = op(operation, field);
        match expected {
            Ok(expected) => assert_eq!(
                run_ok(&body, source),
                json!([{ "out": expected }]),
                "{operation}{field}"
            ),
            Err(expected) => assert_eq!(
                run_err(&body, source),
                format!("graph.shaping.{expected}"),
                "{operation}{field}"
            ),
        }
    }

    let nulls = r#"[{"v":null},{"v":2},{"v":null}]"#;
    assert_eq!(run_ok(&op("first", ""), nulls), json!([{"out": null}]));
    assert_eq!(run_ok(&op("last", ""), nulls), json!([{"out": null}]));
    assert_eq!(
        run_ok(&op("first", ", null: skip"), nulls),
        json!([{"out": 2}])
    );
    assert_eq!(
        run_ok(&op("last", ", null: skip"), nulls),
        json!([{"out": 2}])
    );
    assert_eq!(
        run_ok(&op("min_int", ", null: skip"), r#"[{"v":null}]"#),
        json!([{"out": null}])
    );
    assert_eq!(
        run_ok(&op("sum_int", ", missing: skip"), r#"[{"w":1}]"#),
        json!([{"out": 0}])
    );
}

#[test]
fn count_collect_rows_and_merge_lists_work_per_group() {
    let body = "group_by: [{path: /k, output: k, missing: null}]\noperations:\n  - {operation: count_rows, output: n}\n  - {operation: collect_rows, output: all}\n  - {operation: collect_rows, output: some, retain: {mode: only, fields: [{path: /v, output: value, missing: skip}]}}\n  - {operation: collect_rows, output: rest, retain: {mode: except, fields: [k]}}\n  - {operation: collect, output: merged, field: {path: /v, missing: skip}, merge_lists: true}\n";
    let output = run_ok(
        body,
        r#"[{"k":"a","v":[1,2]},{"v":[]},{"k":"a","v":[3]},{"k":null,"x":1}]"#,
    );
    assert_eq!(
        output,
        json!([
            {"k": "a", "n": 2,
             "all": [{"k": "a", "v": [1, 2]}, {"k": "a", "v": [3]}],
             "some": [{"value": [1, 2]}, {"value": [3]}],
             "rest": [{"v": [1, 2]}, {"v": [3]}],
             "merged": [1, 2, 3]},
            {"k": null, "n": 2,
             "all": [{"v": []}, {"k": null, "x": 1}],
             "some": [{"value": []}, {}],
             "rest": [{"v": []}, {"x": 1}],
             "merged": []}
        ])
    );
    assert_eq!(
        run_err(
            "operations: [{operation: collect, output: m, field: {path: /v}, merge_lists: true}]\n",
            r#"[{"v":[1]},{"v":"SECRET-SENTINEL-42"}]"#
        ),
        "graph.shaping.type_mismatch: operations[0].field at item 1"
    );
    assert_eq!(
        run_err(
            "operations: [{operation: collect, output: m, field: {path: /v}, merge_lists: true}]\n",
            r#"[{"v":null}]"#
        ),
        "graph.shaping.type_mismatch: operations[0].field at item 0"
    );
    assert_eq!(
        run_err(
            "operations: [{operation: collect_rows, output: r, retain: {mode: only, fields: [{path: /a, output: a}, {path: /b, output: b}]}}]\n",
            r#"[{"a":1,"b":2},{"a":1}]"#
        ),
        "graph.shaping.missing_field: operations[0].retain.fields[1] at item 1"
    );
}

#[test]
fn rows_are_validated_for_the_layout() {
    let count = "operations: [{operation: count_rows, output: n}]\n";
    assert_eq!(
        run_err(count, r#"[{}, "SECRET-SENTINEL-42"]"#),
        "graph.shaping.invalid_row: source at item 1"
    );
    let split = format!("{count}layout: split_out\n");
    for bad in [
        r#"{"parent_index":0,"position":0}"#,
        r#"{"parent_index":0,"position":0,"data":[]}"#,
        r#"{"parent_index":-1,"position":0,"data":{}}"#,
        r#"{"parent_index":0,"position":0.5,"data":{}}"#,
        r#"{"parent_index":0,"position":0,"data":{},"x":"SECRET-SENTINEL-42"}"#,
        r#"{"a":1}"#,
    ] {
        assert_eq!(
            run_err(
                &split,
                &format!(r#"[{{"parent_index":0,"position":0,"data":{{}}}},{bad}]"#)
            ),
            "graph.shaping.invalid_envelope: source at item 1",
            "{bad}"
        );
    }
    let by_data = "layout: split_out\ngroup_by: [{path: /sku, output: sku}]\noperations: [{operation: count_rows, output: n}, {operation: collect_rows, output: rows}]\n";
    assert_eq!(
        run_ok(
            by_data,
            r#"[{"parent_index":3,"position":0,"data":{"sku":"x"}},{"parent_index":3,"position":1,"data":{"sku":"x","q":1}}]"#
        ),
        json!([{"sku": "x", "n": 2, "rows": [{"sku": "x"}, {"q": 1, "sku": "x"}]}])
    );
    for source in [
        None,
        Some(json!({"a": 1})),
        Some(json!("rows")),
        Some(Value::Null),
    ] {
        let error = run(&definition(count), source.as_ref()).expect_err("invalid source");
        assert_eq!(error.code(), ShapingCode::InvalidSource);
        assert_eq!(error.message(), "graph.shaping.invalid_source: source");
    }
}

// ---------------------------------------------------------------- integers

#[test]
fn integers_are_exact_and_checked() {
    let sum = "operations: [{operation: sum_int, output: s, field: {path: /v}}]\n";
    assert_eq!(
        run_ok(sum, r#"[{"v":2.0},{"v":1e2},{"v":10e-1},{"v":-0}]"#),
        json!([{"s": 103}])
    );
    assert_eq!(
        run_err(sum, r#"[{"v":1},{"v":2.5}]"#),
        "graph.shaping.type_mismatch: operations[0].field at item 1"
    );
    assert_eq!(
        run_err(sum, r#"[{"v":true}]"#),
        "graph.shaping.type_mismatch: operations[0].field at item 0"
    );
    assert_eq!(
        run_err(sum, r#"[{"v":9223372036854775807},{"v":1}]"#),
        "graph.shaping.integer_overflow: operations[0].field at item 1"
    );
    assert_eq!(
        run_ok(sum, r#"[{"v":9223372036854775807},{"v":-1}]"#),
        json!([{"s": i64::MAX - 1}])
    );
    assert_eq!(
        run_ok(sum, r#"[{"v":-9223372036854775808}]"#),
        json!([{"s": i64::MIN}])
    );
    assert_eq!(
        run_err(sum, r#"[{"v":18446744073709551615}]"#),
        "graph.shaping.integer_overflow: operations[0].field at item 0"
    );
    assert_eq!(
        run_err(sum, r#"[{"v":1e99999999999999999999}]"#),
        "graph.shaping.unsupported_number: operations[0].field at item 0"
    );
    let range = "operations:\n  - {operation: min_int, output: lo, field: {path: /v}}\n  - {operation: max_int, output: hi, field: {path: /v}}\n";
    assert_eq!(
        run_ok(range, r#"[{"v":-3},{"v":-10},{"v":-1.0}]"#),
        json!([{"lo": -10, "hi": -1}])
    );
    assert_eq!(
        serde_json::to_string(&run_ok(range, r#"[{"v":2.0},{"v":1e1}]"#)).expect("serialize"),
        r#"[{"hi":10,"lo":2}]"#
    );
    assert_eq!(
        run_err(range, r#"[{"v":1},{"v":"3"}]"#),
        "graph.shaping.type_mismatch: operations[0].field at item 1"
    );
}

// ---------------------------------------------------------------- grouping

#[test]
fn groups_use_canonical_equality_and_first_appearance() {
    let body = "group_by: [{path: /k, output: k}]\noperations: [{operation: count_rows, output: n}, {operation: collect, output: i, field: {path: /i}}]\n";
    let output = run_ok(
        body,
        r#"[{"k":1.0,"i":0},{"k":"1","i":1},{"k":1,"i":2},{"k":1e0,"i":3},{"k":10e-1,"i":4},
            {"k":{"a":1,"b":[2]},"i":5},{"k":{"b":[2.0],"a":1},"i":6},{"k":-0.0,"i":7},{"k":0,"i":8},
            {"k":"1","i":9},{"k":[1,2],"i":10},{"k":[2,1],"i":11}]"#,
    );
    assert_eq!(
        serde_json::to_string(&output).expect("serialize"),
        concat!(
            r#"[{"i":[0,2,3,4],"k":1.0,"n":4},{"i":[1,9],"k":"1","n":2},"#,
            r#"{"i":[5,6],"k":{"a":1,"b":[2]},"n":2},{"i":[7,8],"k":-0.0,"n":2},"#,
            r#"{"i":[10],"k":[1,2],"n":1},{"i":[11],"k":[2,1],"n":1}]"#
        )
    );

    let composite = "group_by: [{path: /a, output: a}, {path: /b, output: b, missing: null}]\noperations: [{operation: count_rows, output: n}]\n";
    assert_eq!(
        run_ok(
            composite,
            r#"[{"a":"x","b":"y"},{"a":"xy"},{"a":"x","b":"y"},{"a":"xy","b":null},{"a":"x"}]"#
        ),
        json!([
            {"a": "x", "b": "y", "n": 2},
            {"a": "xy", "b": null, "n": 2},
            {"a": "x", "b": null, "n": 1}
        ])
    );
    assert_eq!(
        run_err(composite, r#"[{"a":"x"},{"b":1}]"#),
        "graph.shaping.missing_field: group_by[0] at item 1"
    );
    assert_eq!(
        run_err(
            "group_by: [{path: /k, output: k}]\noperations: [{operation: count_rows, output: n}]\n",
            r#"[{"k":1e99999999999999999999}]"#
        ),
        "graph.shaping.unsupported_number: group_by[0] at item 0"
    );
}

// ---------------------------------------------------------------- limits

#[test]
fn limits_fail_at_the_first_violation() {
    let distinct = (0..1_001)
        .map(|index| format!(r#"{{"k":{index}}}"#))
        .collect::<Vec<_>>()
        .join(",");
    let group =
        "group_by: [{path: /k, output: k}]\noperations: [{operation: count_rows, output: n}]\n";
    assert_eq!(
        run_err(group, &format!("[{distinct}]")),
        "graph.shaping.limit_exceeded: limits.groups at item 1000"
    );
    assert_eq!(
        run_err(
            &format!("{group}limits: {{groups: 2}}\n"),
            r#"[{"k":1},{"k":2},{"k":1},{"k":3}]"#
        ),
        "graph.shaping.limit_exceeded: limits.groups at item 3"
    );
    assert_eq!(
        run_err(
            &format!("{group}limits: {{output_items: 1}}\n"),
            r#"[{"k":1},{"k":2}]"#
        ),
        "graph.shaping.limit_exceeded: limits.output_items at item 1"
    );
    assert_eq!(
        run_err(
            &format!("{group}limits: {{input_items: 2}}\n"),
            r#"[{"k":1},{"k":1},{"k":1}]"#
        ),
        "graph.shaping.limit_exceeded: limits.input_items"
    );

    let long = "x".repeat(30);
    let rows = (0..10)
        .map(|_| format!(r#"{{"v":"{long}"}}"#))
        .collect::<Vec<_>>()
        .join(",");
    let collect = "operations: [{operation: collect, output: c, field: {path: /v}}]\n";
    assert_eq!(
        run_err(
            &format!("{collect}limits: {{bytes: 100}}\n"),
            &format!("[{rows}]")
        ),
        "graph.shaping.limit_exceeded: limits.bytes at item 3"
    );
    let merged = "operations: [{operation: collect, output: c, field: {path: /v}, merge_lists: true}]\nlimits: {values: 4}\n";
    assert_eq!(
        run_err(merged, r#"[{"v":[1,2]},{"v":[3,4]}]"#),
        "graph.shaping.limit_exceeded: limits.values at item 1"
    );

    // Final rows are charged exactly; they are not input rows, so no item.
    assert_eq!(
        run_err(&format!("{collect}limits: {{depth: 3}}\n"), r#"[{"v":1}]"#),
        "graph.shaping.limit_exceeded: limits.depth"
    );
    assert_eq!(
        run_ok(&format!("{collect}limits: {{depth: 4}}\n"), r#"[{"v":1}]"#),
        json!([{"c": [1]}])
    );
    let counts =
        "operations: [{operation: count_rows, output: n}, {operation: count_rows, output: m}]\n";
    assert_eq!(
        run_err(&format!("{counts}limits: {{values: 3}}\n"), "[]"),
        "graph.shaping.limit_exceeded: limits.values"
    );
    assert_eq!(
        run_ok(&format!("{counts}limits: {{values: 4}}\n"), "[]"),
        json!([{"n": 0, "m": 0}])
    );
    let count = "operations: [{operation: count_rows, output: n}]\n";
    assert_eq!(
        run_ok(&format!("{count}limits: {{bytes: 9}}\n"), "[]"),
        json!([{"n": 0}])
    );
    assert_eq!(
        run_err(&format!("{count}limits: {{bytes: 8}}\n"), "[]"),
        "graph.shaping.limit_exceeded: limits.bytes"
    );
    assert_eq!(
        run_err(
            &format!("{group}limits: {{depth: 3}}\n"),
            r#"[{"k":1},{"k":{"a":1}}]"#
        ),
        "graph.shaping.limit_exceeded: limits.depth at item 1"
    );
}

// ---------------------------------------------------------------- regroup

const INVERSE: &str = "layout: split_out\nregroup: parent\noperations: [{operation: collect, field: {path: /items}, output: items}]\n";

/// The `rows_field` / `retain: all` / `remove_source` `SplitOut` of `orders`.
fn split_out(orders: &Value) -> Vec<Value> {
    let mut lines = Vec::new();
    for (parent, order) in orders.as_array().expect("orders").iter().enumerate() {
        let mut base = order.as_object().expect("order").clone();
        let items = base.remove("items").expect("items");
        for (position, item) in items.as_array().expect("list").iter().enumerate() {
            let mut data = base.clone();
            data.insert("items".to_owned(), item.clone());
            lines.push(envelope(
                parent as u64,
                position as u64,
                &Value::Object(data),
            ));
        }
    }
    lines
}

#[test]
fn regroup_restores_the_split_out_input() {
    let orders = json!([
        {"id": "A", "items": [1, {"sku": "x"}, [2]], "meta": {"tags": ["a", "b"]}},
        {"id": "B", "items": [3]},
        {"id": "C", "items": [null], "note": null}
    ]);
    let lines = split_out(&orders);
    let definition = definition(INVERSE);
    assert_eq!(
        run(&definition, Some(&Value::Array(lines.clone()))).expect("inverse"),
        orders
    );

    let mut shuffled = lines;
    shuffled.reverse();
    shuffled.swap(0, 2);
    assert_eq!(
        run(&definition, Some(&Value::Array(shuffled))).expect("shuffled"),
        orders
    );

    let gaps = json!([
        envelope(7, 4, &json!({"id": "B", "items": "late"})),
        envelope(2, 9, &json!({"id": "A", "items": "z"})),
        envelope(2, 0, &json!({"id": "A", "items": "a"})),
        envelope(7, 1, &json!({"id": "B", "items": "early"})),
    ]);
    assert_eq!(
        run(&definition, Some(&gaps)).expect("gaps"),
        json!([{"id": "A", "items": ["a", "z"]}, {"id": "B", "items": ["early", "late"]}])
    );
}

#[test]
fn regroup_applies_operations_in_position_order() {
    let body = "layout: split_out\nregroup: parent\noperations:\n  - {operation: count_rows, output: n}\n  - {operation: sum_int, field: {path: /qty}, output: total}\n  - {operation: last, field: {path: /line/sku}, output: last_sku}\n";
    let source = json!([
        envelope(0, 1, &json!({"order": 1, "qty": 2, "line": {"sku": "b"}})),
        envelope(0, 0, &json!({"order": 1.0, "qty": 3, "line": {"sku": "a"}})),
    ]);
    assert_eq!(
        serde_json::to_string(&run(&definition(body), Some(&source)).expect("regroup"))
            .expect("serialize"),
        r#"[{"last_sku":"b","n":2,"order":1.0,"total":5}]"#
    );
}

#[test]
fn regroup_failures_name_the_row_and_position() {
    let data = json!({"id": "A", "items": 1, "secret": SENTINEL});
    let conflict = json!([
        envelope(0, 0, &data),
        envelope(1, 0, &data),
        envelope(0, 1, &data),
        envelope(1, 0, &data),
        envelope(0, 0, &data)
    ]);
    let error = run(&definition(INVERSE), Some(&conflict)).expect_err("conflict");
    assert_eq!(
        error.message(),
        "graph.shaping.regroup_conflict: regroup at item 3 position 0"
    );

    let inconsistent = json!([
        envelope(0, 0, &data),
        envelope(0, 1, &json!({"id": "A", "items": 2, "secret": SENTINEL})),
        envelope(0, 2, &json!({"id": "B", "items": 3, "secret": SENTINEL})),
    ]);
    let message = run(&definition(INVERSE), Some(&inconsistent))
        .expect_err("inconsistent")
        .message();
    assert_eq!(
        message,
        "graph.shaping.regroup_inconsistent: regroup at item 2 position 2"
    );
    assert!(!message.contains(SENTINEL));
    let extra_field = json!([
        envelope(0, 0, &data),
        envelope(
            0,
            1,
            &json!({"id": "A", "items": 2, "secret": SENTINEL, "x": 1})
        )
    ]);
    assert_eq!(
        run(&definition(INVERSE), Some(&extra_field))
            .expect_err("extra")
            .message(),
        "graph.shaping.regroup_inconsistent: regroup at item 1 position 1"
    );
    let canonical = json!([
        envelope(0, 0, &json!({"id": 1, "items": 1})),
        envelope(0, 1, &json!({"id": 1.0, "items": 2})),
    ]);
    assert_eq!(
        run(&definition(INVERSE), Some(&canonical)).expect("canonical"),
        json!([{"id": 1, "items": [1, 2]}])
    );

    let collision = "layout: split_out\nregroup: parent\noperations: [{operation: collect, field: {path: /items}, output: items}, {operation: count_rows, output: id}]\n";
    let one = json!([envelope(5, 3, &data)]);
    assert_eq!(
        run(&definition(collision), Some(&one))
            .expect_err("collision")
            .message(),
        "graph.shaping.field_collision: operations[1].output at item 0 position 3"
    );

    let parents = json!([envelope(0, 0, &data), envelope(1, 0, &data)]);
    assert_eq!(
        run(
            &definition(&format!("{INVERSE}limits: {{groups: 1}}\n")),
            Some(&parents)
        )
        .expect_err("groups")
        .message(),
        "graph.shaping.limit_exceeded: limits.groups at item 1"
    );
    assert_eq!(
        run(
            &definition(INVERSE),
            Some(&json!([envelope(0, 0, &data), {"data": {}}]))
        )
        .expect_err("envelope")
        .message(),
        "graph.shaping.invalid_envelope: source at item 1"
    );
    assert_eq!(
        run(
            &definition(INVERSE),
            Some(&json!([envelope(0, 0, &json!({"id": "A"}))]))
        )
        .expect_err("missing")
        .message(),
        "graph.shaping.missing_field: operations[0].field at item 0"
    );
}

#[test]
fn regroup_compares_envelope_indices_by_exact_value() {
    let exact_indices = value(
        r#"[{"parent_index":2.0,"position":1e0,"data":{"id":"A","items":"b"}},
            {"parent_index":2,"position":0,"data":{"id":"A","items":"a"}},
            {"parent_index":20e-1,"position":1,"data":{"id":"A","items":"c"}}]"#,
    );
    assert_eq!(
        run(&definition(INVERSE), Some(&exact_indices))
            .expect_err("1e0 == 1")
            .message(),
        "graph.shaping.regroup_conflict: regroup at item 2 position 1"
    );
    let exact_indices = value(
        r#"[{"parent_index":2.0,"position":1e0,"data":{"id":"A","items":"b"}},
            {"parent_index":2,"position":0,"data":{"id":"A","items":"a"}}]"#,
    );
    assert_eq!(
        run(&definition(INVERSE), Some(&exact_indices)).expect("exact indices"),
        json!([{"id": "A", "items": ["a", "b"]}])
    );
}

#[test]
fn errors_never_carry_data_values_or_pointer_text() {
    let sentinel_pointer = format!(
        "group_by: [{{path: /{SENTINEL}, output: k}}]\noperations: [{{operation: count_rows, output: n}}]\n"
    );
    assert_eq!(
        run_err(&sentinel_pointer, &format!(r#"[{{"v":"{SENTINEL}"}}]"#)),
        "graph.shaping.missing_field: group_by[0] at item 0"
    );
    let typed =
        format!("operations: [{{operation: sum_int, output: s, field: {{path: /{SENTINEL}}}}}]\n");
    assert_eq!(
        run_err(&typed, &format!(r#"[{{"{SENTINEL}":"{SENTINEL}"}}]"#)),
        "graph.shaping.type_mismatch: operations[0].field at item 0"
    );
    let error = run(
        &definition(&typed),
        Some(&value(&format!(r#"[{{"{SENTINEL}":"{SENTINEL}"}}]"#))),
    )
    .expect_err("typed");
    match error.into_graph_error("summarize") {
        GraphError::NodeExecutionFailed { node, message } => {
            assert_eq!(node, "summarize");
            assert!(!message.contains(SENTINEL));
        }
        other => panic!("unexpected error: {other}"),
    }
}

// ---------------------------------------------------------------- digest

#[test]
fn config_digest_is_stable_and_field_sensitive() {
    let base = "operations: [{operation: sum_int, output: s, field: {path: /v}}]\n";
    assert_eq!(
        definition(base).config_digest(),
        definition(base).config_digest()
    );
    assert_eq!(
        definition(base).config_digest(),
        AggregateNodeDefinition::from_yaml(
            "operations: [{output: s, field: {path: /v}, operation: sum_int}]\noutput: [summary]\nsource: rows\ntype: aggregate\nid: summarize\n"
        )
        .expect("reordered")
        .config_digest()
    );
    let variants = [
        base.to_owned(),
        base.replace("output: s", "output: t"),
        base.replace("sum_int", "min_int"),
        base.replace("path: /v", "path: /w"),
        base.replace("/v}", "/v, missing: null}"),
        base.replace("/v}", "/v, missing: skip}"),
        base.replace("/v}", "/v, null: error}"),
        base.replace("/v}", "/v, null: skip}"),
        format!("{base}transition: END\n"),
        format!("{base}transition: next\n"),
        format!("{base}limits: {{bytes: 1000}}\n"),
        format!("{base}limits: {{bytes: 524288}}\n"),
        format!("{base}layout: split_out\n"),
        format!("{base}group_by: [{{path: /k, output: k}}]\n"),
        format!("{base}group_by: [{{path: /k, output: key}}]\n"),
        format!("{base}group_by: [{{path: /j, output: k}}]\n"),
        format!("{base}group_by: [{{path: /k, output: k, missing: null}}]\n"),
        "operations: [{operation: collect, output: s, field: {path: /v}}]\n".to_owned(),
        "operations: [{operation: collect, output: s, field: {path: /v, null: keep}}]\n".to_owned(),
        "operations: [{operation: collect, output: s, field: {path: /v}, merge_lists: false}]\n"
            .to_owned(),
        "operations: [{operation: collect, output: s, field: {path: /v}, merge_lists: true}]\n"
            .to_owned(),
        "operations: [{operation: collect_rows, output: s}]\n".to_owned(),
        "operations: [{operation: collect_rows, output: s, retain: {mode: all}}]\n".to_owned(),
        "operations: [{operation: collect_rows, output: s, retain: {mode: except, fields: [v]}}]\n"
            .to_owned(),
        "operations: [{operation: count_rows, output: s}]\n".to_owned(),
        "operations: [{operation: count_rows, output: s}, {operation: count_rows, output: t}]\n"
            .to_owned(),
        "layout: split_out\nregroup: parent\noperations: [{operation: count_rows, output: s}]\n"
            .to_owned(),
        "layout: split_out\noperations: [{operation: count_rows, output: s}]\n".to_owned(),
    ];
    let digests = variants
        .iter()
        .map(|body| definition(body).config_digest())
        .collect::<BTreeSet<_>>();
    assert_eq!(digests.len(), variants.len());
    for header in [
        "id: other\ntype: aggregate\nsource: rows\noutput: [summary]\n",
        "id: summarize\ntype: aggregate\nsource: lines\noutput: [summary]\n",
        "id: summarize\ntype: aggregate\nsource: rows\noutput: [result]\n",
    ] {
        let other =
            AggregateNodeDefinition::from_yaml(&format!("{header}{base}")).expect("variant");
        assert_ne!(
            other.config_digest(),
            definition(base).config_digest(),
            "{header}"
        );
    }
}

// ---------------------------------------------------------------- node

#[tokio::test]
async fn node_emits_exactly_one_update_to_its_output() {
    let node = AggregateNode::new(definition(SUMMARY));
    assert_eq!(node.name(), "summarize");
    let context = NodeContext::new(
        HashMap::from([
            (
                "rows".to_owned(),
                value(r#"[{"sku":"x","qty":2},{"sku":"x","qty":3}]"#),
            ),
            ("summary".to_owned(), json!([])),
        ]),
        ExecutionConfig::new("aggregate"),
        0,
    );
    let output = node.execute(&context).await.expect("aggregate");
    assert_eq!(output.updates.len(), 1);
    assert_eq!(
        output.updates.get("summary"),
        Some(&json!([{"sku": "x", "rows": 2, "qty": 5}]))
    );

    let failing = NodeContext::new(
        HashMap::from([("rows".to_owned(), json!({"sku": SENTINEL}))]),
        ExecutionConfig::new("aggregate-failure"),
        0,
    );
    match node.execute(&failing).await {
        Err(GraphError::NodeExecutionFailed { node, message }) => {
            assert_eq!(node, "summarize");
            assert_eq!(message, "graph.shaping.invalid_source: source");
        }
        Err(other) => panic!("unexpected error: {other}"),
        Ok(_) => panic!("an invalid source was accepted"),
    }
    let absent = NodeContext::new(HashMap::new(), ExecutionConfig::new("aggregate-absent"), 0);
    assert!(node.execute(&absent).await.is_err());
}
