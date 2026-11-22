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
use elitea_agent_runtime::graph::node_settings::{MAX_NODE_ERRORS, NODE_ERRORS_KEY};
use elitea_agent_runtime::memory_tools::looks_like_secret;
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
const MAX_SANITIZE_NODES: usize = 2_048;
const MAX_SECRET_SCAN_EXTRA_BYTES: usize = 1_024;
const REDACTED: &str = "[REDACTED]";
const TRUNCATED_SUFFIX: &str = "...[truncated]";

/// Normalized (alphanumeric-only, lowercase) key tokens matching `events.rs`
/// `sensitive_argument_key` so separator variants (`api-token`, `AuthSecret`,
/// `x_api_key`, `auth-header`) match consistently (SEC-5B1B-02).
const SENSITIVE_KEY_TOKENS: &[&str] = &[
    "password",
    "passwd",
    "secret",
    "token",
    "apikey",
    "authorization",
    "credential",
    "privatekey",
    "accesskey",
    "cookie",
    "bearer",
    "authheader",
];

/// Zero-allocation supplemental secret-prefix scanner on top of
/// `elitea_agent_runtime::memory_tools::looks_like_secret`, covering `Basic`/short
/// `Bearer` headers, GitLab PATs (`glpat-`), shorter GitHub/OpenAI prefixes, and
/// PEM key headers without allocating lowercased copies of input strings (SEC-5B1B-02).
fn contains_ascii_word_prefix_ci(text: &str, needle: &[u8]) -> bool {
    let bytes = text.as_bytes();
    if bytes.len() <= needle.len() {
        return false;
    }
    for i in 0..=bytes.len() - needle.len() {
        let before_ok = i == 0 || !bytes[i - 1].is_ascii_alphanumeric();
        if !before_ok {
            continue;
        }
        if bytes[i..i + needle.len()].eq_ignore_ascii_case(needle) {
            let rest = &bytes[i + needle.len()..];
            if rest.first().is_some_and(u8::is_ascii_whitespace)
                && rest.iter().any(|b| !b.is_ascii_whitespace())
            {
                return true;
            }
        }
    }
    false
}

fn has_extra_secret_marker(text: &str) -> bool {
    if contains_ascii_word_prefix_ci(text, b"bearer")
        || contains_ascii_word_prefix_ci(text, b"basic")
        || text.contains("-----BEGIN ")
        || text.contains("glpat-")
        || text.contains("sk-proj-")
        || text.contains("sk-ant-")
    {
        return true;
    }
    for prefix in ["ghp_", "gho_", "ghu_", "ghs_", "ghr_"] {
        let mut rest = text;
        while let Some(pos) = rest.find(prefix) {
            let tail = &rest[pos + prefix.len()..];
            let run = tail.bytes().take_while(u8::is_ascii_alphanumeric).count();
            if run >= 16 {
                return true;
            }
            rest = tail;
        }
    }
    false
}

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
            .map_or_else(
                || self.transition.clone(),
                |target| {
                    if target == END {
                        "END".to_owned()
                    } else {
                        target.clone()
                    }
                },
            );
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
    let normalized: String = key
        .chars()
        .filter(char::is_ascii_alphanumeric)
        .flat_map(char::to_lowercase)
        .collect();
    SENSITIVE_KEY_TOKENS
        .iter()
        .any(|token| normalized.contains(token))
}

fn has_credentialed_uri(text: &str) -> bool {
    let mut rest = text;
    while let Some(scheme_end) = rest.find("://") {
        let after = &rest[scheme_end + 3..];
        let authority = after
            .split(['/', '?', '#', ' ', '\t', '\r', '\n', '"', '\''])
            .next()
            .unwrap_or("");
        if let Some(at) = authority.rfind('@')
            && authority[..at].contains(':')
        {
            return true;
        }
        rest = after;
    }
    false
}

fn bounded_scan_slice(text: &str, max_str_bytes: usize) -> &str {
    let limit = max_str_bytes.saturating_add(MAX_SECRET_SCAN_EXTRA_BYTES);
    if text.len() <= limit {
        return text;
    }
    let mut end = limit;
    while end > 0 && !text.is_char_boundary(end) {
        end -= 1;
    }
    &text[..end]
}

fn looks_like_secret_value(text: &str, max_str_bytes: usize) -> bool {
    let slice = bounded_scan_slice(text, max_str_bytes).trim();
    if slice.is_empty() {
        return false;
    }
    looks_like_secret(slice) || has_credentialed_uri(slice) || has_extra_secret_marker(slice)
}

