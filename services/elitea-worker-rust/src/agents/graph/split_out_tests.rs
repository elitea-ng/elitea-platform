use std::collections::HashMap;

use adk_rust::graph::{ExecutionConfig, GraphError, Node, NodeContext};
use serde_json::{Value, json};

use super::data_shaping::ShapingCode;
use super::split_out::{SplitOutNode, SplitOutNodeDefinition, run};

const SENTINEL: &str = "SECRET-SENTINEL-42";

fn def(extra: &str) -> SplitOutNodeDefinition {
    SplitOutNodeDefinition::from_yaml(&yaml(extra)).expect("definition")
}

fn yaml(extra: &str) -> String {
    format!("id: split\ntype: split_out\nsource: src\noutput: [out]\n{extra}\n")
}

fn refused(extra: &str) -> bool {
    SplitOutNodeDefinition::from_yaml(&yaml(extra)).is_err()
}

fn ok(extra: &str, source: &Value) -> Value {
    run(&def(extra), Some(source)).expect("run")
}

fn fail(extra: &str, source: &Value) -> (ShapingCode, String) {
    let error = run(&def(extra), Some(source)).expect_err("must fail");
    (error.code(), error.message())
}

const ROWS: &str = "split: {mode: rows_field, path: /f}\ndestination: d";

#[test]
fn config_accepts_defaults_and_exposes_accessors() {
    let d = def("split: {mode: list}\ndestination: d\ntransition: next");
    assert_eq!(d.id(), "split");
    assert_eq!(d.input_keys(), ["src".to_owned()]);
    assert_eq!(d.output_keys(), ["out".to_owned()]);
    assert_eq!(d.transition(), Some("next"));
    assert_eq!(d.source_state_type(), "list");
    assert_eq!(def(ROWS).transition(), None);
    assert_eq!(def(ROWS).source_state_type(), "list");
    let row = def("split: {mode: row_field, path: /f}\ndestination: d");
    assert_eq!(row.source_state_type(), "dict");
    assert_eq!(
        def(&format!("{ROWS}\ntransition: END")).transition(),
        Some("END")
    );
}

#[test]
fn config_refusals() {
    let table = [
        "split: {mode: list}",                                   // no destination
        "split: {mode: list, path: /f}\ndestination: d",         // path on list
        "split: {mode: rows_field}\ndestination: d",             // path missing
        "split: {mode: row_field}\ndestination: d",              // path missing
        "split: {mode: rows_field, path: f}\ndestination: d",    // bad pointer
        "split: {mode: rows_field, path: /a~2}\ndestination: d", // bad escape
        "split: {mode: bogus, path: /f}\ndestination: d",        // bad mode
        "split: {mode: rows_field, path: /f, extra: 1}\ndestination: d", // unknown in split
        "split: {mode: rows_field, path: /f}\ndestination: ''",  // bad name
        "split: {mode: rows_field, path: /f}\ndestination: d\nbogus: 1", // unknown key
        "destination: d",                                        // no split
        "split: {mode: list}\ndestination: d\nretain: {mode: all}", // list + all
        "split: {mode: list}\ndestination: d\nretain: {mode: except}", // list + except
        "split: {mode: list}\ndestination: d\nretain: {mode: only}", // list + only
        "split: {mode: rows_field, path: /f}\ndestination: d\nretain: {mode: only, fields: [{path: /a, output: d}]}",
        "split: {mode: rows_field, path: /f}\ndestination: d\nmissing_list: skip",
        "split: {mode: rows_field, path: /f}\ndestination: d\nnull_list: null",
        "split: {mode: rows_field, path: /f}\ndestination: d\nremove_source: maybe",
        "split: {mode: rows_field, path: /f}\ndestination: d\nlimits: {groups: 5}",
        "split: {mode: rows_field, path: /f}\ndestination: d\nlimits: {bytes: 0}",
        "split: {mode: rows_field, path: /f}\ndestination: d\nlimits: {depth: 33}",
        "split: {mode: rows_field, path: /f}\ndestination: d\ntransition: 'bad id'",
    ];
    for extra in table {
        assert!(refused(extra), "should refuse: {extra}");
    }
    for text in [
        "id: 'bad id'\ntype: split_out\nsource: src\noutput: [out]\n",
        "id: s\ntype: aggregate\nsource: src\noutput: [out]\n",
        "id: s\ntype: split_out\nsource: ''\noutput: [out]\n",
        "id: s\ntype: split_out\nsource: src\noutput: []\n",
        "id: s\ntype: split_out\nsource: src\noutput: [a, b]\n",
        "id: s\ntype: split_out\nsource: src\noutput: [src]\n",
        "id: s\ntype: split_out\nsource: src\noutput: ['']\n",
    ] {
        let full = format!("{text}split: {{mode: list}}\ndestination: d\n");
        assert!(SplitOutNodeDefinition::from_yaml(&full).is_err(), "{text}");
    }
    assert!(SplitOutNodeDefinition::from_yaml("").is_err());
    let big = format!(
        "{}# {}\n",
        yaml("split: {mode: list}\ndestination: d"),
        "x".repeat(64 * 1024)
    );
    assert!(SplitOutNodeDefinition::from_yaml(&big).is_err());
}

