use super::*;
use std::fmt::Write as _;
use std::os::unix::fs::symlink;

fn fixture() -> (Vec<u8>, String, Vec<BundleFile>) {
    let files = vec![
        BundleFile {
            name: LOCK_NAME.into(),
            bytes: 2,
            sha256: hex(digest::digest(&digest::SHA256, b"{}").as_ref()),
        },
        BundleFile {
            name: "example-1.0-py3-none-any.whl".into(),
            bytes: 5,
            sha256: hex(digest::digest(&digest::SHA256, b"wheel").as_ref()),
        },
    ];
    let content = BundleContent {
        revision: 1,
        runtime: "pyodide-0.29.0",
        requirements: &[],
        files: &files,
    };
    let native = serde_json::to_vec(&content).unwrap();
    let root = hex(digest::digest(&digest::SHA256, &native).as_ref());
    let mut record = String::from_utf8(native).unwrap();
    record.pop();
    write!(record, ",\"digest\":\"{root}\"}}").unwrap();
    (record.into_bytes(), root, files)
}
fn private_directory() -> tempfile::TempDir {
    tempfile::Builder::new()
        .permissions(std::fs::Permissions::from_mode(0o700))
        .tempdir()
        .unwrap()
}
fn grant(root: &str) -> SignedSandboxJobGrantV1 {
    let root = (0..32)
        .map(|index| u8::from_str_radix(&root[index * 2..index * 2 + 2], 16).unwrap())
        .collect();
    SignedSandboxJobGrantV1 {
        key_id: "fixture-key".into(),
        signature: vec![7; 64],
        claims_bytes: SandboxJobGrantClaimsV1 {
            revision: 3,
            dependency_bundle_sha256: root,
            ..Default::default()
        }
        .encode_to_vec(),
    }
}

#[test]
fn native_json_identity_and_constructor_bounds() {
    let (bytes, root, _) = fixture();
    let parsed = PythonDependencyBundle::parse(&bytes, &root).unwrap();
    assert_eq!(parsed.root(), root);
    assert_eq!(parsed.canonical, bytes);
    assert!(PythonDependencyBundle::parse(&bytes, &"a".repeat(64)).is_err());
    assert!(PythonDependencyBundle::parse(&bytes, &root.to_uppercase()).is_err());
    assert!(PythonDependencyBundle::parse(&vec![b' '; METADATA_LIMIT + 1], &root).is_err());
    let duplicate = String::from_utf8(bytes.clone())
        .unwrap()
        .replacen('{', "{\"revision\":1,", 1);
    assert!(PythonDependencyBundle::parse(duplicate.as_bytes(), &root).is_err());
    for (field, value) in [
        ("revision", serde_json::json!(2)),
        ("runtime", serde_json::json!("pyodide-0.28.0")),
        ("requirements", serde_json::json!(null)),
        ("requirements", serde_json::json!(["é"])),
        ("requirements", serde_json::json!(["line\nbreak"])),
        ("requirements", serde_json::json!([""])),
        ("requirements", serde_json::json!(["x".repeat(257)])),
        ("files", serde_json::json!([])),
        ("unknown", serde_json::json!(true)),
    ] {
        let mut changed: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        changed[field] = value;
        assert!(
            PythonDependencyBundle::parse(&serde_json::to_vec(&changed).unwrap(), &root).is_err()
        );
    }
}

#[test]
fn safe_file_names_prevent_path_interpretation() {
    for name in [
        "../escape.whl",
        "..",
        "x/y.whl",
        "x%2fy.whl",
        "x?token.whl",
        "x#f.whl",
        "x\\y.whl",
        "x\0.whl",
        "x.json",
        "x.whl\n",
    ] {
        assert!(!safe_name(name), "{name:?}");
    }
    assert!(safe_name(LOCK_NAME));
    assert!(safe_name("example_package-1.2+build-py3-none-any.whl"));
}

#[test]
fn grant_headers_are_sensitive_and_separate_from_execution_authority() {
    let (_, root, _) = fixture();
    let original = grant(&root);
    assert!(grant_header(&original, &root).unwrap().is_sensitive());
    assert!(grant_header(&original, &"a".repeat(64)).is_err());
    for revision in [1, 2, 4] {
        let mut wrong = grant(&root);
        let mut claims = SandboxJobGrantClaimsV1::decode(wrong.claims_bytes.as_slice()).unwrap();
        claims.revision = revision;
        wrong.claims_bytes = claims.encode_to_vec();
        assert!(grant_header(&wrong, &root).is_err());
    }
    let mut wrong = grant(&root);
    let mut claims = SandboxJobGrantClaimsV1::decode(wrong.claims_bytes.as_slice()).unwrap();
    claims.cancel_only = true;
    wrong.claims_bytes = claims.encode_to_vec();
    assert!(grant_header(&wrong, &root).is_err());
    let mut malformed = grant(&root);
    malformed.signature.pop();
    assert!(grant_header(&malformed, &root).is_err());
    let mut duplicate = grant(&root);
    duplicate.claims_bytes.extend_from_slice(&[8, 3]);
    assert!(grant_header(&duplicate, &root).is_err());
}

