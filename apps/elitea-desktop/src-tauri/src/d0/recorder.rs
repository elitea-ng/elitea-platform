//! What one turn did, as the commit (decision 5c) and the changed-files
//! card need it: the tool steps, the HITL exchanges, the commands run, the
//! paths touched, and a before-image of every file a local file tool was
//! about to change.
//!
//! Before-images cover `write_file`, `edit_file` and `apply_patch`. A file a
//! shell command changes is in the turn's checkpoint (undo works) but not
//! in [`Recorder::changes`]: D0 has no checkpoint diff yet.

use std::collections::BTreeMap;
use std::sync::Mutex;

use elitea_local_tools::workspace::{Intent, Workspace};
use serde::Serialize;
use serde_json::{Map, Value, json};
use similar::{ChangeTag, TextDiff};

/// Largest file a before-image keeps; larger files are reported changed
/// without a diff.
const MAX_IMAGE_BYTES: u64 = 2 * 1024 * 1024;
/// Largest file whose state at the turn's end is hashed (for the per-file
/// revert of an older turn); a larger one cannot be checked, so is refused.
const MAX_HASHED_BYTES: u64 = 64 * 1024 * 1024;

/// What a file held when the turn ended.
#[derive(Clone, Debug, PartialEq, Eq)]
enum PostState {
    Absent,
    Content([u8; 32]),
}
/// The commit's bounds (`LocalTurnWorkReport`).
const MAX_COMMANDS: usize = 500;
const MAX_PATHS: usize = 2000;
const MAX_TOOL_OUTPUT: usize = 64 * 1024;

/// One changed file, as `turn_changes` answers it.
#[derive(Clone, Debug, Serialize, PartialEq, Eq)]
pub struct FileChange {
    pub path: String,
    pub status: &'static str,
    pub added: u64,
    pub removed: u64,
    pub diff: String,
}

#[derive(Default)]
struct State {
    steps: Vec<(String, Map<String, Value>)>,
    hitl: Vec<Value>,
    commands: Vec<Value>,
    paths: Vec<String>,
    enforcement: Option<String>,
    /// Path → content before the turn first changed it (`None`: absent).
    before: BTreeMap<String, Option<Vec<u8>>>,
    /// `(from, to)` of renames a patch made.
    renames: Vec<(String, String)>,
    /// What each changed file held when the turn ended ([`Recorder::seal`]);
    /// a file that could not be read then is missing.
    after: BTreeMap<String, PostState>,
}

#[derive(Default)]
pub struct Recorder {
    state: Mutex<State>,
}

fn now() -> String {
    chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Millis, true)
}

fn truncate(text: &str, max: usize) -> String {
    if text.len() <= max {
        return text.to_owned();
    }
    let mut end = max;
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}…", &text[..end])
}

impl Recorder {
    fn with<T>(&self, work: impl FnOnce(&mut State) -> T) -> Option<T> {
        self.state.lock().ok().map(|mut state| work(&mut state))
    }

    /// A tool call began.
    pub fn tool_started(&self, run_id: &str, tool_name: &str, inputs: &Value) {
        let mut entry = Map::new();
        entry.insert("run_id".into(), json!(run_id));
        entry.insert("tool_name".into(), json!(tool_name));
        entry.insert(
            "tool_inputs".into(),
            if inputs.is_object() {
                inputs.clone()
            } else {
                json!({})
            },
        );
        entry.insert("timestamp_start".into(), json!(now()));
        self.with(|state| state.steps.push((run_id.to_owned(), entry)));
    }

    /// A tool call ended; `error` is its error text, if it failed.
    pub fn tool_finished(&self, run_id: &str, output: &Value, error: Option<&str>) {
        let text = match output {
            Value::String(text) => text.clone(),
            other => other.to_string(),
        };
        self.with(|state| {
            if let Some((_, entry)) = state.steps.iter_mut().rev().find(|(id, _)| id == run_id) {
                entry.insert(
                    "tool_output".into(),
                    json!(truncate(&text, MAX_TOOL_OUTPUT)),
                );
                entry.insert("timestamp_finish".into(), json!(now()));
                entry.insert(
                    "finish_reason".into(),
                    json!(if error.is_some() { "error" } else { "stop" }),
                );
                if let Some(error) = error {
                    entry.insert("error".into(), json!(truncate(error, 4096)));
                }
            }
        });
    }

