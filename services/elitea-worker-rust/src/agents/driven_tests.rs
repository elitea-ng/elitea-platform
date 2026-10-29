use std::cell::Cell;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::time::Duration;

use adk_rust::{Event, EventStream};
use tokio::sync::{Notify, oneshot};

use super::{DrivenEventStream, DrivenTask};

thread_local! {
    /// Tasks spawned by `DrivenTask` on this test thread (budget tests use a
    /// current-thread runtime, so every spawn happens here).
    pub(super) static SPAWNED: Cell<usize> = const { Cell::new(0) };
}

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
            yield Ok(Event::with_id(format!("item-{index}"), "invocation"));
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

#[tokio::test(flavor = "current_thread")]
async fn ready_items_are_returned_inline_without_yielding_to_siblings() {
    let sibling_ran = Arc::new(AtomicBool::new(false));
    let marker = sibling_ran.clone();
    let sibling = tokio::spawn(async move { marker.store(true, Ordering::SeqCst) });
    let mut stream = DrivenEventStream::new(Box::pin(async_stream::stream! {
        for index in 0..3 {
            yield Ok(Event::with_id(format!("ready-{index}"), "invocation"));
        }
    }));
    for _ in 0..3 {
        stream.next().await.expect("item").expect("ok");
    }
    assert!(stream.next().await.is_none());
    assert!(
        !sibling_ran.load(Ordering::SeqCst),
        "ready items must not yield to the scheduler"
    );
    sibling.await.expect("sibling");
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
    // Inline (before any pending point) and on the owned task alike.
    for pending_first in [false, true] {
        let mut stream = DrivenEventStream::new(Box::pin(async_stream::stream! {
            if pending_first {
                tokio::task::yield_now().await;
            }
            assert!(!std::hint::black_box(true), "inner stream defect");
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
}

#[tokio::test]
async fn cancel_returns_only_after_the_task_released_what_it_owns() {
    let dropped = Arc::new(AtomicBool::new(false));
    let guard = DropFlag(dropped.clone());
    let mut task = DrivenTask::new(async move {
        let _guard = guard;
        std::future::pending::<()>().await;
    });
    // One inline poll moves the pending work onto its owned task.
    assert!(
        tokio::time::timeout(Duration::from_millis(20), &mut task)
            .await
            .is_err()
    );
    tokio::time::timeout(BOUND, task.cancel())
        .await
        .expect("bounded cancel");
    assert!(dropped.load(Ordering::SeqCst));
}

#[tokio::test]
async fn driven_task_output_is_returned_and_its_await_is_cancellation_safe() {
    let (release, gate) = oneshot::channel::<u32>();
    let mut task = DrivenTask::new(async move { gate.await.unwrap_or(0) });
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

fn nest(stream: EventStream) -> EventStream {
    Box::pin(adk_rust::futures::stream::unfold(
        DrivenEventStream::new(stream),
        |mut driven| async move { driven.next().await.map(|item| (item, driven)) },
    ))
}

#[tokio::test(flavor = "current_thread")]
async fn nested_wrappers_spawn_no_task_for_ready_items_and_one_per_level_when_pending() {
    const DEPTH: usize = 3;
    let mut stream: EventStream = Box::pin(async_stream::stream! {
        yield Ok(Event::with_id("ready-1", "invocation"));
        yield Ok(Event::with_id("ready-2", "invocation"));
        tokio::task::yield_now().await;
        yield Ok(Event::with_id("pending", "invocation"));
    });
    for _ in 0..DEPTH - 1 {
        stream = nest(stream);
    }
    let mut outer = DrivenEventStream::new(stream);
    let mut spawned_per_item = Vec::new();
    while let Some(item) = outer.next().await {
        item.expect("ok item");
        spawned_per_item.push(SPAWNED.with(|spawned| spawned.replace(0)));
    }
    // Budget: ready work never costs a task; a pending item costs exactly one
    // task per driven level, bounded by nesting depth.
    assert_eq!(spawned_per_item, vec![0, 0, DEPTH]);
}

#[tokio::test]
async fn cancel_returns_only_after_an_in_flight_pull_dropped_the_inner_stream() {
    let dropped = Arc::new(AtomicBool::new(false));
    let guard = DropFlag(dropped.clone());
    let mut stream = DrivenEventStream::new(Box::pin(async_stream::stream! {
        let _guard = guard;
        std::future::pending::<()>().await;
        yield Ok(Event::with_id("never", "invocation"));
    }));
    assert!(
        tokio::time::timeout(Duration::from_millis(20), stream.next())
            .await
            .is_err(),
        "the pull is pending on its task"
    );
    tokio::time::timeout(BOUND, stream.cancel())
        .await
        .expect("bounded cancel");
    assert!(
        dropped.load(Ordering::SeqCst),
        "the inner stream is gone when cancel returns"
    );
}

/// Structural guard: a `select!` arm that polls an agent stream directly
/// leaves it suspended (possibly holding a session writer lock) whenever
/// another arm wins. Every such arm must poll a `DrivenEventStream`, or be
/// listed here with the reason it cannot strand a write.
#[test]
fn no_select_arm_polls_a_raw_agent_stream() {
    const ALLOWED: &[(&str, &str, &str)] = &[
        (
            "libs/rust/agent-runtime/src/graph/fanout/runner.rs",
            "running",
            "FuturesUnordered of fan-out children polled only by this loop; the stop arm drains it",
        ),
        (
            "src/execution/native_agent_lifecycle.rs",
            "native",
            "the lease arm returns and the run is dropped before the lease close; other arms finish the event first",
        ),
    ];
    let manifest = std::path::Path::new(env!("CARGO_MANIFEST_DIR"));
    let roots = [
        (manifest.join("src"), "src"),
        (
            manifest.join("../../libs/rust/agent-runtime/src"),
            "libs/rust/agent-runtime/src",
        ),
    ];
    let mut violations = Vec::new();
    let mut seen_allowed = Vec::new();
    for (root, label) in &roots {
        let mut pending = vec![root.clone()];
        while let Some(dir) = pending.pop() {
            for entry in std::fs::read_dir(&dir).expect("read source dir") {
                let path = entry.expect("dir entry").path();
                if path.is_dir() {
                    pending.push(path);
                    continue;
                }
                let name = path
                    .file_name()
                    .and_then(|n| n.to_str())
                    .unwrap_or_default();
                if !path
                    .extension()
                    .is_some_and(|extension| extension.eq_ignore_ascii_case("rs"))
                    || name.ends_with("_tests.rs")
                {
                    continue;
                }
                let source = std::fs::read_to_string(&path).expect("read source");
                let relative = format!(
                    "{label}/{}",
                    path.strip_prefix(root)
                        .expect("walked path stays under its root")
                        .to_string_lossy()
                );
                for (select_at, receiver) in raw_select_polls(&source) {
                    let Some(ident) = receiver else {
                        violations.push(format!(
                            "{relative}: a select! arm polls a non-local receiver"
                        ));
                        continue;
                    };
                    if driven_binding_before(&source, &ident, select_at) {
                        continue;
                    }
                    if ALLOWED
                        .iter()
                        .any(|(file, allowed, _)| *file == relative && *allowed == ident)
                    {
                        if !seen_allowed.contains(&(relative.clone(), ident.clone())) {
                            seen_allowed.push((relative.clone(), ident));
                        }
                        continue;
                    }
                    violations.push(format!("{relative}: `{ident}`"));
                }
            }
        }
    }
    assert!(
        violations.is_empty(),
        "select! arms poll a raw agent stream; wrap it in DrivenEventStream or justify it: {violations:?}"
    );
    assert_eq!(
        seen_allowed.len(),
        ALLOWED.len(),
        "a stale allowlist entry hides nothing: {seen_allowed:?}"
    );
}

/// Whether the nearest `let mut ident` binding before `select_at` wraps the
/// stream in a `DrivenEventStream`.
fn driven_binding_before(source: &str, ident: &str, select_at: usize) -> bool {
    let head = &source[..select_at];
    let pattern = format!("let mut {ident}");
    let mut nearest = None;
    let mut cursor = 0;
    while let Some(found) = head[cursor..].find(&pattern) {
        let at = cursor + found;
        let next = head[at + pattern.len()..].chars().next();
        if matches!(next, Some(' ' | ':' | '=')) {
            nearest = Some(at);
        }
        cursor = at + pattern.len();
    }
    nearest.is_some_and(|at| {
        source[at..]
            .split(';')
            .next()
            .is_some_and(|binding| binding.contains("DrivenEventStream::new"))
    })
}

/// Every `.next()` / `.next_event()` poll inside a `select!` body: the body's
/// offset and the polled local identifier, or `None` when the receiver is not
/// a plain local (a field, a call result, a re-borrow), which the guard cannot
/// trace and therefore refuses.
fn raw_select_polls(source: &str) -> Vec<(usize, Option<String>)> {
    let mut found = Vec::new();
    let mut offset = 0;
    while let Some(start) = source[offset..].find("select!") {
        let select_at = offset + start;
        let after = &source[select_at..];
        let Some(open) = after.find('{') else { break };
        let mut depth = 0_usize;
        let mut end = open;
        for (index, ch) in after[open..].char_indices() {
            match ch {
                '{' => depth += 1,
                '}' => {
                    depth -= 1;
                    if depth == 0 {
                        end = open + index;
                        break;
                    }
                }
                _ => {}
            }
        }
        let body = &after[open..end];
        for marker in [".next()", ".next_event()"] {
            let mut cursor = 0;
            while let Some(at) = body[cursor..].find(marker) {
                let at = cursor + at;
                let before = &body[..at];
                let ident = before
                    .chars()
                    .rev()
                    .take_while(|ch| ch.is_alphanumeric() || *ch == '_')
                    .collect::<String>()
                    .chars()
                    .rev()
                    .collect::<String>();
                let preceded_by = before[..before.len() - ident.len()].chars().next_back();
                let local = !ident.is_empty() && !matches!(preceded_by, Some('.' | ')' | ']'));
                let receiver = local.then_some(ident);
                if !found.contains(&(select_at, receiver.clone())) {
                    found.push((select_at, receiver));
                }
                cursor = at + marker.len();
            }
        }
        offset = select_at + end.max(open) + 1;
    }
    found
}

#[test]
fn the_guard_refuses_untraceable_receivers_and_uses_the_nearest_binding() {
    let source = "let mut events = DrivenEventStream::new(x);\n\
                  let mut events = raw;\n\
                  tokio::select! { e = events.next() => {}, f = self.inner.next() => {}, \
                  g = stream.as_mut().next() => {} }";
    let polls = raw_select_polls(source);
    assert!(
        polls
            .iter()
            .any(|(_, receiver)| receiver.as_deref() == Some("events"))
    );
    assert_eq!(
        polls
            .iter()
            .filter(|(_, receiver)| receiver.is_none())
            .count(),
        1,
        "field and call-result receivers are untraceable: {polls:?}"
    );
    let (select_at, _) = polls[0];
    assert!(
        !driven_binding_before(source, "events", select_at),
        "the nearest binding is the raw one"
    );
}
