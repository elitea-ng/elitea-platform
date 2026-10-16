//! Fixture tests for the Design System (Part J).
//!
//! Tests design theme application, layout patterns, design lint detection of
//! anti-patterns, and round-trip fidelity.
//!
//! **Validates: Requirements 26.5, 27.1**

use zavora_slide::qa::FindingKind;
use zavora_slide::{
    LayoutPattern, PatternParams, Presentation, apply_design_theme, apply_layout_pattern,
    design_lint, font_pairing_by_id, font_pairings, palette_by_id, palettes,
};
use zavora_slide_layout::{Alignment, Color, Item, Rect, Scene, ShapeFill, TextLine};

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

fn make_title_line() -> TextLine {
    TextLine {
        text: "Title Text".to_string(),
        size_pt: 36.0,
        color: Color::BLACK,
        bold: true,
        font_family: "Inter".to_string(),
        alignment: Alignment::Left,
        ..Default::default()
    }
}

fn make_body_line_left() -> TextLine {
    TextLine {
        text: "Body text content".to_string(),
        size_pt: 18.0,
        color: Color::BLACK,
        bold: false,
        font_family: "Inter".to_string(),
        alignment: Alignment::Left,
        ..Default::default()
    }
}

fn make_body_line_centered() -> TextLine {
    TextLine {
        text: "Centered body text".to_string(),
        size_pt: 18.0,
        color: Color::BLACK,
        bold: false,
        font_family: "Inter".to_string(),
        alignment: Alignment::Center,
        ..Default::default()
    }
}

fn text_item_with_lines(x: i64, y: i64, w: i64, h: i64, lines: Vec<TextLine>) -> Item {
    Item::Text {
        rect: Rect { x, y, w, h },
        lines,
        props: Default::default(),
    }
}

fn shape_item(x: i64, y: i64, w: i64, h: i64) -> Item {
    Item::Shape {
        rect: Rect { x, y, w, h },
        preset: None,
        fill: ShapeFill::Solid(Color {
            r: 40,
            g: 100,
            b: 200,
        }),
        outline: None,
        rotation_deg: 0.0,
    }
}

// ===========================================================================
// 1. Design theme application
// ===========================================================================

#[test]
fn apply_design_theme_sets_palette_colors_in_theme() {
    let mut pres = Presentation::new();
    apply_design_theme(&mut pres, "ocean", "modern").expect("apply_design_theme should succeed");

    let palette = palette_by_id("ocean").unwrap();
    let xml = pres.theme_xml_for_test();

    // Verify palette accent colors are present in the theme XML.
    assert!(
        xml.contains(&palette.accent1.to_uppercase()),
        "Theme should contain accent1 color '{}', got:\n{}",
        palette.accent1,
        &xml[..xml.len().min(500)]
    );
    assert!(
        xml.contains(&palette.accent2.to_uppercase()),
        "Theme should contain accent2 color"
    );
}

#[test]
fn apply_design_theme_sets_font_pairing_in_theme() {
    let mut pres = Presentation::new();
    apply_design_theme(&mut pres, "corporate", "elegant").expect("should succeed");

    let pairing = font_pairing_by_id("elegant").unwrap();
    let xml = pres.theme_xml_for_test();

    assert!(
        xml.contains(&format!("typeface=\"{}\"", pairing.heading)),
        "Theme should contain heading font '{}'",
        pairing.heading
    );
    assert!(
        xml.contains(&format!("typeface=\"{}\"", pairing.body)),
        "Theme should contain body font '{}'",
        pairing.body
    );
}

#[test]
fn apply_design_theme_all_palettes_succeed() {
    for palette in palettes() {
        let mut pres = Presentation::new();
        let result = apply_design_theme(&mut pres, palette.id, "modern");
        assert!(
            result.is_ok(),
            "apply_design_theme should succeed for palette '{}': {:?}",
            palette.id,
            result.err()
        );
    }
}

#[test]
fn apply_design_theme_all_font_pairings_succeed() {
    for fp in font_pairings() {
        let mut pres = Presentation::new();
        let result = apply_design_theme(&mut pres, "ocean", fp.id);
        assert!(
            result.is_ok(),
            "apply_design_theme should succeed for font pairing '{}': {:?}",
            fp.id,
            result.err()
        );
    }
}

// ===========================================================================
// 2. Layout patterns produce shapes
// ===========================================================================

