//! Reading and writing the file formats: delimited text, JSON, and xlsx.
//!
//! Every reader treats its bytes as hostile. Row, column, cell and cell-size
//! bounds are checked while a table is being built, never after, so a 50 MiB
//! file of single-character cells cannot allocate its way past the limits.
//! xlsx goes through `elitea-office-core` (ADR-0032), which validates the
//! archive before a parser sees it.

use serde_json::Value;

use super::table::{
    CHECK_EVERY, Cell, Column, DataError, DataResult, Deadline, Limits, Raw, Table, argument,
    format_float, unique_names,
};

/// The file kinds the tools read and write.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Format {
    Csv,
    Tsv,
    Json,
    Xlsx,
}

impl Format {
    /// The format a file name's extension names. `txt` is read as delimited
    /// text, as the SDK does.
    pub(crate) fn from_name(name: &str) -> DataResult<Self> {
        let extension = name
            .rsplit('/')
            .next()
            .unwrap_or(name)
            .rsplit_once('.')
            .map(|(_, e)| e.to_ascii_lowercase());
        match extension.as_deref() {
            Some("csv" | "txt") => Ok(Self::Csv),
            Some("tsv") => Ok(Self::Tsv),
            Some("json" | "jsonl" | "ndjson") => Ok(Self::Json),
            Some("xlsx" | "xlsm") => Ok(Self::Xlsx),
            Some(other @ ("xls" | "ods" | "xlsb")) => Err(DataError::Unsupported(format!(
                "`.{other}` workbooks are not supported; only `.xlsx`. Ask the user to save the file as .xlsx or .csv."
            ))),
            Some(other @ ("parquet" | "feather" | "h5" | "hdf5" | "pkl" | "pickle" | "xml")) => {
                Err(DataError::Unsupported(format!(
                    "`.{other}` files are not supported. Supported: .csv, .tsv, .txt, .json, .jsonl and .xlsx."
                )))
            }
            _ => Err(DataError::Unsupported(format!(
                "`{name}` has no recognized extension. Supported: .csv, .tsv, .txt, .json, .jsonl and .xlsx."
            ))),
        }
    }

    pub(crate) const fn media_type(self) -> &'static str {
        match self {
            Self::Csv => "text/csv",
            Self::Tsv => "text/tab-separated-values",
            Self::Json => "application/json",
            Self::Xlsx => "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
        }
    }
}

/// Where a table came from, for the answer.
#[derive(Clone, Debug, Default)]
pub(crate) struct SourceInfo {
    /// The sheet read, for a workbook.
    pub(crate) sheet: Option<String>,
    /// Every sheet of a workbook.
    pub(crate) sheets: Vec<String>,
    /// A caution the model should hear (inline strings that read blank).
    pub(crate) notes: Vec<String>,
}

pub(crate) struct Loaded {
    pub(crate) table: Table,
    pub(crate) info: SourceInfo,
}

pub(crate) fn read_table(
    bytes: &[u8],
    format: Format,
    sheet: Option<&str>,
    limits: &Limits,
    deadline: &Deadline,
) -> DataResult<Loaded> {
    if bytes.len() as u64 > limits.max_input_bytes {
        return Err(DataError::Limit(format!(
            "The file is {} bytes; the limit is {} bytes.",
            bytes.len(),
            limits.max_input_bytes
        )));
    }
    if sheet.is_some() && format != Format::Xlsx {
        return Err(argument(
            "`sheet` applies to .xlsx files only; this file has no sheets.",
        ));
    }
    match format {
        Format::Csv => delimited(bytes, None, limits, deadline),
        Format::Tsv => delimited(bytes, Some(b'\t'), limits, deadline),
        Format::Json => json(bytes, limits, deadline),
        Format::Xlsx => xlsx(bytes, sheet, limits, deadline),
    }
    .map(|table| Loaded {
        table: table.0,
        info: table.1,
    })
}

type Parsed = (Table, SourceInfo);

// ---------------------------------------------------------------------------
// Delimited text
// ---------------------------------------------------------------------------

