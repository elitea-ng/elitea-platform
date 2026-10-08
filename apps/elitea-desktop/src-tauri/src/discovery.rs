//! Deployment address handling and the anonymous discovery document
//! (ADR-0025 decision 1: `GET {origin}/.well-known/elitea-client`).
//!
//! The document is untrusted input. Every URL in it must sit on the origin the
//! user typed: following one that does not would send the sign-in, and later
//! the refresh token, to a host the user never confirmed (the mobile client
//! refuses the same way, `src/api/discovery.ts`).

use serde::{Deserialize, Serialize};
use url::{Host, Url};

use crate::error::HostError;

pub const DISCOVERY_PATH: &str = "/.well-known/elitea-client";
/// The native brand pack route the document's `brand_pack_url` must point at.
const BRAND_PACK_PATH: &str = "/api/v2/branding/pack.json";
/// The only client contract major version this app speaks.
const SUPPORTED_CONTRACT_MAJOR: &str = "1";

/// Reduce what the user typed to a bare origin, or refuse it.
///
/// `https` only, except loopback hosts (`localhost`, `127.0.0.1`, `[::1]`),
/// which may use `http` for a local stack. A scheme-less input is read as
/// `https://`. Credentials, path, query and fragment are dropped: an origin is
/// all the rest of the app needs, and a pasted deep link must not leak into the
/// derived endpoints.
pub fn normalize_origin(input: &str) -> Result<Url, HostError> {
    let trimmed = input.trim();
    if trimmed.is_empty() {
        return Err(HostError::InvalidAddress(
            "enter your deployment address".into(),
        ));
    }
    let with_scheme = if trimmed.contains("://") {
        trimmed.to_owned()
    } else {
        format!("https://{trimmed}")
    };
    let parsed = Url::parse(&with_scheme)
        .map_err(|_| HostError::InvalidAddress("that is not a valid address".into()))?;
    if !parsed.username().is_empty() || parsed.password().is_some() {
        return Err(HostError::InvalidAddress(
            "the address must not contain credentials".into(),
        ));
    }
    let loopback = is_loopback(&parsed);
    match parsed.scheme() {
        "https" => {}
        "http" if loopback => {}
        "http" => {
            return Err(HostError::InvalidAddress(
                "the address must use https (http is allowed for localhost only)".into(),
            ));
        }
        _ => {
            return Err(HostError::InvalidAddress(
                "the address must start with https://".into(),
            ));
        }
    }
    let host = parsed
        .host_str()
        .ok_or_else(|| HostError::InvalidAddress("the address has no host".into()))?;
    let mut origin = format!("{}://{host}", parsed.scheme());
    if let Some(port) = parsed.port() {
        origin.push_str(&format!(":{port}"));
    }
    Url::parse(&origin).map_err(|_| HostError::InvalidAddress("that is not a valid address".into()))
}

pub fn is_loopback(url: &Url) -> bool {
    match url.host() {
        Some(Host::Domain(name)) => name.eq_ignore_ascii_case("localhost"),
        Some(Host::Ipv4(ip)) => ip.is_loopback(),
        Some(Host::Ipv6(ip)) => ip.is_loopback(),
        None => false,
    }
}

/// The endpoints of ADR-0025 decision 3, as the deployment publishes them.
#[derive(Debug, Clone, Deserialize, Serialize, PartialEq, Eq)]
pub struct NativeAuth {
    pub issuer: String,
    pub authorization_endpoint: String,
    pub token_endpoint: String,
    pub revocation_endpoint: String,
    #[serde(default)]
    pub code_challenge_methods_supported: Vec<String>,
}

/// The parts of the discovery document this client reads. Unknown fields are
/// ignored: the document is additive within a contract major version.
#[derive(Debug, Clone, Deserialize, Serialize, PartialEq, Eq)]
pub struct Discovery {
    pub server_version: String,
    pub client_contract: String,
    pub deployment_kind: String,
    pub display_name: String,
    pub brand_pack_url: String,
    pub native_auth: Option<NativeAuth>,
}

fn same_origin(candidate: &str, origin: &Url) -> bool {
    Url::parse(candidate).is_ok_and(|url| url.origin() == origin.origin())
}

impl Discovery {
    /// Refuse a document that points anywhere but the origin the user chose, or
    /// that speaks another contract major version.
    pub fn validate(&self, origin: &Url) -> Result<(), HostError> {
        let major = self.client_contract.split('.').next().unwrap_or_default();
        if major != SUPPORTED_CONTRACT_MAJOR {
            return Err(HostError::Unsupported(format!(
                "this deployment speaks client contract {}, which this app does not support",
                self.client_contract
            )));
        }
        let pack_ok = same_origin(&self.brand_pack_url, origin)
            && Url::parse(&self.brand_pack_url).is_ok_and(|u| u.path() == BRAND_PACK_PATH);
        if !pack_ok {
            return Err(HostError::OriginMismatch);
        }
        if let Some(auth) = &self.native_auth {
            let urls = [
                &auth.issuer,
                &auth.authorization_endpoint,
                &auth.token_endpoint,
                &auth.revocation_endpoint,
            ];
            if !urls.iter().all(|u| same_origin(u, origin)) {
                return Err(HostError::OriginMismatch);
            }
        }
        Ok(())
    }

    /// The native auth endpoints, or the reason sign-in is unavailable.
    pub fn native_auth(&self) -> Result<&NativeAuth, HostError> {
        let auth = self.native_auth.as_ref().ok_or_else(|| {
            HostError::Unsupported(
                "this deployment has no native client registered; ask an administrator to register the desktop client".into(),
            )
        })?;
        if !auth
            .code_challenge_methods_supported
            .iter()
            .any(|m| m == "S256")
        {
            return Err(HostError::Unsupported(
                "this deployment does not support PKCE S256".into(),
            ));
        }
        Ok(auth)
    }
}

