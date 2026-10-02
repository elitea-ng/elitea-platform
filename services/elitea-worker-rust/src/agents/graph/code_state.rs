//! Language-neutral Code-node data boundary. No sandbox execution is enabled here.

#![allow(dead_code)] // Bound into Code execution after sandbox admission is implemented.

use std::collections::{BTreeMap, BTreeSet};
use std::io::{self, Write};

use adk_rust::graph::State;
use serde_json::Value;
use thiserror::Error;

use super::compiler::{reserved_user_state_key, state_value_matches};
use super::yaml::valid_output_key;

const MAX_STATE_BYTES: usize = 512 * 1024;
const MAX_STATE_KEYS: usize = 256;

/// Construct only from compiler-owned declarations, never sandbox-provided metadata.
pub(super) struct CodeStateBoundary {
    types: BTreeMap<String, String>,
}

impl CodeStateBoundary {
    pub(super) fn new(types: &BTreeMap<String, String>) -> Result<Self, CodeStateError> {
        if types.len() > MAX_STATE_KEYS {
            return Err(CodeStateError::SizeLimit);
        }
        let mut user_types = BTreeMap::new();
        for (key, kind) in types {
            // Messages have a separate assistant-message projection. They are not
            // user data, even when the legacy schema explicitly declares them.
            if key == "messages" {
                continue;
            }
            if !valid_output_key(key)
                || reserved_user_state_key(key)
                || !matches!(
                    kind.as_str(),
                    "str" | "int" | "float" | "bool" | "list" | "dict"
                )
            {
                return Err(CodeStateError::InvalidSchema);
            }
            user_types.insert(key.clone(), kind.clone());
        }
        // The compiler always provides the user input channel.
        if user_types.get("input").is_some_and(|kind| kind != "str") {
            return Err(CodeStateError::InvalidSchema);
        }
        user_types.insert("input".to_owned(), "str".to_owned());
        Ok(Self { types: user_types })
    }

    /// Serialize selected user values without cloning the whole checkpoint.
    pub(super) fn input_json(
        &self,
        state: &State,
        selected: &[String],
    ) -> Result<Vec<u8>, CodeStateError> {
        self.validate_selection(selected)?;
        let all = selected.is_empty() || selected == ["messages"];
        let mut values = BTreeMap::new();
        let mut remaining_values = MAX_STATE_BYTES;
        for (key, kind) in &self.types {
            if (all || selected.contains(key))
                && let Some(value) = state.get(key)
            {
                if !state_value_matches(kind, value) {
                    return Err(CodeStateError::InvalidValue);
                }
                validate_input_depth(value, 128, &mut remaining_values)?;
                values.insert(key.as_str(), value);
            }
        }
        let mut writer = BoundedJson(Vec::new());
        serde_json::to_writer(&mut writer, &values).map_err(|_| CodeStateError::SizeLimit)?;
        Ok(writer.0)
    }

    /// Validate a normalized user-variable patch before the graph applies it.
    /// Raw stdout and assistant messages must not be passed as this patch.
    pub(super) fn validate_updates(
        &self,
        bytes: &[u8],
        outputs: &[String],
        structured_output: bool,
    ) -> Result<BTreeMap<String, Value>, CodeStateError> {
        self.validate_selection(outputs)?;
        if bytes.len() > MAX_STATE_BYTES {
            return Err(CodeStateError::SizeLimit);
        }
        // Value decoding retains serde_json's recursion bound.
        let value: Value =
            serde_json::from_slice(bytes).map_err(|_| CodeStateError::MalformedResult)?;
        let Value::Object(values) = value else {
            return Err(CodeStateError::MalformedResult);
        };
        if values.len() > MAX_STATE_KEYS {
            return Err(CodeStateError::SizeLimit);
        }
        for (key, value) in &values {
            let kind = self.types.get(key).ok_or(CodeStateError::ForbiddenKey)?;
            if !structured_output && !outputs.contains(key) {
                return Err(CodeStateError::ForbiddenKey);
            }
            if !state_value_matches(kind, value) {
                return Err(CodeStateError::InvalidValue);
            }
        }
        // Returning one fully validated patch keeps failures atomic. This method
        // cannot mutate checkpoint state or publish a successful prefix.
        Ok(values.into_iter().collect())
    }

    fn validate_selection(&self, selected: &[String]) -> Result<(), CodeStateError> {
        if selected.len() > MAX_STATE_KEYS {
            return Err(CodeStateError::SizeLimit);
        }
        let mut seen = BTreeSet::new();
        for key in selected {
            if !seen.insert(key) || (key != "messages" && !self.types.contains_key(key)) {
                return Err(CodeStateError::ForbiddenKey);
            }
        }
        Ok(())
    }
}

// Persisted state can already contain nested values before JSON serialization.
// Bound traversal as well as recursion before invoking the recursive serializer.
fn validate_input_depth(
    value: &Value,
    remaining_depth: usize,
    remaining_values: &mut usize,
) -> Result<(), CodeStateError> {
    if remaining_depth == 0 || *remaining_values == 0 {
        return Err(CodeStateError::SizeLimit);
    }
    *remaining_values -= 1;
    match value {
        Value::Array(values) => {
            for value in values {
                validate_input_depth(value, remaining_depth - 1, remaining_values)?;
            }
        }
        Value::Object(values) => {
            for value in values.values() {
                validate_input_depth(value, remaining_depth - 1, remaining_values)?;
            }
        }
        _ => {}
    }
    Ok(())
}

struct BoundedJson(Vec<u8>);

impl Write for BoundedJson {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        if bytes.len() > MAX_STATE_BYTES.saturating_sub(self.0.len()) {
            return Err(io::Error::other("Code input exceeds its byte limit"));
        }
        self.0.extend_from_slice(bytes);
        Ok(bytes.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

/// Data-free errors: source values and runtime identifiers never enter messages.
#[derive(Debug, Error, PartialEq, Eq)]
pub(super) enum CodeStateError {
    #[error("Code-node state exceeds the supported data size")]
    SizeLimit,
    #[error("Code-node state declarations contain an unsupported type or reserved field")]
    InvalidSchema,
    #[error("Code-node input or output selects an undeclared, reserved, or duplicate field")]
    ForbiddenKey,
    #[error("Code-node state does not match its declared variable types")]
    InvalidValue,
    #[error("Code-node output must contain a valid JSON object of variable updates")]
    MalformedResult,
}
