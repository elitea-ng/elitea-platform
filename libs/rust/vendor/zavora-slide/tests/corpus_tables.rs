//! Corpus integration tests for table operations (Part C).
//!
//! Validates that table row/column add/remove, merge/split, sizing, and cell
//! editing on a deck with a table produce surgical edits (only the affected
//! slide part changes) and round-trip cleanly.
//!
//! Requirements: 28.2, 29.1

mod test_util;

use std::io::Cursor;
use test_util::{assert_only_changed, libreoffice_load_gate, package_entries};
use zavora_slide::{Emu, Layout, Presentation};
use zavora_slide_opc::OpcPackage;
use zavora_slide_oxml::{ColorSpec, FillSpec, SlideDom};

// ---------------------------------------------------------------------------
// Helper: create a minimal deck with a 3×3 table on slide 1, save to bytes,
// and return the OPC package + the slide part name containing the table.
// ---------------------------------------------------------------------------

const TABLE_SLIDE_PART: &str = "/ppt/slides/slide1.xml";

fn create_deck_with_table() -> Vec<u8> {
    let mut p = Presentation::new();
    let idx = p.add_slide(Layout::Blank);
    {
        let mut slide = p.slide_mut(idx).unwrap();
        let tid = slide.add_table(
            3,
            3,
            Emu::inches(1.0),
            Emu::inches(1.5),
            Emu::inches(8.0),
            Emu::inches(3.0),
        );
        // Populate cells with identifiable text.
        for r in 0..3 {
            for c in 0..3 {
                slide
                    .set_table_cell(tid, r, c, &format!("R{r}C{c}"))
                    .unwrap();
            }
        }
    }
    p.save_to_buffer().unwrap()
}

fn open_table_dom(buf: &[u8]) -> (SlideDom, OpcPackage) {
    let pkg = OpcPackage::from_reader(Cursor::new(buf.to_vec())).unwrap();
    let part = pkg.get_part(TABLE_SLIDE_PART).unwrap();
    let dom = SlideDom::parse(part).unwrap();
    (dom, pkg)
}

/// Rebuild a package with an edited slide DOM and return the new entries.
fn rebuild_with_edited_dom(
    pkg: &OpcPackage,
    slide_part: &str,
    dom: &SlideDom,
) -> std::collections::BTreeMap<String, Vec<u8>> {
    let mut buf = Cursor::new(Vec::new());
    pkg.write_to(&mut buf).unwrap();
    buf.set_position(0);
    let mut pkg2 = OpcPackage::from_reader(buf).unwrap();
    pkg2.set_part(slide_part, dom.to_bytes());
    package_entries(&pkg2)
}

/// Find the table shape index in the DOM (the graphicFrame containing a:tbl).
fn find_table_shape_idx(dom: &SlideDom) -> usize {
    let inv = dom.shape_inventory();
    inv.iter()
        .position(|s| s.shape_type == "graphicFrame")
        .expect("deck should have a graphicFrame with a table")
}

// ===========================================================================
// add_table_row — surgical
// ===========================================================================

#[test]
fn corpus_add_table_row_is_surgical() {
    let buf = create_deck_with_table();
    let (mut dom, pkg) = open_table_dom(&buf);
    let orig = package_entries(&pkg);
    let tbl_idx = find_table_shape_idx(&dom);

    dom.add_table_row(tbl_idx, 370840).unwrap();

    let after = rebuild_with_edited_dom(&pkg, TABLE_SLIDE_PART, &dom);
    assert_only_changed(&orig, &after, &[TABLE_SLIDE_PART]);
}

// ===========================================================================
// insert_table_row — surgical
// ===========================================================================

#[test]
fn corpus_insert_table_row_is_surgical() {
    let buf = create_deck_with_table();
    let (mut dom, pkg) = open_table_dom(&buf);
    let orig = package_entries(&pkg);
    let tbl_idx = find_table_shape_idx(&dom);

    dom.insert_table_row(tbl_idx, 1, 370840).unwrap();

    let after = rebuild_with_edited_dom(&pkg, TABLE_SLIDE_PART, &dom);
    assert_only_changed(&orig, &after, &[TABLE_SLIDE_PART]);
}

// ===========================================================================
// remove_table_row — surgical
// ===========================================================================

