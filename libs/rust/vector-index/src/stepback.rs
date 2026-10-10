//! Step-back search: the model rewrites the question into a generic search
//! query, the index is searched with it, and (for the summary tool) a second
//! model call answers from the results.
//!
//! The model call is the worker's: it implements [`StepbackModel`] with its
//! existing model path. This module renders the SDK's prompts and reads the
//! reply.
//!
//! As in the SDK, `messages` is **not** part of the step-back prompt (its
//! template has no `{messages}`), but it *is* printed into the answer prompt,
//! as Python's `str(list)`. That is kept: the tool schema is frozen
//! (ADR-0030 decision 5).

use std::future::Future;

use serde_json::Value;

use crate::error::Result;
pub use crate::prompts::{GET_ANSWER_PROMPT, STEPBACK_PROMPT};
use crate::pyfmt;

/// The chat model the step-back tools call. One user message holding one
/// text part is sent; the reply is the message `content`.
pub trait StepbackModel: Send + Sync {
    /// Sends `prompt` as a single human message and returns the reply's
    /// `content`: a string, or a list of content blocks
    /// (`{"type": "text", "text": ...}`, `thinking`, `reasoning`, …).
    fn invoke(&self, prompt: &str) -> impl Future<Output = Result<Value>> + Send;
}

/// Python's `str.format` for `{name}` placeholders, in a single pass:
/// substituted text is never scanned again, so a question containing
/// `{messages}` stays as typed.
#[must_use]
pub fn render(template: &str, values: &[(&str, &str)]) -> String {
    let mut out = String::with_capacity(template.len());
    let mut rest = template;
    while let Some(open) = rest.find('{') {
        out.push_str(&rest[..open]);
        let after = &rest[open + 1..];
        let replaced = after.find('}').and_then(|close| {
            let name = &after[..close];
            values
                .iter()
                .find(|(key, _)| *key == name)
                .map(|(_, value)| (*value, close))
        });
        if let Some((value, close)) = replaced {
            out.push_str(value);
            rest = &after[close + 1..];
        } else {
            out.push('{');
            rest = after;
        }
    }
    out.push_str(rest);
    out
}

/// The prompt that rewrites `query` into a generic search query.
#[must_use]
pub fn stepback_prompt(query: &str) -> String {
    render(STEPBACK_PROMPT, &[("input", query)])
}

/// The prompt that answers `query` from `search_results`. Both the results
/// and the messages print as Python would print the list.
#[must_use]
pub fn answer_prompt(query: &str, search_results: &[Value], messages: &[Value]) -> String {
    let results = pyfmt::repr(&Value::Array(search_results.to_vec()));
    let messages = pyfmt::repr(&Value::Array(messages.to_vec()));
    render(
        GET_ANSWER_PROMPT,
        &[
            ("input", query),
            ("search_results", &results),
            ("messages", &messages),
        ],
    )
}

/// `extract_text_from_completion`: a string reply is the text; a list of
/// content blocks contributes its `text` blocks (thinking and reasoning
/// blocks are skipped), joined by a blank line.
#[must_use]
pub fn extract_completion_text(content: &Value) -> String {
    match content {
        Value::String(text) => text.clone(),
        Value::Array(blocks) => {
            let texts: Vec<&str> = blocks
                .iter()
                .filter(|block| block.get("type").and_then(Value::as_str) == Some("text"))
                .map(|block| block.get("text").and_then(Value::as_str).unwrap_or(""))
                .collect();
            texts.join("\n\n")
        }
        Value::Null | Value::Bool(false) => String::new(),
        Value::Number(number) if number.as_f64() == Some(0.0) => String::new(),
        Value::Object(map) if map.is_empty() => String::new(),
        other => pyfmt::display(other),
    }
}

/// The body of the `## Answer` section of an answer-prompt reply: the text
/// after the heading up to the next `## ` heading, trimmed. `None` when the
/// reply has no such section. The tools return the whole reply, as the SDK
/// does; this is for callers that want the answer alone.
#[must_use]
pub fn answer_section(reply: &str) -> Option<&str> {
    let mut offset = 0;
    let mut start = None;
    for line in reply.split_inclusive('\n') {
        let heading = line.trim_end().strip_prefix("## ").map(str::trim);
        match (start, heading) {
            (None, Some(name)) if name.eq_ignore_ascii_case("answer") => {
                start = Some(offset + line.len());
            }
            (Some(from), Some(_)) => return Some(reply[from..offset].trim()),
            _ => {}
        }
        offset += line.len();
    }
    start.map(|from| reply[from..].trim())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn the_stepback_prompt_is_the_sdks_with_the_question_in_place() {
        let prompt = stepback_prompt("What did Ann ship in March 2025?");
        assert_eq!(
            prompt,
            "Your task is to convert provided question into a more generic question that will be used for similarity search.\n\
Remove all not important words, question words, but save all names, dates and acronym as in original question.\n\
\n\
<input>\n\
What did Ann ship in March 2025? \n\
</input>\n\
\n\
Output:\n"
        );
    }

    #[test]
    fn the_answer_prompt_prints_lists_like_python() {
        let results =
            vec![json!({"page_content": "it's x", "metadata": {"a": true}, "score": 0.5})];
        let messages = vec![json!({"role": "user", "content": "hi"})];
        let prompt = answer_prompt("why?", &results, &messages);
        assert!(prompt.starts_with(
            "<search_results>\n[{'page_content': \"it's x\", 'metadata': {'a': True}, 'score': 0.5}]\n</search_results>\n\n<conversation_history>\n[{'role': 'user', 'content': 'hi'}]\n</conversation_history>\n\nPlease answer the question based on provided search results.\n"
        ), "{prompt}");
        assert!(prompt.contains("<question>\nwhy?\n</question>\n## Answer\nAdd <ANSWER> here\n"));
        assert!(prompt.ends_with("## Explanation\nHow did you come up with the answer?\n"));
        let empty = answer_prompt("q", &[], &[]);
        assert!(empty.starts_with("<search_results>\n[]\n</search_results>"));
        assert!(empty.contains("<conversation_history>\n[]\n</conversation_history>"));
    }

    #[test]
    fn rendering_is_single_pass_and_keeps_unknown_braces() {
        assert_eq!(
            render("{a} and {b} {c} { {a}", &[("a", "{b}"), ("b", "B")]),
            "{b} and B {c} { {b}"
        );
    }

    #[test]
    fn completion_text_follows_the_sdk() {
        assert_eq!(extract_completion_text(&json!("plain")), "plain");
        let blocks = json!([
            {"type": "thinking", "thinking": "hmm"},
            {"type": "text", "text": "first"},
            {"type": "reasoning", "reasoning": "r"},
            {"type": "text", "text": "second"},
            {"type": "image"}
        ]);
        assert_eq!(extract_completion_text(&blocks), "first\n\nsecond");
        assert_eq!(extract_completion_text(&json!([{"type": "thinking"}])), "");
        assert_eq!(extract_completion_text(&json!(null)), "");
        assert_eq!(extract_completion_text(&json!(7)), "7");
    }

    #[test]
    fn the_answer_section_ends_at_the_next_heading() {
        let reply = "## Answer\nIt shipped on Friday.\nSecond line.\n\n## Score\n90\n\n## Citations\n- a (1)\n";
        assert_eq!(
            answer_section(reply),
            Some("It shipped on Friday.\nSecond line.")
        );
        assert_eq!(answer_section("## Answer\nonly this"), Some("only this"));
        assert_eq!(answer_section("no headings"), None);
        assert_eq!(answer_section("## Score\n1"), None);
    }
}
