//! Typed, bounded reducers declared on pipeline state channels.
//!
//! A channel declares `reducer: append | sum_int | merge` in its descriptor;
//! without one it keeps ADK `Overwrite`. ADK `Reducer::Custom` cannot refuse
//! an update, so [`ReducerGuard`] checks every typed update while the node can
//! still fail, and the channel closure repeats the same deterministic
//! reduction. ADK `Append` and `Sum` are never used: they wrap scalars and
//! coerce through `f64`.

use std::collections::BTreeMap;
use std::io;
use std::sync::Arc;

use adk_graph::{GraphError, Node, NodeContext, NodeOutput, Reducer, StateSchema};
use async_trait::async_trait;
use serde_json::{Number, Value};

use crate::exact_number::{NumberFault, exact_i64};

/// Most elements an `append` channel holds.
pub const MAX_APPEND_ELEMENTS: usize = 10_000;
/// Most top-level keys a `merge` channel holds.
pub const MAX_MERGE_KEYS: usize = 1_000;
/// Most serialized bytes an `append` or `merge` channel holds.
pub const MAX_REDUCED_BYTES: usize = 512 * 1024;

/// A non-overwrite reducer. Overwrite is the absence of one.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum StateReducer {
    Append,
    SumInt,
    Merge,
}

/// The reducer name is not one of the closed set.
#[derive(Debug, PartialEq, Eq)]
pub struct UnknownReducer;

/// Why a typed update was refused. Names no value.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ReducerFailure {
    TypeMismatch,
    Limit,
    Overflow,
}

impl ReducerFailure {
    pub const fn code(self) -> &'static str {
        match self {
            Self::TypeMismatch => "graph.state.reducer_type_mismatch",
            Self::Limit => "graph.state.reducer_limit",
            Self::Overflow => "graph.state.reducer_overflow",
        }
    }
}

impl StateReducer {
    /// `Ok(None)` is the explicit default, `overwrite`.
    pub fn parse(name: &str) -> Result<Option<Self>, UnknownReducer> {
        match name {
            "overwrite" => Ok(None),
            "append" => Ok(Some(Self::Append)),
            "sum_int" => Ok(Some(Self::SumInt)),
            "merge" => Ok(Some(Self::Merge)),
            _ => Err(UnknownReducer),
        }
    }

    pub const fn tag(self) -> &'static str {
        match self {
            Self::Append => "append",
            Self::SumInt => "sum_int",
            Self::Merge => "merge",
        }
    }

    /// The only normalized state type this reducer accepts.
    pub const fn state_type(self) -> &'static str {
        match self {
            Self::Append => "list",
            Self::SumInt => "int",
            Self::Merge => "dict",
        }
    }

    /// The value `current` becomes after `update`, or why the update is refused.
    pub fn reduce_checked(self, current: &Value, update: &Value) -> Result<Value, ReducerFailure> {
        let reduced = match (self, current, update) {
            (Self::Append, Value::Array(current), Value::Array(update)) => {
                if current.len().saturating_add(update.len()) > MAX_APPEND_ELEMENTS {
                    return Err(ReducerFailure::Limit);
                }
                let mut values = Vec::with_capacity(current.len() + update.len());
                values.extend(current.iter().cloned());
                values.extend(update.iter().cloned());
                Value::Array(values)
            }
            (Self::SumInt, Value::Number(current), Value::Number(update)) => {
                let sum = integer(current)?
                    .checked_add(integer(update)?)
                    .ok_or(ReducerFailure::Overflow)?;
                return Ok(Value::from(sum));
            }
            (Self::Merge, Value::Object(current), Value::Object(update)) => {
                let mut values = current.clone();
                values.extend(
                    update
                        .iter()
                        .map(|(key, value)| (key.clone(), value.clone())),
                );
                Value::Object(values)
            }
            _ => return Err(ReducerFailure::TypeMismatch),
        };
        self.check_held(&reduced)?;
        Ok(reduced)
    }

    /// The guard's verdict, identical to [`Self::reduce_checked`], without
    /// building an `append` result: element counts and serialized sizes only.
    pub fn check_update(self, current: &Value, update: &Value) -> Result<(), ReducerFailure> {
        let (Self::Append, Value::Array(held), Value::Array(added)) = (self, current, update)
        else {
            return self.reduce_checked(current, update).map(|_| ());
        };
        if held.len().saturating_add(added.len()) > MAX_APPEND_ELEMENTS {
            return Err(ReducerFailure::Limit);
        }
        let held_bytes =
            json_len_within(current, MAX_REDUCED_BYTES).ok_or(ReducerFailure::Limit)?;
        let added_bytes =
            json_len_within(update, MAX_REDUCED_BYTES).ok_or(ReducerFailure::Limit)?;
        // `[a]` ++ `[b]` serializes as `[a,b]`: one comma replaces two brackets.
        let total = match (held.is_empty(), added.is_empty()) {
            (true, _) => added_bytes,
            (_, true) => held_bytes,
            _ => held_bytes + added_bytes - 1,
        };
        if total > MAX_REDUCED_BYTES {
            return Err(ReducerFailure::Limit);
        }
        Ok(())
    }

    /// Whether this reducer's channel may hold `value`, e.g. a declared default.
    pub fn check_held(self, value: &Value) -> Result<(), ReducerFailure> {
        let within_bytes = || {
            json_len_within(value, MAX_REDUCED_BYTES)
                .map(|_| ())
                .ok_or(ReducerFailure::Limit)
        };
        match (self, value) {
            (Self::Append, Value::Array(values)) if values.len() > MAX_APPEND_ELEMENTS => {
                Err(ReducerFailure::Limit)
            }
            (Self::Merge, Value::Object(values)) if values.len() > MAX_MERGE_KEYS => {
                Err(ReducerFailure::Limit)
            }
            (Self::Append, Value::Array(_)) | (Self::Merge, Value::Object(_)) => within_bytes(),
            (Self::SumInt, Value::Number(number)) => integer(number).map(|_| ()),
            _ => Err(ReducerFailure::TypeMismatch),
        }
    }

    /// The ADK channel reducer. The guard has already refused every update this
    /// would refuse, so a refusal here keeps the current value and is logged.
    pub fn channel_reducer(self, channel: &str) -> Reducer {
        let channel = channel.to_owned();
        Reducer::Custom(Arc::new(move |current, update| {
            match self.reduce_checked(&current, &update) {
                Ok(reduced) => reduced,
                Err(failure) => {
                    tracing::error!(
                        channel = %channel,
                        reducer = self.tag(),
                        error_code = failure.code(),
                        "a typed state update reached the channel unchecked; the value was kept"
                    );
                    current
                }
            }
        }))
    }
}

