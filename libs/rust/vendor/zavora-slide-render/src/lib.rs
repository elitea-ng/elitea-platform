//! Slide rasterization: [`Scene`] → SVG (primary, web-client friendly) and PNG
//! (via resvg). Text is laid out as simple top-anchored lines; full shaping is
//! delegated to resvg's font stack for PNG.

pub mod text_shaper;
pub mod theme_resolver;

pub use theme_resolver::{
    ColorMap, ColorModifier, ThemeColorScheme, ThemeFontScheme, apply_lum_mod, apply_lum_off,
    apply_modifiers, apply_shade, apply_tint,
};

use base64::Engine;
use zavora_slide_layout::{Background, Color, GradientFill, Item, Outline, Scene, ShapeFill};

#[derive(Debug, thiserror::Error)]
pub enum RenderError {
    #[error("PNG rasterization failed")]
    Raster,
    #[error("invalid SVG: {0}")]
    Svg(String),
}

fn esc(s: &str) -> String {
    s.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
}

fn rgb(c: Color) -> String {
    format!("#{:02X}{:02X}{:02X}", c.r, c.g, c.b)
}

/// Greedy word-wrap to a pixel width, estimating glyph advance at ~0.55em
/// (adequate for sans-serif without a full shaping pass). Always yields ≥1 line.
/// Kept for backward-compatibility tests; production code uses [`text_shaper`].
#[cfg(test)]
fn wrap(text: &str, font_px: f64, max_px: f64) -> Vec<String> {
    let char_w = font_px * 0.55;
    let max_chars = (max_px / char_w).floor().max(1.0) as usize;
    let mut lines = Vec::new();
    let mut cur = String::new();
    for word in text.split_whitespace() {
        if cur.is_empty() {
            cur.push_str(word);
        } else if cur.chars().count() + 1 + word.chars().count() <= max_chars {
            cur.push(' ');
            cur.push_str(word);
        } else {
            lines.push(std::mem::take(&mut cur));
            cur.push_str(word);
        }
    }
    if !cur.is_empty() || lines.is_empty() {
        lines.push(cur);
    }
    lines
}

/// How to render.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct SvgOptions {
    /// Wrap each scene item in `<g data-item="{index}">` so a client can tell what was
    /// clicked and which element a change refers to.
    ///
    /// Off by default, so existing output is byte-for-byte unchanged. This mirrors
    /// `zavora-docx-html`'s `HtmlOptions { editable }`, which emits `data-p` for the
    /// same reason: a rendered document is not editable unless each part of it can be
    /// named.
    pub identify: bool,
}

/// Render a scene to an SVG document string at the given pixel width.
pub fn scene_to_svg(scene: &Scene, target_px_w: u32) -> String {
    scene_to_svg_with(scene, target_px_w, SvgOptions::default())
}

