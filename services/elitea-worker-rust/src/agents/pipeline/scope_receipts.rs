//! Original synthetic graph calls live in the graph owner's existing checkpoint.

use std::collections::BTreeMap;
use std::fmt::Write as _;
use std::sync::Arc;

use adk_rust::graph::checkpoint::RetentionPolicy;
use adk_rust::graph::{Checkpoint, Checkpointer, GraphError, NodeContext};
use adk_rust::{Content, Event, Part};
use async_trait::async_trait;
use ring::digest;
use serde::{Deserialize, Serialize};
use serde_json::Value;
#[cfg(test)]
use serde_json::json;
use tokio::sync::Mutex;

use super::scoped_applications::PipelineApplicationActivation;

#[path = "scope_receipts_parallel.rs"]
mod parallel_authority;
pub(crate) use parallel_authority::PipelineGraphReceiptAuthority;
#[path = "scope_receipts_map.rs"]
mod map_authority;
pub(crate) use map_authority::PipelineMapReceiptAuthority;

pub(crate) const GRAPH_CALL_RECEIPTS_METADATA_KEY: &str = "elitea.pipeline.graph-calls.v1";
pub(crate) const GRAPH_CALL_REVISION_METADATA_KEY: &str = "elitea.pipeline.graph-call-revision.v1";
const REVISION_SCHEMA: &str = "elitea.pipeline.graph-call-revision.v1";
const RECEIPTS_SCHEMA: &str = "elitea.pipeline.graph-calls.v1";
const MAX_RECEIPT_BYTES: usize = 256 * 1024;
const MAX_RECEIPTS: usize = 128;

#[derive(Clone, Deserialize, Serialize)]
#[serde(tag = "phase", deny_unknown_fields)]
#[allow(clippy::large_enum_variant)] // Durable outcomes are bounded by the receipt codec, with at most one terminal event.
pub(crate) enum GraphCallOutcome {
    Started,
    Paused { result: Value },
    Completed { result: Value, terminal: Event },
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct PipelineGraphCallReceipt {
    schema: String,
    activation: PipelineApplicationActivation,
    arguments_digest: String,
    original_start: Event,
    outcome: GraphCallOutcome,
}

impl PipelineGraphCallReceipt {
    pub(crate) fn original_start(&self) -> &Event {
        &self.original_start
    }
    pub(crate) fn activation(&self) -> &PipelineApplicationActivation {
        &self.activation
    }
    pub(crate) fn branch(&self) -> &str {
        &self.original_start.branch
    }
    pub(crate) fn invocation_id(&self) -> &str {
        &self.original_start.invocation_id
    }
    pub(crate) fn author(&self) -> &str {
        &self.original_start.author
    }

    pub(crate) fn outcome(&self) -> &GraphCallOutcome {
        &self.outcome
    }

