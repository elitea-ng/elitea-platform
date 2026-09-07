//! Elitea-owned graph compilation and durable node extensions.
//!
//! ADK-Rust owns graph execution. This module owns the stricter YAML contract,
//! tenant-safe integrations and durability that are not part of the upstream
//! `Checkpoint` model.

mod agent;
mod application;
#[cfg(test)]
mod application_tests;
pub(crate) mod compiler;
#[cfg(test)]
mod compiler_tests;
mod decision;
mod direct_tool;
#[cfg(test)]
mod direct_tool_tests;
mod hitl;
#[cfg(test)]
mod hitl_tests;
mod llm;
#[cfg(test)]
mod llm_tests;
mod node_events;
mod parallel;
#[cfg(test)]
mod parallel_tests;
mod printer;
#[cfg(test)]
mod printer_tests;
pub(crate) mod resume;
mod router;
#[cfg(test)]
mod routing_tests;
mod state_modifier;
#[cfg(test)]
mod state_modifier_tests;
pub(crate) mod turn_checkpointer;
mod yaml;

pub(crate) use agent::{
    EliteaGraphAgent, PIPELINE_COMPLETED_CONTENT, PIPELINE_COMPLETED_METADATA_KEY,
    PIPELINE_COMPLETED_METADATA_VALUE, pipeline_completed_event, pipeline_result_event,
};
pub(crate) use application::{
    ApplicationExecutionError, PIPELINE_APPLICATION_HITL_SCHEMA, PipelineApplicationResolver,
    PipelineApplicationSelection, ResolvedApplicationParticipant,
};
pub(crate) use direct_tool::{
    DirectToolExecutionError, DirectToolNodeKind, DirectToolSelection, PipelineDirectToolResolver,
    ResolvedDirectTool,
};
pub(crate) use llm::{
    LlmExecutionError, LlmExecutionInput, LlmNodeDefinition, PipelineLlmAgentBinding,
    PipelineLlmAgentFactory, PipelineLlmReplayEnvelope, PipelineToolGuard,
    prepare_pipeline_llm_replay,
};
pub(crate) use node_events::{
    PIPELINE_NODE_EVENT_SCOPE_STATE_KEY, PIPELINE_NODE_METADATA_KEY, PipelineNodeEventReceiver,
    PipelineNodeEventScope, PipelineNodeEventSender, PipelineNodeEventStreamingAgent,
    pipeline_node_event_channel,
};

pub use yaml::{
    ParallelBranchDefinition, ParallelConfigurationError, ParallelErrorPolicy,
    ParallelNodeDefinition, ParallelWaitPolicy,
};

pub(crate) use parallel::{
    ParallelActivation, ParallelChildCheckpoint, ParallelChildCheckpointerFactory,
};
pub(crate) use printer::{PRINTER_PAUSE_METADATA_KEY, PrinterPauseCatalog, PrinterPauseMetadata};
