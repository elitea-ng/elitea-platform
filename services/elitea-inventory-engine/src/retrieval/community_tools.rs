//! The community tools (`retrieval.py`): list, detail, find, search within.

use super::{Call, Handled};

/// See [`super::dispatch`].
#[must_use]
pub fn handle(_call: &Call<'_>) -> Handled {
    None
}
