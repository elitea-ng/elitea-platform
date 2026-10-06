#[allow(
    clippy::wildcard_imports,
    reason = "Share the owner module imports with runtime code and its existing tests."
)]
use super::*;
use crate::{
    protocol::sandbox_grant::{
        AuthorizedCodeIntent, AuthorizedCodePlatformOwner, AuthorizedSnapshotExecute,
        CodePlatformOwnerOperation,
    },
    sandbox::{
        code_platform_owner::{
            CodePlatformLedgerObservation, CodePlatformOwnerObservation, CodePlatformOwnerResponse,
            RetainedCodePlatformRuntime,
        },
        code_recovery::{sha256, whole_result},
    },
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
impl DockerSupervisor {
    pub(crate) async fn register_compiled_code_intent(
        &self,
        authority: &AuthorizedSnapshotExecute,
        intent: &AuthorizedCodeIntent,
        job: &PreparedJob,
        control: &crate::sandbox::compiled_snapshot::Control,
        descriptor: &[u8],
    ) -> Result<(), SupervisorError> {
        let scope = authority.scope();
        self.validate_snapshot(scope, job, control)?;
        if !authority.permits(job, control, chrono::Utc::now().timestamp_millis()) {
            return Err(SupervisorError::Invalid);
        }
        let whole = intent
            .binding(scope, chrono::Utc::now().timestamp_millis())
            .map_err(|_| SupervisorError::Invalid)?;
        // Validate the exact original descriptor before persisting any broker identity.
        crate::sandbox::code_platform_owner::CodePlatformBinding::from_compiled_job(
            job, whole, control, descriptor,
        )
        .map_err(|_| SupervisorError::Invalid)?;
        self.ledger.reserve(scope).await?;
        self.ledger.register_code_intent(scope, intent).await?;
        self.ledger
            .register_compiled_code_platform_binding(scope, intent, job, control, descriptor)
            .await?;
        Ok(())
    }
    pub(crate) async fn observe_code_platform_owner(
        &self,
        authority: &AuthorizedCodePlatformOwner,
        reply: Option<&[u8]>,
    ) -> Result<CodePlatformOwnerObservation, SupervisorError> {
        if !authority.accepts_reply(reply) {
            return Err(SupervisorError::Invalid);
        }
        let result = self
            .observe_current_code_platform_owner(authority, reply)
            .await;
        let Err(error) = result else { return result };
        let closed = close_completed_read(
            authority.operation(),
            error,
            self.ledger.observe_code_platform_completion(authority),
        )
        .await;
        let Err(error) = closed else { return closed };
        if authority.operation() == CodePlatformOwnerOperation::PublishCommittedPlatformReply {
            return Err(error);
        }
        match self.observe_completing_code_platform_read(authority).await {
            Ok(observation) => Ok(observation),
            Err(_) => {
                // Completion can commit during stopped-runtime receipt observation.
                close_completed_read(
                    authority.operation(),
                    error,
                    self.ledger.observe_code_platform_completion(authority),
                )
                .await
            }
        }
    }
    async fn observe_completing_code_platform_read(
        &self,
        authority: &AuthorizedCodePlatformOwner,
    ) -> Result<CodePlatformOwnerObservation, SupervisorError> {
        let retained = self.ledger.read_code_platform_runtime(authority).await?;
        if self.runtime.retained_code_platform_kind().is_none()
            || !authority.permits_completed(
                &retained.binding,
                &retained.broker,
                chrono::Utc::now().timestamp_millis(),
            )
        {
            return Err(SupervisorError::Invalid);
        }
        let identity = authority
            .scope()
            .runtime_identity()?
            .with_runtime_id(retained.runtime_id.clone())
            .map_err(SupervisorError::Runtime)?;
        self.verify_platform_instance(&identity, &retained.runtime_id)
            .await?;
        closing_receipt_observation(
            async {
                self.runtime
                    .receipt(&identity)
                    .await
                    .map_err(SupervisorError::Runtime)
            },
            async {
                self.ledger
                    .observe_code_platform_completion(authority)
                    .await
                    .map_err(SupervisorError::Ledger)
            },
            async {
                self.verify_platform_instance(&identity, &retained.runtime_id)
                    .await?;
                self.ledger
                    .verify_code_platform_runtime(authority, &retained)
                    .await?;
                Ok(())
            },
        )
        .await
    }
    async fn observe_current_code_platform_owner(
        &self,
        authority: &AuthorizedCodePlatformOwner,
        reply: Option<&[u8]>,
    ) -> Result<CodePlatformOwnerObservation, SupervisorError> {
        let retained = match self.ledger.observe_code_platform_runtime(authority).await? {
            CodePlatformLedgerObservation::NotReady => {
                return Ok(CodePlatformOwnerObservation::NotReady);
            }
            CodePlatformLedgerObservation::Completed => {
                return Ok(CodePlatformOwnerObservation::Completed);
            }
            CodePlatformLedgerObservation::Running(retained) => retained,
        };
        let kind = self
            .runtime
            .retained_code_platform_kind()
            .ok_or(SupervisorError::Invalid)?;
        let identity = authority
            .scope()
            .runtime_identity()?
            .with_runtime_id(retained.runtime_id.clone())
            .map_err(SupervisorError::Runtime)?;
        self.verify_platform_instance(&identity, &retained.runtime_id)
            .await?;
        let launch = retained
            .broker
            .launch(&retained.runtime_id)
            .map_err(|_| SupervisorError::Invalid)?;
        if launch.len() > 4096 {
            return Err(SupervisorError::Invalid);
        }
        // This validates the immutable fixed launch on the original runtime;
        // it cannot bind a new launch or invoke a platform operation.
        self.runtime
            .validate_code_platform_launch(&identity, &launch)
            .await
            .map_err(SupervisorError::Runtime)?;
        let (pending, published) = match authority.operation() {
            CodePlatformOwnerOperation::ReadRetainedRuntime => (None, None),
            CodePlatformOwnerOperation::ReadPendingPlatformCall => {
                let bytes = self
                    .runtime
                    .read_code_platform_call(&identity, &launch)
                    .await
                    .map_err(SupervisorError::Runtime)?;
                if bytes
                    .as_ref()
                    .is_some_and(|b| b.is_empty() || b.len() > 327_688)
                {
                    return Err(SupervisorError::Receipt);
                }
                (bytes.map(|bytes| URL_SAFE_NO_PAD.encode(bytes)), None)
            }
            CodePlatformOwnerOperation::PublishCommittedPlatformReply => {
                self.runtime
                    .publish_code_platform_reply(
                        &identity,
                        &launch,
                        reply.ok_or(SupervisorError::Invalid)?,
                    )
                    .await
                    .map_err(SupervisorError::Runtime)?;
                (None, Some(true))
            }
        };
        self.verify_platform_instance(&identity, &retained.runtime_id)
            .await?;
        self.ledger
            .verify_code_platform_runtime(authority, &retained)
            .await?;
        Ok(CodePlatformOwnerObservation::Running(Box::new(
            CodePlatformOwnerResponse {
                schema: "elitea.sandbox.code-platform-owner-response.v1",
                state: "running",
                runtime: RetainedCodePlatformRuntime {
                    kind,
                    runtime_id: retained.runtime_id,
                    owner_epoch: retained.epoch,
                    binding_sha256: sha256(
                        &retained
                            .binding
                            .canonical_bytes()
                            .map_err(|_| SupervisorError::Invalid)?,
                    ),
                    prepared_job_sha256: retained.broker.prepared_job_sha256,
                    prepared_fingerprint: retained.broker.prepared_fingerprint,
                    policy_sha256: retained.broker.policy_sha256,
                    max_calls: retained.broker.max_calls,
                    max_total_bytes: retained.broker.max_total_bytes,
                    lifecycle: "dispatched",
                    compiled_execute: retained.broker.compiled_execute,
                },
                pending_call_base64url: pending,
                reply_published: published,
            },
        )))
    }
    async fn verify_platform_instance(
        &self,
        identity: &adk_sandbox::workspace::docker::CodeJobIdentity,
        expected: &str,
    ) -> Result<(), SupervisorError> {
        if !self
            .runtime
            .exists(identity)
            .await
            .map_err(SupervisorError::Runtime)?
            || self
                .runtime
                .instance(identity)
                .await
                .map_err(SupervisorError::Runtime)?
                .as_deref()
                != Some(expected)
        {
            return Err(SupervisorError::Receipt);
        }
        Ok(())
    }
}
/// Recheck only read failures. The exact ledger check owns completion evidence.
async fn close_completed_read(
    operation: CodePlatformOwnerOperation,
    error: SupervisorError,
    completion: impl std::future::Future<Output = Result<bool, super::super::ledger::LedgerError>>,
) -> Result<CodePlatformOwnerObservation, SupervisorError> {
    if matches!(
        operation,
        CodePlatformOwnerOperation::ReadRetainedRuntime
            | CodePlatformOwnerOperation::ReadPendingPlatformCall
    ) && matches!(completion.await, Ok(true))
    {
        // Cleanup can finish after the first ledger read. Generic refusals stay errors.
        return Ok(CodePlatformOwnerObservation::Completed);
    }
    Err(error)
}
/// A stopped receipt can close polling while its original owner commits it.
/// Both fences remain read-only. The receipt is neither returned nor persisted.
async fn closing_receipt_observation(
    receipt: impl std::future::Future<Output = Result<Option<Vec<u8>>, SupervisorError>>,
    completed: impl std::future::Future<Output = Result<bool, SupervisorError>>,
    recheck: impl std::future::Future<Output = Result<(), SupervisorError>>,
) -> Result<CodePlatformOwnerObservation, SupervisorError> {
    let receipt = receipt.await?.ok_or(SupervisorError::Receipt)?;
    if !whole_result(&receipt) {
        return Err(SupervisorError::Receipt);
    }
    if completed.await? {
        return Ok(CodePlatformOwnerObservation::Completed);
    }
    recheck.await?;
    Ok(CodePlatformOwnerObservation::Completing)
}
#[cfg(test)]
mod completion_tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};
    fn stopped_receipt() -> Vec<u8> {
        serde_json::to_vec(&serde_json::json!({
            "revision":1,"status":"completed","exit_code":0,
            "stdout":r#"{"revision":1,"result":{"status":"PASS"}}"#,"stderr":""
        }))
        .unwrap()
    }
    #[tokio::test]
    async fn stopped_valid_receipt_before_commit_returns_completing_after_both_fences() {
        let reads = AtomicUsize::new(0);
        let commits = AtomicUsize::new(0);
        let rechecks = AtomicUsize::new(0);
        let observed = closing_receipt_observation(
            async {
                reads.fetch_add(1, Ordering::SeqCst);
                Ok(Some(stopped_receipt()))
            },
            async {
                commits.fetch_add(1, Ordering::SeqCst);
                Ok(false)
            },
            async {
                rechecks.fetch_add(1, Ordering::SeqCst);
                Ok(())
            },
        )
        .await;
        assert!(matches!(
            observed,
            Ok(CodePlatformOwnerObservation::Completing)
        ));
        assert_eq!(
            (
                reads.load(Ordering::SeqCst),
                commits.load(Ordering::SeqCst),
                rechecks.load(Ordering::SeqCst)
            ),
            (1, 1, 1)
        );
    }
    #[tokio::test]
    async fn ledger_completion_during_receipt_read_wins_over_runtime_cleanup() {
        let (commit, committed) = tokio::sync::oneshot::channel();
        let rechecks = AtomicUsize::new(0);
        let observed = closing_receipt_observation(
            async {
                commit.send(()).unwrap();
                Ok(Some(stopped_receipt()))
            },
            async {
                committed.await.unwrap();
                Ok(true)
            },
            async {
                rechecks.fetch_add(1, Ordering::SeqCst);
                Err(SupervisorError::Receipt)
            },
        )
        .await;
        assert!(matches!(
            observed,
            Ok(CodePlatformOwnerObservation::Completed)
        ));
        assert_eq!(rechecks.load(Ordering::SeqCst), 0);
    }
    #[tokio::test]
    async fn missing_failed_invalid_and_oversized_runtime_receipts_never_close_reads() {
        let mut failed: serde_json::Value = serde_json::from_slice(&stopped_receipt()).unwrap();
        failed["status"] = "failed".into();
        for receipt in [
            None,
            Some(serde_json::to_vec(&failed).unwrap()),
            Some(b"{}".to_vec()),
            Some(vec![b'x'; 512 * 1024 + 1]),
        ] {
            let rechecks = AtomicUsize::new(0);
            let observed = closing_receipt_observation(
                async { Ok(receipt) },
                async {
                    rechecks.fetch_add(1, Ordering::SeqCst);
                    Ok(false)
                },
                async {
                    rechecks.fetch_add(1, Ordering::SeqCst);
                    Ok(())
                },
            )
            .await;
            assert!(matches!(observed, Err(SupervisorError::Receipt)));
            assert_eq!(rechecks.load(Ordering::SeqCst), 0);
        }
    }
    #[tokio::test]
    async fn receipt_transport_and_postread_owner_refusals_never_become_completing() {
        let observed = closing_receipt_observation(
            async { Err(SupervisorError::Receipt) },
            async { Ok(false) },
            async { Ok(()) },
        )
        .await;
        assert!(matches!(observed, Err(SupervisorError::Receipt)));
        for failed_stage in 0..2 {
            let observed = closing_receipt_observation(
                async { Ok(Some(stopped_receipt())) },
                async {
                    if failed_stage == 0 {
                        Err(SupervisorError::Ledger(LedgerError::Fenced))
                    } else {
                        Ok(false)
                    }
                },
                async { Err(SupervisorError::Ledger(LedgerError::Fenced)) },
            )
            .await;
            assert!(matches!(
                observed,
                Err(SupervisorError::Ledger(LedgerError::Fenced))
            ));
        }
    }
    #[tokio::test]
    async fn fresh_completion_after_runtime_read_failure_closes_both_read_operations() {
        for operation in [
            CodePlatformOwnerOperation::ReadRetainedRuntime,
            CodePlatformOwnerOperation::ReadPendingPlatformCall,
        ] {
            let reads = AtomicUsize::new(0);
            let (commit, committed) = tokio::sync::oneshot::channel();
            let recheck = async {
                reads.fetch_add(1, Ordering::SeqCst);
                committed.await.unwrap();
                Ok(true)
            };
            let (observed, ()) = tokio::join!(
                close_completed_read(operation, SupervisorError::Receipt, recheck),
                async {
                    commit.send(()).unwrap();
                },
            );
            assert!(matches!(
                observed,
                Ok(CodePlatformOwnerObservation::Completed)
            ));
            assert_eq!(reads.load(Ordering::SeqCst), 1);
        }
    }
    #[tokio::test]
    async fn noncompleted_and_failed_rechecks_preserve_the_original_runtime_refusal() {
        for recheck in [
            Ok(false),
            Err(super::super::super::ledger::LedgerError::Fenced),
            Err(super::super::super::ledger::LedgerError::Conflict),
        ] {
            let result = close_completed_read(
                CodePlatformOwnerOperation::ReadPendingPlatformCall,
                SupervisorError::Receipt,
                async { recheck },
            )
            .await;
            assert!(matches!(result, Err(SupervisorError::Receipt)));
        }
    }
    #[tokio::test]
    async fn publish_refusal_never_polls_completion_or_changes_reply_delivery() {
        let reads = AtomicUsize::new(0);
        let result = close_completed_read(
            CodePlatformOwnerOperation::PublishCommittedPlatformReply,
            SupervisorError::Receipt,
            async {
                reads.fetch_add(1, Ordering::SeqCst);
                Ok(true)
            },
        )
        .await;
        assert!(matches!(result, Err(SupervisorError::Receipt)));
        assert_eq!(reads.load(Ordering::SeqCst), 0);
    }
}
