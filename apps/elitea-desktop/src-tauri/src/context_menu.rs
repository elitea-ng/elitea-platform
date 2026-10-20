//! Native context menus (right-click on a thread, a folder, a changed file, a
//! message). The page describes the items; the host pops the OS's own menu at
//! the cursor, so it looks, sounds and behaves like every other menu on the
//! computer. The chosen item comes back on `app://context-menu` as
//! `{ id }`, the id the page gave it; a dismissed menu sends nothing (the
//! command itself returns once the menu is closed).
//!
//! The page can only choose among ids it supplied: menu ids are namespaced
//! with `ctx:` so they can never be mistaken for the menu bar's items.

use serde::Deserialize;
use tauri::menu::{Menu, MenuEvent, MenuItem, PredefinedMenuItem};
use tauri::{AppHandle, Emitter as _, WebviewWindow, Wry};

use tauri::menu::ContextMenu as _;

/// The event the chosen item is delivered on.
pub const CHANNEL: &str = "app://context-menu";
const PREFIX: &str = "ctx:";
const MAX_ITEMS: usize = 40;
const MAX_LABEL_CHARS: usize = 120;
const MAX_ID_CHARS: usize = 64;

fn enabled() -> bool {
    true
}

/// One entry of a context menu, as the page describes it.
#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct ContextItem {
    /// What comes back when it is chosen (not for a separator).
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub label: String,
    #[serde(default = "enabled")]
    pub enabled: bool,
    #[serde(default)]
    pub separator: bool,
}

/// Refuse a menu the page should not be able to build: too long, empty
/// labels, ids that are empty or repeated, nothing to choose.
///
/// # Errors
///
/// A sentence saying what is wrong with the items.
pub fn validate(items: &[ContextItem]) -> Result<(), String> {
    if items.is_empty() || items.len() > MAX_ITEMS {
        return Err(format!("a context menu has 1 to {MAX_ITEMS} items"));
    }
    let mut seen = std::collections::HashSet::new();
    let mut choosable = 0;
    for item in items.iter().filter(|item| !item.separator) {
        if item.label.trim().is_empty() || item.label.chars().count() > MAX_LABEL_CHARS {
            return Err("a menu item needs a label of at most 120 characters".to_owned());
        }
        if item.id.is_empty() || item.id.chars().count() > MAX_ID_CHARS {
            return Err("a menu item needs an id of at most 64 characters".to_owned());
        }
        if !seen.insert(item.id.as_str()) {
            return Err(format!("the menu item id \"{}\" is used twice", item.id));
        }
        choosable += 1;
    }
    if choosable == 0 {
        return Err("a context menu needs an item to choose".to_owned());
    }
    Ok(())
}

/// The OS-level id of a page id.
#[must_use]
pub fn menu_id(id: &str) -> String {
    format!("{PREFIX}{id}")
}

/// The page's id behind an OS-level menu id; `None` for anything that is not
/// a context-menu item (the menu bar's own items).
#[must_use]
pub fn picked(menu_id: &str) -> Option<&str> {
    menu_id.strip_prefix(PREFIX)
}

fn build(app: &AppHandle, items: &[ContextItem]) -> tauri::Result<Menu<Wry>> {
    let menu = Menu::new(app)?;
    for item in items {
        if item.separator {
            menu.append(&PredefinedMenuItem::separator(app)?)?;
        } else {
            menu.append(&MenuItem::with_id(
                app,
                menu_id(&item.id),
                &item.label,
                item.enabled,
                None::<&str>,
            )?)?;
        }
    }
    Ok(menu)
}

/// Pop a native menu at the cursor. Returns once it is closed.
///
/// # Errors
///
/// The items are invalid, or the OS refused the menu.
#[tauri::command(rename_all = "snake_case")]
#[allow(clippy::needless_pass_by_value)] // tauri's extractors and owned args
pub fn context_menu_show(
    app: AppHandle,
    window: WebviewWindow,
    items: Vec<ContextItem>,
) -> Result<(), String> {
    validate(&items)?;
    let menu = build(&app, &items).map_err(|e| e.to_string())?;
    menu.popup(window.as_ref().window())
        .map_err(|e| e.to_string())
}

/// A menu item was chosen: if it is a context-menu item, hand its id to the
/// page. Returns whether it was one.
pub fn on_event(app: &AppHandle, event: &MenuEvent) -> bool {
    let Some(id) = picked(event.id().as_ref()) else {
        return false;
    };
    if let Err(error) = app.emit_to("main", CHANNEL, serde_json::json!({ "id": id })) {
        log::warn!("could not deliver a context menu choice: {error}");
    }
    true
}

#[cfg(test)]
mod tests {
    use super::*;

    fn item(id: &str, label: &str) -> ContextItem {
        ContextItem {
            id: id.to_owned(),
            label: label.to_owned(),
            enabled: true,
            separator: false,
        }
    }

    fn separator() -> ContextItem {
        ContextItem {
            id: String::new(),
            label: String::new(),
            enabled: true,
            separator: true,
        }
    }

    #[test]
    fn a_sensible_menu_is_accepted() {
        assert!(validate(&[item("open", "Open"), separator(), item("rm", "Remove")]).is_ok());
    }

    #[test]
    fn an_empty_or_separator_only_menu_is_refused() {
        assert!(validate(&[]).is_err());
        assert!(validate(&[separator()]).is_err());
    }

    #[test]
    fn duplicate_empty_and_oversized_items_are_refused() {
        assert!(validate(&[item("a", "A"), item("a", "B")]).is_err());
        assert!(validate(&[item("", "A")]).is_err());
        assert!(validate(&[item("a", "  ")]).is_err());
        assert!(validate(&[item("a", &"x".repeat(121))]).is_err());
        assert!(validate(&[item(&"i".repeat(65), "A")]).is_err());
        let many: Vec<ContextItem> = (0..41).map(|n| item(&n.to_string(), "x")).collect();
        assert!(validate(&many).is_err());
    }

    #[test]
    fn menu_ids_are_namespaced_so_the_menu_bar_cannot_be_forged() {
        assert_eq!(menu_id("open"), "ctx:open");
        assert_eq!(picked("ctx:open"), Some("open"));
        assert_eq!(picked("new_thread"), None);
        assert_eq!(picked("open_folder"), None);
    }

    #[test]
    fn items_deserialize_with_defaults() {
        let items: Vec<ContextItem> =
            serde_json::from_str(r#"[{"id":"a","label":"A"},{"separator":true},{"id":"b","label":"B","enabled":false}]"#)
                .unwrap();
        assert!(items[0].enabled && !items[0].separator);
        assert!(items[1].separator);
        assert!(!items[2].enabled);
    }
}
