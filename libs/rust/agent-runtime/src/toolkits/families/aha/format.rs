use std::collections::HashSet;
use std::fmt;

use serde_json::{Map, Value};

pub(super) const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_FIELDS: usize = 128;
const MAX_FIELD_NAME_BYTES: usize = 256;
const MAX_TABLE_CELLS: usize = 16_384;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(in crate::toolkits) enum OutputFormat {
    Json,
    Csv,
    Markdown,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum FormatErrorCode {
    InvalidInput,
    ResourceExhausted,
}

pub(super) struct FormatError {
    code: FormatErrorCode,
}

impl FormatError {
    #[must_use]
    pub(super) const fn code(&self) -> FormatErrorCode {
        self.code
    }
}

impl fmt::Debug for FormatError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("FormatError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for FormatError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            FormatErrorCode::InvalidInput => "the Aha output request is invalid",
            FormatErrorCode::ResourceExhausted => "the Aha output exceeds its approved limit",
        })
    }
}

impl std::error::Error for FormatError {}

impl OutputFormat {
    pub(super) fn parse(value: Option<&str>) -> Result<Self, FormatError> {
        match value.unwrap_or("json").trim().to_ascii_lowercase().as_str() {
            "json" => Ok(Self::Json),
            "csv" => Ok(Self::Csv),
            "markdown" => Ok(Self::Markdown),
            _ => Err(invalid_input()),
        }
    }
}

pub(super) fn parse_fields(value: Option<&Value>) -> Result<Vec<Box<str>>, FormatError> {
    let Some(value) = value else {
        return Ok(Vec::new());
    };
    if value.is_null() {
        return Ok(Vec::new());
    }
    let values = value.as_array().ok_or_else(invalid_input)?;
    if values.len() > MAX_FIELDS {
        return Err(resource_exhausted());
    }
    let mut fields = Vec::with_capacity(values.len());
    for value in values {
        let name = value.as_str().ok_or_else(invalid_input)?;
        if name.is_empty()
            || name.len() > MAX_FIELD_NAME_BYTES
            || name.chars().any(char::is_control)
        {
            return Err(if name.len() > MAX_FIELD_NAME_BYTES {
                resource_exhausted()
            } else {
                invalid_input()
            });
        }
        if !fields
            .iter()
            .map(AsRef::as_ref)
            .any(|existing: &str| existing == name)
        {
            fields.push(name.into());
        }
    }
    Ok(fields)
}

pub(super) fn project_record(record: &Value, fields: &[Box<str>]) -> Value {
    if fields.is_empty() {
        return record.clone();
    }
    let Some(object) = record.as_object() else {
        return record.clone();
    };
    let projected = fields
        .iter()
        .filter_map(|field| {
            object
                .get(field.as_ref())
                .map(|value| (field.to_string(), value.clone()))
        })
        .collect::<Map<_, _>>();
    Value::Object(projected)
}

pub(super) fn project_records(records: &[Value], fields: &[Box<str>]) -> Vec<Value> {
    records
        .iter()
        .map(|record| project_record(record, fields))
        .collect()
}

pub(super) fn render(
    data: Value,
    format: OutputFormat,
    empty_message: Option<String>,
) -> Result<Value, FormatError> {
    if data.as_array().is_some_and(Vec::is_empty)
        && let Some(message) = empty_message
    {
        return bounded(Value::String(message));
    }
    match format {
        OutputFormat::Json => bounded(data),
        OutputFormat::Csv | OutputFormat::Markdown => {
            let records = match &data {
                Value::Object(_) => vec![&data],
                Value::Array(values)
                    if values.iter().all(Value::is_object) && !values.is_empty() =>
                {
                    values.iter().collect()
                }
                _ => return bounded(data),
            };
            let rendered = if format == OutputFormat::Csv {
                render_csv(&records)?
            } else {
                render_markdown(&records)?
            };
            bounded(Value::String(rendered))
        }
    }
}

fn columns(records: &[&Value]) -> Result<Vec<Box<str>>, FormatError> {
    let mut columns = Vec::new();
    let mut seen = HashSet::new();
    for object in records.iter().filter_map(|record| record.as_object()) {
        for key in object.keys() {
            if seen.insert(key.as_str()) {
                if columns.len() >= MAX_FIELDS {
                    return Err(resource_exhausted());
                }
                columns.push(key.as_str().into());
            }
        }
    }
    let cells = records
        .len()
        .checked_mul(columns.len())
        .ok_or_else(resource_exhausted)?;
    if cells > MAX_TABLE_CELLS {
        return Err(resource_exhausted());
    }
    Ok(columns)
}

