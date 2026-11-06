//! The [`DesktopDriver`] trait and the [`Unsupported`] driver.

use std::time::Duration;

use crate::{
    Action, ActionOutcome, AppInfo, Capture, CaptureTarget, ClickAt, DesktopError, ElementRef,
    InputOrigin, KeyChord, OpenedApp, Permissions, ScreenStatus, Snapshot, SnapshotBudget,
    SyntheticMarker, WaitCondition, WindowInfo, WindowTarget,
};

/// Control of the person's other applications (ADR-0047 decision 1).
///
/// One driver per desktop host, used from a dedicated thread: the calls
/// block, some for as long as their timeout. The driver applies the hard
/// limits of ADR-0047 decision 5 that it alone can see, in bypass mode too:
/// secure fields and macOS secure input, denied chords, Elitea's own windows
/// (`own_window_refused`), the locked or sleeping screen
/// (`screen_unavailable`), and stale refs. The policy ceiling, per-app
/// approvals, rate limits and the user-active guard belong to the caller
/// (#1583, #1584).
///
/// Every call except [`DesktopDriver::permissions`] fails with
/// [`DesktopError::ScreenUnavailable`] while the screen is locked, the
/// screensaver is up, the display sleeps or the session is not on the
/// console. Elitea's own apps and windows are left out of the lists, and a
/// target that is one fails with [`DesktopError::OwnWindowRefused`].
pub trait DesktopDriver: Send + Sync {
    /// The OS privacy grants, read live.
    fn permissions(&self) -> Permissions;

    /// The screen as the OS reports it now: locked, screensaver, display
    /// asleep, session off the console. Every other call checks it itself;
    /// a caller reads it to refuse a call before it does anything else.
    /// The Unsupported driver reports no session.
    fn screen(&self) -> ScreenStatus;

    /// Whether macOS secure input is on now (a password prompt anywhere).
    /// Every act checks it itself; a caller reads it to refuse an act
    /// before it asks the person about it.
    fn secure_input(&self) -> bool;

    /// Running applications with a user interface.
    ///
    /// # Errors
    /// [`DesktopError::PermissionDenied`] without the Accessibility grant;
    /// [`DesktopError::PlatformNotSupported`] on the Unsupported driver.
    fn list_apps(&self) -> Result<Vec<AppInfo>, DesktopError>;

    /// Top-level windows, of every app or of the app with this bundle id.
    ///
    /// # Errors
    /// As [`DesktopDriver::list_apps`], and [`DesktopError::AppNotFound`].
    fn list_windows(&self, bundle_id: Option<&str>) -> Result<Vec<WindowInfo>, DesktopError>;

    /// One window's filtered accessibility tree under `budget`, with refs
    /// `@<snapshot>:eN` that only this snapshot answers for. A secure text
    /// field is a node with `secure: true` and no value.
    ///
    /// # Errors
    /// [`DesktopError::WindowNotFound`], [`DesktopError::AppNotFound`],
    /// [`DesktopError::PermissionDenied`], [`DesktopError::Timeout`].
    fn snapshot(
        &self,
        target: &WindowTarget,
        budget: &SnapshotBudget,
    ) -> Result<Snapshot, DesktopError>;

    /// One action on the element `element_ref` names. Semantic first (no
    /// pointer move, no focus taken); the outcome says how it was delivered.
    ///
    /// # Errors
    /// [`DesktopError::StaleRef`] when the snapshot is gone or the element
    /// changed; [`DesktopError::SecureFieldRefused`] for any action while
    /// macOS secure input is on, and for a text write into a secure field;
    /// [`DesktopError::InvalidArgs`] for a `SetValue` longer than
    /// [`MAX_TYPED_CHARS`](crate::MAX_TYPED_CHARS); [`DesktopError::ActionNotSupported`] /
    /// [`DesktopError::ActionFailed`].
    fn act(&self, element_ref: &ElementRef, action: &Action)
    -> Result<ActionOutcome, DesktopError>;

    /// Types `text` into the element `element_ref` names, or, with `None`,
    /// into the focused element of the frontmost app.
    ///
    /// # Errors
    /// [`DesktopError::SecureFieldRefused`] when macOS secure input is on, or
    /// the target is a secure field (with `None`, the focused element);
    /// [`DesktopError::OwnWindowRefused`] when, with `None`, Elitea is the
    /// frontmost app; [`DesktopError::InvalidArgs`] past
    /// [`MAX_TYPED_CHARS`](crate::MAX_TYPED_CHARS); otherwise as
    /// [`DesktopDriver::act`].
    fn type_text(
        &self,
        element_ref: Option<&ElementRef>,
        text: &str,
    ) -> Result<ActionOutcome, DesktopError>;

    /// One key chord into a window.
    ///
    /// # Errors
    /// [`DesktopError::ChordRefused`] for a denied chord (quit, log out,
    /// lock, Spotlight, ...); [`DesktopError::SecureFieldRefused`] when macOS
    /// secure input is on or the window's focused element is secure;
    /// [`DesktopError::OwnWindowRefused`] when the window, or the frontmost
    /// app, is Elitea; [`DesktopError::WindowNotFound`].
    fn key(&self, target: &WindowTarget, chord: &KeyChord) -> Result<ActionOutcome, DesktopError>;

