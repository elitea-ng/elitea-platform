//! One process registry of per-execution state, keyed by execution and
//! generation (#1832): the egress ledger ([`super::egress`]) and the
//! sub-agent ledger ([`super::ephemeral_agents`]) both keep one entry per
//! running command here.
//!
//! An entry lives until its command is forgotten (settled). One that never
//! settles (lease loss, abandonment) ages out after [`MAX_TRACKED_AGE`], and
//! past [`MAX_TRACKED_EXECUTIONS`] the oldest is dropped. A newer generation
//! of an execution drops the older generations' entries: their leases are
//! gone.

use std::collections::HashMap;
use std::hash::Hash;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

/// The most executions a registry holds; past it the oldest is dropped.
pub(crate) const MAX_TRACKED_EXECUTIONS: usize = 4_096;
/// An execution not settled within this is dropped (the run is long over or
/// abandoned).
pub(crate) const MAX_TRACKED_AGE: Duration = Duration::from_hours(12);

/// A registry key: it names one execution generation (and may name more).
pub(crate) trait GenerationKey: Clone + Eq + Hash {
    fn execution_id(&self) -> &str;
    fn generation(&self) -> u64;
}

struct Tracked<V> {
    value: Arc<V>,
    since: Instant,
}

/// The commands this process is running, each with its one `V`.
pub(crate) struct ExecutionRegistry<K, V> {
    tracked: Mutex<HashMap<K, Tracked<V>>>,
}

impl<K, V> Default for ExecutionRegistry<K, V> {
    fn default() -> Self {
        Self {
            tracked: Mutex::new(HashMap::new()),
        }
    }
}

impl<K: GenerationKey, V> ExecutionRegistry<K, V> {
    /// The key's entry when one is tracked and `keep` accepts it, else a new
    /// one from `make`, tracked from now (it replaces a refused one). Making
    /// one first drops aged entries and the execution's older generations,
    /// then the oldest entry when the registry is full. A poisoned registry
    /// tracks nothing: the caller gets an untracked new value.
    pub(crate) fn get_or_make(
        &self,
        key: K,
        keep: impl FnOnce(&V) -> bool,
        make: impl FnOnce() -> Arc<V>,
    ) -> Arc<V> {
        self.get_or_make_evicting(key, keep, make, |_| {})
    }

    /// [`Self::get_or_make`], handing each entry that making one dropped
    /// (aged, an older generation, the oldest of a full registry) to
    /// `evicted`, after the registry's lock is released.
    pub(crate) fn get_or_make_evicting(
        &self,
        key: K,
        keep: impl FnOnce(&V) -> bool,
        make: impl FnOnce() -> Arc<V>,
        mut evicted: impl FnMut(Arc<V>),
    ) -> Arc<V> {
        let Ok(mut tracked) = self.tracked.lock() else {
            return make();
        };
        if let Some(existing) = tracked.get(&key)
            && keep(&existing.value)
        {
            return Arc::clone(&existing.value);
        }
        let now = Instant::now();
        let mut dropped = Vec::new();
        let mut aged = Vec::new();
        for (other, entry) in tracked.iter() {
            if now.duration_since(entry.since) >= MAX_TRACKED_AGE
                || (other.execution_id() == key.execution_id()
                    && other.generation() < key.generation())
            {
                aged.push(other.clone());
            }
        }
        for other in aged {
            if let Some(entry) = tracked.remove(&other) {
                dropped.push(entry.value);
            }
        }
        if !tracked.contains_key(&key)
            && tracked.len() >= MAX_TRACKED_EXECUTIONS
            && let Some(oldest) = tracked
                .iter()
                .min_by_key(|(_, entry)| entry.since)
                .map(|(key, _)| key.clone())
            && let Some(entry) = tracked.remove(&oldest)
        {
            dropped.push(entry.value);
        }
        let value = make();
        tracked.insert(
            key,
            Tracked {
                value: Arc::clone(&value),
                since: now,
            },
        );
        drop(tracked);
        for entry in dropped {
            evicted(entry);
        }
        value
    }

    /// The entry of the newest generation tracked for `execution_id`.
    #[must_use]
    pub(crate) fn newest_of_execution(&self, execution_id: &str) -> Option<Arc<V>> {
        let tracked = self.tracked.lock().ok()?;
        tracked
            .iter()
            .filter(|(key, _)| key.execution_id() == execution_id)
            .max_by_key(|(key, _)| key.generation())
            .map(|(_, entry)| Arc::clone(&entry.value))
    }

    /// The key's entry, if one is tracked.
    #[must_use]
    pub(crate) fn get(&self, key: &K) -> Option<Arc<V>> {
        self.tracked
            .lock()
            .ok()?
            .get(key)
            .map(|entry| Arc::clone(&entry.value))
    }

    /// Stop tracking a settled command; the entry it held, if any.
    pub(crate) fn forget(&self, key: &K) -> Option<Arc<V>> {
        self.tracked
            .lock()
            .ok()?
            .remove(key)
            .map(|entry| entry.value)
    }

    #[cfg(test)]
    pub(crate) fn len(&self) -> usize {
        self.tracked.lock().map_or(0, |tracked| tracked.len())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[derive(Clone, Debug, Eq, Hash, PartialEq)]
    struct Key(String, u64);

    impl GenerationKey for Key {
        fn execution_id(&self) -> &str {
            &self.0
        }
        fn generation(&self) -> u64 {
            self.1
        }
    }

    fn key(execution: &str, generation: u64) -> Key {
        Key(execution.to_owned(), generation)
    }

    #[test]
    fn one_entry_per_key_until_forgotten() {
        let registry = ExecutionRegistry::<Key, u32>::default();
        let first = registry.get_or_make(key("e", 1), |_| true, || Arc::new(1));
        let again = registry.get_or_make(key("e", 1), |_| true, || Arc::new(2));
        assert!(Arc::ptr_eq(&first, &again));
        assert_eq!(registry.get(&key("e", 1)).as_deref(), Some(&1));
        registry.forget(&key("e", 1));
        assert!(registry.get(&key("e", 1)).is_none());
    }

    #[test]
    fn a_refused_entry_is_replaced() {
        let registry = ExecutionRegistry::<Key, u32>::default();
        registry.get_or_make(key("e", 1), |_| true, || Arc::new(1));
        let replaced = registry.get_or_make(key("e", 1), |value| *value == 2, || Arc::new(2));
        assert_eq!(*replaced, 2);
        assert_eq!(registry.len(), 1);
    }

    #[test]
    fn a_newer_generation_drops_the_older_ones_only() {
        let registry = ExecutionRegistry::<Key, u32>::default();
        registry.get_or_make(key("e", 1), |_| true, || Arc::new(1));
        registry.get_or_make(key("other", 1), |_| true, || Arc::new(1));
        registry.get_or_make(key("e", 2), |_| true, || Arc::new(2));
        assert!(registry.get(&key("e", 1)).is_none());
        assert!(registry.get(&key("other", 1)).is_some());
        assert!(registry.get(&key("e", 2)).is_some());
    }

    #[test]
    fn a_full_registry_drops_its_oldest() {
        let registry = ExecutionRegistry::<Key, u32>::default();
        for index in 0..MAX_TRACKED_EXECUTIONS {
            registry.get_or_make(key(&format!("e{index}"), 1), |_| true, || Arc::new(0));
        }
        registry.get_or_make(key("new", 1), |_| true, || Arc::new(0));
        assert_eq!(registry.len(), MAX_TRACKED_EXECUTIONS);
        assert!(registry.get(&key("new", 1)).is_some());
    }
}
