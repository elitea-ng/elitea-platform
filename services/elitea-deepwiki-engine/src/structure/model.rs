//! The model seam of structure planning, and the Python value semantics
//! the planners apply to the model's JSON.
//!
//! Every call here is `llm.invoke([SystemMessage, HumanMessage])` on the
//! one `ChatOpenAI` the subprocess worker builds: the request settings'
//! model and `max_tokens`, temperature 0.1 ([`Sampling::Default`]; 1.0 for
//! an `o*` model), streamed when `llm_settings.streaming` is on. The
//! cluster and classic planners have no sampling of their own (the
//! deepagents planner, a later unit, asks for 0.0).
//!
//! [`ChatModel`] is the seam: [`LiveModel`] calls the gateway; tests answer
//! from a script and record the request bodies.

use crate::errors::{EngineError, ErrorType};
use crate::llm::{ChatClient, ChatMessage, ChatRequest, Sampling};
use crate::runner::StopSignal;
use serde_json::Value;
use std::future::Future;

/// One chat call that returns the answer's text.
pub trait ChatModel: Sync {
    /// Send `request`; return the answer's content.
    ///
    /// # Errors
    ///
    /// The call failed (after the client's retries) or was stopped.
    fn complete(
        &self,
        request: &ChatRequest,
    ) -> impl Future<Output = Result<String, EngineError>> + Send;
}

/// The gateway, through the invocation's chat client.
#[derive(Debug, Clone)]
pub struct LiveModel {
    pub client: ChatClient,
    pub stop: StopSignal,
}

impl ChatModel for LiveModel {
    async fn complete(&self, request: &ChatRequest) -> Result<String, EngineError> {
        let response = self.client.chat(request, &self.stop, &mut |_| {}).await?;
        Ok(response.content)
    }
}

/// A planner request: the messages, the settings' token budget, the
/// default sampling.
#[must_use]
pub fn user_request(messages: Vec<ChatMessage>) -> ChatRequest {
    ChatRequest {
        sampling: Sampling::Default,
        ..ChatRequest::new(messages)
    }
}

/// Whether `error` is the stop line. A stopped call ends planning; any
/// other failure of a naming call is one Python caught and replaced.
#[must_use]
pub fn is_cancelled(error: &EngineError) -> bool {
    *error == EngineError::cancelled()
}

/// Python truthiness of a JSON value.
#[must_use]
pub fn truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(flag) => *flag,
        Value::Number(number) => number.as_f64().is_some_and(|n| n != 0.0),
        Value::String(text) => !text.is_empty(),
        Value::Array(items) => !items.is_empty(),
        Value::Object(map) => !map.is_empty(),
    }
}

/// `x or default` where the result must be a `str` (a pydantic `str`
/// field): `Ok(None)` for a falsy `x`, `Err` for a truthy non-string.
///
/// # Errors
///
/// A truthy value that is not a string: pydantic refuses it.
pub fn truthy_str(value: Option<&Value>) -> Result<Option<String>, NotAString> {
    match value {
        Some(value) if truthy(value) => match value {
            Value::String(text) => Ok(Some(text.clone())),
            _ => Err(NotAString),
        },
        _ => Ok(None),
    }
}

/// A truthy non-string where a `str` was required.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct NotAString;

/// `int(value)` for a JSON value, as the batched naming reads `page_id`.
///
/// `Ok(None)` is the `TypeError` / `ValueError` Python skipped the entry
/// for; `Err` is the `OverflowError` (an infinite float) it did not catch.
/// Strings follow `int(str)` for ASCII digits (Unicode digits, which Python
/// also takes, are refused).
///
/// # Errors
///
/// An infinite float.
pub fn py_int(value: &Value) -> Result<Option<i64>, Overflow> {
    match value {
        Value::Bool(flag) => Ok(Some(i64::from(*flag))),
        Value::Number(number) => {
            if let Some(int) = number.as_i64() {
                return Ok(Some(int));
            }
            match number.as_f64() {
                Some(float) if float.is_infinite() => Err(Overflow),
                Some(float) if float.is_nan() => Ok(None),
                #[allow(clippy::cast_possible_truncation)]
                Some(float) => Ok(Some(float.trunc() as i64)),
                None => Ok(None),
            }
        }
        Value::String(text) => Ok(int_literal(text)),
        _ => Ok(None),
    }
}

/// An infinite float passed to `int()`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Overflow;

/// `int(text)`: surrounding whitespace, an optional sign, ASCII digits with
/// single underscores between them.
fn int_literal(text: &str) -> Option<i64> {
    let text = crate::graph::pystr::strip(text);
    let (negative, digits) = match text.as_bytes().first() {
        Some(b'-') => (true, &text[1..]),
        Some(b'+') => (false, &text[1..]),
        _ => (false, text),
    };
    if digits.is_empty()
        || digits.starts_with('_')
        || digits.ends_with('_')
        || digits.contains("__")
        || !digits.bytes().all(|b| b.is_ascii_digit() || b == b'_')
    {
        return None;
    }
    let value: i64 = digits.replace('_', "").parse().ok()?;
    Some(if negative { -value } else { value })
}

/// A planner failure with Python's exception class, for the messages.
#[must_use]
pub fn runtime(message: impl Into<String>) -> EngineError {
    EngineError::new(ErrorType::Runtime, message)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn int_follows_python() {
        assert_eq!(py_int(&json!(3)), Ok(Some(3)));
        assert_eq!(py_int(&json!(2.9)), Ok(Some(2)));
        assert_eq!(py_int(&json!(-0.5)), Ok(Some(0)));
        assert_eq!(py_int(&json!(" 4 ")), Ok(Some(4)));
        assert_eq!(py_int(&json!("1_0")), Ok(Some(10)));
        assert_eq!(py_int(&json!("1.0")), Ok(None));
        assert_eq!(py_int(&json!(true)), Ok(Some(1)));
        assert_eq!(py_int(&json!(null)), Ok(None));
        assert_eq!(py_int(&json!([1])), Ok(None));
    }

    #[test]
    fn truthiness_follows_python() {
        for value in [
            json!(null),
            json!(false),
            json!(0),
            json!(0.0),
            json!(""),
            json!([]),
            json!({}),
        ] {
            assert!(!truthy(&value), "{value}");
        }
        for value in [
            json!(true),
            json!(1),
            json!(" "),
            json!([0]),
            json!({"a": null}),
        ] {
            assert!(truthy(&value), "{value}");
        }
        assert_eq!(truthy_str(Some(&json!(""))), Ok(None));
        assert_eq!(truthy_str(Some(&json!(5))), Err(NotAString));
        assert_eq!(truthy_str(Some(&json!("x"))), Ok(Some("x".to_owned())));
    }
}
