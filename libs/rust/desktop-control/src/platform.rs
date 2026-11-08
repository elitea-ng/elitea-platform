//! The per-OS hard limits of ADR-0047 decisions 5 and 10, as pure rules, so
//! every one of them is unit-tested on every OS whichever driver this build
//! has.
//!
//! - **Denied chords.** The chords each OS's driver never sends. macOS has
//!   its list in [`crate::is_denied_chord`]; Windows and Linux have theirs
//!   here ([`is_denied_chord_on`]). A chord is denied when its key matches
//!   an entry and it holds at least that entry's modifiers, as on macOS.
//! - **Denied menu items.** [`crate::is_denied_menu_item`] holds on every OS,
//!   with the Windows and Linux titles added (`Exit`, `Sign out`, `Power
//!   off`, `Suspend`, `Hibernate`, `Log off`, `Reboot`).
//! - **Secure fields.** [`is_secure_field`]: a node is secure when the
//!   adapter says so (the vendor's `secure` state: UI Automation
//!   `IsPassword`, AT-SPI `password text`), or its role is one of the
//!   password roles. The value is dropped and no text is written, on every
//!   OS.
//! - **Application ids.** Windows has no bundle id. Its driver names an
//!   application by its executable image (`notepad.exe`), lower-cased:
//!   [`windows_app_id`]. Linux names it by its desktop entry id
//!   (`org.gnome.TextEditor`), or by its executable when it has none:
//!   [`linux_app_id`]. Both are checked by [`valid_app_id`] before a launch.
//! - **The platform deny list.** The applications of ADR-0047 decision 3 by
//!   their Windows and Linux ids ([`PLATFORM_APP_DENY_WINDOWS`],
//!   [`PLATFORM_APP_DENY_LINUX`]); [`is_denied_app`] matches them as the
//!   policy matches bundle ids (exact, ASCII case ignored, or a `prefix.*`
//!   family).

use crate::{KeyChord, Modifier};

/// The OS a rule is for.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Os {
    MacOs,
    Windows,
    Linux,
}

impl Os {
    /// The OS this build runs on (`None` on any other).
    #[must_use]
    pub const fn current() -> Option<Self> {
        if cfg!(target_os = "macos") {
            Some(Self::MacOs)
        } else if cfg!(target_os = "windows") {
            Some(Self::Windows)
        } else if cfg!(target_os = "linux") {
            Some(Self::Linux)
        } else {
            None
        }
    }
}

/// The Windows chords never sent. `Command` is the Windows key here (the
/// chord vocabulary's `cmd`/`meta`), `Option` is Alt.
const DENIED_WINDOWS: &[(&[Modifier], &str)] = &[
    // Close the app, and the shell's "Shut Down Windows" dialog on the desktop.
    (&[Modifier::Option], "f4"),
    // The secure attention sequence and Task Manager.
    (&[Modifier::Control, Modifier::Option], "delete"),
    (&[Modifier::Control, Modifier::Option], "end"),
    (&[Modifier::Control, Modifier::Shift], "escape"),
    // Lock, the power-user menu (shut down, sign out), Run, the Start menu
    // search, Settings, Explorer, show desktop.
    (&[Modifier::Command], "l"),
    (&[Modifier::Command], "x"),
    (&[Modifier::Command], "r"),
    (&[Modifier::Command], "s"),
    (&[Modifier::Command], "i"),
    (&[Modifier::Command], "e"),
    (&[Modifier::Command], "d"),
    // The Windows key alone opens Start (and with it, the power menu).
    (&[], "win"),
    (&[], "lwin"),
    (&[], "rwin"),
    (&[], "meta"),
    (&[], "super"),
    // Switching apps moves input out of the approved one.
    (&[Modifier::Option], "tab"),
    (&[Modifier::Command], "tab"),
    // The power keys.
    (&[], "power"),
    (&[], "sleep"),
];

