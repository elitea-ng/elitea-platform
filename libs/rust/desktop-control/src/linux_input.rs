//! The Linux input watch (ADR-0047 decision 6): `XInput2` raw events on X11,
//! on a thread of its own, feeding [`crate::input::classify`].
//!
//! - **Listen only.** Raw events are a copy the server sends to every
//!   client that selects them on the root window; selecting them grabs
//!   nothing, and no event is dropped, changed or delayed.
//! - **The driver's own events.** The vendor's X11 input goes through XTEST,
//!   and the server reports every XTEST event as coming from the "XTEST"
//!   slave devices. An event whose source is one of them is read as the
//!   driver's own and is never a pause. Another program's XTEST input is
//!   therefore not a pause either.
//! - **Elitea's own windows.** A raw event carries no window; the watch
//!   reads the active window's `_NET_WM_PID` (EWMH) when the event arrives,
//!   so a click on the session panel is not a pause. Without a window
//!   manager that sets them the target is unknown (0) and every event
//!   counts.
//! - **The stop shortcut** is Control+Alt+Escape (Option is Alt).
//! - **Wayland fails closed.** No Wayland compositor gives an application a
//!   global view of the keyboard and pointer, and an X11 watch through
//!   `XWayland` sees only `XWayland` clients. On a Wayland session [`watch`]
//!   fails, so the host refuses every act (`user_active_guard_unavailable`);
//!   reads still work.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::thread::JoinHandle;
use std::time::Duration;

use x11rb::connection::Connection as _;
use x11rb::protocol::Event;
use x11rb::protocol::xinput::{self, ConnectionExt as _, EventMask, XIEventMask};
use x11rb::protocol::xproto::{AtomEnum, ConnectionExt as _, Window};
use x11rb::rust_connection::RustConnection;

use crate::input::{Held, InputEvent, InputFilter, InputKind, InputWatch, SignalSink, classify};
use crate::{DesktopError, SyntheticMarker};

/// How long the watch's thread sleeps when no event is waiting.
const PASS: Duration = Duration::from_millis(10);

/// X keycodes (evdev code + 8, the keycodes every current X server and
/// `XWayland` use).
const XK_ESCAPE: u32 = 9;
const XK_CONTROL_L: u32 = 37;
const XK_CONTROL_R: u32 = 105;
const XK_ALT_L: u32 = 64;
const XK_ALT_R: u32 = 108;
const XK_SHIFT_L: u32 = 50;
const XK_SHIFT_R: u32 = 62;
const XK_SUPER_L: u32 = 133;
const XK_SUPER_R: u32 = 134;

/// What the watch refuses with on a Wayland session.
pub(crate) const WAYLAND_REASON: &str = "user_active_unavailable: a Wayland session gives no \
     application a global input watch, so the user-active guard cannot run and computer use \
     cannot act here (reads still work); sign in to an X11 session to act";

/// What one raw event means to the classifier, before the target pid and
/// the modifiers are filled in. Pure, so it is tested on every OS.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Reading {
    KeyDown { escape: bool },
    Modifier { which: Mod, down: bool },
    MouseDown,
    Scroll,
    Ignored,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Mod {
    Control,
    Alt,
    Shift,
    Super,
}

fn modifier(keycode: u32) -> Option<Mod> {
    match keycode {
        XK_CONTROL_L | XK_CONTROL_R => Some(Mod::Control),
        XK_ALT_L | XK_ALT_R => Some(Mod::Alt),
        XK_SHIFT_L | XK_SHIFT_R => Some(Mod::Shift),
        XK_SUPER_L | XK_SUPER_R => Some(Mod::Super),
        _ => None,
    }
}

/// A raw key event: `down`, its keycode, and whether it is an autorepeat.
pub(crate) fn read_key(keycode: u32, down: bool, repeat: bool) -> Reading {
    match modifier(keycode) {
        Some(which) => Reading::Modifier { which, down },
        None if down && !repeat => Reading::KeyDown {
            escape: keycode == XK_ESCAPE,
        },
        // A held key repeating is still the person at the keyboard.
        None if down => Reading::KeyDown { escape: false },
        None => Reading::Ignored,
    }
}

