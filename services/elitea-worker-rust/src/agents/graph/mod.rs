//! Elitea-owned graph compilation and durable node extensions.
//!
//! ADK-Rust owns graph execution. This module owns the stricter YAML contract,
//! tenant-safe integrations and durability that are not part of the upstream
//! `Checkpoint` model.

mod agent;
mod application;
mod application_activation;
#[cfg(test)]
mod application_tests;
mod code;
pub(crate) mod code_debug;
pub(crate) mod http_action;
pub(crate) use code_debug::{
    CodeDebugAdmission, CodeDebugArtifactReference, CodeDebugArtifactSink, CodeDebugFailure,
};
mod code_remote;
mod code_result;
mod code_runtime;
pub(crate) mod code_trace;
mod code_workspace;
pub(crate) use code_remote::CodeRuntimeFactory;
mod code_state;
#[cfg(test)]
mod code_state_tests;
#[cfg(test)]
mod code_tests;
pub(crate) mod compiler;
#[cfg(test)]
mod compiler_tests;
mod decision;
mod direct_tool;
#[cfg(test)]
mod direct_tool_tests;
#[cfg(test)]
mod fan_in_tests;
mod hitl;
#[cfg(test)]
mod hitl_tests;
mod llm;
#[cfg(test)]
mod llm_tests;
pub(crate) mod map_authority;
pub(crate) mod map_reduce;
#[cfg(test)]
mod map_reduce_tests;
pub(crate) mod map_turn;
mod map_yaml;
mod node_events;
pub(crate) mod node_recovery;
#[cfg(test)]
mod node_recovery_compiler_tests;
pub(crate) mod node_recovery_owner;
pub(crate) mod node_recovery_receipt;
pub(crate) mod node_recovery_runtime;
pub(crate) use map_authority::MapCheckpointAuthority;
pub(crate) use map_reduce::{
    FrozenMapItem, MapActivation, MapChildCheckpoint, MapChildCheckpointerFactory,
    MapExecutionIdentity, MapWorkerKind,
};
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
pub(crate) mod static_pause;
#[cfg(test)]
mod static_pause_tests;
pub(crate) mod static_tool_pause;
pub(crate) mod turn_checkpointer;
pub(crate) mod yaml;

pub(crate) use agent::{
    EliteaGraphAgent, PIPELINE_COMPLETED_CONTENT, PIPELINE_COMPLETED_METADATA_KEY,
    PIPELINE_COMPLETED_METADATA_VALUE, PIPELINE_REUSED_RESULT_METADATA_KEY,
    pipeline_completed_event, pipeline_result_event,
};
pub(crate) use application::{
    ApplicationExecutionError, PIPELINE_APPLICATION_HITL_SCHEMA, PipelineApplicationResolver,
    PipelineApplicationSelection, ResolvedApplicationParticipant,
};
pub(crate) use direct_tool::{
    DirectToolExecutionError, DirectToolNodeKind, DirectToolSelection, PipelineDirectToolResolver,
    ResolvedDirectTool, scoped_pipeline_tool_context,
};
pub(crate) use llm::{
    LlmExecutionError, LlmExecutionInput, LlmNodeDefinition, LlmToolkitSelection,
    PipelineLlmAgentBinding, PipelineLlmAgentFactory, PipelineLlmReplayEnvelope,
    PipelineModelScope, PipelineToolGuard, prepare_pipeline_llm_replay,
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
    ParallelActivation, ParallelBranchExecution, ParallelCheckpointAppender,
    ParallelCheckpointAuthority, ParallelChildCheckpoint, ParallelChildCheckpointerFactory,
};
pub(crate) use printer::{PRINTER_PAUSE_METADATA_KEY, PrinterPauseCatalog, PrinterPauseMetadata};

pub(super) use code_runtime::CodeSandboxRuntime;
