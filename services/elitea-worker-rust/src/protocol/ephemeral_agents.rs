//! The sub-agents one claimed execution started (ADR-0050 decisions 4 and
//! 9d, #1614), for Main's `ephemeral_agents` record.
//!
//! The cloud sub-agent host records each child into the execution's one
//! [`EphemeralAgentLedger`] when it starts and again when it settles. Like
//! the egress ledger, ledgers live in a process registry
//! ([`super::execution_registry`]), keyed here by execution and generation
//! (the host knows the claim, not the command id).
//! `AgentControlClient` sends the ledger's snapshot with
//! `ReportEphemeralAgents` under the live claim, before `PrepareSettlement`,
//! and forgets it once the settlement is prepared. A child still running at
//! that point was stopped with the run, so it is reported cancelled.

use std::collections::{BTreeMap, HashMap};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock};

use elitea_agent_runtime::subagents::{ChildIdentity, ChildReport, ChildStatus, Profile};

use super::elitea::runtime::v1::{EphemeralAgentV1, ExecutionIdentityV1};
use super::execution_registry::{ExecutionRegistry, GenerationKey};

/// Process-wide count of spawns whose brief digest and references were not
/// recorded because the references were over [`MAX_BRIEF_REFS_BYTES`]. The
/// crate has no metrics exporter; the count rides every drop's WARN event.
static DROPPED_BRIEF_REFS_TOTAL: AtomicU64 = AtomicU64::new(0);

/// The most spawns one report carries (the runtime's per-turn cap).
pub(crate) const MAX_REPORTED_AGENTS: usize =
    elitea_agent_runtime::subagents::MAX_CHILDREN_PER_TURN;
/// The longest brief references, as JSON (Main's bound).
const MAX_BRIEF_REFS_BYTES: usize = 8 * 1024;
/// The error code of a child the run's end stopped.
const CANCELLED_WITH_RUN: &str = "subagent_cancelled";

/// One execution's spawns, keyed by ordinal and agent id (Main's key is
/// the agent id), so they iterate in spawn order.
#[derive(Default)]
pub(crate) struct EphemeralAgentLedger {
    agents: Mutex<BTreeMap<(usize, String), EphemeralAgentV1>>,
    /// Each spawn's whole profile by agent id, for `propose_agent` (ADR-0050
    /// decision 5a): never reported, dropped with the ledger.
    profiles: Mutex<HashMap<String, (String, Profile)>>,
    /// The children's live lane (#1823): what the run's event loop projects
    /// as `agent_subagent` frames.
    lane: crate::agents::subagent_lane::SubAgentLane,
}

impl EphemeralAgentLedger {
    /// The children's live lane (#1823).
    #[must_use]
    pub(crate) const fn lane(&self) -> &crate::agents::subagent_lane::SubAgentLane {
        &self.lane
    }

    /// The child started: its row, running.
    pub(crate) fn started(&self, child: &ChildIdentity, now_unix_ms: i64) {
        let record = &child.record;
        let brief_refs = if record.brief_refs.is_null() {
            String::new()
        } else {
            record.brief_refs.to_string()
        };
        // The brief's bounds keep its references far under Main's 8 KiB;
        // past it both are dropped, as Main needs them together (#1832:
        // counted and logged, never silent).
        let (brief_digest, brief_refs) = if brief_refs.len() > MAX_BRIEF_REFS_BYTES {
            let dropped = DROPPED_BRIEF_REFS_TOTAL.fetch_add(1, Ordering::Relaxed) + 1;
            tracing::warn!(
                agent_id = %child.agent_id,
                refs_bytes = brief_refs.len(),
                max_bytes = MAX_BRIEF_REFS_BYTES,
                dropped_total = dropped,
                "sub-agent brief references over the record's bound; the brief digest and references are not recorded"
            );
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
            depth: child.depth.max(1),
            parent_agent_id: child.parent_agent_id.clone().unwrap_or_default(),
            ..EphemeralAgentV1::default()
        };
        if let Ok(mut agents) = self.agents.lock() {
            agents.insert((child.ordinal, child.agent_id.clone()), row);
        }
        if let Ok(mut profiles) = self.profiles.lock() {
            profiles.insert(
                child.agent_id.clone(),
                (
                    child.label.clone().unwrap_or_default(),
                    record.profile.clone(),
                ),
            );
        }
    }

    /// The label and profile of the spawn `agent_id` of this execution, if
    /// it started one.
    #[must_use]
    pub(crate) fn profile(&self, agent_id: &str) -> Option<(String, Profile)> {
        self.profiles.lock().ok()?.get(agent_id).cloned()
    }

    /// The settled child runs again (`send_input`): running, with no end,
    /// until it settles again. Its steps and tokens stay until then.
    pub(crate) fn resumed(&self, child: &ChildIdentity) {
        let Ok(mut agents) = self.agents.lock() else {
            return;
        };
        if let Some(row) = agents.get_mut(&(child.ordinal, child.agent_id.clone())) {
            ChildStatus::Running.as_str().clone_into(&mut row.status);
            row.error_code.clear();
            row.finished_at_unix_millis = 0;
        }
    }

