//! What a warm turn skips (#1213 stage 3): the last resolved agent version
//! and the answering participant of each conversation, kept in memory for
//! the app's session only.
//!
//! Both are keyed by whose session read them (the credentials' identity:
//! the deployment and the sign-in) and are dropped on sign-out, a new
//! sign-in or a deployment switch ([`TurnCaches::clear`]). Neither is ever
//! trusted alone:
//!
//! * a cached version is admitted at once, and the turn's `start` goes out
//!   while the version is revalidated (`If-None-Match` with its `ETag`); a
//!   changed version is admitted again before the model's first request,
//!   and a refused or unreadable one stops the turn there;
//! * a cached participant addresses the `start`, while the conversation is
//!   read again beside it (its message count decides whether the agent's
//!   session must be seeded); a participant that changed, or a `start` the
//!   platform refuses for it (`404`, `422`), is looked up again and the
//!   start retried once.
//!
//! The model catalogue's context limits of each model (#1217) are kept the
//! same way; a lookup that failed is not kept, so the next turn asks again.
//! The catalogue of chat models a turn's chosen model is checked against
//! (#1228) is kept for a minute per project (`super::catalogue`).
//!
//! The local turn's `start` itself is never cached: the memory recall it
//! answers is computed fresh at every turn (ADR-0029 decision 8).

use std::collections::HashMap;
use std::sync::Mutex;

use elitea_agent_runtime::request::ModelContextLimits;

use super::api::{AnsweringParticipant, ResolvedVersion};
use super::catalogue::Catalogues;

/// An agent version as one session read it.
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct VersionKey {
    pub identity: String,
    pub project_id: i64,
    pub application_id: i64,
    pub version_id: i64,
}

/// A conversation's answering participant, for one agent version, as one
/// session read it. `conversation` is the id the UI named it by.
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct ParticipantKey {
    pub identity: String,
    pub project_id: i64,
    pub conversation: String,
    pub application_id: i64,
    pub version_id: i64,
}

/// A model's context limits, as one session read them from the catalogue
/// of the turn's project.
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct LimitsKey {
    pub identity: String,
    pub project_id: i64,
    pub model_name: String,
}

/// A resolved version and the tag that revalidates it.
#[derive(Clone, Debug)]
pub struct CachedVersion {
    pub etag: String,
    pub resolved: ResolvedVersion,
}

/// Bound on each map: a session that turns more distinct versions or
/// conversations than this starts over (a cache, not a store).
const MAX_ENTRIES: usize = 256;

#[derive(Default)]
pub struct TurnCaches {
    versions: Mutex<HashMap<VersionKey, CachedVersion>>,
    participants: Mutex<HashMap<ParticipantKey, AnsweringParticipant>>,
    limits: Mutex<HashMap<LimitsKey, ModelContextLimits>>,
    /// The projects' chat models (#1228), each kept for a minute.
    pub catalogues: Catalogues,
}

fn locked<T>(mutex: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    mutex
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

fn bounded_insert<K: Eq + std::hash::Hash, V>(map: &mut HashMap<K, V>, key: K, value: V) {
    if map.len() >= MAX_ENTRIES && !map.contains_key(&key) {
        map.clear();
    }
    map.insert(key, value);
}

impl TurnCaches {
    #[must_use]
    pub fn version(&self, key: &VersionKey) -> Option<CachedVersion> {
        locked(&self.versions).get(key).cloned()
    }

    /// Keep `resolved` when the platform tagged it (an untagged answer
    /// cannot be revalidated, so it is not kept).
    pub fn keep_version(&self, key: VersionKey, resolved: &ResolvedVersion, etag: Option<&str>) {
        match etag {
            Some(etag) => bounded_insert(
                &mut locked(&self.versions),
                key,
                CachedVersion {
                    etag: etag.to_owned(),
                    resolved: resolved.clone(),
                },
            ),
            None => self.forget_version(&key),
        }
    }

    pub fn forget_version(&self, key: &VersionKey) {
        locked(&self.versions).remove(key);
    }

    #[must_use]
    pub fn participant(&self, key: &ParticipantKey) -> Option<AnsweringParticipant> {
        locked(&self.participants).get(key).cloned()
    }

    pub fn keep_participant(&self, key: ParticipantKey, answering: &AnsweringParticipant) {
        bounded_insert(&mut locked(&self.participants), key, answering.clone());
    }

    pub fn forget_participant(&self, key: &ParticipantKey) {
        locked(&self.participants).remove(key);
    }

    #[must_use]
    pub fn limits(&self, key: &LimitsKey) -> Option<ModelContextLimits> {
        locked(&self.limits).get(key).copied()
    }

    pub fn keep_limits(&self, key: LimitsKey, limits: ModelContextLimits) {
        bounded_insert(&mut locked(&self.limits), key, limits);
    }

    /// Sign-out, a new sign-in, another deployment: nothing of the last
    /// session is reused.
    pub fn clear(&self) {
        locked(&self.versions).clear();
        locked(&self.participants).clear();
        locked(&self.limits).clear();
        self.catalogues.clear();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn version(identity: &str) -> VersionKey {
        VersionKey {
            identity: identity.into(),
            project_id: 1,
            application_id: 5,
            version_id: 9,
        }
    }

    fn resolved() -> ResolvedVersion {
        serde_json::from_value(serde_json::json!({"version_details": {}})).unwrap()
    }

    #[test]
    fn only_a_tagged_version_is_kept_and_only_for_its_session() {
        let caches = TurnCaches::default();
        caches.keep_version(version("a"), &resolved(), None);
        assert!(caches.version(&version("a")).is_none());
        caches.keep_version(version("a"), &resolved(), Some("\"x\""));
        assert_eq!(caches.version(&version("a")).unwrap().etag, "\"x\"");
        assert!(caches.version(&version("b")).is_none(), "another sign-in");
        // An untagged answer replaces a tagged copy: it is dropped.
        caches.keep_version(version("a"), &resolved(), None);
        assert!(caches.version(&version("a")).is_none());
        caches.keep_version(version("a"), &resolved(), Some("\"x\""));
        caches.clear();
        assert!(caches.version(&version("a")).is_none());
    }
}
