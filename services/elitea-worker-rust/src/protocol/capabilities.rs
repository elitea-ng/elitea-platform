//! Two-way capability negotiation for `ClaimCommandRequestV1` and
//! `ClaimReceiptV1` (zero-deployment-order rolling upgrades).
//!
//! All bounds and canonical capability names match
//! `services/elitea-main/internal/domain/runtime/capabilities.go`.

use std::collections::BTreeSet;

use super::control::ControlSemanticError;
use super::elitea::runtime::v1::{ClaimCommandRequestV1, ClaimReceiptV1};

/// Maximum number of capability entries carried in `worker_capabilities` or
/// `server_capabilities`.
pub const MAX_NEGOTIATED_CAPABILITIES: usize = 64;

/// Maximum ASCII byte length of a single domain-named capability identifier.
pub const MAX_CAPABILITY_NAME_BYTES: usize = 128;

// Worker -> Main capabilities matching ClaimCommandRequestV1 fields 16-21.
pub const CAP_AGENT_MODEL_CHECKPOINT_RECOVERY: &str = "runtime.agent_model_checkpoint_recovery.v1";
pub const CAP_NODE_RECOVERY: &str = "runtime.node_recovery.v1";
pub const CAP_GOVERNED_SECURITY_CONTEXT: &str = "runtime.governed_security_context.v1";
pub const CAP_FANOUT_INTERRUPT_DECISIONS: &str = "runtime.fanout_interrupt_decisions.v1";
pub const CAP_FANOUT_DELEGATED_CREDENTIALS: &str = "runtime.fanout_delegated_credentials.v1";
pub const CAP_IDLE_COMPACTION: &str = "runtime.idle_compaction.v1";

// Main -> Worker capabilities matching ClaimReceiptV1 fields 18-20.
pub const CAP_NEGOTIATION: &str = "runtime.capability_negotiation.v1";
pub const CAP_TOOLKIT_TYPED_REFUSALS: &str = "runtime.toolkit_typed_refusals.v1";
pub const CAP_PAUSED_WRITER: &str = "runtime.paused_writer.v1";

/// Validated, deduplicated, deterministically sorted runtime capability set.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct NegotiatedCapabilities {
    sorted: Vec<String>,
}

impl NegotiatedCapabilities {
    /// Validate a raw wire capability list fail-closed: at most
    /// [`MAX_NEGOTIATED_CAPABILITIES`] unique entries, each 1 to
    /// [`MAX_CAPABILITY_NAME_BYTES`] bytes of lowercase dot-separated ASCII
    /// domain syntax.
    ///
    /// # Errors
    ///
    /// Returns [`ControlSemanticError::InvalidInput`] on oversized, malformed,
    /// or duplicate capability lists.
    pub fn parse(raw: &[String]) -> Result<Self, ControlSemanticError> {
        if raw.len() > MAX_NEGOTIATED_CAPABILITIES {
            return Err(ControlSemanticError::InvalidInput(
                "the claim capabilities exceed the maximum allowed count",
            ));
        }
        if raw.is_empty() {
            return Ok(Self::default());
        }
        let mut unique = BTreeSet::new();
        for entry in raw {
            if !valid_capability_name(entry) {
                return Err(ControlSemanticError::InvalidInput(
                    "the claim capability name is malformed",
                ));
            }
            if !unique.insert(entry.clone()) {
                return Err(ControlSemanticError::InvalidInput(
                    "the claim capabilities contain a duplicate entry",
                ));
            }
        }
        Ok(Self {
            sorted: unique.into_iter().collect(),
        })
    }

    /// Parse `receipt.server_capabilities` strictly and merge legacy receipt
    /// capability fields (`toolkit_typed_refusals`, `paused_writer`) for
    /// mixed-version compatibility with older Main deployments.
    ///
    /// # Errors
    ///
    /// Returns [`ControlSemanticError::InvalidInput`] if
    /// `receipt.server_capabilities` violates bounds, syntax, or uniqueness.
    pub fn from_receipt(receipt: &ClaimReceiptV1) -> Result<Self, ControlSemanticError> {
        let mut parsed = Self::parse(&receipt.server_capabilities)?;
        if receipt.toolkit_typed_refusals {
            parsed.insert_known(CAP_TOOLKIT_TYPED_REFUSALS)?;
        }
        if receipt.paused_writer.is_some() {
            parsed.insert_known(CAP_PAUSED_WRITER)?;
        }
        Ok(parsed)
    }