#[test]
fn corpus_remove_table_row_is_surgical() {
    let buf = create_deck_with_table();
    let (mut dom, pkg) = open_table_dom(&buf);
    let orig = package_entries(&pkg);
    let tbl_idx = find_table_shape_idx(&dom);

    dom.remove_table_row(tbl_idx, 2).unwrap();

    let after = rebuild_with_edited_dom(&pkg, TABLE_SLIDE_PART, &dom);
    assert_only_changed(&orig, &after, &[TABLE_SLIDE_PART]);
}

// ===========================================================================
// add_table_column — surgical
// ===========================================================================

#[test]
fn corpus_add_table_column_is_surgical() {
    let buf = create_deck_with_table();
    let (mut dom, pkg) = open_table_dom(&buf);
    let orig = package_entries(&pkg);
    let tbl_idx = find_table_shape_idx(&dom);

    dom.add_table_column(tbl_idx, 914400).unwrap();

    let after = rebuild_with_edited_dom(&pkg, TABLE_SLIDE_PART, &dom);
    assert_only_changed(&orig, &after, &[TABLE_SLIDE_PART]);
}

// ===========================================================================
// remove_table_column — surgical
// ===========================================================================

#[test]
fn corpus_remove_table_column_is_surgical() {
    let buf = create_deck_with_table();
    let (mut dom, pkg) = open_table_dom(&buf);
    let orig = package_entries(&pkg);
    let tbl_idx = find_table_shape_idx(&dom);

    dom.remove_table_column(tbl_idx, 0).unwrap();

    let after = rebuild_with_edited_dom(&pkg, TABLE_SLIDE_PART, &dom);
    assert_only_changed(&orig, &after, &[TABLE_SLIDE_PART]);
}

// ===========================================================================
// merge_table_cells — surgical
// ===========================================================================

#[test]
fn corpus_merge_table_cells_is_surgical() {
    let buf = create_deck_with_table();
    let (mut dom, pkg) = open_table_dom(&buf);
    let orig = package_entries(&pkg);
    let tbl_idx = find_table_shape_idx(&dom);

    // Merge a 2×2 block starting at (0,0).
    dom.merge_table_cells(tbl_idx, 0, 0, 1, 1).unwrap();

    let after = rebuild_with_edited_dom(&pkg, TABLE_SLIDE_PART, &dom);
    assert_only_changed(&orig, &after, &[TABLE_SLIDE_PART]);

    let xml = String::from_utf8(after[TABLE_SLIDE_PART].clone()).unwrap();
    assert!(xml.contains("gridSpan"), "gridSpan set on merged cell");
    assert!(xml.contains("rowSpan"), "rowSpan set on merged cell");
}

// ===========================================================================
// split_table_cell — surgical
// ===========================================================================

#[test]
fn corpus_split_table_cell_is_surgical() {
    let buf = create_deck_with_table();
    let (mut dom, pkg) = open_table_dom(&buf);
    let tbl_idx = find_table_shape_idx(&dom);

    // First merge, then split.
    dom.merge_table_cells(tbl_idx, 0, 0, 1, 1).unwrap();

    // Take snapshot after merge as our "before" for split.
    let before = rebuild_with_edited_dom(&pkg, TABLE_SLIDE_PART, &dom);

    dom.split_table_cell(tbl_idx, 0, 0).unwrap();

    let after = rebuild_with_edited_dom(&pkg, TABLE_SLIDE_PART, &dom);
    assert_only_changed(&before, &after, &[TABLE_SLIDE_PART]);

    let xml = String::from_utf8(after[TABLE_SLIDE_PART].clone()).unwrap();
    // After split, gridSpan/rowSpan/hMerge/vMerge should be removed.
    assert!(!xml.contains("gridSpan"), "gridSpan removed after split");
    assert!(!xml.contains("rowSpan"), "rowSpan removed after split");
    assert!(!xml.contains("hMerge"), "hMerge removed after split");
    assert!(!xml.contains("vMerge"), "vMerge removed after split");
}

// ===========================================================================
// set_column_width — surgical
// ===========================================================================

