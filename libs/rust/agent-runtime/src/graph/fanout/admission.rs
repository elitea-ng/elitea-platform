//! Worker-wide admission for fan-out children.
//!
//! - [`ChildPermits`]: at most [`WORKER_CHILD_PERMITS`] children run at once in
//!   one Worker process, across every activation. A permit is held only while
//!   the child runs: a paused child releases it, so parked work never starves
//!   live work.
//! - [`ModelStreamLimiter`]: at most [`MODEL_STREAMS_PER_WORKER`] concurrent
//!   model streams per model, bounded to [`MAX_TRACKED_MODELS`] models. An
//!   unknown model past the bound shares one overflow lane rather than growing
//!   the table.
//!
//! Waiting is cancellation-safe: dropping a pending acquire leaves nothing
//! held.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use tokio::sync::{OwnedSemaphorePermit, Semaphore};

use crate::graph::fanout_budget::{
    MAX_TRACKED_MODELS, MODEL_STREAMS_PER_WORKER, WORKER_CHILD_PERMITS,
};

/// The process-wide child slots.
#[derive(Clone)]
pub struct ChildPermits {
    slots: Arc<Semaphore>,
}

/// One running child's slot. Dropping it (child settled or paused) frees it.
#[derive(Debug)]
pub struct ChildPermit {
    _permit: OwnedSemaphorePermit,
}

impl Default for ChildPermits {
    fn default() -> Self {
        Self::with_capacity(WORKER_CHILD_PERMITS)
    }
}

impl ChildPermits {
    /// A pool of `capacity` slots (`WORKER_CHILD_PERMITS` in production).
    #[must_use]
    pub fn with_capacity(capacity: usize) -> Self {
        Self {
            slots: Arc::new(Semaphore::new(capacity)),
        }
    }

    /// Wait for a slot. The pool is never closed, so this only waits.
    pub async fn acquire(&self) -> ChildPermit {
        let permit = Arc::clone(&self.slots)
            .acquire_owned()
            .await
            .unwrap_or_else(|_| unreachable!("the child permit pool is never closed"));
        ChildPermit { _permit: permit }
    }

    /// Slots free right now.
    #[must_use]
    pub fn available(&self) -> usize {
        self.slots.available_permits()
    }
}

/// Per-(Worker, model) stream lanes.
#[derive(Clone, Default)]
pub struct ModelStreamLimiter {
    lanes: Arc<Mutex<Lanes>>,
}

#[derive(Default)]
struct Lanes {
    by_model: HashMap<String, Arc<Semaphore>>,
    overflow: Option<Arc<Semaphore>>,
}

/// One model stream's lane slot.
#[derive(Debug)]
pub struct ModelStreamPermit {
    _permit: OwnedSemaphorePermit,
}

impl ModelStreamLimiter {
    fn lane(&self, model: &str) -> Arc<Semaphore> {
        let mut lanes = self
            .lanes
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if let Some(lane) = lanes.by_model.get(model) {
            return Arc::clone(lane);
        }
        if lanes.by_model.len() < MAX_TRACKED_MODELS {
            let lane = Arc::new(Semaphore::new(MODEL_STREAMS_PER_WORKER));
            lanes.by_model.insert(model.to_owned(), Arc::clone(&lane));
            return lane;
        }
        Arc::clone(
            lanes
                .overflow
                .get_or_insert_with(|| Arc::new(Semaphore::new(MODEL_STREAMS_PER_WORKER))),
        )
    }

    /// Wait for a stream slot on `model`'s lane.
    pub async fn acquire(&self, model: &str) -> ModelStreamPermit {
        let permit = self
            .lane(model)
            .acquire_owned()
            .await
            .unwrap_or_else(|_| unreachable!("model lanes are never closed"));
        ModelStreamPermit { _permit: permit }
    }

    /// Models with their own lane.
    #[must_use]
    pub fn tracked_models(&self) -> usize {
        self.lanes
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .by_model
            .len()
    }
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use super::*;

    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn child_permits_limit_and_limit_plus_one_and_release_on_drop() {
        let permits = ChildPermits::default();
        let mut held = Vec::new();
        for _ in 0..WORKER_CHILD_PERMITS {
            held.push(permits.acquire().await);
        }
        assert_eq!(permits.available(), 0);
        let waiting = permits.clone();
        let next = tokio::spawn(async move { waiting.acquire().await });
        tokio::task::yield_now().await;
        assert!(!next.is_finished(), "the 65th child ran without a permit");
        // A child that pauses drops its permit; the waiting child starts.
        drop(held.pop());
        let permit = tokio::time::timeout(Duration::from_secs(5), next)
            .await
            .expect("released permit admitted the next child")
            .expect("join");
        drop(permit);
        assert_eq!(permits.available(), 1);
    }

    #[tokio::test]
    async fn a_cancelled_acquire_holds_nothing() {
        let permits = ChildPermits::with_capacity(1);
        let held = permits.acquire().await;
        let pending = tokio::time::timeout(Duration::from_millis(10), permits.acquire()).await;
        assert!(pending.is_err());
        drop(held);
        assert_eq!(permits.available(), 1);
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn model_lanes_limit_per_model_not_across_models() {
        let limiter = ModelStreamLimiter::default();
        let mut held = Vec::new();
        for _ in 0..MODEL_STREAMS_PER_WORKER {
            held.push(limiter.acquire("model-a").await);
        }
        // Another model is unaffected by model-a's full lane.
        let other = tokio::time::timeout(Duration::from_secs(5), limiter.acquire("model-b"))
            .await
            .expect("model-b has its own lane");
        drop(other);
        let blocked =
            tokio::time::timeout(Duration::from_millis(20), limiter.acquire("model-a")).await;
        assert!(blocked.is_err(), "the 17th stream on one model ran");
        drop(held.pop());
        tokio::time::timeout(Duration::from_secs(5), limiter.acquire("model-a"))
            .await
            .expect("released stream admitted");
    }

    #[tokio::test]
    async fn the_model_table_is_bounded() {
        let limiter = ModelStreamLimiter::default();
        for index in 0..=MAX_TRACKED_MODELS + 3 {
            drop(limiter.acquire(&format!("model-{index}")).await);
        }
        assert_eq!(limiter.tracked_models(), MAX_TRACKED_MODELS);
    }
}
