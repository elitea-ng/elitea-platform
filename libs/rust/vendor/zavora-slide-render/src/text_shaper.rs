//! Real text shaping via rustybuzz + fontdb.
//!
//! Replaces the character-width estimate (`0.55em`) with measured glyph advances
//! for accurate word-wrapping, alignment, indent, line/paragraph spacing, bullets,
//! vertical anchoring, run formatting, and autofit font scaling.
//!
//! # Architecture
//!
//! [`TextShaper`] owns a `fontdb::Database` loaded with bundled and system fonts.
//! For each text run it:
//! 1. Selects the appropriate font face (family + bold/italic).
//! 2. Measures glyph advances via `rustybuzz::shape`.
//! 3. Computes word-level widths for greedy line-breaking.
//! 4. Applies paragraph alignment, indent, spacing, and vertical anchor.
//!
//! The output is a list of [`ShapedLine`]s with pixel positions ready for SVG emission.

use fontdb::{Database, ID};
use zavora_slide_layout::{Alignment, TextFrameProps, TextLine, VerticalAnchor};

/// A shaped text run positioned for rendering.
#[derive(Debug, Clone)]
pub struct ShapedLine {
    /// X position in pixels.
    pub x: f64,
    /// Y position in pixels (baseline).
    pub y: f64,
    /// The text content of this line segment.
    pub text: String,
    /// Font size in pixels.
    pub font_size_px: f64,
    /// Font family name.
    pub font_family: String,
    /// Whether bold.
    pub bold: bool,
    /// Whether italic.
    pub italic: bool,
    /// Whether underlined.
    pub underline: bool,
    /// Fill color as hex string (e.g. "#FF0000").
    pub color_hex: String,
}

/// Text shaper using rustybuzz for glyph measurement.
pub struct TextShaper {
    db: Database,
}

impl TextShaper {
    /// Create a new text shaper with bundled fonts loaded.
    pub fn new() -> Self {
        let mut db = Database::new();
        // Load bundled fonts.
        db.load_font_data(include_bytes!("../fonts/LiberationSans-Regular.ttf").to_vec());
        db.load_font_data(include_bytes!("../fonts/LiberationSans-Bold.ttf").to_vec());
        // Elitea patch (libs/rust/vendor/README.md): no system fonts, so
        // measurement is the same on every host.
        Self { db }
    }

    /// Find the best font face ID for the given family/bold/italic combination.
    fn find_face(&self, family: &str, bold: bool, italic: bool) -> Option<ID> {
        let weight = if bold {
            fontdb::Weight(700)
        } else {
            fontdb::Weight(400)
        };
        let style = if italic {
            fontdb::Style::Italic
        } else {
            fontdb::Style::Normal
        };

        let families = if family.is_empty() {
            vec![fontdb::Family::SansSerif]
        } else {
            vec![fontdb::Family::Name(family), fontdb::Family::SansSerif]
        };

        let query = fontdb::Query {
            families: &families,
            weight,
            stretch: fontdb::Stretch::Normal,
            style,
        };

        self.db.query(&query)
    }

    /// Measure the width of a text string in pixels at the given font size.
    pub fn measure_text(
        &self,
        text: &str,
        font_size_px: f64,
        bold: bool,
        italic: bool,
        family: &str,
    ) -> f64 {
        if text.is_empty() {
            return 0.0;
        }

        let face_id = match self.find_face(family, bold, italic) {
            Some(id) => id,
            None => return text.len() as f64 * font_size_px * 0.55, // fallback estimate
        };

        self.db
            .with_face_data(face_id, |font_data, face_index| {
                let face = match rustybuzz::Face::from_slice(font_data, face_index) {
                    Some(f) => f,
                    None => return text.len() as f64 * font_size_px * 0.55,
                };

                let units_per_em = face.units_per_em() as f64;
                let scale = font_size_px / units_per_em;

                let mut buffer = rustybuzz::UnicodeBuffer::new();
                buffer.push_str(text);

                let glyphs = rustybuzz::shape(&face, &[], buffer);
                let total_advance: i32 = glyphs.glyph_positions().iter().map(|p| p.x_advance).sum();

                total_advance as f64 * scale
            })
            .unwrap_or(text.len() as f64 * font_size_px * 0.55)
    }

