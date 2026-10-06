use std::collections::{BTreeMap, VecDeque};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use tokio::sync::{Notify, Semaphore, watch};

use super::command_delivery::{
    CommandDeliveryIntake, CommandDeliveryIntakeConfig, CommandDeliveryProcessor,
    CommandDeliveryRuntime, CommandDeliveryRuntimeConfig, dead_letter_write_failed_total,
    dead_lettered_total, intake_failure_is_fatal, verification_poison,
};
use crate::protocol::ProtocolError;
use crate::transport::command_bus::{
    CommandBusLimits, CommandDelivery, CommandRetirementClient, CommandRetirementClientError,
    CommandRetirementRequest, DeadLetterRecord, DecodedDelivery, DeliveryVerdict, POISON_DELAY,
    PoisonReason, decode_delivery, delivery_subject, delivery_token,
};
use crate::transport::nats_jetstream::{
    CommandBusConnection, NatsJetStreamError, NatsJetStreamErrorKind,
};

const RETRY_DELAY: Duration = Duration::from_mins(1);

#[derive(Clone, Debug, Eq, PartialEq)]
enum FakeOperation {
    Fetch {
        max: usize,
        expires: Duration,
    },
    InProgress(Vec<u64>),
    Nak {
        sequence: u64,
        delivered: u64,
        delay: Duration,
    },
    Term(u64),
    DeadLetter {
        key: String,
        reason: PoisonReason,
    },
    Retire(u64),
    Close,
}

struct FakeConnection {
    fetch_batch: usize,
    fetches: Mutex<VecDeque<Result<Vec<DecodedDelivery>, NatsJetStreamError>>>,
    heartbeats: Mutex<VecDeque<Result<usize, NatsJetStreamError>>>,
    dead_letter_fails: bool,
    /// When set, a heartbeat round waits for a permit before its `+WPI`
    /// reaches the (fake) server: a round paused mid-send.
    heartbeat_pause: Option<Arc<Semaphore>>,
    heartbeat_entered: Notify,
    /// When set, a nak or term waits for a permit before it reaches the
    /// (fake) server: an answer stuck in its request/reply timeout.
    answer_pause: Option<Arc<Semaphore>>,
    answer_entered: Notify,
    wpi_replies: Mutex<Vec<Vec<String>>>,
    operations: Mutex<Vec<FakeOperation>>,
    operation_changed: Notify,
    fetch_available: Notify,
}

impl FakeConnection {
    fn new(fetch_batch: usize) -> Self {
        Self {
            fetch_batch,
            fetches: Mutex::new(VecDeque::new()),
            heartbeats: Mutex::new(VecDeque::new()),
            dead_letter_fails: false,
            heartbeat_pause: None,
            heartbeat_entered: Notify::new(),
            answer_pause: None,
            answer_entered: Notify::new(),
            wpi_replies: Mutex::new(Vec::new()),
            operations: Mutex::new(Vec::new()),
            operation_changed: Notify::new(),
            fetch_available: Notify::new(),
        }
    }

    fn push_fetch(&self, value: Result<Vec<DecodedDelivery>, NatsJetStreamError>) {
        self.fetches.lock().expect("fetch queue").push_back(value);
        self.fetch_available.notify_one();
    }

    fn push_heartbeat(&self, value: Result<usize, NatsJetStreamError>) {
        self.heartbeats
            .lock()
            .expect("heartbeat queue")
            .push_back(value);
    }

    fn record(&self, operation: FakeOperation) {
        self.operations
            .lock()
            .expect("operation log")
            .push(operation);
        self.operation_changed.notify_waiters();
    }

    async fn answer_paused(&self) {
        self.answer_entered.notify_waiters();
        if let Some(pause) = &self.answer_pause {
            pause.acquire().await.expect("answer pause").forget();
        }
    }

    fn operations(&self) -> Vec<FakeOperation> {
        self.operations.lock().expect("operation log").clone()
    }

    async fn wait_for(&self, predicate: impl Fn(&[FakeOperation]) -> bool) {
        loop {
            let changed = self.operation_changed.notified();
            if predicate(&self.operations.lock().expect("operation log")) {
                return;
            }
            changed.await;
        }
    }
}

fn sequence_of(reply: &str) -> u64 {
    crate::transport::command_bus::AckReplyInfo::parse(reply)
        .expect("fake ack subject")
        .stream_sequence
}

fn delivered_of(reply: &str) -> u64 {
    crate::transport::command_bus::AckReplyInfo::parse(reply)
        .expect("fake ack subject")
        .delivered
}

#[async_trait]
impl CommandRetirementClient for FakeConnection {
    async fn retire_delivery(
        &self,
        request: CommandRetirementRequest,
    ) -> Result<(), CommandRetirementClientError> {
        self.record(FakeOperation::Retire(request.stream_sequence()));
        Ok(())
    }
}

#[async_trait]
impl CommandBusConnection for FakeConnection {
    fn fetch_batch(&self) -> usize {
        self.fetch_batch
    }

    async fn fetch(
        &self,
        max: usize,
        expires: Duration,
    ) -> Result<Vec<DecodedDelivery>, NatsJetStreamError> {
        self.record(FakeOperation::Fetch { max, expires });
        loop {
            let available = self.fetch_available.notified();
            if let Some(result) = self.fetches.lock().expect("fetch queue").pop_front() {
                return result;
            }
            available.await;
        }
    }

