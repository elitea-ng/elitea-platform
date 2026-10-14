//! The deterministic table operations: filter, sort, aggregate, pivot, join,
//! describe and correlate (ADR-0033 decision 1).
//!
//! Each takes a [`Table`] and returns a new one. Nothing here touches a file,
//! the network or a model, and every loop over rows looks at the call's
//! [`Deadline`], so an operation stops when its time is up instead of running
//! on in a thread nobody waits for.

use std::cmp::Ordering;
use std::collections::{HashMap, HashSet};

use regex::RegexBuilder;
use serde_json::{Map, Value, json};

use super::table::{
    CHECK_EVERY, Cell, Column, DType, DataError, DataResult, Deadline, Limits, Table, argument,
    format_time, parse_bool, parse_time, unique_names,
};

// ---------------------------------------------------------------------------
// Filter
// ---------------------------------------------------------------------------

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Op {
    Eq,
    Ne,
    Gt,
    Ge,
    Lt,
    Le,
    Contains,
    NotContains,
    StartsWith,
    EndsWith,
    In,
    NotIn,
    IsNull,
    NotNull,
    Matches,
}

impl Op {
    pub(crate) const NAMES: [&'static str; 15] = [
        "eq",
        "ne",
        "gt",
        "ge",
        "lt",
        "le",
        "contains",
        "not_contains",
        "starts_with",
        "ends_with",
        "in",
        "not_in",
        "is_null",
        "not_null",
        "matches",
    ];

    fn parse(name: &str) -> DataResult<Self> {
        Ok(match name.trim().to_ascii_lowercase().as_str() {
            "eq" | "==" | "=" => Self::Eq,
            "ne" | "!=" | "<>" => Self::Ne,
            "gt" | ">" => Self::Gt,
            "ge" | ">=" => Self::Ge,
            "lt" | "<" => Self::Lt,
            "le" | "<=" => Self::Le,
            "contains" => Self::Contains,
            "not_contains" => Self::NotContains,
            "starts_with" => Self::StartsWith,
            "ends_with" => Self::EndsWith,
            "in" => Self::In,
            "not_in" => Self::NotIn,
            "is_null" => Self::IsNull,
            "not_null" => Self::NotNull,
            "matches" | "regex" => Self::Matches,
            other => {
                return Err(argument(format!(
                    "Unknown filter operator `{other}`. Use one of: {}.",
                    Self::NAMES.join(", ")
                )));
            }
        })
    }
}

#[derive(Clone, Debug)]
pub(crate) struct Condition {
    pub(crate) column: String,
    pub(crate) op: Op,
    pub(crate) value: Value,
}

#[derive(Clone, Debug)]
pub(crate) struct FilterSpec {
    pub(crate) conditions: Vec<Condition>,
    pub(crate) any: bool,
    pub(crate) case_sensitive: bool,
}

const MAX_CONDITIONS: usize = 50;
const MAX_IN_VALUES: usize = 10_000;
const MAX_PATTERN_BYTES: usize = 1_000;

/// The filter arguments shared by every tool that takes `filters`.
pub(crate) fn parse_filter(args: &Value) -> DataResult<Option<FilterSpec>> {
    let Some(list) = args.get("filters") else {
        return Ok(None);
    };
    if list.is_null() {
        return Ok(None);
    }
    let list = list
        .as_array()
        .ok_or_else(|| argument("`filters` must be a list of {column, operator, value}."))?;
    if list.is_empty() {
        return Ok(None);
    }
    if list.len() > MAX_CONDITIONS {
        return Err(argument(format!(
            "At most {MAX_CONDITIONS} filter conditions are allowed."
        )));
    }
    let mut conditions = Vec::with_capacity(list.len());
    for item in list {
        let column = item
            .get("column")
            .and_then(Value::as_str)
            .ok_or_else(|| argument("Each filter needs a `column` name."))?;
        let op = item
            .get("operator")
            .or_else(|| item.get("op"))
            .and_then(Value::as_str)
            .ok_or_else(|| argument("Each filter needs an `operator`."))?;
        let op = Op::parse(op)?;
        let value = item.get("value").cloned().unwrap_or(Value::Null);
        if !matches!(op, Op::IsNull | Op::NotNull) && value.is_null() {
            return Err(argument(format!(
                "The filter on `{column}` needs a `value` (only is_null and not_null take none)."
            )));
        }
        conditions.push(Condition {
            column: column.to_owned(),
            op,
            value,
        });
    }
    let any = match args.get("match").and_then(Value::as_str) {
        None | Some("all") => false,
        Some("any") => true,
        Some(other) => {
            return Err(argument(format!(
                "`match` must be `all` or `any`, not `{other}`."
            )));
        }
    };
    Ok(Some(FilterSpec {
        conditions,
        any,
        case_sensitive: args
            .get("case_sensitive")
            .and_then(Value::as_bool)
            .unwrap_or(true),
    }))
}

#[derive(Clone, Copy)]
enum Num {
    I(i64),
    F(f64),
}

fn num_cmp(a: Num, b: Num) -> Option<Ordering> {
    match (a, b) {
        (Num::I(a), Num::I(b)) => Some(a.cmp(&b)),
        (Num::I(a), Num::F(b)) => (a as f64).partial_cmp(&b),
        (Num::F(a), Num::I(b)) => a.partial_cmp(&(b as f64)),
        (Num::F(a), Num::F(b)) => a.partial_cmp(&b),
    }
}

fn json_num(value: &Value, column: &str) -> DataResult<Num> {
    let fail = || {
        argument(format!(
            "The column `{column}` holds numbers, but the filter value {value} is not a number."
        ))
    };
    match value {
        Value::Number(n) => n
            .as_i64()
            .map(Num::I)
            .or_else(|| n.as_f64().map(Num::F))
            .ok_or_else(fail),
        Value::String(s) => {
            let s = s.trim();
            super::table::parse_int(s)
                .map(Num::I)
                .or_else(|| super::table::parse_float(s).map(Num::F))
                .ok_or_else(fail)
        }
        _ => Err(fail()),
    }
}

fn json_text(value: &Value) -> String {
    match value {
        Value::String(s) => s.clone(),
        other => other.to_string(),
    }
}

fn json_time(value: &Value, column: &str) -> DataResult<i64> {
    value
        .as_str()
        .and_then(|s| parse_time(s.trim()))
        .map(|p| p.0)
        .ok_or_else(|| {
            argument(format!(
                "The column `{column}` holds dates, so the filter value {value} must be a date like 2024-01-31 or 2024-01-31T10:00:00."
            ))
        })
}

type Predicate<'a> = Box<dyn Fn(usize) -> bool + 'a>;

