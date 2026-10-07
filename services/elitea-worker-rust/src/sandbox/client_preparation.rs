//! Bounded worker control for trusted preparation. Package files never enter RPC bodies.
use super::{SandboxCallError, SandboxClient, submission_code};
use crate::{
    protocol::elitea::runtime::v1::{
        AuthorizeSandboxJobRequestV1, LookupSandboxDependenciesRequestV1,
        LookupSandboxDependenciesResponseV1, PrepareSandboxDependenciesRequestV1,
        PrepareSandboxDependenciesResponseV1, PublishSandboxDependenciesRequestV1,
        PublishSandboxDependenciesResponseV1, SandboxJobStatusV1, SignedSandboxJobGrantV1,
    },
    sandbox::{dependency_bundle::DependencyBundle, preparation::PreparationJob},
    transport::control_grpc::{ControlGrpcClient, ControlRpc},
};
use tonic::{Code, Request};
#[cfg(all(test, feature = "sandbox-supervisor"))]
#[path = "client_preparation_tests.rs"]
mod transport_tests;

pub enum PreparationOutcome {
    Pending(Option<DependencyBundle>),
    Completed(DependencyBundle),
    Failed { code: String },
    Cancelled,
    Uncertain { code: String },
}

pub enum PublicationOutcome {
    Pending,
    Completed,
    Failed { code: String },
    Cancelled,
    Uncertain { code: String },
}

impl SandboxClient {
    /// Read one frozen Cargo root without starting or reconciling a preparer.
    /// # Errors
    /// Returns authority, transport, or metadata errors. Errors are never cache misses.
    pub(crate) async fn lookup_dependencies<R: ControlRpc>(
        &self,
        control: &ControlGrpcClient<R>,
        authorization: AuthorizeSandboxJobRequestV1,
        job: &PreparationJob,
        root: &str,
    ) -> Result<Option<DependencyBundle>, SandboxCallError> {
        if job.language() != crate::sandbox::request::Language::Rust || !job.native() {
            return Err(SandboxCallError::Invalid);
        }
        let digest = job.fingerprint().map_err(|_| SandboxCallError::Invalid)?;
        let grant = self
            .authorize_content(control, authorization, &digest, root)
            .await?;
        let mut request = Request::new(LookupSandboxDependenciesRequestV1 {
            content_grant: Some(grant),
            preparation_job_json: job.to_transport().map_err(|_| SandboxCallError::Invalid)?,
        });
        request.set_timeout(self.deadline);
        let response = tokio::time::timeout(
            self.deadline,
            self.rpc.clone().lookup_sandbox_dependencies(request),
        )
        .await
        .map_err(|_| SandboxCallError::Submission {
            code: Code::DeadlineExceeded,
        })?
        .map_err(|status| SandboxCallError::Submission {
            code: submission_code(&status),
        })?
        .into_inner();
        decode_lookup(response, job, root)
    }

    /// Request fresh authority and reconcile the same immutable preparation.
    /// # Errors
    /// Returns safe authorization, transport, or bounded metadata failures.
    pub async fn prepare<R: ControlRpc>(
        &self,
        control: &ControlGrpcClient<R>,
        mut authorization: AuthorizeSandboxJobRequestV1,
        job: &PreparationJob,
    ) -> Result<PreparationOutcome, SandboxCallError> {
        authorization.request_digest = job
            .fingerprint()
            .map_err(|_| SandboxCallError::Invalid)?
            .to_vec();
        authorization.audience.clone_from(&self.audience);
        authorization.cancel_only = false;
        authorization.dependency_bundle_sha256.clear();
        let grant = authorize(control, authorization).await?;
        let mut request = Request::new(PrepareSandboxDependenciesRequestV1 {
            grant: Some(grant),
            preparation_job_json: job.to_transport().map_err(|_| SandboxCallError::Invalid)?,
        });
        request.set_timeout(self.deadline);
        let response = tokio::time::timeout(
            self.deadline,
            self.rpc.clone().prepare_sandbox_dependencies(request),
        )
        .await
        .map_err(|_| SandboxCallError::Submission {
            code: Code::DeadlineExceeded,
        })?
        .map_err(|status| SandboxCallError::Submission {
            code: submission_code(&status),
        })?
        .into_inner();
        let outcome = decode_preparation(response)?;
        match &outcome {
            PreparationOutcome::Pending(Some(bundle)) | PreparationOutcome::Completed(bundle)
                if !job.matches_bundle(bundle) =>
            {
                Err(SandboxCallError::InvalidReceipt)
            }
            _ => Ok(outcome),
        }
    }

