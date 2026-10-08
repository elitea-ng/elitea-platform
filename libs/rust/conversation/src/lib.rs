//! An agent's conversation, and its summarisation (`LangChain`'s
//! `SummarizationMiddleware`, with the thresholds the Python agents set).
//! The engines' tool-calling loops (the `DeepWiki` `ask` and deep research,
//! Inventory's `investigate`) keep their conversation as [`Msg`]s and call
//! [`compact`] before each model call, so a long run is summarised instead
//! of overflowing the model's context.
//!
//! * Thresholds: with a model profile that names `max_input_tokens`
//!   (`langchain_openai` / `langchain_anthropic` profile data, embedded as
//!   `model_profiles.json`), summarise at 85 % of it and keep 10 %; without
//!   one, summarise at 170 000 tokens and keep the last 6 messages. Both
//!   `ask_engine` and `deepagents.compute_summarization_defaults` chose so.
//! * Tokens are `count_tokens_approximately` (characters / 4, 3.3 for an
//!   Anthropic model, 3 tokens per message), scaled by the last reported
//!   usage when the model reported one.
//! * The cut keeps a tool call with its results; the summarised messages
//!   are rendered with `get_buffer_string(format="xml")` into the summary
//!   prompt, sent as one user message, and replaced by
//!   `Here is a summary of the conversation to date:\n\n{summary}`.
//!
//! Deliberate differences: the summary call is not streamed (Python's
//! `ask` model streamed it, and its fragments could reach the answer
//! stream); deep research uses the same replacement message as `ask`
//! (`deepagents` also wrote the evicted history to
//! `/conversation_history/{session}.md` and named that file).

use elitea_engine_core::errors::EngineError;
use elitea_engine_core::pyjson;
use elitea_engine_core::pystr;
use elitea_engine_core::pyvalue::repr_value;
use elitea_model_client::chat::{ChatMessage, ChatRequest, ChatResponse, ToolCall};
use serde_json::{Map, Value};
use std::collections::HashMap;
use std::future::Future;
use std::sync::LazyLock;

/// `langchain.agents.middleware.summarization.DEFAULT_SUMMARY_PROMPT`
/// (`{messages}`).
pub const DEFAULT_SUMMARY_PROMPT: &str = include_str!("summary_prompt.txt");

/// One tool call of an assistant message.
#[derive(Debug, Clone, PartialEq)]
pub struct Call {
    pub id: String,
    pub name: String,
    /// The JSON text the model wrote.
    pub raw: String,
    /// The parsed arguments; `None` when the text is not a JSON object.
    pub args: Option<Map<String, Value>>,
}

impl Call {
    #[must_use]
    pub fn from_tool_call(call: &ToolCall) -> Self {
        Self {
            id: call.id.clone(),
            name: call.name.clone(),
            raw: call.arguments.clone(),
            args: call.parsed_arguments().ok(),
        }
    }

    fn args_value(&self) -> Value {
        Value::Object(self.args.clone().unwrap_or_default())
    }
}

/// One message of the conversation (the system prompt is not one).
#[derive(Debug, Clone, PartialEq)]
pub enum Msg {
    User(String),
    Ai {
        content: String,
        calls: Vec<Call>,
        /// The `total_tokens` the model reported, when it did.
        total_tokens: Option<u64>,
    },
    Tool {
        call_id: String,
        name: String,
        content: String,
    },
}

impl Msg {
    /// The message as the request carries it.
    #[must_use]
    pub fn to_chat(&self) -> ChatMessage {
        match self {
            Self::User(text) => ChatMessage::User(text.clone()),
            Self::Ai { content, calls, .. } => ChatMessage::Assistant {
                content: (!content.is_empty()).then(|| content.clone()),
                tool_calls: calls
                    .iter()
                    .map(|c| ToolCall {
                        id: c.id.clone(),
                        name: c.name.clone(),
                        // `LangChain` sends the PARSED arguments back,
                        // re-encoded (`json.dumps(args, ensure_ascii=False)`).
                        arguments: match &c.args {
                            Some(args) => {
                                pyjson::dumps_with(&Value::Object(args.clone()), None, false)
                            }
                            None => c.raw.clone(),
                        },
                    })
                    .collect(),
            },
            Self::Tool {
                call_id, content, ..
            } => ChatMessage::Tool {
                tool_call_id: call_id.clone(),
                content: content.clone(),
            },
        }
    }
}

/// `repr(message.tool_calls)`.
fn tool_calls_repr(calls: &[Call]) -> String {
    let items: Vec<Value> = calls
        .iter()
        .map(|c| {
            let mut call = Map::new();
            call.insert("name".into(), Value::from(c.name.clone()));
            call.insert("args".into(), c.args_value());
            call.insert("id".into(), Value::from(c.id.clone()));
            call.insert("type".into(), Value::from("tool_call"));
            Value::Object(call)
        })
        .collect();
    repr_value(&Value::Array(items))
}