/// Render a scene, choosing whether each item is identifiable.
pub fn scene_to_svg_with(scene: &Scene, target_px_w: u32, options: SvgOptions) -> String {
    let shaper = text_shaper::TextShaper::new();
    let h = scene.px_height(target_px_w);
    let sw = scene.width_emu;
    let mut s = String::new();
    let mut defs = String::new();
    let mut def_id: u32 = 0;

    s.push_str(&format!(
        "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"{target_px_w}\" height=\"{h}\" \
         viewBox=\"0 0 {target_px_w} {h}\">"
    ));

    // Background — rich_background takes precedence over legacy `background`.
    match &scene.rich_background {
        Some(Background::Solid(c)) => {
            s.push_str(&format!(
                "<rect width=\"{target_px_w}\" height=\"{h}\" fill=\"{}\"/>",
                rgb(*c)
            ));
        }
        Some(Background::Picture(data)) => {
            let mime = if data.starts_with(&[0xFF, 0xD8]) {
                "jpeg"
            } else {
                "png"
            };
            let b64 = base64::engine::general_purpose::STANDARD.encode(data);
            s.push_str(&format!(
                "<image x=\"0\" y=\"0\" width=\"{target_px_w}\" height=\"{h}\" \
                 href=\"data:image/{mime};base64,{b64}\" preserveAspectRatio=\"xMidYMid slice\"/>"
            ));
        }
        None => {
            let bg = scene.background.unwrap_or(Color::WHITE);
            s.push_str(&format!(
                "<rect width=\"{target_px_w}\" height=\"{h}\" fill=\"{}\"/>",
                rgb(bg)
            ));
        }
    }

    let scale = target_px_w as f64 / sw as f64;
    // 1 point = 12700 EMU, and `scale` is pixels-per-EMU.
    let px_per_pt = zavora_slide_layout::EMU_PER_POINT * scale;
    for (item_index, item) in scene.items.iter().enumerate() {
        if options.identify {
            // The item index says which drawn element this is; the source says what it
            // belongs to. A caller needs the second to change anything, because one shape
            // can be drawn as several items.
            let attribution = match scene.source_of(item_index) {
                Some(zavora_slide_layout::ItemSource::Shape(i)) => {
                    format!(" data-shape=\"{i}\"")
                }
                Some(zavora_slide_layout::ItemSource::Image(i)) => {
                    format!(" data-image=\"{i}\"")
                }
                Some(zavora_slide_layout::ItemSource::Table(i)) => {
                    format!(" data-table=\"{i}\"")
                }
                Some(zavora_slide_layout::ItemSource::Chart(i)) => {
                    format!(" data-chart=\"{i}\"")
                }
                Some(zavora_slide_layout::ItemSource::Background) => {
                    " data-background=\"true\"".to_string()
                }
                None => String::new(),
            };
            s.push_str(&format!("<g data-item=\"{item_index}\"{attribution}>"));
        }
        match item {
            Item::Rect {
                rect,
                fill,
                outline,
            } => {
                let (x, y, w, hh) = rect.to_px(sw, target_px_w);
                let fill_attr = fill.map(rgb).unwrap_or_else(|| "none".into());
                let (stroke, sw_attr) = match outline {
                    Some((c, pt)) => (rgb(*c), (*pt * px_per_pt).max(0.5)),
                    None => ("none".into(), 0.0),
                };
                s.push_str(&format!(
                    "<rect x=\"{x:.1}\" y=\"{y:.1}\" width=\"{w:.1}\" height=\"{hh:.1}\" \
                     fill=\"{fill_attr}\" stroke=\"{stroke}\" stroke-width=\"{sw_attr:.2}\"/>"
                ));
            }
            Item::Shape {
                rect,
                preset,
                fill,
                outline,
                rotation_deg,
            } => {
                let (x, y, w, hh) = rect.to_px(sw, target_px_w);
                let xf = x as f64;
                let yf = y as f64;
                let wf = w as f64;
                let hf = hh as f64;

                // Determine fill attribute/defs
                let fill_attr = match fill {
                    ShapeFill::Solid(c) => rgb(*c),
                    ShapeFill::Gradient(grad) => {
                        let gid = format!("grad{def_id}");
                        def_id += 1;
                        defs.push_str(&emit_gradient_def(&gid, grad));
                        format!("url(#{gid})")
                    }
                    ShapeFill::Picture(pic) => {
                        let pid = format!("pat{def_id}");
                        def_id += 1;
                        defs.push_str(&emit_pattern_def(&pid, &pic.data, wf, hf));
                        format!("url(#{pid})")
                    }
                    ShapeFill::None => "none".into(),
                };

                // Outline attributes
                let (stroke_attr, stroke_w, dash_attr) = outline_attrs(outline, px_per_pt);

                // Rotation transform
                let transform = if *rotation_deg != 0.0 {
                    let cx = xf + wf / 2.0;
                    let cy = yf + hf / 2.0;
                    format!(" transform=\"rotate({rotation_deg:.1},{cx:.1},{cy:.1})\"")
                } else {
                    String::new()
                };

                // Get path data
                let path_data = match preset.as_deref() {
                    Some(name) => {
                        zavora_slide_layout::preset_geometry::preset_path(name, xf, yf, wf, hf)
                            .unwrap_or_else(|| {
                                zavora_slide_layout::preset_geometry::bbox_rect_path(xf, yf, wf, hf)
                            })
                    }
                    None => zavora_slide_layout::preset_geometry::bbox_rect_path(xf, yf, wf, hf),
                };

                s.push_str(&format!(
                    "<path d=\"{path_data}\" fill=\"{fill_attr}\" \
                     stroke=\"{stroke_attr}\" stroke-width=\"{stroke_w:.2}\"{dash_attr}{transform}/>"
                ));
            }
            Item::Text { rect, lines, props } => {
                let (x, y, w, h) = rect.to_px(sw, target_px_w);
                let shaped = shaper.shape_text_frame(
                    lines, props, x as f64, y as f64, w as f64, h as f64, px_per_pt,
                );
                for sl in &shaped {
                    let weight = if sl.bold { " font-weight=\"bold\"" } else { "" };
                    let style = if sl.italic {
                        " font-style=\"italic\""
                    } else {
                        ""
                    };
                    let decoration = if sl.underline {
                        " text-decoration=\"underline\""
                    } else {
                        ""
                    };
                    let family = if sl.font_family.is_empty() {
                        "sans-serif"
                    } else {
                        &sl.font_family
                    };
                    s.push_str(&format!(
                        "<text x=\"{x:.1}\" y=\"{y:.1}\" font-family=\"{fam}\" \
                         font-size=\"{sz:.1}\" fill=\"{col}\"{weight}{style}{decoration}>{t}</text>",
                        x = sl.x,
                        y = sl.y,
                        fam = esc(family),
                        sz = sl.font_size_px,
                        col = sl.color_hex,
                        t = esc(&sl.text)
                    ));
                }
            }
            Item::Image {
                rect,
                data,
                crop,
                rotation_deg,
            } => {
                let (x, y, w, hh) = rect.to_px(sw, target_px_w);
                let mime = if data.starts_with(&[0xFF, 0xD8]) {
                    "jpeg"
                } else {
                    "png"
                };
                let b64 = base64::engine::general_purpose::STANDARD.encode(data);

                let has_crop = crop.as_ref().is_some_and(|c| c.is_cropped());
                let has_rotation = *rotation_deg != 0.0;

                if has_crop || has_rotation {
                    let crop = crop.unwrap_or_default();
                    let clip_id = format!("clip{def_id}");
                    def_id += 1;

                    // Compute the visible region after crop
                    let xf = x as f64;
                    let yf = y as f64;
                    let wf = w as f64;
                    let hf = hh as f64;

                    if has_crop {
                        // The clip rect is the visible area within the image bounds
                        let clip_x = xf;
                        let clip_y = yf;
                        let clip_w = wf;
                        let clip_h = hf;

                        defs.push_str(&format!(
                            "<clipPath id=\"{clip_id}\"><rect x=\"{clip_x:.1}\" y=\"{clip_y:.1}\" \
                             width=\"{clip_w:.1}\" height=\"{clip_h:.1}\"/></clipPath>"
                        ));

                        // The image is larger than the clip area to account for crop
                        let scale_w = wf / (1.0 - crop.left - crop.right);
                        let scale_h = hf / (1.0 - crop.top - crop.bottom);
                        let img_x = xf - crop.left * scale_w;
                        let img_y = yf - crop.top * scale_h;

                        let transform = if has_rotation {
                            let cx = xf + wf / 2.0;
                            let cy = yf + hf / 2.0;
                            format!(" transform=\"rotate({rotation_deg:.1},{cx:.1},{cy:.1})\"")
                        } else {
                            String::new()
                        };

                        s.push_str(&format!(
                            "<image x=\"{img_x:.1}\" y=\"{img_y:.1}\" width=\"{scale_w:.1}\" \
                             height=\"{scale_h:.1}\" href=\"data:image/{mime};base64,{b64}\" \
                             clip-path=\"url(#{clip_id})\"{transform}/>"
                        ));
                    } else {
                        // Rotation only, no crop
                        let cx = xf + wf / 2.0;
                        let cy = yf + hf / 2.0;
                        s.push_str(&format!(
                            "<image x=\"{x:.1}\" y=\"{y:.1}\" width=\"{w:.1}\" height=\"{hh:.1}\" \
                             href=\"data:image/{mime};base64,{b64}\" \
                             transform=\"rotate({rotation_deg:.1},{cx:.1},{cy:.1})\"/>"
                        ));
                    }
                } else {
                    s.push_str(&format!(
                        "<image x=\"{x:.1}\" y=\"{y:.1}\" width=\"{w:.1}\" height=\"{hh:.1}\" \
                         href=\"data:image/{mime};base64,{b64}\"/>"
                    ));
                }
            }
        }
        if options.identify {
            s.push_str("</g>");
        }
    }

    // Insert defs block if we have any
    if !defs.is_empty() {
        // Insert defs right after the opening <svg> tag
        let insert_pos = s.find('>').unwrap() + 1;
        s.insert_str(insert_pos, &format!("<defs>{defs}</defs>"));
    }

    s.push_str("</svg>");
    s
}

