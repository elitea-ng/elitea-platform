use super::*;
use rust_prepare_archive::safe_path;
use std::os::unix::fs::{PermissionsExt, symlink};

fn deadline() -> Instant {
    Instant::now() + Duration::from_secs(30)
}

fn image(script: &str, directory: &Path) -> CargoImage {
    let cargo = directory.join("cargo-fixture");
    std::fs::write(&cargo, format!("#!/bin/sh\nset -eu\n{script}\n")).unwrap();
    std::fs::set_permissions(&cargo, std::fs::Permissions::from_mode(0o700)).unwrap();
    CargoImage {
        cargo,
        rustup_home: directory.join("rustup"),
        path: "/usr/bin:/bin".into(),
    }
}

const CARGO_FIXTURE: &str = r#"
test "$CARGO_BUILD_JOBS" = 2
test "$CARGO_INCREMENTAL" = 0
test "$CARGO_HOME" != "$HOME"
test ! -e "$CARGO_HOME/config.toml"
printf '%s\n' "$*" >> "$(dirname "$0")/commands"
case "$*" in
  generate-lockfile)
    printf 'version = 4\n' > Cargo.lock
    ;;
  'vendor --locked --versioned-dirs vendor')
    mkdir -p vendor/fixture-1.0.0/src
    printf 'pub fn item() {}\n' > vendor/fixture-1.0.0/src/lib.rs
    printf '#!/bin/sh\nexit 0\n' > vendor/fixture-1.0.0/build-helper.sh
    chmod 755 vendor/fixture-1.0.0/build-helper.sh
    printf '[source.crates-io]\nreplace-with = "vendored-sources"\n[source.vendored-sources]\ndirectory = "vendor"\n'
    ;;
  *) exit 90 ;;
esac
"#;

#[test]
fn declaration_retains_native_cargo_semantics() {
    let dependencies = parse_dependencies(
        br#"
[dependencies]
regex = "^1.11"
renamed = { package = "itoa", version = "=1.0.18", features = [], default-features = false }
"#,
    )
    .unwrap();
    assert_eq!(dependencies["regex"].as_str(), Some("^1.11"));
    assert_eq!(dependencies["renamed"]["package"].as_str(), Some("itoa"));
    let manifest = compose_manifest(dependencies).unwrap();
    let value: toml::Value = toml::from_str(std::str::from_utf8(&manifest).unwrap()).unwrap();
    let template: toml::Value = toml::from_str(TEMPLATE).unwrap();
    assert_eq!(value["package"], template["package"]);
    assert_eq!(
        value["dependencies"]["serde_json"],
        template["dependencies"]["serde_json"]
    );
    assert_eq!(
        value["dependencies"]["renamed"]["default-features"].as_bool(),
        Some(false)
    );
    assert_eq!(
        value["profile"]["dev"]["debug"].as_str(),
        Some("line-tables-only")
    );
    assert_eq!(
        value["profile"]["test"]["incremental"].as_bool(),
        Some(false)
    );
    assert!(value["workspace"].as_table().unwrap().is_empty());
}

#[test]
fn declaration_rejects_source_sections_and_reserved_wrapper() {
    for declaration in [
        "[dependencies]\na = { path = '/tmp/a' }",
        "[dependencies]\na = { version = '1', path = '/tmp/a' }",
        "[dependencies]\na = { version = '1', git = 'https://example.test/a' }",
        "[dependencies]\na = { version = '1', registry = 'other' }",
        "[dependencies]\na = { version = '1', workspace = true }",
        "[dependencies]\na = { version = '1', optional = true }",
        "[dependencies]\na = { version = '1', unexpected = true }",
        "[dependencies]\na = { features = [] }",
        "[dependencies]\na = { version = '1', features = [false] }",
        "[dependencies]\na = { version = '1', default-features = 'false' }",
        "[dependencies]\na = { version = '1', package = 'serde_json' }",
        "[dependencies]\nserde_json = '1'",
        "[dependencies]\nserde-json = '1'",
        "[dependencies]\nserde = '1'\n[package]\nname = 'override'",
        "[dependencies]\nserde = '1'\n[patch.crates-io]\nother = { path = '/tmp/other' }",
        "[build-dependencies]\nserde = '1'",
        "[target.'cfg(unix)'.dependencies]\nserde = '1'",
        "[dependencies]\na = '1'\na = '2'",
        "[dependencies]\na = {version = ''}",
        "[dependencies]\n'bad/name' = '1'",
    ] {
        assert_eq!(
            parse_dependencies(declaration.as_bytes()).unwrap_err().code,
            ErrorCode::InvalidDeclaration,
            "{declaration}"
        );
    }
    assert!(parse_dependencies(b"[dependencies]\n").is_ok());
    assert!(parse_dependencies(&vec![b' '; DECLARATION_LIMIT + 1]).is_err());
    let many = (0..129).fold(String::from("[dependencies]\n"), |mut value, index| {
        value.push_str(&format!("dependency{index} = '1'\n"));
        value
    });
    assert!(parse_dependencies(many.as_bytes()).is_err());
}

