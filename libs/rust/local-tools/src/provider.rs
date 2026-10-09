//! The local tools as the runtime sees them: adk [`Tool`]s in one
//! [`Toolset`], handed over by [`LocalToolProvider`], the desktop host's
//! [`ToolProvider`] (ADR-0029 decisions 2 and 4).
//!
//! The toolset is assembled per call from the session's workspace and
//! policy (see [`LocalSession::available_tools`]), so plan mode and policy
//! changes apply to the next model turn. Credentialed toolkits are not
//! here: they stay server-side and reach the desktop as remote tools
//! (decision 3), which a host composes with [`LocalToolProvider::with`].

use std::sync::Arc;

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use elitea_agent_runtime::host::{HostError, ToolProvider, ToolsetRequest};
use serde_json::Value;

use crate::session::{LocalSession, ToolSpec};

/// The toolset's name.
pub const TOOLSET_NAME: &str = "local";

/// One local tool.
pub struct LocalTool {
    spec: &'static ToolSpec,
    session: Arc<LocalSession>,
}

#[async_trait]
impl Tool for LocalTool {
    fn name(&self) -> &str {
        self.spec.name
    }

    fn description(&self) -> &str {
        self.spec.description
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some((self.spec.parameters)())
    }

    fn is_read_only(&self) -> bool {
        self.spec.kind.is_read_only()
    }

    fn is_concurrency_safe(&self) -> bool {
        self.spec.kind.is_read_only()
    }

    async fn execute(&self, ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        // Failures are results the model reads, not run failures.
        Ok(self
            .session
            .call(self.spec.name, ctx.function_call_id(), args)
            .await)
    }
}

/// The session's local tools.
pub struct LocalToolset {
    session: Arc<LocalSession>,
}

impl LocalToolset {
    #[must_use]
    pub fn new(session: Arc<LocalSession>) -> Self {
        Self { session }
    }

    /// The tools offered now.
    #[must_use]
    pub fn current_tools(&self) -> Vec<Arc<dyn Tool>> {
        self.session
            .available_tools()
            .into_iter()
            .map(|spec| {
                Arc::new(LocalTool {
                    spec,
                    session: self.session.clone(),
                }) as Arc<dyn Tool>
            })
            .collect()
    }
}

#[async_trait]
impl Toolset for LocalToolset {
    fn name(&self) -> &str {
        TOOLSET_NAME
    }

    async fn tools(&self, _ctx: Arc<dyn ReadonlyContext>) -> adk_core::Result<Vec<Arc<dyn Tool>>> {
        Ok(self.current_tools())
    }
}

/// The desktop host's [`ToolProvider`]: the local toolset, then whatever
/// other providers the host chains (remote toolkits).
pub struct LocalToolProvider {
    session: Arc<LocalSession>,
    others: Vec<Arc<dyn ToolProvider>>,
}

impl LocalToolProvider {
    #[must_use]
    pub fn new(session: Arc<LocalSession>) -> Self {
        Self {
            session,
            others: Vec::new(),
        }
    }

    /// Add another provider's toolsets after the local one.
    #[must_use]
    pub fn with(mut self, other: Arc<dyn ToolProvider>) -> Self {
        self.others.push(other);
        self
    }

    #[must_use]
    pub fn session(&self) -> &Arc<LocalSession> {
        &self.session
    }
}

#[async_trait]
impl ToolProvider for LocalToolProvider {
    async fn toolsets(&self, request: &ToolsetRequest) -> Result<Vec<Arc<dyn Toolset>>, HostError> {
        let mut toolsets: Vec<Arc<dyn Toolset>> =
            vec![Arc::new(LocalToolset::new(self.session.clone()))];
        for other in &self.others {
            toolsets.extend(other.toolsets(request).await?);
        }
        Ok(toolsets)
    }
}
