//! Partial, capability-disabled Carrier toolkit family.
//!
//! Fifteen of the SDK's eighteen Carrier operations (backend and UI
//! performance tests, reports and tickets) over one invocation-scoped,
//! bounded HTTP authority. The three archive tools (`get_report_by_id`,
//! `create_excel_report`, `create_ui_excel_report`) download, unzip, merge
//! and re-upload JMeter/Gatling result archives and build `.xlsx` workbooks;
//! they need archive and spreadsheet writers this crate does not link, so
//! they stay SDK-only and the capability snapshot lists the served subset.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;
