//! Elitea identity adapter for ADK-Rust `GraphAgent` events.
//!
//! ADK-Rust 2.0.0's graph interrupt bridge emits an otherwise correct event
//! with a placeholder invocation ID and no author. The runner does not rewrite
//! those fields. This narrow wrapper binds every graph event back to the exact
//! invocation and root agent that own it before the browser projector sees it.

#![allow(dead_code)] // Construction opens with the stored-pipeline compiler.

use std::sync::Arc;

use adk_rust::futures::StreamExt as _;
use adk_rust::graph::Checkpointer;
use adk_rust::graph::GraphAgent;
use adk_rust::graph::interrupt::{GraphInterruptPayload, INTERRUPT_METADATA_KEY};
use adk_rust::{
    AdkError, Agent, Content, ErrorCategory, ErrorComponent, Event, EventStream, InvocationContext,
};
use async_trait::async_trait;

pub(crate) const PIPELINE_COMPLETED_METADATA_KEY: &str = "elitea.pipeline.completed";
pub(crate) const PIPELINE_COMPLETED_METADATA_VALUE: &str = "v1";
/// Completion reuses checkpoint output after a control-only terminal decision.
pub(crate) const PIPELINE_REUSED_RESULT_METADATA_KEY: &str = "elitea.pipeline.result_reused";
pub(crate) const PIPELINE_COMPLETED_CONTENT: &str = "Pipeline completed.";

