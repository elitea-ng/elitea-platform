//! Cargo-owned native-v2 payload. The outer native envelope owns its root and grant binding.
use super::dependency_bundle::{DependencyBundleFile, InvalidDependencyBundle};
use super::native_bundle::NativePlatform;
use serde::{Deserialize, Serialize};

pub(crate) const CARGO_RECORD_LIMIT: u64 = 8 * 1024 * 1024;
pub(crate) const CARGO_ARCHIVE_LIMIT: u64 = 128 * 1024 * 1024;

#[derive(Clone, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub(crate) struct CargoProfile {
    pub(crate) preparation_image: String,
    pub(crate) execution_image: String,
    pub(crate) rust_revision: String,
    pub(crate) os: String,
    pub(crate) arch: String,
    pub(crate) target: String,
    pub(crate) policy_revision: String,
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub(crate) enum CargoObjectRole {
    Record,
    Archive,
}

#[derive(Clone, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub(crate) struct CargoObject {
    pub(crate) role: CargoObjectRole,
    pub(crate) name: String,
    pub(crate) bytes: u64,
    pub(crate) sha256: String,
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct CargoPayload {
    pub(crate) revision: u8,
    pub(crate) preparation_sha256: String,
    pub(crate) declaration_sha256: String,
    pub(crate) profile: CargoProfile,
    pub(crate) objects: [CargoObject; 2],
    #[serde(skip)]
    wire_files: Vec<DependencyBundleFile>,
}

fn sha256(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}
fn image(value: &str) -> bool {
    value.strip_prefix("sha256:").is_some_and(sha256)
}
fn identifier(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'))
}

