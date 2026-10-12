//! The canonical identity of a verified mTLS peer certificate.
//!
//! The rules are elitea-main's (`internal/auth/workloadidentity`), so one
//! identity string means the same peer on both sides:
//!
//! * exactly one `spiffe://` URI SAN and no DNS SAN gives that URI;
//! * exactly one DNS SAN and no URI SAN gives `dns:<lower-case name>`;
//! * an e-mail or IP SAN, a wildcard, an IP spelled as a DNS name, a
//!   non-ASCII name, or any other mix gives no identity.

use std::net::IpAddr;

use x509_parser::extensions::GeneralName;
use x509_parser::prelude::{FromDer as _, X509Certificate};

/// The identity of a DER certificate, or `None` when it has no valid one.
#[must_use]
pub fn certificate_identity(der: &[u8]) -> Option<String> {
    let (_, certificate) = X509Certificate::from_der(der).ok()?;
    let san = certificate.subject_alternative_name().ok()??;
    let mut dns = Vec::new();
    let mut uris = Vec::new();
    for name in &san.value.general_names {
        match name {
            GeneralName::DNSName(value) => dns.push(*value),
            GeneralName::URI(value) => uris.push(*value),
            // E-mail, IP and every other kind make the identity ambiguous.
            _ => return None,
        }
    }
    match (dns.as_slice(), uris.as_slice()) {
        ([name], []) => dns_identity(name),
        ([], [uri]) => spiffe_identity(uri),
        _ => None,
    }
}

fn dns_identity(name: &str) -> Option<String> {
    let name = name.trim_end_matches('.').to_ascii_lowercase();
    if name.is_empty()
        || !name.is_ascii()
        || name.contains(['*', '\r', '\n', '\0'])
        || name.parse::<IpAddr>().is_ok()
        || !name
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-' || byte == b'.')
    {
        return None;
    }
    Some(format!("dns:{name}"))
}

fn spiffe_identity(uri: &str) -> Option<String> {
    let rest = uri.strip_prefix("spiffe://")?;
    let (host, path) = rest.split_once('/')?;
    if host.is_empty()
        || host.contains('@')
        || path.is_empty()
        || uri.len() > 512
        || uri.contains(['?', '#', '%', '\r', '\n', '\0'])
    {
        return None;
    }
    Some(uri.to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;
    use rcgen::{CertificateParams, KeyPair, SanType};

    fn der(sans: Vec<SanType>) -> Vec<u8> {
        let mut params = CertificateParams::default();
        params.subject_alt_names = sans;
        let key = KeyPair::generate().expect("key");
        params.self_signed(&key).expect("cert").der().to_vec()
    }

    fn dns(name: &str) -> SanType {
        SanType::DnsName(name.try_into().expect("dns name"))
    }

    fn uri(value: &str) -> SanType {
        SanType::URI(value.try_into().expect("uri"))
    }

    #[test]
    fn one_dns_san_is_the_identity() {
        assert_eq!(
            certificate_identity(&der(vec![dns("Elitea-Main")])).as_deref(),
            Some("dns:elitea-main")
        );
    }

    #[test]
    fn one_spiffe_uri_is_the_identity() {
        let id = "spiffe://elitea.example/runtime/worker";
        assert_eq!(
            certificate_identity(&der(vec![uri(id)])).as_deref(),
            Some(id)
        );
    }

    #[test]
    fn ambiguous_or_invalid_names_have_no_identity() {
        for sans in [
            vec![],
            vec![dns("a.example"), dns("b.example")],
            vec![dns("a.example"), uri("spiffe://elitea.example/a")],
            vec![dns("*.example")],
            vec![dns("192.0.2.1")],
            vec![uri("https://elitea.example/a")],
            vec![uri("spiffe://elitea.example/")],
            vec![uri("spiffe://elitea.example/a?x=1")],
            vec![
                dns("a.example"),
                SanType::IpAddress("192.0.2.1".parse().expect("ip")),
            ],
        ] {
            assert_eq!(certificate_identity(&der(sans.clone())), None, "{sans:?}");
        }
    }

    #[test]
    fn garbage_is_not_a_certificate() {
        assert_eq!(certificate_identity(b"not a certificate"), None);
    }
}
