//! The model-visible description of a failed remote MCP tool call (demo issue 3).
//!
//! Before this module, every MCP failure reached the model as
//! `tool.internal: tool execution failed`. A server that answered with an
//! `isError: true` result ("Repository not found ...") lost its explanation,
//! and the model could neither recover (try another repository) nor tell the
//! user why. The MCP specification puts a tool error result in front of the
//! model on purpose, so that it can correct itself.
//!
//! This module turns the ADK error into a bounded, redacted sentence:
//!
//! - An `isError: true` result keeps the server's own first text block.
//! - A JSON-RPC error from the server (`Mcp error: ...`) keeps its code and
//!   message.
//! - A transport failure keeps only a fixed phrase for its class. Transport
//!   errors carry endpoint URLs, header names and library internals, and none
//!   of that is the model's business.
//!
//! Every server-supplied text is cut to `MAX_DETAIL_CHARS`, has control
//! characters replaced, and has these values replaced with `[redacted]`: each
//! credential this toolkit sends (static header values, the delegated token),
//! the endpoint URL and host, URL user-info, `Bearer`/`Basic` credentials,
//! `key=value` pairs whose key names a secret, and well-known token formats.

use std::sync::LazyLock;

use adk_core::{AdkError, ErrorComponent, RetryHint};
use regex::Regex;
use zeroize::Zeroizing;

/// The code of an MCP failure whose message is safe for the model.
/// `invocation::sanitize_tool_error` keeps the message of this code only.
pub(crate) const MCP_TOOL_ERROR_RESULT_CODE: &str = "mcp.tool.error_result";

/// The most characters of server text that reach the model.
const MAX_DETAIL_CHARS: usize = 1_000;
/// A configured value shorter than this is not redacted by value: it would
/// match ordinary words. Pattern redaction still applies.
const MIN_REDACTED_VALUE_CHARS: usize = 6;
const REDACTED: &str = "[redacted]";

static PATTERNS: LazyLock<Vec<(Regex, &'static str)>> = LazyLock::new(|| {
    [
        // URL user-info: scheme://user:password@host
        (r"(?i)\b([a-z][a-z0-9+.\-]*://)[^\s/@]+@", "${1}[redacted]@"),
        // Authorization schemes followed by a credential.
        (r"(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=\-]{6,}", "$1 [redacted]"),
        // key=value / key: value / "key":"value" where the key names a secret.
        (
            r#"(?i)\b([a-z0-9_\-]*(?:api[_\-]?key|apikey|token|secret|password|passwd|pwd|authorization|signature|sig|credential|private[_\-]?key|session)[a-z0-9_\-]*)("?\s*[:=]\s*"?)[^\s"',;&}]+"#,
            "$1$2[redacted]",
        ),
        // Well-known token formats.
        (
            r"\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_\-]{20,}|xox[abposr]-[A-Za-z0-9\-]{10,}|sk-[A-Za-z0-9_\-]{20,}|AKIA[0-9A-Z]{16}|eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,})",
            REDACTED,
        ),
    ]
    .into_iter()
    // Static patterns: `server_text_is_redacted_and_bounded` proves each one
    // compiles, because a dropped pattern lets its secret through.
    .filter_map(|(pattern, replacement)| {
        Regex::new(pattern)
            .ok()
            .map(|pattern| (pattern, replacement))
    })
    .collect()
});

/// The values one MCP toolkit must never echo to the model.
#[derive(Default)]
pub(crate) struct McpErrorRedaction {
    values: Vec<Zeroizing<String>>,
}

impl McpErrorRedaction {
    /// `secrets` are the toolkit's credential values. `endpoint` is the
    /// configured server URL; the URL and its host are both redacted.
    pub(crate) fn new<'a>(secrets: impl IntoIterator<Item = &'a str>, endpoint: &str) -> Self {
        let mut values: Vec<Zeroizing<String>> = secrets
            .into_iter()
            .flat_map(|secret| {
                // A header value such as "Bearer abc" is also redacted by its
                // credential part alone.
                let credential = secret.split_once(' ').map(|(_, rest)| rest.trim());
                std::iter::once(secret.trim()).chain(credential)
            })
            .filter(|value| value.chars().count() >= MIN_REDACTED_VALUE_CHARS)
            .map(|value| Zeroizing::new(value.to_owned()))
            .collect();
        let endpoint = endpoint.trim();
        if !endpoint.is_empty() {
            values.push(Zeroizing::new(endpoint.trim_end_matches('/').to_owned()));
            if let Some(host) = endpoint
                .split_once("://")
                .map(|(_, rest)| rest.split(['/', '?', '#']).next().unwrap_or_default())
                .map(|authority| authority.rsplit('@').next().unwrap_or_default())
                .filter(|host| host.chars().count() >= 4)
            {
                values.push(Zeroizing::new(host.to_owned()));
            }
        }
        // Longest first, so a URL is replaced before its host.
        values.sort_by_key(|value| std::cmp::Reverse(value.len()));
        values.dedup();
        Self { values }
    }

    fn redact(&self, text: &str) -> String {
        let mut text = text.to_owned();
        for value in &self.values {
            if text.contains(value.as_str()) {
                text = text.replace(value.as_str(), REDACTED);
            }
        }
        for (pattern, replacement) in PATTERNS.iter() {
            text = pattern.replace_all(&text, *replacement).into_owned();
        }
        text
    }
}

