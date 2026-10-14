//! The `data_analysis` ADK tools: host I/O around the engine.
//!
//! Every tool follows one shape. Arguments are parsed and checked first, the
//! source bytes are read through the Office host, the work runs on the
//! blocking pool under the engine's own clock (a semaphore keeps a few
//! analyses at a time), and a result file, when one is asked for, is encoded in
//! full before a single write: a refused or failed write leaves nothing
//! behind. Every failure is a sentence in the tool result, never an error
//! that ends the turn.

use std::fmt;
use std::sync::Arc;

use adk_core::{Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::families::office_host::{
    MAX_OFFICE_OBJECT_BYTES, OfficeFailure, OfficeHostError, SharedOfficeToolHost,
};
use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::{ToolAdmissionDecision, ToolAdmissionPolicy};

use super::chart::{self, Chart, Kind, Series};
use super::code::{
    AnalysisCodeFailure, AnalysisCodeRequest, AnalysisCodeRunner, AnalysisLanguage,
    MAX_ANALYSIS_RESULT_BYTES, failure_sentence,
};
use super::formats::{self, Format, Loaded, SourceInfo, WriteNotes};
use super::ops::{self, Func, How, JoinSpec, PivotSpec};
use super::schema::{ALL_TOOLS, DEFAULT_BUCKET, DataTool};
use super::table::{Cell, Column, DataError, DataResult, Deadline, Limits, Table, argument};

/// The toolkit type the tool admission policy names this tool set by.
pub(crate) const TOOLKIT_TYPE: &str = "data_analysis";
const MAX_NAME_BYTES: usize = 1_024;
const MAX_CELL_CHARS: usize = 500;
const DEFAULT_PREVIEW_ROWS: usize = 20;
/// Analyses that may run at once in the whole worker process, whatever number
/// of runs and tool sets exist: each can hold millions of cells, so the bound
/// on memory must not grow with the number of conversations.
const CONCURRENT_ANALYSES: usize = 2;
static ANALYSIS_GATE: tokio::sync::Semaphore =
    tokio::sync::Semaphore::const_new(CONCURRENT_ANALYSES);

/// The gate, for the tests that hold it.
#[cfg(test)]
pub(crate) fn analysis_gate() -> &'static tokio::sync::Semaphore {
    &ANALYSIS_GATE
}
const MAX_CHART_CATEGORIES: usize = 100;
const MAX_SCATTER_POINTS: usize = 5_000;
/// Bytes of the request around the data: the job envelope and key names.
const SANDBOX_ENVELOPE_BYTES: usize = 4 * 1024;
/// Bytes of the `total_rows`/`truncated` keys and the object punctuation.
const SANDBOX_HEAD_BYTES: usize = 200;
/// Nodes held back from the sandbox's count for the `data` wrapper.
const SANDBOX_NODE_MARGIN: usize = 16;
const PNG_MEDIA_TYPE: &str = "image/png";

type Sentence = String;

/// What the host lends the tool set for one run: the object store (and a link
/// to its objects), and, when the deployment has one, the code sandbox.
#[derive(Clone)]
pub struct DataAnalysisAuthority {
    host: SharedOfficeToolHost,
    runner: Option<Arc<dyn AnalysisCodeRunner>>,
}

impl DataAnalysisAuthority {
    #[must_use]
    pub fn new(host: SharedOfficeToolHost) -> Self {
        Self { host, runner: None }
    }

    /// Lend the code sandbox. Without it `run_analysis_code` is not offered.
    #[must_use]
    pub fn with_code_runner(mut self, runner: Arc<dyn AnalysisCodeRunner>) -> Self {
        self.runner = Some(runner);
        self
    }
}

/// Stable construction failure.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct DataAnalysisToolsetError;

impl fmt::Display for DataAnalysisToolsetError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("the data analysis tool definitions are invalid")
    }
}

impl std::error::Error for DataAnalysisToolsetError {}

impl From<MaterializedToolsetError> for DataAnalysisToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self
    }
}

