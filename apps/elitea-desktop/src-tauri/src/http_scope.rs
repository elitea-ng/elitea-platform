//! The HTTP plugin's reach, narrowed at runtime to the connected deployment.
//!
//! A static capability could only say "any https host". The webview never
//! needs that: it talks to the one deployment the person confirmed. So
//! `capabilities/default.json` grants `http:default` no URL at all, and this
//! module adds a runtime capability (Tauri's `dynamic-acl`) scoped to that
//! deployment's origin, at startup when one is stored and again whenever the
//! person connects to another. A runtime capability cannot be removed, so
//! origins connected earlier in this process stay reachable until restart;
//! the bearer-origin checks in the webview (`fetchEventSource`, `http.ts`)
//! are the second layer.

use serde_json::{Value, json};
use tauri::AppHandle;
use url::Url;

/// The scope entries allowing exactly `origin` (any path, any query).
pub fn scope_entries(origin: &str) -> Option<Vec<Value>> {
    let url = Url::parse(origin).ok()?;
    if !matches!(url.scheme(), "http" | "https") || url.host_str().is_none() {
        return None;
    }
    let base = url.origin().ascii_serialization();
    Some(vec![json!({ "url": format!("{base}/*") })])
}

/// Identifier unique per origin so a second connect adds, never replaces.
fn capability_id(origin: &str) -> String {
    let safe: String = origin
        .chars()
        .map(|c| if c.is_ascii_alphanumeric() { c } else { '-' })
        .collect();
    format!("http-deployment-{safe}")
}

pub fn grant(app: &AppHandle, origin: &str) -> Result<(), String> {
    let allow = scope_entries(origin).ok_or_else(|| "not an http(s) origin".to_owned())?;
    let capability = tauri::ipc::CapabilityBuilder::new(capability_id(origin))
        .window("main")
        .permission_scoped("http:default", allow, Vec::<Value>::new());
    tauri::Manager::add_capability(app, capability).map_err(|e| e.to_string())
}

/// Grant the stored deployment, if any, at startup.
pub fn grant_stored(app: &AppHandle, origin: Option<&str>) {
    if let Some(origin) = origin
        && let Err(error) = grant(app, origin)
    {
        log::warn!("could not scope the HTTP plugin: {error}");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_the_deployment_origin_is_allowed() {
        let entries = scope_entries("https://elitea.example.com/some/path").unwrap();
        assert_eq!(
            entries,
            vec![json!({"url": "https://elitea.example.com/*"})]
        );
        let local = scope_entries("http://127.0.0.1:8080").unwrap();
        assert_eq!(local, vec![json!({"url": "http://127.0.0.1:8080/*"})]);
    }

    #[test]
    fn non_http_origins_get_no_scope() {
        for origin in [
            "file:///etc",
            "tauri://localhost",
            "nonsense",
            "javascript:1",
        ] {
            assert!(scope_entries(origin).is_none(), "{origin}");
        }
    }

    #[test]
    fn the_wildcard_host_is_never_produced() {
        let entries = scope_entries("https://a.example").unwrap();
        assert!(!entries[0]["url"].as_str().unwrap().contains("//*"));
    }
}
