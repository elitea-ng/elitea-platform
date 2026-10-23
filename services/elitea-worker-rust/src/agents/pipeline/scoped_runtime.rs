//! One saved Agent node occurrence owns its child receiver, coordinator and call receipt.

use super::scope_receipts::{
    GraphCallOutcome, PipelineGraphCallReceipt, finish_graph_call, prepare_graph_call,
};
use super::scoped_applications::PipelineApplicationScope;
use crate::agents::application_tools::{ApplicationEventSignal, application_signal_event};
use crate::agents::driven::DrivenTask;
use crate::agents::events::{
    APPLICATION_BRANCH_ROOT, DESCENDANT_CHECKPOINT_THREAD_KEY, DESCENDANT_CONTAINER_INVOCATION_KEY,
    DESCENDANT_PARENT_CALL_KEY,
};
use crate::agents::graph::{
    PIPELINE_NODE_EVENT_SCOPE_STATE_KEY, PipelineNodeEventScope, PipelineNodeEventSender,
    scoped_pipeline_tool_context,
};
use crate::agents::session::ApplicationRuntimeProjection;
use adk_rust::graph::{Checkpointer, GraphError, NodeContext};
use adk_rust::{Event, Part, Tool};
use serde_json::Value;
use std::future::Future;
use std::sync::Arc;
use tokio::sync::{Mutex, mpsc};

pub(crate) struct PipelineGraphCallOwner {
    pub(crate) scope: PipelineApplicationScope,
    pub(crate) node_name: String,
    pub(crate) prepare_gate: Arc<Mutex<()>>,
    pub(crate) parallel_events: Option<PipelineNodeEventSender>,
}
impl PipelineGraphCallOwner {
    pub(crate) fn with_parallel_events(&self, events: PipelineNodeEventSender) -> Self {
        Self {
            scope: self.scope.clone(),
            node_name: self.node_name.clone(),
            prepare_gate: Arc::clone(&self.prepare_gate),
            parallel_events: Some(events),
        }
    }

    pub(crate) async fn finish(
        &self,
        context: &NodeContext,
        checkpointer: &dyn Checkpointer,
        original: &PipelineGraphCallReceipt,
        outcome: GraphCallOutcome,
    ) -> Result<PipelineGraphCallReceipt, GraphError> {
        if self
            .scope
            .activate(&context.config.thread_id, &self.node_name, context.step)
            .map_err(|_| failure())?
            != *original.activation()
        {
            return Err(failure());
        }
        finish_graph_call(checkpointer, &self.prepare_gate, context, original, outcome).await
    }

    pub(crate) async fn prepare(
        &self,
        context: &NodeContext,
        checkpointer: &dyn Checkpointer,
        tool_name: &str,
        args: &Value,
    ) -> Result<PipelineGraphCallReceipt, GraphError> {
        let activation = self
            .scope
            .activate(&context.config.thread_id, &self.node_name, context.step)
            .map_err(|_| failure())?;
        let (invocation, author, branch) =
            graph_invocation_identity_with_events(context, self.parallel_events.as_ref())?;
        let (receipt, _) = prepare_graph_call(
            checkpointer,
            &self.prepare_gate,
            context,
            activation,
            tool_name,
            args,
            &invocation,
            &author,
            &branch,
        )
        .await?;
        Ok(receipt)
    }
}

pub(crate) fn graph_invocation_identity(
    context: &NodeContext,
) -> Result<(String, String, String), GraphError> {
    graph_invocation_identity_with_events(context, None)
}

fn graph_invocation_identity_with_events(
    context: &NodeContext,
    events: Option<&PipelineNodeEventSender>,
) -> Result<(String, String, String), GraphError> {
    let parent = context.config.parent_context.as_ref().ok_or_else(failure)?;
    let scope =
        PipelineNodeEventScope::from_state(context.state.get(PIPELINE_NODE_EVENT_SCOPE_STATE_KEY))
            .map_err(|_| failure())?;
    let inherited = events
        .map(|events| events.inherited_event_scope(&context.config.thread_id, scope.as_ref()))
        .transpose()
        .map_err(|_| failure())?
        .flatten();
    let inherited_branch = inherited
        .map(|inherited| graph_branch(parent.branch(), inherited))
        .transpose()?;
    match scope.as_ref().or(inherited) {
        Some(current) => Ok((
            format!("pipeline-child:{}", current.parent_call_id()),
            current.agent_name().to_owned(),
            if let Some(local) = scope.as_ref() {
                graph_branch(
                    inherited_branch.as_deref().unwrap_or(parent.branch()),
                    local,
                )?
            } else {
                inherited_branch.ok_or_else(failure)?
            },
        )),
        None => Ok((
            parent.invocation_id().to_owned(),
            parent.agent_name().to_owned(),
            root_branch(parent.branch())?,
        )),
    }
}

