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
