//! The command's public egress policy (ADR-0044 decision 2, #1490).
//!
//! Main puts `WorkerCommandV1.egress_policy` (field 66) on the signed
//! command: absent, `UNSPECIFIED` or `OPEN` is open (any public host, today's
//! behaviour); `ALLOWLIST` admits only the hosts its entries name, in the
//! egresslib grammar. A malformed entry, too many entries or an unknown mode
//! fails the command closed at verification ([`policy_from_command`]); an
//! entry is never skipped.
//!
//! One [`RunEgress`] per command (execution, generation, command id) holds
//! the policy and the ledger of hosts reached and refused. The registry here
//! hands every path that runs part of the command (an agent turn, its
//! checkpoint recovery, a direct toolkit call) the same instance, so the
//! summary the worker reports before settlement
//! ([`EgressRegistry::summary_for_settlement`]) covers all of it. A later
//! generation of the same execution (a retry, a resumed pause) is a new
//! command: it starts a new ledger under its own signed policy, never the
//! earlier one's, and drops the earlier generations' entries. A settled
//! command is forgotten; one that never settles (lease loss, abandonment)
//! ages out, and can only ever be served to its own command.

use std::collections::HashMap;
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use elitea_agent_runtime::egress::{
    Allowlist, EgressSummary, PublicPolicy, PublicPolicyError, RunEgress,
};

use super::ProtocolError;
use super::elitea::runtime::v1::{ExecutionIdentityV1, WorkerCommandV1, WorkerEgressModeV1};

/// The most executions the registry holds; past it the oldest is dropped.
const MAX_TRACKED_EXECUTIONS: usize = 4_096;
/// An execution not settled within this is dropped (its summary is lost; the
/// run is long over or abandoned).
const MAX_TRACKED_AGE: Duration = Duration::from_hours(12);

/// The command's policy, validated.
///
/// # Errors
///
/// [`ProtocolError::InvalidInput`] for an unknown mode or a malformed entry,
/// [`ProtocolError::ResourceExhausted`] for more than 1024 entries.
pub fn policy_from_command(command: &WorkerCommandV1) -> Result<PublicPolicy, ProtocolError> {
    let Some(policy) = command.egress_policy.as_ref() else {
        return Ok(PublicPolicy::Open);
    };
    match WorkerEgressModeV1::try_from(policy.mode) {
        Ok(WorkerEgressModeV1::Unspecified | WorkerEgressModeV1::Open) => Ok(PublicPolicy::Open),
        Ok(WorkerEgressModeV1::Allowlist) => {
            PublicPolicy::allowlist(policy.allow.iter().map(String::as_str)).map_err(|error| {
                match error {
                    PublicPolicyError::TooManyEntries => ProtocolError::ResourceExhausted(
                        "the worker egress allowlist exceeds the conformance limit",
                    ),
                    PublicPolicyError::Malformed => {
                        ProtocolError::InvalidInput("the worker egress allowlist is malformed")
                    }
                }
            })
        }
        Err(_) => Err(ProtocolError::InvalidInput(
            "the worker egress mode is not supported",
        )),
    }
}

/// One command's key: the execution, its generation and the command id, as
/// both the signed command and the settlement identity carry them. A retry or
/// resume of the same execution is a new generation (and command), so it
/// never shares a ledger or a policy with an earlier one.
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
struct CommandKey {
    execution_id: String,
    generation: u64,
    command_id: String,
}

impl CommandKey {
    fn of_command(command: &WorkerCommandV1) -> Self {
        Self {
            execution_id: command.execution_id.clone(),
            generation: command.generation,
            command_id: command.command_id.clone(),
        }
    }

    fn of_identity(identity: &ExecutionIdentityV1) -> Self {
        Self {
            execution_id: identity.execution_id.clone(),
            generation: identity.generation,
            command_id: identity.command_id.clone(),
        }
    }
}

struct Tracked {
    egress: Arc<RunEgress>,
    since: Instant,
}

/// The commands this process is running, each with its one [`RunEgress`].
/// Production uses [`EgressRegistry::global`]; a test may hold its own.
#[derive(Default)]
pub struct EgressRegistry {
    tracked: Mutex<HashMap<CommandKey, Tracked>>,
}

impl EgressRegistry {
    /// The process's registry.
    #[must_use]
    pub fn global() -> Arc<Self> {
        static GLOBAL: OnceLock<Arc<EgressRegistry>> = OnceLock::new();
        Arc::clone(GLOBAL.get_or_init(|| Arc::new(Self::default())))
    }