pub(crate) struct PipelineApplicationNodeRuntime {
    pub(crate) scope: PipelineApplicationScope,
    pub(crate) node_name: String,
    pub(crate) applications: Arc<ApplicationRuntimeProjection>,
    pub(crate) events: PipelineNodeEventSender,
    pub(crate) prepare_gate: Arc<Mutex<()>>,
}

impl PipelineApplicationNodeRuntime {
    pub(crate) fn new(
        scope: PipelineApplicationScope,
        node_name: String,
        applications: Arc<ApplicationRuntimeProjection>,
        events: PipelineNodeEventSender,
        prepare_gate: Arc<Mutex<()>>,
    ) -> Self {
        Self {
            scope,
            node_name,
            applications,
            events,
            prepare_gate,
        }
    }

    /// Rebind event transport without changing the saved scope or recovery owner.
    pub(crate) fn with_parallel_events(&self, events: PipelineNodeEventSender) -> Self {
        Self {
            scope: self.scope.clone(),
            node_name: self.node_name.clone(),
            applications: Arc::clone(&self.applications),
            events,
            prepare_gate: Arc::clone(&self.prepare_gate),
        }
    }

    #[allow(clippy::too_many_lines)] // Keep durable activation and child drain/replay ordering together.
    pub(crate) async fn execute(
        &self,
        context: &NodeContext,
        checkpointer: &dyn Checkpointer,
        tool: &Arc<dyn Tool>,
        arguments: Value,
    ) -> Result<(Value, String), GraphError> {
        let activation = self
            .scope
            .activate(&context.config.thread_id, &self.node_name, context.step)
            .map_err(|_| failure())?;
        let parent = context.config.parent_context.as_ref().ok_or_else(failure)?;
        let graph_scope = PipelineNodeEventScope::from_state(
            context.state.get(PIPELINE_NODE_EVENT_SCOPE_STATE_KEY),
        )
        .map_err(|_| failure())?;
        let (invocation, author, branch) =
            graph_invocation_identity_with_events(context, Some(&self.events))?;
        let (mut receipt, fresh) = prepare_graph_call(
            checkpointer,
            &self.prepare_gate,
            context,
            activation,
            tool.name(),
            &arguments,
            &invocation,
            &author,
            &branch,
        )
        .await?;
        match receipt.outcome().clone() {
            GraphCallOutcome::Completed { result, terminal } => {
                self.forward(
                    receipt.original_start().clone(),
                    graph_scope.as_ref(),
                    parent.invocation_id(),
                    receipt.invocation_id(),
                )
                .await?;
                self.forward(
                    terminal.clone(),
                    graph_scope.as_ref(),
                    parent.invocation_id(),
                    receipt.invocation_id(),
                )
                .await?;
                return Ok((result.clone(), receipt.activation().call_id().to_owned()));
            }
            GraphCallOutcome::Paused { result } => {
                let coordinator = self.applications.resume_coordinator().ok_or_else(failure)?;
                if !coordinator
                    .has_resume(receipt.invocation_id(), receipt.activation().call_id())
                    .await
                {
                    return Ok((result, receipt.activation().call_id().to_owned()));
                }
                // An approval may dispatch an effect: record durably that its resume
                // started, so no stale copy of this receipt can drop it.
                if coordinator
                    .approves_sensitive_call(
                        receipt.invocation_id(),
                        receipt.activation().call_id(),
                    )
                    .await
                {
                    receipt = finish_graph_call(
                        checkpointer,
                        &self.prepare_gate,
                        context,
                        &receipt,
                        GraphCallOutcome::Resuming { result },
                    )
                    .await?;
                }
            }
            GraphCallOutcome::Resuming { result } => {
                // An earlier continuation of this approval started the child and
                // stopped. The decision carries the root dispatch journal's facts
                // (FU-226): a recorded outcome is served, a dispatch without one is
                // refused as "may have started", and a call that never started runs
                // once. Without a pending resume (a later turn) the pause result
                // stands.
                let coordinator = self.applications.resume_coordinator().ok_or_else(failure)?;
                if !coordinator
                    .has_resume(receipt.invocation_id(), receipt.activation().call_id())
                    .await
                {
                    return Ok((result, receipt.activation().call_id().to_owned()));
                }
            }
            GraphCallOutcome::Started if !fresh => {
                // An interrupted unpaused provider/tool has no graph-level replay authority.
                // The child may have started an effect: it is never dispatched again.
                return Err(may_have_started());
            }
            GraphCallOutcome::Started => {}
        }
        let tool_context = scoped_pipeline_tool_context(
            context,
            receipt.activation().call_id(),
            tool.name(),
            receipt.invocation_id(),
            receipt.author(),
            receipt.branch(),
        )?;
        let receiver_owner = self.applications.event_receiver().ok_or_else(failure)?;
        let mut receiver = receiver_owner.take().await.map_err(|_| failure())?;
        let tool = Arc::clone(tool);
        let mut terminal = None;
        let result = self
            .run_child_draining(
                async move { tool.execute(tool_context, arguments).await },
                &mut receiver,
                &receipt,
                graph_scope.as_ref(),
                parent.invocation_id(),
                &mut terminal,
            )
            .await;
        receiver_owner
            .restore(receiver)
            .await
            .map_err(|_| failure())?;
        let result = match result {
            Ok(result) => result,
            Err(refusal) => return Err(self.child_failure(refusal).await),
        };
        let outcome = if crate::agents::application_tools::nested_application_interrupt_ids(&result)
            .is_some()
        {
            if terminal.is_some() {
                return Err(failure());
            }
            GraphCallOutcome::Paused {
                result: result.clone(),
            }
        } else {
            GraphCallOutcome::Completed {
                result: result.clone(),
                terminal: terminal.ok_or_else(failure)?,
            }
        };
        let finished =
            finish_graph_call(checkpointer, &self.prepare_gate, context, &receipt, outcome).await?;
        if let GraphCallOutcome::Completed { terminal, .. } = finished.outcome() {
            self.forward(
                terminal.clone(),
                graph_scope.as_ref(),
                parent.invocation_id(),
                receipt.invocation_id(),
            )
            .await?;
        }
        Ok((result, receipt.activation().call_id().to_owned()))
    }

