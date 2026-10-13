//! ADR-0032 P4: the docx and xlsx additions to `office-core`: line breaks as
//! break runs, one-pass replacement, the package fact sheets, batch writes,
//! chart ranges, and the counters a tool's "one parse, one save" rests on.
#![allow(clippy::pedantic, clippy::unwrap_used, clippy::expect_used)]

mod common;

use common::*;
use elitea_office_core::testing::{counters, reset_counters};
use elitea_office_core::{
    CellValue, ChartKind, DocBlock, Document, Limit, OfficeError, SheetChartSpec, SheetSeries,
    Workbook, XlsxQuery, facts_of_docx_package, facts_of_xlsx_package, parse_range,
};

fn pairs(list: &[(&str, &str)]) -> Vec<(String, String)> {
    list.iter()
        .map(|(a, b)| ((*a).to_owned(), (*b).to_owned()))
        .collect()
}

#[test]
fn docx_line_breaks_are_break_runs_not_raw_newlines() {
    let l = limits();
    let mut d = Document::new(&l);
    d.add_paragraph("one\ntwo").unwrap();
    d.add_heading("head\nline", 2).unwrap();
    let t = d.add_table(1, 1).unwrap();
    d.set_table_cell(t, 0, 0, "cell a\ncell b").unwrap();
    let saved = d.save().unwrap();
    // The file has w:br and no raw newline inside a w:t.
    let xml = part(&saved, "word/document.xml");
    assert!(xml.contains("<w:br/>"), "{xml}");
    for run in xml.split("<w:t>").skip(1).chain(xml.split("<w:t ").skip(1)) {
        let text = run.split("</w:t>").next().unwrap();
        assert!(!text.contains('\n'), "raw newline in {text}");
        assert!(!text.contains('\u{E002}'), "marker left in {text}");
    }
    let facts = facts_of_docx_package(&saved, &l).unwrap();
    let paragraphs = facts.paragraphs();
    assert_eq!(paragraphs[0].text, "one\ntwo");
    assert_eq!(paragraphs[1].text, "head\nline");
    assert_eq!(paragraphs[1].style.as_deref(), Some("Heading2"));
    assert_eq!(facts.tables()[0].rows[0][0], "cell a\ncell b");
}

#[test]
fn docx_text_with_a_reserved_character_is_refused() {
    let l = limits();
    let mut d = Document::new(&l);
    assert!(matches!(
        d.add_paragraph("a\u{E002}b"),
        Err(OfficeError::InvalidInput(_))
    ));
    d.add_paragraph("fine").unwrap();
    assert!(matches!(
        d.replace_all(&pairs(&[("x", "\u{E100}")])),
        Err(OfficeError::InvalidInput(_))
    ));
}

#[test]
fn docx_set_paragraph_text_with_a_break() {
    let l = limits();
    let mut d = Document::new(&l);
    d.add_paragraph("first").unwrap();
    d.add_paragraph("second").unwrap();
    d.set_paragraph_text(1, "x\ny").unwrap();
    let saved = d.save().unwrap();
    let facts = facts_of_docx_package(&saved, &l).unwrap();
    let p = facts.paragraphs();
    assert_eq!(p[0].text, "first");
    assert_eq!(p[1].text, "x\ny");
}

#[test]
fn docx_replace_all_is_one_pass_leftmost_longest() {
    let l = limits();
    let mut d = Document::new(&l);
    d.add_paragraph("a {{x}} and {{xy}} and {{y}}").unwrap();
    d.add_paragraph("{{x}}{{y}}").unwrap();
    // Replacements that contain other keys, and a key that contains another.
    let counts = d
        .replace_all(&pairs(&[
            ("{{x}}", "{{y}}"),
            ("{{y}}", "[Y]"),
            ("{{xy}}", "XY"),
        ]))
        .unwrap();
    assert_eq!(counts, vec![2, 2, 1]);
    let facts = facts_of_docx_package(&d.save().unwrap(), &l).unwrap();
    let p = facts.paragraphs();
    // `{{x}}` became the text `{{y}}`, which was NOT replaced again.
    assert_eq!(p[0].text, "a {{y}} and XY and [Y]");
    assert_eq!(p[1].text, "{{y}}[Y]");
}

