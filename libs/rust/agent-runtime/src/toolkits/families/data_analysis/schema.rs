//! Tool names, descriptions and argument schemas of the `data_analysis`
//! internal tool (ADR-0033 decision 1).
//!
//! The SDK's one tool, `pandas_analyze_data(query, filename)`, wrote pandas
//! code with the agent's model and ran it in the worker. This tool set has no
//! such step: the agent's model calls the tools below directly, and each one is
//! a deterministic operation over a CSV, TSV, JSON or xlsx file. The only
//! free-form path is `run_code`, which hands rows to the code sandbox.
//! `save_dataframe` keeps the name and arguments of the SDK wrapper method.

use serde_json::{Value, json};

use super::ops::{Func, Op};

/// One tool of the set.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum DataTool {
    DescribeData,
    FilterData,
    SortData,
    AggregateData,
    PivotData,
    JoinData,
    CorrelateData,
    ChartData,
    SaveDataframe,
    RunCode,
}

pub(crate) const ALL_TOOLS: [DataTool; 10] = [
    DataTool::DescribeData,
    DataTool::FilterData,
    DataTool::SortData,
    DataTool::AggregateData,
    DataTool::PivotData,
    DataTool::JoinData,
    DataTool::CorrelateData,
    DataTool::ChartData,
    DataTool::SaveDataframe,
    DataTool::RunCode,
];

