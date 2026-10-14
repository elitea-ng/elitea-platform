//! Carrier toolkit family: the SDK's eighteen Carrier operations (backend and
//! UI performance tests, reports and tickets) over one invocation-scoped,
//! bounded HTTP authority.
//!
//! The three archive tools (`get_report_by_id`, `create_excel_report`,
//! `create_ui_excel_report`) download result archives, stream their logs
//! (`perf_report`, `lighthouse`) and build `.xlsx` workbooks on
//! `elitea-office-core` (`excel`). The two Excel tools store the workbook in
//! the project's artifact store and are served only with that store lent to
//! the run and the `toolkit-office` feature; see `excel.rs` for what the
//! workbooks do not carry.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
#[cfg(feature = "toolkit-office")]
pub(in crate::toolkits) mod excel;
pub(in crate::toolkits) mod lighthouse;
pub(in crate::toolkits) mod perf_report;
pub(in crate::toolkits) mod reports;
pub(in crate::toolkits) mod tools;
