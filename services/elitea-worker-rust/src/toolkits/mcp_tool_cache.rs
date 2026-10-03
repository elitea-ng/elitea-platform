//! TTL-bounded cache of remote MCP tool listings (#6691).
//!
//! A remote MCP toolkit's `enable_caching` / `cache_ttl` settings bound how
//! often the worker re-runs `tools/list`. Only the tool *descriptors* are
//! cached; connections never are. A run that hits the cache still opens its
//! own session with its own credentials and calls tools by name through it.
//!
//! The key is a SHA-256 over the toolkit identity, the frozen endpoint and the
//! exact request headers (credentials included), so a listing obtained with
//! one identity is never served to another.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use adk_rust::{AdkError, Tool, ToolContext};
use async_trait::async_trait;
use serde_json::{Map, Value};
use tokio::time::Instant;

/// Upper bound on distinct cached listings held by one worker process.
const MAX_CACHED_LISTINGS: usize = 512;

/// Calls one MCP tool by name on an established session, without listing.
#[async_trait]
pub(crate) trait McpToolCaller: Send + Sync {
    async fn call_tool(&self, name: &str, arguments: Map<String, Value>)
    -> adk_rust::Result<Value>;
}

/// What a listing contributed to a tool, independent of the session it came from.
#[derive(Clone, Debug, PartialEq)]
pub(crate) struct McpToolDescriptor {
    name: String,
    description: String,
    parameters_schema: Option<Value>,
    response_schema: Option<Value>,
    long_running: bool,
    read_only: bool,
    concurrency_safe: bool,
}

impl McpToolDescriptor {
    pub(crate) fn from_tool(tool: &dyn Tool) -> Self {
        Self {
            name: tool.name().to_owned(),
            description: tool.description().to_owned(),
            parameters_schema: tool.parameters_schema(),
            response_schema: tool.response_schema(),
            long_running: tool.is_long_running(),
            read_only: tool.is_read_only(),
            concurrency_safe: tool.is_concurrency_safe(),
        }
    }
}

/// A tool rebuilt from a cached descriptor and bound to the current session.
pub(crate) struct CachedMcpTool {
    descriptor: McpToolDescriptor,
    caller: Arc<dyn McpToolCaller>,
}

impl CachedMcpTool {
    pub(crate) fn new(descriptor: McpToolDescriptor, caller: Arc<dyn McpToolCaller>) -> Self {
        Self { descriptor, caller }
    }
}

#[async_trait]
impl Tool for CachedMcpTool {
    fn name(&self) -> &str {
        &self.descriptor.name
    }

    fn description(&self) -> &str {
        &self.descriptor.description
    }

    fn is_long_running(&self) -> bool {
        self.descriptor.long_running
    }

    fn parameters_schema(&self) -> Option<Value> {
        self.descriptor.parameters_schema.clone()
    }

    fn response_schema(&self) -> Option<Value> {
        self.descriptor.response_schema.clone()
    }

    fn is_read_only(&self) -> bool {
        self.descriptor.read_only
    }

    fn is_concurrency_safe(&self) -> bool {
        self.descriptor.concurrency_safe
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        let arguments = match arguments {
            Value::Null => Map::new(),
            Value::Object(map) => map,
            _ => return Err(AdkError::tool("Tool arguments must be an object")),
        };
        self.caller
            .call_tool(&self.descriptor.name, arguments)
            .await
    }
}

/// Opaque digest identifying who a listing is valid for.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub(crate) struct McpToolListKey([u8; 32]);

impl McpToolListKey {
    /// Length-prefix every field so no two field lists share a digest input.
    pub(crate) fn from_fields(fields: &[&[u8]]) -> Self {
        let mut context = ring::digest::Context::new(&ring::digest::SHA256);
        for field in fields {
            context.update(&(field.len() as u64).to_be_bytes());
            context.update(field);
        }
        let mut key = [0_u8; 32];
        key.copy_from_slice(context.finish().as_ref());
        Self(key)
    }
}

type CachedListing = (Instant, Arc<[McpToolDescriptor]>);

#[derive(Default)]
pub(crate) struct McpToolListCache {
    entries: Mutex<HashMap<McpToolListKey, CachedListing>>,
}

impl McpToolListCache {
    /// The listing for `key`, unless it expired at or before `now`.
    pub(crate) fn get(
        &self,
        key: &McpToolListKey,
        now: Instant,
    ) -> Option<Arc<[McpToolDescriptor]>> {
        let mut entries = self.entries.lock().ok()?;
        match entries.get(key) {
            Some((expires, descriptors)) if *expires > now => Some(Arc::clone(descriptors)),
            Some(_) => {
                entries.remove(key);
                None
            }
            None => None,
        }
    }

    pub(crate) fn insert(
        &self,
        key: McpToolListKey,
        descriptors: Vec<McpToolDescriptor>,
        expires: Instant,
    ) {
        let Ok(mut entries) = self.entries.lock() else {
            return;
        };
        if entries.len() >= MAX_CACHED_LISTINGS && !entries.contains_key(&key) {
            let now = Instant::now();
            entries.retain(|_, (expiry, _)| *expiry > now);
            if entries.len() >= MAX_CACHED_LISTINGS
                && let Some(oldest) = entries
                    .iter()
                    .min_by_key(|(_, (expiry, _))| *expiry)
                    .map(|(key, _)| *key)
            {
                entries.remove(&oldest);
            }
        }
        entries.insert(key, (expires, descriptors.into()));
    }

    #[cfg(test)]
    pub(crate) fn len(&self) -> usize {
        self.entries.lock().map_or(0, |entries| entries.len())
    }
}
