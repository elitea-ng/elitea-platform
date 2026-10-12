//! Listener-owned execution tasks retain leases and receipts after RPC disconnects.
use std::{
    future::Future,
    sync::{Arc, Mutex},
};

use tokio::{
    sync::oneshot,
    task::{JoinError, JoinSet},
};
use tonic::Status;

pub(super) struct JobOwners {
    limit: usize,
    state: Mutex<State>,
}

struct State {
    closed: bool,
    tasks: JoinSet<()>,
}

/// The listener drops this guard before its database pool can close.
pub(super) struct ServeGuard(Arc<JobOwners>);

impl Drop for ServeGuard {
    fn drop(&mut self) {
        self.0.abort();
    }
}

impl JobOwners {
    pub(super) fn new(limit: usize) -> Self {
        Self {
            limit,
            state: Mutex::new(State {
                closed: false,
                tasks: JoinSet::new(),
            }),
        }
    }

    pub(super) fn serve_guard(self: &Arc<Self>) -> ServeGuard {
        ServeGuard(Arc::clone(self))
    }

    /// Admit only validated work. Receiver cancellation does not cancel its owner.
    pub(super) async fn run<T: Send + 'static>(
        &self,
        operation: impl Future<Output = Result<T, Status>> + Send + 'static,
    ) -> Result<T, Status> {
        let result = {
            let mut state = self.state.lock().map_err(|_| {
                Status::internal("The sandbox execution task owner is unavailable.")
            })?;
            while let Some(result) = state.tasks.try_join_next() {
                observe_join(result);
            }
            if state.closed {
                return Err(Status::unavailable(
                    "The sandbox listener is shutting down.",
                ));
            }
            if state.tasks.len() >= self.limit {
                return Err(Status::resource_exhausted(
                    "The sandbox supervisor is at capacity.",
                ));
            }
            let (sender, receiver) = oneshot::channel();
            state.tasks.spawn(async move {
                let result = operation.await;
                // The ledger retains completion if the RPC receiver has disconnected.
                let _ = sender.send(result);
            });
            receiver
        };
        result.await.map_err(|_| match self.state.lock() {
            Ok(state) if state.closed => Status::unavailable(
                "The sandbox listener is shutting down. Reconcile the same activation.",
            ),
            _ => Status::internal("The sandbox execution task ended without a response."),
        })?
    }

    fn abort(&self) {
        // Poisoning must not let work escape the listener's shutdown boundary.
        let mut state = self
            .state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        state.closed = true;
        state.tasks.abort_all();
    }

    pub(super) async fn shutdown(&self) {
        let mut tasks = {
            let mut state = self
                .state
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            state.closed = true;
            std::mem::take(&mut state.tasks)
        };
        tasks.abort_all();
        while let Some(result) = tasks.join_next().await {
            observe_join(result);
        }
    }
}

