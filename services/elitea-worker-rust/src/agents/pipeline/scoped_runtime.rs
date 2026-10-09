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
use std::sync::Arc;
use tokio::sync::Mutex;

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
        let (receipt, fresh) = prepare_graph_call(
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
        match receipt.outcome() {
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
                if !self
                    .applications
                    .resume_coordinator()
                    .ok_or_else(failure)?
                    .has_resume(receipt.invocation_id(), receipt.activation().call_id())
                    .await
                {
                    return Ok((result.clone(), receipt.activation().call_id().to_owned()));
                }
            }
            GraphCallOutcome::Started if !fresh => {
                // An interrupted unpaused provider/tool has no graph-level replay authority.
                return Err(failure());
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
        // Driven: forwarding a child event (a bounded send the Runner drains by
        // persisting it) must never leave the child parked mid-append holding
        // the root session writer.
        let tool = Arc::clone(tool);
        let mut future =
            DrivenTask::new(async move { tool.execute(tool_context, arguments).await });
        let mut terminal = None;
        let result = loop {
            tokio::select! {
                value = &mut future => break match value { Ok(Ok(value)) => Ok(value), _ => Err(failure()) },
                signal = receiver.recv() => match signal {
                    Some(signal) => if let Err(error) = self.drain(signal,&receipt,graph_scope.as_ref(),parent.invocation_id(),&mut terminal).await { break Err(error); },
                    None => break Err(failure()),
                }
            }
        };
        // A drain failure stops the tool task before the queue drain below.
        future.cancel().await;
        // Once tool polling stops no child sender may remain active. Drain its bounded queue.
        let mut result = result;
        while let Ok(signal) = receiver.try_recv() {
            if let Err(error) = self
                .drain(
                    signal,
                    &receipt,
                    graph_scope.as_ref(),
                    parent.invocation_id(),
                    &mut terminal,
                )
                .await
            {
                result = Err(error);
                break;
            }
        }
        receiver_owner
            .restore(receiver)
            .await
            .map_err(|_| failure())?;
        let result = result?;
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

    async fn drain(
        &self,
        signal: ApplicationEventSignal,
        receipt: &PipelineGraphCallReceipt,
        graph_scope: Option<&PipelineNodeEventScope>,
        root_container: &str,
        terminal: &mut Option<Event>,
    ) -> Result<(), GraphError> {
        let mut event = application_signal_event(signal).map_err(|_| failure())?;
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