#[test]
fn docx_replace_value_containing_its_own_key_terminates() {
    let l = limits();
    let mut d = Document::new(&l);
    d.add_paragraph("a").unwrap();
    // The library's own replace_text loops forever on this.
    let n = d.replace_text("a", "aa").unwrap();
    assert_eq!(n, 1);
    let facts = facts_of_docx_package(&d.save().unwrap(), &l).unwrap();
    assert_eq!(facts.paragraphs()[0].text, "aa");
}

#[test]
fn docx_replace_with_a_newline_makes_a_break_and_counts_across_runs() {
    let l = limits();
    let mut d = Document::new(&l);
    d.add_paragraph("Dear {{name}},").unwrap();
    let n = d.replace_text("{{name}}", "Ada\nLovelace").unwrap();
    assert_eq!(n, 1);
    let saved = d.save().unwrap();
    assert!(part(&saved, "word/document.xml").contains("<w:br/>"));
    let facts = facts_of_docx_package(&saved, &l).unwrap();
    assert_eq!(facts.paragraphs()[0].text, "Dear Ada\nLovelace,");
}

#[test]
fn docx_replace_refuses_overlapping_and_repeated_keys_before_editing() {
    let l = limits();
    let mut d = Document::new(&l);
    d.add_paragraph("abcd").unwrap();
    for bad in [
        pairs(&[("abc", "1"), ("bcd", "2")]),
        pairs(&[("a", "1"), ("a", "2")]),
        pairs(&[("", "1")]),
    ] {
        assert!(matches!(
            d.replace_all(&bad),
            Err(OfficeError::InvalidInput(_))
        ));
    }
    // Containment is fine.
    d.replace_all(&pairs(&[("abcd", "1"), ("bc", "2")]))
        .unwrap();
    let facts = facts_of_docx_package(&d.save().unwrap(), &l).unwrap();
    assert_eq!(facts.paragraphs()[0].text, "1");
    let many: Vec<(String, String)> = (0..101).map(|i| (format!("k{i}"), "v".into())).collect();
    assert!(matches!(
        d.replace_all(&many),
        Err(OfficeError::InvalidInput(_))
    ));
}

#[test]
fn docx_replace_reaches_headers_footers_and_cells() {
    let l = limits();
    let mut d = Document::new(&l);
    d.add_paragraph("body {{k}}").unwrap();
    let t = d.add_table(1, 1).unwrap();
    d.set_table_cell(t, 0, 0, "cell {{k}}").unwrap();
    let counts = d.replace_all(&pairs(&[("{{k}}", "V")])).unwrap();
    assert_eq!(counts, vec![2]);
}

#[test]
fn docx_facts_see_tables_pictures_charts_and_part_kinds() {
    let l = limits();
    let mut d = Document::new(&l);
    d.add_heading("Title", 1).unwrap();
    d.add_paragraph("text").unwrap();
    let t = d.add_table(2, 2).unwrap();
    d.set_table_cell(t, 1, 1, "x").unwrap();
    d.add_image(
        TINY_PNG,
        elitea_office_core::Rect::inches(0.0, 0.0, 1.0, 1.0),
    )
    .unwrap();
    d.add_chart(
        &chart_spec(),
        elitea_office_core::Rect::inches(0.0, 0.0, 4.0, 3.0),
    )
    .unwrap();
    let facts = facts_of_docx_package(&d.save().unwrap(), &l).unwrap();
    assert_eq!(facts.tables().len(), 1);
    assert_eq!(facts.tables()[0].rows, vec![vec!["", ""], vec!["", "x"]]);
    assert_eq!(facts.pictures, 1);
    assert_eq!(facts.charts, 1);
    assert!(facts.parts.keys().any(|k| k.starts_with("word/media")));
    assert!(facts.parts.keys().any(|k| k.starts_with("word/charts")));
    assert!(matches!(facts.blocks[0], DocBlock::Paragraph(_)));
}