/// A raw button press: buttons 4 to 7 are the wheel, the rest clicks.
pub(crate) fn read_button(button: u32) -> Reading {
    match button {
        4..=7 => Reading::Scroll,
        0 => Reading::Ignored,
        _ => Reading::MouseDown,
    }
}

/// The modifiers held, tracked from the raw key events.
#[allow(clippy::struct_excessive_bools)]
#[derive(Debug, Clone, Copy, Default)]
pub(crate) struct Modifiers {
    control: bool,
    alt: bool,
    shift: bool,
    super_: bool,
}

impl Modifiers {
    pub(crate) fn update(&mut self, which: Mod, down: bool) {
        match which {
            Mod::Control => self.control = down,
            Mod::Alt => self.alt = down,
            Mod::Shift => self.shift = down,
            Mod::Super => self.super_ = down,
        }
    }

    pub(crate) fn held(self) -> Held {
        Held {
            control: self.control,
            option: self.alt,
            command: self.super_,
            shift: self.shift,
        }
    }
}

/// The classifier's event for one reading. An XTEST event carries the
/// marker, so [`classify`] leaves it out.
pub(crate) fn to_input(
    reading: Reading,
    synthetic: bool,
    marker: SyntheticMarker,
    held: Held,
    target_pid: u32,
) -> Option<InputEvent> {
    let (kind, keycode) = match reading {
        Reading::KeyDown { escape } => (
            InputKind::KeyDown,
            if escape {
                crate::input::KEYCODE_ESCAPE
            } else {
                0
            },
        ),
        Reading::MouseDown => (InputKind::MouseDown, 0),
        Reading::Scroll => (InputKind::Scroll, 0),
        Reading::Modifier { .. } | Reading::Ignored => return None,
    };
    Some(InputEvent {
        kind,
        source_user_data: if synthetic { marker.value() } else { 0 },
        target_pid,
        keycode,
        held,
    })
}

/// Whether a device is one of the server's XTEST devices.
pub(crate) fn is_xtest_device(name: &[u8]) -> bool {
    String::from_utf8_lossy(name)
        .to_ascii_lowercase()
        .contains("xtest")
}

struct X11Watch {
    stop: Arc<AtomicBool>,
    thread: Option<JoinHandle<()>>,
}

impl InputWatch for X11Watch {}

impl Drop for X11Watch {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
    }
}

/// Start the watch: X11 only; a Wayland session fails closed.
pub(crate) fn watch(
    marker: SyntheticMarker,
    filter: InputFilter,
    sink: SignalSink,
) -> Result<Box<dyn InputWatch>, DesktopError> {
    if crate::linux::is_wayland() {
        return Err(DesktopError::PermissionDenied(WAYLAND_REASON.into()));
    }
    let watcher = Watcher::open()?;
    let stop = Arc::new(AtomicBool::new(false));
    let stopping = stop.clone();
    let thread = std::thread::Builder::new()
        .name("elitea-computer-input".into())
        .spawn(move || watcher.run(marker, &filter, &sink, &stopping))
        .map_err(|e| DesktopError::Internal(format!("input watch thread: {e}")))?;
    Ok(Box::new(X11Watch {
        stop,
        thread: Some(thread),
    }))
}

struct Watcher {
    conn: RustConnection,
    root: Window,
    xtest: Vec<u16>,
    active_atom: u32,
    pid_atom: u32,
}

fn x11_err(error: impl std::fmt::Display) -> DesktopError {
    DesktopError::PermissionDenied(format!(
        "user_active_unavailable: the X11 input watch could not start ({error})"
    ))
}

