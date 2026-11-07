//! The person's input while a computer session runs (ADR-0047 decision 6):
//! what pauses the agent (the user-active guard) and what stops it (the
//! stop shortcut).
//!
//! [`classify`] decides one input event, whoever read it from the OS. The
//! macOS watch ([`DesktopDriver::watch_input`](crate::DesktopDriver::watch_input))
//! feeds it from a listen-only event tap; tests feed it directly.
//!
//! - **The driver's own events** carry the [`SyntheticMarker`] in their
//!   source user-data field and are never the person's.
//! - **The stop shortcut** ([`STOP_SHORTCUT`], Control-Option-Escape) is a
//!   key down of Escape with Control and Option held and neither Command
//!   nor Shift: it stops the session ([`Signal::Stop`]), whichever app is in
//!   front. Command-Option-Escape (Force Quit) is not it.
//! - **The person's input** is a key down, a mouse button down, a drag or a
//!   scroll ([`Signal::UserActive`]). A modifier key alone and a pointer that
//!   only moves are not: they change nothing in any app.
//! - **Elitea's own windows** are left out: an event the OS aims at an
//!   Elitea process (the session panel, an approval card, the menu-bar
//!   item) is not the person using the computer the agent drives, so
//!   pressing **Pause**, **Resume** or **Approve** is not a pause.

use crate::SyntheticMarker;

/// The stop shortcut, as the panel and the docs name it.
pub const STOP_SHORTCUT: &str = "ctrl+option+escape";

/// The virtual key code of Escape (`kVK_Escape`).
pub const KEYCODE_ESCAPE: u16 = 53;

/// The kinds of input event the watch reads.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum InputKind {
    KeyDown,
    /// A modifier key went down or up, alone.
    FlagsChanged,
    /// Any mouse button went down.
    MouseDown,
    /// The pointer moved with a button down.
    MouseDragged,
    /// The pointer moved with no button down.
    MouseMoved,
    Scroll,
    /// Anything else (key and button ups, tablet events).
    Other,
}

/// The modifiers held during a key event. Each is one independent key.
#[allow(clippy::struct_excessive_bools)]
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Held {
    pub control: bool,
    pub option: bool,
    pub command: bool,
    pub shift: bool,
}

/// One input event as the watch read it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct InputEvent {
    pub kind: InputKind,
    /// The event source's user-data field (the driver's marker on its own).
    pub source_user_data: i64,
    /// The process the event is aimed at, when the OS tells it (0: unknown).
    pub target_pid: u32,
    /// The virtual key code of a key event.
    pub keycode: u16,
    pub held: Held,
}

/// What an input event means for the session.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Signal {
    /// The person used the computer: pause the agent (`user_active`).
    UserActive,
    /// The stop shortcut: end the session (`stopped_by_user`).
    Stop,
}

/// What the watch leaves out besides the driver's own events: the events
/// aimed at Elitea's own processes.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct InputFilter {
    own_pids: Vec<u32>,
}

impl InputFilter {
    /// A filter that leaves out events aimed at `own_pids`.
    #[must_use]
    pub fn new(own_pids: Vec<u32>) -> Self {
        Self { own_pids }
    }

    fn is_own(&self, pid: u32) -> bool {
        pid != 0 && self.own_pids.contains(&pid)
    }
}

/// Whether a key down is the stop shortcut.
#[must_use]
pub fn is_stop_chord(keycode: u16, held: Held) -> bool {
    keycode == KEYCODE_ESCAPE && held.control && held.option && !held.command && !held.shift
}

/// What `event` means, or `None` when it is not the person using the
/// computer (the driver's own event, a modifier alone, a bare pointer move,
/// an event aimed at Elitea).
#[must_use]
pub fn classify(
    event: &InputEvent,
    marker: SyntheticMarker,
    filter: &InputFilter,
) -> Option<Signal> {
    if marker.classify(event.source_user_data) == crate::InputOrigin::Synthetic {
        return None;
    }
    match event.kind {
        InputKind::KeyDown if is_stop_chord(event.keycode, event.held) => Some(Signal::Stop),
        InputKind::KeyDown | InputKind::MouseDown | InputKind::MouseDragged | InputKind::Scroll => {
            (!filter.is_own(event.target_pid)).then_some(Signal::UserActive)
        }
        InputKind::FlagsChanged | InputKind::MouseMoved | InputKind::Other => None,
    }
}

/// A running input watch. Dropping it stops the watch; no signal is heard
/// after the drop returns.
pub trait InputWatch: Send {}

