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

/// The keys that move, select, submit or delete but never put text in a
/// field. Everything else is taken to insert text ([`chord_inserts_text`]).
const NON_INSERTING_KEYS: &[&str] = &[
    "up",
    "down",
    "left",
    "right",
    "home",
    "end",
    "pageup",
    "pagedown",
    "tab",
    "escape",
    "return",
    "enter",
    "backspace",
    "delete",
    "insert",
    "menu",
    "apps",
    "contextmenu",
    "capslock",
    "numlock",
    "scrolllock",
    "printscreen",
    "pause",
];

/// The keys that, with Command (the Windows key on Windows), open a panel
/// that inserts what is picked in it: the emoji and symbol panel (`.`,
/// `;`, by character or by name) and the clipboard history (`v`).
const INSERTING_PANEL_KEYS: &[&str] = &[".", ";", "period", "semicolon", "v"];

/// Whether a chord may put text into the focused element: a character
/// (alone, with Shift, with Alt, or with Control+Alt, which is `AltGr` on
/// Windows), Space, paste (Control+V, Command+V, Shift+Insert, the `paste`
/// key), a panel that inserts what is picked in it (`INSERTING_PANEL_KEYS`
/// with Command, which is the Windows key there: Win+. and Win+; open the
/// emoji and symbol panel, Win+V the clipboard history) or any key this
/// list does not know. A Control or Command shortcut on any other
/// character, a function key and the navigation and editing keys do not
/// (#1586: with no focused element found, the first are refused and the
/// rest pass). It fails closed: an unknown key inserts.
#[must_use]
pub fn chord_inserts_text(chord: &KeyChord) -> bool {
    let lower = chord.key.to_ascii_lowercase();
    let key = canonical_key(&lower);
    let has = |m: Modifier| chord.modifiers.contains(&m);
    let shortcut = (has(Modifier::Control) || has(Modifier::Command))
        && !(has(Modifier::Control) && has(Modifier::Option));
    if key == "paste" || (key == "insert" && has(Modifier::Shift)) {
        return true;
    }
    if NON_INSERTING_KEYS.contains(&key) || is_any_function_key(key) {
        return false;
    }
    if has(Modifier::Command) && INSERTING_PANEL_KEYS.contains(&key) {
        return true;
    }
    if key.chars().count() == 1 && key != "space" {
        return !shortcut || key == "v";
    }
    // Space, a dead or composed key, a key name this list does not know.
    !shortcut
}

