//! The wiki structure: `state/wiki_state.py`'s `PageSpec`, `SectionSpec`
//! and `WikiStructureSpec`, with the pydantic validation the classic
//! planner relies on.
//!
//! Field order is the pydantic field order, so `serde_json::to_value`
//! followed by `pyjson::dumps_indent2` is `json.dumps(spec.model_dump(),
//! indent=2)` byte for byte. That JSON is the `wiki_structure` the engine
//! returns and the input of page generation.

use crate::pyjson;
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};
use std::fmt;

/// One page to generate.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct PageSpec {
    pub page_name: String,
    pub page_order: i64,
    pub description: String,
    pub content_focus: String,
    pub rationale: String,
    /// Exact symbol names for graph retrieval.
    #[serde(default)]
    pub target_symbols: Vec<String>,
    /// Documentation files for the page's context.
    #[serde(default)]
    pub target_docs: Vec<String>,
    #[serde(default)]
    pub target_folders: Vec<String>,
    #[serde(default)]
    pub key_files: Vec<String>,
    /// The fallback query for retrieval.
    #[serde(default)]
    pub retrieval_query: String,
    /// The cluster planner's `planner_mode`, `section_id`, `page_id` and
    /// `cluster_node_ids`; empty for the classic planner.
    #[serde(default)]
    pub metadata: Map<String, Value>,
}

/// One section: an ordered list of pages.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct SectionSpec {
    pub section_name: String,
    pub section_order: i64,
    pub description: String,
    pub rationale: String,
    pub pages: Vec<PageSpec>,
}

/// The whole structure.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct WikiStructureSpec {
    pub wiki_title: String,
    pub overview: String,
    pub sections: Vec<SectionSpec>,
    pub total_pages: i64,
}

impl WikiStructureSpec {
    /// `model_dump()` as JSON.
    #[must_use]
    pub fn to_value(&self) -> Value {
        // Every field is a string, an integer, a list or a JSON map, which
        // `serde_json` always serialises.
        serde_json::to_value(self).unwrap_or(Value::Null)
    }

    /// `json.dumps(spec.model_dump(), indent=2)`.
    #[must_use]
    pub fn to_python_json(&self) -> String {
        pyjson::dumps_indent2(&self.to_value())
    }

    /// The number of pages over every section.
    #[must_use]
    pub fn page_count(&self) -> usize {
        self.sections.iter().map(|s| s.pages.len()).sum()
    }
}

/// `WikiStructureSpec.model_validate(data)` failed. The message names the
/// first field in error (pydantic lists them all; nothing reads the list).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ValidationError(pub String);

impl fmt::Display for ValidationError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "validation error: {}", self.0)
    }
}

impl std::error::Error for ValidationError {}

fn error(path: &str, what: &str) -> ValidationError {
    ValidationError(format!("{path}: {what}"))
}

/// A pydantic `str` field in lax mode: a string, nothing else (no number
/// is coerced).
fn lax_str(map: &Map<String, Value>, key: &str, path: &str) -> Result<String, ValidationError> {
    match map.get(key) {
        Some(Value::String(text)) => Ok(text.clone()),
        Some(_) => Err(error(
            &format!("{path}.{key}"),
            "input should be a valid string",
        )),
        None => Err(error(&format!("{path}.{key}"), "field required")),
    }
}

fn lax_str_default(
    map: &Map<String, Value>,
    key: &str,
    path: &str,
) -> Result<String, ValidationError> {
    if map.contains_key(key) {
        lax_str(map, key, path)
    } else {
        Ok(String::new())
    }
}

/// A pydantic `int` from a string: surrounding whitespace, a sign, digits
/// with single `_` between them, then optionally `.` and zeros only
/// (`"1.0"` is 1, `"1.5"` and `"1e2"` are refused).
fn int_from_str(text: &str) -> Option<i64> {
    let text = text.trim();
    let (negative, digits) = match text.as_bytes().first() {
        Some(b'-') => (true, &text[1..]),
        Some(b'+') => (false, &text[1..]),
        _ => (false, text),
    };
    let (whole, fraction) = match digits.split_once('.') {
        Some((whole, fraction)) => (whole, Some(fraction)),
        None => (digits, None),
    };
    if let Some(fraction) = fraction
        && (fraction.is_empty() || !fraction.bytes().all(|b| b == b'0'))
    {
        return None;
    }
    if whole.is_empty()
        || whole.starts_with('_')
        || whole.ends_with('_')
        || whole.contains("__")
        || !whole.bytes().all(|b| b.is_ascii_digit() || b == b'_')
    {
        return None;
    }
    let cleaned: String = whole.chars().filter(|c| *c != '_').collect();
    let value: i64 = cleaned.parse().ok()?;
    Some(if negative { -value } else { value })
}