    /// Perform word-wrapping based on measured glyph widths.
    /// Returns a list of line strings that fit within `max_width_px`.
    pub fn wrap_text(
        &self,
        text: &str,
        font_size_px: f64,
        max_width_px: f64,
        bold: bool,
        italic: bool,
        family: &str,
    ) -> Vec<String> {
        if text.is_empty() {
            return vec![String::new()];
        }

        let words: Vec<&str> = text.split_whitespace().collect();
        if words.is_empty() {
            return vec![String::new()];
        }

        let space_width = self.measure_text(" ", font_size_px, bold, italic, family);
        let mut lines = Vec::new();
        let mut current_line = String::new();
        let mut current_width = 0.0;

        for word in &words {
            let word_width = self.measure_text(word, font_size_px, bold, italic, family);

            if current_line.is_empty() {
                // First word on the line always goes on.
                current_line.push_str(word);
                current_width = word_width;
            } else if current_width + space_width + word_width <= max_width_px {
                // Word fits on current line.
                current_line.push(' ');
                current_line.push_str(word);
                current_width += space_width + word_width;
            } else {
                // Word doesn't fit; start a new line.
                lines.push(std::mem::take(&mut current_line));
                current_line.push_str(word);
                current_width = word_width;
            }
        }

        if !current_line.is_empty() || lines.is_empty() {
            lines.push(current_line);
        }

        lines
    }

    /// Shape a text frame into positioned lines ready for SVG rendering.
    ///
    /// # Parameters
    /// - `lines`: The text lines from the scene item.
    /// - `props`: Text frame properties (anchor, font_scale).
    /// - `frame_x`, `frame_y`, `frame_w`, `frame_h`: Frame bounds in pixels.
    /// - `px_per_pt`: Conversion factor from points to pixels at current scale.
    #[allow(clippy::too_many_arguments)]
    pub fn shape_text_frame(
        &self,
        lines: &[TextLine],
        props: &TextFrameProps,
        frame_x: f64,
        frame_y: f64,
        frame_w: f64,
        frame_h: f64,
        px_per_pt: f64,
    ) -> Vec<ShapedLine> {
        let font_scale = if props.font_scale > 0.0 && props.font_scale <= 1.0 {
            props.font_scale
        } else {
            1.0
        };

        let mut result = Vec::new();
        // First pass: compute all shaped lines and total height.
        struct PendingLine {
            text: String,
            x: f64,
            font_size_px: f64,
            font_family: String,
            bold: bool,
            italic: bool,
            underline: bool,
            color_hex: String,
            line_height: f64,
            space_before: f64,
        }

        let mut pending: Vec<PendingLine> = Vec::new();
        let mut total_height = 0.0;

        for ln in lines {
            let effective_size_pt = ln.size_pt * font_scale;
            let font_px = effective_size_pt * px_per_pt;
            let line_height = font_px * ln.line_spacing.max(1.0);

            // Compute indent.
            let indent_px = ln.level as f64 * font_px * 1.5;
            let available_width = (frame_w - indent_px).max(font_px * 2.0);

            // Bullet prefix.
            let display_text = if ln.has_bullet {
                format!("• {}", ln.text)
            } else {
                ln.text.clone()
            };

            let family = if ln.font_family.is_empty() {
                "Liberation Sans"
            } else {
                &ln.font_family
            };

            // Wrap text.
            let wrapped = self.wrap_text(
                &display_text,
                font_px,
                available_width,
                ln.bold,
                ln.italic,
                family,
            );

            let color_hex = format!("#{:02X}{:02X}{:02X}", ln.color.r, ln.color.g, ln.color.b);

            // Space before (only for paragraph starts).
            let space_before = if ln.is_paragraph_start {
                ln.space_before_pt * px_per_pt
            } else {
                0.0
            };

            for (k, seg) in wrapped.into_iter().enumerate() {
                let seg_width = self.measure_text(&seg, font_px, ln.bold, ln.italic, family);

                // Compute x based on alignment.
                let base_x = frame_x + indent_px;
                let x = match ln.alignment {
                    Alignment::Left => {
                        if k == 0 {
                            base_x
                        } else {
                            base_x + font_px
                        }
                    }
                    Alignment::Center => {
                        let remaining = available_width - seg_width;
                        base_x + (remaining / 2.0).max(0.0)
                    }
                    Alignment::Right => {
                        let remaining = available_width - seg_width;
                        base_x + remaining.max(0.0)
                    }
                    Alignment::Justify => {
                        if k == 0 {
                            base_x
                        } else {
                            base_x + font_px
                        }
                    }
                };

                let sb = if k == 0 { space_before } else { 0.0 };
                total_height += sb + line_height;

                pending.push(PendingLine {
                    text: seg,
                    x,
                    font_size_px: font_px,
                    font_family: family.to_string(),
                    bold: ln.bold,
                    italic: ln.italic,
                    underline: ln.underline,
                    color_hex: color_hex.clone(),
                    line_height,
                    space_before: sb,
                });
            }

            // Space after (for paragraph ends — simplified: add after last line of paragraph).
            if ln.is_paragraph_start {
                total_height += ln.space_after_pt * px_per_pt;
            }
        }

        // Compute vertical offset based on anchor.
        let y_offset = match props.anchor {
            VerticalAnchor::Top => 0.0,
            VerticalAnchor::Middle => ((frame_h - total_height) / 2.0).max(0.0),
            VerticalAnchor::Bottom => (frame_h - total_height).max(0.0),
        };

        // Second pass: emit positioned lines.
        let mut cursor_y = frame_y + y_offset;
        for p in pending {
            cursor_y += p.space_before + p.line_height;
            result.push(ShapedLine {
                x: p.x,
                y: cursor_y,
                text: p.text,
                font_size_px: p.font_size_px,
                font_family: p.font_family,
                bold: p.bold,
                italic: p.italic,
                underline: p.underline,
                color_hex: p.color_hex,
            });
        }

        result
    }
}

