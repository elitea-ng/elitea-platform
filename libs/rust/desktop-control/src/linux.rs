//! The Linux driver (ADR-0047 decision 10): AT-SPI through the vendored
//! agent-desktop `linux` adapter (X11 input through XTEST, Wayland through
//! the portals), behind the shared engine (`engine.rs`).
//!
//! - **Application ids** are desktop entry ids (`org.gnome.TextEditor`),
//!   or the application's name when it has none
//!   ([`crate::platform::linux_app_id`]). `open_app` takes either; the
//!   vendor never launches a path outside the XDG data directories.
//! - **The screen** ([`screen`], decided by
//!   [`crate::platform::linux_screen_status`]): no display, a logind session
//!   that is not active (another user switched in), a locked session
//!   (`LockedHint`), an active screensaver (the desktop's screensaver
//!   service, or the X server's own MIT-SCREEN-SAVER), or a lock state that
//!   cannot be read is `screen_unavailable`. It fails closed: a desktop
//!   whose locker runs no screensaver service (i3lock, slock, xsecurelock)
//!   cannot show the screen is unlocked, so it counts as locked.
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
                check_window: |_| Ok(()),
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

/// What logind says about this process's session: (`Active`,
/// `LockedHint`), or `None` without logind or a session.
fn logind() -> Option<(bool, Option<bool>)> {
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
    );
    Some((active, locked))
}

/// The screensaver services of the desktops (freedesktop, which KDE and
/// xfce4-screensaver serve; GNOME's, Cinnamon's and MATE's own).
const SCREENSAVER_SERVICES: &[(&str, &str)] = &[
    (
        "org.freedesktop.ScreenSaver",
        "/org/freedesktop/ScreenSaver",
    ),
    ("org.gnome.ScreenSaver", "/org/gnome/ScreenSaver"),
    ("org.cinnamon.ScreenSaver", "/org/cinnamon/ScreenSaver"),
    ("org.mate.ScreenSaver", "/org/mate/ScreenSaver"),
];

/// What the screensaver services say: `Some(true)` when one is active,
/// `Some(false)` when one answered and none is, `None` when none answered.
fn screensaver_service() -> Option<bool> {
    let bus = bus(false)?;
    let mut answered = None;
    for (name, path) in SCREENSAVER_SERVICES {
        let active = bus
            .call_method(Some(*name), *path, Some(*name), "GetActive", &())
            .ok()
            .and_then(|reply| reply.body().deserialize::<bool>().ok());
        match active {
            Some(true) => return Some(true),
            Some(false) => answered = Some(false),
            None => {}
        }
    }
    answered
}

/// Whether the X server's own screensaver is on (MIT-SCREEN-SAVER
/// `QueryInfo`), or `None` without an X display or the extension.
fn x11_saver_on() -> Option<bool> {
    use x11rb::connection::Connection as _;
    use x11rb::protocol::screensaver::{ConnectionExt as _, State};

    std::env::var_os("DISPLAY").filter(|v| !v.is_empty())?;
    let (conn, screen) = x11rb::connect(None).ok()?;
    let root = conn.setup().roots.get(screen)?.root;
    let info = conn.screensaver_query_info(root).ok()?.reply().ok()?;
    Some(info.state == u8::from(State::ON) || info.state == u8::from(State::CYCLE))
}

fn screen() -> ScreenStatus {
    let display = std::env::var_os("DISPLAY").is_some_and(|v| !v.is_empty())
        || std::env::var_os("WAYLAND_DISPLAY").is_some_and(|v| !v.is_empty());
    platform::linux_screen_status(&platform::LinuxScreenReads {
        display,
        logind: logind(),
        screensaver_service: screensaver_service(),
        x11_saver_on: x11_saver_on(),
    })
}
