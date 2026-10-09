//! Job-owned immutable repository volumes. Only the trusted supervisor uses this API.
use super::*;
use bollard::models::{Mount, MountTypeEnum, MountVolumeOptions};
use bollard::volume::{CreateVolumeOptions, RemoveVolumeOptions};

const REPOSITORY: &str = "/workspace/repository";
const ROOT_LABEL: &str = "io.elitea.code.workspace.root";
const CREATED_LABEL: &str = "io.elitea.code.workspace.volume-created";
const ROLE_LABEL: &str = "io.elitea.code.workspace.role";
const OWNER_LABEL: &str = "io.elitea.code.workspace.owner";

#[derive(Clone)]
pub struct CodeRepositoryMount {
    root: String,
    volume: String,
    created: String,
    owner: String,
    image: String,
}
fn failed() -> SandboxError {
    SandboxError::ExecutionFailed(
        "Code repository volume identity or hydration is unconfirmed".into(),
    )
}
fn valid(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|v| v.is_ascii_digit() || (b'a'..=b'f').contains(&v))
}
fn helper_name(identity: &CodeJobIdentity) -> String {
    format!("elitea-code-repository-{}", identity.job_key())
}
fn volume_name(identity: &CodeJobIdentity) -> String {
    format!("elitea-code-repository-{}", identity.job_key())
}
impl CodeRepositoryMount {
    pub fn root(&self) -> &str {
        &self.root
    }
    pub fn volume_name(&self) -> &str {
        &self.volume
    }
    pub fn creation_token(&self) -> &str {
        &self.created
    }
    pub fn owner_token(&self) -> &str {
        &self.owner
    }
    pub(super) fn mount(&self, readonly: bool) -> Mount {
        Mount {
            target: Some(REPOSITORY.into()),
            source: Some(self.volume.clone()),
            typ: Some(MountTypeEnum::VOLUME),
            read_only: Some(readonly),
            volume_options: Some(MountVolumeOptions {
                no_copy: Some(false),
                ..Default::default()
            }),
            ..Default::default()
        }
    }
    pub(super) fn labels(&self, identity: &CodeJobIdentity, role: &str) -> HashMap<String, String> {
        HashMap::from([
            ("io.elitea.code.job".into(), identity.job_key().into()),
            (
                "io.elitea.code.request".into(),
                identity.request_digest().into(),
            ),
            (ROOT_LABEL.into(), self.root.clone()),
            (CREATED_LABEL.into(), self.created.clone()),
            (OWNER_LABEL.into(), self.owner.clone()),
            (ROLE_LABEL.into(), role.into()),
        ])
    }
    pub(super) fn environment(&self, identity: &CodeJobIdentity) -> Vec<String> {
        vec![
            format!("ELITEA_CODE_WORKSPACE_JOB_KEY={}", identity.job_key()),
            format!(
                "ELITEA_CODE_WORKSPACE_REQUEST_DIGEST={}",
                identity.request_digest()
            ),
            format!("ELITEA_CODE_WORKSPACE_ROOT_SHA256={}", self.root),
        ]
    }
}

impl DockerClient {
    /// Existence is not deletion authority. Keep orphan cleanup pending when
    /// the original job has no immutable volume provenance.
    pub async fn code_repository_volume_exists(
        &self,
        identity: &CodeJobIdentity,
    ) -> Result<bool, SandboxError> {
        match self.client.inspect_volume(&volume_name(identity)).await {
            Ok(_) => Ok(true),
            Err(bollard::errors::Error::DockerResponseServerError {
                status_code: 404, ..
            }) => Ok(false),
            Err(_) => Err(failed()),
        }
    }

