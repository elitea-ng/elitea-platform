//! The computer stays awake while a local turn runs (ADR-0045 decision 7,
//! #1438).
//!
//! [`KeepAwake`] counts the turns that run (from their start until their
//! commit settles; a chain's next turn takes its hold before the previous
//! one lets go). The first hold asks the OS for a power assertion, and the
//! last release ends it. A guard ([`AwakeGuard`]) releases on drop, so a
//! cancelled turn or a panic releases it too.
//!
//! The OS side is a [`PowerBackend`]:
//! - macOS: `IOPMAssertionCreateWithName` with `PreventUserIdleSystemSleep`;
//! - Windows: `SetThreadExecutionState(ES_CONTINUOUS | ES_SYSTEM_REQUIRED)`;
//! - Linux: logind `Inhibit` of `idle` and `sleep` (`sleep:idle`) over D-Bus.
//!
//! [`OsPower`] makes the assertion on a thread of its own, which holds it
//! until the release: the Windows call is per thread, and the D-Bus call
//! blocks. A lid close on a laptop with no external display may still put
//! the computer to sleep where the OS requires it; the resume record
//! (`crate::d0::resume`) covers that.

use std::sync::{Arc, Mutex};

/// What the OS holds while the computer must stay awake: dropping it ends
/// the assertion.
pub trait PowerHold: Send {}

/// The OS's power assertion.
pub trait PowerBackend: Send + Sync {
    /// Ask the OS to keep the computer awake until the hold drops.
    ///
    /// # Errors
    ///
    /// The OS refused (or this OS has no such assertion); the turn runs
    /// anyway.
    fn acquire(&self) -> Result<Box<dyn PowerHold>, String>;
}

/// No assertion: tests, and an OS without one.
pub struct NoPower;

impl PowerBackend for NoPower {
    fn acquire(&self) -> Result<Box<dyn PowerHold>, String> {
        Err("this system has no power assertion".to_owned())
    }
}

/// Told when the assertion starts (`true`) or ends (`false`): the tray.
pub type AwakeListener = Arc<dyn Fn(bool) + Send + Sync>;

struct Inner {
    running: usize,
    hold: Option<Box<dyn PowerHold>>,
    backend: Arc<dyn PowerBackend>,
    listener: Option<AwakeListener>,
}

/// The running turns' count and the OS assertion they hold.
pub struct KeepAwake {
    inner: Mutex<Inner>,
}

impl Default for KeepAwake {
    fn default() -> Self {
        Self::new(Arc::new(NoPower))
    }
}

impl KeepAwake {
    #[must_use]
    pub fn new(backend: Arc<dyn PowerBackend>) -> Self {
        Self {
            inner: Mutex::new(Inner {
                running: 0,
                hold: None,
                backend,
                listener: None,
            }),
        }
    }

    fn inner(&self) -> std::sync::MutexGuard<'_, Inner> {
        self.inner
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// Use `backend` for the next assertion (start-up: the OS one).
    pub fn set_backend(&self, backend: Arc<dyn PowerBackend>) {
        self.inner().backend = backend;
    }

    /// Tell `listener` when the assertion starts or ends.
    pub fn set_listener(&self, listener: AwakeListener) {
        self.inner().listener = Some(listener);
    }

    /// One more turn runs: the first one asks the OS for the assertion.
    #[must_use]
    pub fn hold(self: &Arc<Self>) -> AwakeGuard {
        let changed = {
            let mut inner = self.inner();
            inner.running = inner.running.saturating_add(1);
            if inner.running == 1 && inner.hold.is_none() {
                match inner.backend.acquire() {
                    Ok(hold) => {
                        inner.hold = Some(hold);
                        inner.listener.clone()
                    }
                    Err(error) => {
                        log::warn!("the computer may sleep while a turn runs: {error}");
                        None
                    }
                }
            } else {
                None
            }
        };
        if let Some(listener) = changed {
            listener(true);
        }
        AwakeGuard {
            awake: self.clone(),
        }
    }

    fn release(&self) {
        let (hold, listener) = {
            let mut inner = self.inner();
            inner.running = inner.running.saturating_sub(1);
            if inner.running == 0 {
                let hold = inner.hold.take();
                let listener = hold.as_ref().and(inner.listener.clone());
                (hold, listener)
            } else {
                (None, None)
            }
        };
        // The OS call runs outside the lock.
        drop(hold);
        if let Some(listener) = listener {
            listener(false);
        }
    }

    /// The turns that run now.
    #[must_use]
    pub fn running(&self) -> usize {
        self.inner().running
    }

    /// The OS holds the assertion now.
    #[cfg(test)]
    #[must_use]
    pub fn is_awake(&self) -> bool {
        self.inner().hold.is_some()
    }
}

/// One running turn's hold; dropping it releases it.
pub struct AwakeGuard {
    awake: Arc<KeepAwake>,
}

impl AwakeGuard {
    /// Another hold on the same count (a chain's next turn takes it before
    /// this turn lets go).
    #[must_use]
    pub fn again(&self) -> Self {
        self.awake.hold()
    }
}

impl Drop for AwakeGuard {
    fn drop(&mut self) {
        self.awake.release();
    }
}

