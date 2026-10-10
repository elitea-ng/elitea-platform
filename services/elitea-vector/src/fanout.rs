//! Bounded-concurrency fan-out over collections.

use std::future::Future;

use futures_util::StreamExt as _;
use tonic::Status;

/// The most collections an operation without a space queries at once.
pub const MAX_FAN_OUT: usize = 8;

/// Runs `work` over `items` with at most [`MAX_FAN_OUT`] in flight, in no
/// particular order; the first failure is returned and abandons the rest.
///
/// # Errors
/// The first error `work` returns.
pub async fn fan_out<I, T, F, Fut>(items: I, work: F) -> Result<Vec<T>, Status>
where
    I: IntoIterator,
    F: FnMut(I::Item) -> Fut,
    Fut: Future<Output = Result<T, Status>>,
{
    let mut results = futures_util::stream::iter(items)
        .map(work)
        .buffer_unordered(MAX_FAN_OUT);
    let mut collected = Vec::new();
    while let Some(result) = results.next().await {
        collected.push(result?);
    }
    Ok(collected)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;

    #[tokio::test(start_paused = true)]
    async fn the_fan_out_is_concurrent_but_bounded() {
        use std::sync::atomic::{AtomicUsize, Ordering};
        let active = Arc::new(AtomicUsize::new(0));
        let peak = Arc::new(AtomicUsize::new(0));
        let results = fan_out(0_u64..40, |item| {
            let (active, peak) = (Arc::clone(&active), Arc::clone(&peak));
            async move {
                let now = active.fetch_add(1, Ordering::SeqCst) + 1;
                peak.fetch_max(now, Ordering::SeqCst);
                // Later items finish first: completion order is not input order.
                tokio::time::sleep(std::time::Duration::from_millis(100 - item)).await;
                active.fetch_sub(1, Ordering::SeqCst);
                Ok(item)
            }
        })
        .await
        .expect("all succeed");
        assert_eq!(peak.load(Ordering::SeqCst), MAX_FAN_OUT);
        let mut sorted = results.clone();
        sorted.sort_unstable();
        assert_eq!(sorted, (0_u64..40).collect::<Vec<_>>());
    }

    #[tokio::test]
    async fn the_fan_out_returns_the_first_failure() {
        let outcome = fan_out(0..20, |item| async move {
            if item == 5 {
                Err(Status::unavailable("boom"))
            } else {
                Ok(item)
            }
        })
        .await;
        assert_eq!(
            outcome.expect_err("failed").code(),
            tonic::Code::Unavailable
        );
    }
}