impl DataTool {
    pub(crate) const fn name(self) -> &'static str {
        match self {
            Self::DescribeData => "describe_data",
            Self::FilterData => "filter_data",
            Self::SortData => "sort_data",
            Self::AggregateData => "aggregate_data",
            Self::PivotData => "pivot_data",
            Self::JoinData => "join_data",
            Self::CorrelateData => "correlate_data",
            Self::ChartData => "chart_data",
            Self::SaveDataframe => "save_dataframe",
            Self::RunCode => "run_analysis_code",
        }
    }

    /// Read-only: writes nothing, so calls may run side by side.
    pub(crate) const fn is_read_only(self) -> bool {
        matches!(
            self,
            Self::DescribeData | Self::CorrelateData | Self::RunCode
        )
    }

    pub(crate) const fn description(self) -> &'static str {
        match self {
            Self::DescribeData => {
                "Profile a table file: columns and types, row count, missing and distinct counts, numeric summaries, top text values, date ranges, first rows. Start here for a file you have not seen. CSV, TSV, JSON and Excel (.xlsx: pass `sheet`, or see `sheets` in the answer). The file is in the conversation attachments bucket unless `bucket_name` says otherwise."
            }
            Self::FilterData => {
                "Keep the rows of a table file that match conditions {column, operator, value} (`matches` is a regex; `match` is all or any). Missing cells match only is_null. Optionally pick columns, sort, limit. Shows the first rows and the total; `output_file` saves every matching row. Other tools take the same `filters`."
            }
            Self::SortData => {
                "Sort a table file by one or more columns (missing values last) and show the first rows. Optional `filters` narrow first; `output_file` saves the whole sorted table."
            }
            Self::AggregateData => {
                "Group rows and summarise them: `group_by` columns (omit for the whole table) and `aggregations` {column, function, name}; column `*` with count counts rows. Groups come out sorted by key; rows with a missing key are left out. Optional `filters`, `sort_by`, `limit`; `output_file` saves the result."
            }
            Self::PivotData => {
                "Pivot a table: one row per `index` combination, one column per distinct value of `columns`, each cell the `function` (default sum) of `values`; empty cells are blank or `fill_value`. More than 500 distinct column values is refused: filter or aggregate first. `output_file` saves the result."
            }
            Self::JoinData => {
                "Join two table files on `on` (same key names in both) or `left_on` and `right_on`; `how` is inner (default), left, right or outer. Missing keys never match; same-named other columns get `_x` and `_y`. A join over 1,000,000 rows is refused. `output_file` saves the result next to the first file."
            }
            Self::CorrelateData => {
                "Correlation matrix of the numeric columns (Pearson or Spearman) over the rows where both values exist. `columns` chooses (at most 50); optional `filters`."
            }
            Self::ChartData => {
                "Draw a chart from a table file and save it as a PNG artifact. bar, line, pie: `x` (categories) and `y` (numeric columns), repeated x combined with `aggregation` (default sum). scatter: numeric `x` and `y`. histogram: `y` and `bins`. Optional `filters`, `sort_by`, `top_n` (largest categories). The answer has a Markdown image to embed; `output_file` names the PNG."
            }
            Self::SaveDataframe => {
                "Convert a table file to .csv, .tsv, .json (a list of records) or .xlsx (the target's extension picks it), saved in the same bucket. Fails if the target exists unless `overwrite` is true."
            }
            Self::RunCode => {
                "Run your own short Python or JavaScript on rows of a table file, for analysis the other tools cannot express. Isolated sandbox: no network, no files, no pandas. Python reads `elitea_state['data']`, and its final expression is the answer; JavaScript reads `elitea_state.data` and default-exports the answer (or a function of the state). `data` is {columns, types, rows, total_rows, truncated}, rows a list of lists. The answer must be JSON, at most 100 KiB, so summarise inside the program. At most 50,000 cells are sent: use `columns`, `filters`, `max_rows`; the answer says if rows were cut."
            }
        }
    }

    pub(crate) fn parameters_schema(self) -> Value {
        let mut properties = serde_json::Map::new();
        let mut required: Vec<&str> = Vec::new();
        let mut add = |name: &str, schema: Value| {
            properties.insert(name.to_owned(), schema);
        };
        match self {
            Self::SaveDataframe => {
                add(
                    "source_df",
                    json!({"type": "string", "minLength": 1, "maxLength": 1024,
                        "description": "The source file, as in the attachment message (`<conversation>/sales.csv`)."}),
                );
                add(
                    "target_file",
                    json!({"type": "string", "minLength": 1, "maxLength": 1024,
                        "description": "The file to create; a bare name goes next to the source."}),
                );
                required.extend(["source_df", "target_file"]);
                add("sheet", sheet());
                add("bucket_name", bucket());
                add("overwrite", overwrite());
            }
            _ => {
                add("filename", filename("The table file"));
                required.push("filename");
                add("sheet", sheet());
                add("bucket_name", bucket());
            }
        }
        match self {
            Self::DescribeData => {
                add("columns", columns("Only these columns."));
                add(
                    "head_rows",
                    json!({"type": "integer", "minimum": 0, "maximum": 50, "default": 5, "description": "How many leading rows to show."}),
                );
            }
            Self::FilterData | Self::SortData => {
                add(
                    "filters",
                    if self == Self::FilterData {
                        filters()
                    } else {
                        filters_short()
                    },
                );
                add("match", match_mode());
                add("case_sensitive", case_sensitive());
                add("columns", columns("Columns to keep, in order."));
                add("sort_by", sort_by());
                add("limit", limit());
                add("output_file", output_file());
                add("overwrite", overwrite());
                if self == Self::SortData {
                    required.push("sort_by");
                }
            }
            Self::AggregateData => {
                add(
                    "group_by",
                    columns("Group-by columns; omit for the whole table."),
                );
                add(
                    "aggregations",
                    json!({"type": "array", "minItems": 1, "maxItems": 100,
                    "description": "What to compute.",
                    "items": {"type": "object", "properties": {
                        "column": {"type": "string", "description": "Column, or `*` with count."},
                        "function": {"type": "string", "enum": Func::NAMES},
                        "name": {"type": "string", "description": "Result column name."}},
                        "required": ["column", "function"]}}),
                );
                required.push("aggregations");
                add("filters", filters_short());
                add("match", match_mode());
                add("case_sensitive", case_sensitive());
                add("sort_by", sort_by());
                add("limit", limit());
                add("output_file", output_file());
                add("overwrite", overwrite());
            }
            Self::PivotData => {
                add("index", columns("Columns that become the row labels."));
                add(
                    "columns",
                    json!({"type": "string", "description": "Column whose distinct values become columns."}),
                );
                add(
                    "values",
                    json!({"type": "string", "description": "The column to summarise in the cells."}),
                );
                add(
                    "function",
                    json!({"type": "string", "enum": Func::NAMES, "default": "sum"}),
                );
                add(
                    "fill_value",
                    json!({"type": "number", "description": "Value for empty cells."}),
                );
                required.extend(["index", "columns", "values"]);
                add("filters", filters_short());
                add("match", match_mode());
                add("case_sensitive", case_sensitive());
                add("limit", limit());
                add("output_file", output_file());
                add("overwrite", overwrite());
            }
            Self::JoinData => {
                add("right_filename", filename("The second table file"));
                add("right_sheet", sheet());
                add("right_bucket_name", bucket());
                add("on", columns("Key columns named the same in both files."));
                add("left_on", columns("Key columns of the first file."));
                add(
                    "right_on",
                    columns("Key columns of the second file, in the same order."),
                );
                add(
                    "how",
                    json!({"type": "string", "enum": ["inner", "left", "right", "outer"], "default": "inner"}),
                );
                add(
                    "columns",
                    columns("Keep only these columns of the joined table."),
                );
                add("sort_by", sort_by());
                add("limit", limit());
                add("output_file", output_file());
                add("overwrite", overwrite());
                required.push("right_filename");
            }
            Self::CorrelateData => {
                add(
                    "columns",
                    columns("Numeric columns (default all, at most 50)."),
                );
                add(
                    "method",
                    json!({"type": "string", "enum": ["pearson", "spearman"], "default": "pearson"}),
                );
                add("filters", filters_short());
                add("match", match_mode());
                add("case_sensitive", case_sensitive());
            }
            Self::ChartData => {
                add(
                    "chart_type",
                    json!({"type": "string", "enum": super::chart::Kind::NAMES}),
                );
                add(
                    "x",
                    json!({"type": "string", "description": "Category or x column."}),
                );
                add(
                    "y",
                    columns("Value column(s); scatter and histogram take one."),
                );
                add(
                    "aggregation",
                    json!({"type": "string", "enum": Func::NAMES, "default": "sum", "description": "How repeated x values combine."}),
                );
                add(
                    "bins",
                    json!({"type": "integer", "minimum": 2, "maximum": 100, "default": 20}),
                );
                add(
                    "top_n",
                    json!({"type": "integer", "minimum": 1, "maximum": 100, "description": "Keep the n largest categories."}),
                );
                add("title", json!({"type": "string", "maxLength": 200}));
                add("x_label", json!({"type": "string", "maxLength": 100}));
                add("y_label", json!({"type": "string", "maxLength": 100}));
                add("filters", filters_short());
                add("match", match_mode());
                add("case_sensitive", case_sensitive());
                add("sort_by", sort_by());
                add(
                    "output_file",
                    json!({"type": "string", "maxLength": 1024, "description": "PNG name (default chart_<id>.png)."}),
                );
                add("overwrite", overwrite());
                required.push("chart_type");
            }
            Self::RunCode => {
                add(
                    "language",
                    json!({"type": "string", "enum": ["python", "javascript"], "default": "python"}),
                );
                add(
                    "code",
                    json!({"type": "string", "minLength": 1, "maxLength": 100_000, "description": "The program."}),
                );
                add("columns", columns("Send only these columns."));
                add("filters", filters_short());
                add("match", match_mode());
                add("case_sensitive", case_sensitive());
                add(
                    "max_rows",
                    json!({"type": "integer", "minimum": 1, "maximum": 50_000, "description": "Send at most this many leading rows."}),
                );
                required.push("code");
            }
            Self::SaveDataframe => {}
        }
        json!({"type": "object", "properties": properties, "required": required,
               "additionalProperties": false})
    }
}

