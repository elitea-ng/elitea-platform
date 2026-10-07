//! The Rust-native Inventory engine (ADR-0027) behind the sub-application
//! host's engine sidecar socket.
//!
//! The socket protocol, the probe and process tracing are the shared
//! `elitea-engine-sidecar` crate's; what is Inventory's is here:
//!
//! * [`tools`] — the families and tools the socket admits;
//! * [`fixture`] — the canned graph every Inventory fixture runner replays;
//! * [`runner`] — what answers a tool;
//! * [`graph`] — the knowledge graph, with the Python graph's semantics;
//! * [`store`] — the graph's PostgreSQL storage;
//! * [`ingest`] — a source's files into the graph;
//! * [`config`] — the `ELITEA_INVENTORY_*` settings.

pub mod config;
pub mod fixture;
pub mod graph;
pub mod ingest;
pub mod runner;
pub mod store;
pub mod tools;

use config::{ConfigError, RunnerKind, Settings};
use fixture::FixtureGraph;
use runner::{FixtureRunner, Runner};

/// The runner the settings name.
///
/// # Errors
///
/// The fixture graph cannot be read (the start fails rather than every tool).
pub fn build_runner(settings: &Settings) -> Result<Runner, ConfigError> {
    match settings.runner {
        RunnerKind::Unavailable => Ok(Runner::Unavailable),
        RunnerKind::Fixture => {
            let graph = FixtureGraph::load(settings.fixtures.as_deref())
                .map_err(|error| ConfigError(error.message))?;
            Ok(Runner::Fixture(FixtureRunner::new(
                graph,
                settings.fixture_step,
            )))
        }
    }
}
