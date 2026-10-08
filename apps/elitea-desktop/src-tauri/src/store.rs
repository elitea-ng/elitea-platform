//! Where secrets live: the OS keychain, behind a trait so the rest of the host
//! (and its tests) never touch a real keychain.
//!
//! One keychain item holds the whole device session as JSON. The refresh token
//! is stored here and nowhere else: not in a file, not in the webview's
//! storage, not in an environment variable. The webview is only ever handed
//! the short-lived access token.

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
}

impl fmt::Debug for StoredSession {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("StoredSession")
            .field("origin", &self.origin)
            .field("client_id", &self.client_id)
            .field("device_id", &self.device_id)
            .field("refresh_token", &"<redacted>")
            .finish()
    }
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

/// The OS keychain (macOS Keychain, Windows Credential Manager, Secret Service).
pub struct KeyringStore {
    entry: keyring::Entry,
}

impl KeyringStore {
    pub fn new(service: &str, account: &str) -> Result<Self, HostError> {
        let entry = keyring::Entry::new(service, account)
            .map_err(|e| HostError::Keychain(e.to_string()))?;
        Ok(Self { entry })
    }
}

impl SecretStore for KeyringStore {
    fn load(&self) -> Result<Option<String>, HostError> {
        match self.entry.get_password() {
            Ok(secret) => Ok(Some(secret)),
            Err(keyring::Error::NoEntry) => Ok(None),
            Err(e) => Err(HostError::Keychain(e.to_string())),
        }
    }

    fn save(&self, secret: &str) -> Result<(), HostError> {
        self.entry
            .set_password(secret)
            .map_err(|e| HostError::Keychain(e.to_string()))
    }

    fn clear(&self) -> Result<(), HostError> {
        match self.entry.delete_credential() {
            Ok(()) | Err(keyring::Error::NoEntry) => Ok(()),
            Err(e) => Err(HostError::Keychain(e.to_string())),
        }
    }
}

/// In-memory stand-in for the keychain, for tests.
#[cfg(test)]
#[derive(Default)]
pub struct MemoryStore {
    slot: std::sync::Mutex<Option<String>>,
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
    }
}
