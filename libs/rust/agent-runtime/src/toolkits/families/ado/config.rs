//! The Azure DevOps connection and toolkit settings live in
//! `elitea-connectors` (ADR-0030 decision 3); the families reach them here.

#[allow(unused_imports)] // `AdoFamilySettings` is the suites' alone.
pub(crate) use elitea_connectors::ado::config::{
    AdoConfigError, AdoConfigErrorCode, AdoConnection, AdoFamilySettings, AdoToolkitConfig,
};
