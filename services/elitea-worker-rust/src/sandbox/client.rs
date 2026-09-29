//! Worker-side transport only. No container runtime or receipt database access.
#[cfg(all(test, feature = "sandbox-supervisor"))]
#[path = "client_disconnect_tests.rs"]
mod disconnect_tests;
use super::request::PreparedJob;
use crate::{
    protocol::elitea::runtime::v1::{
        AuthorizeSandboxJobRequestV1, CancelSandboxJobRequestV1, SandboxJobStatusV1,
        SignedSandboxJobGrantV1, SubmitSandboxJobRequestV1, SubmitSandboxJobResponseV1,
        sandbox_supervisor_service_client::SandboxSupervisorServiceClient,
    },
    transport::control_grpc::{ControlGrpcClient, ControlGrpcError, ControlRpc},
};
use std::time::Duration;
use tonic::{Code, Request, transport::Channel};

/// Output is untrusted. The graph must apply its typed state projection.
pub enum SandboxOutcome {
    Pending,
    Completed(Vec<u8>),
    Failed { code: String },
    Cancelled,
    Uncertain { code: String },
}

#[derive(Debug, thiserror::Error)]
pub enum SandboxCallError {
    #[error("sandbox transport configuration or prepared request is invalid")]
    Invalid,
    #[error("Main could not authorize the sandbox job: {0}")]
    Authorization(#[from] ControlGrpcError),
    #[error("Main rejected the sandbox job grant")]
    Rejected,
    #[error(
        "sandbox submission returned {code}; reconcile the same activation before any further execution"
    )]
    Submission { code: Code },
    #[error("the sandbox response is malformed; completion cannot be confirmed")]
    InvalidReceipt,
}

#[derive(Clone)]
pub struct SandboxClient {
    rpc: SandboxSupervisorServiceClient<Channel>,
    audience: String,
    deadline: Duration,
}

impl SandboxClient {
    pub(crate) fn audience(&self) -> &str {
        &self.audience
    }

    /// The channel must use deployment-verified mTLS, as with the control client.
    /// # Errors
    /// Returns `Invalid` for an invalid audience or unbounded deadline.
    pub fn from_channel(
        channel: Channel,
        audience: String,
        deadline: Duration,
    ) -> Result<Self, SandboxCallError> {
        if audience.is_empty()
            || audience.len() > 256
            || audience
                .chars()
                .any(|c| c.is_control() || c.is_whitespace())
            || deadline.is_zero()
            || deadline > Duration::from_secs(3665)
        {
            return Err(SandboxCallError::Invalid);
        }
        Ok(Self {
            rpc: SandboxSupervisorServiceClient::new(channel)
                .max_encoding_message_size(1024 * 1024 + 8192)
                .max_decoding_message_size(512 * 1024 + 8192),
            audience,
            deadline,
        })
    }

    /// Make one grant/submission attempt. The caller owns durable retry policy.
    /// Always derive the digest and audience here, never from model arguments.
    /// # Errors
    /// Returns authorization, transport, or receipt errors without user payloads.
    pub async fn submit<R: ControlRpc>(
        &self,
        control: &ControlGrpcClient<R>,
        mut authorization: AuthorizeSandboxJobRequestV1,
        job: &PreparedJob,
    ) -> Result<SandboxOutcome, SandboxCallError> {
        authorization.request_digest = job
            .fingerprint()
            .map_err(|_| SandboxCallError::Invalid)?
            .to_vec();
        authorization.audience.clone_from(&self.audience);
        authorization.cancel_only = false;
        let response = control.authorize_sandbox_job(authorization).await?;
        if response.rejection.is_some() {
            return Err(SandboxCallError::Rejected);
        }
        let grant = response.grant.ok_or(SandboxCallError::Rejected)?;
        self.submit_granted(grant, job).await
    }

    /// Request stop authority for the same immutable job. Never dispatch code.
    /// # Errors
    /// Returns authorization, transport, or malformed response errors.
    pub async fn cancel<R: ControlRpc>(
        &self,
        control: &ControlGrpcClient<R>,
        authorization: AuthorizeSandboxJobRequestV1,
        job: &PreparedJob,
    ) -> Result<SandboxJobStatusV1, SandboxCallError> {
        let digest = job.fingerprint().map_err(|_| SandboxCallError::Invalid)?;
        self.cancel_digest(control, authorization, &digest).await
    }

    pub(crate) async fn cancel_digest<R: ControlRpc>(
        &self,
        control: &ControlGrpcClient<R>,
        mut authorization: AuthorizeSandboxJobRequestV1,
        digest: &[u8; 32],
    ) -> Result<SandboxJobStatusV1, SandboxCallError> {
        authorization.request_digest = digest.to_vec();
        authorization.audience.clone_from(&self.audience);
        authorization.cancel_only = true;
        let response = control.authorize_sandbox_job(authorization).await?;
        if response.rejection.is_some() {
            return Err(SandboxCallError::Rejected);
        }
        self.cancel_granted(response.grant.ok_or(SandboxCallError::Rejected)?)
            .await
    }

