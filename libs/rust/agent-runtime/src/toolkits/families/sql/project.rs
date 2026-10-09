use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use chrono::{DateTime, FixedOffset, NaiveDate, NaiveDateTime, NaiveTime, SecondsFormat, Utc};
use serde_json::{Map, Number, Value, json};
use sqlx::mysql::{MySqlRow, types::MySqlTime};
use sqlx::postgres::{
    PgRow,
    types::{Oid, PgInterval, PgMoney, PgTimeTz},
};
use sqlx::types::{BigDecimal, Uuid};
use sqlx::{Column, Row, TypeInfo, ValueRef};

pub(crate) const MAX_COLUMNS: usize = 64;
pub(crate) const MAX_COLUMN_NAME_BYTES: usize = 256;
pub(crate) const MAX_CELL_BYTES: usize = 64 * 1_024;
pub(crate) const MAX_BINARY_BYTES: usize = 48 * 1_024 - 64;
pub(crate) const MAX_JSON_NODES: usize = 16_384;
pub(crate) const MAX_JSON_DEPTH: usize = 32;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ProjectErrorCode {
    InvalidResponse,
    ResourceExhausted,
    UnsupportedType,
}

pub(crate) struct ProjectError {
    code: ProjectErrorCode,
}

impl ProjectError {
    #[must_use]
    pub(crate) const fn code(&self) -> ProjectErrorCode {
        self.code
    }
}

pub(crate) fn project_pg_row(row: &PgRow) -> Result<Map<String, Value>, ProjectError> {
    validate_columns(row.columns())?;
    let mut output = Map::with_capacity(row.len());
    for (index, column) in row.columns().iter().enumerate() {
        let value = if row
            .try_get_raw(index)
            .map_err(|_| invalid_response())?
            .is_null()
        {
            Value::Null
        } else {
            project_pg_cell(row, index, column.type_info().name())?
        };
        output.insert(column.name().to_owned(), value);
    }
    Ok(output)
}

pub(crate) fn project_mysql_row(row: &MySqlRow) -> Result<Map<String, Value>, ProjectError> {
    validate_columns(row.columns())?;
    let mut output = Map::with_capacity(row.len());
    for (index, column) in row.columns().iter().enumerate() {
        let value = if row
            .try_get_raw(index)
            .map_err(|_| invalid_response())?
            .is_null()
        {
            Value::Null
        } else {
            project_mysql_cell(row, index, column.type_info().name())?
        };
        output.insert(column.name().to_owned(), value);
    }
    Ok(output)
}

pub(crate) fn validate_pg_columns<C: Column>(columns: &[C]) -> Result<(), ProjectError> {
    validate_columns(columns)?;
    if columns
        .iter()
        .any(|column| !is_supported_pg_type(column.type_info().name()))
    {
        return Err(unsupported_type());
    }
    Ok(())
}

pub(crate) fn validate_mysql_columns<C: Column>(columns: &[C]) -> Result<(), ProjectError> {
    validate_columns(columns)?;
    if columns
        .iter()
        .any(|column| !is_supported_mysql_type(column.type_info().name()))
    {
        return Err(unsupported_type());
    }
    Ok(())
}

fn validate_columns<C: Column>(columns: &[C]) -> Result<(), ProjectError> {
    if columns.len() > MAX_COLUMNS {
        return Err(resource_exhausted());
    }
    validate_column_names(columns.iter().map(Column::name))
}

fn validate_column_names<'a>(names: impl IntoIterator<Item = &'a str>) -> Result<(), ProjectError> {
    for name in names {
        if name.is_empty()
            || name.len() > MAX_COLUMN_NAME_BYTES
            || name.chars().any(char::is_control)
        {
            return Err(invalid_response());
        }
    }
    Ok(())
}

