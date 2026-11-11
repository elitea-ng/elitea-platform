use std::collections::BTreeMap;

use adk_rust::graph::State;
use serde_json::{Value, json};

use super::code_state::{CodeStateBoundary, CodeStateError};

fn boundary() -> CodeStateBoundary {
    CodeStateBoundary::new(&BTreeMap::from([
        ("count".into(), "int".into()),
        ("payload".into(), "dict".into()),
        ("messages".into(), "list".into()),
    ]))
    .expect("valid declared state")
}

fn state() -> State {
    State::from([
        ("count".into(), json!(2)),
        (
            "payload".into(),
            json!({"text": "one\\ntwo", "nested": [1, 2]}),
        ),
        ("input".into(), json!("user request")),
        ("messages".into(), json!([{"content": "private history"}])),
        ("session_id".into(), json!("private-session")),
        ("context_info".into(), json!({"grant": "private-grant"})),
        // Unknown future control fields must also remain absent.
        ("future_runtime_metadata".into(), json!("private-runtime")),
    ])
}

#[test]
fn legacy_all_input_exposes_only_declared_user_variables() {
    let boundary = boundary();
    let state = state();
    for selected in [vec![], vec!["messages".into()]] {
        let bytes = boundary.input_json(&state, &selected).expect("input");
        assert_eq!(
            serde_json::from_slice::<Value>(&bytes).expect("JSON"),
            json!({
                "count": 2,
                "payload": {"text": "one\\ntwo", "nested": [1, 2]},
                "input": "user request"
            })
        );
        assert!(!String::from_utf8(bytes).expect("UTF-8").contains("private"));
    }
}

#[test]
fn explicit_input_preserves_values_without_mutating_checkpoint() {
    let boundary = boundary();
    let state = state();
    let before = state.clone();
    let bytes = boundary
        .input_json(&state, &["count".into(), "messages".into()])
        .expect("input");
    assert_eq!(
        serde_json::from_slice::<Value>(&bytes).expect("JSON"),
        json!({"count": 2})
    );
    assert_eq!(state, before);
}

#[test]
fn invalid_selections_cannot_export_runtime_values() {
    for selection in [
        vec!["session_id".into()],
        vec!["future_runtime_metadata".into()],
        vec!["count".into(), "count".into()],
    ] {
        assert_eq!(
            boundary().input_json(&state(), &selection),
            Err(CodeStateError::ForbiddenKey)
        );
    }
}

#[test]
fn runtime_fields_cannot_be_disguised_as_declared_user_data() {
    for key in [
        "session_id",
        "context_info",
        "hitl_decisions",
        "state_types",
    ] {
        let types = BTreeMap::from([(key.into(), "str".into())]);
        assert!(matches!(
            CodeStateBoundary::new(&types),
            Err(CodeStateError::InvalidSchema)
        ));
    }
}

#[test]
fn selected_data_is_bounded_after_json_escaping() {
    let mut state = state();
    state.insert(
        "payload".into(),
        json!({"nested": "\u{0000}".repeat(100_000)}),
    );
    assert_eq!(
        boundary().input_json(&state, &[]),
        Err(CodeStateError::SizeLimit)
    );
    // An oversized unselected value does not prevent a small authorized input.
    assert!(boundary().input_json(&state, &["count".into()]).is_ok());
}

#[test]
fn selected_input_with_wrong_type_fails_before_execution() {
    let mut state = state();
    state.insert("count".into(), json!("not an integer"));
    assert_eq!(
        boundary().input_json(&state, &[]),
        Err(CodeStateError::InvalidValue)
    );
}

