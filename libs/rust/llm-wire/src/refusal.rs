//! A gateway refusal: its stable code from the status (and, for 402, the
//! budget scope in the body), its retry hint, and the bounded detail a
//! caller may keep from the body.
//!
//! The codes are the worker's (`model_gateway.*`); the engines record them
//! on their request span, so the two callers report a refusal alike
//! (`conformance/llm-caller/contract.json`, `refusals`).
//!
//! The body is the gateway's `{"error": {"message", "type", "code",
//! "scope"?}}`, or whatever a provider sent through it. It is read only
//! here, by value lookups: never echoed whole, never formatted into an
//! error without [`redact_and_cut`] or [`single_line`].

use serde_json::Value;

/// Which ceiling a 402 names. Only a `budget_exceeded` error is a budget
/// refusal of this platform; its `scope` decides, and an older gateway that
/// sends no scope is known by `code: member_budget_exceeded`. A provider's
/// own quota refusal (no scope, or another scope) is
/// [`BudgetScope::Unscoped`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum BudgetScope {
    Member,
    Project,
    Unscoped,
}

/// The scope of a 402 body (see [`BudgetScope`]). A body that is not JSON,
/// or not of that shape, is [`BudgetScope::Unscoped`].
#[must_use]
pub fn budget_scope(body: &[u8]) -> BudgetScope {
    let scope = || -> Option<BudgetScope> {
        let value: Value = serde_json::from_slice(body).ok()?;
        let error = value.get("error")?;
        if error.get("type")?.as_str()? != "budget_exceeded" {
            return None;
        }
        match (
            error.get("scope").and_then(Value::as_str),
            error.get("code").and_then(Value::as_str),
        ) {
            (Some("member"), _) | (None, Some("member_budget_exceeded")) => {
                Some(BudgetScope::Member)
            }
            (Some("project"), _) => Some(BudgetScope::Project),
            _ => None,
        }
    };
    scope().unwrap_or(BudgetScope::Unscoped)
}

/// What a non-success status means to a caller.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Refusal {
    /// 401: the credential was refused.
    Unauthorized,
    /// 403: the project was refused.
    Forbidden,
    /// 408, 504.
    UpstreamTimeout,
    /// 429.
    RateLimited,
    /// 402, with the scope the body names.
    BudgetExhausted(BudgetScope),
    /// 409: a retryable conflict.
    Conflict,
    /// 3xx and 5xx.
    Unavailable,
    /// Any other status that is not a success (400, 404, 413, 422, …).
    Rejected,
}

impl Refusal {
    /// The refusal a status names, without reading the body: a 402 is
    /// [`BudgetScope::Unscoped`]. `None` for a success (2xx).
    #[must_use]
    pub const fn from_status(status: u16) -> Option<Self> {
        Some(match status {
            200..=299 => return None,
            401 => Self::Unauthorized,
            403 => Self::Forbidden,
            408 | 504 => Self::UpstreamTimeout,
            429 => Self::RateLimited,
            402 => Self::BudgetExhausted(BudgetScope::Unscoped),
            409 => Self::Conflict,
            300..=399 | 500..=599 => Self::Unavailable,
            _ => Self::Rejected,
        })
    }

    /// [`Refusal::from_status`], with a 402's scope read from `body`.
    #[must_use]
    pub fn classify(status: u16, body: &[u8]) -> Option<Self> {
        match Self::from_status(status)? {
            Self::BudgetExhausted(_) => Some(Self::BudgetExhausted(budget_scope(body))),
            other => Some(other),
        }
    }

    /// The stable code (`model_gateway.*`).
    #[must_use]
    pub const fn code(self) -> &'static str {
        match self {
            Self::Unauthorized => "model_gateway.unauthorized",
            Self::Forbidden => "model_gateway.forbidden",
            Self::UpstreamTimeout => "model_gateway.upstream_timeout",
            Self::RateLimited => "model_gateway.rate_limited",
            Self::BudgetExhausted(BudgetScope::Member) => "model_gateway.member_budget_exhausted",
            Self::BudgetExhausted(BudgetScope::Project) => "model_gateway.project_budget_exhausted",
            Self::BudgetExhausted(BudgetScope::Unscoped) => "model_gateway.budget_exhausted",
            Self::Conflict => "model_gateway.conflict",
            Self::Unavailable => "model_gateway.unavailable",
            Self::Rejected => "model_gateway.rejected",
        }
    }
}

/// The code of a gateway refusal with this status and body. A success
/// status has no refusal and reads as `model_gateway.rejected`, as an
/// unexpected answer would.
#[must_use]
pub fn refusal_code(status: u16, body: &[u8]) -> &'static str {
    Refusal::classify(status, body)
        .unwrap_or(Refusal::Rejected)
        .code()
}

