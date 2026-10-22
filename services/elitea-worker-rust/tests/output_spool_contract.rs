#![cfg(any(target_os = "linux", target_os = "macos"))]

use std::fs;
use std::num::NonZeroU64;
use std::os::unix::fs::{PermissionsExt, symlink};
use std::path::{Path, PathBuf};
use std::process::Command;

use elitea_worker_rust::spool::{
    EncryptedOutputSpool, ExecutionSpoolBinding, ExecutionSpoolIdentity, MAX_RETIRED_FRAMES,
    RETIRED_RETENTION, RetiredFrameTag, SpoolError, SpoolLimits, SpoolMasterKey,
};
use tempfile::TempDir;

const FILE_OVERHEAD: u64 = 41;

fn sequence(value: u64) -> NonZeroU64 {
    NonZeroU64::new(value).expect("nonzero test sequence")
}

fn spool_binding(execution_id: &str) -> ExecutionSpoolBinding {
    ExecutionSpoolBinding::new(&ExecutionSpoolIdentity {
        tenant_id: "tenant-1".to_owned(),
        resource_project_id: "resource-1".to_owned(),
        projection_project_id: "projection-1".to_owned(),
        command_id: "command-1".to_owned(),
        execution_id: execution_id.to_owned(),
        generation: 2,
        producer_id: "producer-1".to_owned(),
    })
    .expect("valid spool binding")
}

fn limits() -> SpoolLimits {
    SpoolLimits {
        max_frames: 4,
        max_encrypted_bytes: 4096,
        max_frame_bytes: 1024,
    }
}

fn root() -> (TempDir, PathBuf) {
    let temporary_base = std::env::temp_dir()
        .canonicalize()
        .expect("canonical temporary root");
    let temporary = tempfile::tempdir_in(temporary_base).expect("temporary directory");
    let root = temporary.path().join("spool");
    fs::create_dir(&root).expect("spool root");
    fs::set_permissions(&root, fs::Permissions::from_mode(0o700)).expect("private spool root");
    (temporary, root)
}

fn master(byte: u8) -> SpoolMasterKey {
    SpoolMasterKey::new([byte; 32])
}

fn open(
    root: &Path,
    key: &SpoolMasterKey,
    binding: &ExecutionSpoolBinding,
    limits: SpoolLimits,
) -> EncryptedOutputSpool {
    open_outliving_forked_helpers(root, key, binding, limits).expect("open encrypted spool")
}

/// `EncryptedOutputSpool::open`, retrying ONLY a transient ownership refusal.
///
/// The spool's exclusive `flock` lives on the open file description, and a
/// CONCURRENT test in this binary that forks a helper process (`mkfifo`,
/// `python3`) can have that child inherit this test's spool directory
/// descriptor for the fork-to-exec window — every spool descriptor is CLOEXEC,
/// but CLOEXEC only closes at exec. A reopen inside that window then sees
/// "another live owner" although this test dropped its spool, and the
/// assertion under test (corruption, capacity, safety) never runs. Retrying
/// that one error keeps every other outcome exactly as production sees it;
/// `private_paths_and_single_live_owner_are_enforced` asserts the ownership
/// refusal itself and therefore calls `EncryptedOutputSpool::open` directly.
fn open_outliving_forked_helpers(
    root: &Path,
    key: &SpoolMasterKey,
    binding: &ExecutionSpoolBinding,
    limits: SpoolLimits,
) -> Result<EncryptedOutputSpool, SpoolError> {
    for _ in 0..200 {
        match EncryptedOutputSpool::open(root, key, binding, limits) {
            Err(SpoolError::OwnershipUnavailable(_)) => {
                std::thread::sleep(std::time::Duration::from_millis(5));
            }
            outcome => return outcome,
        }
    }
    EncryptedOutputSpool::open(root, key, binding, limits)
}

fn child(root: &Path, binding: &ExecutionSpoolBinding) -> PathBuf {
    root.join(binding.directory_name())
}

fn tag(claim_attempt: u64, lease_epoch: u64) -> RetiredFrameTag {
    RetiredFrameTag {
        claim_attempt,
        lease_epoch,
    }
}

