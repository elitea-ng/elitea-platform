//! The two reasons `zavora-slide`, `zavora-slide-render` and
//! `zavora-slide-pdf` are vendored under `libs/rust/vendor` (ADR-0032 Risks,
//! decision 5), pinned so that a re-vendor which loses a patch fails here.
#![allow(clippy::pedantic)]
mod common;
use common::*;
use elitea_office_core::{Deck, Rect, SlideLayout};
use std::path::PathBuf;

fn vendor(name: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../vendor")
        .join(name)
}

fn rust_files(dir: &std::path::Path, out: &mut Vec<PathBuf>) {
    for entry in std::fs::read_dir(dir).unwrap().flatten() {
        let path = entry.path();
        if path.is_dir() {
            rust_files(&path, out);
        } else if path.extension().is_some_and(|e| e == "rs") {
            out.push(path);
        }
    }
}

/// Renders use the bundled font set only: no `load_system_fonts()` call in
/// any crate that draws a slide (a comment naming it is fine).
#[test]
fn no_vendored_renderer_loads_system_fonts() {
    for krate in ["zavora-slide-render", "zavora-slide-pdf", "zavora-slide"] {
        let mut files = Vec::new();
        rust_files(&vendor(krate).join("src"), &mut files);
        assert!(!files.is_empty(), "{krate} is vendored");
        for file in files {
            let text = std::fs::read_to_string(&file).unwrap();
            for (n, line) in text.lines().enumerate() {
                let code = line.split("//").next().unwrap();
                assert!(
                    !code.contains("load_system_fonts") && !code.contains("load_fonts_dir"),
                    "{}:{} loads host fonts: renders would differ between hosts",
                    file.display(),
                    n + 1
                );
            }
        }
    }
}

/// Tables and charts added to a deck opened from bytes are saved, in the same
/// session or later ones, beside everything the file already held. Upstream
/// dropped a table (the surgical save serialised the slide DOM and the table
/// lives only in the build model) and re-authored a slide that gained a chart
/// from that model, dropping the tables, charts and pictures it had read from
/// the file.
#[test]
fn tables_and_charts_added_to_opened_decks_survive_and_keep_what_was_there() {
    let l = limits();
    let reopen = |b: &[u8]| Deck::open(b, &l).unwrap();
    let mut d = Deck::new(&l);
    let s = d.add_slide(SlideLayout::Blank).unwrap();
    d.add_text(s, "keep me", Rect::inches(1.0, 0.5, 3.0, 1.0))
        .unwrap();
    let mut bytes = d.save().unwrap();

    // Session 2: a table.
    let mut o = reopen(&bytes);
    let t = o
        .add_table(0, 2, 2, Rect::inches(1.0, 1.5, 4.0, 1.0))
        .unwrap();
    o.set_table_cell(0, t, 1, 1, "cell").unwrap();
    bytes = o.save().unwrap();
    // Session 3: a chart on the slide that holds the table.
    let mut o = reopen(&bytes);
    o.add_chart(0, &chart(), Rect::inches(1.0, 3.0, 5.0, 3.0))
        .unwrap();
    bytes = o.save().unwrap();
    // Session 4: a picture, then a second table and a text box.
    let mut o = reopen(&bytes);
    o.add_image(0, TINY_PNG, Rect::inches(7.0, 0.5, 1.0, 1.0))
        .unwrap();
    o.add_text(0, "later", Rect::inches(1.0, 6.5, 3.0, 0.5))
        .unwrap();
    let t2 = o
        .add_table(0, 1, 1, Rect::inches(6.0, 3.0, 2.0, 0.5))
        .unwrap();
    o.set_table_cell(0, t2, 0, 0, "second").unwrap();
    bytes = o.save().unwrap();

    let facts = reopen(&bytes).facts().unwrap();
    let f = &facts[0];
    assert_eq!(f.tables.len(), 2, "{f:?}");
    assert_eq!(f.tables[0].cells[1][1], "cell");
    assert_eq!(f.tables[1].cells[0][0], "second");
    assert_eq!(f.charts.len(), 1, "{f:?}");
    assert_eq!(f.charts[0].series[0].1, vec![Some(1.0), Some(2.0)]);
    for text in ["keep me", "later"] {
        assert!(
            f.shapes.iter().any(|s| s.text == text),
            "{text}: {:?}",
            f.shapes
        );
    }
    assert_eq!(
        f.shapes
            .iter()
            .filter(|s| s.kind == elitea_office_core::ShapeKind::Picture)
            .count(),
        1
    );
    // Shape ids on the slide are unique (the model numbers from 2 on every
    // slide; the patch renumbers above the file's own).
    let xml = entries(&bytes)
        .into_iter()
        .find(|(n, _)| n == "ppt/slides/slide1.xml")
        .map(|(_, b)| String::from_utf8(b).unwrap())
        .unwrap();
    let mut ids: Vec<u32> = xml
        .split("<p:cNvPr id=\"")
        .skip(1)
        .map(|r| r.split('"').next().unwrap().parse().unwrap())
        .collect();
    let all = ids.len();
    ids.sort_unstable();
    ids.dedup();
    assert_eq!(ids.len(), all, "duplicate shape ids: {xml}");
    // And a render of it still works.
    assert!(
        reopen(&bytes)
            .render_preview(0)
            .unwrap()
            .starts_with(b"\x89PNG")
    );
}
