//! The Linux driver (ADR-0047 decision 10): AT-SPI through the vendored
//! agent-desktop `linux` adapter (X11 input through XTEST, Wayland through
//! the portals), behind the shared engine (`engine.rs`).
//!
//! - **Application ids** are desktop entry ids (`org.gnome.TextEditor`),
//!   or the application's name when it has none
//!   ([`crate::platform::linux_app_id`]). `open_app` takes either; the
//!   vendor never launches a path outside the XDG data directories.
//! - **The screen** ([`screen`]): no display, a logind session that is not
//!   active (another user switched in), a locked session (`LockedHint`), or
//!   an active screensaver (`org.freedesktop.ScreenSaver`, and GNOME's own)
//!   is `screen_unavailable`. A desktop without logind or a screensaver
//!   service reports those reads as clear.
//! - **The user-active guard** runs on X11 only (`linux_input.rs`). Wayland
//!   gives no application a global view of the keyboard and pointer, so on
//!   a Wayland session the watch fails closed and every act is refused
//!   (`user_active_guard_unavailable`); reads still work.

use std::time::Duration;

use agent_desktop_core as ad;
use zbus::blocking::Connection;
use zbus::zvariant::{OwnedObjectPath, OwnedValue};

use crate::engine::{NativeDriver, OsHooks};
use crate::platform::{self, Os};
use crate::{ScreenStatus, SyntheticMarker};

/// The marker the Linux driver reports. XTEST cannot carry it: the watch
/// tells the driver's own events by their XTEST source device instead.
const LINUX_MARKER: i64 = 0x454C_4954_4541_4C01;

/// The longest one screen read waits on the bus.
const BUS_TIMEOUT: Duration = Duration::from_millis(500);

/// The Linux [`crate::DesktopDriver`].
pub type LinuxDriver = NativeDriver;

impl NativeDriver {
    /// The Linux driver.
    #[must_use]
    pub fn new() -> Self {
        Self::with_hooks(
            Box::new(agent_desktop_linux::LinuxAdapter::new()),
            OsHooks {
                os: Os::Linux,
                app_id,
                screen,
                marker: SyntheticMarker::new(LINUX_MARKER).unwrap_or_else(|| unreachable!()),
                watch: crate::linux_input::watch,
            },
        )
    }
}

impl Default for NativeDriver {
    fn default() -> Self {
        Self::new()
    }
}

fn app_id(app: &ad::AppInfo) -> Option<String> {
    platform::linux_app_id(app.bundle_id.as_deref(), &app.name)
}

/// Whether this process runs in a Wayland session (the guard's fail-closed
/// case).
pub(crate) fn is_wayland() -> bool {
    std::env::var_os("WAYLAND_DISPLAY").is_some_and(|v| !v.is_empty())
        || std::env::var("XDG_SESSION_TYPE").is_ok_and(|t| t.eq_ignore_ascii_case("wayland"))
}

fn bus(system: bool) -> Option<Connection> {
    let builder = if system {
        zbus::blocking::connection::Builder::system()
    } else {
        zbus::blocking::connection::Builder::session()
    };
    builder.ok()?.method_timeout(BUS_TIMEOUT).build().ok()
}

fn property(bus: &Connection, dest: &str, path: &str, iface: &str, name: &str) -> Option<bool> {
    let reply = bus
        .call_method(
            Some(dest),
            path,
            Some("org.freedesktop.DBus.Properties"),
            "Get",
            &(iface, name),
        )
        .ok()?;
    let value: OwnedValue = reply.body().deserialize().ok()?;
    bool::try_from(value).ok()
}

/// What logind says about this process's session: (active, locked), or
/// `None` without logind or a session.
fn logind() -> Option<(bool, bool)> {
    let bus = bus(true)?;
    let reply = bus
        .call_method(
            Some("org.freedesktop.login1"),
            "/org/freedesktop/login1",
            Some("org.freedesktop.login1.Manager"),
            "GetSessionByPID",
            &(std::process::id(),),
        )
        .ok()?;
    let path: OwnedObjectPath = reply.body().deserialize().ok()?;
    let session = "org.freedesktop.login1.Session";
    let active = property(
        &bus,
        "org.freedesktop.login1",
        path.as_str(),
        session,
        "Active",
    )?;
    let locked = property(
        &bus,
        "org.freedesktop.login1",
        path.as_str(),
        session,
        "LockedHint",
    )
    .unwrap_or(false);
    Some((active, locked))
}

/// Whether a screensaver service says the screensaver is up.
fn screensaver_active() -> bool {
    let Some(bus) = bus(false) else {
        return false;
    };
    [
        (
            "org.freedesktop.ScreenSaver",
            "/org/freedesktop/ScreenSaver",
        ),
        ("org.gnome.ScreenSaver", "/org/gnome/ScreenSaver"),
    ]
    .iter()
    .any(|(name, path)| {
        bus.call_method(Some(*name), *path, Some(*name), "GetActive", &())
            .ok()
            .and_then(|reply| reply.body().deserialize::<bool>().ok())
            .unwrap_or(false)
    })
}

fn screen() -> ScreenStatus {
    let display = std::env::var_os("DISPLAY").is_some_and(|v| !v.is_empty())
        || std::env::var_os("WAYLAND_DISPLAY").is_some_and(|v| !v.is_empty());
    let (active, locked) = logind().unwrap_or((true, false));
    let screensaver = screensaver_active();
    ScreenStatus {
        session_present: display,
        on_console: active,
        // Most lockers set the screensaver active while locked; logind's
        // hint covers the rest.
        locked,
        screensaver,
        display_asleep: false,
    }
}