fn retired_path(
    root: &Path,
    binding: &ExecutionSpoolBinding,
    sequence: u64,
    tag: RetiredFrameTag,
) -> PathBuf {
    child(root, binding).join(format!(
        "{sequence:020}-{:020}-{:020}.retired",
        tag.claim_attempt, tag.lease_epoch
    ))
}

fn retired_files(root: &Path, binding: &ExecutionSpoolBinding) -> Vec<String> {
    let mut names = fs::read_dir(child(root, binding))
        .unwrap()
        .map(|entry| entry.unwrap().file_name().into_string().unwrap())
        .filter(|name| name.ends_with(".retired"))
        .collect::<Vec<_>>();
    names.sort();
    names
}

fn frame(root: &Path, binding: &ExecutionSpoolBinding, sequence: u64) -> PathBuf {
    child(root, binding).join(format!("{sequence:020}.frame"))
}

#[test]
fn encrypted_put_replay_ack_and_empty_cleanup_are_durable() {
    let (_temporary, root) = root();
    let binding = spool_binding("execution-lifecycle");
    let key = master(b'k');
    let mut spool = open(&root, &key, &binding, limits());

    spool.put(sequence(1), b"sensitive-output").unwrap();
    spool.put(sequence(1), b"sensitive-output").unwrap();
    spool.put(sequence(2), b"second-output").unwrap();

    let raw = fs::read(frame(&root, &binding, 1)).unwrap();
    assert!(!raw.windows(16).any(|window| window == b"sensitive-output"));
    assert_eq!(
        fs::metadata(frame(&root, &binding, 1))
            .unwrap()
            .permissions()
            .mode()
            & 0o777,
        0o600
    );
    let pending = spool.pending().unwrap();
    assert_eq!(pending.len(), 2);
    assert_eq!(pending[0].sequence, sequence(1));
    assert_eq!(pending[0].payload, b"sensitive-output");
    assert_eq!(pending[1].sequence, sequence(2));

    spool.acknowledge_through(sequence(1)).unwrap();
    assert_eq!(spool.pending().unwrap().len(), 1);
    spool.acknowledge_through(sequence(2)).unwrap();
    assert!(spool.pending().unwrap().is_empty());
    assert!(spool.remove_if_empty().unwrap());
    assert!(fs::read_dir(&root).unwrap().next().is_none());
}

#[test]
fn immutable_sequences_and_exact_replacement_preserve_the_winner() {
    let (_temporary, root) = root();
    let binding = spool_binding("execution-replacement");
    let key = master(b'k');
    let mut spool = open(&root, &key, &binding, limits());
    spool.put(sequence(7), b"success-frame").unwrap();

    assert!(matches!(
        spool.put(sequence(7), b"different-frame"),
        Err(SpoolError::InvalidInput(message)) if message.contains("cannot change")
    ));
    assert!(matches!(
        spool.replace_exact(sequence(7), b"wrong-frame", b"cancelled-frame"),
        Err(SpoolError::InvalidInput(message)) if message.contains("changed")
    ));
    assert_eq!(spool.pending().unwrap()[0].payload, b"success-frame");

    spool
        .replace_exact(sequence(7), b"success-frame", b"cancelled-frame")
        .unwrap();
    assert_eq!(spool.pending().unwrap()[0].payload, b"cancelled-frame");
    let raw = fs::read(frame(&root, &binding, 7)).unwrap();
    assert!(!raw.windows(15).any(|window| window == b"cancelled-frame"));
}

#[test]
fn frame_count_plaintext_and_ciphertext_capacity_are_exact() {
    let (_temporary, root) = root();
    let binding = spool_binding("execution-capacity");
    let key = master(b'k');
    let mut spool = open(
        &root,
        &key,
        &binding,
        SpoolLimits {
            max_frames: 1,
            max_encrypted_bytes: FILE_OVERHEAD + 5,
            max_frame_bytes: 5,
        },
    );
    spool.put(sequence(1), b"12345").unwrap();
    assert!(matches!(
        spool.put(sequence(2), b"1"),
        Err(SpoolError::ResourceExhausted(message)) if message.contains("full")
    ));
    assert!(matches!(
        spool.replace_exact(sequence(1), b"12345", b"123456"),
        Err(SpoolError::ResourceExhausted(message)) if message.contains("replacement")
    ));
    assert_eq!(spool.pending().unwrap()[0].payload, b"12345");

    let growth_binding = spool_binding("execution-capacity-growth");
    let mut growth = open(
        &root,
        &key,
        &growth_binding,
        SpoolLimits {
            max_frames: 2,
            max_encrypted_bytes: 2 * FILE_OVERHEAD + 10,
            max_frame_bytes: 10,
        },
    );
    growth.put(sequence(1), b"12345").unwrap();
    growth.put(sequence(2), b"67890").unwrap();
    assert!(matches!(
        growth.replace_exact(sequence(1), b"12345", b"123456"),
        Err(SpoolError::ResourceExhausted(message)) if message.contains("full")
    ));
    assert_eq!(growth.pending().unwrap()[0].payload, b"12345");
}

