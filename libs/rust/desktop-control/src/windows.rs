//! The Windows driver (ADR-0047 decision 10): UI Automation through the
//! vendored agent-desktop `windows` adapter, behind the shared engine
//! (`engine.rs`).
//!
//! - **Application ids** are executable images (`notepad.exe`,
//!   [`crate::platform::windows_app_id`]); `open_app` takes one and the
//!   vendor resolves a bare name in the system directories only.
//! - **Windows the owner does not name** ([`check_window`]): every
//!   packaged app (Settings, Windows Security) is framed by
//!   `applicationframehost.exe`, and the shell, `explorer.exe`, owns the Run
//!   box and the Control Panel as well as File Explorer. A frame is judged
//!   by the app it hosts (refused when that cannot be read); of the shell's
//!   windows only File Explorer folder windows on a file-system path pass,
//!   and those are read and clicked but never typed into (its address bar
//!   runs what is typed; [`crate::platform::text_entry_refusal`]).
//! - **The screen** ([`screen`]): no session, another session on the
//!   console, the lock or sign-in screen or a UAC/credential secure desktop
//!   (the input desktop is not `Default`), or the screensaver is
//!   `screen_unavailable`.
//! - **COM.** The adapter's UI Automation client needs the process-wide
//!   multithreaded apartment; [`WindowsDriver::new`] joins it once
//!   (`ensure_hosted_library_mta_and_dpi`, the vendor's library-host
//!   bootstrap). A failure there leaves every call failing as the adapter
//!   reports it.

use agent_desktop_core as ad;

use crate::engine::{NativeDriver, OsHooks};
use crate::platform::{self, Os};
use crate::{ScreenStatus, SyntheticMarker};

/// The marker the Windows driver reports. `SendInput` cannot carry it: the
/// watch tells the driver's own events by the OS's own `LLKHF_INJECTED` /
/// `LLMHF_INJECTED` flag instead (`windows_input.rs`).
const WINDOWS_MARKER: i64 = 0x454C_4954_4541_5701;

/// The Windows [`crate::DesktopDriver`].
pub type WindowsDriver = NativeDriver;

impl NativeDriver {
    /// The Windows driver.
    #[must_use]
    pub fn new() -> Self {
        // A failure surfaces again on the first call, as the adapter's own
        // error (no COM apartment: UI Automation cannot be created).
        let _ = agent_desktop_windows::ensure_hosted_library_mta_and_dpi();
        Self::with_hooks(
            Box::new(agent_desktop_windows::WindowsAdapter::new()),
            OsHooks {
                os: Os::Windows,
                app_id,
                screen,
                marker: SyntheticMarker::new(WINDOWS_MARKER).unwrap_or_else(|| unreachable!()),
                watch: crate::windows_input::watch,
                check_window,
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
    platform::windows_app_id(&app.name)
}

/// The window rule beyond the owner's image
/// ([`platform::windows_window_verdict`]): a packaged app's frame is judged
/// by the app it hosts, and only Explorer's folder windows pass.
fn check_window(window: &ad::WindowInfo) -> Result<(), crate::DesktopError> {
    let facts = agent_desktop_windows::elitea_ext::window_facts(&window.id);
    match platform::windows_window_verdict(&platform::WindowsWindowFacts {
        owner_image: facts.owner_image.as_deref(),
        class: facts.class.as_deref(),
        hosted_image: facts.hosted_image.as_deref(),
        folder_location: facts.folder_location.as_deref(),
    }) {
        platform::WindowVerdict::Allow => Ok(()),
        platform::WindowVerdict::Deny(why) | platform::WindowVerdict::Unresolved(why) => {
            Err(crate::DesktopError::AppDenied(why))
        }
    }
}

fn screen() -> ScreenStatus {
    let state = agent_desktop_windows::elitea_ext::screen_state();
    ScreenStatus {
        session_present: state.session_present,
        on_console: state.on_console,
        locked: state.locked,
        screensaver: state.screensaver,
        display_asleep: false,
        lock_state_unknown: false,
    }
}

/// The process that owns the foreground window (the watch's target pid).
pub(crate) fn foreground_pid() -> u32 {
    agent_desktop_windows::elitea_ext::foreground_pid().unwrap_or(0)
}
