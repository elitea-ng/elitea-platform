//! Verify root-bound packages while the original execution runtime remains inert.
use super::{DependencyDelivery, DockerSupervisor, SupervisorError};
use crate::sandbox::{
    dependency_bundle::{DependencyBundle, hex},
    dependency_content::DependencyContentError,
    ledger::JobScope,
    request::PreparedJob,
};
use adk_sandbox::workspace::docker::CodeJobIdentity;
use ring::digest;
use std::{
    io,
    pin::Pin,
    task::{Context, Poll},
};
use tokio::io::AsyncWrite;

impl DockerSupervisor {
    pub(super) async fn hydrate_dependencies(
        &self,
        scope: &JobScope,
        identity: &CodeJobIdentity,
        request: &PreparedJob,
        delivery: Option<DependencyDelivery<'_>>,
    ) -> Result<(), SupervisorError> {
        let Some(root) = request.dependency_bundle_root() else {
            return if delivery.is_none() {
                Ok(())
            } else {
                Err(SupervisorError::Invalid)
            };
        };
        let delivery = delivery.ok_or(SupervisorError::Invalid)?;
        if delivery.authorization.scope() != scope
            || !delivery
                .authorization
                .valid_at(chrono::Utc::now().timestamp_millis())
            || hex(delivery.authorization.root()) != root
            || delivery.bundle.root() != root
            || !request.matches_bundle(delivery.bundle)
        {
            return Err(SupervisorError::Invalid);
        }
        // Python metadata and native ready proofs are committed after complete hydration.
        // Native metadata alone does not prove language-owned hydration finished.
        // No network transfer occurs after this proof and before durable dispatch.
        let expected = delivery.bundle.record_json();
        let mut proof = ContentProof::new(expected.len() as u64);
        self.runtime
            .export_bundle_dependency(
                identity,
                delivery.bundle,
                delivery.bundle.proof_name(),
                expected.len() as u64,
                &mut proof,
            )
            .await
            .map_err(SupervisorError::Runtime)?;
        proof.verify(&hex(digest::digest(&digest::SHA256, expected).as_ref()))?;
        Ok(())
    }

    pub(super) async fn hydrate_index_owned(
        &self,
        identity: &CodeJobIdentity,
        bundle: &DependencyBundle,
        index: usize,
        delivery: DependencyDelivery<'_>,
    ) -> Result<(), SupervisorError> {
        if bundle.root() != delivery.bundle.root()
            || bundle.record_json() != delivery.bundle.record_json()
        {
            return Err(SupervisorError::Invalid);
        }
        if let Some(file) = bundle.files().get(index) {
            let (staged, mut bytes) = delivery
                .client
                .download_index(bundle, index, delivery.grant)
                .await?;
            let imported = self
                .runtime
                .import_bundle_dependency(identity, bundle, file.name(), file.bytes(), &mut bytes)
                .await
                .map_err(SupervisorError::Runtime);
            drop(bytes);
            let cleanup = staged.close().map_err(DependencyContentError::Staging);
            imported?;
            cleanup?;
            return Ok(());
        }
        if index != bundle.file_count() {
            return Err(SupervisorError::Invalid);
        }
        // Lost acknowledgements can replay indexes. Verify the complete original runtime
        // before committing metadata, without holding package content in memory.
        for file in bundle.files() {
            let mut proof = ContentProof::new(file.bytes());
            self.runtime
                .export_bundle_dependency(identity, bundle, file.name(), file.bytes(), &mut proof)
                .await
                .map_err(SupervisorError::Runtime)?;
            proof.verify(file.sha256())?;
        }
        let metadata = bundle.record_json();
        let mut bytes = std::io::Cursor::new(metadata);
        self.runtime
            .import_bundle_dependency(
                identity,
                bundle,
                bundle.metadata_name(),
                metadata.len() as u64,
                &mut bytes,
            )
            .await
            .map_err(SupervisorError::Runtime)?;
        self.runtime
            .finalize_bundle(identity, bundle)
            .await
            .map_err(SupervisorError::Runtime)
    }
}

/// Bound exports independently from the runtime and hash their bytes without retaining them.
pub(super) struct ContentProof {
    hash: digest::Context,
    expected: u64,
    written: u64,
}
impl ContentProof {
    pub(super) fn new(expected: u64) -> Self {
        Self {
            hash: digest::Context::new(&digest::SHA256),
            expected,
            written: 0,
        }
    }
    pub(super) fn verify(self, expected_hash: &str) -> Result<(), DependencyContentError> {
        if self.written != self.expected || hex(self.hash.finish().as_ref()) != expected_hash {
            return Err(DependencyContentError::Integrity);
        }
        Ok(())
    }
}
impl AsyncWrite for ContentProof {
    fn poll_write(
        mut self: Pin<&mut Self>,
        _cx: &mut Context<'_>,
        bytes: &[u8],
    ) -> Poll<io::Result<usize>> {
        if bytes.len() as u64 > self.expected.saturating_sub(self.written) {
            return Poll::Ready(Err(io::Error::other(
                "Dependency export exceeds its recorded length",
            )));
        }
        self.hash.update(bytes);
        self.written += bytes.len() as u64;
        Poll::Ready(Ok(bytes.len()))
    }
    fn poll_flush(self: Pin<&mut Self>, _cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Poll::Ready(Ok(()))
    }
    fn poll_shutdown(self: Pin<&mut Self>, _cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Poll::Ready(Ok(()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::AsyncWriteExt as _;

    #[tokio::test]
    async fn content_proof_rejects_changed_short_and_excess_exports() {
        let expected = hex(digest::digest(&digest::SHA256, b"abc").as_ref());
        let mut proof = ContentProof::new(3);
        proof.write_all(b"abc").await.unwrap();
        proof.verify(&expected).unwrap();
        let mut changed = ContentProof::new(3);
        changed.write_all(b"abd").await.unwrap();
        assert!(changed.verify(&expected).is_err());
        assert!(ContentProof::new(3).verify(&expected).is_err());
        assert!(ContentProof::new(3).write_all(b"abcd").await.is_err());
    }
}
