//! Run-time settings layer of a deterministic node (5b-1, PLAN §3.2).
//!
//! Wraps a State Modifier, Router, Split Out or Aggregate node that has
//! non-default settings; a node without settings is bound unwrapped. The
//! bodies are synchronous and pure, so a settled failure (`continue` or
//! `error_output`) is one ordinary node output: the `node_errors` entry, the
//! declared outputs (untouched, or neutral with `always_output_data`) and the
//! transition, all in the step's own checkpointed update. No extra I/O.

use std::sync::Arc;

use adk_rust::graph::{END, GraphError, Node, NodeContext, NodeOutput};
use async_trait::async_trait;
use elitea_agent_runtime::graph::node_settings::{
    DeterministicFamily, EXECUTED_ONCE_KEY, MAX_NODE_ERRORS, NODE_ERRORS_KEY, NodeErrorEntry,
    OnNodeError, executed_once, mark_executed_once, neutral_value, node_error_count,
    node_error_support_ref,
};
use elitea_faults::{Code, codes};
use serde_json::{Value, json};

use super::compiler::AdmittedNodeSettings;

/// The registered failure code of each deterministic family. The entry's
/// code and message come from here only, so a live and a replayed entry are
/// byte-identical and never carry state, template or item text.
pub(crate) const fn family_failure(family: DeterministicFamily) -> Code {
    match family {
        DeterministicFamily::StateModifier => codes::PIPELINE_STATE_MODIFIER_FAILED,
        DeterministicFamily::Router => codes::PIPELINE_ROUTER_FAILED,
        DeterministicFamily::SplitOut => codes::PIPELINE_SPLIT_OUT_FAILED,
        DeterministicFamily::Aggregate => codes::PIPELINE_AGGREGATE_FAILED,
    }
}

/// Whether a body error is a failure of the node's own work, which its
/// settings may settle (D6). Everything else propagates: interrupts,
/// checkpoint, recursion, channel and timeout errors, and the recovery and
/// settings integrity codes.
pub(super) fn settled_by_settings(error: &GraphError) -> bool {
    match error {
        GraphError::NodeExecutionFailed { .. } => true,
        GraphError::Other(message) => {
            !message.starts_with("pipeline.node_recovery.")
                && !message.starts_with("pipeline.node_errors.")
                && !message.starts_with("pipeline.node_settings.")
                && !message.starts_with("graph.")
        }
        _ => false,
    }
}

pub(crate) struct SettingsNode<N> {
    inner: N,
    admitted: Arc<AdmittedNodeSettings>,
    /// The Main execution this graph runs for: the support reference and
    /// the operator line name it. `None` outside a run (tests, debug compile):
    /// the thread then scopes the support reference.
    execution_id: Option<Arc<String>>,
}

impl<N: Node> SettingsNode<N> {
    pub(crate) fn new(
        inner: N,
        admitted: AdmittedNodeSettings,
        execution_id: Option<Arc<String>>,
    ) -> Self {
        Self {
            inner,
            admitted: Arc::new(admitted),
            execution_id,
        }
    }

    fn settings(&self) -> &elitea_agent_runtime::graph::node_settings::NodeSettings {
        &self.admitted.settings
    }

    /// Neutral values for the declared outputs the body wrote as null. A key
    /// the body did not write keeps its state: on success nothing else is
    /// overwritten.
    fn fill_outputs(&self, mut output: NodeOutput) -> NodeOutput {
        if !self.settings().always_output_data {
            return output;
        }
        for (key, kind) in &self.admitted.outputs {
            if let Some(value) = output.updates.get_mut(key)
                && value.is_null()
            {
                *value = neutral_value(kind);
            }
        }
        output
    }

    fn mark_once(
        &self,
        context: &NodeContext,
        mut output: NodeOutput,
        route: Option<&str>,
    ) -> Result<NodeOutput, GraphError> {
        if !self.settings().execute_once {
            return Ok(output);
        }
        let set = mark_executed_once(context.get(EXECUTED_ONCE_KEY), self.inner.name(), route)
            .map_err(|_| {
                GraphError::Other(codes::PIPELINE_NODE_SETTINGS_INVALID.as_str().to_owned())
            })?;
        output.updates.insert(EXECUTED_ONCE_KEY.to_owned(), set);
        Ok(output)
    }