/// `count_tokens_approximately`, with usage scaling when `scale`.
#[must_use]
pub fn count_tokens(messages: &[Msg], chars_per_token: f64, scale: bool) -> u64 {
    let mut count = 0.0_f64;
    let mut last_total: Option<u64> = None;
    let mut at_last: f64 = 0.0;
    let mut saw_ai = false;
    for message in messages {
        let chars = match message {
            Msg::User(text) => chars(text) + "user".len(),
            Msg::Ai { content, calls, .. } => {
                let mut n = chars(content) + "assistant".len();
                if !calls.is_empty() {
                    n += chars(&tool_calls_repr(calls));
                }
                n
            }
            Msg::Tool {
                call_id,
                name,
                content,
            } => chars(content) + chars(call_id) + "tool".len() + chars(name),
        };
        #[allow(clippy::cast_precision_loss)]
        let chars = chars as f64;
        count += (chars / chars_per_token).ceil() + 3.0;
        if let Msg::Ai { total_tokens, .. } = message {
            saw_ai = true;
            if let Some(total) = total_tokens {
                last_total = Some(*total);
                at_last = count;
            }
        }
    }
    if scale
        && messages.len() > 1
        && saw_ai
        && at_last > 0.0
        && let Some(total) = last_total
    {
        #[allow(clippy::cast_precision_loss)]
        let factor = (total as f64 / at_last).clamp(1.0, 1.25);
        count *= factor;
    }
    #[allow(clippy::cast_possible_truncation, clippy::cast_sign_loss)]
    let tokens = count.ceil().max(0.0) as u64;
    tokens
}

static PROFILES: LazyLock<HashMap<(bool, String), Option<u64>>> = LazyLock::new(|| {
    let parsed: Value =
        serde_json::from_str(include_str!("model_profiles.json")).unwrap_or(Value::Null);
    let mut table = HashMap::new();
    for (anthropic, key) in [(false, "openai"), (true, "anthropic")] {
        if let Some(Value::Object(models)) = parsed.get(key) {
            for (name, limit) in models {
                table.insert((anthropic, name.clone()), limit.as_u64());
            }
        }
    }
    table
});

/// The `max_input_tokens` of a model's profile, when it has one.
#[must_use]
pub fn max_input_tokens(model: &str, anthropic: bool) -> Option<u64> {
    PROFILES
        .get(&(anthropic, model.to_owned()))
        .copied()
        .flatten()
}

/// The summarisation policy of one agent.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Policy {
    /// Summarise at this many tokens.
    pub trigger_tokens: u64,
    /// Keep this many tokens (`Some`) or the last 6 messages (`None`).
    pub keep_tokens: Option<u64>,
    pub chars_per_token: f64,
    /// Whether reported usage scales the count (a model that reports it).
    pub scale: bool,
}

/// Messages kept when there is no profile.
pub const KEEP_MESSAGES: usize = 6;

impl Policy {
    /// The policy for `model` (see the module comment).
    #[must_use]
    pub fn for_model(model: &str, anthropic: bool, scale: bool) -> Self {
        let chars_per_token = if anthropic { 3.3 } else { 4.0 };
        match max_input_tokens(model, anthropic) {
            Some(max) => {
                #[allow(
                    clippy::cast_precision_loss,
                    clippy::cast_possible_truncation,
                    clippy::cast_sign_loss
                )]
                let fraction = |f: f64| ((max as f64 * f) as u64).max(1);
                Self {
                    trigger_tokens: fraction(0.85),
                    keep_tokens: Some(fraction(0.10)),
                    chars_per_token,
                    scale,
                }
            }
            None => Self {
                trigger_tokens: 170_000,
                keep_tokens: None,
                chars_per_token,
                scale,
            },
        }
    }

    /// `_should_summarize`: the count, or the last reported usage, at the
    /// trigger.
    #[must_use]
    pub fn should_summarize(&self, messages: &[Msg]) -> bool {
        if count_tokens(messages, self.chars_per_token, self.scale) >= self.trigger_tokens {
            return true;
        }
        let reported = messages.iter().rev().find_map(|m| match m {
            Msg::Ai { total_tokens, .. } => Some(*total_tokens),
            _ => None,
        });
        self.scale
            && reported
                .flatten()
                .is_some_and(|total| total > 0 && total >= self.trigger_tokens)
    }

    /// `_determine_cutoff_index`.
    #[must_use]
    pub fn cutoff(&self, messages: &[Msg]) -> usize {
        match self.keep_tokens {
            None => safe_cutoff(messages, KEEP_MESSAGES),
            Some(target) => self.token_cutoff(messages, target),
        }
    }

    fn token_cutoff(&self, messages: &[Msg], target: u64) -> usize {
        if messages.is_empty() || count_tokens(messages, self.chars_per_token, self.scale) <= target
        {
            return 0;
        }
        let (mut left, mut right) = (0, messages.len());
        let mut candidate = messages.len();
        let iterations = usize::BITS - messages.len().leading_zeros() + 1;
        for _ in 0..iterations {
            if left >= right {
                break;
            }
            let mid = usize::midpoint(left, right);
            if count_tokens(&messages[mid..], self.chars_per_token, false) <= target {
                candidate = mid;
                right = mid;
            } else {
                left = mid + 1;
            }
        }
        if candidate == messages.len() {
            candidate = left;
        }
        if candidate >= messages.len() {
            if messages.len() == 1 {
                return 0;
            }
            candidate = messages.len() - 1;
        }
        safe_cutoff_point(messages, candidate)
    }
}

