//! The in-memory table the data analysis tools compute on: typed, nullable,
//! columnar, and bounded before it grows (ADR-0033 decisions 2 and 4).
//!
//! Everything that reads a file builds a [`Table`] through
//! [`Column::from_raw`], so CSV, JSON and Excel share one type inference. A
//! table never exceeds [`Limits`]: the readers check the row, column and cell
//! counts as they go, not after the allocation.

use std::fmt;
use std::time::{Duration, Instant};

use serde_json::{Number, Value};

/// Every bound the engine enforces. A model cannot change any of them: no
/// tool argument maps to this struct.
#[derive(Clone, Debug)]
pub(crate) struct Limits {
    /// The most bytes a source file may hold (the platform's object cap).
    pub(crate) max_input_bytes: u64,
    /// Rows of one table.
    pub(crate) max_rows: usize,
    /// Columns of one table.
    pub(crate) max_columns: usize,
    /// Cells (rows times columns) of one table.
    pub(crate) max_cells: usize,
    /// Bytes of one text cell.
    pub(crate) max_cell_bytes: usize,
    /// Distinct values a pivot may spread into columns.
    pub(crate) max_pivot_columns: usize,
    /// Rows a tool answer carries inline, at most.
    pub(crate) max_preview_rows: usize,
    /// Bytes of one tool answer, before it is cut and says so.
    pub(crate) max_answer_bytes: usize,
    /// Cells handed to the code sandbox as its input.
    pub(crate) max_sandbox_cells: usize,
    /// Bytes of one sandbox request (its own bound is 1 MiB).
    pub(crate) max_sandbox_bytes: usize,
    /// JSON nodes of one sandbox input value (its own bound is 100,000);
    /// arrays and objects count as well as scalars.
    pub(crate) max_sandbox_nodes: usize,
    /// Wall clock of one call. Checked inside every loop that visits rows, so
    /// it stops the work, unlike a deadline wrapped around a thread.
    pub(crate) deadline: Duration,
    /// The longest a call waits for a free analysis slot (never past its own
    /// deadline) before it says the engine is busy.
    pub(crate) queue_timeout: Duration,
    /// The Office engine's own bounds (archive, XML, cells, permits), for
    /// the `.xlsx` reader and writer and the chart rasteriser.
    #[cfg(feature = "toolkit-data-analysis")]
    pub(crate) office: elitea_office_core::Limits,
}

impl Default for Limits {
    fn default() -> Self {
        Self {
            max_input_bytes: 50 * 1024 * 1024,
            max_rows: 1_000_000,
            max_columns: 1_000,
            max_cells: 5_000_000,
            max_cell_bytes: 32 * 1024,
            max_pivot_columns: 500,
            max_preview_rows: 500,
            max_answer_bytes: 60 * 1024,
            max_sandbox_cells: 50_000,
            max_sandbox_bytes: 1024 * 1024,
            max_sandbox_nodes: 100_000,
            deadline: Duration::from_secs(20),
            queue_timeout: Duration::from_secs(30),
            #[cfg(feature = "toolkit-data-analysis")]
            office: elitea_office_core::Limits::default(),
        }
    }
}

/// A call's clock.
#[derive(Clone, Copy, Debug)]
pub(crate) struct Deadline {
    at: Instant,
}

impl Deadline {
    pub(crate) fn after(limit: Duration) -> Self {
        Self {
            at: Instant::now() + limit,
        }
    }

    /// Time left, zero when it has run out.
    pub(crate) fn remaining(&self) -> Duration {
        self.at.saturating_duration_since(Instant::now())
    }

    /// Fail when the call has run out of time. Cheap enough to call every few
    /// thousand rows.
    pub(crate) fn check(&self) -> Result<(), DataError> {
        if Instant::now() >= self.at {
            Err(DataError::TimedOut)
        } else {
            Ok(())
        }
    }
}

/// How often loops over rows look at the clock.
pub(crate) const CHECK_EVERY: usize = 4096;

/// Why an operation failed, in the terms the model reads: one sentence per
/// cause, and the sentence says what to do next.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) enum DataError {
    /// The caller's arguments are wrong; the text names the argument.
    Argument(String),
    /// The file is not what its name says, or is damaged.
    Malformed(String),
    /// A named column does not exist.
    NoColumn { name: String, available: String },
    /// The data is over a bound.
    Limit(String),
    /// This kind of input is not supported.
    Unsupported(String),
    /// The call ran out of time.
    TimedOut,
}

