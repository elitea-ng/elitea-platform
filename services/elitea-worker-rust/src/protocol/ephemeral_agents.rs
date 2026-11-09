//! The sub-agents one claimed execution started (ADR-0050 decisions 4 and
//! 9d, #1614), for Main's `ephemeral_agents` record.
//!
//! The cloud sub-agent host records each child into the execution's one
//! [`EphemeralAgentLedger`] when it starts and again when it settles. Like
//! the egress ledger, ledgers live in a process registry, keyed here by
//! execution and generation (the host knows the claim, not the command id).
//! `AgentControlClient` sends the ledger's snapshot with
//! `ReportEphemeralAgents` under the live claim, before `PrepareSettlement`,
//! and forgets it once the settlement is prepared. A child still running at
//! that point was stopped with the run, so it is reported cancelled.

use std::collections::{BTreeMap, HashMap};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use elitea_agent_runtime::subagents::{ChildIdentity, ChildReport, ChildStatus};

use super::elitea::runtime::v1::{EphemeralAgentV1, ExecutionIdentityV1};

/// The most executions the registry holds; past it the oldest is dropped.
const MAX_TRACKED_EXECUTIONS: usize = 4_096;
/// An execution not settled within this is dropped.
const MAX_TRACKED_AGE: Duration = Duration::from_hours(12);
/// The most spawns one report carries (the runtime's per-turn cap).
pub(crate) const MAX_REPORTED_AGENTS: usize =
    elitea_agent_runtime::subagents::MAX_CHILDREN_PER_TURN;
/// The longest brief references, as JSON (Main's bound).
const MAX_BRIEF_REFS_BYTES: usize = 8 * 1024;
/// The error code of a child the run's end stopped.
const CANCELLED_WITH_RUN: &str = "subagent_cancelled";

/// One execution's spawns, by agent id, in spawn order.
#[derive(Default)]
pub(crate) struct EphemeralAgentLedger {
    agents: Mutex<BTreeMap<usize, EphemeralAgentV1>>,
}

impl EphemeralAgentLedger {
    /// The child started: its row, running.
    pub(crate) fn started(&self, child: &ChildIdentity, now_unix_ms: i64) {
        let record = &child.record;
        let brief_refs = if record.brief_refs.is_null() {
            String::new()
        } else {
            record.brief_refs.to_string()
        };
        // The brief's bounds keep its references far under Main's 8 KiB;
        // past it both are dropped, as Main needs them together.
        let (brief_digest, brief_refs) = if brief_refs.len() > MAX_BRIEF_REFS_BYTES {
            (String::new(), String::new())
        } else {
            (record.brief_digest.clone().unwrap_or_default(), brief_refs)
        };
        let row = EphemeralAgentV1 {
            agent_id: child.agent_id.clone(),
            parent_call_id: child.parent_call_id.clone(),
            ordinal: u32::try_from(child.ordinal).unwrap_or(u32::MAX),
            label: child.label.clone().unwrap_or_default(),
            display_name: record.display_name.clone().unwrap_or_default(),
            avatar: record.avatar.clone().unwrap_or_default(),
            profile_digest: record.profile_digest.clone(),
            brief_digest,
            brief_refs_json: brief_refs,
            status: ChildStatus::Running.as_str().to_owned(),
            started_at_unix_millis: now_unix_ms,
            ..EphemeralAgentV1::default()
        };
        if let Ok(mut agents) = self.agents.lock() {
            agents.insert(child.ordinal, row);
        }
    }

    /// The child settled (it may run again on `send_input`, and settle
    /// again).
    pub(crate) fn finished(&self, child: &ChildIdentity, report: &ChildReport, now_unix_ms: i64) {
        let Ok(mut agents) = self.agents.lock() else {
            return;
        };
        let Some(row) = agents.get_mut(&child.ordinal) else {
            return;
        };
        report.status.as_str().clone_into(&mut row.status);
        row.steps = report.steps;
        row.input_tokens = report.input_tokens;
        row.output_tokens = report.output_tokens;
        report
            .error
            .unwrap_or_default()
            .clone_into(&mut row.error_code);
        row.finished_at_unix_millis = if report.status.is_settled() {
            now_unix_ms.max(row.started_at_unix_millis)
        } else {
            0
        };
    }

    /// Every spawn, in spawn order, at most [`MAX_REPORTED_AGENTS`]. A
    /// child still running was stopped with the run: it is reported
    /// cancelled at `now_unix_ms`.
    pub(crate) fn snapshot(&self, now_unix_ms: i64) -> Vec<EphemeralAgentV1> {
        let Ok(agents) = self.agents.lock() else {
            return Vec::new();
        };
        agents
            .values()
            .take(MAX_REPORTED_AGENTS)
            .cloned()
            .map(|mut row| {
                if row.status == ChildStatus::Running.as_str() {
                    ChildStatus::Cancelled.as_str().clone_into(&mut row.status);
                    CANCELLED_WITH_RUN.clone_into(&mut row.error_code);
                    row.finished_at_unix_millis = now_unix_ms.max(row.started_at_unix_millis);
                }
                row
            })
            .collect()
    }
}

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
struct ExecutionKey {
    execution_id: String,
    generation: u64,
}

struct Tracked {
    ledger: Arc<EphemeralAgentLedger>,
    since: Instant,
}

/// The executions this process is running, each with its one ledger.
/// Production uses [`EphemeralAgentRegistry::global`]; a test may hold its
/// own.
#[derive(Default)]
pub(crate) struct EphemeralAgentRegistry {
    tracked: Mutex<HashMap<ExecutionKey, Tracked>>,
}