fn decode(bytes: &[u8]) -> DataResult<&str> {
    if bytes.starts_with(&[0xFF, 0xFE]) || bytes.starts_with(&[0xFE, 0xFF]) {
        return Err(DataError::Malformed(
            "The file is UTF-16 text. Only UTF-8 is read; ask the user to save it as UTF-8.".into(),
        ));
    }
    let body = bytes.strip_prefix(&[0xEF, 0xBB, 0xBF]).unwrap_or(bytes);
    std::str::from_utf8(body).map_err(|e| {
        DataError::Malformed(format!(
            "The file is not valid UTF-8 (first bad byte at offset {}). Ask the user to save it as UTF-8.",
            e.valid_up_to()
        ))
    })
}

/// The delimiter the header line uses most (outside quotes); a comma when it
/// names none. The header is the one line that cannot be a continuation of a
/// quoted field.
fn sniff(text: &str) -> u8 {
    let header = text.lines().find(|l| !l.trim().is_empty()).unwrap_or("");
    let mut best = (b',', 0usize);
    for candidate in *b",;\t|" {
        let mut quoted = false;
        let mut n = 0usize;
        for b in header.bytes() {
            if b == b'"' {
                quoted = !quoted;
            } else if b == candidate && !quoted {
                n += 1;
            }
        }
        if n > best.1 {
            best = (candidate, n);
        }
    }
    best.0
}

/// Split RFC 4180 text into records. `visit` gets each record's fields as it
/// completes; returning an error stops the parse.
fn records(
    text: &str,
    delimiter: u8,
    limits: &Limits,
    deadline: &Deadline,
    mut visit: impl FnMut(Vec<String>, usize) -> DataResult<()>,
) -> DataResult<()> {
    let bytes = text.as_bytes();
    let (mut i, mut line) = (0usize, 1usize);
    let mut fields: Vec<String> = Vec::new();
    let mut field: Vec<u8> = Vec::new();
    let mut quoted = false;
    let mut quote_line = 0usize;
    let mut record_started = false;
    let mut count = 0usize;
    let finish_field = |field: &mut Vec<u8>, fields: &mut Vec<String>| -> DataResult<()> {
        if fields.len() >= limits.max_columns {
            return Err(DataError::Limit(format!(
                "The file has more than {} columns, the limit.",
                limits.max_columns
            )));
        }
        // The field is a slice of valid UTF-8 cut at ASCII delimiters.
        fields.push(String::from_utf8_lossy(field).into_owned());
        field.clear();
        Ok(())
    };
    while i < bytes.len() {
        let b = bytes[i];
        if field.len() > limits.max_cell_bytes {
            return Err(DataError::Limit(format!(
                "A cell on line {line} is longer than {} bytes.",
                limits.max_cell_bytes
            )));
        }
        if quoted {
            if b == b'"' {
                if bytes.get(i + 1) == Some(&b'"') {
                    field.push(b'"');
                    i += 2;
                    continue;
                }
                quoted = false;
            } else {
                if b == b'\n' {
                    line += 1;
                }
                field.push(b);
            }
            i += 1;
            continue;
        }
        match b {
            b'"' if field.is_empty() => {
                quoted = true;
                quote_line = line;
                record_started = true;
            }
            _ if b == delimiter => {
                finish_field(&mut field, &mut fields)?;
                record_started = true;
            }
            b'\r' | b'\n' => {
                if b == b'\r' && bytes.get(i + 1) == Some(&b'\n') {
                    i += 1;
                }
                if record_started || !field.is_empty() || !fields.is_empty() {
                    finish_field(&mut field, &mut fields)?;
                    count += 1;
                    if count > limits.max_rows.saturating_add(1) {
                        return Err(DataError::Limit(format!(
                            "The file has more than {} rows, the limit.",
                            limits.max_rows
                        )));
                    }
                    if count.is_multiple_of(CHECK_EVERY) {
                        deadline.check()?;
                    }
                    visit(std::mem::take(&mut fields), line)?;
                }
                record_started = false;
                line += 1;
            }
            _ => {
                field.push(b);
                record_started = true;
            }
        }
        i += 1;
    }
    if quoted {
        return Err(DataError::Malformed(format!(
            "The quoted field that starts on line {quote_line} is never closed. The file is damaged or is not delimited text."
        )));
    }
    if record_started || !field.is_empty() || !fields.is_empty() {
        finish_field(&mut field, &mut fields)?;
        visit(std::mem::take(&mut fields), line)?;
    }
    Ok(())
}

