//! Python's `str()` of a JSON value, the one piece of Python formatting
//! `elitea-engine-core` does not have.
//!
//! Everything else this crate prints the Python way comes from
//! `elitea-engine-core`: `repr(float)` ([`pyjson::float_repr`]),
//! `json.dumps(..., indent=4)` ([`pyjson::dumps_with`]), `repr()` of a JSON
//! value ([`pyvalue::repr_value`]) and truthiness ([`pyvalue::py_truthy`]).
//! Its `pyvalue::py_str` is `str()` for the scalars a repository field
//! holds: it prints a list or a dict as JSON and a float as `serde_json`
//! does (`1e-7`, where Python prints `1e-07`). The SDK applies `str()` to
//! arbitrary metadata values, so this module completes it with `repr`.
//!
//! [`pyjson::float_repr`]: elitea_engine_core::pyjson::float_repr
//! [`pyjson::dumps_with`]: elitea_engine_core::pyjson::dumps_with
//! [`pyvalue::repr_value`]: elitea_engine_core::pyvalue::repr_value
//! [`pyvalue::py_truthy`]: elitea_engine_core::pyvalue::py_truthy

use elitea_engine_core::pyvalue::repr_value;
use serde_json::Value;

/// `str(value)` of the Python object `json.loads` would have made: a string
/// prints as itself, anything else as its `repr`.
#[must_use]
pub(crate) fn display(value: &Value) -> String {
    match value {
        Value::String(text) => text.clone(),
        other => repr_value(other),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn str_is_the_text_of_a_string_and_the_repr_of_anything_else() {
        assert_eq!(display(&json!("plain")), "plain");
        assert_eq!(display(&json!([])), "[]");
        assert_eq!(display(&json!(1e-7)), "1e-07");
        assert_eq!(display(&json!(2.0)), "2.0");
        assert_eq!(
            display(&json!({"a": [1, null, true], "it's": "x"})),
            r#"{'a': [1, None, True], "it's": 'x'}"#
        );
        // engine-core's py_str differs exactly where this module is needed.
        assert_eq!(
            elitea_engine_core::pyvalue::py_str(&json!([1, null])),
            "[1,null]"
        );
        assert_eq!(display(&json!([1, null])), "[1, None]");
    }
}
