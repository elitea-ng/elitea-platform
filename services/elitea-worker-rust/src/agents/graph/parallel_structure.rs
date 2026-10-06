//! Check shape before recursive JSON serializers or clones can run.

use adk_rust::graph::{Checkpoint, GraphError, State};
use serde_json::Value;

use super::parallel_error;

// 126 value levels plus one enclosing state/metadata object stay below serde's
// rejected 128th container. The raw payload therefore permits 125 child edges.
const MAX_JSON_DEPTH: usize = 125;
const MAX_JSON_VALUES: usize = 100_000;

enum Children<'a> {
    Root(std::iter::Once<&'a Value>),
    Array(std::slice::Iter<'a, Value>),
    Object(serde_json::map::Values<'a>),
}

impl<'a> Iterator for Children<'a> {
    type Item = &'a Value;

    fn next(&mut self) -> Option<Self::Item> {
        match self {
            Self::Root(values) => values.next(),
            Self::Array(values) => values.next(),
            Self::Object(values) => values.next(),
        }
    }
}

pub(in crate::agents::graph) fn validate_values<'a>(
    roots: impl IntoIterator<Item = &'a Value>,
) -> Result<(), GraphError> {
    let mut remaining = MAX_JSON_VALUES;
    // The stack holds one iterator per level rather than every sibling value.
    let mut stack = Vec::new();
    for root in roots {
        stack.push((Children::Root(std::iter::once(root)), 0_usize));
        while let Some((children, depth)) = stack.last_mut() {
            let depth = *depth;
            let Some(value) = children.next() else {
                stack.pop();
                continue;
            };
            if depth > MAX_JSON_DEPTH || remaining == 0 {
                return Err(parallel_error(
                    "graph.parallel.structure_resource_exhausted",
                    "the parallel JSON depth or value count exceeds its resource bound",
                ));
            }
            remaining -= 1;
            match value {
                Value::Array(values) => {
                    stack.push((Children::Array(values.iter()), depth + 1));
                }
                Value::Object(values) => {
                    stack.push((Children::Object(values.values()), depth + 1));
                }
                _ => {}
            }
        }
    }
    Ok(())
}

pub(in crate::agents::graph) fn validate_state(state: &State) -> Result<(), GraphError> {
    validate_values(state.values())
}

pub(super) fn validate_checkpoint(checkpoint: &Checkpoint) -> Result<(), GraphError> {
    validate_values(
        checkpoint
            .state
            .values()
            .chain(checkpoint.metadata.values())
            .chain(checkpoint.child_ledger.values()),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parallel_structure_refuses_deep_and_wide_values_without_recursive_walks() {
        let mut value = Value::Null;
        for _ in 0..256 {
            value = Value::Array(vec![value]);
        }
        assert!(validate_values([&value]).is_err());
        assert!(
            super::super::terminal_outcome(super::super::ParallelBranchTerminal::Completed(
                serde_json::Map::from_iter([("deep".into(), value),])
            ))
            .is_err()
        );
        let wide = Value::Array(vec![Value::Null; MAX_JSON_VALUES]);
        assert!(validate_values([&wide]).is_err());
        assert!(validate_values([&serde_json::json!({"arbitrary": [1, "two", null, {}]})]).is_ok());
    }

    #[test]
    fn parallel_structure_budget_is_shared_across_all_roots() {
        let value = Value::Array(vec![Value::Null; MAX_JSON_VALUES / 2]);
        assert!(validate_values([&value]).is_ok());
        assert!(validate_values([&value, &value]).is_err());
        let large = State::from([(
            "unmapped".into(),
            Value::String("x".repeat(super::super::MAX_BRANCH_INPUT_BYTES + 1)),
        )]);
        assert!(super::super::validate_input_state(&large).is_err());
    }
    #[test]
    fn parallel_structure_boundary_retains_json_codec_room_for_checkpoint_envelopes() {
        let mut value = Value::Array(Vec::new());
        for _ in 0..MAX_JSON_DEPTH {
            value = Value::Array(vec![value]);
        }
        let checkpoint = Checkpoint::new("boundary", State::new(), 0, Vec::new());
        let mut valid = checkpoint.clone();
        valid.metadata.insert("receipt".into(), value.clone());
        validate_checkpoint(&valid).unwrap();
        let encoded = serde_json::to_vec(&valid.metadata).unwrap();
        let decoded: std::collections::HashMap<String, Value> =
            serde_json::from_slice(&encoded).unwrap();
        assert_eq!(decoded, valid.metadata);

        let mut invalid = checkpoint;
        invalid
            .metadata
            .insert("receipt".into(), Value::Array(vec![value]));
        assert!(validate_checkpoint(&invalid).is_err());
        let encoded = serde_json::to_vec(&invalid.metadata).unwrap();
        assert!(
            serde_json::from_slice::<std::collections::HashMap<String, Value>>(&encoded).is_err()
        );
    }
}
