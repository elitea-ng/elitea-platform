//! Benchmark-only entry points for FU-347 (5b-1 PR B):
//! - 128-node no-settings pipeline compile guard
//! - Settings wrapper overhead (p99 <= 50 us) on no-settings and deterministic paths.

use std::fmt::Write as _;
use std::sync::Arc;

use adk_rust::graph::{ExecutionConfig, MemoryCheckpointer, Node, NodeContext, State};
use elitea_agent_runtime::graph::node_settings::{
    DeterministicFamily, EXECUTED_ONCE_KEY, NodeSettings, OnNodeError,
};
use elitea_agent_runtime::graph::state_modifier::{StateModifierNode, StateModifierNodeDefinition};
use serde_json::json;

use super::compiler::{
    AdmittedNodeFamily, AdmittedNodeSettings, PipelineDefinition, PipelineNodeRuntimes,
};
use super::node_settings_runtime::SettingsNode;

/// Builds a valid 128-node State Modifier pipeline with no `settings:` blocks.
#[must_use]
pub fn build_128_node_no_settings_yaml() -> String {
    let mut yaml = String::from("state:\n  value: str\n  out: str\nentry_point: n0\nnodes:\n");
    for i in 0..128 {
        let next = if i + 1 == 128 {
            "END".to_owned()
        } else {
            format!("n{}", i + 1)
        };
        let _ = writeln!(
            yaml,
            "  - id: n{i}\n    type: state_modifier\n    template: '{{{{ value }}}}'\n    input: [value]\n    output: [out]\n    transition: {next}"
        );
    }
    yaml
}

/// Parses and compiles a YAML document with default runtimes (no settings).
///
/// # Errors
/// Returns an error string if parsing or compilation fails.
pub fn compile_pipeline(yaml: &str) -> Result<usize, String> {
    let def = PipelineDefinition::from_yaml(yaml).map_err(|e| e.to_string())?;
    let count = def.node_count();
    let _graph = def
        .compile_with_runtime(
            "bench-root",
            Arc::new(MemoryCheckpointer::new()),
            None,
            &PipelineNodeRuntimes::default(),
        )
        .map_err(|e| e.to_string())?;
    Ok(count)
}

/// Pre-built deterministic node bench harness (unwrapped, wrapped success,
/// wrapped continued failure, and wrapped `execute_once` skip).
pub struct DeterministicSettingsBench {
    unwrapped: StateModifierNode,
    wrapped_ok: SettingsNode<StateModifierNode>,
    wrapped_fail: SettingsNode<StateModifierNode>,
    wrapped_once: SettingsNode<StateModifierNode>,
    ok_ctx: NodeContext,
    fail_ctx: NodeContext,
    once_ctx: NodeContext,
}

impl DeterministicSettingsBench {
    /// Constructs the pre-parsed benchmark nodes and contexts outside the timed loop.
    ///
    /// # Errors
    /// Returns an error string if fixture construction fails.
    pub fn new() -> Result<Self, String> {
        let ok_def = StateModifierNodeDefinition::from_yaml(
            "id: shape\ntype: state_modifier\ntemplate: '{{ value }}'\ninput: [value]\noutput: [out]\ntransition: END\n",
        )
        .map_err(|e| e.to_string())?;
        let fail_def = StateModifierNodeDefinition::from_yaml(
            "id: shape\ntype: state_modifier\ntemplate: '{{ value | nosuchfilter }}'\ninput: [value]\noutput: [out]\ntransition: END\n",
        )
        .map_err(|e| e.to_string())?;

        let continue_settings = AdmittedNodeSettings {
            settings: NodeSettings {
                on_error: OnNodeError::Continue,
                always_output_data: true,
                ..NodeSettings::default()
            },
            family: AdmittedNodeFamily::Deterministic(DeterministicFamily::StateModifier),
            outputs: vec![("out".to_owned(), "str".to_owned())],
            router_default: None,
        };
        let once_settings = AdmittedNodeSettings {
            settings: NodeSettings {
                execute_once: true,
                ..NodeSettings::default()
            },
            family: AdmittedNodeFamily::Deterministic(DeterministicFamily::StateModifier),
            outputs: vec![("out".to_owned(), "str".to_owned())],
            router_default: None,
        };

        let mut base_state = State::new();
        base_state.insert("value".to_owned(), json!("hello"));
        let mut once_state = base_state.clone();
        once_state.insert(EXECUTED_ONCE_KEY.to_owned(), json!({"shape": {}}));

        Ok(Self {
            unwrapped: StateModifierNode::new(ok_def.clone()),
            wrapped_ok: SettingsNode::new(
                StateModifierNode::new(ok_def.clone()),
                continue_settings.clone(),
                Some(Arc::new("bench-exec".to_owned())),
            ),
            wrapped_fail: SettingsNode::new(
                StateModifierNode::new(fail_def),
                continue_settings,
                Some(Arc::new("bench-exec".to_owned())),
            ),
            wrapped_once: SettingsNode::new(
                StateModifierNode::new(ok_def),
                once_settings,
                Some(Arc::new("bench-exec".to_owned())),
            ),
            ok_ctx: NodeContext::new(base_state.clone(), ExecutionConfig::new("bench-thread"), 1),
            fail_ctx: NodeContext::new(base_state, ExecutionConfig::new("bench-thread"), 1),
            once_ctx: NodeContext::new(once_state, ExecutionConfig::new("bench-thread"), 2),
        })
    }

    /// Executes the unwrapped (no-settings) `StateModifierNode`.
    ///
    /// # Errors
    /// Returns an error string if execution fails.
    pub async fn run_unwrapped(&self) -> Result<usize, String> {
        self.unwrapped
            .execute(&self.ok_ctx)
            .await
            .map(|out| out.updates.len())
            .map_err(|e| e.to_string())
    }

    /// Executes `SettingsNode` on the success path.
    ///
    /// # Errors
    /// Returns an error string if execution fails.
    pub async fn run_wrapped_ok(&self) -> Result<usize, String> {
        self.wrapped_ok
            .execute(&self.ok_ctx)
            .await
            .map(|out| out.updates.len())
            .map_err(|e| e.to_string())
    }

    /// Executes `SettingsNode` on the continued failure path (`on_error: continue`).
    ///
    /// # Errors
    /// Returns an error string if execution fails.
    pub async fn run_wrapped_continue(&self) -> Result<usize, String> {
        self.wrapped_fail
            .execute(&self.fail_ctx)
            .await
            .map(|out| out.updates.len())
            .map_err(|e| e.to_string())
    }

    /// Executes `SettingsNode` on the `execute_once` revisit skip path.
    ///
    /// # Errors
    /// Returns an error string if execution fails.
    pub async fn run_wrapped_once_skip(&self) -> Result<usize, String> {
        self.wrapped_once
            .execute(&self.once_ctx)
            .await
            .map(|out| out.updates.len())
            .map_err(|e| e.to_string())
    }
}
