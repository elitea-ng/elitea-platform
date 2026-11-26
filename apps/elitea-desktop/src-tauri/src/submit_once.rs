//! A client submit id makes a start idempotent (#3077).
//!
//! The composer gives every message a `submit_id` (a UUID, kept while the
//! message has not gone, renewed once it has). Two `agent_turn_start` calls
//! with the same id in the same workspace (Enter pressed three times, a
//! double IPC call, a retry after a lost answer) run ONE start: a call that
//! arrives while the first is under way waits for it and gets its answer;
//! one that arrives after it succeeded gets the same answer at once. A
//! refused or failed start is forgotten, so the message's retry (with the
//! same id) tries again. Answers are kept a while and at most
//! [`KEPT`] of them; the composer renews its id on every sent message, so a
//! kept id is never one a later message reuses.

use std::collections::HashMap;
use std::future::Future;
use std::sync::{Arc, Mutex, PoisonError};
use std::time::{Duration, Instant};

use tokio::sync::OnceCell;

/// How many answered ids are kept.
pub const KEPT: usize = 256;
/// How long an answered id is kept.
pub const KEPT_FOR: Duration = Duration::from_secs(15 * 60);

type Cell<T, E> = Arc<OnceCell<Result<T, E>>>;

struct Slot<T, E> {
    cell: Cell<T, E>,
    at: Instant,
}

/// The starts under way or answered, by `(workspace, submit id)`.
pub struct SubmitOnce<T, E> {
    slots: Mutex<HashMap<(String, String), Slot<T, E>>>,
}

impl<T, E> Default for SubmitOnce<T, E> {
    fn default() -> Self {
        Self {
            slots: Mutex::new(HashMap::new()),
        }
    }
}

impl<T: Clone, E: Clone> SubmitOnce<T, E> {
    /// Runs `start` once per `(scope, id)`; `id` `None` (an older client, a
    /// host-made turn) always runs it.
    pub async fn run<F, Fut>(&self, scope: &str, id: Option<&str>, start: F) -> Result<T, E>
    where
        F: FnOnce() -> Fut,
        Fut: Future<Output = Result<T, E>>,
    {
        let Some(id) = id.filter(|id| !id.is_empty()) else {
            return start().await;
        };
        let key = (scope.to_owned(), id.to_owned());
        let cell = {
            let mut slots = self.slots.lock().unwrap_or_else(PoisonError::into_inner);
            let now = Instant::now();
            slots.retain(|_, slot| now.duration_since(slot.at) < KEPT_FOR);
            if slots.len() >= KEPT
                && !slots.contains_key(&key)
                && let Some(oldest) = slots
                    .iter()
                    .min_by_key(|(_, slot)| slot.at)
                    .map(|(key, _)| key.clone())
            {
                slots.remove(&oldest);
            }
            Arc::clone(
                &slots
                    .entry(key.clone())
                    .or_insert_with(|| Slot {
                        cell: Arc::new(OnceCell::new()),
                        at: now,
                    })
                    .cell,
            )
        };
        let answer = cell.get_or_init(start).await.clone();
        if answer.is_err() {
            // Forgotten (only this attempt's cell): the retry tries again.
            let mut slots = self.slots.lock().unwrap_or_else(PoisonError::into_inner);
            if slots.get(&key).is_some_and(|slot| Arc::ptr_eq(&slot.cell, &cell)) {
                slots.remove(&key);
            }
        }
        answer
    }
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicUsize, Ordering};

    use super::*;

    #[tokio::test]
    async fn several_starts_with_one_id_in_the_same_tick_run_once() {
        let once = SubmitOnce::<String, String>::default();
        let runs = AtomicUsize::new(0);
        let start = || async {
            runs.fetch_add(1, Ordering::SeqCst);
            tokio::task::yield_now().await;
            Ok::<_, String>("turn-1".to_owned())
        };
        let (a, b, c) = tokio::join!(
            once.run("w1", Some("s1"), start),
            once.run("w1", Some("s1"), start),
            once.run("w1", Some("s1"), start),
        );
        assert_eq!((a, b, c), (Ok("turn-1".into()), Ok("turn-1".into()), Ok("turn-1".into())));
        assert_eq!(runs.load(Ordering::SeqCst), 1);
        // A later repeat answers the same turn without starting.
        assert_eq!(once.run("w1", Some("s1"), start).await, Ok("turn-1".into()));
        assert_eq!(runs.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn another_id_or_workspace_or_none_starts_again() {
        let once = SubmitOnce::<usize, String>::default();
        let runs = AtomicUsize::new(0);
        let start = || async { Ok::<_, String>(runs.fetch_add(1, Ordering::SeqCst)) };
        assert_eq!(once.run("w1", Some("s1"), start).await, Ok(0));
        assert_eq!(once.run("w1", Some("s2"), start).await, Ok(1));
        assert_eq!(once.run("w2", Some("s1"), start).await, Ok(2));
        assert_eq!(once.run("w1", None, start).await, Ok(3));
        assert_eq!(once.run("w1", None, start).await, Ok(4));
    }

    #[tokio::test]
    async fn a_refused_start_is_forgotten_so_its_retry_runs() {
        let once = SubmitOnce::<usize, String>::default();
        assert_eq!(
            once.run("w1", Some("s1"), || async { Err::<usize, _>("refused".to_owned()) })
                .await,
            Err("refused".into())
        );
        assert_eq!(once.run("w1", Some("s1"), || async { Ok::<_, String>(7) }).await, Ok(7));
    }

    #[tokio::test]
    async fn keeps_at_most_kept_answers() {
        let once = SubmitOnce::<usize, String>::default();
        for n in 0..=KEPT {
            let id = n.to_string();
            once.run("w", Some(&id), || async move { Ok::<_, String>(n) }).await.ok();
        }
        assert!(once.slots.lock().unwrap().len() <= KEPT);
    }
}