/// A pydantic `int` field in lax mode: an integer, a float without a
/// fractional part, a numeric string, or a bool. Values outside `i64` are
/// refused (pydantic takes arbitrary integers; no structure has one).
fn lax_int(map: &Map<String, Value>, key: &str, path: &str) -> Result<i64, ValidationError> {
    let field = format!("{path}.{key}");
    match map.get(key) {
        Some(Value::Number(number)) => {
            if let Some(value) = number.as_i64() {
                return Ok(value);
            }
            match number.as_f64() {
                #[allow(clippy::cast_possible_truncation)]
                Some(value)
                    if value.is_finite()
                        && value.fract() == 0.0
                        && (-9.223_372_036_854_776e18..9.223_372_036_854_776e18)
                            .contains(&value) =>
                {
                    Ok(value as i64)
                }
                _ => Err(error(&field, "input should be a valid integer")),
            }
        }
        Some(Value::Bool(flag)) => Ok(i64::from(*flag)),
        Some(Value::String(text)) => {
            int_from_str(text).ok_or_else(|| error(&field, "unable to parse string as an integer"))
        }
        Some(_) => Err(error(&field, "input should be a valid integer")),
        None => Err(error(&field, "field required")),
    }
}

/// A `List[str]` field with a default: a list of strings.
fn lax_str_list(
    map: &Map<String, Value>,
    key: &str,
    path: &str,
) -> Result<Vec<String>, ValidationError> {
    let field = format!("{path}.{key}");
    match map.get(key) {
        None => Ok(Vec::new()),
        Some(Value::Array(items)) => items
            .iter()
            .enumerate()
            .map(|(index, item)| match item {
                Value::String(text) => Ok(text.clone()),
                _ => Err(error(
                    &format!("{field}.{index}"),
                    "input should be a valid string",
                )),
            })
            .collect(),
        Some(_) => Err(error(&field, "input should be a valid list")),
    }
}

fn object<'v>(value: &'v Value, path: &str) -> Result<&'v Map<String, Value>, ValidationError> {
    value
        .as_object()
        .ok_or_else(|| error(path, "input should be a valid dictionary or instance"))
}

fn list<'v>(
    map: &'v Map<String, Value>,
    key: &str,
    path: &str,
) -> Result<&'v [Value], ValidationError> {
    match map.get(key) {
        Some(Value::Array(items)) => Ok(items),
        Some(_) => Err(error(
            &format!("{path}.{key}"),
            "input should be a valid list",
        )),
        None => Err(error(&format!("{path}.{key}"), "field required")),
    }
}

impl PageSpec {
    /// `PageSpec.model_validate` (extra keys are ignored).
    ///
    /// # Errors
    ///
    /// The first field pydantic would refuse.
    pub fn validate(value: &Value, path: &str) -> Result<Self, ValidationError> {
        let map = object(value, path)?;
        let metadata = match map.get("metadata") {
            None => Map::new(),
            Some(Value::Object(meta)) => meta.clone(),
            Some(_) => {
                return Err(error(
                    &format!("{path}.metadata"),
                    "input should be a valid dictionary",
                ));
            }
        };
        Ok(Self {
            page_name: lax_str(map, "page_name", path)?,
            page_order: lax_int(map, "page_order", path)?,
            description: lax_str(map, "description", path)?,
            content_focus: lax_str(map, "content_focus", path)?,
            rationale: lax_str(map, "rationale", path)?,
            target_symbols: lax_str_list(map, "target_symbols", path)?,
            target_docs: lax_str_list(map, "target_docs", path)?,
            target_folders: lax_str_list(map, "target_folders", path)?,
            key_files: lax_str_list(map, "key_files", path)?,
            retrieval_query: lax_str_default(map, "retrieval_query", path)?,
            metadata,
        })
    }
}

impl SectionSpec {
    /// `SectionSpec.model_validate`.
    ///
    /// # Errors
    ///
    /// The first field pydantic would refuse.
    pub fn validate(value: &Value, path: &str) -> Result<Self, ValidationError> {
        let map = object(value, path)?;
        let section_name = lax_str(map, "section_name", path)?;
        let section_order = lax_int(map, "section_order", path)?;
        let description = lax_str(map, "description", path)?;
        let rationale = lax_str(map, "rationale", path)?;
        let pages = list(map, "pages", path)?
            .iter()
            .enumerate()
            .map(|(index, page)| PageSpec::validate(page, &format!("{path}.pages.{index}")))
            .collect::<Result<_, _>>()?;
        Ok(Self {
            section_name,
            section_order,
            description,
            rationale,
            pages,
        })
    }
}