/// Canonical static allowlist of typed model-gateway error codes propagated
/// across `adk-graph` (`FU-343`). Never echoes untrusted message text: returns
/// only the registered static code, ADK `ErrorCategory`, and safe catalog text.
#[allow(
    clippy::too_many_lines,
    reason = "Static allowlist mapping every model_gateway.* and anthropic_gateway.* code."
)]
pub(crate) fn model_gateway_error_spec(
    code: &str,
) -> Option<(ErrorCategory, &'static str, &'static str)> {
    Some(match code {
        "model_gateway.rate_limited" => (
            ErrorCategory::RateLimited,
            "model_gateway.rate_limited",
            "the model gateway rate limit was reached",
        ),
        "model_gateway.response_header_timeout" => (
            ErrorCategory::Timeout,
            "model_gateway.response_header_timeout",
            "the model gateway response timed out",
        ),
        "model_gateway.stream_idle_timeout" => (
            ErrorCategory::Timeout,
            "model_gateway.stream_idle_timeout",
            "the model gateway stream timed out",
        ),
        "model_gateway.upstream_timeout" => (
            ErrorCategory::Timeout,
            "model_gateway.upstream_timeout",
            "the model gateway timed out",
        ),
        "anthropic_gateway.response_header_timeout" => (
            ErrorCategory::Timeout,
            "anthropic_gateway.response_header_timeout",
            "the native Anthropic response timed out",
        ),
        "model_gateway.unavailable" => (
            ErrorCategory::Unavailable,
            "model_gateway.unavailable",
            "the model gateway is unavailable",
        ),
        "model_gateway.transport" => (
            ErrorCategory::Unavailable,
            "model_gateway.transport",
            "the model gateway transport failed",
        ),
        "model_gateway.stream_transport" => (
            ErrorCategory::Unavailable,
            "model_gateway.stream_transport",
            "the model gateway stream transport failed",
        ),
        "model_gateway.conflict" => (
            ErrorCategory::Unavailable,
            "model_gateway.conflict",
            "the model gateway reported a retryable conflict",
        ),
        "model_gateway.http_version" => (
            ErrorCategory::Unavailable,
            "model_gateway.http_version",
            "the model gateway did not negotiate HTTP/2",
        ),
        "anthropic_gateway.transport" => (
            ErrorCategory::Unavailable,
            "anthropic_gateway.transport",
            "the native Anthropic transport is unavailable",
        ),
        "model_gateway.too_many_events" => (
            ErrorCategory::Internal,
            "model_gateway.too_many_events",
            "the model gateway emitted too many stream events",
        ),
        "model_gateway.provider_error" => (
            ErrorCategory::Internal,
            "model_gateway.provider_error",
            "the model provider reported an error",
        ),
        "anthropic_gateway.provider_error" => (
            ErrorCategory::Internal,
            "anthropic_gateway.provider_error",
            "the native Anthropic provider reported an error",
        ),
        "model_gateway.unauthorized" => (
            ErrorCategory::Unauthorized,
            "model_gateway.unauthorized",
            "the model gateway rejected the execution credential",
        ),
        "model_gateway.forbidden" => (
            ErrorCategory::Forbidden,
            "model_gateway.forbidden",
            "the model gateway rejected the execution project",
        ),
        "model_gateway.budget_exhausted" => (
            ErrorCategory::InvalidInput,
            "model_gateway.budget_exhausted",
            "the model budget is exhausted",
        ),
        "model_gateway.project_budget_exhausted" => (
            ErrorCategory::InvalidInput,
            "model_gateway.project_budget_exhausted",
            "the project model budget is exhausted",
        ),
        "model_gateway.member_budget_exhausted" => (
            ErrorCategory::InvalidInput,
            "model_gateway.member_budget_exhausted",
            "the member model budget is exhausted",
        ),
        "model_gateway.rejected" => (
            ErrorCategory::InvalidInput,
            "model_gateway.rejected",
            "the model gateway rejected the admitted request",
        ),
        "model_gateway.guardrail_blocked" => (
            ErrorCategory::InvalidInput,
            "model_gateway.guardrail_blocked",
            "the model gateway blocked the request",
        ),
        "model_gateway.invalid_request" => (
            ErrorCategory::InvalidInput,
            "model_gateway.invalid_request",
            "the model gateway request is invalid",
        ),
        "model_gateway.request_too_large" => (
            ErrorCategory::InvalidInput,
            "model_gateway.request_too_large",
            "the model gateway request exceeds its approved limit",
        ),
        "anthropic_gateway.invalid_request" => (
            ErrorCategory::InvalidInput,
            "anthropic_gateway.invalid_request",
            "the native Anthropic request is invalid",
        ),
        "anthropic_gateway.sampling_unsupported" => (
            ErrorCategory::InvalidInput,
            "anthropic_gateway.sampling_unsupported",
            "the native Anthropic sampling parameter is unsupported",
        ),
        "anthropic_gateway.request_too_large" => (
            ErrorCategory::InvalidInput,
            "anthropic_gateway.request_too_large",
            "the native Anthropic request exceeds its approved limit",
        ),
        "model_gateway.incomplete_stream" => (
            ErrorCategory::Internal,
            "model_gateway.incomplete_stream",
            "the model gateway stream ended before completion",
        ),
        "model_gateway.invalid_sse" => (
            ErrorCategory::Internal,
            "model_gateway.invalid_sse",
            "the model gateway stream framing is malformed",
        ),
        "model_gateway.response_type" => (
            ErrorCategory::Internal,
            "model_gateway.response_type",
            "the model gateway response type is malformed",
        ),
        "model_gateway.done_before_completion" => (
            ErrorCategory::Internal,
            "model_gateway.done_before_completion",
            "the model gateway stream finished without a complete response",
        ),
        "model_gateway.event_after_completion" => (
            ErrorCategory::Internal,
            "model_gateway.event_after_completion",
            "the model gateway stream emitted an event after completion",
        ),
        "anthropic_gateway.invalid_stream" => (
            ErrorCategory::Internal,
            "anthropic_gateway.invalid_stream",
            "the native Anthropic stream is malformed",
        ),
        "anthropic_gateway.incomplete_stream" => (
            ErrorCategory::Internal,
            "anthropic_gateway.incomplete_stream",
            "the native Anthropic stream ended before completion",
        ),
        _ => return None,
    })
}

