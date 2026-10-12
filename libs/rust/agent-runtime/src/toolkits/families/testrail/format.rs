//! The SDK's `_project_records` and `_to_markup`: allowlist projection with
//! ISO timestamps, then `Extracted data:\n{python repr}`, `DataFrame.to_csv`
//! or `DataFrame.to_markdown` (tabulate's `pipe` table).

use chrono::{DateTime, SecondsFormat};
use serde_json::{Number, Value};

use crate::toolkits::families::python_repr::{repr, repr_number, repr_pairs};

/// One record in the SDK's dict insertion order.
pub(super) type Record = Vec<(String, Value)>;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum Markup {
    Json,
    Csv,
    Markdown,
}

impl Markup {
    pub(super) fn parse(value: &str) -> Option<Self> {
        match value {
            "json" => Some(Self::Json),
            "csv" => Some(Self::Csv),
            "markdown" => Some(Self::Markdown),
            _ => None,
        }
    }
}

/// The text `_to_markup` returns for an unknown format (as a returned
/// `ToolException`, which the model reads as the tool output).
pub(super) fn invalid_format(value: &str) -> String {
    format!("Invalid format `{value}`. Supported formats: 'json', 'csv', 'markdown'.")
}

/// A provider object as a record, in the order the value holds its members.
pub(super) fn record_of(value: &Value) -> Option<Record> {
    value.as_object().map(|object| {
        object
            .iter()
            .map(|(key, value)| (key.clone(), value.clone()))
            .collect()
    })
}

/// `_project_records`: the allowlisted fields present, every `custom_*`
/// field, timestamps as ISO-8601; a non-object becomes
/// `{fallback_key: str(record)}`.
pub(super) fn project_records(
    records: &[Value],
    allowlist: &[&str],
    fallback_key: &str,
    timestamps: &[&str],
) -> Vec<Record> {
    records
        .iter()
        .map(|record| {
            let Some(object) = record.as_object() else {
                let text = match record {
                    Value::String(text) => text.clone(),
                    other => repr(other),
                };
                return vec![(fallback_key.to_owned(), Value::String(text))];
            };
            let mut projected: Record = allowlist
                .iter()
                .filter_map(|field| {
                    object
                        .get(*field)
                        .map(|value| ((*field).to_owned(), value.clone()))
                })
                .collect();
            for (key, value) in object {
                if key.starts_with("custom_") {
                    if let Some(existing) = projected.iter_mut().find(|(name, _)| name == key) {
                        existing.1 = value.clone();
                    } else {
                        projected.push((key.clone(), value.clone()));
                    }
                }
            }
            for (key, value) in &mut projected {
                if timestamps.contains(&key.as_str()) {
                    *value = epoch_to_iso(value);
                }
            }
            projected
        })
        .collect()
}

/// `_epoch_to_iso`: a truthy, non-bool number becomes
/// `datetime.fromtimestamp(value, tz=utc).isoformat()`; anything else, or an
/// out-of-range value, passes through.
pub(super) fn epoch_to_iso(value: &Value) -> Value {
    let Value::Number(number) = value else {
        return value.clone();
    };
    let Some(seconds) = number.as_f64().filter(|seconds| *seconds != 0.0) else {
        return value.clone();
    };
    if !seconds.is_finite() || seconds.abs() > 253_402_300_799.0 {
        return value.clone();
    }
    let whole = seconds.floor();
    // Microsecond precision, as Python's datetime holds it.
    #[allow(clippy::cast_possible_truncation, clippy::cast_sign_loss)]
    let micros = ((seconds - whole) * 1_000_000.0).round() as u32;
    #[allow(clippy::cast_possible_truncation)]
    let Some(instant) = DateTime::from_timestamp(whole as i64, micros.min(999_999) * 1_000) else {
        return value.clone();
    };
    let format = if micros == 0 {
        SecondsFormat::Secs
    } else {
        SecondsFormat::Micros
    };
    Value::String(instant.to_rfc3339_opts(format, false))
}

/// `_to_markup` for a validated format.
pub(super) fn to_markup(records: &[Record], format: Markup) -> String {
    match format {
        Markup::Json => {
            let mut output = String::from("Extracted data:\n[");
            for (index, record) in records.iter().enumerate() {
                if index > 0 {
                    output.push_str(", ");
                }
                output.push_str(&repr_pairs(
                    record.iter().map(|(key, value)| (key.as_str(), value)),
                ));
            }
            output.push(']');
            output
        }
        Markup::Csv => to_csv(records),
        Markup::Markdown => to_markdown(records),
    }
}