fn safe_cutoff(messages: &[Msg], keep: usize) -> usize {
    if messages.len() <= keep {
        return 0;
    }
    safe_cutoff_point(messages, messages.len() - keep)
}

/// `_find_safe_cutoff_point`: never split a call from its results.
fn safe_cutoff_point(messages: &[Msg], cutoff: usize) -> usize {
    if !matches!(messages.get(cutoff), Some(Msg::Tool { .. })) {
        return cutoff;
    }
    let mut ids = Vec::new();
    let mut index = cutoff;
    while let Some(Msg::Tool { call_id, .. }) = messages.get(index) {
        if !call_id.is_empty() {
            ids.push(call_id.clone());
        }
        index += 1;
    }
    for i in (0..cutoff).rev() {
        if let Msg::Ai { calls, .. } = &messages[i]
            && calls
                .iter()
                .any(|c| !c.id.is_empty() && ids.contains(&c.id))
        {
            return i;
        }
    }
    index
}

fn xml_escape(text: &str) -> String {
    text.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
}

/// `xml.sax.saxutils.quoteattr`.
fn quoteattr(text: &str) -> String {
    let escaped = xml_escape(text)
        .replace('\n', "&#10;")
        .replace('\r', "&#13;")
        .replace('\t', "&#9;");
    if escaped.contains('"') {
        if escaped.contains('\'') {
            format!("\"{}\"", escaped.replace('"', "&quot;"))
        } else {
            format!("'{escaped}'")
        }
    } else {
        format!("\"{escaped}\"")
    }
}

/// `get_buffer_string(messages, format="xml")`.
#[must_use]
pub fn xml_buffer(messages: &[Msg]) -> String {
    messages
        .iter()
        .map(|message| match message {
            Msg::User(text) => simple("human", text),
            Msg::Tool { content, .. } => simple("tool", content),
            Msg::Ai { content, calls, .. } if calls.is_empty() => simple("ai", content),
            Msg::Ai { content, calls, .. } => {
                let mut parts = vec![format!("<message type={}>", quoteattr("ai"))];
                if !content.is_empty() {
                    parts.push(format!("  <content>{}</content>", xml_escape(content)));
                }
                for call in calls {
                    let args = pyjson::dumps_with(&call.args_value(), None, false);
                    parts.push(format!(
                        "  <tool_call id={} name={}>{}</tool_call>",
                        quoteattr(&call.id),
                        quoteattr(&call.name),
                        xml_escape(&args)
                    ));
                }
                parts.push("</message>".to_owned());
                parts.join("\n")
            }
        })
        .collect::<Vec<_>>()
        .join("\n")
}

fn simple(kind: &str, text: &str) -> String {
    let body = if text.is_empty() {
        String::new()
    } else {
        xml_escape(text)
    };
    format!("<message type={}>{body}</message>", quoteattr(kind))
}

/// The summary request text: `prompt.format(messages=…).rstrip()`.
#[must_use]
pub fn summary_request(prompt: &str, messages: &[Msg]) -> String {
    let buffer = xml_buffer(messages);
    prompt
        .replacen("{messages}", &buffer, 1)
        .trim_end_matches(pystr::is_space)
        .to_owned()
}

/// The message that replaces the summarised history.
#[must_use]
pub fn summary_message(summary: &str) -> Msg {
    Msg::User(format!(
        "Here is a summary of the conversation to date:\n\n{summary}"
    ))
}

/// `len(text)` (code points).
fn chars(text: &str) -> usize {
    text.chars().count()
}

