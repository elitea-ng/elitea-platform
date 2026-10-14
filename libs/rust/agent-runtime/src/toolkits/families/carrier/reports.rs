//! The three Carrier tools that move result archives and spreadsheets:
//! `get_report_by_id`, `create_excel_report` and `create_ui_excel_report`.
//!
//! The SDK downloads each report's zip archives from Carrier's artifact
//! store, merges their logs on local disk, computes statistics with pandas,
//! writes an openpyxl workbook and uploads it back to Carrier. Here the logs
//! are spooled to anonymous temporary files and streamed (bounded by bytes,
//! samples and archive count), the statistics are `perf_report`, and the
//! workbook is stored in the project's artifact store through
//! [`AttachmentHost::write_artifact`] (never replacing a file), so the answer
//! names the stored `/{bucket}/{file}` instead of a Carrier download link.

use std::fmt::Write as _;
use std::fs::File;
use std::io::{BufRead, BufReader, Cursor, Read, Seek, SeekFrom};
use std::sync::Arc;

use reqwest::Method;
use serde_json::{Map, Value, json};

use crate::toolkits::families::attachment_host::{
    AttachmentError, AttachmentHost, ENGINE_BUSY, ENGINE_BUSY_BACKOFF, ENGINE_BUSY_RETRIES,
    derived_bucket_name,
};

use super::client::{
    CarrierAuth, CarrierClient, CarrierClientError, MAX_FILE_BYTES, MAX_LOG_DOWNLOAD_BYTES,
    invalid_response,
};
use super::lighthouse;
use super::perf_report::{self, LogLines, MAX_LOG_BYTES, Parameters, PerfError, Summary};
use super::tools::{
    MAX_ID_BYTES, MAX_TEXT_BYTES, ToolFailure, ToolResult, optional_text, py_str, py_truthy,
    required_text, ui_report_links, ui_reports,
};

/// The most result archives one report downloads.
const MAX_ARCHIVES: usize = 20;
/// The most decompressed errors-log bytes `get_report_by_id` zips.
const MAX_ERRORS_BYTES: u64 = 64 * 1_024 * 1_024;
/// `errors = list(islice(f, 100))`.
const ERROR_LINES: usize = 100;
/// How many times a workbook build waits for an engine permit.
/// The most Lighthouse JSON files, and bytes of each, one UI report reads.
const MAX_UI_FILES: usize = 64;
const MAX_UI_JSON_BYTES: usize = 16 * 1_024 * 1_024;
const XLSX_MEDIA_TYPE: &str = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet";

const MISSING_INPUT: &str = "Please provide report ID or bucket and .jtl/.log file name.";
const CONFIRM_PARAMETERS: &str =
    "Please confirm or override the following parameters to proceed with the report generation.";

fn sentence(message: &str) -> ToolFailure {
    ToolFailure::Sentence(message.to_owned())
}

impl From<PerfError> for ToolFailure {
    fn from(error: PerfError) -> Self {
        sentence(error.sentence())
    }
}

impl From<AttachmentError> for ToolFailure {
    fn from(error: AttachmentError) -> Self {
        Self::Sentence(error.sentence())
    }
}

// ---------------------------------------------------------------------------
// Report archives
// ---------------------------------------------------------------------------

/// `report["name"].replace("_", "").replace(" ", "").lower()`.
fn bucket_of(report: &Value) -> Result<String, ToolFailure> {
    let name = report
        .get("name")
        .and_then(Value::as_str)
        .ok_or_else(|| ToolFailure::Client(invalid_response()))?;
    Ok(name.replace(['_', ' '], "").to_lowercase())
}

async fn report_info(client: &CarrierClient, report_id: &str) -> Result<Value, ToolFailure> {
    let report = client
        .request_json(
            Method::GET,
            &["backend_performance", "reports", client.project_id()],
            &[("report_id", report_id)],
            None,
            false,
        )
        .await?;
    if report.is_object() {
        Ok(report)
    } else {
        Err(ToolFailure::Client(invalid_response()))
    }
}