/// The `DataFrame` columns: every key, in order of first appearance.
fn columns(records: &[Record]) -> Vec<&str> {
    let mut columns: Vec<&str> = Vec::new();
    for record in records {
        for (key, _) in record {
            if !columns.contains(&key.as_str()) {
                columns.push(key);
            }
        }
    }
    columns
}

fn cell<'a>(record: &'a Record, column: &str) -> Option<&'a Value> {
    record
        .iter()
        .find(|(key, _)| key == column)
        .map(|(_, value)| value)
        .filter(|value| !value.is_null())
}

/// pandas stores an integer column with a missing value (or any float) as
/// float64, so its integers print as `1.0`.
fn is_float_column(values: &[Option<&Value>]) -> bool {
    let present = values.iter().flatten().collect::<Vec<_>>();
    !present.is_empty()
        && present.iter().all(|value| value.is_number())
        && (present.len() < values.len() || present.iter().any(|value| value.is_f64()))
}

fn as_float(number: &Number) -> String {
    number.as_f64().map_or_else(
        || number.to_string(),
        |float| {
            Number::from_f64(float).map_or_else(|| "nan".to_owned(), |number| repr_number(&number))
        },
    )
}

fn plain_text(value: &Value) -> String {
    match value {
        Value::String(text) => text.clone(),
        Value::Bool(true) => "True".to_owned(),
        Value::Bool(false) => "False".to_owned(),
        Value::Number(number) => repr_number(number),
        other => repr(other),
    }
}

fn to_csv(records: &[Record]) -> String {
    let columns = columns(records);
    let mut output = String::new();
    push_csv_row(
        &mut output,
        columns.iter().map(|column| (*column).to_owned()),
    );
    let float_columns = columns
        .iter()
        .map(|column| {
            is_float_column(
                &records
                    .iter()
                    .map(|record| cell(record, column))
                    .collect::<Vec<_>>(),
            )
        })
        .collect::<Vec<_>>();
    for record in records {
        push_csv_row(
            &mut output,
            columns
                .iter()
                .zip(&float_columns)
                .map(|(column, float)| match cell(record, column) {
                    None => String::new(),
                    Some(Value::Number(number)) if *float => as_float(number),
                    Some(value) => plain_text(value),
                }),
        );
    }
    output
}

fn push_csv_row(output: &mut String, cells: impl Iterator<Item = String>) {
    for (index, value) in cells.enumerate() {
        if index > 0 {
            output.push(',');
        }
        if value.contains([',', '"', '\r', '\n']) {
            output.push('"');
            output.push_str(&value.replace('"', "\"\""));
            output.push('"');
        } else {
            output.push_str(&value);
        }
    }
    output.push('\n');
}

/// Python's `format(x, "g")`: six significant digits, trailing zeros removed,
/// scientific below 1e-4 or from 1e6.
fn format_g(value: f64) -> String {
    if value == 0.0 {
        return "0".to_owned();
    }
    if !value.is_finite() {
        return if value.is_nan() {
            "nan"
        } else if value > 0.0 {
            "inf"
        } else {
            "-inf"
        }
        .to_owned();
    }
    let scientific = format!("{value:.5e}");
    let (mantissa, exponent) = scientific.split_once('e').unwrap_or((&scientific, "0"));
    let exponent = exponent.parse::<i32>().unwrap_or(0);
    if (-4..6).contains(&exponent) {
        let decimals = usize::try_from(5 - exponent).unwrap_or(0);
        let fixed = format!("{value:.decimals$}");
        if fixed.contains('.') {
            fixed.trim_end_matches('0').trim_end_matches('.').to_owned()
        } else {
            fixed
        }
    } else {
        let mantissa = if mantissa.contains('.') {
            mantissa.trim_end_matches('0').trim_end_matches('.')
        } else {
            mantissa
        };
        let sign = if exponent < 0 { '-' } else { '+' };
        format!("{mantissa}e{sign}{:02}", exponent.abs())
    }
}

fn is_numeric_text(value: &Value) -> bool {
    match value {
        Value::Number(_) => true,
        Value::String(text) => {
            let text = text.trim();
            !text.is_empty() && text.parse::<f64>().is_ok()
        }
        _ => false,
    }
}

