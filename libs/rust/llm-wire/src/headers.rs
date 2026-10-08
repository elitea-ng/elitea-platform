//! The header names and value rules of a `/llm` request.
//!
//! Names are lowercase, as HTTP/2 sends them; header lookup is
//! case-insensitive, so the gateway's canonical `X-Project-Id` and
//! `X-Elitea-Execution-Id` are the same headers.

/// The billing project (ADR-0018's primary selector). A token bound to a
/// project gets 400 `project_scope_conflict` with any other.
pub const PROJECT_HEADER: &str = "x-project-id";

/// The run the gateway attributes the call's spend to; the `/llm` edge keeps
/// it only for a live execution of the same project and user. It must match
/// the Python worker's `_EXECUTION_ID_HEADER` and the gateway's
/// `headerExecutionID`.
pub const EXECUTION_HEADER: &str = "x-elitea-execution-id";

/// Headers a caller never sends: `openai-organization` is the edge's
/// fallback selector, `openai-project` is never a selector.
pub const ABSENT_HEADERS: [&str; 2] = ["openai-organization", "openai-project"];

/// The `Authorization` scheme and its separator: the value is
/// `Bearer <token>`, and the caller marks it sensitive.
pub const BEARER_PREFIX: &str = "Bearer ";

/// The longest execution id the edge keeps (`executionIDFromHeader`).
pub const MAX_EXECUTION_ID_BYTES: usize = 128;

/// The edge's shape rule for an execution id: 1–128 bytes of
/// `A-Za-z0-9-_.`. There is no `:`, because the evaluation namespace uses
/// it. The edge drops (never refuses) an id outside the rule.
#[must_use]
pub fn valid_execution_id(id: &str) -> bool {
    (1..=MAX_EXECUTION_ID_BYTES).contains(&id.len())
        && id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.'))
}

/// Text a header may carry without question: non-empty, at most
/// `max_bytes`, ASCII, and free of control characters. The worker holds a
/// model name, a tool name and a tool-call id to it (its execution id to
/// [`valid_execution_id`], #1156).
#[must_use]
pub fn bounded_header_text(value: &str, max_bytes: usize) -> bool {
    !value.is_empty()
        && value.len() <= max_bytes
        && value.is_ascii()
        && !value.bytes().any(|byte| byte.is_ascii_control())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_execution_id_rule() {
        assert!(valid_execution_id("0123456789abcdef0123456789abcdef"));
        assert!(valid_execution_id("callback-6f1c.x_y"));
        assert!(valid_execution_id(&"a".repeat(MAX_EXECUTION_ID_BYTES)));
        assert!(!valid_execution_id(&"a".repeat(MAX_EXECUTION_ID_BYTES + 1)));
        for invalid in ["", "a:b", "a b", "a/b", "é"] {
            assert!(!valid_execution_id(invalid), "{invalid}");
        }
    }

    #[test]
    fn bounded_header_text_refuses_empty_long_non_ascii_and_control() {
        assert!(bounded_header_text("execution/fixture-one", 256));
        assert!(bounded_header_text("abc", 3));
        assert!(!bounded_header_text("abcd", 3));
        assert!(!bounded_header_text("", 3));
        assert!(!bounded_header_text("é", 8));
        assert!(!bounded_header_text("a\nb", 8));
        assert!(!bounded_header_text("a\u{7f}", 8));
    }
}