    pub(crate) async fn cancel_granted(
        &self,
        grant: SignedSandboxJobGrantV1,
    ) -> Result<SandboxJobStatusV1, SandboxCallError> {
        if grant.signature.len() != 64
            || grant.claims_bytes.is_empty()
            || grant.claims_bytes.len() > 4096
            || grant.key_id.is_empty()
            || grant.key_id.len() > 256
        {
            return Err(SandboxCallError::Rejected);
        }
        let mut request = Request::new(CancelSandboxJobRequestV1 { grant: Some(grant) });
        request.set_timeout(self.deadline);
        let response =
            tokio::time::timeout(self.deadline, self.rpc.clone().cancel_sandbox_job(request))
                .await
                .map_err(|_| SandboxCallError::Submission {
                    code: Code::DeadlineExceeded,
                })?
                .map_err(|status| SandboxCallError::Submission {
                    code: submission_code(&status),
                })?
                .into_inner();
        match SandboxJobStatusV1::try_from(response.status) {
            Ok(SandboxJobStatusV1::Pending) if response.cleanup_pending => {
                Err(SandboxCallError::InvalidReceipt)
            }
            Ok(SandboxJobStatusV1::Unspecified) | Err(_) => Err(SandboxCallError::InvalidReceipt),
            Ok(status) => Ok(status),
        }
    }

    pub(crate) async fn submit_granted(
        &self,
        grant: SignedSandboxJobGrantV1,
        job: &PreparedJob,
    ) -> Result<SandboxOutcome, SandboxCallError> {
        if grant.signature.len() != 64
            || grant.claims_bytes.is_empty()
            || grant.claims_bytes.len() > 4096
            || grant.key_id.is_empty()
            || grant.key_id.len() > 256
        {
            return Err(SandboxCallError::Rejected);
        }
        let mut request = Request::new(SubmitSandboxJobRequestV1 {
            grant: Some(grant),
            prepared_job_json: job.to_transport().map_err(|_| SandboxCallError::Invalid)?,
        });
        request.set_timeout(self.deadline);
        let response =
            tokio::time::timeout(self.deadline, self.rpc.clone().submit_sandbox_job(request))
                .await
                .map_err(|_| SandboxCallError::Submission {
                    code: Code::DeadlineExceeded,
                })?
                .map_err(|status| SandboxCallError::Submission {
                    code: submission_code(&status),
                })?
                .into_inner();
        decode(response)
    }
}

// A wire status has no local transport source. Preserve server rejections;
// only classify a broken local HTTP connection as unavailable.
fn submission_code(status: &tonic::Status) -> Code {
    let mut source = std::error::Error::source(status);
    while let Some(error) = source {
        if error.is::<hyper::Error>() {
            return Code::Unavailable;
        }
        source = error.source();
    }
    status.code()
}

fn decode(response: SubmitSandboxJobResponseV1) -> Result<SandboxOutcome, SandboxCallError> {
    if response.result_json.len() > 512 * 1024
        || response.failure_code.len() > 128
        || response.failure_code.chars().any(char::is_control)
    {
        return Err(SandboxCallError::InvalidReceipt);
    }
    match SandboxJobStatusV1::try_from(response.status)
        .map_err(|_| SandboxCallError::InvalidReceipt)?
    {
        SandboxJobStatusV1::Completed if response.failure_code.is_empty() => {
            let value: serde_json::Value = serde_json::from_slice(&response.result_json)
                .map_err(|_| SandboxCallError::InvalidReceipt)?;
            if value["revision"] != 1
                || value["status"] != "completed"
                || value["exit_code"] != 0
                || !value["stdout"].is_string()
                || !value["stderr"].is_string()
            {
                return Err(SandboxCallError::InvalidReceipt);
            }
            Ok(SandboxOutcome::Completed(response.result_json))
        }
        SandboxJobStatusV1::Pending
            if response.result_json.is_empty()
                && response.failure_code.is_empty()
                && !response.cleanup_pending =>
        {
            Ok(SandboxOutcome::Pending)
        }
        SandboxJobStatusV1::Failed
            if response.result_json.is_empty() && !response.failure_code.is_empty() =>
        {
            Ok(SandboxOutcome::Failed {
                code: response.failure_code,
            })
        }
        SandboxJobStatusV1::Cancelled if response.result_json.is_empty() => {
            Ok(SandboxOutcome::Cancelled)
        }
        SandboxJobStatusV1::Uncertain if response.result_json.is_empty() => {
            Ok(SandboxOutcome::Uncertain {
                code: response.failure_code,
            })
        }
        _ => Err(SandboxCallError::InvalidReceipt),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn supervisor_rejections_keep_their_status() {
        for code in [
            Code::Unknown,
            Code::Internal,
            Code::Cancelled,
            Code::Unauthenticated,
            Code::PermissionDenied,
            Code::FailedPrecondition,
            Code::DataLoss,
            Code::Aborted,
        ] {
            assert_eq!(submission_code(&tonic::Status::new(code, "rejected")), code);
        }
    }

    #[test]
    fn terminal_receipts_cannot_be_confused_with_pending_or_success() {
        let receipt =
            |status: SandboxJobStatusV1, result: &[u8], code: &str| SubmitSandboxJobResponseV1 {
                status: status.into(),
                result_json: result.to_vec(),
                failure_code: code.into(),
                cleanup_pending: false,
            };
        assert!(matches!(
            decode(receipt(
                SandboxJobStatusV1::Completed,
                br#"{"revision":1,"status":"completed","exit_code":0,"stdout":"{}","stderr":""}"#,
                ""
            ))
            .unwrap(),
            SandboxOutcome::Completed(_)
        ));
        assert!(matches!(
            decode(receipt(SandboxJobStatusV1::Failed, b"", "runtime_failed")).unwrap(),
            SandboxOutcome::Failed { .. }
        ));
        for invalid in [
            receipt(SandboxJobStatusV1::Completed, b"partial", ""),
            receipt(SandboxJobStatusV1::Pending, b"{}", ""),
            receipt(SandboxJobStatusV1::Completed, b"{}", "failed"),
            receipt(SandboxJobStatusV1::Unspecified, b"", ""),
        ] {
            assert!(decode(invalid).is_err());
        }
    }
}
