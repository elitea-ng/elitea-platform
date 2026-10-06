//! Tool arguments, validated before use (ADR-0026 decision 8: an argument
//! a model can steer is checked before it reaches a query).
//!
//! The Python tools validated their arguments with pydantic in its lax
//! mode and `LangGraph`'s `ToolNode` turned a failure into the tool's
//! result:
//!
//! ```text
//! Error invoking tool '{name}' with kwargs {repr(args)} with error:
//!  {loc}: {msg}
//!  Please fix the error and try again.
//! ```
//!
//! This module reproduces those coercions (`"7"` → 7, `5.0` → 5, `"yes"`
//! → true, unknown keys ignored) and messages. The RANGES the tools then
//! apply (a `k` of a million, a negative `max_lines`) are each tool's.

use serde_json::{Map, Value};

/// One parameter's type.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    Str,
    Int,
    Bool,
    /// `Optional[str]`.
    OptStr,
    /// `Optional[int]`, and `> 0` when `positive`.
    OptInt {
        positive: bool,
    },
    /// `Literal[...]`.
    Enum(&'static [&'static str]),
    /// `list[Todo]`.
    Todos,
}

/// One parameter.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Param {
    pub name: &'static str,
    pub kind: Kind,
    /// `None` when required.
    pub default: Option<ParamDefault>,
}

/// A parameter's default.
#[derive(Debug, Clone, Copy, PartialEq)]
pub enum ParamDefault {
    Str(&'static str),
    Int(i64),
    Bool(bool),
    Null,
}

impl ParamDefault {
    fn value(self) -> Value {
        match self {
            Self::Str(text) => Value::from(text),
            Self::Int(number) => Value::from(number),
            Self::Bool(flag) => Value::from(flag),
            Self::Null => Value::Null,
        }
    }
}

const fn required(name: &'static str, kind: Kind) -> Param {
    Param {
        name,
        kind,
        default: None,
    }
}

const fn optional(name: &'static str, kind: Kind, default: ParamDefault) -> Param {
    Param {
        name,
        kind,
        default: Some(default),
    }
}

const STR: Kind = Kind::Str;
const INT: Kind = Kind::Int;

/// The parameters of each tool, in schema order.
#[must_use]
pub fn params(tool: &str) -> Option<&'static [Param]> {
    const SEARCH_SYMBOLS: &[Param] = &[
        required("query", STR),
        optional("k", INT, ParamDefault::Int(20)),
        optional("symbol_type", STR, ParamDefault::Str("")),
        optional("file_prefix", STR, ParamDefault::Str("")),
    ];
    const GET_RELATIONSHIPS: &[Param] = &[
        required("symbol_name", STR),
        optional("direction", STR, ParamDefault::Str("both")),
        optional("max_depth", INT, ParamDefault::Int(2)),
    ];
    const GET_CODE: &[Param] = &[
        required("symbol_name", STR),
        optional("max_lines", INT, ParamDefault::Int(200)),
    ];
    const SEARCH_DOCS: &[Param] = &[
        required("query", STR),
        optional("k", INT, ParamDefault::Int(5)),
    ];
    const QUERY_GRAPH: &[Param] = &[required("expression", STR)];
    const THINK: &[Param] = &[required("reflection", STR)];
    const SEARCH_CODEBASE: &[Param] = &[
        required("query", STR),
        optional("k", INT, ParamDefault::Int(10)),
    ];
    const SYMBOL_RELATIONSHIPS: &[Param] = &[
        required("symbol_name", STR),
        optional("max_depth", INT, ParamDefault::Int(2)),
    ];
    const SEARCH_GRAPH: &[Param] = &[
        required("query", STR),
        optional("k", INT, ParamDefault::Int(5)),
        optional("include_neighbors", Kind::Bool, ParamDefault::Bool(true)),
    ];
    const LS: &[Param] = &[required("path", STR)];
    const READ_FILE: &[Param] = &[
        required("file_path", STR),
        optional("offset", INT, ParamDefault::Int(0)),
        optional("limit", INT, ParamDefault::Int(100)),
    ];
    const WRITE_FILE: &[Param] = &[required("file_path", STR), required("content", STR)];
    const EDIT_FILE: &[Param] = &[
        required("file_path", STR),
        required("old_string", STR),
        required("new_string", STR),
        optional("replace_all", Kind::Bool, ParamDefault::Bool(false)),
    ];
    const DELETE: &[Param] = &[required("file_path", STR)];
    const GLOB: &[Param] = &[
        required("pattern", STR),
        optional("path", Kind::OptStr, ParamDefault::Null),
    ];
    const GREP: &[Param] = &[
        required("pattern", STR),
        optional("path", Kind::OptStr, ParamDefault::Null),
        optional("glob", Kind::OptStr, ParamDefault::Null),
        optional(
            "output_mode",
            Kind::Enum(&["files_with_matches", "content", "count"]),
            ParamDefault::Str("files_with_matches"),
        ),
        optional(
            "max_count",
            Kind::OptInt { positive: true },
            ParamDefault::Null,
        ),
    ];
    const EXECUTE: &[Param] = &[
        required("command", STR),
        optional(
            "timeout",
            Kind::OptInt { positive: false },
            ParamDefault::Null,
        ),
    ];
    const WRITE_TODOS: &[Param] = &[required("todos", Kind::Todos)];
    Some(match tool {
        "search_symbols" => SEARCH_SYMBOLS,
        "get_relationships_tool" => GET_RELATIONSHIPS,
        "get_code" => GET_CODE,
        "search_docs" => SEARCH_DOCS,
        "query_graph" => QUERY_GRAPH,
        "think" => THINK,
        "search_codebase" => SEARCH_CODEBASE,
        "get_symbol_relationships" => SYMBOL_RELATIONSHIPS,
        "search_graph" => SEARCH_GRAPH,
        "ls" => LS,
        "read_file" => READ_FILE,
        "write_file" => WRITE_FILE,
        "edit_file" => EDIT_FILE,
        "delete" => DELETE,
        "glob" => GLOB,
        "grep" => GREP,
        "execute" => EXECUTE,
        "write_todos" => WRITE_TODOS,
        _ => return None,
    })
}

