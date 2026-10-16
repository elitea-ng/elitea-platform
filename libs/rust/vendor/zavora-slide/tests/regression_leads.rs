//! Regression suite for the three leading capabilities.
//!
//! **Validates: Requirements 26.4**
//!
//! The engine leads python-pptx in three areas that are non-negotiable invariants:
//! 1. Render (PNG/SVG) + PDF export
//! 2. Slide delete/move/duplicate
//! 3. Byte-faithful round-trip
//!
//! These tests pin the capabilities so any regression is caught immediately.

mod test_util;

use test_util::{package_entries, reopen};
use zavora_slide::{Presentation, RenderFormat};
use zavora_slide_opc::OpcPackage;

const SAMPLE: &str = "tests/corpus/powerpoint_sample.pptx";

// ═══════════════════════════════════════════════════════════════════════════════
// 1. Render (PNG/SVG) + PDF export — regression tests
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn render_png_produces_nonempty_output() {
    let p = Presentation::open(SAMPLE).expect("open corpus deck");
    let png = p
        .render_slide(0, RenderFormat::Png)
        .expect("render slide 0 to PNG");

    // Non-empty output.
    assert!(!png.is_empty(), "PNG output must not be empty");

    // Valid PNG header: the first 8 bytes are the PNG signature.
    let png_signature: [u8; 8] = [137, 80, 78, 71, 13, 10, 26, 10];
    assert!(
        png.len() >= 8,
        "PNG output too short to contain a valid header"
    );
    assert_eq!(
        &png[..8],
        &png_signature,
        "PNG output must start with a valid PNG signature"
    );
}

#[test]
fn render_svg_produces_valid_svg() {
    let p = Presentation::open(SAMPLE).expect("open corpus deck");
    let svg_bytes = p
        .render_slide(0, RenderFormat::Svg)
        .expect("render slide 0 to SVG");

    let svg = String::from_utf8(svg_bytes).expect("SVG output must be valid UTF-8");

    // Non-empty.
    assert!(!svg.is_empty(), "SVG output must not be empty");

    // Starts with `<svg` (possibly with an XML declaration before it).
    let trimmed = svg.trim_start();
    let has_svg_root = trimmed.starts_with("<svg") || trimmed.starts_with("<?xml");
    assert!(
        has_svg_root,
        "SVG output must start with <svg or <?xml declaration, got: {:?}",
        &trimmed[..trimmed.len().min(80)]
    );

    // Contains expected SVG elements (at minimum a rect or text element from
    // the rendered slide content).
    assert!(
        svg.contains("<svg") && svg.contains("</svg>"),
        "SVG must contain opening and closing <svg> tags"
    );
}

#[test]
fn render_png_has_dark_pixels() {
    // Verify the PNG actually contains rendered content — not just a blank white
    // image. The corpus slide has text and shapes that produce dark pixels.
    let p = Presentation::open(SAMPLE).expect("open corpus deck");
    let png_bytes = p
        .render_slide(0, RenderFormat::Png)
        .expect("render slide 0 to PNG");

    // Decode the PNG to access pixel data.
    let decoder = png::Decoder::new(std::io::Cursor::new(&png_bytes));
    let mut reader = decoder.read_info().expect("decode PNG info");
    let mut buf = vec![0u8; reader.output_buffer_size()];
    let info = reader.next_frame(&mut buf).expect("decode PNG frame");
    let pixels = &buf[..info.buffer_size()];

    // Count dark pixels (R+G+B < 384, i.e. average channel < 128).
    // We expect rendered text/shapes to produce a meaningful number of dark pixels.
    let bytes_per_pixel = match info.color_type {
        png::ColorType::Rgba => 4,
        png::ColorType::Rgb => 3,
        png::ColorType::GrayscaleAlpha => 2,
        png::ColorType::Grayscale => 1,
        _ => 4, // assume RGBA
    };

    let total_pixels = pixels.len() / bytes_per_pixel;
    let mut dark_count = 0u64;

    for chunk in pixels.chunks(bytes_per_pixel) {
        let brightness: u32 = match info.color_type {
            png::ColorType::Rgba | png::ColorType::Rgb => {
                chunk[0] as u32 + chunk[1] as u32 + chunk[2] as u32
            }
            png::ColorType::GrayscaleAlpha | png::ColorType::Grayscale => chunk[0] as u32 * 3,
            _ => chunk[0] as u32 + chunk[1] as u32 + chunk[2] as u32,
        };
        if brightness < 384 {
            dark_count += 1;
        }
    }

    // At least 0.1% of pixels should be dark (text/shapes on a white background).
    let dark_ratio = dark_count as f64 / total_pixels as f64;
    assert!(
        dark_ratio > 0.001,
        "PNG should contain rendered content (dark pixels). \
         Only {:.4}% of pixels are dark ({dark_count}/{total_pixels}). \
         The render may have regressed to blank output.",
        dark_ratio * 100.0
    );
}