/// Hears each [`Signal`], on the watch's own thread.
pub type SignalSink = Box<dyn Fn(Signal) + Send + Sync>;

#[cfg(test)]
mod tests {
    use super::*;

    const MARK: i64 = 0x1584;

    fn marker() -> SyntheticMarker {
        SyntheticMarker::new(MARK).unwrap()
    }

    fn event(kind: InputKind) -> InputEvent {
        InputEvent {
            kind,
            source_user_data: 0,
            target_pid: 0,
            keycode: 0,
            held: Held::default(),
        }
    }

    fn stop_key(held: Held) -> InputEvent {
        InputEvent {
            keycode: KEYCODE_ESCAPE,
            held,
            ..event(InputKind::KeyDown)
        }
    }

    const CTRL_OPT: Held = Held {
        control: true,
        option: true,
        command: false,
        shift: false,
    };

    #[test]
    fn keys_clicks_drags_and_scrolls_are_the_person_using_the_computer() {
        let filter = InputFilter::default();
        for kind in [
            InputKind::KeyDown,
            InputKind::MouseDown,
            InputKind::MouseDragged,
            InputKind::Scroll,
        ] {
            assert_eq!(
                classify(&event(kind), marker(), &filter),
                Some(Signal::UserActive),
                "{kind:?}"
            );
        }
    }

    #[test]
    fn a_modifier_alone_a_bare_move_and_ups_are_not() {
        let filter = InputFilter::default();
        for kind in [
            InputKind::FlagsChanged,
            InputKind::MouseMoved,
            InputKind::Other,
        ] {
            assert_eq!(classify(&event(kind), marker(), &filter), None, "{kind:?}");
        }
    }

    #[test]
    fn the_drivers_own_events_are_never_the_persons() {
        let filter = InputFilter::default();
        for kind in [InputKind::KeyDown, InputKind::MouseDown, InputKind::Scroll] {
            let own = InputEvent {
                source_user_data: MARK,
                ..event(kind)
            };
            assert_eq!(classify(&own, marker(), &filter), None, "{kind:?}");
        }
        // Even the stop chord, if the driver ever posted it.
        let posted = InputEvent {
            source_user_data: MARK,
            ..stop_key(CTRL_OPT)
        };
        assert_eq!(classify(&posted, marker(), &filter), None);
    }

    #[test]
    fn control_option_escape_stops_and_force_quit_does_not() {
        let filter = InputFilter::default();
        assert_eq!(
            classify(&stop_key(CTRL_OPT), marker(), &filter),
            Some(Signal::Stop)
        );
        // Command-Option-Escape is Force Quit: ordinary input.
        let force_quit = Held {
            control: false,
            option: true,
            command: true,
            shift: false,
        };
        assert_eq!(
            classify(&stop_key(force_quit), marker(), &filter),
            Some(Signal::UserActive)
        );
        // Extra or missing modifiers are not the shortcut.
        for held in [
            Held {
                command: true,
                ..CTRL_OPT
            },
            Held {
                shift: true,
                ..CTRL_OPT
            },
            Held {
                option: false,
                ..CTRL_OPT
            },
        ] {
            assert!(!is_stop_chord(KEYCODE_ESCAPE, held), "{held:?}");
        }
        assert!(!is_stop_chord(12, CTRL_OPT));
        // The shortcut stops even while Elitea is in front.
        let own = InputFilter::new(vec![7]);
        let aimed_at_elitea = InputEvent {
            target_pid: 7,
            ..stop_key(CTRL_OPT)
        };
        assert_eq!(
            classify(&aimed_at_elitea, marker(), &own),
            Some(Signal::Stop)
        );
    }

    #[test]
    fn input_aimed_at_elitea_is_left_out() {
        let filter = InputFilter::new(vec![7]);
        for kind in [InputKind::KeyDown, InputKind::MouseDown, InputKind::Scroll] {
            let to_elitea = InputEvent {
                target_pid: 7,
                ..event(kind)
            };
            assert_eq!(classify(&to_elitea, marker(), &filter), None, "{kind:?}");
            let elsewhere = InputEvent {
                target_pid: 8,
                ..event(kind)
            };
            assert_eq!(
                classify(&elsewhere, marker(), &filter),
                Some(Signal::UserActive),
                "{kind:?}"
            );
        }
        // An unknown target (0) is never Elitea.
        assert_eq!(
            classify(&event(InputKind::MouseDown), marker(), &filter),
            Some(Signal::UserActive)
        );
    }
}
