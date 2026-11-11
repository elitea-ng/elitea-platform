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
const THREAD_SEGMENT: &str = "/pt1:";
const MAX_RECEIPT_BYTES: usize = 4096;
/// The stable code of a refused admission the model reads as a tool error.
pub(crate) const TOOL_PIPELINE_LIMIT_CODE: &str = "pipeline.tool_thread_limit";
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

/// Whether `error` is a refusal of the admission bounds (D3), returned to the
/// model as a tool error, never a dependency failure.
pub(crate) fn is_tool_pipeline_limit(error: &GraphError) -> bool {
    matches!(error, GraphError::CheckpointError(message)
        if message.starts_with(TOOL_PIPELINE_LIMIT_CODE))
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
    let lineage =
        serde_json::to_vec(lineage).map_err(|_| scope_error("the call lineage is invalid"))?;
    let mut hash = digest::Context::new(&digest::SHA256);
    hash.update(THREAD_DOMAIN);
    for field in [agent_thread.as_bytes(), lineage.as_slice()] {
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

/// A nested pipeline's thread writer: its checkpoints and its node journals.
pub(crate) trait ToolPipelineWriter: Checkpointer + NodeRecoveryFactory {}
impl<T: Checkpointer + NodeRecoveryFactory> ToolPipelineWriter for T {}

/// One admitted nested thread, durable before it is registered.
#[derive(Clone)]
pub(crate) struct AdmittedToolPipeline {
    pub(crate) thread_id: String,
    pub(crate) agent_thread: String,
    pub(crate) writer: Arc<dyn ToolPipelineWriter>,
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

    pub(crate) fn len(&self) -> usize {
        self.entries
            .read()
            .map_or(usize::MAX, |entries| entries.len())
    }

    /// D3 and the shared total, checked before any activation.
    pub(crate) fn check_room(&self) -> Result<(), GraphError> {
        let admitted = self.len();
        if admitted >= MAX_TOOL_PIPELINE_THREADS_PER_MEMBER
            || self.static_threads.saturating_add(admitted)
                >= super::fanout_budget::MAX_CHILD_THREADS
        {
            return Err(limit_error());
        }
        Ok(())
    }

    /// Append one thread whose writer is already active. Never replaces one.
    pub(crate) fn register(&self, admitted: AdmittedToolPipeline) -> Result<(), GraphError> {
        self.check_room()?;
        let mut entries = self
            .entries
            .write()
            .map_err(|_| scope_error("the nested thread set is unavailable"))?;
        if entries.contains_key(&admitted.thread_id) {
            return Err(scope_error("the nested thread is already admitted"));
        }
        entries.insert(admitted.thread_id.clone(), admitted);
        Ok(())
    }
}

/// A member family that admits nested pipeline threads under its own claim.
#[async_trait]
pub(crate) trait ToolPipelineAdmission: Send + Sync {
    /// The one set this family admits into and the member's projector reads.
    fn tool_threads(&self) -> Arc<ToolPipelineThreads>;

    /// D-3: activate the fenced writer of the nested thread of `lineage`, called
    /// by the Agent on `agent_thread`, before the nested graph's first effect.
    async fn admit_tool_pipeline(
        &self,
        agent_thread: &str,
        lineage: &PipelineToolCallLineage,
    ) -> Result<AdmittedToolPipeline, GraphError>;
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct DurableToolThreadReceipt {
    schema: String,
    agent_thread: String,
    lineage: PipelineToolCallLineage,
}

/// Stamp the receipt that proves `event`'s nested thread.
pub(crate) fn stamp_durable_tool_thread(
    event: &mut Event,
    agent_thread: &str,
    lineage: &PipelineToolCallLineage,
) -> Result<(), GraphError> {
    let receipt = serde_json::to_string(&DurableToolThreadReceipt {
        schema: DURABLE_TOOL_THREAD_METADATA_KEY.to_owned(),
        agent_thread: agent_thread.to_owned(),
        lineage: lineage.clone(),
    })
    .map_err(|_| scope_error("the nested thread receipt is invalid"))?;
    if receipt.len() > MAX_RECEIPT_BYTES {
        return Err(scope_error("the nested thread receipt exceeds its bound"));
    }
    event
        .provider_metadata
        .insert(DURABLE_TOOL_THREAD_METADATA_KEY.to_owned(), receipt);
    Ok(())
}

/// The nested thread `event` names, re-derived from its receipt, and the
/// receipt's Agent thread. `None` without a receipt; an error when the
/// receipt is malformed or does not derive the thread the event names.
pub(crate) fn proved_durable_tool_thread(
    event: &Event,
) -> Result<Option<(String, String)>, GraphError> {
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
    Ok(Some((thread, receipt.agent_thread)))
}

/// The call lineage a receipt carries, for the projector's call match.
pub(crate) fn durable_tool_thread_lineage(
    event: &Event,
) -> Result<Option<PipelineToolCallLineage>, GraphError> {
    let Some(raw) = event
        .provider_metadata
        .get(DURABLE_TOOL_THREAD_METADATA_KEY)
    else {
        return Ok(None);
    };
    if raw.len() > MAX_RECEIPT_BYTES {
        return Err(scope_error("the nested thread receipt exceeds its bound"));
    }
    serde_json::from_str::<DurableToolThreadReceipt>(raw)
        .map(|receipt| Some(receipt.lineage))
        .map_err(|_| scope_error("the nested thread receipt is invalid"))
}

/// The browser projector's admission of a durable nested pipeline's event:
/// the receipt's lineage is exactly the projected call (batch, ordinal, call
/// id, name, arguments) and it re-derives the thread the event names. `None`
/// without a receipt.
pub(crate) fn durable_thread_for_projected_call(
    event: &Event,
    batch: &str,
    ordinal: usize,
    call: &str,
    name: &str,
    args: &serde_json::Value,
) -> Result<Option<String>, GraphError> {
    let Some(lineage) = durable_tool_thread_lineage(event)? else {
        return Ok(None);
    };
    let Some((thread, _)) = proved_durable_tool_thread(event)? else {
        return Ok(None);
    };
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
