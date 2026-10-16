//! Deterministic visual QA: structured layout report.
//!
//! Analyzes a resolved [`Scene`] and produces a [`QaReport`] listing every visible
//! element with its bounding box (EMU + fraction of slide), kind, and z-order, plus
//! findings for layout problems: off-canvas, overlaps, zero-area, margin violations,
//! WCAG contrast violations, and minimum font-size violations.
//!
//! The analysis is fully deterministic — same input always produces the same output.

use zavora_slide_layout::{Background, Color, Item, Rect, Scene, ShapeFill};

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

/// The kind of element on the slide.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ElementKind {
    Text,
    Shape,
    Image,
    Table,
}

/// Information about a single visible element.
#[derive(Debug, Clone, PartialEq)]
pub struct ElementInfo {
    /// Index in the scene's item list (also serves as element reference).
    pub index: usize,
    /// What kind of element this is.
    pub kind: ElementKind,
    /// Bounding box in EMU.
    pub bbox_emu: Rect,
    /// Bounding box as fraction of slide dimensions (x, y, w, h) in 0.0–1.0.
    pub bbox_fraction: (f64, f64, f64, f64),
    /// Z-order (0 = bottom-most).
    pub z_order: usize,
}

/// Severity of a finding.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Severity {
    Error,
    Warning,
    Info,
}

/// The kind of layout finding.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FindingKind {
    /// Element extends beyond slide bounds.
    OffCanvas,
    /// Two elements overlap beyond the threshold.
    Overlap,
    /// Text overflows its frame.
    FrameOverflow,
    /// Element is too close to slide edges.
    MarginViolation,
    /// Element has zero or effectively zero area.
    ZeroArea,
    /// Text run has insufficient WCAG contrast ratio against its background.
    LowContrast,
    /// Text run is below the minimum font size.
    SmallFont,
    // -- Design lint kinds (Part J) --
    /// Slide has only text elements — no visual element (shape, image).
    TextOnlySlide,
    /// Body text is center-aligned (anti-pattern).
    CenteredBody,
    /// More than N distinct font families on a slide (default N=3).
    TooManyFonts,
    /// Title text is not larger than body text (size hierarchy violated).
    UndersizedTitle,
}

/// A single layout finding.
#[derive(Debug, Clone, PartialEq)]
pub struct Finding {
    pub severity: Severity,
    pub kind: FindingKind,
    /// Indices of the elements involved.
    pub refs: Vec<usize>,
    /// Human-readable description.
    pub message: String,
}

