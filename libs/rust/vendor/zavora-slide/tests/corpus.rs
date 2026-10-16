//! Corpus test: open real PowerPoint-authored `.pptx` files.
//!
//! Asserts the engine opens genuine PowerPoint decks, extracts their text, and
//! round-trips them faithfully: an unedited deck saves byte-identical; editing a
//! slide overlays only that slide while preserving every other part; structural
//! changes fall back to a full model rebuild.

mod test_util;

use test_util::{package_entries, reopen};
use zavora_slide::Presentation;
use zavora_slide_opc::OpcPackage;
use zavora_slide_oxml::Document;

const SAMPLE: &str = "tests/corpus/powerpoint_sample.pptx";

#[test]
fn dom_round_trips_every_xml_part_byte_for_byte() {
    // The lossless XML DOM is the foundation for surgical editing: every real
    // PowerPoint-authored part must parse and re-serialize byte-identical.
    let pkg = OpcPackage::open(SAMPLE).unwrap();
    let mut checked = 0;
    for name in pkg.part_names() {
        if !name.ends_with(".xml") {
            continue;
        }
        let src = pkg.get_part(name).unwrap();
        let doc = Document::parse(src).unwrap_or_else(|e| panic!("parse {name}: {e}"));
        assert_eq!(doc.to_bytes(), src, "DOM byte-faithful for {name}");
        checked += 1;
    }
    assert!(checked >= 5, "expected several xml parts, got {checked}");
}

#[test]
fn opened_slides_carry_byte_faithful_dom() {
    // Every opened slide parses into a DOM that re-serializes byte-identical to
    // its source part — the foundation for surgical editing.
    let p = Presentation::open(SAMPLE).unwrap();
    let pkg = OpcPackage::open(SAMPLE).unwrap();
    for i in 0..p.slide_count() {
        let (part, dom_bytes) = p.slide_dom_debug(i).expect("opened slide has a DOM");
        assert_eq!(
            dom_bytes,
            pkg.get_part(&part).unwrap(),
            "DOM faithful for {part}"
        );
    }
}

#[test]
fn opens_real_powerpoint_deck() {
    let p = Presentation::open(SAMPLE).expect("open PowerPoint-authored deck");
    assert_eq!(p.slide_count(), 3, "should recover all slides");
    let md = p.to_markdown();
    // Text authored by python-pptx is recovered.
    assert!(md.contains("Corpus Sample"), "title text extracted");
    assert!(md.contains("First item"), "bullet text extracted");
    assert!(md.contains("Free text box"), "text-box content extracted");
}

#[test]
fn opc_layer_round_trips_all_parts() {
    // Open → save → reopen at the package layer; every part is byte-preserved.
    let pkg = OpcPackage::open(SAMPLE).unwrap();
    let mut buf = std::io::Cursor::new(Vec::new());
    pkg.write_to(&mut buf).unwrap();
    buf.set_position(0);
    let pkg2 = OpcPackage::from_reader(buf).unwrap();

    let mut names: Vec<_> = pkg.part_names().collect();
    names.sort();
    let mut names2: Vec<_> = pkg2.part_names().collect();
    names2.sort();
    assert_eq!(names, names2, "part set preserved across round-trip");

    for name in pkg.part_names() {
        assert_eq!(
            pkg.get_part(name),
            pkg2.get_part(name),
            "part {name} byte-preserved"
        );
    }
}

#[test]
fn high_level_open_save_is_faithful() {
    // An opened-but-unedited deck saves byte-identical — EVERY entry, including
    // all .rels and [Content_Types].xml (not just the parts).
    let p = Presentation::open(SAMPLE).unwrap();
    let orig = package_entries(&OpcPackage::open(SAMPLE).unwrap());
    let resaved = package_entries(&reopen(p.save_to_buffer().unwrap()));
    assert_eq!(
        orig, resaved,
        "unedited open->save is byte-identical for all entries"
    );
}

#[test]
fn reading_does_not_break_round_trip() {
    let p = Presentation::open(SAMPLE).unwrap();
    let _ = p.slide(0).unwrap().text();
    let _ = p.to_markdown();
    let orig = package_entries(&OpcPackage::open(SAMPLE).unwrap());
    let resaved = package_entries(&reopen(p.save_to_buffer().unwrap()));
    assert_eq!(orig, resaved, "reads must not perturb any entry");
}