fn filename(what: &str) -> Value {
    json!({"type": "string", "minLength": 1, "maxLength": 1024,
        "description": format!("{what}, as in the attachment message (`<conversation>/sales.csv`).")})
}

fn sheet() -> Value {
    json!({"type": "string", "description": "Sheet of an .xlsx (default the first)."})
}

fn bucket() -> Value {
    json!({"type": "string", "description": "Bucket (default: this conversation's attachments)."})
}

fn columns(description: &str) -> Value {
    json!({"type": "array", "items": {"type": "string"}, "maxItems": 200, "description": description})
}

fn filters() -> Value {
    json!({"type": "array", "maxItems": 50,
        "description": "Row conditions. Dates are ISO text (2024-01-31).",
        "items": {"type": "object", "properties": {
            "column": {"type": "string"},
            "operator": {"type": "string", "enum": Op::NAMES},
            "value": {"description": "The value to compare with; a list for in and not_in; omitted for is_null and not_null."}},
            "required": ["column", "operator"]}})
}

/// The `filters` argument of every tool but `filter_data`: the same
/// conditions, without repeating the operator list in each schema (#2300);
/// `filter_data`'s description and schema carry it.
fn filters_short() -> Value {
    json!({"type": "array", "maxItems": 50,
        "description": "Row conditions {column, operator, value}, as in filter_data.",
        "items": {"type": "object"}})
}

fn match_mode() -> Value {
    json!({"type": "string", "enum": ["all", "any"], "default": "all"})
}

fn case_sensitive() -> Value {
    json!({"type": "boolean", "default": true})
}

fn sort_by() -> Value {
    json!({"type": "array", "maxItems": 20, "description": "Sort keys, main first.",
        "items": {"oneOf": [{"type": "string"}, {"type": "object", "properties": {
            "column": {"type": "string"}, "descending": {"type": "boolean", "default": false}},
            "required": ["column"]}]}})
}

fn limit() -> Value {
    json!({"type": "integer", "minimum": 1, "maximum": 500, "default": 20,
        "description": "Result rows shown; a saved file has all."})
}

fn output_file() -> Value {
    json!({"type": "string", "maxLength": 1024,
        "description": "Save the whole result here (.csv, .tsv, .json, .xlsx); a bare name goes next to the source."})
}

fn overwrite() -> Value {
    json!({"type": "boolean", "default": false, "description": "Replace an existing output file."})
}