fn compile<'a>(
    table: &'a Table,
    condition: &Condition,
    case_sensitive: bool,
) -> DataResult<Predicate<'a>> {
    let index = table.find(&condition.column)?;
    let column = &table.columns[index];
    let name = table.names[index].clone();
    let op = condition.op;
    match op {
        Op::IsNull => return Ok(Box::new(move |r| column.is_null(r))),
        Op::NotNull => return Ok(Box::new(move |r| !column.is_null(r))),
        _ => {}
    }
    let fold = move |s: &str| -> String {
        if case_sensitive {
            s.to_owned()
        } else {
            s.to_lowercase()
        }
    };
    if matches!(
        op,
        Op::Contains | Op::NotContains | Op::StartsWith | Op::EndsWith
    ) {
        let needle = fold(&json_text(&condition.value));
        return Ok(Box::new(move |r| {
            let cell = column.cell(r);
            if cell.is_null() {
                return false;
            }
            let hay = fold(&cell.text());
            match op {
                Op::Contains => hay.contains(&needle),
                Op::NotContains => !hay.contains(&needle),
                Op::StartsWith => hay.starts_with(&needle),
                _ => hay.ends_with(&needle),
            }
        }));
    }
    if op == Op::Matches {
        let pattern = json_text(&condition.value);
        if pattern.len() > MAX_PATTERN_BYTES {
            return Err(argument(format!(
                "The pattern for `{name}` is longer than {MAX_PATTERN_BYTES} bytes."
            )));
        }
        let regex = RegexBuilder::new(&pattern)
            .size_limit(1 << 20)
            .case_insensitive(!case_sensitive)
            .build()
            .map_err(|_| {
                argument(format!(
                    "The pattern for `{name}` is not a valid regular expression."
                ))
            })?;
        return Ok(Box::new(move |r| {
            let cell = column.cell(r);
            !cell.is_null() && regex.is_match(&cell.text())
        }));
    }
    if matches!(op, Op::In | Op::NotIn) {
        let values = condition.value.as_array().ok_or_else(|| {
            argument(format!(
                "The `in` filter on `{name}` needs a list of values."
            ))
        })?;
        if values.len() > MAX_IN_VALUES {
            return Err(argument(format!(
                "An `in` filter takes at most {MAX_IN_VALUES} values."
            )));
        }
        let negate = op == Op::NotIn;
        let members: HashSet<Key> = values
            .iter()
            .map(|v| member_key(column, &name, v, case_sensitive))
            .collect::<DataResult<_>>()?;
        return Ok(Box::new(move |r| {
            let Some(key) = Key::of(column, r) else {
                return false;
            };
            let key = match key {
                Key::T(text) if !case_sensitive => Key::T(text.to_lowercase().into()),
                other => other,
            };
            members.contains(&key) != negate
        }));
    }
    compare(column, &name, op, &condition.value, case_sensitive)
}

/// The hash key a listed `in` value stands for in `column`: the same key
/// `Key::of` makes for a cell, so membership is one lookup, not one comparison
/// per listed value.
fn member_key(column: &Column, name: &str, value: &Value, case_sensitive: bool) -> DataResult<Key> {
    Ok(match column {
        Column::Int(_) | Column::Float(_) => match json_num(value, name)? {
            Num::I(i) => Key::I(i),
            Num::F(f) if f.fract() == 0.0 && f.abs() < 9_007_199_254_740_992.0 => {
                Key::I(f as i64)
            }
            Num::F(f) => Key::F(f.to_bits()),
        },
        Column::Bool(_) => Key::B(match value {
            Value::Bool(b) => *b,
            Value::String(s) => parse_bool(s.trim()).ok_or_else(|| {
                argument(format!(
                    "The column `{name}` holds true/false, but the listed value {value} is not a boolean."
                ))
            })?,
            _ => {
                return Err(argument(format!(
                    "The column `{name}` holds true/false, but the listed value {value} is not a boolean."
                )));
            }
        }),
        Column::Date(_) | Column::DateTime(_) => Key::I(json_time(value, name)?),
        Column::Text(_) => {
            let text = json_text(value);
            Key::T(if case_sensitive { text } else { text.to_lowercase() }.into())
        }
    })
}

fn compare<'a>(
    column: &'a Column,
    name: &str,
    op: Op,
    value: &Value,
    case_sensitive: bool,
) -> DataResult<Predicate<'a>> {
    let accept = move |ord: Ordering| match op {
        Op::Eq => ord == Ordering::Equal,
        Op::Ne => ord != Ordering::Equal,
        Op::Gt => ord == Ordering::Greater,
        Op::Ge => ord != Ordering::Less,
        Op::Lt => ord == Ordering::Less,
        _ => ord != Ordering::Greater,
    };
    match column {
        Column::Int(_) | Column::Float(_) => {
            let rhs = json_num(value, name)?;
            Ok(Box::new(move |r| {
                let lhs = match column.cell(r) {
                    Cell::Int(v) => Num::I(v),
                    Cell::Float(v) => Num::F(v),
                    _ => return false,
                };
                num_cmp(lhs, rhs).is_some_and(accept)
            }))
        }
        Column::Bool(_) => {
            let rhs = match value {
                Value::Bool(b) => *b,
                Value::String(s) => parse_bool(s.trim()).ok_or_else(|| {
                    argument(format!(
                        "The column `{name}` holds true/false, but the filter value {value} is not a boolean."
                    ))
                })?,
                _ => {
                    return Err(argument(format!(
                        "The column `{name}` holds true/false, but the filter value {value} is not a boolean."
                    )));
                }
            };
            Ok(Box::new(move |r| match column.cell(r) {
                Cell::Bool(v) => accept(v.cmp(&rhs)),
                _ => false,
            }))
        }
        Column::Date(_) | Column::DateTime(_) => {
            let rhs = json_time(value, name)?;
            Ok(Box::new(move |r| match column.cell(r) {
                Cell::Date(v) | Cell::DateTime(v) => accept(v.cmp(&rhs)),
                _ => false,
            }))
        }
        Column::Text(_) => {
            let rhs = if case_sensitive {
                json_text(value)
            } else {
                json_text(value).to_lowercase()
            };
            Ok(Box::new(move |r| match column.cell(r) {
                Cell::Text(v) => {
                    if case_sensitive {
                        accept(v.cmp(rhs.as_str()))
                    } else {
                        accept(v.to_lowercase().as_str().cmp(rhs.as_str()))
                    }
                }
                _ => false,
            }))
        }
    }
}

/// The rows (in order) that satisfy `spec`.
pub(crate) fn matching_rows(
    table: &Table,
    spec: &FilterSpec,
    deadline: &Deadline,
) -> DataResult<Vec<usize>> {
    let predicates: Vec<Predicate<'_>> = spec
        .conditions
        .iter()
        .map(|c| compile(table, c, spec.case_sensitive))
        .collect::<DataResult<_>>()?;
    let mut rows = Vec::new();
    for row in 0..table.rows {
        if row % CHECK_EVERY == 0 {
            deadline.check()?;
        }
        let keep = if spec.any {
            predicates.iter().any(|p| p(row))
        } else {
            predicates.iter().all(|p| p(row))
        };
        if keep {
            rows.push(row);
        }
    }
    Ok(rows)
}

/// `table` limited to the rows of `filters`, when there are any.
pub(crate) fn apply_filter(table: Table, args: &Value, deadline: &Deadline) -> DataResult<Table> {
    match parse_filter(args)? {
        None => Ok(table),
        Some(spec) => {
            let rows = matching_rows(&table, &spec, deadline)?;
            Ok(table.take_rows(&rows))
        }
    }
}

// ---------------------------------------------------------------------------
// Sort
// ---------------------------------------------------------------------------

/// Order two rows of one column. Nulls sort last whichever way the key goes.
fn compare_rows(column: &Column, a: usize, b: usize, descending: bool) -> Ordering {
    let ord = match (column.cell(a), column.cell(b)) {
        (Cell::Null, Cell::Null) => return Ordering::Equal,
        (Cell::Null, _) => return Ordering::Greater,
        (_, Cell::Null) => return Ordering::Less,
        (Cell::Int(x), Cell::Int(y)) => x.cmp(&y),
        (Cell::Float(x), Cell::Float(y)) => x.total_cmp(&y),
        (Cell::Bool(x), Cell::Bool(y)) => x.cmp(&y),
        (Cell::Text(x), Cell::Text(y)) => x.cmp(y),
        (Cell::Date(x) | Cell::DateTime(x), Cell::Date(y) | Cell::DateTime(y)) => x.cmp(&y),
        _ => Ordering::Equal,
    };
    if descending { ord.reverse() } else { ord }
}

/// `sort_by`: a list of column names, or of `{column, descending}`.
pub(crate) fn parse_sort(table: &Table, args: &Value) -> DataResult<Vec<(usize, bool)>> {
    let Some(list) = args.get("sort_by").filter(|v| !v.is_null()) else {
        return Ok(Vec::new());
    };
    let list = list
        .as_array()
        .ok_or_else(|| argument("`sort_by` must be a list of columns."))?;
    if list.len() > 20 {
        return Err(argument("At most 20 sort keys are allowed."));
    }
    list.iter()
        .map(|item| match item {
            Value::String(name) => Ok((table.find(name)?, false)),
            Value::Object(object) => {
                let name = object
                    .get("column")
                    .and_then(Value::as_str)
                    .ok_or_else(|| argument("Each sort key needs a `column`."))?;
                let descending = object
                    .get("descending")
                    .and_then(Value::as_bool)
                    .unwrap_or(false);
                Ok((table.find(name)?, descending))
            }
            _ => Err(argument(
                "Each sort key is a column name or {column, descending}.",
            )),
        })
        .collect()
}

