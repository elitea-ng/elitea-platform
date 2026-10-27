//! Idle compaction (IPC.md, "Idle compaction"): a long thread left idle is
//! compacted while the provider's prompt cache still holds its last
//! request, so the summary reads that whole prefix at the cached price
//! (0.1× the input price) instead of paying for it again, uncached, on the
//! next message.
//!
//! * **When.** After a local turn's commit settled, a check is scheduled at
//!   the turn's last model request START plus the settings' delay
//!   ([`CompactionSettings::idle_delay`]: 4 minutes for the five-minute
//!   cache, 55 for the one-hour one, or the custom time). None when `auto`
//!   is off, `idle` is `off`, or a custom time is past the cache.
//! * **Whether** ([`decide`]): no turn runs or waits in the workspace and
//!   none started on the thread since; the conversation's own tokens (the
//!   last request's `message_tokens`, the context meter's) are at least
//!   [`MIN_MESSAGE_TOKENS`]; the model is a paid provider with a prompt
//!   cache (`catalogue::caches_paid_prompts`); the wall clock is still
//!   inside the last request's cache lifetime (a Mac that slept past it
//!   finds the cache gone: skipped); the last request was answered without
//!   a tool call; and the composer holds no unsent text
//!   ([`IdleCompactor::hold`]). A held thread is checked again when the
//!   hold is released after the scheduled time, under the same rules.
//! * **The summary** is the thread's last request, byte for byte (same
//!   model, system message, tools, history, breakpoints), then the model's
//!   answer to it and one user message asking for the v1 continuation record
//!   (`context_summary::continuation_prompt`), with only the output limit
//!   changed (`model::continuation_body`). An answer that calls a tool is
//!   refused; one that ignores the template is asked for once more, then
//!   kept as plain text, as a step compaction does. The call is quiet, on
//!   the summary lane, under the last turn's execution id.
//! * **The result** is stored as a step compaction's is: the summary stands
//!   for everything before the cut `fold_point` chooses (about the newest
//!   20k tokens stay verbatim), as an adk `EventCompaction` marker on the
//!   thread's last committed turn (`SessionStore::append_marker`), so the
//!   next turn starts from it. `workspace-context-compacted` says so.
//! * **Failure** of any kind is logged and leaves the thread as it was: the
//!   next message's threshold compaction still protects it.
//! * **Cancelled** by a new turn on the thread, a settings change, the
//!   thread's deletion, the workspace's removal, a sign-out and the app's
//!   exit.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime};

use adk_core::{Content, Part};
use adk_session::{GetRequest, SessionService};
use elitea_agent_runtime::context_budget::RequestContextBudget;
use elitea_agent_runtime::context_management::{
    CompactionPolicy, SUMMARY_MAX_OUTPUT_TOKENS, TEMPLATE_RETRY, contents_tokens, files_entry,
    fold_point, folded_compaction, summary_of, visible_events,
};
use elitea_agent_runtime::context_summary;
use elitea_agent_runtime::host::ModelRequest;
use serde_json::{Value, json};

use super::context::{MeasuredParts, files_called, marker_for};
use super::events::{ContextMeter, EventEmitter, Silent, TurnEvents};
use super::model::{Appended, CapturedRequest, GatewayTransport, RequestCapture};
use super::session::AGENT_AUTHOR;
use crate::compaction_settings::{CacheTtl, CompactionSettings};
use crate::sessions::SessionStore;

/// The event an idle compaction that landed emits (`emit_app`).
pub const EVENT_NAME: &str = "workspace-context-compacted";

/// Below this many conversation tokens a thread is not worth an idle
/// summary: the next message's cache miss costs less than the summary.
pub const MIN_MESSAGE_TOKENS: u64 = 60_000;

/// A thread: its workspace and the conversation as the UI names it (its id
/// or UUID, `turn::conversation_key`).
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct ThreadKey {
    pub workspace_id: String,
    pub conversation: String,
}

