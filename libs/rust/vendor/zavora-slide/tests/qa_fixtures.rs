//! Fixture tests with known defects for the Visual QA system.
//!
//! Each fixture creates a Scene with a specific known defect and verifies that
//! the QA analysis correctly detects and reports it.
//!
//! **Validates: Requirements 28.5, 29.1**

use zavora_slide::qa::{
    ContrastConfig, FindingKind, Severity, analyze_layout, check_contrast, compute_render_diff,
};
use zavora_slide_layout::{Color, Item, Rect, Scene, ShapeFill, TextLine};

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/// Standard 16:9 slide dimensions in EMU.
const SLIDE_W: i64 = 12_192_000;
const SLIDE_H: i64 = 6_858_000;

fn make_scene(items: Vec<Item>) -> Scene {
    Scene {
        width_emu: SLIDE_W,
        height_emu: SLIDE_H,
        background: None,
        rich_background: None,
        items,
        item_sources: Vec::new(),
    }
}

fn text_item(x: i64, y: i64, w: i64, h: i64) -> Item {
    Item::Text {
        rect: Rect { x, y, w, h },
        lines: vec![],
        props: Default::default(),
    }
}

fn shape_item(x: i64, y: i64, w: i64, h: i64) -> Item {
    Item::Shape {
        rect: Rect { x, y, w, h },
        preset: None,
        fill: ShapeFill::None,
        outline: None,
        rotation_deg: 0.0,
    }
}

fn text_item_with_lines(x: i64, y: i64, w: i64, h: i64, lines: Vec<TextLine>) -> Item {
    Item::Text {
        rect: Rect { x, y, w, h },
        lines,
        props: Default::default(),
    }
}

fn make_text_line(size_pt: f64, color: Color) -> TextLine {
    TextLine {
        text: "Sample text".to_string(),
        size_pt,
        color,
        ..Default::default()
    }
}

/// Create a minimal valid PNG filled with a solid color.
fn make_solid_png(width: u32, height: u32, r: u8, g: u8, b: u8) -> Vec<u8> {
    let mut buf = Vec::new();
    {
        let mut encoder = png::Encoder::new(&mut buf, width, height);
        encoder.set_color(png::ColorType::Rgba);
        encoder.set_depth(png::BitDepth::Eight);
        let mut writer = encoder.write_header().unwrap();
        let pixel_count = (width * height) as usize;
        let mut data = vec![0u8; pixel_count * 4];
        for pixel in data.chunks_mut(4) {
            pixel[0] = r;
            pixel[1] = g;
            pixel[2] = b;
            pixel[3] = 255;
        }
        writer.write_image_data(&data).unwrap();
    }
    buf
}

/// Create a PNG with a colored rectangle drawn in a specific region.
#[allow(clippy::too_many_arguments)]
fn make_png_with_rect(
    width: u32,
    height: u32,
    bg_r: u8,
    bg_g: u8,
    bg_b: u8,
    rect_x: u32,
    rect_y: u32,
    rect_w: u32,
    rect_h: u32,
    rect_r: u8,
    rect_g: u8,
    rect_b: u8,
) -> Vec<u8> {
    let mut buf = Vec::new();
    {
        let mut encoder = png::Encoder::new(&mut buf, width, height);
        encoder.set_color(png::ColorType::Rgba);
        encoder.set_depth(png::BitDepth::Eight);
        let mut writer = encoder.write_header().unwrap();
        let pixel_count = (width * height) as usize;
        let mut data = vec![0u8; pixel_count * 4];
        // Fill background
        for pixel in data.chunks_mut(4) {
            pixel[0] = bg_r;
            pixel[1] = bg_g;
            pixel[2] = bg_b;
            pixel[3] = 255;
        }
        // Draw rectangle
        for row in rect_y..(rect_y + rect_h).min(height) {
            for col in rect_x..(rect_x + rect_w).min(width) {
                let idx = ((row * width + col) as usize) * 4;
                data[idx] = rect_r;
                data[idx + 1] = rect_g;
                data[idx + 2] = rect_b;
                data[idx + 3] = 255;
            }
        }
        writer.write_image_data(&data).unwrap();
    }
    buf
}

// ===========================================================================
// Fixture 1: Off-canvas element
// ===========================================================================

