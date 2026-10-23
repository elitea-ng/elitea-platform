//! What embedding may cost: the token estimate, and the two caps that stop
//! a run (ADR-0029 decision 7, #1360).
//!
//! A remote embedder is paid from the bound project's budget, so a pass is
//! held to a **per-run cap** (2 000 000 tokens) and a **per-day cap**
//! (200 000 tokens, per workspace, counted per UTC day). Both are settings
//! of the workspace's index (`embed_run_cap`, `embed_day_cap`) and apply to
//! remote embedders only: an on-device model costs nothing. A run stops
//! cleanly at a cap, between texts, and what it embedded stays; the next
//! run continues with the chunks that still have no vector.
//!
//! Tokens are an ESTIMATE ([`estimate_tokens`]): the embeddings route
//! reports no usage the index could rely on across models, so a text counts
//! one token per four characters, rounded up. The same rule prices the
//! consent UI's estimate, so what it promised and what a run counts agree.

use crate::sqlite_store::SqliteGraphStore;

/// The default cap of one run, in estimated tokens.
pub const DEFAULT_RUN_CAP: u64 = 2_000_000;
/// The default cap of one UTC day, in estimated tokens.
pub const DEFAULT_DAY_CAP: u64 = 200_000;
/// The largest cap a person may set (a typo guard, not a policy).
pub const MAX_CAP: u64 = 10_000_000_000;

/// The store setting holding the per-run cap.
pub const RUN_CAP_SETTING: &str = "embed_run_cap";
/// The store setting holding the per-day cap.
pub const DAY_CAP_SETTING: &str = "embed_day_cap";

/// Characters per estimated token. The store's estimate query counts with
/// the same number (`(length + 3) / 4`).
pub const CHARS_PER_TOKEN: u64 = 4;

/// The estimated tokens of `text`: its characters over [`CHARS_PER_TOKEN`],
/// rounded up.
#[must_use]
pub fn estimate_tokens(text: &str) -> u64 {
    (text.chars().count() as u64).div_ceil(CHARS_PER_TOKEN)
}

/// Which cap stopped a run.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Cap {
    Run,
    Day,
}

impl Cap {
    /// The name the status carries.
    #[must_use]
    pub const fn name(self) -> &'static str {
        match self {
            Self::Run => "run",
            Self::Day => "day",
        }
    }
}

/// A workspace's two caps.
#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize)]
pub struct EmbedCaps {
    pub run: u64,
    pub day: u64,
}

impl Default for EmbedCaps {
    fn default() -> Self {
        Self {
            run: DEFAULT_RUN_CAP,
            day: DEFAULT_DAY_CAP,
        }
    }
}

impl EmbedCaps {
    /// Whether both caps are positive and within [`MAX_CAP`].
    #[must_use]
    pub fn valid(self) -> bool {
        (1..=MAX_CAP).contains(&self.run) && (1..=MAX_CAP).contains(&self.day)
    }

    /// The workspace's caps; a missing or unreadable setting is the default.
    #[must_use]
    pub fn load(store: &SqliteGraphStore) -> Self {
        let read = |name: &str, default: u64| {
            store
                .setting(name)
                .ok()
                .flatten()
                .and_then(|value| value.parse::<u64>().ok())
                .filter(|value| (1..=MAX_CAP).contains(value))
                .unwrap_or(default)
        };
        Self {
            run: read(RUN_CAP_SETTING, DEFAULT_RUN_CAP),
            day: read(DAY_CAP_SETTING, DEFAULT_DAY_CAP),
        }
    }

    /// Keep the caps.
    ///
    /// # Errors
    ///
    /// A cap is zero or above [`MAX_CAP`], or the store failed (the message).
    pub fn save(self, store: &SqliteGraphStore) -> Result<(), String> {
        if !self.valid() {
            return Err("a cap must be between 1 and 10 000 000 000 tokens".into());
        }
        store
            .set_setting(RUN_CAP_SETTING, &self.run.to_string())
            .and_then(|()| store.set_setting(DAY_CAP_SETTING, &self.day.to_string()))
            .map_err(|error| error.to_string())
    }

