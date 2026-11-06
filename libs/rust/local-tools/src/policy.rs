//! The `local_work` group of the native client policy (ADR-0029 decision
//! 6), as far as local tools read it.
//!
//! The desktop receives the policy with every token response and enforces
//! it here; the server cannot verify what a client did. A field this crate
//! does not use (`cloud_sync`) is ignored when deserialising. Every default
//! fails closed (`local_index` and `memory_write` too: the server sends
//! `true` by default, a policy without the field turns it off).

use serde::{Deserialize, Serialize};

/// How far a command may reach, ordered from the most confined.
#[derive(
    Clone, Copy, Debug, Default, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize,
)]
#[serde(rename_all = "kebab-case")]
pub enum SandboxMode {
    /// Reads anywhere, writes nowhere (but `/dev/null` and the like).
    #[default]
    ReadOnly,
    /// Reads anywhere, writes only inside the workspace and the session's
    /// temporary directory.
    WorkspaceWrite,
    /// No file system confinement.
    FullAccess,
}

impl SandboxMode {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::ReadOnly => "read-only",
            Self::WorkspaceWrite => "workspace-write",
            Self::FullAccess => "full-access",
        }
    }
}

/// `native_client_policy.local_work`.
// The server's wire format: one switch per capability.
#[allow(clippy::struct_excessive_bools)]
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(default)]
pub struct LocalWorkPolicy {
    /// Local work is off unless the policy enables it.
    pub allowed: bool,
    /// Whether the shell tool exists at all.
    pub shell: bool,
    /// The widest sandbox a command may ask for.
    pub max_sandbox_mode: SandboxMode,
    /// Whether a command may use the network.
    pub network: bool,
    /// When non-empty, the only commands that may run (argv prefixes, see
    /// [`crate::command::CommandPattern`]). A ceiling, not an approval:
    /// an admissible command still goes through the rules below (a
    /// destructive one is asked, see [`crate::classify`]).
    pub command_allow: Vec<String>,
    /// Commands refused when the model names them (in any segment of a
    /// compound command, and inside `sh -c`, `env`, `sudo` and other
    /// wrappers). **Advisory, not a security boundary:** a program started
    /// by a script, a build tool or an interpreter is not seen; the sandbox
    /// and the approval are what confine a command (see
    /// [`crate::command`]).
    pub command_deny: Vec<String>,
    /// Paths no local tool reads or writes (globs, see
    /// [`crate::workspace::Workspace::open`]).
    pub path_deny: Vec<String>,
    /// Whether the desktop may build and use a local index of a workspace
    /// (ADR-0029 decision 7). Only with `allowed`: the host gates on both.
    pub local_index: bool,
    /// `local_work_cloud_handoff` (ADR-0034 decision 11): this device may
    /// send a Local work thread to the cloud executor. Off when absent.
    pub cloud_handoff: bool,
    /// `local_work_index_local_embeddings` (client contract 1.9): a local
    /// index may be embedded by a model run on this device. The server
    /// sends `true` by default; off when absent.
    pub index_local_embeddings: bool,
    /// `local_work_index_remote_embeddings` (client contract 1.9): a local
    /// index may be embedded through the deployment's embeddings route.
    /// Off by default and when absent; turned off, those vectors go.
    pub index_remote_embeddings: bool,
    /// `local_work_index_model_mirror_url` (client contract 1.9): where the
    /// on-device embedding models are downloaded from instead of Hugging
    /// Face (an https base laid out as its CDN). Empty: Hugging Face.
    pub index_model_mirror_url: String,
    /// `local_work_local_mcp` (ADR-0029 decision 4, #1230): the person's
    /// own stdio MCP servers may run on this device ([`crate::mcp`]). Only
    /// with `allowed`; off when absent, and turning it off stops them.
    pub local_mcp: bool,
    /// `local_work_memory_write` (ADR-0029 decision 8, #1233): a local turn
    /// may save and delete the person's memories (`save_memory`,
    /// `delete_memory`). Recall and `search_memory` never depend on it. Off
    /// when absent: the tools are not offered and their calls are denied.
    pub memory_write: bool,
    /// `local_work_web` (#1238): local turns may call a remote `web` toolkit
    /// (`web_search`, `web_fetch`), which sends queries and URLs to
    /// third-party providers. Off when absent: the tools are not offered and
    /// the server refuses the calls.
    pub web: bool,
    /// `local_work_host_intel` (#1238): local turns may call a remote
    /// `host_intel` toolkit (Shodan). Off when absent.
    pub host_intel: bool,
    /// `local_work_computer_use` (ADR-0047 decision 3, client contract
    /// 1.36): the `computer` toolset exists at all. Off when absent: the
    /// tools are not offered and their calls are denied.
    pub computer_use: bool,
    /// `local_work_computer_use_apps_allow`: when non-empty, the only
    /// applications (bundle ids, see [`bundle_id_matches`]) the agent may
    /// read or act on. A ceiling, not an approval: an admitted application
    /// is still asked for ([`crate::approvals`]).
    pub computer_use_apps_allow: Vec<String>,
    /// `local_work_computer_use_apps_deny`: applications never read or
    /// acted on, added to [`PLATFORM_APP_DENY`] (which no policy can lift).
    /// A deny wins over an allow.
    pub computer_use_apps_deny: Vec<String>,
    /// `local_work_browser` (ADR-0048 decision 8, client contract 1.39):
    /// the browser pane and its toolset exist for local threads. The server
    /// sends `true` by default; off when absent, like every field here.
    pub browser: bool,
    /// `local_work_browser_agent`: the agent may drive the pane (off: the
    /// person can still browse). Only with [`Self::browser`].
    pub browser_agent: bool,
    /// `local_work_browser_allow_domains`: when non-empty, the only hosts the
    /// pane loads ([`browser_domain_matches`]). A ceiling, not an approval.
    pub browser_allow_domains: Vec<String>,
    /// `local_work_browser_deny_domains`: hosts the pane never loads. A deny
    /// wins over an allow.
    pub browser_deny_domains: Vec<String>,
    /// `local_work_browser_http`: plain `http` pages may load.
    pub browser_http: bool,
    /// `local_work_browser_private_network`: loopback and private addresses
    /// (and the names private by definition, `localhost`, `.local`, …) may
    /// load. Link-local and the other forbidden ranges never do.
    pub browser_private_network: bool,
}

