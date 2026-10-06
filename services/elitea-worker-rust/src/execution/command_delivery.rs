//! Bounded `JetStream` command intake, `+WPI` heartbeat and disposition.
//!
//! NATS `JetStream` is only the durable command-delivery plane
//! (docs/runtime-command-bus.md). This owner pulls at most as many messages
//! as there are free delivery permits (semaphore before pull), keeps every
//! queued or actively processed message heartbeating until its processing
//! future really ends, and then applies the contract's disposition:
//!
//! | Processing result | Action |
//! | --- | --- |
//! | retired (the settlement receipt led to a confirmed double ack) | nothing |
//! | processed but not retired (retry later, active lease elsewhere, recovery pending, a failure) | nak with the retry delay |
//! | poison (decode/signature failure, subject mismatch, unsupported command) | nak 24h + dead-letter record + ERROR log + counter |
//!
//! It never acknowledges a command itself and never terminates one: the exact
//! post-settlement retirement authority remains the only ack path.

#![allow(dead_code)] // Production bootstrap remains capability-disabled.

use std::collections::{BTreeMap, HashSet};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use async_trait::async_trait;
use tokio::sync::{Mutex as AsyncMutex, mpsc, watch};
use tokio::sync::{OwnedSemaphorePermit, Semaphore, TryAcquireError};
use tokio::task::{Id as TaskId, JoinError, JoinSet};
use tokio::time::{Instant, MissedTickBehavior, interval_at};

use crate::transport::command_bus::{
    CommandDelivery, DeadLetterRecord, DecodedDelivery, DeliveryCoordinates, DeliverySettlement,
    DeliveryVerdict, MAX_REQUEST_BATCH, MAX_REQUEST_EXPIRES, POISON_DELAY, PoisonDelivery,
    PoisonReason,
};
use crate::transport::nats_jetstream::{
    CommandBusConnection, NatsJetStreamError, NatsJetStreamErrorKind,
};

const MAX_DELIVERY_CONCURRENCY: usize = 128;
const MAX_QUEUE_CAPACITY: usize = 512;
const MIN_FETCH_EXPIRES: Duration = Duration::from_millis(100);
const MIN_IN_PROGRESS_INTERVAL: Duration = Duration::from_secs(1);
// At most a quarter of AckWait (60s), so three lost heartbeats still do not
// redeliver.
const MAX_IN_PROGRESS_INTERVAL: Duration = Duration::from_secs(15);
const MIN_RETRY_DELAY: Duration = Duration::from_secs(1);
const MAX_RETRY_DELAY: Duration = Duration::from_mins(5);
const MIN_DEPENDENCY_RETRY_MILLIS: u64 = 100;
const MAX_DEPENDENCY_RETRY_MILLIS: u64 = 60_000;
const MIN_SHUTDOWN_TIMEOUT_MILLIS: u64 = 1_000;
const MAX_SHUTDOWN_TIMEOUT_MILLIS: u64 = 300_000;

/// Process-wide count of dead-lettered deliveries. The crate has no metrics
/// exporter; the count is carried on every `worker_command.dead_lettered`
/// ERROR event and readable by tests.
static DEAD_LETTERED_TOTAL: AtomicU64 = AtomicU64::new(0);

#[must_use]
pub(crate) fn dead_lettered_total() -> u64 {
    DEAD_LETTERED_TOTAL.load(Ordering::Relaxed)
}

/// Deployed bounds for queued, active and owned deliveries.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct CommandDeliveryIntakeConfig {
    max_concurrency: usize,
    queue_capacity: usize,
    fetch_batch: usize,
    fetch_expires: Duration,
    in_progress_interval: Duration,
    retry_delay: Duration,
}

