//! `Document`: a docx (over `zavora-docx`).

use zavora_docx::{Document as ZDocument, Error as ZError, Length};

use crate::deck::{ChartKind, ChartSpec, Rect};
use crate::error::{Limit, OfficeError, Result, limit};
use crate::guard::{STATE_LOST, Shared, run_guarded};
use std::sync::atomic::{AtomicBool, Ordering};

/// Test hook: the next `set_paragraph_text` fails after tagging the paragraph.
pub(crate) static PARAGRAPH_FAULT: AtomicBool = AtomicBool::new(false);
use crate::limits::Limits;
use crate::validate::{Kind, Mode, check_new_image, finish_output, validate_and_repack};

fn map_err(e: ZError) -> OfficeError {
    match e {
        ZError::Opc(e) => OfficeError::Malformed(e.to_string()),
        ZError::Oxml(e) => OfficeError::Malformed(e.to_string()),
        ZError::NoDocumentPart => {
            OfficeError::Malformed("document has no main document part".into())
        }
        ZError::Layout(e) => OfficeError::Unsupported(format!("layout: {e}")),
        other => OfficeError::Internal(other.to_string()),
    }
}

/// The text of a table, row by row.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TableData {
    /// `rows[r][c]` is the text of the cell at row `r`, column `c`.
    pub rows: Vec<Vec<String>>,
}

/// A docx document.
///
/// Parse, save, export and render calls run on the dedicated parse thread;
/// small edits run on the caller's thread.
pub struct Document {
    doc: Shared<ZDocument>,
    limits: Limits,
    images: u64,
    cells: u64,
}

impl std::fmt::Debug for Document {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Document").finish_non_exhaustive()
    }
}