/// What a turn's model leaves for its thread's idle compaction (set where
/// the step compaction is composed, `turn.rs` `compacting_model`).
pub struct IdleBasis {
    /// The summary's transport: the turn's, quiet, on the summary lane.
    pub transport: GatewayTransport,
    /// The agent's model request (the model the summary goes to).
    pub model: ModelRequest,
    /// The turn's last request as sent, and its answer.
    pub capture: Arc<RequestCapture>,
    /// The parts of the last request the budget measured (the meter's).
    pub parts: MeasuredParts,
    pub policy: CompactionPolicy,
    pub budget: RequestContextBudget,
    /// The model's output limit (the summary asks for at most
    /// [`SUMMARY_MAX_OUTPUT_TOKENS`] of it).
    pub max_output_tokens: u32,
    /// A paid provider with a prompt cache.
    pub prompt_cache: bool,
    pub window_fallback: bool,
}

/// The thread's durable session (an in-memory one is gone after its turn).
#[derive(Clone)]
pub struct IdleSession {
    pub service: Arc<dyn SessionService>,
    pub store: Arc<SessionStore>,
    pub key: String,
}

/// What [`decide`] looks at when a scheduled check fires.
#[derive(Clone, Copy, Debug)]
pub struct IdleState {
    /// The thread's turn count when the check was scheduled, and now.
    pub scheduled_generation: u64,
    pub generation: u64,
    /// A turn runs, or its commit is still settling, in the workspace.
    pub turn_running: bool,
    /// The composer holds unsent text.
    pub held: bool,
    /// The last request's conversation tokens (`None`: not measured).
    pub message_tokens: Option<u64>,
    pub prompt_cache: bool,
    /// The last request was answered, without a tool call.
    pub answered: bool,
    pub request_started: SystemTime,
    pub cache_ttl: CacheTtl,
}

/// What a check decides.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum IdleDecision {
    Compact,
    /// Held by unsent text: checked again on release.
    Held,
    /// Not now, nor later for this turn; the reason, for the log.
    Skip(&'static str),
}

/// Whether to compact now (module docs, "Whether"); `now` is the wall clock.
#[must_use]
pub fn decide(state: &IdleState, now: SystemTime) -> IdleDecision {
    if state.generation != state.scheduled_generation {
        return IdleDecision::Skip("a newer turn started on the thread");
    }
    if state.turn_running {
        return IdleDecision::Skip("a turn runs in the workspace");
    }
    if !state.prompt_cache {
        return IdleDecision::Skip("the model has no paid prompt cache");
    }
    if state
        .message_tokens
        .is_none_or(|tokens| tokens < MIN_MESSAGE_TOKENS)
    {
        return IdleDecision::Skip("the conversation is too short to be worth it");
    }
    if !state.answered {
        return IdleDecision::Skip("the last request was not answered");
    }
    // A clock set back reads as "before the request": its cache is unknown.
    match now.duration_since(state.request_started) {
        Ok(elapsed) if elapsed < state.cache_ttl.duration() => {}
        _ => return IdleDecision::Skip("the prompt cache has expired (the Mac may have slept)"),
    }
    if state.held {
        return IdleDecision::Held;
    }
    IdleDecision::Compact
}

/// A scheduled check of one thread.
struct Pending {
    work: Arc<Work>,
    generation: u64,
    due: SystemTime,
    /// Fired while held: released, it is checked at once.
    fired_while_held: bool,
    task: Option<tokio::task::AbortHandle>,
}

impl Drop for Pending {
    fn drop(&mut self) {
        if let Some(task) = self.task.take() {
            task.abort();
        }
    }
}

struct Work {
    basis: IdleBasis,
    session: IdleSession,
}

#[derive(Default)]
struct Thread {
    /// Bumped by every turn start.
    generation: u64,
    held: bool,
    pending: Option<Pending>,
}

/// The app's idle compactions, one check at most per thread.
pub struct IdleCompactor {
    threads: Mutex<HashMap<ThreadKey, Thread>>,
    emitter: Arc<dyn EventEmitter>,
    /// Whether a turn (or its commit) holds the workspace now.
    busy: Arc<dyn Fn(&str) -> bool + Send + Sync>,
}

