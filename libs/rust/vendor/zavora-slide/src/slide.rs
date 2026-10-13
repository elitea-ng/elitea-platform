//! High-level slide model and editing handle.
//!
//! A created slide owns a list of DrawingML shapes; serialization wraps them in
//! the canonical blank-slide shell (matching the part PowerPoint accepts). Title
//! and body placeholders are placed with explicit geometry derived from the
//! slide size, so they render regardless of the generated deck's single layout —
//! per-layout placeholder geometry resolution is a later (layout) phase.

use zavora_slide_oxml::{Paragraph, Run, RunProps, Shape, TextBody};

use crate::error::{Result, SlideError};
use crate::units::Emu;

/// One bullet line for [`Slide::add_bullets`].
#[derive(Debug, Clone)]
pub struct Bullet {
    pub text: String,
    pub level: u8,
    pub bold: bool,
}

impl Bullet {
    pub fn new(text: impl Into<String>) -> Self {
        Self {
            text: text.into(),
            level: 0,
            bold: false,
        }
    }
}

/// A shape/slide fill.
#[derive(Debug, Clone, PartialEq)]
pub enum Fill {
    /// Solid color (hex, with or without `#`).
    Solid(String),
    /// A stretched picture fill (background only). `ext` is "png"/"jpg"/"jpeg".
    Picture { data: Vec<u8>, ext: String },
}

/// Fixed slide-rel id for a background picture (one per slide; clear of
/// layout rId1, notes rId2, and image embeds rId10+).
pub(crate) const BG_EMBED_RID: &str = "rId9";

impl Fill {
    fn bg_xml(&self) -> String {
        match self {
            Fill::Solid(hex) => {
                let h = hex.trim_start_matches('#').to_uppercase();
                format!(
                    "<p:bg><p:bgPr><a:solidFill><a:srgbClr val=\"{h}\"/></a:solidFill>\
                     <a:effectLst/></p:bgPr></p:bg>"
                )
            }
            Fill::Picture { .. } => format!(
                "<p:bg><p:bgPr><a:blipFill><a:blip r:embed=\"{BG_EMBED_RID}\"/>\
                 <a:stretch><a:fillRect/></a:stretch></a:blipFill>\
                 <a:effectLst/></p:bgPr></p:bg>"
            ),
        }
    }
}

/// Source of an image to embed.
#[derive(Debug, Clone)]
pub enum ImageSrc {
    /// Read from a filesystem path (extension determines the format).
    Path(std::path::PathBuf),
    /// Raw bytes with an explicit extension ("png", "jpg", "jpeg").
    Bytes { data: Vec<u8>, ext: String },
}

/// Resolve an [`ImageSrc`] to (bytes, lowercased ext), validating the type.
fn read_image(src: ImageSrc) -> Result<(Vec<u8>, String)> {
    let (data, ext) = match src {
        ImageSrc::Path(p) => {
            let ext = p
                .extension()
                .and_then(|e| e.to_str())
                .map(|e| e.to_ascii_lowercase())
                .ok_or_else(|| SlideError::InvalidInput("image path has no extension".into()))?;
            (std::fs::read(&p)?, ext)
        }
        ImageSrc::Bytes { data, ext } => (data, ext.to_ascii_lowercase()),
    };
    if !matches!(ext.as_str(), "png" | "jpg" | "jpeg") {
        return Err(SlideError::InvalidInput(format!(
            "unsupported image type '{ext}'"
        )));
    }
    Ok((data, ext))
}

/// Supported image formats for surgical insert (Req 10.4).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ImageFormat {
    Png,
    Jpeg,
    Gif,
}

impl ImageFormat {
    /// Detect image format from magic bytes.
    pub fn detect(data: &[u8]) -> Option<Self> {
        if data.len() >= 8 && data[..8] == [0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A] {
            Some(ImageFormat::Png)
        } else if data.len() >= 2 && data[..2] == [0xFF, 0xD8] {
            Some(ImageFormat::Jpeg)
        } else if data.len() >= 6 && (&data[..6] == b"GIF87a" || &data[..6] == b"GIF89a") {
            Some(ImageFormat::Gif)
        } else {
            None
        }
    }

    /// File extension for this format.
    pub fn extension(&self) -> &'static str {
        match self {
            ImageFormat::Png => "png",
            ImageFormat::Jpeg => "jpeg",
            ImageFormat::Gif => "gif",
        }
    }

    /// MIME content type for this format.
    pub fn content_type(&self) -> &'static str {
        match self {
            ImageFormat::Png => "image/png",
            ImageFormat::Jpeg => "image/jpeg",
            ImageFormat::Gif => "image/gif",
        }
    }
}

/// Compute a hex-encoded SHA-256 content hash of the given bytes.
fn content_hash(data: &[u8]) -> String {
    use sha2::{Digest, Sha256};
    let hash = Sha256::digest(data);
    hex_encode(&hash)
}

