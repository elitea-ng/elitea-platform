//! Fixed Rust snapshot helpers for a bound original container.
use super::*;
use tokio::io::{AsyncRead, AsyncWrite};
fn failed() -> SandboxError {
    SandboxError::ExecutionFailed("Rust snapshot transfer requires the original runtime".into())
}
fn launch_values(
    image: &str,
    purpose: &str,
    hash: &str,
    policy: &str,
) -> Result<Vec<String>, SandboxError> {
    if !immutable_image_reference(image)
        || !matches!(purpose, "compile" | "execute")
        || hash.len() != 64
        || !hash
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        || policy.is_empty()
        || policy.len() > 128
        || !policy
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'))
    {
        return Err(failed());
    }
    let digest = image.rsplit_once('@').map_or(image, |(_, digest)| digest);
    Ok(vec![
        "ELITEA_COMPILED_CODE_SNAPSHOTS=1".into(),
        format!("ELITEA_COMPILED_CODE_JOB_PURPOSE={purpose}"),
        format!("ELITEA_COMPILED_CODE_CONTROL_SHA256={hash}"),
        format!("ELITEA_COMPILED_CODE_IMAGE_DIGEST={digest}"),
        format!("ELITEA_COMPILED_CODE_POLICY_REVISION={policy}"),
    ])
}
fn validate_launch(
    info: &bollard::models::ContainerInspectResponse,
    identity: &CodeJobIdentity,
    image_id: &str,
    expected: &[String],
) -> Result<(), SandboxError> {
    let config = info.config.as_ref().ok_or_else(failed)?;
    let labels = config.labels.as_ref().ok_or_else(failed)?;
    let env = config.env.as_ref().ok_or_else(failed)?;
    if identity.runtime_id().is_none()
        || info.id.as_deref() != identity.runtime_id()
        || info.image.as_deref() != Some(image_id)
        || labels.get("io.elitea.code.job").map(String::as_str) != Some(identity.job_key())
        || labels.get("io.elitea.code.request").map(String::as_str)
            != Some(identity.request_digest())
    {
        return Err(failed());
    }
    for binding in expected {
        let name = binding.split_once('=').ok_or_else(failed)?.0;
        if env
            .iter()
            .filter(|value| value.split_once('=').is_some_and(|(key, _)| key == name))
            .count()
            != 1
            || !env.contains(binding)
        {
            return Err(failed());
        }
    }
    Ok(())
}
impl DockerClient {
    /// Provision once with immutable PID 1 snapshot authority selected by the supervisor.
    pub async fn provision_compiled_code_job(
        &self,
        identity: &CodeJobIdentity,
        manifest: &Manifest,
        purpose: &str,
        control_sha256: &str,
        policy_revision: &str,
    ) -> Result<SessionHandle, SandboxError> {
        self.provision_compiled_code_job_with_repository(
            identity,
            manifest,
            purpose,
            control_sha256,
            policy_revision,
            None,
        )
        .await
    }

    pub async fn provision_compiled_code_job_with_repository(
        &self,
        identity: &CodeJobIdentity,
        manifest: &Manifest,
        purpose: &str,
        control_sha256: &str,
        policy_revision: &str,
        repository: Option<&super::CodeRepositoryMount>,
    ) -> Result<SessionHandle, SandboxError> {
        if !self.code_job_policy
            || !self.code_compilation_enabled()
            || !matches!(purpose, "compile" | "execute")
            || control_sha256.len() != 64
            || !control_sha256
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
            || self.observe_code_job(identity).await?.is_some()
        {
            return Err(failed());
        }
        let launch = launch_values(&self.base_image, purpose, control_sha256, policy_revision)?;
        for entry in &manifest.entries {
            let path = match entry {
                ManifestEntry::File { path, .. }
                | ManifestEntry::Directory { path }
                | ManifestEntry::GitRepo { path, .. } => path,
            };
            if path != ".elitea-code.json" && path != ".elitea-job.json" {
                return Err(failed());
            }
        }
        self.provision_with_repository(manifest, Some(identity), Some(launch), repository)
            .await
    }
    /// Validate the original configured image and immutable PID 1 environment after reconnect.
    pub async fn validate_compiled_code_job_launch(
        &self,
        identity: &CodeJobIdentity,
        purpose: &str,
        control_sha256: &str,
        policy_revision: &str,
    ) -> Result<(), SandboxError> {
        if !self.code_compilation_enabled() {
            return Err(failed());
        }
        let expected = launch_values(&self.base_image, purpose, control_sha256, policy_revision)?;
        let image_id = self.check_code_image_ready().await?;
        let runtime_id = identity.runtime_id().ok_or_else(failed)?;
        let info = tokio::time::timeout(
            Duration::from_secs(10),
            self.client.inspect_container(runtime_id, None),
        )
        .await
        .map_err(|_| failed())?
        .map_err(|_| failed())?;
        validate_launch(&info, identity, &image_id, &expected)
    }
    /// Probe length only. The status helper supplies the subsequent quiet export proof.
    pub async fn compiled_descriptor_probe(
        &self,
        identity: &CodeJobIdentity,
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        if identity.runtime_id().is_none() {
            return Err(failed());
        }
        let job = self.observe_code_job(identity).await?.ok_or_else(failed)?;
        if !job.running {
            return Ok(None);
        }
        let (out, err, status) = tokio::time::timeout(
            Duration::from_secs(10),
            self.exec_in_container(
                &job.container_id,
                vec!["cat", "/workspace/compiled-snapshot/descriptor.json"],
                None,
                None,
            ),
        )
        .await
        .map_err(|_| failed())??;
        self.observe_code_job(identity).await?.ok_or_else(failed)?;
        if status == 1 {
            return Ok(None);
        }
        if status != 0 || !err.is_empty() || out.len() > 16 * 1024 {
            return Err(failed());
        }
        Ok(Some(out.into_bytes()))
    }
    /// Transfer fixed roles only; header identity is independently checked by PID 1.
    #[allow(clippy::too_many_arguments)]
    pub async fn compiled_code_transfer(
        &self,
        identity: &CodeJobIdentity,
        command: &str,
        header: &[u8],
        bytes: u64,
        reader: &mut (dyn AsyncRead + Unpin + Send),
        writer: &mut (dyn AsyncWrite + Unpin + Send),
        importing: bool,
    ) -> Result<(), SandboxError> {
        if header.len() > 4096
            || bytes > 32 * 1024 * 1024
            || !matches!(
                command,
                "--compiled-control-write"
                    | "--compiled-artifact-write"
                    | "--compiled-artifact-finalize"
                    | "--compiled-artifact-status"
                    | "--compiled-artifact-read"
                    | "--compiled-artifact-release"
            )
            || importing
                != matches!(
                    command,
                    "--compiled-control-write" | "--compiled-artifact-write"
                )
        {
            return Err(failed());
        }
        self.dependency_transfer(identity, command, header, bytes, reader, writer, importing)
            .await
    }
}

