//! Getting the person's attention while the window is in the background:
//! a system notification when a turn asks for an approval or ends, and the
//! dock badge counting the approvals still waiting (README, "Native
//! features").
//!
//! The host already sees every `agent://event` (`MainWindowEvents`), so the
//! [`Tracker`] watches that stream; nothing in the web UI drives it. A
//! notification names the workspace and, for an approval, only the TOOL —
//! never a command line, a path's contents, a diff or a secret.

use std::collections::{HashMap, HashSet};
use std::sync::Mutex;
use std::sync::atomic::{AtomicU8, Ordering};

use tauri::{AppHandle, Manager as _};

use crate::d0::events::AgentEvent;
use crate::local_commands::LocalState;

/// The most characters of a workspace name or tool name a notification shows.
const MAX_LABEL: usize = 60;
/// Ending outcomes remembered for turns that never send `done` (a refusal
/// before the start); past this the oldest are dropped.
const MAX_TRACKED_TURNS: usize = 512;

/// How a turn ended, from the events before its `done`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Outcome {
    Finished,
    Failed,
    Stopped,
}

/// What a notification is about.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Notice {
    Approval { turn_id: String, tool: String },
    Ended { turn_id: String, outcome: Outcome },
}

impl Notice {
    #[must_use]
    pub fn turn_id(&self) -> &str {
        match self {
            Self::Approval { turn_id, .. } | Self::Ended { turn_id, .. } => turn_id,
        }
    }
}

/// What one event changed.
#[derive(Debug, Default, Eq, PartialEq)]
pub struct Observed {
    pub badge_changed: bool,
    pub notice: Option<Notice>,
}

/// The open approvals (request id → turn id) and how running turns are
/// heading to end.
#[derive(Default)]
pub struct Tracker {
    pending: HashMap<String, String>,
    failed: HashSet<String>,
    stopped: HashSet<String>,
}

fn label(text: &str) -> String {
    let text = text.trim();
    if text.chars().count() <= MAX_LABEL {
        return text.to_owned();
    }
    let mut out: String = text.chars().take(MAX_LABEL - 1).collect();
    out.push('…');
    out
}

impl Tracker {
    /// Approvals still waiting for an answer, across workspaces.
    #[must_use]
    pub fn pending(&self) -> usize {
        self.pending.len()
    }

    pub fn observe(&mut self, event: &AgentEvent) -> Observed {
        let turn = event.turn_id.as_str();
        match event.kind {
            "approval_request" => {
                let Some(request_id) = event.payload["request_id"].as_str() else {
                    return Observed::default();
                };
                self.pending.insert(request_id.to_owned(), turn.to_owned());
                let tool = event.payload["tool"].as_str().unwrap_or("a tool");
                Observed {
                    badge_changed: true,
                    notice: Some(Notice::Approval {
                        turn_id: turn.to_owned(),
                        tool: label(tool),
                    }),
                }
            }
            "error" => {
                self.remember(turn, Outcome::Failed);
                Observed::default()
            }
            "status" if event.payload["phase"] == "cancelled" => {
                self.remember(turn, Outcome::Stopped);
                Observed::default()
            }
            "done" => {
                let before = self.pending.len();
                self.pending.retain(|_, of| of != turn);
                let outcome = if self.stopped.remove(turn) {
                    self.failed.remove(turn);
                    Outcome::Stopped
                } else if self.failed.remove(turn) {
                    Outcome::Failed
                } else {
                    Outcome::Finished
                };
                Observed {
                    badge_changed: self.pending.len() != before,
                    notice: Some(Notice::Ended {
                        turn_id: turn.to_owned(),
                        outcome,
                    }),
                }
            }
            _ => Observed::default(),
        }
    }

    fn remember(&mut self, turn: &str, outcome: Outcome) {
        if self.failed.len() + self.stopped.len() >= MAX_TRACKED_TURNS {
            self.failed.clear();
            self.stopped.clear();
        }
        match outcome {
            Outcome::Failed => self.failed.insert(turn.to_owned()),
            Outcome::Stopped => self.stopped.insert(turn.to_owned()),
            Outcome::Finished => false,
        };
    }

