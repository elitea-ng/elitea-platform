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
//! | poison that can never verify (signature failure, subject mismatch) | dead-letter record, then `+TERM` + ERROR log + counter |
//! | other poison (decode failure, unsupported command, malformed message) | dead-letter record, then nak 24h + ERROR log + counter |
//! | poison whose dead-letter record could not be written | nak with the retry delay (the next delivery retries the record) |
//! | queued, never started, when the worker stops | nak with no delay (another replica takes it now) |
//!
//! It never acknowledges a command itself: the exact post-settlement
//! retirement authority remains the only ack path.
//!
//! Capacity: one replica owns at most `max_concurrency` running commands plus
//! a prefetch of `min(fetch_batch, queue_capacity)` waiting behind them. Each
//! pull asks only for the permits that are free, so a replica never holds work
//! it cannot start soon: the KEDA scaler counts delivered-but-unacked commands
//! (`num_ack_pending`) as in flight, and anything a replica hoards is invisible
//! to the replicas KEDA adds.
//!
//! A delayed nak is never followed by a `+WPI` for the same delivery: the
//! server treats `+WPI` as progress and resets the redelivery timer to
//! `AckWait`, which would turn a 24h or retry delay into a 60s loop. The
//! disposition stops owning the delivery under the same gate a heartbeat round
//! holds for its whole send (so the round that included it has finished and no
//! later round includes it), then releases the gate before answering: the
//! answer is a request/reply bounded by the request timeout, and holding the
//! gate across it would queue every heartbeat round behind slow answers.

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

/// Process-wide count of poison deliveries whose dead-letter record could not
/// be written (each is nak'd with the retry delay instead of being parked or
/// terminated, so the next delivery retries the record). Carried on every
/// `worker_command.dead_letter_write_failed` ERROR event.
static DEAD_LETTER_WRITE_FAILED_TOTAL: AtomicU64 = AtomicU64::new(0);

#[must_use]
pub(crate) fn dead_lettered_total() -> u64 {
    DEAD_LETTERED_TOTAL.load(Ordering::Relaxed)
}

