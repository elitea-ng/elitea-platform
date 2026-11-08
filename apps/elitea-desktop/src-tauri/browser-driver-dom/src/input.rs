//! Telling the person's input from the driver's own (ADR-0048 decision 4).
//!
//! The page script reports every trusted input event's type (see
//! `js/driver.ts` and `js/input.ts`). Input the driver posts itself as
//! trusted (macOS and Linux: a `trusted_click`; Windows: every CDP
//! `Input.*` command) reports too. Each such input is remembered as the
//! reports it will cause ([`OwnInput::expect`], [`OwnInput::posting`] for a
//! click's downs); a report of that kind consumes one of them
//! ([`OwnInput::is_own`]), and every other report is the person's. The
//! person's own input right after the driver's still counts unless it is
//! of a kind the driver is waiting for, and only within [`OWN_INPUT_GRACE`]
//! of the driver posting it.

use std::time::{Duration, Instant};

/// How long a posted click's downs may take to be reported.
pub const OWN_INPUT_GRACE: Duration = Duration::from_millis(300);

/// The input event types the page script reports for the person.
pub const REPORTED: &[&str] = &[
    "pointerdown",
    "keydown",
    "wheel",
    "touchstart",
    "beforeinput",
    "paste",
    "drop",
];

/// The reports the driver's own input will cause, not seen yet.
#[derive(Debug, Default)]
pub struct OwnInput {
    pending: Vec<(&'static str, Instant)>,
}

impl OwnInput {
    /// The driver posts a click of `downs` presses (2 for a double click).
    pub fn posting(&mut self, downs: usize, now: Instant) {
        self.expect("pointerdown", downs, now);
    }

    /// The driver posts input that the page reports `count` times as
    /// `kind` (one of [`REPORTED`]).
    pub fn expect(&mut self, kind: &'static str, count: usize, now: Instant) {
        self.expire(now);
        self.pending.extend(std::iter::repeat_n((kind, now), count));
    }

    /// Whether the report of an input of type `kind` is the driver's own
    /// (and consumes it).
    pub fn is_own(&mut self, kind: &str, now: Instant) -> bool {
        self.expire(now);
        let Some(index) = self.pending.iter().position(|(own, _)| *own == kind) else {
            return false;
        };
        self.pending.remove(index);
        true
    }

    fn expire(&mut self, now: Instant) {
        self.pending
            .retain(|(_, at)| now.saturating_duration_since(*at) < OWN_INPUT_GRACE);
    }
}

/// The reports a CDP input command the driver sends will cause (Windows:
/// `WebView2` dispatches `Input.*` as trusted input), as `(kind, count)`.
/// `method` and `params` are the command's. Nothing for any other command.
#[must_use]
pub fn cdp_reports(method: &str, params: &serde_json::Value) -> Vec<(&'static str, usize)> {
    let kind = params["type"].as_str().unwrap_or_default();
    match method {
        "Input.dispatchMouseEvent" => match kind {
            "mousePressed" => vec![("pointerdown", 1)],
            "mouseWheel" => vec![("wheel", 1)],
            _ => Vec::new(),
        },
        "Input.dispatchKeyEvent" => match kind {
            "keyDown" | "rawKeyDown" => {
                // A key that types also reports its text.
                let typed = params["text"].as_str().is_some_and(|text| !text.is_empty());
                if typed {
                    vec![("keydown", 1), ("beforeinput", 1)]
                } else {
                    vec![("keydown", 1)]
                }
            }
            "char" => vec![("beforeinput", 1)],
            _ => Vec::new(),
        },
        "Input.insertText" => vec![("beforeinput", 1)],
        "Input.dispatchTouchEvent" if kind == "touchStart" => vec![("touchstart", 1)],
        _ => Vec::new(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_drivers_own_click_is_dropped_but_the_persons_next_one_counts() {
        let mut own = OwnInput::default();
        let at = Instant::now();
        own.posting(1, at);
        assert!(own.is_own("pointerdown", at + Duration::from_millis(5)));
        // The person clicks 50 ms later, inside what was the grace period.
        assert!(!own.is_own("pointerdown", at + Duration::from_millis(50)));
    }

    #[test]
    fn keys_scrolls_pastes_and_drops_are_never_the_drivers() {
        let mut own = OwnInput::default();
        let at = Instant::now();
        own.posting(1, at);
        for kind in [
            "keydown",
            "wheel",
            "paste",
            "drop",
            "beforeinput",
            "touchstart",
        ] {
            assert!(!own.is_own(kind, at), "{kind}");
        }
        assert!(own.is_own("pointerdown", at));
    }

    #[test]
    fn a_double_click_is_two_own_downs_and_old_ones_expire() {
        let mut own = OwnInput::default();
        let at = Instant::now();
        own.posting(2, at);
        assert!(own.is_own("pointerdown", at));
        assert!(own.is_own("pointerdown", at));
        assert!(!own.is_own("pointerdown", at));
        // A down never reported (the click missed the page) does not hide
        // the person's input after the grace period.
        own.posting(1, at);
        assert!(!own.is_own("pointerdown", at + OWN_INPUT_GRACE));
    }

    #[test]
    fn expected_keys_and_text_are_the_drivers_but_other_kinds_are_not() {
        let mut own = OwnInput::default();
        let at = Instant::now();
        own.expect("keydown", 1, at);
        own.expect("beforeinput", 1, at);
        assert!(!own.is_own("pointerdown", at), "the person clicked");
        assert!(own.is_own("keydown", at));
        assert!(own.is_own("beforeinput", at));
        assert!(!own.is_own("keydown", at), "a second key is the person's");
    }

    #[test]
    fn cdp_input_commands_name_the_reports_they_cause() {
        use serde_json::json;
        let press = json!({"type": "mousePressed", "x": 1, "y": 2, "button": "left"});
        assert_eq!(
            cdp_reports("Input.dispatchMouseEvent", &press),
            [("pointerdown", 1)]
        );
        let moved = json!({"type": "mouseMoved"});
        assert!(cdp_reports("Input.dispatchMouseEvent", &moved).is_empty());
        let enter = json!({"type": "keyDown", "text": "\r"});
        assert_eq!(
            cdp_reports("Input.dispatchKeyEvent", &enter),
            [("keydown", 1), ("beforeinput", 1)]
        );
        let arrow = json!({"type": "rawKeyDown"});
        assert_eq!(
            cdp_reports("Input.dispatchKeyEvent", &arrow),
            [("keydown", 1)]
        );
        let text = json!({"text": "hello"});
        assert_eq!(cdp_reports("Input.insertText", &text), [("beforeinput", 1)]);
        assert!(cdp_reports("Page.navigate", &json!({})).is_empty());
    }
}