impl IdleCompactor {
    #[must_use]
    pub fn new(
        emitter: Arc<dyn EventEmitter>,
        busy: Arc<dyn Fn(&str) -> bool + Send + Sync>,
    ) -> Self {
        Self {
            threads: Mutex::new(HashMap::new()),
            emitter,
            busy,
        }
    }

    fn threads(&self) -> std::sync::MutexGuard<'_, HashMap<ThreadKey, Thread>> {
        self.threads
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// A turn starts on `key`: its check (and a summary under way) is
    /// cancelled, and a check scheduled before it never compacts.
    pub fn turn_started(&self, key: &ThreadKey) {
        let mut threads = self.threads();
        let thread = threads.entry(key.clone()).or_default();
        thread.generation = thread.generation.wrapping_add(1);
        thread.pending = None;
    }

    /// `key`'s turn committed: schedule its check under `settings`.
    pub fn turn_ended(
        self: &Arc<Self>,
        key: ThreadKey,
        basis: IdleBasis,
        session: IdleSession,
        settings: &CompactionSettings,
    ) {
        let Some(delay) = settings.idle_delay() else {
            return;
        };
        let Some(last) = basis.capture.last() else {
            return;
        };
        let work = Arc::new(Work { basis, session });
        let mut threads = self.threads();
        let thread = threads.entry(key.clone()).or_default();
        let generation = thread.generation;
        // What is known now already rules most threads out.
        let state = state_of(&work, &last, generation, generation, false, thread.held);
        if let IdleDecision::Skip(why) = decide(&state, last.started) {
            log::debug!("no idle compaction scheduled: {why}");
            thread.pending = None;
            return;
        }
        let due = last.started + delay;
        let wait = due
            .duration_since(SystemTime::now())
            .unwrap_or(Duration::ZERO);
        let compactor = self.clone();
        let task_key = key.clone();
        let task = tokio::spawn(async move {
            tokio::time::sleep(wait).await;
            compactor.fire(&task_key, generation).await;
        })
        .abort_handle();
        thread.pending = Some(Pending {
            work,
            generation,
            due,
            fired_while_held: false,
            task: Some(task),
        });
        log::info!("idle compaction check scheduled in {} s", wait.as_secs());
    }

    /// The composer of `key` holds unsent text (`hold`), or no longer does:
    /// a check that fired meanwhile runs now (when still in its window).
    pub fn hold(self: &Arc<Self>, key: &ThreadKey, hold: bool) {
        let mut threads = self.threads();
        if !hold && !threads.contains_key(key) {
            return;
        }
        let thread = threads.entry(key.clone()).or_default();
        thread.held = hold;
        if hold {
            return;
        }
        let Some(pending) = thread.pending.as_mut() else {
            threads.remove(key);
            return;
        };
        if !pending.fired_while_held || SystemTime::now() < pending.due {
            return;
        }
        pending.fired_while_held = false;
        let (compactor, task_key, generation) = (self.clone(), key.clone(), pending.generation);
        pending.task = Some(
            tokio::spawn(async move { compactor.fire(&task_key, generation).await }).abort_handle(),
        );
    }

    /// Cancel every check (a settings change, a sign-out, the app's exit).
    pub fn cancel_all(&self) {
        for thread in self.threads().values_mut() {
            thread.pending = None;
        }
    }

    /// Forget `key` (the thread was deleted).
    pub fn forget(&self, key: &ThreadKey) {
        self.threads().remove(key);
    }

    /// Forget every thread of `workspace_id` (the workspace was removed).
    pub fn forget_workspace(&self, workspace_id: &str) {
        self.threads()
            .retain(|key, _| key.workspace_id != workspace_id);
    }

    /// Fire `key`'s scheduled check now, whatever its time (tests); `false`
    /// when none is scheduled.
    #[cfg(test)]
    pub(super) async fn fire_pending(self: &Arc<Self>, key: &ThreadKey) -> bool {
        let generation = self
            .threads()
            .get(key)
            .and_then(|thread| thread.pending.as_ref())
            .map(|pending| pending.generation);
        let Some(generation) = generation else {
            return false;
        };
        self.fire(key, generation).await;
        true
    }