impl Default for TextShaper {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn measure_text_nonzero() {
        let shaper = TextShaper::new();
        let width = shaper.measure_text("Hello World", 20.0, false, false, "Liberation Sans");
        assert!(
            width > 0.0,
            "measured width should be positive, got {width}"
        );
    }

    #[test]
    fn measure_text_proportional_to_length() {
        let shaper = TextShaper::new();
        let short = shaper.measure_text("Hi", 20.0, false, false, "Liberation Sans");
        let long = shaper.measure_text("Hello World", 20.0, false, false, "Liberation Sans");
        assert!(
            long > short,
            "longer text should be wider: {long} vs {short}"
        );
    }

    #[test]
    fn measure_text_empty_is_zero() {
        let shaper = TextShaper::new();
        let width = shaper.measure_text("", 20.0, false, false, "Liberation Sans");
        assert_eq!(width, 0.0);
    }

    #[test]
    fn wrap_breaks_at_word_boundary() {
        let shaper = TextShaper::new();
        // Use a narrow width that forces wrapping.
        let lines = shaper.wrap_text(
            "alpha beta gamma delta epsilon",
            20.0,
            80.0, // very narrow
            false,
            false,
            "Liberation Sans",
        );
        assert!(
            lines.len() > 1,
            "should wrap into multiple lines, got {lines:?}"
        );
        // Each line should contain complete words (no mid-word breaks).
        for line in &lines {
            for word in line.split_whitespace() {
                assert!(
                    "alpha beta gamma delta epsilon".contains(word),
                    "unexpected word fragment: {word}"
                );
            }
        }
    }

    #[test]
    fn wrap_single_word_never_empty() {
        let shaper = TextShaper::new();
        let lines = shaper.wrap_text("solo", 20.0, 10.0, false, false, "Liberation Sans");
        assert_eq!(lines.len(), 1);
        assert_eq!(lines[0], "solo");
    }

    #[test]
    fn wrap_empty_text() {
        let shaper = TextShaper::new();
        let lines = shaper.wrap_text("", 20.0, 100.0, false, false, "Liberation Sans");
        assert_eq!(lines, vec![String::new()]);
    }

    #[test]
    fn alignment_left_offset_is_near_frame_x() {
        let shaper = TextShaper::new();
        let lines = vec![TextLine {
            text: "Hello".into(),
            size_pt: 18.0,
            alignment: Alignment::Left,
            is_paragraph_start: true,
            ..TextLine::default()
        }];
        let props = TextFrameProps::default();
        let shaped = shaper.shape_text_frame(&lines, &props, 100.0, 50.0, 400.0, 200.0, 1.0);
        assert!(!shaped.is_empty());
        // Left-aligned text should start at frame_x (no indent for level 0).
        assert!((shaped[0].x - 100.0).abs() < 1.0, "x={}", shaped[0].x);
    }

    #[test]
    fn alignment_center_offset() {
        let shaper = TextShaper::new();
        let lines = vec![TextLine {
            text: "Hi".into(),
            size_pt: 18.0,
            alignment: Alignment::Center,
            is_paragraph_start: true,
            ..TextLine::default()
        }];
        let props = TextFrameProps::default();
        let shaped = shaper.shape_text_frame(&lines, &props, 100.0, 50.0, 400.0, 200.0, 1.0);
        assert!(!shaped.is_empty());
        // Center-aligned: x should be > frame_x (shifted right).
        assert!(
            shaped[0].x > 100.0,
            "center x should be > frame_x, got {}",
            shaped[0].x
        );
        // And less than frame_x + frame_w.
        assert!(shaped[0].x < 500.0);
    }

