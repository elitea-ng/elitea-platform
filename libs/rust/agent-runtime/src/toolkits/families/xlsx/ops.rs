//! The blocking workbook operations of the `xlsx` family: everything that
//! touches a [`Workbook`]. Each function is synchronous and returns the
//! sentence the model reads on failure, so `tools.rs` runs them on a blocking
//! thread and never maps an `OfficeError` itself. No path appears: bytes in,
//! bytes out (ADR-0032 decision 2).
//!
//! An edit is planned, applied once and checked once:
//!
//! 1. the workbook is opened (the call's one parse) and the call is *planned*
//!    against its sheet names: every argument, address, value and name is
//!    checked, and what the saved file must hold is worked out, before
//!    anything is edited;
//! 2. the file's fact sheet is read from the package (no second library
//!    parse) for the cells the call is aimed at, the workbook is edited and
//!    serialised (the call's one serialisation);
//! 3. the saved bytes' fact sheet is read back and compared with the plan *at
//!    the cells, sheets and charts the edit was aimed at*, and with the file as
//!    it was for everything else (cell counts per sheet, charts, package part
//!    kinds). A save that does not satisfy the plan is refused and nothing is
//!    written.
//!
//! Reads go straight to the package, so they see inline strings, which the
//! workbook library reads back as empty; an edit refuses a workbook that has
//! them, because saving it would erase that text.

use std::collections::BTreeMap;

use elitea_office_core::{
    CellRange, CellValue, ChartKind, Limits, OfficeError, SheetChartSpec, SheetSeries, Workbook,
    XlsxFacts, XlsxQuery, cell_ref, column_name, parse_cell_ref, parse_range, range_ref,
};
use std::fmt::Write as _;

use serde_json::{Map, Value, json};

use super::super::office_common::OfficeKind;
pub(super) use super::super::office_common::{Sentence, opt_bool, opt_str, opt_uint, req_str};
use super::schema::{CHART_TYPES, MAX_READ_CELLS, MAX_WRITE_CELLS, XlsxTool};

/// What this family's sentences call the file.
pub(super) const KIND: OfficeKind = OfficeKind {
    family: "xlsx",
    label: "Excel (.xlsx)",
    noun: "workbook",
    accepts: "an .xlsx",
};

/// An [`OfficeError`] as a sentence the model can act on.
pub(super) fn office_sentence(error: &OfficeError) -> Sentence {
    super::super::office_common::office_sentence(error, &KIND)
}

#[allow(clippy::needless_pass_by_value, reason = "used as a map_err function")]
fn oe(error: OfficeError) -> Sentence {
    office_sentence(&error)
}

const INLINE_STRINGS: &str = "This workbook stores some text as inline strings, which the spreadsheet library cannot read back, so saving an edit would erase that text. Nothing was saved. Open the file in a spreadsheet application and save it once, then try again.";

/// The fact sheet of a package, read without the workbook library.
pub(super) fn facts(bytes: &[u8], limits: &Limits, query: &Query) -> Result<XlsxFacts, Sentence> {
    elitea_office_core::facts_of_xlsx_package(
        bytes,
        limits,
        &XlsxQuery {
            cells: &query.cells,
            ranges: &query.ranges,
            sheet_mention: query.mention.as_deref(),
        },
    )
    .map_err(oe)
}

/// What a call needs read from the package.
#[derive(Default)]
pub(super) struct Query {
    cells: Vec<(usize, u32, u16)>,
    ranges: Vec<(usize, CellRange)>,
    mention: Option<String>,
}

// ---------------------------------------------------------------------------
// Addressing
// ---------------------------------------------------------------------------

/// The sheet a call names: by name (exact, then ignoring case), by number
/// from 1, or the first when none is given.
pub(super) fn sheet_arg(args: &Value, names: &[String]) -> Result<usize, Sentence> {
    let listing = || {
        names
            .iter()
            .enumerate()
            .map(|(i, n)| format!("{} `{n}`", i + 1))
            .collect::<Vec<_>>()
            .join(", ")
    };
    match args.get("sheet") {
        None | Some(Value::Null) => Ok(0),
        Some(Value::Number(n)) => {
            let n = n.as_u64().unwrap_or(0);
            if n == 0 || n > names.len() as u64 {
                return Err(format!(
                    "Sheet {n} does not exist. The sheets are: {}.",
                    listing()
                ));
            }
            Ok(usize::try_from(n - 1).unwrap_or(0))
        }
        Some(Value::String(name)) => {
            let name = name.trim();
            if let Some(i) = names.iter().position(|n| n == name) {
                return Ok(i);
            }
            let folded: Vec<usize> = names
                .iter()
                .enumerate()
                .filter(|(_, n)| n.to_lowercase() == name.to_lowercase())
                .map(|(i, _)| i)
                .collect();
            if let [only] = folded[..] {
                return Ok(only);
            }
            if let Ok(n) = name.parse::<usize>()
                && (1..=names.len()).contains(&n)
            {
                return Ok(n - 1);
            }
            Err(format!(
                "There is no sheet named `{name}`. The sheets are: {}.",
                listing()
            ))
        }
        Some(_) => Err("`sheet` is a sheet name or a number from 1.".to_owned()),
    }
}

