//! Per-window operator lines for a failure that repeats (FU-299 C3, the Rust
//! twin of `libs/go/faults/cycle.go`'s window): the first occurrence of a
//! class in a window is written with the number of that class's occurrences
//! not written since; the others are left to DEBUG.

use std::collections::HashMap;
use std::sync::Mutex;
use std::time::{Duration, Instant};

/// How often a class that keeps repeating writes its line.
pub(crate) const CYCLE_LINE_EVERY: Duration = Duration::from_secs(60);

/// One window per class. Classes are `&'static str` codes, so the map is
/// bounded by the codes the binary holds.
pub(crate) struct CycleWindows {
    every: Duration,
    windows: Mutex<HashMap<&'static str, Window>>,
}

#[derive(Default)]
struct Window {
    last_written: Option<Instant>,
    suppressed: u64,
}

impl CycleWindows {
    pub(crate) fn new(every: Duration) -> Self {
        Self {
            every,
            windows: Mutex::default(),
        }
    }

    /// `Some(suppressed)` when this occurrence of `class` at `now` writes its
    /// line, `suppressed` being the occurrences of `class` not written since
    /// its last line; `None` when it stays in the current window. A poisoned
    /// lock writes the line (never hide a fault behind a broken counter).
    pub(crate) fn admit(&self, class: &'static str, now: Instant) -> Option<u64> {
        let Ok(mut windows) = self.windows.lock() else {
            return Some(0);
        };
        let window = windows.entry(class).or_default();
        if window
            .last_written
            .is_some_and(|last| now.saturating_duration_since(last) < self.every)
        {
            window.suppressed = window.suppressed.saturating_add(1);
            return None;
        }
        window.last_written = Some(now);
        Some(std::mem::take(&mut window.suppressed))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn one_line_per_class_per_window_with_the_suppressed_count() {
        let windows = CycleWindows::new(CYCLE_LINE_EVERY);
        let start = Instant::now();
        assert_eq!(windows.admit("a", start), Some(0));
        assert_eq!(windows.admit("a", start + Duration::from_secs(1)), None);
        assert_eq!(windows.admit("a", start + Duration::from_secs(59)), None);
        // Another class has its own window.
        assert_eq!(windows.admit("b", start + Duration::from_secs(2)), Some(0));
        // The next window writes once, with the two kept quiet.
        assert_eq!(windows.admit("a", start + CYCLE_LINE_EVERY), Some(2));
        assert_eq!(
            windows.admit("a", start + CYCLE_LINE_EVERY + Duration::from_secs(1)),
            None
        );
        assert_eq!(windows.admit("a", start + CYCLE_LINE_EVERY * 2), Some(1));
    }
}
