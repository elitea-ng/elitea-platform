//! Reap only the retained runtime of an immutable no-effect tombstone.
#[allow(
    clippy::wildcard_imports,
    reason = "Share the owner module imports with runtime code and its existing tests."
)]
use super::*;
use crate::sandbox::ledger::CodeCleanupFailure;

impl DockerSupervisor {
    pub(crate) async fn reconcile_code_no_effect_cleanup(&self) -> Result<(), SupervisorError> {
        if self.preparation_policy.is_some() {
            return Ok(());
        }
        for scope in self
            .ledger
            .pending_code_no_effect_cleanup(&self.owner, 32)
            .await?
        {
            match self.cleanup_code_no_effect_runtime(&scope).await {
                Ok(()) => {}
                Err(SupervisorError::Busy) => break,
                Err(error) => {
                    tracing::error!(error = %error, "sandbox no-effect runtime cleanup remains pending");
                }
            }
        }
        Ok(())
    }
    pub(super) async fn cleanup_code_no_effect_runtime(
        &self,
        scope: &JobScope,
    ) -> Result<(), SupervisorError> {
        let _permit = self
            .stop_capacity
            .try_acquire()
            .map_err(|_| SupervisorError::Busy)?;
        let Some(lease) = self
            .ledger
            .claim_code_no_effect_cleanup(scope, &self.owner, LEASE_SECONDS)
            .await?
        else {
            return Ok(());
        };
        self.ledger.check_code_no_effect_cleanup(&lease).await?;
        if let Err(error) = self.runtime.terminate(&lease.identity()?).await {
            self.ledger
                .record_code_no_effect_cleanup(
                    &lease,
                    Some(CodeCleanupFailure::TerminationUnconfirmed),
                )
                .await?;
            return Err(SupervisorError::Runtime(error));
        }
        self.ledger.check_code_no_effect_cleanup(&lease).await?;
        // The shared terminal hook also owns durable workspace volume provenance.
        // It never selects a runtime by name or force-removes a running instance.
        let terminal = self
            .terminal(lease.scope(), self.ledger.read(lease.scope()).await?)
            .await?;
        let Reconciliation::Terminal {
            cleanup_pending, ..
        } = terminal
        else {
            return Err(SupervisorError::Invalid);
        };
        self.ledger
            .record_code_no_effect_cleanup(
                &lease,
                cleanup_pending.then_some(CodeCleanupFailure::CleanupUnconfirmed),
            )
            .await?;
        if cleanup_pending {
            Err(SupervisorError::Receipt)
        } else {
            Ok(())
        }
    }
}

#[cfg(test)]
#[path = "docker_code_recovery_cleanup_tests.rs"]
mod tests;