/// The Linux chords never sent, across GNOME, KDE and the X server itself.
/// `Command` is the Super key.
const DENIED_LINUX: &[(&[Modifier], &str)] = &[
    // Close the window, and log out (GNOME and KDE).
    (&[Modifier::Option], "f4"),
    (&[Modifier::Control, Modifier::Option], "delete"),
    (&[Modifier::Control, Modifier::Option], "end"),
    // Kill the X server, and the kill-window chord.
    (&[Modifier::Control, Modifier::Option], "backspace"),
    (&[Modifier::Control, Modifier::Option], "escape"),
    // Lock the screen (GNOME, KDE, Cinnamon, Xfce).
    (&[Modifier::Command], "l"),
    (&[Modifier::Control, Modifier::Option], "l"),
    // A terminal (Ubuntu, Xfce), the run dialog (KDE, GNOME's Alt+F2).
    (&[Modifier::Control, Modifier::Option], "t"),
    (&[Modifier::Option], "f2"),
    (&[Modifier::Option], "space"),
    // The overview, the application launcher, the system menu.
    (&[], "super"),
    (&[], "win"),
    (&[], "meta"),
    (&[Modifier::Command], "a"),
    (&[Modifier::Command], "d"),
    (&[Modifier::Command], "space"),
    // Switching apps moves input out of the approved one.
    (&[Modifier::Option], "tab"),
    (&[Modifier::Command], "tab"),
    // The power keys.
    (&[], "power"),
    (&[], "poweroff"),
    (&[], "sleep"),
    (&[], "suspend"),
];

/// Key names that mean the same key.
fn canonical_key(key: &str) -> &str {
    match key {
        "esc" => "escape",
        "del" => "delete",
        " " | "spacebar" => "space",
        "windows" | "cmd" | "command" => "win",
        other => other,
    }
}

/// Whether the driver of `os` refuses to send `chord` (`chord_refused`).
/// On macOS this is [`crate::is_denied_chord`].
#[must_use]
pub fn is_denied_chord_on(os: Os, chord: &KeyChord) -> bool {
    let table = match os {
        Os::MacOs => return crate::is_denied_chord(chord),
        Os::Windows => DENIED_WINDOWS,
        Os::Linux => DENIED_LINUX,
    };
    let lower = chord.key.to_ascii_lowercase();
    let key = canonical_key(&lower);
    let held = |mods: &[Modifier]| mods.iter().all(|m| chord.modifiers.contains(m));
    table
        .iter()
        .any(|(mods, denied_key)| *denied_key == key && held(mods))
        || (os == Os::Linux && held(&[Modifier::Control, Modifier::Option]) && is_function_key(key))
}

/// `f1` to `f12`: Control+Alt with one of them switches the Linux virtual
/// terminal, which leaves the session.
fn is_function_key(key: &str) -> bool {
    key.strip_prefix('f')
        .and_then(|n| n.parse::<u8>().ok())
        .is_some_and(|n| (1..=12).contains(&n))
}

/// The roles a password field has, as the vendor adapters name them (the
/// macOS subrole, the AT-SPI role name, a UI Automation edit marked as a
/// password reports the `secure` state instead).
const SECURE_ROLES: &[&str] = &[
    "securetextfield",
    "passwordtext",
    "password text",
    "password",
];

/// The secure-field rule of ADR-0047 decision 5, on every OS: a node is
/// secure when the adapter reports the `secure` state, or its role is a
/// password role. A secure node never carries a value and is never typed
/// into.
#[must_use]
pub fn is_secure_field(role: &str, states: &[String]) -> bool {
    states.iter().any(|s| s.eq_ignore_ascii_case("secure"))
        || SECURE_ROLES
            .iter()
            .any(|r| role.trim().eq_ignore_ascii_case(r))
}

/// The application id of a Windows process: its executable image name,
/// without the directory, lower-cased (`C:\Windows\notepad.exe` and
/// `Notepad.EXE` are both `notepad.exe`).
#[must_use]
pub fn windows_app_id(image: &str) -> Option<String> {
    let base = image.rsplit(['\\', '/']).next().unwrap_or(image).trim();
    (!base.is_empty()).then(|| base.to_ascii_lowercase())
}

