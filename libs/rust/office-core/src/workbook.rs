//! `Workbook`: an xlsx (over `zavora-xlsx`). No render and no PDF exist in the
//! library; those calls return [`OfficeError::Unsupported`].

use std::fmt::Write as _;

use zavora_xlsx::{CellValue as ZCell, Error as ZError, Workbook as ZWorkbook};

use crate::error::{Limit, OfficeError, Result, limit};
use crate::guard::{Shared, run_guarded};
use crate::limits::Limits;
use crate::validate::{Kind, Mode, finish_output, validate_and_repack};

use crate::a1::{MAX_COL, MAX_ROW};

/// The most characters Excel keeps in one cell.
const MAX_CELL_TEXT_CHARS: usize = 32_767;

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
    /// How many cells the range covers.
    #[must_use]
    pub fn cells(&self) -> u64 {
        u64::from(self.last_row - self.first_row + 1)
            * u64::from(self.last_col - self.first_col + 1)
    }
}

/// One series of a spreadsheet chart.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SheetSeries {
    /// The series name (a literal), if any.
    pub name: Option<String>,
    /// The cell range that holds the values.
    pub values: String,
}

/// A chart drawn on a sheet from cell ranges of the workbook.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SheetChartSpec {
    /// The chart type.
    pub kind: crate::ChartKind,
    /// The title, if any.
    pub title: Option<String>,
    /// The cell range of the category labels, if any.
    pub categories: Option<String>,
    /// The series, 1 to 20.
    pub series: Vec<SheetSeries>,
    /// Whether to show a legend (at the bottom).
    pub legend: bool,
    /// Whether to show the value on each point.
    pub data_labels: bool,
    /// Row of the chart's top-left corner (0-based).
    pub anchor_row: u32,
    /// Column of the chart's top-left corner (0-based).
    pub anchor_col: u16,
    /// Width in pixels (100 to 4000).
    pub width_px: u32,
    /// Height in pixels (100 to 4000).
    pub height_px: u32,
}

