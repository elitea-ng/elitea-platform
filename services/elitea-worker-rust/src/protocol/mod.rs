pub mod approval_policy;
pub mod command;
pub mod control;
pub mod delegation;
pub mod egress;
mod error;
pub mod node_event;
pub mod output;
pub mod run_caps;
pub(crate) mod run_steps;
pub mod security_context;
pub mod toolkit_execution;
pub(crate) mod vector_token;
pub(crate) mod wire;

// Generated protobuf and gRPC clients mirror comments and method shapes owned
// by the language-neutral schema generator, not this crate's handwritten API.
#[allow(clippy::all, clippy::pedantic)]
pub mod generated {
    include!(concat!(env!("OUT_DIR"), "/elitea.rs"));
}

/// The document extractor sidecar's contract (`elitea.extract.v1`), spoken
/// over a Unix socket by `transport::extractor_client`.
#[allow(clippy::all, clippy::pedantic)]
pub mod extract {
    include!(concat!(env!("OUT_DIR"), "/elitea.extract.v1.rs"));
}

pub use error::{InputLimitField, ProtocolError};
pub use generated::elitea;

#[cfg(feature = "sandbox-supervisor")]
pub mod sandbox_grant;
