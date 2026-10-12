//! The `bitbucket` settings live in `elitea-connectors` (ADR-0030 decision
//! 3); the family reaches them here.

#[allow(unused_imports)] // `BitbucketHosting` is the suites' alone.
pub(crate) use elitea_connectors::bitbucket::config::{
    BitbucketConfigError, BitbucketConfigErrorCode, BitbucketHosting, BitbucketToolkitConfig,
};