fn cell_arg(text: &str, what: &str) -> Result<(u32, u16), Sentence> {
    parse_cell_ref(text).ok_or_else(|| {
        format!("{what} `{text}` is not a cell address such as B2 (columns A to XFD, rows 1 to 1048576).")
    })
}

fn range_arg(text: &str) -> Result<CellRange, Sentence> {
    parse_range(text).ok_or_else(|| {
        format!("`{text}` is not a range such as A1:D20 (columns A to XFD, rows 1 to 1048576).")
    })
}

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

/// What a call will do and what the saved file must then hold.
pub(crate) enum Plan {
    Write {
        sheet: usize,
        /// Distinct cells, row-major, the last value of each.
        cells: Vec<(u32, u16, CellValue)>,
    },
    Clear {
        sheet: usize,
        range: CellRange,
    },
    AddSheet {
        name: String,
    },
    Rename {
        sheet: usize,
        name: String,
    },
    Delete {
        sheet: usize,
    },
    Chart {
        sheet: usize,
        spec: SheetChartSpec,
        anchor: Option<(u32, u16)>,
        /// The chart type name the file must report.
        kind: &'static str,
    },
}

/// The values of a call as cells: `cells` (a cell with a value or a formula)
/// and/or `values` from `start_cell`. A cell named twice keeps its last value.
fn write_cells_arg(args: &Value) -> Result<Vec<(u32, u16, CellValue)>, Sentence> {
    let mut all: Vec<(u32, u16, CellValue)> = Vec::new();
    if let Some(list) = args.get("cells") {
        let list = list
            .as_array()
            .ok_or_else(|| "`cells` is a list of {cell, value} or {cell, formula}.".to_owned())?;
        for item in list {
            let cell = item
                .get("cell")
                .and_then(Value::as_str)
                .ok_or_else(|| "Every entry of `cells` needs a `cell` such as B2.".to_owned())?;
            let (row, col) = cell_arg(cell, "The cell")?;
            let value = match (item.get("value"), item.get("formula")) {
                (Some(_), Some(_)) => {
                    return Err(format!(
                        "Cell {cell} has both a value and a formula: give one."
                    ));
                }
                (None, Some(f)) => formula_value(f, cell)?,
                (Some(v), None) => cell_value(v, cell)?,
                (None, None) => {
                    return Err(format!(
                        "Cell {cell} has neither a value nor a formula (use \"value\": null to clear it)."
                    ));
                }
            };
            all.push((row, col, value));
        }
    }
    if let Some(rows) = args.get("values") {
        let start = args
            .get("start_cell")
            .and_then(Value::as_str)
            .ok_or_else(|| "`values` needs a `start_cell` such as A1.".to_owned())?;
        let (top, left) = cell_arg(start, "`start_cell`")?;
        let rows = rows
            .as_array()
            .ok_or_else(|| "`values` is a list of rows, each a list of values.".to_owned())?;
        for (r, row) in rows.iter().enumerate() {
            let row = row
                .as_array()
                .ok_or_else(|| "Every row of `values` is a list of values.".to_owned())?;
            for (c, v) in row.iter().enumerate() {
                let (rr, cc) = (
                    u32::try_from(r).ok().and_then(|r| top.checked_add(r)),
                    u16::try_from(c).ok().and_then(|c| left.checked_add(c)),
                );
                let (Some(rr), Some(cc)) = (rr, cc) else {
                    return Err("`values` runs past the edge of the sheet.".to_owned());
                };
                if rr > 1_048_575 || cc > 16_383 {
                    return Err("`values` runs past the edge of the sheet.".to_owned());
                }
                all.push((rr, cc, cell_value(v, &cell_ref(rr, cc))?));
            }
        }
    } else if args.get("start_cell").is_some() {
        return Err("`start_cell` needs `values`.".to_owned());
    }
    if all.is_empty() {
        return Err("Give `cells` and/or `start_cell` with `values`.".to_owned());
    }
    if all.len() > MAX_WRITE_CELLS {
        return Err(format!(
            "At most {MAX_WRITE_CELLS} cells can be written in one call; split it."
        ));
    }
    // The last value of each cell, row-major.
    let mut last: BTreeMap<(u32, u16), CellValue> = BTreeMap::new();
    for (r, c, v) in all {
        last.insert((r, c), v);
    }
    Ok(last.into_iter().map(|((r, c), v)| (r, c, v)).collect())
}

