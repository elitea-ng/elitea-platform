//! File-backed production trust material and exact command-key resolution.

use std::collections::BTreeMap;
use std::fmt;
use std::sync::Arc;

use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64_STANDARD;
use rustls::pki_types::{CertificateDer, PrivateKeyDer, pem::PemObject as _};
use rustls::{ClientConfig, RootCertStore};
use serde::Deserialize;
use tonic::transport::{Certificate, Identity};
use zeroize::Zeroizing;

use crate::config::{RuntimeConfigError, RuntimeDeployConfig, read_regular_file};
use crate::protocol::command::{
    Ed25519CommandAuthenticator, Ed25519PublicKeyResolver, SignedCommandAuthenticator,
};
use crate::spool::SpoolMasterKey;

const KEYRING_SCHEMA_VERSION: &str = "elitea.runtime-ed25519-keyring.v1";
pub(crate) const MAX_CA_BYTES: usize = 1024 * 1024;
pub(crate) const MAX_CERTIFICATE_BYTES: usize = 256 * 1024;
pub(crate) const MAX_PRIVATE_KEY_BYTES: usize = 128 * 1024;
const MAX_KEYRING_BYTES: usize = 64 * 1024;
const MAX_KEY_ID_BYTES: usize = 256;
const MAX_KEYS: usize = 64;

/// Stable, redacted failure at the process trust boundary.
#[derive(Debug)]
pub enum RuntimeTrustError {
    Material(RuntimeConfigError),
    InvalidTlsIdentity,
    InvalidSpoolKey,
    InvalidSigningKeyring,
}

impl fmt::Display for RuntimeTrustError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::Material(_) => "runtime trust material is unavailable or unsafe",
            Self::InvalidTlsIdentity => "the workload TLS identity is invalid",
            Self::InvalidSpoolKey => "the output spool key is invalid",
            Self::InvalidSigningKeyring => "the Ed25519 verification keyring is invalid",
        })
    }
}

impl std::error::Error for RuntimeTrustError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Material(error) => Some(error),
            Self::InvalidTlsIdentity | Self::InvalidSpoolKey | Self::InvalidSigningKeyring => None,
        }
    }
}

impl From<RuntimeConfigError> for RuntimeTrustError {
    fn from(value: RuntimeConfigError) -> Self {
        Self::Material(value)
    }
}

/// Exact immutable key-ID resolver with no default or prefix lookup.
#[derive(Clone)]
pub struct ExactEd25519PublicKeyResolver {
    keys: Arc<BTreeMap<Box<str>, [u8; 32]>>,
}

impl Ed25519PublicKeyResolver for ExactEd25519PublicKeyResolver {
    fn resolve_ed25519_public_key(&self, key_id: &str) -> Option<[u8; 32]> {
        self.keys.get(key_id).copied()
    }
}

/// Process-owned private-plane material for gRPC/HTTP, command verification
/// and encrypted output. The command bus's NATS identity is separate material
/// that its transport reads on every TLS handshake
/// (`transport::nats_jetstream`).
pub struct RuntimeTrustMaterial {
    ca_pem: Vec<u8>,
    certificate_pem: Vec<u8>,
    private_key_pem: Zeroizing<Vec<u8>>,
    spool_key: Zeroizing<[u8; 32]>,
    signing_keys: ExactEd25519PublicKeyResolver,
}

impl RuntimeTrustMaterial {
    /// Load and validate the process-owned workload identity, output-spool key
    /// and exact command-signing keyring.
    ///
    /// # Errors
    ///
    /// Returns a redacted trust error when any file, permission, format,
    /// certificate/key relationship or keyring invariant is invalid.
    pub fn load(config: &RuntimeDeployConfig) -> Result<Self, RuntimeTrustError> {
        let ca_pem = read_regular_file(&config.ca_path, MAX_CA_BYTES, false, "runtime CA bundle")?;
        let certificate_pem = read_regular_file(
            &config.certificate_path,
            MAX_CERTIFICATE_BYTES,
            false,
            "workload certificate chain",
        )?;
        let private_key_pem = Zeroizing::new(read_regular_file(
            &config.private_key_path,
            MAX_PRIVATE_KEY_BYTES,
            true,
            "workload private key",
        )?);
        validate_tls_identity(&ca_pem, &certificate_pem, &private_key_pem)?;
        let spool_key = read_regular_file(&config.spool_key_path, 32, true, "output spool key")?;
        let spool_key: [u8; 32] = spool_key
            .try_into()
            .map_err(|_| RuntimeTrustError::InvalidSpoolKey)?;
        let signing_keys = load_ed25519_keyring(config)?;
        Ok(Self {
            ca_pem,
            certificate_pem,
            private_key_pem,
            spool_key: Zeroizing::new(spool_key),
            signing_keys,
        })
    }

