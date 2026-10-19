//! One-attempt mTLS fan-out decision fetch and ACK (contract
//! `libs/proto/contracts/fanout-interrupt-decisions-v1.md` §7).
//!
//! Same transport as node recovery control: the private content listener,
//! `X-Elitea-Claim-Id` and `X-Elitea-Fence`, HTTP/2, one bounded attempt, no
//! redirects. Every fetched decision's `decision_sha256` is recomputed over
//! the contract canonical form before the runner sees it, and the ACK echoes
//! it. Nothing here logs or formats a decision value or credential reference.

use super::{
    Body, CLAIM_HEADER, CONTENT_LENGTH, CONTENT_TYPE, Duration, FENCE_HEADER, InputContentClient,
    InputContentError, Method, PATH_SEGMENT, Request, StatusCode, Version, timeout,
    utf8_percent_encode,
};
use base64::Engine as _;
use http_body_util::BodyExt as _;
use ring::digest;
use serde::{Deserialize, Serialize};
use zeroize::Zeroizing;

use elitea_agent_runtime::graph::fanout_budget::{
    MAX_DECISION_VALUE_CHARS, MAX_FETCH_DECISIONS, MAX_FETCH_ENTRY_BYTES,
};

const FETCH_SCHEMA: &str = "elitea.pipeline.fanout-interrupt-fetch.v1";
const ACK_RESULT_SCHEMA: &str = "elitea.pipeline.fanout-interrupt-ack-result.v1";
/// The largest valid fetch reply: 16 entries at their bound plus the envelope.
const MAX_FETCH_REPLY_BYTES: usize = MAX_FETCH_DECISIONS * MAX_FETCH_ENTRY_BYTES + 4096;
/// Main's `MaxAckBodyBytes`; a reply is smaller still.
const MAX_ACK_BYTES: usize = 1024;
const ACTIONS: [&str; 8] = [
    "approve",
    "reject",
    "edit",
    "block_with_comment",
    "answer",
    "authorize",
    "skip",
    "continue",
];

/// The claim this execution holds, as the fetch/ACK routes require it.
/// Built only from the accepted claim (composition); never from a frame.
pub(crate) struct FanoutDecisionAuthority {
    execution_id: String,
    generation: u64,
    claim_id: String,
    fence: Zeroizing<Vec<u8>>,
}

impl std::fmt::Debug for FanoutDecisionAuthority {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("FanoutDecisionAuthority")
            .field("generation", &self.generation)
            .finish_non_exhaustive()
    }
}

impl FanoutDecisionAuthority {
    pub(crate) fn new(
        execution_id: &str,
        generation: u64,
        claim_id: &str,
        fence: &[u8],
    ) -> Result<Self, InputContentError> {
        if !lower_hex(execution_id, 32)
            || generation == 0
            || i64::try_from(generation).is_err()
            || claim_id.is_empty()
            || claim_id.len() > 256
            || !claim_id.bytes().all(|byte| byte.is_ascii_graphic())
            || fence.len() != 32
        {
            return Err(InputContentError::InvalidConfiguration(
                "the fan-out decision authority is invalid",
            ));
        }
        Ok(Self {
            execution_id: execution_id.to_owned(),
            generation,
            claim_id: claim_id.to_owned(),
            fence: Zeroizing::new(fence.to_vec()),
        })
    }
}

/// One decision as Main returned it.
#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct FetchedDecisionWire {
    pub(crate) interrupt_key: String,
    pub(crate) interrupt_id: String,
    pub(crate) revision: u64,
    pub(crate) request_id: String,
    pub(crate) action: String,
    pub(crate) value: String,
    #[serde(deserialize_with = "required_nullable")]
    pub(crate) credential_ref: Option<String>,
    pub(crate) decision_sha256: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct FanoutFetchReply {
    schema: String,
    execution_id: String,
    generation: u64,
    pub(crate) decision_revision: u64,
    pub(crate) decisions: Vec<FetchedDecisionWire>,
}

/// The ACK body. Fields in UTF-8 key order, so the bytes are canonical and a
/// retried ACK is byte-identical (Main answers it with `replay: true`).
#[derive(Serialize)]
pub(crate) struct FanoutAckRequest<'a> {
    pub(crate) child_checkpoint_id: Option<&'a str>,
    pub(crate) decision_sha256: &'a str,
    pub(crate) interrupt_key: &'a str,
    pub(crate) outcome: &'static str,
    pub(crate) request_id: &'a str,
    pub(crate) revision: u64,
}

/// What Main did with one ACK.
pub(crate) enum FanoutAckOutcome {
    Recorded(
        #[allow(
            dead_code,
            reason = "validated on receipt; the runner needs only the outcome"
        )]
        FanoutAckReply,
    ),
    /// 409: the card was already resolved with different ACK bytes.
    AlreadyResolved,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct FanoutAckReply {
    schema: String,
    interrupt_key: String,
    pub(crate) state: String,
    pub(crate) revision: u64,
    // A replay grants nothing beyond the original ACK; it is not inspected.
    #[serde(rename = "replay")]
    _replay: bool,
}

