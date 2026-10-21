//! What the app is doing, for the places outside the window: the menu-bar
//! item (a compact title such as `◐ 0:42` or `⚠ 1`, and a menu of the turns
//! that run or wait) and the Dock's progress bar (index builds and sends to
//! the cloud).
//!
//! [`ActivityState`] is the pure core — fed with the same agent / index /
//! handoff events the window gets, it answers "what is the title, the menu,
//! the dock progress now" and is tested without a window. [`Activity`] is
//! the app side: it feeds the state from the event emitters and applies the
//! answers on the main thread.

use std::collections::{BTreeMap, HashMap};
use std::sync::Mutex;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant};

use tauri::image::Image;
use tauri::menu::{Menu, MenuItem, PredefinedMenuItem};
use tauri::tray::TrayIconBuilder;
use tauri::window::{ProgressBarState, ProgressBarStatus};
use tauri::{AppHandle, Manager as _, Wry};

use crate::app_events;
use crate::d0::events::AgentEvent;
use crate::local_commands::LocalState;

/// The tray item's id (`AppHandle::tray_by_id`).
const TRAY_ID: &str = "elitea";
/// Menu ids: a turn's item carries its id after the prefix.
const TURN_PREFIX: &str = "tray_turn:";
const CLOUD_PREFIX: &str = "tray_cloud:";
const SHOW: &str = "tray_show";
const QUIT: &str = "tray_quit";
/// A menu shows at most this many turns; the title says the rest.
const MAX_ENTRIES: usize = 8;

/// A tool's approval waiting for the person.
#[derive(Clone, Debug, PartialEq, Eq)]
struct Approval {
    turn_id: String,
    tool: String,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
struct Progress {
    done: u64,
    total: Option<u64>,
}

/// The Dock's progress bar.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Dock {
    /// No bar.
    None,
    /// Busy, how far unknown.
    Indeterminate,
    /// 0–100.
    Percent(u64),
}

/// One line of the tray menu.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Entry {
    /// A line that does nothing ("Nothing running").
    Note(String),
    /// A cloud turn streaming into this window: choosing it shows its folder.
    Cloud {
        uuid: String,
        label: String,
    },
    /// A turn: choosing it shows the window on its folder.
    Turn {
        turn_id: String,
        label: String,
    },
    Separator,
    Show,
    Quit,
}

/// A cloud turn the window is streaming (the web chat transport runs in the
/// page; the page tells the host through `activity_cloud_run`).
#[derive(Clone, Debug, PartialEq, Eq)]
struct CloudRun {
    since: Instant,
    /// A stream is running now.
    running: bool,
    /// The thread waits on the person (an approval).
    waiting: bool,
    workspace_id: Option<String>,
    workspace_name: Option<String>,
}

#[derive(Default)]
pub struct ActivityState {
    cloud: BTreeMap<String, CloudRun>,
    /// Running turns and when they were first heard of.
    turns: BTreeMap<String, Instant>,
    approvals: BTreeMap<String, Approval>,
    index: HashMap<String, Progress>,
    sends: HashMap<String, Progress>,
}

fn shown(text: &str, limit: usize) -> String {
    let text = text.trim();
    if text.chars().count() <= limit {
        return text.to_owned();
    }
    let mut out: String = text.chars().take(limit - 1).collect();
    out.push('…');
    out
}

/// `m:ss`, or `h:mm:ss` past an hour.
#[must_use]
pub fn clock(elapsed: Duration) -> String {
    let total = elapsed.as_secs();
    let (h, m, s) = (total / 3600, total / 60 % 60, total % 60);
    if h > 0 {
        format!("{h}:{m:02}:{s:02}")
    } else {
        format!("{m}:{s:02}")
    }
}

fn parse_after(message: &str, before: &str, after: &str) -> Option<u64> {
    let start = message.find(before)? + before.len();
    let rest = &message[start..];
    rest[..rest.find(after)?].trim().parse().ok()
}

