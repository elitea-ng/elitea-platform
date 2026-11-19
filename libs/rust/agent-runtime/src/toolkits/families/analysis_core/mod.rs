//! The shared core of the analysis toolkits (`analyse_jira` today;
//! `analyse_ado`, `analyse_gitlab` and `analyse_github` are built on it in
//! #2264): a text table, CSV in and out, time-in-status histories and the
//! status mapping, flow metrics over the dataset, and the writer that stores
//! a dataset in the project's artifact bucket.
//!
//! The datasets are CSV files, which `data_analysis` reads. Nothing here
//! knows a tracker: a family turns its items and status transitions into the
//! dataset columns (`metrics.rs` names the ones the metrics read) and reuses
//! the rest.

pub(crate) mod csv;
pub(crate) mod history;
pub(crate) mod metrics;
pub(crate) mod table;
pub(crate) mod writer;
