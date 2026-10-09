//! Typed, bounded reducers declared on pipeline state channels.
//!
//! A channel declares `reducer: append | sum_int | merge` in its descriptor;
//! without one it keeps ADK `Overwrite`. ADK `Reducer::Custom` cannot refuse
//! an update, so [`ReducerGuard`] checks every typed update while the node can
//! still fail, and the channel closure repeats the same deterministic
//! reduction. ADK `Append` and `Sum` are never used: they wrap scalars and
//! coerce through `f64`.

use std::collections::BTreeMap;
use std::sync::Arc;

use adk_rust::graph::{GraphError, Node, NodeContext, NodeOutput, Reducer, StateSchema};
use async_trait::async_trait;
use ring::digest;
use serde_json::{Number, Value};

use super::data_shaping::{ShapingCode, exact_i64, json_len_within};

/// Most elements an `append` channel holds.
pub(super) const MAX_APPEND_ELEMENTS: usize = 10_000;
/// Most top-level keys a `merge` channel holds.
pub(super) const MAX_MERGE_KEYS: usize = 1_000;
/// Most serialized bytes an `append` or `merge` channel holds.
pub(super) const MAX_REDUCED_BYTES: usize = 512 * 1024;

const REDUCER_DIGEST_DOMAIN: &[u8] = b"elitea.graph.pipeline.state-reducers.v1\0";

/// A non-overwrite reducer. Overwrite is the absence of one.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum StateReducer {
    Append,
    SumInt,
    Merge,
}

/// The reducer name is not one of the closed set.
#[derive(Debug, PartialEq, Eq)]
pub(super) struct UnknownReducer;

/// Why a typed update was refused. Names no value.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum ReducerFailure {
    TypeMismatch,
    Limit,
    Overflow,
}

impl ReducerFailure {
    pub(super) const fn code(self) -> &'static str {
        match self {
            Self::TypeMismatch => "graph.state.reducer_type_mismatch",
            Self::Limit => "graph.state.reducer_limit",
            Self::Overflow => "graph.state.reducer_overflow",
        }
    }
}

impl StateReducer {
    /// `Ok(None)` is the explicit default, `overwrite`.
    pub(super) fn parse(name: &str) -> Result<Option<Self>, UnknownReducer> {
        match name {
            "overwrite" => Ok(None),
            "append" => Ok(Some(Self::Append)),
            "sum_int" => Ok(Some(Self::SumInt)),
            "merge" => Ok(Some(Self::Merge)),
            _ => Err(UnknownReducer),
        }
    }

    pub(super) const fn tag(self) -> &'static str {
        match self {
            Self::Append => "append",
            Self::SumInt => "sum_int",
            Self::Merge => "merge",
        }
    }

    /// The only normalized state type this reducer accepts.
    pub(super) const fn state_type(self) -> &'static str {
        match self {
            Self::Append => "list",
            Self::SumInt => "int",
            Self::Merge => "dict",
        }
    }

    /// The value `current` becomes after `update`, or why the update is refused.
    pub(super) fn reduce_checked(
        self,
        current: &Value,
        update: &Value,
    ) -> Result<Value, ReducerFailure> {
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

    /// Whether this reducer's channel may hold `value`, e.g. a declared default.
    pub(super) fn check_held(self, value: &Value) -> Result<(), ReducerFailure> {
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
    pub(super) fn channel_reducer(self, channel: &str) -> Reducer {
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

fn integer(number: &Number) -> Result<i64, ReducerFailure> {
    exact_i64(number).map_err(|code| match code {
        ShapingCode::IntegerOverflow => ReducerFailure::Overflow,
        _ => ReducerFailure::TypeMismatch,
    })
}

/// Folds the typed reducers into a definition digest. Callers fold only when
/// at least one exists, so overwrite-only definitions keep their digest.
pub(super) fn reducers_digest(
    base: [u8; 32],
    reducers: &BTreeMap<String, StateReducer>,
) -> [u8; 32] {
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(REDUCER_DIGEST_DOMAIN);
    context.update(&base);
    context.update(&(reducers.len() as u64).to_be_bytes());
    for (key, reducer) in reducers {
        for field in [key.as_bytes(), reducer.tag().as_bytes()] {
            context.update(&(field.len() as u64).to_be_bytes());
            context.update(field);
        }
    }
    let mut output = [0_u8; 32];
    output.copy_from_slice(context.finish().as_ref());
    output
}

/// Refuses a node's typed state update before ADK applies it.
///
/// Pipelines run one node per super-step (`max_concurrency(1)`), so the
/// state the node read is the state its update reduces onto. An interrupted
/// node's updates are dropped by ADK and are not checked.
pub(super) struct ReducerGuard<N> {
    inner: N,
    reducers: Arc<BTreeMap<String, StateReducer>>,
}

impl<N> ReducerGuard<N> {
    pub(super) const fn new(inner: N, reducers: Arc<BTreeMap<String, StateReducer>>) -> Self {
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

    fn capabilities(&self) -> adk_rust::AgentCapabilities {
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
            if let Err(failure) = reducer.reduce_checked(current, update) {
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