    async fn in_progress(&self, replies: &[String]) -> Result<usize, NatsJetStreamError> {
        self.heartbeat_entered.notify_waiters();
        if let Some(pause) = &self.heartbeat_pause {
            pause.acquire().await.expect("heartbeat pause").forget();
        }
        self.wpi_replies
            .lock()
            .expect("wpi replies")
            .push(replies.to_vec());
        let mut sequences = replies
            .iter()
            .map(|reply| sequence_of(reply))
            .collect::<Vec<_>>();
        sequences.sort_unstable();
        self.record(FakeOperation::InProgress(sequences));
        self.heartbeats
            .lock()
            .expect("heartbeat queue")
            .pop_front()
            .unwrap_or(Ok(replies.len()))
    }

    async fn nak(&self, reply: &str, delay: Duration) -> Result<(), NatsJetStreamError> {
        self.answer_paused().await;
        self.record(FakeOperation::Nak {
            sequence: sequence_of(reply),
            delivered: delivered_of(reply),
            delay,
        });
        Ok(())
    }

    async fn term(&self, reply: &str) -> Result<(), NatsJetStreamError> {
        self.answer_paused().await;
        self.record(FakeOperation::Term(sequence_of(reply)));
        Ok(())
    }

    async fn record_dead_letter(
        &self,
        record: &DeadLetterRecord,
    ) -> Result<(), NatsJetStreamError> {
        self.record(FakeOperation::DeadLetter {
            key: record.key.clone(),
            reason: record.reason,
        });
        if self.dead_letter_fails {
            Err(NatsJetStreamError::unavailable("fake dead-letter outage"))
        } else {
            Ok(())
        }
    }

    #[allow(clippy::unnecessary_literal_bound)] // The trait borrows from a real connection.
    fn worker_name(&self) -> &str {
        "worker-test"
    }

    async fn close(&self) -> Result<(), NatsJetStreamError> {
        self.record(FakeOperation::Close);
        Ok(())
    }
}

/// What the scripted processor concludes for one stream sequence.
#[derive(Clone, Copy)]
enum Script {
    Retire,
    RetryLater,
    Poison(PoisonReason),
}

struct TestProcessor {
    script: BTreeMap<u64, Script>,
    started: AtomicUsize,
    completed: AtomicUsize,
    active: AtomicUsize,
    maximum_active: AtomicUsize,
    started_changed: Notify,
    completed_changed: Notify,
    release: Semaphore,
    sequences: Mutex<Vec<u64>>,
}

impl TestProcessor {
    fn new() -> Self {
        Self::scripted(BTreeMap::new())
    }

