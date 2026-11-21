//! The `analyse_jira` toolkit family: bulk extraction of Jira issues with
//! their status history into CSV datasets in an artifact bucket, and flow
//! metrics (lead time, cycle time, life time, throughput, flow efficiency,
//! flow load) calculated over them.
//!
//! It reuses the `jira` family's client and credential (`jira_configuration`):
//! no new credential type. The analysis half is `analysis_core`, which the
//! ADO, GitLab and GitHub analysis toolkits (#2264) share.
//!
//! The upstream wrapper is not public (`catalogue.rs` says what survives and
//! what was reconstructed), so the tools and their behaviour are specified in
//! the toolkit's page of the docs: which tools are upstream's by name, which
//! are this platform's, and every behaviour of the `elitea-analyse` library
//! that was deliberately not reproduced.

pub(in crate::toolkits) mod catalogue;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod dataset;
pub(in crate::toolkits) mod extract;
pub(in crate::toolkits) mod overview;
pub(in crate::toolkits) mod records;
pub(in crate::toolkits) mod tools;

#[cfg(test)]
mod http_tests;
#[cfg(test)]
mod tests;