    /// The command's egress: the same instance for every caller of one
    /// command (execution, generation and command id), always under THIS
    /// command's signed policy. A tracked entry whose policy differs is
    /// replaced, never served; a newer generation of the execution drops the
    /// older generations' entries (their leases are gone). A command that
    /// somehow failed validation gets an EMPTY allowlist (nothing public is
    /// reached), never an open policy.
    #[must_use]
    pub fn run_egress(&self, command: &WorkerCommandV1) -> Arc<RunEgress> {
        let policy = policy_from_command(command)
            .unwrap_or_else(|_| PublicPolicy::Allowlist(Allowlist::default()));
        let Ok(mut tracked) = self.tracked.lock() else {
            return RunEgress::new(policy);
        };
        let key = CommandKey::of_command(command);
        if let Some(existing) = tracked.get(&key)
            && *existing.egress.policy() == policy
        {
            return Arc::clone(&existing.egress);
        }
        let now = Instant::now();
        tracked.retain(|other, entry| {
            now.duration_since(entry.since) < MAX_TRACKED_AGE
                && !(other.execution_id == key.execution_id && other.generation < key.generation)
        });
        if !tracked.contains_key(&key)
            && tracked.len() >= MAX_TRACKED_EXECUTIONS
            && let Some(oldest) = tracked
                .iter()
                .min_by_key(|(_, entry)| entry.since)
                .map(|(key, _)| key.clone())
        {
            tracked.remove(&oldest);
        }
        let egress = RunEgress::new(policy);
        tracked.insert(
            key,
            Tracked {
                egress: Arc::clone(&egress),
                since: now,
            },
        );
        egress
    }

    /// The command's cumulative summary, for the report sent before
    /// settlement; `None` when nothing was reached or refused (or the
    /// command never built a client). The command stays tracked until
    /// [`Self::forget`]: a retried settlement reports the same snapshot.
    #[must_use]
    pub fn summary_for_settlement(&self, identity: &ExecutionIdentityV1) -> Option<EgressSummary> {
        let tracked = self.tracked.lock().ok()?;
        let summary = tracked
            .get(&CommandKey::of_identity(identity))?
            .egress
            .summary();
        (!summary.is_empty()).then_some(summary)
    }

    /// Stop tracking a settled command.
    pub fn forget(&self, identity: &ExecutionIdentityV1) {
        if let Ok(mut tracked) = self.tracked.lock() {
            tracked.remove(&CommandKey::of_identity(identity));
        }
    }
}