impl Default for LocalWorkPolicy {
    fn default() -> Self {
        Self {
            allowed: false,
            shell: false,
            max_sandbox_mode: SandboxMode::WorkspaceWrite,
            network: false,
            command_allow: Vec::new(),
            command_deny: Vec::new(),
            path_deny: Vec::new(),
            local_index: false,
            cloud_handoff: false,
            index_local_embeddings: false,
            index_remote_embeddings: false,
            index_model_mirror_url: String::new(),
            local_mcp: false,
            memory_write: false,
            web: false,
            host_intel: false,
            computer_use: false,
            computer_use_apps_allow: Vec::new(),
            computer_use_apps_deny: Vec::new(),
            browser: false,
            browser_agent: false,
            browser_allow_domains: Vec::new(),
            browser_deny_domains: Vec::new(),
            browser_http: false,
            browser_private_network: false,
        }
    }
}

/// The applications computer use never reads or acts on (ADR-0047
/// decisions 3 and 5), merged into every policy's
/// [`LocalWorkPolicy::computer_use_apps_deny`]. An administrator can add to
/// it; nothing removes an entry, and an allow list never admits one.
///
/// Each entry is a bundle id, or a `prefix.*` family ([`bundle_id_matches`]).
/// The list is also matched against the System Settings pane a window
/// shows (the pane extension's bundle id), so the security panes are denied
/// while the rest of System Settings is not; a System Settings window whose
/// pane is unknown is denied too.
pub const PLATFORM_APP_DENY: &[&str] = &[
    // Elitea itself: the agent must never approve its own asks (decision 5,
    // "No Elitea control").
    "ai.elitea.desktop",
    // Password managers and credential stores.
    "com.apple.keychainaccess",   // Keychain Access
    "com.apple.Passwords",        // Passwords (macOS 15)
    "com.1password.*",            // 1Password 8 and its helpers
    "com.agilebits.onepassword7", // 1Password 7
    "com.bitwarden.desktop",      // Bitwarden
    "org.keepassxc.keepassxc",    // KeePassXC
    "com.lastpass.LastPass",      // LastPass
    "in.sinew.Enpass-Desktop",    // Enpass
    "me.proton.pass.electron",    // Proton Pass
    // Terminals: the shell has its own sandboxed tool, and a terminal would
    // bypass it (owner decision 1 at acceptance).
    "com.apple.Terminal",     // Terminal
    "com.googlecode.iterm2",  // iTerm2
    "dev.warp.*",             // Warp (stable and preview)
    "com.mitchellh.ghostty",  // Ghostty
    "org.alacritty",          // Alacritty
    "net.kovidgoyal.kitty",   // kitty
    "com.github.wez.wezterm", // WezTerm
    "co.zeit.hyper",          // Hyper
    "org.tabby",              // Tabby
    // Script runners: `do shell script` and "Run Shell Script" are the shell
    // again, outside its sandbox.
    "com.apple.ScriptEditor2", // Script Editor
    "com.apple.Automator",     // Automator
    "com.apple.shortcuts",     // Shortcuts
    // The login window and the system's authentication prompts.
    "com.apple.loginwindow",   // the login window
    "com.apple.SecurityAgent", // the system's password prompts
    // Screen Time.
    "com.apple.Screen-Time-Settings.extension", // the Screen Time pane
    "com.apple.preference.screentime",          // Screen Time (macOS 12)
    "com.apple.ScreenTimeAgent",                // its agent
    // The security panes of System Settings, and their System Preferences
    // forms before macOS 13.
    "com.apple.settings.PrivacySecurity.extension", // Privacy & Security
    "com.apple.Touch-ID-Settings.extension",        // Touch ID & Password
    "com.apple.Users-Groups-Settings.extension",    // Users & Groups
    "com.apple.Passwords-Settings.extension",       // Passwords (macOS 13 and 14)
    "com.apple.Lock-Screen-Settings.extension",     // Lock Screen
    "com.apple.LoginItems-Settings.extension",      // Login Items & Extensions
    "com.apple.preference.security",                // Security & Privacy (macOS 12)
    "com.apple.preferences.password",               // Touch ID (macOS 12)
    "com.apple.preferences.users",                  // Users & Groups (macOS 12)
];

