//! The Rust-native `DeepWiki` engine (ADR-0026).
//!
//! It serves the engine sidecar protocol the Go sub-application host dials
//! over a Unix socket (`server`). Phase 1 carries the protocol and the two
//! runners that need no engine: `unavailable` and `fixture`.

pub mod config;
pub mod errors;
pub mod graph;
pub mod healthcheck;
pub mod ingest;
pub mod parsers;
pub mod pyjson;
pub mod runner;
pub mod server;
pub mod source;

use config::{RunnerKind, Settings};
use runner::Runner;
use runner::fixture::FixtureRunner;

/// The runner the settings name.
#[must_use]
pub fn build_runner(settings: &Settings) -> Runner {
    match settings.runner {
        RunnerKind::Unavailable => Runner::Unavailable,
        RunnerKind::Fixture => Runner::Fixture(FixtureRunner {
            step: settings.fixture_step,
        }),
    }
}