fn chart_spec() -> elitea_office_core::ChartSpec {
    elitea_office_core::ChartSpec {
        kind: ChartKind::ClusteredColumn,
        title: Some("T".into()),
        categories: vec!["a".into(), "b".into()],
        series: vec![elitea_office_core::Series {
            name: "s".into(),
            values: vec![1.0, 2.0],
        }],
        legend_position: None,
        data_labels: false,
    }
}

#[test]
fn docx_counters_one_open_one_save_for_an_edit() {
    let l = limits();
    let bytes = good_docx();
    reset_counters();
    let mut d = Document::open(&bytes, &l).unwrap();
    d.add_paragraph("more").unwrap();
    d.set_paragraph_text(0, "changed").unwrap();
    d.replace_all(&pairs(&[("more", "MORE")])).unwrap();
    let saved = d.save().unwrap();
    assert_eq!(counters().0, 1, "opens");
    assert_eq!(counters().1, 1, "saves");
    reset_counters();
    let _ = facts_of_docx_package(&saved, &l).unwrap();
    assert_eq!(counters(), (0, 0, 1), "a fact read is neither");
}

// -- xlsx -------------------------------------------------------------------

#[test]
fn xlsx_write_cells_counts_distinct_cells_and_is_all_or_nothing() {
    let l = elitea_office_core::Limits {
        max_cells: 3,
        ..limits()
    };
    let mut w = Workbook::new(&l);
    // The same cell twice counts once; the last value wins.
    let grown = w
        .write_cells(
            0,
            &[
                (0, 0, CellValue::Number(1.0)),
                (0, 0, CellValue::Number(2.0)),
                (0, 1, CellValue::Text("t".into())),
            ],
        )
        .unwrap();
    assert_eq!(grown, 2);
    assert_eq!(w.read_cell(0, 0, 0).unwrap(), CellValue::Number(2.0));
    // Overwriting costs nothing; one new cell fits, two do not.
    let before = w.save().unwrap();
    let over = w.write_cells(
        0,
        &[
            (0, 0, CellValue::Number(9.0)),
            (5, 5, CellValue::Number(1.0)),
            (6, 6, CellValue::Number(1.0)),
        ],
    );
    assert!(matches!(
        over,
        Err(OfficeError::LimitExceeded {
            limit: Limit::Cells,
            ..
        })
    ));
    // Nothing of that batch was applied, not even the overwrite.
    assert_eq!(w.read_cell(0, 0, 0).unwrap(), CellValue::Number(2.0));
    assert_eq!(w.read_cell(0, 5, 5).unwrap(), CellValue::Empty);
    drop(before);
    // A bad value anywhere refuses the whole batch.
    for bad in [
        CellValue::Error("#N/A".into()),
        CellValue::Number(f64::NAN),
        CellValue::Formula {
            formula: "  ".into(),
            cached: None,
        },
        CellValue::Text("x".repeat(40_000)),
    ] {
        let r = w.write_cells(0, &[(1, 1, CellValue::Number(1.0)), (2, 2, bad)]);
        assert!(r.is_err());
        assert_eq!(w.read_cell(0, 1, 1).unwrap(), CellValue::Empty);
    }
    assert!(
        w.write_cells(0, &[(2_000_000, 0, CellValue::Empty)])
            .is_err()
    );
}

