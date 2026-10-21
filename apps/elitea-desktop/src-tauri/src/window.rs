//! The main window, the inspector's panel windows, and the rule that keeps
//! them bundled.
//!
//! The webview holds IPC capabilities (capabilities/default.json), so it must
//! only ever show our own bundled assets. Tauri grants IPC by origin, and a
//! navigation to a remote page would put a page we did not ship next to the
//! credential-backed commands. `on_navigation` refuses every such navigation
//! before it happens; the deployment is reached through the host's fetch
//! (src/net.rs) from bundled code, never by navigating to it.
//!
//! The one other window is an inspector tab opened in a window of its own
//! (`shared/desktop/panelWindows.ts`): the page calls
//! `window.open("about:blank#elitea-panel=<tab>")` and fills the empty,
//! same-origin document from its own React tree. It loads nothing and holds
//! no capability (its label, `panel-<tab>`, is in no capability file); it
//! may not navigate anywhere. Every other `window.open` is refused.

use tauri::webview::{NewWindowFeatures, NewWindowResponse};
use tauri::{AppHandle, Manager, WebviewUrl, WebviewWindow, WebviewWindowBuilder};
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

/// A panel window's first size (the window-state plugin remembers it after that) and its smallest.
const PANEL_SIZE: (f64, f64) = (420.0, 640.0);
const PANEL_MIN_SIZE: (f64, f64) = (300.0, 240.0);

/// The label of the panel window `url` asks for (`about:blank#elitea-panel=<tab>`,
/// `<tab>` lowercase letters and dashes), or `None` for any other URL.
pub fn panel_window_label(url: &Url) -> Option<String> {
    if url.scheme() != "about" || url.path() != "blank" || url.query().is_some() {
        return None;
    }
    let tab = url.fragment()?.strip_prefix("elitea-panel=")?;
    let valid =
        (1..=32).contains(&tab.len()) && tab.bytes().all(|b| b.is_ascii_lowercase() || b == b'-');
    valid.then(|| format!("panel-{tab}"))
}

/// The only document a panel window may show: the empty one it opened with.
fn is_blank(url: &Url) -> bool {
    url.scheme() == "about" && url.path() == "blank"
}

/// `window.open` from the main window: a panel window, or nothing. One window per tab: asking again
/// raises the one that is open.
fn on_new_window(
    app: &AppHandle,
    url: &Url,
    features: NewWindowFeatures,
) -> NewWindowResponse<tauri::Wry> {
    let Some(label) = panel_window_label(url) else {
        log::warn!(target: "window", "refused a new window for {}", url.scheme());
        return NewWindowResponse::Deny;
    };
    if let Some(existing) = app.get_webview_window(&label) {
        let _ = existing.set_focus();
        return NewWindowResponse::Deny;
    }
    match build_panel_window(app, &label, url, features) {
        Ok(window) => NewWindowResponse::Create { window },
        Err(error) => {
            log::warn!(target: "window", "panel window {label}: {error}");
            NewWindowResponse::Deny
        }
    }
}

/// A panel window: normal decorations, titled after its document ("Changes — fix CI").
fn build_panel_window(
    app: &AppHandle,
    label: &str,
    url: &Url,
    features: NewWindowFeatures,
) -> tauri::Result<WebviewWindow> {
    WebviewWindowBuilder::new(app, label, WebviewUrl::External(url.clone()))
        .title("Elitea")
        .inner_size(PANEL_SIZE.0, PANEL_SIZE.1)
        .min_inner_size(PANEL_MIN_SIZE.0, PANEL_MIN_SIZE.1)
        // The opener's webview configuration (macOS) and the size it asked for.
        .window_features(features)
        .on_navigation(is_blank)
        .on_document_title_changed(|window, title| {
            let _ = window.set_title(&title);
        })
        .build()
}

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
    let opener = app.clone();
    let builder =
        builder.on_new_window(move |url, features| on_new_window(&opener, &url, features));
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

    fn panel(url: &str) -> Option<String> {
        panel_window_label(&Url::parse(url).unwrap())
    }

    #[test]
    fn a_panel_window_is_named_after_its_tab() {
        assert_eq!(
            panel("about:blank#elitea-panel=changes").as_deref(),
            Some("panel-changes")
        );
        assert_eq!(
            panel("about:blank#elitea-panel=conversation").as_deref(),
            Some("panel-conversation")
        );
    }

    #[test]
    fn every_other_new_window_is_refused() {
        for url in [
            "about:blank",
            "about:blank#elitea-panel=",
            "about:blank#elitea-panel=../main",
            "about:blank#elitea-panel=Changes",
            "about:blank#elitea-panel=main%20x",
            "about:blank?x=1#elitea-panel=changes",
            "about:srcdoc#elitea-panel=changes",
            "https://elitea.example.com/#elitea-panel=changes",
            "tauri://localhost/index.html#elitea-panel=changes",
            "about:blank#elitea-panel=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        ] {
            assert_eq!(panel(url), None, "{url} must be refused");
        }
    }

    #[test]
    fn a_panel_window_may_not_navigate() {
        assert!(is_blank(
            &Url::parse("about:blank#elitea-panel=changes").unwrap()
        ));
        assert!(!is_blank(
            &Url::parse("tauri://localhost/index.html").unwrap()
        ));
        assert!(!is_blank(
            &Url::parse("https://elitea.example.com/").unwrap()
        ));
    }

    #[test]
    fn the_dev_server_is_allowed_in_debug_builds_only_and_only_on_its_port() {
        assert!(allowed("http://localhost:5173/", true));
        assert!(!allowed("http://localhost:5173/", false));
        assert!(!allowed("http://localhost:8080/", true));
        assert!(!allowed("https://localhost:5173/", true));
    }
}
