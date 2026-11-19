//! The structured row filter that replaces the SDK's pandas `query` (owner
//! decision D6): `filters` is an object of `column: condition`.
//!
//! A condition is a scalar (equality, the SDK's only form), a list (`in`),
//! `null` (`is_null`), or an object of operators (all must hold): `eq`, `ne`,
//! `in`, `gt`, `gte`, `lt`, `lte`, `is_null`. Every column must hold. Numbers
//! compare as numbers, strings as strings (dates and timestamps as their ISO
//! text, which orders correctly), booleans as booleans; a value of another
//! kind never matches an ordering operator. There is no expression language.

use std::cmp::Ordering;

use delta_kernel::arrow::array::{Array, ArrayRef, AsArray};
use delta_kernel::arrow::datatypes::{
    DataType, Float32Type, Float64Type, Int8Type, Int16Type, Int32Type, Int64Type, UInt8Type,
    UInt16Type, UInt32Type, UInt64Type,
};
use delta_kernel::arrow::record_batch::RecordBatch;
use delta_kernel::arrow::util::display::{ArrayFormatter, FormatOptions};
use delta_kernel::expressions::{Expression, Predicate, PredicateRef, Scalar};
use delta_kernel::schema::{DataType as KernelType, PrimitiveType, StructType};
use serde_json::{Map, Value};
use std::sync::Arc;

pub(in crate::toolkits) const MAX_FILTER_COLUMNS: usize = 32;
const MAX_CONDITIONS_PER_COLUMN: usize = 8;
const MAX_IN_VALUES: usize = 1_000;
const MAX_LITERAL_BYTES: usize = 4_096;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Op {
    Eq,
    Ne,
    In,
    Gt,
    Gte,
    Lt,
    Lte,
    IsNull,
}

#[derive(Debug)]
enum Operand {
    Scalar(Value),
    List(Vec<Value>),
    Flag(bool),
}

#[derive(Debug)]
struct Condition {
    op: Op,
    operand: Operand,
}

#[derive(Debug)]
struct ColumnFilter {
    column: String,
    conditions: Vec<Condition>,
}

/// A parsed `filters` argument; empty matches every row.
#[derive(Debug, Default)]
pub(in crate::toolkits) struct RowFilter {
    columns: Vec<ColumnFilter>,
}

impl RowFilter {
    /// Parse the `filters` argument (`None` and `null` filter nothing).
    pub(in crate::toolkits) fn parse(value: Option<&Value>) -> Result<Self, String> {
        let Some(value) = value.filter(|value| !value.is_null()) else {
            return Ok(Self::default());
        };
        let object = value
            .as_object()
            .ok_or_else(|| "filters must be an object of column: condition pairs.".to_owned())?;
        if object.len() > MAX_FILTER_COLUMNS {
            return Err(format!(
                "filters name {} columns; at most {MAX_FILTER_COLUMNS} are allowed.",
                object.len()
            ));
        }
        let mut columns = Vec::with_capacity(object.len());
        for (column, condition) in object {
            columns.push(ColumnFilter {
                column: column.clone(),
                conditions: parse_conditions(column, condition)?,
            });
        }
        Ok(Self { columns })
    }

    /// The columns the filter reads.
    pub(in crate::toolkits) fn columns(&self) -> impl Iterator<Item = &str> {
        self.columns.iter().map(|filter| filter.column.as_str())
    }