/// An xlsx workbook.
pub struct Workbook {
    wb: Shared<ZWorkbook>,
    limits: Limits,
    /// Distinct cells holding a value: counted at open from the gate's pass
    /// over the sheets, then once per cell that goes from empty to written.
    cells: u64,
    inline_strings: bool,
    /// Cells cleared since open. zavora-xlsx keeps the parsed originals of an
    /// opened sheet and reads them back after `clear_cell`; this mask hides
    /// them. The saved file does not have them.
    cleared: std::collections::HashSet<(usize, u32, u16)>,
    /// Renames and deletes since open, for the defined names to follow at save.
    name_ops: Vec<crate::defined_names::NameOp>,
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
            wb: Shared::new(ZWorkbook::new(), limits),
            limits: limits.clone(),
            cells: 0,
            inline_strings: false,
            cleared: std::collections::HashSet::new(),
            name_ops: Vec::new(),
        }
    }

    /// Validate `bytes` (see [`crate::Limits`]) and open them for editing.
    /// Every sheet is parsed here, on the parse thread, so later reads and
    /// writes never trigger a parse on the caller's thread.
    ///
    /// # Errors
    /// [`OfficeError::LimitExceeded`] (including [`Limit::Cells`] for a file
    /// with more cells than `max_cells`), [`OfficeError::Malformed`],
    /// [`OfficeError::Unsupported`], [`OfficeError::Busy`],
    /// [`OfficeError::DeadlineExceeded`].
    pub fn open(bytes: &[u8], limits: &Limits) -> Result<Self> {
        crate::counters::bump(crate::counters::Op::Open);
        crate::validate::precheck_len(bytes.len(), limits)?;
        let (data, l) = (bytes.to_vec(), limits.clone());
        let (wb, cells, inline_strings) = run_guarded(limits, move || {
            let r = validate_and_repack(&data, Kind::Xlsx, Mode::Keep, &l)?;
            let mut wb = ZWorkbook::open_from_buffer(&r.bytes).map_err(map_err)?;
            if wb.sheet_count() as u64 > l.max_sheets {
                return Err(limit(Limit::Sheets, wb.sheet_count() as u64));
            }
            // zavora-xlsx parses a sheet on first access: do all of it now.
            for i in 0..wb.sheet_count() {
                wb.worksheet(i).map_err(map_err)?;
            }
            Ok((wb, r.cells, r.inline_strings))
        })?;
        Ok(Self {
            wb: Shared::new(wb, limits),
            limits: limits.clone(),
            cells,
            inline_strings,
            cleared: std::collections::HashSet::new(),
            name_ops: Vec::new(),
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

    /// Serialise to a validated, canonical xlsx.
    ///
    /// # Errors
    /// Any limit violation of the produced file.
    pub fn save(&self) -> Result<Vec<u8>> {
        crate::counters::bump(crate::counters::Op::Save);
        let ops = self.name_ops.clone();
        self.wb.guarded(move |w, l| {
            let mut raw = w.save_to_buffer().map_err(map_err)?;
            if !ops.is_empty() {
                raw = crate::defined_names::apply(&raw, &ops, l)?;
            }
            finish_output(&raw, Kind::Xlsx, l)
        })
    }

    /// Sheet names, in order.
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn sheet_names(&self) -> Result<Vec<String>> {
        Ok(self
            .wb
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
        let mut w = self.wb.lock()?;
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
        self.wb.lock()?.remove_worksheet(index).map_err(map_err)?;
        self.name_ops
            .push(crate::defined_names::NameOp::Delete(index));
        // Cleared-cell marks follow the sheets that moved down.
        self.cleared = self
            .cleared
            .drain()
            .filter(|&(sh, _, _)| sh != index)
            .map(|(sh, r, c)| (if sh > index { sh - 1 } else { sh }, r, c))
            .collect();
        Ok(())
    }

    /// Rename a sheet.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] or [`OfficeError::InvalidInput`].
    pub fn rename_sheet(&mut self, index: usize, name: &str) -> Result<()> {
        let old = self.sheet_names()?.get(index).cloned();
        self.wb
            .lock()?
            .rename_worksheet(index, name)
            .map_err(map_err)?;
        if let Some(old) = old {
            self.name_ops.push(crate::defined_names::NameOp::Rename {
                index,
                old,
                new: name.to_owned(),
            });
        }
        Ok(())
    }

    /// Write one cell (0-based row and column). A cell counts against
    /// `max_cells` once, when it goes from empty to written; overwriting does
    /// not, and a failed write does not.
    ///
    /// # Errors
    /// [`Limit::Cells`] past `max_cells`, [`OfficeError::InvalidInput`]
    /// outside the sheet grid, [`OfficeError::Unsupported`] for `Error` and
    /// `DateTime` values (zavora-xlsx 0.1.2 has no way to write either).
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
        let mut w = self.wb.lock()?;
        let ws = w.worksheet(sheet).map_err(map_err)?;
        let was_empty = self.cleared.contains(&(sheet, row, col))
            || matches!(ws.read_cell(row, col), ZCell::Empty);
        let grows = was_empty && !matches!(value, CellValue::Empty);
        if grows && self.cells + 1 > self.limits.max_cells {
            return Err(limit(Limit::Cells, self.cells + 1));
        }
        match value {
            CellValue::Empty => {
                // Clearing removes the cell; it never writes a blank.
                ws.clear_cell(row, col);
                self.cleared.insert((sheet, row, col));
                if !was_empty {
                    self.cells = self.cells.saturating_sub(1);
                }
                return Ok(());
            }
            CellValue::Text(s) => ws.write(row, col, s.as_str()).map(|_| ()),
            CellValue::Number(n) => ws.write(row, col, *n).map(|_| ()),
            CellValue::Bool(b) => ws.write(row, col, *b).map(|_| ()),
            CellValue::Formula { formula, .. } => ws.write_formula(row, col, formula).map(|_| ()),
            CellValue::Error(_) => {
                return Err(OfficeError::Unsupported(
                    "zavora-xlsx 0.1.2 cannot write an error cell (t=\"e\")".into(),
                ));
            }
            _ => {
                return Err(OfficeError::Unsupported(
                    "this cell value kind cannot be written".into(),
                ));
            }
        }
        .map_err(map_err)?;
        self.cleared.remove(&(sheet, row, col));
        if grows {
            self.cells += 1;
        }
        Ok(())
    }

    /// Write many cells in one call, all or none: every value is checked
    /// (grid, kind, text length, finite numbers) and the cell cap is checked
    /// against the DISTINCT cells that would go from empty to written, before
    /// the first cell is touched. A cell named twice is written once, with its
    /// last value. Returns how many cells went from empty to written.
    ///
    /// # Errors
    /// [`Limit::Cells`], [`OfficeError::InvalidInput`],
    /// [`OfficeError::Unsupported`], [`OfficeError::NotFound`] for a bad sheet.
    pub fn write_cells(&mut self, sheet: usize, cells: &[(u32, u16, CellValue)]) -> Result<u64> {
        let mut last: std::collections::HashMap<(u32, u16), &CellValue> =
            std::collections::HashMap::with_capacity(cells.len().min(1 << 16));
        for (row, col, value) in cells {
            check_writable(*row, *col, value)?;
            last.insert((*row, *col), value);
        }
        let grows = {
            let mut w = self.wb.lock()?;
            let ws = w.worksheet(sheet).map_err(map_err)?;
            let mut n = 0u64;
            for (&(row, col), value) in &last {
                let was_empty = self.cleared.contains(&(sheet, row, col))
                    || matches!(ws.read_cell(row, col), ZCell::Empty);
                if was_empty && !matches!(value, CellValue::Empty) {
                    n += 1;
                }
            }
            n
        };
        let total = self.cells.saturating_add(grows);
        if total > self.limits.max_cells {
            return Err(limit(Limit::Cells, total));
        }
        // Row-major, so the result does not depend on hash order.
        let mut ordered: Vec<(&(u32, u16), &&CellValue)> = last.iter().collect();
        ordered.sort_by_key(|(k, _)| **k);
        for (&(row, col), value) in ordered {
            self.write_cell(sheet, row, col, value)?;
        }
        Ok(grows)
    }

    /// Clear every cell of `range` that holds a value; returns how many did.
    ///
    /// # Errors
    /// [`Limit::Cells`] when the part of the range inside the sheet's used
    /// range is larger than `max_cells`; [`OfficeError::NotFound`] for a bad
    /// sheet.
    pub fn clear_range(&mut self, sheet: usize, range: CellRange) -> Result<u64> {
        let Some(used) = self.used_range(sheet)? else {
            return Ok(0);
        };
        let first_row = range.first_row.max(used.first_row);
        let first_col = range.first_col.max(used.first_col);
        let last_row = range.last_row.min(used.last_row);
        let last_col = range.last_col.min(used.last_col);
        if first_row > last_row || first_col > last_col {
            return Ok(0);
        }
        let inside = CellRange {
            first_row,
            first_col,
            last_row,
            last_col,
        };
        if inside.cells() > self.limits.max_cells {
            return Err(limit(Limit::Cells, inside.cells()));
        }
        let mut cleared = 0u64;
        for row in first_row..=last_row {
            for col in first_col..=last_col {
                let held = {
                    let mut w = self.wb.lock()?;
                    let ws = w.worksheet(sheet).map_err(map_err)?;
                    !self.cleared.contains(&(sheet, row, col))
                        && !matches!(ws.read_cell(row, col), ZCell::Empty)
                };
                if held {
                    self.write_cell(sheet, row, col, &CellValue::Empty)?;
                    cleared += 1;
                }
            }
        }
        Ok(cleared)
    }

    /// Add a chart to a sheet. Series and categories are cell ranges of the
    /// workbook (`A2:A9`, or `Sheet2!B2:B9` for another sheet); they are
    /// validated against the sheet grid and written in the absolute form the
    /// file format wants. The chart counts its points against `max_cells`.
    ///
    /// # Errors
    /// [`OfficeError::InvalidInput`] for a range that is not one,
    /// [`OfficeError::Unsupported`] for stacked bars and columns,
    /// [`Limit::Cells`], [`OfficeError::NotFound`] for a bad sheet.
    pub fn add_chart(&mut self, sheet: usize, spec: &SheetChartSpec) -> Result<()> {
        use zavora_xlsx::features::chart::{Chart, ChartType, LegendPosition};
        if spec.series.is_empty() || spec.series.len() > 20 {
            return Err(OfficeError::InvalidInput(
                "a chart needs 1 to 20 series".into(),
            ));
        }
        let kind = match spec.kind {
            crate::ChartKind::ClusteredBar => ChartType::Bar,
            crate::ChartKind::ClusteredColumn => ChartType::Column,
            crate::ChartKind::Line => ChartType::Line,
            crate::ChartKind::Pie => ChartType::Pie,
            crate::ChartKind::Area => ChartType::Area,
            crate::ChartKind::Scatter => ChartType::Scatter,
            crate::ChartKind::Doughnut => ChartType::Doughnut,
            crate::ChartKind::StackedBar | crate::ChartKind::StackedColumn => {
                return Err(OfficeError::Unsupported(
                    "xlsx charts support bar, column, line, pie, doughnut, area and scatter".into(),
                ));
            }
        };
        let names = self.sheet_names()?;
        let own = names
            .get(sheet)
            .ok_or_else(|| OfficeError::NotFound(format!("sheet {sheet}")))?
            .clone();
        let mut points = 0u64;
        let categories = match &spec.categories {
            Some(r) => Some(absolute_ref(r, &own, &names, &mut points)?),
            None => None,
        };
        let mut series = Vec::with_capacity(spec.series.len());
        for s in &spec.series {
            let mut counted = 0u64;
            let values = absolute_ref(&s.values, &own, &names, &mut counted)?;
            points = points.saturating_add(counted);
            series.push((s.name.clone(), values));
        }
        let total = self.cells.saturating_add(points);
        if total > self.limits.max_cells {
            return Err(limit(Limit::Cells, total));
        }
        let mut chart = Chart::new(kind);
        if let Some(t) = &spec.title {
            chart.set_title(t);
        }
        chart.set_legend_position(if spec.legend {
            LegendPosition::Bottom
        } else {
            LegendPosition::None
        });
        chart.set_width(spec.width_px.clamp(100, 4000));
        chart.set_height(spec.height_px.clamp(100, 4000));
        for (name, values) in &series {
            let s = chart.add_series();
            s.set_values(values);
            if let Some(c) = &categories {
                s.set_categories(c);
            }
            if let Some(n) = name {
                s.set_name(n);
            }
            if spec.data_labels {
                s.set_data_labels(true);
            }
        }
        let mut w = self.wb.lock()?;
        let ws = w.worksheet(sheet).map_err(map_err)?;
        ws.insert_chart(spec.anchor_row, spec.anchor_col, &chart)
            .map_err(map_err)?;
        drop(w);
        self.cells = total;
        Ok(())
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
        let mut w = self.wb.lock()?;
        let ws = w.worksheet(sheet).map_err(map_err)?;
        if self.cleared.contains(&(sheet, row, col)) {
            return Ok(CellValue::Empty);
        }
        Ok(ws.read_cell(row, col).into())
    }

    /// The used range of a sheet, if any cell exists.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] for a bad sheet.
    pub fn used_range(&self, sheet: usize) -> Result<Option<CellRange>> {
        let mut w = self.wb.lock()?;
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
        let mut w = self.wb.lock()?;
        let ws = w.worksheet(sheet).map_err(map_err)?;
        Ok((range.first_row..=range.last_row)
            .map(|r| {
                (range.first_col..=range.last_col)
                    .map(|c| {
                        if self.cleared.contains(&(sheet, r, c)) {
                            CellValue::Empty
                        } else {
                            ws.read_cell(r, c).into()
                        }
                    })
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

/// What a chart series or category range is, in the file's absolute form
/// (`'My Sheet'!$B$2:$B$9`), with the number of cells it covers added to
/// `points`.
fn absolute_ref(text: &str, own: &str, sheets: &[String], points: &mut u64) -> Result<String> {
    let (sheet, range) = match text.rsplit_once('!') {
        Some((s, r)) => (s.trim().trim_matches('\'').replace("''", "'"), r),
        None => (own.to_owned(), text),
    };
    if !sheets.contains(&sheet) {
        return Err(OfficeError::InvalidInput(format!(
            "the chart range `{text}` names a sheet that does not exist"
        )));
    }
    let r = crate::a1::parse_range(range).ok_or_else(|| {
        OfficeError::InvalidInput(format!("`{text}` is not a cell range such as A2:A9"))
    })?;
    *points = points.saturating_add(r.cells());
    let a = crate::a1::cell_ref(r.first_row, r.first_col);
    let b = crate::a1::cell_ref(r.last_row, r.last_col);
    let abs = |c: &str| {
        let split = c.find(|ch: char| ch.is_ascii_digit()).unwrap_or(c.len());
        format!("${}${}", &c[..split], &c[split..])
    };
    Ok(format!(
        "'{}'!{}:{}",
        sheet.replace('\'', "''"),
        abs(&a),
        abs(&b)
    ))
}

fn check_writable(row: u32, col: u16, value: &CellValue) -> Result<()> {
    if row > MAX_ROW || col > MAX_COL {
        return Err(OfficeError::InvalidInput(format!(
            "cell {} is outside the sheet grid",
            crate::a1::cell_ref(row.min(MAX_ROW), col.min(MAX_COL))
        )));
    }
    match value {
        CellValue::Empty | CellValue::Bool(_) => Ok(()),
        CellValue::Text(s) if s.chars().count() <= MAX_CELL_TEXT_CHARS => Ok(()),
        CellValue::Text(_) => Err(OfficeError::InvalidInput(format!(
            "the text of cell {} is over {MAX_CELL_TEXT_CHARS} characters",
            crate::a1::cell_ref(row, col)
        ))),
        CellValue::Number(n) if n.is_finite() => Ok(()),
        CellValue::Number(_) => Err(OfficeError::InvalidInput(format!(
            "cell {} is not a finite number",
            crate::a1::cell_ref(row, col)
        ))),
        CellValue::Formula { formula, .. } if !formula.trim().is_empty() => Ok(()),
        CellValue::Formula { .. } => Err(OfficeError::InvalidInput(format!(
            "the formula of cell {} is empty",
            crate::a1::cell_ref(row, col)
        ))),
        CellValue::Error(_) => Err(OfficeError::Unsupported(
            "zavora-xlsx 0.1.2 cannot write an error cell (t=\"e\")".into(),
        )),
        _ => Err(OfficeError::Unsupported(
            "this cell value kind cannot be written".into(),
        )),
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