/// `f1` to `f24`.
fn is_any_function_key(key: &str) -> bool {
    key.strip_prefix('f')
        .and_then(|n| n.parse::<u8>().ok())
        .is_some_and(|n| (1..=24).contains(&n))
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
/// `.desktop` suffix when it has one, else its executable name
/// ([`linux_executable_name`]). `None` when it has neither, and never an
/// interpreter's name ([`is_interpreter_name`]).
#[must_use]
pub fn linux_app_id(desktop_id: Option<&str>, executable: &str) -> Option<String> {
    if let Some(id) = desktop_id
        .map(|id| id.strip_suffix(".desktop").unwrap_or(id).trim())
        .filter(|id| !id.is_empty())
    {
        return Some(id.to_owned());
    }
    let base = executable.rsplit('/').next().unwrap_or(executable).trim();
    (!base.is_empty() && !is_interpreter_name(base)).then(|| base.to_owned())
}

/// Why no call may name the application `id` at all, before anything is
/// asked: an interpreter's name ([`is_interpreter_name`]: `python3`,
/// `node`, `bash`) is never an application's id, so it is refused rather
/// than asked for, and "Always allow" can never be granted to it.
#[must_use]
pub fn unnameable_app(id: &str) -> Option<String> {
    is_interpreter_name(id).then(|| {
        format!(
            "`{}` is an interpreter, not an application: name the app by its desktop entry id or its script's name",
            id.trim()
        )
    })
}

/// The longest name the kernel keeps in `/proc/<pid>/comm`
/// (`TASK_COMM_LEN` - 1): a name this long may have been cut.
const COMM_MAX: usize = 15;

/// The interpreters and runtimes a Linux application can run under, by
/// their executable's name without a trailing version (`python3.12`,
/// `lua5.4`, `electron29`). A process of one of them is named by the script
/// it runs, never by the interpreter ([`linux_executable_name`]).
const LINUX_INTERPRETERS: &[&str] = &[
    "python",
    "pypy",
    "perl",
    "ruby",
    "node",
    "nodejs",
    "bun",
    "deno",
    "electron",
    "bash",
    "sh",
    "dash",
    "zsh",
    "fish",
    "ksh",
    "mksh",
    "ash",
    "csh",
    "tcsh",
    "busybox",
    "gjs",
    "gjs-console",
    "cjs",
    "seed",
    "java",
    "lua",
    "luajit",
    "php",
    "tclsh",
    "wish",
    "guile",
    "qjs",
    "osascript",
];

/// Whether `name` (an executable's basename) is an interpreter or runtime
/// (`LINUX_INTERPRETERS`), ignoring a version suffix and ASCII case. Such
/// a name is never an application id on Linux: it says nothing about the
/// app (Terminator and GNOME Secrets both run as `python3.12`).
#[must_use]
pub fn is_interpreter_name(name: &str) -> bool {
    let name = name.trim().to_ascii_lowercase();
    let stem = name.trim_end_matches(|c: char| c.is_ascii_digit() || c == '.');
    !stem.is_empty() && LINUX_INTERPRETERS.contains(&stem)
}

/// The interpreter options that take the next argument as their value, by
/// interpreter (the stem [`is_interpreter_name`] matches). Long options
/// written `--name=value` take nothing more.
fn option_takes_value(interpreter: &str, option: &str) -> bool {
    let list: &[&str] = match interpreter {
        "python" | "pypy" => &["-W", "-X", "--check-hash-based-pycs"],
        "perl" => &["-I", "-M", "-m"],
        "ruby" => &["-I", "-r", "-C", "-E", "--encoding"],
        "node" | "nodejs" | "bun" | "deno" | "electron" => &[
            "-r",
            "--require",
            "--import",
            "--loader",
            "--experimental-loader",
            "-C",
            "--conditions",
            "--title",
            "--inspect-port",
        ],
        "bash" | "sh" | "dash" | "zsh" | "ksh" | "mksh" | "ash" | "busybox" | "fish" => &[
            "-o",
            "-O",
            "+o",
            "+O",
            "--rcfile",
            "--init-file",
            "--init-command",
            "-C",
        ],
        "gjs" | "cjs" | "seed" => &["-I", "--include-path", "--coverage-prefix", "--profile"],
        "java" => &[
            "-cp",
            "-classpath",
            "--class-path",
            "-p",
            "--module-path",
            "--upgrade-module-path",
            "--add-modules",
            "--add-opens",
            "--add-exports",
            "--add-reads",
            "--patch-module",
            "--enable-native-access",
        ],
        "lua" | "luajit" => &["-l"],
        "php" => &["-c", "-d", "-z"],
        "tclsh" | "wish" => &["-encoding", "-name", "-display", "-geometry", "-class"],
        "guile" => &["-L", "-C", "-x", "-l", "--use-srfi"],
        _ => &[],
    };
    list.contains(&option)
}

/// What one interpreter option says about the program the process runs.
enum Opt {
    /// The program is inline code (`python -c`, `perl -e`): it has no name.
    Inline,
    /// The next argument is the program (`java -jar app.jar`, `php -f`,
    /// `gjs -m file`) or a module named instead of a file (`python -m pkg`,
    /// `java -m mod/Main`).
    Program { module: bool },
    /// The next argument is the option's value.
    Value,
    /// A flag on its own.
    Flag,
}

fn read_option(interpreter: &str, option: &str) -> Opt {
    let inline: &[&str] = match interpreter {
        "python" | "pypy" | "bash" | "sh" | "dash" | "zsh" | "ksh" | "mksh" | "ash" | "busybox"
        | "fish" | "gjs" | "cjs" | "qjs" | "csh" | "tcsh" => &["-c", "--command"],
        "perl" | "ruby" | "lua" | "luajit" | "guile" | "osascript" => &["-e", "-E", "-c"],
        "node" | "nodejs" | "bun" | "deno" | "electron" => &["-e", "--eval", "-p", "--print"],
        "php" => &["-r", "-R", "-B", "-E"],
        _ => &[],
    };
    if inline.contains(&option) {
        return Opt::Inline;
    }
    match (interpreter, option) {
        ("python" | "pypy", "-m") | ("java", "-m" | "--module") => Opt::Program { module: true },
        ("java", "-jar") | ("php", "-f") | ("gjs" | "cjs", "-m" | "--module") => {
            Opt::Program { module: false }
        }
        _ if option_takes_value(interpreter, option) => Opt::Value,
        _ => Opt::Flag,
    }
}

/// The stems of an entry point that say nothing about the app
/// (`/usr/lib/bitwarden/app.asar`, `/opt/foo/main.js`): the app is named by
/// the directory instead.
const GENERIC_ENTRY_STEMS: &[&str] = &[
    "app", "main", "index", "__main__", "run", "start", "launcher", "cli", "server", "entry",
];
/// Directories an entry point's name is never taken from.
const GENERIC_DIRS: &[&str] = &[
    "resources",
    "app",
    "lib",
    "lib64",
    "libexec",
    "share",
    "bin",
    "sbin",
    "dist",
    "src",
    "build",
    "out",
    "usr",
    "opt",
    "local",
    "node_modules",
    "site-packages",
    "dist-packages",
];
/// The script suffixes dropped from a name (`secrets.py` is `secrets`).
const SCRIPT_SUFFIXES: &[&str] = &[
    ".py", ".pyw", ".pyc", ".pl", ".rb", ".js", ".mjs", ".cjs", ".ts", ".lua", ".php", ".sh",
    ".bash", ".zsh", ".fish", ".tcl", ".jar", ".asar", ".scm",
];

/// The application name of the program `path` an interpreter runs: its
/// basename without a script suffix, or its directory's name when the
/// basename is generic. `None` when no usable name is left.
fn program_name(path: &str) -> Option<String> {
    let mut parts = path.trim().trim_end_matches('/').rsplit('/');
    let base = parts.next()?.trim();
    let strip = |name: &str| {
        let lower = name.to_ascii_lowercase();
        SCRIPT_SUFFIXES
            .iter()
            .find(|suffix| lower.ends_with(*suffix) && lower.len() > suffix.len())
            .map_or_else(
                || name.to_owned(),
                |suffix| name[..name.len() - suffix.len()].to_owned(),
            )
    };
    let stem = strip(base);
    let generic = |name: &str| GENERIC_ENTRY_STEMS.contains(&name.to_ascii_lowercase().as_str());
    let name = if stem.is_empty() || generic(&stem) {
        parts
            .map(str::trim)
            .find(|dir| {
                !dir.is_empty() && !GENERIC_DIRS.contains(&dir.to_ascii_lowercase().as_str())
            })
            .map(strip)?
    } else {
        stem
    };
    usable_name(&name)
}

/// A name that can be an application id: a plain id ([`valid_app_id`])
/// that is not an interpreter.
fn usable_name(name: &str) -> Option<String> {
    let name = name.trim();
    (valid_app_id(Os::Linux, name) && !is_interpreter_name(name)).then(|| name.to_owned())
}

/// The program an interpreter process runs, read from its command line
/// (`cmdline[0]` is the interpreter; the first argument that is not an
/// option, or the module `-m` names). `None` for inline code (`-c`, `-e`),
/// standard input (`-`) or no program at all.
fn script_of(interpreter: &str, cmdline: &[String]) -> Option<String> {
    let mut args = cmdline.iter().skip(1).map(String::as_str);
    while let Some(arg) = args.next() {
        if arg == "--" {
            return args.next().and_then(program_name);
        }
        if arg == "-" {
            return None;
        }
        if !(arg.starts_with('-') || (arg.starts_with('+') && arg.len() > 1)) {
            return program_name(arg);
        }
        if arg.starts_with("--") && arg.contains('=') {
            continue;
        }
        match read_option(interpreter, arg) {
            Opt::Inline => return None,
            Opt::Program { module: true } => {
                // `pkg.app`, `mod/pkg.Main`: the module as named.
                return args.next().and_then(|m| usable_name(m.rsplit('/').next()?));
            }
            Opt::Program { module: false } => return args.next().and_then(program_name),
            Opt::Value => {
                args.next();
            }
            Opt::Flag => {}
        }
    }
    None
}

/// The name a Linux application's id falls back to when it has no desktop
/// entry ([`linux_app_id`]), never its display name. Pure over what
/// `/proc/<pid>` says: `exe_link` is the target of `exe`, `comm` the
/// kernel's name of the process, `cmdline` its arguments.
///
/// - **A plain binary** is named by its executable (`xterm`).
/// - **An interpreter** ([`is_interpreter_name`]: `python3.12`, `node`,
///   `bash`, `java`, ...) is never the name: Terminator and GNOME Secrets
///   are both `python3.12`, and their deny entries must match. The app is
///   named by the script it runs (the first argument after the
///   interpreter's options, or the module of `-m`), and by `comm` when that
///   is not an interpreter (a `#!` script's `comm` is the script's name).
///   When both are read and disagree, or neither gives a name (inline code,
///   standard input), the process has no name.
/// - **No readable link**: `comm`, unless the kernel may have cut it (15
///   bytes: `gnome-terminal-` must not slip past `gnome-terminal-server`)
///   or it is an interpreter.
///
/// An application with no name has no id, and an application with no id
/// is never matched by a call (`app_not_found`, `not_frontmost`).
#[must_use]
pub fn linux_executable_name(
    exe_link: Option<&str>,
    comm: &str,
    cmdline: &[String],
) -> Option<String> {
    let base = |path: &str| {
        let name = path.rsplit('/').next().unwrap_or(path);
        name.strip_suffix(" (deleted)")
            .unwrap_or(name)
            .trim()
            .to_owned()
    };
    let comm = comm.trim();
    // A comm the kernel may have cut, or an interpreter's, names nothing.
    // `#!` scripts get the script's file name (`secrets`, `tool.py`).
    let comm_name = (comm.len() < COMM_MAX)
        .then(|| program_name(comm))
        .flatten();
    let exe = exe_link.map(base).filter(|n| !n.is_empty());
    let interpreter = match exe {
        Some(exe) if is_interpreter_name(&exe) => exe,
        Some(exe) => return Some(exe),
        // No link: comm is the name, unless it is an interpreter's.
        None if is_interpreter_name(comm) => comm.to_owned(),
        None => return comm_name,
    };
    let stem = interpreter
        .to_ascii_lowercase()
        .trim_end_matches(|c: char| c.is_ascii_digit() || c == '.')
        .to_owned();
    let stem = if stem == "gjs-console" {
        "gjs".to_owned()
    } else {
        stem
    };
    match (script_of(&stem, cmdline), comm_name) {
        (Some(script), Some(comm)) if !script.eq_ignore_ascii_case(&comm) => None,
        (Some(name), _) | (None, Some(name)) => Some(name),
        (None, None) => None,
    }
}

/// Whether `id` names an application `open_app` may launch on `os`. Never
/// a path, a command line or an argument: on macOS a bundle id; on Windows
/// an image name ending in `.exe` (resolved by the vendor in the system
/// directories only); on Linux a desktop entry id or a bare command name,
/// never an interpreter's ([`is_interpreter_name`]).
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
        // An interpreter names no application (`python3` is Terminator
        // and GNOME Secrets alike): never launched, never an id.
        Os::Linux => !is_interpreter_name(id),
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
    // Script hosts and the Run box's targets. `rundll32.exe` and
    // `control.exe` host the Control Panel pages that are not Explorer
    // windows (Credential Manager's and User Accounts' dialogs among them);
    // `mmc.exe` every management console snap-in.
    "wscript.exe",
    "cscript.exe",
    "mshta.exe",
    "rundll32.exe",
    "control.exe",
    "regedit.exe",
    "mmc.exe",
    "taskmgr.exe",
    // Sign-in, the secure desktop's prompts and UAC.
    "logonui.exe",
    "consent.exe",
    "lockapp.exe",
    "winlogon.exe",
    // Settings (its security pages cannot be told apart from the rest) and
    // Windows Security. Both are packaged apps framed by
    // `applicationframehost.exe`: [`windows_window_verdict`] names a framed
    // window by the app it hosts, so these entries match.
    "systemsettings.exe",
    "sechealthui.exe",
    "securityhealthui.exe",
    "securityhealthhost.exe",
];

