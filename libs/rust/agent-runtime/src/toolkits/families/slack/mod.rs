//! Capability-disabled Slack toolkit family.
//!
//! The family keeps all seven current SDK operations behind one
//! invocation-scoped token and a fixed Slack Web API origin. Production
//! assembly remains gated on durable sensitive-tool and effect ownership.

#![allow(dead_code)]

pub(crate) mod client;
pub(crate) mod config;
pub(crate) mod tools;
