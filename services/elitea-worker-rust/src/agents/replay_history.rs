//! Keep replay control events private and present one call/result pair per ID.

use std::collections::HashMap;
use std::sync::Arc;

use adk_rust::schema_adapter::SchemaAdapter;
use adk_rust::{AdkError, Content, FunctionResponseData, Llm, LlmRequest, LlmResponseStream, Part};
use async_trait::async_trait;

mod recovery;

/// Normalize provider history on every turn, including restored sessions.
/// Keep this inside replay adapters. They need the original pending batch.
pub(super) fn provider_model(inner: Arc<dyn Llm>) -> Arc<dyn Llm> {
    Arc::new(ReplayHistoryModel { inner })
}

struct ReplayHistoryModel {
    inner: Arc<dyn Llm>,
}

#[async_trait]
impl Llm for ReplayHistoryModel {
    fn name(&self) -> &str {
        self.inner.name()
    }

    fn schema_adapter(&self) -> &dyn SchemaAdapter {
        self.inner.schema_adapter()
    }

    fn uses_interactions_api(&self) -> bool {
        self.inner.uses_interactions_api()
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        stream: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        recovery::generate(Arc::clone(&self.inner), model_history(request)?, stream).await
    }
}

/// ADK persists the original call and its exact replay. Providers require one.
/// Keep the last call occurrence. Never merge changed arguments or repeated results.
pub(super) fn model_continuation(
    mut request: LlmRequest,
    marker: &Content,
) -> adk_rust::Result<LlmRequest> {
    request
        .contents
        .retain(|content| content.role != marker.role || content.parts != marker.parts);
    model_history(request)
}

/// Also used for preflight measurement; durable checkpoints keep the original replay records.
pub(super) fn model_history(mut request: LlmRequest) -> adk_rust::Result<LlmRequest> {
    let mut calls = HashMap::new();
    let mut results = HashMap::new();
    for (index, content) in request.contents.iter().enumerate() {
        for part in &content.parts {
            match part {
                Part::FunctionCall {
                    id: Some(id),
                    name,
                    args,
                    ..
                } => {
                    if let Some((previous_index, previous_name, previous_args)) =
                        calls.insert(id.clone(), (index, name, args))
                        && (previous_name != name
                            || previous_args != args
                            || previous_index == index
                            || results.contains_key(id))
                    {
                        return Err(invalid_replay());
                    }
                }
                Part::FunctionResponse { id: Some(id), .. }
                    if results.insert(id.clone(), index).is_some() =>
                {
                    return Err(invalid_replay());
                }
                _ => {}
            }
        }
    }
    let last_calls: HashMap<_, _> = calls
        .into_iter()
        .map(|(id, (index, _, _))| (id, index))
        .collect();
    for (index, content) in request.contents.iter_mut().enumerate() {
        content.parts.retain(|part| match part {
            Part::FunctionCall { id: Some(id), .. } => last_calls.get(id) == Some(&index),
            _ => true,
        });
        for part in &mut content.parts {
            if let Part::FunctionResponse {
                function_response, ..
            } = part
            {
                normalize_legacy_skip(&mut function_response.response);
            }
        }
    }
    request.contents.retain(|content| !content.parts.is_empty());
    close_abandoned_calls(&mut request.contents, &results);
    Ok(request)
}

/// A later user turn can follow a failed child without a recorded tool result.
/// Close only that historical provider pair. This is not an execution receipt,
/// a recovery result, or permission to repeat a side effect.
fn close_abandoned_calls(contents: &mut Vec<Content>, results: &HashMap<String, usize>) {
    let Some(boundary) = contents.iter().rposition(|content| {
        content.role == "user"
            && content
                .parts
                .iter()
                .any(|part| matches!(part, Part::Text { text } if !text.is_empty()))
            && !content
                .parts
                .iter()
                .any(|part| matches!(part, Part::FunctionResponse { .. }))
    }) else {
        return;
    };
    let mut missing = Vec::new();
    for (index, content) in contents[..boundary].iter().enumerate() {
        let mut parts = Vec::new();
        for part in &content.parts {
            if let Part::FunctionCall {
                id: Some(id), name, ..
            } = part
                && !results.contains_key(id)
            {
                parts.push(Part::FunctionResponse {
                    id: Some(id.clone()),
                    function_response: FunctionResponseData::new(name, serde_json::json!({
                        "error": {
                            "code": "prior_tool_result_unavailable",
                            "message": "No result was recorded before a later user turn. The operation outcome is unknown. Do not assume success or repeat side effects without checking."
                        }
                    })),
                    annotations: None,
                });
            }
        }
        if !parts.is_empty() {
            missing.push((
                index + 1,
                Content {
                    role: "function".into(),
                    parts,
                },
            ));
        }
    }
    if missing.is_empty() {
        return;
    }
    // Move payloads once; repeated insertion would shift long histories repeatedly.
    let mut repaired = Vec::with_capacity(contents.len() + missing.len());
    let mut missing = missing.into_iter().peekable();
    for (index, content) in std::mem::take(contents).into_iter().enumerate() {
        repaired.push(content);
        if missing
            .peek()
            .is_some_and(|(position, _)| *position == index + 1)
            && let Some((_, result)) = missing.next()
        {
            repaired.push(result);
        }
    }
    *contents = repaired;
}

