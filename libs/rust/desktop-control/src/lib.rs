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
//! - `WindowsDriver` (Windows only, UI Automation) and `LinuxDriver`
//!   (Linux only, AT-SPI with X11 input) are built on the vendored
//!   agent-desktop `windows` and `linux` crates through one shared engine
//!   (`engine.rs`), with each OS's deny list and secure-field rule in
//!   [`platform`] (ADR-0047 decision 10).
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

#[cfg(any(target_os = "macos", target_os = "windows", target_os = "linux"))]
mod crop;
mod driver;
mod error;
mod guard;
pub mod input;
mod model;
pub mod platform;
mod policy;
pub mod snapshot;

#[cfg(target_os = "macos")]
mod macos;
#[cfg(target_os = "macos")]
mod macos_input;

#[cfg(any(target_os = "windows", target_os = "linux"))]
mod engine;
#[cfg(target_os = "linux")]
mod linux;
#[cfg(target_os = "linux")]
mod linux_input;
#[cfg(target_os = "windows")]
mod windows;
#[cfg(target_os = "windows")]
mod windows_input;

pub use driver::{DesktopDriver, Unsupported};
pub use error::DesktopError;
pub use guard::{ELITEA_BUNDLE_ID, OwnApps, ScreenStatus, is_screensaver, overlaps};
pub use input::{Halt, InputFilter, InputWatch, STOP_SHORTCUT, Signal, SignalSink};
#[cfg(target_os = "linux")]
pub use linux::LinuxDriver;
#[cfg(target_os = "macos")]
pub use macos::MacDriver;
pub use model::{
    Action, ActionOutcome, AppInfo, Capture, CaptureTarget, ClickAt, Delivery, ElementRef, Grant,
    KeyChord, MAX_CLICK_COUNT, MAX_TYPED_CHARS, Modifier, Node, OpenedApp, Permissions, Point,
    PointerButton, Rect, ScrollDirection, Snapshot, SnapshotBudget, SnapshotId, WaitCondition,
    WindowInfo, WindowTarget, bundle_matches, rect_contains, region_pixels, topmost_at,
    validate_text_write,
};
pub use policy::{InputOrigin, SyntheticMarker, is_denied_chord, is_denied_menu_item};
#[cfg(target_os = "windows")]
pub use windows::WindowsDriver;

/// Why computer use can read but not act on this computer, or `None` when
/// it can act (as far as the session kind tells: grants are separate). On a
/// Linux Wayland or `XWayland` session the user-active guard cannot run, so
/// every act is refused (`user_active_guard_unavailable`); the host's
/// settings show computer use as read-only with this reason.
#[must_use]
pub fn read_only_reason() -> Option<&'static str> {
    #[cfg(target_os = "linux")]
    {
        linux::guard_unavailable()
    }
    #[cfg(not(target_os = "linux"))]
    {
        None
    }
}

/// The driver of this OS: `MacDriver` on macOS, `WindowsDriver` on
/// Windows, `LinuxDriver` on Linux, [`Unsupported`] elsewhere.
#[must_use]
pub fn default_driver() -> Box<dyn DesktopDriver> {
    #[cfg(target_os = "macos")]
    {
        Box::new(MacDriver::new())
    }
    #[cfg(target_os = "windows")]
    {
        Box::new(WindowsDriver::new())
    }
    #[cfg(target_os = "linux")]
    {
        Box::new(LinuxDriver::new())
    }
    #[cfg(not(any(target_os = "macos", target_os = "windows", target_os = "linux")))]
    {
        Box::new(Unsupported)
    }
}