    /// The scheduled check of `key` (for `generation`) fires.
    async fn fire(self: &Arc<Self>, key: &ThreadKey, generation: u64) {
        let work = {
            let mut threads = self.threads();
            let Some(thread) = threads.get_mut(key) else {
                return;
            };
            let Some(pending) = thread.pending.as_mut() else {
                return;
            };
            if pending.generation != generation {
                return;
            }
            let Some(last) = pending.work.basis.capture.last() else {
                thread.pending = None;
                return;
            };
            let state = state_of(
                &pending.work,
                &last,
                generation,
                thread.generation,
                (self.busy)(&key.workspace_id),
                thread.held,
            );
            match decide(&state, SystemTime::now()) {
                IdleDecision::Compact => pending.work.clone(),
                IdleDecision::Held => {
                    pending.fired_while_held = true;
                    return;
                }
                IdleDecision::Skip(why) => {
                    log::info!("idle compaction skipped: {why}");
                    // Dropping the pending check would abort this very task:
                    // take its handle out first.
                    pending.task = None;
                    thread.pending = None;
                    return;
                }
            }
        };
        let still_current = || {
            !(self.busy)(&key.workspace_id)
                && self
                    .threads()
                    .get(key)
                    .is_some_and(|thread| thread.generation == generation)
        };
        match compact(&work, &still_current).await {
            Ok(done) => {
                log::info!(
                    "an idle thread was compacted ({} → {} estimated tokens)",
                    done.before_tokens,
                    done.after_tokens
                );
                self.emitter.emit_app(EVENT_NAME, done.payload(key));
            }
            Err(why) => log::info!("idle compaction did not run to the end: {why}"),
        }
        let mut threads = self.threads();
        if let Some(thread) = threads.get_mut(key)
            && thread
                .pending
                .as_ref()
                .is_some_and(|pending| pending.generation == generation)
            && let Some(mut pending) = thread.pending.take()
        {
            pending.task = None;
        }
    }
}

fn state_of(
    work: &Work,
    last: &CapturedRequest,
    scheduled_generation: u64,
    generation: u64,
    turn_running: bool,
    held: bool,
) -> IdleState {
    IdleState {
        scheduled_generation,
        generation,
        turn_running,
        held,
        message_tokens: work
            .basis
            .parts
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .map(|parts| parts.message_tokens),
        prompt_cache: work.basis.prompt_cache,
        answered: last.answer.as_ref().is_some_and(|answer| {
            !answer
                .parts
                .iter()
                .any(|part| matches!(part, Part::FunctionCall { .. }))
        }),
        request_started: last.started,
        cache_ttl: last.cache_ttl,
    }
}

/// An idle compaction that landed.
#[derive(Debug)]
struct Compacted {
    before_tokens: u64,
    after_tokens: u64,
    window_tokens: u32,
    window_fallback: bool,
    compact_at_tokens: u64,
}

impl Compacted {
    fn payload(&self, key: &ThreadKey) -> Value {
        let context = ContextMeter {
            used_tokens: self.after_tokens,
            window_tokens: self.window_tokens,
            phase: "compacted",
            window_fallback: self.window_fallback,
            compacted_at: Some(
                chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Millis, true),
            ),
            compact_at_tokens: self.compact_at_tokens,
            parts: None,
            measured_at: None,
        };
        json!({
            "workspace_id": key.workspace_id,
            "conversation_id": key.conversation,
            "reason": "idle",
            "before_tokens": self.before_tokens,
            "after_tokens": self.after_tokens,
            "context": context.payload(),
        })
    }
}

/// One summary request's answer: its text, and whether it called a tool.
type Answer = adk_core::Result<(String, bool)>;

