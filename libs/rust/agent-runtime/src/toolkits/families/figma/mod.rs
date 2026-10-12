//! Partial, capability-disabled Figma toolkit family.
//!
//! The SDK's eight Figma REST reads/comment (with its `extra_params` output
//! reduction) and both design-token extractors, over the fixed
//! `https://api.figma.com/v1/` origin. `analyze_file` (LLM frame analysis
//! over rendered images and the TOON serializer) and the six indexing tools
//! stay SDK-only; the capability snapshot lists the served subset.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod output;
pub(in crate::toolkits) mod tokens;
pub(in crate::toolkits) mod tools;
