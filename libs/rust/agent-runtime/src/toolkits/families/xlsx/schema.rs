//! The tool names, descriptions and argument schemas of the `xlsx` family.
//!
//! The SDK has no `xlsx` or `excel` toolkit (its `pandas` toolkit writes
//! `.xlsx` through `df.to_excel` and is not superseded here), so every name is
//! this family's own (ADR-0032 P4). A sheet is addressed by its name or its
//! 1-based number (default: the first), a cell by its A1 reference, a range by
//! `A1:C5`.

use serde_json::{Value, json};

use super::super::office_common::{
    OfficeTool, catalogue_tools, file_param, object, out_param, text_param,
};

/// One tool of the family.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum XlsxTool {
    CreateWorkbook,
    ListSheets,
    AddSheet,
    RenameSheet,
    DeleteSheet,
    ReadRange,
    SheetToMarkdown,
    WriteCells,
    ClearRange,
    AddChart,
}

/// Every tool, in listing order.
pub(crate) const ALL_TOOLS: [XlsxTool; 10] = [
    XlsxTool::CreateWorkbook,
    XlsxTool::ListSheets,
    XlsxTool::AddSheet,
    XlsxTool::RenameSheet,
    XlsxTool::DeleteSheet,
    XlsxTool::ReadRange,
    XlsxTool::SheetToMarkdown,
    XlsxTool::WriteCells,
    XlsxTool::ClearRange,
    XlsxTool::AddChart,
];

/// The chart types a worksheet can take.
pub(crate) const CHART_TYPES: &[&str] = &[
    "bar", "column", "line", "pie", "doughnut", "area", "scatter",
];

/// The most cells one write takes.
pub(crate) const MAX_WRITE_CELLS: usize = 10_000;
/// The most cells one read returns.
pub(crate) const MAX_READ_CELLS: u64 = 20_000;