/// Encode bytes as lowercase hex string.
fn hex_encode(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// An embedded image: its media bytes/extension plus placement.
#[derive(Debug, Clone)]
pub struct ImageMedia {
    pub ext: String,
    pub data: Vec<u8>,
    pub embed_rid: String,
    pub id: u32,
    pub x: i64,
    pub y: i64,
    pub cx: i64,
    pub cy: i64,
}

impl ImageMedia {
    fn pic_xml(&self) -> String {
        format!(
            "<p:pic><p:nvPicPr><p:cNvPr id=\"{id}\" name=\"Picture {id}\"/>\
             <p:cNvPicPr><a:picLocks noChangeAspect=\"1\"/></p:cNvPicPr><p:nvPr/></p:nvPicPr>\
             <p:blipFill><a:blip r:embed=\"{rid}\"/><a:stretch><a:fillRect/></a:stretch></p:blipFill>\
             <p:spPr><a:xfrm><a:off x=\"{x}\" y=\"{y}\"/><a:ext cx=\"{cx}\" cy=\"{cy}\"/></a:xfrm>\
             <a:prstGeom prst=\"rect\"><a:avLst/></a:prstGeom></p:spPr></p:pic>",
            id = self.id,
            rid = self.embed_rid,
            x = self.x,
            y = self.y,
            cx = self.cx,
            cy = self.cy
        )
    }
}

/// Identifies a table within a slide (its index in `SlideData::tables`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct TableId(pub usize);

/// A table rendered as a `p:graphicFrame` / `a:tbl`.
#[derive(Debug, Clone)]
pub struct Table {
    pub id: u32,
    pub x: i64,
    pub y: i64,
    pub cx: i64,
    pub cy: i64,
    pub rows: usize,
    pub cols: usize,
    /// Cell text in row-major order (`rows * cols` entries).
    pub cells: Vec<String>,
}

impl Table {
    fn esc(s: &str) -> String {
        s.replace('&', "&amp;")
            .replace('<', "&lt;")
            .replace('>', "&gt;")
    }

    pub(crate) fn graphic_frame_xml(&self) -> String {
        let col_w = if self.cols > 0 {
            self.cx / self.cols as i64
        } else {
            self.cx
        };
        let row_h = if self.rows > 0 {
            self.cy / self.rows as i64
        } else {
            self.cy
        };
        let grid: String = (0..self.cols)
            .map(|_| format!("<a:gridCol w=\"{col_w}\"/>"))
            .collect();
        let mut rows_xml = String::new();
        for r in 0..self.rows {
            rows_xml.push_str(&format!("<a:tr h=\"{row_h}\">"));
            for c in 0..self.cols {
                let text = self
                    .cells
                    .get(r * self.cols + c)
                    .map(String::as_str)
                    .unwrap_or("");
                rows_xml.push_str(&format!(
                    "<a:tc><a:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>{}</a:t></a:r></a:p>\
                     </a:txBody><a:tcPr/></a:tc>",
                    Self::esc(text)
                ));
            }
            rows_xml.push_str("</a:tr>");
        }
        format!(
            "<p:graphicFrame><p:nvGraphicFramePr><p:cNvPr id=\"{id}\" name=\"Table {id}\"/>\
             <p:cNvGraphicFramePr/><p:nvPr/></p:nvGraphicFramePr>\
             <p:xfrm><a:off x=\"{x}\" y=\"{y}\"/><a:ext cx=\"{cx}\" cy=\"{cy}\"/></p:xfrm>\
             <a:graphic><a:graphicData uri=\"http://schemas.openxmlformats.org/drawingml/2006/table\">\
             <a:tbl><a:tblPr firstRow=\"1\" bandRow=\"1\">\
             <a:tableStyleId>{{5C22544A-7EE6-4342-B048-85BDC9FD1C3A}}</a:tableStyleId></a:tblPr>\
             <a:tblGrid>{grid}</a:tblGrid>{rows_xml}</a:tbl></a:graphicData></a:graphic></p:graphicFrame>",
            id = self.id,
            x = self.x,
            y = self.y,
            cx = self.cx,
            cy = self.cy
        )
    }
}

/// A chart placeholder for rendering: stores the bounding box and optional title
/// so the Scene can represent charts as positioned shapes without full chart rendering.
#[derive(Debug, Clone)]
pub struct ChartPlaceholder {
    /// Position x in EMU.
    pub x: i64,
    /// Position y in EMU.
    pub y: i64,
    /// Width in EMU.
    pub cx: i64,
    /// Height in EMU.
    pub cy: i64,
    /// Optional chart title (extracted from the chart spec or chart XML).
    pub title: Option<String>,
}

/// Stored slide content: the shapes injected into the slide's `spTree`.
#[derive(Debug, Clone, Default)]
pub struct SlideData {
    pub shapes: Vec<Shape>,
    /// Embedded images (rendered as `p:pic` and written as media parts on save).
    pub images: Vec<ImageMedia>,
    /// Tables (rendered as `p:graphicFrame` / `a:tbl`).
    pub tables: Vec<Table>,
    /// Speaker notes text, if any (emitted as a notesSlide part on save).
    pub notes: Option<String>,
    /// Optional slide background fill.
    pub background: Option<Fill>,
    /// Set when this slide is edited after being opened from a source package,
    /// so the save overlay re-authors just this slide (see `Presentation::save`).
    pub(crate) dirty: bool,
    /// Part path this slide was opened from (e.g. "/ppt/slides/slide3.xml"),
    /// used by the overlay save to overwrite the right part in-place.
    pub(crate) source_part: Option<String>,
    /// Original `<p:sldId>` identity (numeric id, presentation rel id) for a
    /// slide opened from an existing deck. Lets move/delete reconstruct the
    /// slide-id list faithfully without renumbering.
    pub(crate) sld_id: Option<(u32, String)>,
    /// For a slide duplicated from a source-backed slide: the original's part
    /// path, whose relationships are cloned so the copy references the same
    /// layout/media. The copy's content comes from its (cloned) `dom`.
    pub(crate) clone_rels_from: Option<String>,
    /// For a brand-new blank slide added to a source-backed deck: the
    /// `sldLayout@type` to bind to one of the deck's existing layouts. The slide
    /// part is authored minimally (empty spTree) during save.
    pub(crate) new_blank_layout_type: Option<String>,
    /// Editable DOM of the slide when opened from an existing deck. Edits mutate
    /// this tree in place; save serializes it byte-faithfully (untouched content
    /// preserved). `None` for slides authored from scratch.
    pub(crate) dom: Option<zavora_slide_oxml::SlideDom>,
    /// The pictures this slide references, by the relationship id its shapes name.
    ///
    /// Kept at open, because drawing the slide needs the bytes and the package is not held past
    /// then. Without it a slide whose content is a picture drew as nothing at all.
    pub(crate) media: std::collections::HashMap<String, Vec<u8>>,
    /// Where the layout puts each placeholder: "title", "body/1" and so on, to the box the layout
    /// gives it.
    ///
    /// A placeholder on a slide usually states no geometry of its own — PowerPoint reads it from the
    /// layout, and so does every other tool that renders the file correctly. Guessing instead puts
    /// a title where a title usually goes, which is right often enough to look plausible and wrong
    /// often enough to matter. Taken when the package is open, because it is not kept.
    pub(crate) layout_boxes: std::collections::HashMap<String, (i64, i64, i64, i64)>,
    /// The theme's colours by the names a slide calls them: "accent1", "lt1", "tx2".
    ///
    /// Nearly half the colours in a real deck are stated this way rather than as a hex value, and a
    /// colour that is not resolved is drawn black — which is how a white title on a dark band came
    /// out black on black, and why nobody could read it.
    pub(crate) theme_colours: std::collections::HashMap<String, String>,
    /// Editable DOM of the notes-slide part when opened from an existing deck.
    /// Notes edits mutate this tree in place; save serializes only the notes part
    /// (surgical, no full rebuild). `None` when the slide has no notes part.
    pub(crate) notes_dom: Option<zavora_slide_oxml::NotesDom>,
    /// The part path of the notes slide (e.g. "/ppt/notesSlides/notesSlide1.xml").
    /// Used by the overlay save to write the edited notes part back.
    pub(crate) notes_part: Option<String>,
    /// Media registry for content-hash deduplication (Req 10.3). Maps content
    /// hash → (media part path, relationship id). When the same image bytes are
    /// inserted again, the existing media part is reused.
    pub(crate) media_registry: Vec<MediaEntry>,
    /// Pending chart entries to be written as parts during save (Part B).
    pub(crate) charts: Vec<crate::chart::ChartEntry>,
    /// Graphic frame XML snippets for charts (serialized into the slide's spTree).
    pub(crate) chart_frames: Vec<String>,
    /// Chart placeholders for rendering: bounding box + optional title.
    pub(crate) chart_placeholders: Vec<ChartPlaceholder>,
    /// Next shape id (group shape is id 1, so authored shapes start at 2).
    next_id: u32,
}

/// A registered media part, keyed by content hash for deduplication.
#[derive(Debug, Clone)]
pub struct MediaEntry {
    /// Hex-encoded SHA-256 hash of the media bytes.
    pub hash: String,
    /// Part path in the package (e.g. "/ppt/media/image1.png").
    pub part_path: String,
    /// Relationship ID linking the slide to this media part.
    pub r_id: String,
}

impl SlideData {
    pub fn new() -> Self {
        Self {
            shapes: Vec::new(),
            images: Vec::new(),
            media: std::collections::HashMap::new(),
            layout_boxes: std::collections::HashMap::new(),
            theme_colours: std::collections::HashMap::new(),
            tables: Vec::new(),
            notes: None,
            background: None,
            dirty: false,
            source_part: None,
            sld_id: None,
            clone_rels_from: None,
            new_blank_layout_type: None,
            dom: None,
            notes_dom: None,
            notes_part: None,
            media_registry: Vec::new(),
            charts: Vec::new(),
            chart_frames: Vec::new(),
            chart_placeholders: Vec::new(),
            next_id: 2,
        }
    }

    pub(crate) fn alloc_id(&mut self) -> u32 {
        let id = self.next_id;
        self.next_id += 1;
        id
    }

    /// Test helper: whether this slide has a parsed notes DOM.
    #[doc(hidden)]
    pub fn has_notes_dom(&self) -> bool {
        self.notes_dom.is_some()
    }

    /// Test helper: whether this slide has a notes part path.
    #[doc(hidden)]
    pub fn has_notes_part(&self) -> bool {
        self.notes_part.is_some()
    }

    /// Test helper: read the notes text from the notes DOM (if present).
    #[doc(hidden)]
    pub fn notes_dom_text(&self) -> Option<String> {
        self.notes_dom.as_ref().map(|d| d.notes_text())
    }

    /// Find the first placeholder shape of the given type.
    fn find_ph(&mut self, ph_type: &str) -> Option<&mut Shape> {
        self.shapes
            .iter_mut()
            .find(|s| s.placeholder.as_ref().is_some_and(|p| p.ph_type == ph_type))
    }

    /// Serialize to a complete slide part.
    pub fn to_xml(&self) -> Vec<u8> {
        let shapes: String = self.shapes.iter().map(Shape::to_xml).collect();
        let pics: String = self.images.iter().map(ImageMedia::pic_xml).collect();
        let tbls: String = self.tables.iter().map(Table::graphic_frame_xml).collect();
        let charts: String = self.chart_frames.join("");
        let bg = self
            .background
            .as_ref()
            .map(Fill::bg_xml)
            .unwrap_or_default();
        format!(
            "<?xml version=\"1.0\" encoding=\"UTF-8\" standalone=\"yes\"?>\n\
             <p:sld xmlns:a=\"http://schemas.openxmlformats.org/drawingml/2006/main\" \
             xmlns:r=\"http://schemas.openxmlformats.org/officeDocument/2006/relationships\" \
             xmlns:p=\"http://schemas.openxmlformats.org/presentationml/2006/main\">\
             <p:cSld>{bg}<p:spTree>\
             <p:nvGrpSpPr><p:cNvPr id=\"1\" name=\"\"/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>\
             <p:grpSpPr/>{shapes}{pics}{tbls}{charts}</p:spTree></p:cSld>\
             <p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sld>"
        )
        .into_bytes()
    }

    /// Where the layout puts each placeholder, for drawing.
    fn layout_boxes_for_scene(&self) -> std::collections::HashMap<String, (i64, i64, i64, i64)> {
        self.layout_boxes.clone()
    }

    /// Build a render-ready [`Scene`](zavora_slide_layout::Scene) from this slide's content.
    /// The scene to draw.
    ///
    /// From the slide's own shape tree where there is one — that is what the file says, shape by
    /// shape, and it is how a picture or a second column reaches the screen. Only a slide built
    /// here rather than opened falls back to the model, which is the one case where there is no XML
    /// to read.
    pub fn scene(&self, width_emu: i64, height_emu: i64) -> zavora_slide_layout::Scene {
        if let Some(dom) = self.dom.as_ref() {
            let media = &self.media;
            let boxes = &self.layout_boxes_for_scene();
            let scene = crate::from_dom::scene_from_dom(
                dom,
                width_emu,
                height_emu,
                &|embed| media.get(embed).cloned(),
                &|kind, index| {
                    // By type and index first, then by type alone, then by index alone: a slide can
                    // name a placeholder any of those three ways and the layout may key it another.
                    let keys = match index {
                        Some(index) => vec![
                            format!("{kind}/{index}"),
                            kind.to_string(),
                            format!("/{index}"),
                        ],
                        None => vec![kind.to_string()],
                    };
                    keys.iter().find_map(|key| {
                        boxes
                            .get(key)
                            .map(|(x, y, w, h)| zavora_slide_layout::Rect {
                                x: *x,
                                y: *y,
                                w: *w,
                                h: *h,
                            })
                    })
                },
                &|named| {
                    self.theme_colours
                        .get(named)
                        .and_then(|hex| zavora_slide_layout::Color::from_hex(hex))
                },
            );
            if !scene.items.is_empty() {
                return scene;
            }
        }
        self.to_scene(width_emu, height_emu)
    }

    pub fn to_scene(&self, width_emu: i64, height_emu: i64) -> zavora_slide_layout::Scene {
        use zavora_slide_layout::{Color, Item, Rect, Scene, TextFrameProps, TextLine};

        let hex = |h: &str| Color::from_hex(h).unwrap_or(Color::BLACK);
        let mut scene = Scene::new(width_emu, height_emu);
        match &self.background {
            Some(Fill::Solid(c)) => scene.background = Color::from_hex(c),
            Some(Fill::Picture { data, .. }) => scene.push(
                Item::Image {
                    rect: Rect {
                        x: 0,
                        y: 0,
                        w: width_emu,
                        h: height_emu,
                    },
                    data: data.clone(),
                    crop: None,
                    rotation_deg: 0.0,
                },
                Some(zavora_slide_layout::ItemSource::Background),
            ),
            None => {}
        }

        for (shape_index, sp) in self.shapes.iter().enumerate() {
            let source = Some(zavora_slide_layout::ItemSource::Shape(shape_index));
            let Some((x, y, w, h)) = sp.xfrm else {
                continue;
            };
            let rect = Rect { x, y, w, h };
            // Auto-shape body: filled/outlined box.
            if sp.fill.is_some() || sp.line.is_some() {
                scene.push(
                    Item::Rect {
                        rect,
                        fill: sp.fill.as_deref().and_then(Color::from_hex),
                        outline: sp.line.as_ref().and_then(|(c, w)| {
                            Color::from_hex(c)
                                .map(|col| (col, *w as f64 / zavora_slide_layout::EMU_PER_POINT))
                        }),
                    },
                    source,
                );
            }
            // Text content.
            let is_title = sp
                .placeholder
                .as_ref()
                .is_some_and(|p| p.ph_type == "title");
            let lines: Vec<TextLine> = sp
                .body
                .paragraphs
                .iter()
                .filter(|p| !p.text().is_empty())
                .map(|p| {
                    let rp = p.runs.first().map(|r| &r.props);
                    TextLine {
                        text: p.text(),
                        size_pt: rp.and_then(|r| r.size_pt).unwrap_or(if is_title {
                            32.0
                        } else {
                            18.0
                        }),
                        color: rp
                            .and_then(|r| r.color.as_deref())
                            .map(hex)
                            .unwrap_or(Color::BLACK),
                        bold: rp.and_then(|r| r.bold).unwrap_or(false),
                        italic: rp.and_then(|r| r.italic).unwrap_or(false),
                        level: p.level.unwrap_or(0),
                        has_bullet: p.level.unwrap_or(0) > 0,
                        is_paragraph_start: true,
                        ..TextLine::default()
                    }
                })
                .collect();
            if !lines.is_empty() {
                // The same shape, drawn a second time as its text. Both items point at
                // the shape, which is what a caller can actually edit.
                scene.push(
                    Item::Text {
                        rect,
                        lines,
                        props: TextFrameProps::default(),
                    },
                    source,
                );
            }
        }

        for (image_index, img) in self.images.iter().enumerate() {
            scene.push(
                Item::Image {
                    rect: Rect {
                        x: img.x,
                        y: img.y,
                        w: img.cx,
                        h: img.cy,
                    },
                    data: img.data.clone(),
                    crop: None,
                    rotation_deg: 0.0,
                },
                Some(zavora_slide_layout::ItemSource::Image(image_index)),
            );
        }

        // Tables: outline box + per-cell text (even grid).
        for (table_index, t) in self.tables.iter().enumerate() {
            let table_source = Some(zavora_slide_layout::ItemSource::Table(table_index));
            let cw = if t.cols > 0 {
                t.cx / t.cols as i64
            } else {
                t.cx
            };
            let rh = if t.rows > 0 {
                t.cy / t.rows as i64
            } else {
                t.cy
            };
            for r in 0..t.rows {
                for c in 0..t.cols {
                    let cell = Rect {
                        x: t.x + c as i64 * cw,
                        y: t.y + r as i64 * rh,
                        w: cw,
                        h: rh,
                    };
                    scene.push(
                        Item::Rect {
                            rect: cell,
                            fill: None,
                            outline: Some((
                                Color {
                                    r: 200,
                                    g: 200,
                                    b: 200,
                                },
                                0.75,
                            )),
                        },
                        table_source,
                    );
                    if let Some(text) = t.cells.get(r * t.cols + c)
                        && !text.is_empty()
                    {
                        scene.push(
                            Item::Text {
                                rect: cell,
                                lines: vec![TextLine {
                                    text: text.clone(),
                                    size_pt: 14.0,
                                    color: Color::BLACK,
                                    bold: r == 0,
                                    italic: false,
                                    level: 0,
                                    is_paragraph_start: true,
                                    ..TextLine::default()
                                }],
                                props: TextFrameProps::default(),
                            },
                            table_source,
                        );
                    }
                }
            }
        }

        // Charts: light gray placeholder shape + optional title text overlay.
        // For newly authored charts, use the chart_placeholders vector.
        for (chart_index, chart) in self.chart_placeholders.iter().enumerate() {
            let chart_source = Some(zavora_slide_layout::ItemSource::Chart(chart_index));
            let rect = Rect {
                x: chart.x,
                y: chart.y,
                w: chart.cx,
                h: chart.cy,
            };
            use zavora_slide_layout::ShapeFill;
            scene.push(
                Item::Shape {
                    rect,
                    preset: Some("rect".into()),
                    fill: ShapeFill::Solid(Color {
                        r: 220,
                        g: 220,
                        b: 220,
                    }),
                    outline: Some(zavora_slide_layout::Outline {
                        color: Color {
                            r: 180,
                            g: 180,
                            b: 180,
                        },
                        width_pt: 1.0,
                        dash: zavora_slide_layout::DashStyle::Solid,
                    }),
                    rotation_deg: 0.0,
                },
                chart_source,
            );
            if let Some(title) = &chart.title
                && !title.is_empty()
            {
                scene.push(
                    Item::Text {
                        rect,
                        lines: vec![TextLine {
                            text: title.clone(),
                            size_pt: 14.0,
                            color: Color {
                                r: 80,
                                g: 80,
                                b: 80,
                            },
                            bold: true,
                            italic: false,
                            level: 0,
                            is_paragraph_start: true,
                            ..TextLine::default()
                        }],
                        props: TextFrameProps::default(),
                    },
                    chart_source,
                );
            }
        }

        // For opened decks: scan the DOM for chart graphicFrames and render them
        // as placeholders (if not already covered by chart_placeholders above).
        if self.chart_placeholders.is_empty()
            && let Some(dom) = &self.dom
        {
            let chart_uri = b"http://schemas.openxmlformats.org/drawingml/2006/chart";
            for info in dom.shape_inventory() {
                if info.shape_type == "graphicFrame"
                    && let Some((x, y, cx, cy)) = info.geometry
                {
                    // Check if this graphicFrame is a chart by inspecting the
                    // graphicData URI in the DOM element.
                    let is_chart = dom.all_shapes().iter().any(|el| {
                        if el.local_name() != b"graphicFrame" {
                            return false;
                        }
                        // Match by geometry
                        let geom_match = el.find_descendant(b"xfrm").is_some_and(|xfrm| {
                            let off_x = xfrm.children.iter().find_map(|n| match n {
                                zavora_slide_oxml::Node::Element(e) if e.local_name() == b"off" => {
                                    e.attr(b"x").and_then(|v| {
                                        std::str::from_utf8(v).ok()?.parse::<i64>().ok()
                                    })
                                }
                                _ => None,
                            });
                            off_x == Some(x)
                        });
                        if !geom_match {
                            return false;
                        }
                        // Check for chart URI in graphicData
                        el.find_descendant(b"graphicData")
                            .is_some_and(|gd| gd.attr(b"uri") == Some(chart_uri))
                    });

                    if is_chart {
                        let rect = Rect { x, y, w: cx, h: cy };
                        use zavora_slide_layout::ShapeFill;
                        scene.push(
                            Item::Shape {
                                rect,
                                preset: Some("rect".into()),
                                fill: ShapeFill::Solid(Color {
                                    r: 220,
                                    g: 220,
                                    b: 220,
                                }),
                                outline: Some(zavora_slide_layout::Outline {
                                    color: Color {
                                        r: 180,
                                        g: 180,
                                        b: 180,
                                    },
                                    width_pt: 1.0,
                                    dash: zavora_slide_layout::DashStyle::Solid,
                                }),
                                rotation_deg: 0.0,
                            },
                            None,
                        );
                    }
                }
            }
        }

        scene
    }
}

/// A mutable handle to one slide, aware of the deck's slide size for placeholder
/// geometry.
pub struct Slide<'a> {
    pub(crate) data: &'a mut SlideData,
    pub(crate) slide_cx: i64,
    pub(crate) slide_cy: i64,
}