/// Format a typed model-gateway failure as a `GraphError` so `adk-graph`
/// preserves the registered static code across nested graphs and fan-out
/// boundaries without carrying untrusted provider text (`FU-343`).
pub(crate) fn model_gateway_graph_error(
    node: &str,
    code: &str,
) -> Option<adk_rust::graph::GraphError> {
    let (_, static_code, message) = model_gateway_error_spec(code)?;
    Some(adk_rust::graph::GraphError::NodeExecutionFailed {
        node: node.to_owned(),
        message: format!("{static_code}: {message}"),
    })
}

/// Extract a registered static `model_gateway.*` / `anthropic_gateway.*` code
/// from a wrapped graph-error message (`FU-343`).
pub(crate) fn model_gateway_code_in_message(message: &str) -> Option<&'static str> {
    let inner = super::parallel::unwrap_node_failures(message);
    let candidate = inner.split_once(": ").map_or(inner, |(code, _)| code);
    model_gateway_error_spec(candidate).map(|(_, static_code, _)| static_code)
}

/// Restore a typed `AdkError` when `adk-graph` collapses a typed
/// `model_gateway.*` / `anthropic_gateway.*` `GraphError` into
/// `agent.legacy` / `ErrorCategory::Internal` (`FU-343`).
pub(crate) fn restore_typed_graph_error(error: AdkError) -> AdkError {
    if error.code != "agent.legacy" {
        return error;
    }
    let Some(code) = model_gateway_code_in_message(&error.message) else {
        return error;
    };
    let Some((category, static_code, message)) = model_gateway_error_spec(code) else {
        return error;
    };
    AdkError::new(ErrorComponent::Model, category, static_code, message)
}

use super::printer::{
    MAX_PRINTER_OUTPUT_BYTES, PRINTER_COMPLETED_STATE, PRINTER_OUTPUT_STATE_KEY,
    PRINTER_PAUSE_METADATA_KEY, PrinterPauseCatalog,
};

/// Emit one bounded terminal marker without serializing private graph state.
pub(crate) fn pipeline_completed_event() -> Event {
    pipeline_result_event(PIPELINE_COMPLETED_CONTENT)
}

/// Emit one bounded terminal result selected from public pipeline state.
pub(crate) fn pipeline_result_event(content: &str) -> Event {
    let mut event = Event::new("graph_invocation_pending");
    event.set_content(Content::new("assistant").with_text(content));
    event.provider_metadata.insert(
        PIPELINE_COMPLETED_METADATA_KEY.to_owned(),
        PIPELINE_COMPLETED_METADATA_VALUE.to_owned(),
    );
    event
}

pub(crate) struct EliteaGraphAgent {
    name: String,
    description: String,
    graph: GraphAgent,
    sub_agents: Vec<Arc<dyn Agent>>,
    printer_interrupts: Option<PrinterInterruptAdapter>,
    static_interrupts: Option<StaticInterruptAdapter>,
}

impl EliteaGraphAgent {
    #[must_use]
    pub(crate) fn new(graph: GraphAgent) -> Self {
        Self {
            name: graph.name().to_owned(),
            description: graph.description().to_owned(),
            graph,
            sub_agents: Vec::new(),
            printer_interrupts: None,
            static_interrupts: None,
        }
    }

    #[must_use]
    pub(crate) fn with_static_interrupts(
        mut self,
        checkpointer: Arc<dyn Checkpointer>,
        catalog: super::static_pause::StaticPauseCatalog,
    ) -> Self {
        if !catalog.is_empty() {
            self.static_interrupts = Some(StaticInterruptAdapter {
                checkpointer,
                catalog,
            });
        }
        self
    }

    #[must_use]
    pub(crate) fn with_printer_interrupts(
        mut self,
        checkpointer: Arc<dyn Checkpointer>,
        catalog: PrinterPauseCatalog,
    ) -> Self {
        if !catalog.is_empty() {
            self.printer_interrupts = Some(PrinterInterruptAdapter {
                checkpointer,
                catalog,
            });
        }
        self
    }
}

#[derive(Clone)]
struct StaticInterruptAdapter {
    checkpointer: Arc<dyn Checkpointer>,
    catalog: super::static_pause::StaticPauseCatalog,
}