impl Watcher {
    fn open() -> Result<Self, DesktopError> {
        let (conn, screen) = x11rb::connect(None).map_err(x11_err)?;
        let root = conn.setup().roots[screen].root;
        let version = conn
            .xinput_xi_query_version(2, 0)
            .map_err(x11_err)?
            .reply()
            .map_err(x11_err)?;
        if version.major_version < 2 {
            return Err(x11_err("the server has no XInput 2"));
        }
        let devices = conn
            .xinput_xi_query_device(xinput::Device::ALL)
            .map_err(x11_err)?
            .reply()
            .map_err(x11_err)?;
        let xtest = devices
            .infos
            .iter()
            .filter(|d| is_xtest_device(&d.name))
            .map(|d| d.deviceid)
            .collect();
        conn.xinput_xi_select_events(
            root,
            &[EventMask {
                deviceid: xinput::Device::ALL_MASTER.into(),
                mask: vec![
                    XIEventMask::RAW_KEY_PRESS
                        | XIEventMask::RAW_KEY_RELEASE
                        | XIEventMask::RAW_BUTTON_PRESS,
                ],
            }],
        )
        .map_err(x11_err)?
        .check()
        .map_err(x11_err)?;
        let atom = |name: &[u8]| -> Result<u32, DesktopError> {
            Ok(conn
                .intern_atom(false, name)
                .map_err(x11_err)?
                .reply()
                .map_err(x11_err)?
                .atom)
        };
        let active_atom = atom(b"_NET_ACTIVE_WINDOW")?;
        let pid_atom = atom(b"_NET_WM_PID")?;
        conn.flush().map_err(x11_err)?;
        Ok(Self {
            conn,
            root,
            xtest,
            active_atom,
            pid_atom,
        })
    }

    fn card(&self, window: Window, property: u32, kind: AtomEnum) -> Option<u32> {
        self.conn
            .get_property(false, window, property, kind, 0, 1)
            .ok()?
            .reply()
            .ok()?
            .value32()?
            .next()
    }

    /// The process of the active window (EWMH), or 0.
    fn active_pid(&self) -> u32 {
        self.card(self.root, self.active_atom, AtomEnum::WINDOW)
            .filter(|w| *w != 0)
            .and_then(|w| self.card(w, self.pid_atom, AtomEnum::CARDINAL))
            .unwrap_or(0)
    }