impl Slide<'_> {
    const MARGIN: i64 = 457200; // 0.5"
    const TITLE_H: i64 = 1143000; // 1.25"

    fn title_box(&self) -> (i64, i64, i64, i64) {
        (
            Self::MARGIN,
            Self::MARGIN,
            self.slide_cx - 2 * Self::MARGIN,
            Self::TITLE_H,
        )
    }

    fn body_box(&self) -> (i64, i64, i64, i64) {
        let y = 2 * Self::MARGIN + Self::TITLE_H;
        (
            Self::MARGIN,
            y,
            self.slide_cx - 2 * Self::MARGIN,
            self.slide_cy - y - Self::MARGIN,
        )
    }

    /// Set the slide title (creates or replaces the title placeholder).
    pub fn set_title(&mut self, text: &str) -> Result<()> {
        // Opened slides: mutate the lossless DOM in place (surgical, preserves
        // all other content).
        // Worked out before the DOM is borrowed, because it reads the slide's own size.
        let (x, y, cx, cy) = self.title_box();
        if let Some(dom) = self.data.dom.as_mut() {
            if dom.set_title(text).is_ok() {
                self.sync_build_title(text);
                return Ok(());
            }

            // No title placeholder in the file. This used to fall through to the build model, which
            // an opened deck never writes — so titling a slide reported success and changed nothing.
            // A blank slide added and then titled kept the name the interface gave it, and the User
            // was told the title had been set. Put a real text box in the file instead, where the
            // title belongs.
            let placed = dom.add_text_box(text, x, y, cx, cy);
            self.sync_build_title(text);
            return placed.map(|_| ()).map_err(Into::into);
        }
        self.sync_build_title(text);
        Ok(())
    }

    /// Update the build model's title (keeps render/markdown read paths current).
    fn sync_build_title(&mut self, text: &str) {
        let body = TextBody {
            paragraphs: vec![Paragraph {
                runs: vec![Run::new(text)],
                ..Default::default()
            }],
        };
        if let Some(sp) = self.data.find_ph("title") {
            sp.body = body;
        } else {
            let id = self.data.alloc_id();
            let mut sp = Shape::placeholder(id, "Title 1", "title", None, body);
            sp.xfrm = Some(self.title_box());
            self.data.shapes.push(sp);
        }
    }

    /// Populate the body placeholder with one paragraph per bullet.
    pub fn add_bullets(&mut self, items: &[Bullet]) -> Result<()> {
        // Worked out before the DOM is borrowed, because it reads the slide's own size.
        let (x, y, cx, cy) = self.body_box();
        if let Some(dom) = self.data.dom.as_mut() {
            let pairs: Vec<(String, u8)> =
                items.iter().map(|b| (b.text.clone(), b.level)).collect();
            if dom.set_body_bullets(&pairs).is_ok() {
                self.sync_build_bullets(items);
                return Ok(());
            }
            // Elitea patch (libs/rust/vendor/README.md): no body placeholder in the
            // file (a slide added to an opened deck starts blank). Upstream fell
            // through to the build model, which an opened deck never writes, so
            // the bullets were reported as added and were not. Put a text box in
            // the file, one paragraph per item, as `set_title` does for a title.
            let lines: Vec<String> = items
                .iter()
                .map(|b| format!("{}\u{2022} {}", "    ".repeat(usize::from(b.level)), b.text))
                .collect();
            if let Some(first) = lines.first() {
                if dom.add_text_box(first, x, y, cx, cy).is_ok() {
                    let index = dom.shapes().count().saturating_sub(1);
                    for line in &lines[1..] {
                        let _ = dom.add_paragraph(index, line);
                    }
                }
            }
        }
        self.sync_build_bullets(items);
        Ok(())
    }

    /// Crate-internal: populate only the build model during open (no DOM touch).
    pub(crate) fn sync_build_bullets_public(&mut self, items: &[Bullet]) {
        self.sync_build_bullets(items);
    }

    /// Update the build model's body bullets (render/markdown read paths).
    fn sync_build_bullets(&mut self, items: &[Bullet]) {
        let paragraphs = items
            .iter()
            .map(|b| Paragraph {
                runs: vec![Run {
                    text: b.text.clone(),
                    props: RunProps {
                        bold: b.bold.then_some(true),
                        ..Default::default()
                    },
                }],
                level: Some(b.level),
                ..Default::default()
            })
            .collect();
        let body = TextBody { paragraphs };
        if let Some(sp) = self.data.find_ph("body") {
            sp.body = body;
        } else {
            let id = self.data.alloc_id();
            let mut sp = Shape::placeholder(id, "Content 1", "body", Some(1), body);
            sp.xfrm = Some(self.body_box());
            self.data.shapes.push(sp);
        }
    }

    /// Apply character formatting to every run of a placeholder ("title" or
    /// "body") on an opened slide, mutating the DOM in place. Returns an error if
    /// the slide has no DOM or no such placeholder.
    pub fn format_placeholder(
        &mut self,
        ph_type: &str,
        fmt: zavora_slide_oxml::RunFormat,
    ) -> Result<()> {
        let dom =
            self.data.dom.as_mut().ok_or_else(|| {
                SlideError::Unsupported("formatting requires an opened slide".into())
            })?;
        dom.format_placeholder(ph_type, &fmt)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    // ─── Paragraph-level editing (Part A) ────────────────────────────────

    /// Add (append) a paragraph with `text` to the text frame of the shape at
    /// `shape_idx`. Requires an opened slide with a DOM.
    pub fn add_paragraph(&mut self, shape_idx: usize, text: &str) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("paragraph editing requires an opened slide".into())
        })?;
        dom.add_paragraph(shape_idx, text)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Insert a paragraph with `text` at `para_idx` in the text frame of the
    /// shape at `shape_idx`. Requires an opened slide with a DOM.
    pub fn insert_paragraph(
        &mut self,
        shape_idx: usize,
        para_idx: usize,
        text: &str,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("paragraph editing requires an opened slide".into())
        })?;
        dom.insert_paragraph(shape_idx, para_idx, text)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Delete the paragraph at `para_idx` from the text frame of the shape at
    /// `shape_idx`. Requires an opened slide with a DOM.
    pub fn delete_paragraph(&mut self, shape_idx: usize, para_idx: usize) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("paragraph editing requires an opened slide".into())
        })?;
        dom.delete_paragraph(shape_idx, para_idx)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Reorder the paragraph at `from_idx` to `to_idx` within the text frame of
    /// the shape at `shape_idx`. Requires an opened slide with a DOM.
    pub fn move_paragraph(
        &mut self,
        shape_idx: usize,
        from_idx: usize,
        to_idx: usize,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("paragraph editing requires an opened slide".into())
        })?;
        dom.reorder_paragraph(shape_idx, from_idx, to_idx)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Set paragraph alignment on the paragraph at `para_idx` in the shape at
    /// `shape_idx`. Valid values: "l", "ctr", "r", "just", "dist".
    pub fn set_paragraph_alignment(
        &mut self,
        shape_idx: usize,
        para_idx: usize,
        algn: &str,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("paragraph formatting requires an opened slide".into())
        })?;
        dom.set_paragraph_alignment(shape_idx, para_idx, algn)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Set the indent level on the paragraph at `para_idx` in the shape at `shape_idx`.
    pub fn set_paragraph_level(
        &mut self,
        shape_idx: usize,
        para_idx: usize,
        level: u8,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("paragraph formatting requires an opened slide".into())
        })?;
        dom.set_paragraph_indent_level(shape_idx, para_idx, level)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Set space-before on the paragraph at `para_idx` in the shape at `shape_idx`.
    pub fn set_paragraph_space_before(
        &mut self,
        shape_idx: usize,
        para_idx: usize,
        value: zavora_slide_oxml::SpacingValue,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("paragraph formatting requires an opened slide".into())
        })?;
        dom.set_paragraph_space_before(shape_idx, para_idx, value)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Set space-after on the paragraph at `para_idx` in the shape at `shape_idx`.
    pub fn set_paragraph_space_after(
        &mut self,
        shape_idx: usize,
        para_idx: usize,
        value: zavora_slide_oxml::SpacingValue,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("paragraph formatting requires an opened slide".into())
        })?;
        dom.set_paragraph_space_after(shape_idx, para_idx, value)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Set line spacing on the paragraph at `para_idx` in the shape at `shape_idx`.
    pub fn set_paragraph_line_spacing(
        &mut self,
        shape_idx: usize,
        para_idx: usize,
        value: zavora_slide_oxml::SpacingValue,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("paragraph formatting requires an opened slide".into())
        })?;
        dom.set_paragraph_line_spacing(shape_idx, para_idx, value)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Set bullet style on the paragraph at `para_idx` in the shape at `shape_idx`.
    pub fn set_paragraph_bullet(
        &mut self,
        shape_idx: usize,
        para_idx: usize,
        bullet: &zavora_slide_oxml::BulletKind,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("paragraph formatting requires an opened slide".into())
        })?;
        dom.set_paragraph_bullet(shape_idx, para_idx, bullet)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    // ─── Run-level methods ─────────────────────────────────────────────────

    /// Append a run with `text` to the paragraph at `para_idx` in the shape at
    /// `shape_idx`. Requires an opened slide with a DOM.
    pub fn add_run(&mut self, shape_idx: usize, para_idx: usize, text: &str) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("run editing requires an opened slide".into())
        })?;
        dom.add_run(shape_idx, para_idx, text)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Edit the text content of the run at `run_idx` within the paragraph at
    /// `para_idx` in the shape at `shape_idx`. Requires an opened slide with a DOM.
    pub fn edit_run_text(
        &mut self,
        shape_idx: usize,
        para_idx: usize,
        run_idx: usize,
        new_text: &str,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("run editing requires an opened slide".into())
        })?;
        dom.edit_run_text(shape_idx, para_idx, run_idx, new_text)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Delete the run (or line break) at `run_idx` within the paragraph at
    /// `para_idx` in the shape at `shape_idx`. Requires an opened slide with a DOM.
    pub fn delete_run(&mut self, shape_idx: usize, para_idx: usize, run_idx: usize) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("run editing requires an opened slide".into())
        })?;
        dom.delete_run(shape_idx, para_idx, run_idx)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Append a line break (`<a:br/>`) to the paragraph at `para_idx` in the
    /// shape at `shape_idx`. If `position` is given, inserts at that run index.
    /// Requires an opened slide with a DOM.
    pub fn add_line_break(
        &mut self,
        shape_idx: usize,
        para_idx: usize,
        position: Option<usize>,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("run editing requires an opened slide".into())
        })?;
        if let Some(pos) = position {
            dom.insert_line_break(shape_idx, para_idx, pos)
                .map_err(|e| SlideError::InvalidInput(e.to_string()))
        } else {
            dom.add_line_break(shape_idx, para_idx)
                .map_err(|e| SlideError::InvalidInput(e.to_string()))
        }
    }

    /// Apply character formatting to the run at `run_idx` within the paragraph
    /// at `para_idx` in the shape at `shape_idx`. Requires an opened slide with a DOM.
    pub fn format_run(
        &mut self,
        shape_idx: usize,
        para_idx: usize,
        run_idx: usize,
        fmt: &zavora_slide_oxml::RunFormat,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("run formatting requires an opened slide".into())
        })?;
        dom.format_run(shape_idx, para_idx, run_idx, fmt)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Set the auto-fit behavior on a shape's text frame.
    /// Requires an opened slide with a DOM.
    pub fn set_autofit(
        &mut self,
        shape_idx: usize,
        autofit: &zavora_slide_oxml::AutoFit,
    ) -> Result<()> {
        let dom =
            self.data.dom.as_mut().ok_or_else(|| {
                SlideError::Unsupported("autofit requires an opened slide".into())
            })?;
        dom.set_autofit(shape_idx, autofit)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Return the number of runs in the paragraph at `para_idx` in the shape at
    /// `shape_idx`. Requires an opened slide with a DOM.
    pub fn run_count(&self, shape_idx: usize, para_idx: usize) -> Result<usize> {
        let dom =
            self.data.dom.as_ref().ok_or_else(|| {
                SlideError::Unsupported("run access requires an opened slide".into())
            })?;
        dom.runs(shape_idx, para_idx)
            .map(|v| v.len())
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    // --- Shape geometry / lifecycle (Part D) ------------------------------------

    /// Set the geometry (position/size/rotation) of the shape at `shape_idx`.
    /// All values are in EMU. Rotation is in 60,000ths of a degree. Requires
    /// an opened slide with a DOM.
    pub fn set_shape_geometry(
        &mut self,
        shape_idx: usize,
        x: i64,
        y: i64,
        cx: i64,
        cy: i64,
        rot: Option<i64>,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("shape geometry requires an opened slide".into())
        })?;
        dom.set_shape_geometry(shape_idx, x, y, cx, cy, rot)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Delete the shape at `shape_idx` from the slide. Requires an opened
    /// slide with a DOM.
    pub fn delete_shape(&mut self, shape_idx: usize) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("shape deletion requires an opened slide".into())
        })?;
        dom.delete_shape(shape_idx)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Reorder the shape at `from_idx` to `to_idx` within the slide's shape
    /// tree, changing its z-order. Requires an opened slide with a DOM.
    pub fn reorder_shape(&mut self, from_idx: usize, to_idx: usize) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("shape reorder requires an opened slide".into())
        })?;
        dom.reorder_shape(from_idx, to_idx)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Set the fill of an existing shape by index. Requires an opened slide.
    pub fn set_shape_fill(
        &mut self,
        shape_idx: usize,
        fill: &zavora_slide_oxml::FillSpec,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("set_shape_fill requires an opened slide".into())
        })?;
        dom.set_shape_fill(shape_idx, fill)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Set the outline (line) of an existing shape by index. Requires an opened slide.
    pub fn set_shape_line(
        &mut self,
        shape_idx: usize,
        line: &zavora_slide_oxml::LineSpec,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("set_shape_line requires an opened slide".into())
        })?;
        dom.set_shape_line(shape_idx, line)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    // ─── Table operations ─────────────────────────────────────────────────

    pub fn table_add_row(&mut self, shape_idx: usize, height_emu: i64) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("table_add_row requires an opened slide".into())
        })?;
        dom.add_table_row(shape_idx, height_emu)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    pub fn table_remove_row(&mut self, shape_idx: usize, row_idx: usize) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("table_remove_row requires an opened slide".into())
        })?;
        dom.remove_table_row(shape_idx, row_idx)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    pub fn table_add_column(&mut self, shape_idx: usize, width_emu: i64) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("table_add_column requires an opened slide".into())
        })?;
        dom.add_table_column(shape_idx, width_emu)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    pub fn table_remove_column(&mut self, shape_idx: usize, col_idx: usize) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("table_remove_column requires an opened slide".into())
        })?;
        dom.remove_table_column(shape_idx, col_idx)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    pub fn table_merge_cells(
        &mut self,
        shape_idx: usize,
        r1: usize,
        c1: usize,
        r2: usize,
        c2: usize,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("merge_cells requires an opened slide".into())
        })?;
        dom.merge_table_cells(shape_idx, r1, c1, r2, c2)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    pub fn table_split_cell(&mut self, shape_idx: usize, row: usize, col: usize) -> Result<()> {
        let dom =
            self.data.dom.as_mut().ok_or_else(|| {
                SlideError::Unsupported("split_cell requires an opened slide".into())
            })?;
        dom.split_table_cell(shape_idx, row, col)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    pub fn table_set_column_width(
        &mut self,
        shape_idx: usize,
        col_idx: usize,
        width_emu: i64,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("set_column_width requires an opened slide".into())
        })?;
        dom.set_column_width(shape_idx, col_idx, width_emu)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    pub fn table_set_row_height(
        &mut self,
        shape_idx: usize,
        row_idx: usize,
        height_emu: i64,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("set_row_height requires an opened slide".into())
        })?;
        dom.set_row_height(shape_idx, row_idx, height_emu)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    /// Elitea patch (libs/rust/vendor/README.md): set a cell's text as one
    /// paragraph per entry of `lines`, an empty entry staying an empty
    /// paragraph. Upstream's `table_set_cell_text` writes a single paragraph,
    /// so a multi-paragraph cell lost its structure on any edit.
    pub fn table_set_cell_paragraphs(
        &mut self,
        shape_idx: usize,
        row: usize,
        col: usize,
        lines: &[String],
    ) -> Result<()> {
        let first = lines.first().map_or("", String::as_str);
        self.table_set_cell_text(shape_idx, row, col, first)?;
        if lines.len() <= 1 {
            return Ok(());
        }
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("set_cell_text requires an opened slide".into())
        })?;
        let invalid = |m: &str| SlideError::InvalidInput(m.to_owned());
        let table = dom
            .table_element_mut(shape_idx)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))?;
        let tr = table
            .children_named_mut(b"tr")
            .nth(row)
            .ok_or_else(|| invalid("no such table row"))?;
        let tc = tr
            .children_named_mut(b"tc")
            .nth(col)
            .ok_or_else(|| invalid("no such table column"))?;
        let body = tc
            .children_named_mut(b"txBody")
            .next()
            .ok_or_else(|| invalid("the cell has no text body"))?;
        let template = body
            .children_named(b"p")
            .next()
            .cloned()
            .ok_or_else(|| invalid("the cell has no paragraph"))?;
        for line in &lines[1..] {
            let mut paragraph = template.clone();
            for child in &mut paragraph.children {
                if let zavora_slide_oxml::Node::Element(run) = child {
                    if run.local_name() == b"r" {
                        if let Some(t) = run.children_named_mut(b"t").next() {
                            t.set_text_content(line);
                        }
                    }
                }
            }
            body.children.push(zavora_slide_oxml::Node::Element(paragraph));
        }
        Ok(())
    }

    pub fn table_set_cell_text(
        &mut self,
        shape_idx: usize,
        row: usize,
        col: usize,
        text: &str,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("set_cell_text requires an opened slide".into())
        })?;
        dom.set_cell_text(shape_idx, row, col, text)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    pub fn table_set_cell_fill(
        &mut self,
        shape_idx: usize,
        row: usize,
        col: usize,
        hex: &str,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("set_cell_fill requires an opened slide".into())
        })?;
        let fill = zavora_slide_oxml::FillSpec::Solid {
            color: zavora_slide_oxml::ColorSpec::Rgb(hex.to_string()),
        };
        dom.set_cell_fill(shape_idx, row, col, &fill)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    pub fn set_image_crop(
        &mut self,
        shape_idx: usize,
        left: u32,
        top: u32,
        right: u32,
        bottom: u32,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("set_image_crop requires an opened slide".into())
        })?;
        dom.set_image_crop(shape_idx, left, top, right, bottom)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    pub fn set_image_rotation(&mut self, shape_idx: usize, rotation_deg: f64) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("set_image_rotation requires an opened slide".into())
        })?;
        let rot = (rotation_deg * 60_000.0) as i64;
        dom.set_shape_rotation(shape_idx, rot)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    // ─── Hyperlink / click action / footer ────────────────────────────────

    pub fn set_run_hyperlink(
        &mut self,
        shape_idx: usize,
        para_idx: usize,
        run_idx: usize,
        url: &str,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("set_run_hyperlink requires an opened slide".into())
        })?;
        let r_id = format!("rId{}", shape_idx * 100 + para_idx * 10 + run_idx + 900);
        dom.set_run_hyperlink(shape_idx, para_idx, run_idx, url, &r_id)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    pub fn set_shape_click_action(
        &mut self,
        shape_idx: usize,
        action: &zavora_slide_oxml::ClickAction,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("set_shape_click_action requires an opened slide".into())
        })?;
        dom.set_shape_click_action(shape_idx, action)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    pub fn set_footer_text(&mut self, text: &str) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("set_footer_text requires an opened slide".into())
        })?;
        dom.set_footer_text(text)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    pub fn set_footer_visible(&mut self, visible: bool) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("set_footer_visible requires an opened slide".into())
        })?;
        dom.set_footer_visible(visible)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))
    }

    // ─── Shape vocabulary (connectors, freeform) ──────────────────────────

    pub fn add_autoshape_preset(
        &mut self,
        preset: &str,
        x: i64,
        y: i64,
        cx: i64,
        cy: i64,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("add_autoshape requires an opened slide".into())
        })?;
        dom.add_autoshape(preset, x, y, cx, cy)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))?;
        Ok(())
    }

    pub fn add_connector(
        &mut self,
        conn_type: zavora_slide_oxml::ConnectorType,
        x: i64,
        y: i64,
        cx: i64,
        cy: i64,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("add_connector requires an opened slide".into())
        })?;
        dom.add_connector(conn_type, None, None, x, y, cx, cy)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))?;
        Ok(())
    }

    pub fn add_freeform(
        &mut self,
        path: &zavora_slide_oxml::FreeformPath,
        x: i64,
        y: i64,
        cx: i64,
        cy: i64,
    ) -> Result<()> {
        let dom = self.data.dom.as_mut().ok_or_else(|| {
            SlideError::Unsupported("add_freeform requires an opened slide".into())
        })?;
        dom.add_freeform(path, x, y, cx, cy)
            .map_err(|e| SlideError::InvalidInput(e.to_string()))?;
        Ok(())
    }
    pub fn add_text_box(&mut self, text: &str, x: Emu, y: Emu, w: Emu, h: Emu) -> &mut Shape {
        if let Some(dom) = self.data.dom.as_mut() {
            let _ = dom.add_text_box(text, x.0, y.0, w.0, h.0);
        }
        let id = self.data.alloc_id();
        let body = TextBody {
            paragraphs: vec![Paragraph {
                runs: vec![Run::new(text)],
                ..Default::default()
            }],
        };
        let sp = Shape::text_box(id, x.0, y.0, w.0, h.0, body);
        self.data.shapes.push(sp);
        self.data.shapes.last_mut().unwrap()
    }

    /// Add an auto-shape with the given preset geometry. Returns a mutable
    /// reference so callers can set fill/outline. On an opened slide the shape
    /// is also appended to the slide DOM so it is preserved on save.
    pub fn add_shape(
        &mut self,
        preset: crate::units::ShapePreset,
        x: Emu,
        y: Emu,
        w: Emu,
        h: Emu,
    ) -> &mut Shape {
        if let Some(dom) = self.data.dom.as_mut() {
            let _ = dom.add_autoshape(preset.prst(), x.0, y.0, w.0, h.0);
        }
        let id = self.data.alloc_id();
        let sp = Shape::auto_shape(id, preset.prst(), x.0, y.0, w.0, h.0);
        self.data.shapes.push(sp);
        self.data.shapes.last_mut().unwrap()
    }

    /// Add a `rows`×`cols` table at the given position/size. Returns its id for
    /// addressing cells via [`Slide::set_table_cell`].
    pub fn add_table(
        &mut self,
        rows: usize,
        cols: usize,
        x: Emu,
        y: Emu,
        w: Emu,
        h: Emu,
    ) -> TableId {
        let id = self.data.alloc_id();
        self.data.tables.push(Table {
            id,
            x: x.0,
            y: y.0,
            cx: w.0,
            cy: h.0,
            rows,
            cols,
            cells: vec![String::new(); rows * cols],
        });
        TableId(self.data.tables.len() - 1)
    }

    /// Set the text of a table cell.
    pub fn set_table_cell(
        &mut self,
        table: TableId,
        row: usize,
        col: usize,
        text: &str,
    ) -> Result<()> {
        let t = self
            .data
            .tables
            .get_mut(table.0)
            .ok_or_else(|| SlideError::NotFound(format!("table {}", table.0)))?;
        if row >= t.rows || col >= t.cols {
            return Err(SlideError::InvalidInput(format!(
                "cell ({row},{col}) out of bounds for {}x{} table",
                t.rows, t.cols
            )));
        }
        t.cells[row * t.cols + col] = text.to_string();
        Ok(())
    }

    /// Add a chart to the slide at the given EMU position/size.
    ///
    /// The chart is materialized as a chart part (`ppt/charts/chartN.xml`) and an
    /// embedded workbook (`ppt/embeddings/Microsoft_Excel_WorksheetN.xlsx`) during
    /// save, with correct relationships and content types.
    pub fn add_chart(
        &mut self,
        spec: &crate::chart::ChartSpec,
        x: Emu,
        y: Emu,
        w: Emu,
        h: Emu,
        chart_index: usize,
    ) -> Result<()> {
        crate::chart::add_chart_to_slide(self.data, spec, x.0, y.0, w.0, h.0, chart_index)
    }

    /// Set (or replace) the slide's speaker notes.
    ///
    /// On an opened slide with an existing notes part, this edits the notes DOM
    /// in place (surgical — only the notes part changes). On a new slide or one
    /// without an existing notes part, falls back to the build-model path.
    pub fn set_notes(&mut self, text: &str) {
        // Surgical DOM path: edit the notes part in place.
        if let Some(notes_dom) = self.data.notes_dom.as_mut()
            && notes_dom.set_notes_text(text).is_ok()
        {
            self.data.notes = Some(text.to_string());
            return;
        }
        self.data.notes = Some(text.to_string());
    }

    /// Embed an image at the given EMU position/size. PNG and JPEG supported.
    pub fn add_image(&mut self, src: ImageSrc, x: Emu, y: Emu, w: Emu, h: Emu) -> Result<()> {
        let (data, ext) = read_image(src)?;
        let id = self.data.alloc_id();
        // Embed rel ids start at rId10 to stay clear of layout(rId1)/notes(rId2).
        let embed_rid = format!("rId{}", 10 + self.data.images.len());
        self.data.images.push(ImageMedia {
            ext,
            data,
            embed_rid,
            id,
            x: x.0,
            y: y.0,
            cx: w.0,
            cy: h.0,
        });
        Ok(())
    }

    /// Insert an image into an opened slide surgically (Req 10.3, 10.4).
    ///
    /// Accepts raw image bytes + position/size. Detects the format from magic
    /// bytes (PNG, JPEG, GIF). Computes a content hash (SHA-256) and deduplicates:
    /// if the same bytes already exist as a media part, the existing part is
    /// reused and only a new relationship is added. Otherwise a new media part is
    /// created at `ppt/media/imageN.{ext}`.
    ///
    /// On an opened slide (with a DOM), this calls `SlideDom::insert_picture` to
    /// surgically add the `<p:pic>` element. On a build-model slide, it falls
    /// back to the `images` list.
    ///
    /// Returns the shape id of the inserted picture.
    pub fn insert_image_bytes(
        &mut self,
        data: &[u8],
        x: Emu,
        y: Emu,
        w: Emu,
        h: Emu,
    ) -> Result<u32> {
        let format = ImageFormat::detect(data).ok_or_else(|| {
            SlideError::InvalidInput(
                "unsupported image format (expected PNG, JPEG, or GIF magic bytes)".into(),
            )
        })?;

        let hash = content_hash(data);

        // Check for deduplication: if the same content hash already exists, reuse
        // the existing media part and just add a new relationship.
        let (r_id, _part_path, is_new) =
            if let Some(entry) = self.data.media_registry.iter().find(|e| e.hash == hash) {
                (entry.r_id.clone(), entry.part_path.clone(), false)
            } else {
                // Allocate a new media part path.
                let media_idx = self.data.media_registry.len() + 1;
                let ext = format.extension();
                let part_path = format!("/ppt/media/image{media_idx}.{ext}");
                // Allocate a new relationship ID (start at rId10 to avoid collisions).
                let r_id = format!(
                    "rId{}",
                    10 + self.data.media_registry.len() + self.data.images.len()
                );
                self.data.media_registry.push(MediaEntry {
                    hash: hash.clone(),
                    part_path: part_path.clone(),
                    r_id: r_id.clone(),
                });
                (r_id, part_path, true)
            };

        // Insert the picture into the DOM if available (surgical path).
        let name = format!("Picture {}", self.data.media_registry.len());
        let shape_id = if let Some(dom) = self.data.dom.as_mut() {
            dom.insert_picture(x.0, y.0, w.0, h.0, &r_id, &name)
                .map_err(|e| SlideError::InvalidInput(e.to_string()))?
        } else {
            // Fallback: add to the build-model images list.
            let id = self.data.alloc_id();
            self.data.images.push(ImageMedia {
                ext: format.extension().to_string(),
                data: data.to_vec(),
                embed_rid: r_id.clone(),
                id,
                x: x.0,
                y: y.0,
                cx: w.0,
                cy: h.0,
            });
            id
        };

        // Store the media bytes and metadata for the overlay save to pick up.
        if is_new {
            self.data.images.push(ImageMedia {
                ext: format.extension().to_string(),
                data: data.to_vec(),
                embed_rid: r_id,
                id: shape_id,
                x: x.0,
                y: y.0,
                cx: w.0,
                cy: h.0,
            });
        }

        Ok(shape_id)
    }

    /// Set the slide background fill.
    pub fn set_background(&mut self, fill: Fill) {
        self.data.background = Some(fill);
    }

    /// Set the slide background to a stretched picture (PNG/JPEG).
    pub fn set_background_image(&mut self, src: ImageSrc) -> Result<()> {
        let (data, ext) = read_image(src)?;
        self.data.background = Some(Fill::Picture { data, ext });
        Ok(())
    }

    /// Speaker notes text, if any.
    pub fn notes(&self) -> Option<&str> {
        self.data.notes.as_deref()
    }

    /// A summary of the slide's shapes (kind + extracted text).
    pub fn shapes(&self) -> Vec<ShapeInfo> {
        self.data
            .shapes
            .iter()
            .map(|sp| ShapeInfo {
                kind: match &sp.placeholder {
                    Some(ph) => ph.ph_type.clone(),
                    None if sp.text_box => "textbox".to_string(),
                    None => "shape".to_string(),
                },
                text: sp
                    .body
                    .paragraphs
                    .iter()
                    .map(|p| p.text())
                    .collect::<Vec<_>>()
                    .join("\n"),
            })
            .collect()
    }

    /// Extracted plain text of all shapes (one paragraph per line).
    pub fn text(&self) -> String {
        let mut lines = Vec::new();
        for sp in &self.data.shapes {
            for p in &sp.body.paragraphs {
                lines.push(p.text());
            }
        }
        lines.join("\n")
    }

    /// Build a render-ready [`Scene`](zavora_slide_layout::Scene) of this slide.
    pub fn scene(&self) -> zavora_slide_layout::Scene {
        self.data.scene(self.slide_cx, self.slide_cy)
    }
}

