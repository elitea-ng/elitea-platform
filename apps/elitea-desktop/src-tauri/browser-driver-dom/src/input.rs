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

/// How long a CDP input command may wait for its answer before its
/// expected reports stop hiding the person's input (Windows: the command
/// was sent but `WebView2` never answered it).
pub const UNANSWERED_LIMIT: Duration = Duration::from_secs(2);

/// One report the driver's own input will cause.
#[derive(Debug)]
struct Expected {
    kind: &'static str,
    /// The CDP command that causes it (Windows), until it is answered.
    command: Option<u64>,
    /// When the grace starts: the post (macOS and Linux), or the command's
    /// answer (Windows). Before the answer: when the command was sent.
    at: Instant,
}

impl Expected {
    fn live(&self, now: Instant) -> bool {
        let limit = if self.command.is_some() {
            UNANSWERED_LIMIT
        } else {
            OWN_INPUT_GRACE
        };
        now.saturating_duration_since(self.at) < limit
    }
}

/// The reports the driver's own input will cause, not seen yet.
#[derive(Debug, Default)]
pub struct OwnInput {
    pending: Vec<Expected>,
    /// The last dispatch of each kind that was expected to report.
    dispatched: Vec<(&'static str, Instant)>,
}

/// The page script's report interval per event type (`js/input.ts` and
/// `js/driver.ts`: at most one report every 50 ms per type).
pub const REPORT_INTERVAL: Duration = Duration::from_millis(50);

impl OwnInput {
    /// The driver posts a click of `downs` presses (2 for a double click).
    pub fn posting(&mut self, downs: usize, now: Instant) {
        self.expect("pointerdown", downs, now);
    }

    /// The driver posts input that the page reports `count` times as
    /// `kind` (one of [`REPORTED`]); the grace starts now.
    pub fn expect(&mut self, kind: &'static str, count: usize, now: Instant) {
        self.expire(now);
        self.pending.extend(
            std::iter::repeat_with(|| Expected {
                kind,
                command: None,
                at: now,
            })
            .take(count),
        );
    }

