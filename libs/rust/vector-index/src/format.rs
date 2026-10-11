//! The shapes the SDK's index tools return.

use serde_json::{Map, Value, json};

use crate::hit::{Hit, score_text, score_value};
use crate::pyfmt;

/// The `doctype` of a toolkit: whether its search results are code.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Doctype {
    /// `doctype == "code"`: results are formatted as `file -> method`
    /// plus a fenced block.
    Code,
    /// Everything else (`document`, `doc`): results are plain dicts.
    #[default]
    Document,
}

impl Doctype {
    /// Reads a toolkit's `doctype` string.
    #[must_use]
    pub fn parse(doctype: &str) -> Self {
        if doctype == "code" {
            Self::Code
        } else {
            Self::Document
        }
    }
}

/// `os.path.splitext(name)[1]`: the extension with its dot; none for a
/// name with no dot, a dot only at the start of the name, or a trailing dot
/// run.
fn extension(name: &str) -> &str {
    let base_start = name.rfind('/').map_or(0, |index| index + 1);
    let base = &name[base_start..];
    let Some(dot) = base.rfind('.') else {
        return "";
    };
    // A leading run of dots is part of the name (".bashrc", "..x").
    if base[..dot].bytes().all(|byte| byte == b'.') {
        return "";
    }
    &base[dot..]
}

/// The SDK's `get_programming_language(...).value` for a file extension.
#[must_use]
pub fn language_for(extension: &str) -> &'static str {
    match extension {
        ".py" => "python",
        ".js" | ".jsx" | ".mjs" | ".cjs" => "javascript",
        ".ts" | ".tsx" => "typescript",
        ".java" => "java",
        ".kt" => "kotlin",
        ".rs" => "rust",
        ".go" => "go",
        ".cpp" => "cpp",
        ".c" => "c",
        ".cs" => "c_sharp",
        ".hs" => "haskell",
        ".rb" => "ruby",
        _ => "unknown",
    }
}

/// `codesearcher.search_format`: the page content becomes
/// `<filename> -> <method_name> (score: <score>)`, a blank line, a fenced
/// block in the file's language holding the code, and a blank line.
#[must_use]
pub fn code_page_content(hit: &Hit) -> String {
    let filename = hit
        .metadata
        .get("filename")
        .map_or_else(|| "unknown".to_owned(), pyfmt::display);
    let method = hit
        .metadata
        .get("method_name")
        .map_or_else(|| "text".to_owned(), pyfmt::display);
    let language = language_for(extension(&filename));
    format!(
        "{filename} -> {method} (score: {})\n\n```{language}\n{}\n```\n\n",
        score_text(hit.score),
        hit.page_content
    )
}

/// The SDK's result list: `{page_content, metadata, score}` dicts, with
/// code formatting for code toolkits.
#[must_use]
pub fn format_hits(hits: &[Hit], doctype: Doctype) -> Vec<Value> {
    hits.iter()
        .map(|hit| match doctype {
            Doctype::Code => {
                let mut map = Map::new();
                map.insert(
                    "page_content".to_owned(),
                    Value::String(code_page_content(hit)),
                );
                map.insert("metadata".to_owned(), Value::Object(hit.metadata.clone()));
                map.insert("score".to_owned(), score_value(hit.score));
                Value::Object(map)
            }
            Doctype::Document => hit.to_value(),
        })
        .collect()
}