/// The row order of `table` under `keys`, stable.
pub(crate) fn sorted_rows(
    table: &Table,
    keys: &[(usize, bool)],
    deadline: &Deadline,
) -> DataResult<Vec<usize>> {
    let mut rows: Vec<usize> = (0..table.rows).collect();
    if keys.is_empty() {
        return Ok(rows);
    }
    // A sort of at most `max_rows` rows takes well under a second, and a
    // comparator that gave up midway would break the total order the standard
    // sort relies on, so the clock is read around the sort, not inside it.
    deadline.check()?;
    rows.sort_by(|a, b| {
        for (column, descending) in keys {
            let ord = compare_rows(&table.columns[*column], *a, *b, *descending);
            if ord != Ordering::Equal {
                return ord;
            }
        }
        Ordering::Equal
    });
    deadline.check()?;
    Ok(rows)
}

pub(crate) fn sort_table(
    table: &Table,
    keys: &[(usize, bool)],
    deadline: &Deadline,
) -> DataResult<Table> {
    let rows = sorted_rows(table, keys, deadline)?;
    Ok(table.take_rows(&rows))
}

// ---------------------------------------------------------------------------
// Aggregation
// ---------------------------------------------------------------------------

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Func {
    Count,
    Sum,
    Mean,
    Median,
    Min,
    Max,
    Std,
    Var,
    Nunique,
    First,
    Last,
}

impl Func {
    pub(crate) const NAMES: [&'static str; 11] = [
        "count", "sum", "mean", "median", "min", "max", "std", "var", "nunique", "first", "last",
    ];

    pub(crate) fn parse(name: &str) -> DataResult<Self> {
        Ok(match name.trim().to_ascii_lowercase().as_str() {
            "count" => Self::Count,
            "sum" => Self::Sum,
            "mean" | "avg" | "average" => Self::Mean,
            "median" => Self::Median,
            "min" => Self::Min,
            "max" => Self::Max,
            "std" | "stdev" => Self::Std,
            "var" | "variance" => Self::Var,
            "nunique" | "count_distinct" | "distinct_count" => Self::Nunique,
            "first" => Self::First,
            "last" => Self::Last,
            other => {
                return Err(argument(format!(
                    "Unknown function `{other}`. Use one of: {}.",
                    Self::NAMES.join(", ")
                )));
            }
        })
    }

    pub(crate) const fn name(self) -> &'static str {
        Self::NAMES[self as usize]
    }
}

/// The column type a function produces over a column type.
fn result_type(input: DType, func: Func, column: &str) -> DataResult<DType> {
    let refuse = |what: &str| {
        Err(argument(format!(
            "Cannot compute {} of the {what} column `{column}`.",
            func.name()
        )))
    };
    match func {
        Func::Count | Func::Nunique => Ok(DType::Int),
        Func::Sum => match input {
            DType::Int | DType::Bool => Ok(DType::Int),
            DType::Float => Ok(DType::Float),
            other => refuse(other.name()),
        },
        Func::Mean | Func::Median | Func::Std | Func::Var => match input {
            DType::Int | DType::Float | DType::Bool => Ok(DType::Float),
            other => refuse(other.name()),
        },
        Func::Min | Func::Max | Func::First | Func::Last => Ok(input),
    }
}

/// One aggregated value, before it is put in a column.
#[derive(Clone, Debug)]
enum Val {
    Null,
    Int(i64),
    Float(f64),
    Bool(bool),
    Text(Box<str>),
    Time(i64),
}

fn aggregate_rows(column: &Column, rows: &[usize], func: Func) -> DataResult<Val> {
    let present = || rows.iter().copied().filter(|r| !column.is_null(*r));
    match func {
        Func::Count => return Ok(Val::Int(present().count() as i64)),
        Func::Nunique => {
            let mut seen: HashSet<Key> = HashSet::new();
            for r in present() {
                if let Some(k) = Key::of(column, r) {
                    seen.insert(k);
                }
            }
            return Ok(Val::Int(seen.len() as i64));
        }
        Func::First | Func::Last => {
            let row = if func == Func::First {
                present().next()
            } else {
                present().next_back()
            };
            return Ok(row.map_or(Val::Null, |r| val_of(column.cell(r))));
        }
        Func::Min | Func::Max => {
            let mut best: Option<usize> = None;
            for r in present() {
                best = Some(match best {
                    None => r,
                    Some(b) => {
                        let ord = compare_rows(column, r, b, false);
                        if (func == Func::Min && ord == Ordering::Less)
                            || (func == Func::Max && ord == Ordering::Greater)
                        {
                            r
                        } else {
                            b
                        }
                    }
                });
            }
            return Ok(best.map_or(Val::Null, |r| val_of(column.cell(r))));
        }
        _ => {}
    }
    if func == Func::Sum && matches!(column, Column::Int(_) | Column::Bool(_)) {
        let mut total: i128 = 0;
        let mut any = false;
        for r in present() {
            any = true;
            total += match column.cell(r) {
                Cell::Int(v) => i128::from(v),
                Cell::Bool(v) => i128::from(v),
                _ => 0,
            };
        }
        if !any {
            return Ok(Val::Int(0));
        }
        return i64::try_from(total).map(Val::Int).map_err(|_| {
            DataError::Limit("The sum is larger than a 64-bit integer can hold.".into())
        });
    }
    let mut values: Vec<f64> = present().filter_map(|r| column.cell(r).as_f64()).collect();
    let n = values.len();
    if func == Func::Sum {
        return Ok(Val::Float(values.iter().sum()));
    }
    if n == 0 {
        return Ok(Val::Null);
    }
    let mean = values.iter().sum::<f64>() / n as f64;
    Ok(match func {
        Func::Mean => Val::Float(mean),
        Func::Median => {
            values.sort_by(f64::total_cmp);
            Val::Float(quantile(&values, 0.5))
        }
        Func::Var | Func::Std => {
            if n < 2 {
                Val::Null
            } else {
                let var = values.iter().map(|v| (v - mean).powi(2)).sum::<f64>() / (n - 1) as f64;
                Val::Float(if func == Func::Std { var.sqrt() } else { var })
            }
        }
        _ => Val::Null,
    })
}

fn val_of(cell: Cell<'_>) -> Val {
    match cell {
        Cell::Null => Val::Null,
        Cell::Int(v) => Val::Int(v),
        Cell::Float(v) => Val::Float(v),
        Cell::Bool(v) => Val::Bool(v),
        Cell::Text(v) => Val::Text(v.into()),
        Cell::Date(v) | Cell::DateTime(v) => Val::Time(v),
    }
}

/// Linear-interpolated quantile of sorted values.
pub(crate) fn quantile(sorted: &[f64], q: f64) -> f64 {
    let position = (sorted.len() - 1) as f64 * q;
    let (low, high) = (position.floor() as usize, position.ceil() as usize);
    sorted[low] + (sorted[high] - sorted[low]) * (position - low as f64)
}

fn column_of(dtype: DType, values: Vec<Val>) -> Column {
    match dtype {
        DType::Int => Column::Int(
            values
                .into_iter()
                .map(|v| match v {
                    Val::Int(i) => Some(i),
                    _ => None,
                })
                .collect(),
        ),
        DType::Float => Column::Float(
            values
                .into_iter()
                .map(|v| match v {
                    Val::Float(f) if f.is_finite() => Some(f),
                    Val::Int(i) => Some(i as f64),
                    _ => None,
                })
                .collect(),
        ),
        DType::Bool => Column::Bool(
            values
                .into_iter()
                .map(|v| match v {
                    Val::Bool(b) => Some(b),
                    _ => None,
                })
                .collect(),
        ),
        DType::Text => Column::Text(
            values
                .into_iter()
                .map(|v| match v {
                    Val::Text(t) => Some(t),
                    _ => None,
                })
                .collect(),
        ),
        DType::Date | DType::DateTime => {
            let times = values
                .into_iter()
                .map(|v| match v {
                    Val::Time(t) => Some(t),
                    _ => None,
                })
                .collect();
            if dtype == DType::Date {
                Column::Date(times)
            } else {
                Column::DateTime(times)
            }
        }
    }
}

