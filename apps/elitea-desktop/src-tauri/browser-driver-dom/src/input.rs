//! Telling the person's input from the driver's own (ADR-0048 decision 4).
//!
//! The page script reports every trusted input event's type (see
//! `js/driver.ts`). A trusted click the driver posts itself
//! (`trusted_click`) reports too, as one `pointerdown` per click. Each
//! posted click is remembered as the downs it will report
//! ([`OwnInput::posting`]); a `pointerdown` report consumes one of them
//! ([`OwnInput::is_own`]), and every other report is the person's. The
//! person's own click right after the driver's still counts, and so does
//! any key, scroll, paste or drop meanwhile: only the driver's own downs
//! are dropped, and only within [`OWN_INPUT_GRACE`] of being posted.

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

/// The downs the driver posted and has not seen reported yet.
#[derive(Debug, Default)]
pub struct OwnInput {
    pending: Vec<Instant>,
}

impl OwnInput {
    /// The driver posts a click of `downs` presses (2 for a double click).
    pub fn posting(&mut self, downs: usize, now: Instant) {
        self.expire(now);
        self.pending.extend(std::iter::repeat_n(now, downs));
    }

    /// Whether the report of an input of type `kind` is the driver's own
    /// (and consumes it).
    pub fn is_own(&mut self, kind: &str, now: Instant) -> bool {
        self.expire(now);
        if kind != "pointerdown" || self.pending.is_empty() {
            return false;
        }
        self.pending.remove(0);
        true
    }

    fn expire(&mut self, now: Instant) {
        self.pending
            .retain(|at| now.saturating_duration_since(*at) < OWN_INPUT_GRACE);
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
}
