//! FU-291: the durable threads of the saved pipelines a fan-out member's
//! Agent calls as tools.
//!
//! A pipeline tool call inside a member runs its graph on a thread `NT` that
//! is a pure function of the member Agent node's own thread `A` (the
//! activation receipt's thread) and the call's lineage (the batch, ordinal,
//! call id, tool name and arguments digest the coordinator captured from the
//! durable model event). Replay re-emits the stored batch (C2), so takeover,
//! reclaim and a generation bump all re-derive the same `NT`.
//!
//! The name grants nothing. A thread is admitted only through the member's
//! own fenced checkpointer family ([`ToolPipelineAdmission`]), which activates
//! the writer under the member's exact claim before the nested graph's first
//! effect and then registers it in the member's [`ToolPipelineThreads`]. That
//! registry is the one object the checkpointer admits into and the member's
//! event projector reads from (C1): the projector admits an event's thread only
//! by re-deriving it from the event's lineage receipt and the receipt's Agent
//! thread, and only when that exact thread is registered for that Agent.

use std::collections::BTreeMap;
use std::sync::{Arc, RwLock};

use adk_rust::Event;
use adk_rust::graph::{Checkpointer, GraphError};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use ring::digest;
use serde::{Deserialize, Serialize};

use super::node_recovery_runtime::NodeRecoveryFactory;
use crate::agents::application_pipeline::PipelineToolCallLineage;
use crate::agents::events::DESCENDANT_CHECKPOINT_THREAD_KEY;

/// D3 (a): at most this many nested pipeline calls per fan-out member, the
/// maximum of one replayed call batch.
pub(crate) const MAX_TOOL_PIPELINE_THREADS_PER_MEMBER: usize = 8;
/// A nested thread name, checked before any activation.
pub(crate) const MAX_TOOL_PIPELINE_THREAD_BYTES: usize = 480;
/// The private receipt stamped on every event a durable nested pipeline sends.
pub(crate) const DURABLE_TOOL_THREAD_METADATA_KEY: &str = "elitea.pipeline.durable-tool-thread.v1";
const THREAD_DOMAIN: &[u8] = b"elitea.pipeline.tool-thread.v1\0";
pub(crate) const THREAD_SEGMENT: &str = "/pt1:";
/// FU-291 review B: the member's ledger of its admitted nested threads,
/// `<member>/pt1=ledger`. `=` is no graph-id byte and the name holds no
/// `/pt1:`, so it is neither an admitted path nor a nested thread.
pub(crate) const TOOL_LEDGER_SEGMENT: &str = "/pt1=ledger";
/// The ledger row's state key: the admitted nested threads, in order, each
/// with the definition digest stamped at its first admission (FU-338).
pub(crate) const TOOL_LEDGER_KEY: &str = "elitea.pipeline.tool-threads.v2";
/// The FU-291 ledger key, before the stamp: a row holding it is refused.
pub(crate) const TOOL_LEDGER_KEY_UNSTAMPED: &str = "elitea.pipeline.tool-threads.v1";
const MAX_RECEIPT_BYTES: usize = 4096;
/// The stable code of a refused admission the model reads as a tool error.
pub(crate) const TOOL_PIPELINE_LIMIT_CODE: &str = "pipeline.tool_thread_limit";
/// The member's shared thread total (`MAX_CHILD_THREADS`) is reached.
pub(crate) const TOOL_PIPELINE_TOTAL_LIMIT_CODE: &str = "pipeline.tool_thread_total_limit";
const TOOL_PIPELINE_SCOPE_CODE: &str = "pipeline.tool_thread_invalid";

fn scope_error(detail: &str) -> GraphError {
    GraphError::CheckpointError(format!("{TOOL_PIPELINE_SCOPE_CODE}: {detail}"))
}

fn limit_error() -> GraphError {
    GraphError::CheckpointError(format!(
        "{TOOL_PIPELINE_LIMIT_CODE}: a fan-out member runs at most \
         {MAX_TOOL_PIPELINE_THREADS_PER_MEMBER} saved pipeline calls"
    ))
}