#[tokio::test]
async fn rejected_declaration_never_starts_cargo_or_creates_output() {
    let directory = tempfile::tempdir().unwrap();
    let image = image(
        "printf launched > \"$(dirname \"$0\")/launched\"; exit 1",
        directory.path(),
    );
    let output = directory.path().join("output");
    let error = prepare(
        b"[dependencies]\na = { version = '1', git = 'https://example.test/a' }",
        &output,
        Duration::from_secs(10),
        &image,
    )
    .await
    .unwrap_err();
    assert_eq!(error.code, ErrorCode::InvalidDeclaration);
    assert!(!output.exists());
    assert!(!directory.path().join("launched").exists());
    assert_eq!(std::fs::read_dir(directory.path()).unwrap().count(), 1);
}

#[tokio::test]
async fn exact_root_content_and_records_are_deterministic() {
    let directory = tempfile::tempdir().unwrap();
    let image = image(CARGO_FIXTURE, directory.path());
    let declaration = b"[dependencies]\nitoa = '=1.0.18'\n";
    let first = directory.path().join("first");
    let second = directory.path().join("second");
    let record = prepare(declaration, &first, Duration::from_secs(10), &image)
        .await
        .unwrap();
    let repeated = prepare(declaration, &second, Duration::from_secs(10), &image)
        .await
        .unwrap();
    assert_eq!(record, repeated);
    assert_eq!(
        std::fs::read(first.join("content.tar.gz")).unwrap(),
        std::fs::read(second.join("content.tar.gz")).unwrap()
    );
    assert_eq!(
        std::fs::read(first.join("record.json")).unwrap(),
        std::fs::read(second.join("record.json")).unwrap()
    );
    let calls = std::fs::read_to_string(directory.path().join("commands")).unwrap();
    assert_eq!(
        calls,
        "generate-lockfile\nvendor --locked --versioned-dirs vendor\ngenerate-lockfile\nvendor --locked --versioned-dirs vendor\n"
    );
    assert_eq!(record.lock_sha256, digest(b"version = 4\n"));
    assert!(
        record
            .content
            .files
            .iter()
            .any(|file| file.path.starts_with("vendor/"))
    );
    assert!(
        record
            .content
            .files
            .iter()
            .any(|file| file.path == "vendor/fixture-1.0.0/build-helper.sh" && file.mode == 0o755)
    );
    assert!(
        !record
            .content
            .files
            .iter()
            .any(|file| file.path.contains("target/") || file.path.contains("cargo-home"))
    );
    verify_preparation(declaration, &first, &record, deadline()).unwrap();
    assert_eq!(
        verify_preparation(b"[dependencies]\nitoa = '2'", &first, &record, deadline())
            .unwrap_err()
            .code,
        ErrorCode::IntegrityMismatch
    );
    let serialized = serde_json::to_vec(&record).unwrap();
    let mut changed: PreparationRecord = serde_json::from_slice(&serialized).unwrap();
    changed.lock_sha256 = digest(b"another lock");
    assert_eq!(
        verify_preparation(declaration, &first, &changed, deadline())
            .unwrap_err()
            .code,
        ErrorCode::IntegrityMismatch
    );
    let mut archive = std::fs::read(first.join("content.tar.gz")).unwrap();
    let last = archive.len() - 1;
    archive[last] ^= 1;
    std::fs::write(first.join("content.tar.gz"), archive).unwrap();
    assert_eq!(
        verify_preparation(declaration, &first, &record, deadline())
            .unwrap_err()
            .code,
        ErrorCode::IntegrityMismatch
    );
}

