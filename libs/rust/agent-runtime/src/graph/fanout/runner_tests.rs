//! Runner unit tests: in-memory children and an in-memory ledger. Concurrency
//! is driven by gates (no sleeps); tick timing uses paused Tokio time.

use std::collections::BTreeMap;
use std::sync::Mutex;
use std::sync::atomic::AtomicUsize;

use futures::FutureExt;
use serde_json::{Map, json};
use tokio::sync::Semaphore;

use super::*;
use crate::graph::fanout::plan::ChildFailure;

/// What a child does on each run, in order. The last script entry repeats.
#[derive(Clone, Debug)]
enum Step {
    Complete,
    Fail,
    /// Pause with these interrupt ids (one card each).
    Pause(Vec<&'static str>),
    /// Wait on the child's gate, then run the next step.
    Gate,
    LeaseLost,
    /// Ignore the stop latch forever.
    Hang,
}

#[derive(Default)]
struct ChildBook {
    runs: usize,
    /// Decisions each run received.
    received: Vec<Vec<String>>,
    /// Latest checkpoint view and its counter.
    latest: Option<ChildCheckpointView>,
    saves: usize,
    terminal: Option<ChildTerminal>,
}

struct FakeChildren {
    members: Vec<(String, String)>,
    scripts: Vec<Vec<Step>>,
    books: Arc<Vec<Mutex<ChildBook>>>,
    gates: Vec<Arc<Semaphore>>,
    restored: Mutex<Option<Vec<RestoredChild>>>,
    running_now: Arc<AtomicUsize>,
    running_peak: Arc<AtomicUsize>,
    stop: Arc<FanoutStop>,
    /// Earlier pauses: key -> child.
    answered: Arc<Mutex<BTreeMap<String, usize>>>,
}

impl FakeChildren {
    fn new(scripts: Vec<Vec<Step>>, stop: &Arc<FanoutStop>) -> Arc<Self> {
        let count = scripts.len();
        Arc::new(Self {
            members: (0..count)
                .map(|child| (format!("b{child}"), format!("node{child}")))
                .collect(),
            scripts,
            books: Arc::new(
                (0..count)
                    .map(|_| Mutex::new(ChildBook::default()))
                    .collect(),
            ),
            gates: (0..count).map(|_| Arc::new(Semaphore::new(0))).collect(),
            restored: Mutex::new(None),
            running_now: Arc::new(AtomicUsize::new(0)),
            running_peak: Arc::new(AtomicUsize::new(0)),
            stop: Arc::clone(stop),
            answered: Arc::new(Mutex::new(BTreeMap::new())),
        })
    }

    fn runs(&self, child: usize) -> usize {
        self.books[child].lock().expect("book").runs
    }

    fn received(&self, child: usize) -> Vec<Vec<String>> {
        self.books[child].lock().expect("book").received.clone()
    }

    fn open(&self, child: usize) -> Vec<CardIdentity> {
        match self.books[child].lock().expect("book").latest.clone() {
            Some(ChildCheckpointView::Paused {
                checkpoint_id,
                interrupt_ids,
            }) => interrupt_ids
                .iter()
                .map(|id| CardIdentity {
                    interrupt_key: format!("ik-{child}-{checkpoint_id}-{id}"),
                    interrupt_id: id.clone(),
                    child_paused_checkpoint_id: checkpoint_id.clone(),
                })
                .collect(),
            _ => Vec::new(),
        }
    }

    /// Snapshot for a "process replacement": the next runner restores from it.
    fn snapshot(&self) -> Vec<RestoredChild> {
        (0..self.members.len())
            .map(|child| {
                let book = self.books[child].lock().expect("book");
                match (&book.terminal, &book.latest) {
                    (Some(terminal), _) => RestoredChild::Settled(terminal.clone()),
                    (None, Some(ChildCheckpointView::Paused { checkpoint_id, .. })) => {
                        let checkpoint_id = checkpoint_id.clone();
                        drop(book);
                        RestoredChild::Paused {
                            checkpoint_id,
                            cards: self.open(child),
                        }
                    }
                    _ => RestoredChild::Pending,
                }
            })
            .collect()
    }

