//! The SDK `bitbucket` toolkit: one repository on Bitbucket Cloud or
//! Bitbucket Server / Data Center, with a protected base branch and an
//! invocation-local active branch.
//!
//! It serves every SDK tool except the index tools (`index_data`,
//! `search_index`, `stepback_search_index`, `stepback_summary_index`,
//! `list_indexes`, `remove_index`), which need the indexing runtime Rust does
//! not have yet. The OLD/NEW editor is the GitLab Org family's.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;
