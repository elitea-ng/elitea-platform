//! ADR-0026 decision 8 for the page prompts: every embedded prompt is the
//! Python value, byte for byte.
//!
//! For each `src/wiki/prompts/PROMPTS_MANIFEST.json` entry the test reads
//! the Python SOURCE in this repository (no Python needed), evaluates the
//! string literal the entry names, and requires the SHA-256 of that value
//! to equal the manifest's and the embedded file's.

use serde_json::Value;
use sha2::{Digest, Sha256};
use std::path::{Path, PathBuf};

fn root() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
}

fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    bytes.iter().fold(String::new(), |mut out, b| {
        let _ = write!(out, "{b:02x}");
        out
    })
}

/// Evaluate a Python string literal body (the text between the quotes):
/// the escapes the prompt files may use; anything else fails the test.
fn evaluate(body: &str) -> String {
    let mut out = String::new();
    let mut chars = body.chars();
    while let Some(c) = chars.next() {
        if c != '\\' {
            out.push(c);
            continue;
        }
        match chars.next() {
            Some('n') => out.push('\n'),
            Some('t') => out.push('\t'),
            Some('\\') => out.push('\\'),
            Some('"') => out.push('"'),
            Some('\'') => out.push('\''),
            Some('\n') => {}
            other => panic!("unsupported escape \\{other:?} in a prompt literal"),
        }
    }
    out
}

/// The value of `symbol` in `source`.
fn python_value(source: &str, symbol: &str) -> String {
    if let Some((dict, key)) = symbol.split_once("['") {
        // `NAME = {` … `"key": "value",`
        let key = key.trim_end_matches("']");
        let start = source
            .find(&format!("{dict} = {{"))
            .expect("dict assignment");
        let line = source[start..]
            .lines()
            .find(|l| l.trim_start().starts_with(&format!("\"{key}\":")))
            .expect("dict entry");
        let value = line
            .split_once(": ")
            .expect("entry value")
            .1
            .trim()
            .trim_end_matches(',');
        return evaluate(value.trim_matches('"'));
    }
    if let Some(literal) = symbol.strip_suffix(" (system message)") {
        // `("system", "…")` inside the named method.
        let method = literal.rsplit('.').next().expect("method");
        let start = source.find(&format!("def {method}(")).expect("method");
        let tail = &source[start..];
        let at = tail.find("(\"system\", \"").expect("system tuple") + "(\"system\", \"".len();
        let end = tail[at..].find("\")").expect("end of tuple");
        return evaluate(&tail[at..at + end]);
    }
    let marker = format!("\n{symbol} = \"\"\"");
    let start = source.find(&marker).expect("assignment") + marker.len();
    let end = source[start..].find("\"\"\"").expect("closing quotes");
    evaluate(&source[start..start + end])
}

#[test]
fn every_prompt_is_the_python_value() {
    let manifest: Value =
        serde_json::from_str(include_str!("../src/wiki/prompts/PROMPTS_MANIFEST.json")).unwrap();
    let prompts = manifest["prompts"].as_array().unwrap();
    assert!(prompts.len() >= 3);
    let repo = root().join("../..");
    for entry in prompts {
        let file = entry["file"].as_str().unwrap();
        let expected = entry["sha256"].as_str().unwrap();
        let embedded = std::fs::read(root().join("src/wiki/prompts").join(file)).unwrap();
        assert_eq!(
            hex(&Sha256::digest(&embedded)),
            expected,
            "{file}: embedded bytes"
        );
        let source = std::fs::read_to_string(repo.join(entry["source"].as_str().unwrap())).unwrap();
        let value = python_value(&source, entry["symbol"].as_str().unwrap());
        assert_eq!(
            hex(&Sha256::digest(value.as_bytes())),
            expected,
            "{file}: Python source"
        );
        assert_eq!(
            value.chars().count() as u64,
            entry["chars"].as_u64().unwrap(),
            "{file}: length"
        );
    }
}

#[test]
fn the_manifest_names_every_embedded_prompt() {
    let manifest: Value =
        serde_json::from_str(include_str!("../src/wiki/prompts/PROMPTS_MANIFEST.json")).unwrap();
    let named: Vec<&str> = manifest["prompts"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| e["file"].as_str().unwrap())
        .collect();
    for entry in std::fs::read_dir(root().join("src/wiki/prompts")).unwrap() {
        let name = entry.unwrap().file_name().into_string().unwrap();
        if Path::new(&name).extension().is_some_and(|e| e == "txt") {
            assert!(
                named.contains(&name.as_str()),
                "{name} is not in the manifest"
            );
        }
    }
}
