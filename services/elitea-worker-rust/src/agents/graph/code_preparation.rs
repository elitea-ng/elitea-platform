//! Keep the Code frontier pending until frozen dependencies are published and terminal.
use super::{CodeRuntimeProfile, RemoteCodeRuntime, failed, observation_deadline, uncertain};
use crate::agents::graph::code_timing;
use crate::{
    agents::graph::{
        code::CodeLanguage,
        code_runtime::{CodeInvocation, CodePreparationFailure},
    },
    sandbox::{
        client::{PreparationOutcome, PublicationOutcome, SandboxCallError},
        dependency_bundle::DependencyBundle,
        preparation::{PreparationJob, preparation_activation},
    },
};
use adk_rust::graph::GraphError;
use std::{future::Future, time::Duration};

impl RemoteCodeRuntime {
    pub(super) async fn prepare_execution_job(
        &self,
        invocation: &CodeInvocation<'_>,
        profile: &CodeRuntimeProfile,
    ) -> Result<
        (
            crate::sandbox::request::PreparedJob,
            Option<DependencyBundle>,
        ),
        GraphError,
    > {
        self.prepare_execution_job_diagnosed(invocation, profile)
            .await
            .map_err(CodePreparationFailure::graph_error)
    }

    pub(super) async fn prepare_execution_job_diagnosed(
        &self,
        invocation: &CodeInvocation<'_>,
        profile: &CodeRuntimeProfile,
    ) -> Result<
        (
            crate::sandbox::request::PreparedJob,
            Option<DependencyBundle>,
        ),
        CodePreparationFailure,
    > {
        self.admit_preparation_mode(invocation, profile)
            .await
            .map_err(|_| CodePreparationFailure::Failed)?;
        let mut job =
            super::prepare_job(invocation, profile).map_err(|_| CodePreparationFailure::Failed)?;
        let bundle = if requires_dependency_preparation(invocation, profile) {
            let bundle = self
                .prepare_python_dependencies(invocation, profile)
                .await?;
            job = if let Some(native) = bundle.native() {
                job.with_native_dependency_bundle(
                    bundle.root().to_owned(),
                    crate::sandbox::request::NativeDependencies {
                        kind: native.record.kind,
                        platform: native.record.platform.clone(),
                        preparation_sha256: native.record.preparation_sha256.clone(),
                        source_sha256: native.record.source_sha256.clone(),
                        dependencies_toml: invocation.dependencies_toml.map(str::to_owned),
                    },
                )
            } else {
                job.with_python_dependency_bundle(bundle.root().to_owned())
            }
            .map_err(|_| CodePreparationFailure::Failed)?;
            Some(bundle)
        } else {
            None
        };
        Ok((job, bundle))
    }

    pub(super) async fn admit_preparation_mode(
        &self,
        invocation: &CodeInvocation<'_>,
        profile: &CodeRuntimeProfile,
    ) -> Result<(), GraphError> {
        if !requires_dependency_preparation(invocation, profile) {
            let scope = self
                .authority
                .dispatch_scope()
                .map_err(|error| failed(&error.to_string()))?;
            let recorded = self
                .journal
                .contains_activation(&scope, &preparation_activation(&invocation.activation))
                .await
                .map_err(|error| failed(&error.to_string()))?;
            refuse_preparation_downgrade(recorded)?;
        }
        Ok(())
    }

    pub(super) async fn submit_prepared_job(
        &self,
        profile: &CodeRuntimeProfile,
        activation: &[u8; 32],
        job: &crate::sandbox::request::PreparedJob,
        bundle: Option<&DependencyBundle>,
    ) -> Result<crate::sandbox::client::SandboxOutcome, SandboxCallError> {
        if let Some(bundle) = bundle {
            self.control
                .submit_sandbox_job_with_dependencies(
                    &profile.client,
                    &self.authority,
                    activation,
                    job,
                    bundle,
                )
                .await
        } else {
            self.control
                .submit_sandbox_job(&profile.client, &self.authority, activation, job)
                .await
        }
    }

