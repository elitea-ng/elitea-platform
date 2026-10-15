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
//!   nothing but reasoning ([`TextReply::reasoning_only`]);
//! * the claim's toolkit completion budget ([`ModelCallBudget`]) is charged
//!   only for a request that is sent to the gateway: a request the facade
//!   refuses locally (before a byte leaves the worker) gets its unit back,
//!   and an exhausted budget is the typed error
//!   [`TOOLKIT_BUDGET_EXHAUSTED_CODE`], never a sentence to match on.

use std::sync::Arc;
use std::sync::atomic::{AtomicU32, Ordering};

use adk_rust::futures::StreamExt as _;
use adk_rust::{
    AdkError, Content, ErrorCategory, ErrorComponent, FinishReason, GenerateContentConfig, Llm,
    LlmRequest, Part, RetryHint,
};

/// The model completions a claim's toolkits may make in total (the step-back
/// tools, the Office host's prompts): one shared counter per claim, handed to
/// every per-call binding. It is NOT the agent's own `max_model_turns`: each
/// binding is a single-use model (`max_model_turns = 1`), so these calls never
/// spend the agent's turns.
#[derive(Clone, Debug)]
pub(crate) struct ModelCallBudget {
    remaining: Arc<AtomicU32>,
}

/// Completions one claim's toolkits may make when the deployment sets none.
pub(crate) const DEFAULT_TOOLKIT_MODEL_CALLS: u32 = 200;

impl ModelCallBudget {
    #[must_use]
    pub(crate) fn new(calls: u32) -> Self {
        Self {
            remaining: Arc::new(AtomicU32::new(calls)),
        }
    }

    /// Takes one completion; `false` once none is left.
    pub(crate) fn try_take(&self) -> bool {
        self.remaining
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |left| {
                left.checked_sub(1)
            })
            .is_ok()
    }

    /// Gives back a unit taken for a request that was never sent.
    pub(crate) fn refund(&self) {
        let _ = self
            .remaining
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |left| {
                left.checked_add(1)
            });
    }
}

/// The stable code of a call refused because the claim's toolkit completion
/// budget is spent. Callers classify on it, never on a message.
pub(crate) const TOOLKIT_BUDGET_EXHAUSTED_CODE: &str = "model.toolkit_budget_exhausted";
/// The sentence of that refusal: what the user can do about it.
pub(crate) const TOOLKIT_BUDGET_EXHAUSTED: &str = "this run used up its model calls for tools; start a new run or ask an operator to raise toolkit_model_call_cap";

/// The typed error of a call the claim's toolkit budget refused: rate
/// limited in kind (a new run has a fresh budget), never retried within the
/// run, and no request was made.
#[must_use]
pub(crate) fn toolkit_budget_exhausted() -> AdkError {
    AdkError::new(
        ErrorComponent::Model,
        ErrorCategory::RateLimited,
        TOOLKIT_BUDGET_EXHAUSTED_CODE,
        TOOLKIT_BUDGET_EXHAUSTED,
    )
    .with_retry(RetryHint::default())
}

/// Whether the facade raised `error` BEFORE the request was sent: its own
/// admission (the request shape, its size, its encoding, the binding's turn
/// limit, a reused single-use binding, the context budget). Such a call
/// reached no gateway and is not
/// charged to the budget. Everything else (a gateway refusal, a 429, a 5xx,
/// a lost connection) may have reached it and counts.
#[must_use]
pub(crate) fn raised_before_send(error: &AdkError) -> bool {
    matches!(
        error.code,
        "model_gateway.invalid_request"
            | "model_gateway.request_too_large"
            | "model_gateway.request_encoding"
            | "model_gateway.turn_limit"
            | "model_gateway.completion_reused"
            | "model_gateway.completion_state"
            | "anthropic_gateway.invalid_request"
            | "anthropic_gateway.request_too_large"
            | "anthropic_gateway.sampling_unsupported"
            | "anthropic_gateway.turn_limit"
            | "anthropic_gateway.completion_reused"
            | "anthropic_gateway.completion_state"
            | "context_budget_exceeded"
            | "model_request_bytes_exceeded"
    )
}

/// A chat model bound for one call, with the generation config its facade
/// admits and the claim's completion budget, if it has one.
#[derive(Clone)]
pub(crate) struct AdmittedChat {
    pub(crate) model: Arc<dyn Llm>,
    pub(crate) config: GenerateContentConfig,
    pub(crate) budget: Option<ModelCallBudget>,
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
    /// The claim's completion budget is spent; no request was made. As an
    /// error: [`toolkit_budget_exhausted`].
    BudgetExhausted,
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
    if let Some(budget) = &chat.budget
        && !budget.try_take()
    {
        return Err(TextFailure::BudgetExhausted);
    }
    let request = LlmRequest {
        model: model_name.to_owned(),
        contents: vec![user],
        config: Some(chat.config.clone()),
        tools: std::collections::HashMap::new(),
        previous_response_id: None,
    };
    // The unit is taken above, before the call: two calls racing for the
    // last unit cannot both go out. A refusal the facade raised before
    // sending gives it back; once sent (a gateway 429 or 5xx included) the
    // call counts, it reached the gateway.
    let mut responses = match chat.model.generate_content(request, true).await {
        Ok(responses) => responses,
        Err(error) => {
            if let Some(budget) = &chat.budget
                && raised_before_send(&error)
            {
                budget.refund();
            }
            return Err(TextFailure::Model(error));
        }
    };
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
    // Nothing but reasoning: a block that was never closed, a reply that was
    // entirely a closed block, or a reply cut off with nothing in it.
    let reasoning_only =
        stripped.text.trim().is_empty() && (length_cut || stripped.unclosed || stripped.removed);
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
    /// Reasoning was removed: a closed leading block, or the text before a
    /// bare closing tag.
    pub(crate) removed: bool,
}

