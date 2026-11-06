//! The category of one cloud browser call (ADR-0049 decision 3, ADR-0048
//! decision 8), from the call and the latest snapshot the run read.
//!
//! | Call | Category | Ask |
//! | --- | --- | --- |
//! | `BrowserRead`, `BrowserNavigate`, `browser_request_user` | `read` | none |
//! | any other `BrowserAct` | `write` | as the approval policy says for `write` |
//! | a submit: `browser_type` with `submit`, a click on a submit, upload or file control (a `file` role, or `file` in the name of a control that is not a link), Enter in a text field (Shift+Enter is a new line) or with nothing focused | `external_send` | asked; a grant's `browser_submits` answers it for its hosts |
//! | `browser_press` Enter or Space on a focused control: as a click on that control (its words decide) | as that click | as that click |
//! | `browser_press` Enter or Space, with or without modifiers, when the focused element is unknown ([`Focus::Unknown`]) | `external_send` | asked every time; no grant answers it |
//! | an act on a control whose name has a consequential word (send, delete, sign, confirm …) | `external_send` | asked every time; no grant answers it |
//! | an act on a control whose name has a buying or paying word | `payment` | asked every time; no grant answers it |
//! | a click, type or select on a ref the classifier cannot resolve (no snapshot, a stale or unknown ref) | `external_send` | asked every time; no grant answers it |
//!
//! The classifier is pure: it reads the call's arguments, the snapshot
//! text and, for a press, the focused element ([`Focus`]), and nothing
//! else. A host that can read the page's focus right before the press (the
//! desktop pane's driver) passes it to [`classify_with_focus`]; the cloud
//! has only the latest snapshot, whose `[focused]` node is the focus
//! ([`Focus::of_snapshot`]) and which the run forgets after every act or
//! navigation. A press of Enter or Space on a focus the classifier cannot
//! see is [`Escalation::Unresolved`]: the key may submit, buy or send. It fails closed: a ref it cannot resolve is a
//! control it did not see, so the act is [`Escalation::Unresolved`] and asks,
//! never a plain `write`. The session may still hold that ref (the driver
//! keeps refs valid within one document), so the label the classifier lacks
//! could be "Place order".
//!
//! The word lists match whole words of the control's role and name, split
//! at non-alphanumerics and CamelCase boundaries, ignoring case. A false
//! match costs a question; a missed one an unasked action, so the lists err
//! on the side of asking.

use elitea_browser_tools::call::{Key, Modifier};
use elitea_browser_tools::snapshot::State;
use elitea_browser_tools::{BrowserCall, ElementRef, Node, Snapshot, ToolKind};
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
    /// An act on a ref the classifier cannot resolve against the latest
    /// snapshot it holds (none yet, or a stale or unknown ref). The control
    /// is unknown, so it may be any of the above: asked every time, and no
    /// grant answers it.
    Unresolved,
}

/// The element a `browser_press` goes to, as the host knows it.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Focus<'a> {
    /// The host could not read it (no snapshot, no `[focused]` node, a
    /// driver error, a frame out of reach).
    Unknown,
    /// Nothing has the focus (the page's body).
    Nothing,
    /// The focused control: its role and accessible name.
    Control { role: &'a str, name: &'a str },
}

impl<'a> Focus<'a> {
    /// The `[focused]` node of the latest snapshot, or [`Self::Unknown`]
    /// (no snapshot, none marked, or more than one).
    #[must_use]
    pub fn of_snapshot(snapshot: Option<&'a Snapshot>) -> Self {
        let Some(snapshot) = snapshot else {
            return Self::Unknown;
        };
        let mut found = None;
        let mut count = 0usize;
        visit(&snapshot.nodes, &mut |node| {
            if node.states.contains(&State::Focused) {
                count += 1;
                found = Some(node);
            }
        });
        match found {
            Some(node) if count == 1 => Self::Control {
                role: node.role.as_str(),
                name: node.name.as_str(),
            },
            _ => Self::Unknown,
        }
    }
}

fn visit<'a>(nodes: &'a [Node], each: &mut impl FnMut(&'a Node)) {
    for node in nodes {
        each(node);
        visit(&node.children, each);
    }
}

