//! Durable dispatch records for approved sensitive-tool calls of NESTED agents.
//!
//! A root Agent's approved call is replayed by the root Runner, which persists
//! the replayed call event through the fenced session append before ADK runs the
//! tool; that event is the root's dispatch marker. A nested Agent's events reach
//! the root session only through the parent's asynchronous forwarding, so they
//! prove nothing about a dispatch. This journal closes that gap: before an
//! approved call of a nested continuation runs, a `dispatched` record is
//! appended to the ROOT session through the same fenced writer, and after it
//! returns a `completed` record carries its result.
//!
//! The next continuation of the same decision reads them:
//! * no record: the call never started, so it runs (once);
//! * `completed`: the stored result is served, the tool is not called again;
//! * `dispatched` only: the outcome is unknown; a call that changes data is
//!   refused as "may have started", never repeated.
//!
//! A root Agent journals only the `dispatched` record: its outcome is ADK's own
//! function response, which the root Runner persists. The replayed call event
//! alone proves no dispatch there either: ADK's confirmation pre-check persists
//! the replayed message and then pauses on an undecided sensitive sibling
//! without running any tool.
//!
//! Every record names its owner, the execution (and generation) whose
//! continuation wrote it, and the interrupt it answers. Main re-claims a lost
//! execution under the same identity with a higher claim attempt, so only that
//! execution's replacement may continue behind a dead continuation's records;
//! a duplicate or late submission (another execution) is stale.
//!
//! Records carry no content and no state delta, so ADK history, the replay
//! proofs and the browser projection ignore them.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;

use adk_rust::session::{AppendEventRequest, SessionService};
use adk_rust::{AdkIdentity, Event, ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use serde_json::Value;

/// Test-only (process-replacement proofs, set in a child process only): the
/// `dispatched` append fails as a database error would.
#[cfg(test)]
pub(crate) static FAIL_DISPATCHED_APPEND: std::sync::atomic::AtomicBool =
    std::sync::atomic::AtomicBool::new(false);

/// Test-only: exit the process with this code just before the `dispatched`
/// append, once the replayed call this journal owns has been forwarded to the
/// root session (the window between forwarding and the dispatch record).
#[cfg(test)]
pub(crate) static EXIT_BEFORE_DISPATCHED_RECORD: std::sync::atomic::AtomicI32 =
    std::sync::atomic::AtomicI32::new(0);

/// Provider-metadata key of one dispatch record.
pub(crate) const DISPATCH_METADATA_KEY: &str = "elitea.direct_hitl.dispatch.v1";
const DISPATCH_AUTHOR: &str = "elitea-dispatch-journal";
const RECORD_VERSION: u8 = 1;
/// Largest encoded record. A larger result is returned to the model but not
/// stored; a crash before the parent continues then ends as "may have started".
pub(crate) const MAX_DISPATCH_RECORD_BYTES: usize = 256 * 1024;
/// Records one decision may own (one per call of the paused message).
pub(crate) const MAX_DISPATCH_RECORDS: usize = 64;

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
enum Phase {
    Dispatched,
    Completed,
    /// A pipeline continuation of one card began (written before its Runner
    /// starts): the owner proof of everything that continuation forwards.
    Continuing,
}

/// The execution whose continuation wrote a record: stable across Main's
/// re-claims of a lost execution, different for any other submission.
#[derive(Clone, Debug, Deserialize, Eq, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct DispatchOwner {
    execution_id: String,
    generation: u64,
}

impl DispatchOwner {
    pub(crate) fn new(execution_id: impl Into<String>, generation: u64) -> Self {
        Self {
            execution_id: execution_id.into(),
            generation,
        }
    }
}

#[derive(Deserialize, Serialize)]
#[cfg_attr(test, derive(Clone))]
#[serde(deny_unknown_fields)]
struct DispatchRecord {
    version: u8,
    phase: Phase,
    /// The invocation of the paused assistant message the decision answered.
    decision_invocation_id: String,
    /// The card (interrupt) the decision answered.
    interrupt_id: String,
    owner: DispatchOwner,
    call_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    result: Option<Value>,
}

/// What earlier continuations recorded for the calls of one paused message.
#[derive(Default)]
pub(crate) struct RecordedDispatches {
    /// Calls dispatched without a recorded outcome.
    pub(crate) dispatched: BTreeSet<String>,
    /// Calls whose outcome was recorded, with their result.
    pub(crate) completed: BTreeMap<String, Value>,
}

/// Malformed or excessive records fail closed: the decision is not resumed.
#[derive(Debug)]
pub(crate) struct InvalidDispatchRecord;

/// The record one event carries, if any. A forged or malformed record fails
/// closed.
fn dispatch_record(event: &Event) -> Result<Option<DispatchRecord>, InvalidDispatchRecord> {
    let Some(raw) = event.provider_metadata.get(DISPATCH_METADATA_KEY) else {
        return Ok(None);
    };
    if event.author != DISPATCH_AUTHOR
        || raw.len() > MAX_DISPATCH_RECORD_BYTES
        || event.llm_response.content.is_some()
        || !event.actions.state_delta.is_empty()
    {
        return Err(InvalidDispatchRecord);
    }
    let record: DispatchRecord = serde_json::from_str(raw).map_err(|_| InvalidDispatchRecord)?;
    if record.version != RECORD_VERSION
        || record.call_id.is_empty()
        || record.interrupt_id.is_empty()
        || record.owner.execution_id.is_empty()
    {
        return Err(InvalidDispatchRecord);
    }
    Ok(Some(record))
}

/// Whether `event` is a dispatch record (well-formed or not).
pub(crate) fn is_dispatch_record(event: &Event) -> bool {
    event.provider_metadata.contains_key(DISPATCH_METADATA_KEY)
}

/// Whether every dispatch record in `events` was written by `owner` for one of
/// the interrupts `answers` accepts. `Ok(false)` when there is no record.
///
/// # Errors
///
/// A malformed record, or a record of another execution or another interrupt:
/// the suffix is not a dead continuation of this decision.
pub(crate) fn records_bound_to(
    events: &[Event],
    owner: &DispatchOwner,
    answers: impl Fn(&str) -> bool,
) -> Result<bool, InvalidDispatchRecord> {
    let mut any = false;
    for event in events {
        let Some(record) = dispatch_record(event)? else {
            continue;
        };
        if &record.owner != owner || !answers(&record.interrupt_id) {
            return Err(InvalidDispatchRecord);
        }
        any = true;
    }
    Ok(any)
}

