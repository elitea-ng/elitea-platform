//! Design system: curated palettes and font pairings catalog.
//!
//! Provides a data-driven, enumerable catalog of named color palettes and
//! heading/body font pairings. The `apply_design_theme` function maps a palette
//! and font pairing onto the deck theme's `clrScheme` and `fontScheme`.

use crate::error::{Result, SlideError};
use crate::presentation::Presentation;
use crate::theme::ThemeSpec;

/// A curated color palette with scheme colors and intended tone.
#[derive(Debug, Clone, Copy)]
pub struct Palette {
    /// Unique identifier (e.g. "ocean", "corporate").
    pub id: &'static str,
    /// Human-readable display name.
    pub name: &'static str,
    /// Primary dark color (dk2) — hex without `#`.
    pub primary: &'static str,
    /// Secondary light color (lt2) — hex without `#`.
    pub secondary: &'static str,
    /// Accent colors 1–6 — hex without `#`.
    pub accent1: &'static str,
    pub accent2: &'static str,
    pub accent3: &'static str,
    pub accent4: &'static str,
    pub accent5: &'static str,
    pub accent6: &'static str,
    /// Intended tone descriptor (e.g. "professional", "creative").
    pub tone: &'static str,
}

/// A curated heading/body font pairing.
#[derive(Debug, Clone, Copy)]
pub struct FontPairing {
    /// Unique identifier (e.g. "modern", "classic").
    pub id: &'static str,
    /// Human-readable display name.
    pub name: &'static str,
    /// Heading (major) font family.
    pub heading: &'static str,
    /// Body (minor) font family.
    pub body: &'static str,
}

// ---------------------------------------------------------------------------
// Palette catalog
// ---------------------------------------------------------------------------

static PALETTES: &[Palette] = &[
    Palette {
        id: "ocean",
        name: "Ocean",
        primary: "1B3A5C",
        secondary: "E8F1F8",
        accent1: "2E86AB",
        accent2: "A23B72",
        accent3: "F18F01",
        accent4: "C73E1D",
        accent5: "3B1F2B",
        accent6: "44BBA4",
        tone: "professional",
    },
    Palette {
        id: "sunset",
        name: "Sunset",
        primary: "2D2D2A",
        secondary: "FFF8F0",
        accent1: "E63946",
        accent2: "F4A261",
        accent3: "E9C46A",
        accent4: "2A9D8F",
        accent5: "264653",
        accent6: "F77F00",
        tone: "warm",
    },
    Palette {
        id: "forest",
        name: "Forest",
        primary: "1B2D1B",
        secondary: "F0F5EC",
        accent1: "2D6A4F",
        accent2: "40916C",
        accent3: "52B788",
        accent4: "74C69D",
        accent5: "95D5B2",
        accent6: "D8F3DC",
        tone: "natural",
    },
    Palette {
        id: "corporate",
        name: "Corporate",
        primary: "1F2937",
        secondary: "F3F4F6",
        accent1: "2563EB",
        accent2: "3B82F6",
        accent3: "60A5FA",
        accent4: "10B981",
        accent5: "6366F1",
        accent6: "8B5CF6",
        tone: "professional",
    },
    Palette {
        id: "minimal",
        name: "Minimal",
        primary: "111827",
        secondary: "F9FAFB",
        accent1: "374151",
        accent2: "6B7280",
        accent3: "9CA3AF",
        accent4: "D1D5DB",
        accent5: "E5E7EB",
        accent6: "1F2937",
        tone: "cool",
    },
    Palette {
        id: "vibrant",
        name: "Vibrant",
        primary: "1A1A2E",
        secondary: "FEFEFE",
        accent1: "E94560",
        accent2: "0F3460",
        accent3: "16213E",
        accent4: "533483",
        accent5: "E94560",
        accent6: "F5A623",
        tone: "creative",
    },
    Palette {
        id: "earth",
        name: "Earth",
        primary: "3D2C2E",
        secondary: "FAF3E0",
        accent1: "B85C38",
        accent2: "E0C097",
        accent3: "5C3D2E",
        accent4: "2D4A3E",
        accent5: "8B5E3C",
        accent6: "D4A574",
        tone: "warm",
    },
    Palette {
        id: "midnight",
        name: "Midnight",
        primary: "0D1B2A",
        secondary: "E0E1DD",
        accent1: "1B263B",
        accent2: "415A77",
        accent3: "778DA9",
        accent4: "E0E1DD",
        accent5: "0D1B2A",
        accent6: "1B263B",
        tone: "cool",
    },
];

// ---------------------------------------------------------------------------
// Font pairing catalog
// ---------------------------------------------------------------------------

