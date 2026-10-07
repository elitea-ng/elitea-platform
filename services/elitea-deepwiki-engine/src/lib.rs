//! The Rust-native `DeepWiki` engine (ADR-0026).
//!
//! It serves the engine sidecar protocol the Go sub-application host dials
//! over a Unix socket (`server`) with one of three runners: `unavailable`,
//! `fixture` and `native`, the engine itself, whose `generate_wiki` runs in
//! a worker child process (`worker`, `generate`).

pub mod ask;
pub mod cgroup;
pub mod config;
pub use elitea_engine_core::errors;
pub mod generate;
pub mod graph;
pub mod healthcheck;
pub use elitea_code_parsers as parsers;
pub use elitea_engine_core::pyjson;
pub use elitea_model_client as llm;
pub use elitea_repo_ingest as ingest;
pub mod runner;
pub mod server;
pub mod source;
pub mod storage;
pub mod structure;
pub mod wiki;
pub mod worker;

use config::{ConfigError, RunnerKind, Settings};
use runner::Runner;
use runner::fixture::FixtureRunner;
use runner::native::NativeRunner;

/// The runner the settings name.
///
/// # Errors
///
/// The native runner's refusal: no usable database URL, or no path to this
/// executable for its workers.
pub fn build_runner(settings: &Settings) -> Result<Runner, ConfigError> {
    Ok(match settings.runner {
        RunnerKind::Unavailable => Runner::Unavailable,
        RunnerKind::Fixture => Runner::Fixture(FixtureRunner {
            step: settings.fixture_step,
        }),
        RunnerKind::Native => Runner::Native(NativeRunner::new(settings.clone())?),
    })
}