#[test]
fn archive_rejects_links_traversal_duplicate_paths_and_nonportable_config() {
    let directory = tempfile::tempdir().unwrap();
    let root = directory.path().join("root");
    std::fs::create_dir(&root).unwrap();
    symlink("/etc/passwd", root.join("Cargo.toml")).unwrap();
    assert_eq!(
        create_archive(&root, &directory.path().join("content.gz"), deadline())
            .unwrap_err()
            .code,
        ErrorCode::InvalidContent
    );
    for path in [
        "../outside",
        "/absolute",
        "a/../b",
        "a//b",
        "a\\b",
        "a:b",
        "./Cargo.lock",
        "a/\nb",
    ] {
        assert!(!safe_path(path), "{path}");
    }
    assert!(check_vendor_config(b"[source.crates-io]\nreplace-with = 'vendored-sources'\n[source.vendored-sources]\ndirectory = 'vendor'\n").is_ok());
    assert!(check_vendor_config(b"[source.crates-io]\nreplace-with = 'vendored-sources'\n[source.vendored-sources]\ndirectory = '/private/tmp/vendor'\n").is_err());
    let duplicate = rust_prepare_archive::FileRecord {
        path: "Cargo.toml".into(),
        bytes: 0,
        sha256: digest(b""),
        mode: 0o644,
    };
    let record = ArchiveRecord {
        sha256: digest(b""),
        compressed_bytes: 0,
        raw_bytes: 0,
        files: vec![duplicate.clone(), duplicate],
    };
    assert_eq!(
        verify_archive(&directory.path().join("missing"), &record, deadline())
            .unwrap_err()
            .code,
        ErrorCode::InvalidContent
    );
}

#[tokio::test]
async fn subprocess_deadline_output_and_safe_diagnostics_are_bounded() {
    let directory = tempfile::tempdir().unwrap();
    let workspace = directory.path().join("root");
    let home = directory.path().join("cargo-home");
    std::fs::create_dir(&workspace).unwrap();
    std::fs::create_dir(&home).unwrap();
    let noisy = image("exec yes private-dependency-message", directory.path());
    let error = cargo_command(
        &noisy,
        &workspace,
        &home,
        &["generate-lockfile"],
        deadline(),
    )
    .await
    .unwrap_err();
    assert_eq!(error.code, ErrorCode::OutputLimit);
    assert!(!error.diagnostic.contains("private-dependency-message"));
    let sleepy = image("exec /bin/sleep 30", directory.path());
    let start = Instant::now();
    let error = cargo_command(
        &sleepy,
        &workspace,
        &home,
        &["generate-lockfile"],
        start + Duration::from_millis(100),
    )
    .await
    .unwrap_err();
    assert_eq!(error.code, ErrorCode::DeadlineExceeded);
    assert!(start.elapsed() < Duration::from_secs(2));
    let failure = image(
        "printf secret-on-stdout; printf secret-on-stderr >&2; exit 7",
        directory.path(),
    );
    let error = cargo_command(
        &failure,
        &workspace,
        &home,
        &["generate-lockfile"],
        deadline(),
    )
    .await
    .unwrap_err();
    assert_eq!(error.code, ErrorCode::CargoFailed);
    assert!(!format!("{error:?}").contains("secret-on"));
}

#[test]
fn request_and_regular_file_bounds_are_strict() {
    assert!(
        serde_json::from_slice::<Request>(
            br#"{"revision":1,"timeout_seconds":10,"source":"untrusted"}"#
        )
        .is_err()
    );
    for request in [
        Request {
            revision: 2,
            timeout_seconds: 10,
        },
        Request {
            revision: 1,
            timeout_seconds: 0,
        },
        Request {
            revision: 1,
            timeout_seconds: 601,
        },
    ] {
        assert!(request.validate().is_err());
    }
    let directory = tempfile::tempdir().unwrap();
    std::fs::write(directory.path().join("file"), b"large").unwrap();
    assert!(read_regular(&directory.path().join("file"), 4).is_err());
    symlink("file", directory.path().join("link")).unwrap();
    assert!(read_regular(&directory.path().join("link"), 20).is_err());
}

