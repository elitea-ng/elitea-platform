//! Bounded Redis Streams intake, reclaim and PEL-ownership heartbeat.
//!
//! Redis is only the durable command-delivery plane. This owner fairly waits
//! for new commands or reclaims abandoned pending entries, and keeps every
//! queued or actively processed entry heartbeating until its processing future
//! really completes. It never acknowledges or deletes a command; the exact
//! post-settlement retirement authority remains the only such path.

#![allow(dead_code)] // Production bootstrap remains capability-disabled.

use std::collections::{BTreeSet, HashSet};
use std::future::Future;
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use async_trait::async_trait;
use tokio::sync::{Mutex as AsyncMutex, mpsc, watch};
use tokio::sync::{OwnedSemaphorePermit, Semaphore, TryAcquireError};
use tokio::task::{Id as TaskId, JoinError, JoinSet};
use tokio::time::{Instant, MissedTickBehavior, interval_at};

use crate::transport::redis_commands::RedisCommandDelivery;
use crate::transport::redis_generation::{RedisStreamsConnector, RedisStreamsHandle};
use crate::transport::redis_streams::{RedisStreamsError, RedisStreamsErrorKind};

const MAX_DELIVERY_CONCURRENCY: usize = 128;
const MAX_QUEUE_CAPACITY: usize = 512;
const MIN_BLOCK_MILLIS: u64 = 100;
const MAX_BLOCK_MILLIS: u64 = 30_000;
const MIN_RECLAIM_IDLE_MILLIS: u64 = 60_000;
const MAX_RECLAIM_IDLE_MILLIS: u64 = 86_400_000;
const MIN_RECLAIM_INTERVAL_MILLIS: u64 = 100;
const MAX_RECLAIM_INTERVAL_MILLIS: u64 = 10_000;
const MIN_DEPENDENCY_RETRY_MILLIS: u64 = 100;
const MAX_DEPENDENCY_RETRY_MILLIS: u64 = 60_000;
const MIN_SHUTDOWN_TIMEOUT_MILLIS: u64 = 1_000;
const MAX_SHUTDOWN_TIMEOUT_MILLIS: u64 = 300_000;
const INITIAL_RECLAIM_CURSOR: &str = "0-0";

/// Deployed bounds for queued, active and pending-entry ownership.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct RedisDeliveryIntakeConfig {
    max_concurrency: usize,
    queue_capacity: usize,
    read_block_millis: u64,
    reclaim_idle_millis: u64,
    reclaim_interval: Duration,
}

impl RedisDeliveryIntakeConfig {
    pub(crate) fn new(
        max_concurrency: usize,
        queue_capacity: usize,
        read_block_millis: u64,
        reclaim_idle_millis: u64,
        reclaim_interval_millis: u64,
    ) -> Result<Self, RedisStreamsError> {
        if !(1..=MAX_DELIVERY_CONCURRENCY).contains(&max_concurrency)
            || !(1..=MAX_QUEUE_CAPACITY).contains(&queue_capacity)
            || queue_capacity < max_concurrency
            || !(MIN_BLOCK_MILLIS..=MAX_BLOCK_MILLIS).contains(&read_block_millis)
            || !(MIN_RECLAIM_IDLE_MILLIS..=MAX_RECLAIM_IDLE_MILLIS).contains(&reclaim_idle_millis)
            || !(MIN_RECLAIM_INTERVAL_MILLIS..=MAX_RECLAIM_INTERVAL_MILLIS)
                .contains(&reclaim_interval_millis)
            || queue_capacity.checked_add(max_concurrency).is_none()
        {
            return Err(RedisStreamsError::configuration(
                "the Redis delivery intake limits are malformed",
            ));
        }
        Ok(Self {
            max_concurrency,
            queue_capacity,
            read_block_millis,
            reclaim_idle_millis,
            reclaim_interval: Duration::from_millis(reclaim_interval_millis),
        })
    }

    const fn ownership_capacity(self) -> usize {
        self.queue_capacity + self.max_concurrency
    }

    const fn max_concurrency(self) -> usize {
        self.max_concurrency
    }

    const fn queue_capacity(self) -> usize {
        self.queue_capacity
    }

    const fn reclaim_interval(self) -> Duration {
        self.reclaim_interval
    }
}

