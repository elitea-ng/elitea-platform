//! The worker's HTTP client to `platform_origin`.
//!
//! `platform_origin` is elitea-main's internal platform listener
//! (`https://elitea-main:9446`): the product API on a runtime-CA mTLS
//! listener, which REQUIRES a client certificate. The Office host and the
//! artifact index source reach it over reqwest, so their client presents the
//! worker's runtime identity (the certificate the tonic channels present)
//! and trusts the runtime CA, on top of the connectors' hardened policy
//! (HTTPS only, no redirect, the policy's timeouts, pool and user agent).
//!
//! Every other host (GitHub, Confluence, ...) keeps the identity-less
//! hardened transport: the workload identity is offered to elitea-main only.

use std::sync::Arc;

use elitea_connectors::ClientPolicy;
use elitea_connectors::reqwest012::ReqwestTransport;
use zeroize::Zeroizing;

/// The runtime CA and the worker's client identity, PEM. No `Debug`: it
/// holds the private key.
#[derive(Clone)]
pub(crate) struct PlatformClientIdentity {
    ca_pem: Arc<Vec<u8>>,
    /// The certificate chain followed by its private key: the one buffer
    /// `reqwest::Identity::from_pem` reads under rustls.
    identity_pem: Arc<Zeroizing<Vec<u8>>>,
}

/// The identity or the client could not be built. Carries nothing from the
/// material.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct PlatformHttpError;

impl std::fmt::Display for PlatformHttpError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str("the platform HTTP client could not be built")
    }
}

impl std::error::Error for PlatformHttpError {}

impl PlatformClientIdentity {
    /// The runtime CA bundle and the workload `(certificate chain, key)`,
    /// as `RuntimeTrustMaterial::ca_pem` / `identity_pem` hand them out.
    pub(crate) fn new(ca_pem: Vec<u8>, (certificate, key): (Vec<u8>, Vec<u8>)) -> Self {
        let key = Zeroizing::new(key);
        let mut identity = Zeroizing::new(Vec::with_capacity(certificate.len() + key.len() + 1));
        identity.extend_from_slice(&certificate);
        identity.push(b'\n');
        identity.extend_from_slice(&key);
        Self {
            ca_pem: Arc::new(ca_pem),
            identity_pem: Arc::new(identity),
        }
    }

    /// The runtime CA bundle, PEM.
    pub(crate) fn ca_pem(&self) -> &[u8] {
        &self.ca_pem
    }

    /// The certificate chain followed by its key, PEM.
    pub(crate) fn identity_pem(&self) -> &[u8] {
        &self.identity_pem
    }
}

/// The hardened client of `policy` that also presents `identity` and
/// trusts the runtime CA: the transport for `platform_origin` only.
///
/// # Errors
///
/// The PEM material does not parse, or reqwest cannot build the client.
pub(crate) fn platform_transport(
    policy: &ClientPolicy,
    identity: &PlatformClientIdentity,
) -> Result<ReqwestTransport, PlatformHttpError> {
    let roots =
        reqwest::Certificate::from_pem_bundle(identity.ca_pem()).map_err(|_| PlatformHttpError)?;
    if roots.is_empty() {
        return Err(PlatformHttpError);
    }
    let client_identity =
        reqwest::Identity::from_pem(identity.identity_pem()).map_err(|_| PlatformHttpError)?;
    // The same builder as ReqwestTransport::hardened, plus the identity and
    // the runtime CA. Kept in step with it by the test below.
    let mut builder = reqwest::Client::builder()
        .https_only(true)
        .redirect(reqwest::redirect::Policy::none());
    if policy.disable_retries {
        builder = builder.retry(reqwest::retry::never());
    }
    for root in roots {
        builder = builder.add_root_certificate(root);
    }
    let client = builder
        .identity(client_identity)
        .connect_timeout(policy.connect_timeout)
        .timeout(policy.request_timeout)
        .pool_idle_timeout(policy.pool_idle_timeout)
        .pool_max_idle_per_host(policy.max_idle_per_host)
        .user_agent(policy.user_agent)
        .build()
        .map_err(|_| PlatformHttpError)?;
    Ok(ReqwestTransport::from_client(client))
}

/// A throwaway runtime CA and a client certificate under it.
#[cfg(test)]
pub(crate) fn test_identity() -> PlatformClientIdentity {
    use rcgen::{
        BasicConstraints, CertificateParams, CertifiedIssuer, ExtendedKeyUsagePurpose, IsCa,
        KeyPair, KeyUsagePurpose,
    };
    let mut ca_params = CertificateParams::new(Vec::<String>::new()).expect("ca params");
    ca_params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    ca_params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
    let ca = CertifiedIssuer::self_signed(ca_params, KeyPair::generate().expect("ca key"))
        .expect("ca cert");
    let key = KeyPair::generate().expect("client key");
    let mut params = CertificateParams::new(Vec::<String>::new()).expect("client params");
    params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
    let certificate = params.signed_by(&key, &ca).expect("client cert");
    PlatformClientIdentity::new(
        ca.pem().into_bytes(),
        (
            certificate.pem().into_bytes(),
            key.serialize_pem().into_bytes(),
        ),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_platform_transport_builds_from_the_runtime_material() {
        let identity = test_identity();
        assert!(
            platform_transport(&elitea_connectors::github::client::CLIENT_POLICY, &identity)
                .is_ok()
        );
    }

    #[test]
    fn unusable_material_is_refused() {
        let policy = elitea_connectors::github::client::CLIENT_POLICY;
        let good = test_identity();
        let no_ca = PlatformClientIdentity {
            ca_pem: Arc::new(b"not pem".to_vec()),
            identity_pem: Arc::clone(&good.identity_pem),
        };
        assert!(platform_transport(&policy, &no_ca).is_err());
        let no_key = PlatformClientIdentity::new(
            good.ca_pem().to_vec(),
            (b"not pem".to_vec(), b"not pem".to_vec()),
        );
        assert!(platform_transport(&policy, &no_key).is_err());
    }
}
