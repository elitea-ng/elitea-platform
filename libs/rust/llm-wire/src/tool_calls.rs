//! Streamed tool calls: `OpenAI` `delta.tool_calls` fragments assembled into
//! whole calls, under count, name, id and argument limits.
//!
//! A fragment is read into a [`ToolCallDelta`] ([`parse_lenient_delta`] or
//! [`parse_strict_deltas`]) and applied to a [`ToolCallAssembler`] of the
//! same [`ToolCallProfile`]; [`ToolCallAssembler::finish`] yields the calls
//! in index order.
//!
//! # Lenient (the engines' model client)
//!
//! * `index` is optional. A server that omits it sends each call whole: a
//!   new id or a new complete name starts a new call.
//! * On an index already in use, a fragment with a different non-empty id,
//!   or a different name once the call's name is complete (its arguments
//!   began), starts a new call (a "generation" of that index). The same id
//!   renamed after its arguments began is refused.
//! * A name repeated in every fragment is one name; a name split before the
//!   arguments start is joined.
//! * Arguments that are not a string (a few servers send the object) are
//!   kept as their JSON text.
//! * At finish, a slot with no name and no arguments is dropped, one with
//!   arguments but no name is refused, and a call without an id gets
//!   `call_<position>`.
//! * Limits: [`ToolCallLimits::max_calls`] calls, and
//!   [`ToolCallLimits::max_argument_bytes`] per call. Names and ids are
//!   not bounded here (the caller's request bounded its tool names).
//!
//! # Strict (the agent worker)
//!
//! * A legacy `function_call` is refused; `tool_calls` holds at most
//!   [`ToolCallLimits::max_calls`] fragments, each an object with an
//!   `index` below that cap, a `function` object, `type` (when present)
//!   `function`, and `id`, `name`, `arguments` strings or null.
//! * An index is one call. Its id must be bounded header text
//!   ([`crate::headers::bounded_header_text`], [`ToolCallLimits::max_id_bytes`])
//!   and never change; name fragments are appended and the name must stay
//!   bounded header text ([`ToolCallLimits::max_name_bytes`]).
//! * At finish there must be at least one call and no index gap; each call
//!   must have an id, and ids must be unique. The caller checks, call by
//!   call and in order, what it alone knows (an admitted tool, JSON
//!   arguments): [`FinishedCalls`] yields one result per call so that its
//!   checks interleave with these in the original order.

use crate::headers::bounded_header_text;
use serde_json::{Map, Value};
use std::collections::{BTreeMap, HashMap, HashSet};

/// The bounds of one answer's tool calls.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ToolCallLimits {
    /// Calls in one answer (strict: also fragments in one delta, and the
    /// exclusive upper bound of `index`).
    pub max_calls: usize,
    /// One call's assembled arguments.
    pub max_argument_bytes: usize,
    /// Strict only: one call's assembled name.
    pub max_name_bytes: usize,
    /// Strict only: one call's id.
    pub max_id_bytes: usize,
}

/// Which client's reading (see the module documentation).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ToolCallProfile {
    Lenient,
    Strict,
}

/// One fragment of one call.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ToolCallDelta {
    pub index: Option<u64>,
    /// Lenient: an empty id reads as absent.
    pub id: Option<String>,
    /// Lenient: an empty name reads as absent.
    pub name: Option<String>,
    /// The fragment had a `function` member (lenient: a fragment without
    /// one only carries its id).
    pub has_function: bool,
    pub arguments: Option<String>,
}

/// A whole call.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ToolCall {
    pub id: String,
    pub name: String,
    /// The raw JSON text the model wrote.
    pub arguments: String,
}

/// Why tool calls were refused.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ToolCallError {
    /// Lenient: `tool_calls` is not a list.
    NotAList,
    /// Lenient: an `index` that is not a whole number.
    IndexNotWhole,
    /// More calls than [`ToolCallLimits::max_calls`] (strict: also an
    /// index at or above it when applied, or too many fragments).
    TooManyCalls { max: usize },
    /// A call's arguments above [`ToolCallLimits::max_argument_bytes`].
    ArgumentsTooLarge,
    /// Lenient: the same id renamed after its arguments began.
    Renamed { id: String },
    /// Lenient: a call with arguments and no name.
    NamelessArguments { index: u64 },
    /// Strict: a fragment or a set of calls outside the grammar.
    Malformed,
    /// Strict: the legacy `function_call` member.
    LegacyFunctionCall,
}

