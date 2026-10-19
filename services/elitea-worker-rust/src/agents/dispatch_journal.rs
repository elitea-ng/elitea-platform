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
//! Records carry no content and no state delta, so ADK history, the replay
//! proofs and the browser projection ignore them.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;

use adk_rust::session::{AppendEventRequest, SessionService};
use adk_rust::{AdkIdentity, Event, ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use serde_json::Value;

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
}

#[derive(Deserialize, Serialize)]
#[cfg_attr(test, derive(Clone))]
#[serde(deny_unknown_fields)]
struct DispatchRecord {
    version: u8,
    phase: Phase,
    /// The invocation of the paused assistant message the decision answered.
    decision_invocation_id: String,
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

/// Read the records of one paused message from the root session's events.
pub(crate) fn recorded_dispatches(
    events: &[Event],
    decision_invocation_id: &str,
) -> Result<RecordedDispatches, InvalidDispatchRecord> {
    let mut recorded = RecordedDispatches::default();
    let mut count = 0_usize;
    for event in events {
        let Some(raw) = event.provider_metadata.get(DISPATCH_METADATA_KEY) else {
            continue;
        };
        if event.author != DISPATCH_AUTHOR
            || raw.len() > MAX_DISPATCH_RECORD_BYTES
            || event.llm_response.content.is_some()
            || !event.actions.state_delta.is_empty()
        {
            return Err(InvalidDispatchRecord);
        }
        let record: DispatchRecord =
            serde_json::from_str(raw).map_err(|_| InvalidDispatchRecord)?;
        if record.version != RECORD_VERSION || record.call_id.is_empty() {
            return Err(InvalidDispatchRecord);
        }
        if record.decision_invocation_id != decision_invocation_id {
            continue;
        }
        count += 1;
        if count > MAX_DISPATCH_RECORDS * 2 {
            return Err(InvalidDispatchRecord);
        }
        match (record.phase, record.result) {
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
/// after it dispatched an approved call left its own suffix behind it (its
/// marker, the replayed child events and its dispatch records). That suffix
/// belongs to the same decision, so the pause stays answerable and the
/// decision is resolved against its records: a recorded outcome is served and
/// an unrecorded one is refused. Without a dispatch record the suffix is an
/// ordinary advance and the decision is stale (`None`).
pub(crate) fn continuation_pause(
    events: &[Event],
    is_pause: impl Fn(&Event) -> bool,
) -> Option<&Event> {
    let last = events.last()?;
    if is_pause(last) {
        return Some(last);
    }
    let index = events.iter().rposition(&is_pause)?;
    events[index + 1..]
        .iter()
        .any(|event| event.provider_metadata.contains_key(DISPATCH_METADATA_KEY))
        .then_some(&events[index])
}

/// The fenced root-session writer for dispatch records.
#[derive(Clone)]
pub(crate) struct DispatchJournal {
    sessions: Arc<dyn SessionService>,
    identity: AdkIdentity,
}

impl DispatchJournal {
    pub(crate) fn new(sessions: Arc<dyn SessionService>, identity: AdkIdentity) -> Self {
        Self { sessions, identity }
    }

    /// Wrap toolsets so each listed call id is recorded around its execution.
    pub(crate) fn wrap(
        &self,
        toolsets: Vec<Arc<dyn Toolset>>,
        decision_invocation_id: &str,
        call_ids: BTreeSet<String>,
    ) -> Vec<Arc<dyn Toolset>> {
        if call_ids.is_empty() {
            return toolsets;
        }
        let scope = Arc::new(JournalScope {
            journal: self.clone(),
            decision_invocation_id: decision_invocation_id.to_owned(),
            call_ids,
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

struct JournalScope {
    journal: DispatchJournal,
    decision_invocation_id: String,
    call_ids: BTreeSet<String>,
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
            return self.inner.execute(context, arguments).await;
        }
        // The dispatch is durable before the effect, or the effect never starts.
        self.scope
            .journal
            .append(&DispatchRecord {
                version: RECORD_VERSION,
                phase: Phase::Dispatched,
                decision_invocation_id: self.scope.decision_invocation_id.clone(),
                call_id: call_id.clone(),
                result: None,
            })
            .await?;
        let outcome = self.inner.execute(context, arguments).await;
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
            .append(&DispatchRecord {
                version: RECORD_VERSION,
                phase: Phase::Completed,
                decision_invocation_id: self.scope.decision_invocation_id.clone(),
                call_id,
                result: Some(recorded),
            })
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

    fn record(invocation: &str, call: &str, phase: Phase, result: Option<Value>) -> Event {
        let mut event = Event::new(invocation);
        DISPATCH_AUTHOR.clone_into(&mut event.author);
        event.provider_metadata.insert(
            DISPATCH_METADATA_KEY.to_owned(),
            serde_json::to_string(&DispatchRecord {
                version: RECORD_VERSION,
                phase,
                decision_invocation_id: invocation.to_owned(),
                call_id: call.to_owned(),
                result,
            })
            .unwrap(),
        );
        event
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

    #[test]
    fn a_pause_stays_answerable_only_behind_a_dispatched_continuation() {
        let mut pause = Event::new("root-1");
        pause
            .provider_metadata
            .insert("pause".to_owned(), "yes".to_owned());
        let is_pause = |event: &Event| event.provider_metadata.contains_key("pause");
        let mut later = Event::new("root-2");
        later.llm_response.content = Some(adk_rust::Content::new("model").with_text("advanced"));
        assert!(continuation_pause(&[pause.clone()], is_pause).is_some());
        // An ordinary advance makes the pause stale.
        assert!(continuation_pause(&[pause.clone(), later.clone()], is_pause).is_none());
        // A continuation that dispatched an approved call keeps it answerable.
        let record = record("child-1", "call-1", Phase::Dispatched, None);
        let events = [pause.clone(), later, record];
        assert_eq!(
            continuation_pause(&events, is_pause).map(|event| event.id.clone()),
            Some(pause.id)
        );
        assert!(continuation_pause(&[], is_pause).is_none());
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
        let journal = DispatchJournal::new(sessions.clone(), identity);
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
            "inv-1",
            journaled.iter().map(|id| (*id).to_owned()).collect(),
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
        let tool = journaled_tool(&journal, false, &["call-1"]).await;
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
    }

    /// A failed call records its outcome, so recovery serves the failure instead
    /// of "may have started" (#1321 review item 7).
    #[tokio::test(flavor = "current_thread")]
    async fn a_failed_call_records_the_error_the_model_saw() {
        let (sessions, journal) = journal_session().await;
        let tool = journaled_tool(&journal, true, &["call-1"]).await;
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
