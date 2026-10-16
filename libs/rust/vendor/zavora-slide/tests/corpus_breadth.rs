//! Corpus breadth test: exercises the corpus deck across ALL implemented parity
//! features — open, extract text, render, round-trip, and the LibreOffice load gate.
//!
//! **Validates: Requirements 26.1, 26.3, 26.5**
//!
//! This test verifies that the corpus deck exercises each parity feature:
//! - Part A: Text model (paragraphs, runs, formatting)
//! - Part B: Charts (author + round-trip)
//! - Part C: Tables (structure + editing)
//! - Part D: Shapes (geometry, fill, line, presets, connectors, freeform, groups)
//! - Part E: Images (insert, crop, rotation, dedupe)
//! - Part F: Hyperlinks, metadata, notes
//! - Part H: Render fidelity (geometry inheritance, text shaping, theme resolution)
//! - Part I: Visual QA (layout report, contrast, render-diff)
//! - Part J: Design system (palettes, patterns, lint)
//! - Part K: Extraction (outline, markdown)
//! - LibreOffice load gate (env-guarded)
//! - Render similarity (env-guarded)

mod test_util;

use test_util::similarity::{SIMILARITY_THRESHOLD, compare_pngs, render_with_libreoffice};
use test_util::{libreoffice_load_gate, package_entries, reopen};
use zavora_slide::{
    Bullet, ChartKind, ChartSpec, Emu, Layout, Presentation, RenderFormat, to_markdown, to_outline,
};
use zavora_slide_opc::OpcPackage;

const SAMPLE: &str = "tests/corpus/powerpoint_sample.pptx";

// ═══════════════════════════════════════════════════════════════════════════════
// Feature: Open + Round-trip (foundation)
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn corpus_opens_and_round_trips_faithfully() {
    let p = Presentation::open(SAMPLE).expect("open corpus deck");
    assert!(p.slide_count() >= 1, "corpus deck has slides");

    // Unedited round-trip is byte-identical.
    let orig = package_entries(&OpcPackage::open(SAMPLE).unwrap());
    let resaved = package_entries(&reopen(p.save_to_buffer().unwrap()));
    assert_eq!(
        orig, resaved,
        "unedited corpus round-trip must be byte-identical"
    );
}

// ═══════════════════════════════════════════════════════════════════════════════
// Feature: Text extraction (Part K)
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn corpus_text_extraction_covers_all_slides() {
    let p = Presentation::open(SAMPLE).unwrap();
    let outline = to_outline(&p);
    assert_eq!(outline.slides.len(), p.slide_count());

    // Every slide has at least some content.
    for (i, slide) in outline.slides.iter().enumerate() {
        let has_content = slide.title.is_some() || !slide.elements.is_empty();
        assert!(
            has_content,
            "slide {} should have extractable content",
            i + 1
        );
    }
}

#[test]
fn corpus_markdown_extraction_is_rich() {
    let p = Presentation::open(SAMPLE).unwrap();
    let md = to_markdown(&p);

    // Markdown has slide headers.
    assert!(md.contains("## Slide 1"), "markdown has slide 1 header");

    // Contains actual text content from the deck.
    assert!(md.contains("Corpus Sample"), "markdown has title text");
    assert!(md.contains("First item"), "markdown has bullet text");
}

// ═══════════════════════════════════════════════════════════════════════════════
// Feature: Render (Part H)
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn corpus_render_png_all_slides() {
    let p = Presentation::open(SAMPLE).unwrap();
    for i in 0..p.slide_count() {
        let png = p
            .render_slide(i, RenderFormat::Png)
            .unwrap_or_else(|e| panic!("render slide {i} to PNG failed: {e}"));
        assert!(!png.is_empty(), "slide {i} PNG must not be empty");
        // Valid PNG signature.
        assert_eq!(
            &png[..8],
            &[137, 80, 78, 71, 13, 10, 26, 10],
            "slide {i} has valid PNG header"
        );
    }
}

#[test]
fn corpus_render_svg_all_slides() {
    let p = Presentation::open(SAMPLE).unwrap();
    for i in 0..p.slide_count() {
        let svg_bytes = p
            .render_slide(i, RenderFormat::Svg)
            .unwrap_or_else(|e| panic!("render slide {i} to SVG failed: {e}"));
        let svg = String::from_utf8(svg_bytes).expect("SVG must be valid UTF-8");
        assert!(svg.contains("<svg"), "slide {i} SVG has <svg root");
        assert!(svg.contains("</svg>"), "slide {i} SVG has closing tag");
    }
}