#[test]
fn maximum_u64_sequence_round_trips_with_canonical_filename() {
    let (_temporary, root) = root();
    let binding = spool_binding("execution-u64");
    let key = master(b'k');
    let mut spool = open(&root, &key, &binding, limits());

    spool.put(sequence(u64::MAX), b"last-sequence").unwrap();

    assert!(frame(&root, &binding, u64::MAX).is_file());
    assert_eq!(spool.pending().unwrap()[0].sequence.get(), u64::MAX);
}

#[test]
fn startup_cleans_only_safe_recognized_temporary_files() {
    let (_temporary, root) = root();
    let binding = spool_binding("execution-temp-cleanup");
    let child = child(&root, &binding);
    fs::create_dir(&child).unwrap();
    fs::set_permissions(&child, fs::Permissions::from_mode(0o700)).unwrap();
    let residue = child.join(format!(".tmp-{}", "a".repeat(32)));
    fs::write(&residue, b"partial-ciphertext").unwrap();
    fs::set_permissions(&residue, fs::Permissions::from_mode(0o600)).unwrap();

    let key = master(b'k');
    let mut spool = open(&root, &key, &binding, limits());
    assert!(!residue.exists());
    assert!(spool.pending().unwrap().is_empty());
}

#[test]
fn reopen_enforces_lower_frame_byte_and_temp_residue_limits() {
    let (_temporary, root) = root();
    let key = master(b'k');

    for execution_id in ["execution-lower-frames", "execution-lower-bytes"] {
        let binding = spool_binding(execution_id);
        let mut spool = open(&root, &key, &binding, limits());
        spool.put(sequence(1), b"12345").unwrap();
        spool.put(sequence(2), b"67890").unwrap();
    }
    assert!(matches!(
        open_outliving_forked_helpers(
            &root,
            &key,
            &spool_binding("execution-lower-frames"),
            SpoolLimits {
                max_frames: 1,
                max_encrypted_bytes: 4096,
                max_frame_bytes: 1024,
            },
        ),
        Err(SpoolError::ResourceExhausted(message)) if message.contains("full")
    ));
    assert!(matches!(
        open_outliving_forked_helpers(
            &root,
            &key,
            &spool_binding("execution-lower-bytes"),
            SpoolLimits {
                max_frames: 4,
                max_encrypted_bytes: 90,
                max_frame_bytes: 10,
            },
        ),
        Err(SpoolError::ResourceExhausted(message)) if message.contains("full")
    ));

    let temp_binding = spool_binding("execution-excess-temp");
    let temp_child = child(&root, &temp_binding);
    fs::create_dir(&temp_child).unwrap();
    fs::set_permissions(&temp_child, fs::Permissions::from_mode(0o700)).unwrap();
    for digit in ['a', 'b'] {
        let path = temp_child.join(format!(".tmp-{}", digit.to_string().repeat(32)));
        fs::write(&path, b"residue").unwrap();
        fs::set_permissions(&path, fs::Permissions::from_mode(0o600)).unwrap();
    }
    assert!(matches!(
        open_outliving_forked_helpers(&root, &key, &temp_binding, limits()),
        Err(SpoolError::ResourceExhausted(message)) if message.contains("temporary residue")
    ));
}