impl ActivityState {
    /// Fold one agent event. `now` is when it was heard.
    ///
    /// Terminal rules (the web feed folds the same way, IPC.md "Activity"):
    /// a turn ends at `done`, and also at an `error` event or a terminal
    /// `status` (`done`, `cancelled`, `error`) — a turn refused at the start
    /// never sends `done`. An approval is open until `approval_respond`
    /// answers it (`answered`), the host closes it at its wait
    /// (`approval_closed`), or its turn ends: no other event closes it.
    pub fn observe_agent(&mut self, event: &AgentEvent, now: Instant) {
        let turn = event.turn_id.as_str();
        match event.kind {
            "status" => {
                let phase = event.payload["phase"].as_str().unwrap_or("");
                if matches!(phase, "resolving" | "starting" | "running" | "committing") {
                    self.turns.entry(turn.to_owned()).or_insert(now);
                } else if matches!(phase, "done" | "cancelled" | "error") {
                    self.end(turn);
                }
            }
            "approval_request" => {
                if let Some(id) = event.payload["request_id"].as_str() {
                    let tool = if event.payload["kind"] == "continue" {
                        "continue".to_owned()
                    } else {
                        shown(event.payload["tool"].as_str().unwrap_or("a tool"), 40)
                    };
                    self.turns.entry(turn.to_owned()).or_insert(now);
                    self.approvals.insert(
                        id.to_owned(),
                        Approval {
                            turn_id: turn.to_owned(),
                            tool,
                        },
                    );
                }
            }
            "approval_closed" => {
                if let Some(id) = event.payload["request_id"].as_str() {
                    self.answered(id);
                }
            }
            "error" | "done" => self.end(turn),
            _ => {}
        }
    }

    fn end(&mut self, turn: &str) {
        self.turns.remove(turn);
        self.approvals.retain(|_, a| a.turn_id != turn);
    }

    /// Whether `turn` is still shown as running or waiting.
    #[must_use]
    pub fn knows(&self, turn: &str) -> bool {
        self.turns.contains_key(turn)
    }

    /// `approval_respond` answered a request.
    pub fn answered(&mut self, request_id: &str) {
        self.approvals.remove(request_id);
    }

    /// An index report: `building` says whether it is (still) building.
    pub fn observe_index(&mut self, workspace_id: &str, building: bool, message: Option<&str>) {
        if !building {
            self.index.remove(workspace_id);
            return;
        }
        let progress = self.index.entry(workspace_id.to_owned()).or_default();
        let message = message.unwrap_or("");
        if let Some(total) = parse_after(message, "Parsing ", " files") {
            *progress = Progress {
                done: 0,
                total: Some(total),
            };
        } else if let Some(done) = parse_after(message, "Processed ", " files") {
            progress.done = done;
        }
    }

    /// A send to the cloud's report (`complete`, `cancelled` and `error` end it).
    pub fn observe_send(&mut self, uuid: &str, phase: &str, done: u64, total: u64) {
        if matches!(phase, "complete" | "cancelled" | "error") {
            self.sends.remove(uuid);
        } else {
            self.sends.insert(
                uuid.to_owned(),
                Progress {
                    done,
                    total: (total > 0).then_some(total),
                },
            );
        }
    }

    /// The page's cloud turn: streaming (`running`) and/or waiting on the
    /// person (`waiting`). Neither ends it (completion and errors both do).
    pub fn observe_cloud(
        &mut self,
        uuid: &str,
        (running, waiting): (bool, bool),
        (workspace_id, workspace_name): (Option<String>, Option<String>),
        now: Instant,
    ) {
        if !running && !waiting {
            self.cloud.remove(uuid);
            return;
        }
        let entry = self.cloud.entry(uuid.to_owned()).or_insert(CloudRun {
            since: now,
            running,
            waiting,
            workspace_id: workspace_id.clone(),
            workspace_name: workspace_name.clone(),
        });
        // A new stream restarts the timer; a pause does not.
        if running && !entry.running {
            entry.since = now;
        }
        entry.running = running;
        entry.waiting = waiting;
        entry.workspace_id = workspace_id;
        entry.workspace_name = workspace_name;
    }