/// Stop-aware task-group policy around the deployed intake bounds.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct RedisDeliveryRuntimeConfig {
    intake: RedisDeliveryIntakeConfig,
    dependency_retry: Duration,
    shutdown_timeout: Duration,
}

impl RedisDeliveryRuntimeConfig {
    pub(crate) fn new(
        intake: RedisDeliveryIntakeConfig,
        dependency_retry_millis: u64,
        shutdown_timeout_millis: u64,
    ) -> Result<Self, RedisStreamsError> {
        if !(MIN_DEPENDENCY_RETRY_MILLIS..=MAX_DEPENDENCY_RETRY_MILLIS)
            .contains(&dependency_retry_millis)
            || !(MIN_SHUTDOWN_TIMEOUT_MILLIS..=MAX_SHUTDOWN_TIMEOUT_MILLIS)
                .contains(&shutdown_timeout_millis)
        {
            return Err(RedisStreamsError::configuration(
                "the Redis delivery runtime limits are malformed",
            ));
        }
        Ok(Self {
            intake,
            dependency_retry: Duration::from_millis(dependency_retry_millis),
            shutdown_timeout: Duration::from_millis(shutdown_timeout_millis),
        })
    }
}

type DeliveryKey = (String, String);

/// One fetched PEL entry with its exact process-local ownership slot.
///
/// The value is deliberately non-cloneable. [`Self::process`] keeps the slot
/// and heartbeat identity alive for the full processing future, including a
/// supervisor handoff, and releases both only after that future ends.
pub(crate) struct OwnedRedisDelivery {
    delivery: Option<RedisCommandDelivery>,
    key: DeliveryKey,
    owned: Arc<Mutex<BTreeSet<DeliveryKey>>>,
    _permit: OwnedSemaphorePermit,
}

impl OwnedRedisDelivery {
    #[must_use]
    pub(crate) fn stream(&self) -> &str {
        &self.key.0
    }

    #[must_use]
    pub(crate) fn entry_id(&self) -> &str {
        &self.key.1
    }

    /// Run one processing future while the PEL heartbeat and capacity slot are
    /// retained. Dropping the caller future also drops this owner, allowing a
    /// later Redis reclaim rather than acknowledging an uncertain execution.
    pub(crate) async fn process<F, Fut, T>(mut self, processor: F) -> Result<T, RedisStreamsError>
    where
        F: FnOnce(RedisCommandDelivery) -> Fut,
        Fut: Future<Output = T>,
    {
        let delivery = self.delivery.take().ok_or_else(|| {
            RedisStreamsError::protocol("the Redis delivery owner is already consumed")
        })?;
        Ok(processor(delivery).await)
    }
}

impl Drop for OwnedRedisDelivery {
    fn drop(&mut self) {
        lock_owned(&self.owned).remove(&self.key);
    }
}

/// Stateful fair intake over one replaceable Redis generation owner.
pub(crate) struct RedisDeliveryIntake<C>
where
    C: RedisStreamsConnector,
{
    handle: Arc<RedisStreamsHandle<C>>,
    config: RedisDeliveryIntakeConfig,
    capacity: Arc<Semaphore>,
    owned: Arc<Mutex<BTreeSet<DeliveryKey>>>,
    schedule: AsyncMutex<RedisIntakeSchedule>,
}

struct RedisIntakeSchedule {
    reclaim_cursor: String,
    next_reclaim: Instant,
    owned_pending_cursor: String,
    next_owned_pending: Instant,
}