/// A group key: equal values are equal keys across `Int` and `Float`.
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
enum Key {
    I(i64),
    F(u64),
    B(bool),
    T(Box<str>),
}

impl Key {
    fn of(column: &Column, row: usize) -> Option<Key> {
        match column.cell(row) {
            Cell::Null => None,
            Cell::Int(v) | Cell::Date(v) | Cell::DateTime(v) => Some(Key::I(v)),
            Cell::Float(v) => {
                if v.fract() == 0.0 && v.abs() < 9_007_199_254_740_992.0 {
                    Some(Key::I(v as i64))
                } else {
                    Some(Key::F(v.to_bits()))
                }
            }
            Cell::Bool(v) => Some(Key::B(v)),
            Cell::Text(v) => Some(Key::T(v.into())),
        }
    }
}

/// The rows of `table` grouped by `keys`, in key order. A row with a null key
/// belongs to no group (pandas' default). With no keys, one group holds every
/// row.
fn group_rows(
    table: &Table,
    keys: &[usize],
    limits: &Limits,
    deadline: &Deadline,
) -> DataResult<Vec<Vec<usize>>> {
    if keys.is_empty() {
        return Ok(vec![(0..table.rows).collect()]);
    }
    let mut index: HashMap<Vec<Key>, usize> = HashMap::new();
    let mut groups: Vec<Vec<usize>> = Vec::new();
    'rows: for row in 0..table.rows {
        if row % CHECK_EVERY == 0 {
            deadline.check()?;
        }
        let mut key = Vec::with_capacity(keys.len());
        for k in keys {
            match Key::of(&table.columns[*k], row) {
                Some(part) => key.push(part),
                None => continue 'rows,
            }
        }
        let next = groups.len();
        let slot = *index.entry(key).or_insert(next);
        if slot == next {
            if groups.len() >= limits.max_rows {
                return Err(DataError::Limit("Too many groups.".into()));
            }
            groups.push(Vec::new());
        }
        groups[slot].push(row);
    }
    let order: Vec<(usize, bool)> = keys.iter().map(|k| (*k, false)).collect();
    groups.sort_by(|a, b| {
        for (column, descending) in &order {
            let ord = compare_rows(&table.columns[*column], a[0], b[0], *descending);
            if ord != Ordering::Equal {
                return ord;
            }
        }
        Ordering::Equal
    });
    Ok(groups)
}

pub(crate) struct AggSpec {
    pub(crate) column: Option<usize>,
    pub(crate) func: Func,
    pub(crate) name: String,
}

/// Parse `aggregations`: `{column, function, name?}`. Column `*` with
/// `count` counts rows.
pub(crate) fn parse_aggregations(table: &Table, args: &Value) -> DataResult<Vec<AggSpec>> {
    let list = args
        .get("aggregations")
        .and_then(Value::as_array)
        .ok_or_else(|| {
            argument("`aggregations` is required: a list of {column, function, name?}.")
        })?;
    if list.is_empty() || list.len() > 100 {
        return Err(argument("`aggregations` needs between 1 and 100 entries."));
    }
    list.iter()
        .map(|item| {
            let column = item
                .get("column")
                .and_then(Value::as_str)
                .ok_or_else(|| argument("Each aggregation needs a `column` (or `*`)."))?;
            let func = Func::parse(
                item.get("function")
                    .and_then(Value::as_str)
                    .ok_or_else(|| argument("Each aggregation needs a `function`."))?,
            )?;
            let alias = item
                .get("name")
                .and_then(Value::as_str)
                .map(|n| n.trim().to_owned())
                .filter(|n| !n.is_empty());
            if column.trim() == "*" {
                if func != Func::Count {
                    return Err(argument("The column `*` works only with `count`."));
                }
                return Ok(AggSpec {
                    column: None,
                    func,
                    name: alias.unwrap_or_else(|| "count".into()),
                });
            }
            let index = table.find(column)?;
            result_type(table.columns[index].dtype(), func, &table.names[index])?;
            Ok(AggSpec {
                column: Some(index),
                func,
                name: alias.unwrap_or_else(|| format!("{}_{}", table.names[index], func.name())),
            })
        })
        .collect()
}

pub(crate) fn parse_columns(table: &Table, args: &Value, key: &str) -> DataResult<Vec<usize>> {
    let Some(list) = args.get(key).filter(|v| !v.is_null()) else {
        return Ok(Vec::new());
    };
    let list = match list {
        Value::String(one) => vec![Value::String(one.clone())],
        Value::Array(items) => items.clone(),
        _ => return Err(argument(format!("`{key}` must be a list of column names."))),
    };
    if list.len() > 200 {
        return Err(argument(format!("`{key}` names too many columns.")));
    }
    let mut out = Vec::new();
    for item in &list {
        let name = item
            .as_str()
            .ok_or_else(|| argument(format!("`{key}` must be a list of column names.")))?;
        let index = table.find(name)?;
        if !out.contains(&index) {
            out.push(index);
        }
    }
    Ok(out)
}

/// Refuse a result of `rows` x `columns` cells over the limit, before it exists.
fn over_cell_limit(rows: usize, columns: usize, limits: &Limits) -> DataResult<()> {
    if rows.saturating_mul(columns) > limits.max_cells {
        return Err(DataError::Limit(format!(
            "The result would have {rows} rows by {columns} columns, over the {} cell limit. Filter first, group by fewer columns, or ask for fewer aggregations.",
            limits.max_cells
        )));
    }
    Ok(())
}

pub(crate) fn aggregate(
    table: &Table,
    group_by: &[usize],
    specs: &[AggSpec],
    limits: &Limits,
    deadline: &Deadline,
) -> DataResult<Table> {
    let groups = group_rows(table, group_by, limits, deadline)?;
    // The result's size is known now; refuse before any column is allocated.
    over_cell_limit(groups.len(), group_by.len() + specs.len(), limits)?;
    let mut names: Vec<String> = group_by.iter().map(|k| table.names[*k].clone()).collect();
    let firsts: Vec<Option<usize>> = groups.iter().map(|g| g.first().copied()).collect();
    let mut columns: Vec<Column> = group_by
        .iter()
        .map(|k| table.columns[*k].gather(&firsts))
        .collect();
    for spec in specs {
        deadline.check()?;
        let mut values = Vec::with_capacity(groups.len());
        for (n, group) in groups.iter().enumerate() {
            if n % 256 == 0 {
                deadline.check()?;
            }
            values.push(match spec.column {
                None => Val::Int(group.len() as i64),
                Some(c) => aggregate_rows(&table.columns[c], group, spec.func)?,
            });
        }
        let dtype = match spec.column {
            None => DType::Int,
            Some(c) => result_type(table.columns[c].dtype(), spec.func, &table.names[c])?,
        };
        // A date column keeps its kind through min/max/first/last.
        columns.push(column_of(dtype, values));
        names.push(spec.name.clone());
    }
    let names = unique_names(names);
    let rows = groups.len();
    let result = Table {
        names,
        columns,
        rows,
    };
    if result.cell_count() > limits.max_cells {
        return Err(DataError::Limit(
            "The result is over the cell limit.".into(),
        ));
    }
    Ok(result)
}

// ---------------------------------------------------------------------------
// Pivot
// ---------------------------------------------------------------------------

pub(crate) struct PivotSpec<'a> {
    pub(crate) index: &'a [usize],
    pub(crate) columns: usize,
    pub(crate) values: usize,
    pub(crate) func: Func,
    pub(crate) fill: Option<f64>,
}