/// The complete QA report for a slide.
#[derive(Debug, Clone, PartialEq)]
pub struct QaReport {
    /// All visible elements with resolved bounding boxes.
    pub elements: Vec<ElementInfo>,
    /// Layout findings (problems detected).
    pub findings: Vec<Finding>,
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

/// Overlap threshold: intersection area must exceed this fraction of the smaller
/// element's area to be flagged.
const OVERLAP_THRESHOLD: f64 = 0.10;

/// Margin threshold: elements closer than this fraction of the slide dimension
/// to any edge are flagged.
const MARGIN_THRESHOLD: f64 = 0.05;

// ---------------------------------------------------------------------------
// Contrast Configuration
// ---------------------------------------------------------------------------

/// Configuration for WCAG contrast and minimum font-size checks.
#[derive(Debug, Clone, PartialEq)]
pub struct ContrastConfig {
    /// Minimum acceptable WCAG contrast ratio (default 4.5:1 for normal text).
    pub min_ratio: f64,
    /// Minimum acceptable font size in points (default 10pt).
    pub min_font_size_pt: f64,
}

impl Default for ContrastConfig {
    fn default() -> Self {
        Self {
            min_ratio: 4.5,
            min_font_size_pt: 10.0,
        }
    }
}

// ---------------------------------------------------------------------------
// WCAG Contrast Computation
// ---------------------------------------------------------------------------

/// Compute the WCAG relative luminance of an sRGB color.
///
/// Returns a value in the range 0.0 (black) to 1.0 (white).
/// Formula per WCAG 2.1: <https://www.w3.org/TR/WCAG21/#dfn-relative-luminance>
pub fn relative_luminance(color: Color) -> f64 {
    fn linearize(channel: u8) -> f64 {
        let s = channel as f64 / 255.0;
        if s <= 0.04045 {
            s / 12.92
        } else {
            ((s + 0.055) / 1.055).powf(2.4)
        }
    }

    let r = linearize(color.r);
    let g = linearize(color.g);
    let b = linearize(color.b);

    0.2126 * r + 0.7152 * g + 0.0722 * b
}

/// Compute the WCAG contrast ratio between two colors.
///
/// Returns a value in the range 1.0 (identical) to 21.0 (black vs white).
/// Formula per WCAG 2.1: (L1 + 0.05) / (L2 + 0.05) where L1 >= L2.
pub fn contrast_ratio(fg: Color, bg: Color) -> f64 {
    let l1 = relative_luminance(fg);
    let l2 = relative_luminance(bg);

    let (lighter, darker) = if l1 >= l2 { (l1, l2) } else { (l2, l1) };

    (lighter + 0.05) / (darker + 0.05)
}

// ---------------------------------------------------------------------------
// Analysis
// ---------------------------------------------------------------------------

/// Analyze the layout of a scene and produce a deterministic QA report.
///
/// The report lists every visible element with its resolved bounding box and
/// flags layout problems: off-canvas, overlaps, zero-area, and margin violations.
pub fn analyze_layout(scene: &Scene) -> QaReport {
    let elements = build_element_list(scene);
    let mut findings = Vec::new();

    // Check each element for individual issues.
    for elem in &elements {
        check_zero_area(elem, &mut findings);
        check_off_canvas(elem, scene, &mut findings);
        check_margin_violation(elem, scene, &mut findings);
    }

    // Check pairwise overlaps.
    check_overlaps(&elements, &mut findings);

    QaReport { elements, findings }
}

/// Check WCAG contrast ratio and minimum font size for all text items in a scene.
///
/// For each text item, determines the effective background color (from the nearest
/// underlapping shape fill or the slide background), computes the WCAG contrast ratio
/// between each text line's color and the effective background, and flags:
/// - Text runs below the minimum contrast ratio (`LowContrast`)
/// - Text runs below the minimum font size (`SmallFont`)
pub fn check_contrast(scene: &Scene, config: &ContrastConfig) -> Vec<Finding> {
    let mut findings = Vec::new();

    for (item_idx, item) in scene.items.iter().enumerate() {
        let (rect, lines) = match item {
            Item::Text { rect, lines, .. } => (rect, lines),
            _ => continue,
        };

        // Determine effective background color for this text item.
        let bg_color = resolve_effective_background(scene, item_idx, rect);

        for (line_idx, line) in lines.iter().enumerate() {
            // Check font size.
            if line.size_pt < config.min_font_size_pt {
                findings.push(Finding {
                    severity: Severity::Warning,
                    kind: FindingKind::SmallFont,
                    refs: vec![item_idx],
                    message: format!(
                        "Element {} line {}: font size {:.1}pt is below minimum {:.1}pt",
                        item_idx, line_idx, line.size_pt, config.min_font_size_pt
                    ),
                });
            }

            // Check contrast ratio.
            let ratio = contrast_ratio(line.color, bg_color);
            if ratio < config.min_ratio {
                findings.push(Finding {
                    severity: Severity::Warning,
                    kind: FindingKind::LowContrast,
                    refs: vec![item_idx],
                    message: format!(
                        "Element {} line {}: contrast ratio {:.2}:1 is below minimum {:.1}:1 \
                         (fg: #{:02X}{:02X}{:02X}, bg: #{:02X}{:02X}{:02X})",
                        item_idx,
                        line_idx,
                        ratio,
                        config.min_ratio,
                        line.color.r,
                        line.color.g,
                        line.color.b,
                        bg_color.r,
                        bg_color.g,
                        bg_color.b
                    ),
                });
            }
        }
    }

    findings
}

/// Resolve the effective background color for a text item.
///
/// Walks the scene items in z-order (bottom to top) looking for the nearest
/// underlapping shape fill that covers the text item's position. Falls back to
/// the slide background color, or white if no background is set.
fn resolve_effective_background(scene: &Scene, text_idx: usize, text_rect: &Rect) -> Color {
    // Check items below this text item (lower z-order) for an underlapping fill.
    // Walk in reverse z-order (highest z below text first) to find the nearest.
    for i in (0..text_idx).rev() {
        let item = &scene.items[i];
        match item {
            Item::Shape { rect, fill, .. } => {
                if rects_overlap(rect, text_rect)
                    && let Some(color) = fill_to_color(fill)
                {
                    return color;
                }
            }
            Item::Rect {
                rect,
                fill: Some(color),
                ..
            } if rects_overlap(rect, text_rect) => {
                return *color;
            }
            _ => {}
        }
    }

    // Fall back to slide background.
    if let Some(ref rich_bg) = scene.rich_background {
        match rich_bg {
            Background::Solid(c) => return *c,
            Background::Picture(_) => {
                // Can't determine a single color from a picture background;
                // fall back to white as a conservative default.
                return Color::WHITE;
            }
        }
    }

    if let Some(bg) = scene.background {
        return bg;
    }

    // Default: white background.
    Color::WHITE
}

/// Extract a solid color from a shape fill, if possible.
fn fill_to_color(fill: &ShapeFill) -> Option<Color> {
    match fill {
        ShapeFill::Solid(c) => Some(*c),
        ShapeFill::Gradient(g) => {
            // Use the first gradient stop as an approximation.
            g.stops.first().map(|s| s.color)
        }
        ShapeFill::Picture(_) | ShapeFill::None => None,
    }
}

/// Check if two rectangles overlap (share any area).
fn rects_overlap(a: &Rect, b: &Rect) -> bool {
    let x_overlap = (a.x + a.w).min(b.x + b.w) > a.x.max(b.x);
    let y_overlap = (a.y + a.h).min(b.y + b.h) > a.y.max(b.y);
    x_overlap && y_overlap
}

/// Build the element list from the scene items.
fn build_element_list(scene: &Scene) -> Vec<ElementInfo> {
    let sw = scene.width_emu as f64;
    let sh = scene.height_emu as f64;

    scene
        .items
        .iter()
        .enumerate()
        .map(|(i, item)| {
            let (kind, rect) = match item {
                Item::Text { rect, .. } => (ElementKind::Text, *rect),
                Item::Shape { rect, .. } => (ElementKind::Shape, *rect),
                Item::Image { rect, .. } => (ElementKind::Image, *rect),
                Item::Rect { rect, .. } => (ElementKind::Shape, *rect),
            };

            let bbox_fraction = if sw > 0.0 && sh > 0.0 {
                (
                    rect.x as f64 / sw,
                    rect.y as f64 / sh,
                    rect.w as f64 / sw,
                    rect.h as f64 / sh,
                )
            } else {
                (0.0, 0.0, 0.0, 0.0)
            };

            ElementInfo {
                index: i,
                kind,
                bbox_emu: rect,
                bbox_fraction,
                z_order: i,
            }
        })
        .collect()
}

/// Check if an element has zero or effectively zero area.
fn check_zero_area(elem: &ElementInfo, findings: &mut Vec<Finding>) {
    if elem.bbox_emu.w <= 0 || elem.bbox_emu.h <= 0 {
        findings.push(Finding {
            severity: Severity::Error,
            kind: FindingKind::ZeroArea,
            refs: vec![elem.index],
            message: format!(
                "Element {} has zero/negative area ({}×{} EMU)",
                elem.index, elem.bbox_emu.w, elem.bbox_emu.h
            ),
        });
    }
}

/// Check if an element extends beyond the slide bounds.
fn check_off_canvas(elem: &ElementInfo, scene: &Scene, findings: &mut Vec<Finding>) {
    let r = &elem.bbox_emu;
    let right = r.x + r.w;
    let bottom = r.y + r.h;

    if r.x < 0 || r.y < 0 || right > scene.width_emu || bottom > scene.height_emu {
        findings.push(Finding {
            severity: Severity::Warning,
            kind: FindingKind::OffCanvas,
            refs: vec![elem.index],
            message: format!(
                "Element {} extends beyond slide bounds (bbox: x={}, y={}, r={}, b={} vs slide {}×{})",
                elem.index, r.x, r.y, right, bottom, scene.width_emu, scene.height_emu
            ),
        });
    }
}

/// Check if an element is too close to the slide edges.
fn check_margin_violation(elem: &ElementInfo, scene: &Scene, findings: &mut Vec<Finding>) {
    let r = &elem.bbox_emu;
    let sw = scene.width_emu as f64;
    let sh = scene.height_emu as f64;

    // Skip elements with zero/negative dimensions.
    if r.w <= 0 || r.h <= 0 {
        return;
    }

    let margin_x = sw * MARGIN_THRESHOLD;
    let margin_y = sh * MARGIN_THRESHOLD;

    let left_gap = r.x as f64;
    let top_gap = r.y as f64;
    let right_gap = sw - (r.x + r.w) as f64;
    let bottom_gap = sh - (r.y + r.h) as f64;

    // Only flag if the element is partially on-canvas but too close to an edge.
    // Don't flag elements that are already off-canvas (they get OffCanvas finding).
    let is_on_canvas =
        r.x >= 0 && r.y >= 0 && (r.x + r.w) <= scene.width_emu && (r.y + r.h) <= scene.height_emu;

    if !is_on_canvas {
        return;
    }

    if left_gap < margin_x || top_gap < margin_y || right_gap < margin_x || bottom_gap < margin_y {
        findings.push(Finding {
            severity: Severity::Info,
            kind: FindingKind::MarginViolation,
            refs: vec![elem.index],
            message: format!(
                "Element {} is within {}% margin of slide edge (gaps: L={:.0}, T={:.0}, R={:.0}, B={:.0} EMU; threshold={:.0}/{:.0})",
                elem.index,
                (MARGIN_THRESHOLD * 100.0) as u32,
                left_gap, top_gap, right_gap, bottom_gap,
                margin_x, margin_y
            ),
        });
    }
}

/// Check pairwise overlaps between elements.
fn check_overlaps(elements: &[ElementInfo], findings: &mut Vec<Finding>) {
    for i in 0..elements.len() {
        for j in (i + 1)..elements.len() {
            let a = &elements[i];
            let b = &elements[j];

            // Skip zero-area elements.
            if a.bbox_emu.w <= 0 || a.bbox_emu.h <= 0 || b.bbox_emu.w <= 0 || b.bbox_emu.h <= 0 {
                continue;
            }

            if let Some(intersection_area) = rect_intersection_area(&a.bbox_emu, &b.bbox_emu) {
                let area_a = a.bbox_emu.w as f64 * a.bbox_emu.h as f64;
                let area_b = b.bbox_emu.w as f64 * b.bbox_emu.h as f64;
                let smaller_area = area_a.min(area_b);

                if smaller_area > 0.0 && intersection_area / smaller_area > OVERLAP_THRESHOLD {
                    let classification = overlap_classification(a.kind, b.kind);
                    findings.push(Finding {
                        severity: Severity::Warning,
                        kind: FindingKind::Overlap,
                        refs: vec![a.index, b.index],
                        message: format!(
                            "Elements {} and {} overlap ({}) — intersection {:.1}% of smaller element",
                            a.index,
                            b.index,
                            classification,
                            (intersection_area / smaller_area) * 100.0
                        ),
                    });
                }
            }
        }
    }
}

/// Compute the intersection area of two rectangles, or `None` if they don't overlap.
fn rect_intersection_area(a: &Rect, b: &Rect) -> Option<f64> {
    let x_overlap = (a.x + a.w).min(b.x + b.w) - a.x.max(b.x);
    let y_overlap = (a.y + a.h).min(b.y + b.h) - a.y.max(b.y);

    if x_overlap > 0 && y_overlap > 0 {
        Some(x_overlap as f64 * y_overlap as f64)
    } else {
        None
    }
}

/// Classify an overlap by the kinds of elements involved.
fn overlap_classification(a: ElementKind, b: ElementKind) -> &'static str {
    match (a, b) {
        (ElementKind::Text, ElementKind::Text) => "text-over-text",
        (ElementKind::Text, ElementKind::Shape) | (ElementKind::Shape, ElementKind::Text) => {
            "text-over-shape"
        }
        (ElementKind::Text, ElementKind::Image) | (ElementKind::Image, ElementKind::Text) => {
            "text-over-image"
        }
        _ => "shape-over-shape",
    }
}

