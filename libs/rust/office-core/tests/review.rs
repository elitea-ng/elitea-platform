//! Regression tests for review round 1 of PR #1274 and the ADR-0032 round 2
//! changes. Each test names the finding it pins.
#![allow(clippy::pedantic)]
mod common;
use common::*;

use elitea_office_core::testing::{PackageKind, poison_deck, render_copy_pixels, validate};
use elitea_office_core::{
    CellValue, ChartKind, ChartSpec, Deck, Document, Limit, Limits, OfficeError, Rect, Series,
    SlideLayout, Workbook,
};

const NS: &str = r#"xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships""#;

/// A one-slide deck whose slide has the given text boxes; each box is a list
/// of paragraph XML.
fn deck_with_boxes(boxes: &[&str]) -> Vec<u8> {
    let shapes: String = boxes
        .iter()
        .enumerate()
        .map(|(i, paras)| {
            format!(
                r#"<p:sp><p:nvSpPr><p:cNvPr id="{}" name="Box {i}"/><p:cNvSpPr txBox="1"/><p:nvPr/></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/>{paras}</p:txBody></p:sp>"#,
                i + 2
            )
        })
        .collect();
    let xml = format!(
        r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:sld {NS}><p:cSld><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/>{shapes}</p:spTree></p:cSld></p:sld>"#
    );
    with_entry(&good_deck(), MAIN_PPTX, xml.into_bytes())
}

fn run(t: &str) -> String {
    format!("<a:r><a:t>{t}</a:t></a:r>")
}

fn texts(d: &Deck) -> Vec<String> {
    d.shapes(0).unwrap().into_iter().map(|s| s.text).collect()
}

// ---- finding 2: the image part name comes from the signature --------------

fn jpeg_header() -> Vec<u8> {
    vec![
        0xFF, 0xD8, 0xFF, 0xE0, 0, 4, 0, 0, 0xFF, 0xC0, 0, 11, 8, 0, 9, 0, 7, 3, 0, 0, 0,
    ]
}

#[test]
fn docx_image_extension_is_sniffed_not_supplied() {
    let l = limits();
    let mut d = Document::new(&l);
    d.add_image(TINY_PNG, Rect::inches(0.0, 0.0, 1.0, 1.0))
        .unwrap();
    d.add_image(&jpeg_header(), Rect::inches(0.0, 0.0, 1.0, 1.0))
        .unwrap();
    let saved = entries(&d.save().unwrap());
    let media: Vec<&str> = saved
        .iter()
        .map(|(n, _)| n.as_str())
        .filter(|n| n.starts_with("word/media/"))
        .collect();
    assert_eq!(media.len(), 2, "{media:?}");
    for n in &media {
        assert!(n.ends_with(".png") || n.ends_with(".jpeg"), "{n}");
    }
    let ct = String::from_utf8(
        saved
            .iter()
            .find(|(n, _)| n == "[Content_Types].xml")
            .unwrap()
            .1
            .clone(),
    )
    .unwrap();
    assert!(
        ct.contains("Extension=\"png\"") && ct.contains("Extension=\"jpeg\""),
        "{ct}"
    );
    // Not an image: refused before the library sees it, whatever the caller meant.
    assert!(matches!(
        d.add_image(
            b"<svg xmlns='http://www.w3.org/2000/svg'/>",
            Rect::inches(0.0, 0.0, 1.0, 1.0)
        ),
        Err(OfficeError::InvalidInput(_))
    ));
}

// ---- finding 3: paragraph index means paragraph -----------------------------

#[test]
fn docx_set_paragraph_text_skips_tables_and_other_content() {
    let l = limits();
    let mut d = Document::new(&l);
    d.add_paragraph("zero").unwrap();
    let t = d.add_table(1, 1).unwrap();
    d.set_table_cell(t, 0, 0, "cell").unwrap();
    d.add_paragraph("one").unwrap();
    d.add_chart(&chart(), Rect::inches(0.0, 0.0, 4.0, 3.0))
        .unwrap();
    d.add_paragraph("two").unwrap();
    d.set_paragraph_text(1, "ONE").unwrap();
    d.set_paragraph_text(2, "TWO").unwrap();
    let text = d.to_plain_text().unwrap();
    let pos = |s: &str| {
        text.find(s)
            .unwrap_or_else(|| panic!("{s} missing in {text:?}"))
    };
    assert!(
        pos("zero") < pos("cell") && pos("cell") < pos("ONE") && pos("ONE") < pos("TWO"),
        "{text:?}"
    );
    assert!(!text.contains("one") && !text.contains("two"));
    assert_eq!(
        d.tables().unwrap()[0].rows[0][0],
        "cell",
        "the table survived"
    );
    assert!(matches!(
        d.set_paragraph_text(3, "x"),
        Err(OfficeError::NotFound(_))
    ));
}

// ---- findings 4 and 9: replace_text is atomic and position-exact ---------------