    #[expect(
        clippy::too_many_lines,
        reason = "Keep the ordered journal, publication, and receipt transitions together"
    )]
    pub(super) async fn prepare_python_dependencies(
        &self,
        invocation: &CodeInvocation<'_>,
        profile: &CodeRuntimeProfile,
    ) -> Result<DependencyBundle, CodePreparationFailure> {
        let config = profile
            .preparation
            .as_ref()
            .ok_or(CodePreparationFailure::Failed)?;
        let client = profile
            .preparation_client
            .as_ref()
            .ok_or(CodePreparationFailure::Failed)?;
        let activation = preparation_activation(&invocation.activation);
        let job = if let Some(platform) = &config.native_platform {
            let (language, acquisition_source) =
                native_acquisition(invocation).map_err(|_| CodePreparationFailure::Failed)?;
            PreparationJob::new_native(
                language,
                acquisition_source.to_owned(),
                config.image_digest.clone(),
                config.policy_revision.clone(),
                config.timeout_seconds,
                platform.clone(),
                profile.image_digest.clone(),
                profile.policy_revision.clone(),
            )
        } else {
            if invocation.language == CodeLanguage::Rust {
                return Err(CodePreparationFailure::Failed);
            }
            PreparationJob::new(
                invocation.source.to_owned(),
                config.image_digest.clone(),
                config.policy_revision.clone(),
                config.timeout_seconds,
            )
        }
        .map_err(|_| CodePreparationFailure::Failed)?;
        let digest = job
            .fingerprint()
            .map_err(|_| CodePreparationFailure::Failed)?;
        let scope = self
            .authority
            .dispatch_scope()
            .map_err(|_| CodePreparationFailure::Failed)?;
        // Recorded preparation keeps its original runtime and publication lineage.
        // Only a fresh Cargo request can select the operator-pinned frozen root.
        let is_rust = invocation.language == CodeLanguage::Rust;
        let compiled = super::compiled::compiled_activation(&invocation.activation);
        // One round trip answers all identity probes (PREPARATION_IDENTITY_PROBES_BUDGET).
        let probe = [activation, invocation.activation, compiled];
        let found = self
            .journal
            .contains_activations(&scope, if is_rust { &probe } else { &probe[..1] })
            .await
            .map_err(|_| CodePreparationFailure::Failed)?;
        let (recorded, recorded_code) = recorded_identities(is_rust, &found);
        let root = (invocation.language == CodeLanguage::Rust)
            .then(|| self.selected_compiled_profile(profile))
            .flatten()
            .and_then(|selected| selected.dependency_bundle_root());
        resolve_preparation_bundle(
            recorded,
            recorded_code,
            root,
            |root| async {
                // A busy or briefly unavailable lookup waits for the same absolute
                // deadline as fresh preparation. It never falls through to prepare.
                let deadline = observation_deadline(config.timeout_seconds);
                let selected = retry_frozen_lookup(deadline, || async {
                    let lookup = self.control.lookup_sandbox_dependencies(
                        client,
                        &self.authority,
                        &activation,
                        &job,
                        root,
                    );
                    tokio::time::timeout(Duration::from_secs(config.timeout_seconds.into()), lookup)
                        .await
                        .map_err(|_| LookupAttempt::Final(CodePreparationFailure::Unconfirmed))?
                        .map_err(|error| {
                            if retryable(&error) {
                                LookupAttempt::Retry
                            } else {
                                LookupAttempt::Final(CodePreparationFailure::Failed)
                            }
                        })
                })
                .await?;
                if selected
                    .as_ref()
                    .is_some_and(|bundle| !job.matches_bundle(bundle))
                {
                    return Err(CodePreparationFailure::Failed);
                }
                Ok(selected)
            },
            || async {
                self.journal
                    .register(&scope, &activation, &digest, client.audience())
                    .await
                    .map_err(|_| CodePreparationFailure::Failed)?;
                let mut deadline = observation_deadline(config.timeout_seconds);
                let mut bundle: Option<DependencyBundle> = None;
                let mut index = 0;
                loop {
                    let mut wait = true;
                    if let Some(recorded) = &bundle {
                        let attempt = self.control.publish_sandbox_dependencies(
                            client,
                            &self.authority,
                            &activation,
                            &job,
                            recorded,
                            index,
                        );
                        match tokio::time::timeout_at(deadline, attempt)
                            .await
                            .map_err(|_| CodePreparationFailure::Unconfirmed)?
                        {
                            Ok(PublicationOutcome::Completed) => {
                                // A concurrent owner can complete publication before this
                                // indexed retry. Authority binds the locally retained root.
                                self.journal
                                    .resolve(&scope, &activation, &digest, client.audience())
                                    .await
                                    .map_err(|_| CodePreparationFailure::Failed)?;
                                return bundle.ok_or(CodePreparationFailure::Unconfirmed);
                            }
                            Ok(PublicationOutcome::Pending) => {
                                if index < recorded.file_count() {
                                    index += 1;
                                    wait = false;
                                }
                            }
                            Ok(PublicationOutcome::Failed { code }) => {
                                self.journal
                                    .resolve(&scope, &activation, &digest, client.audience())
                                    .await
                                    .map_err(|_| CodePreparationFailure::Failed)?;
                                return Err(preparation_failed(&code));
                            }
                            Ok(PublicationOutcome::Cancelled) => {
                                self.journal
                                    .resolve(&scope, &activation, &digest, client.audience())
                                    .await
                                    .map_err(|_| CodePreparationFailure::Failed)?;
                                return Err(preparation_cancelled());
                            }
                            Ok(PublicationOutcome::Uncertain { .. }) => {
                                return Err(CodePreparationFailure::Unconfirmed);
                            }
                            Err(error) if retryable(&error) => {}
                            Err(_) => return Err(CodePreparationFailure::Failed),
                        }
                    } else {
                        let attempt = self.control.prepare_sandbox_dependencies(
                            client,
                            &self.authority,
                            &activation,
                            &job,
                        );
                        match tokio::time::timeout_at(deadline, attempt)
                            .await
                            .map_err(|_| CodePreparationFailure::Unconfirmed)?
                        {
                            Ok(PreparationOutcome::Completed(recorded)) => {
                                self.journal
                                    .resolve(&scope, &activation, &digest, client.audience())
                                    .await
                                    .map_err(|_| CodePreparationFailure::Failed)?;
                                if !job.matches_bundle(&recorded) {
                                    return Err(CodePreparationFailure::Failed);
                                }
                                return Ok(recorded);
                            }
                            Ok(PreparationOutcome::Pending(recorded)) => {
                                wait = recorded.is_none();
                                if recorded.is_some() {
                                    // Resolution ends before indexed publication begins.
                                    // Retries keep this fixed publication deadline.
                                    deadline = observation_deadline(config.timeout_seconds);
                                }
                                if recorded.as_ref().is_some_and(|v| !job.matches_bundle(v)) {
                                    return Err(CodePreparationFailure::Failed);
                                }
                                bundle = recorded;
                            }
                            Ok(PreparationOutcome::Failed { code }) => {
                                self.journal
                                    .resolve(&scope, &activation, &digest, client.audience())
                                    .await
                                    .map_err(|_| CodePreparationFailure::Failed)?;
                                return Err(preparation_failed(&code));
                            }
                            Ok(PreparationOutcome::Cancelled) => {
                                self.journal
                                    .resolve(&scope, &activation, &digest, client.audience())
                                    .await
                                    .map_err(|_| CodePreparationFailure::Failed)?;
                                return Err(preparation_cancelled());
                            }
                            Ok(PreparationOutcome::Uncertain { .. }) => {
                                return Err(CodePreparationFailure::Unconfirmed);
                            }
                            Err(error) if retryable(&error) => {}
                            Err(_) => return Err(CodePreparationFailure::Failed),
                        }
                    }
                    if tokio::time::Instant::now() >= deadline {
                        return Err(CodePreparationFailure::Unconfirmed);
                    }
                    if wait {
                        tokio::time::sleep_until(
                            (tokio::time::Instant::now() + code_timing::CODE_RECONCILE_INTERVAL)
                                .min(deadline),
                        )
                        .await;
                    }
                }
            },
        )
        .await
    }

    pub(super) async fn hydrate_python_dependencies(
        &self,
        profile: &CodeRuntimeProfile,
        activation: &[u8; 32],
        job: &crate::sandbox::request::PreparedJob,
        bundle: &DependencyBundle,
        original_visit: Option<&crate::sandbox::code_recovery::OriginalCodeVisitRef>,
    ) -> Result<(), GraphError> {
        let config = profile
            .preparation
            .as_ref()
            .ok_or_else(|| failed("Dependency hydration is not configured."))?;
        let deadline = observation_deadline(config.timeout_seconds);
        let mut index = 0;
        loop {
            // Each index uses fresh current-claim authority over the same original job.
            let intent = if let Some(visit) = original_visit {
                let content = self
                    .intent_content
                    .as_deref()
                    .ok_or_else(|| failed("Original Code intent signing is not configured."))?;
                let digest = job
                    .fingerprint()
                    .map_err(|_| failed("The indexed Code execution request is invalid."))?;
                Some(
                    tokio::time::timeout_at(
                        deadline,
                        content.finalize_original_code_intent(
                            &self.authority,
                            visit,
                            *activation,
                            digest,
                            profile.client.audience(),
                            job,
                            None,
                            None,
                        ),
                    )
                    .await
                    .map_err(|_| uncertain())?
                    .map_err(|_| {
                        failed("The original Code intent does not authorize hydration.")
                    })?,
                )
            } else {
                None
            };
            let attempt = self.control.hydrate_sandbox_dependencies(
                &profile.client,
                &self.authority,
                activation,
                job,
                bundle,
                index,
                intent.as_deref(),
            );
            match tokio::time::timeout_at(deadline, attempt)
                .await
                .map_err(|_| uncertain())?
            {
                Ok(true) => return Ok(()),
                Ok(false) => {
                    index += 1;
                    continue;
                }
                Err(error) if retryable(&error) => {}
                Err(error) => return Err(failed(&error.to_string())),
            }
            if tokio::time::Instant::now() >= deadline {
                return Err(uncertain());
            }
            tokio::time::sleep_until(
                (tokio::time::Instant::now() + code_timing::CODE_RECONCILE_INTERVAL).min(deadline),
            )
            .await;
        }
    }
}

