//! Native context menus (right-click on a thread, a folder, a changed file, a
//! message). The page describes the items; the host pops the OS's own menu at
//! the cursor, so it looks, sounds and behaves like every other menu on the
//! computer.
//!
//! Every call carries a `request_id` the page made up. The outcome comes
//! back on `app://context-menu` as `{ request_id, id }`: `id` is the id the
//! page gave the chosen item, or `null` when the menu was dismissed (sent by
//! the host once the menu closed and no choice arrived). The page ignores
//! events whose `request_id` is not its own, so a late or stray event can
//! never answer another menu.
//!
//! The page can only choose among ids it supplied: OS-level menu ids are
//! `ctx:<request_id>:<id>`, so they can never be mistaken for the menu bar's
//! items.

use std::collections::HashSet;
use std::sync::Mutex;
use std::time::Duration;

use serde::Deserialize;
use tauri::menu::{ContextMenu as _, Menu, MenuEvent, MenuItem, PredefinedMenuItem, Submenu};
use tauri::{AppHandle, Emitter as _, Manager as _, State, WebviewWindow, Wry};

/// The event the outcome is delivered on.
pub const CHANNEL: &str = "app://context-menu";
const PREFIX: &str = "ctx:";
/// Choosable items across the menu and its submenus.
const MAX_ITEMS: usize = 60;
const MAX_LABEL_CHARS: usize = 120;
const MAX_ID_CHARS: usize = 64;
const MAX_REQUEST_ID_CHARS: usize = 64;
/// How long after the menu closed a choice may still be on its way (the OS
/// delivers it through the main thread's event queue).
const CHOICE_GRACE: Duration = Duration::from_millis(150);

/// The menus waiting for an outcome (by request id).
#[derive(Default)]
pub struct Pending(Mutex<HashSet<String>>);

impl Pending {
    fn lock(&self) -> std::sync::MutexGuard<'_, HashSet<String>> {
        self.0
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }
}

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
    /// A submenu: the item's own `id` is unused, `children` are its entries
    /// (one level only).
    #[serde(default)]
    pub children: Vec<ContextItem>,
}

/// Refuse a menu the page should not be able to build: too long, empty
/// labels, ids that are empty or repeated, nothing to choose, submenus inside
/// submenus.
///
/// # Errors
///
/// A sentence saying what is wrong with the items.
pub fn validate(items: &[ContextItem]) -> Result<(), String> {
    let mut seen = HashSet::new();
    let mut choosable = 0;
    check(items, 0, &mut seen, &mut choosable)?;
    if choosable == 0 {
        return Err("a context menu needs an item to choose".to_owned());
    }
    if choosable > MAX_ITEMS {
        return Err(format!("a context menu has at most {MAX_ITEMS} items"));
    }
    Ok(())
}

fn check<'a>(
    items: &'a [ContextItem],
    depth: usize,
    seen: &mut HashSet<&'a str>,
    choosable: &mut usize,
) -> Result<(), String> {
    if items.is_empty() || items.len() > MAX_ITEMS {
        return Err(format!("a context menu has 1 to {MAX_ITEMS} items"));
    }
    for item in items.iter().filter(|item| !item.separator) {
        if item.label.trim().is_empty() || item.label.chars().count() > MAX_LABEL_CHARS {
            return Err("a menu item needs a label of at most 120 characters".to_owned());
        }
        if !item.children.is_empty() {
            if depth > 0 {
                return Err("a submenu cannot hold another submenu".to_owned());
            }
            check(&item.children, depth + 1, seen, choosable)?;
            continue;
        }
        if item.id.is_empty() || item.id.chars().count() > MAX_ID_CHARS {
            return Err("a menu item needs an id of at most 64 characters".to_owned());
        }
        if !seen.insert(item.id.as_str()) {
            return Err(format!("the menu item id \"{}\" is used twice", item.id));
        }
        *choosable += 1;
    }
    Ok(())
}

/// A request id the page may use: short, and free of the separator.
fn valid_request_id(id: &str) -> bool {
    !id.is_empty()
        && id.chars().count() <= MAX_REQUEST_ID_CHARS
        && id
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_')
}

/// The OS-level id of a page id within one request.
#[must_use]
pub fn menu_id(request_id: &str, id: &str) -> String {
    format!("{PREFIX}{request_id}:{id}")
}

/// `(request id, page id)` behind an OS-level menu id; `None` for anything
/// that is not a context-menu item (the menu bar's own items).
#[must_use]
pub fn picked(menu_id: &str) -> Option<(&str, &str)> {
    menu_id.strip_prefix(PREFIX)?.split_once(':')
}

fn append_items(
    app: &AppHandle,
    menu: &Menu<Wry>,
    request_id: &str,
    items: &[ContextItem],
) -> tauri::Result<()> {
    for item in items {
        if item.separator {
            menu.append(&PredefinedMenuItem::separator(app)?)?;
        } else if item.children.is_empty() {
            menu.append(&MenuItem::with_id(
                app,
                menu_id(request_id, &item.id),
                &item.label,
                item.enabled,
                None::<&str>,
            )?)?;
        } else {
            let sub = Submenu::new(app, &item.label, item.enabled)?;
            for child in item.children.iter().filter(|c| !c.separator) {
                sub.append(&MenuItem::with_id(
                    app,
                    menu_id(request_id, &child.id),
                    &child.label,
                    child.enabled,
                    None::<&str>,
                )?)?;
            }
            menu.append(&sub)?;
        }
    }
    Ok(())
}