#[test]
fn pptx_replace_is_atomic_when_a_later_paragraph_is_unsupported() {
    let l = limits();
    let bytes = deck_with_boxes(&[
        &format!("<a:p>{}</a:p>", run("fo here")),
        // "fo" spans runs 0 and 1, and a line break follows: Unsupported.
        &format!("<a:p>{}{}<a:br/>{}</a:p>", run("xf"), run("oy"), run("z")),
    ]);
    let mut d = Deck::open(&bytes, &l).unwrap();
    let before = texts(&d);
    let saved_before = d.save().unwrap();
    assert!(matches!(
        d.replace_text("fo", "ZZ"),
        Err(OfficeError::Unsupported(_))
    ));
    assert_eq!(texts(&d), before, "the first box must not have been edited");
    assert_eq!(d.save().unwrap(), saved_before);
}

#[test]
fn pptx_replace_uses_match_positions_not_counts() {
    let l = limits();
    // "aaa" split a|aa: the one match (0..2) spans the runs, while the run
    // "aa" also contains one match of its own. Counting would edit the run
    // and give "aZZ"; positions say the match spans runs, so the paragraph
    // is rewritten: "ZZa".
    let bytes = deck_with_boxes(&[&format!("<a:p>{}{}</a:p>", run("a"), run("aa"))]);
    let mut d = Deck::open(&bytes, &l).unwrap();
    assert_eq!(d.replace_text("aa", "ZZ").unwrap(), 1);
    assert_eq!(texts(&d), ["ZZa"]);
    // Matches that all lie inside runs keep the run structure.
    let bytes = deck_with_boxes(&[&format!("<a:p>{}{}</a:p>", run("one ab"), run(" two ab"))]);
    let mut d = Deck::open(&bytes, &l).unwrap();
    assert_eq!(d.replace_text("ab", "X").unwrap(), 2);
    assert_eq!(texts(&d), ["one X two X"]);
    assert_eq!(d.shapes(0).unwrap()[0].paragraphs[0].runs.len(), 2);
}

// ---- finding 5: parts are classified by their bytes --------------------------

#[test]
fn a_renamed_slide_is_still_xml() {
    let l = limits();
    let g = good_deck();
    // Deeply nested XML named like a PNG, a VML drawing and a .bin.
    let deep = |depth| {
        let mut x = b"<a>".to_vec();
        for _ in 0..depth {
            x.extend_from_slice(b"<b>");
        }
        x.extend(std::iter::repeat_n(b' ', 8));
        x
    };
    let mut nested = deep(300);
    for _ in 0..301 {
        nested.extend_from_slice(b"</b>");
    }
    nested.extend_from_slice(b"</a>");
    for name in [
        "ppt/media/slide9.png",
        "ppt/drawings/vmlDrawing1.vml",
        "ppt/embeddings/x.bin",
    ] {
        let r = validate(&with_extra(&g, name, nested.clone()), PackageKind::Pptx, &l);
        assert!(
            matches!(
                r,
                Err(OfficeError::LimitExceeded {
                    limit: Limit::XmlDepth,
                    ..
                })
            ),
            "{name}: {r:?}"
        );
    }
    // A DTD in a .vml is caught too.
    let dtd = b"<?xml version=\"1.0\"?><!DOCTYPE a [<!ENTITY e \"x\">]><a/>".to_vec();
    let r = validate(
        &with_extra(&g, "ppt/drawings/vmlDrawing1.vml", dtd),
        PackageKind::Pptx,
        &l,
    );
    assert!(matches!(r, Err(OfficeError::Malformed(_))), "{r:?}");
    // XML named .bin gets the XML part cap, not the media cap.
    let big = format!("<a>{}</a>", " ".repeat(9 << 20)).into_bytes();
    let r = validate(
        &with_extra(&g, "ppt/embeddings/blob.bin", big),
        PackageKind::Pptx,
        &l,
    );
    assert!(
        matches!(
            r,
            Err(OfficeError::LimitExceeded {
                limit: Limit::XmlPartBytes,
                ..
            })
        ),
        "{r:?}"
    );
    // A PNG named .xml is binary, not parsed as XML, and counted as an image.
    let r = validate(
        &with_extra(&g, "ppt/media/not-xml.xml", png_header(8, 8)),
        PackageKind::Pptx,
        &l,
    );
    assert!(r.is_ok(), "{r:?}");
    // UTF-16 XML is not parsed by anything here, so it is refused.
    let r = validate(
        &with_extra(&g, "ppt/x.xml", vec![0xFF, 0xFE, b'<', 0]),
        PackageKind::Pptx,
        &l,
    );
    assert!(matches!(r, Err(OfficeError::Unsupported(_))), "{r:?}");
}

#[test]
fn svg_is_recognised_by_its_root_not_its_name() {
    let l = limits();
    let mut e = entries(&good_deck());
    let svg = b"<svg xmlns='http://www.w3.org/2000/svg' width='99999' height='99999'><rect width='99999' height='99999'/></svg>".to_vec();
    e.push(("ppt/media/picture.dat".into(), svg.clone()));
    let bytes = zip_of(&e);
    let copy =
        elitea_office_core::testing::render_copy_bytes(&bytes, PackageKind::Pptx, &l).unwrap();
    let in_copy = entries(&copy)
        .into_iter()
        .find(|(n, _)| n == "ppt/media/picture.dat")
        .unwrap()
        .1;
    assert!(
        String::from_utf8_lossy(&in_copy).contains("width=\"1\""),
        "{in_copy:?}"
    );
    assert!(!String::from_utf8_lossy(&in_copy).contains("99999"));
    let saved = entries(&Deck::open(&bytes, &l).unwrap().save().unwrap());
    assert_eq!(
        saved
            .iter()
            .find(|(n, _)| n == "ppt/media/picture.dat")
            .unwrap()
            .1,
        svg,
        "saves keep it"
    );
}