impl fmt::Display for DataError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Argument(text) | Self::Malformed(text) | Self::Unsupported(text) => {
                f.write_str(text)
            }
            Self::Limit(text) => f.write_str(text),
            Self::NoColumn { name, available } => {
                write!(f, "There is no column named `{name}`. Columns: {available}.")
            }
            Self::TimedOut => f.write_str(
                "The analysis took longer than the 20 second limit and was stopped. Filter the rows or select fewer columns first.",
            ),
        }
    }
}

impl std::error::Error for DataError {}

pub(crate) type DataResult<T> = Result<T, DataError>;

pub(crate) fn argument(text: impl Into<String>) -> DataError {
    DataError::Argument(text.into())
}

// ---------------------------------------------------------------------------
// Columns
// ---------------------------------------------------------------------------

/// The type of a column.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum DType {
    Int,
    Float,
    Bool,
    Text,
    Date,
    DateTime,
}

impl DType {
    pub(crate) const fn name(self) -> &'static str {
        match self {
            Self::Int => "integer",
            Self::Float => "number",
            Self::Bool => "boolean",
            Self::Text => "text",
            Self::Date => "date",
            Self::DateTime => "datetime",
        }
    }

    pub(crate) const fn is_numeric(self) -> bool {
        matches!(self, Self::Int | Self::Float)
    }
}

/// One column: a vector of nullable values of one type. Times are
/// microseconds since 1970-01-01 (naive, no zone); a date is midnight.
#[derive(Clone, Debug)]
pub(crate) enum Column {
    Int(Vec<Option<i64>>),
    Float(Vec<Option<f64>>),
    Bool(Vec<Option<bool>>),
    Text(Vec<Option<Box<str>>>),
    Date(Vec<Option<i64>>),
    DateTime(Vec<Option<i64>>),
}

/// One cell, borrowed.
#[derive(Clone, Copy, Debug)]
pub(crate) enum Cell<'a> {
    Null,
    Int(i64),
    Float(f64),
    Bool(bool),
    Text(&'a str),
    Date(i64),
    DateTime(i64),
}

impl Cell<'_> {
    pub(crate) fn is_null(&self) -> bool {
        matches!(self, Cell::Null)
    }

    /// The cell as a number, for the statistics: booleans count as 0 and 1.
    pub(crate) fn as_f64(&self) -> Option<f64> {
        match *self {
            Cell::Int(v) => Some(v as f64),
            Cell::Float(v) if v.is_finite() => Some(v),
            Cell::Bool(v) => Some(f64::from(u8::from(v))),
            _ => None,
        }
    }

    /// The cell as JSON: dates as ISO text, a non-finite number as null.
    pub(crate) fn json(&self) -> Value {
        match *self {
            Cell::Null => Value::Null,
            Cell::Int(v) => Value::from(v),
            Cell::Float(v) => Number::from_f64(v).map_or(Value::Null, Value::Number),
            Cell::Bool(v) => Value::Bool(v),
            Cell::Text(v) => Value::String(v.to_owned()),
            Cell::Date(v) => Value::String(format_time(v, true)),
            Cell::DateTime(v) => Value::String(format_time(v, false)),
        }
    }

    /// The cell as plain text (CSV and chart labels).
    pub(crate) fn text(&self) -> String {
        match *self {
            Cell::Null => String::new(),
            Cell::Int(v) => v.to_string(),
            Cell::Float(v) => format_float(v),
            Cell::Bool(v) => v.to_string(),
            Cell::Text(v) => v.to_owned(),
            Cell::Date(v) => format_time(v, true),
            Cell::DateTime(v) => format_time(v, false),
        }
    }
}

pub(crate) fn format_float(v: f64) -> String {
    if !v.is_finite() {
        return String::new();
    }
    if v == v.trunc() && v.abs() < 1e15 {
        format!("{v:.1}")
    } else {
        format!("{v}")
    }
}

impl Column {
    pub(crate) fn len(&self) -> usize {
        match self {
            Self::Int(v) | Self::Date(v) | Self::DateTime(v) => v.len(),
            Self::Float(v) => v.len(),
            Self::Bool(v) => v.len(),
            Self::Text(v) => v.len(),
        }
    }

    pub(crate) fn dtype(&self) -> DType {
        match self {
            Self::Int(_) => DType::Int,
            Self::Float(_) => DType::Float,
            Self::Bool(_) => DType::Bool,
            Self::Text(_) => DType::Text,
            Self::Date(_) => DType::Date,
            Self::DateTime(_) => DType::DateTime,
        }
    }