/// Fetch and validate the discovery document for an origin.
pub async fn fetch_discovery(
    client: &reqwest::Client,
    origin: &Url,
) -> Result<Discovery, HostError> {
    let url = origin
        .join(DISCOVERY_PATH)
        .map_err(|_| HostError::InvalidAddress("that is not a valid address".into()))?;
    let response = client
        .get(url)
        .header("Accept", "application/json")
        .send()
        .await
        .map_err(|_| HostError::Unreachable)?;
    match response.status().as_u16() {
        200 => {}
        404 => {
            return Err(HostError::Unsupported(
                "this address is not an Elitea deployment that supports desktop clients".into(),
            ));
        }
        _ => return Err(HostError::Unavailable),
    }
    let document: Discovery = response.json().await.map_err(|_| {
        HostError::Unsupported("the deployment sent an unreadable discovery document".into())
    })?;
    document.validate(origin)?;
    Ok(document)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn origin(input: &str) -> Url {
        normalize_origin(input).expect("valid origin")
    }

    #[test]
    fn https_origins_are_accepted_and_reduced_to_the_origin() {
        assert_eq!(
            origin("https://elitea.example.com").as_str(),
            "https://elitea.example.com/"
        );
        assert_eq!(
            origin("elitea.example.com").as_str(),
            "https://elitea.example.com/"
        );
        assert_eq!(
            origin("  https://elitea.example.com:8443/app/chat?x=1#frag ").as_str(),
            "https://elitea.example.com:8443/"
        );
    }

    #[test]
    fn plain_http_is_accepted_for_loopback_only() {
        assert_eq!(
            origin("http://localhost:8084").as_str(),
            "http://localhost:8084/"
        );
        assert_eq!(
            origin("http://127.0.0.1:8084").as_str(),
            "http://127.0.0.1:8084/"
        );
        assert_eq!(origin("http://[::1]:8084").as_str(), "http://[::1]:8084/");
        assert!(normalize_origin("http://elitea.example.com").is_err());
        assert!(normalize_origin("http://192.168.1.10").is_err());
        assert!(normalize_origin("http://localhost.evil.example").is_err());
    }

    #[test]
    fn other_schemes_credentials_and_junk_are_refused() {
        for bad in [
            "",
            "   ",
            "ftp://x.example",
            "javascript:alert(1)",
            "https://user:pw@x.example",
            "https://",
        ] {
            assert!(normalize_origin(bad).is_err(), "{bad:?} should be refused");
        }
    }

    fn document(host: &str) -> Discovery {
        Discovery {
            server_version: "1.60".into(),
            client_contract: "1.2".into(),
            deployment_kind: "self_hosted".into(),
            display_name: "Acme".into(),
            brand_pack_url: format!("{host}/api/v2/branding/pack.json"),
            native_auth: Some(NativeAuth {
                issuer: host.into(),
                authorization_endpoint: format!("{host}/api/v2/auth/native/authorize"),
                token_endpoint: format!("{host}/api/v2/auth/native/token"),
                revocation_endpoint: format!("{host}/api/v2/auth/native/revoke"),
                code_challenge_methods_supported: vec!["S256".into()],
            }),
        }
    }

    #[test]
    fn a_document_on_the_chosen_origin_validates() {
        let o = origin("https://elitea.example.com");
        assert!(document("https://elitea.example.com").validate(&o).is_ok());
    }

    #[test]
    fn a_document_naming_another_origin_is_refused() {
        let o = origin("https://elitea.example.com");
        let mut doc = document("https://elitea.example.com");
        doc.native_auth.as_mut().unwrap().token_endpoint = "https://evil.example/token".into();
        assert!(matches!(doc.validate(&o), Err(HostError::OriginMismatch)));

        let mut doc = document("https://elitea.example.com");
        doc.brand_pack_url = "https://evil.example/api/v2/branding/pack.json".into();
        assert!(matches!(doc.validate(&o), Err(HostError::OriginMismatch)));

        let mut doc = document("https://elitea.example.com");
        doc.brand_pack_url = "https://elitea.example.com/somewhere/else.json".into();
        assert!(matches!(doc.validate(&o), Err(HostError::OriginMismatch)));
    }

    #[test]
    fn a_different_contract_major_is_refused() {
        let o = origin("https://elitea.example.com");
        let mut doc = document("https://elitea.example.com");
        doc.client_contract = "2.0".into();
        assert!(matches!(doc.validate(&o), Err(HostError::Unsupported(_))));
    }

    #[test]
    fn sign_in_needs_registered_native_auth_with_s256() {
        let mut doc = document("https://elitea.example.com");
        assert!(doc.native_auth().is_ok());
        doc.native_auth
            .as_mut()
            .unwrap()
            .code_challenge_methods_supported = vec!["plain".into()];
        assert!(doc.native_auth().is_err());
        doc.native_auth = None;
        assert!(doc.native_auth().is_err());
    }

    #[test]
    fn the_document_parses_and_ignores_unknown_fields() {
        let json = r#"{"server_version":"1.60","client_contract":"1.0","deployment_kind":"saas",
            "display_name":"Elitea","brand_pack_url":"https://h.example/api/v2/branding/pack.json",
            "native_auth":null,"client_policy":{"min_client_version":"0.1.0"},"extra":true}"#;
        let doc: Discovery = serde_json::from_str(json).expect("parses");
        assert_eq!(doc.display_name, "Elitea");
        assert!(doc.native_auth.is_none());
    }
}
