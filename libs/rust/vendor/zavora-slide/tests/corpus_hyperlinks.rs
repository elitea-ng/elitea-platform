//! Corpus integration tests for Part F (Hyperlinks, metadata, notes).
//!
//! Validates that hyperlink, click-action, core-properties, notes, and
//! footer/slide-number/date operations on the real corpus deck produce surgical
//! edits (only the affected parts change) and round-trip cleanly.
//!
//! Requirements: 28.2, 29.1

mod test_util;

use test_util::{assert_only_changed, package_entries, reopen};
use zavora_slide::{CoreProperties, Layout, Presentation};
use zavora_slide_opc::OpcPackage;
use zavora_slide_oxml::{ClickAction, SlideDom};

const SAMPLE: &str = "tests/corpus/powerpoint_sample.pptx";

// ---------------------------------------------------------------------------
// Helper: open the corpus deck, get a SlideDom for slide 1, and return the
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
    let mut buf = std::io::Cursor::new(Vec::new());
    pkg.write_to(&mut buf).unwrap();
    buf.set_position(0);
    let mut pkg2 = OpcPackage::from_reader(buf).unwrap();
    pkg2.set_part(slide_part, dom.to_bytes());
    package_entries(&pkg2)
}

// ===========================================================================
// set_run_hyperlink — surgical on corpus deck (Req 11.1, 11.3)
// ===========================================================================

#[test]
fn corpus_set_run_hyperlink_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    // Set a hyperlink on the first run of the first shape's first paragraph.
    dom.set_run_hyperlink(0, 0, 0, "https://example.com", "rId20")
        .unwrap();

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains("hlinkClick"), "hyperlink element emitted");
    assert!(xml.contains("rId20"), "relationship id present");
}

// ===========================================================================
// set_shape_click_action — surgical on corpus deck (Req 11.2, 11.3)
// ===========================================================================

#[test]
fn corpus_set_shape_click_action_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    // Set an external URL click action on shape 0.
    dom.set_shape_click_action(
        0,
        &ClickAction::ExternalUrl {
            r_id: "rId21".into(),
        },
    )
    .unwrap();

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains("hlinkClick"), "click action element emitted");
    assert!(xml.contains("rId21"), "relationship id present");
}

#[test]
fn corpus_set_shape_click_action_jump_to_slide_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    // Set a jump-to-slide click action on shape 0.
    dom.set_shape_click_action(
        0,
        &ClickAction::JumpToSlide {
            r_id: "rId22".into(),
            action: "ppaction://hlinksldjump".into(),
        },
    )
    .unwrap();

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains("hlinkClick"), "click action element emitted");
    assert!(xml.contains("rId22"), "relationship id present");
    assert!(
        xml.contains("ppaction://hlinksldjump"),
        "action attribute present"
    );
}

// ===========================================================================
// Core properties — read/write round-trip on corpus deck (Req 12.1, 12.2)
// ===========================================================================

#[test]
fn corpus_core_properties_read_write_round_trip() {
    let mut p = Presentation::open(SAMPLE).unwrap();

    // Read existing properties (should not panic).
    let original_props = p.core_properties();
    let _ = original_props.title;
    let _ = original_props.author;

    // Set new properties.
    let new_props = CoreProperties {
        title: Some("Corpus Test Title".into()),
        author: Some("Corpus Test Author".into()),
        subject: Some("Corpus Subject".into()),
        keywords: Some("corpus, test, hyperlinks".into()),
        comments: Some("Integration test comment".into()),
        category: Some("Testing".into()),
        created: Some("2025-01-15T10:00:00Z".into()),
        modified: Some("2025-01-15T12:00:00Z".into()),
        last_modified_by: Some("Test Runner".into()),
    };
    p.set_core_properties(&new_props);

    // Save and reopen.
    let buf = p.save_to_buffer().unwrap();
    let reopened = Presentation::open_from_bytes(&buf).unwrap();
    let read_back = reopened.core_properties();

    assert_eq!(read_back.title.as_deref(), Some("Corpus Test Title"));
    assert_eq!(read_back.author.as_deref(), Some("Corpus Test Author"));
    assert_eq!(read_back.subject.as_deref(), Some("Corpus Subject"));
    assert_eq!(
        read_back.keywords.as_deref(),
        Some("corpus, test, hyperlinks")
    );
    assert_eq!(
        read_back.comments.as_deref(),
        Some("Integration test comment")
    );
    assert_eq!(read_back.category.as_deref(), Some("Testing"));
    assert_eq!(read_back.created.as_deref(), Some("2025-01-15T10:00:00Z"));
    assert_eq!(read_back.modified.as_deref(), Some("2025-01-15T12:00:00Z"));
    assert_eq!(read_back.last_modified_by.as_deref(), Some("Test Runner"));
}

