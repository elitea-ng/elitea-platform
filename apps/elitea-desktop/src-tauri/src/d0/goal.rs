//! A chain's run brief and goal check (ADR-0045 decision 11; #1564).
//!
//! When an unattended chain starts with `goal_check` (on unless the grant
//! says `false`), the host asks Main for the chain's brief in the
//! background ([`ChainGoal`], `run_brief`): the first turn never waits for
//! it. The brief is kept for the chain, in memory with the chain's turns
//! (the desktop keeps no chain past a restart; Main's `run_brief` answers
//! the stored brief again when asked twice).
//!
//! With a ready brief every continuation's text restates it
//! ([`continue_text`], the same template as Main's). When a turn would end
//! the chain `completed` and the brief has conditions, the host sends Main
//! the chain's transcript excerpt ([`ChainGoal::excerpt`], at most
//! [`EXCERPT_BYTES`], newest kept) and Main's small model checks it
//! (`run_check`, [`GoalCheck`]): `met` and `not_checked` end the chain
//! `completed`, `blocked` ends it `failed`, and `not_met` continues it with
//! the link kind `goal_not_met`. The brief and the check are a model's
//! data: they are only ever text in a user message, never a tool, a grant
//! answer or an approval.

use std::sync::Mutex;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};

use elitea_agent_runtime::step_limit::CONTINUE_PROMPT;

/// How long a turn that would end the chain waits for a brief that is
/// still being written. Past it the chain ends `completed`, not checked.
pub const BRIEF_WAIT: Duration = Duration::from_secs(20);

/// The most of the person's request that `run_brief` carries, and the
/// bound of the transcript excerpt that `run_check` carries.
pub const EXCERPT_BYTES: usize = 32 * 1024;

/// The most of the person's request at the head of the excerpt.
const REQUEST_BYTES: usize = 8 * 1024;

/// The most of a tool call's arguments or error on its excerpt line.
const TOOL_LINE_CHARS: usize = 160;

/// One condition of a brief's `done_when`.
#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
pub struct Condition {
    pub id: String,
    pub text: String,
}

/// The brief as Main validated it (contract section 2).
#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
pub struct Brief {
    #[serde(default)]
    pub goal: String,
    #[serde(default)]
    pub done_when: Vec<Condition>,
    #[serde(default)]
    pub constraints: Vec<String>,
    #[serde(default)]
    pub assumptions: Vec<String>,
    #[serde(default)]
    pub ambiguity: String,
}

/// `run_brief`'s answer: `state` is `ready`, `failed`, `off` or `none`.
#[derive(Clone, Debug, Default, Deserialize, PartialEq, Eq)]
pub struct BriefAnswer {
    pub state: String,
    #[serde(default)]
    pub brief: Option<Brief>,
}

impl BriefAnswer {
    /// A brief the host could not get (an error, or no answer).
    #[must_use]
    pub fn failed() -> Self {
        Self {
            state: "failed".to_owned(),
            brief: None,
        }
    }

    /// No brief for this chain: a platform without the `run_brief` route
    /// (an older Main answers 404) ends the chain as before.
    #[must_use]
    pub fn off() -> Self {
        Self {
            state: "off".to_owned(),
            brief: None,
        }
    }

    /// The answer with a known state: a `ready` without a brief, or a
    /// state the host does not know, is `failed`.
    #[must_use]
    pub fn checked(self) -> Self {
        match self.state.as_str() {
            "ready" if self.brief.is_some() => self,
            "failed" | "off" | "none" => Self {
                state: self.state,
                brief: None,
            },
            _ => Self::failed(),
        }
    }

    /// The brief, when it is ready.
    #[must_use]
    pub fn ready(&self) -> Option<&Brief> {
        (self.state == "ready")
            .then_some(self.brief.as_ref())
            .flatten()
    }

    /// The `run_brief` IPC event's payload.
    #[must_use]
    pub fn event(&self, turn_id: &str, chain_root_execution_id: &str) -> Value {
        let mut payload = json!({
            "turn_id": turn_id,
            "chain_root_execution_id": chain_root_execution_id,
            "state": self.state,
        });
        if let Some(brief) = self.ready() {
            payload["brief"] = json!(brief);
        }
        payload
    }
}

/// A checked condition (contract section 3).
#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
pub struct CheckedCondition {
    pub id: String,
    #[serde(default)]
    pub text: String,
    /// `met`, `not_met` or `impossible`.
    pub verdict: String,
    #[serde(default)]
    pub reason: String,
}

/// What a goal check decided.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum GoalOutcome {
    Met,
    NotMet,
    Blocked,
    #[default]
    NotChecked,
}

