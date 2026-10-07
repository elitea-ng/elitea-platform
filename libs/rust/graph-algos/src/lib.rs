//! Graph algorithms the engines share (ADR-0027).
//!
//! * [`leiden`] — seeded Leiden community detection with the
//!   RB-configuration objective, numbered the way leidenalg numbers its
//!   communities.
//!
//! An engine wraps these behind its own trait when it needs more (`DeepWiki`
//! replays recorded leidenalg partitions for parity); the algorithm itself
//! lives here once.

pub mod leiden;
