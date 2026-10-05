//! Trusted acquisition only. This component never compiles or executes user code.
//! Run this helper in a separate, bounded acquisition container.
#![forbid(unsafe_code)]
#![allow(dead_code)]

#[path = "rust_profile_selector.rs"]
mod rust_profile_selector;
pub(crate) use rust_profile_selector::Profile;
pub(crate) const BROKER_POLICY: &str = rust_profile_selector::BROKER_POLICY;

#[path = "rust_prepare_archive.rs"]
pub(crate) mod rust_prepare_archive;
#[cfg(test)]
#[path = "rust_prepare_tests.rs"]
mod rust_prepare_tests;

use rust_prepare_archive::{ArchiveRecord, create_archive, digest, verify_archive};
use serde::{Deserialize, Serialize};
use std::{
    io::Read,
    path::{Path, PathBuf},
    process::Stdio,
    time::Duration,
};
use tokio::{io::AsyncReadExt, process::Command, time::Instant};

const DECLARATION_LIMIT: usize = 64 * 1024;
const DIRECT_DEPENDENCY_LIMIT: usize = 128;
const CAPTURE_LIMIT: usize = 128 * 1024;
const TEMPLATE: &str = include_str!("../adapters/rust/Cargo.toml");
const WRAPPER: &str = include_str!("../adapters/rust/src/main.rs");
const PLACEHOLDER: &str = include_str!("../adapters/rust/src/user.rs");

#[derive(Debug, Clone, Copy, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub(crate) enum ErrorCode {
    InvalidRequest,
    InvalidDeclaration,
    IoFailed,
    CargoFailed,
    OutputLimit,
    DeadlineExceeded,
    InvalidContent,
    ContentLimit,
    IntegrityMismatch,
}

/// This error contains no Cargo output, dependency declaration, or host path.
#[derive(Debug)]
pub(crate) struct PrepareError {
    pub(crate) code: ErrorCode,
    pub(crate) diagnostic: &'static str,
}

pub(crate) type Result<T> = std::result::Result<T, PrepareError>;

impl PrepareError {
    pub(crate) fn new(code: ErrorCode, diagnostic: &'static str) -> Self {
        Self { code, diagnostic }
    }
}

pub(crate) fn io_error(_: std::io::Error) -> PrepareError {
    PrepareError::new(ErrorCode::IoFailed, "Preparation file operation failed")
}

pub(crate) fn deadline_check(deadline: Instant) -> Result<()> {
    if Instant::now() >= deadline {
        return Err(PrepareError::new(
            ErrorCode::DeadlineExceeded,
            "Preparation deadline expired",
        ));
    }
    Ok(())
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct Request {
    pub(crate) revision: u8,
    pub(crate) timeout_seconds: u64,
}
impl Request {
    pub(crate) fn validate(&self) -> Result<()> {
        if self.revision != 1 || !(1..=600).contains(&self.timeout_seconds) {
            return Err(PrepareError::new(
                ErrorCode::InvalidRequest,
                "Unsupported preparation request",
            ));
        }
        Ok(())
    }
}

fn declaration_error() -> PrepareError {
    PrepareError::new(
        ErrorCode::InvalidDeclaration,
        "Use only bounded crates.io dependencies with supported Cargo fields",
    )
}

fn crate_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 64
        && name
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
        && name
            .bytes()
            .next()
            .is_some_and(|byte| byte.is_ascii_alphabetic())
}

fn version(value: &toml::Value) -> bool {
    value.as_str().is_some_and(|value| {
        !value.is_empty()
            && value.len() <= 256
            && value
                .bytes()
                .all(|byte| byte.is_ascii_graphic() || byte == b' ')
    })
}