/// The application id of a Linux process: its desktop entry id without the
/// `.desktop` suffix when it has one, else its executable name. `None` when
/// it has neither.
#[must_use]
pub fn linux_app_id(desktop_id: Option<&str>, executable: &str) -> Option<String> {
    if let Some(id) = desktop_id
        .map(|id| id.strip_suffix(".desktop").unwrap_or(id).trim())
        .filter(|id| !id.is_empty())
    {
        return Some(id.to_owned());
    }
    let base = executable.rsplit('/').next().unwrap_or(executable).trim();
    (!base.is_empty()).then(|| base.to_owned())
}

/// Whether `id` names an application `open_app` may launch on `os`. Never
/// a path, a command line or an argument: on macOS a bundle id; on Windows
/// an image name ending in `.exe` (resolved by the vendor in the system
/// directories only); on Linux a desktop entry id or a bare command name.
#[must_use]
pub fn valid_app_id(os: Os, id: &str) -> bool {
    let plain = |c: char| c.is_ascii_alphanumeric() || matches!(c, '.' | '-' | '_');
    if id.is_empty() || id.len() > 255 || !id.chars().all(plain) || id.contains("..") {
        return false;
    }
    if id.starts_with(['.', '-']) || id.ends_with('.') {
        return false;
    }
    match os {
        Os::MacOs => id.contains('.'),
        Os::Windows => id
            .len()
            .checked_sub(4)
            .and_then(|at| id.get(at..))
            .is_some_and(|ext| ext.eq_ignore_ascii_case(".exe")),
        Os::Linux => true,
    }
}

/// The applications of ADR-0047 decision 3 by their Windows ids (the
/// executable image, [`windows_app_id`]): password managers, terminals and
/// shells, script hosts, the sign-in and credential prompts, and the
/// security pages of Settings.
pub const PLATFORM_APP_DENY_WINDOWS: &[&str] = &[
    // Elitea itself.
    "elitea.exe",
    "elitea-desktop.exe",
    // Password managers and credential stores.
    "1password.exe",
    "bitwarden.exe",
    "keepass.exe",
    "keepassxc.exe",
    "lastpass.exe",
    "enpass.exe",
    "proton pass.exe",
    "protonpass.exe",
    "credentialuibroker.exe",
    // Terminals and shells: the shell has its own sandboxed tool.
    "cmd.exe",
    "powershell.exe",
    "pwsh.exe",
    "powershell_ise.exe",
    "windowsterminal.exe",
    "wt.exe",
    "openconsole.exe",
    "conhost.exe",
    "wsl.exe",
    "wslhost.exe",
    "bash.exe",
    "mintty.exe",
    "alacritty.exe",
    "wezterm-gui.exe",
    "hyper.exe",
    "tabby.exe",
    "warp.exe",
    // Script hosts and the Run box's targets.
    "wscript.exe",
    "cscript.exe",
    "mshta.exe",
    "regedit.exe",
    "mmc.exe",
    "taskmgr.exe",
    // Sign-in, the secure desktop's prompts and UAC.
    "logonui.exe",
    "consent.exe",
    "lockapp.exe",
    "winlogon.exe",
    // Settings (its security pages cannot be told apart from the rest) and
    // Windows Security.
    "systemsettings.exe",
    "securityhealthui.exe",
    "securityhealthhost.exe",
];