    pub(crate) fn cell(&self, row: usize) -> Cell<'_> {
        match self {
            Self::Int(v) => v[row].map_or(Cell::Null, Cell::Int),
            Self::Float(v) => v[row].map_or(Cell::Null, Cell::Float),
            Self::Bool(v) => v[row].map_or(Cell::Null, Cell::Bool),
            Self::Text(v) => v[row].as_deref().map_or(Cell::Null, Cell::Text),
            Self::Date(v) => v[row].map_or(Cell::Null, Cell::Date),
            Self::DateTime(v) => v[row].map_or(Cell::Null, Cell::DateTime),
        }
    }

    pub(crate) fn is_null(&self, row: usize) -> bool {
        match self {
            Self::Int(v) | Self::Date(v) | Self::DateTime(v) => v[row].is_none(),
            Self::Float(v) => v[row].is_none(),
            Self::Bool(v) => v[row].is_none(),
            Self::Text(v) => v[row].is_none(),
        }
    }

    /// A new column of the rows `index` names, in that order; `None` is a
    /// null row (the unmatched side of a join).
    pub(crate) fn gather(&self, index: &[Option<usize>]) -> Column {
        fn pick<T: Clone>(v: &[Option<T>], index: &[Option<usize>]) -> Vec<Option<T>> {
            index.iter().map(|i| i.and_then(|i| v[i].clone())).collect()
        }
        match self {
            Self::Int(v) => Self::Int(pick(v, index)),
            Self::Float(v) => Self::Float(pick(v, index)),
            Self::Bool(v) => Self::Bool(pick(v, index)),
            Self::Text(v) => Self::Text(pick(v, index)),
            Self::Date(v) => Self::Date(pick(v, index)),
            Self::DateTime(v) => Self::DateTime(pick(v, index)),
        }
    }

    pub(crate) fn take(&self, rows: &[usize]) -> Column {
        let index: Vec<Option<usize>> = rows.iter().map(|r| Some(*r)).collect();
        self.gather(&index)
    }

    /// Build a column from raw cells, inferring the narrowest type that holds
    /// every non-null cell.
    pub(crate) fn from_raw(cells: Vec<Raw>) -> Column {
        let mut kinds = Kinds::default();
        for cell in &cells {
            kinds.add(cell);
        }
        kinds.build(cells)
    }

    /// A column of text built from an iterator of cells.
    pub(crate) fn text_from(values: impl IntoIterator<Item = Option<String>>) -> Column {
        Column::Text(
            values
                .into_iter()
                .map(|v| v.map(String::into_boxed_str))
                .collect(),
        )
    }
}

/// One cell as a reader found it, before any type is chosen.
#[derive(Clone, Debug)]
pub(crate) enum Raw {
    Null,
    Text(String),
    Int(i64),
    Float(f64),
    Bool(bool),
    /// An Excel serial date.
    Serial(f64),
}

#[derive(Default)]
struct Kinds {
    text: bool,
    int: bool,
    float: bool,
    boolean: bool,
    serial: bool,
    nulls: usize,
    total: usize,
}

impl Kinds {
    fn add(&mut self, cell: &Raw) {
        self.total += 1;
        match cell {
            Raw::Null => self.nulls += 1,
            Raw::Text(_) => self.text = true,
            Raw::Int(_) => self.int = true,
            Raw::Float(_) => self.float = true,
            Raw::Bool(_) => self.boolean = true,
            Raw::Serial(_) => self.serial = true,
        }
    }

    fn build(&self, cells: Vec<Raw>) -> Column {
        let kinds = [self.text, self.int || self.float, self.boolean, self.serial]
            .iter()
            .filter(|k| **k)
            .count();
        if self.nulls == self.total || kinds > 1 {
            // All empty, or a mixture: text keeps every cell as written.
            return Column::text_from(cells.into_iter().map(Raw::into_text));
        }
        if self.text {
            return infer_from_text(cells);
        }
        if self.boolean {
            return Column::Bool(
                cells
                    .into_iter()
                    .map(|c| if let Raw::Bool(b) = c { Some(b) } else { None })
                    .collect(),
            );
        }
        if self.serial {
            let all_midnight = cells
                .iter()
                .all(|c| !matches!(c, Raw::Serial(s) if s.fract().abs() > 1e-9));
            let micros: Vec<Option<i64>> = cells
                .into_iter()
                .map(|c| match c {
                    Raw::Serial(s) => serial_to_micros(s),
                    _ => None,
                })
                .collect();
            return if all_midnight {
                Column::Date(micros)
            } else {
                Column::DateTime(micros)
            };
        }
        if self.float {
            return Column::Float(
                cells
                    .into_iter()
                    .map(|c| match c {
                        Raw::Int(v) => Some(v as f64),
                        Raw::Float(v) if v.is_finite() => Some(v),
                        _ => None,
                    })
                    .collect(),
            );
        }
        Column::Int(
            cells
                .into_iter()
                .map(|c| if let Raw::Int(v) = c { Some(v) } else { None })
                .collect(),
        )
    }
}

