use super::*;
use std::os::unix::fs::symlink;

fn fixture() -> (tempfile::TempDir, Vec<u8>, String) {
    let root = tempfile::tempdir().unwrap();
    let bytes =
        include_bytes!("../../../libs/proto/elitea/runtime/v1/code_workspace_manifest_v1.json")
            .to_vec();
    let value: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
    (root, bytes, value["root"].as_str().unwrap().to_owned())
}
fn context(root: &str) -> Identity<'_> {
    Identity {
        job: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        request: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
        root,
        pod: None,
    }
}
fn action(
    command: &str,
    directory: &Path,
    identity: &Identity<'_>,
    index: Option<u32>,
    content: Option<&[u8]>,
) -> io::Result<Vec<u8>> {
    let mut output = Vec::new();
    let bytes = if command == "--workspace-write" || command == "--workspace-manifest" {
        Some(content.unwrap_or_default().len() as u64)
    } else {
        None
    };
    let header = Header {
        revision: 1,
        job_key: identity.job.into(),
        request_digest: identity.request.into(),
        manifest_sha256: identity.root.into(),
        pod_uid: None,
        index,
        bytes,
    };
    apply(
        command,
        directory,
        identity,
        header,
        &mut io::Cursor::new(content.unwrap_or_default()),
        &mut output,
    )?;
    Ok(output)
}
#[test]
fn snapshot_hydration_is_indexed_immutable_and_releases_only_after_verification() {
    let (root, bytes, digest) = fixture();
    let id = context(&digest);
    action("--workspace-manifest", root.path(), &id, None, Some(&bytes)).unwrap();
    assert!(action("--workspace-release", root.path(), &id, None, None).is_err());
    action(
        "--workspace-write",
        root.path(),
        &id,
        Some(0),
        Some(b"hello"),
    )
    .unwrap();
    action(
        "--workspace-write",
        root.path(),
        &id,
        Some(0),
        Some(b"hello"),
    )
    .unwrap();
    assert_eq!(
        read_bounded(&root.path().join("src/greeting.txt"), 5).unwrap(),
        b"hello"
    );
    assert!(
        action(
            "--workspace-write",
            root.path(),
            &id,
            Some(0),
            Some(b"other")
        )
        .is_err()
    );
    action("--workspace-finalize", root.path(), &id, None, None).unwrap();
    action("--workspace-release", root.path(), &id, None, None).unwrap();
    assert!(
        action(
            "--workspace-write",
            root.path(),
            &id,
            Some(0),
            Some(b"hello")
        )
        .is_err()
    );
    let receipt: serde_json::Value =
        serde_json::from_slice(&action("--workspace-probe", root.path(), &id, None, None).unwrap())
            .unwrap();
    assert_eq!(
        receipt,
        serde_json::json!({"revision":1,"next_index":1,"ready":true})
    );
}
#[test]
fn crash_partial_file_is_removed_and_original_published_file_is_reused() {
    let (root, bytes, digest) = fixture();
    let id = context(&digest);
    action("--workspace-manifest", root.path(), &id, None, Some(&bytes)).unwrap();
    fs::write(root.path().join(TEMP), b"partial").unwrap();
    cleanup_pending(root.path()).unwrap();
    action(
        "--workspace-write",
        root.path(),
        &id,
        Some(0),
        Some(b"hello"),
    )
    .unwrap();
    let original = read_bounded(&root.path().join("src/greeting.txt"), 5).unwrap();
    fs::remove_file(root.path().join(PROGRESS)).unwrap();
    action(
        "--workspace-write",
        root.path(),
        &id,
        Some(0),
        Some(b"hello"),
    )
    .unwrap();
    assert_eq!(
        original,
        read_bounded(&root.path().join("src/greeting.txt"), 5).unwrap()
    );
}
#[test]
fn symlink_extra_path_changed_hash_and_skipped_index_fail_before_ready() {
    for kind in ["symlink", "extra", "changed", "skip"] {
        let (root, bytes, digest) = fixture();
        let id = context(&digest);
        action("--workspace-manifest", root.path(), &id, None, Some(&bytes)).unwrap();
        if kind == "skip" {
            assert!(
                action(
                    "--workspace-write",
                    root.path(),
                    &id,
                    Some(1),
                    Some(b"hello")
                )
                .is_err()
            );
            continue;
        }
        if kind == "symlink" {
            let target = tempfile::tempdir().unwrap();
            symlink(target.path(), root.path().join("src")).unwrap();
            assert!(
                action(
                    "--workspace-write",
                    root.path(),
                    &id,
                    Some(0),
                    Some(b"hello")
                )
                .is_err()
            );
            assert!(fs::read_dir(target.path()).unwrap().next().is_none());
            continue;
        }
        action(
            "--workspace-write",
            root.path(),
            &id,
            Some(0),
            Some(b"hello"),
        )
        .unwrap();
        if kind == "extra" {
            fs::write(root.path().join("extra.txt"), b"x").unwrap()
        } else {
            fs::write(root.path().join("src/greeting.txt"), b"other").unwrap()
        }
        assert!(action("--workspace-release", root.path(), &id, None, None).is_err());
        assert!(!root.path().join(READY).exists());
    }
}
#[test]
fn helper_header_refuses_wrong_namespace_nulls_and_duplicates() {
    let (_, _, digest) = fixture();
    let id = context(&digest);
    let value = Header {
        revision: 1,
        job_key: id.job.into(),
        request_digest: id.request.into(),
        manifest_sha256: id.root.into(),
        pod_uid: None,
        index: None,
        bytes: None,
    };
    let raw = serde_json::to_vec(&value).unwrap();
    for altered in [
        raw.iter()
            .copied()
            .chain(b",\n".iter().copied())
            .collect::<Vec<_>>(),
        format!(
            "{},\"index\":null}}\n",
            std::str::from_utf8(&raw[..raw.len() - 1]).unwrap()
        )
        .into_bytes(),
        format!(
            "{},\"revision\":1}}\n",
            std::str::from_utf8(&raw[..raw.len() - 1]).unwrap()
        )
        .into_bytes(),
    ] {
        assert!(header(&mut io::Cursor::new(altered), &id).is_err())
    }
    let mut wrong = value;
    wrong.job_key = "f".repeat(64);
    let mut raw = serde_json::to_vec(&wrong).unwrap();
    raw.push(b'\n');
    assert!(header(&mut io::Cursor::new(raw), &id).is_err());
}
#[test]
fn kernel_mount_proof_refuses_writable_duplicate_missing_and_child_overmount() {
    let read = b"20 10 0:1 / /workspace/repository ro,nosuid - tmpfs tmpfs rw\n";
    assert!(read_only_mount(read));
    for bad in [b"20 10 0:1 / /workspace/repository rw - tmpfs tmpfs rw\n".as_slice(),b"20 10 0:1 / /workspace/repository ro - tmpfs tmpfs rw\n21 10 0:2 / /workspace/repository ro - tmpfs tmpfs rw\n",b"20 10 0:1 / /workspace ro - tmpfs tmpfs rw\n",b"20 10 0:1 / /workspace/repository ro - tmpfs tmpfs rw\n21 20 0:2 / /workspace/repository/src rw - tmpfs tmpfs rw\n"]{assert!(!read_only_mount(bad))}
}

