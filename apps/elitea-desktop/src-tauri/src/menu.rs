//! The native menu bar and its shortcuts (README, "Native features").
//!
//! Standard items (Edit's undo/redo/cut/copy/paste/select all, Hide, Quit,
//! Minimize, Close Window) are the OS's own predefined items: they act on
//! the focused text field, so they keep working everywhere in the webview.
//! The app's own items either act host-side (Open Folder…, zoom, reload,
//! help) or are forwarded to the UI as an `app://command` (IPC.md).

use std::sync::Mutex;

use tauri::menu::{
    AboutMetadataBuilder, Menu, MenuEvent, MenuItem, PredefinedMenuItem, Submenu, SubmenuBuilder,
};
use tauri::{AppHandle, Manager as _, Wry};

use crate::app_events;
use crate::commands::AppState;

/// Menu items forwarded to the UI unchanged: (menu id = command id, label,
/// accelerator).
pub const FORWARDED: &[(&str, &str, Option<&str>)] = &[
    ("new_thread", "New Thread", Some("CmdOrCtrl+N")),
    ("settings", "Settings…", Some("CmdOrCtrl+,")),
    ("command_palette", "Command Palette…", Some("CmdOrCtrl+K")),
    ("toggle_sidebar", "Toggle Sidebar", Some("CmdOrCtrl+\\")),
    (
        "toggle_changes",
        "Toggle Changes Panel",
        Some("CmdOrCtrl+Alt+\\"),
    ),
    ("back", "Back", Some("CmdOrCtrl+[")),
    ("forward", "Forward", Some("CmdOrCtrl+]")),
];

const OPEN_FOLDER: &str = "open_folder";
const RELOAD: &str = "reload";
const ZOOM_IN: &str = "zoom_in";
const ZOOM_OUT: &str = "zoom_out";
const ZOOM_RESET: &str = "zoom_reset";
const HELP: &str = "help";
/// Help ▸ Run Diagnostics…: the UI opens the Doctor (`run_diagnostics`).
const RUN_DIAGNOSTICS: &str = "run_diagnostics";

const ZOOM_STEPS: &[f64] = &[0.5, 0.67, 0.75, 0.8, 0.9, 1.0, 1.1, 1.25, 1.5, 1.75, 2.0];

/// The webview's zoom factor (the menu's zoom items).
#[derive(Default)]
pub struct Zoom(Mutex<Option<f64>>);

/// The next zoom step up (`up`) or down from `current`.
#[must_use]
pub fn zoom_step(current: f64, up: bool) -> f64 {
    let next = if up {
        ZOOM_STEPS.iter().copied().find(|s| *s > current + 1e-6)
    } else {
        ZOOM_STEPS
            .iter()
            .rev()
            .copied()
            .find(|s| *s < current - 1e-6)
    };
    next.unwrap_or(current)
}

fn forwarded(app: &AppHandle, id: &str) -> tauri::Result<MenuItem<Wry>> {
    let (id, label, accelerator) = FORWARDED
        .iter()
        .find(|(known, _, _)| *known == id)
        .copied()
        .unwrap_or((id, id, None));
    MenuItem::with_id(app, id, label, true, accelerator)
}

