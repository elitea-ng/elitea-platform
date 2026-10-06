//! Authenticated supervisor RPC composition. No plaintext listener is provided.
use std::{future::Future, sync::Arc, time::Duration};

use tonic::{
    Request, Response, Status,
    transport::{Certificate, Identity, Server, ServerTlsConfig},
};

use super::{
    dependency_content::{DependencyContentClient, DependencyContentError},
    docker_supervisor::{
        DependencyDelivery, DockerSupervisor, PreparationReconciliation, Reconciliation,
        SupervisorError,
    },
    ledger::{LedgerError, Phase},
    peer_identity::authenticated_peer,
    request::PreparedJob,
};
use crate::protocol::{
    command::Ed25519PublicKeyResolver,
    elitea::runtime::v1::{
        CancelSandboxJobRequestV1, CancelSandboxJobResponseV1, HydrateSandboxDependenciesRequestV1,
        HydrateSandboxDependenciesResponseV1, PrepareSandboxDependenciesRequestV1,
        PrepareSandboxDependenciesResponseV1, PublishSandboxDependenciesRequestV1,
        PublishSandboxDependenciesResponseV1, SandboxJobStatusV1, SubmitSandboxJobRequestV1,
        SubmitSandboxJobResponseV1,
        sandbox_supervisor_service_server::{
            SandboxSupervisorService, SandboxSupervisorServiceServer,
        },
    },
    sandbox_grant::{AuthorizedCodeIntent, AuthorizedJob, GrantVerifier},
};

// This admission boundary has no runtime or content IO. A broker execution must
// bind its exact original visit before inert hydration can allocate a runtime.
fn authorize_indexed_hydration<R: Ed25519PublicKeyResolver>(
    verifier: &GrantVerifier<R>,
    peer: &str,
    grant: &crate::protocol::elitea::runtime::v1::SignedSandboxJobGrantV1,
    prepared: &PreparedJob,
    wire: &[u8],
    now: i64,
) -> Result<(AuthorizedJob, Option<AuthorizedCodeIntent>), Status> {
    let authorization = verifier.verify(grant, peer, prepared, now).map_err(|_| {
        Status::permission_denied("The execution grant does not authorize package staging.")
    })?;
    if wire.is_empty() {
        if prepared.platform_client().is_some() {
            return Err(Status::permission_denied(
                "The original Code intent is required for broker package staging.",
            ));
        }
        return Ok((authorization, None));
    }
    let (execution, generation, dispatch) = authorization.original_execution();
    let intent = verifier
        .verify_code_intent(
            wire,
            peer,
            authorization.scope(),
            execution,
            generation,
            dispatch,
            prepared,
            now,
        )
        .map_err(|_| {
            Status::permission_denied(
                "The original Code intent does not authorize package staging.",
            )
        })?;
    Ok((authorization, Some(intent)))
}

#[derive(Debug, thiserror::Error)]
pub enum SupervisorServeError {
    #[error("sandbox TLS crypto provider initialization failed")]
    Crypto(#[from] crate::diagnostics::DiagnosticInitError),
    #[error("sandbox TLS listener failed")]
    Transport(#[from] tonic::transport::Error),
}

pub struct SupervisorService<R> {
    verifier: Arc<GrantVerifier<R>>,
    supervisor: Arc<DockerSupervisor>,
    content: Option<Arc<DependencyContentClient>>,
}

impl<R: Ed25519PublicKeyResolver + 'static> SupervisorService<R> {
    #[must_use]
    pub fn new(verifier: GrantVerifier<R>, supervisor: Arc<DockerSupervisor>) -> Self {
        Self {
            verifier: Arc::new(verifier),
            supervisor,
            content: None,
        }
    }

    #[must_use]
    pub fn with_dependency_content(mut self, content: Arc<DependencyContentClient>) -> Self {
        self.content = Some(content);
        self
    }

    /// Require a verified client certificate before dispatching any RPC.
    /// # Errors
    /// Returns a typed error for crypto initialization, TLS configuration, or serving failure.
    pub async fn serve(
        self,
        listener: tokio::net::TcpListener,
        identity: Identity,
        client_ca: Certificate,
        shutdown: impl Future<Output = ()> + Send + 'static,
    ) -> Result<(), SupervisorServeError> {
        crate::diagnostics::install_tls_crypto_provider()?;
        let supervisor = Arc::clone(&self.supervisor);
        let owner_reads =
            CodeOwnerJsonService::new(Arc::clone(&self.verifier), Arc::clone(&self.supervisor));
        let platform_owner_reads = code_recovery::CodePlatformOwnerJsonService::new(
            Arc::clone(&self.verifier),
            Arc::clone(&self.supervisor),
        );
        let service = SandboxSupervisorServiceServer::new(self)
            .max_decoding_message_size(6 * 1024 * 1024)
            .max_encoding_message_size(512 * 1024 + 8192);
        let server = Server::builder()
            .tls_config(
                ServerTlsConfig::new()
                    .identity(identity)
                    .client_ca_root(client_ca)
                    .timeout(Duration::from_secs(10)),
            )?
            .concurrency_limit_per_connection(32)
            .timeout(Duration::from_secs(3665))
            .add_service(service)
            .add_service(owner_reads)
            .add_service(platform_owner_reads)
            .serve_with_incoming_shutdown(
                tokio_stream::wrappers::TcpListenerStream::new(listener),
                shutdown,
            );
        // Both futures are owned by the listener. Shutdown drops recovery; its
        // durable intent and lease remain available to the replacement process.
        tokio::select! {
            result = server => { result?; },
            () = supervisor.recover_cancellations() => {},
        }
        Ok(())
    }
}

#[path = "service_code_recovery.rs"]
mod code_recovery;
use code_recovery::CodeOwnerJsonService;

#[tonic::async_trait]
impl<R: Ed25519PublicKeyResolver + 'static> SandboxSupervisorService for SupervisorService<R> {
    async fn hydrate_sandbox_workspace(
        &self,
        request: Request<crate::protocol::elitea::runtime::v1::HydrateSandboxWorkspaceRequestV1>,
    ) -> Result<
        Response<crate::protocol::elitea::runtime::v1::HydrateSandboxWorkspaceResponseV1>,
        Status,
    > {
        self.hydrate_workspace(request).await
    }