fn cell_value(v: &Value, cell: &str) -> Result<CellValue, Sentence> {
    Ok(match v {
        Value::Null => CellValue::Empty,
        Value::Bool(b) => CellValue::Bool(*b),
        Value::Number(n) => {
            let n = n
                .as_f64()
                .filter(|f| f.is_finite())
                .ok_or_else(|| format!("The number in cell {cell} is not finite."))?;
            CellValue::Number(n)
        }
        Value::String(s) => {
            if s.is_empty() {
                return Err(format!(
                    "Cell {cell} has an empty string. Use null to clear a cell."
                ));
            }
            if s.chars()
                .any(|c| c.is_control() && !matches!(c, '\n' | '\t' | '\r'))
            {
                return Err(format!(
                    "The text of cell {cell} contains a control character, which a spreadsheet cannot hold."
                ));
            }
            CellValue::Text(s.clone())
        }
        _ => {
            return Err(format!(
                "The value of cell {cell} must be a string, a number, true/false or null."
            ));
        }
    })
}

fn formula_value(f: &Value, cell: &str) -> Result<CellValue, Sentence> {
    let text = f
        .as_str()
        .ok_or_else(|| format!("The formula of cell {cell} must be a string."))?
        .trim();
    let text = text.strip_prefix('=').unwrap_or(text).trim();
    if text.is_empty() {
        return Err(format!("The formula of cell {cell} is empty."));
    }
    if text.chars().any(char::is_control) {
        return Err(format!(
            "The formula of cell {cell} contains a control character."
        ));
    }
    Ok(CellValue::Formula {
        formula: text.to_owned(),
        cached: None,
    })
}

fn sheet_name_arg(
    args: &Value,
    key: &str,
    names: &[String],
    except: Option<usize>,
) -> Result<String, Sentence> {
    let name = req_str(args, key)?.trim().to_owned();
    if name.chars().count() > 31 {
        return Err("A sheet name is at most 31 characters.".to_owned());
    }
    if name.contains(['\\', '/', '?', '*', '[', ']', ':'])
        || name.starts_with('\'')
        || name.ends_with('\'')
    {
        return Err(
            "A sheet name may not contain \\ / ? * [ ] : and may not start or end with an apostrophe."
                .to_owned(),
        );
    }
    if names
        .iter()
        .enumerate()
        .any(|(i, n)| Some(i) != except && n.to_lowercase() == name.to_lowercase())
    {
        return Err(format!(
            "A sheet named `{name}` already exists (names are compared ignoring case)."
        ));
    }
    Ok(name)
}

fn chart_kind_name(args: &Value) -> Result<(ChartKind, &'static str), Sentence> {
    let name = opt_str(args, "chart_type").unwrap_or_else(|| "column".to_owned());
    let found: Option<(ChartKind, &'static str)> = match name.as_str() {
        "bar" => Some((ChartKind::ClusteredBar, "bar")),
        "column" => Some((ChartKind::ClusteredColumn, "column")),
        "line" => Some((ChartKind::Line, "line")),
        "pie" => Some((ChartKind::Pie, "pie")),
        "doughnut" => Some((ChartKind::Doughnut, "doughnut")),
        "area" => Some((ChartKind::Area, "area")),
        "scatter" => Some((ChartKind::Scatter, "scatter")),
        _ => None,
    };
    found.ok_or_else(|| {
        format!(
            "chart_type `{name}` is not available here: use one of {}.",
            CHART_TYPES.join(", ")
        )
    })
}

