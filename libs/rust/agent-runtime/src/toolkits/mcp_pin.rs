//! The approved tool pin of a custom MCP server (ADR-0044 decision 1,
//! #1501).
//!
//! Under the platform's `approved_only` connector catalogue, Main freezes an
//! approved custom MCP server's toolkit with `pinned_tools`: the
//! `{name, digest}` of every tool an approved connector request declared. The
//! worker keeps only the tools of the server's live listing whose name AND
//! digest are pinned, so a tool the server later adds, or changes (its
//! description or its arguments), is never offered to the model and so can
//! never be called, until a new request is approved.
//!
//! The digest is the lower-case hex SHA-256 of the canonical JSON (keys
//! sorted by bytes, compact, `serde_json`'s string escaping, and EVERY number
//! written in one form: parsed as an IEEE-754 double and written as
//! ECMAScript's `Number.prototype.toString` writes it, so `0.0`, `1E+3`,
//! `1e-07` and `-0` digest as `0`, `1000`, `1e-7` and `0`) of
//! `{"description": <description>, "inputSchema": <schema or null>, "name": <name>}`,
//! the same form Main's `connectorapproval.ToolDigest` writes. The number
//! rule is what makes the two agree: the worker keeps a server's number text
//! (`arbitrary_precision`), while Main and the request form see it through a
//! float64 or a JavaScript number. The two share test vectors. A settings
//! document without the key is not pinned (the open catalogue mode); a
//! present key that does not parse refuses the toolkit.
//!
//! A listed tool whose NAME is pinned but whose digest is not (the server
//! changed it since the approval) is never silently dropped: it is replaced
//! by a stand-in that fails every call with `tool.mcp.pin_mismatch`, so the
//! model and the run's steps see why, and the worker logs it.

use std::collections::HashSet;
use std::fmt::Write as _;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use async_trait::async_trait;
use serde_json::{Map, Value, json};

/// The frozen settings key Main writes.
pub(crate) const PINNED_TOOLS_SETTING: &str = "pinned_tools";

/// The most pins one toolkit carries (Main's bound on declared tools).
const MAX_PINS: usize = 4096;

/// One pinned tool.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub(crate) struct PinnedTool {
    pub(crate) name: String,
    pub(crate) digest: String,
}

/// The pin of one MCP tool.
#[must_use]
pub(crate) fn tool_digest(name: &str, description: &str, schema: Option<&Value>) -> String {
    let document = json!({
        "description": description,
        "inputSchema": schema.cloned().unwrap_or(Value::Null),
        "name": name,
    });
    let mut canonical = Vec::with_capacity(256);
    if write_pin_canonical(&mut canonical, &document, 0).is_err() {
        // A number outside the double range, or a value nested beyond the
        // bound: no pin can match it.
        return String::new();
    }
    let digest = ring::digest::digest(&ring::digest::SHA256, &canonical);
    let mut hex = String::with_capacity(64);
    for byte in digest.as_ref() {
        let _ = write!(hex, "{byte:02x}");
    }
    hex
}

/// The deepest nesting the pin canonicaliser follows (Main's bound).
const MAX_PIN_DEPTH: usize = 64;

