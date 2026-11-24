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
use elitea_agent_runtime::graph::node_recovery::NodeFailureClass;
use elitea_agent_runtime::graph::node_settings::{
    DeterministicFamily, EXECUTED_ONCE_KEY, MAX_NODE_ERRORS, NODE_ERRORS_KEY, NodeErrorEntry,
    OnNodeError, executed_once, mark_executed_once, neutral_value, node_error_count,
    node_error_support_ref,
};
use elitea_agent_runtime::graph::{router, state_modifier};
use elitea_faults::{Code, codes};
use serde_json::{Value, json};

use super::compiler::{AdmittedNodeFamily, AdmittedNodeSettings};

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

/// The registered failure code of a settled Code node failure (5b-1 PR B).
/// Driven strictly by `NodeFailureClass` so live and restored entries are
/// byte-identical and never carry source, stdout, stderr or state text.
pub(crate) const fn code_failure_fault(class: NodeFailureClass) -> Code {
    match class {
        NodeFailureClass::AttemptTimeout => codes::PIPELINE_CODE_TIMEOUT,
        _ => codes::PIPELINE_CODE_FAILED,
    }
}

/// The registered failure code of a settled LLM node failure (5b-2).
/// Driven strictly by `NodeFailureClass` so live and restored entries are
/// byte-identical and never carry model, prompt, or provider text.
pub(crate) const fn llm_failure_fault(class: NodeFailureClass) -> Code {
    match class {
        NodeFailureClass::AttemptTimeout => codes::PIPELINE_LLM_TIMEOUT,
        NodeFailureClass::RateLimited => codes::PIPELINE_LLM_RATE_LIMITED,
        NodeFailureClass::DependencyUnavailable => codes::PIPELINE_LLM_UNAVAILABLE,
        _ => codes::PIPELINE_LLM_FAILED,
    }
}

/// The registered failure code of a settled Direct Tool (`toolkit` / `mcp`)
/// node failure (5b-3). Never carries tool arguments, payloads, or server text.
pub(crate) const fn direct_tool_failure_fault(
    family: AdmittedNodeFamily,
    class: NodeFailureClass,
) -> Code {
    match (family, class) {
        (AdmittedNodeFamily::Mcp, NodeFailureClass::AttemptTimeout) => codes::PIPELINE_MCP_TIMEOUT,
        (AdmittedNodeFamily::Mcp, _) => codes::PIPELINE_MCP_FAILED,
        (_, NodeFailureClass::AttemptTimeout) => codes::PIPELINE_TOOLKIT_TIMEOUT,
        (_, _) => codes::PIPELINE_TOOLKIT_FAILED,
    }
}

/// The registered failure code of any admitted node family's settled failure.
pub(crate) const fn settled_failure_fault(
    family: AdmittedNodeFamily,
    class: NodeFailureClass,
) -> Code {
    match family {
        AdmittedNodeFamily::Deterministic(family) => family_failure(family),
        AdmittedNodeFamily::Code => code_failure_fault(class),
        AdmittedNodeFamily::Llm => llm_failure_fault(class),
        AdmittedNodeFamily::Toolkit | AdmittedNodeFamily::Mcp => {
            direct_tool_failure_fault(family, class)
        }
    }
}

/// Fill any declared output the node wrote as `null` with its neutral value
/// when `always_output_data` is set. Outputs not written keep their state.
pub(crate) fn fill_neutral_outputs(
    admitted: &AdmittedNodeSettings,
    mut output: NodeOutput,
) -> NodeOutput {
    if !admitted.settings.always_output_data {
        return output;
    }
    for (key, kind) in &admitted.outputs {
        if let Some(value) = output.updates.get_mut(key)
            && value.is_null()
        {
            *value = neutral_value(kind);
        }
    }
    output
}

/// Record `node` in `__elitea_executed_once_v1` in the same checkpointed
/// update when `execute_once` is enabled (D8).
pub(crate) fn mark_node_executed_once(
    admitted: &AdmittedNodeSettings,
    node: &str,
    context: &NodeContext,
    mut output: NodeOutput,
    route: Option<&str>,
) -> Result<NodeOutput, GraphError> {
    if !admitted.settings.execute_once {
        return Ok(output);
    }
    let set = mark_executed_once(context.get(EXECUTED_ONCE_KEY), node, route).map_err(|_| {
        GraphError::Other(codes::PIPELINE_NODE_SETTINGS_INVALID.as_str().to_owned())
    })?;
    output.updates.insert(EXECUTED_ONCE_KEY.to_owned(), set);
    Ok(output)
}