    /// What the kernel may use to skip whole files (by partition value and
    /// column statistics): the comparisons of this filter on a column whose
    /// type matches the literal exactly (integers, doubles, strings). Anything
    /// else is left to the row filter, which tests every row anyway, so a
    /// condition left out here is never a wrong answer, only a slower read.
    pub(in crate::toolkits) fn skipping_predicate(
        &self,
        schema: &StructType,
    ) -> Option<PredicateRef> {
        let mut predicates = Vec::new();
        for filter in &self.columns {
            let Some(field) = schema.field(&filter.column) else {
                continue;
            };
            let KernelType::Primitive(kind) = field.data_type() else {
                continue;
            };
            for condition in &filter.conditions {
                let Operand::Scalar(literal) = &condition.operand else {
                    continue;
                };
                let Some(scalar) = scalar_for(kind, literal) else {
                    continue;
                };
                let column = Expression::column([filter.column.as_str()]);
                let literal = Expression::literal(scalar);
                predicates.push(match condition.op {
                    Op::Eq => column.eq(literal),
                    Op::Gt => column.gt(literal),
                    Op::Gte => column.ge(literal),
                    Op::Lt => column.lt(literal),
                    Op::Lte => column.le(literal),
                    Op::Ne | Op::In | Op::IsNull => continue,
                });
            }
        }
        (!predicates.is_empty()).then(|| Arc::new(Predicate::and_from(predicates)))
    }

    /// The rows of `batch` that satisfy every condition.
    pub(in crate::toolkits) fn matches(&self, batch: &RecordBatch) -> Result<Vec<bool>, String> {
        let mut mask = vec![true; batch.num_rows()];
        for filter in &self.columns {
            let index = batch
                .schema()
                .index_of(&filter.column)
                .map_err(|_| format!("Column '{}' not found in table.", filter.column))?;
            let reader = ColumnReader::new(batch.column(index));
            for (row, keep) in mask.iter_mut().enumerate() {
                if *keep {
                    let cell = reader.cell(row);
                    *keep = filter
                        .conditions
                        .iter()
                        .all(|condition| condition.holds(&cell));
                }
            }
        }
        Ok(mask)
    }
}

fn parse_conditions(column: &str, value: &Value) -> Result<Vec<Condition>, String> {
    match value {
        Value::Null => Ok(vec![Condition {
            op: Op::IsNull,
            operand: Operand::Flag(true),
        }]),
        Value::Bool(_) | Value::Number(_) | Value::String(_) => {
            check_literal(column, value)?;
            Ok(vec![Condition {
                op: Op::Eq,
                operand: Operand::Scalar(value.clone()),
            }])
        }
        Value::Array(values) => Ok(vec![in_condition(column, values)?]),
        Value::Object(operators) => parse_operators(column, operators),
    }
}

fn parse_operators(column: &str, operators: &Map<String, Value>) -> Result<Vec<Condition>, String> {
    if operators.is_empty() || operators.len() > MAX_CONDITIONS_PER_COLUMN {
        return Err(format!(
            "The filter for column '{column}' must name between 1 and {MAX_CONDITIONS_PER_COLUMN} operators."
        ));
    }
    let mut conditions = Vec::with_capacity(operators.len());
    for (name, operand) in operators {
        let op = match name.as_str() {
            "eq" => Op::Eq,
            "ne" => Op::Ne,
            "in" => Op::In,
            "gt" => Op::Gt,
            "gte" => Op::Gte,
            "lt" => Op::Lt,
            "lte" => Op::Lte,
            "is_null" => Op::IsNull,
            other => {
                return Err(format!(
                    "Unsupported filter operator '{other}' for column '{column}'. Supported: eq, ne, in, gt, gte, lt, lte, is_null."
                ));
            }
        };
        conditions.push(match op {
            Op::In => {
                let values = operand.as_array().ok_or_else(|| {
                    format!("The 'in' filter for column '{column}' needs a list of values.")
                })?;
                in_condition(column, values)?
            }
            Op::IsNull => Condition {
                op,
                operand: Operand::Flag(operand.as_bool().ok_or_else(|| {
                    format!("The 'is_null' filter for column '{column}' needs true or false.")
                })?),
            },
            Op::Gt | Op::Gte | Op::Lt | Op::Lte => {
                if !matches!(operand, Value::Number(_) | Value::String(_)) {
                    return Err(format!(
                        "The '{name}' filter for column '{column}' needs a number or a string."
                    ));
                }
                check_literal(column, operand)?;
                Condition {
                    op,
                    operand: Operand::Scalar(operand.clone()),
                }
            }
            Op::Eq | Op::Ne => {
                if operand.is_array() || operand.is_object() {
                    return Err(format!(
                        "The '{name}' filter for column '{column}' needs a single value."
                    ));
                }
                check_literal(column, operand)?;
                Condition {
                    op,
                    operand: Operand::Scalar(operand.clone()),
                }
            }
        });
    }
    Ok(conditions)
}