    /// Cloud threads waiting on the person (they count with the approvals).
    #[must_use]
    pub fn cloud_waiting(&self) -> usize {
        self.cloud.values().filter(|run| run.waiting).count()
    }

    /// The folder a cloud run belongs to.
    #[must_use]
    pub fn cloud_workspace(&self, uuid: &str) -> Option<String> {
        self.cloud
            .get(uuid)
            .and_then(|run| run.workspace_id.clone())
    }

    /// Whether a timer is on screen (so something must re-render each second).
    #[must_use]
    pub fn has_timer(&self) -> bool {
        !self.turns.is_empty() || self.cloud.values().any(|run| run.running)
    }

    /// The menu-bar title: `⚠ n` for approvals waiting, else `◐ m:ss` for the
    /// longest-running turn, else empty (the icon alone).
    #[must_use]
    pub fn title(&self, now: Instant) -> String {
        let waiting = self.approvals.len() + self.cloud_waiting();
        if waiting > 0 {
            return format!("⚠ {waiting}");
        }
        let oldest = self
            .turns
            .values()
            .chain(
                self.cloud
                    .values()
                    .filter(|run| run.running)
                    .map(|run| &run.since),
            )
            .min();
        match oldest {
            Some(start) => format!("◐ {}", clock(now.saturating_duration_since(*start))),
            None => String::new(),
        }
    }

    /// The Dock bar: a send to the cloud first, else the index builds.
    #[must_use]
    pub fn dock(&self) -> Dock {
        let bars = if self.sends.is_empty() {
            &self.index
        } else {
            &self.sends
        };
        if bars.is_empty() {
            return Dock::None;
        }
        let (mut done, mut total) = (0_u64, 0_u64);
        for progress in bars.values() {
            match progress.total {
                Some(t) if t > 0 => {
                    done += progress.done.min(t);
                    total += t;
                }
                _ => return Dock::Indeterminate,
            }
        }
        Dock::Percent(done * 100 / total)
    }

    /// The tray menu: approvals first, then the other running turns.
    #[must_use]
    pub fn entries(&self, workspace_of: &dyn Fn(&str) -> Option<String>) -> Vec<Entry> {
        let place = |turn: &str| workspace_of(turn).map(|name| shown(&name, 40));
        let mut list: Vec<Entry> = Vec::new();
        for approval in self.approvals.values() {
            let label = match place(&approval.turn_id) {
                Some(name) => format!("Approve {} — {name}", approval.tool),
                None => format!("Approve {}", approval.tool),
            };
            list.push(Entry::Turn {
                turn_id: approval.turn_id.clone(),
                label,
            });
        }
        for turn in self.turns.keys() {
            if self.approvals.values().any(|a| &a.turn_id == turn) {
                continue;
            }
            let label = match place(turn) {
                Some(name) => format!("Running — {name}"),
                None => "Running".to_owned(),
            };
            list.push(Entry::Turn {
                turn_id: turn.clone(),
                label,
            });
        }
        let cloud_label = |what: &str, run: &CloudRun| match run.workspace_name.as_deref() {
            Some(name) => format!("{what} — {}", shown(name, 40)),
            None => what.to_owned(),
        };
        for (uuid, run) in &self.cloud {
            if run.waiting {
                list.insert(
                    0,
                    Entry::Cloud {
                        uuid: uuid.clone(),
                        label: cloud_label("Approve in cloud", run),
                    },
                );
            }
        }
        for (uuid, run) in &self.cloud {
            if run.running {
                list.push(Entry::Cloud {
                    uuid: uuid.clone(),
                    label: cloud_label("Running in cloud", run),
                });
            }
        }
        list.truncate(MAX_ENTRIES);
        if list.is_empty() {
            list.push(Entry::Note("Nothing running".to_owned()));
        }
        list.extend([Entry::Separator, Entry::Show, Entry::Quit]);
        list
    }
}