fn is_supported_pg_type(name: &str) -> bool {
    matches!(
        name,
        "BOOL"
            | "CHAR"
            | "INT2"
            | "INT4"
            | "INT8"
            | "OID"
            | "FLOAT4"
            | "FLOAT8"
            | "TEXT"
            | "VARCHAR"
            | "BPCHAR"
            | "NAME"
            | "CITEXT"
            | "BYTEA"
            | "NUMERIC"
            | "JSON"
            | "JSONB"
            | "UUID"
            | "DATE"
            | "TIME"
            | "TIMESTAMP"
            | "TIMESTAMPTZ"
            | "TIMETZ"
            | "INTERVAL"
            | "MONEY"
            | "BOOL[]"
            | "CHAR[]"
            | "INT2[]"
            | "INT4[]"
            | "INT8[]"
            | "OID[]"
            | "FLOAT4[]"
            | "FLOAT8[]"
            | "TEXT[]"
            | "VARCHAR[]"
            | "BPCHAR[]"
            | "NAME[]"
            | "CITEXT[]"
            | "BYTEA[]"
            | "NUMERIC[]"
            | "JSON[]"
            | "JSONB[]"
            | "UUID[]"
            | "DATE[]"
            | "TIME[]"
            | "TIMESTAMP[]"
            | "TIMESTAMPTZ[]"
            | "TIMETZ[]"
            | "INTERVAL[]"
            | "MONEY[]"
            | "VOID"
    )
}

fn is_supported_mysql_type(name: &str) -> bool {
    matches!(
        name,
        "BOOLEAN"
            | "TINYINT"
            | "SMALLINT"
            | "INT"
            | "MEDIUMINT"
            | "BIGINT"
            | "TINYINT UNSIGNED"
            | "SMALLINT UNSIGNED"
            | "INT UNSIGNED"
            | "MEDIUMINT UNSIGNED"
            | "BIGINT UNSIGNED"
            | "BIT"
            | "YEAR"
            | "FLOAT"
            | "DOUBLE"
            | "CHAR"
            | "VARCHAR"
            | "TINYTEXT"
            | "TEXT"
            | "MEDIUMTEXT"
            | "LONGTEXT"
            | "ENUM"
            | "SET"
            | "BINARY"
            | "VARBINARY"
            | "TINYBLOB"
            | "BLOB"
            | "MEDIUMBLOB"
            | "LONGBLOB"
            | "DECIMAL"
            | "JSON"
            | "DATE"
            | "TIME"
            | "DATETIME"
            | "TIMESTAMP"
            | "NULL"
    )
}

