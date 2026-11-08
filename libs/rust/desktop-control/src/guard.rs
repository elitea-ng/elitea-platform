//! The gates every driver call passes before it touches anything (ADR-0047
//! decision 5, "Hard limits the agent cannot lift"). They hold in bypass
//! mode too: the driver checks them itself, whoever calls it.
//!
//! - **Locked or idle screen.** No call except
//!   [`DesktopDriver::permissions`](crate::DesktopDriver::permissions) runs
//!   while the screen is locked, the screensaver is up, the display sleeps,
//!   or this session does not own the console (`screen_unavailable`).
//! - **Secure input.** While macOS secure event input is on, every act
//!   (`act` of any kind, `type_text`, `key`) is refused
//!   (`secure_field_refused`).
//! - **No Elitea control.** Elitea's own windows are never listed,
//!   snapshotted, acted on, typed or keyed into, or captured
//!   (`own_window_refused`).
//! - **Only the named app.** Input with no element target (untargeted
//!   typing, a key chord, a coordinate click) goes only into the app the
//!   call names, and only while it is frontmost (`not_frontmost`).
//!
//! The OS reads sit behind [`HostState`], so the gate order is unit-tested
//! with a stub.

// The act-only gates have their one caller in the macOS driver.
#![cfg_attr(
    not(any(target_os = "macos", target_os = "windows", target_os = "linux")),
    allow(dead_code)
)]

use crate::{DesktopError, Rect};

/// The bundle id of the app this library runs in, set once by the host at
/// startup ([`set_app_bundle_id`]): the app is branded at build time
/// (ADR-0052), so the library names no app of its own.
static APP_BUNDLE_ID: std::sync::OnceLock<String> = std::sync::OnceLock::new();

/// Register the host app's bundle id, which [`OwnApps::default`] then counts
/// as its own. The first call wins; later calls are ignored.
pub fn set_app_bundle_id(bundle_id: &str) {
    let bundle_id = bundle_id.trim();
    if !bundle_id.is_empty() {
        let _ = APP_BUNDLE_ID.set(bundle_id.to_owned());
    }
}

/// The host app's bundle id, when it registered one.
#[must_use]
pub fn app_bundle_id() -> Option<&'static str> {
    APP_BUNDLE_ID.get().map(String::as_str)
}

/// The frontmost process while the screensaver is up.
const SCREENSAVER_BUNDLE_ID: &str = "com.apple.ScreenSaver.Engine";
const SCREENSAVER_NAME: &str = "ScreenSaverEngine";

/// What the OS says about the screen, read before each call. Each field is
/// one independent OS flag, so they stay bools.
#[allow(clippy::struct_excessive_bools)]
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct ScreenStatus {
    /// The process runs inside a window-server login session.
    pub session_present: bool,
    /// That session owns the console (false while another user is switched in).
    pub on_console: bool,
    /// The lock screen is up.
    pub locked: bool,
    /// The screensaver is the frontmost process.
    pub screensaver: bool,
    /// The main display sleeps.
    pub display_asleep: bool,
    /// Whether the lock screen is up cannot be told (Linux with no
    /// screensaver service that answers): the screen counts as locked.
    pub lock_state_unknown: bool,
}

impl ScreenStatus {
    /// A usable screen: a console session, unlocked, awake, no screensaver.
    #[must_use]
    pub fn available() -> Self {
        Self {
            session_present: true,
            on_console: true,
            ..Self::default()
        }
    }

    /// Why the screen cannot be used, or `None` when it can.
    #[must_use]
    pub fn unavailable_reason(&self) -> Option<&'static str> {
        if !self.session_present {
            Some("no window-server session")
        } else if !self.on_console {
            Some("the session is not on the console")
        } else if self.locked {
            Some("the screen is locked")
        } else if self.screensaver {
            Some("the screensaver is up")
        } else if self.display_asleep {
            Some("the display is asleep")
        } else if self.lock_state_unknown {
            Some(
                "whether the screen is locked cannot be read (no screensaver service answers), so it counts as locked",
            )
        } else {
            None
        }
    }
}

/// Whether the frontmost app is the macOS screensaver.
#[must_use]
pub fn is_screensaver(bundle_id: Option<&str>, name: &str) -> bool {
    bundle_id.is_some_and(|b| b.eq_ignore_ascii_case(SCREENSAVER_BUNDLE_ID))
        || name == SCREENSAVER_NAME
}

/// The OS reads the gates need. The macOS driver reads them live; tests
/// stub them.
pub(crate) trait HostState {
    fn screen(&self) -> ScreenStatus;
    /// Whether macOS secure event input is on (a password prompt anywhere).
    fn secure_input(&self) -> bool;
    /// Whether the input watch this driver last started has died (its
    /// thread ended on its own). `false` while it runs, and before any.
    fn guard_dead(&self) -> bool {
        false
    }
}

