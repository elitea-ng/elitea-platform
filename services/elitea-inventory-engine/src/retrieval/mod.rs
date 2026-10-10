//! Reading the graph (ADR-0027 P4): the retrieval tools of the
//! `inventory` and `inventory_search` families, over a [`view::GraphView`]
//! loaded from the store and cached until the graph's revision changes.
//!
//! The tools and the cache live in the shared core (ADR-0029 decision 7)
//! and are re-exported here, so every path in this crate stays the same;
//! the native runner caches over [`crate::store::PgGraphStore`].

pub use elitea_inventory_core::retrieval::{
    Call, Handled, ViewCache, admin_tools, answer, community_tools, dispatch, flag,
    inventory_tools, lenient_flag, pattern, query, search_tools, semantic, view,
};