impl CargoPayload {
    /// Decode typed bytes before constructing the root-bound envelope.
    pub(crate) fn from_bytes(bytes: &[u8]) -> Result<Self, InvalidDependencyBundle> {
        if bytes.len() > 128 * 1024 {
            return Err(InvalidDependencyBundle);
        }
        let mut payload: Self =
            serde_json::from_slice(bytes).map_err(|_| InvalidDependencyBundle)?;
        payload.validate()?;
        payload.wire_files = payload
            .objects
            .iter()
            .map(|object| DependencyBundleFile {
                name: object.name.clone(),
                bytes: object.bytes,
                sha256: object.sha256.clone(),
            })
            .collect();
        Ok(payload)
    }
    pub(crate) fn validate(&self) -> Result<(), InvalidDependencyBundle> {
        let expected_target = match self.profile.arch.as_str() {
            "amd64" => "x86_64-unknown-linux-gnu",
            "arm64" => "aarch64-unknown-linux-gnu",
            _ => return Err(InvalidDependencyBundle),
        };
        if self.revision != 1
            || !sha256(&self.preparation_sha256)
            || !sha256(&self.declaration_sha256)
            || !image(&self.profile.preparation_image)
            || !image(&self.profile.execution_image)
            || self.profile.rust_revision != "1.97.1"
            || self.profile.os != "linux"
            || self.profile.target != expected_target
            || !identifier(&self.profile.policy_revision)
        {
            return Err(InvalidDependencyBundle);
        }
        for (index, object) in self.objects.iter().enumerate() {
            let (role, limit) = if index == 0 {
                (CargoObjectRole::Record, CARGO_RECORD_LIMIT)
            } else {
                (CargoObjectRole::Archive, CARGO_ARCHIVE_LIMIT)
            };
            if object.role != role
                || object.bytes == 0
                || object.bytes > limit
                || !sha256(&object.sha256)
                || object.name != format!("{}.blob", object.sha256)
            {
                return Err(InvalidDependencyBundle);
            }
        }
        if self.objects[0].name == self.objects[1].name {
            return Err(InvalidDependencyBundle);
        }
        Ok(())
    }
    pub(crate) fn parse(value: &serde_json::Value) -> Result<Self, InvalidDependencyBundle> {
        Self::from_bytes(&serde_json::to_vec(value).map_err(|_| InvalidDependencyBundle)?)
    }
    pub(crate) fn files(&self) -> &[DependencyBundleFile] {
        &self.wire_files
    }
    pub(crate) fn validate_binding(
        &self,
        preparation: &str,
        source: &str,
        platform: &NativePlatform,
        execution_image: &str,
        policy: &str,
    ) -> Result<(), InvalidDependencyBundle> {
        self.validate()?;
        if self.preparation_sha256 != preparation
            || self.declaration_sha256 != source
            || self.profile.os != platform.os
            || self.profile.arch != platform.arch
            || platform.abi != "gnu"
            || self.profile.execution_image != execution_image
            || self.profile.policy_revision != policy
        {
            return Err(InvalidDependencyBundle);
        }
        Ok(())
    }
    #[cfg(test)]
    fn wire_file(&self, index: usize) -> Option<&CargoObject> {
        self.objects.get(index)
    }
    #[cfg(test)]
    fn validate_execution(
        &self,
        declaration_sha256: &str,
        image_digest: &str,
        policy_revision: &str,
    ) -> Result<(), InvalidDependencyBundle> {
        self.validate()?;
        if self.declaration_sha256 != declaration_sha256
            || self.profile.execution_image != image_digest
            || self.profile.policy_revision != policy_revision
        {
            return Err(InvalidDependencyBundle);
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn payload() -> CargoPayload {
        CargoPayload {
            revision: 1,
            preparation_sha256: "a".repeat(64),
            declaration_sha256: "b".repeat(64),
            profile: CargoProfile {
                preparation_image: format!("sha256:{}", "c".repeat(64)),
                execution_image: format!("sha256:{}", "d".repeat(64)),
                rust_revision: "1.97.1".into(),
                os: "linux".into(),
                arch: "amd64".into(),
                target: "x86_64-unknown-linux-gnu".into(),
                policy_revision: "rust-offline-v1".into(),
            },
            wire_files: Vec::new(),
            objects: [
                CargoObject {
                    role: CargoObjectRole::Record,
                    name: format!("{}.blob", "e".repeat(64)),
                    bytes: 1024,
                    sha256: "e".repeat(64),
                },
                CargoObject {
                    role: CargoObjectRole::Archive,
                    name: format!("{}.blob", "f".repeat(64)),
                    bytes: 4096,
                    sha256: "f".repeat(64),
                },
            ],
        }
    }
    #[test]
    fn binds_profile_and_order_without_inventory_paths() {
        let p = payload();
        p.validate_binding(
            &"a".repeat(64),
            &"b".repeat(64),
            &NativePlatform {
                os: "linux".into(),
                arch: "amd64".into(),
                abi: "gnu".into(),
            },
            &format!("sha256:{}", "d".repeat(64)),
            "rust-offline-v1",
        )
        .unwrap();
        p.validate_execution(
            &"b".repeat(64),
            &format!("sha256:{}", "d".repeat(64)),
            "rust-offline-v1",
        )
        .unwrap();
        assert_eq!(p.wire_file(0).unwrap().role, CargoObjectRole::Record);
        assert!(p.wire_file(2).is_none());
        assert!(
            p.validate_binding(
                &"a".repeat(64),
                &"b".repeat(64),
                &NativePlatform {
                    os: "linux".into(),
                    arch: "arm64".into(),
                    abi: "gnu".into()
                },
                &format!("sha256:{}", "d".repeat(64)),
                "rust-offline-v1"
            )
            .is_err()
        );
        let mut changed = payload();
        changed.objects.swap(0, 1);
        assert!(changed.validate().is_err());
        changed = payload();
        changed.profile.target = "x86_64-pc-windows-msvc".into();
        assert!(changed.validate().is_err());
        changed = payload();
        changed.objects[1].bytes = CARGO_ARCHIVE_LIMIT + 1;
        assert!(changed.validate().is_err());
        changed = payload();
        changed.objects[0].name = "../../record.json".into();
        assert!(changed.validate().is_err());
        assert!(
            p.validate_execution(
                &"b".repeat(64),
                &format!("sha256:{}", "c".repeat(64)),
                "rust-offline-v1"
            )
            .is_err()
        );
    }
    #[test]
    fn rejects_unknown_duplicate_and_incomplete_payloads() {
        let bytes = serde_json::to_vec(&payload()).unwrap();
        assert!(CargoPayload::from_bytes(&bytes).is_ok());
        let mut value: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        value["objects"][0]["path"] = serde_json::json!("Cargo.lock");
        assert!(CargoPayload::from_bytes(&serde_json::to_vec(&value).unwrap()).is_err());
        let text = String::from_utf8(bytes).unwrap().replacen(
            "\"revision\":1",
            "\"revision\":1,\"revision\":1",
            1,
        );
        assert!(CargoPayload::from_bytes(text.as_bytes()).is_err());
    }
}
