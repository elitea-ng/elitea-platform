//! TTL-bounded cache of remote MCP tool listings (#6691).
//!
//! A remote MCP toolkit's `enable_caching` / `cache_ttl` settings bound how
//! often the worker runs `tools/list` while it assembles an agent. Only the
//! tool *descriptors* are cached; connections never are. A run that hits the
//! cache still opens its own session with its own credentials.
//!
//! A cached tool never executes through a second code path. The first call
//! of any cached tool lists the tools once on the run's own session and then
//! delegates to the ADK tool of the same name, so execution is the ADK
//! client's in every respect: argument shape, error text, task and input
//! rounds, and the per-session `x-mcp-header` parameter promotion that rmcp
//! learns only from a `tools/list` on that session. The cache therefore saves
//! the listing for every server whose tools a run never calls.
//!
//! The key is a SHA-256 over the toolkit identity, the frozen endpoint and the
//! exact request headers (credentials included), so a listing obtained with
//! one identity is never served to another. The cache is bounded by entry
//! count, by the size of one listing and by the total size of all listings.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_core::{AdkError, ReadonlyContext, Tool, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use serde_json::Value;
use tokio::sync::OnceCell;
use tokio::time::Instant;

/// Upper bound on distinct cached listings held by one worker process.
pub const MAX_CACHED_LISTINGS: usize = 512;
/// A listing larger than this is never cached: it is listed on every run.
pub const MAX_CACHED_LISTING_BYTES: usize = 1_024 * 1_024;
/// Upper bound on the size of every cached listing together.
pub const MAX_CACHED_TOTAL_BYTES: usize = 64 * 1_024 * 1_024;

/// What a listing contributed to a tool, independent of the session it came from.
#[derive(Clone, Debug, PartialEq)]
pub struct McpToolDescriptor {
    name: String,
    description: String,
    parameters_schema: Option<Value>,
    response_schema: Option<Value>,
    long_running: bool,
    read_only: bool,
    concurrency_safe: bool,
}

impl McpToolDescriptor {
    pub fn from_tool(tool: &dyn Tool) -> Self {
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

    /// The bytes this descriptor pins while cached, measured as serialized.
    fn retained_bytes(&self) -> usize {
        let schema_bytes = |schema: &Option<Value>| {
            schema.as_ref().map_or(0, |value| {
                serde_json::to_vec(value).map_or(usize::MAX, |bytes| bytes.len())
            })
        };
        self.name
            .len()
            .saturating_add(self.description.len())
            .saturating_add(schema_bytes(&self.parameters_schema))
            .saturating_add(schema_bytes(&self.response_schema))
    }
}

/// The tools of one live session, listed at most once and only on demand.
pub struct LiveMcpTools {
    toolset: Arc<dyn Toolset>,
    timeout: Duration,
    tools: OnceCell<HashMap<String, Arc<dyn Tool>>>,
}

impl LiveMcpTools {
    pub fn new(toolset: Arc<dyn Toolset>, timeout: Duration) -> Self {
        Self {
            toolset,
            timeout,
            tools: OnceCell::new(),
        }
    }