/// The canonical digest input (contract §7): keys in UTF-8 byte order.
#[derive(Serialize)]
struct DecisionDigest<'a> {
    action: &'a str,
    credential_ref: Option<&'a str>,
    interrupt_key: &'a str,
    request_id: &'a str,
    revision: u64,
    value: &'a str,
}

/// `decision_sha256` over the contract canonical JSON. `serde_json` without
/// `preserve_order` writes exactly that form for a struct in key order.
pub(crate) fn decision_sha256(decision: &FetchedDecisionWire) -> Result<String, InputContentError> {
    let bytes = serde_json::to_vec(&DecisionDigest {
        action: &decision.action,
        credential_ref: decision.credential_ref.as_deref(),
        interrupt_key: &decision.interrupt_key,
        request_id: &decision.request_id,
        revision: decision.revision,
        value: &decision.value,
    })
    .map_err(|_| invalid())?;
    Ok(hex(digest::digest(&digest::SHA256, &bytes).as_ref()))
}

impl InputContentClient {
    /// Fetch every decided card of this execution's root response.
    pub(crate) async fn fetch_fanout_decisions(
        &self,
        authority: &FanoutDecisionAuthority,
    ) -> Result<FanoutFetchReply, InputContentError> {
        let bytes = self
            .fanout_post(authority, "fetch", Vec::new(), MAX_FETCH_REPLY_BYTES, false)
            .await?
            .ok_or_else(invalid)?;
        let reply: FanoutFetchReply = serde_json::from_slice(&bytes).map_err(|_| invalid())?;
        if reply.schema != FETCH_SCHEMA
            || reply.execution_id != authority.execution_id
            || reply.generation != authority.generation
            || i64::try_from(reply.decision_revision).is_err()
            || reply.decisions.len() > MAX_FETCH_DECISIONS
        {
            return Err(invalid());
        }
        for decision in &reply.decisions {
            validate_decision(decision)?;
        }
        Ok(reply)
    }

    /// ACK one decision: `applied` with the child checkpoint the resumed child
    /// saved, or `stale`.
    pub(crate) async fn ack_fanout_decision(
        &self,
        authority: &FanoutDecisionAuthority,
        request: &FanoutAckRequest<'_>,
    ) -> Result<FanoutAckOutcome, InputContentError> {
        let checkpoint_ok = match (request.outcome, request.child_checkpoint_id) {
            ("applied", Some(checkpoint)) => {
                !checkpoint.is_empty()
                    && checkpoint.len() <= 128
                    && checkpoint
                        .bytes()
                        .all(|byte| byte.is_ascii_alphanumeric() || b"_.:-".contains(&byte))
            }
            ("stale", None) => true,
            _ => false,
        };
        if !checkpoint_ok
            || !nonzero_hex(request.interrupt_key)
            || !nonzero_hex(request.request_id)
            || !nonzero_hex(request.decision_sha256)
            || request.revision < 2
            || i64::try_from(request.revision).is_err()
        {
            return Err(invalid());
        }
        let payload = serde_json::to_vec(request).map_err(|_| invalid())?;
        let Some(bytes) = self
            .fanout_post(authority, "ack", payload, MAX_ACK_BYTES, true)
            .await?
        else {
            return Ok(FanoutAckOutcome::AlreadyResolved);
        };
        let reply: FanoutAckReply = serde_json::from_slice(&bytes).map_err(|_| invalid())?;
        let expected = if request.outcome == "applied" {
            "CONSUMED"
        } else {
            "SUPERSEDED"
        };
        if reply.schema != ACK_RESULT_SCHEMA
            || reply.interrupt_key != request.interrupt_key
            || reply.state != expected
            || reply.revision == 0
        {
            return Err(invalid());
        }
        Ok(FanoutAckOutcome::Recorded(reply))
    }