impl WikiStructureSpec {
    /// `WikiStructureSpec.model_validate(data)` in pydantic's lax mode, the
    /// way the classic planner validates the model's JSON.
    ///
    /// # Errors
    ///
    /// The first field pydantic would refuse.
    pub fn validate(value: &Value) -> Result<Self, ValidationError> {
        let map = object(value, "WikiStructureSpec")?;
        let path = "WikiStructureSpec";
        let wiki_title = lax_str(map, "wiki_title", path)?;
        let overview = lax_str(map, "overview", path)?;
        let sections = list(map, "sections", path)?
            .iter()
            .enumerate()
            .map(|(index, section)| SectionSpec::validate(section, &format!("sections.{index}")))
            .collect::<Result<_, _>>()?;
        let total_pages = lax_int(map, "total_pages", path)?;
        Ok(Self {
            wiki_title,
            overview,
            sections,
            total_pages,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn page(order: &Value) -> Value {
        json!({"page_name": "a", "page_order": order, "description": "d", "content_focus": "c", "rationale": "r"})
    }

    #[test]
    fn integers_follow_pydantic_lax_mode() {
        // Probed against pydantic 2.13.5 (`PageSpec(page_order=…)`).
        let accepted = [
            (json!(1), 1),
            (json!(1.0), 1),
            (json!(-2.0), -2),
            (json!("1"), 1),
            (json!(" 2 "), 2),
            (json!("+3"), 3),
            (json!("3_0"), 30),
            (json!("1.0"), 1),
            (json!("1.00"), 1),
            (json!("-0"), 0),
            (json!("\t7\n"), 7),
            (json!("007"), 7),
            (json!("1_000.0"), 1000),
            (json!(true), 1),
        ];
        for (input, want) in accepted {
            let spec = PageSpec::validate(&page(&input), "p");
            assert_eq!(spec.map(|p| p.page_order), Ok(want), "{input}");
        }
        for input in [
            json!(1.5),
            json!("1e2"),
            json!(null),
            json!("0x1"),
            json!("1."),
            json!("1._0"),
            json!("_1"),
            json!("1__0"),
            json!("1.5"),
            json!(""),
            json!(" "),
            json!("+-1"),
            json!("1 .0"),
            json!("1.0_0"),
            json!(1e300),
            json!([1]),
        ] {
            assert!(PageSpec::validate(&page(&input), "p").is_err(), "{input}");
        }
    }

    #[test]
    fn strings_and_lists_are_strict() {
        let mut value = page(&json!(1));
        value["page_name"] = json!(1);
        assert!(PageSpec::validate(&value, "p").is_err());
        let mut value = page(&json!(1));
        value["target_symbols"] = json!("abc");
        assert!(PageSpec::validate(&value, "p").is_err());
        value["target_symbols"] = json!([1]);
        assert!(PageSpec::validate(&value, "p").is_err());
        value["target_symbols"] = json!(["a"]);
        value["metadata"] = json!([]);
        assert!(PageSpec::validate(&value, "p").is_err());
        value["metadata"] = json!({"a": 1});
        value["extra"] = json!(5);
        let spec = PageSpec::validate(&value, "p").expect("valid");
        assert_eq!(spec.target_symbols, vec!["a".to_owned()]);
        assert_eq!(spec.retrieval_query, "");
    }

    #[test]
    fn model_dump_keeps_the_pydantic_field_order() {
        let spec = WikiStructureSpec {
            wiki_title: "t \u{2014} x".to_owned(),
            overview: String::new(),
            sections: vec![SectionSpec {
                section_name: "s".to_owned(),
                section_order: 1,
                description: String::new(),
                rationale: String::new(),
                pages: vec![PageSpec::validate(&page(&json!(1)), "p").expect("valid")],
            }],
            total_pages: 1,
        };
        let text = spec.to_python_json();
        assert!(text.starts_with("{\n  \"wiki_title\": \"t \\u2014 x\",\n  \"overview\""));
        let keys: Vec<&str> = [
            "page_name",
            "page_order",
            "description",
            "content_focus",
            "rationale",
            "target_symbols",
            "target_docs",
            "target_folders",
            "key_files",
            "retrieval_query",
            "metadata",
        ]
        .to_vec();
        let page = &text[text.find("\"pages\"").expect("pages")..];
        let positions: Vec<usize> = keys
            .iter()
            .map(|k| page.find(&format!("\"{k}\"")).expect("present"))
            .collect();
        assert!(positions.windows(2).all(|w| w[0] < w[1]));
    }
}