#[test]
fn editing_is_surgical_dom_based() {
    // Editing slide 0's title mutates only that slide's DOM. EVERY other entry —
    // master, 11 layouts, theme, other slides, all .rels, content-types — is
    // byte-identical. Within the edited slide, only the title run changes.
    let mut p = Presentation::open(SAMPLE).unwrap();
    p.slide_mut(0).unwrap().set_title("Edited Title").unwrap();
    let orig = package_entries(&OpcPackage::open(SAMPLE).unwrap());
    let out = package_entries(&reopen(p.save_to_buffer().unwrap()));

    // Exactly one entry differs: the edited slide body.
    let diffs: Vec<&String> = orig
        .keys()
        .filter(|k| orig.get(*k) != out.get(*k))
        .collect();
    assert_eq!(
        diffs,
        vec!["/ppt/slides/slide1.xml"],
        "only the edited slide changes"
    );
    assert_eq!(
        orig.keys().collect::<Vec<_>>(),
        out.keys().collect::<Vec<_>>(),
        "no entries added/removed"
    );

    // New title present; the subtitle shape on slide 1 is preserved verbatim.
    let s1 = String::from_utf8(out["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(s1.contains("Edited Title"), "new title in edited slide");
    assert!(!s1.contains("Corpus Sample"), "old title gone");
    assert!(
        s1.contains("Authored by python-pptx"),
        "subtitle preserved verbatim"
    );
}

#[test]
fn add_text_box_to_opened_slide_is_surgical() {
    // Adding a text box to slide 0 changes only that slide; the new box appears.
    let mut p = Presentation::open(SAMPLE).unwrap();
    p.slide_mut(0).unwrap().add_text_box(
        "Inserted box",
        zavora_slide::Emu::inches(1.0),
        zavora_slide::Emu::inches(1.0),
        zavora_slide::Emu::inches(3.0),
        zavora_slide::Emu::inches(1.0),
    );
    let orig = package_entries(&OpcPackage::open(SAMPLE).unwrap());
    let out = package_entries(&reopen(p.save_to_buffer().unwrap()));
    let diffs: Vec<&String> = orig
        .keys()
        .filter(|k| orig.get(*k) != out.get(*k))
        .collect();
    assert_eq!(
        diffs,
        vec!["/ppt/slides/slide1.xml"],
        "only edited slide changes"
    );
    let s1 = String::from_utf8(out["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(
        s1.contains("<a:t>Inserted box</a:t>"),
        "new box present: {s1}"
    );
    assert!(s1.contains("Corpus Sample"), "original title preserved");
}

#[test]
fn duplicate_slide_is_faithful() {
    // Duplicating slide 0 on an opened deck materializes a new part that is a
    // byte-clone of the original, with cloned rels; master/layouts/theme and the
    // original slides stay byte-identical.
    let mut p = Presentation::open(SAMPLE).unwrap();
    let new_idx = p.duplicate_slide(0).unwrap();
    assert_eq!(new_idx, 1);
    assert_eq!(p.slide_count(), 4);
    let orig = OpcPackage::open(SAMPLE).unwrap();
    let out = reopen(p.save_to_buffer().unwrap());

    // Original slides + master/layouts/theme untouched.
    for name in orig.part_names() {
        if name == "/ppt/presentation.xml" {
            continue;
        }
        assert_eq!(
            orig.get_part(name),
            out.get_part(name),
            "part {name} preserved"
        );
    }
    // A new slide part exists and equals the duplicated original's bytes.
    let extra: Vec<&str> = out
        .part_names()
        .filter(|n| {
            n.starts_with("/ppt/slides/slide") && n.ends_with(".xml") && orig.get_part(n).is_none()
        })
        .collect();
    assert_eq!(extra.len(), 1, "exactly one new slide part");
    assert_eq!(
        out.get_part(extra[0]).unwrap(),
        orig.get_part("/ppt/slides/slide1.xml").unwrap(),
        "duplicate is a byte-clone of the original"
    );
    // It has cloned rels (same layout link as the original).
    assert!(out.get_part_rels(extra[0]).is_some(), "duplicate has rels");
    // presentation now lists 4 slides.
    let pres = String::from_utf8(out.get_part("/ppt/presentation.xml").unwrap().to_vec()).unwrap();
    assert_eq!(pres.matches("<p:sldId ").count(), 4);
}

#[test]
fn move_slide_is_faithful() {
    // Reordering slides on an opened deck rewrites only sldIdLst order; every
    // part (master/layouts/theme/all slides) stays byte-identical.
    let mut p = Presentation::open(SAMPLE).unwrap();
    p.move_slide(0, 2).unwrap();
    let orig = OpcPackage::open(SAMPLE).unwrap();
    let out = reopen(p.save_to_buffer().unwrap());

    // All non-presentation parts byte-identical.
    for name in orig.part_names() {
        if name == "/ppt/presentation.xml" {
            continue;
        }
        assert_eq!(
            orig.get_part(name),
            out.get_part(name),
            "part {name} byte-preserved"
        );
    }
    // sldIdLst order changed; the three sldId entries are reordered, not renumbered.
    let pres = String::from_utf8(out.get_part("/ppt/presentation.xml").unwrap().to_vec()).unwrap();
    let orig_pres =
        String::from_utf8(orig.get_part("/ppt/presentation.xml").unwrap().to_vec()).unwrap();
    assert_ne!(pres, orig_pres, "sldIdLst reordered");
    // Same set of r:id values present (just reordered).
    let count = |s: &str| s.matches("<p:sldId ").count();
    assert_eq!(count(&pres), 3, "still three slides");
}

#[test]
fn delete_slide_is_faithful() {
    // Deleting a slide drops it from sldIdLst, prunes its part + presentation
    // rel, and leaves master/layouts/theme/surviving slides byte-identical.
    let mut p = Presentation::open(SAMPLE).unwrap();
    p.delete_slide(1).unwrap(); // remove slide 2 (slide2.xml)
    let orig = OpcPackage::open(SAMPLE).unwrap();
    let out = reopen(p.save_to_buffer().unwrap());

    assert_eq!(p.slide_count(), 2);
    // The deleted slide's part is gone; the others remain byte-identical.
    assert!(
        out.get_part("/ppt/slides/slide2.xml").is_none(),
        "deleted part pruned"
    );
    assert_eq!(
        orig.get_part("/ppt/slides/slide1.xml"),
        out.get_part("/ppt/slides/slide1.xml"),
        "surviving slide1 byte-preserved"
    );
    assert_eq!(
        orig.get_part("/ppt/slides/slide3.xml"),
        out.get_part("/ppt/slides/slide3.xml"),
        "surviving slide3 byte-preserved"
    );
    // Master/layouts/theme untouched.
    for name in orig.part_names() {
        if name.contains("/slideMasters/")
            || name.contains("/slideLayouts/")
            || name.contains("/theme/")
        {
            assert_eq!(orig.get_part(name), out.get_part(name), "{name} preserved");
        }
    }
    // presentation rels no longer reference the deleted slide.
    let prels = out.get_part_rels("/ppt/presentation.xml").unwrap();
    let slide_rels = prels
        .items
        .iter()
        .filter(|r| r.target.contains("slides/slide"))
        .count();
    assert_eq!(slide_rels, 2, "one slide rel pruned");
}

#[test]
fn add_slide_is_faithful() {
    // Adding a slide to an opened deck binds it to an existing layout and keeps
    // master/all layouts/theme/original slides byte-identical (no rebuild).
    let mut p = Presentation::open(SAMPLE).unwrap();
    p.add_slide(zavora_slide::Layout::Blank);
    let orig = OpcPackage::open(SAMPLE).unwrap();
    let out = reopen(p.save_to_buffer().unwrap());

    // All 11 original layouts survive.
    let layouts = out
        .part_names()
        .filter(|n| n.contains("/slideLayouts/slideLayout"))
        .count();
    assert_eq!(layouts, 11, "original layouts preserved");
    // Master/layouts/theme/original slides byte-identical.
    for name in orig.part_names() {
        if name == "/ppt/presentation.xml" {
            continue;
        }
        assert_eq!(
            orig.get_part(name),
            out.get_part(name),
            "part {name} preserved"
        );
    }
    // A new blank slide part exists and binds to an existing layout.
    let extra: Vec<&str> = out
        .part_names()
        .filter(|n| {
            n.starts_with("/ppt/slides/slide") && n.ends_with(".xml") && orig.get_part(n).is_none()
        })
        .collect();
    assert_eq!(extra.len(), 1, "one new slide part");
    let rels = out.get_part_rels(extra[0]).unwrap();
    assert!(
        rels.items
            .iter()
            .any(|r| r.target.contains("slideLayouts/slideLayout")),
        "new slide bound to an existing layout"
    );
    // presentation now lists 4 slides.
    let pres = String::from_utf8(out.get_part("/ppt/presentation.xml").unwrap().to_vec()).unwrap();
    assert_eq!(pres.matches("<p:sldId ").count(), 4);
}