impl StaticInterruptAdapter {
    async fn enrich(&self, event: &mut Event) -> adk_rust::Result<()> {
        if event
            .provider_metadata
            .contains_key(PRINTER_PAUSE_METADATA_KEY)
        {
            return Ok(());
        }
        let Some(payload) = GraphInterruptPayload::from_event(event) else {
            return Ok(());
        };
        if !matches!(payload.kind.as_str(), "before" | "after") {
            return Ok(());
        }
        let node = payload
            .node
            .as_deref()
            .ok_or_else(|| adk_rust::AdkError::agent("Static pause node is missing"))?;
        let checkpoint = self
            .checkpointer
            .load_by_id(&payload.checkpoint_id)
            .await
            .map_err(|_| adk_rust::AdkError::agent("Static pause checkpoint lookup failed"))?
            .ok_or_else(|| adk_rust::AdkError::agent("Static pause checkpoint is unavailable"))?;
        if checkpoint.thread_id != payload.thread_id
            || checkpoint.checkpoint_id != payload.checkpoint_id
        {
            return Err(adk_rust::AdkError::agent(
                "Static pause checkpoint identity is invalid",
            ));
        }
        let metadata = self
            .catalog
            .bind(&payload.kind, node, &checkpoint)
            .ok_or_else(|| adk_rust::AdkError::agent("Static pause frontier is invalid"))?;
        event.set_content(
            Content::new("assistant").with_text(super::static_pause::STATIC_PAUSE_MESSAGE),
        );
        event.provider_metadata.insert(
            super::static_pause::STATIC_PAUSE_METADATA_KEY.to_owned(),
            serde_json::to_string(&metadata)
                .map_err(|_| adk_rust::AdkError::agent("Static pause metadata encoding failed"))?,
        );
        Ok(())
    }
}

#[derive(Clone)]
struct PrinterInterruptAdapter {
    checkpointer: Arc<dyn Checkpointer>,
    catalog: PrinterPauseCatalog,
}

impl PrinterInterruptAdapter {
    async fn enrich(&self, event: &mut Event) -> adk_rust::Result<()> {
        let Some(payload) = GraphInterruptPayload::from_event(event) else {
            return Ok(());
        };
        if payload.kind != "after" {
            return Ok(());
        }
        let Some(node) = payload.node.as_deref() else {
            return Ok(());
        };
        let Some(metadata) = self.catalog.get(node) else {
            return Ok(());
        };
        let checkpoint = self
            .checkpointer
            .load_by_id(&payload.checkpoint_id)
            .await
            .map_err(|_| adk_rust::AdkError::agent("Printer checkpoint lookup failed"))?
            .ok_or_else(|| adk_rust::AdkError::agent("Printer checkpoint is unavailable"))?;
        let output = checkpoint
            .state
            .get(PRINTER_OUTPUT_STATE_KEY)
            .and_then(serde_json::Value::as_str)
            .filter(|value| {
                *value != PRINTER_COMPLETED_STATE
                    && !value.is_empty()
                    && value.len() <= MAX_PRINTER_OUTPUT_BYTES
                    && !value.chars().any(|character| {
                        character == '\0'
                            || (character.is_control() && !matches!(character, '\n' | '\r' | '\t'))
                    })
            })
            .ok_or_else(|| adk_rust::AdkError::agent("Printer checkpoint output is invalid"))?;
        if checkpoint.thread_id != payload.thread_id
            || checkpoint.checkpoint_id != payload.checkpoint_id
            || checkpoint.pending_nodes.as_slice() != [metadata.reset_node_name.as_str()]
        {
            return Err(adk_rust::AdkError::agent(
                "Printer checkpoint identity is invalid",
            ));
        }
        event.set_content(Content::new("assistant").with_text(output));
        event.provider_metadata.insert(
            PRINTER_PAUSE_METADATA_KEY.to_owned(),
            serde_json::to_string(metadata)
                .map_err(|_| adk_rust::AdkError::agent("Printer metadata encoding failed"))?,
        );
        Ok(())
    }
}