/// Project the old worker directive without rewriting durable events.
/// Only the exact retired decision shape matches; ordinary tool data stays intact.
fn normalize_legacy_skip(response: &mut serde_json::Value) {
    if response["type"] != "mcp_auth_decision"
        || response["status"] != "declined"
        || response["denial_reason"] != "user_declined"
        || response["next_step"]
            != "Do not retry this toolkit unless the user explicitly asks to authorize it."
    {
        return;
    }
    let Some(result) = response.as_object_mut() else {
        return;
    };
    result.remove("auth_context");
    result.remove("server_url");
    result.insert("scope".into(), "current_run".into());
    result.insert("next_step".into(), "use_other_tools_or_report".into());
    result.insert("message".into(), "The user skipped this toolkit for that run. No protected operation was executed. A later user turn can request this toolkit again through its authorization tool.".into());
}

fn invalid_replay() -> AdkError {
    AdkError::agent("the replayed tool history contains conflicting call identities")
}

#[cfg(test)]
mod tests {
    use adk_rust::FunctionResponseData;
    use serde_json::json;

    use super::*;

    fn call(index: usize) -> Part {
        Part::FunctionCall {
            id: Some(format!("call-{index}")),
            name: "read".into(),
            args: json!({"index": index}),
            thought_signature: None,
        }
    }

    fn result(index: usize) -> Part {
        Part::FunctionResponse {
            id: Some(format!("call-{index}")),
            function_response: FunctionResponseData::new("read", json!({"status": "declined"})),
            annotations: None,
        }
    }

    fn content(role: &str, parts: Vec<Part>) -> Content {
        Content {
            role: role.into(),
            parts,
        }
    }

    #[test]
    fn replay_projection_is_idempotent_and_keeps_each_result_for_bounded_batches() {
        let marker = Content::new("user").with_text("resume fixture");
        for count in 1..=64 {
            let mut contents = vec![content("model", (0..count).map(call).collect())];
            let mut expected = Vec::new();
            for index in 0..count {
                contents.push(marker.clone());
                contents.push(content("model", vec![call(index)]));
                contents.push(content("function", vec![result(index)]));
                expected.push(call(index));
                expected.push(result(index));
            }
            let output = model_continuation(LlmRequest::new("fixture", contents), &marker).unwrap();
            let actual: Vec<_> = output
                .contents
                .iter()
                .flat_map(|c| c.parts.clone())
                .collect();
            assert_eq!(actual, expected);
            let again = model_continuation(output.clone(), &marker).unwrap();
            assert_eq!(
                serde_json::to_value(again.contents).unwrap(),
                serde_json::to_value(output.contents).unwrap()
            );
        }
    }

    #[test]
    fn replay_rejects_changed_calls_and_duplicate_results() {
        let marker = Content::new("user").with_text("resume fixture");
        let mut changed = call(0);
        if let Part::FunctionCall { args, .. } = &mut changed {
            *args = json!({"index": 1});
        }
        for contents in [
            vec![content("model", vec![call(0), call(0)])],
            vec![
                content("model", vec![call(0)]),
                content("model", vec![changed]),
            ],
            vec![
                content("model", vec![call(0)]),
                content("function", vec![result(0), result(0)]),
            ],
            vec![
                content("model", vec![call(0)]),
                content("function", vec![result(0)]),
                content("model", vec![call(0)]),
            ],
        ] {
            assert!(model_continuation(LlmRequest::new("fixture", contents), &marker).is_err());
        }
    }

