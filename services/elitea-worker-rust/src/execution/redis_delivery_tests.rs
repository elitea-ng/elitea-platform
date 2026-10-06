use std::collections::VecDeque;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use tokio::sync::{Notify, Semaphore, watch};

use super::redis_delivery::{
    RedisDeliveryIntake, RedisDeliveryIntakeConfig, RedisDeliveryProcessor, RedisDeliveryRuntime,
    RedisDeliveryRuntimeConfig, redis_intake_failure_is_fatal,
};
use crate::transport::redis_commands::{
    RedisCommandDelivery, RedisCommandLimits, RedisRetirementClient, RedisRetirementClientError,
    RedisRetirementRequest, RedisRetirementResponse,
};
use crate::transport::redis_generation::{
    RedisGenerationFuture, RedisStreamsConnection, RedisStreamsConnector, RedisStreamsHandle,
};
use crate::transport::redis_streams::{
    RedisOwnedPendingPage, RedisReclaimPage, RedisStreamsError, RedisStreamsErrorKind,
};

#[derive(Debug, Eq, PartialEq)]
enum FakeOperation {
    Pending {
        count: u64,
        start_id: String,
    },
    Read {
        count: u64,
        block_millis: u64,
    },
    Reclaim {
        min_idle_millis: u64,
        start_id: String,
        count: u64,
    },
    Heartbeat(Vec<String>),
    Close,
}

struct FakeConnection {
    batch_size: u64,
    reads: Mutex<VecDeque<Result<Vec<RedisCommandDelivery>, RedisStreamsError>>>,
    pending: Mutex<VecDeque<Result<RedisOwnedPendingPage, RedisStreamsError>>>,
    reclaims: Mutex<VecDeque<Result<RedisReclaimPage, RedisStreamsError>>>,
    heartbeats: Mutex<VecDeque<Result<Vec<String>, RedisStreamsError>>>,
    operations: Mutex<Vec<FakeOperation>>,
    operation_changed: Notify,
    read_available: Notify,
    retirement_calls: AtomicUsize,
}

impl FakeConnection {
    fn new(batch_size: u64) -> Self {
        Self {
            batch_size,
            reads: Mutex::new(VecDeque::new()),
            pending: Mutex::new(VecDeque::new()),
            reclaims: Mutex::new(VecDeque::new()),
            heartbeats: Mutex::new(VecDeque::new()),
            operations: Mutex::new(Vec::new()),
            operation_changed: Notify::new(),
            read_available: Notify::new(),
            retirement_calls: AtomicUsize::new(0),
        }
    }

    fn push_read(&self, value: Result<Vec<RedisCommandDelivery>, RedisStreamsError>) {
        self.reads.lock().expect("read queue").push_back(value);
        self.read_available.notify_one();
    }

    fn push_reclaim(&self, value: Result<RedisReclaimPage, RedisStreamsError>) {
        self.reclaims
            .lock()
            .expect("reclaim queue")
            .push_back(value);
    }

    fn push_pending(&self, value: Result<RedisOwnedPendingPage, RedisStreamsError>) {
        self.pending.lock().expect("pending queue").push_back(value);
    }

    fn push_heartbeat(&self, value: Result<Vec<String>, RedisStreamsError>) {
        self.heartbeats
            .lock()
            .expect("heartbeat queue")
            .push_back(value);
    }

    fn operations(&self) -> Vec<FakeOperation> {
        self.operations
            .lock()
            .expect("operation log")
            .iter()
            .map(|operation| match operation {
                FakeOperation::Pending { count, start_id } => FakeOperation::Pending {
                    count: *count,
                    start_id: start_id.clone(),
                },
                FakeOperation::Read {
                    count,
                    block_millis,
                } => FakeOperation::Read {
                    count: *count,
                    block_millis: *block_millis,
                },
                FakeOperation::Reclaim {
                    min_idle_millis,
                    start_id,
                    count,
                } => FakeOperation::Reclaim {
                    min_idle_millis: *min_idle_millis,
                    start_id: start_id.clone(),
                    count: *count,
                },
                FakeOperation::Heartbeat(entries) => FakeOperation::Heartbeat(entries.clone()),
                FakeOperation::Close => FakeOperation::Close,
            })
            .collect()
    }

