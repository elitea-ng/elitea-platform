//! Reading the graph (ADR-0027 P4): the retrieval tools of the
//! `inventory` and `inventory_search` families, over a [`view::GraphView`].
//! Where the view comes from (a store, cached until the graph's revision
//! changes) is the caller's.

pub mod admin_tools;
pub mod community_tools;
pub mod inventory_tools;
pub mod pattern;
pub mod query;
pub mod search_tools;
pub mod semantic;
pub mod view;

use elitea_engine_core::errors::EngineError;
use serde_json::{Map, Value};
use view::GraphView;

/// What every tool handler receives.
#[derive(Debug, Clone, Copy)]
pub struct Call<'a> {
    /// The tool name (as routed: `search_graph`, `get_entity_details`, …).
    pub tool: &'a str,
    /// `inventory` or `inventory_search`.
    pub family: &'a str,
    /// The merged parameters the host forwarded.
    pub params: &'a Map<String, Value>,
    /// The graph, loaded and indexed.
    pub view: &'a GraphView,
}

/// A handler's answer: `Some` when the module serves `call.tool` — the
/// tool result the host composes (`{"success": true, "result": …}`, as
/// the Python sidecar wrapped a handler's return value) or the failure —
/// and `None` when the tool is another module's.
pub type Handled = Option<Result<Value, EngineError>>;

/// The tool result of a handler's text (or JSON text) answer.
#[must_use]
pub fn answer(result: impl Into<String>) -> Value {
    serde_json::json!({"success": true, "result": result.into()})
}

/// A boolean tool parameter, read strictly: JSON `true`, or the string
/// "true" in any case, is on; everything else (absent, `"false"`, `"0"`,
/// `"no"`, numbers) is off. Python truthiness would turn the string
/// "false" ON, which for a destructive flag deletes data.
#[must_use]
pub fn flag(value: Option<&Value>) -> bool {
    match value {
        Some(Value::Bool(on)) => *on,
        Some(Value::String(text)) => text.trim().eq_ignore_ascii_case("true"),
        _ => false,
    }
}

/// A boolean tool parameter, read leniently: JSON `true`, a non-zero
/// number, or the string "true", "1", "yes" or "on" (trimmed, any case) is
/// on; everything else is off. Only for a flag whose ON is safe — the
/// ingestion's `full_rebuild` builds the new graph and swaps it in, so a
/// caller's `1` or `"yes"` must not silently run incrementally. A
/// destructive flag (`replace_ingestion_state`) stays on [`flag`].
#[must_use]
pub fn lenient_flag(value: Option<&Value>) -> bool {
    match value {
        Some(Value::Bool(on)) => *on,
        Some(Value::Number(number)) => number.as_f64().is_some_and(|n| n != 0.0),
        Some(Value::String(text)) => ["true", "1", "yes", "on"]
            .iter()
            .any(|word| text.trim().eq_ignore_ascii_case(word)),
        _ => false,
    }
}

/// Route `call` to the module that serves it.
#[must_use]
pub fn dispatch(call: &Call<'_>) -> Handled {
    inventory_tools::handle(call)
        .or_else(|| search_tools::handle(call))
        .or_else(|| pattern::handle(call))
        .or_else(|| semantic::handle(call))
        .or_else(|| community_tools::handle(call))
        .or_else(|| admin_tools::handle(call))
}
