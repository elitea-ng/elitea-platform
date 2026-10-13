//! `Workbook`: an xlsx (over `zavora-xlsx`). No render and no PDF exist in the
//! library; those calls return [`OfficeError::Unsupported`].

use std::fmt::Write as _;
use std::sync::{Arc, Mutex, MutexGuard, TryLockError};

use zavora_xlsx::{CellValue as ZCell, Error as ZError, Workbook as ZWorkbook};

use crate::error::{Limit, OfficeError, Result, limit};
use crate::guard::run_guarded;
use crate::limits::Limits;
use crate::validate::{Kind, Mode, finish_output, validate_and_repack};

const MAX_ROW: u32 = 1_048_575;
const MAX_COL: u16 = 16_383;

fn map_err(e: ZError) -> OfficeError {
    match e {
        ZError::SheetNotFound(m) => OfficeError::NotFound(format!("sheet {m}")),
        ZError::InvalidCellRef(m) | ZError::InvalidRange(m) | ZError::InvalidData(m) => {
            OfficeError::InvalidInput(m)
        }
        ZError::Password => OfficeError::Unsupported("password-protected workbook".into()),
        ZError::UnsupportedFormat(m) => OfficeError::Unsupported(m),
        ZError::ReadOnly => OfficeError::Unsupported("workbook is read-only".into()),
        ZError::Zip(e) => OfficeError::Malformed(e.to_string()),
        ZError::Xml(e) => OfficeError::Malformed(e.to_string()),
        other => OfficeError::Malformed(other.to_string()),
    }
}

/// A spreadsheet cell value.
#[derive(Debug, Clone, PartialEq)]
#[non_exhaustive]
pub enum CellValue {
    /// No value.
    Empty,
    /// Text (rich text is flattened).
    Text(String),
    /// A number.
    Number(f64),
    /// A boolean.
    Bool(bool),
    /// A date or time as an Excel serial number.
    DateTime(f64),
    /// An error value such as `#DIV/0!`.
    Error(String),
    /// A formula with its cached value when known. Formulas are stored, not
    /// evaluated, by [`Workbook::write_cell`].
    Formula {
        /// The formula text without the leading `=`.
        formula: String,
        /// The value last computed by the producing application.
        cached: Option<Box<CellValue>>,
    },
}

impl From<ZCell> for CellValue {
    fn from(c: ZCell) -> Self {
        match c {
            ZCell::Empty => CellValue::Empty,
            ZCell::String(s) => CellValue::Text(s),
            ZCell::Number(n) => CellValue::Number(n),
            ZCell::Bool(b) => CellValue::Bool(b),
            ZCell::DateTime(d) => CellValue::DateTime(d.serial()),
            ZCell::Error(e) => CellValue::Error(e),
            ZCell::Formula {
                formula,
                cached_value,
            } => {
                let v = CellValue::from(*cached_value);
                CellValue::Formula {
                    formula,
                    cached: if v == CellValue::Empty {
                        None
                    } else {
                        Some(Box::new(v))
                    },
                }
            }
            ZCell::RichText(r) => CellValue::Text(r.runs.iter().map(|r| r.text.as_str()).collect()),
        }
    }
}

/// The used range of a sheet, inclusive, 0-based.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct CellRange {
    /// First row.
    pub first_row: u32,
    /// First column.
    pub first_col: u16,
    /// Last row.
    pub last_row: u32,
    /// Last column.
    pub last_col: u16,
}

impl CellRange {
    fn cells(&self) -> u64 {
        u64::from(self.last_row - self.first_row + 1)
            * u64::from(self.last_col - self.first_col + 1)
    }
}

/// An xlsx workbook.
pub struct Workbook {
    wb: Arc<Mutex<ZWorkbook>>,
    limits: Limits,
    cells_written: u64,
    inline_strings: bool,
}

impl std::fmt::Debug for Workbook {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Workbook").finish_non_exhaustive()
    }
}

impl Workbook {
    /// A workbook with one empty sheet named `Sheet1`.
    #[must_use]
    pub fn new(limits: &Limits) -> Self {
        Self {
            wb: Arc::new(Mutex::new(ZWorkbook::new())),
            limits: limits.clone(),
            cells_written: 0,
            inline_strings: false,
        }
    }

