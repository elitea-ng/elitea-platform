#![allow(
    clippy::disallowed_methods,
    reason = "inline test fixtures and the contract file, not stored documents"
)]

use std::collections::{BTreeMap, BTreeSet};

use serde_json::{Value, json};

use super::*;

fn parse(yaml: &str) -> Result<NodeSettings, NodeSettingsRefusal> {
    NodeSettings::parse(serde_yaml_ng::from_str(yaml).expect("fixture YAML"))
}

fn contract() -> Value {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../proto/contracts/pipeline-node-settings-v1.json");
    serde_json::from_str(&std::fs::read_to_string(path).expect("the settings contract"))
        .expect("the settings contract is JSON")
}

#[test]
fn an_empty_block_is_the_default_and_digests_nothing() {
    let settings = parse("{}").expect("empty block");
    assert!(settings.is_default());
    assert!(!settings.uses_node_errors());
    let explicit = parse("{on_error: stop, always_output_data: false, execute_once: false}")
        .expect("explicit defaults");
    assert!(explicit.is_default());
    assert_eq!(
        settings_digest(&BTreeMap::from([("a".to_owned(), explicit)])),
        None
    );
    assert_eq!(bind_digest([7; 32], None), [7; 32]);
}

#[test]
fn malformed_blocks_are_refused() {
    for yaml in [
        "[]",
        "true",
        "{unknown: 1}",
        "{max_tries: '3', retry_on_fail: true}",
        "{on_error: later}",
        "{execute_once: {nested: true}}",
        "{always_output_data: [true]}",
    ] {
        assert_eq!(parse(yaml), Err(NodeSettingsRefusal::Malformed), "{yaml}");
    }
}

#[test]
fn every_bound_holds_at_its_limit_and_refuses_one_past_it() {
    for (tries, ok) in [(1, false), (2, true), (5, true), (6, false)] {
        let result = parse(&format!("{{retry_on_fail: true, max_tries: {tries}}}"));
        assert_eq!(result.is_ok(), ok, "max_tries {tries}");
        if !ok {
            assert_eq!(result, Err(NodeSettingsRefusal::MaxTriesRange));
        }
    }
    for (wait, ok) in [(99, false), (100, true), (300_000, true), (300_001, false)] {
        let result = parse(&format!(
            "{{retry_on_fail: true, wait_between_tries_ms: {wait}}}"
        ));
        assert_eq!(result.is_ok(), ok, "wait {wait}");
        if !ok {
            assert_eq!(result, Err(NodeSettingsRefusal::WaitRange));
        }
    }
    for (timeout, ok) in [
        (999, false),
        (1_000, true),
        (900_000, true),
        (900_001, false),
    ] {
        let result = parse(&format!("{{timeout_ms: {timeout}}}"));
        assert_eq!(result.is_ok(), ok, "timeout {timeout}");
        if !ok {
            assert_eq!(result, Err(NodeSettingsRefusal::TimeoutRange));
        }
    }
}

#[test]
fn retry_fields_need_retry_on_fail() {
    assert_eq!(
        parse("{max_tries: 3}"),
        Err(NodeSettingsRefusal::RetryFieldsNeedRetry)
    );
    assert_eq!(
        parse("{wait_between_tries_ms: 500}"),
        Err(NodeSettingsRefusal::RetryFieldsNeedRetry)
    );
}

#[test]
fn the_error_transition_couples_to_error_output() {
    assert_eq!(
        parse("{on_error: error_output}"),
        Err(NodeSettingsRefusal::ErrorTransitionRequired)
    );
    assert_eq!(
        parse("{on_error: continue, error_transition: handler}"),
        Err(NodeSettingsRefusal::ErrorTransitionUnexpected)
    );
    assert_eq!(
        parse("{error_transition: handler}"),
        Err(NodeSettingsRefusal::ErrorTransitionUnexpected)
    );
    assert_eq!(
        parse("{on_error: error_output, error_transition: 'bad target'}"),
        Err(NodeSettingsRefusal::ErrorTransitionTarget)
    );
    let routed = parse("{on_error: error_output, error_transition: END}").expect("END route");
    assert!(routed.uses_node_errors());
}

#[test]
fn deterministic_families_refuse_retry_and_timeout() {
    let family = SettingsFamily::of("state_modifier");
    let retry = parse("{retry_on_fail: true}").expect("retry block");
    assert_eq!(
        retry.check_family(family),
        Err(NodeSettingsRefusal::RetryNotAllowed)
    );
    let timeout = parse("{timeout_ms: 2000}").expect("timeout block");
    assert_eq!(
        timeout.check_family(family),
        Err(NodeSettingsRefusal::TimeoutNotAllowed)
    );
    let admitted = parse("{on_error: continue, always_output_data: true, execute_once: true}")
        .expect("admitted block");
    for node_type in ["state_modifier", "router", "split_out", "aggregate"] {
        assert_eq!(
            admitted.check_family(SettingsFamily::of(node_type)),
            Ok(()),
            "{node_type}"
        );
    }
    for node_type in [
        "code", "llm", "decision", "toolkit", "mcp", "agent", "parallel", "map",
    ] {
        assert_eq!(
            admitted.check_family(SettingsFamily::of(node_type)),
            Err(NodeSettingsRefusal::FamilyNotYet),
            "{node_type}"
        );
    }
    for node_type in ["hitl", "printer"] {
        assert_eq!(
            admitted.check_family(SettingsFamily::of(node_type)),
            Err(NodeSettingsRefusal::FamilyPause),
            "{node_type}"
        );
    }
}