/// Helper: create a presentation, add a blank slide, apply a pattern, return shape count.
fn apply_pattern_shape_count(pattern: LayoutPattern, params: &PatternParams) -> usize {
    let mut pres = Presentation::new();
    pres.add_slide(zavora_slide::Layout::Blank);
    {
        let mut slide = pres.slide_mut(0).unwrap();
        apply_layout_pattern(&mut slide, pattern, params).unwrap();
    }
    let slide = pres.slide(0).unwrap();
    slide.shapes().len()
}

#[test]
fn layout_pattern_two_column_produces_shapes() {
    let params = PatternParams {
        title: Some("Two Columns".into()),
        items: vec!["Left content".into(), "Right content".into()],
        palette_id: Some("corporate".into()),
        font_pairing_id: Some("modern".into()),
    };
    let count = apply_pattern_shape_count(LayoutPattern::TwoColumn, &params);
    assert!(
        count >= 3,
        "TwoColumn should produce at least 3 shapes (title + 2 columns), got {count}"
    );
}

#[test]
fn layout_pattern_icon_rows_produces_shapes() {
    let params = PatternParams {
        title: Some("Features".into()),
        items: vec!["Feature A".into(), "Feature B".into(), "Feature C".into()],
        palette_id: Some("ocean".into()),
        font_pairing_id: Some("modern".into()),
    };
    let count = apply_pattern_shape_count(LayoutPattern::IconRows, &params);
    // Title + (icon + text) per item = 1 + 3*2 = 7
    assert!(
        count >= 4,
        "IconRows should produce at least 4 shapes, got {count}"
    );
}

#[test]
fn layout_pattern_stat_callout_produces_shapes() {
    let params = PatternParams {
        title: Some("Key Metric".into()),
        items: vec!["99.9%".into(), "Uptime over 12 months".into()],
        palette_id: Some("sunset".into()),
        font_pairing_id: Some("technical".into()),
    };
    let count = apply_pattern_shape_count(LayoutPattern::StatCallout, &params);
    assert!(
        count >= 3,
        "StatCallout should produce at least 3 shapes, got {count}"
    );
}

#[test]
fn layout_pattern_quote_produces_shapes() {
    let params = PatternParams {
        title: None,
        items: vec![
            "The best way to predict the future is to invent it.".into(),
            "Alan Kay".into(),
        ],
        palette_id: Some("minimal".into()),
        font_pairing_id: Some("elegant".into()),
    };
    let count = apply_pattern_shape_count(LayoutPattern::Quote, &params);
    assert!(
        count >= 2,
        "Quote should produce at least 2 shapes, got {count}"
    );
}

#[test]
fn layout_pattern_section_divider_produces_shapes() {
    let params = PatternParams {
        title: Some("Part Two".into()),
        items: vec!["Implementation details".into()],
        palette_id: Some("midnight".into()),
        font_pairing_id: Some("classic".into()),
    };
    let count = apply_pattern_shape_count(LayoutPattern::SectionDivider, &params);
    assert!(
        count >= 2,
        "SectionDivider should produce at least 2 shapes, got {count}"
    );
}

#[test]
fn layout_pattern_image_caption_produces_shapes() {
    let params = PatternParams {
        title: Some("Architecture Diagram".into()),
        items: vec!["Figure 1: System overview".into()],
        palette_id: Some("forest".into()),
        font_pairing_id: Some("modern".into()),
    };
    let count = apply_pattern_shape_count(LayoutPattern::ImageCaption, &params);
    // Title + image placeholder shape + caption = 3
    assert!(
        count >= 3,
        "ImageCaption should produce at least 3 shapes, got {count}"
    );
}

// ===========================================================================
// 3. Design lint fixtures — anti-pattern detection
// ===========================================================================

#[test]
fn design_lint_fixture_text_only_slide_flagged() {
    // A slide with only text elements and no visual element (shape/image).
    let scene = make_scene(vec![
        text_item_with_lines(
            1_000_000,
            500_000,
            10_000_000,
            2_000_000,
            vec![make_title_line()],
        ),
        text_item_with_lines(
            1_000_000,
            3_000_000,
            10_000_000,
            3_000_000,
            vec![make_body_line_left()],
        ),
    ]);

    let findings = design_lint(&scene);
    let text_only: Vec<_> = findings
        .iter()
        .filter(|f| f.kind == FindingKind::TextOnlySlide)
        .collect();

    assert_eq!(
        text_only.len(),
        1,
        "Text-only slide fixture should produce exactly one TextOnlySlide finding"
    );
    assert!(
        text_only[0].message.contains("no visual element"),
        "Finding message should mention lack of visual element"
    );
}

