//! The browser pane (ADR-0048 decisions 1 and 2): one page per Local work
//! thread, a CHILD webview of the main window laid over the pane's page
//! rectangle, which the UI reports (IPC.md, "Browser pane").
//!
//! The page is remote content, so it must never reach the host:
//!
//! - **No IPC.** Its label, `browser-<thread>`, is in no capability file:
//!   `capabilities/default.json` names the `main` WEBVIEW (not the window,
//!   which would grant every webview inside it), and its permissions are for
//!   the bundled origin only. Tauri still injects its IPC script into every
//!   webview; each call the page makes is refused by the ACL.
//! - **The navigation guard** ([`check`]): `https`, `http` only with the
//!   host setting `browser_allow_http` (#1590 replaces it with policy),
//!   `about:blank`/`about:srcdoc` and `blob:` of an allowed origin; every
//!   other scheme (`file:`, `tauri:`, `asset:`, `javascript:`, `data:`, …)
//!   is refused, and so are loopback, private and forbidden addresses (the
//!   classification of `libs/rust/egress-guard`) and the names that are
//!   private by definition (`localhost`, `.local`, …). wry calls the guard
//!   for frames as well as the top level, so the rule is the same for both.
//!
//!   **Known gap (#1634).** The guard sees navigations only, and judges
//!   the URL's host as written. A public page can still make SUBRESOURCE
//!   loads (images, scripts, `fetch`/XHR, WebSockets) to loopback and
//!   private addresses, and a public name that resolves to a private
//!   address (DNS rebinding, or a record that simply points inside) passes.
//!   On macOS the fix is a `WKContentRuleList` blocking those hosts for
//!   every load ([`content_rules`] builds it, tested); installing it needs
//!   WebKit calls that are `unsafe` in objc2-web-kit, which this crate
//!   forbids, so it waits on the follow-up. Resolving names in the guard
//!   would be racy (the page's own lookup can differ), so the follow-up
//!   does not do that either: the residual DNS gap stays documented.
//! - **A new window** (`target=_blank`, `window.open`) is never a window:
//!   an allowed URL replaces the page (the opener stays in its history).
//! - **An isolated store per thread.** macOS 14+: a
//!   `WKWebsiteDataStore(forIdentifier:)` whose identifier is kept in
//!   `<data>/browser/stores.json` ([`StoreRegistry`]); macOS 12 and 13 (and
//!   Windows and Linux until their delivery): a non-persistent store. It is
//!   deleted by Forget site data, the thread's delete or archive, and
//!   sign-out; a deletion the OS refused is retried at the next launch.
//! - **No permissions.** Every permission request (camera, microphone,
//!   geolocation, notifications, …) is denied; no autofill; Tauri's own
//!   drag-and-drop handler is off, so a drop reaches the page as a plain
//!   HTML drop and never becomes a host event.
//! - **Downloads** go to `~/Downloads/Elitea/<thread>/`, without a prompt
//!   (owner-accepted, ADR-0048), and are reported on `browser://event`,
//!   which the UI turns into a transcript card. On macOS every finished
//!   download is QUARANTINED (`com.apple.quarantine`, so Gatekeeper checks
//!   it on first open, as for a Safari download) and records where it came
//!   from (`com.apple.metadata:kMDItemWhereFroms`). A download that cannot
//!   be quarantined is deleted and reported as failed.
//! - **History buttons** (Back, Forward, Stop) run `history.back()` and
//!   friends in the page, because the native `goBack`/`stopLoading` calls
//!   need `unsafe` (same follow-up, #1634). A page can override them, so they are a
//!   convenience, not a control: the address field and Close are host-side
//!   and always work. Reload is native (`webview.reload()`).
//!
//! Tauri's child webviews are behind its `unstable` feature: this module is
//! the one place that uses them.

use std::collections::{BTreeMap, HashMap, VecDeque};
use std::net::IpAddr;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, MutexGuard};

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tauri::webview::{
    DownloadEvent, NewWindowResponse, PageLoadEvent, PermissionResponse, WebviewBuilder,
};
use tauri::{AppHandle, Emitter as _, Manager as _, State, Webview, WebviewUrl, Wry};
use url::{Host, Url};

use elitea_egress_guard::{Class, classify};

use crate::local_commands::IpcError;
use crate::settings::SettingsFiles;

/// The event every pane reports on, to the `main` webview only.
pub const EVENT_NAME: &str = "browser://event";

/// Panes kept alive (hidden) at once; the least recently shown one closes.
pub const MAX_LIVE_PANES: usize = 4;

/// How long a closed pane's webview is given to let go of its store before
/// the store is deleted (WebKit refuses to delete a store in use).
const RELEASE_WAIT: std::time::Duration = std::time::Duration::from_millis(400);

/// How long after launch the left-over stores are cleaned up.
const LAUNCH_CLEANUP_AFTER: std::time::Duration = std::time::Duration::from_secs(5);

/// What the guard lets the page load beyond public `https` (ADR-0048
/// decision 8 makes both policy keys, #1590).
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct GuardPolicy {
    pub allow_http: bool,
    pub allow_private_network: bool,
}

/// Why the guard refused a URL: also the `reason` of a `refused` event.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Refusal {
    /// Not a web page: `file:`, `tauri:`, `asset:`, `javascript:`, `data:`, …
    Scheme,
    /// Plain `http` while the setting is off.
    Http,
    /// Loopback, a private range or a name private by definition.
    PrivateNetwork,
    /// Never loaded: link-local (the metadata endpoints), multicast, reserved.
    Forbidden,
    /// A user name or password in the URL (`https://bank.example@evil.example`).
    Credentials,
}

impl Refusal {
    #[must_use]
    pub const fn message(self) -> &'static str {
        match self {
            Self::Scheme => "The browser pane opens web pages (https) only.",
            Self::Http => "Plain http pages are off. Use https.",
            Self::PrivateNetwork => {
                "The browser pane does not open local or private network addresses."
            }
            Self::Forbidden => "That address is never opened.",
            Self::Credentials => "Addresses with a user name or password are not opened.",
        }
    }
}

/// Host names that name this computer or a private network by definition.
const PRIVATE_SUFFIXES: &[&str] = &[".localhost", ".local", ".internal", ".home.arpa"];

fn host_check(host: Option<Host<&str>>, policy: GuardPolicy) -> Result<(), Refusal> {
    let address = match host {
        None => return Err(Refusal::Scheme),
        Some(Host::Ipv4(v4)) => IpAddr::V4(v4),
        Some(Host::Ipv6(v6)) => IpAddr::V6(v6),
        Some(Host::Domain(name)) => {
            let name = name.trim_end_matches('.').to_ascii_lowercase();
            let private =
                name == "localhost" || PRIVATE_SUFFIXES.iter().any(|suffix| name.ends_with(suffix));
            return if private && !policy.allow_private_network {
                Err(Refusal::PrivateNetwork)
            } else {
                Ok(())
            };
        }
    };
    match classify(address) {
        Class::Public => Ok(()),
        Class::Private if policy.allow_private_network => Ok(()),
        Class::Private => Err(Refusal::PrivateNetwork),
        Class::Forbidden => Err(Refusal::Forbidden),
    }
}

/// The navigation guard: may the pane load `url` (top level or a frame)?
pub fn check(url: &Url, policy: GuardPolicy) -> Result<(), Refusal> {
    match url.scheme() {
        "https" => {}
        "http" if policy.allow_http => {}
        "http" => return Err(Refusal::Http),
        // The empty documents a page makes for itself (frames, a new page).
        "about" if matches!(url.path(), "blank" | "srcdoc") => return Ok(()),
        // `blob:<origin>/<uuid>`: content the page made, judged by its origin.
        "blob" => {
            return match Url::parse(url.path()) {
                Ok(inner) if matches!(inner.scheme(), "https" | "http") => check(&inner, policy),
                _ => Err(Refusal::Scheme),
            };
        }
        _ => return Err(Refusal::Scheme),
    }
    if !url.username().is_empty() || url.password().is_some() {
        return Err(Refusal::Credentials);
    }
    host_check(url.host(), policy)
}