#[test]
fn fixture_off_canvas_element_detected() {
    // A shape that extends 500_000 EMU past the right edge of the slide.
    let scene = make_scene(vec![shape_item(
        SLIDE_W - 1_000_000, // x: starts 1M EMU from right edge
        2_000_000,           // y: vertically centered-ish
        1_500_000,           // w: extends 500K past right edge
        2_000_000,           // h: normal height
    )]);

    let report = analyze_layout(&scene);

    let off_canvas: Vec<_> = report
        .findings
        .iter()
        .filter(|f| f.kind == FindingKind::OffCanvas)
        .collect();

    assert_eq!(
        off_canvas.len(),
        1,
        "Expected exactly one OffCanvas finding for element extending past right edge"
    );
    assert_eq!(off_canvas[0].refs, vec![0]);
    assert_eq!(off_canvas[0].severity, Severity::Warning);
    assert!(
        off_canvas[0].message.contains("beyond slide bounds"),
        "Message should describe the off-canvas condition"
    );
}

// ===========================================================================
// Fixture 2: Overlapping text elements
// ===========================================================================

#[test]
fn fixture_overlapping_text_elements_detected() {
    // Two text boxes that significantly overlap (same position, same size).
    let scene = make_scene(vec![
        text_item(2_000_000, 2_000_000, 4_000_000, 2_000_000),
        text_item(2_500_000, 2_500_000, 4_000_000, 2_000_000),
    ]);

    let report = analyze_layout(&scene);

    let overlaps: Vec<_> = report
        .findings
        .iter()
        .filter(|f| f.kind == FindingKind::Overlap)
        .collect();

    assert_eq!(
        overlaps.len(),
        1,
        "Expected exactly one Overlap finding for two significantly overlapping text elements"
    );
    assert_eq!(overlaps[0].refs, vec![0, 1]);
    assert_eq!(overlaps[0].severity, Severity::Warning);
    assert!(
        overlaps[0].message.contains("text-over-text"),
        "Overlap classification should be text-over-text"
    );
}

// ===========================================================================
// Fixture 3: Zero-area element
// ===========================================================================

#[test]
fn fixture_zero_area_element_detected() {
    // A text element with zero width (degenerate box).
    let scene = make_scene(vec![text_item(
        3_000_000, // x: well within slide
        3_000_000, // y: well within slide
        0,         // w: ZERO — this is the defect
        2_000_000, // h: normal height
    )]);

    let report = analyze_layout(&scene);

    let zero_area: Vec<_> = report
        .findings
        .iter()
        .filter(|f| f.kind == FindingKind::ZeroArea)
        .collect();

    assert_eq!(
        zero_area.len(),
        1,
        "Expected exactly one ZeroArea finding for element with zero width"
    );
    assert_eq!(zero_area[0].refs, vec![0]);
    assert_eq!(zero_area[0].severity, Severity::Error);
    assert!(
        zero_area[0].message.contains("zero"),
        "Message should mention zero area"
    );
}

// ===========================================================================
// Fixture 4: Margin violation
// ===========================================================================

#[test]
fn fixture_margin_violation_detected() {
    // An element placed very close to the top-left corner (within 5% margin).
    // 5% of SLIDE_W = 609_600 EMU, 5% of SLIDE_H = 342_900 EMU.
    // Place element at (50_000, 50_000) — well within the margin threshold.
    let scene = make_scene(vec![text_item(
        50_000,    // x: very close to left edge (< 609_600)
        50_000,    // y: very close to top edge (< 342_900)
        2_000_000, // w: moderate width
        1_000_000, // h: moderate height
    )]);

    let report = analyze_layout(&scene);

    let margins: Vec<_> = report
        .findings
        .iter()
        .filter(|f| f.kind == FindingKind::MarginViolation)
        .collect();

    assert_eq!(
        margins.len(),
        1,
        "Expected exactly one MarginViolation finding for element too close to slide edges"
    );
    assert_eq!(margins[0].refs, vec![0]);
    assert_eq!(margins[0].severity, Severity::Info);
    assert!(
        margins[0].message.contains("margin"),
        "Message should mention margin"
    );
}

// ===========================================================================
// Fixture 5: Low-contrast text
// ===========================================================================

#[test]
fn fixture_low_contrast_text_detected() {
    // Light gray text (#C8C8C8) on white background — contrast ratio ≈ 1.6:1
    // (well below the 4.5:1 WCAG minimum).
    let light_gray = Color {
        r: 200,
        g: 200,
        b: 200,
    };
    let scene = make_scene(vec![text_item_with_lines(
        2_000_000,
        2_000_000,
        4_000_000,
        2_000_000,
        vec![make_text_line(18.0, light_gray)],
    )]);

    let config = ContrastConfig::default();
    let findings = check_contrast(&scene, &config);

    let low_contrast: Vec<_> = findings
        .iter()
        .filter(|f| f.kind == FindingKind::LowContrast)
        .collect();

    assert_eq!(
        low_contrast.len(),
        1,
        "Expected exactly one LowContrast finding for light gray text on white background"
    );
    assert_eq!(low_contrast[0].severity, Severity::Warning);
    assert_eq!(low_contrast[0].refs, vec![0]);
    assert!(
        low_contrast[0].message.contains("contrast ratio"),
        "Message should mention contrast ratio"
    );
}

