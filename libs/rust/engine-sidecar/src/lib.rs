//! The engine sidecar: the Unix-socket server every engine behind the
//! sub-application host runs (ADR-0023 H2, ADR-0027).
//!
//! * [`server`] — the NDJSON protocol the host's one engine client speaks,
//!   over any [`server::Engine`];
//! * [`healthcheck`] — the container probe for a distroless image, which
//!   has no shell and no curl;
//! * [`cgroup`] — the container's memory limit and OOM count, for an engine
//!   that sizes or supervises a worker process.

pub mod cgroup;
pub mod healthcheck;
pub mod server;

pub use server::{Engine, bind, router, serve};
