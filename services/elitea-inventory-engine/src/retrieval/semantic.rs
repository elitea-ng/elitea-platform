//! `semantic_search` over the entity embeddings.

use super::{Call, Handled};

/// See [`super::dispatch`].
#[must_use]
pub fn handle(_call: &Call<'_>) -> Handled {
    None
}