/// `_filter_result_fields`: keeps only the requested fields.
///
/// * `page_content`, `score` and `metadata` (all of it) are top-level;
/// * `metadata.<name>` keeps one metadata key, the name being everything
///   after the first dot, looked up as one literal key;
/// * with no valid field at all, or when none of the requested fields exist
///   in any result, the results come back unfiltered;
/// * a result with none of the requested fields becomes `{}`.
#[must_use]
pub fn project_fields(docs: Vec<Value>, include: &[String]) -> Vec<Value> {
    if include.is_empty() || docs.is_empty() {
        return docs;
    }
    let mut want_content = false;
    let mut want_score = false;
    let mut want_metadata = false;
    let mut metadata_fields: Vec<&str> = Vec::new();
    for field in include {
        match field.as_str() {
            "metadata" => want_metadata = true,
            "page_content" => want_content = true,
            "score" => want_score = true,
            other => {
                if let Some(name) = other
                    .strip_prefix("metadata.")
                    .filter(|name| !name.is_empty())
                    && !metadata_fields.contains(&name)
                {
                    metadata_fields.push(name);
                }
            }
        }
    }
    if !want_content && !want_score && !want_metadata && metadata_fields.is_empty() {
        return docs;
    }
    let mut any_found = false;
    let projected: Vec<Value> = docs
        .iter()
        .map(|doc| {
            let mut out = Map::new();
            if want_content && let Some(content) = doc.get("page_content") {
                out.insert("page_content".to_owned(), content.clone());
                any_found = true;
            }
            if want_score && let Some(score) = doc.get("score") {
                out.insert("score".to_owned(), score.clone());
                any_found = true;
            }
            if want_metadata {
                if let Some(metadata) = doc.get("metadata") {
                    out.insert("metadata".to_owned(), metadata.clone());
                    any_found = true;
                }
            } else if !metadata_fields.is_empty()
                && let Some(Value::Object(metadata)) = doc.get("metadata")
            {
                let mut kept = Map::new();
                for name in &metadata_fields {
                    if let Some(value) = metadata.get(*name) {
                        kept.insert((*name).to_owned(), value.clone());
                        any_found = true;
                    }
                }
                if !kept.is_empty() {
                    out.insert("metadata".to_owned(), Value::Object(kept));
                }
            }
            Value::Object(out)
        })
        .collect();
    if any_found { projected } else { docs }
}

/// The filter the SDK's `_build_collection_filter` produced, for the
/// "No documents found" message: the caller's filter, plus the index name,
/// and the exclusion of index-meta rows.
///
/// The Rust tools do not send this filter (the index is a namespace and
/// there are no meta rows); it exists so the message reads as it did.
#[must_use]
pub fn sdk_collection_filter(filter: Option<&Value>, index_name: &str) -> Value {
    let mut filter = match filter {
        Some(Value::Object(map)) => map.clone(),
        _ => Map::new(),
    };
    if !index_name.is_empty() {
        filter.insert("collection".to_owned(), json!({"$eq": index_name.trim()}));
    }
    let meta = json!({"$or": [
        {"type": {"$exists": false}},
        {"type": {"$ne": "index_meta"}}
    ]});
    if filter.is_empty() {
        meta
    } else {
        json!({"$and": [Value::Object(filter), meta]})
    }
}

/// `No documents found by query '…' and filter '…'`.
#[must_use]
pub fn no_documents_message(query: &str, collection_filter: &Value) -> String {
    format!(
        "No documents found by query '{query}' and filter '{}'",
        pyfmt::repr(collection_filter)
    )
}

/// `Index '…' not found. Available indexes: …`.
#[must_use]
pub fn index_not_found_message(index_name: &str, available: &str) -> String {
    format!("Index '{index_name}' not found. Available indexes: {available}")
}

/// What `list_indexes` returns for no indexes.
pub const NO_INDEXES: &str = "No indexed collections";