    fn validate(&self) -> Result<(), GraphError> {
        let calls = self.original_start.tool_calls();
        if self.schema != RECEIPTS_SCHEMA
            || calls.len() != 1
            || calls[0].call_id != Some(self.activation.call_id())
            || self.arguments_digest != arguments_digest(calls[0].args)?
            || !self
                .activation
                .matches_original_start(&self.original_start)
                .map_err(|_| receipt_error())?
        {
            return Err(receipt_error());
        }
        match &self.outcome {
            GraphCallOutcome::Started => {},
            GraphCallOutcome::Paused { result } => {
                if crate::agents::application_tools::nested_application_interrupt_ids(result).is_none() { return Err(receipt_error()); }
            },
            GraphCallOutcome::Completed { result, terminal } => {
                if crate::agents::application_tools::nested_application_interrupt_ids(result).is_some()
                    || terminal.invocation_id != self.original_start.invocation_id
                    || terminal.author != self.original_start.author
                    || terminal.branch != self.original_start.branch
                    || !self.activation.matches_event(terminal).map_err(|_| receipt_error())?
                    || terminal.content().is_none_or(|content| !content.parts.iter().any(|part|
                        matches!(part, Part::FunctionResponse {id:Some(id),function_response,..}
                            if id == self.activation.call_id() && function_response.response == *result && function_response.name==calls[0].name))) {
                    return Err(receipt_error());
                }
            },
        }
        Ok(())
    }
}

#[derive(Default, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct GraphCallReceipts {
    schema: String,
    calls: BTreeMap<String, PipelineGraphCallReceipt>,
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct GraphCallRevision {
    schema: String,
    parent_checkpoint_id: String,
}

/// A receipt append is conditional on the latest immutable graph frontier.
/// This internal marker grants no thread, writer, or continuation authority.
pub(crate) fn receipt_revision_parent(
    checkpoint: &Checkpoint,
) -> Result<Option<String>, GraphError> {
    let Some(raw) = checkpoint.metadata.get(GRAPH_CALL_REVISION_METADATA_KEY) else {
        return Ok(None);
    };
    let revision: GraphCallRevision =
        serde_json::from_value(raw.clone()).map_err(|_| receipt_error())?;
    if revision.schema != REVISION_SCHEMA
        || revision.parent_checkpoint_id.is_empty()
        || revision.parent_checkpoint_id.len() > 1024
        || revision.parent_checkpoint_id.chars().any(char::is_control)
        || revision.parent_checkpoint_id == checkpoint.checkpoint_id
    {
        return Err(receipt_error());
    }
    Ok(Some(revision.parent_checkpoint_id))
}

fn graph_frontier(checkpoint: &Checkpoint) -> Result<Value, GraphError> {
    let mut core = checkpoint.clone();
    core.checkpoint_id.clear();
    // Revision ID/time describe the append, not a change to graph execution state.
    core.created_at = chrono::DateTime::UNIX_EPOCH;
    core.metadata.remove(GRAPH_CALL_RECEIPTS_METADATA_KEY);
    core.metadata.remove(GRAPH_CALL_REVISION_METADATA_KEY);
    serde_json::to_value(core).map_err(|_| receipt_error())
}

/// Used both by the local decorator and atomically inside the fenced PG transaction.
pub(crate) fn validate_graph_call_revision(
    parent: &Checkpoint,
    candidate: &Checkpoint,
) -> Result<(), GraphError> {
    if receipt_revision_parent(candidate)?.as_deref() != Some(parent.checkpoint_id.as_str())
        || graph_frontier(parent)? != graph_frontier(candidate)?
    {
        return Err(receipt_error());
    }
    let previous = read_receipts(parent)?;
    let next = read_receipts(candidate)?;
    let mut changed = 0;
    for (node, old) in &previous.calls {
        let new = next.calls.get(node).ok_or_else(receipt_error)?;
        if serde_json::to_value(old).map_err(|_| receipt_error())?
            == serde_json::to_value(new).map_err(|_| receipt_error())?
        {
            continue;
        }
        changed += 1;
        let allowed = if same_call(old, new)? {
            matches!(
                (&old.outcome, &new.outcome),
                (
                    GraphCallOutcome::Started,
                    GraphCallOutcome::Paused { .. } | GraphCallOutcome::Completed { .. }
                ) | (
                    GraphCallOutcome::Paused { .. },
                    GraphCallOutcome::Paused { .. } | GraphCallOutcome::Completed { .. }
                )
            )
        } else {
            old.activation.step() < new.activation.step()
                && matches!(old.outcome, GraphCallOutcome::Completed { .. })
                && matches!(new.outcome, GraphCallOutcome::Started)
        };
        if !allowed
            || new.activation.step() != candidate.step
            || !candidate.pending_nodes.contains(node)
        {
            return Err(receipt_error());
        }
    }
    for (node, new) in &next.calls {
        if !previous.calls.contains_key(node) {
            changed += 1;
            if !matches!(new.outcome, GraphCallOutcome::Started)
                || new.activation.step() != candidate.step
                || !candidate.pending_nodes.contains(node)
            {
                return Err(receipt_error());
            }
        }
    }
    if changed != 1 {
        return Err(receipt_error());
    }
    Ok(())
}

fn receipt_revision(parent: &Checkpoint, raw: Value) -> Result<Checkpoint, GraphError> {
    let identity = Checkpoint::new(
        &parent.thread_id,
        parent.state.clone(),
        parent.step,
        parent.pending_nodes.clone(),
    );
    let mut candidate = parent.clone();
    candidate.checkpoint_id = identity.checkpoint_id;
    candidate.created_at = identity.created_at;
    candidate
        .metadata
        .insert(GRAPH_CALL_RECEIPTS_METADATA_KEY.to_owned(), raw);
    candidate.metadata.insert(
        GRAPH_CALL_REVISION_METADATA_KEY.to_owned(),
        serde_json::to_value(GraphCallRevision {
            schema: REVISION_SCHEMA.to_owned(),
            parent_checkpoint_id: parent.checkpoint_id.clone(),
        })
        .map_err(|_| receipt_error())?,
    );
    validate_graph_call_revision(parent, &candidate)?;
    Ok(candidate)
}

fn read_receipts(checkpoint: &Checkpoint) -> Result<GraphCallReceipts, GraphError> {
    let Some(raw) = checkpoint.metadata.get(GRAPH_CALL_RECEIPTS_METADATA_KEY) else {
        return Ok(GraphCallReceipts {
            schema: RECEIPTS_SCHEMA.to_owned(),
            calls: BTreeMap::new(),
        });
    };
    if serde_json::to_vec(raw).map_err(|_| receipt_error())?.len() > MAX_RECEIPT_BYTES {
        return Err(receipt_error());
    }
    let value: GraphCallReceipts =
        serde_json::from_value(raw.clone()).map_err(|_| receipt_error())?;
    if value.schema != RECEIPTS_SCHEMA || value.calls.len() > MAX_RECEIPTS {
        return Err(receipt_error());
    }
    for (node, receipt) in &value.calls {
        receipt.validate()?;
        if node != receipt.activation.node_name()
            || receipt.activation.thread_id() != checkpoint.thread_id
            || receipt.activation.step() > checkpoint.step
        {
            return Err(receipt_error());
        }
    }
    Ok(value)
}

pub(crate) fn receipt_for_node(
    checkpoint: &Checkpoint,
    node: &str,
) -> Result<Option<PipelineGraphCallReceipt>, GraphError> {
    Ok(read_receipts(checkpoint)?.calls.remove(node))
}

/// A node occurrence prepares its original call before polling the child provider/tool.
/// The compiler already persists the initial root frontier; native subgraph entry also
/// checkpoints before its saved entry node. No frontier or graph state is invented here.
#[allow(clippy::too_many_arguments)] // Keep distinct checkpoint owner, activation, and original event identities explicit.
pub(crate) async fn prepare_graph_call(
    checkpointer: &dyn Checkpointer,
    gate: &Mutex<()>,
    context: &NodeContext,
    activation: PipelineApplicationActivation,
    tool_name: &str,
    arguments: &Value,
    invocation_id: &str,
    author: &str,
    branch: &str,
) -> Result<(PipelineGraphCallReceipt, bool), GraphError> {
    let _guard = gate.lock().await;
    let checkpoint = checkpointer
        .load(&context.config.thread_id)
        .await?
        .ok_or_else(receipt_error)?;
    if checkpoint.thread_id != context.config.thread_id
        || checkpoint.step != context.step
        || !checkpoint
            .pending_nodes
            .iter()
            .any(|node| node == activation.node_name())
        || activation.thread_id() != context.config.thread_id
    {
        return Err(receipt_error());
    }
    let mut receipts = read_receipts(&checkpoint)?;
    let expected_digest = arguments_digest(arguments)?;
    if let Some(recorded) = receipts.calls.get(activation.node_name()) {
        if recorded.activation == activation {
            if recorded.arguments_digest != expected_digest
                || recorded.original_start.tool_calls()[0].name != tool_name
            {
                return Err(receipt_error());
            }
            return Ok((recorded.clone(), false));
        }
        if recorded.activation.step() >= activation.step() {
            return Err(receipt_error());
        }
    }
    let mut original_start = Event::new(invocation_id);
    original_start.author = author.to_owned();
    original_start.branch = branch.to_owned();
    original_start.llm_response.content = Some(Content {
        role: "model".to_owned(),
        parts: vec![Part::FunctionCall {
            name: tool_name.to_owned(),
            args: arguments.clone(),
            id: Some(activation.call_id().to_owned()),
            thought_signature: None,
        }],
    });
    activation
        .stamp_with_branch(&mut original_start, branch.to_owned())
        .map_err(|_| receipt_error())?;
    let receipt = PipelineGraphCallReceipt {
        schema: RECEIPTS_SCHEMA.to_owned(),
        activation,
        arguments_digest: expected_digest,
        original_start,
        outcome: GraphCallOutcome::Started,
    };
    receipt.validate()?;
    receipts
        .calls
        .insert(receipt.activation.node_name().to_owned(), receipt.clone());
    let raw = serde_json::to_value(receipts).map_err(|_| receipt_error())?;
    if serde_json::to_vec(&raw).map_err(|_| receipt_error())?.len() > MAX_RECEIPT_BYTES {
        return Err(receipt_error());
    }
    // Append a conditional immutable revision; never rewrite the admitted frontier ID.
    checkpointer
        .save(&receipt_revision(&checkpoint, raw)?)
        .await?;
    Ok((receipt, true))
}

/// Persist the child result before the graph advances its frontier. A started call
/// recovered without an exact installed continuation refuses rather than reruns effects.
pub(crate) async fn finish_graph_call(
    checkpointer: &dyn Checkpointer,
    gate: &Mutex<()>,
    context: &NodeContext,
    original: &PipelineGraphCallReceipt,
    outcome: GraphCallOutcome,
) -> Result<PipelineGraphCallReceipt, GraphError> {
    let _guard = gate.lock().await;
    let checkpoint = checkpointer
        .load(&context.config.thread_id)
        .await?
        .ok_or_else(receipt_error)?;
    if checkpoint.step != context.step
        || !checkpoint
            .pending_nodes
            .iter()
            .any(|node| node == original.activation.node_name())
    {
        return Err(receipt_error());
    }
    let mut receipts = read_receipts(&checkpoint)?;
    let current = receipts
        .calls
        .get(original.activation.node_name())
        .ok_or_else(receipt_error)?;
    if serde_json::to_value(current).map_err(|_| receipt_error())?
        != serde_json::to_value(original).map_err(|_| receipt_error())?
    {
        return Err(receipt_error());
    }
    let mut updated = original.clone();
    updated.outcome = outcome;
    updated.validate()?;
    if serde_json::to_value(&updated).map_err(|_| receipt_error())?
        == serde_json::to_value(original).map_err(|_| receipt_error())?
    {
        return Ok(updated);
    }
    receipts
        .calls
        .insert(updated.activation.node_name().to_owned(), updated.clone());
    let raw = serde_json::to_value(receipts).map_err(|_| receipt_error())?;
    if serde_json::to_vec(&raw).map_err(|_| receipt_error())?.len() > MAX_RECEIPT_BYTES {
        return Err(receipt_error());
    }
    // Append a conditional immutable revision; never rewrite the admitted frontier ID.
    checkpointer
        .save(&receipt_revision(&checkpoint, raw)?)
        .await?;
    Ok(updated)
}

fn same_call(
    left: &PipelineGraphCallReceipt,
    right: &PipelineGraphCallReceipt,
) -> Result<bool, GraphError> {
    Ok(left.activation == right.activation
        && left.arguments_digest == right.arguments_digest
        && serde_json::to_value(&left.original_start).map_err(|_| receipt_error())?
            == serde_json::to_value(&right.original_start).map_err(|_| receipt_error())?)
}

/// ADK creates a new checkpoint at each frontier and does not copy metadata.
/// Preserve this exact thread's receipt map; the inner adapter remains the writer authority.
pub(crate) struct PipelineGraphReceiptCheckpointer {
    inner: Arc<dyn Checkpointer>,
    gate: Arc<Mutex<()>>,
}
impl PipelineGraphReceiptCheckpointer {
    pub(crate) fn new(inner: Arc<dyn Checkpointer>, gate: Arc<Mutex<()>>) -> Self {
        Self { inner, gate }
    }
}
#[async_trait]
impl Checkpointer for PipelineGraphReceiptCheckpointer {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        let _guard = self.gate.lock().await;
        // Exact old-ID replays remain immutable and do not absorb newer receipt state.
        if let Some(existing) = self.inner.load_by_id(&checkpoint.checkpoint_id).await? {
            if serde_json::to_value(&existing).map_err(|_| receipt_error())?
                != serde_json::to_value(checkpoint).map_err(|_| receipt_error())?
            {
                return Err(receipt_error());
            }
            return self.inner.save(checkpoint).await;
        }
        let mut checkpoint = checkpoint.clone();
        let revision = receipt_revision_parent(&checkpoint)?;
        if let Some(latest) = self.inner.load(&checkpoint.thread_id).await? {
            if revision.is_some() {
                validate_graph_call_revision(&latest, &checkpoint)?;
                return self.inner.save(&checkpoint).await;
            }
            let latest_receipts = read_receipts(&latest)?;
            let mut supplied = read_receipts(&checkpoint)?;
            for (node, receipt) in latest_receipts.calls {
                match supplied.calls.get(&node) {
                    Some(current) if current.activation.step() < receipt.activation.step() => {
                        return Err(receipt_error());
                    }
                    Some(current) if current.activation.step() == receipt.activation.step() => {
                        if !same_call(current, &receipt)? {
                            return Err(receipt_error());
                        }
                        // The explicit writer may advance Started/Paused to a proved result.
                        // ADK may omit metadata; it cannot replace a completed receipt.
                        if matches!(receipt.outcome, GraphCallOutcome::Paused { .. })
                            && matches!(current.outcome, GraphCallOutcome::Started)
                        {
                            return Err(receipt_error());
                        }
                        if matches!(receipt.outcome, GraphCallOutcome::Completed { .. })
                            && serde_json::to_value(current).map_err(|_| receipt_error())?
                                != serde_json::to_value(&receipt).map_err(|_| receipt_error())?
                        {
                            return Err(receipt_error());
                        }
                    }
                    Some(_) => {}
                    None => {
                        supplied.calls.insert(node, receipt);
                    }
                }
            }
            if !supplied.calls.is_empty() {
                let raw = serde_json::to_value(supplied).map_err(|_| receipt_error())?;
                if serde_json::to_vec(&raw).map_err(|_| receipt_error())?.len() > MAX_RECEIPT_BYTES
                {
                    return Err(receipt_error());
                }
                checkpoint
                    .metadata
                    .insert(GRAPH_CALL_RECEIPTS_METADATA_KEY.to_owned(), raw);
            }
        }
        if revision.is_some() {
            return Err(receipt_error());
        }
        self.inner.save(&checkpoint).await
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.inner.load(thread).await
    }
    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.inner.load_by_id(id).await
    }
    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.inner.list(thread).await
    }
    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.inner.delete(thread).await
    }
    async fn prune(&self, thread: &str, policy: &RetentionPolicy) -> Result<usize, GraphError> {
        self.inner.prune(thread, policy).await
    }
}

