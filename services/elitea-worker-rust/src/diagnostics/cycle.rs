//! Per-window operator lines for a failure that repeats (FU-299 C3, the Rust
//! twin of `libs/go/faults/cycle.go`'s window): the first occurrence of a key
//! in a window is written with the number of that key's occurrences not
//! written since; the others are left to DEBUG.

use std::collections::HashMap;
use std::sync::Mutex;
use std::time::{Duration, Instant};

/// How often a key that keeps repeating writes its line.
pub(crate) const CYCLE_LINE_EVERY: Duration = Duration::from_secs(60);

/// The most keys tracked at once. Past it, expired windows are dropped; a key
/// that still finds no room writes its line untracked (never hidden).
pub(crate) const MAX_CYCLE_KEYS: usize = 4_096;

/// One window per key, owned by the component that reports (no process-wide
/// registry).
pub(crate) struct CycleWindows {
    every: Duration,
    windows: Mutex<HashMap<String, Window>>,
}

struct Window {
    last_written: Instant,
    suppressed: u64,
}

impl CycleWindows {
    pub(crate) fn new(every: Duration) -> Self {
        Self {
            every,
            windows: Mutex::default(),
        }
    }

    /// `Some(suppressed)` when this occurrence of `key` at `now` writes its
    /// line, `suppressed` being the occurrences of `key` not written since its
    /// last line; `None` when it stays in the current window. A poisoned lock
    /// or a full table writes the line (never hide a fault).
    pub(crate) fn admit(&self, key: &str, now: Instant) -> Option<u64> {
        let Ok(mut windows) = self.windows.lock() else {
            return Some(0);
        };
        if let Some(window) = windows.get_mut(key) {
            if now.saturating_duration_since(window.last_written) < self.every {
                window.suppressed = window.suppressed.saturating_add(1);
                return None;
            }
            window.last_written = now;
            return Some(std::mem::take(&mut window.suppressed));
        }
        if windows.len() >= MAX_CYCLE_KEYS {
            let every = self.every;
            windows.retain(|_, window| {
                now.saturating_duration_since(window.last_written) < every || window.suppressed > 0
            });
            if windows.len() >= MAX_CYCLE_KEYS {
                return Some(0);
            }
        }
        windows.insert(
            key.to_owned(),
            Window {
                last_written: now,
                suppressed: 0,
            },
        );
        Some(0)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn one_line_per_key_per_window_with_the_suppressed_count() {
        let windows = CycleWindows::new(CYCLE_LINE_EVERY);
        let start = Instant::now();
        assert_eq!(windows.admit("a", start), Some(0));
        assert_eq!(windows.admit("a", start + Duration::from_secs(1)), None);
        assert_eq!(windows.admit("a", start + Duration::from_secs(59)), None);
        // Another key has its own window.
        assert_eq!(windows.admit("b", start + Duration::from_secs(2)), Some(0));
        // The next window writes once, with the two kept quiet.
        assert_eq!(windows.admit("a", start + CYCLE_LINE_EVERY), Some(2));
        assert_eq!(
            windows.admit("a", start + CYCLE_LINE_EVERY + Duration::from_secs(1)),
            None
        );
        assert_eq!(windows.admit("a", start + CYCLE_LINE_EVERY * 2), Some(1));
    }

    #[test]
    fn a_full_table_drops_expired_windows_and_never_hides_a_line() {
        let windows = CycleWindows::new(CYCLE_LINE_EVERY);
        let start = Instant::now();
        for key in 0..MAX_CYCLE_KEYS {
            assert_eq!(windows.admit(&key.to_string(), start), Some(0));
        }
        // Full, nothing expired: written untracked.
        assert_eq!(windows.admit("late", start), Some(0));
        assert_eq!(windows.admit("late", start), Some(0));
        // A window later the expired keys are dropped and the new key tracked.
        let later = start + CYCLE_LINE_EVERY;
        assert_eq!(windows.admit("late", later), Some(0));
        assert_eq!(windows.admit("late", later), None);
    }
}