#[test]
fn ordinary_outputs_require_selected_destinations() {
    let boundary = boundary();
    assert_eq!(
        boundary.validate_updates(br#"{"count":3}"#, &["count".into()], false),
        Ok(BTreeMap::from([("count".into(), json!(3))]))
    );
    assert_eq!(
        boundary.validate_updates(br#"{"payload":{}}"#, &["count".into()], false),
        Err(CodeStateError::ForbiddenKey)
    );
}

#[test]
fn structured_results_can_update_only_declared_user_state() {
    let boundary = boundary();
    assert!(
        boundary
            .validate_updates(br#"{"count":3,"payload":{"answer":true}}"#, &[], true)
            .is_ok()
    );
    for bytes in [
        br#"{"count":3,"session_id":"foreign"}"#.as_slice(),
        br#"{"count":3,"messages":[]}"#,
        br#"{"count":3,"undeclared":1}"#,
    ] {
        assert_eq!(
            boundary.validate_updates(bytes, &[], true),
            Err(CodeStateError::ForbiddenKey)
        );
    }
}

#[test]
fn invalid_result_returns_no_partial_patch() {
    assert_eq!(
        boundary().validate_updates(br#"{"count":3,"payload":false}"#, &[], true),
        Err(CodeStateError::InvalidValue)
    );
    for bytes in [b"not JSON".as_slice(), b"[]", b"null", b"{} trailing"] {
        assert_eq!(
            boundary().validate_updates(bytes, &[], true),
            Err(CodeStateError::MalformedResult)
        );
    }
}

#[test]
fn oversized_and_deeply_nested_results_fail_safely() {
    assert_eq!(
        boundary().validate_updates(&vec![b' '; 512 * 1024 + 1], &[], true),
        Err(CodeStateError::SizeLimit)
    );
    let deep = format!("{{\"payload\":{}0{}}}", "[".repeat(200), "]".repeat(200));
    assert_eq!(
        boundary().validate_updates(deep.as_bytes(), &[], true),
        Err(CodeStateError::MalformedResult)
    );
}

#[test]
fn deeply_nested_checkpoint_input_fails_before_recursive_serialization() {
    let mut nested = json!(0);
    for _ in 0..200 {
        nested = json!({"child": nested});
    }
    let mut state = state();
    state.insert("payload".into(), nested);
    assert_eq!(
        boundary().input_json(&state, &[]),
        Err(CodeStateError::SizeLimit)
    );
}

/// W3-2: a Map Code worker reads its item, whatever JSON it is, and can never
/// write it back.
#[test]
fn a_map_item_channel_is_read_as_any_json_and_never_written() {
    let boundary = CodeStateBoundary::new(&BTreeMap::from([
        (
            "entity".into(),
            super::code_state::MAP_ITEM_INPUT_KIND.to_owned(),
        ),
        ("item_index".into(), "int".into()),
        ("item_result".into(), "str".into()),
    ]))
    .expect("an item channel is admitted for Code input");
    for item in [json!("a"), json!({"k": [1, 2]}), json!(3), json!(null)] {
        let state = State::from([
            ("entity".into(), item.clone()),
            ("item_index".into(), json!(0)),
        ]);
        let bytes = boundary
            .input_json(&state, &["entity".into(), "item_index".into()])
            .expect("any item JSON is input");
        assert_eq!(
            serde_json::from_slice::<Value>(&bytes).unwrap(),
            json!({"entity": item, "item_index": 0})
        );
    }
    for structured in [false, true] {
        assert!(matches!(
            boundary.validate_updates(
                br#"{"entity":"overwritten"}"#,
                &["entity".into()],
                structured
            ),
            Err(CodeStateError::InvalidValue)
        ));
    }
}

/// D3: a Map item read by Code is bounded like any Code input: nesting of
/// exactly 128 values is admitted, 129 is refused; the field count bound
/// holds for the item's object too.
#[test]
fn a_map_item_is_bounded_by_the_code_input_depth_and_field_limits() {
    let boundary = CodeStateBoundary::new(&BTreeMap::from([(
        "entity".into(),
        super::code_state::MAP_ITEM_INPUT_KIND.to_owned(),
    )]))
    .unwrap();
    let nested = |values: usize| {
        let mut value = json!(0);
        for _ in 1..values {
            value = json!([value]);
        }
        value
    };
    for (values, admitted) in [(128, true), (129, false)] {
        let state = State::from([("entity".into(), nested(values))]);
        let result = boundary.input_json(&state, &["entity".into()]);
        assert_eq!(result.is_ok(), admitted, "{values} nested values");
        if !admitted {
            assert_eq!(result.unwrap_err(), CodeStateError::SizeLimit);
        }
    }
}
