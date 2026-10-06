//! ADR-0026 decision 8: every prompt the analysis and planning nodes send
//! is the Python value, byte for byte. `PROMPTS_MANIFEST.json` records the
//! SHA-256 of each Python value; this test re-derives every hash from the
//! Python SOURCE (python3's `ast`, no engine import: the engine's
//! dependencies are not a test dependency) and from the embedded text.

use elitea_deepwiki_engine::structure::prompts;
use serde_json::Value;
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, BTreeSet};
use std::path::PathBuf;
use std::process::Command;

/// Reads the manifest on stdin, prints `{file: sha256}` of the Python values.
const DERIVE: &str = r#"
import ast, hashlib, json, sys
root = sys.argv[1]
manifest = json.load(sys.stdin)

def module_constant(path, name):
    tree = ast.parse(open(path, encoding="utf-8").read())
    value = None
    for node in tree.body:  # the last assignment wins, as at import
        if isinstance(node, ast.Assign) and any(isinstance(t, ast.Name) and t.id == name for t in node.targets):
            value = ast.literal_eval(node.value)
    return value

def inline_system(path, qualname, index):
    cls, fn = qualname.split(".")
    tree = ast.parse(open(path, encoding="utf-8").read())
    for node in tree.body:
        if isinstance(node, ast.ClassDef) and node.name == cls:
            for f in node.body:
                if isinstance(f, ast.FunctionDef) and f.name == fn:
                    found = sorted(
                        (n for n in ast.walk(f)
                         if isinstance(n, ast.Tuple) and len(n.elts) == 2
                         and isinstance(n.elts[0], ast.Constant) and n.elts[0].value == "system"
                         and isinstance(n.elts[1], ast.Constant) and isinstance(n.elts[1].value, str)),
                        key=lambda n: (n.lineno, n.col_offset))
                    return found[index].elts[1].value
    raise SystemExit(f"{qualname} not found")

out = {}
for entry in manifest["prompts"]:
    path = root + "/" + entry["source"]
    selector = entry.get("selector")
    if selector and selector.startswith("system["):
        value = inline_system(path, entry["symbol"], int(selector[7:-1]))
    else:
        value = module_constant(path, entry["symbol"])
        if selector:
            value = value[ast.literal_eval(selector[1:-1])]
    out[entry["file"]] = hashlib.sha256(value.encode("utf-8")).hexdigest()
print(json.dumps(out))
"#;

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

#[test]
fn python_source_values_match_the_manifest() {
    let root = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../elitea-deepwiki");
    let mut child = Command::new("python3")
        .arg("-c")
        .arg(DERIVE)
        .arg(&root)
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn()
        .expect("python3 runs (it is a test dependency, as git is for the ingest tests)");
    {
        use std::io::Write;
        let mut stdin = child.stdin.take().expect("stdin");
        stdin
            .write_all(prompts::MANIFEST.as_bytes())
            .expect("write manifest");
    }
    let output = child.wait_with_output().expect("python3 finished");
    assert!(
        output.status.success(),
        "deriving the hashes failed: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    let python: BTreeMap<String, String> =
        serde_json::from_slice(&output.stdout).expect("python printed JSON");
    assert_eq!(python, manifest_hashes());
}
