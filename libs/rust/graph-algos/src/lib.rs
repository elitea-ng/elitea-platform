//! Graph algorithms the engines share (ADR-0027).
//!
//! * [`leiden`] (feature `leiden`, on by default) — seeded Leiden community
//!   detection with the RB-configuration objective, numbered the way
//!   leidenalg numbers its communities;
//! * [`pagerank`] — personalized `PageRank` with networkx's semantics, which
//!   the repo map (`elitea-inventory-core`) ranks files with. No dependency.
//!
//! An engine wraps these behind its own trait when it needs more (`DeepWiki`
//! replays recorded leidenalg partitions for parity); the algorithm itself
//! lives here once.

#[cfg(feature = "leiden")]
pub mod leiden;
pub mod pagerank;
