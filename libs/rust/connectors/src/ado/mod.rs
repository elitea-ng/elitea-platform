//! Azure DevOps: the connection, the bounded wire layer and the REST call
//! builder the `ado_boards`, `ado_plans`, `ado_repos` and `ado_wiki`
//! families share (moved from agent-runtime, ADR-0030 decision 3).
//!
//! The SDK builds one `azure.devops` connection per toolkit from
//! `ado_configuration` (`organization_url`, `token`) and the toolkit's
//! `project`, and authenticates every call with the PAT as Basic
//! credentials. Here that connection is one [`client::AdoClient`] over a
//! single bounded, HTTPS-only, redirect-free transport.

pub mod client;
pub mod config;
pub mod repos;
