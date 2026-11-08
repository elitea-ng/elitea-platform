//! The computer session of the app (ADR-0047 decision 6, #1584): the
//! session control ([`crate::computer_control`]) and the input watch that
//! feeds it.
//!
//! The watch (the driver's listen-only event tap) runs only while a turn
//! holds a session: it starts with the session's first call and stops when
//! the session ends. Each of the person's inputs pauses the session (the
//! user-active guard); the stop shortcut, Control-Option-Escape, stops it.
//! The tap sees the shortcut from any app, so no global-shortcut plugin is
//! needed, and nothing listens while no session runs.
//!
//! An act needs the guard: when the OS refuses the watch (no grant, no
//! platform support), or the running watch dies on its own (its thread
//! ended: the display went away, the OS dropped its hook), `ComputerAct`
//! calls are refused (`user_active_guard_unavailable`) — a pause nobody
//! could notice is no pause. Reads still run. A dead watch is tried again
//! next session, as a refused one is.

use std::sync::{Arc, Mutex};

use elitea_desktop_control::{DesktopDriver, InputFilter, InputWatch, Signal};

use crate::computer_control::{ComputerControl, Phase};
use crate::control_state::{TurnId, lock};

/// The session control and its input watch.
pub struct ComputerSession {
    control: Arc<ComputerControl>,
    driver: Arc<dyn DesktopDriver>,
    watch: Mutex<Watch>,
}

#[derive(Default)]
enum Watch {
    #[default]
    Off,
    Running(Box<dyn InputWatch>),
    /// The OS refused it; the reason.
    Refused(String),
}

impl ComputerSession {
    #[must_use]
    pub fn new(driver: Arc<dyn DesktopDriver>, control: Arc<ComputerControl>) -> Self {
        Self {
            control,
            driver,
            watch: Mutex::new(Watch::Off),
        }
    }

    #[must_use]
    pub fn control(&self) -> &Arc<ComputerControl> {
        &self.control
    }

    /// Turn `turn`'s call starts: it holds the session, and the watch runs.
    ///
    /// # Errors
    /// Why the user-active guard cannot run (the call must not act).
    pub fn begin(&self, turn: TurnId) -> Result<(), String> {
        // The watch's lock first, then the control, here and in `end`: a
        // turn ending meanwhile never stops the watch a joining turn needs.
        let mut watch = lock(&self.watch);
        self.control.begin(turn);
        match &*watch {
            Watch::Running(running) if running.alive() => return Ok(()),
            Watch::Running(_) => {
                let why = "the user-active guard's input watch stopped".to_owned();
                log::warn!(target: "computer", "{why}");
                if let Watch::Running(dead) =
                    std::mem::replace(&mut *watch, Watch::Refused(why.clone()))
                {
                    std::thread::spawn(move || drop(dead));
                }
                return Err(why);
            }
            Watch::Refused(why) => return Err(why.clone()),
            Watch::Off => {}
        }
        let control = self.control.clone();
        let sink = Box::new(move |signal: Signal| match signal {
            Signal::UserActive => control.person_input(),
            Signal::Stop => control.stop(),
        });
        // This process is Elitea; its helper processes are not input targets.
        let filter = InputFilter::new(vec![std::process::id()]);
        match self.driver.watch_input(filter, sink) {
            Ok(running) => {
                *watch = Watch::Running(running);
                Ok(())
            }
            Err(error) => {
                let why = error.to_string();
                log::warn!(target: "computer", "the user-active guard cannot run: {why}");
                *watch = Watch::Refused(why.clone());
                Err(why)
            }
        }
    }