#[async_trait]
impl Agent for EliteaGraphAgent {
    fn name(&self) -> &str {
        &self.name
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        &self.sub_agents
    }

    fn supports_agent_transfer(&self) -> bool {
        false
    }

    async fn run(&self, context: Arc<dyn InvocationContext>) -> adk_rust::Result<EventStream> {
        let invocation_id = context.invocation_id().to_owned();
        let author = self.name.clone();
        let printer_interrupts = self.printer_interrupts.clone();
        let static_interrupts = self.static_interrupts.clone();
        let events = self.graph.run(context).await?;
        Ok(Box::pin(async_stream::stream! {
            let mut events = events;
            while let Some(result) = events.next().await {
                let mut event = match result {
                    Ok(event) => event,
                    Err(error) => {
                        yield Err(restore_typed_graph_error(error));
                        return;
                    }
                };
                event.invocation_id.clone_from(&invocation_id);
                event.author.clone_from(&author);
                if event.provider_metadata.contains_key(INTERRUPT_METADATA_KEY)
                    && let Some(adapter) = printer_interrupts.as_ref()
                    && let Err(error) = adapter.enrich(&mut event).await
                {
                    yield Err(error);
                    return;
                }
                if let Some(adapter) = static_interrupts.as_ref()
                    && let Err(error) = adapter.enrich(&mut event).await
                {
                    yield Err(error);
                    return;
                }
                yield Ok(event);
            }
        }))
    }
}

#[cfg(test)]
#[allow(
    clippy::unnecessary_literal_bound,
    reason = "ADK trait stub methods return &str literals in tests."
)]
mod fu343_tests {
    use super::*;
    use adk_rust::graph::{GraphAgent, GraphError, Node, NodeContext, NodeOutput, StateSchema};
    use adk_rust::{Agent, Content, ReadonlyContext, Session, State};
    use std::collections::HashMap;
    use std::sync::Arc;

    struct FailingNode {
        id: &'static str,
        message: String,
    }

    #[async_trait]
    impl Node for FailingNode {
        fn name(&self) -> &str {
            self.id
        }

        async fn execute(&self, _: &NodeContext) -> Result<NodeOutput, GraphError> {
            Err(GraphError::NodeExecutionFailed {
                node: self.id.to_owned(),
                message: self.message.clone(),
            })
        }
    }

    struct StubSession;

    impl Session for StubSession {
        fn id(&self) -> &str {
            "fu343-session"
        }
        fn app_name(&self) -> &str {
            "elitea"
        }
        fn user_id(&self) -> &str {
            "user"
        }
        fn state(&self) -> &dyn State {
            &StubState
        }
        fn conversation_history(&self) -> Vec<Content> {
            Vec::new()
        }
    }

    struct StubState;

    impl State for StubState {
        fn get(&self, _: &str) -> Option<serde_json::Value> {
            None
        }
        fn set(&mut self, _: String, _: serde_json::Value) {}
        fn all(&self) -> HashMap<String, serde_json::Value> {
            HashMap::new()
        }
    }

    struct StubCtx {
        agent: Arc<dyn Agent>,
        content: Content,
        run_config: adk_rust::RunConfig,
    }

    impl ReadonlyContext for StubCtx {
        fn invocation_id(&self) -> &str {
            "inv-fu343"
        }
        fn agent_name(&self) -> &str {
            "graph-root"
        }
        fn user_id(&self) -> &str {
            "user"
        }
        fn app_name(&self) -> &str {
            "elitea"
        }
        fn session_id(&self) -> &str {
            "fu343-session"
        }
        fn branch(&self) -> &str {
            ""
        }
        fn user_content(&self) -> &Content {
            &self.content
        }
    }

    impl adk_rust::CallbackContext for StubCtx {
        fn artifacts(&self) -> Option<Arc<dyn adk_rust::Artifacts>> {
            None
        }
    }