/// The names in the report's bucket that are its result archives.
async fn archive_names(
    client: &CarrierClient,
    report: &Value,
    bucket: &str,
) -> Result<Vec<String>, ToolFailure> {
    let listing = client
        .request_json(
            Method::GET,
            &[
                "artifacts",
                "artifacts",
                "default",
                client.project_id(),
                bucket,
            ],
            &[],
            None,
            false,
        )
        .await?;
    let rows = listing
        .get("rows")
        .and_then(Value::as_array)
        .ok_or_else(|| ToolFailure::Client(invalid_response()))?;
    let prefix = format!("reports_test_results_{}", py_str(report.get("build_id")));
    let mut names = Vec::new();
    for row in rows {
        let name = row
            .get("name")
            .and_then(Value::as_str)
            .ok_or_else(|| ToolFailure::Client(invalid_response()))?;
        if name.starts_with(&prefix) && !name.contains("excel_report") {
            names.push(name.to_owned());
        }
    }
    if names.len() > MAX_ARCHIVES {
        return Err(PerfError::TooManyArchives.into());
    }
    Ok(names)
}

async fn download_archive(
    client: &CarrierClient,
    bucket: &str,
    name: &str,
) -> Result<Vec<u8>, ToolFailure> {
    Ok(client
        .download(
            &["artifacts", "artifact", client.project_id(), bucket, name],
            &[],
            CarrierAuth::Session,
            MAX_FILE_BYTES,
        )
        .await?)
}

/// Which member of a result archive to read.
#[derive(Clone, Copy)]
enum Member {
    /// `jmeter.jtl` at the archive root.
    Jmeter,
    /// `simulation.log` in the newest top-level folder.
    Gatling,
    /// `simulation-errors.log` at the archive root.
    Errors,
}

/// Copy the wanted member of a zip into `out`, spending `budget` bytes.
/// `Ok(false)` when the archive has no such member.
fn extract_into(
    archive: Vec<u8>,
    member: Member,
    out: &mut File,
    budget: &mut u64,
) -> Result<bool, PerfError> {
    let mut zip = zip::ZipArchive::new(Cursor::new(archive)).map_err(|_| PerfError::Unreadable)?;
    if zip.len() > 10_000 {
        return Err(PerfError::TooLarge);
    }
    let wanted = match member {
        Member::Jmeter => "jmeter.jtl".to_owned(),
        Member::Errors => "simulation-errors.log".to_owned(),
        Member::Gatling => {
            // `get_latest_log_file`: the newest folder directly under the
            // extraction root, which the extraction order makes the last one.
            let mut folders: Vec<String> = Vec::new();
            for name in zip.file_names() {
                if let Some((folder, _)) = name.split_once('/')
                    && !folder.is_empty()
                    && !folders.iter().any(|known| known == folder)
                {
                    folders.push(folder.to_owned());
                }
            }
            match folders.last() {
                Some(folder) => format!("{folder}/simulation.log"),
                None => return Ok(false),
            }
        }
    };
    let Ok(entry) = zip.by_name(&wanted) else {
        return Ok(false);
    };
    let mut limited = entry.take(budget.saturating_add(1));
    let copied = std::io::copy(&mut limited, out).map_err(|_| PerfError::Unreadable)?;
    if copied > *budget {
        return Err(PerfError::TooLarge);
    }
    *budget -= copied;
    Ok(true)
}

async fn blocking<T: Send + 'static>(
    work: impl FnOnce() -> T + Send + 'static,
) -> Result<T, ToolFailure> {
    tokio::task::spawn_blocking(work)
        .await
        .map_err(|_| ToolFailure::from(PerfError::Unreadable))
}

/// The merged summary log of a report as one spooled file per archive.
async fn summary_logs(
    client: &CarrierClient,
    bucket: &str,
    names: &[String],
    member: Member,
) -> Result<Vec<File>, ToolFailure> {
    let mut budget = MAX_LOG_BYTES;
    let mut files = Vec::with_capacity(names.len());
    for name in names {
        let archive = download_archive(client, bucket, name).await?;
        let (found, file, remaining) = blocking(move || {
            let mut file = tempfile::tempfile().map_err(|_| PerfError::Unreadable)?;
            let mut remaining = budget;
            let found = extract_into(archive, member, &mut file, &mut remaining)?;
            file.seek(SeekFrom::Start(0))
                .map_err(|_| PerfError::Unreadable)?;
            Ok::<_, PerfError>((found, file, remaining))
        })
        .await??;
        if !found {
            return Err(PerfError::LogMissing.into());
        }
        budget = remaining;
        files.push(file);
    }
    Ok(files)
}

// ---------------------------------------------------------------------------
// get_report_by_id
// ---------------------------------------------------------------------------

