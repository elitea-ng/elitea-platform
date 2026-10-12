//! ADK Code node. Sandbox admission belongs to the invocation-owned runtime.
#![allow(dead_code)] // Production runtime configuration is composed separately.

use adk_rust::graph::{GraphError, Node, NodeContext, NodeOutput};
use async_trait::async_trait;
use ring::digest;
use std::{collections::BTreeMap, sync::Arc};

use super::code::{CodeLanguage, CodeNodeDefinition, CodeProvenance};
use super::code_result::project_code_receipt;
use super::code_state::CodeStateBoundary;
use super::node_recovery::{NodeFailure, NodeFailureClass, ReplaySafety};
use super::node_recovery_runtime::{
    NodeAttemptAuthority, NodeAttemptBody, NodeAttemptReportedFailure,
};

#[path = "code_committed.rs"]
mod committed;
pub(super) use committed::CodeCommittedProjector;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum CodeAttemptPhase {
    Admission,
    Input,
    Preparation,
    Hydration,
    Dispatch,
    Observation,
    Projection,
}

/// Finite caller-safe facts. Owner error text never enters this type.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum CodePreparationFailure {
    Failed,
    Cancelled,
    Unconfirmed,
}

impl CodePreparationFailure {
    pub(crate) const fn code(self) -> &'static str {
        match self {
            Self::Failed => "pipeline.code_preparation_failed",
            Self::Cancelled => "pipeline.code_preparation_cancelled",
            Self::Unconfirmed => "pipeline.code_preparation_unconfirmed",
        }
    }
    pub(super) fn graph_error(self) -> GraphError {
        code_error(crate::protocol::output::model_failure(Some(self.code())).safe_message())
    }
}