impl EphemeralAgentRegistry {
    /// The process's registry.
    #[must_use]
    pub(crate) fn global() -> Arc<Self> {
        static GLOBAL: OnceLock<Arc<EphemeralAgentRegistry>> = OnceLock::new();
        Arc::clone(GLOBAL.get_or_init(|| Arc::new(Self::default())))
    }

    /// The execution generation's ledger, created on first use. A newer
    /// generation drops the older generations' ledgers.
    #[must_use]
    pub(crate) fn ledger(&self, execution_id: &str, generation: u64) -> Arc<EphemeralAgentLedger> {
        let Ok(mut tracked) = self.tracked.lock() else {
            return Arc::default();
        };
        let key = ExecutionKey {
            execution_id: execution_id.to_owned(),
            generation,
        };
        if let Some(existing) = tracked.get(&key) {
            return Arc::clone(&existing.ledger);
        }
        let now = Instant::now();
        tracked.retain(|other, entry| {
            now.duration_since(entry.since) < MAX_TRACKED_AGE
                && !(other.execution_id == key.execution_id && other.generation < key.generation)
        });
        if tracked.len() >= MAX_TRACKED_EXECUTIONS
            && let Some(oldest) = tracked
                .iter()
                .min_by_key(|(_, entry)| entry.since)
                .map(|(key, _)| key.clone())
        {
            tracked.remove(&oldest);
        }
        let ledger = Arc::new(EphemeralAgentLedger::default());
        tracked.insert(
            key,
            Tracked {
                ledger: Arc::clone(&ledger),
                since: now,
            },
        );
        ledger
    }

    /// The command's spawns for the report before settlement; empty when it
    /// started none. The ledger stays until [`Self::forget`]: a retried
    /// settlement reports the same rows.
    #[must_use]
    pub(crate) fn snapshot_for_settlement(
        &self,
        identity: &ExecutionIdentityV1,
        now_unix_ms: i64,
    ) -> Vec<EphemeralAgentV1> {
        let Ok(tracked) = self.tracked.lock() else {
            return Vec::new();
        };
        tracked
            .get(&ExecutionKey {
                execution_id: identity.execution_id.clone(),
                generation: identity.generation,
            })
            .map(|entry| entry.ledger.snapshot(now_unix_ms))
            .unwrap_or_default()
    }

    /// Stop tracking a settled command.
    pub(crate) fn forget(&self, identity: &ExecutionIdentityV1) {
        if let Ok(mut tracked) = self.tracked.lock() {
            tracked.remove(&ExecutionKey {
                execution_id: identity.execution_id.clone(),
                generation: identity.generation,
            });
        }
    }
}

#[cfg(test)]
mod tests {
    use elitea_agent_runtime::subagents::profile::{Brief, Excerpt, Profile, SpawnRecord};

    use super::*;

    fn child(ordinal: usize, brief: &Brief) -> ChildIdentity {
        ChildIdentity {
            agent_id: format!("a_{ordinal:08x}"),
            parent_call_id: format!("call-{ordinal}"),
            label: Some("Owl".to_owned()),
            ordinal,
            record: SpawnRecord::new(
                &Profile {
                    display_name: Some("Owl".to_owned()),
                    ..Profile::default()
                },
                brief,
            ),
        }
    }

    fn report(child: &ChildIdentity, status: ChildStatus) -> ChildReport {
        ChildReport {
            agent_id: child.agent_id.clone(),
            status,
            result: Some("the answer is never recorded".to_owned()),
            steps: 3,
            input_tokens: 100,
            output_tokens: 7,
            error: None,
        }
    }

    fn identity(execution_id: &str, generation: u64) -> ExecutionIdentityV1 {
        ExecutionIdentityV1 {
            execution_id: execution_id.to_owned(),
            generation,
            ..ExecutionIdentityV1::default()
        }
    }

    #[test]
    fn one_row_per_spawn_settled_or_cancelled_with_the_run() {
        let registry = EphemeralAgentRegistry::default();
        let ledger = registry.ledger("e", 1);
        let brief = Brief {
            excerpts: vec![Excerpt {
                message_id: "m-1".to_owned(),
                quote: "a quote".to_owned(),
            }],
            ..Brief::default()
        };
        let first = child(1, &brief);
        let second = child(2, &Brief::default());
        ledger.started(&first, 1_000);
        ledger.started(&second, 1_100);
        ledger.finished(&first, &report(&first, ChildStatus::Completed), 2_000);
        // The same ledger for every caller of the execution generation.
        assert!(Arc::ptr_eq(&ledger, &registry.ledger("e", 1)));

        let rows = registry.snapshot_for_settlement(&identity("e", 1), 3_000);
        assert_eq!(rows.len(), 2);
        assert_eq!(rows[0].agent_id, "a_00000001");
        assert_eq!(rows[0].status, "completed");
        assert_eq!(rows[0].steps, 3);
        assert_eq!(rows[0].finished_at_unix_millis, 2_000);
        assert_eq!(rows[0].display_name, "Owl");
        assert_eq!(rows[0].brief_digest.len(), 64);
        assert!(rows[0].brief_refs_json.contains("m-1"));
        assert!(!rows[0].brief_refs_json.contains("a quote"));
        assert_eq!(rows[1].status, "cancelled");
        assert_eq!(rows[1].error_code, CANCELLED_WITH_RUN);
        assert_eq!(rows[1].finished_at_unix_millis, 3_000);
        assert!(rows[1].brief_digest.is_empty() && rows[1].brief_refs_json.is_empty());

        // A newer generation starts empty and drops the older one.
        assert_eq!(registry.ledger("e", 2).snapshot(0).len(), 0);
        assert_eq!(
            registry.snapshot_for_settlement(&identity("e", 1), 0).len(),
            0
        );
        registry.forget(&identity("e", 2));
        assert_eq!(
            registry.snapshot_for_settlement(&identity("e", 2), 0).len(),
            0
        );
    }
}
