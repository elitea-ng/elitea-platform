//! The worker's REAL command-bus transport against a secured nats-server that
//! runs the NATS chart's own rendered permission table and the real
//! bootstrap.sh (`libs/go/natsconn/natstest/cmd/natstest-serve`).
//!
//! `ELITEA_TEST_NATS_SECURE_ENV` names the environment document natstest-serve
//! writes. Unset, the test skips with a message; with
//! `ELITEA_REQUIRE_NATS_SECURE_TEST=1` (CI) a missing environment FAILS it,
//! because a permission test that skipped reads exactly like one that passed.
//!
//! One test function drives the whole scenario, so nothing else runs against
//! the shared durable at the same time:
//!
//! * elitea-main-runtime publishes five commands with the contract headers;
//! * the worker binds as elitea-worker — from its own WORKER account, through
//!   the `JS.RUNTIME.API` service imports — and runs the real delivery runtime;
//! * "ack" is double-acked (twice: the second is the idempotent
//!   "already acknowledged") and leaves the stream;
//! * "retry" is nak'd with the retry delay, redelivered, then acked;
//! * "poison" (an envelope that does not decode) is recorded in the
//!   dead-letter bucket, nak'd for 24h, and stays in the stream, undelivered;
//! * "forged" (a signature that does not verify) is recorded and TERMINATED:
//!   it leaves the stream and is never redelivered;
//! * "late" is published while the worker is idle-polling and starts well
//!   under the pull's expiry (a pull returns with its first message);
//! * "hold" is processed for longer than `AckWait` (60s) while the runtime sends
//!   `+WPI`; it is never redelivered, then acked;
//! * the server log shows no permission violation for the worker identity,
//!   and the log check is proven able to see one.

use std::collections::BTreeMap;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use async_nats::header::HeaderMap;
use async_nats::jetstream;
use async_trait::async_trait;
use tokio::sync::watch;
use tokio_stream::StreamExt as _;

use super::command_delivery::{
    CommandDeliveryIntakeConfig, CommandDeliveryProcessor, CommandDeliveryRuntime,
    CommandDeliveryRuntimeConfig,
};
use crate::transport::command_bus::{
    CONSUMER_AGENT, CommandBusLimits, CommandDelivery, CommandRetirementClient, DEAD_LETTER_BUCKET,
    DeliveryVerdict, HEADER_DELIVERY_ID, HEADER_MSG_ID, PoisonReason, STREAM_AGENT,
    delivery_subject, delivery_token,
};
use crate::transport::nats_jetstream::{
    NatsCommandBus, NatsJetStreamConfig, NatsJetStreamErrorKind, NatsTlsPaths, reloading_tls_config,
};

const ENV_FILE: &str = "ELITEA_TEST_NATS_SECURE_ENV";
const REQUIRE: &str = "ELITEA_REQUIRE_NATS_SECURE_TEST";
const WORKER: &str = "elitea-worker";
const MAIN_RUNTIME: &str = "elitea-main-runtime";
const BOOTSTRAP: &str = "elitea-nats-bootstrap-runtime";
const WORKER_BOOTSTRAP: &str = "elitea-nats-bootstrap-worker";
/// The pull expiry the live worker uses: the deployed 5s, so "starts well
/// under it" is a real latency bound.
const FETCH_EXPIRES: Duration = Duration::from_secs(5);
const HOLD_FOR: Duration = Duration::from_secs(66);

struct Identity {
    tls: NatsTlsPaths,
    inbox_prefix: String,
    user: String,
}

struct Environment {
    url: String,
    log: PathBuf,
    identities: BTreeMap<String, Identity>,
}

/// natstest's paths as written: on macOS they run through the `/var` symlink,
/// like a cert-manager `..data` mount does in a pod.
fn material(path: &str) -> PathBuf {
    PathBuf::from(path)
}

