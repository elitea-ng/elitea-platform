//! Generates `codes` and `REGISTRY` from `libs/go/faults/registry/*.json`.
//!
//! The Go package owns the format; this script refuses any file the Go
//! well-formedness test would refuse, so a malformed or duplicate code fails
//! the Rust build too, not only Go CI.

use std::collections::BTreeMap;
use std::env;
use std::error::Error;
use std::fmt::Write as _;
use std::fs;
use std::path::{Path, PathBuf};

use serde_json::{Map, Value};

/// Bytes, like Main's `MaxRuntimeFailureSafeMessageBytes`.
const MAX_TEXT_BYTES: usize = 256;
const KNOWN_KEYS: [&str; 8] = [
    "code",
    "sentence",
    "action",
    "audience",
    "retryable",
    "owner",
    "runtime_kind",
    "wire_code",
];

struct Entry {
    file: String,
    code: String,
    sentence: String,
    action: String,
    audience: &'static str,
    retryable: &'static str,
    owner: String,
    runtime_kind: Option<String>,
    wire_code: Option<String>,
}

fn main() -> Result<(), Box<dyn Error>> {
    let manifest_dir =
        PathBuf::from(env::var_os("CARGO_MANIFEST_DIR").ok_or("CARGO_MANIFEST_DIR is required")?);
    let registry = manifest_dir.join("../../go/faults/registry");
    println!("cargo::rerun-if-changed={}", registry.display());
    let mut files: Vec<PathBuf> = fs::read_dir(&registry)
        .map_err(|error| {
            format!(
                "cannot read the fault registry {}: {error}",
                registry.display()
            )
        })?
        .filter_map(Result::ok)
        .map(|entry| entry.path())
        .filter(|path| {
            path.extension()
                .is_some_and(|extension| extension == "json")
        })
        .collect();
    files.sort();
    if files.is_empty() {
        return Err(format!("no registry files in {}", registry.display()).into());
    }
    let mut entries = Vec::new();
    for file in &files {
        println!("cargo::rerun-if-changed={}", file.display());
        read_file(file, &mut entries)?;
    }
    let generated = generate(&entries)?;
    let out = PathBuf::from(env::var_os("OUT_DIR").ok_or("OUT_DIR is required")?);
    fs::write(out.join("registry.rs"), generated)?;
    Ok(())
}

fn read_file(path: &Path, entries: &mut Vec<Entry>) -> Result<(), Box<dyn Error>> {
    let name = path
        .file_name()
        .and_then(|name| name.to_str())
        .ok_or("registry file name is not UTF-8")?
        .to_owned();
    let text = fs::read_to_string(path)?;
    let root: Value =
        serde_json::from_str(&text).map_err(|error| format!("{name}: invalid JSON: {error}"))?;
    let root = root
        .as_object()
        .ok_or_else(|| format!("{name}: not an object"))?;
    for key in root.keys() {
        if key != "version" && key != "entries" {
            return Err(format!("{name}: unknown key {key:?}").into());
        }
    }
    if root.get("version").and_then(Value::as_u64) != Some(1) {
        return Err(format!("{name}: version must be 1").into());
    }
    let list = root
        .get("entries")
        .and_then(Value::as_array)
        .ok_or_else(|| format!("{name}: entries must be an array"))?;
    for (index, value) in list.iter().enumerate() {
        let object = value
            .as_object()
            .ok_or_else(|| format!("{name}: entry {index} is not an object"))?;
        entries.push(read_entry(&name, index, object)?);
    }
    Ok(())
}

fn read_entry(file: &str, index: usize, object: &Map<String, Value>) -> Result<Entry, String> {
    let at = |field: &str| format!("{file}: entry {index} field {field}");
    for key in object.keys() {
        if !KNOWN_KEYS.contains(&key.as_str()) {
            return Err(format!("{file}: entry {index} has unknown key {key:?}"));
        }
    }
    let text = |field: &str| -> Result<String, String> {
        object
            .get(field)
            .and_then(Value::as_str)
            .map(str::to_owned)
            .ok_or_else(|| format!("{} is required and must be a string", at(field)))
    };
    let optional = |field: &str| -> Result<Option<String>, String> {
        match object.get(field) {
            None => Ok(None),
            Some(Value::String(value)) if !value.is_empty() => Ok(Some(value.clone())),
            Some(_) => Err(format!("{} must be a non-empty string", at(field))),
        }
    };
    let code = text("code")?;
    if !valid_code(&code) {
        return Err(format!("{}: {code:?} breaks the code grammar", at("code")));
    }
    let sentence = text("sentence")?;
    check_text(&sentence, true)
        .map_err(|reason| format!("{} ({code}): {reason}", at("sentence")))?;
    let action = text("action")?;
    check_text(&action, false).map_err(|reason| format!("{} ({code}): {reason}", at("action")))?;
    let audience = match text("audience")?.as_str() {
        "operator" => "Operator",
        "user" => "User",
        "both" => "Both",
        other => {
            return Err(format!(
                "{} ({code}): {other:?} is not operator, user or both",
                at("audience")
            ));
        }
    };
    let retryable = match text("retryable")?.as_str() {
        "yes" => "Yes",
        "no" => "No",
        "n/a" => "NotApplicable",
        other => {
            return Err(format!(
                "{} ({code}): {other:?} is not yes, no or n/a",
                at("retryable")
            ));
        }
    };
    let owner = text("owner")?;
    if owner.is_empty() {
        return Err(format!("{} ({code}): empty", at("owner")));
    }
    Ok(Entry {
        file: file.to_owned(),
        code,
        sentence,
        action,
        audience,
        retryable,
        owner,
        runtime_kind: optional("runtime_kind")?,
        wire_code: optional("wire_code")?,
    })
}