    async fn submit_rust_compiled_snapshot(
        &self,
        request: Request<crate::protocol::elitea::runtime::v1::SubmitRustCompiledSnapshotRequestV1>,
    ) -> Result<
        Response<crate::protocol::elitea::runtime::v1::SubmitRustCompiledSnapshotResponseV1>,
        Status,
    > {
        self.submit_snapshot(request).await
    }
    async fn publish_rust_compiled_snapshot(
        &self,
        request: Request<
            crate::protocol::elitea::runtime::v1::PublishRustCompiledSnapshotRequestV1,
        >,
    ) -> Result<
        Response<crate::protocol::elitea::runtime::v1::PublishRustCompiledSnapshotResponseV1>,
        Status,
    > {
        self.publish_snapshot(request).await
    }

    async fn prepare_sandbox_dependencies(
        &self,
        request: Request<PrepareSandboxDependenciesRequestV1>,
    ) -> Result<Response<PrepareSandboxDependenciesResponseV1>, Status> {
        let peer = authenticated_peer(&request)?;
        let input = request.into_inner();
        let grant = input
            .grant
            .ok_or_else(|| Status::unauthenticated("A current preparation grant is required."))?;
        let prepared =
            super::preparation::PreparationJob::from_transport(&input.preparation_job_json)
                .map_err(|_| {
                    Status::invalid_argument(
                        "Dependency preparation has invalid source, runtime, or limits.",
                    )
                })?;
        let authorization = self
            .verifier
            .verify_preparation(
                &grant,
                &peer,
                &prepared,
                chrono::Utc::now().timestamp_millis(),
            )
            .map_err(|_| {
                Status::permission_denied(
                    "The preparation grant is expired or does not authorize this request.",
                )
            })?;
        let outcome = self
            .supervisor
            .prepare_authorized(&authorization, &prepared)
            .await
            .map_err(|error| service_error(&error))?;
        Ok(Response::new(preparation_response(outcome)?))
    }

    async fn publish_sandbox_dependencies(
        &self,
        request: Request<PublishSandboxDependenciesRequestV1>,
    ) -> Result<Response<PublishSandboxDependenciesResponseV1>, Status> {
        let peer = authenticated_peer(&request)?;
        let input = request.into_inner();
        let grant = input.content_grant.ok_or_else(|| {
            Status::unauthenticated("A current dependency content grant is required.")
        })?;
        let authorization = self
            .verifier
            .verify_content(&grant, &peer, chrono::Utc::now().timestamp_millis())
            .map_err(|_| {
                Status::permission_denied(
                    "The content grant is expired or does not authorize this request.",
                )
            })?;
        let content = self.content.as_deref().ok_or_else(|| {
            Status::failed_precondition(
                "Shared dependency storage is not configured on this supervisor.",
            )
        })?;
        let outcome = self
            .supervisor
            .publish_authorized(&authorization, &grant, input.index, content)
            .await
            .map_err(|error| service_error(&error))?;
        let receipt = preparation_response(outcome)?;
        Ok(Response::new(PublishSandboxDependenciesResponseV1 {
            status: receipt.status,
            failure_code: receipt.failure_code,
            cleanup_pending: receipt.cleanup_pending,
        }))
    }

