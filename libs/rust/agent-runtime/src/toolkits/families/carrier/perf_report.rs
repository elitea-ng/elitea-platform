//! The backend performance statistics behind `create_excel_report`: the SDK's
//! `JMeterReportParser`, `GatlingReportParser`, `calculate_thresholds` and
//! `ExcelReporter.get_build_status_and_justification`, over a bounded stream
//! of log lines instead of a pandas frame.
//!
//! Pure functions of the log text: no network and no spreadsheet types, so
//! they build with or without `toolkit-office`.

// Sample counts, millisecond timestamps and percentiles are bounded by
// `MAX_SAMPLES` and the epoch, far inside f64's exact integer range, and the
// SDK computes the same values in Python floats.
#![allow(
    clippy::cast_precision_loss,
    clippy::cast_possible_truncation,
    clippy::cast_sign_loss,
    clippy::cast_possible_wrap
)]

use std::collections::{HashMap, HashSet, VecDeque};
use std::fmt::Write as _;
use std::io::{BufRead, BufReader, Read};

use chrono::{DateTime, Datelike, Duration, NaiveDate, Weekday};

use crate::toolkits::families::python_repr;

/// The most samples (`JMeter` rows or Gatling request/group lines) one report
/// reads. Each sample costs about 16 bytes while the statistics are computed.
pub(in crate::toolkits) const MAX_SAMPLES: usize = 5_000_000;
/// The most decompressed bytes of log text one report reads.
pub(in crate::toolkits) const MAX_LOG_BYTES: u64 = 1_024 * 1_024 * 1_024;
/// The longest single line or CSV record that is read.
const MAX_LINE_BYTES: usize = 1_024 * 1_024;