impl<C> RedisDeliveryIntake<C>
where
    C: RedisStreamsConnector,
{
    #[must_use]
    pub(crate) fn new(
        handle: Arc<RedisStreamsHandle<C>>,
        config: RedisDeliveryIntakeConfig,
    ) -> Self {
        Self {
            handle,
            config,
            capacity: Arc::new(Semaphore::new(config.ownership_capacity())),
            owned: Arc::new(Mutex::new(BTreeSet::new())),
            schedule: AsyncMutex::new(RedisIntakeSchedule {
                reclaim_cursor: INITIAL_RECLAIM_CURSOR.to_owned(),
                next_reclaim: Instant::now() + config.reclaim_interval,
                owned_pending_cursor: INITIAL_RECLAIM_CURSOR.to_owned(),
                next_owned_pending: Instant::now(),
            }),
        }
    }

    /// Await one capacity-bounded pending, new-read or reclaim turn.
    ///
    /// A retryable generation failure is never replayed here. The generation
    /// handle invalidates it; a later call explicitly connects a replacement
    /// before issuing a fresh operation.
    pub(crate) async fn next_batch(&self) -> Result<Vec<OwnedRedisDelivery>, RedisStreamsError> {
        self.handle.connect().await?;
        let batch_size =
            usize::try_from(self.handle.delivery_batch_size().await?).map_err(|_| {
                RedisStreamsError::resource_exhausted(
                    "the Redis delivery batch exceeds the local integer domain",
                )
            })?;
        let permits = self.reserve_capacity(batch_size).await?;
        let count = u64::try_from(permits.len()).map_err(|_| {
            RedisStreamsError::resource_exhausted(
                "the Redis delivery reservation exceeds the protocol domain",
            )
        })?;
        let mut schedule = self.schedule.lock().await;
        let now = Instant::now();
        if now >= schedule.next_owned_pending {
            let result = self
                .handle
                .read_owned_pending(count, schedule.owned_pending_cursor.clone())
                .await;
            // At most one own-PEL page per interval. A retry never steals an
            // entry from another consumer or changes Main's claim authority.
            schedule.next_owned_pending = Instant::now() + self.config.reclaim_interval;
            match result {
                Ok(page) => {
                    schedule.owned_pending_cursor = page.next_start_id;
                    if !page.deliveries.is_empty() {
                        return self.bind_deliveries(page.deliveries, permits);
                    }
                }
                Err(error) => {
                    INITIAL_RECLAIM_CURSOR.clone_into(&mut schedule.owned_pending_cursor);
                    return Err(error);
                }
            }
        }
        let now = Instant::now();
        let deliveries = if now >= schedule.next_reclaim {
            let result = self
                .handle
                .reclaim_page(
                    self.config.reclaim_idle_millis,
                    schedule.reclaim_cursor.clone(),
                    count,
                )
                .await;
            schedule.next_reclaim = Instant::now() + self.config.reclaim_interval;
            match result {
                Ok(page) => {
                    schedule.reclaim_cursor = page.next_start_id;
                    page.deliveries
                }
                Err(error) => {
                    INITIAL_RECLAIM_CURSOR.clone_into(&mut schedule.reclaim_cursor);
                    return Err(error);
                }
            }
        } else {
            let until_pending = schedule
                .next_owned_pending
                .min(schedule.next_reclaim)
                .saturating_duration_since(now);
            let block_millis = u64::try_from(until_pending.as_millis())
                .map_or(u64::MAX, |millis| millis)
                .clamp(1, self.config.read_block_millis);
            self.handle.read_new(count, block_millis).await?
        };
        self.bind_deliveries(deliveries, permits)
    }

    /// Refresh every queued or active PEL entry in deterministic bounded pages.
    pub(crate) async fn heartbeat_owned(&self) -> Result<usize, RedisStreamsError> {
        let entries: Vec<String> = lock_owned(&self.owned)
            .iter()
            .map(|(_, entry_id)| entry_id.clone())
            .collect();
        if entries.is_empty() {
            return Ok(0);
        }
        self.handle.connect().await?;
        let batch_size =
            usize::try_from(self.handle.delivery_batch_size().await?).map_err(|_| {
                RedisStreamsError::resource_exhausted(
                    "the Redis heartbeat batch exceeds the local integer domain",
                )
            })?;
        if batch_size == 0 {
            return Err(RedisStreamsError::configuration(
                "the Redis heartbeat batch is empty",
            ));
        }
        let mut refreshed = 0_usize;
        for batch in entries.chunks(batch_size) {
            refreshed = refreshed
                .checked_add(
                    self.handle
                        .heartbeat_owned_pending(batch.to_vec())
                        .await?
                        .len(),
                )
                .ok_or_else(|| {
                    RedisStreamsError::resource_exhausted(
                        "the Redis heartbeat response count is exhausted",
                    )
                })?;
        }
        Ok(refreshed)
    }

    #[must_use]
    pub(crate) fn owned_count(&self) -> usize {
        lock_owned(&self.owned).len()
    }

    pub(crate) async fn close(&self) -> Result<(), RedisStreamsError> {
        self.capacity.close();
        self.handle.close().await
    }

    async fn reserve_capacity(
        &self,
        maximum: usize,
    ) -> Result<Vec<OwnedSemaphorePermit>, RedisStreamsError> {
        if maximum == 0 {
            return Err(RedisStreamsError::configuration(
                "the Redis delivery batch is empty",
            ));
        }
        let first = Arc::clone(&self.capacity)
            .acquire_owned()
            .await
            .map_err(|_| RedisStreamsError::closed())?;
        let mut permits = Vec::with_capacity(maximum);
        permits.push(first);
        while permits.len() < maximum {
            match Arc::clone(&self.capacity).try_acquire_owned() {
                Ok(permit) => permits.push(permit),
                Err(TryAcquireError::NoPermits) => break,
                Err(TryAcquireError::Closed) => return Err(RedisStreamsError::closed()),
            }
        }
        Ok(permits)
    }

    fn bind_deliveries(
        &self,
        deliveries: Vec<RedisCommandDelivery>,
        permits: Vec<OwnedSemaphorePermit>,
    ) -> Result<Vec<OwnedRedisDelivery>, RedisStreamsError> {
        if deliveries.len() > permits.len() {
            return Err(RedisStreamsError::protocol(
                "Redis returned more deliveries than the reserved capacity",
            ));
        }
        let mut owned = lock_owned(&self.owned);
        let mut permits = permits.into_iter();
        let mut accepted = Vec::with_capacity(deliveries.len());
        for delivery in deliveries {
            let permit = permits.next().ok_or_else(|| {
                RedisStreamsError::protocol(
                    "the Redis delivery reservation does not match the response",
                )
            })?;
            let key = (delivery.stream().to_owned(), delivery.entry_id().to_owned());
            if !owned.insert(key.clone()) {
                continue;
            }
            accepted.push(OwnedRedisDelivery {
                delivery: Some(delivery),
                key,
                owned: Arc::clone(&self.owned),
                _permit: permit,
            });
        }
        Ok(accepted)
    }
}

