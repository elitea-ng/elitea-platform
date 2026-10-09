//! Chat-completion chunk primitives: usage, finish reason, reasoning text,
//! and the strict reader of one streamed chunk.
//!
//! The lenient client composes these into its own event handling (it
//! streams text to a callback as each chunk arrives); the strict client
//! reads a whole chunk with [`parse_strict_chunk`] and keeps the state
//! machine around it (events after completion, `[DONE]` before it).

use crate::tool_calls::{ToolCallDelta, ToolCallError, ToolCallLimits, parse_strict_deltas};
use serde_json::Value;

/// The data of the event that ends an OpenAI-style stream.
pub const DONE: &str = "[DONE]";

/// How the sentinel is matched.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DoneMatch {
    /// The data is exactly `[DONE]` (the worker).
    Exact,
    /// The data is text that, trimmed of (Unicode) whitespace, is `[DONE]`
    /// (the model client).
    Trimmed,
}

/// Whether an event's data is the end-of-stream sentinel.
#[must_use]
pub fn is_done(data: &[u8], matching: DoneMatch) -> bool {
    match matching {
        DoneMatch::Exact => data == DONE.as_bytes(),
        DoneMatch::Trimmed => std::str::from_utf8(data).is_ok_and(|text| text.trim() == DONE),
    }
}

/// Token usage as the gateway reported it. Counts are cumulative per
/// request; cached and reasoning tokens are breakdowns of the prompt and
/// completion counts, never additions to them.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Usage {
    pub prompt_tokens: u64,
    pub completion_tokens: u64,
    /// Lenient: as reported (0 when absent). Strict: the sum of the two
    /// counts, which a reported total must equal.
    pub total_tokens: u64,
    /// `completion_tokens_details.reasoning_tokens`.
    pub reasoning_tokens: Option<u64>,
    /// `prompt_tokens_details.cached_tokens`.
    pub cached_tokens: Option<u64>,
}

/// How strictly `usage` is read.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum UsageProfile {
    /// A `usage` that is not an object is no usage; a count that is absent
    /// or not a whole number reads as 0 (a breakdown as absent).
    Lenient,
    /// A non-null `usage` must carry `prompt_tokens` and
    /// `completion_tokens` as whole numbers of at most `max_count`, whose
    /// sum must not pass `max_count`; a non-null `total_tokens` must equal
    /// that sum, and a non-null breakdown must be such a count too.
    Strict { max_count: u64 },
}

/// A `usage` the strict profile refuses.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct InvalidUsage;

impl std::fmt::Display for InvalidUsage {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("the reported usage is malformed")
    }
}

impl std::error::Error for InvalidUsage {}

/// One breakdown count's value: `usage.<group>.<name>`.
fn detail<'a>(usage: &'a Value, group: &str, name: &str) -> Option<&'a Value> {
    usage.get(group).and_then(|details| details.get(name))
}

/// Read a chunk's or a completion's `usage` member.
///
/// # Errors
///
/// Strict only: [`InvalidUsage`] (see [`UsageProfile::Strict`]).
pub fn parse_usage(
    usage: Option<&Value>,
    profile: UsageProfile,
) -> Result<Option<Usage>, InvalidUsage> {
    match profile {
        UsageProfile::Lenient => {
            let Some(usage) = usage.filter(|u| u.is_object()) else {
                return Ok(None);
            };
            let field = |name: &str| usage.get(name).and_then(Value::as_u64).unwrap_or(0);
            Ok(Some(Usage {
                prompt_tokens: field("prompt_tokens"),
                completion_tokens: field("completion_tokens"),
                total_tokens: field("total_tokens"),
                reasoning_tokens: detail(usage, "completion_tokens_details", "reasoning_tokens")
                    .and_then(Value::as_u64),
                cached_tokens: detail(usage, "prompt_tokens_details", "cached_tokens")
                    .and_then(Value::as_u64),
            }))
        }
        UsageProfile::Strict { max_count } => {
            let Some(usage) = usage.filter(|u| !u.is_null()) else {
                return Ok(None);
            };
            let count = |value: Option<&Value>| {
                value
                    .and_then(Value::as_u64)
                    .filter(|count| *count <= max_count)
                    .ok_or(InvalidUsage)
            };
            let prompt = count(usage.get("prompt_tokens"))?;
            let completion = count(usage.get("completion_tokens"))?;
            let total = prompt
                .checked_add(completion)
                .filter(|total| *total <= max_count)
                .ok_or(InvalidUsage)?;
            if let Some(reported) = usage.get("total_tokens").filter(|v| !v.is_null())
                && count(Some(reported))? != total
            {
                return Err(InvalidUsage);
            }
            let breakdown = |group: &str, name: &str| {
                detail(usage, group, name)
                    .filter(|v| !v.is_null())
                    .map(|v| count(Some(v)))
                    .transpose()
            };
            Ok(Some(Usage {
                prompt_tokens: prompt,
                completion_tokens: completion,
                total_tokens: total,
                cached_tokens: breakdown("prompt_tokens_details", "cached_tokens")?,
                reasoning_tokens: breakdown("completion_tokens_details", "reasoning_tokens")?,
            }))
        }
    }
}