/// Work out the call from its arguments and the workbook's sheet names.
/// Nothing is edited here.
pub(super) fn plan(tool: XlsxTool, args: &Value, names: &[String]) -> Result<Plan, Sentence> {
    match tool {
        XlsxTool::WriteCells => Ok(Plan::Write {
            sheet: sheet_arg(args, names)?,
            cells: write_cells_arg(args)?,
        }),
        XlsxTool::ClearRange => Ok(Plan::Clear {
            sheet: sheet_arg(args, names)?,
            range: range_arg(&req_str(args, "range")?)?,
        }),
        XlsxTool::AddSheet => {
            if names.len() >= 64 {
                return Err("A workbook may have at most 64 sheets.".to_owned());
            }
            Ok(Plan::AddSheet {
                name: sheet_name_arg(args, "name", names, None)?,
            })
        }
        XlsxTool::RenameSheet => {
            let sheet = sheet_arg(args, names)?;
            Ok(Plan::Rename {
                sheet,
                name: sheet_name_arg(args, "new_name", names, Some(sheet))?,
            })
        }
        XlsxTool::DeleteSheet => {
            if names.len() < 2 {
                return Err(
                    "A workbook needs at least one sheet, so the last one cannot be deleted."
                        .to_owned(),
                );
            }
            Ok(Plan::Delete {
                sheet: sheet_arg(args, names)?,
            })
        }
        XlsxTool::AddChart => {
            let sheet = sheet_arg(args, names)?;
            let (kind, kind_name) = chart_kind_name(args)?;
            let series = args
                .get("series")
                .and_then(Value::as_array)
                .filter(|s| !s.is_empty())
                .ok_or_else(|| "`series` is required: a list of {values, name}.".to_owned())?;
            if series.len() > 20 {
                return Err("A chart may have at most 20 series.".to_owned());
            }
            let series = series
                .iter()
                .map(|s| {
                    let values = s.get("values").and_then(Value::as_str).ok_or_else(|| {
                        "Every series needs `values`: a range such as B2:B9.".to_owned()
                    })?;
                    Ok(SheetSeries {
                        name: s.get("name").and_then(Value::as_str).map(str::to_owned),
                        values: values.trim().to_owned(),
                    })
                })
                .collect::<Result<Vec<_>, Sentence>>()?;
            let categories = opt_str(args, "categories").map(|c| c.trim().to_owned());
            for r in series.iter().map(|s| &s.values).chain(categories.iter()) {
                check_chart_range(r, names, sheet)?;
            }
            let anchor = match opt_str(args, "anchor") {
                Some(a) => Some(cell_arg(&a, "`anchor`")?),
                None => None,
            };
            Ok(Plan::Chart {
                sheet,
                spec: SheetChartSpec {
                    kind,
                    title: opt_str(args, "title"),
                    categories,
                    series,
                    legend: opt_bool(args, "legend").unwrap_or(true),
                    data_labels: opt_bool(args, "data_labels").unwrap_or(false),
                    anchor_row: 0,
                    anchor_col: 0,
                    width_px: u32::try_from(
                        opt_uint(args, "width").unwrap_or(480).clamp(100, 4000),
                    )
                    .unwrap_or(480),
                    height_px: u32::try_from(
                        opt_uint(args, "height").unwrap_or(288).clamp(100, 4000),
                    )
                    .unwrap_or(288),
                },
                anchor,
                kind: kind_name,
            })
        }
        _ => Err("This tool does not edit the workbook.".to_owned()),
    }
}

/// `Sheet2!B2:B9` or `B2:B9` as the sheet it names and its range.
fn split_chart_range(
    text: &str,
    names: &[String],
    own: usize,
) -> Result<(usize, CellRange), Sentence> {
    let (sheet, range) = match text.rsplit_once('!') {
        Some((s, r)) => {
            let s = s.trim().trim_matches('\'').replace("''", "'");
            let i = names
                .iter()
                .position(|n| *n == s)
                .ok_or_else(|| format!("The range `{text}` names a sheet that does not exist."))?;
            (i, r)
        }
        None => (own, text),
    };
    Ok((sheet, range_arg(range)?))
}

fn check_chart_range(text: &str, names: &[String], own: usize) -> Result<(), Sentence> {
    let (_, r) = split_chart_range(text, names, own)?;
    if r.cells() > 10_000 {
        return Err(format!("The range `{text}` is longer than 10000 cells."));
    }
    Ok(())
}

impl Plan {
    /// What the call needs read from the package, before and after.
    fn query(&self) -> Query {
        match self {
            Self::Write { sheet, cells } => Query {
                cells: cells.iter().map(|(r, c, _)| (*sheet, *r, *c)).collect(),
                ..Query::default()
            },
            Self::Clear { sheet, range } => Query {
                ranges: vec![(*sheet, *range)],
                ..Query::default()
            },
            Self::Rename { .. }
            | Self::Delete { .. }
            | Self::AddSheet { .. }
            | Self::Chart { .. } => Query::default(),
        }
    }
}

// ---------------------------------------------------------------------------
// Apply, save, check
// ---------------------------------------------------------------------------

fn apply(wb: &mut Workbook, plan: &Plan) -> Result<(), Sentence> {
    match plan {
        Plan::Write { sheet, cells } => {
            wb.write_cells(*sheet, cells).map_err(oe)?;
        }
        Plan::Clear { sheet, range } => {
            wb.clear_range(*sheet, *range).map_err(oe)?;
        }
        Plan::AddSheet { name } => {
            wb.add_sheet(name).map_err(oe)?;
        }
        Plan::Rename { sheet, name } => wb.rename_sheet(*sheet, name).map_err(oe)?,
        Plan::Delete { sheet } => wb.remove_sheet(*sheet).map_err(oe)?,
        Plan::Chart { sheet, spec, .. } => wb.add_chart(*sheet, spec).map_err(oe)?,
    }
    Ok(())
}

fn unplanned(what: &str) -> Sentence {
    format!(
        "Nothing was saved: after the edit {what}, which is not what was asked. The spreadsheet library did not carry the edit through as planned."
    )
}

