//! The caller side of `elitea-vector` (ADR-0030 decisions 2 and 5,
//! ADR-0031 decisions 1 and 4).
//!
//! * [`client`]: a typed `elitea.vector.v1` gRPC client over mTLS. Every
//!   call carries the claim's bearer token; status codes classify as
//!   retryable or not ([`error`]).
//! * [`filter`]: the SDK's filter JSON (langchain-postgres operators)
//!   translated into the facade's filters, refusing what Qdrant cannot
//!   express.
//! * [`search`], [`rerank`], [`format`], [`stepback`], [`tools`]: the SDK's
//!   index tools on top of the client, with the three fixes ADR-0030
//!   decision 5 lists (better score kept on duplicate keys, `full_text_search`
//!   works, `remove_index("")` is refused).
//!
//! The crate pins no model: queries are embedded through [`QueryEmbedder`]
//! and the step-back tools call a [`StepbackModel`], both implemented by
//! the worker on its existing model path.

#![allow(
    clippy::module_name_repetitions,
    reason = "VectorClient and VectorBackend read best under these names"
)]

pub mod client;
pub mod error;
pub mod filter;
pub mod format;
pub mod hit;
mod prompts;
pub mod pyfmt;
pub mod rerank;
pub mod search;
pub mod stepback;
#[cfg(feature = "test-server")]
pub mod testing;
pub mod tools;

mod proto {
    #![allow(clippy::all, clippy::pedantic, missing_docs, reason = "generated code")]
    include!(concat!(env!("OUT_DIR"), "/elitea.rs"));
}

/// The `elitea.vector.v1` messages and service stubs.
pub use proto::elitea::vector::v1 as pb;

pub use client::{
    ClaimToken, ClientConfig, DeleteSelector, HybridSearchParams, SearchParams, UpsertSummary,
    VectorClient,
};
pub use error::{Error, ErrorClass, Result};
pub use filter::FilterError;
pub use search::{QueryEmbedder, VectorBackend, VectorSession};
pub use stepback::StepbackModel;
pub use tools::{IndexCatalog, IndexEntry, IndexTools, SearchArgs, SearchOutput};