#[test]
fn xlsx_clear_range_clears_only_values_and_is_bounded() {
    let l = limits();
    let mut w = Workbook::new(&l);
    w.write_cells(
        0,
        &[
            (0, 0, CellValue::Number(1.0)),
            (1, 1, CellValue::Number(2.0)),
            (9, 9, CellValue::Number(3.0)),
        ],
    )
    .unwrap();
    let n = w.clear_range(0, parse_range("A1:C3").unwrap()).unwrap();
    assert_eq!(n, 2);
    assert_eq!(w.read_cell(0, 9, 9).unwrap(), CellValue::Number(3.0));
    // A huge declared range over a sparse sheet is clipped to the used range.
    let n = w
        .clear_range(0, parse_range("A1:XFD1048576").unwrap())
        .unwrap();
    assert_eq!(n, 1);
    let saved = w.save().unwrap();
    let facts = facts_of_xlsx_package(
        &saved,
        &l,
        &XlsxQuery {
            cells: &[(0, 0, 0), (0, 9, 9)],
            ranges: &[],
            sheet_mention: None,
        },
    )
    .unwrap();
    assert!(facts.cells.is_empty());
}

#[test]
fn xlsx_package_facts_read_values_formulas_and_strings() {
    let l = limits();
    let mut w = Workbook::new(&l);
    w.write_cells(
        0,
        &[
            (0, 0, CellValue::Text("hello & <b>".into())),
            (0, 1, CellValue::Number(12.5)),
            (0, 2, CellValue::Bool(true)),
            (
                1,
                0,
                CellValue::Formula {
                    formula: "SUM(B1:B1)".into(),
                    cached: None,
                },
            ),
        ],
    )
    .unwrap();
    let saved = w.save().unwrap();
    let want = [
        (0usize, 0u32, 0u16),
        (0, 0, 1),
        (0, 0, 2),
        (0, 1, 0),
        (0, 5, 5),
    ];
    let facts = facts_of_xlsx_package(
        &saved,
        &l,
        &XlsxQuery {
            cells: &want,
            ranges: &[],
            sheet_mention: None,
        },
    )
    .unwrap();
    assert_eq!(
        facts.cells.get(&(0, 0, 0)),
        Some(&CellValue::Text("hello & <b>".into()))
    );
    assert_eq!(facts.cells.get(&(0, 0, 1)), Some(&CellValue::Number(12.5)));
    assert_eq!(facts.cells.get(&(0, 0, 2)), Some(&CellValue::Bool(true)));
    match facts.cells.get(&(0, 1, 0)) {
        Some(CellValue::Formula { formula, .. }) => assert_eq!(formula, "SUM(B1:B1)"),
        other => panic!("{other:?}"),
    }
    assert_eq!(facts.cells.get(&(0, 5, 5)), None);
    assert_eq!(facts.sheets[0].name, "Sheet1");
}

#[test]
fn xlsx_chart_is_in_the_file_on_the_target_sheet() {
    let l = limits();
    let mut w = Workbook::new(&l);
    w.write_cells(
        0,
        &[
            (0, 0, CellValue::Text("a".into())),
            (1, 0, CellValue::Text("b".into())),
            (0, 1, CellValue::Number(1.0)),
            (1, 1, CellValue::Number(2.0)),
        ],
    )
    .unwrap();
    let other = w.add_sheet("My Sheet").unwrap();
    w.add_chart(
        0,
        &SheetChartSpec {
            kind: ChartKind::ClusteredColumn,
            title: Some("Sales".into()),
            categories: Some("A1:A2".into()),
            series: vec![SheetSeries {
                name: Some("S".into()),
                values: "B1:B2".into(),
            }],
            legend: true,
            data_labels: false,
            anchor_row: 3,
            anchor_col: 3,
            width_px: 480,
            height_px: 288,
        },
    )
    .unwrap();
    let saved = w.save().unwrap();
    let facts = facts_of_xlsx_package(&saved, &l, &XlsxQuery::default()).unwrap();
    assert_eq!(facts.sheets[0].charts.len(), 1, "{:?}", facts.sheets);
    assert!(facts.sheets[other].charts.is_empty());
    let chart = &facts.sheets[0].charts[0];
    assert_eq!(chart.kind, "column");
    assert_eq!(chart.title.as_deref(), Some("Sales"));
    assert!(
        chart.refs.iter().any(|r| r == "'Sheet1'!$B$1:$B$2"),
        "{:?}",
        chart.refs
    );
    assert!(
        chart.refs.iter().any(|r| r == "'Sheet1'!$A$1:$A$2"),
        "{:?}",
        chart.refs
    );

    // An opened workbook keeps its chart through an edit and a save.
    let mut again = Workbook::open(&saved, &l).unwrap();
    again
        .write_cells(0, &[(0, 5, CellValue::Number(1.0))])
        .unwrap();
    let resaved = again.save().unwrap();
    let facts = facts_of_xlsx_package(&resaved, &l, &XlsxQuery::default()).unwrap();
    assert_eq!(facts.sheets[0].charts.len(), 1);
}