    #[must_use]
    pub fn private_ca(&self) -> Certificate {
        Certificate::from_pem(self.ca_pem.clone())
    }

    #[must_use]
    pub fn client_identity(&self) -> Identity {
        Identity::from_pem(self.certificate_pem.clone(), &self.private_key_pem)
    }

    #[must_use]
    pub fn spool_master_key(&self) -> SpoolMasterKey {
        SpoolMasterKey::new(*self.spool_key)
    }

    #[must_use]
    pub fn command_authenticator(&self) -> Arc<dyn SignedCommandAuthenticator> {
        Arc::new(Ed25519CommandAuthenticator::new(self.signing_keys.clone()))
    }
}

fn load_ed25519_keyring(
    config: &RuntimeDeployConfig,
) -> Result<ExactEd25519PublicKeyResolver, RuntimeTrustError> {
    load_ed25519_keyring_file(&config.ed25519_keyring_path)
}

/// Load the shared public verification keyring for a dedicated runtime process.
/// # Errors
/// Returns a redacted error for unsafe files or invalid key material.
pub fn load_ed25519_keyring_file(
    path: &std::path::Path,
) -> Result<ExactEd25519PublicKeyResolver, RuntimeTrustError> {
    let raw = read_regular_file(
        path,
        MAX_KEYRING_BYTES,
        false,
        "Ed25519 verification keyring",
    )?;
    parse_ed25519_keyring(&raw)
}

fn parse_ed25519_keyring(raw: &[u8]) -> Result<ExactEd25519PublicKeyResolver, RuntimeTrustError> {
    let keyring = serde_json::from_slice::<PublicKeyring>(raw)
        .map_err(|_| RuntimeTrustError::InvalidSigningKeyring)?;
    if keyring.schema_version != KEYRING_SCHEMA_VERSION
        || keyring.keys.is_empty()
        || keyring.keys.len() > MAX_KEYS
    {
        return Err(RuntimeTrustError::InvalidSigningKeyring);
    }
    let mut keys = BTreeMap::new();
    for entry in keyring.keys {
        if entry.key_id.is_empty()
            || entry.key_id.len() > MAX_KEY_ID_BYTES
            || entry
                .key_id
                .bytes()
                .any(|byte| matches!(byte, b'\r' | b'\n' | b'\0'))
            || entry
                .public_key_base64
                .bytes()
                .any(|byte| byte.is_ascii_whitespace())
        {
            return Err(RuntimeTrustError::InvalidSigningKeyring);
        }
        let decoded = BASE64_STANDARD
            .decode(entry.public_key_base64)
            .map_err(|_| RuntimeTrustError::InvalidSigningKeyring)?;
        let public_key: [u8; 32] = decoded
            .try_into()
            .map_err(|_| RuntimeTrustError::InvalidSigningKeyring)?;
        if keys.insert(entry.key_id.into(), public_key).is_some() {
            return Err(RuntimeTrustError::InvalidSigningKeyring);
        }
    }
    Ok(ExactEd25519PublicKeyResolver {
        keys: Arc::new(keys),
    })
}

