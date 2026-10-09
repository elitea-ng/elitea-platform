//! The one window, and the rule that keeps it bundled.
//!
//! The webview holds IPC capabilities (capabilities/default.json), so it must
//! only ever show our own bundled assets. Tauri grants IPC by origin, and a
//! navigation to a remote page would put a page we did not ship next to the
//! credential-backed commands. `on_navigation` refuses every such navigation
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

/// The smallest window the layout is designed for.
pub const MIN_SIZE: (f64, f64) = (900.0, 600.0);

/// The window state the window-state plugin keeps across launches: size,
/// position, maximized and full screen; never visibility or decorations
/// (those are this module's to decide).
#[must_use]
pub fn persisted_state() -> tauri_plugin_window_state::StateFlags {
    use tauri_plugin_window_state::StateFlags;
    StateFlags::SIZE | StateFlags::POSITION | StateFlags::MAXIMIZED | StateFlags::FULLSCREEN
}

/// macOS (14+) shows its input-source indicator, a small round bubble with
/// the keyboard layout's letter ("A"), under the caret whenever a text field
/// takes focus on a Mac with more than one input source. WKWebView leaves it
/// sitting inside the composer, where it reads as a stray avatar. This turns
/// it off for this app only (`TSMLanguageIndicatorEnabled` in the app's own
/// defaults domain), unless the person set that key for the app themselves.
#[cfg(target_os = "macos")]
pub fn quiet_input_source_indicator() {
    use objc2_foundation::{NSString, NSUserDefaults};
    let key = NSString::from_str("TSMLanguageIndicatorEnabled");
    let defaults = NSUserDefaults::standardUserDefaults();
    if defaults.objectForKey(&key).is_none() {
        defaults.setBool_forKey(false, &key);
    }
}

/// The main window. On macOS: no title bar of its own (overlay, hidden
/// title), the traffic lights inset into the sidebar's top area, and a
/// transparent window over the system sidebar material (README, "Native
/// features", for the private-API trade-off). Elsewhere: normal
/// decorations, an opaque window.
pub fn create_main_window(app: &AppHandle) -> tauri::Result<()> {
    let dev = cfg!(debug_assertions);
    let builder = WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
        .title("Elitea")
        .inner_size(1280.0, 820.0)
        .min_inner_size(MIN_SIZE.0, MIN_SIZE.1)
        .on_navigation(move |url| is_bundled_origin(url, dev));
    #[cfg(target_os = "macos")]
    let builder = {
        use crate::platform::TRAFFIC_LIGHT_POSITION;
        use tauri::utils::config::WindowEffectsConfig;
        use tauri::window::{Effect, EffectState};
        builder
            .title_bar_style(tauri::TitleBarStyle::Overlay)
            .hidden_title(true)
            .traffic_light_position(tauri::LogicalPosition::new(
                TRAFFIC_LIGHT_POSITION.0,
                TRAFFIC_LIGHT_POSITION.1,
            ))
            .transparent(true)
            .effects(WindowEffectsConfig {
                effects: vec![Effect::Sidebar],
                state: Some(EffectState::FollowsWindowActiveState),
                radius: None,
                color: None,
                interactive: false,
            })
    };
    let window = builder.build()?;
    let handle = app.clone();
    window.on_window_event(move |event| on_window_event(&handle, event));
    Ok(())
}

fn on_window_event(app: &AppHandle, event: &tauri::WindowEvent) {
    match event {
        // A folder dropped on the window opens as a workspace; files go to
        // the UI (IPC.md, `workspace_opened` / `files_dropped`).
        tauri::WindowEvent::DragDrop(tauri::DragDropEvent::Drop { paths, .. }) => {
            crate::app_events::open_paths(app, paths);
        }
        // macOS keeps the app running with its window closed (⌘W, the red
        // button); the dock icon or ⌘Q decide. Hidden, not destroyed, so
        // a running turn keeps reporting and the window state is kept.
        #[cfg(target_os = "macos")]
        tauri::WindowEvent::CloseRequested { api, .. } => {
            api.prevent_close();
            if let Some(window) = tauri::Manager::get_webview_window(app, "main") {
                let _ = window.hide();
            }
        }
        _ => {}
    }
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
