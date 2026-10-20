//! The flows over a faulty transport: dropped upload, lost publish ack,
//! 5xx, stale plan, corrupt GET, multipart, and the store's own rules.
#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::sync::Arc;

use elitea_workspace_store::digest::sha256_hex;
use elitea_workspace_store::fault::{Fault, FaultTransport, Op};
use elitea_workspace_store::memory::MemoryStore;
use elitea_workspace_store::refs::Writer;
use elitea_workspace_store::sync::{self, Backoff};
use elitea_workspace_store::transport::{PublishRequest, RefUpdate, StoreTransport};

const A: &str = "1111111111111111111111111111111111111111";
const B: &str = "2222222222222222222222222222222222222222";

fn request(bytes: &[u8], epoch: u64, refs: Vec<RefUpdate>) -> PublishRequest {
    PublishRequest {
        epoch,
        kind: "COMPACT".into(),
        segment_sha256: sha256_hex(bytes),
        bytes: bytes.len() as u64,
        prerequisites: Vec::new(),
        refs,
        object_format: "sha1".into(),
    }
}

fn base(new: &str) -> Vec<RefUpdate> {
    vec![RefUpdate {
        name: "refs/elitea/base".into(),
        old: None,
        new: new.into(),
    }]
}

fn setup() -> (Arc<MemoryStore>, FaultTransport) {
    let store = Arc::new(MemoryStore::new(1, Writer::Desktop));
    let faulty = FaultTransport::new(store.clone());
    (store, faulty)
}

const FAST: Backoff = Backoff::immediate(5);

#[tokio::test]
async fn a_clean_upload_and_publish_lands_once() {
    let (store, faulty) = setup();
    let bytes = b"segment one".to_vec();
    let published = sync::upload_and_publish(&faulty, &bytes, &request(&bytes, 1, base(A)), FAST)
        .await
        .unwrap();
    assert_eq!(published.seq, 1);
    assert_eq!(store.refs()["refs/elitea/base"], A);
    // Uploading again finds it stored and sends nothing.
    sync::upload(&faulty, 1, &bytes, FAST).await.unwrap();
    assert_eq!(faulty.calls(Op::Put), 1);
}

#[tokio::test]
async fn a_dropped_upload_is_sent_again() {
    let (store, faulty) = setup();
    faulty.arm(Op::Put, Fault::Dropped, 1);
    let bytes = b"dropped once".to_vec();
    let published = sync::upload_and_publish(&faulty, &bytes, &request(&bytes, 1, base(A)), FAST)
        .await
        .unwrap();
    assert_eq!(published.seq, 1);
    assert_eq!(
        faulty.calls(Op::Put),
        2,
        "the publish found it missing and it was re-sent"
    );
    assert_eq!(store.segment_count(), 1);
}

#[tokio::test]
async fn a_lost_publish_ack_is_not_a_conflict_and_lands_once() {
    let (store, faulty) = setup();
    faulty.arm(Op::Publish, Fault::LostAck, 2);
    let bytes = b"ack lost".to_vec();
    let published = sync::upload_and_publish(&faulty, &bytes, &request(&bytes, 1, base(A)), FAST)
        .await
        .unwrap();
    assert_eq!(published.seq, 1);
    assert_eq!(faulty.calls(Op::Publish), 3);
    assert_eq!(
        store.segment_count(),
        1,
        "the retries were replays, not new segments"
    );
}

/// A store that forgot the request it applied (no replay match): the
/// conflict naming our own refs is still success.
#[tokio::test]
async fn a_conflict_that_names_our_own_refs_after_a_retry_is_success() {
    let (store, faulty) = setup();
    // What the store applied: the same refs, recorded under another
    // segment, so it does not see our retry as a replay.
    let applied = b"applied".to_vec();
    sync::upload(&faulty, 1, &applied, FAST).await.unwrap();
    store.publish(&request(&applied, 1, base(A))).await.unwrap();
    let ours = b"ours".to_vec();
    sync::upload(&faulty, 1, &ours, FAST).await.unwrap();
    // A first attempt without an answer, then the retry meets the ref
    // already where we moved it.
    faulty.arm(Op::Publish, Fault::Unavailable, 1);
    let published = sync::publish(&faulty, &request(&ours, 1, base(A)), FAST)
        .await
        .unwrap();
    assert_eq!(published.refs["refs/elitea/base"], A);
    assert_eq!(published.seq, 1);
    // Without a retry before it, the same conflict is a conflict.
    let error = sync::publish(&faulty, &request(&ours, 1, base(A)), FAST)
        .await
        .unwrap_err();
    assert_eq!(error.code(), "ref_conflict");
}

#[tokio::test]
async fn someone_elses_conflict_is_refused() {
    let (store, faulty) = setup();
    let bytes = b"theirs".to_vec();
    sync::upload_and_publish(&faulty, &bytes, &request(&bytes, 1, base(A)), FAST)
        .await
        .unwrap();
    let mine = b"mine".to_vec();
    let error = sync::upload_and_publish(&faulty, &mine, &request(&mine, 1, base(B)), FAST)
        .await
        .unwrap_err();
    assert_eq!(error.code(), "ref_conflict");
    assert_eq!(store.refs()["refs/elitea/base"], A);
}

