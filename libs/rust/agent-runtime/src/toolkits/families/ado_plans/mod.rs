//! The `ado_plans` (Azure DevOps Test Plans) toolkit family.
//!
//! All twelve non-index SDK tools: test plans, suites and test cases over the
//! `TestPlanClient` v7.0 routes, with test case work items created and read
//! through the shared work item calls in `families::ado`.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod steps;
pub(in crate::toolkits) mod tools;
