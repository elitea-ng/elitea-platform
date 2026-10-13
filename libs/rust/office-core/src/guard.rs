//! The dedicated parse thread and its concurrency permits (ADR-0032
//! decisions 2 and 3).
//!
//! Every call that parses or renders runs on a fresh thread with a fixed
//! [`PARSE_STACK_BYTES`] stack, never on the caller's (a tokio worker's)
//! thread. The thread holds a process-wide permit until it *exits*. A call that
//! passes its deadline returns [`OfficeError::DeadlineExceeded`], but the
//! library cannot be cancelled, so the permit stays held and a later call sees
//! [`OfficeError::Busy`] rather than piling up abandoned work. [`health`]
//! reports a permit held for more than three deadlines so the orchestrator can
//! recycle the process.

use std::panic::{AssertUnwindSafe, catch_unwind};
use std::sync::mpsc::{self, RecvTimeoutError};
use std::sync::{Mutex, MutexGuard, PoisonError};
use std::thread;
use std::time::{Duration, Instant};

use crate::error::{OfficeError, Result};
use crate::limits::{Limits, PARSE_STACK_BYTES};

struct Held {
    id: u64,
    since: Instant,
    deadline: Duration,
}

struct Gate {
    next_id: u64,
    held: Vec<Held>,
}

static GATE: Mutex<Gate> = Mutex::new(Gate {
    next_id: 0,
    held: Vec::new(),
});

fn gate() -> MutexGuard<'static, Gate> {
    GATE.lock().unwrap_or_else(PoisonError::into_inner)
}

/// A permit; released when the parse thread that owns it exits.
struct Permit(u64);

impl Drop for Permit {
    fn drop(&mut self) {
        gate().held.retain(|h| h.id != self.0);
    }
}

fn acquire(limits: &Limits) -> Result<Permit> {
    let mut g = gate();
    if g.held.len() >= limits.max_concurrent.max(1) {
        return Err(OfficeError::Busy);
    }
    let id = g.next_id;
    g.next_id += 1;
    g.held.push(Held {
        id,
        since: Instant::now(),
        deadline: limits.call_deadline,
    });
    Ok(Permit(id))
}

/// A snapshot of the concurrency gate, for a readiness probe.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Health {
    /// Parse threads alive right now.
    pub active: usize,
    /// Age of the oldest live parse thread.
    pub longest_held: Duration,
    /// True when some thread has lived more than three of its call's
    /// deadlines: a runaway library call. The worker should report itself not
    /// ready so the orchestrator recycles it.
    pub degraded: bool,
}

/// The current state of the process-wide permit gate.
#[must_use]
pub fn health() -> Health {
    let g = gate();
    let now = Instant::now();
    let mut longest = Duration::ZERO;
    let mut degraded = false;
    for h in &g.held {
        let age = now.saturating_duration_since(h.since);
        longest = longest.max(age);
        if age > h.deadline.saturating_mul(3) {
            degraded = true;
        }
    }
    Health {
        active: g.held.len(),
        longest_held: longest,
        degraded,
    }
}

/// Run `f` on a dedicated thread with the fixed stack, holding a permit until
/// the thread exits. Panics become [`OfficeError::Internal`].
pub(crate) fn run_guarded<R, F>(limits: &Limits, f: F) -> Result<R>
where
    R: Send + 'static,
    F: FnOnce() -> Result<R> + Send + 'static,
{
    let permit = acquire(limits)?;
    let (tx, rx) = mpsc::channel();
    thread::Builder::new()
        .name("office-core-parse".into())
        .stack_size(PARSE_STACK_BYTES)
        .spawn(move || {
            let _permit = permit;
            let out = catch_unwind(AssertUnwindSafe(f)).unwrap_or_else(|_| {
                Err(OfficeError::Internal("a document library panicked".into()))
            });
            let _ = tx.send(out);
        })
        .map_err(|e| OfficeError::Internal(format!("cannot start parse thread: {e}")))?;
    match rx.recv_timeout(limits.call_deadline) {
        Ok(r) => r,
        Err(RecvTimeoutError::Timeout) => Err(OfficeError::DeadlineExceeded),
        Err(RecvTimeoutError::Disconnected) => Err(OfficeError::Internal(
            "parse thread ended without a result".into(),
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn panics_become_internal_errors_and_release_the_permit() {
        let l = Limits::default();
        let r: Result<()> = run_guarded(&l, || panic!("boom"));
        assert!(matches!(r, Err(OfficeError::Internal(_))));
    }

    #[test]
    fn the_thread_has_the_fixed_stack_and_name() {
        let l = Limits::default();
        let name = run_guarded(&l, || Ok(thread::current().name().map(str::to_owned))).unwrap();
        assert_eq!(name.as_deref(), Some("office-core-parse"));
    }
}