    #[test]
    fn later_user_turn_closes_missing_history_result_without_a_success_receipt() {
        let original = vec![
            content("model", vec![call(0)]),
            Content::new("user").with_text("New task"),
        ];
        let projected = model_history(LlmRequest::new("fixture", original.clone())).unwrap();
        assert_eq!(projected.contents.len(), 3);
        assert_eq!(
            serde_json::to_value(&projected.contents[0]).unwrap(),
            serde_json::to_value(&original[0]).unwrap()
        );
        assert_eq!(
            serde_json::to_value(&projected.contents[2]).unwrap(),
            serde_json::to_value(&original[1]).unwrap()
        );
        let Part::FunctionResponse {
            id,
            function_response,
            ..
        } = &projected.contents[1].parts[0]
        else {
            panic!("missing historical response");
        };
        assert_eq!(id.as_deref(), Some("call-0"));
        assert_eq!(function_response.name, "read");
        assert_eq!(
            function_response.response["error"]["code"],
            "prior_tool_result_unavailable"
        );
        assert!(function_response.response.get("output").is_none());
        let again = model_history(projected.clone()).unwrap();
        assert_eq!(
            serde_json::to_value(&again.contents).unwrap(),
            serde_json::to_value(&projected.contents).unwrap()
        );
    }

    #[test]
    fn mixed_batch_keeps_real_results_and_closes_only_the_missing_call() {
        let request = LlmRequest::new(
            "fixture",
            vec![
                content("model", vec![call(0), call(1)]),
                content("function", vec![result(0)]),
                Content::new("user").with_text("Next task"),
            ],
        );
        let projected = model_history(request).unwrap();
        let responses: Vec<_> = projected
            .contents
            .iter()
            .flat_map(|c| &c.parts)
            .filter_map(|part| match part {
                Part::FunctionResponse {
                    id,
                    function_response,
                    ..
                } => Some((id.as_deref(), &function_response.response)),
                _ => None,
            })
            .collect();
        assert_eq!(responses.len(), 2);
        assert_eq!(responses[0].0, Some("call-1"));
        assert_eq!(
            responses[0].1["error"]["code"],
            "prior_tool_result_unavailable"
        );
        assert_eq!(responses[1].0, Some("call-0"));
        assert_eq!(responses[1].1["status"], "declined");
    }

    #[test]
    fn pending_recovery_calls_and_recorded_results_are_not_replaced() {
        for contents in [
            vec![
                Content::new("user").with_text("Task"),
                content("model", vec![call(0)]),
            ],
            vec![content("model", vec![call(0)]), Content::new("user")],
            vec![
                content("model", vec![call(0)]),
                content("function", vec![result(0)]),
                Content::new("user").with_text("Next"),
            ],
        ] {
            let projected = model_history(LlmRequest::new("fixture", contents.clone())).unwrap();
            let expected: Vec<_> = contents
                .into_iter()
                .filter(|c| !c.parts.is_empty())
                .collect();
            assert_eq!(
                serde_json::to_value(&projected.contents).unwrap(),
                serde_json::to_value(&expected).unwrap()
            );
        }
    }

    #[test]
    fn old_skip_directive_is_scoped_without_mutating_unrelated_tool_data() {
        let legacy = json!({
            "type": "mcp_auth_decision", "status": "declined",
            "tool_name": "fixture_auth", "denial_reason": "user_declined",
            "next_step": "Do not retry this toolkit unless the user explicitly asks to authorize it.",
            "auth_context": {"resource_metadata": {"provided_settings": {"client_id": "fixture"}}},
            "server_url": "https://resource.example.invalid"
        });
        let mut projected = legacy.clone();
        normalize_legacy_skip(&mut projected);
        assert_eq!(projected["scope"], "current_run");
        assert_eq!(projected["tool_name"], "fixture_auth");
        assert!(projected.get("auth_context").is_none());
        assert!(projected.get("server_url").is_none());
        let once = projected.clone();
        normalize_legacy_skip(&mut projected);
        assert_eq!(projected, once);
        assert!(legacy.get("auth_context").is_some());
        for mut unrelated in [
            json!({"status": "declined"}),
            json!({"next_step": "authorize"}),
            json!(null),
        ] {
            let original = unrelated.clone();
            normalize_legacy_skip(&mut unrelated);
            assert_eq!(unrelated, original);
        }
    }
}