    async fn wait_for_heartbeat(&self, entry: &str) {
        loop {
            let changed = self.operation_changed.notified();
            if self.operations().iter().any(|operation| {
                matches!(operation, FakeOperation::Heartbeat(entries) if entries.iter().any(|id| id == entry))
            }) {
                return;
            }
            changed.await;
        }
    }

    async fn wait_operations(&self, expected: usize) {
        while self.operations.lock().expect("operation log").len() < expected {
            let changed = self.operation_changed.notified();
            if self.operations.lock().expect("operation log").len() >= expected {
                return;
            }
            changed.await;
        }
    }
}

#[async_trait]
impl RedisRetirementClient for FakeConnection {
    async fn retire_delivery(
        &self,
        _request: RedisRetirementRequest,
    ) -> Result<RedisRetirementResponse, RedisRetirementClientError> {
        self.retirement_calls.fetch_add(1, Ordering::AcqRel);
        Ok(RedisRetirementResponse {
            acknowledged: 1,
            deleted: 1,
            unmapped: 1,
        })
    }
}

impl RedisStreamsConnection for FakeConnection {
    fn delivery_batch_size(&self) -> u64 {
        self.batch_size
    }

    fn read_new(
        self: Arc<Self>,
        count: u64,
        block_millis: u64,
    ) -> RedisGenerationFuture<Result<Vec<RedisCommandDelivery>, RedisStreamsError>> {
        Box::pin(async move {
            self.operations
                .lock()
                .expect("operation log")
                .push(FakeOperation::Read {
                    count,
                    block_millis,
                });
            self.operation_changed.notify_waiters();
            loop {
                if let Some(result) = self.reads.lock().expect("read queue").pop_front() {
                    return result;
                }
                let available = self.read_available.notified();
                if let Some(result) = self.reads.lock().expect("read queue").pop_front() {
                    return result;
                }
                available.await;
            }
        })
    }

    fn read_owned_pending(
        self: Arc<Self>,
        count: u64,
        start_id: String,
    ) -> RedisGenerationFuture<Result<RedisOwnedPendingPage, RedisStreamsError>> {
        Box::pin(async move {
            self.operations
                .lock()
                .expect("operation log")
                .push(FakeOperation::Pending { count, start_id });
            self.operation_changed.notify_waiters();
            self.pending
                .lock()
                .expect("pending queue")
                .pop_front()
                .unwrap_or_else(|| {
                    Ok(RedisOwnedPendingPage {
                        next_start_id: "0-0".to_owned(),
                        deliveries: Vec::new(),
                    })
                })
        })
    }

    fn reclaim_page(
        self: Arc<Self>,
        min_idle_millis: u64,
        start_id: String,
        count: u64,
    ) -> RedisGenerationFuture<Result<RedisReclaimPage, RedisStreamsError>> {
        Box::pin(async move {
            self.operations
                .lock()
                .expect("operation log")
                .push(FakeOperation::Reclaim {
                    min_idle_millis,
                    start_id: start_id.clone(),
                    count,
                });
            self.operation_changed.notify_waiters();
            self.reclaims
                .lock()
                .expect("reclaim queue")
                .pop_front()
                .unwrap_or_else(|| {
                    Ok(RedisReclaimPage {
                        next_start_id: start_id,
                        deliveries: Vec::new(),
                    })
                })
        })
    }

    fn heartbeat_owned_pending(
        self: Arc<Self>,
        entry_ids: Vec<String>,
    ) -> RedisGenerationFuture<Result<Vec<String>, RedisStreamsError>> {
        Box::pin(async move {
            self.operations
                .lock()
                .expect("operation log")
                .push(FakeOperation::Heartbeat(entry_ids.clone()));
            self.operation_changed.notify_waiters();
            self.heartbeats
                .lock()
                .expect("heartbeat queue")
                .pop_front()
                .unwrap_or(Ok(entry_ids))
        })
    }

    fn close(self: Arc<Self>) -> RedisGenerationFuture<Result<(), RedisStreamsError>> {
        Box::pin(async move {
            self.operations
                .lock()
                .expect("operation log")
                .push(FakeOperation::Close);
            self.operation_changed.notify_waiters();
            Ok(())
        })
    }
}

struct FakeConnector {
    connections: Mutex<VecDeque<Arc<FakeConnection>>>,
}

impl FakeConnector {
    fn new(connections: impl IntoIterator<Item = Arc<FakeConnection>>) -> Self {
        Self {
            connections: Mutex::new(connections.into_iter().collect()),
        }
    }
}