fn render_csv(records: &[&Value]) -> Result<String, FormatError> {
    let columns = columns(records)?;
    let mut output = String::new();
    append_csv_row(&mut output, columns.iter().map(ToString::to_string))?;
    for object in records.iter().filter_map(|record| record.as_object()) {
        append_csv_row(
            &mut output,
            columns
                .iter()
                .map(|column| owned_cell(object.get(column.as_ref()))),
        )?;
    }
    Ok(output)
}

fn append_csv_row(
    output: &mut String,
    cells: impl Iterator<Item = String>,
) -> Result<(), FormatError> {
    for (index, value) in cells.enumerate() {
        if index > 0 {
            push_bounded(output, ",")?;
        }
        if value.contains([',', '"', '\r', '\n']) {
            push_bounded(output, "\"")?;
            for character in value.chars() {
                if character == '"' {
                    push_bounded(output, "\"\"")?;
                } else {
                    let mut buffer = [0; 4];
                    push_bounded(output, character.encode_utf8(&mut buffer))?;
                }
            }
            push_bounded(output, "\"")?;
        } else {
            push_bounded(output, &value)?;
        }
    }
    push_bounded(output, "\n")
}

fn render_markdown(records: &[&Value]) -> Result<String, FormatError> {
    let columns = columns(records)?;
    let mut output = String::new();
    append_markdown_row(&mut output, columns.iter().map(AsRef::as_ref))?;
    append_markdown_row(&mut output, columns.iter().map(|_| "---"))?;
    for object in records.iter().filter_map(|record| record.as_object()) {
        push_bounded(&mut output, "|")?;
        for column in &columns {
            push_bounded(&mut output, " ")?;
            push_bounded(&mut output, &markdown_cell(object.get(column.as_ref())))?;
            push_bounded(&mut output, " |")?;
        }
        push_bounded(&mut output, "\n")?;
    }
    Ok(output)
}

fn append_markdown_row<'a>(
    output: &mut String,
    cells: impl Iterator<Item = &'a str>,
) -> Result<(), FormatError> {
    push_bounded(output, "|")?;
    for value in cells {
        push_bounded(output, " ")?;
        push_bounded(output, value)?;
        push_bounded(output, " |")?;
    }
    push_bounded(output, "\n")
}

fn markdown_cell(value: Option<&Value>) -> String {
    owned_cell(value)
        .replace('\\', "\\\\")
        .replace('|', "\\|")
        .replace(['\r', '\n'], " ")
}

fn cell(value: Option<&Value>) -> &str {
    match value {
        None | Some(Value::Null | Value::Number(_) | Value::Array(_) | Value::Object(_)) => "",
        Some(Value::String(value)) => value,
        Some(Value::Bool(true)) => "true",
        Some(Value::Bool(false)) => "false",
    }
}

fn owned_cell(value: Option<&Value>) -> String {
    match value {
        Some(Value::Number(value)) => value.to_string(),
        Some(Value::Array(value)) => Value::Array(value.clone()).to_string(),
        Some(Value::Object(value)) => Value::Object(value.clone()).to_string(),
        _ => cell(value).to_owned(),
    }
}

fn push_bounded(output: &mut String, value: &str) -> Result<(), FormatError> {
    let next = output
        .len()
        .checked_add(value.len())
        .ok_or_else(resource_exhausted)?;
    if next > MAX_OUTPUT_BYTES {
        return Err(resource_exhausted());
    }
    output.push_str(value);
    Ok(())
}

pub(super) fn bounded(value: Value) -> Result<Value, FormatError> {
    if serde_json::to_vec(&value)
        .map_err(|_| invalid_input())?
        .len()
        > MAX_OUTPUT_BYTES
    {
        return Err(resource_exhausted());
    }
    Ok(value)
}

const fn invalid_input() -> FormatError {
    FormatError {
        code: FormatErrorCode::InvalidInput,
    }
}

const fn resource_exhausted() -> FormatError {
    FormatError {
        code: FormatErrorCode::ResourceExhausted,
    }
}