fn terminal_failure_code(
    class: NodeFailureClass,
    preparation: Option<CodePreparationFailure>,
) -> Option<&'static str> {
    match class {
        NodeFailureClass::LeaseLost => None,
        NodeFailureClass::AuthenticationDenied
        | NodeFailureClass::AuthorizationDenied
        | NodeFailureClass::SensitiveRejected => Some("pipeline.code_authorization_failed"),
        NodeFailureClass::Cancelled if preparation.is_none() => Some("pipeline.code_cancelled"),
        NodeFailureClass::DependencyUnavailable
        | NodeFailureClass::RateLimited
        | NodeFailureClass::AttemptTimeout
        | NodeFailureClass::WorkerInterrupted
        | NodeFailureClass::InvalidConfiguration
        | NodeFailureClass::InvalidInput
        | NodeFailureClass::InvalidResult
        | NodeFailureClass::ModelOutputIncomplete
        | NodeFailureClass::Cancelled
        | NodeFailureClass::Unknown => {
            Some(preparation.map_or("pipeline.code_failed", CodePreparationFailure::code))
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct CodeAttemptFailure {
    pub(crate) phase: CodeAttemptPhase,
    pub(crate) failure: NodeFailure,
    // Owning call detail; generic graph recovery still uses the whole dispatch.
    pub(crate) platform_call_effect: Option<[u8; 32]>,
    pub(crate) preparation: Option<CodePreparationFailure>,
}

impl CodeAttemptFailure {
    pub(crate) const fn new(
        phase: CodeAttemptPhase,
        class: NodeFailureClass,
        replay: ReplaySafety,
    ) -> Self {
        Self {
            phase,
            failure: NodeFailure { class, replay },
            platform_call_effect: None,
            preparation: None,
        }
    }
    pub(crate) fn with_preparation_failure(mut self, failure: CodePreparationFailure) -> Self {
        self.preparation = Some(failure);
        self
    }
    pub(crate) fn with_platform_call_effect(mut self, effect: Option<[u8; 32]>) -> Self {
        self.platform_call_effect = effect;
        self
    }
}

/// No Debug: source and selected state can contain user data.
pub(in crate::agents) struct CodeInvocation<'a> {
    pub(super) activation: [u8; 32],
    pub(super) language: CodeLanguage,
    pub(super) platform_client: bool,
    pub(super) source: &'a str,
    pub(super) dependencies_toml: Option<&'a str>,
    pub(super) provenance: CodeProvenance,
    pub(super) input_json: Vec<u8>,
    pub(super) trace: Option<super::code_trace::CodeTraceContext>,
    pub(super) original: Option<super::code_workspace::CodeOriginalDeclaration>,
    pub(super) debug: Option<super::code_debug::CodeDebugSelection>,
    pub(super) workspace: Option<super::code_workspace::CodeWorkspaceInvocation>,
}

#[async_trait]
pub(in crate::agents) trait CodeSandboxRuntime: Send + Sync {
    /// Bind only the actual Main registered saved family at the compiler seam.
    /// Default runtimes cannot acquire scoped authority from a child definition.
    fn bind_saved_child_scope(
        &self,
        _provider: Arc<
            dyn crate::agents::pipeline::saved_child_scope_provider::SavedChildScopeProvider,
        >,
    ) -> Result<Arc<dyn CodeSandboxRuntime>, GraphError> {
        Err(code_error("Scoped Code runtime authority is unavailable."))
    }
    /// Return the terminal receipt for this exact activation. Implementations
    /// must authorize each submission and reconcile pending jobs without minting
    /// a replacement identity. Failure/cancellation must return an error.
    async fn execute(&self, invocation: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError>;

    /// Use typed facts. The default cannot establish safe replay from `GraphError` text.
    async fn execute_attempt(
        &self,
        invocation: CodeInvocation<'_>,
        authority: &NodeAttemptAuthority,
    ) -> Result<Vec<u8>, CodeAttemptFailure> {
        self.execute(invocation).await.map_err(|_| {
            CodeAttemptFailure::new(
                CodeAttemptPhase::Observation,
                NodeFailureClass::Unknown,
                ReplaySafety::UnknownExternalEffect {
                    effect_id: authority.dispatch_activation(),
                },
            )
        })
    }
}

pub(super) struct CodeNode {
    definition: CodeNodeDefinition,
    types: BTreeMap<String, String>,
    boundary: CodeStateBoundary,
    runtime: Arc<dyn CodeSandboxRuntime>,
    events: Option<super::node_events::PipelineNodeEventSender>,
}

impl CodeNode {
    pub(super) fn new(
        definition: CodeNodeDefinition,
        types: BTreeMap<String, String>,
        runtime: Arc<dyn CodeSandboxRuntime>,
    ) -> Result<Self, GraphError> {
        definition
            .validate_source_types(&types)
            .map_err(code_error)?;
        let boundary = CodeStateBoundary::new(&types).map_err(code_error)?;
        Ok(Self {
            definition,
            types,
            boundary,
            runtime,
            events: None,
        })
    }
    pub(super) fn with_events(
        mut self,
        events: Option<super::node_events::PipelineNodeEventSender>,
    ) -> Self {
        self.events = events;
        self
    }
}

#[async_trait]
impl Node for CodeNode {
    fn name(&self) -> &str {
        self.definition.id()
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let result = self.execute_code(context).await;
        if let Err(error) = &result {
            tracing::error!(node_id = self.definition.id(), error_code = "pipeline.code_failed", failure_reason = %error, "pipeline Code node failed");
            if let Some(events) = &self.events {
                events
                    .send_execution_failure("pipeline.code_failed")
                    .await
                    .map_err(|_| code_error("The pipeline failure channel closed."))?;
            }
        }
        result
    }
}

impl CodeNode {
    async fn execute_code(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let activation = activation(&self.definition, context)?;
        let source = self
            .definition
            .resolve_source(&context.state, &self.types)
            .map_err(code_error)?;
        let input_json = self
            .boundary
            .input_json(&context.state, self.definition.input_keys())
            .map_err(code_error)?;
        let trace = self
            .events
            .as_ref()
            .map(|sender| {
                super::code_trace::CodeTraceContext::new(
                    self.definition.id(),
                    self.definition.language(),
                    &activation,
                    context,
                    sender.clone(),
                )
            })
            .transpose()?;
        let receipt = self
            .runtime
            .execute(CodeInvocation {
                activation,
                language: self.definition.language(),
                platform_client: self.definition.platform_client(),
                source: source.source(),
                dependencies_toml: self.definition.dependencies_toml(),
                provenance: source.provenance(),
                input_json,
                trace,
                original: self
                    .definition
                    .original_declaration(context)
                    .map_err(code_error)?,
                debug: self
                    .definition
                    .debug_selection(context)
                    .map_err(code_error)?,
                workspace: self
                    .definition
                    .workspace_selection(context)
                    .map_err(code_error)?,
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

#[async_trait]
impl NodeAttemptBody for CodeNode {
    fn legacy_dispatch_identity(
        &self,
        context: &NodeContext,
    ) -> Result<Option<[u8; 32]>, GraphError> {
        activation(&self.definition, context).map(Some)
    }

    async fn execute_attempt(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
    ) -> Result<NodeOutput, NodeFailure> {
        self.execute_attempt_reported(context, authority)
            .await
            .map_err(|failure| failure.failure)
    }

    async fn execute_attempt_reported(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
    ) -> Result<NodeOutput, NodeAttemptReportedFailure> {
        let reject = |class| NodeAttemptReportedFailure {
            failure: NodeFailure {
                class,
                replay: ReplaySafety::NoExternalEffect,
            },
            terminal_code: terminal_failure_code(class, None),
        };
        if !authority.matches(
            self.definition.id(),
            self.definition.validated_digest(),
            context.step,
        ) {
            return Err(reject(NodeFailureClass::AuthorizationDenied));
        }
        let source = self
            .definition
            .resolve_source(&context.state, &self.types)
            .map_err(|_| reject(NodeFailureClass::InvalidInput))?;
        let input_json = self
            .boundary
            .input_json(&context.state, self.definition.input_keys())
            .map_err(|_| reject(NodeFailureClass::InvalidInput))?;
        let activation = authority.dispatch_activation();
        let trace = self
            .events
            .as_ref()
            .map(|sender| {
                super::code_trace::CodeTraceContext::new(
                    self.definition.id(),
                    self.definition.language(),
                    &activation,
                    context,
                    sender.clone(),
                )
            })
            .transpose()
            .map_err(|_| reject(NodeFailureClass::InvalidInput))?;
        let receipt = self
            .runtime
            .execute_attempt(
                CodeInvocation {
                    activation,
                    language: self.definition.language(),
                    platform_client: self.definition.platform_client(),
                    source: source.source(),
                    dependencies_toml: self.definition.dependencies_toml(),
                    provenance: source.provenance(),
                    input_json,
                    trace,
                    original: self
                        .definition
                        .original_declaration(context)
                        .map_err(|_| reject(NodeFailureClass::InvalidConfiguration))?,
                    debug: self
                        .definition
                        .debug_selection(context)
                        .map_err(|_| reject(NodeFailureClass::InvalidConfiguration))?,
                    workspace: self
                        .definition
                        .workspace_selection(context)
                        .map_err(|_| reject(NodeFailureClass::InvalidConfiguration))?,
                },
                authority,
            )
            .await
            .map_err(|failure| NodeAttemptReportedFailure {
                failure: failure.failure,
                terminal_code: terminal_failure_code(failure.failure.class, failure.preparation),
            })?;
        let updates = project_code_receipt(
            &receipt,
            &self.boundary,
            self.definition.output_keys(),
            self.definition.structured_output(),
        )
        .map_err(|_| NodeAttemptReportedFailure {
            failure: NodeFailure {
                class: NodeFailureClass::InvalidResult,
                replay: ReplaySafety::CompletedExternalEffect {
                    receipt_id: authority.dispatch_activation(),
                },
            },
            terminal_code: terminal_failure_code(NodeFailureClass::InvalidResult, None),
        })?;
        let mut output = NodeOutput::new();
        for (key, value) in updates {
            output = output.with_update(&key, value);
        }
        Ok(output)
    }

    async fn report_terminal_failure(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
        code: &'static str,
    ) -> Result<(), GraphError> {
        tracing::error!(
            node_id = self.definition.id(),
            graph_thread_id = context.config.thread_id,
            activation_id = %crate::sandbox::code_recovery::hex(&authority.dispatch_activation()),
            attempt = authority.attempt(),
            error_code = code,
            "pipeline Code node stopped"
        );
        if let Some(events) = &self.events {
            events
                .send_execution_failure(code)
                .await
                .map_err(|_| code_error("The pipeline failure channel closed."))?;
        }
        Ok(())
    }

    async fn report_restored_terminal_failure(
        &self,
        context: &NodeContext,
        class: NodeFailureClass,
    ) -> Result<(), GraphError> {
        if let Some(code) = terminal_failure_code(class, None) {
            tracing::error!(
                node_id = self.definition.id(),
                graph_thread_id = context.config.thread_id,
                error_code = code,
                "persisted pipeline Code node failure restored"
            );
            if let Some(events) = &self.events {
                events
                    .send_execution_failure(code)
                    .await
                    .map_err(|_| code_error("The pipeline failure channel closed."))?;
            }
        }
        Ok(())
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
            assert_eq!(invocation.dependencies_toml, None);
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
    #[test]
    fn terminal_categories_use_typed_facts_and_keep_cancellation_distinct() {
        for class in [
            NodeFailureClass::AuthenticationDenied,
            NodeFailureClass::AuthorizationDenied,
            NodeFailureClass::SensitiveRejected,
        ] {
            assert_eq!(
                terminal_failure_code(class, None),
                Some("pipeline.code_authorization_failed")
            );
            assert_eq!(
                terminal_failure_code(class, Some(CodePreparationFailure::Unconfirmed)),
                Some("pipeline.code_authorization_failed")
            );
        }
        assert_eq!(
            terminal_failure_code(NodeFailureClass::Cancelled, None),
            Some("pipeline.code_cancelled")
        );
        assert_eq!(
            terminal_failure_code(
                NodeFailureClass::Cancelled,
                Some(CodePreparationFailure::Cancelled)
            ),
            Some("pipeline.code_preparation_cancelled")
        );
        assert_eq!(
            terminal_failure_code(NodeFailureClass::LeaseLost, None),
            None
        );
        assert_eq!(
            terminal_failure_code(
                NodeFailureClass::LeaseLost,
                Some(CodePreparationFailure::Failed)
            ),
            None
        );
        for class in [
            NodeFailureClass::AttemptTimeout,
            NodeFailureClass::Unknown,
            NodeFailureClass::InvalidInput,
        ] {
            assert_eq!(
                terminal_failure_code(class, None),
                Some("pipeline.code_failed")
            );
        }
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
    async fn rust_dependencies_are_saved_metadata_and_not_selected_state_input() {
        const DECLARATION: &str = "[dependencies]\nregex = \"1\"\n";
        struct DependencyRuntime;
        #[async_trait]
        impl CodeSandboxRuntime for DependencyRuntime {
            async fn execute(&self, invocation: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
                assert_eq!(invocation.language, CodeLanguage::Rust);
                assert_eq!(invocation.source, "7");
                assert_eq!(invocation.dependencies_toml, Some(DECLARATION));
                assert_eq!(invocation.provenance, CodeProvenance::SavedLiteral);
                assert_eq!(
                    serde_json::from_slice::<serde_json::Value>(&invocation.input_json).unwrap(),
                    json!({"count": 2})
                );
                Ok(
                    serde_json::to_vec(&json!({"revision":1,"status":"completed","exit_code":0,
                    "stdout":"{\"revision\":1,\"result\":7}","stderr":""}))
                    .unwrap(),
                )
            }
        }
        let definition = CodeNodeDefinition::from_yaml(&format!(
            "{YAML}language: rust\ndependencies: {}\n",
            serde_json::to_string(DECLARATION).unwrap()
        ))
        .unwrap();
        let node = CodeNode::new(
            definition,
            BTreeMap::from([("count".into(), "int".into())]),
            Arc::new(DependencyRuntime),
        )
        .unwrap();
        let mut invocation_context = context("root", 4);
        invocation_context
            .state
            .insert("dependencies".into(), json!("private state value"));
        let output = node.execute(&invocation_context).await.unwrap();
        assert_eq!(output.updates, State::from([("count".into(), json!(7))]));
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
    #[tokio::test]
    async fn sandbox_failure_reaches_runner_with_safe_code() {
        use super::super::{
            EliteaGraphAgent,
            node_events::{PipelineNodeEventStreamingAgent, pipeline_node_event_channel},
        };
        use crate::agents::runtime::NativeAgentInvocation;
        use adk_rust::runner::Runner;
        use adk_rust::session::{CreateRequest, InMemorySessionService, SessionService};
        use adk_rust::{Content, SessionId, UserId};
        let definition = PipelineDefinition::from_yaml("state:\n  count: {type: int, value: 2}\nentry_point: run\nnodes:\n  - id: run\n    type: code\n    code: '7'\n    input: [count]\n    output: [count]\n    transition: END\n").unwrap();
        let runtime = Arc::new(Runtime {
            fail: true,
            ..Default::default()
        });
        let (sender, receiver) = pipeline_node_event_channel();
        let graph = definition
            .compile_with_runtime(
                "code-failure",
                Arc::new(MemoryCheckpointer::new()),
                None,
                &PipelineNodeRuntimes::default()
                    .with_code(runtime.clone())
                    .with_events(sender),
            )
            .unwrap();
        let sessions = Arc::new(InMemorySessionService::new());
        sessions
            .create(CreateRequest {
                app_name: "elitea".into(),
                user_id: "user-1".into(),
                session_id: Some("code-failure-thread".into()),
                state: std::collections::HashMap::default(),
            })
            .await
            .unwrap();
        let agent =
            PipelineNodeEventStreamingAgent::new(Arc::new(EliteaGraphAgent::new(graph)), receiver);
        let runner = Runner::builder()
            .app_name("elitea")
            .agent(Arc::new(agent))
            .session_service(sessions)
            .build()
            .unwrap();
        let mut running = NativeAgentInvocation::new(
            runner,
            UserId::new("user-1").unwrap(),
            SessionId::new("code-failure-thread").unwrap(),
            Content::new("user").with_text("run"),
        )
        .start()
        .unwrap();
        let error = loop {
            match running.next_event().await {
                Err(error) => break error,
                Ok(Some(_)) => {}
                Ok(None) => panic!("failed code cannot complete"),
            }
        };
        assert_eq!(error.upstream_code(), Some("pipeline.code_failed"));
        let kind = crate::protocol::output::model_failure(error.upstream_code());
        assert_eq!(
            kind,
            crate::protocol::output::RuntimeFailureKind::PipelineCodeFailed
        );
        assert!(kind.safe_message().contains("Code node"));
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
    #[derive(Default)]
    struct TemplateRuntime {
        visits: Mutex<Vec<[u8; 32]>>,
    }
    #[async_trait]
    impl CodeSandboxRuntime for TemplateRuntime {
        async fn execute(&self, invocation: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
            assert_eq!(invocation.source, "result = 7");
            assert_eq!(invocation.provenance, CodeProvenance::StateTemplate);
            assert_eq!(
                serde_json::from_slice::<serde_json::Value>(&invocation.input_json).unwrap(),
                json!({"count": 2})
            );
            self.visits.lock().unwrap().push(invocation.activation);
            Ok(
                serde_json::to_vec(&json!({"revision":1,"status":"completed","exit_code":0,
                "stdout":"{\"revision\":1,\"result\":7}","stderr":""}))
                .unwrap(),
            )
        }
    }

    fn template_pipeline_yaml(source_type: &str) -> String {
        format!(
            "state:\n  count: int\n  source: {source_type}\nentry_point: run\nnodes:\n  - id: run\n    type: code\n    code: {{type: fstring, value: 'result = {{source}}'}}\n    input: [count]\n    output: [count]\n    transition: END\n"
        )
    }

    #[tokio::test]
    async fn compiled_template_resolves_before_runtime_and_keeps_dynamic_provenance() {
        let runtime = Arc::new(TemplateRuntime::default());
        for (kind, source) in [("str", json!("7")), ("int", json!(7))] {
            let definition = PipelineDefinition::from_yaml(&template_pipeline_yaml(kind)).unwrap();
            let graph = definition
                .compile_subgraph_with_runtime(
                    Arc::new(MemoryCheckpointer::new()),
                    &PipelineNodeRuntimes::default().with_code(runtime.clone()),
                )
                .unwrap();
            let state = State::from([("count".into(), json!(2)), ("source".into(), source)]);
            let output = graph
                .invoke(
                    state,
                    ExecutionConfig::new(&format!("compiled-template-{kind}")),
                )
                .await
                .unwrap();
            assert_eq!(output["count"], 7);
        }
        assert_eq!(runtime.visits.lock().unwrap().len(), 2);
    }

    #[test]
    fn compiler_binding_rejects_undeclared_or_unsupported_template_fields_before_runtime() {
        let runtime = Arc::new(TemplateRuntime::default());
        for yaml in [
            template_pipeline_yaml("float"),
            template_pipeline_yaml("str").replace("  source: str\n", ""),
        ] {
            let definition = PipelineDefinition::from_yaml(&yaml).unwrap();
            assert!(
                definition
                    .compile_subgraph_with_runtime(
                        Arc::new(MemoryCheckpointer::new()),
                        &PipelineNodeRuntimes::default().with_code(runtime.clone()),
                    )
                    .is_err()
            );
        }
        assert!(runtime.visits.lock().unwrap().is_empty());
    }

    #[tokio::test]
    async fn invalid_template_values_fail_before_runtime_dispatch() {
        let runtime = Arc::new(TemplateRuntime::default());
        let definition = CodeNodeDefinition::from_yaml("id: run\ntype: code\ncode: {type: fstring, value: '{source}'}\ninput: [count]\noutput: [count]\ntransition: END\n").unwrap();
        let node = CodeNode::new(
            definition,
            BTreeMap::from([
                ("count".into(), "int".into()),
                ("source".into(), "str".into()),
            ]),
            runtime.clone(),
        )
        .unwrap();
        for source in [
            None,
            Some(json!(7)),
            Some(json!("")),
            Some(json!("PRIVATE_MARKER\0")),
            Some(json!("x".repeat(256 * 1024 + 1))),
        ] {
            let mut invocation = context("invalid-template", 1);
            if let Some(source) = source {
                invocation.state.insert("source".into(), source);
            }
            let error = node.execute(&invocation).await.err().unwrap();
            assert!(!error.to_string().contains("PRIVATE_MARKER"));
        }
        assert!(runtime.visits.lock().unwrap().is_empty());
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