#[test]
fn xlsx_chart_ranges_are_validated() {
    let l = limits();
    let mut w = Workbook::new(&l);
    let base = SheetChartSpec {
        kind: ChartKind::Line,
        title: None,
        categories: None,
        series: vec![SheetSeries {
            name: None,
            values: "B1:B2".into(),
        }],
        legend: false,
        data_labels: false,
        anchor_row: 0,
        anchor_col: 0,
        width_px: 480,
        height_px: 288,
    };
    for bad in [
        SheetChartSpec {
            series: vec![SheetSeries {
                name: None,
                values: "not a range".into(),
            }],
            ..base.clone()
        },
        SheetChartSpec {
            series: vec![SheetSeries {
                name: None,
                values: "Nope!B1:B2".into(),
            }],
            ..base.clone()
        },
        SheetChartSpec {
            series: vec![],
            ..base.clone()
        },
        SheetChartSpec {
            kind: ChartKind::StackedBar,
            ..base.clone()
        },
    ] {
        assert!(w.add_chart(0, &bad).is_err());
    }
    assert!(w.add_chart(5, &base).is_err());
    w.add_chart(0, &base).unwrap();
}

#[test]
fn xlsx_sheet_mentions_are_found_in_formulas_and_charts() {
    let l = limits();
    let mut w = Workbook::new(&l);
    w.add_sheet("Data").unwrap();
    w.write_cells(
        0,
        &[(
            0,
            0,
            CellValue::Formula {
                formula: "SUM(Data!A1:A3)".into(),
                cached: None,
            },
        )],
    )
    .unwrap();
    let saved = w.save().unwrap();
    for (name, want) in [("Data", true), ("Other", false)] {
        let facts = facts_of_xlsx_package(
            &saved,
            &l,
            &XlsxQuery {
                cells: &[],
                ranges: &[],
                sheet_mention: Some(name),
            },
        )
        .unwrap();
        assert_eq!(facts.sheet_mentioned, want, "{name}");
    }
}

#[test]
fn xlsx_counters_one_open_one_save_for_an_edit() {
    let l = limits();
    let bytes = good_xlsx();
    reset_counters();
    let mut w = Workbook::open(&bytes, &l).unwrap();
    w.write_cells(0, &[(9, 9, CellValue::Number(1.0))]).unwrap();
    w.clear_range(0, parse_range("A1:B1").unwrap()).unwrap();
    let saved = w.save().unwrap();
    assert_eq!(counters().0, 1);
    assert_eq!(counters().1, 1);
    reset_counters();
    let _ = facts_of_xlsx_package(&saved, &l, &XlsxQuery::default()).unwrap();
    assert_eq!(counters(), (0, 0, 1));
}

fn part(zip_bytes: &[u8], name: &str) -> String {
    use std::io::Read;
    let mut z = zip::ZipArchive::new(std::io::Cursor::new(zip_bytes)).unwrap();
    let mut s = String::new();
    z.by_name(name).unwrap().read_to_string(&mut s).unwrap();
    s
}