/// Emit an SVG gradient definition.
fn emit_gradient_def(id: &str, grad: &GradientFill) -> String {
    let mut d = String::new();
    if grad.is_radial {
        d.push_str(&format!("<radialGradient id=\"{id}\">"));
        for stop in &grad.stops {
            d.push_str(&format!(
                "<stop offset=\"{:.0}%\" stop-color=\"{}\"/>",
                stop.position * 100.0,
                rgb(stop.color)
            ));
        }
        d.push_str("</radialGradient>");
    } else {
        // Convert angle to x1,y1,x2,y2
        let angle_rad = grad.angle_deg.to_radians();
        let x1 = 50.0 - 50.0 * angle_rad.cos();
        let y1 = 50.0 - 50.0 * angle_rad.sin();
        let x2 = 50.0 + 50.0 * angle_rad.cos();
        let y2 = 50.0 + 50.0 * angle_rad.sin();
        d.push_str(&format!(
            "<linearGradient id=\"{id}\" x1=\"{x1:.1}%\" y1=\"{y1:.1}%\" \
             x2=\"{x2:.1}%\" y2=\"{y2:.1}%\">"
        ));
        for stop in &grad.stops {
            d.push_str(&format!(
                "<stop offset=\"{:.0}%\" stop-color=\"{}\"/>",
                stop.position * 100.0,
                rgb(stop.color)
            ));
        }
        d.push_str("</linearGradient>");
    }
    d
}

