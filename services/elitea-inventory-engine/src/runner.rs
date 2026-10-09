//! What answers a tool on the socket.
//!
//! * [`Runner::Unavailable`] — the default. It REFUSES every tool rather than
//!   answering an empty success: a runner that is not wired must look broken,
//!   not idle (the rule both Python sidecars and the `DeepWiki` engine follow).
//! * [`Runner::Fixture`] — the canned graph (`crate::fixture`), with paced
//!   progress, for a stack that proves the host → socket → composition →
//!   upload path with no repository and no model.
//!
//! * [`Runner::Native`] — the engine itself (`crate::native`).

use crate::fixture::{self, FixtureGraph};
use crate::tools;
use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::stream::Context;
use serde_json::{Map, Value, json};
use std::sync::Arc;
use std::time::Duration;

/// The fixture runner: the graph, loaded once, and the pause per step.
#[derive(Debug, Clone)]
pub struct FixtureRunner {
    graph: Arc<FixtureGraph>,
    step: Duration,
}

impl FixtureRunner {
    /// A runner over `graph`, pausing `step` between progress lines.
    #[must_use]
    pub fn new(graph: FixtureGraph, step: Duration) -> Self {
        Self {
            graph: Arc::new(graph),
            step,
        }
    }

    async fn run(
        &self,
        tool: &str,
        arguments: &Map<String, Value>,
        context: &Context,
    ) -> Result<Value, EngineError> {
        let family = arguments
            .get("family")
            .and_then(Value::as_str)
            .filter(|name| !name.is_empty())
            .unwrap_or("inventory");
        let Some(table) = tools::family(family) else {
            return Err(EngineError::new(
                ErrorType::Value,
                format!("Unknown toolkit: {family}. Expected: inventory or inventory_search"),
            ));
        };
        if !table.contains(&tool) {
            let mut available: Vec<&str> = table.to_vec();
            available.sort_unstable();
            return Err(EngineError::new(
                ErrorType::Value,
                format!("Unknown tool: {tool}. Available: {}", available.join(", ")),
            ));
        }
        for step in [
            format!("Received {tool}"),
            "Reading the graph".to_owned(),
            "Done".to_owned(),
        ] {
            context.checkpoint()?;
            context.thinking(step);
            context.pause(self.step).await;
        }
        let empty = Map::new();
        let params = arguments
            .get("params")
            .and_then(Value::as_object)
            .unwrap_or(&empty);
        let Some(handler) = fixture::handler(tool) else {
            // Admitted by the table above with nothing canned for it: said
            // as exactly that, never a generic success.
            return Ok(json!({
                "success": false,
                "error": format!("the fixture runner has no canned answer for '{tool}'; run this deployment against the engine"),
                "error_category": "resource_not_found",
                "artifacts": [],
            }));
        };
        let mut result = handler(&self.graph, params);
        if let Value::Object(fields) = &mut result
            && !fields.contains_key("artifacts")
        {
            fields.insert("artifacts".to_owned(), json!([]));
        }
        Ok(result)
    }
}

/// The runner a sidecar serves.
#[derive(Debug, Clone)]
pub enum Runner {
    Unavailable,
    Fixture(FixtureRunner),
    Native(crate::native::NativeRunner),
}

impl Runner {
    /// The name `GET /engine/health` reports.
    #[must_use]
    pub fn name(&self) -> &'static str {
        match self {
            Self::Unavailable => "unavailable",
            Self::Fixture(_) => "fixture",
            Self::Native(_) => "native",
        }
    }
}

impl elitea_engine_sidecar::Engine for Runner {
    fn runner_name(&self) -> &'static str {
        self.name()
    }

    fn serves(&self, tool: &str) -> bool {
        tools::serves(tool)
    }

    async fn run(
        &self,
        tool: &str,
        arguments: Map<String, Value>,
        context: &Context,
    ) -> Result<Value, EngineError> {
        match self {
            Self::Unavailable => Err(EngineError::new(
                ErrorType::FileNotFound,
                format!(
                    "No Inventory tool runner is configured, so '{tool}' cannot run. \
                     Set ELITEA_INVENTORY_RUNNER=fixture for canned results; the native \
                     analysis engine is not part of this build yet."
                ),
            )),
            Self::Fixture(runner) => runner.run(tool, &arguments, context).await,
            Self::Native(runner) => runner.run(tool, &arguments, context).await,
        }
    }
}
