//! The runtime-context `workspace` field (ADR-0034 decision 9).
//!
//! Main serves it only to a worker that names it in
//! `X-Elitea-Runtime-Context-Accept`, and only for a cloud turn of a
//! handed-off thread. It is parsed as strictly as the rest of the runtime
//! context: an unknown key anywhere refuses the redemption, so a later Main
//! cannot slip a field this worker would silently ignore.

use serde::Deserialize;

/// The accept-list name of the field.
pub(crate) const RUNTIME_CONTEXT_FIELD: &str = "workspace";

const MAX_ALLOWLIST_HOSTS: usize = 256;
const MAX_HOST_BYTES: usize = 253;

/// The policy-derived limits of one workspace run.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub(crate) struct WorkspaceLimits {
    pub(crate) max_segment_bytes: u64,
    pub(crate) max_file_bytes: u64,
    pub(crate) max_files: u64,
    pub(crate) max_unpacked_bytes: u64,
    /// The unpacked tree size the desktop declared at send: the disk-backed
    /// workspace volume is sized from it.
    pub(crate) unpacked_bytes: u64,
    /// Zero: the deployment default.
    pub(crate) turn_wall_seconds: u64,
    pub(crate) cpu_millis: u64,
    pub(crate) memory_bytes: u64,
    pub(crate) disk_bytes: u64,
}

/// The `workspace` field: which thread, which epoch, what the turn may do.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
#[allow(
    clippy::struct_excessive_bools,
    reason = "Main's wire format: one switch per policy"
)]
pub(crate) struct WorkspaceRuntimeContext {
    pub(crate) thread: String,
    pub(crate) epoch: u64,
    /// `refs/elitea/cloud` at claim time: the publish's expected old value.
    /// `null` before the first cloud turn of the handoff.
    pub(crate) expected_old_cloud: Option<String>,
    pub(crate) limits: WorkspaceLimits,
    pub(crate) network: bool,
    pub(crate) shell: bool,
    pub(crate) network_allowlist: Vec<String>,
    pub(crate) has_upstream: bool,
    /// The thread has harvested env (contract 6.4): the worker redeems it
    /// over `WorkspaceEnvSecret`. The values are never in this field.
    pub(crate) env_secret: bool,
    /// This turn regenerates the newest cloud turn (ADR-0034 decision 7):
    /// before it runs, `refs/elitea/cloud` moves back from
    /// `expected_old_cloud` (the regenerated turn's commit) to its parent,
    /// through the normal publish compare-and-set. Absent from an older
    /// Main: false.
    #[serde(default)]
    pub(crate) rewind: bool,
    /// The commit a rewind moves the cloud ref back to: the commit the
    /// regenerated turn started from, a first-parent ancestor of
    /// `expected_old_cloud`. `null` with no rewind. Absent from an older Main
    /// (which named none): the rewind goes to the first parent.
    #[serde(default)]
    pub(crate) rewind_to: Option<String>,
}

impl WorkspaceRuntimeContext {
    /// Check what serde cannot: identifiers, bounds, and that the limits are
    /// a usable whole.
    pub(crate) fn validate(&self) -> Result<(), &'static str> {
        if !canonical_uuid(&self.thread) {
            return Err("the workspace thread is malformed");
        }
        if self.epoch == 0 || i64::try_from(self.epoch).is_err() {
            return Err("the workspace epoch is malformed");
        }
        if self
            .expected_old_cloud
            .as_deref()
            .is_some_and(|sha| !sha1_hex(sha))
        {
            return Err("the expected cloud commit is malformed");
        }
        let limits = &self.limits;
        if limits.max_segment_bytes == 0
            || limits.max_file_bytes == 0
            || limits.max_files == 0
            || limits.max_unpacked_bytes == 0
            || limits.unpacked_bytes > limits.max_unpacked_bytes
        {
            return Err("the workspace limits are malformed");
        }
        if self.rewind && self.expected_old_cloud.is_none() {
            return Err("a rewind without a cloud turn to rewind is malformed");
        }
        match (&self.rewind_to, self.rewind) {
            (Some(_), false) => return Err("a rewind target without a rewind is malformed"),
            (Some(to), true) if !sha1_hex(to) || self.expected_old_cloud.as_ref() == Some(to) => {
                return Err("the rewind target is malformed");
            }
            _ => {}
        }
        if !self.network && !self.network_allowlist.is_empty() {
            return Err("a network allowlist without network is malformed");
        }
        if self.network_allowlist.len() > MAX_ALLOWLIST_HOSTS
            || self.network_allowlist.iter().any(|host| !valid_host(host))
        {
            return Err("the network allowlist is malformed");
        }
        Ok(())
    }
}

pub(crate) fn sha1_hex(value: &str) -> bool {
    value.len() == 40
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

pub(crate) fn canonical_uuid(value: &str) -> bool {
    value.len() == 36
        && value.bytes().enumerate().all(|(index, byte)| match index {
            8 | 13 | 18 | 23 => byte == b'-',
            _ => byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte),
        })
}