    async fn hydrate_sandbox_dependencies(
        &self,
        request: Request<HydrateSandboxDependenciesRequestV1>,
    ) -> Result<Response<HydrateSandboxDependenciesResponseV1>, Status> {
        let peer = authenticated_peer(&request)?;
        let input = request.into_inner();
        let job_grant = input.execution_grant.as_ref().ok_or_else(|| {
            Status::unauthenticated("A current execution grant is required for package staging.")
        })?;
        let content_grant = input.content_grant.as_ref().ok_or_else(|| {
            Status::unauthenticated("A current content grant is required for package staging.")
        })?;
        let prepared = PreparedJob::from_transport(&input.prepared_job_json)
            .map_err(|_| Status::invalid_argument("The package staging request is invalid."))?;
        let now = chrono::Utc::now().timestamp_millis();
        let (authorization, intent) = authorize_indexed_hydration(
            &self.verifier,
            &peer,
            job_grant,
            &prepared,
            &input.code_execution_intent_json,
            now,
        )?;
        let content_authority = self
            .verifier
            .verify_content(content_grant, &peer, now)
            .map_err(|_| {
                Status::permission_denied("The content grant does not authorize package staging.")
            })?;
        let root = prepared.dependency_bundle_root().ok_or_else(|| {
            Status::invalid_argument("Package staging requires a recorded bundle identity.")
        })?;
        let bundle = super::dependency_bundle::DependencyBundle::parse(&input.bundle_json, root)
            .map_err(|_| {
                Status::data_loss("Package metadata differs from the recorded execution bundle.")
            })?;
        let client = self.content.as_deref().ok_or_else(|| {
            Status::failed_precondition(
                "Shared dependency storage is not configured on this supervisor.",
            )
        })?;
        let delivery = DependencyDelivery {
            client,
            grant: content_grant,
            authorization: &content_authority,
            bundle: &bundle,
        };
        if let Some(intent) = intent {
            self.supervisor
                .register_code_intent(authorization.scope(), &intent, &prepared)
                .await
                .map_err(|error| service_error(&SupervisorError::Ledger(error)))?;
        }
        let ready = self
            .supervisor
            .hydrate_authorized(&authorization, &prepared, delivery, &bundle, input.index)
            .await
            .map_err(|error| service_error(&error))?;
        Ok(Response::new(HydrateSandboxDependenciesResponseV1 {
            ready,
        }))
    }

    async fn cancel_sandbox_job(
        &self,
        request: Request<CancelSandboxJobRequestV1>,
    ) -> Result<Response<CancelSandboxJobResponseV1>, Status> {
        let peer = authenticated_peer(&request)?;
        let grant = request
            .into_inner()
            .grant
            .ok_or_else(|| Status::unauthenticated("A current sandbox stop grant is required."))?;
        let authorization = self
            .verifier
            .verify_cancellation(&grant, &peer, chrono::Utc::now().timestamp_millis())
            .map_err(|_| {
                Status::permission_denied(
                    "The sandbox stop grant is expired or does not authorize this request.",
                )
            })?;
        let outcome = self
            .supervisor
            .cancel_authorized(&authorization)
            .await
            .map_err(|error| service_error(&error))?;
        let receipt = response(outcome)?;
        Ok(Response::new(CancelSandboxJobResponseV1 {
            status: receipt.status,
            cleanup_pending: receipt.cleanup_pending,
        }))
    }

    async fn submit_sandbox_job(
        &self,
        request: Request<SubmitSandboxJobRequestV1>,
    ) -> Result<Response<SubmitSandboxJobResponseV1>, Status> {
        let peer = authenticated_peer(&request)?;
        let input = request.into_inner();
        let grant = input
            .grant
            .ok_or_else(|| Status::unauthenticated("A current sandbox job grant is required."))?;
        let prepared = PreparedJob::from_transport(&input.prepared_job_json).map_err(|_| {
            Status::invalid_argument("The sandbox job has invalid code, state, runtime, or limits.")
        })?;
        let authorization = self
            .verifier
            .verify(
                &grant,
                &peer,
                &prepared,
                chrono::Utc::now().timestamp_millis(),
            )
            .map_err(|_| {
                Status::permission_denied(
                    "The sandbox job grant is expired or does not authorize this request.",
                )
            })?;
        if !input.code_execution_intent_json.is_empty() {
            let (execution, generation, dispatch) = authorization.original_execution();
            let intent = self
                .verifier
                .verify_code_intent(
                    &input.code_execution_intent_json,
                    &peer,
                    authorization.scope(),
                    execution,
                    generation,
                    dispatch,
                    &prepared,
                    chrono::Utc::now().timestamp_millis(),
                )
                .map_err(|_| {
                    Status::permission_denied(
                        "The original whole-Code intent does not match this Execute request.",
                    )
                })?;
            self.supervisor
                .register_code_intent(authorization.scope(), &intent, &prepared)
                .await
                .map_err(|error| service_error(&error.into()))?;
        }
        let content_authority = match (&input.dependency_content_grant, prepared.dependency_bundle_root()) {
            (None, None) if input.dependency_bundle_json.is_empty() => None,
            (Some(grant), Some(_)) => Some(self.verifier.verify_content(grant, &peer, chrono::Utc::now().timestamp_millis())
                .map_err(|_| Status::permission_denied("Dependency content authority is expired or does not authorize this request."))?),
            _ => return Err(Status::invalid_argument("Only a root-bound execution request may carry dependency content authority.")),
        };
        let bundle = prepared
            .dependency_bundle_root()
            .map(|root| {
                super::dependency_bundle::DependencyBundle::parse(
                    &input.dependency_bundle_json,
                    root,
                )
                .map_err(|_| {
                    Status::data_loss(
                        "Dependency metadata does not match the recorded execution bundle.",
                    )
                })
            })
            .transpose()?;
        let delivery = if let Some(content_authority) = content_authority.as_ref() {
            Some(DependencyDelivery {
                client: self.content.as_deref().ok_or_else(|| {
                    Status::failed_precondition(
                        "Shared dependency storage is not configured on this supervisor.",
                    )
                })?,
                grant: input.dependency_content_grant.as_ref().ok_or_else(|| {
                    Status::invalid_argument("Dependency content authority is missing.")
                })?,
                authorization: content_authority,
                bundle: bundle
                    .as_ref()
                    .ok_or_else(|| Status::invalid_argument("Dependency metadata is missing."))?,
            })
        } else {
            None
        };
        let outcome = self
            .supervisor
            .submit_authorized_with_dependencies(&authorization, &prepared, delivery)
            .await
            .map_err(|error| service_error(&error))?;
        Ok(Response::new(response(outcome)?))
    }
}

