//! `Deck`: a pptx presentation (over `zavora-slide`).

use std::collections::HashMap;
use std::sync::{Arc, Mutex, TryLockError};

use zavora_slide::{Bullet as ZBullet, Emu, Layout, Presentation, RenderFormat, SlideError};

use crate::error::{Limit, OfficeError, Result, limit};
use crate::guard::run_guarded;
use crate::limits::Limits;
use crate::validate::{Kind, Mode, check_new_image, finish_output, validate_and_repack};

fn map_err(e: SlideError) -> OfficeError {
    match e {
        SlideError::NotFound(m) => OfficeError::NotFound(m),
        SlideError::InvalidInput(m) => OfficeError::InvalidInput(m),
        SlideError::Unsupported(m) => OfficeError::Unsupported(m),
        SlideError::Opc(e) => OfficeError::Malformed(e.to_string()),
        SlideError::Oxml(e) => OfficeError::Malformed(e.to_string()),
        SlideError::Io(e) => OfficeError::Internal(e.to_string()),
    }
}

/// Position and size on a slide, in EMU (914 400 per inch).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Rect {
    /// Left edge.
    pub x: i64,
    /// Top edge.
    pub y: i64,
    /// Width.
    pub w: i64,
    /// Height.
    pub h: i64,
}

impl Rect {
    /// A rectangle given in inches.
    #[must_use]
    #[allow(clippy::many_single_char_names)]
    pub fn inches(x: f64, y: f64, w: f64, h: f64) -> Self {
        let e = |v: f64| Emu::inches(v).0;
        Self {
            x: e(x),
            y: e(y),
            w: e(w),
            h: e(h),
        }
    }
}

/// Built-in slide layouts.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[non_exhaustive]
pub enum SlideLayout {
    /// Title slide.
    Title,
    /// Title and one content placeholder.
    TitleContent,
    /// Section header.
    SectionHeader,
    /// Title and two content placeholders.
    TwoContent,
    /// Empty slide.
    Blank,
}

/// One bullet line.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Bullet {
    /// The text.
    pub text: String,
    /// Indent level, 0 is the top.
    pub level: u8,
    /// Bold run.
    pub bold: bool,
}

impl Bullet {
    /// A level-0, non-bold bullet.
    #[must_use]
    pub fn new(text: impl Into<String>) -> Self {
        Self {
            text: text.into(),
            level: 0,
            bold: false,
        }
    }
}

/// Chart kinds. `Stacked*` are pptx only (a docx chart returns `Unsupported`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[non_exhaustive]
pub enum ChartKind {
    /// Horizontal bars.
    ClusteredBar,
    /// Horizontal stacked bars (pptx only).
    StackedBar,
    /// Vertical bars.
    ClusteredColumn,
    /// Vertical stacked bars (pptx only).
    StackedColumn,
    /// Lines.
    Line,
    /// Pie.
    Pie,
    /// Doughnut (pptx only).
    Doughnut,
    /// Area.
    Area,
    /// Scatter.
    Scatter,
}

/// A named data series.
#[derive(Debug, Clone, PartialEq)]
pub struct Series {
    /// Legend name.
    pub name: String,
    /// One value per category.
    pub values: Vec<f64>,
}

/// A chart to add. The preview renderer draws charts as placeholders; the
/// chart is real in the saved file (ADR-0032 Consequences).
#[derive(Debug, Clone, PartialEq)]
pub struct ChartSpec {
    /// Kind of chart.
    pub kind: ChartKind,
    /// Category labels.
    pub categories: Vec<String>,
    /// Series, each with one value per category.
    pub series: Vec<Series>,
    /// Title.
    pub title: Option<String>,
    /// `b`, `t`, `l`, `r` or `tr`; `None` for no legend.
    pub legend_position: Option<String>,
    /// Show values on the data points.
    pub data_labels: bool,
}

impl ChartSpec {
    pub(crate) fn check(&self, l: &Limits) -> Result<()> {
        let points = (self.categories.len() as u64).saturating_mul(self.series.len().max(1) as u64);
        if points > l.max_cells {
            return Err(limit(Limit::Cells, points));
        }
        if self
            .series
            .iter()
            .any(|s| s.values.len() != self.categories.len())
        {
            return Err(OfficeError::InvalidInput(
                "every series needs one value per category".into(),
            ));
        }
        Ok(())
    }
}

