//! The two JSON extractors the planners apply to a model answer.
//!
//! `json.loads` is `serde_json` here. They differ on one input a model
//! could write: Python takes `NaN` and `Infinity` as numbers, `serde_json`
//! refuses them (the answer then goes down the parse-failure path).

use crate::graph::pyre;
use crate::graph::pystr;
use regex::Regex;
use serde_json::{Map, Value};
use std::sync::LazyLock;

fn compile(pattern: &str) -> Option<Regex> {
    Regex::new(&pyre::translate(pattern)).ok()
}

/// `r"```(?:json)?\s*([\s\S]*?)```"`.
static ANY_FENCE: LazyLock<Option<Regex>> =
    LazyLock::new(|| compile(r"```(?:json)?\s*([\s\S]*?)```"));
/// `r",\s*([}\]])"`.
static TRAILING_COMMA: LazyLock<Option<Regex>> = LazyLock::new(|| compile(r",\s*([}\]])"));
/// `r"```json\s*(.*?)\s*```"` with `DOTALL | IGNORECASE`.
static JSON_FENCE: LazyLock<Option<Regex>> =
    LazyLock::new(|| compile(r"(?si)```json\s*(.*?)\s*```"));
/// `r"```\s*(\{.*?\})\s*```"` with `DOTALL`.
static BARE_FENCE: LazyLock<Option<Regex>> =
    LazyLock::new(|| compile(r"(?s)```\s*(\{.*?\})\s*```"));
/// `r"\{.*\}"` with `DOTALL`.
static OBJECT: LazyLock<Option<Regex>> = LazyLock::new(|| compile(r"(?s)\{.*\}"));

fn loads(text: &str) -> Option<Value> {
    serde_json::from_str(text).ok()
}

/// `cluster_planner._parse_json_response`: best effort, `{}` when nothing
/// parses. The value may be any JSON type (a list answer stays a list —
/// the caller's `.get` then fails, which Python let propagate).
#[must_use]
pub fn naming_json(raw: &str) -> Value {
    let mut text = pystr::strip(raw).to_owned();
    if let Some(captures) = ANY_FENCE.as_ref().and_then(|re| re.captures(&text))
        && let Some(inner) = captures.get(1)
    {
        text = pystr::strip(inner.as_str()).to_owned();
    }
    if let Some(value) = loads(&text) {
        return value;
    }
    if let (Some(first), Some(last)) = (text.find('{'), text.rfind('}'))
        && last > first
    {
        let candidate = &text[first..=last];
        if let Some(value) = loads(candidate) {
            return value;
        }
        if let Some(re) = TRAILING_COMMA.as_ref() {
            let cleaned = re.replace_all(candidate, "$1");
            if let Some(value) = loads(&cleaned) {
                return value;
            }
        }
    }
    tracing::warn!(
        "Failed to parse LLM JSON output (length {})",
        raw.chars().count()
    );
    Value::Object(Map::new())
}

/// A structure answer that looked like JSON and was not (`JSONDecodeError`
/// out of `_parse_llm_json_response`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StructureParseError(pub String);

/// `_parse_llm_json_response`: the answer as JSON, else the JSON of the
/// first fenced block (```` ```json ```` first, then a bare fence around an
/// object), else the widest `{…}`; a candidate that is found but does not
/// parse is an error (Python let the `JSONDecodeError` out, and the
/// structure node failed). With no candidate at all, the fallback
/// structure.
///
/// # Errors
///
/// A fenced block or brace span that is not JSON.
pub fn structure_json(raw: &str) -> Result<Value, StructureParseError> {
    let text = pystr::strip(raw);
    if let Some(value) = loads(text) {
        return Ok(value);
    }
    let candidate = [&JSON_FENCE, &BARE_FENCE]
        .iter()
        .find_map(|re| {
            re.as_ref()
                .and_then(|re| re.captures(text))
                .and_then(|c| c.get(1))
                .map(|m| pystr::strip(m.as_str()).to_owned())
        })
        .or_else(|| {
            OBJECT
                .as_ref()
                .and_then(|re| re.find(text))
                .map(|m| pystr::strip(m.as_str()).to_owned())
        });
    let Some(candidate) = candidate else {
        tracing::warn!("Could not parse LLM JSON response, using fallback structure");
        return Ok(fallback_structure());
    };
    loads(&candidate).ok_or_else(|| {
        StructureParseError("the structure answer holds JSON that does not parse".to_owned())
    })
}

/// The structure `_parse_llm_json_response` returns when the answer holds
/// no JSON at all.
#[must_use]
pub fn fallback_structure() -> Value {
    serde_json::json!({
        "wiki_title": "Generated Wiki",
        "overview": "Documentation for the repository",
        "sections": [{
            "section_name": "Documentation",
            "section_order": 1,
            "description": "Main documentation",
            "rationale": "Fallback structure due to parsing error",
            "pages": [{
                "page_name": "Overview",
                "page_order": 1,
                "description": "Repository overview",
                "content_focus": "General information",
                "rationale": "Basic overview page for fallback structure",
                "target_folders": [],
                "key_files": [],
                "retrieval_query": "repository overview introduction getting started documentation README architecture"
            }]
        }],
        "total_pages": 1
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn naming_json_takes_fences_braces_and_trailing_commas() {
        assert_eq!(naming_json(" {\"a\": 1} "), json!({"a": 1}));
        assert_eq!(naming_json("x ```json\n{\"a\": 2}\n``` y"), json!({"a": 2}));
        assert_eq!(naming_json("Sure: {\"a\": 3} done"), json!({"a": 3}));
        assert_eq!(naming_json("{\"a\": [1, 2,], }"), json!({"a": [1, 2]}));
        assert_eq!(naming_json("[1, 2]"), json!([1, 2]));
        assert_eq!(naming_json("nothing"), json!({}));
    }

    #[test]
    fn structure_json_fails_on_a_broken_candidate() {
        assert_eq!(
            structure_json("```JSON\n{\"a\": 1}\n```"),
            Ok(json!({"a": 1}))
        );
        assert_eq!(
            structure_json("text ```\n{\"b\": 1}\n```"),
            Ok(json!({"b": 1}))
        );
        assert_eq!(
            structure_json("pre {\"c\": {\"d\": 1}} post"),
            Ok(json!({"c": {"d": 1}}))
        );
        assert!(structure_json("pre {not json} post").is_err());
        assert_eq!(structure_json("no json here"), Ok(fallback_structure()));
        assert_eq!(structure_json(""), Ok(fallback_structure()));
    }
}