/// Complete processing boundary for one fetched Redis delivery.
///
/// The implementation owns business routing and its result diagnostics. The
/// serve task group owns only Redis intake, local PEL lifetime and structured
/// shutdown; it never invents an ACK or interprets an agent disposition.
#[async_trait]
pub(crate) trait RedisDeliveryProcessor: Send + Sync + 'static {
    async fn process(&self, delivery: RedisCommandDelivery);
}

/// Stable, data-free Redis task-group failure.
pub(crate) enum RedisDeliveryRuntimeError {
    Transport(RedisStreamsError),
    TaskLost(&'static str),
    DrainTimeout(&'static str),
}

impl RedisDeliveryRuntimeError {
    #[must_use]
    pub(crate) const fn code(&self) -> &'static str {
        match self {
            Self::Transport(error) => error.code(),
            Self::TaskLost(_) => "redis_delivery.task_lost",
            Self::DrainTimeout(_) => "redis_delivery.drain_timeout",
        }
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        match self {
            Self::Transport(error) => error.retryable(),
            Self::TaskLost(_) | Self::DrainTimeout(_) => true,
        }
    }
}

impl std::fmt::Debug for RedisDeliveryRuntimeError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("RedisDeliveryRuntimeError")
            .field("code", &self.code())
            .field("retryable", &self.retryable())
            .finish_non_exhaustive()
    }
}

impl std::fmt::Display for RedisDeliveryRuntimeError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Transport(error) => error.fmt(formatter),
            Self::TaskLost(message) | Self::DrainTimeout(message) => formatter.write_str(message),
        }
    }
}

impl std::error::Error for RedisDeliveryRuntimeError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Transport(error) => Some(error),
            Self::TaskLost(_) | Self::DrainTimeout(_) => None,
        }
    }
}

impl From<RedisStreamsError> for RedisDeliveryRuntimeError {
    fn from(value: RedisStreamsError) -> Self {
        Self::Transport(value)
    }
}

/// One-shot structured Redis delivery task group.
///
/// Dropping the `run` future aborts its owned Tokio task set; queued and active
/// delivery owners are then dropped and remain in the Redis PEL for another
/// worker to reclaim. A normal Stop ends intake first, keeps heartbeat active
/// while existing workers drain, and never turns process shutdown into a
/// business cancellation or a Redis ACK.
pub(crate) struct RedisDeliveryRuntime<C, P>
where
    C: RedisStreamsConnector,
    P: RedisDeliveryProcessor,
{
    intake: Arc<RedisDeliveryIntake<C>>,
    processor: Arc<P>,
    config: RedisDeliveryRuntimeConfig,
}

