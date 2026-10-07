//! Repository ingest (ADR-0026 phase 2, decision 7): from the `repo_config`
//! the Go host sends to a checked-out working tree in the job's scratch
//! directory.
//!
//! The order is the security argument:
//!
//! 1. an artifact-folder source (`provider_type: artifact`,
//!    `artifact://bucket[/prefix]`) takes the other road: [`artifact`]
//!    lists and downloads it from the platform's object API with the
//!    invocation's callback bearer, under the same limits, and steps 2–4
//!    do not apply (there is no git host);
//! 2. [`providers::clone_target`] derives a credential-free URL, the
//!    branch, the identity and the `Authorization` value;
//! 3. [`egress::EgressPolicy::admit`] checks the URL's own host against
//!    `ELITEA_DEEPWIKI_GIT_ALLOWLIST` — BEFORE the credential is used;
//! 4. [`clone::clone_repository`] resolves the branch, clones depth 1 into
//!    the scratch directory under the limits, and verifies containment.
//!
//! See each module for what differs from the Python engine, and why.

pub mod artifact;
pub mod clone;
pub mod egress;
pub mod identity;
pub mod limits;
pub mod providers;
pub use elitea_engine_core::secret;

use crate::errors::{EngineError, ErrorType};
use artifact::{ArtifactCaps, ArtifactTarget, PlatformObjects};
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
    /// Python's own caps on an artifact folder, on top of `limits`.
    pub artifact: ArtifactCaps,
}

/// What a `repo_config` names, admitted.
#[derive(Debug)]
pub enum Admitted {
    /// A git repository whose host passed the allowlist.
    Git(egress::AdmittedTarget),
    /// A folder of the invoking project's artifact store.
    Artifact(ArtifactTarget),
}

impl Admitted {
    /// The `provider_type` the result reports.
    #[must_use]
    pub fn provider(&self) -> &str {
        match self {
            Self::Git(admitted) => admitted.target().provider().value(),
            Self::Artifact(_) => artifact::ARTIFACT_PROVIDER_TYPE,
        }
    }

    /// The repository the identifier is built from.
    #[must_use]
    pub fn repository(&self) -> &str {
        match self {
            Self::Git(admitted) => admitted.target().repo_identifier(),
            Self::Artifact(target) => target.repository(),
        }
    }

    /// The branch to check out (git) or the label (a folder).
    #[must_use]
    pub fn branch(&self) -> &str {
        match self {
            Self::Git(admitted) => admitted.target().branch(),
            Self::Artifact(target) => target.branch(),
        }
    }
}

/// Admit what a `repo_config` names: an artifact folder as it is (step 1),
/// a git repository through steps 2–3.
///
/// # Errors
///
/// An unusable artifact source, or the refusal of [`admit`].
pub fn admit_source(repo_config: &Value, policy: &EgressPolicy) -> Result<Admitted, EngineError> {
    if let Some(target) = ArtifactTarget::from_repo_config(repo_config)? {
        return Ok(Admitted::Artifact(target));
    }
    admit(repo_config, policy).map(Admitted::Git)
}

/// Derive and admit a GIT clone target without cloning: steps 2–3.
///
/// # Errors
///
/// The refusal of step 1, 2 or 3.
pub fn admit(
    repo_config: &Value,
    policy: &EgressPolicy,
) -> Result<egress::AdmittedTarget, EngineError> {
    if artifact::names_artifact_folder(repo_config) {
        // A folder has no clone target; `admit_source` takes it.
        return Err(EngineError::new(
            ErrorType::Value,
            "This repo_config names a bucket folder, which is not a git repository to clone",
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

/// Download an artifact folder into `job_scratch` (step 1's road), under
/// the ingest limits and deadline.
///
/// `client` is the model transport's (rustls, no redirects); `platform` the
/// invocation's own object API and bearer. At the deadline the transfer is
/// dropped, which removes its partial directory.
///
/// # Errors
///
/// A refused key, a limit, a transfer failure, the timeout or the stop.
pub async fn ingest_artifact(
    target: &ArtifactTarget,
    platform: &PlatformObjects,
    client: &reqwest::Client,
    settings: &IngestSettings,
    job_scratch: &Path,
    cancel: Arc<AtomicBool>,
) -> Result<ClonedRepository, EngineError> {
    let run = artifact::Materialise {
        target,
        platform,
        client,
        limits: &settings.limits,
        caps: settings.artifact,
        job_scratch,
        cancel: &cancel,
    }
    .run();
    if let Ok(outcome) = tokio::time::timeout(settings.limits.clone_timeout, run).await {
        outcome
    } else {
        cancel.store(true, Ordering::Release);
        Err(artifact::timeout_error(
            target,
            settings.limits.clone_timeout,
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::errors::classify;
    use serde_json::json;

    #[test]
    fn artifact_folders_take_their_own_road() {
        let policy = EgressPolicy::parse(None);
        for config in [
            json!({"provider_type": "artifact", "provider_config": {"bucket": "docs"}, "repository": "artifact://docs/handbook"}),
            json!({"provider_type": "github", "repository": "ARTIFACT://Docs/handbook/", "branch": "v2"}),
        ] {
            // Never a git target, whatever the allowlist says.
            let error = admit(&config, &policy).err();
            assert!(error.is_some(), "{config}: admitted as git");
            let admitted = admit_source(&config, &policy);
            let Ok(Admitted::Artifact(target)) = admitted else {
                panic!("{config}: {admitted:?}");
            };
            assert_eq!(target.source().url(), "artifact://docs/handbook");
            assert_eq!(Admitted::Artifact(target.clone()).provider(), "artifact");
        }
        // provider_type artifact without an artifact:// repository.
        let error = admit_source(
            &json!({"provider_type": "artifact", "repository": "docs/handbook"}),
            &policy,
        )
        .err();
        assert!(error.is_some_and(|e| e.message.contains("not artifact://")));
        // An unusable source is refused before anything else.
        let error = admit_source(&json!({"repository": "artifact://docs/a/../b"}), &policy).err();
        assert_eq!(
            error.map(|e| classify(e.error_type, &e.message)),
            Some("invalid_input")
        );
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
            artifact: ArtifactCaps::default(),
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