pub(crate) fn pivot(
    table: &Table,
    spec: &PivotSpec<'_>,
    limits: &Limits,
    deadline: &Deadline,
) -> DataResult<Table> {
    let value_column = &table.columns[spec.values];
    let dtype = result_type(value_column.dtype(), spec.func, &table.names[spec.values])?;
    let pivot_column = &table.columns[spec.columns];
    let groups = group_rows(table, spec.index, limits, deadline)?;
    // The distinct values of the pivot column, in their natural order.
    let mut distinct: HashMap<Key, usize> = HashMap::new();
    let mut representatives: Vec<usize> = Vec::new();
    for row in 0..table.rows {
        if row % CHECK_EVERY == 0 {
            deadline.check()?;
        }
        if let Some(key) = Key::of(pivot_column, row)
            && !distinct.contains_key(&key)
        {
            if representatives.len() >= limits.max_pivot_columns {
                return Err(DataError::Limit(format!(
                    "`{}` has more than {} distinct values, so a pivot would be too wide. Filter to the values you need, or aggregate instead.",
                    table.names[spec.columns], limits.max_pivot_columns
                )));
            }
            distinct.insert(key, representatives.len());
            representatives.push(row);
        }
    }
    let mut order: Vec<usize> = (0..representatives.len()).collect();
    order.sort_by(|a, b| {
        compare_rows(
            pivot_column,
            representatives[*a],
            representatives[*b],
            false,
        )
    });
    let mut slot_of = vec![0usize; order.len()];
    for (position, original) in order.iter().enumerate() {
        slot_of[*original] = position;
    }
    // groups x (index columns + pivot columns) is known: refuse before the
    // first cell is allocated, not after the table is built.
    over_cell_limit(groups.len(), spec.index.len() + order.len(), limits)?;
    let mut cells: Vec<Vec<Val>> = (0..order.len())
        .map(|_| Vec::with_capacity(groups.len()))
        .collect();
    for (n, group) in groups.iter().enumerate() {
        if n % 256 == 0 {
            deadline.check()?;
        }
        let mut by_value: Vec<Vec<usize>> = vec![Vec::new(); order.len()];
        for row in group {
            if let Some(key) = Key::of(pivot_column, *row) {
                by_value[slot_of[distinct[&key]]].push(*row);
            }
        }
        for (slot, rows) in by_value.iter().enumerate() {
            cells[slot].push(if rows.is_empty() {
                Val::Null
            } else {
                aggregate_rows(value_column, rows, spec.func)?
            });
        }
    }
    let firsts: Vec<Option<usize>> = groups.iter().map(|g| g.first().copied()).collect();
    let mut names: Vec<String> = spec.index.iter().map(|k| table.names[*k].clone()).collect();
    let mut columns: Vec<Column> = spec
        .index
        .iter()
        .map(|k| table.columns[*k].gather(&firsts))
        .collect();
    for (position, values) in cells.into_iter().enumerate() {
        deadline.check()?;
        let original = order[position];
        names.push(pivot_column.cell(representatives[original]).text());
        let mut column = column_of(dtype, values);
        if let Some(fill) = spec.fill {
            column = fill_nulls(column, fill);
        }
        columns.push(column);
    }
    let result = Table {
        names: unique_names(names),
        columns,
        rows: groups.len(),
    };
    if result.cell_count() > limits.max_cells {
        return Err(DataError::Limit(
            "The result is over the cell limit.".into(),
        ));
    }
    Ok(result)
}

fn fill_nulls(column: Column, fill: f64) -> Column {
    match column {
        Column::Int(values) if fill.fract() == 0.0 && fill.abs() < 9e15 => Column::Int(
            values
                .into_iter()
                .map(|v| v.or(Some(fill as i64)))
                .collect(),
        ),
        Column::Int(values) => Column::Float(
            values
                .into_iter()
                .map(|v| v.map(|i| i as f64).or(Some(fill)))
                .collect(),
        ),
        Column::Float(values) => {
            Column::Float(values.into_iter().map(|v| v.or(Some(fill))).collect())
        }
        other => other,
    }
}

// ---------------------------------------------------------------------------
// Join
// ---------------------------------------------------------------------------

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum How {
    Inner,
    Left,
    Right,
    Outer,
}

impl How {
    pub(crate) fn parse(name: &str) -> DataResult<Self> {
        Ok(match name.trim().to_ascii_lowercase().as_str() {
            "inner" => Self::Inner,
            "left" => Self::Left,
            "right" => Self::Right,
            "outer" | "full" => Self::Outer,
            other => {
                return Err(argument(format!(
                    "`how` must be inner, left, right or outer, not `{other}`."
                )));
            }
        })
    }
}

#[derive(PartialEq, Eq, Clone, Copy)]
enum KeyClass {
    Number,
    Bool,
    Text,
    Time,
}

fn key_class(dtype: DType) -> KeyClass {
    match dtype {
        DType::Int | DType::Float => KeyClass::Number,
        DType::Bool => KeyClass::Bool,
        DType::Text => KeyClass::Text,
        DType::Date | DType::DateTime => KeyClass::Time,
    }
}

pub(crate) struct JoinSpec<'a> {
    pub(crate) left_keys: &'a [usize],
    pub(crate) right_keys: &'a [usize],
    pub(crate) how: How,
    pub(crate) suffixes: (&'a str, &'a str),
    /// Keys named alike on both sides are one output column.
    pub(crate) merge_keys: bool,
}

fn row_key(table: &Table, keys: &[usize], row: usize) -> Option<Vec<Key>> {
    keys.iter()
        .map(|k| Key::of(&table.columns[*k], row))
        .collect()
}

