//! The category of one cloud browser call (ADR-0049 decision 3, ADR-0048
//! decision 8), from the call and the latest snapshot the run read.
//!
//! | Call | Category | Ask |
//! | --- | --- | --- |
//! | `BrowserRead`, `BrowserNavigate`, `browser_request_user` | `read` | none |
//! | any other `BrowserAct` | `write` | as the approval policy says for `write` |
//! | a submit: `browser_type` with `submit`, `browser_press` Enter, a click on a submit, upload or file control (a `file` role, or `file` in its name) | `external_send` | asked; a grant's `browser_submits` answers it for its hosts |
//! | an act on a control whose name has a consequential word (send, delete, sign, confirm …) | `external_send` | asked every time; no grant answers it |
//! | an act on a control whose name has a buying or paying word | `payment` | asked every time; no grant answers it |
//!
//! The classifier is pure: it reads the call's arguments and the snapshot
//! text, and nothing else. A ref it cannot resolve (no snapshot yet, a stale
//! ref) classifies the act as a plain `write`: the session refuses such a
//! call with `stale_ref`, so it cannot act on a control the classifier did
//! not see.
//!
//! The word lists match whole words of the control's role and name, split
//! at non-alphanumerics and CamelCase boundaries, ignoring case. A false
//! match costs a question; a missed one an unasked action, so the lists err
//! on the side of asking.

use elitea_browser_tools::call::Key;
use elitea_browser_tools::{BrowserCall, ElementRef, Snapshot, ToolKind};
use serde_json::Value;

use crate::tool_category::ToolCategory;

/// Words on a control that buys or pays: `payment`.
pub const PAYMENT_WORDS: &[&str] = &[
    "buy",
    "pay",
    "payment",
    "purchase",
    "checkout",
    "order",
    "subscribe",
    "donate",
    "withdraw",
    "transfer",
    "book",
];

/// Words on a control that sends, deletes or signs: `external_send`, asked
/// every time (ADR-0047 decision 4, ADR-0048 decision 8).
pub const CONSEQUENTIAL_WORDS: &[&str] = &[
    "send",
    "post",
    "share",
    "publish",
    "reply",
    "delete",
    "erase",
    "trash",
    "remove",
    "wipe",
    "reset",
    "sign",
    "confirm",
    "approve",
    "authorize",
    "authorise",
    "accept",
];

/// Words on a control that submits a form or starts an upload:
/// `external_send`, which a grant's `browser_submits` may answer.
pub const SUBMIT_WORDS: &[&str] = &["submit", "upload", "attach", "save", "apply", "continue"];

/// Roles that pick a file to upload.
const UPLOAD_ROLES: &[&str] = &["file", "fileupload"];

/// Words on a control that picks a file to upload ("Choose file"): a
/// submit, since the file leaves the run.
const UPLOAD_NAME_WORDS: &[&str] = &["file", "files"];

/// Why an act is not a plain `write`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Escalation {
    /// A form submit or an upload. A grant's `browser_submits` may answer
    /// the ask for the page's host.
    Submit,
    /// A send, delete or sign control. Asked every time.
    Consequential,
    /// A buy or pay control. Asked every time; withheld from unattended
    /// runs by the platform default.
    Payment,
}

/// One classified call.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Classified {
    pub kind: ToolKind,
    pub category: ToolCategory,
    pub escalation: Option<Escalation>,
}

impl Classified {
    const fn plain(kind: ToolKind) -> Self {
        let category = match kind {
            ToolKind::BrowserAct => ToolCategory::Write,
            ToolKind::BrowserRead | ToolKind::BrowserNavigate | ToolKind::Question => {
                ToolCategory::Read
            }
        };
        Self {
            kind,
            category,
            escalation: None,
        }
    }

    const fn escalated(escalation: Escalation) -> Self {
        Self {
            kind: ToolKind::BrowserAct,
            category: match escalation {
                Escalation::Payment => ToolCategory::Payment,
                Escalation::Submit | Escalation::Consequential => ToolCategory::ExternalSend,
            },
            escalation: Some(escalation),
        }
    }

    /// Whether a grant may answer the ask: only a submit.
    #[must_use]
    pub const fn grantable(&self) -> bool {
        matches!(self.escalation, Some(Escalation::Submit))
    }
}