/// Schemes an address is taken as written for; anything else is a host.
const EXPLICIT_SCHEMES: &[&str] = &[
    "about:",
    "asset:",
    "blob:",
    "data:",
    "file:",
    "ftp:",
    "javascript:",
    "mailto:",
    "tauri:",
    "view-source:",
];

/// The URL for what the person typed in the address field: as written when
/// it names a scheme (`https://…`, `file:…`), else `https://` + it. `None`
/// for an empty address or one with spaces (the pane has no search).
#[must_use]
pub fn address_to_url(address: &str) -> Option<Url> {
    let text = address.trim();
    if text.is_empty() || text.len() > 8192 || text.chars().any(char::is_whitespace) {
        return None;
    }
    let lower = text.to_ascii_lowercase();
    let explicit = lower.contains("://") || EXPLICIT_SCHEMES.iter().any(|s| lower.starts_with(s));
    let url = if explicit {
        Url::parse(text).ok()?
    } else {
        Url::parse(&format!("https://{text}")).ok()?
    };
    Some(url)
}

/// The pane's webview label for a thread (its conversation id: 1–64 of
/// `[A-Za-z0-9_-]`), or `None` for anything else.
#[must_use]
pub fn pane_label(thread_id: &str) -> Option<String> {
    let valid = (1..=64).contains(&thread_id.len())
        && thread_id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_');
    valid.then(|| format!("browser-{thread_id}"))
}

/// Which data store a pane gets.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum StoreMode {
    /// `WKWebsiteDataStore(forIdentifier:)`: persistent, one per thread.
    Identified,
    /// A non-persistent store that ends with the webview.
    Ephemeral,
}

/// macOS 14 introduced identified stores. Older systems, and the other
/// OSes for now, get a non-persistent store: wry silently falls back to
/// the DEFAULT store (the main webview's) when given an identifier the OS
/// cannot honour, so the identifier is only ever passed on 14+.
#[must_use]
pub const fn store_mode(macos_major: Option<i64>) -> StoreMode {
    match macos_major {
        Some(major) if major >= 14 => StoreMode::Identified,
        _ => StoreMode::Ephemeral,
    }
}

#[cfg(target_os = "macos")]
fn macos_major() -> Option<i64> {
    let version = objc2_foundation::NSProcessInfo::processInfo().operatingSystemVersion();
    i64::try_from(version.majorVersion).ok()
}

#[cfg(not(target_os = "macos"))]
const fn macos_major() -> Option<i64> {
    None
}

/// Which store identifier belongs to which thread, and the ones still to
/// delete. Kept in `<data>/browser/stores.json`.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct StoreRegistry {
    #[serde(default)]
    threads: BTreeMap<String, uuid::Uuid>,
    #[serde(default)]
    pending_removal: Vec<uuid::Uuid>,
}

impl StoreRegistry {
    /// The thread's store identifier, made the first time it is asked for.
    pub fn identifier(&mut self, thread_id: &str) -> [u8; 16] {
        *self
            .threads
            .entry(thread_id.to_owned())
            .or_insert_with(uuid::Uuid::new_v4)
            .as_bytes()
    }

    /// The thread's store is to be deleted: it no longer belongs to the
    /// thread (the next page gets a new one) and is pending until
    /// [`Self::removed`]. `None` when the thread had none.
    pub fn forget(&mut self, thread_id: &str) -> Option<[u8; 16]> {
        let id = self.threads.remove(thread_id)?;
        if !self.pending_removal.contains(&id) {
            self.pending_removal.push(id);
        }
        Some(*id.as_bytes())
    }

    /// Every thread's store is to be deleted (sign-out).
    pub fn forget_all(&mut self) -> Vec<[u8; 16]> {
        let threads: Vec<String> = self.threads.keys().cloned().collect();
        for thread in threads {
            self.forget(&thread);
        }
        self.pending()
    }

    /// The OS deleted the store.
    pub fn removed(&mut self, id: [u8; 16]) {
        let id = uuid::Uuid::from_bytes(id);
        self.pending_removal.retain(|pending| *pending != id);
    }

    /// The stores still to delete.
    #[must_use]
    pub fn pending(&self) -> Vec<[u8; 16]> {
        self.pending_removal
            .iter()
            .map(|id| *id.as_bytes())
            .collect()
    }

    /// A store this registry hands out or still has to delete.
    #[must_use]
    pub fn knows(&self, id: [u8; 16]) -> bool {
        let id = uuid::Uuid::from_bytes(id);
        self.threads.values().any(|known| *known == id) || self.pending_removal.contains(&id)
    }

    fn load(path: &Path) -> Self {
        std::fs::read_to_string(path)
            .ok()
            .and_then(|text| serde_json::from_str(&text).ok())
            .unwrap_or_default()
    }

    fn save(&self, path: &Path) {
        let write = || -> std::io::Result<()> {
            if let Some(dir) = path.parent() {
                std::fs::create_dir_all(dir)?;
            }
            let text = serde_json::to_string_pretty(self).map_err(std::io::Error::other)?;
            let staging = path.with_extension("json.tmp");
            std::fs::write(&staging, text)?;
            std::fs::rename(staging, path)
        };
        if let Err(error) = write() {
            log::warn!(target: "browser", "could not save the browser store list: {error}");
        }
    }
}

/// The live panes, least recently shown first.
#[derive(Debug, Default)]
pub struct LivePanes(Vec<String>);

impl LivePanes {
    /// `thread_id`'s pane was shown; returns the pane to close when that
    /// leaves more than [`MAX_LIVE_PANES`] alive.
    pub fn touch(&mut self, thread_id: &str) -> Option<String> {
        self.0.retain(|known| known != thread_id);
        self.0.push(thread_id.to_owned());
        (self.0.len() > MAX_LIVE_PANES).then(|| self.0.remove(0))
    }

    pub fn remove(&mut self, thread_id: &str) {
        self.0.retain(|known| known != thread_id);
    }

    pub fn take_all(&mut self) -> Vec<String> {
        std::mem::take(&mut self.0)
    }
}

/// A file name for a download: the name the page suggested, without path
/// separators, control characters, Unicode format characters (category
/// Cf: the bidi overrides that make `invoice\u{202e}fdp.app` show as
/// `invoiceppa.pdf`, zero-width marks, the BOM) or a leading dot, at most
/// 120 characters; `download` when nothing is left.
#[must_use]
pub fn download_name(suggested: &str) -> String {
    use unicode_general_category::{GeneralCategory, get_general_category};
    let base = suggested.rsplit(['/', '\\']).next().unwrap_or_default();
    let cleaned: String = base
        .chars()
        .filter(|c| get_general_category(*c) != GeneralCategory::Format)
        .map(|c| {
            let separator = matches!(
                get_general_category(c),
                GeneralCategory::LineSeparator | GeneralCategory::ParagraphSeparator
            );
            if c.is_control() || separator || c == ':' {
                '_'
            } else {
                c
            }
        })
        .collect();
    let cleaned = cleaned.trim().trim_start_matches('.').trim();
    let mut name: String = cleaned.chars().take(120).collect();
    if name.is_empty() {
        name = "download".to_owned();
    }
    name
}

/// Downloads in flight, by (pane label, URL) → where each is written
/// (macOS does not report the path when one finishes). wry gives a
/// download no id, so two downloads of the same URL queue under one key
/// and are paired first-in, first-out: each finish takes one path, so no
/// path is lost or reported twice.
#[derive(Debug, Default)]
pub struct PendingDownloads(HashMap<(String, String), VecDeque<PathBuf>>);