    async fn tool(&self, name: &str) -> adk_core::Result<Arc<dyn Tool>> {
        let tools = self
            .tools
            .get_or_try_init(|| async {
                let context: Arc<dyn ReadonlyContext> =
                    Arc::new(SimpleToolContext::new("elitea_mcp_cached_call"));
                let listed = tokio::time::timeout(self.timeout, self.toolset.tools(context))
                    .await
                    .map_err(|_| AdkError::tool("MCP server did not list its tools in time"))??;
                Ok::<_, AdkError>(
                    listed
                        .into_iter()
                        .map(|tool| (tool.name().to_owned(), tool))
                        .collect::<HashMap<_, _>>(),
                )
            })
            .await?;
        tools.get(name).cloned().ok_or_else(|| {
            AdkError::tool(format!(
                "MCP tool '{name}' is no longer offered by the server"
            ))
        })
    }
}

/// A tool rebuilt from a cached descriptor and bound to the current session.
pub struct CachedMcpTool {
    descriptor: McpToolDescriptor,
    live: Arc<LiveMcpTools>,
}

impl CachedMcpTool {
    pub fn new(descriptor: McpToolDescriptor, live: Arc<LiveMcpTools>) -> Self {
        Self { descriptor, live }
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
        context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        self.live
            .tool(&self.descriptor.name)
            .await?
            .execute(context, arguments)
            .await
    }
}

/// Opaque digest identifying who a listing is valid for.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct McpToolListKey([u8; 32]);

impl McpToolListKey {
    /// Length-prefix every field so no two field lists share a digest input.
    pub fn from_fields(fields: &[&[u8]]) -> Self {
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

struct CachedListing {
    expires: Instant,
    bytes: usize,
    descriptors: Arc<[McpToolDescriptor]>,
}

#[derive(Default)]
struct CacheEntries {
    listings: HashMap<McpToolListKey, CachedListing>,
    bytes: usize,
}

impl CacheEntries {
    fn remove(&mut self, key: &McpToolListKey) {
        if let Some(listing) = self.listings.remove(key) {
            self.bytes = self.bytes.saturating_sub(listing.bytes);
        }
    }

    fn evict_earliest_expiring(&mut self) -> bool {
        let Some(earliest) = self
            .listings
            .iter()
            .min_by_key(|(_, listing)| listing.expires)
            .map(|(key, _)| *key)
        else {
            return false;
        };
        self.remove(&earliest);
        true
    }
}

#[derive(Default)]
pub struct McpToolListCache {
    entries: Mutex<CacheEntries>,
}

impl McpToolListCache {
    /// The listing for `key`, unless it expired at or before `now`.
    pub fn get(&self, key: &McpToolListKey, now: Instant) -> Option<Arc<[McpToolDescriptor]>> {
        let mut entries = self.entries.lock().ok()?;
        match entries.listings.get(key) {
            Some(listing) if listing.expires > now => Some(Arc::clone(&listing.descriptors)),
            Some(_) => {
                entries.remove(key);
                None
            }
            None => None,
        }
    }

    /// Cache a listing until `expires`. A listing over
    /// [`MAX_CACHED_LISTING_BYTES`] is not cached (and returns `false`); room
    /// for one that fits is made by dropping expired listings, then the
    /// earliest-expiring ones, until both the count and byte bounds hold.
    pub fn insert(
        &self,
        key: McpToolListKey,
        descriptors: Vec<McpToolDescriptor>,
        expires: Instant,
    ) -> bool {
        let bytes = descriptors.iter().fold(0_usize, |total, descriptor| {
            total.saturating_add(descriptor.retained_bytes())
        });
        if bytes > MAX_CACHED_LISTING_BYTES {
            return false;
        }
        let Ok(mut entries) = self.entries.lock() else {
            return false;
        };
        entries.remove(&key);
        let now = Instant::now();
        let stale = entries
            .listings
            .iter()
            .filter(|(_, listing)| listing.expires <= now)
            .map(|(stale_key, _)| *stale_key)
            .collect::<Vec<_>>();
        for stale_key in stale {
            entries.remove(&stale_key);
        }
        while (entries.listings.len() >= MAX_CACHED_LISTINGS
            || entries.bytes.saturating_add(bytes) > MAX_CACHED_TOTAL_BYTES)
            && entries.evict_earliest_expiring()
        {}
        entries.bytes = entries.bytes.saturating_add(bytes);
        entries.listings.insert(
            key,
            CachedListing {
                expires,
                bytes,
                descriptors: descriptors.into(),
            },
        );
        true
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn len(&self) -> usize {
        self.entries
            .lock()
            .map_or(0, |entries| entries.listings.len())
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn retained_bytes(&self) -> usize {
        self.entries.lock().map_or(0, |entries| entries.bytes)
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn contains(&self, key: &McpToolListKey) -> bool {
        self.entries
            .lock()
            .is_ok_and(|entries| entries.listings.contains_key(key))
    }
}

#[cfg(any(test, feature = "test-support"))]
impl McpToolDescriptor {
    pub fn fixture(name: &str, description: &str) -> Self {
        Self {
            name: name.to_owned(),
            description: description.to_owned(),
            parameters_schema: None,
            response_schema: None,
            long_running: false,
            read_only: false,
            concurrency_safe: false,
        }
    }
}
