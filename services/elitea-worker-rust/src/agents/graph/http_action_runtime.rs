//! Native HTTP Request pipeline node (`type: http`, `revision: 2`).
//!
//! The Worker holds no rule origin, no credentials, and no outbound egress
//! client for this node. Every attempt resolves canonical inputs from
//! checkpointed state, binds `effect_id` v3, executes behind [`RecoverableNode`]'s
//! fenced durable `Started` journal, and invokes Main's `ExecuteHTTPAction` RPC
//! over the live execution claim.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use adk_rust::graph::{GraphError, Node, NodeContext, NodeOutput, State};
use async_trait::async_trait;
use elitea_agent_runtime::canonical;
use elitea_agent_runtime::graph::http_action::{
    HTTP_ACTION_INVOCATION_SCHEMA, HttpActionData, HttpActionInvocationV3,
    HttpActionNodeDefinition, HttpActionProjection, HttpActionReceiptState, MAX_HTTP_ATTEMPTS,
    MAX_HTTP_TIMEOUT_MS, decode_receipt_v2, effect_id_v3, effect_id_v3_hex, visit_id,
};
use serde_json::{Map, Value};

use super::node_recovery::{NodeFailure, NodeFailureClass, ReplaySafety};
use super::node_recovery_owner::{
    HTTP_RECEIPT_AUDIENCE, NodeRecoveryOwnerProof, NodeRecoveryOwnerProofKind,
};
use super::node_recovery_runtime::{
    NodeAttemptAuthority, NodeAttemptBody, NodeAttemptReportedFailure, NodeResultRecovery,
};
use crate::protocol::control::ClaimBoundRuntimeContextAuthority;
use crate::transport::platform_client::PlatformClient;
use crate::transport::runtime_context::{HttpActionRpcError, HttpActionRpcResponse};

const DEFAULT_HTTP_TIMEOUT_MS: u32 = MAX_HTTP_TIMEOUT_MS;
const DEFAULT_MAX_TRANSPORT_RETRIES: usize = 3;
const DEFAULT_TRANSPORT_RETRY_BASE_MS: u64 = 200;
const MAX_TRANSPORT_RETRY_BACKOFF_MS: u64 = 2_000;

/// Transport boundary for Main's `ExecuteHTTPAction` RPC.
#[async_trait]
pub(crate) trait PipelineHttpActionExecutor: Send + Sync {
    async fn execute(
        &self,
        invocation: &HttpActionInvocationV3,
        call_timeout: Duration,
    ) -> Result<HttpActionRpcResponse, HttpActionRpcError>;
}

/// Live claim-scoped HTTP action executor over [`PlatformClient`].
pub(crate) struct ClaimHttpActionExecutor {
    platform: Arc<PlatformClient>,
    authority: Arc<ClaimBoundRuntimeContextAuthority>,
}

impl ClaimHttpActionExecutor {
    pub(crate) const fn new(
        platform: Arc<PlatformClient>,
        authority: Arc<ClaimBoundRuntimeContextAuthority>,
    ) -> Self {
        Self {
            platform,
            authority,
        }
    }
}

#[async_trait]
impl PipelineHttpActionExecutor for ClaimHttpActionExecutor {
    async fn execute(
        &self,
        invocation: &HttpActionInvocationV3,
        call_timeout: Duration,
    ) -> Result<HttpActionRpcResponse, HttpActionRpcError> {
        self.platform
            .execute_http_action(&self.authority, invocation, call_timeout)
            .await
    }
}

/// Single-attempt HTTP node body executed exclusively behind [`RecoverableNode`].
pub(crate) struct HttpActionAttempt {
    definition: Arc<HttpActionNodeDefinition>,
    binding_digest_bytes: [u8; 32],
    binding_digest_hex: String,
    state_types: Arc<BTreeMap<String, String>>,
    executor: Arc<dyn PipelineHttpActionExecutor>,
    execution_id: String,
    generation: u64,
    settings_timeout_ms: Option<u64>,
    retries_enabled: bool,
    execution_deadline: Option<tokio::time::Instant>,
    max_transport_retries: usize,
    transport_retry_base_ms: u64,
}