/// Cargo owns version syntax, feature semantics, renames, and the transitive graph.
/// This parser owns the allowed declaration shape and source restrictions.
pub(crate) fn parse_dependencies(bytes: &[u8]) -> Result<toml::Table> {
    if bytes.len() > DECLARATION_LIMIT {
        return Err(declaration_error());
    }
    let text = std::str::from_utf8(bytes).map_err(|_| declaration_error())?;
    let declaration: toml::Table = toml::from_str(text).map_err(|_| declaration_error())?;
    if declaration.len() != 1 {
        return Err(declaration_error());
    }
    let dependencies = declaration
        .get("dependencies")
        .and_then(toml::Value::as_table)
        .ok_or_else(declaration_error)?;
    if dependencies.len() > DIRECT_DEPENDENCY_LIMIT {
        return Err(declaration_error());
    }
    for (name, value) in dependencies {
        // Cargo crate identifiers equate '-' and '_'. Protect the wrapper name
        // and prevent a rename from adding another copy of its reserved package.
        if !crate_name(name) || name.replace('-', "_") == "serde_json" {
            return Err(declaration_error());
        }
        if value.is_str() {
            if !version(value) {
                return Err(declaration_error());
            }
            continue;
        }
        let fields = value.as_table().ok_or_else(declaration_error)?;
        if !fields.get("version").is_some_and(version)
            || fields.keys().any(|key| {
                !matches!(
                    key.as_str(),
                    "version" | "features" | "default-features" | "package"
                )
            })
        {
            return Err(declaration_error());
        }
        if fields
            .get("default-features")
            .is_some_and(|value| !value.is_bool())
        {
            return Err(declaration_error());
        }
        if let Some(package) = fields.get("package") {
            let package = package.as_str().ok_or_else(declaration_error)?;
            if !crate_name(package) || package.replace('-', "_") == "serde_json" {
                return Err(declaration_error());
            }
        }
        if let Some(features) = fields.get("features") {
            let features = features.as_array().ok_or_else(declaration_error)?;
            if features.len() > 128
                || features.iter().any(|feature| {
                    !feature.as_str().is_some_and(|feature| {
                        !feature.is_empty()
                            && feature.len() <= 128
                            && feature.bytes().all(|byte| byte.is_ascii_graphic())
                    })
                })
            {
                return Err(declaration_error());
            }
        }
    }
    Ok(dependencies.clone())
}

pub(crate) fn compose_manifest(dependencies: toml::Table) -> Result<Vec<u8>> {
    compose_manifest_profile(Profile::Legacy, dependencies)
}

pub(crate) fn compose_manifest_profile(
    profile: Profile,
    dependencies: toml::Table,
) -> Result<Vec<u8>> {
    profile.validate_dependencies(&dependencies)?;
    let mut manifest: toml::Table = toml::from_str(profile.template())
        .map_err(|_| PrepareError::new(ErrorCode::InvalidContent, "Image manifest is invalid"))?;
    let base = manifest
        .get_mut("dependencies")
        .and_then(toml::Value::as_table_mut)
        .ok_or_else(|| PrepareError::new(ErrorCode::InvalidContent, "Image manifest is invalid"))?;
    base.extend(dependencies);
    // An empty image-owned workspace prevents discovery of an ancestor workspace.
    manifest.insert("workspace".into(), toml::Value::Table(toml::Table::new()));
    let mut profiles = toml::Table::new();
    for profile in ["dev", "test"] {
        let mut settings = toml::Table::new();
        settings.insert(
            "debug".into(),
            toml::Value::String("line-tables-only".into()),
        );
        settings.insert("incremental".into(), toml::Value::Boolean(false));
        profiles.insert(profile.into(), toml::Value::Table(settings));
    }
    manifest.insert("profile".into(), toml::Value::Table(profiles));
    toml::to_string(&manifest)
        .map(String::into_bytes)
        .map_err(|_| PrepareError::new(ErrorCode::InvalidContent, "Image manifest is invalid"))
}

/// These paths belong to the trusted image or the local test harness.
/// Never deserialize them from a dependency request.
pub(crate) struct CargoImage {
    cargo: PathBuf,
    rustup_home: PathBuf,
    path: String,
}

impl CargoImage {
    #[cfg(test)]
    pub(crate) fn fixture(cargo: PathBuf, rustup_home: PathBuf, path: String) -> Self {
        Self {
            cargo,
            rustup_home,
            path,
        }
    }