    pub fn hitl_exchange(&self, exchange: Value) {
        self.with(|state| state.hitl.push(exchange));
    }

    #[must_use]
    pub fn hitl_exchanges(&self) -> Vec<Value> {
        self.with(|state| state.hitl.clone()).unwrap_or_default()
    }

    /// A command the shell tool ran, from its result.
    pub fn command(&self, command: &str, result: &Value) {
        self.with(|state| {
            if state.commands.len() < MAX_COMMANDS {
                let mut entry = json!({ "command": command });
                if let Some(code) = result.get("exit_code").and_then(Value::as_i64) {
                    entry["exit_code"] = json!(code);
                }
                state.commands.push(entry);
            }
            if let Some(level) = result.get("enforcement").and_then(Value::as_str) {
                let rank = |l: &str| match l {
                    "full" => 2,
                    "partial" => 1,
                    _ => 0,
                };
                let weaker = state
                    .enforcement
                    .as_deref()
                    .is_none_or(|current| rank(level) < rank(current));
                if weaker {
                    state.enforcement = Some(level.to_owned());
                }
            }
        });
    }

    /// Before the first change of `paths` (workspace-relative, as the model
    /// named them) in this turn, keep what they hold now.
    pub fn before_change(&self, workspace: &Workspace, paths: &[String]) {
        for path in paths {
            let Ok(resolved) = workspace.resolve(path, Intent::Read) else {
                continue;
            };
            let key = resolved.display_string();
            let known = self
                .with(|state| state.before.contains_key(&key))
                .unwrap_or(true);
            if known {
                continue;
            }
            let image = read_image(workspace, &resolved);
            self.with(|state| {
                state.before.entry(key.clone()).or_insert(image);
                if state.paths.len() < MAX_PATHS && !state.paths.contains(&key) {
                    state.paths.push(key);
                }
            });
        }
    }

    pub fn renamed(&self, from: &str, to: &str) {
        self.with(|state| state.renames.push((from.to_owned(), to.to_owned())));
    }

    /// The commit's `tool_calls` map, keyed by run id.
    #[must_use]
    pub fn tool_calls(&self) -> Map<String, Value> {
        self.with(|state| {
            state
                .steps
                .iter()
                .map(|(id, entry)| (id.clone(), Value::Object(entry.clone())))
                .collect()
        })
        .unwrap_or_default()
    }

    /// The commit's `local_work` report.
    #[must_use]
    pub fn work_report(&self, sandbox_mode: &str) -> Value {
        self.with(|state| {
            let mut report = json!({
                "sandbox_mode": sandbox_mode,
                "commands": state.commands,
                "paths_touched": state.paths,
            });
            if let Some(enforcement) = &state.enforcement {
                report["enforcement"] = json!(enforcement);
            }
            report
        })
        .unwrap_or_else(|| json!({}))
    }

    /// At the turn's end: what each file its file tools changed holds now,
    /// kept as a hash (the per-file revert of an older turn checks it).
    pub fn seal(&self, workspace: &Workspace) {
        let Some(paths) = self.with(|state| state.before.keys().cloned().collect::<Vec<_>>())
        else {
            return;
        };
        let after: BTreeMap<String, PostState> = paths
            .into_iter()
            .filter_map(|path| post_state(workspace, &path).map(|state| (path, state)))
            .collect();
        self.with(|state| state.after = after);
    }

    /// Whether `path` (workspace-relative, as `turn_changes` lists it)
    /// still holds what this turn left in it. `false` when the turn did
    /// not record the file, or it cannot be read now or could not then.
    #[must_use]
    pub fn unchanged_since_turn(&self, workspace: &Workspace, path: &str) -> bool {
        let Ok(resolved) = workspace.resolve(path, Intent::Read) else {
            return false;
        };
        let key = resolved.display_string();
        let Some(Some(then)) = self.with(|state| state.after.get(&key).cloned()) else {
            return false;
        };
        post_state(workspace, &key).is_some_and(|now| now == then)
    }

