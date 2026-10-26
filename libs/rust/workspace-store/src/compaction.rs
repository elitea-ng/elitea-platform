//! When a writer publishes a `COMPACT` instead of a `PUSH` (ADR-0034
//! decision 12, Phase 2).
//!
//! A thread's live chain is its newest `COMPACT` and the `PUSH` segments
//! after it. Every reader fetches the whole chain, so a long chain costs
//! every take-back and every cloud turn's hydrate a round trip per segment,
//! and a large one costs bytes the `COMPACT` would carry once. A writer
//! therefore compacts when the chain would pass either bound:
//!
//! * more than [`Policy::max_push_segments`] `PUSH` segments (default 8:
//!   eight cloud turns of increments, then one full bundle);
//! * more than [`Policy::max_push_bytes`] of `PUSH` bytes (default 64 MiB,
//!   the multipart threshold: at that size a fresh `COMPACT` is usually
//!   smaller than the increments it replaces).
//!
//! Main refuses a `PUSH` past [`SERVER_MAX_PUSH_CHAIN`] whatever the policy
//! says (`segment_chain_too_long`), so a configured bound is clamped to it.
//! The `COMPACT` supersedes the chain; Main deletes the superseded objects
//! after their 24-hour grace.

use crate::{KIND_COMPACT, KIND_PUSH};

/// Main's bound on the live `PUSH` segments of a thread.
pub const SERVER_MAX_PUSH_CHAIN: usize = 32;

/// The environment variables a host reads the policy from
/// ([`Policy::from_lookup`]): a count, and a size in MiB.
pub const ENV_MAX_PUSH_SEGMENTS: &str = "ELITEA_WORKSPACE_COMPACT_MAX_PUSH_SEGMENTS";
pub const ENV_MAX_PUSH_MB: &str = "ELITEA_WORKSPACE_COMPACT_MAX_PUSH_MB";

/// The compaction policy.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Policy {
    /// The most `PUSH` segments the live chain may hold after its `COMPACT`
    /// (0: always publish a `COMPACT`).
    pub max_push_segments: usize,
    /// The most `PUSH` bytes the live chain may hold.
    pub max_push_bytes: u64,
}

impl Default for Policy {
    fn default() -> Self {
        Self {
            max_push_segments: 8,
            max_push_bytes: 64 * 1024 * 1024,
        }
    }
}

/// What the live chain holds after its `COMPACT`.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct ChainStats {
    pub push_segments: usize,
    pub push_bytes: u64,
}

impl ChainStats {
    /// The stats of a chain given as `(kind, bytes)` in `seq` order: the
    /// `PUSH` segments after the last `COMPACT`.
    #[must_use]
    pub fn of<'a>(segments: impl IntoIterator<Item = (&'a str, u64)>) -> Self {
        let mut stats = Self::default();
        for (kind, bytes) in segments {
            if kind == KIND_COMPACT {
                stats = Self::default();
            } else {
                stats.push_segments += 1;
                stats.push_bytes = stats.push_bytes.saturating_add(bytes);
            }
        }
        stats
    }

    /// The chain after one more segment was published.
    #[must_use]
    pub fn after(self, kind: &str, bytes: u64) -> Self {
        if kind == KIND_COMPACT {
            return Self::default();
        }
        Self {
            push_segments: self.push_segments + 1,
            push_bytes: self.push_bytes.saturating_add(bytes),
        }
    }
}

impl Policy {
    /// The policy from `lookup` (an environment): [`ENV_MAX_PUSH_SEGMENTS`]
    /// and [`ENV_MAX_PUSH_MB`]; a value that is absent or not a number keeps
    /// the default, and the count is clamped to [`SERVER_MAX_PUSH_CHAIN`].
    #[must_use]
    pub fn from_lookup(lookup: impl Fn(&str) -> Option<String>) -> Self {
        let mut policy = Self::default();
        if let Some(count) = lookup(ENV_MAX_PUSH_SEGMENTS).and_then(|v| v.trim().parse().ok()) {
            policy.max_push_segments = count;
        }
        if let Some(mb) = lookup(ENV_MAX_PUSH_MB).and_then(|v| v.trim().parse::<u64>().ok()) {
            policy.max_push_bytes = mb.saturating_mul(1024 * 1024);
        }
        policy.clamped()
    }

    /// The policy from the process environment.
    #[must_use]
    pub fn from_env() -> Self {
        Self::from_lookup(|name| std::env::var(name).ok())
    }

    /// The count bounded by Main's.
    #[must_use]
    pub fn clamped(self) -> Self {
        Self {
            max_push_segments: self.max_push_segments.min(SERVER_MAX_PUSH_CHAIN),
            ..self
        }
    }

    /// Whether the chain may take one more `PUSH` at all (before it is
    /// written).
    #[must_use]
    pub fn allows_push(&self, chain: ChainStats) -> bool {
        let policy = self.clamped();
        chain.push_segments < policy.max_push_segments && chain.push_bytes < policy.max_push_bytes
    }

    /// The kind of the next segment, once the `PUSH` would be `push_bytes`
    /// long: `PUSH` while the chain stays within both bounds, else
    /// `COMPACT`.
    #[must_use]
    pub fn next_kind(&self, chain: ChainStats, push_bytes: u64) -> &'static str {
        let policy = self.clamped();
        if self.allows_push(chain)
            && chain.push_bytes.saturating_add(push_bytes) <= policy.max_push_bytes
        {
            KIND_PUSH
        } else {
            KIND_COMPACT
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_chain_counts_the_pushes_after_its_last_compact() {
        let stats = ChainStats::of([
            (KIND_PUSH, 5),
            (KIND_COMPACT, 100),
            (KIND_PUSH, 10),
            (KIND_PUSH, 20),
        ]);
        assert_eq!(
            stats,
            ChainStats {
                push_segments: 2,
                push_bytes: 30
            }
        );
        assert_eq!(stats.after(KIND_COMPACT, 1), ChainStats::default());
        assert_eq!(stats.after(KIND_PUSH, 1).push_segments, 3);
    }

    #[test]
    fn the_writer_compacts_when_the_chain_gets_long_or_large() {
        let policy = Policy {
            max_push_segments: 3,
            max_push_bytes: 100,
        };
        let short = ChainStats {
            push_segments: 2,
            push_bytes: 50,
        };
        assert_eq!(policy.next_kind(short, 10), KIND_PUSH);
        // Large: the next push would pass the byte bound.
        assert_eq!(policy.next_kind(short, 51), KIND_COMPACT);
        // Long: the chain holds the most pushes already.
        let long = ChainStats {
            push_segments: 3,
            push_bytes: 3,
        };
        assert!(!policy.allows_push(long));
        assert_eq!(policy.next_kind(long, 1), KIND_COMPACT);
        // 0 pushes allowed: always compact.
        let never = Policy {
            max_push_segments: 0,
            ..policy
        };
        assert_eq!(never.next_kind(ChainStats::default(), 1), KIND_COMPACT);
    }

    #[test]
    fn the_policy_is_read_from_the_environment_and_clamped_to_mains_bound() {
        let policy = Policy::from_lookup(|name| match name {
            ENV_MAX_PUSH_SEGMENTS => Some("500".into()),
            ENV_MAX_PUSH_MB => Some(" 16 ".into()),
            _ => None,
        });
        assert_eq!(policy.max_push_segments, SERVER_MAX_PUSH_CHAIN);
        assert_eq!(policy.max_push_bytes, 16 * 1024 * 1024);
        let garbage = Policy::from_lookup(|_| Some("lots".into()));
        assert_eq!(garbage, Policy::default());
    }
}