    fn restore_from(&self, snapshot: Vec<RestoredChild>) {
        *self.restored.lock().expect("restored") = Some(snapshot);
    }
}

#[async_trait]
impl FanoutChildren for FakeChildren {
    fn members(&self) -> &[(String, String)] {
        &self.members
    }

    async fn restore(&self) -> Result<Vec<RestoredChild>, FanoutRunError> {
        Ok(self
            .restored
            .lock()
            .expect("restored")
            .take()
            .unwrap_or_else(|| vec![RestoredChild::Pending; self.members.len()]))
    }

    async fn latest(&self, child: usize) -> Result<ChildCheckpointView, FanoutRunError> {
        Ok(self.books[child]
            .lock()
            .expect("book")
            .latest
            .clone()
            .unwrap_or(ChildCheckpointView::Running {
                checkpoint_id: None,
            }))
    }

    async fn answered_pause(
        &self,
        key: &str,
    ) -> Result<Option<(usize, ChildCheckpointView)>, FanoutRunError> {
        let child = self.answered.lock().expect("answered").get(key).copied();
        match child {
            Some(child) => Ok(Some((child, self.latest(child).await?))),
            None => Ok(None),
        }
    }

    #[allow(clippy::too_many_lines)] // One scripted fake child.
    fn run(&self, child: usize, decisions: Vec<FetchedDecision>) -> BoxFuture<'static, ChildRun> {
        let script = self.scripts[child].clone();
        let gate = Arc::clone(&self.gates[child]);
        let now = Arc::clone(&self.running_now);
        let peak = Arc::clone(&self.running_peak);
        let stop = Arc::clone(&self.stop);
        // Record the run synchronously; the step runs in the future.
        let index = {
            let mut book = self.books[child].lock().expect("book");
            book.runs += 1;
            book.received.push(
                decisions
                    .iter()
                    .map(|decision| decision.interrupt_key.clone())
                    .collect(),
            );
            // A resume answers the pause it leaves: that key stays known.
            if let Some(ChildCheckpointView::Paused {
                checkpoint_id,
                interrupt_ids,
            }) = book.latest.clone()
            {
                for id in interrupt_ids {
                    self.answered
                        .lock()
                        .expect("answered")
                        .insert(format!("ik-{child}-{checkpoint_id}-{id}"), child);
                }
            }
            book.latest = Some(ChildCheckpointView::Running {
                checkpoint_id: None,
            });
            book.runs - 1
        };
        let books = Arc::clone(&self.books);
        async move {
            let running = now.fetch_add(1, Ordering::SeqCst) + 1;
            peak.fetch_max(running, Ordering::SeqCst);
            let mut position = index.min(script.len() - 1);
            let result = loop {
                match &script[position] {
                    Step::Gate => {
                        tokio::select! {
                            permit = gate.acquire() => { permit.expect("gate").forget(); }
                            () = stop.fired() => break ChildRun::Cancelled,
                        }
                        position = (position + 1).min(script.len() - 1);
                    }
                    Step::Hang => std::future::pending::<()>().await,
                    Step::LeaseLost => break ChildRun::LeaseLost,
                    Step::Complete | Step::Fail | Step::Pause(_) => {
                        let mut book = books[child].lock().expect("book");
                        book.saves += 1;
                        let checkpoint_id = format!("{}", book.saves);
                        let run = match &script[position] {
                            Step::Complete | Step::Fail => {
                                let terminal = if matches!(script[position], Step::Fail) {
                                    ChildTerminal::Failed(ChildFailure::new(
                                        "graph.fanout.child_failed",
                                        "The step failed.",
                                    ))
                                } else {
                                    let mut outputs = Map::new();
                                    outputs.insert("child".to_owned(), json!(child));
                                    ChildTerminal::Succeeded(outputs)
                                };
                                book.terminal = Some(terminal.clone());
                                book.latest = Some(ChildCheckpointView::Terminal {
                                    checkpoint_id: checkpoint_id.clone(),
                                });
                                ChildRun::Settled {
                                    terminal,
                                    checkpoint_id,
                                }
                            }
                            Step::Pause(ids) => {
                                book.latest = Some(ChildCheckpointView::Paused {
                                    checkpoint_id: checkpoint_id.clone(),
                                    interrupt_ids: ids.iter().map(|id| (*id).to_owned()).collect(),
                                });
                                ChildRun::Paused {
                                    cards: ids
                                        .iter()
                                        .map(|id| CardIdentity {
                                            interrupt_key: format!(
                                                "ik-{child}-{checkpoint_id}-{id}"
                                            ),
                                            interrupt_id: (*id).to_owned(),
                                            child_paused_checkpoint_id: checkpoint_id.clone(),
                                        })
                                        .collect(),
                                    checkpoint_id,
                                }
                            }
                            _ => unreachable!(),
                        };
                        break run;
                    }
                }
            };
            now.fetch_sub(1, Ordering::SeqCst);
            result
        }
        .boxed()
    }
}