impl RedisStreamsConnector for FakeConnector {
    type Connection = FakeConnection;

    fn connect(
        self: Arc<Self>,
    ) -> RedisGenerationFuture<Result<Arc<Self::Connection>, RedisStreamsError>> {
        Box::pin(async move {
            self.connections
                .lock()
                .expect("connection queue")
                .pop_front()
                .ok_or_else(|| {
                    RedisStreamsError::unavailable("the fake Redis connector is exhausted")
                })
        })
    }
}

struct TestProcessor {
    started: AtomicUsize,
    completed: AtomicUsize,
    active: AtomicUsize,
    maximum_active: AtomicUsize,
    started_changed: Notify,
    completed_changed: Notify,
    release: Semaphore,
    entries: Mutex<Vec<String>>,
}

impl TestProcessor {
    fn new() -> Self {
        Self {
            started: AtomicUsize::new(0),
            completed: AtomicUsize::new(0),
            active: AtomicUsize::new(0),
            maximum_active: AtomicUsize::new(0),
            started_changed: Notify::new(),
            completed_changed: Notify::new(),
            release: Semaphore::new(0),
            entries: Mutex::new(Vec::new()),
        }
    }

    async fn wait_started(&self, expected: usize) {
        while self.started.load(Ordering::Acquire) < expected {
            let changed = self.started_changed.notified();
            if self.started.load(Ordering::Acquire) >= expected {
                return;
            }
            changed.await;
        }
    }

    async fn wait_completed(&self, expected: usize) {
        while self.completed.load(Ordering::Acquire) < expected {
            let changed = self.completed_changed.notified();
            if self.completed.load(Ordering::Acquire) >= expected {
                return;
            }
            changed.await;
        }
    }

    fn release(&self, count: usize) {
        self.release.add_permits(count);
    }
}

struct ActiveProcess<'a> {
    processor: &'a TestProcessor,
}

impl ActiveProcess<'_> {
    fn enter(processor: &TestProcessor) -> ActiveProcess<'_> {
        let active = processor.active.fetch_add(1, Ordering::AcqRel) + 1;
        processor.maximum_active.fetch_max(active, Ordering::AcqRel);
        ActiveProcess { processor }
    }
}

impl Drop for ActiveProcess<'_> {
    fn drop(&mut self) {
        self.processor.active.fetch_sub(1, Ordering::AcqRel);
    }
}

#[async_trait]
impl RedisDeliveryProcessor for TestProcessor {
    async fn process(&self, delivery: RedisCommandDelivery) {
        let _active = ActiveProcess::enter(self);
        self.entries
            .lock()
            .expect("processed entries")
            .push(delivery.entry_id().to_owned());
        self.started.fetch_add(1, Ordering::AcqRel);
        self.started_changed.notify_waiters();
        let permit = self.release.acquire().await.expect("release semaphore");
        permit.forget();
        self.completed.fetch_add(1, Ordering::AcqRel);
        self.completed_changed.notify_waiters();
    }
}

fn config(max_concurrency: usize, queue_capacity: usize) -> RedisDeliveryIntakeConfig {
    RedisDeliveryIntakeConfig::new(max_concurrency, queue_capacity, 30_000, 60_000, 100)
        .expect("valid intake config")
}

fn runtime_config(max_concurrency: usize, queue_capacity: usize) -> RedisDeliveryRuntimeConfig {
    RedisDeliveryRuntimeConfig::new(config(max_concurrency, queue_capacity), 100, 1_000)
        .expect("valid runtime config")
}

fn delivery(entry_id: &str) -> RedisCommandDelivery {
    RedisCommandDelivery::decode(
        b"commands",
        entry_id.as_bytes(),
        [(b"signed_envelope".to_vec(), b"signed-command".to_vec())],
        RedisCommandLimits {
            max_entry_bytes: 64 * 1024,
            max_field_bytes: 48 * 1024,
        },
    )
    .expect("valid delivery fixture")
}

fn intake(
    connection: Arc<FakeConnection>,
    config: RedisDeliveryIntakeConfig,
) -> RedisDeliveryIntake<FakeConnector> {
    let connector = Arc::new(FakeConnector::new([connection]));
    let handle = Arc::new(RedisStreamsHandle::new(connector));
    RedisDeliveryIntake::new(handle, config)
}

