//! The security-relevant parts of the static configuration, read from the
//! files Tauri reads: the CSP in `tauri.conf.json` and the opener scope in
//! `capabilities/default.json`.

use serde_json::Value;

fn json(path: &str) -> Value {
    let text = std::fs::read_to_string(format!("{}/{path}", env!("CARGO_MANIFEST_DIR")))
        .expect("config file");
    serde_json::from_str(&text).expect("valid json")
}

fn opener_scope(key: &str) -> Vec<glob::Pattern> {
    let capability = json("capabilities/default.json");
    let opener = capability["permissions"]
        .as_array()
        .unwrap()
        .iter()
        .find(|p| p["identifier"] == "opener:allow-open-url")
        .expect("opener permission");
    opener[key]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| glob::Pattern::new(e["url"].as_str().unwrap()).unwrap())
        .collect()
}

/// The plugin's rule: allowed when no deny entry matches and an allow entry does.
fn opener_allows(url: &str) -> bool {
    !opener_scope("deny").iter().any(|p| p.matches(url))
        && opener_scope("allow").iter().any(|p| p.matches(url))
}

#[test]
fn the_opener_takes_https_and_loopback_http_only() {
    for url in [
        "https://docs.example.com/guide",
        "http://localhost:5173/x",
        "http://localhost/x",
        "http://127.0.0.1:8080/",
        "http://[::1]:8080/",
    ] {
        assert!(opener_allows(url), "{url}");
    }
    for url in [
        "http://elitea.example.com/",
        "http://localhost.evil.example/",
        "http://localhost:80@evil.example/",
        "http://127.0.0.1:1@evil.example/",
        "file:///etc/passwd",
        "javascript:alert(1)",
        "ftp://example.com/",
    ] {
        assert!(!opener_allows(url), "{url}");
    }
}

#[test]
fn images_and_media_load_from_self_data_and_blob_only() {
    let config = json("tauri.conf.json");
    let csp = &config["app"]["security"]["csp"];
    assert_eq!(csp["img-src"], "'self' data: blob:");
    assert_eq!(csp["media-src"], csp["img-src"]);
    assert_eq!(csp["script-src"], "'self'");
}

/// MUI (emotion) injects `<style>` elements at runtime, which only
/// `'unsafe-inline'` admits. Tauri hashes the bundled page's inline style
/// into `style-src` unless told not to, and a hash in the directive makes
/// the webview ignore `'unsafe-inline'` — every component then renders
/// unstyled. Scripts keep Tauri's hashes and nonces.
#[test]
fn runtime_styles_are_not_switched_off_by_asset_hashes() {
    let conf = json("tauri.conf.json");
    let security = &conf["app"]["security"];
    assert_eq!(
        security["dangerousDisableAssetCspModification"],
        serde_json::json!(["style-src"])
    );
    let style = security["csp"]["style-src"].as_str().expect("style-src");
    assert!(style.split_whitespace().any(|s| s == "'unsafe-inline'"));
}

fn granted() -> Vec<String> {
    json("capabilities/default.json")["permissions"]
        .as_array()
        .unwrap()
        .iter()
        .map(|p| {
            p.as_str().map_or_else(
                || p["identifier"].as_str().unwrap().to_owned(),
                str::to_owned,
            )
        })
        .collect()
}