/// Emit an SVG pattern definition for a picture fill.
fn emit_pattern_def(id: &str, data: &[u8], w: f64, h: f64) -> String {
    let mime = if data.starts_with(&[0xFF, 0xD8]) {
        "jpeg"
    } else {
        "png"
    };
    let b64 = base64::engine::general_purpose::STANDARD.encode(data);
    format!(
        "<pattern id=\"{id}\" patternUnits=\"objectBoundingBox\" width=\"1\" height=\"1\">\
         <image width=\"{w:.1}\" height=\"{h:.1}\" href=\"data:image/{mime};base64,{b64}\"/>\
         </pattern>"
    )
}

/// Compute outline SVG attributes from an optional Outline.
fn outline_attrs(outline: &Option<Outline>, px_per_pt: f64) -> (String, f64, String) {
    match outline {
        Some(o) => {
            let stroke = rgb(o.color);
            let sw = (o.width_pt * px_per_pt).max(0.5);
            let dash = match o.dash.to_svg_dasharray() {
                Some(da) => format!(" stroke-dasharray=\"{da}\""),
                None => String::new(),
            };
            (stroke, sw, dash)
        }
        None => ("none".into(), 0.0, String::new()),
    }
}

/// Render a scene to PNG bytes at the given pixel width (via resvg).
pub fn scene_to_png(scene: &Scene, target_px_w: u32) -> Result<Vec<u8>, RenderError> {
    let svg = scene_to_svg(scene, target_px_w);
    let opt = build_options();
    let tree =
        resvg::usvg::Tree::from_str(&svg, &opt).map_err(|e| RenderError::Svg(e.to_string()))?;
    let size = tree.size().to_int_size();
    let mut pixmap =
        resvg::tiny_skia::Pixmap::new(size.width(), size.height()).ok_or(RenderError::Raster)?;
    resvg::render(
        &tree,
        resvg::tiny_skia::Transform::identity(),
        &mut pixmap.as_mut(),
    );
    pixmap.encode_png().map_err(|_| RenderError::Raster)
}

/// usvg options with a font database. With `bundled-fonts`, LiberationSans
/// (Arial-metric-compatible) is embedded and set as the default sans-serif, so
/// text rasterizes even on hosts with no installed fonts. System fonts are also
/// loaded so named families resolve when available.
fn build_options() -> resvg::usvg::Options<'static> {
    let mut opt = resvg::usvg::Options::default();
    let db = opt.fontdb_mut();
    // Elitea patch (libs/rust/vendor/README.md): bundled fonts only. Loading
    // the host's fonts made a render depend on the machine it ran on.
    #[cfg(feature = "bundled-fonts")]
    {
        db.load_font_data(include_bytes!("../fonts/LiberationSans-Regular.ttf").to_vec());
        db.load_font_data(include_bytes!("../fonts/LiberationSans-Bold.ttf").to_vec());
        // Default sans-serif → the bundled face, so text rasterizes even with no
        // system fonts. (Named families still resolve from system fonts above.)
        db.set_sans_serif_family("Liberation Sans");
    }
    opt
}

