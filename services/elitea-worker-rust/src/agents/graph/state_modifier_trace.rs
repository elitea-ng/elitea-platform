//! State Modifier transitional edge trace (FU-351, D12).
//!
//! Emits one bounded, redacted `agent_on_transitional_edge` frame per State
//! Modifier visit when the node declares a `transition`. Matches legacy
//! `elitea_sdk/runtime/langchain/langraph_agent.py:757,775-777,1655-1658`
//! (`TransitionalEdge`) and `parseRunsByEvent.edgeHandlers.ts:21-99`.

use std::collections::{BTreeMap, HashMap};

use adk_rust::{
    Event,
    graph::{END, GraphError, Node, NodeContext, NodeOutput, State},
};
use async_trait::async_trait;
use chrono::Utc;
use elitea_agent_runtime::graph::node_settings::NODE_ERRORS_KEY;
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};

use super::{
    compiler::reserved_user_state_key,
    node_events::{
        PIPELINE_NODE_EVENT_SCOPE_STATE_KEY, PipelineNodeEventScope, PipelineNodeEventSender,
    },
    yaml::valid_graph_id,
};

pub(crate) const TRANSITIONAL_EDGE_METADATA_KEY: &str = "elitea.pipeline.transitional_edge.v1";
const MAX_TRANSITIONAL_EDGE_BYTES: usize = 24 * 1024;
const MAX_TRANSITIONAL_EDGE_STATE_BYTES: usize = 20 * 1024;
const MAX_REDACT_DEPTH: usize = 16;
const REDACTED: &str = "[REDACTED]";
const TRUNCATED_SUFFIX: &str = "...[truncated]";

const SENSITIVE_KEY_TOKENS: &[&str] = &[
    "password",
    "passwd",
    "secret",
    "token",
    "api_key",
    "apikey",
    "authorization",
    "credential",
    "private_key",
    "access_key",
    "cookie",
    "bearer",
    "auth_header",
    "x-api-key",
    "x_api_key",
];

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct TransitionalEdgeEvent {
    pub(crate) node_id: String,
    pub(crate) next_step: String,
    pub(crate) state: Map<String, Value>,
}

impl TransitionalEdgeEvent {
    pub(crate) fn from_event(event: &Event) -> Result<Option<Self>, GraphError> {
        let Some(encoded) = event.provider_metadata.get(TRANSITIONAL_EDGE_METADATA_KEY) else {
            return Ok(None);
        };
        if encoded.len() > MAX_TRANSITIONAL_EDGE_BYTES
            || event.content().is_some()
            || !event.llm_response.partial
            || event.llm_response.turn_complete
            || !event.actions.state_delta.is_empty()
        {
            return Err(invalid());
        }
        let edge: Self = serde_json::from_str(encoded).map_err(|_| invalid())?;
        edge.validate()?;
        Ok(Some(edge))
    }

    fn validate(&self) -> Result<(), GraphError> {
        if !valid_graph_id(&self.node_id)
            || (self.next_step != "END" && !valid_graph_id(&self.next_step))
        {
            return Err(invalid());
        }
        Ok(())
    }

    fn event(&self) -> Result<Event, GraphError> {
        self.validate()?;
        let encoded = serde_json::to_string(self).map_err(|_| invalid())?;
        if encoded.len() > MAX_TRANSITIONAL_EDGE_BYTES {
            return Err(invalid());
        }
        let mut event = Event::new("pipeline_transitional_edge");
        event.timestamp = Utc::now();
        event.llm_response.partial = true;
        event
            .provider_metadata
            .insert(TRANSITIONAL_EDGE_METADATA_KEY.into(), encoded);
        Ok(event)
    }
}

/// Wraps a `StateModifier` (outside any `SettingsNode`) to emit one bounded
/// `agent_on_transitional_edge` frame after the node settles its outputs.
pub(super) struct StateModifierTraceNode<N> {
    inner: N,
    transition: String,
    sender: PipelineNodeEventSender,
}