    pub(crate) fn container() -> Self {
        Self {
            cargo: PathBuf::from("/usr/local/cargo/bin/cargo"),
            rustup_home: PathBuf::from("/usr/local/rustup"),
            path: "/usr/local/cargo/bin:/usr/local/bin:/usr/bin:/bin".into(),
        }
    }
}

async fn cargo_command(
    image: &CargoImage,
    workspace: &Path,
    home: &Path,
    arguments: &[&str],
    deadline: Instant,
) -> Result<Vec<u8>> {
    deadline_check(deadline)?;
    let mut child = Command::new(&image.cargo)
        .current_dir(workspace)
        .env_clear()
        .env("PATH", &image.path)
        .env("RUSTUP_HOME", &image.rustup_home)
        .env("CARGO_HOME", home)
        .env("HOME", workspace)
        .env("TMPDIR", workspace.join("tmp"))
        .env("CARGO_BUILD_JOBS", "2")
        .env("CARGO_INCREMENTAL", "0")
        .env("CARGO_TERM_COLOR", "never")
        .env("CARGO_HTTP_TIMEOUT", "30")
        .env("CARGO_NET_RETRY", "1")
        .args(arguments)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true)
        .spawn()
        .map_err(|_| PrepareError::new(ErrorCode::CargoFailed, "Cargo could not start"))?;
    let (Some(mut stdout), Some(mut stderr)) = (child.stdout.take(), child.stderr.take()) else {
        let _ = child.start_kill();
        let _ = tokio::time::timeout(Duration::from_secs(2), child.wait()).await;
        return Err(PrepareError::new(
            ErrorCode::CargoFailed,
            "Cargo capture failed",
        ));
    };
    let mut output = Vec::new();
    let mut capture_bytes = 0usize;
    let mut stdout_open = true;
    let mut stderr_open = true;
    let mut status = None;
    let mut out = [0; 8192];
    let mut err = [0; 8192];
    let operation = async {
        loop {
            if !stdout_open && !stderr_open && status.is_some() {
                if status.as_ref().is_some_and(std::process::ExitStatus::success) {
                    return Ok(output);
                }
                return Err(PrepareError::new(ErrorCode::CargoFailed, "Cargo dependency acquisition failed"));
            }
            tokio::select! {
                biased;
                () = tokio::time::sleep_until(deadline) => {
                    return Err(PrepareError::new(ErrorCode::DeadlineExceeded, "Preparation deadline expired"));
                }
                exit = child.wait(), if status.is_none() => {
                    status = Some(exit.map_err(|_| PrepareError::new(ErrorCode::CargoFailed, "Cargo wait failed"))?);
                }
                chunk = stdout.read(&mut out), if stdout_open => {
                    let length = chunk.map_err(io_error)?;
                    stdout_open = length != 0;
                    capture_bytes += length;
                    if capture_bytes > CAPTURE_LIMIT {
                        return Err(PrepareError::new(ErrorCode::OutputLimit, "Cargo diagnostic limit exceeded"));
                    }
                    output.extend_from_slice(&out[..length]);
                }
                chunk = stderr.read(&mut err), if stderr_open => {
                    let length = chunk.map_err(io_error)?;
                    stderr_open = length != 0;
                    capture_bytes += length;
                    if capture_bytes > CAPTURE_LIMIT {
                        return Err(PrepareError::new(ErrorCode::OutputLimit, "Cargo diagnostic limit exceeded"));
                    }
                    // Dependency names, Cargo paths, and response text stay local.
                    // Discard raw diagnostics; return a bounded safe diagnostic.
                }
            }
        }
    }.await;
    if operation.is_err() {
        // start_kill is immediate. kill_on_drop also owns cancellation on drop.
        let _ = child.start_kill();
        let _ = tokio::time::timeout(Duration::from_secs(2), child.wait()).await;
    }
    operation
}

