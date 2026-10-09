//! Runtime last-writer trace and chat rendering of a pipeline's final result.
//!
//! The static topology of a stored pipeline cannot tell which node produced
//! the answer: routers, decisions and conditional paths choose at run time.
//! [`ResultTraceNode`] records, in one reserved Overwrite channel, the last
//! node of the current turn that wrote one of its declared outputs. The
//! result selector then renders the current values of exactly those keys.
//!
//! Rendering is pure text building. It never logs a state value.

use std::collections::HashMap;

use adk_rust::graph::{GraphError, Node, NodeContext, NodeOutput, State, StateSchema};
use async_trait::async_trait;
use serde_json::{Value, json};

/// Reserved runtime channel that records the last value-producing node write.
pub(super) const PIPELINE_RESULT_TRACE_STATE_KEY: &str = "__elitea_pipeline_result_trace_v1";

/// Upper bound of the final chat text, including fences and the notice line.
pub(super) const MAX_PIPELINE_RESULT_BYTES: usize = 512 * 1024;

/// Room kept for the JSON fence and the truncation notice.
const TRUNCATION_RESERVE_BYTES: usize = 256;
const JSON_FENCE_OPEN: &str = "```json\n";
const JSON_FENCE_CLOSE: &str = "\n```";
const MAX_TRACE_KEYS: usize = 256;

/// Message content-block types, mirrored from the legacy Python runtime.
const CONTENT_BLOCK_TYPES: &[&str] = &[
    "text",
    "thinking",
    "reasoning",
    "tool_use",
    "tool_result",
    "image",
    "image_url",
    "document",
    "search_result",
    "redacted_thinking",
    "server_tool_use",
    "web_search_tool_result",
    "mcp_tool_use",
    "mcp_tool_result",
    "code_execution_tool_result",
    "container_upload",
];
const TEXT_ONLY_BLOCK_KEYS: &[&str] = &["text", "type", "index"];

/// Keys that this runtime treats as internal for result display.
fn reserved_trace_key(key: &str) -> bool {
    key == "messages" || super::compiler::internal_result_key(key)
}

/// The declared outputs of one top-level node that can become its result.
#[derive(Clone, Debug)]
pub(super) struct ResultTraceOutputs {
    keys: Vec<String>,
    messages: bool,
}

impl ResultTraceOutputs {
    /// `keys` keeps declared order; `messages` and internal keys are removed.
    pub(super) fn new(keys: Vec<String>, messages: bool) -> Self {
        let mut unique = Vec::with_capacity(keys.len());
        for key in keys {
            if !reserved_trace_key(&key) && !unique.contains(&key) {
                unique.push(key);
            }
        }
        Self {
            keys: unique,
            messages,
        }
    }

    /// Build from a node's declared output list, or `None` when nothing in
    /// it can become the pipeline result.
    pub(super) fn from_declared(outputs: &[String]) -> Option<Self> {
        let outputs = Self::new(
            outputs.to_vec(),
            outputs.iter().any(|key| key == "messages"),
        );
        (!outputs.keys.is_empty() || outputs.messages).then_some(outputs)
    }

    /// The trace update for one completed node write, or `None` when the
    /// node wrote none of its declared result outputs.
    pub(super) fn trace_update(
        &self,
        node: &str,
        updates: &HashMap<String, Value>,
    ) -> Option<Value> {
        let keys = self
            .keys
            .iter()
            .filter(|key| updates.contains_key(key.as_str()))
            .cloned()
            .collect::<Vec<_>>();
        let messages = self.messages && updates.contains_key("messages");
        (!keys.is_empty() || messages)
            .then(|| json!({"node": node, "keys": keys, "messages": messages}))
    }
}

/// Thin wrapper that records the last value-producing write of a turn.
///
/// Errors and interrupts pass through unchanged. A node that wrote no
/// declared result output leaves the previous trace untouched.
pub(super) struct ResultTraceNode<N> {
    inner: N,
    outputs: ResultTraceOutputs,
}

