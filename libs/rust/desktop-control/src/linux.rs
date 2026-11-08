//! The Linux driver (ADR-0047 decision 10): AT-SPI through the vendored
//! agent-desktop `linux` adapter (X11 input through XTEST, Wayland through
//! the portals), behind the shared engine (`engine.rs`).
//!
//! - **Application ids** are desktop entry ids (`org.gnome.TextEditor`),
//!   or the executable's own name when it has none (`xterm`, read from
//!   `/proc/<pid>/exe`; [`crate::platform::linux_app_id`],
//!   [`crate::platform::linux_executable_name`]). An interpreter
//!   (`python3.12`, `node`, `bash`) is never the name: the app is named by
//!   the script it runs (`/proc/<pid>/cmdline`, or `comm`), and has no id
//!   when neither names it. `open_app` takes either; the
//!   vendor never launches a path outside the XDG data directories.
//! - **The screen** ([`screen`], decided by
//!   [`crate::platform::linux_screen_status`]): no display, a logind session
//!   that is not active (another user switched in), a locked session
//!   (`LockedHint`), an active screensaver (the desktop's screensaver
//!   service, or the X server's own MIT-SCREEN-SAVER), or a lock state that
//!   cannot be read is `screen_unavailable`. It fails closed: a desktop
//!   whose locker runs no screensaver service (i3lock, slock, xsecurelock)
//!   cannot show the screen is unlocked, so it counts as locked.
//! - **The user-active guard** runs on plain X11 only (`linux_input.rs`).
//!   Wayland gives no application a global view of the keyboard and
//!   pointer, so on a Wayland session ([`guard_unavailable`]: the
//!   environment, logind's `Session.Type` of the `XDG_SESSION_ID` session,
//!   the user's display session or this pid's, or an X server that is
//!   `XWayland` by its `XWAYLAND` extension or its `XWAYLAND*` `RandR`
//!   outputs) the watch fails closed and every act is refused
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

/// The desktop entry id, else the executable's own name read from
/// `/proc/<pid>` ([`platform::linux_executable_name`]: `exe`, `comm` and,
/// for an interpreter, the script `cmdline` names), never the display name
/// and never an interpreter's.
fn app_id(app: &ad::AppInfo) -> Option<String> {
    let pid = app.pid.get();
    let link = std::fs::read_link(format!("/proc/{pid}/exe")).ok();
    let link = link.as_deref().and_then(std::path::Path::to_str);
    let comm = std::fs::read_to_string(format!("/proc/{pid}/comm")).unwrap_or_default();
    let cmdline: Vec<String> = std::fs::read(format!("/proc/{pid}/cmdline"))
        .map(|bytes| {
            bytes
                .split(|b| *b == 0)
                .filter(|word| !word.is_empty())
                .map(|word| String::from_utf8_lossy(word).into_owned())
                .collect()
        })
        .unwrap_or_default();
    let executable = platform::linux_executable_name(link, comm.trim(), &cmdline);
    platform::linux_app_id(app.bundle_id.as_deref(), &executable.unwrap_or_default())
}

/// Why the user-active guard cannot run in this session, or `None` on a
/// plain X11 session ([`platform::linux_guard_unavailable`]): Wayland by
/// the environment or by logind's `Session.Type` of any session this
/// process could be in, or an X server that is `XWayland`.
pub(crate) fn guard_unavailable() -> Option<&'static str> {
    let env_type = std::env::var("XDG_SESSION_TYPE").ok();
    let logind_types = logind_session_types();
    let (xwayland_extension, randr_outputs) = x_server_kind();
    platform::linux_guard_unavailable(&platform::LinuxSessionReads {
        wayland_display: std::env::var_os("WAYLAND_DISPLAY").is_some_and(|v| !v.is_empty()),
        env_session_type: env_type.as_deref(),
        logind_session_types: &logind_types,
        xwayland_extension,
        randr_outputs: randr_outputs.as_deref(),
    })
}

/// The most `RandR` outputs read: a desktop has a handful.
const MAX_OUTPUTS: usize = 16;

