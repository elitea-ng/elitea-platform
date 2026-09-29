//! ADK Code node. Sandbox admission belongs to the invocation-owned runtime.
#![allow(dead_code)] // Production runtime configuration is composed separately.

use adk_rust::graph::{GraphError, Node, NodeContext, NodeOutput};
use async_trait::async_trait;
use ring::digest;
use std::{collections::BTreeMap, sync::Arc};

use super::code::{CodeLanguage, CodeNodeDefinition, CodeProvenance};
use super::code_result::project_code_receipt;
use super::code_state::CodeStateBoundary;

/// No Debug: source and selected state can contain user data.
pub(in crate::agents) struct CodeInvocation<'a> {
    pub(super) activation: [u8; 32],
    pub(super) language: CodeLanguage,
    pub(super) source: &'a str,
    pub(super) provenance: CodeProvenance,
    pub(super) input_json: Vec<u8>,
}

#[async_trait]
pub(in crate::agents) trait CodeSandboxRuntime: Send + Sync {
    /// Return the terminal receipt for this exact activation. Implementations
    /// must authorize each submission and reconcile pending jobs without minting
    /// a replacement identity. Failure/cancellation must return an error.
    async fn execute(&self, invocation: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError>;
}

pub(super) struct CodeNode {
    definition: CodeNodeDefinition,
    types: BTreeMap<String, String>,
    boundary: CodeStateBoundary,
    runtime: Arc<dyn CodeSandboxRuntime>,
}

impl CodeNode {
    pub(super) fn new(
        definition: CodeNodeDefinition,
        types: BTreeMap<String, String>,
        runtime: Arc<dyn CodeSandboxRuntime>,
    ) -> Result<Self, GraphError> {
        let boundary = CodeStateBoundary::new(&types).map_err(code_error)?;
        Ok(Self {
            definition,
            types,
            boundary,
            runtime,
        })
    }
}

#[async_trait]
impl Node for CodeNode {
    fn name(&self) -> &str {
        self.definition.id()
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let activation = activation(&self.definition, context)?;
        let source = self
            .definition
            .resolve_source(&context.state, &self.types)
            .map_err(code_error)?;
        let input_json = self
            .boundary
            .input_json(&context.state, self.definition.input_keys())
            .map_err(code_error)?;
        let receipt = self
            .runtime
            .execute(CodeInvocation {
                activation,
                language: self.definition.language(),
                source: source.source,
                provenance: source.provenance,
                input_json,
            })
            .await?;
        let updates = project_code_receipt(
            &receipt,
            &self.boundary,
            self.definition.output_keys(),
            self.definition.structured_output(),
        )
        .map_err(code_error)?;
        let mut output = NodeOutput::new();
        for (key, value) in updates {
            output = output.with_update(&key, value);
        }
        Ok(output)
    }
}

fn activation(
    definition: &CodeNodeDefinition,
    context: &NodeContext,
) -> Result<[u8; 32], GraphError> {
    let thread = &context.config.thread_id;
    if thread.is_empty() || thread.len() > 512 {
        return Err(code_error("Code node has no valid durable graph thread"));
    }
    let step = u64::try_from(context.step)
        .map_err(|_| code_error("Code activation step exceeds its durable range"))?;
    let config = definition.config_digest().map_err(code_error)?;
    let mut hash = digest::Context::new(&digest::SHA256);
    hash.update(b"elitea.graph.code.activation.v1\0");
    for field in [
        thread.as_bytes(),
        definition.id().as_bytes(),
        &step.to_be_bytes(),
        &config,
    ] {
        hash.update(&(field.len() as u64).to_be_bytes());
        hash.update(field);
    }
    let mut result = [0; 32];
    result.copy_from_slice(hash.finish().as_ref());
    Ok(result)
}

fn code_error(error: impl std::fmt::Display) -> GraphError {
    GraphError::Other(format!("graph.code.execution_failed: {error}"))
}

#[cfg(test)]
mod tests {
    use super::super::compiler::{PipelineDefinition, PipelineNodeRuntimes};
    use super::*;
    use adk_rust::graph::{ExecutionConfig, MemoryCheckpointer, State};
    use serde_json::json;
    use std::sync::Mutex;