/// The category of a call of `name` with `arguments`, against the latest
/// snapshot the run read (`snapshot`, its rendered text). A call that does
/// not parse is `write` (it never reaches the page).
#[must_use]
pub fn classify(name: &str, arguments: &Value, snapshot: Option<&Snapshot>) -> Classified {
    let Ok(call) = BrowserCall::parse(name, arguments) else {
        return Classified::plain(ToolKind::BrowserAct);
    };
    let kind = call.kind();
    if kind != ToolKind::BrowserAct {
        return Classified::plain(kind);
    }
    let control = target(&call).and_then(|target| control(snapshot, target));
    if let Some(escalation) = control.and_then(|(role, name)| control_escalation(role, name)) {
        return Classified::escalated(escalation);
    }
    let submits = match &call {
        BrowserCall::Type { submit, .. } => *submit,
        BrowserCall::Press(key) => is_enter(key),
        _ => false,
    };
    if submits {
        return Classified::escalated(Escalation::Submit);
    }
    Classified::plain(kind)
}

fn target(call: &BrowserCall) -> Option<&ElementRef> {
    match call {
        BrowserCall::Click { target, .. }
        | BrowserCall::Type { target, .. }
        | BrowserCall::Select { target, .. } => Some(target),
        _ => None,
    }
}

fn control<'a>(snapshot: Option<&'a Snapshot>, target: &ElementRef) -> Option<(&'a str, &'a str)> {
    let node = snapshot?.resolve(target).ok()?;
    Some((node.role.as_str(), node.name.as_str()))
}

fn is_enter(key: &Key) -> bool {
    key.key == "Enter" && key.modifiers.is_empty()
}

/// The escalation a control's role and name ask for, strongest first.
#[must_use]
pub fn control_escalation(role: &str, name: &str) -> Option<Escalation> {
    let words = || {
        name.split(|c: char| !c.is_alphanumeric())
            .flat_map(camel_words)
    };
    let has = |list: &[&str]| words().any(|word| list.iter().any(|w| word.eq_ignore_ascii_case(w)));
    if has(PAYMENT_WORDS) {
        Some(Escalation::Payment)
    } else if has(CONSEQUENTIAL_WORDS) {
        Some(Escalation::Consequential)
    } else if has(SUBMIT_WORDS)
        || has(UPLOAD_NAME_WORDS)
        || UPLOAD_ROLES
            .iter()
            .any(|upload| role.eq_ignore_ascii_case(upload))
    {
        Some(Escalation::Submit)
    } else {
        None
    }
}