/// The process that frames every packaged (UWP) app's window on Windows:
/// Settings, Windows Security and Calculator alike. Its own image says
/// nothing about the app inside.
pub const WINDOWS_FRAME_HOST: &str = "applicationframehost.exe";
/// The Windows shell. It owns File Explorer's folder windows, and also the
/// Run box, the Control Panel, Credential Manager, User Accounts, the
/// taskbar and the Start menu.
pub const WINDOWS_SHELL: &str = "explorer.exe";
/// The window class of a File Explorer folder window (the Control Panel is
/// shown in one too, so the class alone does not make a folder).
pub const WINDOWS_FOLDER_CLASS: &str = "CabinetWClass";

/// What Windows says about one top-level window, for
/// [`windows_window_verdict`]. Every field is `None` when it could not be
/// read.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct WindowsWindowFacts<'a> {
    /// The image of the process that owns the window handle
    /// (`applicationframehost.exe` for a packaged app's frame).
    pub owner_image: Option<&'a str>,
    /// The window class (`ApplicationFrameWindow`, `CabinetWClass`, `#32770`).
    pub class: Option<&'a str>,
    /// For a frame of [`WINDOWS_FRAME_HOST`], the image of the app it hosts
    /// (the process of its `Windows.UI.Core.CoreWindow` child).
    pub hosted_image: Option<&'a str>,
    /// For an Explorer folder window, the shell's location of it
    /// (`IWebBrowserApp::LocationURL`: `file:///C:/Users/...`; empty for the
    /// Control Panel and the other shell namespaces).
    pub folder_location: Option<&'a str>,
}