#[cfg(test)]
mod tests {
    use super::*;
    use zavora_slide_layout::{
        Background, DashStyle, GradientFill, GradientStop, ImageCrop, Item, Outline, PictureFill,
        Rect, Scene, ShapeFill, TextFrameProps, TextLine,
    };

    fn sample() -> Scene {
        let mut s = Scene::new(12192000, 6858000);
        s.background = Some(Color {
            r: 245,
            g: 245,
            b: 245,
        });
        s.items.push(Item::Rect {
            rect: Rect {
                x: 914400,
                y: 914400,
                w: 1828800,
                h: 914400,
            },
            fill: Some(Color {
                r: 68,
                g: 114,
                b: 196,
            }),
            outline: None,
        });
        s.items.push(Item::Text {
            rect: Rect {
                x: 914400,
                y: 457200,
                w: 9000000,
                h: 914400,
            },
            lines: vec![TextLine {
                text: "Title <&>".into(),
                size_pt: 32.0,
                color: Color::BLACK,
                bold: true,
                italic: false,
                level: 0,
                is_paragraph_start: true,
                ..TextLine::default()
            }],
            props: TextFrameProps::default(),
        });
        s
    }

    #[test]
    fn wrap_splits_and_never_empty() {
        let many = wrap("alpha beta gamma delta epsilon", 20.0, 80.0);
        assert!(many.len() > 1, "long text should wrap, got {many:?}");
        assert_eq!(wrap("solo", 20.0, 10.0), vec!["solo".to_string()]);
        assert_eq!(wrap("", 20.0, 100.0), vec![String::new()]);
    }

    #[test]
    fn svg_has_shapes_and_escaped_text() {
        let svg = scene_to_svg(&sample(), 1280);
        assert!(svg.starts_with("<svg"));
        assert!(svg.contains("width=\"1280\" height=\"720\""));
        assert!(svg.contains("fill=\"#4472C4\""));
        assert!(svg.contains("Title &lt;&amp;&gt;"));
        assert!(svg.contains("font-weight=\"bold\""));
    }

    #[test]
    fn png_renders_nonempty() {
        let png = scene_to_png(&sample(), 640).unwrap();
        assert!(png.len() > 100);
        assert_eq!(&png[1..4], b"PNG");
    }

    #[test]
    fn png_actually_renders_dark_text_pixels() {
        // Decode the PNG and confirm it contains dark pixels (text/shape), i.e.
        // glyphs actually rasterized rather than a blank canvas.
        let png = scene_to_png(&sample(), 640).unwrap();
        let mut dec = resvg::tiny_skia::Pixmap::decode_png(&png).unwrap();
        let dark = dec
            .pixels_mut()
            .iter()
            .filter(|p| p.red() < 100 && p.green() < 100 && p.blue() < 100 && p.alpha() > 0)
            .count();
        assert!(dark > 50, "expected rendered dark pixels, got {dark}");
    }

    #[cfg(feature = "bundled-fonts")]
    #[test]
    fn bundled_font_is_loaded() {
        let mut opt = build_options();
        let q = resvg::usvg::fontdb::Query {
            families: &[resvg::usvg::fontdb::Family::Name("Liberation Sans")],
            ..Default::default()
        };
        assert!(
            opt.fontdb_mut().query(&q).is_some(),
            "bundled Liberation Sans should be queryable"
        );
    }

    // --- Task 3.4: Shape/fill/image fidelity tests ---

    #[test]
    fn shape_with_preset_geometry_emits_path() {
        let mut s = Scene::new(12192000, 6858000);
        s.items.push(Item::Shape {
            rect: Rect {
                x: 914400,
                y: 914400,
                w: 1828800,
                h: 914400,
            },
            preset: Some("ellipse".into()),
            fill: ShapeFill::Solid(Color { r: 255, g: 0, b: 0 }),
            outline: None,
            rotation_deg: 0.0,
        });
        let svg = scene_to_svg(&s, 1280);
        assert!(svg.contains("<path"), "shape should emit a <path> element");
        assert!(svg.contains("fill=\"#FF0000\""));
        assert!(svg.contains("C"), "ellipse should have cubic curves");
    }

