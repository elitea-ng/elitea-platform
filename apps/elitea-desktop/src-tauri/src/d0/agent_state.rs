//! A local turn's live avatar state (ADR-0061 decisions 4a and 4b, #3057).
//!
//! The cloud derives an `agent_state` frame per participant from the
//! runtime's frames (elitea-main `participant_frames.go`). The desktop host
//! derives the same frame from its own `agent://event` stream, here, in one
//! table: every event a turn sends passes through [`AvatarTracker::observe`],
//! and a change of the agent's state or activity becomes one `agent_state`
//! event `{participant_id, state, activity?}`. The table's rows are shared
//! with Main's in `libs/proto/contracts/agent-state-activity-v1.json`, so the
//! two producers cannot drift.
//!
//! The activity never carries command text or arguments: `tool` is the
//! tool's name and `program` the command's program name (the basename of
//! its first word that is not a `NAME=value` assignment), each sanitised to
//! `[A-Za-z0-9_.:-]` and cut to [`MAX_NAME`] characters.

use serde::Serialize;
use serde_json::{Value, json};

/// The event kind the webview receives.
pub const KIND: &str = "agent_state";

/// The longest tool or program name an activity carries.
pub const MAX_NAME: usize = 64;

/// The eight states of client contract 1.38 (`AGENT_STATE` in client-core).
pub mod state {
    pub const IDLE: &str = "idle";
    pub const THINKING: &str = "thinking";
    pub const SPEAKING: &str = "speaking";
    pub const WORKING: &str = "working";
    pub const WAITING: &str = "waiting";
    pub const SUCCESS: &str = "success";
    pub const ERROR: &str = "error";
    pub const WAKING: &str = "waking";
}

/// What the agent does in its state (ADR-0061 decision 4a); every field is
/// optional and absent from the wire when unset.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize)]
pub struct Activity {
    /// The tool's sanitised name; only with `working`.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub tool: Option<String>,
    /// The tool's category (`tool-categories-v1.json`), when the host knows it.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub tool_kind: Option<&'static str>,
    /// A command's program name (never its arguments): "Running cargo".
    #[serde(skip_serializing_if = "Option::is_none")]
    pub program: Option<String>,
    /// `person` or `agent`; only with `waiting`.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub waiting_on: Option<&'static str>,
    /// The agent waited on, consulted or handed to. Set by the local
    /// consults and hand-offs of ADR-0061 decision 3 (#3064, #3065).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub peer_participant_id: Option<i64>,
    /// The receivers of a hand-off, on the frame that ends the handing turn.
    /// Set by the local `hand_floor` runner (#3064).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub handoff_to: Option<Vec<i64>>,
}

impl Activity {
    fn is_empty(&self) -> bool {
        *self == Self::default()
    }
}

/// One derived state: the state and what the agent does in it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Derived {
    pub state: &'static str,
    pub activity: Activity,
}

impl Derived {
    fn bare(state: &'static str) -> Self {
        Self {
            state,
            activity: Activity::default(),
        }
    }
}

/// `value` with every character outside `[A-Za-z0-9_.:-]` replaced by `_`,
/// cut to [`MAX_NAME`] bytes (as Main does); `None` when no letter or digit
/// is left.
#[must_use]
pub fn sanitise(value: &str) -> Option<String> {
    let clean: String = value
        .chars()
        .take(MAX_NAME)
        .map(|c| {
            if c.is_ascii_alphanumeric() || matches!(c, '_' | '.' | ':' | '-') {
                c
            } else {
                '_'
            }
        })
        .collect();
    // Main's rule: a name with no letter or digit names nothing.
    clean
        .chars()
        .any(|c| c.is_ascii_alphanumeric())
        .then_some(clean)
}

/// A command line's program name: the basename of its first word that is
/// not an environment assignment (`TOKEN=… cargo test` names `cargo`).
#[must_use]
pub fn program_of(command: &str) -> Option<String> {
    let word = command
        .split_whitespace()
        .find(|word| !word.contains('='))?
        .trim_matches(|c| c == '"' || c == '\'');
    let base = word.rsplit(['/', '\\']).next().unwrap_or(word);
    sanitise(base)
}

