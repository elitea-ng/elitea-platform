//! The desktop host's local tool family (ADR-0029 decision 4).
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
compile_error!("elitea-local-tools supports macOS and Linux; Windows is ADR-0029 phase D3");

pub mod approvals;
pub mod checkpoint;
pub mod command;
pub mod error;
pub mod git;
pub mod ledger;
pub mod patch;
pub mod policy;
pub mod sandbox;
pub mod shell;
pub mod workspace;