fn environment() -> Option<Environment> {
    let required = std::env::var(REQUIRE).is_ok_and(|value| value == "1");
    let path = match std::env::var(ENV_FILE) {
        Ok(path) if !path.is_empty() => path,
        _ => {
            assert!(
                !required,
                "{REQUIRE}=1 but {ENV_FILE} is not set: start natstest-serve \
                 (libs/go/natsconn/natstest/cmd/natstest-serve) and point {ENV_FILE} at its -out file"
            );
            eprintln!(
                "skipping the live NATS command-bus test: {ENV_FILE} is not set \
                 (set {REQUIRE}=1 to make this a failure)"
            );
            return None;
        }
    };
    let raw = std::fs::read(&path).unwrap_or_else(|error| panic!("{ENV_FILE}={path}: {error}"));
    let value: serde_json::Value = serde_json::from_slice(&raw).expect("natstest environment JSON");
    let text = |value: &serde_json::Value, key: &str| -> String {
        value[key]
            .as_str()
            .unwrap_or_else(|| panic!("natstest environment lacks {key}"))
            .to_owned()
    };
    let mut identities = BTreeMap::new();
    for (name, identity) in value["identities"]
        .as_object()
        .expect("natstest identities")
    {
        identities.insert(
            name.clone(),
            Identity {
                tls: NatsTlsPaths {
                    ca_path: material(&text(identity, "ca")),
                    certificate_path: material(&text(identity, "cert")),
                    private_key_path: material(&text(identity, "key")),
                },
                inbox_prefix: text(identity, "inbox_prefix"),
                user: text(identity, "user"),
            },
        );
    }
    Some(Environment {
        url: text(&value, "url"),
        log: PathBuf::from(text(&value, "log")),
        identities,
    })
}

impl Environment {
    fn identity(&self, name: &str) -> &Identity {
        self.identities
            .get(name)
            .unwrap_or_else(|| panic!("natstest identity {name}"))
    }

    async fn client(&self, name: &str) -> async_nats::Client {
        let identity = self.identity(name);
        async_nats::ConnectOptions::new()
            .name(format!("rust-live-test-{name}"))
            .custom_inbox_prefix(identity.inbox_prefix.clone())
            .require_tls(true)
            .tls_client_config(reloading_tls_config(identity.tls.clone()).expect("TLS config"))
            .connection_timeout(Duration::from_secs(5))
            .connect(self.url.as_str())
            .await
            .unwrap_or_else(|error| panic!("connect as {name}: {error}"))
    }

    fn worker_config(&self, stream: &str, consumer: &str) -> NatsJetStreamConfig {
        NatsJetStreamConfig {
            url: self.url.clone(),
            tls: Some(self.identity(WORKER).tls.clone()),
            stream: stream.to_owned(),
            consumer: consumer.to_owned(),
            client_name: "rust-live-test-worker".to_owned(),
            limits: CommandBusLimits::runtime_v1(),
            fetch_batch: 8,
            fetch_expires: FETCH_EXPIRES,
            connection_timeout: Duration::from_secs(5),
            request_timeout: Duration::from_secs(5),
        }
    }

    fn violations(&self, account: &str, user: &str) -> Vec<String> {
        let log = std::fs::read_to_string(&self.log).expect("nats-server log");
        // The server prefixes the mapped user with its account: the producer
        // is in RUNTIME, the worker in WORKER (deploy/helm/nats/values.yaml).
        let marker = format!("{account}/user:{user}\"");
        log.lines()
            .filter(|line| {
                line.contains(&marker)
                    && (line.contains(" - Publish Violation - ")
                        || line.contains(" - Subscription Violation - "))
            })
            .map(str::to_owned)
            .collect()
    }
}

/// What each command's processing does, keyed by delivery ID.
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
enum Behaviour {
    Ack,
    RetryOnce,
    Poison,
    Forged,
    Hold,
    Late,
}

struct LiveProcessor {
    bus: Arc<NatsCommandBus>,
    behaviours: BTreeMap<String, (String, Behaviour)>,
    seen: Mutex<BTreeMap<String, Vec<u64>>>,
    started_at: Mutex<BTreeMap<String, Instant>>,
    double_ack_repeats: Mutex<Vec<bool>>,
}

impl LiveProcessor {
    async fn retire(&self, delivery: &CommandDelivery, repeat: bool) -> DeliveryVerdict {
        let confirmed = self
            .bus
            .retire_delivery(delivery.test_retirement_request())
            .await;
        assert!(confirmed.is_ok(), "double ack: {confirmed:?}");
        if repeat {
            let again = self
                .bus
                .retire_delivery(delivery.test_retirement_request())
                .await;
            self.double_ack_repeats
                .lock()
                .expect("repeats")
                .push(again.is_ok());
        }
        delivery.settlement().test_mark_retired();
        DeliveryVerdict::Processed
    }
}

