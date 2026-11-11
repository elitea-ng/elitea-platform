//! A cloud sub-agent's live lane (ADR-0050 decision 4, #1823).
//!
//! The cloud [`super::subagents::WorkerSubAgents`] host hands each child's
//! lifecycle here: its start (with the profile's display name and avatar
//! key), its own tool calls and their results, its text, and its end. The
//! children run in their own tasks while the parent may sit in `wait_agent`,
//! so the lane is a bounded queue that wakes the run's event loop
//! (`execution::native_agent_lifecycle`), which projects every queued entry
//! as one `agent_subagent` node event on the run's own output stream: the
//! same path, fence and replay log as the parent's tool frames. Main
//! projects the frames onto the answer's `meta.subagents_v1` so a reload
//! shows the settled lane.
//!
//! Bounds: at most [`MAX_EVENTS_PER_CHILD`] tool and text entries per child
//! run (its start and end always go), [`MAX_PENDING`] queued entries; tool
//! arguments are masked by key (as the parent's frames) and every text is
//! summarised and clipped. Nothing here is trusted text: clients render it
//! as plain text.

use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::sync::Mutex;

use adk_rust::{Event, Part};
use elitea_agent_runtime::subagents::{ChildIdentity, ChildReport};
use serde_json::{Map, Value, json};

/// The node event type a lane entry is projected as.
pub(crate) const SUBAGENT_EVENT: &str = "agent_subagent";
/// The `response_metadata` key that carries one entry.
pub(crate) const SUBAGENT_METADATA_KEY: &str = "subagent_v1";
/// The most entries waiting for the event loop; past it a tool or text
/// entry is dropped (a start or an end never is).
pub(crate) const MAX_PENDING: usize = 512;
/// The most tool and text entries of one child run.
pub(crate) const MAX_EVENTS_PER_CHILD: usize = 48;
/// The most entries projected into one batch.
pub(crate) const MAX_ENTRIES_PER_BATCH: usize = 32;
const MAX_ARGS_SUMMARY_CHARS: usize = 160;
const MAX_TOOL_SUMMARY_CHARS: usize = 240;
const MAX_TEXT_CHARS: usize = 600;
const MAX_RESULT_CHARS: usize = 2_000;
const MAX_NAME_CHARS: usize = 120;
/// The error code of a child still running when the run ended.
const CANCELLED_WITH_RUN: &str = "subagent_cancelled";

#[derive(Default)]
struct LaneState {
    queue: VecDeque<Value>,
    /// Tool and text entries queued for each child's current run.
    counts: BTreeMap<String, usize>,
    /// Children started and not settled.
    running: BTreeSet<String>,
}

/// One execution's sub-agent lane queue.
#[derive(Default)]
pub(crate) struct SubAgentLane {
    state: Mutex<LaneState>,
    notify: tokio::sync::Notify,
}

impl SubAgentLane {
    /// The child started.
    pub(crate) fn started(&self, child: &ChildIdentity) {
        let mut entry = json!({
            "kind": "started",
            "agent_id": child.agent_id,
            "parent_call_id": child.parent_call_id,
            "ordinal": child.ordinal,
            // 1 for a child of the run's agent, 2 for a grandchild
            // (#1824, #1872): Main nests a depth-2 lane under its parent's.
            "depth": child.depth,
        });
        put_text(
            &mut entry,
            "parent_agent_id",
            child.parent_agent_id.as_deref(),
            MAX_NAME_CHARS,
        );
        put_text(&mut entry, "label", child.label.as_deref(), MAX_NAME_CHARS);
        put_text(
            &mut entry,
            "display_name",
            child.record.display_name.as_deref(),
            MAX_NAME_CHARS,
        );
        put_text(
            &mut entry,
            "avatar",
            child.record.avatar.as_deref(),
            MAX_NAME_CHARS,
        );
        self.push(&child.agent_id, entry, Mark::Start);
    }

    /// A settled child runs again (`send_input`).
    pub(crate) fn resumed(&self, child: &ChildIdentity) {
        let entry = json!({"kind": "resumed", "agent_id": child.agent_id});
        self.push(&child.agent_id, entry, Mark::Start);
    }

    /// One of the child's adk events: its tool calls, their results and its
    /// final (non-partial) text.
    pub(crate) fn event(&self, child: &ChildIdentity, event: &Event) {
        for call in event.tool_calls() {
            let Some(call_id) = call.call_id else {
                continue;
            };
            let masked = crate::agents::events::mask_sensitive_arguments(call.args, 0)
                .unwrap_or(Value::Null);
            let entry = json!({
                "kind": "tool_start",
                "agent_id": child.agent_id,
                "call_id": clip(call_id, MAX_NAME_CHARS),
                "tool": clip(call.name, MAX_NAME_CHARS),
                "args_summary": args_summary(&masked),
            });
            self.push(&child.agent_id, entry, Mark::Item);
        }
        for result in event.tool_results() {
            let Some(call_id) = result.call_id else {
                continue;
            };
            let (ok, summary) = result_summary(result.response);
            let entry = json!({
                "kind": "tool_end",
                "agent_id": child.agent_id,
                "call_id": clip(call_id, MAX_NAME_CHARS),
                "ok": ok,
                "summary": summary,
            });
            self.push(&child.agent_id, entry, Mark::Item);
        }
        if let Some(text) = model_text(event) {
            let entry = json!({
                "kind": "text",
                "agent_id": child.agent_id,
                "text": clip(&text, MAX_TEXT_CHARS),
            });
            self.push(&child.agent_id, entry, Mark::Item);
        }
    }