/// The turn a tray menu id names.
#[must_use]
pub fn turn_of(menu_id: &str) -> Option<&str> {
    menu_id.strip_prefix(TURN_PREFIX)
}

/// The app side: the state, the tray item and the dock.
pub struct Activity {
    app: AppHandle,
    state: Mutex<ActivityState>,
    ticking: AtomicBool,
    /// The folder name of each live turn, read once (the workspace file is
    /// not touched per event).
    names: Mutex<HashMap<String, String>>,
    /// What was last applied: nothing is posted to the main thread unless it changed.
    applied: Mutex<Option<Rendered>>,
}

/// Everything `render` applies; equal means nothing to do.
#[derive(Clone, Debug, PartialEq, Eq)]
struct Rendered {
    title: String,
    dock: Dock,
    entries: Vec<Entry>,
}

impl Activity {
    #[must_use]
    pub fn new(app: AppHandle) -> Self {
        Self {
            app,
            state: Mutex::new(ActivityState::default()),
            ticking: AtomicBool::new(false),
            names: Mutex::new(HashMap::new()),
            applied: Mutex::new(None),
        }
    }

    fn state(&self) -> std::sync::MutexGuard<'_, ActivityState> {
        self.state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    pub fn agent(&self, event: &AgentEvent) {
        self.remember_name(event);
        self.state().observe_agent(event, Instant::now());
        self.forget_finished();
        self.changed();
    }

    /// Look the folder up once per turn, never on a streamed token and never
    /// while the state lock is held.
    fn remember_name(&self, event: &AgentEvent) {
        if matches!(
            event.kind,
            "text_delta" | "usage" | "context" | "tool_result"
        ) {
            return;
        }
        let known = self
            .names
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .contains_key(&event.turn_id);
        if known {
            return;
        }
        if let Some(name) = Self::workspace_name(&self.app, &event.turn_id) {
            self.names
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .insert(event.turn_id.clone(), name);
        }
    }

    /// Names of turns that ended are dropped with them.
    fn forget_finished(&self) {
        let state = self.state();
        self.names
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .retain(|turn, _| state.knows(turn));
    }

    pub fn cloud(&self, uuid: &str, flags: (bool, bool), place: (Option<String>, Option<String>)) {
        let waiting = {
            let mut state = self.state();
            state.observe_cloud(uuid, flags, place, Instant::now());
            state.cloud_waiting()
        };
        // The Dock badge counts them with the approvals.
        if let Some(attention) = self
            .app
            .try_state::<std::sync::Arc<crate::attention::Attention>>()
        {
            attention.set_external(waiting);
        }
        self.changed();
    }

    pub fn answered(&self, request_id: &str) {
        self.state().answered(request_id);
        self.changed();
    }

    pub fn index(&self, workspace_id: &str, building: bool, message: Option<&str>) {
        self.state().observe_index(workspace_id, building, message);
        self.changed();
    }

    pub fn send(&self, uuid: &str, phase: &str, done: u64, total: u64) {
        self.state().observe_send(uuid, phase, done, total);
        self.changed();
    }

    /// Re-render now, and keep re-rendering each second while a timer shows.
    fn changed(&self) {
        self.render();
        if self.state().has_timer() && !self.ticking.swap(true, Ordering::AcqRel) {
            let app = self.app.clone();
            tauri::async_runtime::spawn(async move {
                loop {
                    tokio::time::sleep(Duration::from_secs(1)).await;
                    let Some(activity) = app.try_state::<std::sync::Arc<Activity>>() else {
                        break;
                    };
                    activity.render();
                    if !activity.state().has_timer() {
                        activity.ticking.store(false, Ordering::Release);
                        // One last look: a turn may have started in between.
                        if activity.state().has_timer()
                            && !activity.ticking.swap(true, Ordering::AcqRel)
                        {
                            continue;
                        }
                        break;
                    }
                }
            });
        }
    }