impl<N> StateModifierTraceNode<N> {
    pub(super) fn new(inner: N, transition: String, sender: PipelineNodeEventSender) -> Self {
        Self {
            inner,
            transition,
            sender,
        }
    }
}

#[async_trait]
impl<N> Node for StateModifierTraceNode<N>
where
    N: Node + 'static,
{
    fn name(&self) -> &str {
        self.inner.name()
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let output = self.inner.execute(context).await?;
        let scope = PipelineNodeEventScope::from_state(
            context.state.get(PIPELINE_NODE_EVENT_SCOPE_STATE_KEY),
        )
        .map_err(|_| invalid())?;
        let next_step = output
            .goto
            .as_ref()
            .and_then(|targets| targets.first())
            .map(|target| {
                if target == END {
                    "END".to_owned()
                } else {
                    target.clone()
                }
            })
            .unwrap_or_else(|| self.transition.clone());
        let state = sanitize_and_bound_state(&context.state, &output.updates);
        let event = TransitionalEdgeEvent {
            node_id: self.inner.name().to_owned(),
            next_step,
            state,
        }
        .event()?;
        self.sender
            .send(self.inner.name(), scope.as_ref(), event)
            .await
            .map_err(|_| invalid())?;
        Ok(output)
    }
}

fn is_omitted_state_key(key: &str) -> bool {
    key.starts_with("__elitea_")
        || reserved_user_state_key(key)
        || matches!(key, "messages" | "chat_history")
}

fn is_sensitive_key(key: &str) -> bool {
    let lower = key.to_ascii_lowercase();
    SENSITIVE_KEY_TOKENS
        .iter()
        .any(|token| lower.contains(token))
}

fn looks_like_secret_value(text: &str) -> bool {
    let trimmed = text.trim();
    if trimmed.is_empty() {
        return false;
    }
    let lower = trimmed.to_ascii_lowercase();
    if lower.starts_with("bearer ")
        || lower.starts_with("basic ")
        || trimmed.contains("-----BEGIN ")
        || trimmed.starts_with("sk-")
        || trimmed.starts_with("ghp_")
        || trimmed.starts_with("gho_")
        || trimmed.starts_with("ghu_")
        || trimmed.starts_with("ghs_")
        || trimmed.starts_with("glpat-")
        || trimmed.starts_with("xoxb-")
        || trimmed.starts_with("xoxp-")
        || trimmed.starts_with("AKIA")
    {
        return true;
    }
    // Redact inline credentials in URLs (`scheme://user:pass@host`) or
    // embedded `Bearer <token>` / `sk-...` tokens inside longer strings.
    if lower.contains("bearer ")
        || trimmed.contains("sk-proj-")
        || trimmed.contains("sk-ant-")
        || trimmed.contains("ghp_")
        || trimmed.contains("glpat-")
        || trimmed.contains("AKIA")
    {
        return true;
    }
    if let Some(scheme_end) = trimmed.find("://") {
        let rest = &trimmed[scheme_end + 3..];
        let authority = rest.split(['/', '?', '#']).next().unwrap_or("");
        if let Some(at) = authority.rfind('@')
            && authority[..at].contains(':')
        {
            return true;
        }
    }
    false
}

fn sanitize_value(value: &Value, depth: usize, max_str_bytes: usize) -> Value {
    if depth >= MAX_REDACT_DEPTH {
        return Value::String(TRUNCATED_SUFFIX.to_owned());
    }
    match value {
        Value::Null | Value::Bool(_) | Value::Number(_) => value.clone(),
        Value::String(text) => {
            if looks_like_secret_value(text) {
                Value::String(REDACTED.to_owned())
            } else {
                Value::String(truncate_utf8(text, max_str_bytes))
            }
        }
        Value::Array(items) => Value::Array(
            items
                .iter()
                .map(|item| sanitize_value(item, depth + 1, max_str_bytes))
                .collect(),
        ),
        Value::Object(map) => {
            let mut out = Map::with_capacity(map.len());
            for (k, v) in map {
                if is_omitted_state_key(k) {
                    continue;
                }
                if is_sensitive_key(k) {
                    out.insert(k.clone(), Value::String(REDACTED.to_owned()));
                } else {
                    out.insert(k.clone(), sanitize_value(v, depth + 1, max_str_bytes));
                }
            }
            Value::Object(out)
        }
    }
}

