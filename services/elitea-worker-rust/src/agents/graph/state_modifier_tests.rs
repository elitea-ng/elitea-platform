use std::collections::HashMap;

use adk_rust::graph::{ExecutionConfig, Node, NodeContext};
use serde_json::{Value, json};

use super::state_modifier::{StateModifierNode, StateModifierNodeDefinition};

#[tokio::test]
async fn state_modifier_preserves_the_active_yaml_and_four_sdk_filters() {
    let definition = StateModifierNodeDefinition::from_yaml(
        r"
id: transform
type: state_modifier
template: >-
  {{ encoded | base64_to_string }}|
  {{ payload | from_json | length }}|
  {{ words | split_by_words(2) | join(';') }}|
  {{ csv | split_by_regex(',') | join(';') }}
input: [encoded, payload, words, csv]
output: [result]
variables_to_clean: [scratch, count, enabled]
transition: END
",
    )
    .expect("state modifier definition");
    let node = StateModifierNode::new(definition);
    let context = NodeContext::new(
        HashMap::from([
            ("encoded".to_owned(), json!("aGVsbG8=")),
            ("payload".to_owned(), json!(r#"{"a":1,"b":2}"#)),
            ("words".to_owned(), json!("one two three four")),
            ("csv".to_owned(), json!("a,b,c")),
            ("result".to_owned(), Value::String(String::new())),
            ("scratch".to_owned(), json!([1, 2])),
            ("count".to_owned(), json!(7)),
            ("enabled".to_owned(), json!(true)),
        ]),
        ExecutionConfig::new("state-modifier"),
        0,
    );

    let output = node.execute(&context).await.expect("state transformation");
    assert_eq!(
        output.updates.get("result"),
        Some(&json!("hello| 2| one two;three four| a;b;c"))
    );
    assert_eq!(output.updates.get("scratch"), Some(&json!([])));
    assert_eq!(output.updates.get("count"), Some(&json!(0)));
    assert_eq!(output.updates.get("enabled"), Some(&json!(false)));
}

#[tokio::test]
async fn state_modifier_projects_to_the_existing_state_type_and_fails_safely() {
    for (existing, template, expected) in [
        (json!({}), r#"{"answer":42}"#, json!({"answer": 42})),
        (json!([]), "[1,2]", json!([1, 2])),
        (json!(0), "42", json!(42)),
        (json!(0.0), "4.5", json!(4.5)),
        (json!(false), "YES", json!(true)),
        (Value::Null, "ignored", Value::Null),
    ] {
        let definition = StateModifierNodeDefinition::from_yaml(&format!(
            "id: transform\ntype: state_modifier\ntemplate: '{template}'\noutput: [result]\n"
        ))
        .expect("typed definition");
        let context = NodeContext::new(
            HashMap::from([("result".to_owned(), existing)]),
            ExecutionConfig::new("typed-state"),
            0,
        );
        let output = StateModifierNode::new(definition)
            .execute(&context)
            .await
            .expect("typed projection");
        assert_eq!(output.updates.get("result"), Some(&expected));
    }

    let invalid = StateModifierNodeDefinition::from_yaml(
        "id: transform\ntype: state_modifier\ntemplate: '{{ value | split_by_regex(\"[\") }}'\ninput: [value]\noutput: [result]\n",
    )
    .expect("invalid template is admitted without executing it");
    let context = NodeContext::new(
        HashMap::from([("value".to_owned(), json!("content"))]),
        ExecutionConfig::new("invalid-template"),
        0,
    );
    let Err(error) = StateModifierNode::new(invalid).execute(&context).await else {
        panic!("invalid regex was accepted");
    };
    assert_eq!(
        error.to_string(),
        "state modifier template evaluation failed"
    );
}

#[tokio::test]
async fn rendered_string_bound_includes_the_json_and_browser_envelopes() {
    let definition = StateModifierNodeDefinition::from_yaml(
        "id: transform\ntype: state_modifier\ntemplate: '{{ value }}'\ninput: [value]\noutput: [result]\n",
    )
    .expect("bounded definition");
    let admitted = NodeContext::new(
        HashMap::from([
            ("value".to_owned(), json!("x".repeat(8 * 1024 - 2))),
            ("result".to_owned(), json!("")),
        ]),
        ExecutionConfig::new("bounded-state"),
        0,
    );
    let output = StateModifierNode::new(definition.clone())
        .execute(&admitted)
        .await
        .expect("exact serialized value bound");
    assert_eq!(
        output.updates.get("result"),
        Some(&json!("x".repeat(8 * 1024 - 2)))
    );

    let exhausted = NodeContext::new(
        HashMap::from([
            ("value".to_owned(), json!("x".repeat(8 * 1024 - 1))),
            ("result".to_owned(), json!("")),
        ]),
        ExecutionConfig::new("exhausted-state"),
        0,
    );
    let Err(error) = StateModifierNode::new(definition).execute(&exhausted).await else {
        panic!("oversized serialized state value was accepted");
    };
    assert_eq!(
        error.to_string(),
        "state modifier output exceeds its resource bound"
    );
}

#[test]
fn state_modifier_definition_is_strict_bounded_and_digest_stable() {
    let first = StateModifierNodeDefinition::from_yaml(
        "id: transform\ntype: state_modifier\ntemplate: '{{ input }}'\ninput: []\noutput: [result]\ntransition: END\n",
    )
    .expect("definition");
    let second = StateModifierNodeDefinition::from_yaml(
        "id: transform\ntype: state_modifier\ntemplate: '{{ input }}'\noutput: [result]\ntransition: END\n",
    )
    .expect("equivalent default input");
    assert_eq!(first.input_keys(), &["messages"]);
    assert_eq!(first.config_digest(), second.config_digest());

    for yaml in [
        "id: bad/id\ntype: state_modifier\n",
        "id: transform\ntype: state_modifier\noutput: ['']\n",
        "id: transform\ntype: state_modifier\ninput: [input, input]\n",
        "id: transform\ntype: state_modifier\nunknown: true\n",
    ] {
        assert!(StateModifierNodeDefinition::from_yaml(yaml).is_err());
    }
}