pub(crate) fn check_vendor_config(config: &[u8]) -> Result<()> {
    let value: toml::Value = toml::from_str(std::str::from_utf8(config).map_err(|_| {
        PrepareError::new(
            ErrorCode::InvalidContent,
            "Cargo vendor configuration is invalid",
        )
    })?)
    .map_err(|_| {
        PrepareError::new(
            ErrorCode::InvalidContent,
            "Cargo vendor configuration is invalid",
        )
    })?;
    let expected: toml::Value = toml::from_str(
        "[source.crates-io]\nreplace-with = 'vendored-sources'\n[source.vendored-sources]\ndirectory = 'vendor'\n",
    ).map_err(|_| PrepareError::new(ErrorCode::InvalidContent, "Image vendor configuration is invalid"))?;
    if value != expected {
        return Err(PrepareError::new(
            ErrorCode::InvalidContent,
            "Cargo vendor configuration is not portable",
        ));
    }
    Ok(())
}

/// Isolated component format. This is not a platform receipt or publication grant.
#[derive(Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub(crate) struct PreparationRecord {
    pub(crate) revision: u8,
    pub(crate) format: String,
    pub(crate) declaration_sha256: String,
    pub(crate) manifest_sha256: String,
    pub(crate) lock_sha256: String,
    pub(crate) vendor_config_sha256: String,
    pub(crate) wrapper_sha256: String,
    pub(crate) content: ArchiveRecord,
}

pub(crate) fn verify_record(declaration: &[u8], record: &PreparationRecord) -> Result<()> {
    verify_record_profile(Profile::Legacy, declaration, record)
}

pub(crate) fn verify_record_profile(
    profile: Profile,
    declaration: &[u8],
    record: &PreparationRecord,
) -> Result<()> {
    let manifest = compose_manifest_profile(profile, parse_dependencies(declaration)?)?;
    let original_root = [
        ("Cargo.toml", record.manifest_sha256.as_str()),
        ("Cargo.lock", record.lock_sha256.as_str()),
        (".cargo/config.toml", record.vendor_config_sha256.as_str()),
        ("src/main.rs", record.wrapper_sha256.as_str()),
    ];
    if record.revision != 1
        || record.format != "elitea.rust-native-preparation.v1"
        || record.declaration_sha256 != digest(declaration)
        || record.manifest_sha256 != digest(&manifest)
        || record.wrapper_sha256 != digest(profile.wrapper().as_bytes())
        || original_root.iter().any(|(path, hash)| {
            !record
                .content
                .files
                .iter()
                .any(|file| file.path == *path && file.sha256 == *hash)
        })
        || profile.sources().iter().any(|(path, bytes)| {
            !record
                .content
                .files
                .iter()
                .any(|file| file.path == *path && file.sha256 == digest(bytes.as_bytes()))
        })
        || !record
            .content
            .files
            .iter()
            .any(|file| file.path == "src/user.rs" && file.sha256 == digest(PLACEHOLDER.as_bytes()))
    {
        return Err(PrepareError::new(
            ErrorCode::IntegrityMismatch,
            "Preparation identity or original root changed",
        ));
    }
    Ok(())
}

pub(crate) fn verify_preparation(
    declaration: &[u8],
    output: &Path,
    record: &PreparationRecord,
    deadline: Instant,
) -> Result<()> {
    verify_record(declaration, record)?;
    verify_archive(&output.join("content.tar.gz"), &record.content, deadline)
}

/// Retain the exact native root and lock. Reuse needs no resolution command.
/// The caller owns admission, the container resource limits, and cancellation.
pub(crate) async fn prepare(
    declaration: &[u8],
    output: &Path,
    timeout: Duration,
    image: &CargoImage,
) -> Result<PreparationRecord> {
    prepare_profile(Profile::Legacy, declaration, output, timeout, image).await
}