// ---- findings 6: max_cells at open -------------------------------------------

#[test]
fn max_cells_is_enforced_at_open_for_every_format() {
    let l = limits();
    let strict = Limits {
        max_cells: 5,
        ..limits()
    };
    // docx: 3 x 2 table cells.
    let mut d = Document::new(&l);
    d.add_table(3, 2).unwrap();
    let docx = d.save().unwrap();
    assert!(matches!(
        Document::open(&docx, &strict),
        Err(OfficeError::LimitExceeded {
            limit: Limit::Cells,
            value: 6
        })
    ));
    assert!(
        Document::open(
            &docx,
            &Limits {
                max_cells: 6,
                ..limits()
            }
        )
        .is_ok()
    );
    // pptx: table cells.
    let mut p = Deck::new(&l);
    let s = p.add_slide(SlideLayout::Blank).unwrap();
    p.add_table(s, 2, 3, Rect::inches(0.0, 0.0, 3.0, 1.0))
        .unwrap();
    let pptx = p.save().unwrap();
    assert!(matches!(
        Deck::open(&pptx, &strict),
        Err(OfficeError::LimitExceeded {
            limit: Limit::Cells,
            ..
        })
    ));
    // pptx: chart points count too.
    let mut p = Deck::new(&l);
    let s = p.add_slide(SlideLayout::Blank).unwrap();
    p.add_chart(s, &chart(), Rect::inches(0.0, 0.0, 3.0, 2.0))
        .unwrap();
    let with_chart = p.save().unwrap();
    assert!(matches!(
        Deck::open(
            &with_chart,
            &Limits {
                max_cells: 1,
                ..limits()
            }
        ),
        Err(OfficeError::LimitExceeded {
            limit: Limit::Cells,
            ..
        })
    ));
    // xlsx: worksheet cells, counted by the gate.
    let mut w = Workbook::new(&l);
    w.write_row(
        0,
        0,
        0,
        &[
            CellValue::Number(1.0),
            CellValue::Number(2.0),
            CellValue::Number(3.0),
        ],
    )
    .unwrap();
    let xlsx = w.save().unwrap();
    assert!(matches!(
        Workbook::open(
            &xlsx,
            &Limits {
                max_cells: 2,
                ..limits()
            }
        ),
        Err(OfficeError::LimitExceeded {
            limit: Limit::Cells,
            value: 3
        })
    ));
    // An opened file's cells count against later edits.
    let mut d = Deck::open(
        &pptx,
        &Limits {
            max_cells: 8,
            ..limits()
        },
    )
    .unwrap();
    assert!(matches!(
        d.add_table(0, 2, 2, Rect::inches(0.0, 0.0, 1.0, 1.0)),
        Err(OfficeError::LimitExceeded {
            limit: Limit::Cells,
            value: 10
        })
    ));
    let mut x = Workbook::open(
        &xlsx,
        &Limits {
            max_cells: 4,
            ..limits()
        },
    )
    .unwrap();
    x.write_cell(0, 5, 0, &CellValue::Number(1.0)).unwrap();
    assert!(matches!(
        x.write_cell(0, 6, 0, &CellValue::Number(1.0)),
        Err(OfficeError::LimitExceeded {
            limit: Limit::Cells,
            value: 5
        })
    ));
}

// ---- findings 7 and 8: cell accounting and error cells --------------------------

#[test]
fn xlsx_counts_distinct_cells_after_a_successful_write() {
    let l = Limits {
        max_cells: 3,
        ..limits()
    };
    let mut w = Workbook::new(&l);
    for _ in 0..10 {
        w.write_cell(0, 0, 0, &CellValue::Number(1.0)).unwrap(); // overwrite
    }
    // Writes that fail do not use up cells.
    assert!(matches!(
        w.write_cell(0, 1_048_576, 0, &CellValue::Number(1.0)),
        Err(OfficeError::InvalidInput(_))
    ));
    assert!(matches!(
        w.write_cell(0, 1, 0, &CellValue::Error("#N/A".into())),
        Err(OfficeError::Unsupported(_))
    ));
    assert!(matches!(
        w.write_cell(9, 0, 0, &CellValue::Number(1.0)),
        Err(OfficeError::NotFound(_))
    ));
    w.write_cell(0, 1, 0, &CellValue::Text("b".into())).unwrap();
    w.write_cell(0, 2, 0, &CellValue::Bool(true)).unwrap();
    // Three distinct cells are written: the fourth is refused, an overwrite is not.
    assert!(matches!(
        w.write_cell(0, 3, 0, &CellValue::Number(1.0)),
        Err(OfficeError::LimitExceeded {
            limit: Limit::Cells,
            value: 4
        })
    ));
    w.write_cell(0, 2, 0, &CellValue::Bool(false)).unwrap();
}