/// The v1 record of the conversation from `ask` (the cached continuation:
/// `appended` after the last request), as a step compaction would accept
/// it: validated against `source`, asked once more when it ignores the
/// template, else kept as plain text. An answer that calls a tool fails.
async fn continuation_summary<F, Fut>(
    ask: F,
    answer: &Content,
    guidance: &str,
    source: &Value,
    objective: Option<&str>,
    fallback_objective: &str,
    files: Option<&str>,
) -> Result<String, String>
where
    F: Fn(Vec<Appended>) -> Fut,
    Fut: std::future::Future<Output = Answer>,
{
    let prompt = context_summary::continuation_prompt(guidance);
    let mut appended = vec![Appended::Answer(answer.clone()), Appended::User(prompt)];
    let refused = |error: adk_core::AdkError| format!("the summary call failed ({})", error.code);
    let (first, calls) = ask(appended.clone()).await.map_err(refused)?;
    if calls {
        return Err("the summary answer called a tool".to_owned());
    }
    let (accepted, last) = match context_summary::validate(&first, source) {
        Ok(accepted) => (Some(accepted), first),
        Err(error) => {
            log::info!(
                "the idle summary ignored the template ({}): asking once more",
                error.code
            );
            appended.push(Appended::Assistant(first));
            appended.push(Appended::User(TEMPLATE_RETRY.to_owned()));
            let (second, calls) = ask(appended).await.map_err(refused)?;
            if calls {
                return Err("the summary answer called a tool".to_owned());
            }
            (context_summary::validate(&second, source).ok(), second)
        }
    };
    if let Some(accepted) = accepted {
        return Ok(
            context_summary::carry_forward(&accepted, objective, files, source).unwrap_or(accepted),
        );
    }
    Ok(context_summary::plain_text(
        objective.unwrap_or(fallback_objective),
        &last,
        files,
    ))
}

