//! Keep replay control events private and present one call/result pair per ID.

use std::collections::HashMap;

use adk_rust::{AdkError, Content, LlmRequest, Part};

/// ADK persists the original call and its exact replay. Providers require one.
/// Keep the last call occurrence. Never merge changed arguments or repeated results.
pub(super) fn model_continuation(
    mut request: LlmRequest,
    marker: &Content,
) -> adk_rust::Result<LlmRequest> {
    request
        .contents
        .retain(|content| content.role != marker.role || content.parts != marker.parts);
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
    }
    request.contents.retain(|content| !content.parts.is_empty());
    Ok(request)
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
}