/// Summarise `messages` when `policy` says it is due: the messages before
/// the cut are rendered into `prompt` (`{messages}`), sent through `call`
/// as one user message (at most `max_tokens` back), and replaced by one
/// [`summary_message`]. Otherwise, or when there is nothing to cut, the
/// messages come back as they were.
///
/// # Errors
///
/// The summary call failed.
pub async fn compact<F, Fut>(
    policy: &Policy,
    prompt: &str,
    max_tokens: Option<u32>,
    messages: Vec<Msg>,
    call: F,
) -> Result<Vec<Msg>, EngineError>
where
    F: FnOnce(ChatRequest) -> Fut,
    Fut: Future<Output = Result<ChatResponse, EngineError>>,
{
    if !policy.should_summarize(&messages) {
        return Ok(messages);
    }
    let cutoff = policy.cutoff(&messages);
    if cutoff == 0 {
        return Ok(messages);
    }
    let (old, kept) = messages.split_at(cutoff);
    let mut request = ChatRequest::new(vec![ChatMessage::User(summary_request(prompt, old))]);
    request.max_tokens = max_tokens;
    let response = call(request).await?;
    let mut out = vec![summary_message(pystr::strip(&response.content))];
    out.extend_from_slice(kept);
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn call(id: &str) -> Call {
        Call {
            id: id.into(),
            name: "think".into(),
            raw: r#"{"reflection": "x"}"#.into(),
            args: serde_json::from_str(r#"{"reflection": "x"}"#).ok(),
        }
    }

    #[test]
    fn counts_like_langchain() {
        // ceil(9/4)+3 = 6 for "user"+5 chars.
        assert_eq!(count_tokens(&[Msg::User("hello".into())], 4.0, false), 6);
        let ai = Msg::Ai {
            content: String::new(),
            calls: vec![call("c1")],
            total_tokens: None,
        };
        let repr =
            "[{'name': 'think', 'args': {'reflection': 'x'}, 'id': 'c1', 'type': 'tool_call'}]";
        let expected = (repr.len() + 9).div_ceil(4) + 3;
        assert_eq!(count_tokens(&[ai], 4.0, false), expected as u64);
    }

    #[test]
    fn the_cut_keeps_calls_with_results() {
        let messages = vec![
            Msg::User("q".into()),
            Msg::Ai {
                content: String::new(),
                calls: vec![call("c1")],
                total_tokens: None,
            },
            Msg::Tool {
                call_id: "c1".into(),
                name: "think".into(),
                content: "r".into(),
            },
            Msg::Ai {
                content: "a".into(),
                calls: Vec::new(),
                total_tokens: None,
            },
        ];
        assert_eq!(safe_cutoff_point(&messages, 2), 1);
        assert_eq!(safe_cutoff(&messages, 6), 0);
        let xml = xml_buffer(&messages[..2]);
        assert_eq!(
            xml,
            "<message type=\"human\">q</message>\n<message type=\"ai\">\n  <tool_call id=\"c1\" name=\"think\">{\"reflection\": \"x\"}</tool_call>\n</message>"
        );
        assert_eq!(quoteattr("a\"b"), "'a\"b'");
    }

    #[tokio::test]
    async fn a_long_conversation_is_summarised_and_keeps_its_tail() {
        let policy = Policy {
            trigger_tokens: 50,
            keep_tokens: None,
            chars_per_token: 4.0,
            scale: false,
        };
        let mut messages = vec![Msg::User("q".repeat(400))];
        for n in 0..8 {
            messages.push(Msg::Ai {
                content: format!("step {n}"),
                calls: Vec::new(),
                total_tokens: None,
            });
        }
        let compacted = compact(
            &policy,
            "Summarise:\n{messages}",
            Some(64),
            messages,
            |request| async move {
                assert_eq!(request.max_tokens, Some(64));
                let ChatMessage::User(text) = &request.messages[0] else {
                    panic!("one user message");
                };
                assert!(text.starts_with("Summarise:\n<message type=\"human\">qqq"));
                Ok(ChatResponse {
                    content: "  the gist  ".to_owned(),
                    ..ChatResponse::default()
                })
            },
        )
        .await
        .unwrap_or_default();
        assert_eq!(compacted.len(), 1 + KEEP_MESSAGES);
        assert_eq!(
            compacted[0],
            Msg::User("Here is a summary of the conversation to date:\n\nthe gist".to_owned())
        );
        let short = vec![Msg::User("hi".to_owned())];
        let kept = compact(&policy, "{messages}", None, short.clone(), |_| async {
            Err(EngineError::cancelled())
        })
        .await
        .unwrap_or_default();
        assert_eq!(kept, short);
    }

    #[test]
    fn profiles_set_the_thresholds() {
        let policy = Policy::for_model("gpt-4o", false, false);
        assert_eq!(policy.trigger_tokens, 108_800);
        assert_eq!(policy.keep_tokens, Some(12_800));
        let policy = Policy::for_model("some-gateway-model", false, false);
        assert_eq!(policy.trigger_tokens, 170_000);
        assert_eq!(policy.keep_tokens, None);
    }
}
