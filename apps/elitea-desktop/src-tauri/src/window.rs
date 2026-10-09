//! The one window, and the rule that keeps it bundled.
//!
//! The webview holds IPC capabilities (capabilities/default.json), so it must
//! only ever show our own bundled assets. Tauri grants IPC by origin, and a
//! navigation to a remote page would put a page we did not ship next to the
//! keychain-backed commands. `on_navigation` refuses every such navigation
//! before it happens; the deployment is reached through the HTTP plugin from
//! bundled code, never by navigating to it.

use tauri::{AppHandle, WebviewUrl, WebviewWindowBuilder};
use url::{Host, Url};

/// True for the origins Tauri serves bundled assets from, per platform
/// (`tauri://localhost` on macOS and Linux, `http(s)://tauri.localhost` on
/// Windows and Android). With `dev`, also the Vite dev server.
pub fn is_bundled_origin(url: &Url, dev: bool) -> bool {
    let host_is = |expected: &str| matches!(url.host(), Some(Host::Domain(h)) if h == expected);
    match url.scheme() {
        "tauri" => host_is("localhost"),
        "http" | "https" if host_is("tauri.localhost") => true,
        "http" if dev => host_is("localhost") && url.port() == Some(5173),
        _ => false,
    }
}

pub fn create_main_window(app: &AppHandle) -> tauri::Result<()> {
    let dev = cfg!(debug_assertions);
    WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
        .title("Elitea")
        .inner_size(1280.0, 820.0)
        .min_inner_size(900.0, 600.0)
        .on_navigation(move |url| is_bundled_origin(url, dev))
        .build()?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn allowed(url: &str, dev: bool) -> bool {
        is_bundled_origin(&Url::parse(url).unwrap(), dev)
    }

    #[test]
    fn the_bundled_app_origins_are_allowed() {
        assert!(allowed("tauri://localhost/index.html", false));
        assert!(allowed("tauri://localhost/chat", false));
        assert!(allowed("http://tauri.localhost/", false));
        assert!(allowed("https://tauri.localhost/", false));
    }

    #[test]
    fn remote_and_look_alike_origins_are_refused() {
        for url in [
            "https://elitea.example.com/app/",
            "http://localhost:5173/",
            "https://tauri.localhost.evil.example/",
            "tauri://evil/",
            "file:///etc/passwd",
            "javascript:alert(1)",
            "data:text/html,hi",
            "about:blank",
        ] {
            assert!(!allowed(url, false), "{url} must be refused");
        }
    }

    #[test]
    fn the_dev_server_is_allowed_in_debug_builds_only_and_only_on_its_port() {
        assert!(allowed("http://localhost:5173/", true));
        assert!(!allowed("http://localhost:5173/", false));
        assert!(!allowed("http://localhost:8080/", true));
        assert!(!allowed("https://localhost:5173/", true));
    }
}
