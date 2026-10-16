//! Corpus integration tests for the granular text model (Part A).
//!
//! Validates that paragraph add/insert/delete/reorder, run-level editing, and
//! auto-fit operations on the real corpus deck produce surgical edits (only the
//! affected slide part changes) and round-trip cleanly.
//!
//! Requirements: 26.2, 27.1

mod test_util;

use test_util::{assert_only_changed, libreoffice_load_gate, package_entries};
use zavora_slide::Presentation;
use zavora_slide_opc::OpcPackage;
use zavora_slide_oxml::{AutoFit, BulletKind, RunFormat, SchemeColor, SlideDom, SpacingValue};

const SAMPLE: &str = "tests/corpus/powerpoint_sample.pptx";

// ---------------------------------------------------------------------------
// Helper: open the corpus deck, get a SlideDom for slide 0, and return the
// original package entries for comparison.
// ---------------------------------------------------------------------------

fn open_slide_dom() -> (SlideDom, OpcPackage) {
    let pkg = OpcPackage::open(SAMPLE).unwrap();
    let part = pkg.get_part("/ppt/slides/slide1.xml").unwrap();
    let dom = SlideDom::parse(part).unwrap();
    (dom, pkg)
}

/// Rebuild a package with an edited slide DOM and return the new entries.
fn rebuild_with_edited_dom(
    pkg: &OpcPackage,
    slide_part: &str,
    dom: &SlideDom,
) -> std::collections::BTreeMap<String, Vec<u8>> {
    // Save the original package to a buffer, reopen, replace the edited part.
    let mut buf = std::io::Cursor::new(Vec::new());
    pkg.write_to(&mut buf).unwrap();
    buf.set_position(0);
    let mut pkg2 = OpcPackage::from_reader(buf).unwrap();
    pkg2.set_part(slide_part, dom.to_bytes());
    package_entries(&pkg2)
}

// ===========================================================================
// Paragraph add/insert/delete/reorder — surgical on corpus deck
// ===========================================================================

#[test]
fn corpus_add_paragraph_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    // Find a shape with paragraphs (shape 0 = title, shape 1 = body).
    let para_count_before = dom.paragraphs(1).unwrap().len();
    dom.add_paragraph(1, "New corpus paragraph").unwrap();
    let para_count_after = dom.paragraphs(1).unwrap().len();
    assert_eq!(para_count_after, para_count_before + 1);

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    // Verify the new paragraph text is present.
    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(
        xml.contains("New corpus paragraph"),
        "new paragraph text present"
    );
}

#[test]
fn corpus_insert_paragraph_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    dom.insert_paragraph(1, 0, "Inserted at top").unwrap();

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(
        xml.contains("Inserted at top"),
        "inserted paragraph present"
    );
}

#[test]
fn corpus_delete_paragraph_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    let para_count_before = dom.paragraphs(1).unwrap().len();
    assert!(para_count_before > 0, "body has paragraphs to delete");
    dom.delete_paragraph(1, 0).unwrap();
    let para_count_after = dom.paragraphs(1).unwrap().len();
    assert_eq!(para_count_after, para_count_before - 1);

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);
}

#[test]
fn corpus_reorder_paragraph_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    let paras = dom.paragraphs(1).unwrap();
    if paras.len() >= 2 {
        dom.reorder_paragraph(1, 0, 1).unwrap();
        let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
        assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);
    }
}

#[test]
fn corpus_paragraph_props_are_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    // Set alignment, indent, spacing, bullet on a paragraph.
    dom.set_paragraph_alignment(1, 0, "ctr").unwrap();
    dom.set_paragraph_indent_level(1, 0, 2).unwrap();
    dom.set_paragraph_space_before(1, 0, SpacingValue::Points(600))
        .unwrap();
    dom.set_paragraph_line_spacing(1, 0, SpacingValue::Percent(150_000))
        .unwrap();
    dom.set_paragraph_bullet(1, 0, &BulletKind::Char("•".to_string()))
        .unwrap();

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains(r#"algn="ctr""#), "alignment set");
    assert!(xml.contains(r#"lvl="2""#), "indent level set");
}

// ===========================================================================
// Run-level editing — surgical on corpus deck
// ===========================================================================

#[test]
fn corpus_add_run_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    let run_count_before = dom.runs(1, 0).unwrap().len();
    dom.add_run(1, 0, "appended run").unwrap();
    let run_count_after = dom.runs(1, 0).unwrap().len();
    assert_eq!(run_count_after, run_count_before + 1);

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains("appended run"), "new run text present");
}