/// Main's ledger: decided rows until acknowledged.
#[derive(Default)]
struct FakeLedger {
    decided: Mutex<Vec<FetchedDecision>>,
    acks: Mutex<Vec<(String, AckResult)>>,
    fetches: AtomicUsize,
    fetch_times: Mutex<Vec<Instant>>,
    lease_lost: AtomicBool,
    revision: AtomicUsize,
}

impl FakeLedger {
    fn decide(&self, key: &str) {
        let revision = self.revision.fetch_add(1, Ordering::SeqCst) + 1;
        self.decided.lock().expect("decided").push(FetchedDecision {
            interrupt_key: key.to_owned(),
            request_id: format!("req-{key}"),
            revision: revision as u64,
            decision_sha256: "0".repeat(64),
            action: "approve".to_owned(),
            value: None,
            credential_ref: None,
        });
    }

    fn decide_action(&self, key: &str, action: &str) {
        self.decide(key);
        if let Some(last) = self.decided.lock().expect("decided").last_mut() {
            action.clone_into(&mut last.action);
        }
    }

    fn acks(&self) -> Vec<(String, AckResult)> {
        self.acks.lock().expect("acks").clone()
    }

    fn fetches(&self) -> usize {
        self.fetches.load(Ordering::SeqCst)
    }
}

#[async_trait]
impl DecisionSource for FakeLedger {
    async fn fetch(&self) -> Result<DecisionBatch, DecisionError> {
        self.fetches.fetch_add(1, Ordering::SeqCst);
        self.fetch_times.lock().expect("times").push(Instant::now());
        if self.lease_lost.load(Ordering::SeqCst) {
            return Err(DecisionError::LeaseLost);
        }
        Ok(DecisionBatch {
            decisions: self.decided.lock().expect("decided").clone(),
            decision_revision: self.revision.load(Ordering::SeqCst) as u64,
        })
    }

    async fn ack(
        &self,
        decision: &FetchedDecision,
        result: &AckResult,
    ) -> Result<(), DecisionError> {
        self.decided
            .lock()
            .expect("decided")
            .retain(|row| row.interrupt_key != decision.interrupt_key);
        self.acks
            .lock()
            .expect("acks")
            .push((decision.interrupt_key.clone(), result.clone()));
        Ok(())
    }
}

struct Harness {
    children: Arc<FakeChildren>,
    ledger: Arc<FakeLedger>,
    stop: Arc<FanoutStop>,
}

impl Harness {
    fn new(scripts: Vec<Vec<Step>>) -> Self {
        let stop = Arc::new(FanoutStop::default());
        Self {
            children: FakeChildren::new(scripts, &stop),
            ledger: Arc::new(FakeLedger::default()),
            stop,
        }
    }

    fn runner(&self, max_concurrency: usize, on_error: OnError) -> FanoutRunner {
        self.runner_with(max_concurrency, on_error, None, ChildPermits::default())
    }

    fn runner_with(
        &self,
        max_concurrency: usize,
        on_error: OnError,
        deadline: Option<Instant>,
        permits: ChildPermits,
    ) -> FanoutRunner {
        FanoutRunner::new(
            self.children.clone(),
            self.ledger.clone(),
            Arc::new(NoEvents),
            permits,
            Arc::clone(&self.stop),
            FanoutRunnerConfig {
                max_concurrency,
                on_error,
                deadline,
            },
        )
        .expect("valid runner")
    }

