//! The Windows input watch (ADR-0047 decision 6): low-level keyboard and
//! mouse hooks through `willhook` (its safe API; winapi underneath), on a
//! thread of its own, feeding [`crate::input::classify`].
//!
//! - **Listen only.** `willhook` passes every event on (`CallNextHookEx`);
//!   it never drops, changes or delays one.
//! - **The driver's own events.** `SendInput` cannot carry a user-data
//!   marker a hook can read, but Windows flags every injected event
//!   (`LLKHF_INJECTED`, `LLMHF_INJECTED`). An injected event is read as the
//!   driver's own and is never a pause. Another program's injected input
//!   is therefore not a pause either; that is the price of the OS's model.
//! - **Elitea's own windows.** A hook event carries no target process; the
//!   watch reads the foreground window's process when the event arrives,
//!   so a click on the session panel is not a pause.
//! - **The stop shortcut** is Control+Alt+Escape here (Option is Alt), as
//!   [`crate::STOP_SHORTCUT`] names it.
//! - **One hook per process.** `willhook` keeps its hooks in a process
//!   global and refuses a second while one runs; a second watch fails with
//!   `internal`.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::thread::JoinHandle;
use std::time::Duration;

use willhook::{
    InputEvent as HookEvent, IsEventInjected, KeyPress, KeyboardKey, MouseButtonPress,
    MouseEventType,
};

use crate::input::{Held, InputEvent, InputFilter, InputKind, InputWatch, SignalSink, classify};
use crate::{DesktopError, SyntheticMarker};

/// How often the watch's thread looks for its stop flag.
const PASS: Duration = Duration::from_millis(100);

/// What one hook event means to the classifier, before the target pid and
/// the modifiers are filled in. Pure, so it is tested on every OS.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Reading {
    /// A key went down (`escape`: it is Escape).
    KeyDown {
        escape: bool,
    },
    /// A modifier changed: `Some(true)` down, `Some(false)` up.
    Modifier {
        which: Mod,
        down: bool,
    },
    MouseDown,
    Scroll,
    /// A bare move, a key or button up, or anything else.
    Ignored,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Mod {
    Control,
    Alt,
    Shift,
    Win,
}

fn modifier(key: KeyboardKey) -> Option<Mod> {
    match key {
        KeyboardKey::LeftControl | KeyboardKey::RightControl => Some(Mod::Control),
        KeyboardKey::LeftAlt | KeyboardKey::RightAlt => Some(Mod::Alt),
        KeyboardKey::LeftShift | KeyboardKey::RightShift => Some(Mod::Shift),
        KeyboardKey::LeftWindows | KeyboardKey::RightWindows => Some(Mod::Win),
        _ => None,
    }
}

/// Reads one hook event.
pub(crate) fn read(event: &HookEvent) -> Reading {
    match event {
        HookEvent::Keyboard(k) => {
            let down = match k.pressed {
                KeyPress::Down(_) => true,
                KeyPress::Up(_) => false,
                KeyPress::Other(_) => return Reading::Ignored,
            };
            match k.key {
                Some(key) => match modifier(key) {
                    Some(which) => Reading::Modifier { which, down },
                    None if down => Reading::KeyDown {
                        escape: key == KeyboardKey::Escape,
                    },
                    None => Reading::Ignored,
                },
                None if down => Reading::KeyDown { escape: false },
                None => Reading::Ignored,
            }
        }
        HookEvent::Mouse(m) => match m.event {
            MouseEventType::Press(p) if p.pressed == MouseButtonPress::Down => Reading::MouseDown,
            MouseEventType::Wheel(_) => Reading::Scroll,
            _ => Reading::Ignored,
        },
        HookEvent::Other(_) => Reading::Ignored,
    }
}

/// Whether the OS flagged the event as injected (the driver's own).
pub(crate) fn injected(event: &HookEvent) -> bool {
    let flag = match event {
        HookEvent::Keyboard(k) => k.is_injected,
        HookEvent::Mouse(m) => m.is_injected,
        HookEvent::Other(_) => None,
    };
    flag == Some(IsEventInjected::Injected)
}

/// The modifiers held, tracked from the hook's own key events.
#[allow(clippy::struct_excessive_bools)]
#[derive(Debug, Clone, Copy, Default)]
pub(crate) struct Modifiers {
    control: bool,
    alt: bool,
    shift: bool,
    win: bool,
}

