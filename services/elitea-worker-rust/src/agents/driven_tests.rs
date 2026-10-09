use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::time::Duration;

use adk_rust::Event;
use tokio::sync::{Notify, oneshot};

use super::{DrivenEventStream, DrivenTask};

const BOUND: Duration = Duration::from_secs(5);

/// Sets its flag when dropped, i.e. when the owning stream or task is gone.
struct DropFlag(Arc<AtomicBool>);

impl Drop for DropFlag {
    fn drop(&mut self) {
        self.0.store(true, Ordering::SeqCst);
    }
}

async fn eventually(flag: &AtomicBool) -> bool {
    tokio::time::timeout(BOUND, async {
        while !flag.load(Ordering::SeqCst) {
            tokio::task::yield_now().await;
        }
    })
    .await
    .is_ok()
}

#[tokio::test]
async fn a_lost_select_arm_keeps_driving_the_inner_item_and_never_loses_it() {
    let (release, gate) = oneshot::channel::<()>();
    let progressed = Arc::new(AtomicBool::new(false));
    let marker = progressed.clone();
    let mut stream = DrivenEventStream::new(Box::pin(async_stream::stream! {
        // Stands in for a session append parked on a database lock.
        let _ = gate.await;
        marker.store(true, Ordering::SeqCst);
        yield Ok(Event::with_id("inner", "invocation"));
    }));
    // The incident shape: `next()` is pending, then a side arm wins the select.
    let side = Notify::new();
    tokio::select! {
        _ = stream.next() => panic!("the gated inner item cannot be ready yet"),
        () = async {
            tokio::task::yield_now().await;
            side.notify_one();
            side.notified().await;
        } => {}
    }
    // The caller is now busy with the side event and does not poll `stream`.
    release.send(()).expect("release gate");
    assert!(
        eventually(&progressed).await,
        "the inner stream must progress while the caller handles the side event"
    );
    let item = tokio::time::timeout(BOUND, stream.next())
        .await
        .expect("item")
        .expect("one item")
        .expect("ok item");
    assert_eq!(item.id, "inner");
    assert!(stream.next().await.is_none());
    assert!(stream.next().await.is_none(), "an ended stream stays ended");
}

#[tokio::test]
async fn pulls_stay_in_lockstep_with_the_caller() {
    let started = Arc::new(AtomicUsize::new(0));
    let counter = started.clone();
    let mut stream = DrivenEventStream::new(Box::pin(async_stream::stream! {
        for index in 0..3 {
            counter.fetch_add(1, Ordering::SeqCst);
            yield Ok(Event::with_id(&format!("item-{index}"), "invocation"));
        }
    }));
    for index in 0..3 {
        let item = stream.next().await.expect("item").expect("ok");
        assert_eq!(item.id, format!("item-{index}"));
        for _ in 0..16 {
            tokio::task::yield_now().await;
        }
        assert_eq!(
            started.load(Ordering::SeqCst),
            index + 1,
            "the next item must not start before the caller asks for it"
        );
    }
    assert!(stream.next().await.is_none());
}

#[tokio::test]
async fn dropping_the_stream_stops_an_in_flight_pull() {
    let dropped = Arc::new(AtomicBool::new(false));
    let guard = DropFlag(dropped.clone());
    let mut stream = DrivenEventStream::new(Box::pin(async_stream::stream! {
        let _guard = guard;
        std::future::pending::<()>().await;
        yield Ok(Event::with_id("never", "invocation"));
    }));
    assert!(
        tokio::time::timeout(Duration::from_millis(50), stream.next())
            .await
            .is_err()
    );
    drop(stream);
    assert!(
        eventually(&dropped).await,
        "the owned pull task must be aborted with its stream"
    );
}

#[tokio::test]
async fn a_panicking_pull_fails_closed_with_a_typed_error() {
    let mut stream = DrivenEventStream::new(Box::pin(async_stream::stream! {
        if std::hint::black_box(true) {
            panic!("inner stream defect");
        }
        yield Ok(Event::with_id("never", "invocation"));
    }));
    let error = stream
        .next()
        .await
        .expect("typed failure item")
        .expect_err("panic is a failure");
    assert_eq!(error.code, "elitea_agent.driven_task_failed");
    assert!(!error.message.contains("inner stream defect"));
    assert!(stream.next().await.is_none());
}

#[tokio::test]
async fn cancel_returns_only_after_the_task_released_what_it_owns() {
    let dropped = Arc::new(AtomicBool::new(false));
    let guard = DropFlag(dropped.clone());
    let task = DrivenTask::spawn(async move {
        let _guard = guard;
        std::future::pending::<()>().await;
    });
    tokio::task::yield_now().await;
    tokio::time::timeout(BOUND, task.cancel())
        .await
        .expect("bounded cancel");
    assert!(dropped.load(Ordering::SeqCst));
}

#[tokio::test]
async fn driven_task_output_is_returned_and_its_await_is_cancellation_safe() {
    let (release, gate) = oneshot::channel::<u32>();
    let mut task = DrivenTask::spawn(async move { gate.await.unwrap_or(0) });
    assert!(
        tokio::time::timeout(Duration::from_millis(20), &mut task)
            .await
            .is_err()
    );
    release.send(7).expect("release");
    assert_eq!(
        tokio::time::timeout(BOUND, &mut task)
            .await
            .expect("bounded")
            .expect("output"),
        7
    );
}