    /// A new process: a fresh stop latch, restored from the fake checkpoints.
    fn replace_process(&mut self) {
        let snapshot = self.children.snapshot();
        self.children.restore_from(snapshot);
    }
}

fn parked(outcome: &RunnerOutcome) -> Vec<String> {
    match outcome {
        RunnerOutcome::Parked { open, .. } => {
            open.iter().map(|card| card.interrupt_key.clone()).collect()
        }
        RunnerOutcome::Settled(_) => panic!("expected a park, got {outcome:?}"),
    }
}

fn settled(outcome: RunnerOutcome) -> Vec<SettledChild> {
    match outcome {
        RunnerOutcome::Settled(children) => children,
        RunnerOutcome::Parked { .. } => panic!("expected settled children"),
    }
}

fn three_asking() -> Vec<Vec<Step>> {
    vec![vec![Step::Pause(vec!["ask"]), Step::Complete]; 3]
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn answering_card_two_first_resumes_only_child_two() {
    let mut harness = Harness::new(three_asking());
    let first = harness
        .runner(3, OnError::Continue)
        .run()
        .await
        .expect("run");
    assert_eq!(
        parked(&first),
        vec!["ik-0-1-ask", "ik-1-1-ask", "ik-2-1-ask"]
    );
    harness.ledger.decide("ik-1-1-ask");
    harness.replace_process();
    let second = harness
        .runner(3, OnError::Continue)
        .run()
        .await
        .expect("wake");
    assert_eq!(parked(&second), vec!["ik-0-1-ask", "ik-2-1-ask"]);
    assert_eq!(
        (
            harness.children.runs(0),
            harness.children.runs(1),
            harness.children.runs(2)
        ),
        (1, 2, 1)
    );
    assert_eq!(harness.children.received(1)[1], vec!["ik-1-1-ask"]);
    assert_eq!(
        harness.ledger.acks(),
        vec![(
            "ik-1-1-ask".to_owned(),
            AckResult::Applied {
                child_checkpoint_id: "2".to_owned()
            }
        )]
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn sixteen_cards_answered_in_reverse_resume_each_child_once() {
    let mut harness = Harness::new(vec![vec![Step::Pause(vec!["ask"]), Step::Complete]; 16]);
    let first = harness
        .runner(8, OnError::Continue)
        .run()
        .await
        .expect("run");
    assert_eq!(parked(&first).len(), 16);
    for child in (0..16).rev() {
        harness.ledger.decide(&format!("ik-{child}-1-ask"));
    }
    harness.replace_process();
    let joined = settled(
        harness
            .runner(8, OnError::Continue)
            .run()
            .await
            .expect("resume"),
    );
    assert_eq!(joined.len(), 16);
    assert_eq!(
        joined
            .iter()
            .map(|child| child.key.clone())
            .collect::<Vec<_>>(),
        (0..16).map(|child| format!("b{child}")).collect::<Vec<_>>(),
        "declared order"
    );
    for child in 0..16 {
        assert_eq!(harness.children.runs(child), 2);
    }
    assert_eq!(harness.ledger.acks().len(), 16);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_duplicate_decision_applies_once() {
    let mut harness = Harness::new(vec![vec![Step::Pause(vec!["ask"]), Step::Complete]; 2]);
    let _ = harness
        .runner(2, OnError::Continue)
        .run()
        .await
        .expect("run");
    // The same card decided twice (Main replays one DECIDED row twice).
    harness.ledger.decide("ik-0-1-ask");
    harness.ledger.decide("ik-0-1-ask");
    harness.replace_process();
    let outcome = harness
        .runner(2, OnError::Continue)
        .run()
        .await
        .expect("resume");
    assert_eq!(parked(&outcome), vec!["ik-1-1-ask"]);
    assert_eq!(harness.children.runs(0), 2, "resumed twice");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn stale_and_foreign_cards_run_nothing_and_ack_stale() {
    let mut harness = Harness::new(vec![vec![Step::Pause(vec!["ask"]), Step::Complete]; 2]);
    let _ = harness
        .runner(2, OnError::Continue)
        .run()
        .await
        .expect("run");
    harness.ledger.decide("ik-foreign");
    harness.replace_process();
    let outcome = harness
        .runner(2, OnError::Continue)
        .run()
        .await
        .expect("resume");
    assert_eq!(parked(&outcome).len(), 2);
    assert_eq!((harness.children.runs(0), harness.children.runs(1)), (1, 1));
    assert_eq!(
        harness.ledger.acks(),
        vec![("ik-foreign".to_owned(), AckResult::Stale)]
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_decision_applied_before_a_crash_acks_applied_without_a_second_resume() {
    // Child 0 pauses twice (multi-card Agent); child 1 keeps the run parked.
    let mut harness = Harness::new(vec![
        vec![
            Step::Pause(vec!["first"]),
            Step::Pause(vec!["second"]),
            Step::Complete,
        ],
        vec![Step::Pause(vec!["other"]), Step::Complete],
    ]);
    let _ = harness
        .runner(2, OnError::Continue)
        .run()
        .await
        .expect("run");
    harness.ledger.decide("ik-0-1-first");
    harness.replace_process();
    let _ = harness
        .runner(2, OnError::Continue)
        .run()
        .await
        .expect("apply");
    assert_eq!(harness.children.runs(0), 2);
    // The ACK was lost in a crash: Main still returns the DECIDED row.
    harness.ledger.decide("ik-0-1-first");
    harness.replace_process();
    let outcome = harness
        .runner(2, OnError::Continue)
        .run()
        .await
        .expect("restore");
    assert_eq!(harness.children.runs(0), 2, "no second resume");
    assert_eq!(parked(&outcome), vec!["ik-0-2-second", "ik-1-1-other"]);
    assert_eq!(
        harness.ledger.acks().last(),
        Some(&(
            "ik-0-1-first".to_owned(),
            AckResult::Applied {
                child_checkpoint_id: "2".to_owned()
            }
        ))
    );
}

/// `after_apply` / `before_ack` with no card left open: the restore fetch
/// still acknowledges the applied decision, and nothing runs again.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_restore_without_open_cards_acks_a_decision_applied_before_a_crash() {
    let mut harness = Harness::new(vec![
        vec![Step::Pause(vec!["ask"]), Step::Complete],
        vec![Step::Complete],
    ]);
    let _ = harness
        .runner(2, OnError::Continue)
        .run()
        .await
        .expect("park");
    harness.ledger.decide("ik-0-1-ask");
    harness.replace_process();
    let joined = settled(
        harness
            .runner(2, OnError::Continue)
            .run()
            .await
            .expect("apply"),
    );
    assert_eq!(joined.len(), 2);
    assert_eq!(harness.children.runs(0), 2);
    // The ACK was lost: Main still holds the DECIDED row.
    harness.ledger.decide("ik-0-1-ask");
    harness.replace_process();
    let fetches = harness.ledger.fetches();
    let joined = settled(
        harness
            .runner(2, OnError::Continue)
            .run()
            .await
            .expect("restore"),
    );
    assert_eq!(joined.len(), 2);
    assert_eq!(harness.ledger.fetches(), fetches + 1, "one restore fetch");
    assert_eq!(harness.children.runs(0), 2, "no second resume");
    assert_eq!(harness.children.runs(1), 1, "a settled child ran again");
    assert_eq!(
        harness.ledger.acks().last(),
        Some(&(
            "ik-0-1-ask".to_owned(),
            AckResult::Applied {
                child_checkpoint_id: "2".to_owned()
            }
        ))
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_child_with_two_cards_resumes_only_when_both_are_decided() {
    let mut harness = Harness::new(vec![
        vec![Step::Pause(vec!["a", "b"]), Step::Complete],
        vec![Step::Pause(vec!["c"]), Step::Complete],
    ]);
    let _ = harness
        .runner(2, OnError::Continue)
        .run()
        .await
        .expect("run");
    harness.ledger.decide("ik-0-1-a");
    harness.replace_process();
    let outcome = harness
        .runner(2, OnError::Continue)
        .run()
        .await
        .expect("partial");
    assert_eq!(harness.children.runs(0), 1, "resumed with half its set");
    assert_eq!(parked(&outcome).len(), 3);
    assert!(harness.ledger.acks().is_empty(), "nothing ACKed early");
    harness.ledger.decide("ik-0-1-b");
    harness.replace_process();
    let _ = harness
        .runner(2, OnError::Continue)
        .run()
        .await
        .expect("complete set");
    assert_eq!(
        harness.children.received(0)[1],
        vec!["ik-0-1-a", "ik-0-1-b"]
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_live_decision_resumes_a_child_while_its_sibling_runs() {
    // Child 0 pauses; child 1 is held on its gate, so the runner stays live.
    let harness = Harness::new(vec![
        vec![Step::Pause(vec!["ask"]), Step::Complete],
        vec![Step::Gate, Step::Complete],
    ]);
    harness.ledger.decide("ik-0-1-ask");
    let runner = harness.runner(2, OnError::Continue);
    let task = tokio::spawn(runner.run());
    // The live tick fetches within ~1 s and resumes child 0.
    tokio::time::timeout(Duration::from_secs(10), async {
        while harness.children.runs(0) < 2 {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .expect("live pickup");
    harness.children.gates[1].add_permits(1);
    let joined = settled(task.await.expect("join").expect("run"));
    assert_eq!(joined.len(), 2);
}

#[tokio::test(start_paused = true)]
async fn no_card_means_zero_fetches() {
    let harness = Harness::new(vec![vec![Step::Gate, Step::Complete], vec![Step::Complete]]);
    let runner = harness.runner(2, OnError::Continue);
    let task = tokio::spawn(runner.run());
    tokio::time::sleep(Duration::from_secs(30)).await;
    harness.children.gates[0].add_permits(1);
    let _ = task.await.expect("join").expect("run");
    assert_eq!(harness.ledger.fetches(), 0);
}

#[tokio::test(start_paused = true)]
async fn the_tick_backs_off_1_2_4_5_5_seconds_while_nothing_is_new() {
    // Child 0 pauses at once; child 1 holds the run open.
    let harness = Harness::new(vec![
        vec![Step::Pause(vec!["ask"]), Step::Complete],
        vec![Step::Gate, Step::Complete],
    ]);
    let started = Instant::now();
    let runner = harness.runner(2, OnError::Continue);
    let task = tokio::spawn(runner.run());
    tokio::time::sleep(Duration::from_millis(17_500)).await;
    let offsets = harness
        .ledger
        .fetch_times
        .lock()
        .expect("times")
        .iter()
        .map(|at| (*at - started).as_secs())
        .collect::<Vec<_>>();
    // Card opens at t=0 -> fetches at 1, 3 (+2), 7 (+4), 12 (+5), 17 (+5).
    assert_eq!(offsets, vec![1, 3, 7, 12, 17]);
    harness.stop.fire();
    let _ = task.await;
}

#[tokio::test(start_paused = true)]
async fn cancel_stops_running_children_within_the_cleanup_bound() {
    let harness = Harness::new(vec![vec![Step::Gate, Step::Complete]; 3]);
    let runner = harness.runner(3, OnError::Continue);
    let task = tokio::spawn(runner.run());
    tokio::time::timeout(Duration::from_secs(5), async {
        while harness.children.running_now.load(Ordering::SeqCst) < 3 {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("children running");
    let fired = Instant::now();
    harness.stop.fire();
    let result = task.await.expect("join");
    assert_eq!(result, Err(FanoutRunError::Cancelled));
    assert!(fired.elapsed() <= CANCEL_CLEANUP, "{:?}", fired.elapsed());
}

#[tokio::test(start_paused = true)]
async fn a_child_that_ignores_cancel_is_a_typed_cleanup_failure() {
    let harness = Harness::new(vec![vec![Step::Hang]; 2]);
    let task = tokio::spawn(harness.runner(2, OnError::Continue).run());
    tokio::time::timeout(Duration::from_secs(5), async {
        while harness.children.running_now.load(Ordering::SeqCst) < 2 {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("running");
    harness.stop.fire();
    assert_eq!(
        task.await.expect("join"),
        Err(FanoutRunError::CancelCleanupExceeded)
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn lease_loss_in_a_child_or_in_a_fetch_is_a_control_stop() {
    let harness = Harness::new(vec![
        vec![Step::LeaseLost],
        vec![Step::Gate, Step::Complete],
    ]);
    assert_eq!(
        harness.runner(2, OnError::Continue).run().await,
        Err(FanoutRunError::LeaseLost)
    );
    let mut harness = Harness::new(three_asking());
    let _ = harness
        .runner(3, OnError::Continue)
        .run()
        .await
        .expect("park");
    harness.replace_process();
    harness.ledger.lease_lost.store(true, Ordering::SeqCst);
    assert_eq!(
        harness.runner(3, OnError::Continue).run().await,
        Err(FanoutRunError::LeaseLost)
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn continue_keeps_admitting_after_a_failure_and_stop_closes_admission() {
    let scripts = vec![vec![Step::Fail], vec![Step::Complete], vec![Step::Complete]];
    let harness = Harness::new(scripts.clone());
    let all = settled(
        harness
            .runner(1, OnError::Continue)
            .run()
            .await
            .expect("continue"),
    );
    assert_eq!(all.len(), 3);
    let harness = Harness::new(scripts);
    let stopped = settled(harness.runner(1, OnError::Stop).run().await.expect("stop"));
    assert_eq!(stopped.len(), 1, "admission closed after the failure");
    assert_eq!(harness.children.runs(1), 0);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn concurrency_limit_and_limit_plus_one() {
    let harness = Harness::new(vec![vec![Step::Complete]; 16]);
    let _ = harness
        .runner(8, OnError::Continue)
        .run()
        .await
        .expect("run");
    assert!(harness.children.running_peak.load(Ordering::SeqCst) <= 8);
    let stop = Arc::new(FanoutStop::default());
    let children = FakeChildren::new(vec![vec![Step::Complete]; 2], &stop);
    for invalid in [0, MAX_FANOUT_CONCURRENCY + 1] {
        assert!(matches!(
            FanoutRunner::new(
                children.clone(),
                Arc::new(FakeLedger::default()),
                Arc::new(NoEvents),
                ChildPermits::default(),
                Arc::clone(&stop),
                FanoutRunnerConfig {
                    max_concurrency: invalid,
                    on_error: OnError::Continue,
                    deadline: None,
                },
            ),
            Err(FanoutRunError::InvalidConfiguration)
        ));
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_paused_child_holds_no_worker_permit() {
    // One permit for the whole worker: three children still all run, because
    // each one that pauses releases its permit.
    let harness = Harness::new(three_asking());
    let outcome = harness
        .runner_with(3, OnError::Continue, None, ChildPermits::with_capacity(1))
        .run()
        .await
        .expect("run");
    assert_eq!(parked(&outcome).len(), 3);
    assert!(harness.children.running_peak.load(Ordering::SeqCst) <= 1);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn the_deadline_parks_when_only_paused_children_remain_and_fails_otherwise() {
    let harness = Harness::new(three_asking());
    let outcome = harness
        .runner_with(
            3,
            OnError::Continue,
            Some(Instant::now() + Duration::from_millis(50)),
            ChildPermits::default(),
        )
        .run()
        .await
        .expect("park");
    assert_eq!(parked(&outcome).len(), 3);
    let harness = Harness::new(vec![vec![Step::Gate, Step::Complete]; 2]);
    assert_eq!(
        harness
            .runner_with(
                2,
                OnError::Continue,
                Some(Instant::now() + Duration::from_millis(50)),
                ChildPermits::default(),
            )
            .run()
            .await,
        Err(FanoutRunError::DeadlineExceeded)
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn open_cards_limit_and_limit_plus_one() {
    let harness = Harness::new(vec![vec![Step::Pause(vec!["a"]), Step::Complete]; 16]);
    assert_eq!(
        parked(
            &harness
                .runner(8, OnError::Continue)
                .run()
                .await
                .expect("16")
        )
        .len(),
        16
    );
    let mut scripts = vec![vec![Step::Pause(vec!["a"]), Step::Complete]; 15];
    scripts.push(vec![Step::Pause(vec!["a", "b"]), Step::Complete]);
    let harness = Harness::new(scripts);
    assert_eq!(
        harness.runner(8, OnError::Continue).run().await,
        Err(FanoutRunError::TooManyOpenInterrupts)
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_restore_with_open_cards_fetches_once_immediately_and_parks() {
    let mut harness = Harness::new(three_asking());
    let _ = harness
        .runner(3, OnError::Continue)
        .run()
        .await
        .expect("park");
    assert_eq!(
        harness.ledger.fetches(),
        1,
        "the final fetch before parking"
    );
    harness.replace_process();
    let outcome = harness
        .runner(3, OnError::Continue)
        .run()
        .await
        .expect("wake");
    assert_eq!(parked(&outcome).len(), 3);
    // Restore fetch + final fetch, no tick.
    assert_eq!(harness.ledger.fetches(), 3);
    for child in 0..3 {
        assert_eq!(harness.children.runs(child), 1, "a paused child re-ran");
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn delegated_auth_applies_only_at_the_park_boundary() {
    // Child 0 asks for authorization; child 1 keeps the execution live.
    let mut harness = Harness::new(vec![
        vec![Step::Pause(vec!["auth"]), Step::Complete],
        vec![Step::Gate, Step::Complete],
    ]);
    harness.ledger.decide_action("ik-0-1-auth", "authorize");
    let task = tokio::spawn(harness.runner(2, OnError::Continue).run());
    // Live ticks see the decision but leave it for the continuation.
    tokio::time::timeout(Duration::from_secs(10), async {
        while harness.ledger.fetches() < 2 {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .expect("live ticks");
    assert_eq!(
        harness.children.runs(0),
        1,
        "auth applied under the live claim"
    );
    harness.children.gates[1].add_permits(1);
    let outcome = task.await.expect("join").expect("run");
    assert_eq!(parked(&outcome), vec!["ik-0-1-auth"]);
    assert_eq!(harness.ledger.acks(), Vec::new());
    // The wake continuation's restore fetch applies it.
    harness.replace_process();
    let joined = settled(
        harness
            .runner(2, OnError::Continue)
            .run()
            .await
            .expect("wake"),
    );
    assert_eq!(joined.len(), 2);
    assert_eq!(harness.children.received(0)[1], vec!["ik-0-1-auth"]);
}

/// Every typed fan-out code follows the dotted `graph.fanout.*` grammar the
/// join-entry schema and Main expect (`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)+$`).
#[test]
fn every_fanout_code_is_in_the_dotted_graph_fanout_grammar() {
    use crate::graph::fanout::plan::FanoutPlanError as Plan;
    let codes = [
        Plan::InvalidOnError.code(),
        Plan::InvalidJoin.code(),
        Plan::ReducerMismatch.code(),
        Plan::MergeNeedsStop.code(),
        Plan::JoinConflict.code(),
        Plan::JoinTypeMismatch.code(),
        Plan::ChildResultTooLarge.code(),
        Plan::JoinedResultTooLarge.code(),
        Plan::TooManyChildren.code(),
        Plan::InvalidChild.code(),
        FanoutRunError::LeaseLost.to_string(),
        FanoutRunError::Cancelled.to_string(),
        FanoutRunError::CancelCleanupExceeded.to_string(),
        FanoutRunError::DeadlineExceeded.to_string(),
        FanoutRunError::TooManyOpenInterrupts.to_string(),
        FanoutRunError::InvalidConfiguration.to_string(),
        FanoutRunError::CorruptChild.to_string(),
        DecisionError::Unavailable.to_string(),
        DecisionError::LeaseLost.to_string(),
    ];
    for code in codes {
        assert!(
            code.starts_with("graph.fanout.") && crate::graph::fanout::plan::valid_code(&code),
            "{code}"
        );
    }
}
