//! Distinct running-owner evidence. This never grants Code execution or retry.
use super::{
    code_recovery::{CodeRecoveryError, WholeCodeBinding, canonical, hex, hex_id, sha256},
    compiled_snapshot::{Binding, ContentSha256, Control, Descriptor, Purpose},
    request::PreparedJob,
};
use serde::{Deserialize, Serialize};
#[derive(Clone, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub(crate) struct CodePlatformBinding {
    pub(crate) schema: String,
    pub(crate) prepared_job_sha256: String,
    pub(crate) prepared_fingerprint: String,
    pub(crate) policy_sha256: String,
    pub(crate) max_calls: u16,
    pub(crate) max_total_bytes: u32,
    #[serde(deserialize_with = "required_nullable")]
    pub(crate) compiled_execute: Option<CodePlatformCompiledExecute>,
}
#[derive(Clone, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub(crate) struct CodePlatformCompiledExecute {
    pub(crate) binding: Binding,
    pub(crate) snapshot_key_sha256: ContentSha256,
    pub(crate) selected_descriptor_sha256: ContentSha256,
}
impl CodePlatformCompiledExecute {
    fn control(&self) -> Control {
        Control {
            revision: 1,
            binding: self.binding.clone(),
            snapshot_key_sha256: self.snapshot_key_sha256.clone(),
            descriptor_sha256: Some(self.selected_descriptor_sha256.clone()),
        }
    }
    pub(crate) fn matches_request(&self, request: &str, prepared_fingerprint: &str) -> bool {
        let control = self.control();
        self.binding.base_prepared_request_sha256.as_str() == prepared_fingerprint
            && control.validate(Purpose::Execute).is_ok()
            && control
                .intent_digest(Purpose::Execute)
                .is_ok_and(|digest| hex(&digest) == request)
    }
}
impl CodePlatformBinding {
    pub(crate) fn from_job(
        job: &PreparedJob,
        whole: &WholeCodeBinding,
    ) -> Result<Option<Self>, CodeRecoveryError> {
        Self::from_selected_job(job, whole, None)
    }
    pub(crate) fn from_compiled_job(
        job: &PreparedJob,
        whole: &WholeCodeBinding,
        control: &Control,
        descriptor_wire: &[u8],
    ) -> Result<Option<Self>, CodeRecoveryError> {
        control
            .validate(Purpose::Execute)
            .map_err(|_| CodeRecoveryError)?;
        control
            .binding
            .validate_job(job)
            .map_err(|_| CodeRecoveryError)?;
        let descriptor: Descriptor =
            serde_json::from_slice(descriptor_wire).map_err(|_| CodeRecoveryError)?;
        descriptor
            .validate(control)
            .map_err(|_| CodeRecoveryError)?;
        if descriptor.bytes().map_err(|_| CodeRecoveryError)? != descriptor_wire
            || control.descriptor_sha256.as_ref() != Some(&ContentSha256::of(descriptor_wire))
        {
            return Err(CodeRecoveryError);
        }
        let selected = CodePlatformCompiledExecute {
            binding: control.binding.clone(),
            snapshot_key_sha256: control.snapshot_key_sha256.clone(),
            selected_descriptor_sha256: ContentSha256::of(descriptor_wire),
        };
        Self::from_selected_job(job, whole, Some(selected))
    }
    fn from_selected_job(
        job: &PreparedJob,
        whole: &WholeCodeBinding,
        compiled_execute: Option<CodePlatformCompiledExecute>,
    ) -> Result<Option<Self>, CodeRecoveryError> {
        let Some(platform) = job.platform_client() else {
            return Ok(None);
        };
        platform.validate().map_err(|_| CodeRecoveryError)?;
        let bytes = job.to_transport().map_err(|_| CodeRecoveryError)?;
        let result = Self {
            schema: "elitea.sandbox.code-platform-binding.v1".into(),
            prepared_job_sha256: sha256(&bytes),
            prepared_fingerprint: hex(&job.fingerprint().map_err(|_| CodeRecoveryError)?),
            policy_sha256: platform.policy_sha256().into(),
            max_calls: platform.max_calls(),
            max_total_bytes: platform.max_total_bytes(),
            compiled_execute,
        };
        if !result.matches(whole) {
            return Err(CodeRecoveryError);
        }
        Ok(Some(result))
    }
    pub(crate) fn matches(&self, whole: &WholeCodeBinding) -> bool {
        self.schema == "elitea.sandbox.code-platform-binding.v1"
            && whole.valid()
            && self.prepared_job_sha256 == whole.prepared_job_sha256
            && match &self.compiled_execute {
                None => self.prepared_fingerprint == whole.request_digest,
                Some(selected) => {
                    whole.language == "rust"
                        && selected.binding.source_sha256.as_str() == whole.source_sha256
                        && selected
                            .matches_request(&whole.request_digest, &self.prepared_fingerprint)
                }
            }
            && hex_id(&self.policy_sha256, 64, false)
            && (1..=4096).contains(&self.max_calls)
            && (1..=64 * 1024 * 1024).contains(&self.max_total_bytes)
    }
    pub(crate) fn launch(&self, runtime: &str) -> Result<Vec<u8>, CodeRecoveryError> {
        // Preserve original plain launch bytes. A compiled runtime identity owns
        // the Execute intent digest, which is distinct from its prepared fingerprint.
        let mut launch = serde_json::json!({"revision":1,"retained_runtime_id":runtime,
            "prepared_sha256":self.prepared_fingerprint,"policy_sha256":self.policy_sha256,
            "max_calls":self.max_calls,"max_total_bytes":self.max_total_bytes});
        if let Some(selected) = &self.compiled_execute {
            let request = selected
                .control()
                .intent_digest(Purpose::Execute)
                .map_err(|_| CodeRecoveryError)?;
            let request = hex(&request);
            if !selected.matches_request(&request, &self.prepared_fingerprint) {
                return Err(CodeRecoveryError);
            }
            launch["revision"] = 2.into();
            launch["request_digest"] = request.into();
        }
        canonical(&launch)
    }
}
#[derive(Clone, Copy, Debug, Serialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
pub enum RetainedRuntimeKind {
    Docker,
    Kubernetes,
}
#[derive(Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct RetainedCodePlatformRuntime {
    pub(crate) kind: RetainedRuntimeKind,
    pub(crate) runtime_id: String,
    pub(crate) owner_epoch: i64,
    pub(crate) binding_sha256: String,
    pub(crate) prepared_job_sha256: String,
    pub(crate) prepared_fingerprint: String,
    pub(crate) policy_sha256: String,
    pub(crate) max_calls: u16,
    pub(crate) max_total_bytes: u32,
    pub(crate) lifecycle: &'static str,
    pub(crate) compiled_execute: Option<CodePlatformCompiledExecute>,
}
fn required_nullable<'de, D, T>(d: D) -> Result<Option<T>, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Deserialize<'de>,
{
    Option::<T>::deserialize(d)
}
pub(crate) enum CodePlatformOwnerObservation {
    NotReady,
    Completed,
    Completing,
    Running(Box<CodePlatformOwnerResponse>),
}
#[derive(Clone, Eq, PartialEq)]
pub(crate) struct OwnedCodePlatformRuntime {
    pub(crate) runtime_id: String,
    pub(crate) owner: String,
    pub(crate) epoch: i64,
    pub(crate) binding: WholeCodeBinding,
    pub(crate) broker: CodePlatformBinding,
}
#[allow(
    clippy::large_enum_variant,
    reason = "Keep bounded authenticated owner records inline without changing their contracts."
)]
pub(crate) enum CodePlatformLedgerObservation {
    NotReady,
    Completed,
    Running(OwnedCodePlatformRuntime),
}
#[derive(Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct CodePlatformOwnerResponse {
    pub(crate) schema: &'static str,
    pub(crate) state: &'static str,
    pub(crate) runtime: RetainedCodePlatformRuntime,
    pub(crate) pending_call_base64url: Option<String>,
    pub(crate) reply_published: Option<bool>,
}