const OPEN: &str = "<think>";
const CLOSE: &str = "</think>";

/// `text` without the reasoning a model wrote into it.
///
/// * A reply that STARTS (after whitespace) with `<think>`: each leading
///   `<think>…</think>` block is removed with the whitespace directly after
///   it, one block after another; a leading `<think>` never closed is all
///   reasoning (the result is empty and `unclosed` is set).
/// * A reply with a `</think>` and NO `<think>` before it: everything up to
///   and including that first `</think>` is reasoning, removed with the
///   whitespace after it. This is Qwen3 on vLLM without a reasoning parser,
///   which emits only the closing tag (`Let me look…</think>\n\nA red
///   square.`).
/// * Anything else is literal and untouched: a `<think>` that is not
///   leading (with or without a later `</think>`) stays as it is.
///
/// Accepted edge case: an ordinary answer that literally mentions a bare
/// `</think>` with no `<think>` before it loses the text before the tag.
/// The visible text is not trimmed at its end.
#[must_use]
pub(crate) fn strip_reasoning(text: &str) -> Stripped {
    let mut rest = text.trim_start();
    if !rest.starts_with(OPEN) {
        let close = text.find(CLOSE);
        let open = text.find(OPEN);
        return match close {
            Some(close) if open.is_none_or(|open| open > close) => Stripped {
                text: text[close + CLOSE.len()..].trim_start().to_owned(),
                unclosed: false,
                removed: true,
            },
            _ => Stripped {
                text: text.to_owned(),
                unclosed: false,
                removed: false,
            },
        };
    }
    while let Some(after_open) = rest.strip_prefix(OPEN) {
        let Some(end) = after_open.find(CLOSE) else {
            return Stripped {
                text: String::new(),
                unclosed: true,
                removed: false,
            };
        };
        rest = after_open[end + CLOSE.len()..].trim_start();
    }
    Stripped {
        text: rest.to_owned(),
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
        assert_eq!(text("  \n<think>a</think><think>b</think>  C "), "C ");
    }

    #[test]
    fn the_visible_text_is_not_trimmed() {
        assert_eq!(text("<think>a</think>\n  code\n"), "code\n");
        assert_eq!(text("  indented\n"), "  indented\n");
        assert_eq!(text("line\n"), "line\n");
    }

    #[test]
    fn a_closing_tag_without_an_opening_one_ends_the_reasoning() {
        // Owner decision (review round 3): Qwen3 on vLLM without a reasoning
        // parser emits only the closing tag; what precedes it is reasoning.
        assert_eq!(
            text("Let me look…</think>\n\nA red square."),
            "A red square."
        );
        assert_eq!(text("a</think>b"), "b");
        // Only the FIRST bare closing tag ends it.
        assert_eq!(text("a</think>b</think>c"), "b</think>c");
        let only = strip_reasoning("thinking only</think>\n");
        assert!(only.removed && only.text.is_empty());
        // Accepted edge case: an answer that mentions a bare closing tag
        // loses the text before it.
        assert_eq!(text("Close it with </think> here"), "here");
        // A `<think>` before the closing tag (not leading) keeps it literal.
        assert_eq!(text("x <think>a</think> y"), "x <think>a</think> y");
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
    }

    #[test]
    fn a_refund_gives_a_unit_back() {
        let budget = ModelCallBudget::new(1);
        assert!(budget.try_take());
        budget.refund();
        assert!(budget.try_take());
        assert!(!budget.try_take());
    }

    #[test]
    fn the_budget_refusal_is_typed() {
        let error = toolkit_budget_exhausted();
        assert_eq!(error.code, TOOLKIT_BUDGET_EXHAUSTED_CODE);
        assert_eq!(error.category, ErrorCategory::RateLimited);
        assert!(
            !error.is_retryable(),
            "a spent budget is not retried in the run"
        );
        assert!(error.message.contains("toolkit_model_call_cap"));
    }

    #[test]
    fn the_budget_stops_at_its_cap() {
        let budget = ModelCallBudget::new(2);
        let other = budget.clone();
        assert!(budget.try_take());
        assert!(other.try_take());
        assert!(!budget.try_take());
        assert!(!other.try_take());
    }
}