    #[test]
    fn alignment_right_offset() {
        let shaper = TextShaper::new();
        let lines = vec![TextLine {
            text: "Hi".into(),
            size_pt: 18.0,
            alignment: Alignment::Right,
            is_paragraph_start: true,
            ..TextLine::default()
        }];
        let props = TextFrameProps::default();
        let shaped = shaper.shape_text_frame(&lines, &props, 100.0, 50.0, 400.0, 200.0, 1.0);
        assert!(!shaped.is_empty());
        // Right-aligned: x should be significantly > frame_x.
        assert!(
            shaped[0].x > 200.0,
            "right x should be well past frame_x, got {}",
            shaped[0].x
        );
    }

    #[test]
    fn vertical_anchor_top() {
        let shaper = TextShaper::new();
        let lines = vec![TextLine {
            text: "Test".into(),
            size_pt: 18.0,
            is_paragraph_start: true,
            ..TextLine::default()
        }];
        let props = TextFrameProps {
            anchor: VerticalAnchor::Top,
            font_scale: 1.0,
        };
        let shaped = shaper.shape_text_frame(&lines, &props, 0.0, 100.0, 400.0, 400.0, 1.0);
        assert!(!shaped.is_empty());
        // Top anchor: first line y should be near frame_y + line_height.
        let expected_y = 100.0 + 18.0; // frame_y + font_size * line_spacing(1.0)
        assert!(
            (shaped[0].y - expected_y).abs() < 1.0,
            "y={}, expected ~{}",
            shaped[0].y,
            expected_y
        );
    }

    #[test]
    fn vertical_anchor_middle() {
        let shaper = TextShaper::new();
        let lines = vec![TextLine {
            text: "Test".into(),
            size_pt: 18.0,
            is_paragraph_start: true,
            ..TextLine::default()
        }];
        let props = TextFrameProps {
            anchor: VerticalAnchor::Middle,
            font_scale: 1.0,
        };
        let shaped = shaper.shape_text_frame(&lines, &props, 0.0, 0.0, 400.0, 400.0, 1.0);
        assert!(!shaped.is_empty());
        // Middle anchor: text should be roughly centered.
        // Total height = 18px, so offset = (400 - 18) / 2 = 191.
        assert!(
            shaped[0].y > 150.0,
            "middle y should be > 150, got {}",
            shaped[0].y
        );
        assert!(
            shaped[0].y < 250.0,
            "middle y should be < 250, got {}",
            shaped[0].y
        );
    }

    #[test]
    fn vertical_anchor_bottom() {
        let shaper = TextShaper::new();
        let lines = vec![TextLine {
            text: "Test".into(),
            size_pt: 18.0,
            is_paragraph_start: true,
            ..TextLine::default()
        }];
        let props = TextFrameProps {
            anchor: VerticalAnchor::Bottom,
            font_scale: 1.0,
        };
        let shaped = shaper.shape_text_frame(&lines, &props, 0.0, 0.0, 400.0, 400.0, 1.0);
        assert!(!shaped.is_empty());
        // Bottom anchor: text should be near the bottom.
        assert!(
            shaped[0].y > 350.0,
            "bottom y should be > 350, got {}",
            shaped[0].y
        );
    }

    #[test]
    fn font_scale_reduces_size() {
        let shaper = TextShaper::new();
        let lines = vec![TextLine {
            text: "Scaled".into(),
            size_pt: 36.0,
            is_paragraph_start: true,
            ..TextLine::default()
        }];
        let props_full = TextFrameProps {
            anchor: VerticalAnchor::Top,
            font_scale: 1.0,
        };
        let props_half = TextFrameProps {
            anchor: VerticalAnchor::Top,
            font_scale: 0.5,
        };

        let shaped_full = shaper.shape_text_frame(&lines, &props_full, 0.0, 0.0, 400.0, 400.0, 1.0);
        let shaped_half = shaper.shape_text_frame(&lines, &props_half, 0.0, 0.0, 400.0, 400.0, 1.0);

        assert!(!shaped_full.is_empty());
        assert!(!shaped_half.is_empty());
        // Half scale should produce half the font size.
        let ratio = shaped_half[0].font_size_px / shaped_full[0].font_size_px;
        assert!(
            (ratio - 0.5).abs() < 0.01,
            "ratio should be ~0.5, got {ratio}"
        );
    }
}