/// `Sheet!$B$2:$B$9` / `'My Sheet'!B2:B9` as `My Sheet!B2:B9`, so a reference
/// as the file writes it compares with one as the call gave it.
fn norm_ref(text: &str, names: &[String], own: usize) -> Option<String> {
    let (sheet, range) = split_chart_range(text, names, own).ok()?;
    Some(format!("{}!{}", names[sheet], range_ref(range)))
}

fn saved_ref(text: &str) -> String {
    let (sheet, range) = text.rsplit_once('!').unwrap_or(("", text));
    let sheet = sheet.trim_matches('\'').replace("''", "'");
    let range = range.replace('$', "");
    format!("{sheet}!{range}")
}

#[allow(
    clippy::too_many_lines,
    clippy::float_cmp,
    reason = "a number we wrote is read back bit for bit"
)]
pub(crate) fn verify(
    before: &XlsxFacts,
    after: &XlsxFacts,
    plan: &Plan,
    names: &[String],
) -> Result<(), Sentence> {
    let before_names: Vec<&str> = before.sheets.iter().map(|s| s.name.as_str()).collect();
    let after_names: Vec<&str> = after.sheets.iter().map(|s| s.name.as_str()).collect();
    // Nothing the file held may be gone: no part of a kind fewer. Deleting a
    // sheet takes the parts ONLY that sheet reaches with it (its drawings,
    // charts, comments, tables and media); every other part must stay.
    let exempt: BTreeMap<String, usize> = match plan {
        Plan::Delete { sheet } => before.sheets[*sheet].owned_parts.clone(),
        _ => BTreeMap::new(),
    };
    {
        for (kind, n) in &before.parts {
            let now = after.parts.get(kind).copied().unwrap_or(0)
                + exempt.get(kind).copied().unwrap_or(0);
            if now < *n {
                tracing::warn!(
                    event = "xlsx_save_would_lose_content",
                    kind = kind.as_str(),
                    before = *n,
                    after = now,
                    "a save would have dropped content; refused"
                );
                return Err(format!(
                    "Nothing was saved: this edit would have removed {} `{kind}` part{} that it was not meant to touch.",
                    n - now,
                    if n - now == 1 { "" } else { "s" }
                ));
            }
        }
    }
    // The sheets other than the target are as they were: same cell count,
    // same charts.
    let same_sheet = |b: usize, a: usize| -> Result<(), Sentence> {
        let (bs, as_) = (&before.sheets[b], &after.sheets[a]);
        if bs.value_cells != as_.value_cells
            || bs.charts.len() != as_.charts.len()
            || bs.owned_parts != as_.owned_parts
        {
            return Err(unplanned(&format!("sheet `{}` had changed", bs.name)));
        }
        Ok(())
    };
    let others = |target: usize| -> Result<(), Sentence> {
        for i in 0..before.sheets.len() {
            if i != target {
                same_sheet(i, i)?;
            }
        }
        Ok(())
    };
    match plan {
        Plan::Write { sheet, cells } => {
            if before_names != after_names {
                return Err(unplanned("the sheets had changed"));
            }
            others(*sheet)?;
            let mut delta: i64 = 0;
            for (row, col, value) in cells {
                let key = (*sheet, *row, *col);
                let held = before.cells.contains_key(&key);
                let now = after.cells.get(&key);
                let want_value = !matches!(value, CellValue::Empty);
                delta += i64::from(want_value) - i64::from(held);
                let ok = match (value, now) {
                    (CellValue::Empty, None) => true,
                    (CellValue::Text(t), Some(CellValue::Text(s))) => t == s,
                    (CellValue::Number(n), Some(CellValue::Number(m))) => n == m,
                    (CellValue::Bool(b), Some(CellValue::Bool(c))) => b == c,
                    (
                        CellValue::Formula { formula: f, .. },
                        Some(CellValue::Formula { formula: g, .. }),
                    ) => f.trim() == g.trim().trim_start_matches('='),
                    _ => false,
                };
                if !ok {
                    return Err(unplanned(&format!(
                        "cell {} of sheet `{}` does not hold what was written",
                        cell_ref(*row, *col),
                        names[*sheet]
                    )));
                }
            }
            let (b, a) = (
                i64::try_from(before.sheets[*sheet].value_cells).unwrap_or(i64::MAX),
                i64::try_from(after.sheets[*sheet].value_cells).unwrap_or(i64::MAX),
            );
            if a != b + delta {
                return Err(unplanned(&format!(
                    "sheet `{}` holds {a} cells with a value, not the {} that were expected",
                    names[*sheet],
                    b + delta
                )));
            }
            if before.sheets[*sheet].charts.len() != after.sheets[*sheet].charts.len() {
                return Err(unplanned("a chart of the sheet had changed"));
            }
        }
        Plan::Clear { sheet, range } => {
            if before_names != after_names {
                return Err(unplanned("the sheets had changed"));
            }
            others(*sheet)?;
            if let Some(left) = after.cells.keys().min() {
                return Err(unplanned(&format!(
                    "cell {} of sheet `{}` is not cleared",
                    cell_ref(left.1, left.2),
                    names[*sheet]
                )));
            }
            let (b, a) = (
                before.sheets[*sheet].value_cells,
                after.sheets[*sheet].value_cells,
            );
            if a + before.cells.len() as u64 != b {
                return Err(unplanned(&format!(
                    "sheet `{}` holds {a} cells with a value, not the {} that were expected after clearing {}",
                    names[*sheet],
                    b.saturating_sub(before.cells.len() as u64),
                    range_ref(*range)
                )));
            }
        }
        Plan::AddSheet { name } => {
            let mut want: Vec<&str> = before_names.clone();
            want.push(name);
            if after_names != want {
                return Err(unplanned("the sheet is not the last one, under that name"));
            }
            for i in 0..before.sheets.len() {
                same_sheet(i, i)?;
            }
            if after.sheets.last().is_some_and(|s| s.value_cells != 0) {
                return Err(unplanned("the new sheet is not empty"));
            }
        }
        Plan::Rename { sheet, name } => {
            let mut want: Vec<&str> = before_names.clone();
            want[*sheet] = name;
            if after_names != want {
                return Err(unplanned("the sheet does not have its new name"));
            }
            for i in 0..before.sheets.len() {
                same_sheet(i, i)?;
            }
        }
        Plan::Delete { sheet } => {
            let mut want: Vec<&str> = before_names.clone();
            want.remove(*sheet);
            if after_names != want {
                return Err(unplanned("the sheet is not gone, or another one is"));
            }
            let mut a = 0;
            for b in 0..before.sheets.len() {
                if b == *sheet {
                    continue;
                }
                same_sheet(b, a)?;
                a += 1;
            }
        }
        Plan::Chart {
            sheet, spec, kind, ..
        } => {
            if before_names != after_names {
                return Err(unplanned("the sheets had changed"));
            }
            others(*sheet)?;
            let (b, a) = (&before.sheets[*sheet], &after.sheets[*sheet]);
            if b.value_cells != a.value_cells {
                return Err(unplanned("cells of the sheet had changed"));
            }
            if a.charts.len() != b.charts.len() + 1 || a.charts[..b.charts.len()] != b.charts[..] {
                return Err(unplanned(
                    "the chart is not in the file as one new chart of the sheet",
                ));
            }
            let new = &a.charts[b.charts.len()];
            if new.kind != *kind {
                return Err(unplanned(&format!(
                    "the new chart is a `{}` chart, not a `{kind}` chart",
                    new.kind
                )));
            }
            let have: Vec<&str> = new.refs.iter().map(String::as_str).collect();
            let mut want: Vec<String> = Vec::new();
            for s in &spec.series {
                want.extend(norm_ref(&s.values, names, *sheet));
            }
            want.extend(
                spec.categories
                    .iter()
                    .filter_map(|c| norm_ref(c, names, *sheet)),
            );
            let saved: Vec<String> = have.iter().map(|r| saved_ref(r)).collect();
            if let Some(missing) = want.iter().find(|w| !saved.contains(w)) {
                return Err(unplanned(&format!(
                    "the new chart does not refer to `{missing}`"
                )));
            }
        }
    }
    Ok(())
}