/// Validated arguments: every parameter present, coerced.
#[derive(Debug, Clone, PartialEq)]
pub struct Args(Map<String, Value>);

impl Args {
    #[must_use]
    pub fn str(&self, name: &str) -> &str {
        self.0.get(name).and_then(Value::as_str).unwrap_or_default()
    }

    #[must_use]
    pub fn opt_str(&self, name: &str) -> Option<&str> {
        self.0.get(name).and_then(Value::as_str)
    }

    /// An integer, saturated to `i64` (pydantic accepted any size).
    #[must_use]
    pub fn int(&self, name: &str) -> i64 {
        self.opt_int(name).unwrap_or(0)
    }

    #[must_use]
    pub fn opt_int(&self, name: &str) -> Option<i64> {
        self.0.get(name).and_then(|v| {
            v.as_i64()
                .or_else(|| v.as_u64().map(|_| i64::MAX))
                .or_else(|| v.as_f64().map(saturate))
        })
    }

    #[must_use]
    pub fn bool(&self, name: &str) -> bool {
        self.0.get(name).and_then(Value::as_bool).unwrap_or(false)
    }

    #[must_use]
    pub fn value(&self, name: &str) -> &Value {
        self.0.get(name).unwrap_or(&Value::Null)
    }
}

#[allow(clippy::cast_possible_truncation)]
fn saturate(value: f64) -> i64 {
    // `as` saturates at the bounds; NaN is 0. The value is integral here.
    value as i64
}

/// A failed validation: the `ToolNode` message.
#[must_use]
pub fn invocation_error(
    tool: &str,
    raw: &Map<String, Value>,
    errors: &[(String, String)],
) -> String {
    let detail: Vec<String> = errors
        .iter()
        .map(|(loc, msg)| format!("{loc}: {msg}"))
        .collect();
    format!(
        "Error invoking tool '{tool}' with kwargs {} with error:\n {}\n Please fix the error and try again.",
        super::pyfmt::repr(&Value::Object(raw.clone())),
        detail.join("\n")
    )
}