    /// Create only a fixed named local volume. There is no user-selected host path.
    /// The caller holds the current Reserved job lease before calling this method.
    pub async fn prepare_code_repository(
        &self,
        identity: &CodeJobIdentity,
        root: &str,
    ) -> Result<CodeRepositoryMount, SandboxError> {
        if !self.code_job_policy || identity.runtime_id().is_some() || !valid(root) {
            return Err(failed());
        }
        self.validate_code_job_policy()?;
        let image = self.check_code_image_ready().await?;
        let volume = volume_name(identity);
        self.client
            .create_volume(CreateVolumeOptions {
                name: volume.clone(),
                driver: "local".into(),
                driver_opts: HashMap::new(),
                labels: HashMap::from([
                    ("io.elitea.code.job".into(), identity.job_key().into()),
                    (
                        "io.elitea.code.request".into(),
                        identity.request_digest().into(),
                    ),
                    (ROOT_LABEL.into(), root.into()),
                    (ROLE_LABEL.into(), "repository".into()),
                    (
                        OWNER_LABEL.into(),
                        uuid::Uuid::new_v4().simple().to_string(),
                    ),
                ]),
            })
            .await
            .map_err(|_| failed())?;
        let info = self
            .client
            .inspect_volume(&volume)
            .await
            .map_err(|_| failed())?;
        let labels = info.labels;
        if info.name != volume
            || info.driver != "local"
            || labels.get("io.elitea.code.job").map(String::as_str) != Some(identity.job_key())
            || labels.get("io.elitea.code.request").map(String::as_str)
                != Some(identity.request_digest())
            || labels.get(ROOT_LABEL).map(String::as_str) != Some(root)
            || labels.get(ROLE_LABEL).map(String::as_str) != Some("repository")
            || !info.options.is_empty()
        {
            return Err(failed());
        }
        let created = info
            .created_at
            .filter(|v| !v.is_empty() && v.len() <= 128 && !v.chars().any(char::is_control))
            .ok_or_else(failed)?;
        let owner = labels
            .get(OWNER_LABEL)
            .filter(|value| {
                value.len() == 32
                    && value
                        .bytes()
                        .all(|v| v.is_ascii_digit() || (b'a'..=b'f').contains(&v))
            })
            .cloned()
            .ok_or_else(failed)?;
        let mount = CodeRepositoryMount {
            root: root.into(),
            volume,
            created,
            owner,
            image,
        };
        self.prepare_repository_helper(identity, &mount).await?;
        Ok(mount)
    }

    async fn prepare_repository_helper(
        &self,
        identity: &CodeJobIdentity,
        mount: &CodeRepositoryMount,
    ) -> Result<(), SandboxError> {
        let name = helper_name(identity);
        match self.client.inspect_container(&name, None).await {
            Ok(info) => {
                self.validate_repository_helper(identity, mount, &info)?;
                return Ok(());
            }
            Err(bollard::errors::Error::DockerResponseServerError {
                status_code: 404, ..
            }) => {}
            Err(_) => return Err(failed()),
        }
        let mut host = self.build_host_config();
        host.mounts = Some(vec![mount.mount(false)]);
        host.network_mode = Some("none".into());
        let container=self.client.create_container(Some(CreateContainerOptions{name,platform:None}),Config {
 image:Some(self.check_code_image_ready().await?),user:Some("10001:10001".into()),env:Some(mount.environment(identity)),labels:Some(mount.labels(identity,"hydrator")),entrypoint:Some(Vec::<String>::new()),
 cmd:Some(vec!["/bin/sh".into(),"-c".into(),"while [ ! -f /workspace/repository/.elitea-workspace-release ]; do sleep 0.1; done".into()]),working_dir:Some("/workspace".into()),host_config:Some(host),..Default::default()}).await.map_err(|_|failed())?;
        self.client
            .start_container::<String>(&container.id, None)
            .await
            .map_err(|_| failed())?;
        let info = self
            .client
            .inspect_container(&container.id, None)
            .await
            .map_err(|_| failed())?;
        self.validate_repository_helper(identity, mount, &info)
    }