    /// The driver sends CDP command `id`, which the page reports as
    /// `reports` ([`cdp_reports`]). Its grace starts at [`Self::answered`]:
    /// the page sees the input only after the command reached it, which
    /// may be long after it was sent (the UI thread was busy).
    pub fn sending(&mut self, id: u64, reports: &[(&'static str, usize)], now: Instant) {
        self.expire(now);
        for &(kind, count) in reports {
            self.pending.extend(
                std::iter::repeat_with(|| Expected {
                    kind,
                    command: Some(id),
                    at: now,
                })
                .take(count),
            );
        }
    }

    /// CDP command `id` was answered: the input was dispatched, and its
    /// reports have [`OWN_INPUT_GRACE`] from now.
    ///
    /// The page script reports at most one event per type every
    /// [`REPORT_INTERVAL`] (`js/input.ts`), so an input of a kind already
    /// dispatched less than that before causes no report: its expectation
    /// is dropped here, or it would be left over and hide the person's next
    /// input of that kind (a chord's three `keydown`s are one report).
    pub fn answered(&mut self, id: u64, now: Instant) {
        let mut index = 0;
        while index < self.pending.len() {
            let expected = &mut self.pending[index];
            if expected.command != Some(id) {
                index += 1;
                continue;
            }
            let kind = expected.kind;
            let throttled = self.dispatched.iter().any(|(seen, at)| {
                *seen == kind && now.saturating_duration_since(*at) < REPORT_INTERVAL
            });
            if throttled {
                self.pending.remove(index);
                continue;
            }
            let expected = &mut self.pending[index];
            expected.command = None;
            expected.at = now;
            self.dispatched.retain(|(seen, _)| *seen != kind);
            self.dispatched.push((kind, now));
            index += 1;
        }
        self.expire(now);
    }

    /// How many reports of `kind` are still taken as the driver's.
    #[must_use]
    pub fn waiting(&self, kind: &str) -> usize {
        self.pending.iter().filter(|own| own.kind == kind).count()
    }

    /// Whether the report of an input of type `kind` is the driver's own
    /// (and consumes it).
    pub fn is_own(&mut self, kind: &str, now: Instant) -> bool {
        self.expire(now);
        let Some(index) = self.pending.iter().position(|own| own.kind == kind) else {
            return false;
        };
        self.pending.remove(index);
        true
    }

    fn expire(&mut self, now: Instant) {
        self.pending.retain(|expected| expected.live(now));
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
                // A key that types a character, or deletes one, also
                // reports `beforeinput` in a text field. Enter and Tab
                // ("\r", "\t") do not in a one-line field; in a textarea
                // the person's report is then taken as theirs (the agent
                // pauses: fail closed).
                let typed = params["text"]
                    .as_str()
                    .is_some_and(|text| !text.is_empty() && !text.chars().all(char::is_control));
                let deletes = matches!(params["key"].as_str(), Some("Backspace" | "Delete"));
                if typed || deletes {
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
            [("keydown", 1)],
            "Enter types no text in a one-line field"
        );
        let letter = json!({"type": "keyDown", "text": "a"});
        assert_eq!(
            cdp_reports("Input.dispatchKeyEvent", &letter),
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

    /// Send each command as `CdpDriver` does (one at a time, each answered
    /// `gap` after the one before) and return the reports still expected.
    fn drive(
        own: &mut OwnInput,
        commands: &[(&str, serde_json::Value)],
        start: Instant,
        gap: Duration,
    ) -> Instant {
        let mut now = start;
        for (id, (method, params)) in (1_u64..).zip(commands) {
            own.sending(id, &cdp_reports(method, params), now);
            now += gap;
            own.answered(id, now);
        }
        now
    }

    fn keys(key: &str) -> Vec<(&'static str, serde_json::Value)> {
        let key = elitea_browser_tools::call::Key::parse(key).unwrap();
        elitea_browser_session::keys::events(&key)
            .into_iter()
            .map(|event| ("Input.dispatchKeyEvent", event))
            .collect()
    }

    #[test]
    fn the_cdp_drivers_own_key_sequences_leave_no_expectation_over() {
        let at = Instant::now();
        let fast = Duration::from_millis(5);
        // A chord: three key downs within the page's report interval are
        // ONE `keydown` report (js/input.ts), and it types nothing.
        let mut own = OwnInput::default();
        let end = drive(&mut own, &keys("Control+Shift+K"), at, fast);
        assert_eq!(own.waiting("keydown"), 1, "one report, not three");
        assert_eq!(own.waiting("beforeinput"), 0);
        assert!(own.is_own("keydown", end), "the chord's own report");
        assert!(
            !own.is_own("keydown", end + Duration::from_millis(60)),
            "the person's next key pauses the agent"
        );

        // Enter submits a one-line field: a `keydown`, no `beforeinput`.
        let mut own = OwnInput::default();
        drive(&mut own, &keys("Enter"), at, fast);
        assert_eq!(own.waiting("keydown"), 1);
        assert_eq!(own.waiting("beforeinput"), 0);

        // A key that types: its `keydown` and its text.
        let mut own = OwnInput::default();
        drive(&mut own, &keys("a"), at, fast);
        assert_eq!(own.waiting("keydown"), 1);
        assert_eq!(own.waiting("beforeinput"), 1);

        // Backspace (`clear` with no text) deletes: `beforeinput` too.
        let mut own = OwnInput::default();
        drive(&mut own, &keys("Backspace"), at, fast);
        assert_eq!(own.waiting("keydown"), 1);
        assert_eq!(own.waiting("beforeinput"), 1);

        // Keys answered further apart than the interval each report.
        let mut own = OwnInput::default();
        drive(
            &mut own,
            &keys("Control+Shift+K"),
            at,
            Duration::from_millis(55),
        );
        assert_eq!(own.waiting("keydown"), 3);
    }

    #[test]
    fn a_cdp_click_is_its_downs_and_a_typed_text_is_one_input() {
        use serde_json::json;
        let at = Instant::now();
        // `CdpDriver::click(double)`: move, then press and release twice.
        let mouse = |kind: &str, clicks: u32| {
            (
                "Input.dispatchMouseEvent",
                json!({ "type": kind, "x": 5, "y": 5, "button": "left", "clickCount": clicks }),
            )
        };
        let double = [
            mouse("mouseMoved", 0),
            mouse("mousePressed", 1),
            mouse("mouseReleased", 1),
            mouse("mousePressed", 2),
            mouse("mouseReleased", 2),
        ];
        // Answered 30 ms apart: the two downs are 60 ms apart and both
        // report.
        let mut own = OwnInput::default();
        drive(&mut own, &double, at, Duration::from_millis(30));
        assert_eq!(own.waiting("pointerdown"), 2);

        let mut own = OwnInput::default();
        let typed = [("Input.insertText", json!({ "text": "Ada" }))];
        drive(&mut own, &typed, at, Duration::from_millis(5));
        assert_eq!(own.waiting("beforeinput"), 1);
    }

    #[test]
    fn the_grace_starts_when_the_command_is_answered() {
        let at = Instant::now();
        let mut own = OwnInput::default();
        let press = serde_json::json!({ "type": "mousePressed" });
        own.sending(7, &cdp_reports("Input.dispatchMouseEvent", &press), at);
        // The UI thread was busy: the click reached the page 1 s later.
        let answered = at + Duration::from_secs(1);
        own.answered(7, answered);
        assert!(
            own.is_own("pointerdown", answered + Duration::from_millis(20)),
            "the agent's own click does not pause the agent"
        );

        // A command never answered stops hiding the person's input.
        let mut own = OwnInput::default();
        own.sending(8, &cdp_reports("Input.dispatchMouseEvent", &press), at);
        assert!(!own.is_own("pointerdown", at + UNANSWERED_LIMIT));
    }
}
