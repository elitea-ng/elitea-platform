//! The Inventory knowledge graph without a store or a model (ADR-0027,
//! ADR-0029 decision 7): what the Inventory engine and the desktop's local
//! workspace index share.
//!
//! * [`graph`] — the knowledge graph, with the Python graph's semantics;
//! * [`ingest`] — a source's documents into the graph, over any
//!   `ContentSource`, parsed by the tree-sitter parsers;
//! * [`retrieval`] — the read tools over a loaded graph;
//! * [`extract`] — the type tables and normalisers the model stage shares;
//! * [`embed`] — the texts entities and queries are embedded from;
//! * [`store`] — the `GraphStore` contract and the data a store keeps
//!   beside the graph (and, with `test-support`, `store_conformance`).
//!
//! Built alone, this crate links `serde_json` without `preserve_order` (the
//! desktop host must, ADR-0029 decision 2): a JSON map then keeps its keys
//! sorted, and the few answers that list a map's keys list them in that
//! order. The engine turns the feature on and answers as the Python engine
//! did, byte for byte (`tests/retrieval_tools.rs` runs the goldens both
//! ways).

pub mod embed;
pub mod extract;
pub mod graph;
pub mod ingest;
pub mod map;
pub mod retrieval;
pub mod store;
#[cfg(feature = "test-support")]
pub mod store_conformance;