    fn workspace_name(app: &AppHandle, turn_id: &str) -> Option<String> {
        let local = app.try_state::<LocalState>()?;
        let workspace_id = local.agents.turn_workspace(turn_id)?;
        local
            .workspaces
            .get(&workspace_id)
            .ok()
            .flatten()
            .map(|w| w.name)
    }

    /// Apply the state to the tray and the dock, on the main thread — only
    /// when the title, the dock bar or the menu actually changed.
    fn render(&self) {
        let names = self
            .names
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .clone();
        let now = {
            let state = self.state();
            Rendered {
                title: state.title(Instant::now()),
                dock: state.dock(),
                entries: state.entries(&|turn| names.get(turn).cloned()),
            }
        };
        let menu_changed = {
            let mut applied = self
                .applied
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            if applied.as_ref() == Some(&now) {
                return;
            }
            let menu_changed = applied
                .as_ref()
                .is_none_or(|before| before.entries != now.entries);
            *applied = Some(now.clone());
            menu_changed
        };
        let app = self.app.clone();
        let _ = self.app.run_on_main_thread(move || {
            apply(
                &app,
                &now.title,
                now.dock,
                menu_changed.then_some(&now.entries),
            );
        });
    }
}

fn apply(app: &AppHandle, title: &str, dock: Dock, entries: Option<&Vec<Entry>>) {
    if let Some(tray) = app.tray_by_id(TRAY_ID) {
        let _ = tray.set_title((!title.is_empty()).then_some(title));
        let _ = tray.set_tooltip(Some(if title.is_empty() { "Elitea" } else { title }));
        if let Some(entries) = entries
            && let Ok(menu) = build_menu(app, entries)
        {
            let _ = tray.set_menu(Some(menu));
        }
    }
    if let Some(window) = app.get_webview_window("main") {
        let state = match dock {
            Dock::None => ProgressBarState {
                status: Some(ProgressBarStatus::None),
                progress: None,
            },
            Dock::Indeterminate => ProgressBarState {
                status: Some(ProgressBarStatus::Indeterminate),
                progress: None,
            },
            Dock::Percent(p) => ProgressBarState {
                status: Some(ProgressBarStatus::Normal),
                progress: Some(p),
            },
        };
        let _ = window.set_progress_bar(state);
    }
}

fn build_menu(app: &AppHandle, entries: &[Entry]) -> tauri::Result<Menu<Wry>> {
    let menu = Menu::new(app)?;
    for entry in entries {
        match entry {
            Entry::Note(text) => menu.append(&MenuItem::new(app, text, false, None::<&str>)?)?,
            Entry::Cloud { uuid, label } => menu.append(&MenuItem::with_id(
                app,
                format!("{CLOUD_PREFIX}{uuid}"),
                label,
                true,
                None::<&str>,
            )?)?,
            Entry::Turn { turn_id, label } => menu.append(&MenuItem::with_id(
                app,
                format!("{TURN_PREFIX}{turn_id}"),
                label,
                true,
                None::<&str>,
            )?)?,
            Entry::Separator => menu.append(&PredefinedMenuItem::separator(app)?)?,
            Entry::Show => menu.append(&MenuItem::with_id(
                app,
                SHOW,
                "Show Elitea",
                true,
                None::<&str>,
            )?)?,
            Entry::Quit => menu.append(&MenuItem::with_id(
                app,
                QUIT,
                "Quit Elitea",
                true,
                Some("CmdOrCtrl+Q"),
            )?)?,
        }
    }
    Ok(menu)
}

/// Create the menu-bar item. A system without a tray (some Linux desktops)
/// reports an error the caller logs; the app works without it.
///
/// # Errors
///
/// The OS refused the tray item.
pub fn create_tray(app: &AppHandle) -> tauri::Result<()> {
    let icon = Image::from_bytes(include_bytes!("../icons/tray-template.png"))?;
    let menu = build_menu(app, &ActivityState::default().entries(&|_| None))?;
    TrayIconBuilder::with_id(TRAY_ID)
        .icon(icon)
        .icon_as_template(true)
        .tooltip("Elitea")
        .menu(&menu)
        .show_menu_on_left_click(true)
        .on_menu_event(|app, event| {
            let id = event.id().as_ref();
            if id == SHOW {
                app_events::show_main(app);
            } else if id == QUIT {
                app.exit(0);
            } else if let Some(turn) = turn_of(id) {
                focus_turn(app, turn);
            } else if let Some(uuid) = id.strip_prefix(CLOUD_PREFIX) {
                focus_cloud(app, uuid);
            }
        })
        .build(app)?;
    Ok(())
}