    /// The files the turn's file tools changed, against their before-images.
    #[must_use]
    pub fn changes(&self, workspace: &Workspace) -> Vec<FileChange> {
        let Some((before, renames)) =
            self.with(|state| (state.before.clone(), state.renames.clone()))
        else {
            return Vec::new();
        };
        let mut changes = Vec::new();
        let mut consumed = Vec::new();
        for (from, to) in &renames {
            let (Some(Some(old)), Some(None)) = (before.get(from), before.get(to)) else {
                continue;
            };
            let now_from = current(workspace, from);
            let now_to = current(workspace, to);
            if let (None, Some(new)) = (now_from, now_to) {
                let mut change = diff(to, Some(old), Some(&new));
                change.status = "renamed";
                changes.push(change);
                consumed.push(from.clone());
                consumed.push(to.clone());
            }
        }
        for (path, old) in &before {
            if consumed.contains(path) {
                continue;
            }
            let new = current(workspace, path);
            if old.as_deref() == new.as_deref() {
                continue;
            }
            changes.push(diff(path, old.as_deref(), new.as_deref()));
        }
        changes
    }
}

fn read_image(
    workspace: &Workspace,
    path: &elitea_local_tools::workspace::WsPath,
) -> Option<Vec<u8>> {
    match workspace.stat(path) {
        Ok(Some(_)) => workspace
            .read(path, MAX_IMAGE_BYTES)
            .ok()
            .map(|file| file.bytes),
        _ => None,
    }
}

/// What `path` holds now; `None` when it cannot be told (unreadable, too
/// large to hash, outside the workspace's view).
fn post_state(workspace: &Workspace, path: &str) -> Option<PostState> {
    let resolved = workspace.resolve(path, Intent::Read).ok()?;
    match workspace.stat(&resolved) {
        Ok(None) => Some(PostState::Absent),
        Ok(Some(_)) => workspace
            .read(&resolved, MAX_HASHED_BYTES)
            .ok()
            .map(|file| PostState::Content(file.stamp.sha256)),
        Err(_) => None,
    }
}

fn current(workspace: &Workspace, path: &str) -> Option<Vec<u8>> {
    let resolved = workspace.resolve(path, Intent::Read).ok()?;
    read_image(workspace, &resolved)
}

