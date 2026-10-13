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
    // 150 images of 0.36 MP: 32 MP / 150 is under the floor, so the floor applies.
    let mut e = entries(&good_deck());
    let small = gray_png(600, 600);
    for i in 0..150 {
        e.push((format!("ppt/media/p{i}.png"), small.clone()));
    }
    let px = render_copy_pixels(&zip_of(&e), PackageKind::Pptx, &l).unwrap();
    assert!(
        px.iter().all(|&p| (240_000..=250_000).contains(&p)),
        "{px:?}"
    );
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
    // The total is 96 MiB: two 48 MiB sheets fit exactly, a third part does not.
    let mut e = entries(&g);
    e.retain(|(n, _)| n != MAIN_XLSX);
    e.push((MAIN_XLSX.into(), pad(47)));
    e.push(("xl/worksheets/sheet2.xml".into(), pad(47)));
    assert!(validate(&zip_of(&e), PackageKind::Xlsx, &l).is_ok());
    e.push(("xl/worksheets/sheet3.xml".into(), pad(5)));
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