/// Whether the driver may touch a Windows window.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum WindowVerdict {
    Allow,
    /// The window belongs to a denied app (`app_denied`); why.
    Deny(String),
    /// What the window is cannot be told, so it is refused (`app_denied`);
    /// why.
    Unresolved(String),
}

/// The Windows window rule of ADR-0047 decisions 3 and 10, a pure function
/// over what the OS says about one window, so that every case is tested on
/// every OS. The owning process's image alone lets three kinds of window
/// through that the deny list means to stop:
///
/// - **A packaged app's frame** is owned by [`WINDOWS_FRAME_HOST`] whatever
///   it hosts: the window is judged by the hosted app's image instead, and
///   refused when that cannot be read (a suspended app's frame, a frame with
///   no app in it).
/// - **The shell** owns the Run box, the Control Panel (Credential Manager,
///   User Accounts), the taskbar and the Start menu as well as File
///   Explorer: only a folder window ([`WINDOWS_FOLDER_CLASS`]) whose location
///   is a file-system path (`file:` URL) is allowed; every other window of
///   [`WINDOWS_SHELL`] is refused, the shell's own namespaces (This PC, Home,
///   the Control Panel) included.
/// - **Anything else** is judged by its owner's image against
///   [`PLATFORM_APP_DENY_WINDOWS`]; an unreadable owner is refused.
#[must_use]
pub fn windows_window_verdict(facts: &WindowsWindowFacts<'_>) -> WindowVerdict {
    let Some(owner) = facts.owner_image.and_then(windows_app_id) else {
        return WindowVerdict::Unresolved("the window's process cannot be read".into());
    };
    if owner == WINDOWS_FRAME_HOST {
        return match facts.hosted_image.and_then(windows_app_id) {
            None => WindowVerdict::Unresolved(format!(
                "the app framed by {WINDOWS_FRAME_HOST} cannot be told"
            )),
            Some(hosted) if hosted == WINDOWS_FRAME_HOST => {
                WindowVerdict::Unresolved(format!("{WINDOWS_FRAME_HOST} frames itself"))
            }
            Some(hosted) if is_denied_app(Os::Windows, &hosted) => WindowVerdict::Deny(hosted),
            Some(_) => WindowVerdict::Allow,
        };
    }
    if owner == WINDOWS_SHELL {
        let folder = facts.class == Some(WINDOWS_FOLDER_CLASS)
            && facts.folder_location.is_some_and(is_file_location);
        return if folder {
            WindowVerdict::Allow
        } else {
            WindowVerdict::Deny(format!(
                "{WINDOWS_SHELL} ({}): only File Explorer folder windows are allowed",
                facts.class.unwrap_or("unknown class")
            ))
        };
    }
    if is_denied_app(Os::Windows, &owner) {
        WindowVerdict::Deny(owner)
    } else {
        WindowVerdict::Allow
    }
}

/// Why the driver of `os` never puts text into the windows of the
/// application `app_id`, or `None` when it may (#1586). File Explorer's
/// address bar runs what is typed into it (`powershell -c ...` and Return
/// starts a shell), and so does its search box's address mode; neither UI
/// Automation nor the vendor tells the address band from the folder view
/// reliably (the band's `Edit` sits inside `Address Band Root`, but the
/// focus moves into it from the folder view on a keystroke). So every act
/// that inserts text is refused in every `explorer.exe` window: typing
/// (with or without a ref), setting a value, and a chord that inserts text
/// ([`chord_inserts_text`], paste included). Clicks, navigation keys and
/// reads still pass.
#[must_use]
pub fn text_entry_refusal(os: Os, app_id: &str) -> Option<String> {
    (os == Os::Windows && app_id.trim().eq_ignore_ascii_case(WINDOWS_SHELL)).then(|| {
        format!(
            "{WINDOWS_SHELL}: File Explorer's address bar runs what is typed into it, so its windows are read and clicked but never typed into"
        )
    })
}

/// A `file:` URL with a path (`file:///C:/...`, `file://server/share`).
fn is_file_location(location: &str) -> bool {
    let location = location.trim();
    location
        .get(..7)
        .is_some_and(|scheme| scheme.eq_ignore_ascii_case("file://"))
        && location.len() > 7
}

/// What a Linux session says about its screen, for [`linux_screen_status`].
/// Each read is `None` when nothing answered it.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct LinuxScreenReads {
    /// `DISPLAY` or `WAYLAND_DISPLAY` is set.
    pub display: bool,
    /// logind's session of this process: `Active`, then `LockedHint`
    /// (`None` when the hint cannot be read).
    pub logind: Option<(bool, Option<bool>)>,
    /// The screensaver services (`org.freedesktop.ScreenSaver`,
    /// `org.gnome.ScreenSaver`): `Some(true)` when one says it is active,
    /// `Some(false)` when at least one answered and none is active.
    pub screensaver_service: Option<bool>,
    /// The X server's own screensaver (MIT-SCREEN-SAVER `QueryInfo`):
    /// `Some(true)` while it is on or cycling.
    pub x11_saver_on: Option<bool>,
}

/// The Linux screen rule (ADR-0047 decision 5, #1586), pure so every case is
/// tested on every OS. It fails closed: only a desktop's own screensaver
/// service that answers can say the screen is not locked. logind's
/// `LockedHint` is set by the desktops that integrate their locker (GNOME,
/// KDE), but i3lock, slock and xsecurelock set nothing and run no D-Bus
/// service, so a `LockedHint` of false, or no logind at all, says nothing:
/// without a screensaver service that answers the lock state is unknown and
/// the screen counts as locked. logind saying the session is inactive or
/// locked, and the X server's screensaver being on, refuse on their own.
#[must_use]
pub fn linux_screen_status(reads: &LinuxScreenReads) -> crate::ScreenStatus {
    let (active, logind_locked) = match reads.logind {
        Some((active, locked)) => (active, locked == Some(true)),
        None => (true, false),
    };
    crate::ScreenStatus {
        session_present: reads.display,
        on_console: active,
        locked: logind_locked,
        screensaver: reads.screensaver_service == Some(true) || reads.x11_saver_on == Some(true),
        display_asleep: false,
        lock_state_unknown: reads.screensaver_service.is_none(),
    }
}

