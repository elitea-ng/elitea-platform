//! Every row of the typed reducer table, its bounds and the ADK closure.

use std::collections::BTreeMap;

use adk_rust::graph::{Channel, StateSchema};
use serde_json::{Value, json};

use super::state_reducers::{
    MAX_APPEND_ELEMENTS, MAX_MERGE_KEYS, MAX_REDUCED_BYTES, ReducerFailure, StateReducer,
    reducers_digest,
};

fn number(text: &str) -> Value {
    serde_json::from_str(text).expect("number fixture")
}

#[test]
fn reducer_names_parse_to_typed_variants_and_overwrite_is_the_default() {
    assert_eq!(StateReducer::parse("overwrite"), Ok(None));
    assert_eq!(
        StateReducer::parse("append"),
        Ok(Some(StateReducer::Append))
    );
    assert_eq!(
        StateReducer::parse("sum_int"),
        Ok(Some(StateReducer::SumInt))
    );
    assert_eq!(StateReducer::parse("merge"), Ok(Some(StateReducer::Merge)));
    for unknown in ["Append", "sum", "add", "custom", "", "extend"] {
        assert!(StateReducer::parse(unknown).is_err(), "{unknown}");
    }
}

#[test]
fn each_reducer_names_its_only_compatible_state_type() {
    assert_eq!(StateReducer::Append.state_type(), "list");
    assert_eq!(StateReducer::SumInt.state_type(), "int");
    assert_eq!(StateReducer::Merge.state_type(), "dict");
}

#[test]
fn append_concatenates_in_order() {
    let reduced = StateReducer::Append.reduce_checked(&json!([1, "a"]), &json!([{"b": 2}, null]));
    assert_eq!(reduced, Ok(json!([1, "a", {"b": 2}, null])));
    assert_eq!(
        StateReducer::Append.reduce_checked(&json!([1]), &json!([])),
        Ok(json!([1]))
    );
}

#[test]
fn append_never_wraps_a_scalar_and_null_never_clears() {
    for update in [
        json!(1),
        json!("x"),
        json!({"a": 1}),
        Value::Null,
        json!(true),
    ] {
        assert_eq!(
            StateReducer::Append.reduce_checked(&json!([1]), &update),
            Err(ReducerFailure::TypeMismatch),
            "{update}"
        );
    }
    assert_eq!(
        StateReducer::Append.reduce_checked(&json!({}), &json!([1])),
        Err(ReducerFailure::TypeMismatch)
    );
}

#[test]
fn append_is_bounded_by_element_count_at_limit_and_limit_plus_one() {
    let current = Value::Array(vec![json!(0); MAX_APPEND_ELEMENTS - 1]);
    let at_limit = StateReducer::Append
        .reduce_checked(&current, &json!([1]))
        .expect("exactly the element limit");
    assert_eq!(at_limit.as_array().map(Vec::len), Some(MAX_APPEND_ELEMENTS));
    assert_eq!(
        StateReducer::Append.reduce_checked(&current, &json!([1, 2])),
        Err(ReducerFailure::Limit)
    );
}

#[test]
fn append_is_bounded_by_serialized_bytes_at_limit_and_limit_plus_one() {
    // `["…"]` costs four bytes of punctuation around the string.
    let filler = "x".repeat(MAX_REDUCED_BYTES - 4);
    let at_limit = StateReducer::Append
        .reduce_checked(&json!([]), &json!([filler]))
        .expect("exactly the byte limit");
    assert_eq!(
        serde_json::to_vec(&at_limit).expect("bytes").len(),
        MAX_REDUCED_BYTES
    );
    let over = "x".repeat(MAX_REDUCED_BYTES - 3);
    assert_eq!(
        StateReducer::Append.reduce_checked(&json!([]), &json!([over])),
        Err(ReducerFailure::Limit)
    );
}

#[test]
fn sum_int_adds_exact_integers_including_integral_spellings() {
    assert_eq!(
        StateReducer::SumInt.reduce_checked(&json!(2), &json!(3)),
        Ok(json!(5))
    );
    assert_eq!(
        StateReducer::SumInt.reduce_checked(&json!(-7), &number("2.0")),
        Ok(json!(-5))
    );
    assert_eq!(
        StateReducer::SumInt.reduce_checked(&json!(0), &number("1e2")),
        Ok(json!(100))
    );
}

#[test]
fn sum_int_refuses_fractions_and_non_numbers() {
    for update in [
        number("2.5"),
        json!("3"),
        Value::Null,
        json!([1]),
        json!(true),
    ] {
        assert_eq!(
            StateReducer::SumInt.reduce_checked(&json!(1), &update),
            Err(ReducerFailure::TypeMismatch),
            "{update}"
        );
    }
    assert_eq!(
        StateReducer::SumInt.reduce_checked(&number("1.5"), &json!(1)),
        Err(ReducerFailure::TypeMismatch)
    );
}

#[test]
fn sum_int_overflows_at_i64_bounds_with_checked_arithmetic() {
    assert_eq!(
        StateReducer::SumInt.reduce_checked(&json!(i64::MAX - 1), &json!(1)),
        Ok(json!(i64::MAX))
    );
    assert_eq!(
        StateReducer::SumInt.reduce_checked(&json!(i64::MAX), &json!(1)),
        Err(ReducerFailure::Overflow)
    );
    assert_eq!(
        StateReducer::SumInt.reduce_checked(&json!(i64::MIN), &json!(-1)),
        Err(ReducerFailure::Overflow)
    );
    assert_eq!(
        StateReducer::SumInt.reduce_checked(&json!(0), &json!(u64::MAX)),
        Err(ReducerFailure::Overflow)
    );
    // An exponent no integer can carry is not an exact i64 value at all.
    assert_eq!(
        StateReducer::SumInt.reduce_checked(&json!(0), &number("1e99999999999999999999")),
        Err(ReducerFailure::TypeMismatch)
    );
}

