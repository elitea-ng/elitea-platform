//! Text helpers the Azure DevOps indexing connectors share: HTML fields
//! reduced to text (the SDK's `sanitize`), entity decoding, the test case
//! `Microsoft.VSTS.TCM.Steps` XML rendered as a numbered list, and the
//! `index_data` argument readers.

use std::fmt::Write as _;

use serde_json::{Map, Value};

/// Decode the entities HTML and XML fields carry (`&lt;`, `&gt;`, `&amp;`,
/// `&quot;`, `&apos;`, `&#39;`, `&nbsp;`, decimal and hex references).
#[must_use]
pub fn decode_entities(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut rest = text;
    while let Some(start) = rest.find('&') {
        out.push_str(&rest[..start]);
        rest = &rest[start..];
        let Some(end) = rest[..rest.len().min(12)].find(';') else {
            out.push('&');
            rest = &rest[1..];
            continue;
        };
        let entity = &rest[1..end];
        let decoded = match entity {
            "lt" => Some('<'),
            "gt" => Some('>'),
            "amp" => Some('&'),
            "quot" => Some('"'),
            "apos" => Some('\''),
            "nbsp" => Some(' '),
            _ => entity
                .strip_prefix('#')
                .and_then(|number| {
                    number.strip_prefix(['x', 'X']).map_or_else(
                        || number.parse::<u32>().ok(),
                        |hex| u32::from_str_radix(hex, 16).ok(),
                    )
                })
                .and_then(char::from_u32),
        };
        if let Some(character) = decoded {
            out.push(character);
            rest = &rest[end + 1..];
        } else {
            out.push('&');
            rest = &rest[1..];
        }
    }
    out.push_str(rest);
    out
}

/// HTML reduced to text: tags dropped (block-level ones become line
/// breaks), entities decoded, white space collapsed, no blank lines.
#[must_use]
pub fn html_to_text(html: &str) -> String {
    let mut out = String::with_capacity(html.len());
    let mut rest = html;
    while let Some(start) = rest.find('<') {
        out.push_str(&rest[..start]);
        let tag_rest = &rest[start..];
        let Some(end) = tag_rest.find('>') else {
            out.push_str(tag_rest);
            rest = "";
            break;
        };
        let tag = tag_rest[1..end]
            .trim_start_matches('/')
            .split([' ', '\t', '\n', '/'])
            .next()
            .unwrap_or_default()
            .to_ascii_lowercase();
        if matches!(
            tag.as_str(),
            "p" | "div"
                | "br"
                | "li"
                | "tr"
                | "h1"
                | "h2"
                | "h3"
                | "h4"
                | "h5"
                | "h6"
                | "ul"
                | "ol"
                | "table"
                | "blockquote"
                | "pre"
        ) {
            out.push('\n');
        } else if matches!(tag.as_str(), "td" | "th") {
            out.push(' ');
        }
        rest = &tag_rest[end + 1..];
    }
    out.push_str(rest);
    let decoded = decode_entities(&out);
    decoded
        .lines()
        .map(|line| line.split_whitespace().collect::<Vec<_>>().join(" "))
        .filter(|line| !line.is_empty())
        .collect::<Vec<_>>()
        .join("\n")
}

/// Whether a field value looks like HTML (the SDK sanitises every string
/// field; a plain one passes through unchanged either way).
fn looks_like_html(text: &str) -> bool {
    text.contains('<') && text.contains('>')
}

/// The SDK's `sanitize`: HTML string fields become text, identity objects
/// (`displayName`, `uniqueName`, …) become their display name, other values
/// stay.
#[must_use]
pub fn sanitize_field(value: &Value) -> Value {
    match value {
        Value::String(text) if looks_like_html(text) => Value::String(html_to_text(text)),
        Value::Object(object) => match object.get("displayName").and_then(Value::as_str) {
            Some(name) => Value::String(name.to_owned()),
            None => Value::Object(
                object
                    .iter()
                    .map(|(key, value)| (key.clone(), sanitize_field(value)))
                    .collect(),
            ),
        },
        Value::Array(values) => Value::Array(values.iter().map(sanitize_field).collect()),
        other => other.clone(),
    }
}