impl std::fmt::Debug for AwakeGuard {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("AwakeGuard")
    }
}

/// The OS's own assertion, made and ended on a thread of its own.
#[cfg(any(target_os = "macos", target_os = "windows", target_os = "linux"))]
pub struct OsPower;

#[cfg(any(target_os = "macos", target_os = "windows", target_os = "linux"))]
struct ThreadHold {
    /// Dropped: the thread ends the assertion and exits.
    _release: std::sync::mpsc::Sender<()>,
}

#[cfg(any(target_os = "macos", target_os = "windows", target_os = "linux"))]
impl PowerHold for ThreadHold {}

#[cfg(any(target_os = "macos", target_os = "windows", target_os = "linux"))]
impl PowerBackend for OsPower {
    fn acquire(&self) -> Result<Box<dyn PowerHold>, String> {
        let (release, released) = std::sync::mpsc::channel::<()>();
        let (made, answer) = std::sync::mpsc::channel::<Result<(), String>>();
        std::thread::Builder::new()
            .name("keep-awake".into())
            .spawn(move || {
                let builder = keepawake::Builder::default()
                    .idle(true)
                    // logind: `sleep:idle`. Elsewhere `sleep` would be a
                    // stronger assertion than the ADR asks for.
                    .sleep(cfg!(target_os = "linux"))
                    .reason("A local agent turn is running")
                    .app_name("Elitea")
                    .app_reverse_domain("ai.elitea.desktop")
                    .create();
                match builder {
                    Ok(assertion) => {
                        let _ = made.send(Ok(()));
                        // Until the hold drops (its sender closes).
                        let _ = released.recv();
                        drop(assertion);
                    }
                    Err(error) => {
                        let _ = made.send(Err(error.to_string()));
                    }
                }
            })
            .map_err(|e| format!("could not start the keep-awake thread: {e}"))?;
        answer
            .recv()
            .map_err(|_| "the keep-awake thread stopped".to_owned())??;
        Ok(Box::new(ThreadHold { _release: release }))
    }
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicUsize, Ordering};

    use super::*;

    /// Counts the assertions the OS would hold now, and every one made.
    #[derive(Default)]
    struct FakeOs {
        held: Arc<AtomicUsize>,
        made: AtomicUsize,
        refuse: std::sync::atomic::AtomicBool,
    }

    struct FakeHold(Arc<AtomicUsize>);

    impl PowerHold for FakeHold {}

    impl Drop for FakeHold {
        fn drop(&mut self) {
            self.0.fetch_sub(1, Ordering::SeqCst);
        }
    }

    impl PowerBackend for FakeOs {
        fn acquire(&self) -> Result<Box<dyn PowerHold>, String> {
            if self.refuse.load(Ordering::SeqCst) {
                return Err("refused".into());
            }
            self.made.fetch_add(1, Ordering::SeqCst);
            self.held.fetch_add(1, Ordering::SeqCst);
            Ok(Box::new(FakeHold(self.held.clone())))
        }
    }

    #[test]
    fn the_first_turn_asks_and_the_last_one_releases() {
        let os = Arc::new(FakeOs::default());
        let awake = Arc::new(KeepAwake::new(os.clone()));
        let seen = Arc::new(Mutex::new(Vec::new()));
        awake.set_listener({
            let seen = seen.clone();
            Arc::new(move |on| seen.lock().unwrap().push(on))
        });
        assert_eq!(os.held.load(Ordering::SeqCst), 0);
        let first = awake.hold();
        let second = awake.hold();
        assert_eq!(awake.running(), 2);
        assert!(awake.is_awake());
        assert_eq!(os.made.load(Ordering::SeqCst), 1, "one assertion for both");
        drop(first);
        assert_eq!(os.held.load(Ordering::SeqCst), 1, "a turn still runs");
        // A chain's next turn holds before the previous lets go.
        let next = second.again();
        drop(second);
        assert_eq!(os.held.load(Ordering::SeqCst), 1);
        drop(next);
        assert_eq!(os.held.load(Ordering::SeqCst), 0);
        assert_eq!(awake.running(), 0);
        assert!(!awake.is_awake());
        assert_eq!(*seen.lock().unwrap(), vec![true, false]);
        // The next run asks again.
        let again = awake.hold();
        assert_eq!(os.made.load(Ordering::SeqCst), 2);
        drop(again);
        assert_eq!(*seen.lock().unwrap(), vec![true, false, true, false]);
    }

    #[test]
    fn a_panic_releases_the_hold_and_a_refusal_still_counts_the_turn() {
        let os = Arc::new(FakeOs::default());
        let awake = Arc::new(KeepAwake::new(os.clone()));
        let held = awake.clone();
        let panicked = std::thread::spawn(move || {
            let _guard = held.hold();
            panic!("the turn failed");
        })
        .join();
        assert!(panicked.is_err());
        assert_eq!(os.held.load(Ordering::SeqCst), 0);
        assert_eq!(awake.running(), 0);

        os.refuse.store(true, Ordering::SeqCst);
        let guard = awake.hold();
        assert_eq!(awake.running(), 1, "the turn runs without the assertion");
        assert!(!awake.is_awake());
        drop(guard);
        assert_eq!(awake.running(), 0);
    }
}
