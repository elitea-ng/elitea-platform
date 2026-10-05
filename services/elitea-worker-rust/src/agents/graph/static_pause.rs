//! Definition-bound authored pauses and ordinary text continuation.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;

use adk_rust::graph::checkpoint::RetentionPolicy;
use adk_rust::graph::{Checkpoint, Checkpointer, GraphError, State};
use adk_rust::session::Session;
use async_trait::async_trait;
use ring::digest;
use serde::{Deserialize, Serialize};
use serde_json::json;

use super::printer::PrinterPauseCatalog;
use super::resume::{
    PipelineResume, PipelineResumeError, PrinterContinuation, PrinterResumeContext,
};
use super::yaml::valid_graph_id;
use crate::agents::events::{PipelineStaticEventBinding, pipeline_static_event_binding};
use crate::agents::request::AgentExecutionPayload;

pub(crate) const STATIC_PAUSE_METADATA_KEY: &str = "elitea.pipeline.static_pause";
pub(crate) const STATIC_PAUSE_SCHEMA: &str = "elitea.pipeline.static-pause.v1";
pub(crate) const STATIC_PAUSE_MESSAGE: &str = "Pipeline paused. Type a message to continue.";
pub(crate) const STATIC_TEXT_RESUME_STATE_KEY: &str = "__elitea_static_text_resume_v1";
pub(crate) const STATIC_AFTER_CHECKPOINTS_STATE_KEY: &str = "__elitea_static_after_checkpoints_v1";

/// Immutable policy plus the exact frontier saved before pause publication.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct StaticPauseMetadata {
    pub(crate) schema: String,
    pub(crate) guardrail_type: String,
    pub(crate) kind: String,
    pub(crate) node_name: String,
    pub(crate) definition_digest: String,
    pub(crate) node_digest: String,
    pub(crate) pending_nodes: Vec<String>,
    pub(crate) step: usize,
}

impl StaticPauseMetadata {
    pub(crate) fn validate(&self) -> bool {
        let unique: BTreeSet<_> = self.pending_nodes.iter().collect();
        self.schema == STATIC_PAUSE_SCHEMA
            && self.guardrail_type == "pipeline_static"
            && matches!(self.kind.as_str(), "before" | "after")
            && valid_graph_id(&self.node_name)
            && valid_digest(&self.definition_digest)
            && valid_digest(&self.node_digest)
            && self.pending_nodes.len() <= 128
            && unique.len() == self.pending_nodes.len()
            && self.pending_nodes.iter().all(|node| valid_graph_id(node))
            && (self.kind != "before" || self.pending_nodes.as_slice() == [self.node_name.as_str()])
    }

    pub(crate) fn checkpoint_matches(&self, checkpoint: &Checkpoint) -> bool {
        self.validate()
            && checkpoint.pending_nodes == self.pending_nodes
            && checkpoint.step == self.step
            && match self.kind.as_str() {
                "before" => checkpoint.cleared_interrupt.as_deref() == Some(&self.node_name),
                "after" => checkpoint
                    .cleared_interrupt
                    .as_deref()
                    .is_none_or(|node| node == self.node_name),
                _ => false,
            }
    }

    pub(crate) fn native_message(&self) -> String {
        format!("Interrupt {} '{}'", self.kind, self.node_name)
    }
}

#[derive(Clone)]
struct StaticPausePolicy {
    definition_digest: String,
    node_digest: String,
    successors: BTreeSet<String>,
}

/// Frozen node policies. No runtime authority or mutable graph state enters it.
#[derive(Clone, Default)]
pub(crate) struct StaticPauseCatalog {
    entries: BTreeMap<(String, String), StaticPausePolicy>,
}

impl StaticPauseCatalog {
    pub(super) fn from_definition(
        definition_digest: [u8; 32],
        before: &[String],
        after: &[String],
        nodes: impl Iterator<Item = (String, [u8; 32], Vec<String>, bool)>,
    ) -> Self {
        let mut entries = BTreeMap::new();
        for (id, node_digest, successors, printer) in nodes {
            let policy = StaticPausePolicy {
                definition_digest: sha256_label(&definition_digest),
                node_digest: sha256_label(&node_digest),
                successors: successors.into_iter().collect(),
            };
            if before.contains(&id) {
                entries.insert(("before".to_owned(), id.clone()), policy.clone());
            }
            // Printer already owns this exact after checkpoint and public output.
            if after.contains(&id) && !printer {
                entries.insert(("after".to_owned(), id), policy);
            }
        }
        Self { entries }
    }

