//! Semaphore-based concurrency control for parallel tool execution.
//!
//! This module re-exports [`ToolConcurrencyManager`] and related types from
//! `adk-core`. The implementation lives in `adk-core` so that both `adk-agent`
//! (which performs parallel tool dispatch) and `adk-runner` can use it without
//! circular dependencies.
//!
//! # Architecture
//!
//! The manager holds an optional global semaphore and a map of per-tool semaphores.
//! When a tool call is requested via [`ToolConcurrencyManager::acquire`], the manager
//! checks for a per-tool semaphore first; if none exists, it falls back to the global
//! semaphore. The returned [`ConcurrencyPermit`] is an RAII guard that releases the
//! semaphore on drop.
//!
//! # Example
//!
//! ```rust
//! use adk_runner::tool_concurrency::{
//!     BackpressurePolicy, ToolConcurrencyConfig, ToolConcurrencyManager,
//! };
//! use std::collections::HashMap;
//!
//! # tokio_test::block_on(async {
//! let config = ToolConcurrencyConfig {
//!     max_concurrency: Some(5),
//!     per_tool: HashMap::from([("web_scraper".to_string(), 2)]),
//!     backpressure: BackpressurePolicy::Queue,
//! };
//!
//! let manager = ToolConcurrencyManager::new(&config);
//!
//! // Acquire a permit — blocks if limit reached (Queue policy)
//! let permit = manager.acquire("web_scraper").await.unwrap();
//! // ... execute tool ...
//! drop(permit); // releases the semaphore
//! # });
//! ```

// Re-export everything from adk-core for backward compatibility.
pub use adk_core::{
    BackpressurePolicy, ConcurrencyPermit, ToolConcurrencyConfig, ToolConcurrencyManager,
};
