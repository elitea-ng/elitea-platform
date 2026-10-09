//! Google Places configuration, bounded provider client, and native ADK tools.
//!
//! The family consumes Main's already-authorized, claim-materialized settings.
//! It preserves the current SDK's two public read operations while replacing
//! frozen legacy Places endpoints, process-global credentials and duplicate
//! detail calls with supported Places API (New), one pooled async client and
//! fixed request/result budgets. Main currently advertises no Google Places
//! connection check, so this family does not invent a probe operation.

#![allow(dead_code)] // Production tool assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;
