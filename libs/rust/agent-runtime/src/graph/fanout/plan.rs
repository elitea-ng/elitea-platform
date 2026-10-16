//! Fan-out plan, failure policy and join landing shared by Parallel and Map.
//!
//! The planners (Worker `parallel.rs`, `map_reduce.rs`) decide *what* runs;
//! this module decides how child terminals become the node's output:
//!
//! - **On Error** (binding PLAN §8 second round): `continue` (default) keeps
//!   going and lands a `status: "failed"` entry for a failed child; `stop`
//!   closes admission, drains, and fails the node. If every child fails, the
//!   node fails under either policy.
//! - **Join landing**: the ordered collection lands through the output key's
//!   typed reducer: `append` (default, declared order), `keyed` (object by
//!   child key), or `merge` (one object of every child's outputs; a key two
//!   children write is a typed conflict, never last-write-wins).
//!
//! Error entries carry a stable code, a safe message and a support reference,
//! never raw model or tool text, and are bounded to
//! [`MAX_JOIN_ERROR_ENTRY_BYTES`].

use std::collections::BTreeSet;

use serde_json::{Map, Value, json};

use crate::graph::fanout_budget::{
    MAX_CHILD_RESULT_BYTES, MAX_FANOUT_CHILDREN, MAX_JOIN_ERROR_ENTRY_BYTES,
    MAX_JOINED_RESULT_BYTES,
};
use crate::graph::state_reducers::StateReducer;

/// Which planner owns the activation. It fixes the entry key name.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum FanoutKind {
    Parallel,
    Map,
}

impl FanoutKind {
    /// The join entry field that names the child.
    #[must_use]
    pub const fn key_field(self) -> &'static str {
        match self {
            Self::Parallel => "branch_id",
            Self::Map => "index",
        }
    }
}

/// The node's On Error setting.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum OnError {
    /// Partial results: a failed child becomes a `failed` entry.
    #[default]
    Continue,
    /// Fail after drain: admission closes on the first failure.
    Stop,
}

impl OnError {
    /// Parse the YAML value. `None` is the default, `continue`.
    pub fn parse(value: Option<&str>) -> Result<Self, FanoutPlanError> {
        match value {
            None | Some("continue") => Ok(Self::Continue),
            Some("stop") => Ok(Self::Stop),
            Some(_) => Err(FanoutPlanError::InvalidOnError),
        }
    }
}

/// How the joined collection lands on the output key.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum JoinMode {
    #[default]
    Append,
    Keyed,
    Merge,
}

impl JoinMode {
    pub fn parse(value: Option<&str>) -> Result<Self, FanoutPlanError> {
        match value {
            None | Some("append") => Ok(Self::Append),
            Some("keyed") => Ok(Self::Keyed),
            Some("merge") => Ok(Self::Merge),
            Some(_) => Err(FanoutPlanError::InvalidJoin),
        }
    }

    /// Save-time check of the join against the policy and the output key's
    /// declared reducer. A combination that cannot land deterministically is
    /// refused here, not discovered at run time.
    pub fn check(
        self,
        on_error: OnError,
        reducer: Option<StateReducer>,
    ) -> Result<(), FanoutPlanError> {
        match (self, reducer) {
            (Self::Append, None | Some(StateReducer::Append))
            | (Self::Keyed | Self::Merge, None | Some(StateReducer::Merge)) => {}
            _ => return Err(FanoutPlanError::ReducerMismatch),
        }
        // A merged object has no place for a failed child's entry.
        if self == Self::Merge && on_error == OnError::Continue {
            return Err(FanoutPlanError::MergeNeedsStop);
        }
        Ok(())
    }
}

/// A child's terminal outcome as the planner projected it.
#[derive(Clone, Debug, PartialEq)]
pub enum ChildTerminal {
    Succeeded(Map<String, Value>),
    Failed(ChildFailure),
}

/// A typed, data-free child failure.
#[derive(Clone, Debug, PartialEq)]
pub struct ChildFailure {
    /// Stable dotted code, for example `graph.fanout.may_have_started`.
    pub code: String,
    /// Fixed, safe text. Never model or tool output.
    pub message_safe: String,
    /// Opaque support reference (correlation id), if any.
    pub support_ref: Option<String>,
}

impl ChildFailure {
    #[must_use]
    pub fn new(code: &str, message_safe: &str) -> Self {
        Self {
            code: code.to_owned(),
            message_safe: message_safe.to_owned(),
            support_ref: None,
        }
    }
}

/// One settled child in declared order.
#[derive(Clone, Debug, PartialEq)]
pub struct SettledChild {
    /// Parallel branch id, or the Map item's decimal 0-based index.
    pub key: String,
    /// The node (Parallel) or worker (Map) that ran.
    pub node: String,
    pub terminal: ChildTerminal,
}

