//! Package validity tests: every saved deck must carry the required parts, have
//! all relationships resolve to existing parts, and assign a content type to
//! every part. These mirror the manual checks that confirmed PowerPoint opens
//! the file without repair.

mod test_util;

use std::path::Path;

use zavora_slide::{Layout, Presentation};
use zavora_slide_opc::OpcPackage;

/// Resolve a (possibly relative) rel target against the source part's directory.
fn resolve(base_part: &str, target: &str) -> String {
    if let Some(stripped) = target.strip_prefix('/') {
        return stripped.to_string();
    }
    let dir = base_part.rsplit_once('/').map(|(d, _)| d).unwrap_or("");
    let mut segs: Vec<&str> = Vec::new();
    for s in dir.split('/').chain(target.split('/')) {
        match s {
            "" | "." => {}
            ".." => {
                segs.pop();
            }
            other => segs.push(other),
        }
    }
    segs.join("/")
}

/// Open a saved deck buffer through the OPC layer.
fn reopen_presentation(p: &Presentation) -> OpcPackage {
    let bytes = p.save_to_buffer().unwrap();
    OpcPackage::from_reader(std::io::Cursor::new(bytes)).unwrap()
}

#[test]
fn required_parts_present() {
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    let pkg = reopen_presentation(&p);

    for required in [
        "/ppt/presentation.xml",
        "/ppt/slideMasters/slideMaster1.xml",
        "/ppt/slideLayouts/slideLayout1.xml",
        "/ppt/theme/theme1.xml",
        "/ppt/presProps.xml",
        "/ppt/viewProps.xml",
        "/ppt/tableStyles.xml",
        "/docProps/core.xml",
        "/docProps/app.xml",
        "/ppt/slides/slide1.xml",
    ] {
        assert!(
            pkg.get_part(required).is_some(),
            "missing required part {required}"
        );
    }

    // Schema-required notesSz is emitted.
    let pres = String::from_utf8(pkg.get_part("/ppt/presentation.xml").unwrap().to_vec()).unwrap();
    assert!(
        pres.contains("notesSz"),
        "presentation.xml missing required notesSz"
    );
}

#[test]
fn all_relationships_resolve() {
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    p.add_slide(Layout::Blank);
    let pkg = reopen_presentation(&p);

    // Package-level rels.
    for rel in &pkg.package_rels.items {
        if rel.target_mode.as_deref() == Some("External") {
            continue;
        }
        let resolved = format!("/{}", resolve("", &rel.target));
        assert!(
            pkg.get_part(&resolved).is_some(),
            "dangling package rel -> {}",
            rel.target
        );
    }

    // Part-level rels.
    for (part, rels) in &pkg.part_rels {
        for rel in &rels.items {
            if rel.target_mode.as_deref() == Some("External") {
                continue;
            }
            let resolved = format!("/{}", resolve(part.trim_start_matches('/'), &rel.target));
            assert!(
                pkg.get_part(&resolved).is_some(),
                "dangling rel in {part}: {} -> {}",
                rel.id,
                rel.target
            );
        }
    }
}

#[test]
fn every_part_has_a_content_type() {
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    let pkg = reopen_presentation(&p);

    for name in pkg.part_names() {
        assert!(
            pkg.content_types.content_type_for(name).is_some(),
            "part {name} has no content type (override or default extension)"
        );
    }
}

#[test]
fn saves_to_disk() {
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    let dir = std::env::temp_dir();
    let path = dir.join("zavora_slide_validity.pptx");
    p.save(&path).unwrap();
    assert!(Path::new(&path).exists());
    // Reopen from disk through the OPC layer.
    let pkg = OpcPackage::open(&path).unwrap();
    assert_eq!(
        pkg.main_presentation_part().as_deref(),
        Some("/ppt/presentation.xml")
    );
    std::fs::remove_file(&path).ok();
}