/// Why a log could not be turned into statistics. Every variant is one
/// sentence the model can act on; none carries a path or a provider body.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(in crate::toolkits) enum PerfError {
    /// The log has more samples than [`MAX_SAMPLES`].
    TooManySamples,
    /// The log is larger than [`MAX_LOG_BYTES`] once decompressed, or has a
    /// line longer than the line cap.
    TooLarge,
    /// A required `JMeter` column is missing from the header.
    MissingColumn(&'static str),
    /// A value that must be a number is not (the column name).
    BadNumber(&'static str),
    /// A `JMeter` `timeStamp` that is not epoch milliseconds (the SDK converts
    /// `%Y-%m-%d %H:%M:%S` strings through the process's local time zone;
    /// this port does not).
    TimestampFormat,
    /// A `JMeter` label or thread name is empty.
    EmptyName,
    /// The `JMeter` file has no sample rows.
    NoSamples,
    /// The test ran for less than a whole second (the SDK divides by zero).
    ZeroDuration,
    /// A Gatling line that must carry a number does not.
    BadGatlingLine,
    /// The log is not readable text (a read error or a bad archive member).
    Unreadable,
    /// A result archive lacks the log file the report reads.
    LogMissing,
    /// The report has more result archives than this tool downloads.
    TooManyArchives,
}

impl PerfError {
    /// The sentence the model reads.
    pub(in crate::toolkits) const fn sentence(self) -> &'static str {
        match self {
            Self::TooManySamples => {
                "The result log has more samples than this tool reads (5 million); nothing was generated."
            }
            Self::TooLarge => {
                "The result log is larger than this tool reads once decompressed (1 GiB), or has an over-long line; nothing was generated."
            }
            Self::MissingColumn(_) => {
                "The JMeter result file lacks a column the report needs (timeStamp, elapsed, label, responseMessage, success, allThreads, threadName); nothing was generated."
            }
            Self::BadNumber(_) => {
                "The result log has a value that is not a number where the report needs one; nothing was generated."
            }
            Self::TimestampFormat => {
                "The JMeter timeStamp column is not epoch milliseconds, the only form this tool reads; nothing was generated."
            }
            Self::EmptyName => {
                "The JMeter result file has a sample with an empty label or thread name; nothing was generated."
            }
            Self::NoSamples => "The result log has no samples; nothing was generated.",
            Self::ZeroDuration => {
                "The test lasted less than one second, so no throughput can be computed; nothing was generated."
            }
            Self::BadGatlingLine => {
                "The Gatling simulation log has a line whose timestamp is not a number; nothing was generated."
            }
            Self::Unreadable => "The result log could not be read; nothing was generated.",
            Self::LogMissing => {
                "A result archive of the report does not contain its log file (jmeter.jtl, or simulation.log in the newest folder); nothing was generated."
            }
            Self::TooManyArchives => {
                "The report has more than 20 result archives, more than this tool downloads; nothing was generated."
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Line source
// ---------------------------------------------------------------------------

/// The result logs of one report read as one stream of lines: the SDK's
/// `merge_log_files` writes every file whole except that the header (first
/// line) of the second and later files is dropped.
pub(in crate::toolkits) struct LogLines {
    pending: VecDeque<Box<dyn Read + Send>>,
    current: Option<BufReader<Box<dyn Read + Send>>>,
    skip_header: bool,
    consumed: u64,
}

impl LogLines {
    pub(in crate::toolkits) fn new(sources: Vec<Box<dyn Read + Send>>) -> Self {
        Self {
            pending: sources.into(),
            current: None,
            skip_header: false,
            consumed: 0,
        }
    }

    /// The next line without its `\n`/`\r\n`, or `None` at the end.
    fn next_line(&mut self, line: &mut Vec<u8>) -> Result<bool, PerfError> {
        loop {
            if self.current.is_none() {
                let Some(source) = self.pending.pop_front() else {
                    return Ok(false);
                };
                self.current = Some(BufReader::new(source));
                if self.skip_header {
                    let mut header = Vec::new();
                    if !self.read_physical(&mut header)? {
                        self.current = None;
                        continue;
                    }
                }
                self.skip_header = true;
            }
            line.clear();
            if self.read_physical(line)? {
                while matches!(line.last(), Some(b'\n' | b'\r')) {
                    line.pop();
                }
                return Ok(true);
            }
            self.current = None;
        }
    }

    fn read_physical(&mut self, line: &mut Vec<u8>) -> Result<bool, PerfError> {
        let Some(reader) = self.current.as_mut() else {
            return Ok(false);
        };
        // One byte past the cap tells an over-long line from a full one.
        let limit = u64::try_from(MAX_LINE_BYTES + 1).unwrap_or(u64::MAX);
        let read = reader
            .by_ref()
            .take(limit)
            .read_until(b'\n', line)
            .map_err(|_| PerfError::Unreadable)?;
        if read == 0 {
            return Ok(false);
        }
        if line.len() > MAX_LINE_BYTES {
            return Err(PerfError::TooLarge);
        }
        self.consumed = self.consumed.saturating_add(read as u64);
        if self.consumed > MAX_LOG_BYTES {
            return Err(PerfError::TooLarge);
        }
        Ok(true)
    }
}

// ---------------------------------------------------------------------------
// Statistics
// ---------------------------------------------------------------------------

/// One row of the report: the SDK's per-request statistics dict.
#[derive(Clone, Debug, PartialEq)]
pub(in crate::toolkits) struct Stats {
    pub(in crate::toolkits) name: String,
    pub(in crate::toolkits) total: u64,
    pub(in crate::toolkits) ko: u64,
    /// `Error%`, a fraction rounded to four places.
    pub(in crate::toolkits) error_fraction: f64,
    pub(in crate::toolkits) min: f64,
    pub(in crate::toolkits) average: f64,
    pub(in crate::toolkits) median: Option<f64>,
    pub(in crate::toolkits) p90: f64,
    pub(in crate::toolkits) p95: f64,
    pub(in crate::toolkits) max: f64,
    /// `JMeter` keeps an `OK` count; Gatling does not.
    pub(in crate::toolkits) ok: Option<u64>,
}

impl Stats {
    /// `results['requests'][req][key]` as the SDK's `float(...)` reads it;
    /// `None` for a key the dict does not have or whose value is text.
    fn value(&self, key: &str) -> Option<f64> {
        match key {
            "min" => Some(self.min),
            "max" => Some(self.max),
            "average" => Some(self.average),
            "median" => self.median,
            "90Pct" => Some(self.p90),
            "95Pct" => Some(self.p95),
            "Total" => Some(self.total as f64),
            "KO" => Some(self.ko as f64),
            "OK" => self.ok.map(|ok| ok as f64),
            "Error%" => Some(self.error_fraction),
            _ => None,
        }
    }
}

/// A cell of the report's title block.
#[derive(Clone, Debug, PartialEq)]
pub(in crate::toolkits) enum Cell {
    Text(String),
    Number(f64),
    Empty,
}

/// The parser's result: the SDK's `transactions_data` dict.
#[derive(Clone, Debug, PartialEq)]
pub(in crate::toolkits) struct Summary {
    /// `requests` in the order the SDK's dict holds them.
    pub(in crate::toolkits) requests: Vec<Stats>,
    pub(in crate::toolkits) max_user_count: u64,
    pub(in crate::toolkits) ramp_up_period: Cell,
    pub(in crate::toolkits) duration: Cell,
    pub(in crate::toolkits) think_time: String,
    pub(in crate::toolkits) date_start: Cell,
    pub(in crate::toolkits) date_end: Cell,
    pub(in crate::toolkits) throughput: f64,
    /// `JMeter`: percent; Gatling: a fraction (the SDK passes it through).
    pub(in crate::toolkits) error_rate: f64,
}

/// Python's `round(x, n)`: correctly rounded, ties to even on the exact value.
fn py_round(value: f64, places: usize) -> f64 {
    if !value.is_finite() {
        return value;
    }
    format!("{value:.places$}").parse().unwrap_or(value)
}

/// numpy's `round(x, n)`: scale, round half to even, scale back.
fn np_round(value: f64, places: i32) -> f64 {
    let factor = 10_f64.powi(places);
    (value * factor).round_ties_even() / factor
}

/// Linear-interpolation quantile of an ascending slice (pandas `quantile`,
/// numpy `percentile`).
fn quantile(sorted: &[f64], fraction: f64) -> f64 {
    let last = sorted.len().saturating_sub(1);
    let position = last as f64 * fraction;
    let low = position.floor();
    let high = position.ceil();
    let (low_index, high_index) = (low as usize, high as usize);
    let lower = sorted.get(low_index).copied().unwrap_or(0.0);
    let upper = sorted.get(high_index.min(last)).copied().unwrap_or(lower);
    lower + (upper - lower) * (position - low)
}

/// `str(pd.to_timedelta(ms, unit='ms')).split('days')[1].split('.')[0]`: the
/// clock part with the leading space the SDK leaves in.
fn clock_text(milliseconds: f64) -> String {
    let total_seconds = (milliseconds / 1000.0).floor().max(0.0) as u64;
    let seconds_of_day = total_seconds % 86_400;
    format!(
        " {:02}:{:02}:{:02}",
        seconds_of_day / 3_600,
        seconds_of_day % 3_600 / 60,
        seconds_of_day % 60
    )
}

// ---------------------------------------------------------------------------
// JMeter
// ---------------------------------------------------------------------------

/// pandas' default `na_values`.
const NA_VALUES: [&str; 19] = [
    "", "#N/A", "#N/A N/A", "#NA", "-1.#IND", "-1.#QNAN", "-NaN", "-nan", "1.#IND", "1.#QNAN",
    "<NA>", "N/A", "NA", "NULL", "NaN", "None", "n/a", "nan", "null",
];

fn is_na(value: &str) -> bool {
    NA_VALUES.contains(&value)
}

/// One CSV record from the line stream: a quoted field may span lines.
fn csv_record(
    lines: &mut LogLines,
    buffer: &mut Vec<u8>,
) -> Result<Option<Vec<String>>, PerfError> {
    let mut record: Vec<u8> = Vec::new();
    loop {
        if !lines.next_line(buffer)? {
            if record.is_empty() {
                return Ok(None);
            }
            break;
        }
        if record.is_empty() && buffer.is_empty() {
            // pandas skips blank lines.
            continue;
        }
        if !record.is_empty() {
            record.push(b'\n');
        }
        record.extend_from_slice(buffer);
        if record.len() > MAX_LINE_BYTES {
            return Err(PerfError::TooLarge);
        }
        let quotes = record
            .iter()
            .fold(0_usize, |count, byte| count + usize::from(*byte == b'"'));
        if quotes % 2 == 0 {
            break;
        }
    }
    Ok(Some(split_csv(&record)))
}

fn split_csv(record: &[u8]) -> Vec<String> {
    let mut fields = Vec::new();
    let mut field: Vec<u8> = Vec::new();
    let mut quoted = false;
    let mut index = 0;
    while index < record.len() {
        let byte = record[index];
        if quoted {
            if byte == b'"' {
                if record.get(index + 1) == Some(&b'"') {
                    field.push(b'"');
                    index += 1;
                } else {
                    quoted = false;
                }
            } else {
                field.push(byte);
            }
        } else if byte == b'"' && field.is_empty() {
            quoted = true;
        } else if byte == b',' {
            fields.push(String::from_utf8_lossy(&field).into_owned());
            field.clear();
        } else {
            field.push(byte);
        }
        index += 1;
    }
    fields.push(String::from_utf8_lossy(&field).into_owned());
    fields
}

/// Per-label accumulation of the `JMeter` rows.
#[derive(Default)]
struct Samples {
    elapsed: Vec<f64>,
    errors: u64,
}

impl Samples {
    fn stats(&self, name: &str) -> Result<Stats, PerfError> {
        if self.elapsed.is_empty() {
            return Err(PerfError::NoSamples);
        }
        let mut sorted = self.elapsed.clone();
        sorted.sort_by(f64::total_cmp);
        let total = sorted.len() as u64;
        let ok = total - self.errors;
        let mean = sorted.iter().sum::<f64>() / sorted.len() as f64;
        let fraction = py_round(self.errors as f64 / total as f64, 4);
        Ok(Stats {
            name: name.to_owned(),
            total,
            ko: self.errors,
            error_fraction: fraction,
            min: py_round(sorted[0], 3),
            average: py_round(mean, 3),
            median: Some(py_round(quantile(&sorted, 0.5), 3)),
            p90: np_round(quantile(&sorted, 0.9), 3),
            p95: np_round(quantile(&sorted, 0.95), 3),
            max: py_round(sorted[sorted.len() - 1], 3),
            ok: Some(ok),
        })
    }
}

/// What the summary needs of the rows that are not transaction controllers.
#[derive(Default)]
struct Overall {
    rows: u64,
    errors: u64,
    first_timestamp: f64,
    last_timestamp: f64,
    max_threads: f64,
    ramp_end_timestamp: f64,
    threads: HashSet<String>,
}

fn number(value: &str, column: &'static str) -> Result<f64, PerfError> {
    value
        .trim()
        .parse::<f64>()
        .ok()
        .filter(|parsed| parsed.is_finite())
        .ok_or(PerfError::BadNumber(column))
}

/// One parsed `JMeter` row.
struct Row {
    label: usize,
    timestamp: f64,
    elapsed: f64,
    success: Option<bool>,
    transaction: bool,
    threads: f64,
    thread: String,
}

/// The SDK's `JMeterReportParser.parse` over a `.jtl` CSV.
#[expect(
    clippy::too_many_lines,
    reason = "one SDK parser, kept in its source order"
)]
pub(in crate::toolkits) fn parse_jmeter(
    lines: &mut LogLines,
    think_time: &str,
) -> Result<Summary, PerfError> {
    let mut buffer = Vec::new();
    let header = csv_record(lines, &mut buffer)?.ok_or(PerfError::NoSamples)?;
    let column = |name: &'static str| {
        header
            .iter()
            .position(|field| field == name)
            .ok_or(PerfError::MissingColumn(name))
    };
    let time_stamp = column("timeStamp")?;
    let elapsed = column("elapsed")?;
    let label = column("label")?;
    let message = column("responseMessage")?;
    let success = column("success")?;
    let all_threads = column("allThreads")?;
    let thread_name = column("threadName")?;

    // First pass state. `success` is a pandas bool column only when every
    // value is a boolean word; otherwise nothing equals `True`.
    let mut labels: Vec<String> = Vec::new();
    let mut label_index: HashMap<String, usize> = HashMap::new();
    let mut rows: Vec<Row> = Vec::new();
    let mut all_message_null = true;
    let mut success_all_boolean = true;
    while let Some(record) = csv_record(lines, &mut buffer)? {
        if rows.len() >= MAX_SAMPLES {
            return Err(PerfError::TooManySamples);
        }
        let field = |index: usize| record.get(index).map_or("", String::as_str);
        let label_text = field(label);
        if is_na(label_text) {
            return Err(PerfError::EmptyName);
        }
        let index = *label_index.entry(label_text.to_owned()).or_insert_with(|| {
            labels.push(label_text.to_owned());
            labels.len() - 1
        });
        let message_text = field(message);
        let message_null = is_na(message_text);
        if !message_null {
            all_message_null = false;
        }
        let transaction = !message_null
            && message_text
                .get(.."number of samples".len())
                .is_some_and(|prefix| prefix.eq_ignore_ascii_case("number of samples"));
        let success_text = field(success);
        let success_value = match success_text {
            "true" | "True" | "TRUE" => Some(true),
            "false" | "False" | "FALSE" => Some(false),
            other => {
                if !is_na(other) {
                    success_all_boolean = false;
                }
                None
            }
        };
        rows.push(Row {
            label: index,
            timestamp: field(time_stamp)
                .trim()
                .parse::<f64>()
                .map_err(|_| PerfError::TimestampFormat)?,
            elapsed: number(field(elapsed), "elapsed")?,
            success: success_value,
            transaction,
            threads: number(field(all_threads), "allThreads")?,
            thread: field(thread_name).to_owned(),
        });
    }
    if rows.is_empty() {
        return Err(PerfError::NoSamples);
    }
    let is_error = |row: &Row| {
        if success_all_boolean {
            row.success != Some(true)
        } else {
            true
        }
    };

    let mut per_label: Vec<Samples> = labels.iter().map(|_| Samples::default()).collect();
    let mut everything = Samples::default();
    let mut overall = Overall {
        first_timestamp: f64::INFINITY,
        last_timestamp: f64::NEG_INFINITY,
        max_threads: f64::NEG_INFINITY,
        ramp_end_timestamp: f64::INFINITY,
        ..Overall::default()
    };
    let mut summary_elapsed = Vec::new();
    for row in &rows {
        let error = is_error(row);
        let samples = &mut per_label[row.label];
        samples.elapsed.push(row.elapsed);
        everything.elapsed.push(row.elapsed);
        if error {
            samples.errors += 1;
            everything.errors += 1;
        }
        if all_message_null || !row.transaction {
            overall.rows += 1;
            summary_elapsed.push(row.elapsed);
            if error {
                overall.errors += 1;
            }
            overall.first_timestamp = overall.first_timestamp.min(row.timestamp);
            overall.last_timestamp = overall.last_timestamp.max(row.timestamp);
            overall.threads.insert(row.thread.clone());
        }
    }
    if overall.rows == 0 {
        return Err(PerfError::NoSamples);
    }
    for row in &rows {
        if all_message_null || !row.transaction {
            if row.threads > overall.max_threads {
                overall.max_threads = row.threads;
                overall.ramp_end_timestamp = row.timestamp;
            } else if (row.threads - overall.max_threads).abs() < f64::EPSILON {
                overall.ramp_end_timestamp = overall.ramp_end_timestamp.min(row.timestamp);
            }
        }
    }
    if overall.threads.iter().any(String::is_empty) {
        return Err(PerfError::EmptyName);
    }

    let mut requests = Vec::with_capacity(labels.len() + 1);
    for (name, samples) in labels.iter().zip(&per_label) {
        requests.push(samples.stats(name)?);
    }
    requests.push(everything.stats("Total Transactions")?);

    let duration_ms = overall.last_timestamp - overall.first_timestamp;
    let seconds_component = (duration_ms / 1000.0).floor().max(0.0) as u64 % 86_400;
    if seconds_component == 0 {
        return Err(PerfError::ZeroDuration);
    }
    let total = overall.rows;
    let ok = total - overall.errors;
    let throughput = py_round(ok as f64 / seconds_component as f64, 2);
    let error_fraction = py_round(overall.errors as f64 / total as f64, 4);
    let user_count = overall
        .threads
        .iter()
        .filter(|name| !name.starts_with("parallel"))
        .count() as u64;
    Ok(Summary {
        requests,
        max_user_count: user_count,
        ramp_up_period: Cell::Text(clock_text(
            overall.ramp_end_timestamp - overall.first_timestamp,
        )),
        duration: Cell::Text(clock_text(duration_ms)),
        think_time: think_time.to_owned(),
        date_start: Cell::Text(utc_text(overall.first_timestamp)),
        date_end: Cell::Text(utc_text(overall.last_timestamp)),
        throughput,
        error_rate: error_fraction * 100.0,
    })
}

/// `datetime.fromtimestamp(int(ms / 1000)).strftime(...)` with the worker's
/// clock in UTC.
fn utc_text(milliseconds: f64) -> String {
    let seconds = (milliseconds / 1000.0) as i64;
    DateTime::from_timestamp(seconds, 0).map_or_else(String::new, |moment| {
        moment.format("%Y-%m-%d %H:%M:%S").to_string()
    })
}

// ---------------------------------------------------------------------------
// Gatling
// ---------------------------------------------------------------------------

/// `US/Eastern` wall-clock text for a UTC instant, under the US daylight
/// saving rules (second Sunday of March to first Sunday of November since
/// 2007; first Sunday of April to last Sunday of October before).
fn eastern_text(milliseconds: i64) -> String {
    let seconds = milliseconds.div_euclid(1000);
    let Some(utc) = DateTime::from_timestamp(seconds, 0) else {
        return String::new();
    };
    let utc = utc.naive_utc();
    let year = utc.year();
    let nth_sunday = |month: u32, nth: u32| {
        let mut day = NaiveDate::from_ymd_opt(year, month, 1)?;
        while day.weekday() != Weekday::Sun {
            day = day.succ_opt()?;
        }
        day.checked_add_signed(Duration::days(i64::from(nth - 1) * 7))
    };
    let last_sunday = |month: u32| {
        let mut day = NaiveDate::from_ymd_opt(year, month + 1, 1)?.pred_opt()?;
        while day.weekday() != Weekday::Sun {
            day = day.pred_opt()?;
        }
        Some(day)
    };
    let (start, end) = if year >= 2007 {
        (nth_sunday(3, 2), nth_sunday(11, 1))
    } else {
        (nth_sunday(4, 1), last_sunday(10))
    };
    // DST starts at 02:00 EST (07:00 UTC) and ends at 02:00 EDT (06:00 UTC).
    let daylight = match (start, end) {
        (Some(start), Some(end)) => {
            let begins = start.and_hms_opt(7, 0, 0);
            let ends = end.and_hms_opt(6, 0, 0);
            matches!((begins, ends), (Some(begins), Some(ends)) if utc >= begins && utc < ends)
        }
        _ => false,
    };
    let offset = if daylight { 4 } else { 5 };
    (utc - Duration::hours(offset))
        .format("%Y-%m-%d %H:%M:%S")
        .to_string()
}

fn gatling_millis(text: &str) -> Result<i64, PerfError> {
    text.trim()
        .parse::<i64>()
        .map_err(|_| PerfError::BadGatlingLine)
}

/// Python's `datetime`-difference minutes: `round(seconds / 60, 3)`.
fn minutes_between(start: Option<i64>, end: Option<i64>) -> f64 {
    match (start, end) {
        (Some(start), Some(end)) => py_round((end - start) as f64 / 1000.0 / 60.0, 3),
        _ => 0.0,
    }
}

fn gatling_stats(name: &str, entries: &[(i64, bool)]) -> Stats {
    let total = entries.len() as u64;
    let ko = entries.iter().filter(|(_, ok)| !*ok).count() as u64;
    let error_fraction = if total > 0 {
        py_round(ko as f64 / total as f64, 4)
    } else {
        0.0
    };
    if entries.is_empty() {
        return Stats {
            name: name.to_owned(),
            total,
            ko,
            error_fraction,
            min: 0.0,
            average: 0.0,
            median: None,
            p90: 0.0,
            p95: 0.0,
            max: 0.0,
            ok: None,
        };
    }
    let mut times: Vec<f64> = entries.iter().map(|(time, _)| *time as f64).collect();
    times.sort_by(f64::total_cmp);
    let mean = times.iter().sum::<f64>() / times.len() as f64;
    Stats {
        name: name.to_owned(),
        total,
        ko,
        error_fraction,
        min: times[0],
        average: np_round(mean, 3),
        median: None,
        p90: np_round(quantile(&times, 0.9), 3),
        p95: np_round(quantile(&times, 0.95), 3),
        max: times[times.len() - 1],
        ok: None,
    }
}

/// The SDK's `GatlingReportParser.parse` over a `simulation.log`, with the
/// groups merged into the requests the way `_parse_report` does.
#[expect(
    clippy::too_many_lines,
    reason = "one SDK parser, kept in its source order"
)]
pub(in crate::toolkits) fn parse_gatling(
    lines: &mut LogLines,
    include_group_pauses: bool,
    think_time: &str,
) -> Result<Summary, PerfError> {
    let mut groups: Vec<(String, Vec<(i64, bool)>)> = Vec::new();
    let mut group_index: HashMap<String, usize> = HashMap::new();
    let mut requests: Vec<(String, Vec<(i64, bool)>)> = Vec::new();
    let mut request_index: HashMap<String, usize> = HashMap::new();
    let mut users = 0_u64;
    let mut date_start: Option<i64> = None;
    let mut date_end: Option<i64> = None;
    let mut ramp_start: Option<i64> = None;
    let mut ramp_end: Option<i64> = None;
    let mut samples = 0_usize;
    let mut buffer = Vec::new();
    while lines.next_line(&mut buffer)? {
        let line = String::from_utf8_lossy(&buffer).into_owned();
        let parts: Vec<&str> = line.split('\t').collect();
        if date_start.is_none() {
            if line.contains("ASSERTION") {
                continue;
            }
            date_start = Some(gatling_millis(parts.get(3).copied().unwrap_or(""))?);
        }
        if let Some(millis) = parts
            .get(3)
            .and_then(|part| part.trim().parse::<i64>().ok())
        {
            date_end = Some(millis);
        }
        if line.starts_with("REQUEST") {
            if parts.len() >= 7 {
                samples += 1;
                if samples > MAX_SAMPLES {
                    return Err(PerfError::TooManySamples);
                }
                let start = gatling_millis(parts[3])?;
                let end = gatling_millis(parts[4])?;
                let slot = *request_index.entry(parts[2].to_owned()).or_insert_with(|| {
                    requests.push((parts[2].to_owned(), Vec::new()));
                    requests.len() - 1
                });
                requests[slot]
                    .1
                    .push((end - start, parts[5].trim() == "OK"));
            }
        } else if line.starts_with("USER") && line.contains("START") {
            users += 1;
            let millis = gatling_millis(parts.get(3).copied().unwrap_or(""))?;
            if ramp_start.is_none() {
                ramp_start = Some(millis);
            } else {
                ramp_end = Some(millis);
            }
        } else if line.starts_with("GROUP") && parts.len() >= 6 {
            samples += 1;
            if samples > MAX_SAMPLES {
                return Err(PerfError::TooManySamples);
            }
            let time = if include_group_pauses {
                gatling_millis(parts[3])? - gatling_millis(parts[2])?
            } else {
                gatling_millis(parts[4])?
            };
            let slot = *group_index.entry(parts[1].to_owned()).or_insert_with(|| {
                groups.push((parts[1].to_owned(), Vec::new()));
                groups.len() - 1
            });
            groups[slot].1.push((time, parts[5].trim() == "OK"));
        }
    }
    if date_start.is_none() {
        return Err(PerfError::NoSamples);
    }

    let mut duration = minutes_between(date_start, date_end);
    let mut ramp_up = minutes_between(ramp_start, ramp_end);
    let all_entries: Vec<(i64, bool)> = requests
        .iter()
        .flat_map(|(_, entries)| entries.iter().copied())
        .collect();
    let total_stats = gatling_stats("Total", &all_entries);

    let mut stats: Vec<Stats> = requests
        .iter()
        .map(|(name, entries)| gatling_stats(name, entries))
        .collect();
    stats.push(Stats {
        name: "Total Requests".to_owned(),
        ..total_stats.clone()
    });
    stats.extend(
        groups
            .iter()
            .map(|(name, entries)| gatling_stats(name, entries)),
    );

    if duration == 0.0 {
        // `total / (duration * 60)` divides by zero in the SDK.
        return Err(PerfError::ZeroDuration);
    }
    let mut throughput = total_stats.total as f64 / (duration * 60.0);
    if duration > 1.0 {
        duration = duration.round_ties_even();
    }
    if ramp_up > 1.0 {
        ramp_up = py_round(ramp_up, 1);
        throughput = total_stats.total as f64 / (duration * 60.0);
    }
    if throughput > 1.0 {
        throughput = py_round(throughput, 1);
    }
    Ok(Summary {
        requests: stats,
        max_user_count: users,
        ramp_up_period: Cell::Number(ramp_up),
        duration: Cell::Number(duration),
        think_time: think_time.to_owned(),
        date_start: date_start.map_or(Cell::Empty, |millis| Cell::Text(eastern_text(millis))),
        date_end: date_end.map_or(Cell::Empty, |millis| Cell::Text(eastern_text(millis))),
        throughput,
        error_rate: total_stats.error_fraction,
    })
}