impl std::fmt::Display for ToolCallError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::NotAList => f.write_str("a tool_calls delta is not a list"),
            Self::IndexNotWhole => f.write_str("a tool call index is not a whole number"),
            Self::TooManyCalls { max } => write!(f, "the answer makes more than {max} tool calls"),
            Self::ArgumentsTooLarge => f.write_str("a tool call's arguments exceed their size cap"),
            Self::Renamed { id } => write!(
                f,
                "streamed tool call {id} changed its name after its arguments began"
            ),
            Self::NamelessArguments { index } => write!(
                f,
                "streamed tool call at index {index} has arguments but no name"
            ),
            Self::Malformed => f.write_str("a streamed tool call is malformed"),
            Self::LegacyFunctionCall => {
                f.write_str("the model returned an unsupported legacy function call")
            }
        }
    }
}

impl std::error::Error for ToolCallError {}

/// One element of a lenient `delta.tool_calls`. Only the index can be
/// refused; every other member that is missing or of another type reads
/// as absent.
///
/// # Errors
///
/// [`ToolCallError::IndexNotWhole`].
pub fn parse_lenient_delta(value: &Value) -> Result<ToolCallDelta, ToolCallError> {
    let non_empty = |text: Option<&str>| text.filter(|t| !t.is_empty()).map(str::to_owned);
    let function = value.get("function");
    let index = match value.get("index").filter(|v| !v.is_null()) {
        Some(index) => Some(index.as_u64().ok_or(ToolCallError::IndexNotWhole)?),
        None => None,
    };
    let arguments = match function.and_then(|f| f.get("arguments")) {
        None | Some(Value::Null) => None,
        Some(Value::String(text)) => Some(text.clone()),
        Some(other) => Some(other.to_string()),
    };
    Ok(ToolCallDelta {
        index,
        id: non_empty(value.get("id").and_then(Value::as_str)),
        name: non_empty(function.and_then(|f| f.get("name")).and_then(Value::as_str)),
        has_function: function.is_some(),
        arguments,
    })
}

/// A strict optional string: absent or null is `None`, a string is itself,
/// anything else is malformed.
fn optional_string(value: Option<&Value>) -> Result<Option<String>, ToolCallError> {
    match value {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(text)) => Ok(Some(text.clone())),
        Some(_) => Err(ToolCallError::Malformed),
    }
}

/// The fragments of a strict delta (the `delta` object of one choice).
///
/// # Errors
///
/// [`ToolCallError::LegacyFunctionCall`] for a non-null `function_call`;
/// [`ToolCallError::TooManyCalls`] for more fragments than
/// [`ToolCallLimits::max_calls`]; [`ToolCallError::Malformed`] for any
/// other shape the module documentation does not admit.
pub fn parse_strict_deltas(
    delta: &Map<String, Value>,
    limits: &ToolCallLimits,
) -> Result<Vec<ToolCallDelta>, ToolCallError> {
    if delta
        .get("function_call")
        .is_some_and(|value| !value.is_null())
    {
        return Err(ToolCallError::LegacyFunctionCall);
    }
    let Some(value) = delta.get("tool_calls").filter(|value| !value.is_null()) else {
        return Ok(Vec::new());
    };
    let calls = value.as_array().ok_or(ToolCallError::Malformed)?;
    if calls.len() > limits.max_calls {
        return Err(ToolCallError::TooManyCalls {
            max: limits.max_calls,
        });
    }
    calls
        .iter()
        .map(|call| {
            let call = call.as_object().ok_or(ToolCallError::Malformed)?;
            let index = call
                .get("index")
                .and_then(Value::as_u64)
                .filter(|index| usize::try_from(*index).is_ok_and(|i| i < limits.max_calls))
                .ok_or(ToolCallError::Malformed)?;
            if call
                .get("type")
                .is_some_and(|value| value.as_str() != Some("function"))
            {
                return Err(ToolCallError::Malformed);
            }
            let function = call
                .get("function")
                .and_then(Value::as_object)
                .ok_or(ToolCallError::Malformed)?;
            Ok(ToolCallDelta {
                index: Some(index),
                id: optional_string(call.get("id"))?,
                name: optional_string(function.get("name"))?,
                has_function: true,
                arguments: optional_string(function.get("arguments"))?,
            })
        })
        .collect()
}