/// What tells a Linux session's kind, for [`linux_guard_unavailable`].
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct LinuxSessionReads<'a> {
    /// `WAYLAND_DISPLAY`, when set and not empty.
    pub wayland_display: bool,
    /// `XDG_SESSION_TYPE`.
    pub env_session_type: Option<&'a str>,
    /// logind's `Session.Type` (`x11`, `wayland`, `tty`, `mir`,
    /// `unspecified`) of every session this process could be in: the one
    /// `XDG_SESSION_ID` names, the user's display session (`User.Display`),
    /// and the session of this process's pid. Empty without logind.
    pub logind_session_types: &'a [String],
    /// Whether the X server has the `XWAYLAND` extension, which only
    /// Xwayland 23.1 and later have (`None`: no X server could be reached).
    pub xwayland_extension: Option<bool>,
    /// The names of the X server's `RandR` outputs (`None`: no X server, or
    /// no `RandR`). `Xwayland` names them `XWAYLAND0`, `XWAYLAND1`, ...; Xorg
    /// after the connectors (`eDP-1`, `HDMI-1`, `DP-0`), Xvfb `screen`.
    pub randr_outputs: Option<&'a [String]>,
}

/// Why the Linux user-active guard cannot run here, or `None` on a plain
/// X11 session (#1586). The `XInput2` watch sees only X clients: under
/// Wayland, and under `XWayland` even with the environment stripped, the
/// person's typing in Wayland apps never reaches it, so a pause could be
/// missed. Any one sign of Wayland is enough: the environment, any logind
/// session this process could be in having type `wayland`, or the X server
/// being `Xwayland` (its `XWAYLAND` extension, or `RandR` outputs named
/// `XWAYLAND*`). It fails closed on every sign; with none of them the
/// session is taken as X11 (a plain Xorg started without logind or
/// `XDG_SESSION_TYPE` must still act).
#[must_use]
pub fn linux_guard_unavailable(reads: &LinuxSessionReads<'_>) -> Option<&'static str> {
    let is = |v: &str, want: &str| v.trim().eq_ignore_ascii_case(want);
    if reads.wayland_display || reads.env_session_type.is_some_and(|v| is(v, "wayland")) {
        return Some("the session is a Wayland session (environment)");
    }
    if reads.logind_session_types.iter().any(|t| is(t, "wayland")) {
        return Some("the session is a Wayland session (logind)");
    }
    let xwayland_output = |name: &String| {
        name.get(..8)
            .is_some_and(|head| head.eq_ignore_ascii_case("XWAYLAND"))
    };
    if reads.xwayland_extension == Some(true)
        || reads
            .randr_outputs
            .is_some_and(|outputs| outputs.iter().any(xwayland_output))
    {
        return Some("the X server is XWayland: input to Wayland apps cannot be watched");
    }
    None
}

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

    fn args(words: &[&str]) -> Vec<String> {
        words.iter().map(|&w| w.to_owned()).collect()
    }

    #[test]
    fn a_linux_app_without_a_desktop_entry_is_named_by_its_executable() {
        // xterm and foot have no desktop entry on many systems: their id is
        // the executable, so the deny entries match.
        for (link, comm, want) in [
            (Some("/usr/bin/xterm"), "xterm", Some("xterm")),
            (Some("/usr/bin/foot"), "foot", Some("foot")),
            (
                Some("/usr/libexec/gnome-terminal-server"),
                "gnome-terminal-",
                Some("gnome-terminal-server"),
            ),
            // A binary replaced by a package upgrade while it runs.
            (Some("/usr/bin/kitty (deleted)"), "kitty", Some("kitty")),
            // No readable link: comm, unless the kernel may have cut it.
            (None, "xterm", Some("xterm")),
            (None, "gnome-terminal-", None),
            (Some(""), "", None),
        ] {
            assert_eq!(
                linux_executable_name(link, comm, &[]).as_deref(),
                want,
                "{link:?} {comm:?}"
            );
        }
        let id = |link, comm| {
            linux_app_id(
                None,
                &linux_executable_name(link, comm, &[]).unwrap_or_default(),
            )
        };
        assert!(id(Some("/usr/bin/xterm"), "xterm").is_some_and(|i| is_denied_app(Os::Linux, &i)));
        assert!(id(Some("/usr/bin/foot"), "foot").is_some_and(|i| is_denied_app(Os::Linux, &i)));
        assert_eq!(id(None, "gnome-terminal-"), None);
    }

    /// What `/proc/<pid>` reads for real interpreted apps (Ubuntu 24.04:
    /// `exe` is the versioned interpreter, a `#!` script's `comm` is the
    /// script's file name, `cmdline` starts with the `#!` line's program).
    #[test]
    fn an_interpreted_linux_app_is_named_by_its_script() {
        let py = Some("/usr/bin/python3.12");
        for (link, comm, cmdline, want) in [
            // Terminator and GNOME Secrets: both `python3.12` by exe.
            (
                py,
                "terminator",
                args(&["/usr/bin/python3", "/usr/bin/terminator"]),
                Some("terminator"),
            ),
            (
                py,
                "secrets",
                args(&["/usr/bin/python3", "/usr/bin/secrets"]),
                Some("secrets"),
            ),
            // Started by hand through the interpreter: comm is python3.
            (
                py,
                "python3",
                args(&["python3", "-u", "/usr/bin/terminator", "--new-tab"]),
                Some("terminator"),
            ),
            // `-m module`: the module, options before it skipped.
            (
                py,
                "python3",
                args(&["python3", "-W", "ignore", "-m", "foo", "--x"]),
                Some("foo"),
            ),
            (
                py,
                "python3",
                args(&["python3", "-m", "pkg.app"]),
                Some("pkg.app"),
            ),
            // A `.py` script whose comm keeps the suffix.
            (
                py,
                "tool.py",
                args(&["/usr/bin/python3", "/opt/tool/tool.py"]),
                Some("tool"),
            ),
            // A node app: the script, or its directory when generic.
            (
                Some("/usr/bin/node"),
                "node",
                args(&[
                    "node",
                    "--max-old-space-size=4096",
                    "/opt/notesapp/index.js",
                ]),
                Some("notesapp"),
            ),
            (
                Some("/usr/lib/node_modules/.bin/node22"),
                "node",
                args(&["node", "-r", "dotenv/config", "/srv/bridge.mjs"]),
                Some("bridge"),
            ),
            // java -jar, gjs -m, a bash script.
            (
                Some("/usr/lib/jvm/java-21/bin/java"),
                "java",
                args(&[
                    "java",
                    "-Xmx1g",
                    "-cp",
                    "lib/*",
                    "-jar",
                    "/opt/keystore-explorer/kse.jar",
                ]),
                Some("kse"),
            ),
            (
                Some("/usr/bin/gjs-console"),
                "gjs",
                args(&["gjs", "-m", "/usr/share/org.example.App/main.js"]),
                Some("org.example.App"),
            ),
            (
                Some("/usr/bin/bash"),
                "launch-foo",
                args(&["/bin/bash", "/usr/local/bin/launch-foo"]),
                Some("launch-foo"),
            ),
        ] {
            assert_eq!(
                linux_executable_name(link, comm, &cmdline).as_deref(),
                want,
                "{link:?} {comm:?} {cmdline:?}"
            );
        }
        let id = |comm, cmdline: &[&str]| {
            linux_executable_name(py, comm, &args(cmdline))
                .and_then(|name| linux_app_id(None, &name))
        };
        assert!(
            id("terminator", &["/usr/bin/python3", "/usr/bin/terminator"])
                .is_some_and(|i| is_denied_app(Os::Linux, &i))
        );
        assert!(
            id("secrets", &["/usr/bin/python3", "/usr/bin/secrets"])
                .is_some_and(|i| is_denied_app(Os::Linux, &i))
        );
    }

    #[test]
    fn an_interpreter_with_no_script_name_has_no_id() {
        let py = Some("/usr/bin/python3.12");
        for (link, comm, cmdline) in [
            // Inline code and standard input name nothing.
            (
                py,
                "python3",
                args(&["python3", "-c", "import x; x.main()"]),
            ),
            (py, "python3", args(&["python3", "-"])),
            (py, "python3", args(&["python3"])),
            (Some("/usr/bin/bash"), "bash", args(&["bash", "-c", "foo"])),
            (
                Some("/usr/bin/node"),
                "node",
                args(&["node", "-e", "run()"]),
            ),
            // A generic entry point under generic directories only.
            (
                Some("/usr/bin/node"),
                "node",
                args(&["node", "/usr/lib/main.js"]),
            ),
            // The script and comm disagree: fail closed.
            (
                py,
                "notepad",
                args(&["/usr/bin/python3", "/usr/bin/terminator"]),
            ),
            // No readable cmdline, comm cut by the kernel.
            (py, "a-very-long-nam", Vec::new()),
            // No link, an interpreter's comm, no script.
            (None, "python3.12", Vec::new()),
            // The script is itself an interpreter's name.
            (py, "python3", args(&["python3", "/usr/bin/node"])),
        ] {
            assert_eq!(
                linux_executable_name(link, comm, &cmdline),
                None,
                "{link:?} {comm:?} {cmdline:?}"
            );
        }
        // Whatever reached it, an interpreter is never an id.
        for name in [
            "python3.12",
            "python3",
            "node",
            "bash",
            "sh",
            "java",
            "lua5.4",
            "Perl",
        ] {
            assert!(is_interpreter_name(name), "{name}");
            assert_eq!(linux_app_id(None, name), None, "{name}");
            assert!(!valid_app_id(Os::Linux, name), "{name}");
            assert!(unnameable_app(name).is_some(), "{name}");
        }
        // A desktop entry id still wins, and ordinary names are not
        // interpreters.
        assert_eq!(
            linux_app_id(Some("org.gnome.World.Secrets.desktop"), "python3.12").as_deref(),
            Some("org.gnome.World.Secrets")
        );
        for name in [
            "gedit",
            "st",
            "nodepad",
            "shotwell",
            "javelin",
            "org.gnome.Shell",
        ] {
            assert!(!is_interpreter_name(name), "{name}");
            assert_eq!(unnameable_app(name), None, "{name}");
        }
    }

    #[test]
    fn text_inserting_chords_are_told_from_navigation() {
        for c in [
            "a",
            "shift+a",
            "1",
            "space",
            "ctrl+v",
            "ctrl+shift+v",
            "cmd+v",
            "shift+insert",
            "paste",
            // AltGr on Windows, the Option layer on Linux keyboards.
            "ctrl+alt+q",
            "alt+e",
            // The Windows emoji and symbol panel, and the clipboard history
            // (Command is the Windows key there), with any extra modifier.
            "cmd+.",
            "cmd+;",
            "meta+period",
            "cmd+semicolon",
            "cmd+v",
            "ctrl+cmd+.",
            "cmd+shift+v",
            // An unknown key name fails closed.
            "kp_1",
            "dead_acute",
        ] {
            assert!(chord_inserts_text(&chord(c)), "{c} inserts text");
        }
        for c in [
            "return",
            "tab",
            "shift+tab",
            "escape",
            "up",
            "ctrl+left",
            "shift+end",
            "pagedown",
            "backspace",
            "delete",
            "insert",
            "ctrl+insert",
            "f5",
            "ctrl+f",
            "ctrl+s",
            "ctrl+a",
            "ctrl+c",
            "ctrl+z",
            "cmd+w",
            "ctrl+space",
            // The same keys under Control are ordinary shortcuts.
            "ctrl+.",
            "ctrl+;",
            "ctrl+period",
        ] {
            assert!(!chord_inserts_text(&chord(c)), "{c} does not insert");
        }
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

    fn facts<'a>(
        owner: &'a str,
        class: &'a str,
        hosted: Option<&'a str>,
        location: Option<&'a str>,
    ) -> WindowsWindowFacts<'a> {
        WindowsWindowFacts {
            owner_image: Some(owner),
            class: Some(class),
            hosted_image: hosted,
            folder_location: location,
        }
    }

    #[test]
    fn the_new_windows_deny_entries_are_denied() {
        for app in [
            "rundll32.exe",
            "control.exe",
            "mmc.exe",
            "regedit.exe",
            "consent.exe",
            "CredentialUIBroker.exe",
        ] {
            assert!(is_denied_app(Os::Windows, app), "{app} must be denied");
        }
    }

    /// The values a Windows 11 desktop reports for each window (owner image
    /// as `ToolHelp` spells it, class as `GetClassNameW` returns it).
    #[test]
    fn a_framed_app_is_judged_by_the_app_it_hosts() {
        let frame = "ApplicationFrameWindow";
        for hosted in ["SystemSettings.exe", "SecHealthUI.exe"] {
            assert!(
                matches!(
                    windows_window_verdict(&facts(
                        "ApplicationFrameHost.exe",
                        frame,
                        Some(hosted),
                        None
                    )),
                    WindowVerdict::Deny(_)
                ),
                "{hosted} must be denied"
            );
        }
        assert_eq!(
            windows_window_verdict(&facts(
                "ApplicationFrameHost.exe",
                frame,
                Some("CalculatorApp.exe"),
                None
            )),
            WindowVerdict::Allow
        );
        // A suspended app's frame has no CoreWindow: what it hosts cannot be
        // told, so it is refused.
        for hosted in [None, Some(""), Some("ApplicationFrameHost.exe")] {
            assert!(
                matches!(
                    windows_window_verdict(&facts("ApplicationFrameHost.exe", frame, hosted, None)),
                    WindowVerdict::Unresolved(_)
                ),
                "{hosted:?}"
            );
        }
    }

    #[test]
    fn only_explorer_folder_windows_on_a_path_are_allowed() {
        for location in [
            "file:///C:/Users/ada/Documents",
            "FILE:///D:/",
            "file://fileserver/share/reports",
        ] {
            assert_eq!(
                windows_window_verdict(&facts(
                    "explorer.exe",
                    "CabinetWClass",
                    None,
                    Some(location)
                )),
                WindowVerdict::Allow,
                "{location}"
            );
        }
        // The Control Panel (Credential Manager, User Accounts) is a
        // CabinetWClass window too, with no file location; so are This PC
        // and Home. An unread location is refused.
        for location in [None, Some(""), Some("file://"), Some("shell:::{26EE0668}")] {
            assert!(
                matches!(
                    windows_window_verdict(&facts("explorer.exe", "CabinetWClass", None, location)),
                    WindowVerdict::Deny(_)
                ),
                "{location:?}"
            );
        }
        // The Run box, the taskbar, the Start menu, the desktop.
        for class in [
            "#32770",
            "Shell_TrayWnd",
            "Windows.UI.Core.CoreWindow",
            "Progman",
            "WorkerW",
        ] {
            assert!(
                matches!(
                    windows_window_verdict(&facts(
                        "Explorer.EXE",
                        class,
                        None,
                        Some("file:///C:/")
                    )),
                    WindowVerdict::Deny(_)
                ),
                "{class}"
            );
        }
        let no_class = WindowsWindowFacts {
            owner_image: Some("explorer.exe"),
            folder_location: Some("file:///C:/"),
            ..WindowsWindowFacts::default()
        };
        assert!(matches!(
            windows_window_verdict(&no_class),
            WindowVerdict::Deny(_)
        ));
    }

    #[test]
    fn explorer_windows_are_never_typed_into() {
        for id in ["explorer.exe", "Explorer.EXE", " explorer.exe "] {
            assert!(
                text_entry_refusal(Os::Windows, id).is_some(),
                "{id} must refuse text"
            );
        }
        // Other apps, and the same name on the other OSs, take text.
        for (os, id) in [
            (Os::Windows, "notepad.exe"),
            (Os::Windows, "explorer2.exe"),
            (Os::Linux, "explorer.exe"),
            (Os::MacOs, "explorer.exe"),
        ] {
            assert_eq!(text_entry_refusal(os, id), None, "{os:?} {id}");
        }
        // What the engine asks of a chord in an Explorer window: the
        // inserting ones are refused, clicks' companions (navigation,
        // Return to open the selection, F2 alone) pass.
        for c in ["p", "shift+p", "ctrl+v", "shift+insert", "space", "alt+d"] {
            assert!(chord_inserts_text(&chord(c)), "{c} inserts text");
        }
        for c in [
            "return",
            "up",
            "down",
            "backspace",
            "f5",
            "ctrl+c",
            "delete",
        ] {
            assert!(!chord_inserts_text(&chord(c)), "{c} does not insert");
        }
    }

    #[test]
    fn other_windows_are_judged_by_their_owner() {
        assert_eq!(
            windows_window_verdict(&facts("notepad.exe", "Notepad", None, None)),
            WindowVerdict::Allow
        );
        for owner in ["cmd.exe", "rundll32.exe", "mmc.exe", "consent.exe"] {
            assert!(
                matches!(
                    windows_window_verdict(&facts(owner, "#32770", None, None)),
                    WindowVerdict::Deny(_)
                ),
                "{owner}"
            );
        }
        assert!(matches!(
            windows_window_verdict(&WindowsWindowFacts::default()),
            WindowVerdict::Unresolved(_)
        ));
    }

    #[test]
    fn a_linux_screen_is_usable_only_when_a_screensaver_service_says_so() {
        let gnome = LinuxScreenReads {
            display: true,
            logind: Some((true, Some(false))),
            screensaver_service: Some(false),
            x11_saver_on: Some(false),
        };
        assert_eq!(linux_screen_status(&gnome).unavailable_reason(), None);
        // KDE without logind (a container, a non-systemd distribution).
        let no_logind = LinuxScreenReads {
            logind: None,
            ..gnome
        };
        assert_eq!(linux_screen_status(&no_logind).unavailable_reason(), None);
        // The X server's own saver is not read on Wayland: still fine.
        let wayland = LinuxScreenReads {
            x11_saver_on: None,
            ..gnome
        };
        assert_eq!(linux_screen_status(&wayland).unavailable_reason(), None);
    }

    #[test]
    fn an_unknown_linux_lock_state_counts_as_locked() {
        // i3 + i3lock: logind says unlocked because nobody tells it, and no
        // screensaver service runs.
        let i3 = LinuxScreenReads {
            display: true,
            logind: Some((true, Some(false))),
            screensaver_service: None,
            x11_saver_on: Some(false),
        };
        // No logind and no screensaver service at all.
        let bare = LinuxScreenReads {
            display: true,
            ..LinuxScreenReads::default()
        };
        for reads in [i3, bare] {
            let status = linux_screen_status(&reads);
            assert!(status.lock_state_unknown, "{reads:?}");
            assert!(
                status
                    .unavailable_reason()
                    .is_some_and(|r| r.contains("counts as locked")),
                "{reads:?}"
            );
        }
    }

    #[test]
    fn logind_and_the_x_saver_refuse_on_their_own() {
        let ok = LinuxScreenReads {
            display: true,
            logind: Some((true, Some(false))),
            screensaver_service: Some(false),
            x11_saver_on: Some(false),
        };
        let cases = [
            (
                LinuxScreenReads {
                    logind: Some((true, Some(true))),
                    ..ok
                },
                "locked",
            ),
            (
                LinuxScreenReads {
                    logind: Some((false, Some(false))),
                    ..ok
                },
                "console",
            ),
            (
                LinuxScreenReads {
                    screensaver_service: Some(true),
                    ..ok
                },
                "screensaver",
            ),
            (
                LinuxScreenReads {
                    x11_saver_on: Some(true),
                    ..ok
                },
                "screensaver",
            ),
            (
                LinuxScreenReads {
                    display: false,
                    ..ok
                },
                "session",
            ),
        ];
        for (reads, word) in cases {
            let reason = linux_screen_status(&reads).unavailable_reason();
            assert!(
                reason.is_some_and(|r| r.contains(word)),
                "{reads:?}: {reason:?}"
            );
        }
    }

    fn names(list: &[&str]) -> Vec<String> {
        list.iter().map(|&n| n.to_owned()).collect()
    }

    /// Values real sessions report: Ubuntu 24.04 "GNOME on Xorg"
    /// (modesetting outputs), an NVIDIA Xorg, Xvfb, and Xwayland 22.1
    /// (Ubuntu 22.04: no `XWAYLAND` extension yet) and 23.2 (Ubuntu 24.04).
    /// Every X server's vendor string is "The X.Org Foundation", Xwayland's
    /// included, so it tells nothing and is not read.
    #[test]
    fn the_guard_runs_on_plain_x11_only() {
        let x11 = names(&["x11"]);
        let xorg_outputs = names(&["eDP-1", "HDMI-1", "DP-1"]);
        let xorg = LinuxSessionReads {
            wayland_display: false,
            env_session_type: Some("x11"),
            logind_session_types: &x11,
            xwayland_extension: Some(false),
            randr_outputs: Some(&xorg_outputs),
        };
        assert_eq!(linux_guard_unavailable(&xorg), None);
        // startx on a distribution without logind: nothing names the
        // session, and the X server is not Xwayland.
        let nvidia = names(&["DP-0", "HDMI-0"]);
        let xvfb = names(&["screen"]);
        for outputs in [Some(nvidia.as_slice()), Some(xvfb.as_slice()), None] {
            let bare = LinuxSessionReads {
                env_session_type: None,
                logind_session_types: &[],
                randr_outputs: outputs,
                ..xorg
            };
            assert_eq!(linux_guard_unavailable(&bare), None, "{outputs:?}");
        }
        // startx from a text console: logind says tty.
        let tty = names(&["tty"]);
        let startx = LinuxSessionReads {
            env_session_type: Some("tty"),
            logind_session_types: &tty,
            ..xorg
        };
        assert_eq!(linux_guard_unavailable(&startx), None);
    }

    #[test]
    fn any_sign_of_wayland_stops_the_guard() {
        let x11 = names(&["x11"]);
        let wayland = names(&["wayland"]);
        // An app scope's pid has no session, the env one is an ssh tty,
        // the user's display session is Wayland.
        let mixed = names(&["tty", "wayland"]);
        let xorg_outputs = names(&["eDP-1"]);
        let xwayland_outputs = names(&["XWAYLAND0", "XWAYLAND1"]);
        let xorg = LinuxSessionReads {
            wayland_display: false,
            env_session_type: Some("x11"),
            logind_session_types: &x11,
            xwayland_extension: Some(false),
            randr_outputs: Some(&xorg_outputs),
        };
        let stripped = LinuxSessionReads {
            env_session_type: None,
            logind_session_types: &[],
            ..xorg
        };
        for reads in [
            LinuxSessionReads {
                wayland_display: true,
                ..xorg
            },
            LinuxSessionReads {
                env_session_type: Some("Wayland"),
                ..xorg
            },
            // The environment stripped (a launcher that unsets
            // WAYLAND_DISPLAY and XDG_SESSION_TYPE): logind still knows.
            LinuxSessionReads {
                logind_session_types: &wayland,
                ..stripped
            },
            LinuxSessionReads {
                logind_session_types: &mixed,
                ..stripped
            },
            // Environment stripped and no logind: Xwayland 23.1 or later
            // has the XWAYLAND extension.
            LinuxSessionReads {
                xwayland_extension: Some(true),
                ..stripped
            },
            // An older Xwayland (22.1) has no extension, but names its
            // `RandR` outputs XWAYLAND0...
            LinuxSessionReads {
                xwayland_extension: Some(false),
                randr_outputs: Some(&xwayland_outputs),
                ..stripped
            },
        ] {
            assert!(linux_guard_unavailable(&reads).is_some(), "{reads:?}");
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