#[test]
fn archive_rejects_file_budgets_special_files_and_trailing_content() {
    let directory = tempfile::tempdir().unwrap();
    let root = directory.path().join("root");
    std::fs::create_dir(&root).unwrap();
    let file = std::fs::File::create(root.join("Cargo.toml")).unwrap();
    file.set_len(32 * 1024 * 1024 + 1).unwrap();
    assert_eq!(
        create_archive(&root, &directory.path().join("oversized.gz"), deadline())
            .unwrap_err()
            .code,
        ErrorCode::ContentLimit
    );
    drop(file);
    std::fs::remove_file(root.join("Cargo.toml")).unwrap();
    let fifo = std::process::Command::new("mkfifo")
        .arg(root.join("Cargo.toml"))
        .status()
        .unwrap();
    assert!(fifo.success());
    assert_eq!(
        create_archive(&root, &directory.path().join("socket.gz"), deadline())
            .unwrap_err()
            .code,
        ErrorCode::InvalidContent
    );
    std::fs::remove_file(root.join("Cargo.toml")).unwrap();
    std::fs::write(root.join("Cargo.toml"), b"original").unwrap();
    let output = directory.path().join("content.gz");
    let mut record = create_archive(&root, &output, deadline()).unwrap();
    let mut bytes = std::fs::read(&output).unwrap();
    bytes.extend_from_slice(b"trailing-content");
    std::fs::write(&output, &bytes).unwrap();
    record.sha256 = digest(&bytes);
    record.compressed_bytes = bytes.len() as u64;
    assert_eq!(
        verify_archive(&output, &record, deadline())
            .unwrap_err()
            .code,
        ErrorCode::IntegrityMismatch
    );
    record.raw_bytes = 256 * 1024 * 1024 + 1;
    assert_eq!(
        verify_archive(&output, &record, deadline())
            .unwrap_err()
            .code,
        ErrorCode::InvalidContent
    );
    record.raw_bytes = 1;
    record.compressed_bytes = 128 * 1024 * 1024 + 1;
    assert_eq!(
        verify_archive(&output, &record, deadline())
            .unwrap_err()
            .code,
        ErrorCode::InvalidContent
    );
}

#[tokio::test]
#[ignore = "Requires native Cargo with crates.io network access and a fresh Cargo home"]
async fn native_cargo_prepares_and_reuses_exact_vendor_content() {
    let directory = tempfile::tempdir().unwrap();
    let host_home = PathBuf::from(std::env::var_os("HOME").unwrap());
    let image = CargoImage {
        cargo: host_home.join(".cargo/bin/cargo"),
        rustup_home: host_home.join(".rustup"),
        path: format!("{}:/usr/bin:/bin", host_home.join(".cargo/bin").display()),
    };
    let declaration = b"[dependencies]\nnumber = {package='itoa',version='=1.0.18',features=[],default-features=false}\n";
    let output = directory.path().join("prepared");
    let record = prepare(declaration, &output, Duration::from_secs(120), &image)
        .await
        .unwrap();
    verify_preparation(declaration, &output, &record, deadline()).unwrap();
    let archive = std::fs::File::open(output.join("content.tar.gz")).unwrap();
    let mut archive = tar::Archive::new(flate2::read::GzDecoder::new(archive));
    let mut lock = Vec::new();
    let mut manifest = Vec::new();
    for entry in archive.entries().unwrap() {
        let mut entry = entry.unwrap();
        let path = entry.path().unwrap();
        if path == Path::new("Cargo.lock") {
            entry.read_to_end(&mut lock).unwrap();
        } else if path == Path::new("Cargo.toml") {
            entry.read_to_end(&mut manifest).unwrap();
        }
    }
    assert_eq!(digest(&lock), record.lock_sha256);
    assert_eq!(digest(&manifest), record.manifest_sha256);
    let manifest: toml::Value = toml::from_str(std::str::from_utf8(&manifest).unwrap()).unwrap();
    assert_eq!(
        manifest["dependencies"]["number"]["package"].as_str(),
        Some("itoa")
    );
    let lock: toml::Value = toml::from_str(std::str::from_utf8(&lock).unwrap()).unwrap();
    assert!(
        lock["package"]
            .as_array()
            .unwrap()
            .iter()
            .any(|package| package["name"].as_str() == Some("itoa")
                && package["version"].as_str() == Some("1.0.18"))
    );
    assert!(
        record
            .content
            .files
            .iter()
            .any(|file| file.path == "vendor/itoa-1.0.18/.cargo-checksum.json")
    );
    assert_eq!(
        std::fs::read_dir(directory.path()).unwrap().count(),
        1,
        "fresh Cargo home and work root must be removed"
    );
}

