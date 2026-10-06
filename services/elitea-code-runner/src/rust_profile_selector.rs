//! Fixed image assets only. The supervisor authenticates the policy and image pair.
use super::{ErrorCode, PrepareError, Result, TEMPLATE, WRAPPER};

pub(crate) const BROKER_POLICY: &str = "cargo-broker-execute-v1";
const BROKER_TEMPLATE: &str = include_str!("../adapters/rust/platform-v1.Cargo.toml");
const BROKER_SOURCES: &[(&str, &str)] = &[
    (
        "src/main.rs",
        include_str!("../adapters/rust/src/main_platform.rs"),
    ),
    (
        "src/platform.rs",
        include_str!("../adapters/rust/src/platform.rs"),
    ),
    (
        "src/platform_client.rs",
        include_str!("../adapters/rust/src/platform_client.rs"),
    ),
    (
        "src/platform_pipe.rs",
        include_str!("../adapters/rust/src/platform_pipe.rs"),
    ),
];
const LEGACY_SOURCES: &[(&str, &str)] = &[("src/main.rs", WRAPPER)];

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Profile {
    Legacy,
    Broker,
}
impl Profile {
    pub(crate) fn for_policy(policy: &str) -> Result<Self> {
        if policy.is_empty()
            || policy.len() > 128
            || !policy
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-'))
        {
            return Err(Self::invalid());
        }
        // Legacy authority stays with the supervisor's exact admitted image/policy pair.
        // No caller-controlled template, file, path or argument enters this selection.
        Ok(if policy == BROKER_POLICY {
            Self::Broker
        } else {
            Self::Legacy
        })
    }
    pub(crate) fn for_execution(policy: &str, broker: bool) -> Result<Self> {
        let profile = Self::for_policy(policy)?;
        if (profile == Self::Broker) != broker {
            return Err(Self::invalid());
        }
        Ok(profile)
    }
    pub(crate) fn template(self) -> &'static str {
        match self {
            Self::Legacy => TEMPLATE,
            Self::Broker => BROKER_TEMPLATE,
        }
    }
    pub(crate) fn wrapper(self) -> &'static str {
        self.sources()[0].1
    }
    pub(crate) fn sources(self) -> &'static [(&'static str, &'static str)] {
        match self {
            Self::Legacy => LEGACY_SOURCES,
            Self::Broker => BROKER_SOURCES,
        }
    }
    pub(crate) fn validate_dependencies(self, dependencies: &toml::Table) -> Result<()> {
        if self == Self::Broker
            && dependencies.iter().any(|(name, value)| {
                name.replace('-', "_") == "serde"
                    || value
                        .get("package")
                        .and_then(toml::Value::as_str)
                        .is_some_and(|name| name.replace('-', "_") == "serde")
            })
        {
            return Err(PrepareError::new(
                ErrorCode::InvalidDeclaration,
                "Image broker dependency cannot be overridden",
            ));
        }
        Ok(())
    }
    fn invalid() -> PrepareError {
        PrepareError::new(
            ErrorCode::InvalidRequest,
            "Image Cargo broker profile does not match its request",
        )
    }
}

#[cfg(test)]
#[path = "rust_profile_selector_tests.rs"]
mod tests;
