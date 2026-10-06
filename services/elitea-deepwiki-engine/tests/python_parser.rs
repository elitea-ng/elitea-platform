//! The Python parser against the Python engine on a corpus of tricky
//! Python (`tests/fixtures/python`): decorators, nested and local classes,
//! async code, every annotation shape, PEP 695, `match`, f-strings, odd
//! docstrings, CRLF, a BOM, a non-UTF-8 file, syntax errors and a small
//! package with cross-file inheritance, calls and creation.
//!
//! `python.expected.jsonl` is what `PythonParser.parse_multiple_files`
//! returned for these files (sorted, inline executor), dumped as
//! `parity/python_parse_dump.py` dumps a repository. Every field must match,
//! in order — except the TEXT of a syntax error, where only the kind is
//! compared for the files in [`MESSAGE_ONLY`]: Python picks among its
//! specialised error rules, which tree-sitter cannot tell apart.

use elitea_deepwiki_engine::parsers::parser_for;
use serde_json::Value;
use std::collections::BTreeMap;
use std::path::PathBuf;

/// Files whose syntax error MESSAGE differs (the result shape does not).
const MESSAGE_ONLY: &[&str] = &["broken.py"];

fn fixtures() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("tests/fixtures/python")
}

fn expected() -> BTreeMap<String, Value> {
    let path =
        PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("tests/fixtures/python.expected.jsonl");
    let text = std::fs::read_to_string(path).unwrap_or_default();
    text.lines()
        .filter_map(|line| serde_json::from_str::<Value>(line).ok())
        .filter_map(|row| {
            let path = row.get("file_path")?.as_str()?.to_owned();
            Some((path, row))
        })
        .collect()
}

fn relativise(value: &mut Value, prefix: &str) {
    match value {
        Value::String(text) => {
            if let Some(rest) = text.strip_prefix(prefix) {
                *text = rest.to_owned();
            }
        }
        Value::Array(items) => items.iter_mut().for_each(|v| relativise(v, prefix)),
        Value::Object(map) => map.values_mut().for_each(|v| relativise(v, prefix)),
        _ => {}
    }
}

/// Parse the fixture files and dump them as the reference is dumped.
fn parse_fixtures(relative: &[String]) -> BTreeMap<String, Value> {
    let root = fixtures();
    let prefix = format!("{}/", root.to_string_lossy());
    let files: Vec<String> = relative
        .iter()
        .map(|r| root.join(r).to_string_lossy().into_owned())
        .collect();
    let parser = parser_for("python");
    assert!(parser.is_some());
    let Some(parser) = parser else {
        return BTreeMap::new();
    };
    parser
        .parse_files(&files)
        .into_iter()
        .map(|(path, result)| {
            let mut value = serde_json::to_value(&result).unwrap_or_default();
            relativise(&mut value, &prefix);
            if let Some(object) = value.as_object_mut() {
                object.remove("warnings");
            }
            let key = path.strip_prefix(&prefix).unwrap_or(&path).to_owned();
            (key, value)
        })
        .collect()
}

fn error_kinds(value: &Value) -> Vec<String> {
    value["errors"]
        .as_array()
        .map(|errors| {
            errors
                .iter()
                .map(|e| {
                    e.as_str()
                        .unwrap_or("")
                        .split(':')
                        .next()
                        .unwrap_or("")
                        .to_owned()
                })
                .collect()
        })
        .unwrap_or_default()
}

#[test]
fn the_fixture_corpus_parses_as_the_python_engine_parses_it() {
    let expected = expected();
    assert_eq!(expected.len(), 20, "the reference lists every fixture");
    let files: Vec<String> = expected.keys().cloned().collect();
    let actual = parse_fixtures(&files);
    for (path, want) in &expected {
        let got = &actual[path];
        for field in [
            "symbols",
            "relationships",
            "imports",
            "exports",
            "dependencies",
            "module_docstring",
            "language",
        ] {
            assert_eq!(got[field], want[field], "{path}: {field}");
        }
        if MESSAGE_ONLY.contains(&path.as_str()) {
            assert_eq!(error_kinds(got), error_kinds(want), "{path}: errors");
        } else {
            assert_eq!(got["errors"], want["errors"], "{path}: errors");
        }
    }
}

#[test]
fn the_output_does_not_depend_on_scheduling() {
    let files: Vec<String> = expected().keys().cloned().collect();
    let first = parse_fixtures(&files);
    for _ in 0..3 {
        assert_eq!(parse_fixtures(&files), first);
    }
}
