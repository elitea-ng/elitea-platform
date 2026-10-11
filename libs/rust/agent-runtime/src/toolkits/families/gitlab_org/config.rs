//! The `gitlab_org` settings live in `elitea-connectors` (ADR-0030 decision
//! 3); the family reaches them here.

pub(crate) use elitea_connectors::gitlab::org_config::{
    GitLabOrgConfigError, GitLabOrgConfigErrorCode, GitLabOrgToolkitConfig,
};
