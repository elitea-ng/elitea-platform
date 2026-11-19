//! CSV for the analysis datasets (RFC 4180: comma separated, double-quoted
//! cells, `""` for a quote inside one, any newline kind between rows).
//!
//! The writer guards against spreadsheet formula injection: a text cell that
//! starts with `=`, `+`, `-`, `@`, a tab or a carriage return and is not a
//! number is written with a leading `'`, so a Jira summary such as
//! `=HYPERLINK(...)` is data in Excel, not a formula. Numbers and dates are
//! untouched. The reader does not undo it.

use super::table::Table;

/// Why a table could not be written or read.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum CsvError {
    /// The file would be, or is, larger than the cap.
    TooLarge,
    /// The file has more rows than the cap.
    TooManyRows,
    /// The file has no header row, or a quote is never closed.
    Malformed,
}

/// Write a table as CSV, refusing a file over `max_bytes`.
pub(crate) fn write(table: &Table, max_bytes: usize) -> Result<Vec<u8>, CsvError> {
    let mut out = String::new();
    write_row(&mut out, table.columns.iter().map(String::as_str), false);
    for row in &table.rows {
        if out.len() > max_bytes {
            return Err(CsvError::TooLarge);
        }
        write_row(&mut out, row.iter().map(String::as_str), true);
    }
    if out.len() > max_bytes {
        return Err(CsvError::TooLarge);
    }
    Ok(out.into_bytes())
}

fn write_row<'a>(out: &mut String, cells: impl Iterator<Item = &'a str>, guard: bool) {
    for (position, cell) in cells.enumerate() {
        if position > 0 {
            out.push(',');
        }
        write_cell(out, cell, guard);
    }
    out.push('\n');
}

fn write_cell(out: &mut String, cell: &str, guard: bool) {
    let guarded = guard && starts_like_a_formula(cell);
    let needs_quotes = guarded || cell.contains([',', '"', '\n', '\r']);
    if !needs_quotes {
        out.push_str(cell);
        return;
    }
    out.push('"');
    if guarded {
        out.push('\'');
    }
    for character in cell.chars() {
        if character == '"' {
            out.push('"');
        }
        out.push(character);
    }
    out.push('"');
}

fn starts_like_a_formula(cell: &str) -> bool {
    cell.starts_with(['=', '+', '-', '@', '\t', '\r']) && cell.trim().parse::<f64>().is_err()
}

/// Read CSV bytes into a table. A row with fewer cells than the header is
/// padded; a longer one is cut. At most `max_rows` data rows.
pub(crate) fn read(bytes: &[u8], max_rows: usize) -> Result<Table, CsvError> {
    let text = String::from_utf8_lossy(bytes);
    let text = text.strip_prefix('\u{feff}').unwrap_or(&text);
    let mut records = parse_records(text)?.into_iter();
    let header = records.next().ok_or(CsvError::Malformed)?;
    if header.iter().all(String::is_empty) {
        return Err(CsvError::Malformed);
    }
    let mut table = Table::new(&header);
    for record in records {
        if table.rows.len() >= max_rows {
            return Err(CsvError::TooManyRows);
        }
        table.push(record);
    }
    Ok(table)
}