    /// Report a typed child refusal with its registered public reason.
    async fn child_failure(&self, refusal: ChildFailure) -> GraphError {
        let (code, error) = match refusal {
            ChildFailure::Scope(error) => return error,
            // The decided call never ran (FU-226).
            ChildFailure::ReplayUnsupported => (
                REPLAY_UNSUPPORTED_PUBLIC_CODE,
                replay_unsupported(&self.node_name),
            ),
            // The approved call may have run (not repeated), or its dispatch
            // record could not be written (it never ran). The run ends with the
            // registered reason, as an unscoped Agent node does
            // (`graph/application.rs` `child_failure`).
            ChildFailure::ApprovedCallStop(code) => {
                return self
                    .events
                    .approved_call_stop(code, &self.node_name)
                    .await
                    .unwrap_or_else(failure);
            }
            // Another typed refusal of the decided card (stale, invalid or
            // corrupt decision, or an exhausted budget): the node fails with a
            // registered reason, not as a broken scope.
            ChildFailure::Refused(code) => (code, node_refused(&self.node_name)),
            // Not a refusal of the decided card: unchanged scope failure.
            ChildFailure::Child => return failure(),
        };
        match self.events.send_execution_failure(code).await {
            Ok(()) => error,
            Err(_) => failure(),
        }
    }

    /// Run the child tool while forwarding its events, then drain what it
    /// queued. The tool is driven (`DrivenTask`): forwarding a child event is a
    /// bounded send the Runner drains by persisting it, so it must never leave
    /// the child parked mid-append holding the root session writer.
    async fn run_child_draining(
        &self,
        work: impl Future<Output = adk_rust::Result<Value>> + Send + 'static,
        receiver: &mut mpsc::Receiver<ApplicationEventSignal>,
        receipt: &PipelineGraphCallReceipt,
        graph_scope: Option<&PipelineNodeEventScope>,
        root_container: &str,
        terminal: &mut Option<Event>,
    ) -> Result<Value, ChildFailure> {
        let mut future = DrivenTask::new(work);
        let result = loop {
            tokio::select! {
                value = &mut future => break match value {
                    Ok(Ok(value)) => Ok(value),
                    Ok(Err(error)) => Err(ChildFailure::from_child(error.code)),
                    Err(_) => Err(ChildFailure::Scope(failure())),
                },
                signal = receiver.recv() => match signal {
                    Some(signal) => if let Err(error) = self.drain(signal, receipt, graph_scope, root_container, terminal).await { break Err(error); },
                    None => break Err(ChildFailure::Scope(failure())),
                }
            }
        };
        // A drain failure stops the tool task before the queue drain below.
        future.cancel().await;
        // Once tool polling stops no child sender may remain active. Drain its bounded queue.
        let mut result = result;
        while let Ok(signal) = receiver.try_recv() {
            if let Err(error) = self
                .drain(signal, receipt, graph_scope, root_container, terminal)
                .await
            {
                result = Err(error);
                break;
            }
        }
        result
    }