#[test]
fn last_file_reports_not_ready_until_explicit_finalization() {
    let (root, bytes, digest) = fixture();
    let id = context(&digest);
    action("--workspace-manifest", root.path(), &id, None, Some(&bytes)).unwrap();
    action(
        "--workspace-write",
        root.path(),
        &id,
        Some(0),
        Some(b"hello"),
    )
    .unwrap();
    let probe = action("--workspace-probe", root.path(), &id, None, None).unwrap();
    assert_eq!(
        serde_json::from_slice::<serde_json::Value>(&probe).unwrap(),
        serde_json::json!({"revision":1,"next_index":1,"ready":false})
    );
    assert!(!root.path().join(RELEASE).exists());
    action("--workspace-finalize", root.path(), &id, None, None).unwrap();
    assert!(!root.path().join(RELEASE).exists());
    action("--workspace-release", root.path(), &id, None, None).unwrap();
    action("--workspace-release", root.path(), &id, None, None).unwrap();
}

#[test]
fn corrupted_cursor_content_or_mode_never_acknowledges_previous_progress() {
    for kind in [
        "advanced_cursor",
        "cursor_encoding",
        "changed_content",
        "changed_mode",
        "changed_ready",
    ] {
        let (root, bytes, digest) = fixture();
        let id = context(&digest);
        action("--workspace-manifest", root.path(), &id, None, Some(&bytes)).unwrap();
        if kind == "advanced_cursor" {
            fs::write(root.path().join(PROGRESS), b"1").unwrap();
        } else {
            action(
                "--workspace-write",
                root.path(),
                &id,
                Some(0),
                Some(b"hello"),
            )
            .unwrap();
            match kind {
                "cursor_encoding" => fs::write(root.path().join(PROGRESS), b"01").unwrap(),
                "changed_content" => {
                    fs::write(root.path().join("src/greeting.txt"), b"other").unwrap()
                }
                "changed_mode" => fs::set_permissions(
                    root.path().join("src/greeting.txt"),
                    fs::Permissions::from_mode(0o700),
                )
                .unwrap(),
                "changed_ready" => {
                    fs::write(root.path().join(READY), b"forged ready proof").unwrap()
                }
                _ => unreachable!(),
            }
        }
        assert!(
            action("--workspace-probe", root.path(), &id, None, None).is_err(),
            "accepted {kind}"
        );
        assert!(!root.path().join(RELEASE).exists());
    }
}

#[test]
fn changed_manifest_or_pod_identity_does_not_replace_original_files() {
    let (root, bytes, digest) = fixture();
    let id = context(&digest);
    action("--workspace-manifest", root.path(), &id, None, Some(&bytes)).unwrap();
    let changed = bytes
        .iter()
        .copied()
        .chain(b" ".iter().copied())
        .collect::<Vec<_>>();
    assert!(
        action(
            "--workspace-manifest",
            root.path(),
            &id,
            None,
            Some(&changed)
        )
        .is_err()
    );
    assert_eq!(
        read_bounded(&root.path().join(MANIFEST), 4 << 20).unwrap(),
        bytes
    );
    let pod = Identity {
        pod: Some("original-pod-uid"),
        ..id
    };
    let request = Header {
        revision: 1,
        job_key: pod.job.into(),
        request_digest: pod.request.into(),
        manifest_sha256: pod.root.into(),
        pod_uid: Some("replacement-pod-uid".into()),
        index: None,
        bytes: None,
    };
    let mut encoded = serde_json::to_vec(&request).unwrap();
    encoded.push(b'\n');
    assert!(header(&mut io::Cursor::new(encoded), &pod).is_err());
    assert!(
        fs::read_dir(root.path())
            .unwrap()
            .all(|entry| entry.unwrap().file_name() == MANIFEST)
    );
}