    /// Validate `bytes` (see [`crate::Limits`]) and open them for editing.
    ///
    /// # Errors
    /// [`OfficeError::LimitExceeded`], [`OfficeError::Malformed`],
    /// [`OfficeError::Unsupported`], [`OfficeError::Busy`],
    /// [`OfficeError::DeadlineExceeded`].
    pub fn open(bytes: &[u8], limits: &Limits) -> Result<Self> {
        crate::validate::precheck_len(bytes.len(), limits)?;
        let (data, l) = (bytes.to_vec(), limits.clone());
        let (wb, inline_strings) = run_guarded(limits, move || {
            let r = validate_and_repack(&data, Kind::Xlsx, Mode::Keep, &l)?;
            let wb = ZWorkbook::open_from_buffer(&r.bytes).map_err(map_err)?;
            if wb.sheet_count() as u64 > l.max_sheets {
                return Err(limit(Limit::Sheets, wb.sheet_count() as u64));
            }
            Ok((wb, r.inline_strings))
        })?;
        Ok(Self {
            wb: Arc::new(Mutex::new(wb)),
            limits: limits.clone(),
            cells_written: 0,
            inline_strings,
        })
    }

    /// True when the opened file stores some text as inline strings
    /// (`t="inlineStr"`, as openpyxl writes). zavora-xlsx 0.1.2 reads those
    /// cells back as empty text, so [`Workbook::read_cell`] and
    /// [`Workbook::sheet_to_markdown`] are incomplete for such a file. Callers
    /// should say so rather than present the blanks as content.
    #[must_use]
    pub fn reads_inline_strings_as_empty(&self) -> bool {
        self.inline_strings
    }