/// Keep lookup, original reconciliation, and fresh acquisition in one tested flow.
async fn resolve_preparation_bundle<'root, Lookup, Acquire, LookupFuture, AcquireFuture>(
    recorded_preparation: bool,
    recorded_code: bool,
    root: Option<&'root str>,
    lookup: Lookup,
    reconcile_or_prepare: Acquire,
) -> Result<DependencyBundle, CodePreparationFailure>
where
    Lookup: FnOnce(&'root str) -> LookupFuture,
    Acquire: FnOnce() -> AcquireFuture,
    LookupFuture: Future<Output = Result<Option<DependencyBundle>, CodePreparationFailure>>,
    AcquireFuture: Future<Output = Result<DependencyBundle, CodePreparationFailure>>,
{
    if !recorded_preparation {
        if let Some(root) = root
            && let Some(bundle) = lookup(root).await?
        {
            return Ok(bundle);
        }
        if recorded_code {
            return Err(CodePreparationFailure::Failed);
        }
    }
    reconcile_or_prepare().await
}

fn requires_dependency_preparation(
    invocation: &CodeInvocation<'_>,
    profile: &CodeRuntimeProfile,
) -> bool {
    match invocation.language {
        CodeLanguage::Rust => invocation.dependencies_toml.is_some(),
        CodeLanguage::Python | CodeLanguage::JavaScript | CodeLanguage::TypeScript => {
            profile.preparation.is_some() || profile.preparation_client.is_some()
        }
    }
}

/// Cargo acquisition contains only saved metadata. Ordinary source and state remain execution input.
fn native_acquisition<'a>(
    invocation: &CodeInvocation<'a>,
) -> Result<(crate::sandbox::request::Language, &'a str), GraphError> {
    use crate::sandbox::request::Language;
    match invocation.language {
        CodeLanguage::JavaScript => Ok((Language::JavaScript, invocation.source)),
        CodeLanguage::TypeScript => Ok((Language::TypeScript, invocation.source)),
        CodeLanguage::Rust => Ok((
            Language::Rust,
            invocation.dependencies_toml.ok_or_else(|| {
                failed("Cargo acquisition requires a saved dependency declaration.")
            })?,
        )),
        CodeLanguage::Python => Err(failed(
            "Native dependency acquisition requires its language adapter.",
        )),
    }
}