/// The number's exact `i64`. `Display` is the exact text with and without
/// `arbitrary_precision`, so no value passes through `f64`.
fn integer(number: &Number) -> Result<i64, ReducerFailure> {
    exact_i64(&number.to_string()).map_err(|fault| match fault {
        NumberFault::Overflow => ReducerFailure::Overflow,
        NumberFault::Unsupported | NumberFault::NotInteger => ReducerFailure::TypeMismatch,
    })
}

/// The serialized JSON length of `value`, or `None` once it passes `cap`
/// bytes. Length does not depend on member order, so `preserve_order` cannot
/// change it.
fn json_len_within(value: &Value, cap: usize) -> Option<usize> {
    let mut writer = CappedLen { remaining: cap };
    serde_json::to_writer(&mut writer, value).ok()?;
    Some(cap - writer.remaining)
}

struct CappedLen {
    remaining: usize,
}

impl io::Write for CappedLen {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        self.remaining = self
            .remaining
            .checked_sub(bytes.len())
            .ok_or_else(|| io::Error::other("typed state value exceeds its byte bound"))?;
        Ok(bytes.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

/// Refuses a node's typed state update before ADK applies it.
///
/// Pipelines run one node per super-step (`max_concurrency(1)`), so the
/// state the node read is the state its update reduces onto. An interrupted
/// node's updates are dropped by ADK and are not checked.
pub struct ReducerGuard<N> {
    inner: N,
    reducers: Arc<BTreeMap<String, StateReducer>>,
}

impl<N> ReducerGuard<N> {
    pub const fn new(inner: N, reducers: Arc<BTreeMap<String, StateReducer>>) -> Self {
        Self { inner, reducers }
    }
}

#[async_trait]
impl<N> Node for ReducerGuard<N>
where
    N: Node,
{
    fn name(&self) -> &str {
        self.inner.name()
    }

    fn description(&self) -> &str {
        self.inner.description()
    }

    fn capabilities(&self) -> adk_core::AgentCapabilities {
        self.inner.capabilities()
    }

    fn validate_against(&self, parent: &StateSchema) -> Result<(), GraphError> {
        self.inner.validate_against(parent)
    }

    fn validate(&self) -> Result<(), GraphError> {
        self.inner.validate()
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let output = self.inner.execute(context).await?;
        if output.interrupt.is_some() {
            return Ok(output);
        }
        for (channel, reducer) in self.reducers.iter() {
            let Some(update) = output.updates.get(channel) else {
                continue;
            };
            let current = context.state.get(channel).unwrap_or(&Value::Null);
            if let Err(failure) = reducer.check_update(current, update) {
                tracing::warn!(
                    node_id = self.inner.name(),
                    channel = %channel,
                    reducer = reducer.tag(),
                    error_code = failure.code(),
                    "A typed state update was refused; no output was written"
                );
                return Err(GraphError::NodeExecutionFailed {
                    node: self.inner.name().to_owned(),
                    message: format!("{}: {channel}", failure.code()),
                });
            }
        }
        Ok(output)
    }
}

#[cfg(test)]
#[path = "state_reducers_tests.rs"]
mod tests;
