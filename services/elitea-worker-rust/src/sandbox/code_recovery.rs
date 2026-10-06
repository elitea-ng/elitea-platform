//! Immutable whole-Code owner evidence. It never dispatches a runtime.

use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use ring::digest;
use serde::{Deserialize, Serialize};

pub(crate) const CODE_RESULT_AUDIENCE: &str = "elitea.runtime.code-sandbox-whole-result.v1";
pub(crate) const NO_EFFECT_FAILURE_CODE: &str = "recovery_verified_no_effect";
pub(crate) const MAX_CODE_RECOVERY_WIRE: usize = 1024 * 1024;

/// Exact compiler-emitted declaration bytes. Main independently checks the saved source.
pub(crate) struct OriginalCodeDeclarationInput<'a> {
    pub(crate) node_id: &'a str,
    pub(crate) graph_thread: &'a str,
    pub(crate) step: u64,
    pub(crate) configuration_json: &'a str,
    pub(crate) owning_yaml_sha256: [u8; 32],
}

#[derive(Clone, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub(crate) struct OriginalCodeVisitRef {
    pub(crate) visit_id: String,
    pub(crate) revision: u64,
    pub(crate) digest_sha256: String,
}
impl OriginalCodeVisitRef {
    pub(crate) fn valid(&self) -> bool {
        self.revision == 1
            && hex_id(&self.visit_id, 64, true)
            && hex_id(&self.digest_sha256, 64, true)
    }
}
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct SignedCodeEnvelope {
    pub(crate) schema: String,
    pub(crate) key_id: String,
    pub(crate) claims_base64url: String,
    pub(crate) signature_base64url: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub(crate) struct WholeCodeBinding {
    pub(crate) schema: String,
    pub(crate) purpose: String,
    pub(crate) execution_id: String,
    pub(crate) original_generation: u64,
    pub(crate) dispatch_activation: String,
    pub(crate) job_key: String,
    pub(crate) request_digest: String,
    pub(crate) supervisor_audience: String,
    pub(crate) node_digest: String,
    pub(crate) activation_id: String,
    pub(crate) node_id: String,
    pub(crate) graph_thread: String,
    pub(crate) step: u64,
    pub(crate) attempt: u16,
    pub(crate) language: String,
    pub(crate) prepared_job_sha256: String,
    pub(crate) source_sha256: String,
    pub(crate) input_sha256: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub(crate) struct CodeRecoveryVisit {
    pub(crate) activation_id: String,
    pub(crate) node_id: String,
    pub(crate) graph_thread: String,
    pub(crate) step: u64,
    pub(crate) attempt: u16,
    pub(crate) expected_revision: u64,
    pub(crate) receipt_sha256: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub(crate) enum WholeCodeRecoveryReceipt {
    CommittedResult {
        schema: String,
        binding: WholeCodeBinding,
        visit: CodeRecoveryVisit,
        result_json_base64url: String,
        result_sha256: String,
    },
    VerifiedNoEffect {
        schema: String,
        binding: WholeCodeBinding,
        visit: CodeRecoveryVisit,
        seal_id: String,
    },
}

#[derive(Debug, thiserror::Error)]
#[error("the whole-Code owner evidence is invalid")]
pub(crate) struct CodeRecoveryError;

impl WholeCodeBinding {
    pub(crate) fn canonical_bytes(&self) -> Result<Vec<u8>, CodeRecoveryError> {
        if !self.valid() {
            return Err(CodeRecoveryError);
        }
        canonical(self)
    }
    pub(crate) fn valid(&self) -> bool {
        self.schema == "elitea.sandbox.whole-code-binding.v1"
            && self.purpose == "whole_code_execute"
            && hex_id(&self.execution_id, 32, false)
            && (1..=i64::MAX as u64).contains(&self.original_generation)
            && hex_id(&self.dispatch_activation, 64, true)
            && hex_id(&self.job_key, 64, true)
            && self.job_key == original_job_key(&self.execution_id, &self.dispatch_activation)
            && hex_id(&self.request_digest, 64, true)
            && identity(&self.supervisor_audience)
            && hex_id(&self.node_digest, 64, true)
            && valid_original_visit(
                &self.activation_id,
                &self.node_id,
                &self.graph_thread,
                self.step,
                self.attempt,
            )
            && matches!(
                self.language.as_str(),
                "python" | "javascript" | "typescript" | "rust"
            )
            && hex_id(&self.prepared_job_sha256, 64, true)
            && hex_id(&self.source_sha256, 64, true)
            && hex_id(&self.input_sha256, 64, true)
    }
}
impl CodeRecoveryVisit {
    pub(crate) fn valid(&self) -> bool {
        valid_original_visit(
            &self.activation_id,
            &self.node_id,
            &self.graph_thread,
            self.step,
            self.attempt,
        ) && (1..=i64::MAX as u64).contains(&self.expected_revision)
            && hex_id(&self.receipt_sha256, 64, true)
    }
}
pub(crate) fn valid_original_visit(
    activation: &str,
    node: &str,
    thread: &str,
    step: u64,
    attempt: u16,
) -> bool {
    hex_id(activation, 64, true)
        && !node.is_empty()
        && node.len() <= 128
        && node
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'-' | b'.' | b':'))
        && !thread.is_empty()
        && thread.len() <= 512
        && !thread
            .chars()
            .any(|c| c.is_control() || matches!(c, '\u{2028}' | '\u{2029}'))
        && i64::try_from(step).is_ok()
        && (1..=16).contains(&attempt)
}
impl WholeCodeBinding {
    pub(crate) fn matches_original_visit(&self, visit: &CodeRecoveryVisit) -> bool {
        self.activation_id == visit.activation_id
            && self.node_id == visit.node_id
            && self.graph_thread == visit.graph_thread
            && self.step == visit.step
            && self.attempt == visit.attempt
    }
}
impl WholeCodeRecoveryReceipt {
    pub(crate) fn committed(
        binding: WholeCodeBinding,
        visit: CodeRecoveryVisit,
        bytes: &[u8],
    ) -> Result<Self, CodeRecoveryError> {
        if !binding.valid()
            || !visit.valid()
            || !binding.matches_original_visit(&visit)
            || !whole_result(bytes)
        {
            return Err(CodeRecoveryError);
        }
        let receipt = Self::CommittedResult {
            schema: "elitea.sandbox.whole-code-recovery-receipt.v1".into(),
            binding,
            visit,
            result_json_base64url: URL_SAFE_NO_PAD.encode(bytes),
            result_sha256: sha256(bytes),
        };
        receipt.canonical_bytes()?;
        Ok(receipt)
    }
    pub(crate) fn sealed_no_effect(
        binding: WholeCodeBinding,
        visit: CodeRecoveryVisit,
    ) -> Result<Self, CodeRecoveryError> {
        if !binding.valid() || !visit.valid() || !binding.matches_original_visit(&visit) {
            return Err(CodeRecoveryError);
        }
        let mut context = digest::Context::new(&digest::SHA256);
        context.update(b"elitea.sandbox.whole-code-no-effect-seal.v1\0");
        let exact = canonical(&(binding.clone(), visit.clone()))?;
        context.update(&(exact.len() as u64).to_be_bytes());
        context.update(&exact);
        Ok(Self::VerifiedNoEffect {
            schema: "elitea.sandbox.whole-code-recovery-receipt.v1".into(),
            binding,
            visit,
            seal_id: hex(context.finish().as_ref()),
        })
    }
    pub(crate) fn parse(bytes: &[u8]) -> Result<Self, CodeRecoveryError> {
        if bytes.is_empty() || bytes.len() > MAX_CODE_RECOVERY_WIRE {
            return Err(CodeRecoveryError);
        }
        let receipt: Self = serde_json::from_slice(bytes).map_err(|_| CodeRecoveryError)?;
        if receipt.canonical_bytes()?.as_slice() != bytes {
            return Err(CodeRecoveryError);
        }
        match &receipt {
            Self::CommittedResult {
                schema,
                binding,
                visit,
                ..
            }
            | Self::VerifiedNoEffect {
                schema,
                binding,
                visit,
                ..
            } if schema == "elitea.sandbox.whole-code-recovery-receipt.v1"
                && binding.valid()
                && visit.valid()
                && binding.matches_original_visit(visit) => {}
            _ => return Err(CodeRecoveryError),
        }
        match &receipt {
            Self::CommittedResult { .. } => {
                receipt.result_bytes()?;
            }
            Self::VerifiedNoEffect { binding, visit, .. } => {
                if Self::sealed_no_effect(binding.clone(), visit.clone())? != receipt {
                    return Err(CodeRecoveryError);
                }
            }
        }
        Ok(receipt)
    }
    pub(crate) fn binding(&self) -> &WholeCodeBinding {
        match self {
            Self::CommittedResult { binding, .. } | Self::VerifiedNoEffect { binding, .. } => {
                binding
            }
        }
    }
    pub(crate) fn visit(&self) -> &CodeRecoveryVisit {
        match self {
            Self::CommittedResult { visit, .. } | Self::VerifiedNoEffect { visit, .. } => visit,
        }
    }
    pub(crate) fn result_bytes(&self) -> Result<Vec<u8>, CodeRecoveryError> {
        let Self::CommittedResult {
            result_json_base64url,
            result_sha256,
            ..
        } = self
        else {
            return Err(CodeRecoveryError);
        };
        if result_json_base64url.is_empty() || result_json_base64url.len() > 699_051 {
            return Err(CodeRecoveryError);
        }
        let bytes = URL_SAFE_NO_PAD
            .decode(result_json_base64url)
            .map_err(|_| CodeRecoveryError)?;
        if !whole_result(&bytes)
            || URL_SAFE_NO_PAD.encode(&bytes) != *result_json_base64url
            || sha256(&bytes) != *result_sha256
        {
            return Err(CodeRecoveryError);
        }
        Ok(bytes)
    }
    pub(crate) fn canonical_bytes(&self) -> Result<Vec<u8>, CodeRecoveryError> {
        let bytes = canonical(self)?;
        if bytes.len() > MAX_CODE_RECOVERY_WIRE {
            return Err(CodeRecoveryError);
        }
        Ok(bytes)
    }
}
/// Establish whole execution completion; the original Code definition still
/// owns business output selection and type admission.
pub(crate) fn whole_result(bytes: &[u8]) -> bool {
    #[derive(Deserialize)]
    #[serde(deny_unknown_fields)]
    struct ResultReceipt {
        revision: u8,
        status: String,
        exit_code: Option<i32>,
        stdout: String,
        stderr: String,
    }
    #[derive(Deserialize)]
    #[serde(deny_unknown_fields)]
    struct AdapterResult {
        revision: u8,
        result: serde_json::Value,
    }
    if bytes.is_empty() || bytes.len() > 512 * 1024 {
        return false;
    }
    let Ok(receipt) = serde_json::from_slice::<ResultReceipt>(bytes) else {
        return false;
    };
    if receipt.revision != 1
        || receipt.status != "completed"
        || receipt.exit_code != Some(0)
        || receipt.stdout.len() > 256 * 1024 + 64
    {
        return false;
    }
    drop(receipt.stderr);
    let Ok(adapter) = serde_json::from_str::<AdapterResult>(&receipt.stdout) else {
        return false;
    };
    adapter.revision == 1
        && serde_json::to_vec(&adapter.result).is_ok_and(|bytes| bytes.len() <= 256 * 1024)
}
pub(crate) fn canonical(value: &impl Serialize) -> Result<Vec<u8>, CodeRecoveryError> {
    let mut value = serde_json::to_value(value).map_err(|_| CodeRecoveryError)?;
    value.sort_all_objects();
    serde_json::to_vec(&value).map_err(|_| CodeRecoveryError)
}
pub(crate) fn original_job_key(execution: &str, activation: &str) -> String {
    let mut hash = digest::Context::new(&digest::SHA256);
    hash.update(b"elitea.sandbox.activation.v1\0");
    for part in [execution, activation] {
        hash.update(&(part.len() as u64).to_be_bytes());
        hash.update(part.as_bytes());
    }
    hex(hash.finish().as_ref())
}
pub(crate) fn sha256(bytes: &[u8]) -> String {
    hex(digest::digest(&digest::SHA256, bytes).as_ref())
}
pub(crate) fn hex(bytes: &[u8]) -> String {
    const DIGITS: &[u8; 16] = b"0123456789abcdef";
    bytes
        .iter()
        .flat_map(|byte| {
            [
                char::from(DIGITS[usize::from(byte >> 4)]),
                char::from(DIGITS[usize::from(byte & 0x0f)]),
            ]
        })
        .collect()
}
pub(crate) fn hex_id(value: &str, len: usize, nonzero: bool) -> bool {
    value.len() == len
        && value
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        && (!nonzero || !value.bytes().all(|b| b == b'0'))
}
pub(crate) fn identity(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 256
        && !value.chars().any(|c| c.is_control() || c.is_whitespace())
}

#[cfg(test)]
mod tests {
    use super::*;
    fn fixture() -> (WholeCodeBinding, CodeRecoveryVisit) {
        let execution = "1".repeat(32);
        let activation = "2".repeat(64);
        (
            WholeCodeBinding {
                schema: "elitea.sandbox.whole-code-binding.v1".into(),
                purpose: "whole_code_execute".into(),
                job_key: original_job_key(&execution, &activation),
                execution_id: execution,
                original_generation: 1,
                dispatch_activation: activation,
                request_digest: "3".repeat(64),
                supervisor_audience: "spiffe://elitea/sandbox/original".into(),
                node_digest: "4".repeat(64),
                activation_id: "6".repeat(64),
                node_id: "run".into(),
                graph_thread: "root".into(),
                step: 7,
                attempt: 1,
                language: "python".into(),
                prepared_job_sha256: "5".repeat(64),
                source_sha256: sha256(b"41"),
                input_sha256: sha256(b"{}"),
            },
            CodeRecoveryVisit {
                activation_id: "6".repeat(64),
                node_id: "run".into(),
                graph_thread: "root".into(),
                step: 7,
                attempt: 1,
                expected_revision: 2,
                receipt_sha256: "7".repeat(64),
            },
        )
    }
    #[test]
    fn whole_code_receipt_preserves_exact_original_bytes_and_separate_domain() {
        let (binding, visit) = fixture();
        let bytes = br#"{ "revision":1,"status":"completed","exit_code":0,"stdout":"{\"revision\":1,\"result\":41}","stderr":"" }"#;
        let record =
            WholeCodeRecoveryReceipt::committed(binding.clone(), visit.clone(), bytes).unwrap();
        let exact = record.canonical_bytes().unwrap();
        assert_eq!(
            WholeCodeRecoveryReceipt::parse(&exact)
                .unwrap()
                .result_bytes()
                .unwrap(),
            bytes
        );
        let seal = WholeCodeRecoveryReceipt::sealed_no_effect(binding, visit).unwrap();
        assert!(seal.result_bytes().is_err());
        assert_ne!(seal.canonical_bytes().unwrap(), exact);
    }
    #[test]
    fn whole_code_receipt_rejects_changed_hash_noncanonical_wire_and_wrong_purpose() {
        let (binding, visit) = fixture();
        let record =
            WholeCodeRecoveryReceipt::committed(binding.clone(), visit.clone(), br#"{"revision":1,"status":"completed","exit_code":0,"stdout":"{\"revision\":1,\"result\":41}","stderr":""}"#).unwrap();
        let mut value: serde_json::Value =
            serde_json::from_slice(&record.canonical_bytes().unwrap()).unwrap();
        value["result_sha256"] = serde_json::json!("8".repeat(64));
        assert!(WholeCodeRecoveryReceipt::parse(&canonical(&value).unwrap()).is_err());
        let mut spaced = record.canonical_bytes().unwrap();
        spaced.push(b'\n');
        assert!(WholeCodeRecoveryReceipt::parse(&spaced).is_err());
        let mut wrong = binding;
        wrong.purpose = "platform_call".into();
        assert!(WholeCodeRecoveryReceipt::committed(wrong, visit, b"{}").is_err());
    }
}