/// Whether a refusal with this status is worth another attempt: 408, 409,
/// 429 and 5xx, as the contract's `retryable` says.
///
/// A 3xx is `model_gateway.unavailable` but NOT retryable here: the engines
/// refuse redirects, so a 3xx is final for them. The worker derives its
/// retry from its error category and files 3xx as retryable — a known
/// difference (`llm-caller-contract.md`, "Known differences").
#[must_use]
pub const fn retryable_status(status: u16) -> bool {
    matches!(status, 408 | 409 | 429 | 500..=599)
}

/// A client-error status that is the generic [`Refusal::Rejected`] (400,
/// 404, 413, 422, …): the refusals whose upstream detail is worth logging.
#[must_use]
pub const fn is_generic_rejection(status: u16) -> bool {
    matches!(status, 400..=499) && matches!(Refusal::from_status(status), Some(Refusal::Rejected))
}

/// Where a refusal body may carry its message.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum MessageShapes {
    /// `{"error": {"message": "…"}}` or `{"error": "…"}` (the worker).
    ErrorOnly,
    /// Those, then `{"message": "…"}` and `{"detail": "…"}` (the model
    /// client: `OpenAI`-style, and `FastAPI`-style bodies).
    Common,
}

/// The message inside a refusal body, unscrubbed. The first of the shapes'
/// fields that is PRESENT decides: a present field that is not a string is
/// no message, and later fields are not tried.
#[must_use]
pub fn upstream_message(body: &[u8], shapes: MessageShapes) -> Option<String> {
    let value: Value = serde_json::from_slice(body).ok()?;
    let error = value.get("error");
    let first = error
        .and_then(|e| e.get("message"))
        .or(error.filter(|e| e.is_string()));
    let found = match shapes {
        MessageShapes::ErrorOnly => first,
        MessageShapes::Common => first
            .or_else(|| value.get("message"))
            .or_else(|| value.get("detail")),
    };
    found.and_then(Value::as_str).map(str::to_owned)
}

/// One line for an operator log: control characters become spaces, the
/// text is cut to `max_chars` characters and trimmed. `None` when nothing
/// is left.
#[must_use]
pub fn single_line(message: &str, max_chars: usize) -> Option<String> {
    let detail: String = message
        .chars()
        .map(|c| if c.is_control() { ' ' } else { c })
        .take(max_chars)
        .collect();
    let detail = detail.trim().to_owned();
    (!detail.is_empty()).then_some(detail)
}

/// Replace every occurrence of `secret` (when it is not empty) with
/// `<redacted>`, then cut to `max_chars` characters, marking a cut with
/// `…`. A misconfigured upstream can echo request headers, so text the
/// gateway sent back passes through this before it reaches an error.
#[must_use]
pub fn redact_and_cut(text: &str, secret: &str, max_chars: usize) -> String {
    let cleaned = if secret.is_empty() {
        text.to_owned()
    } else {
        text.replace(secret, "<redacted>")
    };
    let mut cut: String = cleaned.chars().take(max_chars).collect();
    if cut.len() < cleaned.len() {
        cut.push('…');
    }
    cut
}

/// How much of the upstream message a caller keeps.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Detail<'a> {
    /// None: the caller's error is data-free and nothing is logged.
    Omit,
    /// [`single_line`], for an operator log line.
    SingleLine { max_chars: usize },
    /// [`redact_and_cut`], for an error message a user may read.
    Redacted { secret: &'a str, max_chars: usize },
}