/// The tools for one run, behind the deployment's tool policy. `None` when the
/// policy blocks the `data_analysis` toolkit.
///
/// # Errors
///
/// A tool definition the policy wrapper refuses.
pub fn build_data_analysis_toolset(
    authority: &DataAnalysisAuthority,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<Option<BasicToolset>, DataAnalysisToolsetError> {
    build_with(authority, policy, Limits::default())
}

pub(crate) fn build_with(
    authority: &DataAnalysisAuthority,
    policy: &Arc<ToolAdmissionPolicy>,
    limits: Limits,
) -> Result<Option<BasicToolset>, DataAnalysisToolsetError> {
    if policy.toolkit_decision(TOOLKIT_TYPE) == ToolAdmissionDecision::BlockedToolkit {
        return Ok(None);
    }
    let limits = Arc::new(limits);
    let tools: Vec<Arc<dyn Tool>> = ALL_TOOLS
        .iter()
        .copied()
        .filter(|tool| *tool != DataTool::RunCode || authority.runner.is_some())
        .map(|tool| -> Arc<dyn Tool> {
            Arc::new(DataToolImpl {
                tool,
                host: Arc::clone(&authority.host),
                runner: authority.runner.clone(),
                limits: Arc::clone(&limits),
            })
        })
        .collect();
    admit_materialized_toolset(TOOLKIT_TYPE, TOOLKIT_TYPE, policy, tools)
        .map(Some)
        .map_err(Into::into)
}

struct DataToolImpl {
    tool: DataTool,
    host: SharedOfficeToolHost,
    runner: Option<Arc<dyn AnalysisCodeRunner>>,
    limits: Arc<Limits>,
}

#[async_trait]
impl Tool for DataToolImpl {
    fn name(&self) -> &str {
        self.tool.name()
    }

    fn description(&self) -> &str {
        self.tool.description()
    }

    fn is_read_only(&self) -> bool {
        self.tool.is_read_only()
    }

    fn is_concurrency_safe(&self) -> bool {
        self.tool.is_read_only()
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(self.tool.parameters_schema())
    }

    async fn execute(
        &self,
        context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        let call_id = format!("{}:{}", context.invocation_id(), context.function_call_id());
        Ok(match self.run(&arguments, &call_id).await {
            Ok(value) => value,
            Err(message) => json!({"status": "error", "message": message}),
        })
    }
}

/// A host failure as the model reads it: one sentence per real cause.
fn host_sentence(action: &str, error: &OfficeHostError) -> Sentence {
    tracing::warn!(
        event = "data_analysis_host_failed",
        reason_code = error.reason_code(),
        action,
        "a data analysis tool's platform call failed"
    );
    match error.kind() {
        OfficeFailure::NotFound => format!(
            "Could not {action}: no such file in the bucket. Check the file name and bucket against the attachment message."
        ),
        OfficeFailure::AccessRefused => format!(
            "Could not {action}: the platform refused access to the file store. Do not retry; tell the user."
        ),
        OfficeFailure::AlreadyExists => format!(
            "Could not {action}: a file with that name already exists. Choose another name, or pass overwrite=true."
        ),
        OfficeFailure::UnsupportedType => {
            format!("Could not {action}: the file store does not accept that type of file.")
        }
        OfficeFailure::InvalidRequest => format!(
            "Could not {action}: the file name or bucket is not valid, or the request was refused as invalid."
        ),
        OfficeFailure::TooLarge => {
            format!("Could not {action}: the file is larger than the platform accepts.")
        }
        OfficeFailure::BudgetRefused | OfficeFailure::NoModel => {
            format!("Could not {action}: the platform refused the request.")
        }
        OfficeFailure::Unreachable => format!(
            "Could not {action}: the platform could not be reached. Tell the user it did not happen."
        ),
    }
}

// ---------------------------------------------------------------------------
// Names and arguments
// ---------------------------------------------------------------------------

pub(crate) fn valid_bucket_name(bucket: &str) -> bool {
    let bytes = bucket.as_bytes();
    bytes.len() >= 2
        && bytes.len() <= 63
        && bytes[0].is_ascii_lowercase()
        && bytes[1..]
            .iter()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || *b == b'-')
}

fn text_arg(args: &Value, key: &str) -> Option<String> {
    args.get(key)
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(str::to_owned)
}

fn required(args: &Value, key: &str) -> Result<String, Sentence> {
    text_arg(args, key).ok_or_else(|| format!("`{key}` is required."))
}

fn bool_arg(args: &Value, key: &str) -> bool {
    args.get(key).and_then(Value::as_bool).unwrap_or(false)
}

fn bucket_arg(args: &Value, key: &str) -> Result<String, Sentence> {
    match text_arg(args, key) {
        None => Ok(DEFAULT_BUCKET.to_owned()),
        Some(bucket) if valid_bucket_name(&bucket) => Ok(bucket),
        Some(bucket) => Err(format!(
            "`{bucket}` is not a valid bucket name (lowercase letters, digits and hyphens, starting with a letter)."
        )),
    }
}

/// The object key a name denotes: a leading slash is dropped, and so is a
/// leading `/{bucket}/`, so the path the attachment message shows works as
/// the next call's name.
fn object_key(bucket: &str, name: &str) -> Result<String, Sentence> {
    let trimmed = name.trim();
    let key = if let Some(rooted) = trimmed.strip_prefix('/') {
        let rooted = rooted.trim_start_matches('/');
        rooted
            .strip_prefix(bucket)
            .and_then(|rest| rest.strip_prefix('/'))
            .unwrap_or(rooted)
    } else {
        trimmed
    };
    if key.is_empty() || key.len() > MAX_NAME_BYTES {
        return Err("The file name is empty or too long.".to_owned());
    }
    if key
        .split('/')
        .any(|segment| segment == ".." || segment.is_empty())
        || key.contains(['\\', '\0'])
        || key.chars().any(char::is_control)
    {
        return Err("The file name may not contain `..`, empty path parts, backslashes or control characters.".to_owned());
    }
    Ok(key.to_owned())
}

fn directory_of(key: &str) -> &str {
    key.rfind('/').map_or("", |i| &key[..=i])
}

/// Where a result file goes: a bare name next to the source, a path as given.
fn output_key(bucket: &str, source_key: &str, name: &str) -> Result<String, Sentence> {
    let trimmed = name.trim();
    if trimmed.contains('/') {
        return object_key(bucket, trimmed);
    }
    object_key(bucket, &format!("{}{trimmed}", directory_of(source_key)))
}

struct Source {
    bucket: String,
    key: String,
    sheet: Option<String>,
    format: Format,
}

fn source(
    args: &Value,
    name_key: &str,
    bucket_key: &str,
    sheet_key: &str,
) -> Result<Source, Sentence> {
    let bucket = bucket_arg(args, bucket_key)?;
    let key = object_key(&bucket, &required(args, name_key)?)?;
    let format = Format::from_name(&key).map_err(|e| e.to_string())?;
    Ok(Source {
        bucket,
        key,
        sheet: text_arg(args, sheet_key),
        format,
    })
}