enum LookupAttempt {
    Retry,
    Final(CodePreparationFailure),
}

/// Retry retryable frozen-lookup errors at the reconcile cadence until `deadline`.
async fn retry_frozen_lookup<T, Op, Fut>(
    deadline: tokio::time::Instant,
    mut attempt: Op,
) -> Result<T, CodePreparationFailure>
where
    Op: FnMut() -> Fut,
    Fut: Future<Output = Result<T, LookupAttempt>>,
{
    loop {
        match attempt().await {
            Ok(value) => return Ok(value),
            Err(LookupAttempt::Final(failure)) => return Err(failure),
            Err(LookupAttempt::Retry) => {}
        }
        if tokio::time::Instant::now() >= deadline {
            return Err(CodePreparationFailure::Unconfirmed);
        }
        tokio::time::sleep_until(
            (tokio::time::Instant::now() + code_timing::CODE_RECONCILE_INTERVAL).min(deadline),
        )
        .await;
    }
}

/// Identity probes per Rust preparation; all are answered by one query.
#[cfg(test)]
pub(super) const PREPARATION_IDENTITY_PROBES_BUDGET: usize = 1;

/// `found` is [preparation, code, compiled] for Rust, or [preparation] otherwise.
fn recorded_identities(is_rust: bool, found: &[bool]) -> (bool, bool) {
    let recorded = found.first().copied().unwrap_or(false);
    let recorded_code = is_rust && !recorded && found.iter().skip(1).any(|found| *found);
    (recorded, recorded_code)
}