impl<C, P> RedisDeliveryRuntime<C, P>
where
    C: RedisStreamsConnector,
    P: RedisDeliveryProcessor,
{
    #[must_use]
    pub(crate) fn new(
        handle: Arc<RedisStreamsHandle<C>>,
        processor: Arc<P>,
        config: RedisDeliveryRuntimeConfig,
    ) -> Self {
        Self {
            intake: Arc::new(RedisDeliveryIntake::new(handle, config.intake)),
            processor,
            config,
        }
    }

    /// Run intake, heartbeat and bounded workers until Stop or a fatal task.
    ///
    /// # Errors
    ///
    /// Returns a stable transport, task-loss or drain-timeout failure after
    /// stopping admission and attempting to drain already-owned deliveries.
    pub(crate) async fn run(
        self,
        mut stop: watch::Receiver<bool>,
    ) -> Result<(), RedisDeliveryRuntimeError> {
        if *stop.borrow() {
            return self.intake.close().await.map_err(Into::into);
        }
        let (sender, receiver) = mpsc::channel(self.config.intake.queue_capacity());
        let receiver = Arc::new(AsyncMutex::new(receiver));
        let (heartbeat_stop, heartbeat_stopped) = watch::channel(false);
        let mut tasks = JoinSet::new();
        let mut registry = RedisDeliveryTaskRegistry {
            workers: HashSet::with_capacity(self.config.intake.max_concurrency()),
            heartbeat: None,
        };
        for worker_index in 0..self.config.intake.max_concurrency() {
            let task = tasks.spawn(delivery_worker(
                Arc::clone(&receiver),
                Arc::clone(&self.processor),
                worker_index,
            ));
            registry.workers.insert(task.id());
        }
        tracing::info!(
            event = "redis_delivery_workers_spawned",
            worker_count = registry.workers.len(),
            queue_capacity = self.config.intake.queue_capacity(),
        );
        let heartbeat = tasks.spawn(heartbeat_worker(
            Arc::clone(&self.intake),
            heartbeat_stopped,
            self.config.intake.reclaim_interval(),
        ));
        registry.heartbeat = Some(heartbeat.id());

        let failure = self
            .drive_intake(&mut stop, &sender, &mut tasks, &mut registry)
            .await;
        drop(sender);
        let drain = drain_workers(&mut tasks, &mut registry, heartbeat_stop, failure);
        let result = match tokio::time::timeout(self.config.shutdown_timeout, drain).await {
            Ok(result) => result,
            Err(_) => Err(RedisDeliveryRuntimeError::DrainTimeout(
                "the Redis delivery workers did not drain before shutdown",
            )),
        };
        let close = self.intake.close().await.map_err(Into::into);
        combine_runtime_results(result, close)
    }

    async fn drive_intake(
        &self,
        stop: &mut watch::Receiver<bool>,
        sender: &mpsc::Sender<OwnedRedisDelivery>,
        tasks: &mut JoinSet<RedisDeliveryTaskExit>,
        registry: &mut RedisDeliveryTaskRegistry,
    ) -> Option<RedisDeliveryRuntimeError> {
        loop {
            tokio::select! {
                biased;
                () = wait_for_stop(stop) => return None,
                task = tasks.join_next_with_id() => {
                    return Some(registry.classify(task).failure());
                },
                batch = self.intake.next_batch() => {
                    match batch {
                        Ok(deliveries) => {
                            if !deliveries.is_empty() {
                                tracing::info!(
                                    event = "redis_delivery_batch_received",
                                    delivery_count = deliveries.len(),
                                    owned_delivery_count = self.intake.owned_count(),
                                );
                            }
                            if !enqueue_batch(deliveries, sender, stop).await {
                                return None;
                            }
                        }
                        Err(error) if !redis_intake_failure_is_fatal(&error) => {
                            tracing::warn!(
                                event = "redis_intake_retry",
                                error_code = error.code(),
                                retryable = true,
                            );
                            if !wait_for_retry_or_stop(stop, self.config.dependency_retry).await {
                                return None;
                            }
                        }
                        Err(error) => return Some(error.into()),
                    }
                }
            }
        }
    }
}

