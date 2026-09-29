//! Project untrusted adapter output into one atomic, typed graph update.

#![allow(dead_code)] // Bound by the admitted Code runtime during graph assembly.

use std::collections::BTreeMap;

use serde::Deserialize;
use serde_json::{Value, json};

use super::code_state::{CodeStateBoundary, CodeStateError};

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Receipt {
    revision: u8,
    status: String,
    exit_code: Option<i32>,
    stdout: String,
    stderr: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct AdapterResult {
    revision: u8,
    result: Value,
}

/// Message content is constructed here; user JSON cannot supply message roles,
/// tool calls, checkpoint metadata, or runtime control fields.
pub(super) fn project_code_receipt(
    bytes: &[u8],
    boundary: &CodeStateBoundary,
    outputs: &[String],
    structured: bool,
) -> Result<BTreeMap<String, Value>, CodeStateError> {
    if bytes.len() > 512 * 1024 {
        return Err(CodeStateError::SizeLimit);
    }
    let receipt: Receipt =
        serde_json::from_slice(bytes).map_err(|_| CodeStateError::MalformedResult)?;
    if receipt.revision != 1 || receipt.status != "completed" || receipt.exit_code != Some(0) {
        return Err(CodeStateError::MalformedResult);
    }
    // Stderr is diagnostic data, never a state patch or an assistant message.
    drop(receipt.stderr);
    if receipt.stdout.len() > 256 * 1024 + 64 {
        return Err(CodeStateError::SizeLimit);
    }
    let adapter: AdapterResult =
        serde_json::from_str(&receipt.stdout).map_err(|_| CodeStateError::MalformedResult)?;
    if adapter.revision != 1 {
        return Err(CodeStateError::MalformedResult);
    }
    let result_bytes =
        serde_json::to_vec(&adapter.result).map_err(|_| CodeStateError::MalformedResult)?;
    if outputs.len() > 256 || result_bytes.len().saturating_mul(outputs.len().max(1)) > 512 * 1024 {
        return Err(CodeStateError::SizeLimit);
    }
    let mut updates = BTreeMap::new();
    // The legacy adapter returns {result: value}; ordinary output destinations
    // receive that value, rather than treating its object keys as authority.
    for key in outputs.iter().filter(|key| key.as_str() != "messages") {
        updates.insert(key.clone(), adapter.result.clone());
    }
    let mut structured_list_result = None;
    if structured {
        let value = match &adapter.result {
            Value::String(text) => {
                serde_json::from_str(text).map_err(|_| CodeStateError::MalformedResult)?
            }
            value => value.clone(),
        };
        match value {
            Value::Object(values) => updates.extend(values),
            Value::Array(values) => {
                // Unlike legacy Python state, the graph's built-in result
                // channel is a string. Project lists as JSON text here; never
                // allow an arbitrary object to write this reserved channel.
                structured_list_result = Some(
                    serde_json::to_string(&values).map_err(|_| CodeStateError::MalformedResult)?,
                );
            }
            _ => return Err(CodeStateError::MalformedResult),
        }
    }
    let encoded = serde_json::to_vec(&updates).map_err(|_| CodeStateError::MalformedResult)?;
    // Validate the original selection too, including duplicates and messages.
    let mut updates = boundary.validate_updates(&encoded, outputs, structured)?;
    if let Some(result) = structured_list_result {
        updates.insert("result".to_owned(), Value::String(result));
    }
    if outputs.is_empty() || outputs.iter().any(|key| key == "messages") {
        let content =
            serde_json::to_string(&adapter.result).map_err(|_| CodeStateError::MalformedResult)?;
        updates.insert(
            "messages".to_owned(),
            json!([{"role":"assistant", "content":content}]),
        );
    }
    Ok(updates)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn boundary() -> CodeStateBoundary {
        CodeStateBoundary::new(&BTreeMap::from([
            ("count".into(), "int".into()),
            ("payload".into(), "dict".into()),
        ]))
        .unwrap()
    }

    fn receipt(result: &Value) -> Vec<u8> {
        serde_json::to_vec(&json!({"revision":1,"status":"completed","exit_code":0,
            "stdout":json!({"revision":1,"result":result}).to_string(),
            "stderr":"private diagnostic"}))
        .unwrap()
    }

    #[test]
    fn ordinary_output_maps_whole_result_and_constructs_only_assistant_messages() {
        let result = json!({"count":7});
        let update = project_code_receipt(
            &receipt(&result),
            &boundary(),
            &["payload".into(), "messages".into()],
            false,
        )
        .unwrap();
        assert_eq!(update["payload"], result);
        assert_eq!(
            update["messages"],
            json!([{"role":"assistant","content":"{\"count\":7}"}])
        );
        assert_eq!(update.len(), 2);
    }

    #[test]
    fn structured_output_is_atomic_and_cannot_replace_control_state() {
        for result in [
            json!({"count":7,"session_id":"attacker"}),
            json!({"count":7,"messages":[]}),
            json!({"count":"wrong type"}),
        ] {
            assert!(project_code_receipt(&receipt(&result), &boundary(), &[], true).is_err());
        }
        let updates = project_code_receipt(
            &receipt(&json!({"count":7})),
            &boundary(),
            &["count".into()],
            true,
        )
        .unwrap();
        assert_eq!(updates, BTreeMap::from([("count".into(), json!(7))]));
    }

    #[test]
    fn structured_json_string_preserves_named_projection() {
        for value in [json!({"count":7}), json!("{\"count\":7}")] {
            let updates = project_code_receipt(&receipt(&value), &boundary(), &[], true).unwrap();
            assert_eq!(updates["count"], 7);
        }
        let updates =
            project_code_receipt(&receipt(&json!([1, 2])), &boundary(), &[], true).unwrap();
        assert_eq!(updates["result"], "[1,2]");
    }

    #[test]
    fn failed_or_ambiguous_receipts_never_become_updates() {
        let valid = receipt(&json!(7));
        for field in ["status", "exit_code", "revision", "stdout"] {
            let mut value: Value = serde_json::from_slice(&valid).unwrap();
            value[field] = Value::Null;
            assert!(
                project_code_receipt(
                    &serde_json::to_vec(&value).unwrap(),
                    &boundary(),
                    &["count".into()],
                    false
                )
                .is_err()
            );
        }
        assert!(
            project_code_receipt(
                &valid,
                &boundary(),
                &["count".into(), "count".into()],
                false
            )
            .is_err()
        );
    }

    #[test]
    fn repeated_outputs_are_bounded_before_cloning_large_values() {
        let bytes = receipt(&json!({"text":"x".repeat(200_000)}));
        let outputs = vec!["payload".into(); 4];
        assert_eq!(
            project_code_receipt(&bytes, &boundary(), &outputs, false),
            Err(CodeStateError::SizeLimit)
        );
    }
}
