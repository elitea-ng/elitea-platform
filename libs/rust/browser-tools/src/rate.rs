//! The `BrowserAct` rate limit (ADR-0048 decision 5): at most
//! [`MAX_ACTS_PER_SECOND`] acts in any rolling second, per pane. A driver
//! keeps one [`ActRate`] per pane and admits every `BrowserAct` call through
//! it before acting; the limit holds in bypass mode too.

use std::collections::VecDeque;
use std::time::{Duration, Instant};

use crate::error::{BrowserError, ErrorCode};

pub const MAX_ACTS_PER_SECOND: usize = 10;
const WINDOW: Duration = Duration::from_secs(1);

/// The acts of one pane in the last second.
#[derive(Debug, Default)]
pub struct ActRate {
    recent: VecDeque<Instant>,
}

impl ActRate {
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Admit one act at `now`.
    ///
    /// # Errors
    ///
    /// `rate_limited` when [`MAX_ACTS_PER_SECOND`] acts were admitted in the
    /// second before `now`; a refused act does not count.
    pub fn admit(&mut self, now: Instant) -> Result<(), BrowserError> {
        while self
            .recent
            .front()
            .is_some_and(|at| now.saturating_duration_since(*at) >= WINDOW)
        {
            self.recent.pop_front();
        }
        if self.recent.len() >= MAX_ACTS_PER_SECOND {
            return Err(BrowserError::new(
                ErrorCode::RateLimited,
                "more than 10 browser actions in one second: slow down",
            ));
        }
        self.recent.push_back(now);
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ten_per_rolling_second() {
        let start = Instant::now();
        let mut rate = ActRate::new();
        for n in 0..10 {
            rate.admit(start + Duration::from_millis(n * 10)).unwrap();
        }
        let refused = rate.admit(start + Duration::from_millis(500));
        assert_eq!(refused.unwrap_err().code(), ErrorCode::RateLimited);
        rate.admit(start + Duration::from_millis(1000)).unwrap();
    }
}