fn preparation_response(
    outcome: PreparationReconciliation,
) -> Result<PrepareSandboxDependenciesResponseV1, Status> {
    match outcome {
        PreparationReconciliation::Pending { bundle } => Ok(PrepareSandboxDependenciesResponseV1 {
            status: SandboxJobStatusV1::Pending.into(),
            bundle_json: bundle
                .map(|value| value.record_json().to_vec())
                .unwrap_or_default(),
            failure_code: String::new(),
            cleanup_pending: false,
        }),
        PreparationReconciliation::Terminal {
            record,
            cleanup_pending,
        } => {
            let receipt = response(Reconciliation::Terminal {
                record,
                cleanup_pending,
            })?;
            if receipt.status == i32::from(SandboxJobStatusV1::Completed) {
                crate::sandbox::dependency_bundle::DependencyBundle::parse_record(
                    &receipt.result_json,
                )
                .map_err(|_| {
                    Status::data_loss(
                        "The recorded dependency bundle is invalid. Do not resolve it again.",
                    )
                })?;
            }
            Ok(PrepareSandboxDependenciesResponseV1 {
                status: receipt.status,
                bundle_json: receipt.result_json,
                failure_code: receipt.failure_code,
                cleanup_pending: receipt.cleanup_pending,
            })
        }
    }
}

fn response(outcome: Reconciliation) -> Result<SubmitSandboxJobResponseV1, Status> {
    let Reconciliation::Terminal {
        record,
        cleanup_pending,
    } = outcome
    else {
        return Ok(SubmitSandboxJobResponseV1 {
            status: SandboxJobStatusV1::Pending.into(),
            ..Default::default()
        });
    };
    let status = match record.phase {
        Phase::Completed => SandboxJobStatusV1::Completed,
        Phase::Failed => SandboxJobStatusV1::Failed,
        Phase::Cancelled => SandboxJobStatusV1::Cancelled,
        Phase::Uncertain => SandboxJobStatusV1::Uncertain,
        Phase::Reserved | Phase::Dispatched => {
            return Err(Status::internal("The sandbox receipt is not terminal."));
        }
    };
    Ok(SubmitSandboxJobResponseV1 {
        status: status.into(),
        result_json: record.result_json.unwrap_or_default().into_bytes(),
        failure_code: record.failure_code.unwrap_or_default(),
        cleanup_pending,
    })
}

