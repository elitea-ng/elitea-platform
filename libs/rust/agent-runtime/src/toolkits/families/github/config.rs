//! The `github` settings live in `elitea-connectors` (ADR-0030 decision 3);
//! the family reaches them here.

#[allow(unused_imports)] // `GitHubAuthKind` is the suites' alone.
pub(crate) use elitea_connectors::github::config::{
    GitHubAuthKind, GitHubToolkitConfig, GitHubToolkitConfigError,
};
