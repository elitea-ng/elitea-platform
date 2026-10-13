//! Behaviour of the typed wrappers on valid files.
#![allow(clippy::pedantic)]
mod common;
use common::*;
use elitea_office_core::{
    Bullet, CellRange, CellValue, Deck, Document, Limit, OfficeError, Rect, SlideLayout, Theme,
    Workbook,
};

#[test]
fn deck_authoring_round_trip() {
    let l = limits();
    let mut d = Deck::new(&l);
    let s0 = d.add_slide(SlideLayout::TitleContent).unwrap();
    d.set_title(s0, "Quarterly review").unwrap();
    d.add_bullets(
        s0,
        &[
            Bullet::new("Revenue up"),
            Bullet {
                text: "Costs flat".into(),
                level: 1,
                bold: true,
            },
        ],
    )
    .unwrap();
    d.set_notes(s0, "say hello").unwrap();
    let s1 = d.add_slide(SlideLayout::Blank).unwrap();
    d.add_text(s1, "free text", Rect::inches(1.0, 1.0, 4.0, 1.0))
        .unwrap();
    let t = d
        .add_table(s1, 2, 2, Rect::inches(1.0, 2.0, 4.0, 1.0))
        .unwrap();
    d.set_table_cell(s1, t, 0, 1, "hdr").unwrap();
    d.add_chart(s1, &chart(), Rect::inches(1.0, 3.0, 5.0, 3.0))
        .unwrap();
    d.add_image(s1, TINY_PNG, Rect::inches(6.0, 1.0, 1.0, 1.0))
        .unwrap();
    let s2 = d.add_slide(SlideLayout::Blank).unwrap();
    d.move_slide(s2, 0).unwrap();
    d.apply_theme(&Theme {
        accent: Some("#FF0000".into()),
        ..Theme::default()
    })
    .unwrap();
    assert_eq!(d.slide_count().unwrap(), 3);

    let bytes = d.save().unwrap();
    let again = Deck::open(&bytes, &l).unwrap();
    assert_eq!(again.slide_count().unwrap(), 3);
    let md = again.to_markdown().unwrap();
    assert!(
        md.contains("Quarterly review") && md.contains("Revenue up") && md.contains("hdr"),
        "{md}"
    );
    assert_eq!(again.notes(1).unwrap().as_deref(), Some("say hello"));
    // The saved file is a canonical archive that reopens byte-identically.
    assert_eq!(
        Deck::open(&bytes, &l).unwrap().save().unwrap().len(),
        bytes.len()
    );

    d.delete_slide(0).unwrap();
    assert_eq!(d.slide_count().unwrap(), 2);
    assert!(matches!(d.delete_slide(9), Err(OfficeError::NotFound(_))));
}

#[test]
fn deck_placeholders_and_replace() {
    // Finding (ADR-0032 P2): zavora-slide reads a deck it authored back with
    // the title and body merged, so the placeholder inventory is exercised on
    // the in-session deck. The edit still has to survive a save.
    let l = limits();
    let mut d = Deck::new(&l);
    let s0 = d.add_slide(SlideLayout::TitleContent).unwrap();
    d.set_title(s0, "{{TITLE}}").unwrap();
    d.add_bullets(s0, &[Bullet::new("a {{X}} b {{X}}")])
        .unwrap();
    let ph = d.list_placeholders().unwrap();
    assert!(
        ph.iter()
            .any(|p| p.shape.kind == "title" && p.shape.text == "{{TITLE}}"),
        "{ph:?}"
    );
    // Run-level access needs an opened slide (the library's DOM path).
    assert!(matches!(
        d.run_count(0, 0, 0),
        Err(OfficeError::Unsupported(_))
    ));
    let mut o = Deck::open(&d.save().unwrap(), &l).unwrap();
    assert_eq!(o.shapes(0).unwrap().len(), 2);
    let n = o.replace_text("{{TITLE}}", "Filled").unwrap() + o.replace_text("{{X}}", "x").unwrap();
    assert_eq!(n, 3);
    let md = Deck::open(&o.save().unwrap(), &l)
        .unwrap()
        .to_markdown()
        .unwrap();
    assert!(md.contains("Filled") && md.contains("a x b x"), "{md}");
    assert!(matches!(
        o.replace_text("", "x"),
        Err(OfficeError::InvalidInput(_))
    ));
}

#[test]
fn deck_preview_and_pdf() {
    let l = limits();
    let mut d = Deck::new(&l);
    let s = d.add_slide(SlideLayout::Title).unwrap();
    d.set_title(s, "Hello").unwrap();
    let png = d.render_preview(0).unwrap();
    assert!(png.starts_with(b"\x89PNG"));
    assert!(d.export_pdf().unwrap().starts_with(b"%PDF"));
    assert!(matches!(d.render_preview(5), Err(OfficeError::NotFound(_))));
}