/// Roles that take typed text: Enter there submits the form.
const TEXT_ROLES: &[&str] = &["textbox", "searchbox", "combobox", "spinbutton"];

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
                Escalation::Submit | Escalation::Consequential | Escalation::Unresolved => {
                    ToolCategory::ExternalSend
                }
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
/// snapshot the run read (`snapshot`, its rendered text); a press goes to
/// the snapshot's `[focused]` node ([`Focus::of_snapshot`]). A call that
/// does not parse is `write` (it never reaches the page).
#[must_use]
pub fn classify(name: &str, arguments: &Value, snapshot: Option<&Snapshot>) -> Classified {
    classify_with_focus(name, arguments, snapshot, Focus::of_snapshot(snapshot))
}

/// [`classify`] with the focused element the host read itself, for a
/// `browser_press`.
#[must_use]
pub fn classify_with_focus(
    name: &str,
    arguments: &Value,
    snapshot: Option<&Snapshot>,
    focus: Focus<'_>,
) -> Classified {
    let Ok(call) = BrowserCall::parse(name, arguments) else {
        return Classified::plain(ToolKind::BrowserAct);
    };
    let kind = call.kind();
    if kind != ToolKind::BrowserAct {
        return Classified::plain(kind);
    }
    if let Some(target) = target(&call) {
        let Some((role, name)) = control(snapshot, target) else {
            // Fail closed: the session may still act on this ref.
            return Classified::escalated(Escalation::Unresolved);
        };
        if let Some(escalation) = control_escalation(role, name) {
            return Classified::escalated(escalation);
        }
    }
    let escalation = match &call {
        BrowserCall::Type { submit: true, .. } => Some(Escalation::Submit),
        BrowserCall::Press(key) => press_escalation(key, focus),
        _ => None,
    };
    escalation.map_or_else(|| Classified::plain(kind), Classified::escalated)
}

