//! `Document`: a docx (over `zavora-docx`).

use std::sync::{Arc, Mutex, MutexGuard, TryLockError};

use zavora_docx::{Document as ZDocument, Error as ZError, Length};

use crate::deck::{ChartKind, ChartSpec, Rect};
use crate::error::{Limit, OfficeError, Result, limit};
use crate::guard::run_guarded;
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
    doc: Arc<Mutex<ZDocument>>,
    limits: Limits,
    images: u64,
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
            doc: Arc::new(Mutex::new(ZDocument::new())),
            limits: limits.clone(),
            images: 0,
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
        let (doc, images) = run_guarded(limits, move || {
            let r = validate_and_repack(&data, Kind::Docx, Mode::Keep, &l)?;
            Ok((ZDocument::from_bytes(&r.bytes).map_err(map_err)?, r.images))
        })?;
        Ok(Self {
            doc: Arc::new(Mutex::new(doc)),
            limits: limits.clone(),
            images,
        })
    }

    fn lock(&self) -> Result<MutexGuard<'_, ZDocument>> {
        self.doc.try_lock().map_err(|e| match e {
            TryLockError::WouldBlock => OfficeError::Busy,
            TryLockError::Poisoned(_) => {
                OfficeError::Internal("document state poisoned by a panic".into())
            }
        })
    }

    fn guarded<R, F>(&self, f: F) -> Result<R>
    where
        R: Send + 'static,
        F: FnOnce(&mut ZDocument, &Limits) -> Result<R> + Send + 'static,
    {
        let (doc, l) = (self.doc.clone(), self.limits.clone());
        run_guarded(&self.limits, move || {
            let mut g = doc.try_lock().map_err(|e| match e {
                TryLockError::WouldBlock => OfficeError::Busy,
                TryLockError::Poisoned(_) => {
                    OfficeError::Internal("document state poisoned".into())
                }
            })?;
            f(&mut g, &l)
        })
    }

    /// Serialise to a validated, canonical docx.
    ///
    /// # Errors
    /// Any limit violation of the produced file.
    pub fn save(&self) -> Result<Vec<u8>> {
        self.guarded(|d, l| {
            let raw = d.to_bytes().map_err(map_err)?;
            let out = finish_output(&raw, Kind::Docx, l)?;
            if out.len() as u64 > l.max_input_bytes {
                return Err(limit(Limit::InputBytes, out.len() as u64));
            }
            Ok(out)
        })
    }

    /// Append a paragraph.
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn add_paragraph(&mut self, text: &str) -> Result<()> {
        self.lock()?.add_paragraph(text);
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
        self.lock()?
            .add_paragraph(text)
            .style(&format!("Heading{level}"));
        Ok(())
    }

    /// Replace the text of the `index`th paragraph.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] for a bad index.
    pub fn set_paragraph_text(&mut self, index: usize, text: &str) -> Result<()> {
        if self.lock()?.set_paragraph_text(index, text) {
            Ok(())
        } else {
            Err(OfficeError::NotFound(format!("content index {index}")))
        }
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
        let cells = (rows as u64).saturating_mul(cols as u64);
        if cells > self.limits.max_cells {
            return Err(limit(Limit::Cells, cells));
        }
        let mut d = self.lock()?;
        let idx = d.table_count();
        d.add_table(rows, cols);
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
        let mut d = self.lock()?;
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
        let d = self.lock()?;
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
        Ok(self.lock()?.replace_text(find, replacement))
    }

    /// The heading outline as `(level, text)`.
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn headings(&self) -> Result<Vec<(u32, String)>> {
        Ok(self.lock()?.headings())
    }

    /// Insert an inline PNG, JPEG or GIF (header-checked first).
    ///
    /// # Errors
    /// [`Limit::ImagePixels`], [`Limit::Images`], [`Limit::MediaPartBytes`],
    /// [`OfficeError::InvalidInput`].
    pub fn add_image(&mut self, data: &[u8], filename: &str, size: Rect) -> Result<()> {
        check_new_image(data, self.images, &self.limits)?;
        self.lock()?
            .add_picture(data, filename, Length::emu(size.w), Length::emu(size.h));
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
        spec.check(&self.limits)?;
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
        self.lock()?
            .add_chart(&chart, Length::emu(size.w), Length::emu(size.h));
        Ok(())
    }

    /// All text in document order, table cells one per line.
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn to_plain_text(&self) -> Result<String> {
        Ok(self.lock()?.to_plain_text())
    }

    /// The document as Markdown.
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn to_markdown(&self) -> Result<String> {
        self.guarded(|d, _| Ok(d.to_markdown()))
    }

    /// Export to PDF. Uses the bundled font set only (Carlito, Caladea,
    /// Liberation): embedded document fonts are emptied before the library
    /// runs, and `zavora-docx-layout` loads no system fonts.
    ///
    /// # Errors
    /// [`Limit::RenderPixels`].
    pub fn export_pdf(&self) -> Result<Vec<u8>> {
        self.guarded(|d, l| render_copy(d, l, 0.0)?.to_pdf().map_err(map_err))
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
        self.guarded(move |d, l| {
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