/// The node's outcome after every admitted child settled.
#[derive(Clone, Debug, PartialEq)]
pub enum JoinOutcome {
    /// The ordered collection with a status on every entry.
    Joined(Vec<Value>),
    /// Every child failed, or `stop` saw a failure: the node fails with the
    /// first failure in declared order.
    Failed { key: String, failure: ChildFailure },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, thiserror::Error)]
pub enum FanoutPlanError {
    #[error("graph.fanout.invalid_on_error")]
    InvalidOnError,
    #[error("graph.fanout.invalid_join")]
    InvalidJoin,
    #[error("graph.fanout.join_reducer_mismatch")]
    ReducerMismatch,
    #[error("graph.fanout.join_merge_needs_stop")]
    MergeNeedsStop,
    #[error("graph.fanout.join_conflict")]
    JoinConflict,
    #[error("graph.fanout.join_type_mismatch")]
    JoinTypeMismatch,
    #[error("graph.fanout.child_result_too_large")]
    ChildResultTooLarge,
    #[error("graph.fanout.joined_result_too_large")]
    JoinedResultTooLarge,
    #[error("graph.fanout.too_many_children")]
    TooManyChildren,
    #[error("graph.fanout.invalid_child")]
    InvalidChild,
}

impl FanoutPlanError {
    /// The stable code (also the `Display` text): names no value.
    #[must_use]
    pub fn code(self) -> String {
        self.to_string()
    }
}

/// Build the ordered entries for `settled` (already in declared order).
pub fn join(
    kind: FanoutKind,
    on_error: OnError,
    settled: Vec<SettledChild>,
) -> Result<JoinOutcome, FanoutPlanError> {
    if settled.is_empty() {
        return Err(FanoutPlanError::InvalidChild);
    }
    if settled.len() > MAX_FANOUT_CHILDREN {
        return Err(FanoutPlanError::TooManyChildren);
    }
    let mut keys = BTreeSet::new();
    for child in &settled {
        if child.key.is_empty() || !keys.insert(child.key.as_str()) {
            return Err(FanoutPlanError::InvalidChild);
        }
    }
    let first_failure = settled.iter().find_map(|child| match &child.terminal {
        ChildTerminal::Failed(failure) => Some((child.key.clone(), failure.clone())),
        ChildTerminal::Succeeded(_) => None,
    });
    let all_failed = settled
        .iter()
        .all(|child| matches!(child.terminal, ChildTerminal::Failed(_)));
    if let Some((key, failure)) = first_failure
        && (all_failed || on_error == OnError::Stop)
    {
        return Ok(JoinOutcome::Failed { key, failure });
    }
    let mut total = 2_usize;
    let mut entries = Vec::with_capacity(settled.len());
    for child in settled {
        let entry = entry(kind, child)?;
        total = total
            .saturating_add(serialized_len(&entry))
            .saturating_add(1);
        if total > MAX_JOINED_RESULT_BYTES {
            return Err(FanoutPlanError::JoinedResultTooLarge);
        }
        entries.push(entry);
    }
    Ok(JoinOutcome::Joined(entries))
}

fn key_value(kind: FanoutKind, key: &str) -> Result<Value, FanoutPlanError> {
    match kind {
        FanoutKind::Parallel => Ok(Value::String(key.to_owned())),
        FanoutKind::Map => key
            .parse::<u64>()
            .ok()
            .filter(|index| key == index.to_string())
            .map(Value::from)
            .ok_or(FanoutPlanError::InvalidChild),
    }
}

fn entry(kind: FanoutKind, child: SettledChild) -> Result<Value, FanoutPlanError> {
    let key = key_value(kind, &child.key)?;
    match child.terminal {
        ChildTerminal::Succeeded(outputs) => {
            let outputs = Value::Object(outputs);
            if serialized_len(&outputs) > MAX_CHILD_RESULT_BYTES {
                return Err(FanoutPlanError::ChildResultTooLarge);
            }
            Ok(json!({
                kind.key_field(): key,
                "node": child.node,
                "status": "succeeded",
                "outputs": outputs,
            }))
        }
        ChildTerminal::Failed(failure) => Ok(error_entry(kind, &key, &child.node, &failure)),
    }
}

