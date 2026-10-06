//! Fixed parent-only launch metadata. Supervisor writes it before dispatch.
use super::{code_platform_bridge::LaunchBinding, code_platform_signature::TrustedKeys};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::{
    io::{self, Read},
    time::Duration,
};

pub(crate) const LAUNCH_PATH: &str = "/workspace/.elitea-platform-launch";
const TRUST_PATH: &str = "/opt/elitea-code-trust/main-receipt-keys.json";
fn refused() -> io::Error {
    io::Error::other("Code platform launch identity refused")
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct LaunchMetadata {
    pub(crate) revision: u8,
    pub(crate) request_digest: Option<String>,
    pub(crate) retained_runtime_id: String,
    pub(crate) prepared_sha256: String,
    pub(crate) policy_sha256: String,
    pub(crate) max_calls: u64,
    pub(crate) max_total_bytes: u64,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct KeyFile {
    revision: u8,
    keys: Vec<Key>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Key {
    key_id: String,
    public_key_hex: String,
}
pub(crate) fn digest(value: &str) -> io::Result<[u8; 32]> {
    if value.len() != 64 {
        return Err(refused());
    }
    let mut output = [0; 32];
    for (index, pair) in value.as_bytes().chunks_exact(2).enumerate() {
        let digit = |v| match v {
            b'0'..=b'9' => Ok(v - b'0'),
            b'a'..=b'f' => Ok(v - b'a' + 10),
            _ => Err(refused()),
        };
        output[index] = (digit(pair[0])? << 4) | digit(pair[1])?;
    }
    Ok(output)
}
pub(crate) fn read_fixed(path: &'static str, maximum: usize) -> io::Result<Vec<u8>> {
    let fd = rustix::fs::open(
        path,
        rustix::fs::OFlags::RDONLY
            | rustix::fs::OFlags::NOFOLLOW
            | rustix::fs::OFlags::NONBLOCK
            | rustix::fs::OFlags::CLOEXEC,
        rustix::fs::Mode::empty(),
    )?;
    let file = std::fs::File::from(fd);
    let metadata = file.metadata()?;
    if !metadata.is_file() || metadata.len() > maximum as u64 {
        return Err(refused());
    }
    let mut bytes = Vec::new();
    file.take(maximum as u64 + 1).read_to_end(&mut bytes)?;
    if bytes.len() > maximum {
        return Err(refused());
    }
    Ok(bytes)
}
pub(crate) fn metadata(bytes: &[u8]) -> io::Result<LaunchMetadata> {
    if bytes.len() > 4096 {
        return Err(refused());
    }
    let value: LaunchMetadata = serde_json::from_slice(bytes).map_err(|_| refused())?;
    let object: serde_json::Value = serde_json::from_slice(bytes).map_err(|_| refused())?;
    match value.revision {
        1 if !object
            .as_object()
            .ok_or_else(refused)?
            .contains_key("request_digest") => {}
        2 if value.request_digest.as_deref().is_some_and(|request| {
            digest(request).is_ok_and(|hash| hash != [0; 32]) && request != value.prepared_sha256
        }) => {}
        _ => return Err(refused()),
    }
    if value.retained_runtime_id.is_empty()
        || value.retained_runtime_id.len() > 512
        || value.retained_runtime_id.contains(['\0', '\r', '\n'])
        || digest(&value.prepared_sha256)? == [0; 32]
        || digest(&value.policy_sha256)? == [0; 32]
        || !(1..=4096).contains(&value.max_calls)
        || !(1..=64 * 1024 * 1024).contains(&value.max_total_bytes)
    {
        return Err(refused());
    }
    Ok(value)
}

// The optional request digest is computed from the trusted original Execute
// control by the Rust parent. A workspace or Code JSON field cannot supply it.
pub(crate) fn verify_request_identity(
    launch: &LaunchMetadata,
    original_request: Option<[u8; 32]>,
) -> io::Result<()> {
    match (
        launch.revision,
        launch.request_digest.as_deref(),
        original_request,
    ) {
        (1, None, None) => Ok(()),
        (2, Some(request), Some(expected)) if digest(request)? == expected => Ok(()),
        _ => Err(refused()),
    }
}

/// Call only in the fixed native parent, before user source executes.
/// The fixed trust asset belongs to the admitted image/operator profile.
pub(crate) fn capture(
    exact_prepared: &[u8],
    request: &serde_json::Value,
    timeout: Duration,
    original_request: Option<[u8; 32]>,
) -> io::Result<LaunchBinding> {
    let raw = read_fixed(LAUNCH_PATH, 4096)?;
    let launch = metadata(&raw)?;
    verify_request_identity(&launch, original_request)?;
    let mut hash = Sha256::new();
    hash.update(b"elitea.sandbox.prepared-job.v1\0");
    hash.update((exact_prepared.len() as u64).to_be_bytes());
    hash.update(exact_prepared);
    let prepared: [u8; 32] = hash.finalize().into();
    let capability = &request["platform_client"];
    if request["revision"] != 5
        || capability["revision"] != 1
        || digest(&launch.prepared_sha256)? != prepared
        || capability["policy_sha256"].as_str() != Some(launch.policy_sha256.as_str())
        || capability["max_calls"].as_u64() != Some(launch.max_calls)
        || capability["max_total_bytes"].as_u64() != Some(launch.max_total_bytes)
    {
        return Err(refused());
    }
    // Capture and close this descriptor before child spawn. No key reaches its pipes.
    let key_file: KeyFile =
        serde_json::from_slice(&read_fixed(TRUST_PATH, 8192)?).map_err(|_| refused())?;
    if key_file.revision != 1 || key_file.keys.is_empty() || key_file.keys.len() > 8 {
        return Err(refused());
    }
    let keys = key_file
        .keys
        .into_iter()
        .map(|entry| Ok((entry.key_id, digest(&entry.public_key_hex)?)))
        .collect::<io::Result<Vec<_>>>()?;
    LaunchBinding::new(
        launch.retained_runtime_id,
        prepared,
        digest(&launch.policy_sha256)?,
        launch.max_calls,
        launch.max_total_bytes,
        timeout,
        TrustedKeys::new(keys)?,
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    fn launch(revision: u8, request: Option<serde_json::Value>) -> Vec<u8> {
        let mut value = serde_json::json!({"revision":revision,"retained_runtime_id":"original-runtime","prepared_sha256":"a".repeat(64),"policy_sha256":"b".repeat(64),"max_calls":32,"max_total_bytes":1048576});
        if let Some(request) = request {
            value["request_digest"] = request;
        }
        serde_json::to_vec(&value).unwrap()
    }
    #[test]
    fn legacy_launch_omits_request_and_compiled_requires_original_control_digest() {
        let legacy = metadata(&launch(1, None)).unwrap();
        assert!(verify_request_identity(&legacy, None).is_ok());
        assert!(verify_request_identity(&legacy, Some([0xcc; 32])).is_err());
        let compiled = metadata(&launch(2, Some(serde_json::json!("c".repeat(64))))).unwrap();
        assert!(verify_request_identity(&compiled, Some([0xcc; 32])).is_ok());
        assert!(verify_request_identity(&compiled, None).is_err());
        assert!(verify_request_identity(&compiled, Some([0xdd; 32])).is_err());
        for (revision, request) in [
            (1, Some(serde_json::json!("c".repeat(64)))),
            (1, Some(serde_json::Value::Null)),
            (2, None),
            (2, Some(serde_json::Value::Null)),
            (2, Some(serde_json::json!("a".repeat(64)))),
            (2, Some(serde_json::json!("0".repeat(64)))),
        ] {
            assert!(metadata(&launch(revision, request)).is_err());
        }
    }
}
