//! The cloud executor (ADR-0034 decisions 7–10): a cloud turn of a
//! handed-off Local work thread.
//!
//! The worker, outside the sandbox, moves the bytes: it fetches the thread's
//! segment over the gRPC runtime plane, verifies and checks it out, re-scans
//! it for secrets, and at the end of the turn commits, bundles, uploads and
//! publishes. The sandbox only runs the tools.

pub(crate) mod context;
