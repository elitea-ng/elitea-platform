//! Hard limits the driver holds whoever calls it (ADR-0047 decision 5), and
//! the synthetic-event marker the user-active guard reads (decision 6).

use crate::{KeyChord, Modifier};

/// The chords a driver never sends: quit, force quit, log out, shut down,
/// lock screen, switch user, Spotlight and Siri. A chord is denied when its
/// key matches an entry and it holds at least that entry's modifiers, so
/// adding a modifier to a denied chord does not get it through.
const DENIED: &[(&[Modifier], &str)] = &[
    // Quit and force quit.
    (&[Modifier::Command], "q"),
    (&[Modifier::Command, Modifier::Option], "escape"),
    // Log out (with and without the confirmation).
    (&[Modifier::Command, Modifier::Shift], "q"),
    // Lock screen.
    (&[Modifier::Command, Modifier::Control], "q"),
    // Shut down, restart, sleep (the power-key chords).
    (&[Modifier::Control], "power"),
    (&[Modifier::Command, Modifier::Control], "power"),
    (&[Modifier::Command, Modifier::Option], "power"),
    (
        &[Modifier::Command, Modifier::Option, Modifier::Control],
        "eject",
    ),
    (&[Modifier::Command, Modifier::Control], "eject"),
    (&[Modifier::Shift, Modifier::Control], "eject"),
    // Empty the Trash, a destructive Finder chord.
    (&[Modifier::Command, Modifier::Shift], "delete"),
    // Spotlight and its Finder variant.
    (&[Modifier::Command], "space"),
    (&[Modifier::Command, Modifier::Option], "space"),
    // Siri (the default "Hold Command Space" is covered above; the
    // function-key variant).
    (&[], "siri"),
];

/// Key names that mean the same key.
fn canonical_key(key: &str) -> &str {
    match key {
        "esc" => "escape",
        "backspace" => "delete",
        " " | "spacebar" => "space",
        other => other,
    }
}

/// Whether the driver refuses to send `chord` (`chord_refused`).
#[must_use]
pub fn is_denied_chord(chord: &KeyChord) -> bool {
    let lower = chord.key.to_ascii_lowercase();
    let key = canonical_key(&lower);
    DENIED.iter().any(|(mods, denied_key)| {
        *denied_key == key && mods.iter().all(|m| chord.modifiers.contains(m))
    })
}

/// Where one input event came from.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum InputOrigin {
    /// The driver posted it.
    Synthetic,
    /// The person's keyboard or pointer, or another program.
    Physical,
}

/// The value the driver stamps into every event it posts (on macOS the
/// `kCGEventSourceUserData` field). An event tap compares an event's field
/// with it to tell the driver's own events from the person's (ADR-0047
/// decision 6, the user-active guard of #1584).
///
/// The value is never zero, because zero is what the HID system leaves in
/// the field of a physical event.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SyntheticMarker(i64);

impl SyntheticMarker {
    /// A marker with this value; `None` for zero.
    #[must_use]
    pub fn new(value: i64) -> Option<Self> {
        (value != 0).then_some(Self(value))
    }

    #[must_use]
    pub fn value(self) -> i64 {
        self.0
    }

    /// Classifies an event by its source user-data field.
    #[must_use]
    pub fn classify(self, event_source_user_data: i64) -> InputOrigin {
        if event_source_user_data == self.0 {
            InputOrigin::Synthetic
        } else {
            InputOrigin::Physical
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn chord(text: &str) -> KeyChord {
        KeyChord::parse(text).unwrap()
    }

    #[test]
    fn quit_logout_lock_and_spotlight_are_denied() {
        for c in [
            "cmd+q",
            "cmd+shift+q",
            "cmd+alt+shift+q",
            "ctrl+cmd+q",
            "cmd+opt+esc",
            "cmd+space",
            "cmd+option+space",
            "ctrl+power",
            "cmd+shift+backspace",
        ] {
            assert!(is_denied_chord(&chord(c)), "{c} must be denied");
        }
    }

    #[test]
    fn ordinary_chords_pass() {
        for c in [
            "cmd+c",
            "cmd+v",
            "q",
            "shift+q",
            "return",
            "cmd+w",
            "ctrl+space",
            "space",
        ] {
            assert!(!is_denied_chord(&chord(c)), "{c} must pass");
        }
    }

    #[test]
    fn the_marker_tells_the_drivers_events_from_physical_ones() {
        let marker = SyntheticMarker::new(42).unwrap();
        assert_eq!(marker.classify(42), InputOrigin::Synthetic);
        assert_eq!(marker.classify(0), InputOrigin::Physical);
        assert_eq!(marker.classify(7), InputOrigin::Physical);
        assert_eq!(SyntheticMarker::new(0), None);
    }
}