#[tokio::test(start_paused = true)]
async fn intake_alternates_a_bounded_read_with_the_due_reclaim_turn() {
    let connection = Arc::new(FakeConnection::new(2));
    connection.push_read(Ok(vec![delivery("1-0")]));
    connection.push_reclaim(Ok(RedisReclaimPage {
        next_start_id: "7-0".to_owned(),
        deliveries: vec![delivery("2-0")],
    }));
    let intake = intake(Arc::clone(&connection), config(1, 1));

    let first = intake.next_batch().await.expect("bounded new read");
    assert_eq!(first.len(), 1);
    assert_eq!(first[0].entry_id(), "1-0");
    drop(first);

    tokio::time::advance(Duration::from_millis(100)).await;
    let reclaimed = intake.next_batch().await.expect("due reclaim turn");
    assert_eq!(reclaimed.len(), 1);
    assert_eq!(reclaimed[0].entry_id(), "2-0");

    let operations = connection.operations();
    assert!(matches!(
        operations.as_slice(),
        [
            FakeOperation::Pending { count: 2, start_id: own_start },
            FakeOperation::Read {
                count: 2,
                block_millis: 1..=100
            },
            FakeOperation::Pending { count: 2, start_id: next_own_start },
            FakeOperation::Reclaim {
                min_idle_millis: 60_000,
                start_id,
                count: 2
            }
        ] if start_id == "0-0" && own_start == "0-0" && next_own_start == "0-0"
    ));
}

#[tokio::test(start_paused = true)]
async fn an_owned_pending_entry_is_not_locally_admitted_twice() {
    let connection = Arc::new(FakeConnection::new(2));
    connection.push_read(Ok(vec![delivery("1-0")]));
    connection.push_reclaim(Ok(RedisReclaimPage {
        next_start_id: "0-0".to_owned(),
        deliveries: vec![delivery("1-0")],
    }));
    connection.push_reclaim(Ok(RedisReclaimPage {
        next_start_id: "0-0".to_owned(),
        deliveries: vec![delivery("1-0")],
    }));
    let intake = intake(Arc::clone(&connection), config(1, 1));

    let first = intake.next_batch().await.expect("first delivery");
    assert_eq!(intake.owned_count(), 1);
    tokio::time::advance(Duration::from_millis(100)).await;
    assert!(
        intake
            .next_batch()
            .await
            .expect("duplicate reclaim")
            .is_empty()
    );
    assert_eq!(intake.owned_count(), 1);

    drop(first);
    assert_eq!(intake.owned_count(), 0);
    tokio::time::advance(Duration::from_millis(100)).await;
    let accepted_again = intake.next_batch().await.expect("later reclaim");
    assert_eq!(accepted_again.len(), 1);
}

#[tokio::test]
async fn ownership_capacity_is_retained_until_the_delivery_owner_drops() {
    let connection = Arc::new(FakeConnection::new(2));
    connection.push_read(Ok(vec![delivery("1-0"), delivery("2-0")]));
    connection.push_read(Ok(vec![delivery("3-0")]));
    let intake = intake(connection, config(1, 1));
    let first = intake.next_batch().await.expect("capacity-filling batch");
    assert_eq!(first.len(), 2);

    let waiting = tokio::spawn(async move {
        let result = intake.next_batch().await;
        (intake, result)
    });
    tokio::task::yield_now().await;
    assert!(!waiting.is_finished());

    drop(first);
    let (intake, next) = waiting.await.expect("waiting intake task");
    let next = next.expect("next admitted batch");
    assert_eq!(next.len(), 1);
    assert_eq!(intake.owned_count(), 1);
}

#[tokio::test]
async fn processing_retains_heartbeat_identity_until_the_future_really_finishes() {
    let connection = Arc::new(FakeConnection::new(2));
    connection.push_read(Ok(vec![delivery("2-0"), delivery("1-0")]));
    connection.push_heartbeat(Ok(vec!["1-0".to_owned(), "2-0".to_owned()]));
    let intake = intake(Arc::clone(&connection), config(1, 1));
    let mut batch = intake.next_batch().await.expect("owned deliveries");
    let processing = batch.pop().expect("processing delivery");
    let queued = batch.pop().expect("queued delivery");
    let started = Arc::new(Notify::new());
    let release = Arc::new(Notify::new());
    let process_started = Arc::clone(&started);
    let process_release = Arc::clone(&release);
    let task = tokio::spawn(async move {
        processing
            .process(|_delivery| async move {
                process_started.notify_one();
                process_release.notified().await;
                7_u8
            })
            .await
    });
    started.notified().await;

    assert_eq!(intake.heartbeat_owned().await.expect("PEL heartbeat"), 2);
    assert!(matches!(
        connection.operations().last(),
        Some(FakeOperation::Heartbeat(entries))
            if entries == &["1-0".to_owned(), "2-0".to_owned()]
    ));
    assert_eq!(intake.owned_count(), 2);

    drop(queued);
    release.notify_one();
    assert_eq!(task.await.expect("processing task").expect("processing"), 7);
    assert_eq!(intake.owned_count(), 0);
}