/// System Settings' bundle id. Computer use admits it only with a known
/// pane that no deny entry names ([`LocalWorkPolicy::computer_use_refusal`]).
pub const SYSTEM_SETTINGS: &str = "com.apple.systempreferences";

/// The longest bundle id (or `prefix.*` pattern) a policy may carry.
pub const MAX_BUNDLE_ID_BYTES: usize = 255;

/// Whether `pattern` is a bundle id a policy may carry: ASCII letters,
/// digits and `-`, in `.`-separated components none of which is empty, at
/// most [`MAX_BUNDLE_ID_BYTES`]; or such an id followed by `.*` (a family:
/// every bundle id under that prefix). The server's
/// `platformconfig.ValidBundleIDPattern` is the same rule.
#[must_use]
pub fn valid_bundle_id_pattern(pattern: &str) -> bool {
    if pattern.is_empty() || pattern.len() > MAX_BUNDLE_ID_BYTES {
        return false;
    }
    let id = pattern.strip_suffix(".*").unwrap_or(pattern);
    !id.is_empty()
        && id.split('.').all(|part| {
            !part.is_empty()
                && part
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
        })
}

/// Whether `bundle_id` is `pattern`: equal ignoring ASCII case (Launch
/// Services compares bundle ids that way), or under a `prefix.*` family
/// (one component more, at least). An invalid pattern matches nothing.
#[must_use]
pub fn bundle_id_matches(pattern: &str, bundle_id: &str) -> bool {
    if !valid_bundle_id_pattern(pattern) {
        return false;
    }
    match pattern.strip_suffix(".*") {
        Some(prefix) => {
            bundle_id.len() > prefix.len() + 1
                && bundle_id.as_bytes()[prefix.len()] == b'.'
                && bundle_id
                    .get(..prefix.len())
                    .is_some_and(|head| head.eq_ignore_ascii_case(prefix))
        }
        None => bundle_id.eq_ignore_ascii_case(pattern),
    }
}

