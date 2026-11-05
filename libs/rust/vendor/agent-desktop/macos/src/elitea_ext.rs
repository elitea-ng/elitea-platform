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
