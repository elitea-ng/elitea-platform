// Elitea patch 3 (libs/rust/vendor/README.md, "agent-desktop"): two reads
// the upstream crate has internally but does not export, which the Elitea
// driver needs for ADR-0047 decision 5 (secure fields and macOS secure
// input). Nothing here sends input.

use agent_desktop_core::{AdapterError, Deadline, ErrorCode, NativeHandle};

/// The focused element of the application `pid`, as a handle the adapter's
/// own calls take, or `None` when the app reports none.
pub fn focused_element(pid: u32, deadline: Deadline) -> Result<Option<NativeHandle>, AdapterError> {
    let pid = i32::try_from(pid)
        .map_err(|_| AdapterError::new(ErrorCode::InvalidArgs, "Process id is out of range"))?;
    let app = crate::tree::element_for_pid(pid);
    let result = crate::tree::properties::attributes::copy_element_attr_result(
        &app,
        "AXFocusedUIElement",
        deadline,
    );
    deadline.ensure_remaining()?;
    match result {
        Ok(element) => Ok(element.map(crate::tree::AXElement::into_native_handle)),
        Err(code) if code == accessibility_sys::kAXErrorAPIDisabled => {
            Err(AdapterError::permission_denied())
        }
        Err(code) => Err(AdapterError::new(
            ErrorCode::ActionFailed,
            format!("AXFocusedUIElement could not be read (AXError {code})"),
        )),
    }
}

#[link(name = "Carbon", kind = "framework")]
unsafe extern "C" {
    fn IsSecureEventInputEnabled() -> u8;
}

/// Whether macOS secure event input is on (a password prompt anywhere has
/// it), in which case synthetic keystrokes must not be sent.
pub fn secure_event_input_enabled() -> bool {
    unsafe { IsSecureEventInputEnabled() != 0 }
}

// Elitea patch 3, continued: the screen state ADR-0047 decision 5 needs
// ("no call runs while the screen is locked, the screensaver is up or the
// display sleeps"). Reads only.

/// What the window server reports about this login session and the main
/// display.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ScreenState {
    /// `CGSessionCopyCurrentDictionary` returned a dictionary: the process
    /// runs inside a window-server session.
    pub session_present: bool,
    /// `kCGSSessionOnConsoleKey`: the session owns the console (false while
    /// another user is switched in).
    pub on_console: bool,
    /// `CGSSessionScreenIsLocked`: the lock screen is up.
    pub locked: bool,
    /// `CGDisplayIsAsleep(CGMainDisplayID())`.
    pub display_asleep: bool,
}

#[link(name = "CoreGraphics", kind = "framework")]
unsafe extern "C" {
    fn CGSessionCopyCurrentDictionary() -> core_foundation_sys::dictionary::CFDictionaryRef;
    fn CGMainDisplayID() -> u32;
    fn CGDisplayIsAsleep(display: u32) -> u32;
}

/// The session and display state, read live.
pub fn screen_state() -> ScreenState {
    use core_foundation::base::{CFType, TCFType};
    use core_foundation::boolean::CFBoolean;
    use core_foundation::dictionary::CFDictionary;
    use core_foundation::number::CFNumber;
    use core_foundation::string::CFString;

    let display_asleep = unsafe { CGDisplayIsAsleep(CGMainDisplayID()) != 0 };
    let raw = unsafe { CGSessionCopyCurrentDictionary() };
    if raw.is_null() {
        return ScreenState {
            session_present: false,
            on_console: false,
            locked: false,
            display_asleep,
        };
    }
    // The dictionary follows the Create rule: this wrapper releases it.
    let dict: CFDictionary<CFString, CFType> = unsafe { CFDictionary::wrap_under_create_rule(raw) };
    let flag = |key: &'static str| -> Option<bool> {
        let value = dict.find(CFString::from_static_string(key))?;
        if let Some(b) = value.downcast::<CFBoolean>() {
            return Some(bool::from(b));
        }
        value
            .downcast::<CFNumber>()
            .and_then(|n| n.to_i64())
            .map(|n| n != 0)
    };
    ScreenState {
        session_present: true,
        on_console: flag("kCGSSessionOnConsoleKey").unwrap_or(false),
        locked: flag("CGSSessionScreenIsLocked").unwrap_or(false),
        display_asleep,
    }
}