// ---------------------------------------------------------------------------
// Thresholds, build status
// ---------------------------------------------------------------------------

/// The user's thresholds (pydantic `int`s) and percentile key.
#[derive(Clone, Debug)]
pub(in crate::toolkits) struct Parameters {
    pub(in crate::toolkits) think_time: String,
    pub(in crate::toolkits) pct: String,
    pub(in crate::toolkits) tp_threshold: i64,
    pub(in crate::toolkits) rt_threshold: i64,
    pub(in crate::toolkits) er_threshold: i64,
    pub(in crate::toolkits) include_group_pauses: bool,
}

impl Parameters {
    /// The SDK's `_get_default_parameters`.
    pub(in crate::toolkits) fn defaults() -> Self {
        Self {
            think_time: "2,0-5,0".to_owned(),
            pct: "95Pct".to_owned(),
            tp_threshold: 10,
            rt_threshold: 500,
            er_threshold: 5,
            include_group_pauses: false,
        }
    }
}

/// One threshold verdict. The SDK also records a `scope`, which the report
/// never reads.
#[derive(Clone, Debug, PartialEq)]
pub(in crate::toolkits) struct Threshold {
    pub(in crate::toolkits) target: &'static str,
    pub(in crate::toolkits) value: f64,
    pub(in crate::toolkits) limit: i64,
    pub(in crate::toolkits) failed: bool,
}