    #[test]
    fn shape_unknown_preset_falls_back_to_bbox() {
        let mut s = Scene::new(12192000, 6858000);
        s.items.push(Item::Shape {
            rect: Rect {
                x: 0,
                y: 0,
                w: 4572000,
                h: 2286000,
            },
            preset: Some("veryRareUnknownPreset".into()),
            fill: ShapeFill::Solid(Color { r: 0, g: 128, b: 0 }),
            outline: None,
            rotation_deg: 0.0,
        });
        let svg = scene_to_svg(&s, 1280);
        // Should still render (as a rectangle path)
        assert!(svg.contains("<path"));
        assert!(svg.contains("fill=\"#008000\""));
        // Bbox path has only M and L commands (no curves)
        let path_start = svg.find("d=\"").unwrap() + 3;
        let path_end = svg[path_start..].find('"').unwrap() + path_start;
        let path_data = &svg[path_start..path_end];
        assert!(path_data.contains('M'));
        assert!(path_data.contains('L'));
        assert!(
            !path_data.contains('C'),
            "bbox fallback should not have curves"
        );
    }

    #[test]
    fn gradient_fill_emits_linear_gradient_def() {
        let mut s = Scene::new(12192000, 6858000);
        s.items.push(Item::Shape {
            rect: Rect {
                x: 0,
                y: 0,
                w: 4572000,
                h: 2286000,
            },
            preset: Some("rect".into()),
            fill: ShapeFill::Gradient(GradientFill {
                stops: vec![
                    GradientStop {
                        position: 0.0,
                        color: Color { r: 255, g: 0, b: 0 },
                    },
                    GradientStop {
                        position: 1.0,
                        color: Color { r: 0, g: 0, b: 255 },
                    },
                ],
                angle_deg: 90.0,
                is_radial: false,
            }),
            outline: None,
            rotation_deg: 0.0,
        });
        let svg = scene_to_svg(&s, 1280);
        assert!(svg.contains("<defs>"), "should have a defs section");
        assert!(
            svg.contains("<linearGradient"),
            "should emit linearGradient"
        );
        assert!(svg.contains("stop-color=\"#FF0000\""));
        assert!(svg.contains("stop-color=\"#0000FF\""));
        assert!(
            svg.contains("url(#grad0)"),
            "fill should reference the gradient"
        );
    }

    #[test]
    fn gradient_fill_radial_emits_radial_gradient_def() {
        let mut s = Scene::new(12192000, 6858000);
        s.items.push(Item::Shape {
            rect: Rect {
                x: 0,
                y: 0,
                w: 4572000,
                h: 2286000,
            },
            preset: Some("ellipse".into()),
            fill: ShapeFill::Gradient(GradientFill {
                stops: vec![
                    GradientStop {
                        position: 0.0,
                        color: Color {
                            r: 255,
                            g: 255,
                            b: 0,
                        },
                    },
                    GradientStop {
                        position: 1.0,
                        color: Color { r: 0, g: 128, b: 0 },
                    },
                ],
                angle_deg: 0.0,
                is_radial: true,
            }),
            outline: None,
            rotation_deg: 0.0,
        });
        let svg = scene_to_svg(&s, 1280);
        assert!(
            svg.contains("<radialGradient"),
            "should emit radialGradient"
        );
    }

    #[test]
    fn picture_fill_emits_pattern_def() {
        let mut s = Scene::new(12192000, 6858000);
        // Minimal 1x1 PNG
        let png_data = vec![
            0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, // PNG header
            0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52, // IHDR chunk
        ];
        s.items.push(Item::Shape {
            rect: Rect {
                x: 0,
                y: 0,
                w: 4572000,
                h: 2286000,
            },
            preset: Some("rect".into()),
            fill: ShapeFill::Picture(PictureFill { data: png_data }),
            outline: None,
            rotation_deg: 0.0,
        });
        let svg = scene_to_svg(&s, 1280);
        assert!(svg.contains("<pattern"), "should emit a pattern element");
        assert!(
            svg.contains("url(#pat0)"),
            "fill should reference the pattern"
        );
        assert!(svg.contains("data:image/png;base64,"));
    }

    #[test]
    fn outline_dash_emits_stroke_dasharray() {
        let mut s = Scene::new(12192000, 6858000);
        s.items.push(Item::Shape {
            rect: Rect {
                x: 0,
                y: 0,
                w: 4572000,
                h: 2286000,
            },
            preset: Some("rect".into()),
            fill: ShapeFill::None,
            outline: Some(Outline {
                color: Color { r: 0, g: 0, b: 0 },
                width_pt: 2.0,
                dash: DashStyle::DashDot,
            }),
            rotation_deg: 0.0,
        });
        let svg = scene_to_svg(&s, 1280);
        assert!(
            svg.contains("stroke-dasharray=\"4 3 1 3\""),
            "DashDot should produce '4 3 1 3'"
        );
        assert!(svg.contains("stroke=\"#000000\""));
    }

