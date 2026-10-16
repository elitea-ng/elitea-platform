//! The engine sidecar: the Unix-socket server every engine behind the
//! sub-application host runs (ADR-0023 H2, ADR-0027).
//!
//! * [`socket`] — binding the socket a host dials (always built);
//! * [`cgroup`] — the container's memory limit and OOM count, for an engine
//!   that sizes or supervises a worker process (always built);
//! * [`server`] — the NDJSON protocol the host's one engine client speaks,
//!   over any [`server::Engine`] (feature `server`, on by default);
//! * [`healthcheck`] — the container probe for a distroless image, which
//!   has no shell and no curl (feature `server`);
//! * [`telemetry`] — stderr log lines, and OTLP span export under the
//!   worker's switches (feature `telemetry`, on by default).
//!
//! The two features exist for a sidecar that speaks another protocol over
//! its socket and must link no HTTP client at all (the document extractor,
//! `services/elitea-doc-extractor`, which reads untrusted documents): with
//! `default-features = false` this crate is the socket binder and the cgroup
//! reader, and nothing in its dependency graph can open a connection.

pub mod cgroup;
#[cfg(feature = "server")]
pub mod healthcheck;
#[cfg(feature = "server")]
pub mod server;
pub mod socket;
#[cfg(feature = "telemetry")]
pub mod telemetry;

#[cfg(feature = "server")]
pub use server::{Engine, router, serve};
pub use socket::bind;