struct Output {
    key: String,
    format: Format,
    overwrite: bool,
}

fn output(args: &Value, name_key: &str, src: &Source) -> Result<Option<Output>, Sentence> {
    let Some(name) = text_arg(args, name_key) else {
        return Ok(None);
    };
    let key = output_key(&src.bucket, &src.key, &name)?;
    let format = Format::from_name(&key).map_err(|_| {
        format!("The output file `{name}` needs a .csv, .tsv, .json or .xlsx extension.")
    })?;
    Ok(Some(Output {
        key,
        format,
        overwrite: bool_arg(args, "overwrite"),
    }))
}

fn preview_limit(args: &Value, limits: &Limits) -> usize {
    args.get("limit")
        .and_then(Value::as_u64)
        .map_or(DEFAULT_PREVIEW_ROWS, |n| n as usize)
        .clamp(1, limits.max_preview_rows)
}

// ---------------------------------------------------------------------------
// Running
// ---------------------------------------------------------------------------

/// What the blocking half of a tool produced.
struct Outcome {
    answer: Map<String, Value>,
    encoded: Option<Encoded>,
}

struct Encoded {
    bytes: Vec<u8>,
    media_type: &'static str,
    notes: WriteNotes,
}

struct Input {
    bytes: Vec<u8>,
    source: Source,
}

impl DataToolImpl {
    async fn run(&self, args: &Value, call_id: &str) -> Result<Value, Sentence> {
        if !args.is_object() {
            return Err("The arguments must be a JSON object.".to_owned());
        }
        let primary = match self.tool {
            DataTool::SaveDataframe => source(args, "source_df", "bucket_name", "sheet")?,
            _ => source(args, "filename", "bucket_name", "sheet")?,
        };
        let second = if self.tool == DataTool::JoinData {
            Some(source(
                args,
                "right_filename",
                "right_bucket_name",
                "right_sheet",
            )?)
        } else {
            None
        };
        let out: Option<Output> = match self.tool {
            DataTool::SaveDataframe => Some(
                output(args, "target_file", &primary)?
                    .ok_or_else(|| "`target_file` is required.".to_owned())?,
            ),
            DataTool::ChartData => Some(chart_output(args, &primary)?),
            DataTool::DescribeData | DataTool::CorrelateData | DataTool::RunCode => None,
            _ => output(args, "output_file", &primary)?,
        };
        let first = self.read(primary).await?;
        let second = match second {
            Some(s) => Some(self.read(s).await?),
            None => None,
        };
        let (bucket, source_key) = (first.source.bucket.clone(), first.source.key.clone());
        let (tool, call) = (self.tool, args.clone());
        let runner = self.runner.clone();
        let call_id = call_id.to_owned();
        let format = out.as_ref().map(|o| o.format);
        let outcome = self
            .blocking(move |limits, deadline| {
                compute(tool, &call, first, second, format, limits, deadline)
            })
            .await?;
        let Outcome {
            mut answer,
            encoded,
        } = match (outcome, runner) {
            (Pending::Done(outcome), _) => outcome,
            (Pending::Code(request), Some(runner)) => {
                let CodeJob {
                    mut answer,
                    language,
                    source,
                    input,
                } = request;
                let result = runner
                    .run(AnalysisCodeRequest {
                        call_id,
                        language,
                        source,
                        input,
                    })
                    .await
                    .map_err(|f| failure_sentence(f).to_owned())?;
                let size = serde_json::to_vec(&result).map_or(usize::MAX, |b| b.len());
                if size > MAX_ANALYSIS_RESULT_BYTES {
                    return Err(format!(
                        "The program returned {size} bytes; return at most {MAX_ANALYSIS_RESULT_BYTES}. Summarise inside the program."
                    ));
                }
                answer.insert("result".into(), result);
                Outcome {
                    answer,
                    encoded: None,
                }
            }
            (Pending::Code(_), None) => {
                return Err(failure_sentence(AnalysisCodeFailure::Unavailable).to_owned());
            }
        };
        if let (Some(encoded), Some(out)) = (encoded, out) {
            let stored = self
                .host
                .write_object(
                    &bucket,
                    &out.key,
                    encoded.media_type,
                    encoded.bytes,
                    out.overwrite,
                )
                .await
                .map_err(|e| {
                    if e.kind() == OfficeFailure::AlreadyExists {
                        format!(
                            "`{}` already exists. Pass overwrite=true to replace it, or choose another output name.",
                            out.key
                        )
                    } else {
                        host_sentence(&format!("save `{}`", out.key), &e)
                    }
                })?;
            let link = self.host.object_link(&stored.bucket, &stored.name);
            let mut saved = json!({
                "file": stored.name,
                "bucket": stored.bucket,
                "bytes": stored.byte_length,
                "source": source_key,
            });
            if let Some(link) = link {
                saved["link"] = Value::String(link);
            }
            if encoded.notes.neutralized_cells > 0 {
                saved["csv_formula_cells_prefixed"] = json!(encoded.notes.neutralized_cells);
                saved["note"] = json!(
                    "Text cells that begin like a spreadsheet formula (= + - @) got a leading ' so they open as text, not as a formula."
                );
            }
            if self.tool == DataTool::ChartData {
                let title = answer
                    .get("title")
                    .and_then(Value::as_str)
                    .unwrap_or("Chart")
                    .to_owned();
                if let Some(link) = saved.get("link").and_then(Value::as_str) {
                    answer.insert(
                        "markdown".into(),
                        json!(format!(
                            "![{}]({link})",
                            title
                                .chars()
                                .filter(|c| !matches!(c, '[' | ']' | '(' | ')') && !c.is_control())
                                .collect::<String>()
                        )),
                    );
                }
            }
            if self.tool == DataTool::SaveDataframe {
                answer.insert(
                    "message".into(),
                    json!(format!(
                        "Successfully saved dataframe to {} in {}",
                        stored.name, stored.bucket
                    )),
                );
            }
            answer.insert("saved".into(), saved);
        }
        answer.insert("status".into(), json!("success"));
        Ok(Value::Object(answer))
    }