impl LocalWorkPolicy {
    /// The applications computer use refuses: [`PLATFORM_APP_DENY`], then
    /// the policy's own `computer_use_apps_deny`.
    #[must_use]
    pub fn computer_use_denied_apps(&self) -> Vec<String> {
        PLATFORM_APP_DENY
            .iter()
            .map(|&entry| entry.to_owned())
            .chain(self.computer_use_apps_deny.iter().cloned())
            .collect()
    }

    /// Why computer use may not touch the application `bundle_id` (showing
    /// the System Settings pane `pane`, if any), or `None` when the policy
    /// admits it. The `computer_use` switch is checked by the caller. In
    /// order: a value that is not a bundle id; a deny entry (the platform's
    /// or the policy's) matching the bundle id or the pane, whatever the
    /// allow list says; System Settings ([`SYSTEM_SETTINGS`]) with no known
    /// pane (fail closed: it could show a security pane); a non-empty allow
    /// list that does not name the app.
    #[must_use]
    pub fn computer_use_refusal(&self, bundle_id: &str, pane: Option<&str>) -> Option<String> {
        if bundle_id.ends_with(".*") || !valid_bundle_id_pattern(bundle_id) {
            return Some(format!("`{bundle_id}` is not an application bundle id"));
        }
        let denied = |id: &str| {
            PLATFORM_APP_DENY
                .iter()
                .copied()
                .chain(self.computer_use_apps_deny.iter().map(String::as_str))
                .any(|pattern| bundle_id_matches(pattern, id))
        };
        if denied(bundle_id) {
            return Some(format!("computer use never reads or acts on `{bundle_id}`"));
        }
        if let Some(pane) = pane.filter(|pane| denied(pane)) {
            return Some(format!(
                "computer use never reads or acts on the `{pane}` settings"
            ));
        }
        // System Settings fails closed: with no pane named, the window could
        // show a security pane, so only a known pane no deny entry names
        // admits it.
        if bundle_id.eq_ignore_ascii_case(SYSTEM_SETTINGS)
            && !pane.is_some_and(|pane| !pane.ends_with(".*") && valid_bundle_id_pattern(pane))
        {
            return Some(
                "computer use reads or acts on System Settings only when the pane it shows is known"
                    .to_owned(),
            );
        }
        if !self.computer_use_apps_allow.is_empty()
            && !self
                .computer_use_apps_allow
                .iter()
                .any(|pattern| bundle_id_matches(pattern, bundle_id))
        {
            return Some(format!(
                "`{bundle_id}` is not in the policy's computer_use_apps_allow"
            ));
        }
        None
    }
}

/// The longest browser domain entry a policy may carry.
pub const MAX_BROWSER_DOMAIN_BYTES: usize = 253;

/// Whether `entry` is a browser domain a policy may carry: a lowercase DNS
/// name of at least two labels (`example.com`), or `*.` and such a name
/// (`*.example.com`); no scheme, port, path or IP address, at most
/// [`MAX_BROWSER_DOMAIN_BYTES`]. The server's
/// `platformconfig.ValidBrowserDomain` is the same rule.
#[must_use]
pub fn valid_browser_domain(entry: &str) -> bool {
    if entry.is_empty() || entry.len() > MAX_BROWSER_DOMAIN_BYTES {
        return false;
    }
    let name = entry.strip_prefix("*.").unwrap_or(entry);
    let labels: Vec<&str> = name.split('.').collect();
    labels.len() >= 2
        && labels.iter().all(|label| {
            !label.is_empty()
                && label.len() <= 63
                && !label.starts_with('-')
                && !label.ends_with('-')
                && label
                    .bytes()
                    .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'-')
        })
        && !name
            .bytes()
            .all(|byte| byte.is_ascii_digit() || byte == b'.')
}