#[test]
fn unsafe_or_unexpected_entries_fail_closed() {
    let (_temporary, root) = root();
    let key = master(b'k');

    let unexpected_binding = spool_binding("execution-unexpected");
    let mut unexpected = open(&root, &key, &unexpected_binding, limits());
    fs::write(child(&root, &unexpected_binding).join("unexpected"), b"x").unwrap();
    assert!(matches!(
        unexpected.pending(),
        Err(SpoolError::InvalidInput(message)) if message.contains("unexpected entry")
    ));

    let symlink_binding = spool_binding("execution-symlink");
    let target = root.join("target");
    fs::write(&target, b"x").unwrap();
    let symlink_child = child(&root, &symlink_binding);
    fs::create_dir(&symlink_child).unwrap();
    fs::set_permissions(&symlink_child, fs::Permissions::from_mode(0o700)).unwrap();
    symlink(
        &target,
        symlink_child.join(format!(".tmp-{}", "b".repeat(32))),
    )
    .unwrap();
    assert!(matches!(
        open_outliving_forked_helpers(&root, &key, &symlink_binding, limits()),
        Err(SpoolError::InvalidInput(message)) if message.contains("unsafe temporary")
    ));
    assert!(target.exists());
}

#[test]
fn fifo_frame_names_never_block_put_or_replacement() {
    let (_temporary, root) = root();
    let binding = spool_binding("execution-fifo");
    let key = master(b'k');
    let mut spool = open(&root, &key, &binding, limits());
    let fifo = frame(&root, &binding, 1);
    assert!(
        Command::new("mkfifo")
            .arg(&fifo)
            .status()
            .unwrap()
            .success()
    );
    fs::set_permissions(&fifo, fs::Permissions::from_mode(0o600)).unwrap();

    assert!(matches!(
        spool.put(sequence(1), b"frame"),
        Err(SpoolError::InvalidInput(message)) if message.contains("unsafe frame")
    ));
    assert!(matches!(
        spool.replace_exact(sequence(1), b"frame", b"replacement"),
        Err(SpoolError::InvalidInput(message)) if message.contains("unsafe frame")
    ));
}

#[test]
fn truncated_oversized_and_accessible_frames_fail_before_replay() {
    let (_temporary, root) = root();
    let key = master(b'k');

    let truncated_binding = spool_binding("execution-truncated");
    let mut truncated = open(&root, &key, &truncated_binding, limits());
    truncated.put(sequence(1), b"frame").unwrap();
    drop(truncated);
    fs::write(frame(&root, &truncated_binding, 1), b"ELITEASPOOL1").unwrap();
    fs::set_permissions(
        frame(&root, &truncated_binding, 1),
        fs::Permissions::from_mode(0o600),
    )
    .unwrap();
    assert!(matches!(
        open_outliving_forked_helpers(&root, &key, &truncated_binding, limits())
            .and_then(|mut spool| spool.pending()),
        Err(SpoolError::InvalidInput(message)) if message.contains("corrupt")
    ));

    let oversized_binding = spool_binding("execution-oversized");
    let small_limits = SpoolLimits {
        max_frames: 1,
        max_encrypted_bytes: FILE_OVERHEAD + 1,
        max_frame_bytes: 1,
    };
    let oversized = open(&root, &key, &oversized_binding, small_limits);
    drop(oversized);
    fs::write(
        frame(&root, &oversized_binding, 1),
        vec![0_u8; usize::try_from(FILE_OVERHEAD + 2).unwrap()],
    )
    .unwrap();
    fs::set_permissions(
        frame(&root, &oversized_binding, 1),
        fs::Permissions::from_mode(0o600),
    )
    .unwrap();
    assert!(matches!(
        open_outliving_forked_helpers(&root, &key, &oversized_binding, small_limits)
            .and_then(|mut spool| spool.pending()),
        Err(SpoolError::ResourceExhausted(message)) if message.contains("exceeds")
    ));

    let accessible_binding = spool_binding("execution-accessible-frame");
    let mut accessible = open(&root, &key, &accessible_binding, limits());
    accessible.put(sequence(1), b"frame").unwrap();
    fs::set_permissions(
        frame(&root, &accessible_binding, 1),
        fs::Permissions::from_mode(0o640),
    )
    .unwrap();
    assert!(matches!(
        accessible.pending(),
        Err(SpoolError::InvalidInput(message)) if message.contains("unsafe frame")
    ));
}

