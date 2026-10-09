//! Binary debug transfer reuses the original private mTLS context channel.
use super::*;
use crate::agents::graph::{
    CodeDebugAdmission, CodeDebugArtifactReference, CodeDebugArtifactSink, CodeDebugFailure,
};
use crate::protocol::control::ClaimBoundSandboxAuthority;
use async_trait::async_trait;
#[async_trait]
impl CodeDebugArtifactSink for RuntimeContextClient {
    async fn export(
        &self,
        authority: &ClaimBoundSandboxAuthority,
        intent: &CodeDebugAdmission,
        snapshot: &[u8],
    ) -> Result<CodeDebugArtifactReference, CodeDebugFailure> {
        let binding = authority.debug_content_binding();
        validate_binding(&binding).map_err(|_| CodeDebugFailure::Denied)?;
        let admission = serde_json::to_vec(intent).map_err(|_| CodeDebugFailure::Unavailable)?;
        if admission.len() > 3 * 1024 * 1024 || snapshot.len() > 3 * 1024 * 1024 {
            return Err(CodeDebugFailure::Unavailable);
        }
        let request = debug_request(&binding, "admit", admission)?;
        let response = self
            .rpc
            .post(request)
            .await
            .map_err(|_| CodeDebugFailure::Unavailable)?;
        if response.status() != StatusCode::NO_CONTENT {
            return Err(debug_failure(response.status()));
        }
        let request = debug_request(
            &binding,
            &format!("commit/{}", intent.original_visit.visit_id),
            snapshot.to_vec(),
        )?;
        let response = self
            .rpc
            .post(request)
            .await
            .map_err(|_| CodeDebugFailure::Unavailable)?;
        if response.status() != StatusCode::OK {
            return Err(debug_failure(response.status()));
        }
        let raw_length = response
            .headers()
            .get(CONTENT_LENGTH)
            .and_then(|v| v.to_str().ok())
            .and_then(|v| v.parse::<usize>().ok())
            .filter(|n| *n > 0 && *n <= 4096)
            .ok_or(CodeDebugFailure::Unavailable)?;
        if response
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|v| v.to_str().ok())
            != Some("application/json")
        {
            return Err(CodeDebugFailure::Unavailable);
        }
        let body = collect_body(response, raw_length, 4096)
            .await
            .map_err(|_| CodeDebugFailure::Unavailable)?;
        serde_json::from_slice(&body).map_err(|_| CodeDebugFailure::Unavailable)
    }
}
fn debug_failure(status: StatusCode) -> CodeDebugFailure {
    if matches!(
        status,
        StatusCode::FORBIDDEN
            | StatusCode::UNAUTHORIZED
            | StatusCode::NOT_FOUND
            | StatusCode::UNPROCESSABLE_ENTITY
    ) {
        CodeDebugFailure::Denied
    } else {
        CodeDebugFailure::Unavailable
    }
}
fn debug_request(
    binding: &RuntimeContextRedemptionBinding<'_>,
    operation: &str,
    raw: Vec<u8>,
) -> Result<Request<Body>, CodeDebugFailure> {
    let execution = utf8_percent_encode(binding.execution_id, PATH_SEGMENT);
    let path = format!(
        "/executions/{execution}/generations/{}/code-debug/{operation}",
        binding.generation
    );
    let mut request = Request::builder()
        .method(Method::POST)
        .uri(path)
        .header(CONTENT_TYPE, "application/json")
        .header(CONTENT_LENGTH, raw.len())
        .header(CACHE_CONTROL, "no-store")
        .body(Body::new(http_body_util::Full::new(bytes::Bytes::from(
            raw,
        ))))
        .map_err(|_| CodeDebugFailure::Unavailable)?;
    insert_claim_headers(&mut request, binding).map_err(|_| CodeDebugFailure::Denied)?;
    Ok(request)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[tokio::test]
    async fn binary_request_keeps_exact_bytes_and_original_claim_headers() {
        let fence = [5_u8; 32];
        let binding = RuntimeContextRedemptionBinding {
            execution_id: "execution-one",
            generation: 3,
            claim_id: "claim-1",
            fence_token: &fence,
            resource_project_id: "7",
        };
        let bytes =
            br#"{"source":"line\r\n","selected_input":{"large":9007199254740993}}"#.to_vec();
        let request = debug_request(&binding, "admit", bytes.clone()).unwrap();
        assert_eq!(
            request.uri().path(),
            "/executions/execution-one/generations/3/code-debug/admit"
        );
        assert_eq!(request.headers()["x-elitea-claim-id"], "claim-1");
        assert_eq!(request.headers()[CONTENT_LENGTH], bytes.len().to_string());
        assert!(!request.headers().contains_key("authorization"));
        assert_eq!(
            request
                .into_body()
                .collect()
                .await
                .unwrap()
                .to_bytes()
                .as_ref(),
            bytes
        );
    }
    #[test]
    fn denied_statuses_do_not_become_artifact_references() {
        for status in [
            StatusCode::FORBIDDEN,
            StatusCode::UNAUTHORIZED,
            StatusCode::NOT_FOUND,
            StatusCode::UNPROCESSABLE_ENTITY,
        ] {
            assert_eq!(debug_failure(status), CodeDebugFailure::Denied);
        }
        for status in [
            StatusCode::TOO_MANY_REQUESTS,
            StatusCode::INTERNAL_SERVER_ERROR,
            StatusCode::BAD_GATEWAY,
        ] {
            assert_eq!(debug_failure(status), CodeDebugFailure::Unavailable);
        }
    }
}
