//! Reasoning a model wrote INTO an ANSWER's text, and the rule the agent
//! worker removes it by (`model_text::stream_text`: the toolkit completions,
//! the step-back tools, image descriptions).
//!
//! It is deliberately NOT the rule of two other readers, whose input is not
//! a plain answer (owner decision, #1292 review round 4):
//!
//! * the engines' model client (`elitea-model-client`,
//!   `split_leaked_reasoning`) splits at the LAST `</think>` and keeps the
//!   reasoning as its own field, because an engine answer is a whole
//!   monologue followed by the answer;
//! * the proposal chunker (`elitea-doc-chunk`, `parse_chunk_analysis`)
//!   removes `<think>` blocks ANYWHERE and cuts at a trailing unclosed one,
//!   because it hunts for a JSON object in the reply and a draft object
//!   inside reasoning must never be chosen.
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

use std::borrow::Cow;

const OPEN: &str = "<think>";
const CLOSE: &str = "</think>";

/// A reply's visible text, with what was removed. The text borrows from the
/// input: no reply is copied.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Stripped<'a> {
    /// The reply without its reasoning.
    pub text: Cow<'a, str>,
    /// The reply began a `<think>` block and never closed it.
    pub unclosed: bool,
    /// Reasoning was removed: a closed leading block, or the text before a
    /// bare closing tag.
    pub removed: bool,
}

/// `text` split by the rule above.
#[must_use]
pub fn strip_reasoning(text: &str) -> Stripped<'_> {
    let mut rest = text.trim_start();
    if !rest.starts_with(OPEN) {
        let close = text.find(CLOSE);
        let open = text.find(OPEN);
        return match close {
            Some(close) if open.is_none_or(|open| open > close) => Stripped {
                text: Cow::Borrowed(text[close + CLOSE.len()..].trim_start()),
                unclosed: false,
                removed: true,
            },
            _ => Stripped {
                text: Cow::Borrowed(text),
                ..Stripped::default()
            },
        };
    }
    while let Some(after_open) = rest.strip_prefix(OPEN) {
        let Some(end) = after_open.find(CLOSE) else {
            return Stripped {
                text: Cow::Borrowed(""),
                unclosed: true,
                removed: false,
            };
        };
        rest = after_open[end + CLOSE.len()..].trim_start();
    }
    Stripped {
        text: Cow::Borrowed(rest),
        unclosed: false,
        removed: true,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn text(raw: &str) -> String {
        strip_reasoning(raw).text.into_owned()
    }

    /// Round 4: the no-tag path and the stripped paths borrow, never copy.
    #[test]
    fn plain_and_stripped_text_is_borrowed() {
        for raw in [
            "plain answer",
            "<think>a</think>\nB",
            "<think>a</think><think>b</think>c",
            "a</think>b",
        ] {
            assert!(
                matches!(strip_reasoning(raw).text, Cow::Borrowed(_)),
                "{raw}"
            );
        }
    }

    #[test]
    fn a_leading_block_and_the_whitespace_after_it_are_removed() {
        assert_eq!(text("<think>a</think>\nB"), "B");
        assert_eq!(text("  \n<think>a</think><think>b</think>  C "), "C ");
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