fn diff(path: &str, old: Option<&[u8]>, new: Option<&[u8]>) -> FileChange {
    let status = match (old, new) {
        (None, _) => "added",
        (_, None) => "deleted",
        _ => "modified",
    };
    let (Ok(old_text), Ok(new_text)) = (
        std::str::from_utf8(old.unwrap_or_default()),
        std::str::from_utf8(new.unwrap_or_default()),
    ) else {
        // Binary: changed, no line diff.
        return FileChange {
            path: path.to_owned(),
            status,
            added: 0,
            removed: 0,
            diff: String::new(),
        };
    };
    let text_diff = TextDiff::from_lines(old_text, new_text);
    let (mut added, mut removed) = (0, 0);
    for change in text_diff.iter_all_changes() {
        match change.tag() {
            ChangeTag::Insert => added += 1,
            ChangeTag::Delete => removed += 1,
            ChangeTag::Equal => {}
        }
    }
    let unified = text_diff
        .unified_diff()
        .context_radius(3)
        .header(
            &if old.is_some() {
                format!("a/{path}")
            } else {
                "/dev/null".to_owned()
            },
            &if new.is_some() {
                format!("b/{path}")
            } else {
                "/dev/null".to_owned()
            },
        )
        .to_string();
    FileChange {
        path: path.to_owned(),
        status,
        added,
        removed,
        diff: unified,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn changes_are_diffed_against_before_images() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("a.txt"), "one\ntwo\n").unwrap();
        std::fs::write(dir.path().join("gone.txt"), "x\n").unwrap();
        let workspace = Workspace::open(dir.path(), &[]).unwrap();
        let recorder = Recorder::default();
        recorder.before_change(
            &workspace,
            &[
                "a.txt".into(),
                "new.txt".into(),
                "gone.txt".into(),
                "same.txt".into(),
            ],
        );
        std::fs::write(dir.path().join("a.txt"), "one\nTWO\nthree\n").unwrap();
        std::fs::write(dir.path().join("new.txt"), "hi\n").unwrap();
        std::fs::remove_file(dir.path().join("gone.txt")).unwrap();
        // A second capture of a changed path keeps the first image.
        recorder.before_change(&workspace, &["a.txt".into()]);
        let changes = recorder.changes(&workspace);
        let by_path: BTreeMap<_, _> = changes.iter().map(|c| (c.path.as_str(), c)).collect();
        assert_eq!(by_path.len(), 3);
        assert_eq!(by_path["a.txt"].status, "modified");
        assert_eq!((by_path["a.txt"].added, by_path["a.txt"].removed), (2, 1));
        assert!(by_path["a.txt"].diff.contains("-two\n"));
        assert!(by_path["a.txt"].diff.contains("+TWO\n"));
        assert_eq!(by_path["new.txt"].status, "added");
        assert!(by_path["new.txt"].diff.starts_with("--- /dev/null"));
        assert_eq!(by_path["gone.txt"].status, "deleted");
        let report = recorder.work_report("workspace-write");
        assert_eq!(report["paths_touched"].as_array().unwrap().len(), 4);
    }

    #[test]
    fn a_file_is_unchanged_since_the_turn_only_while_it_holds_the_turns_result() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("a.txt"), "before\n").unwrap();
        let workspace = Workspace::open(dir.path(), &[]).unwrap();
        let recorder = Recorder::default();
        recorder.before_change(&workspace, &["a.txt".into(), "made.txt".into()]);
        std::fs::write(dir.path().join("a.txt"), "the turn's\n").unwrap();
        std::fs::write(dir.path().join("made.txt"), "new\n").unwrap();
        recorder.seal(&workspace);
        assert!(recorder.unchanged_since_turn(&workspace, "a.txt"));
        assert!(recorder.unchanged_since_turn(&workspace, "./made.txt"));
        // Edited since (by the person or a later turn): no longer.
        std::fs::write(dir.path().join("a.txt"), "edited later\n").unwrap();
        assert!(!recorder.unchanged_since_turn(&workspace, "a.txt"));
        std::fs::remove_file(dir.path().join("made.txt")).unwrap();
        assert!(!recorder.unchanged_since_turn(&workspace, "made.txt"));
        // A file the turn did not change cannot be vouched for.
        std::fs::write(dir.path().join("other.txt"), "x").unwrap();
        assert!(!recorder.unchanged_since_turn(&workspace, "other.txt"));
    }

    #[test]
    fn the_work_report_keeps_commands_and_the_weakest_enforcement() {
        let recorder = Recorder::default();
        recorder.command(
            "cargo test",
            &json!({"exit_code": 0, "enforcement": "full"}),
        );
        recorder.command("make", &json!({"exit_code": 2, "enforcement": "partial"}));
        recorder.command("ls", &json!({"exit_code": 0, "enforcement": "full"}));
        let report = recorder.work_report("read-only");
        assert_eq!(report["enforcement"], "partial");
        assert_eq!(
            report["commands"][1],
            json!({"command": "make", "exit_code": 2})
        );
        assert_eq!(report["sandbox_mode"], "read-only");
    }

    #[test]
    fn tool_steps_carry_the_trace_keys_the_commit_stores() {
        let recorder = Recorder::default();
        recorder.tool_started("call-1", "read_file", &json!({"path": "a"}));
        recorder.tool_finished("call-1", &json!({"status": "error"}), Some("not found"));
        let calls = recorder.tool_calls();
        let entry = &calls["call-1"];
        assert_eq!(entry["tool_name"], "read_file");
        assert_eq!(entry["error"], "not found");
        assert_eq!(entry["finish_reason"], "error");
        assert!(entry["timestamp_start"].as_str().unwrap().ends_with('Z'));
        assert!(entry["tool_output"].is_string());
    }
}