impl PendingDownloads {
    pub fn start(&mut self, label: &str, url: &str, path: PathBuf) {
        self.0
            .entry((label.to_owned(), url.to_owned()))
            .or_default()
            .push_back(path);
    }

    /// The oldest download of `url` in `label`'s pane finished.
    pub fn finish(&mut self, label: &str, url: &str) -> Option<PathBuf> {
        let key = (label.to_owned(), url.to_owned());
        let queue = self.0.get_mut(&key)?;
        let path = queue.pop_front();
        if queue.is_empty() {
            self.0.remove(&key);
        }
        path
    }

    #[cfg(test)]
    fn is_empty(&self) -> bool {
        self.0.is_empty()
    }
}

/// The `com.apple.quarantine` value of a download, in the format the
/// system's own downloaders write: `<flags>;<seconds since 1970, hex>;
/// <agent>;<event UUID>`. Flags `0081`: downloaded (`0x0001`) by an app
/// that is not sandboxed (`0x0080`), as Edge and Chrome write; Gatekeeper
/// checks the file on first open. The agent, `Elitea`, is the app Finder
/// names as the downloader.
#[cfg_attr(
    not(any(target_os = "macos", test)),
    expect(dead_code, reason = "macOS only")
)]
#[must_use]
pub fn quarantine_value(unix_seconds: u64, event: uuid::Uuid) -> String {
    format!(
        "0081;{unix_seconds:08x};Elitea;{}",
        event
            .hyphenated()
            .encode_upper(&mut uuid::Uuid::encode_buffer())
    )
}

/// The `com.apple.metadata:kMDItemWhereFroms` value: a binary plist array
/// holding the download's URL (at most 2048 characters), which Finder's
/// Get Info shows as "Where from".
#[cfg(target_os = "macos")]
fn where_froms(url: &str) -> Vec<u8> {
    let url: String = url.chars().take(2048).collect();
    let value = plist::Value::Array(vec![plist::Value::String(url)]);
    let mut bytes = Vec::new();
    // Writing to a Vec cannot fail.
    let _ = value.to_writer_binary(&mut bytes);
    bytes
}

/// Mark a finished download as quarantined, and where it came from. The
/// quarantine is the part that matters: its error is returned. The origin
/// is best effort.
#[cfg(target_os = "macos")]
fn quarantine(path: &Path, url: &str) -> std::io::Result<()> {
    use rustix::fs::{XattrFlags, setxattr};
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_secs());
    let value = quarantine_value(now, uuid::Uuid::new_v4());
    setxattr(
        path,
        "com.apple.quarantine",
        value.as_bytes(),
        XattrFlags::empty(),
    )?;
    if let Err(error) = setxattr(
        path,
        "com.apple.metadata:kMDItemWhereFroms",
        &where_froms(url),
        XattrFlags::empty(),
    ) {
        log::info!(target: "browser", "a download's origin was not recorded: {error}");
    }
    Ok(())
}

/// Other systems have no quarantine attribute; their delivery (#1593)
/// decides the equivalent (Windows: the Mark of the Web).
#[cfg(not(target_os = "macos"))]
const fn quarantine(_path: &Path, _url: &str) -> std::io::Result<()> {
    Ok(())
}

/// Host names whose every load the pane's content rules block (ADR-0048
/// decision 2): the private-by-definition names of [`PRIVATE_SUFFIXES`].
const RULE_HOSTS: &[&str] = &[
    "([^/:@]*\\.)?localhost",
    "[^/:@]*\\.local",
    "[^/:@]*\\.internal",
    "[^/:@]*\\.home\\.arpa",
];

/// Address literals the content rules block, as WebKit writes them in a
/// canonical URL (IPv4 dotted decimal, IPv6 lower-case and compressed).
/// WebKit's rule regexes have no alternation (`|`) and no counted
/// repetition (`{n}`), so a range is several patterns.
const RULE_ADDRESSES: &[&str] = &[
    // 0.0.0.0/8, 127.0.0.0/8 (loopback), 10.0.0.0/8
    "0\\.[0-9]+\\.[0-9]+\\.[0-9]+",
    "127\\.[0-9]+\\.[0-9]+\\.[0-9]+",
    "10\\.[0-9]+\\.[0-9]+\\.[0-9]+",
    // 172.16.0.0/12
    "172\\.1[6-9]\\.[0-9]+\\.[0-9]+",
    "172\\.2[0-9]\\.[0-9]+\\.[0-9]+",
    "172\\.3[01]\\.[0-9]+\\.[0-9]+",
    // 192.168.0.0/16, 169.254.0.0/16 (link-local: the metadata endpoints)
    "192\\.168\\.[0-9]+\\.[0-9]+",
    "169\\.254\\.[0-9]+\\.[0-9]+",
    // 100.64.0.0/10 (CGNAT, private to the egress guard)
    "100\\.6[4-9]\\.[0-9]+\\.[0-9]+",
    "100\\.[7-9][0-9]\\.[0-9]+\\.[0-9]+",
    "100\\.1[01][0-9]\\.[0-9]+\\.[0-9]+",
    "100\\.12[0-7]\\.[0-9]+\\.[0-9]+",
    // [::], [::1], IPv4-mapped [::ffff:…]
    "\\[::\\]",
    "\\[::1\\]",
    "\\[::ffff:[0-9a-f:.]*\\]",
    // fc00::/7 (unique-local), fe80::/10 (link-local)
    "\\[f[cd][0-9a-f][0-9a-f]:[0-9a-f:.]*\\]",
    "\\[fe[89ab][0-9a-f]:[0-9a-f:.]*\\]",
];

/// The `WKContentRuleList` JSON that blocks EVERY load (top level, frames,
/// subresources, `fetch`/XHR, WebSockets: no `resource-type`, so all of
/// them) to loopback, private and link-local addresses and the private
/// names, whatever the scheme. `None` when the policy allows the private
/// network. Not installed yet: see the module docs ("Known gap").
#[cfg_attr(
    not(test),
    expect(dead_code, reason = "installed by the webkit-shim follow-up, #1634")
)]
#[must_use]
pub fn content_rules(policy: GuardPolicy) -> Option<String> {
    if policy.allow_private_network {
        return None;
    }
    // Any scheme, optional user info, the host, then a port or the path
    // (a canonical http(s)/ws(s) URL always has one). An optional final
    // dot on a name (`localhost.`).
    let rules: Vec<Value> = RULE_HOSTS
        .iter()
        .map(|host| format!("{host}\\.?"))
        .chain(RULE_ADDRESSES.iter().map(|address| (*address).to_owned()))
        .map(|host| {
            json!({
                "trigger": { "url-filter": format!("^[a-z][-a-z0-9+.]*://([^/@]*@)?{host}[:/]") },
                "action": { "type": "block" },
            })
        })
        .collect();
    Some(Value::Array(rules).to_string())
}

/// `dir/name`, or `dir/name (2).ext`, … when that exists already.
#[must_use]
pub fn unique_path(dir: &Path, name: &str) -> PathBuf {
    let first = dir.join(name);
    if !first.exists() {
        return first;
    }
    let (stem, ext) = match name.rsplit_once('.') {
        Some((stem, ext)) if !stem.is_empty() => (stem, format!(".{ext}")),
        _ => (name, String::new()),
    };
    (2..10_000)
        .map(|n| dir.join(format!("{stem} ({n}){ext}")))
        .find(|candidate| !candidate.exists())
        .unwrap_or(first)
}

/// The page rectangle the UI reports, in the main page's CSS pixels.
#[derive(Clone, Copy, Debug, Deserialize, PartialEq)]
pub struct PaneRect {
    pub x: f64,
    pub y: f64,
    pub width: f64,
    pub height: f64,
}

