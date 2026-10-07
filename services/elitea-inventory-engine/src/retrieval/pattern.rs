//! `query_pattern`: the Cypher-like pattern queries (`KnowledgeGraph`).

use super::{Call, Handled};

/// See [`super::dispatch`].
#[must_use]
pub fn handle(_call: &Call<'_>) -> Handled {
    None
}