impl HttpActionAttempt {
    #[allow(
        clippy::too_many_arguments,
        reason = "Explicitly separate node contract, frozen binding, runtime authority, and execution scope."
    )]
    pub(crate) fn new(
        definition: HttpActionNodeDefinition,
        binding_digest_bytes: [u8; 32],
        binding_digest_hex: String,
        state_types: BTreeMap<String, String>,
        executor: Arc<dyn PipelineHttpActionExecutor>,
        execution_id: String,
        generation: u64,
        settings_timeout_ms: Option<u64>,
        retries_enabled: bool,
        execution_deadline: Option<tokio::time::Instant>,
    ) -> Self {
        Self {
            definition: Arc::new(definition),
            binding_digest_bytes,
            binding_digest_hex,
            state_types: Arc::new(state_types),
            executor,
            execution_id,
            generation,
            settings_timeout_ms,
            retries_enabled,
            execution_deadline,
            max_transport_retries: DEFAULT_MAX_TRANSPORT_RETRIES,
            transport_retry_base_ms: DEFAULT_TRANSPORT_RETRY_BASE_MS,
        }
    }

    #[cfg(test)]
    pub(crate) const fn with_transport_retry_config(
        mut self,
        max_retries: usize,
        base_delay_ms: u64,
    ) -> Self {
        self.max_transport_retries = max_retries;
        self.transport_retry_base_ms = base_delay_ms;
        self
    }

    fn effective_node_timeout_ms(&self) -> u64 {
        let mut limit = u64::from(
            self.definition
                .timeout_ms()
                .unwrap_or(DEFAULT_HTTP_TIMEOUT_MS),
        );
        if let Some(settings_ms) = self.settings_timeout_ms {
            limit = limit.min(settings_ms);
        }
        limit.clamp(1, u64::from(MAX_HTTP_TIMEOUT_MS))
    }

    fn remaining_budget(&self) -> Option<Duration> {
        // Give Main headroom for artifact spill (5 s) + receipt commit (5 s)
        // after the outbound HTTP timeout (`effective_node_timeout_ms <= 30 s`),
        // clamped by the run's execution deadline.
        const MAIN_COMMIT_HEADROOM_MS: u64 = 12_000;
        let rpc_limit = Duration::from_millis(
            self.effective_node_timeout_ms()
                .saturating_add(MAIN_COMMIT_HEADROOM_MS),
        );
        match self.execution_deadline {
            Some(deadline) => {
                let remaining = deadline.checked_duration_since(tokio::time::Instant::now())?;
                if remaining.is_zero() {
                    None
                } else {
                    Some(rpc_limit.min(remaining))
                }
            }
            None => Some(rpc_limit),
        }
    }

    fn map_receipt(
        &self,
        response: HttpActionRpcResponse,
        expected_activation_id: &str,
        effect_id: [u8; 32],
    ) -> Result<NodeOutput, NodeAttemptReportedFailure> {
        let expected_effect_hex = encode_hex(&effect_id);
        let receipt = response.receipt;
        if receipt.activation_id != expected_activation_id
            || receipt.binding_digest != self.binding_digest_hex
            || receipt.effect_id != expected_effect_hex
        {
            return Err(NodeFailure::new(
                NodeFailureClass::InvalidResult,
                ReplaySafety::UnknownExternalEffect { effect_id },
            )
            .into());
        }

        match receipt.state {
            HttpActionReceiptState::Completed => {
                let result = receipt.result.ok_or_else(|| {
                    NodeFailure::new(
                        NodeFailureClass::InvalidResult,
                        ReplaySafety::UnknownExternalEffect { effect_id },
                    )
                })?;
                project_http_result(&self.definition, &self.state_types, &result)
                    .map_err(Into::into)
            }
            HttpActionReceiptState::Uncertain => {
                if response.retryable {
                    Err(NodeFailure::new(
                        NodeFailureClass::DependencyUnavailable,
                        ReplaySafety::IdempotentEffectNotCommitted { effect_id },
                    )
                    .into())
                } else {
                    Err(NodeFailure::new(
                        NodeFailureClass::Unknown,
                        ReplaySafety::UnknownExternalEffect { effect_id },
                    )
                    .into())
                }
            }
            HttpActionReceiptState::Failed => {
                let code = receipt.failure_code.as_deref().unwrap_or("");
                Err(
                    map_failed_receipt(code, response.retryable, self.retries_enabled, effect_id)
                        .into(),
                )
            }
        }
    }
}

