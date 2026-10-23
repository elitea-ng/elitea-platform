//! The engine sidecar: the Unix-socket server every engine behind the
//! sub-application host runs (ADR-0023 H2, ADR-0027).
//!
//! * [`socket`] — binding the socket a host dials (always built);
//! * [`cgroup`] — the container's memory limit and OOM count, for an engine
//!   that sizes or supervises a worker process (always built);
//! * [`server`] — `elitea.engine.v1.EngineService`, gRPC over the socket
//!   (#1359), over any [`server::Engine`] (feature `server`, on by default);
//! * [`client`] — a client of that service, for the probe and the engines'
//!   tests (feature `server`);
//! * [`healthcheck`] — the container probe for a distroless image, which
//!   has no shell and no curl (feature `server`);
//! * [`telemetry`] — stderr log lines, and OTLP span export under the
//!   worker's switches (feature `telemetry`, on by default).
//!
//! The two features exist for a sidecar that speaks another protocol over
//! its socket and must link no client at all (the document extractor,
//! `services/elitea-doc-extractor`, which reads untrusted documents): with
//! `default-features = false` this crate is the socket binder and the cgroup
//! reader, and nothing in its dependency graph can open a connection.

pub mod cgroup;
#[cfg(feature = "server")]
pub mod client;
#[cfg(feature = "server")]
pub mod healthcheck;
#[cfg(feature = "server")]
pub mod server;
pub mod socket;
#[cfg(feature = "telemetry")]
pub mod telemetry;

/// The generated `elitea.engine.v1` messages, service and client.
#[cfg(feature = "server")]
#[allow(
    clippy::all,
    clippy::pedantic,
    clippy::nursery,
    missing_docs,
    unreachable_pub
)]
pub mod proto {
    tonic::include_proto!("elitea.engine.v1");
}

#[cfg(feature = "server")]
pub use server::{Engine, ServeOptions, serve, serve_with_options, service};
pub use socket::bind;