// ---------------------------------------------------------------------------
// Render-diff: before/after tile comparison
// ---------------------------------------------------------------------------

/// A rectangular region of the image that changed between before and after.
#[derive(Debug, Clone, PartialEq)]
pub struct DiffRegion {
    /// X offset of the tile (pixels).
    pub x: u32,
    /// Y offset of the tile (pixels).
    pub y: u32,
    /// Width of the tile (pixels, may be smaller at right/bottom edges).
    pub w: u32,
    /// Height of the tile (pixels, may be smaller at right/bottom edges).
    pub h: u32,
    /// Fraction of pixels in this tile that differ (0.0–1.0).
    pub change_fraction: f64,
}

/// Summary of visual differences between two rendered PNGs.
#[derive(Debug, Clone, PartialEq)]
pub struct RenderDiff {
    /// Overall fraction of pixels that differ across the entire image (0.0–1.0).
    pub total_change_fraction: f64,
    /// Tiles/regions with significant changes.
    pub changed_regions: Vec<DiffRegion>,
}

/// Per-channel threshold for considering two pixels "different".
/// Small differences (≤ this value per channel) are ignored to tolerate
/// anti-aliasing noise and sub-pixel rendering variations.
const PIXEL_DIFF_THRESHOLD: u8 = 4;

/// Minimum fraction of changed pixels in a tile to report it as a changed region.
const TILE_CHANGE_THRESHOLD: f64 = 0.01;

/// Error type for render-diff operations.
#[derive(Debug, thiserror::Error)]
pub enum RenderDiffError {
    #[error("failed to decode PNG: {0}")]
    PngDecode(String),
    #[error("image dimensions differ: before={bw}×{bh}, after={aw}×{ah}")]
    DimensionMismatch { bw: u32, bh: u32, aw: u32, ah: u32 },
}

/// Decoded RGBA image data.
struct DecodedImage {
    width: u32,
    height: u32,
    /// RGBA pixels, row-major, 4 bytes per pixel.
    data: Vec<u8>,
}

/// Decode a PNG from bytes into RGBA pixel data.
fn decode_png(png_bytes: &[u8]) -> Result<DecodedImage, RenderDiffError> {
    let decoder = png::Decoder::new(png_bytes);
    let mut reader = decoder
        .read_info()
        .map_err(|e| RenderDiffError::PngDecode(e.to_string()))?;

    let mut buf = vec![0u8; reader.output_buffer_size()];
    let info = reader
        .next_frame(&mut buf)
        .map_err(|e| RenderDiffError::PngDecode(e.to_string()))?;

    let width = info.width;
    let height = info.height;

    // Convert to RGBA regardless of source format.
    let rgba = match info.color_type {
        png::ColorType::Rgba => buf[..info.buffer_size()].to_vec(),
        png::ColorType::Rgb => {
            let src = &buf[..info.buffer_size()];
            let pixel_count = (width * height) as usize;
            let mut rgba = Vec::with_capacity(pixel_count * 4);
            for chunk in src.chunks(3) {
                rgba.push(chunk[0]);
                rgba.push(chunk[1]);
                rgba.push(chunk[2]);
                rgba.push(255);
            }
            rgba
        }
        png::ColorType::GrayscaleAlpha => {
            let src = &buf[..info.buffer_size()];
            let pixel_count = (width * height) as usize;
            let mut rgba = Vec::with_capacity(pixel_count * 4);
            for chunk in src.chunks(2) {
                rgba.push(chunk[0]);
                rgba.push(chunk[0]);
                rgba.push(chunk[0]);
                rgba.push(chunk[1]);
            }
            rgba
        }
        png::ColorType::Grayscale => {
            let src = &buf[..info.buffer_size()];
            let pixel_count = (width * height) as usize;
            let mut rgba = Vec::with_capacity(pixel_count * 4);
            for &g in &src[..(pixel_count)] {
                rgba.push(g);
                rgba.push(g);
                rgba.push(g);
                rgba.push(255);
            }
            rgba
        }
        png::ColorType::Indexed => {
            return Err(RenderDiffError::PngDecode(
                "indexed/palette PNG not supported; re-encode as RGBA".into(),
            ));
        }
    };

    Ok(DecodedImage {
        width,
        height,
        data: rgba,
    })
}

/// Returns true if two pixels differ beyond the anti-aliasing threshold.
#[inline]
fn pixels_differ(a: &[u8], b: &[u8]) -> bool {
    a[0].abs_diff(b[0]) > PIXEL_DIFF_THRESHOLD
        || a[1].abs_diff(b[1]) > PIXEL_DIFF_THRESHOLD
        || a[2].abs_diff(b[2]) > PIXEL_DIFF_THRESHOLD
        || a[3].abs_diff(b[3]) > PIXEL_DIFF_THRESHOLD
}

/// Compute a structural-difference summary between two rendered PNGs.
///
/// Divides the images into tiles of `tile_size × tile_size` pixels and reports
/// which tiles have significant pixel changes. This surfaces unintended visual
/// changes after an edit.
///
/// # Errors
///
/// Returns an error if either PNG cannot be decoded or if the images have
/// different dimensions.
pub fn compute_render_diff(
    before_png: &[u8],
    after_png: &[u8],
    tile_size: u32,
) -> Result<RenderDiff, RenderDiffError> {
    let before = decode_png(before_png)?;
    let after = decode_png(after_png)?;

    if before.width != after.width || before.height != after.height {
        return Err(RenderDiffError::DimensionMismatch {
            bw: before.width,
            bh: before.height,
            aw: after.width,
            ah: after.height,
        });
    }

    let width = before.width;
    let height = before.height;
    let total_pixels = (width as u64) * (height as u64);

    if total_pixels == 0 || tile_size == 0 {
        return Ok(RenderDiff {
            total_change_fraction: 0.0,
            changed_regions: Vec::new(),
        });
    }

    let cols = width.div_ceil(tile_size);
    let rows = height.div_ceil(tile_size);

    let mut total_changed: u64 = 0;
    let mut changed_regions = Vec::new();

    for ty in 0..rows {
        for tx in 0..cols {
            let tile_x = tx * tile_size;
            let tile_y = ty * tile_size;
            let tile_w = (width - tile_x).min(tile_size);
            let tile_h = (height - tile_y).min(tile_size);
            let tile_pixels = (tile_w as u64) * (tile_h as u64);

            let mut tile_changed: u64 = 0;

            for row in tile_y..(tile_y + tile_h) {
                for col in tile_x..(tile_x + tile_w) {
                    let idx = ((row as usize) * (width as usize) + (col as usize)) * 4;
                    let a = &before.data[idx..idx + 4];
                    let b = &after.data[idx..idx + 4];
                    if pixels_differ(a, b) {
                        tile_changed += 1;
                    }
                }
            }

            total_changed += tile_changed;

            let change_fraction = tile_changed as f64 / tile_pixels as f64;
            if change_fraction >= TILE_CHANGE_THRESHOLD {
                changed_regions.push(DiffRegion {
                    x: tile_x,
                    y: tile_y,
                    w: tile_w,
                    h: tile_h,
                    change_fraction,
                });
            }
        }
    }

    let total_change_fraction = total_changed as f64 / total_pixels as f64;

    Ok(RenderDiff {
        total_change_fraction,
        changed_regions,
    })
}