    async fn read(&self, source: Source) -> Result<Input, Sentence> {
        let bytes = self
            .host
            .read_object(&source.bucket, &source.key, MAX_OFFICE_OBJECT_BYTES)
            .await
            .map_err(|e| host_sentence(&format!("read `{}`", source.key), &e))?;
        Ok(Input { bytes, source })
    }

    /// Run `work` on the blocking pool, a few at a time, under the engine's
    /// own clock.
    async fn blocking<F>(&self, work: F) -> Result<Pending, Sentence>
    where
        F: FnOnce(&Limits, &Deadline) -> DataResult<Pending> + Send + 'static,
    {
        let _permit = ANALYSIS_GATE
            .acquire()
            .await
            .map_err(|_| "The analysis engine is shutting down.".to_owned())?;
        let limits = Arc::clone(&self.limits);
        tokio::task::spawn_blocking(move || {
            let deadline = Deadline::after(limits.deadline);
            work(&limits, &deadline)
        })
        .await
        .map_err(|_| "The analysis engine stopped unexpectedly on this file.".to_owned())?
        .map_err(|e| e.to_string())
    }
}

enum Pending {
    Done(Outcome),
    Code(CodeJob),
}

struct CodeJob {
    answer: Map<String, Value>,
    language: AnalysisLanguage,
    source: String,
    input: Value,
}

fn chart_output(args: &Value, src: &Source) -> Result<Output, Sentence> {
    let key = match text_arg(args, "output_file") {
        Some(name) => {
            let key = output_key(&src.bucket, &src.key, &name)?;
            if !key.to_ascii_lowercase().ends_with(".png") {
                return Err("The chart is a PNG: `output_file` must end in .png.".to_owned());
            }
            key
        }
        None => {
            let nanos = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map_or(0, |d| d.as_nanos());
            let digest = ring::digest::digest(
                &ring::digest::SHA256,
                format!("{}{args}{nanos}", src.key).as_bytes(),
            );
            let id = digest
                .as_ref()
                .iter()
                .take(6)
                .fold(String::new(), |mut id, b| {
                    use std::fmt::Write as _;
                    let _ = write!(id, "{b:02x}");
                    id
                });
            format!("{}chart_{id}.png", directory_of(&src.key))
        }
    };
    Ok(Output {
        key,
        // A chart is not a table format; this value is unused for charts.
        format: Format::Csv,
        overwrite: bool_arg(args, "overwrite"),
    })
}

// ---------------------------------------------------------------------------
// Compute (blocking)
// ---------------------------------------------------------------------------

fn load(input: &Input, limits: &Limits, deadline: &Deadline) -> DataResult<Loaded> {
    formats::read_table(
        &input.bytes,
        input.source.format,
        input.source.sheet.as_deref(),
        limits,
        deadline,
    )
}