/// The test steps of a test case (`Microsoft.VSTS.TCM.Steps`) as a numbered
/// markdown list: each `<step>` holds two `parameterizedString`s, the
/// action and the expected result, each HTML-escaped HTML.
#[must_use]
pub fn steps_to_markdown(xml: &str) -> String {
    let mut out = String::new();
    let mut number = 0_usize;
    let mut rest = xml;
    while let Some(start) = rest.find("<step ") {
        let after = &rest[start..];
        let Some(end) = after.find("</step>") else {
            break;
        };
        let step = &after[..end];
        let mut texts = Vec::new();
        let mut inner = step;
        while let Some(open) = inner.find("<parameterizedString") {
            let body_start = match inner[open..].find('>') {
                Some(offset) => open + offset + 1,
                None => break,
            };
            let Some(close) = inner[body_start..].find("</parameterizedString>") else {
                break;
            };
            let raw = &inner[body_start..body_start + close];
            texts.push(html_to_text(&decode_entities(raw)));
            inner = &inner[body_start + close..];
        }
        if texts.iter().all(|text| text.trim().is_empty()) {
            rest = &after[end..];
            continue;
        }
        number += 1;
        let action = texts.first().map(String::as_str).unwrap_or_default();
        let expected = texts.get(1).map(String::as_str).unwrap_or_default();
        let _ = write!(out, "{number}. {}", action.replace('\n', " "));
        if !expected.trim().is_empty() {
            let _ = write!(out, "\n   Expected: {}", expected.replace('\n', " "));
        }
        out.push('\n');
        rest = &after[end..];
    }
    out
}

/// An optional string argument, trimmed; `None` when absent, null or blank.
///
/// # Errors
///
/// Another type.
pub fn optional_text(arguments: &Map<String, Value>, key: &str) -> Result<Option<String>, String> {
    match arguments.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) => {
            let trimmed = value.trim();
            Ok((!trimmed.is_empty()).then(|| trimmed.to_owned()))
        }
        Some(_) => Err(format!("`{key}` must be a string")),
    }
}

/// An optional boolean argument.
///
/// # Errors
///
/// Another type.
pub fn optional_bool(arguments: &Map<String, Value>, key: &str) -> Result<Option<bool>, String> {
    match arguments.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Bool(flag)) => Ok(Some(*flag)),
        Some(_) => Err(format!("`{key}` must be a boolean")),
    }
}

/// An optional non-negative integer argument (a number, or a numeric string
/// as the index form sometimes sends one).
///
/// # Errors
///
/// Another type or a negative number.
pub fn optional_u64(arguments: &Map<String, Value>, key: &str) -> Result<Option<u64>, String> {
    match arguments.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Number(number)) => number
            .as_u64()
            .map(Some)
            .ok_or_else(|| format!("`{key}` must be a non-negative integer")),
        Some(Value::String(text)) => text
            .trim()
            .parse::<u64>()
            .map(Some)
            .map_err(|_| format!("`{key}` must be a non-negative integer")),
        Some(_) => Err(format!("`{key}` must be a non-negative integer")),
    }
}

/// An optional list of strings (`[]` and null are "none given").
///
/// # Errors
///
/// Not a list of strings.
pub fn optional_text_list(
    arguments: &Map<String, Value>,
    key: &str,
) -> Result<Vec<String>, String> {
    match arguments.get(key) {
        None | Some(Value::Null) => Ok(Vec::new()),
        Some(Value::Array(items)) => items
            .iter()
            .map(|item| match item {
                Value::String(text) => Ok(text.clone()),
                _ => Err(format!("`{key}` must hold strings")),
            })
            .collect(),
        Some(_) => Err(format!("`{key}` must be a list of strings")),
    }
}