    /// `Ok(None)` only for a conflicting ACK (409), which Main already resolved.
    async fn fanout_post(
        &self,
        authority: &FanoutDecisionAuthority,
        operation: &str,
        payload: Vec<u8>,
        limit: usize,
        is_ack: bool,
    ) -> Result<Option<Vec<u8>>, InputContentError> {
        let uri = format!(
            "/executions/{}/generations/{}/interrupts/{operation}",
            utf8_percent_encode(&authority.execution_id, PATH_SEGMENT),
            authority.generation
        );
        let request = Request::builder()
            .method(Method::POST)
            .version(Version::HTTP_2)
            .uri(uri)
            .header(CLAIM_HEADER, authority.claim_id.as_str())
            .header(
                FENCE_HEADER,
                base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(authority.fence.as_slice()),
            )
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
            match response.status() {
                StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN => {
                    return Err(InputContentError::AuthorizationFailed(
                        "the fan-out decision authority is no longer current",
                    ));
                }
                // A fetch refused for its fence is a lost claim. A conflicting
                // ACK means Main already resolved the card: nothing to retry.
                StatusCode::CONFLICT if !is_ack => {
                    return Err(InputContentError::AuthorizationFailed(
                        "the fan-out decision authority is no longer current",
                    ));
                }
                StatusCode::CONFLICT => return Ok(None),
                StatusCode::OK => {}
                _ => {
                    return Err(InputContentError::DependencyUnavailable(
                        "the fan-out decision route is unavailable",
                    ));
                }
            }
            if response.version() != Version::HTTP_2
                || response
                    .headers()
                    .get(CONTENT_TYPE)
                    .and_then(|header| header.to_str().ok())
                    .is_none_or(|header| header.split(';').next() != Some("application/json"))
            {
                return Err(invalid());
            }
            let mut result = Vec::new();
            while let Some(frame) = response.body_mut().frame().await {
                let frame = frame.map_err(|_| {
                    InputContentError::DependencyUnavailable(
                        "the fan-out decision response was interrupted",
                    )
                })?;
                if frame.is_trailers() {
                    return Err(invalid());
                }
                if let Ok(bytes) = frame.into_data() {
                    if result
                        .len()
                        .checked_add(bytes.len())
                        .is_none_or(|length| length > limit)
                    {
                        return Err(InputContentError::ResourceExhausted(
                            "the fan-out decision response exceeds its bound",
                        ));
                    }
                    result.extend_from_slice(&bytes);
                }
            }
            if result.is_empty() {
                return Err(invalid());
            }
            Ok(Some(result))
        })
        .await
        .map_err(|_| InputContentError::Timeout("the fan-out decision request timed out"))?
    }
}

fn validate_decision(decision: &FetchedDecisionWire) -> Result<(), InputContentError> {
    let printable = |text: &str, max: usize| {
        !text.is_empty()
            && text.len() <= max
            && text.bytes().all(|byte| (0x20..=0x7e).contains(&byte))
            && !text.starts_with(' ')
    };
    let value_ok = decision.value.chars().count() <= MAX_DECISION_VALUE_CHARS
        && !decision
            .value
            .chars()
            .any(|ch| matches!(ch, '\u{0}'..='\u{8}' | '\u{b}' | '\u{c}' | '\u{e}'..='\u{1f}' | '\u{7f}'));
    let credential_ok = decision.credential_ref.as_deref().is_none_or(|reference| {
        (16..=128).contains(&reference.len())
            && reference
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
    });
    if !nonzero_hex(&decision.interrupt_key)
        || !nonzero_hex(&decision.request_id)
        || !nonzero_hex(&decision.decision_sha256)
        || !printable(&decision.interrupt_id, 512)
        || decision.revision < 2
        || i64::try_from(decision.revision).is_err()
        || !ACTIONS.contains(&decision.action.as_str())
        || !value_ok
        || !credential_ok
        // A credential reference travels only with an authorize decision.
        || (decision.action == "authorize") != decision.credential_ref.is_some()
    {
        return Err(invalid());
    }
    let canonical_entry = serde_json::to_vec(&serde_json::json!({
        "action": decision.action,
        "credential_ref": decision.credential_ref,
        "decision_sha256": decision.decision_sha256,
        "interrupt_id": decision.interrupt_id,
        "interrupt_key": decision.interrupt_key,
        "request_id": decision.request_id,
        "revision": decision.revision,
        "value": decision.value,
    }))
    .map_err(|_| invalid())?;
    if canonical_entry.len() > MAX_FETCH_ENTRY_BYTES {
        return Err(InputContentError::ResourceExhausted(
            "a fetched fan-out decision exceeds its bound",
        ));
    }
    if decision_sha256(decision)? != decision.decision_sha256 {
        return Err(invalid());
    }
    Ok(())
}

fn required_nullable<'de, D, T>(deserializer: D) -> Result<Option<T>, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Deserialize<'de>,
{
    Option::<T>::deserialize(deserializer)
}

fn lower_hex(value: &str, length: usize) -> bool {
    value.len() == length
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

fn nonzero_hex(value: &str) -> bool {
    lower_hex(value, 64) && value.bytes().any(|byte| byte != b'0')
}

fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    bytes
        .iter()
        .fold(String::with_capacity(64), |mut out, byte| {
            let _ = write!(out, "{byte:02x}");
            out
        })
}

fn invalid() -> InputContentError {
    InputContentError::InvalidInput("the fan-out decision binding is invalid")
}

#[cfg(test)]
#[path = "fanout_interrupts_content_tests.rs"]
mod tests;
