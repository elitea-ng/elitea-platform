//! Arrow to JSON for result rows, and the schema text `get_table_schema`
//! returns (pyarrow's `Schema.to_string()`, which is what the SDK returns).

use std::fmt::Write as _;

use delta_kernel::arrow::array::{Array, ArrayRef, AsArray};
use delta_kernel::arrow::datatypes::{
    DataType, Field, Float32Type, Float64Type, Int8Type, Int16Type, Int32Type, Int64Type, Schema,
    TimeUnit, UInt8Type, UInt16Type, UInt32Type, UInt64Type,
};
use delta_kernel::arrow::util::display::{ArrayFormatter, FormatOptions};
use serde_json::{Map, Number, Value};

/// A text cell longer than this is cut (on a character boundary) and marked.
pub(super) const MAX_CELL_CHARS: usize = 2_000;
/// A list cell (an embedding) longer than this is cut.
const MAX_LIST_ELEMENTS: usize = 8_192;
/// The JSON values (scalars, list elements, struct fields) one row may
/// carry in all; a cell past it is cut. A row is therefore a bounded value
/// before any byte count sees it.
pub(super) const MAX_ROW_VALUES: usize = 32_768;
const MAX_DEPTH: usize = 4;
const TRUNCATED: &str = "...[truncated]";

/// The JSON of one cell. Integers and floats are numbers (a non-finite float
/// is `null`), text is text, lists and structs recurse, and everything else
/// (dates, timestamps, decimals, binary) is Arrow's display text.
pub(super) fn cell_json(array: &ArrayRef, row: usize, budget: &mut usize) -> Value {
    value_json(array, row, 0, budget)
}

fn value_json(array: &ArrayRef, row: usize, depth: usize, budget: &mut usize) -> Value {
    if array.is_null(row) || *budget == 0 {
        return Value::Null;
    }
    *budget -= 1;
    match array.data_type() {
        DataType::Boolean => Value::Bool(array.as_boolean().value(row)),
        DataType::Int8 => Value::from(array.as_primitive::<Int8Type>().value(row)),
        DataType::Int16 => Value::from(array.as_primitive::<Int16Type>().value(row)),
        DataType::Int32 => Value::from(array.as_primitive::<Int32Type>().value(row)),
        DataType::Int64 => Value::from(array.as_primitive::<Int64Type>().value(row)),
        DataType::UInt8 => Value::from(array.as_primitive::<UInt8Type>().value(row)),
        DataType::UInt16 => Value::from(array.as_primitive::<UInt16Type>().value(row)),
        DataType::UInt32 => Value::from(array.as_primitive::<UInt32Type>().value(row)),
        DataType::UInt64 => Value::from(array.as_primitive::<UInt64Type>().value(row)),
        DataType::Float32 => f32_json(array.as_primitive::<Float32Type>().value(row)),
        DataType::Float64 => float_json(array.as_primitive::<Float64Type>().value(row)),
        DataType::Utf8 => text_json(array.as_string::<i32>().value(row)),
        DataType::LargeUtf8 => text_json(array.as_string::<i64>().value(row)),
        DataType::Utf8View => text_json(array.as_string_view().value(row)),
        DataType::List(_) => list_json(&array.as_list::<i32>().value(row), depth, budget),
        DataType::LargeList(_) => list_json(&array.as_list::<i64>().value(row), depth, budget),
        DataType::FixedSizeList(..) => {
            list_json(&array.as_fixed_size_list().value(row), depth, budget)
        }
        DataType::Struct(fields) if depth < MAX_DEPTH => {
            let columns = array.as_struct().columns();
            let mut object = Map::with_capacity(fields.len());
            for (field, column) in fields.iter().zip(columns) {
                object.insert(
                    field.name().clone(),
                    value_json(column, row, depth + 1, budget),
                );
            }
            Value::Object(object)
        }
        _ => text_json(&display(array, row)),
    }
}

fn float_json(value: f64) -> Value {
    Number::from_f64(value).map_or(Value::Null, Value::Number)
}

/// A `float` cell as its shortest decimal (`0.9`, not `0.8999999761581421`).
fn f32_json(value: f32) -> Value {
    value
        .to_string()
        .parse::<f64>()
        .map_or(Value::Null, float_json)
}

fn text_json(text: &str) -> Value {
    Value::String(truncate_chars(text))
}