#[async_trait]
impl Node for HttpActionAttempt {
    fn name(&self) -> &str {
        self.definition.id()
    }

    async fn execute(&self, _: &NodeContext) -> Result<NodeOutput, GraphError> {
        Err(GraphError::Other(
            "pipeline.http.recoverable_wrapper_required".to_owned(),
        ))
    }
}

impl HttpActionAttempt {
    async fn dispatch_with_transport_retry(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
        invocation: &HttpActionInvocationV3,
        activation_id: &str,
        effect_id: [u8; 32],
        attempt_deadline: tokio::time::Instant,
    ) -> Result<NodeOutput, NodeAttemptReportedFailure> {
        let mut transport_try = 0_usize;
        loop {
            if context
                .config
                .parent_context
                .as_ref()
                .is_some_and(|parent| parent.is_cancelled())
            {
                let safety = if transport_try == 0 && !authority.recovering_started() {
                    ReplaySafety::NoExternalEffect
                } else {
                    ReplaySafety::UnknownExternalEffect { effect_id }
                };
                return Err(NodeFailure::new(NodeFailureClass::Cancelled, safety).into());
            }

            let Some(call_timeout) =
                attempt_deadline.checked_duration_since(tokio::time::Instant::now())
            else {
                let pre_dispatch = transport_try == 0 && !authority.recovering_started();
                let (class, safety) = if pre_dispatch {
                    let class = if self
                        .execution_deadline
                        .is_some_and(|d| tokio::time::Instant::now() >= d)
                    {
                        NodeFailureClass::DeadlineInsufficient
                    } else {
                        NodeFailureClass::AttemptTimeout
                    };
                    (class, ReplaySafety::NoExternalEffect)
                } else {
                    (
                        NodeFailureClass::AttemptTimeout,
                        ReplaySafety::UnknownExternalEffect { effect_id },
                    )
                };
                return Err(NodeFailure::new(class, safety).into());
            };
            if call_timeout.is_zero() {
                let safety = if transport_try == 0 && !authority.recovering_started() {
                    ReplaySafety::NoExternalEffect
                } else {
                    ReplaySafety::UnknownExternalEffect { effect_id }
                };
                return Err(NodeFailure::new(NodeFailureClass::AttemptTimeout, safety).into());
            }

            match self.executor.execute(invocation, call_timeout).await {
                Ok(response) => return self.map_receipt(response, activation_id, effect_id),
                Err(HttpActionRpcError::Unavailable)
                    if transport_try < self.max_transport_retries =>
                {
                    let backoff_ms = self
                        .transport_retry_base_ms
                        .saturating_mul(1_u64 << transport_try.min(10))
                        .min(MAX_TRANSPORT_RETRY_BACKOFF_MS);
                    let remaining =
                        attempt_deadline.saturating_duration_since(tokio::time::Instant::now());
                    if remaining <= Duration::from_millis(backoff_ms) {
                        return Err(NodeFailure::new(
                            NodeFailureClass::DependencyUnavailable,
                            ReplaySafety::UnknownExternalEffect { effect_id },
                        )
                        .into());
                    }
                    tokio::time::sleep(Duration::from_millis(backoff_ms)).await;
                    transport_try += 1;
                }
                Err(rpc_error) => {
                    return Err(map_rpc_error(rpc_error, effect_id).into());
                }
            }
        }
    }
}

#[async_trait]
impl NodeAttemptBody for HttpActionAttempt {
    async fn execute_attempt(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
    ) -> Result<NodeOutput, NodeFailure> {
        self.execute_attempt_reported(context, authority)
            .await
            .map_err(|reported| reported.failure)
    }