/// Bring the window forward on the folder a cloud run belongs to.
fn focus_cloud(app: &AppHandle, uuid: &str) {
    app_events::show_main(app);
    let workspace = app
        .try_state::<std::sync::Arc<Activity>>()
        .and_then(|activity| activity.state().cloud_workspace(uuid));
    if let Some(workspace_id) = workspace {
        app_events::emit(
            app,
            "focus_turn",
            Some(serde_json::json!({ "workspace_id": workspace_id, "turn_id": uuid })),
        );
    }
}

/// The page tells the host a cloud turn of this window started or ended: the
/// menu-bar item shows it like a local turn. (`workspace_id` / `workspace_name`
/// are display and focus hints; the host reads no file for them.)
#[tauri::command(rename_all = "snake_case")]
#[allow(clippy::needless_pass_by_value)] // tauri's extractors and owned args
pub fn activity_cloud_run(
    activity: tauri::State<'_, std::sync::Arc<Activity>>,
    conversation_uuid: String,
    running: bool,
    waiting: Option<bool>,
    workspace_id: Option<String>,
    workspace_name: Option<String>,
) -> Result<(), String> {
    if conversation_uuid.is_empty() || conversation_uuid.chars().count() > 80 {
        return Err("a cloud run needs the conversation's uuid".to_owned());
    }
    let hint = |text: Option<String>| text.map(|t| shown(&t, 80));
    activity.cloud(
        &conversation_uuid,
        (running, waiting.unwrap_or(false)),
        (hint(workspace_id), hint(workspace_name)),
    );
    Ok(())
}

/// Bring the window forward on the folder a turn runs in.
fn focus_turn(app: &AppHandle, turn_id: &str) {
    app_events::show_main(app);
    if let Some(workspace_id) = app
        .try_state::<LocalState>()
        .and_then(|local| local.agents.turn_workspace(turn_id))
    {
        app_events::emit(
            app,
            "focus_turn",
            Some(serde_json::json!({ "workspace_id": workspace_id, "turn_id": turn_id })),
        );
    }
}

#[cfg(test)]
mod tests {
    use serde_json::{Value, json};

    use super::*;

    fn event(turn: &str, kind: &'static str, payload: Value) -> AgentEvent {
        AgentEvent {
            turn_id: turn.into(),
            seq: 0,
            kind,
            payload,
        }
    }

    fn running(turn: &str) -> AgentEvent {
        event(turn, "status", json!({"phase": "running"}))
    }

    fn approval(turn: &str, request: &str, tool: &str) -> AgentEvent {
        event(
            turn,
            "approval_request",
            json!({"request_id": request, "tool": tool, "title": "Run `curl -H secret`"}),
        )
    }

    fn names(turn: &str) -> Option<String> {
        Some(
            match turn {
                "t1" => "elitea-platform",
                "t2" => "docs",
                _ => return None,
            }
            .to_owned(),
        )
    }

    #[test]
    fn clock_counts_minutes_and_hours() {
        assert_eq!(clock(Duration::from_secs(42)), "0:42");
        assert_eq!(clock(Duration::from_secs(125)), "2:05");
        assert_eq!(clock(Duration::from_secs(3725)), "1:02:05");
    }