pub(crate) async fn prepare_profile(
    profile: Profile,
    declaration: &[u8],
    output: &Path,
    timeout: Duration,
    image: &CargoImage,
) -> Result<PreparationRecord> {
    let deadline = Instant::now() + timeout;
    // Select and validate all image assets before workspace creation, Cargo, or network access.
    let manifest = compose_manifest_profile(profile, parse_dependencies(declaration)?)?;
    deadline_check(deadline)?;
    let parent = output.parent().ok_or_else(|| {
        PrepareError::new(
            ErrorCode::InvalidRequest,
            "Trusted output parent is required",
        )
    })?;
    if std::fs::symlink_metadata(output).is_ok() {
        return Err(PrepareError::new(
            ErrorCode::IoFailed,
            "Preparation output already exists",
        ));
    }
    let scratch = tempfile::Builder::new()
        .prefix(".elitea-rust-acquire-")
        .tempdir_in(parent)
        .map_err(io_error)?;
    let workspace = scratch.path().join("root");
    let home = scratch.path().join("cargo-home");
    for directory in [
        &workspace,
        &home,
        &workspace.join("src"),
        &workspace.join("tmp"),
        &workspace.join(".cargo"),
    ] {
        std::fs::create_dir(directory).map_err(io_error)?;
    }
    std::fs::write(workspace.join("Cargo.toml"), &manifest).map_err(io_error)?;
    for (path, bytes) in profile.sources() {
        std::fs::write(workspace.join(path), bytes).map_err(io_error)?;
    }
    std::fs::write(workspace.join("src/user.rs"), PLACEHOLDER).map_err(io_error)?;
    cargo_command(image, &workspace, &home, &["generate-lockfile"], deadline).await?;
    let config = cargo_command(
        image,
        &workspace,
        &home,
        &["vendor", "--locked", "--versioned-dirs", "vendor"],
        deadline,
    )
    .await?;
    check_vendor_config(&config)?;
    std::fs::write(workspace.join(".cargo/config.toml"), &config).map_err(io_error)?;
    // Cargo scratch caches and temporary directories never enter retained content.
    std::fs::remove_dir(workspace.join("tmp")).map_err(io_error)?;
    let lock = read_regular(&workspace.join("Cargo.lock"), 4 * 1024 * 1024)?;
    let archive_path = scratch.path().join("content.tar.gz");
    let content = create_archive(&workspace, &archive_path, deadline)?;
    verify_archive(&archive_path, &content, deadline)?;
    let record = PreparationRecord {
        revision: 1,
        format: "elitea.rust-native-preparation.v1".into(),
        declaration_sha256: digest(declaration),
        manifest_sha256: digest(&manifest),
        lock_sha256: digest(&lock),
        vendor_config_sha256: digest(&config),
        wrapper_sha256: digest(profile.wrapper().as_bytes()),
        content,
    };
    deadline_check(deadline)?;
    let record_bytes = serde_json::to_vec(&record).map_err(|_| {
        PrepareError::new(
            ErrorCode::InvalidContent,
            "Preparation record encoding failed",
        )
    })?;
    // Exclusive output claim. The record is written last; callers require success.
    std::fs::create_dir(output).map_err(io_error)?;
    let publish = (|| {
        std::fs::rename(&archive_path, output.join("content.tar.gz")).map_err(io_error)?;
        std::fs::write(output.join("record.json"), record_bytes).map_err(io_error)?;
        deadline_check(deadline)
    })();
    if publish.is_err() {
        // Only this invocation created the directory in a private container.
        let _ = std::fs::remove_dir_all(output);
    }
    publish?;
    Ok(record)
}

pub(crate) fn read_regular(path: &Path, limit: u64) -> Result<Vec<u8>> {
    let metadata = std::fs::symlink_metadata(path).map_err(io_error)?;
    if !metadata.file_type().is_file() || metadata.len() > limit {
        return Err(PrepareError::new(
            ErrorCode::InvalidContent,
            "Preparation input is not a bounded regular file",
        ));
    }
    let mut bytes = Vec::new();
    std::fs::File::open(path)
        .map_err(io_error)?
        .take(limit + 1)
        .read_to_end(&mut bytes)
        .map_err(io_error)?;
    if bytes.len() as u64 > limit {
        return Err(PrepareError::new(
            ErrorCode::ContentLimit,
            "Preparation input limit exceeded",
        ));
    }
    Ok(bytes)
}