fn sanitize_value(
    value: &Value,
    depth: usize,
    max_str_bytes: usize,
    nodes_left: &mut usize,
) -> Option<Value> {
    if *nodes_left == 0 {
        return None;
    }
    *nodes_left -= 1;
    if depth >= MAX_REDACT_DEPTH {
        return Some(Value::String(TRUNCATED_SUFFIX.to_owned()));
    }
    match value {
        Value::Null | Value::Bool(_) | Value::Number(_) => Some(value.clone()),
        Value::String(text) => {
            if looks_like_secret_value(text, max_str_bytes) {
                Some(Value::String(REDACTED.to_owned()))
            } else {
                Some(Value::String(truncate_utf8(text, max_str_bytes)))
            }
        }
        Value::Array(items) => {
            if items.len() > *nodes_left {
                *nodes_left = 0;
                return None;
            }
            let mut out = Vec::with_capacity(items.len());
            for item in items {
                out.push(sanitize_value(item, depth + 1, max_str_bytes, nodes_left)?);
            }
            Some(Value::Array(out))
        }
        Value::Object(map) => {
            if map.len() > *nodes_left {
                *nodes_left = 0;
                return None;
            }
            let mut out = Map::with_capacity(map.len());
            for (k, v) in map {
                if is_omitted_state_key(k) {
                    if *nodes_left == 0 {
                        return None;
                    }
                    *nodes_left -= 1;
                    continue;
                }
                if is_sensitive_key(k) {
                    if *nodes_left == 0 {
                        return None;
                    }
                    *nodes_left -= 1;
                    out.insert(k.clone(), Value::String(REDACTED.to_owned()));
                } else {
                    out.insert(
                        k.clone(),
                        sanitize_value(v, depth + 1, max_str_bytes, nodes_left)?,
                    );
                }
            }
            Some(Value::Object(out))
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

fn try_sanitize_map(
    merged: &BTreeMap<String, Value>,
    max_str_bytes: usize,
) -> Option<Map<String, Value>> {
    let mut nodes_left = MAX_SANITIZE_NODES;
    if merged.len() > nodes_left {
        return None;
    }
    let mut candidate = Map::with_capacity(merged.len());
    for (k, v) in merged {
        if is_sensitive_key(k) {
            if nodes_left == 0 {
                return None;
            }
            nodes_left -= 1;
            candidate.insert(k.clone(), Value::String(REDACTED.to_owned()));
        } else {
            candidate.insert(
                k.clone(),
                sanitize_value(v, 0, max_str_bytes, &mut nodes_left)?,
            );
        }
    }
    Some(candidate)
}

/// Preserve the most recent sanitized `node_errors` entries that fit within
/// `MAX_TRANSITIONAL_EDGE_STATE_BYTES` in a single O(N) byte-accumulator pass.
fn bounded_node_errors_fallback(errors: &Value) -> Option<Value> {
    let Value::Array(entries) = errors else {
        return None;
    };
    let mut shell = Map::new();
    shell.insert("_truncated".to_owned(), Value::Bool(true));
    shell.insert(NODE_ERRORS_KEY.to_owned(), Value::Array(Vec::new()));
    let mut used_bytes = encoded_len(&shell);
    if used_bytes > MAX_TRANSITIONAL_EDGE_STATE_BYTES {
        return None;
    }
    let mut kept = Vec::new();
    for entry in entries.iter().rev().take(MAX_NODE_ERRORS) {
        let mut entry_budget = 64;
        let Some(sanitized) = sanitize_value(entry, 0, 128, &mut entry_budget) else {
            continue;
        };
        let Ok(entry_json) = serde_json::to_string(&sanitized) else {
            continue;
        };
        let separator = usize::from(!kept.is_empty());
        if used_bytes.saturating_add(separator + entry_json.len())
            > MAX_TRANSITIONAL_EDGE_STATE_BYTES
        {
            break;
        }
        used_bytes += separator + entry_json.len();
        kept.push(sanitized);
    }
    kept.reverse();
    Some(Value::Array(kept))
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
        let Some(candidate) = try_sanitize_map(&merged, max_str_bytes) else {
            // Node/element budget exceeded — short-circuit straight to bounded fallback.
            break;
        };
        if encoded_len(&candidate) <= MAX_TRANSITIONAL_EDGE_STATE_BYTES {
            return candidate;
        }
    }

    // State still exceeded the frame budget (e.g. very large arrays or many keys).
    // Preserve bounded `node_errors` (D12) and mark truncation explicitly.
    let mut minimal = Map::new();
    minimal.insert("_truncated".to_owned(), Value::Bool(true));
    if let Some(errors) = merged.get(NODE_ERRORS_KEY)
        && let Some(bounded) = bounded_node_errors_fallback(errors)
    {
        minimal.insert(NODE_ERRORS_KEY.to_owned(), bounded);
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
        let synthetic_token = format!("{}_{}", "ghp", "1234567890abcdef1234567890");
        updates.insert("gh_leak".into(), json!(synthetic_token));

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

    #[test]
    fn redacts_platform_tokens_separator_keys_and_later_credentialed_uris() {
        let mut state = State::new();
        state.insert("api-token".into(), json!("sep-token-value"));
        state.insert("AuthSecret".into(), json!("camel-secret-value"));
        state.insert("x_api_key".into(), json!("snake-key-value"));

        let native_tok = format!("{}_{}", "elnat", "0123456789abcdef0123456789");
        let gh_fine = format!("{}_{}", "github_pat", "0123456789abcdef0123456789");
        let gcp_key = format!("{}{}", "AIza", "0123456789abcdef0123456789abcdef0123");
        let multi_uri = "see https://docs.example.test/ok and https://bob:s3cr3t@db.internal/app";

        let mut updates = HashMap::new();
        updates.insert("native".into(), json!(native_tok));
        updates.insert("gh_fine".into(), json!(gh_fine));
        updates.insert("gcp".into(), json!(gcp_key));
        updates.insert("uris".into(), json!(multi_uri));

        let sanitized = sanitize_and_bound_state(&state, &updates);
        let serialized = serde_json::to_string(&sanitized).expect("json");
        for key in [
            "api-token",
            "AuthSecret",
            "x_api_key",
            "native",
            "gh_fine",
            "gcp",
            "uris",
        ] {
            assert_eq!(
                sanitized[key],
                json!(REDACTED),
                "key {key} must be redacted"
            );
        }
        assert!(!serialized.contains("sep-token-value"));
        assert!(!serialized.contains("camel-secret-value"));
        assert!(!serialized.contains("s3cr3t"));
        assert!(!serialized.contains("elnat_"));
        assert!(!serialized.contains("github_pat_"));
        assert!(!serialized.contains("AIza"));
    }

    #[test]
    fn short_circuits_oversized_numeric_array_and_bounds_64_node_errors() {
        let mut state = State::new();
        let big_numbers: Vec<Value> = (0..50_000).map(|i| json!(i)).collect();
        state.insert("numbers".into(), Value::Array(big_numbers));
        let errors: Vec<Value> = (0..64)
            .map(|i| {
                json!({
                    "node": format!("step_{i:02}"),
                    "handled": "continue",
                    "code": "pipeline.code.failed",
                    "message": format!("error entry {i:02} {}", "m".repeat(80))
                })
            })
            .collect();
        let mut updates = HashMap::new();
        updates.insert(NODE_ERRORS_KEY.into(), Value::Array(errors));

        let bounded = sanitize_and_bound_state(&state, &updates);
        let encoded = serde_json::to_string(&bounded).expect("json");
        assert!(encoded.len() <= MAX_TRANSITIONAL_EDGE_STATE_BYTES);
        assert_eq!(bounded["_truncated"], json!(true));
        assert!(!bounded.contains_key("numbers"));
        let kept = bounded[NODE_ERRORS_KEY]
            .as_array()
            .expect("node_errors array preserved");
        assert_ne!(kept.len(), 0);
        assert_eq!(kept.last().unwrap()["node"], json!("step_63"));
    }

    #[test]
    fn short_circuits_nested_objects_of_sensitive_and_omitted_keys_past_node_budget() {
        let mut items = Vec::with_capacity(64);
        for i in 0..64 {
            let mut obj = Map::with_capacity(64);
            for j in 0..32 {
                obj.insert(format!("secret_{i}_{j}"), json!("hidden"));
                obj.insert(format!("__elitea_internal_{i}_{j}"), json!("omitted"));
            }
            items.push(Value::Object(obj));
        }
        let mut state = State::new();
        state.insert("nested_many".into(), Value::Array(items));

        let bounded = sanitize_and_bound_state(&state, &HashMap::new());
        assert_eq!(bounded["_truncated"], json!(true));
        assert!(!bounded.contains_key("nested_many"));
    }
}
