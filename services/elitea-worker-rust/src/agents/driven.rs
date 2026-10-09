//! Keep an inner agent stream or tool future progressing while its caller
//! handles a side channel.
//!
//! A `select!` that races `inner.next()` against a side channel drops the
//! losing `next()` future, but not the inner stream: the stream stays
//! suspended wherever it was, possibly mid-transaction holding a session
//! writer row lock. If the caller then yields or forwards the side event, the
//! Runner persists it under that same lock, and the suspended stream is never
//! polled again to release it (2026-10-09 "answer card 1" self-deadlock).
//!
//! A [`DrivenTask`] polls its future inline first; only work that is still
//! pending moves to an owned task, so a lost select arm never parks it. A
//! [`DrivenEventStream`] pulls each item through one such task, in lockstep:
//! the next item is only requested when the caller asks for it, which keeps
//! the inner stream's ordering and effect timing identical to direct polling.
//! Ready work costs no task and keeps its inline scheduling.

use std::future::Future;
use std::panic::{AssertUnwindSafe, catch_unwind};
use std::pin::Pin;
use std::task::{Context, Poll};

use adk_rust::futures::StreamExt as _;
use adk_rust::{AdkError, ErrorCategory, ErrorComponent, Event, EventStream};
use tokio::task::JoinHandle;
use tracing::Instrument as _;

type BoxedWork<T> = Pin<Box<dyn Future<Output = T> + Send + 'static>>;

enum Stage<T> {
    /// Not yet pending: polled in the caller's task.
    Inline(BoxedWork<T>),
    /// Pending once: finished on its own task.
    Owned(JoinHandle<T>),
    /// The output (or failure) was taken.
    Done,
}

/// Work the caller awaits that keeps progressing when the await is dropped.
///
/// Awaiting `&mut DrivenTask` is cancellation-safe: once the work has been
/// pending it runs on an owned task, so dropping the await leaves it running
/// and awaiting again resumes it. Dropping the `DrivenTask` aborts the task.
/// A panic, inline or on the task, fails closed with a typed error.
pub(crate) struct DrivenTask<T> {
    stage: Stage<T>,
}

impl<T: Send + 'static> DrivenTask<T> {
    pub(crate) fn new(work: impl Future<Output = T> + Send + 'static) -> Self {
        Self {
            stage: Stage::Inline(Box::pin(work.in_current_span())),
        }
    }

    /// Stop the work and wait until it no longer runs, so nothing it owns
    /// (channel senders, transactions) outlives this call.
    pub(crate) async fn cancel(mut self) {
        if let Stage::Owned(handle) = &mut self.stage {
            handle.abort();
            let _ = handle.await;
        }
        self.stage = Stage::Done;
    }
}

impl<T: Send + 'static> Future for DrivenTask<T> {
    type Output = adk_rust::Result<T>;

    fn poll(mut self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<Self::Output> {
        if let Stage::Inline(work) = &mut self.stage {
            match catch_unwind(AssertUnwindSafe(|| work.as_mut().poll(context))) {
                Ok(Poll::Ready(output)) => {
                    self.stage = Stage::Done;
                    return Poll::Ready(Ok(output));
                }
                Ok(Poll::Pending) => {
                    if let Stage::Inline(work) = std::mem::replace(&mut self.stage, Stage::Done) {
                        self.stage = Stage::Owned(tokio::spawn(work));
                    }
                }
                Err(_) => {
                    self.stage = Stage::Done;
                    return Poll::Ready(Err(panicked()));
                }
            }
        }
        let Stage::Owned(handle) = &mut self.stage else {
            return Poll::Ready(Err(driven_task_error()));
        };
        let joined = std::task::ready!(Pin::new(handle).poll(context));
        self.stage = Stage::Done;
        Poll::Ready(joined.map_err(|error| {
            if error.is_panic() {
                panicked()
            } else {
                driven_task_error()
            }
        }))
    }
}

impl<T> Drop for DrivenTask<T> {
    fn drop(&mut self) {
        if let Stage::Owned(handle) = &self.stage {
            handle.abort();
        }
    }
}

type Pull = (EventStream, Option<adk_rust::Result<Event>>);

/// An agent [`EventStream`] whose pending pulls finish on an owned task.
pub(crate) struct DrivenEventStream {
    idle: Option<EventStream>,
    pull: Option<DrivenTask<Pull>>,
}

impl DrivenEventStream {
    #[must_use]
    pub(crate) fn new(stream: EventStream) -> Self {
        Self {
            idle: Some(stream),
            pull: None,
        }
    }

    /// The next inner item, or `None` once the inner stream has ended.
    ///
    /// Cancellation-safe: a dropped call keeps its pull running, and the next
    /// call returns that pull's item. A panicked pull ends the stream with a
    /// typed error.
    pub(crate) async fn next(&mut self) -> Option<adk_rust::Result<Event>> {
        if self.pull.is_none() {
            let mut stream = self.idle.take()?;
            self.pull = Some(DrivenTask::new(async move {
                let item = stream.next().await;
                (stream, item)
            }));
        }
        let joined = self.pull.as_mut()?.await;
        self.pull = None;
        match joined {
            Ok((stream, item)) => {
                if item.is_some() {
                    self.idle = Some(stream);
                }
                item
            }
            Err(error) => Some(Err(error)),
        }
    }
}

fn panicked() -> AdkError {
    tracing::error!("an agent task panicked; the execution fails closed");
    driven_task_error()
}

fn driven_task_error() -> AdkError {
    AdkError::new(
        ErrorComponent::Agent,
        ErrorCategory::Internal,
        "elitea_agent.driven_task_failed",
        "an agent task stopped before producing its result",
    )
}

#[cfg(test)]
#[path = "driven_tests.rs"]
mod tests;