impl XlsxTool {
    pub(crate) const fn name(self) -> &'static str {
        match self {
            Self::CreateWorkbook => "create_workbook",
            Self::ListSheets => "list_sheets",
            Self::AddSheet => "add_sheet",
            Self::RenameSheet => "rename_sheet",
            Self::DeleteSheet => "delete_sheet",
            Self::ReadRange => "read_range",
            Self::SheetToMarkdown => "sheet_to_markdown",
            Self::WriteCells => "write_cells",
            Self::ClearRange => "clear_range",
            Self::AddChart => "add_chart",
        }
    }

    /// Whether the tool leaves every artifact as it was.
    pub(crate) const fn is_read_only(self) -> bool {
        matches!(
            self,
            Self::ListSheets | Self::ReadRange | Self::SheetToMarkdown
        )
    }

    pub(crate) const fn description(self) -> &'static str {
        match self {
            Self::CreateWorkbook => {
                "Create a new XLSX file in the bucket with one empty sheet (named Sheet1, or sheet_name). Refuses to replace an existing file unless overwrite is true. Fill it with write_cells."
            }
            Self::ListSheets => {
                "List the sheets of an XLSX file: number, name, the used range (the smallest rectangle that holds values), how many cells hold a value, and how many charts."
            }
            Self::AddSheet => {
                "Add an empty sheet at the end of an XLSX file. Sheet names are at most 31 characters and unique ignoring case."
            }
            Self::RenameSheet => {
                "Rename a sheet of an XLSX file. Refused when a formula or a chart refers to the sheet by name, because those references are not rewritten and would break."
            }
            Self::DeleteSheet => {
                "Delete a sheet of an XLSX file, with its cells and charts. Refused when a formula or a chart elsewhere refers to the sheet, and for the last sheet."
            }
            Self::ReadRange => {
                "Read a range of an XLSX file (A1:D20) as a Markdown table with row numbers and column letters, or as JSON with typed values. A formula shows its last calculated value and is listed with its text; this tool does not calculate. Dates are Excel serial numbers. At most 20000 cells are returned."
            }
            Self::SheetToMarkdown => {
                "Read the used range of one sheet of an XLSX file as a Markdown table with row numbers and column letters. A preview for reading: there is no rendering or PDF for spreadsheets. At most 20000 cells are returned and a longer sheet says so."
            }
            Self::WriteCells => {
                "Write cells of an XLSX file, all or none: give `cells` (each a cell like B2 with a value, or a formula) and/or `start_cell` with `values` (rows of values written from that cell). A string is text (it is never turned into a number), a number is a number, true/false is a boolean, null clears the cell. A formula is stored, not calculated: its value appears when the file is opened in a spreadsheet application. Error values and dates cannot be written (write a date as its Excel serial number)."
            }
            Self::ClearRange => {
                "Clear every cell of a range of an XLSX file (values and formulas). Formatting is not changed."
            }
            Self::AddChart => {
                "Add a native chart to a sheet of an XLSX file from cell ranges: bar, column, line, pie, doughnut, area or scatter. Give the series as ranges of values (B2:B9, one per series, with an optional name) and optionally the category labels as a range (A2:A9). The chart is drawn where `anchor` says (default: right of the used range). There is no rendering of spreadsheets, so the chart cannot be previewed."
            }
        }
    }

    #[allow(clippy::too_many_lines)]
    pub(crate) fn parameters_schema(self) -> Value {
        let file = || ("file_name", file_param("XLSX file name in the bucket"));
        let output = || {
            (
                "output_file_name",
                out_param(
                    "Output XLSX file name. Defaults to file_name (the file is edited in place).",
                ),
            )
        };
        let sheet = || {
            (
                "sheet",
                json!({"type": ["string", "integer"], "description": "The sheet's name, or its number from 1 (default: the first sheet)."}),
            )
        };
        match self {
            Self::CreateWorkbook => object(
                &[
                    (
                        "file_name",
                        file_param("Name of the XLSX file to create in the bucket"),
                    ),
                    (
                        "sheet_name",
                        text_param("Name of the first sheet (default Sheet1).", 31),
                    ),
                    (
                        "overwrite",
                        json!({"type": "boolean", "description": "Replace the file if it exists (default false)."}),
                    ),
                ],
                &["file_name"],
            ),
            Self::ListSheets => object(&[file()], &["file_name"]),
            Self::AddSheet => object(
                &[
                    file(),
                    ("name", text_param("The new sheet's name.", 31)),
                    output(),
                ],
                &["file_name", "name"],
            ),
            Self::RenameSheet => object(
                &[
                    file(),
                    sheet(),
                    ("new_name", text_param("The new name.", 31)),
                    output(),
                ],
                &["file_name", "sheet", "new_name"],
            ),
            Self::DeleteSheet => object(&[file(), sheet(), output()], &["file_name", "sheet"]),
            Self::ReadRange => object(
                &[
                    file(),
                    sheet(),
                    (
                        "range",
                        json!({"type": "string", "maxLength": 40, "description": "The range, such as A1:D20 or B2. Default: the sheet's used range."}),
                    ),
                    (
                        "format",
                        json!({"type": "string", "enum": ["markdown", "json"], "description": "markdown (default) or json."}),
                    ),
                ],
                &["file_name"],
            ),
            Self::SheetToMarkdown => object(&[file(), sheet()], &["file_name"]),
            Self::WriteCells => object(
                &[
                    file(),
                    sheet(),
                    (
                        "cells",
                        json!({"type": "array", "minItems": 1, "maxItems": MAX_WRITE_CELLS, "items": {
                            "type": "object",
                            "properties": {
                                "cell": {"type": "string", "maxLength": 12, "description": "The cell, such as B2."},
                                "value": {"type": ["string", "number", "boolean", "null"], "description": "The value; null clears the cell."},
                                "formula": {"type": "string", "maxLength": 8192, "description": "A formula such as SUM(B2:B9), with or without the leading =. Instead of value."}
                            },
                            "required": ["cell"],
                            "additionalProperties": false
                        }, "description": "Cells to write."}),
                    ),
                    (
                        "start_cell",
                        json!({"type": "string", "maxLength": 12, "description": "Where `values` start (the top-left cell), such as A1."}),
                    ),
                    (
                        "values",
                        json!({"type": "array", "minItems": 1, "maxItems": 2_000, "items": {"type": "array", "minItems": 1, "maxItems": 200, "items": {"type": ["string", "number", "boolean", "null"]}}, "description": "Rows of values, written from start_cell to the right and downwards."}),
                    ),
                    output(),
                ],
                &["file_name"],
            ),
            Self::ClearRange => object(
                &[
                    file(),
                    sheet(),
                    (
                        "range",
                        json!({"type": "string", "maxLength": 40, "description": "The range to clear, such as A2:D20."}),
                    ),
                    output(),
                ],
                &["file_name", "range"],
            ),
            Self::AddChart => object(
                &[
                    file(),
                    sheet(),
                    (
                        "chart_type",
                        json!({"type": "string", "enum": CHART_TYPES, "description": "The chart type (default column)."}),
                    ),
                    (
                        "series",
                        json!({"type": "array", "minItems": 1, "maxItems": 20, "items": {"type": "object", "properties": {"name": {"type": "string", "maxLength": 200}, "values": {"type": "string", "maxLength": 80, "description": "The range of values, such as B2:B9 (or Sheet2!B2:B9)."}}, "required": ["values"], "additionalProperties": false}, "description": "The series: a range of values and an optional name."}),
                    ),
                    (
                        "categories",
                        json!({"type": "string", "maxLength": 80, "description": "The range of category labels, such as A2:A9."}),
                    ),
                    ("title", text_param("Chart title.", 500)),
                    (
                        "legend",
                        json!({"type": "boolean", "description": "Show a legend (default true)."}),
                    ),
                    (
                        "data_labels",
                        json!({"type": "boolean", "description": "Show the value on each point."}),
                    ),
                    (
                        "anchor",
                        json!({"type": "string", "maxLength": 12, "description": "The cell of the chart's top-left corner (default: two columns right of the used range)."}),
                    ),
                    (
                        "width",
                        json!({"type": "integer", "minimum": 100, "maximum": 4000, "description": "Width in pixels (default 480)."}),
                    ),
                    (
                        "height",
                        json!({"type": "integer", "minimum": 100, "maximum": 4000, "description": "Height in pixels (default 288)."}),
                    ),
                    output(),
                ],
                &["file_name", "series"],
            ),
        }
    }

    /// The argument schema as the toolkit editor's catalogue carries it.
    pub(crate) fn catalogue_schema(self) -> Value {
        super::super::office_common::catalogue_schema(self.name(), self.parameters_schema())
    }
}

impl OfficeTool for XlsxTool {
    const FAMILY: &'static str = "xlsx";
    const ALL: &'static [Self] = &ALL_TOOLS;

    fn name(self) -> &'static str {
        XlsxTool::name(self)
    }

    fn description(self) -> &'static str {
        XlsxTool::description(self)
    }
}

/// This family's catalogue entry for `rust_only_toolkit_tools.json`: the type
/// has no SDK counterpart, so the entry carries the type and all its tools.
pub(crate) fn type_entry() -> Value {
    super::super::office_common::type_entry(
        "xlsx",
        "XLSX",
        "Bucket name where XLSX files are stored",
        &["spreadsheet", "excel", "office automation"],
        catalogue_tools::<XlsxTool>(|_| false, XlsxTool::parameters_schema),
    )
}

/// Every tool this family serves, with whether it needs the run's file store
/// (`materialize::omitted_selection` reads it for the run's notice).
pub(crate) fn tool_catalog() -> Vec<(&'static str, bool)> {
    ALL_TOOLS.iter().map(|tool| (tool.name(), true)).collect()
}
