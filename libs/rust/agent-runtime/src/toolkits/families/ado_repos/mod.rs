//! The `ado_repos` (Azure DevOps Git) toolkit family.
//!
//! All fifteen non-index SDK tools over the `GitClient` v7.0 routes:
//! branches, files (read with the SDK's line slicing and read guard,
//! create/update/delete as pushes), pull requests, their diffs, threads and
//! work items, and commits. The toolset owns the SDK wrapper's mutable
//! active branch. File edits reuse the OLD/NEW marker engine of the GitLab
//! Org family, and pull request diffs reproduce Python's `difflib`.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod diff;
pub(in crate::toolkits) mod tools;
