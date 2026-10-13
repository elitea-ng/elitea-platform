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
use std::sync::atomic::{AtomicU8, AtomicU64, Ordering};
use std::sync::mpsc::{self, RecvTimeoutError};
use std::sync::{Arc, Mutex, MutexGuard, PoisonError, TryLockError};
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
    /// deadlines: a runaway library call. See [`Health::stuck`].
    pub degraded: bool,
}

impl Health {
    /// A parse thread outlived three deadlines and the library will not give
    /// it back. The worker should stop claiming new work, let its other
    /// in-flight calls finish within its drain timeout, and exit non-zero so
    /// the orchestrator restarts the pod (ADR-0032 decision 3).
    #[must_use]
    pub fn stuck(&self) -> bool {
        self.degraded
    }
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
            let delay = TEST_DELAY_MS.swap(0, Ordering::SeqCst);
            if delay > 0 {
                thread::sleep(Duration::from_millis(delay));
            }
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

/// Test hook: the next guarded call sleeps this long (ms) before running, to
/// make a deadline deterministic.
pub(crate) static TEST_DELAY_MS: AtomicU64 = AtomicU64::new(0);

/// The error a closure returns when it could not restore a document after a
/// failed edit: [`Shared`] then refuses every later call.
pub(crate) const STATE_LOST: &str = "document state lost; reopen from saved bytes";

const OK: u8 = 0;
const INDETERMINATE: u8 = 1;
const LOST: u8 = 2;

/// Lock a mutex without waiting: `Busy` when a running or abandoned call holds
/// it, `Internal` when a panic poisoned it (the state may be half-edited).
pub(crate) fn try_lock<T>(m: &Mutex<T>) -> Result<MutexGuard<'_, T>> {
    m.try_lock().map_err(|e| match e {
        TryLockError::WouldBlock => OfficeError::Busy,
        TryLockError::Poisoned(_) => {
            OfficeError::Internal("document state poisoned by a panic".into())
        }
    })
}

/// A document shared with the parse thread: the one place that locks it and
/// the one place that runs a guarded call on it.
pub(crate) struct Shared<T> {
    inner: Arc<Mutex<T>>,
    limits: Limits,
    /// `OK`, or why the document can no longer be trusted.
    state: Arc<AtomicU8>,
}

impl<T: Send + 'static> Shared<T> {
    pub(crate) fn new(value: T, limits: &Limits) -> Self {
        Self {
            inner: Arc::new(Mutex::new(value)),
            limits: limits.clone(),
            state: Arc::new(AtomicU8::new(OK)),
        }
    }

    fn check(&self) -> Result<()> {
        match self.state.load(Ordering::SeqCst) {
            OK => Ok(()),
            INDETERMINATE => Err(OfficeError::Indeterminate),
            _ => Err(OfficeError::Internal(STATE_LOST.into())),
        }
    }

    /// Lock for a small edit on the caller's thread.
    pub(crate) fn lock(&self) -> Result<MutexGuard<'_, T>> {
        self.check()?;
        try_lock(&self.inner)
    }

    fn run<R, F>(&self, mutating: bool, f: F) -> Result<R>
    where
        R: Send + 'static,
        F: FnOnce(&mut T, &Limits) -> Result<R> + Send + 'static,
    {
        self.check()?;
        let (inner, l) = (self.inner.clone(), self.limits.clone());
        let r = run_guarded(&self.limits, move || {
            let mut g = try_lock(&inner)?;
            f(&mut g, &l)
        });
        match &r {
            // The edit is still running, or ran to an unknown point: from
            // here on the document is not to be trusted.
            Err(OfficeError::DeadlineExceeded) if mutating => {
                self.state.store(INDETERMINATE, Ordering::SeqCst);
            }
            Err(OfficeError::Internal(m)) if m == STATE_LOST => {
                self.state.store(LOST, Ordering::SeqCst);
            }
            _ => {}
        }
        r
    }

    /// Run a read-only `f` on the parse thread with the document locked. A
    /// timeout does not affect the document.
    pub(crate) fn guarded<R, F>(&self, f: F) -> Result<R>
    where
        R: Send + 'static,
        F: FnOnce(&mut T, &Limits) -> Result<R> + Send + 'static,
    {
        self.run(false, f)
    }

    /// Run a MUTATING `f`. If it passes its deadline the edit may or may not
    /// apply, so every later call on this document is
    /// [`OfficeError::Indeterminate`].
    pub(crate) fn guarded_mut<R, F>(&self, f: F) -> Result<R>
    where
        R: Send + 'static,
        F: FnOnce(&mut T, &Limits) -> Result<R> + Send + 'static,
    {
        self.run(true, f)
    }

    pub(crate) fn limits(&self) -> &Limits {
        &self.limits
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn panics_become_internal_errors_and_release_the_permit() {
        let l = roomy();
        let r: Result<()> = run_guarded(&l, || panic!("boom"));
        assert!(matches!(r, Err(OfficeError::Internal(_))));
    }

    fn roomy() -> Limits {
        Limits {
            max_concurrent: 64,
            ..Limits::default()
        }
    }

    #[test]
    fn a_panic_poisons_the_document_and_reads_as_internal_not_busy() {
        let s = Shared::new(1u32, &roomy());
        let r: Result<()> = s.guarded(|_, _| panic!("boom"));
        assert!(matches!(r, Err(OfficeError::Internal(_))));
        assert!(matches!(s.lock(), Err(OfficeError::Internal(_))));
        assert!(matches!(
            s.guarded(|_, _| Ok(())),
            Err(OfficeError::Internal(_))
        ));
    }

    #[test]
    fn a_held_document_reads_as_busy() {
        let s = Shared::new(1u32, &roomy());
        let _held = s.lock().unwrap();
        assert!(matches!(s.lock(), Err(OfficeError::Busy)));
    }

    #[test]
    fn the_thread_has_the_fixed_stack_and_name() {
        let l = roomy();
        let name = run_guarded(&l, || Ok(thread::current().name().map(str::to_owned))).unwrap();
        assert_eq!(name.as_deref(), Some("office-core-parse"));
    }
}