#[test]
fn extracted_inventory_rejects_prefix_collisions_and_linked_staging() {
    let directory = tempfile::tempdir().unwrap();
    let root = directory.path().join("root");
    std::fs::create_dir(&root).unwrap();
    std::fs::create_dir(root.join("vendor")).unwrap();
    std::fs::write(root.join("vendor/package"), b"native").unwrap();
    let archive = directory.path().join("archive.gz");
    let record = create_archive(&root, &archive, deadline()).unwrap();
    let outside = tempfile::tempdir().unwrap();
    let linked = directory.path().join("linked");
    symlink(outside.path(), &linked).unwrap();
    assert!(rust_prepare_archive::extract_archive(&archive, &record, &linked, deadline()).is_err());
    assert!(std::fs::read_dir(outside.path()).unwrap().next().is_none());
    let mut collision = ArchiveRecord {
        sha256: record.sha256.clone(),
        compressed_bytes: record.compressed_bytes,
        raw_bytes: record.raw_bytes,
        files: record.files.clone(),
    };
    collision.files.push(rust_prepare_archive::FileRecord {
        path: "vendor/package/child".into(),
        bytes: 0,
        sha256: digest(b""),
        mode: 0o644,
    });
    assert!(verify_archive(&archive, &collision, deadline()).is_err());
    let destination = directory.path().join("destination");
    std::fs::create_dir(&destination).unwrap();
    rust_prepare_archive::extract_archive(&archive, &record, &destination, deadline()).unwrap();
    assert_eq!(
        std::fs::read(destination.join("vendor/package")).unwrap(),
        b"native"
    );
    assert!(
        rust_prepare_archive::extract_archive(&archive, &record, &destination, deadline()).is_err()
    );
}

#[tokio::test]
async fn broker_acquisition_records_fixed_sources_before_content_capture() {
    let directory = tempfile::tempdir().unwrap();
    let image = image(CARGO_FIXTURE, directory.path());
    let declaration = b"[dependencies]\nitoa='=1.0.18'\n";
    let output = directory.path().join("broker");
    let record = prepare_profile(
        Profile::Broker,
        declaration,
        &output,
        Duration::from_secs(10),
        &image,
    )
    .await
    .unwrap();
    assert_eq!(
        record.wrapper_sha256,
        digest(Profile::Broker.wrapper().as_bytes())
    );
    assert_ne!(record.wrapper_sha256, digest(WRAPPER.as_bytes()));
    verify_record_profile(Profile::Broker, declaration, &record).unwrap();
    assert!(verify_record(declaration, &record).is_err());
    for (path, bytes) in Profile::Broker.sources() {
        assert!(
            record
                .content
                .files
                .iter()
                .any(|file| file.path == *path && file.sha256 == digest(bytes.as_bytes()))
        );
        let mut forged: PreparationRecord =
            serde_json::from_slice(&serde_json::to_vec(&record).unwrap()).unwrap();
        forged
            .content
            .files
            .iter_mut()
            .find(|file| file.path == *path)
            .unwrap()
            .sha256 = digest(b"forged image module");
        assert_eq!(
            verify_record_profile(Profile::Broker, declaration, &forged)
                .unwrap_err()
                .code,
            ErrorCode::IntegrityMismatch
        );
    }
    let mut forged: PreparationRecord =
        serde_json::from_slice(&serde_json::to_vec(&record).unwrap()).unwrap();
    forged.manifest_sha256 =
        digest(&compose_manifest(parse_dependencies(declaration).unwrap()).unwrap());
    assert!(verify_record_profile(Profile::Broker, declaration, &forged).is_err());
}

#[test]
fn archive_keeps_fixed_broker_modules_and_refuses_other_source_paths() {
    let directory = tempfile::tempdir().unwrap();
    let root = directory.path().join("root");
    std::fs::create_dir_all(root.join("src")).unwrap();
    for (path, bytes) in Profile::Broker.sources() {
        std::fs::write(root.join(path), bytes).unwrap();
    }
    let archive = directory.path().join("broker.gz");
    let record = create_archive(&root, &archive, deadline()).unwrap();
    assert_eq!(record.files.len(), Profile::Broker.sources().len());
    verify_archive(&archive, &record, deadline()).unwrap();
    for path in ["src/other.rs", "src/platform_client.rs.backup"] {
        std::fs::write(root.join(path), b"unexpected source").unwrap();
        assert!(create_archive(&root, &archive, deadline()).is_err());
        std::fs::remove_file(root.join(path)).unwrap();
    }
}

#[tokio::test]
async fn broker_dependency_override_stops_before_any_acquisition_effect() {
    let directory = tempfile::tempdir().unwrap();
    let image = image(
        "printf launched > \"$(dirname \"$0\")/launched\"; exit 1",
        directory.path(),
    );
    let output = directory.path().join("broker");
    let error = prepare_profile(
        Profile::Broker,
        b"[dependencies]\nserde='1'\n",
        &output,
        Duration::from_secs(10),
        &image,
    )
    .await
    .unwrap_err();
    assert_eq!(error.code, ErrorCode::InvalidDeclaration);
    assert!(!output.exists());
    assert!(!directory.path().join("launched").exists());
    assert_eq!(std::fs::read_dir(directory.path()).unwrap().count(), 1);
}