fn delimited(
    bytes: &[u8],
    forced: Option<u8>,
    limits: &Limits,
    deadline: &Deadline,
) -> DataResult<Parsed> {
    let text = decode(bytes)?;
    let delimiter = forced.unwrap_or_else(|| sniff(text));
    let mut header: Option<Vec<String>> = None;
    let mut cells: Vec<Vec<Raw>> = Vec::new();
    let mut rows = 0usize;
    records(text, delimiter, limits, deadline, |record, line| {
        let Some(names) = &header else {
            cells = (0..record.len()).map(|_| Vec::new()).collect();
            header = Some(record);
            return Ok(());
        };
        if record.len() > names.len() {
            return Err(DataError::Malformed(format!(
                "Line {line} has {} fields but the header has {}. The file is ragged or uses another delimiter.",
                record.len(),
                names.len()
            )));
        }
        rows += 1;
        if rows.saturating_mul(names.len()) > limits.max_cells {
            return Err(DataError::Limit(format!(
                "The file has more than {} cells, the limit.",
                limits.max_cells
            )));
        }
        let mut record = record.into_iter();
        for column in &mut cells {
            column.push(record.next().map_or(Raw::Null, Raw::Text));
        }
        Ok(())
    })?;
    let Some(header) = header else {
        return Err(DataError::Malformed("The file is empty.".into()));
    };
    deadline.check()?;
    let names = unique_names(header);
    let columns = cells.into_iter().map(Column::from_raw).collect();
    Ok((Table::new(names, columns), SourceInfo::default()))
}

// ---------------------------------------------------------------------------
// JSON
// ---------------------------------------------------------------------------

fn json_cell(value: &Value) -> Raw {
    match value {
        Value::Null => Raw::Null,
        Value::Bool(b) => Raw::Bool(*b),
        Value::Number(n) => n
            .as_i64()
            .map(Raw::Int)
            .or_else(|| n.as_f64().map(Raw::Float))
            .unwrap_or(Raw::Null),
        Value::String(s) => Raw::Text(s.clone()),
        other => Raw::Text(other.to_string()),
    }
}