impl Modifiers {
    pub(crate) fn update(&mut self, which: Mod, down: bool) {
        match which {
            Mod::Control => self.control = down,
            Mod::Alt => self.alt = down,
            Mod::Shift => self.shift = down,
            Mod::Win => self.win = down,
        }
    }

    pub(crate) fn held(self) -> Held {
        Held {
            control: self.control,
            option: self.alt,
            command: self.win,
            shift: self.shift,
        }
    }
}

/// The classifier's event for one reading, or `None` for one it never sees.
/// An injected event carries the marker, so [`classify`] leaves it out.
pub(crate) fn to_input(
    reading: Reading,
    injected: bool,
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
        source_user_data: if injected { marker.value() } else { 0 },
        target_pid,
        keycode,
        held,
    })
}

struct WinWatch {
    stop: Arc<AtomicBool>,
    alive: crate::input::Alive,
    thread: Option<JoinHandle<()>>,
}

impl InputWatch for WinWatch {
    fn alive(&self) -> bool {
        self.alive.get()
    }
}

impl Drop for WinWatch {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
    }
}

/// Start the watch.
pub(crate) fn watch(
    marker: SyntheticMarker,
    filter: InputFilter,
    sink: SignalSink,
    alive: crate::input::Alive,
) -> Result<Box<dyn InputWatch>, DesktopError> {
    let (ready, started) = std::sync::mpsc::channel::<Result<(), DesktopError>>();
    let stop = Arc::new(AtomicBool::new(false));
    let stopping = stop.clone();
    let living = alive.guard();
    let thread = std::thread::Builder::new()
        .name("elitea-computer-input".into())
        .spawn(move || {
            // Cleared however the thread ends, a dropped hook included.
            let _living = living;
            run(marker, &filter, &sink, &stopping, &ready);
        })
        .map_err(|e| DesktopError::Internal(format!("input watch thread: {e}")))?;
    match started.recv() {
        Ok(Ok(())) => Ok(Box::new(WinWatch {
            stop,
            alive,
            thread: Some(thread),
        })),
        Ok(Err(error)) => {
            let _ = thread.join();
            Err(error)
        }
        Err(_) => {
            let _ = thread.join();
            Err(DesktopError::Internal("the input watch stopped".into()))
        }
    }
}

