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
use adk_rust::{Agent, Content, Event, EventStream, InvocationContext};
use async_trait::async_trait;

pub(crate) const PIPELINE_COMPLETED_METADATA_KEY: &str = "elitea.pipeline.completed";
pub(crate) const PIPELINE_COMPLETED_METADATA_VALUE: &str = "v1";
pub(crate) const PIPELINE_COMPLETED_CONTENT: &str = "Pipeline completed.";

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
    fresh_run_checkpointer: Option<Arc<dyn Checkpointer>>,
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
            fresh_run_checkpointer: None,
        }
    }

    /// Declare that this invocation STARTS a run rather than continuing one,
    /// so the thread's previous checkpoints are discarded before it begins.
    ///
    /// **WHY A RUN THAT IS NOT TOLD THIS ANSWERS THE PREVIOUS QUESTION.** The
    /// graph's checkpoint thread is the CONVERSATION — ADK's `GraphAgent::run`
    /// builds `ExecutionConfig::new(ctx.session_id())`, and Elitea activates
    /// one checkpoint adapter per conversation thread — and ADK's executor
    /// opens EVERY run with `try_resume_from_checkpoint`: whatever checkpoint
    /// the thread already holds is restored (state, `step` AND
    /// `pending_nodes`) and the new input is merged on top. A run that
    /// finished leaves a checkpoint whose `pending_nodes` is EMPTY, so the
    /// executor's `while !self.pending_nodes.is_empty()` loop executes no node
    /// at all and the graph "completes" instantly, carrying the FIRST turn's
    /// terminal state into this turn's answer.
    ///
    /// Measured, not reasoned about: on the native runtime the second turn of
    /// a pipeline's test chat was admitted, streamed, and finalised an
    /// assistant row holding the FIRST turn's answer verbatim (CI run
    /// 34191944006, `chat.pipeline-execution.spec.ts`), and the state-modifier
    /// graph in this module's own tests reduced to the bare "Pipeline
    /// completed." marker with no node executed.
    ///
    /// It is NOT unconditional, and that is the whole reason it is a caller's
    /// declaration rather than a default: a HITL, printer or MCP-authorization
    /// resume must find the checkpoint its pause left behind, and clearing it
    /// there would turn every pause into a lost turn. Only the fresh-start arm
    /// passes this.
    ///
    /// Clearing an outstanding pause is the RIGHT outcome for the one case it
    /// happens in: a user who types a new question instead of answering the
    /// prompt has abandoned it, and the resume paths already refuse a decision
    /// whose checkpoint has gone (`PipelineResumeErrorCode::StaleDecision`)
    /// rather than answering the wrong turn.
    #[must_use]
    pub(crate) fn starting_a_fresh_run(mut self, checkpointer: Arc<dyn Checkpointer>) -> Self {
        self.fresh_run_checkpointer = Some(checkpointer);
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
        // BEFORE the graph reads it. A failure here fails the turn rather than
        // running anyway: running anyway is exactly how the previous turn's
        // answer is served as this one's.
        if let Some(checkpointer) = self.fresh_run_checkpointer.as_ref() {
            checkpointer
                .delete(context.session_id())
                .await
                .map_err(|_| {
                    adk_rust::AdkError::agent("the previous graph run could not be discarded")
                })?;
        }
        let events = self.graph.run(context).await?;
        Ok(Box::pin(async_stream::stream! {
            let mut events = events;
            while let Some(result) = events.next().await {
                let mut event = match result {
                    Ok(event) => event,
                    Err(error) => {
                        yield Err(error);
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
                yield Ok(event);
            }
        }))
    }
}