/// Whether `host` is under the browser domain `entry`: `example.com` names
/// it and every host under it, `*.example.com` only the hosts under it.
/// Compared ignoring ASCII case and a final dot. An invalid entry matches
/// nothing.
#[must_use]
pub fn browser_domain_matches(entry: &str, host: &str) -> bool {
    if !valid_browser_domain(entry) {
        return false;
    }
    let host = host.trim_end_matches('.').to_ascii_lowercase();
    let (suffix, exact) = match entry.strip_prefix("*.") {
        Some(suffix) => (suffix, false),
        None => (entry, true),
    };
    (exact && host == suffix)
        || host
            .strip_suffix(suffix)
            .is_some_and(|head| head.len() > 1 && head.ends_with('.'))
}

impl LocalWorkPolicy {
    /// Whether the person may use the browser pane at all
    /// (`local_work_browser`, and local work itself).
    #[must_use]
    pub const fn browser_pane(&self) -> bool {
        self.allowed && self.browser
    }

    /// Whether the agent is offered the browser tools: the pane exists and
    /// `local_work_browser_agent` is on.
    #[must_use]
    pub const fn browser_tools(&self) -> bool {
        self.browser_pane() && self.browser_agent
    }

    /// Why the pane may not load a page of `host` (a name or an address
    /// literal, as the URL gives it), or `None` when the domain lists admit
    /// it: a deny entry naming it, whatever the allow list says; a
    /// non-empty allow list that does not name it. An address literal is
    /// named by no domain entry, so an allow list refuses it.
    #[must_use]
    pub fn browser_domain_refusal(&self, host: &str) -> Option<String> {
        browser_domains_refusal(
            &self.browser_allow_domains,
            &self.browser_deny_domains,
            host,
        )
    }
}

/// [`LocalWorkPolicy::browser_domain_refusal`] over the two lists as the
/// browser pane's guard holds them.
#[must_use]
pub fn browser_domains_refusal(allow: &[String], deny: &[String], host: &str) -> Option<String> {
    let host = host.trim_end_matches('.').to_ascii_lowercase();
    if deny
        .iter()
        .any(|entry| browser_domain_matches(entry, &host))
    {
        return Some(format!("the policy never loads `{host}`"));
    }
    if !allow.is_empty()
        && !allow
            .iter()
            .any(|entry| browser_domain_matches(entry, &host))
    {
        return Some(format!(
            "`{host}` is not in the policy's browser_allow_domains"
        ));
    }
    None
}

#[cfg(test)]
mod tests {
    use super::{
        LocalWorkPolicy, PLATFORM_APP_DENY, SandboxMode, browser_domain_matches, bundle_id_matches,
        valid_browser_domain, valid_bundle_id_pattern,
    };

    #[test]
    fn computer_use_absent_is_off() {
        let absent: LocalWorkPolicy =
            serde_json::from_value(serde_json::json!({"allowed": true})).expect("parse");
        assert!(!absent.computer_use);
        assert_eq!(absent.computer_use_apps_allow, Vec::<String>::new());
        assert_eq!(absent.computer_use_apps_deny, Vec::<String>::new());
        assert!(!LocalWorkPolicy::default().computer_use);
        let on: LocalWorkPolicy = serde_json::from_value(serde_json::json!({
            "allowed": true,
            "computer_use": true,
            "computer_use_apps_allow": ["com.apple.mail"],
            "computer_use_apps_deny": ["com.example.*"]
        }))
        .expect("parse");
        assert!(on.computer_use);
        assert_eq!(on.computer_use_apps_allow, ["com.apple.mail"]);
        assert_eq!(on.computer_use_apps_deny, ["com.example.*"]);
    }

