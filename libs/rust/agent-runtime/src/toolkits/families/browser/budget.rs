//! The run's cloud browser time (ADR-0049 decision 8): a per-run cap and the
//! project's monthly quota, both in engine seconds.
//!
//! One [`BrowserBudget`] is shared by every browser call of a run. Before a
//! call it checks that the run and the project have time left; after the
//! call it charges the seconds the call held the session for (rounded up, at
//! least one). The first limit reached latches the run's
//! [`crate::run_caps::RunCapsMeter`] with [`RunCap::MaxBrowserSeconds`] or
//! [`RunCap::BrowserMonthlyQuota`], so the run ends `cap_reached` with that
//! cap's sentence: the stop is visible, never a silent truncation.
//!
//! The quota is checked against what Main read at claim time
//! ([`BrowserLimits::month_used_seconds`]) plus what this run used. Two runs
//! of one project that start together may each use the time left: the quota
//! is a stop, not a reservation, and the usage ledger still bills each
//! second.

use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use crate::run_caps::{RunCap, RunCapsMeter};

/// The run's browser limits, as Main sent them. 0 is no limit.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct BrowserLimits {
    /// The most engine seconds this run may use.
    pub max_run_seconds: u64,
    /// The project's monthly engine seconds.
    pub monthly_quota_seconds: u64,
    /// The engine seconds the project used this month before this run.
    pub month_used_seconds: u64,
}

/// The run's browser time.
pub struct BrowserBudget {
    limits: BrowserLimits,
    used: AtomicU64,
    caps: Arc<RunCapsMeter>,
}

impl BrowserBudget {
    #[must_use]
    pub fn new(limits: BrowserLimits, caps: Arc<RunCapsMeter>) -> Arc<Self> {
        Arc::new(Self {
            limits,
            used: AtomicU64::new(0),
            caps,
        })
    }

    /// The engine seconds this run used.
    #[must_use]
    pub fn used_seconds(&self) -> u64 {
        self.used.load(Ordering::Acquire)
    }

    /// The limits.
    #[must_use]
    pub const fn limits(&self) -> BrowserLimits {
        self.limits
    }

    /// Whether the run may make another call: no cap is latched, and the run
    /// and the project have time left.
    ///
    /// # Errors
    ///
    /// The cap the run reached, latched on the run's caps meter.
    pub fn admit(&self) -> Result<(), RunCap> {
        if let Some(cap) = self.caps.tripped() {
            return Err(cap);
        }
        self.over().map_or(Ok(()), |cap| Err(self.caps.latch(cap)))
    }

    /// Charge one call that held the session for `elapsed`, and latch a cap
    /// the charge reached. The call's result still reaches the model; the
    /// next call is refused.
    pub fn charge(&self, elapsed: Duration) -> u64 {
        let seconds = billable_seconds(elapsed);
        self.used.fetch_add(seconds, Ordering::AcqRel);
        if let Some(cap) = self.over() {
            self.caps.latch(cap);
        }
        seconds
    }

    fn over(&self) -> Option<RunCap> {
        let used = self.used_seconds();
        let limits = self.limits;
        if limits.max_run_seconds > 0 && used >= limits.max_run_seconds {
            return Some(RunCap::MaxBrowserSeconds);
        }
        if limits.monthly_quota_seconds > 0
            && limits.month_used_seconds.saturating_add(used) >= limits.monthly_quota_seconds
        {
            return Some(RunCap::BrowserMonthlyQuota);
        }
        None
    }
}

/// `elapsed` in whole seconds, rounded up, at least one: a call that held
/// the engine at all is billed for a second.
#[must_use]
pub fn billable_seconds(elapsed: Duration) -> u64 {
    let whole = elapsed.as_secs();
    let seconds = if elapsed.subsec_nanos() > 0 {
        whole.saturating_add(1)
    } else {
        whole
    };
    seconds.max(1)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::run_caps::RunCapLimits;

    fn budget(limits: BrowserLimits) -> (Arc<BrowserBudget>, Arc<RunCapsMeter>) {
        let caps = RunCapsMeter::new(RunCapLimits::unlimited());
        (BrowserBudget::new(limits, Arc::clone(&caps)), caps)
    }

    #[test]
    fn seconds_round_up_and_never_bill_zero() {
        assert_eq!(billable_seconds(Duration::ZERO), 1);
        assert_eq!(billable_seconds(Duration::from_millis(200)), 1);
        assert_eq!(billable_seconds(Duration::from_secs(2)), 2);
        assert_eq!(billable_seconds(Duration::from_millis(2_001)), 3);
    }

    #[test]
    fn no_limit_never_stops() {
        let (budget, caps) = budget(BrowserLimits::default());
        for _ in 0..100 {
            assert_eq!(budget.admit(), Ok(()));
            budget.charge(Duration::from_secs(60));
        }
        assert_eq!(caps.tripped(), None);
        assert_eq!(budget.used_seconds(), 6_000);
    }

    #[test]
    fn the_run_cap_stops_the_next_call() {
        let (budget, caps) = budget(BrowserLimits {
            max_run_seconds: 10,
            ..BrowserLimits::default()
        });
        assert_eq!(budget.admit(), Ok(()));
        budget.charge(Duration::from_secs(6));
        assert_eq!(budget.admit(), Ok(()));
        budget.charge(Duration::from_secs(5));
        assert_eq!(caps.tripped(), Some(RunCap::MaxBrowserSeconds));
        assert_eq!(budget.admit(), Err(RunCap::MaxBrowserSeconds));
    }

    #[test]
    fn the_monthly_quota_counts_earlier_runs() {
        let (budget, caps) = budget(BrowserLimits {
            max_run_seconds: 0,
            monthly_quota_seconds: 3_600,
            month_used_seconds: 3_590,
        });
        assert_eq!(budget.admit(), Ok(()));
        budget.charge(Duration::from_secs(10));
        assert_eq!(caps.tripped(), Some(RunCap::BrowserMonthlyQuota));
        assert_eq!(budget.admit(), Err(RunCap::BrowserMonthlyQuota));
    }

    #[test]
    fn a_used_up_quota_refuses_the_first_call() {
        let (budget, caps) = budget(BrowserLimits {
            max_run_seconds: 0,
            monthly_quota_seconds: 100,
            month_used_seconds: 100,
        });
        assert_eq!(budget.admit(), Err(RunCap::BrowserMonthlyQuota));
        assert_eq!(caps.tripped(), Some(RunCap::BrowserMonthlyQuota));
    }

    #[test]
    fn another_cap_reached_first_wins() {
        let caps = RunCapsMeter::new(RunCapLimits {
            max_tool_calls: 1,
            max_model_turns: 0,
            max_records_read: 0,
        });
        let budget = BrowserBudget::new(
            BrowserLimits {
                max_run_seconds: 1,
                ..BrowserLimits::default()
            },
            Arc::clone(&caps),
        );
        assert_eq!(caps.charge_tool_call(), Ok(()));
        assert_eq!(caps.charge_tool_call(), Err(RunCap::MaxToolCalls));
        assert_eq!(budget.admit(), Err(RunCap::MaxToolCalls));
        budget.charge(Duration::from_secs(5));
        assert_eq!(caps.tripped(), Some(RunCap::MaxToolCalls));
    }
}
