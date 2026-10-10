//! The desktop's local workspace index (ADR-0029 decision 7).
//!
//! The Inventory knowledge graph of a folder the person opened, built on
//! their machine from the shared core (`elitea-inventory-core`) and kept
//! beside the workspace's other host data, never inside the folder:
//!
//! * [`source`] — the folder as a `ContentSource`, listed by the same
//!   confined walk the agent's tools use, re-hashing only what changed;
//! * [`sqlite_store`] — the `GraphStore` over one owner-only SQLite file,
//!   with diff writes that keep the graph's order;
//! * [`fs`] — the owner-only directories and files it lives in.
//!
//! Nothing here talks to the network: the index is code structure parsed
//! locally.
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

#[cfg(not(unix))]
compile_error!("elitea-local-index supports macOS and Linux; Windows is ADR-0029 phase D3");

pub mod fs;
pub mod source;
pub mod sqlite_store;