#[allow(clippy::too_many_lines)]
fn project_pg_cell(row: &PgRow, index: usize, name: &str) -> Result<Value, ProjectError> {
    macro_rules! scalar {
        ($ty:ty, $map:expr) => {{
            let value = row
                .try_get::<$ty, _>(index)
                .map_err(|_| invalid_response())?;
            $map(value)?
        }};
    }
    macro_rules! array {
        ($ty:ty, $map:expr) => {{
            let values = row
                .try_get::<Vec<Option<$ty>>, _>(index)
                .map_err(|_| invalid_response())?;
            if values.len() > MAX_JSON_NODES {
                return Err(resource_exhausted());
            }
            let mut output = Vec::with_capacity(values.len());
            let mut output_bytes = 2usize;
            for value in values {
                let value = value.map_or(Ok(Value::Null), &$map)?;
                let value_bytes = serde_json::to_vec(&value)
                    .map_err(|_| invalid_response())?
                    .len();
                output_bytes = output_bytes
                    .checked_add(value_bytes + usize::from(!output.is_empty()))
                    .ok_or_else(resource_exhausted)?;
                if output_bytes > MAX_CELL_BYTES {
                    return Err(resource_exhausted());
                }
                output.push(value);
            }
            Value::Array(output)
        }};
    }

    Ok(match name {
        "BOOL" => scalar!(bool, |value| Ok(Value::Bool(value))),
        "CHAR" => scalar!(i8, |value| Ok(Value::Number(Number::from(value)))),
        "INT2" => scalar!(i16, |value| Ok(Value::Number(Number::from(value)))),
        "INT4" => scalar!(i32, |value| Ok(Value::Number(Number::from(value)))),
        "INT8" => scalar!(i64, |value| Ok(Value::Number(Number::from(value)))),
        "OID" => scalar!(Oid, |value: Oid| Ok(Value::Number(Number::from(value.0)))),
        "FLOAT4" => scalar!(f32, |value| Ok(float_value(f64::from(value)))),
        "FLOAT8" => scalar!(f64, |value| Ok(float_value(value))),
        "TEXT" | "VARCHAR" | "BPCHAR" | "NAME" | "CITEXT" => {
            scalar!(String, bounded_string)
        }
        "BYTEA" => scalar!(Vec<u8>, binary_value),
        "NUMERIC" => scalar!(BigDecimal, |value: BigDecimal| decimal_value(&value)),
        "JSON" | "JSONB" => scalar!(Value, bounded_json),
        "UUID" => scalar!(Uuid, |value: Uuid| Ok(Value::String(
            value.hyphenated().to_string()
        ))),
        "DATE" => scalar!(NaiveDate, |value: NaiveDate| Ok(Value::String(
            value.format("%Y-%m-%d").to_string()
        ))),
        "TIME" => scalar!(NaiveTime, |value: NaiveTime| Ok(Value::String(
            format_naive_time(value)
        ))),
        "TIMESTAMP" => scalar!(NaiveDateTime, |value: NaiveDateTime| Ok(Value::String(
            format_naive_datetime(value)
        ))),
        "TIMESTAMPTZ" => scalar!(DateTime<Utc>, |value: DateTime<Utc>| Ok(Value::String(
            value.to_rfc3339_opts(SecondsFormat::AutoSi, true)
        ))),
        "TIMETZ" => scalar!(PgTimeTz<NaiveTime, FixedOffset>, |value: PgTimeTz<
            NaiveTime,
            FixedOffset,
        >| {
            bounded_string(format!("{}{}", format_naive_time(value.time), value.offset))
        }),
        "INTERVAL" => scalar!(PgInterval, |value: PgInterval| Ok(json!({
            "months": value.months,
            "days": value.days,
            "microseconds": value.microseconds
        }))),
        "MONEY" => scalar!(PgMoney, |value: PgMoney| Ok(json!({
            "raw_minor_units": value.0
        }))),
        "BOOL[]" => array!(bool, |value| Ok(Value::Bool(value))),
        "CHAR[]" => array!(i8, |value| Ok(Value::Number(Number::from(value)))),
        "INT2[]" => array!(i16, |value| Ok(Value::Number(Number::from(value)))),
        "INT4[]" => array!(i32, |value| Ok(Value::Number(Number::from(value)))),
        "INT8[]" => array!(i64, |value| Ok(Value::Number(Number::from(value)))),
        "OID[]" => array!(Oid, |value: Oid| Ok(Value::Number(Number::from(value.0)))),
        "FLOAT4[]" => array!(f32, |value| Ok(float_value(f64::from(value)))),
        "FLOAT8[]" => array!(f64, |value| Ok(float_value(value))),
        "TEXT[]" | "VARCHAR[]" | "BPCHAR[]" | "NAME[]" | "CITEXT[]" => {
            array!(String, bounded_string)
        }
        "BYTEA[]" => array!(Vec<u8>, binary_value),
        "NUMERIC[]" => array!(BigDecimal, |value: BigDecimal| decimal_value(&value)),
        "JSON[]" | "JSONB[]" => array!(Value, bounded_json),
        "UUID[]" => array!(Uuid, |value: Uuid| Ok(Value::String(
            value.hyphenated().to_string()
        ))),
        "DATE[]" => array!(NaiveDate, |value: NaiveDate| Ok(Value::String(
            value.format("%Y-%m-%d").to_string()
        ))),
        "TIME[]" => array!(NaiveTime, |value| Ok(Value::String(format_naive_time(
            value
        )))),
        "TIMESTAMP[]" => array!(NaiveDateTime, |value| Ok(Value::String(
            format_naive_datetime(value)
        ))),
        "TIMESTAMPTZ[]" => array!(DateTime<Utc>, |value: DateTime<Utc>| Ok(Value::String(
            value.to_rfc3339_opts(SecondsFormat::AutoSi, true)
        ))),
        "TIMETZ[]" => array!(PgTimeTz<NaiveTime, FixedOffset>, |value: PgTimeTz<
            NaiveTime,
            FixedOffset,
        >| {
            bounded_string(format!("{}{}", format_naive_time(value.time), value.offset))
        }),
        "INTERVAL[]" => array!(PgInterval, |value: PgInterval| Ok(json!({
            "months": value.months,
            "days": value.days,
            "microseconds": value.microseconds
        }))),
        "MONEY[]" => array!(PgMoney, |value: PgMoney| Ok(json!({
            "raw_minor_units": value.0
        }))),
        "VOID" => Value::Null,
        _ => return Err(unsupported_type()),
    })
}

