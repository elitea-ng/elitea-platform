//! Operator-selected compiled profile. Main independently checks every grant.
use super::compiled_snapshot::{Binding, ContentSha256, SnapshotProfile};
use crate::config::read_regular_file;
use serde::{Deserialize, Serialize};
use std::{collections::BTreeSet, path::PathBuf, sync::Arc};

const PROFILE_BYTES: usize = 1024 * 1024;
const PROFILE_COUNT: usize = 64;

/// Omit this setting to leave compiled snapshots disabled.
/// Select one exact release profile for the configured Rust runtime.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RustCompiledSnapshotConfig {
    pub profiles_file: PathBuf,
    pub profiles_sha256: String,
    pub dependency_bundle_sha256: String,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, thiserror::Error)]
pub(crate) enum CompiledProfileError {
    #[error("compiled snapshot profile settings are incomplete or invalid")]
    Configuration,
    #[error("compiled snapshot release file is not a bounded canonical regular file")]
    File,
    #[error("compiled snapshot release file does not match its immutable SHA-256 pin")]
    Pin,
    #[error("compiled snapshot release file is not exact revision 1 canonical JSON")]
    Contract,
    #[error("compiled snapshot release profiles are invalid or duplicated")]
    Profiles,
    #[error("compiled snapshot runtime requires one exact image, policy and bundle profile")]
    Selection,
    #[error("compiled snapshot release profile does not match the native runtime platform")]
    Platform,
}

impl CompiledProfileError {
    #[cfg(feature = "sandbox-supervisor")]
    pub(crate) const fn operation(self) -> &'static str {
        match self {
            Self::Configuration => "validate compiled snapshot profile settings",
            Self::File => "read bounded regular compiled snapshot profiles",
            Self::Pin => "verify the compiled snapshot release file pin",
            Self::Contract => "parse exact compiled snapshot release JSON",
            Self::Profiles => "validate finite compiled snapshot release profiles",
            Self::Selection => "select one exact compiled image, policy and bundle profile",
            Self::Platform => "match the compiled snapshot native runtime platform",
        }
    }
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct ReleaseProfile {
    binding: Binding,
    dependency_bundle_sha256: String,
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct ReleaseProfiles {
    revision: u32,
    profiles: Vec<ReleaseProfile>,
}

impl RustCompiledSnapshotConfig {
    pub(crate) fn validate(&self) -> Result<(), CompiledProfileError> {
        if !self.profiles_file.is_absolute()
            || self.profiles_file.file_name().is_none()
            || self.profiles_file.components().any(|part| {
                !matches!(
                    part,
                    std::path::Component::RootDir | std::path::Component::Normal(_)
                )
            })
            || ContentSha256::parse(self.profiles_sha256.clone()).is_err()
            || (!self.dependency_bundle_sha256.is_empty()
                && ContentSha256::parse(self.dependency_bundle_sha256.clone()).is_err())
        {
            return Err(CompiledProfileError::Configuration);
        }
        Ok(())
    }

    pub(crate) fn load(
        &self,
        image: &str,
        policy: &str,
        platform: Option<&super::native_bundle::NativePlatform>,
    ) -> Result<Arc<SnapshotProfile>, CompiledProfileError> {
        self.validate()?;
        let raw = read_regular_file(
            &self.profiles_file,
            PROFILE_BYTES,
            false,
            "compiled snapshot release profiles",
        )
        .map_err(|_| CompiledProfileError::File)?;
        self.parse(&raw, image, policy, platform)
    }

    fn parse(
        &self,
        raw: &[u8],
        image: &str,
        policy: &str,
        platform: Option<&super::native_bundle::NativePlatform>,
    ) -> Result<Arc<SnapshotProfile>, CompiledProfileError> {
        self.validate()?;
        if raw.is_empty() || raw.len() > PROFILE_BYTES {
            return Err(CompiledProfileError::File);
        }
        if ContentSha256::of(raw).as_str() != self.profiles_sha256 {
            return Err(CompiledProfileError::Pin);
        }
        let manifest: ReleaseProfiles =
            serde_json::from_slice(raw).map_err(|_| CompiledProfileError::Contract)?;
        if manifest.revision != 1
            || manifest.profiles.is_empty()
            || manifest.profiles.len() > PROFILE_COUNT
            || serde_json::to_vec(&manifest).map_err(|_| CompiledProfileError::Contract)? != raw
        {
            return Err(CompiledProfileError::Contract);
        }
        let mut seen = BTreeSet::new();
        let mut selected = None;
        for profile in manifest.profiles {
            let key = profile
                .binding
                .key()
                .map_err(|_| CompiledProfileError::Profiles)?;
            if (!profile.dependency_bundle_sha256.is_empty()
                && ContentSha256::parse(profile.dependency_bundle_sha256.clone()).is_err())
                || !seen.insert((
                    key.as_str().to_owned(),
                    profile.dependency_bundle_sha256.clone(),
                ))
            {
                return Err(CompiledProfileError::Profiles);
            }
            if profile.binding.compilation_image_digest == image
                && profile.binding.execution_image_digest == image
                && profile.binding.policy_revision == policy
                && profile.dependency_bundle_sha256 == self.dependency_bundle_sha256
                && selected.replace(profile.binding).is_some()
            {
                return Err(CompiledProfileError::Selection);
            }
        }
        let selected = selected.ok_or(CompiledProfileError::Selection)?;
        if let Some(platform) = platform {
            if platform.validate().is_err()
                || selected.platform
                    != format!("{}/{}/{}", platform.os, platform.arch, platform.abi)
            {
                return Err(CompiledProfileError::Platform);
            }
        } else if !self.dependency_bundle_sha256.is_empty() {
            return Err(CompiledProfileError::Platform);
        }
        let dependency_bundle_sha256 = if self.dependency_bundle_sha256.is_empty() {
            None
        } else {
            Some(
                ContentSha256::parse(self.dependency_bundle_sha256.clone())
                    .map_err(|_| CompiledProfileError::Configuration)?,
            )
        };
        SnapshotProfile::with_dependency_bundle(selected, dependency_bundle_sha256)
            .map(Arc::new)
            .map_err(|_| CompiledProfileError::Profiles)
    }
}

pub(crate) fn non_null_config<'de, D: serde::Deserializer<'de>>(
    deserializer: D,
) -> Result<Option<RustCompiledSnapshotConfig>, D::Error> {
    RustCompiledSnapshotConfig::deserialize(deserializer).map(Some)
}

pub(crate) fn load_worker_profile(
    runtimes: &[crate::config::SandboxRuntimeConfig],
) -> Result<Option<Arc<SnapshotProfile>>, CompiledProfileError> {
    let mut selected = None;
    for runtime in runtimes {
        let Some(config) = &runtime.compiled_snapshot else {
            continue;
        };
        if runtime.language != super::request::Language::Rust || selected.is_some() {
            return Err(CompiledProfileError::Configuration);
        }
        selected = Some(
            config.load(
                &runtime.image_digest,
                &runtime.policy_revision,
                runtime
                    .preparation
                    .as_ref()
                    .and_then(|p| p.native_platform.as_ref()),
            )?,
        );
    }
    Ok(selected)
}

#[cfg(test)]
#[path = "compiled_profile_config_tests.rs"]
mod tests;