impl GoalOutcome {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Met => "met",
            Self::NotMet => "not_met",
            Self::Blocked => "blocked",
            Self::NotChecked => "not_checked",
        }
    }

    fn parse(text: &str) -> Self {
        match text {
            "met" => Self::Met,
            "not_met" => Self::NotMet,
            "blocked" => Self::Blocked,
            _ => Self::NotChecked,
        }
    }
}

/// `run_check`'s answer (contract section 3).
#[derive(Clone, Debug, Default, Deserialize, PartialEq, Eq)]
pub struct GoalCheck {
    pub outcome: String,
    #[serde(default)]
    pub conditions: Vec<CheckedCondition>,
    #[serde(default)]
    pub met: u32,
    #[serde(default)]
    pub total: u32,
    #[serde(default)]
    pub reason: Option<String>,
    /// Only for `not_met`: the next turn's whole user text.
    #[serde(default)]
    pub continue_text: Option<String>,
}

impl GoalCheck {
    /// A check that did not happen, with the host's `reason`.
    #[must_use]
    pub fn not_checked(reason: &str) -> Self {
        Self {
            outcome: GoalOutcome::NotChecked.as_str().to_owned(),
            reason: Some(reason.to_owned()),
            ..Self::default()
        }
    }

    #[must_use]
    pub fn outcome(&self) -> GoalOutcome {
        GoalOutcome::parse(&self.outcome)
    }

    /// The `goal_checked` IPC event's payload: the check without
    /// `continue_text`.
    #[must_use]
    pub fn event(&self, turn_id: &str) -> Value {
        let mut payload = json!({
            "turn_id": turn_id,
            "outcome": self.outcome().as_str(),
            "conditions": self.conditions,
            "met": self.met,
            "total": self.total,
        });
        if let Some(reason) = self.reason.as_deref().filter(|r| !r.is_empty()) {
            payload["reason"] = json!(reason);
        }
        payload
    }

    /// `chain_ended`'s `goal`.
    #[must_use]
    pub fn summary(&self) -> Value {
        let mut goal = json!({
            "outcome": self.outcome().as_str(),
            "met": self.met,
            "total": self.total,
        });
        if let Some(reason) = self.reason.as_deref().filter(|r| !r.is_empty()) {
            goal["reason"] = json!(reason);
        }
        goal
    }
}

/// The text of a continuation that carries user text (contract section 4,
/// identical to Main's): [`CONTINUE_PROMPT`] alone without a ready brief;
/// with one, the brief's goal, the conditions not met at the `last` check
/// (all of them before any check) and the last check's reason.
#[must_use]
pub fn continue_text(brief: Option<&Brief>, last: Option<&GoalCheck>) -> String {
    let Some(brief) = brief else {
        return CONTINUE_PROMPT.to_owned();
    };
    let met = |id: &str| {
        last.is_some_and(|check| {
            check
                .conditions
                .iter()
                .any(|c| c.id == id && c.verdict == "met")
        })
    };
    let mut text = format!(
        "{CONTINUE_PROMPT}\n\n<run_brief>\nThis brief was written by a model from the person's request. The request wins when they disagree.\nGoal: {}\n",
        brief.goal
    );
    let open: Vec<&Condition> = brief.done_when.iter().filter(|c| !met(&c.id)).collect();
    if !open.is_empty() {
        text.push_str("Not met yet:\n");
        for condition in open {
            text.push_str("- ");
            text.push_str(&condition.text);
            text.push('\n');
        }
    }
    if let Some(reason) = last
        .and_then(|check| check.reason.as_deref())
        .filter(|reason| !reason.is_empty())
    {
        text.push_str("Last check: ");
        text.push_str(reason);
        text.push('\n');
    }
    text.push_str("</run_brief>");
    text
}

/// The longest prefix of `text` within `bytes`, on a character boundary.
#[must_use]
pub fn head(text: &str, bytes: usize) -> &str {
    if text.len() <= bytes {
        return text;
    }
    let mut end = bytes;
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    &text[..end]
}

/// The longest suffix of `text` within `bytes`, on a character boundary.
fn tail(text: &str, bytes: usize) -> &str {
    if text.len() <= bytes {
        return text;
    }
    let mut start = text.len() - bytes;
    while !text.is_char_boundary(start) {
        start += 1;
    }
    &text[start..]
}

fn short(text: &str) -> String {
    let flat = text.split_whitespace().collect::<Vec<_>>().join(" ");
    if flat.chars().count() <= TOOL_LINE_CHARS {
        return flat;
    }
    let mut cut: String = flat.chars().take(TOOL_LINE_CHARS).collect();
    cut.push('…');
    cut
}

