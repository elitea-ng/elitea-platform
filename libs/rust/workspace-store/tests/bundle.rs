//! Bundles: write → read → verify → refs equal; git itself reads ours; a
//! tampered bundle is refused with nothing usable; sha256 refused.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::format_push_string)]

mod common;

use std::collections::BTreeMap;

use elitea_workspace_store::bundle::{self, Expect, Limits};
use elitea_workspace_store::objects::{self, ChangeKind, ObjectDb};

use common::{commit_file, git, repo};

#[test]
fn a_compact_bundle_round_trips_and_its_refs_and_trees_are_equal() {
    let (dir, top, first) = repo();
    let second = commit_file(&top, "src/new.txt", "new\n");
    let db = ObjectDb::open_git_dir(&top.join(".git")).unwrap();
    let refs = vec![
        ("refs/elitea/base".to_owned(), objects::oid(&first).unwrap()),
        (
            "refs/elitea/handoff/1".to_owned(),
            objects::oid(&second).unwrap(),
        ),
    ];
    let written = bundle::write_compact(&db, &refs, Limits::default()).unwrap();
    assert_eq!(written.commits, 2);
    assert_eq!(written.blobs, 5);
    let header = bundle::parse_header(&written.bytes).unwrap();
    assert_eq!(header.prerequisites, [] as [objects::ObjectId; 0]);
    assert_eq!(header.refs, refs);

    let quarantine = dir.path().join("q");
    bundle::init_quarantine(&quarantine).unwrap();
    let expected: BTreeMap<String, String> = header.ref_map();
    let verified = bundle::verify_into(
        &written.bytes,
        &quarantine,
        &Expect {
            sha256: Some(&written.sha256),
            bytes: Some(written.bytes.len() as u64),
            refs: Some(&expected),
        },
        Limits::default(),
    )
    .unwrap();
    assert_eq!(verified.header.refs, refs);
    assert_eq!(verified.objects, written.objects);
    // The trees read back are the trees written, file by file.
    let tree = |db: &ObjectDb, commit: &str| {
        db.flatten(&db.commit(&objects::oid(commit).unwrap()).unwrap().tree)
            .unwrap()
    };
    assert_eq!(tree(&db, &second), tree(&verified.db, &second));
    let changes = objects::diff(&tree(&verified.db, &first), &tree(&verified.db, &second));
    assert_eq!(changes.len(), 1);
    assert_eq!(changes[0].path, "src/new.txt");
    assert_eq!(changes[0].kind, ChangeKind::Added);
    let files = tree(&verified.db, &first);
    assert!(files["run.sh"].is_executable());
    assert_eq!(
        verified.db.blob(&files["src/deep/data.bin"].oid).unwrap(),
        [0u8, 1, 2, 255, 0, 7]
    );
}

#[test]
fn git_reads_the_bundle_this_crate_writes() {
    let (dir, top, head) = repo();
    let db = ObjectDb::open_git_dir(&top.join(".git")).unwrap();
    let refs = vec![(
        "refs/elitea/handoff/1".to_owned(),
        objects::oid(&head).unwrap(),
    )];
    let written = bundle::write_compact(&db, &refs, Limits::default()).unwrap();
    let path = dir.path().join("seg.bundle");
    std::fs::write(&path, &written.bytes).unwrap();
    let other = dir.path().join("other");
    std::fs::create_dir(&other).unwrap();
    git(&other, &["init", "-q"]);
    let listed = git(&other, &["bundle", "verify", path.to_str().unwrap()]);
    assert!(
        listed.contains("refs/elitea/handoff/1") || listed.is_empty() || listed.contains("okay"),
        "{listed}"
    );
    git(
        &other,
        &[
            "fetch",
            "-q",
            path.to_str().unwrap(),
            "refs/elitea/handoff/1:refs/heads/got",
        ],
    );
    assert_eq!(git(&other, &["rev-parse", "refs/heads/got"]), head);
    git(&other, &["fsck", "--strict"]);
}