#[tokio::test]
async fn retryable_generation_failure_is_not_replayed_and_next_turn_reconnects() {
    let first = Arc::new(FakeConnection::new(1));
    first.push_read(Err(RedisStreamsError::unavailable(
        "the first fake generation is unavailable",
    )));
    let second = Arc::new(FakeConnection::new(1));
    second.push_read(Ok(vec![delivery("9-0")]));
    let connector = Arc::new(FakeConnector::new([
        Arc::clone(&first),
        Arc::clone(&second),
    ]));
    let handle = Arc::new(RedisStreamsHandle::new(connector));
    let intake = RedisDeliveryIntake::new(handle, config(1, 1));

    let error = intake.next_batch().await.err().expect("first read fails");
    assert_eq!(error.kind(), RedisStreamsErrorKind::DependencyUnavailable);
    assert!(!redis_intake_failure_is_fatal(&error));
    assert_eq!(first.operations().len(), 2);

    let next = intake.next_batch().await.expect("explicit next turn");
    assert_eq!(next.len(), 1);
    assert_eq!(second.operations().len(), 1);
    assert!(redis_intake_failure_is_fatal(&RedisStreamsError::protocol(
        "malformed Redis response"
    )));
}

#[tokio::test]
async fn missing_consumer_group_stops_before_processing_and_closes_transport() {
    let connection = Arc::new(FakeConnection::new(1));
    connection.push_read(Err(RedisStreamsError::consumer_group_missing(
        "Redis command intake failed",
    )));
    let connector = Arc::new(FakeConnector::new([Arc::clone(&connection)]));
    let handle = Arc::new(RedisStreamsHandle::new(connector));
    let processor = Arc::new(TestProcessor::new());
    let runtime = RedisDeliveryRuntime::new(handle, Arc::clone(&processor), runtime_config(2, 2));
    let (_stop, stopped) = watch::channel(false);
    let error = tokio::time::timeout(Duration::from_secs(1), runtime.run(stopped))
        .await
        .expect("missing group drains promptly")
        .expect_err("missing group remains fatal");
    assert_eq!(error.code(), "redis_streams.consumer_group_missing");
    assert!(!error.retryable());
    assert_eq!(processor.started.load(Ordering::Acquire), 0);
    assert_eq!(processor.completed.load(Ordering::Acquire), 0);
    assert!(matches!(
        connection.operations().last(),
        Some(FakeOperation::Close)
    ));
    assert_eq!(
        connection
            .operations()
            .iter()
            .filter(|operation| matches!(operation, FakeOperation::Read { .. }))
            .count(),
        1
    );
}

#[tokio::test(start_paused = true)]
async fn runtime_stops_intake_but_heartbeats_until_owned_processing_drains() {
    let connection = Arc::new(FakeConnection::new(2));
    connection.push_read(Ok(vec![delivery("1-0")]));
    let connector = Arc::new(FakeConnector::new([Arc::clone(&connection)]));
    let handle = Arc::new(RedisStreamsHandle::new(connector));
    let processor = Arc::new(TestProcessor::new());
    let runtime = RedisDeliveryRuntime::new(handle, Arc::clone(&processor), runtime_config(1, 1));
    let (stop, stopped) = watch::channel(false);
    let task = tokio::spawn(runtime.run(stopped));
    processor.wait_started(1).await;

    stop.send(true).expect("request runtime stop");
    tokio::task::yield_now().await;
    assert!(!task.is_finished());
    tokio::time::advance(Duration::from_millis(100)).await;
    connection.wait_for_heartbeat("1-0").await;
    assert!(connection.operations().iter().any(
        |operation| matches!(operation, FakeOperation::Heartbeat(entries) if entries == &["1-0"])
    ));

    processor.release(1);
    task.await
        .expect("runtime task")
        .expect("graceful runtime drain");
    assert_eq!(processor.active.load(Ordering::Acquire), 0);
    assert!(matches!(
        connection.operations().last(),
        Some(FakeOperation::Close)
    ));
}