fn compute(
    tool: DataTool,
    args: &Value,
    first: Input,
    second: Option<Input>,
    format: Option<Format>,
    limits: &Limits,
    deadline: &Deadline,
) -> DataResult<Pending> {
    let loaded = load(&first, limits, deadline)?;
    let Loaded { table, info } = loaded;
    let mut answer = Map::new();
    answer.insert("source".into(), source_json(&first.source, &info));
    if !info.notes.is_empty() {
        answer.insert("notes".into(), json!(info.notes));
    }
    let outcome = |answer: Map<String, Value>, encoded: Option<Encoded>| {
        Ok(Pending::Done(Outcome { answer, encoded }))
    };
    match tool {
        DataTool::DescribeData => {
            let table = apply_columns(table, args)?;
            describe(&table, args, &mut answer);
            outcome(answer, None)
        }
        DataTool::FilterData | DataTool::SortData => {
            let mut table = ops::apply_filter(table, args, deadline)?;
            answer.insert("rows_matching".into(), json!(table.rows));
            let keys = ops::parse_sort(&table, args)?;
            if tool == DataTool::SortData && keys.is_empty() {
                return Err(argument("`sort_by` is required."));
            }
            if !keys.is_empty() {
                table = ops::sort_table(&table, &keys, deadline)?;
            }
            let table = apply_columns(table, args)?;
            finish_table(table, args, format, answer, limits, deadline)
        }
        DataTool::AggregateData => {
            let table = ops::apply_filter(table, args, deadline)?;
            answer.insert("rows_used".into(), json!(table.rows));
            let group_by = ops::parse_columns(&table, args, "group_by")?;
            let specs = ops::parse_aggregations(&table, args)?;
            let mut result = ops::aggregate(&table, &group_by, &specs, limits, deadline)?;
            let keys = ops::parse_sort(&result, args)?;
            if !keys.is_empty() {
                result = ops::sort_table(&result, &keys, deadline)?;
            }
            finish_table(result, args, format, answer, limits, deadline)
        }
        DataTool::PivotData => {
            let table = ops::apply_filter(table, args, deadline)?;
            answer.insert("rows_used".into(), json!(table.rows));
            let index = ops::parse_columns(&table, args, "index")?;
            if index.is_empty() {
                return Err(argument("`index` needs at least one column."));
            }
            let columns = table.find(&required_arg(args, "columns")?)?;
            let values = table.find(&required_arg(args, "values")?)?;
            let func = Func::parse(&text_arg(args, "function").unwrap_or_else(|| "sum".into()))?;
            let fill = match args.get("fill_value") {
                None | Some(Value::Null) => None,
                Some(v) => Some(
                    v.as_f64()
                        .ok_or_else(|| argument("`fill_value` must be a number."))?,
                ),
            };
            let result = ops::pivot(
                &table,
                &PivotSpec {
                    index: &index,
                    columns,
                    values,
                    func,
                    fill,
                },
                limits,
                deadline,
            )?;
            finish_table(result, args, format, answer, limits, deadline)
        }
        DataTool::JoinData => {
            let second = second.ok_or_else(|| argument("`right_filename` is required."))?;
            let right = load(&second, limits, deadline)?;
            answer.insert(
                "right_source".into(),
                source_json(&second.source, &right.info),
            );
            join(&table, &right.table, args, format, answer, limits, deadline)
        }
        DataTool::CorrelateData => {
            let table = ops::apply_filter(table, args, deadline)?;
            let mut columns = ops::parse_columns(&table, args, "columns")?;
            if columns.is_empty() {
                columns = (0..table.width())
                    .filter(|c| table.columns[*c].dtype().is_numeric())
                    .collect();
            }
            if let Some(bad) = columns
                .iter()
                .find(|c| !table.columns[**c].dtype().is_numeric())
            {
                return Err(argument(format!(
                    "The column `{}` is {}, not numeric.",
                    table.names[*bad],
                    table.columns[*bad].dtype().name()
                )));
            }
            if columns.len() < 2 {
                return Err(argument("Correlation needs at least two numeric columns."));
            }
            if columns.len() > 50 {
                return Err(argument(
                    "Correlation takes at most 50 columns; choose them with `columns`.",
                ));
            }
            let spearman = match text_arg(args, "method").as_deref() {
                None | Some("pearson") => false,
                Some("spearman") => true,
                Some(other) => {
                    return Err(argument(format!(
                        "`method` must be pearson or spearman, not `{other}`."
                    )));
                }
            };
            let result = ops::correlate(&table, &columns, spearman, deadline)?;
            answer.insert(
                "method".into(),
                json!(if spearman { "spearman" } else { "pearson" }),
            );
            answer.insert("rows_used".into(), json!(table.rows));
            table_into(&mut answer, &result, result.rows, limits);
            outcome(answer, None)
        }
        DataTool::ChartData => chart_tool(table, args, answer, limits, deadline),
        DataTool::SaveDataframe => {
            let format = format.ok_or_else(|| argument("`target_file` is required."))?;
            answer.insert("rows".into(), json!(table.rows));
            answer.insert("columns".into(), json!(table.width()));
            let (bytes, notes) = formats::write_table(&table, format, limits, deadline)?;
            outcome(
                answer,
                Some(Encoded {
                    bytes,
                    media_type: format.media_type(),
                    notes,
                }),
            )
        }
        DataTool::RunCode => code_job(table, args, answer, limits, deadline),
    }
}

fn required_arg(args: &Value, key: &str) -> DataResult<String> {
    text_arg(args, key).ok_or_else(|| argument(format!("`{key}` is required.")))
}

fn source_json(source: &Source, info: &SourceInfo) -> Value {
    let mut value = json!({"file": source.key, "bucket": source.bucket});
    if let Some(sheet) = &info.sheet {
        value["sheet"] = json!(sheet);
        value["sheets"] = json!(info.sheets);
    }
    value
}

fn apply_columns(table: Table, args: &Value) -> DataResult<Table> {
    let columns = ops::parse_columns(&table, args, "columns")?;
    if columns.is_empty() {
        Ok(table)
    } else {
        Ok(table.select(&columns))
    }
}

/// Put the first rows of `table` in the answer, with what was cut said plainly.
fn table_into(answer: &mut Map<String, Value>, table: &Table, shown: usize, limits: &Limits) {
    let mut rows: Vec<Value> = Vec::new();
    let mut bytes = 0usize;
    let mut budget_hit = false;
    for row in 0..table.rows.min(shown) {
        let cells: Vec<Value> = table
            .columns
            .iter()
            .map(|c| match c.cell(row) {
                Cell::Text(t) => Value::String(ops::clip(t, MAX_CELL_CHARS)),
                other => other.json(),
            })
            .collect();
        bytes += cells.iter().map(|c| c.to_string().len() + 1).sum::<usize>();
        if bytes > limits.max_answer_bytes {
            budget_hit = true;
            break;
        }
        rows.push(Value::Array(cells));
    }
    let returned = rows.len();
    answer.insert("columns".into(), json!(table.names));
    answer.insert("types".into(), json!(formats::dtype_names(table)));
    answer.insert("row_count".into(), json!(table.rows));
    answer.insert("rows_returned".into(), json!(returned));
    answer.insert("rows".into(), Value::Array(rows));
    if returned < table.rows {
        answer.insert("truncated".into(), json!(true));
        answer.insert(
            "note".into(),
            json!(if budget_hit {
                format!(
                    "Only the first {returned} of {} rows fit in this answer. Narrow the result, or pass output_file to save all of it.",
                    table.rows
                )
            } else {
                format!(
                    "Showing the first {returned} of {} rows. Raise `limit` (at most {}), or pass output_file to save all of it.",
                    table.rows, limits.max_preview_rows
                )
            }),
        );
    }
}