#[test]
fn xlsx_error_cells_are_not_written_as_text() {
    let l = limits();
    let mut w = Workbook::new(&l);
    let r = w.write_cell(0, 0, 0, &CellValue::Error("#DIV/0!".into()));
    assert!(matches!(r, Err(OfficeError::Unsupported(m)) if m.contains("error cell")));
    assert_eq!(w.read_cell(0, 0, 0).unwrap(), CellValue::Empty);
}

// ---- finding 10: poisoned state is Internal, not Busy -----------------------------

#[test]
fn a_panic_in_a_guarded_call_reads_as_internal_afterwards() {
    let l = limits();
    let d = Deck::new(&l);
    poison_deck(&d);
    assert!(matches!(d.slide_count(), Err(OfficeError::Internal(_))));
    assert!(matches!(d.save(), Err(OfficeError::Internal(_))));
}

// ---- round 2: render copies are fitted, not refused ---------------------------------

fn gray_png(w: u32, h: u32) -> Vec<u8> {
    use image::ImageEncoder;
    let img = vec![200u8; (w * h) as usize];
    let mut out = Vec::new();
    image::codecs::png::PngEncoder::new_with_quality(
        &mut out,
        image::codecs::png::CompressionType::Default,
        image::codecs::png::FilterType::Up,
    )
    .write_image(&img, w, h, image::ExtendedColorType::L8)
    .unwrap();
    out
}

#[test]
fn render_copy_fits_a_35_megapixel_image_into_two() {
    let l = limits();
    let g = good_deck();
    let big = gray_png(7000, 5000); // 35 MP: under the 40 MP cap, over the 2 MP allowance
    assert!(big.len() < 30 << 20, "{}", big.len());
    let bytes = with_extra(&g, "ppt/media/photo.png", big.clone());
    let px = render_copy_pixels(&bytes, PackageKind::Pptx, &l).unwrap();
    assert_eq!(px.len(), 1);
    assert!(px[0] <= 2_000_000 && px[0] > 1_500_000, "{px:?}");
    // Rendering works, and the saved file keeps the original bytes.
    let d = Deck::open(&bytes, &l).unwrap();
    assert!(d.render_preview(0).unwrap().starts_with(b"\x89PNG"));
    let saved = entries(&d.save().unwrap());
    assert_eq!(
        saved
            .iter()
            .find(|(n, _)| n == "ppt/media/photo.png")
            .unwrap()
            .1,
        big
    );
}

#[test]
fn render_copy_shrinks_each_image_more_when_there_are_many() {
    let l = limits();
    let mut e = entries(&good_deck());
    let one = gray_png(1800, 1200); // 2.2 MP
    for i in 0..20 {
        e.push((format!("ppt/media/p{i}.png"), one.clone()));
    }
    let bytes = zip_of(&e);
    let px = render_copy_pixels(&bytes, PackageKind::Pptx, &l).unwrap();
    assert_eq!(px.len(), 20);
    // 20 > 16 images: 32 MP / 20 = 1.6 MP each, never under 0.25 MP.
    assert!(
        px.iter().all(|&p| (250_000..=1_600_000).contains(&p)),
        "{px:?}"
    );
    assert!(px.iter().sum::<u64>() <= 32_000_000);
    // 500 images: only 127 fit at the floor (32 MP less 500 placeholder pixels, over 0.25 MP). The first 127
    // (in document order) are fitted, the rest become 1x1 placeholders, and
    // the decoded total stays inside the budget.
    let mut e = entries(&good_deck());
    let small = gray_png(600, 600);
    for i in 0..500 {
        e.push((format!("ppt/media/p{i:03}.png"), small.clone()));
    }
    let px = render_copy_pixels(&zip_of(&e), PackageKind::Pptx, &l).unwrap();
    assert_eq!(px.len(), 500);
    assert!(
        px[..127].iter().all(|&p| (240_000..=250_000).contains(&p)),
        "{:?}",
        &px[..127]
    );
    assert!(
        px[127..].iter().all(|&p| p == 1),
        "the rest are placeholders"
    );
    assert!(px.iter().sum::<u64>() <= 32_000_000);
}

#[test]
fn small_images_are_left_alone_in_the_render_copy() {
    let l = limits();
    let bytes = with_extra(&good_deck(), "ppt/media/small.png", gray_png(100, 100));
    let copy =
        elitea_office_core::testing::render_copy_bytes(&bytes, PackageKind::Pptx, &l).unwrap();
    assert_eq!(
        entries(&copy)
            .into_iter()
            .find(|(n, _)| n == "ppt/media/small.png")
            .unwrap()
            .1,
        gray_png(100, 100)
    );
}

// ---- round 2: xlsx XML caps -------------------------------------------------------------