#[test]
fn list_mode_emits_one_parent() {
    let out = ok(
        "split: {mode: list}\ndestination: d",
        &json!(["a", {"k": 1}, null]),
    );
    assert_eq!(
        out,
        json!([
            {"parent_index": 0, "position": 0, "data": {"d": "a"}},
            {"parent_index": 0, "position": 1, "data": {"d": {"k": 1}}},
            {"parent_index": 0, "position": 2, "data": {"d": null}},
        ])
    );
    assert_eq!(
        ok("split: {mode: list}\ndestination: d", &json!([])),
        json!([])
    );
}

#[test]
fn row_field_mode_is_parent_zero() {
    let extra = "split: {mode: row_field, path: /items}\ndestination: d\nretain: {mode: all}";
    let out = ok(extra, &json!({"id": 7, "items": [1, 2]}));
    assert_eq!(
        out,
        json!([
            {"parent_index": 0, "position": 0, "data": {"id": 7, "d": 1}},
            {"parent_index": 0, "position": 1, "data": {"id": 7, "d": 2}},
        ])
    );
}

#[test]
fn rows_field_with_several_parents_and_empty_lists() {
    let extra = format!("{ROWS}\nretain: {{mode: all}}");
    let out = ok(
        &extra,
        &json!([{"id": "A", "f": [1, 2]}, {"id": "B", "f": []}, {"id": "C", "f": [3]}]),
    );
    assert_eq!(
        out,
        json!([
            {"parent_index": 0, "position": 0, "data": {"id": "A", "d": 1}},
            {"parent_index": 0, "position": 1, "data": {"id": "A", "d": 2}},
            {"parent_index": 2, "position": 0, "data": {"id": "C", "d": 3}},
        ])
    );
}

#[test]
fn retain_modes_and_remove_source() {
    let source = json!([{"id": 1, "keep": "k", "drop": "x", "f": [10], "nested": {"z": 2}}]);
    let cases: [(&str, bool, Value); 8] = [
        ("{mode: none}", true, json!({"d": 10})),
        ("{mode: none}", false, json!({"d": 10})),
        (
            "{mode: all}",
            true,
            json!({"id": 1, "keep": "k", "drop": "x", "nested": {"z": 2}, "d": 10}),
        ),
        (
            "{mode: except, fields: [drop, nested]}",
            true,
            json!({"id": 1, "keep": "k", "d": 10}),
        ),
        (
            "{mode: except, fields: [drop, f]}",
            false,
            json!({"id": 1, "keep": "k", "nested": {"z": 2}, "d": 10}),
        ),
        (
            "{mode: only, fields: [{path: /keep, output: kept}, {path: /nested/z, output: z}]}",
            true,
            json!({"kept": "k", "z": 2, "d": 10}),
        ),
        ("{mode: only, fields: []}", true, json!({"d": 10})),
        (
            "{mode: only, fields: [{path: /f, output: all_f}]}",
            false,
            json!({"all_f": [10], "d": 10}),
        ),
    ];
    for (retain, remove, data) in cases {
        let extra = format!("{ROWS}\nretain: {retain}\nremove_source: {remove}");
        let out = ok(&extra, &source);
        assert_eq!(
            out,
            json!([{"parent_index": 0, "position": 0, "data": data}]),
            "{retain} {remove}"
        );
    }
    // remove_source true: `only` of the split field itself sees it removed
    let extra =
        format!("{ROWS}\nretain: {{mode: only, fields: [{{path: /f, output: g, missing: null}}]}}");
    assert_eq!(ok(&extra, &source)[0]["data"], json!({"g": null, "d": 10}));
}

#[test]
fn destination_may_replace_the_split_field() {
    let extra = "split: {mode: rows_field, path: /items}\ndestination: items\nretain: {mode: all}";
    let out = ok(extra, &json!([{"id": "A", "items": [1, 2]}]));
    assert_eq!(out[1]["data"], json!({"id": "A", "items": 2}));
}

