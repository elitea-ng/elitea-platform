//! A test stand-in for Main's decision ledger (contract §5, §7): DECIDED rows
//! are fetched until an ACK consumes them, every ACK is recorded, and a claim
//! fence refuses a superseded process. Crash seams make one call never return
//! so a test can kill the process at that exact point.

use std::sync::Mutex;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};

use async_trait::async_trait;
use elitea_agent_runtime::graph::fanout::runner::{
    AckResult, DecisionBatch, DecisionError, DecisionSource, FetchedDecision,
};
use tokio::sync::Notify;

/// Where a ledger call stops forever (the process dies there).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum LedgerSeam {
    /// The fetch returned the decisions to nobody.
    AfterFetch,
    /// The child resumed and saved; the ACK never reached Main.
    BeforeAck,
}

#[derive(Default)]
struct Rows {
    /// DECIDED rows in decision order (a redelivered row appears twice).
    decided: Vec<FetchedDecision>,
    acks: Vec<(String, AckResult)>,
    revision: u64,
    seam: Option<LedgerSeam>,
}

/// Shared by every process of one test; [`TestLedger::client`] binds a claim.
#[derive(Default)]
pub(crate) struct TestLedger {
    rows: Mutex<Rows>,
    current_claim: AtomicU64,
    fetches: AtomicUsize,
    /// Notified when a seam is reached.
    pub(crate) reached: Notify,
}

impl TestLedger {
    pub(crate) fn new() -> std::sync::Arc<Self> {
        std::sync::Arc::new(Self::default())
    }

    /// Decide one card, as a decision POST would (revision + 1).
    pub(crate) fn decide(&self, interrupt_key: &str, action: &str, value: Option<&str>) {
        let mut rows = self.rows.lock().expect("ledger lock");
        rows.revision += 1;
        let revision = rows.revision;
        rows.decided
            .retain(|decided| decided.interrupt_key != interrupt_key);
        rows.decided.push(FetchedDecision {
            interrupt_key: interrupt_key.to_owned(),
            request_id: format!("request-{revision}"),
            revision,
            decision_sha256: "0".repeat(64),
            action: action.to_owned(),
            value: value.map(str::to_owned),
            credential_ref: None,
        });
    }

    /// Return one DECIDED row twice in the next fetches (a duplicate delivery).
    pub(crate) fn redeliver(&self, interrupt_key: &str) {
        let mut rows = self.rows.lock().expect("ledger lock");
        let again = rows
            .decided
            .iter()
            .find(|decided| decided.interrupt_key == interrupt_key)
            .cloned()
            .expect("a decided row to redeliver");
        rows.decided.push(again);
    }

    /// Stop the next matching call forever.
    pub(crate) fn crash_at(&self, seam: LedgerSeam) {
        self.rows.lock().expect("ledger lock").seam = Some(seam);
    }

    pub(crate) fn acks(&self) -> Vec<(String, AckResult)> {
        self.rows.lock().expect("ledger lock").acks.clone()
    }

    pub(crate) fn open_decisions(&self) -> usize {
        self.rows.lock().expect("ledger lock").decided.len()
    }

    pub(crate) fn fetches(&self) -> usize {
        self.fetches.load(Ordering::SeqCst)
    }

    /// A client fenced to `claim`. A later claim supersedes it.
    pub(crate) fn client(self: &std::sync::Arc<Self>, claim: u64) -> std::sync::Arc<LedgerClient> {
        self.current_claim.fetch_max(claim, Ordering::SeqCst);
        std::sync::Arc::new(LedgerClient {
            ledger: std::sync::Arc::clone(self),
            claim,
        })
    }

    fn take_seam(&self, seam: LedgerSeam) -> bool {
        let mut rows = self.rows.lock().expect("ledger lock");
        if rows.seam == Some(seam) {
            rows.seam = None;
            return true;
        }
        false
    }
}

pub(crate) struct LedgerClient {
    ledger: std::sync::Arc<TestLedger>,
    claim: u64,
}

impl LedgerClient {
    fn fenced(&self) -> Result<(), DecisionError> {
        if self.ledger.current_claim.load(Ordering::SeqCst) == self.claim {
            Ok(())
        } else {
            Err(DecisionError::LeaseLost)
        }
    }
}

#[async_trait]
impl DecisionSource for LedgerClient {
    async fn fetch(&self) -> Result<DecisionBatch, DecisionError> {
        self.fenced()?;
        self.ledger.fetches.fetch_add(1, Ordering::SeqCst);
        let batch = {
            let rows = self.ledger.rows.lock().expect("ledger lock");
            DecisionBatch {
                decisions: rows.decided.iter().take(16).cloned().collect(),
                decision_revision: rows.revision,
            }
        };
        if !batch.decisions.is_empty() && self.ledger.take_seam(LedgerSeam::AfterFetch) {
            self.ledger.reached.notify_one();
            std::future::pending::<()>().await;
        }
        Ok(batch)
    }

    async fn ack(
        &self,
        decision: &FetchedDecision,
        result: &AckResult,
    ) -> Result<(), DecisionError> {
        self.fenced()?;
        if self.ledger.take_seam(LedgerSeam::BeforeAck) {
            self.ledger.reached.notify_one();
            std::future::pending::<()>().await;
        }
        let mut rows = self.ledger.rows.lock().expect("ledger lock");
        let before = rows.decided.len();
        rows.decided.retain(|decided| {
            decided.interrupt_key != decision.interrupt_key || decided.revision != decision.revision
        });
        if rows.decided.len() < before {
            rows.acks
                .push((decision.interrupt_key.clone(), result.clone()));
        }
        Ok(())
    }
}