/// The gate of every call except `permissions`: `screen_unavailable`.
pub(crate) fn before_call(host: &impl HostState) -> Result<(), DesktopError> {
    match host.screen().unavailable_reason() {
        Some(reason) => Err(DesktopError::ScreenUnavailable(reason.to_owned())),
        None => Ok(()),
    }
}

/// The gate of every act (`act` of any kind, `type_text`, `key`): the
/// screen, then the user-active guard's watch, then secure input. It runs
/// before any ref is resolved, so no act kind gets past them.
pub(crate) fn before_act(host: &impl HostState) -> Result<(), DesktopError> {
    before_call(host)?;
    if host.guard_dead() {
        return Err(DesktopError::UserActiveGuardUnavailable(
            "the input watch stopped, so the person's input can no longer pause the agent".into(),
        ));
    }
    if host.secure_input() {
        return Err(DesktopError::SecureFieldRefused);
    }
    Ok(())
}

/// The processes that are the host app: this process, its bundle id
/// ([`set_app_bundle_id`]), and whatever the host adds (its helper processes).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct OwnApps {
    pids: Vec<u32>,
    bundle_ids: Vec<String>,
}

impl Default for OwnApps {
    fn default() -> Self {
        Self {
            pids: vec![std::process::id()],
            bundle_ids: app_bundle_id().map(str::to_owned).into_iter().collect(),
        }
    }
}

impl OwnApps {
    /// The default set plus `extra_pids` and `extra_bundle_ids`.
    #[must_use]
    pub fn with_extra(extra_pids: &[u32], extra_bundle_ids: &[String]) -> Self {
        let mut own = Self::default();
        for pid in extra_pids {
            if !own.pids.contains(pid) {
                own.pids.push(*pid);
            }
        }
        for bundle in extra_bundle_ids {
            if !own.is_own_bundle(bundle) {
                own.bundle_ids.push(bundle.clone());
            }
        }
        own
    }

    #[must_use]
    pub fn is_own_bundle(&self, bundle_id: &str) -> bool {
        self.bundle_ids
            .iter()
            .any(|b| b.eq_ignore_ascii_case(bundle_id))
    }

    /// Whether the process `pid` (with this bundle id, when it has one) is
    /// Elitea.
    #[must_use]
    pub fn is_own(&self, pid: u32, bundle_id: Option<&str>) -> bool {
        self.pids.contains(&pid) || bundle_id.is_some_and(|b| self.is_own_bundle(b))
    }

    /// `own_window_refused` when the process is Elitea.
    pub(crate) fn refuse(
        &self,
        pid: u32,
        bundle_id: Option<&str>,
        what: &str,
    ) -> Result<(), DesktopError> {
        if self.is_own(pid, bundle_id) {
            Err(DesktopError::OwnWindowRefused(what.to_owned()))
        } else {
            Ok(())
        }
    }
}

/// The frontmost application, as far as the OS tells it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Frontmost<'a> {
    /// No application is frontmost.
    None,
    /// One is, and its bundle id cannot be read.
    Unknown,
    /// The frontmost application's bundle id.
    Bundle(&'a str),
}

/// `not_frontmost` unless the frontmost application is `bundle_id`. Input
/// with no element target lands in the frontmost app, so it goes only into
/// the app the call names; an app whose bundle id cannot be read is not it.
pub(crate) fn require_frontmost(
    frontmost: Frontmost<'_>,
    bundle_id: &str,
) -> Result<(), DesktopError> {
    match frontmost {
        Frontmost::Bundle(actual) if crate::bundle_matches(Some(actual), bundle_id) => Ok(()),
        Frontmost::Bundle(actual) => Err(DesktopError::NotFrontmost(format!(
            "{actual} is frontmost, not {bundle_id}: bring it forward first (computer_open_app)"
        ))),
        Frontmost::Unknown => Err(DesktopError::NotFrontmost(format!(
            "the frontmost application cannot be told: bring {bundle_id} forward first"
        ))),
        Frontmost::None => Err(DesktopError::NotFrontmost(format!(
            "no application is frontmost: bring {bundle_id} forward first"
        ))),
    }
}

/// Whether two rectangles share any area (touching edges do not count).
#[must_use]
pub fn overlaps(a: &Rect, b: &Rect) -> bool {
    a.x < b.x + b.width && b.x < a.x + a.width && a.y < b.y + b.height && b.y < a.y + a.height
}

#[cfg(test)]
mod tests {
    use std::cell::Cell;

    use super::*;

    struct Stub {
        screen: ScreenStatus,
        secure: bool,
        secure_reads: Cell<u32>,
        guard: crate::input::GuardState,
    }

