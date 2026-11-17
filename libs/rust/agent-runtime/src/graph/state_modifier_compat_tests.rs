//! Jinja2 compatibility of the `state_modifier` template environment: Python
//! str and dict methods, and the Jinja2 text filters minijinja lacks.

use std::collections::HashMap;

use adk_graph::{ExecutionConfig, Node, NodeContext};
use serde_json::{Value, json};

use super::state_modifier::{StateModifierNode, StateModifierNodeDefinition};

/// Renders `template` over `input` the way a pipeline node does; `None` when
/// the node refuses the template.
async fn render(template: &str, input: Value) -> Option<Value> {
    let yaml = format!(
        "id: t\ntype: state_modifier\ntemplate: {}\ninput: [v]\noutput: [result]\n",
        serde_json::to_string(template).expect("template json")
    );
    let node =
        StateModifierNode::new(StateModifierNodeDefinition::from_yaml(&yaml).expect("definition"));
    let context = NodeContext::new(
        HashMap::from([
            ("v".to_owned(), input),
            ("result".to_owned(), Value::String(String::new())),
        ]),
        ExecutionConfig::new("state-modifier"),
        0,
    );
    node.execute(&context)
        .await
        .ok()
        .and_then(|output| output.updates.get("result").cloned())
}

#[tokio::test]
async fn python_str_methods_render_like_jinja2() {
    for (template, input, expected) in [
        ("{{ v.split(',') | join('|') }}", json!("a,b,c"), "a|b|c"),
        ("{{ v.split() | length }}", json!("  one two   three "), "3"),
        ("{{ v.split(',', 1) | join('|') }}", json!("a,b,c"), "a|b,c"),
        ("{{ v.rsplit(',', 1) | join('|') }}", json!("a,b,c"), "a,b|c"),
        ("{{ v.rsplit(None, 1) | join('|') }}", json!("a b c"), "a b|c"),
        ("{{ v.split(None, 1) | join('|') }}", json!("a b  "), "a|b  "),
        ("{{ v.replace(',', ';', 1) }}", json!("a,b,c"), "a;b,c"),
        ("{{ v.splitlines() | length }}", json!("x\ny\nz"), "3"),
        ("[{{ v.strip() }}]", json!("  hi \n"), "[hi]"),
        (
            "[{{ v.lstrip('x') }}][{{ v.rstrip('z') }}]",
            json!("xxhizz"),
            "[hizz][xxhi]",
        ),
        (
            "{{ v.title() }}/{{ v.capitalize() }}",
            json!("hello wORLD"),
            "Hello World/Hello world",
        ),
        (
            "{{ 'Y' if v.startswith('he') else 'N' }}{{ 'Y' if v.endswith('x') else 'N' }}",
            json!("hello"),
            "YN",
        ),
        ("{{ v.replace('l', 'L') }}", json!("hello"), "heLLo"),
        (
            "{{ v.find('l') }}/{{ v.find('z') }}/{{ v.count('l') }}",
            json!("hello"),
            "2/-1/2",
        ),
        ("{{ ', '.join(v.split(' ')) }}", json!("a b"), "a, b"),
        (
            "{{ 'Y' if v.isdigit() else 'N' }}{{ 'Y' if v.isalpha() else 'N' }}",
            json!("123"),
            "YN",
        ),
    ] {
        assert_eq!(
            render(template, input).await,
            Some(json!(expected)),
            "{template}"
        );
    }
}

#[tokio::test]
async fn python_dict_methods_render_like_jinja2() {
    let input = json!({"a": 1, "b": 2});
    for (template, expected) in [
        ("{{ v.keys() | join(',') }}", "a,b"),
        ("{{ v.values() | join(',') }}", "1,2"),
        (
            "{% for k, x in v.items() %}{{ k }}={{ x }};{% endfor %}",
            "a=1;b=2;",
        ),
        ("{{ v.get('a') }}/{{ v.get('z', 'dflt') }}", "1/dflt"),
    ] {
        assert_eq!(
            render(template, input.clone()).await,
            Some(json!(expected)),
            "{template}"
        );
    }
}

#[tokio::test]
async fn jinja2_text_filters_render_like_jinja2() {
    for (template, input, expected) in [
        (
            "{{ v | wordcount }}",
            json!("The quick brown fox, jumps!"),
            "5",
        ),
        ("{{ v | wordcount }}", json!(""), "0"),
        ("{{ v | capitalize }}", json!("hELLO"), "Hello"),
        ("{{ v | truncate(10) }}", json!("short"), "short"),
        (
            "{{ v | truncate(12, true) }}",
            json!("a long sentence that needs cutting"),
            "a long se...",
        ),
        (
            "{{ v | truncate(14) }}",
            json!("a long sentence that needs cutting"),
            "a long...",
        ),
        ("[{{ v | center(7) }}]", json!("ab"), "[   ab  ]"),
    ] {
        assert_eq!(
            render(template, input).await,
            Some(json!(expected)),
            "{template}"
        );
    }
}

#[tokio::test]
async fn unknown_python_methods_still_fail() {
    assert_eq!(render("{{ v.nonexistent() }}", json!("x")).await, None);
    assert_eq!(render("{{ v.split('') }}", json!("x")).await, None);
    // An argument that would be ignored is refused instead.
    assert_eq!(render("{{ v.find('x', 5) }}", json!("x")).await, None);
    assert_eq!(render("{{ v.replace('', 'y') }}", json!("x")).await, None);
}