fn run(
    marker: SyntheticMarker,
    filter: &InputFilter,
    sink: &SignalSink,
    stop: &AtomicBool,
    ready: &std::sync::mpsc::Sender<Result<(), DesktopError>>,
) {
    // The hook lives on this thread and is removed when it drops here.
    let Some(hook) = willhook::willhook() else {
        let _ = ready.send(Err(DesktopError::Internal(
            "the keyboard and mouse hooks could not be installed (another watch runs?)".into(),
        )));
        return;
    };
    if ready.send(Ok(())).is_err() {
        return;
    }
    let mut modifiers = Modifiers::default();
    while !stop.load(Ordering::SeqCst) {
        match hook.try_recv() {
            Ok(event) => {
                let reading = read(&event);
                if let Reading::Modifier { which, down } = reading {
                    modifiers.update(which, down);
                    continue;
                }
                let target = crate::windows::foreground_pid();
                if let Some(input) =
                    to_input(reading, injected(&event), marker, modifiers.held(), target)
                    && let Some(signal) = classify(&input, marker, filter)
                {
                    sink(signal);
                }
            }
            Err(std::sync::mpsc::TryRecvError::Empty) => std::thread::sleep(PASS / 10),
            // The hook's channel closed: the OS removed the hook (or its
            // thread ended), so nothing more will be heard. End the thread:
            // the watch reads as dead and the driver refuses every act.
            Err(std::sync::mpsc::TryRecvError::Disconnected) => break,
        }
    }
    drop(hook);
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::Signal;
    use willhook::{
        IsSystemKeyPress, KeyboardEvent, MouseButton, MouseClick, MouseEvent, MouseMoveEvent,
        MousePressEvent, MouseWheel, MouseWheelDirection, MouseWheelEvent,
    };

    fn key(key: KeyboardKey, down: bool, injected: bool) -> HookEvent {
        HookEvent::Keyboard(KeyboardEvent {
            pressed: if down {
                KeyPress::Down(IsSystemKeyPress::Normal)
            } else {
                KeyPress::Up(IsSystemKeyPress::Normal)
            },
            key: Some(key),
            is_injected: Some(if injected {
                IsEventInjected::Injected
            } else {
                IsEventInjected::NotInjected
            }),
        })
    }

    fn marker() -> SyntheticMarker {
        SyntheticMarker::new(WINDOWS_TEST_MARK).unwrap()
    }

    const WINDOWS_TEST_MARK: i64 = 0x1586;

    /// Feeds hook events through the watch's own path.
    fn signals(events: &[HookEvent], own: &[u32], target: u32) -> Vec<Signal> {
        let filter = InputFilter::new(own.to_vec());
        let mut modifiers = Modifiers::default();
        let mut out = Vec::new();
        for event in events {
            let reading = read(event);
            if let Reading::Modifier { which, down } = reading {
                modifiers.update(which, down);
                continue;
            }
            if let Some(input) =
                to_input(reading, injected(event), marker(), modifiers.held(), target)
                && let Some(signal) = classify(&input, marker(), &filter)
            {
                out.push(signal);
            }
        }
        out
    }

    #[test]
    fn keys_clicks_and_wheels_are_the_person() {
        let click = HookEvent::Mouse(MouseEvent {
            event: MouseEventType::Press(MousePressEvent {
                pressed: MouseButtonPress::Down,
                button: MouseButton::Left(MouseClick::SingleClick),
            }),
            is_injected: Some(IsEventInjected::NotInjected),
        });
        let wheel = HookEvent::Mouse(MouseEvent {
            event: MouseEventType::Wheel(MouseWheelEvent {
                wheel: MouseWheel::Vertical,
                direction: Some(MouseWheelDirection::Forward),
            }),
            is_injected: Some(IsEventInjected::NotInjected),
        });
        assert_eq!(
            signals(&[key(KeyboardKey::A, true, false), click, wheel], &[], 9),
            vec![Signal::UserActive; 3]
        );
    }

    #[test]
    fn moves_ups_and_modifiers_alone_are_not() {
        let mv = HookEvent::Mouse(MouseEvent {
            event: MouseEventType::Move(MouseMoveEvent { point: None }),
            is_injected: Some(IsEventInjected::NotInjected),
        });
        assert_eq!(
            signals(
                &[
                    mv,
                    key(KeyboardKey::A, false, false),
                    key(KeyboardKey::LeftShift, true, false),
                    key(KeyboardKey::LeftShift, false, false),
                ],
                &[],
                9
            ),
            Vec::<Signal>::new()
        );
    }

    #[test]
    fn injected_events_are_the_drivers_own() {
        assert_eq!(
            signals(&[key(KeyboardKey::A, true, true)], &[], 9),
            Vec::<Signal>::new()
        );
    }

    #[test]
    fn control_alt_escape_stops_even_over_elitea() {
        let chord = [
            key(KeyboardKey::LeftControl, true, false),
            key(KeyboardKey::RightAlt, true, false),
            key(KeyboardKey::Escape, true, false),
        ];
        assert_eq!(signals(&chord, &[7], 7), vec![Signal::Stop]);
        // With the Windows key held it is not the shortcut.
        let with_win = [
            key(KeyboardKey::LeftWindows, true, false),
            key(KeyboardKey::LeftControl, true, false),
            key(KeyboardKey::LeftAlt, true, false),
            key(KeyboardKey::Escape, true, false),
        ];
        assert_eq!(signals(&with_win, &[], 9), vec![Signal::UserActive]);
        // Released modifiers no longer count.
        let released = [
            key(KeyboardKey::LeftControl, true, false),
            key(KeyboardKey::LeftAlt, true, false),
            key(KeyboardKey::LeftAlt, false, false),
            key(KeyboardKey::Escape, true, false),
        ];
        assert_eq!(signals(&released, &[], 9), vec![Signal::UserActive]);
    }

    #[test]
    fn input_while_elitea_is_in_front_is_left_out() {
        assert_eq!(
            signals(&[key(KeyboardKey::A, true, false)], &[7], 7),
            Vec::<Signal>::new()
        );
        assert_eq!(
            signals(&[key(KeyboardKey::A, true, false)], &[7], 8),
            vec![Signal::UserActive]
        );
    }
}