/// Why a choice ended.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FinishReason {
    Stop,
    /// `length`: the output token cap.
    Length,
    ContentFilter,
    ToolCalls,
    /// The legacy `function_call`; the strict reader refuses it.
    FunctionCall,
    /// Any other value.
    Other,
}

impl FinishReason {
    #[must_use]
    pub fn parse(value: &str) -> Self {
        match value {
            "stop" => Self::Stop,
            "length" => Self::Length,
            "content_filter" => Self::ContentFilter,
            "tool_calls" => Self::ToolCalls,
            "function_call" => Self::FunctionCall,
            _ => Self::Other,
        }
    }
}

/// How a member of the wrong JSON type is read.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TypeCheck {
    /// As absent.
    Lenient,
    /// As malformed.
    Strict,
}

/// Why a reasoning text was refused.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ReasoningError {
    /// Strict: a reasoning member that is neither a string nor null.
    Malformed,
    /// `reasoning_content` and `reasoning` both present and different.
    Conflicting,
}

impl std::fmt::Display for ReasoningError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::Malformed => "the reasoning text is malformed",
            Self::Conflicting => "the answer carries two different reasoning texts",
        })
    }
}

impl std::error::Error for ReasoningError {}

/// The reasoning text of a message or a delta: `reasoning_content` (vLLM,
/// `DeepSeek`, the gateway) or `reasoning` (newer vLLM). The gateway may
/// emit both spellings of one value, consumed once; both present and
/// different is refused.
///
/// # Errors
///
/// See [`ReasoningError`].
pub fn reasoning_text(part: &Value, check: TypeCheck) -> Result<Option<&str>, ReasoningError> {
    let text = |key: &str| match (part.get(key), check) {
        (Some(Value::String(text)), _) => Ok(Some(text.as_str())),
        (None | Some(Value::Null), _) | (Some(_), TypeCheck::Lenient) => Ok(None),
        (Some(_), TypeCheck::Strict) => Err(ReasoningError::Malformed),
    };
    let content = text("reasoning_content")?;
    let reasoning = text("reasoning")?;
    match (content, reasoning) {
        (Some(a), Some(b)) if a != b => Err(ReasoningError::Conflicting),
        (Some(a), _) => Ok(Some(a)),
        (None, b) => Ok(b),
    }
}

/// One strictly read chunk.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum StrictChunk {
    /// The `[DONE]` sentinel.
    Done,
    /// A chunk that carries nothing but (possibly) usage: `choices` empty
    /// with a non-null `usage`, or one choice with an empty delta and no
    /// finish reason.
    Usage(Option<Usage>),
    Delta(Delta),
}

/// The one choice of a strictly read chunk.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Delta {
    pub usage: Option<Usage>,
    pub content: Option<String>,
    pub reasoning: Option<String>,
    pub tool_calls: Vec<ToolCallDelta>,
    /// Never [`FinishReason::FunctionCall`]: that chunk is refused.
    pub finish: Option<FinishReason>,
}

/// Why a chunk was refused by the strict reader.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ChunkError {
    /// The chunk carries a non-null `error`.
    ProviderError,
    /// Not JSON, or outside the shape the strict reader admits.
    Malformed,
    /// A legacy `function_call` delta or finish reason.
    LegacyFunctionCall,
    /// More tool-call fragments than the limit.
    TooManyToolCalls,
}

impl std::fmt::Display for ChunkError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::ProviderError => "the model provider reported an error",
            Self::Malformed => "the stream chunk is malformed",
            Self::LegacyFunctionCall => "the model returned an unsupported legacy function call",
            Self::TooManyToolCalls => "the stream chunk carries too many tool calls",
        })
    }
}

