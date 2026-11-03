//! The computer stays awake while a local turn runs (ADR-0045 decision 7,
//! #1438).
//!
//! [`KeepAwake`] counts the turns that run (from their start until their
//! commit settles; a chain's next turn takes its hold before the previous
//! one lets go). While the count is above 0 the OS holds a power assertion
//! against **idle** sleep only: the person can still put the computer to
//! sleep, and the display can still sleep. A guard ([`AwakeGuard`])
//! releases on drop, so a cancelled turn or a panic releases it too.
//!
//! The OS side is a [`PowerBackend`]:
//! - macOS: `IOPMAssertionCreateWithName` with `PreventUserIdleSystemSleep`;
//! - Windows: `SetThreadExecutionState(ES_CONTINUOUS | ES_SYSTEM_REQUIRED)`;
//! - Linux: logind `Inhibit("idle", …, "block")` over D-Bus.
//!
//! The count is a lock of its own that no OS call ever runs under: a
//! worker thread of [`KeepAwake`] (never a tokio worker) makes and ends the
//! assertion. A hold or a release only changes the count and wakes the
//! worker, which then brings the assertion in line with the count (so a
//! quick hold and release in a row end with no assertion). [`OsPower`]
//! makes the assertion on a thread of its own, which holds it until the
//! release: the Windows call is per thread, and the D-Bus call blocks. A lid
//! close on a laptop with no external display may still put the computer to
//! sleep where the OS requires it; the resume record (`crate::d0::resume`)
//! covers that.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, mpsc};

/// What the OS holds while the computer must stay awake: dropping it ends
/// the assertion.
pub trait PowerHold: Send {}

/// The OS's power assertion.
pub trait PowerBackend: Send + Sync {
    /// Ask the OS to keep the computer awake until the hold drops. May
    /// block (it runs on the keep-awake worker only).
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

/// What the worker and the count share.
struct Shared {
    running: Mutex<usize>,
    backend: Mutex<Arc<dyn PowerBackend>>,
    listener: Mutex<Option<AwakeListener>>,
    awake: AtomicBool,
}

fn locked<T>(mutex: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    mutex
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

enum Wake {
    /// The count changed: bring the assertion in line.
    Count,
    /// Tests: answer once every earlier wake was handled.
    #[cfg(test)]
    Settled(mpsc::Sender<()>),
}

/// The running turns' count and the OS assertion they hold.
pub struct KeepAwake {
    shared: Arc<Shared>,
    /// The worker's queue, started on the first hold.
    worker: Mutex<Option<mpsc::Sender<Wake>>>,
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
            shared: Arc::new(Shared {
                running: Mutex::new(0),
                backend: Mutex::new(backend),
                listener: Mutex::new(None),
                awake: AtomicBool::new(false),
            }),
            worker: Mutex::new(None),
        }
    }

    /// Use `backend` for the next assertion (start-up: the OS one).
    pub fn set_backend(&self, backend: Arc<dyn PowerBackend>) {
        *locked(&self.shared.backend) = backend;
    }

    /// Tell `listener` when the assertion starts or ends (on the worker
    /// thread).
    pub fn set_listener(&self, listener: AwakeListener) {
        *locked(&self.shared.listener) = Some(listener);
    }

    /// One more turn runs: the first one has the worker ask the OS for the
    /// assertion. Never waits on the OS.
    #[must_use]
    pub fn hold(self: &Arc<Self>) -> AwakeGuard {
        let first = {
            let mut running = locked(&self.shared.running);
            *running = running.saturating_add(1);
            *running == 1
        };
        if first {
            self.wake(Wake::Count);
        }
        AwakeGuard {
            awake: self.clone(),
        }
    }

    fn release(&self) {
        let last = {
            let mut running = locked(&self.shared.running);
            *running = running.saturating_sub(1);
            *running == 0
        };
        if last {
            self.wake(Wake::Count);
        }
    }

    /// Queue `wake` for the worker, starting it when it is not running.
    fn wake(&self, wake: Wake) {
        let mut worker = locked(&self.worker);
        let wake = match worker.as_ref() {
            Some(queue) => match queue.send(wake) {
                Ok(()) => return,
                Err(mpsc::SendError(wake)) => wake,
            },
            None => wake,
        };
        let (queue, waiting) = mpsc::channel();
        let shared = self.shared.clone();
        let started = std::thread::Builder::new()
            .name("keep-awake".into())
            .spawn(move || work(&shared, &waiting));
        match started {
            Ok(_) => {
                let _ = queue.send(wake);
                *worker = Some(queue);
            }
            Err(error) => {
                log::warn!("the computer may sleep while a turn runs: {error}");
            }
        }
    }

    /// The turns that run now. Never waits on the OS.
    #[must_use]
    pub fn running(&self) -> usize {
        *locked(&self.shared.running)
    }

    /// The OS holds the assertion now.
    #[cfg(test)]
    #[must_use]
    pub fn is_awake(&self) -> bool {
        self.shared.awake.load(Ordering::SeqCst)
    }