    async fn execute_attempt_reported(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
    ) -> Result<NodeOutput, NodeAttemptReportedFailure> {
        if !authority.matches(
            self.definition.id(),
            self.definition.config_digest(),
            context.step,
        ) {
            return Err(NodeFailure::new(
                NodeFailureClass::InvalidInput,
                ReplaySafety::NoExternalEffect,
            )
            .into());
        }
        let step = u64::try_from(context.step).map_err(|_| {
            NodeFailure::new(
                NodeFailureClass::InvalidInput,
                ReplaySafety::NoExternalEffect,
            )
        })?;
        let inv_attempt = authority
            .attempt()
            .checked_sub(1)
            .map(u32::from)
            .filter(|attempt| *attempt < MAX_HTTP_ATTEMPTS)
            .ok_or_else(|| {
                NodeFailure::new(
                    NodeFailureClass::InvalidInput,
                    ReplaySafety::NoExternalEffect,
                )
            })?;

        let state_map = state_to_btree(&context.state);
        let resolved = self
            .definition
            .resolve_canonical_inputs(&state_map)
            .map_err(|_| {
                NodeFailure::new(
                    NodeFailureClass::InvalidInput,
                    ReplaySafety::NoExternalEffect,
                )
            })?;

        let activation_id = visit_id(
            &context.config.thread_id,
            self.definition.id(),
            step,
            self.binding_digest_bytes,
        );
        let effect_id = effect_id_v3(
            &self.execution_id,
            self.generation,
            &activation_id,
            inv_attempt,
            &self.binding_digest_hex,
            &resolved.inputs_digest_hex,
        );

        let invocation = HttpActionInvocationV3 {
            schema_version: HTTP_ACTION_INVOCATION_SCHEMA.to_owned(),
            node_id: self.definition.id().to_owned(),
            thread_id: context.config.thread_id.clone(),
            step,
            attempt: inv_attempt,
            activation_id: activation_id.clone(),
            binding_digest: self.binding_digest_hex.clone(),
            inputs_b64: resolved.inputs_b64,
            inputs_digest: resolved.inputs_digest_hex,
            scope_ref: None,
        };

        let attempt_deadline = tokio::time::Instant::now()
            + self.remaining_budget().ok_or_else(|| {
                NodeFailure::new(
                    NodeFailureClass::DeadlineInsufficient,
                    ReplaySafety::NoExternalEffect,
                )
            })?;

        self.dispatch_with_transport_retry(
            context,
            authority,
            &invocation,
            &activation_id,
            effect_id,
            attempt_deadline,
        )
        .await
    }

    fn retry_deadline_budget(&self) -> Option<(u64, u64)> {
        let deadline = self.execution_deadline?;
        let remaining_ms = deadline
            .checked_duration_since(tokio::time::Instant::now())
            .map_or(0, |duration| {
                u64::try_from(duration.as_millis()).unwrap_or(u64::MAX)
            });
        Some((remaining_ms, 1))
    }
}

/// Read-only owning projector for Main-owned `CommittedResult` recovery proofs
/// (`HTTP_RECEIPT_AUDIENCE`). Never dispatches any external request.
pub(crate) struct HttpActionCommittedProjector {
    definition: HttpActionNodeDefinition,
    state_types: BTreeMap<String, String>,
    execution_id: String,
    generation: u64,
}

impl HttpActionCommittedProjector {
    pub(crate) fn new(
        definition: HttpActionNodeDefinition,
        state_types: BTreeMap<String, String>,
        execution_id: String,
        generation: u64,
    ) -> Result<Self, &'static str> {
        if definition.output_keys().len() != 1 {
            return Err("HTTP node requires exactly one output key");
        }
        Ok(Self {
            definition,
            state_types,
            execution_id,
            generation,
        })
    }
}

#[async_trait]
impl Node for HttpActionCommittedProjector {
    fn name(&self) -> &str {
        self.definition.id()
    }

    async fn execute(&self, _: &NodeContext) -> Result<NodeOutput, GraphError> {
        Err(GraphError::Other(
            "pipeline.http.committed_projector_read_only".to_owned(),
        ))
    }
}