    fn validate_repository_helper(
        &self,
        identity: &CodeJobIdentity,
        mount: &CodeRepositoryMount,
        info: &bollard::models::ContainerInspectResponse,
    ) -> Result<(), SandboxError> {
        if info.image.as_deref() != Some(mount.image.as_str()) {
            return Err(failed());
        }
        let host = info.host_config.as_ref().ok_or_else(failed)?;
        if host.readonly_rootfs != Some(true)
            || host.network_mode.as_deref() != Some("none")
            || host.privileged == Some(true)
            || host
                .cap_drop
                .as_ref()
                .is_none_or(|v| v != &vec!["ALL".to_owned()])
            || host
                .security_opt
                .as_ref()
                .is_none_or(|v| !v.iter().any(|v| v == "no-new-privileges:true"))
            || host.binds.as_ref().is_some_and(|v| !v.is_empty())
        {
            return Err(failed());
        }
        let config = info.config.as_ref().ok_or_else(failed)?;
        let labels = config.labels.as_ref().ok_or_else(failed)?;
        if mount
            .labels(identity, "hydrator")
            .iter()
            .any(|(k, v)| labels.get(k) != Some(v))
            || config.user.as_deref() != Some("10001:10001")
            || info.id.as_ref().is_none_or(String::is_empty)
            || config.working_dir.as_deref()!=Some("/workspace")
            || config.entrypoint.as_ref().is_some_and(|v|!v.is_empty())
            || config.cmd.as_ref()!=Some(&vec!["/bin/sh".into(),"-c".into(),"while [ ! -f /workspace/repository/.elitea-workspace-release ]; do sleep 0.1; done".into()])
        {
            return Err(failed());
        }
        let env = config.env.as_ref().ok_or_else(failed)?;
        if mount
            .environment(identity)
            .iter()
            .any(|v| env.iter().filter(|e| *e == v).count() != 1)
        {
            return Err(failed());
        }
        let mounts = info.mounts.as_ref().ok_or_else(failed)?;
        if mounts
            .iter()
            .filter(|v| {
                v.destination.as_deref() == Some(REPOSITORY)
                    && v.name.as_deref() == Some(mount.volume.as_str())
                    && v.rw == Some(true)
            })
            .count()
            != 1
        {
            return Err(failed());
        };
        Ok(())
    }

    /// Validate the original Code container's immutable volume creation binding.
    pub async fn validate_code_repository(
        &self,
        identity: &CodeJobIdentity,
        root: &str,
    ) -> Result<CodeRepositoryMount, SandboxError> {
        if identity.runtime_id().is_none() || !valid(root) {
            return Err(failed());
        }
        let job = self.observe_code_job(identity).await?.ok_or_else(failed)?;
        let info = self
            .client
            .inspect_container(&job.container_id, None)
            .await
            .map_err(|_| failed())?;
        let labels = info
            .config
            .as_ref()
            .and_then(|v| v.labels.as_ref())
            .ok_or_else(failed)?;
        if labels.get(ROOT_LABEL).map(String::as_str) != Some(root)
            || labels.get(ROLE_LABEL).map(String::as_str) != Some("execution")
        {
            return Err(failed());
        }
        let created = labels.get(CREATED_LABEL).cloned().ok_or_else(failed)?;
        let owner = labels.get(OWNER_LABEL).cloned().ok_or_else(failed)?;
        let volume = volume_name(identity);
        let mounted = info.mounts.as_ref().ok_or_else(failed)?;
        if mounted
            .iter()
            .filter(|v| {
                v.destination.as_deref() == Some(REPOSITORY)
                    && v.name.as_deref() == Some(volume.as_str())
                    && v.rw == Some(false)
            })
            .count()
            != 1
        {
            return Err(failed());
        }
        let current = self
            .client
            .inspect_volume(&volume)
            .await
            .map_err(|_| failed())?;
        let volume_labels = current.labels;
        if current.name != volume
            || current.driver != "local"
            || current.created_at.as_deref() != Some(created.as_str())
            || volume_labels.get("io.elitea.code.job").map(String::as_str)
                != Some(identity.job_key())
            || volume_labels
                .get("io.elitea.code.request")
                .map(String::as_str)
                != Some(identity.request_digest())
            || volume_labels.get(ROOT_LABEL).map(String::as_str) != Some(root)
            || volume_labels.get(OWNER_LABEL).map(String::as_str) != Some(owner.as_str())
            || !current.options.is_empty()
        {
            return Err(failed());
        }
        Ok(CodeRepositoryMount {
            root: root.into(),
            volume,
            created,
            owner,
            image: self.check_code_image_ready().await?,
        })
    }

