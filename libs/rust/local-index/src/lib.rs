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
//! * [`fs`] — the owner-only directories and files it lives in;
//! * [`service`] — one workspace's index kept current: refresh on the
//!   blocking pool, progress, cancel, status;
//! * [`tools`] — the read-only `workspace_index` toolset, named and shaped
//!   as the cloud Inventory tools.
//!
//! Nothing here talks to the network: the index is code structure parsed
//! locally.
//!
//! **What the desktop host links through this crate.** `elitea-inventory-core`,
//! `elitea-engine-core` and `elitea-code-parsers` (the parsers, the
//! Python-compatible text, the progress context). The host must never build
//! `serde_json` with `preserve_order` (ADR-0029 decision 2), and none of them
//! turns it on in its normal dependencies any more: engine-core and
//! code-parsers keep it (and `float_roundtrip`) in their dev-dependencies,
//! each engine binary turns it on in its own manifest, and CI checks the
//! resolved features with `cargo tree` (`ci-deepwiki-engine.yml`, "Engine
//! core and code parsers — normal build without `preserve_order`"). This crate
//! takes `serde_json` with default features only.
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
pub mod service;
pub mod source;
pub mod sqlite_store;
pub mod tools;