/// Whether every record of one decision (its paused message's invocation and
/// card) was written by `owner`. `Ok(false)`: another execution already
/// continued this card, so this submission is a duplicate or late one. Only
/// that decision's records are parsed (a record event carries the decision's
/// invocation id).
///
/// # Errors
///
/// A malformed or forged record of this decision.
pub(crate) fn decision_records_owned_by(
    events: &[Event],
    owner: &DispatchOwner,
    decision_invocation_id: &str,
    interrupt_id: &str,
) -> Result<bool, InvalidDispatchRecord> {
    for event in events
        .iter()
        .filter(|event| event.invocation_id == decision_invocation_id)
    {
        let Some(record) = dispatch_record(event)? else {
            continue;
        };
        if record.decision_invocation_id != decision_invocation_id {
            return Err(InvalidDispatchRecord);
        }
        if record.interrupt_id == interrupt_id && &record.owner != owner {
            return Ok(false);
        }
    }
    Ok(true)
}

/// Read the records of one paused message from the root session's events.
pub(crate) fn recorded_dispatches(
    events: &[Event],
    decision_invocation_id: &str,
) -> Result<RecordedDispatches, InvalidDispatchRecord> {
    let mut recorded = RecordedDispatches::default();
    let mut count = 0_usize;
    for event in events {
        let Some(record) = dispatch_record(event)? else {
            continue;
        };
        if record.decision_invocation_id != decision_invocation_id {
            continue;
        }
        count += 1;
        if count > MAX_DISPATCH_RECORDS * 2 {
            return Err(InvalidDispatchRecord);
        }
        match (record.phase, record.result) {
            (Phase::Continuing, None) => {}
            (Phase::Dispatched, None) => {
                if !recorded.completed.contains_key(&record.call_id) {
                    recorded.dispatched.insert(record.call_id);
                }
            }
            (Phase::Completed, Some(result)) => {
                recorded.dispatched.remove(&record.call_id);
                recorded.completed.insert(record.call_id, result);
            }
            _ => return Err(InvalidDispatchRecord),
        }
    }
    Ok(recorded)
}

/// The pause a pipeline continuation answers.
///
/// Normally the pause is the session's last event. A continuation that died
/// left its own suffix behind it. Every pipeline application continuation
/// first appends a `continuing` record for its cards (before its Runner
/// starts), then its marker, the replayed calls the child forwards and the
/// dispatch records of approved calls. The pause stays answerable behind such
/// a suffix only when:
/// * its FIRST event is a `continuing` record of THIS execution (an earlier
///   claim of it: Main re-claims a lost execution under the same identity)
///   for one of the pause's own cards (`interrupt_ids` of the pause);
/// * every record in it belongs to this execution and those cards; and
/// * every other event is that continuation's own bookkeeping
///   (`is_continuation_event`): anything else is an advance past the pause.
///
/// The decision is then resolved against the journal, so a recorded outcome is
/// served, an unrecorded dispatch is refused, and a call that never dispatched
/// runs once. Anything else is stale (`None`): an ordinary advance, or a
/// continuation of another execution (a duplicate or late submission).
pub(crate) fn continuation_pause<'a>(
    events: &'a [Event],
    is_pause: impl Fn(&Event) -> bool,
    owner: &DispatchOwner,
    interrupt_ids: impl FnOnce(&Event) -> Option<BTreeSet<String>>,
    mut is_continuation_event: impl FnMut(&Event) -> bool,
) -> Option<&'a Event> {
    let last = events.last()?;
    if is_pause(last) {
        return Some(last);
    }
    let index = events.iter().rposition(&is_pause)?;
    let pause = &events[index];
    let answers = interrupt_ids(pause)?;
    let suffix = &events[index + 1..];
    let first = dispatch_record(suffix.first()?).ok()??;
    if first.phase != Phase::Continuing
        || &first.owner != owner
        || !answers.contains(&first.interrupt_id)
        || first.call_id != pause.id
    {
        return None;
    }
    records_bound_to(suffix, owner, |id| answers.contains(id)).ok()?;
    suffix
        .iter()
        .all(|event| is_dispatch_record(event) || is_continuation_event(event))
        .then_some(pause)
}

/// The code a run ends with when an approved call's `dispatched` record could
/// not be written: that call never ran, and no later journaled call of the
/// invocation starts. The run fails (not retryable): another submission of the
/// card is stale behind this execution's continuation.
pub(crate) const DISPATCH_UNRECORDED_CODE: &str = "agent.dispatch_unrecorded";
/// The internal (never user-facing) detail of [`DISPATCH_UNRECORDED_CODE`].
pub(crate) const DISPATCH_UNRECORDED_DETAIL: &str =
    "an approved tool call's dispatch could not be recorded; it was not run";

/// One invocation's stop after a failed `dispatched` append.
///
/// ADK turns every tool error into a function response the model reads, so the
/// approved call's tool error alone would let the run continue and even
/// SUCCEED without its effect. The journal records the failure here, and the
/// invocation stops on it at three points: no later journaled call starts, every
/// guarded model step refuses, and the root agent's stream ends with the error
/// even when no further model step happens (a child at its step limit).
#[derive(Clone, Default)]
pub(crate) struct DispatchAbort(Arc<AbortState>);

#[derive(Default)]
struct AbortState {
    failed: std::sync::OnceLock<()>,
    /// Orders the stop check and the `dispatched` append of every approved call
    /// of the invocation (concurrent siblings and parallel children alike):
    /// once one append failed, no other approved effect starts.
    dispatch: tokio::sync::Mutex<()>,
}

impl DispatchAbort {
    fn record(&self) {
        let _ = self.0.failed.set(());
    }

    /// Order one approved call's stop check and `dispatched` append against
    /// every other approved call of the invocation.
    async fn lock_dispatch(&self) -> tokio::sync::MutexGuard<'_, ()> {
        self.0.dispatch.lock().await
    }

    /// The stop, once a dispatch record failed.
    pub(crate) fn error(&self) -> Option<adk_rust::AdkError> {
        self.0.failed.get().map(|()| {
            adk_rust::AdkError::new(
                adk_rust::ErrorComponent::Session,
                // Not retryable: the run stops.
                adk_rust::ErrorCategory::Internal,
                DISPATCH_UNRECORDED_CODE,
                DISPATCH_UNRECORDED_DETAIL,
            )
        })
    }

    /// Wrap the root agent so its run ends with the stop even when no guarded
    /// model step follows the failed record.
    pub(crate) fn guard_agent(&self, inner: Arc<dyn adk_rust::Agent>) -> Arc<dyn adk_rust::Agent> {
        Arc::new(AbortGuardedAgent {
            inner,
            abort: self.clone(),
        })
    }

    /// Wrap one model of the invocation so it refuses after a failed dispatch append.
    pub(crate) fn guard(&self, inner: Arc<dyn adk_rust::Llm>) -> Arc<dyn adk_rust::Llm> {
        Arc::new(AbortGuardedModel {
            inner,
            abort: self.clone(),
        })
    }
}

