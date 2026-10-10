//! Reading the graph (ADR-0027 P4): the retrieval tools of the
//! `inventory` and `inventory_search` families, over a [`view::GraphView`]
//! loaded from the store and cached until the graph's revision changes.
//!
//! The tools themselves live in the shared core (ADR-0029 decision 7) and
//! are re-exported here, so every path in this crate stays the same; the
//! cache over the PostgreSQL store is this crate's.

pub use elitea_inventory_core::retrieval::{
    Call, Handled, admin_tools, answer, community_tools, dispatch, flag, inventory_tools,
    lenient_flag, pattern, query, search_tools, semantic, view,
};

use crate::store::{self, GraphKey, StoreError};
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
        let mut view = GraphView::new(graph, revision);
        for (source, document, acl) in
            crate::store::sources::restricted_documents(pool, key).await?
        {
            view.restricted.insert((source, document), acl);
        }
        let view = Arc::new(view);
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