// -- the fact readers on awkward files ----------------------------------------

const W: &str = "http://schemas.openxmlformats.org/wordprocessingml/2006/main";

fn docx_with_body(body: &str) -> Vec<u8> {
    let xml = format!(
        "<?xml version=\"1.0\" encoding=\"UTF-8\" standalone=\"yes\"?><w:document xmlns:w=\"{W}\" xmlns:mc=\"http://schemas.openxmlformats.org/markup-compatibility/2006\" xmlns:c=\"http://schemas.openxmlformats.org/drawingml/2006/chart\"><w:body>{body}</w:body></w:document>"
    );
    with_entry(&good_docx(), MAIN_DOCX, xml.into_bytes())
}

#[test]
fn docx_facts_nested_tables_text_boxes_entities_and_empty_elements() {
    let l = limits();
    let bytes = docx_with_body(
        "<w:p><w:pPr><w:pStyle w:val=\"Heading3\"/></w:pPr><w:r><w:t xml:space=\"preserve\">a &amp; b &lt;c&gt; &#65;</w:t></w:r><w:r><w:tab/><w:t>x</w:t><w:br/><w:t>y</w:t></w:r></w:p>\
         <w:p/>\
         <w:p><w:r><w:t>box</w:t></w:r><w:r><w:txbxContent><w:p><w:r><w:t>INSIDE</w:t></w:r></w:p></w:txbxContent></w:r><mc:AlternateContent><mc:Choice><w:r><w:t>kept</w:t></w:r></mc:Choice><mc:Fallback><w:r><w:t>COPY</w:t></w:r></mc:Fallback></mc:AlternateContent></w:p>\
         <w:tbl><w:tr><w:tc><w:p><w:r><w:t>one</w:t></w:r></w:p><w:p><w:r><w:t>two</w:t></w:r></w:p></w:tc><w:tc><w:tbl><w:tr><w:tc><w:p><w:r><w:t>nested</w:t></w:r></w:p></w:tc></w:tr></w:tbl><w:p/></w:tc></w:tr></w:tbl>\
         <w:sdt><w:sdtContent><w:p><w:r><w:t>in a content control</w:t></w:r></w:p></w:sdtContent></w:sdt>\
         <w:p><w:r><w:t>last</w:t></w:r></w:p>\
         <w:sectPr/>",
    );
    let facts = facts_of_docx_package(&bytes, &l).unwrap();
    let p = facts.paragraphs();
    // Body paragraphs are the direct children of w:body only: the one inside
    // the content control is not one (the library holds that block raw).
    let texts: Vec<&str> = p.iter().map(|p| p.text.as_str()).collect();
    assert_eq!(
        texts,
        ["a & b <c> A\tx\ny", "", "boxkept", "last"],
        "{texts:?}"
    );
    assert_eq!(p[0].style.as_deref(), Some("Heading3"));
    let t = facts.tables();
    assert_eq!(t.len(), 1);
    // Paragraphs of a cell are joined by a newline; a nested table's text
    // belongs to the cell that holds it.
    assert_eq!(
        t[0].rows,
        vec![vec!["one\ntwo".to_owned(), "nested\n".to_owned()]]
    );
}

#[test]
fn docx_facts_count_pictures_and_charts_by_kind() {
    let l = limits();
    let bytes = docx_with_body(
        "<w:p><w:r><w:drawing><a:blip xmlns:a=\"http://schemas.openxmlformats.org/drawingml/2006/main\" r:embed=\"x\" xmlns:r=\"http://schemas.openxmlformats.org/officeDocument/2006/relationships\"/></w:drawing></w:r></w:p>\
         <w:p><w:r><w:drawing><c:chart r:id=\"y\" xmlns:r=\"http://schemas.openxmlformats.org/officeDocument/2006/relationships\"/></w:drawing></w:r></w:p>\
         <w:p><w:r><w:drawing><x:chart xmlns:x=\"urn:other\"/></w:drawing></w:r></w:p>",
    );
    let facts = facts_of_docx_package(&bytes, &l).unwrap();
    assert_eq!((facts.pictures, facts.charts), (1, 1));
    let p = facts.paragraphs();
    assert_eq!((p[0].pictures, p[0].charts), (1, 0));
    assert_eq!((p[1].pictures, p[1].charts), (0, 1));
    assert_eq!((p[2].pictures, p[2].charts), (0, 0));
}