struct AbortGuardedAgent {
    inner: Arc<dyn adk_rust::Agent>,
    abort: DispatchAbort,
}

#[async_trait]
impl adk_rust::Agent for AbortGuardedAgent {
    fn name(&self) -> &str {
        self.inner.name()
    }

    fn description(&self) -> &str {
        self.inner.description()
    }

    fn sub_agents(&self) -> &[Arc<dyn adk_rust::Agent>] {
        self.inner.sub_agents()
    }

    async fn run(
        &self,
        ctx: Arc<dyn adk_rust::InvocationContext>,
    ) -> adk_rust::Result<adk_rust::EventStream> {
        Ok(self.abort.stop_at_end(self.inner.run(ctx).await?))
    }
}

impl DispatchAbort {
    /// The events, then the stop if a dispatch record failed meanwhile.
    fn stop_at_end(&self, events: adk_rust::EventStream) -> adk_rust::EventStream {
        use adk_rust::futures::StreamExt as _;
        let abort = self.clone();
        let end = adk_rust::futures::stream::once(async move { abort.error() })
            .filter_map(|error| async move { error.map(Err) });
        Box::pin(events.chain(end))
    }
}

struct AbortGuardedModel {
    inner: Arc<dyn adk_rust::Llm>,
    abort: DispatchAbort,
}

#[async_trait]
impl adk_rust::Llm for AbortGuardedModel {
    fn name(&self) -> &str {
        self.inner.name()
    }

    fn schema_adapter(&self) -> &dyn adk_rust::SchemaAdapter {
        self.inner.schema_adapter()
    }

    fn uses_interactions_api(&self) -> bool {
        self.inner.uses_interactions_api()
    }

    async fn generate_content(
        &self,
        request: adk_rust::LlmRequest,
        stream_response: bool,
    ) -> adk_rust::Result<adk_rust::LlmResponseStream> {
        if let Some(error) = self.abort.error() {
            return Err(error);
        }
        self.inner.generate_content(request, stream_response).await
    }
}

/// Which records a journaled call writes.
#[derive(Clone, Copy, Eq, PartialEq)]
pub(crate) enum JournalOutcomes {
    /// `dispatched` and `completed`: a nested Agent, whose function responses
    /// reach the root session only asynchronously.
    Recorded,
    /// `dispatched` only: the root Agent, whose Runner persists the function
    /// response itself.
    ByRunner,
}

/// What one replay's journaled calls answer.
pub(crate) struct JournalDecision<'a> {
    /// The invocation of the paused assistant message.
    pub(crate) invocation_id: &'a str,
    /// The card the decision answered.
    pub(crate) interrupt_id: &'a str,
}

/// The fenced root-session writer for dispatch records.
#[derive(Clone)]
pub(crate) struct DispatchJournal {
    sessions: Arc<dyn SessionService>,
    identity: AdkIdentity,
    owner: DispatchOwner,
    abort: DispatchAbort,
}

impl DispatchJournal {
    pub(crate) fn new(
        sessions: Arc<dyn SessionService>,
        identity: AdkIdentity,
        owner: DispatchOwner,
        abort: DispatchAbort,
    ) -> Self {
        Self {
            sessions,
            identity,
            owner,
            abort,
        }
    }

    /// The invocation-wide stop this journal records a failed dispatch in.
    pub(crate) fn abort(&self) -> &DispatchAbort {
        &self.abort
    }

    /// Record, with one append before its Runner starts, that this execution's
    /// continuation of `pause` begins for `interrupt_id`, one of the cards it
    /// answers (see [`continuation_pause`]).
    ///
    /// # Errors
    ///
    /// The fenced root writer refused the append.
    pub(crate) async fn record_continuing(
        &self,
        pause_id: &str,
        pause_invocation_id: &str,
        interrupt_id: &str,
    ) -> adk_rust::Result<()> {
        self.append(&DispatchRecord {
            version: RECORD_VERSION,
            phase: Phase::Continuing,
            decision_invocation_id: pause_invocation_id.to_owned(),
            interrupt_id: interrupt_id.to_owned(),
            owner: self.owner.clone(),
            call_id: pause_id.to_owned(),
            result: None,
        })
        .await
    }

    /// The execution this journal writes for.
    pub(crate) fn owner(&self) -> &DispatchOwner {
        &self.owner
    }

    /// Wrap toolsets so each listed call id is recorded around its execution.
    pub(crate) fn wrap(
        &self,
        toolsets: Vec<Arc<dyn Toolset>>,
        decision: &JournalDecision<'_>,
        call_ids: BTreeSet<String>,
        outcomes: JournalOutcomes,
    ) -> Vec<Arc<dyn Toolset>> {
        // Wrapped even when nothing is journaled: no tool of the replay starts
        // after the invocation's stop (`DispatchAbort`).
        let scope = Arc::new(JournalScope {
            journal: self.clone(),
            decision_invocation_id: decision.invocation_id.to_owned(),
            interrupt_id: decision.interrupt_id.to_owned(),
            call_ids,
            outcomes,
        });
        toolsets
            .into_iter()
            .map(|inner| {
                Arc::new(JournalToolset {
                    name: format!("{}-journaled", inner.name()),
                    inner,
                    scope: Arc::clone(&scope),
                }) as Arc<dyn Toolset>
            })
            .collect()
    }

    async fn append(&self, record: &DispatchRecord) -> adk_rust::Result<()> {
        #[cfg(test)]
        if record.phase == Phase::Dispatched {
            self.test_hooks_before_dispatched(&record.call_id).await?;
        }
        let encoded = serde_json::to_string(record).map_err(|_| journal_error())?;
        if encoded.len() > MAX_DISPATCH_RECORD_BYTES {
            return Err(journal_error());
        }
        let mut event = Event::new(&record.decision_invocation_id);
        DISPATCH_AUTHOR.clone_into(&mut event.author);
        event
            .provider_metadata
            .insert(DISPATCH_METADATA_KEY.to_owned(), encoded);
        self.sessions
            .append_event_for_identity(AppendEventRequest {
                identity: self.identity.clone(),
                event,
            })
            .await
    }
}