/// Theme overrides: unset fields keep the Office defaults.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Theme {
    /// `accent1` as hex.
    pub accent: Option<String>,
    /// Heading font family.
    pub heading_font: Option<String>,
    /// Body font family.
    pub body_font: Option<String>,
    /// Scheme colour overrides (`dk2`, `lt2`, `accent1`..`accent6`, `hlink`,
    /// `folHlink`) to hex.
    pub colors: HashMap<String, String>,
}

/// One run of a paragraph: a text run, or a line break (`a:br`). Run
/// indexes in [`Deck::set_run_text`] count both, as the library does.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RunInfo {
    /// The run's text (empty for a line break).
    pub text: String,
    /// True for a line break.
    pub line_break: bool,
}

/// One paragraph of a shape.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ParagraphInfo {
    /// The paragraph's text, line breaks as `\n`.
    pub text: String,
    /// Its runs, in order.
    pub runs: Vec<RunInfo>,
}

/// One text shape of a slide.
///
/// There is no stable id in the library. `index` is the shape's position among
/// the slide's `p:sp` elements, the index space every edit call uses; it
/// changes when shapes are added or deleted (ADR-0032 Consequences). For a
/// slide opened from a file the inventory is read from the slide's own XML
/// (the library's `shapes()` merges all shapes of an opened slide into one);
/// for a slide authored in this session the library's coarser list is used and
/// run-level editing is `Unsupported`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ShapeInfo {
    /// Position among the slide's `p:sp` elements.
    pub index: usize,
    /// The shape's name (empty for authored slides).
    pub name: String,
    /// Placeholder type (`title`, `body`, ...), `textbox` or `shape`.
    pub kind: String,
    /// True for a layout placeholder.
    pub is_placeholder: bool,
    /// All text, paragraphs joined by `\n`.
    pub text: String,
    /// The paragraphs.
    pub paragraphs: Vec<ParagraphInfo>,
}

/// A placeholder found by [`Deck::list_placeholders`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PlaceholderRef {
    /// 0-based slide.
    pub slide: usize,
    /// The shape.
    pub shape: ShapeInfo,
}

/// A pptx presentation.
///
/// Parse, save and render calls run on a dedicated 16 MiB thread
/// ([`crate::PARSE_STACK_BYTES`]) under a permit; small edits run on the
/// caller's thread. All calls are synchronous: from async code, wrap them in
/// `spawn_blocking`.
pub struct Deck {
    pres: Arc<Mutex<Presentation>>,
    limits: Limits,
    images: u64,
    next_chart: usize,
}

impl std::fmt::Debug for Deck {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Deck").finish_non_exhaustive()
    }
}

impl Deck {
    /// An empty deck.
    #[must_use]
    pub fn new(limits: &Limits) -> Self {
        Self {
            pres: Arc::new(Mutex::new(Presentation::new())),
            limits: limits.clone(),
            images: 0,
            next_chart: 1,
        }
    }

    /// Validate `bytes` (see [`crate::Limits`]) and open them.
    ///
    /// # Errors
    /// [`OfficeError::LimitExceeded`], [`OfficeError::Malformed`],
    /// [`OfficeError::Unsupported`], [`OfficeError::Busy`],
    /// [`OfficeError::DeadlineExceeded`].
    pub fn open(bytes: &[u8], limits: &Limits) -> Result<Self> {
        crate::validate::precheck_len(bytes.len(), limits)?;
        let (data, l) = (bytes.to_vec(), limits.clone());
        let (pres, images, max_chart) = run_guarded(limits, move || {
            let r = validate_and_repack(&data, Kind::Pptx, Mode::Keep, &l)?;
            let p = Presentation::open_from_bytes(&r.bytes).map_err(map_err)?;
            if p.slide_count() as u64 > l.max_slides {
                return Err(limit(Limit::Slides, p.slide_count() as u64));
            }
            Ok((p, r.images, r.max_chart_index))
        })?;
        Ok(Self {
            pres: Arc::new(Mutex::new(pres)),
            limits: limits.clone(),
            images,
            next_chart: usize::try_from(max_chart).unwrap_or(0) + 1,
        })
    }