/// The stepback search tool's result text.
#[must_use]
pub fn stepback_result_text(docs: &[Value]) -> String {
    if docs.is_empty() {
        "No documents found matching the query.".to_owned()
    } else {
        format!(
            "Found {} documents matching the query\n{}",
            docs.len(),
            pyfmt::json_dumps(&Value::Array(docs.to_vec()), 4)
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn hit(content: &str, metadata: &Value, score: f64) -> Hit {
        Hit {
            key: "k_0".to_owned(),
            page_content: content.to_owned(),
            metadata: metadata.as_object().cloned().unwrap_or_default(),
            score,
            document_key: "k".to_owned(),
            chunk_id: "0".to_owned(),
        }
    }

    #[test]
    fn extensions_follow_splitext() {
        for (name, expected) in [
            ("a.py", ".py"),
            ("dir.d/a", ""),
            ("dir.d/a.tar.gz", ".gz"),
            (".bashrc", ""),
            ("..x", ""),
            ("a.", "."),
            ("unknown", ""),
            ("Main.PY", ".PY"),
        ] {
            assert_eq!(extension(name), expected, "{name}");
        }
        assert_eq!(language_for(".PY"), "unknown");
        assert_eq!(language_for(".tsx"), "typescript");
        assert_eq!(language_for(".cs"), "c_sharp");
    }

    #[test]
    fn code_documents_print_as_file_method_and_a_fenced_block() {
        let found = hit(
            "def run():\n    pass",
            &json!({"filename": "src/app.py", "method_name": "run", "id": "x"}),
            0.8,
        );
        let out = format_hits(&[found], Doctype::Code);
        assert_eq!(
            out[0]["page_content"],
            "src/app.py -> run (score: 0.8)\n\n```python\ndef run():\n    pass\n```\n\n"
        );
        assert_eq!(out[0]["score"], json!(0.8));
        assert_eq!(out[0]["metadata"]["id"], "x");
        let keys: Vec<&String> = out[0].as_object().unwrap().keys().collect();
        assert_eq!(keys, ["page_content", "metadata", "score"]);
    }

    #[test]
    fn code_documents_without_metadata_use_the_sdk_defaults() {
        let out = format_hits(&[hit("body", &json!({}), 0.25)], Doctype::Code);
        assert_eq!(
            out[0]["page_content"],
            "unknown -> text (score: 0.25)\n\n```unknown\nbody\n```\n\n"
        );
    }

    #[test]
    fn plain_documents_are_page_content_metadata_score() {
        let out = format_hits(&[hit("body", &json!({"a": 1}), 0.5)], Doctype::Document);
        assert_eq!(
            serde_json::to_string(&out[0]).unwrap(),
            r#"{"page_content":"body","metadata":{"a":1},"score":0.5}"#
        );
    }

    fn docs() -> Vec<Value> {
        vec![
            json!({"page_content": "one", "metadata": {"source": "a", "config.timeout": 3, "n": 1}, "score": 0.9}),
            json!({"page_content": "two", "metadata": {"n": 2}, "score": 0.8}),
        ]
    }

    #[test]
    fn output_fields_project_top_level_and_metadata_fields() {
        let out = project_fields(docs(), &["score".into(), "page_content".into()]);
        assert_eq!(
            serde_json::to_string(&out).unwrap(),
            r#"[{"page_content":"one","score":0.9},{"page_content":"two","score":0.8}]"#
        );
        let out = project_fields(
            docs(),
            &["metadata.source".into(), "metadata.config.timeout".into()],
        );
        assert_eq!(
            serde_json::to_string(&out).unwrap(),
            r#"[{"metadata":{"source":"a","config.timeout":3}},{}]"#
        );
        let out = project_fields(docs(), &["metadata".into(), "metadata.n".into()]);
        assert_eq!(out[1], json!({"metadata": {"n": 2}}));
    }

    #[test]
    fn invalid_or_absent_output_fields_return_everything() {
        assert_eq!(project_fields(docs(), &[]), docs());
        assert_eq!(
            project_fields(docs(), &["nope".into(), "metadata.".into()]),
            docs()
        );
        assert_eq!(project_fields(docs(), &["metadata.absent".into()]), docs());
    }

    #[test]
    fn the_no_documents_message_prints_the_sdk_filter() {
        let message = no_documents_message("find me", &sdk_collection_filter(None, ""));
        assert_eq!(
            message,
            "No documents found by query 'find me' and filter '{'$or': [{'type': {'$exists': False}}, {'type': {'$ne': 'index_meta'}}]}'"
        );
        let filter = json!({"author": "ann"});
        let message = no_documents_message("q", &sdk_collection_filter(Some(&filter), " docs "));
        assert_eq!(
            message,
            "No documents found by query 'q' and filter '{'$and': [{'author': 'ann', 'collection': {'$eq': 'docs'}}, {'$or': [{'type': {'$exists': False}}, {'type': {'$ne': 'index_meta'}}]}]}'"
        );
    }

    #[test]
    fn stepback_results_are_counted_and_dumped() {
        assert_eq!(
            stepback_result_text(&[]),
            "No documents found matching the query."
        );
        let text = stepback_result_text(&[json!({"page_content": "x", "score": 0.5})]);
        assert_eq!(
            text,
            "Found 1 documents matching the query\n[\n    {\n        \"page_content\": \"x\",\n        \"score\": 0.5\n    }\n]"
        );
    }

    #[test]
    fn index_messages() {
        assert_eq!(
            index_not_found_message("x", "a,b"),
            "Index 'x' not found. Available indexes: a,b"
        );
        assert_eq!(NO_INDEXES, "No indexed collections");
    }
}