#[cfg(test)]
impl DispatchJournal {
    async fn test_hooks_before_dispatched(&self, call_id: &str) -> adk_rust::Result<()> {
        use std::sync::atomic::Ordering;
        if let code @ 1.. = EXIT_BEFORE_DISPATCHED_RECORD.load(Ordering::Acquire) {
            for _ in 0..500 {
                let forwarded = self
                    .sessions
                    .get_for_identity(&self.identity)
                    .await
                    .is_ok_and(|session| {
                        session.events().all().iter().any(|event| {
                            event
                                .tool_calls()
                                .iter()
                                .any(|call| call.call_id == Some(call_id))
                                && event.provider_metadata.contains_key(
                                    crate::agents::events::DESCENDANT_CONTAINER_INVOCATION_KEY,
                                )
                        })
                    });
                if forwarded {
                    std::process::exit(code);
                }
                tokio::time::sleep(std::time::Duration::from_millis(10)).await;
            }
            panic!("the replayed call was never forwarded to the root session");
        }
        if FAIL_DISPATCHED_APPEND.load(Ordering::Acquire) {
            return Err(journal_error());
        }
        Ok(())
    }
}

struct JournalScope {
    journal: DispatchJournal,
    decision_invocation_id: String,
    interrupt_id: String,
    call_ids: BTreeSet<String>,
    outcomes: JournalOutcomes,
}

impl JournalScope {
    fn record(&self, phase: Phase, call_id: String, result: Option<Value>) -> DispatchRecord {
        DispatchRecord {
            version: RECORD_VERSION,
            phase,
            decision_invocation_id: self.decision_invocation_id.clone(),
            interrupt_id: self.interrupt_id.clone(),
            owner: self.journal.owner.clone(),
            call_id,
            result,
        }
    }
}

struct JournalToolset {
    name: String,
    inner: Arc<dyn Toolset>,
    scope: Arc<JournalScope>,
}

#[async_trait]
impl Toolset for JournalToolset {
    fn name(&self) -> &str {
        &self.name
    }

    async fn tools(
        &self,
        context: Arc<dyn ReadonlyContext>,
    ) -> adk_rust::Result<Vec<Arc<dyn Tool>>> {
        self.inner.tools(context).await.map(|tools| {
            tools
                .into_iter()
                .map(|inner| {
                    Arc::new(JournalTool {
                        inner,
                        scope: Arc::clone(&self.scope),
                    }) as Arc<dyn Tool>
                })
                .collect()
        })
    }
}

struct JournalTool {
    inner: Arc<dyn Tool>,
    scope: Arc<JournalScope>,
}

#[async_trait]
impl Tool for JournalTool {
    fn name(&self) -> &str {
        self.inner.name()
    }
    fn description(&self) -> &str {
        self.inner.description()
    }
    fn declaration(&self) -> Value {
        self.inner.declaration()
    }
    fn enhanced_description(&self) -> String {
        self.inner.enhanced_description()
    }
    fn is_long_running(&self) -> bool {
        self.inner.is_long_running()
    }
    fn is_builtin(&self) -> bool {
        self.inner.is_builtin()
    }
    fn parameters_schema(&self) -> Option<Value> {
        self.inner.parameters_schema()
    }
    fn response_schema(&self) -> Option<Value> {
        self.inner.response_schema()
    }
    fn required_scopes(&self) -> &[&str] {
        self.inner.required_scopes()
    }
    fn is_read_only(&self) -> bool {
        self.inner.is_read_only()
    }
    fn is_concurrency_safe(&self) -> bool {
        self.inner.is_concurrency_safe()
    }

    async fn execute(
        &self,
        context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        let call_id = context.function_call_id().to_owned();
        if !self.scope.call_ids.contains(&call_id) {
            // Not journaled (it may re-run, or its continuation runs one
            // non-sensitive call), but it still never starts after the stop.
            if let Some(error) = self.scope.journal.abort.error() {
                return Err(error);
            }
            return self.inner.execute(context, arguments).await;
        }
        {
            // One at a time per invocation, so no other approved call (a
            // concurrent sibling or a parallel child's) starts after this call's
            // dispatch record failed.
            let _dispatch = self.scope.journal.abort.lock_dispatch().await;
            // An earlier call's dispatch record failed: the invocation is
            // stopping, and no further approved effect starts.
            if let Some(error) = self.scope.journal.abort.error() {
                return Err(error);
            }
            // The dispatch is durable before the effect, or the effect never
            // starts. A failed append stops the whole invocation
            // (`DispatchAbort`): the tool error alone would be read by the model.
            if let Err(error) = self
                .scope
                .journal
                .append(&self.scope.record(Phase::Dispatched, call_id.clone(), None))
                .await
            {
                self.scope.journal.abort.record();
                return Err(error);
            }
        }
        let outcome = self.inner.execute(context, arguments).await;
        if self.scope.outcomes == JournalOutcomes::ByRunner {
            return outcome;
        }
        // A failed call has an outcome too: record what the model is shown
        // (ADK's `{"error": ...}` function response), so recovery serves it
        // instead of reporting "may have started".
        let recorded = match &outcome {
            Ok(result) => result.clone(),
            Err(error) => serde_json::json!({ "error": error.to_string() }),
        };
        if let Err(error) = self
            .scope
            .journal
            .append(&self.scope.record(Phase::Completed, call_id, Some(recorded)))
            .await
        {
            // The effect happened; the model must still see its result. Only a
            // crash before the parent continues degrades to "may have started".
            tracing::warn!(
                event = "direct_hitl_dispatch_outcome_unrecorded",
                error_code = error.code,
                "approved nested call ran but its outcome was not recorded"
            );
        }
        outcome
    }
}

/// Test-only: the `dispatched` record a continuation of `owner` writes for one call.
#[cfg(test)]
pub(crate) fn test_dispatched_record(
    decision_invocation_id: &str,
    interrupt_id: &str,
    call_id: &str,
    owner: DispatchOwner,
) -> Event {
    let mut event = Event::new(decision_invocation_id);
    DISPATCH_AUTHOR.clone_into(&mut event.author);
    event.provider_metadata.insert(
        DISPATCH_METADATA_KEY.to_owned(),
        serde_json::to_string(&DispatchRecord {
            version: RECORD_VERSION,
            phase: Phase::Dispatched,
            decision_invocation_id: decision_invocation_id.to_owned(),
            interrupt_id: interrupt_id.to_owned(),
            owner,
            call_id: call_id.to_owned(),
            result: None,
        })
        .unwrap_or_default(),
    );
    event
}