#[test]
fn xlsx_worksheets_get_48_mib_and_other_parts_8() {
    let l = limits();
    let g = good_xlsx();
    let pad = |mib: usize| format!("<a>{}</a>", " ".repeat(mib << 20)).into_bytes();
    // 40 MiB in the worksheet is fine, 40 MiB in docProps is not.
    let sheet = {
        let x = String::from_utf8(
            entries(&g)
                .into_iter()
                .find(|(n, _)| n == MAIN_XLSX)
                .unwrap()
                .1,
        )
        .unwrap();
        x.replacen(
            "</sheetData>",
            &format!("</sheetData>{}", " ".repeat(40 << 20)),
            1,
        )
        .into_bytes()
    };
    assert!(validate(&with_entry(&g, MAIN_XLSX, sheet), PackageKind::Xlsx, &l).is_ok());
    let r = validate(
        &with_extra(&g, "docProps/big.xml", pad(40)),
        PackageKind::Xlsx,
        &l,
    );
    assert!(
        matches!(
            r,
            Err(OfficeError::LimitExceeded {
                limit: Limit::XmlPartBytes,
                ..
            })
        ),
        "{r:?}"
    );
    // 49 MiB of worksheet is over the large-part cap.
    let r = validate(&with_entry(&g, MAIN_XLSX, pad(49)), PackageKind::Xlsx, &l);
    assert!(
        matches!(
            r,
            Err(OfficeError::LimitExceeded {
                limit: Limit::XmlPartBytes,
                ..
            })
        ),
        "{r:?}"
    );
    // The total is 96 MiB: two 47 MiB sheets (both reachable through the
    // workbook's relationships) fit, a further 5 MiB part does not.
    let mut w = Workbook::new(&l);
    w.write_cell(0, 0, 0, &CellValue::Number(1.0)).unwrap();
    w.add_sheet("Two").unwrap();
    let two = w.save().unwrap();
    let mut e = entries(&two);
    for (n, d) in &mut e {
        if n.starts_with("xl/worksheets/sheet") {
            *d = pad(47);
        }
    }
    assert!(validate(&zip_of(&e), PackageKind::Xlsx, &l).is_ok());
    e.push(("docProps/extra.xml".into(), pad(5)));
    let r = validate(&zip_of(&e), PackageKind::Xlsx, &l);
    assert!(
        matches!(
            r,
            Err(OfficeError::LimitExceeded {
                limit: Limit::XmlTotalBytes,
                ..
            })
        ),
        "{r:?}"
    );
}

#[test]
fn chart_spec_is_checked_before_the_library() {
    let l = limits();
    let mut d = Deck::new(&l);
    let s = d.add_slide(SlideLayout::Blank).unwrap();
    let spec = ChartSpec {
        kind: ChartKind::Line,
        categories: (0..2000).map(|i| i.to_string()).collect(),
        series: vec![
            Series {
                name: "s".into(),
                values: vec![0.0; 2000]
            };
            600
        ],
        title: None,
        legend_position: None,
        data_labels: false,
    };
    assert!(matches!(
        d.add_chart(s, &spec, Rect::inches(0.0, 0.0, 1.0, 1.0)),
        Err(OfficeError::LimitExceeded {
            limit: Limit::Cells,
            value: 1_200_000
        })
    ));
}

// ============================ review round 2 ===================================

fn nested_xml(prefix: &[u8], depth: usize) -> Vec<u8> {
    let mut x = prefix.to_vec();
    for _ in 0..depth {
        x.extend_from_slice(b"<b>");
    }
    x
}

// ---- 1: classification by declared type, then bytes ---------------------------------

#[test]
fn junk_before_the_root_does_not_hide_nesting() {
    let l = limits();
    let g = good_deck();
    // Undeclared part: `x<p:sld>` is not well formed, and is scanned anyway.
    let r = validate(
        &with_extra(&g, "ppt/slides/slide9.dat", nested_xml(b"x<p:sld>", 400)),
        PackageKind::Pptx,
        &l,
    );
    assert!(
        matches!(
            r,
            Err(OfficeError::LimitExceeded {
                limit: Limit::XmlDepth,
                ..
            })
        ),
        "{r:?}"
    );
    // GIF89a-prefixed XML declared as XML by its content type, under a name
    // that says nothing: still XML-checked.
    let mut gif_xml = nested_xml(b"GIF89a<a>", 400);
    gif_xml.extend_from_slice(b"</a>");
    let declared = with_content_types(
        &with_extra(&g, "ppt/slides/slide8.dat", gif_xml),
        r#"<Override PartName="/ppt/slides/slide8.dat" ContentType="application/vnd.foo+xml"/>"#,
    );
    let r = validate(&declared, PackageKind::Pptx, &l);
    assert!(
        matches!(
            r,
            Err(OfficeError::LimitExceeded {
                limit: Limit::XmlDepth,
                ..
            })
        ),
        "{r:?}"
    );
    // A plain GIF with no declaration is still a GIF.
    let mut gif = b"GIF89a".to_vec();
    gif.extend_from_slice(&[4, 0, 4, 0, 0, 0, 0]);
    assert!(
        validate(
            &with_extra(&g, "ppt/media/ok.gif", gif),
            PackageKind::Pptx,
            &l
        )
        .is_ok()
    );
}

