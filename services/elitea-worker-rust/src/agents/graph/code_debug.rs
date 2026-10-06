//! Claim-bound debug export. User data never enters trace metadata.
use super::{
    code::CodeLanguage, code_runtime::CodeInvocation, node_recovery_runtime::NodeAttemptAuthority,
};
use crate::{
    protocol::control::ClaimBoundSandboxAuthority, sandbox::code_recovery::OriginalCodeVisitRef,
};
use async_trait::async_trait;
use ring::digest;
use serde::{Deserialize, Serialize};
use serde_json::value::RawValue;

pub(crate) const SNAPSHOT_SCHEMA: &str = "elitea.runtime.code-debug-snapshot.v1";
pub(crate) const ARTIFACT_SCHEMA: &str = "elitea.runtime.code-debug-artifact.v1";
const MAX_SNAPSHOT_BYTES: usize = 3 * 1024 * 1024;

/// Compiler-owned identity. Skipped by saved definition serialization.
#[derive(Clone)]
pub(super) struct CodeDebugDefinitionPin {
    pub(super) definition: [u8; 32],
    pub(super) yaml: [u8; 32],
}
/// No Debug: configuration can contain the saved source.
pub(super) struct CodeDebugSelection {
    pub(super) node_id: String,
    pub(super) graph_thread_id: String,
    pub(super) graph_step: String,
    pub(super) configuration_json: String,
    pub(super) original: Option<CodeDebugDefinitionPin>,
}

/// This typed attestation comes only from an admitted Code invocation.
/// Main independently resolves the declaration and current writer before upload.
#[derive(Serialize)]
pub(crate) struct CodeDebugAdmission {
    pub(crate) original_visit: OriginalCodeVisitRef,
    pub(crate) attempt: u16,
    pub(crate) schema_version: &'static str,
    pub(crate) node_id: String,
    pub(crate) graph_thread_id: String,
    pub(crate) graph_step: String,
    pub(crate) activation_id: String,
    pub(crate) definition_sha256: String,
    pub(crate) yaml_sha256: String,
    pub(crate) configuration_json: String,
    pub(crate) request_sha256: String,
    pub(crate) source_sha256: String,
    pub(crate) input_sha256: String,
    pub(crate) snapshot_sha256: String,
    pub(crate) byte_length: usize,
}
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct CodeDebugArtifactReference {
    pub(crate) schema_version: String,
    pub(crate) project_id: u64,
    pub(crate) bucket: String,
    pub(crate) name: String,
    pub(crate) media_type: String,
    pub(crate) byte_length: usize,
    pub(crate) sha256: String,
}
impl CodeDebugArtifactReference {
    pub(super) fn matches(&self, intent: &CodeDebugAdmission, project: i32) -> bool {
        self.schema_version == ARTIFACT_SCHEMA
            && u64::try_from(project).ok() == Some(self.project_id)
            && project > 0
            && self.bucket == "code-debug"
            && self.media_type == "application/json"
            && self.byte_length == intent.byte_length
            && self.sha256 == intent.snapshot_sha256
            && self.name.strip_suffix(".json").is_some_and(valid_digest)
    }
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum CodeDebugFailure {
    Denied,
    Unavailable,
}
#[async_trait]
pub(crate) trait CodeDebugArtifactSink: Send + Sync {
    /// Resolve live claim and original declaration before accepting binary bytes.
    /// Replay one durable staging/committed receipt after response loss.
    async fn export(
        &self,
        authority: &ClaimBoundSandboxAuthority,
        intent: &CodeDebugAdmission,
        snapshot: &[u8],
    ) -> Result<CodeDebugArtifactReference, CodeDebugFailure>;
}

#[allow(
    clippy::items_after_statements,
    reason = "Keep local protocol types beside their exact validation checks."
)]
pub(super) fn snapshot(
    invocation: &CodeInvocation<'_>,
    original_visit: &OriginalCodeVisitRef,
    original_activation: &[u8; 32],
    attempt: u16,
    request: &[u8; 32],
) -> Result<(CodeDebugAdmission, Vec<u8>), CodeDebugFailure> {
    if !original_visit.valid() || *original_activation == [0; 32] || !(1..=16).contains(&attempt) {
        return Err(CodeDebugFailure::Unavailable);
    }
    let selection = invocation
        .debug
        .as_ref()
        .ok_or(CodeDebugFailure::Unavailable)?;
    let pin = selection
        .original
        .as_ref()
        .ok_or(CodeDebugFailure::Unavailable)?;
    if invocation.source.len() > 256 * 1024
        || invocation.input_json.len() > 512 * 1024
        || selection.configuration_json.len() > 2 * 1024 * 1024
    {
        return Err(CodeDebugFailure::Unavailable);
    }
    let input =
        std::str::from_utf8(&invocation.input_json).map_err(|_| CodeDebugFailure::Unavailable)?;
    // RawValue preserves the exact input bytes, including meaningful integer values.
    let raw = RawValue::from_string(input.to_owned()).map_err(|_| CodeDebugFailure::Unavailable)?;
    if !input.trim_start().starts_with('{') {
        return Err(CodeDebugFailure::Unavailable);
    }
    #[derive(Serialize)]
    struct Snapshot<'a> {
        schema_version: &'static str,
        language: &'static str,
        source: &'a str,
        selected_input: &'a RawValue,
    }
    let bytes = serde_json::to_vec(&Snapshot {
        schema_version: SNAPSHOT_SCHEMA,
        language: language(invocation.language),
        source: invocation.source,
        selected_input: &raw,
    })
    .map_err(|_| CodeDebugFailure::Unavailable)?;
    if bytes.len() > MAX_SNAPSHOT_BYTES {
        return Err(CodeDebugFailure::Unavailable);
    }
    Ok((
        CodeDebugAdmission {
            original_visit: original_visit.clone(),
            attempt,
            schema_version: "elitea.runtime.code-debug-admission.v1",
            node_id: selection.node_id.clone(),
            graph_thread_id: selection.graph_thread_id.clone(),
            graph_step: selection.graph_step.clone(),
            activation_id: hex(original_activation),
            definition_sha256: hex(&pin.definition),
            yaml_sha256: hex(&pin.yaml),
            configuration_json: selection.configuration_json.clone(),
            request_sha256: hex(request),
            source_sha256: sha(invocation.source.as_bytes()),
            input_sha256: sha(&invocation.input_json),
            snapshot_sha256: sha(&bytes),
            byte_length: bytes.len(),
        },
        bytes,
    ))
}
pub(super) fn language(value: CodeLanguage) -> &'static str {
    match value {
        CodeLanguage::Python => "python",
        CodeLanguage::JavaScript => "javascript",
        CodeLanguage::TypeScript => "typescript",
        CodeLanguage::Rust => "rust",
    }
}
pub(super) fn hex(value: &[u8]) -> String {
    use std::fmt::Write as _;
    value
        .iter()
        .fold(String::with_capacity(value.len() * 2), |mut s, b| {
            let _ = write!(&mut s, "{b:02x}");
            s
        })
}
fn sha(value: &[u8]) -> String {
    hex(digest::digest(&digest::SHA256, value).as_ref())
}

