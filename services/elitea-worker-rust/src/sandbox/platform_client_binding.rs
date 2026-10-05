//! Content binding only. The existing signed prepared fingerprint is the authority input.
use serde::{Deserialize, Serialize};

use super::request::InvalidRequest;

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct PlatformClientBinding {
    revision: u8,
    policy_sha256: String,
    max_calls: u16,
    max_total_bytes: u32,
}

impl PlatformClientBinding {
    /// Construct from an operator-selected policy before signing the prepared request.
    pub(crate) fn new(
        policy_sha256: String,
        max_calls: u16,
        max_total_bytes: u32,
    ) -> Result<Self, InvalidRequest> {
        let binding = Self {
            revision: 1,
            policy_sha256,
            max_calls,
            max_total_bytes,
        };
        binding.validate()?;
        Ok(binding)
    }

    pub(crate) fn validate(&self) -> Result<(), InvalidRequest> {
        if self.revision != 1
            || self.policy_sha256.len() != 64
            || !self
                .policy_sha256
                .bytes()
                .all(|v| v.is_ascii_digit() || (b'a'..=b'f').contains(&v))
            || !(1..=4096).contains(&self.max_calls)
            || !(1..=64 * 1024 * 1024).contains(&self.max_total_bytes)
        {
            return Err(InvalidRequest);
        }
        Ok(())
    }

    #[allow(
        dead_code,
        reason = "Retain required protocol foundations without enabling deferred execution paths."
    )]
    pub(crate) fn matches(&self, trusted: &Self) -> bool {
        self.revision == trusted.revision
            && self.policy_sha256 == trusted.policy_sha256
            && self.max_calls == trusted.max_calls
            && self.max_total_bytes == trusted.max_total_bytes
    }

    pub(crate) fn max_calls(&self) -> u16 {
        self.max_calls
    }
    pub(crate) fn max_total_bytes(&self) -> u32 {
        self.max_total_bytes
    }
    pub(crate) fn policy_sha256(&self) -> &str {
        &self.policy_sha256
    }
}

// Deployment-owned limits use the same ordered bytes and domain as Main.
// This type is not an authored YAML object or a runtime endpoint selector.
#[derive(Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct PlatformClientPolicy {
    revision: u8,
    max_calls: u16,
    max_total_bytes: u32,
}
impl PlatformClientPolicy {
    pub(crate) fn new(max_calls: u16, max_total_bytes: u32) -> Result<Self, InvalidRequest> {
        let policy = Self {
            revision: 1,
            max_calls,
            max_total_bytes,
        };
        if !(1..=4096).contains(&max_calls) || !(1..=64 * 1024 * 1024).contains(&max_total_bytes) {
            return Err(InvalidRequest);
        }
        Ok(policy)
    }
    pub(crate) fn binding(&self) -> Result<PlatformClientBinding, InvalidRequest> {
        let raw = serde_json::to_vec(self).map_err(|_| InvalidRequest)?;
        let mut hash = ring::digest::Context::new(&ring::digest::SHA256);
        hash.update(b"elitea.code.platform-policy.v1\0");
        hash.update(&raw);
        let digest = hash.finish();
        let mut text = String::with_capacity(64);
        for byte in digest.as_ref() {
            use std::fmt::Write as _;
            write!(&mut text, "{byte:02x}").map_err(|_| InvalidRequest)?;
        }
        PlatformClientBinding::new(text, self.max_calls, self.max_total_bytes)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn operator_policy_uses_exact_cross_language_bytes_and_digest() {
        let policy = PlatformClientPolicy::new(32, 1_048_576).unwrap();
        assert_eq!(
            serde_json::to_vec(&policy).unwrap(),
            br#"{"revision":1,"max_calls":32,"max_total_bytes":1048576}"#
        );
        assert_eq!(
            policy.binding().unwrap().policy_sha256(),
            "a299856c1b28b5aee8488da91e8dd2cb33251c89b677733b63447179144d03da"
        );
        let changed = PlatformClientPolicy::new(33, 1_048_576).unwrap();
        assert!(
            !policy
                .binding()
                .unwrap()
                .matches(&changed.binding().unwrap())
        );
        assert!(PlatformClientPolicy::new(0, 1).is_err());
        assert!(PlatformClientPolicy::new(4097, 1).is_err());
        assert!(PlatformClientPolicy::new(1, 67_108_865).is_err());
    }
}