/// [`EgressRegistry::run_egress`] on the process's registry.
#[must_use]
pub fn run_egress(command: &WorkerCommandV1) -> Arc<RunEgress> {
    EgressRegistry::global().run_egress(command)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::protocol::elitea::runtime::v1::WorkerEgressPolicyV1;

    fn command(execution_id: &str, policy: Option<WorkerEgressPolicyV1>) -> WorkerCommandV1 {
        WorkerCommandV1 {
            execution_id: execution_id.to_owned(),
            egress_policy: policy,
            ..WorkerCommandV1::default()
        }
    }

    fn allowlist(entries: &[&str]) -> WorkerEgressPolicyV1 {
        WorkerEgressPolicyV1 {
            mode: WorkerEgressModeV1::Allowlist as i32,
            allow: entries.iter().map(|entry| (*entry).to_owned()).collect(),
        }
    }

    #[test]
    fn absent_unspecified_and_open_are_open() {
        for policy in [
            None,
            Some(WorkerEgressPolicyV1::default()),
            Some(WorkerEgressPolicyV1 {
                mode: WorkerEgressModeV1::Open as i32,
                allow: vec!["ignored.example".to_owned()],
            }),
        ] {
            assert!(
                policy_from_command(&command("e", policy))
                    .expect("valid")
                    .is_open()
            );
        }
    }

    #[test]
    fn an_allowlist_is_parsed_and_fails_closed() {
        let policy = policy_from_command(&command(
            "e",
            Some(allowlist(&[
                "api.example.com:443",
                "*.atlassian.net",
                "10.0.0.0/8",
            ])),
        ))
        .expect("valid");
        assert!(policy.admits("api.example.com", 443));
        assert!(!policy.admits("api.example.com", 8443));
        assert!(!policy.admits("evil.example", 443));
        assert!(
            !policy_from_command(&command("e", Some(allowlist(&[]))))
                .expect("an empty allowlist is valid")
                .admits("api.example.com", 443)
        );
        for bad in [
            &["https://api.example.com"][..],
            &["ok.example", ""],
            &["a b"],
        ] {
            assert!(matches!(
                policy_from_command(&command("e", Some(allowlist(bad)))),
                Err(ProtocolError::InvalidInput(_))
            ));
        }
        let many: Vec<String> = (0..1_025)
            .map(|index| format!("h{index}.example"))
            .collect();
        assert!(matches!(
            policy_from_command(&command(
                "e",
                Some(WorkerEgressPolicyV1 {
                    mode: WorkerEgressModeV1::Allowlist as i32,
                    allow: many,
                })
            )),
            Err(ProtocolError::ResourceExhausted(_))
        ));
        assert!(matches!(
            policy_from_command(&command(
                "e",
                Some(WorkerEgressPolicyV1 {
                    mode: 9,
                    allow: Vec::new(),
                })
            )),
            Err(ProtocolError::InvalidInput(_))
        ));
    }

    fn command_at(
        execution_id: &str,
        generation: u64,
        policy: Option<WorkerEgressPolicyV1>,
    ) -> WorkerCommandV1 {
        WorkerCommandV1 {
            command_id: format!("{execution_id}-command-{generation}"),
            generation,
            ..command(execution_id, policy)
        }
    }

    fn identity_of(command: &WorkerCommandV1) -> ExecutionIdentityV1 {
        ExecutionIdentityV1 {
            command_id: command.command_id.clone(),
            execution_id: command.execution_id.clone(),
            generation: command.generation,
            ..ExecutionIdentityV1::default()
        }
    }

    #[test]
    fn one_command_shares_one_ledger_until_forgotten() {
        let registry = EgressRegistry::default();
        let signed = command_at("e-1", 1, Some(allowlist(&["a.example"])));
        let first = registry.run_egress(&signed);
        let again = registry.run_egress(&signed);
        assert!(Arc::ptr_eq(&first, &again));
        let identity = identity_of(&signed);
        assert!(registry.summary_for_settlement(&identity).is_none());
        let _ = first.check("b.example", 443);
        let summary = registry.summary_for_settlement(&identity).expect("summary");
        assert_eq!(summary.refused[0].host, "b.example");
        registry.forget(&identity);
        assert!(registry.summary_for_settlement(&identity).is_none());
        // A malformed policy never opens egress.
        let closed = registry.run_egress(&command_at("e-2", 1, Some(allowlist(&["a b"]))));
        assert!(!closed.policy().admits("a.example", 443));
    }

    #[test]
    fn a_new_generation_runs_under_its_own_policy_and_ledger() {
        let registry = EgressRegistry::default();
        let wide = command_at("e-1", 1, None);
        let first = registry.run_egress(&wide);
        assert!(first.policy().is_open());
        assert!(first.check("wide.example", 443).is_ok());

        // The retry (generation 2) carries a narrower signed policy: it is
        // enforced, and the ledger starts empty.
        let narrow = command_at("e-1", 2, Some(allowlist(&["a.example"])));
        let second = registry.run_egress(&narrow);
        assert!(!Arc::ptr_eq(&first, &second));
        assert!(second.check("wide.example", 443).is_err());
        assert!(second.check("a.example", 443).is_ok());
        let summary = registry
            .summary_for_settlement(&identity_of(&narrow))
            .expect("summary");
        assert_eq!(summary.refused.len(), 1);
        assert_eq!(summary.refused[0].host, "wide.example");
        // The superseded generation is no longer tracked.
        assert!(
            registry
                .summary_for_settlement(&identity_of(&wide))
                .is_none()
        );

        // Same execution and generation, another command id: its own entry.
        let other = WorkerCommandV1 {
            command_id: "e-1-other".to_owned(),
            ..command_at("e-1", 2, None)
        };
        let third = registry.run_egress(&other);
        assert!(!Arc::ptr_eq(&second, &third));
        assert!(third.policy().is_open());
        assert!(
            registry
                .summary_for_settlement(&identity_of(&other))
                .is_none()
        );
    }

    #[test]
    fn a_tracked_entry_with_another_policy_is_never_served() {
        let registry = EgressRegistry::default();
        let open = command_at("e-1", 1, None);
        let first = registry.run_egress(&open);
        let narrowed = WorkerCommandV1 {
            egress_policy: Some(allowlist(&["a.example"])),
            ..open
        };
        let second = registry.run_egress(&narrowed);
        assert!(!Arc::ptr_eq(&first, &second));
        assert!(!second.policy().admits("wide.example", 443));
    }
}