#[test]
fn ciphertext_tampering_wrong_key_and_wrong_binding_fail_closed() {
    let (_temporary, root) = root();
    let binding = spool_binding("execution-corruption");
    let key = master(b'k');
    let mut spool = open(&root, &key, &binding, limits());
    spool.put(sequence(1), b"authenticated-frame").unwrap();
    drop(spool);

    assert!(matches!(
        open_outliving_forked_helpers(&root, &master(b'x'), &binding, limits())
            .and_then(|mut spool| spool.pending()),
        Err(SpoolError::InvalidInput(message)) if message.contains("corrupt")
    ));

    let other_binding = spool_binding("execution-other-binding");
    let other_key = master(b'k');
    let other_spool = open(&root, &other_key, &other_binding, limits());
    fs::write(
        frame(&root, &other_binding, 1),
        fs::read(frame(&root, &binding, 1)).unwrap(),
    )
    .unwrap();
    fs::set_permissions(
        frame(&root, &other_binding, 1),
        fs::Permissions::from_mode(0o600),
    )
    .unwrap();
    drop(other_spool);
    assert!(matches!(
        open_outliving_forked_helpers(&root, &other_key, &other_binding, limits())
            .and_then(|mut spool| spool.pending()),
        Err(SpoolError::InvalidInput(message)) if message.contains("corrupt")
    ));

    let path = frame(&root, &binding, 1);
    let mut body = fs::read(&path).unwrap();
    let final_byte = body.last_mut().unwrap();
    *final_byte ^= 1;
    fs::write(&path, body).unwrap();
    fs::set_permissions(&path, fs::Permissions::from_mode(0o600)).unwrap();
    assert!(matches!(
        open_outliving_forked_helpers(&root, &key, &binding, limits())
            .and_then(|mut spool| spool.pending()),
        Err(SpoolError::InvalidInput(message)) if message.contains("corrupt")
    ));
}

#[test]
fn private_paths_and_single_live_owner_are_enforced() {
    let (_temporary, spool_root) = root();
    let owner_binding = spool_binding("execution-owner");
    let key = master(b'k');
    let spool = open(&spool_root, &key, &owner_binding, limits());

    assert_eq!(
        fs::metadata(&spool_root).unwrap().permissions().mode() & 0o777,
        0o700
    );
    assert_eq!(
        fs::metadata(child(&spool_root, &owner_binding))
            .unwrap()
            .permissions()
            .mode()
            & 0o777,
        0o700
    );
    assert!(matches!(
        EncryptedOutputSpool::open(&spool_root, &key, &owner_binding, limits()),
        Err(SpoolError::OwnershipUnavailable(message)) if message.contains("live owner")
    ));
    let process_status = Command::new("python3")
        .args([
            "-c",
            concat!(
                "import fcntl,os,sys; ",
                "fd=os.open(sys.argv[1],os.O_RDONLY); ",
                "\ntry: fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB)\n",
                "except BlockingIOError: sys.exit(73)\n",
                "sys.exit(0)"
            ),
        ])
        .arg(child(&spool_root, &owner_binding))
        .status()
        .expect("Python lock probe");
    assert_eq!(process_status.code(), Some(73));

    drop(spool);
    let _reopened = open(&spool_root, &key, &owner_binding, limits());

    let (_unsafe_temporary, unsafe_root) = root();
    fs::set_permissions(&unsafe_root, fs::Permissions::from_mode(0o750)).unwrap();
    assert!(matches!(
        EncryptedOutputSpool::open(
            &unsafe_root,
            &key,
            &spool_binding("unsafe-root"),
            limits()
        ),
        Err(SpoolError::InvalidInput(message)) if message.contains("root is unsafe")
    ));
}

#[test]
fn repeated_plaintext_uses_distinct_ciphertext_and_sequence_aad() {
    let (_temporary, root) = root();
    let binding = spool_binding("execution-nonce-aad");
    let key = master(b'k');
    let mut spool = open(&root, &key, &binding, limits());
    spool.put(sequence(1), b"same-plaintext").unwrap();
    spool.put(sequence(2), b"same-plaintext").unwrap();
    let first = fs::read(frame(&root, &binding, 1)).unwrap();
    let second = fs::read(frame(&root, &binding, 2)).unwrap();
    assert_ne!(first, second);
    assert_ne!(&first[13..25], &second[13..25]);

    fs::write(frame(&root, &binding, 2), first).unwrap();
    fs::set_permissions(frame(&root, &binding, 2), fs::Permissions::from_mode(0o600)).unwrap();
    assert!(matches!(
        spool.pending(),
        Err(SpoolError::InvalidInput(message)) if message.contains("corrupt")
    ));
}