fn project_mysql_cell(row: &MySqlRow, index: usize, name: &str) -> Result<Value, ProjectError> {
    macro_rules! scalar {
        ($ty:ty, $map:expr) => {{
            let value = row
                .try_get::<$ty, _>(index)
                .map_err(|_| invalid_response())?;
            $map(value)?
        }};
    }
    Ok(match name {
        "BOOLEAN" => scalar!(bool, |value| Ok(Value::Bool(value))),
        "TINYINT" => scalar!(i8, |value| Ok(Value::Number(Number::from(value)))),
        "SMALLINT" => scalar!(i16, |value| Ok(Value::Number(Number::from(value)))),
        "INT" | "MEDIUMINT" => scalar!(i32, |value| Ok(Value::Number(Number::from(value)))),
        "BIGINT" => scalar!(i64, |value| Ok(Value::Number(Number::from(value)))),
        "TINYINT UNSIGNED" => scalar!(u8, |value| Ok(Value::Number(Number::from(value)))),
        "SMALLINT UNSIGNED" => scalar!(u16, |value| Ok(Value::Number(Number::from(value)))),
        "INT UNSIGNED" | "MEDIUMINT UNSIGNED" => {
            scalar!(u32, |value| Ok(Value::Number(Number::from(value))))
        }
        "BIGINT UNSIGNED" | "BIT" => {
            scalar!(u64, |value| Ok(Value::Number(Number::from(value))))
        }
        "YEAR" => scalar!(u16, |value| Ok(Value::Number(Number::from(value)))),
        "FLOAT" => scalar!(f32, |value| Ok(float_value(f64::from(value)))),
        "DOUBLE" => scalar!(f64, |value| Ok(float_value(value))),
        "CHAR" | "VARCHAR" | "TINYTEXT" | "TEXT" | "MEDIUMTEXT" | "LONGTEXT" | "ENUM" | "SET" => {
            scalar!(String, bounded_string)
        }
        "BINARY" | "VARBINARY" | "TINYBLOB" | "BLOB" | "MEDIUMBLOB" | "LONGBLOB" => {
            scalar!(Vec<u8>, binary_value)
        }
        "DECIMAL" => scalar!(BigDecimal, |value: BigDecimal| decimal_value(&value)),
        "JSON" => scalar!(Value, bounded_json),
        "DATE" => scalar!(NaiveDate, |value: NaiveDate| Ok(Value::String(
            value.format("%Y-%m-%d").to_string()
        ))),
        "TIME" => scalar!(MySqlTime, |value: MySqlTime| bounded_string(
            value.to_string()
        )),
        "DATETIME" => scalar!(NaiveDateTime, |value: NaiveDateTime| Ok(Value::String(
            format_naive_datetime(value)
        ))),
        "TIMESTAMP" => scalar!(DateTime<Utc>, |value: DateTime<Utc>| Ok(Value::String(
            value.to_rfc3339_opts(SecondsFormat::AutoSi, true)
        ))),
        "NULL" => Value::Null,
        _ => return Err(unsupported_type()),
    })
}

fn bounded_string(value: impl Into<String>) -> Result<Value, ProjectError> {
    let value = value.into();
    if value.len() > MAX_CELL_BYTES {
        return Err(resource_exhausted());
    }
    Ok(Value::String(value))
}