    pub async fn provision_code_job_with_repository(
        &self,
        identity: &CodeJobIdentity,
        manifest: &Manifest,
        repository: &CodeRepositoryMount,
    ) -> Result<SessionHandle, SandboxError> {
        for entry in &manifest.entries {
            match entry {
                ManifestEntry::File { path, .. }
                    if path == ".elitea-code.json" || path == ".elitea-job.json" => {}
                _ => return Err(failed()),
            }
        }
        if !self.code_job_policy
            || identity.runtime_id().is_some()
            || self.observe_code_job(identity).await?.is_some()
        {
            return Err(failed());
        }
        self.provision_with_repository(manifest, Some(identity), None, Some(repository))
            .await
    }

    /// Import only a validated fixed helper command, on the existing original volume.
    pub async fn code_repository_command(
        &self,
        identity: &CodeJobIdentity,
        root: &str,
        command: &str,
        header: &[u8],
        bytes: &[u8],
    ) -> Result<Vec<u8>, SandboxError> {
        if !matches!(
            command,
            "--workspace-manifest"
                | "--workspace-write"
                | "--workspace-probe"
                | "--workspace-finalize"
                | "--workspace-release"
        ) || header.len() > 4096
            || bytes.len() > 4 << 20
        {
            return Err(failed());
        }
        let mount = self.validate_code_repository(identity, root).await?;
        let helper = self
            .client
            .inspect_container(&helper_name(identity), None)
            .await
            .map_err(|_| failed())?;
        self.validate_repository_helper(identity, &mount, &helper)?;
        if helper.state.as_ref().and_then(|v| v.running) != Some(true) {
            return Err(failed());
        }
        let id = helper.id.ok_or_else(failed)?;
        let mut input = Vec::with_capacity(header.len() + bytes.len());
        input.extend_from_slice(header);
        input.extend_from_slice(bytes);
        let (out, err, status) = tokio::time::timeout(
            Duration::from_secs(30),
            self.exec_in_container(
                &id,
                vec!["/usr/local/bin/elitea-code-runner", command],
                None,
                Some(&input),
            ),
        )
        .await
        .map_err(|_| failed())??;
        if status != 0 || !err.is_empty() || out.len() > 1024 {
            return Err(failed());
        }
        self.validate_code_repository(identity, root).await?;
        Ok(out.into_bytes())
    }

    /// Release only after the helper has verified every immutable repository file.
    pub async fn code_repository_helper_stopped(
        &self,
        identity: &CodeJobIdentity,
        root: &str,
    ) -> Result<bool, SandboxError> {
        let mount = self.validate_code_repository(identity, root).await?;
        let helper = self
            .client
            .inspect_container(&helper_name(identity), None)
            .await
            .map_err(|_| failed())?;
        self.validate_repository_helper(identity, &mount, &helper)?;
        Ok(helper
            .state
            .is_some_and(|v| v.running == Some(false) && v.exit_code == Some(0)))
    }
}

