//! Identity extraction for a supervisor listener configured with mandatory mTLS.
//!
//! TLS verifies the certificate chain. This module selects the leaf identity;
//! parsing a certificate alone does not authenticate a caller.
use std::net::IpAddr;

use x509_parser::{
    extensions::GeneralName,
    prelude::{FromDer, X509Certificate},
};

/// Extract the peer identity from tonic's TLS connection, never request metadata.
///
/// # Errors
/// Returns `Unauthenticated` for missing TLS certificates or ambiguous identities.
/// The listener must require client certificates from its configured trust roots.
pub fn authenticated_peer<T>(request: &tonic::Request<T>) -> Result<String, tonic::Status> {
    let certificates = request.peer_certs().ok_or_else(invalid_identity)?;
    let leaf = certificates.first().ok_or_else(invalid_identity)?;
    certificate_identity(leaf.as_ref()).ok_or_else(invalid_identity)
}

fn invalid_identity() -> tonic::Status {
    tonic::Status::unauthenticated("A single supported workload certificate identity is required.")
}

fn certificate_identity(der: &[u8]) -> Option<String> {
    let (remaining, certificate) = X509Certificate::from_der(der).ok()?;
    if !remaining.is_empty() {
        return None;
    }
    let san = certificate.subject_alternative_name().ok()??;
    match san.value.general_names.as_slice() {
        [GeneralName::DNSName(name)] => dns_identity(name),
        [GeneralName::URI(uri)] => spiffe_identity(uri),
        _ => None,
    }
}

fn dns_identity(name: &str) -> Option<String> {
    let name = name.strip_suffix('.').unwrap_or(name).to_ascii_lowercase();
    if name.is_empty()
        || !name.is_ascii()
        || name
            .bytes()
            .any(|byte| byte.is_ascii_control() || byte == b'*')
        || name.parse::<IpAddr>().is_ok()
    {
        return None;
    }
    Some(format!("dns:{name}"))
}

fn spiffe_identity(identity: &str) -> Option<String> {
    // Keep signed identity bytes stable. Reject escaped or normalized URI forms
    // rather than resolve a different principal through URL normalization.
    if identity.len() > 512
        || !identity.is_ascii()
        || identity.bytes().any(|byte| byte.is_ascii_control())
        || identity.contains(['%', '?', '#', '\\'])
    {
        return None;
    }
    let uri = reqwest::Url::parse(identity).ok()?;
    if uri.scheme() != "spiffe"
        || uri.host_str().is_none()
        || identity
            .split_once("://")?
            .1
            .split('/')
            .next()?
            .contains('@')
        || !uri.username().is_empty()
        || uri.password().is_some()
        || uri.path().is_empty()
        || uri.path() == "/"
        || uri.as_str() != identity
    {
        return None;
    }
    Some(identity.to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn metadata_cannot_supply_a_peer_identity() {
        let mut request = tonic::Request::new(());
        request
            .metadata_mut()
            .insert("x-workload-identity", "dns:worker.test".parse().unwrap());
        assert_eq!(
            authenticated_peer(&request).unwrap_err().code(),
            tonic::Code::Unauthenticated
        );
    }

    #[test]
    fn canonical_dns_and_spiffe_identities() {
        assert_eq!(
            dns_identity("Worker.TEST."),
            Some("dns:worker.test".to_owned())
        );
        assert_eq!(
            spiffe_identity("spiffe://elitea.test/worker/one"),
            Some("spiffe://elitea.test/worker/one".to_owned())
        );
        for invalid in ["", "*.test", "127.0.0.1", "::1", "wörker.test", "worker\n"] {
            assert!(dns_identity(invalid).is_none(), "{invalid:?}");
        }
        for invalid in [
            "https://elitea.test/worker",
            "spiffe://elitea.test/",
            "spiffe://elitea.test",
            "spiffe://user@elitea.test/worker",
            "spiffe://@elitea.test/worker",
            "spiffe://elitea.test/worker?x=1",
            "spiffe://elitea.test/worker#x",
            "spiffe://elitea.test/%77orker",
            "spiffe://elitea.test/a/../worker",
        ] {
            assert!(spiffe_identity(invalid).is_none(), "{invalid:?}");
        }
    }

    #[test]
    fn leaf_certificates_require_one_identity_san() {
        assert_eq!(
            certificate_identity(include_bytes!("testdata/peer-dns.der")),
            Some("dns:worker.test".to_owned())
        );
        assert_eq!(
            certificate_identity(include_bytes!("testdata/peer-spiffe.der")),
            Some("spiffe://elitea.test/worker/one".to_owned())
        );
        for der in [
            include_bytes!("testdata/peer-mixed.der").as_slice(),
            include_bytes!("testdata/peer-cn.der").as_slice(),
            include_bytes!("testdata/peer-email.der").as_slice(),
        ] {
            assert!(certificate_identity(der).is_none());
        }
        let mut trailing = include_bytes!("testdata/peer-dns.der").to_vec();
        trailing.push(0);
        assert!(certificate_identity(&trailing).is_none());
        assert!(certificate_identity(b"invalid certificate").is_none());
    }
}