    fn lock(&self) -> Result<MutexGuard<'_, ZWorkbook>> {
        self.wb.try_lock().map_err(|e| match e {
            TryLockError::WouldBlock => OfficeError::Busy,
            TryLockError::Poisoned(_) => {
                OfficeError::Internal("workbook state poisoned by a panic".into())
            }
        })
    }

    /// Serialise to a validated, canonical xlsx.
    ///
    /// # Errors
    /// Any limit violation of the produced file.
    pub fn save(&self) -> Result<Vec<u8>> {
        let (wb, l) = (self.wb.clone(), self.limits.clone());
        run_guarded(&self.limits, move || {
            let mut g = wb.try_lock().map_err(|_| OfficeError::Busy)?;
            let raw = g.save_to_buffer().map_err(map_err)?;
            let out = finish_output(&raw, Kind::Xlsx, &l)?;
            if out.len() as u64 > l.max_input_bytes {
                return Err(limit(Limit::InputBytes, out.len() as u64));
            }
            Ok(out)
        })
    }

    /// Sheet names, in order.
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn sheet_names(&self) -> Result<Vec<String>> {
        Ok(self
            .lock()?
            .sheet_names()
            .into_iter()
            .map(str::to_owned)
            .collect())
    }

    /// Add a sheet; returns its index.
    ///
    /// # Errors
    /// [`Limit::Sheets`] past `max_sheets`; [`OfficeError::InvalidInput`] for a
    /// bad or duplicate name.
    pub fn add_sheet(&mut self, name: &str) -> Result<usize> {
        let mut w = self.lock()?;
        let n = w.sheet_count() as u64 + 1;
        if n > self.limits.max_sheets {
            return Err(limit(Limit::Sheets, n));
        }
        w.add_worksheet_with_name(name).map_err(map_err)?;
        Ok(w.sheet_count() - 1)
    }

    /// Remove a sheet.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] for a bad index.
    pub fn remove_sheet(&mut self, index: usize) -> Result<()> {
        self.lock()?.remove_worksheet(index).map_err(map_err)
    }

    /// Rename a sheet.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] or [`OfficeError::InvalidInput`].
    pub fn rename_sheet(&mut self, index: usize, name: &str) -> Result<()> {
        self.lock()?.rename_worksheet(index, name).map_err(map_err)
    }

    /// Write one cell (0-based row and column).
    ///
    /// # Errors
    /// [`Limit::Cells`] past `max_cells` writes, [`OfficeError::InvalidInput`]
    /// outside the sheet grid, [`OfficeError::Unsupported`] for `DateTime`
    /// (write a number with a date format once styles are exposed).
    pub fn write_cell(
        &mut self,
        sheet: usize,
        row: u32,
        col: u16,
        value: &CellValue,
    ) -> Result<()> {
        if row > MAX_ROW || col > MAX_COL {
            return Err(OfficeError::InvalidInput(format!(
                "cell ({row},{col}) is outside the sheet grid"
            )));
        }
        self.cells_written += 1;
        if self.cells_written > self.limits.max_cells {
            return Err(limit(Limit::Cells, self.cells_written));
        }
        let mut w = self.lock()?;
        let ws = w.worksheet(sheet).map_err(map_err)?;
        match value {
            CellValue::Empty => ws
                .write_blank(row, col, &zavora_xlsx::Format::new())
                .map(|_| ()),
            CellValue::Text(s) => ws.write(row, col, s.as_str()).map(|_| ()),
            CellValue::Number(n) => ws.write(row, col, *n).map(|_| ()),
            CellValue::Bool(b) => ws.write(row, col, *b).map(|_| ()),
            CellValue::Formula { formula, .. } => ws.write_formula(row, col, formula).map(|_| ()),
            CellValue::Error(e) => ws.write(row, col, e.as_str()).map(|_| ()),
            _ => {
                return Err(OfficeError::Unsupported(
                    "this cell value kind cannot be written".into(),
                ));
            }
        }
        .map_err(map_err)
    }

    /// Write a row of cells starting at `col`.
    ///
    /// # Errors
    /// As [`Workbook::write_cell`].
    pub fn write_row(
        &mut self,
        sheet: usize,
        row: u32,
        col: u16,
        values: &[CellValue],
    ) -> Result<()> {
        for (i, v) in values.iter().enumerate() {
            let c = u16::try_from(usize::from(col) + i)
                .map_err(|_| OfficeError::InvalidInput("row runs past the last column".into()))?;
            self.write_cell(sheet, row, c, v)?;
        }
        Ok(())
    }

    /// Read one cell.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] for a bad sheet.
    pub fn read_cell(&self, sheet: usize, row: u32, col: u16) -> Result<CellValue> {
        let mut w = self.lock()?;
        Ok(w.worksheet(sheet)
            .map_err(map_err)?
            .read_cell(row, col)
            .into())
    }

    /// The used range of a sheet, if any cell exists.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] for a bad sheet.
    pub fn used_range(&self, sheet: usize) -> Result<Option<CellRange>> {
        let mut w = self.lock()?;
        Ok(w.worksheet(sheet).map_err(map_err)?.used_range().map(
            |(first_row, first_col, last_row, last_col)| CellRange {
                first_row,
                first_col,
                last_row,
                last_col,
            },
        ))
    }

    /// Read a rectangle of cells.
    ///
    /// # Errors
    /// [`Limit::Cells`] when the rectangle is larger than `max_cells`: a
    /// sparse sheet can declare `A1:XFD1048576`, and walking it is refused.
    pub fn read_range(&self, sheet: usize, range: CellRange) -> Result<Vec<Vec<CellValue>>> {
        if range.last_row < range.first_row || range.last_col < range.first_col {
            return Err(OfficeError::InvalidInput("empty range".into()));
        }
        let n = range.cells();
        if n > self.limits.max_cells {
            return Err(limit(Limit::Cells, n));
        }
        let mut w = self.lock()?;
        let ws = w.worksheet(sheet).map_err(map_err)?;
        Ok((range.first_row..=range.last_row)
            .map(|r| {
                (range.first_col..=range.last_col)
                    .map(|c| ws.read_cell(r, c).into())
                    .collect()
            })
            .collect())
    }

    /// A Markdown table of a sheet's used range (empty string for an empty
    /// sheet). Formulas show their cached value, or `=formula` without one.
    ///
    /// # Errors
    /// [`Limit::Cells`] as in [`Workbook::read_range`].
    pub fn sheet_to_markdown(&self, sheet: usize) -> Result<String> {
        let Some(range) = self.used_range(sheet)? else {
            return Ok(String::new());
        };
        let rows = self.read_range(sheet, range)?;
        let mut out = String::new();
        for (i, row) in rows.iter().enumerate() {
            out.push('|');
            for c in row {
                let _ = write!(
                    out,
                    " {} |",
                    cell_text(c).replace('|', "\\|").replace('\n', " ")
                );
            }
            out.push('\n');
            if i == 0 {
                out.push('|');
                for _ in row {
                    out.push_str(" --- |");
                }
                out.push('\n');
            }
        }
        Ok(out)
    }

    /// Render a sheet. The library has no spreadsheet renderer.
    ///
    /// # Errors
    /// Always [`OfficeError::Unsupported`]; use [`Workbook::sheet_to_markdown`].
    pub fn render_preview(&self, _sheet: usize) -> Result<Vec<u8>> {
        Err(OfficeError::Unsupported(
            "xlsx has no renderer; use sheet_to_markdown".into(),
        ))
    }

    /// Export to PDF.
    ///
    /// # Errors
    /// Always [`OfficeError::Unsupported`]: zavora-xlsx has no PDF export.
    pub fn export_pdf(&self) -> Result<Vec<u8>> {
        Err(OfficeError::Unsupported("xlsx has no PDF export".into()))
    }
}

fn cell_text(c: &CellValue) -> String {
    match c {
        CellValue::Empty => String::new(),
        CellValue::Text(s) | CellValue::Error(s) => s.clone(),
        CellValue::Number(n) | CellValue::DateTime(n) => n.to_string(),
        CellValue::Bool(b) => b.to_string(),
        CellValue::Formula { formula, cached } => cached
            .as_deref()
            .map_or_else(|| format!("={formula}"), cell_text),
    }
}
