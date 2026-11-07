//! The [`DesktopDriver`] trait and the [`Unsupported`] driver.

use std::time::Duration;

use crate::{
    Action, ActionOutcome, AppInfo, Capture, CaptureTarget, ClickAt, DesktopError, ElementRef,
    InputFilter, InputOrigin, InputWatch, KeyChord, OpenedApp, Permissions, ScreenStatus,
    SignalSink, Snapshot, SnapshotBudget, SyntheticMarker, WaitCondition, WindowInfo, WindowTarget,
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

    /// One action on the element `element_ref` names, in the application
    /// with bundle id `bundle_id`. Semantic first (no pointer move, no focus
    /// taken); the outcome says how it was delivered.
    ///
    /// # Errors
    /// [`DesktopError::StaleRef`] when the snapshot is gone, the element
    /// changed, or the snapshot was not taken of `bundle_id` (an unknown
    /// bundle never matches); [`DesktopError::MenuItemRefused`] for a menu
    /// item that quits, logs out, shuts down, restarts, sleeps, locks,
    /// force quits or switches user
    /// ([`is_denied_menu_item`](crate::is_denied_menu_item));
    /// [`DesktopError::SecureFieldRefused`] for any action while
    /// macOS secure input is on, and for a text write into a secure field;
    /// [`DesktopError::InvalidArgs`] for a `SetValue` longer than
    /// [`MAX_TYPED_CHARS`](crate::MAX_TYPED_CHARS); [`DesktopError::ActionNotSupported`] /
    /// [`DesktopError::ActionFailed`].
    fn act(
        &self,
        bundle_id: &str,
        element_ref: &ElementRef,
        action: &Action,
    ) -> Result<ActionOutcome, DesktopError>;

    /// Types `text` into the element `element_ref` names, or, with `None`,
    /// into the focused element of the frontmost app, which must be the
    /// application `bundle_id`.
    ///
    /// # Errors
    /// [`DesktopError::SecureFieldRefused`] when macOS secure input is on, or
    /// the target is a secure field (with `None`, the focused element);
    /// [`DesktopError::OwnWindowRefused`] when, with `None`, Elitea is the
    /// frontmost app; [`DesktopError::NotFrontmost`] when, with `None`, the
    /// frontmost app is not `bundle_id` (or cannot be told);
    /// [`DesktopError::InvalidArgs`] past
    /// [`MAX_TYPED_CHARS`](crate::MAX_TYPED_CHARS); otherwise as
    /// [`DesktopDriver::act`].
    fn type_text(
        &self,
        bundle_id: &str,
        element_ref: Option<&ElementRef>,
        text: &str,
    ) -> Result<ActionOutcome, DesktopError>;

    /// One key chord into a window of the application `target` names. The
    /// chord lands in the frontmost app, so that app must be the named one.
    ///
    /// # Errors
    /// [`DesktopError::InvalidArgs`] for a target that names no application
    /// ([`WindowTarget::Focused`]); [`DesktopError::NotFrontmost`] when the
    /// frontmost app is not the named one;
    /// [`DesktopError::ChordRefused`] for a denied chord (quit, log out,
    /// lock, Spotlight, ...); [`DesktopError::SecureFieldRefused`] when macOS
    /// secure input is on or the window's focused element is secure;
    /// [`DesktopError::OwnWindowRefused`] when the window, or the frontmost
    /// app, is Elitea; [`DesktopError::WindowNotFound`].
    fn key(&self, target: &WindowTarget, chord: &KeyChord) -> Result<ActionOutcome, DesktopError>;

    /// A coordinate click inside one window of the application
    /// [`ClickAt::bundle_id`] names, which must be frontmost (the fallback for
    /// apps with no accessible tree). Physical input, tagged with the marker.
    ///
    /// # Errors
    /// [`DesktopError::InvalidArgs`] for a bad point or count, or a point
    /// outside the window; [`DesktopError::WindowNotFound`] (a window of
    /// another app included); [`DesktopError::NotFrontmost`];
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

    /// A window, or a region of one window, as PNG. The window is captured
    /// alone and without its shadow, so no other window shows in the image
    /// and its pixels map 1:1 onto the window's frame times the scale.
    ///
    /// # Errors
    /// [`DesktopError::PermissionDenied`] without the Screen Recording
    /// grant; [`DesktopError::WindowNotFound`] (a window of another app
    /// included); [`DesktopError::InvalidArgs`] for a region outside the
    /// window; [`DesktopError::OwnWindowRefused`] for an Elitea window.
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

    /// Watch the person's keyboard and pointer (ADR-0047 decision 6, the
    /// user-active guard and the stop shortcut): each input that is not the
    /// driver's own, nor aimed at a process in `filter`, reaches `sink` as a
    /// [`Signal`](crate::Signal), on the watch's own thread. Dropping the
    /// handle stops the watch.
    ///
    /// # Errors
    /// [`DesktopError::PermissionDenied`] when the OS refuses the watch;
    /// [`DesktopError::PlatformNotSupported`] where there is none (the
    /// default).
    fn watch_input(
        &self,
        filter: InputFilter,
        sink: SignalSink,
    ) -> Result<Box<dyn InputWatch>, DesktopError> {
        let _ = (filter, sink);
        Err(DesktopError::PlatformNotSupported)
    }

    /// The halt of the calls from now on (`None`: none), set by the caller
    /// before each call (calls run one at a time). Once it is raised, the
    /// call in flight fails with [`DesktopError::Interrupted`] at its next
    /// step, before it posts any more input; a wait stops polling. The
    /// default ignores it (a driver that posts nothing).
    fn set_halt(&self, halt: Option<crate::Halt>) {
        let _ = halt;
    }
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
        _bundle_id: &str,
        _element_ref: &ElementRef,
        _action: &Action,
    ) -> Result<ActionOutcome, DesktopError> {
        Err(DesktopError::PlatformNotSupported)
    }

    fn type_text(
        &self,
        _bundle_id: &str,
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
        assert_eq!(
            d.watch_input(InputFilter::default(), Box::new(|_| {}))
                .err()
                .map(|e| e.code()),
            Some("platform_not_supported")
        );
    }
}