    fn scripted(script: BTreeMap<u64, Script>) -> Self {
        Self {
            script,
            started: AtomicUsize::new(0),
            completed: AtomicUsize::new(0),
            active: AtomicUsize::new(0),
            maximum_active: AtomicUsize::new(0),
            started_changed: Notify::new(),
            completed_changed: Notify::new(),
            release: Semaphore::new(0),
            sequences: Mutex::new(Vec::new()),
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
impl CommandDeliveryProcessor for TestProcessor {
    async fn process(&self, delivery: CommandDelivery) -> DeliveryVerdict {
        let _active = ActiveProcess::enter(self);
        let sequence = delivery.stream_sequence();
        self.sequences.lock().expect("sequences").push(sequence);
        self.started.fetch_add(1, Ordering::AcqRel);
        self.started_changed.notify_waiters();
        let permit = self.release.acquire().await.expect("release semaphore");
        permit.forget();
        let verdict = match self.script.get(&sequence).copied() {
            Some(Script::Retire) => {
                // Stands in for CommandRetirer's confirmed double ack.
                delivery.settlement().test_mark_retired();
                DeliveryVerdict::Processed
            }
            Some(Script::Poison(reason)) => DeliveryVerdict::Poison {
                reason,
                delivery_token: Some(delivery_token("poison-key")),
            },
            Some(Script::RetryLater) | None => DeliveryVerdict::Processed,
        };
        self.completed.fetch_add(1, Ordering::AcqRel);
        self.completed_changed.notify_waiters();
        verdict
    }
}

fn config(max_concurrency: usize, queue_capacity: usize) -> CommandDeliveryIntakeConfig {
    CommandDeliveryIntakeConfig::new(max_concurrency, queue_capacity, 64, 30_000, 1_000, 60_000)
        .expect("valid intake config")
}

fn runtime_config(max_concurrency: usize, queue_capacity: usize) -> CommandDeliveryRuntimeConfig {
    CommandDeliveryRuntimeConfig::new(config(max_concurrency, queue_capacity), 100, 3_000)
        .expect("valid runtime config")
}

fn reply(sequence: u64, delivered: u64) -> String {
    format!(
        "$JS.ACK.ELITEA_RT_V1_AGENT.elitea-agent-worker-v1.{delivered}.{sequence}.{sequence}.1700000000000000000.0"
    )
}

fn delivery(sequence: u64) -> DecodedDelivery {
    delivered(sequence, 1)
}

fn delivered(sequence: u64, count: u64) -> DecodedDelivery {
    let subject =
        delivery_subject("ELITEA_RT_V1_AGENT", &format!("delivery-{sequence}")).expect("subject");
    decode_delivery(
        &subject,
        &reply(sequence, count),
        b"signed-command".to_vec(),
        64,
        CommandBusLimits::runtime_v1(),
    )
    .expect("valid delivery fixture")
}

fn malformed_subject(sequence: u64) -> DecodedDelivery {
    decode_delivery(
        "elitea.rt.v1.agent.d.not-a-token",
        &reply(sequence, 1),
        b"signed-command".to_vec(),
        64,
        CommandBusLimits::runtime_v1(),
    )
    .expect("ackable poison fixture")
}

fn intake(
    connection: Arc<FakeConnection>,
    config: CommandDeliveryIntakeConfig,
) -> CommandDeliveryIntake<FakeConnection> {
    CommandDeliveryIntake::new(connection, config)
}

fn naks(connection: &FakeConnection) -> Vec<(u64, Duration)> {
    connection
        .operations()
        .into_iter()
        .filter_map(|operation| match operation {
            FakeOperation::Nak {
                sequence, delay, ..
            } => Some((sequence, delay)),
            _ => None,
        })
        .collect()
}

#[tokio::test]
async fn intake_reserves_permits_before_the_pull_and_never_asks_for_more() {
    let connection = Arc::new(FakeConnection::new(4));
    connection.push_fetch(Ok(vec![delivery(1), delivery(2)]));
    connection.push_fetch(Ok(vec![delivery(3)]));
    // Concurrency 1 + queue 1: two owned deliveries fill the capacity.
    let intake = Arc::new(intake(Arc::clone(&connection), config(1, 1)));
    let mut first = intake.next_batch().await.expect("capacity-filling batch");
    assert_eq!(first.commands.len(), 2);
    assert_eq!(
        connection.operations(),
        [FakeOperation::Fetch {
            max: 2,
            expires: Duration::from_secs(30)
        }]
    );

    let waiting = tokio::spawn({
        let intake = Arc::clone(&intake);
        async move { intake.next_batch().await }
    });
    tokio::task::yield_now().await;
    assert!(!waiting.is_finished());
    assert_eq!(connection.operations().len(), 1, "no pull without a permit");

    drop(first.commands.pop());
    let next = waiting
        .await
        .expect("waiting intake task")
        .expect("next admitted batch");
    assert_eq!(next.commands.len(), 1);
    assert_eq!(
        connection.operations().last(),
        Some(&FakeOperation::Fetch {
            max: 1,
            expires: Duration::from_secs(30)
        })
    );
    assert_eq!(intake.owned_count(), 2);
}

#[tokio::test]
async fn a_redelivery_of_an_owned_message_is_not_locally_admitted_twice() {
    let connection = Arc::new(FakeConnection::new(2));
    connection.push_fetch(Ok(vec![delivery(1)]));
    connection.push_fetch(Ok(vec![delivered(1, 2)]));
    connection.push_fetch(Ok(vec![delivered(1, 3)]));
    let intake = intake(Arc::clone(&connection), config(1, 1));

    let first = intake.next_batch().await.expect("first delivery");
    assert_eq!(intake.owned_count(), 1);
    assert!(
        intake
            .next_batch()
            .await
            .expect("duplicate redelivery")
            .commands
            .is_empty()
    );
    assert_eq!(intake.owned_count(), 1);
    assert!(naks(&connection).is_empty(), "the duplicate is not nak'd");

    drop(first);
    assert_eq!(intake.owned_count(), 0);
    let accepted_again = intake.next_batch().await.expect("later redelivery");
    assert_eq!(accepted_again.commands.len(), 1);
    assert_eq!(accepted_again.commands[0].coordinates().delivered, 3);
}

#[tokio::test]
async fn heartbeat_covers_queued_and_active_owners_until_processing_really_ends() {
    let connection = Arc::new(FakeConnection::new(2));
    connection.push_fetch(Ok(vec![delivery(2), delivery(1)]));
    let intake = intake(Arc::clone(&connection), config(1, 1));
    let mut batch = intake.next_batch().await.expect("owned deliveries");
    let mut processing = batch.commands.pop().expect("processing delivery");
    let queued = batch.commands.pop().expect("queued delivery");
    let processor = Arc::new(TestProcessor::new());
    let task = tokio::spawn({
        let processor = Arc::clone(&processor);
        async move {
            let verdict = processing.process(processor.as_ref()).await;
            (processing, verdict)
        }
    });
    processor.wait_started(1).await;

    assert_eq!(intake.heartbeat_owned().await.expect("+WPI round"), 2);
    assert_eq!(
        connection.operations().last(),
        Some(&FakeOperation::InProgress(vec![1, 2]))
    );

    drop(queued);
    processor.release(1);
    let (processing, verdict) = task.await.expect("processing task");
    assert_eq!(verdict.expect("processing"), DeliveryVerdict::Processed);
    assert_eq!(intake.owned_count(), 1, "the owner outlives the future");
    drop(processing);
    assert_eq!(intake.owned_count(), 0);
    assert_eq!(intake.heartbeat_owned().await.expect("empty round"), 0);
}

#[tokio::test]
async fn retryable_pull_failure_is_not_fatal_and_the_next_turn_pulls_again() {
    let connection = Arc::new(FakeConnection::new(1));
    connection.push_fetch(Err(NatsJetStreamError::unavailable(
        "the fake server is unavailable",
    )));
    connection.push_fetch(Ok(vec![delivery(9)]));
    let intake = intake(Arc::clone(&connection), config(1, 1));

    let error = intake.next_batch().await.err().expect("first pull fails");
    assert_eq!(error.kind(), NatsJetStreamErrorKind::DependencyUnavailable);
    assert!(!intake_failure_is_fatal(&error));
    assert_eq!(intake.owned_count(), 0, "a failed pull owns nothing");

    let next = intake.next_batch().await.expect("next turn");
    assert_eq!(next.commands.len(), 1);
    assert!(intake_failure_is_fatal(&NatsJetStreamError::protocol(
        "malformed server response"
    )));
    assert!(intake_failure_is_fatal(&NatsJetStreamError::configuration(
        "drifted consumer"
    )));
    assert!(intake_failure_is_fatal(&NatsJetStreamError::consumer_missing(
        "absent durable"
    )));
}

#[tokio::test]
async fn missing_durable_stops_before_processing_and_closes_transport() {
    let connection = Arc::new(FakeConnection::new(1));
    connection.push_fetch(Err(NatsJetStreamError::consumer_missing(
        "the NATS durable consumer is absent",
    )));
    let processor = Arc::new(TestProcessor::new());
    let runtime = CommandDeliveryRuntime::new(
        Arc::clone(&connection),
        Arc::clone(&processor),
        runtime_config(2, 2),
    );
    let (_stop, stopped) = watch::channel(false);
    let error = tokio::time::timeout(Duration::from_secs(1), runtime.run(stopped))
        .await
        .expect("a missing durable drains promptly")
        .expect_err("a missing durable remains fatal");
    assert_eq!(error.code(), "nats_jetstream.consumer_missing");
    assert!(!error.retryable());
    assert_eq!(processor.started.load(Ordering::Acquire), 0);
    assert_eq!(processor.completed.load(Ordering::Acquire), 0);
    let operations = connection.operations();
    assert_eq!(operations.last(), Some(&FakeOperation::Close));
    assert_eq!(
        operations
            .iter()
            .filter(|operation| matches!(operation, FakeOperation::Fetch { .. }))
            .count(),
        1
    );
}

#[tokio::test(start_paused = true)]
async fn runtime_stops_intake_but_heartbeats_until_owned_processing_drains() {
    let connection = Arc::new(FakeConnection::new(2));
    connection.push_fetch(Ok(vec![delivery(1)]));
    let processor = Arc::new(TestProcessor::new());
    let runtime = CommandDeliveryRuntime::new(
        Arc::clone(&connection),
        Arc::clone(&processor),
        runtime_config(1, 1),
    );
    let (stop, stopped) = watch::channel(false);
    let task = tokio::spawn(runtime.run(stopped));
    processor.wait_started(1).await;

    stop.send(true).expect("request runtime stop");
    tokio::task::yield_now().await;
    assert!(!task.is_finished());
    tokio::time::advance(Duration::from_secs(1)).await;
    connection
        .wait_for(|operations| {
            operations
                .iter()
                .any(|operation| operation == &FakeOperation::InProgress(vec![1]))
        })
        .await;

    processor.release(1);
    task.await
        .expect("runtime task")
        .expect("graceful runtime drain");
    assert_eq!(processor.active.load(Ordering::Acquire), 0);
    let operations = connection.operations();
    let fetches = operations
        .iter()
        .filter(|operation| matches!(operation, FakeOperation::Fetch { .. }))
        .count();
    assert_eq!(fetches, 2, "one pull served, one parked, none after Stop");
    // Work that ends during the drain still gets its disposition.
    assert_eq!(naks(&connection), [(1, RETRY_DELAY)]);
    assert_eq!(operations.last(), Some(&FakeOperation::Close));
}

#[tokio::test(start_paused = true)]
async fn dispositions_follow_the_contract_table() {
    let before = dead_lettered_total();
    let connection = Arc::new(FakeConnection::new(4));
    connection.push_fetch(Ok(vec![
        delivery(1),
        delivery(2),
        delivery(3),
        malformed_subject(4),
    ]));
    let processor = Arc::new(TestProcessor::scripted(BTreeMap::from([
        (1, Script::Retire),
        (2, Script::RetryLater),
        (3, Script::Poison(PoisonReason::SignatureInvalid)),
    ])));
    processor.release(3);
    let runtime = CommandDeliveryRuntime::new(
        Arc::clone(&connection),
        Arc::clone(&processor),
        runtime_config(3, 3),
    );
    let (stop, stopped) = watch::channel(false);
    let task = tokio::spawn(runtime.run(stopped));
    processor.wait_completed(3).await;
    connection
        .wait_for(|operations| {
            operations
                .iter()
                .filter(|operation| matches!(operation, FakeOperation::DeadLetter { .. }))
                .count()
                == 2
                && operations
                    .iter()
                    .any(|operation| matches!(operation, FakeOperation::Nak { sequence: 2, .. }))
        })
        .await;
    stop.send(true).expect("request runtime stop");
    task.await.expect("runtime task").expect("runtime drain");

    let mut naks = naks(&connection);
    naks.sort_unstable();
    assert_eq!(
        naks,
        [(2, RETRY_DELAY), (4, POISON_DELAY)],
        "retired: nothing; retry: retry delay; a poison that may still be served: 24h"
    );
    assert_eq!(
        terms(&connection),
        [3],
        "a signature that does not verify can never become valid: Term"
    );
    // The record is written before the answer.
    let operations = connection.operations();
    let recorded = operations
        .iter()
        .position(|operation| {
            matches!(
                operation,
                FakeOperation::DeadLetter {
                    reason: PoisonReason::SignatureInvalid,
                    ..
                }
            )
        })
        .expect("record");
    let terminated = operations
        .iter()
        .position(|operation| operation == &FakeOperation::Term(3))
        .expect("term");
    assert!(recorded < terminated, "dead-letter record first, then Term");
    let mut dead_letters = connection
        .operations()
        .into_iter()
        .filter_map(|operation| match operation {
            FakeOperation::DeadLetter { key, reason } => Some((key, reason)),
            _ => None,
        })
        .collect::<Vec<_>>();
    dead_letters.sort();
    let malformed_key = format!(
        "agent.{}",
        delivery_token("elitea.rt.v1.agent.d.not-a-token")
    );
    let mut expected = vec![
        (
            format!("agent.{}", delivery_token("poison-key")),
            PoisonReason::SignatureInvalid,
        ),
        (malformed_key, PoisonReason::MalformedSubject),
    ];
    expected.sort();
    assert_eq!(dead_letters, expected);
    assert!(
        !processor.sequences.lock().expect("sequences").contains(&4),
        "a transport poison never reaches the processor"
    );
    assert!(dead_lettered_total() >= before + 2);
}

#[tokio::test(start_paused = true)]
async fn a_failed_dead_letter_write_retries_the_poison_instead_of_parking_it() {
    let before = dead_letter_write_failed_total();
    let mut fake = FakeConnection::new(1);
    fake.dead_letter_fails = true;
    let connection = Arc::new(fake);
    connection.push_fetch(Ok(vec![malformed_subject(5)]));
    let processor = Arc::new(TestProcessor::new());
    let runtime = CommandDeliveryRuntime::new(
        Arc::clone(&connection),
        Arc::clone(&processor),
        runtime_config(1, 1),
    );
    let (stop, stopped) = watch::channel(false);
    let task = tokio::spawn(runtime.run(stopped));
    connection
        .wait_for(|operations| {
            operations
                .iter()
                .any(|operation| matches!(operation, FakeOperation::DeadLetter { .. }))
        })
        .await;
    stop.send(true).expect("request runtime stop");
    task.await.expect("runtime task").expect("runtime drain");
    assert_eq!(
        naks(&connection),
        [(5, RETRY_DELAY)],
        "no poison is parked without its record: the next delivery retries it"
    );
    assert!(terms(&connection).is_empty());
    assert!(dead_letter_write_failed_total() > before);
}

#[tokio::test(start_paused = true)]
async fn a_failed_dead_letter_write_never_terminates_the_poison() {
    let mut fake = FakeConnection::new(1);
    fake.dead_letter_fails = true;
    let connection = Arc::new(fake);
    connection.push_fetch(Ok(vec![delivery(6)]));
    let processor = Arc::new(TestProcessor::scripted(BTreeMap::from([(
        6,
        Script::Poison(PoisonReason::SubjectMismatch),
    )])));
    processor.release(1);
    let runtime = CommandDeliveryRuntime::new(
        Arc::clone(&connection),
        Arc::clone(&processor),
        runtime_config(1, 1),
    );
    let (stop, stopped) = watch::channel(false);
    let task = tokio::spawn(runtime.run(stopped));
    connection
        .wait_for(|operations| {
            operations
                .iter()
                .any(|operation| matches!(operation, FakeOperation::Nak { sequence: 6, .. }))
        })
        .await;
    stop.send(true).expect("request runtime stop");
    task.await.expect("runtime task").expect("runtime drain");
    assert_eq!(naks(&connection), [(6, RETRY_DELAY)]);
    assert!(terms(&connection).is_empty());
}

#[test]
fn every_verification_failure_is_poison_with_a_stable_reason() {
    assert_eq!(
        verification_poison(&ProtocolError::AuthorizationFailed("bad signature")),
        PoisonReason::SignatureInvalid
    );
    assert_eq!(
        verification_poison(&ProtocolError::UnsupportedCapability("unknown")),
        PoisonReason::UnsupportedCommand
    );
    for error in [
        ProtocolError::InvalidInput("decode"),
        ProtocolError::ResourceExhausted("bound"),
        ProtocolError::IncompatibleVersion("revision"),
    ] {
        assert_eq!(verification_poison(&error), PoisonReason::EnvelopeInvalid);
    }
}

#[tokio::test]
async fn runtime_worker_count_is_a_structural_processing_bound() {
    let connection = Arc::new(FakeConnection::new(4));
    connection.push_fetch(Ok(vec![delivery(1), delivery(2), delivery(3), delivery(4)]));
    let processor = Arc::new(TestProcessor::new());
    let runtime =
        CommandDeliveryRuntime::new(connection, Arc::clone(&processor), runtime_config(2, 2));
    let (stop, stopped) = watch::channel(false);
    let task = tokio::spawn(runtime.run(stopped));

    processor.wait_started(2).await;
    assert_eq!(processor.started.load(Ordering::Acquire), 2);
    assert_eq!(processor.maximum_active.load(Ordering::Acquire), 2);
    stop.send(true).expect("request runtime stop");
    processor.release(4);
    processor.wait_completed(2).await;
    task.await
        .expect("runtime task")
        .expect("bounded runtime drain");

    assert_eq!(processor.maximum_active.load(Ordering::Acquire), 2);
    // The two that never started were given back, not run.
    assert_eq!(processor.sequences.lock().expect("sequences").len(), 2);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn runtime_hands_an_enqueued_delivery_to_a_worker_while_intake_waits() {
    let connection = Arc::new(FakeConnection::new(1));
    connection.push_fetch(Ok(vec![delivery(1)]));
    let processor = Arc::new(TestProcessor::new());
    let runtime =
        CommandDeliveryRuntime::new(connection, Arc::clone(&processor), runtime_config(1, 1));
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
async fn runtime_pulls_again_only_after_the_stop_aware_dependency_delay() {
    let connection = Arc::new(FakeConnection::new(1));
    connection.push_fetch(Err(NatsJetStreamError::timeout("the fake pull timed out")));
    let processor = Arc::new(TestProcessor::new());
    let runtime = CommandDeliveryRuntime::new(
        Arc::clone(&connection),
        Arc::clone(&processor),
        runtime_config(1, 1),
    );
    let (stop, stopped) = watch::channel(false);
    let task = tokio::spawn(runtime.run(stopped));
    connection
        .wait_for(|operations| operations.len() == 1)
        .await;

    tokio::time::advance(Duration::from_millis(99)).await;
    tokio::task::yield_now().await;
    assert_eq!(connection.operations().len(), 1);
    connection.push_fetch(Ok(vec![delivery(9)]));
    tokio::time::advance(Duration::from_millis(1)).await;
    processor.wait_started(1).await;
    assert_eq!(
        processor.sequences.lock().expect("sequences").as_slice(),
        [9]
    );

    stop.send(true).expect("request runtime stop");
    processor.release(1);
    task.await
        .expect("runtime task")
        .expect("runtime after a retryable pull failure");
}

#[tokio::test(start_paused = true)]
async fn a_heartbeat_spanning_a_reconnect_is_retried_not_fatal() {
    let connection = Arc::new(FakeConnection::new(1));
    connection.push_fetch(Ok(vec![delivery(1)]));
    connection.push_heartbeat(Err(NatsJetStreamError::unavailable(
        "the heartbeat spanned a reconnect",
    )));
    let processor = Arc::new(TestProcessor::new());
    let runtime = CommandDeliveryRuntime::new(
        Arc::clone(&connection),
        Arc::clone(&processor),
        runtime_config(1, 1),
    );
    let (stop, stopped) = watch::channel(false);
    let task = tokio::spawn(runtime.run(stopped));
    processor.wait_started(1).await;
    tokio::time::advance(Duration::from_secs(1)).await;
    tokio::time::advance(Duration::from_secs(1)).await;
    connection
        .wait_for(|operations| {
            operations
                .iter()
                .filter(|operation| matches!(operation, FakeOperation::InProgress(_)))
                .count()
                >= 2
        })
        .await;
    assert!(!task.is_finished(), "an unconfirmed round is retried");
    stop.send(true).expect("request runtime stop");
    processor.release(1);
    task.await.expect("runtime task").expect("runtime drain");
}

#[tokio::test(start_paused = true)]
async fn drain_timeout_abandons_ownership_without_ack_or_nak() {
    let connection = Arc::new(FakeConnection::new(1));
    connection.push_fetch(Ok(vec![delivery(1)]));
    let processor = Arc::new(TestProcessor::new());
    let runtime = CommandDeliveryRuntime::new(
        Arc::clone(&connection),
        Arc::clone(&processor),
        runtime_config(1, 1),
    );
    let (stop, stopped) = watch::channel(false);
    let task = tokio::spawn(runtime.run(stopped));
    processor.wait_started(1).await;
    stop.send(true).expect("request runtime stop");
    tokio::task::yield_now().await;

    tokio::time::advance(Duration::from_secs(3)).await;
    let error = task
        .await
        .expect("runtime task")
        .expect_err("drain deadline must fail");
    assert_eq!(error.code(), "command_delivery.drain_timeout");
    assert!(error.retryable());
    assert_eq!(processor.completed.load(Ordering::Acquire), 0);
    assert_eq!(processor.active.load(Ordering::Acquire), 0);
    assert!(naks(&connection).is_empty(), "AckWait redelivers it");
    assert!(
        !connection
            .operations()
            .iter()
            .any(|operation| matches!(operation, FakeOperation::Retire(_)))
    );
}

#[tokio::test]
async fn cancelling_the_runtime_owner_cannot_detach_a_processing_task() {
    let connection = Arc::new(FakeConnection::new(1));
    connection.push_fetch(Ok(vec![delivery(1)]));
    let processor = Arc::new(TestProcessor::new());
    let runtime = CommandDeliveryRuntime::new(
        Arc::clone(&connection),
        Arc::clone(&processor),
        runtime_config(1, 1),
    );
    let (_stop, stopped) = watch::channel(false);
    let task = tokio::spawn(runtime.run(stopped));
    processor.wait_started(1).await;

    task.abort();
    task.await.expect_err("cancelled runtime owner");
    tokio::task::yield_now().await;
    assert_eq!(processor.active.load(Ordering::Acquire), 0);
    assert_eq!(processor.completed.load(Ordering::Acquire), 0);
    assert!(naks(&connection).is_empty());
}

#[test]
fn deployed_intake_bounds_fail_closed() {
    assert!(CommandDeliveryIntakeConfig::new(1, 1, 1, 100, 1_000, 1_000).is_ok());
    assert!(CommandDeliveryIntakeConfig::new(128, 512, 64, 30_000, 15_000, 300_000).is_ok());
    for (concurrency, queue, batch, expires, progress, retry) in [
        (0, 1, 1, 100, 1_000, 1_000),
        (2, 1, 1, 100, 1_000, 1_000),
        (1, 513, 1, 100, 1_000, 1_000),
        (1, 1, 0, 100, 1_000, 1_000),
        (1, 1, 65, 100, 1_000, 1_000),
        (1, 1, 1, 99, 1_000, 1_000),
        (1, 1, 1, 30_001, 1_000, 1_000),
        (1, 1, 1, 100, 999, 1_000),
        (1, 1, 1, 100, 15_001, 1_000),
        (1, 1, 1, 100, 1_000, 999),
        (1, 1, 1, 100, 1_000, 300_001),
    ] {
        assert!(
            CommandDeliveryIntakeConfig::new(concurrency, queue, batch, expires, progress, retry)
                .is_err()
        );
    }
    let intake =
        CommandDeliveryIntakeConfig::new(1, 1, 1, 100, 1_000, 1_000).expect("valid bounded intake");
    assert!(CommandDeliveryRuntimeConfig::new(intake, 100, 1_000).is_ok());
    assert!(CommandDeliveryRuntimeConfig::new(intake, 60_000, 300_000).is_ok());
    assert!(CommandDeliveryRuntimeConfig::new(intake, 99, 1_000).is_err());
    assert!(CommandDeliveryRuntimeConfig::new(intake, 100, 999).is_err());
}

fn terms(connection: &FakeConnection) -> Vec<u64> {
    connection
        .operations()
        .into_iter()
        .filter_map(|operation| match operation {
            FakeOperation::Term(sequence) => Some(sequence),
            _ => None,
        })
        .collect()
}

/// Every `+WPI` that reached the fake server AFTER the delayed nak of `sequence`.
fn wpi_after_nak(connection: &FakeConnection, sequence: u64) -> Vec<FakeOperation> {
    let operations = connection.operations();
    let Some(nak) = operations.iter().position(
        |operation| matches!(operation, FakeOperation::Nak { sequence: s, .. } if *s == sequence),
    ) else {
        return Vec::new();
    };
    operations[nak..]
        .iter()
        .filter(|operation| matches!(operation, FakeOperation::InProgress(sequences) if sequences.contains(&sequence)))
        .cloned()
        .collect()
}

/// D2: a heartbeat round paused mid-send while processing ends with a
/// delayed nak. Without the shared gate the nak reaches the server first and
/// the round's `+WPI` lands after it, resetting the redelivery timer to `AckWait`.
async fn no_wpi_follows_a_delayed_nak(script: Script, expected_delay: Duration) {
    let mut fake = FakeConnection::new(1);
    let pause = Arc::new(Semaphore::new(0));
    fake.heartbeat_pause = Some(Arc::clone(&pause));
    let connection = Arc::new(fake);
    let intake = Arc::new(intake(Arc::clone(&connection), config(1, 1)));
    connection.push_fetch(Ok(vec![delivery(7)]));
    let mut batch = intake.next_batch().await.expect("owned delivery");
    let mut owned = batch.commands.pop().expect("delivery");

    // A heartbeat round snapshots delivery 7 and pauses before its +WPI.
    let entered = connection.heartbeat_entered.notified();
    let round = tokio::spawn({
        let intake = Arc::clone(&intake);
        async move { intake.heartbeat_owned().await }
    });
    entered.await;

    // Processing ends; the disposition must not overtake the round.
    let processor = TestProcessor::scripted(BTreeMap::from([(7, script)]));
    processor.release(1);
    let verdict = owned.process(&processor).await.expect("processing");
    let dispose = tokio::spawn({
        let connection = Arc::clone(&connection);
        async move {
            let disposition = super::command_delivery::test_dispose_processed(
                connection.as_ref(),
                &mut owned,
                verdict,
                RETRY_DELAY,
            )
            .await;
            drop(owned);
            disposition
        }
    });
    for _ in 0..10 {
        tokio::task::yield_now().await;
    }
    assert!(
        naks(&connection).is_empty(),
        "the delayed nak waits for the round in flight"
    );
    pause.add_permits(1);
    round.await.expect("round").expect("+WPI round");
    dispose.await.expect("disposition");

    assert_eq!(naks(&connection), [(7, expected_delay)]);
    assert!(
        wpi_after_nak(&connection, 7).is_empty(),
        "a +WPI followed the delayed nak: {:?}",
        connection.operations()
    );
    // And the next round does not include it.
    pause.add_permits(1);
    assert_eq!(intake.heartbeat_owned().await.expect("empty round"), 0);
}

#[tokio::test]
async fn no_wpi_follows_a_retry_later_nak() {
    no_wpi_follows_a_delayed_nak(Script::RetryLater, RETRY_DELAY).await;
}

#[tokio::test]
async fn no_wpi_follows_a_poison_nak() {
    no_wpi_follows_a_delayed_nak(Script::Poison(PoisonReason::EnvelopeInvalid), POISON_DELAY).await;
}

/// A delayed answer stuck in its request/reply timeout must not hold the
/// heartbeat gate: a concurrent round for the other owned deliveries still
/// completes (and no longer includes the delivery being answered), or a slow
/// answer per delivery would starve every `+WPI` past `AckWait`.
#[tokio::test]
async fn a_slow_answer_does_not_block_a_concurrent_heartbeat_round() {
    let mut fake = FakeConnection::new(2);
    let answer_pause = Arc::new(Semaphore::new(0));
    fake.answer_pause = Some(Arc::clone(&answer_pause));
    let connection = Arc::new(fake);
    let intake = Arc::new(intake(Arc::clone(&connection), config(2, 2)));
    connection.push_fetch(Ok(vec![delivery(7), delivery(8)]));
    let mut batch = intake.next_batch().await.expect("owned deliveries");
    let other = batch.commands.pop().expect("delivery 8");
    let mut owned = batch.commands.pop().expect("delivery 7");
    assert_eq!(owned.coordinates().stream_sequence, 7);

    let processor = TestProcessor::scripted(BTreeMap::from([(7, Script::RetryLater)]));
    processor.release(1);
    let verdict = owned.process(&processor).await.expect("processing");
    let entered = connection.answer_entered.notified();
    let dispose = tokio::spawn({
        let connection = Arc::clone(&connection);
        async move {
            let disposition = super::command_delivery::test_dispose_processed(
                connection.as_ref(),
                &mut owned,
                verdict,
                RETRY_DELAY,
            )
            .await;
            drop(owned);
            disposition
        }
    });
    // The nak for 7 is now in flight and stuck.
    entered.await;

    let round = tokio::time::timeout(Duration::from_secs(5), intake.heartbeat_owned())
        .await
        .expect("the heartbeat round is not queued behind the slow answer")
        .expect("+WPI round");
    assert_eq!(round, 1);
    assert_eq!(
        connection
            .wpi_replies
            .lock()
            .expect("wpi replies")
            .last()
            .map(|replies| replies
                .iter()
                .map(|reply| sequence_of(reply))
                .collect::<Vec<_>>()),
        Some(vec![8]),
        "the round heartbeats the other delivery only"
    );
    assert!(naks(&connection).is_empty(), "the nak is still in flight");

    answer_pause.add_permits(1);
    assert_eq!(dispose.await.expect("disposition"), "retry_later");
    assert_eq!(naks(&connection), [(7, RETRY_DELAY)]);
    assert!(wpi_after_nak(&connection, 7).is_empty());
    drop(other);
}

#[tokio::test]
async fn the_newest_copy_of_an_owned_redelivery_is_heartbeated_and_answered() {
    let connection = Arc::new(FakeConnection::new(1));
    connection.push_fetch(Ok(vec![delivery(8)]));
    connection.push_fetch(Ok(vec![delivered(8, 2)]));
    let intake = intake(Arc::clone(&connection), config(1, 1));
    let mut first = intake.next_batch().await.expect("first copy");
    let mut owned = first.commands.pop().expect("owner");
    assert!(
        intake
            .next_batch()
            .await
            .expect("second copy")
            .commands
            .is_empty(),
        "the redelivery is not run twice"
    );
    intake.heartbeat_owned().await.expect("+WPI round");
    let rounds = connection.wpi_replies.lock().expect("wpi replies").clone();
    assert_eq!(
        rounds.last().map(|round| round
            .iter()
            .map(|reply| delivered_of(reply))
            .collect::<Vec<_>>()),
        Some(vec![2])
    );

    let processor = TestProcessor::new();
    processor.release(1);
    let verdict = owned.process(&processor).await.expect("processing");
    super::command_delivery::test_dispose_processed(
        connection.as_ref(),
        &mut owned,
        verdict,
        RETRY_DELAY,
    )
    .await;
    assert!(connection.operations().contains(&FakeOperation::Nak {
        sequence: 8,
        delivered: 2,
        delay: RETRY_DELAY
    }));
    assert_eq!(processor.started.load(Ordering::Acquire), 1);
}

#[tokio::test(start_paused = true)]
async fn queued_unstarted_deliveries_are_given_back_at_shutdown() {
    let connection = Arc::new(FakeConnection::new(4));
    connection.push_fetch(Ok(vec![delivery(1), delivery(2), delivery(3)]));
    let processor = Arc::new(TestProcessor::scripted(BTreeMap::from([(
        1,
        Script::Retire,
    )])));
    let runtime = CommandDeliveryRuntime::new(
        Arc::clone(&connection),
        Arc::clone(&processor),
        runtime_config(1, 2),
    );
    let (stop, stopped) = watch::channel(false);
    let task = tokio::spawn(runtime.run(stopped));
    processor.wait_started(1).await;
    stop.send(true).expect("request runtime stop");
    tokio::task::yield_now().await;
    processor.release(1);
    task.await.expect("runtime task").expect("runtime drain");

    assert_eq!(
        processor.sequences.lock().expect("sequences").as_slice(),
        [1],
        "nothing starts after Stop"
    );
    let mut naks = naks(&connection);
    naks.sort_unstable();
    assert_eq!(
        naks,
        [(2, Duration::ZERO), (3, Duration::ZERO)],
        "queued, never started: given back with no delay"
    );
}

#[tokio::test]
async fn a_replica_owns_its_concurrency_plus_one_pull_and_no_more() {
    // Concurrency 2, queue 8, pull 2: four owned at most, not ten.
    let connection = Arc::new(FakeConnection::new(2));
    let intake = Arc::new(CommandDeliveryIntake::new(
        Arc::clone(&connection),
        CommandDeliveryIntakeConfig::new(2, 8, 2, 30_000, 1_000, 60_000).expect("config"),
    ));
    connection.push_fetch(Ok(vec![delivery(1), delivery(2)]));
    connection.push_fetch(Ok(vec![delivery(3), delivery(4)]));
    let first = intake.next_batch().await.expect("first pull");
    let second = intake.next_batch().await.expect("second pull");
    assert_eq!(intake.owned_count(), 4);
    let waiting = tokio::spawn({
        let intake = Arc::clone(&intake);
        async move { intake.next_batch().await }
    });
    for _ in 0..10 {
        tokio::task::yield_now().await;
    }
    assert!(
        !waiting.is_finished(),
        "no pull beyond concurrency + one pull"
    );
    assert_eq!(
        connection
            .operations()
            .iter()
            .filter(|operation| matches!(operation, FakeOperation::Fetch { .. }))
            .count(),
        2
    );
    drop(first);
    drop(second);
    waiting.abort();
}
