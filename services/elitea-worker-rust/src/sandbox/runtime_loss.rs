//! "The runtime was lost" versus "the user's code failed" (FU-214).
//!
//! The runner is the container's PID 1 (`exec elitea-code-runner`). It writes
//! exactly one envelope to stdout and exits 0, also when the user's program
//! fails, so a user failure always arrives as an envelope. Code inside the
//! container cannot signal PID 1 of its own PID namespace. A runtime is
//! therefore *lost* only when the container platform itself shows that the
//! runner never finished: the container is gone, or it stopped through a
//! signal or an unknown kubelet state, without an envelope. That evidence comes
//! from the container runtime, never from code output: the log validators do
//! not accept the synthesized `runtime_lost` status from a runner envelope.
//!
//! An out-of-memory kill stays `memory_limit`: a resource limit the program
//! reached repeats on a re-run, and Docker cannot tell a host-level kill from
//! the container's own limit.
#![cfg_attr(
    not(feature = "sandbox-supervisor"),
    allow(
        dead_code,
        reason = "Only the supervisor runtime adapters read the platform evidence."
    )
)]

/// Supervisor ledger failure code for a lost runtime. Earlier Workers read it
/// as an ordinary sandbox failure; it fits the ledger `failure_code` check.
pub const FAILURE_CODE: &str = "sandbox.runtime_lost";
/// Synthesized envelope status. Only the runtime adapters produce it.
pub(crate) const RECEIPT_STATUS: &str = "runtime_lost";
/// Exit codes of a runner that did not finish: SIGKILL and SIGTERM from outside
/// its PID namespace (`docker kill`, a daemon or kubelet stop, an eviction),
/// and 255, the code a container runtime reports for a container whose state
/// it lost (a node or daemon restart).
pub(crate) const LOST_EXIT_CODES: [i64; 3] = [137, 143, 255];
/// Kubelet container reasons for a container whose state was lost.
const LOST_CONTAINER_REASONS: [&str; 2] = ["Unknown", "ContainerStatusUnknown"];
/// Pod reasons for a Pod the platform stopped: preemption, node loss and node
/// shutdown. A kubelet eviction (`Evicted`) is deliberately absent: the
/// kubelet also evicts a Pod for its own workspace or ephemeral-storage use,
/// which the code causes and a re-run repeats.
const LOST_POD_REASONS: [&str; 5] = [
    "Preempting",
    "NodeLost",
    "Terminated",
    "Shutdown",
    "NodeShutdown",
];
/// `DisruptionTarget` reason of a kubelet-initiated termination (eviction for
/// resource pressure or the Pod's own limits). Not loss evidence by itself.
const KUBELET_TERMINATION: &str = "TerminationByKubelet";

/// The container runtime's view of a stopped Docker Code job whose envelope
/// could not be read.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum StoppedRuntime {
    /// The bound container no longer exists. Docker runs it on this host only,
    /// so a missing container cannot still be running.
    AbsentAfterBinding,
    Exited {
        exit_code: Option<i64>,
        oom_killed: bool,
        wrote_output: bool,
    },
}

/// The envelope status this evidence proves, if any. `None` keeps today's
/// answer: the receipt is invalid and completion stays unconfirmed.
pub(crate) fn docker_status(stopped: StoppedRuntime) -> Option<&'static str> {
    match stopped {
        StoppedRuntime::AbsentAfterBinding => Some(RECEIPT_STATUS),
        // Any output means the runner was writing its envelope; a damaged
        // envelope is not evidence of loss.
        StoppedRuntime::Exited {
            wrote_output: true, ..
        } => None,
        StoppedRuntime::Exited {
            oom_killed: true, ..
        } => Some("memory_limit"),
        StoppedRuntime::Exited {
            exit_code: Some(code),
            ..
        } if LOST_EXIT_CODES.contains(&code) => Some(RECEIPT_STATUS),
        StoppedRuntime::Exited { .. } => None,
    }
}

/// Whether a terminated Kubernetes `code` container that exited non-zero (and
/// not by `OOMKilled` or the Pod deadline) was lost rather than failed.
///
/// `disruption` is the reason of a true `DisruptionTarget` condition. A node
/// shutdown or node loss is loss; a kubelet eviction is not, because the Pod's
/// own resource use can cause it; any other platform disruption (taint
/// manager, preemption, Pod GC, an API eviction during a drain) is loss.
pub(crate) fn kubernetes_lost(
    exit_code: i32,
    container_reason: Option<&str>,
    pod_reason: Option<&str>,
    disruption: Option<&str>,
) -> bool {
    if exit_code == 0 {
        return false;
    }
    if pod_reason.is_some_and(|reason| LOST_POD_REASONS.contains(&reason)) {
        return true;
    }
    if pod_reason == Some("Evicted") || disruption == Some(KUBELET_TERMINATION) {
        return false;
    }
    LOST_EXIT_CODES.contains(&i64::from(exit_code))
        || container_reason.is_some_and(|reason| LOST_CONTAINER_REASONS.contains(&reason))
        || disruption.is_some()
}