/// Classify an ADK MCP tool error and build the error the model sees.
///
/// `tool_name` is the remote tool's own name, which the ADK puts in its
/// messages. The returned error never carries the ADK message verbatim.
pub(crate) fn model_visible_mcp_error(
    error: &AdkError,
    tool_name: &str,
    redaction: &McpErrorRedaction,
) -> AdkError {
    let result_prefix = format!("MCP tool '{tool_name}' execution failed");
    let call_prefix = format!("Failed to call MCP tool '{tool_name}': ");
    let message = if let Some(rest) = error.message.strip_prefix(&result_prefix) {
        // `isError: true`. The ADK appends ": <first text block>" when the
        // result has one (adk-tool 2.2.0, `McpTool::execute`).
        match bounded_detail(rest.strip_prefix(": ").unwrap_or_default(), redaction) {
            Some(detail) => format!("the remote MCP tool returned an error: {detail}"),
            None => "the remote MCP tool returned an error without a description".to_owned(),
        }
    } else if let Some(rest) = error
        .message
        .strip_prefix(&call_prefix)
        .and_then(|rest| rest.strip_prefix("Mcp error: "))
    {
        // A JSON-RPC error object from the server (rmcp `ServiceError::McpError`).
        match bounded_detail(rest, redaction) {
            Some(detail) => format!("the remote MCP server rejected the call: {detail}"),
            None => "the remote MCP server rejected the call".to_owned(),
        }
    } else if let Some(rest) = error.message.strip_prefix("Task execution failed: ") {
        // A task-capable tool (adk-tool 2.2.0 `McpTool::execute` -> `TaskError`).
        task_phrase(rest, redaction)
    } else if error.message.starts_with(&call_prefix)
        || error
            .message
            .contains("result is uncertain and was not replayed")
    {
        transport_phrase(&error.message).to_owned()
    } else if error.message == "MCP tool returned no content"
        || error.message.starts_with("Invalid MCP result from ")
        || error.message.contains("result invalid: ")
    {
        "the remote MCP tool returned an empty or unreadable result".to_owned()
    } else if error.message == "Tool arguments must be an object" {
        "the tool arguments must be a JSON object".to_owned()
    } else if error.message.contains("unresolved MRTR input request") {
        "the remote MCP tool asked for more input, which this runtime cannot supply".to_owned()
    } else {
        // An ADK message this module does not know. Its text may hold
        // internals, so only a neutral phrase passes.
        "the remote MCP tool failed".to_owned()
    };
    AdkError::new(
        ErrorComponent::Tool,
        error.category,
        MCP_TOOL_ERROR_RESULT_CODE,
        message,
    )
    .with_retry(RetryHint {
        should_retry: false,
        retry_after_ms: None,
        max_attempts: Some(1),
    })
}

/// The model-visible text of a failed MCP task (`TaskError` display). Only a
/// failed task's own error, which the server wrote, passes, through the same
/// redaction and bound as an `isError` result.
fn task_phrase(rest: &str, redaction: &McpErrorRedaction) -> String {
    if let Some((_, detail)) = rest
        .strip_prefix("Task '")
        .and_then(|rest| rest.split_once("' failed: "))
    {
        return match bounded_detail(detail, redaction) {
            Some(detail) => format!("the remote MCP tool returned an error: {detail}"),
            None => "the remote MCP tool returned an error without a description".to_owned(),
        };
    }
    if rest.starts_with("Task '") && rest.contains("' timed out after ") {
        "the remote MCP task did not finish in time".to_owned()
    } else if rest.starts_with("Task '") && rest.ends_with("' was cancelled") {
        "the remote MCP task was cancelled".to_owned()
    } else if rest.starts_with("Task '") && rest.contains("' requires input: ") {
        "the remote MCP tool asked for more input, which this runtime cannot supply".to_owned()
    } else {
        // Create or poll failures carry transport text.
        "the remote MCP task could not be completed".to_owned()
    }
}

/// A fixed phrase per transport failure class. No transport text passes.
fn transport_phrase(message: &str) -> &'static str {
    let lower = message.to_ascii_lowercase();
    if lower.contains("auth required")
        || lower.contains("authorization required")
        || lower.contains("auth error")
    {
        "the remote MCP server requires authorization (HTTP 401)"
    } else if lower.contains("insufficient scope") {
        "the remote MCP server refused the granted scope (HTTP 403)"
    } else if lower.contains("session expired") {
        "the remote MCP session expired (HTTP 404); try the call again"
    } else if lower.contains("request timeout") {
        "the remote MCP server did not answer in time"
    } else if lower.contains("uncertain and was not replayed") {
        "the connection to the remote MCP server was lost; the outcome of the call is unknown"
    } else if lower.contains("unexpected server response")
        || lower.contains("unexpected content type")
    {
        "the remote MCP server returned an unexpected response"
    } else {
        "the remote MCP server could not be reached"
    }
}

fn bounded_detail(text: &str, redaction: &McpErrorRedaction) -> Option<String> {
    // Redact before cutting, so a cut cannot split a secret and expose its head.
    let redacted = redaction.redact(text);
    let mut detail = String::with_capacity(redacted.len().min(MAX_DETAIL_CHARS + 3));
    let mut last_space = false;
    let mut count = 0_usize;
    let mut truncated = false;
    for c in redacted.chars() {
        let c = if c.is_control() { ' ' } else { c };
        if c == ' ' && last_space {
            continue;
        }
        if count == MAX_DETAIL_CHARS {
            truncated = true;
            break;
        }
        last_space = c == ' ';
        detail.push(c);
        count += 1;
    }
    let mut detail = detail.trim().to_owned();
    if detail.is_empty() {
        return None;
    }
    if truncated {
        detail.push_str("...");
    }
    Some(detail)
}

/// True when `error` is an MCP failure that `model_visible_mcp_error` built.
pub(crate) fn is_model_visible_mcp_error(error: &AdkError) -> bool {
    error.component == ErrorComponent::Tool && error.code == MCP_TOOL_ERROR_RESULT_CODE
}

#[cfg(test)]
#[path = "mcp_error_tests.rs"]
mod tests;