fn emit_outcome(app: &AppHandle, request_id: &str, id: Option<&str>) {
    if let Err(error) = app.emit_to(
        "main",
        CHANNEL,
        serde_json::json!({ "request_id": request_id, "id": id }),
    ) {
        log::warn!("could not deliver a context menu outcome: {error}");
    }
}

/// Pop a native menu at the cursor. Returns once it is closed; the outcome
/// (the chosen id, or `null` for a dismissal) follows on `app://context-menu`.
///
/// # Errors
///
/// The items or the request id are invalid, or the OS refused the menu.
#[tauri::command(rename_all = "snake_case")]
#[allow(clippy::needless_pass_by_value)] // tauri's extractors and owned args
pub fn context_menu_show(
    app: AppHandle,
    pending: State<'_, Pending>,
    window: WebviewWindow,
    request_id: String,
    items: Vec<ContextItem>,
) -> Result<(), String> {
    if !valid_request_id(&request_id) {
        return Err("a context menu needs a short request id".to_owned());
    }
    validate(&items)?;
    let menu = Menu::new(&app).map_err(|e| e.to_string())?;
    append_items(&app, &menu, &request_id, &items).map_err(|e| e.to_string())?;
    pending.lock().insert(request_id.clone());
    let shown = menu
        .popup(window.as_ref().window())
        .map_err(|e| e.to_string());
    // Closed (or refused): if no choice claims the request in the grace
    // period, say it was dismissed.
    let app = app.clone();
    tauri::async_runtime::spawn(async move {
        tokio::time::sleep(CHOICE_GRACE).await;
        let claimed = app
            .try_state::<Pending>()
            .is_some_and(|pending| pending.lock().remove(&request_id));
        if claimed {
            emit_outcome(&app, &request_id, None);
        }
    });
    shown
}

/// A menu item was chosen: if it is a context-menu item, hand its id to the
/// page. Returns whether it was one.
pub fn on_event(app: &AppHandle, event: &MenuEvent) -> bool {
    let Some((request_id, id)) = picked(event.id().as_ref()) else {
        return false;
    };
    let open = app
        .try_state::<Pending>()
        .is_some_and(|pending| pending.lock().remove(request_id));
    if open {
        emit_outcome(app, request_id, Some(id));
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
            children: Vec::new(),
        }
    }

    fn separator() -> ContextItem {
        ContextItem {
            separator: true,
            ..item("", "")
        }
    }

    fn submenu(label: &str, children: Vec<ContextItem>) -> ContextItem {
        ContextItem {
            children,
            ..item("", label)
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
        let many: Vec<ContextItem> = (0..61).map(|n| item(&n.to_string(), "x")).collect();
        assert!(validate(&many).is_err());
    }

    #[test]
    fn a_submenu_holds_items_and_counts_toward_the_total_but_never_nests() {
        let project = |n: usize| item(&format!("project:{n}"), "Project");
        let menu = vec![
            item("new", "New"),
            submenu("Project", (0..25).map(project).collect()),
        ];
        assert!(validate(&menu).is_ok(), "25 projects in a submenu fit");
        let too_many = vec![submenu("Project", (0..61).map(project).collect())];
        assert!(validate(&too_many).is_err());
        let nested = vec![submenu("A", vec![submenu("B", vec![item("x", "X")])])];
        assert!(validate(&nested).is_err());
        // Ids are unique across the whole menu, submenus included.
        assert!(validate(&[item("a", "A"), submenu("S", vec![item("a", "A2")])]).is_err());
    }

    #[test]
    fn menu_ids_carry_the_request_so_a_stray_choice_cannot_answer_another_menu() {
        assert_eq!(menu_id("r1", "open"), "ctx:r1:open");
        assert_eq!(picked("ctx:r1:open"), Some(("r1", "open")));
        assert_eq!(picked("ctx:r1:project:42"), Some(("r1", "project:42")));
        assert_eq!(picked("new_thread"), None);
        assert_eq!(picked("open_folder"), None);
        assert!(valid_request_id("3f2a-9b_1"));
        assert!(!valid_request_id(""));
        assert!(!valid_request_id("a:b"));
        assert!(!valid_request_id(&"x".repeat(65)));
    }

    #[test]
    fn items_deserialize_with_defaults() {
        let items: Vec<ContextItem> = serde_json::from_str(
            r#"[{"id":"a","label":"A"},{"separator":true},{"id":"b","label":"B","enabled":false},{"label":"S","children":[{"id":"c","label":"C"}]}]"#,
        )
        .unwrap();
        assert!(items[0].enabled && !items[0].separator);
        assert!(items[1].separator);
        assert!(!items[2].enabled);
        assert_eq!(items[3].children.len(), 1);
    }
}
