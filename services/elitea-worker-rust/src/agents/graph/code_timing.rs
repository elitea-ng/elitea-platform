//! Named polling and observation timings shared by the remote Code paths.
//!
//! Every interval here is transport reconciliation for an exact attempt identity.
//! Changing one never changes dispatch identity, fencing, or the absolute deadline.
use std::time::Duration;

/// Fast reconcile cadence for compile and publication observation.
pub(crate) const CODE_FAST_RECONCILE_INTERVAL: Duration = Duration::from_millis(250);
/// Steady reconcile cadence for preparation, hydration and workspace observation.
pub(crate) const CODE_RECONCILE_INTERVAL: Duration = Duration::from_secs(1);
/// Allowance added to the job timeout: Supervisor lease (60 s) plus one heartbeat (20 s)
/// plus delivery slack. Pinned against the Supervisor constants by a test.
pub(crate) const OBSERVATION_MARGIN: Duration = Duration::from_secs(90);

/// Platform pump: first idle interval, and the interval after any progress.
pub(crate) const PLATFORM_PUMP_MIN_INTERVAL: Duration = CODE_FAST_RECONCILE_INTERVAL;
/// Platform pump: idle cap. Costs at most this much latency for a call after quiet.
pub(crate) const PLATFORM_PUMP_MAX_IDLE_INTERVAL: Duration = Duration::from_secs(1);
/// Upper bound on idle pump steps in one minute (60 s / 1 s cap, plus warm-up).
#[cfg(test)]
pub(crate) const PLATFORM_PUMP_IDLE_STEPS_PER_MINUTE_BUDGET: usize = 70;

/// Takeover reconcile of a `Pending`/busy submission: first and capped interval.
pub(crate) const CODE_PENDING_INITIAL_INTERVAL: Duration = CODE_RECONCILE_INTERVAL;
pub(crate) const CODE_PENDING_MAX_INTERVAL: Duration = Duration::from_secs(5);
/// Jitter removes up to this percent of an interval so Workers do not synchronise.
pub(crate) const CODE_PENDING_JITTER_PERCENT: u64 = 25;
/// Upper bound on takeover submissions in one minute of `Pending`.
#[cfg(test)]
pub(crate) const CODE_RECONCILE_SUBMISSIONS_PER_MINUTE_BUDGET: usize = 20;

/// Next idle pump interval: doubles while idle, capped.
pub(crate) fn next_idle_interval(current: Duration) -> Duration {
    current
        .saturating_mul(2)
        .min(PLATFORM_PUMP_MAX_IDLE_INTERVAL)
}

/// Capped exponential backoff with jitter for a `Pending` exact-attempt reconcile.
pub(crate) struct PendingBackoff {
    base: Duration,
}

impl PendingBackoff {
    pub(crate) const fn new() -> Self {
        Self {
            base: CODE_PENDING_INITIAL_INTERVAL,
        }
    }

    /// `sample` is any uniform `u64`; the wait lies in `[base * (1 - jitter), base]`.
    pub(crate) fn next(&mut self, sample: u64) -> Duration {
        let base = self.base;
        self.base = base.saturating_mul(2).min(CODE_PENDING_MAX_INTERVAL);
        let millis = u64::try_from(base.as_millis()).unwrap_or(u64::MAX);
        let cut = millis / 100 * CODE_PENDING_JITTER_PERCENT * (sample % 1024) / 1024;
        base.saturating_sub(Duration::from_millis(cut))
    }
}

/// Non-cryptographic jitter sample.
pub(crate) fn jitter_sample() -> u64 {
    use ring::rand::{SecureRandom as _, SystemRandom};
    let mut bytes = [0_u8; 8];
    // A failed sample only removes jitter; the cap and deadline still bound the wait.
    let _ = SystemRandom::new().fill(&mut bytes);
    u64::from_le_bytes(bytes)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::sandbox::docker_supervisor::{HEARTBEAT_INTERVAL, LEASE_SECONDS};

    #[test]
    fn idle_interval_grows_and_is_capped() {
        let mut interval = PLATFORM_PUMP_MIN_INTERVAL;
        let mut seen = Vec::new();
        for _ in 0..5 {
            seen.push(interval);
            interval = next_idle_interval(interval);
        }
        assert_eq!(
            seen,
            [250, 500, 1000, 1000, 1000].map(Duration::from_millis)
        );
    }

    #[test]
    fn idle_steps_per_minute_stay_within_budget() {
        let minute = Duration::from_mins(1);
        let (mut elapsed, mut interval, mut steps) =
            (Duration::ZERO, PLATFORM_PUMP_MIN_INTERVAL, 0);
        while elapsed < minute {
            steps += 1;
            elapsed += interval;
            interval = next_idle_interval(interval);
        }
        assert!(
            steps <= PLATFORM_PUMP_IDLE_STEPS_PER_MINUTE_BUDGET,
            "{steps}"
        );
        // Before: a fixed 250 ms pump took 240 steps per idle minute.
        assert!(steps * 3 < 240);
    }

    #[test]
    fn pending_backoff_is_capped_jittered_and_bounded() {
        for sample in [0, 511, u64::MAX] {
            let mut backoff = PendingBackoff::new();
            let mut elapsed = Duration::ZERO;
            let mut submissions = 0;
            let mut previous = Duration::ZERO;
            while elapsed < Duration::from_mins(1) {
                let wait = backoff.next(sample);
                assert!(wait <= CODE_PENDING_MAX_INTERVAL);
                assert!(wait * 4 >= CODE_PENDING_INITIAL_INTERVAL * 3);
                if sample == 0 {
                    assert!(wait >= previous);
                }
                previous = wait;
                submissions += 1;
                elapsed += wait;
            }
            assert!(
                submissions <= CODE_RECONCILE_SUBMISSIONS_PER_MINUTE_BUDGET,
                "{submissions}"
            );
        }
    }

    #[test]
    fn observation_margin_covers_supervisor_lease_and_heartbeat() {
        let lease = Duration::from_secs(u64::try_from(LEASE_SECONDS).unwrap());
        assert!(OBSERVATION_MARGIN >= lease + HEARTBEAT_INTERVAL);
    }
}