    /// `approval_respond` closed a question. True when it was open.
    pub fn answered(&mut self, request_id: &str) -> bool {
        self.pending.remove(request_id).is_some()
    }
}

/// A notification's title and body.
#[must_use]
pub fn notice_text(notice: &Notice, workspace: Option<&str>) -> (String, String) {
    let place = workspace.map(label);
    let with_place = |headline: &str| match &place {
        Some(name) => format!("{headline} — {name}"),
        None => headline.to_owned(),
    };
    match notice {
        Notice::Approval { tool, .. } => (
            with_place("Approval needed"),
            format!("The agent wants to use {tool}."),
        ),
        Notice::Ended { outcome, .. } => match outcome {
            Outcome::Finished => (with_place("Turn finished"), "The agent is done.".into()),
            Outcome::Failed => (
                with_place("Turn failed"),
                "The agent stopped with an error.".into(),
            ),
            Outcome::Stopped => (with_place("Turn stopped"), "The turn was cancelled.".into()),
        },
    }
}

const PERMISSION_UNKNOWN: u8 = 0;
const PERMISSION_GRANTED: u8 = 1;
const PERMISSION_DENIED: u8 = 2;

/// The app side: the tracker, the badge and the notifications.
pub struct Attention {
    app: AppHandle,
    tracker: Mutex<Tracker>,
    permission: AtomicU8,
}

impl Attention {
    #[must_use]
    pub fn new(app: AppHandle) -> Self {
        Self {
            app,
            tracker: Mutex::new(Tracker::default()),
            permission: AtomicU8::new(PERMISSION_UNKNOWN),
        }
    }

    fn tracker(&self) -> std::sync::MutexGuard<'_, Tracker> {
        self.tracker
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// Every agent event passes here after it was emitted to the window.
    pub fn observe(&self, event: &AgentEvent) {
        let observed = self.tracker().observe(event);
        if observed.badge_changed {
            self.refresh_badge();
        }
        if let Some(notice) = observed.notice {
            let app = self.app.clone();
            // Off the emitting thread: the lookups below take the turn
            // table's lock, which an emitter may hold.
            tauri::async_runtime::spawn(async move {
                if let Some(attention) = app.try_state::<std::sync::Arc<Attention>>() {
                    attention.notify(&notice);
                }
            });
        }
    }

    /// `approval_respond` answered `request_id`.
    pub fn answered(&self, request_id: &str) {
        if self.tracker().answered(request_id) {
            self.refresh_badge();
        }
    }

    /// Set the badge on the main thread, reading the count THERE: closures
    /// run in order, so the last one always shows the current count.
    fn refresh_badge(&self) {
        let app = self.app.clone();
        let _ = self.app.run_on_main_thread(move || {
            let Some(attention) = app.try_state::<std::sync::Arc<Attention>>() else {
                return;
            };
            let count = attention.tracker().pending();
            if let Some(window) = app.get_webview_window("main") {
                // Windows has no badge count (unsupported, ignored).
                let _ = window
                    .set_badge_count((count > 0).then(|| i64::try_from(count).unwrap_or(i64::MAX)));
            }
        });
    }

    fn workspace_name(&self, turn_id: &str) -> Option<String> {
        let local = self.app.try_state::<LocalState>()?;
        let workspace_id = local.agents.turn_workspace(turn_id)?;
        local
            .workspaces
            .get(&workspace_id)
            .ok()
            .flatten()
            .map(|w| w.name)
    }

    fn notify(&self, notice: &Notice) {
        let focused = self
            .app
            .get_webview_window("main")
            .is_some_and(|w| w.is_focused().unwrap_or(false) && w.is_visible().unwrap_or(true));
        if focused || !self.allowed() {
            return;
        }
        let name = self.workspace_name(notice.turn_id());
        let (title, body) = notice_text(notice, name.as_deref());
        use tauri_plugin_notification::NotificationExt as _;
        if let Err(error) = self
            .app
            .notification()
            .builder()
            .title(title)
            .body(body)
            .show()
        {
            log::warn!("could not show a notification: {error}");
        }
    }