/// Debug export and its trace are optional. Neither changes the Code result.
pub(super) async fn export(
    invocation: &CodeInvocation<'_>,
    original_visit: &OriginalCodeVisitRef,
    node_authority: &NodeAttemptAuthority,
    request: &[u8; 32],
    authority: &ClaimBoundSandboxAuthority,
    sink: Option<&dyn CodeDebugArtifactSink>,
    trace: Option<&super::code_trace::BoundCodeTrace<'_>>,
) {
    if invocation.debug.is_none() {
        return;
    }
    let original_activation = node_authority.logical_activation();
    let selection_matches = invocation.debug.as_ref().is_some_and(|selection| {
        let mut config = digest::Context::new(&digest::SHA256);
        config.update(b"elitea.graph.code.config.v1\0");
        config.update(selection.configuration_json.as_bytes());
        let mut node_digest = [0; 32];
        node_digest.copy_from_slice(config.finish().as_ref());
        selection
            .graph_step
            .parse::<usize>()
            .ok()
            .is_some_and(|step| node_authority.matches(&selection.node_id, node_digest, step))
    });
    let original_snapshot =
        if invocation.activation == node_authority.dispatch_activation() && selection_matches {
            snapshot(
                invocation,
                original_visit,
                &original_activation,
                node_authority.attempt(),
                request,
            )
        } else {
            Err(CodeDebugFailure::Denied)
        };
    let result = match (original_snapshot, sink) {
        (Ok((intent, bytes)), Some(sink)) => {
            match tokio::time::timeout(
                std::time::Duration::from_secs(5),
                sink.export(authority, &intent, &bytes),
            )
            .await
            {
                Ok(Ok(reference)) => {
                    let request = authority.request(&invocation.activation);
                    let project = request
                        .identity
                        .as_ref()
                        .and_then(|identity| identity.resource_project_id.parse::<i32>().ok());
                    if project.is_some_and(|p| reference.matches(&intent, p)) {
                        Ok(reference)
                    } else {
                        Err(CodeDebugFailure::Unavailable)
                    }
                }
                Ok(Err(error)) => Err(error),
                Err(_) => Err(CodeDebugFailure::Unavailable),
            }
        }
        (Err(error), _) => Err(error),
        (Ok(_), None) => Err(CodeDebugFailure::Unavailable),
    };
    if result.is_err() {
        tracing::warn!(
            error_code = "pipeline.code_debug_unavailable",
            "The Code debug artifact is unavailable."
        );
    }
    if let Some(trace) = trace
        && !matches!(
            tokio::time::timeout(
                std::time::Duration::from_secs(1),
                trace.debug(
                    original_visit,
                    node_authority.attempt(),
                    &original_activation,
                    request,
                    result
                )
            )
            .await,
            Ok(Ok(()))
        )
    {
        tracing::warn!(
            error_code = "pipeline.code_debug_trace_unavailable",
            "The Code debug artifact trace is unavailable."
        );
    }
}
#[cfg(test)]
#[path = "code_debug_tests.rs"]
mod tests;