    impl InvocationContext for StubCtx {
        fn agent(&self) -> Arc<dyn Agent> {
            Arc::clone(&self.agent)
        }
        fn memory(&self) -> Option<Arc<dyn adk_rust::Memory>> {
            None
        }
        fn session(&self) -> &dyn Session {
            &StubSession
        }
        fn run_config(&self) -> &adk_rust::RunConfig {
            &self.run_config
        }
        fn end_invocation(&self) {}
        fn ended(&self) -> bool {
            false
        }
    }

    #[tokio::test]
    async fn elitea_graph_agent_preserves_every_typed_model_gateway_class_without_echoing_text() {
        use crate::protocol::output::{RuntimeFailureKind, model_failure};
        let cases = [
            (
                "model_gateway.rate_limited",
                ErrorCategory::RateLimited,
                RuntimeFailureKind::ModelRateLimited,
            ),
            (
                "model_gateway.upstream_timeout",
                ErrorCategory::Timeout,
                RuntimeFailureKind::ModelTimeout,
            ),
            (
                "model_gateway.response_header_timeout",
                ErrorCategory::Timeout,
                RuntimeFailureKind::ModelTimeout,
            ),
            (
                "model_gateway.stream_idle_timeout",
                ErrorCategory::Timeout,
                RuntimeFailureKind::ModelTimeout,
            ),
            (
                "model_gateway.unavailable",
                ErrorCategory::Unavailable,
                RuntimeFailureKind::ModelUnavailable,
            ),
            (
                "model_gateway.transport",
                ErrorCategory::Unavailable,
                RuntimeFailureKind::ModelUnavailable,
            ),
            (
                "model_gateway.too_many_events",
                ErrorCategory::Internal,
                RuntimeFailureKind::ModelResponseInvalid,
            ),
        ];

        for (code, expected_category, expected_kind) in cases {
            let graph = GraphAgent::builder("graph-root")
                .state_schema(StateSchema::new())
                .node(FailingNode {
                    id: "nested_llm",
                    message: format!(
                        "Node 'inner' execution failed: {code}: SENTINEL-secret-provider-text"
                    ),
                })
                .edge(adk_rust::graph::START, "nested_llm")
                .edge("nested_llm", adk_rust::graph::END)
                .build()
                .expect("graph builds");
            let wrapped: Arc<dyn Agent> = Arc::new(EliteaGraphAgent::new(graph));
            let ctx: Arc<dyn InvocationContext> = Arc::new(StubCtx {
                agent: Arc::clone(&wrapped),
                content: Content::new("user").with_text("hi"),
                run_config: adk_rust::RunConfig::default(),
            });
            let mut stream = wrapped.run(ctx).await.expect("stream starts");
            let error = stream
                .next()
                .await
                .expect("yields error")
                .expect_err("typed model failure");
            assert_eq!(error.code, code, "{code}");
            assert_eq!(error.category, expected_category, "{code}");
            assert_eq!(error.component, ErrorComponent::Model, "{code}");
            assert!(
                !error.message.contains("SENTINEL"),
                "{code} echoed untrusted text"
            );
            assert_eq!(model_failure(Some(error.code)), expected_kind, "{code}");
        }
    }

    #[test]
    fn non_gateway_graph_errors_stay_untouched() {
        let error = AdkError::agent("Node 'step' execution failed: ordinary failure");
        let restored = restore_typed_graph_error(error);
        assert_eq!(restored.code, "agent.legacy");
        assert_eq!(restored.category, ErrorCategory::Internal);
    }

    #[test]
    fn fanout_graph_error_code_preserves_every_model_gateway_class() {
        let codes = [
            "model_gateway.rate_limited",
            "model_gateway.upstream_timeout",
            "model_gateway.unavailable",
            "model_gateway.too_many_events",
            "model_gateway.invalid_request",
            "model_gateway.unauthorized",
            "model_gateway.forbidden",
        ];
        for code in codes {
            let err = GraphError::NodeExecutionFailed {
                node: "branch".to_owned(),
                message: format!(
                    "Node 'nested' execution failed: {code}: SENTINEL-secret-provider-text"
                ),
            };
            assert_eq!(super::super::parallel::graph_error_code(&err), code);
        }
    }
}