impl CommandDeliveryIntakeConfig {
    pub(crate) fn new(
        max_concurrency: usize,
        queue_capacity: usize,
        fetch_batch: usize,
        fetch_expires_millis: u64,
        in_progress_interval_millis: u64,
        retry_delay_millis: u64,
    ) -> Result<Self, NatsJetStreamError> {
        let fetch_expires = Duration::from_millis(fetch_expires_millis);
        let in_progress_interval = Duration::from_millis(in_progress_interval_millis);
        let retry_delay = Duration::from_millis(retry_delay_millis);
        if !(1..=MAX_DELIVERY_CONCURRENCY).contains(&max_concurrency)
            || !(1..=MAX_QUEUE_CAPACITY).contains(&queue_capacity)
            || queue_capacity < max_concurrency
            || !(1..=MAX_REQUEST_BATCH).contains(&fetch_batch)
            || !(MIN_FETCH_EXPIRES..=MAX_REQUEST_EXPIRES).contains(&fetch_expires)
            || !(MIN_IN_PROGRESS_INTERVAL..=MAX_IN_PROGRESS_INTERVAL)
                .contains(&in_progress_interval)
            || !(MIN_RETRY_DELAY..=MAX_RETRY_DELAY).contains(&retry_delay)
            || queue_capacity.checked_add(max_concurrency).is_none()
        {
            return Err(NatsJetStreamError::configuration(
                "the command delivery intake limits are malformed",
            ));
        }
        Ok(Self {
            max_concurrency,
            queue_capacity,
            fetch_batch,
            fetch_expires,
            in_progress_interval,
            retry_delay,
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

    const fn in_progress_interval(self) -> Duration {
        self.in_progress_interval
    }

    pub(crate) const fn retry_delay(self) -> Duration {
        self.retry_delay
    }
}

/// Stop-aware task-group policy around the deployed intake bounds.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct CommandDeliveryRuntimeConfig {
    intake: CommandDeliveryIntakeConfig,
    dependency_retry: Duration,
    shutdown_timeout: Duration,
}

impl CommandDeliveryRuntimeConfig {
    pub(crate) fn new(
        intake: CommandDeliveryIntakeConfig,
        dependency_retry_millis: u64,
        shutdown_timeout_millis: u64,
    ) -> Result<Self, NatsJetStreamError> {
        if !(MIN_DEPENDENCY_RETRY_MILLIS..=MAX_DEPENDENCY_RETRY_MILLIS)
            .contains(&dependency_retry_millis)
            || !(MIN_SHUTDOWN_TIMEOUT_MILLIS..=MAX_SHUTDOWN_TIMEOUT_MILLIS)
                .contains(&shutdown_timeout_millis)
        {
            return Err(NatsJetStreamError::configuration(
                "the command delivery runtime limits are malformed",
            ));
        }
        Ok(Self {
            intake,
            dependency_retry: Duration::from_millis(dependency_retry_millis),
            shutdown_timeout: Duration::from_millis(shutdown_timeout_millis),
        })
    }
}

/// (stream, stream sequence): one message, whatever its delivery count.
type DeliveryKey = (String, u64);
type OwnedReplies = Arc<Mutex<BTreeMap<DeliveryKey, String>>>;

/// One fetched message with its exact process-local ownership slot.
///
/// The value is deliberately non-cloneable. [`Self::process`] keeps the slot
/// and heartbeat identity alive for the full processing future, including a
/// supervisor handoff and the disposition, and releases both only after that.
pub(crate) struct OwnedCommandDelivery {
    delivery: Option<CommandDelivery>,
    coordinates: DeliveryCoordinates,
    settlement: Arc<DeliverySettlement>,
    key: DeliveryKey,
    owned: OwnedReplies,
    _permit: OwnedSemaphorePermit,
}

impl OwnedCommandDelivery {
    #[must_use]
    pub(crate) fn coordinates(&self) -> &DeliveryCoordinates {
        &self.coordinates
    }

    /// Run one processing future while the heartbeat and capacity slot are
    /// retained. Dropping the caller future also drops this owner, leaving
    /// the message to `AckWait` rather than acknowledging an uncertain
    /// execution.
    pub(crate) async fn process<P>(
        &mut self,
        processor: &P,
    ) -> Result<DeliveryVerdict, NatsJetStreamError>
    where
        P: CommandDeliveryProcessor + ?Sized,
    {
        let delivery = self.delivery.take().ok_or_else(|| {
            NatsJetStreamError::protocol("the command delivery owner is already consumed")
        })?;
        Ok(processor.process(delivery).await)
    }

    fn retired(&self) -> bool {
        self.settlement.retired()
    }
}

impl Drop for OwnedCommandDelivery {
    fn drop(&mut self) {
        lock_owned(&self.owned).remove(&self.key);
    }
}

/// One intake turn: commands for the workers and poison messages the intake
/// disposes of itself.
pub(crate) struct CommandBatch {
    pub(crate) commands: Vec<OwnedCommandDelivery>,
    pub(crate) poison: Vec<PoisonDelivery>,
}

/// Stateful bounded intake over one bound consumer.
pub(crate) struct CommandDeliveryIntake<C>
where
    C: CommandBusConnection,
{
    connection: Arc<C>,
    config: CommandDeliveryIntakeConfig,
    capacity: Arc<Semaphore>,
    owned: OwnedReplies,
}

impl<C> CommandDeliveryIntake<C>
where
    C: CommandBusConnection,
{
    #[must_use]
    pub(crate) fn new(connection: Arc<C>, config: CommandDeliveryIntakeConfig) -> Self {
        Self {
            connection,
            config,
            capacity: Arc::new(Semaphore::new(config.ownership_capacity())),
            owned: Arc::new(Mutex::new(BTreeMap::new())),
        }
    }

    /// Await one capacity-bounded pull. At least one permit is reserved
    /// before the pull, and the pull never asks for more messages than there
    /// are free permits.
    pub(crate) async fn next_batch(&self) -> Result<CommandBatch, NatsJetStreamError> {
        let batch_size = self
            .connection
            .fetch_batch()
            .min(self.config.fetch_batch)
            .max(1);
        let permits = self.reserve_capacity(batch_size).await?;
        let fetched = self
            .connection
            .fetch(permits.len(), self.config.fetch_expires)
            .await?;
        self.bind_deliveries(fetched, permits)
    }

    /// Send `+WPI` for every queued or active message.
    pub(crate) async fn heartbeat_owned(&self) -> Result<usize, NatsJetStreamError> {
        let replies: Vec<String> = lock_owned(&self.owned).values().cloned().collect();
        if replies.is_empty() {
            return Ok(0);
        }
        self.connection.in_progress(&replies).await
    }

    #[must_use]
    pub(crate) fn owned_count(&self) -> usize {
        lock_owned(&self.owned).len()
    }

    pub(crate) async fn close(&self) -> Result<(), NatsJetStreamError> {
        self.capacity.close();
        self.connection.close().await
    }

    async fn reserve_capacity(
        &self,
        maximum: usize,
    ) -> Result<Vec<OwnedSemaphorePermit>, NatsJetStreamError> {
        let first = Arc::clone(&self.capacity)
            .acquire_owned()
            .await
            .map_err(|_| NatsJetStreamError::closed())?;
        let mut permits = Vec::with_capacity(maximum);
        permits.push(first);
        while permits.len() < maximum {
            match Arc::clone(&self.capacity).try_acquire_owned() {
                Ok(permit) => permits.push(permit),
                Err(TryAcquireError::NoPermits) => break,
                Err(TryAcquireError::Closed) => return Err(NatsJetStreamError::closed()),
            }
        }
        Ok(permits)
    }

    fn bind_deliveries(
        &self,
        fetched: Vec<DecodedDelivery>,
        permits: Vec<OwnedSemaphorePermit>,
    ) -> Result<CommandBatch, NatsJetStreamError> {
        if fetched.len() > permits.len() {
            return Err(NatsJetStreamError::protocol(
                "the pull returned more messages than the reserved capacity",
            ));
        }
        let mut owned = lock_owned(&self.owned);
        let mut permits = permits.into_iter();
        let mut batch = CommandBatch {
            commands: Vec::with_capacity(fetched.len()),
            poison: Vec::new(),
        };
        for decoded in fetched {
            let delivery = match decoded {
                DecodedDelivery::Command(delivery) => delivery,
                DecodedDelivery::Poison(poison) => {
                    batch.poison.push(poison);
                    continue;
                }
            };
            let permit = permits.next().ok_or_else(|| {
                NatsJetStreamError::protocol("the delivery reservation does not match the pull")
            })?;
            let coordinates = delivery.coordinates().clone();
            let key = (coordinates.stream.clone(), coordinates.stream_sequence);
            // A redelivery of a message this worker still owns (its earlier
            // delivery outlived AckWait): keep the owner, heartbeat it, and
            // let the duplicate go. `+WPI` and the ack are per message.
            if owned.contains_key(&key) {
                tracing::warn!(
                    event = "command_delivery_duplicate_ignored",
                    nats_stream = %coordinates.stream,
                    nats_stream_sequence = coordinates.stream_sequence,
                    nats_delivered = coordinates.delivered,
                );
                continue;
            }
            owned.insert(key.clone(), coordinates.reply.clone());
            batch.commands.push(OwnedCommandDelivery {
                settlement: delivery.settlement(),
                delivery: Some(delivery),
                coordinates,
                key,
                owned: Arc::clone(&self.owned),
                _permit: permit,
            });
        }
        Ok(batch)
    }
}

/// Complete processing boundary for one fetched command.
///
/// The implementation owns verification, the subject-token check, business
/// routing and retirement. The runtime owns intake, ownership, heartbeat and
/// the nak/dead-letter disposition; it never invents an ack.
#[async_trait]
pub(crate) trait CommandDeliveryProcessor: Send + Sync + 'static {
    async fn process(&self, delivery: CommandDelivery) -> DeliveryVerdict;
}

/// Stable, data-free task-group failure.
pub(crate) enum CommandDeliveryRuntimeError {
    Transport(NatsJetStreamError),
    TaskLost(&'static str),
    DrainTimeout(&'static str),
}

impl CommandDeliveryRuntimeError {
    #[must_use]
    pub(crate) const fn code(&self) -> &'static str {
        match self {
            Self::Transport(error) => error.code(),
            Self::TaskLost(_) => "command_delivery.task_lost",
            Self::DrainTimeout(_) => "command_delivery.drain_timeout",
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

impl std::fmt::Debug for CommandDeliveryRuntimeError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("CommandDeliveryRuntimeError")
            .field("code", &self.code())
            .field("retryable", &self.retryable())
            .finish_non_exhaustive()
    }
}

impl std::fmt::Display for CommandDeliveryRuntimeError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Transport(error) => error.fmt(formatter),
            Self::TaskLost(message) | Self::DrainTimeout(message) => formatter.write_str(message),
        }
    }
}

