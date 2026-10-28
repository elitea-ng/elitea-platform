//! The words on the host's own menus — the menu bar, the menu-bar (tray)
//! item and its activity lines — in the language the person chose in the
//! app.
//!
//! The page owns the translations (its catalogues, `t()`): once it has
//! applied a language it sends `app_menu_labels_set` with the label of every
//! key in [`KEYS`], and the host rebuilds both menus with them. Until then,
//! and for any key the page leaves out, a menu reads the English in
//! [`KEYS`]. Labels are plain text, checked against the key list and a
//! length limit; they never carry an accelerator, an id or a URL.

use std::collections::HashMap;
use std::sync::{OnceLock, RwLock};

use tauri::{AppHandle, Manager as _};

/// Every translatable label: (key, English). `{tool}` in a template is
/// replaced with the tool's name.
pub const KEYS: &[(&str, &str)] = &[
    ("about", "About Elitea"),
    ("services", "Services"),
    ("hide", "Hide Elitea"),
    ("hide_others", "Hide Others"),
    ("show_all", "Show All"),
    ("quit", "Quit Elitea"),
    ("file", "File"),
    ("new_thread", "New Thread"),
    ("open_folder", "Open Folder…"),
    ("close_window", "Close Window"),
    ("settings", "Settings…"),
    ("edit", "Edit"),
    ("undo", "Undo"),
    ("redo", "Redo"),
    ("cut", "Cut"),
    ("copy", "Copy"),
    ("paste", "Paste"),
    ("select_all", "Select All"),
    ("view", "View"),
    ("command_palette", "Command Palette…"),
    ("toggle_sidebar", "Toggle Sidebar"),
    ("toggle_changes", "Toggle Inspector"),
    ("back", "Back"),
    ("forward", "Forward"),
    ("reload", "Reload"),
    ("zoom_reset", "Actual Size"),
    ("zoom_in", "Zoom In"),
    ("zoom_out", "Zoom Out"),
    ("fullscreen", "Toggle Full Screen"),
    ("thread", "Thread"),
    ("send_to_cloud", "Send to Cloud…"),
    ("open_in_chat", "Open in Chat"),
    ("toggle_plan", "Plan Mode"),
    ("choose_agent", "Choose Agent…"),
    ("folder", "Folder"),
    ("index_settings", "Index Settings…"),
    ("handoff_settings", "Handoff Settings…"),
    ("window", "Window"),
    ("minimize", "Minimize"),
    ("maximize", "Zoom"),
    ("help_menu", "Help"),
    ("help", "Elitea Help"),
    ("run_diagnostics", "Run Diagnostics…"),
    ("tray_show", "Show Elitea"),
    ("tray_quit", "Quit Elitea"),
    ("activity_nothing", "Nothing running"),
    ("activity_running", "Running"),
    ("activity_approve", "Approve {tool}"),
    ("activity_approve_cloud", "Approve in cloud"),
    ("activity_running_cloud", "Running in cloud"),
];

const MAX_LABEL_CHARS: usize = 80;

fn store() -> &'static RwLock<HashMap<String, String>> {
    static LABELS: OnceLock<RwLock<HashMap<String, String>>> = OnceLock::new();
    LABELS.get_or_init(|| RwLock::new(HashMap::new()))
}

/// The label for `key`: the page's translation, else the English in [`KEYS`].
#[must_use]
pub fn text(key: &str) -> String {
    let chosen = store()
        .read()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
        .get(key)
        .cloned();
    chosen.unwrap_or_else(|| english(key).to_owned())
}

fn english(key: &str) -> &'static str {
    KEYS.iter()
        .find(|(known, _)| *known == key)
        .map_or("", |(_, english)| english)
}

/// The labels the page sent, checked: known keys only, short plain text, and
/// `{tool}` kept wherever the English has it.
///
/// # Errors
///
/// A key is unknown, or a label is empty, too long or carries control
/// characters, or drops `{tool}`.
pub fn validate(labels: &HashMap<String, String>) -> Result<(), String> {
    for (key, label) in labels {
        let Some((_, english)) = KEYS.iter().find(|(known, _)| *known == key) else {
            return Err(format!("unknown menu label `{key}`"));
        };
        if label.trim().is_empty() || label.chars().count() > MAX_LABEL_CHARS {
            return Err(format!(
                "menu label `{key}` must be 1-{MAX_LABEL_CHARS} characters"
            ));
        }
        if label.chars().any(char::is_control) {
            return Err(format!("menu label `{key}` carries a control character"));
        }
        if english.contains("{tool}") && !label.contains("{tool}") {
            return Err(format!("menu label `{key}` must keep {{tool}}"));
        }
    }
    Ok(())
}

/// Use the page's translations for the host's menus, and rebuild them.
///
/// # Errors
///
/// The labels are invalid (see [`validate`]), or the OS refused the menu.
#[tauri::command(rename_all = "snake_case")]
#[allow(clippy::needless_pass_by_value)] // tauri's extractors and owned args
pub fn app_menu_labels_set(app: AppHandle, labels: HashMap<String, String>) -> Result<(), String> {
    validate(&labels)?;
    *store()
        .write()
        .unwrap_or_else(std::sync::PoisonError::into_inner) = labels;
    let handle = app.clone();
    app.run_on_main_thread(move || {
        match crate::menu::build(&handle) {
            Ok(menu) => {
                if let Err(error) = handle.set_menu(menu) {
                    log::warn!("menu not relabelled: {error}");
                }
            }
            Err(error) => log::warn!("menu not relabelled: {error}"),
        }
        if let Some(activity) = handle.try_state::<std::sync::Arc<crate::activity::Activity>>() {
            activity.relabel();
        }
    })
    .map_err(|e| e.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn labels(pairs: &[(&str, &str)]) -> HashMap<String, String> {
        pairs
            .iter()
            .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
            .collect()
    }

    #[test]
    fn keys_are_unique_and_every_one_has_english() {
        let unique: std::collections::HashSet<&str> = KEYS.iter().map(|(k, _)| *k).collect();
        assert_eq!(unique.len(), KEYS.len());
        assert!(KEYS.iter().all(|(_, english)| !english.is_empty()));
    }

    #[test]
    fn accepts_known_keys_with_short_plain_text() {
        assert!(
            validate(&labels(&[
                ("file", "Fichier"),
                ("activity_approve", "Approuver {tool}")
            ]))
            .is_ok()
        );
    }

    #[test]
    fn refuses_unknown_keys_long_or_control_text_and_a_lost_placeholder() {
        assert!(validate(&labels(&[("menu.evil", "x")])).is_err());
        assert!(validate(&labels(&[("file", "")])).is_err());
        assert!(validate(&labels(&[("file", &"x".repeat(81))])).is_err());
        assert!(validate(&labels(&[("file", "a\nb")])).is_err());
        assert!(validate(&labels(&[("activity_approve", "Approuver")])).is_err());
    }

    #[test]
    fn untranslated_keys_read_english() {
        assert_eq!(english("file"), "File");
        assert_eq!(english("nope"), "");
    }
}
