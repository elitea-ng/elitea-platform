//! `browser_press` keys as `Input.dispatchKeyEvent` parameters.

use elitea_browser_tools::call::{Key, Modifier};
use serde_json::{Value, json};

/// The CDP modifier bit of one modifier.
const fn bit(modifier: Modifier) -> u32 {
    match modifier {
        Modifier::Alt => 1,
        Modifier::Control => 2,
        Modifier::Meta => 4,
        Modifier::Shift => 8,
    }
}

/// `(key, code, windowsVirtualKeyCode, text)` of a modifier key.
const fn modifier_key(modifier: Modifier) -> (&'static str, &'static str, u32) {
    match modifier {
        Modifier::Alt => ("Alt", "AltLeft", 18),
        Modifier::Control => ("Control", "ControlLeft", 17),
        Modifier::Meta => ("Meta", "MetaLeft", 91),
        Modifier::Shift => ("Shift", "ShiftLeft", 16),
    }
}

/// `(key, code, windowsVirtualKeyCode, text)` of a named key.
fn named(key: &str) -> Option<(&'static str, &'static str, u32, &'static str)> {
    Some(match key {
        "Enter" => ("Enter", "Enter", 13, "\r"),
        "Tab" => ("Tab", "Tab", 9, ""),
        "Escape" => ("Escape", "Escape", 27, ""),
        "Backspace" => ("Backspace", "Backspace", 8, ""),
        "Delete" => ("Delete", "Delete", 46, ""),
        "Space" => (" ", "Space", 32, " "),
        "Home" => ("Home", "Home", 36, ""),
        "End" => ("End", "End", 35, ""),
        "PageUp" => ("PageUp", "PageUp", 33, ""),
        "PageDown" => ("PageDown", "PageDown", 34, ""),
        "ArrowUp" => ("ArrowUp", "ArrowUp", 38, ""),
        "ArrowDown" => ("ArrowDown", "ArrowDown", 40, ""),
        "ArrowLeft" => ("ArrowLeft", "ArrowLeft", 37, ""),
        "ArrowRight" => ("ArrowRight", "ArrowRight", 39, ""),
        "F1" => ("F1", "F1", 112, ""),
        "F2" => ("F2", "F2", 113, ""),
        "F3" => ("F3", "F3", 114, ""),
        "F4" => ("F4", "F4", 115, ""),
        "F5" => ("F5", "F5", 116, ""),
        "F6" => ("F6", "F6", 117, ""),
        "F7" => ("F7", "F7", 118, ""),
        "F8" => ("F8", "F8", 119, ""),
        "F9" => ("F9", "F9", 120, ""),
        "F10" => ("F10", "F10", 121, ""),
        "F11" => ("F11", "F11", 122, ""),
        "F12" => ("F12", "F12", 123, ""),
        _ => return None,
    })
}

/// The events of one key or chord, in order: modifiers down, the key down
/// and up, modifiers up.
#[must_use]
pub fn events(key: &Key) -> Vec<Value> {
    let modifiers: u32 = key.modifiers.iter().map(|modifier| bit(*modifier)).sum();
    // A chord with Control, Alt or Meta types no character.
    let types_text = key
        .modifiers
        .iter()
        .all(|modifier| *modifier == Modifier::Shift);
    let mut out = Vec::new();
    let mut held = 0;
    for modifier in &key.modifiers {
        held |= bit(*modifier);
        let (name, code, vk) = modifier_key(*modifier);
        out.push(json!({
            "type": "rawKeyDown", "key": name, "code": code,
            "windowsVirtualKeyCode": vk, "modifiers": held,
        }));
    }
    let (name, code, vk, text) = if let Some((name, code, vk, text)) = named(&key.key) {
        (name.to_owned(), code.to_owned(), vk, text.to_owned())
    } else {
        {
            let ch = key.key.chars().next().unwrap_or(' ');
            let shown = if key.modifiers.contains(&Modifier::Shift) {
                ch.to_uppercase().collect()
            } else {
                key.key.clone()
            };
            let (code, vk) = if ch.is_ascii_alphabetic() {
                (
                    format!("Key{}", ch.to_ascii_uppercase()),
                    u32::from(ch.to_ascii_uppercase()),
                )
            } else if ch.is_ascii_digit() {
                (format!("Digit{ch}"), u32::from(ch))
            } else {
                (String::new(), 0)
            };
            (shown.clone(), code, vk, shown)
        }
    };
    let text = if types_text { text } else { String::new() };
    let mut down = json!({
        "type": if text.is_empty() { "rawKeyDown" } else { "keyDown" },
        "key": name, "code": code, "windowsVirtualKeyCode": vk, "modifiers": modifiers,
    });
    if !text.is_empty() {
        down["text"] = Value::String(text.clone());
        down["unmodifiedText"] = Value::String(text);
    }
    out.push(down);
    out.push(json!({
        "type": "keyUp", "key": name, "code": code,
        "windowsVirtualKeyCode": vk, "modifiers": modifiers,
    }));
    for modifier in key.modifiers.iter().rev() {
        held &= !bit(*modifier);
        let (name, code, vk) = modifier_key(*modifier);
        out.push(json!({
            "type": "keyUp", "key": name, "code": code,
            "windowsVirtualKeyCode": vk, "modifiers": held,
        }));
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn enter_types_a_carriage_return() {
        let events = events(&Key::parse("Enter").unwrap());
        assert_eq!(events.len(), 2);
        assert_eq!(events[0]["type"], "keyDown");
        assert_eq!(events[0]["text"], "\r");
        assert_eq!(events[0]["windowsVirtualKeyCode"], 13);
        assert_eq!(events[1]["type"], "keyUp");
    }

    #[test]
    fn a_control_chord_types_nothing_and_releases_in_reverse() {
        let events = events(&Key::parse("Control+Shift+K").unwrap());
        let types: Vec<_> = events.iter().map(|event| event["type"].clone()).collect();
        assert_eq!(
            types,
            [
                "rawKeyDown",
                "rawKeyDown",
                "rawKeyDown",
                "keyUp",
                "keyUp",
                "keyUp"
            ]
        );
        assert_eq!(events[2]["key"], "K");
        assert_eq!(events[2]["code"], "KeyK");
        assert_eq!(events[2]["modifiers"], 2 | 8);
        assert!(events[2].get("text").is_none());
        assert_eq!(events[3]["key"], "K");
        assert_eq!(events[4]["key"], "Shift");
        assert_eq!(events[5]["key"], "Control");
        assert_eq!(events[5]["modifiers"], 0);
    }

    #[test]
    fn a_character_types_itself() {
        let events = events(&Key::parse("a").unwrap());
        assert_eq!(events[0]["text"], "a");
        assert_eq!(events[0]["code"], "KeyA");
        let shifted = super::events(&Key::parse("Shift+a").unwrap());
        assert_eq!(shifted[1]["text"], "A");
        let space = super::events(&Key::parse("Space").unwrap());
        assert_eq!(space[0]["text"], " ");
    }
}