enum RedisDeliveryTaskExit {
    Worker(Result<(), RedisStreamsError>),
    Heartbeat(Result<(), RedisStreamsError>),
}

struct RedisDeliveryTaskRegistry {
    workers: HashSet<TaskId>,
    heartbeat: Option<TaskId>,
}

enum JoinedRedisDeliveryTask {
    Worker(Result<(), RedisStreamsError>),
    Heartbeat(Result<(), RedisStreamsError>),
    Lost,
}

impl JoinedRedisDeliveryTask {
    fn failure(self) -> RedisDeliveryRuntimeError {
        match self {
            Self::Worker(Err(error)) | Self::Heartbeat(Err(error)) => error.into(),
            Self::Worker(Ok(())) | Self::Heartbeat(Ok(())) | Self::Lost => {
                RedisDeliveryRuntimeError::TaskLost(
                    "a Redis delivery background task ended unexpectedly",
                )
            }
        }
    }
}

impl RedisDeliveryTaskRegistry {
    fn classify(
        &mut self,
        result: Option<Result<(TaskId, RedisDeliveryTaskExit), JoinError>>,
    ) -> JoinedRedisDeliveryTask {
        let Some(result) = result else {
            return JoinedRedisDeliveryTask::Lost;
        };
        match result {
            Ok((id, RedisDeliveryTaskExit::Worker(result))) if self.workers.remove(&id) => {
                JoinedRedisDeliveryTask::Worker(result)
            }
            Ok((id, RedisDeliveryTaskExit::Heartbeat(result))) if self.heartbeat == Some(id) => {
                self.heartbeat = None;
                JoinedRedisDeliveryTask::Heartbeat(result)
            }
            Ok(_) => JoinedRedisDeliveryTask::Lost,
            Err(error) => {
                let id = error.id();
                self.workers.remove(&id);
                if self.heartbeat == Some(id) {
                    self.heartbeat = None;
                }
                JoinedRedisDeliveryTask::Lost
            }
        }
    }
}

async fn delivery_worker<P>(
    receiver: Arc<AsyncMutex<mpsc::Receiver<OwnedRedisDelivery>>>,
    processor: Arc<P>,
    worker_index: usize,
) -> RedisDeliveryTaskExit
where
    P: RedisDeliveryProcessor,
{
    tracing::info!(event = "redis_delivery_worker_started", worker_index);
    loop {
        let delivery = receiver.lock().await.recv().await;
        let Some(delivery) = delivery else {
            tracing::info!(event = "redis_delivery_worker_stopped", worker_index);
            return RedisDeliveryTaskExit::Worker(Ok(()));
        };
        let redis_stream = delivery.stream().to_owned();
        let redis_entry_id = delivery.entry_id().to_owned();
        tracing::info!(
            event = "redis_delivery_worker_received",
            worker_index,
            redis_stream,
            redis_entry_id,
        );
        let processor = Arc::clone(&processor);
        if let Err(error) = delivery
            .process(|delivery| async move { processor.process(delivery).await })
            .await
        {
            return RedisDeliveryTaskExit::Worker(Err(error));
        }
        tracing::info!(
            event = "redis_delivery_worker_completed",
            worker_index,
            redis_stream,
            redis_entry_id,
        );
    }
}

async fn heartbeat_worker<C>(
    intake: Arc<RedisDeliveryIntake<C>>,
    mut stop: watch::Receiver<bool>,
    cadence: Duration,
) -> RedisDeliveryTaskExit
where
    C: RedisStreamsConnector,
{
    let mut ticker = interval_at(Instant::now() + cadence, cadence);
    ticker.set_missed_tick_behavior(MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            biased;
            () = wait_for_stop(&mut stop) => {
                return RedisDeliveryTaskExit::Heartbeat(Ok(()));
            }
            _ = ticker.tick() => {
                match intake.heartbeat_owned().await {
                    Ok(_) => {}
                    Err(error) if !redis_intake_failure_is_fatal(&error) => {
                        tracing::warn!(
                            event = "redis_heartbeat_retry",
                            error_code = error.code(),
                            retryable = true,
                        );
                    }
                    Err(error) => return RedisDeliveryTaskExit::Heartbeat(Err(error)),
                }
            }
        }
    }
}

