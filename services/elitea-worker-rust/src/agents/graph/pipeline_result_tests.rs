use std::collections::HashMap;

use adk_rust::graph::{ExecutionConfig, GraphError, Node, NodeContext, NodeOutput};
use async_trait::async_trait;
use serde_json::{Value, json};

use super::*;

fn text(value: &Value) -> Option<String> {
    render_state_value(value).map(RenderedResult::into_bounded_text)
}

fn fenced(body: &str) -> String {
    format!("```json\n{body}\n```")
}

#[test]
fn scalars_render_as_text_and_numbers_keep_their_lexical_form() {
    assert_eq!(
        text(&json!("plain answer")).as_deref(),
        Some("plain answer")
    );
    assert_eq!(text(&json!(5)).as_deref(), Some("5"));
    assert_eq!(text(&json!(true)).as_deref(), Some("true"));
    let lexical: Value = serde_json::from_str("1.50").expect("number");
    assert_eq!(text(&lexical).as_deref(), Some("1.50"));
    let huge: Value = serde_json::from_str("18446744073709551616").expect("number");
    assert_eq!(text(&huge).as_deref(), Some("18446744073709551616"));
    assert_eq!(text(&Value::Null), None);
}

#[test]
fn only_lists_of_content_block_objects_are_joined_as_text() {
    let blocks = json!([
        {"type": "text", "text": "Hello, "},
        {"type": "thinking", "thinking": "private"},
        {"text": "world", "index": 1}
    ]);
    assert_eq!(text(&blocks).as_deref(), Some("Hello, world"));
    let thinking_only = render_state_value(&json!([{"type": "thinking", "thinking": "x"}]))
        .expect("recognised content");
    assert!(thinking_only.is_blank());

    for records in [
        json!([{"id": "A", "items": [10, 20]}]),
        // A record can borrow the `text` type value without the `text` key.
        json!([{"type": "text", "content": "a SharePoint row"}]),
        // A bare text chunk that carries another field is a search hit.
        json!([{"text": "hit", "score": 1}]),
        // Plain strings in state are data, not message content.
        json!(["a", "b"]),
        json!([{"type": "text", "text": "mixed"}, {"id": 1}]),
        json!([]),
    ] {
        let expected = fenced(&serde_json::to_string_pretty(&records).expect("pretty"));
        assert_eq!(text(&records), Some(expected), "{records}");
    }
}

#[test]
fn objects_render_as_fenced_pretty_json() {
    assert_eq!(
        text(&json!({"b": 1, "a": [true]})).as_deref(),
        Some("```json\n{\n  \"a\": [\n    true\n  ],\n  \"b\": 1\n}\n```")
    );
}

#[test]
fn several_traced_keys_render_one_object_in_declared_order() {
    let state = HashMap::from([
        ("zeta".to_owned(), json!({"nested": ["x", 2]})),
        ("alpha".to_owned(), json!("two\nlines")),
        ("mid".to_owned(), Value::Null),
    ]);
    let keys = ["zeta".to_owned(), "alpha".to_owned(), "mid".to_owned()];
    let rendered = render_traced_keys(&state, &keys)
        .expect("one visible value")
        .into_bounded_text();
    assert_eq!(
        rendered,
        fenced(
            "{\n  \"zeta\": {\n    \"nested\": [\n      \"x\",\n      2\n    ]\n  },\n  \"alpha\": \"two\\nlines\",\n  \"mid\": null\n}"
        )
    );
    let body = rendered
        .strip_prefix("```json\n")
        .and_then(|body| body.strip_suffix("\n```"))
        .expect("fence");
    let parsed: Value = serde_json::from_str(body).expect("valid JSON");
    assert_eq!(
        parsed,
        json!({"zeta": {"nested": ["x", 2]}, "alpha": "two\nlines", "mid": null})
    );

    let blank = HashMap::from([
        ("zeta".to_owned(), json!("  ")),
        ("alpha".to_owned(), Value::Null),
    ]);
    assert!(render_traced_keys(&blank, &keys).is_none());
    let one = HashMap::from([("alpha".to_owned(), json!("only"))]);
    assert_eq!(
        render_traced_keys(&one, &["alpha".to_owned()]).map(RenderedResult::into_bounded_text),
        Some("only".to_owned())
    );
}

