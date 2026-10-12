//! The `gitlab` settings live in `elitea-connectors` (ADR-0030 decision 3);
//! the family reaches them here.

pub(crate) use elitea_connectors::gitlab::config::{
    GitLabConfigError, GitLabConfigErrorCode, GitLabToolkitConfig,
};