/// `calculate_thresholds`; `None` when the SDK would raise (an unknown or
/// non-numeric `pct`), which `_calculate_thresholds` turns into `[]`.
pub(in crate::toolkits) fn thresholds(
    summary: &Summary,
    parameters: &Parameters,
) -> Vec<Threshold> {
    let mut verdicts = vec![
        Threshold {
            target: "throughput",
            value: summary.throughput,
            limit: parameters.tp_threshold,
            failed: summary.throughput < parameters.tp_threshold as f64,
        },
        Threshold {
            target: "error_rate",
            value: summary.error_rate,
            limit: parameters.er_threshold,
            failed: summary.error_rate > parameters.er_threshold as f64,
        },
    ];
    for request in &summary.requests {
        let Some(value) = request.value(&parameters.pct) else {
            return Vec::new();
        };
        verdicts.push(Threshold {
            target: "response_time",
            value,
            limit: parameters.rt_threshold,
            failed: value > parameters.rt_threshold as f64,
        });
    }
    verdicts
}

/// `ExcelReporter.get_build_status_and_justification`, with the SDK's
/// fallback to `("SUCCESS", "")` when it raises (no thresholds to read).
pub(in crate::toolkits) fn build_status(
    verdicts: &[Threshold],
    pct: &str,
) -> (&'static str, String) {
    let Some(response_threshold) = verdicts
        .iter()
        .find(|verdict| verdict.target == "response_time")
        .map(|verdict| verdict.limit)
    else {
        return ("SUCCESS", String::new());
    };
    let seconds = python_repr::repr_float(response_threshold as f64 / 1000.0);
    let mut justification = String::new();
    let mut status = "SUCCESS";
    for verdict in verdicts {
        if verdict.target == "error_rate" {
            if verdict.failed {
                justification = format!(
                    "Total error rate exceed the threshold of {}% ",
                    verdict.limit
                );
                status = "FAILED";
            } else {
                justification = format!(
                    "Total error rate doesn't exceed the threshold of {}% ",
                    verdict.limit
                );
            }
        }
        if verdict.target == "response_time" && verdict.failed {
            if status == "FAILED" {
                let _ = write!(
                    justification,
                    "and Response Time for some transaction(s) exceed the threshold of {seconds} seconds by {pct}"
                );
            } else {
                status = "FAILED";
                justification = format!(
                    "Response Time for some transaction(s) exceed the threshold of {seconds} seconds by {pct}"
                );
            }
            return (status, justification);
        }
    }
    let _ = write!(
        justification,
        "and Response Time for all transactions doesn't exceed the threshold of {seconds} seconds by {pct}"
    );
    (status, justification)
}