/// Write `value` in the pin's canonical form: keys sorted by bytes, compact,
/// `serde_json`'s string escaping, numbers through [`es6_number`].
fn write_pin_canonical(out: &mut Vec<u8>, value: &Value, depth: usize) -> Result<(), ()> {
    if depth > MAX_PIN_DEPTH {
        return Err(());
    }
    match value {
        Value::Null => out.extend_from_slice(b"null"),
        Value::Bool(true) => out.extend_from_slice(b"true"),
        Value::Bool(false) => out.extend_from_slice(b"false"),
        Value::Number(number) => {
            let parsed = number.to_string().parse::<f64>().map_err(|_| ())?;
            out.extend_from_slice(es6_number(parsed).ok_or(())?.as_bytes());
        }
        Value::String(text) => serde_json::to_writer(&mut *out, text).map_err(|_| ())?,
        Value::Array(items) => {
            out.push(b'[');
            for (index, item) in items.iter().enumerate() {
                if index > 0 {
                    out.push(b',');
                }
                write_pin_canonical(out, item, depth + 1)?;
            }
            out.push(b']');
        }
        Value::Object(members) => {
            let mut sorted = members.iter().collect::<Vec<_>>();
            sorted.sort_unstable_by(|left, right| left.0.as_bytes().cmp(right.0.as_bytes()));
            out.push(b'{');
            for (index, (key, member)) in sorted.into_iter().enumerate() {
                if index > 0 {
                    out.push(b',');
                }
                serde_json::to_writer(&mut *out, key).map_err(|_| ())?;
                out.push(b':');
                write_pin_canonical(out, member, depth + 1)?;
            }
            out.push(b'}');
        }
    }
    Ok(())
}

/// ECMAScript's `Number.prototype.toString` of a finite double (the
/// shortest digits that round-trip, positional between 1e-7 and 1e21,
/// otherwise `d.ddde±n`); `-0` is `0`. `None` for a NaN or an infinity.
/// Main's `connectorapproval.es6Number` is the same rule.
#[must_use]
pub(crate) fn es6_number(value: f64) -> Option<String> {
    if !value.is_finite() {
        return None;
    }
    if value == 0.0 {
        return Some("0".to_owned());
    }
    // Rust's `{:e}` writes the shortest round-trip digits: `-1.2345e-7`.
    let scientific = format!("{value:e}");
    let (mantissa, exponent) = scientific.split_once('e')?;
    let exponent: i64 = exponent.parse().ok()?;
    let (sign, mantissa) = match mantissa.strip_prefix('-') {
        Some(rest) => ("-", rest),
        None => ("", mantissa),
    };
    let digits: String = mantissa.chars().filter(|c| *c != '.').collect();
    let k = i64::try_from(digits.len()).ok()?;
    let n = exponent + 1;
    let body = if k <= n && n <= 21 {
        let zeros = usize::try_from(n - k).ok()?;
        format!("{digits}{}", "0".repeat(zeros))
    } else if 0 < n && n <= 21 {
        let point = usize::try_from(n).ok()?;
        format!("{}.{}", &digits[..point], &digits[point..])
    } else if -6 < n && n <= 0 {
        let zeros = usize::try_from(-n).ok()?;
        format!("0.{}{digits}", "0".repeat(zeros))
    } else {
        let e = n - 1;
        let exponent = if e >= 0 {
            format!("+{e}")
        } else {
            e.to_string()
        };
        if k == 1 {
            format!("{digits}e{exponent}")
        } else {
            format!("{}.{}e{exponent}", &digits[..1], &digits[1..])
        }
    };
    Some(format!("{sign}{body}"))
}

/// The pins in a settings document: `None` when the key is absent, `Err`
/// when it is present and malformed.
pub(crate) fn parse_pinned_tools(
    settings: &Map<String, Value>,
) -> Result<Option<HashSet<PinnedTool>>, ()> {
    let Some(raw) = settings.get(PINNED_TOOLS_SETTING) else {
        return Ok(None);
    };
    let list = raw.as_array().ok_or(())?;
    if list.len() > MAX_PINS {
        return Err(());
    }
    let mut pins = HashSet::with_capacity(list.len());
    for item in list {
        let entry = item.as_object().ok_or(())?;
        let name = entry.get("name").and_then(Value::as_str).ok_or(())?;
        let digest = entry.get("digest").and_then(Value::as_str).ok_or(())?;
        if name.is_empty()
            || digest.len() != 64
            || !digest
                .bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
        {
            return Err(());
        }
        pins.insert(PinnedTool {
            name: name.to_owned(),
            digest: digest.to_owned(),
        });
    }
    Ok(Some(pins))
}