#[test]
fn origin_and_staging_refuse_unsafe_configuration() {
    assert_eq!(
        canonical_origin("https://CONTENT.internal:8443/").unwrap(),
        "https://content.internal:8443"
    );
    for origin in [
        "http://content.internal",
        "https://user@content.internal",
        "https://content.internal/path",
        "https://content.internal?x",
        "https://content.internal#f",
        "https://content.internal:0",
        "https://content.internal:99999",
    ] {
        assert!(canonical_origin(origin).is_err(), "{origin}");
    }
    let directory = private_directory();
    let private = directory.path().canonicalize().unwrap();
    assert!(validate_staging(&private, 1, Duration::from_secs(30)).is_ok());
    assert!(validate_staging(&private, 0, Duration::from_secs(30)).is_err());
    assert!(validate_staging(&private, 33, Duration::from_secs(30)).is_err());
    assert!(validate_staging(&private, 1, Duration::ZERO).is_err());
    assert!(validate_staging(&private, 1, Duration::from_secs(301)).is_err());
    let alias = private.join("alias");
    symlink(&private, &alias).unwrap();
    assert!(validate_staging(&alias, 1, Duration::from_secs(30)).is_err());
    std::fs::set_permissions(&private, std::fs::Permissions::from_mode(0o755)).unwrap();
    assert!(validate_staging(&private, 1, Duration::from_secs(30)).is_err());
}

#[tokio::test]
async fn local_upload_verification_refuses_symlinks_and_changed_bytes() {
    let (bytes, root, files) = fixture();
    let bundle = PythonDependencyBundle::parse(&bytes, &root).unwrap();
    let directory = tempfile::tempdir().unwrap();
    tokio::fs::write(directory.path().join(LOCK_NAME), b"{}")
        .await
        .unwrap();
    let path = directory.path().join(&files[1].name);
    tokio::fs::write(&path, b"wheel").await.unwrap();
    assert!(
        open_verified_file(directory.path(), &bundle.files[1])
            .await
            .is_ok()
    );
    tokio::fs::write(&path, b"wrong").await.unwrap();
    assert!(matches!(
        open_verified_file(directory.path(), &bundle.files[1]).await,
        Err(DependencyContentError::Integrity)
    ));
    tokio::fs::remove_file(&path).await.unwrap();
    symlink(directory.path().join(LOCK_NAME), &path).unwrap();
    assert!(
        open_verified_file(directory.path(), &bundle.files[1])
            .await
            .is_err()
    );
}

fn response(status: StatusCode, media: &str, declared: usize, body: Vec<u8>) -> Response {
    http::Response::builder()
        .status(status)
        .header(CONTENT_TYPE, media)
        .header(CONTENT_LENGTH, declared)
        .body(body)
        .unwrap()
        .into()
}

#[tokio::test]
async fn downloads_reject_corruption_and_incorrect_lengths_before_metadata_publication() {
    let (_, _, files) = fixture();
    let private = private_directory();
    let wheel = &files[1];
    for (length, bytes, media) in [
        (5, b"wrong".to_vec(), "application/octet-stream"),
        (4, b"wheel".to_vec(), "application/octet-stream"),
        (6, b"wheel!".to_vec(), "application/octet-stream"),
        (5, b"wheel".to_vec(), "text/html"),
    ] {
        let staging = tempfile::tempdir_in(private.path()).unwrap();
        assert!(matches!(
            write_verified_file(
                response(StatusCode::OK, media, length, bytes),
                wheel,
                staging.path()
            )
            .await,
            Err(DependencyContentError::Integrity)
        ));
        assert!(!staging.path().join(RECORD_NAME).exists());
    }
    let staging = tempfile::tempdir_in(private.path()).unwrap();
    write_verified_file(
        response(
            StatusCode::OK,
            "application/octet-stream",
            5,
            b"wheel".to_vec(),
        ),
        wheel,
        staging.path(),
    )
    .await
    .unwrap();
    assert_eq!(
        std::fs::read(staging.path().join(&wheel.name)).unwrap(),
        b"wheel"
    );
}