/// The whole menu bar.
///
/// # Errors
///
/// The OS refused a menu item.
pub fn build(app: &AppHandle) -> tauri::Result<Menu<Wry>> {
    let about = PredefinedMenuItem::about(
        app,
        Some("About Elitea"),
        Some(
            AboutMetadataBuilder::new()
                .name(Some("Elitea"))
                .version(Some(env!("CARGO_PKG_VERSION")))
                .build(),
        ),
    )?;
    let separator = || PredefinedMenuItem::separator(app);

    let open_folder =
        MenuItem::with_id(app, OPEN_FOLDER, "Open Folder…", true, Some("CmdOrCtrl+O"))?;
    let mut file = SubmenuBuilder::new(app, "File")
        .item(&forwarded(app, "new_thread")?)
        .item(&open_folder)
        .item(&separator()?)
        .item(&PredefinedMenuItem::close_window(
            app,
            Some("Close Window"),
        )?);
    if !cfg!(target_os = "macos") {
        // No app menu here: Settings and Quit live in File.
        file = file
            .item(&separator()?)
            .item(&forwarded(app, "settings")?)
            .item(&separator()?)
            .item(&PredefinedMenuItem::quit(app, None)?);
    }
    let file = file.build()?;

    let edit = SubmenuBuilder::new(app, "Edit")
        .item(&PredefinedMenuItem::undo(app, None)?)
        .item(&PredefinedMenuItem::redo(app, None)?)
        .item(&separator()?)
        .item(&PredefinedMenuItem::cut(app, None)?)
        .item(&PredefinedMenuItem::copy(app, None)?)
        .item(&PredefinedMenuItem::paste(app, None)?)
        .item(&PredefinedMenuItem::select_all(app, None)?)
        .build()?;

    let mut view = SubmenuBuilder::new(app, "View")
        .item(&forwarded(app, "command_palette")?)
        .item(&separator()?)
        .item(&forwarded(app, "toggle_sidebar")?)
        .item(&forwarded(app, "toggle_changes")?)
        .item(&separator()?)
        .item(&forwarded(app, "back")?)
        .item(&forwarded(app, "forward")?)
        .item(&separator()?);
    if cfg!(debug_assertions) {
        view = view
            .item(&MenuItem::with_id(
                app,
                RELOAD,
                "Reload",
                true,
                Some("CmdOrCtrl+R"),
            )?)
            .item(&separator()?);
    }
    let view = view
        .item(&MenuItem::with_id(
            app,
            ZOOM_RESET,
            "Actual Size",
            true,
            Some("CmdOrCtrl+0"),
        )?)
        .item(&MenuItem::with_id(
            app,
            ZOOM_IN,
            "Zoom In",
            true,
            Some("CmdOrCtrl+="),
        )?)
        .item(&MenuItem::with_id(
            app,
            ZOOM_OUT,
            "Zoom Out",
            true,
            Some("CmdOrCtrl+-"),
        )?)
        .item(&separator()?)
        .item(&PredefinedMenuItem::fullscreen(app, None)?)
        .build()?;

    // These two ids make Tauri register them with the OS, which adds its
    // window list and the help search box.
    let window = SubmenuBuilder::with_id(app, tauri::menu::WINDOW_SUBMENU_ID, "Window")
        .item(&PredefinedMenuItem::minimize(app, None)?)
        .item(&PredefinedMenuItem::maximize(app, Some("Zoom"))?)
        .build()?;

    let mut help = SubmenuBuilder::with_id(app, tauri::menu::HELP_SUBMENU_ID, "Help")
        .item(&MenuItem::with_id(
            app,
            HELP,
            "Elitea Help",
            true,
            None::<&str>,
        )?)
        .item(&MenuItem::with_id(
            app,
            RUN_DIAGNOSTICS,
            "Run Diagnostics…",
            true,
            None::<&str>,
        )?);
    if !cfg!(target_os = "macos") {
        help = help.item(&separator()?).item(&about);
    }
    let help = help.build()?;

    let mut menus: Vec<Submenu<Wry>> = Vec::new();
    if cfg!(target_os = "macos") {
        menus.push(
            SubmenuBuilder::new(app, "Elitea")
                .item(&about)
                .item(&separator()?)
                .item(&forwarded(app, "settings")?)
                .item(&separator()?)
                .item(&PredefinedMenuItem::services(app, None)?)
                .item(&separator()?)
                .item(&PredefinedMenuItem::hide(app, None)?)
                .item(&PredefinedMenuItem::hide_others(app, None)?)
                .item(&PredefinedMenuItem::show_all(app, None)?)
                .item(&separator()?)
                .item(&PredefinedMenuItem::quit(app, None)?)
                .build()?,
        );
    }
    menus.extend([file, edit, view, window, help]);
    let refs: Vec<&dyn tauri::menu::IsMenuItem<Wry>> = menus
        .iter()
        .map(|m| m as &dyn tauri::menu::IsMenuItem<Wry>)
        .collect();
    Menu::with_items(app, &refs)
}