#[test]
fn cleanup_never_removes_a_replaced_child_directory() {
    let (_temporary, root) = root();
    let binding = spool_binding("execution-cleanup-race");
    let key = master(b'k');
    let spool = open(&root, &key, &binding, limits());
    let original = child(&root, &binding);
    let moved = root.join("moved-child");
    fs::rename(&original, &moved).unwrap();
    fs::create_dir(&original).unwrap();
    fs::set_permissions(&original, fs::Permissions::from_mode(0o700)).unwrap();

    assert!(matches!(
        spool.remove_if_empty(),
        Err(SpoolError::InvalidInput(message)) if message.contains("identity changed")
    ));
    assert!(original.is_dir());
    assert!(moved.is_dir());
}

#[test]
fn binding_and_limit_validation_reject_ambiguous_inputs() {
    let mut identity = ExecutionSpoolIdentity {
        tenant_id: "tenant-1".to_owned(),
        resource_project_id: "resource-1".to_owned(),
        projection_project_id: "projection-1".to_owned(),
        command_id: "command-1".to_owned(),
        execution_id: "execution-1".to_owned(),
        generation: 1,
        producer_id: "producer-1".to_owned(),
    };
    identity.command_id = "command\n1".to_owned();
    assert!(ExecutionSpoolBinding::new(&identity).is_ok());
    identity.command_id = "command\t1".to_owned();
    assert!(ExecutionSpoolBinding::new(&identity).is_ok());
    identity.command_id.clear();
    assert!(matches!(
        ExecutionSpoolBinding::new(&identity),
        Err(SpoolError::InvalidInput(message)) if message.contains("identity")
    ));

    let (_temporary, root) = root();
    assert!(matches!(
        open_outliving_forked_helpers(
            &root,
            &master(b'k'),
            &spool_binding("invalid-limits"),
            SpoolLimits {
                max_frames: 1,
                max_encrypted_bytes: 40,
                max_frame_bytes: 1,
            },
        ),
        Err(SpoolError::InvalidInput(message)) if message.contains("must fit")
    ));
}

#[test]
fn a_retired_frame_leaves_replay_but_stays_as_an_encrypted_record() {
    let (_temporary, root) = root();
    let binding = spool_binding("execution-retired");
    let key = master(b'k');
    let mut spool = open(&root, &key, &binding, limits());
    spool.put(sequence(8), b"superseded-progress").unwrap();
    // Only the exact bytes can be retired.
    assert!(
        spool
            .retire_pending(sequence(8), b"other", tag(1, 1))
            .is_err()
    );
    assert!(
        spool
            .retire_pending(sequence(9), b"superseded-progress", tag(1, 1))
            .is_err()
    );
    spool
        .retire_pending(sequence(8), b"superseded-progress", tag(1, 1))
        .unwrap();
    assert!(spool.pending().unwrap().is_empty());
    assert!(spool.has_retired_frames().unwrap());
    // The record is encrypted and the sequence is free for the new claim.
    let record = retired_path(&root, &binding, 8, tag(1, 1));
    let raw = fs::read(&record).unwrap();
    assert!(!raw.windows(10).any(|window| window == b"superseded"));
    spool.put(sequence(8), b"fresh-progress").unwrap();
    assert_eq!(spool.pending().unwrap().len(), 1);
    spool.acknowledge_through(sequence(8)).unwrap();
    // The record outlives the output: the directory is not removed under it.
    assert!(!spool.remove_if_empty().unwrap());
    assert!(record.exists());
    // A reopened spool still enumerates cleanly and keeps the record.
    let mut reopened = open(&root, &key, &binding, limits());
    assert!(reopened.pending().unwrap().is_empty());
    assert!(reopened.has_retired_frames().unwrap());
}