/// The cells with a value a refused call named, for the sentence.
fn message(plan: &Plan, names: &[String], before: &XlsxFacts) -> String {
    match plan {
        Plan::Write { sheet, cells } => format!(
            "Wrote {} cell{} in sheet `{}`.",
            cells.len(),
            if cells.len() == 1 { "" } else { "s" },
            names[*sheet]
        ),
        Plan::Clear { sheet, range } => format!(
            "Cleared {} cell{} with a value in {} of sheet `{}`.",
            before.cells.len(),
            if before.cells.len() == 1 { "" } else { "s" },
            range_ref(*range),
            names[*sheet]
        ),
        Plan::AddSheet { name } => format!("Added sheet {} `{name}`.", names.len() + 1),
        Plan::Rename { sheet, name } => {
            format!(
                "Renamed sheet {} `{}` to `{name}`.",
                sheet + 1,
                names[*sheet]
            )
        }
        Plan::Delete { sheet } => format!("Deleted sheet `{}`.", names[*sheet]),
        Plan::Chart { sheet, .. } => format!("Added a chart to sheet `{}`.", names[*sheet]),
    }
}

/// An authoring call end to end on a blocking thread: open (the one parse),
/// plan, edit, save (the one serialisation), check the saved bytes, and return
/// the message and the bytes to store.
pub(crate) fn run_edit(
    tool: XlsxTool,
    args: &Value,
    bytes: &[u8],
    limits: &Limits,
) -> Result<(String, Vec<u8>), Sentence> {
    let mut wb = Workbook::open(bytes, limits).map_err(oe)?;
    if wb.reads_inline_strings_as_empty() {
        return Err(INLINE_STRINGS.to_owned());
    }
    let names = wb.sheet_names().map_err(oe)?;
    let mut plan = plan(tool, args, &names)?;
    let mut query = plan.query();
    match &plan {
        Plan::Rename { sheet, .. } | Plan::Delete { sheet } => {
            query.mention = Some(names[*sheet].clone());
        }
        _ => {}
    }
    let before = facts(bytes, limits, &query)?;
    if before.sheets.len() != names.len() {
        return Err("The workbook's sheets could not be read consistently.".to_owned());
    }
    match &mut plan {
        Plan::Rename { sheet, .. } if before.sheet_mentioned => {
            return Err(format!(
                "Sheet `{}` is referred to by name from a formula, a defined name, a validation, a conditional format, a hyperlink, a chart or a pivot table, and those references are not rewritten, so the edit would break them. Nothing was changed.",
                names[*sheet]
            ));
        }
        // A delete takes the sheet's own cells and charts with it, so what
        // they say about the sheet does not block it.
        Plan::Delete { sheet } if before.sheet_mentioned_elsewhere => {
            return Err(format!(
                "Sheet `{}` is referred to by name from a formula or chart of another sheet, a defined name, a validation, a conditional format, a hyperlink or a pivot table, and those references are not rewritten, so the edit would break them. Nothing was changed.",
                names[*sheet]
            ));
        }
        Plan::Delete { sheet } if before.local_name_sheet_ids.iter().any(|id| *id >= *sheet) => {
            return Err(format!(
                "A defined name or print area is scoped to sheet `{}` or to a sheet after it by position, and those scopes are not renumbered when a sheet is deleted, so the edit would move them to the wrong sheets. Nothing was changed.",
                names[*sheet]
            ));
        }
        Plan::Chart {
            sheet,
            spec,
            anchor,
            ..
        } => {
            let (row, col) = anchor.unwrap_or_else(|| {
                before.sheets[*sheet].used_range.map_or((0, 0), |u| {
                    (u.first_row, u.last_col.saturating_add(2).min(16_000))
                })
            });
            spec.anchor_row = row;
            spec.anchor_col = col;
        }
        _ => {}
    }
    apply(&mut wb, &plan)?;
    let saved = wb.save().map_err(oe)?;
    let after = facts(&saved, limits, &query)?;
    verify(&before, &after, &plan, &names)?;
    Ok((message(&plan, &names, &before), saved))
}

