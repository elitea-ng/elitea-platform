//! The `ado_boards` (Azure DevOps work items) toolkit family.
//!
//! Eleven of the SDK's thirteen non-index tools: WIQL search, work item
//! reads/effects, links, relation types, comments, wiki-page artifact links
//! and work item type field discovery. `get_image_by_url` needs a vision
//! LLM and `attach_file_to_work_item` needs artifact storage; a toolkit holds
//! neither here, so both stay on the Python worker with the index tools.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;