/// An array of records, or one record per line.
fn json(bytes: &[u8], limits: &Limits, deadline: &Deadline) -> DataResult<Parsed> {
    let text = decode(bytes)?;
    let trimmed = text.trim_start();
    let bad = |e: serde_json::Error| {
        DataError::Malformed(format!(
            "The file is not valid JSON (line {}, column {}): {}.",
            e.line(),
            e.column(),
            e.classify_name()
        ))
    };
    let records: Vec<Value> = if trimmed.starts_with('[') {
        serde_json::from_str(trimmed).map_err(bad)?
    } else {
        let mut out = Vec::new();
        for (n, line) in trimmed.lines().enumerate() {
            if line.trim().is_empty() {
                continue;
            }
            if out.len() >= limits.max_rows {
                return Err(DataError::Limit(format!(
                    "The file has more than {} records, the limit.",
                    limits.max_rows
                )));
            }
            if n % CHECK_EVERY == 0 {
                deadline.check()?;
            }
            out.push(serde_json::from_str(line).map_err(bad)?);
        }
        out
    };
    if records.len() > limits.max_rows {
        return Err(DataError::Limit(format!(
            "The file has more than {} records, the limit.",
            limits.max_rows
        )));
    }
    let mut keys: Vec<String> = Vec::new();
    let mut index: std::collections::HashMap<String, usize> = std::collections::HashMap::new();
    for record in &records {
        let Value::Object(object) = record else {
            return Err(DataError::Malformed(
                "The JSON must be an array of objects (one object per row) or one object per line."
                    .into(),
            ));
        };
        for key in object.keys() {
            if !index.contains_key(key) {
                if keys.len() >= limits.max_columns {
                    return Err(DataError::Limit(format!(
                        "The records name more than {} different fields, the column limit.",
                        limits.max_columns
                    )));
                }
                index.insert(key.clone(), keys.len());
                keys.push(key.clone());
            }
        }
    }
    if keys.is_empty() {
        return Err(DataError::Malformed("The JSON holds no records.".into()));
    }
    if records.len().saturating_mul(keys.len()) > limits.max_cells {
        return Err(DataError::Limit(format!(
            "The file would hold more than {} cells, the limit.",
            limits.max_cells
        )));
    }
    let mut cells: Vec<Vec<Raw>> = keys
        .iter()
        .map(|_| Vec::with_capacity(records.len()))
        .collect();
    for (n, record) in records.iter().enumerate() {
        if n % CHECK_EVERY == 0 {
            deadline.check()?;
        }
        let Value::Object(object) = record else {
            continue;
        };
        for (k, key) in keys.iter().enumerate() {
            cells[k].push(object.get(key).map_or(Raw::Null, json_cell));
        }
    }
    let columns = cells.into_iter().map(Column::from_raw).collect();
    Ok((
        Table::new(unique_names(keys), columns),
        SourceInfo::default(),
    ))
}

trait ClassifyName {
    fn classify_name(&self) -> &'static str;
}

impl ClassifyName for serde_json::Error {
    fn classify_name(&self) -> &'static str {
        match self.classify() {
            serde_json::error::Category::Io => "read error",
            serde_json::error::Category::Syntax => "syntax error",
            serde_json::error::Category::Data => "unexpected value",
            serde_json::error::Category::Eof => "unexpected end of file",
        }
    }
}

// ---------------------------------------------------------------------------
// xlsx
// ---------------------------------------------------------------------------

/// Run an Office engine call, waiting a short while for a free permit: the
/// engine admits a few documents at a time per process and answers `Busy` at
/// once rather than queueing (ADR-0032 decision 3).
#[cfg(feature = "toolkit-data-analysis")]
pub(crate) fn with_office<T>(
    deadline: &Deadline,
    mut call: impl FnMut() -> Result<T, elitea_office_core::OfficeError>,
) -> DataResult<T> {
    let mut attempt = 0u32;
    loop {
        match call() {
            Err(elitea_office_core::OfficeError::Busy) if attempt < 8 => {
                attempt += 1;
                deadline.check()?;
                std::thread::sleep(std::time::Duration::from_millis(250 * u64::from(attempt)));
            }
            other => return other.map_err(|e| office(&e)),
        }
    }
}

#[cfg(feature = "toolkit-data-analysis")]
fn office(e: &elitea_office_core::OfficeError) -> DataError {
    use elitea_office_core::OfficeError as E;
    match e {
        E::Busy => DataError::Limit(
            "The document engine is busy with other work. Try again in a moment.".into(),
        ),
        E::DeadlineExceeded => DataError::TimedOut,
        E::LimitExceeded { limit, .. } => DataError::Limit(format!(
            "The workbook is over a size limit (`{limit}`); it was not opened."
        )),
        E::Unsupported(what) => DataError::Unsupported(format!(
            "The workbook uses something not supported: {what}."
        )),
        _ => DataError::Malformed(
            "The file is not a readable .xlsx workbook; it may be damaged, or not an Excel file."
                .into(),
        ),
    }
}