/// The rectangle in the window's logical pixels: CSS pixels times the main
/// page's zoom (View ▸ Zoom). `None` for a rectangle that is not one.
#[must_use]
pub fn logical_bounds(rect: PaneRect, zoom: f64) -> Option<PaneRect> {
    const LIMIT: f64 = 100_000.0;
    let values = [rect.x, rect.y, rect.width, rect.height, zoom];
    if values.iter().any(|v| !v.is_finite()) || zoom <= 0.0 {
        return None;
    }
    if rect.width < 0.0 || rect.height < 0.0 {
        return None;
    }
    let scale = |v: f64| (v * zoom).clamp(-LIMIT, LIMIT);
    Some(PaneRect {
        x: scale(rect.x).max(0.0),
        y: scale(rect.y).max(0.0),
        width: scale(rect.width),
        height: scale(rect.height),
    })
}

/// The panes' host state (`app.manage`d).
pub struct BrowserPanes {
    settings: SettingsFiles,
    registry_path: PathBuf,
    downloads_root: PathBuf,
    inner: Mutex<Inner>,
}

#[derive(Default)]
struct Inner {
    registry: StoreRegistry,
    live: LivePanes,
    downloads: PendingDownloads,
}

impl BrowserPanes {
    #[must_use]
    pub fn new(config_dir: PathBuf, data_dir: &Path, downloads_root: PathBuf) -> Self {
        let registry_path = data_dir.join("browser").join("stores.json");
        let registry = StoreRegistry::load(&registry_path);
        Self {
            settings: SettingsFiles::new(config_dir),
            registry_path,
            downloads_root,
            inner: Mutex::new(Inner {
                registry,
                ..Inner::default()
            }),
        }
    }

    fn lock(&self) -> MutexGuard<'_, Inner> {
        self.inner
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    fn policy(&self) -> GuardPolicy {
        let allow_http = self
            .settings
            .settings()
            .ok()
            .and_then(|s| s.browser_allow_http)
            .unwrap_or(false);
        GuardPolicy {
            allow_http,
            allow_private_network: false,
        }
    }

    fn downloads_dir(&self, thread_id: &str) -> PathBuf {
        self.downloads_root.join(thread_id)
    }

    fn forget_store(&self, thread_id: &str) -> Option<[u8; 16]> {
        let mut inner = self.lock();
        let id = inner.registry.forget(thread_id);
        inner.registry.save(&self.registry_path);
        id
    }

    fn mark_removed(&self, id: [u8; 16]) {
        let mut inner = self.lock();
        inner.registry.removed(id);
        inner.registry.save(&self.registry_path);
    }
}

fn invalid(message: &str) -> IpcError {
    IpcError::new("browser_invalid", message)
}

fn thread_label(thread_id: &str) -> Result<String, IpcError> {
    pane_label(thread_id).ok_or_else(|| invalid("not a thread id"))
}

fn emit(app: &AppHandle, thread_id: &str, kind: &str, mut fields: Value) {
    if let Value::Object(map) = &mut fields {
        map.insert("thread_id".into(), thread_id.into());
        map.insert("kind".into(), kind.into());
    }
    if let Err(error) = app.emit_to("main", EVENT_NAME, fields) {
        log::warn!(target: "browser", "could not deliver a browser event: {error}");
    }
}

/// A URL as an event carries it: at most 2048 characters.
fn shown_url(url: &Url) -> String {
    url.as_str().chars().take(2048).collect()
}

fn refused(app: &AppHandle, thread_id: &str, url: &Url, reason: Refusal) {
    log::info!(target: "browser", "refused a {} navigation ({reason:?})", url.scheme());
    emit(
        app,
        thread_id,
        "refused",
        json!({ "url": shown_url(url), "reason": reason, "message": reason.message() }),
    );
}

fn webview_of(app: &AppHandle, thread_id: &str) -> Option<Webview> {
    app.get_webview(&pane_label(thread_id)?)
}

fn apply_bounds(webview: &Webview, app: &AppHandle, rect: PaneRect) -> Result<(), IpcError> {
    let zoom = app
        .try_state::<crate::menu::Zoom>()
        .map_or(1.0, |zoom| zoom.factor());
    let bounds = logical_bounds(rect, zoom).ok_or_else(|| invalid("not a rectangle"))?;
    webview
        .set_bounds(tauri::Rect {
            position: tauri::LogicalPosition::new(bounds.x, bounds.y).into(),
            size: tauri::LogicalSize::new(bounds.width, bounds.height).into(),
        })
        .map_err(|error| IpcError::new("browser_unavailable", error.to_string()))
}

/// The downloads of one pane: into the thread's folder, reported on the
/// event channel.
fn on_download(
    app: &AppHandle,
    panes: &Arc<BrowserPanes>,
    thread_id: &str,
    label: &str,
    event: DownloadEvent<'_>,
) -> bool {
    match event {
        DownloadEvent::Requested { url, destination } => {
            let dir = panes.downloads_dir(thread_id);
            if let Err(error) = std::fs::create_dir_all(&dir) {
                log::warn!(target: "browser", "no download folder: {error}");
                return false;
            }
            let suggested = destination
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or_default();
            let path = unique_path(&dir, &download_name(&suggested));
            let name = path
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or_default();
            path.clone_into(destination);
            panes.lock().downloads.start(label, url.as_str(), path);
            emit(app, thread_id, "download_started", json!({ "name": name }));
            true
        }
        DownloadEvent::Finished { url, path, success } => {
            let kept = panes.lock().downloads.finish(label, url.as_str());
            let Some(path) = kept.or(path) else {
                return true;
            };
            // Quarantined whenever the file is there (a failed download can
            // leave a partial one). Fail closed: no quarantine, no file.
            let mut success = success;
            if path.is_file()
                && let Err(error) = quarantine(&path, url.as_str())
            {
                log::warn!(target: "browser", "a download was not quarantined, deleted: {error}");
                let _ = std::fs::remove_file(&path);
                success = false;
            }
            let name = path
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or_default();
            let size = std::fs::metadata(&path).ok().map(|m| m.len());
            emit(
                app,
                thread_id,
                "download_finished",
                json!({ "name": name, "size": size, "success": success }),
            );
            true
        }
        _ => true,
    }
}

fn build_pane(
    app: &AppHandle,
    panes: &Arc<BrowserPanes>,
    thread_id: &str,
    url: Url,
    rect: PaneRect,
) -> Result<Webview, IpcError> {
    let label = thread_label(thread_id)?;
    let window = app
        .get_window("main")
        .ok_or_else(|| IpcError::new("browser_unavailable", "the main window is not open"))?;
    let policy = panes.policy();
    let thread = thread_id.to_owned();

    let mut builder = WebviewBuilder::<Wry>::new(&label, WebviewUrl::External(url));
    {
        let app = app.clone();
        let thread = thread.clone();
        builder = builder.on_navigation(move |url| match check(url, policy) {
            Ok(()) => true,
            Err(reason) => {
                refused(&app, &thread, url, reason);
                false
            }
        });
    }
    {
        // One page per pane: an allowed new window replaces the page, a
        // normal navigation, so the opener stays in back history.
        let app = app.clone();
        let thread = thread.clone();
        builder = builder.on_new_window(move |url, _features| {
            match check(&url, policy) {
                Ok(()) => {
                    let app = app.clone();
                    let thread = thread.clone();
                    tauri::async_runtime::spawn(async move {
                        if let Some(webview) = webview_of(&app, &thread)
                            && let Err(error) = webview.navigate(url)
                        {
                            log::warn!(target: "browser", "a new page did not open: {error}");
                        }
                    });
                }
                Err(reason) => refused(&app, &thread, &url, reason),
            }
            NewWindowResponse::Deny
        });
    }
    {
        let app = app.clone();
        let thread = thread.clone();
        builder = builder.on_page_load(move |_webview, payload| {
            let kind = match payload.event() {
                PageLoadEvent::Started => "loading",
                PageLoadEvent::Finished => "loaded",
            };
            emit(
                &app,
                &thread,
                kind,
                json!({ "url": shown_url(payload.url()) }),
            );
        });
    }
    {
        let app = app.clone();
        let thread = thread.clone();
        builder = builder.on_document_title_changed(move |_webview, title| {
            let title: String = title.chars().take(300).collect();
            emit(&app, &thread, "title", json!({ "title": title }));
        });
    }
    {
        let app = app.clone();
        let panes = panes.clone();
        let thread = thread.clone();
        let label = label.clone();
        builder = builder
            .on_download(move |_webview, event| on_download(&app, &panes, &thread, &label, event));
    }
    builder = builder
        // Camera, microphone, geolocation, notifications, … : always no.
        .on_permission_request(|_webview, _kind| PermissionResponse::Deny)
        // A drop on the page is the page's (an upload), never a host event.
        .disable_drag_drop_handler()
        .general_autofill_enabled(false)
        .zoom_hotkeys_enabled(false)
        .accept_first_mouse(true);
    #[cfg(target_os = "macos")]
    {
        builder = builder.allow_link_preview(false);
    }
    builder = match store_mode(macos_major()) {
        StoreMode::Identified => {
            let id = {
                let mut inner = panes.lock();
                let id = inner.registry.identifier(thread_id);
                inner.registry.save(&panes.registry_path);
                id
            };
            builder.data_store_identifier(id)
        }
        StoreMode::Ephemeral => builder.incognito(true),
    };

    let zoom = app
        .try_state::<crate::menu::Zoom>()
        .map_or(1.0, |zoom| zoom.factor());
    let bounds = logical_bounds(rect, zoom).ok_or_else(|| invalid("not a rectangle"))?;
    window
        .add_child(
            builder,
            tauri::LogicalPosition::new(bounds.x, bounds.y),
            tauri::LogicalSize::new(bounds.width, bounds.height),
        )
        .map_err(|error| IpcError::new("browser_unavailable", error.to_string()))
}