#[test]
fn ours_reads_a_bundle_git_writes() {
    let (dir, top, head) = repo();
    git(&top, &["update-ref", "refs/elitea/cloud", &head]);
    let path = dir.path().join("git.bundle");
    git(
        &top,
        &[
            "bundle",
            "create",
            "-q",
            path.to_str().unwrap(),
            "refs/elitea/cloud",
        ],
    );
    let bytes = std::fs::read(&path).unwrap();
    let quarantine = dir.path().join("q");
    bundle::init_quarantine(&quarantine).unwrap();
    let verified =
        bundle::verify_into(&bytes, &quarantine, &Expect::default(), Limits::default()).unwrap();
    assert_eq!(
        verified.ref_id("refs/elitea/cloud").unwrap().to_string(),
        head
    );
}

fn tampered_is_refused(flip: impl Fn(&mut Vec<u8>, usize)) -> String {
    let (dir, top, head) = repo();
    let db = ObjectDb::open_git_dir(&top.join(".git")).unwrap();
    let refs = vec![(
        "refs/elitea/handoff/1".to_owned(),
        objects::oid(&head).unwrap(),
    )];
    let written = bundle::write_compact(&db, &refs, Limits::default()).unwrap();
    let mut bytes = written.bytes.clone();
    let offset = bundle::parse_header(&bytes).unwrap().pack_offset;
    flip(&mut bytes, offset);
    let quarantine = dir.path().join("q");
    bundle::init_quarantine(&quarantine).unwrap();
    bundle::verify_into(&bytes, &quarantine, &Expect::default(), Limits::default())
        .unwrap_err()
        .to_string()
}

#[test]
fn a_tampered_bundle_is_refused() {
    // A byte inside the compressed objects: the pack is broken.
    let error = tampered_is_refused(|bytes, offset| bytes[offset + 40] ^= 0x55);
    assert!(error.starts_with("integrity"), "{error}");
    // The trailer: the pack checksum does not match.
    let error = tampered_is_refused(|bytes, _| {
        let last = bytes.len() - 1;
        bytes[last] ^= 0x01;
    });
    assert!(error.starts_with("integrity"), "{error}");
    // The ref line names another object: connectivity fails.
    let error = tampered_is_refused(|bytes, _| {
        let at = b"# v2 git bundle\n".len();
        bytes[at] = if bytes[at] == b'0' { b'1' } else { b'0' };
    });
    assert!(error.starts_with("integrity"), "{error}");
}

#[test]
fn the_plans_digest_and_refs_are_checked_before_indexing() {
    let (dir, top, head) = repo();
    let db = ObjectDb::open_git_dir(&top.join(".git")).unwrap();
    let refs = vec![(
        "refs/elitea/handoff/1".to_owned(),
        objects::oid(&head).unwrap(),
    )];
    let written = bundle::write_compact(&db, &refs, Limits::default()).unwrap();
    let quarantine = dir.path().join("q");
    bundle::init_quarantine(&quarantine).unwrap();
    let wrong = "0".repeat(64);
    let error = bundle::verify_into(
        &written.bytes,
        &quarantine,
        &Expect {
            sha256: Some(&wrong),
            ..Expect::default()
        },
        Limits::default(),
    )
    .unwrap_err();
    assert_eq!(error.code(), "integrity_failed");
    let other_refs = BTreeMap::from([("refs/elitea/cloud".to_owned(), head.clone())]);
    let error = bundle::verify_into(
        &written.bytes,
        &quarantine,
        &Expect {
            refs: Some(&other_refs),
            ..Expect::default()
        },
        Limits::default(),
    )
    .unwrap_err();
    assert_eq!(error.code(), "integrity_failed");
    // Nothing was indexed by either refusal.
    assert_eq!(
        std::fs::read_dir(quarantine.join("objects/pack"))
            .unwrap()
            .count(),
        0
    );
}

