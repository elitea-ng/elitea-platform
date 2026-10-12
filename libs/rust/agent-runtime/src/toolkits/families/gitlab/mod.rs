//! The SDK `gitlab` toolkit: one configured GitLab project and base branch.
//!
//! It serves every SDK tool except the index tools (`index_data`,
//! `search_index`, `stepback_search_index`, `stepback_summary_index`,
//! `list_indexes`, `remove_index`), which need the indexing runtime Rust does
//! not have yet. The HTTP layer, the OLD/NEW editor and the merge-request diff
//! positions are the GitLab Org family's.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;