fn close_pane(app: &AppHandle, panes: &BrowserPanes, thread_id: &str) {
    panes.lock().live.remove(thread_id);
    if let Some(webview) = webview_of(app, thread_id)
        && let Err(error) = webview.close()
    {
        log::warn!(target: "browser", "a pane did not close: {error}");
    }
}

/// Delete the stores still pending (macOS 14+ only: older systems have
/// none, and the call does not exist there).
async fn remove_pending(app: &AppHandle, panes: &BrowserPanes) {
    if store_mode(macos_major()) != StoreMode::Identified {
        return;
    }
    #[cfg(target_vendor = "apple")]
    {
        let pending = panes.lock().registry.pending();
        for id in pending {
            match app.remove_data_store(id).await {
                Ok(()) => panes.mark_removed(id),
                Err(error) => {
                    log::warn!(target: "browser", "a browser store was not deleted yet: {error}");
                }
            }
        }
    }
    #[cfg(not(target_vendor = "apple"))]
    let _ = (app, panes);
}

/// At launch: the deletions the OS refused last time, and any identified
/// store the list does not know (the list was lost): only the browser pane
/// makes identified stores; the main webview uses the default one.
pub fn clean_up_at_launch(app: AppHandle) {
    tauri::async_runtime::spawn(async move {
        tokio::time::sleep(LAUNCH_CLEANUP_AFTER).await;
        let Some(panes) = app
            .try_state::<Arc<BrowserPanes>>()
            .map(|p| p.inner().clone())
        else {
            return;
        };
        if store_mode(macos_major()) != StoreMode::Identified {
            return;
        }
        #[cfg(target_vendor = "apple")]
        if let Ok(ids) = app.fetch_data_store_identifiers().await {
            let mut inner = panes.lock();
            let mut changed = false;
            for id in ids {
                if !inner.registry.knows(id) {
                    inner
                        .registry
                        .pending_removal
                        .push(uuid::Uuid::from_bytes(id));
                    changed = true;
                }
            }
            if changed {
                inner.registry.save(&panes.registry_path);
            }
        }
        remove_pending(&app, &panes).await;
    });
}

/// Close the thread's pane and delete its store.
async fn forget_thread(app: &AppHandle, panes: &BrowserPanes, thread_id: &str) -> bool {
    let had_pane = webview_of(app, thread_id).is_some();
    close_pane(app, panes, thread_id);
    let forgotten = panes.forget_store(thread_id).is_some();
    if forgotten {
        if had_pane {
            tokio::time::sleep(RELEASE_WAIT).await;
        }
        remove_pending(app, panes).await;
    }
    forgotten || had_pane
}

/// Sign-out (and the Doctor's): every pane closes and every store goes.
pub async fn forget_all(app: &AppHandle) {
    let Some(panes) = app
        .try_state::<Arc<BrowserPanes>>()
        .map(|p| p.inner().clone())
    else {
        return;
    };
    let live = panes.lock().live.take_all();
    let had_panes = !live.is_empty();
    for thread in live {
        close_pane(app, &panes, &thread);
    }
    {
        let mut inner = panes.lock();
        inner.registry.forget_all();
        inner.registry.save(&panes.registry_path);
    }
    if had_panes {
        tokio::time::sleep(RELEASE_WAIT).await;
    }
    remove_pending(app, &panes).await;
}

#[derive(Debug, Serialize)]
pub struct BrowserOpened {
    url: String,
}

/// Open (or re-point) the thread's pane at `address` over `rect`.
#[tauri::command(rename_all = "snake_case")]
pub async fn browser_open(
    app: AppHandle,
    panes: State<'_, Arc<BrowserPanes>>,
    thread_id: String,
    address: String,
    rect: PaneRect,
) -> Result<BrowserOpened, IpcError> {
    let panes = panes.inner().clone();
    thread_label(&thread_id)?;
    let url = address_to_url(&address).ok_or_else(|| invalid("Type a web address."))?;
    check(&url, panes.policy())
        .map_err(|reason| IpcError::new("browser_refused", reason.message()))?;
    let shown = shown_url(&url);
    if let Some(webview) = webview_of(&app, &thread_id) {
        apply_bounds(&webview, &app, rect)?;
        let _ = webview.show();
        webview
            .navigate(url)
            .map_err(|error| IpcError::new("browser_unavailable", error.to_string()))?;
    } else {
        build_pane(&app, &panes, &thread_id, url, rect)?;
    }
    let evicted = panes.lock().live.touch(&thread_id);
    if let Some(evicted) = evicted {
        close_pane(&app, &panes, &evicted);
        emit(&app, &evicted, "closed", json!({}));
    }
    Ok(BrowserOpened { url: shown })
}

/// The pane's page rectangle moved or changed size, or the page is to be
/// shown or hidden (another tab, a modal sheet). No pane: nothing to do.
#[tauri::command(rename_all = "snake_case")]
pub async fn browser_set_rect(
    app: AppHandle,
    panes: State<'_, Arc<BrowserPanes>>,
    thread_id: String,
    rect: PaneRect,
    visible: bool,
) -> Result<(), IpcError> {
    thread_label(&thread_id)?;
    let Some(webview) = webview_of(&app, &thread_id) else {
        return Ok(());
    };
    if visible {
        apply_bounds(&webview, &app, rect)?;
        let evicted = panes.lock().live.touch(&thread_id);
        if let Some(evicted) = evicted {
            close_pane(&app, &panes, &evicted);
            emit(&app, &evicted, "closed", json!({}));
        }
        webview.show()
    } else {
        webview.hide()
    }
    .map_err(|error| IpcError::new("browser_unavailable", error.to_string()))
}

/// What the toolbar's history buttons ask for.
#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum GoAction {
    Back,
    Forward,
    Reload,
    Stop,
}

impl GoAction {
    /// The script run in the page. History only: nothing is read back. The
    /// page can override these (module docs, "History buttons"); the native
    /// calls wait on the webkit-shim follow-up (#1634).
    const fn script(self) -> &'static str {
        match self {
            Self::Back => "history.back()",
            Self::Forward => "history.forward()",
            Self::Reload => "location.reload()",
            Self::Stop => "window.stop()",
        }
    }
}