#[test]
fn pdf_export_produces_nonempty_output() {
    let p = Presentation::open(SAMPLE).expect("open corpus deck");
    let pdf = p.to_pdf_bytes().expect("export deck to PDF");

    // Non-empty output.
    assert!(!pdf.is_empty(), "PDF output must not be empty");

    // Valid PDF header: starts with %PDF.
    assert!(
        pdf.len() >= 5,
        "PDF output too short to contain a valid header"
    );
    assert_eq!(
        &pdf[..5],
        b"%PDF-",
        "PDF output must start with %PDF- header"
    );
}

// ═══════════════════════════════════════════════════════════════════════════════
// 2. Slide delete/move/duplicate — regression tests
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn delete_slide_removes_correct_slide() {
    // Open the 3-slide corpus deck, delete the middle slide (index 1),
    // verify remaining 2 slides have the correct content.
    let mut p = Presentation::open(SAMPLE).expect("open corpus deck");
    assert_eq!(p.slide_count(), 3, "corpus has 3 slides");

    // Capture content of slides 0 and 2 before deletion.
    let text_0 = p.slide(0).unwrap().text();
    let text_2 = p.slide(2).unwrap().text();

    // Delete middle slide.
    p.delete_slide(1).unwrap();
    assert_eq!(p.slide_count(), 2, "one slide removed");

    // Remaining slides: original 0 is still at 0, original 2 is now at 1.
    let after_0 = p.slide(0).unwrap().text();
    let after_1 = p.slide(1).unwrap().text();
    assert_eq!(
        after_0, text_0,
        "first slide content preserved after delete"
    );
    assert_eq!(
        after_1, text_2,
        "last slide content preserved (was index 2, now 1)"
    );

    // Round-trip: the saved file has exactly 2 slides.
    let out = reopen(p.save_to_buffer().unwrap());
    let pres_xml =
        String::from_utf8(out.get_part("/ppt/presentation.xml").unwrap().to_vec()).unwrap();
    assert_eq!(
        pres_xml.matches("<p:sldId ").count(),
        2,
        "presentation.xml lists exactly 2 slides after delete"
    );
}

#[test]
fn move_slide_reorders_correctly() {
    // Open the 3-slide corpus deck, move first slide to last position,
    // verify the new order.
    let mut p = Presentation::open(SAMPLE).expect("open corpus deck");
    assert_eq!(p.slide_count(), 3);

    // Capture original order by text content.
    let text_0 = p.slide(0).unwrap().text();
    let text_1 = p.slide(1).unwrap().text();
    let text_2 = p.slide(2).unwrap().text();

    // Move slide 0 to position 2 (last).
    p.move_slide(0, 2).unwrap();

    // New order: [old_1, old_2, old_0].
    let new_0 = p.slide(0).unwrap().text();
    let new_1 = p.slide(1).unwrap().text();
    let new_2 = p.slide(2).unwrap().text();
    assert_eq!(new_0, text_1, "after move: position 0 has old slide 1");
    assert_eq!(new_1, text_2, "after move: position 1 has old slide 2");
    assert_eq!(new_2, text_0, "after move: position 2 has old slide 0");

    // Still 3 slides.
    assert_eq!(p.slide_count(), 3);
}