fn binary_value(value: Vec<u8>) -> Result<Value, ProjectError> {
    if value.len() > MAX_BINARY_BYTES {
        return Err(resource_exhausted());
    }
    bounded_json(json!({
        "encoding": "base64",
        "data": STANDARD.encode(value)
    }))
}

fn decimal_value(value: &BigDecimal) -> Result<Value, ProjectError> {
    bounded_string(value.to_string())
}

fn bounded_json(value: Value) -> Result<Value, ProjectError> {
    let mut nodes = 0usize;
    let mut stack = vec![(&value, 1usize)];
    while let Some((node, depth)) = stack.pop() {
        nodes = nodes.checked_add(1).ok_or_else(resource_exhausted)?;
        if nodes > MAX_JSON_NODES || depth > MAX_JSON_DEPTH {
            return Err(resource_exhausted());
        }
        match node {
            Value::String(value) if value.len() > MAX_CELL_BYTES => {
                return Err(resource_exhausted());
            }
            Value::Array(values) => {
                stack.extend(values.iter().map(|value| (value, depth + 1)));
            }
            Value::Object(values) => {
                if values.keys().any(|key| key.len() > MAX_COLUMN_NAME_BYTES) {
                    return Err(resource_exhausted());
                }
                stack.extend(values.values().map(|value| (value, depth + 1)));
            }
            _ => {}
        }
    }
    if serde_json::to_vec(&value)
        .map_err(|_| invalid_response())?
        .len()
        > MAX_CELL_BYTES
    {
        return Err(resource_exhausted());
    }
    Ok(value)
}

fn float_value(value: f64) -> Value {
    Number::from_f64(value).map_or_else(
        || {
            Value::String(
                if value.is_nan() {
                    "NaN"
                } else if value.is_sign_positive() {
                    "Infinity"
                } else {
                    "-Infinity"
                }
                .to_owned(),
            )
        },
        Value::Number,
    )
}

fn format_naive_time(value: NaiveTime) -> String {
    value.format("%H:%M:%S%.f").to_string()
}

fn format_naive_datetime(value: NaiveDateTime) -> String {
    value.format("%Y-%m-%dT%H:%M:%S%.f").to_string()
}

const fn invalid_response() -> ProjectError {
    ProjectError {
        code: ProjectErrorCode::InvalidResponse,
    }
}

const fn resource_exhausted() -> ProjectError {
    ProjectError {
        code: ProjectErrorCode::ResourceExhausted,
    }
}

const fn unsupported_type() -> ProjectError {
    ProjectError {
        code: ProjectErrorCode::UnsupportedType,
    }
}

#[cfg(test)]
pub(in crate::toolkits) fn test_bounded_json(value: Value) -> Result<Value, ProjectError> {
    bounded_json(value)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_binary_value(value: Vec<u8>) -> Result<Value, ProjectError> {
    binary_value(value)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_float_value(value: f64) -> Value {
    float_value(value)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_supported_types() -> (Vec<&'static str>, Vec<&'static str>) {
    let postgres = [
        "BOOL",
        "BYTEA",
        "NUMERIC",
        "JSONB",
        "UUID",
        "DATE",
        "TIME",
        "TIMESTAMP",
        "TIMESTAMPTZ",
        "TIMETZ",
        "INTERVAL",
        "MONEY",
        "NUMERIC[]",
        "JSONB[]",
        "UUID[]",
    ];
    let mysql = [
        "BOOLEAN",
        "BIGINT UNSIGNED",
        "BLOB",
        "DECIMAL",
        "JSON",
        "DATE",
        "TIME",
        "DATETIME",
        "TIMESTAMP",
    ];
    debug_assert!(postgres.iter().all(|name| is_supported_pg_type(name)));
    debug_assert!(mysql.iter().all(|name| is_supported_mysql_type(name)));
    (postgres.to_vec(), mysql.to_vec())
}

#[cfg(test)]
pub(in crate::toolkits) fn test_validate_column_names(names: &[&str]) -> Result<(), ProjectError> {
    if names.len() > MAX_COLUMNS {
        return Err(resource_exhausted());
    }
    validate_column_names(names.iter().copied())
}
