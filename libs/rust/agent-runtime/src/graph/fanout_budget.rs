//! Fan-out limits and budgets shared by every host that runs Parallel and Map.
//!
//! The limits match the node contracts. The Worker asserts its own limits
//! against these at compile time, and its checkpoint store and parent-row
//! accounting enforce the budgets.

use std::time::Duration;

/// Most fixed Parallel branches in one activation.
pub const MAX_PARALLEL_BRANCHES: usize = 16;

/// Most Map items in one activation: the largest fan-out of any kind.
pub const MAX_FANOUT_CHILDREN: usize = 64;

/// Threads one child may own: its root plus at most 128 admitted application
/// threads.
pub const MAX_CHILD_THREADS: usize = 129;

/// Parent rows one visit of an activation may append: its frozen occurrence
/// plus exactly one of join, pause, typed denial or accepted decision set.
/// Pause cards ride on the pause row; per-card HITL never writes the parent.
pub const MAX_PARENT_ROWS_PER_VISIT: usize = 2;

/// Transactions to prepare every child of one activation: one batched writer
/// activation and one batched read of the latest receipts.
pub const MAX_PREPARE_TRANSACTIONS: u64 = 2;

/// Transactions of one fresh child that runs one step: its fenced start probe
/// and its terminal save.
pub const MAX_CHILD_TRANSACTIONS: u64 = 2;

/// Wall-clock overhead of one instant child on a local `PostgreSQL`, p99.
pub const CHILD_OVERHEAD_P99: Duration = Duration::from_millis(30);

/// Restoring 64 completed children, p95.
pub const RESTORE_MAX_CHILDREN_P95: Duration = Duration::from_millis(500);

const _: () = assert!(MAX_PARALLEL_BRANCHES <= MAX_FANOUT_CHILDREN);
const _: () = assert!(
    MAX_PARENT_ROWS_PER_VISIT >= 2,
    "freeze plus one terminal row"
);