    const YAML: &str =
        "id: run\ntype: code\ncode: '7'\ninput: [count]\noutput: [count]\ntransition: END\n";

    #[derive(Default)]
    struct Runtime {
        visits: Mutex<Vec<[u8; 32]>>,
        fail: bool,
    }
    #[async_trait]
    impl CodeSandboxRuntime for Runtime {
        async fn execute(&self, invocation: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
            assert_eq!(invocation.language, CodeLanguage::Python);
            assert_eq!(invocation.source, "7");
            assert_eq!(invocation.provenance, CodeProvenance::SavedLiteral);
            assert_eq!(
                serde_json::from_slice::<serde_json::Value>(&invocation.input_json).unwrap(),
                json!({"count":2})
            );
            self.visits.lock().unwrap().push(invocation.activation);
            if self.fail {
                return Err(GraphError::Other("sandbox job failed".into()));
            }
            Ok(
                serde_json::to_vec(&json!({"revision":1,"status":"completed","exit_code":0,
                "stdout":"{\"revision\":1,\"result\":7}","stderr":""}))
                .unwrap(),
            )
        }
    }
    fn context(thread: &str, step: usize) -> NodeContext {
        NodeContext::new(
            State::from([
                ("count".into(), json!(2)),
                ("session_id".into(), json!("private")),
            ]),
            ExecutionConfig::new(thread),
            step,
        )
    }
    fn node(runtime: Arc<Runtime>) -> CodeNode {
        CodeNode::new(
            CodeNodeDefinition::from_yaml(YAML).unwrap(),
            BTreeMap::from([("count".into(), "int".into())]),
            runtime,
        )
        .unwrap()
    }
    #[tokio::test]
    async fn recovered_visit_has_same_identity_but_new_step_or_thread_does_not() {
        let runtime = Arc::new(Runtime::default());
        let node = node(runtime.clone());
        for (thread, step) in [("root", 4), ("root", 4), ("root", 5), ("child", 4)] {
            let output = node.execute(&context(thread, step)).await.unwrap();
            assert_eq!(output.updates, State::from([("count".into(), json!(7))]));
        }
        let visits = runtime.visits.lock().unwrap();
        assert_eq!(visits[0], visits[1]);
        assert_ne!(visits[0], visits[2]);
        assert_ne!(visits[0], visits[3]);
    }
    #[tokio::test]
    async fn invalid_input_does_not_dispatch_and_runtime_failure_has_no_state_update() {
        let runtime = Arc::new(Runtime {
            fail: true,
            ..Default::default()
        });
        let node = node(runtime.clone());
        assert!(node.execute(&context("", 0)).await.is_err());
        assert!(runtime.visits.lock().unwrap().is_empty());
        assert!(node.execute(&context("root", 0)).await.is_err());
        assert_eq!(runtime.visits.lock().unwrap().len(), 1);
    }
    #[test]
    fn compiler_accepts_code_only_with_an_explicit_runtime_binding() {
        let yaml = format!(
            "state:\n  count: int\nentry_point: run\nnodes:\n  - {}",
            YAML.replace('\n', "\n    ")
        );
        let definition = PipelineDefinition::from_yaml(&yaml).unwrap();
        let runtime = PipelineNodeRuntimes::default().with_code(Arc::new(Runtime::default()));
        assert!(
            definition
                .compile_subgraph_with_runtime(Arc::new(MemoryCheckpointer::new()), &runtime)
                .is_ok()
        );
        assert!(
            definition
                .compile_subgraph_with_runtime(
                    Arc::new(MemoryCheckpointer::new()),
                    &PipelineNodeRuntimes::default()
                )
                .is_err()
        );
    }

