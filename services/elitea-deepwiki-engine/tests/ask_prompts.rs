//! ADR-0026 decision 8: the `ask`, deep research and `resolve_wiki`
//! prompts are the Python values, byte for byte.
//!
//! * The engine's own prompts: held to the manifest's SHA-256 of each
//!   Python value. The manifest's `source` paths are historical (the Python
//!   engine was deleted after origin/main 1233e1582).
//! * The third-party texts (`LangChain`'s todo and summary prompts,
//!   `deepagents`' summary prompt), pinned by package version in the
//!   manifest: the todo prompt is held to the recorded deep research
//!   request; with `ELITEA_DEEPWIKI_PARITY_VENV` set (a venv with the
//!   pinned `langchain` and `deepagents` versions) every one is re-read
//!   from the installed package.

use elitea_deepwiki_engine::ask::prompts;
use serde_json::Value;
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, BTreeSet};
use std::path::PathBuf;
use std::process::Command;

const PACKAGES: &str = r#"
import hashlib, importlib, json, sys
manifest = json.load(sys.stdin)
out = {}
for entry in manifest["prompts"]:
    if "module" in entry:
        value = getattr(importlib.import_module(entry["module"]), entry["symbol"])
        out[entry["file"]] = hashlib.sha256(value.encode("utf-8")).hexdigest()
print(json.dumps(out))
"#;

fn manifest() -> Vec<Value> {
    let parsed: Value = serde_json::from_str(prompts::MANIFEST).expect("the manifest is JSON");
    parsed["prompts"].as_array().expect("prompts").clone()
}

fn hashes(filter: impl Fn(&Value) -> bool) -> BTreeMap<String, String> {
    manifest()
        .iter()
        .filter(|e| filter(e))
        .map(|e| {
            (
                e["file"].as_str().expect("file").to_owned(),
                e["sha256"].as_str().expect("sha256").to_owned(),
            )
        })
        .collect()
}

fn run_python(python: &str, script: &str, arg: &str) -> BTreeMap<String, String> {
    let mut child = Command::new(python)
        .arg("-c")
        .arg(script)
        .arg(arg)
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn()
        .expect("python3 runs (a test dependency)");
    {
        use std::io::Write;
        let mut stdin = child.stdin.take().expect("stdin");
        stdin
            .write_all(prompts::MANIFEST.as_bytes())
            .expect("write manifest");
    }
    let output = child.wait_with_output().expect("python finished");
    assert!(
        output.status.success(),
        "deriving the hashes failed: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    serde_json::from_slice(&output.stdout).expect("python printed JSON")
}

#[test]
fn embedded_prompts_match_the_manifest() {
    let embedded: BTreeMap<String, String> = prompts::ALL
        .iter()
        .map(|(file, text)| {
            (
                (*file).to_owned(),
                format!("{:x}", Sha256::digest(text.as_bytes())),
            )
        })
        .collect();
    assert_eq!(embedded, hashes(|_| true));
    let dir = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("src/ask/prompts");
    // LangChain's summary prompt lives with the shared conversation crate
    // (libs/rust/conversation), which both engines' agents use.
    let shared = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../libs/rust/conversation/src/summary_prompt.txt");
    assert!(shared.is_file(), "{}", shared.display());
    let on_disk: BTreeSet<String> = std::iter::once("summary_prompt.txt".to_owned())
        .chain(
            std::fs::read_dir(&dir)
                .expect("prompts dir")
                .map(|e| e.expect("entry").file_name().into_string().expect("utf-8"))
                .filter(|name| {
                    std::path::Path::new(name)
                        .extension()
                        .is_some_and(|e| e == "txt")
                }),
        )
        .collect();
    assert_eq!(on_disk, embedded.keys().cloned().collect());
}

#[test]
fn the_todo_prompt_is_the_one_python_sent() {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures/ask/ref/deep_research/requests.jsonl");
    let first = std::fs::read_to_string(path).expect("recording");
    let request: Value =
        serde_json::from_str(first.lines().next().expect("a request")).expect("JSON");
    let block = request["messages"][0]["content"][1]["text"]
        .as_str()
        .expect("the todo block");
    assert_eq!(block, format!("\n\n{}", prompts::TODO_SYSTEM));
}

#[test]
fn third_party_values_match_the_installed_packages() {
    let Ok(venv) = std::env::var("ELITEA_DEEPWIKI_PARITY_VENV") else {
        eprintln!(
            "ELITEA_DEEPWIKI_PARITY_VENV is not set: the package texts are checked by their hashes only"
        );
        return;
    };
    let python = PathBuf::from(venv).join("bin/python");
    let derived = run_python(&python.to_string_lossy(), PACKAGES, "");
    assert_eq!(derived, hashes(|e| e.get("module").is_some()));
}
