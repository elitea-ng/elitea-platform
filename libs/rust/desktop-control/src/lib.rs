//! Desktop computer use (ADR-0047 decision 1).
//!
//! [`DesktopDriver`] is the one boundary between the computer toolset and
//! the OS: list applications and windows, snapshot a window into a budgeted
//! accessibility tree with snapshot-scoped refs (`@<snapshot>:eN`), act on a
//! ref, type, send a key chord, capture a window or region, and wait for a
//! condition.
//!
//! - `MacDriver` (macOS only) is built on the vendored agent-desktop
//!   `core` and `macos` crates (`libs/rust/vendor/agent-desktop`). No vendor
//!   type crosses the trait.
//! - [`Unsupported`] is the driver of every other OS; every call fails with
//!   [`DesktopError::PlatformNotSupported`].
//! - [`default_driver`] returns the one this OS has.
//!
//! The driver itself enforces the hard limits of ADR-0047 decision 5, in
//! bypass mode too: a secure text field is reported as `secure` with no
//! value and is never typed into; while macOS secure input is on every act
//! is refused; denied key chords are refused; Elitea's own windows are never
//! read, acted on or captured ([`DesktopError::OwnWindowRefused`]); and no
//! call but `permissions` runs while the screen is locked, the screensaver
//! is up or the display sleeps ([`DesktopError::ScreenUnavailable`]). A ref
//! from a dropped or changed snapshot fails as [`DesktopError::StaleRef`]. Every input event it posts carries the
//! [`SyntheticMarker`], so the user-active guard can tell the person's input
//! from the driver's ([`DesktopDriver::is_synthetic_event`]).
//!
//! The desktop host's `computer` toolset (#1583) is its caller. It also
//! calls [`DesktopDriver::screen`] before every tool call, so a locked
//! screen refuses the whole call.

mod driver;
mod error;
mod guard;
mod model;
mod policy;
pub mod snapshot;

#[cfg(target_os = "macos")]
mod macos;

pub use driver::{DesktopDriver, Unsupported};
pub use error::DesktopError;
pub use guard::{ELITEA_BUNDLE_ID, OwnApps, ScreenStatus, is_screensaver, overlaps};
#[cfg(target_os = "macos")]
pub use macos::MacDriver;
pub use model::{
    Action, ActionOutcome, AppInfo, Capture, CaptureTarget, ClickAt, Delivery, ElementRef, Grant,
    KeyChord, MAX_CLICK_COUNT, MAX_TYPED_CHARS, Modifier, Node, OpenedApp, Permissions, Point,
    PointerButton, Rect, ScrollDirection, Snapshot, SnapshotBudget, SnapshotId, WaitCondition,
    WindowInfo, WindowTarget, rect_contains, topmost_at, validate_text_write,
};
pub use policy::{InputOrigin, SyntheticMarker, is_denied_chord};

/// The driver of this OS: `MacDriver` on macOS, [`Unsupported`] elsewhere.
#[must_use]
pub fn default_driver() -> Box<dyn DesktopDriver> {
    #[cfg(target_os = "macos")]
    {
        Box::new(MacDriver::new())
    }
    #[cfg(not(target_os = "macos"))]
    {
        Box::new(Unsupported)
    }
}