impl NodeResultRecovery for HttpActionCommittedProjector {
    fn project_committed_result(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
        proof: &NodeRecoveryOwnerProof,
        receipt_wire: &[u8],
    ) -> Result<NodeOutput, NodeFailure> {
        let deny = || {
            NodeFailure::new(
                NodeFailureClass::AuthorizationDenied,
                ReplaySafety::NoExternalEffect,
            )
        };
        if proof.kind != NodeRecoveryOwnerProofKind::CommittedResult
            || proof.execution_id != self.execution_id
            || proof.generation != self.generation
            || !proof.matches_result_bytes(receipt_wire)
            || !authority.matches(
                self.definition.id(),
                self.definition.config_digest(),
                context.step,
            )
        {
            return Err(deny());
        }
        let Some(reference) = proof.result_ref.as_ref() else {
            return Err(deny());
        };
        if reference.required_grant_audience != HTTP_RECEIPT_AUDIENCE {
            return Err(deny());
        }
        let receipt = decode_receipt_v2(receipt_wire).map_err(|_| deny())?;
        if receipt.state != HttpActionReceiptState::Completed
            || receipt.effect_id != proof.effect_id
        {
            return Err(deny());
        }
        let binding_bytes = decode_hex_32(&receipt.binding_digest).ok_or_else(deny)?;
        let step = u64::try_from(context.step).map_err(|_| deny())?;
        let inv_attempt = authority
            .attempt()
            .checked_sub(1)
            .map(u32::from)
            .filter(|attempt| *attempt < MAX_HTTP_ATTEMPTS)
            .ok_or_else(deny)?;
        let state_map = state_to_btree(&context.state);
        let resolved = self
            .definition
            .resolve_canonical_inputs(&state_map)
            .map_err(|_| deny())?;
        let expected_activation = visit_id(
            &context.config.thread_id,
            self.definition.id(),
            step,
            binding_bytes,
        );
        let expected_effect = effect_id_v3_hex(
            &self.execution_id,
            self.generation,
            &expected_activation,
            inv_attempt,
            &receipt.binding_digest,
            &resolved.inputs_digest_hex,
        );
        if receipt.activation_id != expected_activation || receipt.effect_id != expected_effect {
            return Err(deny());
        }
        let result = receipt.result.ok_or_else(deny)?;
        project_http_result(&self.definition, &self.state_types, &result)
    }
}

fn state_to_btree(state: &State) -> BTreeMap<String, Value> {
    state.iter().map(|(k, v)| (k.clone(), v.clone())).collect()
}

fn projection_to_json(result: &HttpActionProjection) -> Result<Value, NodeFailure> {
    let mut obj = Map::new();
    obj.insert("status".to_owned(), Value::Number(result.status.into()));
    if let Some(ct) = &result.content_type {
        obj.insert("content_type".to_owned(), Value::String(ct.clone()));
    }
    obj.insert(
        "byte_length".to_owned(),
        Value::Number(result.byte_length.into()),
    );
    let data_value = match &result.data {
        HttpActionData::Empty => Value::Null,
        HttpActionData::Json { value } => value.clone(),
        HttpActionData::Text { value } => Value::String(value.clone()),
        HttpActionData::Artifact { reference } => {
            serde_json::to_value(reference).map_err(|_| {
                NodeFailure::new(
                    NodeFailureClass::InvalidResult,
                    ReplaySafety::NoExternalEffect,
                )
            })?
        }
    };
    obj.insert("data".to_owned(), data_value);
    Ok(Value::Object(obj))
}

fn project_http_result(
    definition: &HttpActionNodeDefinition,
    state_types: &BTreeMap<String, String>,
    result: &HttpActionProjection,
) -> Result<NodeOutput, NodeFailure> {
    let Some(output_key) = definition.output_keys().first() else {
        return Err(NodeFailure::new(
            NodeFailureClass::InvalidResult,
            ReplaySafety::NoExternalEffect,
        ));
    };
    let projected = projection_to_json(result)?;
    let declared = state_types.get(output_key).map_or("dict", String::as_str);
    let value = match declared {
        "str" => {
            let bytes = canonical::to_vec(&projected).map_err(|_| {
                NodeFailure::new(
                    NodeFailureClass::InvalidResult,
                    ReplaySafety::NoExternalEffect,
                )
            })?;
            let text = String::from_utf8(bytes).map_err(|_| {
                NodeFailure::new(
                    NodeFailureClass::InvalidResult,
                    ReplaySafety::NoExternalEffect,
                )
            })?;
            Value::String(text)
        }
        "dict" | "any" => projected,
        _ => {
            return Err(NodeFailure::new(
                NodeFailureClass::InvalidResult,
                ReplaySafety::NoExternalEffect,
            ));
        }
    };
    Ok(NodeOutput::new().with_update(output_key, value))
}