static FONT_PAIRINGS: &[FontPairing] = &[
    FontPairing {
        id: "modern",
        name: "Modern",
        heading: "Inter",
        body: "Inter",
    },
    FontPairing {
        id: "classic",
        name: "Classic",
        heading: "Georgia",
        body: "Garamond",
    },
    FontPairing {
        id: "technical",
        name: "Technical",
        heading: "Roboto",
        body: "Roboto Mono",
    },
    FontPairing {
        id: "elegant",
        name: "Elegant",
        heading: "Playfair Display",
        body: "Lato",
    },
    FontPairing {
        id: "playful",
        name: "Playful",
        heading: "Poppins",
        body: "Nunito",
    },
    FontPairing {
        id: "minimal",
        name: "Minimal",
        heading: "Helvetica Neue",
        body: "Helvetica Neue",
    },
];

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

/// Returns the full catalog of curated palettes.
pub fn palettes() -> &'static [Palette] {
    PALETTES
}

/// Look up a palette by its unique id.
pub fn palette_by_id(id: &str) -> Option<&'static Palette> {
    PALETTES.iter().find(|p| p.id == id)
}

/// Returns the full catalog of curated font pairings.
pub fn font_pairings() -> &'static [FontPairing] {
    FONT_PAIRINGS
}

/// Look up a font pairing by its unique id.
pub fn font_pairing_by_id(id: &str) -> Option<&'static FontPairing> {
    FONT_PAIRINGS.iter().find(|fp| fp.id == id)
}

/// Apply a named palette and font pairing to a presentation's theme.
///
/// Maps the palette colors onto the theme's `clrScheme` (dk2, lt2, accent1–6,
/// hlink, folHlink) and the font pairing onto the theme's `fontScheme`
/// (major/minor Latin typefaces).
///
/// Returns an error if either the palette id or font pairing id is not found
/// in the catalog.
pub fn apply_design_theme(
    presentation: &mut Presentation,
    palette_id: &str,
    font_pairing_id: &str,
) -> Result<()> {
    let palette = palette_by_id(palette_id)
        .ok_or_else(|| SlideError::NotFound(format!("palette not found: '{palette_id}'")))?;
    let pairing = font_pairing_by_id(font_pairing_id).ok_or_else(|| {
        SlideError::NotFound(format!("font pairing not found: '{font_pairing_id}'"))
    })?;

    let mut spec = ThemeSpec::default();

    // Map palette onto scheme colors.
    spec.colors.insert("dk2".into(), palette.primary.into());
    spec.colors.insert("lt2".into(), palette.secondary.into());
    spec.colors.insert("accent1".into(), palette.accent1.into());
    spec.colors.insert("accent2".into(), palette.accent2.into());
    spec.colors.insert("accent3".into(), palette.accent3.into());
    spec.colors.insert("accent4".into(), palette.accent4.into());
    spec.colors.insert("accent5".into(), palette.accent5.into());
    spec.colors.insert("accent6".into(), palette.accent6.into());
    // Use accent1 as hyperlink color, accent4 as followed hyperlink.
    spec.colors.insert("hlink".into(), palette.accent1.into());
    spec.colors
        .insert("folHlink".into(), palette.accent4.into());

    // Map font pairing onto theme fonts.
    spec.heading_font = Some(pairing.heading.into());
    spec.body_font = Some(pairing.body.into());

    presentation.apply_theme(&spec);
    Ok(())
}

// ---------------------------------------------------------------------------
// Layout patterns (Req 23.1, 23.2)
// ---------------------------------------------------------------------------

/// Available layout patterns for slide composition.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LayoutPattern {
    /// Two text columns side by side.
    TwoColumn,
    /// Rows of icon+text pairs.
    IconRows,
    /// Large number/stat with supporting text.
    StatCallout,
    /// Centered quote with attribution.
    Quote,
    /// Full-bleed title for section breaks.
    SectionDivider,
    /// Image placeholder with caption text below.
    ImageCaption,
}

/// Parameters for applying a layout pattern to a slide.
#[derive(Debug, Clone, Default)]
pub struct PatternParams {
    /// Optional title text for the pattern.
    pub title: Option<String>,
    /// Content items — meaning depends on the pattern:
    /// - TwoColumn: alternating left/right items (even indices = left, odd = right)
    /// - IconRows: each item is "icon_text|description" or just text
    /// - StatCallout: first item is the stat number, second is supporting text
    /// - Quote: first item is the quote, second is attribution
    /// - SectionDivider: first item is subtitle (title field is the main heading)
    /// - ImageCaption: first item is caption text
    pub items: Vec<String>,
    /// Palette id to use (looks up from catalog; uses defaults if not found).
    pub palette_id: Option<String>,
    /// Font pairing id to use (looks up from catalog; uses defaults if not found).
    pub font_pairing_id: Option<String>,
}