    /// Tests: once the worker handled every earlier hold and release.
    #[cfg(test)]
    pub fn settled(&self) {
        let (answer, answered) = mpsc::channel();
        self.wake(Wake::Settled(answer));
        let _ = answered.recv_timeout(std::time::Duration::from_secs(5));
    }
}

/// The worker: on each wake, make or end the assertion to match the count.
/// It ends (and drops the assertion) when the [`KeepAwake`] goes.
fn work(shared: &Shared, waiting: &mpsc::Receiver<Wake>) {
    let mut hold: Option<Box<dyn PowerHold>> = None;
    while let Ok(wake) = waiting.recv() {
        match wake {
            Wake::Count => {}
            #[cfg(test)]
            Wake::Settled(answer) => {
                let _ = answer.send(());
                continue;
            }
        }
        let want = *locked(&shared.running) > 0;
        let changed = if want && hold.is_none() {
            let backend = locked(&shared.backend).clone();
            match backend.acquire() {
                Ok(made) => {
                    hold = Some(made);
                    true
                }
                Err(error) => {
                    log::warn!("the computer may sleep while a turn runs: {error}");
                    false
                }
            }
        } else if !want && hold.is_some() {
            hold = None;
            true
        } else {
            false
        };
        if changed {
            let on = hold.is_some();
            shared.awake.store(on, Ordering::SeqCst);
            let listener = locked(&shared.listener).clone();
            if let Some(listener) = listener {
                listener(on);
            }
        }
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
                // Idle sleep only (ADR-0045 decision 7): `idle` is macOS's
                // PreventUserIdleSystemSleep, Windows' ES_SYSTEM_REQUIRED and
                // logind's `idle` inhibitor. `sleep` (PreventSystemSleep,
                // ES_AWAYMODE_REQUIRED, logind `sleep`) would also block the
                // person's own suspend, and `display` keeps the screen on.
                let builder = keepawake::Builder::default()
                    .idle(true)
                    .sleep(false)
                    .display(false)
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
    use std::sync::atomic::AtomicUsize;

    use super::*;

    /// Counts the assertions the OS would hold now, and every one made.
    #[derive(Default)]
    struct FakeOs {
        held: Arc<AtomicUsize>,
        made: AtomicUsize,
        refuse: AtomicBool,
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
        awake.settled();
        assert_eq!(awake.running(), 2);
        assert!(awake.is_awake());
        assert_eq!(os.made.load(Ordering::SeqCst), 1, "one assertion for both");
        drop(first);
        awake.settled();
        assert_eq!(os.held.load(Ordering::SeqCst), 1, "a turn still runs");
        // A chain's next turn holds before the previous lets go.
        let next = second.again();
        drop(second);
        awake.settled();
        assert_eq!(os.held.load(Ordering::SeqCst), 1);
        drop(next);
        awake.settled();
        assert_eq!(os.held.load(Ordering::SeqCst), 0);
        assert_eq!(awake.running(), 0);
        assert!(!awake.is_awake());
        assert_eq!(*seen.lock().unwrap(), vec![true, false]);
        // The next run asks again.
        let again = awake.hold();
        awake.settled();
        assert_eq!(os.made.load(Ordering::SeqCst), 2);
        drop(again);
        awake.settled();
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
        awake.settled();
        assert_eq!(os.held.load(Ordering::SeqCst), 0);
        assert_eq!(awake.running(), 0);

        os.refuse.store(true, Ordering::SeqCst);
        let guard = awake.hold();
        awake.settled();
        assert_eq!(awake.running(), 1, "the turn runs without the assertion");
        assert!(!awake.is_awake());
        drop(guard);
        assert_eq!(awake.running(), 0);
    }

    /// An OS call that does not answer until the test lets it.
    struct SlowOs {
        go: Mutex<mpsc::Receiver<()>>,
        held: Arc<AtomicUsize>,
    }

    impl PowerBackend for SlowOs {
        fn acquire(&self) -> Result<Box<dyn PowerHold>, String> {
            let _ = self.go.lock().unwrap().recv();
            self.held.fetch_add(1, Ordering::SeqCst);
            Ok(Box::new(FakeHold(self.held.clone())))
        }
    }

    /// A blocked OS call never blocks a hold, a release or the count (the
    /// window's close and the quit read it on the main thread), and the
    /// assertion ends up in line with the count.
    #[test]
    fn a_slow_os_call_never_blocks_the_count() {
        let (go, waiting) = mpsc::channel();
        let held = Arc::new(AtomicUsize::new(0));
        let awake = Arc::new(KeepAwake::new(Arc::new(SlowOs {
            go: Mutex::new(waiting),
            held: held.clone(),
        })));
        let guard = awake.hold();
        assert_eq!(awake.running(), 1);
        drop(guard);
        assert_eq!(awake.running(), 0);
        let again = awake.hold();
        assert_eq!(awake.running(), 1);
        // The OS answers now: one assertion, for the turn still running.
        go.send(()).unwrap();
        awake.settled();
        assert!(awake.is_awake());
        assert_eq!(held.load(Ordering::SeqCst), 1);
        drop(again);
        awake.settled();
        assert!(!awake.is_awake());
        assert_eq!(held.load(Ordering::SeqCst), 0);
    }
}