/// `GetReportByIDTool._run`: the report, the first 100 lines of its merged
/// errors log, and a link to a zip of that log uploaded to the report's bucket.
///
/// The SDK also downloads and merges each archive's summary log, which this
/// answer never uses, so that step is skipped here.
pub(super) async fn get_report_by_id(
    client: &CarrierClient,
    arguments: &Map<String, Value>,
) -> ToolResult {
    let report_id = required_text(arguments, "report_id", MAX_ID_BYTES)?;
    let mut report = report_info(client, report_id).await?;
    let bucket = bucket_of(&report)?;
    let names = archive_names(client, &report, &bucket).await?;
    let lg_type = report
        .get("lg_type")
        .and_then(Value::as_str)
        .map(str::to_owned);

    // `merge_error_files`: every archive's `simulation-errors.log`, appended;
    // a failure stops the merge and keeps what was written, as the SDK does.
    let mut errors_file =
        tempfile::tempfile().map_err(|_| ToolFailure::from(PerfError::Unreadable))?;
    let mut budget = MAX_ERRORS_BYTES;
    for name in &names {
        let archive = download_archive(client, &bucket, name).await?;
        let merged = blocking(move || {
            let outcome = extract_into(archive, Member::Errors, &mut errors_file, &mut budget);
            (outcome, errors_file, budget)
        })
        .await?;
        let (outcome, file, remaining) = merged;
        errors_file = file;
        budget = remaining;
        if !matches!(outcome, Ok(true)) {
            break;
        }
    }

    let build_id = py_str(report.get("build_id"));
    let log_name = if lg_type.as_deref() == Some("jmeter") {
        format!("error_{bucket}_jmeter.log")
    } else {
        format!("error_{bucket}_simulation.log")
    };
    let zip_name = format!("{}_error_log_archive.zip", file_safe(&build_id));
    let outcome = blocking(move || errors_and_archive(errors_file, &log_name)).await?;
    let mut linked = None;
    if let Some((lines, archive)) = outcome {
        // The upload is the report's one remote effect, sent once.
        if client.upload(&bucket, &zip_name, &archive).await.is_ok() {
            linked = Some((lines, zip_name));
        }
    }
    let object = report
        .as_object_mut()
        .ok_or_else(|| ToolFailure::Client(invalid_response()))?;
    if let Some((lines, zip_name)) = linked {
        object.insert("errors_log".to_owned(), json!(lines));
        object.insert(
            "link_to_errors_file".to_owned(),
            Value::String(format!(
                "{}/api/v1/artifacts/artifact/default/{}/{}/{}",
                client.display_url(),
                client.project_id(),
                bucket,
                zip_name
            )),
        );
    } else {
        object.insert("errors_log".to_owned(), json!([]));
        object.insert(
            "link_to_errors_file".to_owned(),
            Value::String("link is not available".to_owned()),
        );
    }
    Ok(report)
}

/// The first 100 lines of the errors log (text mode: universal newlines,
/// strict UTF-8) and a zip of the whole log; `None` when either fails, which
/// is the SDK's inner `except`.
fn errors_and_archive(mut file: File, member: &str) -> Option<(Vec<String>, Vec<u8>)> {
    file.seek(SeekFrom::Start(0)).ok()?;
    let mut lines = Vec::new();
    {
        let mut reader = BufReader::new(&mut file);
        let mut raw = Vec::new();
        for _ in 0..ERROR_LINES {
            raw.clear();
            if reader.read_until(b'\n', &mut raw).ok()? == 0 {
                break;
            }
            if raw.len() > 1_024 * 1_024 {
                return None;
            }
            let text = String::from_utf8(raw.clone()).ok()?;
            lines.push(text.replace("\r\n", "\n").replace('\r', "\n"));
        }
    }
    file.seek(SeekFrom::Start(0)).ok()?;
    let mut writer = zip::ZipWriter::new(Cursor::new(Vec::new()));
    writer
        .start_file(
            member,
            zip::write::SimpleFileOptions::default()
                .compression_method(zip::CompressionMethod::Deflated),
        )
        .ok()?;
    std::io::copy(&mut file, &mut writer).ok()?;
    let archive = writer.finish().ok()?.into_inner();
    (archive.len() <= MAX_FILE_BYTES).then_some((lines, archive))
}