    /// Forward one queued child signal. A child that stopped on an approved
    /// call (FU-226) reports that typed stop on its stream as well as in its
    /// result; any other failed signal breaks the scope.
    async fn drain(
        &self,
        signal: ApplicationEventSignal,
        receipt: &PipelineGraphCallReceipt,
        graph_scope: Option<&PipelineNodeEventScope>,
        root_container: &str,
        terminal: &mut Option<Event>,
    ) -> Result<(), ChildFailure> {
        let event =
            application_signal_event(signal).map_err(|error| {
                match ChildFailure::from_child(error.code) {
                    stop @ ChildFailure::ApprovedCallStop(_) => stop,
                    _ => ChildFailure::Scope(failure()),
                }
            })?;
        self.drain_event(event, receipt, graph_scope, root_container, terminal)
            .await
            .map_err(ChildFailure::Scope)
    }

    async fn drain_event(
        &self,
        mut event: Event,
        receipt: &PipelineGraphCallReceipt,
        graph_scope: Option<&PipelineNodeEventScope>,
        root_container: &str,
        terminal: &mut Option<Event>,
    ) -> Result<(), GraphError> {
        let calls = event.tool_calls();
        if event.invocation_id == receipt.invocation_id()
            && calls
                .iter()
                .any(|call| call.call_id == Some(receipt.activation().call_id()))
        {
            if calls.len() != 1
                || calls[0].name != receipt.original_start().tool_calls()[0].name
                || calls[0].args != receipt.original_start().tool_calls()[0].args
            {
                return Err(failure());
            }
            // The immutable checkpoint receipt is the original model-call identity.
            return self
                .forward(
                    receipt.original_start().clone(),
                    graph_scope,
                    root_container,
                    receipt.invocation_id(),
                )
                .await;
        }
        if event.invocation_id == receipt.invocation_id() {
            receipt.branch().clone_into(&mut event.branch);
        }
        super::scoped_applications::stamp_activation_parent(&mut event, receipt)
            .map_err(|_| failure())?;
        if event.invocation_id == receipt.invocation_id() && event.content().is_some_and(|content|content.parts.iter().any(|part| matches!(part,Part::FunctionResponse {id:Some(id),..} if id==receipt.activation().call_id()))) {
            if terminal.replace(event).is_some(){return Err(failure());}
            return Ok(());
        }
        self.forward(event, graph_scope, root_container, receipt.invocation_id())
            .await
    }

    async fn forward(
        &self,
        mut event: Event,
        graph_scope: Option<&PipelineNodeEventScope>,
        root_container: &str,
        original_root: &str,
    ) -> Result<(), GraphError> {
        if self.events.has_parallel_event_scope() {
            self.events
                .project_parallel_application_event(&mut event, graph_scope, root_container)
                .map_err(|_| failure())?;
            return self
                .events
                .send_routed_application_event(event)
                .await
                .map_err(|_| failure());
        }
        if graph_scope.is_none() {
            super::scoped_applications::stamp_root_scope_projection(
                &mut event,
                original_root,
                root_container,
            )
            .map_err(|_| failure())?;
        }
        // Ordinary leaves keep their original descendant edge. Only the graph-owned
        // synthetic call/result acquires the outer graph container edge here.
        if let Some(scope) = graph_scope
            && !event
                .provider_metadata
                .contains_key(DESCENDANT_CONTAINER_INVOCATION_KEY)
        {
            event.provider_metadata.insert(
                DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
                graph_container(scope, root_container),
            );
            event.provider_metadata.insert(
                DESCENDANT_PARENT_CALL_KEY.to_owned(),
                scope.parent_call_id().to_owned(),
            );
            event.provider_metadata.insert(
                DESCENDANT_CHECKPOINT_THREAD_KEY.to_owned(),
                scope.checkpoint_thread_id().to_owned(),
            );
        }
        self.events
            .send_routed_application_event(event)
            .await
            .map_err(|_| failure())
    }
}