/// Apply a layout pattern to a slide, emitting correctly positioned shapes.
///
/// Enforces:
/// - Minimum 5% margins from all edges
/// - Size hierarchy: title font > body font > caption font
/// - Left-aligned body text (no centered body — anti-pattern)
/// - No accent underlines beneath titles (anti-pattern)
///
/// Uses the active palette/font pairing if specified in params.
pub fn apply_layout_pattern(
    slide: &mut crate::slide::Slide<'_>,
    pattern: LayoutPattern,
    params: &PatternParams,
) -> Result<()> {
    let slide_cx = slide.slide_cx;
    let slide_cy = slide.slide_cy;

    // Resolve fonts from pairing (defaults: heading=Inter, body=Inter)
    let (heading_font, body_font) = resolve_fonts(params);

    // Resolve colors from palette (defaults: dark text, accent for highlights)
    let (title_color, body_color, accent_color) = resolve_colors(params);

    // Minimum 5% margins from edges
    let margin_x = slide_cx * 5 / 100;
    let margin_y = slide_cy * 5 / 100;
    let content_w = slide_cx - 2 * margin_x;
    let content_h = slide_cy - 2 * margin_y;

    let ctx = LayoutCtx {
        margin_x,
        margin_y,
        content_w,
        content_h,
        heading_font,
        body_font,
        title_color,
        body_color,
        accent_color,
        title_size: 36.0,
        body_size: 18.0,
        caption_size: 14.0,
    };

    match pattern {
        LayoutPattern::TwoColumn => emit_two_column(slide, params, &ctx),
        LayoutPattern::IconRows => emit_icon_rows(slide, params, &ctx),
        LayoutPattern::StatCallout => emit_stat_callout(slide, params, &ctx),
        LayoutPattern::Quote => emit_quote(slide, params, &ctx),
        LayoutPattern::SectionDivider => emit_section_divider(slide, params, &ctx),
        LayoutPattern::ImageCaption => emit_image_caption(slide, params, &ctx),
    }

    Ok(())
}

// ---------------------------------------------------------------------------
// Internal helpers for layout patterns
// ---------------------------------------------------------------------------

fn resolve_fonts(params: &PatternParams) -> (String, String) {
    let pairing = params
        .font_pairing_id
        .as_deref()
        .and_then(font_pairing_by_id);
    match pairing {
        Some(fp) => (fp.heading.to_string(), fp.body.to_string()),
        None => ("Inter".to_string(), "Inter".to_string()),
    }
}

fn resolve_colors(params: &PatternParams) -> (String, String, String) {
    let palette = params.palette_id.as_deref().and_then(palette_by_id);
    match palette {
        Some(p) => (
            p.primary.to_string(),
            p.primary.to_string(),
            p.accent1.to_string(),
        ),
        None => (
            "1F2937".to_string(), // dark gray for title
            "374151".to_string(), // medium gray for body
            "2563EB".to_string(), // blue accent
        ),
    }
}

/// Resolved layout context passed to pattern emitters.
struct LayoutCtx {
    margin_x: i64,
    margin_y: i64,
    content_w: i64,
    content_h: i64,
    heading_font: String,
    body_font: String,
    title_color: String,
    body_color: String,
    accent_color: String,
    title_size: f64,
    body_size: f64,
    caption_size: f64,
}

/// Emit a two-column layout: optional title at top, two text columns below.
fn emit_two_column(slide: &mut crate::slide::Slide<'_>, params: &PatternParams, ctx: &LayoutCtx) {
    use crate::units::Emu;
    use zavora_slide_oxml::Align;

    let mut y_cursor = ctx.margin_y;

    // Title (if provided)
    if let Some(title) = &params.title {
        let title_h = Emu::points(ctx.title_size).0 * 2; // ~2 lines height
        let sp = slide.add_text_box(
            title,
            Emu(ctx.margin_x),
            Emu(y_cursor),
            Emu(ctx.content_w),
            Emu(title_h),
        );
        sp.size(ctx.title_size)
            .font(&ctx.heading_font)
            .color(&ctx.title_color)
            .bold(true)
            .align(Align::Left);
        y_cursor += title_h + ctx.margin_y / 2;
    }

    // Two columns with a gap
    let gap = ctx.content_w * 4 / 100; // 4% gap between columns
    let col_w = (ctx.content_w - gap) / 2;
    let col_h = ctx.content_h - (y_cursor - ctx.margin_y);

    // Left column: even-indexed items
    let left_items: Vec<&str> = params.items.iter().step_by(2).map(|s| s.as_str()).collect();
    let left_text = left_items.join("\n");
    if !left_text.is_empty() {
        let sp = slide.add_text_box(
            &left_text,
            Emu(ctx.margin_x),
            Emu(y_cursor),
            Emu(col_w),
            Emu(col_h),
        );
        sp.size(ctx.body_size)
            .font(&ctx.body_font)
            .color(&ctx.body_color)
            .align(Align::Left);
    }

    // Right column: odd-indexed items
    let right_items: Vec<&str> = params
        .items
        .iter()
        .skip(1)
        .step_by(2)
        .map(|s| s.as_str())
        .collect();
    let right_text = right_items.join("\n");
    if !right_text.is_empty() {
        let right_x = ctx.margin_x + col_w + gap;
        let sp = slide.add_text_box(
            &right_text,
            Emu(right_x),
            Emu(y_cursor),
            Emu(col_w),
            Emu(col_h),
        );
        sp.size(ctx.body_size)
            .font(&ctx.body_font)
            .color(&ctx.body_color)
            .align(Align::Left);
    }
}