#[tauri::command(rename_all = "snake_case")]
pub async fn browser_go(
    app: AppHandle,
    thread_id: String,
    action: GoAction,
) -> Result<(), IpcError> {
    thread_label(&thread_id)?;
    let webview = webview_of(&app, &thread_id)
        .ok_or_else(|| IpcError::new("browser_closed", "The page is closed."))?;
    let result = if action == GoAction::Reload {
        webview.reload()
    } else {
        webview.eval(action.script())
    };
    result.map_err(|error| IpcError::new("browser_unavailable", error.to_string()))
}

/// Close the thread's pane; its store stays (Forget site data deletes it).
#[tauri::command(rename_all = "snake_case")]
pub async fn browser_close(
    app: AppHandle,
    panes: State<'_, Arc<BrowserPanes>>,
    thread_id: String,
) -> Result<(), IpcError> {
    thread_label(&thread_id)?;
    close_pane(&app, &panes, &thread_id);
    Ok(())
}

#[derive(Debug, Serialize)]
pub struct BrowserForgotten {
    forgotten: bool,
}

/// Forget site data, or the thread was deleted or archived: the pane
/// closes and the thread's store is deleted. Downloads stay.
#[tauri::command(rename_all = "snake_case")]
pub async fn browser_forget(
    app: AppHandle,
    panes: State<'_, Arc<BrowserPanes>>,
    thread_id: String,
) -> Result<BrowserForgotten, IpcError> {
    thread_label(&thread_id)?;
    let panes = panes.inner().clone();
    Ok(BrowserForgotten {
        forgotten: forget_thread(&app, &panes, &thread_id).await,
    })
}

/// Show a download of the thread's pane in Finder / the file manager.
#[tauri::command(rename_all = "snake_case")]
pub async fn browser_reveal_download(
    app: AppHandle,
    panes: State<'_, Arc<BrowserPanes>>,
    thread_id: String,
    name: String,
) -> Result<(), IpcError> {
    use tauri_plugin_opener::OpenerExt as _;
    thread_label(&thread_id)?;
    if download_name(&name) != name {
        return Err(invalid("not a download"));
    }
    let path = panes.downloads_dir(&thread_id).join(&name);
    if !path.is_file() {
        return Err(IpcError::new(
            "not_found",
            "That download is no longer there.",
        ));
    }
    app.opener()
        .reveal_item_in_dir(&path)
        .map_err(|error| IpcError::new("os_refused", error.to_string()))
}

#[cfg(test)]
mod tests {
    use super::*;

    const STRICT: GuardPolicy = GuardPolicy {
        allow_http: false,
        allow_private_network: false,
    };
    const HTTP: GuardPolicy = GuardPolicy {
        allow_http: true,
        allow_private_network: false,
    };

    /// A row of the guard table: the URL, its verdict with the default
    /// policy, and with http on.
    type Row = (&'static str, Verdict, Verdict);
    type Verdict = Result<(), Refusal>;

    #[test]
    fn the_guard_admits_public_web_pages_only() {
        use Refusal::{Credentials, Forbidden, Http, PrivateNetwork, Scheme};
        let table: &[Row] = &[
            ("https://example.com/", Ok(()), Ok(())),
            ("https://example.com:8443/a?b#c", Ok(()), Ok(())),
            ("https://93.184.216.34/", Ok(()), Ok(())),
            ("https://[2606:4700::1111]/", Ok(()), Ok(())),
            ("http://example.com/", Err(Http), Ok(())),
            ("about:blank", Ok(()), Ok(())),
            ("about:srcdoc", Ok(()), Ok(())),
            ("about:config", Err(Scheme), Err(Scheme)),
            ("blob:https://example.com/4f1c", Ok(()), Ok(())),
            ("blob:http://example.com/4f1c", Err(Http), Ok(())),
            ("blob:null/4f1c", Err(Scheme), Err(Scheme)),
            (
                "blob:https://127.0.0.1/4f1c",
                Err(PrivateNetwork),
                Err(PrivateNetwork),
            ),
            ("file:///etc/passwd", Err(Scheme), Err(Scheme)),
            ("tauri://localhost/index.html", Err(Scheme), Err(Scheme)),
            (
                "asset://localhost/%2Fetc%2Fpasswd",
                Err(Scheme),
                Err(Scheme),
            ),
            (
                "ipc://localhost/host_access_token",
                Err(Scheme),
                Err(Scheme),
            ),
            ("javascript:alert(1)", Err(Scheme), Err(Scheme)),
            (
                "data:text/html,<script>1</script>",
                Err(Scheme),
                Err(Scheme),
            ),
            ("view-source:https://example.com/", Err(Scheme), Err(Scheme)),
            ("ftp://example.com/", Err(Scheme), Err(Scheme)),
            ("mailto:a@example.com", Err(Scheme), Err(Scheme)),
            ("http://tauri.localhost/", Err(Http), Err(PrivateNetwork)),
            (
                "https://tauri.localhost/",
                Err(PrivateNetwork),
                Err(PrivateNetwork),
            ),
            (
                "https://localhost/",
                Err(PrivateNetwork),
                Err(PrivateNetwork),
            ),
            (
                "https://LOCALHOST./",
                Err(PrivateNetwork),
                Err(PrivateNetwork),
            ),
            (
                "https://app.localhost/",
                Err(PrivateNetwork),
                Err(PrivateNetwork),
            ),
            (
                "https://printer.local/",
                Err(PrivateNetwork),
                Err(PrivateNetwork),
            ),
            (
                "https://db.internal/",
                Err(PrivateNetwork),
                Err(PrivateNetwork),
            ),
            (
                "https://nas.home.arpa/",
                Err(PrivateNetwork),
                Err(PrivateNetwork),
            ),
            (
                "https://127.0.0.1/",
                Err(PrivateNetwork),
                Err(PrivateNetwork),
            ),
            ("http://127.0.0.1:5173/", Err(Http), Err(PrivateNetwork)),
            (
                "https://2130706433/",
                Err(PrivateNetwork),
                Err(PrivateNetwork),
            ),
            ("https://0x7f.1/", Err(PrivateNetwork), Err(PrivateNetwork)),
            ("https://[::1]/", Err(PrivateNetwork), Err(PrivateNetwork)),
            (
                "https://[::ffff:127.0.0.1]/",
                Err(PrivateNetwork),
                Err(PrivateNetwork),
            ),
            (
                "https://10.0.0.8/",
                Err(PrivateNetwork),
                Err(PrivateNetwork),
            ),
            (
                "https://172.16.1.1/",
                Err(PrivateNetwork),
                Err(PrivateNetwork),
            ),
            (
                "https://192.168.1.1/",
                Err(PrivateNetwork),
                Err(PrivateNetwork),
            ),
            (
                "https://100.64.0.1/",
                Err(PrivateNetwork),
                Err(PrivateNetwork),
            ),
            (
                "https://[fd12::1]/",
                Err(PrivateNetwork),
                Err(PrivateNetwork),
            ),
            ("https://169.254.169.254/", Err(Forbidden), Err(Forbidden)),
            ("https://0.0.0.0/", Err(Forbidden), Err(Forbidden)),
            ("https://[fe80::1]/", Err(Forbidden), Err(Forbidden)),
            ("https://224.0.0.1/", Err(Forbidden), Err(Forbidden)),
            (
                "https://user@example.com/",
                Err(Credentials),
                Err(Credentials),
            ),
            (
                "https://bank.example:pw@evil.example/",
                Err(Credentials),
                Err(Credentials),
            ),
        ];
        for (raw, strict, http) in table {
            let url = Url::parse(raw).unwrap_or_else(|e| panic!("{raw}: {e}"));
            assert_eq!(check(&url, STRICT), *strict, "{raw} (default)");
            assert_eq!(check(&url, HTTP), *http, "{raw} (http on)");
        }
    }

    #[test]
    fn the_private_network_policy_lifts_private_but_never_forbidden() {
        let open = GuardPolicy {
            allow_http: true,
            allow_private_network: true,
        };
        for raw in [
            "http://localhost:3000/",
            "https://192.168.1.1/",
            "http://[::1]/",
        ] {
            assert_eq!(check(&Url::parse(raw).unwrap(), open), Ok(()), "{raw}");
        }
        for raw in ["http://169.254.169.254/latest/", "file:///etc/hosts"] {
            assert!(check(&Url::parse(raw).unwrap(), open).is_err(), "{raw}");
        }
    }

    #[test]
    fn an_address_is_https_unless_it_names_its_scheme() {
        let url = |a: &str| address_to_url(a).map(|u| u.to_string());
        assert_eq!(url("example.com").as_deref(), Some("https://example.com/"));
        assert_eq!(
            url("  example.com/docs?q=1 ").as_deref(),
            Some("https://example.com/docs?q=1")
        );
        assert_eq!(
            url("example.com:8080").as_deref(),
            Some("https://example.com:8080/")
        );
        assert_eq!(
            url("http://example.com").as_deref(),
            Some("http://example.com/")
        );
        assert_eq!(
            url("FILE:///etc/passwd").as_deref(),
            Some("file:///etc/passwd")
        );
        assert_eq!(
            url("javascript:alert(1)").as_deref(),
            Some("javascript:alert(1)")
        );
        assert_eq!(url(""), None);
        assert_eq!(url("two words"), None);
        // Whatever it becomes, the guard has the last word.
        for typed in [
            "file:///etc/passwd",
            "javascript:alert(1)",
            "data:text/html,x",
            "localhost:5173",
        ] {
            let parsed = address_to_url(typed).unwrap();
            assert!(check(&parsed, STRICT).is_err(), "{typed}");
        }
    }

    #[test]
    fn a_pane_is_named_after_its_thread() {
        assert_eq!(pane_label("1234").as_deref(), Some("browser-1234"));
        assert_eq!(
            pane_label("9f1c2d3e-aaaa-bbbb-cccc-123456789abc").as_deref(),
            Some("browser-9f1c2d3e-aaaa-bbbb-cccc-123456789abc")
        );
        for bad in ["", "../main", "main thread", "a/b", "a:b", &"x".repeat(65)] {
            assert_eq!(pane_label(bad), None, "{bad}");
        }
        // Never a label a capability names.
        assert_ne!(pane_label("main").as_deref(), Some("main"));
    }

    #[test]
    fn only_macos_14_and_later_get_an_identified_store() {
        assert_eq!(store_mode(Some(15)), StoreMode::Identified);
        assert_eq!(store_mode(Some(14)), StoreMode::Identified);
        assert_eq!(store_mode(Some(13)), StoreMode::Ephemeral);
        assert_eq!(store_mode(Some(12)), StoreMode::Ephemeral);
        assert_eq!(store_mode(None), StoreMode::Ephemeral);
    }

    #[test]
    fn a_threads_store_is_stable_until_forgotten_then_pending_until_removed() {
        let mut registry = StoreRegistry::default();
        let a = registry.identifier("a");
        assert_eq!(registry.identifier("a"), a, "the same store every time");
        let b = registry.identifier("b");
        assert_ne!(a, b, "one store per thread");
        assert!(registry.knows(a) && registry.knows(b));

        assert_eq!(registry.forget("a"), Some(a));
        assert_eq!(registry.forget("a"), None, "forgotten once");
        assert_eq!(registry.pending(), vec![a]);
        assert!(registry.knows(a), "still known until the OS deleted it");
        let again = registry.identifier("a");
        assert_ne!(again, a, "the next page starts with a fresh store");

        registry.removed(a);
        assert!(registry.pending().is_empty());
        assert!(!registry.knows(a));
        assert!(registry.knows(again) && registry.knows(b));
    }

    #[test]
    fn sign_out_forgets_every_store_and_a_refused_deletion_survives_a_restart() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("browser").join("stores.json");
        let mut registry = StoreRegistry::default();
        let a = registry.identifier("a");
        let b = registry.identifier("b");
        let mut pending = registry.forget_all();
        pending.sort_unstable();
        let mut expected = vec![a, b];
        expected.sort_unstable();
        assert_eq!(pending, expected);
        registry.removed(a);
        registry.save(&path);

        let reloaded = StoreRegistry::load(&path);
        assert_eq!(reloaded, registry);
        assert_eq!(reloaded.pending(), vec![b], "retried at the next launch");
        assert!(!reloaded.knows(a));
        // A missing or corrupt file reads as empty, never an error.
        std::fs::write(&path, "{not json").unwrap();
        assert_eq!(StoreRegistry::load(&path), StoreRegistry::default());
        assert_eq!(
            StoreRegistry::load(&dir.path().join("none.json")),
            StoreRegistry::default()
        );
    }