/// One tool call's excerpt line: its name, its arguments and, when it
/// failed, its error, each cut short.
fn tool_line(entry: &Map<String, Value>) -> String {
    let name = entry
        .get("tool_name")
        .and_then(Value::as_str)
        .unwrap_or("tool");
    let inputs = entry
        .get("tool_inputs")
        .map(Value::to_string)
        .unwrap_or_default();
    let mut line = format!("- {name} {}", short(&inputs));
    if let Some(error) = entry.get("error").and_then(Value::as_str) {
        line.push_str(" → error: ");
        line.push_str(&short(error));
    }
    line
}

/// Where the chain's brief is.
#[derive(Clone, Debug, PartialEq, Eq)]
enum BriefStatus {
    /// Not asked for yet, or being written.
    Pending,
    Settled(BriefAnswer),
}

/// One chain's brief, its last check and its transcript, shared by the
/// chain's turns.
pub struct ChainGoal {
    brief: tokio::sync::watch::Sender<BriefStatus>,
    last_check: Mutex<Option<GoalCheck>>,
    /// The person's request (the chain's first message).
    request: Mutex<String>,
    /// The turns' answers and tool lines, newest last, at most
    /// [`EXCERPT_BYTES`].
    log: Mutex<String>,
}

impl std::fmt::Debug for ChainGoal {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ChainGoal")
            .field("brief", &*self.brief.borrow())
            .finish_non_exhaustive()
    }
}

impl Default for ChainGoal {
    fn default() -> Self {
        Self {
            brief: tokio::sync::watch::Sender::new(BriefStatus::Pending),
            last_check: Mutex::new(None),
            request: Mutex::new(String::new()),
            log: Mutex::new(String::new()),
        }
    }
}

