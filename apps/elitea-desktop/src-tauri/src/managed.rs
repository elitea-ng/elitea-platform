//! Managed configuration of the desktop app (ADR-0052 decision 7, W6.2).
//!
//! macOS: a configuration profile (Jamf, Intune, Kandji, ...) with a
//! `com.apple.ManagedClient.preferences` payload for the app's bundle
//! identifier puts its keys in the app's defaults domain as FORCED values. Only
//! a forced value counts here: a key someone wrote with `defaults write` is not
//! the organization's. Windows and Linux read nothing yet (the Windows host
//! comes with #1828).
//!
//! Keys:
//!
//! - `support_contact`: the IT support line shown in the About panel, the
//!   account menu and Settings › Troubleshoot. It beats the workspace's
//!   `client_policy.support_contact`.

use serde_json::Value;

/// The server's bound (`platformconfig.MaxSupportContactRunes`), in characters.
pub const MAX_SUPPORT_CONTACT_CHARS: usize = 200;

/// One line as the apps show it: trimmed, at most 200 characters, no control
/// or bidirectional-override characters; anything else is none. The same
/// rule as `normalizeSupportContact` in `@elitea/client-core/support`.
#[must_use]
pub fn normalize_support_contact(raw: &str) -> Option<String> {
    let text = raw.trim();
    let unsafe_char =
        |c: char| c.is_control() || matches!(c, '\u{202A}'..='\u{202E}' | '\u{2066}'..='\u{2069}');
    if text.is_empty()
        || text.chars().count() > MAX_SUPPORT_CONTACT_CHARS
        || text.chars().any(unsafe_char)
    {
        return None;
    }
    Some(text.to_owned())
}

/// Managed beats workspace; an invalid managed value does not hide the workspace's.
#[must_use]
pub fn effective_support_contact(managed: Option<&str>, policy: Option<&Value>) -> Option<String> {
    managed.and_then(normalize_support_contact).or_else(|| {
        policy?
            .get("support_contact")?
            .as_str()
            .and_then(normalize_support_contact)
    })
}

/// The managed `support_contact`, if the device management set one.
#[must_use]
pub fn support_contact() -> Option<String> {
    forced_string("support_contact")
        .as_deref()
        .and_then(normalize_support_contact)
}

#[cfg(target_os = "macos")]
fn forced_string(key: &str) -> Option<String> {
    use objc2_foundation::{NSString, NSUserDefaults};
    let key = NSString::from_str(key);
    let defaults = NSUserDefaults::standardUserDefaults();
    if !defaults.objectIsForcedForKey(&key) {
        return None;
    }
    defaults.stringForKey(&key).map(|value| value.to_string())
}

#[cfg(not(target_os = "macos"))]
fn forced_string(_key: &str) -> Option<String> {
    None
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn a_support_line_is_one_bounded_line() {
        assert_eq!(
            normalize_support_contact("  IT desk ").as_deref(),
            Some("IT desk")
        );
        assert_eq!(normalize_support_contact("   "), None);
        assert_eq!(normalize_support_contact("IT\ndesk"), None);
        assert_eq!(normalize_support_contact("IT\u{202E}ksed"), None);
        let max = "é".repeat(MAX_SUPPORT_CONTACT_CHARS);
        assert_eq!(
            normalize_support_contact(&max).as_deref(),
            Some(max.as_str())
        );
        assert_eq!(normalize_support_contact(&format!("{max}é")), None);
    }

    #[test]
    fn managed_beats_the_workspace() {
        let policy = json!({"support_contact": "Workspace desk"});
        assert_eq!(
            effective_support_contact(Some("Managed desk"), Some(&policy)).as_deref(),
            Some("Managed desk")
        );
        assert_eq!(
            effective_support_contact(None, Some(&policy)).as_deref(),
            Some("Workspace desk")
        );
        assert_eq!(
            effective_support_contact(Some("two\nlines"), Some(&policy)).as_deref(),
            Some("Workspace desk")
        );
        assert_eq!(
            effective_support_contact(None, Some(&json!({"support_contact": 5}))),
            None
        );
        assert_eq!(effective_support_contact(None, None), None);
    }

    #[test]
    fn an_unmanaged_mac_has_no_managed_line() {
        // A test run is never under a configuration profile for the test binary's domain.
        assert_eq!(support_contact(), None);
    }
}