#[test]
fn declared_fonts_pass_and_are_emptied_in_render_copies_only() {
    let l = limits();
    let g = good_deck();
    // A real-looking font by content type (the Default PowerPoint writes)...
    let ttf = [vec![0u8, 1, 0, 0], noise(4096, 5)].concat();
    let bytes = with_content_types(
        &with_extra(&g, "ppt/fonts/font1.fntdata", ttf.clone()),
        r#"<Default Extension="fntdata" ContentType="application/x-fontdata"/>"#,
    );
    assert!(validate(&bytes, PackageKind::Pptx, &l).is_ok());
    // ...and the obfuscated docx kind.
    let dx = with_content_types(
        &with_extra(&good_docx(), "word/fonts/font1.odttf", noise(2048, 6)),
        r#"<Default Extension="odttf" ContentType="application/vnd.openxmlformats-officedocument.obfuscatedFont"/>"#,
    );
    assert!(validate(&dx, PackageKind::Docx, &l).is_ok());
    // A font declared only by a relationship, under a name nothing else gives
    // away, and full of tags: a font is not scanned as XML.
    let weird = nested_xml(b"", 400);
    let px = with_extra(&g, "ppt/odd/f1.dat", weird.clone());
    let px = patch_text(
        &px,
        "ppt/_rels/presentation.xml.rels",
        "</Relationships>",
        r#"<Relationship Id="rIdF" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/font" Target="odd/f1.dat"/></Relationships>"#,
    );
    assert!(validate(&px, PackageKind::Pptx, &l).is_ok());
    let copy =
        elitea_office_core::testing::render_copy_bytes(&bytes, PackageKind::Pptx, &l).unwrap();
    assert!(
        entries(&copy)
            .iter()
            .any(|(n, d)| n == "ppt/fonts/font1.fntdata" && d.is_empty())
    );
    let saved = entries(&Deck::open(&bytes, &l).unwrap().save().unwrap());
    assert_eq!(
        saved
            .iter()
            .find(|(n, _)| n == "ppt/fonts/font1.fntdata")
            .unwrap()
            .1,
        ttf
    );
}

// ---- 2: WebP and TIFF headers -------------------------------------------------------------

fn webp_vp8x(w: u32, h: u32) -> Vec<u8> {
    let mut v = b"RIFF\0\0\0\0WEBPVP8X\x0a\0\0\0\0\0\0\0".to_vec();
    v.extend_from_slice(&(w - 1).to_le_bytes()[..3]);
    v.extend_from_slice(&(h - 1).to_le_bytes()[..3]);
    v
}

fn tiff(w: u32, h: u32) -> Vec<u8> {
    let mut t = vec![0x49, 0x49, 0x2A, 0, 8, 0, 0, 0, 2, 0];
    t.extend_from_slice(&[0, 1, 4, 0, 1, 0, 0, 0]);
    t.extend_from_slice(&w.to_le_bytes());
    t.extend_from_slice(&[1, 1, 4, 0, 1, 0, 0, 0]);
    t.extend_from_slice(&h.to_le_bytes());
    t
}

#[test]
fn webp_and_tiff_get_the_pixel_cap_the_count_and_fitting() {
    let l = limits();
    let g = good_deck();
    for (name, data) in [
        ("ppt/media/big.webp", webp_vp8x(10_000, 10_000)),
        ("ppt/media/big.tif", tiff(10_000, 10_000)),
    ] {
        let r = validate(&with_extra(&g, name, data), PackageKind::Pptx, &l);
        assert!(
            matches!(
                r,
                Err(OfficeError::LimitExceeded {
                    limit: Limit::ImagePixels,
                    value: 100_000_000
                })
            ),
            "{name}: {r:?}"
        );
    }
    // They count toward the image limit.
    let mut e = entries(&g);
    e.extend((0..501).map(|i| (format!("ppt/media/w{i}.webp"), webp_vp8x(8, 8))));
    let r = validate(&zip_of(&e), PackageKind::Pptx, &l);
    assert!(
        matches!(
            r,
            Err(OfficeError::LimitExceeded {
                limit: Limit::Images,
                ..
            })
        ),
        "{r:?}"
    );
    // Render copy: a small one is untouched (1 px counted as its own size), an
    // over-allowance one that cannot be decoded here is a placeholder, and one
    // whose dimensions cannot be read is a placeholder too, never passed on.
    let mut e = entries(&g);
    e.push(("ppt/media/small.webp".into(), webp_vp8x(100, 100)));
    e.push(("ppt/media/large.webp".into(), webp_vp8x(4000, 4000)));
    e.push((
        "ppt/media/broken.webp".into(),
        b"RIFF\0\0\0\0WEBPVP8 ".to_vec(),
    ));
    e.push(("ppt/media/large.tif".into(), tiff(4000, 4000)));
    let bytes = zip_of(&e);
    let px = render_copy_pixels(&bytes, PackageKind::Pptx, &l).unwrap();
    assert_eq!(
        px,
        [10_000, 1, 1, 1],
        "small kept, the three others are placeholders"
    );
    // The file itself opens (the unreadable header does not stop it) and saves
    // the originals back.
    let d = Deck::open(&bytes, &l).unwrap();
    assert!(
        entries(&d.save().unwrap())
            .iter()
            .any(|(n, d)| n == "ppt/media/broken.webp" && d.len() == 16)
    );
    assert!(d.render_preview(0).unwrap().starts_with(b"\x89PNG"));
}

