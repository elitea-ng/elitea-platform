//! `resolve_wiki` (`wiki_query.py`): which wiki a question is about, by one
//! model call at temperature 0.
//!
//! The Go host (`run/wikiquery.go`) sends `question`, `wikis` (each
//! `{wiki_id, wiki_title, description}`) and `llm_settings`, reads
//! `success` and `wiki_id`, trims quotes, and validates the id against its
//! registry itself. A model failure is an unsuccessful RESULT, not an
//! invocation error: the host turns it into "could not determine which
//! wiki to query".
//!
//! The request is the Python one: the prompt as a single user message,
//! `temperature` 0 (1 for an `o*` model), `max_completion_tokens` from
//! `llm_settings.max_tokens` or 4000, not streamed, sent to
//! `{api_base}/v1/chat/completions` when the base does not end in `/v1`
//! (`create_llm` appended it). An Anthropic provider goes through the
//! gateway's OpenAI-compatible surface (owner decision 4).

use super::pyfmt;
use crate::errors::{EngineError, ErrorType};
use crate::llm::{ChatClient, ChatMessage, ChatRequest, ModelSettings, Sampling, Transport};
use crate::runner::Context;
use serde_json::{Map, Value, json};

/// `_wiki_list_text`.
#[must_use]
pub fn wiki_list_text(wikis: &[Value]) -> String {
    wikis
        .iter()
        .map(|wiki| {
            let (id, title, description) = match wiki {
                Value::Object(map) => {
                    let get = |key: &str| match map.get(key) {
                        None => String::new(),
                        Some(value) => pyfmt::str_of(value),
                    };
                    let description = match map.get("description") {
                        None | Some(Value::Null) => String::new(),
                        Some(value) if !pyfmt::truthy(value) => String::new(),
                        Some(value) => pyfmt::str_of(value),
                    };
                    (get("wiki_id"), get("wiki_title"), description)
                }
                other => (pyfmt::str_of(other), String::new(), String::new()),
            };
            format!("- {id}: {title} - {}", pyfmt::head(&description, 150))
        })
        .collect::<Vec<_>>()
        .join("\n")
}

/// The prompt.
///
/// # Errors
///
/// A template error (a build error).
pub fn prompt(question: &str, wikis: &[Value]) -> Result<String, EngineError> {
    crate::structure::prompts::format(
        super::prompts::RESOLUTION,
        &[
            ("question", question),
            ("wiki_list_text", wiki_list_text(wikis).as_str()),
        ],
    )
    .map_err(|e| EngineError::new(ErrorType::Runtime, e.to_string()))
}

fn refused(message: &str) -> Value {
    json!({
        "success": false,
        "error": message,
        "error_type": "ValueError",
        "error_category": "invalid_input",
    })
}

fn text_setting<'a>(settings: &'a Map<String, Value>, keys: &[&str]) -> Option<&'a str> {
    keys.iter().find_map(|key| {
        settings
            .get(*key)
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty())
    })
}

/// The request of one resolution (for the parity gate).
#[must_use]
pub fn request(prompt: String, max_tokens: u32) -> ChatRequest {
    let mut chat = ChatRequest::new(vec![ChatMessage::User(prompt)]);
    chat.sampling = Sampling::Deterministic;
    chat.max_tokens = Some(max_tokens);
    chat
}

/// `resolve_wiki(question, wikis, llm_settings)`.
///
/// # Errors
///
/// The stop line. Every other failure is the result's.
pub async fn resolve_wiki(
    arguments: &Map<String, Value>,
    transport: &Transport,
    context: &Context,
) -> Result<Value, EngineError> {
    let question = arguments
        .get("question")
        .map(pyfmt::str_of)
        .unwrap_or_default();
    let wikis: Vec<Value> = match arguments.get("wikis") {
        Some(Value::Array(items)) => items.clone(),
        _ => Vec::new(),
    };
    if wikis.is_empty() {
        return Ok(json!({"success": true, "wiki_id": "NONE"}));
    }
    let empty = Map::new();
    let settings = match arguments.get("llm_settings") {
        Some(Value::Object(map)) => map,
        _ => &empty,
    };
    if text_setting(settings, &["model_name"]).is_none() {
        return Ok(refused(
            "llm_settings carries no model_name, so the wiki cannot be resolved. The wiki_query toolkit's llm_model is what supplies it.",
        ));
    }
    if text_setting(settings, &["api_base", "openai_api_base"]).is_none()
        || text_setting(settings, &["api_key", "openai_api_key"]).is_none()
    {
        return Ok(refused(
            "llm_settings carries no api_base/api_key, so no model can be reached.",
        ));
    }
    let outcome = async {
        let mut model = ModelSettings::from_llm_settings(&Value::Object(settings.clone()))?;
        if !model.api_base.ends_with("/v1") {
            model.api_base.push_str("/v1");
        }
        let max_tokens = match settings.get("max_tokens") {
            None | Some(Value::Null) => 4000,
            Some(_) => model.max_tokens,
        };
        let client = ChatClient::new(transport.clone(), model);
        let chat = request(prompt(&question, &wikis)?, max_tokens);
        client.complete(&chat, context.stop_signal()).await
    }
    .await;
    match outcome {
        Ok(response) => {
            let answer = crate::graph::pystr::strip(&response.content);
            let answer = answer.trim_matches('"').trim_matches('\'');
            Ok(json!({"success": true, "wiki_id": answer}))
        }
        Err(error) if error == EngineError::cancelled() => Err(error),
        Err(error) => Ok(json!({
            "success": false,
            "error": format!("Wiki resolution failed: {}", error.wire_message()),
        })),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_candidate_list_is_legacy_formatted() {
        let wikis = vec![
            json!({"wiki_id": "a--b--main", "wiki_title": "B", "description": "x".repeat(200)}),
            json!({"wiki_id": "c--d--main", "description": null}),
            json!("bare--id--main"),
        ];
        let text = wiki_list_text(&wikis);
        let lines: Vec<&str> = text.lines().collect();
        assert_eq!(lines[0], format!("- a--b--main: B - {}", "x".repeat(150)));
        assert_eq!(lines[1], "- c--d--main:  - ");
        assert_eq!(lines[2], "- bare--id--main:  - ");
        let prompt = prompt("Where?", &wikis[..1]).unwrap_or_default();
        assert!(prompt.starts_with("Given the following question"));
        assert!(prompt.contains("Question: Where?"));
    }
}