#[must_use]
pub(crate) fn dead_letter_write_failed_total() -> u64 {
    DEAD_LETTER_WRITE_FAILED_TOTAL.load(Ordering::Relaxed)
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

    /// Running commands plus a small prefetch behind them. The prefetch is
    /// at most one pull (`fetch_batch`) and at most the queue.
    const fn ownership_capacity(self) -> usize {
        self.max_concurrency + self.prefetch()
    }

    const fn prefetch(self) -> usize {
        if self.fetch_batch < self.queue_capacity {
            self.fetch_batch
        } else {
            self.queue_capacity
        }
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

/// Every queued or active delivery this process owns, with the reply subject
/// of its NEWEST delivery (a redelivery that arrives while the first copy is
/// still owned is not run twice, but its ack subject is the one the server now
/// tracks), and the gate a heartbeat round and a delayed answer share.
#[derive(Default)]
struct OwnershipLedger {
    owned: Mutex<BTreeMap<DeliveryKey, String>>,
    gate: Arc<AsyncMutex<()>>,
}

type OwnedReplies = Arc<OwnershipLedger>;

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
    released: bool,
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

    /// Stop owning this delivery so it can be answered. The removal happens
    /// under the heartbeat gate: a round holds the gate for its whole send, so
    /// acquiring it means any round that included this delivery has finished,
    /// and every later round snapshots the ledger without it. The gate is
    /// released before returning, so the (possibly slow) answer never delays
    /// a heartbeat round for the other deliveries. Returns the reply subject
    /// of the newest copy of the message.
    async fn release_for_answer(&mut self) -> String {
        let _gate = self.owned.gate.lock().await;
        let reply = lock_owned(&self.owned.owned)
            .remove(&self.key)
            .unwrap_or_else(|| self.coordinates.reply.clone());
        self.released = true;
        reply
    }
}

impl Drop for OwnedCommandDelivery {
    fn drop(&mut self) {
        if !self.released {
            lock_owned(&self.owned.owned).remove(&self.key);
        }
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
            owned: Arc::new(OwnershipLedger::default()),
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

    /// Send `+WPI` for every queued or active message. The gate is held for
    /// the whole round, and a delivery leaves the ledger only under the gate
    /// before it is answered, so a delivery answered with a delayed nak is
    /// either in a round that finished before the nak was sent or in none.
    pub(crate) async fn heartbeat_owned(&self) -> Result<usize, NatsJetStreamError> {
        let _round = self.owned.gate.lock().await;
        let replies: Vec<String> = lock_owned(&self.owned.owned).values().cloned().collect();
        if replies.is_empty() {
            return Ok(0);
        }
        self.connection.in_progress(&replies).await
    }

    #[must_use]
    pub(crate) fn owned_count(&self) -> usize {
        lock_owned(&self.owned.owned).len()
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
        let mut owned = lock_owned(&self.owned.owned);
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
            // delivery outlived AckWait): keep the owner — the command is not
            // run twice — but heartbeat and answer the NEWEST copy, whose ack
            // subject is the one the server now tracks.
            if let Some(reply) = owned.get_mut(&key) {
                tracing::warn!(
                    event = "command_delivery_duplicate_ignored",
                    nats_stream = %coordinates.stream,
                    nats_stream_sequence = coordinates.stream_sequence,
                    nats_delivered = coordinates.delivered,
                );
                reply.clone_from(&coordinates.reply);
                continue;
            }
            owned.insert(key.clone(), coordinates.reply.clone());
            batch.commands.push(OwnedCommandDelivery {
                settlement: delivery.settlement(),
                delivery: Some(delivery),
                coordinates,
                key,
                owned: Arc::clone(&self.owned),
                released: false,
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
                stop.clone(),
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
                                // Never owned, so never heartbeated: answer
                                // it directly.
                                let answer = record_poison(
                                    self.connection.as_ref(),
                                    &poison.coordinates,
                                    poison.reason,
                                    None,
                                    self.config.intake.retry_delay(),
                                )
                                .await;
                                send_answer(
                                    self.connection.as_ref(),
                                    &poison.coordinates.reply,
                                    answer,
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
                            if let Err(unsent) = enqueue_batch(batch.commands, sender, stop).await {
                                for delivery in unsent {
                                    release_unstarted(self.connection.as_ref(), delivery).await;
                                }
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
    stop: watch::Receiver<bool>,
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
        if *stop.borrow() {
            // Queued, never started, and this process is stopping: give it
            // back now rather than start it only to have shutdown cut it off
            // (or leave it to AckWait).
            release_unstarted(connection.as_ref(), delivery).await;
            continue;
        }
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
        // The disposition runs while the owner (and its heartbeat) is held,
        // and stops owning the delivery before any delayed answer.
        let disposition =
            dispose_processed(connection.as_ref(), &mut delivery, verdict, retry_delay).await;
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

/// How a delivery that ends without a retirement is answered.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Answer {
    Nak(Duration),
    Term,
}

/// Give a queued, never-started delivery back at shutdown: nak with no delay.
async fn release_unstarted<C>(connection: &C, mut delivery: OwnedCommandDelivery)
where
    C: CommandBusConnection + ?Sized,
{
    let reply = delivery.release_for_answer().await;
    let released = connection.nak(&reply, Duration::ZERO).await;
    tracing::info!(
        event = "command_delivery_released_at_shutdown",
        nats_stream = %delivery.coordinates().stream,
        nats_stream_sequence = delivery.coordinates().stream_sequence,
        nak_confirmed = released.is_ok(),
    );
}

/// Apply the contract's disposition to one processed delivery and return its
/// stable name.
async fn dispose_processed<C>(
    connection: &C,
    delivery: &mut OwnedCommandDelivery,
    verdict: DeliveryVerdict,
    retry_delay: Duration,
) -> &'static str
where
    C: CommandBusConnection,
{
    let (answer, disposition) = match verdict {
        DeliveryVerdict::Processed if delivery.retired() => return "retired",
        DeliveryVerdict::Processed => (Answer::Nak(retry_delay), "retry_later"),
        DeliveryVerdict::Poison {
            reason,
            delivery_token,
        } => {
            let answer = record_poison(
                connection,
                delivery.coordinates(),
                reason,
                delivery_token.as_deref(),
                retry_delay,
            )
            .await;
            let disposition = match answer {
                Answer::Term => "dead_lettered_terminated",
                Answer::Nak(delay) if delay == POISON_DELAY => "dead_lettered",
                Answer::Nak(_) => "dead_letter_unrecorded_retry",
            };
            (answer, disposition)
        }
    };
    let reply = delivery.release_for_answer().await;
    send_answer(connection, &reply, answer).await;
    disposition
}

#[cfg(test)]
pub(super) async fn test_dispose_processed<C>(
    connection: &C,
    delivery: &mut OwnedCommandDelivery,
    verdict: DeliveryVerdict,
    retry_delay: Duration,
) -> &'static str
where
    C: CommandBusConnection,
{
    dispose_processed(connection, delivery, verdict, retry_delay).await
}

/// Send one answer; an unconfirmed one is left to `AckWait`.
async fn send_answer<C>(connection: &C, reply: &str, answer: Answer)
where
    C: CommandBusConnection + ?Sized,
{
    let result = match answer {
        Answer::Nak(delay) => connection.nak(reply, delay).await,
        Answer::Term => connection.term(reply).await,
    };
    if let Err(error) = result {
        tracing::warn!(
            event = "command_delivery_answer_unconfirmed",
            answer = match answer {
                Answer::Nak(_) => "nak",
                Answer::Term => "term",
            },
            error_code = error.code(),
            retryable = error.retryable(),
        );
    }
}

/// Record a poison delivery in the dead-letter bucket FIRST, raise the ERROR
/// event and counter, and decide its answer: `+TERM` for a command that can
/// never verify, a 24h nak for the other poison classes — and, when the record
/// could not be written, the ordinary retry delay instead, so that no poison is
/// parked or terminated without its record (the next delivery retries it).
async fn record_poison<C>(
    connection: &C,
    coordinates: &DeliveryCoordinates,
    reason: PoisonReason,
    delivery_token: Option<&str>,
    retry_delay: Duration,
) -> Answer
where
    C: CommandBusConnection + ?Sized,
{
    let record = DeadLetterRecord::new(
        coordinates,
        reason,
        delivery_token,
        connection.worker_name(),
        unix_millis_now(),
    );
    if let Err(error) = connection.record_dead_letter(&record).await {
        let total = DEAD_LETTER_WRITE_FAILED_TOTAL.fetch_add(1, Ordering::Relaxed) + 1;
        tracing::error!(
            event = "worker_command.dead_letter_write_failed",
            reason = reason.code(),
            nats_stream = %coordinates.stream,
            nats_consumer = %coordinates.consumer,
            nats_subject = %coordinates.subject,
            nats_stream_sequence = coordinates.stream_sequence,
            nats_delivered = coordinates.delivered,
            dead_letter_key = %record.key,
            error_code = error.code(),
            retry_delay_millis = u64::try_from(retry_delay.as_millis()).unwrap_or(u64::MAX),
            dead_letter_write_failed_total = total,
        );
        return Answer::Nak(retry_delay);
    }
    let answer = if reason.terminates() {
        Answer::Term
    } else {
        Answer::Nak(POISON_DELAY)
    };
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
        terminated = answer == Answer::Term,
        dead_lettered_total = total,
    );
    answer
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

/// Hand a batch to the workers. On Stop, every delivery not yet handed over is
/// returned so it can be given back at once.
async fn enqueue_batch(
    deliveries: Vec<OwnedCommandDelivery>,
    sender: &mpsc::Sender<OwnedCommandDelivery>,
    stop: &mut watch::Receiver<bool>,
) -> Result<(), Vec<OwnedCommandDelivery>> {
    let mut pending = deliveries.into_iter();
    while let Some(delivery) = pending.next() {
        let nats_stream = delivery.coordinates().stream.clone();
        let nats_stream_sequence = delivery.coordinates().stream_sequence;
        if *stop.borrow() {
            return Err(std::iter::once(delivery).chain(pending).collect());
        }
        let permit = tokio::select! {
            biased;
            () = wait_for_stop(stop) => None,
            permit = sender.reserve() => permit.ok(),
        };
        let Some(permit) = permit else {
            return Err(std::iter::once(delivery).chain(pending).collect());
        };
        permit.send(delivery);
        tracing::info!(
            event = "command_delivery_enqueued",
            nats_stream,
            nats_stream_sequence,
            queue_remaining_capacity = sender.capacity(),
        );
    }
    Ok(())
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