/// A lightweight description of one shape, for read/inspection.
#[derive(Debug, Clone, PartialEq)]
pub struct ShapeInfo {
    /// Placeholder type (e.g. "title", "body"), "textbox", or "shape".
    pub kind: String,
    pub text: String,
}

/// A read-only borrow of a slide (does not invalidate a preserved source).
pub struct SlideRef<'a> {
    pub(crate) data: &'a SlideData,
    pub(crate) slide_cx: i64,
    pub(crate) slide_cy: i64,
}

impl SlideRef<'_> {
    /// Speaker notes text, if any.
    pub fn notes(&self) -> Option<&str> {
        self.data.notes.as_deref()
    }

    /// Shape inventory (kind + text).
    pub fn shapes(&self) -> Vec<ShapeInfo> {
        self.data
            .shapes
            .iter()
            .map(|sp| ShapeInfo {
                kind: match &sp.placeholder {
                    Some(ph) => ph.ph_type.clone(),
                    None if sp.text_box => "textbox".to_string(),
                    None => "shape".to_string(),
                },
                text: sp
                    .body
                    .paragraphs
                    .iter()
                    .map(|p| p.text())
                    .collect::<Vec<_>>()
                    .join("\n"),
            })
            .collect()
    }

    /// Extracted plain text of all shapes (one paragraph per line).
    pub fn text(&self) -> String {
        self.data
            .shapes
            .iter()
            .flat_map(|sp| sp.body.paragraphs.iter().map(|p| p.text()))
            .collect::<Vec<_>>()
            .join("\n")
    }

    /// Build a render-ready scene of this slide.
    pub fn scene(&self) -> zavora_slide_layout::Scene {
        self.data.scene(self.slide_cx, self.slide_cy)
    }
}