pub(crate) const CODE_DEBUG_METADATA_KEY: &str = "elitea.code.debug.v1";
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct CodeDebugProof {
    pub(crate) revision: u8,
    pub(crate) original_visit: OriginalCodeVisitRef,
    pub(crate) attempt: u16,
    pub(crate) execution_id: String,
    pub(crate) generation: String,
    pub(crate) node_id: String,
    pub(crate) activation_id: String,
    pub(crate) request_sha256: String,
    pub(crate) status: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) artifact: Option<CodeDebugArtifactReference>,
}
impl CodeDebugProof {
    pub(crate) fn validate(&self) -> Result<(), adk_rust::graph::GraphError> {
        let number = self.generation.parse::<u64>().ok();
        if self.revision != 1
            || !self.original_visit.valid()
            || !(1..=16).contains(&self.attempt)
            || self.execution_id.is_empty()
            || self.execution_id.len() > 256
            || self.execution_id.chars().any(char::is_control)
            || number.is_none_or(|value| value == 0 || value.to_string() != self.generation)
            || !super::yaml::valid_graph_id(&self.node_id)
            || !valid_digest(&self.activation_id)
            || !valid_digest(&self.request_sha256)
            || !matches!(self.status.as_str(), "committed" | "denied" | "unavailable")
            || (self.status == "committed") != self.artifact.is_some()
        {
            return Err(adk_rust::graph::GraphError::Other(
                "Code debug trace is invalid".into(),
            ));
        }
        if let Some(artifact) = &self.artifact
            && (artifact.schema_version != ARTIFACT_SCHEMA
                || artifact.project_id == 0
                || artifact.project_id > 2_147_483_647
                || artifact.bucket != "code-debug"
                || artifact.media_type != "application/json"
                || artifact.byte_length == 0
                || artifact.byte_length > MAX_SNAPSHOT_BYTES
                || !valid_digest(&artifact.sha256)
                || !artifact
                    .name
                    .strip_suffix(".json")
                    .is_some_and(valid_digest))
        {
            return Err(adk_rust::graph::GraphError::Other(
                "Code debug reference is invalid".into(),
            ));
        }
        Ok(())
    }
    pub(crate) fn from_event(
        event: &adk_rust::Event,
    ) -> Result<Option<Self>, adk_rust::graph::GraphError> {
        let Some(raw) = event.provider_metadata.get(CODE_DEBUG_METADATA_KEY) else {
            return Ok(None);
        };
        if raw.len() > 4096
            || event.content().is_some()
            || !event.llm_response.partial
            || event.llm_response.turn_complete
            || !event.actions.state_delta.is_empty()
        {
            return Err(adk_rust::graph::GraphError::Other(
                "Code debug event is invalid".into(),
            ));
        }
        let proof: Self = serde_json::from_str(raw).map_err(|_| {
            adk_rust::graph::GraphError::Other("Code debug trace is invalid".into())
        })?;
        proof.validate()?;
        Ok(Some(proof))
    }
    pub(crate) fn run_id(&self) -> String {
        let mut hash = digest::Context::new(&digest::SHA256);
        hash.update(b"elitea.graph.code.debug-trace.v1\0");
        let visit_revision = self.original_visit.revision.to_string();
        let attempt = self.attempt.to_string();
        for field in [
            &self.execution_id,
            &self.generation,
            &self.original_visit.visit_id,
            &visit_revision,
            &self.original_visit.digest_sha256,
            &attempt,
            &self.activation_id,
            &self.request_sha256,
        ] {
            hash.update(&(field.len() as u64).to_be_bytes());
            hash.update(field.as_bytes());
        }
        format!("code-debug-{}", hex(hash.finish().as_ref()))
    }
}
fn valid_digest(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}