    /// The child settled.
    pub(crate) fn finished(&self, child: &ChildIdentity, report: &ChildReport) {
        let mut entry = json!({
            "kind": "finished",
            "agent_id": child.agent_id,
            "status": report.status.as_str(),
            "steps": report.steps,
            "input_tokens": report.input_tokens,
            "output_tokens": report.output_tokens,
        });
        put_text(
            &mut entry,
            "result",
            report.result.as_deref(),
            MAX_RESULT_CHARS,
        );
        put_text(&mut entry, "error", report.error, MAX_NAME_CHARS);
        let mark = if report.status.is_settled() {
            Mark::End
        } else {
            Mark::Item
        };
        self.push(&child.agent_id, entry, mark);
    }

    /// The run ended: every child still running is stopped with it.
    pub(crate) fn close_running(&self) {
        let running: Vec<String> = match self.state.lock() {
            Ok(state) => state.running.iter().cloned().collect(),
            Err(_) => return,
        };
        for agent_id in running {
            let entry = json!({
                "kind": "finished",
                "agent_id": agent_id,
                "status": "cancelled",
                "error": CANCELLED_WITH_RUN,
            });
            self.push(&agent_id, entry, Mark::End);
        }
    }

    /// At most [`MAX_ENTRIES_PER_BATCH`] queued entries, oldest first.
    pub(crate) fn drain(&self) -> Vec<Value> {
        let Ok(mut state) = self.state.lock() else {
            return Vec::new();
        };
        let take = state.queue.len().min(MAX_ENTRIES_PER_BATCH);
        let drained: Vec<Value> = state.queue.drain(..take).collect();
        if !state.queue.is_empty() {
            // More than one batch waits: the loop comes back for it.
            self.notify.notify_one();
        }
        drained
    }

    /// Wait until an entry is queued (a permit is kept, so a push between
    /// two waits is never lost).
    pub(crate) async fn notified(&self) {
        self.notify.notified().await;
    }

    fn push(&self, agent_id: &str, entry: Value, mark: Mark) {
        let Ok(mut state) = self.state.lock() else {
            return;
        };
        match mark {
            Mark::Start => {
                state.counts.insert(agent_id.to_owned(), 0);
                state.running.insert(agent_id.to_owned());
            }
            Mark::Item => {
                if !state.running.contains(agent_id) {
                    return;
                }
                let queued = state.queue.len();
                let count = state.counts.entry(agent_id.to_owned()).or_insert(0);
                if *count >= MAX_EVENTS_PER_CHILD || queued >= MAX_PENDING {
                    return;
                }
                *count += 1;
            }
            Mark::End => {
                if !state.running.remove(agent_id) {
                    return;
                }
            }
        }
        state.queue.push_back(entry);
        drop(state);
        self.notify.notify_one();
    }
}

#[derive(Clone, Copy)]
enum Mark {
    Start,
    Item,
    End,
}

fn put_text(entry: &mut Value, key: &str, value: Option<&str>, max_chars: usize) {
    if let Some(value) = value.filter(|value| !value.trim().is_empty())
        && let Value::Object(object) = entry
    {
        object.insert(key.to_owned(), Value::String(clip(value, max_chars)));
    }
}

/// `text` cut to `max_chars` characters (an ellipsis marks a cut), with
/// control characters other than newlines and tabs dropped.
fn clip(text: &str, max_chars: usize) -> String {
    let mut out = String::new();
    for (index, character) in text.chars().enumerate() {
        if index == max_chars {
            out.pop();
            out.push('…');
            break;
        }
        if character.is_control() && character != '\n' && character != '\t' {
            continue;
        }
        out.push(character);
    }
    out
}

/// One tool call's arguments as one short line: its first telling string
/// argument, or its compact JSON.
fn args_summary(args: &Value) -> String {
    if let Value::Object(object) = args {
        for key in [
            "command",
            "path",
            "pattern",
            "query",
            "url",
            "file_path",
            "name",
        ] {
            if let Some(value) = object.get(key).and_then(Value::as_str) {
                return clip(&single_line(value), MAX_ARGS_SUMMARY_CHARS);
            }
        }
        if object.is_empty() {
            return String::new();
        }
    }
    if args.is_null() {
        return String::new();
    }
    clip(&single_line(&args.to_string()), MAX_ARGS_SUMMARY_CHARS)
}

fn single_line(text: &str) -> String {
    text.split_whitespace().collect::<Vec<_>>().join(" ")
}

/// Whether a tool result is a success, and its first characters.
fn result_summary(response: &Value) -> (bool, String) {
    let object: Option<&Map<String, Value>> = response.as_object();
    if let Some(error) = object
        .and_then(|value| value.get("error"))
        .and_then(Value::as_str)
    {
        return (false, clip(&single_line(error), MAX_TOOL_SUMMARY_CHARS));
    }
    let text = match response {
        Value::String(text) => text.clone(),
        Value::Object(object) if object.len() == 1 => match object.values().next() {
            Some(Value::String(text)) => text.clone(),
            _ => response.to_string(),
        },
        _ => response.to_string(),
    };
    (true, clip(&single_line(&text), MAX_TOOL_SUMMARY_CHARS))
}

/// The child's own text of a final (non-partial) model event.
fn model_text(event: &Event) -> Option<String> {
    if event.llm_response.partial {
        return None;
    }
    let content = event.llm_response.content.as_ref()?;
    if content.role != "model" {
        return None;
    }
    let text: String = content
        .parts
        .iter()
        .filter_map(|part| match part {
            Part::Text { text } => Some(text.as_str()),
            _ => None,
        })
        .collect();
    (!text.trim().is_empty()).then_some(text)
}

#[cfg(test)]
#[path = "subagent_lane_tests.rs"]
mod tests;