/// A string that is safe inside an object name.
fn file_safe(text: &str) -> String {
    let cleaned: String = text
        .chars()
        .map(|c| {
            if c.is_control() || matches!(c, '/' | '\\' | '"' | '?' | '#') {
                '_'
            } else {
                c
            }
        })
        .collect();
    if cleaned.is_empty() {
        "_".to_owned()
    } else {
        cleaned
    }
}

// ---------------------------------------------------------------------------
// create_excel_report
// ---------------------------------------------------------------------------

/// A pydantic `int`: a JSON integer, an integral float or a decimal string.
#[allow(
    clippy::cast_possible_truncation,
    reason = "the float is integral and under 9e15"
)]
fn lax_int(value: &Value) -> Option<i64> {
    match value {
        Value::Number(number) => number.as_i64().or_else(|| {
            number
                .as_f64()
                .filter(|float| float.fract() == 0.0 && float.abs() < 9.0e15)
                .map(|float| float as i64)
        }),
        Value::String(text) => text.trim().parse::<i64>().ok(),
        _ => None,
    }
}

/// The parameters the caller named (a null counts as not named), merged over
/// the SDK's defaults; `None` when it named none.
fn parameters(arguments: &Map<String, Value>) -> Result<Option<Parameters>, ToolFailure> {
    let mut merged = Parameters::defaults();
    let mut named = false;
    for (key, value) in arguments {
        if value.is_null() {
            continue;
        }
        match key.as_str() {
            "think_time" => {
                value
                    .as_str()
                    .ok_or(ToolFailure::Arguments)?
                    .clone_into(&mut merged.think_time);
            }
            "pct" => value
                .as_str()
                .ok_or(ToolFailure::Arguments)?
                .clone_into(&mut merged.pct),
            "tp_threshold" => merged.tp_threshold = lax_int(value).ok_or(ToolFailure::Arguments)?,
            "rt_threshold" => merged.rt_threshold = lax_int(value).ok_or(ToolFailure::Arguments)?,
            "er_threshold" => merged.er_threshold = lax_int(value).ok_or(ToolFailure::Arguments)?,
            "include_group_pauses" => {
                merged.include_group_pauses = value.as_bool().ok_or(ToolFailure::Arguments)?;
            }
            _ => continue,
        }
        named = true;
    }
    Ok(named.then_some(merged))
}

fn defaults_object() -> Value {
    let defaults = Parameters::defaults();
    json!({
        "think_time":defaults.think_time,
        "pct":defaults.pct,
        "tp_threshold":defaults.tp_threshold,
        "rt_threshold":defaults.rt_threshold,
        "er_threshold":defaults.er_threshold,
        "include_group_pauses":defaults.include_group_pauses,
    })
}

#[derive(Clone, Copy)]
enum LogKind {
    Jmeter,
    Gatling,
}

fn parse_logs(
    files: Vec<Box<dyn Read + Send>>,
    kind: LogKind,
    parameters: &Parameters,
) -> Result<Summary, PerfError> {
    let mut lines = LogLines::new(files);
    match kind {
        LogKind::Jmeter => perf_report::parse_jmeter(&mut lines, &parameters.think_time),
        LogKind::Gatling => perf_report::parse_gatling(
            &mut lines,
            parameters.include_group_pauses,
            &parameters.think_time,
        ),
    }
}

