//! One admitted chat call for a caller that wants the reply's TEXT: the index
//! steps (image descriptions, the proposal chunker), the step-back tools and,
//! next, the Office host.
//!
//! The gateway facades admit exactly one request shape: STREAMING, carrying
//! the generation config the model was bound with (`validate_llm_request`).
//! Anything else is `model_gateway.invalid_request`, raised locally before a
//! byte reaches the gateway. A caller therefore never builds that config by
//! hand: it takes the bound model's own
//! ([`BoundModelFacade::admitted_config`](super::model_facade::BoundModelFacade::admitted_config))
//! and hands it to [`stream_text`], which also owns the reply handling that
//! every such caller needs:
//!
//! * the text is every text delta of the stream, in order (the terminal frame
//!   of the facades carries no text of the deltas before it);
//! * a provider's reasoning deltas are not text;
//! * reasoning a model writes INTO its text is removed
//!   ([`strip_reasoning`]);
//! * a reply cut by `max_tokens` is reported, with whether the cut left
//!   nothing but reasoning ([`TextReply::reasoning_only`]).

use std::sync::Arc;

use adk_rust::futures::StreamExt as _;
use adk_rust::{AdkError, Content, FinishReason, GenerateContentConfig, Llm, LlmRequest, Part};

/// A chat model bound for one call, with the generation config its facade
/// admits.
#[derive(Clone)]
pub(crate) struct AdmittedChat {
    pub(crate) model: Arc<dyn Llm>,
    pub(crate) config: GenerateContentConfig,
}

/// What a model answered in text.
#[derive(Debug, PartialEq, Eq)]
pub(crate) struct TextReply {
    /// The reply without its reasoning, trimmed.
    pub(crate) text: String,
    /// `max_tokens` cut the reply off.
    pub(crate) length_cut: bool,
    /// Nothing but reasoning came back: the text is empty and the reply was
    /// cut off, or it began a `<think>` block it never closed. More
    /// `max_tokens` cures it; another identical attempt does not.
    pub(crate) reasoning_only: bool,
}

/// Why a call produced no [`TextReply`].
#[derive(Debug)]
pub(crate) enum TextFailure {
    /// The model or the gateway refused or failed the call.
    Model(AdkError),
    /// The stream ended the reply with an error code.
    ErrorCode,
    /// The model stopped for a reason other than finishing or `max_tokens`.
    Stopped(FinishReason),
    /// The reply is over the caller's size bound.
    TooLarge,
    /// The stream held no complete reply.
    NoReply,
}

/// One user message on `chat`, and the text of its reply.
///
/// # Errors
///
/// [`TextFailure`].
pub(crate) async fn stream_text(
    chat: &AdmittedChat,
    model_name: &str,
    user: Content,
    max_reply_bytes: usize,
) -> Result<TextReply, TextFailure> {
    let request = LlmRequest {
        model: model_name.to_owned(),
        contents: vec![user],
        config: Some(chat.config.clone()),
        tools: std::collections::HashMap::new(),
        previous_response_id: None,
    };
    let mut responses = chat
        .model
        .generate_content(request, true)
        .await
        .map_err(TextFailure::Model)?;
    let mut raw = String::new();
    let mut ended: Option<bool> = None;
    while let Some(response) = responses.next().await {
        let response = response.map_err(TextFailure::Model)?;
        if response.error_code.is_some() {
            return Err(TextFailure::ErrorCode);
        }
        if let Some(content) = &response.content {
            for part in &content.parts {
                if let Part::Text { text } = part {
                    if raw.len().saturating_add(text.len()) > max_reply_bytes {
                        return Err(TextFailure::TooLarge);
                    }
                    raw.push_str(text);
                }
            }
        }
        if response.partial {
            continue;
        }
        let length_cut = response.finish_reason == Some(FinishReason::MaxTokens);
        if let Some(reason) = response.finish_reason
            && reason != FinishReason::Stop
            && !length_cut
        {
            return Err(TextFailure::Stopped(reason));
        }
        ended = Some(length_cut);
    }
    let length_cut = ended.ok_or(TextFailure::NoReply)?;
    let stripped = strip_reasoning(&raw);
    let reasoning_only = stripped.text.is_empty() && (length_cut || stripped.unclosed);
    Ok(TextReply {
        text: stripped.text,
        length_cut,
        reasoning_only,
    })
}

/// A reply with its reasoning removed.
#[derive(Debug, PartialEq, Eq)]
pub(crate) struct Stripped {
    pub(crate) text: String,
    /// The reply began a `<think>` block and never closed it.
    pub(crate) unclosed: bool,
}

const OPEN: &str = "<think>";
const CLOSE: &str = "</think>";

/// `text` without the reasoning a model wrote into it.
///
/// * `<think>…</think>` blocks at the START of the reply (after whitespace)
///   are removed, one after another;
/// * a `<think>` at the start that is never closed is all reasoning: the
///   result is empty and `unclosed` is set;
/// * a reply with a `</think>` and no `<think>` before it (the template put
///   the opening tag in the prompt) loses everything up to and including it;
/// * a `<think>` anywhere else is literal text and stays.
#[must_use]
pub(crate) fn strip_reasoning(text: &str) -> Stripped {
    let mut rest = text.trim_start();
    if rest.starts_with(OPEN) {
        while let Some(after_open) = rest.strip_prefix(OPEN) {
            let Some(end) = after_open.find(CLOSE) else {
                return Stripped {
                    text: String::new(),
                    unclosed: true,
                };
            };
            rest = after_open[end + CLOSE.len()..].trim_start();
        }
    } else if let Some(close) = rest.find(CLOSE)
        && !rest[..close].contains(OPEN)
    {
        rest = &rest[close + CLOSE.len()..];
    }
    Stripped {
        text: rest.trim().to_owned(),
        unclosed: false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn text(raw: &str) -> String {
        strip_reasoning(raw).text
    }

    #[test]
    fn a_leading_block_is_removed() {
        assert_eq!(text("<think>a</think>\nB"), "B");
        assert_eq!(text("  \n<think>a</think><think>b</think>  C "), "C");
    }

    #[test]
    fn a_reply_with_only_a_closing_tag_loses_everything_up_to_it() {
        assert_eq!(
            text("I should look.</think>\n\nA red square."),
            "A red square."
        );
        assert_eq!(text("a</think>b</think>c"), "b</think>c");
    }

    #[test]
    fn an_unclosed_leading_block_is_all_reasoning() {
        let stripped = strip_reasoning("  <think>Let me think");
        assert_eq!(
            stripped,
            Stripped {
                text: String::new(),
                unclosed: true
            }
        );
        let after_a_closed_one = strip_reasoning("<think>a</think><think>b");
        assert!(after_a_closed_one.unclosed && after_a_closed_one.text.is_empty());
    }

    #[test]
    fn a_think_tag_elsewhere_is_literal() {
        let literal = "Use the <think> tag to reason; it ends with </think>.";
        assert_eq!(text(literal), literal);
        assert_eq!(text("x <think>a</think> y"), "x <think>a</think> y");
        assert_eq!(text("kept <think>cut off"), "kept <think>cut off");
        assert_eq!(text("plain"), "plain");
        assert_eq!(text(""), "");
    }
}