    /// Publish one recorded file, or publish metadata at the final index.
    /// # Errors
    /// Returns safe authority, transport, index, or receipt failures.
    pub async fn publish<R: ControlRpc>(
        &self,
        control: &ControlGrpcClient<R>,
        authorization: AuthorizeSandboxJobRequestV1,
        job: &PreparationJob,
        bundle: &DependencyBundle,
        index: usize,
    ) -> Result<PublicationOutcome, SandboxCallError> {
        if index > bundle.file_count() || !job.matches_bundle(bundle) {
            return Err(SandboxCallError::Invalid);
        }
        let digest = job.fingerprint().map_err(|_| SandboxCallError::Invalid)?;
        let grant = self
            .authorize_content(control, authorization, &digest, bundle.root())
            .await?;
        let mut request = Request::new(PublishSandboxDependenciesRequestV1 {
            content_grant: Some(grant),
            index: u32::try_from(index).map_err(|_| SandboxCallError::Invalid)?,
        });
        request.set_timeout(self.deadline);
        let response = tokio::time::timeout(
            self.deadline,
            self.rpc.clone().publish_sandbox_dependencies(request),
        )
        .await
        .map_err(|_| SandboxCallError::Submission {
            code: Code::DeadlineExceeded,
        })?
        .map_err(|status| SandboxCallError::Submission {
            code: submission_code(&status),
        })?
        .into_inner();
        decode_publication(response)
    }

    pub(super) async fn authorize_content<R: ControlRpc>(
        &self,
        control: &ControlGrpcClient<R>,
        mut authorization: AuthorizeSandboxJobRequestV1,
        digest: &[u8; 32],
        root: &str,
    ) -> Result<SignedSandboxJobGrantV1, SandboxCallError> {
        authorization.request_digest = digest.to_vec();
        authorization.audience.clone_from(&self.audience);
        authorization.cancel_only = false;
        authorization.dependency_bundle_sha256 = root_bytes(root)?.to_vec();
        authorize(control, authorization).await
    }
}

pub(super) async fn authorize<R: ControlRpc>(
    control: &ControlGrpcClient<R>,
    request: AuthorizeSandboxJobRequestV1,
) -> Result<SignedSandboxJobGrantV1, SandboxCallError> {
    let response = control.authorize_sandbox_job(request).await?;
    if response.rejection.is_some() {
        return Err(SandboxCallError::Rejected);
    }
    let grant = response.grant.ok_or(SandboxCallError::Rejected)?;
    if grant.signature.len() != 64
        || grant.claims_bytes.is_empty()
        || grant.claims_bytes.len() > 4096
        || grant.key_id.is_empty()
        || grant.key_id.len() > 256
    {
        return Err(SandboxCallError::Rejected);
    }
    Ok(grant)
}

fn root_bytes(root: &str) -> Result<[u8; 32], SandboxCallError> {
    if root.len() != 64 {
        return Err(SandboxCallError::Invalid);
    }
    let mut bytes = [0; 32];
    for (slot, pair) in bytes.iter_mut().zip(root.as_bytes().chunks_exact(2)) {
        let nibble = |byte| match byte {
            b'0'..=b'9' => Ok(byte - b'0'),
            b'a'..=b'f' => Ok(byte - b'a' + 10),
            _ => Err(SandboxCallError::Invalid),
        };
        *slot = nibble(pair[0])? * 16 + nibble(pair[1])?;
    }
    Ok(bytes)
}

fn valid_failure(code: &str) -> bool {
    code.len() <= 128 && !code.chars().any(char::is_control)
}

fn decode_preparation(
    response: PrepareSandboxDependenciesResponseV1,
) -> Result<PreparationOutcome, SandboxCallError> {
    if response.bundle_json.len() > 128 * 1024 || !valid_failure(&response.failure_code) {
        return Err(SandboxCallError::InvalidReceipt);
    }
    let status = SandboxJobStatusV1::try_from(response.status)
        .map_err(|_| SandboxCallError::InvalidReceipt)?;
    match status {
        SandboxJobStatusV1::Pending | SandboxJobStatusV1::Completed
            if response.failure_code.is_empty() =>
        {
            let bundle = if response.bundle_json.is_empty() {
                None
            } else {
                Some(
                    DependencyBundle::parse_record(&response.bundle_json)
                        .map_err(|_| SandboxCallError::InvalidReceipt)?,
                )
            };
            if status == SandboxJobStatusV1::Completed {
                Ok(PreparationOutcome::Completed(
                    bundle.ok_or(SandboxCallError::InvalidReceipt)?,
                ))
            } else {
                Ok(PreparationOutcome::Pending(bundle))
            }
        }
        SandboxJobStatusV1::Failed
            if response.bundle_json.is_empty() && !response.failure_code.is_empty() =>
        {
            Ok(PreparationOutcome::Failed {
                code: response.failure_code,
            })
        }
        SandboxJobStatusV1::Cancelled if response.bundle_json.is_empty() => {
            Ok(PreparationOutcome::Cancelled)
        }
        SandboxJobStatusV1::Uncertain if response.bundle_json.is_empty() => {
            Ok(PreparationOutcome::Uncertain {
                code: response.failure_code,
            })
        }
        _ => Err(SandboxCallError::InvalidReceipt),
    }
}