fn graph_container(scope: &PipelineNodeEventScope, root_container: &str) -> String {
    scope.parent().map_or_else(
        || root_container.to_owned(),
        |parent| format!("pipeline-child:{}", parent.parent_call_id()),
    )
}
fn root_branch(branch: &str) -> Result<String, GraphError> {
    let branch = if branch.is_empty() {
        APPLICATION_BRANCH_ROOT
    } else {
        branch
    };
    crate::agents::pipeline::scoped_applications::local_application_branch(
        branch,
        APPLICATION_BRANCH_ROOT,
    )
    .ok_or_else(failure)?;
    Ok(branch.to_owned())
}
fn graph_branch(root: &str, scope: &PipelineNodeEventScope) -> Result<String, GraphError> {
    let mut branch = match scope.parent() {
        Some(parent) => graph_branch(root, parent)?,
        None => root_branch(root)?,
    };
    branch.push_str(".application_");
    branch.push_str(&numeric_call(scope.parent_call_id()));
    Ok(branch)
}
fn numeric_call(call: &str) -> String {
    // Decimal encoding of all SHA-256 bytes keeps the branch grammar without a
    // lossy truncation or a new identity authority; the exact call remains stamped.
    let digest = ring::digest::digest(&ring::digest::SHA256, call.as_bytes());
    let mut digits = vec![0u8];
    for byte in digest.as_ref() {
        let mut carry = u16::from(*byte);
        for digit in &mut digits {
            let value = u16::from(*digit) * 256 + carry;
            *digit = (value % 10).to_le_bytes()[0];
            carry = value / 10;
        }
        while carry > 0 {
            digits.push((carry % 10).to_le_bytes()[0]);
            carry /= 10;
        }
    }
    digits
        .iter()
        .rev()
        .map(|digit| char::from(b'0' + *digit))
        .collect()
}
/// Code prefix of the refusal for a child call an earlier process started and
/// never finished. Fan-out maps it to a typed child failure.
pub(crate) const CALL_MAY_HAVE_STARTED_CODE: &str = "pipeline.agent_call.may_have_started";

fn may_have_started() -> GraphError {
    GraphError::NodeExecutionFailed {
        node: "application_scope".to_owned(),
        message: format!(
            "{CALL_MAY_HAVE_STARTED_CODE}: an earlier run started this Agent call and may have \
             started its work; it is not repeated"
        ),
    }
}