/// tabulate `pipe`, as `DataFrame.to_markdown(index=False)` calls it:
/// numeric columns right-aligned, others left-aligned, each column at least
/// two wider than its header, missing values blank.
fn to_markdown(records: &[Record]) -> String {
    let columns = columns(records);
    if columns.is_empty() {
        return String::new();
    }
    let mut table: Vec<Vec<String>> = Vec::with_capacity(records.len());
    let mut numeric = Vec::with_capacity(columns.len());
    for column in &columns {
        let values = records
            .iter()
            .map(|record| cell(record, column))
            .collect::<Vec<_>>();
        let present = values.iter().flatten().collect::<Vec<_>>();
        numeric.push(!present.is_empty() && present.iter().all(|value| is_numeric_text(value)));
        let float = is_float_column(&values);
        for (row, value) in values.iter().enumerate() {
            if table.len() <= row {
                table.push(Vec::with_capacity(columns.len()));
            }
            table[row].push(match value {
                None if float => "nan".to_owned(),
                None => String::new(),
                Some(Value::Number(number)) if float || number.is_f64() => {
                    format_g(number.as_f64().unwrap_or(f64::NAN))
                }
                Some(value) => plain_text(value),
            });
        }
    }
    let widths = columns
        .iter()
        .enumerate()
        .map(|(index, column)| {
            table
                .iter()
                .map(|row| row[index].chars().count())
                .max()
                .unwrap_or(0)
                .max(column.chars().count() + 2)
        })
        .collect::<Vec<_>>();
    let pad = |text: &str, width: usize, right: bool| {
        let fill = " ".repeat(width.saturating_sub(text.chars().count()));
        if right {
            format!("{fill}{text}")
        } else {
            format!("{text}{fill}")
        }
    };
    let mut output = String::new();
    let row = |cells: Vec<String>| format!("| {} |", cells.join(" | "));
    output.push_str(&row(columns
        .iter()
        .enumerate()
        .map(|(index, column)| pad(column, widths[index], numeric[index]))
        .collect()));
    output.push('\n');
    output.push('|');
    for (index, width) in widths.iter().enumerate() {
        if numeric[index] {
            output.push_str(&"-".repeat(width + 1));
            output.push(':');
        } else {
            output.push(':');
            output.push_str(&"-".repeat(width + 1));
        }
        output.push('|');
    }
    for cells in table {
        output.push('\n');
        output.push_str(&row(cells
            .iter()
            .enumerate()
            .map(|(index, text)| pad(text, widths[index], numeric[index]))
            .collect()));
    }
    output
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::{Markup, epoch_to_iso, format_g, project_records, to_markup};

    #[test]
    fn projection_keeps_allowlist_order_custom_fields_and_iso_time() {
        let records = project_records(
            &[
                json!({"name":"Run","id":7,"created_on":1_700_000_000,"custom_x":"y","noise":1}),
                json!("legacy"),
            ],
            &["id", "name", "created_on"],
            "run",
            &["created_on"],
        );
        assert_eq!(
            to_markup(&records, Markup::Json),
            "Extracted data:\n[{'id': 7, 'name': 'Run', 'created_on': '2023-11-14T22:13:20+00:00', 'custom_x': 'y'}, {'run': 'legacy'}]"
        );
        assert_eq!(epoch_to_iso(&json!(0)), json!(0));
        assert_eq!(epoch_to_iso(&json!(true)), json!(true));
        assert_eq!(
            epoch_to_iso(&json!(1.5)),
            json!("1970-01-01T00:00:01.500000+00:00")
        );
    }

    #[test]
    fn csv_and_markdown_follow_pandas() {
        let records = project_records(
            &[
                json!({"id":1,"title":"Login, fast","priority":2}),
                json!({"id":22,"title":"Logout"}),
            ],
            &["id", "title", "priority"],
            "case",
            &[],
        );
        assert_eq!(
            to_markup(&records, Markup::Csv),
            "id,title,priority\n1,\"Login, fast\",2.0\n22,Logout,\n"
        );
        assert_eq!(
            to_markup(&records, Markup::Markdown),
            "|   id | title       |   priority |\n|-----:|:------------|-----------:|\n|    1 | Login, fast |          2 |\n|   22 | Logout      |        nan |"
        );
        assert_eq!(format_g(1_234_567.0), "1.23457e+06");
        assert_eq!(format_g(0.000_12), "0.00012");
        assert_eq!(format_g(2.50), "2.5");
    }
}