#[async_trait]
impl CommandDeliveryProcessor for LiveProcessor {
    async fn process(&self, delivery: CommandDelivery) -> DeliveryVerdict {
        let Some((name, behaviour)) = self.behaviours.get(delivery.delivery_token()).cloned()
        else {
            // A leftover of an earlier run on the same server: leave it.
            return DeliveryVerdict::Processed;
        };
        assert_eq!(
            delivery.signed_envelope(),
            name.as_bytes(),
            "the body is the published bytes"
        );
        self.started_at
            .lock()
            .expect("started at")
            .entry(name.clone())
            .or_insert_with(Instant::now);
        self.seen
            .lock()
            .expect("seen")
            .entry(name)
            .or_default()
            .push(delivery.delivered());
        match behaviour {
            Behaviour::Ack => self.retire(&delivery, true).await,
            Behaviour::Forged => DeliveryVerdict::poison(PoisonReason::SignatureInvalid),
            Behaviour::RetryOnce if delivery.delivered() == 1 => DeliveryVerdict::Processed,
            Behaviour::RetryOnce | Behaviour::Late => self.retire(&delivery, false).await,
            Behaviour::Poison => DeliveryVerdict::poison(PoisonReason::EnvelopeInvalid),
            Behaviour::Hold => {
                tokio::time::sleep(HOLD_FOR).await;
                self.retire(&delivery, false).await
            }
        }
    }
}

async fn subject_count(stream: &jetstream::stream::Stream, subject: &str) -> usize {
    let mut info = stream
        .info_with_subjects(subject)
        .await
        .expect("stream info with subjects");
    let mut total = 0;
    while let Some(entry) = info.next().await {
        let (found, count) = entry.expect("subject count");
        if found == subject {
            total += count;
        }
    }
    total
}