#[test]
fn field_collision_cases() {
    let extra = "split: {mode: rows_field, path: /items}\ndestination: items\nretain: {mode: all}\nremove_source: false";
    let (code, message) = fail(extra, &json!([{"items": [1]}]));
    assert_eq!(code, ShapingCode::FieldCollision);
    assert_eq!(
        message,
        "graph.shaping.field_collision: destination at item 0"
    );
    // even when the list is empty, and for except
    let (code, _) = fail(extra, &json!([{"items": []}]));
    assert_eq!(code, ShapingCode::FieldCollision);
    let (code, message) = fail(
        &format!("{ROWS}\nretain: {{mode: except, fields: [x]}}"),
        &json!([{"d": 0, "f": []}, {"f": []}]),
    );
    assert_eq!(
        (code, message.as_str()),
        (
            ShapingCode::FieldCollision,
            "graph.shaping.field_collision: destination at item 0"
        )
    );
    // collision reported at the offending parent
    let (_, message) = fail(
        &format!("{ROWS}\nretain: {{mode: all}}"),
        &json!([{"f": [1]}, {"d": 1, "f": [1]}]),
    );
    assert!(message.ends_with("at item 1"), "{message}");
}

#[test]
fn nested_split_path_removes_only_the_leaf() {
    let extra = "split: {mode: rows_field, path: /a/b}\ndestination: d\nretain: {mode: all}";
    let out = ok(extra, &json!([{"a": {"b": [1], "c": 2}, "e": 3}]));
    assert_eq!(out[0]["data"], json!({"a": {"c": 2}, "e": 3, "d": 1}));
    // array slot removal
    let extra = "split: {mode: rows_field, path: /a/0}\ndestination: d\nretain: {mode: all}";
    let out = ok(extra, &json!([{"a": [[7], "keep"]}]));
    assert_eq!(out[0]["data"], json!({"a": ["keep"], "d": 7}));
}

#[test]
fn missing_and_null_lists() {
    let source = json!([{"id": 1}, {"id": 2, "f": null}, {"id": 3, "f": [9]}]);
    let (code, message) = fail(ROWS, &source);
    assert_eq!(
        (code, message.as_str()),
        (
            ShapingCode::MissingField,
            "graph.shaping.missing_field: split.path at item 0"
        )
    );
    let (code, message) = fail(&format!("{ROWS}\nmissing_list: empty"), &source);
    assert_eq!(
        (code, message.as_str()),
        (
            ShapingCode::NullValue,
            "graph.shaping.null_value: split.path at item 1"
        )
    );
    let out = ok(
        &format!("{ROWS}\nmissing_list: empty\nnull_list: empty"),
        &source,
    );
    assert_eq!(
        out,
        json!([{"parent_index": 2, "position": 0, "data": {"d": 9}}])
    );
    // null_list empty alone still fails on missing
    let (code, _) = fail(&format!("{ROWS}\nnull_list: empty"), &source);
    assert_eq!(code, ShapingCode::MissingField);
}

#[test]
fn scalar_at_path_is_a_type_mismatch_never_wrapped() {
    for bad in [json!("s"), json!(1), json!(true), json!({"a": 1})] {
        let (code, message) = fail(ROWS, &json!([{"f": [1]}, {"f": bad}]));
        assert_eq!(
            (code, message.as_str()),
            (
                ShapingCode::TypeMismatch,
                "graph.shaping.type_mismatch: split.path at item 1"
            )
        );
    }
    // empty policies do not mask type errors
    let (code, _) = fail(
        &format!("{ROWS}\nmissing_list: empty\nnull_list: empty"),
        &json!([{"f": 1}]),
    );
    assert_eq!(code, ShapingCode::TypeMismatch);
}

