//! Shared test utilities for `zavora-slide` integration tests.
//!
//! Provides:
//! - `package_entries`: extracts all byte-comparable entries from an OPC package
//!   (parts, per-part `.rels`, package `.rels`, and `[Content_Types].xml`).
//! - `assert_only_changed`: asserts that only the listed parts differ between two
//!   snapshots, and all other entries are byte-identical.
//! - `libreoffice_load_gate`: env-guarded LibreOffice headless "opens without error"
//!   check.
//! - `similarity`: PNG comparison and LibreOffice render utilities for fidelity testing.

#![allow(dead_code)]

pub mod similarity;

use std::collections::BTreeMap;
use std::io::Cursor;

use zavora_slide_opc::OpcPackage;

/// All byte-comparable entries of a package: parts, every part's `.rels`, the
/// package `.rels`, and `[Content_Types].xml`.
///
/// Used to assert true byte-fidelity — the earlier `get_part`-only checks
/// silently skipped rels/content-types.
pub fn package_entries(pkg: &OpcPackage) -> BTreeMap<String, Vec<u8>> {
    let mut m = BTreeMap::new();
    for name in pkg.part_names() {
        m.insert(name.to_string(), pkg.get_part(name).unwrap().to_vec());
    }
    for (part, rels) in &pkg.part_rels {
        m.insert(format!("rels::{part}"), rels.to_xml().unwrap());
    }
    m.insert("rels::PACKAGE".into(), pkg.package_rels.to_xml().unwrap());
    m.insert(
        "[Content_Types].xml".into(),
        pkg.content_types.to_xml().unwrap(),
    );
    m
}

/// Reopen a saved buffer through the OPC layer.
pub fn reopen(buf: Vec<u8>) -> OpcPackage {
    OpcPackage::from_reader(Cursor::new(buf)).unwrap()
}

/// Assert that only the listed parts differ between `before` and `after`, and
/// all other entries are byte-identical.
///
/// # Panics
///
/// Panics if:
/// - An entry not in `expected_changed` differs between `before` and `after`.
/// - An entry in `expected_changed` is actually identical in both snapshots.
/// - The set of keys differs (entries were added or removed unexpectedly).
pub fn assert_only_changed(
    before: &BTreeMap<String, Vec<u8>>,
    after: &BTreeMap<String, Vec<u8>>,
    expected_changed: &[&str],
) {
    // Check no entries were added or removed.
    let before_keys: Vec<&String> = before.keys().collect();
    let after_keys: Vec<&String> = after.keys().collect();
    assert_eq!(
        before_keys,
        after_keys,
        "entry sets differ: before has {} entries, after has {}",
        before.len(),
        after.len()
    );

    // Collect actually changed entries.
    let actually_changed: Vec<&String> = before
        .keys()
        .filter(|k| before.get(*k) != after.get(*k))
        .collect();

    // Every expected_changed entry must actually differ.
    for expected in expected_changed {
        let key = expected.to_string();
        assert!(
            before.contains_key(&key),
            "expected_changed entry {expected:?} not found in before snapshot"
        );
        assert!(
            actually_changed.contains(&&key),
            "expected entry {expected:?} to change, but it is byte-identical"
        );
    }

    // No unexpected changes.
    let expected_set: std::collections::HashSet<&str> = expected_changed.iter().copied().collect();
    let unexpected: Vec<&String> = actually_changed
        .iter()
        .filter(|k| !expected_set.contains(k.as_str()))
        .copied()
        .collect();
    assert!(
        unexpected.is_empty(),
        "unexpected entries changed: {unexpected:?}"
    );
}

/// Run LibreOffice headless on a `.pptx` buffer to verify it opens without error.
///
/// This gate is **env-guarded**: it only runs when the environment variable
/// `ZAVORA_LIBREOFFICE_GATE` is set to `"1"`. When the variable is unset or set
/// to any other value, this function returns `Ok(())` immediately (a no-op).
///
/// # Errors
///
/// Returns an error string if:
/// - The temporary file cannot be written.
/// - LibreOffice exits with a non-zero status.
/// - LibreOffice cannot be found or fails to start.
pub fn libreoffice_load_gate(pptx_bytes: &[u8]) -> Result<(), String> {
    let gate = std::env::var("ZAVORA_LIBREOFFICE_GATE").unwrap_or_default();
    if gate != "1" {
        return Ok(());
    }

    // Write to a temp file.
    let tmp_dir = std::env::temp_dir();
    let tmp_path = tmp_dir.join("zavora_lo_gate_test.pptx");
    std::fs::write(&tmp_path, pptx_bytes).map_err(|e| format!("failed to write temp pptx: {e}"))?;

    let output_dir = tmp_dir.join("zavora_lo_gate_out");
    std::fs::create_dir_all(&output_dir)
        .map_err(|e| format!("failed to create output dir: {e}"))?;

    let result = std::process::Command::new("libreoffice")
        .args(["--headless", "--convert-to", "pdf", "--outdir"])
        .arg(&output_dir)
        .arg(&tmp_path)
        .output();

    // Clean up temp files regardless of outcome.
    let _ = std::fs::remove_file(&tmp_path);
    let _ = std::fs::remove_dir_all(&output_dir);

    match result {
        Ok(output) => {
            if output.status.success() {
                Ok(())
            } else {
                let stderr = String::from_utf8_lossy(&output.stderr);
                Err(format!(
                    "libreoffice exited with status {}: {}",
                    output.status, stderr
                ))
            }
        }
        Err(e) => Err(format!("failed to run libreoffice: {e}")),
    }
}