fn decode_lookup(
    response: LookupSandboxDependenciesResponseV1,
    job: &PreparationJob,
    root: &str,
) -> Result<Option<DependencyBundle>, SandboxCallError> {
    if response.bundle_json.is_empty() {
        return Ok(None);
    }
    if response.bundle_json.len() > 128 * 1024 {
        return Err(SandboxCallError::InvalidReceipt);
    }
    let bundle = DependencyBundle::parse(&response.bundle_json, root)
        .map_err(|_| SandboxCallError::InvalidReceipt)?;
    if !job.matches_bundle(&bundle) {
        return Err(SandboxCallError::InvalidReceipt);
    }
    Ok(Some(bundle))
}

pub(super) fn decode_publication(
    response: PublishSandboxDependenciesResponseV1,
) -> Result<PublicationOutcome, SandboxCallError> {
    if !valid_failure(&response.failure_code) {
        return Err(SandboxCallError::InvalidReceipt);
    }
    match SandboxJobStatusV1::try_from(response.status)
        .map_err(|_| SandboxCallError::InvalidReceipt)?
    {
        SandboxJobStatusV1::Pending if response.failure_code.is_empty() => {
            Ok(PublicationOutcome::Pending)
        }
        SandboxJobStatusV1::Completed if response.failure_code.is_empty() => {
            Ok(PublicationOutcome::Completed)
        }
        SandboxJobStatusV1::Failed if !response.failure_code.is_empty() => {
            Ok(PublicationOutcome::Failed {
                code: response.failure_code,
            })
        }
        SandboxJobStatusV1::Cancelled => Ok(PublicationOutcome::Cancelled),
        SandboxJobStatusV1::Uncertain => Ok(PublicationOutcome::Uncertain {
            code: response.failure_code,
        }),
        _ => Err(SandboxCallError::InvalidReceipt),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    pub(super) fn bundle_json() -> Vec<u8> {
        let content = format!(
            r#"{{"revision":1,"runtime":"pyodide-0.29.0","requirements":[],"files":[{{"name":"elitea-python-lock.json","bytes":2,"sha256":"{}"}}]}}"#,
            "a".repeat(64),
        );
        let root = crate::sandbox::dependency_bundle::hex(
            ring::digest::digest(&ring::digest::SHA256, content.as_bytes()).as_ref(),
        );
        format!(r#"{},"digest":"{}"}}"#, &content[..content.len() - 1], root).into_bytes()
    }

    fn preparation(
        status: SandboxJobStatusV1,
        bundle: &[u8],
    ) -> PrepareSandboxDependenciesResponseV1 {
        PrepareSandboxDependenciesResponseV1 {
            status: status.into(),
            bundle_json: bundle.to_vec(),
            failure_code: String::new(),
            cleanup_pending: false,
        }
    }

    #[test]
    fn ready_metadata_remains_pending_and_completed_requires_a_verified_bundle() {
        let metadata = bundle_json();
        let pending =
            decode_preparation(preparation(SandboxJobStatusV1::Pending, &metadata)).unwrap();
        let PreparationOutcome::Pending(Some(bundle)) = pending else {
            panic!("metadata falsely completed preparation")
        };
        assert_eq!(bundle.file_count(), 1);
        assert!(matches!(
            decode_preparation(preparation(SandboxJobStatusV1::Completed, &metadata)).unwrap(),
            PreparationOutcome::Completed(_)
        ));
        assert!(matches!(
            decode_preparation(preparation(SandboxJobStatusV1::Pending, b"")).unwrap(),
            PreparationOutcome::Pending(None)
        ));
        assert!(decode_preparation(preparation(SandboxJobStatusV1::Completed, b"")).is_err());
    }

    #[test]
    fn preparation_receipt_refuses_metadata_substitution_and_excessive_or_failed_success() {
        let metadata = bundle_json();
        let mut value: serde_json::Value = serde_json::from_slice(&metadata).unwrap();
        for (key, changed) in [
            ("runtime", serde_json::json!("other")),
            ("digest", serde_json::json!("b".repeat(64))),
            ("requirements", serde_json::json!(["new-package"])),
        ] {
            value[key] = changed;
            assert!(
                decode_preparation(preparation(
                    SandboxJobStatusV1::Pending,
                    &serde_json::to_vec(&value).unwrap()
                ))
                .is_err()
            );
            value = serde_json::from_slice(&metadata).unwrap();
        }
        assert!(
            decode_preparation(preparation(
                SandboxJobStatusV1::Pending,
                &vec![b' '; 128 * 1024 + 1]
            ))
            .is_err()
        );
        let mut failed = preparation(SandboxJobStatusV1::Completed, &metadata);
        failed.failure_code = "package-not-found".into();
        assert!(decode_preparation(failed).is_err());
        let mut hidden_failure = preparation(SandboxJobStatusV1::Failed, b"");
        hidden_failure.failure_code = "private\nerror".into();
        assert!(decode_preparation(hidden_failure).is_err());
        let mut cancelled = preparation(SandboxJobStatusV1::Cancelled, b"");
        cancelled.failure_code = "sandbox.cancelled".into();
        assert!(matches!(
            decode_preparation(cancelled).unwrap(),
            PreparationOutcome::Cancelled
        ));
    }

    #[test]
    fn publication_requires_terminal_confirmation_and_keeps_cleanup_separate() {
        let publication =
            |status: SandboxJobStatusV1, failure: &str| PublishSandboxDependenciesResponseV1 {
                status: status.into(),
                failure_code: failure.into(),
                cleanup_pending: true,
            };
        assert!(matches!(
            decode_publication(publication(SandboxJobStatusV1::Pending, "")).unwrap(),
            PublicationOutcome::Pending
        ));
        assert!(matches!(
            decode_publication(publication(SandboxJobStatusV1::Completed, "")).unwrap(),
            PublicationOutcome::Completed
        ));
        assert!(matches!(
            decode_publication(publication(SandboxJobStatusV1::Failed, "package-not-found"))
                .unwrap(),
            PublicationOutcome::Failed { .. }
        ));
        assert!(matches!(
            decode_publication(publication(
                SandboxJobStatusV1::Cancelled,
                "sandbox.cancelled"
            ))
            .unwrap(),
            PublicationOutcome::Cancelled
        ));
        assert!(decode_publication(publication(SandboxJobStatusV1::Completed, "failed")).is_err());
        assert!(decode_publication(publication(SandboxJobStatusV1::Unspecified, "")).is_err());
    }

    #[test]
    fn content_authority_requires_one_exact_lowercase_root() {
        assert_eq!(root_bytes(&"ab".repeat(32)).unwrap(), [0xab; 32]);
        for invalid in [
            "A".repeat(64),
            "z".repeat(64),
            "a".repeat(63),
            "a".repeat(65),
        ] {
            assert!(root_bytes(&invalid).is_err());
        }
    }
}

#[cfg(test)]
mod frozen_lookup_tests {
    use super::*;
    use crate::sandbox::preparation::frozen_lookup_tests::fixture;

    #[test]
    fn frozen_lookup_response_miss_and_exact_hit_cannot_hide_changed_metadata() {
        let (request, bundle) = fixture();
        assert!(
            decode_lookup(
                LookupSandboxDependenciesResponseV1 {
                    bundle_json: Vec::new()
                },
                &request,
                bundle.root()
            )
            .unwrap()
            .is_none()
        );
        let found = decode_lookup(
            LookupSandboxDependenciesResponseV1 {
                bundle_json: bundle.record_json().to_vec(),
            },
            &request,
            bundle.root(),
        )
        .unwrap()
        .unwrap();
        assert_eq!(found.record_json(), bundle.record_json());
        for bytes in [b"{}".to_vec(), vec![b' '; 128 * 1024 + 1]] {
            assert!(matches!(
                decode_lookup(
                    LookupSandboxDependenciesResponseV1 { bundle_json: bytes },
                    &request,
                    bundle.root()
                ),
                Err(SandboxCallError::InvalidReceipt)
            ));
        }
        assert!(matches!(
            decode_lookup(
                LookupSandboxDependenciesResponseV1 {
                    bundle_json: bundle.record_json().to_vec(),
                },
                &request,
                &"c".repeat(64)
            ),
            Err(SandboxCallError::InvalidReceipt)
        ));
        let mut changed: serde_json::Value =
            serde_json::from_slice(&request.to_transport().unwrap()).unwrap();
        changed["timeout_seconds"] = 61.into();
        let changed =
            PreparationJob::from_transport(&serde_json::to_vec(&changed).unwrap()).unwrap();
        assert!(matches!(
            decode_lookup(
                LookupSandboxDependenciesResponseV1 {
                    bundle_json: bundle.record_json().to_vec(),
                },
                &changed,
                bundle.root()
            ),
            Err(SandboxCallError::InvalidReceipt)
        ));
    }
}
