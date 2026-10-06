use std::{
    collections::VecDeque,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering},
    },
};

use async_trait::async_trait;
use tokio::sync::Notify;

use super::redis_commands::{
    RedisRetirementClient, RedisRetirementClientError, RedisRetirementRequest,
    RedisRetirementResponse,
};
use super::redis_generation::{
    RedisGenerationFuture, RedisStreamsConnection, RedisStreamsConnector, RedisStreamsHandle,
};
use super::redis_streams::{
    RedisOwnedPendingPage, RedisReclaimPage, RedisStreamsError, RedisStreamsErrorKind,
};

enum FakeOutcome {
    Success,
    Unavailable,
}

struct FakeConnection {
    number: u64,
    read_outcomes: Mutex<VecDeque<FakeOutcome>>,
    heartbeat_outcomes: Mutex<VecDeque<FakeOutcome>>,
    read_started: Notify,
    read_release: Notify,
    gate_read: AtomicBool,
    close_calls: AtomicUsize,
    retirement_calls: AtomicUsize,
}

impl FakeConnection {
    fn new(number: u64) -> Self {
        Self {
            number,
            read_outcomes: Mutex::new(VecDeque::new()),
            heartbeat_outcomes: Mutex::new(VecDeque::new()),
            read_started: Notify::new(),
            read_release: Notify::new(),
            gate_read: AtomicBool::new(false),
            close_calls: AtomicUsize::new(0),
            retirement_calls: AtomicUsize::new(0),
        }
    }

    fn fail_next_read(&self) {
        self.read_outcomes
            .lock()
            .expect("read outcomes")
            .push_back(FakeOutcome::Unavailable);
    }

    fn fail_next_heartbeat(&self) {
        self.heartbeat_outcomes
            .lock()
            .expect("heartbeat outcomes")
            .push_back(FakeOutcome::Unavailable);
    }

    fn gate_next_read(&self) {
        self.gate_read.store(true, Ordering::Release);
    }

    async fn read_started(&self) {
        self.read_started.notified().await;
    }

    fn release_read(&self) {
        self.read_release.notify_one();
    }

    fn outcome(
        outcomes: &Mutex<VecDeque<FakeOutcome>>,
    ) -> Result<Vec<super::redis_commands::RedisCommandDelivery>, RedisStreamsError> {
        match outcomes
            .lock()
            .expect("fake outcomes")
            .pop_front()
            .unwrap_or(FakeOutcome::Success)
        {
            FakeOutcome::Success => Ok(Vec::new()),
            FakeOutcome::Unavailable => Err(RedisStreamsError::unavailable(
                "the fake Redis generation is unavailable",
            )),
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
        4
    }

    fn read_new(
        self: Arc<Self>,
        _count: u64,
        _block_millis: u64,
    ) -> RedisGenerationFuture<
        Result<Vec<super::redis_commands::RedisCommandDelivery>, RedisStreamsError>,
    > {
        Box::pin(async move {
            if self.gate_read.swap(false, Ordering::AcqRel) {
                self.read_started.notify_one();
                self.read_release.notified().await;
            }
            Self::outcome(&self.read_outcomes)
        })
    }

    fn read_owned_pending(
        self: Arc<Self>,
        _count: u64,
        _start_id: String,
    ) -> RedisGenerationFuture<Result<RedisOwnedPendingPage, RedisStreamsError>> {
        Box::pin(async move {
            Self::outcome(&self.read_outcomes).map(|deliveries| RedisOwnedPendingPage {
                next_start_id: "0-0".to_owned(),
                deliveries,
            })
        })
    }

    fn reclaim_page(
        self: Arc<Self>,
        _min_idle_millis: u64,
        start_id: String,
        _count: u64,
    ) -> RedisGenerationFuture<Result<RedisReclaimPage, RedisStreamsError>> {
        Box::pin(async move {
            Ok(RedisReclaimPage {
                next_start_id: start_id,
                deliveries: Vec::new(),
            })
        })
    }

    fn heartbeat_owned_pending(
        self: Arc<Self>,
        _entry_ids: Vec<String>,
    ) -> RedisGenerationFuture<Result<Vec<String>, RedisStreamsError>> {
        Box::pin(async move { Self::outcome(&self.heartbeat_outcomes).map(|_| Vec::new()) })
    }

    fn close(self: Arc<Self>) -> RedisGenerationFuture<Result<(), RedisStreamsError>> {
        Box::pin(async move {
            self.close_calls.fetch_add(1, Ordering::AcqRel);
            Ok(())
        })
    }
}

struct FakeConnector {
    attempts: AtomicUsize,
    next_number: AtomicU64,
    connections: Mutex<Vec<Arc<FakeConnection>>>,
    gate_first: AtomicBool,
    first_started: Notify,
    first_release: Notify,
}

impl FakeConnector {
    fn new() -> Self {
        Self {
            attempts: AtomicUsize::new(0),
            next_number: AtomicU64::new(1),
            connections: Mutex::new(Vec::new()),
            gate_first: AtomicBool::new(false),
            first_started: Notify::new(),
            first_release: Notify::new(),
        }
    }

    fn gate_first_connect(&self) {
        self.gate_first.store(true, Ordering::Release);
    }

    async fn first_connect_started(&self) {
        self.first_started.notified().await;
    }

    fn connection(&self, index: usize) -> Arc<FakeConnection> {
        Arc::clone(
            self.connections
                .lock()
                .expect("connections")
                .get(index)
                .expect("connected generation"),
        )
    }
}

impl RedisStreamsConnector for FakeConnector {
    type Connection = FakeConnection;