#[tokio::test]
async fn five_hundreds_are_retried_and_then_given_up() {
    let (_store, faulty) = setup();
    faulty.arm(Op::Grant, Fault::Unavailable, 2);
    faulty.arm(Op::Put, Fault::Unavailable, 1);
    let bytes = b"flaky".to_vec();
    sync::upload_and_publish(&faulty, &bytes, &request(&bytes, 1, base(A)), FAST)
        .await
        .unwrap();
    let (_store, faulty) = setup();
    faulty.arm(Op::Grant, Fault::Unavailable, 10);
    let error = sync::upload(&faulty, 1, &bytes, Backoff::immediate(3))
        .await
        .unwrap_err();
    assert_eq!(error.code(), "unavailable");
    assert_eq!(faulty.calls(Op::Grant), 3);
}

#[tokio::test]
async fn an_expired_put_url_gets_a_fresh_grant() {
    let (store, faulty) = setup();
    faulty.arm(Op::Put, Fault::Expired, 1);
    let bytes = b"expired".to_vec();
    sync::upload(&faulty, 1, &bytes, FAST).await.unwrap();
    assert_eq!(faulty.calls(Op::Grant), 2);
    assert!(store.object(&sha256_hex(&bytes)).is_some());
}

#[tokio::test]
async fn the_epoch_fence_and_the_allowlist_hold() {
    let (store, faulty) = setup();
    let bytes = b"stale".to_vec();
    sync::upload(&faulty, 1, &bytes, FAST).await.unwrap();
    store.transition(2, Writer::Desktop);
    let error = sync::publish(&faulty, &request(&bytes, 1, base(A)), FAST)
        .await
        .unwrap_err();
    assert_eq!(error.code(), "stale_epoch");
    let error = sync::upload(&faulty, 1, b"later", FAST).await.unwrap_err();
    assert_eq!(error.code(), "stale_epoch");
    let cloud = vec![RefUpdate {
        name: "refs/elitea/cloud".into(),
        old: None,
        new: A.into(),
    }];
    let error = sync::publish(&faulty, &request(&bytes, 2, cloud), FAST)
        .await
        .unwrap_err();
    assert_eq!(error.code(), "ref_not_allowed");
    assert_eq!(store.segment_count(), 0);
}

#[tokio::test]
async fn a_stale_plan_is_fetched_again_and_a_corrupt_get_never_passes() {
    let (store, faulty) = setup();
    let bytes = b"fetch me".to_vec();
    sync::upload_and_publish(&faulty, &bytes, &request(&bytes, 1, base(A)), FAST)
        .await
        .unwrap();
    // The URL handed out expired before the GET.
    faulty.arm(Op::Get, Fault::Expired, 1);
    let (plan, segment, got) = sync::fetch_latest(&faulty, 0, FAST).await.unwrap();
    assert_eq!(got, bytes);
    assert_eq!(plan.head_seq, 1);
    assert_eq!(segment.refs["refs/elitea/base"], A);
    assert_eq!(faulty.calls(Op::Plan), 2);
    // A corrupt answer once: refetched and fine.
    faulty.arm(Op::Get, Fault::Corrupt, 1);
    assert_eq!(sync::fetch_latest(&faulty, 0, FAST).await.unwrap().2, bytes);
    // Corrupt every time: an integrity failure, never the bytes.
    faulty.arm(Op::Get, Fault::Corrupt, 10);
    assert_eq!(
        sync::fetch_latest(&faulty, 0, FAST)
            .await
            .unwrap_err()
            .code(),
        "integrity_failed"
    );
    // Tampered at rest: the same.
    store.tamper(&sha256_hex(&bytes), 0);
    let clean = FaultTransport::new(store.clone());
    assert_eq!(
        sync::fetch_latest(&clean, 0, FAST)
            .await
            .unwrap_err()
            .code(),
        "integrity_failed"
    );
}

#[tokio::test]
async fn a_large_segment_goes_up_in_parts() {
    let store = Arc::new(MemoryStore::new(1, Writer::Desktop).with_multipart(10, 4));
    let faulty = FaultTransport::new(store.clone());
    faulty.arm(Op::Part, Fault::Unavailable, 1);
    let bytes: Vec<u8> = (0u8..=22).collect();
    sync::upload_and_publish(&faulty, &bytes, &request(&bytes, 1, base(A)), FAST)
        .await
        .unwrap();
    assert_eq!(store.object(&sha256_hex(&bytes)).unwrap(), bytes);
    assert_eq!(faulty.calls(Op::Part), 7);
}

#[tokio::test]
async fn a_publish_must_describe_its_bytes() {
    let (_store, faulty) = setup();
    let bytes = b"x".to_vec();
    let mut wrong = request(&bytes, 1, base(A));
    wrong.bytes = 99;
    assert_eq!(
        sync::upload_and_publish(&faulty, &bytes, &wrong, FAST)
            .await
            .unwrap_err()
            .code(),
        "invalid"
    );
}
