//! The device session's shape, and the slot it is kept in, behind a trait so
//! the rest of the host (and its tests) never touch the real file.
//!
//! One slot of the owner-only credentials file (`credentials_file.rs`) holds
//! the whole device session as JSON. The refresh token is stored there and
//! nowhere else: not in the webview's storage, not in an environment
//! variable, not in the settings files. The webview is only ever handed the
//! short-lived access token.

use std::fmt;

use serde::{Deserialize, Serialize};

use crate::error::HostError;

/// The device session as it is kept between launches.
#[derive(Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct StoredSession {
    /// The deployment this session belongs to (an origin, no trailing path).
    pub origin: String,
    pub client_id: String,
    pub device_id: String,
    pub refresh_token: String,
    /// The revocation endpoint discovery named when this token was issued:
    /// the sign-out fallback when discovery cannot be fetched at that moment.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub revocation_endpoint: String,
}

impl fmt::Debug for StoredSession {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("StoredSession")
            .field("origin", &self.origin)
            .field("client_id", &self.client_id)
            .field("device_id", &self.device_id)
            .field("refresh_token", &"<redacted>")
            .field("revocation_endpoint", &self.revocation_endpoint)
            .finish()
    }
}

/// A signed-out session whose server-side revoke did not get through. Kept in
/// its OWN slot of the credentials file (the refresh token is a secret, so
/// never the settings files) and retried at the next launch.
#[derive(Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct PendingRevoke {
    pub origin: String,
    pub client_id: String,
    pub refresh_token: String,
    #[serde(default)]
    pub revocation_endpoint: String,
}

impl fmt::Debug for PendingRevoke {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("PendingRevoke")
            .field("origin", &self.origin)
            .field("client_id", &self.client_id)
            .field("refresh_token", &"<redacted>")
            .field("revocation_endpoint", &self.revocation_endpoint)
            .finish()
    }
}

impl From<&StoredSession> for PendingRevoke {
    fn from(session: &StoredSession) -> Self {
        Self {
            origin: session.origin.clone(),
            client_id: session.client_id.clone(),
            refresh_token: session.refresh_token.clone(),
            revocation_endpoint: session.revocation_endpoint.clone(),
        }
    }
}

/// At most this many revokes wait; the oldest is dropped beyond it.
pub const MAX_PENDING_REVOKES: usize = 8;

/// The waiting revokes. A value that does not parse reads as none.
pub fn load_pending(store: &dyn SecretStore) -> Result<Vec<PendingRevoke>, HostError> {
    Ok(store
        .load()?
        .and_then(|raw| serde_json::from_str(&raw).ok())
        .unwrap_or_default())
}

/// Replace the waiting revokes; an empty list removes the slot.
pub fn save_pending(store: &dyn SecretStore, pending: &[PendingRevoke]) -> Result<(), HostError> {
    if pending.is_empty() {
        return store.clear();
    }
    let start = pending.len().saturating_sub(MAX_PENDING_REVOKES);
    let raw =
        serde_json::to_string(&pending[start..]).map_err(|e| HostError::Internal(e.to_string()))?;
    store.save(&raw)
}

/// A single secret slot.
pub trait SecretStore: Send + Sync {
    fn load(&self) -> Result<Option<String>, HostError>;
    fn save(&self, secret: &str) -> Result<(), HostError>;
    /// Removing an absent secret is not an error.
    fn clear(&self) -> Result<(), HostError>;
}

/// Read the session out of a store. A value that does not parse is treated as
/// absent and cleared: a corrupted item must not wedge sign-in.
pub fn load_session(store: &dyn SecretStore) -> Result<Option<StoredSession>, HostError> {
    let Some(raw) = store.load()? else {
        return Ok(None);
    };
    if let Ok(session) = serde_json::from_str::<StoredSession>(&raw) {
        return Ok(Some(session));
    }
    store.clear()?;
    Ok(None)
}

pub fn save_session(store: &dyn SecretStore, session: &StoredSession) -> Result<(), HostError> {
    let raw = serde_json::to_string(session).map_err(|e| HostError::Internal(e.to_string()))?;
    store.save(&raw)
}

/// In-memory stand-in for the credentials file, for tests.
#[cfg(test)]
#[derive(Default)]
pub struct MemoryStore {
    slot: std::sync::Mutex<Option<String>>,
    pub fail_saves: std::sync::atomic::AtomicBool,
}

#[cfg(test)]
impl MemoryStore {
    pub fn raw(&self) -> Option<String> {
        self.slot.lock().expect("lock").clone()
    }
}

#[cfg(test)]
impl SecretStore for MemoryStore {
    fn load(&self) -> Result<Option<String>, HostError> {
        Ok(self.slot.lock().expect("lock").clone())
    }

    fn save(&self, secret: &str) -> Result<(), HostError> {
        if self.fail_saves.load(std::sync::atomic::Ordering::SeqCst) {
            return Err(HostError::Credentials("locked".into()));
        }
        *self.slot.lock().expect("lock") = Some(secret.to_owned());
        Ok(())
    }

    fn clear(&self) -> Result<(), HostError> {
        *self.slot.lock().expect("lock") = None;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn session() -> StoredSession {
        StoredSession {
            origin: "https://elitea.example.com".into(),
            client_id: "desktop".into(),
            device_id: "dev-1".into(),
            refresh_token: "r-secret-token-value".into(),
            revocation_endpoint: String::new(),
        }
    }

    #[test]
    fn a_session_round_trips_through_the_store() {
        let store = MemoryStore::default();
        assert_eq!(load_session(&store).unwrap(), None);
        save_session(&store, &session()).unwrap();
        assert_eq!(load_session(&store).unwrap(), Some(session()));
        store.clear().unwrap();
        assert_eq!(load_session(&store).unwrap(), None);
        store.clear().unwrap(); // clearing twice is fine
    }

    #[test]
    fn a_corrupt_item_reads_as_absent_and_is_cleared() {
        let store = MemoryStore::default();
        store.save("not json").unwrap();
        assert_eq!(load_session(&store).unwrap(), None);
        assert_eq!(store.raw(), None);
    }

    #[test]
    fn debug_output_never_contains_the_refresh_token() {
        let printed = format!("{:?}", session());
        assert!(!printed.contains("r-secret-token-value"));
        assert!(printed.contains("<redacted>"));
        let pending = format!("{:?}", PendingRevoke::from(&session()));
        assert!(!pending.contains("r-secret-token-value"));
    }

    #[test]
    fn a_session_stored_before_the_endpoint_field_still_loads() {
        let store = MemoryStore::default();
        store
            .save(r#"{"origin":"https://a","client_id":"desktop","device_id":"d","refresh_token":"r"}"#)
            .unwrap();
        let loaded = load_session(&store).unwrap().unwrap();
        assert_eq!(loaded.revocation_endpoint, "");
    }

    #[test]
    fn pending_revokes_are_bounded_and_an_empty_list_clears_the_item() {
        let store = MemoryStore::default();
        assert!(load_pending(&store).unwrap().is_empty());
        let many: Vec<PendingRevoke> = (0..MAX_PENDING_REVOKES + 3)
            .map(|n| PendingRevoke {
                refresh_token: format!("r{n}"),
                ..PendingRevoke::from(&session())
            })
            .collect();
        save_pending(&store, &many).unwrap();
        let kept = load_pending(&store).unwrap();
        assert_eq!(kept.len(), MAX_PENDING_REVOKES);
        assert_eq!(kept[0].refresh_token, "r3", "the oldest are dropped");
        save_pending(&store, &[]).unwrap();
        assert_eq!(store.raw(), None);
    }
}