fn finish_table(
    table: Table,
    args: &Value,
    format: Option<Format>,
    mut answer: Map<String, Value>,
    limits: &Limits,
    deadline: &Deadline,
) -> DataResult<Pending> {
    table_into(&mut answer, &table, preview_limit(args, limits), limits);
    let encoded = match format {
        Some(format) => {
            let (bytes, notes) = formats::write_table(&table, format, limits, deadline)?;
            Some(Encoded {
                bytes,
                media_type: format.media_type(),
                notes,
            })
        }
        None => None,
    };
    Ok(Pending::Done(Outcome { answer, encoded }))
}

fn describe(table: &Table, args: &Value, answer: &mut Map<String, Value>) {
    answer.insert("rows".into(), json!(table.rows));
    let profiles: Vec<Value> = table
        .names
        .iter()
        .zip(&table.columns)
        .map(|(n, c)| ops::describe_column(n, c))
        .collect();
    answer.insert("column_count".into(), json!(table.width()));
    answer.insert("columns".into(), Value::Array(profiles));
    let head = args
        .get("head_rows")
        .and_then(Value::as_u64)
        .map_or(5, |n| (n as usize).min(50));
    if head > 0 {
        let mut preview = Map::new();
        let limits = Limits {
            max_answer_bytes: 20 * 1024,
            ..Limits::default()
        };
        table_into(&mut preview, table, head, &limits);
        answer.insert(
            "head".into(),
            json!({"columns": preview.get("columns"), "rows": preview.get("rows")}),
        );
    }
}

fn join(
    left: &Table,
    right: &Table,
    args: &Value,
    format: Option<Format>,
    answer: Map<String, Value>,
    limits: &Limits,
    deadline: &Deadline,
) -> DataResult<Pending> {
    let names =
        |t: &Table, key: &str| -> DataResult<Vec<usize>> { ops::parse_columns(t, args, key) };
    let (left_keys, right_keys, merge_keys) = if args.get("on").is_some_and(|v| !v.is_null()) {
        (names(left, "on")?, names(right, "on")?, true)
    } else {
        (names(left, "left_on")?, names(right, "right_on")?, false)
    };
    if left_keys.is_empty() || left_keys.len() != right_keys.len() {
        return Err(argument(
            "Give `on`, or `left_on` and `right_on` with the same number of columns.",
        ));
    }
    let how = How::parse(&text_arg(args, "how").unwrap_or_else(|| "inner".into()))?;
    let mut joined = ops::join(
        left,
        right,
        &JoinSpec {
            left_keys: &left_keys,
            right_keys: &right_keys,
            how,
            suffixes: ("_x", "_y"),
            merge_keys,
        },
        limits,
        deadline,
    )?;
    let keys = ops::parse_sort(&joined, args)?;
    if !keys.is_empty() {
        joined = ops::sort_table(&joined, &keys, deadline)?;
    }
    let joined = apply_columns(joined, args)?;
    finish_table(joined, args, format, answer, limits, deadline)
}

// ---------------------------------------------------------------------------
// Charts
// ---------------------------------------------------------------------------

