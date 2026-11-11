//! Per-window operator lines for a failure that repeats (FU-299 C3, the Rust
//! twin of `libs/go/faults/cycle.go`'s window), one window per class (a result
//! code): the first occurrence in a window writes its line; the others are
//! counted and left to DEBUG. When the window has run out, its summary (how
//! many were kept quiet, how many distinct executions, a bounded sample of
//! their support references) is reported once and the window is evicted.

use std::collections::{BTreeSet, HashMap};
use std::sync::Mutex;
use std::time::{Duration, Instant};

/// How long one window lasts.
pub(crate) const CYCLE_LINE_EVERY: Duration = Duration::from_secs(60);
/// The most classes tracked at once (a class is a result code, so few).
pub(crate) const MAX_CYCLE_CLASSES: usize = 64;
/// The support references a summary names.
pub(crate) const MAX_SAMPLE_REFS: usize = 8;
/// The distinct executions one window counts; past it the count is a floor.
pub(crate) const MAX_COUNTED_EXECUTIONS: usize = 4_096;

/// The summary of one window that ran out.
#[derive(Debug, Eq, PartialEq)]
pub(crate) struct CycleSummary {
    pub(crate) class: &'static str,
    /// Occurrences not written (all but the window's first).
    pub(crate) suppressed: u64,
    /// Distinct executions in the window, the first included.
    pub(crate) executions: usize,
    /// Whether `executions` reached [`MAX_COUNTED_EXECUTIONS`] (a floor).
    pub(crate) executions_capped: bool,
    /// Up to [`MAX_SAMPLE_REFS`] of those executions, in first-seen order.
    pub(crate) sample: Vec<String>,
}

/// The windows, owned by the component that reports (no process-wide state).
pub(crate) struct CycleWindows {
    every: Duration,
    windows: Mutex<HashMap<&'static str, Window>>,
}

struct Window {
    opened: Instant,
    suppressed: u64,
    executions: BTreeSet<String>,
    sample: Vec<String>,
}

impl Window {
    fn count(&mut self, execution: &str) {
        if self.executions.len() < MAX_COUNTED_EXECUTIONS
            && self.executions.insert(execution.to_owned())
            && self.sample.len() < MAX_SAMPLE_REFS
        {
            self.sample.push(execution.to_owned());
        }
    }

    fn summary(&mut self, class: &'static str) -> CycleSummary {
        let executions = std::mem::take(&mut self.executions);
        CycleSummary {
            class,
            suppressed: self.suppressed,
            executions_capped: executions.len() >= MAX_COUNTED_EXECUTIONS,
            executions: executions.len(),
            sample: std::mem::take(&mut self.sample),
        }
    }
}

impl CycleWindows {
    pub(crate) fn new(every: Duration) -> Self {
        Self {
            every,
            windows: Mutex::default(),
        }
    }

    /// Report and evict every window that ran out by `now`: a summary for each
    /// that kept something quiet; the others are evicted without one.
    pub(crate) fn flush_expired(&self, now: Instant) -> Vec<CycleSummary> {
        let Ok(mut windows) = self.windows.lock() else {
            return Vec::new();
        };
        let mut summaries = Vec::new();
        windows.retain(|class, window| {
            if now.saturating_duration_since(window.opened) < self.every {
                return true;
            }
            if window.suppressed > 0 {
                summaries.push(window.summary(class));
            }
            false
        });
        summaries.sort_by_key(|summary| summary.class);
        summaries
    }

    /// Whether this occurrence of `class` by `execution` at `now` writes its
    /// line (it opens a window) or is counted quietly in the open one. A
    /// poisoned lock or a full table writes the line (never hide a fault).
    pub(crate) fn admit(&self, class: &'static str, execution: &str, now: Instant) -> bool {
        let Ok(mut windows) = self.windows.lock() else {
            return true;
        };
        if let Some(window) = windows.get_mut(class) {
            if now.saturating_duration_since(window.opened) < self.every {
                window.suppressed = window.suppressed.saturating_add(1);
                window.count(execution);
                return false;
            }
            // Ran out without a flush: the caller flushes first, so this only
            // happens on a clock that moved backwards; start over.
            windows.remove(class);
        }
        if windows.len() >= MAX_CYCLE_CLASSES {
            return true;
        }
        let mut window = Window {
            opened: now,
            suppressed: 0,
            executions: BTreeSet::new(),
            sample: Vec::new(),
        };
        window.count(execution);
        windows.insert(class, window);
        true
    }

    #[cfg(test)]
    pub(crate) fn tracked(&self) -> usize {
        self.windows.lock().map_or(0, |windows| windows.len())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn many_executions_in_one_window_are_one_line_and_one_summary() {
        let windows = CycleWindows::new(CYCLE_LINE_EVERY);
        let start = Instant::now();
        assert!(windows.admit("transient", "exec-0", start));
        for index in 1..20 {
            let execution = format!("exec-{}", index % 12);
            assert!(!windows.admit("transient", &execution, start + Duration::from_secs(index)));
        }
        // Inside the window nothing is flushed.
        assert_eq!(windows.flush_expired(start + Duration::from_secs(59)), []);
        let summaries = windows.flush_expired(start + CYCLE_LINE_EVERY);
        assert_eq!(summaries.len(), 1);
        let summary = &summaries[0];
        assert_eq!(summary.class, "transient");
        assert_eq!(summary.suppressed, 19);
        assert_eq!(summary.executions, 12);
        assert!(!summary.executions_capped);
        assert_eq!(summary.sample.len(), MAX_SAMPLE_REFS);
        assert_eq!(summary.sample[0], "exec-0");
        // The expired window was evicted: the next occurrence opens a new one.
        assert_eq!(windows.tracked(), 0);
        assert!(windows.admit("transient", "exec-0", start + CYCLE_LINE_EVERY));
    }

    #[test]
    fn an_expired_window_with_nothing_quiet_is_evicted_without_a_summary() {
        let windows = CycleWindows::new(CYCLE_LINE_EVERY);
        let start = Instant::now();
        assert!(windows.admit("transient", "exec-0", start));
        assert_eq!(windows.flush_expired(start + CYCLE_LINE_EVERY), []);
        assert_eq!(windows.tracked(), 0);
    }

    #[test]
    fn the_table_never_stays_full() {
        let windows = CycleWindows::new(CYCLE_LINE_EVERY);
        let start = Instant::now();
        let classes = (0..=MAX_CYCLE_CLASSES)
            .map(|index| &*Box::leak(format!("class-{index}").into_boxed_str()))
            .collect::<Vec<&'static str>>();
        for class in &classes[..MAX_CYCLE_CLASSES] {
            assert!(windows.admit(class, "exec", start));
            assert!(!windows.admit(class, "exec", start));
        }
        // Full: a new class is written untracked, never hidden.
        let late = classes[MAX_CYCLE_CLASSES];
        assert!(windows.admit(late, "exec", start));
        assert!(windows.admit(late, "exec", start));
        // Every window ran out: each is reported once and evicted.
        assert_eq!(
            windows.flush_expired(start + CYCLE_LINE_EVERY).len(),
            MAX_CYCLE_CLASSES
        );
        assert_eq!(windows.tracked(), 0);
        assert!(windows.admit(late, "exec", start + CYCLE_LINE_EVERY));
        assert!(!windows.admit(late, "exec", start + CYCLE_LINE_EVERY));
    }
}