/// Emit icon-rows layout: title + rows of icon/label pairs.
fn emit_icon_rows(slide: &mut crate::slide::Slide<'_>, params: &PatternParams, ctx: &LayoutCtx) {
    use crate::units::Emu;
    use zavora_slide_oxml::Align;

    let mut y_cursor = ctx.margin_y;

    // Title
    if let Some(title) = &params.title {
        let title_h = Emu::points(ctx.title_size).0 * 2;
        let sp = slide.add_text_box(
            title,
            Emu(ctx.margin_x),
            Emu(y_cursor),
            Emu(ctx.content_w),
            Emu(title_h),
        );
        sp.size(ctx.title_size)
            .font(&ctx.heading_font)
            .color(&ctx.title_color)
            .bold(true)
            .align(Align::Left);
        y_cursor += title_h + ctx.margin_y / 2;
    }

    // Rows: each item gets an icon marker + text
    let remaining_h = ctx.content_h - (y_cursor - ctx.margin_y);
    let row_count = params.items.len().max(1);
    let row_h = remaining_h / row_count as i64;
    let icon_w = ctx.content_w * 8 / 100; // 8% width for icon area
    let text_x = ctx.margin_x + icon_w + ctx.content_w * 2 / 100; // small gap
    let text_w = ctx.content_w - icon_w - ctx.content_w * 2 / 100;

    for (i, item) in params.items.iter().enumerate() {
        let row_y = y_cursor + i as i64 * row_h;

        // Icon marker (a small colored square as visual element)
        let icon_size = row_h.min(icon_w) * 60 / 100;
        let icon_y = row_y + (row_h - icon_size) / 2;
        let sp = slide.add_shape(
            crate::units::ShapePreset::RoundRect,
            Emu(ctx.margin_x),
            Emu(icon_y),
            Emu(icon_size),
            Emu(icon_size),
        );
        sp.set_fill(&ctx.accent_color);

        // Text beside the icon
        let sp = slide.add_text_box(item, Emu(text_x), Emu(row_y), Emu(text_w), Emu(row_h));
        sp.size(ctx.body_size)
            .font(&ctx.body_font)
            .color(&ctx.body_color)
            .align(Align::Left);
    }
}

/// Emit stat-callout layout: large stat number + supporting text.
fn emit_stat_callout(slide: &mut crate::slide::Slide<'_>, params: &PatternParams, ctx: &LayoutCtx) {
    use crate::units::Emu;
    use zavora_slide_oxml::Align;

    let mut y_cursor = ctx.margin_y;

    // Title (if provided)
    if let Some(title) = &params.title {
        let title_h = Emu::points(ctx.title_size).0 * 2;
        let sp = slide.add_text_box(
            title,
            Emu(ctx.margin_x),
            Emu(y_cursor),
            Emu(ctx.content_w),
            Emu(title_h),
        );
        sp.size(ctx.title_size)
            .font(&ctx.heading_font)
            .color(&ctx.title_color)
            .bold(true)
            .align(Align::Left);
        y_cursor += title_h + ctx.margin_y / 2;
    }

    let remaining_h = ctx.content_h - (y_cursor - ctx.margin_y);

    // Large stat number (centered, large font — this is the stat, not body text)
    let stat_text = params.items.first().map(|s| s.as_str()).unwrap_or("0");
    let stat_h = remaining_h * 60 / 100;
    let stat_size = 72.0; // Very large for the stat number
    let sp = slide.add_text_box(
        stat_text,
        Emu(ctx.margin_x),
        Emu(y_cursor),
        Emu(ctx.content_w),
        Emu(stat_h),
    );
    // Stat numbers are centered as a design element (not body text)
    sp.size(stat_size)
        .font(&ctx.heading_font)
        .color(&ctx.accent_color)
        .bold(true)
        .align(Align::Center);
    y_cursor += stat_h;

    // Supporting text (left-aligned body)
    let support_text = params.items.get(1).map(|s| s.as_str()).unwrap_or("");
    if !support_text.is_empty() {
        let support_h = remaining_h - stat_h;
        let sp = slide.add_text_box(
            support_text,
            Emu(ctx.margin_x),
            Emu(y_cursor),
            Emu(ctx.content_w),
            Emu(support_h),
        );
        sp.size(ctx.body_size)
            .font(&ctx.body_font)
            .color(&ctx.body_color)
            .align(Align::Left);
    }
}