pub(crate) fn join(
    left: &Table,
    right: &Table,
    spec: &JoinSpec<'_>,
    limits: &Limits,
    deadline: &Deadline,
) -> DataResult<Table> {
    for (l, r) in spec.left_keys.iter().zip(spec.right_keys) {
        let (lt, rt) = (left.columns[*l].dtype(), right.columns[*r].dtype());
        if key_class(lt) != key_class(rt) {
            return Err(argument(format!(
                "Cannot join `{}` ({}) to `{}` ({}): the key columns must hold the same kind of value.",
                left.names[*l],
                lt.name(),
                right.names[*r],
                rt.name()
            )));
        }
    }
    let mut pairs: Vec<(Option<usize>, Option<usize>)> = Vec::new();
    let width = left.width() + right.width();
    let check = |pairs: &Vec<(Option<usize>, Option<usize>)>| -> DataResult<()> {
        if pairs.len() > limits.max_rows || pairs.len().saturating_mul(width) > limits.max_cells {
            return Err(DataError::Limit(format!(
                "The join would produce more than {} rows or {} cells (many rows share a key). Filter either file or aggregate first.",
                limits.max_rows, limits.max_cells
            )));
        }
        Ok(())
    };
    // Index the side we probe into.
    let (probe, build, build_keys, probe_keys) = if spec.how == How::Right {
        (right, left, spec.left_keys, spec.right_keys)
    } else {
        (left, right, spec.right_keys, spec.left_keys)
    };
    let mut index: HashMap<Vec<Key>, Vec<usize>> = HashMap::new();
    for row in 0..build.rows {
        if row % CHECK_EVERY == 0 {
            deadline.check()?;
        }
        if let Some(key) = row_key(build, build_keys, row) {
            index.entry(key).or_default().push(row);
        }
    }
    let mut matched_build = vec![false; build.rows];
    for row in 0..probe.rows {
        if row % CHECK_EVERY == 0 {
            deadline.check()?;
        }
        let hits = row_key(probe, probe_keys, row).and_then(|k| index.get(&k));
        match hits {
            Some(rows) => {
                for b in rows {
                    matched_build[*b] = true;
                    pairs.push(if spec.how == How::Right {
                        (Some(*b), Some(row))
                    } else {
                        (Some(row), Some(*b))
                    });
                }
            }
            None if matches!(spec.how, How::Left | How::Right | How::Outer) => {
                pairs.push(if spec.how == How::Right {
                    (None, Some(row))
                } else {
                    (Some(row), None)
                });
            }
            None => {}
        }
        check(&pairs)?;
    }
    if spec.how == How::Outer {
        for (b, seen) in matched_build.iter().enumerate() {
            if !seen {
                pairs.push((None, Some(b)));
            }
        }
        check(&pairs)?;
    }
    let lefts: Vec<Option<usize>> = pairs.iter().map(|p| p.0).collect();
    let rights: Vec<Option<usize>> = pairs.iter().map(|p| p.1).collect();
    // Same-named keys with the same type are one column; the rest are kept.
    let merged: Vec<(usize, usize)> = if spec.merge_keys {
        spec.left_keys
            .iter()
            .zip(spec.right_keys)
            .filter(|(l, r)| {
                left.names[**l] == right.names[**r]
                    && left.columns[**l].dtype() == right.columns[**r].dtype()
            })
            .map(|(l, r)| (*l, *r))
            .collect()
    } else {
        Vec::new()
    };
    let dropped: Vec<usize> = merged.iter().map(|(_, r)| *r).collect();
    let right_kept: Vec<usize> = (0..right.width())
        .filter(|c| !dropped.contains(c))
        .collect();
    let overlap = |name: &str, others: &Vec<&String>| others.iter().any(|o| o.as_str() == name);
    let right_names: Vec<&String> = right_kept.iter().map(|c| &right.names[*c]).collect();
    let left_names: Vec<&String> = left.names.iter().collect();
    let mut names = Vec::new();
    let mut columns = Vec::new();
    for (c, column) in left.columns.iter().enumerate() {
        let name = &left.names[c];
        names.push(if overlap(name, &right_names) {
            format!("{name}{}", spec.suffixes.0)
        } else {
            name.clone()
        });
        match merged.iter().find(|(l, _)| *l == c) {
            Some((_, r)) => {
                let rcol = &right.columns[*r];
                let both: Vec<Option<usize>> = lefts.clone();
                let mut gathered = column.gather(&both);
                // Rows with no left side take the key from the right.
                let from_right = rcol.gather(&rights);
                gathered = coalesce(gathered, &from_right);
                columns.push(gathered);
            }
            None => columns.push(column.gather(&lefts)),
        }
    }
    for c in right_kept {
        let name = &right.names[c];
        names.push(if overlap(name, &left_names) {
            format!("{name}{}", spec.suffixes.1)
        } else {
            name.clone()
        });
        columns.push(right.columns[c].gather(&rights));
    }
    Ok(Table {
        names: unique_names(names),
        columns,
        rows: pairs.len(),
    })
}

/// `primary`, with its nulls taken from `fallback` (same type).
fn coalesce(primary: Column, fallback: &Column) -> Column {
    macro_rules! merge {
        ($variant:ident, $a:expr, $b:expr) => {
            Column::$variant(
                $a.into_iter()
                    .zip($b.iter())
                    .map(|(x, y)| x.or_else(|| y.clone()))
                    .collect(),
            )
        };
    }
    match (primary, fallback) {
        (Column::Int(a), Column::Int(b)) => merge!(Int, a, b),
        (Column::Float(a), Column::Float(b)) => merge!(Float, a, b),
        (Column::Bool(a), Column::Bool(b)) => merge!(Bool, a, b),
        (Column::Text(a), Column::Text(b)) => merge!(Text, a, b),
        (Column::Date(a), Column::Date(b)) => merge!(Date, a, b),
        (Column::DateTime(a), Column::DateTime(b)) => merge!(DateTime, a, b),
        (other, _) => other,
    }
}

// ---------------------------------------------------------------------------
// Describe
// ---------------------------------------------------------------------------

/// One column's profile.
pub(crate) fn describe_column(name: &str, column: &Column) -> Value {
    let rows = column.len();
    let present: Vec<usize> = (0..rows).filter(|r| !column.is_null(*r)).collect();
    let mut unique: HashSet<Key> = HashSet::new();
    for r in &present {
        if let Some(k) = Key::of(column, *r) {
            unique.insert(k);
        }
    }
    let mut profile = Map::new();
    profile.insert("name".into(), json!(name));
    profile.insert("type".into(), json!(column.dtype().name()));
    profile.insert("count".into(), json!(present.len()));
    profile.insert("nulls".into(), json!(rows - present.len()));
    profile.insert("unique".into(), json!(unique.len()));
    match column.dtype() {
        DType::Int | DType::Float => {
            let mut values: Vec<f64> = present
                .iter()
                .filter_map(|r| column.cell(*r).as_f64())
                .collect();
            if !values.is_empty() {
                values.sort_by(f64::total_cmp);
                let n = values.len() as f64;
                let mean = values.iter().sum::<f64>() / n;
                profile.insert("mean".into(), json!(mean));
                if values.len() > 1 {
                    let var = values.iter().map(|v| (v - mean).powi(2)).sum::<f64>() / (n - 1.0);
                    profile.insert("std".into(), json!(var.sqrt()));
                }
                profile.insert("min".into(), json!(values[0]));
                profile.insert("25%".into(), json!(quantile(&values, 0.25)));
                profile.insert("50%".into(), json!(quantile(&values, 0.5)));
                profile.insert("75%".into(), json!(quantile(&values, 0.75)));
                profile.insert("max".into(), json!(values[values.len() - 1]));
                profile.insert("sum".into(), json!(values.iter().sum::<f64>()));
            }
        }
        DType::Bool => {
            let trues = present
                .iter()
                .filter(|r| matches!(column.cell(**r), Cell::Bool(true)))
                .count();
            profile.insert("true".into(), json!(trues));
            profile.insert("false".into(), json!(present.len() - trues));
        }
        DType::Date | DType::DateTime => {
            let ordered = |max: bool| {
                present
                    .iter()
                    .copied()
                    .reduce(|a, b| {
                        let ord = compare_rows(column, a, b, false);
                        if (ord == Ordering::Less) != max { a } else { b }
                    })
                    .map(|r| column.cell(r).json())
            };
            if let Some(min) = ordered(false) {
                profile.insert("min".into(), min);
            }
            if let Some(max) = ordered(true) {
                profile.insert("max".into(), max);
            }
        }
        DType::Text => {
            let mut counts: HashMap<&str, usize> = HashMap::new();
            let (mut shortest, mut longest) = (usize::MAX, 0usize);
            for r in &present {
                if let Cell::Text(t) = column.cell(*r) {
                    *counts.entry(t).or_default() += 1;
                    let length = t.chars().count();
                    shortest = shortest.min(length);
                    longest = longest.max(length);
                }
            }
            // The most frequent value; ties go to the smallest text.
            let top = counts
                .iter()
                .max_by(|a, b| a.1.cmp(b.1).then_with(|| b.0.cmp(a.0)));
            if let Some((text, count)) = top {
                profile.insert("top".into(), json!(clip(text, 200)));
                profile.insert("top_count".into(), json!(count));
                profile.insert("min_length".into(), json!(shortest));
                profile.insert("max_length".into(), json!(longest));
            }
        }
    }
    Value::Object(profile)
}

pub(crate) fn clip(text: &str, max_chars: usize) -> String {
    if text.chars().count() <= max_chars {
        text.to_owned()
    } else {
        let cut: String = text.chars().take(max_chars).collect();
        format!("{cut}...")
    }
}

// ---------------------------------------------------------------------------
// Correlation
// ---------------------------------------------------------------------------

