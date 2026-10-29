//! The command's public egress policy (ADR-0044 decision 2, #1490).
//!
//! Main puts `WorkerCommandV1.egress_policy` (field 66) on the signed
//! command: absent, `UNSPECIFIED` or `OPEN` is open (any public host, today's
//! behaviour); `ALLOWLIST` admits only the hosts its entries name, in the
//! egresslib grammar. A malformed entry, too many entries or an unknown mode
//! fails the command closed at verification ([`policy_from_command`]); an
//! entry is never skipped.
//!
//! One [`RunEgress`] per execution holds the policy and the ledger of hosts
//! reached and refused. The registry here hands every path that runs part of
//! the execution (an agent turn, its checkpoint recovery, a direct toolkit
//! call) the same instance, so the summary the worker reports before
//! settlement ([`EgressRegistry::summary_for_settlement`]) covers all of it.
//! A settled execution is forgotten: a later turn of the same execution (a
//! resumed pause) starts a new ledger, and its report replaces the earlier
//! one on Main.

use std::collections::HashMap;
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use elitea_agent_runtime::egress::{
    Allowlist, EgressSummary, PublicPolicy, PublicPolicyError, RunEgress,
};

use super::ProtocolError;
use super::elitea::runtime::v1::{WorkerCommandV1, WorkerEgressModeV1};

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

struct Tracked {
    egress: Arc<RunEgress>,
    since: Instant,
}

/// The executions this process is running, each with its one [`RunEgress`].
/// Production uses [`EgressRegistry::global`]; a test may hold its own.
#[derive(Default)]
pub struct EgressRegistry {
    tracked: Mutex<HashMap<String, Tracked>>,
}

impl EgressRegistry {
    /// The process's registry.
    #[must_use]
    pub fn global() -> Arc<Self> {
        static GLOBAL: OnceLock<Arc<EgressRegistry>> = OnceLock::new();
        Arc::clone(GLOBAL.get_or_init(|| Arc::new(Self::default())))
    }

    /// The execution's egress: the same instance for every caller of one
    /// execution. A command that somehow failed validation gets an EMPTY
    /// allowlist (nothing public is reached), never an open policy.
    #[must_use]
    pub fn run_egress(&self, command: &WorkerCommandV1) -> Arc<RunEgress> {
        let policy = policy_from_command(command)
            .unwrap_or_else(|_| PublicPolicy::Allowlist(Allowlist::default()));
        let Ok(mut tracked) = self.tracked.lock() else {
            return RunEgress::new(policy);
        };
        if let Some(existing) = tracked.get(&command.execution_id) {
            return Arc::clone(&existing.egress);
        }
        let now = Instant::now();
        tracked.retain(|_, entry| now.duration_since(entry.since) < MAX_TRACKED_AGE);
        if tracked.len() >= MAX_TRACKED_EXECUTIONS
            && let Some(oldest) = tracked
                .iter()
                .min_by_key(|(_, entry)| entry.since)
                .map(|(key, _)| key.clone())
        {
            tracked.remove(&oldest);
        }
        let egress = RunEgress::new(policy);
        tracked.insert(
            command.execution_id.clone(),
            Tracked {
                egress: Arc::clone(&egress),
                since: now,
            },
        );
        egress
    }

    /// The execution's cumulative summary, for the report sent before
    /// settlement; `None` when nothing was reached or refused (or the
    /// execution never built a client). The execution stays tracked until
    /// [`Self::forget`]: a retried settlement reports the same snapshot.
    #[must_use]
    pub fn summary_for_settlement(&self, execution_id: &str) -> Option<EgressSummary> {
        let tracked = self.tracked.lock().ok()?;
        let summary = tracked.get(execution_id)?.egress.summary();
        (!summary.is_empty()).then_some(summary)
    }

    /// Stop tracking a settled execution.
    pub fn forget(&self, execution_id: &str) {
        if let Ok(mut tracked) = self.tracked.lock() {
            tracked.remove(execution_id);
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

    #[test]
    fn one_execution_shares_one_ledger_until_forgotten() {
        let registry = EgressRegistry::default();
        let first = registry.run_egress(&command("e-1", Some(allowlist(&["a.example"]))));
        let again = registry.run_egress(&command("e-1", None));
        assert!(Arc::ptr_eq(&first, &again));
        assert!(registry.summary_for_settlement("e-1").is_none());
        let _ = first.check("b.example", 443);
        let summary = registry.summary_for_settlement("e-1").expect("summary");
        assert_eq!(summary.refused[0].host, "b.example");
        registry.forget("e-1");
        assert!(registry.summary_for_settlement("e-1").is_none());
        // A malformed policy never opens egress.
        let closed = registry.run_egress(&command("e-2", Some(allowlist(&["a b"]))));
        assert!(!closed.policy().admits("a.example", 443));
    }
}