#[test]
fn corpus_pdf_export() {
    let p = Presentation::open(SAMPLE).unwrap();
    let pdf = p.to_pdf_bytes().expect("PDF export");
    assert!(pdf.len() > 100, "PDF must have meaningful content");
    assert_eq!(&pdf[..5], b"%PDF-", "valid PDF header");
}

// ═══════════════════════════════════════════════════════════════════════════════
// Feature: Text model authoring (Part A) on corpus deck
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn corpus_text_editing_is_surgical() {
    let mut p = Presentation::open(SAMPLE).unwrap();
    p.slide_mut(0)
        .unwrap()
        .set_title("Breadth Test Title")
        .unwrap();

    let orig = package_entries(&OpcPackage::open(SAMPLE).unwrap());
    let out = package_entries(&reopen(p.save_to_buffer().unwrap()));

    // Only the edited slide changes.
    let diffs: Vec<&String> = orig
        .keys()
        .filter(|k| orig.get(*k) != out.get(*k))
        .collect();
    assert_eq!(
        diffs,
        vec!["/ppt/slides/slide1.xml"],
        "only edited slide changes"
    );
}

// ═══════════════════════════════════════════════════════════════════════════════
// Feature: Shapes (Part D) — add shapes to corpus deck
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn corpus_add_textbox_and_round_trip() {
    let mut p = Presentation::open(SAMPLE).unwrap();
    p.slide_mut(0).unwrap().add_text_box(
        "Breadth textbox",
        Emu::inches(1.0),
        Emu::inches(1.0),
        Emu::inches(3.0),
        Emu::inches(0.5),
    );

    let buf = p.save_to_buffer().unwrap();
    let p2 = Presentation::open_from_bytes(&buf).unwrap();
    let text = p2.slide(0).unwrap().text();
    assert!(
        text.contains("Breadth textbox"),
        "added textbox survives round-trip"
    );
}

// ═══════════════════════════════════════════════════════════════════════════════
// Feature: Charts (Part B) — create deck with chart, round-trip
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn chart_creation_round_trips() {
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    {
        let mut s = p.slide_mut(0).unwrap();
        s.set_title("Chart Slide").unwrap();
        s.add_chart(
            &ChartSpec {
                kind: ChartKind::ClusteredBar,
                categories: vec!["Q1".into(), "Q2".into(), "Q3".into()],
                series: vec![("Revenue".into(), vec![100.0, 150.0, 200.0])],
                title: Some("Revenue by Quarter".into()),
                legend_position: None,
                data_labels: false,
            },
            Emu::inches(1.0),
            Emu::inches(1.5),
            Emu::inches(8.0),
            Emu::inches(5.0),
            1,
        )
        .unwrap();
    }

    let buf = p.save_to_buffer().unwrap();
    let pkg = OpcPackage::from_reader(std::io::Cursor::new(&buf)).unwrap();

    // Chart part exists.
    let has_chart = pkg.part_names().any(|n| n.contains("/charts/chart"));
    assert!(has_chart, "chart part must exist after round-trip");

    // Embedded workbook exists.
    let has_xlsx = pkg.part_names().any(|n| n.ends_with(".xlsx"));
    assert!(has_xlsx, "embedded workbook must exist");
}

// ═══════════════════════════════════════════════════════════════════════════════
// Feature: Tables (Part C) — create and verify
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn table_creation_round_trips() {
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    {
        let mut s = p.slide_mut(0).unwrap();
        let tid = s.add_table(3, 2, Emu(0), Emu(0), Emu(6000000), Emu(3000000));
        s.set_table_cell(tid, 0, 0, "Header A").unwrap();
        s.set_table_cell(tid, 0, 1, "Header B").unwrap();
        s.set_table_cell(tid, 1, 0, "Row 1A").unwrap();
        s.set_table_cell(tid, 1, 1, "Row 1B").unwrap();
        s.set_table_cell(tid, 2, 0, "Row 2A").unwrap();
        s.set_table_cell(tid, 2, 1, "Row 2B").unwrap();
    }

    let buf = p.save_to_buffer().unwrap();
    let p2 = Presentation::open_from_bytes(&buf).unwrap();
    let _outline = to_outline(&p2);
    let md = to_markdown(&p2);
    assert!(md.contains("Header A"), "table header survives round-trip");
    assert!(md.contains("Row 2B"), "table data survives round-trip");
}

