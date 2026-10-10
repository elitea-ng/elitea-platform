//! The SDK's `FigmaApiWrapper.process_output`: the `extra_params` output
//! controls every Figma REST tool applies to its result.
//!
//! The result is serialized as Python's `json.dumps` writes it (`", "` and
//! `": "` separators, `ensure_ascii`), because `limit` counts characters of
//! that text and `regexp` is matched against it. Over the limit, the RAW
//! result is reduced by `fields_retain`/`fields_remove` between
//! `depth_start` and `depth_end`, a note says what was dropped, and the
//! whole answer is cut at `limit` characters.

use std::collections::BTreeSet;
use std::fmt::Write as _;
use std::sync::OnceLock;

use regex::Regex;
use serde_json::{Map, Value};

use super::config::FigmaToolkitConfig;

/// The SDK's `GLOBAL_RETAIN`.
const GLOBAL_RETAIN: [&str; 5] = ["id", "name", "type", "document", "children"];
const GLOBAL_DEPTH_START: i64 = 1;
const GLOBAL_DEPTH_END: i64 = 6;
const MAX_REGEXP_BYTES: usize = 4 * 1_024;
const MAX_FIELDS: usize = 256;
/// fancy-regex's own default, stated so the bound is visible here.
const REGEXP_BACKTRACK_LIMIT: usize = 1_000_000;

/// The resolved controls for one call.
pub(super) struct OutputControls {
    limit: usize,
    regexp: Option<fancy_regex::Regex>,
    fields_retain: BTreeSet<String>,
    fields_remove: BTreeSet<String>,
    depth_start: i64,
    depth_end: Option<i64>,
}

/// Resolve `extra_params` key by key over the toolkit defaults, as
/// `extra_params.get(key, self.global_*)` does. The error is the text the
/// SDK's wrapper would have answered, without the tool name.
pub(super) fn controls(
    extra: Option<&Map<String, Value>>,
    config: &FigmaToolkitConfig,
) -> Result<OutputControls, String> {
    let empty = Map::new();
    let extra = extra.unwrap_or(&empty);
    let limit = match extra.get("limit") {
        None => config.global_limit(),
        Some(Value::Number(number)) => number
            .as_u64()
            .ok_or_else(|| format!("invalid literal for int() with base 10: '{number}'"))?,
        Some(Value::String(text)) => text
            .trim()
            .parse::<u64>()
            .map_err(|_| format!("invalid literal for int() with base 10: '{text}'"))?,
        Some(_) => {
            return Err(
                "int() argument must be a string, a bytes-like object or a real number".to_owned(),
            );
        }
    };
    let limit = usize::try_from(limit).unwrap_or(usize::MAX);
    let pattern = match extra.get("regexp") {
        None => config.global_regexp().map(ToOwned::to_owned),
        Some(Value::Null) => None,
        Some(Value::String(pattern)) => Some(pattern.clone()),
        Some(_) => return Err("first argument must be string or compiled pattern".to_owned()),
    };
    let regexp = match pattern.filter(|pattern| !pattern.is_empty()) {
        Some(pattern) if pattern.len() > MAX_REGEXP_BYTES => {
            return Err("the regular expression exceeds the approved length".to_owned());
        }
        Some(pattern) => Some(
            fancy_regex::RegexBuilder::new(&pattern)
                .backtrack_limit(REGEXP_BACKTRACK_LIMIT)
                .build()
                .map_err(|error| error.to_string())?,
        ),
        None => None,
    };
    let fields = |key: &str, default: &[&str]| -> Result<BTreeSet<String>, String> {
        match extra.get(key) {
            None => Ok(default.iter().map(|field| (*field).to_owned()).collect()),
            Some(Value::Null) => Ok(BTreeSet::new()),
            Some(Value::Array(values)) if values.len() <= MAX_FIELDS => Ok(values
                .iter()
                .map(|value| match value {
                    Value::String(text) => text.clone(),
                    other => other.to_string(),
                })
                .collect()),
            Some(_) => Err(format!("'{key}' must be a list of field names")),
        }
    };
    let depth = |key: &str, default: i64| -> Result<Option<i64>, String> {
        match extra.get(key) {
            None => Ok(Some(default)),
            Some(Value::Null) => Ok(None),
            Some(Value::Number(number)) => number
                .as_i64()
                .map(Some)
                .ok_or_else(|| format!("'{key}' must be an integer")),
            Some(Value::String(text)) => text
                .trim()
                .parse::<i64>()
                .map(Some)
                .map_err(|_| format!("'{key}' must be an integer")),
            Some(_) => Err(format!("'{key}' must be an integer")),
        }
    };
    let fields_retain = fields("fields_retain", &GLOBAL_RETAIN)?;
    // `fields_remove` has lower priority than `fields_retain`.
    let fields_remove = fields("fields_remove", &[])?
        .difference(&fields_retain)
        .cloned()
        .collect();
    Ok(OutputControls {
        limit,
        regexp,
        fields_retain,
        fields_remove,
        depth_start: depth("depth_start", GLOBAL_DEPTH_START)?.unwrap_or(GLOBAL_DEPTH_START),
        depth_end: depth("depth_end", GLOBAL_DEPTH_END)?,
    })
}