#[test]
fn deck_authoring_caps() {
    let l = elitea_office_core::Limits {
        max_slides: 2,
        max_cells: 10,
        ..limits()
    };
    let mut d = Deck::new(&l);
    d.add_slide(SlideLayout::Blank).unwrap();
    d.add_slide(SlideLayout::Blank).unwrap();
    assert!(matches!(
        d.add_slide(SlideLayout::Blank),
        Err(OfficeError::LimitExceeded {
            limit: Limit::Slides,
            value: 3
        })
    ));
    assert!(matches!(
        d.add_table(0, 4, 4, Rect::inches(0.0, 0.0, 1.0, 1.0)),
        Err(OfficeError::LimitExceeded {
            limit: Limit::Cells,
            value: 16
        })
    ));
    assert!(matches!(
        d.add_image(
            0,
            &png_header(12_000, 12_000),
            Rect::inches(0.0, 0.0, 1.0, 1.0)
        ),
        Err(OfficeError::LimitExceeded {
            limit: Limit::ImagePixels,
            value: 144_000_000
        })
    ));
    assert!(matches!(
        d.add_image(0, b"not an image", Rect::inches(0.0, 0.0, 1.0, 1.0)),
        Err(OfficeError::InvalidInput(_))
    ));
    let mut bad = chart();
    bad.series[0].values.pop();
    assert!(matches!(
        d.add_chart(0, &bad, Rect::inches(0.0, 0.0, 1.0, 1.0)),
        Err(OfficeError::InvalidInput(_))
    ));
    // An opened deck over the slide cap is refused.
    let big = {
        let mut b = Deck::new(&limits());
        for _ in 0..3 {
            b.add_slide(SlideLayout::Blank).unwrap();
        }
        b.save().unwrap()
    };
    assert!(matches!(
        Deck::open(&big, &l),
        Err(OfficeError::LimitExceeded {
            limit: Limit::Slides,
            value: 3
        })
    ));
}

#[test]
fn document_basics() {
    let l = limits();
    let mut d = Document::new(&l);
    d.add_heading("Report {{NAME}}", 1).unwrap();
    d.add_paragraph("Dear {{NAME}}, hello.").unwrap();
    let t = d.add_table(2, 2).unwrap();
    d.set_table_cell(t, 1, 1, "x").unwrap();
    d.add_image(TINY_PNG, "t.png", Rect::inches(0.0, 0.0, 1.0, 1.0))
        .unwrap();
    d.add_chart(&chart(), Rect::inches(0.0, 0.0, 4.0, 3.0))
        .unwrap();
    assert_eq!(d.replace_text("{{NAME}}", "World").unwrap(), 2);
    assert_eq!(d.tables().unwrap()[0].rows[1][1], "x");
    assert_eq!(d.headings().unwrap(), vec![(1, "Report World".to_owned())]);
    let bytes = d.save().unwrap();
    let again = Document::open(&bytes, &l).unwrap();
    assert!(again.to_plain_text().unwrap().contains("Dear World"));
    assert!(again.to_markdown().unwrap().contains("Report World"));
    assert!(again.export_pdf().unwrap().starts_with(b"%PDF"));
    assert!(
        again
            .render_page_png(0, 72.0)
            .unwrap()
            .starts_with(b"\x89PNG")
    );
    assert!(matches!(
        again.render_page_png(99, 72.0),
        Err(OfficeError::NotFound(_))
    ));
    assert!(matches!(
        again.render_page_png(0, 5000.0),
        Err(OfficeError::InvalidInput(_))
    ));
    let mut stacked = chart();
    stacked.kind = elitea_office_core::ChartKind::StackedBar;
    assert!(matches!(
        d.add_chart(&stacked, Rect::inches(0.0, 0.0, 1.0, 1.0)),
        Err(OfficeError::Unsupported(_))
    ));
}

#[test]
fn workbook_basics() {
    let l = limits();
    let mut w = Workbook::new(&l);
    w.write_row(
        0,
        0,
        0,
        &[CellValue::Text("name".into()), CellValue::Text("n".into())],
    )
    .unwrap();
    w.write_row(
        0,
        1,
        0,
        &[CellValue::Text("a|b".into()), CellValue::Number(2.5)],
    )
    .unwrap();
    w.write_cell(
        0,
        2,
        1,
        &CellValue::Formula {
            formula: "=SUM(B2:B2)".into(),
            cached: None,
        },
    )
    .unwrap();
    w.write_cell(0, 3, 0, &CellValue::Bool(true)).unwrap();
    let i = w.add_sheet("Data").unwrap();
    w.write_cell(i, 0, 0, &CellValue::Number(1.0)).unwrap();
    w.rename_sheet(i, "Numbers").unwrap();
    let bytes = w.save().unwrap();
    let r = Workbook::open(&bytes, &l).unwrap();
    assert_eq!(r.sheet_names().unwrap(), ["Sheet1", "Numbers"]);
    assert_eq!(r.read_cell(0, 1, 1).unwrap(), CellValue::Number(2.5));
    assert!(matches!(
        r.read_cell(0, 2, 1).unwrap(),
        CellValue::Formula { .. }
    ));
    let md = r.sheet_to_markdown(0).unwrap();
    assert!(
        md.starts_with("| name | n |\n| --- | --- |\n| a\\|b | 2.5 |"),
        "{md}"
    );
    assert!(matches!(r.export_pdf(), Err(OfficeError::Unsupported(_))));
    assert!(matches!(
        r.render_preview(0),
        Err(OfficeError::Unsupported(_))
    ));
    assert!(matches!(
        w.write_cell(0, 1_048_576, 0, &CellValue::Empty),
        Err(OfficeError::InvalidInput(_))
    ));
    assert!(w.add_sheet("Data").is_ok());
    let _ = CellRange {
        first_row: 0,
        first_col: 0,
        last_row: 0,
        last_col: 0,
    };
}

