//! The CDP records the bridge reads and writes (plain Rust, every OS).
//!
//! The driver writes `{id, method, params, sessionId?}` and reads
//! `{id, result}`, `{id, error}` and `{method, params, sessionId}`, one
//! JSON message per NUL-terminated record (`elitea-browser-session`'s
//! `cdp` module). `WebView2` takes a method name and the parameters as a
//! JSON text, and answers with a JSON text; this module turns one into the
//! other.

use serde_json::{Value, json};

use crate::PAGE_SESSION;

/// A command, as `WebView2` takes it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Command {
    pub id: u64,
    pub method: String,
    /// The parameters as a JSON object text (`{}` when there are none).
    pub params: String,
    /// A child target's session; `None`: the page itself.
    pub session: Option<String>,
}

/// The largest record the bridge accepts from the driver.
pub const MAX_COMMAND_BYTES: usize = 16 << 20;

/// Read one record. `None`: not a command (it is dropped; the driver then
/// times out on it, as it would on a dead engine).
#[must_use]
pub fn command(record: &[u8]) -> Option<Command> {
    if record.len() > MAX_COMMAND_BYTES {
        return None;
    }
    let message: Value = serde_json::from_slice(record).ok()?;
    let id = message["id"].as_u64()?;
    let method = message["method"].as_str()?.to_owned();
    let params = match message.get("params") {
        None | Some(Value::Null) => "{}".to_owned(),
        Some(params @ Value::Object(_)) => params.to_string(),
        Some(_) => return None,
    };
    let session = message["sessionId"]
        .as_str()
        .filter(|session| *session != PAGE_SESSION && !session.is_empty())
        .map(str::to_owned);
    Some(Command {
        id,
        method,
        params,
        session,
    })
}

/// A command's answer as a record. `failed`: `WebView2` reported the call
/// as failed, and `answer` is then its error object (`{code, message}`)
/// when it has one.
#[must_use]
pub fn reply(id: u64, failed: bool, answer: &str) -> Vec<u8> {
    let answer: Option<Value> = serde_json::from_str(answer).ok();
    let message = if failed {
        let error = answer
            .filter(|error| error["message"].is_string())
            .map_or_else(
                || json!({ "code": -32000, "message": "the page refused the command" }),
                |error| {
                    json!({
                        "code": error["code"].as_i64().unwrap_or(-32000),
                        "message": error["message"],
                    })
                },
            );
        json!({ "id": id, "error": error })
    } else {
        json!({ "id": id, "result": answer.unwrap_or_else(|| json!({})) })
    };
    record(&message)
}

/// A command that never reached `WebView2` (the page is gone, the call was
/// refused at once).
#[must_use]
pub fn refused(id: u64, why: &str) -> Vec<u8> {
    record(&json!({ "id": id, "error": { "code": -32000, "message": why } }))
}

/// An event as a record. `session` empty: the page's own.
#[must_use]
pub fn event(method: &str, params: &str, session: &str) -> Vec<u8> {
    let params: Value = serde_json::from_str(params).unwrap_or_else(|_| json!({}));
    let session = if session.is_empty() {
        PAGE_SESSION
    } else {
        session
    };
    record(&json!({ "method": method, "params": params, "sessionId": session }))
}

fn record(message: &Value) -> Vec<u8> {
    let mut bytes = message.to_string().into_bytes();
    bytes.push(0);
    bytes
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parse(record: &[u8]) -> Value {
        assert_eq!(record.last(), Some(&0), "NUL-terminated");
        serde_json::from_slice(&record[..record.len() - 1]).unwrap()
    }

    #[test]
    fn a_page_command_goes_to_the_page_and_a_child_one_to_its_session() {
        let page = command(
            br#"{"id":7,"method":"Page.navigate","params":{"url":"https://example.com/"},"sessionId":"elitea-webview2-page"}"#,
        )
        .unwrap();
        assert_eq!(page.id, 7);
        assert_eq!(page.method, "Page.navigate");
        assert_eq!(page.session, None);
        let params: Value = serde_json::from_str(&page.params).unwrap();
        assert_eq!(params["url"], "https://example.com/");

        let child = command(br#"{"id":8,"method":"DOM.enable","sessionId":"F1"}"#).unwrap();
        assert_eq!(child.session.as_deref(), Some("F1"));
        assert_eq!(child.params, "{}");

        let bare = command(br#"{"id":9,"method":"Browser.getVersion","params":{}}"#).unwrap();
        assert_eq!(bare.session, None, "no browser target: the page answers");
    }

    #[test]
    fn records_that_are_not_commands_are_dropped() {
        assert_eq!(command(b"not json"), None);
        assert_eq!(command(br#"{"method":"Page.enable"}"#), None, "no id");
        assert_eq!(command(br#"{"id":1}"#), None, "no method");
        assert_eq!(
            command(br#"{"id":1,"method":"Page.enable","params":[1]}"#),
            None,
            "params must be an object"
        );
        let huge = vec![b' '; MAX_COMMAND_BYTES + 1];
        assert_eq!(command(&huge), None);
    }

    #[test]
    fn answers_carry_the_result_or_the_error() {
        let ok = parse(&reply(3, false, r#"{"frameTree":{"frame":{"id":"F"}}}"#));
        assert_eq!(ok["id"], 3);
        assert_eq!(ok["result"]["frameTree"]["frame"]["id"], "F");

        let empty = parse(&reply(4, false, ""));
        assert_eq!(empty["result"], json!({}));

        let error = parse(&reply(
            5,
            true,
            r#"{"code":-32601,"message":"'Target.getTargets' wasn't found"}"#,
        ));
        assert_eq!(
            error["error"]["code"], -32601,
            "the unsupported count reads it"
        );
        assert_eq!(
            error["error"]["message"],
            "'Target.getTargets' wasn't found"
        );

        let opaque = parse(&reply(6, true, "E_FAIL"));
        assert_eq!(opaque["error"]["code"], -32000);
        assert!(opaque.get("result").is_none());

        let gone = parse(&refused(7, "the page is closed"));
        assert_eq!(gone["error"]["message"], "the page is closed");
    }

    #[test]
    fn events_name_their_session_and_the_page_has_its_own() {
        let page = parse(&event("Page.loadEventFired", r#"{"timestamp":1.5}"#, ""));
        assert_eq!(page["method"], "Page.loadEventFired");
        assert_eq!(page["params"]["timestamp"], 1.5);
        assert_eq!(page["sessionId"], PAGE_SESSION);
        assert!(page.get("id").is_none(), "an event, not an answer");

        let frame = parse(&event("Network.loadingFinished", "{}", "F1"));
        assert_eq!(frame["sessionId"], "F1");

        let broken = parse(&event("Page.frameNavigated", "not json", ""));
        assert_eq!(broken["params"], json!({}));
    }
}