/// The tool category of a local tool, when it has one (`tool-categories-v1`
/// lists toolkit tools; the desktop's own tools are classified here).
#[must_use]
pub fn local_tool_kind(tool: &str) -> Option<&'static str> {
    match tool {
        "read_file" | "list_tree" | "search_files" | "find_files" | "read_document"
        | "read_output" | "git_status" | "git_diff" | "git_log" | "git_branches" | "web_fetch"
        | "web_search" | "spawn_agent" => Some("read"),
        "write_file" | "edit_file" | "multi_edit" | "apply_patch" | "git_commit" => Some("write"),
        "run_command" | "run_tests" => Some("code_execution"),
        _ => None,
    }
}

fn is_command_tool(tool: &str) -> bool {
    tool == "run_command"
}

/// The state an event moves the turn's agent to; `None` for an event that
/// does not move it. The desktop column of ADR-0061 decision 4b.
#[must_use]
pub fn derive(kind: &str, payload: &Value) -> Option<Derived> {
    let text = |key: &str| payload.get(key).and_then(Value::as_str);
    match kind {
        "status" => match text("phase")? {
            "running" => Some(Derived::bare(state::THINKING)),
            "cancelled" => Some(Derived::bare(state::IDLE)),
            _ => None,
        },
        // A continuation of this agent's own chain. A floor continuation
        // names the NEXT agent, whose own turn reports its state.
        "turn_continued" if text("kind") != Some(super::floor::CONTINUED_KIND) => {
            Some(Derived::bare(state::WAKING))
        }
        "thinking_delta" | "tool_result" | "subagent_finished" => {
            Some(Derived::bare(state::THINKING))
        }
        "text_delta" => Some(Derived::bare(state::SPEAKING)),
        "tool_call" => {
            let tool = text("tool").unwrap_or_default();
            Some(Derived {
                state: state::WORKING,
                activity: Activity {
                    tool: sanitise(tool),
                    tool_kind: local_tool_kind(tool),
                    program: if is_command_tool(tool) {
                        text("args_summary").and_then(program_of)
                    } else {
                        None
                    },
                    ..Activity::default()
                },
            })
        }
        "process_started" => Some(Derived {
            state: state::WORKING,
            activity: Activity {
                tool: Some("run_command".to_owned()),
                tool_kind: local_tool_kind("run_command"),
                program: text("command").and_then(program_of),
                ..Activity::default()
            },
        }),
        "approval_request" => Some(Derived {
            state: state::WAITING,
            activity: Activity {
                waiting_on: Some("person"),
                ..Activity::default()
            },
        }),
        "subagent_started" => Some(Derived {
            state: state::WAITING,
            activity: Activity {
                waiting_on: Some("agent"),
                ..Activity::default()
            },
        }),
        // A cancelled turn's `done` commits nothing: it stays idle.
        "done" if text("commit") != Some("none") => Some(Derived::bare(state::SUCCESS)),
        "error" | "commit_failed" => Some(Derived::bare(state::ERROR)),
        "floor_stopped" | "chain_ended" => Some(Derived::bare(state::IDLE)),
        _ => None,
    }
}

/// One turn's avatar: whose turn it is, and the state it last reported.
#[derive(Debug, Default)]
pub struct AvatarTracker {
    participant_id: Option<i64>,
    last: Option<Derived>,
}

impl AvatarTracker {
    /// The participant whose turn this is, once Main named it.
    pub fn set_participant(&mut self, participant_id: i64) {
        if participant_id > 0 {
            self.participant_id = Some(participant_id);
        }
    }

