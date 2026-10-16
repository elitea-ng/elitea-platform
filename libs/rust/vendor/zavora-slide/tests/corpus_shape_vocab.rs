//! Corpus integration tests for Part D extended — Shape vocabulary breadth.
//!
//! Validates that autoshape presets, connectors, freeform shapes, and group
//! traversal/add produce correct XML, round-trip cleanly through save→reopen,
//! and pass the LibreOffice load gate.
//!
//! Requirements: 28.2, 29.1

mod test_util;

use test_util::{assert_only_changed, libreoffice_load_gate, package_entries};
use zavora_slide::Presentation;
use zavora_slide_opc::OpcPackage;
use zavora_slide_oxml::{ConnectorAnchor, ConnectorType, FreeformPath, SlideDom};

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

/// Save a package to bytes for round-trip testing.
fn save_pkg_to_bytes(pkg: &OpcPackage) -> Vec<u8> {
    let mut buf = std::io::Cursor::new(Vec::new());
    pkg.write_to(&mut buf).unwrap();
    buf.into_inner()
}

// ===========================================================================
// Autoshape presets — add various presets, save, reopen, verify
// ===========================================================================

#[test]
fn corpus_add_autoshape_rect_round_trip() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    let id = dom
        .add_autoshape("rect", 100000, 200000, 300000, 400000)
        .unwrap();
    assert!(id > 0, "returned a valid shape id");

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains(r#"prst="rect""#), "rect preset emitted");
    assert!(xml.contains(r#"x="100000""#), "x position correct");
    assert!(xml.contains(r#"y="200000""#), "y position correct");
    assert!(xml.contains(r#"cx="300000""#), "width correct");
    assert!(xml.contains(r#"cy="400000""#), "height correct");

    // Round-trip: save → reopen → verify shape persists.
    let bytes = save_pkg_to_bytes(&{
        let mut buf = std::io::Cursor::new(Vec::new());
        pkg.write_to(&mut buf).unwrap();
        buf.set_position(0);
        let mut pkg2 = OpcPackage::from_reader(buf).unwrap();
        pkg2.set_part("/ppt/slides/slide1.xml", dom.to_bytes());
        pkg2
    });
    let reopened = OpcPackage::from_reader(std::io::Cursor::new(bytes)).unwrap();
    let part2 = reopened.get_part("/ppt/slides/slide1.xml").unwrap();
    let dom2 = SlideDom::parse(part2).unwrap();
    let xml2 = String::from_utf8(dom2.to_bytes()).unwrap();
    assert!(
        xml2.contains(r#"prst="rect""#),
        "rect preset persists after reopen"
    );
}

#[test]
fn corpus_add_autoshape_ellipse_round_trip() {
    let (mut dom, pkg) = open_slide_dom();

    dom.add_autoshape("ellipse", 500000, 600000, 700000, 800000)
        .unwrap();

    let mut buf = std::io::Cursor::new(Vec::new());
    pkg.write_to(&mut buf).unwrap();
    buf.set_position(0);
    let mut pkg2 = OpcPackage::from_reader(buf).unwrap();
    pkg2.set_part("/ppt/slides/slide1.xml", dom.to_bytes());
    let bytes = save_pkg_to_bytes(&pkg2);

    let reopened = OpcPackage::from_reader(std::io::Cursor::new(bytes)).unwrap();
    let part2 = reopened.get_part("/ppt/slides/slide1.xml").unwrap();
    let xml = String::from_utf8(part2.to_vec()).unwrap();
    assert!(xml.contains(r#"prst="ellipse""#), "ellipse preset persists");
}

#[test]
fn corpus_add_autoshape_star5_round_trip() {
    let (mut dom, pkg) = open_slide_dom();

    dom.add_autoshape("star5", 200000, 300000, 400000, 400000)
        .unwrap();

    let mut buf = std::io::Cursor::new(Vec::new());
    pkg.write_to(&mut buf).unwrap();
    buf.set_position(0);
    let mut pkg2 = OpcPackage::from_reader(buf).unwrap();
    pkg2.set_part("/ppt/slides/slide1.xml", dom.to_bytes());
    let bytes = save_pkg_to_bytes(&pkg2);

    let reopened = OpcPackage::from_reader(std::io::Cursor::new(bytes)).unwrap();
    let part2 = reopened.get_part("/ppt/slides/slide1.xml").unwrap();
    let xml = String::from_utf8(part2.to_vec()).unwrap();
    assert!(xml.contains(r#"prst="star5""#), "star5 preset persists");
}

#[test]
fn corpus_add_autoshape_flowchart_process_round_trip() {
    let (mut dom, pkg) = open_slide_dom();

    dom.add_autoshape("flowChartProcess", 100000, 100000, 500000, 300000)
        .unwrap();

    let mut buf = std::io::Cursor::new(Vec::new());
    pkg.write_to(&mut buf).unwrap();
    buf.set_position(0);
    let mut pkg2 = OpcPackage::from_reader(buf).unwrap();
    pkg2.set_part("/ppt/slides/slide1.xml", dom.to_bytes());
    let bytes = save_pkg_to_bytes(&pkg2);

    let reopened = OpcPackage::from_reader(std::io::Cursor::new(bytes)).unwrap();
    let part2 = reopened.get_part("/ppt/slides/slide1.xml").unwrap();
    let xml = String::from_utf8(part2.to_vec()).unwrap();
    assert!(
        xml.contains(r#"prst="flowChartProcess""#),
        "flowChartProcess preset persists"
    );
}

// ===========================================================================
// Connectors — straight, elbow, curved with anchors → save → reopen → verify
// ===========================================================================

#[test]
fn corpus_add_connector_straight_round_trip() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    let id = dom
        .add_connector(
            ConnectorType::Straight,
            Some(ConnectorAnchor {
                shape_id: 2,
                connection_idx: 0,
            }),
            Some(ConnectorAnchor {
                shape_id: 3,
                connection_idx: 2,
            }),
            100000,
            200000,
            500000,
            0,
        )
        .unwrap();
    assert!(id > 0);

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(
        xml.contains(r#"prst="straightConnector1""#),
        "straight connector preset"
    );
    assert!(xml.contains("stCxn"), "start connection anchor present");
    assert!(xml.contains("endCxn"), "end connection anchor present");

    // Round-trip
    let mut buf = std::io::Cursor::new(Vec::new());
    pkg.write_to(&mut buf).unwrap();
    buf.set_position(0);
    let mut pkg2 = OpcPackage::from_reader(buf).unwrap();
    pkg2.set_part("/ppt/slides/slide1.xml", dom.to_bytes());
    let bytes = save_pkg_to_bytes(&pkg2);

    let reopened = OpcPackage::from_reader(std::io::Cursor::new(bytes)).unwrap();
    let part2 = reopened.get_part("/ppt/slides/slide1.xml").unwrap();
    let xml2 = String::from_utf8(part2.to_vec()).unwrap();
    assert!(
        xml2.contains(r#"prst="straightConnector1""#),
        "straight connector persists"
    );
    assert!(xml2.contains("stCxn"), "start anchor persists");
    assert!(xml2.contains("endCxn"), "end anchor persists");
}

#[test]
fn corpus_add_connector_elbow_round_trip() {
    let (mut dom, pkg) = open_slide_dom();

    dom.add_connector(
        ConnectorType::Elbow,
        Some(ConnectorAnchor {
            shape_id: 2,
            connection_idx: 1,
        }),
        None,
        200000,
        300000,
        400000,
        200000,
    )
    .unwrap();

    let mut buf = std::io::Cursor::new(Vec::new());
    pkg.write_to(&mut buf).unwrap();
    buf.set_position(0);
    let mut pkg2 = OpcPackage::from_reader(buf).unwrap();
    pkg2.set_part("/ppt/slides/slide1.xml", dom.to_bytes());
    let bytes = save_pkg_to_bytes(&pkg2);

    let reopened = OpcPackage::from_reader(std::io::Cursor::new(bytes)).unwrap();
    let part2 = reopened.get_part("/ppt/slides/slide1.xml").unwrap();
    let xml2 = String::from_utf8(part2.to_vec()).unwrap();
    assert!(
        xml2.contains(r#"prst="bentConnector3""#),
        "elbow connector persists"
    );
}

#[test]
fn corpus_add_connector_curved_round_trip() {
    let (mut dom, pkg) = open_slide_dom();

    dom.add_connector(
        ConnectorType::Curved,
        None,
        Some(ConnectorAnchor {
            shape_id: 3,
            connection_idx: 3,
        }),
        300000,
        400000,
        600000,
        300000,
    )
    .unwrap();

    let mut buf = std::io::Cursor::new(Vec::new());
    pkg.write_to(&mut buf).unwrap();
    buf.set_position(0);
    let mut pkg2 = OpcPackage::from_reader(buf).unwrap();
    pkg2.set_part("/ppt/slides/slide1.xml", dom.to_bytes());
    let bytes = save_pkg_to_bytes(&pkg2);

    let reopened = OpcPackage::from_reader(std::io::Cursor::new(bytes)).unwrap();
    let part2 = reopened.get_part("/ppt/slides/slide1.xml").unwrap();
    let xml2 = String::from_utf8(part2.to_vec()).unwrap();
    assert!(
        xml2.contains(r#"prst="curvedConnector3""#),
        "curved connector persists"
    );
}

// ===========================================================================
// Freeform — triangle path → save → reopen → verify
// ===========================================================================

#[test]
fn corpus_add_freeform_triangle_round_trip() {
    let (mut dom, pkg) = open_slide_dom();
    let orig = package_entries(&pkg);

    // Build a triangle path: (250,0) → (500,500) → (0,500) → close
    let mut path = FreeformPath::new(500, 500);
    path.move_to(250, 0);
    path.line_to(500, 500);
    path.line_to(0, 500);
    path.close();

    let id = dom
        .add_freeform(&path, 1000000, 1000000, 914400, 914400)
        .unwrap();
    assert!(id > 0);

    let after = rebuild_with_edited_dom(&pkg, "/ppt/slides/slide1.xml", &dom);
    assert_only_changed(&orig, &after, &["/ppt/slides/slide1.xml"]);

    let xml = String::from_utf8(after["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(xml.contains("custGeom"), "custom geometry emitted");
    assert!(xml.contains("moveTo"), "moveTo segment present");
    assert!(xml.contains("lnTo"), "lineTo segments present");
    assert!(xml.contains("close"), "close segment present");

    // Round-trip
    let mut buf = std::io::Cursor::new(Vec::new());
    pkg.write_to(&mut buf).unwrap();
    buf.set_position(0);
    let mut pkg2 = OpcPackage::from_reader(buf).unwrap();
    pkg2.set_part("/ppt/slides/slide1.xml", dom.to_bytes());
    let bytes = save_pkg_to_bytes(&pkg2);

    let reopened = OpcPackage::from_reader(std::io::Cursor::new(bytes)).unwrap();
    let part2 = reopened.get_part("/ppt/slides/slide1.xml").unwrap();
    let xml2 = String::from_utf8(part2.to_vec()).unwrap();
    assert!(
        xml2.contains("custGeom"),
        "custom geometry persists after reopen"
    );
    assert!(xml2.contains("moveTo"), "moveTo persists");
    assert!(xml2.contains("lnTo"), "lineTo persists");
    assert!(xml2.contains("close"), "close persists");
}

// ===========================================================================
// Group shapes — traversal on a deck with a group
// ===========================================================================

#[test]
fn corpus_group_shapes_traversal() {
    // Create a slide with a group shape for testing.
    let slide_xml = br#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"
       xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"
       xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
  <p:cSld><p:spTree>
    <p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>
    <p:grpSpPr/>
    <p:grpSp>
      <p:nvGrpSpPr><p:cNvPr id="10" name="Group 10"/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>
      <p:grpSpPr>
        <a:xfrm><a:off x="0" y="0"/><a:ext cx="5000000" cy="3000000"/><a:chOff x="0" y="0"/><a:chExt cx="5000000" cy="3000000"/></a:xfrm>
      </p:grpSpPr>
      <p:sp>
        <p:nvSpPr><p:cNvPr id="11" name="Rect 11"/><p:cNvSpPr/><p:nvPr/></p:nvSpPr>
        <p:spPr><a:xfrm><a:off x="100000" y="100000"/><a:ext cx="200000" cy="200000"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></p:spPr>
      </p:sp>
      <p:sp>
        <p:nvSpPr><p:cNvPr id="12" name="Ellipse 12"/><p:cNvSpPr/><p:nvPr/></p:nvSpPr>
        <p:spPr><a:xfrm><a:off x="400000" y="100000"/><a:ext cx="200000" cy="200000"/></a:xfrm><a:prstGeom prst="ellipse"><a:avLst/></a:prstGeom></p:spPr>
      </p:sp>
    </p:grpSp>
    <p:sp>
      <p:nvSpPr><p:cNvPr id="20" name="Outside 20"/><p:cNvSpPr/><p:nvPr/></p:nvSpPr>
      <p:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="100000" cy="100000"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></p:spPr>
    </p:sp>
  </p:spTree></p:cSld>
</p:sld>"#;

    let dom = SlideDom::parse(slide_xml).unwrap();

    // Shape 0 is the group (grpSp).
    let children = dom.group_shapes(0).unwrap();
    assert_eq!(children.len(), 2, "group has 2 child shapes");

    // Shape 1 is a regular sp — should error.
    let err = dom.group_shapes(1);
    assert!(err.is_err(), "non-group shape returns error");
}

// ===========================================================================
// Add shape to group — save → reopen → verify
// ===========================================================================

#[test]
fn corpus_add_shape_to_group_round_trip() {
    // Create a slide with a group shape.
    let slide_xml = br#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"
       xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"
       xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
  <p:cSld><p:spTree>
    <p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>
    <p:grpSpPr/>
    <p:grpSp>
      <p:nvGrpSpPr><p:cNvPr id="10" name="Group 10"/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>
      <p:grpSpPr>
        <a:xfrm><a:off x="0" y="0"/><a:ext cx="5000000" cy="3000000"/><a:chOff x="0" y="0"/><a:chExt cx="5000000" cy="3000000"/></a:xfrm>
      </p:grpSpPr>
      <p:sp>
        <p:nvSpPr><p:cNvPr id="11" name="Rect 11"/><p:cNvSpPr/><p:nvPr/></p:nvSpPr>
        <p:spPr><a:xfrm><a:off x="100000" y="100000"/><a:ext cx="200000" cy="200000"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></p:spPr>
      </p:sp>
    </p:grpSp>
  </p:spTree></p:cSld>
</p:sld>"#;

    let mut dom = SlideDom::parse(slide_xml).unwrap();

    // Group at index 0 initially has 1 child shape.
    let before = dom.group_shapes(0).unwrap();
    assert_eq!(before.len(), 1, "group starts with 1 child");

    // Add a new shape to the group.
    let id = dom
        .add_shape_to_group(0, "ellipse", 500000, 500000, 200000, 200000)
        .unwrap();
    assert!(id > 0, "returned a valid shape id");

    // Verify the group now has 2 children.
    let after_children = dom.group_shapes(0).unwrap();
    assert_eq!(after_children.len(), 2, "group now has 2 children");

    // Serialize → reparse → verify persistence.
    let bytes = dom.to_bytes();
    let dom2 = SlideDom::parse(&bytes).unwrap();
    let children2 = dom2.group_shapes(0).unwrap();
    assert_eq!(children2.len(), 2, "group children persist after reparse");

    let xml = String::from_utf8(bytes).unwrap();
    assert!(
        xml.contains(r#"prst="ellipse""#),
        "added ellipse preset in group"
    );
}

// ===========================================================================
// Round-trip: add multiple shape types → save → reopen → verify all persist
// ===========================================================================

#[test]
fn corpus_multiple_shape_types_round_trip() {
    let (mut dom, pkg) = open_slide_dom();

    // Add an autoshape.
    dom.add_autoshape("roundRect", 100000, 100000, 300000, 200000)
        .unwrap();

    // Add a connector.
    dom.add_connector(
        ConnectorType::Straight,
        None,
        None,
        400000,
        100000,
        200000,
        0,
    )
    .unwrap();

    // Add a freeform (triangle).
    let mut path = FreeformPath::new(400, 400);
    path.move_to(200, 0);
    path.line_to(400, 400);
    path.line_to(0, 400);
    path.close();
    dom.add_freeform(&path, 700000, 100000, 400000, 400000)
        .unwrap();

    // Save the package with all edits.
    let mut buf = std::io::Cursor::new(Vec::new());
    pkg.write_to(&mut buf).unwrap();
    buf.set_position(0);
    let mut pkg2 = OpcPackage::from_reader(buf).unwrap();
    pkg2.set_part("/ppt/slides/slide1.xml", dom.to_bytes());
    let bytes = save_pkg_to_bytes(&pkg2);

    // Reopen and verify all shapes persist.
    let reopened = OpcPackage::from_reader(std::io::Cursor::new(bytes)).unwrap();
    let part2 = reopened.get_part("/ppt/slides/slide1.xml").unwrap();
    let xml = String::from_utf8(part2.to_vec()).unwrap();

    assert!(
        xml.contains(r#"prst="roundRect""#),
        "roundRect autoshape persists"
    );
    assert!(
        xml.contains(r#"prst="straightConnector1""#),
        "straight connector persists"
    );
    assert!(
        xml.contains("custGeom"),
        "freeform custom geometry persists"
    );
    assert!(xml.contains("moveTo"), "freeform moveTo persists");
    assert!(xml.contains("lnTo"), "freeform lineTo persists");

    // Verify the deck opens cleanly via the high-level API.
    let p = Presentation::open_from_bytes(&save_pkg_to_bytes(&reopened)).unwrap();
    assert!(
        p.slide_count() > 0,
        "deck opens cleanly with all shape types"
    );
}

// ===========================================================================
// LibreOffice load gate (env-guarded) — all shape vocab features
// ===========================================================================

#[test]
fn corpus_shape_vocab_libreoffice_gate() {
    let (mut dom, pkg) = open_slide_dom();

    // Add various shape types to exercise the full vocabulary.
    dom.add_autoshape("star5", 100000, 100000, 400000, 400000)
        .unwrap();
    dom.add_autoshape("flowChartProcess", 600000, 100000, 300000, 200000)
        .unwrap();

    dom.add_connector(
        ConnectorType::Elbow,
        None,
        None,
        100000,
        600000,
        500000,
        200000,
    )
    .unwrap();

    let mut path = FreeformPath::new(300, 300);
    path.move_to(150, 0);
    path.line_to(300, 300);
    path.line_to(0, 300);
    path.close();
    dom.add_freeform(&path, 700000, 600000, 300000, 300000)
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