/// Emit quote layout: large quote text + attribution below.
fn emit_quote(slide: &mut crate::slide::Slide<'_>, params: &PatternParams, ctx: &LayoutCtx) {
    use crate::units::Emu;
    use zavora_slide_oxml::Align;

    // Quote text (larger, italic, left-aligned — not centered body)
    let quote_text = params.items.first().map(|s| s.as_str()).unwrap_or("");
    let quote_h = ctx.content_h * 65 / 100;
    // Indent quote slightly from margins for visual distinction
    let quote_indent = ctx.content_w * 5 / 100;
    let quote_w = ctx.content_w - quote_indent;
    let quote_x = ctx.margin_x + quote_indent;

    if !quote_text.is_empty() {
        let display_text = format!("\u{201C}{quote_text}\u{201D}");
        let sp = slide.add_text_box(
            &display_text,
            Emu(quote_x),
            Emu(ctx.margin_y),
            Emu(quote_w),
            Emu(quote_h),
        );
        sp.size(ctx.title_size * 0.8) // Slightly smaller than title but larger than body
            .font(&ctx.heading_font)
            .color(&ctx.title_color)
            .italic(true)
            .align(Align::Left);
    }

    // Attribution (smaller, left-aligned)
    let attribution = params.items.get(1).map(|s| s.as_str()).unwrap_or("");
    if !attribution.is_empty() {
        let attr_y = ctx.margin_y + quote_h + ctx.margin_y / 4;
        let attr_h = ctx.content_h - quote_h - ctx.margin_y / 4;
        let display_attr = format!("\u{2014} {attribution}");
        let sp = slide.add_text_box(
            &display_attr,
            Emu(quote_x),
            Emu(attr_y),
            Emu(quote_w),
            Emu(attr_h),
        );
        sp.size(ctx.caption_size)
            .font(&ctx.body_font)
            .color(&ctx.body_color)
            .align(Align::Left);
    }
}

/// Emit section-divider layout: large title for section breaks.
fn emit_section_divider(
    slide: &mut crate::slide::Slide<'_>,
    params: &PatternParams,
    ctx: &LayoutCtx,
) {
    use crate::units::Emu;
    use zavora_slide_oxml::Align;

    // Section title: large, bold (this is a title, not body)
    let title_text = params.title.as_deref().unwrap_or("Section");
    let section_title_size = ctx.title_size * 1.5; // Extra large for section breaks
    let title_h = ctx.content_h * 50 / 100;
    let title_y = ctx.margin_y + (ctx.content_h - title_h) / 3; // Upper third

    let sp = slide.add_text_box(
        title_text,
        Emu(ctx.margin_x),
        Emu(title_y),
        Emu(ctx.content_w),
        Emu(title_h),
    );
    sp.size(section_title_size)
        .font(&ctx.heading_font)
        .color(&ctx.title_color)
        .bold(true)
        .align(Align::Left);

    // Subtitle (if provided as first item) — left-aligned body
    let subtitle = params.items.first().map(|s| s.as_str()).unwrap_or("");
    if !subtitle.is_empty() {
        let sub_y = title_y + title_h + ctx.margin_y / 4;
        let sub_h = ctx.content_h - (sub_y - ctx.margin_y);
        let sp = slide.add_text_box(
            subtitle,
            Emu(ctx.margin_x),
            Emu(sub_y),
            Emu(ctx.content_w),
            Emu(sub_h),
        );
        sp.size(ctx.body_size)
            .font(&ctx.body_font)
            .color(&ctx.body_color)
            .align(Align::Left);
    }
}

