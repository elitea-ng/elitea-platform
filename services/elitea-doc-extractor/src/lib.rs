//! `elitea-doc-extractor`: the document extraction sidecar of the Rust
//! indexing worker (ADR-0030 I1).
//!
//! One process, one Unix socket, one service: `elitea.extract.v1`. The
//! worker streams a PDF or Office document in; the text comes back, or a
//! typed reason it did not. The process is a separate binary with its own
//! Cargo.lock because the worker's lock cannot hold the PDF backend (see the
//! crate README), and a separate PROCESS because the bytes are untrusted:
//! a crash or an OOM kill here is one skipped document to the worker.
//!
//! * [`config`] — the environment the process reads;
//! * [`service`] — the gRPC service, its limits and its capacity;
//! * [`client`] — a client over the socket, for `healthcheck` and tests.

#![cfg_attr(
    not(test),
    deny(
        clippy::expect_used,
        clippy::panic,
        clippy::todo,
        clippy::unimplemented,
        clippy::unwrap_used
    )
)]

pub mod client;
pub mod config;
pub mod service;

/// The generated `elitea.extract.v1` bindings.
#[allow(
    clippy::all,
    clippy::pedantic,
    missing_docs,
    rustdoc::all,
    reason = "generated code; its docs are the proto comments verbatim"
)]
pub mod pb {
    tonic::include_proto!("elitea.extract.v1");
}