fn total_limit_error() -> GraphError {
    GraphError::CheckpointError(format!(
        "{TOOL_PIPELINE_TOTAL_LIMIT_CODE}: a fan-out member holds at most {} threads in all",
        super::fanout_budget::MAX_CHILD_THREADS
    ))
}

fn coded(error: &GraphError, code: &str) -> bool {
    matches!(error, GraphError::CheckpointError(message)
        if message.strip_prefix(code).is_some_and(|rest| rest.starts_with(':')))
}

/// Whether `error` is a refusal of the admission bounds (D3, or the member's
/// shared total), returned to the model as a tool error, never a dependency
/// failure.
pub(crate) fn is_tool_pipeline_limit(error: &GraphError) -> bool {
    coded(error, TOOL_PIPELINE_LIMIT_CODE) || is_tool_pipeline_total_limit(error)
}

/// Whether `error` is the member's shared thread total (review 5).
pub(crate) fn is_tool_pipeline_total_limit(error: &GraphError) -> bool {
    coded(error, TOOL_PIPELINE_TOTAL_LIMIT_CODE)
}

/// This module's own code carried by `error`, for an operator log line.
pub(crate) fn tool_pipeline_error_code(error: &GraphError) -> Option<&'static str> {
    [
        TOOL_PIPELINE_LIMIT_CODE,
        TOOL_PIPELINE_TOTAL_LIMIT_CODE,
        TOOL_PIPELINE_SCOPE_CODE,
    ]
    .into_iter()
    .find(|code| coded(error, code))
}

fn valid_agent_thread(thread: &str) -> bool {
    !thread.is_empty()
        && thread.len() < MAX_TOOL_PIPELINE_THREAD_BYTES
        && !thread.chars().any(char::is_control)
        && !thread.contains(THREAD_SEGMENT)
}

/// D-2: the nested thread of one pipeline call by the Agent on `agent_thread`.
/// Pure: it activates nothing and reads no clock.
pub(crate) fn tool_pipeline_thread(
    agent_thread: &str,
    lineage: &PipelineToolCallLineage,
) -> Result<String, GraphError> {
    if !valid_agent_thread(agent_thread) {
        return Err(scope_error("the Agent thread is invalid"));
    }
    lineage
        .validate()
        .map_err(|_| scope_error("the call lineage is invalid"))?;
    // Review B1: an explicit, length-prefixed encoding of the five lineage
    // fields, never a serializer's output, so a later lineage field or rename
    // cannot change the thread of an in-flight call. Pinned by the golden test.
    let (batch, ordinal, call, tool, arguments) = lineage.identity_fields();
    let ordinal = u64::try_from(ordinal)
        .map_err(|_| scope_error("the call lineage is invalid"))?
        .to_be_bytes();
    let mut hash = digest::Context::new(&digest::SHA256);
    hash.update(THREAD_DOMAIN);
    for field in [
        agent_thread.as_bytes(),
        batch.as_bytes(),
        ordinal.as_slice(),
        call.as_bytes(),
        tool.as_bytes(),
        arguments.as_bytes(),
    ] {
        let length =
            u64::try_from(field.len()).map_err(|_| scope_error("the identity is invalid"))?;
        hash.update(&length.to_be_bytes());
        hash.update(field);
    }
    let thread = format!(
        "{agent_thread}{THREAD_SEGMENT}{}",
        URL_SAFE_NO_PAD.encode(hash.finish().as_ref())
    );
    if thread.len() > MAX_TOOL_PIPELINE_THREAD_BYTES {
        return Err(scope_error("the nested thread exceeds its bound"));
    }
    Ok(thread)
}

/// D-6: a durable nested pipeline's visit, a pure function of its thread:
/// its direct nodes' journal identity. Pinned by the golden test.
pub(crate) fn tool_pipeline_visit(thread: &str) -> String {
    let mut digest = digest::Context::new(&digest::SHA256);
    digest.update(b"elitea.pipeline.tool-thread.visit.v1\0");
    digest.update(thread.as_bytes());
    let mut visit = String::with_capacity(64);
    for byte in digest.finish().as_ref() {
        let _ = std::fmt::Write::write_fmt(&mut visit, format_args!("{byte:02x}"));
    }
    visit
}

