//! Presets, cache and maintenance tools, and `remove_source_entities`.

use super::{Call, Handled};

/// See [`super::dispatch`].
#[must_use]
pub fn handle(_call: &Call<'_>) -> Handled {
    None
}
