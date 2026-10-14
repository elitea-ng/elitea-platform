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

/// The default bucket: where chat attachments are stored.
pub(crate) const DEFAULT_BUCKET: &str = "chat-attachments";

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
                "Profile a table file: its columns with types, row count, missing values, distinct counts, numeric summaries (mean, std, min, quartiles, max), top text values, date ranges, and the first rows. Always start here for a file you have not seen. Works on CSV, TSV, JSON and Excel (.xlsx; pass `sheet`, or see `sheets` in the answer). The file is in the conversation attachments bucket unless `bucket_name` says otherwise."
            }
            Self::FilterData => {
                "Keep the rows of a table file that match conditions, optionally pick columns, sort, and limit. Conditions are {column, operator, value}; operators: eq, ne, gt, ge, lt, le, contains, not_contains, starts_with, ends_with, in, not_in, is_null, not_null, matches (regex). Missing cells match only is_null. The answer shows the first rows and the total; pass `output_file` to save every matching row as .csv, .tsv, .json or .xlsx next to the source."
            }
            Self::SortData => {
                "Sort a table file by one or more columns (missing values last) and show the first rows. Optional `filters` narrow the rows first. Pass `output_file` to save the whole sorted table next to the source."
            }
            Self::AggregateData => {
                "Group rows and summarise them: group_by columns (omit to summarise the whole table) and aggregations {column, function, name}. Functions: count, sum, mean, median, min, max, std, var, nunique, first, last. Use column `*` with count to count rows. Groups come out sorted by key; rows with a missing key are left out. Optional `filters` narrow the rows first; `sort_by` and `limit` shape the result; `output_file` saves it."
            }
            Self::PivotData => {
                "Pivot a table: one row per `index` combination, one column per distinct value of `columns`, each cell the `function` (default sum) of `values`. Cells with no rows are empty, or `fill_value`. Optional `filters` first. A pivot with more than 500 distinct column values is refused; filter or aggregate instead. `output_file` saves the result."
            }
            Self::JoinData => {
                "Join two table files on key columns: `on` (same column names in both) or `left_on` and `right_on`. `how` is inner (default), left, right or outer. Rows whose key is missing never match. Same-named non-key columns get the suffixes `_x` and `_y`. A join that would produce more than 1,000,000 rows is refused. `output_file` saves the joined table next to the first file."
            }
            Self::CorrelateData => {
                "Correlation matrix between the numeric columns (Pearson, or Spearman rank correlation) using the rows where both values exist. Pass `columns` to choose; at most 50. Optional `filters` first."
            }
            Self::ChartData => {
                "Draw a chart from a table file and save it as a PNG artifact. chart_type: bar, line, scatter, pie or histogram. For bar, line and pie give `x` (categories) and `y` (one or more numeric columns); repeated x values are combined with `aggregation` (default sum). For scatter give numeric `x` and `y`. For histogram give `y` (one numeric column) and `bins`. Optional `filters`, `sort_by`, `top_n` (keep the largest categories). The answer includes a Markdown image to embed. Pass `output_file` to name the PNG."
            }
            Self::SaveDataframe => {
                "Convert a table file to another format, saved in the same bucket. The target's extension picks the format: .csv, .tsv, .json (a list of records) or .xlsx. Fails if the target exists unless `overwrite` is true."
            }
            Self::RunCode => {
                "Run your own short Python or JavaScript on rows from a table file, for analysis the other tools cannot express. The code runs in an isolated sandbox with no network and no files. Python reads `elitea_state['data']`; JavaScript is a module whose default export (or the result of a default-exported function of the state) is the answer, and reads `elitea_state.data`. `data` is an object {columns, types, rows, total_rows, truncated} where rows is a list of lists. The final Python expression is the answer. The answer must be JSON, at most 100 KiB; a larger answer is refused, so summarise inside the program. At most 50,000 cells are sent: choose `columns`, `filters` and `max_rows` so the rows you need fit; the answer says if rows were cut. The sandbox has no pandas."
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
                        "description": "The source file name, as shown in the attachment message (for example `<conversation>/sales.csv`)."}),
                );
                add(
                    "target_file",
                    json!({"type": "string", "minLength": 1, "maxLength": 1024,
                        "description": "The file to create; its extension picks the format. A bare name is placed next to the source."}),
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
                add("columns", columns("Only profile these columns."));
                add(
                    "head_rows",
                    json!({"type": "integer", "minimum": 0, "maximum": 50, "default": 5, "description": "How many leading rows to show."}),
                );
            }
            Self::FilterData | Self::SortData => {
                add("filters", filters());
                add("match", match_mode());
                add("case_sensitive", case_sensitive());
                add("columns", columns("The columns to keep, in this order."));
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
                    columns("Columns to group by. Omit to summarise the whole table."),
                );
                add(
                    "aggregations",
                    json!({"type": "array", "minItems": 1, "maxItems": 100,
                    "description": "What to compute.",
                    "items": {"type": "object", "properties": {
                        "column": {"type": "string", "description": "Column name, or `*` with count to count rows."},
                        "function": {"type": "string", "enum": Func::NAMES},
                        "name": {"type": "string", "description": "Name of the result column (default `<column>_<function>`)."}},
                        "required": ["column", "function"]}}),
                );
                required.push("aggregations");
                add("filters", filters());
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
                    json!({"type": "string", "description": "The column whose distinct values become columns."}),
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
                    json!({"type": "number", "description": "What to put in cells with no rows (default empty)."}),
                );
                required.extend(["index", "columns", "values"]);
                add("filters", filters());
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
                add(
                    "on",
                    columns("Key columns present, by the same name, in both files."),
                );
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
                    columns(
                        "Numeric columns to correlate (default: all numeric columns, at most 50).",
                    ),
                );
                add(
                    "method",
                    json!({"type": "string", "enum": ["pearson", "spearman"], "default": "pearson"}),
                );
                add("filters", filters());
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
                    json!({"type": "string", "description": "Category or x column (not used by histogram)."}),
                );
                add(
                    "y",
                    columns("Value column(s). Scatter and histogram take one."),
                );
                add(
                    "aggregation",
                    json!({"type": "string", "enum": Func::NAMES, "default": "sum", "description": "How repeated x values are combined."}),
                );
                add(
                    "bins",
                    json!({"type": "integer", "minimum": 2, "maximum": 100, "default": 20}),
                );
                add(
                    "top_n",
                    json!({"type": "integer", "minimum": 1, "maximum": 100, "description": "Keep the n categories with the largest total."}),
                );
                add("title", json!({"type": "string", "maxLength": 200}));
                add("x_label", json!({"type": "string", "maxLength": 100}));
                add("y_label", json!({"type": "string", "maxLength": 100}));
                add("filters", filters());
                add("match", match_mode());
                add("case_sensitive", case_sensitive());
                add("sort_by", sort_by());
                add(
                    "output_file",
                    json!({"type": "string", "maxLength": 1024, "description": "Name of the PNG (default chart_<id>.png, next to the source)."}),
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
                add("filters", filters());
                add("match", match_mode());
                add("case_sensitive", case_sensitive());
                add(
                    "max_rows",
                    json!({"type": "integer", "minimum": 1, "maximum": 50_000, "description": "Send at most this many rows (the first ones)."}),
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
        "description": format!("{what}: its name in the bucket, as shown in the attachment message (for example `<conversation>/sales.csv`). CSV, TSV, TXT, JSON, JSONL and .xlsx are read.")})
}