/// CR-WK-P09-hyd (review M3): the retired-record cap must never wedge a spool.
/// Cap records are kept, cap+1 prunes exactly the oldest and logs nothing else,
/// and enumeration, replay, ACK and reopen keep working at every count.
#[test]
fn retired_records_at_the_cap_and_beyond_never_wedge_the_spool() {
    let (_temporary, root) = root();
    let binding = spool_binding("execution-retired-cap");
    let key = master(b'k');
    let mut spool = open(&root, &key, &binding, limits());
    let cap = u64::try_from(MAX_RETIRED_FRAMES).unwrap();
    for attempt in 1..=cap {
        spool.put(sequence(5), b"progress").unwrap();
        spool
            .retire_pending(sequence(5), b"progress", tag(attempt, 1))
            .unwrap();
        // Every operation that enumerates the directory still works.
        assert!(spool.pending().unwrap().is_empty());
    }
    assert_eq!(retired_files(&root, &binding).len(), MAX_RETIRED_FRAMES);
    // Cap + 1: the oldest record is pruned, the new one is kept, nothing fails.
    spool.put(sequence(5), b"progress").unwrap();
    spool
        .retire_pending(sequence(5), b"progress", tag(cap + 1, 1))
        .unwrap();
    let names = retired_files(&root, &binding);
    assert_eq!(names.len(), MAX_RETIRED_FRAMES);
    assert!(
        !retired_path(&root, &binding, 5, tag(1, 1)).exists(),
        "oldest pruned"
    );
    assert!(retired_path(&root, &binding, 5, tag(2, 1)).exists());
    assert!(retired_path(&root, &binding, 5, tag(cap + 1, 1)).exists());
    // The live path is unaffected: put, replay, acknowledge, reopen.
    spool.put(sequence(6), b"live").unwrap();
    assert_eq!(spool.pending().unwrap().len(), 1);
    spool.acknowledge_through(sequence(6)).unwrap();
    assert!(spool.has_retired_frames().unwrap());
    drop(spool);
    let mut reopened = open(&root, &key, &binding, limits());
    assert!(reopened.pending().unwrap().is_empty());
    assert_eq!(retired_files(&root, &binding).len(), MAX_RETIRED_FRAMES);
}

/// Many more retirements than the cap keep exactly the newest records.
#[test]
fn many_more_retirements_than_the_cap_keep_only_the_newest_records() {
    let (_temporary, root) = root();
    let binding = spool_binding("execution-retired-many");
    let mut spool = open(&root, &master(b'k'), &binding, limits());
    let total = u64::try_from(MAX_RETIRED_FRAMES).unwrap() * 3;
    for attempt in 1..=total {
        spool.put(sequence(1), b"p").unwrap();
        spool
            .retire_pending(sequence(1), b"p", tag(attempt, attempt))
            .unwrap();
    }
    let names = retired_files(&root, &binding);
    assert_eq!(names.len(), MAX_RETIRED_FRAMES);
    for attempt in (total - u64::try_from(MAX_RETIRED_FRAMES).unwrap() + 1)..=total {
        assert!(retired_path(&root, &binding, 1, tag(attempt, attempt)).exists());
    }
}

/// A sequence number reused by a later claim keeps the earlier record: the
/// name carries the claim attempt and epoch, and a record is never replaced.
#[test]
fn a_sequence_reused_by_a_later_claim_keeps_every_earlier_record() {
    let (_temporary, root) = root();
    let binding = spool_binding("execution-retired-reuse");
    let mut spool = open(&root, &master(b'k'), &binding, limits());
    spool.put(sequence(3), b"claim-one-progress").unwrap();
    spool
        .retire_pending(sequence(3), b"claim-one-progress", tag(1, 1))
        .unwrap();
    let first = fs::read(retired_path(&root, &binding, 3, tag(1, 1))).unwrap();
    spool.put(sequence(3), b"claim-two-progress").unwrap();
    spool
        .retire_pending(sequence(3), b"claim-two-progress", tag(2, 1))
        .unwrap();
    assert_eq!(retired_files(&root, &binding).len(), 2);
    assert_eq!(
        fs::read(retired_path(&root, &binding, 3, tag(1, 1))).unwrap(),
        first
    );
    // The same claim cannot overwrite its own record with different bytes.
    spool.put(sequence(3), b"claim-two-other").unwrap();
    assert!(
        spool
            .retire_pending(sequence(3), b"claim-two-other", tag(2, 1))
            .is_err()
    );
    assert_eq!(retired_files(&root, &binding).len(), 2);
    // The refused frame stays live; nothing was lost or replaced.
    assert_eq!(spool.pending().unwrap().len(), 1);
}

