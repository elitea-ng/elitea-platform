//! `app_platform`: what the web shell needs to lay out its own title area
//! (IPC.md, "App").
//!
//! On macOS the window has no title bar of its own (`TitleBarStyle::Overlay`,
//! hidden title): the web content runs up to the top edge, the traffic
//! lights sit inside the sidebar's top area, and the sidebar shows the
//! system's sidebar material through a transparent window. The shell then
//! marks a drag region (`data-tauri-drag-region`) and leaves
//! `traffic_light_inset_px` free on the left. Linux and Windows keep their
//! normal decorations and need neither.

use serde::Serialize;

/// Where the traffic lights' top-left corner sits, in logical pixels.
#[cfg_attr(not(target_os = "macos"), allow(dead_code))] // macOS window chrome only
pub const TRAFFIC_LIGHT_POSITION: (f64, f64) = (18.0, 22.0);
/// The width the traffic lights take from the left edge, gap included:
/// 18 px inset, three 12 px buttons with 8 px between them, and 20 px of
/// room after the last.
pub const TRAFFIC_LIGHT_INSET_PX: u32 = 18 + 3 * 12 + 2 * 8 + 20;

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct AppPlatform {
    /// `macos`, `linux` or `windows`.
    pub os: &'static str,
    /// The web content runs under the title bar: the shell draws the drag
    /// region and leaves room for the window controls.
    pub titlebar_overlay: bool,
    /// The room to leave on the left of the top bar (0 without the overlay).
    pub traffic_light_inset_px: u32,
    /// The window shows the system sidebar material through transparent
    /// web content.
    pub vibrancy: bool,
}

#[must_use]
pub const fn current() -> AppPlatform {
    if cfg!(target_os = "macos") {
        AppPlatform {
            os: "macos",
            titlebar_overlay: true,
            traffic_light_inset_px: TRAFFIC_LIGHT_INSET_PX,
            vibrancy: true,
        }
    } else {
        AppPlatform {
            os: if cfg!(target_os = "windows") {
                "windows"
            } else {
                "linux"
            },
            titlebar_overlay: false,
            traffic_light_inset_px: 0,
            vibrancy: false,
        }
    }
}

#[tauri::command(rename_all = "snake_case")]
pub fn app_platform() -> AppPlatform {
    current()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_platform_shape_is_the_ipc_contract() {
        let value = serde_json::to_value(current()).unwrap();
        let keys: Vec<&str> = value
            .as_object()
            .unwrap()
            .keys()
            .map(String::as_str)
            .collect();
        assert_eq!(
            keys.len(),
            4,
            "os, titlebar_overlay, traffic_light_inset_px, vibrancy"
        );
        #[cfg(target_os = "macos")]
        assert_eq!(
            value,
            serde_json::json!({"os": "macos", "titlebar_overlay": true, "traffic_light_inset_px": 90, "vibrancy": true})
        );
        #[cfg(not(target_os = "macos"))]
        assert_eq!(value["traffic_light_inset_px"], 0);
    }
}