    #[test]
    fn at_most_four_panes_stay_alive() {
        let mut live = LivePanes::default();
        for thread in ["a", "b", "c", "d"] {
            assert_eq!(live.touch(thread), None);
        }
        assert_eq!(live.touch("a"), None, "shown again: most recent");
        assert_eq!(
            live.touch("e").as_deref(),
            Some("b"),
            "the least recently shown closes"
        );
        live.remove("c");
        assert_eq!(live.touch("f"), None);
        let mut all = live.take_all();
        all.sort_unstable();
        assert_eq!(all, ["a", "d", "e", "f"]);
    }

    #[test]
    fn a_download_gets_a_plain_unique_name_in_the_threads_folder() {
        assert_eq!(download_name("report.pdf"), "report.pdf");
        assert_eq!(
            download_name("../../.ssh/authorized_keys"),
            "authorized_keys"
        );
        assert_eq!(download_name("..\\evil.exe"), "evil.exe");
        assert_eq!(download_name(".bashrc"), "bashrc");
        assert_eq!(download_name("a\u{0}b\nc:d"), "a_b_c_d");
        assert_eq!(download_name(""), "download");
        assert_eq!(download_name("..."), "download");
        assert_eq!(download_name(&"x".repeat(500)).chars().count(), 120);
        // Unicode format characters (Cf) go: the right-to-left override that
        // shows `invoice\u{202e}fdp.app` as `invoiceppa.pdf`, the other bidi
        // embeddings and isolates, zero-width marks, the BOM.
        assert_eq!(download_name("invoice\u{202e}fdp.app"), "invoicefdp.app");
        for c in [
            '\u{200b}', '\u{200c}', '\u{200d}', '\u{200e}', '\u{200f}', '\u{202a}', '\u{202b}',
            '\u{202c}', '\u{202d}', '\u{202e}', '\u{2066}', '\u{2067}', '\u{2068}', '\u{2069}',
            '\u{feff}', '\u{00ad}', '\u{061c}',
        ] {
            assert_eq!(
                download_name(&format!("a{c}b.pdf")),
                "ab.pdf",
                "U+{:04X}",
                u32::from(c)
            );
        }
        assert_eq!(download_name("\u{feff}.\u{202e}bashrc"), "bashrc");
        assert_eq!(download_name("\u{202e}\u{200b}"), "download");
        assert_eq!(download_name("a\u{2028}b\u{2029}c"), "a_b_c");
        // Letters and marks of other scripts stay.
        assert_eq!(download_name("отчёт-報告.pdf"), "отчёт-報告.pdf");

        let dir = tempfile::tempdir().unwrap();
        assert_eq!(unique_path(dir.path(), "a.pdf"), dir.path().join("a.pdf"));
        std::fs::write(dir.path().join("a.pdf"), "").unwrap();
        assert_eq!(
            unique_path(dir.path(), "a.pdf"),
            dir.path().join("a (2).pdf")
        );
        std::fs::write(dir.path().join("a (2).pdf"), "").unwrap();
        assert_eq!(
            unique_path(dir.path(), "a.pdf"),
            dir.path().join("a (3).pdf")
        );
        std::fs::write(dir.path().join("notes"), "").unwrap();
        assert_eq!(
            unique_path(dir.path(), "notes"),
            dir.path().join("notes (2)")
        );
    }