fn journal_error() -> adk_rust::AdkError {
    adk_rust::AdkError::new(
        adk_rust::ErrorComponent::Session,
        adk_rust::ErrorCategory::InvalidInput,
        "direct_hitl.dispatch_record_invalid",
        "the approved call's dispatch record could not be written",
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn owner() -> DispatchOwner {
        DispatchOwner::new("execution-1", 1)
    }

    fn owned_record(
        invocation: &str,
        call: &str,
        phase: Phase,
        result: Option<Value>,
        owner: DispatchOwner,
        interrupt: &str,
    ) -> Event {
        let mut event = Event::new(invocation);
        DISPATCH_AUTHOR.clone_into(&mut event.author);
        event.provider_metadata.insert(
            DISPATCH_METADATA_KEY.to_owned(),
            serde_json::to_string(&DispatchRecord {
                version: RECORD_VERSION,
                phase,
                decision_invocation_id: invocation.to_owned(),
                interrupt_id: interrupt.to_owned(),
                owner,
                call_id: call.to_owned(),
                result,
            })
            .unwrap(),
        );
        event
    }

    fn record(invocation: &str, call: &str, phase: Phase, result: Option<Value>) -> Event {
        owned_record(invocation, call, phase, result, owner(), "hitl-1")
    }

    #[test]
    fn records_classify_never_started_dispatched_and_completed_calls() {
        let events = vec![
            record("inv-1", "call-1", Phase::Dispatched, None),
            record("inv-1", "call-1", Phase::Completed, Some(json!({"ok": 1}))),
            record("inv-1", "call-2", Phase::Dispatched, None),
            record("inv-other", "call-3", Phase::Dispatched, None),
        ];
        let recorded = recorded_dispatches(&events, "inv-1").unwrap();
        assert_eq!(recorded.completed.get("call-1"), Some(&json!({"ok": 1})));
        assert!(recorded.dispatched.contains("call-2"));
        assert!(!recorded.dispatched.contains("call-1"));
        assert!(!recorded.dispatched.contains("call-3"));
        assert!(
            recorded_dispatches(&events, "inv-none")
                .unwrap()
                .dispatched
                .is_empty()
        );
    }

    fn continuing(pause: &Event, owner: DispatchOwner, interrupt: &str) -> Event {
        let mut event = Event::new(&pause.invocation_id);
        DISPATCH_AUTHOR.clone_into(&mut event.author);
        event.provider_metadata.insert(
            DISPATCH_METADATA_KEY.to_owned(),
            serde_json::to_string(&DispatchRecord {
                version: RECORD_VERSION,
                phase: Phase::Continuing,
                decision_invocation_id: pause.invocation_id.clone(),
                interrupt_id: interrupt.to_owned(),
                owner,
                call_id: pause.id.clone(),
                result: None,
            })
            .unwrap(),
        );
        event
    }

    /// #1321 review rounds 2–4: a pause stays answerable only behind THIS
    /// execution's own dead continuation (its `continuing` record first, its
    /// bookkeeping only), so a crash before any dispatch record resumes (R) and
    /// an advance, another execution or another card is stale.
    #[test]
    fn a_pause_stays_answerable_only_behind_this_executions_dead_continuation() {
        let mut pause = Event::new("root-1");
        pause
            .provider_metadata
            .insert("pause".to_owned(), "yes".to_owned());
        let is_pause = |event: &Event| event.provider_metadata.contains_key("pause");
        let ids = |_: &Event| Some(BTreeSet::from(["hitl-1".to_owned(), "hitl-2".to_owned()]));
        // Bookkeeping of the dead continuation in this fixture: events marked "own".
        let own = |event: &Event| event.provider_metadata.contains_key("own");
        let answerable = |events: &[Event], owner: &DispatchOwner| {
            continuation_pause(events, is_pause, owner, ids, own).map(|event| event.id.clone())
        };
        let mut forwarded = Event::new("child-1");
        forwarded
            .provider_metadata
            .insert("own".to_owned(), "forwarded call".to_owned());
        let mut advance = Event::new("root-2");
        advance.llm_response.content = Some(adk_rust::Content::new("model").with_text("advanced"));
        let started = continuing(&pause, owner(), "hitl-1");

        assert_eq!(
            answerable(&[pause.clone()], &owner()),
            Some(pause.id.clone())
        );
        // An ordinary advance makes the pause stale.
        assert!(answerable(&[pause.clone(), advance.clone()], &owner()).is_none());
        // Died after forwarding its replayed call, before any dispatch record
        // (round 4 item 1): this execution's replacement continues it.
        let forwarded_only = [pause.clone(), started.clone(), forwarded.clone()];
        assert_eq!(
            answerable(&forwarded_only, &owner()),
            Some(pause.id.clone())
        );
        // Died after a dispatch record.
        let dispatched = [
            pause.clone(),
            started.clone(),
            forwarded.clone(),
            record("child-1", "call-1", Phase::Dispatched, None),
        ];
        assert_eq!(answerable(&dispatched, &owner()), Some(pause.id.clone()));
        // A duplicate or late submission (another execution) is stale, even with
        // nothing else moved.
        assert!(answerable(&dispatched, &DispatchOwner::new("execution-2", 1)).is_none());
        assert!(answerable(&dispatched, &DispatchOwner::new("execution-1", 2)).is_none());
        // Any advance in the suffix is stale (round 4 item 2).
        let mut advanced = dispatched.to_vec();
        advanced.push(advance.clone());
        assert!(answerable(&advanced, &owner()).is_none());
        // Without a continuing record first, records alone prove nothing.
        assert!(
            answerable(
                &[
                    pause.clone(),
                    forwarded.clone(),
                    record("child-1", "call-1", Phase::Dispatched, None)
                ],
                &owner()
            )
            .is_none()
        );
        // A continuing record of another card, another pause or another
        // execution, or a foreign record later in the suffix, is stale.
        assert!(
            answerable(
                &[pause.clone(), continuing(&pause, owner(), "hitl-9")],
                &owner()
            )
            .is_none()
        );
        let mut other_pause = Event::new("root-1");
        other_pause.id = "another-pause".to_owned();
        assert!(
            answerable(
                &[pause.clone(), continuing(&other_pause, owner(), "hitl-1")],
                &owner()
            )
            .is_none()
        );
        assert!(
            answerable(
                &[
                    pause.clone(),
                    continuing(&pause, DispatchOwner::new("execution-2", 1), "hitl-1")
                ],
                &owner()
            )
            .is_none()
        );
        let foreign = [
            pause.clone(),
            started.clone(),
            owned_record(
                "child-1",
                "call-2",
                Phase::Dispatched,
                None,
                DispatchOwner::new("execution-2", 1),
                "hitl-1",
            ),
        ];
        assert!(answerable(&foreign, &owner()).is_none());
        // A pause whose cards cannot be read is never answerable behind a suffix.
        assert!(continuation_pause(&dispatched, is_pause, &owner(), |_| None, own).is_none());
        assert!(answerable(&[], &owner()).is_none());
        // Continuing records are not dispatches.
        assert!(
            recorded_dispatches(&dispatched, &pause.invocation_id)
                .unwrap()
                .dispatched
                .is_empty()
        );
    }

    fn padded(invocation: &str, call: &str, target_bytes: usize) -> Event {
        let base = record(invocation, call, Phase::Completed, Some(json!({"pad": ""})));
        let used = base.provider_metadata[DISPATCH_METADATA_KEY].len();
        let pad = "x".repeat(target_bytes - used);
        let event = record(
            invocation,
            call,
            Phase::Completed,
            Some(json!({ "pad": pad })),
        );
        assert_eq!(
            event.provider_metadata[DISPATCH_METADATA_KEY].len(),
            target_bytes
        );
        event
    }

    #[test]
    fn a_record_at_the_byte_limit_reads_and_one_byte_more_fails_closed() {
        let at_limit = padded("inv-1", "call-1", MAX_DISPATCH_RECORD_BYTES);
        assert!(recorded_dispatches(&[at_limit], "inv-1").is_ok());
        let over = padded("inv-1", "call-1", MAX_DISPATCH_RECORD_BYTES + 1);
        assert!(recorded_dispatches(&[over], "inv-1").is_err());
    }

    #[test]
    fn a_decision_reads_its_record_limit_and_refuses_one_more() {
        let records = |count: usize| {
            (0..count)
                .map(|index| record("inv-1", &format!("call-{index}"), Phase::Dispatched, None))
                .collect::<Vec<_>>()
        };
        assert!(recorded_dispatches(&records(MAX_DISPATCH_RECORDS * 2), "inv-1").is_ok());
        assert!(recorded_dispatches(&records(MAX_DISPATCH_RECORDS * 2 + 1), "inv-1").is_err());
        // Another decision's records do not count against this one.
        let mut mixed = records(MAX_DISPATCH_RECORDS * 2);
        mixed.push(record("inv-other", "call-x", Phase::Dispatched, None));
        assert!(recorded_dispatches(&mixed, "inv-1").is_ok());
    }

    async fn journal_session() -> (
        Arc<adk_rust::session::InMemorySessionService>,
        DispatchJournal,
    ) {
        let sessions = Arc::new(adk_rust::session::InMemorySessionService::new());
        sessions
            .create(adk_rust::session::CreateRequest {
                app_name: "elitea-agent-v1".to_owned(),
                user_id: "user-1".to_owned(),
                session_id: Some("session-1".to_owned()),
                state: std::collections::HashMap::new(),
            })
            .await
            .expect("fixture session");
        let identity = AdkIdentity::new(
            adk_rust::AppName::new("elitea-agent-v1").expect("app"),
            adk_rust::UserId::new("user-1").expect("user"),
            adk_rust::SessionId::new("session-1").expect("session"),
        );
        let journal = DispatchJournal::new(
            sessions.clone(),
            identity,
            owner(),
            DispatchAbort::default(),
        );
        (sessions, journal)
    }

    async fn stored(sessions: &adk_rust::session::InMemorySessionService) -> Vec<Event> {
        sessions
            .get(adk_rust::session::GetRequest {
                app_name: "elitea-agent-v1".to_owned(),
                user_id: "user-1".to_owned(),
                session_id: "session-1".to_owned(),
                num_recent_events: None,
                after: None,
            })
            .await
            .expect("session")
            .events()
            .all()
    }

    #[tokio::test(flavor = "current_thread")]
    async fn a_record_at_the_byte_limit_writes_and_one_byte_more_is_refused() {
        let (sessions, journal) = journal_session().await;
        let base = DispatchRecord {
            version: RECORD_VERSION,
            phase: Phase::Completed,
            decision_invocation_id: "inv-1".to_owned(),
            interrupt_id: "hitl-1".to_owned(),
            owner: owner(),
            call_id: "call-1".to_owned(),
            result: Some(json!({"pad": ""})),
        };
        let used = serde_json::to_string(&base).unwrap().len();
        let sized = |bytes: usize| DispatchRecord {
            result: Some(json!({ "pad": "x".repeat(bytes - used) })),
            ..base.clone()
        };
        journal
            .append(&sized(MAX_DISPATCH_RECORD_BYTES))
            .await
            .expect("a record at the limit is written");
        assert!(
            journal
                .append(&sized(MAX_DISPATCH_RECORD_BYTES + 1))
                .await
                .is_err()
        );
        assert_eq!(
            stored(&sessions).await.len(),
            1,
            "the oversized record is not written"
        );
    }

    struct Counting {
        calls: Arc<std::sync::atomic::AtomicUsize>,
        fail: bool,
    }

    #[async_trait]
    impl Tool for Counting {
        fn name(&self) -> &'static str {
            "write_record"
        }
        fn description(&self) -> &'static str {
            "fixture"
        }
        async fn execute(
            &self,
            _context: Arc<dyn ToolContext>,
            _arguments: Value,
        ) -> adk_rust::Result<Value> {
            self.calls.fetch_add(1, std::sync::atomic::Ordering::AcqRel);
            if self.fail {
                return Err(adk_rust::AdkError::agent("the target refused the write"));
            }
            Ok(json!({"written": true}))
        }
    }

    async fn journaled_tool(
        journal: &DispatchJournal,
        fail: bool,
        journaled: &[&str],
        outcomes: JournalOutcomes,
    ) -> Arc<dyn Tool> {
        let inner: Arc<dyn Toolset> = Arc::new(adk_rust::tool::BasicToolset::new(
            "fixture",
            vec![Arc::new(Counting {
                calls: Arc::new(std::sync::atomic::AtomicUsize::new(0)),
                fail,
            })],
        ));
        let wrapped = journal.wrap(
            vec![inner],
            &JournalDecision {
                invocation_id: "inv-1",
                interrupt_id: "hitl-1",
            },
            journaled.iter().map(|id| (*id).to_owned()).collect(),
            outcomes,
        );
        let context: Arc<dyn ReadonlyContext> =
            Arc::new(adk_rust::tool::SimpleToolContext::new("fixture"));
        wrapped[0].tools(context).await.expect("tools").remove(0)
    }

    fn call_context(call_id: &str) -> Arc<dyn ToolContext> {
        Arc::new(adk_rust::tool::SimpleToolContext::new("fixture").with_function_call_id(call_id))
    }

    /// Performance budget: a journaled call costs exactly two root appends, and
    /// a call the journal does not own costs none.
    #[tokio::test(flavor = "current_thread")]
    async fn a_journaled_call_costs_two_appends_and_any_other_call_none() {
        let (sessions, journal) = journal_session().await;
        let tool = journaled_tool(&journal, false, &["call-1"], JournalOutcomes::Recorded).await;
        tool.execute(call_context("call-2"), json!({}))
            .await
            .expect("unjournaled call");
        assert_eq!(
            stored(&sessions).await.len(),
            0,
            "no append for a call it does not own"
        );
        tool.execute(call_context("call-1"), json!({}))
            .await
            .expect("journaled call");
        let events = stored(&sessions).await;
        assert_eq!(events.len(), 2, "dispatched + completed");
        let recorded = recorded_dispatches(&events, "inv-1").unwrap();
        assert_eq!(
            recorded.completed.get("call-1"),
            Some(&json!({"written": true}))
        );
        assert!(records_bound_to(&events, &owner(), |id| id == "hitl-1").unwrap());
    }

    /// Performance budget at the root: one append per journaled call, the
    /// `dispatched` record; the Runner persists the outcome itself.
    #[tokio::test(flavor = "current_thread")]
    async fn a_root_journaled_call_costs_one_append() {
        let (sessions, journal) = journal_session().await;
        let tool = journaled_tool(&journal, false, &["call-1"], JournalOutcomes::ByRunner).await;
        tool.execute(call_context("call-1"), json!({}))
            .await
            .expect("journaled call");
        let events = stored(&sessions).await;
        assert_eq!(events.len(), 1, "dispatched only");
        let recorded = recorded_dispatches(&events, "inv-1").unwrap();
        assert!(recorded.dispatched.contains("call-1"));
    }

    /// A failed call records its outcome, so recovery serves the failure instead
    /// of "may have started" (#1321 review item 7).
    #[tokio::test(flavor = "current_thread")]
    async fn a_failed_call_records_the_error_the_model_saw() {
        let (sessions, journal) = journal_session().await;
        let tool = journaled_tool(&journal, true, &["call-1"], JournalOutcomes::Recorded).await;
        let error = tool
            .execute(call_context("call-1"), json!({}))
            .await
            .expect_err("the target refused");
        let recorded = recorded_dispatches(&stored(&sessions).await, "inv-1").unwrap();
        assert!(recorded.dispatched.is_empty(), "the outcome is known");
        assert_eq!(
            recorded.completed.get("call-1"),
            Some(&json!({ "error": error.to_string() }))
        );
    }

    /// A session whose first `failures` appends fail with `code` (a database
    /// error, or a lost writer); later appends succeed.
    struct FailingAppends {
        inner: Arc<adk_rust::session::InMemorySessionService>,
        code: &'static str,
        failures: std::sync::atomic::AtomicUsize,
    }

    #[async_trait]
    impl SessionService for FailingAppends {
        async fn create(
            &self,
            req: adk_rust::session::CreateRequest,
        ) -> adk_rust::Result<Box<dyn adk_rust::session::Session>> {
            self.inner.create(req).await
        }
        async fn get(
            &self,
            req: adk_rust::session::GetRequest,
        ) -> adk_rust::Result<Box<dyn adk_rust::session::Session>> {
            self.inner.get(req).await
        }
        async fn list(
            &self,
            req: adk_rust::session::ListRequest,
        ) -> adk_rust::Result<Vec<Box<dyn adk_rust::session::Session>>> {
            self.inner.list(req).await
        }
        async fn delete(&self, req: adk_rust::session::DeleteRequest) -> adk_rust::Result<()> {
            self.inner.delete(req).await
        }
        async fn append_event(&self, session_id: &str, event: Event) -> adk_rust::Result<()> {
            self.fail_first().await?;
            self.inner.append_event(session_id, event).await
        }
        async fn append_event_for_identity(&self, req: AppendEventRequest) -> adk_rust::Result<()> {
            self.fail_first().await?;
            self.inner.append_event_for_identity(req).await
        }
    }

    impl FailingAppends {
        async fn fail_first(&self) -> adk_rust::Result<()> {
            use std::sync::atomic::Ordering;
            if self
                .failures
                .try_update(Ordering::AcqRel, Ordering::Acquire, |left| {
                    left.checked_sub(1)
                })
                .is_ok()
            {
                // A slow failure: other approved calls get to run meanwhile.
                tokio::time::sleep(std::time::Duration::from_millis(20)).await;
                return Err(adk_rust::AdkError::new(
                    adk_rust::ErrorComponent::Session,
                    adk_rust::ErrorCategory::Unavailable,
                    self.code,
                    "fixture append failure",
                ));
            }
            Ok(())
        }
    }

    fn failing_journal(
        abort: &DispatchAbort,
        sessions: Arc<adk_rust::session::InMemorySessionService>,
    ) -> DispatchJournal {
        let failing: Arc<dyn SessionService> = Arc::new(FailingAppends {
            inner: sessions,
            code: "session.storage_unavailable",
            failures: std::sync::atomic::AtomicUsize::new(1),
        });
        DispatchJournal::new(
            failing,
            AdkIdentity::new(
                adk_rust::AppName::new("elitea-agent-v1").expect("app"),
                adk_rust::UserId::new("user-1").expect("user"),
                adk_rust::SessionId::new("session-1").expect("session"),
            ),
            owner(),
            abort.clone(),
        )
    }

    async fn counting_tool(
        journal: &DispatchJournal,
        interrupt_id: &str,
        journaled: &[&str],
        calls: &Arc<std::sync::atomic::AtomicUsize>,
    ) -> Arc<dyn Tool> {
        let inner: Arc<dyn Toolset> = Arc::new(adk_rust::tool::BasicToolset::new(
            "fixture",
            vec![Arc::new(Counting {
                calls: Arc::clone(calls),
                fail: false,
            })],
        ));
        journal.wrap(
            vec![inner],
            &JournalDecision {
                invocation_id: "inv-1",
                interrupt_id,
            },
            journaled.iter().map(|id| (*id).to_owned()).collect(),
            JournalOutcomes::Recorded,
        )[0]
        .tools(Arc::new(adk_rust::tool::SimpleToolContext::new("fixture")))
        .await
        .expect("tools")
        .remove(0)
    }

    /// #1321 review round 8: the dispatch lock and the stop are invocation-wide.
    /// Two parallel children (two journal scopes of one invocation): while the
    /// first child's `dispatched` append is slowly failing, the second child's
    /// approved call waits for it and is refused; a call that is not journaled
    /// (an authorization continuation's) never starts after the stop either.
    #[tokio::test(flavor = "current_thread")]
    async fn no_approved_effect_of_the_invocation_starts_after_a_failed_record() {
        let (sessions, _) = journal_session().await;
        let abort = DispatchAbort::default();
        let journal = failing_journal(&abort, sessions);
        let calls = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let child_a = counting_tool(&journal, "hitl-a", &["call-a"], &calls).await;
        let child_b = counting_tool(&journal, "hitl-b", &["call-b"], &calls).await;
        let authorization = counting_tool(&journal, "hitl-c", &[], &calls).await;
        let (a, b) = tokio::join!(child_a.execute(call_context("call-a"), json!({})), async {
            // Starts while A's append is still in flight.
            tokio::task::yield_now().await;
            child_b.execute(call_context("call-b"), json!({})).await
        },);
        assert!(
            a.is_err() && b.is_err(),
            "both children's calls are refused"
        );
        assert!(
            authorization
                .execute(call_context("call-c"), json!({}))
                .await
                .is_err(),
            "an unjournaled call does not start after the stop"
        );
        assert_eq!(
            calls.load(std::sync::atomic::Ordering::Acquire),
            0,
            "no effect of the invocation started"
        );
    }

    struct EchoModel;

    #[async_trait]
    impl adk_rust::Llm for EchoModel {
        fn name(&self) -> &'static str {
            "echo"
        }
        async fn generate_content(
            &self,
            _request: adk_rust::LlmRequest,
            _stream_response: bool,
        ) -> adk_rust::Result<adk_rust::LlmResponseStream> {
            Ok(Box::pin(adk_rust::futures::stream::empty()))
        }
    }

    /// #1321 review rounds 4–5: a failed `dispatched` append never lets the run
    /// continue past the approved call. Its effect does not start, no LATER
    /// approved call of the invocation starts (even though its own append would
    /// succeed), and every guarded model step refuses with the typed code,
    /// whether the store failed or the writer was lost.
    #[tokio::test(flavor = "current_thread")]
    async fn a_failed_dispatch_record_stops_every_later_effect_and_model_step() {
        for append_code in ["session.storage_unavailable", "session.writer_not_current"] {
            let (sessions, _) = journal_session().await;
            let failing: Arc<dyn SessionService> = Arc::new(FailingAppends {
                inner: sessions,
                code: append_code,
                failures: std::sync::atomic::AtomicUsize::new(1),
            });
            let identity = AdkIdentity::new(
                adk_rust::AppName::new("elitea-agent-v1").expect("app"),
                adk_rust::UserId::new("user-1").expect("user"),
                adk_rust::SessionId::new("session-1").expect("session"),
            );
            let abort = DispatchAbort::default();
            let journal = DispatchJournal::new(failing, identity, owner(), abort.clone());
            let guarded = abort.guard(Arc::new(EchoModel));
            assert!(
                guarded
                    .generate_content(adk_rust::LlmRequest::new("echo", Vec::new()), false)
                    .await
                    .is_ok(),
                "a model runs while no dispatch failed"
            );
            let calls = Arc::new(std::sync::atomic::AtomicUsize::new(0));
            let inner: Arc<dyn Toolset> = Arc::new(adk_rust::tool::BasicToolset::new(
                "fixture",
                vec![Arc::new(Counting {
                    calls: Arc::clone(&calls),
                    fail: false,
                })],
            ));
            let tool = journal.wrap(
                vec![inner],
                &JournalDecision {
                    invocation_id: "inv-1",
                    interrupt_id: "hitl-1",
                },
                BTreeSet::from(["call-1".to_owned(), "call-2".to_owned()]),
                JournalOutcomes::Recorded,
            )[0]
            .tools(Arc::new(adk_rust::tool::SimpleToolContext::new("fixture")))
            .await
            .expect("tools")
            .remove(0);
            // Both approved calls of the message run concurrently, as ADK may run
            // them: neither effect starts once the first dispatch record failed.
            let (first, second) = tokio::join!(
                tool.execute(call_context("call-1"), json!({})),
                tool.execute(call_context("call-2"), json!({})),
            );
            assert!(first.is_err() && second.is_err(), "both calls are refused");
            assert_eq!(
                calls.load(std::sync::atomic::Ordering::Acquire),
                0,
                "no effect starts once a dispatch record failed"
            );
            let Err(error) = guarded
                .generate_content(adk_rust::LlmRequest::new("echo", Vec::new()), false)
                .await
            else {
                panic!("the next model step must stop the run");
            };
            assert_eq!(error.code, DISPATCH_UNRECORDED_CODE);
        }
    }

    /// #1321 review round 5: the root run ends with the stop even when no model
    /// step follows the failed record (a child at its step limit returns
    /// normally); without a failure the stream is unchanged.
    #[tokio::test(flavor = "current_thread")]
    async fn the_root_run_ends_with_the_stop_when_no_model_step_follows() {
        use adk_rust::futures::StreamExt as _;
        let one = || -> adk_rust::EventStream {
            Box::pin(adk_rust::futures::stream::iter([Ok(Event::new("inv-1"))]))
        };
        let abort = DispatchAbort::default();
        let clean: Vec<_> = abort.stop_at_end(one()).collect().await;
        assert_eq!(clean.len(), 1);
        assert!(clean[0].is_ok());
        abort.record();
        let stopped: Vec<_> = abort.stop_at_end(one()).collect().await;
        assert_eq!(stopped.len(), 2, "the inner events, then the stop");
        assert!(
            matches!(&stopped[1], Err(error) if error.code == DISPATCH_UNRECORDED_CODE),
            "the run ends with the typed stop"
        );
    }

    #[test]
    fn forged_or_malformed_records_fail_closed() {
        let mut foreign = record("inv-1", "call-1", Phase::Dispatched, None);
        foreign.author = "user".to_owned();
        assert!(recorded_dispatches(&[foreign], "inv-1").is_err());
        let mut content = record("inv-1", "call-1", Phase::Dispatched, None);
        content.llm_response.content = Some(adk_rust::Content::new("user").with_text("x"));
        assert!(recorded_dispatches(&[content], "inv-1").is_err());
        let completed_without_result = record("inv-1", "call-1", Phase::Completed, None);
        assert!(recorded_dispatches(&[completed_without_result], "inv-1").is_err());
        let mut garbage = Event::new("inv-1");
        DISPATCH_AUTHOR.clone_into(&mut garbage.author);
        garbage.provider_metadata.insert(
            DISPATCH_METADATA_KEY.to_owned(),
            "{\"version\":1}".to_owned(),
        );
        assert!(recorded_dispatches(&[garbage], "inv-1").is_err());
    }
}
