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

/// Most children of one activation running at once (Parallel and Map).
pub const MAX_FANOUT_CONCURRENCY: usize = 8;

/// Open (pending or decided) interrupt cards per root response. Matches Main
/// `executioninterrupt.MaxOpenInterrupts`.
pub const MAX_OPEN_INTERRUPTS: usize = 16;

/// Decision value bound in characters. Matches Main `MaxDecisionValueChars`.
pub const MAX_DECISION_VALUE_CHARS: usize = 8192;

/// Decisions one private fetch returns. Matches Main `MaxFetchDecisions`.
pub const MAX_FETCH_DECISIONS: usize = 16;

/// Bytes of one canonical fetch entry. Matches Main `MaxFetchEntryBytes`.
pub const MAX_FETCH_ENTRY_BYTES: usize = 64 * 1024;

/// Largest fan-out member ordinal (1-based). Matches Main `MaxFanoutOrdinal`.
pub const MAX_FANOUT_ORDINAL: usize = MAX_FANOUT_CHILDREN;

/// Agent path tiers including the fan-out member tier. Matches Main
/// `MaxHierarchyTiers`.
pub const MAX_AGENT_PATH_TIERS: usize = 3;

/// Serialized bytes of one child's projected outputs.
pub const MAX_CHILD_RESULT_BYTES: usize = 512 * 1024;

/// Serialized bytes of the joined collection one activation lands.
pub const MAX_JOINED_RESULT_BYTES: usize = 8 * 1024 * 1024;

/// Serialized bytes of one failed child's `error` object in the join
/// (contract v1.1 §8a; no raw text).
pub const MAX_JOIN_ERROR_ENTRY_BYTES: usize = 512;

/// The live decision tick while at least one card is open: starts here...
pub const DECISION_TICK_INITIAL: Duration = Duration::from_secs(1);

/// ...doubles while fetches bring nothing new, and stops growing here.
pub const DECISION_TICK_MAX: Duration = Duration::from_secs(5);

/// Children running at once across one Worker process. A paused child holds
/// no permit.
pub const WORKER_CHILD_PERMITS: usize = 64;

/// Concurrent model streams per (Worker, model) across all fan-out children.
pub const MODEL_STREAMS_PER_WORKER: usize = 16;

/// Distinct models the per-model limiter tracks at once.
pub const MAX_TRACKED_MODELS: usize = 256;

/// Upper bound for a running child to stop after cancellation is requested.
pub const CANCEL_CLEANUP: Duration = Duration::from_millis(100);

const _: () = assert!(MAX_PARALLEL_BRANCHES <= MAX_FANOUT_CHILDREN);
const _: () = assert!(MAX_OPEN_INTERRUPTS == MAX_PARALLEL_BRANCHES);
const _: () = assert!(MAX_FANOUT_CONCURRENCY <= MAX_PARALLEL_BRANCHES);
const _: () = assert!(
    MAX_PARENT_ROWS_PER_VISIT >= 2,
    "freeze plus one terminal row"
);