pub(crate) fn has_graph_call_receipts(checkpoint: &Checkpoint) -> Result<bool, GraphError> {
    Ok(!read_receipts(checkpoint)?.calls.is_empty())
}

pub(crate) fn arguments_digest(value: &Value) -> Result<String, GraphError> {
    fn canonical(value: &Value) -> Value {
        match value {
            Value::Object(object) => Value::Object(
                object
                    .iter()
                    .map(|(key, value)| (key.clone(), canonical(value)))
                    .collect::<BTreeMap<_, _>>()
                    .into_iter()
                    .collect(),
            ),
            Value::Array(array) => Value::Array(array.iter().map(canonical).collect()),
            value => value.clone(),
        }
    }
    let raw = serde_json::to_vec(&canonical(value)).map_err(|_| receipt_error())?;
    let mut value = String::with_capacity(64);
    for byte in digest::digest(&digest::SHA256, &raw).as_ref() {
        let _ = write!(value, "{byte:02x}");
    }
    Ok(value)
}
fn receipt_error() -> GraphError {
    GraphError::NodeExecutionFailed {
        node: "application_receipt".to_owned(),
        message: "the original graph application call does not match its owned checkpoint frontier"
            .to_owned(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::agents::events::APPLICATION_BRANCH_ROOT;
    use crate::agents::graph::compiler::PipelineDefinition;
    use crate::agents::pipeline::scoped_applications::PipelineApplicationScope;
    use adk_rust::graph::{
        ExecutionConfig, MemoryCheckpointer as UncheckedMemoryCheckpointer, State,
    };

    // Mirrors PG's immutable IDs and old-ID idempotency, unlike upstream MemoryCheckpointer.
    #[derive(Default)]
    struct MemoryCheckpointer(UncheckedMemoryCheckpointer);
    impl MemoryCheckpointer {
        fn new() -> Self {
            Self::default()
        }
    }
    #[async_trait]
    impl Checkpointer for MemoryCheckpointer {
        async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
            if let Some(old) = self.0.load_by_id(&checkpoint.checkpoint_id).await? {
                if serde_json::to_value(old).unwrap() != serde_json::to_value(checkpoint).unwrap() {
                    return Err(receipt_error());
                }
                return Ok(checkpoint.checkpoint_id.clone());
            }
            self.0.save(checkpoint).await
        }
        async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
            self.0.load(thread).await
        }
        async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
            self.0.load_by_id(id).await
        }
        async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
            self.0.list(thread).await
        }
        async fn delete(&self, thread: &str) -> Result<(), GraphError> {
            self.0.delete(thread).await
        }
        async fn prune(&self, thread: &str, policy: &RetentionPolicy) -> Result<usize, GraphError> {
            self.0.prune(thread, policy).await
        }
    }
    fn scope() -> PipelineApplicationScope {
        let definition=PipelineDefinition::from_yaml("state: {answer: str}\nentry_point: child\nnodes:\n  - id: child\n    type: agent\n    tool: assistant\n    input_mapping: {task: {type: fixed, value: work}}\n    output: [answer]\n    transition: END\n").unwrap();
        PipelineApplicationScope::new(String::new(), &definition, None).unwrap()
    }
    fn context(step: usize) -> NodeContext {
        NodeContext::new(State::new(), ExecutionConfig::new("owned-root"), step)
    }
    async fn prepare(
        checkpointer: &dyn Checkpointer,
        step: usize,
        args: &Value,
    ) -> Result<(PipelineGraphCallReceipt, bool), GraphError> {
        prepare_graph_call(
            checkpointer,
            &Mutex::new(()),
            &context(step),
            scope().activate("owned-root", "child", step).unwrap(),
            "assistant",
            args,
            "original-runtime",
            "graph",
            APPLICATION_BRANCH_ROOT,
        )
        .await
    }

    #[tokio::test]
    async fn receipt_revision_refuses_changed_graph_core_stale_parent_and_malformed_marker() {
        let checkpointer = MemoryCheckpointer::new();
        let root = Checkpoint::new("owned-root", State::new(), 0, vec!["child".to_owned()]);
        checkpointer.save(&root).await.unwrap();
        prepare(&checkpointer, 0, &json!({"task":"work"}))
            .await
            .unwrap();
        let begun = checkpointer.load("owned-root").await.unwrap().unwrap();
        validate_graph_call_revision(&root, &begun).unwrap();
        assert!(validate_graph_call_revision(&begun, &begun).is_err());
        for field in [
            "state", "frontier", "step", "attempts", "ledger", "cleared", "metadata", "marker",
        ] {
            let mut candidate = begun.clone();
            match field {
                "state" => {
                    candidate.state.insert("changed".to_owned(), json!(true));
                }
                "frontier" => candidate.pending_nodes.clear(),
                "step" => candidate.step += 1,
                "attempts" => {
                    candidate.attempts.insert("child".to_owned(), 2);
                }
                "ledger" => {
                    candidate
                        .child_ledger
                        .insert("child".to_owned(), json!({"completed":true}));
                }
                "cleared" => candidate.cleared_interrupt = Some("forged".to_owned()),
                "metadata" => {
                    candidate
                        .metadata
                        .insert("unrelated".to_owned(), json!(true));
                }
                "marker" => {
                    candidate
                        .metadata
                        .get_mut(GRAPH_CALL_REVISION_METADATA_KEY)
                        .unwrap()["extra"] = json!(true);
                }
                _ => unreachable!(),
            }
            assert!(
                validate_graph_call_revision(&root, &candidate).is_err(),
                "{field}"
            );
        }
    }

    #[tokio::test]
    async fn original_call_is_written_before_polling_and_bound_to_exact_frontier() {
        let checkpointer = MemoryCheckpointer::new();
        let args = json!({"task":"work"});
        assert!(prepare(&checkpointer, 0, &args).await.is_err());
        let root = Checkpoint::new("owned-root", State::new(), 0, vec!["child".to_owned()]);
        checkpointer.save(&root).await.unwrap();
        let (first, fresh) = prepare(&checkpointer, 0, &args).await.unwrap();
        assert!(fresh);
        let stored = checkpointer.load("owned-root").await.unwrap().unwrap();
        assert_ne!(stored.checkpoint_id, root.checkpoint_id);
        assert_eq!(
            serde_json::to_value(
                checkpointer
                    .load_by_id(&root.checkpoint_id)
                    .await
                    .unwrap()
                    .unwrap()
            )
            .unwrap(),
            serde_json::to_value(&root).unwrap()
        );
        assert_eq!(stored.pending_nodes, root.pending_nodes);
        assert_eq!(
            receipt_for_node(&stored, "child")
                .unwrap()
                .unwrap()
                .original_start()
                .id,
            first.original_start().id
        );
        let (again, fresh) = prepare(&checkpointer, 0, &args).await.unwrap();
        assert!(!fresh);
        assert_eq!(again.original_start().id, first.original_start().id);
        assert!(
            prepare(&checkpointer, 0, &json!({"task":"edited"}))
                .await
                .is_err()
        );
        assert!(prepare(&checkpointer, 1, &args).await.is_err());
        let mut wrong = Checkpoint::new("owned-root", stored.state, stored.step, vec![]);
        wrong.pending_nodes = vec!["neighbor".to_owned()];
        checkpointer.save(&wrong).await.unwrap();
        assert!(prepare(&checkpointer, 0, &args).await.is_err());
    }

    #[tokio::test]
    async fn completed_receipt_survives_adk_metadata_omission_and_repeated_activation_is_fresh() {
        let inner: Arc<dyn Checkpointer> = Arc::new(MemoryCheckpointer::new());
        let checkpointer = PipelineGraphReceiptCheckpointer::new(inner, Arc::new(Mutex::new(())));
        checkpointer
            .save(&Checkpoint::new(
                "owned-root",
                State::new(),
                0,
                vec!["child".to_owned()],
            ))
            .await
            .unwrap();
        let args = json!({"task":"work"});
        let (first, _) = prepare(&checkpointer, 0, &args).await.unwrap();
        let result = json!({"response":"done once"});
        let mut terminal = Event::new(first.invocation_id());
        terminal.author = first.author().to_owned();
        terminal.branch = first.branch().to_owned();
        terminal.llm_response.content = Some(Content {
            role: "function".to_owned(),
            parts: vec![Part::FunctionResponse {
                function_response: adk_rust::FunctionResponseData::new("assistant", result.clone()),
                id: Some(first.activation().call_id().to_owned()),
                annotations: None,
            }],
        });
        first
            .activation()
            .stamp_with_branch(&mut terminal, first.branch().to_owned())
            .unwrap();
        let finished = finish_graph_call(
            &checkpointer,
            &Mutex::new(()),
            &context(0),
            &first,
            GraphCallOutcome::Completed { result, terminal },
        )
        .await
        .unwrap();
        assert!(matches!(
            finished.outcome(),
            GraphCallOutcome::Completed { .. }
        ));
        let next = Checkpoint::new("owned-root", State::new(), 1, vec!["child".to_owned()]);
        checkpointer.save(&next).await.unwrap();
        let persisted = checkpointer.load("owned-root").await.unwrap().unwrap();
        assert!(matches!(
            receipt_for_node(&persisted, "child")
                .unwrap()
                .unwrap()
                .outcome(),
            GraphCallOutcome::Completed { .. }
        ));
        let (second, fresh) = prepare(&checkpointer, 1, &args).await.unwrap();
        assert!(fresh);
        assert_ne!(first.activation().call_id(), second.activation().call_id());
        assert_ne!(first.original_start().id, second.original_start().id);
        assert!(
            finish_graph_call(
                &checkpointer,
                &Mutex::new(()),
                &context(1),
                &first,
                GraphCallOutcome::Started
            )
            .await
            .is_err()
        );
    }

    #[tokio::test]
    async fn paused_receipt_cannot_be_downgraded_or_changed_by_same_step_checkpoint() {
        let inner: Arc<dyn Checkpointer> = Arc::new(MemoryCheckpointer::new());
        let checkpointer = PipelineGraphReceiptCheckpointer::new(inner, Arc::new(Mutex::new(())));
        checkpointer
            .save(&Checkpoint::new(
                "owned-root",
                State::new(),
                0,
                vec!["child".to_owned()],
            ))
            .await
            .unwrap();
        let (first, _) = prepare(&checkpointer, 0, &json!({"task":"work"}))
            .await
            .unwrap();
        let old = checkpointer.load("owned-root").await.unwrap().unwrap();
        let ids = std::collections::BTreeSet::from(["leaf-a".to_owned(), "leaf-b".to_owned()]);
        finish_graph_call(
            &checkpointer,
            &Mutex::new(()),
            &context(0),
            &first,
            GraphCallOutcome::Paused {
                result: crate::agents::application_tools::nested_interrupt_result(&ids),
            },
        )
        .await
        .unwrap();
        let paused = checkpointer.load("owned-root").await.unwrap().unwrap();
        assert!(checkpointer.save(&old).await.is_ok());
        let current = checkpointer.load("owned-root").await.unwrap().unwrap();
        assert_eq!(current.checkpoint_id, paused.checkpoint_id);
        let receipt = receipt_for_node(&current, "child").unwrap().unwrap();
        assert!(matches!(receipt.outcome(), GraphCallOutcome::Paused { .. }));
        let mut changed = old;
        changed.metadata.insert("edited".to_owned(), json!(true));
        assert!(checkpointer.save(&changed).await.is_err());
    }
}