#[test]
fn sha256_repositories_and_bundles_are_refused_clearly() {
    let dir = tempfile::tempdir().unwrap();
    let top = dir.path().join("r");
    std::fs::create_dir(&top).unwrap();
    git(&top, &["init", "-q", "--object-format=sha256"]);
    let error = ObjectDb::open_git_dir(&top.join(".git")).unwrap_err();
    assert_eq!(error.code(), "unsupported_object_format");
    assert!(error.to_string().contains("sha256"), "{error}");
    let v3 = b"# v3 git bundle\n@object-format=sha256\n\nPACK".to_vec();
    assert_eq!(
        bundle::parse_header(&v3).unwrap_err().code(),
        "unsupported_object_format"
    );
    let v3_sha1 = b"# v3 git bundle\n@object-format=sha1\n\nPACK".to_vec();
    assert!(bundle::parse_header(&v3_sha1).is_ok());
    let filtered = b"# v3 git bundle\n@filter=blob:none\n\nPACK".to_vec();
    assert_eq!(
        bundle::parse_header(&filtered).unwrap_err().code(),
        "unsupported"
    );
}

#[test]
fn limits_bound_the_writer() {
    let (_dir, top, head) = repo();
    let db = ObjectDb::open_git_dir(&top.join(".git")).unwrap();
    let refs = vec![(
        "refs/elitea/handoff/1".to_owned(),
        objects::oid(&head).unwrap(),
    )];
    let tight = Limits {
        max_objects: 1000,
        max_unpacked_bytes: 4,
    };
    assert_eq!(
        bundle::write_compact(&db, &refs, tight).unwrap_err().code(),
        "invalid"
    );
}

#[test]
fn a_deltified_pack_is_wrapped_verified_and_read_by_git() {
    let (dir, top, _) = repo();
    // Many revisions of one text file: git deltifies them.
    let mut text = String::new();
    for line in 0..400 {
        text.push_str(&format!(
            "line {line} of a longer file that git will deltify\n"
        ));
    }
    let mut head = String::new();
    for revision in 0..8 {
        text.push_str(&format!("revision {revision}\n"));
        head = commit_file(&top, "src/long.txt", &text);
    }
    let db = ObjectDb::open_git_dir(&top.join(".git")).unwrap();
    let refs = vec![(
        "refs/elitea/handoff/1".to_owned(),
        objects::oid(&head).unwrap(),
    )];
    let packs = dir.path().join("packs");
    std::fs::create_dir(&packs).unwrap();
    let base = packs.join("seg");
    let name = std::process::Command::new("git")
        .current_dir(&top)
        .args(["pack-objects", "--revs", "--delta-base-offset", "-q"])
        .arg(&base)
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::piped())
        .spawn()
        .and_then(|mut child| {
            use std::io::Write as _;
            child
                .stdin
                .take()
                .unwrap()
                .write_all(format!("{head}\n").as_bytes())?;
            child.wait_with_output()
        })
        .unwrap();
    let hash = String::from_utf8_lossy(&name.stdout).trim().to_owned();
    let pack = std::fs::read(packs.join(format!("seg-{hash}.pack"))).unwrap();
    let written = bundle::write_compact_with_pack(&db, &refs, &pack, Limits::default()).unwrap();
    let whole = bundle::write_compact(&db, &refs, Limits::default()).unwrap();
    assert!(
        written.bytes.len() < whole.bytes.len(),
        "deltas make it smaller: {} vs {}",
        written.bytes.len(),
        whole.bytes.len()
    );
    let quarantine = dir.path().join("q");
    bundle::init_quarantine(&quarantine).unwrap();
    let verified = bundle::verify_into(
        &written.bytes,
        &quarantine,
        &Expect {
            sha256: Some(&written.sha256),
            ..Expect::default()
        },
        Limits::default(),
    )
    .unwrap();
    assert_eq!(verified.objects, written.objects);
    // A pack of other objects is refused before anything is wrapped.
    let other = bundle::write_compact(
        &db,
        &[("refs/elitea/base".to_owned(), objects::oid(&head).unwrap())],
        Limits::default(),
    )
    .unwrap();
    let short = &other.bytes[bundle::parse_header(&other.bytes).unwrap().pack_offset..];
    let first = git(&top, &["rev-list", "--max-parents=0", "HEAD"]);
    assert_eq!(
        bundle::write_compact_with_pack(
            &db,
            &[("refs/elitea/base".to_owned(), objects::oid(&first).unwrap())],
            short,
            Limits::default()
        )
        .unwrap_err()
        .code(),
        "invalid"
    );
    assert!(bundle::write_compact_with_pack(&db, &refs, b"not a pack", Limits::default()).is_err());
}
