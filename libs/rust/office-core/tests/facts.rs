//! `Deck::facts`: the read-only fact sheet the pptx toolkit builds on.
#![allow(clippy::pedantic)]
mod common;
use common::*;
use elitea_office_core::{Bullet, Deck, Rect, ShapeKind, SlideLayout};

#[test]
fn facts_cover_tables_charts_geometry_and_both_index_spaces() {
    let l = limits();
    let mut d = Deck::new(&l);
    let s0 = d.add_slide(SlideLayout::TitleContent).unwrap();
    d.set_title(s0, "Quarterly review").unwrap();
    d.add_bullets(s0, &[Bullet::new("Revenue up")]).unwrap();
    let s1 = d.add_slide(SlideLayout::Blank).unwrap();
    d.add_text(s1, "free text", Rect::inches(1.0, 1.0, 4.0, 1.0))
        .unwrap();
    let t = d
        .add_table(s1, 2, 2, Rect::inches(1.0, 2.0, 4.0, 1.0))
        .unwrap();
    d.set_table_cell(s1, t, 0, 1, "hdr").unwrap();
    d.set_table_cell(s1, t, 1, 0, "a & b").unwrap();
    d.add_chart(s1, &chart(), Rect::inches(1.0, 3.0, 5.0, 3.0))
        .unwrap();
    let again = Deck::open(&d.save().unwrap(), &l).unwrap();
    let facts = again.facts().unwrap();
    assert_eq!(facts.len(), 2);
    assert!(facts[0].size.0 > 0 && facts[0].size.1 > 0);
    assert!(facts[0].shapes.iter().any(|s| s.text == "Quarterly review"));
    let text = facts[1]
        .shapes
        .iter()
        .find(|s| s.text == "free text")
        .expect("the text box");
    assert_eq!(text.kind, ShapeKind::Text);
    assert_eq!(text.text_index, Some(0));
    assert!(text.frame.is_some());
    let table = &facts[1].tables[0];
    assert_eq!(table.cells.len(), 2);
    assert_eq!(table.cells[0][1], "hdr");
    assert_eq!(table.cells[1][0], "a & b");
    let shape = &facts[1].shapes[table.shape];
    assert_eq!(shape.kind, ShapeKind::Table);
    assert!(shape.text_index.is_none());
    // The table is addressable in the drawable space it was reported in.
    let mut edit = Deck::open(&again.save().unwrap(), &l).unwrap();
    edit.set_shape_table_cell(1, table.shape, 0, 0, "edited")
        .unwrap();
    let after = Deck::open(&edit.save().unwrap(), &l)
        .unwrap()
        .facts()
        .unwrap();
    assert_eq!(after[1].tables[0].cells[0][0], "edited");
    let chart = &facts[1].charts[0];
    assert!(!chart.categories.is_empty(), "{chart:?}");
    assert!(!chart.series.is_empty(), "{chart:?}");
    assert!(chart.series[0].1.iter().all(Option::is_some));
    assert_eq!(facts[1].shapes[chart.shape].kind, ShapeKind::Chart);
}

/// A paragraph's new text keeps its line structure: `\n` becomes a break run,
/// no raw newline lands inside a run, and a break the paragraph had before
/// does not survive after the new text.
#[test]
fn paragraph_text_maps_newlines_to_break_runs_and_leaves_no_stale_break() {
    let l = limits();
    let mut d = Deck::new(&l);
    let s = d.add_slide(SlideLayout::Blank).unwrap();
    d.add_text(s, "old", Rect::inches(1.0, 1.0, 4.0, 1.0))
        .unwrap();
    let mut o = Deck::open(&d.save().unwrap(), &l).unwrap();
    // First: a paragraph with a break in it, rewritten without one.
    o.set_paragraph_text(0, 0, 0, "line one\nline two").unwrap();
    let mut o = Deck::open(&o.save().unwrap(), &l).unwrap();
    let p = &o.shapes(0).unwrap()[0].paragraphs[0];
    let kinds: Vec<(bool, &str)> = p
        .runs
        .iter()
        .map(|r| (r.line_break, r.text.as_str()))
        .collect();
    assert_eq!(
        kinds,
        [(false, "line one"), (true, ""), (false, "line two")]
    );
    assert_eq!(p.text, "line one\nline two");
    assert!(p.runs.iter().all(|r| !r.text.contains('\n')));
    // Then: rewritten to one line; the break and the second run are gone.
    o.set_paragraph_text(0, 0, 0, "single").unwrap();
    let o = Deck::open(&o.save().unwrap(), &l).unwrap();
    let p = &o.shapes(0).unwrap()[0].paragraphs[0];
    assert_eq!(p.runs.len(), 1, "{p:?}");
    assert_eq!(p.text, "single");
    // CRLF counts as one break; a bad address is NotFound.
    let mut o = o;
    o.set_paragraph_text(0, 0, 0, "a\r\nb").unwrap();
    assert_eq!(o.shapes(0).unwrap()[0].paragraphs[0].text, "a\nb");
    assert!(o.set_paragraph_text(0, 0, 9, "x").is_err());
}

#[test]
fn a_cell_keeps_one_paragraph_per_line() {
    let l = limits();
    let mut d = Deck::new(&l);
    let s = d.add_slide(SlideLayout::Blank).unwrap();
    d.add_table(s, 1, 1, Rect::inches(1.0, 1.0, 4.0, 1.0))
        .unwrap();
    let mut o = Deck::open(&d.save().unwrap(), &l).unwrap();
    let table = o.facts().unwrap()[0].tables[0].shape;
    let lines = ["first".to_owned(), String::new(), "third".to_owned()];
    o.set_cell_paragraphs(0, table, 0, 0, &lines).unwrap();
    let again = Deck::open(&o.save().unwrap(), &l).unwrap();
    assert_eq!(
        again.facts().unwrap()[0].tables[0].cells[0][0],
        "first\n\nthird"
    );
}

/// `facts_of_package` reads a file without opening it in the slide library
/// or serialising anything, and reports the speaker notes.
#[test]
fn facts_of_package_is_the_cheap_read_and_carries_notes() {
    let l = limits();
    let mut d = Deck::new(&l);
    let s = d.add_slide(SlideLayout::Blank).unwrap();
    d.add_text(s, "body", Rect::inches(1.0, 1.0, 4.0, 1.0))
        .unwrap();
    d.set_notes(s, "say hello").unwrap();
    let bytes = d.save().unwrap();
    elitea_office_core::testing::reset_counters();
    let facts = elitea_office_core::facts_of_package(&bytes, &l).unwrap();
    assert_eq!(
        elitea_office_core::testing::counters(),
        (0, 0, 1),
        "no open, no save"
    );
    assert_eq!(facts[0].notes, "say hello");
    assert!(facts[0].shapes.iter().any(|s| s.text == "body"));
    // The same answer as the dear path.
    let dear = Deck::open(&bytes, &l).unwrap().facts().unwrap();
    assert_eq!(dear, facts);
    assert!(elitea_office_core::facts_of_package(b"nope", &l).is_err());
}