    #[test]
    fn bundle_id_patterns_are_ids_or_families() {
        for good in [
            "com.apple.mail",
            "ai.elitea.desktop",
            "dev.warp.*",
            "org.tabby",
            "a",
        ] {
            assert!(valid_bundle_id_pattern(good), "{good}");
        }
        for bad in [
            "",
            ".*",
            "*",
            "com..apple",
            ".com",
            "com.",
            "com apple",
            "com/apple",
            "com.*.mail",
            "com.\u{e9}",
        ] {
            assert!(!valid_bundle_id_pattern(bad), "{bad}");
        }
        assert!(!valid_bundle_id_pattern(&"a".repeat(256)));
        assert!(bundle_id_matches(
            "com.apple.Terminal",
            "com.apple.terminal"
        ));
        assert!(!bundle_id_matches(
            "com.apple.Terminal",
            "com.apple.TerminalX"
        ));
        assert!(bundle_id_matches("dev.warp.*", "dev.warp.Warp-Stable"));
        assert!(
            !bundle_id_matches("dev.warp.*", "dev.warp"),
            "a family needs a component more"
        );
        assert!(!bundle_id_matches("dev.warp.*", "dev.warpx.app"));
        assert!(!bundle_id_matches("bad pattern", "bad pattern"));
    }

    #[test]
    fn every_platform_deny_entry_is_a_valid_pattern_and_covers_the_adr_list() {
        for entry in PLATFORM_APP_DENY {
            assert!(valid_bundle_id_pattern(entry), "{entry}");
        }
        for app in [
            "ai.elitea.desktop",
            "com.apple.keychainaccess",
            "com.apple.Passwords",
            "com.1password.1password",
            "com.bitwarden.desktop",
            "com.apple.Terminal",
            "com.googlecode.iterm2",
            "dev.warp.Warp-Stable",
            "com.mitchellh.ghostty",
            "com.apple.loginwindow",
            "com.apple.Screen-Time-Settings.extension",
            "com.apple.settings.PrivacySecurity.extension",
        ] {
            assert!(
                PLATFORM_APP_DENY
                    .iter()
                    .any(|pattern| bundle_id_matches(pattern, app)),
                "{app} must be denied"
            );
        }
    }

    /// The platform list is always merged in, and an allow list never
    /// admits one of its entries; the policy's own deny wins over its allow.
    #[test]
    fn the_platform_deny_list_is_merged_and_beats_the_allow_list() {
        let policy = LocalWorkPolicy {
            computer_use: true,
            computer_use_apps_allow: vec![
                "com.apple.Terminal".to_owned(),
                "ai.elitea.desktop".to_owned(),
                "com.apple.mail".to_owned(),
                "com.example.*".to_owned(),
            ],
            computer_use_apps_deny: vec!["com.example.secret".to_owned()],
            ..LocalWorkPolicy::default()
        };
        let denied = policy.computer_use_denied_apps();
        assert_eq!(denied.len(), PLATFORM_APP_DENY.len() + 1);
        assert_eq!(
            denied.last().map(String::as_str),
            Some("com.example.secret")
        );
        assert!(
            policy
                .computer_use_refusal("com.apple.Terminal", None)
                .is_some()
        );
        assert!(
            policy
                .computer_use_refusal("ai.elitea.desktop", None)
                .is_some()
        );
        assert!(
            policy
                .computer_use_refusal("com.example.secret", None)
                .is_some()
        );
        assert_eq!(policy.computer_use_refusal("com.apple.mail", None), None);
        assert_eq!(policy.computer_use_refusal("com.example.notes", None), None);
        let outside = policy
            .computer_use_refusal("com.apple.Safari", None)
            .expect("refused");
        assert!(outside.contains("computer_use_apps_allow"), "{outside}");
        // System Settings is admitted on a known pane that is not denied;
        // with no pane (or a pane that is no bundle id) it fails closed.
        let open = LocalWorkPolicy {
            computer_use: true,
            ..LocalWorkPolicy::default()
        };
        for unknown in [None, Some(""), Some("not a pane"), Some("com.apple.*")] {
            let refused = open
                .computer_use_refusal("com.apple.systempreferences", unknown)
                .expect("refused");
            assert!(
                refused.contains("pane it shows is known"),
                "{unknown:?}: {refused}"
            );
        }
        assert!(
            open.computer_use_refusal("com.apple.SystemPreferences", None)
                .is_some(),
            "bundle ids compare ignoring case"
        );
        assert_eq!(
            open.computer_use_refusal(
                "com.apple.systempreferences",
                Some("com.apple.Displays-Settings.extension")
            ),
            None
        );
        assert!(
            open.computer_use_refusal(
                "com.apple.systempreferences",
                Some("com.apple.settings.PrivacySecurity.extension")
            )
            .is_some()
        );
        assert!(
            open.computer_use_refusal("dev.warp.*", None).is_some(),
            "a family is no app"
        );
        assert!(open.computer_use_refusal("", None).is_some());
    }