    /// A coordinate click inside one window (the fallback for apps with no
    /// accessible tree). Physical input, tagged with the marker.
    ///
    /// # Errors
    /// [`DesktopError::InvalidArgs`] for a bad point or count, or a point
    /// outside the window; [`DesktopError::WindowNotFound`];
    /// [`DesktopError::OwnWindowRefused`] for an Elitea window, or when the
    /// window topmost at the point is not this one (another window,
    /// Elitea's included, covers it); [`DesktopError::SecureFieldRefused`]
    /// while macOS secure input is on.
    fn click_at(&self, click: &ClickAt) -> Result<ActionOutcome, DesktopError>;

    /// Launches the application with this bundle id, or brings it forward,
    /// and waits a short time for a window.
    ///
    /// # Errors
    /// [`DesktopError::OwnWindowRefused`] for Elitea;
    /// [`DesktopError::InvalidArgs`] for a value that is not a bundle id;
    /// [`DesktopError::AppNotFound`]; [`DesktopError::SecureFieldRefused`]
    /// while macOS secure input is on.
    fn open_app(&self, bundle_id: &str) -> Result<OpenedApp, DesktopError>;

    /// A window or a screen region as PNG.
    ///
    /// # Errors
    /// [`DesktopError::PermissionDenied`] without the Screen Recording
    /// grant; [`DesktopError::WindowNotFound`]; [`DesktopError::InvalidArgs`]
    /// for a region outside every display; [`DesktopError::OwnWindowRefused`]
    /// for an Elitea window, or a region that overlaps one (refused, not
    /// masked).
    fn capture(&self, target: &CaptureTarget) -> Result<Capture, DesktopError>;

    /// Polls until `condition` holds or `timeout` passes.
    ///
    /// # Errors
    /// [`DesktopError::Timeout`] when it does not hold in time; a ref
    /// condition fails as [`DesktopDriver::act`] would.
    fn wait(&self, condition: &WaitCondition, timeout: Duration) -> Result<(), DesktopError>;

    /// The value the driver stamps into every input event it posts.
    fn synthetic_marker(&self) -> SyntheticMarker;

    /// Classifies an input event by its source user-data field (on macOS
    /// `CGEventGetIntegerValueField(event, kCGEventSourceUserData)`): the
    /// driver's own, or the person's.
    fn is_synthetic_event(&self, event_source_user_data: i64) -> InputOrigin {
        self.synthetic_marker().classify(event_source_user_data)
    }

    /// Drops every held snapshot: every ref handed out so far is stale.
    fn forget_snapshots(&self);
}

/// The driver of every OS without an implementation. It compiles
/// everywhere, reports no grants and fails every call with
/// [`DesktopError::PlatformNotSupported`].
#[derive(Debug, Clone, Copy, Default)]
pub struct Unsupported;

/// The marker the Unsupported driver reports. It posts no events.
const UNSUPPORTED_MARKER: i64 = 0x454C_4954_4541_4443;

impl DesktopDriver for Unsupported {
    fn permissions(&self) -> Permissions {
        Permissions {
            accessibility: crate::Grant::Unknown,
            screen_recording: crate::Grant::Unknown,
        }
    }

    fn screen(&self) -> ScreenStatus {
        ScreenStatus::default()
    }

    fn secure_input(&self) -> bool {
        false
    }

    fn list_apps(&self) -> Result<Vec<AppInfo>, DesktopError> {
        Err(DesktopError::PlatformNotSupported)
    }

    fn click_at(&self, _click: &ClickAt) -> Result<ActionOutcome, DesktopError> {
        Err(DesktopError::PlatformNotSupported)
    }

    fn open_app(&self, _bundle_id: &str) -> Result<OpenedApp, DesktopError> {
        Err(DesktopError::PlatformNotSupported)
    }

    fn list_windows(&self, _bundle_id: Option<&str>) -> Result<Vec<WindowInfo>, DesktopError> {
        Err(DesktopError::PlatformNotSupported)
    }

    fn snapshot(
        &self,
        _target: &WindowTarget,
        _budget: &SnapshotBudget,
    ) -> Result<Snapshot, DesktopError> {
        Err(DesktopError::PlatformNotSupported)
    }

    fn act(
        &self,
        _element_ref: &ElementRef,
        _action: &Action,
    ) -> Result<ActionOutcome, DesktopError> {
        Err(DesktopError::PlatformNotSupported)
    }

    fn type_text(
        &self,
        _element_ref: Option<&ElementRef>,
        _text: &str,
    ) -> Result<ActionOutcome, DesktopError> {
        Err(DesktopError::PlatformNotSupported)
    }

    fn key(
        &self,
        _target: &WindowTarget,
        _chord: &KeyChord,
    ) -> Result<ActionOutcome, DesktopError> {
        Err(DesktopError::PlatformNotSupported)
    }

    fn capture(&self, _target: &CaptureTarget) -> Result<Capture, DesktopError> {
        Err(DesktopError::PlatformNotSupported)
    }

    fn wait(&self, _condition: &WaitCondition, _timeout: Duration) -> Result<(), DesktopError> {
        Err(DesktopError::PlatformNotSupported)
    }

    fn synthetic_marker(&self) -> SyntheticMarker {
        SyntheticMarker::new(UNSUPPORTED_MARKER).unwrap_or_else(|| unreachable!())
    }

    fn forget_snapshots(&self) {}
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_unsupported_driver_fails_every_call() {
        let d = Unsupported;
        assert_eq!(d.list_apps(), Err(DesktopError::PlatformNotSupported));
        assert_eq!(
            d.snapshot(&WindowTarget::Focused, &SnapshotBudget::default())
                .unwrap_err()
                .code(),
            "platform_not_supported"
        );
        assert_eq!(d.is_synthetic_event(0), InputOrigin::Physical);
    }
}
