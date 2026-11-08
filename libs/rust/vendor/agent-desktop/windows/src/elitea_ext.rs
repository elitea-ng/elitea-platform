// Elitea patch (libs/rust/vendor/README.md, "agent-desktop", windows change
// 1): the screen state ADR-0047 decision 5 needs ("no call runs while the
// screen is locked, the screensaver is up, a secure desktop is shown or the
// session is not on the console"). Reads only; nothing here sends input.
// Every Win32 call these reads need is an `unsafe fn` in windows-sys, and
// the Elitea driver crate forbids `unsafe`, so they live here.

/// What Windows reports about the input desktop and this process's session.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ScreenState {
    /// This process belongs to a session (`ProcessIdToSessionId` answered).
    pub session_present: bool,
    /// That session is the console session, and it is connected
    /// (`WTSGetActiveConsoleSessionId`, `WTSConnectState == WTSActive`).
    pub on_console: bool,
    /// The input desktop is not `Default`: the lock screen, the sign-in
    /// screen, or the secure desktop of a UAC or credential prompt (or the
    /// desktop could not be opened at all, which is what a locked
    /// workstation does to a normal process).
    pub locked: bool,
    /// `SPI_GETSCREENSAVERRUNNING`.
    pub screensaver: bool,
}

#[cfg(target_os = "windows")]
mod imp {
    use super::ScreenState;
    use windows_sys::Win32::System::RemoteDesktop::{
        ProcessIdToSessionId, WTS_CONNECTSTATE_CLASS, WTS_CURRENT_SERVER_HANDLE, WTSActive,
        WTSConnectState, WTSFreeMemory, WTSGetActiveConsoleSessionId, WTSQuerySessionInformationW,
    };
    use windows_sys::Win32::System::StationsAndDesktops::{
        CloseDesktop, DESKTOP_READOBJECTS, GetUserObjectInformationW, OpenInputDesktop, UOI_NAME,
    };
    use windows_sys::Win32::System::Threading::GetCurrentProcessId;
    use windows_sys::Win32::UI::WindowsAndMessaging::{
        GetForegroundWindow, GetWindowThreadProcessId, SPI_GETSCREENSAVERRUNNING,
        SystemParametersInfoW,
    };

    fn session_id() -> Option<u32> {
        let mut session = 0u32;
        // SAFETY: an out-pointer to a live local.
        let ok = unsafe { ProcessIdToSessionId(GetCurrentProcessId(), &mut session) };
        (ok != 0).then_some(session)
    }

    fn connected(session: u32) -> bool {
        let mut buffer: *mut u16 = std::ptr::null_mut();
        let mut bytes = 0u32;
        // SAFETY: out-pointers to live locals; the buffer is freed below.
        let ok = unsafe {
            WTSQuerySessionInformationW(
                WTS_CURRENT_SERVER_HANDLE,
                session,
                WTSConnectState,
                &mut buffer,
                &mut bytes,
            )
        };
        if ok == 0 || buffer.is_null() {
            return false;
        }
        let state = if bytes as usize >= std::mem::size_of::<WTS_CONNECTSTATE_CLASS>() {
            // SAFETY: the API returned at least one WTS_CONNECTSTATE_CLASS.
            unsafe { std::ptr::read_unaligned(buffer.cast::<WTS_CONNECTSTATE_CLASS>()) }
        } else {
            -1
        };
        // SAFETY: the buffer came from WTSQuerySessionInformationW.
        unsafe { WTSFreeMemory(buffer.cast()) };
        state == WTSActive
    }

    /// The name of the input desktop, or `None` when it cannot be opened.
    fn input_desktop_name() -> Option<String> {
        // SAFETY: no pointers; the handle is closed below.
        let desktop = unsafe { OpenInputDesktop(0, 0, DESKTOP_READOBJECTS) };
        if desktop.is_null() {
            return None;
        }
        let mut name = [0u16; 256];
        let mut needed = 0u32;
        // SAFETY: the buffer and its length in bytes are a live local.
        let ok = unsafe {
            GetUserObjectInformationW(
                desktop,
                UOI_NAME,
                name.as_mut_ptr().cast(),
                (name.len() * 2) as u32,
                &mut needed,
            )
        };
        // SAFETY: the handle came from OpenInputDesktop.
        unsafe { CloseDesktop(desktop) };
        if ok == 0 {
            return None;
        }
        let end = name.iter().position(|&c| c == 0).unwrap_or(name.len());
        Some(String::from_utf16_lossy(&name[..end]))
    }

    fn screensaver_running() -> bool {
        let mut running = 0i32;
        // SAFETY: SPI_GETSCREENSAVERRUNNING writes one BOOL to the pointer.
        let ok = unsafe {
            SystemParametersInfoW(
                SPI_GETSCREENSAVERRUNNING,
                0,
                (&mut running as *mut i32).cast(),
                0,
            )
        };
        ok != 0 && running != 0
    }

    pub(super) fn screen_state() -> ScreenState {
        let session = session_id();
        // SAFETY: no arguments.
        let console = unsafe { WTSGetActiveConsoleSessionId() };
        let on_console = session.is_some_and(|s| s == console && connected(s));
        let locked = !input_desktop_name().is_some_and(|n| n.eq_ignore_ascii_case("Default"));
        ScreenState {
            session_present: session.is_some(),
            on_console,
            locked,
            screensaver: screensaver_running(),
        }
    }

    pub(super) fn foreground_pid() -> Option<u32> {
        // SAFETY: no arguments.
        let window = unsafe { GetForegroundWindow() };
        if window.is_null() {
            return None;
        }
        let mut pid = 0u32;
        // SAFETY: an out-pointer to a live local.
        unsafe { GetWindowThreadProcessId(window, &mut pid) };
        (pid != 0).then_some(pid)
    }
}

#[cfg(not(target_os = "windows"))]
mod imp {
    use super::ScreenState;

    pub(super) fn screen_state() -> ScreenState {
        ScreenState {
            session_present: false,
            on_console: false,
            locked: false,
            screensaver: false,
        }
    }

    pub(super) fn foreground_pid() -> Option<u32> {
        None
    }
}

/// The session and input-desktop state, read live.
pub fn screen_state() -> ScreenState {
    imp::screen_state()
}

/// The process that owns the foreground window, when there is one.
pub fn foreground_pid() -> Option<u32> {
    imp::foreground_pid()
}