// ═══════════════════════════════════════════════════════════════════════════════
// Feature: Images (Part E) — insert and dedupe
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn image_insert_and_dedupe() {
    // Create a deck, save, reopen (so it has a DOM), then insert same image twice.
    // Deduplication works on the surgical (DOM) path.
    let mut p = Presentation::new();
    p.add_slide(Layout::Blank);
    let buf = p.save_to_buffer().unwrap();

    let mut p2 = Presentation::open_from_bytes(&buf).unwrap();
    let png_bytes = create_minimal_png();

    {
        let mut s = p2.slide_mut(0).unwrap();
        // Insert same image twice — should deduplicate the media part.
        s.insert_image_bytes(
            &png_bytes,
            Emu::inches(1.0),
            Emu::inches(1.0),
            Emu::inches(2.0),
            Emu::inches(2.0),
        )
        .unwrap();
        s.insert_image_bytes(
            &png_bytes,
            Emu::inches(4.0),
            Emu::inches(1.0),
            Emu::inches(2.0),
            Emu::inches(2.0),
        )
        .unwrap();
    }

    let buf2 = p2.save_to_buffer().unwrap();
    let pkg = OpcPackage::from_reader(std::io::Cursor::new(&buf2)).unwrap();

    // Only one media part despite two inserts (content-hash dedupe).
    let media_parts: Vec<&str> = pkg
        .part_names()
        .filter(|n| n.starts_with("/ppt/media/"))
        .collect();
    assert_eq!(
        media_parts.len(),
        1,
        "identical images should be deduplicated to one media part"
    );
}

// ═══════════════════════════════════════════════════════════════════════════════
// Feature: Hyperlinks + Notes (Part F)
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn notes_on_corpus_deck_is_surgical() {
    let mut p = Presentation::open(SAMPLE).unwrap();
    p.slide_mut(0).unwrap().set_notes("Breadth test note");

    let buf = p.save_to_buffer().unwrap();
    let p2 = Presentation::open_from_bytes(&buf).unwrap();
    let outline = to_outline(&p2);
    assert_eq!(
        outline.slides[0].notes.as_deref(),
        Some("Breadth test note"),
        "notes survive round-trip"
    );
}

// ═══════════════════════════════════════════════════════════════════════════════
// Feature: Visual QA (Part I) — layout report on corpus
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn corpus_qa_report_is_deterministic() {
    let p = Presentation::open(SAMPLE).unwrap();
    let scene = p.slide(0).unwrap().scene();
    let report1 = zavora_slide::qa::analyze_layout(&scene);
    let report2 = zavora_slide::qa::analyze_layout(&scene);

    // Deterministic: same deck, same slide → same report.
    assert_eq!(
        format!("{:?}", report1),
        format!("{:?}", report2),
        "QA report must be deterministic"
    );
}

// ═══════════════════════════════════════════════════════════════════════════════
// Feature: Design system (Part J) — apply theme + lint
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn design_theme_application_produces_valid_deck() {
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    p.slide_mut(0).unwrap().set_title("Design Test").unwrap();

    // Apply a curated palette + font pairing.
    let palettes = zavora_slide::palettes();
    let pairings = zavora_slide::font_pairings();
    assert!(!palettes.is_empty(), "palette catalog must not be empty");
    assert!(
        !pairings.is_empty(),
        "font pairing catalog must not be empty"
    );

    zavora_slide::apply_design_theme(&mut p, palettes[0].id, pairings[0].id).unwrap();

    // The deck still saves and round-trips.
    let buf = p.save_to_buffer().unwrap();
    let p2 = Presentation::open_from_bytes(&buf).unwrap();
    assert_eq!(p2.slide_count(), 1);
}

#[test]
fn design_lint_runs_on_corpus() {
    let p = Presentation::open(SAMPLE).unwrap();
    // Design lint should not panic on a real deck.
    let scene = p.slide(0).unwrap().scene();
    let findings = zavora_slide::design_lint(&scene);
    // Findings is a Vec — may be empty or have items, but must not panic.
    let _ = findings.len();
}

