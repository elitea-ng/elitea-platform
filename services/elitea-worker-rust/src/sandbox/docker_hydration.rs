//! Import one verified dependency index while execution remains inert.
use super::{DependencyDelivery, DockerSupervisor, InertJob, LEASE_SECONDS, SupervisorError};
use crate::{
    protocol::sandbox_grant::AuthorizedJob,
    sandbox::{
        dependency_content::DependencyBundle,
        ledger::{JobLease, JobScope, Phase},
        request::PreparedJob,
    },
};
use std::time::Duration;

impl DockerSupervisor {
    /// Acknowledge one imported index with fresh content and execution authority.
    /// Ready permits submission to reconcile or dispatch the same immutable request.
    /// # Errors
    /// Returns invalid authority, capacity, lease fencing, persistence, content, or runtime errors.
    pub async fn hydrate_authorized(
        &self,
        authorization: &AuthorizedJob,
        request: &PreparedJob,
        delivery: DependencyDelivery<'_>,
        bundle: &DependencyBundle,
        index: u32,
    ) -> Result<bool, SupervisorError> {
        self.validate_execution(authorization, request, Some(delivery))?;
        let index = usize::try_from(index).map_err(|_| SupervisorError::Invalid)?;
        if !request.matches_bundle(bundle) || index > bundle.file_count() {
            return Err(SupervisorError::Invalid);
        }
        let _permit = self
            .capacity
            .try_acquire()
            .map_err(|_| SupervisorError::Busy)?;
        let scope = authorization.scope();
        if self.ledger.reserve(scope).await?.phase != Phase::Reserved {
            return Ok(true);
        }
        let lease = self.claim_indexed_transfer(scope).await?;
        // A concurrent owner can dispatch before this claim's authoritative observation.
        let outcome = if lease.observed_phase == Phase::Reserved {
            self.hydrate_owned(scope, &lease, request, delivery, bundle, index)
                .await
        } else {
            Ok(true)
        };
        // Unknown provisioning can still create a runtime after an early absence observation.
        // Preserve its lease until expiry. Confirmed runtime binding permits immediate handoff.
        if outcome.is_ok() || self.ledger.read(scope).await?.runtime_id.is_some() {
            self.release_failed_preparation(&lease).await?;
        }
        outcome
    }

    async fn hydrate_owned(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        request: &PreparedJob,
        delivery: DependencyDelivery<'_>,
        bundle: &DependencyBundle,
        index: usize,
    ) -> Result<bool, SupervisorError> {
        let hydration = async {
            if self.stop_if_requested(scope, lease).await?.is_some() {
                return Ok(true);
            }
            if self
                .expire_hydration_if_needed(scope, lease)
                .await?
                .is_some()
            {
                return Ok(true);
            }
            let manifest = request.manifest().map_err(|_| SupervisorError::Invalid)?;
            let identity = match self.provision_inert(scope, lease, &manifest).await? {
                InertJob::Ready(identity) => identity,
                InertJob::Stopped(_) => return Ok(true),
            };
            self.ledger.renew(lease, LEASE_SECONDS).await?;
            if self.stop_if_requested(scope, lease).await?.is_some() {
                return Ok(true);
            }
            if self
                .expire_hydration_if_needed(scope, lease)
                .await?
                .is_some()
            {
                return Ok(true);
            }
            self.hydrate_index_owned(&identity, bundle, index, delivery)
                .await?;
            if self.stop_if_requested(scope, lease).await?.is_some() {
                return Ok(true);
            }
            if self
                .expire_hydration_if_needed(scope, lease)
                .await?
                .is_some()
            {
                return Ok(true);
            }
            Ok(index == bundle.file_count())
        };
        tokio::pin!(hydration);
        let mut heartbeat = tokio::time::interval(Duration::from_secs(20));
        heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        heartbeat.tick().await;
        loop {
            tokio::select! {
                result = &mut hydration => return result,
                _ = heartbeat.tick() => {
                    if self.stop_if_requested(scope, lease).await?.is_some() {
                        return Ok(true);
                    }
                    if self.expire_hydration_if_needed(scope, lease).await?.is_some() {
                        return Ok(true);
                    }
                    self.ledger.renew(lease, LEASE_SECONDS).await?;
                }
            }
        }
    }
}

#[cfg(test)]
#[path = "docker_hydration_deadline_tests.rs"]
mod deadline_tests;