impl Raw {
    fn into_text(self) -> Option<String> {
        match self {
            Raw::Null => None,
            Raw::Text(t) => Some(t),
            Raw::Int(v) => Some(v.to_string()),
            Raw::Float(v) => Some(format_float(v)),
            Raw::Bool(v) => Some(v.to_string()),
            Raw::Serial(s) => serial_to_micros(s).map(|m| {
                let midnight = s.fract().abs() < 1e-9;
                format_time(m, midnight)
            }),
        }
    }
}

/// The strings pandas reads as missing.
fn is_null_token(text: &str) -> bool {
    matches!(
        text.trim(),
        "" | "#N/A"
            | "#N/A N/A"
            | "#NA"
            | "-1.#IND"
            | "-1.#QNAN"
            | "-NaN"
            | "-nan"
            | "1.#IND"
            | "1.#QNAN"
            | "<NA>"
            | "N/A"
            | "NA"
            | "NULL"
            | "NaN"
            | "None"
            | "n/a"
            | "nan"
            | "null"
    )
}

/// Parse every text cell as one type, or keep them all as text.
fn infer_from_text(cells: Vec<Raw>) -> Column {
    let texts: Vec<Option<String>> = cells
        .into_iter()
        .map(|c| match c {
            Raw::Text(t) if is_null_token(&t) => None,
            Raw::Null => None,
            other => other.into_text(),
        })
        .collect();
    let present = || texts.iter().flatten().map(|t| t.trim());
    if present().next().is_none() {
        return Column::text_from(texts);
    }
    if present().all(|t| parse_bool(t).is_some()) {
        return Column::Bool(
            texts
                .iter()
                .map(|t| t.as_deref().and_then(|t| parse_bool(t.trim())))
                .collect(),
        );
    }
    if present().all(|t| parse_int(t).is_some()) {
        return Column::Int(
            texts
                .iter()
                .map(|t| t.as_deref().and_then(|t| parse_int(t.trim())))
                .collect(),
        );
    }
    if present().all(|t| parse_float(t).is_some()) {
        return Column::Float(
            texts
                .iter()
                .map(|t| t.as_deref().and_then(|t| parse_float(t.trim())))
                .collect(),
        );
    }
    if present().all(|t| parse_time(t).is_some()) {
        let date_only = present().all(|t| parse_time(t).is_some_and(|(_, d)| d));
        let micros = texts
            .iter()
            .map(|t| t.as_deref().and_then(|t| parse_time(t.trim()).map(|p| p.0)))
            .collect();
        return if date_only {
            Column::Date(micros)
        } else {
            Column::DateTime(micros)
        };
    }
    Column::text_from(texts)
}

pub(crate) fn parse_bool(text: &str) -> Option<bool> {
    match text {
        "true" | "True" | "TRUE" => Some(true),
        "false" | "False" | "FALSE" => Some(false),
        _ => None,
    }
}

pub(crate) fn parse_int(text: &str) -> Option<i64> {
    let digits = text.strip_prefix(['-', '+']).unwrap_or(text);
    if digits.is_empty() || !digits.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    text.parse().ok()
}

pub(crate) fn parse_float(text: &str) -> Option<f64> {
    let body = text.strip_prefix(['-', '+']).unwrap_or(text);
    // Rust accepts `inf`, `nan` and `infinity`; a data file that says so is
    // text, not a number.
    if !body
        .bytes()
        .next()
        .is_some_and(|b| b.is_ascii_digit() || b == b'.')
        || !body
            .bytes()
            .all(|b| b.is_ascii_digit() || matches!(b, b'.' | b'e' | b'E' | b'+' | b'-'))
    {
        return None;
    }
    text.parse::<f64>().ok().filter(|v| v.is_finite())
}

// ---------------------------------------------------------------------------
// Time
// ---------------------------------------------------------------------------

const MICROS_PER_DAY: i64 = 86_400_000_000;