impl<N> ResultTraceNode<N> {
    pub(super) const fn new(inner: N, outputs: ResultTraceOutputs) -> Self {
        Self { inner, outputs }
    }
}

#[async_trait]
impl<N> Node for ResultTraceNode<N>
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
        let mut output = self.inner.execute(context).await?;
        if output.interrupt.is_some() {
            return Ok(output);
        }
        if let Some(trace) = self
            .outputs
            .trace_update(self.inner.name(), &output.updates)
        {
            output
                .updates
                .insert(PIPELINE_RESULT_TRACE_STATE_KEY.to_owned(), trace);
        }
        Ok(output)
    }
}

/// The checkpointed trace of the current turn.
#[derive(Debug)]
pub(super) struct ResultTrace {
    keys: Vec<String>,
    messages: bool,
}

impl ResultTrace {
    /// Read a well-formed trace. Malformed or absent values select nothing.
    pub(super) fn from_state(state: &State) -> Option<Self> {
        let trace = state.get(PIPELINE_RESULT_TRACE_STATE_KEY)?.as_object()?;
        let raw_keys = trace.get("keys")?.as_array()?;
        if raw_keys.len() > MAX_TRACE_KEYS {
            return None;
        }
        let mut keys = Vec::with_capacity(raw_keys.len());
        for key in raw_keys {
            let key = key.as_str()?;
            if !reserved_trace_key(key) {
                keys.push(key.to_owned());
            }
        }
        let messages = trace.get("messages")?.as_bool()?;
        Some(Self { keys, messages })
    }

    pub(super) fn keys(&self) -> &[String] {
        &self.keys
    }

    pub(super) const fn messages(&self) -> bool {
        self.messages
    }
}

/// One rendered result before its size bound is applied.
#[derive(Debug, PartialEq, Eq)]
pub(super) enum RenderedResult {
    /// Plain chat text.
    Text(String),
    /// Pretty JSON text that is shown inside a `json` fence.
    Json(String),
}

impl RenderedResult {
    pub(super) fn is_blank(&self) -> bool {
        match self {
            Self::Text(text) | Self::Json(text) => text.trim().is_empty(),
        }
    }

    /// Final chat text, at most [`MAX_PIPELINE_RESULT_BYTES`] bytes.
    pub(super) fn into_bounded_text(self) -> String {
        match self {
            Self::Text(text) if text.len() <= MAX_PIPELINE_RESULT_BYTES => text,
            Self::Text(text) => {
                let budget = MAX_PIPELINE_RESULT_BYTES.saturating_sub(TRUNCATION_RESERVE_BYTES);
                let shown = char_prefix(&text, budget);
                let mut output = shown.to_owned();
                push_truncation_notice(&mut output, shown.len(), text.len());
                output
            }
            Self::Json(body) => {
                let fenced = JSON_FENCE_OPEN.len() + body.len() + JSON_FENCE_CLOSE.len();
                if fenced <= MAX_PIPELINE_RESULT_BYTES {
                    return format!("{JSON_FENCE_OPEN}{body}{JSON_FENCE_CLOSE}");
                }
                let budget = MAX_PIPELINE_RESULT_BYTES
                    .saturating_sub(TRUNCATION_RESERVE_BYTES)
                    .saturating_sub(JSON_FENCE_OPEN.len() + JSON_FENCE_CLOSE.len());
                let shown = char_prefix(&body, budget);
                let mut output = format!("{JSON_FENCE_OPEN}{shown}{JSON_FENCE_CLOSE}");
                push_truncation_notice(&mut output, shown.len(), body.len());
                output
            }
        }
    }
}

fn char_prefix(text: &str, limit: usize) -> &str {
    let mut end = limit.min(text.len());
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    &text[..end]
}

fn push_truncation_notice(output: &mut String, shown: usize, total: usize) {
    output.push_str("\n\n… output truncated: ");
    output.push_str(&shown.to_string());
    output.push_str(" of ");
    output.push_str(&total.to_string());
    output.push_str(" bytes. The full value is in the run state.");
}