#[derive(Debug, Default)]
struct PartialCall {
    /// Empty until a fragment names it.
    id: String,
    name: String,
    /// Lenient: the name is whole (arguments came with or after it). A
    /// different name after that is another call, never a suffix.
    name_complete: bool,
    arguments: String,
}

/// Where a call is kept: its `index`, then the generation of that index
/// (lenient: a server that reuses an index for a new call starts the next
/// generation; strict: always 0).
type CallKey = (u64, u32);

/// Builds one answer's tool calls from its fragments.
#[derive(Debug)]
pub struct ToolCallAssembler {
    profile: ToolCallProfile,
    limits: ToolCallLimits,
    /// Ordered by index, then generation. An index nobody used has no slot.
    calls: BTreeMap<CallKey, PartialCall>,
    /// Lenient: the current generation of each index.
    generations: HashMap<u64, u32>,
    /// Lenient: the call the last fragment went to, for a server without
    /// `index`.
    last: Option<CallKey>,
}

impl ToolCallAssembler {
    #[must_use]
    pub fn new(profile: ToolCallProfile, limits: ToolCallLimits) -> Self {
        Self {
            profile,
            limits,
            calls: BTreeMap::new(),
            generations: HashMap::new(),
            last: None,
        }
    }

    /// No fragment created a call yet.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.calls.is_empty()
    }

    /// Apply one fragment.
    ///
    /// # Errors
    ///
    /// The profile's refusals (see the module documentation).
    pub fn apply(&mut self, delta: ToolCallDelta) -> Result<(), ToolCallError> {
        match self.profile {
            ToolCallProfile::Lenient => self.apply_lenient(&delta),
            ToolCallProfile::Strict => self.apply_strict(delta),
        }
    }

    fn apply_strict(&mut self, delta: ToolCallDelta) -> Result<(), ToolCallError> {
        let index = delta.index.ok_or(ToolCallError::Malformed)?;
        if usize::try_from(index).map_or(true, |index| index >= self.limits.max_calls) {
            return Err(ToolCallError::TooManyCalls {
                max: self.limits.max_calls,
            });
        }
        let limits = self.limits;
        let call = self.calls.entry((index, 0)).or_default();
        if let Some(id) = delta.id {
            if !bounded_header_text(&id, limits.max_id_bytes)
                || (!call.id.is_empty() && call.id != id)
            {
                return Err(ToolCallError::Malformed);
            }
            call.id = id;
        }
        if let Some(name) = delta.name {
            call.name.push_str(&name);
            if !bounded_header_text(&call.name, limits.max_name_bytes) {
                return Err(ToolCallError::Malformed);
            }
        }
        if let Some(arguments) = delta.arguments {
            if call.arguments.len().saturating_add(arguments.len()) > limits.max_argument_bytes {
                return Err(ToolCallError::ArgumentsTooLarge);
            }
            call.arguments.push_str(&arguments);
        }
        Ok(())
    }

    fn apply_lenient(&mut self, delta: &ToolCallDelta) -> Result<(), ToolCallError> {
        let id = delta.id.as_deref().unwrap_or("");
        let name = delta.name.as_deref().unwrap_or("");
        let key = self.call_key(delta.index, id, name)?;
        self.last = Some(key);
        let max_argument_bytes = self.limits.max_argument_bytes;
        let call = self.calls.entry(key).or_default();
        if !id.is_empty() && call.id.is_empty() {
            id.clone_into(&mut call.id);
        }
        if !delta.has_function {
            return Ok(());
        }
        // OpenAI sends the name once; a server that repeats it in every
        // fragment is matched by the equality test. A name split over
        // fragments is joined only until the arguments start.
        if !name.is_empty() && call.name != name {
            call.name.push_str(name);
        }
        if let Some(fragment) = &delta.arguments {
            call.name_complete |= !call.name.is_empty();
            call.arguments.push_str(fragment);
        }
        if call.arguments.len() > max_argument_bytes {
            return Err(ToolCallError::ArgumentsTooLarge);
        }
        Ok(())
    }

    /// Lenient: the call a fragment belongs to, created when it is new.
    fn call_key(
        &mut self,
        index: Option<u64>,
        id: &str,
        name: &str,
    ) -> Result<CallKey, ToolCallError> {
        let starts_new = |call: &PartialCall| {
            let new_id = !id.is_empty() && !call.id.is_empty() && call.id != id;
            let new_name = !name.is_empty() && call.name_complete && call.name != name;
            new_id || new_name
        };
        let key = match index {
            Some(index) => match self.generations.get(&index).copied() {
                None => (index, 0),
                Some(generation) => {
                    let current = (index, generation);
                    match self.calls.get(&current) {
                        Some(call) if starts_new(call) => {
                            same_id_renamed(call, id)?;
                            (index, generation + 1)
                        }
                        _ => current,
                    }
                }
            },
            None => match self
                .last
                .and_then(|key| self.calls.get(&key).map(|call| (key, call)))
            {
                Some((key, call)) if !starts_new(call) => key,
                Some((_, call)) => {
                    same_id_renamed(call, id)?;
                    (self.next_index(), 0)
                }
                None => (self.next_index(), 0),
            },
        };
        if !self.calls.contains_key(&key) {
            if self.calls.len() >= self.limits.max_calls {
                return Err(ToolCallError::TooManyCalls {
                    max: self.limits.max_calls,
                });
            }
            self.generations.insert(key.0, key.1);
        }
        Ok(key)
    }

    /// Lenient: the index after every index seen, for a call without one.
    fn next_index(&self) -> u64 {
        self.calls
            .keys()
            .next_back()
            .map_or(0, |(index, _)| index.saturating_add(1))
    }

    /// The calls, in index order.
    ///
    /// # Errors
    ///
    /// Lenient: [`ToolCallError::NamelessArguments`]. Strict:
    /// [`ToolCallError::Malformed`] when there is no call or an index gap;
    /// each call's id is checked as it is yielded.
    pub fn finish(self) -> Result<FinishedCalls, ToolCallError> {
        match self.profile {
            ToolCallProfile::Lenient => {
                let mut calls = Vec::with_capacity(self.calls.len());
                for ((index, _), call) in self.calls {
                    if call.name.is_empty() {
                        // A slot that got no name and no arguments carries
                        // nothing to run (a keep-alive fragment, an id
                        // alone): dropped.
                        if call.arguments.is_empty() {
                            continue;
                        }
                        return Err(ToolCallError::NamelessArguments { index });
                    }
                    let position = calls.len();
                    calls.push(ToolCall {
                        id: if call.id.is_empty() {
                            format!("call_{position}")
                        } else {
                            call.id
                        },
                        name: call.name,
                        arguments: call.arguments,
                    });
                }
                Ok(FinishedCalls {
                    inner: Finished::Ready(calls.into_iter()),
                })
            }
            ToolCallProfile::Strict => {
                let contiguous = self
                    .calls
                    .keys()
                    .zip(0_u64..)
                    .all(|((index, _), position)| *index == position);
                if self.calls.is_empty() || !contiguous {
                    return Err(ToolCallError::Malformed);
                }
                Ok(FinishedCalls {
                    inner: Finished::Checked {
                        ids: HashSet::with_capacity(self.calls.len()),
                        calls: self.calls.into_values(),
                    },
                })
            }
        }
    }
}