/// Whether a tool's name and live digest are pinned.
#[must_use]
pub(crate) fn is_pinned(pins: &HashSet<PinnedTool>, tool: &dyn Tool) -> bool {
    let schema = tool.parameters_schema();
    let candidate = PinnedTool {
        name: tool.name().to_owned(),
        digest: tool_digest(tool.name(), tool.description(), schema.as_ref()),
    };
    pins.contains(&candidate)
}

/// Keeps the pinned tools; `None` keeps every tool. A tool no pin names is
/// left out (it was never approved). A tool a pin names whose live digest
/// differs is replaced by [`PinMismatchTool`] and logged, never dropped
/// silently.
#[must_use]
pub(crate) fn keep_pinned(
    tools: Vec<Arc<dyn Tool>>,
    pins: Option<&HashSet<PinnedTool>>,
) -> Vec<Arc<dyn Tool>> {
    let Some(pins) = pins else {
        return tools;
    };
    tools
        .into_iter()
        .filter_map(|tool| {
            if is_pinned(pins, tool.as_ref()) {
                return Some(tool);
            }
            if !pins.iter().any(|pin| pin.name == tool.name()) {
                return None;
            }
            tracing::warn!(
                event = "mcp_tool_pin_mismatch",
                tool = tool.name(),
                "an approved MCP tool changed since its approval; it is unavailable until approved again"
            );
            Some(Arc::new(PinMismatchTool {
                name: tool.name().into(),
            }) as Arc<dyn Tool>)
        })
        .collect()
}

/// The error code of a call to a pinned tool the server changed.
pub(crate) const PIN_MISMATCH_CODE: &str = "tool.mcp.pin_mismatch";

/// Stands in for an approved MCP tool whose live description or schema no
/// longer matches its pin. It exposes neither (the change is what is not
/// approved) and fails every call with [`PIN_MISMATCH_CODE`].
struct PinMismatchTool {
    name: Box<str>,
}

#[async_trait]
impl Tool for PinMismatchTool {
    fn name(&self) -> &str {
        &self.name
    }

    fn description(&self) -> &'static str {
        "This MCP tool is unavailable: the server changed it since its connector request was approved. \
         A new connector request for it must be approved."
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({"type": "object", "additionalProperties": true}))
    }

    fn is_read_only(&self) -> bool {
        true
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_core::Result<Value> {
        Err(AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::Forbidden,
            PIN_MISMATCH_CODE,
            "the MCP server changed this tool since its connector request was approved; a new request must be approved",
        ))
    }
}

/// Whether a tool NAME is pinned at all (a guarded tool whose listing could
/// not be read is offered only when some pin names it).
#[must_use]
pub(crate) fn name_is_pinned(pins: Option<&HashSet<PinnedTool>>, name: &str) -> bool {
    pins.is_none_or(|pins| pins.iter().any(|pin| pin.name == name))
}

#[cfg(test)]
mod tests {
    use super::*;
    use adk_core::ToolContext;
    use async_trait::async_trait;

    struct ListedTool {
        name: &'static str,
        description: &'static str,
    }

    #[async_trait]
    impl Tool for ListedTool {
        fn name(&self) -> &str {
            self.name
        }

        fn description(&self) -> &str {
            self.description
        }

        fn parameters_schema(&self) -> Option<Value> {
            Some(json!({"type": "object"}))
        }

        async fn execute(
            &self,
            _context: Arc<dyn ToolContext>,
            _arguments: Value,
        ) -> adk_core::Result<Value> {
            Ok(Value::Null)
        }
    }

    fn listed(name: &'static str, description: &'static str) -> Arc<dyn Tool> {
        Arc::new(ListedTool { name, description })
    }

