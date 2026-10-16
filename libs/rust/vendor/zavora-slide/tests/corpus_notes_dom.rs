//! Integration tests for notes editing via DOM (Req 13.1) and
//! footer/slide-number/date placeholders (Req 13.2).

mod test_util;

use zavora_slide::{Layout, Presentation};
use zavora_slide_oxml::{NotesDom, SlideDom};

// ─── Notes DOM unit tests ───────────────────────────────────────────────────

/// A notes-slide XML with existing notes text (as PowerPoint would emit).
fn notes_xml_with_text(text: &str) -> Vec<u8> {
    let esc = text
        .replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;");
    format!(
        "<?xml version=\"1.0\" encoding=\"UTF-8\" standalone=\"yes\"?>\n\
         <p:notes xmlns:a=\"http://schemas.openxmlformats.org/drawingml/2006/main\" \
         xmlns:r=\"http://schemas.openxmlformats.org/officeDocument/2006/relationships\" \
         xmlns:p=\"http://schemas.openxmlformats.org/presentationml/2006/main\">\
         <p:cSld><p:spTree>\
         <p:nvGrpSpPr><p:cNvPr id=\"1\" name=\"\"/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>\
         <p:grpSpPr/>\
         <p:sp><p:nvSpPr><p:cNvPr id=\"2\" name=\"Slide Image Placeholder 1\"/>\
         <p:cNvSpPr><a:spLocks noGrp=\"1\"/></p:cNvSpPr>\
         <p:nvPr><p:ph type=\"sldImg\" idx=\"1\"/></p:nvPr></p:nvSpPr><p:spPr/></p:sp>\
         <p:sp><p:nvSpPr><p:cNvPr id=\"3\" name=\"Notes Placeholder 2\"/>\
         <p:cNvSpPr><a:spLocks noGrp=\"1\"/></p:cNvSpPr>\
         <p:nvPr><p:ph type=\"body\" idx=\"1\"/></p:nvPr></p:nvSpPr><p:spPr/>\
         <p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>{esc}</a:t></a:r></a:p></p:txBody></p:sp>\
         </p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:notes>"
    )
    .into_bytes()
}

#[test]
fn notes_dom_set_and_read_text() {
    let xml = notes_xml_with_text("Original speaker notes");
    let mut dom = NotesDom::parse(&xml).unwrap();
    assert_eq!(dom.notes_text(), "Original speaker notes");

    dom.set_notes_text("Updated notes for the audience")
        .unwrap();
    assert_eq!(dom.notes_text(), "Updated notes for the audience");
}

#[test]
fn notes_dom_multi_paragraph() {
    let xml = notes_xml_with_text("First line");
    let mut dom = NotesDom::parse(&xml).unwrap();
    dom.set_notes_text("Point one\nPoint two\nPoint three")
        .unwrap();
    assert_eq!(dom.notes_text(), "Point one\nPoint two\nPoint three");
}