// ===========================================================================
// Fixture 6: Small font size
// ===========================================================================

#[test]
fn fixture_small_font_size_detected() {
    // Text at 6pt — below the default 10pt minimum.
    let scene = make_scene(vec![text_item_with_lines(
        2_000_000,
        2_000_000,
        4_000_000,
        2_000_000,
        vec![make_text_line(6.0, Color::BLACK)],
    )]);

    let config = ContrastConfig::default();
    let findings = check_contrast(&scene, &config);

    let small_font: Vec<_> = findings
        .iter()
        .filter(|f| f.kind == FindingKind::SmallFont)
        .collect();

    assert_eq!(
        small_font.len(),
        1,
        "Expected exactly one SmallFont finding for 6pt text"
    );
    assert_eq!(small_font[0].severity, Severity::Warning);
    assert_eq!(small_font[0].refs, vec![0]);
    assert!(
        small_font[0].message.contains("font size"),
        "Message should mention font size"
    );
}

// ===========================================================================
// Fixture 7: No defects — clean report
// ===========================================================================

#[test]
fn fixture_no_defects_clean_report() {
    // A well-placed element: centered, good size, no overlaps, no edge issues.
    // Position: well within margins, normal dimensions, black text on white bg.
    let scene = make_scene(vec![text_item_with_lines(
        2_000_000, // x: well within 5% margin (609_600)
        2_000_000, // y: well within 5% margin (342_900)
        4_000_000, // w: moderate
        2_000_000, // h: moderate
        vec![make_text_line(18.0, Color::BLACK)],
    )]);

    // Layout analysis should produce no findings.
    let report = analyze_layout(&scene);
    assert!(
        report.findings.is_empty(),
        "Expected no layout findings for a well-placed element, got: {:?}",
        report.findings
    );

    // Contrast check should produce no findings (black on white = 21:1).
    let config = ContrastConfig::default();
    let contrast_findings = check_contrast(&scene, &config);
    assert!(
        contrast_findings.is_empty(),
        "Expected no contrast findings for black text on white background, got: {:?}",
        contrast_findings
    );
}

// ===========================================================================
// Fixture 8: Render-diff detects visual change
// ===========================================================================

#[test]
fn fixture_render_diff_detects_change() {
    // Create two slightly different scenes rendered as PNGs:
    // - "before": solid gray 64×64
    // - "after": gray with a red rectangle in the top-left quadrant
    let before_png = make_solid_png(64, 64, 128, 128, 128);
    let after_png = make_png_with_rect(
        64, 64, // dimensions
        128, 128, 128, // background (same gray)
        0, 0, 32, 32, // rectangle position and size (top-left quadrant)
        255, 0, 0, // rectangle color (red)
    );

    let diff = compute_render_diff(&before_png, &after_png, 32).unwrap();

    // The diff should detect the change.
    assert!(
        diff.total_change_fraction > 0.0,
        "Expected non-zero total change fraction when images differ"
    );

    // Only the top-left tile (0,0) should be flagged as changed.
    assert_eq!(
        diff.changed_regions.len(),
        1,
        "Expected exactly one changed region (top-left tile)"
    );

    let region = &diff.changed_regions[0];
    assert_eq!(region.x, 0, "Changed region should be at x=0");
    assert_eq!(region.y, 0, "Changed region should be at y=0");
    assert_eq!(region.w, 32, "Changed region width should be 32");
    assert_eq!(region.h, 32, "Changed region height should be 32");
    assert!(
        (region.change_fraction - 1.0).abs() < 1e-9,
        "Entire top-left tile should be changed (fraction=1.0)"
    );

    // Total change should be 25% (one quadrant of four).
    assert!(
        (diff.total_change_fraction - 0.25).abs() < 0.01,
        "Expected ~25% total change, got {}",
        diff.total_change_fraction
    );
}

#[test]
fn fixture_render_diff_identical_scenes_no_change() {
    // Two identical PNGs should produce zero diff.
    let png = make_solid_png(64, 64, 100, 150, 200);

    let diff = compute_render_diff(&png, &png, 16).unwrap();

    assert!(
        (diff.total_change_fraction - 0.0).abs() < 1e-9,
        "Identical images should have zero change fraction"
    );
    assert!(
        diff.changed_regions.is_empty(),
        "Identical images should have no changed regions"
    );
}