impl std::error::Error for CommandDeliveryRuntimeError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Transport(error) => Some(error),
            Self::TaskLost(_) | Self::DrainTimeout(_) => None,
        }
    }
}

impl From<NatsJetStreamError> for CommandDeliveryRuntimeError {
    fn from(value: NatsJetStreamError) -> Self {
        Self::Transport(value)
    }
}

/// One-shot structured delivery task group.
///
/// Dropping the `run` future aborts its owned Tokio task set; queued and
/// active delivery owners are then dropped and their messages redeliver after
/// `AckWait`. A normal Stop ends intake first, keeps the heartbeat active
/// while existing workers drain, and never turns process shutdown into a
/// business cancellation, an ack or a nak.
pub(crate) struct CommandDeliveryRuntime<C, P>
where
    C: CommandBusConnection,
    P: CommandDeliveryProcessor,
{
    intake: Arc<CommandDeliveryIntake<C>>,
    connection: Arc<C>,
    processor: Arc<P>,
    config: CommandDeliveryRuntimeConfig,
}

impl<C, P> CommandDeliveryRuntime<C, P>
where
    C: CommandBusConnection,
    P: CommandDeliveryProcessor,
{
    #[must_use]
    pub(crate) fn new(
        connection: Arc<C>,
        processor: Arc<P>,
        config: CommandDeliveryRuntimeConfig,
    ) -> Self {
        Self {
            intake: Arc::new(CommandDeliveryIntake::new(
                Arc::clone(&connection),
                config.intake,
            )),
            connection,
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
    ) -> Result<(), CommandDeliveryRuntimeError> {
        if *stop.borrow() {
            return self.intake.close().await.map_err(Into::into);
        }
        let (sender, receiver) = mpsc::channel(self.config.intake.queue_capacity());
        let receiver = Arc::new(AsyncMutex::new(receiver));
        let (heartbeat_stop, heartbeat_stopped) = watch::channel(false);
        let mut tasks = JoinSet::new();
        let mut registry = DeliveryTaskRegistry {
            workers: HashSet::with_capacity(self.config.intake.max_concurrency()),
            heartbeat: None,
        };
        for worker_index in 0..self.config.intake.max_concurrency() {
            let task = tasks.spawn(delivery_worker(
                Arc::clone(&receiver),
                Arc::clone(&self.processor),
                Arc::clone(&self.connection),
                self.config.intake.retry_delay(),
                worker_index,
            ));
            registry.workers.insert(task.id());
        }
        tracing::info!(
            event = "command_delivery_workers_spawned",
            worker_count = registry.workers.len(),
            queue_capacity = self.config.intake.queue_capacity(),
        );
        let heartbeat = tasks.spawn(heartbeat_worker(
            Arc::clone(&self.intake),
            heartbeat_stopped,
            self.config.intake.in_progress_interval(),
        ));
        registry.heartbeat = Some(heartbeat.id());

        let failure = self
            .drive_intake(&mut stop, &sender, &mut tasks, &mut registry)
            .await;
        drop(sender);
        let drain = drain_workers(&mut tasks, &mut registry, heartbeat_stop, failure);
        let result = match tokio::time::timeout(self.config.shutdown_timeout, drain).await {
            Ok(result) => result,
            Err(_) => Err(CommandDeliveryRuntimeError::DrainTimeout(
                "the command delivery workers did not drain before shutdown",
            )),
        };
        // Whatever is still owned here is left to AckWait: no ack, no nak.
        tasks.abort_all();
        let close = self.intake.close().await.map_err(Into::into);
        combine_runtime_results(result, close)
    }

    async fn drive_intake(
        &self,
        stop: &mut watch::Receiver<bool>,
        sender: &mpsc::Sender<OwnedCommandDelivery>,
        tasks: &mut JoinSet<DeliveryTaskExit>,
        registry: &mut DeliveryTaskRegistry,
    ) -> Option<CommandDeliveryRuntimeError> {
        loop {
            tokio::select! {
                biased;
                () = wait_for_stop(stop) => return None,
                task = tasks.join_next_with_id() => {
                    return Some(registry.classify(task).failure());
                },
                batch = self.intake.next_batch() => {
                    match batch {
                        Ok(batch) => {
                            for poison in batch.poison {
                                dispose_poison(
                                    self.connection.as_ref(),
                                    &poison.coordinates,
                                    poison.reason,
                                    None,
                                )
                                .await;
                            }
                            if !batch.commands.is_empty() {
                                tracing::info!(
                                    event = "command_delivery_batch_received",
                                    delivery_count = batch.commands.len(),
                                    owned_delivery_count = self.intake.owned_count(),
                                );
                            }
                            if !enqueue_batch(batch.commands, sender, stop).await {
                                return None;
                            }
                        }
                        Err(error) if !intake_failure_is_fatal(&error) => {
                            tracing::warn!(
                                event = "command_intake_retry",
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

enum DeliveryTaskExit {
    Worker(Result<(), NatsJetStreamError>),
    Heartbeat(Result<(), NatsJetStreamError>),
}

struct DeliveryTaskRegistry {
    workers: HashSet<TaskId>,
    heartbeat: Option<TaskId>,
}

enum JoinedDeliveryTask {
    Worker(Result<(), NatsJetStreamError>),
    Heartbeat(Result<(), NatsJetStreamError>),
    Lost,
}

impl JoinedDeliveryTask {
    fn failure(self) -> CommandDeliveryRuntimeError {
        match self {
            Self::Worker(Err(error)) | Self::Heartbeat(Err(error)) => error.into(),
            Self::Worker(Ok(())) | Self::Heartbeat(Ok(())) | Self::Lost => {
                CommandDeliveryRuntimeError::TaskLost(
                    "a command delivery background task ended unexpectedly",
                )
            }
        }
    }
}

impl DeliveryTaskRegistry {
    fn classify(
        &mut self,
        result: Option<Result<(TaskId, DeliveryTaskExit), JoinError>>,
    ) -> JoinedDeliveryTask {
        let Some(result) = result else {
            return JoinedDeliveryTask::Lost;
        };
        match result {
            Ok((id, DeliveryTaskExit::Worker(result))) if self.workers.remove(&id) => {
                JoinedDeliveryTask::Worker(result)
            }
            Ok((id, DeliveryTaskExit::Heartbeat(result))) if self.heartbeat == Some(id) => {
                self.heartbeat = None;
                JoinedDeliveryTask::Heartbeat(result)
            }
            Ok(_) => JoinedDeliveryTask::Lost,
            Err(error) => {
                let id = error.id();
                self.workers.remove(&id);
                if self.heartbeat == Some(id) {
                    self.heartbeat = None;
                }
                JoinedDeliveryTask::Lost
            }
        }
    }
}

async fn delivery_worker<P, C>(
    receiver: Arc<AsyncMutex<mpsc::Receiver<OwnedCommandDelivery>>>,
    processor: Arc<P>,
    connection: Arc<C>,
    retry_delay: Duration,
    worker_index: usize,
) -> DeliveryTaskExit
where
    P: CommandDeliveryProcessor,
    C: CommandBusConnection,
{
    tracing::info!(event = "command_delivery_worker_started", worker_index);
    loop {
        let delivery = receiver.lock().await.recv().await;
        let Some(mut delivery) = delivery else {
            tracing::info!(event = "command_delivery_worker_stopped", worker_index);
            return DeliveryTaskExit::Worker(Ok(()));
        };
        let nats_stream = delivery.coordinates().stream.clone();
        let nats_stream_sequence = delivery.coordinates().stream_sequence;
        let nats_delivered = delivery.coordinates().delivered;
        tracing::info!(
            event = "command_delivery_worker_received",
            worker_index,
            nats_stream,
            nats_stream_sequence,
            nats_delivered,
        );
        let verdict = match delivery.process(processor.as_ref()).await {
            Ok(verdict) => verdict,
            Err(error) => return DeliveryTaskExit::Worker(Err(error)),
        };
        // The disposition runs while the owner (and its heartbeat) is held.
        let disposition =
            dispose_processed(connection.as_ref(), &delivery, verdict, retry_delay).await;
        drop(delivery);
        tracing::info!(
            event = "command_delivery_worker_completed",
            worker_index,
            nats_stream,
            nats_stream_sequence,
            disposition,
        );
    }
}

/// Apply the contract's disposition to one processed delivery and return its
/// stable name.
async fn dispose_processed<C>(
    connection: &C,
    delivery: &OwnedCommandDelivery,
    verdict: DeliveryVerdict,
    retry_delay: Duration,
) -> &'static str
where
    C: CommandBusConnection,
{
    match verdict {
        DeliveryVerdict::Poison {
            reason,
            delivery_token,
        } => {
            dispose_poison(
                connection,
                delivery.coordinates(),
                reason,
                delivery_token.as_deref(),
            )
            .await;
            "dead_lettered"
        }
        DeliveryVerdict::Processed if delivery.retired() => "retired",
        DeliveryVerdict::Processed => {
            if let Err(error) = connection
                .nak(&delivery.coordinates().reply, retry_delay)
                .await
            {
                // Unconfirmed: AckWait redelivers instead.
                tracing::warn!(
                    event = "command_delivery_nak_unconfirmed",
                    error_code = error.code(),
                    retryable = error.retryable(),
                );
            }
            "retry_later"
        }
    }
}

/// Nak a poison delivery for 24h (never `Term`), record it in the
/// dead-letter bucket, and raise the ERROR event and counter.
pub(crate) async fn dispose_poison<C>(
    connection: &C,
    coordinates: &DeliveryCoordinates,
    reason: PoisonReason,
    delivery_token: Option<&str>,
) where
    C: CommandBusConnection + ?Sized,
{
    let nak = connection.nak(&coordinates.reply, POISON_DELAY).await;
    let record = DeadLetterRecord::new(
        coordinates,
        reason,
        delivery_token,
        connection.worker_name(),
        unix_millis_now(),
    );
    let recorded = connection.record_dead_letter(&record).await;
    let total = DEAD_LETTERED_TOTAL.fetch_add(1, Ordering::Relaxed) + 1;
    tracing::error!(
        event = "worker_command.dead_lettered",
        reason = reason.code(),
        nats_stream = %coordinates.stream,
        nats_consumer = %coordinates.consumer,
        nats_subject = %coordinates.subject,
        nats_stream_sequence = coordinates.stream_sequence,
        nats_delivered = coordinates.delivered,
        dead_letter_key = %record.key,
        nak_confirmed = nak.is_ok(),
        record_written = recorded.is_ok(),
        dead_lettered_total = total,
    );
}

fn unix_millis_now() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .ok()
        .and_then(|elapsed| u64::try_from(elapsed.as_millis()).ok())
        .unwrap_or(0)
}

async fn heartbeat_worker<C>(
    intake: Arc<CommandDeliveryIntake<C>>,
    mut stop: watch::Receiver<bool>,
    cadence: Duration,
) -> DeliveryTaskExit
where
    C: CommandBusConnection,
{
    let mut ticker = interval_at(Instant::now() + cadence, cadence);
    ticker.set_missed_tick_behavior(MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            biased;
            () = wait_for_stop(&mut stop) => {
                return DeliveryTaskExit::Heartbeat(Ok(()));
            }
            _ = ticker.tick() => {
                match intake.heartbeat_owned().await {
                    Ok(_) => {}
                    Err(error) if !intake_failure_is_fatal(&error) => {
                        tracing::warn!(
                            event = "command_heartbeat_retry",
                            error_code = error.code(),
                            retryable = true,
                        );
                    }
                    Err(error) => return DeliveryTaskExit::Heartbeat(Err(error)),
                }
            }
        }
    }
}

async fn enqueue_batch(
    deliveries: Vec<OwnedCommandDelivery>,
    sender: &mpsc::Sender<OwnedCommandDelivery>,
    stop: &mut watch::Receiver<bool>,
) -> bool {
    for delivery in deliveries {
        let nats_stream = delivery.coordinates().stream.clone();
        let nats_stream_sequence = delivery.coordinates().stream_sequence;
        tokio::select! {
            biased;
            () = wait_for_stop(stop) => return false,
            result = sender.send(delivery) => {
                if result.is_err() {
                    return false;
                }
                tracing::info!(
                    event = "command_delivery_enqueued",
                    nats_stream,
                    nats_stream_sequence,
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
    tasks: &mut JoinSet<DeliveryTaskExit>,
    registry: &mut DeliveryTaskRegistry,
    heartbeat_stop: watch::Sender<bool>,
    mut failure: Option<CommandDeliveryRuntimeError>,
) -> Result<(), CommandDeliveryRuntimeError> {
    while !registry.workers.is_empty() {
        match registry.classify(tasks.join_next_with_id().await) {
            JoinedDeliveryTask::Worker(Ok(())) => {}
            JoinedDeliveryTask::Worker(Err(error)) | JoinedDeliveryTask::Heartbeat(Err(error)) => {
                failure.get_or_insert_with(|| error.into());
            }
            JoinedDeliveryTask::Heartbeat(Ok(())) => {
                failure.get_or_insert(CommandDeliveryRuntimeError::TaskLost(
                    "the heartbeat stopped before delivery drain completed",
                ));
            }
            JoinedDeliveryTask::Lost => {
                failure.get_or_insert(CommandDeliveryRuntimeError::TaskLost(
                    "the command delivery task group ended before drain completed",
                ));
                break;
            }
        }
    }
    let _ignored = heartbeat_stop.send(true);
    while !tasks.is_empty() {
        match registry.classify(tasks.join_next_with_id().await) {
            JoinedDeliveryTask::Heartbeat(Ok(())) | JoinedDeliveryTask::Worker(Ok(())) => {}
            JoinedDeliveryTask::Worker(Err(error)) | JoinedDeliveryTask::Heartbeat(Err(error)) => {
                failure.get_or_insert_with(|| error.into());
            }
            JoinedDeliveryTask::Lost => {
                failure.get_or_insert(CommandDeliveryRuntimeError::TaskLost(
                    "a command delivery background task was lost during shutdown",
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
    primary: Result<(), CommandDeliveryRuntimeError>,
    close: Result<(), CommandDeliveryRuntimeError>,
) -> Result<(), CommandDeliveryRuntimeError> {
    match (primary, close) {
        (Ok(()), Ok(())) => Ok(()),
        (Err(error), Ok(())) | (Ok(()), Err(error)) => Err(error),
        (Err(primary), Err(close_error)) => {
            tracing::warn!(
                event = "command_bus_close_after_failure",
                error_code = close_error.code(),
                retryable = close_error.retryable(),
            );
            Err(primary)
        }
    }
}

fn lock_owned(
    owned: &Mutex<BTreeMap<DeliveryKey, String>>,
) -> MutexGuard<'_, BTreeMap<DeliveryKey, String>> {
    match owned.lock() {
        Ok(guard) => guard,
        Err(poisoned) => poisoned.into_inner(),
    }
}

#[must_use]
pub(crate) const fn intake_failure_is_fatal(error: &NatsJetStreamError) -> bool {
    !matches!(
        error.kind(),
        NatsJetStreamErrorKind::DependencyUnavailable | NatsJetStreamErrorKind::Timeout
    )
}

/// The dead-letter reason of a command that failed signed-envelope
/// verification: every such failure is poison (docs/runtime-command-bus.md).
#[must_use]
pub(crate) const fn verification_poison(error: &crate::protocol::ProtocolError) -> PoisonReason {
    match error {
        crate::protocol::ProtocolError::AuthorizationFailed(_) => PoisonReason::SignatureInvalid,
        crate::protocol::ProtocolError::UnsupportedCapability(_) => {
            PoisonReason::UnsupportedCommand
        }
        crate::protocol::ProtocolError::InvalidInput(_)
        | crate::protocol::ProtocolError::ResourceExhausted(_)
        | crate::protocol::ProtocolError::InputFieldLimit { .. }
        | crate::protocol::ProtocolError::IncompatibleVersion(_) => PoisonReason::EnvelopeInvalid,
    }
}