#[tokio::test]
async fn runtime_worker_count_is_a_structural_processing_bound() {
    let connection = Arc::new(FakeConnection::new(4));
    connection.push_read(Ok(vec![
        delivery("1-0"),
        delivery("2-0"),
        delivery("3-0"),
        delivery("4-0"),
    ]));
    let connector = Arc::new(FakeConnector::new([connection]));
    let handle = Arc::new(RedisStreamsHandle::new(connector));
    let processor = Arc::new(TestProcessor::new());
    let runtime = RedisDeliveryRuntime::new(handle, Arc::clone(&processor), runtime_config(2, 2));
    let (stop, stopped) = watch::channel(false);
    let task = tokio::spawn(runtime.run(stopped));

    processor.wait_started(2).await;
    assert_eq!(processor.started.load(Ordering::Acquire), 2);
    assert_eq!(processor.maximum_active.load(Ordering::Acquire), 2);
    stop.send(true).expect("request runtime stop");
    processor.release(4);
    processor.wait_completed(4).await;
    task.await
        .expect("runtime task")
        .expect("bounded runtime drain");

    assert_eq!(processor.maximum_active.load(Ordering::Acquire), 2);
    assert_eq!(
        processor.entries.lock().expect("processed entries").len(),
        4
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn runtime_hands_an_enqueued_delivery_to_a_worker_while_intake_waits() {
    let connection = Arc::new(FakeConnection::new(1));
    connection.push_read(Ok(vec![delivery("1-0")]));
    let connector = Arc::new(FakeConnector::new([connection]));
    let handle = Arc::new(RedisStreamsHandle::new(connector));
    let processor = Arc::new(TestProcessor::new());
    let runtime = RedisDeliveryRuntime::new(handle, Arc::clone(&processor), runtime_config(1, 1));
    let (stop, stopped) = watch::channel(false);
    let task = tokio::spawn(runtime.run(stopped));

    tokio::time::timeout(Duration::from_secs(1), processor.wait_started(1))
        .await
        .expect("an enqueued delivery reaches a worker");
    stop.send(true).expect("request runtime stop");
    processor.release(1);
    task.await
        .expect("runtime task")
        .expect("runtime after queue handoff");
}

#[tokio::test(start_paused = true)]
async fn runtime_reconnects_only_after_the_stop_aware_dependency_delay() {
    let first = Arc::new(FakeConnection::new(1));
    first.push_read(Err(RedisStreamsError::unavailable(
        "the first runtime generation is unavailable",
    )));
    let second = Arc::new(FakeConnection::new(1));
    second.push_reclaim(Ok(RedisReclaimPage {
        next_start_id: "0-0".to_owned(),
        deliveries: vec![delivery("9-0")],
    }));
    let connector = Arc::new(FakeConnector::new([
        Arc::clone(&first),
        Arc::clone(&second),
    ]));
    let handle = Arc::new(RedisStreamsHandle::new(connector));
    let processor = Arc::new(TestProcessor::new());
    let runtime = RedisDeliveryRuntime::new(handle, Arc::clone(&processor), runtime_config(1, 1));
    let (stop, stopped) = watch::channel(false);
    let task = tokio::spawn(runtime.run(stopped));
    first.wait_operations(2).await;

    tokio::time::advance(Duration::from_millis(99)).await;
    tokio::task::yield_now().await;
    assert!(second.operations().is_empty());
    tokio::time::advance(Duration::from_millis(1)).await;
    processor.wait_started(1).await;
    assert!(matches!(
        second.operations().get(1),
        Some(FakeOperation::Reclaim { .. })
    ));

    stop.send(true).expect("request runtime stop");
    processor.release(1);
    task.await
        .expect("runtime task")
        .expect("runtime after replacement");
}

#[tokio::test(start_paused = true)]
async fn drain_timeout_abandons_only_local_ownership_and_reports_a_safe_code() {
    let connection = Arc::new(FakeConnection::new(1));
    connection.push_read(Ok(vec![delivery("1-0")]));
    let connector = Arc::new(FakeConnector::new([connection]));
    let handle = Arc::new(RedisStreamsHandle::new(connector));
    let processor = Arc::new(TestProcessor::new());
    let runtime = RedisDeliveryRuntime::new(handle, Arc::clone(&processor), runtime_config(1, 1));
    let (stop, stopped) = watch::channel(false);
    let task = tokio::spawn(runtime.run(stopped));
    processor.wait_started(1).await;
    stop.send(true).expect("request runtime stop");
    tokio::task::yield_now().await;

    tokio::time::advance(Duration::from_secs(1)).await;
    let error = task
        .await
        .expect("runtime task")
        .expect_err("drain deadline must fail");
    assert_eq!(error.code(), "redis_delivery.drain_timeout");
    assert!(error.retryable());
    assert_eq!(processor.completed.load(Ordering::Acquire), 0);
    assert_eq!(processor.active.load(Ordering::Acquire), 0);
}

#[tokio::test]
async fn cancelling_the_runtime_owner_cannot_detach_a_processing_task() {
    let connection = Arc::new(FakeConnection::new(1));
    connection.push_read(Ok(vec![delivery("1-0")]));
    let connector = Arc::new(FakeConnector::new([connection]));
    let handle = Arc::new(RedisStreamsHandle::new(connector));
    let processor = Arc::new(TestProcessor::new());
    let runtime = RedisDeliveryRuntime::new(handle, Arc::clone(&processor), runtime_config(1, 1));
    let (_stop, stopped) = watch::channel(false);
    let task = tokio::spawn(runtime.run(stopped));
    processor.wait_started(1).await;

    task.abort();
    task.await.expect_err("cancelled runtime owner");
    tokio::task::yield_now().await;
    assert_eq!(processor.active.load(Ordering::Acquire), 0);
    assert_eq!(processor.completed.load(Ordering::Acquire), 0);
}

#[test]
fn deployed_intake_bounds_fail_closed() {
    assert!(RedisDeliveryIntakeConfig::new(1, 1, 100, 60_000, 100).is_ok());
    assert!(RedisDeliveryIntakeConfig::new(128, 512, 30_000, 86_400_000, 10_000).is_ok());
    assert!(RedisDeliveryIntakeConfig::new(0, 1, 100, 60_000, 100).is_err());
    assert!(RedisDeliveryIntakeConfig::new(2, 1, 100, 60_000, 100).is_err());
    assert!(RedisDeliveryIntakeConfig::new(1, 513, 100, 60_000, 100).is_err());
    assert!(RedisDeliveryIntakeConfig::new(1, 1, 99, 60_000, 100).is_err());
    assert!(RedisDeliveryIntakeConfig::new(1, 1, 100, 59_999, 100).is_err());
    assert!(RedisDeliveryIntakeConfig::new(1, 1, 100, 60_000, 99).is_err());
    let intake =
        RedisDeliveryIntakeConfig::new(1, 1, 100, 60_000, 100).expect("valid bounded intake");
    assert!(RedisDeliveryRuntimeConfig::new(intake, 100, 1_000).is_ok());
    assert!(RedisDeliveryRuntimeConfig::new(intake, 60_000, 300_000).is_ok());
    assert!(RedisDeliveryRuntimeConfig::new(intake, 99, 1_000).is_err());
    assert!(RedisDeliveryRuntimeConfig::new(intake, 100, 999).is_err());
}

#[tokio::test(start_paused = true)]
async fn own_pending_revisits_deferred_delivery_after_the_existing_claim_lease_expires() {
    let connection = Arc::new(FakeConnection::new(2));
    for _ in 0..=6 {
        connection.push_pending(Ok(RedisOwnedPendingPage {
            next_start_id: "0-0".to_owned(),
            deliveries: vec![delivery("1-0")],
        }));
    }
    let intake = intake(
        Arc::clone(&connection),
        RedisDeliveryIntakeConfig::new(1, 1, 30_000, 60_000, 5_000).unwrap(),
    );
    let started = tokio::time::Instant::now();
    let lease_end = started + Duration::from_secs(30);
    let mut dispatched = 0;
    for _ in 0..=6 {
        // Model the existing Main fence: a live old claim defers without ACK.
        let batch = intake.next_batch().await.expect("same consumer redelivery");
        assert_eq!(batch.len(), 1);
        assert_eq!(batch[0].entry_id(), "1-0");
        if tokio::time::Instant::now() >= lease_end {
            dispatched += 1;
        }
        drop(batch);
        assert_eq!(intake.owned_count(), 0);
        if dispatched == 1 {
            break;
        }
        tokio::time::advance(Duration::from_secs(5)).await;
    }
    assert_eq!(dispatched, 1);
    assert_eq!(connection.retirement_calls.load(Ordering::Acquire), 0);
    assert_eq!(
        tokio::time::Instant::now() - started,
        Duration::from_secs(30)
    );
    assert!(
        connection
            .operations()
            .iter()
            .all(|operation| !matches!(operation, FakeOperation::Close))
    );
}

#[tokio::test(start_paused = true)]
async fn own_pending_pages_preserve_new_and_cross_consumer_intake_fairness() {
    let connection = Arc::new(FakeConnection::new(1));
    for id in ["1-0", "2-0"] {
        connection.push_pending(Ok(RedisOwnedPendingPage {
            next_start_id: id.to_owned(),
            deliveries: vec![delivery(id)],
        }));
    }
    connection.push_read(Ok(vec![delivery("3-0")]));
    connection.push_read(Ok(vec![delivery("4-0")]));
    connection.push_reclaim(Ok(RedisReclaimPage {
        next_start_id: "0-0".to_owned(),
        deliveries: vec![delivery("5-0")],
    }));
    let intake = intake(Arc::clone(&connection), config(1, 1));
    for expected in ["1-0", "3-0"] {
        let batch = intake.next_batch().await.unwrap();
        assert_eq!(batch[0].entry_id(), expected);
        drop(batch);
    }
    tokio::time::advance(Duration::from_millis(100)).await;
    for expected in ["2-0", "5-0", "4-0"] {
        let batch = intake.next_batch().await.unwrap();
        assert_eq!(batch[0].entry_id(), expected);
        drop(batch);
    }
    let operations = connection.operations();
    assert!(
        matches!(&operations[0], FakeOperation::Pending { count: 1, start_id } if start_id == "0-0")
    );
    assert!(matches!(&operations[1], FakeOperation::Read { .. }));
    assert!(
        matches!(&operations[2], FakeOperation::Pending { count: 1, start_id } if start_id == "1-0")
    );
    assert!(matches!(
        &operations[3],
        FakeOperation::Reclaim {
            min_idle_millis: 60_000,
            ..
        }
    ));
    assert!(matches!(&operations[4], FakeOperation::Read { .. }));
}

#[tokio::test(start_paused = true)]
async fn own_pending_does_not_dispatch_an_active_local_delivery_twice() {
    let connection = Arc::new(FakeConnection::new(2));
    for _ in 0..2 {
        connection.push_pending(Ok(RedisOwnedPendingPage {
            next_start_id: "0-0".to_owned(),
            deliveries: vec![delivery("1-0")],
        }));
    }
    let intake = intake(Arc::clone(&connection), config(1, 1));
    let active = intake.next_batch().await.unwrap();
    tokio::time::advance(Duration::from_millis(100)).await;
    assert!(intake.next_batch().await.unwrap().is_empty());
    assert_eq!(intake.owned_count(), 1);
    assert!(matches!(
        connection.operations().last(),
        Some(FakeOperation::Pending { count: 1, .. })
    ));
    drop(active);
    assert_eq!(intake.owned_count(), 0);
}

#[tokio::test]
async fn own_pending_waits_for_existing_capacity_and_stop_cancels_the_wait() {
    let connection = Arc::new(FakeConnection::new(2));
    connection.push_pending(Ok(RedisOwnedPendingPage {
        next_start_id: "2-0".to_owned(),
        deliveries: vec![delivery("1-0"), delivery("2-0")],
    }));
    let intake = intake(Arc::clone(&connection), config(1, 1));
    let active = intake.next_batch().await.unwrap();
    let waiting = intake.next_batch();
    tokio::pin!(waiting);
    assert!(
        tokio::time::timeout(Duration::from_millis(1), &mut waiting)
            .await
            .is_err()
    );
    assert_eq!(connection.operations().len(), 1);
    intake.close().await.unwrap();
    assert!(matches!(waiting.await, Err(error) if error.kind() == RedisStreamsErrorKind::Closed));
    drop(active);
    assert_eq!(intake.owned_count(), 0);
}
