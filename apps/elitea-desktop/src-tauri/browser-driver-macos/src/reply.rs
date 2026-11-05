//! What the page script ([`crate::SCRIPT`]) answers, read on the host.
//!
//! Every call into the driver world answers one JSON string: `{"ok": …}` or
//! `{"error": {"code", "message"}}`. The error codes a script may give are
//! the three it can know ([`SCRIPT_CODES`]); anything else is `internal`.
//! A snapshot answers a [`RawPage`]: the DOM and ARIA walk, one
//! [`RawNode`] per element or text run, which [`to_node`] turns into the
//! contract's [`Node`] (the secure decision is made again here, with the
//! contract's own [`is_secure_field`], so a field the script got wrong
//! still shows no value).

use elitea_browser_tools::snapshot::{Node, State, is_secure_field};
use elitea_browser_tools::{BrowserError, ErrorCode};
use serde::Deserialize;
use serde_json::Value;

/// The codes a script answer may carry; the script knows no others.
pub const SCRIPT_CODES: &[ErrorCode] = &[
    ErrorCode::StaleRef,
    ErrorCode::SecureFieldRefused,
    ErrorCode::InvalidArgument,
];

/// Read one script answer.
///
/// # Errors
///
/// The script's own error (one of [`SCRIPT_CODES`], else `internal`), or
/// `internal` for an answer that is not the protocol.
pub fn parse(text: &str) -> Result<Value, BrowserError> {
    let malformed = || BrowserError::new(ErrorCode::Internal, "the page script gave no answer");
    let mut answer: Value = serde_json::from_str(text).map_err(|_| malformed())?;
    if let Some(ok) = answer.get_mut("ok") {
        return Ok(ok.take());
    }
    let error = answer.get("error").ok_or_else(malformed)?;
    let code = error
        .get("code")
        .and_then(Value::as_str)
        .and_then(ErrorCode::parse)
        .filter(|code| SCRIPT_CODES.contains(code))
        .unwrap_or(ErrorCode::Internal);
    // The script's messages are its own fixed strings; an exception's text
    // is never passed on (it can quote the page).
    let message = match code {
        ErrorCode::Internal => "the page script failed",
        _ => error
            .get("message")
            .and_then(Value::as_str)
            .unwrap_or("the page script refused"),
    };
    Err(BrowserError::new(code, cut(message, 200)))
}

fn cut(text: &str, max: usize) -> String {
    text.chars().take(max).collect()
}

/// One element or text run of the walk.
#[derive(Clone, Debug, Default, Deserialize, PartialEq, Eq)]
pub struct RawNode {
    /// Role.
    pub r: String,
    /// Accessible name.
    #[serde(default)]
    pub n: String,
    /// Value (never sent for a secure field).
    #[serde(default)]
    pub v: Option<String>,
    /// State flags as the grammar spells them (`checked`, `level=2`).
    #[serde(default)]
    pub s: Vec<String>,
    /// Actionable although its role is not interactive.
    #[serde(default)]
    pub a: bool,
    /// The element's number in its frame's document (0: a text run).
    #[serde(default)]
    pub k: u64,
    /// `type` and `autocomplete` of an `input` or `textarea`.
    #[serde(default)]
    pub t: Option<String>,
    #[serde(default)]
    pub ac: Option<String>,
    /// A cross-origin frame's address: its content is read in that frame.
    #[serde(default)]
    pub x: Option<String>,
    #[serde(default)]
    pub c: Vec<RawNode>,
}

/// A frame's walk.
#[derive(Clone, Debug, Default, Deserialize, PartialEq, Eq)]
pub struct RawPage {
    /// The document's random id: a new document has a new one.
    pub doc: String,
    pub url: String,
    pub title: String,
    pub nodes: Vec<RawNode>,
    /// The `k` of every cross-origin frame element, in document order.
    #[serde(default)]
    pub frames: Vec<u64>,
}

/// A frame's document and where it is (`state`).
#[derive(Clone, Debug, Default, Deserialize, PartialEq, Eq)]
pub struct RawState {
    pub doc: String,
    pub url: String,
    pub title: String,
    /// `document.readyState`.
    #[serde(default)]
    pub ready: String,
}

/// A click's answer: the page cancelled the scripted events and nothing
/// changed, so it may take real input only.
#[derive(Clone, Copy, Debug, Default, Deserialize, PartialEq)]
pub struct RawClick {
    #[serde(default)]
    pub ignored: bool,
    #[serde(default)]
    pub rect: Option<RawRect>,
}

/// A rectangle in the top viewport's CSS pixels.
#[derive(Clone, Copy, Debug, Default, Deserialize, PartialEq)]
pub struct RawRect {
    pub x: f64,
    pub y: f64,
    pub width: f64,
    pub height: f64,
}

/// Read a typed answer.
///
/// # Errors
///
/// `internal` when it is not that shape.
pub fn typed<T: for<'de> Deserialize<'de>>(value: Value) -> Result<T, BrowserError> {
    serde_json::from_value(value)
        .map_err(|_| BrowserError::new(ErrorCode::Internal, "the page script answered oddly"))
}

