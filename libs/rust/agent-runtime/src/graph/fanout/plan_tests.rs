use serde_json::{Map, Value, json};

use super::*;

fn ok(key: &str, value: Value) -> SettledChild {
    let mut outputs = Map::new();
    outputs.insert("out".to_owned(), value);
    SettledChild {
        key: key.to_owned(),
        node: format!("node_{key}"),
        terminal: ChildTerminal::Succeeded(outputs),
    }
}

fn failed(key: &str) -> SettledChild {
    SettledChild {
        key: key.to_owned(),
        node: format!("node_{key}"),
        terminal: ChildTerminal::Failed(ChildFailure {
            code: "graph.fanout.child_failed".to_owned(),
            message_safe: "The step failed.".to_owned(),
            support_ref: Some("ref-1".to_owned()),
        }),
    }
}

fn joined(outcome: JoinOutcome) -> Vec<Value> {
    match outcome {
        JoinOutcome::Joined(entries) => entries,
        JoinOutcome::Failed { .. } => panic!("expected a join"),
    }
}

#[test]
fn defaults_are_continue_and_append() {
    assert_eq!(OnError::parse(None), Ok(OnError::Continue));
    assert_eq!(OnError::parse(Some("stop")), Ok(OnError::Stop));
    assert_eq!(
        OnError::parse(Some("ignore")),
        Err(FanoutPlanError::InvalidOnError)
    );
    assert_eq!(JoinMode::parse(None), Ok(JoinMode::Append));
    assert_eq!(JoinMode::parse(Some("keyed")), Ok(JoinMode::Keyed));
    assert_eq!(
        JoinMode::parse(Some("last")),
        Err(FanoutPlanError::InvalidJoin)
    );
}

#[test]
fn continue_lands_partial_results_with_a_status_on_every_entry() {
    let entries = joined(
        join(
            FanoutKind::Parallel,
            OnError::Continue,
            vec![ok("a", json!(1)), failed("b"), ok("c", json!(3))],
        )
        .expect("join"),
    );
    assert_eq!(
        entries,
        vec![
            json!({"branch_id": "a", "node": "node_a", "status": "succeeded", "outputs": {"out": 1}}),
            json!({"branch_id": "b", "node": "node_b", "status": "failed", "error": {
                "code": "graph.fanout.child_failed",
                "message_safe": "The step failed.",
                "support_ref": "ref-1"}}),
            json!({"branch_id": "c", "node": "node_c", "status": "succeeded", "outputs": {"out": 3}}),
        ]
    );
}

#[test]
fn map_entries_carry_a_numeric_index() {
    let entries = joined(
        join(
            FanoutKind::Map,
            OnError::Continue,
            vec![ok("0", json!("x")), failed("1")],
        )
        .expect("join"),
    );
    assert_eq!(entries[0]["index"], json!(0));
    assert_eq!(entries[1]["index"], json!(1));
    assert_eq!(
        join(FanoutKind::Map, OnError::Continue, vec![ok("01", json!(1))]),
        Err(FanoutPlanError::InvalidChild)
    );
}

#[test]
fn stop_fails_on_the_first_failure_in_declared_order() {
    assert_eq!(
        join(
            FanoutKind::Parallel,
            OnError::Stop,
            vec![ok("a", json!(1)), failed("b"), failed("c")],
        ),
        Ok(JoinOutcome::Failed {
            key: "b".to_owned(),
            failure: ChildFailure {
                code: "graph.fanout.child_failed".to_owned(),
                message_safe: "The step failed.".to_owned(),
                support_ref: Some("ref-1".to_owned()),
            },
        })
    );
}

#[test]
fn every_child_failing_fails_the_node_under_continue() {
    assert!(matches!(
        join(
            FanoutKind::Parallel,
            OnError::Continue,
            vec![failed("a"), failed("b")]
        ),
        Ok(JoinOutcome::Failed { key, .. }) if key == "a"
    ));
}

#[test]
fn duplicate_or_empty_keys_are_refused() {
    assert_eq!(
        join(
            FanoutKind::Parallel,
            OnError::Continue,
            vec![ok("a", json!(1)), ok("a", json!(2))]
        ),
        Err(FanoutPlanError::InvalidChild)
    );
    assert_eq!(
        join(
            FanoutKind::Parallel,
            OnError::Continue,
            vec![ok("", json!(1))]
        ),
        Err(FanoutPlanError::InvalidChild)
    );
    assert_eq!(
        join(FanoutKind::Parallel, OnError::Continue, vec![]),
        Err(FanoutPlanError::InvalidChild)
    );
}

#[test]
fn child_count_limit_and_limit_plus_one() {
    let at = (0..MAX_FANOUT_CHILDREN)
        .map(|index| ok(&index.to_string(), json!(index)))
        .collect::<Vec<_>>();
    assert!(join(FanoutKind::Map, OnError::Continue, at).is_ok());
    let over = (0..=MAX_FANOUT_CHILDREN)
        .map(|index| ok(&index.to_string(), json!(index)))
        .collect::<Vec<_>>();
    assert_eq!(
        join(FanoutKind::Map, OnError::Continue, over),
        Err(FanoutPlanError::TooManyChildren)
    );
}

/// `{"out":"<n x>"}` serializes to n + 10 bytes.
fn outputs_of(bytes: usize) -> Value {
    Value::String("x".repeat(bytes - 10))
}

#[test]
fn child_result_limit_and_limit_plus_one() {
    assert!(
        join(
            FanoutKind::Parallel,
            OnError::Continue,
            vec![ok("a", outputs_of(MAX_CHILD_RESULT_BYTES))]
        )
        .is_ok()
    );
    assert_eq!(
        join(
            FanoutKind::Parallel,
            OnError::Continue,
            vec![ok("a", outputs_of(MAX_CHILD_RESULT_BYTES + 1))]
        ),
        Err(FanoutPlanError::ChildResultTooLarge)
    );
}