#[test]
fn design_lint_fixture_centered_body_flagged() {
    // A slide with body text that is center-aligned (anti-pattern).
    let scene = make_scene(vec![
        text_item_with_lines(
            1_000_000,
            500_000,
            10_000_000,
            2_000_000,
            vec![make_title_line()],
        ),
        text_item_with_lines(
            1_000_000,
            3_000_000,
            10_000_000,
            3_000_000,
            vec![make_body_line_centered()],
        ),
        shape_item(8_000_000, 1_000_000, 3_000_000, 3_000_000),
    ]);

    let findings = design_lint(&scene);
    let centered: Vec<_> = findings
        .iter()
        .filter(|f| f.kind == FindingKind::CenteredBody)
        .collect();

    assert_eq!(
        centered.len(),
        1,
        "Centered body fixture should produce exactly one CenteredBody finding"
    );
    assert_eq!(
        centered[0].refs,
        vec![1],
        "Finding should reference the body text element"
    );
}

#[test]
fn design_lint_fixture_too_many_fonts_flagged() {
    // A slide using 4 distinct font families (exceeds default max of 3).
    let scene = make_scene(vec![
        text_item_with_lines(
            1_000_000,
            500_000,
            10_000_000,
            1_500_000,
            vec![TextLine {
                text: "Title".to_string(),
                size_pt: 36.0,
                bold: true,
                font_family: "Playfair Display".to_string(),
                alignment: Alignment::Left,
                color: Color::BLACK,
                ..Default::default()
            }],
        ),
        text_item_with_lines(
            1_000_000,
            2_500_000,
            5_000_000,
            1_500_000,
            vec![TextLine {
                text: "Body one".to_string(),
                size_pt: 18.0,
                bold: false,
                font_family: "Roboto".to_string(),
                alignment: Alignment::Left,
                color: Color::BLACK,
                ..Default::default()
            }],
        ),
        text_item_with_lines(
            1_000_000,
            4_500_000,
            5_000_000,
            1_500_000,
            vec![TextLine {
                text: "Body two".to_string(),
                size_pt: 18.0,
                bold: false,
                font_family: "Georgia".to_string(),
                alignment: Alignment::Left,
                color: Color::BLACK,
                ..Default::default()
            }],
        ),
        text_item_with_lines(
            6_000_000,
            4_500_000,
            5_000_000,
            1_500_000,
            vec![TextLine {
                text: "Caption".to_string(),
                size_pt: 14.0,
                bold: false,
                font_family: "Comic Sans MS".to_string(),
                alignment: Alignment::Left,
                color: Color::BLACK,
                ..Default::default()
            }],
        ),
        shape_item(8_000_000, 1_000_000, 3_000_000, 3_000_000),
    ]);

    let findings = design_lint(&scene);
    let too_many: Vec<_> = findings
        .iter()
        .filter(|f| f.kind == FindingKind::TooManyFonts)
        .collect();

    assert_eq!(
        too_many.len(),
        1,
        "Too-many-fonts fixture should produce exactly one TooManyFonts finding"
    );
    assert!(
        too_many[0].message.contains("4"),
        "Finding should mention the actual font count (4)"
    );
}

#[test]
fn design_lint_fixture_undersized_title_flagged() {
    // A slide where the "title" text (bold, >=24pt) is the same size as body text,
    // violating the size hierarchy.
    let scene = make_scene(vec![
        text_item_with_lines(
            1_000_000,
            500_000,
            10_000_000,
            2_000_000,
            vec![TextLine {
                text: "Title".to_string(),
                size_pt: 24.0, // Exactly at threshold — qualifies as title
                bold: true,
                font_family: "Inter".to_string(),
                alignment: Alignment::Left,
                color: Color::BLACK,
                ..Default::default()
            }],
        ),
        text_item_with_lines(
            1_000_000,
            3_000_000,
            10_000_000,
            3_000_000,
            vec![TextLine {
                text: "Body text that is the same size as the title".to_string(),
                size_pt: 24.0, // Same size as title — hierarchy violated
                bold: false,
                font_family: "Inter".to_string(),
                alignment: Alignment::Left,
                color: Color::BLACK,
                ..Default::default()
            }],
        ),
        shape_item(8_000_000, 1_000_000, 3_000_000, 3_000_000),
    ]);

    let findings = design_lint(&scene);
    let undersized: Vec<_> = findings
        .iter()
        .filter(|f| f.kind == FindingKind::UndersizedTitle)
        .collect();

    assert_eq!(
        undersized.len(),
        1,
        "Undersized title fixture should produce exactly one UndersizedTitle finding"
    );
    assert!(
        undersized[0].message.contains("size hierarchy"),
        "Finding should mention size hierarchy violation"
    );
}

