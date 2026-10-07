//! ADR-0026 decision 8 for the page prompts: every embedded prompt is the
//! Python value, byte for byte.
//!
//! `src/wiki/prompts/PROMPTS_MANIFEST.json` records the SHA-256 and length
//! of each Python value; the test holds every embedded file to them. The
//! manifest's `source` paths are historical: the Python engine was deleted
//! after origin/main 1233e1582.

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

#[test]
fn every_prompt_is_the_recorded_python_value() {
    let manifest: Value =
        serde_json::from_str(include_str!("../src/wiki/prompts/PROMPTS_MANIFEST.json")).unwrap();
    let prompts = manifest["prompts"].as_array().unwrap();
    assert!(prompts.len() >= 3);
    for entry in prompts {
        let file = entry["file"].as_str().unwrap();
        let expected = entry["sha256"].as_str().unwrap();
        let embedded = std::fs::read(root().join("src/wiki/prompts").join(file)).unwrap();
        assert_eq!(
            hex(&Sha256::digest(&embedded)),
            expected,
            "{file}: embedded bytes"
        );
        let value = String::from_utf8(embedded).unwrap();
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