// ---------------------------------------------------------------------------
// Design Lint (Part J — Req 24)
// ---------------------------------------------------------------------------

/// Default maximum number of distinct font families allowed on a single slide.
const MAX_FONTS_DEFAULT: usize = 3;

/// Analyze a scene for design anti-patterns and return structured findings.
///
/// Checks performed:
/// - **TextOnlySlide**: slide has only text elements, no visual element (shape/image).
/// - **CenteredBody**: body text (non-title) is center-aligned.
/// - **TooManyFonts**: more than `max_fonts` distinct font families on the slide.
/// - **UndersizedTitle**: title text (bold/large) is not larger than body text.
///
/// A text item is considered a "title" if it contains at least one bold line with
/// a font size ≥ 24pt. All other text items are "body".
pub fn design_lint(scene: &Scene) -> Vec<Finding> {
    design_lint_with_config(scene, MAX_FONTS_DEFAULT)
}

/// Like [`design_lint`] but with a configurable max-fonts threshold.
pub fn design_lint_with_config(scene: &Scene, max_fonts: usize) -> Vec<Finding> {
    let mut findings = Vec::new();

    check_text_only_slide(scene, &mut findings);
    check_centered_body(scene, &mut findings);
    check_too_many_fonts(scene, max_fonts, &mut findings);
    check_undersized_title(scene, &mut findings);

    findings
}

/// Title detection threshold: a text item is a "title" if it has at least one
/// bold line with font size >= this value.
const TITLE_SIZE_THRESHOLD_PT: f64 = 24.0;

/// Check if the slide has only text elements (no shape or image).
fn check_text_only_slide(scene: &Scene, findings: &mut Vec<Finding>) {
    if scene.items.is_empty() {
        return;
    }

    let has_visual = scene.items.iter().any(|item| {
        matches!(
            item,
            Item::Shape { .. } | Item::Image { .. } | Item::Rect { .. }
        )
    });

    if !has_visual {
        // Collect all text element indices as refs.
        let refs: Vec<usize> = scene
            .items
            .iter()
            .enumerate()
            .filter_map(|(i, item)| {
                if matches!(item, Item::Text { .. }) {
                    Some(i)
                } else {
                    None
                }
            })
            .collect();

        if !refs.is_empty() {
            findings.push(Finding {
                severity: Severity::Warning,
                kind: FindingKind::TextOnlySlide,
                refs,
                message:
                    "Slide contains only text elements with no visual element (shape or image)"
                        .to_string(),
            });
        }
    }
}

/// Check for centered body text (non-title text items with center alignment).
fn check_centered_body(scene: &Scene, findings: &mut Vec<Finding>) {
    use zavora_slide_layout::Alignment;

    for (idx, item) in scene.items.iter().enumerate() {
        let lines = match item {
            Item::Text { lines, .. } => lines,
            _ => continue,
        };

        if lines.is_empty() {
            continue;
        }

        // Determine if this text item is a title (bold + large).
        let is_title = is_title_item(lines);
        if is_title {
            continue;
        }

        // Check if any line in this body text item is center-aligned.
        let has_centered = lines.iter().any(|l| l.alignment == Alignment::Center);
        if has_centered {
            findings.push(Finding {
                severity: Severity::Warning,
                kind: FindingKind::CenteredBody,
                refs: vec![idx],
                message: format!(
                    "Element {}: body text is center-aligned (design anti-pattern)",
                    idx
                ),
            });
        }
    }
}

/// Check if there are too many distinct font families on the slide.
fn check_too_many_fonts(scene: &Scene, max_fonts: usize, findings: &mut Vec<Finding>) {
    let mut font_set = std::collections::HashSet::new();

    for item in &scene.items {
        let lines = match item {
            Item::Text { lines, .. } => lines,
            _ => continue,
        };

        for line in lines {
            let family = line.font_family.trim();
            if !family.is_empty() {
                font_set.insert(family.to_lowercase());
            }
        }
    }

    if font_set.len() > max_fonts {
        // Collect all text element indices.
        let refs: Vec<usize> = scene
            .items
            .iter()
            .enumerate()
            .filter_map(|(i, item)| {
                if matches!(item, Item::Text { .. }) {
                    Some(i)
                } else {
                    None
                }
            })
            .collect();

        findings.push(Finding {
            severity: Severity::Warning,
            kind: FindingKind::TooManyFonts,
            refs,
            message: format!(
                "Slide uses {} distinct font families (maximum recommended: {})",
                font_set.len(),
                max_fonts
            ),
        });
    }
}

/// Check size hierarchy: title text should be larger than body text.
fn check_undersized_title(scene: &Scene, findings: &mut Vec<Finding>) {
    // Collect the maximum body text size and the minimum title text size.
    let mut max_body_size: f64 = 0.0;
    let mut min_title_size: f64 = f64::MAX;
    let mut title_indices: Vec<usize> = Vec::new();
    let mut has_title = false;
    let mut has_body = false;

    for (idx, item) in scene.items.iter().enumerate() {
        let lines = match item {
            Item::Text { lines, .. } => lines,
            _ => continue,
        };

        if lines.is_empty() {
            continue;
        }

        if is_title_item(lines) {
            has_title = true;
            title_indices.push(idx);
            for line in lines {
                if line.size_pt < min_title_size {
                    min_title_size = line.size_pt;
                }
            }
        } else {
            has_body = true;
            for line in lines {
                if line.size_pt > max_body_size {
                    max_body_size = line.size_pt;
                }
            }
        }
    }

    // Only flag if both title and body exist and title is not larger.
    if has_title && has_body && min_title_size <= max_body_size {
        findings.push(Finding {
            severity: Severity::Warning,
            kind: FindingKind::UndersizedTitle,
            refs: title_indices,
            message: format!(
                "Title text ({:.1}pt) is not larger than body text ({:.1}pt) — size hierarchy violated",
                min_title_size, max_body_size
            ),
        });
    }
}