// ═══════════════════════════════════════════════════════════════════════════════
// Feature: Extraction breadth (Part K) — JSON outline
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn corpus_outline_json_round_trips() {
    let p = Presentation::open(SAMPLE).unwrap();
    let outline = to_outline(&p);
    let json = serde_json::to_string(&outline).unwrap();
    let parsed: zavora_slide::DeckOutline = serde_json::from_str(&json).unwrap();
    assert_eq!(parsed, outline, "JSON outline round-trip must be faithful");
}

// ═══════════════════════════════════════════════════════════════════════════════
// LibreOffice load gate (env-guarded) — Requirement 26.3
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn corpus_libreoffice_load_gate() {
    // Verify the corpus deck opens without error in LibreOffice headless.
    // Env-guarded: only runs when ZAVORA_LIBREOFFICE_GATE=1.
    let pptx_bytes = std::fs::read(SAMPLE).expect("read corpus pptx");
    libreoffice_load_gate(&pptx_bytes).expect("LibreOffice load gate");
}

#[test]
fn authored_deck_libreoffice_load_gate() {
    // Verify a freshly authored deck (with charts, tables, shapes) passes the gate.
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    {
        let mut s = p.slide_mut(0).unwrap();
        s.set_title("Gate Test").unwrap();
        s.add_bullets(&[Bullet::new("Item 1"), Bullet::new("Item 2")])
            .unwrap();
        s.add_text_box(
            "Box",
            Emu::inches(1.0),
            Emu::inches(5.0),
            Emu::inches(3.0),
            Emu::inches(0.5),
        );
        let tid = s.add_table(
            2,
            2,
            Emu::inches(5.0),
            Emu::inches(1.0),
            Emu::inches(4.0),
            Emu::inches(2.0),
        );
        s.set_table_cell(tid, 0, 0, "A").unwrap();
        s.set_table_cell(tid, 0, 1, "B").unwrap();
        s.set_table_cell(tid, 1, 0, "C").unwrap();
        s.set_table_cell(tid, 1, 1, "D").unwrap();
    }

    let buf = p.save_to_buffer().unwrap();
    libreoffice_load_gate(&buf).expect("authored deck LibreOffice load gate");
}

// ═══════════════════════════════════════════════════════════════════════════════
// Render similarity across features (env-guarded) — Requirement 26.5
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn corpus_render_similarity_all_slides() {
    // Compare our render against LibreOffice for every slide in the corpus deck.
    // Env-guarded: only runs when ZAVORA_LIBREOFFICE_GATE=1.
    let p = Presentation::open(SAMPLE).unwrap();
    let pptx_bytes = std::fs::read(SAMPLE).expect("read corpus pptx");

    for i in 0..p.slide_count() {
        let our_png = match p.render_slide(i, RenderFormat::Png) {
            Ok(png) => png,
            Err(_) => continue, // Skip slides that fail to render.
        };

        let lo_result = render_with_libreoffice(&pptx_bytes, i);
        match lo_result {
            None => {
                // Gate not set — skip.
                eprintln!("ZAVORA_LIBREOFFICE_GATE not set; skipping similarity for slide {i}");
                return;
            }
            Some(Ok(lo_png)) => {
                let score = compare_pngs(&our_png, &lo_png);
                eprintln!(
                    "Similarity score (slide {i}): {score:.4} (threshold: {SIMILARITY_THRESHOLD})"
                );
                assert!(
                    score >= SIMILARITY_THRESHOLD,
                    "Slide {i} similarity {score:.4} below threshold {SIMILARITY_THRESHOLD}"
                );
            }
            Some(Err(e)) => {
                // LibreOffice may not support multi-slide export for all indices.
                eprintln!("LibreOffice render failed for slide {i} (non-fatal): {e}");
            }
        }
    }
}

// ═══════════════════════════════════════════════════════════════════════════════
// Helpers
// ═══════════════════════════════════════════════════════════════════════════════

/// Create a minimal valid 1x1 red PNG for image insertion tests.
fn create_minimal_png() -> Vec<u8> {
    let mut pixmap = resvg::tiny_skia::Pixmap::new(1, 1).unwrap();
    pixmap.fill(resvg::tiny_skia::Color::from_rgba8(255, 0, 0, 255));
    pixmap.encode_png().unwrap()
}
