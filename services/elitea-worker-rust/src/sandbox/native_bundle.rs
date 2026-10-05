//! Versioned native content identity. No descriptor can choose a host path or command.
use super::dependency_bundle::{DependencyBundleFile, InvalidDependencyBundle, hex, valid_digest};
use super::request::Language;
use ring::digest;
use serde::{Deserialize, Serialize};

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum NativeKind {
    Deno,
    Cargo,
}
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct NativePlatform {
    pub os: String,
    pub arch: String,
    pub abi: String,
}
impl NativePlatform {
    /// # Errors
    /// Rejects unsupported operating systems, architectures, and native ABI values.
    pub fn validate(&self) -> Result<(), InvalidDependencyBundle> {
        if self.os != "linux"
            || !matches!(self.arch.as_str(), "amd64" | "arm64")
            || self.abi != "gnu"
        {
            return Err(InvalidDependencyBundle);
        }
        Ok(())
    }
}
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct NativeRecord {
    pub revision: u8,
    pub kind: NativeKind,
    pub language: Language,
    pub platform: NativePlatform,
    pub preparation_sha256: String,
    pub source_sha256: String,
    pub execution_image_digest: String,
    pub execution_policy_revision: String,
    pub payload: serde_json::Value,
    pub digest: String,
}
pub struct NativeDependencyBundle {
    pub record: NativeRecord,
    files: Vec<DependencyBundleFile>,
    canonical: Vec<u8>,
}
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct DenoPayload {
    revision: u8,
    runtime: String,
    requirements: Vec<String>,
    files: Vec<DependencyBundleFile>,
    digest: String,
}
fn path_safe(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 256
        && name.split('/').count() <= 24
        && name.split('/').all(|p| {
            !p.is_empty()
                && p != "."
                && p != ".."
                && p.bytes()
                    .all(|b| b.is_ascii_alphanumeric() || b"_.+-@".contains(&b))
        })
}
fn deno_path(name: &str) -> bool {
    name == "elitea-javascript-lock.json"
        || name == "elitea-javascript-dependencies.mjs"
        || name.starts_with("deno-cache/npm/registry.npmjs.org/")
        || name
            .strip_prefix("deno-cache/remote/https/jsr.io/")
            .is_some_and(valid_digest)
}
fn policy(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"._-".contains(&b))
}
/// Encode native metadata with sorted object keys.
/// # Errors
/// Rejects values that cannot be serialized as JSON.
pub fn canonical(value: &serde_json::Value) -> Result<Vec<u8>, InvalidDependencyBundle> {
    let mut value = value.clone();
    value.sort_all_objects();
    serde_json::to_vec(&value).map_err(|_| InvalidDependencyBundle)
}
impl NativeDependencyBundle {
    /// # Errors
    /// Rejects unsupported bindings, unsafe content records, and non-canonical or mismatched roots.
    pub fn parse(bytes: &[u8], root: &str) -> Result<Self, InvalidDependencyBundle> {
        if bytes.len() > 128 * 1024 || !valid_digest(root) {
            return Err(InvalidDependencyBundle);
        }
        let record: NativeRecord =
            serde_json::from_slice(bytes).map_err(|_| InvalidDependencyBundle)?;
        record.platform.validate()?;
        if record.revision != 2
            || record.digest != root
            || !valid_digest(&record.preparation_sha256)
            || !valid_digest(&record.source_sha256)
            || !record
                .execution_image_digest
                .strip_prefix("sha256:")
                .is_some_and(valid_digest)
            || !policy(&record.execution_policy_revision)
        {
            return Err(InvalidDependencyBundle);
        }
        let files = match (record.kind, record.language) {
            (NativeKind::Deno, Language::JavaScript | Language::TypeScript) => {
                deno_files(&record.payload)?
            }
            (NativeKind::Cargo, Language::Rust) => cargo_files(&record)?,
            _ => return Err(InvalidDependencyBundle),
        };
        let mut value = serde_json::to_value(&record).map_err(|_| InvalidDependencyBundle)?;
        value
            .as_object_mut()
            .ok_or(InvalidDependencyBundle)?
            .remove("digest");
        if hex(digest::digest(&digest::SHA256, &canonical(&value)?).as_ref()) != root {
            return Err(InvalidDependencyBundle);
        }
        let canonical =
            canonical(&serde_json::to_value(&record).map_err(|_| InvalidDependencyBundle)?)?;
        if bytes != canonical {
            return Err(InvalidDependencyBundle);
        }
        Ok(Self {
            record,
            files,
            canonical,
        })
    }
    #[must_use]
    pub fn files(&self) -> &[DependencyBundleFile] {
        &self.files
    }
    #[must_use]
    pub fn root(&self) -> &str {
        &self.record.digest
    }
    #[must_use]
    pub fn record_json(&self) -> &[u8] {
        &self.canonical
    }
}
fn deno_files(
    value: &serde_json::Value,
) -> Result<Vec<DependencyBundleFile>, InvalidDependencyBundle> {
    // The component retains its ordered revision 1 content hash.
    #[derive(Serialize)]
    struct Content<'a> {
        revision: u8,
        runtime: &'a str,
        requirements: &'a [String],
        files: &'a [DependencyBundleFile],
    }
    let payload: DenoPayload =
        serde_json::from_value(value.clone()).map_err(|_| InvalidDependencyBundle)?;
    if payload.revision != 1
        || payload.runtime != "deno-2.5.4"
        || payload.requirements.len() > 128
        || payload.requirements.iter().any(|v| {
            v.is_empty()
                || v.len() > 256
                || !v.is_ascii()
                || !(v.starts_with("npm:") || v.starts_with("jsr:"))
        })
        || payload.files.len() < 2
        || payload.files.len() > 512
        || !valid_digest(&payload.digest)
    {
        return Err(InvalidDependencyBundle);
    }
    let mut total = 0u64;
    let mut lock = false;
    let mut module = false;
    let mut output = Vec::new();
    for (index, f) in payload.files.iter().enumerate() {
        if !path_safe(&f.name)
            || !deno_path(&f.name)
            || !valid_digest(&f.sha256)
            || f.bytes > 32 * 1024 * 1024
            || (index > 0 && f.name <= payload.files[index - 1].name)
        {
            return Err(InvalidDependencyBundle);
        }
        if f.name == "elitea-javascript-lock.json" {
            lock = true;
            if f.bytes > 1024 * 1024 {
                return Err(InvalidDependencyBundle);
            }
        }
        if f.name == "elitea-javascript-dependencies.mjs" {
            module = true;
            if f.bytes > 40 * 1024 {
                return Err(InvalidDependencyBundle);
            }
        }
        total = total.checked_add(f.bytes).ok_or(InvalidDependencyBundle)?;
        output.push(DependencyBundleFile {
            name: format!("{}.blob", f.sha256),
            bytes: f.bytes,
            sha256: f.sha256.clone(),
        });
    }
    if !lock || !module || total > 128 * 1024 * 1024 {
        return Err(InvalidDependencyBundle);
    }
    let bytes = serde_json::to_vec(&Content {
        revision: payload.revision,
        runtime: &payload.runtime,
        requirements: &payload.requirements,
        files: &payload.files,
    })
    .map_err(|_| InvalidDependencyBundle)?;
    if hex(digest::digest(&digest::SHA256, &bytes).as_ref()) != payload.digest {
        return Err(InvalidDependencyBundle);
    }
    Ok(output)
}
fn cargo_files(
    record: &NativeRecord,
) -> Result<Vec<DependencyBundleFile>, InvalidDependencyBundle> {
    let p = super::cargo_dependency_bundle::CargoPayload::parse(&record.payload)?;
    p.validate_binding(
        &record.preparation_sha256,
        &record.source_sha256,
        &record.platform,
        &record.execution_image_digest,
        &record.execution_policy_revision,
    )?;
    Ok(p.files().to_vec())
}