    fn lock(&self) -> Result<std::sync::MutexGuard<'_, Presentation>> {
        self.pres.try_lock().map_err(|e| match e {
            TryLockError::WouldBlock => OfficeError::Busy,
            TryLockError::Poisoned(_) => {
                OfficeError::Internal("deck state poisoned by a panic".into())
            }
        })
    }

    fn guarded<R, F>(&self, f: F) -> Result<R>
    where
        R: Send + 'static,
        F: FnOnce(&Presentation, &Limits) -> Result<R> + Send + 'static,
    {
        let (pres, l) = (self.pres.clone(), self.limits.clone());
        run_guarded(&self.limits, move || {
            let g = pres.try_lock().map_err(|e| match e {
                TryLockError::WouldBlock => OfficeError::Busy,
                TryLockError::Poisoned(_) => OfficeError::Internal("deck state poisoned".into()),
            })?;
            f(&g, &l)
        })
    }

    /// Serialise to a validated, canonical pptx.
    ///
    /// # Errors
    /// Any limit violation of the produced file, [`OfficeError::Busy`].
    pub fn save(&self) -> Result<Vec<u8>> {
        self.guarded(|p, l| {
            let raw = p.save_to_buffer().map_err(map_err)?;
            let out = finish_output(&raw, Kind::Pptx, l)?;
            if out.len() as u64 > l.max_input_bytes {
                return Err(limit(Limit::InputBytes, out.len() as u64));
            }
            Ok(out)
        })
    }

    /// Number of slides.
    ///
    /// # Errors
    /// [`OfficeError::Busy`] when a runaway call holds the deck.
    pub fn slide_count(&self) -> Result<usize> {
        Ok(self.lock()?.slide_count())
    }

    /// Append a slide; returns its 0-based index.
    ///
    /// # Errors
    /// [`Limit::Slides`] past `max_slides`.
    pub fn add_slide(&mut self, layout: SlideLayout) -> Result<usize> {
        let mut p = self.lock()?;
        let n = p.slide_count() as u64 + 1;
        if n > self.limits.max_slides {
            return Err(limit(Limit::Slides, n));
        }
        Ok(p.add_slide(match layout {
            SlideLayout::Title => Layout::Title,
            SlideLayout::TitleContent => Layout::TitleContent,
            SlideLayout::SectionHeader => Layout::SectionHeader,
            SlideLayout::TwoContent => Layout::TwoContent,
            SlideLayout::Blank => Layout::Blank,
        }))
    }

    /// Delete a slide.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] for a bad index.
    pub fn delete_slide(&mut self, index: usize) -> Result<()> {
        self.lock()?.delete_slide(index).map_err(map_err)
    }

    /// Move slide `from` so that it ends up at index `to`.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] for a bad index.
    pub fn move_slide(&mut self, from: usize, to: usize) -> Result<()> {
        self.lock()?.move_slide(from, to).map_err(map_err)
    }

    /// Set the title placeholder text.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] when the slide has no title placeholder.
    pub fn set_title(&mut self, slide: usize, text: &str) -> Result<()> {
        self.lock()?
            .slide_mut(slide)
            .map_err(map_err)?
            .set_title(text)
            .map_err(map_err)
    }

    /// Add a text box.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] for a bad slide.
    pub fn add_text(&mut self, slide: usize, text: &str, at: Rect) -> Result<()> {
        let mut p = self.lock()?;
        let mut s = p.slide_mut(slide).map_err(map_err)?;
        s.add_text_box(text, Emu(at.x), Emu(at.y), Emu(at.w), Emu(at.h));
        Ok(())
    }

    /// Fill the slide's body placeholder with bullets.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] when there is no body placeholder.
    pub fn add_bullets(&mut self, slide: usize, items: &[Bullet]) -> Result<()> {
        let z: Vec<ZBullet> = items
            .iter()
            .map(|b| ZBullet {
                text: b.text.clone(),
                level: b.level,
                bold: b.bold,
            })
            .collect();
        self.lock()?
            .slide_mut(slide)
            .map_err(map_err)?
            .add_bullets(&z)
            .map_err(map_err)
    }

    /// Add a `rows` by `cols` table; returns its index among the slide's
    /// tables created in this session, for [`Deck::set_table_cell`].
    ///
    /// # Errors
    /// [`Limit::Cells`] when `rows * cols` exceeds `max_cells`.
    pub fn add_table(&mut self, slide: usize, rows: usize, cols: usize, at: Rect) -> Result<usize> {
        if rows == 0 || cols == 0 {
            return Err(OfficeError::InvalidInput(
                "a table needs at least one row and column".into(),
            ));
        }
        let cells = (rows as u64).saturating_mul(cols as u64);
        if cells > self.limits.max_cells {
            return Err(limit(Limit::Cells, cells));
        }
        let mut p = self.lock()?;
        let mut s = p.slide_mut(slide).map_err(map_err)?;
        Ok(
            s.add_table(rows, cols, Emu(at.x), Emu(at.y), Emu(at.w), Emu(at.h))
                .0,
        )
    }

    /// Set a cell of a table added with [`Deck::add_table`].
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] or [`OfficeError::InvalidInput`] for a bad
    /// table or cell.
    pub fn set_table_cell(
        &mut self,
        slide: usize,
        table: usize,
        row: usize,
        col: usize,
        text: &str,
    ) -> Result<()> {
        self.lock()?
            .slide_mut(slide)
            .map_err(map_err)?
            .set_table_cell(zavora_slide::TableId(table), row, col, text)
            .map_err(map_err)
    }

    /// Set a cell of a table that came from an opened deck, addressed by its
    /// shape index.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] when the shape is not a table.
    pub fn set_shape_table_cell(
        &mut self,
        slide: usize,
        shape: usize,
        row: usize,
        col: usize,
        text: &str,
    ) -> Result<()> {
        self.lock()?
            .slide_mut(slide)
            .map_err(map_err)?
            .table_set_cell_text(shape, row, col, text)
            .map_err(map_err)
    }

    /// Add a chart.
    ///
    /// # Errors
    /// [`Limit::Cells`] for a chart with too many points.
    pub fn add_chart(&mut self, slide: usize, spec: &ChartSpec, at: Rect) -> Result<()> {
        spec.check(&self.limits)?;
        let kind = match spec.kind {
            ChartKind::ClusteredBar => zavora_slide::ChartKind::ClusteredBar,
            ChartKind::StackedBar => zavora_slide::ChartKind::StackedBar,
            ChartKind::ClusteredColumn => zavora_slide::ChartKind::ClusteredColumn,
            ChartKind::StackedColumn => zavora_slide::ChartKind::StackedColumn,
            ChartKind::Line => zavora_slide::ChartKind::Line,
            ChartKind::Pie => zavora_slide::ChartKind::Pie,
            ChartKind::Doughnut => zavora_slide::ChartKind::Doughnut,
            ChartKind::Area => zavora_slide::ChartKind::Area,
            ChartKind::Scatter => zavora_slide::ChartKind::Scatter,
        };
        let z = zavora_slide::ChartSpec {
            kind,
            categories: spec.categories.clone(),
            series: spec
                .series
                .iter()
                .map(|s| (s.name.clone(), s.values.clone()))
                .collect(),
            title: spec.title.clone(),
            legend_position: spec.legend_position.clone(),
            data_labels: spec.data_labels,
        };
        let index = self.next_chart;
        self.lock()?
            .slide_mut(slide)
            .map_err(map_err)?
            .add_chart(&z, Emu(at.x), Emu(at.y), Emu(at.w), Emu(at.h), index)
            .map_err(map_err)?;
        self.next_chart += 1;
        Ok(())
    }

    /// Insert a PNG, JPEG or GIF. The header is read first: more than
    /// `max_image_pixels` pixels, or an unreadable header, is refused before
    /// the library sees the bytes.
    ///
    /// # Errors
    /// [`Limit::ImagePixels`], [`Limit::Images`], [`Limit::MediaPartBytes`],
    /// [`OfficeError::InvalidInput`].
    pub fn add_image(&mut self, slide: usize, data: &[u8], at: Rect) -> Result<()> {
        check_new_image(data, self.images, &self.limits)?;
        self.lock()?
            .slide_mut(slide)
            .map_err(map_err)?
            .insert_image_bytes(data, Emu(at.x), Emu(at.y), Emu(at.w), Emu(at.h))
            .map_err(map_err)?;
        self.images += 1;
        Ok(())
    }

    /// Set the speaker notes.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] for a bad slide.
    pub fn set_notes(&mut self, slide: usize, text: &str) -> Result<()> {
        self.lock()?
            .slide_mut(slide)
            .map_err(map_err)?
            .set_notes(text);
        Ok(())
    }

    /// The speaker notes, if any.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] for a bad slide.
    pub fn notes(&self, slide: usize) -> Result<Option<String>> {
        Ok(self
            .lock()?
            .slide(slide)
            .map_err(map_err)?
            .notes()
            .map(str::to_owned))
    }

    /// Apply a theme (colours and fonts).
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn apply_theme(&mut self, theme: &Theme) -> Result<()> {
        let z = zavora_slide::ThemeSpec {
            accent: theme.accent.clone(),
            heading_font: theme.heading_font.clone(),
            body_font: theme.body_font.clone(),
            colors: theme.colors.clone(),
        };
        self.lock()?.apply_theme(&z);
        Ok(())
    }

    /// The text shapes of a slide, in shape-tree order.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] for a bad slide.
    pub fn shapes(&self, slide: usize) -> Result<Vec<ShapeInfo>> {
        let p = self.lock()?;
        let fallback = p.slide(slide).map_err(map_err)?.shapes();
        if let Some((_, xml)) = p.slide_dom_debug(slide) {
            return Ok(read_shapes(&xml));
        }
        Ok(fallback
            .into_iter()
            .enumerate()
            .map(|(index, i)| ShapeInfo {
                index,
                name: String::new(),
                is_placeholder: i.kind != "textbox" && i.kind != "shape",
                paragraphs: i
                    .text
                    .split('\n')
                    .map(|t| ParagraphInfo {
                        text: t.to_owned(),
                        runs: vec![RunInfo {
                            text: t.to_owned(),
                            line_break: false,
                        }],
                    })
                    .collect(),
                kind: i.kind,
                text: i.text,
            })
            .collect())
    }

    /// Every layout placeholder of the deck, with its slide.
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn list_placeholders(&self) -> Result<Vec<PlaceholderRef>> {
        let n = self.slide_count()?;
        let mut out = Vec::new();
        for slide in 0..n {
            out.extend(
                self.shapes(slide)?
                    .into_iter()
                    .filter(|s| s.is_placeholder)
                    .map(|shape| PlaceholderRef { slide, shape }),
            );
        }
        Ok(out)
    }

    /// Number of runs in a paragraph.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] for a bad address.
    pub fn run_count(&self, slide: usize, shape: usize, paragraph: usize) -> Result<usize> {
        self.lock()?
            .slide_mut(slide)
            .map_err(map_err)?
            .run_count(shape, paragraph)
            .map_err(map_err)
    }

    /// Replace the text of one run, keeping its formatting.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] for a bad address.
    pub fn set_run_text(
        &mut self,
        slide: usize,
        shape: usize,
        paragraph: usize,
        run: usize,
        text: &str,
    ) -> Result<()> {
        self.lock()?
            .slide_mut(slide)
            .map_err(map_err)?
            .edit_run_text(shape, paragraph, run, text)
            .map_err(map_err)
    }

    /// Replace `find` by `replacement` in the text shapes of every slide and
    /// return the number of occurrences replaced.
    ///
    /// Each occurrence inside one run is replaced in place, keeping the run's
    /// formatting. An occurrence split across runs is rewritten into the
    /// paragraph's first run and the other runs are deleted, so that paragraph
    /// keeps the first run's formatting; a paragraph with line breaks in that
    /// case is [`OfficeError::Unsupported`] (the raw-part editor of ADR-0032 P3
    /// covers it). Tables, charts and notes are not searched.
    ///
    /// # Errors
    /// [`OfficeError::InvalidInput`] for an empty `find`;
    /// [`OfficeError::Unsupported`] as above or on an authored slide.
    pub fn replace_text(&mut self, find: &str, replacement: &str) -> Result<usize> {
        if find.is_empty() {
            return Err(OfficeError::InvalidInput(
                "the text to find is empty".into(),
            ));
        }
        let mut count = 0;
        for slide in 0..self.slide_count()? {
            for shape in self.shapes(slide)? {
                for (pi, para) in shape.paragraphs.iter().enumerate() {
                    if !para.text.contains(find) {
                        continue;
                    }
                    let total = para.text.matches(find).count();
                    count += total;
                    let in_runs: usize =
                        para.runs.iter().map(|r| r.text.matches(find).count()).sum();
                    if in_runs == total {
                        for (ri, run) in para.runs.iter().enumerate() {
                            if run.text.contains(find) {
                                self.set_run_text(
                                    slide,
                                    shape.index,
                                    pi,
                                    ri,
                                    &run.text.replace(find, replacement),
                                )?;
                            }
                        }
                        continue;
                    }
                    // An occurrence spans runs: rewrite the paragraph into its first run.
                    if para.runs.iter().any(|r| r.line_break) {
                        return Err(OfficeError::Unsupported(
                            "an occurrence spans runs in a paragraph with line breaks".into(),
                        ));
                    }
                    self.set_run_text(
                        slide,
                        shape.index,
                        pi,
                        0,
                        &para.text.replace(find, replacement),
                    )?;
                    let mut p = self.lock()?;
                    let mut s = p.slide_mut(slide).map_err(map_err)?;
                    for r in (1..para.runs.len()).rev() {
                        s.delete_run(shape.index, pi, r).map_err(map_err)?;
                    }
                }
            }
        }
        Ok(count)
    }

    /// Markdown for the whole deck (slides, tables, notes, chart data).
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn to_markdown(&self) -> Result<String> {
        self.guarded(|p, _| Ok(p.to_markdown()))
    }

    /// Render one slide to PNG at the fixed 1280 px width. Approximate: charts
    /// draw as placeholders, SVG images draw as nothing, and system fonts can
    /// change the text metrics (see the crate documentation, "Determinism").
    ///
    /// # Errors
    /// [`Limit::RenderPixels`], [`OfficeError::NotFound`].
    pub fn render_preview(&self, slide: usize) -> Result<Vec<u8>> {
        self.guarded(move |p, l| {
            let copy = render_copy(p, l)?;
            copy.render_slide(slide, RenderFormat::Png).map_err(map_err)
        })
    }

    /// Render the whole deck to PDF, one page per slide.
    ///
    /// # Errors
    /// [`Limit::RenderPixels`].
    pub fn export_pdf(&self) -> Result<Vec<u8>> {
        self.guarded(|p, l| render_copy(p, l)?.to_pdf_bytes().map_err(map_err))
    }
}