#[cfg(test)]
mod tests {
    use super::{CodeJobIdentity, launch_values, validate_launch};
    use bollard::models::ContainerInspectResponse;
    use serde_json::json;

    fn fixture() -> (
        CodeJobIdentity,
        ContainerInspectResponse,
        Vec<String>,
        String,
    ) {
        let identity = CodeJobIdentity::new("a".repeat(64), "b".repeat(64))
            .unwrap()
            .with_runtime_id("original".into())
            .unwrap();
        let image = format!("sha256:{}", "c".repeat(64));
        let env = launch_values(
            &format!("registry/runner@{image}"),
            "compile",
            &"d".repeat(64),
            "rust-v1",
        )
        .unwrap();
        let info = serde_json::from_value(json!({"Id":"original", "Image":image,
            "Config":{"Labels":{"io.elitea.code.job":identity.job_key(), "io.elitea.code.request":identity.request_digest()}, "Env":env}
        })).unwrap();
        (identity, info, env, image)
    }

    #[test]
    fn launch_binds_backend_digest_and_configured_policy_for_both_roles() {
        for purpose in ["compile", "execute"] {
            let env = launch_values(
                &format!("registry/runner@sha256:{}", "a".repeat(64)),
                purpose,
                &"b".repeat(64),
                "rust-v2",
            )
            .unwrap();
            assert_eq!(env.len(), 5);
            assert!(env.contains(&format!(
                "ELITEA_COMPILED_CODE_IMAGE_DIGEST=sha256:{}",
                "a".repeat(64)
            )));
            assert!(env.contains(&"ELITEA_COMPILED_CODE_POLICY_REVISION=rust-v2".into()));
        }
        for policy in ["", "x y", "x\n", "x=override"] {
            assert!(
                launch_values(
                    &format!("sha256:{}", "a".repeat(64)),
                    "compile",
                    &"b".repeat(64),
                    policy
                )
                .is_err()
            );
        }
        assert!(launch_values("runner:latest", "compile", &"b".repeat(64), "rust-v1").is_err());
    }

    #[test]
    fn recovery_rejects_missing_duplicate_or_changed_launch_bindings() {
        let (identity, info, expected, image) = fixture();
        assert!(validate_launch(&info, &identity, &image, &expected).is_ok());
        for index in 0..expected.len() {
            let mut missing = info.clone();
            missing
                .config
                .as_mut()
                .unwrap()
                .env
                .as_mut()
                .unwrap()
                .remove(index);
            assert!(validate_launch(&missing, &identity, &image, &expected).is_err());
            let mut duplicate = info.clone();
            duplicate
                .config
                .as_mut()
                .unwrap()
                .env
                .as_mut()
                .unwrap()
                .push(expected[index].clone());
            assert!(validate_launch(&duplicate, &identity, &image, &expected).is_err());
            let mut changed = info.clone();
            changed.config.as_mut().unwrap().env.as_mut().unwrap()[index].push('x');
            assert!(validate_launch(&changed, &identity, &image, &expected).is_err());
        }
    }

    #[test]
    fn recovery_rejects_replacement_runtime_image_and_original_request_drift() {
        let (identity, info, expected, image) = fixture();
        let mut changed = info.clone();
        changed.id = Some("replacement".into());
        assert!(validate_launch(&changed, &identity, &image, &expected).is_err());
        let mut changed = info.clone();
        changed.image = Some(format!("sha256:{}", "e".repeat(64)));
        assert!(validate_launch(&changed, &identity, &image, &expected).is_err());
        let mut changed = info.clone();
        changed
            .config
            .as_mut()
            .unwrap()
            .labels
            .as_mut()
            .unwrap()
            .insert("io.elitea.code.request".into(), "e".repeat(64));
        assert!(validate_launch(&changed, &identity, &image, &expected).is_err());
    }
}