/// What the X server of `DISPLAY` says about being `XWayland`: whether it
/// has the `XWAYLAND` extension (`Xwayland` 23.1 and later), and its `RandR`
/// output names (`XWAYLAND0`... on Xwayland). `(None, None)` without one.
fn x_server_kind() -> (Option<bool>, Option<Vec<String>>) {
    use x11rb::connection::Connection as _;
    use x11rb::protocol::randr::ConnectionExt as _;
    use x11rb::protocol::xproto::ConnectionExt as _;

    if std::env::var_os("DISPLAY").is_none_or(|v| v.is_empty()) {
        return (None, None);
    }
    let Ok((conn, screen)) = x11rb::connect(None) else {
        return (None, None);
    };
    let xwayland = conn
        .query_extension(b"XWAYLAND")
        .ok()
        .and_then(|cookie| cookie.reply().ok())
        .map(|reply| reply.present);
    let outputs = conn.setup().roots.get(screen).and_then(|root| {
        let resources = conn
            .randr_get_screen_resources_current(root.root)
            .ok()?
            .reply()
            .ok()?;
        Some(
            resources
                .outputs
                .iter()
                .take(MAX_OUTPUTS)
                .filter_map(|&output| {
                    let info = conn
                        .randr_get_output_info(output, resources.config_timestamp)
                        .ok()?
                        .reply()
                        .ok()?;
                    Some(String::from_utf8_lossy(&info.name).into_owned())
                })
                .collect::<Vec<_>>(),
        )
    });
    (xwayland, outputs)
}

fn bus(system: bool) -> Option<Connection> {
    let builder = if system {
        zbus::blocking::connection::Builder::system()
    } else {
        zbus::blocking::connection::Builder::session()
    };
    builder.ok()?.method_timeout(BUS_TIMEOUT).build().ok()
}

fn get(bus: &Connection, dest: &str, path: &str, iface: &str, name: &str) -> Option<OwnedValue> {
    let reply = bus
        .call_method(
            Some(dest),
            path,
            Some("org.freedesktop.DBus.Properties"),
            "Get",
            &(iface, name),
        )
        .ok()?;
    reply.body().deserialize().ok()
}

fn property(bus: &Connection, dest: &str, path: &str, iface: &str, name: &str) -> Option<bool> {
    bool::try_from(get(bus, dest, path, iface, name)?).ok()
}

const LOGIND: &str = "org.freedesktop.login1";
const LOGIND_PATH: &str = "/org/freedesktop/login1";
const LOGIND_MANAGER: &str = "org.freedesktop.login1.Manager";
const LOGIND_SESSION: &str = "org.freedesktop.login1.Session";
const LOGIND_USER: &str = "org.freedesktop.login1.User";

fn manager_call<B>(bus: &Connection, method: &str, body: &B) -> Option<OwnedObjectPath>
where
    B: serde::Serialize + zbus::zvariant::DynamicType,
{
    let reply = bus
        .call_method(
            Some(LOGIND),
            LOGIND_PATH,
            Some(LOGIND_MANAGER),
            method,
            body,
        )
        .ok()?;
    reply.body().deserialize().ok()
}

/// The logind sessions this process could be in, most specific first:
/// the session `XDG_SESSION_ID` names, the user's display session
/// (`/org/freedesktop/login1/user/self`, `Display`), and the session of
/// this pid. The last alone fails for an app started in a systemd
/// `app-*.scope` (most desktop launchers), which belongs to no session.
fn logind_sessions() -> Option<(Connection, Vec<OwnedObjectPath>)> {
    let bus = bus(true)?;
    let mut sessions: Vec<OwnedObjectPath> = Vec::new();
    if let Some(id) = std::env::var("XDG_SESSION_ID")
        .ok()
        .filter(|id| !id.trim().is_empty())
        && let Some(path) = manager_call(&bus, "GetSession", &(id.trim(),))
    {
        sessions.push(path);
    }
    if let Some(value) = get(
        &bus,
        LOGIND,
        "/org/freedesktop/login1/user/self",
        LOGIND_USER,
        "Display",
    ) && let Ok((_, path)) = <(String, OwnedObjectPath)>::try_from(value)
        && path.as_str() != "/"
    {
        sessions.push(path);
    }
    if let Some(path) = manager_call(&bus, "GetSessionByPID", &(std::process::id(),)) {
        sessions.push(path);
    }
    sessions.dedup_by(|a, b| a.as_str() == b.as_str());
    Some((bus, sessions))
}

/// What logind says about this process's session (the first
/// [`logind_sessions`] finds): (`Active`, `LockedHint`), or `None` without
/// logind or a session.
fn logind() -> Option<(bool, Option<bool>)> {
    let (bus, sessions) = logind_sessions()?;
    let path = sessions.first()?;
    let active = property(&bus, LOGIND, path.as_str(), LOGIND_SESSION, "Active")?;
    let locked = property(&bus, LOGIND, path.as_str(), LOGIND_SESSION, "LockedHint");
    Some((active, locked))
}

/// logind's `Session.Type` of every session this process could be in.
fn logind_session_types() -> Vec<String> {
    let Some((bus, sessions)) = logind_sessions() else {
        return Vec::new();
    };
    sessions
        .iter()
        .filter_map(|path| {
            String::try_from(get(&bus, LOGIND, path.as_str(), LOGIND_SESSION, "Type")?).ok()
        })
        .collect()
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