    pub(crate) fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    pub(crate) fn contains_exact(&self, metadata: &StaticPauseMetadata) -> bool {
        self.entries
            .get(&(metadata.kind.clone(), metadata.node_name.clone()))
            .is_some_and(|policy| {
                metadata.validate()
                    && policy.definition_digest == metadata.definition_digest
                    && policy.node_digest == metadata.node_digest
                    && (metadata.kind == "before"
                        || ((!metadata.pending_nodes.is_empty()
                            || policy.successors.is_empty()
                            || policy.successors.contains("END"))
                            && metadata.pending_nodes.iter().all(|node| {
                                policy.successors.contains(node)
                                    || (node == "__elitea_subgraph_result_v1"
                                        && policy.successors.contains("END"))
                            })))
            })
    }

    pub(crate) fn bind(
        &self,
        kind: &str,
        node: &str,
        checkpoint: &Checkpoint,
    ) -> Option<StaticPauseMetadata> {
        let policy = self.entries.get(&(kind.to_owned(), node.to_owned()))?;
        let metadata = StaticPauseMetadata {
            schema: STATIC_PAUSE_SCHEMA.to_owned(),
            guardrail_type: "pipeline_static".to_owned(),
            kind: kind.to_owned(),
            node_name: node.to_owned(),
            definition_digest: policy.definition_digest.clone(),
            node_digest: policy.node_digest.clone(),
            pending_nodes: checkpoint.pending_nodes.clone(),
            step: checkpoint.step,
        };
        (self.contains_exact(&metadata) && metadata.checkpoint_matches(checkpoint))
            .then_some(metadata)
    }

    pub(crate) fn bind_native_message(
        &self,
        message: &str,
        checkpoint: &Checkpoint,
    ) -> Option<StaticPauseMetadata> {
        self.entries.keys().find_map(|(kind, node)| {
            (message == format!("Interrupt {kind} '{node}'"))
                .then(|| self.bind(kind, node, checkpoint))
                .flatten()
        })
    }
}

/// Exact admitted ADK descendant paths, reconstructed from frozen saved versions.
#[derive(Clone)]
pub(crate) struct StaticPauseFamily {
    root: StaticPauseCatalog,
    descendants: BTreeMap<String, StaticPauseCatalog>,
}

impl StaticPauseFamily {
    pub(crate) const fn new(
        root: StaticPauseCatalog,
        descendants: BTreeMap<String, StaticPauseCatalog>,
    ) -> Self {
        Self { root, descendants }
    }
    fn catalog(&self, root_thread: &str, thread: &str) -> Option<&StaticPauseCatalog> {
        if thread == root_thread {
            return Some(&self.root);
        }
        self.descendants
            .get(thread.strip_prefix(root_thread)?.strip_prefix('/')?)
    }
}

pub(super) fn policy_digest(base: [u8; 32], before: &[String], after: &[String]) -> [u8; 32] {
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(b"elitea.graph.pipeline.static-policy.v1\0");
    context.update(&base);
    for (kind, nodes) in [(b"before".as_slice(), before), (b"after".as_slice(), after)] {
        context.update(&(kind.len() as u64).to_be_bytes());
        context.update(kind);
        context.update(&(nodes.len() as u64).to_be_bytes());
        for node in nodes {
            context.update(&(node.len() as u64).to_be_bytes());
            context.update(node.as_bytes());
        }
    }
    let mut result = [0; 32];
    result.copy_from_slice(context.finish().as_ref());
    result
}

/// Preserve Printer admission and select the persisted pause kind after session restore.
pub(crate) struct PipelineTextContinuation {
    printer: PrinterContinuation,
    expected_pause_id: Option<String>,
}

impl PipelineTextContinuation {
    /// Build the Printer continuation for an ordinary message.
    #[must_use]
    pub(crate) const fn ordinary_message() -> Self {
        Self {
            printer: PrinterContinuation::ordinary_message(),
            expected_pause_id: None,
        }
    }