// ---- 3: xlsx cells are found through the workbook's relationships ---------------------------

#[test]
fn xlsx_cells_are_counted_in_whatever_part_the_workbook_calls_a_sheet() {
    let l = limits();
    let mut w = Workbook::new(&l);
    w.write_row(
        0,
        0,
        0,
        &[
            CellValue::Number(1.0),
            CellValue::Number(2.0),
            CellValue::Number(3.0),
        ],
    )
    .unwrap();
    let x = w.save().unwrap();
    // Move the sheet to a name no path rule would guess, and repoint the rels.
    let mut e = entries(&x);
    for (n, _) in &mut e {
        if n == "xl/worksheets/sheet1.xml" {
            *n = "xl/odd/s.dat".to_owned();
        }
    }
    let moved = patch_text(
        &zip_of(&e),
        "xl/_rels/workbook.xml.rels",
        "worksheets/sheet1.xml",
        "odd/s.dat",
    );
    let moved = patch_text(
        &moved,
        "[Content_Types].xml",
        "/xl/worksheets/sheet1.xml",
        "/xl/odd/s.dat",
    );
    let strict = Limits {
        max_cells: 2,
        ..limits()
    };
    let r = validate(&moved, PackageKind::Xlsx, &strict);
    assert!(
        matches!(
            r,
            Err(OfficeError::LimitExceeded {
                limit: Limit::Cells,
                value: 3
            })
        ),
        "{r:?}"
    );
    assert!(
        Workbook::open(&moved, &limits()).is_ok(),
        "and it still opens normally"
    );
    // `c` elements elsewhere (calcChain lists formula cells) are not cells.
    let calc = format!(
        "<calcChain>{}</calcChain>",
        "<c r=\"A1\" i=\"1\"/>".repeat(50)
    );
    let with_chain = with_extra(&x, "xl/calcChain.xml", calc.into_bytes());
    assert!(
        validate(
            &with_chain,
            PackageKind::Xlsx,
            &Limits {
                max_cells: 3,
                ..limits()
            }
        )
        .is_ok()
    );
}

// ---- 4: Empty clears -----------------------------------------------------------------------------

#[test]
fn xlsx_empty_clears_the_cell_and_gives_the_count_back() {
    let l = Limits {
        max_cells: 2,
        ..limits()
    };
    let mut w = Workbook::new(&l);
    w.write_cell(0, 0, 0, &CellValue::Number(1.0)).unwrap();
    w.write_cell(0, 0, 1, &CellValue::Text("x".into())).unwrap();
    assert!(matches!(
        w.write_cell(0, 0, 2, &CellValue::Number(3.0)),
        Err(OfficeError::LimitExceeded {
            limit: Limit::Cells,
            value: 3
        })
    ));
    w.write_cell(0, 0, 0, &CellValue::Empty).unwrap();
    assert_eq!(w.read_cell(0, 0, 0).unwrap(), CellValue::Empty);
    w.write_cell(0, 0, 2, &CellValue::Number(3.0)).unwrap(); // room again
    // Clearing a cell that was never there neither writes nor underflows.
    w.write_cell(0, 9, 9, &CellValue::Empty).unwrap();
    w.write_cell(0, 9, 9, &CellValue::Empty).unwrap();
    assert!(matches!(
        w.write_cell(0, 5, 5, &CellValue::Number(1.0)),
        Err(OfficeError::LimitExceeded { .. })
    ));
    // The saved file has no blank cell element for it.
    let saved = w.save().unwrap();
    let sheet = String::from_utf8(
        entries(&saved)
            .into_iter()
            .find(|(n, _)| n == MAIN_XLSX)
            .unwrap()
            .1,
    )
    .unwrap();
    assert!(
        !sheet.contains("r=\"A1\"") && !sheet.contains("r=\"J10\""),
        "{sheet}"
    );
    // And on a file that was opened (its cell is in the library's parsed map).
    let mut o = Workbook::open(&good_xlsx(), &limits()).unwrap();
    assert_ne!(o.read_cell(0, 0, 0).unwrap(), CellValue::Empty);
    o.write_cell(0, 0, 0, &CellValue::Empty).unwrap();
    assert_eq!(o.read_cell(0, 0, 0).unwrap(), CellValue::Empty);
    let again = Workbook::open(&o.save().unwrap(), &limits()).unwrap();
    assert_eq!(again.read_cell(0, 0, 0).unwrap(), CellValue::Empty);
}

// ---- 5: absurd part numbers --------------------------------------------------------------------------

