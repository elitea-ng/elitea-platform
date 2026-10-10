//! The Azure DevOps authority, transport and result shapes shared by the
//! `ado_boards`, `ado_plans`, `ado_repos` and `ado_wiki` families.
//!
//! The SDK builds one `azure.devops` connection per toolkit from
//! `ado_configuration` (`organization_url`, `token`) and the toolkit's
//! `project`, and authenticates every call with the PAT as Basic credentials.
//! Here that connection is one claim-scoped [`client::AdoClient`] per
//! toolset, over a single bounded, HTTPS-only, redirect-free transport.
//! Work item operations live here because `ado_plans` creates and reads its
//! test cases through the same work item API `ado_boards` serves.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod format;
pub(in crate::toolkits) mod toolset;
pub(in crate::toolkits) mod work_items;