#[test]
fn the_constants_tables_and_texts_are_the_contracts() {
    let contract = contract();
    let limits = &contract["limits"];
    for (key, value) in [
        ("min_node_tries", u64::from(MIN_NODE_TRIES)),
        ("max_node_tries", u64::from(MAX_NODE_TRIES)),
        ("min_wait_between_tries_ms", MIN_WAIT_BETWEEN_TRIES_MS),
        ("max_wait_between_tries_ms", MAX_WAIT_BETWEEN_TRIES_MS),
        (
            "default_wait_between_tries_ms",
            DEFAULT_WAIT_BETWEEN_TRIES_MS,
        ),
        ("min_node_timeout_ms", MIN_NODE_TIMEOUT_MS),
        ("max_node_timeout_ms", MAX_NODE_TIMEOUT_MS),
        ("max_node_errors", MAX_NODE_ERRORS as u64),
        (
            "max_node_error_entry_bytes",
            MAX_NODE_ERROR_ENTRY_BYTES as u64,
        ),
        (
            "max_node_error_object_bytes",
            MAX_NODE_ERROR_OBJECT_BYTES as u64,
        ),
        ("max_executed_once_nodes", MAX_EXECUTED_ONCE_NODES as u64),
    ] {
        assert_eq!(limits[key].as_u64(), Some(value), "{key}");
    }
    assert_eq!(contract["settings_key"], SETTINGS_KEY);
    assert_eq!(contract["node_errors_key"], NODE_ERRORS_KEY);
    assert_eq!(contract["executed_once_key"], EXECUTED_ONCE_KEY);
    assert_eq!(
        contract["on_error"],
        json!(["stop", "continue", "error_output"])
    );
    let refusals = contract["refusals"].as_object().expect("refusals");
    assert_eq!(refusals.len(), NodeSettingsRefusal::ALL.len());
    for refusal in NodeSettingsRefusal::ALL {
        assert_eq!(
            refusals.get(refusal.key()).and_then(Value::as_str),
            Some(refusal.text()),
            "{}",
            refusal.key()
        );
    }
    let families = contract["families"].as_object().expect("families");
    assert_eq!(
        families.keys().map(String::as_str).collect::<BTreeSet<_>>(),
        DeterministicFamily::ALL
            .iter()
            .map(|family| family.node_type())
            .collect::<BTreeSet<_>>()
    );
    for family in DeterministicFamily::ALL {
        let listed: Vec<&str> = families[family.node_type()]
            .as_array()
            .expect("family keys")
            .iter()
            .filter_map(Value::as_str)
            .collect();
        assert_eq!(
            listed,
            SettingsFamily::Deterministic(family).admitted_keys(),
            "{}",
            family.node_type()
        );
    }
    for pause in contract["pause_families"].as_array().expect("pauses") {
        assert_eq!(
            SettingsFamily::of(pause.as_str().expect("type")),
            SettingsFamily::Pause
        );
    }
    let keys: BTreeSet<&str> = contract["keys"]
        .as_array()
        .expect("keys")
        .iter()
        .filter_map(Value::as_str)
        .collect();
    let encoded = serde_json::to_value(NodeSettings::default()).expect("encode");
    assert_eq!(
        encoded
            .as_object()
            .expect("object")
            .keys()
            .map(String::as_str)
            .collect::<BTreeSet<_>>(),
        keys
    );
}

#[test]
fn the_digest_is_stable_and_names_every_field() {
    let base = parse("{on_error: continue}").expect("block");
    let digest = |settings: &NodeSettings| {
        settings_digest(&BTreeMap::from([("node".to_owned(), settings.clone())]))
            .expect("non-default")
    };
    assert_eq!(digest(&base), digest(&base.clone()));
    for changed in [
        "{on_error: continue, always_output_data: true}",
        "{on_error: continue, execute_once: true}",
        "{on_error: error_output, error_transition: END}",
        "{execute_once: true}",
    ] {
        assert_ne!(
            digest(&base),
            digest(&parse(changed).expect(changed)),
            "{changed}"
        );
    }
    let renamed = settings_digest(&BTreeMap::from([("other".to_owned(), base.clone())]));
    assert_ne!(Some(digest(&base)), renamed);
    assert_ne!(bind_digest([1; 32], Some(digest(&base))), [1; 32]);
}