#[test]
fn joined_result_limit_and_limit_plus_one() {
    // Sixteen children at the per-child bound exceed 8 MiB; fifteen do not.
    let children = |count: usize| {
        (0..count)
            .map(|index| ok(&format!("b{index}"), outputs_of(MAX_CHILD_RESULT_BYTES)))
            .collect::<Vec<_>>()
    };
    assert!(join(FanoutKind::Parallel, OnError::Continue, children(15)).is_ok());
    assert_eq!(
        join(FanoutKind::Parallel, OnError::Continue, children(17)),
        Err(FanoutPlanError::JoinedResultTooLarge)
    );
}

#[test]
fn error_entries_never_exceed_their_bound_and_keep_the_code() {
    for message_bytes in [0, 10, 400, 512, 513, 4096] {
        let child = SettledChild {
            key: "k".repeat(64),
            node: "n".repeat(64),
            terminal: ChildTerminal::Failed(ChildFailure {
                code: "graph.fanout.child_failed".to_owned(),
                message_safe: "é".repeat(message_bytes / 2),
                support_ref: Some("r".repeat(200)),
            }),
        };
        let entries = joined(
            join(
                FanoutKind::Parallel,
                OnError::Continue,
                vec![child, ok("other", json!(1))],
            )
            .expect("join"),
        );
        let bytes = serde_json::to_vec(&entries[0]).expect("encode").len();
        assert!(bytes <= MAX_JOIN_ERROR_ENTRY_BYTES, "{bytes} bytes");
        assert_eq!(entries[0]["error"]["code"], "graph.fanout.child_failed");
    }
}

#[test]
fn append_lands_through_the_declared_reducer_or_concatenates() {
    let entries = vec![json!({"branch_id": "a"}), json!({"branch_id": "b"})];
    assert_eq!(
        land(
            JoinMode::Append,
            None,
            FanoutKind::Parallel,
            None,
            entries.clone()
        ),
        Ok(Value::Array(entries.clone()))
    );
    assert_eq!(
        land(
            JoinMode::Append,
            None,
            FanoutKind::Parallel,
            Some(&json!([{"branch_id": "old"}])),
            entries.clone()
        ),
        Ok(json!([{"branch_id": "old"}, {"branch_id": "a"}, {"branch_id": "b"}]))
    );
    // A declared append reducer receives only the new entries.
    assert_eq!(
        land(
            JoinMode::Append,
            Some(StateReducer::Append),
            FanoutKind::Parallel,
            Some(&json!([1])),
            entries.clone()
        ),
        Ok(Value::Array(entries.clone()))
    );
    assert_eq!(
        land(
            JoinMode::Append,
            None,
            FanoutKind::Parallel,
            Some(&json!("text")),
            entries
        ),
        Err(FanoutPlanError::JoinTypeMismatch)
    );
}

#[test]
fn keyed_lands_an_object_and_refuses_a_held_key() {
    let entries = vec![
        json!({"index": 0, "status": "succeeded"}),
        json!({"index": 1, "status": "failed"}),
    ];
    let landed = land(
        JoinMode::Keyed,
        None,
        FanoutKind::Map,
        None,
        entries.clone(),
    )
    .expect("keyed");
    assert_eq!(landed["0"]["status"], "succeeded");
    assert_eq!(landed["1"]["status"], "failed");
    assert_eq!(
        land(
            JoinMode::Keyed,
            Some(StateReducer::Merge),
            FanoutKind::Map,
            Some(&json!({"1": "held"})),
            entries
        ),
        Err(FanoutPlanError::JoinConflict)
    );
}

#[test]
fn merge_refuses_a_key_two_children_write() {
    let entry = |out: Value| json!({"branch_id": "a", "status": "succeeded", "outputs": out});
    assert_eq!(
        land(
            JoinMode::Merge,
            None,
            FanoutKind::Parallel,
            None,
            vec![entry(json!({"x": 1})), entry(json!({"y": 2}))]
        ),
        Ok(json!({"x": 1, "y": 2}))
    );
    assert_eq!(
        land(
            JoinMode::Merge,
            None,
            FanoutKind::Parallel,
            None,
            vec![entry(json!({"x": 1})), entry(json!({"x": 2}))]
        ),
        Err(FanoutPlanError::JoinConflict)
    );
    assert_eq!(
        land(
            JoinMode::Merge,
            None,
            FanoutKind::Parallel,
            Some(&json!({"x": 0})),
            vec![entry(json!({"x": 1}))]
        ),
        Err(FanoutPlanError::JoinConflict)
    );
}

#[test]
fn save_time_check_refuses_reducer_mismatches_and_merge_with_continue() {
    assert_eq!(JoinMode::Append.check(OnError::Continue, None), Ok(()));
    assert_eq!(
        JoinMode::Append.check(OnError::Continue, Some(StateReducer::Append)),
        Ok(())
    );
    assert_eq!(
        JoinMode::Append.check(OnError::Continue, Some(StateReducer::Merge)),
        Err(FanoutPlanError::ReducerMismatch)
    );
    assert_eq!(
        JoinMode::Keyed.check(OnError::Continue, Some(StateReducer::SumInt)),
        Err(FanoutPlanError::ReducerMismatch)
    );
    assert_eq!(
        JoinMode::Merge.check(OnError::Continue, None),
        Err(FanoutPlanError::MergeNeedsStop)
    );
    assert_eq!(JoinMode::Merge.check(OnError::Stop, None), Ok(()));
}