    #[test]
    fn outline_solid_no_dasharray() {
        let mut s = Scene::new(12192000, 6858000);
        s.items.push(Item::Shape {
            rect: Rect {
                x: 0,
                y: 0,
                w: 4572000,
                h: 2286000,
            },
            preset: Some("rect".into()),
            fill: ShapeFill::Solid(Color {
                r: 200,
                g: 200,
                b: 200,
            }),
            outline: Some(Outline {
                color: Color { r: 0, g: 0, b: 0 },
                width_pt: 1.0,
                dash: DashStyle::Solid,
            }),
            rotation_deg: 0.0,
        });
        let svg = scene_to_svg(&s, 1280);
        assert!(
            !svg.contains("stroke-dasharray"),
            "solid outline should not have dasharray"
        );
    }

    #[test]
    fn dash_style_mapping_all_variants() {
        // Verify all dash styles produce valid (or None for solid) dasharray values
        let styles = [
            (DashStyle::Solid, None),
            (DashStyle::Dot, Some("1 1")),
            (DashStyle::Dash, Some("4 3")),
            (DashStyle::LgDash, Some("8 3")),
            (DashStyle::DashDot, Some("4 3 1 3")),
            (DashStyle::LgDashDot, Some("8 3 1 3")),
            (DashStyle::LgDashDotDot, Some("8 3 1 3 1 3")),
            (DashStyle::SysDot, Some("1 1")),
            (DashStyle::SysDash, Some("3 1")),
            (DashStyle::SysDashDot, Some("3 1 1 1")),
            (DashStyle::SysDashDotDot, Some("3 1 1 1 1 1")),
        ];
        for (style, expected) in &styles {
            assert_eq!(
                style.to_svg_dasharray(),
                *expected,
                "mismatch for {style:?}"
            );
        }
    }

    #[test]
    fn image_crop_emits_clip_path() {
        let mut s = Scene::new(12192000, 6858000);
        // Minimal JPEG header
        let jpeg_data = vec![0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10];
        s.items.push(Item::Image {
            rect: Rect {
                x: 914400,
                y: 914400,
                w: 4572000,
                h: 2286000,
            },
            data: jpeg_data,
            crop: Some(ImageCrop {
                left: 0.1,
                top: 0.2,
                right: 0.1,
                bottom: 0.2,
            }),
            rotation_deg: 0.0,
        });
        let svg = scene_to_svg(&s, 1280);
        assert!(
            svg.contains("<clipPath"),
            "cropped image should have a clipPath"
        );
        assert!(svg.contains("clip-path=\"url(#clip0)\""));
        assert!(svg.contains("data:image/jpeg;base64,"));
    }

    #[test]
    fn image_rotation_emits_transform() {
        let mut s = Scene::new(12192000, 6858000);
        let jpeg_data = vec![0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10];
        s.items.push(Item::Image {
            rect: Rect {
                x: 914400,
                y: 914400,
                w: 4572000,
                h: 2286000,
            },
            data: jpeg_data,
            crop: None,
            rotation_deg: 45.0,
        });
        let svg = scene_to_svg(&s, 1280);
        assert!(
            svg.contains("transform=\"rotate(45.0"),
            "rotated image should have rotate transform"
        );
    }

    #[test]
    fn image_no_crop_no_rotation_simple_output() {
        let mut s = Scene::new(12192000, 6858000);
        let jpeg_data = vec![0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10];
        s.items.push(Item::Image {
            rect: Rect {
                x: 914400,
                y: 914400,
                w: 4572000,
                h: 2286000,
            },
            data: jpeg_data,
            crop: None,
            rotation_deg: 0.0,
        });
        let svg = scene_to_svg(&s, 1280);
        assert!(
            !svg.contains("<clipPath"),
            "uncropped image should not have clipPath"
        );
        assert!(
            !svg.contains("transform="),
            "unrotated image should not have transform"
        );
        assert!(svg.contains("<image"));
    }

    #[test]
    fn solid_background_renders() {
        let mut s = Scene::new(12192000, 6858000);
        s.rich_background = Some(Background::Solid(Color {
            r: 30,
            g: 60,
            b: 90,
        }));
        let svg = scene_to_svg(&s, 1280);
        assert!(
            svg.contains("fill=\"#1E3C5A\""),
            "solid background should use the specified color"
        );
    }

