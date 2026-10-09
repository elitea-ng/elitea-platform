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
//! A [`DrivenEventStream`] polls the inner stream inline once; an item that is
//! not ready yet is finished on an owned task, so a lost select arm never parks
//! the inner stream. Pulls stay in lockstep: the next item is only requested
//! when the caller asks for it, which keeps the inner stream's ordering and
//! effect timing identical to direct polling. A ready item costs no task, and
//! work that never waits keeps its inline scheduling.

use std::future::Future;
use std::pin::Pin;
use std::task::{Context, Poll};

use adk_rust::futures::StreamExt as _;
use adk_rust::{AdkError, ErrorCategory, ErrorComponent, Event, EventStream};
use tokio::task::JoinHandle;
use tracing::Instrument as _;

/// An owned task whose output the caller awaits; aborted when dropped.
///
/// Awaiting `&mut DrivenTask` is cancellation-safe: dropping that await leaves
/// the task running, and awaiting again resumes the same task.
pub(crate) struct DrivenTask<T> {
    /// `None` once the output was taken.
    handle: Option<JoinHandle<T>>,
}

impl<T: Send + 'static> DrivenTask<T> {
    pub(crate) fn spawn(future: impl Future<Output = T> + Send + 'static) -> Self {
        Self {
            handle: Some(tokio::spawn(future.in_current_span())),
        }
    }

    /// Stop the task and wait until it no longer runs, so nothing it owns
    /// (channel senders, transactions) outlives this call. A no-op once the
    /// output was taken.
    pub(crate) async fn cancel(mut self) {
        if let Some(handle) = self.handle.as_mut() {
            handle.abort();
            let _ = handle.await;
            self.handle = None;
        }
    }
}

impl<T> Future for DrivenTask<T> {
    type Output = adk_rust::Result<T>;

    fn poll(mut self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<Self::Output> {
        let Some(handle) = self.handle.as_mut() else {
            return Poll::Ready(Err(driven_task_error()));
        };
        let joined = std::task::ready!(Pin::new(handle).poll(context));
        self.handle = None;
        Poll::Ready(joined.map_err(|error| {
            if error.is_panic() {
                tracing::error!("an agent task panicked; the execution fails closed");
            }
            driven_task_error()
        }))
    }
}

impl<T> Drop for DrivenTask<T> {
    fn drop(&mut self) {
        if let Some(handle) = &self.handle {
            handle.abort();
        }
    }
}

type Pull = (EventStream, Option<adk_rust::Result<Event>>);

/// An agent [`EventStream`] whose items are each pulled on an owned task.
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
    /// call returns that pull's item. A panicked task pull ends the stream with
    /// a typed error.
    pub(crate) async fn next(&mut self) -> Option<adk_rust::Result<Event>> {
        if self.pull.is_none() {
            let mut stream = self.idle.take()?;
            // One inline poll; it completes within this call, so it is never
            // abandoned. A pending pull is handed to its task before this
            // call can return Pending, so nothing is left un-driven.
            let ready =
                std::future::poll_fn(|context| Poll::Ready(stream.poll_next_unpin(context))).await;
            if let Poll::Ready(item) = ready {
                if item.is_some() {
                    self.idle = Some(stream);
                }
                return item;
            }
            self.pull = Some(DrivenTask::spawn(async move {
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
