//! Corpus integration tests for shape operations (Part D).
//!
//! Validates that shape position/size/rotation, delete/reorder, fill/line styling,
//! and shape inventory on the real corpus deck produce surgical edits (only the
//! affected slide part changes) and round-trip cleanly.
//!
//! Requirements: 28.2, 29.1

mod test_util;

use test_util::{assert_only_changed, libreoffice_load_gate, package_entries};
use zavora_slide::Presentation;
use zavora_slide_opc::OpcPackage;
use zavora_slide_oxml::{ColorSpec, FillSpec, LineSpec, SlideDom};

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
// Shape position — surgical on corpus deck
// ===========================================================================

#[test]
fn corpus_set_shape_position_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    // Move shape 0 to a new position.
    dom.set_shape_position(0, 914400, 1828800).unwrap(); // 1in, 2in

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains(r#"x="914400""#), "x position updated");
    assert!(xml.contains(r#"y="1828800""#), "y position updated");
}

// ===========================================================================
// Shape size — surgical on corpus deck
// ===========================================================================

#[test]
fn corpus_set_shape_size_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    // Resize shape 0.
    dom.set_shape_size(0, 3657600, 2743200).unwrap(); // 4in x 3in

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains(r#"cx="3657600""#), "width updated");
    assert!(xml.contains(r#"cy="2743200""#), "height updated");
}

// ===========================================================================
// Shape rotation — surgical on corpus deck
// ===========================================================================

#[test]
fn corpus_set_shape_rotation_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    // Rotate shape 0 by 45 degrees (in 60000ths of a degree).
    dom.set_shape_rotation(0, 2700000).unwrap();

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains(r#"rot="2700000""#), "rotation set");
}

// ===========================================================================
// Shape geometry (all at once) — surgical on corpus deck
// ===========================================================================

#[test]
fn corpus_set_shape_geometry_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    // Set position, size, and rotation all at once.
    dom.set_shape_geometry(0, 457200, 914400, 5486400, 3657600, Some(5400000))
        .unwrap();

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains(r#"x="457200""#), "x set");
    assert!(xml.contains(r#"y="914400""#), "y set");
    assert!(xml.contains(r#"cx="5486400""#), "cx set");
    assert!(xml.contains(r#"cy="3657600""#), "cy set");
    assert!(xml.contains(r#"rot="5400000""#), "rot set");
}

// ===========================================================================
// Delete shape — surgical on corpus deck
// ===========================================================================

#[test]
fn corpus_delete_shape_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    let shape_count_before = dom.shape_inventory().len();
    assert!(shape_count_before > 0, "corpus deck has shapes to delete");

    dom.delete_shape(0).unwrap();

    let shape_count_after = dom.shape_inventory().len();
    assert_eq!(shape_count_after, shape_count_before - 1);

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);
}

// ===========================================================================
// Reorder shape — surgical on corpus deck
// ===========================================================================

#[test]
fn corpus_reorder_shape_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    let shapes = dom.shape_inventory();
    if shapes.len() >= 2 {
        let first_name = shapes[0].name.clone();
        let second_name = shapes[1].name.clone();

        dom.reorder_shape(0, 1).unwrap();

        let after_shapes = dom.shape_inventory();
        // After reorder, the first shape should now be at index 1.
        assert_eq!(
            after_shapes[0].name, second_name,
            "second shape moved to front"
        );
        assert_eq!(after_shapes[1].name, first_name, "first shape moved back");

        let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
        assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);
    }
}

// ===========================================================================
// Shape fill (solid, gradient, no-fill) — surgical on corpus deck
// ===========================================================================

#[test]
fn corpus_set_shape_fill_solid_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    let fill = FillSpec::Solid {
        color: ColorSpec::Rgb("4472C4".to_string()),
    };
    dom.set_shape_fill(0, &fill).unwrap();

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains("solidFill"), "solid fill emitted");
    assert!(xml.contains("4472C4"), "color value present");
}

#[test]
fn corpus_set_shape_fill_gradient_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    let fill = FillSpec::Gradient {
        stops: vec![
            (0.0, ColorSpec::Rgb("FF0000".to_string())),
            (1.0, ColorSpec::Rgb("0000FF".to_string())),
        ],
        angle_deg: 90.0,
    };
    dom.set_shape_fill(0, &fill).unwrap();

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains("gradFill"), "gradient fill emitted");
    assert!(xml.contains("FF0000"), "first stop color present");
    assert!(xml.contains("0000FF"), "second stop color present");
}

#[test]
fn corpus_set_shape_fill_no_fill_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    dom.set_shape_fill(0, &FillSpec::None).unwrap();

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains("noFill"), "noFill emitted");
}

// ===========================================================================
// Shape line — surgical on corpus deck
// ===========================================================================

#[test]
fn corpus_set_shape_line_is_surgical() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    let line = LineSpec::Styled {
        color: ColorSpec::Rgb("ED7D31".to_string()),
        width_emu: 25400, // 2pt
        dash: Some("dash".to_string()),
    };
    dom.set_shape_line(0, &line).unwrap();

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains("ED7D31"), "line color present");
    assert!(xml.contains(r#"w="25400""#), "line width set");
    assert!(xml.contains("prstDash"), "dash style emitted");
}

// ===========================================================================
// Shape inventory — returns correct data for the corpus deck
// ===========================================================================

#[test]
fn corpus_shape_inventory_returns_correct_data() {
    let (dom, _pkg) = open_slide_dom();

    let inventory = dom.shape_inventory();
    assert!(!inventory.is_empty(), "corpus deck has shapes");

    // Every shape should have an id and a name.
    for info in &inventory {
        assert!(info.id > 0, "shape has a positive id");
        assert!(!info.name.is_empty(), "shape has a name");
        assert!(
            ["sp", "pic", "graphicFrame", "cxnSp", "grpSp"].contains(&info.shape_type.as_str()),
            "shape_type is a known type: {}",
            info.shape_type
        );
    }

    // Placeholders in the corpus deck may not have explicit geometry (they
    // inherit from the layout/master). After setting geometry on a shape,
    // the inventory should reflect it.
    let (mut dom2, _) = open_slide_dom();
    dom2.set_shape_position(0, 100000, 200000).unwrap();
    dom2.set_shape_size(0, 300000, 400000).unwrap();
    let inv2 = dom2.shape_inventory();
    let geo = inv2[0]
        .geometry
        .expect("geometry present after explicit set");
    assert_eq!(geo.0, 100000, "x matches");
    assert_eq!(geo.1, 200000, "y matches");
    assert_eq!(geo.2, 300000, "cx matches");
    assert_eq!(geo.3, 400000, "cy matches");
}

// ===========================================================================
// Round-trip: edit → save → reopen → verify
// ===========================================================================

#[test]
fn corpus_shapes_round_trip() {
    let p = Presentation::open(SAMPLE).unwrap();
    let slide_count = p.slide_count();

    // Save to buffer, reopen via OPC to get a SlideDom for editing.
    let buf = p.save_to_buffer().unwrap();
    let mut pkg = OpcPackage::from_reader(std::io::Cursor::new(buf)).unwrap();
    let part = pkg.get_part("/ppt/slides/slide1.xml").unwrap().to_vec();
    let mut dom = SlideDom::parse(&part).unwrap();

    // Perform shape edits.
    dom.set_shape_position(0, 1000000, 2000000).unwrap();
    dom.set_shape_size(0, 4000000, 3000000).unwrap();
    dom.set_shape_fill(
        0,
        &FillSpec::Solid {
            color: ColorSpec::Rgb("A5D6A7".to_string()),
        },
    )
    .unwrap();

    // Write back and save.
    pkg.set_part("/ppt/slides/slide1.xml", dom.to_bytes());
    let mut out = std::io::Cursor::new(Vec::new());
    pkg.write_to(&mut out).unwrap();

    // Reopen and verify.
    let reopened = Presentation::open_from_bytes(out.get_ref()).unwrap();
    assert_eq!(reopened.slide_count(), slide_count, "slide count preserved");

    // Verify the edit persisted by re-parsing the slide DOM.
    let pkg2 = OpcPackage::from_reader(std::io::Cursor::new(out.into_inner())).unwrap();
    let part2 = pkg2.get_part("/ppt/slides/slide1.xml").unwrap();
    let dom2 = SlideDom::parse(part2).unwrap();

    let xml = String::from_utf8(dom2.to_bytes()).unwrap();
    assert!(xml.contains(r#"x="1000000""#), "position persisted");
    assert!(xml.contains(r#"cx="4000000""#), "size persisted");
    assert!(xml.contains("A5D6A7"), "fill persisted");

    // Save again to verify stability.
    let buf3 = reopened.save_to_buffer().unwrap();
    let p3 = Presentation::open_from_bytes(&buf3).unwrap();
    assert_eq!(p3.slide_count(), slide_count, "stable across saves");
}

// ===========================================================================
// LibreOffice load gate (env-guarded)
// ===========================================================================

#[test]
fn corpus_shapes_libreoffice_gate() {
    let pkg = OpcPackage::open(SAMPLE).unwrap();
    let part = pkg.get_part("/ppt/slides/slide1.xml").unwrap();
    let mut dom = SlideDom::parse(part).unwrap();

    // Apply multiple shape edits.
    dom.set_shape_position(0, 500000, 500000).unwrap();
    dom.set_shape_size(0, 6000000, 4000000).unwrap();
    dom.set_shape_rotation(0, 1800000).unwrap();
    dom.set_shape_fill(
        0,
        &FillSpec::Solid {
            color: ColorSpec::Rgb("70AD47".to_string()),
        },
    )
    .unwrap();
    dom.set_shape_line(
        0,
        &LineSpec::Styled {
            color: ColorSpec::Rgb("000000".to_string()),
            width_emu: 12700,
            dash: None,
        },
    )
    .unwrap();

    // Rebuild the package with the edited DOM.
    let mut buf = std::io::Cursor::new(Vec::new());
    pkg.write_to(&mut buf).unwrap();
    buf.set_position(0);
    let mut pkg2 = OpcPackage::from_reader(buf).unwrap();
    pkg2.set_part("/ppt/slides/slide1.xml", dom.to_bytes());

    let mut out = std::io::Cursor::new(Vec::new());
    pkg2.write_to(&mut out).unwrap();

    if let Err(e) = libreoffice_load_gate(out.get_ref()) {
        panic!("LibreOffice load gate failed: {e}");
    }
}
