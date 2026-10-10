//! The `ado_wiki` (Azure DevOps Wiki) toolkit family.
//!
//! All eight non-index SDK tools: wiki and page reads, page deletes, page
//! create/update (creating the wiki when missing) and page moves over the
//! `WikiClient` v7.0 routes. Image description inside page content needs the
//! toolkit LLM, which this runtime does not lend to toolkits; page content is
//! returned with its image references unchanged.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;
