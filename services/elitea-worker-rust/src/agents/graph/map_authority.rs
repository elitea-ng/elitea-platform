//! One original parent owns Map append and descendant activation.

use super::map_reduce::MapChildCheckpointerFactory;
use super::parallel::ParallelCheckpointAppender;

pub(crate) mod sealed {
    pub(crate) trait Sealed {}
}

/// Implement only for admitted storage or receipt holders. Never add a blanket implementation.
pub(crate) trait MapCheckpointAuthority:
    sealed::Sealed + ParallelCheckpointAppender + MapChildCheckpointerFactory
{
}