/// Settle a committed failure into one `node_errors` entry, optional neutral
/// outputs (`always_output_data`), optional `error_transition` goto, and the
/// single `pipeline.node.failed_continued` operator line. Shared by
/// `SettingsNode` (deterministic families) and `RecoverableNode` (Code live
/// and restore paths).
#[allow(
    clippy::too_many_arguments,
    reason = "Explicit data-free settlement facts shared across deterministic and Code paths."
)]
pub(crate) fn settle_node_failure(
    admitted: &AdmittedNodeSettings,
    execution_id: Option<&str>,
    node: &str,
    context: &NodeContext,
    failure: Code,
    retryable: bool,
    attempts: u16,
) -> Result<NodeOutput, GraphError> {
    let on_error = admitted.settings.on_error;
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
        execution_id.unwrap_or(""),
        context.config.thread_id
    );
    let support_ref = node_error_support_ref(&scope, node, step);
    let entry = NodeErrorEntry {
        node,
        node_type: admitted.family.node_type(),
        handled: on_error,
        code: failure.as_str(),
        message: failure.sentence(),
        support_ref: support_ref.clone(),
        retryable,
        attempts: attempts.max(1),
        input_step: step,
    }
    .to_value()
    .map_err(|_| GraphError::Other(codes::PIPELINE_NODE_SETTINGS_INVALID.as_str().to_owned()))?;
    let mut output = NodeOutput::new().with_update(NODE_ERRORS_KEY, json!([entry]));
    if admitted.settings.always_output_data {
        for (key, kind) in &admitted.outputs {
            output = output.with_update(key, neutral_value(kind));
        }
    }
    let route = match (on_error, admitted.router_default.as_ref()) {
        (OnNodeError::ErrorOutput, _) => admitted.settings.error_transition.clone(),
        (OnNodeError::Continue, Some((target, label))) => {
            output = output.with_update(router::ROUTER_OUTPUT_KEY, Value::String(label.clone()));
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
        attempts = attempts.max(1),
        support_ref = %support_ref,
        execution_id = execution_id.unwrap_or("")
    );
    // A Router records the route it took, so a skipped revisit takes it
    // again; other families take their declared transition.
    let recorded = admitted.router_default.as_ref().and(route.as_deref());
    mark_node_executed_once(admitted, node, context, output, recorded)
}

/// The `GraphError::Other` messages a wrapped deterministic body is known to
/// raise for its own work failing. Router, Split Out and Aggregate fail with
/// `NodeExecutionFailed` only (`router.rs` `node_failure`,
/// `data_shaping.rs`).
const FAMILY_OTHER_FAILURES: [&str; 2] = [
    state_modifier::TEMPLATE_FAILED,
    state_modifier::OUTPUT_BOUND_EXCEEDED,
];

/// Whether a body error is a failure of the node's own work, which its
/// settings may settle (D6). An allowlist: `NodeExecutionFailed` and the exact
/// messages in `FAMILY_OTHER_FAILURES`. Everything else propagates:
/// interrupts, checkpoint, recursion, channel and timeout errors, the recovery
/// and settings integrity codes, and any `Other` not on the list.
pub(super) fn settled_by_settings(error: &GraphError) -> bool {
    match error {
        GraphError::NodeExecutionFailed { .. } => true,
        GraphError::Other(message) => FAMILY_OTHER_FAILURES.contains(&message.as_str()),
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

    fn fill_outputs(&self, output: NodeOutput) -> NodeOutput {
        fill_neutral_outputs(&self.admitted, output)
    }

    fn mark_once(
        &self,
        context: &NodeContext,
        output: NodeOutput,
        route: Option<&str>,
    ) -> Result<NodeOutput, GraphError> {
        mark_node_executed_once(&self.admitted, self.inner.name(), context, output, route)
    }

    fn settle(&self, context: &NodeContext, error: GraphError) -> Result<NodeOutput, GraphError> {
        let on_error = self.settings().on_error;
        if on_error == OnNodeError::Stop || !settled_by_settings(&error) {
            return Err(error);
        }
        let AdmittedNodeFamily::Deterministic(family) = self.admitted.family else {
            return Err(error);
        };
        settle_node_failure(
            &self.admitted,
            self.execution_id.as_deref().map(String::as_str),
            self.inner.name(),
            context,
            family_failure(family),
            false,
            1,
        )
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