    #[test]
    fn the_title_is_the_timer_of_the_oldest_turn_and_an_approval_beats_it() {
        let t0 = Instant::now();
        let mut state = ActivityState::default();
        assert_eq!(state.title(t0), "");
        state.observe_agent(&running("t1"), t0);
        state.observe_agent(&running("t2"), t0 + Duration::from_secs(10));
        assert_eq!(state.title(t0 + Duration::from_secs(42)), "◐ 0:42");
        assert!(state.has_timer());
        state.observe_agent(&approval("t2", "r1", "run_command"), t0);
        assert_eq!(state.title(t0 + Duration::from_secs(42)), "⚠ 1");
        // Answered by the person: back to the timer.
        state.answered("r1");
        assert_eq!(state.title(t0 + Duration::from_secs(42)), "◐ 0:42");
        state.observe_agent(&event("t1", "done", json!({"committed": true})), t0);
        state.observe_agent(&event("t2", "done", json!({"committed": true})), t0);
        assert_eq!(state.title(t0), "");
        assert!(!state.has_timer());
    }

    #[test]
    fn only_an_answer_its_close_or_the_end_of_its_turn_closes_an_approval() {
        let t0 = Instant::now();
        let mut state = ActivityState::default();
        state.observe_agent(&approval("t1", "r1", "run_command"), t0);
        state.observe_agent(&approval("t1", "r2", "write_file"), t0);
        assert_eq!(state.title(t0), "⚠ 2");
        // Another tool's result or streamed text says nothing about these.
        state.observe_agent(&event("t1", "tool_result", json!({"call_id": "c"})), t0);
        state.observe_agent(&event("t1", "text_delta", json!({"text": "hi"})), t0);
        assert_eq!(state.title(t0), "⚠ 2");
        state.answered("r1");
        assert_eq!(state.title(t0), "⚠ 1");
        // The host closed the other at its wait.
        state.observe_agent(
            &event(
                "t1",
                "approval_closed",
                json!({"request_id": "r2", "reason": "timeout"}),
            ),
            t0,
        );
        assert!(!state.title(t0).starts_with('⚠'));
        state.observe_agent(&event("t1", "done", json!({"committed": true})), t0);
        assert_eq!(state.title(t0), "");
    }

    #[test]
    fn a_turn_refused_at_the_start_never_shows_as_running() {
        let t0 = Instant::now();
        let mut state = ActivityState::default();
        state.observe_agent(&event("t1", "status", json!({"phase": "resolving"})), t0);
        assert!(state.has_timer());
        state.observe_agent(
            &event("t1", "error", json!({"code": "x", "message": "m"})),
            t0,
        );
        assert!(
            !state.has_timer(),
            "an error ends it; no `done` follows a refusal"
        );
        assert_eq!(state.title(t0), "");
        // The terminal statuses end a turn too, with or without the error before them.
        for phase in ["error", "cancelled", "done"] {
            state.observe_agent(&running("t2"), t0);
            assert!(state.knows("t2"));
            state.observe_agent(&event("t2", "status", json!({"phase": phase})), t0);
            assert!(!state.knows("t2"), "{phase}");
        }
    }

    #[test]
    fn the_menu_lists_approvals_first_then_running_turns_and_never_a_command() {
        let t0 = Instant::now();
        let mut state = ActivityState::default();
        assert_eq!(
            state.entries(&names),
            [
                Entry::Note("Nothing running".into()),
                Entry::Separator,
                Entry::Show,
                Entry::Quit
            ]
        );
        state.observe_agent(&running("t1"), t0);
        state.observe_agent(&approval("t2", "r1", "run_command"), t0);
        let entries = state.entries(&names);
        assert_eq!(
            entries[..2],
            [
                Entry::Turn {
                    turn_id: "t2".into(),
                    label: "Approve run_command — docs".into()
                },
                Entry::Turn {
                    turn_id: "t1".into(),
                    label: "Running — elitea-platform".into()
                },
            ]
        );
        let text = format!("{entries:?}");
        assert!(!text.contains("curl") && !text.contains("secret"));
        assert_eq!(turn_of("tray_turn:t2"), Some("t2"));
        assert_eq!(turn_of("tray_show"), None);
    }

