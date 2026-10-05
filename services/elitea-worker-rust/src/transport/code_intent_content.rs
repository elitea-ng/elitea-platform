//! Two-stage original Code admission on the existing current-claim mTLS channel.
use super::{
    Body, CLAIM_HEADER, CONTENT_LENGTH, CONTENT_TYPE, Duration, FENCE_HEADER, InputContentClient,
    InputContentError, Method, PATH_SEGMENT, Request, StatusCode, Version, timeout,
    utf8_percent_encode,
};
use crate::{
    protocol::control::ClaimBoundSandboxAuthority,
    sandbox::{
        code_recovery::{OriginalCodeVisitRef, SignedCodeEnvelope, hex, sha256},
        request::PreparedJob,
    },
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use http_body_util::BodyExt as _;
use serde::{Deserialize, Serialize};

#[derive(Serialize)]
#[serde(deny_unknown_fields)]
struct VisitRequest<'a> {
    schema: &'static str,
    activation_id: String,
    node_id: &'a str,
    graph_thread: &'a str,
    step: u64,
    attempt: u16,
    node_digest: String,
    owning_yaml_sha256: String,
    exact_configuration_json_base64url: String,
    pre_workspace_prepared_job_json_base64url: String,
    saved_child_scope: Option<&'a crate::agents::pipeline::saved_child_http::SavedChildScopeRef>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct VisitResponse {
    schema: String,
    original_visit: OriginalCodeVisitRef,
    execution_id: String,
    original_generation: u64,
    activation_id: String,
    attempt: u16,
    node_digest: String,
    pre_workspace_prepared_sha256: String,
}
#[derive(Serialize)]
#[serde(deny_unknown_fields)]
struct IntentRequest<'a> {
    schema: &'static str,
    original_visit: &'a OriginalCodeVisitRef,
    dispatch_activation: String,
    request_digest: String,
    supervisor_audience: &'a str,
    prepared_job_json_base64url: String,
    compiled_binding_json_base64url: Option<String>,
    selected_descriptor_sha256: Option<&'a str>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct IntentResponse {
    schema: String,
    intent: SignedCodeEnvelope,
}
impl InputContentClient {
    #[cfg(test)]
    pub(crate) fn with_original_code_visit_fixture_rpc(
        rpc: impl super::InputContentRpc + 'static,
    ) -> Self {
        Self::with_rpc(
            rpc,
            super::InputContentConfig {
                origin: "https://main.original-code.fixture".into(),
                deadline: Duration::from_secs(1),
                max_materialized_bytes: 1024 * 1024,
            },
        )
        .unwrap()
    }
    pub(crate) async fn admit_original_code_visit(
        &self,
        claim: &ClaimBoundSandboxAuthority,
        declaration: &crate::sandbox::code_recovery::OriginalCodeDeclarationInput<'_>,
        authority: &crate::agents::graph::node_recovery_runtime::NodeAttemptAuthority,
        pre_job: &PreparedJob,
        saved_child_scope: Option<&crate::agents::pipeline::saved_child_http::SavedChildScopeRef>,
    ) -> Result<OriginalCodeVisitRef, InputContentError> {
        let pre = pre_job.to_transport().map_err(|_| invalid())?;
        let request = original_visit_request(declaration, authority, &pre, saved_child_scope)?;
        let bytes = self
            .code_intent_post(
                claim,
                "visits",
                serde_json::to_vec(&request).map_err(|_| invalid())?,
            )
            .await?;
        let response: VisitResponse = serde_json::from_slice(&bytes).map_err(|_| invalid())?;
        let (execution, generation, _, _) = claim.intent_content_binding();
        if response.schema != "elitea.sandbox.original-code-visit-response.v1"
            || !response.original_visit.valid()
            || response.execution_id != execution
            || response.original_generation != generation
            || response.activation_id != request.activation_id
            || response.attempt != request.attempt
            || response.node_digest != request.node_digest
            || response.pre_workspace_prepared_sha256 != sha256(&pre)
        {
            return Err(invalid());
        }
        Ok(response.original_visit)
    }
    #[allow(
        clippy::too_many_arguments,
        reason = "Keep the existing authenticated identity fields explicit."
    )]
    pub(crate) async fn finalize_original_code_intent(
        &self,
        claim: &ClaimBoundSandboxAuthority,
        visit: &OriginalCodeVisitRef,
        dispatch: [u8; 32],
        request_digest: [u8; 32],
        audience: &str,
        job: &PreparedJob,
        binding: Option<&crate::sandbox::compiled_snapshot::Binding>,
        descriptor: Option<&str>,
    ) -> Result<Vec<u8>, InputContentError> {
        if !visit.valid() || binding.is_some() != descriptor.is_some() {
            return Err(invalid());
        }
        let request = IntentRequest {
            schema: "elitea.sandbox.original-code-intent-request.v1",
            original_visit: visit,
            dispatch_activation: hex(&dispatch),
            request_digest: hex(&request_digest),
            supervisor_audience: audience,
            prepared_job_json_base64url: URL_SAFE_NO_PAD
                .encode(job.to_transport().map_err(|_| invalid())?),
            compiled_binding_json_base64url: binding
                .map(|binding| {
                    serde_json::to_vec(binding).map(|bytes| URL_SAFE_NO_PAD.encode(bytes))
                })
                .transpose()
                .map_err(|_| invalid())?,
            selected_descriptor_sha256: descriptor,
        };
        let bytes = self
            .code_intent_post(
                claim,
                "intents",
                serde_json::to_vec(&request).map_err(|_| invalid())?,
            )
            .await?;
        let response: IntentResponse = serde_json::from_slice(&bytes).map_err(|_| invalid())?;
        if response.schema != "elitea.sandbox.original-code-intent-response.v1" {
            return Err(invalid());
        }
        let envelope = response.intent;
        if envelope.schema != "elitea.sandbox.original-code-intent-signed.v1"
            || envelope.key_id.is_empty()
            || URL_SAFE_NO_PAD
                .decode(&envelope.claims_base64url)
                .map_err(|_| invalid())?
                .len()
                > 8192
            || URL_SAFE_NO_PAD
                .decode(&envelope.signature_base64url)
                .map_err(|_| invalid())?
                .len()
                != 64
        {
            return Err(invalid());
        }
        // Supervisor verifies the independently signed exact claims against the actual Execute grant/job.
        serde_json::to_vec(&envelope).map_err(|_| invalid())
    }
    async fn code_intent_post(
        &self,
        claim: &ClaimBoundSandboxAuthority,
        operation: &str,
        payload: Vec<u8>,
    ) -> Result<Vec<u8>, InputContentError> {
        if payload.len() > 5 * 1024 * 1024 {
            return Err(InputContentError::ResourceExhausted(
                "the original Code admission exceeds its bound",
            ));
        }
        let (execution, generation, claim_id, fence) = claim.intent_content_binding();
        let uri = format!(
            "{}/executions/{}/generations/{generation}/code-sandbox/{operation}",
            self.config.origin,
            utf8_percent_encode(execution, PATH_SEGMENT)
        );
        let request = Request::builder()
            .method(Method::POST)
            .version(Version::HTTP_2)
            .uri(uri)
            .header(CLAIM_HEADER, claim_id)
            .header(FENCE_HEADER, URL_SAFE_NO_PAD.encode(fence))
            .header(CONTENT_TYPE, "application/json")
            .header(CONTENT_LENGTH, payload.len())
            .body(Body::new(http_body_util::Full::new(bytes::Bytes::from(
                payload,
            ))))
            .map_err(|_| invalid())?;
        timeout(self.config.deadline.min(Duration::from_secs(10)), async {
            let mut response = self
                .rpc
                .get(request)
                .await
                .map_err(InputContentError::Transport)?;
            if matches!(
                response.status(),
                StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN | StatusCode::CONFLICT
            ) {
                return Err(InputContentError::AuthorizationFailed(
                    "the original Code intent was refused",
                ));
            }
            if response.status() != StatusCode::OK
                || response.version() != Version::HTTP_2
                || response
                    .headers()
                    .get(CONTENT_TYPE)
                    .and_then(|v| v.to_str().ok())
                    .is_none_or(|v| v.split(';').next() != Some("application/json"))
            {
                return Err(invalid());
            }
            let mut bytes = Vec::new();
            while let Some(frame) = response.body_mut().frame().await {
                let frame = frame.map_err(|_| {
                    InputContentError::DependencyUnavailable(
                        "the original Code intent response was interrupted",
                    )
                })?;
                if frame.is_trailers() {
                    return Err(invalid());
                }
                if let Ok(data) = frame.into_data() {
                    if bytes
                        .len()
                        .checked_add(data.len())
                        .is_none_or(|n| n > 16 * 1024)
                    {
                        return Err(invalid());
                    }
                    bytes.extend_from_slice(&data);
                }
            }
            Ok(bytes)
        })
        .await
        .map_err(|_| {
            InputContentError::DependencyUnavailable("the original Code intent response timed out")
        })?
    }
}
fn original_visit_request<'a>(
    declaration: &'a crate::sandbox::code_recovery::OriginalCodeDeclarationInput<'a>,
    authority: &crate::agents::graph::node_recovery_runtime::NodeAttemptAuthority,
    prepared: &[u8],
    saved_child_scope: Option<&'a crate::agents::pipeline::saved_child_http::SavedChildScopeRef>,
) -> Result<VisitRequest<'a>, InputContentError> {
    let step = usize::try_from(declaration.step).map_err(|_| invalid())?;
    let mut hash = ring::digest::Context::new(&ring::digest::SHA256);
    hash.update(b"elitea.graph.code.config.v1\0");
    hash.update(declaration.configuration_json.as_bytes());
    let digest: [u8; 32] = hash.finish().as_ref().try_into().map_err(|_| invalid())?;
    if !authority.matches(declaration.node_id, digest, step) {
        return Err(InputContentError::AuthorizationFailed(
            "the sealed Code attempt does not bind the original declaration",
        ));
    }
    if declaration.configuration_json.is_empty()
        || declaration.configuration_json.len() > 2 * 1024 * 1024
        || declaration.owning_yaml_sha256 == [0; 32]
        || prepared.is_empty()
        || prepared.len() > 1024 * 1024
        || !crate::sandbox::code_recovery::valid_original_visit(
            &hex(&authority.logical_activation()),
            declaration.node_id,
            declaration.graph_thread,
            declaration.step,
            authority.attempt(),
        )
    {
        return Err(invalid());
    }
    Ok(VisitRequest {
        schema: "elitea.sandbox.original-code-visit-request.v1",
        activation_id: hex(&authority.logical_activation()),
        node_id: declaration.node_id,
        graph_thread: declaration.graph_thread,
        step: declaration.step,
        attempt: authority.attempt(),
        node_digest: hex(&authority.node_digest()),
        owning_yaml_sha256: hex(&declaration.owning_yaml_sha256),
        exact_configuration_json_base64url: URL_SAFE_NO_PAD
            .encode(declaration.configuration_json.as_bytes()),
        pre_workspace_prepared_job_json_base64url: URL_SAFE_NO_PAD.encode(prepared),
        saved_child_scope,
    })
}

#[cfg(test)]
pub(crate) fn actual_original_code_visit_fixture(
    declaration: &crate::sandbox::code_recovery::OriginalCodeDeclarationInput<'_>,
    authority: &crate::agents::graph::node_recovery_runtime::NodeAttemptAuthority,
    job: &PreparedJob,
) -> Result<Vec<u8>, InputContentError> {
    serde_json::to_vec(&original_visit_request(
        declaration,
        authority,
        &job.to_transport().map_err(|_| invalid())?,
        None,
    )?)
    .map_err(|_| invalid())
}
fn invalid() -> InputContentError {
    InputContentError::AuthorizationFailed("the original Code binding is invalid")
}