/// `^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`
fn valid_code(code: &str) -> bool {
    let mut segments = 0;
    for segment in code.split('.') {
        let mut bytes = segment.bytes();
        if !bytes.next().is_some_and(|byte| byte.is_ascii_lowercase()) {
            return false;
        }
        if !bytes.all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'_') {
            return false;
        }
        segments += 1;
    }
    segments >= 2
}

/// The sentence and action bans (review condition C7): bounded, no
/// placeholder or format verb, nothing that looks like an id, a path or a URL.
fn check_text(text: &str, sentence: bool) -> Result<(), String> {
    if text.trim().is_empty() {
        return Err("empty".into());
    }
    if text.len() > MAX_TEXT_BYTES {
        return Err(format!("{} bytes, over {MAX_TEXT_BYTES}", text.len()));
    }
    if sentence && !text.ends_with('.') {
        return Err("does not end with a period".into());
    }
    if text.contains('{') || text.contains('}') || text.contains('%') {
        return Err("holds a placeholder or format verb".into());
    }
    if text.contains("://") || text.contains("www.") {
        return Err("holds a URL".into());
    }
    if text.contains('/') || text.contains('\\') {
        return Err("holds a path".into());
    }
    for token in
        text.split(|character: char| !(character.is_ascii_alphanumeric() || character == '-'))
    {
        if looks_like_id(token) {
            return Err(format!("holds an id-like token {token:?}"));
        }
    }
    Ok(())
}

/// A hex run of eight or more with a digit (uuid parts, digests) or five or
/// more digits in a row (numeric ids).
fn looks_like_id(token: &str) -> bool {
    let hexlike = token.len() >= 8
        && token
            .bytes()
            .all(|byte| byte.is_ascii_hexdigit() || byte == b'-')
        && token.bytes().any(|byte| byte.is_ascii_digit());
    let mut run = 0;
    let mut long_number = false;
    for byte in token.bytes() {
        run = if byte.is_ascii_digit() { run + 1 } else { 0 };
        long_number |= run >= 5;
    }
    hexlike || long_number
}

fn constant_name(code: &str) -> String {
    code.replace('.', "_").to_ascii_uppercase()
}

fn generate(entries: &[Entry]) -> Result<String, Box<dyn Error>> {
    let mut codes: BTreeMap<&str, &Entry> = BTreeMap::new();
    let mut names: BTreeMap<String, &str> = BTreeMap::new();
    for entry in entries {
        if let Some(previous) = codes.insert(&entry.code, entry) {
            return Err(format!(
                "duplicate code {:?} in {} and {}",
                entry.code, previous.file, entry.file
            )
            .into());
        }
        let name = constant_name(&entry.code);
        if let Some(previous) = names.insert(name.clone(), &entry.code) {
            return Err(format!(
                "codes {previous:?} and {:?} generate the same constant {name}",
                entry.code
            )
            .into());
        }
    }
    let mut out =
        String::from("// @generated by build.rs from libs/go/faults/registry. Do not edit.\n\n");
    for (index, entry) in codes.values().enumerate() {
        writeln!(
            out,
            "const E{index}: Entry = Entry {{ code: {:?}, sentence: {:?}, action: {:?}, audience: Audience::{}, retryable: Retryable::{}, owner: {:?}, runtime_kind: {:?}, wire_code: {:?} }};",
            entry.code,
            entry.sentence,
            entry.action,
            entry.audience,
            entry.retryable,
            entry.owner,
            entry.runtime_kind,
            entry.wire_code,
        )?;
    }
    out.push_str(
        "\n/// Every registered entry, sorted by code.\npub static REGISTRY: &[Entry] = &[",
    );
    for index in 0..codes.len() {
        write!(out, "E{index}, ")?;
    }
    out.push_str("];\n\n/// The registered codes. A code that is not registered does not compile.\npub mod codes {\n");
    for (index, entry) in codes.values().enumerate() {
        writeln!(out, "    #[doc = {:?}]", entry.sentence)?;
        writeln!(
            out,
            "    pub const {}: crate::Code = crate::Code(&super::E{index});",
            constant_name(&entry.code)
        )?;
    }
    out.push_str("}\n");
    Ok(out)
}