/// The detail of a refusal body under a [`Detail`] policy.
#[must_use]
pub fn rejection_detail(body: &[u8], shapes: MessageShapes, detail: Detail<'_>) -> Option<String> {
    match detail {
        Detail::Omit => None,
        Detail::SingleLine { max_chars } => {
            single_line(&upstream_message(body, shapes)?, max_chars)
        }
        Detail::Redacted { secret, max_chars } => Some(redact_and_cut(
            &upstream_message(body, shapes)?,
            secret,
            max_chars,
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn statuses_map_to_their_refusals() {
        for status in [200, 201, 204, 299] {
            assert_eq!(Refusal::from_status(status), None, "{status}");
        }
        let code = |status| refusal_code(status, b"{}");
        assert_eq!(code(401), "model_gateway.unauthorized");
        assert_eq!(code(403), "model_gateway.forbidden");
        assert_eq!(code(408), "model_gateway.upstream_timeout");
        assert_eq!(code(504), "model_gateway.upstream_timeout");
        assert_eq!(code(429), "model_gateway.rate_limited");
        assert_eq!(code(402), "model_gateway.budget_exhausted");
        assert_eq!(code(409), "model_gateway.conflict");
        assert_eq!(code(302), "model_gateway.unavailable");
        assert_eq!(code(500), "model_gateway.unavailable");
        assert_eq!(code(503), "model_gateway.unavailable");
        assert_eq!(code(400), "model_gateway.rejected");
        assert_eq!(code(404), "model_gateway.rejected");
        assert_eq!(code(100), "model_gateway.rejected");
        assert_eq!(code(600), "model_gateway.rejected");
        assert_eq!(code(200), "model_gateway.rejected");
    }

    #[test]
    fn the_retry_hint_and_the_generic_rejection() {
        for status in [408, 409, 429, 500, 502, 503, 504, 599] {
            assert!(retryable_status(status), "{status}");
        }
        for status in [200, 302, 400, 401, 402, 403, 404, 422] {
            assert!(!retryable_status(status), "{status}");
        }
        for status in [400, 404, 405, 413, 422, 451, 499] {
            assert!(is_generic_rejection(status), "{status}");
        }
        for status in [200, 302, 401, 402, 403, 408, 409, 429, 500, 600] {
            assert!(!is_generic_rejection(status), "{status}");
        }
    }

    #[test]
    fn a_budget_body_names_its_scope() {
        let scope = |body: &str| budget_scope(body.as_bytes());
        assert_eq!(
            scope(r#"{"error":{"type":"budget_exceeded","scope":"member"}}"#),
            BudgetScope::Member
        );
        assert_eq!(
            scope(r#"{"error":{"type":"budget_exceeded","code":"member_budget_exceeded"}}"#),
            BudgetScope::Member
        );
        assert_eq!(
            scope(
                r#"{"error":{"type":"budget_exceeded","scope":"project","code":"member_budget_exceeded"}}"#
            ),
            BudgetScope::Project
        );
        assert_eq!(
            scope(r#"{"error":{"type":"budget_exceeded","scope":"provider"}}"#),
            BudgetScope::Unscoped
        );
        assert_eq!(
            scope(r#"{"error":{"type":"insufficient_quota","scope":"member"}}"#),
            BudgetScope::Unscoped
        );
        assert_eq!(scope("not json"), BudgetScope::Unscoped);
        // Key order does not matter.
        assert_eq!(
            scope(r#"{"error":{"scope":"project","type":"budget_exceeded"}}"#),
            BudgetScope::Project
        );
        assert_eq!(
            refusal_code(
                402,
                br#"{"error":{"type":"budget_exceeded","scope":"project"}}"#
            ),
            "model_gateway.project_budget_exhausted"
        );
    }

    #[test]
    fn upstream_messages_take_the_shapes_asked_for() {
        let common = |body: &[u8]| upstream_message(body, MessageShapes::Common);
        let error_only = |body: &[u8]| upstream_message(body, MessageShapes::ErrorOnly);
        assert_eq!(
            common(br#"{"error":{"message":"a"}}"#).as_deref(),
            Some("a")
        );
        assert_eq!(common(br#"{"error":"b"}"#).as_deref(), Some("b"));
        assert_eq!(common(br#"{"message":"m"}"#).as_deref(), Some("m"));
        assert_eq!(common(br#"{"detail":"c"}"#).as_deref(), Some("c"));
        assert_eq!(common(b"<html>"), None);
        assert_eq!(error_only(br#"{"detail":"c"}"#), None);
        assert_eq!(error_only(br#"{"error":"b"}"#).as_deref(), Some("b"));
        // A present message that is not text is no message.
        assert_eq!(common(br#"{"error":{"message":1},"detail":"c"}"#), None);
        assert_eq!(error_only(br#"{"error":{"message":1}}"#), None);
    }

    #[test]
    fn detail_is_bounded_scrubbed_or_omitted() {
        let body = br#"{"error":{"message":"  System message\nmust be first. key sk-1  "}}"#;
        assert_eq!(
            rejection_detail(body, MessageShapes::ErrorOnly, Detail::Omit),
            None
        );
        assert_eq!(
            rejection_detail(
                body,
                MessageShapes::ErrorOnly,
                Detail::SingleLine { max_chars: 240 }
            )
            .as_deref(),
            Some("System message must be first. key sk-1")
        );
        assert_eq!(
            rejection_detail(
                body,
                MessageShapes::Common,
                Detail::Redacted {
                    secret: "sk-1",
                    max_chars: 300
                }
            )
            .as_deref(),
            Some("  System message\nmust be first. key <redacted>  ")
        );
        assert_eq!(single_line("   ", 10), None);
        assert_eq!(
            single_line(&"x".repeat(500), 240).map(|d| d.len()),
            Some(240)
        );
        let cut = redact_and_cut(&"y".repeat(5000), "", 300);
        assert_eq!(cut.chars().count(), 301);
        assert!(cut.ends_with('…'));
        assert_eq!(redact_and_cut("short", "", 300), "short");
        // An empty message is kept as empty under the redacted policy.
        assert_eq!(
            rejection_detail(
                br#"{"error":{"message":""}}"#,
                MessageShapes::Common,
                Detail::Redacted {
                    secret: "k",
                    max_chars: 300
                }
            )
            .as_deref(),
            Some("")
        );
    }
}