impl Document {
    /// An empty document.
    #[must_use]
    pub fn new(limits: &Limits) -> Self {
        Self {
            doc: Shared::new(ZDocument::new(), limits),
            limits: limits.clone(),
            images: 0,
            cells: 0,
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
        let (doc, images, cells) = run_guarded(limits, move || {
            let r = validate_and_repack(&data, Kind::Docx, Mode::Keep, &l)?;
            Ok((
                ZDocument::from_bytes(&r.bytes).map_err(map_err)?,
                r.images,
                r.cells,
            ))
        })?;
        Ok(Self {
            doc: Shared::new(doc, limits),
            limits: limits.clone(),
            images,
            cells,
        })
    }

    fn charge_cells(&self, more: u64) -> Result<u64> {
        let total = self.cells.saturating_add(more);
        if total > self.limits.max_cells {
            return Err(limit(Limit::Cells, total));
        }
        Ok(total)
    }

    /// Serialise to a validated, canonical docx.
    ///
    /// # Errors
    /// Any limit violation of the produced file.
    pub fn save(&self) -> Result<Vec<u8>> {
        self.doc.guarded(|d, l| {
            let raw = d.to_bytes().map_err(map_err)?;
            finish_output(&raw, Kind::Docx, l)
        })
    }

    /// Append a paragraph.
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn add_paragraph(&mut self, text: &str) -> Result<()> {
        self.doc.lock()?.add_paragraph(text);
        Ok(())
    }

    /// Append a heading of `level` 1 to 9.
    ///
    /// # Errors
    /// [`OfficeError::InvalidInput`] for a level outside 1 to 9.
    pub fn add_heading(&mut self, text: &str, level: u8) -> Result<()> {
        if !(1..=9).contains(&level) {
            return Err(OfficeError::InvalidInput(
                "heading level must be 1 to 9".into(),
            ));
        }
        self.doc
            .lock()?
            .add_paragraph(text)
            .style(&format!("Heading{level}"));
        Ok(())
    }

    /// Replace the text of the `index`th paragraph (0-based, tables and
    /// other body content do not count). The first run's formatting is kept.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] for a bad index.
    pub fn set_paragraph_text(&mut self, index: usize, text: &str) -> Result<()> {
        let text = text.to_owned();
        self.doc.guarded_mut(move |d, _| {
            const MARKER: &str = "\u{E000}office-core-marker-9c1d\u{E000}";
            if d.find_content_index(MARKER).is_some() {
                return Err(OfficeError::Unsupported(
                    "the document contains the internal marker text".into(),
                ));
            }
            // Snapshot before the tag goes in: the library cannot remove a run
            // it added, so every failure after tagging restores this.
            let snapshot = d.to_bytes().map_err(map_err)?;
            let outcome = (|| {
                d.paragraph_mut(index)
                    .ok_or_else(|| OfficeError::NotFound(format!("paragraph {index}")))?
                    .add_run(MARKER);
                if PARAGRAPH_FAULT.swap(false, Ordering::SeqCst) {
                    return Err(OfficeError::Internal("injected failure".into()));
                }
                // The library addresses body content (paragraphs, tables, raw
                // blocks) by one index. The tag tells which one is ours;
                // setting the text drops it.
                match d.find_content_index(MARKER) {
                    Some(c) if d.set_paragraph_text(c, &text) => Ok(()),
                    _ => Err(OfficeError::Internal(
                        "paragraph index mapping failed".into(),
                    )),
                }
            })();
            if let Err(e) = outcome {
                if d.find_content_index(MARKER).is_some() {
                    return match ZDocument::from_bytes(&snapshot) {
                        Ok(restored) => {
                            *d = restored;
                            Err(e)
                        }
                        Err(_) => Err(OfficeError::Internal(STATE_LOST.into())),
                    };
                }
                return Err(e);
            }
            Ok(())
        })
    }

    /// Append a `rows` by `cols` table; returns its index among the tables.
    ///
    /// # Errors
    /// [`Limit::Cells`] when `rows * cols` exceeds `max_cells`.
    pub fn add_table(&mut self, rows: usize, cols: usize) -> Result<usize> {
        if rows == 0 || cols == 0 {
            return Err(OfficeError::InvalidInput(
                "a table needs at least one row and column".into(),
            ));
        }
        let total = self.charge_cells((rows as u64).saturating_mul(cols as u64))?;
        let idx = {
            let mut d = self.doc.lock()?;
            let idx = d.table_count();
            d.add_table(rows, cols);
            idx
        };
        self.cells = total;
        Ok(idx)
    }

    /// Set the text of a table cell.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] for a bad table or cell.
    pub fn set_table_cell(
        &mut self,
        table: usize,
        row: usize,
        col: usize,
        text: &str,
    ) -> Result<()> {
        let mut d = self.doc.lock()?;
        let mut t = d
            .table_mut(table)
            .ok_or_else(|| OfficeError::NotFound(format!("table {table}")))?;
        let mut c = t
            .cell(row, col)
            .ok_or_else(|| OfficeError::NotFound(format!("cell ({row},{col}) of table {table}")))?;
        c.set_text(text);
        Ok(())
    }

    /// The text of every table.
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn tables(&self) -> Result<Vec<TableData>> {
        let d = self.doc.lock()?;
        Ok(d.tables()
            .iter()
            .map(|t| TableData {
                rows: (0..t.row_count())
                    .map(|r| {
                        (0..t.column_count())
                            .map(|c| t.cell(r, c).map(|c| c.text()).unwrap_or_default())
                            .collect()
                    })
                    .collect(),
            })
            .collect())
    }

    /// Replace every occurrence of `find` (also when it is split across runs)
    /// in the body, tables, headers and footers; returns the count.
    ///
    /// # Errors
    /// [`OfficeError::InvalidInput`] for an empty `find`.
    pub fn replace_text(&mut self, find: &str, replacement: &str) -> Result<usize> {
        if find.is_empty() {
            return Err(OfficeError::InvalidInput(
                "the text to find is empty".into(),
            ));
        }
        Ok(self.doc.lock()?.replace_text(find, replacement))
    }

    /// The heading outline as `(level, text)`.
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn headings(&self) -> Result<Vec<(u32, String)>> {
        Ok(self.doc.lock()?.headings())
    }

    /// Insert an inline PNG, JPEG or GIF. The signature and header are
    /// checked first, and the part name and content type come from the
    /// sniffed format, never from the caller.
    ///
    /// # Errors
    /// [`Limit::ImagePixels`], [`Limit::Images`], [`Limit::MediaPartBytes`],
    /// [`OfficeError::InvalidInput`].
    pub fn add_image(&mut self, data: &[u8], size: Rect) -> Result<()> {
        let ext = check_new_image(data, self.images, &self.limits)?;
        self.doc.lock()?.add_picture(
            data,
            &format!("image.{ext}"),
            Length::emu(size.w),
            Length::emu(size.h),
        );
        self.images += 1;
        Ok(())
    }

    /// Append a chart (pptx-only kinds return `Unsupported`). The page
    /// renderer may not draw it (ADR-0032 Consequences); it is real in the
    /// saved file.
    ///
    /// # Errors
    /// [`OfficeError::Unsupported`] for stacked bars and doughnuts.
    pub fn add_chart(&mut self, spec: &ChartSpec, size: Rect) -> Result<()> {
        let points = spec.check(&self.limits)?;
        let total = self.charge_cells(points)?;
        let kind = match spec.kind {
            ChartKind::ClusteredBar => zavora_docx::ChartKind::Bar,
            ChartKind::ClusteredColumn => zavora_docx::ChartKind::Column,
            ChartKind::Line => zavora_docx::ChartKind::Line,
            ChartKind::Pie => zavora_docx::ChartKind::Pie,
            ChartKind::Area => zavora_docx::ChartKind::Area,
            ChartKind::Scatter => zavora_docx::ChartKind::Scatter,
            ChartKind::StackedBar | ChartKind::StackedColumn | ChartKind::Doughnut => {
                return Err(OfficeError::Unsupported(
                    "docx charts support bar, column, line, pie, area and scatter".into(),
                ));
            }
        };
        let chart = zavora_docx::Chart {
            kind,
            title: spec.title.clone(),
            categories: spec.categories.clone(),
            series: spec
                .series
                .iter()
                .map(|s| zavora_docx::Series {
                    name: s.name.clone(),
                    values: s.values.clone(),
                })
                .collect(),
            labels: None,
        };
        self.doc
            .lock()?
            .add_chart(&chart, Length::emu(size.w), Length::emu(size.h));
        self.cells = total;
        Ok(())
    }

    /// All text in document order, table cells one per line.
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn to_plain_text(&self) -> Result<String> {
        Ok(self.doc.lock()?.to_plain_text())
    }

    /// The document as Markdown.
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn to_markdown(&self) -> Result<String> {
        self.doc.guarded(|d, _| Ok(d.to_markdown()))
    }

    /// Export to PDF. Uses the bundled font set only (Carlito, Caladea,
    /// Liberation): embedded document fonts are emptied before the library
    /// runs, and `zavora-docx-layout` loads no system fonts.
    ///
    /// # Errors
    /// [`Limit::RenderPixels`].
    pub fn export_pdf(&self) -> Result<Vec<u8>> {
        self.doc
            .guarded(|d, l| render_copy(d, l, 0.0)?.to_pdf().map_err(map_err))
    }

    /// Render one page (0-based) to PNG at `dpi` (36 to 300).
    ///
    /// # Errors
    /// [`Limit::RenderPixels`] when the page would exceed the pixel budget,
    /// [`OfficeError::NotFound`] for a page past the end.
    pub fn render_page_png(&self, page: usize, dpi: f64) -> Result<Vec<u8>> {
        if !(36.0..=300.0).contains(&dpi) {
            return Err(OfficeError::InvalidInput("dpi must be 36 to 300".into()));
        }
        self.doc.guarded(move |d, l| {
            render_copy(d, l, dpi)?
                .render_page_to_png(page, dpi)
                .map_err(map_err)?
                .ok_or_else(|| OfficeError::NotFound(format!("page {page}")))
        })
    }
}

fn render_copy(d: &mut ZDocument, l: &Limits, dpi: f64) -> Result<ZDocument> {
    let raw = d.to_bytes().map_err(map_err)?;
    let r = validate_and_repack(&raw, Kind::Docx, Mode::Render { dpi }, l)?;
    ZDocument::from_bytes(&r.bytes).map_err(map_err)
}