fn parse_records(text: &str) -> Result<Vec<Vec<String>>, CsvError> {
    let mut records: Vec<Vec<String>> = Vec::new();
    let mut record: Vec<String> = Vec::new();
    let mut cell = String::new();
    let mut quoted = false;
    let mut cell_started = false;
    let mut characters = text.chars().peekable();
    while let Some(character) = characters.next() {
        if quoted {
            if character == '"' {
                if characters.peek() == Some(&'"') {
                    characters.next();
                    cell.push('"');
                } else {
                    quoted = false;
                }
            } else {
                cell.push(character);
            }
            continue;
        }
        match character {
            '"' if cell.is_empty() && !cell_started => {
                quoted = true;
                cell_started = true;
            }
            ',' => {
                record.push(std::mem::take(&mut cell));
                cell_started = false;
            }
            '\r' | '\n' => {
                if character == '\r' && characters.peek() == Some(&'\n') {
                    characters.next();
                }
                if cell_started || !cell.is_empty() || !record.is_empty() {
                    record.push(std::mem::take(&mut cell));
                    records.push(std::mem::take(&mut record));
                }
                cell_started = false;
            }
            other => {
                cell.push(other);
                cell_started = true;
            }
        }
    }
    if quoted {
        return Err(CsvError::Malformed);
    }
    if cell_started || !cell.is_empty() || !record.is_empty() {
        record.push(cell);
        records.push(record);
    }
    Ok(records)
}

#[cfg(test)]
mod tests {
    use super::{CsvError, read, write};
    use crate::toolkits::families::analysis_core::table::Table;

    fn row(cells: &[&str]) -> Vec<String> {
        cells.iter().map(|cell| (*cell).to_owned()).collect()
    }

    #[test]
    fn a_table_with_commas_quotes_and_newlines_round_trips() {
        let mut table = Table::new(&["key", "summary"]);
        table.push(row(&["A-1", "plain"]));
        table.push(row(&["A-2", "a, b"]));
        table.push(row(&["A-3", "say \"hi\"\nthen go"]));
        table.push(row(&["A-4", ""]));
        let bytes = write(&table, 10_000).expect("written");
        assert_eq!(read(&bytes, 100).expect("read"), table);
    }

    #[test]
    fn a_formula_looking_text_cell_is_defanged_and_a_number_is_not() {
        let mut table = Table::new(&["a"]);
        for cell in [
            "=1+1",
            "+cmd",
            "-x",
            "@sum",
            "-5",
            "+2.5",
            "2026-01-01 10:00:00",
        ] {
            table.push(row(&[cell]));
        }
        let text = String::from_utf8(write(&table, 10_000).expect("written")).expect("utf8");
        let lines: Vec<&str> = text.lines().collect();
        assert_eq!(
            lines,
            [
                "a",
                "\"'=1+1\"",
                "\"'+cmd\"",
                "\"'-x\"",
                "\"'@sum\"",
                "-5",
                "+2.5",
                "2026-01-01 10:00:00"
            ]
        );
    }

    #[test]
    fn the_header_is_never_defanged() {
        let table = Table::new(&["-x"]);
        assert_eq!(write(&table, 100).expect("written"), b"-x\n");
    }

    #[test]
    fn a_file_over_the_byte_cap_is_refused() {
        let mut table = Table::new(&["a"]);
        for _ in 0..100 {
            table.push(row(&["0123456789"]));
        }
        assert_eq!(write(&table, 200), Err(CsvError::TooLarge));
        assert!(write(&table, 2_000).is_ok());
    }

    #[test]
    fn reading_accepts_crlf_a_bom_and_ragged_rows() {
        let bytes = "\u{feff}a,b\r\n1\r\n2,3,4\r\n\r\n";
        let table = read(bytes.as_bytes(), 10).expect("read");
        assert_eq!(table.columns, ["a", "b"]);
        assert_eq!(table.rows, [row(&["1", ""]), row(&["2", "3"])]);
    }

    #[test]
    fn an_empty_file_an_unclosed_quote_and_too_many_rows_are_refused() {
        assert_eq!(read(b"", 10), Err(CsvError::Malformed));
        assert_eq!(read(b"a,b\n\"x,y\n", 10), Err(CsvError::Malformed));
        assert_eq!(read(b"a\n1\n2\n3\n", 2), Err(CsvError::TooManyRows));
    }

    #[test]
    fn an_empty_quoted_cell_is_an_empty_cell() {
        let table = read(b"a,b\n\"\",x\n", 10).expect("read");
        assert_eq!(table.rows, [row(&["", "x"])]);
    }
}
