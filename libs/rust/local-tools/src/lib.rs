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

pub mod apply;
pub mod apply_patch;
pub mod approvals;
pub mod budget;
pub mod checkpoint;
pub mod classify;
pub mod clone_file;
pub mod command;
pub mod conditions;
pub mod credentials;
pub mod deny;
pub mod document;
pub mod edit;
pub mod egress_proxy;
pub mod error;
pub mod execpolicy;
pub mod files;
pub mod find;
pub mod folder_scan;
pub mod git;
#[cfg(feature = "handoff")]
pub mod handoff;
pub mod hooks;
pub mod host_env;
pub mod ledger;
mod line_ending;
pub mod lsp;
pub mod mcp;
pub mod patch;
pub mod picture;
pub mod policy;
pub mod process;
pub mod project;
pub mod project_instructions;
pub mod provider;
pub mod redact;
pub mod run_tests;
pub mod sandbox;
pub mod session;
pub mod shell;
pub mod slash_commands;
pub mod sys;
pub mod test_report;
pub mod workspace;
pub mod worktree;
