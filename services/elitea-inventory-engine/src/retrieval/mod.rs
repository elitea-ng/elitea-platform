//! Reading the graph (ADR-0027 P4): the retrieval tools of the
//! `inventory` and `inventory_search` families, over a [`view::GraphView`]
//! loaded from the store and cached until the graph's revision changes.

pub mod admin_tools;
pub mod community_tools;
pub mod inventory_tools;
pub mod pattern;
pub mod query;
pub mod search_tools;
pub mod semantic;
pub mod view;

use crate::store::{self, GraphKey, StoreError};
use elitea_engine_core::errors::EngineError;
use serde_json::{Map, Value};
use sqlx::postgres::PgPool;
use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use view::GraphView;

/// The views this process loaded, one per graph, kept while current.
#[derive(Debug, Default)]
pub struct ViewCache {
    views: Mutex<HashMap<GraphKey, Arc<GraphView>>>,
}

impl ViewCache {
    /// The current view of `key`, or `None` when the graph does not exist.
    /// A cached view is reused while the stored revision is unchanged.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub async fn view(
        &self,
        pool: &PgPool,
        key: GraphKey,
    ) -> Result<Option<Arc<GraphView>>, StoreError> {
        let Some(revision) = store::revision(pool, key).await? else {
            self.forget(key);
            return Ok(None);
        };
        if let Some(view) = self.cached(key).filter(|view| view.revision == revision) {
            return Ok(Some(view));
        }
        let Some((graph, revision)) = store::load(pool, key).await? else {
            self.forget(key);
            return Ok(None);
        };
        let view = Arc::new(GraphView::new(graph, revision));
        if let Ok(mut views) = self.views.lock() {
            views.insert(key, Arc::clone(&view));
        }
        Ok(Some(view))
    }

    fn cached(&self, key: GraphKey) -> Option<Arc<GraphView>> {
        self.views.lock().ok()?.get(&key).cloned()
    }

    fn forget(&self, key: GraphKey) {
        if let Ok(mut views) = self.views.lock() {
            views.remove(&key);
        }
    }
}

/// What every tool handler receives.
#[derive(Debug, Clone, Copy)]
pub struct Call<'a> {
    /// The tool name (as routed: `search_graph`, `get_entity_details`, …).
    pub tool: &'a str,
    /// `inventory` or `inventory_search`.
    pub family: &'a str,
    /// The merged parameters the host forwarded.
    pub params: &'a Map<String, Value>,
    /// The graph, loaded and indexed.
    pub view: &'a GraphView,
}

/// A handler's answer: `Some` when the module serves `call.tool` — the
/// tool result the host composes (`{"success": true, "result": …}`, as
/// the Python sidecar wrapped a handler's return value) or the failure —
/// and `None` when the tool is another module's.
pub type Handled = Option<Result<Value, EngineError>>;

/// The tool result of a handler's text (or JSON text) answer.
#[must_use]
pub fn answer(result: impl Into<String>) -> Value {
    serde_json::json!({"success": true, "result": result.into()})
}

/// Route `call` to the module that serves it.
#[must_use]
pub fn dispatch(call: &Call<'_>) -> Handled {
    inventory_tools::handle(call)
        .or_else(|| search_tools::handle(call))
        .or_else(|| pattern::handle(call))
        .or_else(|| semantic::handle(call))
        .or_else(|| community_tools::handle(call))
        .or_else(|| admin_tools::handle(call))
}