#[cfg(feature = "toolkit-data-analysis")]
fn xlsx(
    bytes: &[u8],
    sheet: Option<&str>,
    limits: &Limits,
    deadline: &Deadline,
) -> DataResult<Parsed> {
    use elitea_office_core::{CellRange, CellValue, Workbook};

    fn raw(value: &CellValue) -> Raw {
        match value {
            CellValue::Empty => Raw::Null,
            CellValue::Text(t) => Raw::Text(t.clone()),
            CellValue::Number(n) => number(*n),
            CellValue::Bool(b) => Raw::Bool(*b),
            CellValue::DateTime(s) => Raw::Serial(*s),
            CellValue::Error(e) => Raw::Text(e.clone()),
            CellValue::Formula { cached, formula } => match cached {
                Some(inner) => raw(inner),
                None => Raw::Text(format!("={formula}")),
            },
            _ => Raw::Null,
        }
    }

    fn number(n: f64) -> Raw {
        if n.is_finite() && n.fract() == 0.0 && n.abs() < 9_007_199_254_740_992.0 {
            Raw::Int(n as i64)
        } else {
            Raw::Float(n)
        }
    }

    let workbook = with_office(deadline, || Workbook::open(bytes, &limits.office))?;
    let names = workbook.sheet_names().map_err(|e| office(&e))?;
    let index = match sheet {
        None => 0,
        Some(wanted) => {
            let wanted = wanted.trim();
            names
                .iter()
                .position(|n| n == wanted)
                .or_else(|| {
                    let lower = wanted.to_lowercase();
                    names.iter().position(|n| n.to_lowercase() == lower)
                })
                .ok_or_else(|| {
                    DataError::Argument(format!(
                        "There is no sheet named `{wanted}`. Sheets: {}.",
                        names
                            .iter()
                            .map(|n| format!("`{n}`"))
                            .collect::<Vec<_>>()
                            .join(", ")
                    ))
                })?
        }
    };
    let mut info = SourceInfo {
        sheet: names.get(index).cloned(),
        sheets: names.clone(),
        notes: Vec::new(),
    };
    if workbook.reads_inline_strings_as_empty() {
        info.notes.push(
            "This workbook stores some text as inline strings (as openpyxl writes it); those cells read as empty here. Re-save the file in Excel if text is missing."
                .into(),
        );
    }
    let Some(range) = workbook.used_range(index).map_err(|e| office(&e))? else {
        return Err(DataError::Malformed(format!(
            "The sheet `{}` is empty.",
            info.sheet.clone().unwrap_or_default()
        )));
    };
    let CellRange {
        first_row,
        first_col,
        last_row,
        last_col,
    } = range;
    let width = usize::from(last_col - first_col) + 1;
    let height = (last_row - first_row) as usize + 1;
    if width > limits.max_columns {
        return Err(DataError::Limit(format!(
            "The sheet has {width} columns; the limit is {}.",
            limits.max_columns
        )));
    }
    if height > limits.max_rows.saturating_add(1) || height.saturating_mul(width) > limits.max_cells
    {
        return Err(DataError::Limit(format!(
            "The sheet range is {height} rows by {width} columns; the limits are {} rows and {} cells.",
            limits.max_rows, limits.max_cells
        )));
    }
    let grid = workbook.read_range(index, range).map_err(|e| office(&e))?;
    deadline.check()?;
    let mut rows = grid.iter();
    let header: Vec<String> = rows
        .next()
        .map(|row| {
            row.iter()
                .map(|c| match raw(c) {
                    Raw::Text(t) => t,
                    Raw::Int(v) => v.to_string(),
                    Raw::Float(v) => format_float(v),
                    Raw::Bool(v) => v.to_string(),
                    Raw::Serial(_) | Raw::Null => String::new(),
                })
                .collect()
        })
        .unwrap_or_default();
    let mut cells: Vec<Vec<Raw>> = (0..width).map(|_| Vec::with_capacity(height)).collect();
    for (n, row) in rows.enumerate() {
        if n % CHECK_EVERY == 0 {
            deadline.check()?;
        }
        for (c, cell) in row.iter().enumerate() {
            cells[c].push(raw(cell));
        }
    }
    let columns = cells.into_iter().map(Column::from_raw).collect();
    Ok((Table::new(unique_names(header), columns), info))
}

