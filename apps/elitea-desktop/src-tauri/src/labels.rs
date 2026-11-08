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
//!
//! `{product}` in a label is the brand's product name (src/brand.rs), put in
//! when the label is read: the page's translations keep the placeholder, so
//! one catalogue serves every brand (ADR-0052).

use std::collections::HashMap;
use std::sync::{OnceLock, RwLock};

use tauri::{AppHandle, Manager as _};

/// Every translatable label: (key, English). `{tool}` in a template is
/// replaced with the tool's name, `{product}` with the brand's product name.
pub const KEYS: &[(&str, &str)] = &[
    ("about", "About {product}"),
    ("services", "Services"),
    ("hide", "Hide {product}"),
    ("hide_others", "Hide Others"),
    ("show_all", "Show All"),
    ("quit", "Quit {product}"),
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
    ("toggle_browser", "Browser"),
    ("folder", "Folder"),
    ("index_settings", "Index Settings…"),
    ("handoff_settings", "Handoff Settings…"),
    ("window", "Window"),
    ("minimize", "Minimize"),
    ("maximize", "Zoom"),
    ("help_menu", "Help"),
    ("help", "{product} Help"),
    ("run_diagnostics", "Run Diagnostics…"),
    ("tray_show", "Show {product}"),
    ("tray_quit", "Quit {product}"),
    ("activity_nothing", "Nothing running"),
    ("activity_running", "Running"),
    ("activity_approve", "Approve {tool}"),
    ("activity_approve_cloud", "Approve in cloud"),
    ("activity_running_cloud", "Running in cloud"),
    ("activity_awake", "Keeping the computer awake"),
    ("computer_using", "Using {app}"),
    ("computer_active", "Using the computer"),
    ("computer_paused", "Paused, you are using the computer"),
    ("computer_paused_button", "Paused"),
    ("computer_pause", "Pause"),
    ("computer_resume", "Resume"),
    ("computer_stop", "Stop"),
    ("quit_running_title", "Quit {product}?"),
    (
        "quit_running_message",
        "A turn is running. Quit stops it until you open {product} again.",
    ),
    ("quit_running_confirm", "Quit"),
    ("quit_running_cancel", "Keep running"),
];

const MAX_LABEL_CHARS: usize = 80;

fn store() -> &'static RwLock<HashMap<String, String>> {
    static LABELS: OnceLock<RwLock<HashMap<String, String>>> = OnceLock::new();
    LABELS.get_or_init(|| RwLock::new(HashMap::new()))
}

/// The label for `key`: the page's translation, else the English in [`KEYS`],
/// with `{product}` the brand's product name.
#[must_use]
pub fn text(key: &str) -> String {
    text_for(key, crate::brand::product())
}

/// [`text`] for the product name `product`.
#[must_use]
pub fn text_for(key: &str, product: &str) -> String {
    let chosen = store()
        .read()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
        .get(key)
        .cloned();
    chosen
        .unwrap_or_else(|| english(key).to_owned())
        .replace("{product}", product)
}

fn english(key: &str) -> &'static str {
    KEYS.iter()
        .find(|(known, _)| *known == key)
        .map_or("", |(_, english)| english)
}

/// The labels the page sent, checked: known keys only, short plain text, and
/// `{tool}` kept wherever the English has it. A label may name the product as
/// `{product}` (or in words, from a catalogue made before the placeholder).
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

    /// The menus name the brand's product, never a fixed one (ADR-0052).
    #[test]
    fn product_names_come_from_the_brand() {
        assert_eq!(text_for("about", "Acme"), "About Acme");
        assert_eq!(text_for("help", "Acme"), "Acme Help");
        assert_eq!(
            text_for("quit_running_message", "Acme"),
            "A turn is running. Quit stops it until you open Acme again."
        );
        for (key, english) in KEYS {
            assert!(
                !english.to_lowercase().contains("elitea"),
                "{key} names a product: {english}"
            );
        }
    }

    /// The page's catalogues (`apps/elitea-web`, `*.desktop.json`) name the
    /// product as `{product}`, never in words, so a branded build's menus say
    /// its own name in every language, and each catalogue passes [`validate`].
    #[test]
    fn the_web_catalogues_name_no_product() {
        let dir = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../elitea-web/src/entries/desktop/i18n");
        let mut seen = 0;
        for entry in std::fs::read_dir(&dir).expect("the desktop catalogues") {
            let path = entry.expect("entry").path();
            if !path.to_string_lossy().ends_with(".desktop.json") {
                continue;
            }
            seen += 1;
            let catalogue: HashMap<String, String> =
                serde_json::from_str(&std::fs::read_to_string(&path).expect("read"))
                    .expect("a flat catalogue");
            let menu: HashMap<String, String> = KEYS
                .iter()
                .filter_map(|(key, _)| {
                    let web = format!("desktop.nativeMenu.{}", camel(key));
                    catalogue
                        .get(&web)
                        .map(|label| ((*key).to_owned(), label.clone()))
                })
                .collect();
            validate(&menu).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
            for (key, label) in &menu {
                assert!(
                    !label.to_lowercase().contains("elitea"),
                    "{}: {key} names the product in words: {label}",
                    path.display()
                );
            }
            for key in ["about", "quit", "tray_show"] {
                assert!(
                    menu.get(key).is_some_and(|l| l.contains("{product}")),
                    "{}: {key} has no {{product}}",
                    path.display()
                );
            }
        }
        assert!(
            seen >= 19,
            "found {seen} desktop catalogues in {}",
            dir.display()
        );
    }

    /// `quit_running_title` → `quitRunningTitle` (the page's key names).
    fn camel(key: &str) -> String {
        let mut out = String::new();
        let mut upper = false;
        for c in key.chars() {
            if c == '_' {
                upper = true;
            } else if upper {
                out.extend(c.to_uppercase());
                upper = false;
            } else {
                out.push(c);
            }
        }
        out
    }

    #[test]
    fn untranslated_keys_read_english() {
        assert_eq!(english("file"), "File");
        assert_eq!(english("nope"), "");
    }
}
