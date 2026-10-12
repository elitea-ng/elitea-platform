//! Zephyr Scale (Cloud v2) toolkit family.
//!
//! The twenty business operations of the SDK's `ZephyrScaleToolkit`, which
//! drives `zephyr-python-api==0.1.0`'s Cloud wrapper. Like the SDK, every
//! request goes to the fixed Cloud API (`ZephyrScale(token=...)` ignores the
//! configured `base_url`) and authenticates with the bearer token only.
//! The six indexing tools are not served: indexing does not exist in this
//! runtime.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod folders;
pub(in crate::toolkits) mod render;
pub(in crate::toolkits) mod tools;