    impl Stub {
        fn new(screen: ScreenStatus, secure: bool) -> Self {
            Self {
                screen,
                secure,
                secure_reads: Cell::new(0),
                guard: crate::input::GuardState::default(),
            }
        }
    }

    impl HostState for Stub {
        fn screen(&self) -> ScreenStatus {
            self.screen
        }
        fn secure_input(&self) -> bool {
            self.secure_reads.set(self.secure_reads.get() + 1);
            self.secure
        }
        fn guard_dead(&self) -> bool {
            self.guard.dead()
        }
    }

    /// A fake watch: the driver's record of it, and the thread guard the
    /// test drops to end it.
    #[test]
    fn a_dead_watch_refuses_every_act_but_not_reads() {
        let host = Stub::new(ScreenStatus::available(), false);
        let alive = host.guard.start();
        let thread = alive.guard();
        assert_eq!(before_act(&host), Ok(()));
        drop(thread);
        assert_eq!(before_call(&host), Ok(()));
        assert_eq!(
            before_act(&host).unwrap_err().code(),
            "user_active_guard_unavailable"
        );
        // A new watch acts again.
        let _next = host.guard.start().guard();
        assert_eq!(before_act(&host), Ok(()));
    }

    #[test]
    fn an_available_screen_passes() {
        let host = Stub::new(ScreenStatus::available(), false);
        assert_eq!(before_call(&host), Ok(()));
        assert_eq!(before_act(&host), Ok(()));
    }

    #[test]
    fn locked_asleep_screensaver_and_off_console_are_screen_unavailable() {
        let cases = [
            ScreenStatus {
                locked: true,
                ..ScreenStatus::available()
            },
            ScreenStatus {
                display_asleep: true,
                ..ScreenStatus::available()
            },
            ScreenStatus {
                screensaver: true,
                ..ScreenStatus::available()
            },
            ScreenStatus {
                on_console: false,
                ..ScreenStatus::available()
            },
            ScreenStatus {
                lock_state_unknown: true,
                ..ScreenStatus::available()
            },
            ScreenStatus::default(),
        ];
        for screen in cases {
            let host = Stub::new(screen, false);
            let err = before_call(&host).unwrap_err();
            assert_eq!(err.code(), "screen_unavailable", "{screen:?}");
            assert_eq!(before_act(&host).unwrap_err().code(), "screen_unavailable");
        }
    }

    #[test]
    fn secure_input_refuses_every_act_but_not_reads() {
        let host = Stub::new(ScreenStatus::available(), true);
        assert_eq!(before_call(&host), Ok(()));
        assert_eq!(before_act(&host), Err(DesktopError::SecureFieldRefused));
    }

    #[test]
    fn the_screen_is_checked_before_secure_input() {
        let host = Stub::new(
            ScreenStatus {
                locked: true,
                ..ScreenStatus::available()
            },
            true,
        );
        assert_eq!(before_act(&host).unwrap_err().code(), "screen_unavailable");
        assert_eq!(host.secure_reads.get(), 0);
    }

    #[test]
    fn the_screensaver_is_recognised() {
        assert!(is_screensaver(Some("com.apple.ScreenSaver.Engine"), "x"));
        assert!(is_screensaver(None, "ScreenSaverEngine"));
        assert!(!is_screensaver(Some("com.apple.TextEdit"), "TextEdit"));
    }

    #[test]
    fn the_app_itself_and_the_extras_are_own() {
        set_app_bundle_id("com.example.host.desktop");
        let own = OwnApps::with_extra(&[4242], &["com.example.helper".to_owned()]);
        assert!(own.is_own(std::process::id(), None));
        assert!(own.is_own(1, Some("com.example.host.desktop")));
        assert!(own.is_own(1, Some("COM.Example.Host.Desktop")));
        assert!(own.is_own(4242, Some("com.apple.TextEdit")));
        assert!(own.is_own(1, Some("com.example.helper")));
        assert!(!own.is_own(1, Some("com.apple.TextEdit")));
        assert!(!own.is_own(1, None));
        assert_eq!(
            own.refuse(1, Some("com.example.host.desktop"), "snapshot")
                .unwrap_err()
                .code(),
            "own_window_refused"
        );
        assert_eq!(
            own.refuse(1, Some("com.apple.TextEdit"), "snapshot"),
            Ok(())
        );
    }

    #[test]
    fn untargeted_input_goes_only_into_the_named_frontmost_app() {
        let app = "com.apple.TextEdit";
        assert_eq!(
            require_frontmost(Frontmost::Bundle("com.apple.textedit"), app),
            Ok(())
        );
        for frontmost in [
            Frontmost::Bundle("com.apple.Notes"),
            Frontmost::Unknown,
            Frontmost::None,
        ] {
            assert_eq!(
                require_frontmost(frontmost, app).unwrap_err().code(),
                "not_frontmost",
                "{frontmost:?}"
            );
        }
    }
}
