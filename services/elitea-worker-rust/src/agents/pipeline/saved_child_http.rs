//! Main-owned saved-family reference for the Code authority port.
//! This module contains no family registration or HTTP dispatcher.
use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct SavedChildScopeRef {
    pub(crate) scope_id: String,
    pub(crate) revision: u64,
    pub(crate) digest_sha256: String,
}
impl SavedChildScopeRef {
    pub(crate) fn validate(&self) -> bool {
        valid_digest(&self.scope_id) && self.revision == 1 && valid_digest(&self.digest_sha256)
    }
}

/// What a Code visit names as its owner below the run root. Main resolves
/// either against its own frozen state inside the visit's transaction; the
/// value itself grants nothing.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
#[serde(untagged)]
pub(crate) enum CodeVisitScope {
    /// A Main-registered saved family.
    Registered(SavedChildScopeRef),
    /// A Code node run directly as a Map item or Parallel branch of the run
    /// root (W3-2).
    FanoutChild(FanoutCodeChildScope),
    /// A Code node inside a saved pipeline a root-level Agent node runs
    /// (W3-2b).
    SavedChild(super::saved_code_child::SavedCodeChildScope),
}

/// The wire schema of [`FanoutCodeChildScope`].
pub(crate) const FANOUT_CODE_CHILD_SCOPE_SCHEMA: &str = "elitea.runtime.fanout-code-child-scope.v1";

/// A direct Code child of a root fan-out node (W3-2). Main checks, in the
/// visit's own transaction against the run's frozen root source, that
/// `node_id` is that family's Map or Parallel, that it owns `owned_node_id`
/// at `ordinal`, that the owned node is the visit's Code node, that
/// `parent_thread_id` is the run's root thread and that the ordinal lies
/// inside the frozen family.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub(crate) struct FanoutCodeChildScope {
    pub(crate) schema: &'static str,
    /// `map_item` or `parallel_branch`.
    pub(crate) kind: &'static str,
    pub(crate) node_id: String,
    pub(crate) owned_node_id: String,
    pub(crate) ordinal: u32,
    pub(crate) item_count: u32,
    pub(crate) parent_thread_id: String,
    pub(crate) child_thread_id: String,
}

fn valid_digest(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}
