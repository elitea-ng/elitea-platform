//! Elitea-owned graph compilation and durable node extensions.
//!
//! ADK-Rust owns graph execution. This module owns the stricter YAML contract,
//! tenant-safe integrations and durability that are not part of the upstream
//! `Checkpoint` model.
//!
//! The modules without cloud coupling live in `elitea-agent-runtime`
//! (`libs/rust/agent-runtime`, ADR-0029) and are re-exported here at their old
//! paths, so `super::printer::…` and `crate::agents::graph::yaml::…` resolve
//! unchanged.

mod agent;
mod application;
use elitea_agent_runtime::graph::application_activation;
#[cfg(test)]
mod application_tests;
mod code;
pub(crate) mod code_debug;
pub(crate) use code_debug::{
    CodeDebugAdmission, CodeDebugArtifactReference, CodeDebugArtifactSink, CodeDebugFailure,
};
pub(crate) use elitea_agent_runtime::graph::http_action;
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
mod compiler_identifier_tests;
#[cfg(test)]
mod compiler_limit_tests;
#[cfg(test)]
mod compiler_tests;
mod decision;
mod direct_tool;
#[cfg(test)]
mod direct_tool_tests;
pub(crate) mod fanout_control;
use elitea_agent_runtime::graph::hitl;
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
pub(crate) use elitea_agent_runtime::graph::node_recovery;
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
mod pipeline_result;
#[cfg(test)]
mod pipeline_result_graph_tests;
use elitea_agent_runtime::graph::printer;
#[cfg(test)]
mod printer_tests;
pub(crate) mod resume;
use elitea_agent_runtime::graph::router;
#[cfg(test)]
mod routing_tests;
use elitea_agent_runtime::graph::state_modifier;
pub(crate) mod static_pause;
#[cfg(test)]
mod static_pause_tests;
pub(crate) mod static_tool_pause;
pub(crate) use elitea_agent_runtime::graph::turn_checkpointer;
pub(crate) use elitea_agent_runtime::graph::yaml;

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

#[cfg(test)]
pub(crate) use parallel::{
    AdkParallelBranchRuntime, DurableParallelNode, PARALLEL_INTERRUPT_SCHEMA,
    PARALLEL_RESUME_STATE_KEY, ParallelBranchGraphFactory, ParallelBranchPause,
    ParallelBranchRuntime, ParallelBranchTerminal, ParallelDecision,
    ParallelOccurrenceCheckpointer, ParallelPauseCard, PreparedParallelActivation,
};
pub(crate) use parallel::{
    ParallelActivation, ParallelBranchExecution, ParallelCheckpointAppender,
    ParallelCheckpointAuthority, ParallelChildCheckpoint, ParallelChildCheckpointerFactory,
    ParallelChildOrigin,
};
pub(crate) use printer::{PRINTER_PAUSE_METADATA_KEY, PrinterPauseCatalog, PrinterPauseMetadata};

pub(super) use code_runtime::CodeSandboxRuntime;