pub(super) fn retryable(error: &SandboxCallError) -> bool {
    matches!(
        error,
        SandboxCallError::Authorization(
            crate::transport::control_grpc::ControlGrpcError::Unavailable(_)
        ) | SandboxCallError::Submission {
            code: tonic::Code::Unavailable
                | tonic::Code::DeadlineExceeded
                | tonic::Code::Aborted
                | tonic::Code::ResourceExhausted
        }
    )
}

fn preparation_failed(_: &str) -> CodePreparationFailure {
    // Owner failure codes are bounded text, not an approved public allowlist.
    CodePreparationFailure::Failed
}

fn preparation_cancelled() -> CodePreparationFailure {
    CodePreparationFailure::Cancelled
}

fn refuse_preparation_downgrade(recorded: bool) -> Result<(), GraphError> {
    if recorded {
        return Err(failed(
            "This Code activation already requires Dependency preparation. Restore its preparation profile before retrying.",
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cargo_acquisition_uses_saved_declaration_without_source_or_state() {
        let invocation = CodeInvocation {
            activation: [4; 32],
            language: CodeLanguage::Rust,
            platform_client: false,
            source: "pub fn run() { panic!(\"never acquire this source\"); }",
            dependencies_toml: Some("[dependencies]\nnumber={package='itoa',version='=1.0.18'}\n"),
            provenance: crate::agents::graph::code::CodeProvenance::SavedLiteral,
            input_json: br#"{"private_state":"must not enter acquisition"}"#.to_vec(),
            trace: None,
            original: None,
            debug: None,
            workspace: None,
        };
        let (language, source) = native_acquisition(&invocation).unwrap();
        assert_eq!(language, crate::sandbox::request::Language::Rust);
        assert_eq!(source, invocation.dependencies_toml.unwrap());
        assert!(!source.contains("private_state"));
        assert!(!source.contains("panic!"));
        let missing = CodeInvocation {
            dependencies_toml: None,
            ..invocation
        };
        assert!(native_acquisition(&missing).is_err());
    }

    #[tokio::test(start_paused = true)]
    async fn slow_inert_transfer_keeps_the_post_readiness_execution_allowance() {
        let transfer_deadline = observation_deadline(120);
        tokio::time::advance(Duration::from_secs(200)).await;
        assert_eq!(
            transfer_deadline - tokio::time::Instant::now(),
            Duration::from_secs(10)
        );
        let execution_deadline = observation_deadline(30);
        assert_eq!(
            execution_deadline - tokio::time::Instant::now(),
            Duration::from_mins(2)
        );
        tokio::time::advance(Duration::from_mins(2)).await;
        assert!(
            tokio::time::timeout_at(execution_deadline, std::future::pending::<()>())
                .await
                .is_err()
        );
    }

    #[tokio::test(start_paused = true)]
    async fn transfer_and_publication_deadlines_remain_bounded_across_waits() {
        let resolution_deadline = observation_deadline(30);
        tokio::time::advance(Duration::from_secs(100)).await;
        let publication_deadline = observation_deadline(30);
        assert_eq!(
            resolution_deadline - tokio::time::Instant::now(),
            Duration::from_secs(20)
        );
        for _ in 0..3 {
            tokio::time::advance(Duration::from_secs(40)).await;
        }
        assert!(
            tokio::time::timeout_at(publication_deadline, std::future::pending::<()>())
                .await
                .is_err()
        );
    }

    #[test]
    fn recorded_preparation_cannot_downgrade_to_the_image_profile() {
        assert!(refuse_preparation_downgrade(false).is_ok());
        let error = refuse_preparation_downgrade(true).unwrap_err();
        assert!(
            error
                .to_string()
                .contains("Restore its preparation profile")
        );
    }

    #[test]
    fn preparation_retries_only_ambiguous_transport_failures() {
        for code in [
            tonic::Code::Unavailable,
            tonic::Code::DeadlineExceeded,
            tonic::Code::Aborted,
            tonic::Code::ResourceExhausted,
        ] {
            assert!(retryable(&SandboxCallError::Submission { code }));
        }
        for code in [
            tonic::Code::NotFound,
            tonic::Code::InvalidArgument,
            tonic::Code::PermissionDenied,
            tonic::Code::FailedPrecondition,
        ] {
            assert!(!retryable(&SandboxCallError::Submission { code }));
        }
        assert!(!retryable(&SandboxCallError::InvalidReceipt));
    }
}

#[cfg(test)]
mod safe_preparation_diagnostic_tests {
    use super::*;

    #[test]
    fn owner_failure_text_does_not_enter_a_public_preparation_diagnostic() {
        let raw = "SELECT private_schema; credential=private-token; /private/source";
        let reason = preparation_failed(raw);
        assert_eq!(reason, CodePreparationFailure::Failed);
        assert_eq!(reason.code(), "pipeline.code_preparation_failed");
        let rendered = reason.graph_error().to_string();
        assert!(rendered.contains("Code dependency preparation failed"));
        for secret in ["SELECT", "credential", "private-token", "/private/source"] {
            assert!(!rendered.contains(secret));
        }
    }

    #[test]
    fn cancellation_and_unconfirmed_status_have_distinct_safe_codes() {
        assert_eq!(
            preparation_cancelled().code(),
            "pipeline.code_preparation_cancelled"
        );
        let unconfirmed = CodePreparationFailure::Unconfirmed
            .graph_error()
            .to_string();
        assert!(unconfirmed.contains("could not be confirmed"));
        assert!(unconfirmed.contains("was not restarted"));
        assert!(unconfirmed.contains("reconcile the existing attempt"));
        assert!(!unconfirmed.contains("agent.legacy"));
    }
}

#[cfg(test)]
mod frozen_preparation_flow_tests {
    use super::*;
    use std::cell::Cell;

    fn bundle() -> DependencyBundle {
        crate::sandbox::preparation::frozen_lookup_tests::fixture().1
    }

    #[tokio::test]
    async fn frozen_flow_fresh_exact_hit_bypasses_prepare() {
        let selected = bundle();
        let root = selected.root().to_owned();
        let lookups = Cell::new(0);
        let preparations = Cell::new(0);
        let result = resolve_preparation_bundle(
            false,
            false,
            Some(&root),
            |_| async {
                lookups.set(lookups.get() + 1);
                Ok(Some(selected))
            },
            || async {
                preparations.set(preparations.get() + 1);
                Ok(bundle())
            },
        )
        .await
        .unwrap();
        assert_eq!(result.root(), root);
        assert_eq!((lookups.get(), preparations.get()), (1, 0));
    }

    #[tokio::test]
    async fn frozen_flow_genuine_fresh_miss_can_prepare() {
        let lookups = Cell::new(0);
        let preparations = Cell::new(0);
        let result = resolve_preparation_bundle(
            false,
            false,
            Some(&"c".repeat(64)),
            |_| async {
                lookups.set(lookups.get() + 1);
                Ok(None)
            },
            || async {
                preparations.set(preparations.get() + 1);
                Ok(bundle())
            },
        )
        .await
        .unwrap();
        assert_eq!(result.root(), bundle().root());
        assert_eq!((lookups.get(), preparations.get()), (1, 1));
    }

    #[tokio::test]
    async fn frozen_flow_lookup_errors_never_prepare() {
        for failure in [
            CodePreparationFailure::Failed,
            CodePreparationFailure::Unconfirmed,
        ] {
            let preparations = Cell::new(0);
            let result = resolve_preparation_bundle(
                false,
                false,
                Some(&"c".repeat(64)),
                |_| async { Err(failure) },
                || async {
                    preparations.set(preparations.get() + 1);
                    Ok(bundle())
                },
            )
            .await;
            assert_eq!(result.err(), Some(failure));
            assert_eq!(preparations.get(), 0);
        }
    }

    #[tokio::test]
    async fn frozen_flow_recorded_preparation_keeps_original_reconciliation() {
        let lookups = Cell::new(0);
        let reconciliations = Cell::new(0);
        let original = bundle();
        let original_root = original.root().to_owned();
        let result = resolve_preparation_bundle(
            true,
            true,
            Some(&"c".repeat(64)),
            |_| async {
                lookups.set(lookups.get() + 1);
                Ok(Some(bundle()))
            },
            || async {
                reconciliations.set(reconciliations.get() + 1);
                Ok(original)
            },
        )
        .await
        .unwrap();
        assert_eq!(result.root(), original_root);
        assert_eq!((lookups.get(), reconciliations.get()), (0, 1));
    }

    #[tokio::test]
    async fn frozen_flow_recorded_compile_or_execute_miss_blocks_new_acquisition() {
        for recorded_owner in ["compile", "execute"] {
            for root in [
                None,
                Some("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
            ] {
                let preparations = Cell::new(0);
                let result = resolve_preparation_bundle(
                    false,
                    true,
                    root,
                    |_| async { Ok(None) },
                    || async {
                        preparations.set(preparations.get() + 1);
                        Ok(bundle())
                    },
                )
                .await;
                assert_eq!(
                    result.err(),
                    Some(CodePreparationFailure::Failed),
                    "{recorded_owner}"
                );
                assert_eq!(preparations.get(), 0, "{recorded_owner}");
            }
        }
    }
}

#[cfg(test)]
mod frozen_lookup_retry_tests {
    use super::*;
    use std::cell::Cell;

    #[tokio::test(start_paused = true)]
    async fn frozen_flow_retries_busy_lookup_until_hit() {
        let lookups = Cell::new(0);
        let preparations = Cell::new(0);
        let deadline = tokio::time::Instant::now() + Duration::from_secs(30);
        let started = tokio::time::Instant::now();
        let result = resolve_preparation_bundle(
            false,
            false,
            Some(&"c".repeat(64)),
            |_| async {
                retry_frozen_lookup(deadline, || async {
                    lookups.set(lookups.get() + 1);
                    if lookups.get() < 3 {
                        Err(LookupAttempt::Retry)
                    } else {
                        Ok(Some(
                            crate::sandbox::preparation::frozen_lookup_tests::fixture().1,
                        ))
                    }
                })
                .await
            },
            || async {
                preparations.set(preparations.get() + 1);
                Err(CodePreparationFailure::Failed)
            },
        )
        .await;
        assert!(result.is_ok());
        assert_eq!((lookups.get(), preparations.get()), (3, 0));
        assert_eq!(started.elapsed(), code_timing::CODE_RECONCILE_INTERVAL * 2);
    }

    #[tokio::test(start_paused = true)]
    async fn frozen_lookup_busy_until_deadline_is_unconfirmed_and_bounded() {
        let lookups = Cell::new(0_usize);
        let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
        let result: Result<(), _> = retry_frozen_lookup(deadline, || async {
            lookups.set(lookups.get() + 1);
            Err(LookupAttempt::Retry)
        })
        .await;
        assert_eq!(result.err(), Some(CodePreparationFailure::Unconfirmed));
        assert!(tokio::time::Instant::now() >= deadline);
        // 1 s cadence: at most one attempt per second until the deadline.
        assert!(lookups.get() <= 11, "{}", lookups.get());
    }

    #[tokio::test(start_paused = true)]
    async fn frozen_lookup_terminal_error_is_not_retried() {
        let lookups = Cell::new(0);
        let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
        let result: Result<(), _> = retry_frozen_lookup(deadline, || async {
            lookups.set(lookups.get() + 1);
            Err(LookupAttempt::Final(CodePreparationFailure::Failed))
        })
        .await;
        assert_eq!(result.err(), Some(CodePreparationFailure::Failed));
        assert_eq!(lookups.get(), 1);
    }

    #[test]
    fn identity_probe_fold_matches_sequential_probes() {
        // The three sequential probes this replaced, as a reference model.
        for bits in 0..8_u8 {
            let [prep, code, compiled] = [bits & 1 != 0, bits & 2 != 0, bits & 4 != 0];
            for is_rust in [true, false] {
                let old_recorded = prep;
                let old_code = is_rust && !prep && (code || compiled);
                let found = if is_rust {
                    vec![prep, code, compiled]
                } else {
                    vec![prep]
                };
                assert_eq!(
                    recorded_identities(is_rust, &found),
                    (old_recorded, old_code)
                );
            }
        }
        assert_eq!(PREPARATION_IDENTITY_PROBES_BUDGET, 1);
    }
}