    /// The child settled (it may run again on `send_input`, and settle
    /// again).
    pub(crate) fn finished(&self, child: &ChildIdentity, report: &ChildReport, now_unix_ms: i64) {
        let Ok(mut agents) = self.agents.lock() else {
            return;
        };
        let Some(row) = agents.get_mut(&(child.ordinal, child.agent_id.clone())) else {
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

impl ExecutionKey {
    fn new(execution_id: &str, generation: u64) -> Self {
        Self {
            execution_id: execution_id.to_owned(),
            generation,
        }
    }
}

impl GenerationKey for ExecutionKey {
    fn execution_id(&self) -> &str {
        &self.execution_id
    }

    fn generation(&self) -> u64 {
        self.generation
    }
}

/// The executions this process is running, each with its one ledger.
/// Production uses [`EphemeralAgentRegistry::global`]; a test may hold its
/// own.
#[derive(Default)]
pub(crate) struct EphemeralAgentRegistry {
    tracked: ExecutionRegistry<ExecutionKey, EphemeralAgentLedger>,
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
        self.tracked.get_or_make(
            ExecutionKey::new(execution_id, generation),
            |_| true,
            Arc::default,
        )
    }

    /// The execution generation's ledger when its host made one (a run that
    /// binds sub-agents makes it at assembly, before its stream starts).
    #[must_use]
    pub(crate) fn existing(
        &self,
        execution_id: &str,
        generation: u64,
    ) -> Option<Arc<EphemeralAgentLedger>> {
        self.tracked
            .get(&ExecutionKey::new(execution_id, generation))
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
        self.tracked
            .get(&ExecutionKey::new(
                &identity.execution_id,
                identity.generation,
            ))
            .map(|ledger| ledger.snapshot(now_unix_ms))
            .unwrap_or_default()
    }

    /// Stop tracking a settled command.
    pub(crate) fn forget(&self, identity: &ExecutionIdentityV1) {
        self.tracked.forget(&ExecutionKey::new(
            &identity.execution_id,
            identity.generation,
        ));
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
            depth: 1,
            parent_agent_id: None,
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
    fn a_started_spawn_keeps_its_whole_profile_for_a_proposal() {
        let ledger = EphemeralAgentLedger::default();
        assert!(ledger.profile("a_00000001").is_none());
        let mut spawned = child(1, &Brief::default());
        spawned.record.profile.soul = Some("Careful.".to_owned());
        ledger.started(&spawned, 10);
        let (label, profile) = ledger.profile("a_00000001").expect("kept");
        assert_eq!(label, "Owl");
        assert_eq!(profile.soul.as_deref(), Some("Careful."));
        assert_eq!(profile.display_name.as_deref(), Some("Owl"));
        let reported = ledger.snapshot(20);
        assert_eq!(reported.len(), 1, "the profile is never a reported row");
        assert!(ledger.profile("a_00000002").is_none());
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

    #[test]
    fn a_child_continued_after_it_settled_is_running_again() {
        let ledger = EphemeralAgentLedger::default();
        let first = child(1, &Brief::default());
        ledger.started(&first, 1_000);
        ledger.finished(&first, &report(&first, ChildStatus::Completed), 2_000);
        ledger.resumed(&first);
        // The run ended before the continued child settled again.
        let rows = ledger.snapshot(3_000);
        assert_eq!(rows.len(), 1);
        assert_eq!(rows[0].status, "cancelled");
        assert_eq!(rows[0].error_code, CANCELLED_WITH_RUN);
        assert_eq!(rows[0].finished_at_unix_millis, 3_000);
        assert_eq!(rows[0].steps, 3);
    }

    #[test]
    fn a_depth_two_run_reports_two_levels_with_their_parents() {
        let ledger = EphemeralAgentLedger::default();
        let first = child(1, &Brief::default());
        let mut grandchild = child(2, &Brief::default());
        grandchild.depth = 2;
        grandchild.parent_agent_id = Some(first.agent_id.clone());
        let mut legacy = child(3, &Brief::default());
        legacy.depth = 0;
        ledger.started(&first, 1_000);
        ledger.started(&grandchild, 1_100);
        ledger.started(&legacy, 1_200);
        let rows = ledger.snapshot(2_000);
        let levels: Vec<(u32, &str)> = rows
            .iter()
            .map(|row| (row.depth, row.parent_agent_id.as_str()))
            .collect();
        assert_eq!(levels, [(1, ""), (2, "a_00000001"), (1, "")]);
    }

    #[test]
    fn brief_references_over_the_bound_are_dropped_and_counted() {
        let ledger = EphemeralAgentLedger::default();
        let mut spawned = child(1, &Brief::default());
        spawned.record.brief_digest = Some("ab".repeat(32));
        spawned.record.brief_refs = serde_json::json!({
            "message_ids": vec!["m".repeat(64); 200],
        });
        let before = DROPPED_BRIEF_REFS_TOTAL.load(Ordering::Relaxed);
        ledger.started(&spawned, 1_000);
        let rows = ledger.snapshot(2_000);
        assert!(rows[0].brief_digest.is_empty() && rows[0].brief_refs_json.is_empty());
        assert!(DROPPED_BRIEF_REFS_TOTAL.load(Ordering::Relaxed) > before);
    }
}