#[test]
fn retain_failures() {
    let all = format!("{ROWS}\nretain: {{mode: all}}");
    let (code, message) = fail(&all, &json!([{"f": [1]}, 5]));
    // non-object parent is a list failure first (missing) -> use list-in-array parent instead
    assert_eq!(code, ShapingCode::MissingField, "{message}");
    let nested = "split: {mode: rows_field, path: /0}\ndestination: d\nretain: {mode: all}";
    let (code, message) = fail(nested, &json!([[[1, 2]]]));
    assert_eq!(
        (code, message.as_str()),
        (
            ShapingCode::InvalidRow,
            "graph.shaping.invalid_row: retain at item 0"
        )
    );
    let only = |missing: &str| {
        format!(
            "{ROWS}\nretain: {{mode: only, fields: [{{path: /a, output: a}}, {{path: /b, output: b{missing}}}]}}"
        )
    };
    let source = json!([{"a": 1, "f": [1]}]);
    let (code, message) = fail(&only(""), &source);
    assert_eq!(
        (code, message.as_str()),
        (
            ShapingCode::MissingField,
            "graph.shaping.missing_field: retain.fields[1] at item 0"
        )
    );
    assert_eq!(
        ok(&only(", missing: 'null'"), &source)[0]["data"],
        json!({"a": 1, "b": null, "d": 1})
    );
    assert_eq!(
        ok(&only(", missing: skip"), &source)[0]["data"],
        json!({"a": 1, "d": 1})
    );
    // retained null stays null
    assert_eq!(
        ok(&only(""), &json!([{"a": 1, "b": null, "f": [1]}]))[0]["data"],
        json!({"a": 1, "b": null, "d": 1})
    );
}

#[test]
fn invalid_source_cases() {
    let list = "split: {mode: list}\ndestination: d";
    let row = "split: {mode: row_field, path: /f}\ndestination: d";
    for (extra, source) in [
        (list, Some(json!({"a": 1}))),
        (list, Some(json!("s"))),
        (list, None),
        (ROWS, Some(json!({"f": [1]}))),
        (ROWS, Some(Value::Null)),
        (row, Some(json!([1]))),
        (row, Some(Value::Null)),
        (row, None),
    ] {
        let error = run(&def(extra), source.as_ref()).expect_err("invalid source");
        assert_eq!(error.code(), ShapingCode::InvalidSource);
        assert_eq!(error.message(), "graph.shaping.invalid_source: source");
    }
}

#[test]
fn limits_input_output_bytes_values_depth() {
    let (code, message) = fail(
        &format!("{ROWS}\nlimits: {{input_items: 2}}"),
        &json!([{"f": []}, {"f": []}, {"f": []}]),
    );
    assert_eq!(
        (code, message.as_str()),
        (
            ShapingCode::LimitExceeded,
            "graph.shaping.limit_exceeded: limits.input_items"
        )
    );
    let (code, _) = fail(
        "split: {mode: list}\ndestination: d\nlimits: {input_items: 1}",
        &json!([1, 2]),
    );
    assert_eq!(code, ShapingCode::LimitExceeded);
    assert!(
        run(
            &def("split: {mode: row_field, path: /f}\ndestination: d\nlimits: {input_items: 1}"),
            Some(&json!({"f": [1]}))
        )
        .is_ok()
    );

    let big = json!({"f": (0..10_001).map(|n| n % 2).collect::<Vec<_>>()});
    let row_field = "split: {mode: row_field, path: /f}\ndestination: d";
    // Five values per row make the default `values` ceiling bind first.
    let (code, message) = fail(row_field, &big);
    assert_eq!(code, ShapingCode::LimitExceeded);
    assert!(
        message.contains("limits.values") && message.ends_with("position 6553"),
        "{message}"
    );
    let capped = format!("{row_field}\nlimits: {{output_items: 10000}}");
    let (_, message) = fail(&capped, &big);
    assert!(message.contains("limits.values"), "{message}");
    let (_, message) = fail(
        &format!("{ROWS}\nlimits: {{output_items: 2}}"),
        &json!([{"f": [1, 2]}, {"f": [3]}]),
    );
    assert!(
        message.contains("limits.output_items") && message.contains("at item 1 position 0"),
        "{message}"
    );

    let fat = json!([{"blob": "y".repeat(400 * 1024), "f": vec![0; 10_000]}]);
    let (code, message) = fail(&format!("{ROWS}\nretain: {{mode: all}}"), &fat);
    assert_eq!(code, ShapingCode::LimitExceeded);
    assert!(
        message.contains("limits.bytes")
            && (message.ends_with("position 0") || message.ends_with("position 1")),
        "{message}"
    );
    assert!(message.contains("at item 0"));

    let (_, message) = fail(
        &format!("{ROWS}\nlimits: {{values: 8}}"),
        &json!([{"f": [1, 2, 3]}]),
    );
    assert!(
        message.contains("limits.values") && message.ends_with("position 1"),
        "{message}"
    );

    let mut deep = json!(1);
    for _ in 0..40 {
        deep = json!([deep]);
    }
    let (_, message) = fail(ROWS, &json!([{"f": [deep]}]));
    assert!(
        message.contains("limits.depth") && message.ends_with("position 0"),
        "{message}"
    );
}