    #[test]
    fn picture_background_renders() {
        let mut s = Scene::new(12192000, 6858000);
        let png_data = vec![0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A];
        s.rich_background = Some(Background::Picture(png_data));
        let svg = scene_to_svg(&s, 1280);
        assert!(
            svg.contains("<image"),
            "picture background should emit an image element"
        );
        assert!(svg.contains("preserveAspectRatio=\"xMidYMid slice\""));
        assert!(svg.contains("data:image/png;base64,"));
    }

    #[test]
    fn shape_rotation_emits_transform() {
        let mut s = Scene::new(12192000, 6858000);
        s.items.push(Item::Shape {
            rect: Rect {
                x: 914400,
                y: 914400,
                w: 1828800,
                h: 914400,
            },
            preset: Some("diamond".into()),
            fill: ShapeFill::Solid(Color {
                r: 128,
                g: 0,
                b: 128,
            }),
            outline: None,
            rotation_deg: 30.0,
        });
        let svg = scene_to_svg(&s, 1280);
        assert!(
            svg.contains("transform=\"rotate(30.0"),
            "rotated shape should have rotate transform"
        );
    }

    #[test]
    fn shape_no_preset_renders_as_rect_path() {
        let mut s = Scene::new(12192000, 6858000);
        s.items.push(Item::Shape {
            rect: Rect {
                x: 0,
                y: 0,
                w: 4572000,
                h: 2286000,
            },
            preset: None,
            fill: ShapeFill::Solid(Color {
                r: 100,
                g: 100,
                b: 100,
            }),
            outline: None,
            rotation_deg: 0.0,
        });
        let svg = scene_to_svg(&s, 1280);
        assert!(
            svg.contains("<path"),
            "shape without preset should still emit a path"
        );
        assert!(svg.contains("fill=\"#646464\""));
    }

    /// Identifiers are what make a rendered slide editable: without them a click on a
    /// shape cannot be traced back to anything.
    #[test]
    fn identified_output_names_every_item() {
        let scene = sample();
        let svg = scene_to_svg_with(&scene, 1280, SvgOptions { identify: true });
        for index in 0..scene.items.len() {
            assert!(
                svg.contains(&format!("data-item=\"{index}\"")),
                "item {index} is not addressable in the rendered slide"
            );
        }
        assert_eq!(
            svg.matches("<g data-item=").count(),
            scene.items.len(),
            "every item, and nothing else, should be wrapped"
        );
        assert_eq!(
            svg.matches("</g>").count(),
            scene.items.len(),
            "every wrapper must be closed, or the SVG is malformed"
        );
    }

    /// The default must be untouched, so nothing that renders slides today changes.
    #[test]
    fn default_output_is_unchanged_by_the_option() {
        let scene = sample();
        let plain = scene_to_svg(&scene, 1280);
        let also_plain = scene_to_svg_with(&scene, 1280, SvgOptions::default());
        assert_eq!(plain, also_plain);
        assert!(!plain.contains("data-item"), "identifiers must be opt-in");
    }

    #[test]
    fn identifying_adds_nothing_but_the_wrapper() {
        let scene = sample();
        let plain = scene_to_svg(&scene, 1280);
        let identified = scene_to_svg_with(&scene, 1280, SvgOptions { identify: true });
        let stripped = identified
            .replace("</g>", "")
            .split("<g data-item=")
            .enumerate()
            .map(|(i, part)| {
                if i == 0 {
                    part.to_string()
                } else {
                    part[part.find('>').unwrap() + 1..].to_string()
                }
            })
            .collect::<String>();
        assert_eq!(
            stripped, plain,
            "the drawing itself must be identical whether or not it is identified"
        );
    }

    /// Guards the two tests above from proving nothing: if `sample()` were empty, an
    /// assertion that `data-item` is absent would hold trivially.
    #[test]
    fn the_sample_used_by_those_tests_actually_draws_something() {
        let scene = sample();
        assert!(
            scene.items.len() >= 2,
            "the sample must have several elements or the identifier tests are vacuous, got {}",
            scene.items.len()
        );
        let identified = scene_to_svg_with(&scene, 1280, SvgOptions { identify: true });
        assert!(
            identified.contains("data-item"),
            "the opt-in must actually do something"
        );
    }
}