async fn wait_until<F, Fut>(what: &str, deadline: Duration, mut check: F)
where
    F: FnMut() -> Fut,
    Fut: std::future::Future<Output = bool>,
{
    let started = Instant::now();
    while !check().await {
        assert!(started.elapsed() < deadline, "timed out waiting for {what}");
        tokio::time::sleep(Duration::from_millis(250)).await;
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[allow(clippy::too_many_lines)] // One scenario on one shared durable, by design.
async fn the_real_worker_transport_runs_on_the_chart_permission_table() {
    let Some(env) = environment() else {
        return;
    };
    assert!(env.log.is_file(), "the nats-server log is readable");
    let run = format!(
        "{}-{}",
        std::process::id(),
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .expect("clock")
            .as_nanos()
    );

    // A pairing outside the contract is refused before any connection.
    let refused =
        NatsCommandBus::connect(env.worker_config(STREAM_AGENT, "elitea-index-worker-v1"))
            .await
            .err()
            .expect("a mismatched stream/durable pairing is refused");
    assert_eq!(refused.kind(), NatsJetStreamErrorKind::Configuration);

    // The producer: elitea-main-runtime, with the contract headers.
    let producer = env.client(MAIN_RUNTIME).await;
    let producer_js = jetstream::new(producer.clone());
    let mut behaviours = BTreeMap::new();
    let mut subjects = BTreeMap::new();
    let publish = |delivery_id: String| {
        let producer_js = producer_js.clone();
        async move {
            let token = delivery_token(&delivery_id);
            let subject = delivery_subject(STREAM_AGENT, &delivery_id).expect("subject");
            let mut headers = HeaderMap::new();
            headers.insert(HEADER_MSG_ID, token.as_str());
            headers.insert(HEADER_DELIVERY_ID, delivery_id.as_str());
            headers.insert("Nats-Expected-Stream", STREAM_AGENT);
            producer_js
                .publish_with_headers(subject.clone(), headers, delivery_id.clone().into())
                .await
                .expect("publish")
                .await
                .expect("PubAck");
            (token, subject)
        }
    };
    for behaviour in [
        Behaviour::Ack,
        Behaviour::RetryOnce,
        Behaviour::Poison,
        Behaviour::Forged,
        Behaviour::Hold,
        Behaviour::Late,
    ] {
        let delivery_id = format!("rust-live-{run}-{behaviour:?}");
        let token = delivery_token(&delivery_id);
        let subject = delivery_subject(STREAM_AGENT, &delivery_id).expect("subject");
        behaviours.insert(token.clone(), (delivery_id.clone(), behaviour));
        subjects.insert(behaviour, (delivery_id, token, subject));
    }
    for behaviour in [
        Behaviour::Ack,
        Behaviour::RetryOnce,
        Behaviour::Poison,
        Behaviour::Forged,
        Behaviour::Hold,
    ] {
        publish(subjects[&behaviour].0.clone()).await;
    }

    // The observers: RUNTIME's bootstrap identity for the command stream, and
    // WORKER's for the dead-letter bucket (stream and consumer info only).
    let observer = env.client(BOOTSTRAP).await;
    let observer_js = jetstream::new(observer.clone());
    let agent_stream = observer_js.get_stream(STREAM_AGENT).await.expect("stream");
    let worker_observer = env.client(WORKER_BOOTSTRAP).await;
    let dead_letters = jetstream::new(worker_observer.clone())
        .get_stream(format!("KV_{DEAD_LETTER_BUCKET}"))
        .await
        .expect("dead-letter bucket stream in WORKER");
    assert!(
        observer_js
            .get_stream(format!("KV_{DEAD_LETTER_BUCKET}"))
            .await
            .is_err(),
        "RUNTIME holds no dead-letter bucket"
    );

    // The worker: the real transport and the real delivery runtime.
    let bus = Arc::new(
        NatsCommandBus::connect(env.worker_config(STREAM_AGENT, CONSUMER_AGENT))
            .await
            .expect("the worker binds the bootstrapped durable"),
    );
    let processor = Arc::new(LiveProcessor {
        bus: Arc::clone(&bus),
        behaviours,
        seen: Mutex::new(BTreeMap::new()),
        started_at: Mutex::new(BTreeMap::new()),
        double_ack_repeats: Mutex::new(Vec::new()),
    });
    let intake = CommandDeliveryIntakeConfig::new(
        4,
        4,
        8,
        u64::try_from(FETCH_EXPIRES.as_millis()).expect("expires"),
        1_000,
        1_000,
    )
    .expect("live intake config");
    let runtime = CommandDeliveryRuntime::new(
        Arc::clone(&bus),
        Arc::clone(&processor),
        CommandDeliveryRuntimeConfig::new(intake, 250, 30_000).expect("live runtime config"),
    );
    let (stop, stopped) = watch::channel(false);
    let running = tokio::spawn(runtime.run(stopped));

    let seen = |name: &str| -> Vec<u64> {
        processor
            .seen
            .lock()
            .expect("seen")
            .get(name)
            .cloned()
            .unwrap_or_default()
    };
    let (ack_id, _, ack_subject) = subjects[&Behaviour::Ack].clone();
    let (retry_id, _, retry_subject) = subjects[&Behaviour::RetryOnce].clone();
    let (poison_id, poison_token, poison_subject) = subjects[&Behaviour::Poison].clone();
    let (hold_id, _, hold_subject) = subjects[&Behaviour::Hold].clone();
    let (forged_id, forged_token, forged_subject) = subjects[&Behaviour::Forged].clone();
    let (late_id, _, late_subject) = subjects[&Behaviour::Late].clone();

    // Fetch + double ack removes the message from the WorkQueue stream.
    wait_until(
        "the acked command to leave the stream",
        Duration::from_secs(20),
        || {
            let stream = agent_stream.clone();
            let subject = ack_subject.clone();
            async move { subject_count(&stream, &subject).await == 0 }
        },
    )
    .await;
    assert_eq!(seen(&ack_id), [1]);
    // The stream empties on the FIRST double ack; the repeat is recorded only
    // when its own answer arrives. Asserting right after the stream emptied
    // raced it (CI run 37433037989 read an empty list here).
    wait_until(
        "the repeated double ack to be answered",
        Duration::from_secs(10),
        || {
            let answered = !processor
                .double_ack_repeats
                .lock()
                .expect("repeats")
                .is_empty();
            async move { answered }
        },
    )
    .await;
    assert_eq!(
        processor
            .double_ack_repeats
            .lock()
            .expect("repeats")
            .as_slice(),
        [true],
        "a repeated double ack is confirmed (already acknowledged)"
    );

    // Nak with the retry delay redelivers later; the second delivery acks.
    wait_until(
        "the retried command to be redelivered and acked",
        Duration::from_secs(20),
        || {
            let stream = agent_stream.clone();
            let subject = retry_subject.clone();
            async move { subject_count(&stream, &subject).await == 0 }
        },
    )
    .await;
    assert_eq!(seen(&retry_id), [1, 2]);

    // Poison: dead-letter record present, message still in the stream.
    let dead_letter_subject = format!("$KV.{DEAD_LETTER_BUCKET}.agent.{poison_token}");
    wait_until("the dead-letter record", Duration::from_secs(20), || {
        let stream = dead_letters.clone();
        let subject = dead_letter_subject.clone();
        async move { subject_count(&stream, &subject).await == 1 }
    })
    .await;
    assert_eq!(subject_count(&agent_stream, &poison_subject).await, 1);
    assert_eq!(seen(&poison_id), [1]);

    // Forged: recorded, then terminated — it leaves the WorkQueue stream.
    let forged_record = format!("$KV.{DEAD_LETTER_BUCKET}.agent.{forged_token}");
    wait_until(
        "the forged command's record",
        Duration::from_secs(20),
        || {
            let stream = dead_letters.clone();
            let subject = forged_record.clone();
            async move { subject_count(&stream, &subject).await == 1 }
        },
    )
    .await;
    wait_until(
        "the terminated command to leave the stream",
        Duration::from_secs(20),
        || {
            let stream = agent_stream.clone();
            let subject = forged_subject.clone();
            async move { subject_count(&stream, &subject).await == 0 }
        },
    )
    .await;
    assert_eq!(seen(&forged_id), [1]);

    // Late: published while the worker idles in a long poll, it starts well
    // under the pull's expiry rather than waiting it out.
    tokio::time::sleep(Duration::from_millis(500)).await;
    let published = Instant::now();
    let _ = publish(late_id.clone()).await;
    wait_until(
        "the late command to be acked",
        Duration::from_secs(20),
        || {
            let stream = agent_stream.clone();
            let subject = late_subject.clone();
            async move { subject_count(&stream, &subject).await == 0 }
        },
    )
    .await;
    let started = processor.started_at.lock().expect("started at")[&late_id];
    let latency = started.saturating_duration_since(published);
    assert!(
        latency < FETCH_EXPIRES / 2,
        "a command published to an idle worker started after {latency:?}; the pull held it"
    );

    // +WPI keeps the held command owned past AckWait: no redelivery.
    wait_until(
        "the held command to finish",
        HOLD_FOR + Duration::from_secs(30),
        || {
            let stream = agent_stream.clone();
            let subject = hold_subject.clone();
            async move { subject_count(&stream, &subject).await == 0 }
        },
    )
    .await;
    assert_eq!(seen(&hold_id), [1], "+WPI suppressed every redelivery");
    assert_eq!(seen(&poison_id), [1], "the 24h nak holds the poison back");
    assert_eq!(
        seen(&forged_id),
        [1],
        "a terminated command is never redelivered"
    );
    assert_eq!(subject_count(&agent_stream, &poison_subject).await, 1);
    let mut consumer: jetstream::consumer::PullConsumer = agent_stream
        .get_consumer(CONSUMER_AGENT)
        .await
        .expect("consumer");
    let info = consumer.info().await.expect("consumer info");
    assert!(info.num_ack_pending >= 1, "the poison stays pending");

    stop.send(true).expect("stop the runtime");
    running
        .await
        .expect("runtime task")
        .expect("the runtime drains cleanly");

    // The log check can see a violation: the producer may not touch the
    // dead-letter bucket, and the server logs that.
    producer
        .publish(format!("$KV.{DEAD_LETTER_BUCKET}.probe"), "x".into())
        .await
        .expect("publish probe");
    producer.flush().await.expect("flush probe");
    let producer_user = env.identity(MAIN_RUNTIME).user.clone();
    wait_until(
        "the server to log the probe violation",
        Duration::from_secs(10),
        || {
            let found = !env.violations("RUNTIME", &producer_user).is_empty();
            async move { found }
        },
    )
    .await;
    let worker_violations = env.violations("WORKER", &env.identity(WORKER).user);
    assert!(
        worker_violations.is_empty(),
        "the worker hit permission violations:\n{}",
        worker_violations.join("\n")
    );
    let _ = producer.drain().await;
    let _ = observer.drain().await;
    let _ = worker_observer.drain().await;
}