    fn insert_known(&mut self, capability: &str) -> Result<(), ControlSemanticError> {
        if let Err(idx) = self
            .sorted
            .binary_search_by(|entry| entry.as_str().cmp(capability))
        {
            if self.sorted.len() >= MAX_NEGOTIATED_CAPABILITIES {
                return Err(ControlSemanticError::InvalidInput(
                    "the claim capabilities exceed the maximum allowed count",
                ));
            }
            self.sorted.insert(idx, capability.to_owned());
        }
        Ok(())
    }

    /// Report whether `capability` is supported by the peer.
    #[must_use]
    pub fn has(&self, capability: &str) -> bool {
        self.sorted
            .binary_search_by(|entry| entry.as_str().cmp(capability))
            .is_ok()
    }

    /// Enforce that every required capability is supported by the peer.
    ///
    /// # Errors
    ///
    /// Returns [`ControlSemanticError::UnsupportedCapability`] when any
    /// required capability is missing.
    pub fn require(&self, required: &[String]) -> Result<(), ControlSemanticError> {
        for capability in required {
            if !self.has(capability) {
                return Err(ControlSemanticError::UnsupportedCapability(
                    "the runtime control server lacks a required capability",
                ));
            }
        }
        Ok(())
    }

    /// Return the deterministically sorted capabilities.
    #[must_use]
    pub fn as_slice(&self) -> &[String] {
        &self.sorted
    }
}

/// Synchronize `request.worker_capabilities` deterministically from the
/// request's legacy boolean opt-in fields so both wire representations stay
/// identical during zero-deployment-order rolling upgrades.
pub fn sync_worker_capabilities(request: &mut ClaimCommandRequestV1) {
    let mut caps = Vec::with_capacity(6);
    if request.agent_model_checkpoint_recovery {
        caps.push(CAP_AGENT_MODEL_CHECKPOINT_RECOVERY.to_owned());
    }
    if request.fanout_delegated_credentials {
        caps.push(CAP_FANOUT_DELEGATED_CREDENTIALS.to_owned());
    }
    if request.fanout_interrupt_decisions {
        caps.push(CAP_FANOUT_INTERRUPT_DECISIONS.to_owned());
    }
    if request.governed_security_context_v1 {
        caps.push(CAP_GOVERNED_SECURITY_CONTEXT.to_owned());
    }
    if request.idle_compaction {
        caps.push(CAP_IDLE_COMPACTION.to_owned());
    }
    if request.node_recovery {
        caps.push(CAP_NODE_RECOVERY.to_owned());
    }
    request.worker_capabilities = caps;
}

/// Report whether `name` is a bounded lowercase ASCII domain-named capability
/// with at least two dot-separated segments (for example
/// `"runtime.idle_compaction.v1"`).
#[must_use]
pub fn valid_capability_name(name: &str) -> bool {
    let bytes = name.as_bytes();
    if bytes.is_empty() || bytes.len() > MAX_CAPABILITY_NAME_BYTES {
        return false;
    }
    let mut segments: usize = 0;
    let mut seg_start: usize = 0;
    for i in 0..=bytes.len() {
        if i < bytes.len() && bytes[i] != b'.' {
            let b = bytes[i];
            if !is_lower_alnum(b) && b != b'_' && b != b'-' {
                return false;
            }
            continue;
        }
        if i == seg_start {
            return false;
        }
        if !is_lower_alnum(bytes[seg_start]) || !is_lower_alnum(bytes[i - 1]) {
            return false;
        }
        segments += 1;
        seg_start = i + 1;
    }
    segments >= 2
}