#[test]
fn absurd_part_numbers_are_typed_errors_not_overflows() {
    let l = limits();
    let g = good_deck();
    for name in [
        "ppt/charts/chart18446744073709551615.xml",
        "ppt/charts/chart1000001.xml",
        "ppt/slides/slide99999999999.xml",
        "ppt/media/image4294967296.png",
    ] {
        let bytes = with_extra(&g, name, b"<c/>".to_vec());
        assert!(
            matches!(Deck::open(&bytes, &l), Err(OfficeError::Malformed(m)) if m.contains("part number")),
            "{name}"
        );
    }
    // At the bound it is fine: one chart before the end of the range, one more
    // can be added and saved; at the end of the range the next is a typed error.
    let bytes = with_extra(&g, "ppt/charts/chart999999.xml", b"<c/>".to_vec());
    let mut d = Deck::open(&bytes, &l).unwrap();
    d.add_chart(0, &chart(), Rect::inches(0.0, 0.0, 3.0, 2.0))
        .unwrap();
    let saved = d.save().unwrap();
    assert!(
        entries(&saved)
            .iter()
            .any(|(n, _)| n == "ppt/charts/chart1000000.xml")
    );
    let mut d = Deck::open(&saved, &l).unwrap();
    let r = d.add_chart(0, &chart(), Rect::inches(0.0, 0.0, 3.0, 2.0));
    assert!(
        matches!(r, Err(OfficeError::InvalidInput(ref m)) if m.contains("used up")),
        "{r:?}"
    );
}

// ---- 9: the main part comes from _rels/.rels --------------------------------------------------------------

#[test]
fn render_size_checks_follow_the_main_part_not_a_path() {
    let l = limits();
    // pptx: the relationships name ppt/pres.xml as the main part, whose slide
    // is a million times taller than wide; ppt/presentation.xml (the path the
    // library opens) is benign. The render gate reads the part the
    // relationships name.
    let g = good_deck();
    let renamed = rename_main(&g, "ppt/presentation.xml", "ppt/pres.xml");
    let x = String::from_utf8(
        entries(&renamed)
            .into_iter()
            .find(|(n, _)| n == "ppt/pres.xml")
            .unwrap()
            .1,
    )
    .unwrap();
    let start = x.find("<p:sldSz").unwrap();
    let end = x[start..].find("/>").unwrap() + start + 2;
    let tall = format!(
        "{}<p:sldSz cx=\"1000\" cy=\"2147483000\"/>{}",
        &x[..start],
        &x[end..]
    );
    let bytes = with_entry(&renamed, "ppt/pres.xml", tall.into_bytes());
    let r = elitea_office_core::testing::render_copy_bytes(&bytes, PackageKind::Pptx, &l);
    assert!(
        matches!(
            r,
            Err(OfficeError::LimitExceeded {
                limit: Limit::RenderPixels,
                ..
            })
        ),
        "{r:?}"
    );
    // The other way round: benign main part by relationship, huge size at the
    // fixed path. Also refused.
    let x = String::from_utf8(
        entries(&g)
            .into_iter()
            .find(|(n, _)| n == "ppt/presentation.xml")
            .unwrap()
            .1,
    )
    .unwrap();
    let start = x.find("<p:sldSz").unwrap();
    let end = x[start..].find("/>").unwrap() + start + 2;
    let tall = format!(
        "{}<p:sldSz cx=\"1000\" cy=\"2147483000\"/>{}",
        &x[..start],
        &x[end..]
    );
    let split = with_extra(
        &rename_main(&g, "ppt/presentation.xml", "ppt/pres.xml"),
        "ppt/presentation.xml",
        tall.into_bytes(),
    );
    let r = elitea_office_core::testing::render_copy_bytes(&split, PackageKind::Pptx, &l);
    assert!(
        matches!(
            r,
            Err(OfficeError::LimitExceeded {
                limit: Limit::RenderPixels,
                ..
            })
        ),
        "{r:?}"
    );
    // docx: word/document.xml renamed to word/main.xml, 2e9 twips of page.
    let g = good_docx();
    let renamed = rename_main(&g, "word/document.xml", "word/main.xml");
    let x = String::from_utf8(
        entries(&renamed)
            .into_iter()
            .find(|(n, _)| n == "word/main.xml")
            .unwrap()
            .1,
    )
    .unwrap();
    let huge = x.replacen(
        "</w:body>",
        "<w:sectPr><w:pgSz w:w=\"2000000000\" w:h=\"2000000000\"/></w:sectPr></w:body>",
        1,
    );
    let d = Document::open(
        &with_entry(&renamed, "word/main.xml", huge.into_bytes()),
        &l,
    )
    .unwrap();
    let r = d.render_page_png(0, 100.0);
    assert!(
        matches!(
            r,
            Err(OfficeError::LimitExceeded {
                limit: Limit::RenderPixels,
                ..
            })
        ),
        "{r:?}"
    );
    // A render copy needs a main part to check; without one it is refused.
    let r = elitea_office_core::testing::render_copy_bytes(
        &patch_text(
            &g,
            "_rels/.rels",
            "relationships/officeDocument",
            "relationships/other",
        ),
        PackageKind::Docx,
        &l,
    );
    assert!(matches!(r, Err(OfficeError::Malformed(_))), "{r:?}");
}