/// A nested pipeline's thread writer: its checkpoints and its node journals.
pub(crate) trait ToolPipelineWriter: Checkpointer + NodeRecoveryFactory {}
impl<T: Checkpointer + NodeRecoveryFactory> ToolPipelineWriter for T {}

/// One admitted nested thread, durable before it is registered.
#[derive(Clone)]
pub(crate) struct AdmittedToolPipeline {
    pub(crate) thread_id: String,
    pub(crate) agent_thread: String,
    pub(crate) writer: Arc<dyn ToolPipelineWriter>,
    /// FU-338: Main's frozen definition digest of the saved pipeline, stamped
    /// in the member's ledger at this thread's first admission, before its
    /// writer existed; `None` when that admission had none.
    pub(crate) definition_sha256: Option<[u8; 32]>,
}

/// The append-only, bounded set of one member's admitted nested threads.
pub(crate) struct ToolPipelineThreads {
    /// The member family's own threads at assembly: the 129 total includes them.
    static_threads: usize,
    entries: RwLock<BTreeMap<String, AdmittedToolPipeline>>,
    /// Admission is serialized per member: one activation per new thread.
    admission: tokio::sync::Mutex<()>,
}

impl ToolPipelineThreads {
    pub(crate) fn new(static_threads: usize) -> Self {
        Self {
            static_threads,
            entries: RwLock::default(),
            admission: tokio::sync::Mutex::new(()),
        }
    }

    pub(crate) async fn admission(&self) -> tokio::sync::MutexGuard<'_, ()> {
        self.admission.lock().await
    }

    /// The registered thread, if it belongs to exactly `agent_thread`.
    pub(crate) fn admitted(
        &self,
        thread: &str,
        agent_thread: &str,
    ) -> Option<AdmittedToolPipeline> {
        self.entries
            .read()
            .ok()?
            .get(thread)
            .filter(|entry| entry.agent_thread == agent_thread)
            .cloned()
    }

    #[cfg(test)]
    pub(crate) fn len(&self) -> usize {
        self.entries
            .read()
            .map_or(usize::MAX, |entries| entries.len())
    }

    /// D3 and the shared total for this registry alone (tests; admission
    /// checks the durable ledger with [`Self::check_room_for`]).
    #[cfg(test)]
    pub(crate) fn check_room(&self) -> Result<(), GraphError> {
        self.check_room_for(self.len())
    }

    /// The bounds for a member that already holds `admitted` nested threads:
    /// the durable count at admission (review 2), never only this process's.
    pub(crate) fn check_room_for(&self, admitted: usize) -> Result<(), GraphError> {
        if admitted >= MAX_TOOL_PIPELINE_THREADS_PER_MEMBER {
            return Err(limit_error());
        }
        if self.static_threads.saturating_add(admitted) >= super::fanout_budget::MAX_CHILD_THREADS {
            return Err(total_limit_error());
        }
        Ok(())
    }

    /// Append one thread whose writer is already active (review H: the bounds
    /// were checked against the durable ledger before the activation, so this
    /// cannot refuse and never orphans the row). Never replaces one.
    pub(crate) fn register(&self, admitted: AdmittedToolPipeline) {
        let mut entries = self
            .entries
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        entries
            .entry(admitted.thread_id.clone())
            .or_insert(admitted);
    }
}

/// A member family that admits nested pipeline threads under its own claim.
#[async_trait]
pub(crate) trait ToolPipelineAdmission: Send + Sync {
    /// The one set this family admits into and the member's projector reads.
    fn tool_threads(&self) -> Arc<ToolPipelineThreads>;