/// A menu item was chosen.
pub fn on_event(app: &AppHandle, event: &MenuEvent) {
    let id = event.id().as_ref();
    if let Some((command, _, _)) = FORWARDED.iter().find(|(known, _, _)| *known == id) {
        app_events::show_main(app);
        app_events::emit(app, command, None);
        return;
    }
    match id {
        OPEN_FOLDER => app_events::open_folder_dialog(app),
        RELOAD => {
            if let Some(window) = app.get_webview_window("main") {
                let _ = window.reload();
            }
        }
        ZOOM_IN | ZOOM_OUT | ZOOM_RESET => zoom(app, id),
        HELP => open_help(app),
        RUN_DIAGNOSTICS => {
            app_events::show_main(app);
            app_events::emit_live(app, RUN_DIAGNOSTICS);
        }
        _ => {}
    }
}

fn zoom(app: &AppHandle, id: &str) {
    let Some(window) = app.get_webview_window("main") else {
        return;
    };
    let state = app.state::<Zoom>();
    let mut current = state
        .0
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    let now = current.unwrap_or(1.0);
    let next = match id {
        ZOOM_IN => zoom_step(now, true),
        ZOOM_OUT => zoom_step(now, false),
        _ => 1.0,
    };
    if window.set_zoom(next).is_ok() {
        *current = Some(next);
    }
}

/// The deployment's documentation (`<origin>/docs/`), when connected. The
/// origin was validated (https, or loopback http) when it was connected.
#[must_use]
pub fn help_url(origin: Option<&str>) -> Option<String> {
    let origin = origin?.trim_end_matches('/');
    let parsed = url::Url::parse(origin).ok()?;
    matches!(parsed.scheme(), "https" | "http").then(|| format!("{origin}/docs/"))
}

fn open_help(app: &AppHandle) {
    use tauri_plugin_opener::OpenerExt as _;
    let origin = app
        .try_state::<AppState>()
        .and_then(|state| state.auth.state().ok())
        .and_then(|state| state.origin);
    if let Some(url) = help_url(origin.as_deref()) {
        let _ = app.opener().open_url(url, None::<&str>);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn zoom_steps_up_and_down_and_stops_at_the_ends() {
        assert!((zoom_step(1.0, true) - 1.1).abs() < 1e-9);
        assert!((zoom_step(1.0, false) - 0.9).abs() < 1e-9);
        assert!((zoom_step(2.0, true) - 2.0).abs() < 1e-9);
        assert!((zoom_step(0.5, false) - 0.5).abs() < 1e-9);
        assert!((zoom_step(1.05, true) - 1.1).abs() < 1e-9);
    }

    #[test]
    fn help_opens_the_connected_deployments_docs_only() {
        assert_eq!(
            help_url(Some("https://elitea.example.com/")).as_deref(),
            Some("https://elitea.example.com/docs/")
        );
        assert_eq!(help_url(None), None);
        assert_eq!(help_url(Some("file:///etc")), None);
    }

    #[test]
    fn every_forwarded_item_is_an_app_command_id_with_a_unique_shortcut() {
        let ids: Vec<&str> = FORWARDED.iter().map(|(id, _, _)| *id).collect();
        assert_eq!(
            ids,
            [
                "new_thread",
                "settings",
                "command_palette",
                "toggle_sidebar",
                "toggle_changes",
                "back",
                "forward"
            ]
        );
        let mut shortcuts: Vec<&str> = FORWARDED.iter().filter_map(|(_, _, a)| *a).collect();
        shortcuts.extend([
            "CmdOrCtrl+O",
            "CmdOrCtrl+R",
            "CmdOrCtrl+0",
            "CmdOrCtrl+=",
            "CmdOrCtrl+-",
        ]);
        let unique: std::collections::HashSet<&&str> = shortcuts.iter().collect();
        assert_eq!(unique.len(), shortcuts.len());
        for accelerator in shortcuts {
            assert!(accelerator.starts_with("CmdOrCtrl+"), "{accelerator}");
        }
    }
}
