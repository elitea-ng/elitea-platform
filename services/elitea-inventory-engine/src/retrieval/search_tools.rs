//! The `inventory_search` family's tools, except `investigate`.

use super::{Call, Handled};

/// See [`super::dispatch`].
#[must_use]
pub fn handle(_call: &Call<'_>) -> Handled {
    None
}