    /// D-3: activate the fenced writer of the nested thread of `lineage`, called
    /// by the Agent on `agent_thread`, before the nested graph's first effect.
    /// FU-338: a first admission stamps `definition_sha256` durably before the
    /// writer; a re-admission returns the stamp it holds, never replaced.
    async fn admit_tool_pipeline(
        &self,
        agent_thread: &str,
        lineage: &PipelineToolCallLineage,
        definition_sha256: Option<[u8; 32]>,
    ) -> Result<AdmittedToolPipeline, GraphError>;
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct DurableToolThreadReceipt {
    schema: String,
    agent_thread: String,
    lineage: PipelineToolCallLineage,
}

/// The receipt that proves a durable call's nested thread, serialized once
/// per call and stamped on each of its events (review 6).
pub(crate) fn durable_tool_receipt(
    agent_thread: &str,
    lineage: &PipelineToolCallLineage,
) -> Result<String, GraphError> {
    let receipt = serde_json::to_string(&DurableToolThreadReceipt {
        schema: DURABLE_TOOL_THREAD_METADATA_KEY.to_owned(),
        agent_thread: agent_thread.to_owned(),
        lineage: lineage.clone(),
    })
    .map_err(|_| scope_error("the nested thread receipt is invalid"))?;
    if receipt.len() > MAX_RECEIPT_BYTES {
        return Err(scope_error("the nested thread receipt exceeds its bound"));
    }
    Ok(receipt)
}

/// Stamp the receipt that proves `event`'s nested thread.
#[cfg(test)]
pub(crate) fn stamp_durable_tool_thread(
    event: &mut Event,
    agent_thread: &str,
    lineage: &PipelineToolCallLineage,
) -> Result<(), GraphError> {
    let receipt = durable_tool_receipt(agent_thread, lineage)?;
    event
        .provider_metadata
        .insert(DURABLE_TOOL_THREAD_METADATA_KEY.to_owned(), receipt);
    Ok(())
}

/// An event's receipt, parsed once, with the nested thread it derives (one
/// SHA-256). `None` without a receipt; an error when it is malformed or does
/// not derive the thread the event names.
fn proved_receipt(event: &Event) -> Result<Option<(String, DurableToolThreadReceipt)>, GraphError> {
    let Some(raw) = event
        .provider_metadata
        .get(DURABLE_TOOL_THREAD_METADATA_KEY)
    else {
        return Ok(None);
    };
    if raw.len() > MAX_RECEIPT_BYTES {
        return Err(scope_error("the nested thread receipt exceeds its bound"));
    }
    let receipt: DurableToolThreadReceipt = serde_json::from_str(raw)
        .map_err(|_| scope_error("the nested thread receipt is invalid"))?;
    if receipt.schema != DURABLE_TOOL_THREAD_METADATA_KEY {
        return Err(scope_error("the nested thread receipt is invalid"));
    }
    let thread = tool_pipeline_thread(&receipt.agent_thread, &receipt.lineage)?;
    if event
        .provider_metadata
        .get(DESCENDANT_CHECKPOINT_THREAD_KEY)
        .is_some_and(|named| named != &thread)
    {
        return Err(scope_error("the nested thread does not match its receipt"));
    }
    Ok(Some((thread, receipt)))
}

/// The nested thread `event` names, re-derived from its receipt, and the
/// receipt's Agent thread. `None` without a receipt; an error when the
/// receipt is malformed or does not derive the thread the event names.
pub(crate) fn proved_durable_tool_thread(
    event: &Event,
) -> Result<Option<(String, String)>, GraphError> {
    Ok(proved_receipt(event)?.map(|(thread, receipt)| (thread, receipt.agent_thread)))
}

/// The browser projector's admission of a durable nested pipeline's event:
/// the receipt's lineage is exactly the projected call (batch, ordinal, call
/// id, name, arguments) and it re-derives the thread the event names. `None`
/// without a receipt. One parse and one derivation (review 6).
pub(crate) fn durable_thread_for_projected_call(
    event: &Event,
    batch: &str,
    ordinal: usize,
    call: &str,
    name: &str,
    args: &serde_json::Value,
) -> Result<Option<String>, GraphError> {
    let Some((thread, receipt)) = proved_receipt(event)? else {
        return Ok(None);
    };
    let lineage = &receipt.lineage;
    if lineage.parent_call_id() != call
        || !event
            .provider_metadata
            .contains_key(DESCENDANT_CHECKPOINT_THREAD_KEY)
        || !lineage
            .matches_projection(batch, ordinal, name, args)
            .map_err(|_| scope_error("the call lineage is invalid"))?
    {
        return Err(scope_error("the nested thread receipt names another call"));
    }
    Ok(Some(thread))
}

#[cfg(test)]
#[path = "tool_pipeline_threads_tests.rs"]
pub(crate) mod tests;