#[test]
fn oversized_results_are_truncated_on_a_char_boundary_with_a_notice() {
    let long = "é".repeat(MAX_PIPELINE_RESULT_BYTES);
    let total = long.len();
    let output = text(&json!(long)).expect("never dropped for size");
    assert!(output.len() <= MAX_PIPELINE_RESULT_BYTES);
    assert!(output.starts_with("éé"));
    assert!(output.ends_with(&format!(
        "of {total} bytes. The full value is in the run state."
    )));
    assert!(output.contains("\n\n… output truncated: "));

    let records = Value::Array(vec![json!({"payload": "x".repeat(1024)}); 1024]);
    let output = text(&records).expect("never dropped for size");
    assert!(output.len() <= MAX_PIPELINE_RESULT_BYTES);
    assert!(output.starts_with("```json\n[\n  {"));
    let (fenced_part, notice) = output.rsplit_once("\n\n").expect("notice line");
    assert!(fenced_part.ends_with("\n```"));
    assert!(notice.starts_with("… output truncated: "));
}

#[test]
fn traces_only_declared_outputs_that_the_node_wrote() {
    let outputs = ResultTraceOutputs::new(vec!["a".to_owned(), "b".to_owned()], true);
    let updates = |keys: &[&str]| {
        keys.iter()
            .map(|key| ((*key).to_owned(), json!(1)))
            .collect::<HashMap<_, _>>()
    };
    assert_eq!(
        outputs.trace_update("node", &updates(&["b", "scratch", "a"])),
        Some(json!({"node": "node", "keys": ["a", "b"], "messages": false}))
    );
    assert_eq!(
        outputs.trace_update("node", &updates(&["messages", "router_output"])),
        Some(json!({"node": "node", "keys": [], "messages": true}))
    );
    assert_eq!(outputs.trace_update("node", &updates(&["scratch"])), None);
    let undeclared_messages = ResultTraceOutputs::new(vec!["a".to_owned()], false);
    assert_eq!(
        undeclared_messages.trace_update("node", &updates(&["messages"])),
        None
    );
}

#[test]
fn reads_only_a_well_formed_trace_without_internal_keys() {
    let trace = |value: Value| {
        ResultTrace::from_state(&HashMap::from([(
            PIPELINE_RESULT_TRACE_STATE_KEY.to_owned(),
            value,
        )]))
    };
    let parsed = trace(json!({"node": "n", "keys": ["a", "messages", "b"], "messages": true}))
        .expect("trace");
    assert_eq!(parsed.keys(), ["a", "b"]);
    assert!(parsed.messages());
    assert!(trace(Value::Null).is_none());
    assert!(trace(json!({"node": "n", "keys": "a", "messages": false})).is_none());
    assert!(trace(json!({"node": "n", "keys": [1], "messages": false})).is_none());
    assert!(ResultTrace::from_state(&HashMap::new()).is_none());
}

struct Fixed(fn() -> Result<NodeOutput, GraphError>);

#[async_trait]
impl Node for Fixed {
    fn name(&self) -> &'static str {
        "fixed"
    }

    async fn execute(&self, _: &NodeContext) -> Result<NodeOutput, GraphError> {
        (self.0)()
    }
}

#[tokio::test]
async fn the_wrapper_adds_one_trace_update_and_passes_errors_and_interrupts_through() {
    let context = NodeContext::new(HashMap::new(), ExecutionConfig::new("trace"), 0);
    let wrap = |inner| {
        ResultTraceNode::new(
            Fixed(inner),
            ResultTraceOutputs::new(vec!["a".to_owned()], false),
        )
    };

    let wrote = wrap(|| Ok(NodeOutput::new().with_update("a", json!("value"))));
    assert_eq!(wrote.name(), "fixed");
    let output = wrote.execute(&context).await.expect("output");
    assert_eq!(output.updates.len(), 2);
    assert_eq!(
        output.updates.get(PIPELINE_RESULT_TRACE_STATE_KEY),
        Some(&json!({"node": "fixed", "keys": ["a"], "messages": false}))
    );

    let silent = wrap(|| Ok(NodeOutput::new().with_update("scratch", json!(""))));
    let output = silent.execute(&context).await.expect("output");
    assert!(!output.updates.contains_key(PIPELINE_RESULT_TRACE_STATE_KEY));

    let paused = wrap(|| Ok(NodeOutput::interrupt("wait").with_update("a", json!("partial"))));
    let output = paused.execute(&context).await.expect("interrupt output");
    assert!(output.interrupt.is_some());
    assert!(!output.updates.contains_key(PIPELINE_RESULT_TRACE_STATE_KEY));

    let failed = wrap(|| Err(GraphError::Other("failed".to_owned())));
    assert!(failed.execute(&context).await.is_err());
}