/// Build a canonical notesSlide part for the given notes text. Mirrors the
/// structure PowerPoint emits: a slide-image placeholder and a body
/// placeholder carrying the notes.
pub(crate) fn notes_slide_xml(notes: &str) -> Vec<u8> {
    let esc = notes
        .replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;");
    format!(
        "<?xml version=\"1.0\" encoding=\"UTF-8\" standalone=\"yes\"?>\n\
         <p:notes xmlns:a=\"http://schemas.openxmlformats.org/drawingml/2006/main\" \
         xmlns:r=\"http://schemas.openxmlformats.org/officeDocument/2006/relationships\" \
         xmlns:p=\"http://schemas.openxmlformats.org/presentationml/2006/main\">\
         <p:cSld><p:spTree>\
         <p:nvGrpSpPr><p:cNvPr id=\"1\" name=\"\"/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>\
         <p:grpSpPr/>\
         <p:sp><p:nvSpPr><p:cNvPr id=\"2\" name=\"Slide Image Placeholder 1\"/>\
         <p:cNvSpPr><a:spLocks noGrp=\"1\"/></p:cNvSpPr>\
         <p:nvPr><p:ph type=\"sldImg\" idx=\"1\"/></p:nvPr></p:nvSpPr><p:spPr/></p:sp>\
         <p:sp><p:nvSpPr><p:cNvPr id=\"3\" name=\"Notes Placeholder 2\"/>\
         <p:cNvSpPr><a:spLocks noGrp=\"1\"/></p:cNvSpPr>\
         <p:nvPr><p:ph type=\"body\" idx=\"1\"/></p:nvPr></p:nvSpPr><p:spPr/>\
         <p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>{esc}</a:t></a:r></a:p></p:txBody></p:sp>\
         </p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:notes>"
    )
    .into_bytes()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn slide(data: &mut SlideData) -> Slide<'_> {
        Slide {
            data,
            slide_cx: 12192000,
            slide_cy: 6858000,
        }
    }

    #[test]
    fn title_and_bullets_populate_placeholders() {
        let mut d = SlideData::new();
        {
            let mut s = slide(&mut d);
            s.set_title("Quarterly Review").unwrap();
            s.add_bullets(&[
                Bullet::new("Revenue up 23%"),
                Bullet {
                    text: "EMEA".into(),
                    level: 1,
                    bold: false,
                },
            ])
            .unwrap();
        }
        assert_eq!(d.shapes.len(), 2);
        let xml = String::from_utf8(d.to_xml()).unwrap();
        assert!(xml.contains("<p:ph type=\"title\"/>"));
        assert!(xml.contains("<a:t>Quarterly Review</a:t>"));
        assert!(xml.contains("<p:ph type=\"body\" idx=\"1\"/>"));
        assert!(xml.contains("<a:t>Revenue up 23%</a:t>"));
        assert!(xml.contains("lvl=\"1\""));
    }

    #[test]
    fn set_title_replaces_not_duplicates() {
        let mut d = SlideData::new();
        slide(&mut d).set_title("A").unwrap();
        slide(&mut d).set_title("B").unwrap();
        assert_eq!(d.shapes.len(), 1);
        assert_eq!(slide(&mut d).text(), "B");
    }

    #[test]
    fn text_box_with_fluent_format() {
        let mut d = SlideData::new();
        {
            let mut s = slide(&mut d);
            s.add_text_box(
                "Hi",
                Emu::inches(1.0),
                Emu::inches(1.0),
                Emu::inches(2.0),
                Emu::inches(0.5),
            )
            .bold(true)
            .color("#FF0000")
            .size(18.0);
        }
        let xml = String::from_utf8(d.to_xml()).unwrap();
        assert!(xml.contains("txBox=\"1\""));
        assert!(xml.contains("b=\"1\""));
        assert!(xml.contains("<a:srgbClr val=\"FF0000\"/>"));
        assert!(xml.contains("sz=\"1800\""));
    }

    #[test]
    fn notes_and_shape_inventory() {
        let mut d = SlideData::new();
        {
            let mut s = slide(&mut d);
            s.set_title("T").unwrap();
            s.add_bullets(&[Bullet::new("one")]).unwrap();
            s.set_notes("remember this");
            assert_eq!(s.notes(), Some("remember this"));
            let inv = s.shapes();
            assert_eq!(inv.len(), 2);
            assert_eq!(inv[0].kind, "title");
            assert_eq!(inv[1].kind, "body");
            assert_eq!(inv[1].text, "one");
        }
        assert_eq!(d.notes.as_deref(), Some("remember this"));
    }

    #[test]
    fn notes_slide_part_escapes() {
        let xml = String::from_utf8(notes_slide_xml("a < b & c")).unwrap();
        assert!(xml.contains("<p:ph type=\"body\""));
        assert!(xml.contains("a &lt; b &amp; c"));
    }

    #[test]
    fn background_emitted_in_csld() {
        let mut d = SlideData::new();
        slide(&mut d).set_background(Fill::Solid("#102030".into()));
        let xml = String::from_utf8(d.to_xml()).unwrap();
        // bg precedes spTree inside cSld.
        let bg = xml.find("<p:bg>").unwrap();
        let tree = xml.find("<p:spTree>").unwrap();
        assert!(bg < tree);
        assert!(xml.contains("<a:srgbClr val=\"102030\"/>"));
    }

    #[test]
    fn picture_background_emits_blipfill() {
        let mut d = SlideData::new();
        slide(&mut d)
            .set_background_image(ImageSrc::Bytes {
                data: vec![0xFF, 0xD8, 1, 2],
                ext: "PNG".into(),
            })
            .unwrap();
        let xml = String::from_utf8(d.to_xml()).unwrap();
        assert!(xml.contains(&format!(
            "<a:blip r:embed=\"{}\"/>",
            crate::slide::BG_EMBED_RID
        )));
        assert!(xml.contains("<a:stretch>"));
    }

    #[test]
    fn add_image_from_bytes_emits_pic() {
        let mut d = SlideData::new();
        slide(&mut d)
            .add_image(
                ImageSrc::Bytes {
                    data: vec![1, 2, 3],
                    ext: "PNG".into(),
                },
                Emu::inches(1.0),
                Emu::inches(1.0),
                Emu::inches(2.0),
                Emu::inches(2.0),
            )
            .unwrap();
        assert_eq!(d.images.len(), 1);
        assert_eq!(d.images[0].ext, "png");
        assert_eq!(d.images[0].embed_rid, "rId10");
        let xml = String::from_utf8(d.to_xml()).unwrap();
        assert!(xml.contains("<p:pic>"));
        assert!(xml.contains("r:embed=\"rId10\""));
    }

    #[test]
    fn add_image_rejects_unsupported_type() {
        let mut d = SlideData::new();
        let r = slide(&mut d).add_image(
            ImageSrc::Bytes {
                data: Vec::<u8>::new(),
                ext: "gif".into(),
            },
            Emu::inches(0.0),
            Emu::inches(0.0),
            Emu::inches(1.0),
            Emu::inches(1.0),
        );
        assert!(r.is_err());
    }

    #[test]
    fn add_shape_with_fill_and_outline() {
        use crate::units::ShapePreset;
        let mut d = SlideData::new();
        {
            let mut s = slide(&mut d);
            let sp = s.add_shape(
                ShapePreset::Ellipse,
                Emu::inches(1.0),
                Emu::inches(1.0),
                Emu::inches(2.0),
                Emu::inches(2.0),
            );
            sp.set_fill("#00AA00").set_outline("#000000", 2.0);
        }
        let xml = String::from_utf8(d.to_xml()).unwrap();
        assert!(xml.contains("prst=\"ellipse\""));
        assert!(xml.contains("<a:srgbClr val=\"00AA00\"/>"));
        assert!(xml.contains("<a:ln w=\"25400\">"));
    }

    #[test]
    fn scene_includes_text_and_table() {
        use zavora_slide_layout::Item;
        let mut d = SlideData::new();
        {
            let mut s = slide(&mut d);
            s.set_title("Hi").unwrap();
            s.set_background(Fill::Solid("#FFFFFF".into()));
            let t = s.add_table(
                1,
                2,
                Emu::inches(1.0),
                Emu::inches(3.0),
                Emu::inches(4.0),
                Emu::inches(1.0),
            );
            s.set_table_cell(t, 0, 0, "A").unwrap();
        }
        let scene = d.to_scene(12192000, 6858000);
        assert!(scene.background.is_some());
        let texts = scene
            .items
            .iter()
            .filter(|i| matches!(i, Item::Text { .. }))
            .count();
        let rects = scene
            .items
            .iter()
            .filter(|i| matches!(i, Item::Rect { .. }))
            .count();
        assert!(texts >= 2); // title + 1 non-empty cell
        assert_eq!(rects, 2); // 2 cell outlines
    }

    #[test]
    fn add_table_and_set_cells() {
        let mut d = SlideData::new();
        {
            let mut s = slide(&mut d);
            let t = s.add_table(
                2,
                2,
                Emu::inches(1.0),
                Emu::inches(1.0),
                Emu::inches(4.0),
                Emu::inches(2.0),
            );
            s.set_table_cell(t, 0, 0, "H1").unwrap();
            s.set_table_cell(t, 1, 1, "v & w").unwrap();
            assert!(s.set_table_cell(t, 5, 0, "x").is_err());
        }
        let xml = String::from_utf8(d.to_xml()).unwrap();
        assert!(xml.contains(
            "graphicData uri=\"http://schemas.openxmlformats.org/drawingml/2006/table\""
        ));
        assert!(xml.contains("<a:gridCol"));
        assert!(xml.contains("<a:t>H1</a:t>"));
        assert!(xml.contains("v &amp; w"));
        assert_eq!(xml.matches("<a:tr ").count(), 2);
    }

    // ─── Tests for insert_image_bytes (Req 10.3, 10.4) ─────────────────────

    /// Minimal valid PNG (1x1 pixel, transparent).
    fn tiny_png() -> Vec<u8> {
        vec![
            0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, // PNG signature
            0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52, // IHDR chunk
            0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x02, 0x00, 0x00, 0x00, 0x90,
            0x77, 0x53, 0xDE, 0x00, 0x00, 0x00, 0x0C, 0x49, 0x44, 0x41, 0x54, 0x08, 0xD7, 0x63,
            0xF8, 0xCF, 0xC0, 0x00, 0x00, 0x00, 0x02, 0x00, 0x01, 0xE2, 0x21, 0xBC, 0x33, 0x00,
            0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
        ]
    }

    /// Minimal GIF89a header (enough for magic-byte detection).
    fn tiny_gif() -> Vec<u8> {
        let mut data = b"GIF89a".to_vec();
        // Logical screen descriptor (1x1, no GCT)
        data.extend_from_slice(&[0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00]);
        // Image descriptor
        data.extend_from_slice(&[0x2C, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00]);
        // LZW minimum code size + block
        data.extend_from_slice(&[0x02, 0x02, 0x4C, 0x01, 0x00]);
        // Trailer
        data.push(0x3B);
        data
    }

    #[test]
    fn insert_picture_creates_pic_element_in_dom() {
        // Create a slide with a DOM (simulating an opened slide).
        let slide_xml = b"<?xml version=\"1.0\" encoding=\"UTF-8\" standalone=\"yes\"?>\
            <p:sld xmlns:a=\"http://schemas.openxmlformats.org/drawingml/2006/main\" \
            xmlns:r=\"http://schemas.openxmlformats.org/officeDocument/2006/relationships\" \
            xmlns:p=\"http://schemas.openxmlformats.org/presentationml/2006/main\">\
            <p:cSld><p:spTree>\
            <p:nvGrpSpPr><p:cNvPr id=\"1\" name=\"\"/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>\
            <p:grpSpPr/>\
            <p:sp><p:nvSpPr><p:cNvPr id=\"2\" name=\"Title 1\"/><p:cNvSpPr/>\
            <p:nvPr><p:ph type=\"title\"/></p:nvPr></p:nvSpPr><p:spPr/>\
            <p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>Hello</a:t></a:r></a:p></p:txBody></p:sp>\
            </p:spTree></p:cSld></p:sld>";
        let mut d = SlideData::new();
        d.dom = Some(zavora_slide_oxml::SlideDom::parse(slide_xml).unwrap());

        let mut s = slide(&mut d);
        let png_data = tiny_png();
        let shape_id = s
            .insert_image_bytes(
                &png_data,
                Emu::inches(1.0),
                Emu::inches(1.0),
                Emu::inches(3.0),
                Emu::inches(2.0),
            )
            .unwrap();

        assert!(shape_id >= 3); // id 1 = grpSp, id 2 = title shape

        // Verify the DOM contains a p:pic element.
        let dom = d.dom.as_ref().unwrap();
        let dom_xml = String::from_utf8(dom.to_bytes()).unwrap();
        assert!(dom_xml.contains("<p:pic>"), "DOM should contain <p:pic>");
        assert!(dom_xml.contains("r:embed="), "DOM should contain r:embed");
        assert!(dom_xml.contains("noChangeAspect=\"1\""));
    }

    #[test]
    fn insert_picture_has_correct_geometry() {
        let slide_xml = b"<?xml version=\"1.0\" encoding=\"UTF-8\" standalone=\"yes\"?>\
            <p:sld xmlns:a=\"http://schemas.openxmlformats.org/drawingml/2006/main\" \
            xmlns:r=\"http://schemas.openxmlformats.org/officeDocument/2006/relationships\" \
            xmlns:p=\"http://schemas.openxmlformats.org/presentationml/2006/main\">\
            <p:cSld><p:spTree>\
            <p:nvGrpSpPr><p:cNvPr id=\"1\" name=\"\"/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>\
            <p:grpSpPr/></p:spTree></p:cSld></p:sld>";
        let mut d = SlideData::new();
        d.dom = Some(zavora_slide_oxml::SlideDom::parse(slide_xml).unwrap());

        let mut s = slide(&mut d);
        let png_data = tiny_png();
        s.insert_image_bytes(
            &png_data,
            Emu(914400),  // 1 inch
            Emu(1828800), // 2 inches
            Emu(2743200), // 3 inches
            Emu(1371600), // 1.5 inches
        )
        .unwrap();

        let dom = d.dom.as_ref().unwrap();
        let dom_xml = String::from_utf8(dom.to_bytes()).unwrap();
        // Check xfrm geometry values.
        assert!(dom_xml.contains("x=\"914400\""), "x should be 914400");
        assert!(dom_xml.contains("y=\"1828800\""), "y should be 1828800");
        assert!(dom_xml.contains("cx=\"2743200\""), "cx should be 2743200");
        assert!(dom_xml.contains("cy=\"1371600\""), "cy should be 1371600");
    }

    #[test]
    fn content_hash_dedupe_same_image_twice() {
        let slide_xml = b"<?xml version=\"1.0\" encoding=\"UTF-8\" standalone=\"yes\"?>\
            <p:sld xmlns:a=\"http://schemas.openxmlformats.org/drawingml/2006/main\" \
            xmlns:r=\"http://schemas.openxmlformats.org/officeDocument/2006/relationships\" \
            xmlns:p=\"http://schemas.openxmlformats.org/presentationml/2006/main\">\
            <p:cSld><p:spTree>\
            <p:nvGrpSpPr><p:cNvPr id=\"1\" name=\"\"/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>\
            <p:grpSpPr/></p:spTree></p:cSld></p:sld>";
        let mut d = SlideData::new();
        d.dom = Some(zavora_slide_oxml::SlideDom::parse(slide_xml).unwrap());

        let png_data = tiny_png();
        {
            let mut s = slide(&mut d);
            // Insert the same image twice at different positions.
            s.insert_image_bytes(
                &png_data,
                Emu::inches(0.0),
                Emu::inches(0.0),
                Emu::inches(2.0),
                Emu::inches(2.0),
            )
            .unwrap();
            s.insert_image_bytes(
                &png_data,
                Emu::inches(3.0),
                Emu::inches(3.0),
                Emu::inches(2.0),
                Emu::inches(2.0),
            )
            .unwrap();
        }

        // Only one media entry should exist (deduplication).
        assert_eq!(
            d.media_registry.len(),
            1,
            "same image should be deduplicated"
        );
        // Only one media part in images (the first insert creates it).
        assert_eq!(d.images.len(), 1, "only one media part should be stored");
        // But the DOM should have two <p:pic> elements.
        let dom = d.dom.as_ref().unwrap();
        let dom_xml = String::from_utf8(dom.to_bytes()).unwrap();
        assert_eq!(
            dom_xml.matches("<p:pic>").count(),
            2,
            "DOM should have 2 pictures"
        );
    }

    #[test]
    fn gif_format_accepted_by_magic_bytes() {
        let slide_xml = b"<?xml version=\"1.0\" encoding=\"UTF-8\" standalone=\"yes\"?>\
            <p:sld xmlns:a=\"http://schemas.openxmlformats.org/drawingml/2006/main\" \
            xmlns:r=\"http://schemas.openxmlformats.org/officeDocument/2006/relationships\" \
            xmlns:p=\"http://schemas.openxmlformats.org/presentationml/2006/main\">\
            <p:cSld><p:spTree>\
            <p:nvGrpSpPr><p:cNvPr id=\"1\" name=\"\"/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>\
            <p:grpSpPr/></p:spTree></p:cSld></p:sld>";
        let mut d = SlideData::new();
        d.dom = Some(zavora_slide_oxml::SlideDom::parse(slide_xml).unwrap());

        let gif_data = tiny_gif();
        let mut s = slide(&mut d);
        let result = s.insert_image_bytes(
            &gif_data,
            Emu::inches(1.0),
            Emu::inches(1.0),
            Emu::inches(2.0),
            Emu::inches(2.0),
        );
        assert!(result.is_ok(), "GIF should be accepted");

        // Verify the media entry has the correct extension.
        assert_eq!(d.media_registry.len(), 1);
        assert!(d.media_registry[0].part_path.ends_with(".gif"));
    }

    #[test]
    fn insert_picture_preserves_sibling_shapes() {
        let slide_xml = b"<?xml version=\"1.0\" encoding=\"UTF-8\" standalone=\"yes\"?>\
            <p:sld xmlns:a=\"http://schemas.openxmlformats.org/drawingml/2006/main\" \
            xmlns:r=\"http://schemas.openxmlformats.org/officeDocument/2006/relationships\" \
            xmlns:p=\"http://schemas.openxmlformats.org/presentationml/2006/main\">\
            <p:cSld><p:spTree>\
            <p:nvGrpSpPr><p:cNvPr id=\"1\" name=\"\"/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>\
            <p:grpSpPr/>\
            <p:sp><p:nvSpPr><p:cNvPr id=\"2\" name=\"Title 1\"/><p:cNvSpPr/>\
            <p:nvPr><p:ph type=\"title\"/></p:nvPr></p:nvSpPr><p:spPr/>\
            <p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>Keep me</a:t></a:r></a:p></p:txBody></p:sp>\
            </p:spTree></p:cSld></p:sld>";
        let mut d = SlideData::new();
        d.dom = Some(zavora_slide_oxml::SlideDom::parse(slide_xml).unwrap());

        let mut s = slide(&mut d);
        s.insert_image_bytes(
            &tiny_png(),
            Emu::inches(1.0),
            Emu::inches(1.0),
            Emu::inches(2.0),
            Emu::inches(2.0),
        )
        .unwrap();

        // The original title shape should still be present and unchanged.
        let dom = d.dom.as_ref().unwrap();
        let dom_xml = String::from_utf8(dom.to_bytes()).unwrap();
        assert!(
            dom_xml.contains("<a:t>Keep me</a:t>"),
            "sibling shape text preserved"
        );
        assert!(
            dom_xml.contains("name=\"Title 1\""),
            "sibling shape name preserved"
        );
        // Both the original shape and the new picture should be present.
        assert!(dom_xml.contains("<p:sp>"), "original sp preserved");
        assert!(dom_xml.contains("<p:pic>"), "new pic added");
    }

    #[test]
    fn image_format_detect_works() {
        // PNG
        assert_eq!(
            ImageFormat::detect(&[0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00]),
            Some(ImageFormat::Png)
        );
        // JPEG
        assert_eq!(
            ImageFormat::detect(&[0xFF, 0xD8, 0xFF, 0xE0]),
            Some(ImageFormat::Jpeg)
        );
        // GIF87a
        assert_eq!(
            ImageFormat::detect(b"GIF87a\x01\x00"),
            Some(ImageFormat::Gif)
        );
        // GIF89a
        assert_eq!(
            ImageFormat::detect(b"GIF89a\x01\x00"),
            Some(ImageFormat::Gif)
        );
        // Unknown
        assert_eq!(ImageFormat::detect(&[0x00, 0x01, 0x02]), None);
    }

    #[test]
    fn scene_includes_chart_placeholder() {
        use zavora_slide_layout::Item;
        let mut d = SlideData::new();
        d.chart_placeholders.push(ChartPlaceholder {
            x: 914400,
            y: 914400,
            cx: 5000000,
            cy: 3000000,
            title: Some("Sales Chart".into()),
        });
        let scene = d.to_scene(12192000, 6858000);
        // The scene should not be empty — it must contain the chart placeholder.
        assert!(
            !scene.items.is_empty(),
            "scene should have items for the chart"
        );
        // There should be a Shape item (the gray placeholder box).
        let shapes = scene
            .items
            .iter()
            .filter(|i| matches!(i, Item::Shape { .. }))
            .count();
        assert!(
            shapes >= 1,
            "expected at least one Shape item for the chart placeholder"
        );
        // There should be a Text item (the chart title overlay).
        let texts = scene
            .items
            .iter()
            .filter(|i| matches!(i, Item::Text { .. }))
            .count();
        assert!(texts >= 1, "expected a Text item for the chart title");
        // Verify the bounding box is correct by checking the Shape's rect.
        let chart_shape = scene
            .items
            .iter()
            .find(|i| matches!(i, Item::Shape { .. }))
            .unwrap();
        if let Item::Shape { rect, .. } = chart_shape {
            assert_eq!(rect.x, 914400);
            assert_eq!(rect.y, 914400);
            assert_eq!(rect.w, 5000000);
            assert_eq!(rect.h, 3000000);
        }
    }

    #[test]
    fn scene_chart_placeholder_no_title() {
        use zavora_slide_layout::Item;
        let mut d = SlideData::new();
        d.chart_placeholders.push(ChartPlaceholder {
            x: 0,
            y: 0,
            cx: 4000000,
            cy: 2000000,
            title: None,
        });
        let scene = d.to_scene(12192000, 6858000);
        // Should have the shape but no text overlay.
        let shapes = scene
            .items
            .iter()
            .filter(|i| matches!(i, Item::Shape { .. }))
            .count();
        let texts = scene
            .items
            .iter()
            .filter(|i| matches!(i, Item::Text { .. }))
            .count();
        assert_eq!(shapes, 1);
        assert_eq!(texts, 0);
    }

    /// A scene is a flattening, and this is the case that makes an item index alone
    /// useless: a shape with both a fill and text is drawn twice. Both drawn elements
    /// must point back at the one shape a caller can actually edit.
    #[test]
    fn one_shape_drawn_twice_still_points_at_that_shape() {
        use zavora_slide_layout::ItemSource;

        let mut d = SlideData::new();
        {
            let mut s = slide(&mut d);
            // Shape 0: text only, drawn as one item.
            s.add_text_box(
                "First",
                Emu::inches(1.0),
                Emu::inches(1.0),
                Emu::inches(4.0),
                Emu::inches(1.0),
            );
            // Shape 1: filled and with text, drawn as two.
            s.add_text_box(
                "Second",
                Emu::inches(1.0),
                Emu::inches(3.0),
                Emu::inches(4.0),
                Emu::inches(1.0),
            )
            .fill = Some("FF0000".to_string());
        }

        let scene = d.to_scene(12_192_000, 6_858_000);
        assert!(
            scene.is_attributed(),
            "a scene must be wholly attributed or not at all"
        );

        let sources: Vec<_> = (0..scene.items.len()).map(|i| scene.source_of(i)).collect();
        assert!(
            sources.iter().all(|s| s.is_some()),
            "every drawn element must name what it came from: {sources:?}"
        );
        // Shape 1 contributes more than one drawn element, and every one names shape 1.
        let for_shape_1 = sources
            .iter()
            .filter(|s| **s == Some(ItemSource::Shape(1)))
            .count();
        assert!(
            for_shape_1 >= 2,
            "a filled shape with text is drawn at least twice, got {for_shape_1}"
        );
        assert_eq!(
            sources
                .iter()
                .filter(|s| **s == Some(ItemSource::Shape(0)))
                .count(),
            1,
            "a text-only shape is drawn once"
        );
    }

    /// The index must survive a change elsewhere on the slide, or a caller holding it
    /// would edit the wrong thing.
    ///
    /// This goes through a real save and reopen, because paragraph editing only applies
    /// to an opened slide — which is the path a caller editing a User's deck takes.
    #[test]
    fn a_shape_index_still_names_the_same_shape_after_another_is_edited() {
        use crate::presentation::Presentation;
        use crate::units::Layout;

        let dir = std::env::temp_dir().join(format!("zs-attr-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("attribution.pptx");

        let mut pres = Presentation::new();
        let idx = pres.add_slide(Layout::Blank);
        {
            let mut s = pres.slide_mut(idx).unwrap();
            for (i, text) in ["Alpha", "Beta", "Gamma"].iter().enumerate() {
                s.add_text_box(
                    text,
                    Emu::inches(1.0),
                    Emu::inches(1.0 + i as f64),
                    Emu::inches(4.0),
                    Emu::inches(1.0),
                );
            }
        }
        pres.save(&path).unwrap();

        let find_gamma = |scene: &zavora_slide_layout::Scene| {
            scene
                .items
                .iter()
                .position(|item| {
                    matches!(item, zavora_slide_layout::Item::Text { lines, .. }
                        if lines.iter().any(|l| l.text.contains("Gamma")))
                })
                .expect("Gamma is drawn")
        };

        let mut reopened = Presentation::open(&path).unwrap();
        let before = reopened.slide(0).unwrap().scene();
        assert!(
            before.is_attributed(),
            "an opened slide must be attributed too"
        );
        let gamma_shape = before.source_of(find_gamma(&before));
        assert!(gamma_shape.is_some(), "Gamma must name its shape");

        // Change a different shape, at a different length, through the engine's own
        // shape-indexed edit API.
        reopened
            .slide_mut(0)
            .unwrap()
            .add_paragraph(0, "Alpha, revised at considerably greater length")
            .expect("shape 0 accepts a paragraph once the deck is opened");

        let after = reopened.slide(0).unwrap().scene();
        assert_eq!(
            after.source_of(find_gamma(&after)),
            gamma_shape,
            "editing one shape must not change which shape another element names"
        );

        let _ = std::fs::remove_dir_all(&dir);
    }
}
