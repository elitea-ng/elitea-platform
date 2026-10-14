//! The data side of `create_ui_excel_report`: the SDK's
//! `LighthouseExcelReporter._process_lighthouse_data` plus the pandas
//! `pivot_table(index="Step name", columns="Audit", values="Numeric Value",
//! aggfunc="mean")` it feeds to the worksheet, and the worksheet naming.
//!
//! Pure functions of the Lighthouse JSON: no spreadsheet types.

use std::collections::{BTreeMap, BTreeSet};

use serde_json::Value;

use crate::toolkits::families::python_repr;

/// The audits the SDK extracts from every step.
const PERFORMANCE_AUDITS: [&str; 10] = [
    "first-contentful-paint",
    "speed-index",
    "interactive",
    "total-blocking-time",
    "largest-contentful-paint",
    "cumulative-layout-shift",
    "network-requests",
    "bootup-time",
    "interaction-to-next-paint",
    "server-response-time",
];

/// Audit names that get the `, sec` suffix once renamed.
const SEC_AUDITS: [&str; 7] = [
    "first-contentful-paint",
    "interactive",
    "largest-contentful-paint",
    "mainthread-work-breakdown",
    "network-requests",
    "speed-index",
    "javaScript-execution-time",
];

/// Audit names that get the `, ms` suffix once renamed.
const MS_AUDITS: [&str; 3] = [
    "interaction-to-next-paint",
    "total-blocking-time",
    "time-to-first-byte",
];

/// The pivoted worksheet: audits across, steps down, both sorted ascending.
#[derive(Clone, Debug, PartialEq)]
pub(in crate::toolkits) struct Table {
    pub(in crate::toolkits) columns: Vec<String>,
    /// `(step name, one cell per column)`; `None` is the empty string the SDK
    /// writes as an empty cell.
    pub(in crate::toolkits) rows: Vec<(String, Vec<Option<f64>>)>,
}

/// The audit's column name: renamed, then suffixed.
fn display_name(audit: &str) -> String {
    let renamed = match audit {
        "bootup-time" => "javaScript-execution-time",
        "server-response-time" => "time-to-first-byte",
        other => other,
    };
    if SEC_AUDITS.contains(&renamed) {
        format!("{renamed}, sec")
    } else if MS_AUDITS.contains(&renamed) {
        format!("{renamed}, ms")
    } else {
        renamed.to_owned()
    }
}

/// `re.sub(r'[a-zA-Z\s]', '', text)`, then the comma strip and `float()`;
/// `None` for an empty remainder or a non-number.
fn numeric_value(display: &Value) -> Option<f64> {
    let text = python_repr::str_of(display);
    let kept: String = text
        .chars()
        .filter(|c| !c.is_ascii_alphabetic() && !c.is_whitespace())
        .collect();
    if kept.is_empty() {
        return None;
    }
    kept.replace(',', "")
        .parse::<f64>()
        .ok()
        .filter(|v| v.is_finite())
}

/// The pivot of one Lighthouse JSON document, or `None` when the SDK would
/// raise while reading it (it logs that and adds no worksheet).
pub(in crate::toolkits) fn table_from(data: &Value) -> Option<Table> {
    let data = data.as_object()?;
    let single;
    let steps: &[Value] = match data.get("steps") {
        None | Some(Value::Null) => &[],
        Some(Value::Array(steps)) => steps,
        Some(_) => return None,
    };
    let steps = if steps.is_empty() {
        // "If no steps, treat the entire data as a single step".
        single = [serde_json::json!({
            "name":"main_report",
            "lhr":Value::Object(data.clone())
        })];
        &single[..]
    } else {
        steps
    };

    // (step, audit) -> (sum, count) of the numeric values.
    let mut cells: BTreeMap<(String, String), (f64, u32)> = BTreeMap::new();
    for (index, step) in steps.iter().enumerate() {
        let step = step.as_object()?;
        let name = step
            .get("name")
            .map_or_else(|| format!("step_{index}"), python_repr::str_of);
        let lhr = match step.get("lhr") {
            None => None,
            Some(Value::Object(lhr)) => Some(lhr),
            Some(_) => return None,
        };
        for audit in PERFORMANCE_AUDITS {
            let display = lhr
                .and_then(|lhr| lhr.get("audits"))
                .and_then(Value::as_object)
                .and_then(|audits| audits.get(audit))
                .and_then(Value::as_object)
                .and_then(|result| result.get("displayValue"))
                .filter(|value| !value.is_null());
            let number = display.and_then(numeric_value);
            if let Some(number) = number {
                let entry = cells
                    .entry((name.clone(), display_name(audit)))
                    .or_insert((0.0, 0));
                entry.0 += number;
                entry.1 += 1;
            }
        }
    }

    // `pivot_table` drops all-empty rows and columns and sorts both axes.
    let columns: BTreeSet<&String> = cells.keys().map(|(_, audit)| audit).collect();
    let rows: BTreeSet<&String> = cells.keys().map(|(step, _)| step).collect();
    let columns: Vec<String> = columns.into_iter().cloned().collect();
    let rows = rows
        .into_iter()
        .map(|step| {
            let values = columns
                .iter()
                .map(|audit| {
                    cells
                        .get(&(step.clone(), audit.clone()))
                        .map(|(sum, count)| sum / f64::from(*count))
                })
                .collect();
            (step.clone(), values)
        })
        .collect();
    Some(Table { columns, rows })
}

/// The worksheet title for one JSON file name: the SDK's
/// `_create_worksheet_name`.
pub(in crate::toolkits) fn worksheet_name(json_file_name: &str) -> String {
    let mut name = json_file_name.replace(".json", "").replace(':', "_");
    for forbidden in ['/', '\\', '[', ']', '*', '?'] {
        name = name.replace(forbidden, "_");
    }
    let parts: Vec<&str> = name.split('_').collect();
    if parts.len() >= 4 {
        name = parts[..4].join("_");
    }
    if name.chars().count() > 31 {
        name = name.chars().take(31).collect();
    }
    name
}

/// openpyxl's `avoid_duplicate_name`: a title that is taken (any case) gets
/// the highest number already used with it plus one, kept within 31 chars.
pub(in crate::toolkits) fn unique_title(existing: &[String], wanted: &str) -> String {
    let base = if wanted.is_empty() { "Sheet" } else { wanted };
    let lower = base.to_lowercase();
    if !existing.iter().any(|name| name.to_lowercase() == lower) {
        return base.to_owned();
    }
    let highest = existing
        .iter()
        .filter_map(|name| {
            let name = name.to_lowercase();
            name.strip_prefix(&lower)
                .filter(|rest| rest.chars().all(|c| c.is_ascii_digit()))
                .map(|rest| rest.parse::<u64>().unwrap_or(0))
        })
        .max()
        .unwrap_or(0);
    let suffix = (highest + 1).to_string();
    let keep = 31_usize.saturating_sub(suffix.len());
    format!("{}{suffix}", base.chars().take(keep).collect::<String>())
}
