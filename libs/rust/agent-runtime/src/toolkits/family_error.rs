//! One mapping from a ported family's config and toolset error codes to the
//! materialization outcome (#1207 review round 2).
//!
//! Each family ported in #1207 used to carry its own
//! `*_toolset_materialization_error` function or an inline closure, and they
//! disagreed: a `ResourceExhausted` became `resource_exhausted` for ado, aha
//! and jira but `invalid_configuration` for bitbucket, gitlab, qtest, testio,
//! testrail and xray. Every family now states only what its code MEANS
//! ([`FamilyFailure`]); [`family_error`] is the one place that decides what
//! that meaning does to the agent:
//!
//! * `UnsupportedSelection` → unsupported toolkit (skipped, as before).
//! * `ResourceExhausted` → resource exhausted.
//! * `UnsupportedCapability` → a NAMED refusal: the toolkit is left out and
//!   the run says why (`materialize::RefusedToolkit`).
//! * everything else → invalid configuration.
//!
//! The pre-existing families (artifact, openapi, github, sharepoint and the
//! `map_err(|_| invalid_configuration())` ones) keep their own mapping: their
//! behaviour is not this change's to alter.

use super::families::UnsupportedSetting;
use super::families::ado::config::AdoConfigErrorCode;
use super::families::ado::toolset::AdoToolsetErrorCode;
use super::families::aha::config::AhaConfigErrorCode;
use super::families::aha::tools::AhaToolsetErrorCode;
use super::families::bigquery::config::BigQueryConfigErrorCode;
use super::families::bigquery::tools::BigQueryToolsetErrorCode;
use super::families::bitbucket::config::BitbucketConfigErrorCode;
use super::families::bitbucket::tools::BitbucketToolsetErrorCode;
use super::families::carrier::config::CarrierConfigErrorCode;
use super::families::carrier::tools::CarrierToolsetErrorCode;
use super::families::confluence::config::ConfluenceConfigErrorCode;
use super::families::confluence::tools::ConfluenceToolsetErrorCode;
use super::families::figma::config::FigmaConfigErrorCode;
use super::families::figma::tools::FigmaToolsetErrorCode;
use super::families::gitlab::config::GitLabConfigErrorCode;
use super::families::gitlab::tools::GitLabToolsetErrorCode;
use super::families::jira::config::JiraConfigErrorCode;
use super::families::jira::tools::JiraToolsetErrorCode;
use super::families::qtest::config::QtestConfigErrorCode;
use super::families::qtest::tools::QtestToolsetErrorCode;
use super::families::testio::config::TestIoConfigErrorCode;
use super::families::testio::tools::TestIoToolsetErrorCode;
use super::families::testrail::config::TestRailConfigErrorCode;
use super::families::testrail::tools::TestRailToolsetErrorCode;
use super::families::xray_cloud::config::XrayConfigErrorCode;
use super::families::xray_cloud::tools::XrayToolsetErrorCode;
use super::families::zephyr_rest::config::ZephyrRestConfigErrorCode;
use super::families::zephyr_rest::tools::ZephyrRestToolsetErrorCode;
use super::materialize::{ToolsetMaterializationError, ToolsetMaterializationErrorCode};

/// What a family's error code means, independent of the family.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum FamilyFailure {
    InvalidConfiguration,
    ResourceExhausted,
    /// The selected tools name nothing this runtime serves.
    UnsupportedSelection,
    /// A configured setting this runtime deliberately refuses.
    UnsupportedCapability(UnsupportedSetting),
}

/// A family error code that says what it means.
pub(super) trait FamilyErrorCode: Copy {
    fn failure(self) -> FamilyFailure;
}

/// The one mapping every family ported in #1207 uses.
pub(super) fn family_error(code: impl FamilyErrorCode) -> ToolsetMaterializationError {
    match code.failure() {
        FamilyFailure::InvalidConfiguration => {
            ToolsetMaterializationError::new(ToolsetMaterializationErrorCode::InvalidConfiguration)
        }
        FamilyFailure::ResourceExhausted => {
            ToolsetMaterializationError::new(ToolsetMaterializationErrorCode::ResourceExhausted)
        }
        FamilyFailure::UnsupportedSelection => {
            ToolsetMaterializationError::new(ToolsetMaterializationErrorCode::UnsupportedToolkit)
        }
        FamilyFailure::UnsupportedCapability(setting) => {
            ToolsetMaterializationError::refused(setting)
        }
    }
}

