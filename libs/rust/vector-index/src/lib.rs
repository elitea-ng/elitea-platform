//! The caller side of `elitea-vector` (ADR-0030 decisions 2 and 5,
//! ADR-0031 decisions 1 and 4).
//!
//! * [`callback`]: the callback bearer of an engine invocation (`DeepWiki`,
//!   Inventory).
//! * [`client`]: a typed `elitea.vector.v1` gRPC client over mTLS. Every
//!   call carries the claim's bearer token; status codes classify as
//!   retryable or not ([`error`]).
//! * [`filter`]: the SDK's filter JSON (langchain-postgres operators)
//!   translated into the facade's filters, refusing what Qdrant cannot
//!   express.
//! * [`search`], [`rerank`], [`mod@format`], [`stepback`], [`tools`]: the SDK's
//!   index tools on top of the client, with the three fixes ADR-0030
//!   decision 5 lists (better score kept on duplicate keys, `full_text_search`
//!   works, `remove_index("")` is refused).
//!
//! The crate pins no model: queries are embedded through [`QueryEmbedder`]
//! and the step-back tools call a [`StepbackModel`], both implemented by
//! the worker on its existing model path.
//!
//! **Key order.** The SDK prints its result dicts with the keys in insertion
//! order (`page_content`, `metadata`, `score`). That is `serde_json`'s
//! `preserve_order`, which Cargo would unify into every crate of the binary;
//! the agent worker must build without it (its canonical digests, ADR-0029).
//! So this crate takes it through the `sdk-key-order` feature: on for its
//! own tests and for hosts that can afford it, off in the worker, where the
//! same keys come out sorted (`metadata`, `page_content`, `score`). Nothing
//! else changes.

#![allow(
    clippy::module_name_repetitions,
    reason = "VectorClient and VectorBackend read best under these names"
)]

pub mod callback;
pub mod client;
pub mod error;
pub mod filter;
pub mod format;
pub mod hit;
pub mod memory;
mod prompts;
mod pyfmt;
pub mod rerank;
pub mod search;
pub mod stepback;
#[cfg(feature = "test-server")]
pub mod testing;
pub mod tools;

mod proto {
    #![allow(
        clippy::all,
        clippy::pedantic,
        missing_docs,
        rustdoc::invalid_html_tags,
        reason = "generated code: the proto comments are not rustdoc"
    )]
    include!(concat!(env!("OUT_DIR"), "/elitea.rs"));
}

/// The `elitea.vector.v1` messages and service stubs.
pub use proto::elitea::vector::v1 as pb;

pub use callback::callback_bearer;
pub use client::{
    ClaimToken, ClientConfig, DeleteSelector, HybridSearchParams, MemoryTarget, SearchParams,
    UpsertSummary, VectorClient,
};
pub use error::{Error, ErrorClass, Result};
pub use filter::FilterError;
pub use search::{QueryEmbedder, VectorBackend, VectorSession};
pub use stepback::StepbackModel;
pub use tools::{IndexCatalog, IndexEntry, IndexTools, SearchArgs, SearchOutput};