fn truncate_utf8(text: &str, max_bytes: usize) -> String {
    if text.len() <= max_bytes {
        return text.to_owned();
    }
    let keep = max_bytes.saturating_sub(TRUNCATED_SUFFIX.len());
    let mut end = keep.min(text.len());
    while end > 0 && !text.is_char_boundary(end) {
        end -= 1;
    }
    let mut out = String::with_capacity(end + TRUNCATED_SUFFIX.len());
    out.push_str(&text[..end]);
    out.push_str(TRUNCATED_SUFFIX);
    out
}

fn encoded_len(map: &Map<String, Value>) -> usize {
    serde_json::to_string(map).map_or(usize::MAX, |s| s.len())
}

pub(crate) fn sanitize_and_bound_state(
    current: &State,
    updates: &HashMap<String, Value>,
) -> Map<String, Value> {
    let mut merged: BTreeMap<String, Value> = BTreeMap::new();
    for (k, v) in current {
        if !is_omitted_state_key(k) {
            merged.insert(k.clone(), v.clone());
        }
    }
    for (k, v) in updates {
        if is_omitted_state_key(k) {
            continue;
        }
        if k == NODE_ERRORS_KEY
            && let Value::Array(new_entries) = v
        {
            let mut combined = match merged.get(NODE_ERRORS_KEY) {
                Some(Value::Array(existing)) => existing.clone(),
                _ => Vec::new(),
            };
            combined.extend(new_entries.iter().cloned());
            merged.insert(k.clone(), Value::Array(combined));
            continue;
        }
        merged.insert(k.clone(), v.clone());
    }

    for max_str_bytes in [4096usize, 1024, 256, 64] {
        let mut candidate = Map::with_capacity(merged.len());
        for (k, v) in &merged {
            if is_sensitive_key(k) {
                candidate.insert(k.clone(), Value::String(REDACTED.to_owned()));
            } else {
                candidate.insert(k.clone(), sanitize_value(v, 0, max_str_bytes));
            }
        }
        if encoded_len(&candidate) <= MAX_TRANSITIONAL_EDGE_STATE_BYTES {
            return candidate;
        }
    }

    // State still exceeded the frame budget (e.g. very large arrays or many keys).
    // Preserve bounded `node_errors` (D12) and mark truncation explicitly.
    let mut minimal = Map::new();
    minimal.insert("_truncated".to_owned(), Value::Bool(true));
    if let Some(errors) = merged.get(NODE_ERRORS_KEY) {
        let sanitized = sanitize_value(errors, 0, 128);
        minimal.insert(NODE_ERRORS_KEY.to_owned(), sanitized);
        if encoded_len(&minimal) > MAX_TRANSITIONAL_EDGE_STATE_BYTES {
            if let Some(Value::Array(entries)) = minimal.get(NODE_ERRORS_KEY).cloned() {
                // Keep the most recent entries that fit within budget.
                let mut kept = Vec::new();
                for entry in entries.into_iter().rev() {
                    kept.push(entry);
                    let mut probe = Map::new();
                    probe.insert("_truncated".to_owned(), Value::Bool(true));
                    let mut ordered = kept.clone();
                    ordered.reverse();
                    probe.insert(NODE_ERRORS_KEY.to_owned(), Value::Array(ordered));
                    if encoded_len(&probe) > MAX_TRANSITIONAL_EDGE_STATE_BYTES {
                        kept.pop();
                        break;
                    }
                }
                kept.reverse();
                minimal.insert(NODE_ERRORS_KEY.to_owned(), Value::Array(kept));
            } else {
                minimal.remove(NODE_ERRORS_KEY);
            }
        }
    }
    minimal
}