/// A crash after the link and before the unlink leaves one inode under two
/// names; the retry finishes the retirement instead of failing or duplicating.
#[test]
fn a_retirement_interrupted_between_link_and_unlink_completes_on_retry() {
    let (_temporary, root) = root();
    let binding = spool_binding("execution-retired-resume");
    let mut spool = open(&root, &master(b'k'), &binding, limits());
    spool.put(sequence(4), b"progress").unwrap();
    fs::hard_link(
        frame(&root, &binding, 4),
        retired_path(&root, &binding, 4, tag(7, 2)),
    )
    .unwrap();
    spool
        .retire_pending(sequence(4), b"progress", tag(7, 2))
        .unwrap();
    assert!(!frame(&root, &binding, 4).exists());
    assert_eq!(retired_files(&root, &binding).len(), 1);
    assert!(spool.pending().unwrap().is_empty());
}

/// The execution settles: its retired records are deleted and the directory
/// can be removed. Records of an execution that never settles through this
/// spool are swept once they pass the retention window.
#[test]
fn settlement_and_the_retention_sweep_delete_retired_records() {
    let (_temporary, root) = root();
    let binding = spool_binding("execution-retired-settle");
    let key = master(b'k');
    let mut spool = open(&root, &key, &binding, limits());
    for attempt in 1..=3 {
        spool.put(sequence(2), b"progress").unwrap();
        spool
            .retire_pending(sequence(2), b"progress", tag(attempt, 1))
            .unwrap();
    }
    // Inside the retention window nothing is swept.
    assert_eq!(
        spool
            .sweep_expired_retired(RETIRED_RETENTION, std::time::SystemTime::now())
            .unwrap(),
        0
    );
    assert_eq!(retired_files(&root, &binding).len(), 3);
    // Past the window every record goes; a fresh one stays inside its own window.
    let later =
        std::time::SystemTime::now() + RETIRED_RETENTION + std::time::Duration::from_mins(1);
    assert_eq!(
        spool
            .sweep_expired_retired(RETIRED_RETENTION, later)
            .unwrap(),
        3
    );
    assert_eq!(retired_files(&root, &binding), [] as [String; 0]);
    for attempt in 1..=2 {
        spool.put(sequence(2), b"progress").unwrap();
        spool
            .retire_pending(sequence(2), b"progress", tag(attempt, 1))
            .unwrap();
    }
    // Settlement discards what remains and the directory is then removable.
    assert_eq!(spool.discard_retired_records().unwrap(), 2);
    assert!(!spool.has_retired_frames().unwrap());
    assert!(spool.remove_if_empty().unwrap());
    assert!(!child(&root, &binding).exists());
}

/// A record left by an earlier build, `{sequence}.retired`, still enumerates,
/// counts as the oldest, and is removed like any other.
#[test]
fn a_legacy_retired_record_still_enumerates_and_is_pruned_first() {
    let (_temporary, root) = root();
    let binding = spool_binding("execution-retired-legacy");
    let key = master(b'k');
    let mut spool = open(&root, &key, &binding, limits());
    spool.put(sequence(9), b"old-build").unwrap();
    fs::rename(
        frame(&root, &binding, 9),
        child(&root, &binding).join(format!("{:020}.retired", 9)),
    )
    .unwrap();
    assert!(spool.pending().unwrap().is_empty());
    assert!(spool.has_retired_frames().unwrap());
    for attempt in 1..=u64::try_from(MAX_RETIRED_FRAMES).unwrap() {
        spool.put(sequence(1), b"p").unwrap();
        spool
            .retire_pending(sequence(1), b"p", tag(attempt, 1))
            .unwrap();
    }
    assert!(
        !child(&root, &binding)
            .join(format!("{:020}.retired", 9))
            .exists()
    );
    assert_eq!(retired_files(&root, &binding).len(), MAX_RETIRED_FRAMES);
}
