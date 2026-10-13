//! Tests for the shared test utility functions themselves.
//!
//! Validates that `package_entries`, `assert_only_changed`, and
//! `libreoffice_load_gate` behave correctly.

mod test_util;

use std::collections::BTreeMap;
use test_util::{assert_only_changed, libreoffice_load_gate, package_entries, reopen};
use zavora_slide::Presentation;
use zavora_slide_opc::OpcPackage;

const SAMPLE: &str = "tests/corpus/powerpoint_sample.pptx";

// --- package_entries tests ---

#[test]
fn package_entries_includes_parts_rels_and_content_types() {
    let pkg = OpcPackage::open(SAMPLE).unwrap();
    let entries = package_entries(&pkg);

    // Must contain actual parts (slide XML, presentation, etc.)
    assert!(
        entries.contains_key("/ppt/presentation.xml"),
        "should contain presentation part"
    );
    assert!(
        entries.contains_key("/ppt/slides/slide1.xml"),
        "should contain slide1 part"
    );

    // Must contain rels entries.
    assert!(
        entries.contains_key("rels::PACKAGE"),
        "should contain package rels"
    );
    let has_part_rels = entries
        .keys()
        .any(|k| k.starts_with("rels::") && k != "rels::PACKAGE");
    assert!(has_part_rels, "should contain at least one part-level rels");

    // Must contain content types.
    assert!(
        entries.contains_key("[Content_Types].xml"),
        "should contain content types"
    );
}

#[test]
fn package_entries_is_deterministic() {
    let pkg = OpcPackage::open(SAMPLE).unwrap();
    let entries1 = package_entries(&pkg);
    let entries2 = package_entries(&pkg);
    assert_eq!(entries1, entries2, "package_entries must be deterministic");
}

#[test]
fn package_entries_round_trip_identical() {
    // An unedited open→save→reopen produces identical entries.
    let orig = package_entries(&OpcPackage::open(SAMPLE).unwrap());
    let p = Presentation::open(SAMPLE).unwrap();
    let resaved = package_entries(&reopen(p.save_to_buffer().unwrap()));
    assert_eq!(orig, resaved);
}

// --- assert_only_changed tests ---

#[test]
fn assert_only_changed_passes_when_correct() {
    let mut before: BTreeMap<String, Vec<u8>> = BTreeMap::new();
    before.insert("a".into(), vec![1, 2, 3]);
    before.insert("b".into(), vec![4, 5, 6]);
    before.insert("c".into(), vec![7, 8, 9]);

    let mut after = before.clone();
    after.insert("b".into(), vec![10, 11, 12]); // only "b" changed

    // Should pass: only "b" changed.
    assert_only_changed(&before, &after, &["b"]);
}

#[test]
#[should_panic(expected = "unexpected entries changed")]
fn assert_only_changed_panics_on_unexpected_change() {
    let mut before: BTreeMap<String, Vec<u8>> = BTreeMap::new();
    before.insert("a".into(), vec![1, 2, 3]);
    before.insert("b".into(), vec![4, 5, 6]);

    let mut after = before.clone();
    after.insert("a".into(), vec![99]); // "a" changed
    after.insert("b".into(), vec![88]); // "b" changed

    // Should panic: we only expect "b" to change, but "a" also changed.
    assert_only_changed(&before, &after, &["b"]);
}

#[test]
#[should_panic(expected = "expected entry")]
fn assert_only_changed_panics_when_expected_did_not_change() {
    let mut before: BTreeMap<String, Vec<u8>> = BTreeMap::new();
    before.insert("a".into(), vec![1, 2, 3]);
    before.insert("b".into(), vec![4, 5, 6]);

    let after = before.clone(); // nothing changed

    // Should panic: we expect "a" to change, but it didn't.
    assert_only_changed(&before, &after, &["a"]);
}

#[test]
#[should_panic(expected = "entry sets differ")]
fn assert_only_changed_panics_on_added_entry() {
    let mut before: BTreeMap<String, Vec<u8>> = BTreeMap::new();
    before.insert("a".into(), vec![1, 2, 3]);

    let mut after = before.clone();
    after.insert("b".into(), vec![4, 5, 6]); // new entry added

    assert_only_changed(&before, &after, &[]);
}

#[test]
fn assert_only_changed_with_real_package() {
    // Editing slide 0's title should change only that slide part.
    let mut p = Presentation::open(SAMPLE).unwrap();
    p.slide_mut(0).unwrap().set_title("Changed").unwrap();
    let before = package_entries(&OpcPackage::open(SAMPLE).unwrap());
    let after = package_entries(&reopen(p.save_to_buffer().unwrap()));
    assert_only_changed(&before, &after, &["/ppt/slides/slide1.xml"]);
}

// --- libreoffice_load_gate tests ---

#[test]
fn libreoffice_gate_is_noop_when_env_unset() {
    // Ensure the env var is not set for this test.
    // SAFETY: test-only; no concurrent threads depend on this env var.
    unsafe { std::env::remove_var("ZAVORA_LIBREOFFICE_GATE") };
    let p = Presentation::open(SAMPLE).unwrap();
    let buf = p.save_to_buffer().unwrap();
    // Should return Ok immediately without running LibreOffice.
    assert!(libreoffice_load_gate(&buf).is_ok());
}

#[test]
fn libreoffice_gate_is_noop_when_env_not_one() {
    // SAFETY: test-only; no concurrent threads depend on this env var.
    unsafe { std::env::set_var("ZAVORA_LIBREOFFICE_GATE", "0") };
    let p = Presentation::open(SAMPLE).unwrap();
    let buf = p.save_to_buffer().unwrap();
    assert!(libreoffice_load_gate(&buf).is_ok());
    unsafe { std::env::remove_var("ZAVORA_LIBREOFFICE_GATE") };
}