/// Save, re-validate in render mode (fonts emptied, SVG neutralised, pixel
/// budget) and reopen: the renderer only ever sees the sanitised copy.
fn render_copy(p: &Presentation, l: &Limits) -> Result<Presentation> {
    let raw = p.save_to_buffer().map_err(map_err)?;
    let r = validate_and_repack(&raw, Kind::Pptx, Mode::Render { dpi: 0.0 }, l)?;
    Presentation::open_from_bytes(&r.bytes).map_err(map_err)
}

/// The text shapes (`p:sp`) of an opened slide, from its serialised XML.
#[allow(clippy::too_many_lines)]
fn read_shapes(xml: &[u8]) -> Vec<ShapeInfo> {
    use quick_xml::Reader;
    use quick_xml::events::Event;

    let mut reader = Reader::from_reader(xml);
    let mut stack: Vec<Vec<u8>> = Vec::new();
    let mut out: Vec<ShapeInfo> = Vec::new();
    let mut cur: Option<ShapeInfo> = None;
    let mut para: Option<ParagraphInfo> = None;
    let mut run: Option<String> = None;
    let mut in_t = false;
    loop {
        let ev = reader.read_event();
        let is_start = matches!(ev, Ok(Event::Start(_)));
        match ev {
            Ok(Event::Start(e) | Event::Empty(e)) => {
                let name = e.local_name().as_ref().to_vec();
                let parent_is = |n: &[u8]| stack.last().is_some_and(|p| p == n);
                match name.as_slice() {
                    b"sp" if parent_is(b"spTree") => {
                        cur = Some(ShapeInfo {
                            index: out.len(),
                            name: String::new(),
                            kind: "shape".into(),
                            is_placeholder: false,
                            text: String::new(),
                            paragraphs: Vec::new(),
                        });
                    }
                    b"cNvPr"
                        if cur.is_some() && cur.as_ref().is_some_and(|c| c.name.is_empty()) =>
                    {
                        if let Some(c) = cur.as_mut() {
                            for a in e.attributes().flatten() {
                                if a.key.local_name().as_ref() == b"name" {
                                    c.name = String::from_utf8_lossy(&a.value).into_owned();
                                }
                            }
                        }
                    }
                    b"ph" => {
                        if let Some(c) = cur.as_mut() {
                            c.is_placeholder = true;
                            c.kind = "body".into();
                            for a in e.attributes().flatten() {
                                if a.key.local_name().as_ref() == b"type" {
                                    c.kind = String::from_utf8_lossy(&a.value).into_owned();
                                }
                            }
                        }
                    }
                    b"cNvSpPr" => {
                        if let Some(c) = cur.as_mut()
                            && !c.is_placeholder
                            && e.attributes()
                                .flatten()
                                .any(|a| a.key.local_name().as_ref() == b"txBox")
                        {
                            c.kind = "textbox".into();
                        }
                    }
                    b"p" if cur.is_some() => {
                        para = Some(ParagraphInfo {
                            text: String::new(),
                            runs: Vec::new(),
                        });
                    }
                    b"r" if para.is_some() => run = Some(String::new()),
                    b"t" if run.is_some() => in_t = true,
                    b"br" if para.is_some() => {
                        if let Some(p) = para.as_mut() {
                            p.runs.push(RunInfo {
                                text: String::new(),
                                line_break: true,
                            });
                            p.text.push('\n');
                        }
                    }
                    _ => {}
                }
                if is_start {
                    stack.push(name);
                }
            }
            Ok(Event::End(e)) => {
                stack.pop();
                match e.local_name().as_ref() {
                    b"t" => in_t = false,
                    b"r" => {
                        if let (Some(r), Some(p)) = (run.take(), para.as_mut()) {
                            p.text.push_str(&r);
                            p.runs.push(RunInfo {
                                text: r,
                                line_break: false,
                            });
                        }
                    }
                    b"p" => {
                        if let (Some(p), Some(c)) = (para.take(), cur.as_mut()) {
                            c.paragraphs.push(p);
                        }
                    }
                    b"sp" if stack.last().is_some_and(|p| p == b"spTree") => {
                        if let Some(mut c) = cur.take() {
                            c.text = c
                                .paragraphs
                                .iter()
                                .map(|p| p.text.as_str())
                                .collect::<Vec<_>>()
                                .join("\n");
                            out.push(c);
                        }
                    }
                    _ => {}
                }
            }
            Ok(Event::Text(t)) if in_t => {
                if let (Some(r), Ok(s)) = (run.as_mut(), t.decode()) {
                    r.push_str(&s);
                }
            }
            Ok(Event::GeneralRef(g)) if in_t => {
                if let Some(r) = run.as_mut() {
                    if let Ok(Some(c)) = g.resolve_char_ref() {
                        r.push(c);
                    } else if let Ok(n) = g.decode() {
                        match n.as_ref() {
                            "amp" => r.push('&'),
                            "lt" => r.push('<'),
                            "gt" => r.push('>'),
                            "quot" => r.push('"'),
                            "apos" => r.push('\''),
                            _ => {}
                        }
                    }
                }
            }
            Ok(Event::Eof) | Err(_) => break,
            Ok(_) => {}
        }
    }
    out
}
