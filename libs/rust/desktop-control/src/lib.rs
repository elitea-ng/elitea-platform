//! Desktop computer use (ADR-0047 decision 1).
//!
//! [`DesktopDriver`] is the one boundary between the computer toolset and
//! the OS: list applications and windows, snapshot a window into a budgeted
//! accessibility tree with snapshot-scoped refs (`@<snapshot>:eN`), act on a
//! ref, type, send a key chord, capture a window or region, and wait for a
//! condition.
//!
//! - [`MacDriver`] (macOS only) is built on the vendored agent-desktop
//!   `core` and `macos` crates (`libs/rust/vendor/agent-desktop`). No vendor
//!   type crosses the trait.
//! - [`Unsupported`] is the driver of every other OS; every call fails with
//!   [`DesktopError::PlatformNotSupported`].
//! - [`default_driver`] returns the one this OS has.
//!
//! The driver itself enforces what only it can see: a secure text field is
//! reported as `secure` with no value and is never typed into, denied key
//! chords are refused, and a ref from a dropped or changed snapshot fails as
//! [`DesktopError::StaleRef`]. Every input event it posts carries the
//! [`SyntheticMarker`], so the user-active guard can tell the person's input
//! from the driver's ([`DesktopDriver::is_synthetic_event`]).
//!
//! Nothing calls this crate yet; #1583 wires the toolset.

mod driver;
mod error;
mod model;
mod policy;
pub mod snapshot;

#[cfg(target_os = "macos")]
mod macos;

pub use driver::{DesktopDriver, Unsupported};
pub use error::DesktopError;
#[cfg(target_os = "macos")]
pub use macos::MacDriver;
pub use model::{
    Action, ActionOutcome, AppInfo, Capture, CaptureTarget, Delivery, ElementRef, Grant, KeyChord,
    Modifier, Node, Permissions, Point, Rect, ScrollDirection, Snapshot, SnapshotBudget,
    SnapshotId, WaitCondition, WindowInfo, WindowTarget,
};
pub use policy::{InputOrigin, SyntheticMarker, is_denied_chord};

/// The driver of this OS: [`MacDriver`] on macOS, [`Unsupported`] elsewhere.
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