/// The report's title rows in the SDK's order: `(label, value)`.
pub(in crate::toolkits) fn title_rows(
    summary: &Summary,
    carrier_report: &str,
    status: &str,
    justification: &str,
) -> Vec<(&'static str, Cell)> {
    vec![
        ("Users", Cell::Number(summary.max_user_count as f64)),
        ("Ramp Up, min", summary.ramp_up_period.clone()),
        ("Duration, min", summary.duration.clone()),
        ("Think time, sec", Cell::Text(summary.think_time.clone())),
        ("Start Date, EST", summary.date_start.clone()),
        ("End Date, EST", summary.date_end.clone()),
        ("Throughput, req/sec", Cell::Number(summary.throughput)),
        ("Error rate, %", Cell::Number(summary.error_rate)),
        ("Carrier report", Cell::Text(carrier_report.to_owned())),
        ("Build status", Cell::Text(status.to_owned())),
        ("Justification", Cell::Text(justification.to_owned())),
    ]
}

/// The report's column headers, in order.
pub(in crate::toolkits) const HEADERS: [&str; 9] = [
    "Transaction",
    "Req, count",
    "KO, count",
    "KO, %",
    "Min, sec",
    "Avg, sec",
    "90p, sec",
    "95p, sec",
    "Max, sec",
];

/// One data row of the report: `round(value / 1000, 3)` for the timings.
pub(in crate::toolkits) fn data_row(stats: &Stats) -> Vec<Cell> {
    let seconds = |value: f64| Cell::Number(py_round(value / 1000.0, 3));
    vec![
        Cell::Text(stats.name.clone()),
        Cell::Number(stats.total as f64),
        Cell::Number(stats.ko as f64),
        Cell::Number(stats.error_fraction),
        seconds(stats.min),
        seconds(stats.average),
        seconds(stats.p90),
        seconds(stats.p95),
        seconds(stats.max),
    ]
}

#[cfg(test)]
pub(in crate::toolkits) fn eastern_for_tests(milliseconds: i64) -> String {
    eastern_text(milliseconds)
}
