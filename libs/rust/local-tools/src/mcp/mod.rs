//! The person's own stdio MCP servers on this device (ADR-0029 decision 4,
//! #1230), distinct from the credentialed toolkits that stay server-side
//! (decision 3).
//!
//! * [`config`]: the `mcp.json` files in host data (never the workspace),
//!   schema-checked, owner-only.
//! * [`proposal`]: a repository's `.mcp.json` (and Cursor's and VS Code's)
//!   as a proposal the person accepts server by server; nothing starts from it.
//! * [`server`]: the servers of one session, each under the OS sandbox
//!   with the session's deny list, started lazily and stopped with the
//!   session, a policy change or a configuration change.
//! * [`provider`]: their tools in the turn, named `mcp__<server>__<tool>`
//!   ([`naming`]), each call decided by the rules
//!   ([`crate::approvals::ToolKind::McpCall`]).
//!
//! Gated by the policy's `local_mcp` (with `allowed`): off or absent, no
//! server starts and no tool is offered. Local-only: the cloud executor
//! never runs a stdio server (its contract test names the exclusion).

pub mod config;
pub mod naming;
pub mod proposal;
pub mod provider;
pub mod server;

pub use provider::{Framed, McpFramer, McpToolProvider, TOOLSET_NAME};
pub use server::{McpServers, ServerState, ServerStatus};