#[cfg(not(feature = "toolkit-data-analysis"))]
fn xlsx(_: &[u8], _: Option<&str>, _: &Limits, _: &Deadline) -> DataResult<Parsed> {
    Err(DataError::Unsupported(
        "This build cannot read .xlsx workbooks.".into(),
    ))
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

/// What a write changed that the model should hear about.
#[derive(Clone, Debug, Default)]
pub(crate) struct WriteNotes {
    /// CSV cells that began like a spreadsheet formula and got a leading `'`.
    pub(crate) neutralized_cells: usize,
}

/// The bytes of `table` in `format`, all or nothing.
pub(crate) fn write_table(
    table: &Table,
    format: Format,
    limits: &Limits,
    deadline: &Deadline,
) -> DataResult<(Vec<u8>, WriteNotes)> {
    let mut notes = WriteNotes::default();
    let bytes = match format {
        Format::Csv => delimited_bytes(table, b',', &mut notes, limits, deadline)?,
        Format::Tsv => delimited_bytes(table, b'\t', &mut notes, limits, deadline)?,
        Format::Json => json_bytes(table, limits, deadline)?,
        Format::Xlsx => xlsx_bytes(table, limits, deadline)?,
    };
    if bytes.len() as u64 > limits.max_input_bytes {
        return Err(DataError::Limit(format!(
            "The output would be {} bytes; the limit is {} bytes. Filter or aggregate first.",
            bytes.len(),
            limits.max_input_bytes
        )));
    }
    Ok((bytes, notes))
}

fn field(out: &mut Vec<u8>, text: &str, delimiter: u8) {
    let needs = text
        .bytes()
        .any(|b| b == delimiter || matches!(b, b'"' | b'\n' | b'\r'));
    if needs {
        out.push(b'"');
        for b in text.bytes() {
            if b == b'"' {
                out.push(b'"');
            }
            out.push(b);
        }
        out.push(b'"');
    } else {
        out.extend_from_slice(text.as_bytes());
    }
}

/// Text a spreadsheet would run as a formula when the CSV is opened in it.
fn looks_like_formula(text: &str) -> bool {
    match text.as_bytes().first() {
        Some(b'=' | b'@' | b'\t' | b'\r') => true,
        // A signed number is data; a sign in front of anything else starts a
        // formula in a spreadsheet.
        Some(b'-' | b'+') => {
            super::table::parse_float(text).is_none() && super::table::parse_int(text).is_none()
        }
        _ => false,
    }
}

fn delimited_bytes(
    table: &Table,
    delimiter: u8,
    notes: &mut WriteNotes,
    limits: &Limits,
    deadline: &Deadline,
) -> DataResult<Vec<u8>> {
    let mut out: Vec<u8> = Vec::new();
    for (i, name) in table.names.iter().enumerate() {
        if i > 0 {
            out.push(delimiter);
        }
        if looks_like_formula(name) {
            notes.neutralized_cells += 1;
            field(&mut out, &format!("'{name}"), delimiter);
        } else {
            field(&mut out, name, delimiter);
        }
    }
    out.push(b'\n');
    for row in 0..table.rows {
        if row % CHECK_EVERY == 0 {
            deadline.check()?;
            if out.len() as u64 > limits.max_input_bytes {
                return Err(DataError::Limit(format!(
                    "The output is over {} bytes. Filter or aggregate first.",
                    limits.max_input_bytes
                )));
            }
        }
        for (c, column) in table.columns.iter().enumerate() {
            if c > 0 {
                out.push(delimiter);
            }
            match column.cell(row) {
                Cell::Text(t) if looks_like_formula(t) => {
                    notes.neutralized_cells += 1;
                    field(&mut out, &format!("'{t}"), delimiter);
                }
                cell => field(&mut out, &cell.text(), delimiter),
            }
        }
        out.push(b'\n');
    }
    Ok(out)
}

fn json_bytes(table: &Table, limits: &Limits, deadline: &Deadline) -> DataResult<Vec<u8>> {
    let mut out: Vec<u8> = Vec::from(*b"[");
    for row in 0..table.rows {
        if row % CHECK_EVERY == 0 {
            deadline.check()?;
            if out.len() as u64 > limits.max_input_bytes {
                return Err(DataError::Limit(format!(
                    "The output is over {} bytes. Filter or aggregate first.",
                    limits.max_input_bytes
                )));
            }
        }
        if row > 0 {
            out.push(b',');
        }
        let object: serde_json::Map<String, Value> = table
            .names
            .iter()
            .zip(&table.columns)
            .map(|(n, c)| (n.clone(), c.cell(row).json()))
            .collect();
        serde_json::to_writer(&mut out, &object)
            .map_err(|_| DataError::Malformed("The rows could not be written as JSON.".into()))?;
    }
    out.push(b']');
    Ok(out)
}

#[cfg(feature = "toolkit-data-analysis")]
fn xlsx_bytes(table: &Table, limits: &Limits, deadline: &Deadline) -> DataResult<Vec<u8>> {
    use elitea_office_core::{CellValue, Workbook};

    let cells = table.cell_count() + table.width();
    let office_limits = &limits.office;
    if cells as u64 > office_limits.max_cells {
        return Err(DataError::Limit(format!(
            "The result has {cells} cells; an .xlsx holds at most {}. Save it as .csv, or filter or aggregate first.",
            office_limits.max_cells
        )));
    }
    if table.width() > 16_384 || table.rows >= 1_048_576 {
        return Err(DataError::Limit(
            "The result is larger than an Excel sheet. Save it as .csv.".into(),
        ));
    }
    let fail = |_: elitea_office_core::OfficeError| {
        DataError::Malformed("The workbook could not be written.".into())
    };
    let mut workbook = Workbook::new(office_limits);
    let header: Vec<CellValue> = table
        .names
        .iter()
        .map(|n| CellValue::Text(n.clone()))
        .collect();
    workbook.write_row(0, 0, 0, &header).map_err(fail)?;
    for row in 0..table.rows {
        if row % CHECK_EVERY == 0 {
            deadline.check()?;
        }
        for (c, column) in table.columns.iter().enumerate() {
            let value = match column.cell(row) {
                Cell::Null => continue,
                Cell::Int(v) => CellValue::Number(v as f64),
                Cell::Float(v) if v.is_finite() => CellValue::Number(v),
                Cell::Float(_) => continue,
                Cell::Bool(v) => CellValue::Bool(v),
                // The library cannot write a date cell; ISO text sorts and
                // round-trips through this tool.
                other => CellValue::Text(other.text()),
            };
            workbook
                .write_cell(0, (row + 1) as u32, c as u16, &value)
                .map_err(fail)?;
        }
    }
    let mut attempt = 0u32;
    loop {
        match workbook.save() {
            Err(elitea_office_core::OfficeError::Busy) if attempt < 8 => {
                attempt += 1;
                deadline.check()?;
                std::thread::sleep(std::time::Duration::from_millis(250 * u64::from(attempt)));
            }
            Err(elitea_office_core::OfficeError::LimitExceeded { .. }) => {
                return Err(DataError::Limit(
                    "The workbook would be over a size limit. Save it as .csv, or filter first."
                        .into(),
                ));
            }
            Err(elitea_office_core::OfficeError::Busy) => {
                return Err(DataError::Limit(
                    "The document engine is busy with other work. Try again in a moment.".into(),
                ));
            }
            Err(_) => {
                return Err(DataError::Malformed(
                    "The workbook could not be written.".into(),
                ));
            }
            Ok(bytes) => return Ok(bytes),
        }
    }
}

#[cfg(not(feature = "toolkit-data-analysis"))]
fn xlsx_bytes(_: &Table, _: &Limits, _: &Deadline) -> DataResult<Vec<u8>> {
    Err(DataError::Unsupported(
        "This build cannot write .xlsx workbooks.".into(),
    ))
}

/// The dtype names of a table, for the answer.
pub(crate) fn dtype_names(table: &Table) -> Vec<&'static str> {
    table.columns.iter().map(|c| c.dtype().name()).collect()
}