const fn is_lower_alnum(b: u8) -> bool {
    b.is_ascii_lowercase() || b.is_ascii_digit()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_capabilities_enforces_bounds_syntax_and_uniqueness() {
        let exact: Vec<String> = (0..MAX_NEGOTIATED_CAPABILITIES)
            .map(|i| format!("runtime.cap_{i:02}.v1"))
            .collect();
        let parsed = NegotiatedCapabilities::parse(&exact).expect("64 valid entries");
        assert_eq!(parsed.as_slice().len(), MAX_NEGOTIATED_CAPABILITIES);
        assert!(parsed.has("runtime.cap_00.v1"));
        assert!(parsed.has("runtime.cap_63.v1"));

        let mut over = exact;
        over.push("runtime.cap_64.v1".to_owned());
        assert!(matches!(
            NegotiatedCapabilities::parse(&over),
            Err(ControlSemanticError::InvalidInput(_))
        ));

        let exact_name = format!(
            "runtime.{}.v1",
            "a".repeat(MAX_CAPABILITY_NAME_BYTES - "runtime.".len() - ".v1".len())
        );
        assert_eq!(exact_name.len(), MAX_CAPABILITY_NAME_BYTES);
        assert!(NegotiatedCapabilities::parse(std::slice::from_ref(&exact_name)).is_ok());
        assert!(matches!(
            NegotiatedCapabilities::parse(&[format!("{exact_name}a")]),
            Err(ControlSemanticError::InvalidInput(_))
        ));

        for bad in [
            "",
            "single",
            "runtime.Upper.v1",
            ".runtime.cap.v1",
            "runtime.cap.v1.",
            "runtime..cap.v1",
            "runtime.-cap.v1",
            "runtime.cap_.v1",
            "runtime/cap.v1",
            "runtime.cap.v1\r\n",
        ] {
            assert!(
                matches!(
                    NegotiatedCapabilities::parse(&[bad.to_owned()]),
                    Err(ControlSemanticError::InvalidInput(_))
                ),
                "expected invalid capability rejection for {bad:?}"
            );
        }

        assert!(matches!(
            NegotiatedCapabilities::parse(&[
                CAP_IDLE_COMPACTION.to_owned(),
                CAP_IDLE_COMPACTION.to_owned()
            ]),
            Err(ControlSemanticError::InvalidInput(_))
        ));
    }

    #[test]
    fn from_receipt_merges_legacy_fields_and_requires_fail_closed() {
        // Older Main: empty server_capabilities, legacy toolkit_typed_refusals = true.
        let legacy_receipt = ClaimReceiptV1 {
            toolkit_typed_refusals: true,
            ..ClaimReceiptV1::default()
        };
        let legacy_caps =
            NegotiatedCapabilities::from_receipt(&legacy_receipt).expect("legacy receipt");
        assert!(legacy_caps.has(CAP_TOOLKIT_TYPED_REFUSALS));
        assert!(!legacy_caps.has(CAP_NEGOTIATION));
        assert!(
            legacy_caps
                .require(&[CAP_TOOLKIT_TYPED_REFUSALS.to_owned()])
                .is_ok()
        );
        assert!(matches!(
            legacy_caps.require(&["runtime.strict_future.v1".to_owned()]),
            Err(ControlSemanticError::UnsupportedCapability(_))
        ));

        // Newer Main: server_capabilities carries capability even if legacy bool is false.
        let modern_receipt = ClaimReceiptV1 {
            toolkit_typed_refusals: false,
            server_capabilities: vec![
                CAP_NEGOTIATION.to_owned(),
                CAP_TOOLKIT_TYPED_REFUSALS.to_owned(),
                "runtime.future_server_feature.v2".to_owned(),
            ],
            ..ClaimReceiptV1::default()
        };
        let modern_caps =
            NegotiatedCapabilities::from_receipt(&modern_receipt).expect("modern receipt");
        assert!(modern_caps.has(CAP_TOOLKIT_TYPED_REFUSALS));
        assert!(modern_caps.has(CAP_NEGOTIATION));
        assert!(modern_caps.has("runtime.future_server_feature.v2"));
    }

    #[test]
    fn sync_worker_capabilities_matches_active_boolean_opt_ins() {
        let mut req = ClaimCommandRequestV1 {
            fanout_interrupt_decisions: true,
            fanout_delegated_credentials: true,
            idle_compaction: true,
            governed_security_context_v1: true,
            ..ClaimCommandRequestV1::default()
        };
        sync_worker_capabilities(&mut req);
        assert_eq!(
            req.worker_capabilities,
            vec![
                CAP_FANOUT_DELEGATED_CREDENTIALS.to_owned(),
                CAP_FANOUT_INTERRUPT_DECISIONS.to_owned(),
                CAP_GOVERNED_SECURITY_CONTEXT.to_owned(),
                CAP_IDLE_COMPACTION.to_owned(),
            ]
        );
    }

    fn fixture_vector(name: &str) -> Vec<u8> {
        let (_, hex) = include_str!("../../tests/fixtures/agent_control_vectors.txt")
            .lines()
            .filter_map(|line| line.split_once('='))
            .find(|(key, _)| *key == name)
            .expect("fixture");
        hex.as_bytes()
            .as_chunks::<2>()
            .0
            .iter()
            .map(|pair| {
                u8::from_str_radix(std::str::from_utf8(pair).expect("hex"), 16).expect("byte")
            })
            .collect()
    }

    #[derive(Clone)]
    enum FirstCallBehavior {
        Accept,
        RejectProtocolViolation,
        RejectStrictCodecUnknownField22,
        FailStatus(tonic::Code, &'static str),
    }

    struct NegotiationRpc {
        first_call: FirstCallBehavior,
        response: crate::protocol::elitea::runtime::v1::ClaimCommandResponseV1,
        seen: std::sync::Arc<std::sync::Mutex<Vec<ClaimCommandRequestV1>>>,
    }

    #[async_trait::async_trait]
    impl crate::transport::ControlRpc for NegotiationRpc {
        async fn claim_command(
            &self,
            request: tonic::Request<ClaimCommandRequestV1>,
        ) -> Result<
            tonic::Response<crate::protocol::elitea::runtime::v1::ClaimCommandResponseV1>,
            tonic::Status,
        > {
            let req = request.into_inner();
            let has_caps = !req.worker_capabilities.is_empty();
            self.seen.lock().expect("lock").push(req);
            match self.first_call {
                FirstCallBehavior::RejectProtocolViolation if has_caps => {
                    return Ok(tonic::Response::new(
                        crate::protocol::elitea::runtime::v1::ClaimCommandResponseV1 {
                            receipt: None,
                            rejection: Some(crate::protocol::elitea::runtime::v1::RuntimeErrorV1 {
                                code: crate::protocol::elitea::runtime::v1::RuntimeErrorCodeV1::ProtocolViolation as i32,
                                safe_message: "The claim request is malformed.".to_owned(),
                                retryable: false,
                            }),
                        },
                    ));
                }
                FirstCallBehavior::RejectStrictCodecUnknownField22 if has_caps => {
                    return Err(tonic::Status::internal(
                        "grpc: error unmarshalling request: strict protobuf contract violation: unknown field 22 in elitea.runtime.v1.ClaimCommandRequestV1",
                    ));
                }
                FirstCallBehavior::FailStatus(code, message) => {
                    return Err(tonic::Status::new(code, message));
                }
                FirstCallBehavior::Accept
                | FirstCallBehavior::RejectProtocolViolation
                | FirstCallBehavior::RejectStrictCodecUnknownField22 => {}
            }
            Ok(tonic::Response::new(self.response.clone()))
        }

        async fn begin_execution(
            &self,
            _: tonic::Request<crate::protocol::elitea::runtime::v1::BeginExecutionRequestV1>,
        ) -> Result<
            tonic::Response<crate::protocol::elitea::runtime::v1::BeginExecutionResponseV1>,
            tonic::Status,
        > {
            panic!("unexpected begin_execution")
        }

        async fn authorize_invocation(
            &self,
            _: tonic::Request<crate::protocol::elitea::runtime::v1::AuthorizeInvocationRequestV1>,
        ) -> Result<
            tonic::Response<crate::protocol::elitea::runtime::v1::AuthorizeInvocationResponseV1>,
            tonic::Status,
        > {
            panic!("unexpected authorize_invocation")
        }

        async fn renew_lease(
            &self,
            _: tonic::Request<crate::protocol::elitea::runtime::v1::RenewLeaseRequestV1>,
        ) -> Result<
            tonic::Response<crate::protocol::elitea::runtime::v1::RenewLeaseResponseV1>,
            tonic::Status,
        > {
            panic!("unexpected renew_lease")
        }

        async fn observe_desired_state(
            &self,
            _: tonic::Request<crate::protocol::elitea::runtime::v1::ObserveDesiredStateRequestV1>,
        ) -> Result<
            tonic::Response<crate::protocol::elitea::runtime::v1::ObserveDesiredStateResponseV1>,
            tonic::Status,
        > {
            panic!("unexpected observe_desired_state")
        }

        async fn prepare_settlement(
            &self,
            _: tonic::Request<crate::protocol::elitea::runtime::v1::PrepareSettlementRequestV1>,
        ) -> Result<
            tonic::Response<crate::protocol::elitea::runtime::v1::PrepareSettlementResponseV1>,
            tonic::Status,
        > {
            panic!("unexpected prepare_settlement")
        }
    }

    fn test_client_and_command(
        first_call: FirstCallBehavior,
        mutate_receipt: impl FnOnce(&mut ClaimReceiptV1),
    ) -> (
        crate::protocol::control::AgentControlClient<NegotiationRpc>,
        std::sync::Arc<std::sync::Mutex<Vec<ClaimCommandRequestV1>>>,
        crate::protocol::command::VerifiedAgentCommand,
        i64,
    ) {
        use prost::Message;

        let verified = crate::protocol::command::parse_and_verify_agent_command(
            &fixture_vector("signed_command"),
            Some(&crate::protocol::command::TestOnlyConformanceHmacAuthenticator),
        )
        .expect("verified command");
        let mut response = crate::protocol::elitea::runtime::v1::ClaimCommandResponseV1::decode(
            fixture_vector("accepted_claim").as_slice(),
        )
        .expect("accepted claim");
        let receipt = response.receipt.as_mut().expect("receipt");
        let fence = receipt.fence.clone().expect("fence");
        let now = receipt.lease_expires_at_unix_millis - 1000;
        mutate_receipt(receipt);

        let seen = std::sync::Arc::new(std::sync::Mutex::new(Vec::new()));
        let rpc = NegotiationRpc {
            first_call,
            response,
            seen: seen.clone(),
        };
        let config = crate::transport::ControlGrpcConfig {
            workload_session_id: fence.workload_session_id,
            producer_id: fence.producer_id,
            deadline: std::time::Duration::from_secs(5),
        };
        let client = crate::protocol::control::AgentControlClient::new(rpc, config)
            .expect("client")
            .with_governed_security_context(true);
        (client, seen, verified, now)
    }

    #[tokio::test]
    async fn new_worker_falls_back_once_for_old_main_strict_codec_and_protocol_violation() {
        for behavior in [
            FirstCallBehavior::RejectStrictCodecUnknownField22,
            FirstCallBehavior::RejectProtocolViolation,
        ] {
            let (client, seen, verified, now) = test_client_and_command(behavior, |receipt| {
                receipt.toolkit_typed_refusals = true;
                receipt.server_capabilities.clear();
            });
            let claim = client
                .claim_agent(&verified, now)
                .await
                .expect("fallback claim succeeds");
            assert!(claim.server_capabilities().has(CAP_TOOLKIT_TYPED_REFUSALS));

            let seen = seen.lock().expect("lock");
            assert_eq!(
                seen.len(),
                2,
                "expected initial attempt plus one legacy fallback"
            );
            assert_ne!(seen[0].worker_capabilities.len(), 0);
            assert_eq!(seen[1].worker_capabilities.len(), 0);
            assert!(seen[1].fanout_interrupt_decisions);
            assert!(seen[1].fanout_delegated_credentials);
            assert!(seen[1].idle_compaction);
            assert!(seen[1].governed_security_context_v1);
        }
    }

    #[tokio::test]
    async fn ambiguous_grpc_statuses_never_retry_claim_command() {
        for (code, message) in [
            (tonic::Code::Unavailable, "transient transport reset"),
            (tonic::Code::DeadlineExceeded, "deadline exceeded"),
            (tonic::Code::Internal, "database crashed"),
            (
                tonic::Code::Internal,
                "strict protobuf contract violation: unknown field 99 in elitea.runtime.v1.ClaimCommandRequestV1",
            ),
        ] {
            let (client, seen, verified, now) =
                test_client_and_command(FirstCallBehavior::FailStatus(code, message), |_| {});
            let err = client
                .claim_agent(&verified, now)
                .await
                .err()
                .expect("ambiguous status must fail without fallback");
            assert!(matches!(
                err,
                crate::protocol::control::AgentControlError::Transport(
                    crate::transport::ControlGrpcError::Unavailable(_)
                )
            ));
            assert_eq!(
                seen.lock().expect("lock").len(),
                1,
                "status {code:?} ({message}) must never trigger a second claim RPC"
            );
        }
    }

    #[tokio::test]
    async fn new_worker_and_new_main_negotiate_two_way_capabilities() {
        let (client, seen, verified, now) =
            test_client_and_command(FirstCallBehavior::Accept, |receipt| {
                receipt.toolkit_typed_refusals = false;
                receipt.server_capabilities = vec![
                    CAP_NEGOTIATION.to_owned(),
                    CAP_TOOLKIT_TYPED_REFUSALS.to_owned(),
                    "runtime.future_server.v2".to_owned(),
                ];
            });
        let claim = client
            .claim_agent(&verified, now)
            .await
            .expect("negotiated claim succeeds");
        assert!(claim.server_capabilities().has(CAP_NEGOTIATION));
        assert!(claim.server_capabilities().has(CAP_TOOLKIT_TYPED_REFUSALS));
        assert!(claim.server_capabilities().has("runtime.future_server.v2"));

        let seen = seen.lock().expect("lock");
        assert_eq!(seen.len(), 1);
        assert!(
            seen[0]
                .worker_capabilities
                .contains(&CAP_IDLE_COMPACTION.to_owned())
        );
        assert!(
            seen[0]
                .worker_capabilities
                .contains(&CAP_GOVERNED_SECURITY_CONTEXT.to_owned())
        );
    }

    #[tokio::test]
    async fn missing_required_server_capability_and_malformed_caps_fail_closed() {
        let (client, _, verified, now) =
            test_client_and_command(FirstCallBehavior::Accept, |receipt| {
                receipt.server_capabilities = vec![CAP_NEGOTIATION.to_owned()];
            });
        let strict_client = client
            .with_required_server_capabilities(&["runtime.mandatory_security.v1".to_owned()])
            .expect("valid required capability");
        let err = strict_client
            .claim_agent(&verified, now)
            .await
            .err()
            .expect("missing required capability must fail closed");
        assert!(matches!(
            err,
            crate::protocol::control::AgentControlError::Semantic(
                ControlSemanticError::UnsupportedCapability(_)
            )
        ));

        let (bad_client, _, bad_verified, bad_now) =
            test_client_and_command(FirstCallBehavior::Accept, |receipt| {
                receipt.server_capabilities = vec!["InvalidUpper".to_owned()];
            });
        let bad_err = bad_client
            .claim_agent(&bad_verified, bad_now)
            .await
            .err()
            .expect("malformed server capabilities must fail closed");
        assert!(matches!(
            bad_err,
            crate::protocol::control::AgentControlError::Semantic(
                ControlSemanticError::InvalidInput(_)
            )
        ));
    }

    #[test]
    fn from_receipt_enforces_max_bound_when_merging_legacy_booleans() {
        let exact: Vec<String> = (0..MAX_NEGOTIATED_CAPABILITIES)
            .map(|i| format!("runtime.cap_{i:02}.v1"))
            .collect();
        let overflow_receipt = ClaimReceiptV1 {
            toolkit_typed_refusals: true,
            server_capabilities: exact,
            ..ClaimReceiptV1::default()
        };
        assert!(matches!(
            NegotiatedCapabilities::from_receipt(&overflow_receipt),
            Err(ControlSemanticError::InvalidInput(_))
        ));
    }

    #[tokio::test]
    async fn non_executable_receipts_reject_server_capabilities_fail_closed() {
        let (retry_client, _, retry_verified, retry_now) =
            test_client_and_command(FirstCallBehavior::Accept, |receipt| {
                receipt.disposition =
                    crate::protocol::elitea::runtime::v1::ClaimDispositionV1::RetryLaterNoack
                        as i32;
                receipt.fence = None;
                receipt.lease_expires_at_unix_millis = 0;
                receipt.input_bundle_ref = None;
                receipt.input_bundle = None;
                receipt.claim_handoff_watermark = 0;
                receipt.claim_id.clear();
                receipt.settlement_recovery = None;
                receipt.claim_started_at_unix_micros = 0;
                receipt.node_recovery_receipt_json.clear();
                receipt.server_capabilities = vec![CAP_NEGOTIATION.to_owned()];
            });
        let err = retry_client
            .claim_agent_delivery(&retry_verified, retry_now)
            .await
            .err()
            .expect("non-authority receipt carrying server_capabilities must fail closed");
        assert!(matches!(
            err,
            crate::protocol::control::AgentControlError::Semantic(
                ControlSemanticError::InvalidInput(_)
            )
        ));

        let (settled_client, _, settled_verified, settled_now) =
            test_client_and_command(FirstCallBehavior::Accept, |receipt| {
                receipt.disposition =
                    crate::protocol::elitea::runtime::v1::ClaimDispositionV1::SettledAck as i32;
                receipt.input_bundle_ref = None;
                receipt.input_bundle = None;
                receipt.claim_started_at_unix_micros = 0;
                receipt.node_recovery_receipt_json.clear();
                receipt.server_capabilities = vec![CAP_NEGOTIATION.to_owned()];
            });
        let err = settled_client
            .claim_agent_delivery(&settled_verified, settled_now)
            .await
            .err()
            .expect("recovery receipt carrying server_capabilities must fail closed");
        assert!(matches!(
            err,
            crate::protocol::control::AgentControlError::Semantic(
                ControlSemanticError::InvalidInput(_)
            )
        ));
    }
}