impl DockerClient {
    /// Stop both original execution and its exact fixed repository helper.
    /// The supervisor must persist terminal state before removing either resource.
    pub async fn terminate_code_repository_helper(
        &self,
        identity: &CodeJobIdentity,
        root: &str,
    ) -> Result<(), SandboxError> {
        let mount = self.validate_code_repository(identity, root).await?;
        let helper = self
            .client
            .inspect_container(&helper_name(identity), None)
            .await
            .map_err(|_| failed())?;
        self.validate_repository_helper(identity, &mount, &helper)?;
        let id = helper.id.ok_or_else(failed)?;
        if helper.state.as_ref().and_then(|v| v.running) == Some(true) {
            let _ = self
                .client
                .kill_container(&id, Some(KillContainerOptions { signal: "SIGKILL" }))
                .await;
        }
        let current = self
            .client
            .inspect_container(&id, None)
            .await
            .map_err(|_| failed())?;
        self.validate_repository_helper(identity, &mount, &current)?;
        if current.state.as_ref().and_then(|v| v.running) != Some(false) {
            return Err(failed());
        };
        Ok(())
    }

    /// Capture exact provenance while the stopped original container still exists.
    /// Missing original metadata leaves cleanup pending. It never authorizes volume deletion.
    pub async fn code_repository_cleanup_binding(
        &self,
        identity: &CodeJobIdentity,
    ) -> Result<Option<CodeRepositoryMount>, SandboxError> {
        let Some(job) = self.observe_code_job(identity).await? else {
            return Ok(None);
        };
        if job.running {
            return Err(failed());
        }
        let info = self
            .client
            .inspect_container(&job.container_id, None)
            .await
            .map_err(|_| failed())?;
        let Some(root) = info
            .config
            .and_then(|v| v.labels)
            .and_then(|mut v| v.remove(ROOT_LABEL))
        else {
            return Ok(None);
        };
        Ok(Some(self.validate_code_repository(identity, &root).await?))
    }

    /// Reading the original immutable container label never infers a missing volume owner.
    pub async fn code_repository_root(
        &self,
        identity: &CodeJobIdentity,
    ) -> Result<Option<String>, SandboxError> {
        let Some(job) = self.observe_code_job(identity).await? else {
            return Ok(None);
        };
        let info = self
            .client
            .inspect_container(&job.container_id, None)
            .await
            .map_err(|_| failed())?;
        let root = info
            .config
            .and_then(|v| v.labels)
            .and_then(|mut v| v.remove(ROOT_LABEL));
        if root.as_ref().is_some_and(|v| !valid(v)) {
            return Err(failed());
        }
        Ok(root)
    }

    /// Terminal cleanup requires the durable original receipt, including the creation token.
    /// A crash after container removal can retry only the same measured named volume.
    pub async fn cleanup_code_repository_receipt(
        &self,
        identity: &CodeJobIdentity,
        root: &str,
        volume: &str,
        owner: &str,
        created: &str,
    ) -> Result<(), SandboxError> {
        if identity.runtime_id().is_none()
            || !valid(root)
            || volume != volume_name(identity)
            || owner.len() != 32
            || !owner
                .bytes()
                .all(|v| v.is_ascii_digit() || (b'a'..=b'f').contains(&v))
            || created.is_empty()
            || created.len() > 128
        {
            return Err(failed());
        }
        let mount = CodeRepositoryMount {
            root: root.into(),
            volume: volume.into(),
            owner: owner.into(),
            created: created.into(),
            image: self.check_code_image_ready().await?,
        };
        let job = self.observe_code_job(identity).await?;
        if let Some(job) = &job {
            if job.running {
                return Err(failed());
            }
            let observed = self.validate_code_repository(identity, root).await?;
            if observed.owner != owner || observed.created != created {
                return Err(failed());
            }
        }
        let helper = match self
            .client
            .inspect_container(&helper_name(identity), None)
            .await
        {
            Ok(value) => Some(value),
            Err(bollard::errors::Error::DockerResponseServerError {
                status_code: 404, ..
            }) => None,
            Err(_) => return Err(failed()),
        };
        let actual = match self.client.inspect_volume(volume).await {
            Ok(value) => value,
            Err(bollard::errors::Error::DockerResponseServerError {
                status_code: 404, ..
            }) if job.is_none() && helper.is_none() => return Ok(()),
            Err(_) => return Err(failed()),
        };
        validate_cleanup_volume(identity, &mount, &actual)?;
        if let Some(helper) = helper {
            self.validate_repository_helper(identity, &mount, &helper)?;
            if helper.state.as_ref().and_then(|v| v.running) != Some(false) {
                return Err(failed());
            }
            self.client
                .remove_container(
                    &helper.id.ok_or_else(failed)?,
                    Some(RemoveContainerOptions {
                        force: false,
                        v: false,
                        ..Default::default()
                    }),
                )
                .await
                .map_err(|_| failed())?;
        }
        self.remove_code_job(identity).await?;
        // Recheck immutable volume provenance after both containers no longer reference it.
        let actual = self
            .client
            .inspect_volume(volume)
            .await
            .map_err(|_| failed())?;
        validate_cleanup_volume(identity, &mount, &actual)?;
        self.client
            .remove_volume(volume, Some(RemoveVolumeOptions { force: false }))
            .await
            .map_err(|_| failed())?;
        Ok(())
    }
}