fn coerce_int(value: &Value) -> Result<Value, &'static str> {
    const INVALID: &str = "Input should be a valid integer";
    match value {
        Value::Bool(flag) => Ok(Value::from(i64::from(*flag))),
        Value::Number(number) => {
            if number.is_i64() || number.is_u64() {
                return Ok(value.clone());
            }
            let float = number.as_f64().unwrap_or(f64::NAN);
            if float.is_finite() && float.fract() == 0.0 {
                Ok(Value::from(saturate(float)))
            } else if float.is_finite() {
                Err("Input should be a valid integer, got a number with a fractional part")
            } else {
                Err(INVALID)
            }
        }
        Value::String(text) => {
            let trimmed = crate::graph::pystr::strip(text);
            let digits = trimmed.strip_prefix(['-', '+']).unwrap_or(trimmed);
            if digits.is_empty() || !digits.bytes().all(|b| b.is_ascii_digit()) {
                return Err(
                    "Input should be a valid integer, unable to parse string as an integer",
                );
            }
            let negative = trimmed.starts_with('-');
            Ok(Value::from(match digits.parse::<i64>() {
                Ok(n) if negative => -n,
                Ok(n) => n,
                Err(_) if negative => i64::MIN,
                Err(_) => i64::MAX,
            }))
        }
        _ => Err(INVALID),
    }
}

fn coerce_bool(value: &Value) -> Result<Value, &'static str> {
    const UNREADABLE: &str = "Input should be a valid boolean, unable to interpret input";
    match value {
        Value::Bool(_) => Ok(value.clone()),
        Value::Number(number) => match (number.as_i64(), number.as_f64()) {
            (Some(0), _) => Ok(Value::Bool(false)),
            (Some(1), _) => Ok(Value::Bool(true)),
            (None, Some(v)) if v.total_cmp(&0.0).is_eq() || v.total_cmp(&-0.0).is_eq() => {
                Ok(Value::Bool(false))
            }
            (None, Some(v)) if v.total_cmp(&1.0).is_eq() => Ok(Value::Bool(true)),
            _ => Err(UNREADABLE),
        },
        Value::String(text) => match text.to_lowercase().as_str() {
            "0" | "off" | "f" | "false" | "n" | "no" => Ok(Value::Bool(false)),
            "1" | "on" | "t" | "true" | "y" | "yes" => Ok(Value::Bool(true)),
            _ => Err(UNREADABLE),
        },
        _ => Err("Input should be a valid boolean"),
    }
}

fn literal_message(choices: &[&str]) -> String {
    let quoted: Vec<String> = choices.iter().map(|c| format!("'{c}'")).collect();
    match quoted.split_last() {
        Some((last, rest)) if !rest.is_empty() => {
            format!("Input should be {} or {last}", rest.join(", "))
        }
        _ => format!("Input should be {}", quoted.join("")),
    }
}

const TODO_STATUSES: &[&str] = &["pending", "in_progress", "completed"];

fn coerce_todos(value: &Value, loc: &str, errors: &mut Vec<(String, String)>) -> Value {
    let Value::Array(items) = value else {
        errors.push((loc.to_owned(), "Input should be a valid list".to_owned()));
        return Value::Null;
    };
    let mut todos = Vec::with_capacity(items.len());
    for (index, item) in items.iter().enumerate() {
        let at = format!("{loc}.{index}");
        let Value::Object(map) = item else {
            errors.push((at, "Input should be a valid dictionary".to_owned()));
            continue;
        };
        let mut todo = Map::new();
        match map.get("content") {
            None => errors.push((format!("{at}.content"), "Field required".to_owned())),
            Some(Value::String(text)) => {
                todo.insert("content".into(), Value::from(text.clone()));
            }
            Some(_) => errors.push((
                format!("{at}.content"),
                "Input should be a valid string".to_owned(),
            )),
        }
        match map.get("status") {
            None => errors.push((format!("{at}.status"), "Field required".to_owned())),
            Some(Value::String(text)) if TODO_STATUSES.contains(&text.as_str()) => {
                todo.insert("status".into(), Value::from(text.clone()));
            }
            Some(_) => errors.push((format!("{at}.status"), literal_message(TODO_STATUSES))),
        }
        todos.push(Value::Object(todo));
    }
    Value::Array(todos)
}