pub(crate) fn validate_tls_identity(
    ca_pem: &[u8],
    certificate_pem: &[u8],
    private_key_pem: &[u8],
) -> Result<(), RuntimeTrustError> {
    let mut roots = RootCertStore::empty();
    let ca_certificates = CertificateDer::pem_slice_iter(ca_pem)
        .collect::<Result<Vec<_>, _>>()
        .map_err(|_| RuntimeTrustError::InvalidTlsIdentity)?;
    if ca_certificates.is_empty() {
        return Err(RuntimeTrustError::InvalidTlsIdentity);
    }
    for certificate in ca_certificates {
        roots
            .add(certificate)
            .map_err(|_| RuntimeTrustError::InvalidTlsIdentity)?;
    }
    let client_certificates = CertificateDer::pem_slice_iter(certificate_pem)
        .collect::<Result<Vec<_>, _>>()
        .map_err(|_| RuntimeTrustError::InvalidTlsIdentity)?;
    if client_certificates.is_empty() {
        return Err(RuntimeTrustError::InvalidTlsIdentity);
    }
    let private_key = PrivateKeyDer::from_pem_slice(private_key_pem)
        .map_err(|_| RuntimeTrustError::InvalidTlsIdentity)?;
    ClientConfig::builder_with_provider(rustls::crypto::ring::default_provider().into())
        .with_protocol_versions(&[&rustls::version::TLS13])
        .map_err(|_| RuntimeTrustError::InvalidTlsIdentity)?
        .with_root_certificates(roots)
        .with_client_auth_cert(client_certificates, private_key)
        .map_err(|_| RuntimeTrustError::InvalidTlsIdentity)?;
    Ok(())
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct PublicKeyring {
    schema_version: String,
    keys: Vec<PublicKeyEntry>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct PublicKeyEntry {
    key_id: String,
    public_key_base64: String,
}

#[cfg(test)]
mod tests {
    use base64::Engine as _;
    use base64::engine::general_purpose::STANDARD as BASE64_STANDARD;
    use serde_json::json;

    use super::{RuntimeTrustError, parse_ed25519_keyring};
    use crate::protocol::command::Ed25519PublicKeyResolver;

    #[test]
    fn exact_keyring_has_no_default_prefix_or_duplicate_resolution() {
        let public_key = [7_u8; 32];
        let encoded = BASE64_STANDARD.encode(public_key);
        let raw = serde_json::to_vec(&json!({
            "schema_version": "elitea.runtime-ed25519-keyring.v1",
            "keys": [{"key_id": "runtime-signing-1", "public_key_base64": encoded}]
        }))
        .expect("keyring JSON");

        let resolver = parse_ed25519_keyring(&raw).expect("valid keyring");

        assert_eq!(
            resolver.resolve_ed25519_public_key("runtime-signing-1"),
            Some(public_key)
        );
        assert_eq!(resolver.resolve_ed25519_public_key("runtime-signing"), None);
        assert_eq!(resolver.resolve_ed25519_public_key("unknown"), None);
    }

    #[test]
    fn malformed_or_ambiguous_keyrings_fail_closed() {
        let encoded = BASE64_STANDARD.encode([9_u8; 32]);
        let cases = [
            json!({"schema_version": "wrong", "keys": [{"key_id": "one", "public_key_base64": encoded}]}),
            json!({"schema_version": "elitea.runtime-ed25519-keyring.v1", "keys": []}),
            json!({"schema_version": "elitea.runtime-ed25519-keyring.v1", "keys": [
                {"key_id": "one", "public_key_base64": encoded},
                {"key_id": "one", "public_key_base64": encoded}
            ]}),
            json!({"schema_version": "elitea.runtime-ed25519-keyring.v1", "keys": [
                {"key_id": "one\nleak", "public_key_base64": encoded}
            ]}),
            json!({"schema_version": "elitea.runtime-ed25519-keyring.v1", "keys": [
                {"key_id": "one", "public_key_base64": "AAAA\nAAAA"}
            ]}),
            json!({"schema_version": "elitea.runtime-ed25519-keyring.v1", "keys": [
                {"key_id": "one", "public_key_base64": BASE64_STANDARD.encode([1_u8; 31])}
            ]}),
        ];
        for value in cases {
            let raw = serde_json::to_vec(&value).expect("keyring JSON");
            assert!(matches!(
                parse_ed25519_keyring(&raw),
                Err(RuntimeTrustError::InvalidSigningKeyring)
            ));
        }
    }
}