/// A failed entry that always fits [`MAX_JOIN_ERROR_ENTRY_BYTES`]: the safe
/// message, then the support reference, are shortened on a character
/// boundary, never the code or the key.
fn error_entry(kind: FanoutKind, key: &Value, node: &str, failure: &ChildFailure) -> Value {
    let build = |message: &str, support: Option<&str>| {
        let mut error = Map::new();
        error.insert("code".to_owned(), Value::String(failure.code.clone()));
        error.insert("message_safe".to_owned(), Value::String(message.to_owned()));
        if let Some(support) = support {
            error.insert("support_ref".to_owned(), Value::String(support.to_owned()));
        }
        json!({
            kind.key_field(): key.clone(),
            "node": node,
            "status": "failed",
            "error": Value::Object(error),
        })
    };
    let mut message = failure.message_safe.as_str();
    let mut support = failure.support_ref.as_deref();
    loop {
        let candidate = build(message, support);
        if serialized_len(&candidate) <= MAX_JOIN_ERROR_ENTRY_BYTES {
            return candidate;
        }
        if !message.is_empty() {
            message = shorten(message);
        } else if let Some(reference) = support {
            support = (!reference.is_empty()).then(|| shorten(reference));
            if support == Some("") {
                support = None;
            }
        } else {
            // Only the code and key remain; both are bounded by their
            // producers (code ≤ 128 ASCII, key ≤ 128). Keep the code.
            return build("", None);
        }
    }
}

fn shorten(text: &str) -> &str {
    let mut end = text.len().saturating_sub(text.len() / 4 + 1);
    while end > 0 && !text.is_char_boundary(end) {
        end -= 1;
    }
    &text[..end]
}

fn serialized_len(value: &Value) -> usize {
    serde_json::to_vec(value).map_or(usize::MAX, |bytes| bytes.len())
}

/// The update the node writes to its output key so that, after the key's
/// own reducer runs, the key holds the landed join.
///
/// `current` is the key's value before the node ran (`None` when unset).
pub fn land(
    mode: JoinMode,
    reducer: Option<StateReducer>,
    kind: FanoutKind,
    current: Option<&Value>,
    entries: Vec<Value>,
) -> Result<Value, FanoutPlanError> {
    let current = current.filter(|value| !value.is_null());
    let landed = match mode {
        JoinMode::Append => {
            let update = Value::Array(entries);
            match (reducer, current) {
                // The declared append reducer concatenates for us.
                (Some(StateReducer::Append), _) | (None, None) => update,
                (None, Some(Value::Array(held))) => {
                    let mut all = held.clone();
                    all.extend(update.as_array().into_iter().flatten().cloned());
                    Value::Array(all)
                }
                (None, Some(_)) => return Err(FanoutPlanError::JoinTypeMismatch),
                (Some(_), _) => return Err(FanoutPlanError::ReducerMismatch),
            }
        }
        JoinMode::Keyed => {
            let mut object = Map::new();
            for entry in entries {
                let key = entry_key(kind, &entry)?;
                object.insert(key, entry);
            }
            merge_object(reducer, current, object)?
        }
        JoinMode::Merge => {
            let mut object = Map::new();
            for entry in entries {
                if entry.get("status") != Some(&Value::String("succeeded".to_owned())) {
                    // `JoinMode::check` makes merge require `stop`.
                    return Err(FanoutPlanError::JoinConflict);
                }
                let Some(Value::Object(outputs)) = entry.get("outputs") else {
                    return Err(FanoutPlanError::JoinTypeMismatch);
                };
                for (key, value) in outputs {
                    if object.insert(key.clone(), value.clone()).is_some() {
                        return Err(FanoutPlanError::JoinConflict);
                    }
                }
            }
            merge_object(reducer, current, object)?
        }
    };
    if serialized_len(&landed) > MAX_JOINED_RESULT_BYTES {
        return Err(FanoutPlanError::JoinedResultTooLarge);
    }
    Ok(landed)
}

fn entry_key(kind: FanoutKind, entry: &Value) -> Result<String, FanoutPlanError> {
    match entry.get(kind.key_field()) {
        Some(Value::String(key)) => Ok(key.clone()),
        Some(Value::Number(index)) => Ok(index.to_string()),
        _ => Err(FanoutPlanError::InvalidChild),
    }
}

/// Merge `update` onto an object key. Whether the key keeps overwrite or
/// declares `merge`, a key already held is a conflict, never replaced.
fn merge_object(
    reducer: Option<StateReducer>,
    current: Option<&Value>,
    update: Map<String, Value>,
) -> Result<Value, FanoutPlanError> {
    let held = match current {
        None => Map::new(),
        Some(Value::Object(held)) => held.clone(),
        Some(_) => return Err(FanoutPlanError::JoinTypeMismatch),
    };
    if update.keys().any(|key| held.contains_key(key)) {
        return Err(FanoutPlanError::JoinConflict);
    }
    match reducer {
        // The merge reducer folds `update` onto the held object itself.
        Some(StateReducer::Merge) => Ok(Value::Object(update)),
        None => {
            let mut all = held;
            all.extend(update);
            Ok(Value::Object(all))
        }
        Some(_) => Err(FanoutPlanError::ReducerMismatch),
    }
}

#[cfg(test)]
#[path = "plan_tests.rs"]
mod tests;
