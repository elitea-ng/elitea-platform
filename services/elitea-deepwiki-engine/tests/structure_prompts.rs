//! ADR-0026 decision 8: every prompt the analysis and planning nodes send
//! is the Python value, byte for byte. `PROMPTS_MANIFEST.json` records the
//! SHA-256 of each Python value (its `source` paths are historical: the
//! Python engine was deleted after origin/main 1233e1582); this test checks
//! the embedded text against those hashes.

use elitea_deepwiki_engine::structure::prompts;
use serde_json::Value;
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, BTreeSet};
use std::path::PathBuf;

fn manifest() -> Value {
    serde_json::from_str(prompts::MANIFEST).expect("the manifest is JSON")
}

fn manifest_hashes() -> BTreeMap<String, String> {
    manifest()["prompts"]
        .as_array()
        .expect("prompts")
        .iter()
        .map(|e| {
            (
                e["file"].as_str().expect("file").to_owned(),
                e["sha256"].as_str().expect("sha256").to_owned(),
            )
        })
        .collect()
}

#[test]
fn embedded_prompts_match_the_manifest() {
    let manifest = manifest_hashes();
    let embedded: BTreeMap<String, String> = prompts::ALL
        .iter()
        .map(|(file, text)| {
            (
                (*file).to_owned(),
                format!("{:x}", Sha256::digest(text.as_bytes())),
            )
        })
        .collect();
    assert_eq!(embedded, manifest);
    // Every data file is embedded and listed.
    let dir = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("src/structure/prompts");
    let on_disk: BTreeSet<String> = std::fs::read_dir(&dir)
        .expect("prompts dir")
        .map(|e| e.expect("entry").file_name().into_string().expect("utf-8"))
        .filter(|name| {
            std::path::Path::new(name)
                .extension()
                .is_some_and(|e| e == "txt")
        })
        .collect();
    assert_eq!(on_disk, manifest.keys().cloned().collect());
}