#[test]
fn corpus_set_column_width_is_surgical() {
    let buf = create_deck_with_table();
    let (mut dom, pkg) = open_table_dom(&buf);
    let orig = package_entries(&pkg);
    let tbl_idx = find_table_shape_idx(&dom);

    dom.set_column_width(tbl_idx, 1, 1828800).unwrap(); // 2 inches

    let after = rebuild_with_edited_dom(&pkg, TABLE_SLIDE_PART, &dom);
    assert_only_changed(&orig, &after, &[TABLE_SLIDE_PART]);

    let xml = String::from_utf8(after[TABLE_SLIDE_PART].clone()).unwrap();
    assert!(xml.contains(r#"w="1828800""#), "column width updated");
}

// ===========================================================================
// set_row_height — surgical
// ===========================================================================

#[test]
fn corpus_set_row_height_is_surgical() {
    let buf = create_deck_with_table();
    let (mut dom, pkg) = open_table_dom(&buf);
    let orig = package_entries(&pkg);
    let tbl_idx = find_table_shape_idx(&dom);

    dom.set_row_height(tbl_idx, 0, 457200).unwrap(); // 0.5 inches

    let after = rebuild_with_edited_dom(&pkg, TABLE_SLIDE_PART, &dom);
    assert_only_changed(&orig, &after, &[TABLE_SLIDE_PART]);

    let xml = String::from_utf8(after[TABLE_SLIDE_PART].clone()).unwrap();
    assert!(xml.contains(r#"h="457200""#), "row height updated");
}

// ===========================================================================
// set_cell_text — surgical
// ===========================================================================

#[test]
fn corpus_set_cell_text_is_surgical() {
    let buf = create_deck_with_table();
    let (mut dom, pkg) = open_table_dom(&buf);
    let orig = package_entries(&pkg);
    let tbl_idx = find_table_shape_idx(&dom);

    dom.set_cell_text(tbl_idx, 1, 1, "Updated Cell").unwrap();

    let after = rebuild_with_edited_dom(&pkg, TABLE_SLIDE_PART, &dom);
    assert_only_changed(&orig, &after, &[TABLE_SLIDE_PART]);

    let xml = String::from_utf8(after[TABLE_SLIDE_PART].clone()).unwrap();
    assert!(xml.contains("Updated Cell"), "cell text updated");
}

// ===========================================================================
// set_cell_alignment — surgical
// ===========================================================================

#[test]
fn corpus_set_cell_alignment_is_surgical() {
    let buf = create_deck_with_table();
    let (mut dom, pkg) = open_table_dom(&buf);
    let orig = package_entries(&pkg);
    let tbl_idx = find_table_shape_idx(&dom);

    dom.set_cell_alignment(tbl_idx, 0, 0, "ctr").unwrap();

    let after = rebuild_with_edited_dom(&pkg, TABLE_SLIDE_PART, &dom);
    assert_only_changed(&orig, &after, &[TABLE_SLIDE_PART]);

    let xml = String::from_utf8(after[TABLE_SLIDE_PART].clone()).unwrap();
    assert!(xml.contains(r#"algn="ctr""#), "cell alignment set");
}

// ===========================================================================
// set_cell_fill — surgical
// ===========================================================================

#[test]
fn corpus_set_cell_fill_is_surgical() {
    let buf = create_deck_with_table();
    let (mut dom, pkg) = open_table_dom(&buf);
    let orig = package_entries(&pkg);
    let tbl_idx = find_table_shape_idx(&dom);

    let fill = FillSpec::Solid {
        color: ColorSpec::Rgb("4472C4".to_string()),
    };
    dom.set_cell_fill(tbl_idx, 0, 0, &fill).unwrap();

    let after = rebuild_with_edited_dom(&pkg, TABLE_SLIDE_PART, &dom);
    assert_only_changed(&orig, &after, &[TABLE_SLIDE_PART]);

    let xml = String::from_utf8(after[TABLE_SLIDE_PART].clone()).unwrap();
    assert!(xml.contains("solidFill"), "solid fill emitted in cell");
    assert!(xml.contains("4472C4"), "fill color present");
}

// ===========================================================================
// set_cell_margins — surgical
// ===========================================================================

#[test]
fn corpus_set_cell_margins_is_surgical() {
    let buf = create_deck_with_table();
    let (mut dom, pkg) = open_table_dom(&buf);
    let orig = package_entries(&pkg);
    let tbl_idx = find_table_shape_idx(&dom);

    dom.set_cell_margins(tbl_idx, 0, 0, 91440, 45720, 68580, 34290)
        .unwrap();

    let after = rebuild_with_edited_dom(&pkg, TABLE_SLIDE_PART, &dom);
    assert_only_changed(&orig, &after, &[TABLE_SLIDE_PART]);

    let xml = String::from_utf8(after[TABLE_SLIDE_PART].clone()).unwrap();
    assert!(xml.contains(r#"marL="91440""#), "left margin set");
    assert!(xml.contains(r#"marT="45720""#), "top margin set");
    assert!(xml.contains(r#"marR="68580""#), "right margin set");
    assert!(xml.contains(r#"marB="34290""#), "bottom margin set");
}

// ===========================================================================
// Round-trip: create table → edit → save → reopen → verify
// ===========================================================================

#[test]
fn corpus_tables_round_trip() {
    let buf = create_deck_with_table();
    let (mut dom, pkg) = open_table_dom(&buf);
    let tbl_idx = find_table_shape_idx(&dom);

    // Perform multiple table edits.
    dom.add_table_row(tbl_idx, 370840).unwrap();
    dom.set_cell_text(tbl_idx, 0, 0, "Edited Origin").unwrap();
    dom.set_column_width(tbl_idx, 2, 2743200).unwrap();
    dom.set_row_height(tbl_idx, 1, 457200).unwrap();
    dom.set_cell_alignment(tbl_idx, 1, 1, "r").unwrap();
    dom.set_cell_fill(
        tbl_idx,
        2,
        0,
        &FillSpec::Solid {
            color: ColorSpec::Rgb("70AD47".to_string()),
        },
    )
    .unwrap();
    dom.set_cell_margins(tbl_idx, 0, 1, 50000, 50000, 25000, 25000)
        .unwrap();

    // Write back and save.
    let mut pkg2_buf = Cursor::new(Vec::new());
    pkg.write_to(&mut pkg2_buf).unwrap();
    pkg2_buf.set_position(0);
    let mut pkg2 = OpcPackage::from_reader(pkg2_buf).unwrap();
    pkg2.set_part(TABLE_SLIDE_PART, dom.to_bytes());

    let mut out = Cursor::new(Vec::new());
    pkg2.write_to(&mut out).unwrap();

    // Reopen and verify.
    let reopened = Presentation::open_from_bytes(out.get_ref()).unwrap();
    assert_eq!(reopened.slide_count(), 1, "slide count preserved");

    // Verify edits persisted by re-parsing the slide DOM.
    let pkg3 = OpcPackage::from_reader(Cursor::new(out.into_inner())).unwrap();
    let part3 = pkg3.get_part(TABLE_SLIDE_PART).unwrap();
    let dom3 = SlideDom::parse(part3).unwrap();

    let xml = String::from_utf8(dom3.to_bytes()).unwrap();
    assert!(xml.contains("Edited Origin"), "cell text persisted");
    assert!(xml.contains(r#"w="2743200""#), "column width persisted");
    assert!(xml.contains(r#"h="457200""#), "row height persisted");
    assert!(xml.contains("70AD47"), "cell fill persisted");

    // Save again to verify stability.
    let buf2 = reopened.save_to_buffer().unwrap();
    let p3 = Presentation::open_from_bytes(&buf2).unwrap();
    assert_eq!(p3.slide_count(), 1, "stable across saves");
}

// ===========================================================================
// LibreOffice load gate (env-guarded)
// ===========================================================================

#[test]
fn corpus_tables_libreoffice_gate() {
    let buf = create_deck_with_table();
    let (mut dom, pkg) = open_table_dom(&buf);
    let tbl_idx = find_table_shape_idx(&dom);

    // Apply multiple table edits.
    dom.add_table_row(tbl_idx, 370840).unwrap();
    dom.add_table_column(tbl_idx, 914400).unwrap();
    dom.merge_table_cells(tbl_idx, 0, 0, 1, 1).unwrap();
    dom.set_cell_text(tbl_idx, 2, 2, "LO Gate Cell").unwrap();
    dom.set_column_width(tbl_idx, 0, 2000000).unwrap();
    dom.set_row_height(tbl_idx, 0, 500000).unwrap();
    dom.set_cell_fill(
        tbl_idx,
        1,
        0,
        &FillSpec::Solid {
            color: ColorSpec::Rgb("ED7D31".to_string()),
        },
    )
    .unwrap();

    // Rebuild the package with the edited DOM.
    let mut pkg2_buf = Cursor::new(Vec::new());
    pkg.write_to(&mut pkg2_buf).unwrap();
    pkg2_buf.set_position(0);
    let mut pkg2 = OpcPackage::from_reader(pkg2_buf).unwrap();
    pkg2.set_part(TABLE_SLIDE_PART, dom.to_bytes());

    let mut out = Cursor::new(Vec::new());
    pkg2.write_to(&mut out).unwrap();

    if let Err(e) = libreoffice_load_gate(out.get_ref()) {
        panic!("LibreOffice load gate failed: {e}");
    }
}