/// Config codes that are only `InvalidConfiguration`/`ResourceExhausted`.
macro_rules! config_codes {
    ($($code:ident),+ $(,)?) => {$(
        impl FamilyErrorCode for $code {
            fn failure(self) -> FamilyFailure {
                match self {
                    Self::InvalidConfiguration => FamilyFailure::InvalidConfiguration,
                    Self::ResourceExhausted => FamilyFailure::ResourceExhausted,
                }
            }
        }
    )+};
}

/// Toolset codes with the common five variants.
macro_rules! toolset_codes {
    ($($code:ident),+ $(,)?) => {$(
        impl FamilyErrorCode for $code {
            fn failure(self) -> FamilyFailure {
                match self {
                    Self::InvalidConfiguration | Self::Client | Self::InvalidDefinition => {
                        FamilyFailure::InvalidConfiguration
                    }
                    Self::ResourceExhausted => FamilyFailure::ResourceExhausted,
                    Self::UnsupportedSelection => FamilyFailure::UnsupportedSelection,
                }
            }
        }
    )+};
}

config_codes!(
    AdoConfigErrorCode,
    AhaConfigErrorCode,
    BigQueryConfigErrorCode,
    BitbucketConfigErrorCode,
    CarrierConfigErrorCode,
    FigmaConfigErrorCode,
    GitLabConfigErrorCode,
    QtestConfigErrorCode,
    TestIoConfigErrorCode,
    TestRailConfigErrorCode,
    XrayConfigErrorCode,
    ZephyrRestConfigErrorCode,
);

toolset_codes!(
    AdoToolsetErrorCode,
    AhaToolsetErrorCode,
    BigQueryToolsetErrorCode,
    BitbucketToolsetErrorCode,
    CarrierToolsetErrorCode,
    FigmaToolsetErrorCode,
    GitLabToolsetErrorCode,
    QtestToolsetErrorCode,
    TestIoToolsetErrorCode,
    TestRailToolsetErrorCode,
    XrayToolsetErrorCode,
    ZephyrRestToolsetErrorCode,
);

impl FamilyErrorCode for JiraConfigErrorCode {
    fn failure(self) -> FamilyFailure {
        match self {
            Self::InvalidConfiguration => FamilyFailure::InvalidConfiguration,
            Self::ResourceExhausted => FamilyFailure::ResourceExhausted,
            Self::UnsupportedCapability(setting) => FamilyFailure::UnsupportedCapability(setting),
        }
    }
}

impl FamilyErrorCode for JiraToolsetErrorCode {
    fn failure(self) -> FamilyFailure {
        match self {
            Self::InvalidConfiguration | Self::Client | Self::InvalidDefinition => {
                FamilyFailure::InvalidConfiguration
            }
            Self::ResourceExhausted => FamilyFailure::ResourceExhausted,
            Self::UnsupportedSelection => FamilyFailure::UnsupportedSelection,
            Self::UnsupportedCapability(setting) => FamilyFailure::UnsupportedCapability(setting),
        }
    }
}

impl FamilyErrorCode for ConfluenceConfigErrorCode {
    fn failure(self) -> FamilyFailure {
        match self {
            Self::InvalidConfiguration => FamilyFailure::InvalidConfiguration,
            Self::ResourceExhausted => FamilyFailure::ResourceExhausted,
            Self::UnsupportedCapability(setting) => FamilyFailure::UnsupportedCapability(setting),
        }
    }
}

impl FamilyErrorCode for ConfluenceToolsetErrorCode {
    fn failure(self) -> FamilyFailure {
        match self {
            Self::InvalidConfiguration | Self::Client | Self::InvalidDefinition => {
                FamilyFailure::InvalidConfiguration
            }
            Self::ResourceExhausted => FamilyFailure::ResourceExhausted,
            Self::UnsupportedSelection => FamilyFailure::UnsupportedSelection,
            Self::UnsupportedCapability(setting) => FamilyFailure::UnsupportedCapability(setting),
        }
    }
}