    pub(crate) fn from_payload(
        payload: &AgentExecutionPayload,
    ) -> Result<Self, PipelineResumeError> {
        let printer = PrinterContinuation::from_payload(payload)?;
        let expected_pause_id = payload
            .meta
            .get("pipeline_static_resume_v1")
            .map(|raw| {
                let object = raw.as_object().ok_or_else(PipelineResumeError::invalid)?;
                let id = object
                    .get("pause_id")
                    .and_then(serde_json::Value::as_str)
                    .ok_or_else(PipelineResumeError::invalid)?;
                if object.len() != 2
                    || object.get("revision").and_then(serde_json::Value::as_u64) != Some(1)
                    || !id
                        .strip_prefix("pipeline-static:")
                        .is_some_and(valid_digest)
                {
                    return Err(PipelineResumeError::invalid());
                }
                Ok(id.to_owned())
            })
            .transpose()?;
        Ok(Self {
            printer,
            expected_pause_id,
        })
    }

    #[cfg(test)]
    #[allow(clippy::too_many_arguments)] // Each argument binds an existing resume authority.
    pub(crate) async fn resolve(
        self,
        session: &dyn Session,
        checkpointer: &dyn Checkpointer,
        root_agent_name: &str,
        thread_id: &str,
        printer_catalog: &PrinterPauseCatalog,
        static_catalog: &StaticPauseCatalog,
        user_input: &str,
    ) -> Result<PipelineResume, PipelineResumeError> {
        self.resolve_with_family(
            session,
            checkpointer,
            root_agent_name,
            thread_id,
            printer_catalog,
            &StaticPauseFamily::new(static_catalog.clone(), BTreeMap::new()),
            user_input,
        )
        .await
    }

    #[allow(clippy::too_many_arguments)] // Exact frozen path and pause authorities remain explicit.
    pub(crate) async fn resolve_with_family(
        self,
        session: &dyn Session,
        checkpointer: &dyn Checkpointer,
        root_agent_name: &str,
        thread_id: &str,
        printer_catalog: &PrinterPauseCatalog,
        family: &StaticPauseFamily,
        user_input: &str,
    ) -> Result<PipelineResume, PipelineResumeError> {
        let events = session.events().all();
        let latest = events.last().ok_or_else(PipelineResumeError::stale)?;
        if latest
            .provider_metadata
            .contains_key(super::printer::PRINTER_PAUSE_METADATA_KEY)
        {
            if self.expected_pause_id.is_some() {
                return Err(PipelineResumeError::stale());
            }
            let resume = self
                .printer
                .resolve(
                    PrinterResumeContext::new(
                        session,
                        checkpointer,
                        root_agent_name,
                        thread_id,
                        printer_catalog,
                    ),
                    user_input,
                )
                .await?;
            let payload = adk_rust::graph::interrupt::GraphInterruptPayload::from_event(latest)
                .ok_or_else(PipelineResumeError::corrupt)?;
            let mut state = resume.into_state();
            state.insert(
                STATIC_AFTER_CHECKPOINTS_STATE_KEY.to_owned(),
                json!({thread_id: payload.checkpoint_id}),
            );
            return Ok(PipelineResume::from_text_state(state));
        }
        if user_input.trim().is_empty() || user_input.len() > 8 * 1024 || user_input.contains('\0')
        {
            return Err(PipelineResumeError::invalid());
        }
        if !latest
            .provider_metadata
            .contains_key(adk_rust::graph::interrupt::INTERRUPT_METADATA_KEY)
            && !latest
                .provider_metadata
                .contains_key(STATIC_PAUSE_METADATA_KEY)
        {
            return Err(PipelineResumeError::stale());
        }
        let binding = pipeline_static_event_binding(latest, root_agent_name, thread_id)
            .map_err(|_| PipelineResumeError::corrupt())?;
        let expected = self
            .expected_pause_id
            .as_deref()
            .ok_or_else(PipelineResumeError::stale)?;
        {
            let proof = binding
                .public_proof(latest)
                .map_err(|_| PipelineResumeError::corrupt())?;
            if proof.get("pause_id").and_then(serde_json::Value::as_str) != Some(expected) {
                return Err(PipelineResumeError::stale());
            }
        }
        let leaf = validate_static_frontier(checkpointer, thread_id, family, &binding).await?;
        let mut state: State = [
            ("input".to_owned(), json!(user_input)),
            (
                "messages".to_owned(),
                json!([{"role":"user", "content":user_input}]),
            ),
        ]
        .into_iter()
        .collect();
        if binding.metadata.kind == "after" {
            state.insert(
                STATIC_AFTER_CHECKPOINTS_STATE_KEY.to_owned(),
                json!(BTreeMap::from([(
                    leaf.thread_id.clone(),
                    leaf.checkpoint_id.clone()
                )])),
            );
        }
        if !binding.nested_checkpoints.is_empty() {
            state.insert(
                STATIC_TEXT_RESUME_STATE_KEY.to_owned(),
                json!(BTreeMap::from([(leaf.thread_id.clone(), user_input)])),
            );
        }
        Ok(PipelineResume::from_text_state(state))
    }
}