/// `CreateExcelReportTool._run`.
#[expect(
    clippy::too_many_lines,
    reason = "the SDK's one tool, kept in its source order"
)]
pub(super) async fn create_excel_report(
    client: &CarrierClient,
    attachments: Option<&AttachmentHost>,
    arguments: &Map<String, Value>,
) -> ToolResult {
    let report_id = optional_text(arguments, "report_id", MAX_ID_BYTES)?.unwrap_or_default();
    let bucket = optional_text(arguments, "bucket", MAX_ID_BYTES)?.unwrap_or_default();
    let file_name = optional_text(arguments, "file_name", MAX_TEXT_BYTES)?.unwrap_or_default();
    if report_id.is_empty() && (bucket.is_empty() || file_name.is_empty()) {
        return Ok(json!({
            "message":MISSING_INPUT,
            "parameters":{"report_id":null,"bucket":null,"file_name":null},
        }));
    }
    let Some(parameters) = parameters(arguments)? else {
        return Ok(json!({"message":CONFIRM_PARAMETERS,"parameters":defaults_object()}));
    };
    let host = attachments.ok_or(ToolFailure::Arguments)?;

    // 1. The result log(s), the report's own link and the artifact's name.
    let (sources, kind, carrier_report, stem, target_bucket): (
        Vec<Box<dyn Read + Send>>,
        LogKind,
        String,
        String,
        String,
    );
    if report_id.is_empty() {
        kind = if file_name.contains("jtl") {
            LogKind::Jmeter
        } else {
            LogKind::Gatling
        };
        let bytes = client
            .download(
                &[
                    "artifacts",
                    "artifact",
                    "default",
                    client.project_id(),
                    bucket,
                    file_name,
                ],
                &[("integration_id", "1"), ("is_local", "False")],
                CarrierAuth::Bare,
                MAX_LOG_DOWNLOAD_BYTES,
            )
            .await?;
        sources = vec![Box::new(Cursor::new(bytes)) as Box<dyn Read + Send>];
        carrier_report = "not specified".to_owned();
        let date = chrono::Utc::now().format("%Y-%m-%d");
        let base = file_name.rsplit('/').next().unwrap_or(file_name);
        stem = format!("{}_{date}", file_safe(base));
        target_bucket = bucket.to_owned();
    } else {
        let report = report_info(client, report_id).await?;
        kind = match report.get("lg_type").and_then(Value::as_str) {
            Some("gatling") => LogKind::Gatling,
            Some("jmeter") => LogKind::Jmeter,
            _ => return Err(sentence("Unsupported type of backend report")),
        };
        let report_bucket = bucket_of(&report)?;
        let names = archive_names(client, &report, &report_bucket).await?;
        let member = match kind {
            LogKind::Jmeter => Member::Jmeter,
            LogKind::Gatling => Member::Gatling,
        };
        let files = summary_logs(client, &report_bucket, &names, member).await?;
        sources = files
            .into_iter()
            .map(|file| Box::new(file) as Box<dyn Read + Send>)
            .collect();
        carrier_report = format!(
            "{}/-/performance/backend/results?result_id={report_id}",
            client.display_url()
        );
        stem = format!(
            "reports_test_results_{}_excel_report",
            file_safe(&py_str(report.get("build_id")))
        );
        target_bucket = report_bucket;
    }

    // 2. Statistics, thresholds, workbook.
    let summary_parameters = parameters.clone();
    let parsed = blocking(move || {
        parse_logs(sources, kind, &summary_parameters).map(|summary| {
            let verdicts = perf_report::thresholds(&summary, &summary_parameters);
            let (status, justification) =
                perf_report::build_status(&verdicts, &summary_parameters.pct);
            (summary, status, justification)
        })
    })
    .await??;
    let (summary, status, justification) = parsed;
    let summary = Arc::new(summary);
    let carrier_report = Arc::new(carrier_report);
    let justification = Arc::new(justification);
    let bytes =
        build_workbook(move || workbook_bytes(&summary, &carrier_report, status, &justification))
            .await?;

    // 3. Store it, never replacing a file.
    let stored = host
        .write_unique_artifact_creating_bucket(
            &derived_bucket_name("carrier", &target_bucket),
            &stem,
            "xlsx",
            XLSX_MEDIA_TYPE,
            bytes,
        )
        .await?;
    Ok(Value::String(format!(
        "Excel report generated and stored in the project's artifact store, bucket {}, report name: {}, artifact path: /{}/{}",
        stored.bucket, stored.key, stored.bucket, stored.key
    )))
}