/// Run one idle compaction (module docs); `still_current` is asked right
/// before the marker is written.
async fn compact(
    work: &Work,
    still_current: &(dyn Fn() -> bool + Sync),
) -> Result<Compacted, String> {
    let basis = &work.basis;
    let last = basis
        .capture
        .last()
        .ok_or("the thread's last request is gone")?;
    let answer = last
        .answer
        .clone()
        .ok_or("the last request was not answered")?;
    // The thread's history as its next request would carry it: the session
    // after the summary the last request started from, if any.
    let based_on = last
        .contents
        .first()
        .filter(|first| summary_of(first).is_some());
    let session = work
        .session
        .service
        .get(GetRequest {
            app_name: crate::sessions::APP_NAME.to_owned(),
            user_id: crate::sessions::ADK_USER.to_owned(),
            session_id: work.session.key.clone(),
            num_recent_events: None,
            after: None,
        })
        .await
        .map_err(|_| "the session could not be read")?;
    let events = session.events().all();
    let visible = visible_events(&events, AGENT_AUTHOR, based_on);
    if visible.is_empty() {
        return Err("the session no longer holds the last request's history".to_owned());
    }
    let history: Vec<Content> = based_on
        .cloned()
        .into_iter()
        .chain(visible.into_iter().map(|(_, content)| content))
        .collect();
    let base = usize::from(based_on.is_some());
    let cut = fold_point(&history, &basis.policy, &basis.budget)
        .filter(|cut| *cut > base)
        .ok_or("nothing complete to fold")?;
    let previous = based_on.and_then(summary_of);
    let files = files_entry(previous.as_deref(), files_called(&history[base..cut]));
    let objective = previous.as_deref().and_then(context_summary::objective);
    let fallback_objective = history[base..cut]
        .iter()
        .find(|content| content.role == "user")
        .map(|content| {
            content
                .parts
                .iter()
                .filter_map(|part| match part {
                    Part::Text { text } => Some(text.as_str()),
                    _ => None,
                })
                .collect::<Vec<_>>()
                .join("\n")
        })
        .unwrap_or_default();
    // References must occur in what the model saw: the request as sent.
    let source = json!({"messages": last.body["messages"], "answer": answer});
    let model = GatewayTransport {
        events: Arc::new(TurnEvents::new(
            format!("idle-{}", uuid::Uuid::new_v4()),
            Arc::new(Silent),
        )),
        ..basis.transport.clone()
    }
    .gateway_model(basis.model.clone())
    .map_err(|error| error.to_string())?;
    let max_tokens = SUMMARY_MAX_OUTPUT_TOKENS.min(basis.max_output_tokens);
    let ask = |appended: Vec<Appended>| {
        let body = super::model::continuation_body(&last, &appended, max_tokens);
        let model = &model;
        async move {
            let body = body.ok_or_else(|| {
                adk_core::AdkError::model(
                    "idle_summary_layout: the last answer cannot be continued",
                )
            })?;
            let (text, calls, usage) = model.send_body(&body).await?;
            if let Some(usage) = usage {
                log::info!(
                    "idle summary call: {} prompt tokens, {} read from the cache, {} written to it",
                    usage.prompt,
                    usage.cached.unwrap_or(0),
                    usage.cache_write.unwrap_or(0)
                );
            }
            Ok((text, calls))
        }
    };
    let summary = continuation_summary(
        ask,
        &answer,
        &basis.policy.guidance,
        &source,
        objective.as_deref(),
        &fallback_objective,
        files.as_deref(),
    )
    .await?;
    let compaction = folded_compaction(&history, cut, summary);
    let marker = marker_for(&events, &compaction).map_err(str::to_owned)?;
    if !still_current() {
        return Err("a turn started while the summary ran".to_owned());
    }
    if !work
        .session
        .store
        .append_marker(&work.session.key, marker)
        .await
        .map_err(|error| error.to_string())?
    {
        return Err("the session is held by a turn, or its last turn is not settled".to_owned());
    }
    let parts = *basis
        .parts
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    let before_tokens = parts.map_or(0, |parts| {
        parts.system_tokens
            + parts.tool_tokens
            + parts.mcp_tool_tokens
            + parts.skill_tokens
            + parts.message_tokens
    });
    let after_tokens = before_tokens
        .saturating_sub(contents_tokens(&history[..cut]))
        .saturating_add(contents_tokens(std::slice::from_ref(
            &compaction.summary_content,
        )));
    Ok(Compacted {
        before_tokens,
        after_tokens,
        window_tokens: basis.budget.total_tokens,
        window_fallback: basis.window_fallback,
        compact_at_tokens: if basis.policy.automatic {
            basis.policy.trigger_tokens(&basis.budget)
        } else {
            0
        },
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn state(at: SystemTime) -> IdleState {
        IdleState {
            scheduled_generation: 3,
            generation: 3,
            turn_running: false,
            held: false,
            message_tokens: Some(80_000),
            prompt_cache: true,
            answered: true,
            request_started: at,
            cache_ttl: CacheTtl::FiveMinutes,
        }
    }

    #[test]
    fn an_idle_thread_is_compacted_only_while_its_cache_is_warm() {
        let started = SystemTime::UNIX_EPOCH + Duration::from_secs(1_000_000);
        let at = |seconds: u64| started + Duration::from_secs(seconds);
        let ready = state(started);
        // Four minutes after the last request started: compact.
        assert_eq!(decide(&ready, at(240)), IdleDecision::Compact);
        // Past the five-minute cache (a Mac that slept): skipped.
        assert!(matches!(decide(&ready, at(300)), IdleDecision::Skip(_)));
        assert!(matches!(decide(&ready, at(3_600)), IdleDecision::Skip(_)));
        // The one-hour cache lasts until the hour.
        let hour = IdleState {
            cache_ttl: CacheTtl::OneHour,
            ..ready
        };
        assert_eq!(decide(&hour, at(55 * 60)), IdleDecision::Compact);
        assert!(matches!(decide(&hour, at(60 * 60)), IdleDecision::Skip(_)));
        // A clock set back before the request: the cache is unknown.
        assert!(matches!(
            decide(&ready, started - Duration::from_secs(1)),
            IdleDecision::Skip(_)
        ));
    }

    #[test]
    fn every_condition_must_hold() {
        let started = SystemTime::UNIX_EPOCH + Duration::from_secs(1_000_000);
        let now = started + Duration::from_secs(240);
        let skip = |state: IdleState| matches!(decide(&state, now), IdleDecision::Skip(_));
        let ready = state(started);
        assert!(skip(IdleState {
            generation: 4,
            ..ready
        }));
        assert!(skip(IdleState {
            turn_running: true,
            ..ready
        }));
        assert!(skip(IdleState {
            prompt_cache: false,
            ..ready
        }));
        assert!(skip(IdleState {
            message_tokens: Some(MIN_MESSAGE_TOKENS - 1),
            ..ready
        }));
        assert!(skip(IdleState {
            message_tokens: None,
            ..ready
        }));
        assert!(skip(IdleState {
            answered: false,
            ..ready
        }));
        assert_eq!(
            decide(
                &IdleState {
                    message_tokens: Some(MIN_MESSAGE_TOKENS),
                    ..ready
                },
                now
            ),
            IdleDecision::Compact
        );
        // Unsent text holds it (checked again on release), unless it would
        // be skipped anyway.
        let held = IdleState {
            held: true,
            ..ready
        };
        assert_eq!(decide(&held, now), IdleDecision::Held);
        assert!(skip(IdleState {
            prompt_cache: false,
            ..held
        }));
    }

    fn summary(objective: &str) -> String {
        json!({"version":1,"objective":objective,"constraints":[],"decisions":[],
            "key_facts":["The build is red."],"completed_work":[],"open_work":["Fix it."],
            "next_steps":[],"unresolved_issues":[],"references":[]})
        .to_string()
    }

    /// What each scripted request appended.
    type Asked = Arc<Mutex<Vec<Vec<Appended>>>>;

    /// Answers from a script, recording what each request appended.
    fn scripted(
        answers: Vec<(String, bool)>,
    ) -> (Asked, impl Fn(Vec<Appended>) -> std::future::Ready<Answer>) {
        let asked = Arc::new(Mutex::new(Vec::new()));
        let answers = Arc::new(Mutex::new(answers.into_iter().rev().collect::<Vec<_>>()));
        let seen = asked.clone();
        (asked, move |appended: Vec<Appended>| {
            seen.lock().unwrap().push(appended);
            std::future::ready(Ok(answers.lock().unwrap().pop().unwrap()))
        })
    }

    #[tokio::test]
    async fn the_continuation_asks_once_and_keeps_the_record() {
        let answer = Content::new("model").with_text("Done: the tests pass.");
        let source = json!({"messages": []});
        let (asked, ask) = scripted(vec![(summary("Something else"), false)]);
        let kept = continuation_summary(
            ask,
            &answer,
            "Keep ids.",
            &source,
            Some("Fix the build."),
            "first message",
            Some(r#"[{"path":"a.rs","op":"edit"}]"#),
        )
        .await
        .unwrap();
        // The previous objective and the files entry are carried forward.
        assert_eq!(
            context_summary::objective(&kept).as_deref(),
            Some("Fix the build.")
        );
        assert!(kept.contains("files_touched: "), "{kept}");
        let asked = asked.lock().unwrap();
        assert_eq!(asked.len(), 1);
        // The answer, then the one continuation prompt.
        assert!(matches!(&asked[0][0], Appended::Answer(content) if content.role == "model"));
        assert!(
            matches!(&asked[0][1], Appended::User(prompt) if prompt.contains("the conversation above") && prompt.contains("Keep ids."))
        );
        assert_eq!(asked[0].len(), 2);
    }

    #[tokio::test]
    async fn a_template_miss_is_asked_again_then_kept_as_text_and_a_tool_call_fails() {
        let answer = Content::new("model").with_text("ok");
        let source = json!({"messages": []});
        let (asked, ask) = scripted(vec![
            ("not json".to_owned(), false),
            ("still not json".to_owned(), false),
        ]);
        let kept = continuation_summary(ask, &answer, "", &source, None, "Fix it.", None)
            .await
            .unwrap();
        assert_eq!(
            context_summary::objective(&kept).as_deref(),
            Some("Fix it.")
        );
        {
            let asked = asked.lock().unwrap();
            assert_eq!(asked.len(), 2);
            assert!(matches!(&asked[1][2], Appended::Assistant(text) if text == "not json"));
            assert!(matches!(&asked[1][3], Appended::User(text) if text == TEMPLATE_RETRY));
        }
        let (_, ask) = scripted(vec![(String::new(), true)]);
        assert!(
            continuation_summary(ask, &answer, "", &source, None, "x", None)
                .await
                .is_err()
        );
    }
}