fn chart_tool(
    table: Table,
    args: &Value,
    mut answer: Map<String, Value>,
    limits: &Limits,
    deadline: &Deadline,
) -> DataResult<Pending> {
    let kind = Kind::parse(&required_arg(args, "chart_type")?)?;
    let mut table = ops::apply_filter(table, args, deadline)?;
    answer.insert("rows_used".into(), json!(table.rows));
    if table.rows == 0 {
        return Err(argument("No rows are left to draw after the filters."));
    }
    // Scatter and histogram draw rows, so `sort_by` orders the rows. Bar, line
    // and pie draw groups: there it orders the aggregated groups, below.
    if matches!(kind, Kind::Scatter | Kind::Histogram) {
        let keys = ops::parse_sort(&table, args)?;
        if !keys.is_empty() {
            table = ops::sort_table(&table, &keys, deadline)?;
        }
    }
    let y = ops::parse_columns(&table, args, "y")?;
    let numeric = |c: usize| -> DataResult<()> {
        if table.columns[c].dtype().is_numeric() {
            Ok(())
        } else {
            Err(argument(format!(
                "The column `{}` is {}, not numeric, so it cannot be a value axis.",
                table.names[c],
                table.columns[c].dtype().name()
            )))
        }
    };
    let (categories, series, x_name, y_name, mut notes) = match kind {
        Kind::Histogram => {
            let &[y] = y.as_slice() else {
                return Err(argument("A histogram needs exactly one column in `y`."));
            };
            numeric(y)?;
            let bins = args
                .get("bins")
                .and_then(Value::as_u64)
                .map_or(20, |n| (n as usize).clamp(2, 100));
            let (labels, counts) = histogram(&table.columns[y], bins);
            (
                labels,
                vec![Series {
                    name: table.names[y].clone(),
                    values: counts.into_iter().map(|c| Some(c as f64)).collect(),
                    points: Vec::new(),
                }],
                table.names[y].clone(),
                "count".to_owned(),
                Vec::new(),
            )
        }
        Kind::Scatter => {
            let x = table.find(&required_arg(args, "x")?)?;
            numeric(x)?;
            if y.is_empty() {
                return Err(argument("A scatter chart needs `y`."));
            }
            let mut notes = Vec::new();
            let mut series = Vec::new();
            for c in &y {
                numeric(*c)?;
                let mut points: Vec<(f64, f64)> = (0..table.rows)
                    .filter_map(|r| {
                        Some((
                            table.columns[x].cell(r).as_f64()?,
                            table.columns[*c].cell(r).as_f64()?,
                        ))
                    })
                    .collect();
                if points.len() > MAX_SCATTER_POINTS {
                    let stride = points.len().div_ceil(MAX_SCATTER_POINTS);
                    notes.push(format!(
                        "`{}` has {} points; every {stride}th is drawn.",
                        table.names[*c],
                        points.len()
                    ));
                    points = points.into_iter().step_by(stride).collect();
                }
                series.push(Series {
                    name: table.names[*c].clone(),
                    values: Vec::new(),
                    points,
                });
            }
            (
                Vec::new(),
                series,
                table.names[x].clone(),
                table.names[y[0]].clone(),
                notes,
            )
        }
        Kind::Bar | Kind::Line | Kind::Pie => {
            let x = table.find(&required_arg(args, "x")?)?;
            let func = Func::parse(&text_arg(args, "aggregation").unwrap_or_else(|| "sum".into()))?;
            if kind == Kind::Pie && y.len() > 1 {
                return Err(argument("A pie chart takes one column in `y`."));
            }
            let specs: Vec<ops::AggSpec> = if y.is_empty() {
                vec![ops::AggSpec {
                    column: None,
                    func: Func::Count,
                    name: "count".into(),
                }]
            } else {
                y.iter()
                    .map(|c| {
                        numeric(*c)?;
                        Ok(ops::AggSpec {
                            column: Some(*c),
                            func,
                            name: table.names[*c].clone(),
                        })
                    })
                    .collect::<DataResult<_>>()?
            };
            let mut grouped = ops::aggregate(&table, &[x], &specs, limits, deadline)?;
            let mut notes = Vec::new();
            if let Some(top) = args.get("top_n").and_then(Value::as_u64) {
                let order = ops::sorted_rows(&grouped, &[(1, true)], deadline)?;
                let kept: Vec<usize> = order.into_iter().take(top as usize).collect();
                notes.push(format!(
                    "Showing the {} categories with the largest {}.",
                    kept.len(),
                    grouped.names[1]
                ));
                grouped = grouped.take_rows(&kept);
            }
            let keys = ops::parse_sort(&grouped, args)?;
            if !keys.is_empty() {
                grouped = ops::sort_table(&grouped, &keys, deadline)?;
            }
            if grouped.rows > MAX_CHART_CATEGORIES {
                return Err(argument(format!(
                    "`{}` has {} distinct values; a chart shows at most {MAX_CHART_CATEGORIES}. Use `top_n`, `filters`, or group coarsely first.",
                    table.names[x], grouped.rows
                )));
            }
            if grouped.rows == 0 {
                return Err(argument("No categories are left to draw."));
            }
            let categories: Vec<String> = (0..grouped.rows)
                .map(|r| ops::label(&grouped.columns[0], r))
                .collect();
            let series = (1..grouped.width())
                .map(|c| Series {
                    name: grouped.names[c].clone(),
                    values: (0..grouped.rows)
                        .map(|r| grouped.columns[c].cell(r).as_f64())
                        .collect(),
                    points: Vec::new(),
                })
                .collect();
            let y_name = if y.len() == 1 {
                format!("{} ({})", table.names[y[0]], func.name())
            } else {
                "value".to_owned()
            };
            (categories, series, table.names[x].clone(), y_name, notes)
        }
    };
    let title = text_arg(args, "title").unwrap_or_else(|| match kind {
        Kind::Histogram => format!("Distribution of {x_name}"),
        _ => format!("{y_name} by {x_name}"),
    });
    let chart = Chart {
        kind,
        title: title.clone(),
        x_label: text_arg(args, "x_label").unwrap_or(x_name),
        y_label: text_arg(args, "y_label").unwrap_or(y_name),
        categories: categories.clone(),
        series,
    };
    let svg = chart::render_svg(&chart)?;
    deadline.check()?;
    let png = svg_to_png(&svg, limits, deadline)?;
    answer.insert("title".into(), json!(title));
    answer.insert("chart_type".into(), json!(args["chart_type"]));
    answer.insert("categories".into(), json!(categories.len()));
    answer.insert(
        "series".into(),
        json!(
            chart
                .series
                .iter()
                .map(|s| s.name.clone())
                .collect::<Vec<_>>()
        ),
    );
    if !categories.is_empty() {
        let plotted: Vec<Value> = categories
            .iter()
            .take(50)
            .enumerate()
            .map(|(i, c)| {
                let mut row = vec![json!(c)];
                row.extend(chart.series.iter().map(|s| json!(s.values[i])));
                Value::Array(row)
            })
            .collect();
        answer.insert("plotted_rows".into(), Value::Array(plotted));
    }
    if !notes.is_empty() {
        answer.entry("notes").or_insert_with(|| json!([]));
        if let Some(Value::Array(existing)) = answer.get_mut("notes") {
            existing.extend(notes.drain(..).map(Value::String));
        }
    }
    Ok(Pending::Done(Outcome {
        answer,
        encoded: Some(Encoded {
            bytes: png,
            media_type: PNG_MEDIA_TYPE,
            notes: WriteNotes::default(),
        }),
    }))
}