fn observe_join(result: Result<(), JoinError>) {
    if let Err(error) = result
        && !error.is_cancelled()
    {
        tracing::error!(
            event = "sandbox_execution_task_failed",
            panic = error.is_panic(),
            "The sandbox execution task failed; reconcile its durable receipt"
        );
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicBool, Ordering};

    struct Dropped(Arc<AtomicBool>);
    impl Drop for Dropped {
        fn drop(&mut self) {
            self.0.store(true, Ordering::SeqCst);
        }
    }

    #[tokio::test]
    async fn disconnected_receiver_retains_owner_and_receipt() {
        let owners = Arc::new(JobOwners::new(1));
        let (started, start) = oneshot::channel();
        let (finish, finished) = oneshot::channel();
        let (persisted, receipt) = oneshot::channel();
        let caller = {
            let owners = Arc::clone(&owners);
            tokio::spawn(async move {
                owners
                    .run(async move {
                        started.send(()).unwrap();
                        finished.await.unwrap();
                        persisted.send("exact durable receipt").unwrap();
                        Ok(())
                    })
                    .await
            })
        };
        start.await.unwrap();
        caller.abort();
        assert!(caller.await.unwrap_err().is_cancelled());
        assert_eq!(
            owners.run(async { Ok(()) }).await.unwrap_err().code(),
            tonic::Code::ResourceExhausted
        );
        finish.send(()).unwrap();
        assert_eq!(receipt.await.unwrap(), "exact durable receipt");
        owners.shutdown().await;
    }

    #[tokio::test]
    async fn overload_does_not_poll_or_dispatch_another_operation() {
        let owners = Arc::new(JobOwners::new(1));
        let (started, start) = oneshot::channel();
        let (finish, finished) = oneshot::channel();
        let caller = {
            let owners = Arc::clone(&owners);
            tokio::spawn(async move {
                owners
                    .run(async move {
                        started.send(()).unwrap();
                        finished.await.unwrap();
                        Ok(())
                    })
                    .await
            })
        };
        start.await.unwrap();
        let dispatched = Arc::new(AtomicBool::new(false));
        let probe = Arc::clone(&dispatched);
        let refused = owners
            .run(async move {
                probe.store(true, Ordering::SeqCst);
                Ok(())
            })
            .await;
        assert_eq!(refused.unwrap_err().code(), tonic::Code::ResourceExhausted);
        assert!(!dispatched.load(Ordering::SeqCst));
        finish.send(()).unwrap();
        caller.await.unwrap().unwrap();
        owners.shutdown().await;
    }

    #[tokio::test]
    async fn completed_tasks_are_reaped_before_next_admission() {
        let owners = JobOwners::new(1);
        for value in 0..4 {
            assert_eq!(owners.run(async move { Ok(value) }).await.unwrap(), value);
        }
        owners.shutdown().await;
        assert!(owners.state.lock().unwrap().tasks.is_empty());
    }

    #[tokio::test]
    async fn shutdown_aborts_and_drains_owners_then_closes_admission() {
        let owners = Arc::new(JobOwners::new(1));
        let dropped = Arc::new(AtomicBool::new(false));
        let (started, start) = oneshot::channel();
        let caller = {
            let owners = Arc::clone(&owners);
            let dropped = Arc::clone(&dropped);
            tokio::spawn(async move {
                owners
                    .run(async move {
                        let _resource = Dropped(dropped);
                        started.send(()).unwrap();
                        std::future::pending::<Result<(), Status>>().await
                    })
                    .await
            })
        };
        start.await.unwrap();
        owners.shutdown().await;
        assert!(dropped.load(Ordering::SeqCst));
        assert_eq!(
            caller.await.unwrap().unwrap_err().code(),
            tonic::Code::Unavailable
        );
        assert_eq!(
            owners.run(async { Ok(()) }).await.unwrap_err().code(),
            tonic::Code::Unavailable
        );
        assert!(owners.state.lock().unwrap().tasks.is_empty());
    }

    #[tokio::test]
    async fn listener_scope_drop_aborts_owned_work() {
        let owners = Arc::new(JobOwners::new(1));
        let guard = owners.serve_guard();
        let dropped = Arc::new(AtomicBool::new(false));
        let (started, start) = oneshot::channel();
        let caller = {
            let owners = Arc::clone(&owners);
            let dropped = Arc::clone(&dropped);
            tokio::spawn(async move {
                owners
                    .run(async move {
                        let _resource = Dropped(dropped);
                        started.send(()).unwrap();
                        std::future::pending::<Result<(), Status>>().await
                    })
                    .await
            })
        };
        start.await.unwrap();
        drop(guard);
        assert_eq!(
            caller.await.unwrap().unwrap_err().code(),
            tonic::Code::Unavailable
        );
        assert!(dropped.load(Ordering::SeqCst));
        owners.shutdown().await;
    }

    #[tokio::test]
    async fn panic_has_safe_response_and_does_not_leak_capacity() {
        let owners = JobOwners::new(1);
        let error = owners
            .run::<()>(async { panic!("PRIVATE_TASK_PANIC") })
            .await
            .unwrap_err();
        assert_eq!(error.code(), tonic::Code::Internal);
        assert!(!error.message().contains("PRIVATE_TASK_PANIC"));
        assert_eq!(owners.run(async { Ok(42) }).await.unwrap(), 42);
        owners.shutdown().await;
    }
}