/// A state flag as the grammar spells it; `None` for anything else.
#[must_use]
pub fn state(token: &str) -> Option<State> {
    Some(match token {
        "checked" => State::Checked,
        "checked=mixed" => State::CheckedMixed,
        "pressed" => State::Pressed,
        "pressed=mixed" => State::PressedMixed,
        "selected" => State::Selected,
        "expanded" => State::Expanded,
        "collapsed" => State::Collapsed,
        "disabled" => State::Disabled,
        "readonly" => State::Readonly,
        "required" => State::Required,
        "invalid" => State::Invalid,
        "focused" => State::Focused,
        _ => {
            let level: u8 = token.strip_prefix("level=")?.parse().ok()?;
            State::Level(level.clamp(1, 9))
        }
    })
}

/// `raw` as a contract node. `key` maps an element number (`k`, non-zero)
/// to the driver's key for it; a text run gets key 0.
pub fn to_node(raw: RawNode, key: &mut impl FnMut(u64) -> u64) -> Node {
    let secure = (raw.t.is_some() || raw.ac.is_some())
        && is_secure_field(raw.t.as_deref(), raw.ac.as_deref());
    let mut node = Node::new(&raw.r, &raw.n);
    node.states = raw.s.iter().filter_map(|token| state(token)).collect();
    node.actionable = raw.a;
    if raw.k != 0 {
        node.key = key(raw.k);
    }
    if secure {
        node.states.push(State::Secure);
    } else {
        node.value = raw.v;
    }
    if !secure {
        node.children = raw.c.into_iter().map(|child| to_node(child, key)).collect();
    }
    node
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn an_answer_is_ok_or_a_known_error() {
        assert_eq!(parse(r#"{"ok":{"a":1}}"#).unwrap(), json!({"a": 1}));
        let stale = parse(r#"{"error":{"code":"stale_ref","message":"gone"}}"#).unwrap_err();
        assert_eq!(stale.code(), ErrorCode::StaleRef);
        assert_eq!(stale.message(), "gone");
        let secure = parse(r#"{"error":{"code":"secure_field_refused","message":"no"}}"#);
        assert_eq!(secure.unwrap_err().code(), ErrorCode::SecureFieldRefused);
    }

    #[test]
    fn a_script_cannot_claim_another_code_or_quote_the_page() {
        // `user_active` is the host's to say, never a page script's.
        let claimed = parse(r#"{"error":{"code":"user_active","message":"page text"}}"#);
        let claimed = claimed.unwrap_err();
        assert_eq!(claimed.code(), ErrorCode::Internal);
        assert_eq!(claimed.message(), "the page script failed");
        for junk in ["", "null", "[]", "{}", r#"{"error":7}"#, "not json"] {
            assert_eq!(
                parse(junk).unwrap_err().code(),
                ErrorCode::Internal,
                "{junk}"
            );
        }
    }

    #[test]
    fn states_are_the_grammar_tokens() {
        assert_eq!(state("level=3"), Some(State::Level(3)));
        assert_eq!(state("level=0"), Some(State::Level(1)));
        assert_eq!(state("checked=mixed"), Some(State::CheckedMixed));
        assert_eq!(state("secure"), None, "secure is the host's decision");
        assert_eq!(state("level=x"), None);
        assert_eq!(state("bogus"), None);
    }

    fn raw(value: Value) -> RawNode {
        serde_json::from_value(value).unwrap()
    }

    #[test]
    fn a_secure_field_keeps_no_value_and_no_children() {
        let mut keys = Vec::new();
        let mut key = |k| {
            keys.push(k);
            k + 100
        };
        for (t, ac) in [
            (Some("password"), None),
            (Some("text"), Some("cc-number")),
            (None, Some("section-x cc-csc")),
            (Some("text"), Some("new-password")),
        ] {
            let node = to_node(
                raw(json!({
                    "r": "textbox", "n": "Field", "k": 4, "v": "hunter2",
                    "t": t, "ac": ac, "c": [{"r": "text", "n": "hunter2"}]
                })),
                &mut key,
            );
            assert!(node.is_secure(), "{t:?} {ac:?}");
            assert_eq!(node.value, None);
            assert!(node.children.is_empty());
            assert_eq!(node.key, 104);
        }
        let plain = to_node(
            raw(
                json!({"r": "textbox", "n": "Name", "k": 5, "v": "Ada", "t": "text", "ac": "cc-name"}),
            ),
            &mut key,
        );
        assert!(!plain.is_secure());
        assert_eq!(plain.value.as_deref(), Some("Ada"));
        assert_eq!(keys, [4, 4, 4, 4, 5]);
    }

    #[test]
    fn a_text_run_has_no_key_and_children_keep_order() {
        let mut next = 0;
        let mut key = |_| {
            next += 1;
            next
        };
        let node = to_node(
            raw(json!({
                "r": "form", "n": "Sign in", "k": 9, "s": ["required", "nope"],
                "c": [{"r": "text", "n": "Email"}, {"r": "textbox", "n": "Email", "k": 10}]
            })),
            &mut key,
        );
        assert_eq!(node.key, 1);
        assert_eq!(node.states, [State::Required]);
        assert_eq!(node.children[0].key, 0);
        assert_eq!(node.children[1].key, 2);
    }

    #[test]
    fn pages_clicks_and_states_read() {
        let page: RawPage =
            typed(json!({"doc": "d", "url": "u", "title": "t", "nodes": []})).unwrap();
        assert!(page.frames.is_empty());
        let click: RawClick = typed(
            json!({"ignored": true, "rect": {"x": 1.0, "y": 2.0, "width": 3.0, "height": 4.0}}),
        )
        .unwrap();
        assert!(click.ignored);
        assert!(typed::<RawState>(json!({"doc": 1})).is_err());
    }
}