impl std::error::Error for ChunkError {}

impl From<ToolCallError> for ChunkError {
    fn from(error: ToolCallError) -> Self {
        match error {
            ToolCallError::LegacyFunctionCall => Self::LegacyFunctionCall,
            ToolCallError::TooManyCalls { .. } => Self::TooManyToolCalls,
            _ => Self::Malformed,
        }
    }
}

fn optional_str(value: Option<&Value>) -> Result<Option<&str>, ChunkError> {
    match value {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(text)) => Ok(Some(text)),
        Some(_) => Err(ChunkError::Malformed),
    }
}

/// Read one event's data strictly: the sentinel, or a JSON object with no
/// `error`, a `choices` list of exactly one choice (or none, with usage),
/// whose `delta` is an object of string-or-null members. Usage is
/// telemetry: an invalid `usage` reads as none and never refuses the chunk.
///
/// # Errors
///
/// See [`ChunkError`]; the checks run in this order: provider error,
/// shape, tool calls, finish reason.
pub fn parse_strict_chunk(
    bytes: &[u8],
    tool_limits: &ToolCallLimits,
    max_usage_count: u64,
) -> Result<StrictChunk, ChunkError> {
    if is_done(bytes, DoneMatch::Exact) {
        return Ok(StrictChunk::Done);
    }
    let value: Value = serde_json::from_slice(bytes).map_err(|_| ChunkError::Malformed)?;
    let object = value.as_object().ok_or(ChunkError::Malformed)?;
    if object.get("error").is_some_and(|error| !error.is_null()) {
        return Err(ChunkError::ProviderError);
    }
    let choices = object
        .get("choices")
        .and_then(Value::as_array)
        .ok_or(ChunkError::Malformed)?;
    let usage = parse_usage(
        object.get("usage"),
        UsageProfile::Strict {
            max_count: max_usage_count,
        },
    )
    .ok()
    .flatten();
    if choices.is_empty() {
        return if object.get("usage").is_some_and(|usage| !usage.is_null()) {
            Ok(StrictChunk::Usage(usage))
        } else {
            Err(ChunkError::Malformed)
        };
    }
    let [choice] = choices.as_slice() else {
        return Err(ChunkError::Malformed);
    };
    let choice = choice.as_object().ok_or(ChunkError::Malformed)?;
    let delta_value = choice.get("delta").ok_or(ChunkError::Malformed)?;
    let delta = delta_value.as_object().ok_or(ChunkError::Malformed)?;
    let content = optional_str(delta.get("content"))?.map(str::to_owned);
    let reasoning = reasoning_text(delta_value, TypeCheck::Strict)
        .map_err(|_| ChunkError::Malformed)?
        .map(str::to_owned);
    let tool_calls = parse_strict_deltas(delta, tool_limits)?;
    let finish = optional_str(choice.get("finish_reason"))?;
    if finish.is_none() && content.is_none() && reasoning.is_none() && tool_calls.is_empty() {
        return Ok(StrictChunk::Usage(usage));
    }
    let finish = finish.map(FinishReason::parse);
    if finish == Some(FinishReason::FunctionCall) {
        return Err(ChunkError::LegacyFunctionCall);
    }
    Ok(StrictChunk::Delta(Delta {
        usage,
        content,
        reasoning,
        tool_calls,
        finish,
    }))
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    const MAX: u64 = i32::MAX as u64;

    const LIMITS: ToolCallLimits = ToolCallLimits {
        max_calls: 16,
        max_argument_bytes: 256 * 1024,
        max_name_bytes: 256,
        max_id_bytes: 512,
    };

    fn strict(value: &Value) -> Result<StrictChunk, ChunkError> {
        parse_strict_chunk(value.to_string().as_bytes(), &LIMITS, MAX)
    }

    #[test]
    fn the_sentinel() {
        assert!(is_done(b"[DONE]", DoneMatch::Exact));
        assert!(!is_done(b" [DONE]\n", DoneMatch::Exact));
        assert!(is_done(b" [DONE]\n", DoneMatch::Trimmed));
        assert!(!is_done(b"[DONE]x", DoneMatch::Trimmed));
        assert!(is_done("\u{a0}[DONE]".as_bytes(), DoneMatch::Trimmed));
    }

    #[test]
    fn lenient_usage_reads_what_is_there() {
        let full = json!({"prompt_tokens": 10, "completion_tokens": 9, "total_tokens": 19,
            "completion_tokens_details": {"reasoning_tokens": 6},
            "prompt_tokens_details": {"cached_tokens": 4}});
        assert_eq!(
            parse_usage(Some(&full), UsageProfile::Lenient),
            Ok(Some(Usage {
                prompt_tokens: 10,
                completion_tokens: 9,
                total_tokens: 19,
                reasoning_tokens: Some(6),
                cached_tokens: Some(4),
            }))
        );
        assert_eq!(
            parse_usage(
                Some(&json!({"prompt_tokens": "x", "total_tokens": 3})),
                UsageProfile::Lenient
            ),
            Ok(Some(Usage {
                total_tokens: 3,
                ..Usage::default()
            }))
        );
        assert_eq!(
            parse_usage(Some(&json!(null)), UsageProfile::Lenient),
            Ok(None)
        );
        assert_eq!(
            parse_usage(Some(&json!(7)), UsageProfile::Lenient),
            Ok(None)
        );
        assert_eq!(parse_usage(None, UsageProfile::Lenient), Ok(None));
    }

    #[test]
    fn strict_usage_checks_counts_and_their_total() {
        let profile = UsageProfile::Strict { max_count: MAX };
        let read = |value: Value| parse_usage(Some(&value), profile);
        assert_eq!(
            read(json!({"prompt_tokens": 10, "completion_tokens": 9})),
            Ok(Some(Usage {
                prompt_tokens: 10,
                completion_tokens: 9,
                total_tokens: 19,
                reasoning_tokens: None,
                cached_tokens: None,
            }))
        );
        assert!(
            read(json!({"prompt_tokens": 10, "completion_tokens": 9, "total_tokens": 19})).is_ok()
        );
        assert_eq!(
            read(json!({"prompt_tokens": 10, "completion_tokens": 9, "total_tokens": 20})),
            Err(InvalidUsage)
        );
        assert_eq!(read(json!({"prompt_tokens": 10})), Err(InvalidUsage));
        assert_eq!(
            read(json!({"prompt_tokens": -1, "completion_tokens": 0})),
            Err(InvalidUsage)
        );
        assert_eq!(
            read(json!({"prompt_tokens": 1.5, "completion_tokens": 0})),
            Err(InvalidUsage)
        );
        assert_eq!(
            read(json!({"prompt_tokens": MAX + 1, "completion_tokens": 0})),
            Err(InvalidUsage)
        );
        assert_eq!(
            read(json!({"prompt_tokens": MAX, "completion_tokens": 1})),
            Err(InvalidUsage)
        );
        assert_eq!(read(json!(7)), Err(InvalidUsage));
        assert_eq!(
            read(json!({"prompt_tokens": 1, "completion_tokens": 1,
                "prompt_tokens_details": {"cached_tokens": "x"}})),
            Err(InvalidUsage)
        );
        assert_eq!(
            read(json!({"prompt_tokens": 1, "completion_tokens": 1,
                "prompt_tokens_details": {"cached_tokens": null},
                "completion_tokens_details": {"reasoning_tokens": 1}}))
            .map(|u| u.map(|u| (u.cached_tokens, u.reasoning_tokens))),
            Ok(Some((None, Some(1))))
        );
        assert_eq!(parse_usage(Some(&json!(null)), profile), Ok(None));
    }

    #[test]
    fn finish_reasons() {
        assert_eq!(FinishReason::parse("stop"), FinishReason::Stop);
        assert_eq!(FinishReason::parse("length"), FinishReason::Length);
        assert_eq!(
            FinishReason::parse("content_filter"),
            FinishReason::ContentFilter
        );
        assert_eq!(FinishReason::parse("tool_calls"), FinishReason::ToolCalls);
        assert_eq!(
            FinishReason::parse("function_call"),
            FinishReason::FunctionCall
        );
        assert_eq!(FinishReason::parse("eos"), FinishReason::Other);
    }

    #[test]
    fn reasoning_aliases_are_one_text() {
        let read =
            |value: Value, check| reasoning_text(&value, check).map(|t| t.map(str::to_owned));
        for check in [TypeCheck::Lenient, TypeCheck::Strict] {
            assert_eq!(
                read(json!({"reasoning_content": "a"}), check),
                Ok(Some("a".to_owned()))
            );
            assert_eq!(
                read(json!({"reasoning": "b"}), check),
                Ok(Some("b".to_owned()))
            );
            assert_eq!(
                read(json!({"reasoning_content": "a", "reasoning": "a"}), check),
                Ok(Some("a".to_owned()))
            );
            assert_eq!(
                read(json!({"reasoning_content": "a", "reasoning": "b"}), check),
                Err(ReasoningError::Conflicting)
            );
            assert_eq!(read(json!({"content": "x"}), check), Ok(None));
            assert_eq!(read(json!("not an object"), check), Ok(None));
        }
        assert_eq!(
            read(
                json!({"reasoning_content": 1, "reasoning": "b"}),
                TypeCheck::Lenient
            ),
            Ok(Some("b".to_owned()))
        );
        assert_eq!(
            read(json!({"reasoning_content": 1}), TypeCheck::Strict),
            Err(ReasoningError::Malformed)
        );
    }

    #[test]
    fn strict_chunks_read_one_choice() {
        assert_eq!(
            parse_strict_chunk(b"[DONE]", &LIMITS, MAX),
            Ok(StrictChunk::Done)
        );
        let delta = strict(
            &json!({"choices": [{"delta": {"content": "hi", "reasoning": "r"},
            "finish_reason": "stop"}], "usage": {"prompt_tokens": 1, "completion_tokens": 2}}),
        );
        assert_eq!(
            delta,
            Ok(StrictChunk::Delta(Delta {
                usage: Some(Usage {
                    prompt_tokens: 1,
                    completion_tokens: 2,
                    total_tokens: 3,
                    reasoning_tokens: None,
                    cached_tokens: None,
                }),
                content: Some("hi".to_owned()),
                reasoning: Some("r".to_owned()),
                tool_calls: Vec::new(),
                finish: Some(FinishReason::Stop),
            }))
        );
        // Usage-only chunks; an invalid usage is no usage, not a refusal.
        assert_eq!(
            strict(&json!({"choices": [], "usage": {"prompt_tokens": 1}})),
            Ok(StrictChunk::Usage(None))
        );
        assert_eq!(
            strict(&json!({"choices": [{"delta": {"role": "assistant"}}]})),
            Ok(StrictChunk::Usage(None))
        );
    }

    #[test]
    fn strict_chunks_refuse_what_they_do_not_admit() {
        for (value, expected) in [
            (
                json!({"error": {"message": "x"}}),
                ChunkError::ProviderError,
            ),
            (
                json!({"error": "x", "choices": "y"}),
                ChunkError::ProviderError,
            ),
            (json!([1]), ChunkError::Malformed),
            (json!({}), ChunkError::Malformed),
            (json!({"choices": []}), ChunkError::Malformed),
            (json!({"choices": [], "usage": null}), ChunkError::Malformed),
            (
                json!({"choices": [{"delta": {}}, {"delta": {}}]}),
                ChunkError::Malformed,
            ),
            (json!({"choices": [1]}), ChunkError::Malformed),
            (json!({"choices": [{}]}), ChunkError::Malformed),
            (json!({"choices": [{"delta": []}]}), ChunkError::Malformed),
            (
                json!({"choices": [{"delta": {"content": 1}}]}),
                ChunkError::Malformed,
            ),
            (
                json!({"choices": [{"delta": {"reasoning": 1}}]}),
                ChunkError::Malformed,
            ),
            (
                json!({"choices": [{"delta": {"reasoning_content": "a", "reasoning": "b"}}]}),
                ChunkError::Malformed,
            ),
            (
                json!({"choices": [{"delta": {}, "finish_reason": 1}]}),
                ChunkError::Malformed,
            ),
            (
                json!({"choices": [{"delta": {"function_call": {}}}]}),
                ChunkError::LegacyFunctionCall,
            ),
            (
                json!({"choices": [{"delta": {}, "finish_reason": "function_call"}]}),
                ChunkError::LegacyFunctionCall,
            ),
            (
                json!({"choices": [{"delta": {"tool_calls": vec![json!({"index": 0, "function": {}}); 17]}}]}),
                ChunkError::TooManyToolCalls,
            ),
            (
                json!({"choices": [{"delta": {"tool_calls": [{}]}}]}),
                ChunkError::Malformed,
            ),
        ] {
            assert_eq!(strict(&value), Err(expected), "{value}");
        }
        assert_eq!(
            parse_strict_chunk(b"{not-json}", &LIMITS, MAX),
            Err(ChunkError::Malformed)
        );
    }
}