/// Emit image-caption layout: image placeholder area + caption text below.
fn emit_image_caption(
    slide: &mut crate::slide::Slide<'_>,
    params: &PatternParams,
    ctx: &LayoutCtx,
) {
    use crate::units::Emu;
    use zavora_slide_oxml::Align;

    let mut y_cursor = ctx.margin_y;

    // Title (if provided)
    if let Some(title) = &params.title {
        let title_h = Emu::points(ctx.title_size).0 * 2;
        let sp = slide.add_text_box(
            title,
            Emu(ctx.margin_x),
            Emu(y_cursor),
            Emu(ctx.content_w),
            Emu(title_h),
        );
        sp.size(ctx.title_size)
            .font(&ctx.heading_font)
            .color(&ctx.title_color)
            .bold(true)
            .align(Align::Left);
        y_cursor += title_h + ctx.margin_y / 2;
    }

    let remaining_h = ctx.content_h - (y_cursor - ctx.margin_y);

    // Image placeholder area (a light-filled rectangle representing where the image goes)
    let image_h = remaining_h * 75 / 100;
    let sp = slide.add_shape(
        crate::units::ShapePreset::Rect,
        Emu(ctx.margin_x),
        Emu(y_cursor),
        Emu(ctx.content_w),
        Emu(image_h),
    );
    sp.set_fill("F3F4F6"); // Light gray placeholder
    y_cursor += image_h + ctx.margin_y / 4;

    // Caption text (left-aligned, smaller)
    let caption = params.items.first().map(|s| s.as_str()).unwrap_or("");
    if !caption.is_empty() {
        let caption_h = remaining_h - image_h - ctx.margin_y / 4;
        let sp = slide.add_text_box(
            caption,
            Emu(ctx.margin_x),
            Emu(y_cursor),
            Emu(ctx.content_w),
            Emu(caption_h),
        );
        sp.size(ctx.caption_size)
            .font(&ctx.body_font)
            .color(&ctx.body_color)
            .align(Align::Left);
    }
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn catalog_has_expected_palette_count() {
        assert!(
            palettes().len() >= 8,
            "Expected at least 8 palettes, got {}",
            palettes().len()
        );
    }

    #[test]
    fn catalog_has_expected_font_pairing_count() {
        assert!(
            font_pairings().len() >= 6,
            "Expected at least 6 font pairings, got {}",
            font_pairings().len()
        );
    }

    #[test]
    fn palette_lookup_by_id() {
        let p = palette_by_id("ocean").expect("ocean palette should exist");
        assert_eq!(p.id, "ocean");
        assert_eq!(p.name, "Ocean");
        assert_eq!(p.tone, "professional");
    }

    #[test]
    fn palette_lookup_invalid_returns_none() {
        assert!(palette_by_id("nonexistent").is_none());
    }

    #[test]
    fn font_pairing_lookup_by_id() {
        let fp = font_pairing_by_id("modern").expect("modern pairing should exist");
        assert_eq!(fp.id, "modern");
        assert_eq!(fp.heading, "Inter");
        assert_eq!(fp.body, "Inter");
    }

    #[test]
    fn font_pairing_lookup_invalid_returns_none() {
        assert!(font_pairing_by_id("nonexistent").is_none());
    }

    #[test]
    fn all_palettes_have_valid_hex_colors() {
        for p in palettes() {
            for (label, hex) in [
                ("primary", p.primary),
                ("secondary", p.secondary),
                ("accent1", p.accent1),
                ("accent2", p.accent2),
                ("accent3", p.accent3),
                ("accent4", p.accent4),
                ("accent5", p.accent5),
                ("accent6", p.accent6),
            ] {
                assert_eq!(
                    hex.len(),
                    6,
                    "Palette '{}' {}: expected 6-char hex, got '{}'",
                    p.id,
                    label,
                    hex
                );
                assert!(
                    hex.chars().all(|c| c.is_ascii_hexdigit()),
                    "Palette '{}' {}: '{}' contains non-hex characters",
                    p.id,
                    label,
                    hex
                );
            }
        }
    }

    #[test]
    fn all_palettes_have_unique_ids() {
        let mut ids: Vec<&str> = palettes().iter().map(|p| p.id).collect();
        ids.sort();
        ids.dedup();
        assert_eq!(ids.len(), palettes().len(), "Palette ids must be unique");
    }

    #[test]
    fn all_font_pairings_have_unique_ids() {
        let mut ids: Vec<&str> = font_pairings().iter().map(|fp| fp.id).collect();
        ids.sort();
        ids.dedup();
        assert_eq!(
            ids.len(),
            font_pairings().len(),
            "Font pairing ids must be unique"
        );
    }

    #[test]
    fn apply_design_theme_modifies_presentation() {
        let mut pres = Presentation::new();
        apply_design_theme(&mut pres, "corporate", "modern").expect("should succeed");

        // Verify the theme was modified by rendering the theme XML and checking
        // that the corporate palette colors are present.
        let palette = palette_by_id("corporate").unwrap();
        let xml = pres.theme_xml_for_test();
        assert!(
            xml.contains(&format!("val=\"{}\"", palette.accent1.to_uppercase())),
            "Theme XML should contain accent1 color"
        );
        assert!(
            xml.contains("typeface=\"Inter\""),
            "Theme XML should contain the heading font"
        );
    }

    #[test]
    fn apply_design_theme_invalid_palette_returns_error() {
        let mut pres = Presentation::new();
        let result = apply_design_theme(&mut pres, "nonexistent", "modern");
        assert!(result.is_err());
        let err = result.unwrap_err().to_string();
        assert!(err.contains("palette not found"), "Error: {err}");
    }

    #[test]
    fn apply_design_theme_invalid_font_pairing_returns_error() {
        let mut pres = Presentation::new();
        let result = apply_design_theme(&mut pres, "ocean", "nonexistent");
        assert!(result.is_err());
        let err = result.unwrap_err().to_string();
        assert!(err.contains("font pairing not found"), "Error: {err}");
    }

    // -----------------------------------------------------------------------
    // Layout pattern tests (Req 23.1, 23.2)
    // -----------------------------------------------------------------------

    /// Helper: create a presentation, add a blank slide, apply a pattern, return shapes.
    fn apply_pattern_and_get_shapes(
        pattern: LayoutPattern,
        params: &PatternParams,
    ) -> Vec<crate::slide::ShapeInfo> {
        let mut pres = Presentation::new();
        pres.add_slide(crate::units::Layout::Blank);
        {
            let mut slide = pres.slide_mut(0).unwrap();
            apply_layout_pattern(&mut slide, pattern, params).unwrap();
        }
        let slide = pres.slide(0).unwrap();
        slide.shapes()
    }

    #[test]
    fn two_column_produces_shapes() {
        let params = PatternParams {
            title: Some("Two Columns".into()),
            items: vec![
                "Left 1".into(),
                "Right 1".into(),
                "Left 2".into(),
                "Right 2".into(),
            ],
            ..Default::default()
        };
        let shapes = apply_pattern_and_get_shapes(LayoutPattern::TwoColumn, &params);
        // Should have at least: title + left column + right column = 3 shapes
        assert!(
            shapes.len() >= 3,
            "TwoColumn should produce at least 3 shapes, got {}",
            shapes.len()
        );
    }

    #[test]
    fn icon_rows_produces_shapes() {
        let params = PatternParams {
            title: Some("Features".into()),
            items: vec!["Feature A".into(), "Feature B".into(), "Feature C".into()],
            ..Default::default()
        };
        let shapes = apply_pattern_and_get_shapes(LayoutPattern::IconRows, &params);
        // Title + (icon shape + text) per item = 1 + 3*2 = 7
        assert!(
            shapes.len() >= 4,
            "IconRows should produce shapes, got {}",
            shapes.len()
        );
    }

    #[test]
    fn stat_callout_produces_shapes() {
        let params = PatternParams {
            title: Some("Key Metric".into()),
            items: vec!["99.9%".into(), "Uptime over the last 12 months".into()],
            ..Default::default()
        };
        let shapes = apply_pattern_and_get_shapes(LayoutPattern::StatCallout, &params);
        // Title + stat + supporting text = 3
        assert!(
            shapes.len() >= 3,
            "StatCallout should produce at least 3 shapes, got {}",
            shapes.len()
        );
    }

    #[test]
    fn quote_produces_shapes() {
        let params = PatternParams {
            items: vec![
                "The best way to predict the future is to invent it.".into(),
                "Alan Kay".into(),
            ],
            ..Default::default()
        };
        let shapes = apply_pattern_and_get_shapes(LayoutPattern::Quote, &params);
        // Quote text + attribution = 2
        assert!(
            shapes.len() >= 2,
            "Quote should produce at least 2 shapes, got {}",
            shapes.len()
        );
    }

    #[test]
    fn section_divider_produces_shapes() {
        let params = PatternParams {
            title: Some("Part Two".into()),
            items: vec!["The implementation details".into()],
            ..Default::default()
        };
        let shapes = apply_pattern_and_get_shapes(LayoutPattern::SectionDivider, &params);
        // Title + subtitle = 2
        assert!(
            shapes.len() >= 2,
            "SectionDivider should produce at least 2 shapes, got {}",
            shapes.len()
        );
    }

    #[test]
    fn image_caption_produces_shapes() {
        let params = PatternParams {
            title: Some("Figure 1".into()),
            items: vec!["A diagram showing the architecture".into()],
            ..Default::default()
        };
        let shapes = apply_pattern_and_get_shapes(LayoutPattern::ImageCaption, &params);
        // Title + image placeholder (shape) + caption = 3
        assert!(
            shapes.len() >= 3,
            "ImageCaption should produce at least 3 shapes, got {}",
            shapes.len()
        );
    }

    #[test]
    fn margins_respected_all_patterns() {
        // Standard widescreen: 12192000 x 6858000 EMU
        let slide_cx: i64 = 12192000;
        let slide_cy: i64 = 6858000;
        let min_margin_x = slide_cx * 5 / 100; // 5% = 609600
        let min_margin_y = slide_cy * 5 / 100; // 5% = 342900

        let patterns = [
            LayoutPattern::TwoColumn,
            LayoutPattern::IconRows,
            LayoutPattern::StatCallout,
            LayoutPattern::Quote,
            LayoutPattern::SectionDivider,
            LayoutPattern::ImageCaption,
        ];

        for pattern in patterns {
            let params = PatternParams {
                title: Some("Test Title".into()),
                items: vec!["Item 1".into(), "Item 2".into()],
                ..Default::default()
            };

            let mut pres = Presentation::new();
            pres.add_slide(crate::units::Layout::Blank);
            {
                let mut slide = pres.slide_mut(0).unwrap();
                apply_layout_pattern(&mut slide, pattern, &params).unwrap();
            }

            // Check shapes via the slide XML — verify positions respect margins
            let slide_ref = pres.slide(0).unwrap();
            let scene = slide_ref.scene();
            for item in &scene.items {
                let rect = match item {
                    zavora_slide_layout::Item::Text { rect, .. } => rect,
                    zavora_slide_layout::Item::Rect { rect, .. } => rect,
                    zavora_slide_layout::Item::Image { rect, .. } => rect,
                    zavora_slide_layout::Item::Shape { rect, .. } => rect,
                };
                // Left margin
                assert!(
                    rect.x >= min_margin_x,
                    "{pattern:?}: shape x={} violates left margin {min_margin_x}",
                    rect.x
                );
                // Top margin
                assert!(
                    rect.y >= min_margin_y,
                    "{pattern:?}: shape y={} violates top margin {min_margin_y}",
                    rect.y
                );
                // Right margin: x + w <= slide_cx - margin
                assert!(
                    rect.x + rect.w <= slide_cx - min_margin_x,
                    "{pattern:?}: shape right edge {} violates right margin {}",
                    rect.x + rect.w,
                    slide_cx - min_margin_x
                );
            }
        }
    }

    #[test]
    fn size_hierarchy_enforced() {
        // For patterns with both title and body, title font must be larger.
        let params = PatternParams {
            title: Some("Big Title".into()),
            items: vec!["Body text here".into(), "More body".into()],
            ..Default::default()
        };

        let mut pres = Presentation::new();
        pres.add_slide(crate::units::Layout::Blank);
        {
            let mut slide = pres.slide_mut(0).unwrap();
            apply_layout_pattern(&mut slide, LayoutPattern::TwoColumn, &params).unwrap();
        }

        let slide_ref = pres.slide(0).unwrap();
        let scene = slide_ref.scene();

        // Collect font sizes from text items
        let mut sizes: Vec<(f64, bool)> = Vec::new(); // (size, is_bold)
        for item in &scene.items {
            if let zavora_slide_layout::Item::Text { lines, .. } = item {
                for line in lines {
                    sizes.push((line.size_pt, line.bold));
                }
            }
        }

        // Find the title (bold, largest) and body sizes
        let title_size = sizes
            .iter()
            .filter(|(_, bold)| *bold)
            .map(|(s, _)| *s)
            .fold(0.0_f64, f64::max);
        let body_size = sizes
            .iter()
            .filter(|(_, bold)| !*bold)
            .map(|(s, _)| *s)
            .fold(f64::MAX, f64::min);

        if title_size > 0.0 && body_size < f64::MAX {
            assert!(
                title_size > body_size,
                "Title size ({title_size}) must be larger than body size ({body_size})"
            );
        }
    }

    #[test]
    fn no_centered_body_text() {
        // Body text should be left-aligned (anti-pattern: no centered body).
        // The stat callout's stat number is a design element (heading font, bold),
        // not body text, so it's exempt.
        let params = PatternParams {
            title: Some("Title".into()),
            items: vec!["Left item".into(), "Right item".into()],
            ..Default::default()
        };

        let patterns_with_body = [
            LayoutPattern::TwoColumn,
            LayoutPattern::IconRows,
            LayoutPattern::SectionDivider,
            LayoutPattern::ImageCaption,
        ];

        for pattern in patterns_with_body {
            let mut pres = Presentation::new();
            pres.add_slide(crate::units::Layout::Blank);
            {
                let mut slide = pres.slide_mut(0).unwrap();
                apply_layout_pattern(&mut slide, pattern, &params).unwrap();
            }

            // Check that body text shapes (non-bold, non-title) have left alignment
            // by inspecting the underlying shapes' alignment
            let data = &pres.slides_for_test()[0];
            for sp in &data.shapes {
                // Skip shapes that are title-like (bold) or non-text
                let is_title_shape = sp
                    .body
                    .paragraphs
                    .iter()
                    .all(|p| p.runs.iter().all(|r| r.props.bold == Some(true)));
                if is_title_shape {
                    continue;
                }
                // Body text paragraphs should be left-aligned
                for p in &sp.body.paragraphs {
                    if let Some(align) = p.align {
                        assert_ne!(
                            align,
                            zavora_slide_oxml::Align::Center,
                            "{pattern:?}: body text should not be centered"
                        );
                    }
                    // None alignment defaults to left, which is fine
                }
            }
        }
    }

    #[test]
    fn pattern_uses_specified_palette() {
        let params = PatternParams {
            title: Some("Styled".into()),
            items: vec!["Content".into()],
            palette_id: Some("ocean".into()),
            font_pairing_id: Some("elegant".into()),
        };

        let mut pres = Presentation::new();
        pres.add_slide(crate::units::Layout::Blank);
        {
            let mut slide = pres.slide_mut(0).unwrap();
            apply_layout_pattern(&mut slide, LayoutPattern::TwoColumn, &params).unwrap();
        }

        // Verify the ocean palette's primary color is used for title
        let data = &pres.slides_for_test()[0];
        let ocean = palette_by_id("ocean").unwrap();
        let has_palette_color = data.shapes.iter().any(|sp| {
            sp.body.paragraphs.iter().any(|p| {
                p.runs
                    .iter()
                    .any(|r| r.props.color.as_deref() == Some(ocean.primary))
            })
        });
        assert!(
            has_palette_color,
            "Pattern should use the specified palette colors"
        );

        // Verify the elegant font pairing is used
        let elegant = font_pairing_by_id("elegant").unwrap();
        let has_font = data.shapes.iter().any(|sp| {
            sp.body.paragraphs.iter().any(|p| {
                p.runs
                    .iter()
                    .any(|r| r.props.font.as_deref() == Some(elegant.heading))
            })
        });
        assert!(has_font, "Pattern should use the specified font pairing");
    }
}