fn validate_cleanup_volume(
    identity: &CodeJobIdentity,
    mount: &CodeRepositoryMount,
    volume: &bollard::models::Volume,
) -> Result<(), SandboxError> {
    let labels = &volume.labels;
    if volume.name != mount.volume
        || volume.driver != "local"
        || volume.created_at.as_deref() != Some(mount.created.as_str())
        || !volume.options.is_empty()
        || labels.get("io.elitea.code.job").map(String::as_str) != Some(identity.job_key())
        || labels.get("io.elitea.code.request").map(String::as_str)
            != Some(identity.request_digest())
        || labels.get(ROOT_LABEL) != Some(&mount.root)
        || labels.get(OWNER_LABEL) != Some(&mount.owner)
        || labels.get(ROLE_LABEL).map(String::as_str) != Some("repository")
    {
        return Err(failed());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn retained_receipt_checks_volume_after_original_container_is_gone() {
        let identity = CodeJobIdentity::new("a".repeat(64), "b".repeat(64))
            .unwrap()
            .with_runtime_id("original-cid".into())
            .unwrap();
        let mount = CodeRepositoryMount {
            root: "c".repeat(64),
            volume: volume_name(&identity),
            owner: "d".repeat(32),
            created: "2026-10-04T00:00:00Z".into(),
            image: format!("sha256:{}", "e".repeat(64)),
        };
        let volume = bollard::models::Volume {
            name: mount.volume.clone(),
            driver: "local".into(),
            created_at: Some(mount.created.clone()),
            labels: HashMap::from([
                ("io.elitea.code.job".into(), identity.job_key().into()),
                (
                    "io.elitea.code.request".into(),
                    identity.request_digest().into(),
                ),
                (ROOT_LABEL.into(), mount.root.clone()),
                (ROLE_LABEL.into(), "repository".into()),
                (OWNER_LABEL.into(), mount.owner.clone()),
            ]),
            ..Default::default()
        };
        // No container read participates in this comparison; durable original provenance does.
        validate_cleanup_volume(&identity, &mount, &volume).unwrap();
        for key in [
            "io.elitea.code.job",
            "io.elitea.code.request",
            ROOT_LABEL,
            ROLE_LABEL,
            OWNER_LABEL,
        ] {
            let mut changed = volume.clone();
            changed.labels.insert(key.into(), "forged".into());
            assert!(validate_cleanup_volume(&identity, &mount, &changed).is_err())
        }
        let mut replacement = volume.clone();
        replacement.created_at = Some("2026-10-04T00:00:01Z".into());
        assert!(validate_cleanup_volume(&identity, &mount, &replacement).is_err());
    }
}