/// What a key press would do: Enter and Space act on the focused control
/// (Space clicks a button, Enter follows a link or submits a field's form);
/// any other key only moves or types. Fails closed on an unknown focus.
#[must_use]
pub fn press_escalation(key: &Key, focus: Focus<'_>) -> Option<Escalation> {
    let enter = key.key == "Enter";
    if !enter && !is_space(key) {
        return None;
    }
    match focus {
        Focus::Unknown => Some(Escalation::Unresolved),
        Focus::Nothing => enter.then_some(Escalation::Submit),
        Focus::Control { role, name } => {
            if let Some(escalation) = control_escalation(role, name) {
                return Some(escalation);
            }
            let text = TEXT_ROLES
                .iter()
                .any(|text| role.eq_ignore_ascii_case(text));
            // Enter in a text field submits its form; Shift+Enter is a new
            // line. Ctrl, Alt or Meta+Enter sends in many editors.
            (enter && text && key.modifiers != [Modifier::Shift]).then_some(Escalation::Submit)
        }
    }
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

fn is_space(key: &Key) -> bool {
    key.key == "Space" || key.key == " "
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
        // "file" on a link names a page or a download, never a file
        // chooser: a link uploads nothing.
        || (has(UPLOAD_NAME_WORDS) && !role.eq_ignore_ascii_case("link"))
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

    #[allow(
        clippy::needless_pass_by_value,
        reason = "the tests pass json! literals"
    )]
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
        // Enter in the focused text field submits its form; Shift+Enter is
        // a new line.
        let enter = Key::parse("Enter").expect("key");
        let field = Focus::Control {
            role: "textbox",
            name: "Search",
        };
        assert_eq!(press_escalation(&enter, field), Some(Escalation::Submit));
        assert_eq!(
            press_escalation(&Key::parse("Shift+Enter").expect("key"), field),
            None
        );
        assert_eq!(
            press_escalation(&enter, Focus::Nothing),
            Some(Escalation::Submit)
        );
    }

    /// Finding 1 (ADR-0048 decision 8): a key press is classified against
    /// the control that has the focus, like a click on it.
    #[test]
    fn a_press_is_a_click_on_the_focused_control() {
        let keys = ["Enter", "Space", "Control+Enter", "Shift+Space"];
        for (role, name, expected) in [
            ("button", "Place order", Escalation::Payment),
            ("button", "Delete account", Escalation::Consequential),
            ("link", "SendNow", Escalation::Consequential),
            ("button", "Submit", Escalation::Submit),
        ] {
            for key in keys {
                let key = Key::parse(key).expect("key");
                assert_eq!(
                    press_escalation(&key, Focus::Control { role, name }),
                    Some(expected),
                    "{key:?} on {name}"
                );
            }
        }
        // Other keys only move or type, whatever has the focus.
        for key in ["Tab", "ArrowDown", "a", "Escape"] {
            let key = Key::parse(key).expect("key");
            let pay = Focus::Control {
                role: "button",
                name: "Pay now",
            };
            assert_eq!(press_escalation(&key, pay), None, "{key:?}");
            assert_eq!(press_escalation(&key, Focus::Unknown), None, "{key:?}");
        }
        // A plain control: Space or Enter is a plain write.
        let next = Focus::Control {
            role: "button",
            name: "Next page",
        };
        assert_eq!(
            press_escalation(&Key::parse("Space").expect("key"), next),
            None
        );
        assert_eq!(
            press_escalation(&Key::parse("Enter").expect("key"), next),
            None
        );
        // Through the classifier with a focus the host read.
        let classified = classify_with_focus(
            "browser_press",
            &json!({"key": "Enter"}),
            None,
            Focus::Control {
                role: "button",
                name: "Buy now",
            },
        );
        assert_eq!(classified.category, ToolCategory::Payment);
        assert!(!classified.grantable());
    }

    /// Fail closed: Enter or Space, modified or not, on a focus nobody could
    /// read is asked every time and no grant answers it. The cloud has only
    /// its latest snapshot: the `[focused]` node there, or unknown.
    #[test]
    fn a_press_on_an_unknown_focus_fails_closed() {
        for key in [
            "Enter",
            "Space",
            "Shift+Enter",
            "Meta+Enter",
            "Control+Space",
        ] {
            let classified = classify_with_focus(
                "browser_press",
                &json!({"key": key}),
                Some(&page()),
                Focus::Unknown,
            );
            assert_eq!(classified.escalation, Some(Escalation::Unresolved), "{key}");
            assert_eq!(classified.category, ToolCategory::ExternalSend);
            assert!(!classified.grantable(), "{key}");
            // The cloud's form: no snapshot, or no `[focused]` node.
            assert_eq!(
                classify("browser_press", &json!({"key": key}), None).escalation,
                Some(Escalation::Unresolved),
                "{key}"
            );
            assert_eq!(
                of("browser_press", json!({"key": key})).escalation,
                Some(Escalation::Unresolved),
                "{key}"
            );
        }
        let focused = Snapshot::parse(
            "# snapshot s2\n# url \"https://shop.example/cart\"\n# title \"Cart\"\n\
- textbox \"Search\" [ref=e1]\n\
- button \"Place order\" [focused] [ref=e2]\n",
        )
        .expect("snapshot");
        assert_eq!(
            classify("browser_press", &json!({"key": "Enter"}), Some(&focused)).category,
            ToolCategory::Payment,
            "the snapshot's focused node"
        );
        assert_eq!(
            classify("browser_press", &json!({"key": "Tab"}), Some(&focused)).category,
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
    fn an_unresolved_ref_fails_closed() {
        for (arguments, snapshot) in [
            // No snapshot yet.
            (json!({"ref": "e2"}), None),
            // A ref pinned to another snapshot.
            (json!({"ref": "@s9:e2"}), Some(page())),
            // A ref the snapshot never gave.
            (json!({"ref": "e99"}), Some(page())),
        ] {
            let classified = classify("browser_click", &arguments, snapshot.as_ref());
            assert_eq!(
                classified.category,
                ToolCategory::ExternalSend,
                "{arguments}"
            );
            assert_eq!(classified.escalation, Some(Escalation::Unresolved));
            assert!(
                !classified.grantable(),
                "no grant answers an unknown control"
            );
        }
        // Typing into an unknown field asks too; a page scroll or a key has
        // no ref and stays a plain write.
        assert_eq!(
            classify("browser_type", &json!({"ref": "e1", "text": "x"}), None).escalation,
            Some(Escalation::Unresolved)
        );
        assert_eq!(
            classify("browser_scroll", &json!({"direction": "down"}), None).category,
            ToolCategory::Write
        );
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

    /// A link is never a file chooser: "file" in its name is no upload.
    #[test]
    fn a_link_named_for_a_file_is_not_an_upload() {
        assert_eq!(control_escalation("link", "Open file list"), None);
        assert_eq!(
            control_escalation("button", "Choose file"),
            Some(Escalation::Submit)
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