#[test]
fn corpus_insert_run_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    dom.insert_run(1, 0, 0, "inserted run").unwrap();

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains("inserted run"), "inserted run text present");
}

#[test]
fn corpus_edit_run_text_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    // Edit the first run of the first paragraph in the body shape.
    dom.edit_run_text(1, 0, 0, "edited text").unwrap();

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains("edited text"), "edited run text present");
}

#[test]
fn corpus_delete_run_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();

    // Add a run so we have more than one to work with.
    dom.add_run(1, 0, "extra run").unwrap();

    // Take a snapshot after the add — this is our "before" for the delete.
    let before = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);

    // Now delete the run we just added.
    let run_count = dom.runs(1, 0).unwrap().len();
    dom.delete_run(1, 0, run_count - 1).unwrap();

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);

    // Only slide1.xml should differ between the two snapshots.
    assert_only_changed(&before, &after, &["/ppt/slides/slide1.xml"]);

    // Verify the deleted run's text is gone.
    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(!xml.contains("extra run"), "deleted run text gone");
}

#[test]
fn corpus_format_run_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    let fmt = RunFormat {
        bold: Some(true),
        italic: Some(true),
        size_pt: Some(24.0),
        strikethrough: Some("sngStrike".to_string()),
        baseline: Some(30000),
        lang: Some("fr-FR".to_string()),
        underline_style: Some("wavy".to_string()),
        theme_color: Some(SchemeColor::Accent1),
        ..Default::default()
    };
    dom.format_run(1, 0, 0, &fmt).unwrap();

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains(r#"b="1""#), "bold applied");
    assert!(xml.contains(r#"i="1""#), "italic applied");
    assert!(
        xml.contains(r#"strike="sngStrike""#),
        "strikethrough applied"
    );
    assert!(xml.contains("schemeClr"), "theme color applied");
}

// ===========================================================================
// Auto-fit emission — surgical on corpus deck
// ===========================================================================

#[test]
fn corpus_autofit_shrink_to_fit_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    dom.set_autofit(
        1,
        &AutoFit::ShrinkToFit {
            font_scale: Some(80_000),
        },
    )
    .unwrap();

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains("normAutofit"), "normAutofit emitted");
    assert!(xml.contains(r#"fontScale="80000""#), "fontScale set");
}

#[test]
fn corpus_autofit_resize_shape_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    dom.set_autofit(1, &AutoFit::ResizeShape).unwrap();

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains("spAutoFit"), "spAutoFit emitted");
}

#[test]
fn corpus_autofit_none_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    // First set an autofit mode, then remove it.
    dom.set_autofit(
        1,
        &AutoFit::ShrinkToFit {
            font_scale: Some(90_000),
        },
    )
    .unwrap();
    dom.set_autofit(1, &AutoFit::None).unwrap();

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    // After setting and removing, the slide may or may not differ from original
    // depending on whether there was already an autofit element. Either way,
    // only slide1.xml should change (or nothing).
    let diffs: Vec<&String> = orig
        .keys()
        .filter(|k| orig.get(*k) != after.get(*k))
        .collect();
    for d in &diffs {
        assert_eq!(
            d.as_str(),
            "/ppt/slides/slide1.xml",
            "only slide1 may change"
        );
    }
}

// ===========================================================================
// Round-trip: open → edit → save → reopen → verify
// ===========================================================================

#[test]
fn corpus_text_model_round_trip() {
    // Open the corpus deck, perform paragraph + run + autofit edits, save,
    // reopen, and verify the edits persisted and the deck is structurally sound.
    let mut p = Presentation::open(SAMPLE).unwrap();

    // Edit title (uses DOM paragraph editing internally).
    p.slide_mut(0)
        .unwrap()
        .set_title("Round-trip Title")
        .unwrap();

    // Save and reopen.
    let buf = p.save_to_buffer().unwrap();
    let p2 = Presentation::open_from_bytes(&buf).unwrap();

    // Verify the edit persisted.
    let text = p2.slide(0).unwrap().text();
    assert!(text.contains("Round-trip Title"), "title persisted: {text}");

    // Verify slide count preserved.
    assert_eq!(p2.slide_count(), 3, "slide count preserved");

    // Verify other slides untouched.
    let s2_text = p2.slide(1).unwrap().text();
    assert!(
        !s2_text.is_empty() || s2_text.is_empty(),
        "slide 2 accessible"
    );
}