    /// Turn `turn` ended: it leaves the session. When it was the last one,
    /// the session ends and the watch stops (a refused watch is tried again
    /// next session: the person may have granted it since). The leaving and
    /// the stop are one step under the watch's lock, so a turn joining at
    /// the same moment keeps the watch.
    pub fn end(&self, turn: TurnId) {
        let old = {
            let mut watch = lock(&self.watch);
            self.control.end(turn);
            if self.control.view().phase != Phase::Idle {
                return;
            }
            std::mem::take(&mut *watch)
        };
        if let Watch::Running(running) = old {
            // Stopping joins the tap's thread (one run loop pass at most):
            // never on the caller's thread.
            std::thread::spawn(move || drop(running));
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::time::Duration;

    use elitea_desktop_control::{DesktopError, InputWatch, SignalSink, Unsupported};

    use super::*;

    /// A driver whose watch counts its live handles; `dead` ends the
    /// thread of every watch it started (a fake watch dying on its own).
    struct Watched {
        live: Arc<AtomicUsize>,
        starts: AtomicUsize,
        dead: Arc<std::sync::atomic::AtomicBool>,
    }

    struct Handle(Arc<AtomicUsize>, Arc<std::sync::atomic::AtomicBool>);
    impl InputWatch for Handle {
        fn alive(&self) -> bool {
            !self.1.load(Ordering::SeqCst)
        }
    }
    impl Drop for Handle {
        fn drop(&mut self) {
            self.0.fetch_sub(1, Ordering::SeqCst);
        }
    }

    impl Watched {
        fn new() -> Arc<Self> {
            Arc::new(Self {
                live: Arc::new(AtomicUsize::new(0)),
                starts: AtomicUsize::new(0),
                dead: Arc::default(),
            })
        }
    }

    // Everything but the watch is the unsupported driver's.
    impl DesktopDriver for Watched {
        fn permissions(&self) -> elitea_desktop_control::Permissions {
            Unsupported.permissions()
        }
        fn screen(&self) -> elitea_desktop_control::ScreenStatus {
            Unsupported.screen()
        }
        fn secure_input(&self) -> bool {
            false
        }
        fn list_apps(&self) -> Result<Vec<elitea_desktop_control::AppInfo>, DesktopError> {
            Unsupported.list_apps()
        }
        fn list_windows(
            &self,
            b: Option<&str>,
        ) -> Result<Vec<elitea_desktop_control::WindowInfo>, DesktopError> {
            Unsupported.list_windows(b)
        }
        fn snapshot(
            &self,
            t: &elitea_desktop_control::WindowTarget,
            b: &elitea_desktop_control::SnapshotBudget,
        ) -> Result<elitea_desktop_control::Snapshot, DesktopError> {
            Unsupported.snapshot(t, b)
        }
        fn act(
            &self,
            b: &str,
            r: &elitea_desktop_control::ElementRef,
            a: &elitea_desktop_control::Action,
        ) -> Result<elitea_desktop_control::ActionOutcome, DesktopError> {
            Unsupported.act(b, r, a)
        }
        fn type_text(
            &self,
            b: &str,
            r: Option<&elitea_desktop_control::ElementRef>,
            t: &str,
        ) -> Result<elitea_desktop_control::ActionOutcome, DesktopError> {
            Unsupported.type_text(b, r, t)
        }
        fn key(
            &self,
            t: &elitea_desktop_control::WindowTarget,
            c: &elitea_desktop_control::KeyChord,
        ) -> Result<elitea_desktop_control::ActionOutcome, DesktopError> {
            Unsupported.key(t, c)
        }
        fn click_at(
            &self,
            c: &elitea_desktop_control::ClickAt,
        ) -> Result<elitea_desktop_control::ActionOutcome, DesktopError> {
            Unsupported.click_at(c)
        }
        fn open_app(&self, b: &str) -> Result<elitea_desktop_control::OpenedApp, DesktopError> {
            Unsupported.open_app(b)
        }
        fn capture(
            &self,
            t: &elitea_desktop_control::CaptureTarget,
        ) -> Result<elitea_desktop_control::Capture, DesktopError> {
            Unsupported.capture(t)
        }
        fn wait(
            &self,
            c: &elitea_desktop_control::WaitCondition,
            t: Duration,
        ) -> Result<(), DesktopError> {
            Unsupported.wait(c, t)
        }
        fn synthetic_marker(&self) -> elitea_desktop_control::SyntheticMarker {
            Unsupported.synthetic_marker()
        }
        fn forget_snapshots(&self) {}
        fn watch_input(
            &self,
            _f: InputFilter,
            _s: SignalSink,
        ) -> Result<Box<dyn InputWatch>, DesktopError> {
            self.starts.fetch_add(1, Ordering::SeqCst);
            self.live.fetch_add(1, Ordering::SeqCst);
            Ok(Box::new(Handle(self.live.clone(), self.dead.clone())))
        }
    }

    fn until(what: &str, holds: impl Fn() -> bool) {
        let end = std::time::Instant::now() + Duration::from_secs(5);
        while !holds() {
            assert!(std::time::Instant::now() < end, "{what}");
            std::thread::sleep(Duration::from_millis(2));
        }
    }

    /// The session's last turn ends while a new turn joins. Both wait on the
    /// watch's lock (the test holds it), then run in either order: whichever
    /// runs first, a running session has its watch. Without one step for the
    /// leaving and the stop, the join could land between `end`'s Idle check
    /// and its stop, and the joiner's session ran with no guard.
    #[test]
    fn a_turn_joining_while_the_last_one_ends_keeps_its_watch() {
        for round in 0..30 {
            let driver = Watched::new();
            let session = Arc::new(ComputerSession::new(driver.clone(), Arc::default()));
            session.begin(1).unwrap();
            let held = lock(&session.watch);
            let ending = {
                let session = session.clone();
                std::thread::spawn(move || session.end(1))
            };
            std::thread::sleep(Duration::from_millis(5));
            let joining = {
                let session = session.clone();
                std::thread::spawn(move || session.begin(2))
            };
            std::thread::sleep(Duration::from_millis(5));
            drop(held);
            ending.join().unwrap();
            joining.join().unwrap().unwrap();
            assert_eq!(
                session.control().view().phase,
                Phase::Active,
                "round {round}"
            );
            // The old watch's handle is dropped on a thread of its own.
            until("one watch left", || driver.live.load(Ordering::SeqCst) <= 1);
            std::thread::sleep(Duration::from_millis(5));
            assert_eq!(
                driver.live.load(Ordering::SeqCst),
                1,
                "round {round}: the session has its watch"
            );
            session.end(2);
            until("the watch stopped", || {
                driver.live.load(Ordering::SeqCst) == 0
            });
        }
    }

    #[test]
    fn a_watch_that_died_refuses_the_session_until_the_next_one() {
        let driver = Watched::new();
        let session = ComputerSession::new(driver.clone(), Arc::default());
        session.begin(1).unwrap();
        // The watch's thread ends on its own mid-session.
        driver.dead.store(true, Ordering::SeqCst);
        let why = session.begin(1).unwrap_err();
        assert!(why.contains("stopped"), "{why}");
        // It stays refused for the session, and is not restarted.
        assert!(session.begin(2).is_err());
        assert_eq!(driver.starts.load(Ordering::SeqCst), 1);
        until("the dead watch dropped", || {
            driver.live.load(Ordering::SeqCst) == 0
        });
        session.end(1);
        session.end(2);
        // The next session tries again.
        driver.dead.store(false, Ordering::SeqCst);
        session.begin(3).unwrap();
        assert_eq!(driver.starts.load(Ordering::SeqCst), 2);
        session.end(3);
    }

    #[test]
    fn two_turns_share_one_watch_until_the_last_one_ends() {
        let driver = Watched::new();
        let session = ComputerSession::new(driver.clone(), Arc::default());
        session.begin(1).unwrap();
        session.begin(2).unwrap();
        assert_eq!(driver.starts.load(Ordering::SeqCst), 1);
        session.end(1);
        std::thread::sleep(Duration::from_millis(20));
        assert_eq!(
            driver.live.load(Ordering::SeqCst),
            1,
            "turn 2 still uses it"
        );
        session.end(2);
        until("the watch stopped", || {
            driver.live.load(Ordering::SeqCst) == 0
        });
    }
}
