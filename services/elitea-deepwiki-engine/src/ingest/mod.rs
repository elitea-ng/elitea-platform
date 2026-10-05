//! Repository ingest (ADR-0026 phase 2, decision 7): from the `repo_config`
//! the Go host sends to a checked-out working tree in the job's scratch
//! directory.
//!
//! The order is the security argument:
//!
//! 1. an artifact-folder source is refused (a later phase ports
//!    `artifact_source.py`);
//! 2. [`providers::clone_target`] derives a credential-free URL, the
//!    branch, the identity and the `Authorization` value;
//! 3. [`egress::EgressPolicy::admit`] checks the URL's own host against
//!    `ELITEA_DEEPWIKI_GIT_ALLOWLIST` — BEFORE the credential is used;
//! 4. [`clone::clone_repository`] resolves the branch, clones depth 1 into
//!    the scratch directory under the limits, and verifies containment.
//!
//! See each module for what differs from the Python engine, and why.

pub mod clone;
pub mod egress;
pub mod identity;
pub mod limits;
pub mod providers;
pub mod secret;

use crate::errors::{EngineError, ErrorType};
use crate::source::{is_artifact_source, py_str, py_truthy};
use egress::EgressPolicy;
use limits::IngestLimits;
use serde_json::Value;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

pub use clone::ClonedRepository;

/// What the ingest reads from the settings.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct IngestSettings {
    pub git_allowlist: EgressPolicy,
    pub limits: IngestLimits,
    /// The root under which each job gets its scratch directory
    /// (`ELITEA_DEEPWIKI_SCRATCH_PATH`, Python's `scratch_path`).
    pub scratch_path: PathBuf,
}

/// Whether a `repo_config` names an artifact folder (`provider_type:
/// artifact` from the Go host, or an `artifact://` repository).
fn names_artifact_folder(repo_config: &Value) -> bool {
    let field = |key: &str| {
        repo_config
            .get(key)
            .filter(|v| py_truthy(v))
            .map(py_str)
            .unwrap_or_default()
    };
    field("provider_type")
        .trim()
        .eq_ignore_ascii_case("artifact")
        || is_artifact_source(&field("repository"))
}

/// Derive and admit a clone target without cloning: steps 1–3.
///
/// # Errors
///
/// The refusal of step 1, 2 or 3.
pub fn admit(
    repo_config: &Value,
    policy: &EgressPolicy,
) -> Result<egress::AdmittedTarget, EngineError> {
    if names_artifact_folder(repo_config) {
        // Worded without "artifact" or "download": the legacy classifier
        // would file those under artifact_error, and this is a runtime
        // limitation of this engine, not a failed transfer.
        return Err(EngineError::new(
            ErrorType::Runtime,
            "This engine cannot read a bucket-folder source yet (ADR-0026: a later phase ports it); run the Python engine for folder sources",
        ));
    }
    let target = providers::clone_target(repo_config)?;
    policy.admit(target)
}

/// Ingest the repository a `repo_config` names into `job_scratch`.
///
/// Setting `cancel` stops the clone at its next checkpoint.
///
/// # Errors
///
/// Any refusal of [`admit`], or a failure of [`ingest_admitted`].
pub async fn ingest(
    repo_config: &Value,
    settings: &IngestSettings,
    job_scratch: &Path,
    cancel: Arc<AtomicBool>,
) -> Result<ClonedRepository, EngineError> {
    let admitted = admit(repo_config, &settings.git_allowlist)?;
    ingest_admitted(admitted, settings.limits, job_scratch, cancel).await
}

/// Clone an admitted target on a blocking thread, under the deadline.
///
/// The deadline is enforced twice: inside the clone (a watchdog
/// interrupts the fetch and the checkout) and here, because a request the
/// remote never answers blocks in a socket read gitoxide cannot interrupt.
/// Here, at the deadline, the clone is reported as timed out at once; its
/// thread is told to stop and removes its partial directory when it ends.
///
/// # Errors
///
/// A failure of [`clone::clone_repository`], or the timeout.
pub async fn ingest_admitted(
    admitted: egress::AdmittedTarget,
    limits: IngestLimits,
    job_scratch: &Path,
    cancel: Arc<AtomicBool>,
) -> Result<ClonedRepository, EngineError> {
    let repo = admitted.target().repo_identifier().to_owned();
    let branch = admitted.target().branch().to_owned();
    let scratch = job_scratch.to_path_buf();
    let stop = Arc::clone(&cancel);
    let worker = tokio::task::spawn_blocking(move || {
        clone::clone_repository(&admitted, &scratch, &limits, &stop)
    });
    match tokio::time::timeout(limits.clone_timeout, worker).await {
        Ok(Ok(outcome)) => outcome,
        Ok(Err(join)) => Err(EngineError::new(
            ErrorType::Runtime,
            format!("Git clone failed: the clone task ended abnormally ({join})"),
        )),
        Err(_) => {
            cancel.store(true, Ordering::Release);
            Err(clone::timeout_error(&repo, &branch, limits.clone_timeout))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::errors::classify;
    use serde_json::json;

    #[test]
    fn artifact_folders_are_not_supported_yet() {
        let policy = EgressPolicy::parse(Some("*"));
        for config in [
            json!({"provider_type": "artifact", "provider_config": {"bucket": "docs"}, "repository": "docs/handbook"}),
            json!({"provider_type": "github", "repository": "artifact://docs/handbook"}),
        ] {
            let error = admit(&config, &policy).err();
            let error = error.unwrap_or_else(|| panic!("{config}: admitted"));
            assert!(
                error
                    .message
                    .contains("not read a bucket-folder source yet"),
                "{error}"
            );
            assert_eq!(classify(error.error_type, &error.message), "runtime_error");
        }
    }

    #[test]
    fn the_derived_host_is_checked_before_anything_else() {
        let config = json!({"provider_type": "github", "provider_config": {"base_url": "https://ghe.attacker.example/api/v3", "access_token": "ghp_x"}, "repository": "owner/repo"});
        let error = admit(&config, &EgressPolicy::parse(Some("github.com"))).err();
        assert!(
            error
                .as_ref()
                .is_some_and(|e| e.message.contains("'ghe.attacker.example'")
                    && !e.message.contains("ghp_x")),
            "{error:?}"
        );
        assert!(admit(&config, &EgressPolicy::parse(None)).is_err());
        let public = json!({"provider_type": "github", "provider_config": {"base_url": "https://api.github.com"}, "repository": "o/r"});
        let admitted = admit(
            &public,
            &EgressPolicy::parse(Some("github.com,*.github.com")),
        );
        assert_eq!(
            admitted
                .map(|a| a.target().host().to_owned())
                .ok()
                .as_deref(),
            Some("github.com")
        );
    }

    #[tokio::test]
    async fn a_refused_host_never_reaches_the_network() {
        let settings = IngestSettings {
            git_allowlist: EgressPolicy::parse(Some("github.com")),
            limits: IngestLimits::default(),
            scratch_path: PathBuf::from("/nonexistent"),
        };
        let config = json!({"provider_type": "gitlab", "provider_config": {"url": "https://gitlab.example.com"}, "repository": "g/p"});
        let error = ingest(
            &config,
            &settings,
            Path::new("/nonexistent/job"),
            Arc::new(AtomicBool::new(false)),
        )
        .await;
        assert!(error.is_err_and(|e| e.message.contains("not on the git-host allowlist")));
    }
}