fn map_failed_receipt(
    code: &str,
    retryable: bool,
    retries_enabled: bool,
    effect_id: [u8; 32],
) -> NodeFailure {
    match code {
        "policy_denied" | "authorization" | "egress_denied" | "redirect_refused" => {
            NodeFailure::new(
                NodeFailureClass::AuthorizationDenied,
                ReplaySafety::NoExternalEffect,
            )
        }
        "authentication" | "credential_missing" => NodeFailure::new(
            NodeFailureClass::AuthenticationDenied,
            ReplaySafety::NoExternalEffect,
        ),
        "nondeterministic_inputs" | "invalid_input" | "inputs_too_large" => NodeFailure::new(
            NodeFailureClass::InvalidInput,
            ReplaySafety::NoExternalEffect,
        ),
        "timeout" => map_transient_failed_receipt(
            NodeFailureClass::AttemptTimeout,
            retryable,
            retries_enabled,
            effect_id,
        ),
        "rate_limited" => map_transient_failed_receipt(
            NodeFailureClass::RateLimited,
            retryable,
            retries_enabled,
            effect_id,
        ),
        "dependency_unavailable" => map_transient_failed_receipt(
            NodeFailureClass::DependencyUnavailable,
            retryable,
            retries_enabled,
            effect_id,
        ),
        // Non-transient result failures ("response_too_large", "unexpected_status",
        // "invalid_response") and any unrecognized/future Main failure codes fail
        // closed without retrying.
        _ => NodeFailure::new(
            NodeFailureClass::InvalidResult,
            ReplaySafety::NoExternalEffect,
        ),
    }
}

fn map_transient_failed_receipt(
    transient_class: NodeFailureClass,
    retryable: bool,
    retries_enabled: bool,
    effect_id: [u8; 32],
) -> NodeFailure {
    if retryable {
        NodeFailure::new(
            transient_class,
            ReplaySafety::IdempotentEffectNotCommitted { effect_id },
        )
    } else if !retries_enabled {
        NodeFailure::new(transient_class, ReplaySafety::NoExternalEffect)
    } else {
        // Class C with retries enabled: stop immediately without issuing an
        // illegal attempt+1 while still honoring `on_error`.
        NodeFailure::new(
            NodeFailureClass::InvalidResult,
            ReplaySafety::NoExternalEffect,
        )
    }
}

fn map_rpc_error(error: HttpActionRpcError, effect_id: [u8; 32]) -> NodeFailure {
    match error {
        HttpActionRpcError::Unavailable => NodeFailure::new(
            NodeFailureClass::DependencyUnavailable,
            ReplaySafety::UnknownExternalEffect { effect_id },
        ),
        // Always emit AttemptTimeout (never DeadlineInsufficient) when paired with
        // UnknownExternalEffect so the emitted NodeRecoveryRequiredReceipt passes
        // validate() and Main's noderecovery contract.
        HttpActionRpcError::Timeout => NodeFailure::new(
            NodeFailureClass::AttemptTimeout,
            ReplaySafety::UnknownExternalEffect { effect_id },
        ),
        HttpActionRpcError::PolicyDenied => NodeFailure::new(
            NodeFailureClass::AuthorizationDenied,
            ReplaySafety::NoExternalEffect,
        ),
        HttpActionRpcError::AuthenticationDenied => NodeFailure::new(
            NodeFailureClass::AuthenticationDenied,
            ReplaySafety::NoExternalEffect,
        ),
        HttpActionRpcError::NondeterministicInputs
        | HttpActionRpcError::InvalidInput
        | HttpActionRpcError::ResourceExhausted => NodeFailure::new(
            NodeFailureClass::InvalidInput,
            ReplaySafety::NoExternalEffect,
        ),
        HttpActionRpcError::InvalidReceipt | HttpActionRpcError::Internal => NodeFailure::new(
            NodeFailureClass::Unknown,
            ReplaySafety::UnknownExternalEffect { effect_id },
        ),
    }
}

fn decode_hex_32(value: &str) -> Option<[u8; 32]> {
    if value.len() != 64 {
        return None;
    }
    let mut out = [0_u8; 32];
    for (index, byte) in out.iter_mut().enumerate() {
        let pair = value.get(index * 2..index * 2 + 2)?;
        if pair
            .bytes()
            .any(|digit| !matches!(digit, b'0'..=b'9' | b'a'..=b'f'))
        {
            return None;
        }
        *byte = u8::from_str_radix(pair, 16).ok()?;
    }
    if out.iter().all(|byte| *byte == 0) {
        return None;
    }
    Some(out)
}

fn encode_hex(bytes: &[u8; 32]) -> String {
    use std::fmt::Write;
    let mut out = String::with_capacity(64);
    for byte in bytes {
        let _ = write!(out, "{byte:02x}");
    }
    out
}
