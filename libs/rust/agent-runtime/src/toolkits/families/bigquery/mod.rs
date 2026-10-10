//! Partial, capability-disabled Google `BigQuery` toolkit family.
//!
//! The SDK family (`tools/google/bigquery`) wraps `google.cloud.bigquery.Client`
//! with a service-account key. This family keeps the SDK's tool names and
//! argument schemas and speaks the `BigQuery` REST API directly: jobs.query and
//! jobs.getQueryResults for every SQL tool, jobs.get for `job_stats`, and
//! tables.insert for `create_delta_lake_table`. The service-account JWT grant
//! is the `gcp` family's (`service_account_token_request`), so one signing
//! implementation serves both.
//!
//! Three SDK tools are deliberately not served (see `tools.rs`):
//! `similarity_search` and `similarity_search_with_score` need a text
//! embedding model that the SDK never wires into this wrapper (both always
//! fail there), and `execute` reflects over arbitrary Python client methods,
//! which has no REST equivalent.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod format;
pub(in crate::toolkits) mod tools;
