//! One claim-scoped Main broker step. No Code frames or runtime selectors cross here.
use super::{
    Body, CLAIM_HEADER, CONTENT_LENGTH, CONTENT_TYPE, Duration, FENCE_HEADER, InputContentClient,
    InputContentError, Method, PATH_SEGMENT, Request, StatusCode, Version, timeout,
    utf8_percent_encode,
};
use crate::protocol::control::ClaimBoundSandboxAuthority;
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use http_body_util::BodyExt as _;
use serde::{Deserialize, Serialize};

#[derive(Serialize)]
struct StepRequest {
    schema: &'static str,
    revision: u8,
    dispatch_activation: String,
    prepared_request_fingerprint: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct StepResponse {
    schema: String,
    revision: u8,
    disposition: String,
    // Required nullable field: Option alone would also admit an omitted field.
    effect_id: serde_json::Value,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum CodePlatformStep {
    Idle,
    Committed { call_effect: [u8; 32] },
    Unknown { call_effect: [u8; 32] },
}
impl InputContentClient {
    pub(crate) async fn step_code_platform(
        &self,
        claim: &ClaimBoundSandboxAuthority,
        dispatch: [u8; 32],
        prepared_fingerprint: [u8; 32],
    ) -> Result<CodePlatformStep, InputContentError> {
        if dispatch == [0; 32] || prepared_fingerprint == [0; 32] {
            return Err(invalid());
        }
        let payload = serde_json::to_vec(&StepRequest {
            schema: "elitea.runtime.code-platform-step-request.v1",
            revision: 1,
            dispatch_activation: crate::sandbox::code_recovery::hex(&dispatch),
            prepared_request_fingerprint: crate::sandbox::code_recovery::hex(&prepared_fingerprint),
        })
        .map_err(|_| invalid())?;
        let (execution, generation, claim_id, fence) = claim.intent_content_binding();
        let request = Request::builder()
            .method(Method::POST)
            .version(Version::HTTP_2)
            .uri(format!(
                "{}/executions/{}/generations/{generation}/code-platform/step",
                self.config.origin,
                utf8_percent_encode(execution, PATH_SEGMENT)
            ))
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
                    "the original Code broker step was refused",
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
            let mut raw = Vec::new();
            while let Some(frame) = response.body_mut().frame().await {
                let frame = frame.map_err(|_| unavailable())?;
                if frame.is_trailers() {
                    return Err(invalid());
                }
                if let Ok(bytes) = frame.into_data() {
                    if raw.len().checked_add(bytes.len()).is_none_or(|n| n > 512) {
                        return Err(invalid());
                    }
                    raw.extend_from_slice(&bytes);
                }
            }
            parse_step(&raw)
        })
        .await
        .map_err(|_| unavailable())?
    }
}
fn parse_step(raw: &[u8]) -> Result<CodePlatformStep, InputContentError> {
    if raw.is_empty() || raw.len() > 512 {
        return Err(invalid());
    }
    // Derived struct decoding rejects duplicate known fields and unknown fields.
    let response: StepResponse = serde_json::from_slice(raw).map_err(|_| invalid())?;
    if response.schema != "elitea.runtime.code-platform-step-response.v1" || response.revision != 1
    {
        return Err(invalid());
    }
    if response.disposition == "idle" && response.effect_id.is_null() {
        return Ok(CodePlatformStep::Idle);
    }
    let text = response.effect_id.as_str().ok_or_else(invalid)?;
    if text.len() != 64
        || !text
            .bytes()
            .all(|v| v.is_ascii_digit() || (b'a'..=b'f').contains(&v))
    {
        return Err(invalid());
    }
    let mut digest = [0; 32];
    for (index, chunk) in text.as_bytes().chunks_exact(2).enumerate() {
        let number = |v: u8| {
            if v.is_ascii_digit() {
                v - b'0'
            } else {
                v - b'a' + 10
            }
        };
        digest[index] = (number(chunk[0]) << 4) | number(chunk[1]);
    }
    if digest == [0; 32] {
        return Err(invalid());
    }
    match response.disposition.as_str() {
        "committed" => Ok(CodePlatformStep::Committed {
            call_effect: digest,
        }),
        "unknown" => Ok(CodePlatformStep::Unknown {
            call_effect: digest,
        }),
        _ => Err(invalid()),
    }
}
fn invalid() -> InputContentError {
    InputContentError::AuthorizationFailed("the original Code broker observation is invalid")
}
fn unavailable() -> InputContentError {
    InputContentError::DependencyUnavailable("the original Code broker observation was interrupted")
}

#[cfg(test)]
mod tests {
    use super::*;
    fn response(state: &str, effect: &str) -> Vec<u8> {
        format!(r#"{{"schema":"elitea.runtime.code-platform-step-response.v1","revision":1,"disposition":"{state}","effect_id":{effect}}}"#).into_bytes()
    }
    #[test]
    fn typed_response_keeps_required_null_and_closed_effect_identity() {
        assert_eq!(
            parse_step(&response("idle", "null")).unwrap(),
            CodePlatformStep::Idle
        );
        let effect = format!("\"{}\"", "ab".repeat(32));
        assert_eq!(
            parse_step(&response("unknown", &effect)).unwrap(),
            CodePlatformStep::Unknown {
                call_effect: [0xab; 32]
            }
        );
        assert!(parse_step(&response("idle", &effect)).is_err());
        assert!(parse_step(&response("committed", "null")).is_err());
        assert!(parse_step(&response("unknown", &format!("\"{}\"", "00".repeat(32)))).is_err());
        assert!(parse_step(br#"{"schema":"elitea.runtime.code-platform-step-response.v1","revision":1,"disposition":"idle"}"#).is_err());
        assert!(parse_step(br#"{"schema":"elitea.runtime.code-platform-step-response.v1","revision":1,"revision":1,"disposition":"idle","effect_id":null}"#).is_err());
        assert!(parse_step(&[b' '; 513]).is_err());
    }
}