// ===========================================================================
// Notes editing via DOM — surgical on corpus deck (Req 13.1)
// ===========================================================================

#[test]
fn corpus_notes_editing_is_surgical() {
    // Create a deck with notes, save, reopen, edit notes — only notes part changes.
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    {
        let mut s = p.slide_mut(0).unwrap();
        s.set_title("Corpus Notes Test").unwrap();
        s.set_notes("Original notes content");
    }
    let buf1 = p.save_to_buffer().unwrap();

    // Snapshot the original package entries.
    let orig_entries = package_entries(&reopen(buf1.clone()));

    // Reopen and edit notes.
    let mut p2 = Presentation::open_from_bytes(&buf1).unwrap();
    {
        let mut s = p2.slide_mut(0).unwrap();
        s.set_notes("Updated notes via DOM path");
    }
    let buf2 = p2.save_to_buffer().unwrap();
    let after_entries = package_entries(&reopen(buf2.clone()));

    // Only the notes part should change (not the slide part).
    let diffs: Vec<&String> = orig_entries
        .keys()
        .filter(|k| orig_entries.get(*k) != after_entries.get(*k))
        .collect();

    // The notes part should be the only thing that changed.
    assert!(
        diffs
            .iter()
            .all(|d| d.contains("notesSlide") || d.contains("notes")),
        "only notes-related parts should change, but got: {diffs:?}"
    );
    assert!(!diffs.is_empty(), "at least one notes part should change");

    // Verify the notes text was updated.
    let p3 = Presentation::open_from_bytes(&buf2).unwrap();
    assert_eq!(
        p3.slide(0).unwrap().notes(),
        Some("Updated notes via DOM path")
    );

    // Verify the slide title is preserved.
    let slide_text = p3.slide(0).unwrap().text();
    assert!(
        slide_text.contains("Corpus Notes Test"),
        "slide title preserved"
    );
}

// ===========================================================================
// Footer/slide-number/date placeholder editing — surgical (Req 13.2)
// ===========================================================================

#[test]
fn corpus_footer_editing_is_surgical() {
    // Use a slide DOM with furniture placeholders to test surgical editing.
    let slide_xml = br#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:cSld><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/><p:sp><p:nvSpPr><p:cNvPr id="2" name="Title 1"/><p:cNvSpPr><a:spLocks noGrp="1"/></p:cNvSpPr><p:nvPr><p:ph type="title"/></p:nvPr></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>Main Title</a:t></a:r></a:p></p:txBody></p:sp><p:sp><p:nvSpPr><p:cNvPr id="3" name="Footer Placeholder 3"/><p:cNvSpPr><a:spLocks noGrp="1"/></p:cNvSpPr><p:nvPr><p:ph type="ftr" sz="quarter" idx="10"/></p:nvPr></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>Original Footer</a:t></a:r></a:p></p:txBody></p:sp><p:sp><p:nvSpPr><p:cNvPr id="4" name="Slide Number Placeholder 4"/><p:cNvSpPr><a:spLocks noGrp="1"/></p:cNvSpPr><p:nvPr><p:ph type="sldNum" sz="quarter" idx="11"/></p:nvPr></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>1</a:t></a:r></a:p></p:txBody></p:sp><p:sp><p:nvSpPr><p:cNvPr id="5" name="Date Placeholder 5"/><p:cNvSpPr><a:spLocks noGrp="1"/></p:cNvSpPr><p:nvPr><p:ph type="dt" sz="quarter" idx="12"/></p:nvPr></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>2024-01-15</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sld>"#;

    let original = slide_xml.to_vec();
    let mut dom = SlideDom::parse(slide_xml).unwrap();

    // Verify unedited round-trip is byte-identical.
    assert_eq!(dom.to_bytes(), original);

    // Edit footer only.
    dom.set_footer_text("Updated Footer").unwrap();
    let output = String::from_utf8_lossy(&dom.to_bytes()).to_string();

    // Title is preserved.
    assert!(output.contains("Main Title"), "title preserved");
    // Slide number and date are preserved.
    assert!(
        output.contains("Slide Number Placeholder 4"),
        "slide number placeholder preserved"
    );
    assert!(
        output.contains("Date Placeholder 5"),
        "date placeholder preserved"
    );
    assert!(output.contains("2024-01-15"), "date text preserved");
    // Footer is updated.
    assert!(output.contains("Updated Footer"), "footer updated");
    assert!(!output.contains("Original Footer"), "old footer removed");

    // Edit slide number — only that placeholder changes.
    let mut dom2 = SlideDom::parse(slide_xml).unwrap();
    dom2.set_slide_number_text("99").unwrap();
    let output2 = String::from_utf8_lossy(&dom2.to_bytes()).to_string();
    assert!(output2.contains(">99<"), "slide number updated");
    assert!(
        output2.contains("Original Footer"),
        "footer preserved when editing slide number"
    );
    assert!(
        output2.contains("Main Title"),
        "title preserved when editing slide number"
    );

    // Edit date — only that placeholder changes.
    let mut dom3 = SlideDom::parse(slide_xml).unwrap();
    dom3.set_date_text("2025-06-15").unwrap();
    let output3 = String::from_utf8_lossy(&dom3.to_bytes()).to_string();
    assert!(output3.contains("2025-06-15"), "date updated");
    assert!(!output3.contains("2024-01-15"), "old date removed");
    assert!(
        output3.contains("Original Footer"),
        "footer preserved when editing date"
    );
    assert!(
        output3.contains("Main Title"),
        "title preserved when editing date"
    );
}

