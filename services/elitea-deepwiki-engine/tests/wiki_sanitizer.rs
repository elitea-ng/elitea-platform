//! The Mermaid sanitizer gate: `tests/fixtures/wiki/sanitizer_corpus.jsonl`
//! is Python's `sanitize_content` over real diagrams (design documents,
//! the Mermaid project's own README examples) and hand cases that break
//! diagrams the ways models do (`parity/python_sanitizer_corpus.py`).
//!
//! Every page must come out byte-identical, every diagram with Python's
//! status and fix list; a case Python raised on must fail here too (the
//! page path then keeps the unsanitised text).

use elitea_deepwiki_engine::wiki::sanitizer::{DiagramStatus, SanitizerConfig, sanitize_content};
use serde_json::Value;
use std::path::PathBuf;

fn corpus() -> Vec<Value> {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures/wiki/sanitizer_corpus.jsonl");
    std::fs::read_to_string(path)
        .unwrap()
        .lines()
        .map(|line| serde_json::from_str(line).unwrap())
        .collect()
}

fn status_name(status: DiagramStatus) -> &'static str {
    match status {
        DiagramStatus::Valid => "valid",
        DiagramStatus::Fixed => "fixed",
        DiagramStatus::Failed => "failed",
    }
}

#[test]
fn every_corpus_page_matches_python_exactly() {
    let cases = corpus();
    assert!(cases.len() >= 90, "the corpus has {} cases", cases.len());
    let mut failures = Vec::new();
    for case in &cases {
        let name = case["name"].as_str().unwrap();
        let input = case["input"].as_str().unwrap();
        let result = sanitize_content(input, &SanitizerConfig::default());
        match (&case["output"], result) {
            (Value::Null, Err(_)) => {}
            (Value::Null, Ok((output, _))) => {
                failures.push(format!("{name}: Python raised, Rust returned {output:?}"));
            }
            (expected, Err(error)) => {
                failures.push(format!(
                    "{name}: Rust raised {error}, Python returned {expected}"
                ));
            }
            (expected, Ok((output, summary))) => {
                if expected.as_str() != Some(output.as_str()) {
                    failures.push(format!(
                        "{name}: output differs\n--- python\n{}\n--- rust\n{output}",
                        expected.as_str().unwrap_or_default()
                    ));
                    continue;
                }
                let diagrams = case["diagrams"].as_array().unwrap();
                if diagrams.len() != summary.records.len() {
                    failures.push(format!(
                        "{name}: {} diagrams vs {}",
                        diagrams.len(),
                        summary.records.len()
                    ));
                    continue;
                }
                for (want, got) in diagrams.iter().zip(&summary.records) {
                    let fixes: Vec<String> = serde_json::from_value(want["fixes"].clone()).unwrap();
                    if want["status"] != status_name(got.status)
                        || fixes != got.fixes
                        || want["hash"] != got.hash.as_str()
                    {
                        failures.push(format!(
                            "{name}: record differs: python {want} / rust {:?} {:?} {}",
                            got.status, got.fixes, got.hash
                        ));
                    }
                }
            }
        }
    }
    assert!(
        failures.is_empty(),
        "{} of {} cases differ:\n{}",
        failures.len(),
        cases.len(),
        failures.join("\n\n")
    );
}