/// Validate `raw` against `params`.
///
/// # Errors
///
/// The `(loc, msg)` pairs pydantic would report, in parameter order.
pub fn validate(params: &[Param], raw: &Map<String, Value>) -> Result<Args, Vec<(String, String)>> {
    let mut out = Map::new();
    let mut errors = Vec::new();
    for param in params {
        let Some(value) = raw.get(param.name) else {
            match param.default {
                Some(default) => {
                    out.insert(param.name.to_owned(), default.value());
                }
                None => errors.push((param.name.to_owned(), "Field required".to_owned())),
            }
            continue;
        };
        let coerced = match param.kind {
            Kind::Str => match value {
                Value::String(_) => Ok(value.clone()),
                _ => Err("Input should be a valid string".to_owned()),
            },
            Kind::OptStr => match value {
                Value::String(_) | Value::Null => Ok(value.clone()),
                _ => Err("Input should be a valid string".to_owned()),
            },
            Kind::Int => coerce_int(value).map_err(str::to_owned),
            Kind::OptInt { positive } => {
                if value.is_null() {
                    Ok(Value::Null)
                } else {
                    coerce_int(value).map_err(str::to_owned).and_then(|v| {
                        if positive && v.as_i64().is_some_and(|n| n <= 0) {
                            Err("Input should be greater than 0".to_owned())
                        } else {
                            Ok(v)
                        }
                    })
                }
            }
            Kind::Bool => coerce_bool(value).map_err(str::to_owned),
            Kind::Enum(choices) => match value {
                Value::String(text) if choices.contains(&text.as_str()) => Ok(value.clone()),
                _ => Err(literal_message(choices)),
            },
            Kind::Todos => Ok(coerce_todos(value, param.name, &mut errors)),
        };
        match coerced {
            Ok(value) => {
                out.insert(param.name.to_owned(), value);
            }
            Err(message) => errors.push((param.name.to_owned(), message)),
        }
    }
    if errors.is_empty() {
        Ok(Args(out))
    } else {
        Err(errors)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn check(tool: &str, raw: Value) -> Result<Args, Vec<(String, String)>> {
        let Value::Object(map) = raw else {
            return Err(Vec::new());
        };
        validate(params(tool).unwrap_or_default(), &map)
    }

    #[test]
    fn pydantic_lax_coercions() {
        let args = check(
            "search_symbols",
            json!({"query": "a", "k": "7", "extra": 1}),
        );
        assert_eq!(args.map(|a| a.int("k")), Ok(7));
        let args = check("search_symbols", json!({"query": "a", "k": 5.0}));
        assert_eq!(args.map(|a| a.int("k")), Ok(5));
        let args = check("search_symbols", json!({"query": "a", "k": true}));
        assert_eq!(args.map(|a| a.int("k")), Ok(1));
        let args = check(
            "search_graph",
            json!({"query": "a", "include_neighbors": "no"}),
        );
        assert_eq!(args.map(|a| a.bool("include_neighbors")), Ok(false));
        let args = check("search_symbols", json!({"query": "a", "k": 1e30}));
        assert_eq!(args.map(|a| a.int("k")), Ok(i64::MAX));
    }

    #[test]
    fn pydantic_messages() {
        let err = |tool, raw| check(tool, raw).err().unwrap_or_default();
        assert_eq!(
            err("search_symbols", json!({"query": 5, "k": 5.5})),
            vec![
                (
                    "query".to_owned(),
                    "Input should be a valid string".to_owned()
                ),
                (
                    "k".to_owned(),
                    "Input should be a valid integer, got a number with a fractional part"
                        .to_owned()
                ),
            ]
        );
        assert_eq!(
            err("read_file", json!({"file_path": "/a.md", "limit": "many"})),
            vec![(
                "limit".to_owned(),
                "Input should be a valid integer, unable to parse string as an integer".to_owned()
            )]
        );
        assert_eq!(
            err(
                "write_todos",
                json!({"todos": [{"content": "A", "status": "bogus"}]})
            ),
            vec![(
                "todos.0.status".to_owned(),
                "Input should be 'pending', 'in_progress' or 'completed'".to_owned()
            )]
        );
        assert_eq!(
            err("grep", json!({"pattern": "x", "max_count": 0})),
            vec![(
                "max_count".to_owned(),
                "Input should be greater than 0".to_owned()
            )]
        );
        assert_eq!(
            err("read_file", json!({})),
            vec![("file_path".to_owned(), "Field required".to_owned())]
        );
        let raw = json!({"todos": [{"content": "A"}]});
        let message = invocation_error(
            "write_todos",
            raw.as_object().unwrap_or(&Map::new()),
            &err("write_todos", raw.clone()),
        );
        assert_eq!(
            message,
            "Error invoking tool 'write_todos' with kwargs {'todos': [{'content': 'A'}]} with error:\n todos.0.status: Field required\n Please fix the error and try again."
        );
    }
}