/// An optional list of non-negative integers (numbers or numeric strings).
///
/// # Errors
///
/// Not a list of non-negative integers.
pub fn optional_u64_list(arguments: &Map<String, Value>, key: &str) -> Result<Vec<u64>, String> {
    match arguments.get(key) {
        None | Some(Value::Null) => Ok(Vec::new()),
        Some(Value::Array(items)) => items
            .iter()
            .map(|item| match item {
                Value::Number(number) => number.as_u64(),
                Value::String(text) => text.trim().parse::<u64>().ok(),
                _ => None,
            })
            .map(|item| item.ok_or_else(|| format!("`{key}` must hold non-negative integers")))
            .collect(),
        Some(_) => Err(format!("`{key}` must be a list of integers")),
    }
}

/// A file name safe as one key segment: no slash, no control character,
/// not `.`/`..`, at most 255 bytes.
#[must_use]
pub fn safe_name(name: &str) -> Option<String> {
    let trimmed = name.trim();
    if trimmed.is_empty()
        || trimmed.len() > 255
        || matches!(trimmed, "." | "..")
        || trimmed.contains(['/', '\\'])
        || trimmed.chars().any(char::is_control)
    {
        return None;
    }
    Some(trimmed.to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn html_fields_become_text_and_identities_their_display_name() {
        assert_eq!(
            html_to_text(
                "<div><p>Hello&nbsp;<b>world</b></p><ul><li>one</li><li>two &amp; three</li></ul></div>"
            ),
            "Hello world\none\ntwo & three"
        );
        assert_eq!(decode_entities("&#65;&#x42;&unknown;&"), "AB&unknown;&");
        let sanitized = sanitize_field(&json!({
            "System.Description": "<p>Fix <i>it</i></p>",
            "System.AssignedTo": {"displayName": "Ada", "uniqueName": "ada@example.com"},
            "Count": 3,
            "Plain": "text"
        }));
        assert_eq!(sanitized["System.Description"], "Fix it");
        assert_eq!(sanitized["System.AssignedTo"], "Ada");
        assert_eq!(sanitized["Count"], 3);
        assert_eq!(sanitized["Plain"], "text");
    }

    #[test]
    fn steps_render_as_a_numbered_list() {
        let xml = r#"<?xml version="1.0"?><steps id="0" last="2"><step id="2" type="ValidateStep"><parameterizedString isformatted="true">&lt;DIV&gt;&lt;P&gt;Open the app&lt;/P&gt;&lt;/DIV&gt;</parameterizedString><parameterizedString isformatted="true">&lt;P&gt;It opens&lt;/P&gt;</parameterizedString><description/></step><step id="3" type="ActionStep"><parameterizedString isformatted="true">Log in</parameterizedString><parameterizedString isformatted="true"></parameterizedString><description/></step></steps>"#;
        assert_eq!(
            steps_to_markdown(xml),
            "1. Open the app\n   Expected: It opens\n2. Log in\n"
        );
        assert_eq!(steps_to_markdown(""), "");
    }

    #[test]
    fn argument_readers_are_strict() {
        let arguments =
            json!({"a": " x ", "b": true, "c": "12", "d": [1, "2"], "e": ["p"], "f": 3});
        let arguments = arguments.as_object().expect("object");
        assert_eq!(optional_text(arguments, "a").unwrap().as_deref(), Some("x"));
        assert_eq!(optional_bool(arguments, "b").unwrap(), Some(true));
        assert_eq!(optional_u64(arguments, "c").unwrap(), Some(12));
        assert_eq!(optional_u64_list(arguments, "d").unwrap(), vec![1, 2]);
        assert_eq!(optional_text_list(arguments, "e").unwrap(), vec!["p"]);
        assert!(optional_text(arguments, "f").is_err());
        assert!(optional_bool(arguments, "a").is_err());
        assert!(optional_u64(arguments, "b").is_err());
        assert_eq!(optional_text(arguments, "missing").unwrap(), None);
        assert_eq!(safe_name(" spec.pdf "), Some("spec.pdf".to_owned()));
        assert_eq!(safe_name("a/b"), None);
        assert_eq!(safe_name(".."), None);
    }
}
