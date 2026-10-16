//! The spreadsheet side of the two Excel report tools, on
//! `elitea-office-core` (values only: the library has no styling API).
//!
//! What the SDK's openpyxl workbooks carry that these do not: header and title
//! fills, borders, fonts and alignment; the `0.00%` number formats; merged
//! title value cells; the hyperlink on the Carrier report cell (its target is
//! written as the cell text instead); conditional formatting (green, yellow
//! and red bands against the response-time threshold, and the Lighthouse
//! CLS/FCP/LCP bands); column widths, the auto filter and frozen panes. Every
//! sheet, row, column and value is the SDK's.

use elitea_office_core::{CellValue, Limits, OfficeError, Workbook};

use super::lighthouse::{self, Table};
use super::perf_report::{self, Cell, Summary};

fn value(cell: &Cell) -> Option<CellValue> {
    match cell {
        Cell::Text(text) => Some(CellValue::Text(text.clone())),
        Cell::Number(number) => Some(CellValue::Number(*number)),
        Cell::Empty => None,
    }
}

type Cells = Vec<(u32, u16, CellValue)>;

fn push(cells: &mut Cells, row: usize, column: usize, cell: &Cell) {
    if let (Some(value), Ok(row), Ok(column)) =
        (value(cell), u32::try_from(row), u16::try_from(column))
    {
        cells.push((row, column, value));
    }
}

/// The `Test results` workbook of `create_excel_report`: the title block
/// (rows 1-11, label in A and value in B), the header row (row 12) and one
/// row per transaction.
pub(super) fn backend_workbook(
    summary: &Summary,
    carrier_report: &str,
    status: &str,
    justification: &str,
) -> Result<Vec<u8>, OfficeError> {
    let mut workbook = Workbook::new(&Limits::default());
    workbook.rename_sheet(0, "Test results")?;
    let mut cells = Cells::new();
    let title = perf_report::title_rows(summary, carrier_report, status, justification);
    for (row, (label, cell)) in title.iter().enumerate() {
        push(&mut cells, row, 0, &Cell::Text((*label).to_owned()));
        push(&mut cells, row, 1, cell);
    }
    let header_row = title.len();
    for (column, header) in perf_report::HEADERS.iter().enumerate() {
        push(
            &mut cells,
            header_row,
            column,
            &Cell::Text((*header).to_owned()),
        );
    }
    for (index, stats) in summary.requests.iter().enumerate() {
        for (column, cell) in perf_report::data_row(stats).iter().enumerate() {
            push(&mut cells, header_row + 1 + index, column, cell);
        }
    }
    workbook.write_cells(0, &cells)?;
    workbook.save()
}

/// The workbook of `create_ui_excel_report`: one sheet per Lighthouse JSON,
/// a `Step name` column and one column per audit.
pub(super) fn ui_workbook(tables: &[(String, Table)]) -> Result<Vec<u8>, OfficeError> {
    let mut workbook = Workbook::new(&Limits::default());
    let mut titles: Vec<String> = Vec::new();
    for (index, (wanted, table)) in tables.iter().enumerate() {
        let title = lighthouse::unique_title(&titles, wanted);
        let sheet = if index == 0 {
            workbook.rename_sheet(0, &title)?;
            0
        } else {
            workbook.add_sheet(&title)?
        };
        titles.push(title);
        let mut cells = Cells::new();
        push(&mut cells, 0, 0, &Cell::Text("Step name".to_owned()));
        for (column, audit) in table.columns.iter().enumerate() {
            push(&mut cells, 0, column + 1, &Cell::Text(audit.clone()));
        }
        for (row, (step, values)) in table.rows.iter().enumerate() {
            push(&mut cells, row + 1, 0, &Cell::Text(step.clone()));
            for (column, number) in values.iter().enumerate() {
                if let Some(number) = number {
                    push(&mut cells, row + 1, column + 1, &Cell::Number(*number));
                }
            }
        }
        workbook.write_cells(sheet, &cells)?;
    }
    workbook.save()
}