#[test]
fn merge_is_shallow_update_keys_win_and_null_sets_null() {
    let reduced = StateReducer::Merge.reduce_checked(
        &json!({"a": 1, "b": {"x": 1}, "c": 3}),
        &json!({"b": {"y": 2}, "c": null, "d": 4}),
    );
    assert_eq!(
        reduced,
        Ok(json!({"a": 1, "b": {"y": 2}, "c": null, "d": 4}))
    );
}

#[test]
fn merge_requires_objects() {
    for update in [json!([1]), json!(1), Value::Null, json!("x")] {
        assert_eq!(
            StateReducer::Merge.reduce_checked(&json!({}), &update),
            Err(ReducerFailure::TypeMismatch),
            "{update}"
        );
    }
}

#[test]
fn merge_is_bounded_by_key_count_at_limit_and_limit_plus_one() {
    let current: serde_json::Map<String, Value> = (0..MAX_MERGE_KEYS - 1)
        .map(|index| (format!("k{index}"), json!(0)))
        .collect();
    let current = Value::Object(current);
    assert!(
        StateReducer::Merge
            .reduce_checked(&current, &json!({"new": 1, "k0": 2}))
            .is_ok()
    );
    assert_eq!(
        StateReducer::Merge.reduce_checked(&current, &json!({"new": 1, "other": 2})),
        Err(ReducerFailure::Limit)
    );
}

#[test]
fn merge_is_bounded_by_serialized_bytes() {
    let big = "x".repeat(MAX_REDUCED_BYTES);
    assert_eq!(
        StateReducer::Merge.reduce_checked(&json!({}), &json!({ "k": big })),
        Err(ReducerFailure::Limit)
    );
}

#[test]
fn failure_codes_are_stable_and_name_no_value() {
    assert_eq!(
        ReducerFailure::TypeMismatch.code(),
        "graph.state.reducer_type_mismatch"
    );
    assert_eq!(ReducerFailure::Limit.code(), "graph.state.reducer_limit");
    assert_eq!(
        ReducerFailure::Overflow.code(),
        "graph.state.reducer_overflow"
    );
}

#[test]
fn bounds_check_a_value_the_reducer_would_hold() {
    assert_eq!(StateReducer::Append.check_held(&json!([1, 2])), Ok(()));
    assert_eq!(
        StateReducer::Append.check_held(&Value::Array(vec![json!(0); MAX_APPEND_ELEMENTS + 1])),
        Err(ReducerFailure::Limit)
    );
    assert_eq!(
        StateReducer::SumInt.check_held(&json!(u64::MAX)),
        Err(ReducerFailure::Overflow)
    );
    assert_eq!(
        StateReducer::Merge.check_held(&json!([])),
        Err(ReducerFailure::TypeMismatch)
    );
}

#[test]
fn the_adk_channel_reducer_repeats_the_reduction_and_never_panics() {
    let mut schema = StateSchema::new();
    for (name, reducer) in [
        ("findings", StateReducer::Append),
        ("total", StateReducer::SumInt),
        ("seen", StateReducer::Merge),
    ] {
        schema.channels.insert(
            name.to_owned(),
            Channel::new(name).with_reducer(reducer.channel_reducer(name)),
        );
    }
    let mut state = adk_rust::graph::State::new();
    state.insert("findings".to_owned(), json!(["a"]));
    state.insert("total".to_owned(), json!(i64::MAX));
    state.insert("seen".to_owned(), json!({"a": 1}));
    schema.apply_update(&mut state, "findings", json!(["b"]));
    schema.apply_update(&mut state, "seen", json!({"b": 2}));
    assert_eq!(state["findings"], json!(["a", "b"]));
    assert_eq!(state["seen"], json!({"a": 1, "b": 2}));
    // A violation the guard should have refused keeps the current value.
    schema.apply_update(&mut state, "total", json!(1));
    schema.apply_update(&mut state, "findings", json!("scalar"));
    assert_eq!(state["total"], json!(i64::MAX));
    assert_eq!(state["findings"], json!(["a", "b"]));
}

#[test]
fn the_reducer_digest_is_order_independent_and_tag_sensitive() {
    let base = [7_u8; 32];
    let one = BTreeMap::from([
        ("a".to_owned(), StateReducer::Append),
        ("b".to_owned(), StateReducer::Merge),
    ]);
    let mut two = BTreeMap::new();
    two.insert("b".to_owned(), StateReducer::Merge);
    two.insert("a".to_owned(), StateReducer::Append);
    assert_eq!(reducers_digest(base, &one), reducers_digest(base, &two));
    assert_ne!(reducers_digest(base, &one), base);
    let changed = BTreeMap::from([
        ("a".to_owned(), StateReducer::Append),
        ("b".to_owned(), StateReducer::Append),
    ]);
    assert_ne!(reducers_digest(base, &one), reducers_digest(base, &changed));
    let renamed = BTreeMap::from([
        ("a".to_owned(), StateReducer::Append),
        ("c".to_owned(), StateReducer::Merge),
    ]);
    assert_ne!(reducers_digest(base, &one), reducers_digest(base, &renamed));
}