    #[tokio::test]
    async fn compiled_graph_applies_code_result_and_stops_on_sandbox_failure() {
        let yaml = format!(
            "state:\n  count: int\nentry_point: run\nnodes:\n  - {}",
            YAML.replace('\n', "\n    ")
        );
        let definition = PipelineDefinition::from_yaml(&yaml).unwrap();
        for fail in [false, true] {
            let runtime = Arc::new(Runtime {
                fail,
                ..Default::default()
            });
            let graph = definition
                .compile_subgraph_with_runtime(
                    Arc::new(MemoryCheckpointer::new()),
                    &PipelineNodeRuntimes::default().with_code(runtime.clone()),
                )
                .unwrap();
            let result = graph
                .invoke(
                    State::from([("count".into(), json!(2))]),
                    ExecutionConfig::new("compiled-code"),
                )
                .await;
            if fail {
                assert!(result.is_err());
            } else {
                assert_eq!(result.unwrap()["count"], 7);
            }
            assert_eq!(runtime.visits.lock().unwrap().len(), 1);
        }
    }
    #[tokio::test]
    async fn lost_response_recovery_reuses_activation_and_terminal_checkpoint_skips_dispatch() {
        use adk_rust::graph::Checkpointer;
        use std::sync::atomic::{AtomicBool, Ordering};

        struct LostResponse {
            runtime: Runtime,
            started: tokio::sync::Notify,
            first: AtomicBool,
            receipt: Mutex<Option<Vec<u8>>>,
            activations: Mutex<Vec<[u8; 32]>>,
        }
        #[async_trait]
        impl CodeSandboxRuntime for LostResponse {
            async fn execute(&self, invocation: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
                self.activations.lock().unwrap().push(invocation.activation);
                if self.first.swap(false, Ordering::SeqCst) {
                    let receipt = self.runtime.execute(invocation).await?;
                    *self.receipt.lock().unwrap() = Some(receipt);
                    self.started.notify_one();
                    std::future::pending::<()>().await;
                    unreachable!();
                }
                Ok(self.receipt.lock().unwrap().clone().unwrap())
            }
        }
        let runtime = Arc::new(LostResponse {
            runtime: Runtime::default(),
            started: tokio::sync::Notify::new(),
            first: AtomicBool::new(true),
            receipt: Mutex::new(None),
            activations: Mutex::new(Vec::new()),
        });
        let definition = PipelineDefinition::from_yaml(&format!(
            "state:\n  count: int\nentry_point: run\nnodes:\n  - {}",
            YAML.replace('\n', "\n    ")
        ))
        .unwrap();
        let checkpoints = Arc::new(MemoryCheckpointer::new());
        let bindings = PipelineNodeRuntimes::default().with_code(runtime.clone());
        {
            let graph = definition
                .compile_subgraph_with_runtime(checkpoints.clone(), &bindings)
                .unwrap();
            let execution = graph.invoke(
                State::from([("count".into(), json!(2))]),
                ExecutionConfig::new("child-code"),
            );
            tokio::pin!(execution);
            tokio::select! {
                result = &mut execution => panic!("execution unexpectedly completed: {result:?}"),
                () = runtime.started.notified() => {}
            }
            // Drop the caller after the remote effect, before its receipt arrives.
        }
        assert_eq!(
            checkpoints.load("child-code").await.unwrap().unwrap().step,
            1
        );
        let recovered = definition
            .compile_subgraph_with_runtime(checkpoints.clone(), &bindings)
            .unwrap();
        assert_eq!(
            recovered
                .invoke(State::new(), ExecutionConfig::new("child-code"))
                .await
                .unwrap()["count"],
            7
        );
        let activations = runtime.activations.lock().unwrap().clone();
        assert_eq!(activations.len(), 2);
        assert_eq!(activations[0], activations[1]);
        assert_eq!(runtime.runtime.visits.lock().unwrap().len(), 1);
        assert!(
            checkpoints
                .load("child-code")
                .await
                .unwrap()
                .unwrap()
                .pending_nodes
                .is_empty()
        );
        recovered
            .invoke(State::new(), ExecutionConfig::new("child-code"))
            .await
            .unwrap();
        assert_eq!(runtime.activations.lock().unwrap().len(), 2);
    }
}