/// `word` split at its CamelCase boundaries: before an upper-case letter
/// that follows a lower-case letter or a digit (`SendNow`: `Send`, `Now`),
/// and before the last capital of a run followed by a lower-case letter
/// (`URLSend`: `URL`, `Send`). Empty pieces are left out.
fn camel_words(word: &str) -> Vec<&str> {
    let chars: Vec<(usize, char)> = word.char_indices().collect();
    let mut words = Vec::new();
    let mut start = 0;
    for (i, &(at, c)) in chars.iter().enumerate().skip(1) {
        let prev = chars[i - 1].1;
        let next_lower = chars.get(i + 1).is_some_and(|&(_, n)| n.is_lowercase());
        if c.is_uppercase()
            && (prev.is_lowercase() || prev.is_numeric() || (prev.is_uppercase() && next_lower))
        {
            words.push(&word[start..at]);
            start = at;
        }
    }
    words.push(&word[start..]);
    words.retain(|piece| !piece.is_empty());
    words
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;

    const PAGE: &str = "# snapshot s1\n# url \"https://shop.example/cart\"\n# title \"Cart\"\n\
- textbox \"Search\" [ref=e1]\n\
- button \"Place order\" [ref=e2]\n\
- button \"Delete account\" [ref=e3]\n\
- button \"Submit\" [ref=e4]\n\
- button \"Next page\" [ref=e5]\n\
- link \"SendNow\" [ref=e6]\n\
- button \"Choose file\" [ref=e7]\n\
- checkbox \"Remember me\" [ref=e8]\n\
- button \"Sign in\" [ref=e9]\n";

    fn page() -> Snapshot {
        Snapshot::parse(PAGE).expect("snapshot")
    }

    fn of(name: &str, arguments: Value) -> Classified {
        classify(name, &arguments, Some(&page()))
    }

    #[test]
    fn reads_and_navigations_are_read() {
        for (name, arguments) in [
            ("browser_snapshot", json!({})),
            ("browser_screenshot", json!({})),
            ("browser_wait", json!({"text": "Done"})),
            ("browser_open", json!({"url": "https://example.com"})),
            ("browser_back", json!({})),
            ("browser_forward", json!({})),
            ("browser_reload", json!({})),
            ("browser_request_user", json!({"reason": "sign in"})),
        ] {
            let classified = of(name, arguments);
            assert_eq!(classified.category, ToolCategory::Read, "{name}");
            assert_eq!(classified.escalation, None, "{name}");
        }
    }

    #[test]
    fn a_plain_act_is_write() {
        for (name, arguments) in [
            ("browser_click", json!({"ref": "e5"})),
            ("browser_click", json!({"ref": "e8"})),
            ("browser_type", json!({"ref": "e1", "text": "shoes"})),
            ("browser_scroll", json!({"direction": "down"})),
            ("browser_press", json!({"key": "Tab"})),
        ] {
            let classified = of(name, arguments.clone());
            assert_eq!(
                classified.category,
                ToolCategory::Write,
                "{name} {arguments}"
            );
            assert_eq!(classified.escalation, None);
        }
    }

    #[test]
    fn a_submit_is_external_send_and_grantable() {
        for (name, arguments) in [
            (
                "browser_type",
                json!({"ref": "e1", "text": "shoes", "submit": true}),
            ),
            ("browser_press", json!({"key": "Enter"})),
            ("browser_click", json!({"ref": "e4"})),
            ("browser_click", json!({"ref": "e7"})),
        ] {
            let classified = of(name, arguments.clone());
            assert_eq!(
                classified.category,
                ToolCategory::ExternalSend,
                "{arguments}"
            );
            assert_eq!(classified.escalation, Some(Escalation::Submit));
            assert!(classified.grantable());
        }
        // Shift+Enter is a new line, not a submit.
        assert_eq!(
            of("browser_press", json!({"key": "Shift+Enter"})).category,
            ToolCategory::Write
        );
    }

    #[test]
    fn a_consequential_control_is_never_grantable() {
        for target in ["e3", "e6", "e9"] {
            let classified = of("browser_click", json!({"ref": target}));
            assert_eq!(classified.category, ToolCategory::ExternalSend, "{target}");
            assert_eq!(classified.escalation, Some(Escalation::Consequential));
            assert!(!classified.grantable());
        }
    }

    #[test]
    fn a_buying_control_is_payment() {
        let classified = of("browser_click", json!({"ref": "e2"}));
        assert_eq!(classified.category, ToolCategory::Payment);
        assert!(!classified.grantable());
        // Typing with submit into a field named for payment is payment too.
        assert_eq!(
            control_escalation("button", "Pay now"),
            Some(Escalation::Payment)
        );
        assert_eq!(
            control_escalation("button", "CheckoutButton"),
            Some(Escalation::Payment)
        );
    }

    #[test]
    fn an_unresolved_ref_is_a_plain_write() {
        let classified = classify("browser_click", &json!({"ref": "e2"}), None);
        assert_eq!(classified.category, ToolCategory::Write);
        let stale = classify("browser_click", &json!({"ref": "@s9:e2"}), Some(&page()));
        assert_eq!(stale.category, ToolCategory::Write);
    }

    #[test]
    fn an_unparsed_call_is_write() {
        assert_eq!(
            classify("browser_click", &json!({"ref": "nope"}), None).category,
            ToolCategory::Write
        );
        assert_eq!(
            classify("unknown", &json!({}), None).category,
            ToolCategory::Write
        );
    }

    #[test]
    fn words_are_whole_words() {
        assert_eq!(control_escalation("button", "Postcode lookup"), None);
        assert_eq!(control_escalation("button", "Signal strength"), None);
        assert_eq!(control_escalation("button", "Bookmarks"), None);
        assert_eq!(
            control_escalation("button", "URLSend"),
            Some(Escalation::Consequential)
        );
    }
}