/// The applications of ADR-0047 decision 3 by their Linux ids (the
/// desktop entry id, else the executable, [`linux_app_id`]).
pub const PLATFORM_APP_DENY_LINUX: &[&str] = &[
    // Elitea itself.
    "ai.elitea.desktop",
    "elitea",
    "elitea-desktop",
    // Password managers and credential stores.
    "1password",
    "com.1password.*",
    "bitwarden",
    "com.bitwarden.desktop",
    "org.keepassxc.KeePassXC",
    "keepassxc",
    "org.gnome.seahorse.Application",
    "seahorse",
    "org.kde.kwalletmanager5",
    "org.kde.kwalletmanager",
    "kwalletmanager5",
    "me.proton.Pass",
    "proton-pass",
    "enpass",
    "org.gnome.World.Secrets",
    "secrets",
    // Terminals: the shell has its own sandboxed tool.
    "org.gnome.Terminal",
    "gnome-terminal",
    "gnome-terminal-server",
    "org.gnome.Console",
    "kgx",
    "org.gnome.Ptyxis",
    "ptyxis",
    "org.kde.konsole",
    "konsole",
    "org.kde.yakuake",
    "xfce4-terminal",
    "xterm",
    "uxterm",
    "lxterminal",
    "mate-terminal",
    "terminator",
    "tilix",
    "com.gexperts.Tilix",
    "Alacritty",
    "alacritty",
    "kitty",
    "foot",
    "org.wezfurlong.wezterm",
    "wezterm-gui",
    "com.mitchellh.ghostty",
    "ghostty",
    "dev.warp.*",
    "warp-terminal",
    "hyper",
    "tabby",
    "urxvt",
    "st",
    // Privilege prompts, the lock screen and the greeter.
    "polkit-gnome-authentication-agent-1",
    "polkit-kde-authentication-agent-1",
    "lxpolkit",
    "pinentry",
    "pinentry-gnome3",
    "pinentry-qt",
    "gcr-prompter",
    "gnome-screensaver",
    "xscreensaver",
    "light-locker",
    "kscreenlocker_greet",
    "gdm",
    "lightdm",
    "sddm",
    // The settings apps, whole: their privacy, user-account, password and
    // sharing pages run in the one process and the one window, under the
    // app's own id, so no single page can be told apart (GNOME Settings
    // by its desktop id and its executable; KDE System Settings likewise).
    "org.gnome.Settings",
    "gnome-control-center",
    "org.kde.systemsettings",
    "systemsettings",
    "systemsettings5",
];

/// Whether `pattern` matches the application id `id`: equal ignoring ASCII
/// case, or under a `prefix.*` family (one component more, at least), the
/// rule the policy uses for bundle ids.
#[must_use]
pub fn app_id_matches(pattern: &str, id: &str) -> bool {
    match pattern.strip_suffix(".*") {
        Some(prefix) => {
            id.len() > prefix.len() + 1
                && id.as_bytes()[prefix.len()] == b'.'
                && id
                    .get(..prefix.len())
                    .is_some_and(|head| head.eq_ignore_ascii_case(prefix))
        }
        None => id.eq_ignore_ascii_case(pattern),
    }
}