#[tokio::test]
async fn metadata_limits_and_service_errors_are_explicit() {
    let mut too_large = response(
        StatusCode::OK,
        "application/json",
        METADATA_LIMIT + 1,
        vec![b' '; METADATA_LIMIT + 1],
    );
    assert!(check_response(&too_large, StatusCode::OK, "application/json", None).is_err());
    assert!(bounded_metadata(&mut too_large).await.is_err());
    let mut mismatched = response(StatusCode::OK, "application/json", 3, b"{}".to_vec());
    assert!(bounded_metadata(&mut mismatched).await.is_err());
    for status in [StatusCode::UNAUTHORIZED, StatusCode::FORBIDDEN] {
        assert!(matches!(
            check_status(&response(status, "text/plain", 0, vec![]), StatusCode::OK),
            Err(DependencyContentError::Authority)
        ));
    }
    for status in [
        StatusCode::UNPROCESSABLE_ENTITY,
        StatusCode::PAYLOAD_TOO_LARGE,
    ] {
        assert!(matches!(
            check_status(&response(status, "text/plain", 0, vec![]), StatusCode::OK),
            Err(DependencyContentError::Integrity)
        ));
    }
    for status in [
        StatusCode::FOUND,
        StatusCode::NOT_FOUND,
        StatusCode::SERVICE_UNAVAILABLE,
    ] {
        assert!(
            matches!(check_status(&response(status, "text/plain", 0, vec![]), StatusCode::OK), Err(DependencyContentError::Unavailable { status: value }) if value == status.as_u16())
        );
    }
}

#[tokio::test]
async fn whole_bundle_deadline_releases_capacity_without_detached_grant_work() {
    let (_, root, _) = fixture();
    let directory = tempfile::tempdir().unwrap();
    let client = DependencyContentClient {
        client: Client::new(),
        origin: "https://never-called.invalid".into(),
        staging: directory.path().into(),
        slots: Semaphore::new(1),
        deadline: Duration::from_millis(1),
    };
    assert!(matches!(
        client.download(&root, std::future::pending).await,
        Err(DependencyContentError::Timeout)
    ));
    assert_eq!(client.slots.available_permits(), 1);
    assert_eq!(std::fs::read_dir(directory.path()).unwrap().count(), 0);
}

#[tokio::test]
#[ignore = "Requires a private TLS fixture server and a native resolved Python bundle."]
async fn live_native_mtls_download_and_publication() {
    let origin = std::env::var("ELITEA_TEST_BUNDLE_ORIGIN").unwrap();
    let tls = PathBuf::from(std::env::var("ELITEA_TEST_BUNDLE_TLS").unwrap());
    let source = PathBuf::from(std::env::var("ELITEA_TEST_BUNDLE_SOURCE").unwrap());
    let native = std::fs::read(source.join(RECORD_NAME)).unwrap();
    let root = serde_json::from_slice::<serde_json::Value>(&native).unwrap()["digest"]
        .as_str()
        .unwrap()
        .to_owned();
    let bundle = PythonDependencyBundle::parse(&native, &root).unwrap();
    let private = private_directory();
    let certificate = std::fs::read(tls.join("client-combined.pem")).unwrap();
    let ca = std::fs::read(tls.join("ca.pem")).unwrap();
    let original_grant =
        SignedSandboxJobGrantV1::decode(std::fs::read(tls.join("grant.pb")).unwrap().as_slice())
            .unwrap();
    let client = DependencyContentClient::new(
        &origin,
        &ca,
        &certificate,
        private.path(),
        1,
        Duration::from_secs(30),
    )
    .unwrap();
    let mut download_grants = 0;
    let downloaded = client
        .download(&root, || {
            download_grants += 1;
            std::future::ready(Ok(original_grant.clone()))
        })
        .await
        .unwrap();
    for file in &bundle.files {
        assert_eq!(
            std::fs::read(downloaded.directory().join(&file.name)).unwrap(),
            std::fs::read(source.join(&file.name)).unwrap()
        );
    }
    assert_eq!(download_grants, bundle.files.len() + 1);
    assert_eq!(downloaded.bundle().canonical, bundle.canonical);
    let mut publication_grants = 0;
    client
        .publish(downloaded.bundle(), downloaded.directory(), || {
            publication_grants += 1;
            std::future::ready(Ok(original_grant.clone()))
        })
        .await
        .unwrap();
    assert_eq!(publication_grants, bundle.files.len() + 1);
    downloaded.close().unwrap();
    assert_eq!(std::fs::read_dir(private.path()).unwrap().count(), 0);
    let mut invalid_signature = original_grant.clone();
    invalid_signature.signature[0] ^= 1;
    assert!(matches!(
        client
            .download(&root, || std::future::ready(Ok(invalid_signature.clone())))
            .await,
        Err(DependencyContentError::Authority)
    ));
    assert_eq!(std::fs::read_dir(private.path()).unwrap().count(), 0);
    let occupied = client.slots.acquire().await.unwrap();
    assert!(matches!(
        client
            .download(&root, || std::future::ready(Ok(grant(&root))))
            .await,
        Err(DependencyContentError::Busy)
    ));
    drop(occupied);
}