    #[test]
    fn a_cloud_turn_streaming_here_counts_like_a_local_one_and_clears_when_it_ends() {
        let t0 = Instant::now();
        let mut state = ActivityState::default();
        state.observe_cloud(
            "u1",
            (true, false),
            (Some("w1".into()), Some("elitea-platform".into())),
            t0,
        );
        assert!(state.has_timer());
        assert_eq!(state.title(t0 + Duration::from_secs(7)), "◐ 0:07");
        let entries = state.entries(&names);
        assert_eq!(
            entries[0],
            Entry::Cloud {
                uuid: "u1".into(),
                label: "Running in cloud — elitea-platform".into()
            }
        );
        assert_eq!(state.cloud_workspace("u1").as_deref(), Some("w1"));
        // Completion or an error both end it.
        state.observe_cloud("u1", (false, false), (None, None), t0);
        assert!(!state.has_timer());
        assert_eq!(state.title(t0), "");
    }

    #[test]
    fn a_cloud_thread_waiting_on_an_approval_raises_the_attention_state() {
        let t0 = Instant::now();
        let mut state = ActivityState::default();
        state.observe_cloud(
            "u1",
            (false, true),
            (Some("w1".into()), Some("app".into())),
            t0,
        );
        assert_eq!(state.cloud_waiting(), 1);
        assert!(!state.has_timer(), "paused: no stream, no timer");
        assert_eq!(state.title(t0), "⚠ 1");
        state.observe_agent(&approval("t2", "r1", "run_command"), t0);
        assert_eq!(state.title(t0), "⚠ 2", "counts with the local approvals");
        assert_eq!(
            state.entries(&names)[0],
            Entry::Cloud {
                uuid: "u1".into(),
                label: "Approve in cloud — app".into()
            }
        );
        // Answered: it resumes streaming; then it ends.
        state.observe_cloud(
            "u1",
            (true, false),
            (Some("w1".into()), Some("app".into())),
            t0,
        );
        assert_eq!(state.cloud_waiting(), 0);
        assert!(state.has_timer());
        state.observe_cloud("u1", (false, false), (None, None), t0);
        assert_eq!(state.cloud_waiting(), 0);
    }

    #[test]
    fn the_menu_is_bounded() {
        let t0 = Instant::now();
        let mut state = ActivityState::default();
        for n in 0..20 {
            state.observe_agent(&running(&format!("turn-{n}")), t0);
        }
        let turns = state
            .entries(&names)
            .into_iter()
            .filter(|e| matches!(e, Entry::Turn { .. }))
            .count();
        assert_eq!(turns, MAX_ENTRIES);
    }

    #[test]
    fn the_dock_follows_a_send_before_an_index_build() {
        let mut state = ActivityState::default();
        assert_eq!(state.dock(), Dock::None);
        state.observe_index("w1", true, None);
        assert_eq!(
            state.dock(),
            Dock::Indeterminate,
            "files to read not known yet"
        );
        state.observe_index("w1", true, Some("[extract] Parsing 120 files"));
        state.observe_index("w1", true, Some("[progress] Processed 30 files"));
        assert_eq!(state.dock(), Dock::Percent(25));
        state.observe_index("w2", true, Some("[extract] Parsing 80 files"));
        state.observe_index("w2", true, Some("[progress] Processed 70 files"));
        assert_eq!(
            state.dock(),
            Dock::Percent(50),
            "100 of 200 files across both"
        );
        state.observe_send("u1", "upload", 3, 10);
        assert_eq!(state.dock(), Dock::Percent(30));
        state.observe_send("u1", "complete", 10, 10);
        assert_eq!(state.dock(), Dock::Percent(50));
        state.observe_index("w1", false, None);
        state.observe_index("w2", false, None);
        assert_eq!(state.dock(), Dock::None);
    }

    #[test]
    fn a_send_without_a_total_is_indeterminate() {
        let mut state = ActivityState::default();
        state.observe_send("u1", "scan", 0, 0);
        assert_eq!(state.dock(), Dock::Indeterminate);
        state.observe_send("u1", "cancelled", 0, 0);
        assert_eq!(state.dock(), Dock::None);
    }
}