fn sheet() -> Value {
    json!({"type": "string", "description": "Sheet name, for an .xlsx file (default: the first sheet)."})
}

fn bucket() -> Value {
    json!({"type": "string", "default": DEFAULT_BUCKET, "description": "The bucket the file is in (default: the conversation attachments bucket)."})
}

fn columns(description: &str) -> Value {
    json!({"type": "array", "items": {"type": "string"}, "maxItems": 200, "description": description})
}

fn filters() -> Value {
    json!({"type": "array", "maxItems": 50,
        "description": "Conditions on the rows. Dates are ISO text such as 2024-01-31.",
        "items": {"type": "object", "properties": {
            "column": {"type": "string"},
            "operator": {"type": "string", "enum": Op::NAMES},
            "value": {"description": "The value to compare with; a list for in and not_in; omitted for is_null and not_null."}},
            "required": ["column", "operator"]}})
}

fn match_mode() -> Value {
    json!({"type": "string", "enum": ["all", "any"], "default": "all",
        "description": "Whether a row must meet all of the filters or any one."})
}

fn case_sensitive() -> Value {
    json!({"type": "boolean", "default": true, "description": "Whether text comparisons respect case."})
}

fn sort_by() -> Value {
    json!({"type": "array", "maxItems": 20, "description": "Columns to sort by, first is the main key.",
        "items": {"oneOf": [{"type": "string"}, {"type": "object", "properties": {
            "column": {"type": "string"}, "descending": {"type": "boolean", "default": false}},
            "required": ["column"]}]}})
}

fn limit() -> Value {
    json!({"type": "integer", "minimum": 1, "maximum": 500, "default": 20,
        "description": "How many result rows to show in the answer. The saved file always has all of them."})
}

fn output_file() -> Value {
    json!({"type": "string", "maxLength": 1024,
        "description": "Save the whole result under this name (.csv, .tsv, .json or .xlsx). A bare name is placed next to the source file. Not saved when omitted."})
}

fn overwrite() -> Value {
    json!({"type": "boolean", "default": false, "description": "Replace the output file if it exists (default: refuse)."})
}