/// Lenient: a call whose id stays the same but whose complete name changes
/// is not two calls and not one: refused.
fn same_id_renamed(call: &PartialCall, id: &str) -> Result<(), ToolCallError> {
    if !call.id.is_empty() && (id.is_empty() || call.id == id) {
        return Err(ToolCallError::Renamed {
            id: call.id.clone(),
        });
    }
    Ok(())
}

/// The calls of [`ToolCallAssembler::finish`], one result per call.
#[derive(Debug)]
pub struct FinishedCalls {
    inner: Finished,
}

#[derive(Debug)]
enum Finished {
    Ready(std::vec::IntoIter<ToolCall>),
    Checked {
        calls: std::collections::btree_map::IntoValues<CallKey, PartialCall>,
        ids: HashSet<String>,
    },
}

impl Iterator for FinishedCalls {
    type Item = Result<ToolCall, ToolCallError>;

    fn next(&mut self) -> Option<Self::Item> {
        match &mut self.inner {
            Finished::Ready(calls) => calls.next().map(Ok),
            Finished::Checked { calls, ids } => calls.next().map(|call| {
                if call.id.is_empty() || !ids.insert(call.id.clone()) {
                    return Err(ToolCallError::Malformed);
                }
                Ok(ToolCall {
                    id: call.id,
                    name: call.name,
                    arguments: call.arguments,
                })
            }),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    const LENIENT: ToolCallLimits = ToolCallLimits {
        max_calls: 128,
        max_argument_bytes: 1024 * 1024,
        max_name_bytes: usize::MAX,
        max_id_bytes: usize::MAX,
    };

    const STRICT: ToolCallLimits = ToolCallLimits {
        max_calls: 16,
        max_argument_bytes: 256 * 1024,
        max_name_bytes: 256,
        max_id_bytes: 512,
    };

    fn lenient(deltas: &[Value]) -> Result<Vec<ToolCall>, ToolCallError> {
        let mut assembler = ToolCallAssembler::new(ToolCallProfile::Lenient, LENIENT);
        for delta in deltas {
            assembler.apply(parse_lenient_delta(delta)?)?;
        }
        assembler.finish()?.collect()
    }

    fn strict(deltas: &[Value]) -> Result<Vec<ToolCall>, ToolCallError> {
        let mut assembler = ToolCallAssembler::new(ToolCallProfile::Strict, STRICT);
        for delta in deltas {
            let Value::Object(delta) = delta else {
                return Err(ToolCallError::Malformed);
            };
            for fragment in parse_strict_deltas(delta, &STRICT)? {
                assembler.apply(fragment)?;
            }
        }
        assembler.finish()?.collect()
    }

    fn summary(calls: &[ToolCall]) -> Vec<(&str, &str, &str)> {
        calls
            .iter()
            .map(|c| (c.id.as_str(), c.name.as_str(), c.arguments.as_str()))
            .collect()
    }

    #[test]
    fn lenient_deltas_without_an_index_start_a_call_per_id() {
        let calls = lenient(&[
            json!({"id": "a", "function": {"name": "f", "arguments": "{}"}}),
            json!({"id": "b", "function": {"name": "g", "arguments": "{\"x\""}}),
            json!({"function": {"arguments": ":1}"}}),
        ]);
        assert_eq!(
            calls.as_deref().map(summary),
            Ok(vec![("a", "f", "{}"), ("b", "g", "{\"x\":1}")])
        );
    }

    #[test]
    fn lenient_a_new_id_on_a_used_index_starts_a_new_call() {
        let calls = lenient(&[
            json!({"index": 0, "id": "a", "function": {"name": "get_code", "arguments": "{\"p\""}}),
            json!({"index": 0, "function": {"arguments": ":1}"}}),
            json!({"index": 0, "id": "b", "function": {"name": "think", "arguments": "{}"}}),
            json!({"index": 0, "function": {"arguments": ""}}),
        ]);
        assert_eq!(
            calls.as_deref().map(summary),
            Ok(vec![("a", "get_code", "{\"p\":1}"), ("b", "think", "{}")])
        );
    }

    #[test]
    fn lenient_names_repeat_join_and_rename() {
        let repeated = lenient(&[
            json!({"index": 0, "id": "a", "function": {"name": "f", "arguments": "{"}}),
            json!({"index": 0, "function": {"name": "f", "arguments": "}"}}),
        ]);
        assert_eq!(repeated.as_deref().map(summary), Ok(vec![("a", "f", "{}")]));
        let split = lenient(&[
            json!({"index": 0, "id": "a", "function": {"name": "get_"}}),
            json!({"index": 0, "function": {"name": "code", "arguments": "{}"}}),
        ]);
        assert_eq!(
            split.as_deref().map(summary),
            Ok(vec![("a", "get_code", "{}")])
        );
        let reused = lenient(&[
            json!({"index": 0, "function": {"name": "f", "arguments": "{}"}}),
            json!({"index": 0, "function": {"name": "g", "arguments": "{}"}}),
        ]);
        assert_eq!(
            reused.as_deref().map(summary),
            Ok(vec![("call_0", "f", "{}"), ("call_1", "g", "{}")])
        );
        let renamed = lenient(&[
            json!({"index": 0, "id": "a", "function": {"name": "f", "arguments": "{}"}}),
            json!({"index": 0, "id": "a", "function": {"name": "g"}}),
        ]);
        assert_eq!(
            renamed.map_err(|e| e.to_string()),
            Err("streamed tool call a changed its name after its arguments began".to_owned())
        );
    }

    #[test]
    fn lenient_slots_order_drop_and_refuse() {
        let calls = lenient(&[
            json!({"index": 1, "id": "a", "function": {"name": "f", "arguments": "{}"}}),
            json!({"index": 5, "id": "b", "function": {"name": "g", "arguments": "{}"}}),
            json!({"index": 3, "id": "c", "function": {"name": "h", "arguments": "{}"}}),
            json!({"index": 7, "id": "ghost"}),
        ]);
        assert_eq!(
            calls.as_deref().map(summary),
            Ok(vec![("a", "f", "{}"), ("c", "h", "{}"), ("b", "g", "{}")])
        );
        assert_eq!(
            lenient(&[json!({"index": 2, "function": {"arguments": "{}"}})])
                .map_err(|e| e.to_string()),
            Err("streamed tool call at index 2 has arguments but no name".to_owned())
        );
        assert_eq!(
            lenient(&[json!({"index": "0", "function": {"name": "f"}})]),
            Err(ToolCallError::IndexNotWhole)
        );
        // An object for arguments is kept as its JSON text.
        let object =
            lenient(&[json!({"index": 0, "function": {"name": "f", "arguments": {"a": 1}}})]);
        assert_eq!(
            object.as_deref().map(summary),
            Ok(vec![("call_0", "f", "{\"a\":1}")])
        );
    }

    #[test]
    fn lenient_caps_count_calls_and_argument_bytes() {
        let deltas: Vec<Value> = (0..=LENIENT.max_calls)
            .map(|i| json!({"index": i * 1000, "id": format!("c{i}"), "function": {"name": "f", "arguments": "{}"}}))
            .collect();
        assert!(lenient(&deltas[..LENIENT.max_calls]).is_ok());
        assert_eq!(
            lenient(&deltas).map_err(|e| e.to_string()),
            Err("the answer makes more than 128 tool calls".to_owned())
        );
        let big = "x".repeat(LENIENT.max_argument_bytes + 1);
        assert_eq!(
            lenient(&[json!({"index": 0, "function": {"name": "f", "arguments": big}})]),
            Err(ToolCallError::ArgumentsTooLarge)
        );
    }

    #[test]
    fn strict_assembles_fragments_by_index() {
        let calls = strict(&[
            json!({"tool_calls": [{"index": 0, "id": "a", "type": "function", "function": {"name": "get_", "arguments": "{\"p\""}}]}),
            json!({"tool_calls": [{"index": 0, "function": {"name": "code", "arguments": ":1}"}}, {"index": 1, "id": "b", "function": {"name": "f", "arguments": null}}]}),
            json!({"tool_calls": null}),
            json!({"content": "x"}),
        ]);
        assert_eq!(
            calls.as_deref().map(summary),
            Ok(vec![("a", "get_code", "{\"p\":1}"), ("b", "f", "")])
        );
        // A repeated name is appended, not matched.
        let repeated = strict(&[
            json!({"tool_calls": [{"index": 0, "id": "a", "function": {"name": "f"}}]}),
            json!({"tool_calls": [{"index": 0, "function": {"name": "f"}}]}),
        ]);
        assert_eq!(repeated.as_deref().map(summary), Ok(vec![("a", "ff", "")]));
    }

    #[test]
    fn strict_fragments_outside_the_grammar_are_refused() {
        for (delta, expected) in [
            (
                json!({"function_call": {"name": "f"}}),
                ToolCallError::LegacyFunctionCall,
            ),
            (json!({"tool_calls": {}}), ToolCallError::Malformed),
            (json!({"tool_calls": [1]}), ToolCallError::Malformed),
            (json!({"tool_calls": [{}]}), ToolCallError::Malformed),
            (
                json!({"tool_calls": [{"index": 16, "function": {}}]}),
                ToolCallError::Malformed,
            ),
            (
                json!({"tool_calls": [{"index": -1, "function": {}}]}),
                ToolCallError::Malformed,
            ),
            (
                json!({"tool_calls": [{"index": 0, "type": null, "function": {}}]}),
                ToolCallError::Malformed,
            ),
            (
                json!({"tool_calls": [{"index": 0, "type": "custom", "function": {}}]}),
                ToolCallError::Malformed,
            ),
            (
                json!({"tool_calls": [{"index": 0}]}),
                ToolCallError::Malformed,
            ),
            (
                json!({"tool_calls": [{"index": 0, "id": 1, "function": {}}]}),
                ToolCallError::Malformed,
            ),
            (
                json!({"tool_calls": [{"index": 0, "function": {"name": 1}}]}),
                ToolCallError::Malformed,
            ),
            (
                json!({"tool_calls": [{"index": 0, "function": {"arguments": {}}}]}),
                ToolCallError::Malformed,
            ),
            (
                json!({"tool_calls": vec![json!({"index": 0, "function": {}}); 17]}),
                ToolCallError::TooManyCalls { max: 16 },
            ),
        ] {
            let Value::Object(delta) = &delta else {
                panic!("not an object");
            };
            assert_eq!(
                parse_strict_deltas(delta, &STRICT),
                Err(expected),
                "{delta:?}"
            );
        }
    }

    #[test]
    fn strict_ids_names_and_arguments_are_bounded() {
        let one = |fragments: &[Value]| strict(&[json!({"tool_calls": fragments})]);
        assert_eq!(
            one(&[json!({"index": 0, "id": "", "function": {"name": "f"}})]),
            Err(ToolCallError::Malformed)
        );
        assert_eq!(
            one(&[
                json!({"index": 0, "id": "a", "function": {"name": "f"}}),
                json!({"index": 0, "id": "b", "function": {}}),
            ]),
            Err(ToolCallError::Malformed)
        );
        assert_eq!(
            one(&[json!({"index": 0, "id": "a", "function": {"name": ""}})]),
            Err(ToolCallError::Malformed)
        );
        assert_eq!(
            one(&[json!({"index": 0, "id": "a", "function": {"name": "x".repeat(257)}})]),
            Err(ToolCallError::Malformed)
        );
        assert_eq!(
            one(&[json!({"index": 0, "id": "a\n", "function": {"name": "f"}})]),
            Err(ToolCallError::Malformed)
        );
        let half = "x".repeat(STRICT.max_argument_bytes / 2 + 1);
        assert_eq!(
            one(&[
                json!({"index": 0, "id": "a", "function": {"name": "f", "arguments": half}}),
                json!({"index": 0, "function": {"arguments": half}}),
            ]),
            Err(ToolCallError::ArgumentsTooLarge)
        );
        let mut assembler = ToolCallAssembler::new(ToolCallProfile::Strict, STRICT);
        assert_eq!(
            assembler.apply(ToolCallDelta {
                index: Some(16),
                ..ToolCallDelta::default()
            }),
            Err(ToolCallError::TooManyCalls { max: 16 })
        );
        assert_eq!(
            assembler.apply(ToolCallDelta::default()),
            Err(ToolCallError::Malformed)
        );
    }

    #[test]
    fn strict_finish_needs_contiguous_unique_ids() {
        let gap = strict(&[json!({"tool_calls": [
            {"index": 0, "id": "a", "function": {"name": "f"}},
            {"index": 2, "id": "c", "function": {"name": "h"}},
        ]})]);
        assert_eq!(gap, Err(ToolCallError::Malformed));
        assert_eq!(strict(&[json!({})]), Err(ToolCallError::Malformed));
        let duplicate = strict(&[json!({"tool_calls": [
            {"index": 0, "id": "a", "function": {"name": "f"}},
            {"index": 1, "id": "a", "function": {"name": "g"}},
        ]})]);
        assert_eq!(duplicate, Err(ToolCallError::Malformed));
        // Calls are yielded in order, each checked as it comes: the first
        // call is already out when the second is refused.
        let mut assembler = ToolCallAssembler::new(ToolCallProfile::Strict, STRICT);
        for fragment in [
            ToolCallDelta {
                index: Some(0),
                id: Some("a".to_owned()),
                name: Some("f".to_owned()),
                has_function: true,
                arguments: None,
            },
            ToolCallDelta {
                index: Some(1),
                name: Some("g".to_owned()),
                has_function: true,
                ..ToolCallDelta::default()
            },
        ] {
            assert_eq!(assembler.apply(fragment), Ok(()));
        }
        let Ok(mut calls) = assembler.finish() else {
            panic!("refused");
        };
        assert!(matches!(calls.next(), Some(Ok(call)) if call.id == "a"));
        assert_eq!(calls.next(), Some(Err(ToolCallError::Malformed)));
    }
}