/// Whether the platform deny list of `os` names the application `id`. On
/// macOS the list is the policy's (`elitea_local_tools::PLATFORM_APP_DENY`,
/// applied by the caller); this returns `false` there.
#[must_use]
pub fn is_denied_app(os: Os, id: &str) -> bool {
    let list = match os {
        Os::MacOs => return false,
        Os::Windows => PLATFORM_APP_DENY_WINDOWS,
        Os::Linux => PLATFORM_APP_DENY_LINUX,
    };
    list.iter().any(|pattern| app_id_matches(pattern, id))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn chord(text: &str) -> KeyChord {
        KeyChord::parse(text).unwrap()
    }

    #[test]
    fn windows_close_lock_sas_run_and_switch_chords_are_denied() {
        for c in [
            "alt+f4",
            "ctrl+alt+delete",
            "ctrl+alt+del",
            "ctrl+shift+esc",
            "cmd+l",
            "meta+x",
            "cmd+r",
            "cmd+shift+r",
            "alt+tab",
            "shift+alt+tab",
            "win",
            "cmd+d",
            "ctrl+alt+end",
        ] {
            assert!(
                is_denied_chord_on(Os::Windows, &chord(c)),
                "{c} must be denied"
            );
        }
    }

    #[test]
    fn ordinary_windows_chords_pass() {
        for c in [
            "ctrl+c", "ctrl+v", "ctrl+s", "f4", "alt+f", "return", "ctrl+w", "l",
        ] {
            assert!(!is_denied_chord_on(Os::Windows, &chord(c)), "{c} must pass");
        }
    }

    #[test]
    fn linux_logout_lock_vt_and_kill_chords_are_denied() {
        for c in [
            "alt+f4",
            "ctrl+alt+delete",
            "ctrl+alt+backspace",
            "ctrl+alt+l",
            "cmd+l",
            "ctrl+alt+t",
            "alt+f2",
            "ctrl+alt+f1",
            "ctrl+alt+f7",
            "ctrl+alt+shift+f3",
            "super",
            "alt+tab",
        ] {
            assert!(
                is_denied_chord_on(Os::Linux, &chord(c)),
                "{c} must be denied"
            );
        }
    }

    #[test]
    fn ordinary_linux_chords_pass() {
        for c in [
            "ctrl+c",
            "ctrl+shift+v",
            "ctrl+l",
            "f2",
            "ctrl+f1",
            "alt+f",
            "ctrl+alt+f13",
            "return",
        ] {
            assert!(!is_denied_chord_on(Os::Linux, &chord(c)), "{c} must pass");
        }
    }

    #[test]
    fn macos_keeps_its_own_list() {
        assert!(is_denied_chord_on(Os::MacOs, &chord("cmd+q")));
        assert!(!is_denied_chord_on(Os::MacOs, &chord("alt+f4")));
        assert!(!is_denied_chord_on(Os::Windows, &chord("cmd+q")));
    }

    #[test]
    fn windows_and_linux_power_menu_items_are_denied() {
        for title in [
            "Exit",
            "E&xit",
            "Sign out",
            "Shut down",
            "Power Off\u{2026}",
            "Suspend",
            "Hibernate",
            "Lock Screen",
            "Shut down or sign out",
            "Log Off",
            "Log Out\u{2026}",
            "Quit",
        ] {
            assert!(
                crate::is_denied_menu_item("menuitem", Some(title)),
                "{title}"
            );
        }
        for title in [
            "Exit full screen",
            "Export",
            "Lockdown mode docs",
            "Locked cells",
        ] {
            assert!(
                !crate::is_denied_menu_item("menuitem", Some(title)),
                "{title}"
            );
        }
    }

    #[test]
    fn secure_fields_are_secure_on_every_os() {
        let states = |s: &[&str]| s.iter().map(|&s| s.to_owned()).collect::<Vec<_>>();
        // UI Automation `IsPassword`, AT-SPI `password text` and the macOS
        // subrole all reach the driver as the `secure` state.
        assert!(is_secure_field("textfield", &states(&["secure"])));
        assert!(is_secure_field(
            "textfield",
            &states(&["focused", "SECURE"])
        ));
        // A password role is secure even when the state was not read.
        assert!(is_secure_field("password text", &[]));
        assert!(is_secure_field("PasswordText", &[]));
        assert!(is_secure_field("securetextfield", &[]));
        assert!(!is_secure_field("textfield", &states(&["focused"])));
        assert!(!is_secure_field("statictext", &[]));
    }

    #[test]
    fn app_ids_come_from_the_image_or_the_desktop_entry() {
        assert_eq!(
            windows_app_id(r"C:\Windows\System32\Notepad.EXE").as_deref(),
            Some("notepad.exe")
        );
        assert_eq!(
            windows_app_id("WindowsTerminal.exe").as_deref(),
            Some("windowsterminal.exe")
        );
        assert_eq!(windows_app_id("  "), None);
        assert_eq!(
            linux_app_id(
                Some("org.gnome.TextEditor.desktop"),
                "/usr/bin/gnome-text-editor"
            )
            .as_deref(),
            Some("org.gnome.TextEditor")
        );
        assert_eq!(
            linux_app_id(None, "/usr/bin/gedit").as_deref(),
            Some("gedit")
        );
        assert_eq!(linux_app_id(Some(""), "xterm").as_deref(), Some("xterm"));
        assert_eq!(linux_app_id(None, ""), None);
    }

    #[test]
    fn only_plain_app_ids_launch() {
        assert!(valid_app_id(Os::MacOs, "com.apple.TextEdit"));
        assert!(!valid_app_id(Os::MacOs, "TextEdit"));
        assert!(valid_app_id(Os::Windows, "notepad.exe"));
        assert!(valid_app_id(Os::Windows, "Notepad.EXE"));
        assert!(valid_app_id(Os::Linux, "org.gnome.TextEditor"));
        assert!(valid_app_id(Os::Linux, "gedit"));
        for bad in [
            "",
            r"C:\Windows\notepad.exe",
            "/usr/bin/gedit",
            "../evil",
            "notepad.exe --flag",
            "-rf",
            "cmd.exe&calc",
            ".hidden",
            "a..b.exe",
        ] {
            for os in [Os::MacOs, Os::Windows, Os::Linux] {
                assert!(!valid_app_id(os, bad), "{os:?} {bad:?}");
            }
        }
        assert!(!valid_app_id(Os::Windows, "notepad"));
    }

    #[test]
    fn the_platform_deny_lists_cover_the_adr_list() {
        for app in [
            "elitea.exe",
            "1password.exe",
            "KeePassXC.exe",
            "cmd.exe",
            "PowerShell.exe",
            "pwsh.exe",
            "WindowsTerminal.exe",
            "wsl.exe",
            "logonui.exe",
            "consent.exe",
            "SystemSettings.exe",
        ] {
            assert!(is_denied_app(Os::Windows, app), "{app} must be denied");
        }
        for app in [
            "ai.elitea.desktop",
            "com.1password.1password",
            "org.keepassxc.KeePassXC",
            "org.gnome.seahorse.Application",
            "org.gnome.Terminal",
            "org.kde.konsole",
            "kitty",
            "xterm",
            "dev.warp.Warp",
            "pinentry-gnome3",
            "polkit-gnome-authentication-agent-1",
        ] {
            assert!(is_denied_app(Os::Linux, app), "{app} must be denied");
        }
        for (os, app) in [
            (Os::Windows, "notepad.exe"),
            (Os::Windows, "winword.exe"),
            (Os::Linux, "org.gnome.TextEditor"),
            (Os::Linux, "firefox"),
            (Os::Linux, "dev.warp"),
            (Os::MacOs, "com.apple.Terminal"),
        ] {
            assert!(!is_denied_app(os, app), "{os:?} {app} must pass");
        }
    }

    #[test]
    fn the_linux_settings_apps_are_denied_whole() {
        // The ids AT-SPI and the desktop entries actually report: GNOME
        // Settings (desktop id, then its executable when it has no entry)
        // and KDE System Settings. Their privacy and account pages are
        // pages of these apps, never apps of their own.
        for app in [
            "org.gnome.Settings",
            "gnome-control-center",
            "org.kde.systemsettings",
            "systemsettings",
            "systemsettings5",
            "ORG.GNOME.SETTINGS",
        ] {
            assert!(is_denied_app(Os::Linux, app), "{app} must be denied");
        }
        // The panel ids that never appear as an application id are gone
        // from the list: an entry that cannot match only reads as coverage.
        for never in [
            "gnome-privacy-panel",
            "gnome-user-accounts-panel",
            "kcm_users",
        ] {
            assert!(
                !PLATFORM_APP_DENY_LINUX.contains(&never),
                "{never} never matches an app id"
            );
        }
        // Lookalikes stay allowed.
        for app in ["org.gnome.SettingsDaemon", "org.gnome.Settings2"] {
            assert!(!is_denied_app(Os::Linux, app), "{app} must pass");
        }
    }

    #[test]
    fn a_family_needs_one_component_more() {
        assert!(app_id_matches("com.1password.*", "com.1password.1password"));
        assert!(!app_id_matches("com.1password.*", "com.1password"));
        assert!(!app_id_matches("com.1password.*", "com.1passwordx.app"));
        assert!(app_id_matches("cmd.exe", "CMD.EXE"));
    }
}