fn list_json(values: &ArrayRef, depth: usize, budget: &mut usize) -> Value {
    if depth >= MAX_DEPTH {
        return Value::Null;
    }
    let length = values.len().min(MAX_LIST_ELEMENTS).min(*budget);
    Value::Array(
        (0..length)
            .map(|index| value_json(values, index, depth + 1, budget))
            .collect(),
    )
}

fn display(array: &ArrayRef, row: usize) -> String {
    ArrayFormatter::try_new(array.as_ref(), &FormatOptions::default())
        .map(|formatter| formatter.value(row).to_string())
        .unwrap_or_default()
}

/// `text` cut to [`MAX_CELL_CHARS`] characters, marked when cut.
pub(super) fn truncate_chars(text: &str) -> String {
    match text.char_indices().nth(MAX_CELL_CHARS) {
        None => text.to_owned(),
        Some((end, _)) => format!("{}{TRUNCATED}", &text[..end]),
    }
}

/// pyarrow's `Schema.to_string()`: one `name: type` line per field, with
/// ` not null` on a non-nullable field.
pub(super) fn schema_text(schema: &Schema) -> String {
    schema
        .fields()
        .iter()
        .map(|field| field_text(field))
        .collect::<Vec<_>>()
        .join("\n")
}

fn field_text(field: &Field) -> String {
    let mut text = format!("{}: {}", field.name(), type_text(field.data_type()));
    if !field.is_nullable() {
        text.push_str(" not null");
    }
    text
}

/// pyarrow's spelling of an Arrow type.
fn type_text(data_type: &DataType) -> String {
    match data_type {
        DataType::Null => "null".to_owned(),
        DataType::Boolean => "bool".to_owned(),
        DataType::Int8 => "int8".to_owned(),
        DataType::Int16 => "int16".to_owned(),
        DataType::Int32 => "int32".to_owned(),
        DataType::Int64 => "int64".to_owned(),
        DataType::UInt8 => "uint8".to_owned(),
        DataType::UInt16 => "uint16".to_owned(),
        DataType::UInt32 => "uint32".to_owned(),
        DataType::UInt64 => "uint64".to_owned(),
        DataType::Float16 => "halffloat".to_owned(),
        DataType::Float32 => "float".to_owned(),
        DataType::Float64 => "double".to_owned(),
        DataType::Utf8 => "string".to_owned(),
        DataType::LargeUtf8 => "large_string".to_owned(),
        DataType::Utf8View => "string_view".to_owned(),
        DataType::Binary => "binary".to_owned(),
        DataType::LargeBinary => "large_binary".to_owned(),
        DataType::BinaryView => "binary_view".to_owned(),
        DataType::FixedSizeBinary(width) => format!("fixed_size_binary[{width}]"),
        DataType::Date32 => "date32[day]".to_owned(),
        DataType::Date64 => "date64[ms]".to_owned(),
        DataType::Timestamp(unit, zone) => {
            let unit = match unit {
                TimeUnit::Second => "s",
                TimeUnit::Millisecond => "ms",
                TimeUnit::Microsecond => "us",
                TimeUnit::Nanosecond => "ns",
            };
            zone.as_ref().map_or_else(
                || format!("timestamp[{unit}]"),
                |zone| format!("timestamp[{unit}, tz={zone}]"),
            )
        }
        DataType::Decimal128(precision, scale) => format!("decimal128({precision}, {scale})"),
        DataType::Decimal256(precision, scale) => format!("decimal256({precision}, {scale})"),
        DataType::List(element) => format!("list<{}>", element_text(element)),
        DataType::LargeList(element) => format!("large_list<{}>", element_text(element)),
        DataType::FixedSizeList(element, size) => {
            format!("fixed_size_list<{}>[{size}]", element_text(element))
        }
        DataType::Struct(fields) => {
            let mut text = String::from("struct<");
            for (index, field) in fields.iter().enumerate() {
                if index != 0 {
                    text.push_str(", ");
                }
                let _ = write!(text, "{}", field_text(field));
            }
            text.push('>');
            text
        }
        DataType::Map(entries, _) => match entries.data_type() {
            DataType::Struct(fields) if fields.len() == 2 => format!(
                "map<{}, {}>",
                type_text(fields[0].data_type()),
                type_text(fields[1].data_type())
            ),
            other => format!("map<{}>", type_text(other)),
        },
        other => other.to_string(),
    }
}

/// pyarrow names a list's element `item` (the Delta reader's Arrow schema
/// does), whatever the Parquet-level name was.
fn element_text(element: &Field) -> String {
    let mut text = format!("item: {}", type_text(element.data_type()));
    if !element.is_nullable() {
        text.push_str(" not null");
    }
    text
}