fn invalid() -> GraphError {
    GraphError::Other("pipeline transitional edge event is invalid".into())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn redacts_secret_keys_secret_values_and_omits_internal_keys() {
        let mut state = State::new();
        state.insert("__elitea_executed_once_v1".into(), json!({"shape": {}}));
        state.insert(
            "__elitea_application_variable_1_x".into(),
            json!("internal"),
        );
        state.insert(
            "messages".into(),
            json!([{"role": "user", "content": "hi"}]),
        );
        state.insert("chat_history".into(), json!(["secret history"]));
        state.insert("api_token".into(), json!("TOP-SECRET-VALUE-123"));
        state.insert(
            "nested".into(),
            json!({
                "db_password": "SUPER-SECRET-PASSWORD",
                "header": "Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig",
                "url": "https://alice:hunter2@db.internal/prod",
                "ok": "visible"
            }),
        );
        state.insert("public".into(), json!("hello"));

        let mut updates = HashMap::new();
        updates.insert("out".into(), json!("world"));
        updates.insert("gh_leak".into(), json!("ghp_1234567890abcdef1234567890"));

        let sanitized = sanitize_and_bound_state(&state, &updates);
        let serialized = serde_json::to_string(&sanitized).expect("json");

        assert!(!sanitized.contains_key("__elitea_executed_once_v1"));
        assert!(!sanitized.contains_key("__elitea_application_variable_1_x"));
        assert!(!sanitized.contains_key("messages"));
        assert!(!sanitized.contains_key("chat_history"));
        assert_eq!(sanitized["api_token"], json!(REDACTED));
        assert_eq!(sanitized["nested"]["db_password"], json!(REDACTED));
        assert_eq!(sanitized["nested"]["header"], json!(REDACTED));
        assert_eq!(sanitized["nested"]["url"], json!(REDACTED));
        assert_eq!(sanitized["nested"]["ok"], json!("visible"));
        assert_eq!(sanitized["gh_leak"], json!(REDACTED));
        assert_eq!(sanitized["public"], json!("hello"));
        assert_eq!(sanitized["out"], json!("world"));
        assert!(!serialized.contains("TOP-SECRET"));
        assert!(!serialized.contains("SUPER-SECRET"));
        assert!(!serialized.contains("hunter2"));
        assert!(!serialized.contains("ghp_1234567890"));
    }

    #[test]
    fn bounds_oversized_state_and_preserves_node_errors() {
        let mut state = State::new();
        for i in 0..400 {
            state.insert(format!("field_{i:03}"), json!("x".repeat(500)));
        }
        let mut updates = HashMap::new();
        updates.insert(
            NODE_ERRORS_KEY.into(),
            json!([{"node": "shape", "handled": "continue"}]),
        );

        let bounded = sanitize_and_bound_state(&state, &updates);
        let encoded = serde_json::to_string(&bounded).expect("json");
        assert!(encoded.len() <= MAX_TRANSITIONAL_EDGE_STATE_BYTES);
        assert_eq!(bounded["_truncated"], json!(true));
        assert_eq!(bounded[NODE_ERRORS_KEY][0]["node"], json!("shape"));

        let event = TransitionalEdgeEvent {
            node_id: "shape".into(),
            next_step: "after".into(),
            state: bounded,
        }
        .event()
        .expect("bounded event fits");
        let parsed = TransitionalEdgeEvent::from_event(&event)
            .expect("round-trip")
            .expect("present");
        assert_eq!(parsed.node_id, "shape");
        assert_eq!(parsed.next_step, "after");
    }

    #[test]
    fn merges_appended_node_errors_without_dropping_prior_entries() {
        let mut state = State::new();
        state.insert(NODE_ERRORS_KEY.into(), json!([{"node": "first"}]));
        let mut updates = HashMap::new();
        updates.insert(NODE_ERRORS_KEY.into(), json!([{"node": "second"}]));

        let sanitized = sanitize_and_bound_state(&state, &updates);
        assert_eq!(
            sanitized[NODE_ERRORS_KEY],
            json!([{"node": "first"}, {"node": "second"}])
        );
    }
}