/// The SDK wrapper's answer for one result. `Err` carries the wrapper's
/// failure text (a regular expression that exhausts its backtracking budget).
pub(super) fn render(result: &Value, controls: &OutputControls) -> Result<String, String> {
    if !truthy(result) {
        return Ok(
            "Response result is empty. Check your input parameters or credentials".to_owned(),
        );
    }
    let serialized = py_dumps(result);
    if !matches!(result, Value::Object(_) | Value::Array(_)) {
        let text = match &controls.regexp {
            Some(regexp) => fix_trailing_commas(&strip(regexp, &serialized)?),
            None => serialized,
        };
        return Ok(truncate_chars(&text, controls.limit));
    }
    let text = match &controls.regexp {
        Some(regexp) => fix_trailing_commas(&strip(regexp, &serialized)?),
        None => serialized,
    };
    if text.chars().count() <= controls.limit {
        return Ok(text);
    }
    let mut retained = BTreeSet::new();
    let mut removed = BTreeSet::new();
    let reduced = reduce(result, controls, 1, &mut retained, &mut removed);
    let depth_end = controls
        .depth_end
        .map_or_else(|| "None".to_owned(), |depth| depth.to_string());
    let note = format!(
        "Size of the output exceeds limit {}. Data reducing has been applied. Starting from the depth_start = {} the following object fields were removed: {}. The following fields were retained: {}. Starting from depth_end = {depth_end} all fields were ignored. You can adjust fields_retain, fields_remove, depth_start, depth_end, limit and regexp parameters to get more precise output",
        controls.limit,
        controls.depth_start,
        py_list(&removed),
        py_list(&retained),
    );
    Ok(truncate_chars(
        &format!("## NOTE:\n{note}.\n## Result: {}", py_dumps(&reduced)),
        controls.limit,
    ))
}

/// The SDK's `process_fields._process`.
fn reduce(
    value: &Value,
    controls: &OutputControls,
    depth: i64,
    retained: &mut BTreeSet<String>,
    removed: &mut BTreeSet<String>,
) -> Value {
    if controls.depth_end.is_some_and(|end| depth >= end) {
        return Value::Null;
    }
    match value {
        Value::Object(object) => {
            let mut result = Map::new();
            for (key, child) in object {
                if controls.fields_remove.contains(key) {
                    removed.insert(key.clone());
                    continue;
                }
                if depth >= controls.depth_start {
                    if controls.fields_retain.contains(key) {
                        retained.insert(key.clone());
                        result.insert(
                            key.clone(),
                            reduce(child, controls, depth + 1, retained, removed),
                        );
                    } else {
                        removed.insert(key.clone());
                    }
                } else {
                    result.insert(
                        key.clone(),
                        reduce(child, controls, depth + 1, retained, removed),
                    );
                }
            }
            Value::Object(result)
        }
        Value::Array(values) => Value::Array(
            values
                .iter()
                .map(|child| reduce(child, controls, depth + 1, retained, removed))
                .collect(),
        ),
        other => other.clone(),
    }
}

fn strip(regexp: &fancy_regex::Regex, text: &str) -> Result<String, String> {
    regexp
        .try_replacen(text, 0, fancy_regex::NoExpand(""))
        .map(std::borrow::Cow::into_owned)
        .map_err(|error| error.to_string())
}

/// The SDK's `fix_trailing_commas` after a regexp removed list items.
fn fix_trailing_commas(text: &str) -> String {
    static PATTERNS: OnceLock<Option<[Regex; 3]>> = OnceLock::new();
    let Some([doubled, before_close, after_open]) = PATTERNS.get_or_init(|| {
        Some([
            Regex::new(r",\s*,+").ok()?,
            Regex::new(r",\s*([\]}])").ok()?,
            Regex::new(r"([\[{])\s*,").ok()?,
        ])
    }) else {
        return text.to_owned();
    };
    let text = doubled.replace_all(text, ",");
    let text = before_close.replace_all(&text, "$1");
    after_open.replace_all(&text, "$1").into_owned()
}

fn truncate_chars(text: &str, limit: usize) -> String {
    match text.char_indices().nth(limit) {
        Some((index, _)) => text[..index].to_owned(),
        None => text.to_owned(),
    }
}

fn truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(value) => *value,
        Value::Number(number) => number.as_f64().is_some_and(|value| value != 0.0),
        Value::String(text) => !text.is_empty(),
        Value::Array(values) => !values.is_empty(),
        Value::Object(values) => !values.is_empty(),
    }
}

fn py_list(values: &BTreeSet<String>) -> String {
    let quoted = values
        .iter()
        .map(|value| format!("'{value}'"))
        .collect::<Vec<_>>();
    format!("[{}]", quoted.join(", "))
}

/// Python's `json.dumps(value)` with its default separators and
/// `ensure_ascii=True`.
pub(super) fn py_dumps(value: &Value) -> String {
    let mut out = String::new();
    write_py_json(&mut out, value);
    out
}

fn write_py_json(out: &mut String, value: &Value) {
    match value {
        Value::Null => out.push_str("null"),
        Value::Bool(true) => out.push_str("true"),
        Value::Bool(false) => out.push_str("false"),
        Value::Number(number) => out.push_str(&number.to_string()),
        Value::String(text) => write_py_string(out, text),
        Value::Array(values) => {
            out.push('[');
            for (index, child) in values.iter().enumerate() {
                if index > 0 {
                    out.push_str(", ");
                }
                write_py_json(out, child);
            }
            out.push(']');
        }
        Value::Object(object) => {
            out.push('{');
            for (index, (key, child)) in object.iter().enumerate() {
                if index > 0 {
                    out.push_str(", ");
                }
                write_py_string(out, key);
                out.push_str(": ");
                write_py_json(out, child);
            }
            out.push('}');
        }
    }
}

fn write_py_string(out: &mut String, text: &str) {
    out.push('"');
    for character in text.chars() {
        match character {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{08}' => out.push_str("\\b"),
            '\u{0c}' => out.push_str("\\f"),
            character if character < ' ' || !character.is_ascii() => {
                let mut units = [0_u16; 2];
                for unit in character.encode_utf16(&mut units) {
                    let _ = write!(out, "\\u{unit:04x}");
                }
            }
            character => out.push(character),
        }
    }
    out.push('"');
}
