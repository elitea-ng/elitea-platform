//! Project only an immutable whole-Code receipt. No runtime client is retained.

use super::{CodeNodeDefinition, CodeStateBoundary, activation, code_error, project_code_receipt};
use crate::agents::graph::{
    node_recovery::{NodeFailure, NodeFailureClass, ReplaySafety},
    node_recovery_owner::{NodeRecoveryOwnerProof, NodeRecoveryOwnerProofKind},
    node_recovery_runtime::{NodeAttemptAuthority, NodeResultRecovery},
};
use crate::sandbox::code_recovery::{CODE_RESULT_AUDIENCE, WholeCodeRecoveryReceipt, hex, sha256};
use adk_rust::graph::{GraphError, Node, NodeContext, NodeOutput};
use async_trait::async_trait;
use std::collections::BTreeMap;

pub(crate) struct CodeCommittedProjector {
    definition: CodeNodeDefinition,
    types: BTreeMap<String, String>,
    boundary: CodeStateBoundary,
    execution_id: String,
    generation: u64,
    legacy_first_attempt: bool,
}

impl CodeCommittedProjector {
    pub(crate) fn new(
        definition: CodeNodeDefinition,
        types: BTreeMap<String, String>,
        execution_id: String,
        generation: u64,
        legacy_first_attempt: bool,
    ) -> Result<Self, GraphError> {
        if execution_id.len() != 32
            || !execution_id
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
            || !(1..=i64::MAX as u64).contains(&generation)
        {
            return Err(code_error(
                "The original Code execution identity is invalid.",
            ));
        }
        definition
            .validate_source_types(&types)
            .map_err(code_error)?;
        let boundary = CodeStateBoundary::new(&types).map_err(code_error)?;
        Ok(Self {
            definition,
            types,
            boundary,
            execution_id,
            generation,
            legacy_first_attempt,
        })
    }

    /// No checkpoint field can substitute for an authenticated original owner receipt.
    pub(crate) fn validate_owner_receipt(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
        proof: &NodeRecoveryOwnerProof,
        wire: &[u8],
    ) -> Result<WholeCodeRecoveryReceipt, NodeFailure> {
        let reject = || {
            NodeFailure::new(
                NodeFailureClass::AuthorizationDenied,
                ReplaySafety::NoExternalEffect,
            )
        };
        if !authority.matches(
            self.definition.id(),
            self.definition.validated_digest(),
            context.step,
        ) || proof.schema != "elitea.pipeline.node-recovery-owner-proof.v1"
            || proof.execution_id != self.execution_id
            || proof.generation != self.generation
            || proof.activation_id != hex(&authority.logical_activation())
            || proof.attempt != authority.attempt()
        {
            return Err(reject());
        }
        let receipt = WholeCodeRecoveryReceipt::parse(wire).map_err(|_| reject())?;
        let binding = receipt.binding();
        let visit = receipt.visit();
        let dispatch = if self.legacy_first_attempt && authority.attempt() == 1 {
            activation(&self.definition, context).map_err(|_| reject())?
        } else {
            authority.dispatch_activation()
        };
        let source = self
            .definition
            .resolve_source(&context.state, &self.types)
            .map_err(|_| reject())?;
        let input = self
            .boundary
            .input_json(&context.state, self.definition.input_keys())
            .map_err(|_| reject())?;
        let mut input: serde_json::Value = serde_json::from_slice(&input).map_err(|_| reject())?;
        input.sort_all_objects();
        let input = serde_json::to_vec(&input).map_err(|_| reject())?;
        let language = serde_json::to_value(self.definition.language()).map_err(|_| reject())?;
        if binding.execution_id != self.execution_id
            || binding.original_generation != self.generation
            || binding.dispatch_activation != hex(&dispatch)
            || binding.dispatch_activation != proof.effect_id
            || binding.node_digest != hex(&self.definition.validated_digest())
            || language.as_str() != Some(binding.language.as_str())
            || binding.source_sha256 != sha256(source.source().as_bytes())
            || binding.input_sha256 != sha256(&input)
            || visit.activation_id != proof.activation_id
            || visit.attempt != proof.attempt
            || visit.expected_revision != proof.expected_revision
            || visit.node_id != self.definition.id()
            || visit.graph_thread != context.config.thread_id
            || visit.step != context.step as u64
            || authority
                .recovery_receipt_sha256()
                .is_none_or(|hash| visit.receipt_sha256 != hex(&hash))
        {
            return Err(reject());
        }
        let exact = match (&receipt, proof.kind) {
            (
                WholeCodeRecoveryReceipt::CommittedResult { .. },
                NodeRecoveryOwnerProofKind::CommittedResult,
            ) => {
                proof.owner_receipt_ref.is_none()
                    && proof.matches_result_bytes(wire)
                    && proof
                        .result_ref
                        .as_ref()
                        .is_some_and(|r| r.required_grant_audience == CODE_RESULT_AUDIENCE)
            }
            (
                WholeCodeRecoveryReceipt::VerifiedNoEffect { .. },
                NodeRecoveryOwnerProofKind::VerifiedNoEffect,
            ) => proof.matches_owner_receipt_bytes(wire),
            _ => false,
        };
        if !exact {
            return Err(reject());
        }
        Ok(receipt)
    }
}

#[async_trait]
impl Node for CodeCommittedProjector {
    fn name(&self) -> &str {
        self.definition.id()
    }
    async fn execute(&self, _: &NodeContext) -> Result<NodeOutput, GraphError> {
        Err(code_error("An owner-result projector cannot execute Code."))
    }
}

impl NodeResultRecovery for CodeCommittedProjector {
    fn verify_no_effect_receipt(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
        proof: &NodeRecoveryOwnerProof,
        wire: &[u8],
    ) -> Result<(), NodeFailure> {
        let receipt = self.validate_owner_receipt(context, authority, proof, wire)?;
        if !matches!(receipt, WholeCodeRecoveryReceipt::VerifiedNoEffect { .. }) {
            return Err(NodeFailure::new(
                NodeFailureClass::AuthorizationDenied,
                ReplaySafety::NoExternalEffect,
            ));
        }
        Ok(())
    }
    fn project_committed_result(
        &self,
        context: &NodeContext,
        authority: &NodeAttemptAuthority,
        proof: &NodeRecoveryOwnerProof,
        wire: &[u8],
    ) -> Result<NodeOutput, NodeFailure> {
        let receipt = self.validate_owner_receipt(context, authority, proof, wire)?;
        let result = receipt.result_bytes().map_err(|_| {
            NodeFailure::new(
                NodeFailureClass::AuthorizationDenied,
                ReplaySafety::NoExternalEffect,
            )
        })?;
        let updates = project_code_receipt(
            &result,
            &self.boundary,
            self.definition.output_keys(),
            self.definition.structured_output(),
        )
        .map_err(|_| {
            NodeFailure::new(
                NodeFailureClass::InvalidResult,
                ReplaySafety::CompletedExternalEffect {
                    receipt_id: if self.legacy_first_attempt && authority.attempt() == 1 {
                        activation(&self.definition, context)
                            .unwrap_or_else(|_| authority.dispatch_activation())
                    } else {
                        authority.dispatch_activation()
                    },
                },
            )
        })?;
        let mut output = NodeOutput::new();
        for (key, value) in updates {
            output = output.with_update(&key, value);
        }
        Ok(output)
    }
}