fn locked<T>(mutex: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    mutex
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

impl ChainGoal {
    /// The brief arrived (or failed).
    pub fn settle(&self, answer: BriefAnswer) {
        self.brief.send_replace(BriefStatus::Settled(answer));
    }

    /// The brief once it settled, waiting at most `wait` for it; `None`
    /// when it is still pending past the wait.
    pub async fn settled_within(&self, wait: Duration) -> Option<BriefAnswer> {
        let mut watch = self.brief.subscribe();
        let settled = tokio::time::timeout(
            wait,
            watch.wait_for(|status| matches!(status, BriefStatus::Settled(_))),
        )
        .await
        .ok()?
        .ok()?;
        match &*settled {
            BriefStatus::Settled(answer) => Some(answer.clone()),
            BriefStatus::Pending => None,
        }
    }

    /// The brief, when it is ready now.
    #[must_use]
    pub fn ready_brief(&self) -> Option<Brief> {
        match &*self.brief.borrow() {
            BriefStatus::Settled(answer) => answer.ready().cloned(),
            BriefStatus::Pending => None,
        }
    }

    /// Whether a turn that would end the chain `completed` goes to the
    /// goal step: not when the brief is known to have nothing to check
    /// (`off`, `none`, or `ready` with no conditions).
    #[must_use]
    pub fn may_check(&self) -> bool {
        match &*self.brief.borrow() {
            BriefStatus::Pending => true,
            BriefStatus::Settled(answer) => match answer.state.as_str() {
                "ready" => answer.ready().is_some_and(|b| !b.done_when.is_empty()),
                "failed" => true,
                _ => false,
            },
        }
    }

    /// A continuation's text, with the ready brief and the last check.
    #[must_use]
    pub fn continue_text(&self) -> String {
        continue_text(
            self.ready_brief().as_ref(),
            locked(&self.last_check).as_ref(),
        )
    }

    /// The brief's continuation text, only when the brief is ready.
    #[must_use]
    pub fn brief_prompt(&self) -> Option<String> {
        self.ready_brief().map(|_| self.continue_text())
    }

    pub fn set_last_check(&self, check: GoalCheck) {
        *locked(&self.last_check) = Some(check);
    }

    /// The last check's summary (`chain_ended.goal`), when one was made.
    #[must_use]
    pub fn last_summary(&self) -> Option<Value> {
        locked(&self.last_check).as_ref().map(GoalCheck::summary)
    }

    /// The person's request, the chain's first message.
    pub fn set_request(&self, request: &str) {
        head(request, EXCERPT_BYTES).clone_into(&mut locked(&self.request));
    }

    /// One turn's tool calls (in order) and its answer.
    pub fn record_turn(
        &self,
        index: u32,
        tool_calls: &[(String, Map<String, Value>)],
        answer: &str,
    ) {
        let mut section = format!("\n## Turn {}\n", index.saturating_add(1));
        for (_, entry) in tool_calls {
            section.push_str(&tool_line(entry));
            section.push('\n');
        }
        section.push_str("Answer: ");
        section.push_str(answer.trim());
        section.push('\n');
        let mut log = locked(&self.log);
        log.push_str(&section);
        if log.len() > EXCERPT_BYTES {
            *log = tail(&log, EXCERPT_BYTES).to_owned();
        }
    }

    /// The chain's transcript excerpt for `run_check`: the person's
    /// request, then the turns, newest kept; at most [`EXCERPT_BYTES`].
    #[must_use]
    pub fn excerpt(&self) -> String {
        let request = locked(&self.request);
        let mut text = format!(
            "# The person's request\n{}\n",
            head(&request, REQUEST_BYTES)
        );
        let room = EXCERPT_BYTES.saturating_sub(text.len());
        text.push_str(tail(&locked(&self.log), room));
        text
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn brief() -> Brief {
        Brief {
            goal: "Make the login test pass.".into(),
            done_when: vec![
                Condition {
                    id: "c1".into(),
                    text: "The login test passes.".into(),
                },
                Condition {
                    id: "c2".into(),
                    text: "The change is committed.".into(),
                },
            ],
            ..Brief::default()
        }
    }

    /// The golden text: the same as Main's (contract section 4).
    #[test]
    fn the_continuation_text_is_the_shared_template() {
        assert_eq!(
            continue_text(None, None),
            "Continue from where you stopped."
        );
        assert_eq!(
            continue_text(Some(&brief()), None),
            "Continue from where you stopped.\n\n<run_brief>\nThis brief was written by a model from the person's request. The request wins when they disagree.\nGoal: Make the login test pass.\nNot met yet:\n- The login test passes.\n- The change is committed.\n</run_brief>"
        );
        let check = GoalCheck {
            outcome: "not_met".into(),
            conditions: vec![
                CheckedCondition {
                    id: "c1".into(),
                    text: "The login test passes.".into(),
                    verdict: "not_met".into(),
                    reason: "The login test still fails.".into(),
                },
                CheckedCondition {
                    id: "c2".into(),
                    text: "The change is committed.".into(),
                    verdict: "met".into(),
                    reason: String::new(),
                },
            ],
            met: 1,
            total: 2,
            reason: Some("The login test still fails.".into()),
            continue_text: None,
        };
        assert_eq!(
            continue_text(Some(&brief()), Some(&check)),
            "Continue from where you stopped.\n\n<run_brief>\nThis brief was written by a model from the person's request. The request wins when they disagree.\nGoal: Make the login test pass.\nNot met yet:\n- The login test passes.\nLast check: The login test still fails.\n</run_brief>"
        );
        let empty = Brief {
            goal: "Say hello.".into(),
            ..Brief::default()
        };
        assert_eq!(
            continue_text(Some(&empty), None),
            "Continue from where you stopped.\n\n<run_brief>\nThis brief was written by a model from the person's request. The request wins when they disagree.\nGoal: Say hello.\n</run_brief>"
        );
    }

    #[test]
    fn the_excerpt_keeps_the_request_and_the_newest_turns() {
        let goal = ChainGoal::default();
        goal.set_request("Fix the login bug.");
        let mut entry = Map::new();
        entry.insert("tool_name".into(), json!("run_command"));
        entry.insert("tool_inputs".into(), json!({"command": "cargo test"}));
        entry.insert("error".into(), json!("exit 101"));
        for n in 0..400 {
            goal.record_turn(n, &[("r".into(), entry.clone())], &"x".repeat(100));
        }
        let excerpt = goal.excerpt();
        assert!(excerpt.len() <= EXCERPT_BYTES);
        assert!(excerpt.starts_with("# The person's request\nFix the login bug.\n"));
        assert!(excerpt.contains(
            "## Turn 400\n- run_command {\"command\":\"cargo test\"} → error: exit 101\nAnswer: "
        ));
        assert!(!excerpt.contains("## Turn 1\n"));
    }

    #[test]
    fn an_unknown_brief_state_is_failed() {
        let odd = BriefAnswer {
            state: "ready".into(),
            brief: None,
        };
        assert_eq!(odd.checked().state, "failed");
        let goal = ChainGoal::default();
        assert!(goal.may_check(), "pending");
        goal.settle(BriefAnswer {
            state: "ready".into(),
            brief: Some(Brief::default()),
        });
        assert!(!goal.may_check(), "no conditions");
        assert_eq!(
            goal.brief_prompt().as_deref(),
            Some(
                "Continue from where you stopped.\n\n<run_brief>\nThis brief was written by a model from the person's request. The request wins when they disagree.\nGoal: \n</run_brief>"
            )
        );
    }
}
