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
//!
//! The OS reads sit behind [`HostState`], so the gate order is unit-tested
//! with a stub.

// The act-only gates have their one caller in the macOS driver.
#![cfg_attr(not(target_os = "macos"), allow(dead_code))]

use crate::{DesktopError, Rect};

/// The bundle id of the Elitea desktop app (`apps/elitea-desktop`).
pub const ELITEA_BUNDLE_ID: &str = "ai.elitea.desktop";

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
}

/// The gate of every call except `permissions`: `screen_unavailable`.
pub(crate) fn before_call(host: &impl HostState) -> Result<(), DesktopError> {
    match host.screen().unavailable_reason() {
        Some(reason) => Err(DesktopError::ScreenUnavailable(reason.to_owned())),
        None => Ok(()),
    }
}

/// The gate of every act (`act` of any kind, `type_text`, `key`): the
/// screen, then secure input. It runs before any ref is resolved, so no act
/// kind gets past secure input.
pub(crate) fn before_act(host: &impl HostState) -> Result<(), DesktopError> {
    before_call(host)?;
    if host.secure_input() {
        return Err(DesktopError::SecureFieldRefused);
    }
    Ok(())
}

/// The processes that are Elitea: this process, the `ai.elitea.desktop`
/// bundle, and whatever the host adds (its helper processes).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct OwnApps {
    pids: Vec<u32>,
    bundle_ids: Vec<String>,
}

impl Default for OwnApps {
    fn default() -> Self {
        Self {
            pids: vec![std::process::id()],
            bundle_ids: vec![ELITEA_BUNDLE_ID.to_owned()],
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

/// Whether two rectangles share any area (touching edges do not count).
#[must_use]
pub fn overlaps(a: &Rect, b: &Rect) -> bool {
    a.x < b.x + b.width && b.x < a.x + a.width && a.y < b.y + b.height && b.y < a.y + a.height
}

/// `own_window_refused` when `region` overlaps any of Elitea's window
/// frames. A region capture is refused rather than masked: the image would
/// show Elitea's own asks.
pub(crate) fn refuse_region(region: &Rect, own_frames: &[Rect]) -> Result<(), DesktopError> {
    if own_frames.iter().any(|f| overlaps(region, f)) {
        Err(DesktopError::OwnWindowRefused(
            "the region overlaps an Elitea window".into(),
        ))
    } else {
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use std::cell::Cell;

    use super::*;

    struct Stub {
        screen: ScreenStatus,
        secure: bool,
        secure_reads: Cell<u32>,
    }

    impl Stub {
        fn new(screen: ScreenStatus, secure: bool) -> Self {
            Self {
                screen,
                secure,
                secure_reads: Cell::new(0),
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
    fn elitea_itself_and_the_extras_are_own() {
        let own = OwnApps::with_extra(&[4242], &["ai.elitea.helper".to_owned()]);
        assert!(own.is_own(std::process::id(), None));
        assert!(own.is_own(1, Some("ai.elitea.desktop")));
        assert!(own.is_own(1, Some("AI.Elitea.Desktop")));
        assert!(own.is_own(4242, Some("com.apple.TextEdit")));
        assert!(own.is_own(1, Some("ai.elitea.helper")));
        assert!(!own.is_own(1, Some("com.apple.TextEdit")));
        assert!(!own.is_own(1, None));
        assert_eq!(
            own.refuse(1, Some("ai.elitea.desktop"), "snapshot")
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
    fn a_region_over_an_elitea_window_is_refused() {
        let r = |x, y, width, height| Rect {
            x,
            y,
            width,
            height,
        };
        let own = [r(100.0, 100.0, 200.0, 200.0)];
        assert_eq!(
            refuse_region(&r(250.0, 250.0, 100.0, 100.0), &own)
                .unwrap_err()
                .code(),
            "own_window_refused"
        );
        // Touching an edge is not an overlap.
        assert_eq!(refuse_region(&r(300.0, 100.0, 50.0, 50.0), &own), Ok(()));
        assert_eq!(refuse_region(&r(0.0, 0.0, 50.0, 50.0), &own), Ok(()));
        assert_eq!(refuse_region(&r(0.0, 0.0, 50.0, 50.0), &[]), Ok(()));
    }
}