pub(crate) fn correlate(
    table: &Table,
    columns: &[usize],
    spearman: bool,
    deadline: &Deadline,
) -> DataResult<Table> {
    let n = columns.len();
    let mut matrix = vec![vec![None::<f64>; n]; n];
    for i in 0..n {
        for j in i..n {
            deadline.check()?;
            let (a, b) = (&table.columns[columns[i]], &table.columns[columns[j]]);
            let (mut xs, mut ys) = (Vec::new(), Vec::new());
            for r in 0..table.rows {
                if let (Some(x), Some(y)) = (a.cell(r).as_f64(), b.cell(r).as_f64()) {
                    xs.push(x);
                    ys.push(y);
                }
            }
            if spearman {
                xs = ranks(&xs);
                ys = ranks(&ys);
            }
            let value = pearson(&xs, &ys);
            matrix[i][j] = value;
            matrix[j][i] = value;
        }
    }
    let mut names = vec!["column".to_owned()];
    names.extend(columns.iter().map(|c| table.names[*c].clone()));
    let mut out = vec![Column::text_from(
        columns.iter().map(|c| Some(table.names[*c].clone())),
    )];
    for j in 0..n {
        out.push(Column::Float((0..n).map(|i| matrix[i][j]).collect()));
    }
    Ok(Table {
        names: unique_names(names),
        columns: out,
        rows: n,
    })
}

fn pearson(xs: &[f64], ys: &[f64]) -> Option<f64> {
    let n = xs.len();
    if n < 2 {
        return None;
    }
    let (mx, my) = (
        xs.iter().sum::<f64>() / n as f64,
        ys.iter().sum::<f64>() / n as f64,
    );
    let (mut sxy, mut sxx, mut syy) = (0.0, 0.0, 0.0);
    for (x, y) in xs.iter().zip(ys) {
        sxy += (x - mx) * (y - my);
        sxx += (x - mx).powi(2);
        syy += (y - my).powi(2);
    }
    if sxx == 0.0 || syy == 0.0 {
        return None;
    }
    Some((sxy / (sxx * syy).sqrt()).clamp(-1.0, 1.0))
}

/// Average ranks, ties sharing their mean rank.
fn ranks(values: &[f64]) -> Vec<f64> {
    let mut order: Vec<usize> = (0..values.len()).collect();
    order.sort_by(|a, b| values[*a].total_cmp(&values[*b]));
    let mut out = vec![0.0; values.len()];
    let mut i = 0;
    while i < order.len() {
        let mut j = i;
        while j + 1 < order.len() && values[order[j + 1]] == values[order[i]] {
            j += 1;
        }
        let rank = (i + j) as f64 / 2.0 + 1.0;
        for k in i..=j {
            out[order[k]] = rank;
        }
        i = j + 1;
    }
    out
}

