//! Reasoning a model wrote INTO its reply text, and the ONE rule every caller
//! removes it by: the worker (`model_text::stream_text`, every toolkit and
//! index call), the engines' model client (`chat::split_leaked_reasoning`)
//! and the proposal chunker's reply parser (`elitea-doc-chunk`).
//!
//! * A reply that STARTS (after whitespace) with `<think>`: each leading
//!   `<think>…</think>` block is reasoning, removed with the whitespace
//!   directly after it, one block after another; a leading `<think>` never
//!   closed is all reasoning (the visible text is empty, `unclosed` is set).
//! * A reply with a `</think>` and NO `<think>` before it: everything up to
//!   and including that FIRST `</think>` is reasoning, removed with the
//!   whitespace after it. This is Qwen3 on vLLM without a reasoning parser,
//!   which emits only the closing tag (`Let me look…</think>\n\nA red
//!   square.`).
//! * Anything else is literal and untouched: a `<think>` that does not lead
//!   the reply (with or without a later `</think>`) stays as it is.
//!
//! Accepted edge case (owner decision): an ordinary answer that literally
//! mentions a bare `</think>` with no `<think>` before it loses the text
//! before the tag; leaking reasoning into answers is worse. The visible text
//! is not trimmed at its end.

const OPEN: &str = "<think>";
const CLOSE: &str = "</think>";

/// A reply split into its visible text and the reasoning removed from it.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Stripped {
    /// The reply without its reasoning.
    pub text: String,
    /// The reasoning removed (the blocks' contents, joined by a newline, or
    /// the text before a bare closing tag), trimmed.
    pub reasoning: String,
    /// The reply began a `<think>` block and never closed it.
    pub unclosed: bool,
    /// Reasoning was removed: a closed leading block, or the text before a
    /// bare closing tag.
    pub removed: bool,
}

/// `text` split by the rule above.
#[must_use]
pub fn strip_reasoning(text: &str) -> Stripped {
    let mut rest = text.trim_start();
    if !rest.starts_with(OPEN) {
        let close = text.find(CLOSE);
        let open = text.find(OPEN);
        return match close {
            Some(close) if open.is_none_or(|open| open > close) => Stripped {
                text: text[close + CLOSE.len()..].trim_start().to_owned(),
                reasoning: text[..close].trim().to_owned(),
                unclosed: false,
                removed: true,
            },
            _ => Stripped {
                text: text.to_owned(),
                ..Stripped::default()
            },
        };
    }
    let mut reasoning: Vec<&str> = Vec::new();
    while let Some(after_open) = rest.strip_prefix(OPEN) {
        let Some(end) = after_open.find(CLOSE) else {
            reasoning.push(after_open.trim());
            return Stripped {
                text: String::new(),
                reasoning: reasoning.join("\n"),
                unclosed: true,
                removed: false,
            };
        };
        reasoning.push(after_open[..end].trim());
        rest = after_open[end + CLOSE.len()..].trim_start();
    }
    Stripped {
        text: rest.to_owned(),
        reasoning: reasoning.join("\n"),
        unclosed: false,
        removed: true,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn text(raw: &str) -> String {
        strip_reasoning(raw).text
    }

    #[test]
    fn a_leading_block_and_the_whitespace_after_it_are_removed() {
        assert_eq!(text("<think>a</think>\nB"), "B");
        let two = strip_reasoning("  \n<think>a</think><think>b</think>  C ");
        assert_eq!((two.text.as_str(), two.reasoning.as_str()), ("C ", "a\nb"));
    }

    #[test]
    fn the_visible_text_is_not_trimmed() {
        assert_eq!(text("<think>a</think>\n  code\n"), "code\n");
        assert_eq!(text("  indented\n"), "  indented\n");
        assert_eq!(text("line\n"), "line\n");
    }

    #[test]
    fn qwen_bare_closing_tag_ends_the_reasoning() {
        let qwen = strip_reasoning("Let me look…</think>\n\nA red square.");
        assert_eq!(qwen.text, "A red square.");
        assert_eq!(qwen.reasoning, "Let me look…");
        assert!(qwen.removed && !qwen.unclosed);
        assert_eq!(text("a</think>b"), "b");
        // Only the FIRST bare closing tag ends it.
        assert_eq!(text("a</think>b</think>c"), "b</think>c");
        let only = strip_reasoning("thinking only</think>\n");
        assert!(only.removed && only.text.is_empty());
        // Accepted edge case.
        assert_eq!(text("Close it with </think> here"), "here");
    }

    #[test]
    fn an_unclosed_leading_block_is_all_reasoning() {
        let stripped = strip_reasoning("  <think>Let me think");
        assert!(stripped.unclosed && stripped.text.is_empty());
        assert_eq!(stripped.reasoning, "Let me think");
        let after_a_closed_one = strip_reasoning("<think>a</think><think>b");
        assert!(after_a_closed_one.unclosed && after_a_closed_one.text.is_empty());
    }

    #[test]
    fn a_reply_that_is_only_a_closed_block_has_no_text() {
        let stripped = strip_reasoning("<think>all of it</think>\n");
        assert!(stripped.removed && stripped.text.is_empty());
    }

    #[test]
    fn a_think_tag_elsewhere_is_literal() {
        let literal = "Use the <think> tag to reason; it ends with </think>.";
        assert_eq!(text(literal), literal);
        assert_eq!(text("x <think>a</think> y"), "x <think>a</think> y");
        assert_eq!(text("kept <think>cut off"), "kept <think>cut off");
        assert_eq!(text("plain"), "plain");
        assert_eq!(text(""), "");
        assert!(!strip_reasoning("plain").removed);
    }
}