/// Determine if a text item is a "title" based on its lines.
///
/// A text item is considered a title if it has at least one bold line with
/// font size >= TITLE_SIZE_THRESHOLD_PT (24pt).
fn is_title_item(lines: &[zavora_slide_layout::TextLine]) -> bool {
    lines
        .iter()
        .any(|l| l.bold && l.size_pt >= TITLE_SIZE_THRESHOLD_PT)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use zavora_slide_layout::{Item, Rect, Scene};

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
            fill: zavora_slide_layout::ShapeFill::None,
            outline: None,
            rotation_deg: 0.0,
        }
    }

    fn image_item(x: i64, y: i64, w: i64, h: i64) -> Item {
        Item::Image {
            rect: Rect { x, y, w, h },
            data: vec![],
            crop: None,
            rotation_deg: 0.0,
        }
    }

    #[test]
    fn elements_listed_with_correct_bboxes_and_fractions() {
        let scene = make_scene(vec![
            text_item(0, 0, SLIDE_W / 2, SLIDE_H / 2),
            shape_item(SLIDE_W / 2, 0, SLIDE_W / 2, SLIDE_H),
        ]);

        let report = analyze_layout(&scene);

        assert_eq!(report.elements.len(), 2);

        // First element: text at origin, half-width, half-height.
        let e0 = &report.elements[0];
        assert_eq!(e0.index, 0);
        assert_eq!(e0.kind, ElementKind::Text);
        assert_eq!(
            e0.bbox_emu,
            Rect {
                x: 0,
                y: 0,
                w: SLIDE_W / 2,
                h: SLIDE_H / 2
            }
        );
        assert!((e0.bbox_fraction.0 - 0.0).abs() < 1e-9);
        assert!((e0.bbox_fraction.1 - 0.0).abs() < 1e-9);
        assert!((e0.bbox_fraction.2 - 0.5).abs() < 1e-9);
        assert!((e0.bbox_fraction.3 - 0.5).abs() < 1e-9);
        assert_eq!(e0.z_order, 0);

        // Second element: shape at right half, full height.
        let e1 = &report.elements[1];
        assert_eq!(e1.index, 1);
        assert_eq!(e1.kind, ElementKind::Shape);
        assert!((e1.bbox_fraction.0 - 0.5).abs() < 1e-9);
        assert!((e1.bbox_fraction.2 - 0.5).abs() < 1e-9);
        assert!((e1.bbox_fraction.3 - 1.0).abs() < 1e-9);
        assert_eq!(e1.z_order, 1);
    }

    #[test]
    fn off_canvas_detection() {
        // Element extends 1 EMU past the right edge.
        let scene = make_scene(vec![shape_item(SLIDE_W - 100, 0, 200, 1_000_000)]);

        let report = analyze_layout(&scene);
        let off_canvas: Vec<_> = report
            .findings
            .iter()
            .filter(|f| f.kind == FindingKind::OffCanvas)
            .collect();

        assert_eq!(off_canvas.len(), 1);
        assert_eq!(off_canvas[0].refs, vec![0]);
        assert_eq!(off_canvas[0].severity, Severity::Warning);
    }

    #[test]
    fn off_canvas_negative_position() {
        // Element starts at negative x.
        let scene = make_scene(vec![text_item(-100_000, 500_000, 2_000_000, 1_000_000)]);

        let report = analyze_layout(&scene);
        let off_canvas: Vec<_> = report
            .findings
            .iter()
            .filter(|f| f.kind == FindingKind::OffCanvas)
            .collect();

        assert_eq!(off_canvas.len(), 1);
    }

    #[test]
    fn no_off_canvas_for_fully_inside_element() {
        let scene = make_scene(vec![shape_item(1_000_000, 1_000_000, 2_000_000, 2_000_000)]);

        let report = analyze_layout(&scene);
        let off_canvas: Vec<_> = report
            .findings
            .iter()
            .filter(|f| f.kind == FindingKind::OffCanvas)
            .collect();

        assert!(off_canvas.is_empty());
    }

    #[test]
    fn overlap_detection_above_threshold() {
        // Two elements that overlap significantly (same position and size).
        let scene = make_scene(vec![
            text_item(1_000_000, 1_000_000, 4_000_000, 2_000_000),
            shape_item(1_000_000, 1_000_000, 4_000_000, 2_000_000),
        ]);

        let report = analyze_layout(&scene);
        let overlaps: Vec<_> = report
            .findings
            .iter()
            .filter(|f| f.kind == FindingKind::Overlap)
            .collect();

        assert_eq!(overlaps.len(), 1);
        assert_eq!(overlaps[0].refs, vec![0, 1]);
        assert!(overlaps[0].message.contains("text-over-shape"));
    }

    #[test]
    fn overlap_detection_below_threshold() {
        // Two elements that barely touch (1 EMU overlap in x).
        let scene = make_scene(vec![
            text_item(0, 0, 1_000_001, 2_000_000),
            shape_item(1_000_000, 0, 4_000_000, 2_000_000),
        ]);

        let report = analyze_layout(&scene);
        let overlaps: Vec<_> = report
            .findings
            .iter()
            .filter(|f| f.kind == FindingKind::Overlap)
            .collect();

        // 1 EMU × 2_000_000 EMU = 2_000_000 sq EMU intersection.
        // Smaller area = 1_000_001 × 2_000_000 = 2_000_002_000_000.
        // Ratio ≈ 0.000001 — well below 10% threshold.
        assert!(overlaps.is_empty());
    }

    #[test]
    fn zero_area_detection() {
        let scene = make_scene(vec![
            text_item(100_000, 100_000, 0, 500_000),  // zero width
            shape_item(200_000, 200_000, 500_000, 0), // zero height
            image_item(300_000, 300_000, 1_000_000, 1_000_000), // normal
        ]);

        let report = analyze_layout(&scene);
        let zero_area: Vec<_> = report
            .findings
            .iter()
            .filter(|f| f.kind == FindingKind::ZeroArea)
            .collect();

        assert_eq!(zero_area.len(), 2);
        assert_eq!(zero_area[0].refs, vec![0]);
        assert_eq!(zero_area[1].refs, vec![1]);
        assert_eq!(zero_area[0].severity, Severity::Error);
    }

    #[test]
    fn margin_violation_detection() {
        // Element very close to the left edge (within 5% of slide width).
        // 5% of SLIDE_W = 609_600 EMU. Place element at x=100_000 (< 609_600).
        let scene = make_scene(vec![text_item(100_000, 1_000_000, 2_000_000, 1_000_000)]);

        let report = analyze_layout(&scene);
        let margins: Vec<_> = report
            .findings
            .iter()
            .filter(|f| f.kind == FindingKind::MarginViolation)
            .collect();

        assert_eq!(margins.len(), 1);
        assert_eq!(margins[0].refs, vec![0]);
        assert_eq!(margins[0].severity, Severity::Info);
    }

    #[test]
    fn no_margin_violation_for_well_placed_element() {
        // Element well within margins (centered).
        let scene = make_scene(vec![shape_item(2_000_000, 2_000_000, 4_000_000, 2_000_000)]);

        let report = analyze_layout(&scene);
        let margins: Vec<_> = report
            .findings
            .iter()
            .filter(|f| f.kind == FindingKind::MarginViolation)
            .collect();

        assert!(margins.is_empty());
    }

    #[test]
    fn report_is_deterministic() {
        let items = vec![
            text_item(0, 0, 4_000_000, 2_000_000),
            shape_item(3_000_000, 1_000_000, 4_000_000, 2_000_000),
            image_item(8_000_000, 4_000_000, 5_000_000, 3_000_000),
        ];

        let scene = make_scene(items.clone());
        let report1 = analyze_layout(&scene);

        // Run again — must produce identical output.
        let scene2 = make_scene(items);
        let report2 = analyze_layout(&scene2);

        assert_eq!(report1, report2);
    }

    #[test]
    fn text_over_text_classification() {
        let scene = make_scene(vec![
            text_item(1_000_000, 1_000_000, 3_000_000, 2_000_000),
            text_item(2_000_000, 1_500_000, 3_000_000, 2_000_000),
        ]);

        let report = analyze_layout(&scene);
        let overlaps: Vec<_> = report
            .findings
            .iter()
            .filter(|f| f.kind == FindingKind::Overlap)
            .collect();

        assert_eq!(overlaps.len(), 1);
        assert!(overlaps[0].message.contains("text-over-text"));
    }

    #[test]
    fn empty_scene_produces_empty_report() {
        let scene = make_scene(vec![]);
        let report = analyze_layout(&scene);

        assert!(report.elements.is_empty());
        assert!(report.findings.is_empty());
    }

    // -----------------------------------------------------------------------
    // Contrast + min-size tests
    // -----------------------------------------------------------------------

    #[test]
    fn relative_luminance_black_is_zero() {
        let lum = relative_luminance(Color::BLACK);
        assert!((lum - 0.0).abs() < 1e-9);
    }

    #[test]
    fn relative_luminance_white_is_one() {
        let lum = relative_luminance(Color::WHITE);
        assert!((lum - 1.0).abs() < 1e-9);
    }

    #[test]
    fn relative_luminance_known_values() {
        // Pure red: 0.2126 * linearize(255) = 0.2126
        let red = Color { r: 255, g: 0, b: 0 };
        let lum = relative_luminance(red);
        assert!((lum - 0.2126).abs() < 0.001);

        // Pure green: 0.7152 * linearize(255) = 0.7152
        let green = Color { r: 0, g: 255, b: 0 };
        let lum = relative_luminance(green);
        assert!((lum - 0.7152).abs() < 0.001);

        // Pure blue: 0.0722 * linearize(255) = 0.0722
        let blue = Color { r: 0, g: 0, b: 255 };
        let lum = relative_luminance(blue);
        assert!((lum - 0.0722).abs() < 0.001);
    }

    #[test]
    fn contrast_ratio_black_white_is_21() {
        let ratio = contrast_ratio(Color::BLACK, Color::WHITE);
        assert!((ratio - 21.0).abs() < 0.01);
    }

    #[test]
    fn contrast_ratio_same_color_is_1() {
        let color = Color {
            r: 128,
            g: 64,
            b: 200,
        };
        let ratio = contrast_ratio(color, color);
        assert!((ratio - 1.0).abs() < 1e-9);
    }

    #[test]
    fn contrast_ratio_is_symmetric() {
        let a = Color {
            r: 100,
            g: 50,
            b: 200,
        };
        let b = Color {
            r: 200,
            g: 220,
            b: 240,
        };
        let ratio_ab = contrast_ratio(a, b);
        let ratio_ba = contrast_ratio(b, a);
        assert!((ratio_ab - ratio_ba).abs() < 1e-9);
    }

    fn text_item_with_lines(
        x: i64,
        y: i64,
        w: i64,
        h: i64,
        lines: Vec<zavora_slide_layout::TextLine>,
    ) -> Item {
        Item::Text {
            rect: Rect { x, y, w, h },
            lines,
            props: Default::default(),
        }
    }

    fn make_text_line(size_pt: f64, color: Color) -> zavora_slide_layout::TextLine {
        zavora_slide_layout::TextLine {
            text: "Hello".to_string(),
            size_pt,
            color,
            ..Default::default()
        }
    }

    #[test]
    fn low_contrast_text_is_flagged() {
        // Light gray text on white background → low contrast.
        let light_gray = Color {
            r: 200,
            g: 200,
            b: 200,
        };
        let scene = make_scene(vec![text_item_with_lines(
            1_000_000,
            1_000_000,
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

        assert_eq!(low_contrast.len(), 1);
        assert_eq!(low_contrast[0].severity, Severity::Warning);
        assert_eq!(low_contrast[0].refs, vec![0]);
    }

    #[test]
    fn high_contrast_text_is_not_flagged() {
        // Black text on white background → high contrast (21:1).
        let scene = make_scene(vec![text_item_with_lines(
            1_000_000,
            1_000_000,
            4_000_000,
            2_000_000,
            vec![make_text_line(18.0, Color::BLACK)],
        )]);

        let config = ContrastConfig::default();
        let findings = check_contrast(&scene, &config);

        let low_contrast: Vec<_> = findings
            .iter()
            .filter(|f| f.kind == FindingKind::LowContrast)
            .collect();

        assert!(low_contrast.is_empty());
    }

    #[test]
    fn small_font_size_is_flagged() {
        // 8pt text (below default 10pt minimum).
        let scene = make_scene(vec![text_item_with_lines(
            1_000_000,
            1_000_000,
            4_000_000,
            2_000_000,
            vec![make_text_line(8.0, Color::BLACK)],
        )]);

        let config = ContrastConfig::default();
        let findings = check_contrast(&scene, &config);

        let small_font: Vec<_> = findings
            .iter()
            .filter(|f| f.kind == FindingKind::SmallFont)
            .collect();

        assert_eq!(small_font.len(), 1);
        assert_eq!(small_font[0].severity, Severity::Warning);
        assert_eq!(small_font[0].refs, vec![0]);
    }

    #[test]
    fn adequate_font_size_is_not_flagged() {
        // 12pt text (above default 10pt minimum).
        let scene = make_scene(vec![text_item_with_lines(
            1_000_000,
            1_000_000,
            4_000_000,
            2_000_000,
            vec![make_text_line(12.0, Color::BLACK)],
        )]);

        let config = ContrastConfig::default();
        let findings = check_contrast(&scene, &config);

        let small_font: Vec<_> = findings
            .iter()
            .filter(|f| f.kind == FindingKind::SmallFont)
            .collect();

        assert!(small_font.is_empty());
    }

    #[test]
    fn custom_config_thresholds_work() {
        // Use a stricter config: 7:1 ratio, 14pt minimum.
        let config = ContrastConfig {
            min_ratio: 7.0,
            min_font_size_pt: 14.0,
        };

        // Dark gray on white: contrast ~5.7:1 (below 7:1 but above default 4.5:1).
        let dark_gray = Color {
            r: 90,
            g: 90,
            b: 90,
        };
        let scene = make_scene(vec![text_item_with_lines(
            1_000_000,
            1_000_000,
            4_000_000,
            2_000_000,
            vec![make_text_line(12.0, dark_gray)],
        )]);

        let findings = check_contrast(&scene, &config);

        // Should flag both low contrast (below 7:1) and small font (12pt < 14pt).
        let low_contrast: Vec<_> = findings
            .iter()
            .filter(|f| f.kind == FindingKind::LowContrast)
            .collect();
        let small_font: Vec<_> = findings
            .iter()
            .filter(|f| f.kind == FindingKind::SmallFont)
            .collect();

        assert_eq!(low_contrast.len(), 1);
        assert_eq!(small_font.len(), 1);
    }

    #[test]
    fn contrast_uses_underlapping_shape_fill_as_background() {
        // A blue shape underneath white text → should use blue as background.
        let blue = Color { r: 0, g: 0, b: 128 };
        let scene = Scene {
            width_emu: SLIDE_W,
            height_emu: SLIDE_H,
            background: None,
            rich_background: None,
            items: vec![
                // Shape fill underneath the text.
                Item::Shape {
                    rect: Rect {
                        x: 500_000,
                        y: 500_000,
                        w: 5_000_000,
                        h: 3_000_000,
                    },
                    preset: None,
                    fill: ShapeFill::Solid(blue),
                    outline: None,
                    rotation_deg: 0.0,
                },
                // White text on top of the blue shape.
                text_item_with_lines(
                    1_000_000,
                    1_000_000,
                    3_000_000,
                    1_000_000,
                    vec![make_text_line(18.0, Color::WHITE)],
                ),
            ],
            item_sources: Vec::new(),
        };

        let config = ContrastConfig::default();
        let findings = check_contrast(&scene, &config);

        // White on dark blue has high contrast — should NOT be flagged.
        let low_contrast: Vec<_> = findings
            .iter()
            .filter(|f| f.kind == FindingKind::LowContrast)
            .collect();

        assert!(low_contrast.is_empty());
    }

    #[test]
    fn contrast_uses_slide_background_when_no_underlapping_shape() {
        // Dark slide background with white text → high contrast, no flag.
        let dark_bg = Color {
            r: 30,
            g: 30,
            b: 30,
        };
        let scene = Scene {
            width_emu: SLIDE_W,
            height_emu: SLIDE_H,
            background: Some(dark_bg),
            rich_background: None,
            items: vec![text_item_with_lines(
                1_000_000,
                1_000_000,
                4_000_000,
                2_000_000,
                vec![make_text_line(18.0, Color::WHITE)],
            )],
            item_sources: Vec::new(),
        };

        let config = ContrastConfig::default();
        let findings = check_contrast(&scene, &config);

        let low_contrast: Vec<_> = findings
            .iter()
            .filter(|f| f.kind == FindingKind::LowContrast)
            .collect();

        assert!(low_contrast.is_empty());
    }

    #[test]
    fn contrast_default_config_values() {
        let config = ContrastConfig::default();
        assert!((config.min_ratio - 4.5).abs() < 1e-9);
        assert!((config.min_font_size_pt - 10.0).abs() < 1e-9);
    }

    // -----------------------------------------------------------------------
    // Render-diff tests
    // -----------------------------------------------------------------------

    /// Create a minimal valid PNG of the given dimensions filled with a solid color.
    fn make_solid_png(width: u32, height: u32, r: u8, g: u8, b: u8) -> Vec<u8> {
        let mut buf = Vec::new();
        {
            let mut encoder = png::Encoder::new(&mut buf, width, height);
            encoder.set_color(png::ColorType::Rgba);
            encoder.set_depth(png::BitDepth::Eight);
            let mut writer = encoder.write_header().unwrap();
            let row_size = (width as usize) * 4;
            let mut data = vec![0u8; row_size * height as usize];
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

    /// Create a PNG with a colored rectangle in a specific region.
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

    #[test]
    fn render_diff_identical_images_zero_diff() {
        let png = make_solid_png(64, 64, 128, 128, 128);
        let diff = compute_render_diff(&png, &png, 16).unwrap();

        assert!((diff.total_change_fraction - 0.0).abs() < 1e-9);
        assert!(diff.changed_regions.is_empty());
    }

    #[test]
    fn render_diff_completely_different_images_high_diff() {
        let before = make_solid_png(64, 64, 0, 0, 0);
        let after = make_solid_png(64, 64, 255, 255, 255);
        let diff = compute_render_diff(&before, &after, 16).unwrap();

        // Every pixel differs → total_change_fraction should be 1.0
        assert!((diff.total_change_fraction - 1.0).abs() < 1e-9);
        // All tiles should be reported as changed
        let expected_tiles = (64 / 16) * (64 / 16); // 4×4 = 16
        assert_eq!(diff.changed_regions.len(), expected_tiles as usize);
        // Each tile should have change_fraction = 1.0
        for region in &diff.changed_regions {
            assert!((region.change_fraction - 1.0).abs() < 1e-9);
            assert_eq!(region.w, 16);
            assert_eq!(region.h, 16);
        }
    }

    #[test]
    fn render_diff_localized_change_correct_region() {
        // 64×64 image, tile_size=32 → 2×2 grid of tiles
        // Change only in the top-left quadrant (0,0)-(32,32)
        let before = make_solid_png(64, 64, 100, 100, 100);
        let after = make_png_with_rect(64, 64, 100, 100, 100, 0, 0, 32, 32, 255, 0, 0);

        let diff = compute_render_diff(&before, &after, 32).unwrap();

        // Only the top-left tile should be changed
        assert_eq!(diff.changed_regions.len(), 1);
        let region = &diff.changed_regions[0];
        assert_eq!(region.x, 0);
        assert_eq!(region.y, 0);
        assert_eq!(region.w, 32);
        assert_eq!(region.h, 32);
        assert!((region.change_fraction - 1.0).abs() < 1e-9);

        // Total change should be 25% (one quadrant of four)
        assert!((diff.total_change_fraction - 0.25).abs() < 1e-9);
    }

    #[test]
    fn render_diff_different_dimensions_error() {
        let before = make_solid_png(64, 64, 0, 0, 0);
        let after = make_solid_png(128, 64, 0, 0, 0);

        let result = compute_render_diff(&before, &after, 16);
        assert!(result.is_err());
        let err = result.unwrap_err();
        assert!(matches!(err, RenderDiffError::DimensionMismatch { .. }));
    }

    #[test]
    fn render_diff_non_aligned_tile_size() {
        // 50×50 image with tile_size=32 → tiles are 32×32, 18×32, 32×18, 18×18
        let before = make_solid_png(50, 50, 0, 0, 0);
        let after = make_solid_png(50, 50, 255, 255, 255);

        let diff = compute_render_diff(&before, &after, 32).unwrap();

        // Should have 4 tiles (2×2 grid)
        assert_eq!(diff.changed_regions.len(), 4);
        // All should be fully changed
        for region in &diff.changed_regions {
            assert!((region.change_fraction - 1.0).abs() < 1e-9);
        }
        // Check edge tile dimensions
        let bottom_right = diff
            .changed_regions
            .iter()
            .find(|r| r.x == 32 && r.y == 32)
            .expect("should have bottom-right tile");
        assert_eq!(bottom_right.w, 18);
        assert_eq!(bottom_right.h, 18);
    }

    #[test]
    fn render_diff_sub_threshold_noise_ignored() {
        // Create two images that differ by only 1 per channel (below PIXEL_DIFF_THRESHOLD=4)
        let before = make_solid_png(32, 32, 100, 100, 100);
        // Manually create an "after" with tiny differences
        let mut buf = Vec::new();
        {
            let mut encoder = png::Encoder::new(&mut buf, 32, 32);
            encoder.set_color(png::ColorType::Rgba);
            encoder.set_depth(png::BitDepth::Eight);
            let mut writer = encoder.write_header().unwrap();
            let pixel_count = 32 * 32;
            let mut data = vec![0u8; pixel_count * 4];
            for pixel in data.chunks_mut(4) {
                pixel[0] = 101; // +1 from before
                pixel[1] = 101;
                pixel[2] = 101;
                pixel[3] = 255;
            }
            writer.write_image_data(&data).unwrap();
        }

        let diff = compute_render_diff(&before, &buf, 16).unwrap();
        // Differences of 1 per channel should be below the threshold
        assert!((diff.total_change_fraction - 0.0).abs() < 1e-9);
        assert!(diff.changed_regions.is_empty());
    }

    #[test]
    fn render_diff_invalid_png_error() {
        let garbage = b"not a png file";
        let valid = make_solid_png(32, 32, 0, 0, 0);

        let result = compute_render_diff(garbage, &valid, 16);
        assert!(result.is_err());
        assert!(matches!(result.unwrap_err(), RenderDiffError::PngDecode(_)));
    }

    // -----------------------------------------------------------------------
    // Design lint tests (Part J — Req 24)
    // -----------------------------------------------------------------------

    fn make_title_text_line() -> zavora_slide_layout::TextLine {
        zavora_slide_layout::TextLine {
            text: "Title".to_string(),
            size_pt: 36.0,
            color: Color::BLACK,
            bold: true,
            font_family: "Arial".to_string(),
            alignment: zavora_slide_layout::Alignment::Left,
            ..Default::default()
        }
    }

    fn make_body_text_line_centered() -> zavora_slide_layout::TextLine {
        zavora_slide_layout::TextLine {
            text: "Body text".to_string(),
            size_pt: 18.0,
            color: Color::BLACK,
            bold: false,
            font_family: "Arial".to_string(),
            alignment: zavora_slide_layout::Alignment::Center,
            ..Default::default()
        }
    }

    fn make_body_text_line_left() -> zavora_slide_layout::TextLine {
        zavora_slide_layout::TextLine {
            text: "Body text".to_string(),
            size_pt: 18.0,
            color: Color::BLACK,
            bold: false,
            font_family: "Arial".to_string(),
            alignment: zavora_slide_layout::Alignment::Left,
            ..Default::default()
        }
    }

    #[test]
    fn design_lint_text_only_slide_flagged() {
        // Slide with only text items — no shape or image.
        let scene = make_scene(vec![
            text_item_with_lines(
                1_000_000,
                1_000_000,
                4_000_000,
                2_000_000,
                vec![make_title_text_line()],
            ),
            text_item_with_lines(
                1_000_000,
                3_000_000,
                4_000_000,
                2_000_000,
                vec![make_body_text_line_left()],
            ),
        ]);

        let findings = design_lint(&scene);
        let text_only: Vec<_> = findings
            .iter()
            .filter(|f| f.kind == FindingKind::TextOnlySlide)
            .collect();

        assert_eq!(text_only.len(), 1);
        assert_eq!(text_only[0].severity, Severity::Warning);
        assert_eq!(text_only[0].refs, vec![0, 1]);
    }

    #[test]
    fn design_lint_slide_with_visual_not_flagged_text_only() {
        // Slide with text + a shape — should NOT be flagged as text-only.
        let scene = make_scene(vec![
            text_item_with_lines(
                1_000_000,
                1_000_000,
                4_000_000,
                2_000_000,
                vec![make_title_text_line()],
            ),
            shape_item(6_000_000, 1_000_000, 3_000_000, 3_000_000),
        ]);

        let findings = design_lint(&scene);
        let text_only: Vec<_> = findings
            .iter()
            .filter(|f| f.kind == FindingKind::TextOnlySlide)
            .collect();

        assert!(text_only.is_empty());
    }

    #[test]
    fn design_lint_centered_body_flagged() {
        // Body text (non-title) with center alignment.
        let scene = make_scene(vec![
            text_item_with_lines(
                1_000_000,
                1_000_000,
                4_000_000,
                2_000_000,
                vec![make_title_text_line()],
            ),
            text_item_with_lines(
                1_000_000,
                3_000_000,
                4_000_000,
                2_000_000,
                vec![make_body_text_line_centered()],
            ),
            shape_item(6_000_000, 1_000_000, 3_000_000, 3_000_000),
        ]);

        let findings = design_lint(&scene);
        let centered: Vec<_> = findings
            .iter()
            .filter(|f| f.kind == FindingKind::CenteredBody)
            .collect();

        assert_eq!(centered.len(), 1);
        assert_eq!(centered[0].refs, vec![1]);
    }

    #[test]
    fn design_lint_left_aligned_body_not_flagged() {
        // Body text with left alignment — should NOT be flagged.
        let scene = make_scene(vec![
            text_item_with_lines(
                1_000_000,
                1_000_000,
                4_000_000,
                2_000_000,
                vec![make_title_text_line()],
            ),
            text_item_with_lines(
                1_000_000,
                3_000_000,
                4_000_000,
                2_000_000,
                vec![make_body_text_line_left()],
            ),
            shape_item(6_000_000, 1_000_000, 3_000_000, 3_000_000),
        ]);

        let findings = design_lint(&scene);
        let centered: Vec<_> = findings
            .iter()
            .filter(|f| f.kind == FindingKind::CenteredBody)
            .collect();

        assert!(centered.is_empty());
    }

    #[test]
    fn design_lint_too_many_fonts_flagged() {
        // Slide with 4 distinct font families (exceeds default max of 3).
        let line_arial = zavora_slide_layout::TextLine {
            text: "A".to_string(),
            font_family: "Arial".to_string(),
            size_pt: 18.0,
            bold: true,
            ..Default::default()
        };
        let line_georgia = zavora_slide_layout::TextLine {
            text: "B".to_string(),
            font_family: "Georgia".to_string(),
            size_pt: 14.0,
            ..Default::default()
        };
        let line_roboto = zavora_slide_layout::TextLine {
            text: "C".to_string(),
            font_family: "Roboto".to_string(),
            size_pt: 14.0,
            ..Default::default()
        };
        let line_courier = zavora_slide_layout::TextLine {
            text: "D".to_string(),
            font_family: "Courier New".to_string(),
            size_pt: 14.0,
            ..Default::default()
        };

        let scene = make_scene(vec![
            text_item_with_lines(1_000_000, 500_000, 4_000_000, 1_000_000, vec![line_arial]),
            text_item_with_lines(
                1_000_000,
                2_000_000,
                4_000_000,
                1_000_000,
                vec![line_georgia],
            ),
            text_item_with_lines(
                1_000_000,
                3_500_000,
                4_000_000,
                1_000_000,
                vec![line_roboto],
            ),
            text_item_with_lines(
                1_000_000,
                5_000_000,
                4_000_000,
                1_000_000,
                vec![line_courier],
            ),
            shape_item(8_000_000, 1_000_000, 2_000_000, 4_000_000),
        ]);

        let findings = design_lint(&scene);
        let too_many: Vec<_> = findings
            .iter()
            .filter(|f| f.kind == FindingKind::TooManyFonts)
            .collect();

        assert_eq!(too_many.len(), 1);
        assert_eq!(too_many[0].severity, Severity::Warning);
        assert!(too_many[0].message.contains("4 distinct font families"));
    }

    #[test]
    fn design_lint_undersized_title_flagged() {
        // Title at 24pt (bold) but body at 24pt too — title not larger.
        let title_line = zavora_slide_layout::TextLine {
            text: "Title".to_string(),
            size_pt: 24.0,
            bold: true,
            font_family: "Arial".to_string(),
            ..Default::default()
        };
        let body_line = zavora_slide_layout::TextLine {
            text: "Body".to_string(),
            size_pt: 24.0,
            bold: false,
            font_family: "Arial".to_string(),
            ..Default::default()
        };

        let scene = make_scene(vec![
            text_item_with_lines(1_000_000, 1_000_000, 4_000_000, 1_500_000, vec![title_line]),
            text_item_with_lines(1_000_000, 3_000_000, 4_000_000, 2_000_000, vec![body_line]),
            shape_item(6_000_000, 1_000_000, 3_000_000, 3_000_000),
        ]);

        let findings = design_lint(&scene);
        let undersized: Vec<_> = findings
            .iter()
            .filter(|f| f.kind == FindingKind::UndersizedTitle)
            .collect();

        assert_eq!(undersized.len(), 1);
        assert_eq!(undersized[0].severity, Severity::Warning);
        assert!(undersized[0].message.contains("size hierarchy violated"));
    }

    #[test]
    fn design_lint_clean_slide_no_findings() {
        // A well-designed slide: title (36pt bold), body (18pt left-aligned),
        // 2 fonts (within limit), and a visual element.
        let title_line = zavora_slide_layout::TextLine {
            text: "Good Title".to_string(),
            size_pt: 36.0,
            bold: true,
            font_family: "Inter".to_string(),
            alignment: zavora_slide_layout::Alignment::Left,
            ..Default::default()
        };
        let body_line = zavora_slide_layout::TextLine {
            text: "Good body text".to_string(),
            size_pt: 18.0,
            bold: false,
            font_family: "Inter".to_string(),
            alignment: zavora_slide_layout::Alignment::Left,
            ..Default::default()
        };

        let scene = make_scene(vec![
            text_item_with_lines(1_000_000, 1_000_000, 8_000_000, 2_000_000, vec![title_line]),
            text_item_with_lines(1_000_000, 3_500_000, 8_000_000, 2_000_000, vec![body_line]),
            shape_item(1_000_000, 6_000_000, 8_000_000, 500_000),
        ]);

        let findings = design_lint(&scene);
        assert!(
            findings.is_empty(),
            "Clean slide should produce no design lint findings, got: {:?}",
            findings
        );
    }
}
