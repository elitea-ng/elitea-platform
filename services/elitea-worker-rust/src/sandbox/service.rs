//! Authenticated supervisor RPC composition. No plaintext listener is provided.
use std::{future::Future, sync::Arc, time::Duration};

use tonic::{
    Request, Response, Status,
    transport::{Certificate, Identity, Server, ServerTlsConfig},
};

use super::{
    docker_supervisor::{DockerSupervisor, Reconciliation, SupervisorError},
    ledger::{LedgerError, Phase},
    peer_identity::authenticated_peer,
    request::PreparedJob,
};
use crate::protocol::{
    command::Ed25519PublicKeyResolver,
    elitea::runtime::v1::{
        CancelSandboxJobRequestV1, CancelSandboxJobResponseV1, SandboxJobStatusV1,
        SubmitSandboxJobRequestV1, SubmitSandboxJobResponseV1,
        sandbox_supervisor_service_server::{
            SandboxSupervisorService, SandboxSupervisorServiceServer,
        },
    },
    sandbox_grant::GrantVerifier,
};

#[derive(Debug, thiserror::Error)]
pub enum SupervisorServeError {
    #[error("sandbox TLS crypto provider initialization failed")]
    Crypto(#[from] crate::diagnostics::DiagnosticInitError),
    #[error("sandbox TLS listener failed")]
    Transport(#[from] tonic::transport::Error),
}

pub struct SupervisorService<R> {
    verifier: GrantVerifier<R>,
    supervisor: Arc<DockerSupervisor>,
}

impl<R: Ed25519PublicKeyResolver + 'static> SupervisorService<R> {
    #[must_use]
    pub fn new(verifier: GrantVerifier<R>, supervisor: Arc<DockerSupervisor>) -> Self {
        Self {
            verifier,
            supervisor,
        }
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
        let service = SandboxSupervisorServiceServer::new(self)
            .max_decoding_message_size(1024 * 1024 + 8192)
            .max_encoding_message_size(512 * 1024 + 8192);
        Server::builder()
            .tls_config(
                ServerTlsConfig::new()
                    .identity(identity)
                    .client_ca_root(client_ca)
                    .timeout(Duration::from_secs(10)),
            )?
            .concurrency_limit_per_connection(32)
            .timeout(Duration::from_secs(3665))
            .add_service(service)
            .serve_with_incoming_shutdown(
                tokio_stream::wrappers::TcpListenerStream::new(listener),
                shutdown,
            )
            .await?;
        Ok(())
    }
}

#[tonic::async_trait]
impl<R: Ed25519PublicKeyResolver + 'static> SandboxSupervisorService for SupervisorService<R> {
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
        let outcome = self
            .supervisor
            .submit_authorized(&authorization, &prepared)
            .await
            .map_err(|error| service_error(&error))?;
        Ok(Response::new(response(outcome)?))
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
        SupervisorError::Ledger(LedgerError::Database(_)) => {
            Status::unavailable("Sandbox persistence is unavailable. Retry the same activation.")
        }
        SupervisorError::Runtime(_) => Status::unavailable(
            "The sandbox runtime could not be observed. Reconcile the same activation; do not start a replacement.",
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::sandbox::ledger::JobRecord;

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
            },
            cleanup_pending: true,
        })
        .unwrap();
        assert_eq!(receipt.status, SandboxJobStatusV1::Completed as i32);
        assert_eq!(receipt.result_json, b"{\"result\":42}");
        assert!(receipt.cleanup_pending);
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