/// A cell as the plain text a chart label or a note shows.
pub(crate) fn label(column: &Column, row: usize) -> String {
    match column.cell(row) {
        Cell::Date(v) => format_time(v, true),
        other => other.text(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::toolkits::families::data_analysis::table::Raw;

    fn text_col(values: &[&str]) -> Column {
        Column::from_raw(values.iter().map(|s| Raw::Text((*s).into())).collect())
    }

    fn sample() -> Table {
        Table::new(
            vec![
                "region".into(),
                "units".into(),
                "price".into(),
                "day".into(),
            ],
            vec![
                text_col(&["north", "south", "north", "east", "south", ""]),
                text_col(&["10", "5", "7", "3", "", "8"]),
                text_col(&["2.5", "4.0", "2.5", "10.0", "4.0", "1.5"]),
                text_col(&[
                    "2024-01-01",
                    "2024-01-02",
                    "2024-01-03",
                    "2024-01-04",
                    "2024-01-05",
                    "2024-01-06",
                ]),
            ],
        )
    }

    fn clock() -> Deadline {
        Deadline::after(std::time::Duration::from_secs(5))
    }

    #[test]
    fn filter_compares_by_column_type_and_skips_nulls() {
        let table = sample();
        let args = json!({"filters": [{"column": "units", "operator": "gt", "value": "5"}]});
        let spec = parse_filter(&args).unwrap().unwrap();
        assert_eq!(matching_rows(&table, &spec, &clock()).unwrap(), [0, 2, 5]);
        let args = json!({"filters": [{"column": "day", "operator": "ge", "value": "2024-01-05"}]});
        let spec = parse_filter(&args).unwrap().unwrap();
        assert_eq!(matching_rows(&table, &spec, &clock()).unwrap(), [4, 5]);
        let args = json!({"filters": [
            {"column": "region", "operator": "in", "value": ["north", "east"]},
            {"column": "price", "operator": "lt", "value": 5}], "match": "all"});
        let spec = parse_filter(&args).unwrap().unwrap();
        assert_eq!(matching_rows(&table, &spec, &clock()).unwrap(), [0, 2]);
        let args = json!({"filters": [{"column": "region", "operator": "is_null"}]});
        let spec = parse_filter(&args).unwrap().unwrap();
        assert_eq!(matching_rows(&table, &spec, &clock()).unwrap(), [5]);
        let args = json!({"filters": [{"column": "units", "operator": "gt", "value": "abc"}]});
        let spec = parse_filter(&args).unwrap().unwrap();
        assert!(matching_rows(&table, &spec, &clock()).is_err());
    }

    #[test]
    fn case_insensitive_text_and_regex() {
        let table = sample();
        let args = json!({"case_sensitive": false,
            "filters": [{"column": "region", "operator": "eq", "value": "NORTH"}]});
        let spec = parse_filter(&args).unwrap().unwrap();
        assert_eq!(matching_rows(&table, &spec, &clock()).unwrap(), [0, 2]);
        let args =
            json!({"filters": [{"column": "region", "operator": "matches", "value": "^(so|ea)"}]});
        let spec = parse_filter(&args).unwrap().unwrap();
        assert_eq!(matching_rows(&table, &spec, &clock()).unwrap(), [1, 3, 4]);
        let args = json!({"filters": [{"column": "region", "operator": "matches", "value": "("}]});
        let spec = parse_filter(&args).unwrap().unwrap();
        assert!(matching_rows(&table, &spec, &clock()).is_err());
    }

    /// A million distinct groups: the result's size is refused from the group
    /// count, before any result column exists.
    fn million_groups() -> Table {
        let n = 1_000_000usize;
        Table::new(
            vec!["id".into(), "kind".into(), "v".into()],
            vec![
                Column::Int((0..n as i64).map(Some).collect()),
                Column::Int((0..n as i64).map(|i| Some(i % 6)).collect()),
                Column::Float((0..n).map(|i| Some(i as f64)).collect()),
            ],
        )
    }

    #[test]
    fn a_result_over_the_cell_limit_is_refused_before_it_is_built() {
        let table = million_groups();
        let specs: Vec<AggSpec> = (0..6)
            .map(|i| AggSpec {
                column: Some(2),
                func: Func::Sum,
                name: format!("s{i}"),
            })
            .collect();
        // 1,000,000 groups x (1 + 6) columns = 7,000,000 > 5,000,000.
        let e = aggregate(&table, &[0], &specs, &Limits::default(), &clock()).unwrap_err();
        assert!(e.to_string().contains("1000000 rows by 7 columns"), "{e}");
        let e = pivot(
            &table,
            &PivotSpec {
                index: &[0],
                columns: 1,
                values: 2,
                func: Func::Sum,
                fill: Some(0.0),
            },
            &Limits::default(),
            &clock(),
        )
        .unwrap_err();
        assert!(e.to_string().contains("1000000 rows by 7 columns"), "{e}");
        // Under the limit the same shapes still work.
        let ok = aggregate(&table, &[0], &specs[..2], &Limits::default(), &clock()).unwrap();
        assert_eq!(ok.rows, 1_000_000);
    }

    #[test]
    fn in_is_a_hash_lookup_across_numeric_kinds_and_case() {
        let table = sample();
        let by = |column: &str, op: &str, value: Value, extra: Value| {
            let mut args = json!({"filters": [{"column": column, "operator": op, "value": value}]});
            args.as_object_mut()
                .unwrap()
                .extend(extra.as_object().unwrap().clone());
            let spec = parse_filter(&args).unwrap().unwrap();
            matching_rows(&table, &spec, &clock()).unwrap()
        };
        // An integer column matches 5.0 and "10" as it matches 5 and 10.
        assert_eq!(by("units", "in", json!([5.0, "10"]), json!({})), [0, 1]);
        assert_eq!(by("units", "not_in", json!([5, 10]), json!({})), [2, 3, 5]);
        assert_eq!(
            by(
                "region",
                "in",
                json!(["NORTH", "East"]),
                json!({"case_sensitive": false})
            ),
            [0, 2, 3]
        );
        assert_eq!(
            by("region", "in", json!(["NORTH"]), json!({})),
            Vec::<usize>::new()
        );
        assert_eq!(by("day", "in", json!(["2024-01-02"]), json!({})), [1]);
        let spec = parse_filter(
            &json!({"filters": [{"column": "units", "operator": "in", "value": ["x"]}]}),
        )
        .unwrap()
        .unwrap();
        assert!(matching_rows(&table, &spec, &clock()).is_err());
        let spec =
            parse_filter(&json!({"filters": [{"column": "units", "operator": "in", "value": 5}]}))
                .unwrap()
                .unwrap();
        assert!(matching_rows(&table, &spec, &clock()).is_err());
    }

    #[test]
    fn sort_is_stable_with_nulls_last_both_ways() {
        let table = sample();
        let asc = sorted_rows(&table, &[(1, false)], &clock()).unwrap();
        assert_eq!(asc, [3, 1, 2, 5, 0, 4]);
        let desc = sorted_rows(&table, &[(1, true)], &clock()).unwrap();
        assert_eq!(desc, [0, 5, 2, 1, 3, 4]);
    }

    #[test]
    fn aggregate_groups_sorted_dropping_null_keys() {
        let table = sample();
        let args = json!({"aggregations": [
            {"column": "units", "function": "sum"},
            {"column": "price", "function": "mean", "name": "avg_price"},
            {"column": "*", "function": "count", "name": "rows"}]});
        let specs = parse_aggregations(&table, &args).unwrap();
        let out = aggregate(&table, &[0], &specs, &Limits::default(), &clock()).unwrap();
        assert_eq!(out.names, ["region", "units_sum", "avg_price", "rows"]);
        assert_eq!(out.rows, 3);
        assert_eq!(out.columns[0].cell(0).text(), "east");
        assert_eq!(out.columns[1].cell(1).text(), "17");
        assert_eq!(out.columns[2].cell(2).text(), "4.0");
        assert_eq!(out.columns[3].cell(2).text(), "2");
        let whole = aggregate(&table, &[], &specs, &Limits::default(), &clock()).unwrap();
        assert_eq!(whole.rows, 1);
        assert_eq!(whole.columns[0].cell(0).text(), "33");
    }

    #[test]
    fn std_and_median_match_the_sample_definitions() {
        let col = text_col(&["2", "4", "4", "4", "5", "5", "7", "9"]);
        let rows: Vec<usize> = (0..8).collect();
        let Val::Float(std) = aggregate_rows(&col, &rows, Func::Std).unwrap() else {
            panic!()
        };
        assert!((std - 2.138_089_935_299_395).abs() < 1e-12);
        let Val::Float(median) = aggregate_rows(&col, &rows, Func::Median).unwrap() else {
            panic!()
        };
        assert!((median - 4.5).abs() < 1e-12);
    }

    #[test]
    fn text_columns_refuse_arithmetic_with_a_sentence() {
        let table = sample();
        let args = json!({"aggregations": [{"column": "region", "function": "mean"}]});
        let Err(e) = parse_aggregations(&table, &args) else {
            panic!()
        };
        assert!(
            e.to_string()
                .contains("Cannot compute mean of the text column `region`")
        );
    }

    #[test]
    fn pivot_spreads_values_and_fills() {
        let table = sample();
        let spec = PivotSpec {
            index: &[0],
            columns: 3,
            values: 1,
            func: Func::Sum,
            fill: Some(0.0),
        };
        let wide = Table::new(
            vec![
                "region".into(),
                "tier".into(),
                "units".into(),
                "price".into(),
            ],
            vec![
                text_col(&["n", "n", "s", "s"]),
                text_col(&["a", "b", "a", "a"]),
                text_col(&["1", "2", "3", "4"]),
                text_col(&["1", "1", "1", "1"]),
            ],
        );
        let out = pivot(
            &wide,
            &PivotSpec {
                index: &[0],
                columns: 1,
                values: 2,
                func: Func::Sum,
                fill: Some(0.0),
            },
            &Limits::default(),
            &clock(),
        )
        .unwrap();
        assert_eq!(out.names, ["region", "a", "b"]);
        assert_eq!(out.columns[1].cell(1).text(), "7");
        assert_eq!(out.columns[2].cell(1).text(), "0");
        // A wide pivot is refused before it is built.
        let tight = Limits {
            max_pivot_columns: 2,
            ..Limits::default()
        };
        assert!(pivot(&table, &spec, &tight, &clock()).is_err());
    }

    fn pair() -> (Table, Table) {
        (
            Table::new(
                vec!["id".into(), "name".into()],
                vec![text_col(&["1", "2", "3"]), text_col(&["a", "b", "c"])],
            ),
            Table::new(
                vec!["id".into(), "name".into(), "score".into()],
                vec![
                    text_col(&["2", "3", "3", "4"]),
                    text_col(&["B", "C", "C2", "D"]),
                    text_col(&["20", "30", "31", "40"]),
                ],
            ),
        )
    }

    #[test]
    fn join_kinds_and_suffixes() {
        let (l, r) = pair();
        let run = |how| {
            join(
                &l,
                &r,
                &JoinSpec {
                    left_keys: &[0],
                    right_keys: &[0],
                    how,
                    suffixes: ("_x", "_y"),
                    merge_keys: true,
                },
                &Limits::default(),
                &clock(),
            )
            .unwrap()
        };
        let inner = run(How::Inner);
        assert_eq!(inner.names, ["id", "name_x", "name_y", "score"]);
        assert_eq!(inner.rows, 3);
        assert_eq!(run(How::Left).rows, 4);
        assert_eq!(run(How::Right).rows, 4);
        let outer = run(How::Outer);
        assert_eq!(outer.rows, 5);
        assert_eq!(outer.columns[0].cell(4).text(), "4");
    }

    #[test]
    fn join_refuses_mismatched_keys_and_exploding_output() {
        let (l, r) = pair();
        let bad = join(
            &l,
            &r,
            &JoinSpec {
                left_keys: &[0],
                right_keys: &[1],
                how: How::Inner,
                suffixes: ("_x", "_y"),
                merge_keys: false,
            },
            &Limits::default(),
            &clock(),
        );
        assert!(bad.unwrap_err().to_string().contains("same kind of value"));
        let same = |n: usize| Table::new(vec!["k".into()], vec![text_col(&vec!["1"; n])]);
        let tight = Limits {
            max_rows: 50,
            ..Limits::default()
        };
        let boom = join(
            &same(10),
            &same(10),
            &JoinSpec {
                left_keys: &[0],
                right_keys: &[0],
                how: How::Inner,
                suffixes: ("_x", "_y"),
                merge_keys: true,
            },
            &tight,
            &clock(),
        );
        assert!(
            boom.unwrap_err()
                .to_string()
                .contains("many rows share a key")
        );
    }

    #[test]
    fn correlation_is_perfect_for_a_line() {
        let t = Table::new(
            vec!["a".into(), "b".into(), "c".into()],
            vec![
                text_col(&["1", "2", "3", "4"]),
                text_col(&["2", "4", "6", "8"]),
                text_col(&["4", "3", "2", "1"]),
            ],
        );
        let out = correlate(&t, &[0, 1, 2], false, &clock()).unwrap();
        assert_eq!(out.columns[2].cell(0).text(), "1.0");
        assert_eq!(out.columns[3].cell(0).text(), "-1.0");
        let sp = correlate(&t, &[0, 1], true, &clock()).unwrap();
        assert_eq!(sp.columns[2].cell(0).text(), "1.0");
    }

    #[test]
    fn describe_reports_quartiles() {
        let col = text_col(&["1", "2", "3", "4", ""]);
        let d = describe_column("x", &col);
        assert_eq!(d["count"], 4);
        assert_eq!(d["nulls"], 1);
        assert_eq!(d["50%"], 2.5);
        assert_eq!(d["25%"], 1.75);
    }
}