/// The webview may listen to host events (never emit them), and gets the
/// two window permissions `data-tauri-drag-region` needs (drag;
/// double-click to zoom) plus `set-theme` (the window's appearance follows
/// the app's palette mode), nothing else of the window or of the
/// Rust-driven plugins (notification, window-state): a page cannot post
/// notifications, move the window or rewrite its saved state. Of the log
/// plugin it gets the one write command, never a default set.
#[test]
fn the_webview_gets_only_the_window_and_log_permissions_it_needs() {
    let granted = granted();
    let core: Vec<&str> = granted
        .iter()
        .map(String::as_str)
        .filter(|p| p.starts_with("core:"))
        .collect();
    assert_eq!(
        core,
        [
            "core:event:allow-listen",
            "core:event:allow-unlisten",
            "core:window:allow-start-dragging",
            "core:window:allow-internal-toggle-maximize",
            "core:window:allow-set-theme"
        ]
    );
    let log: Vec<&str> = granted
        .iter()
        .map(String::as_str)
        .filter(|p| p.starts_with("log:"))
        .collect();
    assert_eq!(log, ["log:allow-log"]);
    for p in &granted {
        assert!(
            !p.starts_with("notification:") && !p.starts_with("window-state:"),
            "{p}"
        );
        assert!(!p.ends_with(":default"), "{p}: no default sets");
    }
    let capability = json("capabilities/default.json");
    assert_eq!(capability["windows"], serde_json::json!(["main"]));
    assert!(capability.get("remote").is_none());
}

/// Every host command build.rs generates a permission for is granted, and
/// nothing else is: the new native commands included.
#[test]
fn every_host_command_is_granted_once() {
    let build = std::fs::read_to_string(concat!(env!("CARGO_MANIFEST_DIR"), "/build.rs")).unwrap();
    let start = build.find(".commands(&[").unwrap();
    let end = start + build[start..].find("])").unwrap();
    let commands: Vec<String> = build[start..end]
        .split('"')
        .skip(1)
        .step_by(2)
        .map(|c| format!("allow-{}", c.replace('_', "-")))
        .collect();
    for native in ["allow-app-platform", "allow-reveal-path", "allow-open-path"] {
        assert!(commands.iter().any(|c| c == native), "{native}");
    }
    let own: Vec<String> = granted()
        .into_iter()
        .filter(|p| p.starts_with("allow-"))
        .collect();
    assert_eq!(own, commands);
}

/// macOS takes a folder dropped on the dock icon only for a declared
/// document type; Alternate keeps Finder the default for folders.
#[test]
fn folders_are_an_alternate_document_type() {
    let plist =
        std::fs::read_to_string(concat!(env!("CARGO_MANIFEST_DIR"), "/Info.plist")).unwrap();
    assert!(plist.contains("<string>public.folder</string>"));
    assert!(plist.contains("<key>LSHandlerRank</key>\n      <string>Alternate</string>"));
}

/// The session lives in the owner-only credentials file, never the OS
/// keychain: an ad-hoc-signed build prompted for the login password on
/// every keychain read. Nothing may bring a keychain crate back.
#[test]
fn no_keychain_crate_is_linked() {
    let manifest =
        std::fs::read_to_string(concat!(env!("CARGO_MANIFEST_DIR"), "/Cargo.toml")).unwrap();
    assert!(!manifest.contains("keyring"));
    let lock = std::fs::read_to_string(concat!(env!("CARGO_MANIFEST_DIR"), "/Cargo.lock")).unwrap();
    assert!(!lock.contains("name = \"keyring\""));
}

/// The host passes its bundle identifier from the Tauri config to the local
/// tools, which deny the app's default directories under it: the shared
/// library names no app.
#[test]
fn the_sandbox_gets_the_tauri_identifier() {
    let config: tauri::Config =
        serde_json::from_value(json("tauri.conf.json")).expect("tauri config");
    assert_eq!(
        crate::sandbox_app_id(&config).as_deref(),
        Some("ai.elitea.desktop")
    );
    assert_eq!(config.identifier, "ai.elitea.desktop");
}

/// An app directory that cannot be resolved is left out with a warning,
/// never an error that stops the app; one that resolves is kept.
#[test]
fn an_unresolvable_app_dir_is_dropped_not_fatal() {
    let resolved = crate::optional_app_dir("cache", Ok(std::path::PathBuf::from("/c")));
    assert_eq!(resolved, Some(std::path::PathBuf::from("/c")));
    let missing = crate::optional_app_dir("cache", Err(tauri::Error::UnknownPath));
    assert_eq!(missing, None);
}