fn service_error(error: &SupervisorError) -> Status {
    match error {
        SupervisorError::Invalid => Status::failed_precondition(
            "The job does not match the admitted runtime policy or its grant expired. Request a fresh grant.",
        ),
        SupervisorError::Busy => Status::resource_exhausted(
            "The sandbox supervisor is at capacity. Retry the same job later.",
        ),
        SupervisorError::Ledger(LedgerError::Conflict) => {
            Status::already_exists("This sandbox activation already exists with different inputs.")
        }
        SupervisorError::Ledger(LedgerError::Fenced) => {
            Status::aborted("Another supervisor owns this job. Retry the same activation.")
        }
        SupervisorError::Ledger(LedgerError::Missing) => {
            Status::not_found("The sandbox job was not found.")
        }
        SupervisorError::Ledger(LedgerError::Invalid) | SupervisorError::Receipt => {
            Status::data_loss(
                "The sandbox receipt could not be validated. Do not start a replacement job.",
            )
        }
        SupervisorError::Ledger(LedgerError::SchemaMigrationRequired(schema)) => {
            Status::failed_precondition(schema.to_string())
        }
        SupervisorError::Ledger(LedgerError::Database(_)) => {
            Status::unavailable("Sandbox persistence is unavailable. Retry the same activation.")
        }
        SupervisorError::Content(error) => match error {
            DependencyContentError::Integrity => Status::data_loss(error.to_string()),
            // Admission already verified authority. It can expire during native export.
            DependencyContentError::Authority => Status::aborted(error.to_string()),
            DependencyContentError::Configuration => Status::failed_precondition(error.to_string()),
            DependencyContentError::Busy => Status::resource_exhausted(error.to_string()),
            _ => Status::unavailable(error.to_string()),
        },
        SupervisorError::Runtime(_) => Status::unavailable(
            "The sandbox runtime could not be observed. Reconcile the same activation; do not start a replacement.",
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::sandbox::ledger::JobRecord;

    mod indexed_hydration {
        use super::*;
        use crate::{
            protocol::elitea::runtime::v1::{SandboxJobGrantClaimsV1, SignedSandboxJobGrantV1},
            sandbox::{
                code_recovery::{hex, original_job_key, sha256},
                native_bundle::{NativeKind, NativePlatform},
                platform_client_binding::PlatformClientBinding,
                request::{Language, NativeDependencies},
            },
        };
        use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
        use prost::Message as _;
        use ring::signature::{Ed25519KeyPair, KeyPair as _};

        struct Keys([u8; 32]);
        impl Ed25519PublicKeyResolver for Keys {
            fn resolve_ed25519_public_key(&self, id: &str) -> Option<[u8; 32]> {
                (id == "hydration-fixture").then_some(self.0)
            }
        }
        struct Fixture {
            key: Ed25519KeyPair,
            verifier: GrantVerifier<Keys>,
            job: PreparedJob,
            execute: SandboxJobGrantClaimsV1,
            intent: serde_json::Value,
        }
        impl Fixture {
            fn new(broker: bool) -> Self {
                let key = Ed25519KeyPair::from_seed_unchecked(&[19; 32]).unwrap();
                let verifier = GrantVerifier::new(
                    Keys(key.public_key().as_ref().try_into().unwrap()),
                    "dns:supervisor.fixture".into(),
                )
                .unwrap();
                let source = "export function main() { return 41; }";
                let mut job = PreparedJob::new(
                    Language::JavaScript,
                    source.into(),
                    std::collections::BTreeMap::from([(
                        "input".into(),
                        serde_json::json!("original"),
                    )]),
                    format!("sha256:{}", "a".repeat(64)),
                    "deno-execute-v1".into(),
                    60,
                )
                .unwrap()
                .with_native_dependency_bundle(
                    "b".repeat(64),
                    NativeDependencies {
                        kind: NativeKind::Deno,
                        platform: NativePlatform {
                            os: "linux".into(),
                            arch: "arm64".into(),
                            abi: "gnu".into(),
                        },
                        preparation_sha256: "c".repeat(64),
                        source_sha256: sha256(source.as_bytes()),
                        dependencies_toml: None,
                    },
                )
                .unwrap();
                if broker {
                    job = job
                        .with_platform_client(
                            PlatformClientBinding::new("d".repeat(64), 8, 4096).unwrap(),
                        )
                        .unwrap();
                }
                let execute = SandboxJobGrantClaimsV1 {
                    revision: 1,
                    cancel_only: false,
                    tenant_id: "hydration-fixture".into(),
                    project_id: 1,
                    execution_id: "1".repeat(32),
                    activation_id: "2".repeat(64),
                    request_digest: job.fingerprint().unwrap().to_vec(),
                    submitter_workload_identity: "dns:worker.fixture".into(),
                    audience: "dns:supervisor.fixture".into(),
                    issued_at_unix_millis: 1000,
                    expires_at_unix_millis: 31000,
                    generation: 3,
                    dependency_bundle_sha256: vec![],
                };
                let intent = serde_json::json!({
                    "schema":"elitea.sandbox.original-code-intent.v1", "purpose":"whole_code_execute",
                    "tenant_id":execute.tenant_id,"project_id":execute.project_id,
                    "execution_id":execute.execution_id,"original_generation":execute.generation,
                    "claim_id":"3".repeat(32),"claim_attempt":1,"lease_epoch":1,"fence_sha256":"4".repeat(64),
                    "activation_id":"5".repeat(64),"node_id":"code","graph_thread":"original-thread",
                    "step":2,"attempt":1,"node_digest":"6".repeat(64),
                    "dispatch_activation":execute.activation_id,
                    "job_key":original_job_key(&execute.execution_id, &execute.activation_id),
                    "request_digest":hex(&job.fingerprint().unwrap()),
                    "supervisor_audience":execute.audience,"submitter_workload_identity":execute.submitter_workload_identity,
                    "language":"javascript","prepared_job_sha256":sha256(&job.to_transport().unwrap()),
                    "source_sha256":sha256(source.as_bytes()),"input_sha256":sha256(br#"{"input":"original"}"#),
                    "issued_at_unix_millis":1000,"expires_at_unix_millis":31000,
                });
                Self {
                    key,
                    verifier,
                    job,
                    execute,
                    intent,
                }
            }
            fn grant(&self, claims: &SandboxJobGrantClaimsV1) -> SignedSandboxJobGrantV1 {
                let bytes = claims.encode_to_vec();
                let mut framed = b"elitea.sandbox.job-grant.ed25519.v1\0".to_vec();
                framed.extend_from_slice(&(bytes.len() as u64).to_be_bytes());
                framed.extend_from_slice(&bytes);
                SignedSandboxJobGrantV1 {
                    key_id: "hydration-fixture".into(),
                    claims_bytes: bytes,
                    signature: self.key.sign(&framed).as_ref().to_vec(),
                }
            }
            fn wire(&self, claims: &serde_json::Value) -> Vec<u8> {
                let bytes = serde_json::to_vec(claims).unwrap();
                let mut framed = b"elitea.sandbox.original-code-intent.ed25519.v1\0".to_vec();
                framed.extend_from_slice(&(bytes.len() as u64).to_be_bytes());
                framed.extend_from_slice(&bytes);
                serde_json::to_vec(&serde_json::json!({
                    "schema":"elitea.sandbox.original-code-intent-signed.v1", "key_id":"hydration-fixture",
                    "claims_base64url":URL_SAFE_NO_PAD.encode(&bytes),
                    "signature_base64url":URL_SAFE_NO_PAD.encode(self.key.sign(&framed).as_ref()),
                })).unwrap()
            }
            fn admit(
                &self,
                wire: &[u8],
            ) -> Result<(AuthorizedJob, Option<AuthorizedCodeIntent>), Status> {
                authorize_indexed_hydration(
                    &self.verifier,
                    "dns:worker.fixture",
                    &self.grant(&self.execute),
                    &self.job,
                    wire,
                    1001,
                )
            }
        }

        #[test]
        fn refreshed_native_broker_intent_preserves_original_visit_and_exact_launch_binding() {
            let f = Fixture::new(true);
            let (authorization, intent) = f.admit(&f.wire(&f.intent)).unwrap();
            let binding = intent
                .unwrap()
                .binding(authorization.scope(), 1001)
                .unwrap()
                .clone();
            assert_eq!(binding.activation_id, "5".repeat(64));
            assert_eq!(binding.dispatch_activation, "2".repeat(64));
            assert_eq!(binding.original_generation, 3);
            assert_eq!(binding.graph_thread, "original-thread");
            let broker = crate::sandbox::code_platform_owner::CodePlatformBinding::from_job(
                &f.job, &binding,
            )
            .unwrap()
            .unwrap();
            assert_eq!(broker.policy_sha256, "d".repeat(64));
            assert_eq!(broker.prepared_fingerprint, binding.request_digest);
            assert_eq!(broker.prepared_job_sha256, binding.prepared_job_sha256);
            let mut refreshed = f.intent.clone();
            refreshed["claim_id"] = "7".repeat(32).into();
            refreshed["claim_attempt"] = 2.into();
            refreshed["lease_epoch"] = 2.into();
            refreshed["fence_sha256"] = "8".repeat(64).into();
            let (authorization, intent) = f.admit(&f.wire(&refreshed)).unwrap();
            assert_eq!(
                intent
                    .unwrap()
                    .binding(authorization.scope(), 1001)
                    .unwrap(),
                &binding
            );
            let wire: serde_json::Value =
                serde_json::from_slice(&f.job.to_transport().unwrap()).unwrap();
            assert_eq!(wire["revision"], 5);
            assert_eq!(wire["native_dependencies"]["kind"], "deno");
        }

        #[test]
        fn missing_broker_intent_is_denied_at_admission_before_any_runtime_or_content_io() {
            let f = Fixture::new(true);
            assert_eq!(
                f.admit(&[]).err().unwrap().code(),
                tonic::Code::PermissionDenied
            );
            assert_eq!(
                f.admit(&vec![b'x'; 16 * 1024 + 1]).err().unwrap().code(),
                tonic::Code::PermissionDenied
            );
        }

        #[test]
        fn legacy_pure_native_hydration_keeps_optional_intent_and_validates_supplied_intent() {
            let f = Fixture::new(false);
            let (authority, intent) = f.admit(&[]).unwrap();
            assert!(intent.is_none());
            assert!(authority.permits(&f.job, 1001));
            assert!(f.admit(&f.wire(&f.intent)).unwrap().1.is_some());
            assert_eq!(
                f.admit(b"not-a-signed-intent").err().unwrap().code(),
                tonic::Code::PermissionDenied
            );
            let mut plain: serde_json::Value =
                serde_json::from_slice(&f.job.to_transport().unwrap()).unwrap();
            plain
                .as_object_mut()
                .unwrap()
                .remove("dependency_bundle_sha256");
            plain.as_object_mut().unwrap().remove("native_dependencies");
            plain["revision"] = 1.into();
            let plain = PreparedJob::from_transport(&serde_json::to_vec(&plain).unwrap()).unwrap();
            let grant = SandboxJobGrantClaimsV1 {
                request_digest: plain.fingerprint().unwrap().to_vec(),
                ..f.execute.clone()
            };
            let (authority, intent) = authorize_indexed_hydration(
                &f.verifier,
                "dns:worker.fixture",
                &f.grant(&grant),
                &plain,
                &[],
                1001,
            )
            .unwrap();
            assert!(authority.permits(&plain, 1001));
            assert!(intent.is_none());
        }

        #[test]
        fn signed_intent_cannot_change_original_owner_scope_or_final_prepared_identity() {
            let f = Fixture::new(true);
            for (field, value) in [
                ("tenant_id", serde_json::json!("other-tenant")),
                ("project_id", serde_json::json!(2)),
                ("execution_id", serde_json::json!("9".repeat(32))),
                ("original_generation", serde_json::json!(4)),
                ("dispatch_activation", serde_json::json!("9".repeat(64))),
                ("job_key", serde_json::json!("9".repeat(64))),
                ("request_digest", serde_json::json!("9".repeat(64))),
                ("prepared_job_sha256", serde_json::json!("9".repeat(64))),
                ("source_sha256", serde_json::json!("9".repeat(64))),
                ("input_sha256", serde_json::json!("9".repeat(64))),
                ("language", serde_json::json!("typescript")),
                (
                    "supervisor_audience",
                    serde_json::json!("dns:other.fixture"),
                ),
                (
                    "submitter_workload_identity",
                    serde_json::json!("dns:other.fixture"),
                ),
                ("claim_attempt", serde_json::json!(0)),
                ("lease_epoch", serde_json::json!(0)),
                ("fence_sha256", serde_json::json!("")),
                ("purpose", serde_json::json!("whole_code_compile")),
                ("expires_at_unix_millis", serde_json::json!(1001)),
            ] {
                let mut changed = f.intent.clone();
                changed[field] = value;
                assert_eq!(
                    f.admit(&f.wire(&changed)).err().unwrap().code(),
                    tonic::Code::PermissionDenied,
                    "{field}"
                );
            }
        }

        #[test]
        fn refreshed_execute_grant_cannot_rebind_hydration_to_changed_job_or_broker_policy() {
            let f = Fixture::new(true);
            let wire = f.wire(&f.intent);
            for selector in [
                "source",
                "input",
                "image_digest",
                "native_preparation",
                "broker_policy",
                "broker_limit",
            ] {
                let mut value: serde_json::Value =
                    serde_json::from_slice(&f.job.to_transport().unwrap()).unwrap();
                match selector {
                    "source" => {
                        value["source"] = "export function main() { return 42; }".into();
                        value["native_dependencies"]["source_sha256"] =
                            sha256(value["source"].as_str().unwrap().as_bytes()).into();
                    }
                    "input" => value["input"]["input"] = "changed".into(),
                    "image_digest" => {
                        value["image_digest"] = format!("sha256:{}", "9".repeat(64)).into();
                    }
                    "native_preparation" => {
                        value["native_dependencies"]["preparation_sha256"] = "9".repeat(64).into();
                    }
                    "broker_policy" => {
                        value["platform_client"]["policy_sha256"] = "9".repeat(64).into();
                    }
                    "broker_limit" => value["platform_client"]["max_calls"] = 9.into(),
                    _ => unreachable!(),
                }
                let changed =
                    PreparedJob::from_transport(&serde_json::to_vec(&value).unwrap()).unwrap();
                let grant = SandboxJobGrantClaimsV1 {
                    request_digest: changed.fingerprint().unwrap().to_vec(),
                    ..f.execute.clone()
                };
                assert_eq!(
                    authorize_indexed_hydration(
                        &f.verifier,
                        "dns:worker.fixture",
                        &f.grant(&grant),
                        &changed,
                        &wire,
                        1001
                    )
                    .err()
                    .unwrap()
                    .code(),
                    tonic::Code::PermissionDenied,
                    "{selector}"
                );
            }
        }

        #[test]
        #[allow(
            clippy::too_many_lines,
            reason = "Keep the complete identity and failure assertions in one fixture."
        )]
        fn execution_grant_peer_scope_purpose_and_signature_remain_required() {
            let f = Fixture::new(true);
            let wire = f.wire(&f.intent);
            assert_eq!(
                authorize_indexed_hydration(
                    &f.verifier,
                    "dns:other.fixture",
                    &f.grant(&f.execute),
                    &f.job,
                    &wire,
                    1001
                )
                .err()
                .unwrap()
                .code(),
                tonic::Code::PermissionDenied
            );
            for changed in [
                SandboxJobGrantClaimsV1 {
                    tenant_id: "other-tenant".into(),
                    ..f.execute.clone()
                },
                SandboxJobGrantClaimsV1 {
                    project_id: 2,
                    ..f.execute.clone()
                },
                SandboxJobGrantClaimsV1 {
                    generation: 4,
                    ..f.execute.clone()
                },
                SandboxJobGrantClaimsV1 {
                    execution_id: "9".repeat(32),
                    ..f.execute.clone()
                },
                SandboxJobGrantClaimsV1 {
                    activation_id: "9".repeat(64),
                    ..f.execute.clone()
                },
                SandboxJobGrantClaimsV1 {
                    cancel_only: true,
                    ..f.execute.clone()
                },
                SandboxJobGrantClaimsV1 {
                    revision: 3,
                    dependency_bundle_sha256: vec![1; 32],
                    ..f.execute.clone()
                },
            ] {
                assert_eq!(
                    authorize_indexed_hydration(
                        &f.verifier,
                        "dns:worker.fixture",
                        &f.grant(&changed),
                        &f.job,
                        &wire,
                        1001
                    )
                    .err()
                    .unwrap()
                    .code(),
                    tonic::Code::PermissionDenied
                );
            }
            let mut grant = f.grant(&f.execute);
            grant.signature[0] ^= 1;
            assert_eq!(
                authorize_indexed_hydration(
                    &f.verifier,
                    "dns:worker.fixture",
                    &grant,
                    &f.job,
                    &wire,
                    1001
                )
                .err()
                .unwrap()
                .code(),
                tonic::Code::PermissionDenied
            );
            let mut unsigned: serde_json::Value = serde_json::from_slice(&wire).unwrap();
            unsigned["signature_base64url"] = URL_SAFE_NO_PAD.encode([0; 64]).into();
            assert_eq!(
                f.admit(&serde_json::to_vec(&unsigned).unwrap())
                    .err()
                    .unwrap()
                    .code(),
                tonic::Code::PermissionDenied
            );
            assert_eq!(
                authorize_indexed_hydration(
                    &f.verifier,
                    "dns:worker.fixture",
                    &f.grant(&f.execute),
                    &f.job,
                    &wire,
                    31000
                )
                .err()
                .unwrap()
                .code(),
                tonic::Code::PermissionDenied
            );
        }
    }

    #[test]
    fn pending_and_terminal_receipts_preserve_state() {
        assert_eq!(
            response(Reconciliation::OwnedElsewhere).unwrap().status,
            SandboxJobStatusV1::Pending as i32
        );
        let receipt = response(Reconciliation::Terminal {
            record: JobRecord {
                phase: Phase::Completed,
                result_json: Some("{\"result\":42}".into()),
                failure_code: None,
                runtime_id: None,
            },
            cleanup_pending: true,
        })
        .unwrap();
        assert_eq!(receipt.status, SandboxJobStatusV1::Completed as i32);
        assert_eq!(receipt.result_json, b"{\"result\":42}");
        assert!(receipt.cleanup_pending);
    }

    #[test]
    fn missing_schema_stops_rpc_retry_with_fixed_migration_diagnostic() {
        use crate::sandbox::ledger::SchemaMigrationRequired;
        for schema in [
            SchemaMigrationRequired::MissingColumn,
            SchemaMigrationRequired::MissingTable,
        ] {
            let status = service_error(&SupervisorError::Ledger(
                LedgerError::SchemaMigrationRequired(schema),
            ));
            assert_eq!(status.code(), tonic::Code::FailedPrecondition);
            assert!(status.message().contains(schema.sqlstate()));
            assert!(
                status
                    .message()
                    .contains("Main AgentState release migrations")
            );
            assert!(status.message().contains("same activation"));
        }
        let status = service_error(&SupervisorError::Ledger(LedgerError::Database(
            sqlx::Error::Protocol("PRIVATE_DATABASE_CAUSE".into()),
        )));
        assert_eq!(status.code(), tonic::Code::Unavailable);
        assert!(!status.message().contains("PRIVATE_DATABASE_CAUSE"));
    }

    #[test]
    fn retryable_errors_do_not_advise_a_new_activation() {
        assert_eq!(
            service_error(&SupervisorError::Busy).code(),
            tonic::Code::ResourceExhausted
        );
        assert_eq!(
            service_error(&SupervisorError::Ledger(LedgerError::Conflict)).code(),
            tonic::Code::AlreadyExists
        );
        assert_eq!(
            service_error(&SupervisorError::Receipt).code(),
            tonic::Code::DataLoss
        );
    }
}

#[path = "service_compiled.rs"]
mod compiled;

#[path = "service_workspace.rs"]
mod workspace;