    #[test]
    fn defaults_fail_closed_and_unknown_fields_are_ignored() {
        let policy: LocalWorkPolicy = serde_json::from_value(serde_json::json!({
            "allowed": true,
            "max_sandbox_mode": "full-access",
            "cloud_sync": true,
            "local_index": false
        }))
        .expect("parse");
        assert!(policy.allowed);
        assert!(!policy.shell);
        assert!(!policy.network);
        assert_eq!(policy.max_sandbox_mode, SandboxMode::FullAccess);
        assert!(!policy.local_index, "sent as false");
        assert!(!LocalWorkPolicy::default().allowed);
        assert!(
            !LocalWorkPolicy::default().local_index,
            "absent is off, like every other field"
        );
        let on: LocalWorkPolicy =
            serde_json::from_value(serde_json::json!({"allowed": true, "local_index": true}))
                .expect("parse");
        assert!(on.local_index);
        assert!(!on.cloud_handoff, "absent is off");
        let handoff: LocalWorkPolicy =
            serde_json::from_value(serde_json::json!({"allowed": true, "cloud_handoff": true}))
                .expect("parse");
        assert!(handoff.cloud_handoff);
        assert!(!handoff.index_local_embeddings && !handoff.index_remote_embeddings);
        let embeddings: LocalWorkPolicy = serde_json::from_value(serde_json::json!({
            "allowed": true, "index_local_embeddings": true, "index_remote_embeddings": true
        }))
        .expect("parse");
        assert!(embeddings.index_local_embeddings && embeddings.index_remote_embeddings);
        assert_eq!(embeddings.index_model_mirror_url, "");
        let mirror: LocalWorkPolicy = serde_json::from_value(serde_json::json!({
            "index_model_mirror_url": "https://mirror.example.com/hf"
        }))
        .expect("parse");
        assert_eq!(
            mirror.index_model_mirror_url,
            "https://mirror.example.com/hf"
        );
        assert!(!policy.local_mcp, "absent is off");
        let mcp: LocalWorkPolicy =
            serde_json::from_value(serde_json::json!({"allowed": true, "local_mcp": true}))
                .expect("parse");
        assert!(mcp.local_mcp);
        assert!(SandboxMode::ReadOnly < SandboxMode::WorkspaceWrite);
        assert!(SandboxMode::WorkspaceWrite < SandboxMode::FullAccess);
    }

    #[test]
    fn web_and_host_intel_absent_are_off() {
        let absent: LocalWorkPolicy =
            serde_json::from_value(serde_json::json!({"allowed": true})).unwrap();
        assert!(!absent.web && !absent.host_intel);
        assert!(!LocalWorkPolicy::default().web);
        let on: LocalWorkPolicy = serde_json::from_value(
            serde_json::json!({"allowed": true, "web": true, "host_intel": true}),
        )
        .unwrap();
        assert!(on.web && on.host_intel);
    }