    fn run(
        self,
        marker: SyntheticMarker,
        filter: &InputFilter,
        sink: &SignalSink,
        stop: &AtomicBool,
    ) {
        let mut modifiers = Modifiers::default();
        while !stop.load(Ordering::SeqCst) {
            let event = match self.conn.poll_for_event() {
                Ok(Some(event)) => event,
                Ok(None) => {
                    std::thread::sleep(PASS);
                    continue;
                }
                // The display went away: nothing more to hear.
                Err(_) => return,
            };
            let (reading, source) = match &event {
                Event::XinputRawKeyPress(e) => (
                    read_key(
                        e.detail,
                        true,
                        e.flags.contains(xinput::KeyEventFlags::KEY_REPEAT),
                    ),
                    e.sourceid,
                ),
                Event::XinputRawKeyRelease(e) => (read_key(e.detail, false, false), e.sourceid),
                Event::XinputRawButtonPress(e) => (read_button(e.detail), e.sourceid),
                _ => continue,
            };
            if let Reading::Modifier { which, down } = reading {
                modifiers.update(which, down);
                continue;
            }
            let synthetic = self.xtest.contains(&source);
            let target = if synthetic { 0 } else { self.active_pid() };
            if let Some(input) = to_input(reading, synthetic, marker, modifiers.held(), target)
                && let Some(signal) = classify(&input, marker, filter)
            {
                sink(signal);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::Signal;

    fn marker() -> SyntheticMarker {
        SyntheticMarker::new(0x1586).unwrap()
    }

    enum Raw {
        Key(u32, bool),
        Repeat(u32),
        Button(u32),
    }

    fn signals(events: &[(Raw, bool)], own: &[u32], target: u32) -> Vec<Signal> {
        let filter = InputFilter::new(own.to_vec());
        let mut modifiers = Modifiers::default();
        let mut out = Vec::new();
        for (raw, xtest) in events {
            let reading = match raw {
                Raw::Key(code, down) => read_key(*code, *down, false),
                Raw::Repeat(code) => read_key(*code, true, true),
                Raw::Button(b) => read_button(*b),
            };
            if let Reading::Modifier { which, down } = reading {
                modifiers.update(which, down);
                continue;
            }
            if let Some(input) = to_input(reading, *xtest, marker(), modifiers.held(), target)
                && let Some(signal) = classify(&input, marker(), &filter)
            {
                out.push(signal);
            }
        }
        out
    }

    #[test]
    fn keys_clicks_and_wheels_are_the_person() {
        let got = signals(
            &[
                (Raw::Key(38, true), false),
                (Raw::Repeat(38), false),
                (Raw::Button(1), false),
                (Raw::Button(8), false),
                (Raw::Button(5), false),
            ],
            &[],
            9,
        );
        assert_eq!(got, vec![Signal::UserActive; 5]);
    }

    #[test]
    fn ups_and_modifiers_alone_are_not() {
        assert_eq!(
            signals(
                &[
                    (Raw::Key(38, false), false),
                    (Raw::Key(XK_SHIFT_L, true), false),
                    (Raw::Key(XK_SHIFT_L, false), false),
                    (Raw::Key(XK_SUPER_R, true), false),
                ],
                &[],
                9
            ),
            Vec::<Signal>::new()
        );
    }

    #[test]
    fn xtest_events_are_the_drivers_own() {
        assert_eq!(
            signals(
                &[(Raw::Key(38, true), true), (Raw::Button(1), true)],
                &[],
                9
            ),
            Vec::<Signal>::new()
        );
        assert!(is_xtest_device(b"Virtual core XTEST keyboard"));
        assert!(is_xtest_device(b"Virtual core XTEST pointer"));
        assert!(!is_xtest_device(b"AT Translated Set 2 keyboard"));
    }

    #[test]
    fn control_alt_escape_stops_even_over_elitea() {
        let chord = [
            (Raw::Key(XK_CONTROL_R, true), false),
            (Raw::Key(XK_ALT_L, true), false),
            (Raw::Key(XK_ESCAPE, true), false),
        ];
        assert_eq!(signals(&chord, &[7], 7), vec![Signal::Stop]);
        let with_super = [
            (Raw::Key(XK_SUPER_L, true), false),
            (Raw::Key(XK_CONTROL_L, true), false),
            (Raw::Key(XK_ALT_L, true), false),
            (Raw::Key(XK_ESCAPE, true), false),
        ];
        assert_eq!(signals(&with_super, &[], 9), vec![Signal::UserActive]);
        let released = [
            (Raw::Key(XK_CONTROL_L, true), false),
            (Raw::Key(XK_ALT_R, true), false),
            (Raw::Key(XK_ALT_R, false), false),
            (Raw::Key(XK_ESCAPE, true), false),
        ];
        assert_eq!(signals(&released, &[], 9), vec![Signal::UserActive]);
    }

    #[test]
    fn input_while_elitea_is_in_front_is_left_out() {
        assert_eq!(
            signals(&[(Raw::Key(38, true), false)], &[7], 7),
            Vec::<Signal>::new()
        );
        assert_eq!(
            signals(&[(Raw::Key(38, true), false)], &[7], 8),
            vec![Signal::UserActive]
        );
        // An unknown target (no EWMH) counts.
        assert_eq!(
            signals(&[(Raw::Key(38, true), false)], &[7], 0),
            vec![Signal::UserActive]
        );
    }

    #[test]
    fn the_wayland_reason_names_the_guard() {
        assert!(WAYLAND_REASON.starts_with("user_active_unavailable"));
        assert!(WAYLAND_REASON.contains("Wayland"));
    }
}