fn entry(node: &str, message: &str) -> NodeErrorEntry<'static> {
    NodeErrorEntry {
        node: Box::leak(node.to_owned().into_boxed_str()),
        node_type: "state_modifier",
        handled: OnNodeError::Continue,
        code: "pipeline.state_modifier.failed",
        message: Box::leak(message.to_owned().into_boxed_str()),
        support_ref: node_error_support_ref("execution-1", node, 3),
        retryable: false,
        attempts: 1,
        input_step: 3,
    }
}

#[test]
fn an_entry_matches_the_schema_fixture_byte_for_byte() {
    let fixture = std::fs::read_to_string(
        std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../jsonschema/runtime/v1/fixtures/pipeline-node-error-entry-v1.json"),
    )
    .expect("the entry fixture");
    let value = entry("enrich", "The State Modifier node failed.")
        .to_value()
        .expect("entry");
    assert_eq!(serde_json::to_string(&value).expect("encode"), fixture);
}

#[test]
fn the_entry_bounds_hold_at_the_limit_and_refuse_one_past_it() {
    let padded = |bytes: usize| {
        let mut value = json!({"node": ""});
        let overhead = serde_json::to_vec(&value).expect("encode").len();
        value["node"] = json!("n".repeat(bytes - overhead));
        value
    };
    assert_eq!(
        check_entry_bytes(&padded(MAX_NODE_ERROR_ENTRY_BYTES)),
        Ok(())
    );
    assert_eq!(
        check_entry_bytes(&padded(MAX_NODE_ERROR_ENTRY_BYTES + 1)),
        Err(EntryRefused)
    );
    // The longest node id and the longest catalog message still fit: the
    // message is cut by its own safe bound, never by the entry bound.
    let longest = entry(&"n".repeat(128), &"m".repeat(2_000))
        .to_value()
        .expect("largest entry");
    assert!(serde_json::to_vec(&longest).expect("encode").len() <= MAX_NODE_ERROR_ENTRY_BYTES);
    let mut bad = entry("enrich", "x");
    bad.code = "Not A Code";
    assert_eq!(bad.to_value(), Err(EntryRefused));
    let mut stop = entry("enrich", "x");
    stop.handled = OnNodeError::Stop;
    assert_eq!(stop.to_value(), Err(EntryRefused));
}

#[test]
fn entry_text_is_reduced_to_the_safe_charset() {
    let value = entry("enrich", "quote \" backslash \\ newline \n end")
        .to_value()
        .expect("entry");
    let message = value["error"]["message_safe"].as_str().expect("message");
    assert!(!message.contains('"') && !message.contains('\\') && !message.contains('\n'));
}

#[test]
fn the_support_ref_is_stable_and_scoped() {
    let one = node_error_support_ref("execution-1", "a", 2);
    assert_eq!(one, node_error_support_ref("execution-1", "a", 2));
    assert_ne!(one, node_error_support_ref("execution-2", "a", 2));
    assert_ne!(one, node_error_support_ref("execution-1", "a", 3));
    assert_eq!(one.len(), 32);
}

#[test]
fn neutral_values_follow_the_declared_type() {
    for (kind, value) in [
        ("str", json!("")),
        ("list", json!([])),
        ("dict", json!({})),
        ("int", json!(0)),
        ("float", json!(0.0)),
        ("bool", json!(false)),
        ("unknown", Value::Null),
    ] {
        assert_eq!(neutral_value(kind), value, "{kind}");
    }
}

#[test]
fn node_errors_counts_and_a_malformed_channel_counts_as_full() {
    assert_eq!(node_error_count(None), 0);
    assert_eq!(node_error_count(Some(&Value::Null)), 0);
    assert_eq!(node_error_count(Some(&json!([1, 2]))), 2);
    assert_eq!(node_error_count(Some(&json!("x"))), usize::MAX);
}

#[test]
fn the_run_once_set_holds_128_ids_and_refuses_the_129th() {
    let mut set: Option<Value> = None;
    for index in 0..MAX_EXECUTED_ONCE_NODES {
        set = Some(
            mark_executed_once(set.as_ref(), &format!("n{index}"), None).expect("within bound"),
        );
    }
    assert_eq!(
        mark_executed_once(set.as_ref(), "one-more", None),
        Err(ExecutedOnceFull)
    );
    // Re-marking a known id never grows the set.
    assert!(mark_executed_once(set.as_ref(), "n0", None).is_ok());
    let routed = mark_executed_once(None, "route", Some("left")).expect("router");
    assert_eq!(
        executed_once(Some(&routed), "route"),
        Some(Some("left".to_owned()))
    );
    assert_eq!(executed_once(set.as_ref(), "n5"), Some(None));
    assert_eq!(executed_once(set.as_ref(), "absent"), None);
    assert_eq!(executed_once(None, "n5"), None);
}

#[test]
fn refusal_messages_name_the_node_only_when_node_level() {
    assert_eq!(
        NodeSettingsRefusal::MaxTriesRange.message("enrich"),
        "Node enrich: Max tries must be between 2 and 5."
    );
    assert_eq!(
        NodeSettingsRefusal::NodeErrorsType.message("enrich"),
        NodeSettingsRefusal::NodeErrorsType.text()
    );
}