fn in_condition(column: &str, values: &[Value]) -> Result<Condition, String> {
    if values.len() > MAX_IN_VALUES {
        return Err(format!(
            "The 'in' filter for column '{column}' lists {} values; at most {MAX_IN_VALUES} are allowed.",
            values.len()
        ));
    }
    for value in values {
        if value.is_array() || value.is_object() {
            return Err(format!(
                "The 'in' filter for column '{column}' lists single values only."
            ));
        }
        check_literal(column, value)?;
    }
    Ok(Condition {
        op: Op::In,
        operand: Operand::List(values.to_vec()),
    })
}

fn check_literal(column: &str, value: &Value) -> Result<(), String> {
    if matches!(value, Value::String(text) if text.len() > MAX_LITERAL_BYTES) {
        return Err(format!(
            "A filter value for column '{column}' is longer than {MAX_LITERAL_BYTES} bytes."
        ));
    }
    Ok(())
}

impl Condition {
    fn holds(&self, cell: &Cell) -> bool {
        match (&self.op, &self.operand) {
            (Op::IsNull, Operand::Flag(flag)) => matches!(cell, Cell::Null) == *flag,
            (Op::Eq, Operand::Scalar(literal)) => equals(cell, literal),
            (Op::Ne, Operand::Scalar(literal)) => !equals(cell, literal),
            (Op::In, Operand::List(literals)) => {
                literals.iter().any(|literal| equals(cell, literal))
            }
            (Op::Gt, Operand::Scalar(literal)) => compare(cell, literal) == Some(Ordering::Greater),
            (Op::Gte, Operand::Scalar(literal)) => {
                matches!(
                    compare(cell, literal),
                    Some(Ordering::Greater | Ordering::Equal)
                )
            }
            (Op::Lt, Operand::Scalar(literal)) => compare(cell, literal) == Some(Ordering::Less),
            (Op::Lte, Operand::Scalar(literal)) => {
                matches!(
                    compare(cell, literal),
                    Some(Ordering::Less | Ordering::Equal)
                )
            }
            _ => false,
        }
    }
}

fn equals(cell: &Cell, literal: &Value) -> bool {
    match literal {
        Value::Null => matches!(cell, Cell::Null),
        other => compare(cell, other) == Some(Ordering::Equal),
    }
}

/// One cell of a column, reduced to the four kinds a filter compares.
#[derive(Debug)]
enum Cell {
    Null,
    Bool(bool),
    Int(i128),
    Float(f64),
    Text(String),
}

#[allow(
    clippy::cast_precision_loss,
    reason = "an integer cell compared with a fractional literal is compared as a float"
)]
fn compare(cell: &Cell, literal: &Value) -> Option<Ordering> {
    match (cell, literal) {
        (Cell::Int(cell), Value::Number(number)) => {
            if let Some(literal) = number.as_i64() {
                Some(cell.cmp(&i128::from(literal)))
            } else if let Some(literal) = number.as_u64() {
                Some(cell.cmp(&i128::from(literal)))
            } else {
                (*cell as f64).partial_cmp(&number.as_f64()?)
            }
        }
        (Cell::Float(cell), Value::Number(number)) => cell.partial_cmp(&number.as_f64()?),
        (Cell::Text(cell), Value::String(literal)) => Some(cell.as_str().cmp(literal.as_str())),
        (Cell::Bool(cell), Value::Bool(literal)) => Some(cell.cmp(literal)),
        _ => None,
    }
}

/// Reads one column's cells. A type with no native reading is read through
/// Arrow's display, which is the ISO text for dates and timestamps and the
/// exact digits for decimals.
struct ColumnReader<'a> {
    array: &'a ArrayRef,
    display: Option<ArrayFormatter<'a>>,
}