#[test]
fn workbook_caps() {
    let l = elitea_office_core::Limits {
        max_sheets: 2,
        max_cells: 3,
        ..limits()
    };
    let mut w = Workbook::new(&l);
    w.add_sheet("B").unwrap();
    assert!(matches!(
        w.add_sheet("C"),
        Err(OfficeError::LimitExceeded {
            limit: Limit::Sheets,
            value: 3
        })
    ));
    for c in 0..3 {
        w.write_cell(0, 0, c, &CellValue::Number(1.0)).unwrap();
    }
    assert!(matches!(
        w.write_cell(0, 0, 3, &CellValue::Number(1.0)),
        Err(OfficeError::LimitExceeded {
            limit: Limit::Cells,
            value: 4
        })
    ));
}

#[test]
fn limits_are_not_reachable_from_tool_input() {
    // `Limits` is built by the host from deployment config; the wrappers take
    // no argument that carries one besides the host's own `&Limits`.
    let l = limits();
    assert_eq!(l.max_xml_depth, 256);
    assert_eq!(
        elitea_office_core::Limits::default().max_input_bytes,
        50 * 1024 * 1024
    );
}

// Files made by python-pptx, python-docx and openpyxl (committed for the
// extract tests of elitea-main); the only non-zavora producers we have.
const REAL_PPTX: &[u8] =
    include_bytes!("../../../../services/elitea-main/internal/infra/extract/testdata/deck.pptx");
const REAL_DOCX: &[u8] =
    include_bytes!("../../../../services/elitea-main/internal/infra/extract/testdata/notes.docx");
const REAL_XLSX: &[u8] =
    include_bytes!("../../../../services/elitea-main/internal/infra/extract/testdata/sheet.xlsx");

#[test]
fn real_deck_inventory_and_exact_run_replace() {
    let l = limits();
    let mut d = Deck::open(REAL_PPTX, &l).unwrap();
    assert_eq!(d.slide_count().unwrap(), 2);
    let shapes = d.shapes(0).unwrap();
    assert_eq!(shapes.len(), 2, "{shapes:?}");
    assert_eq!(
        (shapes[0].kind.as_str(), shapes[0].text.as_str()),
        ("title", "Opening slide")
    );
    assert_eq!(
        (shapes[1].kind.as_str(), shapes[1].paragraphs[0].runs.len()),
        ("body", 1)
    );
    assert_eq!(d.list_placeholders().unwrap().len(), 4);
    assert_eq!(d.replace_text("FIXTURETOKENPPTX1", "Filled").unwrap(), 1);
    d.set_run_text(1, 0, 0, 0, "Edited title").unwrap();
    let md = Deck::open(&d.save().unwrap(), &l)
        .unwrap()
        .to_markdown()
        .unwrap();
    assert!(
        md.contains("appears here") && md.contains("Filled") && md.contains("Edited title"),
        "{md}"
    );
    assert!(!md.contains("FIXTURETOKENPPTX1"));
    // Rendering goes through the sanitised copy.
    assert!(d.render_preview(0).unwrap().starts_with(b"\x89PNG"));
    assert!(d.export_pdf().unwrap().starts_with(b"%PDF"));
}

#[test]
fn real_docx_and_xlsx() {
    let l = limits();
    let mut d = Document::open(REAL_DOCX, &l).unwrap();
    assert_eq!(d.replace_text("FIXTURETOKENDOCX", "done").unwrap(), 1);
    assert!(d.to_plain_text().unwrap().contains("carries done in"));
    assert_eq!(d.tables().unwrap()[0].rows[1], ["alpha", "42"]);
    assert!(d.export_pdf().unwrap().starts_with(b"%PDF"));

    let w = Workbook::open(REAL_XLSX, &l).unwrap();
    assert_eq!(w.sheet_names().unwrap(), ["Sales", "Empty", "Totals"]);
    assert_eq!(w.read_cell(0, 1, 1).unwrap(), CellValue::Number(120.0));
    assert_eq!(w.read_cell(2, 0, 1).unwrap(), CellValue::Number(240.0));
    // openpyxl writes text as inline strings, which zavora-xlsx 0.1.2 reads as "".
    assert!(w.reads_inline_strings_as_empty());
    assert!(
        !Workbook::open(&good_xlsx(), &l)
            .unwrap()
            .reads_inline_strings_as_empty()
    );
}