async fn validate_static_frontier(
    checkpointer: &dyn Checkpointer,
    thread_id: &str,
    family: &StaticPauseFamily,
    binding: &PipelineStaticEventBinding,
) -> Result<Checkpoint, PipelineResumeError> {
    let checkpoint = checkpointer
        .load(thread_id)
        .await
        .map_err(|_| PipelineResumeError::dependency())?
        .ok_or_else(PipelineResumeError::stale)?;
    if checkpoint.thread_id != thread_id || checkpoint.checkpoint_id != binding.checkpoint_id {
        return Err(PipelineResumeError::stale());
    }
    let mut leaf = checkpoint;
    for nested in &binding.nested_checkpoints {
        if leaf.pending_nodes.as_slice() != [nested.node_name()] {
            return Err(PipelineResumeError::stale());
        }
        leaf = checkpointer
            .load(nested.thread_id())
            .await
            .map_err(|_| PipelineResumeError::dependency())?
            .ok_or_else(PipelineResumeError::stale)?;
        if leaf.thread_id != nested.thread_id() || leaf.checkpoint_id != nested.checkpoint_id() {
            return Err(PipelineResumeError::stale());
        }
    }
    let catalog = family
        .catalog(thread_id, &leaf.thread_id)
        .ok_or_else(PipelineResumeError::stale)?;
    if !catalog.contains_exact(&binding.metadata) || !binding.metadata.checkpoint_matches(&leaf) {
        return Err(PipelineResumeError::stale());
    }
    Ok(leaf)
}

/// Rearm before gates only after the validated node completion checkpoint.
/// ADK retains its before marker when the same execution also raises an after gate.
pub(super) struct StaticResumeCheckpointer {
    inner: Arc<dyn Checkpointer>,
    after_checkpoints: BTreeMap<String, String>,
}

impl StaticResumeCheckpointer {
    pub(super) const fn new(
        inner: Arc<dyn Checkpointer>,
        after_checkpoints: BTreeMap<String, String>,
    ) -> Self {
        Self {
            inner,
            after_checkpoints,
        }
    }
    fn rearm(&self, checkpoint: Option<Checkpoint>) -> Result<Option<Checkpoint>, GraphError> {
        checkpoint
            .map(|mut checkpoint| {
                if self.after_checkpoints.get(&checkpoint.thread_id)
                    == Some(&checkpoint.checkpoint_id)
                {
                    checkpoint.cleared_interrupt = None;
                    checkpoint.step = checkpoint.step.checked_add(1).ok_or_else(|| {
                        GraphError::InvalidGraph("Static pause step is invalid".to_owned())
                    })?;
                }
                Ok(checkpoint)
            })
            .transpose()
    }
}

#[async_trait]
impl Checkpointer for StaticResumeCheckpointer {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        self.inner.save(checkpoint).await
    }
    async fn load(&self, thread_id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.rearm(self.inner.load(thread_id).await?)
    }
    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.rearm(self.inner.load_by_id(id).await?)
    }
    async fn list(&self, thread_id: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.inner.list(thread_id).await
    }
    async fn delete(&self, thread_id: &str) -> Result<(), GraphError> {
        self.inner.delete(thread_id).await
    }
    async fn prune(&self, thread_id: &str, policy: &RetentionPolicy) -> Result<usize, GraphError> {
        self.inner.prune(thread_id, policy).await
    }
}

pub(super) fn valid_digest(value: &str) -> bool {
    value.strip_prefix("sha256:").is_some_and(|digest| {
        digest.len() == 64
            && digest
                .bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    })
}

fn sha256_label(value: &[u8; 32]) -> String {
    use std::fmt::Write as _;
    let mut result = String::from("sha256:");
    for byte in value {
        let _ = write!(result, "{byte:02x}");
    }
    result
}