    fn settle(&self, context: &NodeContext, error: GraphError) -> Result<NodeOutput, GraphError> {
        let on_error = self.settings().on_error;
        if on_error == OnNodeError::Stop || !settled_by_settings(&error) {
            return Err(error);
        }
        let node = self.inner.name();
        if node_error_count(context.get(NODE_ERRORS_KEY)) >= MAX_NODE_ERRORS {
            let code = codes::PIPELINE_NODE_ERRORS_EXHAUSTED;
            elitea_faults::fault!(
                tracing::Level::WARN,
                code = code,
                event = "pipeline_node_errors_exhausted",
                node_id = %node
            );
            return Err(GraphError::Other(code.as_str().to_owned()));
        }
        let step = u64::try_from(context.step).unwrap_or(u64::MAX);
        // The execution and the thread: a parent and a saved child of one
        // execution never share a support reference.
        let scope = format!(
            "{}\0{}",
            self.execution_id.as_ref().map_or("", |id| id.as_str()),
            context.config.thread_id
        );
        let failure = family_failure(self.admitted.family);
        let support_ref = node_error_support_ref(&scope, node, step);
        let entry = NodeErrorEntry {
            node,
            node_type: self.admitted.family.node_type(),
            handled: on_error,
            code: failure.as_str(),
            message: failure.sentence(),
            support_ref: support_ref.clone(),
            retryable: false,
            attempts: 1,
            input_step: step,
        }
        .to_value()
        .map_err(|_| {
            GraphError::Other(codes::PIPELINE_NODE_SETTINGS_INVALID.as_str().to_owned())
        })?;
        let mut output = NodeOutput::new().with_update(NODE_ERRORS_KEY, json!([entry]));
        if self.settings().always_output_data {
            for (key, kind) in &self.admitted.outputs {
                output = output.with_update(key, neutral_value(kind));
            }
        }
        let route = match (on_error, self.admitted.router_default.as_ref()) {
            (OnNodeError::ErrorOutput, _) => self.settings().error_transition.clone(),
            (OnNodeError::Continue, Some((target, label))) => {
                output = output.with_update("router_output", Value::String(label.clone()));
                Some(target.clone())
            }
            _ => None,
        };
        if let Some(target) = route.as_deref() {
            output = output.with_goto([if target == "END" { END } else { target }]);
        }
        elitea_faults::fault!(
            tracing::Level::INFO,
            code = codes::PIPELINE_NODE_FAILED_CONTINUED,
            event = "pipeline_node_failed_continued",
            node_id = %node,
            failure_code = failure.as_str(),
            handled = on_error.handled().unwrap_or_default(),
            attempts = 1_u16,
            support_ref = %support_ref,
            execution_id = self.execution_id.as_ref().map_or("", |id| id.as_str())
        );
        // A Router records the route it took, so a skipped revisit takes it
        // again; other families take their declared transition.
        let recorded = self.admitted.router_default.as_ref().and(route.as_deref());
        self.mark_once(context, output, recorded)
    }
}

#[async_trait]
impl<N: Node> Node for SettingsNode<N> {
    fn name(&self) -> &str {
        self.inner.name()
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        if self.settings().execute_once
            && let Some(route) = executed_once(context.get(EXECUTED_ONCE_KEY), self.inner.name())
        {
            // D8: a revisit in the same turn runs no body and changes no
            // state; a Router takes the route its first visit took.
            let output = NodeOutput::new();
            return Ok(match (self.admitted.router_default.as_ref(), route) {
                (Some(_), Some(target)) => output.with_goto([if target == "END" {
                    END.to_owned()
                } else {
                    target
                }]),
                (Some((target, _)), None) => output.with_goto([if target == "END" {
                    END.to_owned()
                } else {
                    target.clone()
                }]),
                (None, _) => output,
            });
        }
        match self.inner.execute(context).await {
            Ok(output) => {
                let route = output
                    .goto
                    .as_ref()
                    .and_then(|targets| targets.first())
                    .map(|target| {
                        if target == END {
                            "END".to_owned()
                        } else {
                            target.clone()
                        }
                    });
                let output = self.fill_outputs(output);
                let recorded = self.admitted.router_default.as_ref().and(route.as_deref());
                self.mark_once(context, output, recorded)
            }
            Err(error) => self.settle(context, error),
        }
    }
}