#[test]
fn notes_dom_edit_is_surgical_only_notes_part_changes() {
    let xml = notes_xml_with_text("Before edit");
    let original_bytes = xml.clone();
    let mut dom = NotesDom::parse(&xml).unwrap();

    // Verify unedited round-trip is byte-identical.
    assert_eq!(dom.to_bytes(), original_bytes);

    // Edit the notes.
    dom.set_notes_text("After edit").unwrap();
    let output = dom.to_bytes();

    // The slide-image placeholder must be preserved byte-for-byte.
    let output_str = String::from_utf8_lossy(&output);
    assert!(output_str.contains("Slide Image Placeholder 1"));
    assert!(output_str.contains(r#"<p:ph type="sldImg" idx="1"/>"#));
    assert!(output_str.contains(r#"<p:cNvPr id="2" name="Slide Image Placeholder 1"/>"#));

    // The notes text is updated.
    assert!(output_str.contains("After edit"));
    assert!(!output_str.contains("Before edit"));
}

// ─── Footer / slide-number / date placeholder tests ─────────────────────────

/// A slide XML with footer, slide-number, and date placeholders (as a layout
/// would provide them).
fn slide_with_furniture_placeholders() -> Vec<u8> {
    br#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:cSld><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/><p:sp><p:nvSpPr><p:cNvPr id="2" name="Title 1"/><p:cNvSpPr><a:spLocks noGrp="1"/></p:cNvSpPr><p:nvPr><p:ph type="title"/></p:nvPr></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>Slide Title</a:t></a:r></a:p></p:txBody></p:sp><p:sp><p:nvSpPr><p:cNvPr id="3" name="Footer Placeholder 3"/><p:cNvSpPr><a:spLocks noGrp="1"/></p:cNvSpPr><p:nvPr><p:ph type="ftr" sz="quarter" idx="10"/></p:nvPr></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>Company Footer</a:t></a:r></a:p></p:txBody></p:sp><p:sp><p:nvSpPr><p:cNvPr id="4" name="Slide Number Placeholder 4"/><p:cNvSpPr><a:spLocks noGrp="1"/></p:cNvSpPr><p:nvPr><p:ph type="sldNum" sz="quarter" idx="11"/></p:nvPr></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>1</a:t></a:r></a:p></p:txBody></p:sp><p:sp><p:nvSpPr><p:cNvPr id="5" name="Date Placeholder 5"/><p:cNvSpPr><a:spLocks noGrp="1"/></p:cNvSpPr><p:nvPr><p:ph type="dt" sz="quarter" idx="12"/></p:nvPr></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>2024-01-15</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sld>"#.to_vec()
}

#[test]
fn set_footer_text_updates_placeholder() {
    let xml = slide_with_furniture_placeholders();
    let mut dom = SlideDom::parse(&xml).unwrap();
    dom.set_footer_text("New Footer Text").unwrap();
    let output = String::from_utf8_lossy(&dom.to_bytes()).to_string();
    assert!(output.contains("New Footer Text"));
    assert!(!output.contains("Company Footer"));
    // Title is preserved.
    assert!(output.contains("Slide Title"));
}

#[test]
fn set_slide_number_text_updates_placeholder() {
    let xml = slide_with_furniture_placeholders();
    let mut dom = SlideDom::parse(&xml).unwrap();
    dom.set_slide_number_text("42").unwrap();
    let output = String::from_utf8_lossy(&dom.to_bytes()).to_string();
    assert!(output.contains(">42<"));
    // Footer is preserved.
    assert!(output.contains("Company Footer"));
}

#[test]
fn set_date_text_updates_placeholder() {
    let xml = slide_with_furniture_placeholders();
    let mut dom = SlideDom::parse(&xml).unwrap();
    dom.set_date_text("2025-06-01").unwrap();
    let output = String::from_utf8_lossy(&dom.to_bytes()).to_string();
    assert!(output.contains("2025-06-01"));
    assert!(!output.contains("2024-01-15"));
    // Title is preserved.
    assert!(output.contains("Slide Title"));
}

#[test]
fn set_slide_number_visible_hides_placeholder() {
    let xml = slide_with_furniture_placeholders();
    let mut dom = SlideDom::parse(&xml).unwrap();
    dom.set_slide_number_visible(false).unwrap();
    let output = String::from_utf8_lossy(&dom.to_bytes()).to_string();
    // The cNvPr should have hidden="1".
    assert!(output.contains(r#"hidden="1""#));
    // The slide number text is still there (just hidden).
    assert!(output.contains("Slide Number Placeholder 4"));
}

#[test]
fn set_slide_number_visible_shows_placeholder() {
    let xml = slide_with_furniture_placeholders();
    let mut dom = SlideDom::parse(&xml).unwrap();
    // First hide, then show.
    dom.set_slide_number_visible(false).unwrap();
    dom.set_slide_number_visible(true).unwrap();
    let output = String::from_utf8_lossy(&dom.to_bytes()).to_string();
    // hidden attribute should be removed.
    assert!(!output.contains(r#"hidden="1""#));
}

#[test]
fn footer_error_when_no_placeholder() {
    // A slide without footer placeholder.
    let xml = br#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:cSld><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/><p:sp><p:nvSpPr><p:cNvPr id="2" name="Title 1"/><p:cNvSpPr/><p:nvPr><p:ph type="title"/></p:nvPr></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>Title</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>"#;
    let mut dom = SlideDom::parse(xml).unwrap();
    assert!(dom.set_footer_text("test").is_err());
    assert!(dom.set_slide_number_text("1").is_err());
    assert!(dom.set_date_text("today").is_err());
}

#[test]
fn furniture_edits_are_surgical_sibling_shapes_preserved() {
    let xml = slide_with_furniture_placeholders();
    let original = xml.clone();
    let mut dom = SlideDom::parse(&xml).unwrap();

    // Verify unedited round-trip.
    assert_eq!(dom.to_bytes(), original);

    // Edit footer only.
    dom.set_footer_text("Updated Footer").unwrap();
    let output = String::from_utf8_lossy(&dom.to_bytes()).to_string();

    // Title placeholder is byte-for-byte preserved (its raw bytes are unchanged).
    assert!(output.contains(r#"<p:cNvPr id="2" name="Title 1"/>"#));
    assert!(output.contains("Slide Title"));
    // Slide number and date are preserved.
    assert!(output.contains("Slide Number Placeholder 4"));
    assert!(output.contains("Date Placeholder 5"));
    assert!(output.contains("2024-01-15"));
}

// ─── High-level Slide API notes integration ─────────────────────────────────

#[test]
fn new_deck_notes_set_and_read() {
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    {
        let mut s = p.slide_mut(0).unwrap();
        s.set_title("Test Slide").unwrap();
        s.set_notes("These are my speaker notes");
    }
    let s = p.slide(0).unwrap();
    assert_eq!(s.notes(), Some("These are my speaker notes"));
}

#[test]
fn notes_round_trip_through_save_reopen() {
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    {
        let mut s = p.slide_mut(0).unwrap();
        s.set_title("Noted Slide").unwrap();
        s.set_notes("Important speaker notes");
    }

    // Save and reopen.
    let buf = p.save_to_buffer().unwrap();
    let p2 = Presentation::open_from_bytes(&buf).unwrap();
    let s = p2.slide(0).unwrap();
    assert_eq!(s.notes(), Some("Important speaker notes"));
}

#[test]
fn notes_edit_on_reopened_deck_is_surgical() {
    // Create a deck with notes, save it, reopen, edit notes, save again.
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    {
        let mut s = p.slide_mut(0).unwrap();
        s.set_title("Original Title").unwrap();
        s.set_notes("Original notes");
    }
    let buf1 = p.save_to_buffer().unwrap();

    // Reopen and edit notes.
    let mut p2 = Presentation::open_from_bytes(&buf1).unwrap();
    {
        let mut s = p2.slide_mut(0).unwrap();
        s.set_notes("Updated notes via DOM");
    }
    let buf2 = p2.save_to_buffer().unwrap();

    // Reopen and verify.
    let p3 = Presentation::open_from_bytes(&buf2).unwrap();
    let s = p3.slide(0).unwrap();
    assert_eq!(s.notes(), Some("Updated notes via DOM"));

    // The slide title should still be there (surgical — only notes part changed).
    let slide_text = p3.slide(0).unwrap().text();
    assert!(slide_text.contains("Original Title"));
}

#[test]
fn notes_edit_uses_dom_path_not_rebuild() {
    // Create a deck with notes, save, reopen.
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    {
        let mut s = p.slide_mut(0).unwrap();
        s.set_title("Title").unwrap();
        s.set_notes("Initial notes");
    }
    let buf = p.save_to_buffer().unwrap();

    // Reopen — the slide should have a notes_dom.
    let mut p2 = Presentation::open_from_bytes(&buf).unwrap();

    // Verify the notes_dom was parsed.
    let slides = p2.slides_for_test();
    assert!(
        slides[0].has_notes_dom(),
        "notes_dom should be parsed on open"
    );
    assert!(
        slides[0].has_notes_part(),
        "notes_part should be set on open"
    );

    // Edit notes — should use the DOM path.
    {
        let mut s = p2.slide_mut(0).unwrap();
        s.set_notes("Edited via DOM");
    }

    // Verify the notes_dom was updated.
    let slides = p2.slides_for_test();
    assert_eq!(
        slides[0].notes_dom_text(),
        Some("Edited via DOM".to_string())
    );
}