/// Why the child call of one scoped Agent node did not produce a value.
#[derive(Debug)]
enum ChildFailure {
    /// The scope, its receipt or the child stream broke an invariant.
    Scope(GraphError),
    /// The nested Agent refused its decided call before running it.
    ReplayUnsupported,
    /// The approved call may have started (`RECOVERY_REFUSED_UPSTREAM_CODE`),
    /// or its dispatch record could not be written (`DISPATCH_UNRECORDED_CODE`).
    ApprovedCallStop(&'static str),
    /// The nested Agent refused its decided card for another typed reason;
    /// the registered public code.
    Refused(&'static str),
    /// The child failed for a reason that is not a direct-HITL refusal.
    Child,
}

impl ChildFailure {
    /// Classify a child tool error by its code. The nested Agent keeps each
    /// direct-HITL refusal's typed code (FU-226).
    fn from_child(code: &str) -> Self {
        use crate::agents::direct_hitl::DirectHitlErrorCode;
        use crate::agents::dispatch_journal::DISPATCH_UNRECORDED_CODE;
        use crate::protocol::output::RECOVERY_REFUSED_UPSTREAM_CODE;
        if code == RECOVERY_REFUSED_UPSTREAM_CODE {
            Self::ApprovedCallStop(RECOVERY_REFUSED_UPSTREAM_CODE)
        } else if code == DISPATCH_UNRECORDED_CODE {
            Self::ApprovedCallStop(DISPATCH_UNRECORDED_CODE)
        } else if code == DirectHitlErrorCode::UnsupportedCapability.as_str() {
            Self::ReplayUnsupported
        } else if code == DirectHitlErrorCode::ResourceExhausted.as_str() {
            Self::Refused(REFUSED_LIMIT_PUBLIC_CODE)
        } else if code.starts_with("direct_hitl.") {
            Self::Refused(REFUSED_PUBLIC_CODE)
        } else {
            Self::Child
        }
    }
}

/// Registered public code for a decided call that cannot run (no effect ran).
const REPLAY_UNSUPPORTED_PUBLIC_CODE: &str = "pipeline.tool_unavailable";
/// Registered public code for a decided card the nested Agent refused as stale,
/// invalid or corrupt.
const REFUSED_PUBLIC_CODE: &str = "pipeline.input_invalid";
/// Registered public code for a decided card refused for an exhausted budget.
const REFUSED_LIMIT_PUBLIC_CODE: &str = "pipeline.input_limit";

fn node_refused(node: &str) -> GraphError {
    GraphError::NodeExecutionFailed {
        node: node.to_owned(),
        message: "the pipeline Agent node failed".to_owned(),
    }
}

fn replay_unsupported(node: &str) -> GraphError {
    GraphError::NodeExecutionFailed {
        node: node.to_owned(),
        message: "the decided tool call cannot run on this deployment; nothing ran".to_owned(),
    }
}

fn failure() -> GraphError {
    GraphError::NodeExecutionFailed {
        node: "application_scope".to_owned(),
        message:
            "the scoped application call is unavailable or does not match its durable activation"
                .to_owned(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::agents::graph::{compiler::PipelineDefinition, pipeline_node_event_channel};
    use crate::agents::pipeline::composition::{
        PipelineCheckpointCatalog, PipelineCheckpointRevision,
    };
    use crate::agents::pipeline::scoped_applications::checkpoint_thread_for_event;
    use adk_rust::graph::{Checkpoint, ExecutionConfig, MemoryCheckpointer, State};
    use adk_rust::{Content, FunctionResponseData};
    use serde_json::json;

    /// Each typed refusal of a scoped Agent node's child ends the run with its
    /// registered public code; a broken scope or an untyped child failure sends
    /// none and keeps the scope failure.
    #[tokio::test]
    async fn every_child_refusal_reports_its_registered_public_code() {
        use crate::agents::direct_hitl::DirectHitlErrorCode;
        use crate::agents::dispatch_journal::DISPATCH_UNRECORDED_CODE;
        use crate::protocol::output::RuntimeFailureKind;
        use crate::protocol::output::{RECOVERY_REFUSED_UPSTREAM_CODE, model_failure};
        let definition=PipelineDefinition::from_yaml("state: {answer: str}\nentry_point: child\nnodes:\n  - id: child\n    type: agent\n    tool: assistant\n    input_mapping: {task: {type: fixed, value: work}}\n    output: [answer]\n    transition: END\n").unwrap();
        let cases = [
            (
                DirectHitlErrorCode::UnsupportedCapability.as_str(),
                Some(RuntimeFailureKind::PipelineToolUnavailable),
            ),
            (
                DirectHitlErrorCode::StaleDecision.as_str(),
                Some(RuntimeFailureKind::PipelineInputInvalid),
            ),
            (
                DirectHitlErrorCode::InvalidInput.as_str(),
                Some(RuntimeFailureKind::PipelineInputInvalid),
            ),
            (
                DirectHitlErrorCode::CorruptSession.as_str(),
                Some(RuntimeFailureKind::PipelineInputInvalid),
            ),
            (
                DirectHitlErrorCode::ResourceExhausted.as_str(),
                Some(RuntimeFailureKind::PipelineInputLimit),
            ),
            (
                RECOVERY_REFUSED_UPSTREAM_CODE,
                Some(RuntimeFailureKind::RecoveryRefused),
            ),
            (DISPATCH_UNRECORDED_CODE, Some(RuntimeFailureKind::Internal)),
            ("model_gateway.provider_error", None),
        ];
        for (child_code, expected) in cases {
            let scope = PipelineApplicationScope::new(String::new(), &definition, None).unwrap();
            let (events, receiver) = pipeline_node_event_channel();
            let runtime = PipelineApplicationNodeRuntime::new(
                scope,
                "child".to_owned(),
                Arc::new(ApplicationRuntimeProjection::default()),
                events,
                Arc::new(Mutex::new(())),
            );
            let error = runtime
                .child_failure(ChildFailure::from_child(child_code))
                .await;
            let sent = receiver.try_failure_code().await;
            assert_eq!(
                sent.map(|code| model_failure(Some(code))),
                expected,
                "{child_code}"
            );
            let GraphError::NodeExecutionFailed { node, .. } = error else {
                panic!("{child_code}: a node failure");
            };
            let expected_node = if expected.is_some() {
                "child"
            } else {
                "application_scope"
            };
            assert_eq!(node, expected_node, "{child_code}");
        }
    }

    #[tokio::test]
    #[allow(clippy::too_many_lines)] // Keep start, child events, completion, and routing assertions in one ordered proof.
    async fn scope_drain_reuses_original_start_preserves_ordinary_edges_and_holds_terminal_until_commit()
     {
        let definition=PipelineDefinition::from_yaml("state: {answer: str}\nentry_point: child\nnodes:\n  - id: child\n    type: agent\n    tool: assistant\n    input_mapping: {task: {type: fixed, value: work}}\n    output: [answer]\n    transition: END\n").unwrap();
        let revision = PipelineCheckpointRevision {
            application_id: 1,
            version_id: 7,
            definition_digest: definition.definition_digest(),
        };
        let scope =
            PipelineApplicationScope::new(String::new(), &definition, Some(revision.clone()))
                .unwrap();
        let activation = scope.activate("saved-root", "child", 2).unwrap();
        let checkpointer = MemoryCheckpointer::new();
        checkpointer
            .save(&Checkpoint::new(
                "saved-root",
                State::new(),
                2,
                vec!["child".to_owned()],
            ))
            .await
            .unwrap();
        let context = NodeContext::new(State::new(), ExecutionConfig::new("saved-root"), 2);
        let branch = format!("{APPLICATION_BRANCH_ROOT}.application_2");
        let (receipt, _) = prepare_graph_call(
            &checkpointer,
            &Mutex::new(()),
            &context,
            activation,
            "assistant",
            &json!({"task":"work"}),
            "graph-runtime",
            "graph",
            &branch,
        )
        .await
        .unwrap();
        let (events, receiver) = pipeline_node_event_channel();
        let runtime = PipelineApplicationNodeRuntime::new(
            scope,
            "child".to_owned(),
            Arc::new(ApplicationRuntimeProjection::default()),
            events,
            Arc::new(Mutex::new(())),
        );
        let mut terminal = None;
        let mut generated = receipt.original_start().clone();
        generated.id = "fresh-synthetic-id".to_owned();
        generated.provider_metadata.clear();
        generated.branch = APPLICATION_BRANCH_ROOT.to_owned();
        runtime
            .drain(
                ApplicationEventSignal::ContainerEvent(Box::new(generated)),
                &receipt,
                None,
                "graph-runtime",
                &mut terminal,
            )
            .await
            .unwrap();
        let mut child = Event::with_id("ordinary-model", "original-ordinary-runtime");
        child.branch = format!("{branch}.application_4");
        child.llm_response.content = Some(Content::new("model").with_text("working"));
        runtime
            .drain(
                ApplicationEventSignal::Event {
                    container_invocation_id: "graph-runtime".to_owned(),
                    parent_call_id: receipt.activation().call_id().to_owned(),
                    checkpoint_thread_id: None,
                    event: Box::new(child),
                },
                &receipt,
                None,
                "graph-runtime",
                &mut terminal,
            )
            .await
            .unwrap();
        let mut result = Event::with_id("original-result", "graph-runtime");
        result.branch = APPLICATION_BRANCH_ROOT.to_owned();
        result.llm_response.content = Some(Content {
            role: "function".to_owned(),
            parts: vec![Part::FunctionResponse {
                function_response: FunctionResponseData::new(
                    "assistant",
                    json!({"response":"done"}),
                ),
                id: Some(receipt.activation().call_id().to_owned()),
                annotations: None,
            }],
        });
        runtime
            .drain(
                ApplicationEventSignal::ContainerEvent(Box::new(result)),
                &receipt,
                None,
                "graph-runtime",
                &mut terminal,
            )
            .await
            .unwrap();
        let mut drain = receiver
            .drain("new-outer-runtime", "graph", APPLICATION_BRANCH_ROOT)
            .await
            .unwrap();
        let original = drain.try_recv().unwrap().unwrap();
        assert_eq!(original.id, receipt.original_start().id);
        let leaf = drain.try_recv().unwrap().unwrap();
        assert_eq!(leaf.invocation_id, "original-ordinary-runtime");
        assert_eq!(
            leaf.provider_metadata
                .get(DESCENDANT_CONTAINER_INVOCATION_KEY),
            Some(&"graph-runtime".to_owned())
        );
        assert!(
            !leaf
                .provider_metadata
                .contains_key(DESCENDANT_CHECKPOINT_THREAD_KEY)
        );
        let catalog = PipelineCheckpointCatalog {
            root: Some(revision),
            descendants: std::collections::BTreeMap::default(),
        };
        assert_eq!(
            checkpoint_thread_for_event("saved-root", &catalog, &leaf).unwrap(),
            Some("saved-root".to_owned())
        );
        assert!(drain.try_recv().is_none());
        let terminal = terminal.unwrap();
        assert_eq!(terminal.id, "original-result");
        assert_eq!(terminal.branch, branch);
    }

    #[tokio::test(flavor = "current_thread")]
    #[allow(clippy::too_many_lines)] // One ordered proof: hold, flood, block, release.
    async fn a_full_node_event_channel_never_strands_a_child_holding_the_root_writer() {
        const FLOOD: usize = 70;
        let definition=PipelineDefinition::from_yaml("state: {answer: str}\nentry_point: child\nnodes:\n  - id: child\n    type: agent\n    tool: assistant\n    input_mapping: {task: {type: fixed, value: work}}\n    output: [answer]\n    transition: END\n").unwrap();
        let revision = PipelineCheckpointRevision {
            application_id: 1,
            version_id: 7,
            definition_digest: definition.definition_digest(),
        };
        let scope =
            PipelineApplicationScope::new(String::new(), &definition, Some(revision)).unwrap();
        let activation = scope.activate("saved-root", "child", 2).unwrap();
        let checkpointer = MemoryCheckpointer::new();
        checkpointer
            .save(&Checkpoint::new(
                "saved-root",
                State::new(),
                2,
                vec!["child".to_owned()],
            ))
            .await
            .unwrap();
        let context = NodeContext::new(State::new(), ExecutionConfig::new("saved-root"), 2);
        let branch = format!("{APPLICATION_BRANCH_ROOT}.application_2");
        let (receipt, _) = prepare_graph_call(
            &checkpointer,
            &Mutex::new(()),
            &context,
            activation,
            "assistant",
            &json!({"task":"work"}),
            "graph-runtime",
            "graph",
            &branch,
        )
        .await
        .unwrap();
        let (events, node_receiver) = pipeline_node_event_channel();
        let node_probe = events.clone();
        let runtime = PipelineApplicationNodeRuntime::new(
            scope,
            "child".to_owned(),
            Arc::new(ApplicationRuntimeProjection::default()),
            events,
            Arc::new(Mutex::new(())),
        );
        let (child_events, mut child_receiver) = mpsc::channel(64);
        let writer = Arc::new(Mutex::new(()));
        let locked = Arc::new(tokio::sync::Notify::new());
        let (io_done, io) = tokio::sync::oneshot::channel::<()>();
        // The child tool: one "append" holding the root writer, parked on I/O.
        let work = {
            let writer = Arc::clone(&writer);
            let locked = Arc::clone(&locked);
            async move {
                let held = writer.lock().await;
                locked.notify_one();
                let _ = io.await;
                drop(held);
                Ok(json!({"response": "done"}))
            }
        };
        let received = std::sync::atomic::AtomicUsize::new(0);
        let mut terminal = None;
        let drive = runtime.run_child_draining(
            work,
            &mut child_receiver,
            &receipt,
            None,
            "graph-runtime",
            &mut terminal,
        );
        // The Runner: persists every forwarded node event under the root writer.
        let consumer = async {
            let mut drain = node_receiver
                .drain("new-outer-runtime", "graph", APPLICATION_BRANCH_ROOT)
                .await
                .unwrap();
            while received.load(std::sync::atomic::Ordering::SeqCst) < FLOOD {
                if drain.recv().await.is_none() {
                    break;
                }
                received.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                let _persisted = writer.lock().await;
            }
        };
        let flood = async {
            locked.notified().await;
            for index in 0..FLOOD {
                let mut child = Event::with_id(format!("leaf-{index}"), "ordinary-runtime");
                child.branch = format!("{branch}.application_4");
                child.llm_response.content = Some(Content::new("model").with_text("working"));
                child_events
                    .send(ApplicationEventSignal::Event {
                        container_invocation_id: "graph-runtime".to_owned(),
                        parent_call_id: receipt.activation().call_id().to_owned(),
                        checkpoint_thread_id: None,
                        event: Box::new(child),
                    })
                    .await
                    .map_err(|_| ())
                    .unwrap();
            }
            // Precondition: the consumer waits on the writer the child holds
            // and the node-event channel is full, so the next forward blocks.
            let filled = tokio::time::timeout(std::time::Duration::from_secs(5), async {
                while received.load(std::sync::atomic::Ordering::SeqCst) == 0
                    || node_probe.capacity() > 0
                {
                    tokio::task::yield_now().await;
                }
            })
            .await;
            assert!(
                filled.is_ok(),
                "precondition: the node-event channel never filled"
            );
            io_done.send(()).unwrap();
        };
        let Ok(result) = tokio::time::timeout(std::time::Duration::from_secs(10), async {
            tokio::join!(drive, consumer, flood).0
        })
        .await
        else {
            panic!(
                "self-deadlock: the child tool was left suspended holding the root writer \
                 while the full node-event channel blocked its forward"
            );
        };
        assert_eq!(result.unwrap(), json!({"response": "done"}));
        assert_eq!(received.load(std::sync::atomic::Ordering::SeqCst), FLOOD);
        assert!(terminal.is_none());
    }

    #[test]
    fn graph_branch_uses_valid_full_digest_segments_and_distinct_parent_calls() {
        let left = PipelineNodeEventScope::new("left", "saved", "root/child").unwrap();
        let right = PipelineNodeEventScope::new("right", "saved", "root/child").unwrap();
        let left_branch = graph_branch(APPLICATION_BRANCH_ROOT, &left).unwrap();
        let right_branch = graph_branch(APPLICATION_BRANCH_ROOT, &right).unwrap();
        assert_ne!(left_branch, right_branch);
        assert!(
            crate::agents::pipeline::scoped_applications::local_application_branch(
                &left_branch,
                APPLICATION_BRANCH_ROOT
            )
            .is_some()
        );
        assert_eq!(numeric_call("left"), numeric_call("left"));
    }
}