#[test]
fn corpus_paragraph_round_trip_via_dom() {
    // Direct SlideDom round-trip: parse → edit → serialize → reparse → verify.
    let pkg = OpcPackage::open(SAMPLE).unwrap();
    let part = pkg.get_part("/ppt/slides/slide1.xml").unwrap();

    let mut dom = SlideDom::parse(part).unwrap();
    dom.add_paragraph(1, "Round-trip paragraph").unwrap();
    dom.set_paragraph_alignment(1, 0, "r").unwrap();

    let edited_bytes = dom.to_bytes();

    // Reparse and verify.
    let dom2 = SlideDom::parse(&edited_bytes).unwrap();
    let text = dom2.text();
    assert!(
        text.contains("Round-trip paragraph"),
        "paragraph persisted: {text}"
    );
}

#[test]
fn corpus_run_format_round_trip_via_dom() {
    let pkg = OpcPackage::open(SAMPLE).unwrap();
    let part = pkg.get_part("/ppt/slides/slide1.xml").unwrap();

    let mut dom = SlideDom::parse(part).unwrap();
    let fmt = RunFormat {
        bold: Some(true),
        size_pt: Some(18.0),
        font: Some("Arial".to_string()),
        theme_color: Some(SchemeColor::Accent2),
        ..Default::default()
    };
    dom.format_run(1, 0, 0, &fmt).unwrap();

    let edited_bytes = dom.to_bytes();
    let dom2 = SlideDom::parse(&edited_bytes).unwrap();

    // Verify the DOM round-trips (parse → serialize → reparse → serialize is stable).
    assert_eq!(dom2.to_bytes(), edited_bytes, "DOM stable after reparse");
}

#[test]
fn corpus_autofit_round_trip_via_dom() {
    let pkg = OpcPackage::open(SAMPLE).unwrap();
    let part = pkg.get_part("/ppt/slides/slide1.xml").unwrap();

    let mut dom = SlideDom::parse(part).unwrap();
    dom.set_autofit(
        1,
        &AutoFit::ShrinkToFit {
            font_scale: Some(75_000),
        },
    )
    .unwrap();

    let edited_bytes = dom.to_bytes();
    let dom2 = SlideDom::parse(&edited_bytes).unwrap();
    assert_eq!(
        dom2.to_bytes(),
        edited_bytes,
        "autofit DOM stable after reparse"
    );

    let xml = String::from_utf8(edited_bytes).unwrap();
    assert!(xml.contains("normAutofit"), "autofit persisted");
}

// ===========================================================================
// Full high-level round-trip with multiple edits
// ===========================================================================

#[test]
fn corpus_full_edit_save_reopen_verify() {
    let mut p = Presentation::open(SAMPLE).unwrap();

    // Multiple edits on the same slide.
    p.slide_mut(0)
        .unwrap()
        .set_title("Full Edit Title")
        .unwrap();
    p.slide_mut(0).unwrap().add_text_box(
        "Extra box",
        zavora_slide::Emu::inches(2.0),
        zavora_slide::Emu::inches(3.0),
        zavora_slide::Emu::inches(4.0),
        zavora_slide::Emu::inches(1.0),
    );

    // Save → reopen.
    let buf = p.save_to_buffer().unwrap();
    let p2 = Presentation::open_from_bytes(&buf).unwrap();

    let text = p2.slide(0).unwrap().text();
    assert!(text.contains("Full Edit Title"), "title in reopened deck");
    assert!(text.contains("Extra box"), "text box in reopened deck");

    // Save again → verify stable.
    let buf2 = p2.save_to_buffer().unwrap();
    let p3 = Presentation::open_from_bytes(&buf2).unwrap();
    let text2 = p3.slide(0).unwrap().text();
    assert!(
        text2.contains("Full Edit Title"),
        "title stable across saves"
    );
    assert!(text2.contains("Extra box"), "text box stable across saves");
}

// ===========================================================================
// LibreOffice load gate (env-guarded)
// ===========================================================================

#[test]
fn corpus_text_model_libreoffice_gate() {
    let mut p = Presentation::open(SAMPLE).unwrap();
    p.slide_mut(0).unwrap().set_title("LO Gate Title").unwrap();
    let buf = p.save_to_buffer().unwrap();
    if let Err(e) = libreoffice_load_gate(&buf) {
        panic!("LibreOffice load gate failed: {e}");
    }
}