    /// A tool the server added after the approval, and a tool it changed,
    /// are not offered; with no pins (open mode) everything is.
    #[test]
    fn only_pinned_unchanged_tools_are_kept() {
        let pin = PinnedTool {
            name: "search".to_owned(),
            digest: tool_digest("search", "Search.", Some(&json!({"type": "object"}))),
        };
        let pins = HashSet::from([pin]);
        let kept = keep_pinned(
            vec![
                listed("search", "Search."),
                listed("delete_all", "Delete everything."),
            ],
            Some(&pins),
        );
        assert_eq!(
            kept.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
            ["search"]
        );
        // A changed pinned tool is not dropped: it stands in, and fails.
        let changed = keep_pinned(
            vec![listed("search", "Search. And exfiltrate.")],
            Some(&pins),
        );
        assert_eq!(changed.len(), 1);
        assert_eq!(changed[0].name(), "search");
        assert!(!changed[0].description().contains("exfiltrate"));
        assert_eq!(keep_pinned(vec![listed("delete_all", "")], None).len(), 1);
        assert!(name_is_pinned(Some(&pins), "search"));
        assert!(!name_is_pinned(Some(&pins), "delete_all"));
        assert!(name_is_pinned(None, "delete_all"));
    }

    /// The vector Main's `connectorapproval` digest test asserts too
    /// (services/elitea-main/internal/application/connectorapproval/digest_test.go).
    #[test]
    fn the_digest_matches_mains_vector() {
        let schema = json!({
            "type": "object",
            "properties": {"query": {"type": "string", "description": "a \"quoted\"\n<tag> & ünïcode"}},
            "required": ["query"],
            "additionalProperties": false
        });
        assert_eq!(
            tool_digest("search", "Search the docs.\tTabbed.", Some(&schema)),
            "551f3dee2f607af84054d833c4fdd55ffda6091ef0a6a8106da2a0fa018ada3b"
        );
    }

    /// The number vectors Main's `connectorapproval` digest test asserts
    /// too: every spelling of one double is one canonical number.
    #[test]
    fn numbers_canonicalise_to_one_form() {
        let cases: &[(&str, &str)] = &[
            ("0.0", "0"),
            ("-0", "0"),
            ("-0.0", "0"),
            ("1.0", "1"),
            ("1e-07", "1e-7"),
            ("1E-7", "1e-7"),
            ("1E+3", "1000"),
            ("0.000001", "0.000001"),
            ("1.5", "1.5"),
            ("-2.5e-8", "-2.5e-8"),
            ("1e21", "1e+21"),
            ("1.5E300", "1.5e+300"),
            ("9007199254740993", "9007199254740992"),
            ("12345678901234567890", "12345678901234567000"),
            ("0.1", "0.1"),
            ("100", "100"),
        ];
        for (text, want) in cases {
            let value: f64 = text.parse().expect("number");
            assert_eq!(es6_number(value).as_deref(), Some(*want), "{text}");
        }
        assert_eq!(es6_number(f64::INFINITY), None);
        let written: Value = serde_json::from_str(
            r#"{"type":"object","properties":{"n":{"type":"number","minimum":0.0,"maximum":1E+3,"multipleOf":1e-07,"default":-0}}}"#,
        )
        .expect("schema");
        let normalised: Value = serde_json::from_str(
            r#"{"type":"object","properties":{"n":{"type":"number","minimum":0,"maximum":1000,"multipleOf":1e-7,"default":0}}}"#,
        )
        .expect("schema");
        let digest = tool_digest("bounded", "Numbers.", Some(&written));
        assert_eq!(
            digest,
            tool_digest("bounded", "Numbers.", Some(&normalised))
        );
        assert_eq!(
            digest,
            "b5d7b208c63bd18571ec21160061b959317057c9f5f38786c594ccd262cc7724"
        );
    }

    #[test]
    fn absent_pins_keep_everything_and_malformed_pins_refuse() {
        let mut settings = Map::new();
        assert_eq!(parse_pinned_tools(&settings), Ok(None));
        settings.insert(PINNED_TOOLS_SETTING.into(), json!([{"name": "a"}]));
        assert_eq!(parse_pinned_tools(&settings), Err(()));
        settings.insert(PINNED_TOOLS_SETTING.into(), json!([]));
        assert_eq!(parse_pinned_tools(&settings), Ok(Some(HashSet::new())));
    }
}