    #[test]
    fn memory_write_absent_is_off() {
        let absent: LocalWorkPolicy =
            serde_json::from_value(serde_json::json!({"allowed": true})).expect("parse");
        assert!(!absent.memory_write);
        assert!(!LocalWorkPolicy::default().memory_write);
        let on: LocalWorkPolicy =
            serde_json::from_value(serde_json::json!({"allowed": true, "memory_write": true}))
                .expect("parse");
        assert!(on.memory_write);
    }

    #[test]
    fn browser_absent_is_off_and_the_switches_need_local_work() {
        let absent: LocalWorkPolicy =
            serde_json::from_value(serde_json::json!({"allowed": true})).expect("parse");
        assert!(!absent.browser && !absent.browser_agent);
        assert!(!absent.browser_http && !absent.browser_private_network);
        assert!(absent.browser_allow_domains.is_empty() && absent.browser_deny_domains.is_empty());
        let on: LocalWorkPolicy = serde_json::from_value(serde_json::json!({
            "allowed": true, "browser": true, "browser_agent": true,
            "browser_allow_domains": ["example.com"], "browser_deny_domains": ["*.ads.example.com"],
            "browser_http": true, "browser_private_network": true
        }))
        .expect("parse");
        assert!(on.browser_pane() && on.browser_tools());
        assert!(on.browser_http && on.browser_private_network);
        let person_only = LocalWorkPolicy {
            browser_agent: false,
            ..on.clone()
        };
        assert!(person_only.browser_pane() && !person_only.browser_tools());
        let no_local_work = LocalWorkPolicy {
            allowed: false,
            ..on
        };
        assert!(!no_local_work.browser_pane() && !no_local_work.browser_tools());
    }

    #[test]
    fn browser_domains_are_names_or_wildcards() {
        for good in ["example.com", "*.example.com", "a-b.example.co.uk", "x1.io"] {
            assert!(valid_browser_domain(good), "{good}");
        }
        for bad in [
            "",
            "example",
            "localhost",
            "*",
            "*.com.",
            "Example.com",
            "example.com:443",
            "https://example.com",
            "10.0.0.1",
            "1.2.3.4.5",
            "exa mple.com",
            "-x.example.com",
            "x-.example.com",
            "a..b",
            "*.*.example.com",
        ] {
            assert!(!valid_browser_domain(bad), "{bad}");
        }
        assert!(browser_domain_matches("example.com", "example.com"));
        assert!(browser_domain_matches("example.com", "Docs.Example.COM."));
        assert!(!browser_domain_matches("example.com", "badexample.com"));
        assert!(!browser_domain_matches("*.example.com", "example.com"));
        assert!(browser_domain_matches("*.example.com", "a.example.com"));
        assert!(!browser_domain_matches("*.example.com", ".example.com"));
        assert!(!browser_domain_matches("bad entry", "bad entry"));
    }

    #[test]
    fn a_browser_deny_wins_and_an_allow_list_is_a_ceiling() {
        let open = LocalWorkPolicy {
            allowed: true,
            browser: true,
            browser_deny_domains: vec!["ads.example.com".to_owned()],
            ..LocalWorkPolicy::default()
        };
        assert!(open.browser_domain_refusal("example.org").is_none());
        assert!(open.browser_domain_refusal("x.ads.example.com").is_some());
        let ceiling = LocalWorkPolicy {
            browser_allow_domains: vec!["example.com".to_owned()],
            browser_deny_domains: vec!["*.secret.example.com".to_owned()],
            ..open
        };
        assert!(ceiling.browser_domain_refusal("docs.example.com").is_none());
        assert!(
            ceiling
                .browser_domain_refusal("a.secret.example.com")
                .is_some()
        );
        let outside = ceiling.browser_domain_refusal("example.org").unwrap();
        assert!(outside.contains("browser_allow_domains"), "{outside}");
        assert!(
            ceiling.browser_domain_refusal("10.0.0.1").is_some(),
            "an address literal is outside every allow list"
        );
    }
}