// ===========================================================================
// Round-trip: set hyperlinks + properties + notes → save → reopen → verify
// ===========================================================================

#[test]
fn corpus_hyperlinks_properties_notes_round_trip() {
    // Open the corpus deck, apply hyperlink + properties + notes edits, save,
    // reopen, and verify everything persists.
    let mut p = Presentation::open(SAMPLE).unwrap();
    let slide_count = p.slide_count();

    // Set core properties.
    let props = CoreProperties {
        title: Some("Round-Trip Test".into()),
        author: Some("Integration Tester".into()),
        subject: Some("Hyperlinks + Notes".into()),
        keywords: Some("round-trip, corpus".into()),
        comments: None,
        category: None,
        created: Some("2025-01-15T08:00:00Z".into()),
        modified: Some("2025-01-15T09:00:00Z".into()),
        last_modified_by: Some("Tester".into()),
    };
    p.set_core_properties(&props);

    // Save and reopen.
    let buf = p.save_to_buffer().unwrap();
    let reopened = Presentation::open_from_bytes(&buf).unwrap();

    // Verify slide count preserved.
    assert_eq!(reopened.slide_count(), slide_count, "slide count preserved");

    // Verify properties persisted.
    let read_props = reopened.core_properties();
    assert_eq!(read_props.title.as_deref(), Some("Round-Trip Test"));
    assert_eq!(read_props.author.as_deref(), Some("Integration Tester"));
    assert_eq!(read_props.subject.as_deref(), Some("Hyperlinks + Notes"));
    assert_eq!(read_props.keywords.as_deref(), Some("round-trip, corpus"));
    assert_eq!(read_props.created.as_deref(), Some("2025-01-15T08:00:00Z"));
    assert_eq!(read_props.modified.as_deref(), Some("2025-01-15T09:00:00Z"));
    assert_eq!(read_props.last_modified_by.as_deref(), Some("Tester"));

    // Verify the deck still opens cleanly (save again to confirm stability).
    let buf2 = reopened.save_to_buffer().unwrap();
    let p3 = Presentation::open_from_bytes(&buf2).unwrap();
    assert_eq!(
        p3.slide_count(),
        slide_count,
        "stable across multiple saves"
    );

    // Verify hyperlink edits at the DOM level persist through round-trip.
    let pkg = OpcPackage::from_reader(std::io::Cursor::new(buf.clone())).unwrap();
    let part = pkg.get_part("/ppt/slides/slide1.xml").unwrap().to_vec();
    let mut dom = SlideDom::parse(&part).unwrap();

    // Set a hyperlink on the first run.
    dom.set_run_hyperlink(0, 0, 0, "https://round-trip-test.com", "rId30")
        .unwrap();

    // Set a click action on shape 0.
    dom.set_shape_click_action(
        0,
        &ClickAction::ExternalUrl {
            r_id: "rId31".into(),
        },
    )
    .unwrap();

    // Write back and rebuild.
    let mut buf_out = std::io::Cursor::new(Vec::new());
    pkg.write_to(&mut buf_out).unwrap();
    buf_out.set_position(0);
    let mut pkg2 = OpcPackage::from_reader(buf_out).unwrap();
    pkg2.set_part("/ppt/slides/slide1.xml", dom.to_bytes());

    let mut final_buf = std::io::Cursor::new(Vec::new());
    pkg2.write_to(&mut final_buf).unwrap();

    // Reopen and verify the hyperlink edits persisted.
    let final_pkg = OpcPackage::from_reader(std::io::Cursor::new(final_buf.into_inner())).unwrap();
    let final_part = final_pkg.get_part("/ppt/slides/slide1.xml").unwrap();
    let final_xml = String::from_utf8(final_part.to_vec()).unwrap();

    assert!(
        final_xml.contains("hlinkClick"),
        "hyperlink persisted through round-trip"
    );
    assert!(final_xml.contains("rId30"), "run hyperlink rId persisted");
    assert!(final_xml.contains("rId31"), "click action rId persisted");
}
