//! PKCE (RFC 7636, S256 only) and the `state` value of the native
//! authorization request (RFC 8252, ADR-0025 decision 3).
//!
//! Logic follows the mobile client's `src/auth/pkce.ts`; randomness is the OS
//! CSPRNG (`rand`'s thread-local, seeded from it) and the digest is SHA-256.

use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use rand::RngCore as _;
use sha2::{Digest as _, Sha256};

/// Bytes of entropy in a verifier and in a `state` (43 base64url characters).
const SECRET_BYTES: usize = 32;

/// A PKCE pair. The verifier never leaves this process except in the token
/// request; only the challenge goes into the browser URL.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Pkce {
    pub verifier: String,
    pub challenge: String,
}

/// Unpadded base64url (RFC 4648 section 5).
fn base64url(bytes: &[u8]) -> String {
    URL_SAFE_NO_PAD.encode(bytes)
}

fn random_token() -> String {
    let mut bytes = [0_u8; SECRET_BYTES];
    rand::rng().fill_bytes(&mut bytes);
    base64url(&bytes)
}

/// The S256 challenge of a verifier: `BASE64URL(SHA256(ASCII(verifier)))`.
pub fn code_challenge(verifier: &str) -> String {
    base64url(&Sha256::digest(verifier.as_bytes()))
}

/// A fresh verifier of 43 unreserved characters and its S256 challenge.
pub fn create_pkce() -> Pkce {
    let verifier = random_token();
    let challenge = code_challenge(&verifier);
    Pkce {
        verifier,
        challenge,
    }
}

/// An unguessable `state` (43 base64url characters).
pub fn create_state() -> String {
    random_token()
}

/// Comparison whose time does not depend on where two strings first differ,
/// for the `state` check. Lengths are folded into the result, not short-circuited.
pub fn constant_time_eq(a: &str, b: &str) -> bool {
    let (a, b) = (a.as_bytes(), b.as_bytes());
    let mut diff = a.len() ^ b.len();
    for i in 0..a.len().max(b.len()) {
        diff |= usize::from(a.get(i).copied().unwrap_or(0) ^ b.get(i).copied().unwrap_or(0));
    }
    diff == 0
}

#[cfg(test)]
mod tests {
    use super::*;

    /// RFC 7636 appendix B.
    #[test]
    fn challenge_matches_the_rfc_vector() {
        assert_eq!(
            code_challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"),
            "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
        );
    }

    #[test]
    fn verifier_is_43_unreserved_characters_and_matches_its_challenge() {
        let pkce = create_pkce();
        assert_eq!(pkce.verifier.len(), 43);
        assert!(
            pkce.verifier
                .chars()
                .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_')
        );
        assert_eq!(pkce.challenge, code_challenge(&pkce.verifier));
        assert_eq!(pkce.challenge.len(), 43);
    }

    #[test]
    fn values_are_not_repeated() {
        assert_ne!(create_pkce().verifier, create_pkce().verifier);
        assert_ne!(create_state(), create_state());
        assert_eq!(create_state().len(), 43);
    }

    #[test]
    fn constant_time_eq_is_exact() {
        assert!(constant_time_eq("abc", "abc"));
        assert!(!constant_time_eq("abc", "abd"));
        assert!(!constant_time_eq("abc", "abcd"));
        assert!(!constant_time_eq("", "a"));
        assert!(constant_time_eq("", ""));
    }
}