async fn enqueue_batch(
    deliveries: Vec<OwnedRedisDelivery>,
    sender: &mpsc::Sender<OwnedRedisDelivery>,
    stop: &mut watch::Receiver<bool>,
) -> bool {
    for delivery in deliveries {
        let redis_stream = delivery.stream().to_owned();
        let redis_entry_id = delivery.entry_id().to_owned();
        tokio::select! {
            biased;
            () = wait_for_stop(stop) => return false,
            result = sender.send(delivery) => {
                if result.is_err() {
                    return false;
                }
                tracing::info!(
                    event = "redis_delivery_enqueued",
                    redis_stream,
                    redis_entry_id,
                    queue_remaining_capacity = sender.capacity(),
                );
            }
        }
    }
    true
}

async fn wait_for_retry_or_stop(stop: &mut watch::Receiver<bool>, retry: Duration) -> bool {
    tokio::select! {
        biased;
        () = wait_for_stop(stop) => false,
        () = tokio::time::sleep(retry) => true,
    }
}

async fn wait_for_stop(stop: &mut watch::Receiver<bool>) {
    loop {
        if *stop.borrow() || stop.changed().await.is_err() {
            return;
        }
    }
}

async fn drain_workers(
    tasks: &mut JoinSet<RedisDeliveryTaskExit>,
    registry: &mut RedisDeliveryTaskRegistry,
    heartbeat_stop: watch::Sender<bool>,
    mut failure: Option<RedisDeliveryRuntimeError>,
) -> Result<(), RedisDeliveryRuntimeError> {
    while !registry.workers.is_empty() {
        match registry.classify(tasks.join_next_with_id().await) {
            JoinedRedisDeliveryTask::Worker(Ok(())) => {}
            JoinedRedisDeliveryTask::Worker(Err(error))
            | JoinedRedisDeliveryTask::Heartbeat(Err(error)) => {
                failure.get_or_insert_with(|| error.into());
            }
            JoinedRedisDeliveryTask::Heartbeat(Ok(())) => {
                failure.get_or_insert(RedisDeliveryRuntimeError::TaskLost(
                    "the Redis heartbeat stopped before delivery drain completed",
                ));
            }
            JoinedRedisDeliveryTask::Lost => {
                failure.get_or_insert(RedisDeliveryRuntimeError::TaskLost(
                    "the Redis delivery task group ended before drain completed",
                ));
                break;
            }
        }
    }
    let _ignored = heartbeat_stop.send(true);
    while !tasks.is_empty() {
        match registry.classify(tasks.join_next_with_id().await) {
            JoinedRedisDeliveryTask::Heartbeat(Ok(()))
            | JoinedRedisDeliveryTask::Worker(Ok(())) => {}
            JoinedRedisDeliveryTask::Worker(Err(error))
            | JoinedRedisDeliveryTask::Heartbeat(Err(error)) => {
                failure.get_or_insert_with(|| error.into());
            }
            JoinedRedisDeliveryTask::Lost => {
                failure.get_or_insert(RedisDeliveryRuntimeError::TaskLost(
                    "a Redis delivery background task was lost during shutdown",
                ));
            }
        }
    }
    match failure {
        Some(error) => Err(error),
        None => Ok(()),
    }
}

fn combine_runtime_results(
    primary: Result<(), RedisDeliveryRuntimeError>,
    close: Result<(), RedisDeliveryRuntimeError>,
) -> Result<(), RedisDeliveryRuntimeError> {
    match (primary, close) {
        (Ok(()), Ok(())) => Ok(()),
        (Err(error), Ok(())) | (Ok(()), Err(error)) => Err(error),
        (Err(primary), Err(close_error)) => {
            tracing::warn!(
                event = "redis_close_after_failure",
                error_code = close_error.code(),
                retryable = close_error.retryable(),
            );
            Err(primary)
        }
    }
}

fn lock_owned(owned: &Mutex<BTreeSet<DeliveryKey>>) -> MutexGuard<'_, BTreeSet<DeliveryKey>> {
    match owned.lock() {
        Ok(guard) => guard,
        Err(poisoned) => poisoned.into_inner(),
    }
}

#[must_use]
pub(crate) const fn redis_intake_failure_is_fatal(error: &RedisStreamsError) -> bool {
    !matches!(
        error.kind(),
        RedisStreamsErrorKind::DependencyUnavailable | RedisStreamsErrorKind::Timeout
    )
}