/// Days from 1970-01-01 to a civil date (proleptic Gregorian).
fn days_from_civil(year: i64, month: i64, day: i64) -> i64 {
    let y = if month <= 2 { year - 1 } else { year };
    let era = y.div_euclid(400);
    let yoe = y - era * 400;
    let doy = (153 * (month + if month > 2 { -3 } else { 9 }) + 2) / 5 + day - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146_097 + doe - 719_468
}

fn civil_from_days(days: i64) -> (i64, i64, i64) {
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z - era * 146_097;
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    (if m <= 2 { y + 1 } else { y }, m, d)
}

fn days_in_month(year: i64, month: i64) -> i64 {
    match month {
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        4 | 6 | 9 | 11 => 30,
        _ if (year % 4 == 0 && year % 100 != 0) || year % 400 == 0 => 29,
        _ => 28,
    }
}

/// `YYYY-MM-DD`, or `YYYY-MM-DD` then `T` or a space and `HH:MM[:SS[.f]]`
/// with an optional `Z`. Returns the micros and whether it was a bare date.
pub(crate) fn parse_time(text: &str) -> Option<(i64, bool)> {
    let bytes = text.as_bytes();
    if bytes.len() < 10 || bytes[4] != b'-' || bytes[7] != b'-' {
        return None;
    }
    let num = |s: &str| -> Option<i64> {
        if s.is_empty() || !s.bytes().all(|b| b.is_ascii_digit()) {
            return None;
        }
        s.parse().ok()
    };
    let (year, month, day) = (num(&text[0..4])?, num(&text[5..7])?, num(&text[8..10])?);
    if !(1..=12).contains(&month) || day < 1 || day > days_in_month(year, month) || year < 1 {
        return None;
    }
    let base = days_from_civil(year, month, day) * MICROS_PER_DAY;
    if bytes.len() == 10 {
        return Some((base, true));
    }
    if bytes[10] != b'T' && bytes[10] != b' ' {
        return None;
    }
    let rest = text[11..].strip_suffix('Z').unwrap_or(&text[11..]);
    let mut parts = rest.splitn(3, ':');
    let hour = num(parts.next()?)?;
    let minute = num(parts.next()?)?;
    let (second, micros) = match parts.next() {
        None => (0, 0),
        Some(s) => {
            let (whole, fraction) = s.split_once('.').map_or((s, ""), |(w, f)| (w, f));
            let micros = if fraction.is_empty() {
                0
            } else if fraction.len() > 9 {
                return None;
            } else {
                let padded = format!("{fraction:0<6}");
                num(&padded[..6])?
            };
            (num(whole)?, micros)
        }
    };
    if hour > 23 || minute > 59 || second > 59 {
        return None;
    }
    Some((
        base + ((hour * 60 + minute) * 60 + second) * 1_000_000 + micros,
        false,
    ))
}

pub(crate) fn format_time(micros: i64, date_only: bool) -> String {
    let days = micros.div_euclid(MICROS_PER_DAY);
    let (y, m, d) = civil_from_days(days);
    if date_only {
        return format!("{y:04}-{m:02}-{d:02}");
    }
    let rest = micros.rem_euclid(MICROS_PER_DAY);
    let seconds = rest / 1_000_000;
    let fraction = rest % 1_000_000;
    let base = format!(
        "{y:04}-{m:02}-{d:02}T{:02}:{:02}:{:02}",
        seconds / 3600,
        seconds % 3600 / 60,
        seconds % 60
    );
    if fraction == 0 {
        base
    } else {
        format!("{base}.{fraction:06}")
    }
}

/// An Excel serial (1900 system) as micros since 1970, or `None` outside the
/// years a table can hold.
fn serial_to_micros(serial: f64) -> Option<i64> {
    if !serial.is_finite() || !(1.0..2_958_466.0).contains(&serial) {
        return None;
    }
    let micros = ((serial - 25_569.0) * 86_400.0 * 1_000_000.0).round();
    Some(micros as i64)
}

// ---------------------------------------------------------------------------
// Tables
// ---------------------------------------------------------------------------

/// A named, typed grid.
#[derive(Clone, Debug)]
pub(crate) struct Table {
    pub(crate) names: Vec<String>,
    pub(crate) columns: Vec<Column>,
    pub(crate) rows: usize,
}

impl Table {
    pub(crate) fn new(names: Vec<String>, columns: Vec<Column>) -> Self {
        let rows = columns.first().map_or(0, Column::len);
        Self {
            names,
            columns,
            rows,
        }
    }

    pub(crate) fn width(&self) -> usize {
        self.columns.len()
    }

