pub mod command;
pub mod control;
mod error;
pub mod node_event;
pub mod output;
pub mod toolkit_execution;
pub(crate) mod wire;

// Generated protobuf and gRPC clients mirror comments and method shapes owned
// by the language-neutral schema generator, not this crate's handwritten API.
#[allow(clippy::all, clippy::pedantic)]
pub mod generated {
    include!(concat!(env!("OUT_DIR"), "/elitea.rs"));
}

pub use error::{InputLimitField, ProtocolError};
pub use generated::elitea;

#[cfg(feature = "sandbox-supervisor")]
pub mod sandbox_grant;