#[test]
fn duplicate_slide_creates_copy() {
    // Duplicate slide 0, verify count increases and content matches.
    let mut p = Presentation::open(SAMPLE).expect("open corpus deck");
    let original_count = p.slide_count();
    let original_text = p.slide(0).unwrap().text();

    let new_idx = p.duplicate_slide(0).unwrap();
    assert_eq!(
        p.slide_count(),
        original_count + 1,
        "slide count increases by 1 after duplicate"
    );

    // The duplicate is inserted after the original.
    assert_eq!(new_idx, 1, "duplicate inserted at index 1");

    // The duplicate has the same text content as the original.
    let dup_text = p.slide(new_idx).unwrap().text();
    assert_eq!(
        dup_text, original_text,
        "duplicated slide has same text content as original"
    );

    // Round-trip: saved file has the correct slide count.
    let out = reopen(p.save_to_buffer().unwrap());
    let pres_xml =
        String::from_utf8(out.get_part("/ppt/presentation.xml").unwrap().to_vec()).unwrap();
    assert_eq!(
        pres_xml.matches("<p:sldId ").count(),
        original_count + 1,
        "presentation.xml lists correct slide count after duplicate"
    );
}

// ═══════════════════════════════════════════════════════════════════════════════
// 3. Byte-faithful round-trip — regression tests
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn unedited_open_save_is_byte_identical() {
    // Open the corpus deck, save without any edits, verify every single entry
    // (parts, rels, content-types) is byte-identical.
    let p = Presentation::open(SAMPLE).expect("open corpus deck");
    let orig = package_entries(&OpcPackage::open(SAMPLE).unwrap());
    let resaved = package_entries(&reopen(p.save_to_buffer().unwrap()));

    // Same set of entries.
    let orig_keys: Vec<&String> = orig.keys().collect();
    let resaved_keys: Vec<&String> = resaved.keys().collect();
    assert_eq!(
        orig_keys, resaved_keys,
        "entry set must be identical after unedited round-trip"
    );

    // Every entry byte-identical.
    for (key, orig_bytes) in &orig {
        let resaved_bytes = resaved.get(key).unwrap();
        assert_eq!(
            orig_bytes, resaved_bytes,
            "entry {key} must be byte-identical after unedited round-trip"
        );
    }
}

#[test]
fn surgical_edit_changes_only_target_part() {
    // Edit one slide's title, verify only that slide's part changes — all other
    // entries (other slides, master, layouts, theme, rels, content-types) remain
    // byte-identical.
    let mut p = Presentation::open(SAMPLE).expect("open corpus deck");
    p.slide_mut(0)
        .unwrap()
        .set_title("Regression Test Title")
        .unwrap();

    let orig = package_entries(&OpcPackage::open(SAMPLE).unwrap());
    let out = package_entries(&reopen(p.save_to_buffer().unwrap()));

    // Same entry set.
    let orig_keys: Vec<&String> = orig.keys().collect();
    let out_keys: Vec<&String> = out.keys().collect();
    assert_eq!(orig_keys, out_keys, "no entries added or removed");

    // Exactly one entry differs: the edited slide.
    let diffs: Vec<&String> = orig
        .keys()
        .filter(|k| orig.get(*k) != out.get(*k))
        .collect();
    assert_eq!(
        diffs,
        vec!["/ppt/slides/slide1.xml"],
        "only the edited slide part should change; got: {diffs:?}"
    );

    // The edit is present in the changed part.
    let edited = String::from_utf8(out["/ppt/slides/slide1.xml"].clone()).unwrap();
    assert!(
        edited.contains("Regression Test Title"),
        "edited title must appear in the changed slide part"
    );
}