    fn connect(
        self: Arc<Self>,
    ) -> RedisGenerationFuture<Result<Arc<Self::Connection>, RedisStreamsError>> {
        Box::pin(async move {
            let attempt = self.attempts.fetch_add(1, Ordering::AcqRel) + 1;
            if attempt == 1 && self.gate_first.load(Ordering::Acquire) {
                self.first_started.notify_one();
                self.first_release.notified().await;
            }
            let number = self.next_number.fetch_add(1, Ordering::AcqRel);
            let connection = Arc::new(FakeConnection::new(number));
            self.connections
                .lock()
                .expect("connections")
                .push(Arc::clone(&connection));
            Ok(connection)
        })
    }
}

#[tokio::test]
async fn concurrent_connection_requests_create_exactly_one_generation() {
    let connector = Arc::new(FakeConnector::new());
    let handle = Arc::new(RedisStreamsHandle::new(Arc::clone(&connector)));

    let (first, second) = tokio::join!(handle.connect(), handle.connect());

    assert_eq!(first.expect("first connection"), 1);
    assert_eq!(second.expect("shared connection"), 1);
    assert_eq!(connector.attempts.load(Ordering::Acquire), 1);
    assert_eq!(handle.delivery_batch_size().await.expect("batch size"), 4);
}

#[tokio::test]
async fn retryable_operation_failure_requires_an_explicit_new_generation() {
    let connector = Arc::new(FakeConnector::new());
    let handle = RedisStreamsHandle::new(Arc::clone(&connector));
    assert_eq!(handle.connect().await.expect("first generation"), 1);
    connector.connection(0).fail_next_read();

    let Err(error) = handle.read_new(1, 1).await else {
        panic!("the read unexpectedly succeeded");
    };

    assert_eq!(error.kind(), RedisStreamsErrorKind::DependencyUnavailable);
    assert_eq!(
        handle
            .delivery_batch_size()
            .await
            .expect_err("generation was invalidated")
            .kind(),
        RedisStreamsErrorKind::DependencyUnavailable
    );
    assert_eq!(handle.connect().await.expect("replacement generation"), 2);
    assert_eq!(connector.attempts.load(Ordering::Acquire), 2);
}

#[tokio::test]
async fn a_late_old_generation_failure_cannot_evict_its_replacement() {
    let connector = Arc::new(FakeConnector::new());
    let handle = Arc::new(RedisStreamsHandle::new(Arc::clone(&connector)));
    assert_eq!(handle.connect().await.expect("first generation"), 1);
    let first = connector.connection(0);
    first.gate_next_read();
    first.fail_next_read();
    first.fail_next_heartbeat();

    let read_handle = Arc::clone(&handle);
    let read = tokio::spawn(async move { read_handle.read_new(1, 1).await });
    first.read_started().await;
    handle
        .heartbeat_owned_pending(vec!["1-0".to_owned()])
        .await
        .expect_err("heartbeat invalidates first generation");
    assert_eq!(handle.connect().await.expect("replacement generation"), 2);

    first.release_read();
    assert!(matches!(
        read.await.expect("read task"),
        Err(ref error) if error.kind() == RedisStreamsErrorKind::DependencyUnavailable
    ));

    assert_eq!(handle.connect().await.expect("replacement remains"), 2);
    assert_eq!(connector.connection(1).number, 2);
    assert_eq!(connector.attempts.load(Ordering::Acquire), 2);
}

#[tokio::test]
async fn cancelled_connection_attempt_does_not_strand_replacement_ownership() {
    let connector = Arc::new(FakeConnector::new());
    connector.gate_first_connect();
    let handle = Arc::new(RedisStreamsHandle::new(Arc::clone(&connector)));
    let first_handle = Arc::clone(&handle);
    let first = tokio::spawn(async move { first_handle.connect().await });
    connector.first_connect_started().await;

    first.abort();
    first.await.expect_err("cancelled first attempt");
    assert_eq!(handle.connect().await.expect("second attempt"), 1);
    assert_eq!(connector.attempts.load(Ordering::Acquire), 2);
}

#[tokio::test]
async fn close_is_one_way_and_observes_the_current_generation_once() {
    let connector = Arc::new(FakeConnector::new());
    let handle = RedisStreamsHandle::new(Arc::clone(&connector));
    handle.connect().await.expect("connected generation");
    let connection = connector.connection(0);

    handle.close().await.expect("first close");
    handle.close().await.expect("idempotent close");

    assert_eq!(connection.close_calls.load(Ordering::Acquire), 1);
    assert_eq!(
        handle.connect().await.expect_err("closed handle").kind(),
        RedisStreamsErrorKind::Closed
    );
}

#[tokio::test]
async fn owned_pending_failure_invalidates_the_generation_without_implicit_replay() {
    let connector = Arc::new(FakeConnector::new());
    let handle = RedisStreamsHandle::new(Arc::clone(&connector));
    handle.connect().await.unwrap();
    connector.connection(0).fail_next_read();
    assert!(
        handle
            .read_owned_pending(1, "0-0".to_owned())
            .await
            .is_err()
    );
    assert_eq!(connector.attempts.load(Ordering::Acquire), 1);
    assert_eq!(handle.connect().await.unwrap(), 2);
    let page = handle
        .read_owned_pending(1, "0-0".to_owned())
        .await
        .unwrap();
    assert!(page.deliveries.is_empty());
    assert_eq!(page.next_start_id, "0-0");
}