    /// The index of a column by name: exact, else the single case-insensitive
    /// match, else an error that lists what exists.
    pub(crate) fn find(&self, name: &str) -> DataResult<usize> {
        let wanted = name.trim();
        if let Some(i) = self.names.iter().position(|n| n == wanted) {
            return Ok(i);
        }
        let lower = wanted.to_lowercase();
        let mut hits = self
            .names
            .iter()
            .enumerate()
            .filter(|(_, n)| n.to_lowercase() == lower);
        if let (Some((i, _)), None) = (hits.next(), hits.next()) {
            return Ok(i);
        }
        Err(DataError::NoColumn {
            name: wanted.to_owned(),
            available: self.available(),
        })
    }

    pub(crate) fn available(&self) -> String {
        let shown: Vec<String> = self
            .names
            .iter()
            .take(40)
            .map(|n| format!("`{n}`"))
            .collect();
        let more = self.names.len().saturating_sub(40);
        if more > 0 {
            format!("{} and {more} more", shown.join(", "))
        } else {
            shown.join(", ")
        }
    }

    pub(crate) fn take_rows(&self, rows: &[usize]) -> Table {
        Table {
            names: self.names.clone(),
            columns: self.columns.iter().map(|c| c.take(rows)).collect(),
            rows: rows.len(),
        }
    }

    pub(crate) fn select(&self, indices: &[usize]) -> Table {
        Table {
            names: indices.iter().map(|i| self.names[*i].clone()).collect(),
            columns: indices.iter().map(|i| self.columns[*i].clone()).collect(),
            rows: self.rows,
        }
    }

    pub(crate) fn cell_count(&self) -> usize {
        self.rows.saturating_mul(self.columns.len())
    }
}

/// Header names as pandas makes them: an empty one is `Unnamed: N`, a repeat
/// gets `.1`, `.2` ...
pub(crate) fn unique_names(raw: Vec<String>) -> Vec<String> {
    let mut seen: std::collections::HashMap<String, usize> = std::collections::HashMap::new();
    let mut names = Vec::with_capacity(raw.len());
    for (i, name) in raw.into_iter().enumerate() {
        let name = name.trim().to_owned();
        let base = if name.is_empty() {
            format!("Unnamed: {i}")
        } else {
            name
        };
        let mut candidate = base.clone();
        while seen.contains_key(&candidate) {
            let n = seen.entry(base.clone()).or_insert(0);
            *n += 1;
            candidate = format!("{base}.{n}");
        }
        seen.entry(candidate.clone()).or_insert(0);
        names.push(candidate);
    }
    names
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn civil_dates_round_trip() {
        for (y, m, d) in [(1970, 1, 1), (2000, 2, 29), (2024, 12, 31), (1969, 7, 20)] {
            let days = days_from_civil(y, m, d);
            assert_eq!(civil_from_days(days), (y, m, d));
        }
        assert_eq!(parse_time("1970-01-02"), Some((MICROS_PER_DAY, true)));
        assert_eq!(parse_time("2023-02-29"), None);
        assert_eq!(parse_time("2024-02-29").map(|p| p.1), Some(true));
        let (m, _) = parse_time("2024-05-06T07:08:09.5Z").unwrap();
        assert_eq!(format_time(m, false), "2024-05-06T07:08:09.500000");
    }

    #[test]
    fn inference_picks_the_narrowest_type() {
        let col = |v: &[&str]| Column::from_raw(v.iter().map(|s| Raw::Text((*s).into())).collect());
        assert_eq!(col(&["1", "2", ""]).dtype(), DType::Int);
        assert_eq!(col(&["1", "2.5"]).dtype(), DType::Float);
        assert_eq!(col(&["true", "False"]).dtype(), DType::Bool);
        assert_eq!(col(&["2024-01-01", "2024-02-03"]).dtype(), DType::Date);
        assert_eq!(
            col(&["2024-01-01", "2024-02-03 10:00"]).dtype(),
            DType::DateTime
        );
        assert_eq!(col(&["1", "x"]).dtype(), DType::Text);
        assert_eq!(col(&["inf", "nan1"]).dtype(), DType::Text);
        assert_eq!(col(&["", "NA"]).dtype(), DType::Text);
    }

    #[test]
    fn names_are_made_unique_the_way_pandas_does() {
        let names = unique_names(vec!["a".into(), String::new(), "a".into(), "a".into()]);
        assert_eq!(names, ["a", "Unnamed: 1", "a.1", "a.2"]);
    }
}