#[cfg(feature = "toolkit-data-analysis")]
fn svg_to_png(svg: &str, limits: &Limits, deadline: &Deadline) -> DataResult<Vec<u8>> {
    formats::with_office(deadline, || {
        elitea_office_core::svg_to_png(
            svg,
            u64::from(chart::WIDTH) * u64::from(chart::HEIGHT),
            &limits.office,
        )
    })
    .map_err(|e| match e {
        DataError::Malformed(_) => DataError::Malformed("The chart could not be drawn.".into()),
        other => other,
    })
}

#[cfg(not(feature = "toolkit-data-analysis"))]
fn svg_to_png(_: &str, _: &Limits, _: &Deadline) -> DataResult<Vec<u8>> {
    Err(DataError::Unsupported(
        "This build cannot draw charts.".into(),
    ))
}

/// Equal-width bins over the range of the values, labelled by their bounds.
fn histogram(column: &Column, bins: usize) -> (Vec<String>, Vec<usize>) {
    let values: Vec<f64> = (0..column.len())
        .filter_map(|r| column.cell(r).as_f64())
        .collect();
    let (lo, hi) = values
        .iter()
        .fold((f64::INFINITY, f64::NEG_INFINITY), |(lo, hi), v| {
            (lo.min(*v), hi.max(*v))
        });
    if values.is_empty() {
        return (Vec::new(), Vec::new());
    }
    let width = if hi > lo {
        (hi - lo) / bins as f64
    } else {
        1.0
    };
    let mut counts = vec![0usize; bins];
    for v in &values {
        let slot = (((v - lo) / width) as usize).min(bins - 1);
        counts[slot] += 1;
    }
    let labels = (0..bins)
        .map(|b| {
            format!(
                "{}-{}",
                super::table::format_float(lo + width * b as f64),
                super::table::format_float(lo + width * (b + 1) as f64)
            )
        })
        .collect();
    (labels, counts)
}

// ---------------------------------------------------------------------------
// Code
// ---------------------------------------------------------------------------

fn code_job(
    table: Table,
    args: &Value,
    mut answer: Map<String, Value>,
    limits: &Limits,
    deadline: &Deadline,
) -> DataResult<Pending> {
    let language = match text_arg(args, "language").as_deref() {
        None | Some("python") => AnalysisLanguage::Python,
        Some("javascript") => AnalysisLanguage::JavaScript,
        Some(other) => {
            return Err(argument(format!(
                "`language` must be python or javascript, not `{other}`."
            )));
        }
    };
    let source = required_arg(args, "code")?;
    let table = ops::apply_filter(table, args, deadline)?;
    let table = apply_columns(table, args)?;
    let total = table.rows;
    let width = table.width().max(1);
    let by_cells = limits.max_sandbox_cells / width;
    let wanted = args
        .get("max_rows")
        .and_then(Value::as_u64)
        .map_or(by_cells, |n| (n as usize).min(by_cells));
    if wanted == 0 {
        return Err(argument(format!(
            "A row has {width} columns, more than the {} cells the sandbox accepts. Choose fewer `columns`.",
            limits.max_sandbox_cells
        )));
    }
    // Rows go in one at a time against three budgets: the cell count, the
    // request's bytes (the source takes its share) and the sandbox's JSON node
    // count, in which every array, object and scalar counts. The first row that
    // would break one ends the list, and the answer says how many were sent.
    let names_json = json!(table.names);
    let types_json = json!(formats::dtype_names(&table));
    let head_bytes =
        names_json.to_string().len() + types_json.to_string().len() + SANDBOX_HEAD_BYTES;
    let byte_budget = limits
        .max_sandbox_bytes
        .saturating_sub(source.len() + SANDBOX_ENVELOPE_BYTES);
    // data object, columns + types arrays and their names, rows array,
    // total_rows, truncated.
    let fixed_nodes = 6 + 2 * table.width();
    let node_budget = limits.max_sandbox_nodes.saturating_sub(SANDBOX_NODE_MARGIN);
    let (mut bytes, mut nodes) = (head_bytes, fixed_nodes);
    let mut rows: Vec<Value> = Vec::new();
    for r in 0..total.min(wanted) {
        let row = Value::Array(table.columns.iter().map(|c| c.cell(r).json()).collect());
        let row_bytes = row.to_string().len() + 1;
        let row_nodes = 1 + table.width();
        if bytes + row_bytes > byte_budget || nodes + row_nodes > node_budget {
            break;
        }
        bytes += row_bytes;
        nodes += row_nodes;
        rows.push(row);
    }
    let sent = rows.len();
    if sent == 0 && total > 0 {
        return Err(argument(
            "Not even one row fits in what the sandbox accepts (the code is long or a row is very large). Choose fewer `columns`, or shorten the code.",
        ));
    }
    answer.insert("rows_total".into(), json!(total));
    answer.insert("rows_sent".into(), json!(sent));
    if sent < total {
        answer.insert("truncated".into(), json!(true));
        answer.insert(
            "note".into(),
            json!(format!(
                "Only the first {sent} of {total} rows were sent to the program, so any total it computes covers those rows only. Filter, choose columns, or aggregate with the other tools to fit."
            )),
        );
    }
    let input = json!({"data": {
        "columns": names_json,
        "types": types_json,
        "rows": rows,
        "total_rows": total,
        "truncated": sent < total,
    }});
    Ok(Pending::Code(CodeJob {
        answer,
        language,
        source,
        input,
    }))
}