    /// What a run that has used `run_used` tokens may still spend, with the
    /// cap that binds; `day_used` is everything embedded today (this run
    /// included).
    #[must_use]
    pub fn remaining(self, run_used: u64, day_used: u64) -> (u64, Cap) {
        let run_left = self.run.saturating_sub(run_used);
        let day_left = self.day.saturating_sub(day_used);
        if day_left <= run_left {
            (day_left, Cap::Day)
        } else {
            (run_left, Cap::Run)
        }
    }
}

/// How many of `texts`, from the front, fit in `budget` estimated tokens.
#[must_use]
pub fn fit(texts: &[&str], budget: u64) -> usize {
    let mut spent = 0_u64;
    texts
        .iter()
        .take_while(|text| {
            spent = spent.saturating_add(estimate_tokens(text));
            spent <= budget
        })
        .count()
}

/// Today's total from the stored `YYYY-MM-DD:<tokens>` counter: what the
/// counter holds when it is today's, else nothing (a new UTC day starts at
/// zero).
#[must_use]
pub fn day_total(stored: Option<&str>, today: &str) -> u64 {
    stored
        .and_then(|value| value.split_once(':'))
        .filter(|(day, _)| *day == today)
        .and_then(|(_, tokens)| tokens.parse().ok())
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_text_is_a_token_per_four_characters_rounded_up() {
        assert_eq!(estimate_tokens(""), 0);
        assert_eq!(estimate_tokens("abcd"), 1);
        assert_eq!(estimate_tokens("abcde"), 2);
        // Characters, not bytes.
        assert_eq!(estimate_tokens("ééééé"), 2);
    }

    #[test]
    fn the_tighter_cap_binds_and_earlier_use_today_counts_against_the_day_only() {
        let caps = EmbedCaps {
            run: 1000,
            day: 300,
        };
        assert_eq!(caps.remaining(0, 0), (300, Cap::Day));
        assert_eq!(caps.remaining(100, 100), (200, Cap::Day));
        assert_eq!(caps.remaining(0, 250), (50, Cap::Day));
        assert_eq!(caps.remaining(0, 400), (0, Cap::Day));
        let wide = EmbedCaps {
            run: 100,
            day: 10_000,
        };
        assert_eq!(wide.remaining(40, 5040), (60, Cap::Run));
        assert_eq!(wide.remaining(100, 100), (0, Cap::Run));
        assert_eq!(wide.remaining(500, 500), (0, Cap::Run), "never negative");
    }

    #[test]
    fn a_batch_is_cut_at_the_texts_that_fit() {
        let texts = ["aaaa", "bbbbbbbb", "cccc"];
        assert_eq!(fit(&texts, 100), 3);
        assert_eq!(fit(&texts, 3), 2, "1 + 2 tokens");
        assert_eq!(fit(&texts, 2), 1);
        assert_eq!(fit(&texts, 0), 0);
        assert_eq!(fit(&[], 5), 0);
    }

    #[test]
    fn the_day_counter_starts_over_on_a_new_utc_day() {
        assert_eq!(day_total(Some("2026-10-23:1500"), "2026-10-23"), 1500);
        assert_eq!(day_total(Some("2026-10-22:1500"), "2026-10-23"), 0);
        assert_eq!(day_total(Some("garbage"), "2026-10-23"), 0);
        assert_eq!(day_total(None, "2026-10-23"), 0);
    }

    #[test]
    fn caps_outside_the_range_are_invalid() {
        assert!(EmbedCaps::default().valid());
        assert!(!EmbedCaps { run: 0, day: 5 }.valid());
        assert!(
            !EmbedCaps {
                run: 5,
                day: MAX_CAP + 1
            }
            .valid()
        );
    }
}