    #[test]
    fn downloads_of_the_same_url_pair_first_in_first_out() {
        let mut pending = PendingDownloads::default();
        let url = "https://example.com/report.pdf";
        pending.start("browser-a", url, PathBuf::from("/d/report.pdf"));
        pending.start("browser-a", url, PathBuf::from("/d/report (2).pdf"));
        pending.start("browser-b", url, PathBuf::from("/e/report.pdf"));
        assert_eq!(
            pending.finish("browser-a", url),
            Some(PathBuf::from("/d/report.pdf"))
        );
        assert_eq!(
            pending.finish("browser-a", url),
            Some(PathBuf::from("/d/report (2).pdf")),
            "the second download keeps its own path"
        );
        assert_eq!(pending.finish("browser-a", url), None, "each path once");
        assert_eq!(
            pending.finish("browser-a", "https://example.com/other"),
            None
        );
        assert_eq!(
            pending.finish("browser-b", url),
            Some(PathBuf::from("/e/report.pdf")),
            "another pane's download is its own"
        );
        assert!(pending.is_empty(), "a finished key is dropped");
    }

    #[test]
    fn a_download_is_quarantined_as_the_system_downloaders_do() {
        let event = uuid::Uuid::parse_str("89387f8c-3c2e-4230-8d57-23c6e2bdcba6").unwrap();
        assert_eq!(
            quarantine_value(0x6aea_4fc9, event),
            "0081;6aea4fc9;Elitea;89387F8C-3C2E-4230-8D57-23C6E2BDCBA6"
        );
        assert_eq!(
            quarantine_value(5, event),
            "0081;00000005;Elitea;89387F8C-3C2E-4230-8D57-23C6E2BDCBA6",
            "the time is eight hex digits"
        );
    }

    #[cfg(target_os = "macos")]
    #[test]
    fn a_finished_download_carries_its_quarantine_and_origin() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("report.pdf");
        std::fs::write(&path, "%PDF").unwrap();
        quarantine(&path, "https://example.com/report.pdf").unwrap();

        let mut buffer = vec![0_u8; 512];
        let n = rustix::fs::getxattr(&path, "com.apple.quarantine", &mut buffer[..]).unwrap();
        let value = std::str::from_utf8(&buffer[..n]).unwrap();
        let fields: Vec<&str> = value.split(';').collect();
        assert_eq!(fields.len(), 4, "{value}");
        assert_eq!(fields[0], "0081");
        assert_eq!(fields[2], "Elitea");
        assert!(uuid::Uuid::parse_str(fields[3]).is_ok(), "{value}");

        let n = rustix::fs::getxattr(
            &path,
            "com.apple.metadata:kMDItemWhereFroms",
            &mut buffer[..],
        )
        .unwrap();
        let froms = plist::Value::from_reader(std::io::Cursor::new(&buffer[..n])).unwrap();
        assert_eq!(
            froms,
            plist::Value::Array(vec![plist::Value::String(
                "https://example.com/report.pdf".into()
            )])
        );
        assert!(
            quarantine(&dir.path().join("gone"), "https://example.com/").is_err(),
            "no file: an error, so the caller fails closed"
        );
    }

    /// Does any rule block `url`? WebKit's rule regexes are a subset of
    /// these (no `|`, no `{n}`), matched case-insensitively.
    fn blocked(rules: &str, url: &str) -> bool {
        let rules: Vec<Value> = serde_json::from_str(rules).unwrap();
        rules.iter().any(|rule| {
            let filter = rule["trigger"]["url-filter"].as_str().unwrap();
            regex::RegexBuilder::new(filter)
                .case_insensitive(true)
                .build()
                .unwrap()
                .is_match(url)
        })
    }

    #[test]
    fn the_content_rules_block_every_private_load() {
        let rules = content_rules(STRICT).unwrap();
        let parsed: Vec<Value> = serde_json::from_str(&rules).unwrap();
        for rule in &parsed {
            assert_eq!(rule["action"]["type"], "block");
            assert!(
                rule["trigger"].get("resource-type").is_none(),
                "every kind of load"
            );
            let filter = rule["trigger"]["url-filter"].as_str().unwrap();
            assert!(
                !filter.contains('|') && !filter.contains('{'),
                "WebKit has no alternation or counted repetition: {filter}"
            );
        }
        for url in [
            "http://localhost/",
            "https://localhost:5173/api",
            "ws://localhost/socket",
            "wss://LOCALHOST./x",
            "https://tauri.localhost/index.html",
            "https://user@app.localhost/",
            "https://printer.local/",
            "https://db.internal:8443/",
            "https://nas.home.arpa/",
            "http://127.0.0.1/",
            "http://127.255.0.9:9400/healthz",
            "http://0.0.0.0/",
            "http://10.1.2.3/",
            "http://172.16.0.1/",
            "http://172.24.9.9/",
            "http://172.31.255.255/",
            "http://192.168.1.1/",
            "http://169.254.169.254/latest/meta-data/",
            "http://100.64.0.1/",
            "http://100.127.255.255/",
            "http://[::1]/",
            "http://[::]/",
            "http://[::ffff:7f00:1]/",
            "http://[fc00::1]/",
            "http://[fd12:3456::1]:8080/",
            "http://[fe80::1]/",
        ] {
            assert!(blocked(&rules, url), "{url} must be blocked");
        }
        for url in [
            "https://example.com/",
            "https://localhost.example.com/",
            "https://example.local.dev/",
            "https://mylocalhost/",
            "https://93.184.216.34/",
            "https://172.15.0.1/",
            "https://172.32.0.1/",
            "https://100.63.0.1/",
            "https://100.128.0.1/",
            "https://110.0.0.1/",
            "https://192.169.0.1/",
            "https://[2606:4700::1111]/",
            "https://example.com/?next=http://127.0.0.1/",
            "https://example.com/http://localhost/",
        ] {
            assert!(!blocked(&rules, url), "{url} must load");
        }
        assert_eq!(
            content_rules(GuardPolicy {
                allow_http: true,
                allow_private_network: true,
            }),
            None
        );
    }

    #[test]
    fn the_page_rectangle_follows_the_main_page_zoom() {
        let rect = PaneRect {
            x: 600.0,
            y: 80.0,
            width: 400.0,
            height: 500.0,
        };
        assert_eq!(logical_bounds(rect, 1.0), Some(rect));
        assert_eq!(
            logical_bounds(rect, 1.25),
            Some(PaneRect {
                x: 750.0,
                y: 100.0,
                width: 500.0,
                height: 625.0
            })
        );
        let off = PaneRect { x: -5.0, ..rect };
        assert_eq!(logical_bounds(off, 1.0).map(|r| r.x), Some(0.0));
        assert_eq!(
            logical_bounds(
                PaneRect {
                    width: -1.0,
                    ..rect
                },
                1.0
            ),
            None
        );
        assert_eq!(
            logical_bounds(
                PaneRect {
                    x: f64::NAN,
                    ..rect
                },
                1.0
            ),
            None
        );
        assert_eq!(logical_bounds(rect, 0.0), None);
    }

    #[test]
    fn the_history_buttons_run_history_only() {
        assert_eq!(GoAction::Back.script(), "history.back()");
        assert_eq!(GoAction::Forward.script(), "history.forward()");
        assert_eq!(GoAction::Stop.script(), "window.stop()");
        let parsed: GoAction = serde_json::from_str("\"forward\"").unwrap();
        assert_eq!(parsed, GoAction::Forward);
        assert!(serde_json::from_str::<GoAction>("\"eval\"").is_err());
    }
}