#[test]
fn errors_never_leak_data() {
    let leak = format!("/{SENTINEL}");
    let cases: [(String, Value); 5] = [
        (
            format!("split: {{mode: rows_field, path: '{leak}'}}\ndestination: d"),
            json!([{SENTINEL: "x", "other": SENTINEL}]),
        ),
        (
            format!("{ROWS}\nretain: {{mode: only, fields: [{{path: '{leak}', output: o}}]}}"),
            json!([{"f": [SENTINEL]}]),
        ),
        (ROWS.to_owned(), json!([{"f": SENTINEL}])),
        (ROWS.to_owned(), json!({SENTINEL: SENTINEL})),
        (
            format!("{ROWS}\nretain: {{mode: all}}\nremove_source: false\nlimits: {{bytes: 40}}"),
            json!([{"f": [SENTINEL], SENTINEL: SENTINEL}]),
        ),
    ];
    for (extra, source) in cases {
        let error = run(&def(&extra), Some(&source)).expect_err("fails");
        let message = error.message();
        assert!(message.starts_with("graph.shaping."), "{message}");
        assert!(!message.contains(SENTINEL), "{message}");
        let GraphError::NodeExecutionFailed { node, message } = error.into_graph_error("split")
        else {
            panic!("wrong error kind");
        };
        assert_eq!(node, "split");
        assert!(!message.contains(SENTINEL));
    }
}

#[test]
fn digest_is_stable_and_sensitive() {
    let base = "split: {mode: rows_field, path: /f}\ndestination: d\nretain: {mode: all}\nlimits: {bytes: 1000}\ntransition: next";
    assert_eq!(def(base).config_digest(), def(base).config_digest());
    let variants = [
        base.replace("rows_field", "row_field"),
        base.replace("/f", "/g"),
        base.replace("destination: d", "destination: e"),
        base.replace("mode: all", "mode: none"),
        base.replace("mode: all", "mode: except, fields: [a]"),
        base.replace("mode: all", "mode: only, fields: [{path: /a, output: a}]"),
        format!("{base}\nmissing_list: empty"),
        format!("{base}\nnull_list: empty"),
        format!("{base}\nremove_source: false"),
        base.replace("bytes: 1000", "bytes: 1001"),
        base.replace("limits: {bytes: 1000}", "limits: {depth: 3}"),
        base.replace("next", "other"),
        base.replace("\ntransition: next", ""),
    ];
    let reference = def(base).config_digest();
    let mut seen = vec![reference];
    for variant in &variants {
        let digest = def(variant).config_digest();
        assert!(!seen.contains(&digest), "collision for {variant}");
        seen.push(digest);
    }
    let other_id = SplitOutNodeDefinition::from_yaml(&base_with(
        "id: split2\ntype: split_out\nsource: src\noutput: [out]\n",
        base,
    ))
    .expect("def");
    assert_ne!(other_id.config_digest(), reference);
    let other_out = SplitOutNodeDefinition::from_yaml(&base_with(
        "id: split\ntype: split_out\nsource: src2\noutput: [out]\n",
        base,
    ))
    .expect("def");
    assert_ne!(other_out.config_digest(), reference);
    let authored = def("split: {mode: list}\ndestination: d\nlimits: {bytes: 524288}");
    let absent = def("split: {mode: list}\ndestination: d");
    assert_ne!(authored.config_digest(), absent.config_digest());
}

fn base_with(head: &str, tail: &str) -> String {
    format!("{head}{tail}\n")
}

#[tokio::test]
async fn node_emits_one_update_and_leaves_source_untouched() {
    let node = SplitOutNode::new(def(&format!("{ROWS}\nretain: {{mode: all}}")));
    assert_eq!(node.name(), "split");
    let source = json!([{"id": 1, "f": [5]}]);
    let context = NodeContext::new(
        HashMap::from([
            ("src".to_owned(), source.clone()),
            ("out".to_owned(), json!([])),
        ]),
        ExecutionConfig::new("split-out"),
        0,
    );
    let output = node.execute(&context).await.expect("split");
    assert_eq!(output.updates.len(), 1);
    assert_eq!(
        output.updates.get("out"),
        Some(&json!([{"parent_index": 0, "position": 0, "data": {"id": 1, "d": 5}}]))
    );
    assert_eq!(context.get("src"), Some(&source));

    let missing = NodeContext::new(HashMap::new(), ExecutionConfig::new("split-out"), 0);
    let Err(error) = node.execute(&missing).await else {
        panic!("no source must fail");
    };
    assert!(
        matches!(error, GraphError::NodeExecutionFailed { ref node, ref message }
        if node == "split" && message == "graph.shaping.invalid_source: source")
    );
}