/// Render one state value for chat.
///
/// Strings stay as they are. Numbers and booleans become JSON text with
/// their lexical form. A non-empty list whose every element is a message
/// content-block object becomes its joined text. Every other list or object
/// becomes pretty JSON.
pub(super) fn render_state_value(value: &Value) -> Option<RenderedResult> {
    match value {
        Value::Null => None,
        Value::String(text) => Some(RenderedResult::Text(text.clone())),
        Value::Number(_) | Value::Bool(_) => Some(RenderedResult::Text(value.to_string())),
        Value::Array(items) if !items.is_empty() && items.iter().all(is_content_block_object) => {
            Some(RenderedResult::Text(
                items.iter().filter_map(content_block_text).collect(),
            ))
        }
        Value::Array(_) | Value::Object(_) => serde_json::to_string_pretty(value)
            .ok()
            .map(RenderedResult::Json),
    }
}

/// Render the traced keys of one node write.
///
/// One key uses [`render_state_value`]. Several keys become one JSON object
/// whose keys keep the declared output order. Returns `None` when every value
/// renders blank.
pub(super) fn render_traced_keys(state: &State, keys: &[String]) -> Option<RenderedResult> {
    let null = Value::Null;
    let values = keys
        .iter()
        .map(|key| (key.as_str(), state.get(key).unwrap_or(&null)))
        .collect::<Vec<_>>();
    if values
        .iter()
        .all(|(_, value)| render_state_value(value).is_none_or(|rendered| rendered.is_blank()))
    {
        return None;
    }
    if let [(_, value)] = values.as_slice() {
        return render_state_value(value);
    }
    ordered_json_object(&values).map(RenderedResult::Json)
}

/// Pretty JSON object text with keys in the given order.
///
/// `serde_json::Map` sorts keys, so the object is composed from the pretty
/// text of each value. Nested lines receive one extra indentation level.
fn ordered_json_object(entries: &[(&str, &Value)]) -> Option<String> {
    let mut output = String::from("{");
    for (index, (key, value)) in entries.iter().enumerate() {
        if index > 0 {
            output.push(',');
        }
        output.push_str("\n  ");
        output.push_str(&serde_json::to_string(key).ok()?);
        output.push_str(": ");
        let pretty = serde_json::to_string_pretty(value).ok()?;
        for (line_index, line) in pretty.split('\n').enumerate() {
            if line_index > 0 {
                output.push_str("\n  ");
            }
            output.push_str(line);
        }
    }
    output.push_str("\n}");
    Some(output)
}

fn is_content_block_object(block: &Value) -> bool {
    let Some(block) = block.as_object() else {
        return false;
    };
    match block.get("type") {
        Some(Value::String(kind)) if CONTENT_BLOCK_TYPES.contains(&kind.as_str()) => {
            let required: &[&str] = match kind.as_str() {
                "text" => &["text"],
                "thinking" => &["thinking", "text"],
                "reasoning" => &["reasoning", "text"],
                "image" => &["image_url", "source", "data", "url"],
                "image_url" => &["image_url", "url"],
                "document" => &["source", "data"],
                _ => &[],
            };
            required.is_empty() || required.iter().any(|key| block.contains_key(*key))
        }
        Some(_) => false,
        None => {
            block.contains_key("text")
                && block
                    .keys()
                    .all(|key| TEXT_ONLY_BLOCK_KEYS.contains(&key.as_str()))
        }
    }
}

fn content_block_text(block: &Value) -> Option<&str> {
    let block = block.as_object()?;
    match block.get("type") {
        None => block.get("text")?.as_str(),
        Some(Value::String(kind)) if kind == "text" => block.get("text")?.as_str(),
        Some(_) => None,
    }
}

#[cfg(test)]
#[path = "pipeline_result_tests.rs"]
mod tests;