/// The bounded envelope a runtime adapter synthesizes from platform evidence.
pub(crate) fn envelope(status: &str, exit_code: Option<i64>) -> Result<Vec<u8>, serde_json::Error> {
    serde_json::to_vec(&serde_json::json!({
        "revision": 1, "status": status, "exit_code": exit_code,
        "stdout": "", "stderr": ""
    }))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn docker_loss_needs_platform_evidence_and_no_output() {
        let exited = |exit_code, oom_killed, wrote_output| StoppedRuntime::Exited {
            exit_code,
            oom_killed,
            wrote_output,
        };
        assert_eq!(
            docker_status(StoppedRuntime::AbsentAfterBinding),
            Some(RECEIPT_STATUS)
        );
        for code in LOST_EXIT_CODES {
            assert_eq!(
                docker_status(exited(Some(code), false, false)),
                Some(RECEIPT_STATUS)
            );
            // An envelope was being written: never loss.
            assert_eq!(docker_status(exited(Some(code), false, true)), None);
        }
        assert_eq!(
            docker_status(exited(Some(137), true, false)),
            Some("memory_limit")
        );
        // A runner error exit (1), a panic (101), a clean exit without output
        // and an unknown exit code are not loss evidence.
        for code in [Some(0), Some(1), Some(101), Some(134), None] {
            assert_eq!(docker_status(exited(code, false, false)), None, "{code:?}");
        }
    }

    #[test]
    fn kubernetes_loss_is_a_signal_or_a_lost_state_never_a_runner_error() {
        assert!(kubernetes_lost(255, Some("Unknown"), None, None));
        assert!(kubernetes_lost(137, Some("Error"), None, None));
        assert!(kubernetes_lost(143, Some("Error"), None, None));
        assert!(kubernetes_lost(
            1,
            Some("ContainerStatusUnknown"),
            None,
            None
        ));
        assert!(kubernetes_lost(
            2,
            Some("Error"),
            None,
            Some("DeletionByTaintManager")
        ));
        assert!(kubernetes_lost(
            2,
            Some("Error"),
            None,
            Some("PreemptionByScheduler")
        ));
        // A graceful node shutdown is reported by the kubelet, but it is loss.
        assert!(kubernetes_lost(
            143,
            Some("Error"),
            Some("Terminated"),
            Some("TerminationByKubelet")
        ));
        assert!(!kubernetes_lost(1, Some("Error"), None, None));
        assert!(!kubernetes_lost(101, None, Some("Running"), None));
        assert!(!kubernetes_lost(
            0,
            Some("Unknown"),
            Some("NodeLost"),
            Some("DeletionByTaintManager")
        ));
    }

    /// A kubelet eviction can be the code's own doing (a full workspace or
    /// ephemeral storage): it repeats on a re-run, so it is never loss, even
    /// though the kubelet kills the runner with a signal.
    #[test]
    fn a_kubelet_eviction_is_not_loss() {
        assert!(!kubernetes_lost(
            137,
            Some("Error"),
            Some("Evicted"),
            Some("TerminationByKubelet")
        ));
        assert!(!kubernetes_lost(137, Some("Error"), Some("Evicted"), None));
        assert!(!kubernetes_lost(
            143,
            Some("Error"),
            None,
            Some("TerminationByKubelet")
        ));
    }

    /// Main pins the same allowance (`MaxRuntimeLostRetries`) from this file.
    #[test]
    fn the_retry_allowance_matches_the_shared_policy() {
        use crate::agents::graph::node_recovery::{
            MAX_RUNTIME_LOST_RETRIES, RUNTIME_LOST_RETRY_WINDOW_MS,
        };
        let policy: serde_json::Value = serde_json::from_str(include_str!(
            "../../../../testdata/proto/runtime/v1/code_runtime_lost_retry_policy.json"
        ))
        .unwrap();
        assert_eq!(
            policy["schema"],
            "elitea.runtime.code-runtime-lost-retry-policy.v1"
        );
        assert_eq!(policy["supervisor_failure_code"], FAILURE_CODE);
        assert_eq!(
            policy["max_runtime_lost_retries"],
            u64::from(MAX_RUNTIME_LOST_RETRIES)
        );
        assert_eq!(policy["retry_window_ms"], RUNTIME_LOST_RETRY_WINDOW_MS);
        let terminal = crate::protocol::output::runtime_error(
            crate::protocol::output::model_failure(Some("pipeline.code_failed")),
        );
        let name =
            crate::protocol::elitea::runtime::v1::RuntimeErrorCodeV1::try_from(terminal.code)
                .unwrap()
                .as_str_name();
        assert_eq!(
            Some(name),
            policy["terminal_error_code"]
                .as_str()
                .map(|code| format!("RUNTIME_ERROR_CODE_V1_{code}"))
                .as_deref()
        );
    }

    #[test]
    fn the_envelope_is_the_runner_shape_without_output() {
        let value: serde_json::Value =
            serde_json::from_slice(&envelope(RECEIPT_STATUS, Some(255)).unwrap()).unwrap();
        assert_eq!(
            value,
            serde_json::json!({"revision":1,"status":"runtime_lost","exit_code":255,"stdout":"","stderr":""})
        );
    }
}