/// A new workbook, with its first sheet named when asked, checked on the saved
/// bytes.
pub(crate) fn create(sheet_name: Option<&str>, limits: &Limits) -> Result<Vec<u8>, Sentence> {
    let mut wb = Workbook::new(limits);
    let name = match sheet_name {
        Some(n) => {
            let n = sheet_name_arg(&json!({"name": n}), "name", &[], None)?;
            wb.rename_sheet(0, &n).map_err(oe)?;
            n
        }
        None => "Sheet1".to_owned(),
    };
    let saved = wb.save().map_err(oe)?;
    let after = facts(&saved, limits, &Query::default())?;
    if after.sheets.len() != 1 || after.sheets[0].name != name {
        return Err(unplanned("the workbook does not have its one sheet"));
    }
    Ok(saved)
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

fn text_of(value: &CellValue) -> String {
    match value {
        CellValue::Text(s) | CellValue::Error(s) => s.clone(),
        CellValue::Number(n) | CellValue::DateTime(n) => n.to_string(),
        CellValue::Bool(b) => if *b { "TRUE" } else { "FALSE" }.to_owned(),
        CellValue::Formula { formula, cached } => cached
            .as_deref()
            .map_or_else(|| format!("={formula}"), text_of),
        _ => String::new(),
    }
}

fn json_of(value: &CellValue) -> Value {
    match value {
        CellValue::Text(s) | CellValue::Error(s) => Value::String(s.clone()),
        CellValue::Number(n) | CellValue::DateTime(n) => {
            serde_json::Number::from_f64(*n).map_or(Value::Null, Value::Number)
        }
        CellValue::Bool(b) => Value::Bool(*b),
        CellValue::Formula { formula, cached } => cached
            .as_deref()
            .map_or_else(|| Value::String(format!("={formula}")), json_of),
        _ => Value::Null,
    }
}

fn check_width(r: CellRange) -> Result<(), Sentence> {
    let width = u64::from(r.last_col - r.first_col) + 1;
    if width > MAX_READ_CELLS {
        return Err(format!(
            "The range is {width} columns wide; at most {MAX_READ_CELLS} cells can be read at once."
        ));
    }
    Ok(())
}

/// `r` cut to the rows that fit the cell cap, and its row count before the cut.
fn fit_rows(r: CellRange) -> (CellRange, u32) {
    let width = u64::from(r.last_col - r.first_col) + 1;
    let rows_fit = u32::try_from(MAX_READ_CELLS / width.max(1))
        .unwrap_or(u32::MAX)
        .max(1);
    let total = r.last_row - r.first_row + 1;
    (
        CellRange {
            last_row: r.first_row + total.min(rows_fit) - 1,
            ..r
        },
        total,
    )
}

/// `list_sheets`.
pub(crate) fn list_sheets(bytes: &[u8], limits: &Limits) -> Result<Value, Sentence> {
    let f = facts(bytes, limits, &Query::default())?;
    let sheets: Vec<Value> = f
        .sheets
        .iter()
        .enumerate()
        .map(|(i, s)| {
            json!({
                "sheet": i + 1,
                "name": s.name,
                "used_range": s.used_range.map(range_ref),
                "cells_with_a_value": s.value_cells,
                "charts": s.charts.len(),
            })
        })
        .collect();
    Ok(json!({"sheets": sheets}))
}

/// `read_range` and `sheet_to_markdown`: the range (or the used range) of one
/// sheet, cut at `MAX_READ_CELLS`.
pub(crate) fn read_range(
    bytes: &[u8],
    args: &Value,
    limits: &Limits,
    json_format: bool,
) -> Result<Value, Sentence> {
    let asked = match opt_str(args, "range") {
        Some(r) => Some(range_arg(&r)?),
        None => None,
    };
    if let Some(r) = asked {
        check_width(r)?;
    }
    // One validate-and-scan: the sheet is picked once the names are known, and
    // the range's cells are collected in the same pass.
    let picked = args.clone();
    let (sheet, f) = elitea_office_core::facts_of_xlsx_package_choosing(
        bytes,
        limits,
        usize::try_from(MAX_READ_CELLS).unwrap_or(usize::MAX),
        move |names| {
            let sheet = sheet_arg(&picked, names).map_err(OfficeError::InvalidInput)?;
            Ok((sheet, asked.map(|r| fit_rows(r).0)))
        },
    )
    .map_err(oe)?;
    let names: Vec<String> = f.sheets.iter().map(|s| s.name.clone()).collect();
    let Some(wanted) = asked.or(f.sheets[sheet].used_range) else {
        return Ok(Value::String(format!("Sheet `{}` is empty.", names[sheet])));
    };
    check_width(wanted)?;
    let (range, total_rows) = fit_rows(wanted);
    let truncated = total_rows > range.last_row - range.first_row + 1;
    let at = |r: u32, c: u16| f.cells.get(&(sheet, r, c));
    let mut formulas: Map<String, Value> = Map::new();
    for ((_, r, c), v) in &f.cells {
        let inside = (range.first_row..=range.last_row).contains(r)
            && (range.first_col..=range.last_col).contains(c);
        if !inside {
            continue;
        }
        if let CellValue::Formula { formula, .. } = v {
            formulas.insert(cell_ref(*r, *c), Value::String(format!("={formula}")));
        }
    }
    let note = "Formulas show their last calculated value (this tool does not calculate); dates are Excel serial numbers.";
    let cut = if truncated {
        format!(
            "Showing the first {} of {} rows of the range; read the rest with a range starting at row {}.",
            range.last_row - range.first_row + 1,
            total_rows,
            range.last_row + 2
        )
    } else {
        String::new()
    };
    if json_format {
        let rows: Vec<Value> = (range.first_row..=range.last_row)
            .map(|r| {
                Value::Array(
                    (range.first_col..=range.last_col)
                        .map(|c| at(r, c).map_or(Value::Null, json_of))
                        .collect(),
                )
            })
            .collect();
        return Ok(json!({
            "sheet": names[sheet],
            "range": range_ref(range),
            "rows": rows,
            "formulas": formulas,
            "truncated": truncated,
            "note": format!("{note} {cut}").trim().to_owned(),
        }));
    }
    let mut out = format!("Sheet `{}`, {}:\n\n| |", names[sheet], range_ref(range));
    for c in range.first_col..=range.last_col {
        let _ = write!(out, " {} |", column_name(c));
    }
    out.push_str("\n| --- |");
    for _ in range.first_col..=range.last_col {
        out.push_str(" --- |");
    }
    out.push('\n');
    for r in range.first_row..=range.last_row {
        let _ = write!(out, "| {} |", r + 1);
        for c in range.first_col..=range.last_col {
            let text = at(r, c).map(text_of).unwrap_or_default();
            let cell = text.replace('|', "\\|").replace(['\n', '\r'], " ");
            let _ = write!(out, " {cell} |");
        }
        out.push('\n');
    }
    if !formulas.is_empty() {
        out.push_str("\nFormulas:\n");
        for (cell, f) in &formulas {
            let _ = writeln!(out, "- {cell}: {}", f.as_str().unwrap_or_default());
        }
    }
    let _ = write!(out, "\n{note} {cut}");
    Ok(Value::String(out.trim_end().to_owned()))
}
