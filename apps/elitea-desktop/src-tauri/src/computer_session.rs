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
//! platform support), `ComputerAct` calls are refused
//! (`user_active_guard_unavailable`) — a pause nobody could notice is no
//! pause. Reads still run.

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
        self.control.begin(turn);
        let mut watch = lock(&self.watch);
        match &*watch {
            Watch::Running(_) => return Ok(()),
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

    /// Turn `turn` ended: when it held the session, the session ends and
    /// the watch stops (a refused watch is tried again next session: the
    /// person may have granted it since).
    pub fn end(&self, turn: TurnId) {
        self.control.end(turn);
        if self.control.view().phase != Phase::Idle {
            return;
        }
        let old = std::mem::take(&mut *lock(&self.watch));
        if let Watch::Running(running) = old {
            // Stopping joins the tap's thread (one run loop pass at most):
            // never on the caller's thread.
            std::thread::spawn(move || drop(running));
        }
    }
}