#[test]
fn xlsx_facts_read_shared_inline_phonetic_and_error_cells() {
    let l = limits();
    let sheet = "<worksheet xmlns=\"http://schemas.openxmlformats.org/spreadsheetml/2006/main\"><sheetData>\
        <row r=\"1\"><c r=\"A1\" t=\"s\"><v>1</v></c><c r=\"B1\" t=\"inlineStr\"><is><t>in</t><r><t>line</t></r><rPh><t>IGNORED</t></rPh></is></c><c r=\"C1\" t=\"e\"><v>#DIV/0!</v></c><c r=\"D1\" t=\"b\"><v>0</v></c></row>\
        <row r=\"2\"><c r=\"A2\" t=\"str\"><f>CONCAT(A1,\"&amp;\")</f><v>x&amp;</v></c><c r=\"B2\"><f>1+1</f></c><c r=\"C2\" s=\"3\"/></row>\
        </sheetData></worksheet>";
    let shared = "<sst xmlns=\"http://schemas.openxmlformats.org/spreadsheetml/2006/main\"><si><t>zero</t></si><si><t>one</t><rPh><t>NOPE</t></rPh><r><t xml:space=\"preserve\"> &amp; more</t></r></si></sst>";
    let base = good_xlsx();
    let with_sheet = with_entry(&base, MAIN_XLSX, sheet.as_bytes().to_vec());
    let has_shared = entries(&with_sheet)
        .iter()
        .any(|(n, _)| n == "xl/sharedStrings.xml");
    let bytes = if has_shared {
        with_entry(
            &with_sheet,
            "xl/sharedStrings.xml",
            shared.as_bytes().to_vec(),
        )
    } else {
        // The workbook writes strings inline-free only with a shared table.
        with_sheet
    };
    let want: Vec<(usize, u32, u16)> = vec![
        (0, 0, 0),
        (0, 0, 1),
        (0, 0, 2),
        (0, 0, 3),
        (0, 1, 0),
        (0, 1, 1),
        (0, 1, 2),
    ];
    let facts = facts_of_xlsx_package(
        &bytes,
        &l,
        &XlsxQuery {
            cells: &want,
            ranges: &[],
            sheet_mention: None,
        },
    )
    .unwrap();
    if has_shared {
        assert_eq!(
            facts.cells[&(0, 0, 0)],
            CellValue::Text("one & more".into())
        );
    }
    assert_eq!(facts.cells[&(0, 0, 1)], CellValue::Text("inline".into()));
    assert_eq!(facts.cells[&(0, 0, 2)], CellValue::Error("#DIV/0!".into()));
    assert_eq!(facts.cells[&(0, 0, 3)], CellValue::Bool(false));
    match &facts.cells[&(0, 1, 0)] {
        CellValue::Formula { formula, cached } => {
            assert_eq!(formula, "CONCAT(A1,\"&\")");
            assert_eq!(cached.as_deref(), Some(&CellValue::Text("x&".into())));
        }
        other => panic!("{other:?}"),
    }
    match &facts.cells[&(0, 1, 1)] {
        CellValue::Formula { formula, cached } => {
            assert_eq!(formula, "1+1");
            assert!(cached.is_none());
        }
        other => panic!("{other:?}"),
    }
    // A styled blank cell holds nothing.
    assert!(!facts.cells.contains_key(&(0, 1, 2)));
    assert_eq!(facts.sheets[0].value_cells, 6);
}
