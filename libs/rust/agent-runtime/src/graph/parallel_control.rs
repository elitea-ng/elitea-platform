//! Branch cancellation keeps the exact parent identity and service authority.

use std::collections::HashMap;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

use adk_core::{
    Agent, Artifacts, CallbackContext, Content, InvocationContext, Memory, ReadonlyContext,
    RunConfig, SecretRequest, SharedState, State, ToolOutcome,
};
use async_trait::async_trait;
use serde_json::Value;

pub struct ParallelInvocationContext {
    parent: Arc<dyn InvocationContext>,
    cancelled: Arc<AtomicBool>,
}

impl ParallelInvocationContext {
    pub fn new(parent: Arc<dyn InvocationContext>, cancelled: Arc<AtomicBool>) -> Self {
        Self { parent, cancelled }
    }
}

#[async_trait]
impl ReadonlyContext for ParallelInvocationContext {
    fn invocation_id(&self) -> &str {
        self.parent.invocation_id()
    }
    fn agent_name(&self) -> &str {
        self.parent.agent_name()
    }
    fn user_id(&self) -> &str {
        self.parent.user_id()
    }
    fn app_name(&self) -> &str {
        self.parent.app_name()
    }
    fn session_id(&self) -> &str {
        self.parent.session_id()
    }
    fn branch(&self) -> &str {
        self.parent.branch()
    }
    fn user_content(&self) -> &Content {
        self.parent.user_content()
    }
    fn state(&self) -> Option<&dyn State> {
        self.parent.state()
    }
    fn try_app_name(&self) -> adk_core::Result<adk_core::AppName> {
        self.parent.try_app_name()
    }
    fn try_user_id(&self) -> adk_core::Result<adk_core::UserId> {
        self.parent.try_user_id()
    }
    fn try_session_id(&self) -> adk_core::Result<adk_core::SessionId> {
        self.parent.try_session_id()
    }
    fn try_invocation_id(&self) -> adk_core::Result<adk_core::InvocationId> {
        self.parent.try_invocation_id()
    }
    fn try_identity(&self) -> adk_core::Result<adk_core::AdkIdentity> {
        self.parent.try_identity()
    }
    fn try_execution_identity(&self) -> adk_core::Result<adk_core::ExecutionIdentity> {
        self.parent.try_execution_identity()
    }
}

#[async_trait]
impl CallbackContext for ParallelInvocationContext {
    fn artifacts(&self) -> Option<Arc<dyn Artifacts>> {
        self.parent.artifacts()
    }
    fn tool_outcome(&self) -> Option<ToolOutcome> {
        self.parent.tool_outcome()
    }
    fn tool_name(&self) -> Option<&str> {
        self.parent.tool_name()
    }
    fn tool_input(&self) -> Option<&Value> {
        self.parent.tool_input()
    }
    fn shared_state(&self) -> Option<Arc<SharedState>> {
        self.parent.shared_state()
    }
}

#[async_trait]
impl InvocationContext for ParallelInvocationContext {
    fn agent(&self) -> Arc<dyn Agent> {
        self.parent.agent()
    }
    fn memory(&self) -> Option<Arc<dyn Memory>> {
        self.parent.memory()
    }
    fn session(&self) -> &dyn adk_core::Session {
        self.parent.session()
    }
    fn run_config(&self) -> &RunConfig {
        self.parent.run_config()
    }
    fn end_invocation(&self) {
        self.parent.end_invocation();
    }
    fn ended(&self) -> bool {
        self.parent.ended()
    }
    fn is_cancelled(&self) -> bool {
        self.cancelled.load(Ordering::Acquire) || self.parent.is_cancelled()
    }
    fn user_scopes(&self) -> Vec<String> {
        self.parent.user_scopes()
    }
    fn request_metadata(&self) -> HashMap<String, Value> {
        self.parent.request_metadata()
    }
    fn authoritative_transfer_targets(&self) -> bool {
        self.parent.authoritative_transfer_targets()
    }
    fn delegation_depth(&self) -> u32 {
        self.parent.delegation_depth()
    }
    fn max_delegation_depth(&self) -> Option<u32> {
        self.parent.max_delegation_depth()
    }
    fn orchestration_root_invocation_id(&self) -> &str {
        self.parent.orchestration_root_invocation_id()
    }
    fn orchestration_edge_id(&self) -> Option<&str> {
        self.parent.orchestration_edge_id()
    }
    fn requires_tool_confirmation(&self, tool_name: &str) -> bool {
        self.parent.requires_tool_confirmation(tool_name)
    }
    async fn get_secret(&self, name: &str) -> adk_core::Result<Option<String>> {
        self.parent.get_secret(name).await
    }
    async fn get_secret_for(&self, request: &SecretRequest) -> adk_core::Result<Option<String>> {
        self.parent.get_secret_for(request).await
    }
}