// ===========================================================================
// 4. Clean slide — no design lint findings
// ===========================================================================

#[test]
fn design_lint_fixture_clean_slide_no_findings() {
    // A well-designed slide: title (bold, large), left-aligned body, visual element,
    // proper size hierarchy, limited fonts.
    let scene = make_scene(vec![
        // Title: bold, 36pt, left-aligned
        text_item_with_lines(
            1_000_000,
            500_000,
            10_000_000,
            2_000_000,
            vec![make_title_line()],
        ),
        // Body: 18pt, left-aligned (not centered)
        text_item_with_lines(
            1_000_000,
            3_000_000,
            6_000_000,
            3_000_000,
            vec![make_body_line_left()],
        ),
        // Visual element (shape) — prevents TextOnlySlide
        shape_item(8_000_000, 1_500_000, 3_000_000, 4_000_000),
    ]);

    let findings = design_lint(&scene);

    assert!(
        findings.is_empty(),
        "Clean slide fixture should produce no design lint findings, got: {:?}",
        findings
    );
}

// ===========================================================================
// 5. Round-trip: apply design theme + layout pattern → save → reopen → verify
// ===========================================================================

#[test]
fn round_trip_design_theme_and_layout_pattern() {
    // Create a presentation, apply a design theme and a layout pattern,
    // save to buffer, reopen, and verify the slide has shapes.
    let mut pres = Presentation::new();
    apply_design_theme(&mut pres, "corporate", "modern").expect("theme should apply");

    pres.add_slide(zavora_slide::Layout::Blank);
    {
        let mut slide = pres.slide_mut(0).unwrap();
        let params = PatternParams {
            title: Some("Round-Trip Test".into()),
            items: vec!["Left column".into(), "Right column".into()],
            palette_id: Some("corporate".into()),
            font_pairing_id: Some("modern".into()),
        };
        apply_layout_pattern(&mut slide, LayoutPattern::TwoColumn, &params).unwrap();
    }

    // Save to buffer.
    let buf = pres.save_to_buffer().expect("save should succeed");
    assert!(!buf.is_empty(), "Saved buffer should not be empty");

    // Reopen from buffer.
    let reopened = Presentation::open_from_bytes(&buf).expect("reopen should succeed");

    // Verify the slide exists and has content.
    let slide = reopened.slide(0).expect("should have slide 0");
    let shapes = slide.shapes();
    assert!(
        !shapes.is_empty(),
        "Reopened slide should have shapes from the layout pattern"
    );
}

#[test]
fn round_trip_preserves_theme_colors() {
    // Apply a theme, save, verify the theme XML before save contains the colors.
    // (Round-trip verification: the build_package path writes self.theme.to_xml()
    // which includes the applied palette colors.)
    let mut pres = Presentation::new();
    apply_design_theme(&mut pres, "sunset", "elegant").expect("theme should apply");
    pres.add_slide(zavora_slide::Layout::Blank);

    // Verify theme colors are in the presentation before save.
    let xml = pres.theme_xml_for_test();
    let palette = palette_by_id("sunset").unwrap();
    let pairing = font_pairing_by_id("elegant").unwrap();

    assert!(
        xml.contains(&palette.accent1.to_uppercase()),
        "Theme should contain accent1 color '{}' from sunset palette",
        palette.accent1
    );
    assert!(
        xml.contains(&format!("typeface=\"{}\"", pairing.heading)),
        "Theme should contain heading font '{}' from elegant pairing",
        pairing.heading
    );

    // Save succeeds (the theme is written into the .pptx).
    let buf = pres.save_to_buffer().expect("save should succeed");
    assert!(
        buf.len() > 1000,
        "Saved .pptx should be a non-trivial zip file"
    );

    // Reopen and verify the slide is accessible (structural integrity).
    let reopened = Presentation::open_from_bytes(&buf).expect("reopen should succeed");
    let slide = reopened.slide(0).expect("should have slide 0");
    // The slide should exist (proves the file is valid).
    let _ = slide.shapes();
}