fn valid_host(host: &str) -> bool {
    !host.is_empty()
        && host.len() <= MAX_HOST_BYTES
        && host.bytes().all(|byte| {
            byte.is_ascii_lowercase() || byte.is_ascii_digit() || matches!(byte, b'.' | b'-' | b'*')
        })
}

#[cfg(test)]
pub(crate) fn fixture() -> WorkspaceRuntimeContext {
    WorkspaceRuntimeContext {
        thread: "0b9c6f4e-9a7f-4d4e-8f0e-1d2c3b4a5f60".to_owned(),
        epoch: 2,
        expected_old_cloud: None,
        limits: WorkspaceLimits {
            max_segment_bytes: 200 << 20,
            max_file_bytes: 25 << 20,
            max_files: 20_000,
            max_unpacked_bytes: 2 << 30,
            unpacked_bytes: 5 << 20,
            turn_wall_seconds: 0,
            cpu_millis: 0,
            memory_bytes: 0,
            disk_bytes: 0,
        },
        network: true,
        shell: true,
        network_allowlist: Vec::new(),
        has_upstream: true,
        env_secret: false,
        rewind: false,
        rewind_to: None,
    }
}

#[cfg(test)]
mod tests {
    use super::{WorkspaceRuntimeContext, fixture};

    fn raw() -> serde_json::Value {
        serde_json::json!({
            "thread": "0b9c6f4e-9a7f-4d4e-8f0e-1d2c3b4a5f60",
            "epoch": 2,
            "expected_old_cloud": null,
            "limits": {
                "max_segment_bytes": 209_715_200_u64, "max_file_bytes": 26_214_400_u64,
                "max_files": 20_000, "max_unpacked_bytes": 2_147_483_648_u64,
                "unpacked_bytes": 5_242_880, "turn_wall_seconds": 0, "cpu_millis": 0,
                "memory_bytes": 0, "disk_bytes": 0
            },
            "network": true, "shell": true, "network_allowlist": [], "has_upstream": true,
            "env_secret": false
        })
    }

    #[test]
    fn the_field_parses_strictly() {
        let parsed: WorkspaceRuntimeContext =
            serde_json::from_value(raw()).expect("the field Main serves");
        assert_eq!(parsed, fixture());
        assert_eq!(parsed.validate(), Ok(()));

        let mut extra = raw();
        extra["future_key"] = serde_json::json!(1);
        assert!(serde_json::from_value::<WorkspaceRuntimeContext>(extra).is_err());
        let mut nested = raw();
        nested["limits"]["future_limit"] = serde_json::json!(1);
        assert!(serde_json::from_value::<WorkspaceRuntimeContext>(nested).is_err());
        let mut missing = raw();
        missing.as_object_mut().expect("object").remove("shell");
        assert!(serde_json::from_value::<WorkspaceRuntimeContext>(missing).is_err());
    }

    #[test]
    fn validation_refuses_malformed_identity_and_policy() {
        let mut context = fixture();
        context.epoch = 0;
        assert!(context.validate().is_err());
        let mut context = fixture();
        context.thread = "not-a-uuid".to_owned();
        assert!(context.validate().is_err());
        let mut context = fixture();
        context.expected_old_cloud = Some("ABC".to_owned());
        assert!(context.validate().is_err());
        let mut context = fixture();
        context.network = false;
        context.network_allowlist = vec!["pypi.org".to_owned()];
        assert!(context.validate().is_err());
        let mut context = fixture();
        context.limits.unpacked_bytes = context.limits.max_unpacked_bytes + 1;
        assert!(context.validate().is_err());
        let cloud = "1111111111111111111111111111111111111111".to_owned();
        let mut context = fixture();
        context.rewind_to = Some("2222222222222222222222222222222222222222".to_owned());
        assert!(context.validate().is_err(), "a target without a rewind");
        context.rewind = true;
        context.expected_old_cloud = Some(cloud.clone());
        assert_eq!(context.validate(), Ok(()));
        context.rewind_to = Some(cloud);
        assert!(
            context.validate().is_err(),
            "a target equal to the cloud commit"
        );
        context.rewind_to = Some("XYZ".to_owned());
        assert!(context.validate().is_err());
    }

    /// Main serves `rewind` and `rewind_to` on every field now.
    #[test]
    fn the_rewind_target_parses() {
        let mut raw = raw();
        raw["expected_old_cloud"] = serde_json::json!("1111111111111111111111111111111111111111");
        raw["rewind"] = serde_json::json!(true);
        raw["rewind_to"] = serde_json::json!("2222222222222222222222222222222222222222");
        let parsed: WorkspaceRuntimeContext = serde_json::from_value(raw).expect("parsed");
        assert!(parsed.rewind);
        assert_eq!(
            parsed.rewind_to.as_deref(),
            Some("2222222222222222222222222222222222222222")
        );
        assert_eq!(parsed.validate(), Ok(()));
    }
}