impl<'a> ColumnReader<'a> {
    fn new(array: &'a ArrayRef) -> Self {
        let native = matches!(
            array.data_type(),
            DataType::Boolean
                | DataType::Int8
                | DataType::Int16
                | DataType::Int32
                | DataType::Int64
                | DataType::UInt8
                | DataType::UInt16
                | DataType::UInt32
                | DataType::UInt64
                | DataType::Float32
                | DataType::Float64
                | DataType::Utf8
                | DataType::LargeUtf8
                | DataType::Utf8View
        );
        let display = if native {
            None
        } else {
            ArrayFormatter::try_new(array.as_ref(), &FormatOptions::default()).ok()
        };
        Self { array, display }
    }

    fn cell(&self, row: usize) -> Cell {
        let array = self.array;
        if array.is_null(row) {
            return Cell::Null;
        }
        match array.data_type() {
            DataType::Boolean => Cell::Bool(array.as_boolean().value(row)),
            DataType::Int8 => Cell::Int(i128::from(array.as_primitive::<Int8Type>().value(row))),
            DataType::Int16 => Cell::Int(i128::from(array.as_primitive::<Int16Type>().value(row))),
            DataType::Int32 => Cell::Int(i128::from(array.as_primitive::<Int32Type>().value(row))),
            DataType::Int64 => Cell::Int(i128::from(array.as_primitive::<Int64Type>().value(row))),
            DataType::UInt8 => Cell::Int(i128::from(array.as_primitive::<UInt8Type>().value(row))),
            DataType::UInt16 => {
                Cell::Int(i128::from(array.as_primitive::<UInt16Type>().value(row)))
            }
            DataType::UInt32 => {
                Cell::Int(i128::from(array.as_primitive::<UInt32Type>().value(row)))
            }
            DataType::UInt64 => {
                Cell::Int(i128::from(array.as_primitive::<UInt64Type>().value(row)))
            }
            DataType::Float32 => {
                Cell::Float(f64::from(array.as_primitive::<Float32Type>().value(row)))
            }
            DataType::Float64 => Cell::Float(array.as_primitive::<Float64Type>().value(row)),
            DataType::Utf8 => Cell::Text(array.as_string::<i32>().value(row).to_owned()),
            DataType::LargeUtf8 => Cell::Text(array.as_string::<i64>().value(row).to_owned()),
            DataType::Utf8View => Cell::Text(array.as_string_view().value(row).to_owned()),
            DataType::Decimal128(..) | DataType::Decimal256(..) => self
                .shown(row)
                .and_then(|text| text.parse::<f64>().ok())
                .map_or(Cell::Null, Cell::Float),
            _ => self.shown(row).map_or(Cell::Null, Cell::Text),
        }
    }

    fn shown(&self, row: usize) -> Option<String> {
        self.display
            .as_ref()
            .map(|display| display.value(row).to_string())
    }
}

/// The kernel scalar of a filter literal for a column of exactly this type.
fn scalar_for(kind: &PrimitiveType, literal: &Value) -> Option<Scalar> {
    match (kind, literal) {
        (PrimitiveType::Long, Value::Number(number)) => number.as_i64().map(Scalar::Long),
        (PrimitiveType::Integer, Value::Number(number)) => number
            .as_i64()
            .and_then(|value| i32::try_from(value).ok())
            .map(Scalar::Integer),
        (PrimitiveType::Short, Value::Number(number)) => number
            .as_i64()
            .and_then(|value| i16::try_from(value).ok())
            .map(Scalar::Short),
        (PrimitiveType::Byte, Value::Number(number)) => number
            .as_i64()
            .and_then(|value| i8::try_from(value).ok())
            .map(Scalar::Byte),
        (PrimitiveType::Double, Value::Number(number)) => number.as_f64().map(Scalar::Double),
        (PrimitiveType::String, Value::String(text)) => Some(Scalar::String(text.clone())),
        _ => None,
    }
}
