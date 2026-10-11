//! Tool runners: what actually answers a tool on the sidecar socket.
//!
//! * [`Runner::Unavailable`] — the default. It **refuses** every tool with a
//!   `resource_not_found` error rather than returning an empty success. A
//!   runner that is not wired must look broken, not idle.
//! * [`Runner::Fixture`] — canned results with paced progress, for a stack
//!   that proves the host → socket → composition → upload path without a
//!   model or a git host. The browser journeys run against it.
//!
//! * [`Runner::Native`] — the Rust engine (ADR-0026): `generate_wiki` in a
//!   worker child process ([`native`]); `ask`, `deep_research` and
//!   `resolve_wiki` in this process, over PostgreSQL; the index deletions
//!   (`delete_wiki_index`, `delete_project_wikis`, [`maintenance`]) the same.

pub mod fixture;
pub mod maintenance;
pub mod native;

use crate::errors::{EngineError, ErrorType};
use serde_json::{Map, Value};

pub use elitea_engine_core::stream::{Context, Line, StopSignal};

/// The tools the sidecar serves. The Go host serves everything else itself.
pub const ENGINE_TOOLS: [&str; 6] = [
    "generate_wiki",
    "ask",
    "deep_research",
    "resolve_wiki",
    // The index deletions (ADR-0031 phase D0, issue #1243).
    "delete_wiki_index",
    "delete_project_wikis",
];

/// Reader-selected wiki pages (`context_paths`) and their pinned version.
pub const PATHS_PARAM: &str = "context_paths";
pub const VERSION_PARAM: &str = "context_wiki_version_id";
/// Reader-uploaded text files.
pub const EXTRA_CONTEXT_PARAM: &str = "extra_context";

/// The argument set a tool sees, after the attachment keys.
///
/// The Go host resolves `context_paths` and `extra_context` in front of its
/// tool table (`run/contextpaths.go`, `run/extracontext.go`) and REMOVES
/// both keys, so a request that came through the host arrives here with
/// nothing left to do; the version key is dropped as the Python sidecar's
/// `consume` drops it. A sidecar called directly with attachments still in
/// place is REFUSED rather than silently answering without them: the
/// engine has no artifact client, and the host is the one place that
/// resolves them.
///
/// # Errors
///
/// A `ValueError` (Python's `ContextRefused`) when an attachment reached
/// the engine unresolved.
pub fn prepare_arguments(
    tool: &str,
    mut arguments: Map<String, Value>,
) -> Result<Map<String, Value>, EngineError> {
    for key in [PATHS_PARAM, EXTRA_CONTEXT_PARAM] {
        let present = arguments.get(key).is_some_and(crate::source::py_truthy);
        if !present {
            continue;
        }
        if !matches!(tool, "ask" | "deep_research") {
            let what = if key == PATHS_PARAM { "pages" } else { "files" };
            return Err(EngineError::new(
                ErrorType::Value,
                format!("{key} is not supported by {tool}; attach {what} to ask or deep_research"),
            ));
        }
        return Err(EngineError::new(
            ErrorType::Value,
            format!(
                "{key} reached the engine unresolved; the sub-application host resolves it before this socket"
            ),
        ));
    }
    arguments.remove(PATHS_PARAM);
    arguments.remove(VERSION_PARAM);
    arguments.remove(EXTRA_CONTEXT_PARAM);
    Ok(arguments)
}

/// The runner a sidecar serves.
#[derive(Debug, Clone)]
pub enum Runner {
    Unavailable,
    Fixture(fixture::FixtureRunner),
    Native(native::NativeRunner),
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

    /// Run one tool with the keyword set the host derived.
    ///
    /// # Errors
    ///
    /// The tool's failure, or the stop line after a stop request.
    pub async fn run(
        &self,
        tool: &str,
        arguments: Map<String, Value>,
        context: &Context,
    ) -> Result<Value, EngineError> {
        match self {
            Self::Unavailable => Err(EngineError::new(
                ErrorType::FileNotFound,
                format!(
                    "No DeepWiki tool runner is configured, so '{tool}' cannot run. \
                     Set ELITEA_DEEPWIKI_RUNNER=fixture for canned results; the native \
                     analysis engine is not part of this build yet."
                ),
            )),
            Self::Fixture(runner) => runner.run(tool, arguments, context).await,
            Self::Native(runner) => runner.run(tool, arguments, context).await,
        }
    }

    /// Publish a completed generation's index. The fixture has none; the
    /// native worker publishes before its result line (ADR-0026 decision 5),
    /// so there is nothing left to do here.
    #[allow(clippy::unused_async)] // The Python sidecar's publish was async.
    pub async fn publish(&self, _result: &Value, _context: &Context) {}
}

/// The socket serves [`ENGINE_TOOLS`] through this runner; the shared
/// sidecar crate owns the protocol (ADR-0027).
impl elitea_engine_sidecar::Engine for Runner {
    fn runner_name(&self) -> &'static str {
        self.name()
    }

    fn serves(&self, tool: &str) -> bool {
        ENGINE_TOOLS.contains(&tool)
    }

    fn tools(&self) -> &'static [&'static str] {
        &ENGINE_TOOLS
    }

    async fn run(
        &self,
        tool: &str,
        arguments: Map<String, Value>,
        context: &Context,
    ) -> Result<Value, EngineError> {
        Runner::run(self, tool, arguments, context).await
    }

    async fn after_success(&self, tool: &str, result: &Value, context: &Context) {
        if tool == "generate_wiki" {
            self.publish(result, context).await;
        }
    }
}