#[cfg(test)]
mod tests {
    use super::*;
    const FIXTURE: &[u8] = include_bytes!("native-deno-v2.json");
    #[test]
    fn native_fixture_preserves_index_order_and_rejects_unsafe_revisions() {
        let value: serde_json::Value = serde_json::from_slice(FIXTURE).unwrap();
        let root = value["digest"].as_str().unwrap();
        let bundle = NativeDependencyBundle::parse(FIXTURE, root).unwrap();
        assert_eq!(bundle.record_json(), FIXTURE);
        assert_eq!(bundle.files().len(), 2);
        assert!(
            super::super::dependency_bundle::PythonDependencyBundle::parse(FIXTURE, root).is_err()
        );
        for (key, changed) in [
            ("kind", serde_json::json!("cargo")),
            ("language", serde_json::json!("python")),
            ("revision", serde_json::json!(1)),
        ] {
            let mut v = value.clone();
            v[key] = changed;
            assert!(NativeDependencyBundle::parse(&canonical(&v).unwrap(), root).is_err());
        }
        let duplicate = String::from_utf8(FIXTURE.to_vec())
            .unwrap()
            .replace("\"revision\":2", "\"revision\":2,\"revision\":2");
        assert!(NativeDependencyBundle::parse(duplicate.as_bytes(), root).is_err());
    }
    #[test]
    fn safe_inventory_rejects_traversal_and_unbounded_nested_paths() {
        for name in [
            "../escaped",
            "/absolute",
            "a//b",
            "a/./b",
            "a/../../b",
            "a\\b",
        ] {
            assert!(!path_safe(name));
        }
        assert!(deno_path(
            "deno-cache/remote/https/jsr.io/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
        ));
        assert!(!deno_path("deno-cache/remote/https/other.invalid/abc"));
    }
}

#[cfg(test)]
mod cargo_conformance {
    use super::*;
    #[test]
    fn cargo_outer_fixture_uses_language_owned_order() {
        let bytes = include_bytes!("native-cargo-v2.json");
        let v: serde_json::Value = serde_json::from_slice(bytes).unwrap();
        let b = NativeDependencyBundle::parse(bytes, v["digest"].as_str().unwrap()).unwrap();
        assert_eq!(b.files().len(), 2);
        assert_eq!(
            b.files()[0].name(),
            v["payload"]["objects"][0]["name"].as_str().unwrap()
        );
        assert_eq!(
            b.files()[1].name(),
            v["payload"]["objects"][1]["name"].as_str().unwrap()
        );
    }
}