/// Why a workbook could not be written.
enum SheetError {
    /// The engine admits a few documents at a time per process and answers at
    /// once; the call waits its turn.
    Busy,
    /// A sentence for the model.
    Refused(&'static str),
}

/// Run the blocking workbook build off the async workers, waiting a short
/// while for the engine's per-process document permits.
async fn build_workbook<F>(make: F) -> Result<Vec<u8>, ToolFailure>
where
    F: Fn() -> Result<Vec<u8>, SheetError> + Send + Sync + 'static,
{
    let make = Arc::new(make);
    let mut attempt = 0;
    loop {
        let run = Arc::clone(&make);
        match blocking(move || run()).await? {
            Ok(bytes) => return Ok(bytes),
            Err(SheetError::Refused(message)) => return Err(sentence(message)),
            Err(SheetError::Busy) if attempt < ENGINE_BUSY_RETRIES => {
                attempt += 1;
                tokio::time::sleep(ENGINE_BUSY_BACKOFF * attempt).await;
            }
            // Still busy after the shared wait: the shared sentence.
            Err(SheetError::Busy) => return Err(sentence(ENGINE_BUSY)),
        }
    }
}

#[cfg(feature = "toolkit-office")]
fn sheet_error(error: &elitea_office_core::OfficeError) -> SheetError {
    use elitea_office_core::OfficeError;
    match error {
        OfficeError::Busy => SheetError::Busy,
        OfficeError::LimitExceeded { .. } => SheetError::Refused(
            "The report is larger than the spreadsheet limits allow; nothing was generated.",
        ),
        OfficeError::DeadlineExceeded => SheetError::Refused(
            "Writing the spreadsheet took too long and was abandoned; nothing was generated.",
        ),
        _ => SheetError::Refused("The spreadsheet could not be written; nothing was generated."),
    }
}

#[cfg(feature = "toolkit-office")]
fn workbook_bytes(
    summary: &Summary,
    carrier_report: &str,
    status: &str,
    justification: &str,
) -> Result<Vec<u8>, SheetError> {
    super::excel::backend_workbook(summary, carrier_report, status, justification)
        .map_err(|error| sheet_error(&error))
}

#[cfg(not(feature = "toolkit-office"))]
fn workbook_bytes(_: &Summary, _: &str, _: &str, _: &str) -> Result<Vec<u8>, SheetError> {
    Err(SheetError::Refused(
        "This runtime has no spreadsheet writer; nothing was generated.",
    ))
}

// ---------------------------------------------------------------------------
// create_ui_excel_report
// ---------------------------------------------------------------------------

/// `CreateUIExcelReportTool._run`.
pub(super) async fn create_ui_excel_report(
    client: &CarrierClient,
    attachments: Option<&AttachmentHost>,
    arguments: &Map<String, Value>,
) -> ToolResult {
    let report_id = optional_text(arguments, "report_id", MAX_ID_BYTES)?.unwrap_or_default();
    if report_id.trim().is_empty() {
        return Ok(Value::String(
            "Please provide me test id for generating excel report from your UI test".to_owned(),
        ));
    }
    let host = attachments.ok_or(ToolFailure::Arguments)?;
    let reports = ui_reports(client).await?;
    let Some(report) = reports
        .iter()
        .find(|report| py_str(report.get("id")) == report_id)
    else {
        return Ok(Value::String(available_reports_message(client).await));
    };

    let Some(uid) = report.get("uid").filter(|uid| py_truthy(uid)) else {
        return Ok(Value::String(format!(
            "\u{274c} **No UID found for report {report_id}. Cannot process this report.**"
        )));
    };
    // The SDK swallows every failure of this lookup into an empty list.
    let links = ui_report_links(client, &py_str(Some(uid)))
        .await
        .unwrap_or_default();
    if links.is_empty() {
        return Ok(Value::String(format!(
            "\u{274c} **No report links found for report {report_id}.**"
        )));
    }

    let mut processed = 0_usize;
    let mut skipped = 0_usize;
    let mut tables: Vec<(String, lighthouse::Table)> = Vec::new();
    let over_limit = links.len().saturating_sub(MAX_UI_FILES);
    for html_url in links.iter().take(MAX_UI_FILES) {
        let json_url = html_url.replace(".html", ".json");
        let mut parts = json_url.rsplit('/');
        let file_name = parts.next().unwrap_or_default().to_owned();
        let file_bucket = parts.next().unwrap_or("reports").to_owned();
        let Ok(content) = ui_json(client, &file_bucket, &file_name).await else {
            skipped += 1;
            continue;
        };
        if !py_truthy(&content) {
            continue;
        }
        processed += 1;
        if let Some(table) = lighthouse::table_from(&content) {
            tables.push((lighthouse::worksheet_name(&file_name), table));
        }
    }
    if processed == 0 {
        return Ok(Value::String(format!(
            "\u{274c} **No JSON files could be processed for report {report_id}.**"
        )));
    }
    if tables.is_empty() {
        return Err(sentence(
            "Error processing UI report: No worksheets were created",
        ));
    }
    let tables = Arc::new(tables);
    let bytes = build_workbook(move || ui_workbook_bytes(&tables)).await?;

    let report_name = report
        .get("name")
        .map_or_else(|| format!("report_{report_id}"), |name| py_str(Some(name)));
    let bucket = report_name.replace(['_', ' '], "").to_lowercase();
    let stem = format!(
        "ui_report_{}_{}",
        file_safe(report_id),
        chrono::Utc::now().format("%Y-%m-%d_%H-%M-%S")
    );
    let stored = host
        .write_unique_artifact_creating_bucket(
            &derived_bucket_name("carrier", &bucket),
            &stem,
            "xlsx",
            XLSX_MEDIA_TYPE,
            bytes,
        )
        .await?;
    let name_shown = report
        .get("name")
        .map_or_else(|| "N/A".to_owned(), |name| py_str(Some(name)));
    let mut notes = String::new();
    if skipped > 0 {
        let _ = writeln!(notes, "- **JSON Files Skipped:** `{skipped}`");
    }
    if over_limit > 0 {
        let _ = writeln!(
            notes,
            "- **Not Read:** `{over_limit}` more JSON files (at most {MAX_UI_FILES} are read)"
        );
    }
    Ok(Value::String(format!(
        "# \u{2705} UI Excel Report Generated Successfully!\n\n## Report Information:\n- **Report ID:** `{report_id}`\n- **Report Name:** `{name_shown}`\n- **JSON Files Processed:** `{processed}`\n- **Excel File:** `{}`\n- **Bucket:** `{}`\n{notes}\n## \u{1f4e5} Stored Artifact:\n`/{}/{}` in the project's artifact store (nothing is uploaded to Carrier).\n\n## \u{1f3af} What's included:\n- Multiple worksheets for each JSON report file\n- Lighthouse performance metrics formatted for analysis (values only: the colour bands, borders and widths of the SDK workbook are not applied)",
        stored.key, stored.bucket, stored.bucket, stored.key
    )))
}

#[cfg(feature = "toolkit-office")]
fn ui_workbook_bytes(tables: &[(String, lighthouse::Table)]) -> Result<Vec<u8>, SheetError> {
    super::excel::ui_workbook(tables).map_err(|error| sheet_error(&error))
}

#[cfg(not(feature = "toolkit-office"))]
fn ui_workbook_bytes(_: &[(String, lighthouse::Table)]) -> Result<Vec<u8>, SheetError> {
    Err(SheetError::Refused(
        "This runtime has no spreadsheet writer; nothing was generated.",
    ))
}

/// `download_ui_report_json`: the report file as JSON, through the session.
async fn ui_json(
    client: &CarrierClient,
    bucket: &str,
    file_name: &str,
) -> Result<Value, CarrierClientError> {
    let response = client
        .send_capped(
            Method::GET,
            &[
                "artifacts",
                "artifact",
                "default",
                client.project_id(),
                bucket,
                file_name,
            ],
            &[],
            super::client::CarrierBody::Empty,
            CarrierAuth::Session,
            false,
            MAX_UI_JSON_BYTES,
        )
        .await?;
    super::client::json_success(&response, false)
}

/// `_show_available_reports_message`.
async fn available_reports_message(client: &CarrierClient) -> String {
    let Ok(reports) = ui_reports(client).await else {
        return "\u{274c} **Error retrieving available report IDs. Please check your report_id and try again.**".to_owned();
    };
    if reports.is_empty() {
        return "\u{274c} **No UI test reports found.**".to_owned();
    }
    let mut message = vec![
        "# \u{274c} No report found for the specified report ID\n".to_owned(),
        "## Available Report IDs:".to_owned(),
    ];
    for report in reports.iter().take(10) {
        message.push(format!(
            "- **Report ID: {}** - {} ({}) - {}",
            py_str(report.get("id")),
            report
                .get("name")
                .map_or_else(|| "Unnamed Report".to_owned(), |name| py_str(Some(name))),
            report
                .get("test_status")
                .map_or_else(|| "Unknown".to_owned(), |status| py_str(Some(status))),
            report
                .get("start_time")
                .map_or_else(String::new, |time| py_str(Some(time))),
        ));
    }
    if reports.len() > 10 {
        message.push(format!("... and {} more reports", reports.len() - 10));
    }
    message.push("\n## \u{1f4a1} Example:".to_owned());
    message.push("```".to_owned());
    message.push(format!(
        "report_id: {}",
        reports
            .first()
            .and_then(|report| report.get("id"))
            .map_or_else(|| "YOUR_REPORT_ID".to_owned(), |id| py_str(Some(id)))
    ));
    message.push("```".to_owned());
    message.join("\n")
}