    /// Asked lazily, on the first notification, then remembered.
    fn allowed(&self) -> bool {
        use tauri_plugin_notification::{NotificationExt as _, PermissionState};
        match self.permission.load(Ordering::Acquire) {
            PERMISSION_GRANTED => return true,
            PERMISSION_DENIED => return false,
            _ => {}
        }
        let notifications = self.app.notification();
        let state = match notifications.permission_state() {
            Ok(PermissionState::Granted) => PermissionState::Granted,
            _ => notifications
                .request_permission()
                .unwrap_or(PermissionState::Denied),
        };
        let granted = state == PermissionState::Granted;
        self.permission.store(
            if granted {
                PERMISSION_GRANTED
            } else {
                PERMISSION_DENIED
            },
            Ordering::Release,
        );
        granted
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

    fn approval(turn: &str, request: &str) -> AgentEvent {
        event(
            turn,
            "approval_request",
            json!({"request_id": request, "tool": "run_command", "title": "Run `curl -H 'Authorization: secret'`", "command": ["curl"]}),
        )
    }

    fn done(turn: &str) -> AgentEvent {
        event(turn, "done", json!({"committed": true}))
    }

    #[test]
    fn approvals_count_until_answered_or_their_turn_ends() {
        let mut tracker = Tracker::default();
        let first = tracker.observe(&approval("t1", "r1"));
        assert!(first.badge_changed);
        assert_eq!(
            first.notice,
            Some(Notice::Approval {
                turn_id: "t1".into(),
                tool: "run_command".into()
            })
        );
        tracker.observe(&approval("t1", "r2"));
        tracker.observe(&approval("t2", "r3"));
        assert_eq!(tracker.pending(), 3);
        assert!(tracker.answered("r1"));
        assert!(!tracker.answered("r1"), "answered once");
        assert_eq!(tracker.pending(), 2);
        let ended = tracker.observe(&done("t1"));
        assert!(ended.badge_changed);
        assert_eq!(tracker.pending(), 1);
        assert!(!tracker.observe(&done("t9")).badge_changed);
        tracker.observe(&done("t2"));
        assert_eq!(
            tracker.pending(),
            0,
            "a sign-out cancels the turn, whose done clears it"
        );
    }

    #[test]
    fn a_turn_ends_finished_failed_or_stopped() {
        let mut tracker = Tracker::default();
        let outcome = |observed: Observed| match observed.notice {
            Some(Notice::Ended { outcome, .. }) => outcome,
            other => panic!("{other:?}"),
        };
        assert_eq!(outcome(tracker.observe(&done("a"))), Outcome::Finished);
        tracker.observe(&event("b", "error", json!({"code": "x", "message": "m"})));
        assert_eq!(outcome(tracker.observe(&done("b"))), Outcome::Failed);
        tracker.observe(&event("c", "status", json!({"phase": "cancelled"})));
        assert_eq!(outcome(tracker.observe(&done("c"))), Outcome::Stopped);
        // Remembered outcomes are dropped with the turn.
        assert_eq!(outcome(tracker.observe(&done("b"))), Outcome::Finished);
        let quiet = tracker.observe(&event("d", "text_delta", json!({"text": "hi"})));
        assert_eq!(quiet, Observed::default());
    }

    #[test]
    fn notification_text_names_the_workspace_and_tool_only() {
        let mut tracker = Tracker::default();
        let notice = tracker.observe(&approval("t", "r")).notice.unwrap();
        let (title, body) = notice_text(&notice, Some("elitea-platform"));
        assert_eq!(title, "Approval needed — elitea-platform");
        assert_eq!(body, "The agent wants to use run_command.");
        assert!(!body.contains("secret") && !title.contains("curl"));
        let ended = Notice::Ended {
            turn_id: "t".into(),
            outcome: Outcome::Failed,
        };
        assert_eq!(notice_text(&ended, None).0, "Turn failed");
        let long = "x".repeat(200);
        let (title, _) = notice_text(&ended, Some(&long));
        assert!(title.chars().count() < 80, "{title}");
    }
}