    /// The `agent_state` payload `kind` causes, only when the agent's state
    /// or activity changed, and only once the turn's participant is known.
    pub fn observe(&mut self, kind: &str, payload: &Value) -> Option<Value> {
        let participant_id = self.participant_id?;
        let derived = derive(kind, payload)?;
        if self.last.as_ref() == Some(&derived) {
            return None;
        }
        let mut frame = json!({ "participant_id": participant_id, "state": derived.state });
        if !derived.activity.is_empty() {
            frame["activity"] = serde_json::to_value(&derived.activity).ok()?;
        }
        self.last = Some(derived);
        Some(frame)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const FIXTURE: &str =
        include_str!("../../../../../libs/proto/contracts/agent-state-activity-v1.json");

    fn rows() -> Vec<Value> {
        let fixture: Value = serde_json::from_str(FIXTURE).expect("the fixture is JSON");
        fixture["desktop"].as_array().expect("desktop rows").clone()
    }

    fn row(name: &str) -> Value {
        rows()
            .into_iter()
            .find(|row| row["name"] == name)
            .unwrap_or_else(|| panic!("no fixture row {name}"))
    }

    /// The fixture row `name` gives its state and activity.
    fn check(name: &str) {
        let row = row(name);
        let kind = row["event"]["kind"].as_str().expect("kind");
        let derived =
            derive(kind, &row["event"]["payload"]).unwrap_or_else(|| panic!("{name}: no state"));
        assert_eq!(derived.state, row["state"], "{name}");
        let activity = serde_json::to_value(&derived.activity).expect("activity");
        let expected = row.get("activity").cloned().unwrap_or_else(|| json!({}));
        assert_eq!(activity, expected, "{name}");
    }

    #[test]
    fn status_running_is_thinking() {
        check("status running");
    }

    #[test]
    fn a_chain_continuation_is_waking() {
        check("turn_continued");
        // The message's next agent reports from its own turn.
        assert!(
            derive(
                "turn_continued",
                &json!({"kind": "floor", "participant_id": 88})
            )
            .is_none()
        );
    }

    #[test]
    fn thinking_delta_is_thinking() {
        check("thinking_delta");
    }

    #[test]
    fn text_delta_is_speaking() {
        check("text_delta");
    }

    #[test]
    fn tool_call_is_working_with_its_tool() {
        check("tool_call");
    }

    #[test]
    fn run_command_names_its_program_only() {
        check("tool_call run_command");
    }

    #[test]
    fn tool_result_is_thinking() {
        check("tool_result");
    }

    #[test]
    fn a_background_process_is_working() {
        check("process_started");
    }

    #[test]
    fn an_approval_waits_on_the_person() {
        check("approval_request");
    }

    #[test]
    fn a_consult_waits_on_an_agent() {
        check("subagent_started");
    }

    #[test]
    fn a_finished_consult_is_thinking_again() {
        check("subagent_finished");
    }

    #[test]
    fn done_is_success() {
        check("done");
        check("done with floor_next");
        // A cancelled turn's `done` keeps it idle.
        assert!(derive("done", &json!({"commit": "none"})).is_none());
    }

    #[test]
    fn error_is_error() {
        check("error");
    }

    #[test]
    fn a_failed_commit_is_error() {
        check("commit_failed");
    }

    #[test]
    fn a_cancel_is_idle() {
        check("status cancelled");
    }

    #[test]
    fn a_stopped_floor_is_idle() {
        check("floor_stopped");
    }

    #[test]
    fn an_ended_chain_is_idle() {
        check("chain_ended");
    }

    /// Every desktop row of the shared fixture has a test above.
    #[test]
    fn every_fixture_row_is_covered() {
        let covered = [
            "status running",
            "turn_continued",
            "thinking_delta",
            "text_delta",
            "tool_call",
            "tool_call run_command",
            "tool_result",
            "process_started",
            "approval_request",
            "subagent_started",
            "subagent_finished",
            "done",
            "done with floor_next",
            "error",
            "commit_failed",
            "status cancelled",
            "floor_stopped",
            "chain_ended",
        ];
        for row in rows() {
            let name = row["name"].as_str().expect("name");
            assert!(covered.contains(&name), "fixture row {name} has no test");
            check(name);
        }
    }

    #[test]
    fn names_are_sanitised_and_cut() {
        assert_eq!(sanitise("read file").as_deref(), Some("read_file"));
        assert_eq!(sanitise("/etc/passwd").as_deref(), Some("_etc_passwd"));
        let long = "a".repeat(80);
        assert_eq!(sanitise(&long).map(|name| name.len()), Some(MAX_NAME));
        assert!(sanitise("  ").is_none());
        assert!(sanitise("../--").is_none());
        assert_eq!(program_of("/usr/bin/cargo test").as_deref(), Some("cargo"));
        assert_eq!(
            program_of("C:\\tools\\npm.cmd run").as_deref(),
            Some("npm.cmd")
        );
        assert_eq!(
            program_of("API_KEY=sk-secret-123 cargo test").as_deref(),
            Some("cargo")
        );
        let pattern = regex::Regex::new(r"^[A-Za-z0-9_.:-]{1,64}$").expect("pattern");
        for name in ["read file", "/a/b c", &long, "naïve", "x;rm -rf /"] {
            assert!(pattern.is_match(&sanitise(name).expect(name)), "{name}");
        }
    }

    /// No argument of a command reaches an emitted state.
    #[test]
    fn no_command_text_is_ever_emitted() {
        let mut tracker = AvatarTracker::default();
        tracker.set_participant(77);
        let mut emitted = Vec::new();
        for (kind, payload) in [
            ("status", json!({"phase": "running"})),
            (
                "tool_call",
                json!({"call_id": "c", "tool": "run_command", "args_summary": "TOKEN=sk-secret-123 cargo test --password hunter2 /home/me/secret", "remote": false}),
            ),
            (
                "process_started",
                json!({"process_id": "p", "call_id": "c", "command": "curl -H 'Authorization: sk-secret-123' https://x", "tty": false}),
            ),
            (
                "tool_call",
                json!({"call_id": "d", "tool": "a tool /with/a path and a very long name that goes past sixty four characters", "args_summary": "sk-secret-123"}),
            ),
        ] {
            emitted.extend(tracker.observe(kind, &payload));
        }
        let text = serde_json::to_string(&emitted).expect("json");
        for secret in [
            "sk-secret",
            "hunter2",
            "password",
            "/home",
            "Authorization",
            "test",
        ] {
            assert!(!text.contains(secret), "{secret} leaked: {text}");
        }
        assert_eq!(emitted[1]["activity"]["program"], "cargo");
        assert_eq!(emitted[2]["activity"]["program"], "curl");
        let tool = emitted[3]["activity"]["tool"].as_str().expect("tool");
        assert_eq!(tool.len(), MAX_NAME);
        assert!(!tool.contains(' ') && !tool.contains('/'));
    }

    #[test]
    fn a_state_is_emitted_on_change_only_and_only_with_a_participant() {
        let mut tracker = AvatarTracker::default();
        let running = json!({"phase": "running"});
        assert!(tracker.observe("status", &running).is_none());
        tracker.set_participant(77);
        let first = tracker.observe("status", &running).expect("thinking");
        assert_eq!(first, json!({"participant_id": 77, "state": "thinking"}));
        assert!(
            tracker
                .observe("thinking_delta", &json!({"text": "x"}))
                .is_none()
        );
        assert!(tracker.observe("usage", &json!({})).is_none());
        let speaking = tracker.observe("text_delta", &json!({"text": "a"}));
        assert_eq!(speaking.expect("speaking")["state"], "speaking");
        assert!(
            tracker
                .observe("text_delta", &json!({"text": "b"}))
                .is_none()
        );
        // Another tool is another activity, in the same state.
        let read = json!({"tool": "read_file"});
        let grep = json!({"tool": "search_files"});
        assert!(tracker.observe("tool_call", &read).is_some());
        assert!(tracker.observe("tool_call", &read).is_none());
        assert!(tracker.observe("tool_call", &grep).is_some());
    }
}
